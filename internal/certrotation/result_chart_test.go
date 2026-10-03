package certrotation_test

import (
	"maps"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stokaro/ptah-operator/internal/certrotation"
)

func resultChartObject(t *testing.T, objects []*unstructured.Unstructured, kind, name string, into any) {
	t.Helper()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(mustObject(t, objects, kind, name).Object, into); err != nil {
		t.Fatal(err)
	}
}

func TestResultDeliveryChartWiresTrustAndScopedPermissions(t *testing.T) {
	objects := renderChart(t, "--set", "resultDelivery.enabled=true")
	base := releaseName + "-ptah-operator"
	var deployment appsv1.Deployment
	resultChartObject(t, objects, "Deployment", base, &deployment)
	manager := deployment.Spec.Template.Spec.Containers[0]
	for _, arg := range []string{"--result-endpoint=https://" + base + "-results." + releaseNamespace + ".svc", "--result-cert-dir=/result-certs", "--result-bind-address=:9444", "--result-enrollment-policy=" + base + "-result-enrollment"} {
		if !slices.Contains(manager.Args, arg) {
			t.Fatalf("missing manager argument %s", arg)
		}
	}
	foundMount, foundProjection := false, false
	for _, m := range manager.VolumeMounts {
		if m.Name == "result-cert" {
			foundMount = m.ReadOnly && m.MountPath == "/result-certs" && m.SubPath == ""
		}
	}
	for _, v := range deployment.Spec.Template.Spec.Volumes {
		if v.Name != "result-cert" {
			continue
		}
		if v.Secret == nil || v.Secret.SecretName != base+"-result-trust" || v.Secret.DefaultMode == nil || *v.Secret.DefaultMode != 0440 {
			t.Fatal("result trust volume is not restricted")
		}
		keys := map[string]bool{}
		for _, item := range v.Secret.Items {
			if item.Key != item.Path {
				t.Fatal("renamed projected trust file")
			}
			keys[item.Key] = true
		}
		foundProjection = maps.Equal(keys, map[string]bool{"tls.crt": true, "tls.key": true, "ca.crt": true, "client-ca.crt": true, "client-ca.key": true, "client-trust.crt": true})
	}
	if !foundMount || !foundProjection {
		t.Fatal("manager does not mount the six-file projection for atomic reload")
	}
	for suffix, role := range map[string]string{"result-trust": "projection", "result-journal": "journal", "result-enrollment": "enrollment"} {
		kind := "Secret"
		if role == "enrollment" {
			kind = "ConfigMap"
		}
		obj := mustObject(t, objects, kind, base+"-"+suffix)
		if !maps.Equal(obj.GetLabels(), map[string]string{"app.kubernetes.io/managed-by": "Helm", certrotation.ResultTrustLabel: role}) || !maps.Equal(obj.GetAnnotations(), map[string]string{"meta.helm.sh/release-name": releaseName, "meta.helm.sh/release-namespace": releaseNamespace}) {
			t.Fatal("trust object metadata violates rotator contract")
		}
		if data, found, _ := unstructured.NestedMap(obj.Object, "data"); found && len(data) != 0 {
			t.Fatal("Helm generated or retained private rotation material")
		}
	}
	mustObject(t, objects, "Lease", base+"-result-rotation")
	var svc corev1.Service
	resultChartObject(t, objects, "Service", base+"-results", &svc)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP || !maps.Equal(svc.Spec.Selector, deployment.Spec.Selector.MatchLabels) || len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Name != "https" || svc.Spec.Ports[0].Port != 443 || svc.Spec.Ports[0].TargetPort.StrVal != "results" {
		t.Fatal("receiver Service does not route to manager")
	}
	foundPort := false
	for _, port := range manager.Ports {
		if port.Name == "results" {
			foundPort = port.ContainerPort == 9444
		}
	}
	if !foundPort {
		t.Fatal("receiver port missing")
	}
	var role rbacv1.ClusterRole
	resultChartObject(t, objects, "ClusterRole", base, &role)
	permissions := func(resource string) []string {
		var verbs []string
		for _, rule := range role.Rules {
			if slices.Contains(rule.Resources, resource) {
				verbs = append(verbs, rule.Verbs...)
			}
		}
		slices.Sort(verbs)
		return slices.Compact(verbs)
	}
	if len(permissions("secrets")) != 0 || !slices.Equal(permissions("tokenreviews"), []string{"create"}) || !slices.Equal(permissions("ptahresultrecords"), []string{"create", "delete", "get", "list"}) || len(permissions("pods/log")) != 0 {
		t.Fatal("manager grant widened or retained log dependence")
	}
	var rotatorRole rbacv1.Role
	resultChartObject(t, objects, "Role", base+"-cert-rotator", &rotatorRole)
	expected := map[string][]string{"secrets": {base + "-webhook-cert", base + "-cert-rotation-stage", base + "-result-trust", base + "-result-journal"}, "configmaps": {base + "-result-enrollment"}, "leases": {base + "-cert-rotation", base + "-result-rotation"}}
	for resource, names := range expected {
		count := 0
		for _, rule := range rotatorRole.Rules {
			if slices.Contains(rule.Resources, resource) {
				count++
				if !slices.Equal(rule.ResourceNames, names) || !slices.Equal(rule.Verbs, []string{"get", "update"}) {
					t.Fatalf("rotator %s permissions are not exact", resource)
				}
			}
		}
		if count != 1 {
			t.Fatalf("rotator %s rule count %d", resource, count)
		}
	}
	var rotator appsv1.Deployment
	resultChartObject(t, objects, "Deployment", base+"-cert-rotator", &rotator)
	for _, arg := range []string{"--result-secret-name=" + base + "-result-trust", "--result-journal-secret-name=" + base + "-result-journal", "--result-enrollment-policy=" + base + "-result-enrollment", "--result-service-name=" + base + "-results", "--result-lease-name=" + base + "-result-rotation"} {
		if !slices.Contains(rotator.Spec.Template.Spec.Containers[0].Args, arg) {
			t.Fatalf("missing rotator argument %s", arg)
		}
	}
}

func TestResultDeliveryChartRequiresExplicitNetworkPeers(t *testing.T) {
	for _, args := range [][]string{
		{"--set", "resultDelivery.enabled=true,certificateRotation.enabled=false,webhook.existingSecret=external"},
		{"--set", "resultDelivery.enabled=true,webhook.port=9444"},
		{"--set", "resultDelivery.enabled=true,resultDelivery.networkPolicy.enabled=true"},
	} {
		if _, err := renderChartCommand(t, args...); err == nil {
			t.Fatalf("unsafe configuration rendered: %v", args)
		}
	}
	objects := renderChart(t, "--set", "resultDelivery.enabled=true,resultDelivery.networkPolicy.enabled=true", "--set-json", `resultDelivery.networkPolicy.infrastructurePeers=[{"ipBlock":{"cidr":"192.0.2.0/24"}}]`)
	var policy networkingv1.NetworkPolicy
	resultChartObject(t, objects, "NetworkPolicy", releaseName+"-ptah-operator-results", &policy)
	if len(policy.Spec.Ingress) != 2 || len(policy.Spec.Ingress[0].From) != 1 || policy.Spec.Ingress[0].From[0].IPBlock == nil || policy.Spec.Ingress[0].From[0].IPBlock.CIDR != "192.0.2.0/24" {
		t.Fatal("infrastructure ingress widened")
	}
	results := policy.Spec.Ingress[1]
	if len(results.Ports) != 1 || results.Ports[0].Port == nil || results.Ports[0].Port.StrVal != "results" || len(results.From) != 2 {
		t.Fatal("result ingress is not port scoped")
	}
	if results.From[0].NamespaceSelector == nil || results.From[0].PodSelector == nil || results.From[0].PodSelector.MatchLabels["app.kubernetes.io/managed-by"] != "ptah-operator" || len(results.From[0].PodSelector.MatchExpressions) != 1 || !slices.Equal(results.From[0].PodSelector.MatchExpressions[0].Values, []string{"schema-operation", "migration-operation"}) {
		t.Fatal("operation ingress selector widened")
	}
	if results.From[1].NamespaceSelector != nil || results.From[1].PodSelector == nil || results.From[1].PodSelector.MatchLabels["app.kubernetes.io/component"] != "certificate-rotation" || results.From[1].PodSelector.MatchLabels["app.kubernetes.io/instance"] != releaseName {
		t.Fatal("rotator ingress is not release scoped")
	}
}
