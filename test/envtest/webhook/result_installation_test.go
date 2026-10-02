package webhook_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/internal/certrotation"
	"github.com/stokaro/ptah-operator/internal/resultconsumer"
	"github.com/stokaro/ptah-operator/internal/resultservice"
	"github.com/stokaro/ptah-operator/test/envtest/internal/harness"
)

// Install the enabled chart's actual RBAC and precreated trust objects, then
// bootstrap through that rotator identity. No Deployment or kubelet runs here;
// projection to disk below is explicit and does not prove kubelet behavior.
func TestResultChartRBACAndTrustBootstrap(t *testing.T) {
	plane.Require(t)
	release := harness.DefaultRelease()
	release.Name = "result-install"
	release.Namespace = newNamespace(t, "result-install")
	release.TypedValues = [][2]string{{"resultDelivery.enabled", "true"}, {"resultDelivery.networkPolicy.enabled", "true"}, {"resultDelivery.networkPolicy.infrastructurePeers[0].ipBlock.cidr", "192.0.2.0/24"}}
	objects, err := harness.RenderChart(t.Context(), release)
	if err != nil {
		t.Fatal(err)
	}
	base := release.Name + "-ptah-operator"
	var managerArgs, rotatorArgs []string
	installed := 0
	for _, object := range objects {
		if object.GetKind() == "Deployment" {
			var deployment appsv1.Deployment
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &deployment); err != nil {
				t.Fatal(err)
			}
			for _, container := range deployment.Spec.Template.Spec.Containers {
				switch container.Name {
				case "manager":
					managerArgs = container.Args
				case "certificate-rotator":
					rotatorArgs = container.Args
				}
			}
		}
		if object.GetAnnotations()["helm.sh/hook"] != "" {
			continue
		}
		switch object.GetKind() {
		case "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding", "Secret", "ConfigMap", "Lease", "Service", "NetworkPolicy":
			if err := admin.Create(t.Context(), object); err != nil {
				t.Fatalf("install %s/%s: %v", object.GetKind(), object.GetName(), err)
			}
			installed++
			if object.GetNamespace() == "" {
				t.Cleanup(func() { _ = admin.Delete(context.Background(), object) })
			}
		}
	}
	if installed < 10 || len(managerArgs) == 0 || len(rotatorArgs) == 0 {
		t.Fatal("chart installation examined no complete runtime")
	}
	flags := func(args []string) map[string]string {
		m := map[string]string{}
		for _, arg := range args {
			k, v, ok := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
			if ok {
				m[k] = v
			}
		}
		return m
	}
	rargs, margs := flags(rotatorArgs), flags(managerArgs)
	managerUser := margs["controller-service-account-username"]
	rotatorUser := "system:serviceaccount:" + release.Namespace + ":" + base + "-cert-rotator"
	for _, row := range []struct {
		user, group, resource, name, verb string
		allowed                           bool
	}{
		{managerUser, "", "secrets", "database", "get", false},
		{managerUser, "", "secrets", "delivery", "create", true},
		{managerUser, "", "secrets", "delivery", "update", false},
		{managerUser, "", "pods/log", "runner", "get", false},
		{managerUser, "operator.ptah.run", "ptahresultrecords", "receipt", "get", true},
		{managerUser, "operator.ptah.run", "ptahresultrecords", "receipt", "create", true},
		{managerUser, "operator.ptah.run", "ptahresultrecords", "receipt", "delete", false},
		{managerUser, "", "configmaps", rargs["result-enrollment-policy"], "get", true},
		{managerUser, "", "configmaps", rargs["result-enrollment-policy"], "update", false},
		{rotatorUser, "", "secrets", rargs["result-secret-name"], "update", true},
		{rotatorUser, "", "secrets", rargs["result-journal-secret-name"], "get", true},
		{rotatorUser, "", "secrets", "database", "get", false},
		{rotatorUser, "", "secrets", rargs["result-secret-name"], "create", false},
		{rotatorUser, "", "configmaps", rargs["result-enrollment-policy"], "update", true},
		{rotatorUser, "", "configmaps", "another-policy", "update", false},
		{rotatorUser, "coordination.k8s.io", "leases", rargs["result-lease-name"], "update", true},
	} {
		resource, subresource, _ := strings.Cut(row.resource, "/")
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{User: row.user, ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: release.Namespace, Group: row.group, Resource: resource, Subresource: subresource, Name: row.name, Verb: row.verb}}}
		if err := admin.Create(t.Context(), review); err != nil {
			t.Fatal(err)
		}
		if review.Status.Allowed != row.allowed {
			t.Fatalf("%s %s %s/%s allowed=%t want=%t", row.user, row.verb, row.resource, row.name, review.Status.Allowed, row.allowed)
		}
	}
	duration := func(name string) time.Duration {
		value, err := time.ParseDuration(rargs[name])
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	config := certrotation.ResultConfig{Config: certrotation.Config{Namespace: release.Namespace, ReleaseName: release.Name, SecretName: rargs["result-secret-name"], StagingSecretName: rargs["result-journal-secret-name"], LeaseName: rargs["result-lease-name"], ServiceName: rargs["result-service-name"], ServiceNamespace: release.Namespace, EndpointPortName: "https", HolderIdentity: "test/rotator", RenewalThreshold: duration("renewal-threshold"), ServingCertificateValidity: duration("serving-certificate-validity"), CACertificateValidity: duration("ca-certificate-validity"), ProbeTimeout: 100 * time.Millisecond, ProbeInterval: time.Millisecond, LeaseDuration: duration("lease-duration"), AcquireTimeout: duration("lease-acquire-timeout")}, PolicyName: rargs["result-enrollment-policy"]}
	api, err := kubernetes.NewForConfig(plane.Impersonate(rotatorUser))
	if err != nil {
		t.Fatal(err)
	}
	rotator, err := certrotation.NewResultRotator(api, config)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := rotator.Run(t.Context()); err != nil || !result.Pending {
		t.Fatalf("persist bootstrap: pending=%t error=%v", result.Pending, err)
	}
	// Restart from the persisted journal. With no endpoint it must publish the
	// material but refuse readiness, leaving the same journal for another pass.
	rotator, err = certrotation.NewResultRotator(api, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotator.Run(t.Context()); err == nil {
		t.Fatal("bootstrap claimed success with no receiver endpoint")
	}
	projection := &corev1.Secret{}
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: release.Namespace, Name: config.SecretName}, projection); err != nil {
		t.Fatal(err)
	}
	if len(projection.Data) != 6 {
		t.Fatal("rotator did not publish all receiver trust files")
	}
	directory := t.TempDir()
	for key, value := range projection.Data {
		if err := os.WriteFile(filepath.Join(directory, key), value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	managerAPI := clientAs(t, managerUser)
	if _, err := resultservice.New(resultservice.Config{Endpoint: margs["result-endpoint"], Address: "127.0.0.1:0", CertificateDirectory: directory, EnrollmentPolicyNamespace: release.Namespace, EnrollmentPolicyName: config.PolicyName, Uploads: 1, UploadTimeout: time.Second, Consumer: resultconsumer.Options{Workers: 1, Entries: 1, Timeout: time.Second, Retention: time.Minute}}, managerAPI, managerAPI); err != nil {
		t.Fatalf("receiver rejects rotator projection: %v", err)
	}
	policy := &corev1.ConfigMap{}
	if err := managerAPI.Get(t.Context(), client.ObjectKey{Namespace: release.Namespace, Name: config.PolicyName}, policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Data) != 3 || policy.Data["version"] != "1" {
		t.Fatal("public enrollment policy was not persisted")
	}
	// Direct Secret read remains unavailable even after the manager can load the
	// projected files. Rotator updates did not grant the manager API read access.
	if err := managerAPI.Get(t.Context(), client.ObjectKeyFromObject(projection), &corev1.Secret{}); !apierrors.IsForbidden(err) {
		t.Fatalf("manager Secret read: %v, want Forbidden", err)
	}
}
