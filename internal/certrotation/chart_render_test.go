package certrotation_test

import (
	"bytes"
	"context"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/stokaro/ptah-operator/internal/certrotation"
)

const (
	releaseName      = "rotation-test"
	releaseNamespace = "ptah-system"
	managerDigest    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	executorDigest   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	runnerDigest     = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestGeneratedCertificateLifecycleRender(t *testing.T) {
	t.Parallel()
	objects := renderChart(t)
	managerName := releaseName + "-ptah-operator"
	rotatorName := releaseName + "-ptah-operator-cert-rotator"
	secretName := releaseName + "-ptah-operator-webhook-cert"
	leaseName := releaseName + "-ptah-operator-cert-rotation"
	configurationName := "ptah-operator-admission"

	secret := mustObject(t, objects, "Secret", secretName)
	if !maps.Equal(secret.GetLabels(), map[string]string{certrotation.GeneratedSecretLabel: certrotation.GeneratedSecretLabelValue}) {
		t.Fatalf("generated Secret labels = %v, want only the recovery guard label", secret.GetLabels())
	}
	for _, key := range []string{"ca.crt", "ca.key", "tls.crt", "tls.key"} {
		if _, found, err := unstructured.NestedString(secret.Object, "data", key); err != nil || !found {
			t.Errorf("generated Secret data is missing %q", key)
		}
	}

	managerRole := mustObject(t, objects, "ClusterRole", managerName)
	for _, rule := range objectRules(t, managerRole) {
		if slices.Contains(stringSlice(rule["resources"]), "secrets") {
			t.Fatal("manager ClusterRole grants Secret access")
		}
	}

	role := mustObject(t, objects, "Role", rotatorName)
	assertExactRule(t, role, "", "secrets", []string{secretName}, []string{"get", "update"})
	assertNoResourceVerb(t, role, "", "secrets", "create")
	assertExactRule(t, role, "coordination.k8s.io", "leases", []string{leaseName}, []string{"get", "update"})
	assertExactRule(t, role, "discovery.k8s.io", "endpointslices", nil, []string{"list"})

	clusterRole := mustObject(t, objects, "ClusterRole", rotatorName)
	assertExactRule(t, clusterRole, "admissionregistration.k8s.io", "mutatingwebhookconfigurations", []string{configurationName}, []string{"get", "update"})
	assertExactRule(t, clusterRole, "admissionregistration.k8s.io", "validatingwebhookconfigurations", []string{configurationName}, []string{"get", "update"})
	assertNoResourceVerb(t, clusterRole, "admissionregistration.k8s.io", "validatingadmissionpolicies", "get")
	assertNoResourceVerb(t, clusterRole, "admissionregistration.k8s.io", "validatingadmissionpolicybindings", "get")
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
		"--mutating-webhook-names=mapproval.operator.ptah.dev",
		"--validating-webhook-names=vapproval.operator.ptah.dev,vpodintent.operator.ptah.dev",
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
	if len(ports) != 1 {
		t.Fatalf("rotator ports = %d, want 1", len(ports))
	}
	port := ports[0].(map[string]any)
	if port["name"] != "health" || port["containerPort"] != int64(8081) {
		t.Errorf("rotator health port = %#v", port)
	}
	assertHTTPProbe(t, container, "livenessProbe", "/healthz")
	assertHTTPProbe(t, container, "readinessProbe", "/readyz")

	deployment := mustObject(t, objects, "Deployment", managerName)
	assertManagerTLSProjection(t, deployment, secretName)
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
	for _, setting := range []string{
		"certificateRotation.probeTimeout=0s",
		"certificateRotation.interval=0s",
		"certificateRotation.operationTimeout=0s",
		"certificateRotation.retryInitial=0s",
		"certificateRotation.retryMax=0s",
		"certificateRotation.healthPort=0",
		"certificateRotation.recreateMissingSecret=not-a-boolean",
		"webhook.existingSecret=Bad_Name",
	} {
		t.Run(setting, func(t *testing.T) {
			if _, err := renderChartCommand(t, "--set-string", setting); err == nil {
				t.Fatalf("Helm accepted invalid value %q", setting)
			}
		})
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
		"object.metadata.labels ==",
		"operator.ptah.dev/generated-webhook-certificate",
		"object.metadata.annotations.size() == 0",
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

func renderChart(t *testing.T, additionalArgs ...string) []*unstructured.Unstructured {
	t.Helper()
	output, err := renderChartCommand(t, additionalArgs...)
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
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart render tests")
	}
	_, filename, _, _ := runtime.Caller(0)
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	args := []string{
		"template", releaseName, filepath.Join(repositoryRoot, "charts", "ptah-operator"),
		"--namespace", releaseNamespace,
		"--set-string", "image.digest=sha256:" + managerDigest,
		"--set-string", "execution.executorImage=example.invalid/ptah@sha256:" + executorDigest,
		"--set-string", "execution.runnerImage=example.invalid/operator@sha256:" + runnerDigest,
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
