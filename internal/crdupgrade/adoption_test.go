package crdupgrade

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const supportedPredecessorPodIntentMatchExpressionFixture = `object.metadata.ownerReferences.exists(ref,
  ref.apiVersion == 'batch/v1' && ref.kind == 'Job' && ref.controller == true) ||
(request.operation == 'UPDATE' && oldObject != null &&
  oldObject.metadata.ownerReferences.exists(ref,
    ref.apiVersion == 'batch/v1' && ref.kind == 'Job' && ref.controller == true))`

func TestAdmissionAdopterStampsExactLegacySingleton(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
	mutating.object.Annotations["owner.example.test/preserved"] = "true"
	if err := adopter.Adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	for kind, annotations := range map[string]map[string]string{
		"mutating":   mutating.object.Annotations,
		"validating": validating.object.Annotations,
	} {
		for key, want := range adopter.Expected.annotations() {
			if got := annotations[key]; got != want {
				t.Fatalf("%s annotation %s = %q, want %q", kind, key, got, want)
			}
		}
	}
	if mutating.object.Annotations["owner.example.test/preserved"] != "true" {
		t.Fatal("adoption removed a foreign annotation")
	}
	if mutating.dryRunUpdates != 1 || validating.dryRunUpdates != 1 ||
		mutating.realUpdates != 1 || validating.realUpdates != 1 {
		t.Fatalf("updates mutating=%d/%d validating=%d/%d, want dry-run/real 1/1 each",
			mutating.dryRunUpdates, mutating.realUpdates, validating.dryRunUpdates, validating.realUpdates)
	}
}

func TestAdmissionAdopterPreflightDryRunsLegacyTupleWithoutPersisting(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
	mutatingBefore := mutating.object.DeepCopy()
	validatingBefore := validating.object.DeepCopy()
	if err := adopter.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mutating.dryRunUpdates != 1 || validating.dryRunUpdates != 1 ||
		mutating.realUpdates != 0 || validating.realUpdates != 0 {
		t.Fatalf("preflight updates mutating=%d/%d validating=%d/%d, want dry-run/real 1/0 each",
			mutating.dryRunUpdates, mutating.realUpdates, validating.dryRunUpdates, validating.realUpdates)
	}
	if !reflect.DeepEqual(mutating.object, mutatingBefore) || !reflect.DeepEqual(validating.object, validatingBefore) {
		t.Fatal("admission preflight persisted a legacy adoption")
	}
}

func TestAdmissionAdopterResumesPartialExactAdoption(t *testing.T) {
	tests := []struct {
		name                             string
		mutatingLegacy, validatingLegacy bool
		wantMutating, wantValidating     int
	}{
		{name: "mutating already stamped", validatingLegacy: true, wantValidating: 1},
		{name: "validating already stamped", mutatingLegacy: true, wantMutating: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adopter, mutating, validating := readyAdmissionAdopter(t, test.mutatingLegacy, test.validatingLegacy)
			if err := adopter.Adopt(context.Background()); err != nil {
				t.Fatal(err)
			}
			if mutating.dryRunUpdates != test.wantMutating || mutating.realUpdates != test.wantMutating ||
				validating.dryRunUpdates != test.wantValidating || validating.realUpdates != test.wantValidating {
				t.Fatalf("partial adoption updates mutating=%d/%d validating=%d/%d",
					mutating.dryRunUpdates, mutating.realUpdates, validating.dryRunUpdates, validating.realUpdates)
			}
		})
	}
}

func TestAdmissionAdopterRejectsDriftedStampedPeerDuringPartialResume(t *testing.T) {
	tests := []struct {
		name                             string
		mutatingLegacy, validatingLegacy bool
		mutate                           func(*mutatingAdmissionClient, *validatingAdmissionClient)
	}{
		{
			name: "mutating peer drift", validatingLegacy: true,
			mutate: func(mutating *mutatingAdmissionClient, _ *validatingAdmissionClient) {
				mutating.object.Webhooks[0].ClientConfig.Service.Name = "foreign-service"
			},
		},
		{
			name: "validating peer drift", mutatingLegacy: true,
			mutate: func(_ *mutatingAdmissionClient, validating *validatingAdmissionClient) {
				validating.object.Webhooks[0].ClientConfig.Service.Name = "foreign-service"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adopter, mutating, validating := readyAdmissionAdopter(t, test.mutatingLegacy, test.validatingLegacy)
			test.mutate(mutating, validating)
			err := adopter.Adopt(context.Background())
			if err == nil || !strings.Contains(err.Error(), "Service target does not match") {
				t.Fatalf("Adopt error = %v, want stamped peer contract refusal", err)
			}
			if mutating.dryRunUpdates != 0 || validating.dryRunUpdates != 0 ||
				mutating.realUpdates != 0 || validating.realUpdates != 0 {
				t.Fatal("partial adoption mutated a pair with stamped-peer contract drift")
			}
		})
	}
}

func TestAdmissionAdopterAllowsOwnedContractToChangeAfterPreflight(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, false, false)
	adopter.Expected.WebhookTimeoutSeconds = 7
	mutating.object.Webhooks[0].ClientConfig.Service.Name = "old-service"
	validating.object.Webhooks[0].TimeoutSeconds = valuePointer(int32(4))
	if err := adopter.Adopt(context.Background()); err != nil {
		t.Fatalf("owned previous contract was rejected before Helm could update it: %v", err)
	}
	if mutating.dryRunUpdates != 0 || mutating.realUpdates != 0 ||
		validating.dryRunUpdates != 0 || validating.realUpdates != 0 {
		t.Fatal("unchanged owned versions unexpectedly mutated admission objects")
	}
}

func TestAdmissionAdopterLeavesOlderOwnedVersionForAtomicHelmUpdate(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, false, false)
	adopter.Expected.ControllerStateVersion = 2
	adopter.Expected.AdmissionContractVersion = 2
	if err := adopter.Adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	for kind, annotations := range map[string]map[string]string{
		"mutating":   mutating.object.Annotations,
		"validating": validating.object.Annotations,
	} {
		if annotations[ControllerStateVersionAnnotation] != "1" ||
			annotations[AdmissionContractVersionAnnotation] != "1" {
			t.Fatalf("%s version ratchets changed before the Helm contract update: %v", kind, annotations)
		}
	}
	if mutating.dryRunUpdates != 0 || mutating.realUpdates != 0 ||
		validating.dryRunUpdates != 0 || validating.realUpdates != 0 {
		t.Fatal("owned version transition mutated annotations before Helm")
	}
}

func TestAdmissionAdopterRecognizesLegacyContractWithPreviousTimeout(t *testing.T) {
	adopter, _, _ := readyAdmissionAdopter(t, true, true)
	adopter.Expected.WebhookTimeoutSeconds = 7
	if err := adopter.Adopt(context.Background()); err != nil {
		t.Fatalf("exact legacy contract with an older timeout was rejected: %v", err)
	}
}

func TestAdmissionAdopterRecognizesAPIDefaultedSupportedPredecessorContract(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
	applyAdmissionSelectorDefaults(mutating.object, validating.object)
	if err := adopter.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight API-defaulted predecessor contract: %v", err)
	}
	if mutating.dryRunUpdates != 1 || validating.dryRunUpdates != 1 {
		t.Fatalf("dry-run updates mutating=%d validating=%d, want 1 each", mutating.dryRunUpdates, validating.dryRunUpdates)
	}
}

func TestAdmissionAdopterAcceptsSupportedPredecessorWebhookPermutation(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
	applyAdmissionSelectorDefaults(mutating.object, validating.object)
	validating.object.Webhooks[0], validating.object.Webhooks[1] =
		validating.object.Webhooks[1], validating.object.Webhooks[0]
	if err := adopter.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight permuted predecessor webhooks: %v", err)
	}
}

func TestAdmissionAdopterRejectsSupportedPredecessorApprovalContractDrift(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*admissionregistrationv1.MutatingWebhook, *admissionregistrationv1.ValidatingWebhook)
	}{
		{
			name: "mutating review version", want: "admissionReviewVersions",
			mutate: func(mutating *admissionregistrationv1.MutatingWebhook, _ *admissionregistrationv1.ValidatingWebhook) {
				mutating.AdmissionReviewVersions = []string{"v1beta1"}
			},
		},
		{
			name: "mutating reinvocation", want: "reinvocationPolicy",
			mutate: func(mutating *admissionregistrationv1.MutatingWebhook, _ *admissionregistrationv1.ValidatingWebhook) {
				ifNeeded := admissionregistrationv1.IfNeededReinvocationPolicy
				mutating.ReinvocationPolicy = &ifNeeded
			},
		},
		{
			name: "mutating rules", want: "rules do not match",
			mutate: func(mutating *admissionregistrationv1.MutatingWebhook, _ *admissionregistrationv1.ValidatingWebhook) {
				mutating.Rules[0].Operations = append(mutating.Rules[0].Operations, admissionregistrationv1.Update)
			},
		},
		{
			name: "validating failure policy", want: "failurePolicy must be Fail",
			mutate: func(_ *admissionregistrationv1.MutatingWebhook, validating *admissionregistrationv1.ValidatingWebhook) {
				ignore := admissionregistrationv1.Ignore
				validating.FailurePolicy = &ignore
			},
		},
		{
			name: "validating service path", want: "Service target does not match",
			mutate: func(_ *admissionregistrationv1.MutatingWebhook, validating *admissionregistrationv1.ValidatingWebhook) {
				path := "/foreign"
				validating.ClientConfig.Service.Path = &path
			},
		},
		{
			name: "validating match condition", want: "must not have matchConditions",
			mutate: func(_ *admissionregistrationv1.MutatingWebhook, validating *admissionregistrationv1.ValidatingWebhook) {
				validating.MatchConditions = []admissionregistrationv1.MatchCondition{{Name: "foreign", Expression: "true"}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
			applyAdmissionSelectorDefaults(mutating.object, validating.object)
			test.mutate(&mutating.object.Webhooks[0], &validating.object.Webhooks[0])
			err := adopter.Preflight(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Preflight error = %v, want %q", err, test.want)
			}
			if mutating.dryRunUpdates != 0 || validating.dryRunUpdates != 0 {
				t.Fatal("drifted predecessor approval contract was dry-run updated")
			}
		})
	}
}

func TestAdmissionAdopterRejectsSupportedPredecessorPodContractDrift(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*admissionregistrationv1.ValidatingWebhook)
	}{
		{
			name: "match-all object selector", want: "objectSelector",
			mutate: func(webhook *admissionregistrationv1.ValidatingWebhook) {
				webhook.ObjectSelector = &metav1.LabelSelector{}
			},
		},
		{
			name: "object selector label", want: "objectSelector",
			mutate: func(webhook *admissionregistrationv1.ValidatingWebhook) {
				webhook.ObjectSelector.MatchLabels["app.kubernetes.io/component"] = "foreign"
			},
		},
		{
			name: "match condition name", want: "matchConditions",
			mutate: func(webhook *admissionregistrationv1.ValidatingWebhook) {
				webhook.MatchConditions[0].Name = "foreign"
			},
		},
		{
			name: "match condition expression", want: "matchConditions",
			mutate: func(webhook *admissionregistrationv1.ValidatingWebhook) {
				webhook.MatchConditions[0].Expression = "true"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
			applyAdmissionSelectorDefaults(mutating.object, validating.object)
			test.mutate(&validating.object.Webhooks[1])
			err := adopter.Preflight(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Preflight error = %v, want %q", err, test.want)
			}
			if mutating.dryRunUpdates != 0 || validating.dryRunUpdates != 0 {
				t.Fatal("drifted predecessor contract was dry-run updated")
			}
		})
	}
}

func TestAdmissionAdopterAllowsBothObjectsAbsentOnInstall(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, false, false)
	mutating.object = nil
	validating.object = nil
	if err := adopter.Adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionAdopterRejectsIncompleteSingleton(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
	validating.object = nil
	err := adopter.Adopt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("Adopt error = %v, want incomplete singleton refusal", err)
	}
	if mutating.dryRunUpdates != 0 || mutating.realUpdates != 0 {
		t.Fatal("incomplete singleton was mutated")
	}
}

func TestAdmissionAdopterRejectsUnknownLegacyContractBeforeMutation(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
	mutating.object.Webhooks[0].ClientConfig.Service.Name = "foreign-service"
	err := adopter.Adopt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Service target does not match") {
		t.Fatalf("Adopt error = %v, want exact contract refusal", err)
	}
	if mutating.dryRunUpdates != 0 || validating.dryRunUpdates != 0 ||
		mutating.realUpdates != 0 || validating.realUpdates != 0 {
		t.Fatal("unknown legacy contract was mutated")
	}
}

func TestAdmissionAdopterRejectsForeignOrPartialOwnership(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AdmissionAdopter, *mutatingAdmissionClient)
		want   string
	}{
		{
			name: "foreign Helm release",
			mutate: func(_ *AdmissionAdopter, mutating *mutatingAdmissionClient) {
				mutating.object.Annotations[helmReleaseNameAnnotation] = "foreign"
			},
			want: "is not owned by Helm release",
		},
		{
			name: "partial owned tuple",
			mutate: func(adopter *AdmissionAdopter, mutating *mutatingAdmissionClient) {
				mutating.object.Annotations[ReleaseNameAnnotation] = adopter.Expected.ReleaseName
			},
			want: "incomplete owned annotation tuple",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
			test.mutate(adopter, mutating)
			err := adopter.Adopt(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Adopt error = %v, want %q", err, test.want)
			}
			if mutating.dryRunUpdates != 0 || validating.dryRunUpdates != 0 ||
				mutating.realUpdates != 0 || validating.realUpdates != 0 {
				t.Fatal("invalid singleton ownership was mutated")
			}
		})
	}
}

func TestAdmissionAdopterPreflightsBothObjectsBeforeMutation(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
	validating.dryRunError = errors.New("policy denied")
	err := adopter.Adopt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dry-run legacy ValidatingWebhookConfiguration adoption") {
		t.Fatalf("Adopt error = %v, want dry-run refusal", err)
	}
	if mutating.dryRunUpdates != 1 || validating.dryRunUpdates != 1 ||
		mutating.realUpdates != 0 || validating.realUpdates != 0 {
		t.Fatalf("failed preflight updates mutating=%d/%d validating=%d/%d",
			mutating.dryRunUpdates, mutating.realUpdates, validating.dryRunUpdates, validating.realUpdates)
	}
}

func TestAdmissionAdopterRechecksPairAfterFirstRealUpdate(t *testing.T) {
	adopter, mutating, validating := readyAdmissionAdopter(t, true, true)
	mutating.onGetCall = func(call int) {
		if call == 3 {
			mutating.object.Webhooks[0].ClientConfig.Service.Name = "foreign-service"
		}
	}
	err := adopter.Adopt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Service target does not match") {
		t.Fatalf("Adopt error = %v, want drift refusal at second retry boundary", err)
	}
	if mutating.dryRunUpdates != 1 || validating.dryRunUpdates != 1 ||
		mutating.realUpdates != 1 || validating.realUpdates != 0 {
		t.Fatalf("drifted retry updates mutating=%d/%d validating=%d/%d, want dry-run/real 1/1 and 1/0",
			mutating.dryRunUpdates, mutating.realUpdates, validating.dryRunUpdates, validating.realUpdates)
	}
}

func readyAdmissionAdopter(t *testing.T, mutatingLegacy, validatingLegacy bool) (*AdmissionAdopter, *mutatingAdmissionClient, *validatingAdmissionClient) {
	t.Helper()
	verifier := readyRuntimeVerifier(t)
	mutating := verifier.Mutating.(*mutatingAdmissionClient)
	validating := verifier.Validating.(*validatingAdmissionClient)
	setHelmOwnership(&mutating.object.ObjectMeta, verifier.Expected)
	setHelmOwnership(&validating.object.ObjectMeta, verifier.Expected)
	if mutatingLegacy || validatingLegacy {
		mutating.object.Webhooks = []admissionregistrationv1.MutatingWebhook{
			readySupportedPredecessorMutatingApprovalWebhook(verifier.Expected),
		}
		validating.object.Webhooks = []admissionregistrationv1.ValidatingWebhook{
			readySupportedPredecessorValidatingApprovalWebhook(verifier.Expected),
			readySupportedPredecessorPodIntentWebhook(verifier.Expected),
		}
	}
	if mutatingLegacy {
		removeOwnedAnnotations(&mutating.object.ObjectMeta, verifier.Expected)
	}
	if validatingLegacy {
		removeOwnedAnnotations(&validating.object.ObjectMeta, verifier.Expected)
	}
	return &AdmissionAdopter{Mutating: mutating, Validating: validating, Expected: verifier.Expected}, mutating, validating
}

func readySupportedPredecessorMutatingApprovalWebhook(expected RuntimeInvariants) admissionregistrationv1.MutatingWebhook {
	fail := admissionregistrationv1.Fail
	none := admissionregistrationv1.SideEffectClassNone
	equivalent := admissionregistrationv1.Equivalent
	never := admissionregistrationv1.NeverReinvocationPolicy
	scope := admissionregistrationv1.NamespacedScope
	return admissionregistrationv1.MutatingWebhook{
		Name: "mapproval.operator.ptah.dev", AdmissionReviewVersions: []string{"v1"},
		FailurePolicy: &fail, SideEffects: &none, MatchPolicy: &equivalent,
		ReinvocationPolicy: &never, TimeoutSeconds: valuePointer(expected.WebhookTimeoutSeconds),
		ClientConfig: readyWebhookClientConfig(expected, "/mutate-operator-ptah-dev-v1alpha1-ptahschemaapproval"),
		Rules: []admissionregistrationv1.RuleWithOperations{{
			Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
			Rule: admissionregistrationv1.Rule{
				APIGroups: []string{"operator.ptah.dev"}, APIVersions: []string{"v1alpha1"},
				Resources: []string{"ptahschemaapprovals"}, Scope: &scope,
			},
		}},
	}
}

func readySupportedPredecessorValidatingApprovalWebhook(expected RuntimeInvariants) admissionregistrationv1.ValidatingWebhook {
	fail := admissionregistrationv1.Fail
	none := admissionregistrationv1.SideEffectClassNone
	equivalent := admissionregistrationv1.Equivalent
	scope := admissionregistrationv1.NamespacedScope
	return admissionregistrationv1.ValidatingWebhook{
		Name: "vapproval.operator.ptah.dev", AdmissionReviewVersions: []string{"v1"},
		FailurePolicy: &fail, SideEffects: &none, MatchPolicy: &equivalent,
		TimeoutSeconds: valuePointer(expected.WebhookTimeoutSeconds),
		ClientConfig:   readyWebhookClientConfig(expected, "/validate-operator-ptah-dev-v1alpha1-ptahschemaapproval"),
		Rules: []admissionregistrationv1.RuleWithOperations{{
			Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
			Rule: admissionregistrationv1.Rule{
				APIGroups: []string{"operator.ptah.dev"}, APIVersions: []string{"v1alpha1"},
				Resources: []string{"ptahschemaapprovals"}, Scope: &scope,
			},
		}},
	}
}

func readySupportedPredecessorPodIntentWebhook(expected RuntimeInvariants) admissionregistrationv1.ValidatingWebhook {
	fail := admissionregistrationv1.Fail
	none := admissionregistrationv1.SideEffectClassNone
	equivalent := admissionregistrationv1.Equivalent
	scope := admissionregistrationv1.NamespacedScope
	return admissionregistrationv1.ValidatingWebhook{
		Name: "vpodintent.operator.ptah.dev", AdmissionReviewVersions: []string{"v1"},
		FailurePolicy: &fail, SideEffects: &none, MatchPolicy: &equivalent,
		TimeoutSeconds: valuePointer(expected.WebhookTimeoutSeconds),
		ClientConfig:   readyWebhookClientConfig(expected, "/validate-v1-pod-ptah-operation-intent"),
		Rules: []admissionregistrationv1.RuleWithOperations{{
			Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
			Rule: admissionregistrationv1.Rule{
				APIGroups: []string{""}, APIVersions: []string{"v1"},
				Resources: []string{"pods", "pods/ephemeralcontainers", "pods/resize"}, Scope: &scope,
			},
		}},
		ObjectSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			"app.kubernetes.io/managed-by": "ptah-operator",
			"app.kubernetes.io/component":  "schema-operation",
		}},
		MatchConditions: []admissionregistrationv1.MatchCondition{{
			Name: "job-owned-pod", Expression: supportedPredecessorPodIntentMatchExpressionFixture,
		}},
	}
}

func setHelmOwnership(metadata *metav1.ObjectMeta, expected RuntimeInvariants) {
	metadata.Annotations[helmReleaseNameAnnotation] = expected.ReleaseName
	metadata.Annotations[helmReleaseNamespaceAnnotation] = expected.ReleaseNamespace
	metadata.Labels = map[string]string{
		managedByLabel: "Helm",
		instanceLabel:  expected.ReleaseName,
	}
}

func removeOwnedAnnotations(metadata *metav1.ObjectMeta, expected RuntimeInvariants) {
	for key := range expected.annotations() {
		delete(metadata.Annotations, key)
	}
}
