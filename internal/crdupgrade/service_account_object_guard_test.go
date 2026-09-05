package crdupgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestServiceAccountObjectGuardIsStableAcrossReleaseAttempts(t *testing.T) {
	first := testServiceAccountObjectGuard()
	firstPolicy, firstBinding, err := first.ExpectedObjects()
	if err != nil {
		t.Fatal(err)
	}

	secondRollout := *first.rollout
	secondRollout.ReleaseSequence = 2
	secondRollout.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	secondRollout.WebhookSecretName = "different-webhook-secret"
	secondRollout.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(secondRollout.ReleaseNamespace, secondRollout.ReleaseName, secondRollout.ReleaseSequence, secondRollout.ManagerImage)[:12]
	secondRollout.ControllerServiceAccountName = "ptah-controller-v2-" + strings.Repeat("b", 12)
	second := NewServiceAccountObjectGuard(&secondRollout)
	secondPolicy, secondBinding, err := second.ExpectedObjects()
	if err != nil {
		t.Fatal(err)
	}

	if firstPolicy.Name != secondPolicy.Name || firstBinding.Name != secondBinding.Name {
		t.Fatal("service account object guard name changed across release attempts")
	}
	if !reflect.DeepEqual(firstPolicy.Spec, secondPolicy.Spec) || !reflect.DeepEqual(firstBinding.Spec, secondBinding.Spec) {
		t.Fatal("service account object guard contract changed across release attempts")
	}
	if got := ServiceAccountObjectGuardInventoryNames(first.rollout.ReleaseNamespace, first.rollout.ReleaseName); !reflect.DeepEqual(got, []string{firstPolicy.Name}) {
		t.Fatalf("service account object guard inventory = %#v", got)
	}
}

func TestServiceAccountObjectIdentityContractAndExternalEpochNames(t *testing.T) {
	guard := testServiceAccountObjectGuard()
	contract, err := ServiceAccountObjectIdentityContractForRollout(guard.rollout)
	if err != nil {
		t.Fatal(err)
	}
	wantData := map[string]string{
		"contract-version":                   "2",
		"controller-service-account-base":    "ptah-controller",
		"controller-service-account-managed": "true",
		"hook-service-account-base":          "ptah",
		"certificate-service-account-name":   guard.rollout.CertificateDeploymentName,
	}
	if got := contract.MarkerData(); !reflect.DeepEqual(got, wantData) {
		t.Fatalf("identity marker data = %#v, want %#v", got, wantData)
	}

	externalName, err := ExternalControllerServiceAccountName("external-controller", 7)
	if err != nil {
		t.Fatal(err)
	}
	if externalName != "external-controller-v7" {
		t.Fatalf("external controller ServiceAccount = %q", externalName)
	}
	if _, err := ExternalControllerServiceAccountName("external-controller", 0); err == nil {
		t.Fatal("zero external controller epoch was accepted")
	}

	externalFirst := *guard.rollout
	externalFirst.ControllerServiceAccountManaged = false
	externalFirst.ControllerServiceAccountName = "external-controller-v1"
	externalFirstPolicy, _, err := NewServiceAccountObjectGuard(&externalFirst).ExpectedObjects()
	if err != nil {
		t.Fatal(err)
	}
	externalSecond := externalFirst
	externalSecond.ReleaseSequence = 2
	externalSecond.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("c", 64)
	externalSecond.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(externalSecond.ReleaseNamespace, externalSecond.ReleaseName, externalSecond.ReleaseSequence, externalSecond.ManagerImage)[:12]
	externalSecond.ControllerServiceAccountName = "external-controller-v2"
	externalSecond.PreviousControllerServiceAccountName = "external-controller-v1"
	externalSecond.PreviousControllerServiceAccountManaged = false
	externalSecond.PreviousControllerReleaseSequence = 1
	externalSecondPolicy, _, err := NewServiceAccountObjectGuard(&externalSecond).ExpectedObjects()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(externalFirstPolicy.Spec, externalSecondPolicy.Spec) {
		t.Fatal("external controller identity contract changed across release epochs")
	}
}

func TestServiceAccountObjectIdentityContractRejectsIdentityDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RolloutGuard)
	}{
		{
			name: "static external name",
			mutate: func(rollout *RolloutGuard) {
				rollout.ControllerServiceAccountManaged = false
				rollout.ControllerServiceAccountName = "external-controller"
			},
		},
		{
			name: "wrong current sequence",
			mutate: func(rollout *RolloutGuard) {
				rollout.ControllerServiceAccountName = "ptah-controller-v2-" + strings.Repeat("a", 12)
			},
		},
		{
			name: "later predecessor base drift",
			mutate: func(rollout *RolloutGuard) {
				rollout.PreviousControllerServiceAccountName = "other-controller-v2-" + strings.Repeat("b", 12)
				rollout.PreviousControllerServiceAccountManaged = true
				rollout.PreviousControllerReleaseSequence = 2
			},
		},
		{
			name: "later predecessor mode drift",
			mutate: func(rollout *RolloutGuard) {
				rollout.PreviousControllerServiceAccountName = "ptah-controller-v2"
				rollout.PreviousControllerServiceAccountManaged = false
				rollout.PreviousControllerReleaseSequence = 2
			},
		},
		{
			name: "hook sequence drift",
			mutate: func(rollout *RolloutGuard) {
				rollout.HookServiceAccountName = "ptah-crd-v2-" + strings.Repeat("b", 12)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rollout := *testServiceAccountObjectGuard().rollout
			test.mutate(&rollout)
			if _, err := ServiceAccountObjectIdentityContractForRollout(&rollout); err == nil {
				t.Fatal("identity drift was accepted")
			}
		})
	}

	legacy := *testServiceAccountObjectGuard().rollout
	legacy.PreviousControllerServiceAccountName = "legacy-static-controller"
	legacy.PreviousControllerServiceAccountManaged = false
	legacy.PreviousControllerReleaseSequence = 0
	if _, err := ServiceAccountObjectIdentityContractForRollout(&legacy); err != nil {
		t.Fatalf("separately trusted epoch-zero predecessor was rejected: %v", err)
	}
}

func TestServiceAccountObjectGuardIsFailClosedAndDenyOnly(t *testing.T) {
	guard := testServiceAccountObjectGuard()
	policy, binding, err := guard.ExpectedObjects()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Spec.ParamKind != nil || binding.Spec.ParamRef != nil {
		t.Fatal("service account object guard must not depend on a mutable parameter")
	}
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatal("service account object guard is not fail-closed")
	}
	assertServiceAccountObjectMatchResources(t, policy.Spec.MatchConstraints)
	assertServiceAccountObjectMatchResources(t, binding.Spec.MatchResources)
	if binding.Spec.PolicyName != policy.Name || !reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) {
		t.Fatalf("service account object guard binding is not exact deny-only enforcement: %#v", binding.Spec)
	}
	if policy.Annotations["helm.sh/resource-policy"] != "keep" || binding.Annotations["helm.sh/resource-policy"] != "keep" {
		t.Fatal("stable service account object guard is not retained across upgrades")
	}
	if policy.Annotations["helm.sh/hook-weight"] != serviceAccountObjectPolicyWeight || binding.Annotations["helm.sh/hook-weight"] != serviceAccountObjectBindingWeight {
		t.Fatal("service account object guard hook ordering differs from its compiled contract")
	}

	contract, err := json.Marshal(policy.Spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`oldObject.metadata.name`,
		`request.name == \"\"`,
		`namespaceObject.metadata.deletionTimestamp`,
		`system:serviceaccount:kube-system:namespace-controller`,
		`system:kube-controller-manager`,
		`-crd-v[1-9][0-9]*-[0-9a-f]{12}`,
		`-cleanup-v[1-9][0-9]*-[0-9a-f]{12}`,
		`-quiesce-v[1-9][0-9]*-[0-9a-f]{12}`,
		`ptah-teardown-bootstrap-v1-`,
		`validatingadmissionpolicies`,
		`validatingadmissionpolicybindings`,
	} {
		if !strings.Contains(string(contract), required) {
			t.Fatalf("service account object contract does not contain %q", required)
		}
	}
}

func TestServiceAccountObjectGuardMatchesEveryProtectedIdentityFromOldObject(t *testing.T) {
	guard := testServiceAccountObjectGuard()
	policy, err := guard.ExpectedPolicy()
	if err != nil {
		t.Fatal(err)
	}
	patterns, err := guard.patterns()
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := TeardownServiceAccountName(guard.rollout.HookServiceAccountName, guard.rollout.ReleaseSequence)
	if err != nil {
		t.Fatal(err)
	}
	quiesce, err := TeardownQuiesceJobName(guard.rollout.HookServiceAccountName)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		guard.rollout.ControllerServiceAccountName,
		"ptah-controller-v9-" + strings.Repeat("f", 12),
		guard.rollout.CertificateDeploymentName,
		guard.rollout.HookServiceAccountName,
		cleanup,
		quiesce,
		patterns.bootstrap,
	} {
		t.Run(name, func(t *testing.T) {
			oldObject := serviceAccountObjectForDelete(guard, name)
			request := serviceAccountObjectRequest(guard, "DELETE", "", "namespace-writer", nil)
			if !evaluatePolicyMatchConditions(t, policy, map[string]any{}, oldObject, request, nil) {
				t.Fatal("protected ServiceAccount delete-collection item did not match from oldObject")
			}
		})
	}
}

func TestServiceAccountObjectGuardCreateAndUpdateAuthorizationAndShape(t *testing.T) {
	guard := testServiceAccountObjectGuard()
	policy, err := guard.ExpectedPolicy()
	if err != nil {
		t.Fatal(err)
	}
	name := guard.rollout.ControllerServiceAccountName
	createObject := safeManagedServiceAccountObject(guard, name, false)
	createRequest := serviceAccountObjectRequest(guard, "CREATE", name, "helm-installer", nil)

	tests := []struct {
		name       string
		object     map[string]any
		oldObject  map[string]any
		request    map[string]any
		authorized bool
		want       bool
	}{
		{name: "authorized create", object: createObject, oldObject: map[string]any{}, request: createRequest, authorized: true, want: true},
		{name: "namespace writer create", object: createObject, oldObject: map[string]any{}, request: createRequest, want: false},
		{
			name:       "unsafe automount",
			object:     mutateServiceAccountObject(t, createObject, func(object map[string]any) { object["automountServiceAccountToken"] = false }),
			oldObject:  map[string]any{},
			request:    createRequest,
			authorized: true,
			want:       false,
		},
		{
			name: "unsafe owner",
			object: mutateServiceAccountObject(t, createObject, func(object map[string]any) {
				object["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"uid": "attacker"}}
			}),
			oldObject:  map[string]any{},
			request:    createRequest,
			authorized: true,
			want:       false,
		},
		{
			name: "unsafe secret reference",
			object: mutateServiceAccountObject(t, createObject, func(object map[string]any) {
				object["secrets"] = []any{map[string]any{"name": "token", "uid": "forged-uid"}}
			}),
			oldObject:  map[string]any{},
			request:    createRequest,
			authorized: true,
			want:       false,
		},
		{
			name: "safe generic secret reference",
			object: mutateServiceAccountObject(t, createObject, func(object map[string]any) {
				object["secrets"] = []any{map[string]any{"name": "token", "namespace": guard.rollout.ReleaseNamespace, "apiVersion": "v1", "kind": "Secret"}}
			}),
			oldObject:  map[string]any{},
			request:    createRequest,
			authorized: true,
			want:       true,
		},
	}

	oldObject := safeManagedServiceAccountObject(guard, name, true)
	updateObject := mutateServiceAccountObject(t, oldObject, func(object map[string]any) {
		object["metadata"].(map[string]any)["labels"].(map[string]any)["release-test"] = "updated"
	})
	updateRequest := serviceAccountObjectRequest(guard, "UPDATE", name, "helm-installer", nil)
	tests = append(tests,
		struct {
			name       string
			object     map[string]any
			oldObject  map[string]any
			request    map[string]any
			authorized bool
			want       bool
		}{name: "authorized update", object: updateObject, oldObject: oldObject, request: updateRequest, authorized: true, want: true},
		struct {
			name       string
			object     map[string]any
			oldObject  map[string]any
			request    map[string]any
			authorized bool
			want       bool
		}{name: "UID replacement", object: mutateServiceAccountObject(t, updateObject, func(object map[string]any) { object["metadata"].(map[string]any)["uid"] = "other" }), oldObject: oldObject, request: updateRequest, authorized: true, want: false},
	)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepared := serviceAccountObjectPolicyForCEL(t, policy, guard, test.authorized)
			if !evaluatePolicyMatchConditions(t, prepared, test.object, test.oldObject, test.request, map[string]any{}) {
				t.Fatal("protected ServiceAccount request did not match")
			}
			got := allAdmissionValidationsAllow(t, prepared, test.object, test.oldObject, test.request, map[string]any{})
			if got != test.want {
				t.Fatalf("admission decision = %t, want %t", got, test.want)
			}
		})
	}
}

func TestServiceAccountObjectGuardKeepsExternalMetadataFlexibleWithoutAllowingDeletionTriggers(t *testing.T) {
	rollout := *testServiceAccountObjectGuard().rollout
	rollout.ControllerServiceAccountManaged = false
	rollout.ControllerServiceAccountName = "external-controller-v1"
	guard := NewServiceAccountObjectGuard(&rollout)
	policy, err := guard.ExpectedPolicy()
	if err != nil {
		t.Fatal(err)
	}
	object := map[string]any{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata": map[string]any{
			"name":        rollout.ControllerServiceAccountName,
			"namespace":   rollout.ReleaseNamespace,
			"labels":      map[string]any{"user.example/purpose": "controller"},
			"annotations": map[string]any{"user.example/owner": "platform"},
			"finalizers":  []any{"user.example/cleanup"},
		},
	}
	request := serviceAccountObjectRequest(guard, "CREATE", rollout.ControllerServiceAccountName, "helm-installer", nil)

	tests := []struct {
		name       string
		object     map[string]any
		authorized bool
		want       bool
	}{
		{name: "authorized omitted automount and user finalizer", object: object, authorized: true, want: true},
		{name: "namespace writer", object: object},
		{
			name: "owner reference deletion trigger",
			object: mutateServiceAccountObject(t, object, func(candidate map[string]any) {
				candidate["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"uid": "foreign"}}
			}),
			authorized: true,
		},
		{
			name: "reserved hook annotation",
			object: mutateServiceAccountObject(t, object, func(candidate map[string]any) {
				candidate["metadata"].(map[string]any)["annotations"].(map[string]any)["helm.sh/hook"] = "pre-install"
			}),
			authorized: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepared := serviceAccountObjectPolicyForCEL(t, policy, guard, test.authorized)
			if !evaluatePolicyMatchConditions(t, prepared, test.object, map[string]any{}, request, map[string]any{}) {
				t.Fatal("external controller ServiceAccount create did not match")
			}
			if got := allAdmissionValidationsAllow(t, prepared, test.object, map[string]any{}, request, map[string]any{}); got != test.want {
				t.Fatalf("admission decision = %t, want %t", got, test.want)
			}
		})
	}
}

func TestServiceAccountObjectGuardNamedAndCollectionDelete(t *testing.T) {
	guard := testServiceAccountObjectGuard()
	policy, err := guard.ExpectedPolicy()
	if err != nil {
		t.Fatal(err)
	}
	name := guard.rollout.HookServiceAccountName
	oldObject := serviceAccountObjectForDelete(guard, name)
	namespace := map[string]any{"metadata": map[string]any{"name": guard.rollout.ReleaseNamespace}}
	terminatingNamespace := map[string]any{"metadata": map[string]any{"name": guard.rollout.ReleaseNamespace, "deletionTimestamp": "2026-09-05T00:00:00Z"}}

	tests := []struct {
		name        string
		requestName string
		username    string
		groups      []any
		authorized  bool
		namespace   map[string]any
		mutateOld   func(map[string]any)
		want        bool
	}{
		{name: "authorized named delete", requestName: name, username: "helm-installer", authorized: true, namespace: namespace, want: true},
		{name: "unauthorized named delete", requestName: name, username: "namespace-writer", namespace: namespace},
		{name: "authorized collection delete", requestName: "", username: "helm-installer", authorized: true, namespace: namespace, want: true},
		{name: "unauthorized collection delete", requestName: "", username: "namespace-writer", namespace: namespace},
		{
			name:        "namespace controller cleanup",
			requestName: "",
			username:    "system:serviceaccount:kube-system:namespace-controller",
			groups:      []any{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"},
			namespace:   terminatingNamespace,
			want:        true,
		},
		{
			name:        "namespace controller before termination",
			requestName: "",
			username:    "system:serviceaccount:kube-system:namespace-controller",
			groups:      []any{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"},
			namespace:   namespace,
		},
		{
			name:        "namespace controller with extra group",
			requestName: "",
			username:    "system:serviceaccount:kube-system:namespace-controller",
			groups:      []any{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated", "system:masters"},
			namespace:   terminatingNamespace,
		},
		{
			name:        "legacy controller cleanup",
			requestName: "",
			username:    "system:kube-controller-manager",
			groups:      []any{"system:authenticated"},
			namespace:   terminatingNamespace,
			want:        true,
		},
		{
			name:        "missing old UID",
			requestName: name,
			username:    "helm-installer",
			authorized:  true,
			namespace:   namespace,
			mutateOld: func(object map[string]any) {
				delete(object["metadata"].(map[string]any), "uid")
			},
		},
		{name: "named delete mismatch", requestName: "different", username: "helm-installer", authorized: true, namespace: namespace},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidateOld := mutateServiceAccountObject(t, oldObject, test.mutateOld)
			request := serviceAccountObjectRequest(guard, "DELETE", test.requestName, test.username, test.groups)
			prepared := serviceAccountObjectPolicyForCEL(t, policy, guard, test.authorized)
			if !evaluatePolicyMatchConditions(t, prepared, map[string]any{}, candidateOld, request, test.namespace) {
				t.Fatal("protected ServiceAccount delete did not match from oldObject")
			}
			got := allAdmissionValidationsAllow(t, prepared, map[string]any{}, candidateOld, request, test.namespace)
			if got != test.want {
				t.Fatalf("admission decision = %t, want %t", got, test.want)
			}
		})
	}
}

func TestServiceAccountObjectGuardCollectionDeleteUsesPerItemOldObject(t *testing.T) {
	guard := testServiceAccountObjectGuard()
	policy, err := guard.ExpectedPolicy()
	if err != nil {
		t.Fatal(err)
	}
	request := serviceAccountObjectRequest(guard, "DELETE", "", "namespace-writer", nil)
	protected := serviceAccountObjectForDelete(guard, guard.rollout.HookServiceAccountName)
	prepared := serviceAccountObjectPolicyForCEL(t, policy, guard, false)
	if !evaluatePolicyMatchConditions(t, prepared, map[string]any{}, protected, request, map[string]any{}) {
		t.Fatal("protected collection-delete item did not match from oldObject")
	}
	if allAdmissionValidationsAllow(t, prepared, map[string]any{}, protected, request, map[string]any{}) {
		t.Fatal("namespace writer was allowed to delete a protected collection member")
	}

	unrelated := serviceAccountObjectForDelete(guard, "unrelated-service-account")
	if evaluatePolicyMatchConditions(t, prepared, map[string]any{}, unrelated, request, map[string]any{}) {
		t.Fatal("unrelated collection-delete item was unnecessarily matched")
	}

	malformedOldObject := map[string]any{}
	authorizedRequest := serviceAccountObjectRequest(guard, "DELETE", "", "helm-installer", nil)
	authorized := serviceAccountObjectPolicyForCEL(t, policy, guard, true)
	if evaluatePolicyMatchConditions(t, authorized, map[string]any{}, malformedOldObject, authorizedRequest, map[string]any{}) {
		t.Fatal("name-less delete without an old object unexpectedly matched a protected identity")
	}
	if allAdmissionValidationsAllow(t, authorized, map[string]any{}, malformedOldObject, authorizedRequest, map[string]any{}) {
		t.Fatal("evaluated name-less delete without an old object did not fail closed")
	}

	terminatingNamespace := map[string]any{"metadata": map[string]any{
		"name":              guard.rollout.ReleaseNamespace,
		"deletionTimestamp": "2026-09-05T00:00:00Z",
	}}
	cleanupRequest := serviceAccountObjectRequest(
		guard,
		"DELETE",
		"",
		"system:serviceaccount:kube-system:namespace-controller",
		[]any{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"},
	)
	if allAdmissionValidationsAllow(t, authorized, map[string]any{}, malformedOldObject, cleanupRequest, terminatingNamespace) {
		t.Fatal("namespace cleanup without an authoritative old object did not fail closed")
	}
}

func TestServiceAccountObjectGuardAllowsExactBootstrapShapeOnlyForInstaller(t *testing.T) {
	guard := testServiceAccountObjectGuard()
	policy, err := guard.ExpectedPolicy()
	if err != nil {
		t.Fatal(err)
	}
	patterns, err := guard.patterns()
	if err != nil {
		t.Fatal(err)
	}
	object := safeBootstrapServiceAccountObject(guard, patterns.bootstrap)
	request := serviceAccountObjectRequest(guard, "CREATE", patterns.bootstrap, "helm-installer", nil)

	tests := []struct {
		name       string
		object     map[string]any
		authorized bool
		want       bool
	}{
		{name: "authorized exact bootstrap", object: object, authorized: true, want: true},
		{name: "unauthorized exact bootstrap", object: object},
		{
			name: "unsafe bootstrap automount",
			object: mutateServiceAccountObject(t, object, func(candidate map[string]any) {
				candidate["automountServiceAccountToken"] = true
			}),
			authorized: true,
		},
		{
			name: "wrong bootstrap hook weight",
			object: mutateServiceAccountObject(t, object, func(candidate map[string]any) {
				candidate["metadata"].(map[string]any)["annotations"].(map[string]any)["helm.sh/hook-weight"] = "-326"
			}),
			authorized: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepared := serviceAccountObjectPolicyForCEL(t, policy, guard, test.authorized)
			if !evaluatePolicyMatchConditions(t, prepared, test.object, map[string]any{}, request, map[string]any{}) {
				t.Fatal("bootstrap ServiceAccount create did not match")
			}
			if got := allAdmissionValidationsAllow(t, prepared, test.object, map[string]any{}, request, map[string]any{}); got != test.want {
				t.Fatalf("admission decision = %t, want %t", got, test.want)
			}
		})
	}
}

func TestServiceAccountObjectGuardVerifyRejectsTamperingAndWaitsReady(t *testing.T) {
	guard := testServiceAccountObjectGuard()
	policy, binding, err := guard.ExpectedObjects()
	if err != nil {
		t.Fatal(err)
	}
	name := policy.Name
	livePolicy := persistedServiceAccountObjectPolicy(readyPolicy(policy))
	liveBinding := persistedServiceAccountObjectBinding(binding)
	guard.rollout.Policies = &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{name: livePolicy}}
	guard.rollout.Bindings = &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{name: liveBinding}}
	if err := guard.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}

	mutated := livePolicy.DeepCopy()
	mutated.Spec.Validations[0].Expression = "true"
	if err := guard.verifyPolicy(mutated, policy); err == nil || !strings.Contains(err.Error(), "immutable contract") {
		t.Fatalf("tampered policy error = %v", err)
	}
	mutatedBinding := liveBinding.DeepCopy()
	mutatedBinding.Spec.ValidationActions = []admissionregistrationv1.ValidationAction{admissionregistrationv1.Warn}
	if err := guard.verifyBinding(mutatedBinding, binding); err == nil || !strings.Contains(err.Error(), "immutable contract") {
		t.Fatalf("tampered binding error = %v", err)
	}
}

func TestServiceAccountObjectGuardProbeRequiresExactCurrentMarkerAndDenial(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*testing.T, *admissionConvergenceFixture, *ServiceAccountObjectGuard)
		updateErr error
		want      bool
		wantErr   string
		wantCalls int
	}{
		{
			name:      "exact unsealed marker denial",
			updateErr: exactServiceAccountObjectProbeDenial(testServiceAccountObjectGuard()),
			want:      true,
			wantCalls: 1,
		},
		{
			name: "exact sealed marker denial",
			mutate: func(t *testing.T, fixture *admissionConvergenceFixture, _ *ServiceAccountObjectGuard) {
				sealAdmissionConvergenceMarkerForTest(t, fixture.guard, fixture.configMaps.objects[fixture.markerName])
			},
			updateErr: exactServiceAccountObjectProbeDenial(testServiceAccountObjectGuard()),
			want:      true,
			wantCalls: 1,
		},
		{
			name:      "admitted update is inconclusive",
			wantCalls: 1,
		},
		{
			name: "malformed current marker fails closed",
			mutate: func(_ *testing.T, fixture *admissionConvergenceFixture, _ *ServiceAccountObjectGuard) {
				fixture.configMaps.objects[fixture.markerName].Data[admissionConvergenceAttemptDataKey] = "foreign"
			},
			wantErr: "neither exact unsealed nor sealed state",
		},
		{
			name: "wrong release sequence cannot select another marker",
			mutate: func(_ *testing.T, _ *admissionConvergenceFixture, guard *ServiceAccountObjectGuard) {
				rollout := *guard.rollout
				rollout.ReleaseSequence++
				rollout.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)[:12]
				rollout.ControllerServiceAccountName = "ptah-controller-v2"
				rollout.PreviousControllerServiceAccountName = guard.rollout.ControllerServiceAccountName
				rollout.PreviousControllerServiceAccountManaged = false
				rollout.PreviousControllerReleaseSequence = guard.rollout.ReleaseSequence
				guard.rollout = &rollout
			},
			wantErr: "not found",
		},
		{
			name:      "foreign denial fails closed",
			updateErr: exactPolicyDenialError("foreign-policy", "foreign-binding", "foreign denial"),
			wantErr:   "unexpected response",
			wantCalls: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAdmissionConvergenceFixture(t)
			guard := NewServiceAccountObjectGuard(fixture.guard.dependencyRollout)
			if test.mutate != nil {
				test.mutate(t, fixture, guard)
			}
			if test.updateErr != nil {
				fixture.configMaps.updateErrors = []error{test.updateErr}
			}
			got, err := guard.Probe(context.Background(), fixture.configMaps)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Probe() error = %v, want containing %q", err, test.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("Probe() = %t, want %t", got, test.want)
			}
			if len(fixture.configMaps.updates) != test.wantCalls {
				t.Fatalf("dry-run updates = %d, want %d", len(fixture.configMaps.updates), test.wantCalls)
			}
			for index, options := range fixture.configMaps.updateOptions {
				probe := serviceAccountObjectGuardProbe(guard.rollout.ReleaseNamespace, guard.rollout.ReleaseName)
				if !reflect.DeepEqual(options.DryRun, []string{metav1.DryRunAll}) || options.FieldManager != probe.FieldManager {
					t.Fatalf("update %d options = %#v, want exact service account object probe", index, options)
				}
			}
		})
	}
}

func exactServiceAccountObjectProbeDenial(guard *ServiceAccountObjectGuard) error {
	probe := serviceAccountObjectGuardProbe(guard.rollout.ReleaseNamespace, guard.rollout.ReleaseName)
	return exactPolicyDenialError(probe.PolicyName, probe.PolicyName, probe.Message)
}

func TestRenderedServiceAccountObjectGuardMatchesCompiledContract(t *testing.T) {
	path := os.Getenv("PTAH_ROLLOUT_GUARD_RENDER")
	if path == "" {
		t.Skip("PTAH_ROLLOUT_GUARD_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rollout := runtimePodGuardFixture()
	rollout.ReleaseName = "ptah-e2e"
	rollout.ReleaseNamespace = "ptah-e2e"
	rollout.CoordinationNamespace = "ptah-e2e"
	rollout.ManagerImage = renderedGuardManagerImage
	rollout.ControllerDeploymentName = "ptah-e2e-ptah-operator"
	rollout.CertificateDeploymentName = "ptah-e2e-ptah-operator-cert-rotator"
	rollout.ControllerServiceAccountName = renderedDeploymentServiceAccount(t, rendered, rollout.ControllerDeploymentName)
	rollout.ControllerServiceAccountManaged = true
	rollout.HookServiceAccountName = "ptah-e2e-ptah-operator-crd-v1-" + hookIdentityDigest(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)[:12]
	rollout.WebhookSecretName = "ptah-e2e-ptah-operator-webhook-cert"
	guard := NewServiceAccountObjectGuard(rollout)
	expectedPolicy, expectedBinding, err := guard.ExpectedObjects()
	if err != nil {
		t.Fatal(err)
	}

	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
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
			if object.Name == expectedPolicy.Name {
				policy = &object
			}
		case "ValidatingAdmissionPolicyBinding":
			var object admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if object.Name == expectedBinding.Name {
				binding = &object
			}
		}
	}
	if policy != nil {
		policy = persistedServiceAccountObjectPolicy(policy)
	}
	if binding != nil {
		binding = persistedServiceAccountObjectBinding(binding)
	}
	if err := guard.verifyPolicy(policy, expectedPolicy); err != nil {
		t.Fatalf("rendered service account object policy: %v", err)
	}
	if err := guard.verifyBinding(binding, expectedBinding); err != nil {
		t.Fatalf("rendered service account object binding: %v", err)
	}
}

func testServiceAccountObjectGuard() *ServiceAccountObjectGuard {
	rollout := runtimePodGuardFixture()
	rollout.ControllerServiceAccountManaged = true
	rollout.ControllerServiceAccountName = "ptah-controller-v1-" + strings.Repeat("a", 12)
	return NewServiceAccountObjectGuard(rollout)
}

func assertServiceAccountObjectMatchResources(t *testing.T, match *admissionregistrationv1.MatchResources) {
	t.Helper()
	if match == nil || match.MatchPolicy == nil || *match.MatchPolicy != admissionregistrationv1.Exact || match.NamespaceSelector == nil || match.ObjectSelector == nil || len(match.ExcludeResourceRules) != 0 || len(match.ResourceRules) != 2 {
		t.Fatalf("service account object match resources are not explicit and exact: %#v", match)
	}
	rule := match.ResourceRules[0]
	if !reflect.DeepEqual(rule.Operations, []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update, admissionregistrationv1.Delete}) ||
		!reflect.DeepEqual(rule.APIGroups, []string{""}) || !reflect.DeepEqual(rule.APIVersions, []string{"v1"}) ||
		!reflect.DeepEqual(rule.Resources, []string{"serviceaccounts"}) || rule.Scope == nil || *rule.Scope != admissionregistrationv1.NamespacedScope {
		t.Fatalf("service account object resource rule is not exact: %#v", rule)
	}
	if got, want := match.ResourceRules[1], admissionConvergenceProbeResourceRule(); !reflect.DeepEqual(got, want) {
		t.Fatalf("service account object convergence resource rule = %#v, want %#v", got, want)
	}
}

func safeManagedServiceAccountObject(guard *ServiceAccountObjectGuard, name string, persisted bool) map[string]any {
	metadata := map[string]any{
		"name":      name,
		"namespace": guard.rollout.ReleaseNamespace,
		"labels": map[string]any{
			"app.kubernetes.io/managed-by": "Helm",
			"app.kubernetes.io/instance":   guard.rollout.ReleaseName,
			"app.kubernetes.io/name":       "ptah-operator",
			"app.kubernetes.io/version":    "test",
			"helm.sh/chart":                "ptah-operator-test",
		},
		"annotations": map[string]any{
			"meta.helm.sh/release-name":      guard.rollout.ReleaseName,
			"meta.helm.sh/release-namespace": guard.rollout.ReleaseNamespace,
		},
	}
	if persisted {
		metadata["uid"] = "controller-uid"
		metadata["resourceVersion"] = "42"
		metadata["creationTimestamp"] = "2026-09-05T00:00:00Z"
	}
	return map[string]any{
		"apiVersion":                   "v1",
		"kind":                         "ServiceAccount",
		"metadata":                     metadata,
		"automountServiceAccountToken": true,
	}
}

func safeBootstrapServiceAccountObject(guard *ServiceAccountObjectGuard, name string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata": map[string]any{
			"name":      name,
			"namespace": guard.rollout.ReleaseNamespace,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "Helm",
				"app.kubernetes.io/instance":   guard.rollout.ReleaseName,
				"app.kubernetes.io/name":       "ptah-operator",
				"app.kubernetes.io/version":    "test",
				"helm.sh/chart":                "ptah-operator-test",
				"app.kubernetes.io/component":  "teardown-retirement-bootstrap",
			},
			"annotations": map[string]any{
				"helm.sh/hook":               "pre-delete",
				"helm.sh/hook-weight":        "-327",
				"helm.sh/hook-delete-policy": "before-hook-creation,hook-succeeded",
			},
		},
		"automountServiceAccountToken": false,
	}
}

func persistedServiceAccountObjectPolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	result := policy.DeepCopy()
	result.UID = "service-account-object-policy-uid"
	result.ResourceVersion = "42"
	return result
}

func persistedServiceAccountObjectBinding(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	result := binding.DeepCopy()
	result.UID = "service-account-object-binding-uid"
	result.ResourceVersion = "42"
	return result
}

func serviceAccountObjectForDelete(guard *ServiceAccountObjectGuard, name string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       guard.rollout.ReleaseNamespace,
			"uid":             "protected-uid",
			"resourceVersion": "42",
		},
	}
}

func serviceAccountObjectRequest(guard *ServiceAccountObjectGuard, operation, name, username string, groups []any) map[string]any {
	if groups == nil {
		groups = []any{}
	}
	return map[string]any{
		"operation": operation,
		"namespace": guard.rollout.ReleaseNamespace,
		"name":      name,
		"dryRun":    false,
		"resource": map[string]any{
			"group":    "",
			"version":  "v1",
			"resource": "serviceaccounts",
		},
		"userInfo": map[string]any{"username": username, "groups": groups},
	}
}

func mutateServiceAccountObject(t *testing.T, source map[string]any, mutate func(map[string]any)) map[string]any {
	t.Helper()
	clone, ok := rolloutCELClone(t, source).(map[string]any)
	if !ok {
		t.Fatal("ServiceAccount clone is not an object")
	}
	if mutate != nil {
		mutate(clone)
	}
	return clone
}

func serviceAccountObjectPolicyForCEL(t *testing.T, policy *admissionregistrationv1.ValidatingAdmissionPolicy, guard *ServiceAccountObjectGuard, authorized bool) *admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	prepared := policy.DeepCopy()
	namespaceGuard := NamespaceDeletionGuardPolicyName(guard.rollout.ReleaseNamespace, guard.rollout.ReleaseName)
	prepare := func(expression string) string {
		expression = resolveParentHookAuthorizerChecks(expression, namespaceGuard, authorized)
		return strings.ReplaceAll(expression, "namespaceObject", "params")
	}
	for index := range prepared.Spec.MatchConditions {
		prepared.Spec.MatchConditions[index].Expression = prepare(prepared.Spec.MatchConditions[index].Expression)
	}
	for index := range prepared.Spec.Variables {
		prepared.Spec.Variables[index].Expression = prepare(prepared.Spec.Variables[index].Expression)
	}
	for index := range prepared.Spec.Validations {
		prepared.Spec.Validations[index].Expression = prepare(prepared.Spec.Validations[index].Expression)
	}
	return prepared
}

func allAdmissionValidationsAllow(t *testing.T, policy *admissionregistrationv1.ValidatingAdmissionPolicy, object, oldObject, request map[string]any, namespaceObject map[string]any) bool {
	t.Helper()
	for _, allowed := range evaluatePolicyValidations(t, policy, object, oldObject, request, namespaceObject) {
		if !allowed {
			return false
		}
	}
	return true
}
