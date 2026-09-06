package certrotation_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/stokaro/ptah-operator/internal/certrotation"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

const (
	releaseName      = "rotation-test"
	releaseNamespace = "ptah-system"
	managerDigest    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	executorDigest   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	runnerDigest     = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	ptahVersion      = "e2e-explicit-version"
)

func TestGeneratedCertificateLifecycleRender(t *testing.T) {
	t.Parallel()
	renderStarted := time.Now()
	objects := renderChart(t, "--set", "webhook.timeoutSeconds=7")
	renderFinished := time.Now()
	managerName := releaseName + "-ptah-operator"
	rotatorName := releaseName + "-ptah-operator-cert-rotator"
	discoveryRoleName, err := crdupgrade.CertificateDiscoveryRoleName(releaseNamespace, releaseName)
	if err != nil {
		t.Fatal(err)
	}
	secretName := releaseName + "-ptah-operator-webhook-cert"
	stagingSecretName := releaseName + "-ptah-operator-cert-rotation-stage"
	candidateServiceName := releaseName + "-ptah-operator-cert-transition"
	canaryConfigMapName := releaseName + "-ptah-operator-cert-canary"
	leaseName := releaseName + "-ptah-operator-cert-rotation"
	configurationName := "ptah-operator-admission"

	secret := mustObject(t, objects, "Secret", secretName)
	if !maps.Equal(secret.GetLabels(), map[string]string{
		certrotation.GeneratedSecretLabel: certrotation.GeneratedSecretLabelValue,
		certrotation.HelmManagedByLabel:   certrotation.HelmManagedByLabelValue,
	}) || !maps.Equal(secret.GetAnnotations(), map[string]string{
		certrotation.HelmReleaseNameAnnotation:      releaseName,
		certrotation.HelmReleaseNamespaceAnnotation: releaseNamespace,
	}) {
		t.Fatalf("generated Secret ownership metadata = labels=%v annotations=%v", secret.GetLabels(), secret.GetAnnotations())
	}
	for _, key := range []string{"ca.crt", "ca.key", "tls.crt", "tls.key"} {
		if _, found, err := unstructured.NestedString(secret.Object, "data", key); err != nil || !found {
			t.Errorf("generated Secret data is missing %q", key)
		}
	}
	ca := mustCertificateFromSecret(t, secret, "ca.crt")
	serving := mustCertificateFromSecret(t, secret, "tls.crt")
	assertBootstrapCertificateExpiry(t, ca, renderStarted, renderFinished, 2*24*time.Hour)
	assertBootstrapCertificateExpiry(t, serving, renderStarted, renderFinished, 24*time.Hour)
	if !serving.NotAfter.Before(ca.NotAfter) {
		t.Fatalf("bootstrap serving certificate expires at %s, CA expires at %s", serving.NotAfter, ca.NotAfter)
	}
	stagingSecret := mustObject(t, objects, "Secret", stagingSecretName)
	if !maps.Equal(stagingSecret.GetLabels(), map[string]string{
		certrotation.StagingSecretLabel: certrotation.StagingSecretLabelValue,
		certrotation.HelmManagedByLabel: certrotation.HelmManagedByLabelValue,
	}) || !maps.Equal(stagingSecret.GetAnnotations(), map[string]string{
		certrotation.HelmReleaseNameAnnotation:      releaseName,
		certrotation.HelmReleaseNamespaceAnnotation: releaseNamespace,
	}) {
		t.Fatalf("staging Secret ownership metadata = labels=%v annotations=%v", stagingSecret.GetLabels(), stagingSecret.GetAnnotations())
	}
	if len(stagingSecret.GetOwnerReferences()) != 0 || len(stagingSecret.GetFinalizers()) != 0 {
		t.Fatalf("staging Secret contains unsupported metadata: %#v", stagingSecret.Object["metadata"])
	}
	if secretType, _, _ := unstructured.NestedString(stagingSecret.Object, "type"); secretType != "Opaque" {
		t.Fatalf("staging Secret type = %q, want Opaque", secretType)
	}
	if data, found, err := unstructured.NestedMap(stagingSecret.Object, "data"); err != nil || found || len(data) != 0 {
		t.Fatalf("chart owns staging Secret data: found=%v data=%v err=%v", found, data, err)
	}
	if stringData, found, err := unstructured.NestedMap(stagingSecret.Object, "stringData"); err != nil || found || len(stringData) != 0 {
		t.Fatalf("chart owns staging Secret stringData: found=%v data=%v err=%v", found, stringData, err)
	}
	if immutable, found, err := unstructured.NestedBool(stagingSecret.Object, "immutable"); err != nil || found || immutable {
		t.Fatalf("chart sets staging Secret immutable: found=%v value=%v err=%v", found, immutable, err)
	}
	canary := mustObject(t, objects, "ConfigMap", canaryConfigMapName)
	wantMarker := certrotation.AdmissionCanaryMarker(releaseNamespace, canaryConfigMapName, releaseName)
	if !maps.Equal(canary.GetLabels(), wantMarker.Labels) {
		t.Fatalf("certificate canary labels = %v, want exact Helm-owned marker labels", canary.GetLabels())
	}
	if !maps.Equal(canary.GetAnnotations(), wantMarker.Annotations) || len(canary.GetOwnerReferences()) != 0 || len(canary.GetFinalizers()) != 0 {
		t.Fatalf("certificate canary contains unsupported metadata: %#v", canary.Object["metadata"])
	}
	if immutable, found, err := unstructured.NestedBool(canary.Object, "immutable"); err != nil || !found || !immutable {
		t.Fatalf("certificate canary immutable = found=%v value=%v err=%v, want true", found, immutable, err)
	}
	for _, field := range []string{"data", "binaryData"} {
		if value, found, err := unstructured.NestedMap(canary.Object, field); err != nil || found || len(value) != 0 {
			t.Fatalf("certificate canary owns %s: found=%v value=%v err=%v", field, found, value, err)
		}
	}
	candidateService := mustObject(t, objects, "Service", candidateServiceName)
	if publish, found, err := unstructured.NestedBool(candidateService.Object, "spec", "publishNotReadyAddresses"); err != nil || !found || !publish {
		t.Fatalf("candidate Service publishNotReadyAddresses = found=%v value=%v err=%v, want true", found, publish, err)
	}
	selector, found, err := unstructured.NestedStringMap(candidateService.Object, "spec", "selector")
	if err != nil || !found || !maps.Equal(selector, map[string]string{
		"app.kubernetes.io/name":      "ptah-operator",
		"app.kubernetes.io/instance":  releaseName,
		"app.kubernetes.io/component": "certificate-rotation",
	}) {
		t.Fatalf("candidate Service selector = %v, found=%v err=%v", selector, found, err)
	}
	servicePorts, found, err := unstructured.NestedSlice(candidateService.Object, "spec", "ports")
	if err != nil || !found || len(servicePorts) != 1 {
		t.Fatalf("candidate Service ports = %#v, found=%v err=%v", servicePorts, found, err)
	}
	servicePort := servicePorts[0].(map[string]any)
	if servicePort["name"] != "https" || servicePort["port"] != int64(443) ||
		servicePort["protocol"] != "TCP" || servicePort["targetPort"] != "candidate" {
		t.Fatalf("candidate Service port = %#v", servicePort)
	}

	managerRole := mustObject(t, objects, "ClusterRole", managerName)
	for _, rule := range objectRules(t, managerRole) {
		if slices.Contains(stringSlice(rule["resources"]), "secrets") {
			t.Fatal("manager ClusterRole grants Secret access")
		}
	}

	role := mustNamespacedObject(t, objects, "Role", releaseNamespace, rotatorName)
	assertExactRule(t, role, "", "secrets", []string{secretName, stagingSecretName}, []string{"get", "update"})
	assertNoResourceVerb(t, role, "", "secrets", "create")
	assertExactRule(t, role, "coordination.k8s.io", "leases", []string{leaseName}, []string{"get", "update"})
	assertExactRule(t, role, "", "configmaps", []string{canaryConfigMapName}, []string{"get", "update"})
	assertExactRule(t, role, "discovery.k8s.io", "endpointslices", nil, []string{"list"})
	defaultDiscoveryRole := mustNamespacedObject(t, objects, "Role", "default", discoveryRoleName)
	assertExactRule(t, defaultDiscoveryRole, "discovery.k8s.io", "endpointslices", nil, []string{"list"})
	assertExactCertificateRoleBinding(t, objects, "default", discoveryRoleName, rotatorName, releaseNamespace)
	runtimeAdmissionRole := mustObject(t, objects, "Role", managerName+"-runtime-admission")
	assertExactRule(t, runtimeAdmissionRole, "", "configmaps", []string{"ptah-admission-convergence-v1-1-f1e165dcd72a"}, []string{"get", "update"})

	clusterRole := mustObject(t, objects, "ClusterRole", rotatorName)
	assertNoResourceVerb(t, clusterRole, "discovery.k8s.io", "endpointslices", "list")
	assertExactRule(t, clusterRole, "admissionregistration.k8s.io", "mutatingwebhookconfigurations", []string{configurationName}, []string{"get", "update"})
	assertExactRule(t, clusterRole, "admissionregistration.k8s.io", "validatingwebhookconfigurations", []string{configurationName}, []string{"get", "update"})
	runtimeGuardNames := []string{
		"ptah-operator-rollout-guard-v1",
		"ptah-operator-runtime-guard-v1",
		"ptah-operator-runtime-pod-identity-v1",
		"ptah-operator-hook-identity-v1-90a0385b562b",
		"ptah-operator-hook-probe-guard-v1-90a0385b562b",
		"ptah-operator-release-activation-guard-v1-f1e165dcd72a",
		"ptah-operator-admission-convergence-v1-f1e165dcd72a",
		"ptah-operator-service-account-object-guard-v1-f1e165dcd72a",
		"ptah-operator-service-account-origin-guard-v2-90a0385b562b",
		"ptah-operator-controller-write-guard-v2-90a0385b562b",
		"ptah-operator-job-write-guard-v2-90a0385b562b",
		"ptah-operator-chunk-write-guard-v2-90a0385b562b",
		"ptah-operator-plan-write-guard-v2-90a0385b562b",
		"ptah-operator-certificate-mutate-guard-v1-f1e165dcd72a",
		"ptah-operator-certificate-validate-guard-v1-f1e165dcd72a",
		"ptah-operator-namespace-deletion-guard-v1-f1e165dcd72a",
		"ptah-operator-runtime-parent-guard-v2-90a0385b562b",
		"ptah-operator-hook-pod-origin-guard-v2-f1e165dcd72a",
		"ptah-operator-hook-parent-origin-guard-v2-f1e165dcd72a",
		"ptah-operator-cert-stage-guard-v1-f1e165dcd72a",
		"ptah-operator-hook-pod-origin-guard-v1-f1e165dcd72a",
		"ptah-operator-hook-parent-origin-guard-v1-f1e165dcd72a",
		"ptah-operator-hook-parent-contract-v1-90a0385b562b",
	}
	assertExactRule(t, clusterRole, "admissionregistration.k8s.io", "validatingadmissionpolicies", runtimeGuardNames, []string{"get"})
	assertExactRule(t, clusterRole, "admissionregistration.k8s.io", "validatingadmissionpolicybindings", runtimeGuardNames, []string{"get"})
	cleanupBase := managerName
	if len(cleanupBase) > 24 {
		cleanupBase = cleanupBase[:24]
	}
	cleanupPrivilegeName := strings.TrimSuffix(cleanupBase, "-") + "-cleanup-priv-v1-90a0385b562b"
	cleanupRole := mustObject(t, objects, "Role", cleanupPrivilegeName)
	assertExactRule(t, cleanupRole, "", "secrets", []string{stagingSecretName}, []string{"get", "update", "delete"})
	assertObjectAbsent(t, objects, "ValidatingAdmissionPolicy", rotatorName)
	assertObjectAbsent(t, objects, "ValidatingAdmissionPolicyBinding", rotatorName)
	mustObject(t, objects, "Lease", leaseName)

	rotatorDeployment := mustObject(t, objects, "Deployment", rotatorName)
	if len(rotatorDeployment.GetName()) > 63 {
		t.Fatalf("rotator Deployment name length = %d, want at most 63", len(rotatorDeployment.GetName()))
	}
	if got, _, _ := unstructured.NestedString(rotatorDeployment.Object, "spec", "strategy", "type"); got != "Recreate" {
		t.Errorf("rotator Deployment strategy = %q, want Recreate", got)
	}
	containers, _, err := unstructured.NestedSlice(rotatorDeployment.Object, "spec", "template", "spec", "containers")
	if err != nil || len(containers) != 1 {
		t.Fatalf("rotator Deployment containers = %d, want 1", len(containers))
	}
	container := containers[0].(map[string]any)
	if got := stringSlice(container["command"]); !slices.Equal(got, []string{"/ptah-cert-rotator"}) {
		t.Errorf("rotator command = %v", got)
	}
	args := stringSlice(container["args"])
	for _, want := range []string{
		"--release-name=" + releaseName,
		"--staging-secret-name=" + stagingSecretName,
		"--candidate-service-name=" + candidateServiceName,
		"--candidate-bind-address=:9444",
		"--candidate-probe-config-map-name=" + canaryConfigMapName,
		"--candidate-probe-username=system:serviceaccount:" + releaseNamespace + ":" + rotatorName,
		"--candidate-mutating-field-manager=ptah-certificate-rotation-canary-mutate-v1",
		"--candidate-validating-field-manager=ptah-certificate-rotation-canary-validate-v1",
		"--candidate-stability-duration=10s",
		"--candidate-poll-interval=1s",
		"--candidate-request-timeout=5s",
		"--mutating-webhook-names=mapproval.operator.ptah.dev",
		"--validating-webhook-names=vapproval.operator.ptah.dev,vpodintent.operator.ptah.dev,vcontrollerwrite.operator.ptah.dev",
		"--run-interval=6h",
		"--operation-timeout=15m",
		"--retry-initial=5s",
		"--retry-max=5m",
		"--health-bind-address=:8081",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("rotator args do not contain %q: %v", want, args)
		}
	}
	mutatingConfiguration := mustObject(t, objects, "MutatingWebhookConfiguration", configurationName)
	validatingConfiguration := mustObject(t, objects, "ValidatingWebhookConfiguration", configurationName)
	for _, configuration := range []*unstructured.Unstructured{mutatingConfiguration, validatingConfiguration} {
		if got := configuration.GetAnnotations()["operator.ptah.dev/admission-contract-version"]; got != "2" {
			t.Fatalf("%s admission contract version = %q, want 2", configuration.GetKind(), got)
		}
	}
	if got, want := strings.Split(requiredArgumentValue(t, args, "--mutating-webhook-names="), ","), []string{"mapproval.operator.ptah.dev"}; !slices.Equal(got, want) {
		t.Fatalf("rotator mutating production webhook inventory = %v, want %v", got, want)
	}
	if got, want := strings.Split(requiredArgumentValue(t, args, "--validating-webhook-names="), ","), []string{"vapproval.operator.ptah.dev", "vpodintent.operator.ptah.dev", "vcontrollerwrite.operator.ptah.dev"}; !slices.Equal(got, want) {
		t.Fatalf("rotator validating production webhook inventory = %v, want %v", got, want)
	}
	wantMutatingCanary, wantValidatingCanary := certrotation.AdmissionCanaryStaticContractForTest(
		certrotation.AdmissionCanaryConfig{
			ReleaseName:          releaseName,
			MarkerNamespace:      releaseNamespace,
			MarkerName:           canaryConfigMapName,
			ServiceAccountName:   rotatorName,
			CandidateServiceName: candidateServiceName,
			ServiceNamespace:     releaseNamespace,
		},
	)
	assertMutatingCanaryStaticWebhookContract(t, mutatingConfiguration, wantMutatingCanary)
	assertValidatingCanaryStaticWebhookContract(t, validatingConfiguration, wantValidatingCanary)
	for _, forbiddenPrefix := range []string{
		"--recreate-missing-secret",
		"--secret-create-policy-name=",
		"--secret-create-policy-binding-name=",
		"--secret-create-service-account-name=",
	} {
		if slices.ContainsFunc(args, func(arg string) bool { return strings.HasPrefix(arg, forbiddenPrefix) }) {
			t.Errorf("rotator args unexpectedly contain %q: %v", forbiddenPrefix, args)
		}
	}
	ports := container["ports"].([]any)
	if len(ports) != 2 {
		t.Fatalf("rotator ports = %d, want 2", len(ports))
	}
	wantPorts := map[string]int64{"health": 8081, "candidate": 9444}
	for _, value := range ports {
		port := value.(map[string]any)
		name, _ := port["name"].(string)
		if port["containerPort"] != wantPorts[name] || port["protocol"] != "TCP" {
			t.Errorf("rotator port = %#v", port)
		}
		delete(wantPorts, name)
	}
	if len(wantPorts) != 0 {
		t.Errorf("rotator ports omit %v", wantPorts)
	}
	assertHTTPProbe(t, container, "livenessProbe", "/healthz")
	assertHTTPProbe(t, container, "readinessProbe", "/readyz")

	deployment := mustObject(t, objects, "Deployment", releaseName+"-ptah-operator")
	containers, _, err = unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || len(containers) != 1 {
		t.Fatalf("manager Deployment containers = %d, want 1", len(containers))
	}
	managerArgs := stringSlice(containers[0].(map[string]any)["args"])
	if !slices.Contains(managerArgs, "--ptah-version="+ptahVersion) {
		t.Fatalf("manager args do not bind the explicit Ptah version: %v", managerArgs)
	}
	if component := deployment.GetLabels()["app.kubernetes.io/component"]; component != "controller" {
		t.Fatalf("manager Deployment component label = %q, want controller", component)
	}
	var controllerDeployments []string
	for _, object := range objects {
		labels := object.GetLabels()
		if object.GetKind() == "Deployment" &&
			labels["app.kubernetes.io/instance"] == releaseName &&
			labels["app.kubernetes.io/component"] == "controller" {
			controllerDeployments = append(controllerDeployments, object.GetName())
		}
	}
	if !slices.Equal(controllerDeployments, []string{managerName}) {
		t.Fatalf("controller-labeled Deployments = %v, want only %q", controllerDeployments, managerName)
	}
	assertManagerTLSProjection(t, deployment, secretName)
}

func mustCertificateFromSecret(t *testing.T, secret *unstructured.Unstructured, key string) *x509.Certificate {
	t.Helper()
	encoded, found, err := unstructured.NestedString(secret.Object, "data", key)
	if err != nil || !found {
		t.Fatalf("generated Secret data is missing %q", key)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode generated Secret %s: %v", key, err)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("generated Secret %s is not exactly one PEM certificate", key)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse generated Secret %s: %v", key, err)
	}
	return certificate
}

func assertBootstrapCertificateExpiry(
	t *testing.T,
	certificate *x509.Certificate,
	renderStarted time.Time,
	renderFinished time.Time,
	validity time.Duration,
) {
	t.Helper()
	const timestampTolerance = time.Minute
	earliest := renderStarted.Add(validity - timestampTolerance)
	latest := renderFinished.Add(validity + timestampTolerance)
	if certificate.NotAfter.Before(earliest) || certificate.NotAfter.After(latest) {
		t.Fatalf("bootstrap certificate expiry = %s, want between %s and %s", certificate.NotAfter, earliest, latest)
	}
}

func TestCertificateEndpointSliceRBACIsNamespaceBound(t *testing.T) {
	t.Parallel()
	for _, namespace := range []string{releaseNamespace, "default"} {
		namespace := namespace
		t.Run(namespace, func(t *testing.T) {
			t.Parallel()
			objects := renderChartInNamespace(t, namespace)
			rotatorName := releaseName + "-ptah-operator-cert-rotator"
			discoveryRoleName, err := crdupgrade.CertificateDiscoveryRoleName(namespace, releaseName)
			if err != nil {
				t.Fatal(err)
			}
			allowedIdentities := map[string]string{namespace + "/" + rotatorName: rotatorName}
			if namespace != "default" {
				allowedIdentities["default/"+discoveryRoleName] = rotatorName
			}

			roleCount := 0
			bindingCount := 0
			for _, object := range objects {
				switch object.GetKind() {
				case "ClusterRole":
					for _, rule := range objectRules(t, object) {
						groups := stringSlice(rule["apiGroups"])
						resources := stringSlice(rule["resources"])
						if (slices.Contains(groups, "discovery.k8s.io") || slices.Contains(groups, "*")) &&
							(slices.Contains(resources, "endpointslices") || slices.Contains(resources, "*")) {
							t.Fatalf("ClusterRole/%s grants EndpointSlice authority: %v", object.GetName(), rule)
						}
					}
				case "Role":
					if object.GetName() != rotatorName && object.GetName() != discoveryRoleName {
						continue
					}
					identity := object.GetNamespace() + "/" + object.GetName()
					if _, allowed := allowedIdentities[identity]; !allowed {
						t.Fatalf("certificate EndpointSlice Role rendered outside the namespace contract: %s", identity)
					}
					assertExactRule(t, object, "discovery.k8s.io", "endpointslices", nil, []string{"list"})
					roleCount++
				case "RoleBinding":
					if object.GetName() != rotatorName && object.GetName() != discoveryRoleName {
						continue
					}
					identity := object.GetNamespace() + "/" + object.GetName()
					subjectName, allowed := allowedIdentities[identity]
					if !allowed {
						t.Fatalf("certificate EndpointSlice RoleBinding rendered outside the namespace contract: %s", identity)
					}
					assertExactCertificateRoleBinding(t, objects, object.GetNamespace(), object.GetName(), subjectName, namespace)
					bindingCount++
				}
			}
			if roleCount != len(allowedIdentities) || bindingCount != len(allowedIdentities) {
				t.Fatalf(
					"certificate EndpointSlice RBAC objects = %d Roles and %d RoleBindings, want %d of each",
					roleCount,
					bindingCount,
					len(allowedIdentities),
				)
			}
			for name := range allowedIdentities {
				_, objectName, _ := strings.Cut(name, "/")
				assertObjectAbsentInNamespace(t, objects, "Role", "unrelated", objectName)
				assertObjectAbsentInNamespace(t, objects, "RoleBinding", "unrelated", objectName)
			}
		})
	}
}

func TestCertificateDiscoveryRoleNameSeparatesEqualReleaseNamesAcrossNamespaces(t *testing.T) {
	t.Parallel()
	first, err := crdupgrade.CertificateDiscoveryRoleName("team-a", releaseName)
	if err != nil {
		t.Fatal(err)
	}
	second, err := crdupgrade.CertificateDiscoveryRoleName("team-b", releaseName)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("certificate discovery Role name %q collides across release namespaces", first)
	}
	for namespace, name := range map[string]string{"team-a": first, "team-b": second} {
		const prefix = "ptah-cert-discovery-v1-"
		if len(name) != 63 || !strings.HasPrefix(name, prefix) {
			t.Fatalf("certificate discovery Role name %q is not an exact DNS-63 identity", name)
		}
		if suffix := strings.TrimPrefix(name, prefix); len(suffix) != 40 {
			t.Fatalf("certificate discovery Role digest %q has length %d, want 40", suffix, len(suffix))
		} else if _, err := hex.DecodeString(suffix); err != nil {
			t.Fatalf("certificate discovery Role digest %q is not hexadecimal: %v", suffix, err)
		}
		objects := renderChartInNamespace(t, namespace)
		mustNamespacedObject(t, objects, "Role", "default", name)
		assertExactCertificateRoleBinding(t, objects, "default", name, releaseName+"-ptah-operator-cert-rotator", namespace)
	}
}

func TestMissingSecretRecreationOptInRender(t *testing.T) {
	t.Parallel()
	objects := renderChart(t, "--set", "certificateRotation.recreateMissingSecret=true")
	rotatorName := releaseName + "-ptah-operator-cert-rotator"
	secretName := releaseName + "-ptah-operator-webhook-cert"

	role := mustObject(t, objects, "Role", rotatorName)
	assertExactRule(t, role, "", "secrets", nil, []string{"create"})
	clusterRole := mustObject(t, objects, "ClusterRole", rotatorName)
	assertExactRule(t, clusterRole, "admissionregistration.k8s.io", "validatingadmissionpolicies", []string{rotatorName}, []string{"get"})
	assertExactRule(t, clusterRole, "admissionregistration.k8s.io", "validatingadmissionpolicybindings", []string{rotatorName}, []string{"get"})
	assertSecretCreateGuard(t, objects, rotatorName, secretName)

	deployment := mustObject(t, objects, "Deployment", rotatorName)
	containers, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || len(containers) != 1 {
		t.Fatalf("rotator Deployment containers = %d, want 1", len(containers))
	}
	args := stringSlice(containers[0].(map[string]any)["args"])
	for _, want := range []string{
		"--recreate-missing-secret=true",
		"--secret-create-policy-name=" + rotatorName,
		"--secret-create-policy-binding-name=" + rotatorName,
		"--secret-create-service-account-name=" + rotatorName,
	} {
		if !slices.Contains(args, want) {
			t.Errorf("rotator args do not contain %q: %v", want, args)
		}
	}
}

func TestGeneratedCertificateRequiresRotation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "schema validation"},
		{name: "template validation", args: []string{"--skip-schema-validation"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			args := append(slices.Clone(test.args), "--set", "certificateRotation.enabled=false")
			if _, err := renderChartCommand(t, args...); err == nil {
				t.Fatal("Helm accepted a generated webhook certificate without its rotator")
			}
		})
	}
}

func TestExistingSecretDisablesBuiltInLifecycle(t *testing.T) {
	t.Parallel()
	objects := renderChart(t,
		"--set-string", "webhook.existingSecret=external-webhook-cert",
		"--set-string", "webhook.caBundle=external-ca",
		"--set", "certificateRotation.recreateMissingSecret=true",
	)
	for _, object := range objects {
		if object.GetLabels()["app.kubernetes.io/component"] == "certificate-rotation" {
			t.Fatalf("external Secret render contains certificate lifecycle object %s/%s", object.GetKind(), object.GetName())
		}
		if object.GetKind() == "CronJob" || object.GetKind() == "Lease" {
			t.Fatalf("external Secret render contains %s %q", object.GetKind(), object.GetName())
		}
	}
	assertObjectAbsent(t, objects, "Secret", releaseName+"-ptah-operator-cert-rotation-stage")
	assertObjectAbsent(t, objects, "ConfigMap", releaseName+"-ptah-operator-cert-canary")
	assertObjectAbsent(t, objects, "Service", releaseName+"-ptah-operator-cert-transition")
	assertObjectAbsent(t, objects, "ValidatingAdmissionPolicy", "ptah-operator-cert-stage-guard-v1-f1e165dcd72a")
	assertObjectAbsent(t, objects, "ValidatingAdmissionPolicyBinding", "ptah-operator-cert-stage-guard-v1-f1e165dcd72a")
	for _, kind := range []string{"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"} {
		configuration := mustObject(t, objects, kind, "ptah-operator-admission")
		if got := configuration.GetAnnotations()["operator.ptah.dev/admission-contract-version"]; got != "1" {
			t.Fatalf("%s external-certificate admission contract version = %q, want 1", kind, got)
		}
	}
	deployment := mustObject(t, objects, "Deployment", releaseName+"-ptah-operator")
	assertManagerTLSProjection(t, deployment, "external-webhook-cert")
}

func TestLongFullnameKeepsGeneratedNamesValid(t *testing.T) {
	t.Parallel()
	objects := renderChart(t,
		"--set-string", "fullnameOverride="+strings.Repeat("a", 120),
		"--set", "certificateRotation.recreateMissingSecret=true",
	)
	for _, object := range objects {
		limit := 253
		switch object.GetKind() {
		case "ConfigMap", "Deployment", "Lease", "PodDisruptionBudget", "Secret", "Service", "ServiceAccount":
			limit = 63
		}
		if len(object.GetName()) > limit {
			t.Errorf("%s name %q has length %d, limit %d", object.GetKind(), object.GetName(), len(object.GetName()), limit)
		}
	}
}

func TestCertificateRotationValueValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		flag    string
		setting string
	}{
		{flag: "--set-string", setting: "certificateRotation.probeTimeout=0s"},
		{flag: "--set-string", setting: "certificateRotation.interval=0s"},
		{flag: "--set-string", setting: "certificateRotation.operationTimeout=0s"},
		{flag: "--set-string", setting: "certificateRotation.retryInitial=0s"},
		{flag: "--set-string", setting: "certificateRotation.retryMax=0s"},
		{flag: "--set-string", setting: "certificateRotation.admissionConvergence.stabilityDuration=0s"},
		{flag: "--set-string", setting: "certificateRotation.admissionConvergence.pollInterval=0s"},
		{flag: "--set-string", setting: "certificateRotation.admissionConvergence.requestTimeout=0s"},
		{flag: "--set", setting: "certificateRotation.healthPort=0"},
		{flag: "--set", setting: "certificateRotation.candidatePort=0"},
		{flag: "--set", setting: "certificateRotation.candidatePort=8081"},
		{flag: "--set-string", setting: "certificateRotation.recreateMissingSecret=not-a-boolean"},
		{flag: "--set-string", setting: "webhook.existingSecret=Bad_Name"},
	} {
		t.Run(test.setting, func(t *testing.T) {
			if _, err := renderChartCommand(t, test.flag, test.setting); err == nil {
				t.Fatalf("Helm accepted invalid value %q", test.setting)
			}
		})
	}
	if _, err := renderChartCommand(t, "--set", "certificateRotation.candidatePort=9443"); err != nil {
		t.Fatalf("Helm rejected a candidate listener port reused only by a different Pod: %v", err)
	}
}

func assertObjectAbsent(t *testing.T, objects []*unstructured.Unstructured, kind, name string) {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind && object.GetName() == name {
			t.Fatalf("rendered object %s/%s must be absent", kind, name)
		}
	}
}

func assertSecretCreateGuard(
	t *testing.T,
	objects []*unstructured.Unstructured,
	guardName string,
	secretName string,
) {
	t.Helper()
	policy := mustObject(t, objects, "ValidatingAdmissionPolicy", guardName)
	if failurePolicy, _, _ := unstructured.NestedString(policy.Object, "spec", "failurePolicy"); failurePolicy != "Fail" {
		t.Fatalf("Secret CREATE guard failurePolicy = %q, want Fail", failurePolicy)
	}
	matchConditions, _, err := unstructured.NestedSlice(policy.Object, "spec", "matchConditions")
	if err != nil || len(matchConditions) != 1 {
		t.Fatalf("Secret CREATE guard matchConditions = %#v", matchConditions)
	}
	matchExpression := matchConditions[0].(map[string]any)["expression"].(string)
	wantUsername := "system:serviceaccount:" + releaseNamespace + ":" + guardName
	if !strings.Contains(matchExpression, wantUsername) {
		t.Fatalf("Secret CREATE guard does not bind exact ServiceAccount %q", wantUsername)
	}
	validations, _, err := unstructured.NestedSlice(policy.Object, "spec", "validations")
	if err != nil || len(validations) != 1 {
		t.Fatalf("Secret CREATE guard validations = %#v", validations)
	}
	validation := validations[0].(map[string]any)
	if validation["message"] != "certificate rotator Secret CREATE is outside its exact recovery contract" {
		t.Fatalf("Secret CREATE guard denial message = %v", validation["message"])
	}
	expression := validation["expression"].(string)
	for _, required := range []string{
		"object.metadata.name == '" + secretName + "'",
		"object.metadata.namespace == '" + releaseNamespace + "'",
		"(!has(object.metadata.generateName) || object.metadata.generateName == '')",
		"object.metadata.labels ==",
		"operator.ptah.dev/generated-webhook-certificate",
		"object.metadata.annotations ==",
		"app.kubernetes.io/managed-by",
		"meta.helm.sh/release-name",
		"meta.helm.sh/release-namespace",
		"object.metadata.ownerReferences.size() == 0",
		"object.metadata.finalizers.size() == 0",
		"object.type == 'kubernetes.io/tls'",
		"!has(object.immutable)",
		"object.stringData.size() == 0",
		"object.data.size() == 4",
		"'ca.crt' in object.data",
		"'ca.key' in object.data",
		"'tls.crt' in object.data",
		"'tls.key' in object.data",
	} {
		if !strings.Contains(expression, required) {
			t.Errorf("Secret CREATE guard expression does not contain %q", required)
		}
	}

	binding := mustObject(t, objects, "ValidatingAdmissionPolicyBinding", guardName)
	if policyName, _, _ := unstructured.NestedString(binding.Object, "spec", "policyName"); policyName != guardName {
		t.Fatalf("Secret CREATE guard binding policyName = %q, want %q", policyName, guardName)
	}
	actions, _, err := unstructured.NestedStringSlice(binding.Object, "spec", "validationActions")
	if err != nil || !slices.Equal(actions, []string{"Deny"}) {
		t.Fatalf("Secret CREATE guard validationActions = %v, want [Deny]", actions)
	}
	namespace, _, _ := unstructured.NestedString(
		binding.Object,
		"spec", "matchResources", "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name",
	)
	if namespace != releaseNamespace {
		t.Fatalf("Secret CREATE guard namespace = %q, want %q", namespace, releaseNamespace)
	}
	contract := certrotation.Config{
		Namespace:                      releaseNamespace,
		ReleaseName:                    releaseName,
		SecretName:                     secretName,
		SecretCreatePolicyName:         guardName,
		SecretCreatePolicyBindingName:  guardName,
		SecretCreateServiceAccountName: guardName,
	}
	typedPolicy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(policy.Object, typedPolicy); err != nil {
		t.Fatalf("decode Secret CREATE guard policy: %v", err)
	}
	if err := certrotation.VerifySecretCreatePolicyContract(typedPolicy, contract); err != nil {
		t.Fatalf("rendered Secret CREATE guard policy differs from runtime contract: %v", err)
	}
	typedBinding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(binding.Object, typedBinding); err != nil {
		t.Fatalf("decode Secret CREATE guard binding: %v", err)
	}
	if err := certrotation.VerifySecretCreateBindingContract(typedBinding, contract); err != nil {
		t.Fatalf("rendered Secret CREATE guard binding differs from runtime contract: %v", err)
	}
}

func assertHTTPProbe(t *testing.T, container map[string]any, field, path string) {
	t.Helper()
	probe, ok := container[field].(map[string]any)
	if !ok {
		t.Fatalf("rotator %s = %#v", field, container[field])
	}
	httpGet, ok := probe["httpGet"].(map[string]any)
	if !ok {
		t.Fatalf("rotator %s.httpGet = %#v", field, probe["httpGet"])
	}
	if httpGet["path"] != path || httpGet["port"] != "health" || httpGet["scheme"] != "HTTP" {
		t.Errorf("rotator %s.httpGet = %#v", field, httpGet)
	}
}

func TestChartRequiresBoundedExplicitPtahVersion(t *testing.T) {
	t.Parallel()
	for name, version := range map[string]string{
		"missing":     "",
		"over-bounds": strings.Repeat("v", 129),
	} {
		name, version := name, version
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := renderChartCommand(t,
				"--set-string", "execution.ptahVersion="+version,
			); err == nil {
				t.Fatalf("chart accepted invalid explicit Ptah version %q", version)
			}
		})
	}
}

func TestChartPreservesExplicitPtahVersionAsOneExactArgument(t *testing.T) {
	t.Parallel()

	const version = "v1 # exact-build"
	objects := renderChart(t, "--set-string", "execution.ptahVersion="+version)
	deployment := mustObject(t, objects, "Deployment", releaseName+"-ptah-operator")
	containers, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || len(containers) != 1 {
		t.Fatalf("manager Deployment containers = %d, want 1", len(containers))
	}
	managerArgs := stringSlice(containers[0].(map[string]any)["args"])
	if !slices.Contains(managerArgs, "--ptah-version="+version) {
		t.Fatalf("manager args did not preserve the exact Ptah version: %v", managerArgs)
	}
}

func TestExternalControllerServiceAccountUsesReleaseEpochName(t *testing.T) {
	t.Parallel()

	const base = "platform-controller"
	objects := renderChart(t,
		"--set", "serviceAccount.create=false",
		"--set-string", "serviceAccount.name="+base,
	)
	deployment := mustObject(t, objects, "Deployment", releaseName+"-ptah-operator")
	name, found, err := unstructured.NestedString(deployment.Object, "spec", "template", "spec", "serviceAccountName")
	if err != nil || !found {
		t.Fatalf("external controller Deployment serviceAccountName: found=%t err=%v", found, err)
	}
	if want := base + "-v1"; name != want {
		t.Fatalf("external controller ServiceAccount name = %q, want %q", name, want)
	}
	for _, object := range objects {
		if object.GetKind() == "ServiceAccount" && object.GetName() == name {
			t.Fatalf("chart rendered user-owned external ServiceAccount %s/%s", releaseNamespace, name)
		}
	}
}

func TestExternalControllerServiceAccountBaseReservesEpochSuffix(t *testing.T) {
	t.Parallel()

	if _, err := renderChartCommand(t,
		"--set", "serviceAccount.create=false",
		"--set-string", "serviceAccount.name="+strings.Repeat("a", 242),
	); err == nil {
		t.Fatal("chart accepted an external controller ServiceAccount base that cannot fit every release epoch")
	}
}

func TestManagedControllerServiceAccountBaseKeepsFullConfigurationRange(t *testing.T) {
	t.Parallel()

	if _, err := renderChartCommand(t,
		"--set-string", "serviceAccount.name="+strings.Repeat("a", 242),
	); err != nil {
		t.Fatalf("chart rejected a valid managed controller ServiceAccount base: %v", err)
	}
}

func TestConfiguredPriorityClassIsLimitedToReconcileHookJob(t *testing.T) {
	t.Parallel()

	const priorityClassName = "runtime-critical"
	objects := renderChart(t,
		"--set-string", "priorityClassName="+priorityClassName,
		"--set", "priorityClassValue=1000",
	)
	wantClassless := map[string]bool{
		"crd-manager-image-check":      false,
		"hook-identity-probe":          false,
		"crd-manager-preflight":        false,
		"crd-manager-teardown-quiesce": false,
		"crd-manager-teardown":         false,
	}
	reconcileJobs := 0
	for _, object := range objects {
		if object.GetKind() != "Job" {
			continue
		}
		component := object.GetLabels()["app.kubernetes.io/component"]
		weight := object.GetAnnotations()["helm.sh/hook-weight"]
		priorityClass, found, err := unstructured.NestedString(object.Object, "spec", "template", "spec", "priorityClassName")
		if err != nil {
			t.Fatalf("Job/%s priorityClassName: %v", object.GetName(), err)
		}
		if component == "crd-manager" && weight == "0" {
			reconcileJobs++
			if !found || priorityClass != priorityClassName {
				t.Fatalf("reconcile Job priorityClassName = %q, found=%t; want %q", priorityClass, found, priorityClassName)
			}
		} else {
			if found {
				t.Fatalf("bootstrap or teardown Job/%s unexpectedly uses priorityClassName %q", object.GetName(), priorityClass)
			}
			if _, expected := wantClassless[component]; expected {
				wantClassless[component] = true
			}
		}
		for _, field := range []string{"priority", "preemptionPolicy"} {
			if value, found, err := unstructured.NestedFieldNoCopy(object.Object, "spec", "template", "spec", field); err != nil {
				t.Fatalf("Job/%s %s: %v", object.GetName(), field, err)
			} else if found {
				t.Fatalf("Job/%s contains Pod-admission output field %s=%v", object.GetName(), field, value)
			}
		}
	}
	if reconcileJobs != 1 {
		t.Fatalf("configured priority class appears on %d reconcile hook Jobs, want exactly one", reconcileJobs)
	}
	for component, seen := range wantClassless {
		if !seen {
			t.Errorf("classless hook Job component %q was not rendered", component)
		}
	}
}

func renderChart(t *testing.T, additionalArgs ...string) []*unstructured.Unstructured {
	return renderChartInNamespace(t, releaseNamespace, additionalArgs...)
}

func renderChartInNamespace(t *testing.T, namespace string, additionalArgs ...string) []*unstructured.Unstructured {
	t.Helper()
	output, err := renderChartCommandInNamespace(t, namespace, additionalArgs...)
	if err != nil {
		// Never print renderer output: a generated chart render contains private
		// key material by design.
		t.Fatalf("helm template failed: %v", err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var objects []*unstructured.Unstructured
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode Helm object: %v", err)
		}
		if object.Object == nil || object.GetKind() == "" {
			continue
		}
		objects = append(objects, object)
	}
	return objects
}

func renderChartCommand(t *testing.T, additionalArgs ...string) ([]byte, error) {
	return renderChartCommandInNamespace(t, releaseNamespace, additionalArgs...)
}

func renderChartCommandInNamespace(t *testing.T, namespace string, additionalArgs ...string) ([]byte, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart render tests")
	}
	_, filename, _, _ := runtime.Caller(0)
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	args := []string{
		"template", releaseName, filepath.Join(repositoryRoot, "charts", "ptah-operator"),
		"--namespace", namespace,
		"--set-string", "image.digest=sha256:" + managerDigest,
		"--set-string", "execution.executorImage=example.invalid/ptah@sha256:" + executorDigest,
		"--set-string", "execution.runnerImage=example.invalid/operator@sha256:" + runnerDigest,
		"--set-string", "execution.ptahVersion=" + ptahVersion,
	}
	args = append(args, additionalArgs...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, helm, args...)
	temporaryHome := t.TempDir()
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(temporaryHome, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(temporaryHome, "config"),
		"HELM_DATA_HOME="+filepath.Join(temporaryHome, "data"),
	)
	return command.Output()
}

func mustObject(t *testing.T, objects []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind && object.GetName() == name {
			return object
		}
	}
	t.Fatalf("rendered object %s/%s was not found", kind, name)
	return nil
}

func mustNamespacedObject(
	t *testing.T,
	objects []*unstructured.Unstructured,
	kind string,
	namespace string,
	name string,
) *unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind && object.GetNamespace() == namespace && object.GetName() == name {
			return object
		}
	}
	t.Fatalf("rendered object %s/%s/%s was not found", kind, namespace, name)
	return nil
}

func assertObjectAbsentInNamespace(
	t *testing.T,
	objects []*unstructured.Unstructured,
	kind string,
	namespace string,
	name string,
) {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind && object.GetNamespace() == namespace && object.GetName() == name {
			t.Fatalf("rendered object %s/%s/%s must be absent", kind, namespace, name)
		}
	}
}

func assertExactCertificateRoleBinding(
	t *testing.T,
	objects []*unstructured.Unstructured,
	namespace string,
	name string,
	subjectName string,
	subjectNamespace string,
) {
	t.Helper()
	binding := mustNamespacedObject(t, objects, "RoleBinding", namespace, name)
	roleRef, found, err := unstructured.NestedStringMap(binding.Object, "roleRef")
	if err != nil || !found || !maps.Equal(roleRef, map[string]string{
		"apiGroup": "rbac.authorization.k8s.io",
		"kind":     "Role",
		"name":     name,
	}) {
		t.Fatalf("certificate RoleBinding %s/%s roleRef = %v, found=%t err=%v", namespace, name, roleRef, found, err)
	}
	subjects, found, err := unstructured.NestedSlice(binding.Object, "subjects")
	if err != nil || !found || len(subjects) != 1 {
		t.Fatalf("certificate RoleBinding %s/%s subjects = %#v, found=%t err=%v", namespace, name, subjects, found, err)
	}
	subject, ok := subjects[0].(map[string]any)
	if !ok || !reflect.DeepEqual(subject, map[string]any{
		"kind":      "ServiceAccount",
		"name":      subjectName,
		"namespace": subjectNamespace,
	}) {
		t.Fatalf("certificate RoleBinding %s/%s subject = %#v", namespace, name, subjects[0])
	}
}

func objectRules(t *testing.T, object *unstructured.Unstructured) []map[string]any {
	t.Helper()
	rawRules, found, err := unstructured.NestedSlice(object.Object, "rules")
	if err != nil || !found {
		t.Fatalf("%s/%s has no rules", object.GetKind(), object.GetName())
	}
	rules := make([]map[string]any, 0, len(rawRules))
	for _, rawRule := range rawRules {
		rules = append(rules, rawRule.(map[string]any))
	}
	return rules
}

func assertExactRule(
	t *testing.T,
	object *unstructured.Unstructured,
	apiGroup string,
	resource string,
	resourceNames []string,
	verbs []string,
) {
	t.Helper()
	for _, rule := range objectRules(t, object) {
		if !slices.Contains(stringSlice(rule["apiGroups"]), apiGroup) || !slices.Contains(stringSlice(rule["resources"]), resource) {
			continue
		}
		gotNames := stringSlice(rule["resourceNames"])
		gotVerbs := stringSlice(rule["verbs"])
		slices.Sort(gotNames)
		slices.Sort(resourceNames)
		slices.Sort(gotVerbs)
		slices.Sort(verbs)
		if !slices.Equal(gotNames, resourceNames) || !slices.Equal(gotVerbs, verbs) {
			continue
		}
		return
	}
	t.Fatalf("%s/%s has no exact rule for %s/%s with resourceNames=%v verbs=%v", object.GetKind(), object.GetName(), apiGroup, resource, resourceNames, verbs)
}

func assertNoResourceVerb(
	t *testing.T,
	object *unstructured.Unstructured,
	apiGroup string,
	resource string,
	verb string,
) {
	t.Helper()
	for _, rule := range objectRules(t, object) {
		if slices.Contains(stringSlice(rule["apiGroups"]), apiGroup) &&
			slices.Contains(stringSlice(rule["resources"]), resource) &&
			slices.Contains(stringSlice(rule["verbs"]), verb) {
			t.Fatalf("%s/%s unexpectedly grants %s on %s/%s", object.GetKind(), object.GetName(), verb, apiGroup, resource)
		}
	}
}

func assertManagerTLSProjection(t *testing.T, deployment *unstructured.Unstructured, secretName string) {
	t.Helper()
	volumes, found, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "volumes")
	if err != nil || !found {
		t.Fatal("manager Deployment has no volumes")
	}
	for _, rawVolume := range volumes {
		volume := rawVolume.(map[string]any)
		if volume["name"] != "webhook-cert" {
			continue
		}
		secret := volume["secret"].(map[string]any)
		if secret["secretName"] != secretName {
			t.Fatalf("manager certificate volume uses Secret %v, want %q", secret["secretName"], secretName)
		}
		items := secret["items"].([]any)
		keys := make([]string, 0, len(items))
		for _, rawItem := range items {
			keys = append(keys, rawItem.(map[string]any)["key"].(string))
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"tls.crt", "tls.key"}) {
			t.Fatalf("manager certificate projection keys = %v, want only tls.crt and tls.key", keys)
		}
		return
	}
	t.Fatal("manager Deployment has no webhook-cert volume")
}

func stringSlice(value any) []string {
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.(string))
	}
	return result
}

func requiredArgumentValue(t *testing.T, args []string, prefix string) string {
	t.Helper()
	var value string
	for _, arg := range args {
		if !strings.HasPrefix(arg, prefix) {
			continue
		}
		if value != "" {
			t.Fatalf("rotator args contain duplicate %q arguments: %v", prefix, args)
		}
		value = strings.TrimPrefix(arg, prefix)
	}
	if value == "" {
		t.Fatalf("rotator args do not contain a nonempty %q argument: %v", prefix, args)
	}
	return value
}

func assertMutatingCanaryStaticWebhookContract(
	t *testing.T,
	configuration *unstructured.Unstructured,
	want admissionregistrationv1.MutatingWebhook,
) {
	t.Helper()
	raw := exactRenderedWebhook(t, configuration, want.Name)
	var got admissionregistrationv1.MutatingWebhook
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(raw, &got); err != nil {
		t.Fatalf("decode rendered mutating canary webhook %q: %v", want.Name, err)
	}
	got.ClientConfig.CABundle = nil
	want.ClientConfig.CABundle = nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rendered mutating canary webhook %q differs from compiled static contract:\n got: %#v\nwant: %#v", want.Name, got, want)
	}
}

func assertValidatingCanaryStaticWebhookContract(
	t *testing.T,
	configuration *unstructured.Unstructured,
	want admissionregistrationv1.ValidatingWebhook,
) {
	t.Helper()
	raw := exactRenderedWebhook(t, configuration, want.Name)
	var got admissionregistrationv1.ValidatingWebhook
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(raw, &got); err != nil {
		t.Fatalf("decode rendered validating canary webhook %q: %v", want.Name, err)
	}
	got.ClientConfig.CABundle = nil
	want.ClientConfig.CABundle = nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rendered validating canary webhook %q differs from compiled static contract:\n got: %#v\nwant: %#v", want.Name, got, want)
	}
}

func exactRenderedWebhook(
	t *testing.T,
	configuration *unstructured.Unstructured,
	name string,
) map[string]any {
	t.Helper()
	rawWebhooks, found, err := unstructured.NestedSlice(configuration.Object, "webhooks")
	if err != nil || !found {
		t.Fatalf("%s/%s webhooks: found=%v err=%v", configuration.GetKind(), configuration.GetName(), found, err)
	}
	var result map[string]any
	for _, rawWebhook := range rawWebhooks {
		webhook, ok := rawWebhook.(map[string]any)
		if !ok || webhook["name"] != name {
			continue
		}
		if result != nil {
			t.Fatalf("canary webhook %q appears more than once in %s/%s", name, configuration.GetKind(), configuration.GetName())
		}
		result = webhook
	}
	if result == nil {
		t.Fatalf("canary webhook %q is absent from %s/%s", name, configuration.GetKind(), configuration.GetName())
	}
	return result
}
