package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// FieldOwner is the field manager every write the harness makes carries, so
// a phase's own writes can be told apart from the operator's and Helm's in
// managedFields.
const FieldOwner = "ptah-e2e"

// Cluster is the cluster the driver stood up, reached through the kubeconfig
// it wrote.
//
// Client reads straight from the API server. A phase asserts what the cluster
// holds now, and a cache that trails a write by one watch event would make an
// assertion about the harness rather than the operator.
type Cluster struct {
	Kubeconfig string
	Config     *rest.Config
	Client     client.Client
	Clientset  kubernetes.Interface
	Scheme     *runtime.Scheme
	// Namespace is the kubeconfig context's namespace, "default" when it
	// names none: what kubectl sends for a request that names no namespace.
	Namespace string
}

// Connect reaches the cluster the kubeconfig names, with the built-in types
// and the operator's own API registered.
func Connect(kubeconfig string) (*Cluster, error) {
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}, &clientcmd.ConfigOverrides{})
	config, err := loader.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("read the kubeconfig %s: %w", kubeconfig, err)
	}
	namespace, _, err := loader.Namespace()
	if err != nil {
		return nil, fmt.Errorf("read the namespace of the kubeconfig %s: %w", kubeconfig, err)
	}
	// The phases poll every second or two across a handful of objects; the
	// client's default of five requests a second would make a wait measure
	// its own throttle.
	config.QPS, config.Burst = 50, 100
	config.UserAgent = FieldOwner
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := ptahv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("build a client for %s: %w", kubeconfig, err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("build a clientset for %s: %w", kubeconfig, err)
	}
	return &Cluster{
		Kubeconfig: kubeconfig, Config: config, Client: direct, Clientset: clientset, Scheme: scheme,
		Namespace: namespace,
	}, nil
}

// CanI asks the API server what `kubectl auth can-i --as=<user>` asks: a
// SelfSubjectAccessReview sent as the user alone, whose groups the server
// derives itself.
func (c *Cluster) CanI(ctx context.Context, user string, attributes authorizationv1.ResourceAttributes) (bool, error) {
	impersonated, err := c.As(rest.ImpersonationConfig{UserName: user})
	if err != nil {
		return false, err
	}
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attributes},
	}
	if err := impersonated.Create(ctx, review); err != nil {
		return false, err
	}
	return review.Status.Allowed, nil
}

// As returns a client that sends every request as the given identity. The
// administrator the kubeconfig names has to be allowed to impersonate it,
// which kind's is.
func (c *Cluster) As(identity rest.ImpersonationConfig) (client.Client, error) {
	config := rest.CopyConfig(c.Config)
	config.Impersonate = identity
	return client.New(config, client.Options{Scheme: c.Scheme})
}

// WaitForRollout waits for the Deployment the way `kubectl rollout status`
// does, reading DeploymentRolledOut every two seconds.
func (c *Cluster) WaitForRollout(ctx context.Context, namespace, name string, timeout time.Duration) error {
	return Wait(ctx, fmt.Sprintf("rollout of Deployment %s/%s", namespace, name), timeout, 2*time.Second,
		func(ctx context.Context) (bool, string, error) {
			deployment := &appsv1.Deployment{}
			if err := c.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, deployment); err != nil {
				return false, fmt.Sprintf("read failed: %v", err), nil
			}
			return DeploymentRolledOut(deployment)
		})
}

// ContainerLog reads what one container of a Pod has written so far.
func (c *Cluster) ContainerLog(ctx context.Context, namespace, pod, container string) ([]byte, error) {
	return c.Clientset.CoreV1().Pods(namespace).
		GetLogs(pod, &corev1.PodLogOptions{Container: container}).
		DoRaw(ctx)
}

// Describe writes `kubectl describe` of one object to w. It is diagnostics
// for a failure the phase is about to report, so its own failure is written
// down rather than returned.
func (c *Cluster) Describe(ctx context.Context, w io.Writer, namespace, kind, name string) {
	command := exec.CommandContext(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, //nolint:gosec // Arguments, not a shell.
		"-n", namespace, "describe", kind, name)
	command.Stdout, command.Stderr = w, w
	if err := command.Run(); err != nil {
		_, _ = fmt.Fprintf(w, "e2e: kubectl describe %s %s/%s: %v\n", kind, namespace, name, err)
	}
}

// Kubectl runs the kubectl CLI against the cluster and returns what it wrote
// on standard output and standard error, separately. A phase reaches for it
// where the API has no typed call that says the same thing: running a command
// inside a Pod, which kubectl streams over a protocol the client libraries the
// harness links do not carry. Everything a phase reads or writes goes through
// Client.
func (c *Cluster) Kubectl(ctx context.Context, arguments ...string) (stdout, stderr []byte, err error) {
	var out, errOut bytes.Buffer
	command := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", c.Kubeconfig}, arguments...)...) //nolint:gosec // Arguments, not a shell.
	command.Stdout, command.Stderr = &out, &errOut
	err = command.Run()
	return out.Bytes(), errOut.Bytes(), err
}

// Raw reads one path from the API server, as `kubectl get --raw` does: a
// Pod's proxy subresource, which answers with whatever the Pod serves.
func (c *Cluster) Raw(ctx context.Context, path string) ([]byte, error) {
	return c.Clientset.CoreV1().RESTClient().Get().AbsPath(path).DoRaw(ctx)
}

// Helm runs the helm CLI against the cluster and returns what it printed on
// standard output. Its standard error goes to the phase's as it is written,
// as it did when a shell phase ran it, so a refusal is in the log above the
// failure that reports it.
func (c *Cluster) Helm(ctx context.Context, arguments ...string) ([]byte, error) {
	var stdout bytes.Buffer
	command := exec.CommandContext(ctx, "helm", append([]string{"--kubeconfig", c.Kubeconfig}, arguments...)...) //nolint:gosec // Arguments, not a shell.
	command.Stdout = &stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("helm %v: %w; its standard error is above", arguments, err)
	}
	return stdout.Bytes(), nil
}
