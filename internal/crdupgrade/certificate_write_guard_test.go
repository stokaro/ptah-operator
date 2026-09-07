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
			policy := stripStableAdmissionConvergenceDependencyProbeForTest(
				t,
				guard.policy(entry),
				guard.ReleaseNamespace,
				guard.ReleaseName,
			)
			binding := stripAdmissionConvergenceProbeBindingForTest(t, guard.binding(entry))
			if policy.Spec.ParamKind != nil || binding.Spec.ParamRef != nil {
				t.Fatal("certificate write guard must not depend on admission parameters")
			}
			if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
				t.Fatal("certificate write guard is not fail-closed")
			}
			assertExactCertificateWriteMatch(t, policy.Spec.MatchConstraints, entry.resource)
			assertExactCertificateWriteMatch(t, binding.Spec.MatchResources, entry.resource)
			wantUsername := `request.userInfo.username == "system:serviceaccount:ptah-system:ptah-cert-rotator" && request.resource.group == "admissionregistration.k8s.io"`
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
			policy := stripStableAdmissionConvergenceDependencyProbeForTest(
				t,
				guard.policy(entry),
				guard.ReleaseNamespace,
				guard.ReleaseName,
			)
			validations := policy.Spec.Validations
			if len(validations) != 3 {
				t.Fatalf("%s validations = %d, want 3", entry.resource, len(validations))
			}
			if validations[0].Expression != certificateMetadataValidation() {
				t.Fatalf("%s metadata is not immutable: %q", entry.resource, validations[0].Expression)
			}
			for _, marker := range []string{"metadata.selfLink", "metadata.labels", "metadata.annotations", "metadata.ownerReferences", "metadata.finalizers", "dyn(object).webhooks != dyn(oldObject).webhooks", "generation + 1"} {
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
				"dyn(object).webhooks.all",
				"dyn(oldObject).webhooks.exists",
				"clientConfig.service",
				`clientConfig.service.namespace == "ptah-system"`,
				`clientConfig.service.name == "ptah-webhook"`,
				`clientConfig.service.name == "ptah-cert-transition"`,
				entry.canaryName,
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
	if !reflect.DeepEqual(certificateMutatingWebhookNames(), []string{
		mutatingApprovalWebhookName,
		mutatingCertificateCanaryWebhookName,
	}) {
		t.Fatalf("mutating webhook order is not the exact release inventory: %#v", certificateMutatingWebhookNames())
	}
	if !reflect.DeepEqual(certificateValidatingWebhookNames(), []string{
		validatingApprovalWebhookName,
		podIntentWebhookName,
		controllerWriteWebhookName,
		validatingCertificateCanaryWebhookName,
	}) {
		t.Fatalf("validating webhook order is not the exact release inventory: %#v", certificateValidatingWebhookNames())
	}
}

func TestCertificateWriteGuardConvergenceProbesHaveOnePolicyCause(t *testing.T) {
	t.Parallel()

	guard := testCertificateWriteGuard()
	entries := guard.entries()
	policies := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicy, len(entries))
	for _, entry := range entries {
		policies[entry.name] = guard.policy(entry)
	}
	markerName := AdmissionConvergenceMarkerName(guard.ReleaseNamespace, guard.ReleaseName, 1)
	object := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name": markerName, "namespace": guard.ReleaseNamespace,
		},
	}
	for _, entry := range entries {
		probe := newStableAdmissionConvergenceDependencyProbe(entry.name, strings.Repeat("a", 64))
		request := map[string]any{
			"operation": "UPDATE",
			"namespace": guard.ReleaseNamespace,
			"name":      markerName,
			"dryRun":    true,
			"resource":  map[string]any{"group": "", "version": "v1", "resource": "configmaps"},
			"options":   map[string]any{"fieldManager": probe.FieldManager},
			"userInfo":  map[string]any{"username": "system:serviceaccount:ptah-system:probe"},
		}
		matched := 0
		for name, policy := range policies {
			if !evaluatePolicyMatchConditions(t, policy, object, object, request, nil) {
				continue
			}
			results := evaluatePolicyValidations(t, policy, object, object, request, nil)
			if name != entry.name {
				// The union selector lets every stable guard see the probe; a
				// foreign guard escapes its native validations and must not
				// answer in place of the target.
				for index, allowed := range results {
					if !allowed {
						t.Fatalf("probe for %s was denied by certificate policy %s validation %d", entry.name, name, index)
					}
				}
				continue
			}
			matched++
			denied := 0
			for index, allowed := range results {
				if allowed {
					continue
				}
				denied++
				validation := policy.Spec.Validations[index]
				if validation.MessageExpression == "" {
					t.Fatalf("probe for %s was denied by a native validation %d", entry.name, index)
				}
				message := evaluateRolloutCEL(t, validation.MessageExpression, map[string]any{"request": request}, map[string]any{})
				if message != probe.Message {
					t.Fatalf("probe for %s denial = %v, want %q", entry.name, message, probe.Message)
				}
			}
			if denied != 1 {
				t.Fatalf("probe for %s denial count = %d, want one", entry.name, denied)
			}
		}
		if matched != 1 {
			t.Fatalf("probe for %s matched %d certificate policies, want one", entry.name, matched)
		}
	}
}

func TestCertificateWebhookEntriesValidationEvaluatesServiceAuthority(t *testing.T) {
	t.Parallel()

	port443 := int64(certificateWebhookServicePort)
	port8443 := int64(8443)
	managedDefault := certificateWebhookCELTarget{serviceNamespace: "ptah-system", serviceName: "ptah-webhook"}
	managed443 := certificateWebhookCELTarget{serviceNamespace: "ptah-system", serviceName: "ptah-webhook", port: &port443}
	candidateService := certificateWebhookCELTarget{serviceNamespace: "ptah-system", serviceName: "ptah-cert-transition"}
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
		{
			name: "exact candidate canary bundle may rotate",
			old:  []certificateWebhookCELEntry{{name: "canary.example", target: candidateService, bundle: oldCA}},
			new:  []certificateWebhookCELEntry{{name: "canary.example", target: candidateService, bundle: newCA}},
			want: true,
		},
		{
			name: "foreign candidate Service entry bundle is immutable",
			old:  []certificateWebhookCELEntry{{name: "foreign.example", target: candidateService, bundle: oldCA}},
			new:  []certificateWebhookCELEntry{{name: "foreign.example", target: candidateService, bundle: newCA}},
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
			expression := certificateWebhookEntriesValidation(
				"ptah-system",
				"ptah-webhook",
				"ptah-cert-transition",
				"canary.example",
				includeReinvocation,
			)
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

func TestCertificateWebhookEntriesValidationRefusesAFieldChangeBesideTheCABundle(t *testing.T) {
	t.Parallel()

	// The rotator may replace the CA bundle of a webhook that points at the
	// release's own Service. CEL binds && tighter than ||, so an alternative
	// written without parentheses inside the conjunction would put every field
	// comparison in its right branch and skip them whenever the bundle write
	// qualifies, letting one write carry any other change with it.
	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("oldObject", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	expression := certificateWebhookEntriesValidation(
		"ptah-system", "ptah-webhook", "ptah-cert-transition", "canary.example", true,
	)
	ast, issues := environment.Compile(expression)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile certificate write CEL: %v", issues.Err())
	}
	program, programErr := environment.Program(ast)
	if programErr != nil {
		t.Fatalf("build certificate write CEL program: %v", programErr)
	}
	webhook := func(bundle, reinvocation string) map[string]any {
		return map[string]any{
			"name": "managed.example",
			"clientConfig": map[string]any{
				"service":  map[string]any{"namespace": "ptah-system", "name": "ptah-webhook"},
				"caBundle": bundle,
			},
			"reinvocationPolicy": reinvocation,
		}
	}
	evaluate := func(newWebhook map[string]any) bool {
		t.Helper()
		result, _, evalErr := program.Eval(map[string]any{
			"object":    map[string]any{"webhooks": []any{newWebhook}},
			"oldObject": map[string]any{"webhooks": []any{webhook("old-ca", "Never")}},
		})
		if evalErr != nil {
			t.Fatalf("evaluate certificate write CEL: %v", evalErr)
		}
		admitted, ok := result.Value().(bool)
		if !ok {
			t.Fatalf("certificate write CEL result = %T(%v), want bool", result.Value(), result.Value())
		}
		return admitted
	}
	// The control: replacing only the CA bundle of the release's own webhook is
	// what the rotator exists to do, so the refusal below is the second change.
	if !evaluate(webhook("new-ca", "Never")) {
		t.Fatal("certificate write CEL refused a bounded CA-bundle rotation")
	}
	if evaluate(webhook("new-ca", "IfNeeded")) {
		t.Fatal("certificate write CEL admitted a reinvocationPolicy change beside the CA bundle")
	}
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
		releaseActivationHookWeight,
		controllerWriteBindingWeight,
		certificateMutatingWritePolicyWeight,
		certificateMutatingWriteBindingWeight,
		certificateValidatingWritePolicyWeight,
		certificateValidatingWriteBindingWeight,
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
	if match.NamespaceSelector == nil || len(match.NamespaceSelector.MatchLabels) != 0 ||
		len(match.NamespaceSelector.MatchExpressions) != 0 || match.ObjectSelector == nil ||
		len(match.ObjectSelector.MatchLabels) != 0 || len(match.ObjectSelector.MatchExpressions) != 0 ||
		len(match.ExcludeResourceRules) != 0 {
		t.Fatalf("certificate write guard must declare exact match-all selectors without exclusions: %#v", match)
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

func stripStableAdmissionConvergenceDependencyProbeForTest(
	t *testing.T,
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	releaseNamespace,
	releaseName string,
) *admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	if policy == nil || policy.Spec.MatchConstraints == nil {
		t.Fatal("stable dependency policy or match constraints are nil")
	}
	wantExpression := stableAdmissionConvergenceProbeRequestExpression(
		policy.Name,
		releaseNamespace,
		serviceAccountObjectGuardMarkerPattern(releaseNamespace, releaseName),
	)
	wantAnyExpression := stableAdmissionConvergenceAnyProbeRequestExpression(
		releaseNamespace,
		serviceAccountObjectGuardMarkerPattern(releaseNamespace, releaseName),
	)
	if len(policy.Spec.Variables) < 2 ||
		policy.Spec.Variables[0] != (admissionregistrationv1.Variable{Name: "isAnyAdmissionConvergenceProbe", Expression: wantAnyExpression}) ||
		policy.Spec.Variables[1] != (admissionregistrationv1.Variable{Name: "isAdmissionConvergenceProbe", Expression: wantExpression}) {
		t.Fatalf("stable dependency variables differ from the policy-specific selector: %#v", policy.Spec.Variables)
	}
	resourceRules := policy.Spec.MatchConstraints.ResourceRules
	if len(resourceRules) < 2 || !reflect.DeepEqual(resourceRules[len(resourceRules)-1], admissionConvergenceProbeResourceRule("")) {
		t.Fatalf("stable dependency marker rule differs from the exact wrapper: %#v", resourceRules)
	}
	if len(policy.Spec.Validations) < 2 {
		t.Fatal("stable dependency policy lacks proof validations")
	}
	proof := policy.Spec.Validations[len(policy.Spec.Validations)-2:]
	if proof[0].Expression != `!variables.isAnyAdmissionConvergenceProbe || request.dryRun == true` ||
		proof[0].Message != admissionConvergenceProbePersistenceMessage ||
		proof[1].Expression != `!variables.isAdmissionConvergenceProbe` ||
		proof[1].MessageExpression != `"Ptah admission convergence confirmed exact workload guard " + request.options.fieldManager` {
		t.Fatalf("stable dependency proof validations differ from the exact wrapper: %#v", proof)
	}

	native := policy.DeepCopy()
	native.Spec.MatchConstraints.ResourceRules = native.Spec.MatchConstraints.ResourceRules[:len(native.Spec.MatchConstraints.ResourceRules)-1]
	native.Spec.Variables = native.Spec.Variables[2:]
	native.Spec.Validations = native.Spec.Validations[:len(native.Spec.Validations)-2]
	matchPrefix := "(" + wantAnyExpression + ") || ("
	for index := range native.Spec.MatchConditions {
		expression := native.Spec.MatchConditions[index].Expression
		if !strings.HasPrefix(expression, matchPrefix) || !strings.HasSuffix(expression, ")") {
			t.Fatalf("stable dependency match condition %d differs from the exact wrapper", index)
		}
		native.Spec.MatchConditions[index].Expression = strings.TrimSuffix(strings.TrimPrefix(expression, matchPrefix), ")")
	}
	validationPrefix := "variables.isAnyAdmissionConvergenceProbe || ("
	for index := range native.Spec.Validations {
		expression := native.Spec.Validations[index].Expression
		if !strings.HasPrefix(expression, validationPrefix) || !strings.HasSuffix(expression, ")") {
			t.Fatalf("stable dependency validation %d differs from the exact wrapper", index)
		}
		native.Spec.Validations[index].Expression = strings.TrimSuffix(strings.TrimPrefix(expression, validationPrefix), ")")
	}
	return native
}

func stripAdmissionConvergenceProbeBindingForTest(
	t *testing.T,
	binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding,
) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	t.Helper()
	if binding == nil || binding.Spec.MatchResources == nil {
		t.Fatal("dependency binding or match resources are nil")
	}
	rules := binding.Spec.MatchResources.ResourceRules
	if len(rules) < 2 || !reflect.DeepEqual(rules[len(rules)-1], admissionConvergenceProbeResourceRule("")) {
		t.Fatalf("dependency binding marker rule differs from the exact wrapper: %#v", rules)
	}
	native := binding.DeepCopy()
	native.Spec.MatchResources.ResourceRules = native.Spec.MatchResources.ResourceRules[:len(native.Spec.MatchResources.ResourceRules)-1]
	return native
}

func testCertificateWriteGuard() *CertificateWriteGuard {
	return &CertificateWriteGuard{
		Policies:                      &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}},
		Bindings:                      &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}},
		ReleaseName:                   "ptah",
		ReleaseNamespace:              "ptah-system",
		WebhookServiceName:            "ptah-webhook",
		CertificateServiceAccountName: "ptah-cert-rotator",
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

// The runtime verifier in the certificate rotator probes every dependency
// policy under the certificate principal, which these guards match by name.
// A probe of a foreign family must pass through each of them untouched: the
// target policy alone answers it.
func TestCertificateWriteGuardsAdmitForeignConvergenceProbesUnderTheCertificatePrincipal(t *testing.T) {
	t.Parallel()

	guard := testCertificateWriteGuard()
	markerName := AdmissionConvergenceMarkerName(guard.ReleaseNamespace, guard.ReleaseName, 1)
	marker := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name": markerName, "namespace": guard.ReleaseNamespace,
			"managedFields": []any{map[string]any{"manager": "helm"}},
		},
	}
	probed := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name": markerName, "namespace": guard.ReleaseNamespace,
			"managedFields": []any{map[string]any{"manager": "helm"}, map[string]any{"manager": "probe"}},
		},
	}
	username := "system:serviceaccount:" + guard.ReleaseNamespace + ":" + guard.CertificateServiceAccountName
	fieldManagers := map[string]string{
		"dependency probe":              admissionConvergenceProbeFieldManagerPrefix + strings.Repeat("b", 64),
		"stable probe of another guard": stableAdmissionConvergenceProbeFieldManagerPrefix("another-policy") + strings.Repeat("c", 64),
		"service account object probe":  serviceAccountObjectProbeFieldManagerPrefix + strings.Repeat("d", 64),
	}
	for _, entry := range guard.entries() {
		policy := guard.policy(entry)
		for family, fieldManager := range fieldManagers {
			request := map[string]any{
				"operation": "UPDATE",
				"namespace": guard.ReleaseNamespace,
				"name":      markerName,
				"dryRun":    true,
				"resource":  map[string]any{"group": "", "version": "v1", "resource": "configmaps"},
				"options":   map[string]any{"fieldManager": fieldManager},
				"userInfo":  map[string]any{"username": username},
			}
			if !evaluatePolicyMatchConditions(t, policy, probed, marker, request, nil) {
				t.Fatalf("%s under the certificate principal escaped %s entirely", family, entry.name)
			}
			for index, allowed := range evaluatePolicyValidations(t, policy, probed, marker, request, nil) {
				if !allowed {
					t.Fatalf("%s under the certificate principal was denied by %s validation %d", family, entry.name, index)
				}
			}
		}
	}
}

// The admission canary proves convergence with dry-run updates of its own
// marker ConfigMap under the certificate principal, judged by the canary
// webhooks. The certificate write guards must leave that request to them: a
// ConfigMap under the certificate principal is matched only as a probe.
func TestCertificateWriteGuardsLeaveTheCanaryMarkerToTheCanaryWebhooks(t *testing.T) {
	t.Parallel()

	guard := testCertificateWriteGuard()
	marker := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "ptah-webhook-certificate-canary", "namespace": guard.ReleaseNamespace},
	}
	for _, fieldManager := range []string{"ptah-certificate-rotation-canary-mutate-v1", "ptah-certificate-rotation-canary-validate-v1"} {
		request := map[string]any{
			"operation": "UPDATE",
			"namespace": guard.ReleaseNamespace,
			"name":      "ptah-webhook-certificate-canary",
			"dryRun":    true,
			"resource":  map[string]any{"group": "", "version": "v1", "resource": "configmaps"},
			"options":   map[string]any{"fieldManager": fieldManager},
			"userInfo":  map[string]any{"username": "system:serviceaccount:" + guard.ReleaseNamespace + ":" + guard.CertificateServiceAccountName},
		}
		for _, entry := range guard.entries() {
			if evaluatePolicyMatchConditions(t, guard.policy(entry), marker, marker, request, nil) {
				t.Fatalf("%s matched the canary marker update under %s", entry.name, fieldManager)
			}
		}
	}
}
