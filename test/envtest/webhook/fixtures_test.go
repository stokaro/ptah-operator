package webhook_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/coordination"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	executionBindingID     = "v1-0123456789abcdef0123456789abcdef"
	verificationPolicyName = "ptah-verification-policy"
	verificationPolicyKey  = "policy.yaml"
	executionAccount       = "ptah-orders"
)

func digest(character string) string { return "sha256:" + strings.Repeat(character, 64) }

// clientAs returns a client that authenticates as username. The API server
// authorizes and admits its requests as that identity, which is what the
// chart's matchConditions and the handlers read.
func clientAs(t *testing.T, username string, groups ...string) client.Client {
	t.Helper()
	return clientFor(t, plane.Impersonate(username, groups...))
}

func clientFor(t *testing.T, config *rest.Config) client.Client {
	t.Helper()
	api, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build a client for %q: %v", config.Impersonate.UserName, err)
	}
	return api
}

// newNamespace creates a namespace of the test's own. envtest runs no
// namespace controller, so nothing cleans one up; the control plane goes away
// with the package.
func newNamespace(t *testing.T, prefix string) string {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix + "-"}}
	if err := admin.Create(context.Background(), namespace); err != nil {
		t.Fatalf("create a namespace for %s: %v", prefix, err)
	}
	return namespace.Name
}

// grant lets subject do rules in namespace. RBAC comes first on every
// request, so an identity the test impersonates needs a grant before its
// request can reach admission at all.
func grant(t *testing.T, namespace, name string, subject rbacv1.Subject, rules ...rbacv1.PolicyRule) {
	t.Helper()
	ctx := context.Background()
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Rules: rules}
	if err := admin.Create(ctx, role); err != nil {
		t.Fatalf("create Role %s/%s: %v", namespace, name, err)
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{subject},
	}
	if err := admin.Create(ctx, binding); err != nil {
		t.Fatalf("create RoleBinding %s/%s: %v", namespace, name, err)
	}
}

func userSubject(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}
}

// managerSubject is the manager's ServiceAccount as an RBAC subject, derived
// from the username the chart configures.
func managerSubject(t *testing.T) rbacv1.Subject {
	t.Helper()
	parts := strings.Split(manager.username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		t.Fatalf("the chart's manager username %q is not a ServiceAccount", manager.username)
	}
	return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: parts[2], Name: parts[3]}
}

// createPolicy creates the immutable verification-policy ConfigMap a schema
// names. The approval handler binds the approval to its UID and bytes.
func createPolicy(t *testing.T, namespace string) *corev1.ConfigMap {
	t.Helper()
	immutable := true
	policy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: verificationPolicyName},
		Immutable:  &immutable,
		Data:       map[string]string{verificationPolicyKey: "version: 1\n"},
	}
	if err := admin.Create(context.Background(), policy); err != nil {
		t.Fatalf("create verification policy %s/%s: %v", namespace, verificationPolicyName, err)
	}
	return policy
}

// createSchema creates a PtahSchema from the fields a person writes, the
// smallest shape in the reference examples plus the execution identity.
func createSchema(t *testing.T, namespace, name string) *operatorv1alpha1.PtahSchema {
	t.Helper()
	ctx := context.Background()
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": operatorv1alpha1.GroupVersion.String(),
		"kind":       "PtahSchema",
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"spec": map[string]any{
			"target": map[string]any{
				"engine":          "PostgreSQL",
				"coordinationKey": "production/" + name + "-primary",
				"urlFrom":         map[string]any{"name": name + "-database", "key": "url"},
			},
			"desired": map[string]any{
				"ociRef":                 "oci://ghcr.io/example/" + name + "-schema:1.0.0",
				"verificationPolicyFrom": map[string]any{"name": verificationPolicyName, "key": verificationPolicyKey},
			},
			"execution": map[string]any{"serviceAccountName": executionAccount},
		},
	}}
	if err := admin.Create(ctx, object); err != nil {
		t.Fatalf("create PtahSchema %s/%s: %v", namespace, name, err)
	}
	schema := &operatorv1alpha1.PtahSchema{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(object), schema); err != nil {
		t.Fatalf("read PtahSchema %s/%s back: %v", namespace, name, err)
	}
	return schema
}

// executionBinding is the binding the manager records before it dispatches or
// plans: the chart's images and version, and this suite's revision.
func executionBinding() *operatorv1alpha1.ExecutionBindingStatus {
	return &operatorv1alpha1.ExecutionBindingStatus{
		Epoch:                  executionBindingID,
		ControllerImage:        manager.controllerImage,
		ControllerRevision:     controllerRevision,
		ControllerStateVersion: controllerstate.CurrentVersion,
		PtahVersion:            manager.ptahVersion,
		ExecutorImage:          manager.executorImage,
		RunnerImage:            manager.runnerImage,
		RunnerProtocolVersion:  int32(runner.ProtocolVersion),
	}
}

func coordinationDigest(t *testing.T, schema *operatorv1alpha1.PtahSchema) string {
	t.Helper()
	value, err := coordination.Digest(schema.Namespace, schema.Spec.Target)
	if err != nil {
		t.Fatalf("derive the coordination digest of %s/%s: %v", schema.Namespace, schema.Name, err)
	}
	return value
}

// writeStatus stores status through the subresource, the only way status is
// written: there is no controller here to write it.
func writeStatus(t *testing.T, object client.Object) {
	t.Helper()
	if err := admin.Status().Update(context.Background(), object); err != nil {
		t.Fatalf("write the status of %T %s: %v", object, client.ObjectKeyFromObject(object), err)
	}
}

func now() metav1.Time { return metav1.NewTime(time.Now().UTC().Truncate(time.Second)) }

func policyDigest() string { return fingerprint.DigestBytes([]byte("version: 1\n")) }

// requireDenied holds err to a refusal by the named webhook that carries
// reason. The API server words a webhook denial as
// `admission webhook "<name>" denied the request: <reason>`.
func requireDenied(t *testing.T, err error, webhook, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the API server admitted the request; want %s to deny it with %q", webhook, reason)
	}
	message := err.Error()
	want := `admission webhook "` + webhook + `" denied the request`
	if !strings.Contains(message, want) || !strings.Contains(message, reason) {
		t.Fatalf("the API server said %q; want %q with %q", message, want, reason)
	}
}

// requireNotDeniedBy fails when err is a refusal by webhook. Any other outcome
// -- admitted, or refused by something else -- is returned for the caller.
func requireNotDeniedBy(t *testing.T, err error, webhook string) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), `admission webhook "`+webhook+`"`) {
		t.Fatalf("webhook %s answered a request it should not have been sent: %v", webhook, err)
	}
}
