package crdupgrade

// These tests intentionally use the package internals: the public behavior is
// an admission policy, and its safety depends on exact CEL/spec construction
// plus UID/resourceVersion transitions that are not externally observable.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/certrotation"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestStagingSecretGuardAdmissionContract(t *testing.T) {
	t.Parallel()
	guard := newReadyStagingSecretGuard(t)
	policy, binding, err := guard.ExpectedObjects()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatalf("failurePolicy = %v, want Fail", policy.Spec.FailurePolicy)
	}
	if policy.Spec.ParamKind == nil || policy.Spec.ParamKind.APIVersion != "v1" || policy.Spec.ParamKind.Kind != "ConfigMap" {
		t.Fatalf("paramKind = %#v, want v1 ConfigMap", policy.Spec.ParamKind)
	}
	if policy.Spec.MatchConstraints == nil || policy.Spec.MatchConstraints.MatchPolicy == nil ||
		*policy.Spec.MatchConstraints.MatchPolicy != admissionregistrationv1.Exact {
		t.Fatalf("policy matchPolicy = %v, want Exact", policy.Spec.MatchConstraints)
	}
	if binding.Spec.MatchResources == nil || binding.Spec.MatchResources.MatchPolicy == nil ||
		*binding.Spec.MatchResources.MatchPolicy != admissionregistrationv1.Exact {
		t.Fatalf("binding matchPolicy = %v, want Exact", binding.Spec.MatchResources)
	}
	secretRule := policy.Spec.MatchConstraints.ResourceRules[0]
	if !reflect.DeepEqual(secretRule.Operations, []admissionregistrationv1.OperationType{
		admissionregistrationv1.Create,
		admissionregistrationv1.Update,
		admissionregistrationv1.Delete,
	}) || !reflect.DeepEqual(secretRule.APIGroups, []string{""}) ||
		!reflect.DeepEqual(secretRule.APIVersions, []string{"v1"}) ||
		!reflect.DeepEqual(secretRule.Resources, []string{"secrets"}) ||
		!reflect.DeepEqual(secretRule.ResourceNames, []string{"ptah-webhook-cert-stage"}) {
		t.Fatalf("staging Secret rule = %#v, want exact named UPDATE/DELETE", secretRule)
	}
	if !reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) {
		t.Fatalf("validationActions = %v, want [Deny]", binding.Spec.ValidationActions)
	}
	if binding.Spec.ParamRef == nil || binding.Spec.ParamRef.Name != ReleaseActivationName ||
		binding.Spec.ParamRef.Namespace != guard.rollout.ReleaseNamespace ||
		binding.Spec.ParamRef.ParameterNotFoundAction == nil ||
		*binding.Spec.ParamRef.ParameterNotFoundAction != admissionregistrationv1.DenyAction {
		t.Fatalf("paramRef = %#v, want exact fail-closed release activation parameter", binding.Spec.ParamRef)
	}

	oldWithData := stagingSecretCELObject(guard, map[string]any{"candidate": "credential-material"})
	empty := stagingSecretCELObject(guard, nil)
	rotated := stagingSecretCELObject(guard, map[string]any{"candidate": "replacement"})
	createEmpty := stagingSecretCreateCELObject(guard, nil)
	createWithData := stagingSecretCreateCELObject(guard, map[string]any{"candidate": "credential-material"})
	params := stagingSecretActivationCELObject(guard, guard.rollout.ReleaseSequence, hookIdentityDigest(
		guard.rollout.ReleaseNamespace,
		guard.rollout.ReleaseName,
		guard.rollout.ReleaseSequence,
		guard.rollout.ManagerImage,
	))
	cleanup := stagingSecretRequest(guard, "DELETE", stagingCleanupUsername(guard))
	rotator := stagingSecretRequest(guard, "UPDATE", "system:serviceaccount:ptah-system:ptah-cert-rotator")
	foreign := stagingSecretRequest(guard, "UPDATE", "system:serviceaccount:ptah-system:foreign")
	wrongManager := stagingSecretCELObject(guard, map[string]any{"candidate": "credential-material"})
	wrongManager["metadata"].(map[string]any)["labels"].(map[string]any)[certrotation.HelmManagedByLabel] = "foreign"
	missingReleaseName := stagingSecretCELObject(guard, map[string]any{"candidate": "credential-material"})
	delete(missingReleaseName["metadata"].(map[string]any)["annotations"].(map[string]any), certrotation.HelmReleaseNameAnnotation)

	tests := []struct {
		name      string
		object    map[string]any
		oldObject map[string]any
		request   map[string]any
		params    map[string]any
		want      bool
	}{
		{name: "exact empty create", object: createEmpty, request: stagingSecretRequest(guard, "CREATE", "helm"), want: true},
		{name: "credential-bearing create denied", object: createWithData, request: stagingSecretRequest(guard, "CREATE", "helm"), want: false},
		{name: "rotator exact data update", object: rotated, oldObject: oldWithData, request: rotator, want: true},
		{name: "foreign data update", object: rotated, oldObject: oldWithData, request: foreign, want: false},
		{name: "foreign unchanged update", object: oldWithData, oldObject: oldWithData, request: foreign, want: true},
		{name: "cleanup atomically empties data", object: empty, oldObject: oldWithData, request: stagingSecretRequest(guard, "UPDATE", stagingCleanupUsername(guard)), want: true},
		{name: "cleanup cannot replace nonempty data", object: rotated, oldObject: oldWithData, request: stagingSecretRequest(guard, "UPDATE", stagingCleanupUsername(guard)), want: false},
		{name: "nonempty delete denied", oldObject: oldWithData, request: cleanup, want: false},
		{name: "empty exact cleanup delete allowed", oldObject: empty, request: cleanup, want: true},
		{
			name:      "stale cleanup identity denied",
			oldObject: empty,
			request:   cleanup,
			params:    stagingSecretActivationCELObject(guard, guard.rollout.ReleaseSequence+1, strings.Repeat("f", 64)),
			want:      false,
		},
		{name: "empty foreign delete denied", oldObject: empty, request: stagingSecretRequest(guard, "DELETE", "system:serviceaccount:ptah-system:foreign"), want: false},
		{name: "metadata mutation denied", object: stagingSecretCELObjectWithAnnotation(guard), oldObject: oldWithData, request: foreign, want: false},
		{name: "foreign Helm manager denied", object: wrongManager, oldObject: oldWithData, request: foreign, want: false},
		{name: "missing Helm release name denied", object: missingReleaseName, oldObject: oldWithData, request: foreign, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			parameter := params
			if test.params != nil {
				parameter = test.params
			}
			if !evaluatePolicyMatchConditions(t, policy, test.object, test.oldObject, test.request, parameter) {
				t.Fatal("exact staging Secret request did not match its policy")
			}
			results := evaluatePolicyValidations(t, policy, test.object, test.oldObject, test.request, parameter)
			got := true
			for _, result := range results {
				got = got && result
			}
			if got != test.want {
				t.Fatalf("policy allowed = %t, want %t; validations=%v", got, test.want, results)
			}
		})
	}
}

func TestStagingSecretGuardCleanupClearsThenDeletesExactIdentity(t *testing.T) {
	t.Parallel()
	guard := newReadyStagingSecretGuard(t)
	client := &stagingSecretTestClient{object: stagingSecretObject(guard, map[string][]byte{"candidate": []byte("credential-material")})}

	if err := guard.Cleanup(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if client.object != nil || client.updates != 1 || client.deletes != 1 {
		t.Fatalf("cleanup result: object=%v updates=%d deletes=%d", client.object, client.updates, client.deletes)
	}
	if client.deletedUID != "stage-uid" || client.deletedResourceVersion != "2" {
		t.Fatalf("delete preconditions = uid=%q rv=%q, want stage-uid/2", client.deletedUID, client.deletedResourceVersion)
	}
}

func TestStagingSecretGuardCleanupFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*stagingSecretTestClient)
		want   string
	}{
		{
			name: "foreign metadata",
			mutate: func(client *stagingSecretTestClient) {
				client.object.Annotations = map[string]string{"foreign": "true"}
			},
			want: "foreign or incomplete ownership metadata",
		},
		{
			name: "concurrent replacement during clear",
			mutate: func(client *stagingSecretTestClient) {
				client.replaceOnUpdate = true
			},
			want: "changed identity",
		},
		{
			name: "concurrent refill before delete",
			mutate: func(client *stagingSecretTestClient) {
				client.refillAfterUpdate = true
			},
			want: "changed identity or data before deletion",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			guard := newReadyStagingSecretGuard(t)
			client := &stagingSecretTestClient{object: stagingSecretObject(guard, map[string][]byte{"candidate": []byte("credential-material")})}
			test.mutate(client)
			err := guard.Cleanup(context.Background(), client)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Cleanup() error = %v, want containing %q", err, test.want)
			}
			if client.deletes != 0 {
				t.Fatalf("unsafe cleanup performed %d deletes", client.deletes)
			}
		})
	}
}

func TestStagingSecretGuardWiresExactTeardownInventories(t *testing.T) {
	t.Parallel()
	guard := newReadyStagingSecretGuard(t)
	contracts, err := teardownGuardContracts(guard.rollout)
	if err != nil {
		t.Fatal(err)
	}
	guardName := StagingSecretGuardPolicyName(guard.rollout.ReleaseNamespace, guard.rollout.ReleaseName)
	count := 0
	for _, contract := range contracts {
		if contract.name == guardName {
			count++
			if !contract.parameterized {
				t.Fatal("staging Secret guard teardown contract must retire before its activation parameter")
			}
		}
	}
	if count != 1 {
		t.Fatalf("release teardown staging guard contracts = %d, want 1", count)
	}

	fixture := newPrivilegeTeardownFixture(t, true, true)
	wantSecret := fixture.teardown.certificateStagingSecretName()
	foundRule := false
	for _, authorization := range fixture.teardown.authorizationContracts() {
		if authorization.name != fixture.cleanupPrivilege || authorization.namespace != fixture.guard.ReleaseNamespace || authorization.cluster {
			continue
		}
		for _, rule := range authorization.rules {
			if reflect.DeepEqual(rule.APIGroups, []string{""}) &&
				reflect.DeepEqual(rule.Resources, []string{"secrets"}) &&
				reflect.DeepEqual(rule.ResourceNames, []string{wantSecret}) &&
				reflect.DeepEqual(rule.Verbs, []string{"get", "update", "delete"}) {
				foundRule = true
			}
		}
	}
	if !foundRule {
		t.Fatalf("cleanup Role omits exact get/update/delete grant for Secret %q", wantSecret)
	}

	guard.rollout.CertificateRuntimeEnabled = false
	contracts, err = teardownGuardContracts(guard.rollout)
	if err != nil {
		t.Fatal(err)
	}
	for _, contract := range contracts {
		if contract.name == guardName {
			t.Fatalf("external-certificate teardown includes staging guard %q", guardName)
		}
	}
}

func TestRenderedStagingSecretGuardMatchesCompiledContract(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for staging Secret guard render tests")
	}
	rendered, err := runStagingSecretGuardHelm(t, helm, "client", "")
	if err != nil {
		t.Fatalf("render staging Secret guard: %v\n%s", err, rendered)
	}
	actualPolicy, actualBinding := renderedStagingSecretGuardPair(t, rendered)
	rollout := renderedStagingSecretRollout(t)
	expectedPolicy, expectedBinding, err := NewStagingSecretGuard(rollout).ExpectedObjects()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actualPolicy.ObjectMeta, expectedPolicy.ObjectMeta) || !reflect.DeepEqual(actualPolicy.Spec, expectedPolicy.Spec) {
		t.Fatalf("rendered staging Secret policy differs from compiled contract\nactual=%#v\nexpected=%#v", actualPolicy, expectedPolicy)
	}
	if !reflect.DeepEqual(actualBinding.ObjectMeta, expectedBinding.ObjectMeta) || !reflect.DeepEqual(actualBinding.Spec, expectedBinding.Spec) {
		t.Fatalf("rendered staging Secret binding differs from compiled contract\nactual=%#v\nexpected=%#v", actualBinding, expectedBinding)
	}
}

func TestRenderedStagingSecretGuardRejectsMutatedLiveContract(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for staging Secret guard live lookup tests")
	}
	rendered, err := runStagingSecretGuardHelm(t, helm, "client", "")
	if err != nil {
		t.Fatalf("render expected staging Secret guard: %v\n%s", err, rendered)
	}
	policy, binding := renderedStagingSecretGuardPair(t, rendered)
	policy = persistedServiceAccountObjectPolicy(policy)
	binding = persistedServiceAccountObjectBinding(binding)
	ignore := admissionregistrationv1.Ignore
	policy.Spec.FailurePolicy = &ignore

	var policyRead atomic.Bool
	server := httptest.NewServer(serviceAccountObjectGuardAPIServer(t, policy, binding, &policyRead))
	t.Cleanup(server.Close)
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	config := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
contexts:
- name: test
  context:
    cluster: test
    namespace: ptah-e2e
    user: test
current-context: test
users:
- name: test
  user:
    token: test
`, server.URL)
	if err := os.WriteFile(kubeconfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	output, renderErr := runStagingSecretGuardHelm(t, helm, "server", kubeconfig)
	if renderErr == nil {
		t.Fatalf("server-side Helm render accepted a mutated retained policy:\n%s", output)
	}
	if !policyRead.Load() {
		t.Fatalf("server-side Helm render did not read the retained policy: %v\n%s", renderErr, output)
	}
	if !bytes.Contains(output, []byte("differs from the exact live contract")) {
		t.Fatalf("server-side Helm render failed for the wrong reason: %v\n%s", renderErr, output)
	}
}

func newReadyStagingSecretGuard(t *testing.T) *StagingSecretGuard {
	t.Helper()
	rollout, policies, bindings, _ := readyRolloutGuard()
	rollout.CertificateRuntimeEnabled = true
	guard := NewStagingSecretGuard(rollout)
	policy, binding, err := guard.ExpectedObjects()
	if err != nil {
		t.Fatal(err)
	}
	policies.objects[policy.Name] = persistedServiceAccountObjectPolicy(readyPolicy(policy))
	bindings.objects[binding.Name] = persistedServiceAccountObjectBinding(binding)
	return guard
}

func stagingSecretCELObject(guard *StagingSecretGuard, data map[string]any) map[string]any {
	object := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":            "ptah-webhook-cert-stage",
			"namespace":       guard.rollout.ReleaseNamespace,
			"uid":             "stage-uid",
			"resourceVersion": "1",
			"labels": map[string]any{
				certrotation.StagingSecretLabel: certrotation.StagingSecretLabelValue,
				certrotation.HelmManagedByLabel: certrotation.HelmManagedByLabelValue,
			},
			"annotations": map[string]any{
				certrotation.HelmReleaseNameAnnotation:      guard.rollout.ReleaseName,
				certrotation.HelmReleaseNamespaceAnnotation: guard.rollout.ReleaseNamespace,
			},
		},
		"type": "Opaque",
	}
	if data != nil {
		object["data"] = data
	}
	return object
}

// stagingSecretCreateCELObject is the object validating admission sees on a
// CREATE: the API server has filled uid and creationTimestamp by then, and
// storage assigns the resourceVersion only afterwards.
func stagingSecretCreateCELObject(guard *StagingSecretGuard, data map[string]any) map[string]any {
	object := stagingSecretCELObject(guard, data)
	metadata := object["metadata"].(map[string]any)
	metadata["creationTimestamp"] = "2026-09-06T17:36:02Z"
	delete(metadata, "resourceVersion")
	return object
}

func stagingSecretCELObjectWithAnnotation(guard *StagingSecretGuard) map[string]any {
	object := stagingSecretCELObject(guard, map[string]any{"candidate": "credential-material"})
	object["metadata"].(map[string]any)["annotations"] = map[string]any{"foreign": "true"}
	return object
}

func stagingSecretActivationCELObject(guard *StagingSecretGuard, target int32, attempt string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":            ReleaseActivationName,
			"namespace":       guard.rollout.ReleaseNamespace,
			"uid":             "activation-uid",
			"resourceVersion": "1",
		},
		"data": map[string]any{
			activeReleaseDataKey:                "0",
			controllerCredentialsDataKey:        string(ControllerCredentialsDraining),
			controllerCredentialsTargetDataKey:  strconv.FormatInt(int64(target), 10),
			controllerCredentialsAttemptDataKey: attempt,
		},
	}
}

func stagingSecretRequest(guard *StagingSecretGuard, operation, username string) map[string]any {
	return map[string]any{
		"operation": operation,
		"namespace": guard.rollout.ReleaseNamespace,
		"name":      "ptah-webhook-cert-stage",
		"dryRun":    false,
		"resource": map[string]any{
			"group":    "",
			"version":  "v1",
			"resource": "secrets",
		},
		"userInfo": map[string]any{
			"username": username,
			"groups": []any{
				"system:serviceaccounts",
				"system:serviceaccounts:" + guard.rollout.ReleaseNamespace,
				"system:authenticated",
			},
		},
	}
}

func stagingCleanupUsername(guard *StagingSecretGuard) string {
	name, _ := TeardownServiceAccountName(guard.rollout.HookServiceAccountName, guard.rollout.ReleaseSequence)
	return "system:serviceaccount:" + guard.rollout.ReleaseNamespace + ":" + name
}

func stagingSecretObject(guard *StagingSecretGuard, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            "ptah-webhook-cert-stage",
			Namespace:       guard.rollout.ReleaseNamespace,
			UID:             types.UID("stage-uid"),
			ResourceVersion: "1",
			Labels: map[string]string{
				certrotation.StagingSecretLabel: certrotation.StagingSecretLabelValue,
				certrotation.HelmManagedByLabel: certrotation.HelmManagedByLabelValue,
			},
			Annotations: map[string]string{
				certrotation.HelmReleaseNameAnnotation:      guard.rollout.ReleaseName,
				certrotation.HelmReleaseNamespaceAnnotation: guard.rollout.ReleaseNamespace,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

type stagingSecretTestClient struct {
	object                 *corev1.Secret
	updates                int
	deletes                int
	replaceOnUpdate        bool
	refillAfterUpdate      bool
	readAfterUpdate        bool
	deletedUID             types.UID
	deletedResourceVersion string
}

func (c *stagingSecretTestClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.Secret, error) {
	if c.object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	if c.readAfterUpdate && c.refillAfterUpdate {
		object := c.object.DeepCopy()
		object.Data = map[string][]byte{"candidate": []byte("refilled")}
		return object, nil
	}
	return c.object.DeepCopy(), nil
}

func (c *stagingSecretTestClient) Update(_ context.Context, object *corev1.Secret, _ metav1.UpdateOptions) (*corev1.Secret, error) {
	c.updates++
	if c.object == nil || object.UID != c.object.UID || object.ResourceVersion != c.object.ResourceVersion {
		return nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, object.Name, errors.New("identity precondition failed"))
	}
	updated := object.DeepCopy()
	updated.ResourceVersion = "2"
	if c.replaceOnUpdate {
		updated.UID = "replacement"
	}
	c.object = updated.DeepCopy()
	c.readAfterUpdate = true
	return updated, nil
}

func (c *stagingSecretTestClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	c.deletes++
	if c.object == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil {
		return errors.New("missing deletion preconditions")
	}
	c.deletedUID = *options.Preconditions.UID
	c.deletedResourceVersion = *options.Preconditions.ResourceVersion
	if c.deletedUID != c.object.UID || c.deletedResourceVersion != c.object.ResourceVersion {
		return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, name, errors.New("identity precondition failed"))
	}
	c.object = nil
	return nil
}

func renderedStagingSecretRollout(t *testing.T) *RolloutGuard {
	t.Helper()
	rollout := runtimePodGuardFixture()
	rollout.ReleaseName = "ptah-e2e"
	rollout.ReleaseNamespace = "ptah-e2e"
	rollout.CoordinationNamespace = "ptah-e2e"
	rollout.ManagerImage = renderedGuardManagerImage
	rollout.ControllerDeploymentName = "ptah-e2e-ptah-operator"
	rollout.CertificateDeploymentName = "ptah-e2e-ptah-operator-cert-rotator"
	rollout.ControllerServiceAccountName = "ptah-e2e-ptah-operator-v1-" + hookIdentityDigest(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)[:12]
	rollout.ControllerServiceAccountManaged = true
	rollout.HookServiceAccountName = "ptah-e2e-ptah-operator-crd-v1-" + hookIdentityDigest(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)[:12]
	rollout.WebhookSecretName = "ptah-e2e-ptah-operator-webhook-cert"
	rollout.CertificateRuntimeEnabled = true
	replaceRuntimeArg(rollout.CertificateArgs, "--staging-secret-name=", "--staging-secret-name=ptah-e2e-ptah-operator-cert-rotation-stage")
	return rollout
}

func runStagingSecretGuardHelm(t *testing.T, helm, dryRun, kubeconfig string) ([]byte, error) {
	t.Helper()
	args := []string{
		"template", "ptah-e2e", serviceAccountObjectGuardChartPath(t),
		"--namespace", "ptah-e2e",
		"--dry-run=" + dryRun,
		"--show-only", "templates/staging-secret-guard.yaml",
		"--disable-openapi-validation",
		"--set-string", "image.digest=sha256:" + strings.Repeat("2", 64),
		"--set-string", "execution.executorImage=e2e.invalid/executor@sha256:" + strings.Repeat("0", 64),
		"--set-string", "execution.runnerImage=e2e.invalid/runner@sha256:" + strings.Repeat("1", 64),
		"--set-string", "execution.ptahVersion=e2e-explicit-version",
	}
	if kubeconfig != "" {
		args = append(args, "--kubeconfig", kubeconfig)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, helm, args...)
	temporaryHome := t.TempDir()
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(temporaryHome, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(temporaryHome, "config"),
		"HELM_DATA_HOME="+filepath.Join(temporaryHome, "data"),
	)
	return command.CombinedOutput()
}

func renderedStagingSecretGuardPair(t *testing.T, rendered []byte) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	t.Helper()
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(rendered))
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var typeMeta metav1.TypeMeta
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			t.Fatal(err)
		}
		switch typeMeta.Kind {
		case "ValidatingAdmissionPolicy":
			var object admissionregistrationv1.ValidatingAdmissionPolicy
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(object.Name, stagingSecretGuardNamePrefix) {
				policy = &object
			}
		case "ValidatingAdmissionPolicyBinding":
			var object admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(object.Name, stagingSecretGuardNamePrefix) {
				binding = &object
			}
		}
	}
	if policy == nil || binding == nil {
		t.Fatal("rendered staging Secret guard pair is incomplete")
	}
	return policy, binding
}
