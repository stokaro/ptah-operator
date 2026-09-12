package crdupgrade

// These tests are intentionally white-box because hook ordering, exact
// metadata, and denial envelopes are private parts of one atomic uninstall
// protocol and are not observable through a useful public API.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestTeardownRetirementIdentityIsContentVersioned(t *testing.T) {
	t.Parallel()

	rollout := teardownRetirementTestRollout()
	wantAttempt := hookIdentityDigest(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	gotAttempt, err := TeardownRetirementAttempt(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	if err != nil {
		t.Fatal(err)
	}
	if gotAttempt != wantAttempt || len(gotAttempt) != 64 {
		t.Fatalf("attempt = %q, want full release identity digest %q", gotAttempt, wantAttempt)
	}
	marker, err := TeardownRetirementProbeName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ptah-teardown-probe-v1-1-" + wantAttempt[:12]; marker != want {
		t.Fatalf("marker = %q, want %q", marker, want)
	}
	stableDigest := sha256.Sum256([]byte(teardownRetirementContractVersion + "\n" + rollout.ReleaseNamespace + "\n" + rollout.ReleaseName))
	baseFences := make(map[TeardownFence]string, 2)
	for _, fence := range []TeardownFence{TeardownFenceA, TeardownFenceB} {
		name, err := TeardownRetirementFenceName(fence, rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(name, fmt.Sprintf("%x", stableDigest)[:12]) || len(name) > 63 {
			t.Fatalf("fence %s name = %q", fence, name)
		}
		baseFences[fence] = name
	}

	mutations := []struct {
		name      string
		namespace string
		release   string
		sequence  int32
		image     string
	}{
		{name: "namespace", namespace: "other", release: rollout.ReleaseName, sequence: 1, image: rollout.ManagerImage},
		{name: "release", namespace: rollout.ReleaseNamespace, release: "other", sequence: 1, image: rollout.ManagerImage},
		{name: "sequence", namespace: rollout.ReleaseNamespace, release: rollout.ReleaseName, sequence: 2, image: rollout.ManagerImage},
		{name: "image", namespace: rollout.ReleaseNamespace, release: rollout.ReleaseName, sequence: 1, image: "registry.example/ptah@sha256:" + strings.Repeat("b", 64)},
	}
	for _, mutation := range mutations {
		mutation := mutation
		t.Run(mutation.name, func(t *testing.T) {
			t.Parallel()
			attempt, err := TeardownRetirementAttempt(mutation.namespace, mutation.release, mutation.sequence, mutation.image)
			if err != nil {
				t.Fatal(err)
			}
			if attempt == wantAttempt {
				t.Fatal("changed release identity reused the teardown attempt")
			}
			for _, fence := range []TeardownFence{TeardownFenceA, TeardownFenceB} {
				name, err := TeardownRetirementFenceName(fence, mutation.namespace, mutation.release, mutation.sequence, mutation.image)
				if err != nil {
					t.Fatal(err)
				}
				wantStable := mutation.name == "sequence" || mutation.name == "image"
				if (name == baseFences[fence]) != wantStable {
					t.Fatalf("fence %s name = %q; stable across attempt-only mutation = %v", fence, name, wantStable)
				}
			}
		})
	}
}

func TestTeardownRetirementRejectsInvalidIdentity(t *testing.T) {
	t.Parallel()

	validImage := "registry.example/ptah@sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name      string
		namespace string
		release   string
		sequence  int32
		image     string
	}{
		{name: "empty namespace", release: "ptah", sequence: 1, image: validImage},
		{name: "padded release", namespace: "ptah-system", release: " ptah", sequence: 1, image: validImage},
		{name: "zero sequence", namespace: "ptah-system", release: "ptah", image: validImage},
		{name: "mutable image", namespace: "ptah-system", release: "ptah", sequence: 1, image: "registry.example/ptah:latest"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := TeardownRetirementAttempt(test.namespace, test.release, test.sequence, test.image); err == nil {
				t.Fatal("invalid teardown identity was accepted")
			}
		})
	}
	if _, err := TeardownRetirementFenceName("c", "ptah-system", "ptah", 1, validImage); err == nil {
		t.Fatal("unknown fence was accepted")
	}
}

func TestTeardownRetirementMarkerContractIsExact(t *testing.T) {
	t.Parallel()

	guard := NewTeardownRetirementGuard(teardownRetirementTestRollout())
	marker, err := guard.Marker()
	if err != nil {
		t.Fatal(err)
	}
	if marker.Immutable == nil || !*marker.Immutable || len(marker.Data[teardownRetirementAttemptDataKey]) != 64 ||
		marker.Data[teardownRetirementSequenceDataKey] != "1" || len(marker.BinaryData) != 0 ||
		len(marker.OwnerReferences) != 0 || len(marker.Finalizers) != 0 {
		t.Fatalf("marker = %#v", marker)
	}
	if marker.Annotations["helm.sh/hook"] != "pre-delete" ||
		marker.Annotations["helm.sh/hook-weight"] != teardownRetirementMarkerHookWeight ||
		marker.Annotations["helm.sh/hook-delete-policy"] != "before-hook-creation,hook-succeeded" {
		t.Fatalf("marker hook annotations = %#v", marker.Annotations)
	}
	if err := guard.VerifyMarker(marker.DeepCopy()); err != nil {
		t.Fatal(err)
	}

	mutations := []struct {
		name   string
		mutate func(*corev1.ConfigMap)
	}{
		{name: "attempt", mutate: func(object *corev1.ConfigMap) {
			object.Data[teardownRetirementAttemptDataKey] = strings.Repeat("b", 64)
		}},
		{name: "annotation", mutate: func(object *corev1.ConfigMap) { object.Annotations[teardownRetirementAttemptAnnotation] = "foreign" }},
		{name: "label", mutate: func(object *corev1.ConfigMap) { object.Labels[instanceLabel] = "foreign" }},
		{name: "mutable", mutate: func(object *corev1.ConfigMap) { value := false; object.Immutable = &value }},
		{name: "binary data", mutate: func(object *corev1.ConfigMap) { object.BinaryData = map[string][]byte{"x": {1}} }},
		{name: "owner", mutate: func(object *corev1.ConfigMap) { object.OwnerReferences = []metav1.OwnerReference{{Name: "owner"}} }},
	}
	for _, mutation := range mutations {
		mutation := mutation
		t.Run(mutation.name, func(t *testing.T) {
			t.Parallel()
			changed := marker.DeepCopy()
			mutation.mutate(changed)
			if err := guard.VerifyMarker(changed); err == nil {
				t.Fatal("foreign marker was accepted")
			}
		})
	}
}

func TestTeardownRetirementPhaseIsDerivedFromExactActivation(t *testing.T) {
	t.Parallel()

	guard := NewTeardownRetirementGuard(teardownRetirementTestRollout())
	active := activationObject(guard.rollout.releaseActivationGuard(), 0)
	draining := active.DeepCopy()
	draining.Data = map[string]string{
		activeReleaseDataKey:                "0",
		controllerCredentialsDataKey:        string(ControllerCredentialsDraining),
		controllerCredentialsTargetDataKey:  strconv.FormatInt(int64(guard.rollout.ReleaseSequence), 10),
		controllerCredentialsAttemptDataKey: guard.attempt(),
	}
	foreignDrain := draining.DeepCopy()
	foreignDrain.Data[controllerCredentialsAttemptDataKey] = strings.Repeat("f", 64)
	foreignObject := active.DeepCopy()
	foreignObject.Labels[instanceLabel] = "foreign"

	tests := []struct {
		name    string
		object  *corev1.ConfigMap
		err     error
		want    TeardownRetirementPhase
		wantErr string
	}{
		{name: "credential active", object: active, want: TeardownRetirementActive},
		{name: "matching credential drain", object: draining, want: TeardownRetirementActive},
		{name: "activation absent", err: apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, ReleaseActivationName), want: TeardownRetirementTerminal},
		{name: "unauthorized read", err: apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, ReleaseActivationName, errors.New("denied")), wantErr: "get teardown retirement activation"},
		{name: "foreign drain tuple", object: foreignDrain, wantErr: "want candidate"},
		{name: "foreign activation", object: foreignObject, wantErr: "verify teardown retirement activation"},
		{name: "nil API object", wantErr: "verify teardown retirement activation"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := guard.Phase(context.Background(), teardownRetirementActivationReader{object: test.object, err: test.err})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Phase() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("Phase() = %q, %v, want %q", got, err, test.want)
			}
		})
	}
}

func TestTeardownRetirementOriginalFencesAreIndependentAndOrdered(t *testing.T) {
	t.Parallel()

	guard := NewTeardownRetirementGuard(teardownRetirementTestRollout())
	type expectedWeights struct{ policy, binding string }
	wants := map[TeardownFence]expectedWeights{
		TeardownFenceA: {policy: teardownFenceAPolicyHookWeight, binding: teardownFenceABindingHookWeight},
		TeardownFenceB: {policy: teardownFenceBPolicyHookWeight, binding: teardownFenceBBindingHookWeight},
	}
	seenMessages := map[string]struct{}{}
	seenManagers := map[string]struct{}{}
	for fence, want := range wants {
		policy, binding, probe, err := guard.OriginalFencePair(fence)
		if err != nil {
			t.Fatal(err)
		}
		if policy.Annotations["helm.sh/hook-weight"] != want.policy || binding.Annotations["helm.sh/hook-weight"] != want.binding ||
			policy.Annotations["helm.sh/hook-delete-policy"] != "before-hook-creation" || binding.Annotations["helm.sh/hook-delete-policy"] != "before-hook-creation" {
			t.Fatalf("fence %s weights/deletion = %#v %#v", fence, policy.Annotations, binding.Annotations)
		}
		if policy.Spec.ParamKind != nil || binding.Spec.ParamRef != nil {
			t.Fatalf("fence %s depends on a parameter that can disappear during finalization", fence)
		}
		if len(policy.Spec.MatchConstraints.ResourceRules) != 1 {
			t.Fatalf("fence %s has %d resource rules", fence, len(policy.Spec.MatchConstraints.ResourceRules))
		}
		rule := policy.Spec.MatchConstraints.ResourceRules[0].RuleWithOperations
		if !slices.Equal(rule.Operations, []admissionregistrationv1.OperationType{
			admissionregistrationv1.Create, admissionregistrationv1.Update, admissionregistrationv1.Delete, admissionregistrationv1.Connect,
		}) || !slices.Equal(rule.Resources, []string{"*/*"}) {
			t.Fatalf("fence %s does not independently cover the full admission surface: %#v", fence, rule)
		}
		joined := admissionPolicyText(policy)
		for _, required := range []string{
			"serviceaccounts", "token", "system:nodes", serviceAccountPodNameExtra, serviceAccountPodUIDExtra,
			guard.bootstrapServiceAccountName(), guard.probeAJobName(), guard.gateJobName(),
			guard.quiesceJobName(), guard.cleanupJobName(), guard.finalJobName(),
			ReleaseActivationName,
			"exec", "attach", "portforward", "proxy", "ephemeralcontainers", "resize",
			"after uninstall fencing", probe.FieldManager, probe.Message,
		} {
			if !strings.Contains(joined, required) {
				t.Errorf("fence %s lacks %q", fence, required)
			}
		}
		if strings.Contains(joined, "params") {
			t.Errorf("fence %s is not static: %s", fence, joined)
		}
		if !teardownRetirementProbeFieldManagerPattern.MatchString(probe.FieldManager) {
			t.Errorf("fence %s field manager = %q", fence, probe.FieldManager)
		}
		seenMessages[probe.Message] = struct{}{}
		seenManagers[probe.FieldManager] = struct{}{}
	}
	if len(seenMessages) != 2 || len(seenManagers) != 2 {
		t.Fatal("A and B do not have independent exact probe causes")
	}
}

func TestTeardownRetirementDormantFencesProtectOnlyBootstrapBoundary(t *testing.T) {
	t.Parallel()

	guard := NewTeardownRetirementGuard(teardownRetirementTestRollout())
	for _, fence := range []TeardownFence{TeardownFenceA, TeardownFenceB} {
		policy, binding, probe, err := guard.DormantFencePair(fence)
		if err != nil {
			t.Fatal(err)
		}
		if policy.Spec.ParamKind != nil || binding.Spec.ParamRef != nil {
			t.Fatalf("dormant fence %s is parameterized", fence)
		}
		for _, annotations := range []map[string]string{policy.Annotations, binding.Annotations} {
			for _, key := range []string{"helm.sh/hook", "helm.sh/hook-weight", "helm.sh/hook-delete-policy"} {
				if _, found := annotations[key]; found {
					t.Fatalf("dormant fence %s unexpectedly carries %s", fence, key)
				}
			}
		}
		match := policy.Spec.MatchConditions[0].Expression
		for _, required := range []string{guard.bootstrapServiceAccountName(), guard.probeAJobName(), guard.gateJobName(), `subResource == "status"`, `request.operation == "DELETE"`} {
			if !strings.Contains(match, required) {
				t.Fatalf("dormant fence %s match omits %q", fence, required)
			}
		}
		for _, forbidden := range []string{guard.markerName(), guard.quiesceJobName(), guard.cleanupJobName(), guard.finalJobName(), guard.rollout.ControllerServiceAccountName} {
			if strings.Contains(match, forbidden) {
				t.Fatalf("dormant fence %s match unexpectedly includes %q", fence, forbidden)
			}
		}
		if probe.PolicyName != policy.Name || probe.BindingName != binding.Name {
			t.Fatalf("dormant fence %s identity probe = %#v", fence, probe)
		}
		joined := admissionPolicyText(policy)
		for _, required := range []string{
			teardownRetirementHelmAuthorizerExpression(),
			teardownRetirementJobControllerPrincipalExpression(),
			teardownRetirementSchedulerPrincipalExpression(),
			`condition.type in ["Complete", "Failed"]`,
		} {
			if !strings.Contains(joined, required) {
				t.Fatalf("dormant fence %s omits bootstrap-origin contract %q", fence, required)
			}
		}
	}
}

func TestTeardownRetirementClassifiesEveryExactHookLocalState(t *testing.T) {
	t.Parallel()

	rollout, policyClient, bindingClient, _ := readyRolloutGuard()
	guard := NewTeardownRetirementGuard(rollout)
	pairs, err := guard.RetirementPairs()
	if err != nil {
		t.Fatal(err)
	}
	var pair TeardownRetirementPair
	var originalPolicy *admissionregistrationv1.ValidatingAdmissionPolicy
	var originalBinding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	for _, candidate := range pairs {
		if policyClient.objects[candidate.Original.Name] != nil && bindingClient.objects[candidate.Original.Name] != nil {
			pair = candidate
			originalPolicy = policyClient.objects[candidate.Original.Name]
			originalBinding = bindingClient.objects[candidate.Original.Name]
			break
		}
	}
	if originalPolicy == nil || originalBinding == nil {
		t.Fatal("test inventory contains no exact original pair")
	}
	tests := []struct {
		name    string
		policy  *admissionregistrationv1.ValidatingAdmissionPolicy
		binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
		want    TeardownPairForm
		bad     bool
	}{
		{name: "original", policy: originalPolicy.DeepCopy(), binding: originalBinding.DeepCopy(), want: TeardownPairOriginal},
		{name: "policy delete", binding: originalBinding.DeepCopy(), want: TeardownPairReplacingPolicy},
		{name: "policy create", policy: pair.Policy.DeepCopy(), binding: originalBinding.DeepCopy(), want: TeardownPairPolicyReplaced},
		{name: "binding delete", policy: pair.Policy.DeepCopy(), want: TeardownPairReplacingBinding},
		{name: "replayed policy delete", binding: pair.Binding.DeepCopy(), want: TeardownPairReplayingPolicy},
		{name: "retirement", policy: pair.Policy.DeepCopy(), binding: pair.Binding.DeepCopy(), want: TeardownPairRetirement},
		{name: "hook cleanup", want: TeardownPairAbsent},
		{name: "binding cannot lead policy", policy: originalPolicy.DeepCopy(), binding: pair.Binding.DeepCopy(), bad: true},
		{name: "original policy cannot remain alone", policy: originalPolicy.DeepCopy(), bad: true},
		{name: "foreign", policy: pair.Policy.DeepCopy(), binding: func() *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
			copy := pair.Binding.DeepCopy()
			copy.Annotations[teardownRetirementAttemptAnnotation] = "foreign"
			return copy
		}(), bad: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := guard.ClassifyPair(pair.Original, pair.Policy, pair.Binding, test.policy, test.binding)
			if test.bad {
				if err == nil {
					t.Fatalf("ClassifyPair() = %q, want failure", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("ClassifyPair() = %q, %v, want %q", got, err, test.want)
			}
		})
	}
}

func TestTeardownRetirementInventoryIsSortedDeduplicatedAndBounded(t *testing.T) {
	t.Parallel()

	guard := NewTeardownRetirementGuard(teardownRetirementTestRollout())
	additionalPolicy := &admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "optional-certificate-recovery"}}
	additionalBinding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: additionalPolicy.Name}}
	extended, err := guard.WithOriginalPairs(exactTestOriginalPair(additionalPolicy, additionalBinding))
	if err != nil {
		t.Fatal(err)
	}
	pairs, err := extended.RetirementPairs()
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) < 26 {
		t.Fatalf("retirement inventory has %d pairs; want all current, stable, legacy, and optional pairs", len(pairs))
	}
	names := make([]string, len(pairs))
	for index, pair := range pairs {
		names[index] = pair.Original.Name
		if pair.PolicyWeight != teardownRetirementPairFirstWeight+2*index || pair.BindingWeight != pair.PolicyWeight+1 || pair.BindingWeight > teardownRetirementPairLastWeight {
			t.Errorf("pair %s weights = %d/%d", pair.Original.Name, pair.PolicyWeight, pair.BindingWeight)
		}
		if pair.Policy.Name != pair.Original.Name || pair.Binding.Name != pair.Original.Name || pair.Probe.PolicyName != pair.Original.Name {
			t.Errorf("pair %s changes the stored guard name", pair.Original.Name)
		}
		if pair.Binding.Spec.ParamRef != nil || pair.Policy.Spec.ParamKind != nil {
			t.Errorf("pair %s replacement is parameterized", pair.Original.Name)
		}
		for kind, annotations := range map[string]map[string]string{"policy": pair.Policy.Annotations, "binding": pair.Binding.Annotations} {
			if annotations["helm.sh/hook"] != "pre-delete" || annotations["helm.sh/hook-delete-policy"] != "before-hook-creation,hook-succeeded" {
				t.Errorf("pair %s %s is not owned by successful Helm hook cleanup: %#v", pair.Original.Name, kind, annotations)
			}
		}
	}
	if !slices.IsSorted(names) {
		t.Fatalf("inventory is not deterministic: %v", names)
	}
	if len(slices.Compact(slices.Clone(names))) != len(names) {
		t.Fatalf("inventory has duplicate names: %v", names)
	}
	if !slices.Contains(names, additionalPolicy.Name) {
		t.Fatal("conditional optional guard was not appended")
	}
	if _, err := extended.WithOriginalPairs(exactTestOriginalPair(additionalPolicy, additionalBinding)); err != nil {
		// The append-only extension rejects a duplicate before it can make the
		// complete retirement inventory ambiguous.
	} else {
		t.Fatal("duplicate append unexpectedly passed validation")
	}
}

func TestTeardownRetirementActivePreflightAcceptsEveryCrashTransition(t *testing.T) {
	t.Parallel()

	for _, legacyPresent := range []bool{false, true} {
		legacyPresent := legacyPresent
		t.Run(fmt.Sprintf("legacy-present-%t", legacyPresent), func(t *testing.T) {
			t.Parallel()
			inventory := newTeardownRetirementTestInventory(t, legacyPresent)
			for index, pair := range inventory.pairs {
				steps := []struct {
					name            string
					policy, binding teardownRetirementObjectForm
				}{
					{name: "baseline", policy: teardownRetirementObjectOriginal, binding: teardownRetirementObjectOriginal},
					{name: "policy deleted", policy: teardownRetirementObjectAbsent, binding: teardownRetirementObjectOriginal},
					{name: "policy created", policy: teardownRetirementObjectRetired, binding: teardownRetirementObjectOriginal},
					{name: "binding deleted", policy: teardownRetirementObjectRetired, binding: teardownRetirementObjectAbsent},
					{name: "binding created", policy: teardownRetirementObjectRetired, binding: teardownRetirementObjectRetired},
					{name: "replay policy deleted", policy: teardownRetirementObjectAbsent, binding: teardownRetirementObjectRetired},
					{name: "success cleanup complete", policy: teardownRetirementObjectAbsent, binding: teardownRetirementObjectAbsent},
				}
				if !inventory.originPresent(pair) {
					steps = slices.Delete(steps, 0, 3)
				}
				for _, step := range steps {
					policies, bindings := inventory.baseline()
					for previous := 0; previous < index; previous++ {
						inventory.set(policies, bindings, inventory.pairs[previous], teardownRetirementObjectRetired, teardownRetirementObjectRetired)
					}
					inventory.set(policies, bindings, pair, step.policy, step.binding)
					if _, err := inventory.guard.PreflightPairsForPhase(
						context.Background(),
						&rolloutPolicyClient{objects: policies},
						&rolloutBindingClient{objects: bindings},
						TeardownRetirementActive,
					); err != nil {
						t.Fatalf("pair %d %s after %s: %v", index, pair.Original.Name, step.name, err)
					}
				}
			}
		})
	}
}

func TestTeardownRetirementParentV1UpgradeAndFreshInstallStates(t *testing.T) {
	t.Parallel()

	fresh := newTeardownRetirementTestInventory(t, false)
	policies, bindings := fresh.baseline()
	if _, err := fresh.guard.PreflightPairsForPhase(
		context.Background(),
		&rolloutPolicyClient{objects: policies},
		&rolloutBindingClient{objects: bindings},
		TeardownRetirementActive,
	); err != nil {
		t.Fatalf("fresh install without retained parent v1 pairs: %v", err)
	}
	for _, name := range []string{
		legacyParentHookJobOriginGuardPolicyName(fresh.guard.rollout.ReleaseNamespace, fresh.guard.rollout.ReleaseName),
		legacyParentHookPodOriginGuardPolicyName(fresh.guard.rollout.ReleaseNamespace, fresh.guard.rollout.ReleaseName),
	} {
		if policies[name] != nil || bindings[name] != nil {
			t.Fatalf("fresh install baseline unexpectedly contains parent v1 pair %s", name)
		}
	}

	upgrade := newTeardownRetirementTestInventory(t, true)
	policies, bindings = upgrade.baseline()
	if _, err := upgrade.guard.PreflightPairsForPhase(
		context.Background(),
		&rolloutPolicyClient{objects: policies},
		&rolloutBindingClient{objects: bindings},
		TeardownRetirementActive,
	); err != nil {
		t.Fatalf("upgrade from exact retained parent v1 pairs: %v", err)
	}
	legacyJobName := legacyParentHookJobOriginGuardPolicyName(upgrade.guard.rollout.ReleaseNamespace, upgrade.guard.rollout.ReleaseName)
	changed := policies[legacyJobName].DeepCopy()
	changed.Spec.Validations[0].Expression = "true"
	policies[legacyJobName] = changed
	if _, err := upgrade.guard.PreflightPairsForPhase(
		context.Background(),
		&rolloutPolicyClient{objects: policies},
		&rolloutBindingClient{objects: bindings},
		TeardownRetirementActive,
	); err == nil {
		t.Fatal("drifted retained parent v1 policy was accepted")
	}
}

func TestTeardownRetirementActivePreflightIsClosedUnderRepeatedRetries(t *testing.T) {
	t.Parallel()

	inventory := newTeardownRetirementTestInventory(t, true)
	frontier := -1
	for index := len(inventory.pairs) / 2; index < len(inventory.pairs); index++ {
		if inventory.pairs[index].Original.OptionalGroup == "" {
			frontier = index
			break
		}
	}
	if frontier < 4 {
		t.Fatal("test inventory has no useful mandatory frontier")
	}
	gaps := [][2]teardownRetirementObjectForm{
		{teardownRetirementObjectAbsent, teardownRetirementObjectAbsent},
		{teardownRetirementObjectAbsent, teardownRetirementObjectRetired},
		{teardownRetirementObjectRetired, teardownRetirementObjectAbsent},
		{teardownRetirementObjectRetired, teardownRetirementObjectRetired},
	}
	for replay := 0; replay < frontier; replay++ {
		policies, bindings := inventory.baseline()
		for index := 0; index < frontier; index++ {
			gap := gaps[index%len(gaps)]
			inventory.set(policies, bindings, inventory.pairs[index], gap[0], gap[1])
		}
		// A later attempt was interrupted after deleting the frontier policy.
		inventory.set(policies, bindings, inventory.pairs[frontier], teardownRetirementObjectAbsent, teardownRetirementObjectOriginal)
		// A new retry can simultaneously interrupt any earlier, already
		// progressed pair without invalidating the historical frontier.
		inventory.set(policies, bindings, inventory.pairs[replay], teardownRetirementObjectAbsent, teardownRetirementObjectRetired)
		if _, err := inventory.guard.PreflightPairsForPhase(
			context.Background(),
			&rolloutPolicyClient{objects: policies},
			&rolloutBindingClient{objects: bindings},
			TeardownRetirementActive,
		); err != nil {
			t.Fatalf("replay interruption at pair %d with frontier %d: %v", replay, frontier, err)
		}
	}
}

func TestTeardownRetirementActivePreflightRejectsForeignAndUnreachableStates(t *testing.T) {
	t.Parallel()

	inventory := newTeardownRetirementTestInventory(t, true)
	legacy := make([]int, 0, 2)
	for index, pair := range inventory.pairs {
		if pair.Original.OptionalGroup != "" {
			legacy = append(legacy, index)
		}
	}
	if len(legacy) < 2 {
		t.Fatal("test inventory has fewer than two optional legacy pairs")
	}
	tests := []struct {
		name    string
		arrange func(map[string]*admissionregistrationv1.ValidatingAdmissionPolicy, map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding)
	}{
		{name: "binding leads policy", arrange: func(policies map[string]*admissionregistrationv1.ValidatingAdmissionPolicy, bindings map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			inventory.set(policies, bindings, inventory.pairs[0], teardownRetirementObjectOriginal, teardownRetirementObjectRetired)
		}},
		{name: "original policy without binding", arrange: func(policies map[string]*admissionregistrationv1.ValidatingAdmissionPolicy, bindings map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			inventory.set(policies, bindings, inventory.pairs[0], teardownRetirementObjectOriginal, teardownRetirementObjectAbsent)
		}},
		{name: "progress after untouched pair", arrange: func(policies map[string]*admissionregistrationv1.ValidatingAdmissionPolicy, bindings map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			inventory.set(policies, bindings, inventory.pairs[1], teardownRetirementObjectRetired, teardownRetirementObjectOriginal)
		}},
		{name: "inconsistent optional origin", arrange: func(policies map[string]*admissionregistrationv1.ValidatingAdmissionPolicy, bindings map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			inventory.set(policies, bindings, inventory.pairs[legacy[1]], teardownRetirementObjectAbsent, teardownRetirementObjectAbsent)
		}},
		{name: "foreign policy", arrange: func(policies map[string]*admissionregistrationv1.ValidatingAdmissionPolicy, _ map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			changed := policies[inventory.pairs[0].Original.Name].DeepCopy()
			changed.Spec.Validations[0].Expression = "true"
			policies[changed.Name] = changed
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			policies, bindings := inventory.baseline()
			test.arrange(policies, bindings)
			if _, err := inventory.guard.PreflightPairsForPhase(
				context.Background(),
				&rolloutPolicyClient{objects: policies},
				&rolloutBindingClient{objects: bindings},
				TeardownRetirementActive,
			); err == nil {
				t.Fatal("foreign or unreachable state was accepted")
			}
		})
	}
}

func TestTeardownRetirementTerminalPreflightNeverReopensAuthority(t *testing.T) {
	t.Parallel()

	inventory := newTeardownRetirementTestInventory(t, true)
	nonAuthoritative := [][2]teardownRetirementObjectForm{
		{teardownRetirementObjectRetired, teardownRetirementObjectRetired},
		{teardownRetirementObjectRetired, teardownRetirementObjectAbsent},
		{teardownRetirementObjectAbsent, teardownRetirementObjectRetired},
		{teardownRetirementObjectAbsent, teardownRetirementObjectAbsent},
	}
	policies, bindings := inventory.baseline()
	for index, pair := range inventory.pairs {
		state := nonAuthoritative[index%len(nonAuthoritative)]
		inventory.set(policies, bindings, pair, state[0], state[1])
	}
	if _, err := inventory.guard.PreflightPairsForPhase(
		context.Background(),
		&rolloutPolicyClient{objects: policies},
		&rolloutBindingClient{objects: bindings},
		TeardownRetirementTerminal,
	); err != nil {
		t.Fatalf("terminal crash-cleanup state: %v", err)
	}

	for _, component := range []string{"policy", "binding"} {
		policies, bindings := inventory.baseline()
		for _, pair := range inventory.pairs {
			inventory.set(policies, bindings, pair, teardownRetirementObjectRetired, teardownRetirementObjectRetired)
		}
		if component == "policy" {
			policies[inventory.pairs[0].Original.Name] = inventory.originalPolicies[inventory.pairs[0].Original.Name].DeepCopy()
		} else {
			bindings[inventory.pairs[0].Original.Name] = inventory.originalBindings[inventory.pairs[0].Original.Name].DeepCopy()
		}
		if _, err := inventory.guard.PreflightPairsForPhase(
			context.Background(),
			&rolloutPolicyClient{objects: policies},
			&rolloutBindingClient{objects: bindings},
			TeardownRetirementTerminal,
		); err == nil {
			t.Fatalf("terminal state accepted an original %s", component)
		}
	}
}

func TestTeardownRetirementJobDeleteRequiresTerminalContractAndHelmAuthority(t *testing.T) {
	t.Parallel()

	guard := NewTeardownRetirementGuard(teardownRetirementTestRollout())
	jobContract := guard.teardownJobValidationExpressions(false)
	deleteContract := guard.teardownJobDeletionValidationExpressions(false)
	if len(deleteContract) != len(jobContract)+1 {
		t.Fatalf("delete contract has %d expressions, want %d", len(deleteContract), len(jobContract)+1)
	}
	for index := 2; index < len(jobContract); index++ {
		want := strings.ReplaceAll(jobContract[index], "object.", "oldObject.")
		if deleteContract[index] != want {
			t.Fatalf("delete contract expression %d does not preserve the exact stored Job contract", index)
		}
	}

	terminal := deleteContract[len(deleteContract)-1]
	tests := []struct {
		name       string
		conditions []map[string]any
		want       bool
	}{
		{name: "pending"},
		{name: "complete false", conditions: []map[string]any{{"type": "Complete", "status": "False"}}},
		{name: "complete", conditions: []map[string]any{{"type": "Complete", "status": "True"}}, want: true},
		{name: "failed", conditions: []map[string]any{{"type": "Failed", "status": "True"}}, want: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			oldObject := map[string]any{"status": map[string]any{}}
			if test.conditions != nil {
				oldObject["status"].(map[string]any)["conditions"] = test.conditions
			}
			got := evaluateRolloutCEL(t, terminal, map[string]any{"oldObject": oldObject}, nil)
			if got != test.want {
				t.Fatalf("terminal deletion expression = %v, want %v", got, test.want)
			}
		})
	}

	authority := teardownRetirementHelmAuthorizerExpression()
	for _, verb := range []string{"create", "update", "delete"} {
		if strings.Count(authority, `.check("`+verb+`").allowed()`) < 2 {
			t.Fatalf("Job DELETE authority does not require %s on both admission resource kinds", verb)
		}
	}
	wantAuthorityValidation := `!variables.isProtectedJobDelete || (` + authority + `)`
	for _, form := range []struct {
		name  string
		build func(TeardownFence) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding, TeardownRetirementProbe, error)
	}{
		{name: "ordinary", build: guard.DormantFencePair},
		{name: "broad", build: guard.OriginalFencePair},
	} {
		form := form
		t.Run(form.name, func(t *testing.T) {
			policy, _, _, err := form.build(TeardownFenceA)
			if err != nil {
				t.Fatal(err)
			}
			match := policy.Spec.MatchConditions[0].Expression
			if !strings.Contains(match, `resource == "jobs"`) || !strings.Contains(match, `request.operation == "DELETE"`) {
				t.Fatal("Job DELETE is outside the fence match")
			}
			if !slices.ContainsFunc(policy.Spec.Validations, func(validation admissionregistrationv1.Validation) bool {
				return validation.Expression == wantAuthorityValidation
			}) {
				t.Fatal("terminal Job deletion is not gated by Helm-level admission-management authority")
			}
		})
	}

	failedObject := map[string]any{
		"status": map[string]any{
			"conditions": []map[string]any{{"type": "Failed", "status": "True"}},
		},
	}
	request := map[string]any{"operation": "DELETE"}
	combined := "(" + authority + ") && (" + terminal + ")"
	for _, test := range []struct {
		name    string
		allowed map[string]bool
		want    bool
	}{
		{name: "namespace writer denied", allowed: map[string]bool{}},
		{
			name: "Helm admission manager allowed",
			allowed: map[string]bool{
				"validatingadmissionpolicies/create":       true,
				"validatingadmissionpolicybindings/create": true,
				"validatingadmissionpolicies/update":       true,
				"validatingadmissionpolicybindings/update": true,
				"validatingadmissionpolicies/delete":       true,
				"validatingadmissionpolicybindings/delete": true,
			},
			want: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			expression := resolveTeardownRetirementAuthorizerChecks(t, combined, test.allowed)
			got := evaluateRolloutCEL(t, expression, map[string]any{"request": request, "oldObject": failedObject}, nil)
			if got != test.want {
				t.Fatalf("Failed Job DELETE admission = %v, want %v", got, test.want)
			}
		})
	}
}

func resolveTeardownRetirementAuthorizerChecks(t *testing.T, expression string, allowed map[string]bool) string {
	t.Helper()
	for _, resource := range []string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"} {
		for _, verb := range []string{"create", "update", "delete"} {
			check := fmt.Sprintf(
				`authorizer.group("admissionregistration.k8s.io").resource(%q).check(%q).allowed()`,
				resource,
				verb,
			)
			expression = strings.ReplaceAll(expression, check, strconv.FormatBool(allowed[resource+"/"+verb]))
		}
	}
	if strings.Contains(expression, "authorizer.") {
		t.Fatalf("unresolved authorizer check in %q", expression)
	}
	return expression
}

func TestTeardownRetirementControllerStatusPrincipalsRequireExactGroups(t *testing.T) {
	t.Parallel()

	expression := teardownRetirementJobControllerPrincipalExpression()
	tests := []struct {
		name     string
		username string
		groups   []string
		want     bool
	}{
		{name: "controller manager certificate", username: "system:kube-controller-manager", groups: []string{"system:authenticated"}, want: true},
		{name: "job controller service account", username: "system:serviceaccount:kube-system:job-controller", groups: []string{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"}, want: true},
		{name: "controller manager injected group", username: "system:kube-controller-manager", groups: []string{"system:authenticated", "system:masters"}},
		{name: "service account missing namespace group", username: "system:serviceaccount:kube-system:job-controller", groups: []string{"system:serviceaccounts", "system:authenticated"}},
		{name: "same name namespace writer", username: "job-controller", groups: []string{"system:authenticated"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := evaluateRolloutCEL(t, expression, map[string]any{"request": map[string]any{"userInfo": map[string]any{"username": test.username, "groups": test.groups}}}, nil)
			if got != test.want {
				t.Fatalf("principal expression = %v, want %v", got, test.want)
			}
		})
	}
}

func TestTeardownRetirementDeleteMatchesUseOldObjectName(t *testing.T) {
	t.Parallel()

	guard := NewTeardownRetirementGuard(teardownRetirementTestRollout())
	jobName := guard.probeAJobName()
	podName := jobName + "-abc12"
	for _, form := range []struct {
		name  string
		build func(TeardownFence) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding, TeardownRetirementProbe, error)
	}{
		{name: "ordinary", build: guard.DormantFencePair},
		{name: "broad", build: guard.OriginalFencePair},
	} {
		form := form
		t.Run(form.name, func(t *testing.T) {
			policy, _, _, err := form.build(TeardownFenceA)
			if err != nil {
				t.Fatal(err)
			}
			jobExpression := teardownRetirementTestVariableExpression(t, policy, "isProtectedJobDelete")
			podExpression := teardownRetirementTestVariableExpression(t, policy, "isProtectedPodDelete")
			jobRequest := map[string]any{
				"operation": "DELETE", "name": "", "namespace": guard.rollout.ReleaseNamespace,
				"resource": map[string]any{"group": "batch", "version": "v1", "resource": "jobs"},
			}
			podRequest := map[string]any{
				"operation": "DELETE", "name": "", "namespace": guard.rollout.ReleaseNamespace,
				"resource": map[string]any{"group": "", "version": "v1", "resource": "pods"},
			}
			for _, test := range []struct {
				name       string
				expression string
				request    map[string]any
				oldName    string
				want       bool
			}{
				{name: "Job collection member", expression: jobExpression, request: jobRequest, oldName: jobName, want: true},
				{name: "foreign Job", expression: jobExpression, request: jobRequest, oldName: "foreign"},
				{name: "Pod collection member", expression: podExpression, request: podRequest, oldName: podName, want: true},
				{name: "foreign Pod", expression: podExpression, request: podRequest, oldName: "foreign-abc12"},
			} {
				t.Run(test.name, func(t *testing.T) {
					got := evaluateRolloutCEL(t, test.expression, map[string]any{
						"request":   test.request,
						"oldObject": map[string]any{"metadata": map[string]any{"name": test.oldName}},
					}, nil)
					if got != test.want {
						t.Fatalf("protected DELETE match = %v, want %t", got, test.want)
					}
				})
			}
		})
	}

	policy, _, _, err := guard.OriginalFencePair(TeardownFenceA)
	if err != nil {
		t.Fatal(err)
	}
	markerExpression := teardownRetirementTestVariableExpression(t, policy, "isProtectedRetainedMarkerDelete")
	request := map[string]any{
		"operation": "DELETE", "name": "", "namespace": guard.rollout.ReleaseNamespace,
		"resource": map[string]any{"group": "", "version": "v1", "resource": "configmaps"},
	}
	if got := evaluateRolloutCEL(t, markerExpression, map[string]any{
		"request": request, "oldObject": map[string]any{"metadata": map[string]any{"name": ReleaseActivationName}},
	}, nil); got != true {
		t.Fatalf("retained marker collection-delete match = %v, want true", got)
	}
}

func teardownRetirementTestVariableExpression(t *testing.T, policy *admissionregistrationv1.ValidatingAdmissionPolicy, name string) string {
	t.Helper()
	for _, variable := range policy.Spec.Variables {
		if variable.Name == name {
			return variable.Expression
		}
	}
	t.Fatalf("teardown policy %s has no variable %s", policy.Name, name)
	return ""
}

func TestTeardownRetirementPodStatusWritersAreExact(t *testing.T) {
	t.Parallel()

	guard := NewTeardownRetirementGuard(teardownRetirementTestRollout())
	expression := guard.teardownPodStatusValidationExpressions(false)[0]
	tests := []struct {
		name      string
		username  string
		groups    []string
		nodeName  string
		oldStatus map[string]any
		newStatus map[string]any
		want      bool
	}{
		{
			name: "bound node may report status", username: "system:node:worker-1",
			groups: []string{"system:nodes", "system:authenticated"}, nodeName: "worker-1",
			oldStatus: map[string]any{"phase": "Pending"}, newStatus: map[string]any{"phase": "Succeeded"}, want: true,
		},
		{
			name: "different node is rejected", username: "system:node:worker-2",
			groups: []string{"system:nodes", "system:authenticated"}, nodeName: "worker-1",
			oldStatus: map[string]any{"phase": "Pending"}, newStatus: map[string]any{"phase": "Succeeded"},
		},
		{
			name: "scheduler may nominate", username: "system:kube-scheduler",
			groups:    []string{"system:authenticated"},
			oldStatus: map[string]any{"phase": "Pending"}, newStatus: map[string]any{"phase": "Pending", "nominatedNodeName": "worker-1"}, want: true,
		},
		{
			name: "scheduler service account may nominate", username: "system:serviceaccount:kube-system:kube-scheduler",
			groups:    []string{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"},
			oldStatus: map[string]any{"phase": "Pending"}, newStatus: map[string]any{"phase": "Pending", "nominatedNodeName": "worker-1"}, want: true,
		},
		{
			name: "scheduler cannot forge success", username: "system:kube-scheduler",
			groups:    []string{"system:authenticated"},
			oldStatus: map[string]any{"phase": "Pending"}, newStatus: map[string]any{"phase": "Succeeded"},
		},
		{
			name: "scheduler injected group is rejected", username: "system:kube-scheduler",
			groups:    []string{"system:authenticated", "system:masters"},
			oldStatus: map[string]any{"phase": "Pending"}, newStatus: map[string]any{"phase": "Pending", "nominatedNodeName": "worker-1"},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			oldObject := map[string]any{"spec": map[string]any{}, "status": test.oldStatus}
			if test.nodeName != "" {
				oldObject["spec"].(map[string]any)["nodeName"] = test.nodeName
			}
			object := map[string]any{"status": test.newStatus}
			got := evaluateRolloutCEL(t, expression, map[string]any{
				"request":   map[string]any{"userInfo": map[string]any{"username": test.username, "groups": test.groups}},
				"object":    object,
				"oldObject": oldObject,
			}, nil)
			if got != test.want {
				t.Fatalf("Pod status writer expression = %v, want %v", got, test.want)
			}
		})
	}
}

func TestTeardownRetirementSchedulerStatusFieldInventoryIsComplete(t *testing.T) {
	t.Parallel()

	typeOfStatus := reflect.TypeOf(corev1.PodStatus{})
	got := make([]string, 0, typeOfStatus.NumField())
	for index := range typeOfStatus.NumField() {
		name := strings.Split(typeOfStatus.Field(index).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			got = append(got, name)
		}
	}
	if want := teardownRetirementPodStatusFields(); !slices.Equal(got, want) {
		t.Fatalf("PodStatus field inventory changed\n got: %v\nwant: %v", got, want)
	}
}

type teardownRetirementTestInventory struct {
	guard            *TeardownRetirementGuard
	pairs            []TeardownRetirementPair
	originalPolicies map[string]*admissionregistrationv1.ValidatingAdmissionPolicy
	originalBindings map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding
}

func newTeardownRetirementTestInventory(t *testing.T, legacyPresent bool) teardownRetirementTestInventory {
	t.Helper()
	fixture := newReleaseTeardownFixture(t)
	if legacyPresent {
		fixture = newReleaseTeardownFixtureWithLegacyControllerGuards(t)
	}
	rollout := fixture.guard
	policyClient := fixture.policies
	bindingClient := fixture.bindings
	if legacyPresent {
		for _, entry := range NewParentWorkloadGuard(rollout).legacyOriginEntries() {
			policyClient.objects[entry.name] = entry.policy.DeepCopy()
			bindingClient.objects[entry.name] = entry.binding.DeepCopy()
		}
	}
	guard := NewTeardownRetirementGuard(rollout)
	pairs, err := guard.RetirementPairs()
	if err != nil {
		t.Fatal(err)
	}
	policies := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicy, len(pairs))
	bindings := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding, len(pairs))
	for _, pair := range pairs {
		policy := policyClient.objects[pair.Original.Name]
		binding := bindingClient.objects[pair.Original.Name]
		if policy == nil || binding == nil {
			if pair.Original.OptionalGroup == "" || legacyPresent {
				t.Fatalf("original pair %s is missing from the test fixture", pair.Original.Name)
			}
			continue
		}
		if err := pair.Original.VerifyPolicy(policy); err != nil {
			t.Fatalf("fixture policy %s: %v", pair.Original.Name, err)
		}
		if err := pair.Original.VerifyBinding(binding); err != nil {
			t.Fatalf("fixture binding %s: %v", pair.Original.Name, err)
		}
		policies[pair.Original.Name] = policy.DeepCopy()
		bindings[pair.Original.Name] = binding.DeepCopy()
	}
	return teardownRetirementTestInventory{
		guard:            guard,
		pairs:            pairs,
		originalPolicies: policies,
		originalBindings: bindings,
	}
}

func (i teardownRetirementTestInventory) originPresent(pair TeardownRetirementPair) bool {
	return i.originalPolicies[pair.Original.Name] != nil && i.originalBindings[pair.Original.Name] != nil
}

func (i teardownRetirementTestInventory) baseline() (map[string]*admissionregistrationv1.ValidatingAdmissionPolicy, map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	policies := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicy, len(i.originalPolicies))
	bindings := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding, len(i.originalBindings))
	for name, policy := range i.originalPolicies {
		policies[name] = policy.DeepCopy()
	}
	for name, binding := range i.originalBindings {
		bindings[name] = binding.DeepCopy()
	}
	return policies, bindings
}

func (i teardownRetirementTestInventory) set(
	policies map[string]*admissionregistrationv1.ValidatingAdmissionPolicy,
	bindings map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding,
	pair TeardownRetirementPair,
	policyForm, bindingForm teardownRetirementObjectForm,
) {
	name := pair.Original.Name
	switch policyForm {
	case teardownRetirementObjectAbsent:
		delete(policies, name)
	case teardownRetirementObjectOriginal:
		policies[name] = i.originalPolicies[name].DeepCopy()
	case teardownRetirementObjectRetired:
		policies[name] = pair.Policy.DeepCopy()
	default:
		panic(fmt.Sprintf("unsupported test policy form %d", policyForm))
	}
	switch bindingForm {
	case teardownRetirementObjectAbsent:
		delete(bindings, name)
	case teardownRetirementObjectOriginal:
		bindings[name] = i.originalBindings[name].DeepCopy()
	case teardownRetirementObjectRetired:
		bindings[name] = pair.Binding.DeepCopy()
	default:
		panic(fmt.Sprintf("unsupported test binding form %d", bindingForm))
	}
}

func TestTeardownRetirementProbeRequiresExactDenial(t *testing.T) {
	t.Parallel()

	guard := NewTeardownRetirementGuard(teardownRetirementTestRollout())
	_, _, probe, err := guard.OriginalFencePair(TeardownFenceA)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := guard.Marker()
	if err != nil {
		t.Fatal(err)
	}
	marker.UID = "marker-uid"
	marker.ResourceVersion = "1"
	tests := []struct {
		name      string
		updateErr error
		want      bool
		wantError string
	}{
		{name: "exact denial", updateErr: exactPolicyDenialError(probe.PolicyName, probe.BindingName, probe.Message), want: true},
		{name: "wrong policy", updateErr: exactPolicyDenialError("other", probe.BindingName, probe.Message)},
		{name: "wrong binding", updateErr: exactPolicyDenialError(probe.PolicyName, "other", probe.Message)},
		{name: "wrong message", updateErr: exactPolicyDenialError(probe.PolicyName, probe.BindingName, "other")},
		{name: "admitted", wantError: "was admitted"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := &teardownRetirementMarkerClient{marker: marker.DeepCopy(), updateErr: test.updateErr}
			got, err := guard.Probe(context.Background(), client, probe)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Probe() error = %v, want containing %q", err, test.wantError)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("Probe() = %v, %v, want %v", got, err, test.want)
			}
			if !reflect.DeepEqual(client.options.DryRun, []string{metav1.DryRunAll}) || client.options.FieldManager != probe.FieldManager {
				t.Fatalf("Update options = %#v", client.options)
			}
		})
	}
}

func teardownRetirementTestRollout() *RolloutGuard {
	rollout, _, _, _ := readyRolloutGuard()
	return rollout
}

func exactTestOriginalPair(policy *admissionregistrationv1.ValidatingAdmissionPolicy, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) TeardownOriginalPairVerifier {
	return TeardownOriginalPairVerifier{
		Name: policy.Name,
		VerifyPolicy: func(actual *admissionregistrationv1.ValidatingAdmissionPolicy) error {
			if !reflect.DeepEqual(actual, policy) {
				return errors.New("policy differs")
			}
			return nil
		},
		VerifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
			if !reflect.DeepEqual(actual, binding) {
				return errors.New("binding differs")
			}
			return nil
		},
	}
}

func admissionPolicyText(policy *admissionregistrationv1.ValidatingAdmissionPolicy) string {
	var parts []string
	for _, condition := range policy.Spec.MatchConditions {
		parts = append(parts, condition.Expression)
	}
	for _, variable := range policy.Spec.Variables {
		parts = append(parts, variable.Name, variable.Expression)
	}
	for _, validation := range policy.Spec.Validations {
		parts = append(parts, validation.Expression, validation.Message)
	}
	return strings.Join(parts, "\n")
}

type teardownRetirementMarkerClient struct {
	marker    *corev1.ConfigMap
	updateErr error
	options   metav1.UpdateOptions
}

type teardownRetirementActivationReader struct {
	object *corev1.ConfigMap
	err    error
}

func (r teardownRetirementActivationReader) Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.object == nil {
		return nil, nil
	}
	return r.object.DeepCopy(), nil
}

func (c *teardownRetirementMarkerClient) Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error) {
	if c.marker == nil {
		return nil, errors.New("marker missing")
	}
	return c.marker.DeepCopy(), nil
}

func (c *teardownRetirementMarkerClient) Update(_ context.Context, object *corev1.ConfigMap, options metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	c.options = options
	if c.updateErr != nil {
		return nil, c.updateErr
	}
	return object.DeepCopy(), nil
}

func TestTeardownRetirementWeightCapacity(t *testing.T) {
	t.Parallel()

	capacity := (teardownRetirementPairLastWeight - teardownRetirementPairFirstWeight + 1) / 2
	if capacity != 45 {
		t.Fatalf("retirement pair capacity = %d, want 45", capacity)
	}
	if strconv.Itoa(teardownRetirementPairFirstWeight) != "10" || strconv.Itoa(teardownRetirementPairLastWeight) != "99" {
		t.Fatal("unexpected retirement weight range")
	}
}
