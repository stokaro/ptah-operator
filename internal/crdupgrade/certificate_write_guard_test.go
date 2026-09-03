package crdupgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	celgo "github.com/google/cel-go/cel"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

type certificateWebhookCELTarget struct {
	serviceNamespace string
	serviceName      string
	port             *int64
	url              string
}

type certificateWebhookCELEntry struct {
	name   string
	target certificateWebhookCELTarget
	bundle []byte
}

func TestCertificateWriteGuardNamesAreStableDistinctAndVersioned(t *testing.T) {
	t.Parallel()

	mutating := CertificateMutatingWriteGuardPolicyName("ptah-system", "ptah")
	validating := CertificateValidatingWriteGuardPolicyName("ptah-system", "ptah")
	for name, prefix := range map[string]string{
		mutating:   certificateMutatingWriteGuardNamePrefix,
		validating: certificateValidatingWriteGuardNamePrefix,
	} {
		if !strings.HasPrefix(name, prefix) || len(name) > 63 {
			t.Fatalf("certificate write guard name %q is not bounded and versioned", name)
		}
	}
	if mutating == validating {
		t.Fatal("typed certificate write guards share one policy name")
	}
	if mutating != CertificateMutatingWriteGuardPolicyName("ptah-system", "ptah") ||
		validating != CertificateValidatingWriteGuardPolicyName("ptah-system", "ptah") {
		t.Fatal("certificate write guard names are not deterministic")
	}
	if mutating == CertificateMutatingWriteGuardPolicyName("other", "ptah") ||
		mutating == CertificateMutatingWriteGuardPolicyName("ptah-system", "other") {
		t.Fatal("certificate write guard name does not bind both release identity fields")
	}

	rollout := runtimePodGuardFixture()
	other := *rollout
	other.ReleaseSequence++
	other.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	if CertificateMutatingWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName) !=
		CertificateMutatingWriteGuardPolicyName(other.ReleaseNamespace, other.ReleaseName) ||
		CertificateValidatingWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName) !=
			CertificateValidatingWriteGuardPolicyName(other.ReleaseNamespace, other.ReleaseName) {
		t.Fatal("stable certificate write guard names changed with candidate release identity")
	}
}

func TestCertificateWriteGuardsAreTypedExactAndFailClosed(t *testing.T) {
	t.Parallel()

	guard := testCertificateWriteGuard()
	entries := guard.entries()
	if len(entries) != 2 {
		t.Fatalf("certificate write guard entries = %d, want two typed policies", len(entries))
	}
	for _, entry := range entries {
		entry := entry
		t.Run(entry.resource, func(t *testing.T) {
			t.Parallel()
			policy := guard.policy(entry)
			binding := guard.binding(entry)
			if policy.Spec.ParamKind != nil || binding.Spec.ParamRef != nil {
				t.Fatal("certificate write guard must not depend on admission parameters")
			}
			if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
				t.Fatal("certificate write guard is not fail-closed")
			}
			assertExactCertificateWriteMatch(t, policy.Spec.MatchConstraints, entry.resource)
			assertExactCertificateWriteMatch(t, binding.Spec.MatchResources, entry.resource)
			wantUsername := `request.userInfo.username == "system:serviceaccount:ptah-system:ptah-certificate"`
			if !reflect.DeepEqual(policy.Spec.MatchConditions, []admissionregistrationv1.MatchCondition{{
				Name: "exact-certificate-service-account", Expression: wantUsername,
			}}) {
				t.Fatalf("certificate caller match is not exact: %#v", policy.Spec.MatchConditions)
			}
			if binding.Spec.PolicyName != policy.Name ||
				!reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) {
				t.Fatalf("certificate write binding is not exact deny-only enforcement: %#v", binding.Spec)
			}
		})
	}
}

func TestCertificateWriteGuardCELContracts(t *testing.T) {
	t.Parallel()

	guard := testCertificateWriteGuard()
	for _, entry := range guard.entries() {
		entry := entry
		t.Run(entry.resource, func(t *testing.T) {
			t.Parallel()
			validations := guard.policy(entry).Spec.Validations
			if len(validations) != 3 {
				t.Fatalf("%s validations = %d, want 3", entry.resource, len(validations))
			}
			if validations[0].Expression != certificateMetadataValidation() {
				t.Fatalf("%s metadata is not immutable: %q", entry.resource, validations[0].Expression)
			}
			for _, marker := range []string{"metadata.selfLink", "metadata.labels", "metadata.annotations", "metadata.ownerReferences", "metadata.finalizers", "object.webhooks != oldObject.webhooks", "generation + 1"} {
				if !strings.Contains(validations[0].Expression, marker) {
					t.Fatalf("%s metadata contract lacks %q", entry.resource, marker)
				}
			}
			if strings.Contains(validations[0].Expression, "metadata.managedFields") {
				t.Fatalf("%s metadata contract freezes server-managed fields", entry.resource)
			}
			if validations[1].Expression != certificateWebhookNamesValidation() {
				t.Fatalf("%s ordered webhook inventory differs: %q", entry.resource, validations[1].Expression)
			}
			for index, validation := range validations {
				if validation.Message != entry.denialMessage {
					t.Fatalf("%s validation %d lacks its typed denial message", entry.resource, index)
				}
			}
			if strings.Contains(validations[1].Expression, validatingApprovalWebhookName) ||
				strings.Contains(validations[1].Expression, podIntentWebhookName) ||
				strings.Contains(validations[1].Expression, controllerWriteWebhookName) {
				t.Fatalf("%s inventory contract is tied to one release's entry list", entry.resource)
			}
			validation := validations[2]
			for _, marker := range []string{
				"object.webhooks.all",
				"oldObject.webhooks.exists",
				"clientConfig.service",
				`clientConfig.service.namespace == "ptah-system"`,
				`clientConfig.service.name == "ptah-webhook"`,
				"clientConfig.service.port == 443",
				"clientConfig.url",
				"clientConfig.caBundle",
				"clientConfig.caBundle == previous.clientConfig.caBundle",
				"caBundle.size() > 0",
				"caBundle.size() <= " + strconv.Itoa(maximumCertificateCABundleBytes),
				".rules",
				".failurePolicy",
				".matchPolicy",
				".namespaceSelector",
				".objectSelector",
				".sideEffects",
				".timeoutSeconds",
				".admissionReviewVersions",
				".matchConditions",
			} {
				if !strings.Contains(validation.Expression, marker) {
					t.Fatalf("%s entry contract lacks %q", entry.resource, marker)
				}
			}
			if got := strings.Contains(validation.Expression, ".reinvocationPolicy"); got != entry.includeReinvocation {
				t.Fatalf("%s reinvocation equality = %t, want %t", entry.resource, got, entry.includeReinvocation)
			}
		})
	}
	if !reflect.DeepEqual(certificateValidatingWebhookNames(), []string{
		validatingApprovalWebhookName,
		podIntentWebhookName,
		controllerWriteWebhookName,
	}) {
		t.Fatalf("validating webhook order is not the exact release inventory: %#v", certificateValidatingWebhookNames())
	}
}

func TestCertificateWebhookEntriesValidationEvaluatesServiceAuthority(t *testing.T) {
	t.Parallel()

	port443 := int64(certificateWebhookServicePort)
	port8443 := int64(8443)
	managedDefault := certificateWebhookCELTarget{serviceNamespace: "ptah-system", serviceName: "ptah-webhook"}
	managed443 := certificateWebhookCELTarget{serviceNamespace: "ptah-system", serviceName: "ptah-webhook", port: &port443}
	foreignService := certificateWebhookCELTarget{serviceNamespace: "ptah-system", serviceName: "foreign-webhook"}
	foreignURL := certificateWebhookCELTarget{url: "https://foreign.example/validate"}
	otherPort := certificateWebhookCELTarget{serviceNamespace: "ptah-system", serviceName: "ptah-webhook", port: &port8443}
	oldCA := []byte("old-ca")
	newCA := []byte("new-ca")

	tests := []struct {
		name string
		old  []certificateWebhookCELEntry
		new  []certificateWebhookCELEntry
		want bool
	}{
		{
			name: "default service port may rotate",
			old:  []certificateWebhookCELEntry{{name: "managed.example", target: managedDefault, bundle: oldCA}},
			new:  []certificateWebhookCELEntry{{name: "managed.example", target: managedDefault, bundle: newCA}},
			want: true,
		},
		{
			name: "explicit service port 443 may rotate",
			old:  []certificateWebhookCELEntry{{name: "managed.example", target: managed443, bundle: oldCA}},
			new:  []certificateWebhookCELEntry{{name: "managed.example", target: managed443, bundle: newCA}},
			want: true,
		},
		{
			name: "managed service bundle must remain nonempty",
			old:  []certificateWebhookCELEntry{{name: "managed.example", target: managedDefault, bundle: oldCA}},
			new:  []certificateWebhookCELEntry{{name: "managed.example", target: managedDefault}},
			want: false,
		},
		{
			name: "foreign service bundle is immutable",
			old:  []certificateWebhookCELEntry{{name: "foreign.example", target: foreignService, bundle: oldCA}},
			new:  []certificateWebhookCELEntry{{name: "foreign.example", target: foreignService, bundle: newCA}},
			want: false,
		},
		{
			name: "URL bundle is immutable",
			old:  []certificateWebhookCELEntry{{name: "foreign.example", target: foreignURL, bundle: oldCA}},
			new:  []certificateWebhookCELEntry{{name: "foreign.example", target: foreignURL, bundle: newCA}},
			want: false,
		},
		{
			name: "other service port bundle is immutable",
			old:  []certificateWebhookCELEntry{{name: "other-port.example", target: otherPort, bundle: oldCA}},
			new:  []certificateWebhookCELEntry{{name: "other-port.example", target: otherPort, bundle: newCA}},
			want: false,
		},
		{
			name: "managed rotation preserves every foreign bundle",
			old: []certificateWebhookCELEntry{
				{name: "managed.example", target: managedDefault, bundle: oldCA},
				{name: "foreign-service.example", target: foreignService, bundle: oldCA},
				{name: "foreign-url.example", target: foreignURL, bundle: oldCA},
				{name: "other-port.example", target: otherPort, bundle: oldCA},
			},
			new: []certificateWebhookCELEntry{
				{name: "managed.example", target: managedDefault, bundle: newCA},
				{name: "foreign-service.example", target: foreignService, bundle: oldCA},
				{name: "foreign-url.example", target: foreignURL, bundle: oldCA},
				{name: "other-port.example", target: otherPort, bundle: oldCA},
			},
			want: true,
		},
		{
			name: "managed rotation cannot hide foreign mutation",
			old: []certificateWebhookCELEntry{
				{name: "managed.example", target: managedDefault, bundle: oldCA},
				{name: "foreign.example", target: foreignService, bundle: oldCA},
			},
			new: []certificateWebhookCELEntry{
				{name: "managed.example", target: managedDefault, bundle: newCA},
				{name: "foreign.example", target: foreignService, bundle: newCA},
			},
			want: false,
		},
	}

	for _, includeReinvocation := range []bool{false, true} {
		includeReinvocation := includeReinvocation
		t.Run("reinvocation="+strconv.FormatBool(includeReinvocation), func(t *testing.T) {
			t.Parallel()
			environment, err := celgo.NewEnv(
				celgo.Variable("object", celgo.DynType),
				celgo.Variable("oldObject", celgo.DynType),
			)
			if err != nil {
				t.Fatal(err)
			}
			expression := certificateWebhookEntriesValidation("ptah-system", "ptah-webhook", includeReinvocation)
			ast, issues := environment.Compile(expression)
			if issues != nil && issues.Err() != nil {
				t.Fatalf("compile certificate write CEL: %v", issues.Err())
			}
			program, err := environment.Program(ast)
			if err != nil {
				t.Fatalf("build certificate write CEL program: %v", err)
			}
			for _, test := range tests {
				test := test
				t.Run(test.name, func(t *testing.T) {
					t.Parallel()
					result, _, err := program.Eval(map[string]any{
						"oldObject": map[string]any{"webhooks": certificateWebhookCELValues(test.old)},
						"object":    map[string]any{"webhooks": certificateWebhookCELValues(test.new)},
					})
					if err != nil {
						t.Fatalf("evaluate certificate write CEL: %v", err)
					}
					got, ok := result.Value().(bool)
					if !ok {
						t.Fatalf("certificate write CEL result = %T(%v), want bool", result.Value(), result.Value())
					}
					if got != test.want {
						t.Fatalf("certificate write CEL = %t, want %t", got, test.want)
					}
				})
			}
		})
	}
}

func certificateWebhookCELValues(entries []certificateWebhookCELEntry) []any {
	values := make([]any, 0, len(entries))
	for _, entry := range entries {
		clientConfig := make(map[string]any, 3)
		if entry.target.serviceName != "" {
			service := map[string]any{
				"namespace": entry.target.serviceNamespace,
				"name":      entry.target.serviceName,
			}
			if entry.target.port != nil {
				service["port"] = *entry.target.port
			}
			clientConfig["service"] = service
		}
		if entry.target.url != "" {
			clientConfig["url"] = entry.target.url
		}
		if entry.bundle != nil {
			clientConfig["caBundle"] = append([]byte(nil), entry.bundle...)
		}
		values = append(values, map[string]any{
			"name":         entry.name,
			"clientConfig": clientConfig,
		})
	}
	return values
}

func TestCertificateWriteGuardFieldCoverageMatchesKubernetesTypes(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		typeOf reflect.Type
		want   []string
	}{
		"object metadata": {
			typeOf: reflect.TypeOf(metav1.ObjectMeta{}),
			want: []string{
				"name",
				"generateName",
				"namespace",
				"selfLink",
				"uid",
				"resourceVersion",
				"generation",
				"creationTimestamp",
				"deletionTimestamp",
				"deletionGracePeriodSeconds",
				"labels",
				"annotations",
				"ownerReferences",
				"finalizers",
				"managedFields",
			},
		},
		"webhook client config": {
			typeOf: reflect.TypeOf(admissionregistrationv1.WebhookClientConfig{}),
			want:   []string{"url", "service", "caBundle"},
		},
		"mutating webhook": {
			typeOf: reflect.TypeOf(admissionregistrationv1.MutatingWebhook{}),
			want: []string{
				"name",
				"clientConfig",
				"rules",
				"failurePolicy",
				"matchPolicy",
				"namespaceSelector",
				"objectSelector",
				"sideEffects",
				"timeoutSeconds",
				"admissionReviewVersions",
				"reinvocationPolicy",
				"matchConditions",
			},
		},
		"validating webhook": {
			typeOf: reflect.TypeOf(admissionregistrationv1.ValidatingWebhook{}),
			want: []string{
				"name",
				"clientConfig",
				"rules",
				"failurePolicy",
				"matchPolicy",
				"namespaceSelector",
				"objectSelector",
				"sideEffects",
				"timeoutSeconds",
				"admissionReviewVersions",
				"matchConditions",
			},
		},
		"mutating webhook configuration": {
			typeOf: reflect.TypeOf(admissionregistrationv1.MutatingWebhookConfiguration{}),
			want:   []string{"kind", "apiVersion", "metadata", "webhooks"},
		},
		"validating webhook configuration": {
			typeOf: reflect.TypeOf(admissionregistrationv1.ValidatingWebhookConfiguration{}),
			want:   []string{"kind", "apiVersion", "metadata", "webhooks"},
		},
	} {
		test := test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := jsonFieldNames(test.typeOf); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Kubernetes %s JSON fields = %v, want exact guarded inventory %v; update the certificate write contract before accepting a new field", name, got, test.want)
			}
		})
	}

	metadataExpression := certificateMetadataValidation()
	for _, field := range []string{
		"name",
		"generateName",
		"namespace",
		"selfLink",
		"uid",
		"resourceVersion",
		"creationTimestamp",
		"deletionTimestamp",
		"deletionGracePeriodSeconds",
		"labels",
		"annotations",
		"ownerReferences",
		"finalizers",
	} {
		if !strings.Contains(metadataExpression, "object.metadata."+field) {
			t.Fatalf("certificate metadata contract does not freeze %q", field)
		}
	}
	if !strings.Contains(metadataExpression, "object.metadata.generation") ||
		strings.Contains(metadataExpression, "object.metadata.managedFields") {
		t.Fatalf("certificate metadata server-field exceptions changed: %q", metadataExpression)
	}
}

func TestCertificateWriteGuardsPrecedeCertificatePrivileges(t *testing.T) {
	t.Parallel()

	weights := []string{
		controllerWriteBindingWeight,
		certificateMutatingWritePolicyWeight,
		certificateMutatingWriteBindingWeight,
		certificateValidatingWritePolicyWeight,
		certificateValidatingWriteBindingWeight,
		releaseActivationHookWeight,
	}
	previous, err := strconv.Atoi(weights[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, weight := range weights[1:] {
		current, err := strconv.Atoi(weight)
		if err != nil {
			t.Fatal(err)
		}
		if current <= previous {
			t.Fatalf("certificate write hook order is not strictly increasing: %v", weights)
		}
		previous = current
	}
}

func TestCertificateWriteGuardVerifyRejectsContractTampering(t *testing.T) {
	t.Parallel()

	guard := testCertificateWriteGuard()
	policies := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicy)
	bindings := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding)
	for _, entry := range guard.entries() {
		policies[entry.name] = guard.policy(entry)
		bindings[entry.name] = guard.binding(entry)
	}
	guard.Policies = &rolloutPolicyClient{objects: policies}
	guard.Bindings = &rolloutBindingClient{objects: bindings}
	if err := guard.Verify(context.Background()); err != nil {
		t.Fatalf("verify exact certificate write guards: %v", err)
	}

	mutating := guard.entries()[0]
	policies[mutating.name].Spec.Validations[2].Expression = "true"
	if err := guard.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable contract") {
		t.Fatalf("tampered certificate write policy error = %v", err)
	}
	policies[mutating.name] = guard.policy(mutating)

	validating := guard.entries()[1]
	bindings[validating.name].Spec.MatchResources.ResourceRules[0].ResourceNames = []string{"other"}
	if err := guard.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable contract") {
		t.Fatalf("tampered certificate write binding error = %v", err)
	}
}

func TestCertificateWriteGuardWaitReady(t *testing.T) {
	t.Parallel()

	guard := testCertificateWriteGuard()
	policies := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicy)
	bindings := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding)
	for _, entry := range guard.entries() {
		policies[entry.name] = readyPolicy(guard.policy(entry))
		bindings[entry.name] = guard.binding(entry)
	}
	guard.Policies = &rolloutPolicyClient{objects: policies}
	guard.Bindings = &rolloutBindingClient{objects: bindings}
	if err := guard.WaitReady(context.Background()); err != nil {
		t.Fatalf("wait for ready certificate write guards: %v", err)
	}
}

func TestCertificateWriteGuardWaitReadyRejectsTypeWarnings(t *testing.T) {
	t.Parallel()

	guard := testCertificateWriteGuard()
	policies := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicy)
	bindings := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding)
	entries := guard.entries()
	for _, entry := range entries {
		policies[entry.name] = readyPolicy(guard.policy(entry))
		bindings[entry.name] = guard.binding(entry)
	}
	policies[entries[1].name].Status.TypeChecking.ExpressionWarnings = []admissionregistrationv1.ExpressionWarning{{
		FieldRef: "spec.validations[2].expression",
		Warning:  "mixed resource type",
	}}
	guard.Policies = &rolloutPolicyClient{objects: policies}
	guard.Bindings = &rolloutBindingClient{objects: bindings}
	err := guard.WaitReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CEL type-check warnings: mixed resource type") {
		t.Fatalf("type-check warning error = %v", err)
	}
}

func TestRenderedCertificateWriteGuardsMatchCompiledContracts(t *testing.T) {
	path := os.Getenv("PTAH_ROLLOUT_GUARD_RENDER")
	if path == "" {
		t.Skip("PTAH_ROLLOUT_GUARD_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	guard := testCertificateWriteGuard()
	guard.ReleaseName = "ptah-e2e"
	guard.ReleaseNamespace = "ptah-e2e"
	guard.WebhookServiceName = "ptah-e2e-ptah-operator-webhook"
	guard.CertificateServiceAccountName = "ptah-e2e-ptah-operator-cert-rotator"
	policies := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicy)
	bindings := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding)
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(rendered))
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
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
			policies[object.Name] = &object
		case "ValidatingAdmissionPolicyBinding":
			var object admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			bindings[object.Name] = &object
		}
	}
	for _, entry := range guard.entries() {
		policy := policies[entry.name]
		binding := bindings[entry.name]
		if err := guard.verifyPolicy(entry, policy); err != nil {
			t.Fatalf("rendered %s policy: %v", entry.component, err)
		}
		if err := guard.verifyBinding(entry, binding); err != nil {
			t.Fatalf("rendered %s binding: %v", entry.component, err)
		}
		if policy.Annotations["helm.sh/hook-weight"] != entry.policyWeight ||
			binding.Annotations["helm.sh/hook-weight"] != entry.bindingWeight {
			t.Fatalf("%s is not installed in its exact early hook order", entry.component)
		}
	}
}

func assertExactCertificateWriteMatch(t *testing.T, match *admissionregistrationv1.MatchResources, resource string) {
	t.Helper()
	if match == nil || match.MatchPolicy == nil || *match.MatchPolicy != admissionregistrationv1.Exact {
		t.Fatal("certificate write guard matching is not Exact")
	}
	if match.NamespaceSelector != nil || match.ObjectSelector != nil || len(match.ExcludeResourceRules) != 0 {
		t.Fatalf("certificate write guard must not rely on selectors or exclusions: %#v", match)
	}
	if len(match.ResourceRules) != 1 {
		t.Fatalf("certificate write guard rules = %d, want one", len(match.ResourceRules))
	}
	rule := match.ResourceRules[0]
	if !reflect.DeepEqual(rule.Operations, []admissionregistrationv1.OperationType{admissionregistrationv1.Update}) ||
		!reflect.DeepEqual(rule.APIGroups, []string{"admissionregistration.k8s.io"}) ||
		!reflect.DeepEqual(rule.APIVersions, []string{"v1"}) ||
		!reflect.DeepEqual(rule.Resources, []string{resource}) ||
		!reflect.DeepEqual(rule.ResourceNames, []string{AdmissionConfigurationName}) ||
		rule.Scope == nil || *rule.Scope != admissionregistrationv1.ClusterScope {
		t.Fatalf("certificate write rule is not exact: %#v", rule)
	}
}

func testCertificateWriteGuard() *CertificateWriteGuard {
	return &CertificateWriteGuard{
		Policies:                      &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}},
		Bindings:                      &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}},
		ReleaseName:                   "ptah",
		ReleaseNamespace:              "ptah-system",
		WebhookServiceName:            "ptah-webhook",
		CertificateServiceAccountName: "ptah-certificate",
		PollEvery:                     time.Millisecond,
	}
}

func jsonFieldNames(typeOf reflect.Type) []string {
	fields := make([]string, 0, typeOf.NumField())
	for index := 0; index < typeOf.NumField(); index++ {
		field := typeOf.Field(index)
		tag := field.Tag.Get("json")
		name, options, _ := strings.Cut(tag, ",")
		if name == "" && strings.Contains(","+options+",", ",inline,") {
			fields = append(fields, jsonFieldNames(field.Type)...)
			continue
		}
		if name != "" && name != "-" {
			fields = append(fields, name)
		}
	}
	return fields
}
