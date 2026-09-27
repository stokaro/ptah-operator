package crdupgrade

// These tests intentionally use the package internals: safe deletion depends
// on checking the same contracts each private guard builder compiles.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestReleaseTeardownDeletesExactInventoryInSafeOrder(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t, false)
	wantOrder := expectedReleaseTeardownOrder(fixture.guard)
	if len(wantOrder) != 40 {
		t.Fatalf("known teardown inventory has %d objects, want 40", len(wantOrder))
	}
	if err := fixture.teardown.Preflight(context.Background()); err != nil {
		t.Fatalf("read-only preflight: %v", err)
	}
	if len(fixture.recorder.deletes) != 0 {
		t.Fatalf("read-only preflight issued deletes: %v", fixture.recorder.deletes)
	}

	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.recorder.deletes, wantOrder) {
		t.Fatalf("delete order:\n got: %v\nwant: %v", fixture.recorder.deletes, wantOrder)
	}
	for _, key := range wantOrder {
		options, found := fixture.recorder.options[key]
		if !found {
			t.Fatalf("%s delete options were not recorded", key)
		}
		if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil {
			t.Fatalf("%s delete lacks UID/resourceVersion preconditions: %#v", key, options)
		}
		wantIdentity := fixture.identities[key]
		if *options.Preconditions.UID != wantIdentity.uid || *options.Preconditions.ResourceVersion != wantIdentity.resourceVersion {
			t.Fatalf("%s delete preconditions = %s/%s, want %s/%s", key,
				*options.Preconditions.UID, *options.Preconditions.ResourceVersion,
				wantIdentity.uid, wantIdentity.resourceVersion,
			)
		}
	}
	if remaining := fixture.remaining(); len(remaining) != 0 {
		t.Fatalf("teardown left %v behind", remaining)
	}
	activationName := ReleaseActivationGuardPolicyName(fixture.guard.ReleaseNamespace, fixture.guard.ReleaseName)
	tail := fixture.recorder.deletes[len(fixture.recorder.deletes)-4:]
	wantTail := []string{
		teardownKey("ValidatingAdmissionPolicyBinding", activationName),
		teardownKey("ValidatingAdmissionPolicy", activationName),
		teardownKey("ConfigMap", AdmissionConvergenceMarkerName(fixture.guard.ReleaseNamespace, fixture.guard.ReleaseName, fixture.guard.ReleaseSequence)),
		teardownKey("ConfigMap", ReleaseActivationName),
	}
	if !reflect.DeepEqual(tail, wantTail) {
		t.Fatalf("teardown tail = %v, want %v", tail, wantTail)
	}
}

// The certificate staging Secret is guarded against a Helm deletion, so the
// teardown deletes it itself, after that guard and without reading it: it
// holds a pending CA private key.
func TestReleaseTeardownDeletesTheStagingSecretAfterItsGuardWithoutReadingIt(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t, true)
	order := expectedReleaseTeardownOrder(fixture.guard)
	if len(order) != 43 {
		t.Fatalf("certificate runtime teardown inventory has %d objects, want 43", len(order))
	}
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.recorder.deletes, order) {
		t.Fatalf("delete order:\n got: %v\nwant: %v", fixture.recorder.deletes, order)
	}
	stagingGuard := StagingSecretGuardPolicyName(fixture.guard.ReleaseNamespace, fixture.guard.ReleaseName)
	secretKey := teardownKey("Secret", "ptah-webhook-cert-stage")
	secretIndex := slicesIndex(order, secretKey)
	if slicesIndex(order, teardownKey("ValidatingAdmissionPolicyBinding", stagingGuard)) > secretIndex ||
		slicesIndex(order, teardownKey("ValidatingAdmissionPolicy", stagingGuard)) > secretIndex {
		t.Fatalf("the staging Secret is deleted before its guard: %v", order)
	}
	if options := fixture.recorder.options[secretKey]; options.Preconditions != nil {
		t.Fatalf("the staging Secret delete carries preconditions it could only have from a read: %#v", options)
	}
	if fixture.secrets.present {
		t.Fatal("the staging Secret survived the teardown")
	}

	fixture.recorder.deletes = nil
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("repeated teardown: %v", err)
	}
	if want := []string{secretKey}; !reflect.DeepEqual(fixture.recorder.deletes, want) {
		t.Fatalf("repeated teardown deletes = %v, want only the blind Secret delete %v", fixture.recorder.deletes, want)
	}
}

func TestReleaseTeardownKeepsActivationSelfGuardUntilConsumersAreRemoved(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t, false)
	targets, err := fixture.teardown.targets()
	if err != nil {
		t.Fatalf("targets() error = %v", err)
	}
	activationName := ReleaseActivationGuardPolicyName(
		fixture.guard.ReleaseNamespace,
		fixture.guard.ReleaseName,
	)
	index := func(kind, name string) int {
		t.Helper()
		for targetIndex, target := range targets {
			if target.kind == kind && target.name == name {
				return targetIndex
			}
		}
		t.Fatalf("teardown target %s/%s is missing", kind, name)
		return -1
	}
	activationBindingIndex := index("ValidatingAdmissionPolicyBinding", activationName)
	activationPolicyIndex := index("ValidatingAdmissionPolicy", activationName)
	activationConfigMapIndex := index("ConfigMap", ReleaseActivationName)
	if activationBindingIndex >= activationPolicyIndex || activationPolicyIndex >= activationConfigMapIndex {
		t.Fatalf(
			"activation self-guard order = binding %d, policy %d, ConfigMap %d",
			activationBindingIndex,
			activationPolicyIndex,
			activationConfigMapIndex,
		)
	}
	for _, name := range []string{
		RolloutGuardPolicyName(fixture.guard.ReleaseSequence),
		RuntimeGuardPolicyName(fixture.guard.ReleaseSequence),
		RuntimePodGuardPolicyName(fixture.guard.ReleaseSequence),
		ControllerJobWriteGuardPolicyName(fixture.guard.ReleaseNamespace, fixture.guard.ReleaseName, fixture.guard.ReleaseSequence, fixture.guard.ManagerImage),
		ControllerChunkWriteGuardPolicyName(fixture.guard.ReleaseNamespace, fixture.guard.ReleaseName, fixture.guard.ReleaseSequence, fixture.guard.ManagerImage),
		ControllerPlanWriteGuardPolicyName(fixture.guard.ReleaseNamespace, fixture.guard.ReleaseName, fixture.guard.ReleaseSequence, fixture.guard.ManagerImage),
	} {
		if bindingIndex := index("ValidatingAdmissionPolicyBinding", name); bindingIndex >= activationBindingIndex {
			t.Fatalf("activation self-guard binding precedes consumer binding %s", name)
		}
		if policyIndex := index("ValidatingAdmissionPolicy", name); policyIndex >= activationBindingIndex {
			t.Fatalf("activation self-guard binding precedes consumer policy %s", name)
		}
	}
	// Every ConfigMap a guard protects comes after the guards that could
	// refuse its deletion.
	lastGuardPolicy := 0
	for targetIndex, target := range targets {
		if target.kind == "ValidatingAdmissionPolicy" && target.name != activationName {
			lastGuardPolicy = targetIndex
		}
	}
	for targetIndex, target := range targets {
		if target.kind == "ConfigMap" && targetIndex < lastGuardPolicy {
			t.Fatalf("ConfigMap/%s is deleted before the guard policies that protect it", target.name)
		}
	}
}

func TestReleaseTeardownRetriesOnlyErrorsALaterAttemptCanClear(t *testing.T) {
	t.Parallel()

	readinessName := ParentOriginReadyMarkerName("ptah-system", "ptah")
	readinessKey := teardownKey("ConfigMap", readinessName)
	policyDenial := func(reason string) error {
		message := fmt.Sprintf("ValidatingAdmissionPolicy 'guard' with binding 'guard' denied request: %s", reason)
		return apierrors.NewForbidden(teardownGroupResource("ConfigMap"), readinessName, errors.New(message))
	}
	invalidDenial := apierrors.NewInvalid(
		schema.GroupKind{Kind: "ConfigMap"},
		readinessName,
		field.ErrorList{field.Forbidden(field.NewPath(""), "ValidatingAdmissionPolicy 'guard' with binding 'guard' denied request: refused")},
	)
	for _, test := range []struct {
		name      string
		transient error
	}{
		{name: "a cached admission policy refusal", transient: policyDenial("refused")},
		{name: "a cached admission policy refusal answered as invalid", transient: invalidDenial},
		{name: "an API server that is not answering", transient: apierrors.NewServiceUnavailable("etcd leader changed")},
		{name: "a server timeout", transient: apierrors.NewServerTimeout(teardownGroupResource("ConfigMap"), "delete", 1)},
		{name: "a precondition that moved", transient: apierrors.NewConflict(teardownGroupResource("ConfigMap"), readinessName, errors.New("precondition failed"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newReleaseTeardownFixture(t, false)
			order := expectedReleaseTeardownOrder(fixture.guard)
			readinessIndex := slicesIndex(order, readinessKey)
			attempts := 0
			fixture.recorder.beforeDelete[readinessKey] = func() {
				attempts++
				if attempts < 3 {
					fixture.recorder.errors[readinessKey] = test.transient
					return
				}
				delete(fixture.recorder.errors, readinessKey)
			}

			if err := fixture.teardown.Teardown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if attempts != 3 {
				t.Fatalf("readiness marker delete attempts = %d, want 3", attempts)
			}
			want := append([]string{}, order[:readinessIndex]...)
			want = append(want, readinessKey, readinessKey, readinessKey)
			want = append(want, order[readinessIndex+1:]...)
			if !reflect.DeepEqual(fixture.recorder.deletes, want) {
				t.Fatalf("delete calls = %v, want two retries of %s in %v", fixture.recorder.deletes, readinessKey, want)
			}
		})
	}

	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{
			name: "a refusal that no admission policy gave",
			err:  apierrors.NewForbidden(teardownGroupResource("ConfigMap"), readinessName, errors.New("RBAC: access denied")),
			want: "RBAC: access denied",
		},
		{name: "an error that says nothing about the API", err: errors.New("injected delete failure"), want: "injected delete failure"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newReleaseTeardownFixture(t, false)
			fixture.recorder.errors[readinessKey] = test.err

			err := fixture.teardown.Teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Teardown error = %v, want %q", err, test.want)
			}
			attempts := 0
			for _, key := range fixture.recorder.deletes {
				if key == readinessKey {
					attempts++
				}
			}
			if attempts != 1 {
				t.Fatalf("readiness marker delete attempts = %d, want 1", attempts)
			}
		})
	}
}

// The activation goes back to its bootstrap state before it is deleted, and
// the delete is held to the identity of that reset rather than the read before
// it.
func TestReleaseTeardownReturnsTheActivationToBootstrapBeforeDeletingIt(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t, false)
	activation := fixture.configMaps.objects[ReleaseActivationName]
	activation.Data = map[string]string{activeReleaseDataKey: "1"}
	if reflect.DeepEqual(activation.Data, ReleaseActivationBootstrapData()) {
		t.Fatal("the fixture activation already holds the bootstrap state, so the reset would not be exercised")
	}

	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fixture.configMaps.updates) != 1 {
		t.Fatalf("activation updates = %d, want exactly the reset", len(fixture.configMaps.updates))
	}
	reset := fixture.configMaps.updates[0]
	if reset.Name != ReleaseActivationName || !reflect.DeepEqual(reset.Data, ReleaseActivationBootstrapData()) {
		t.Fatalf("activation update = %s %v, want the bootstrap state", reset.Name, reset.Data)
	}
	options := fixture.recorder.options[teardownKey("ConfigMap", ReleaseActivationName)]
	if options.Preconditions == nil || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != reset.ResourceVersion {
		t.Fatalf("activation delete preconditions = %#v, want the reset's resource version %q", options.Preconditions, reset.ResourceVersion)
	}
	if _, present := fixture.configMaps.objects[ReleaseActivationName]; present {
		t.Fatal("the activation survived the teardown")
	}
}

// A refusal that never clears ends with the context, and the error says what
// the last attempt was told rather than only that time ran out.
func TestReleaseTeardownNamesThePersistentRefusalWhenItsTimeRunsOut(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t, false)
	activationKey := teardownKey("ConfigMap", ReleaseActivationName)
	fixture.recorder.errors[activationKey] = apierrors.NewForbidden(
		teardownGroupResource("ConfigMap"),
		ReleaseActivationName,
		errors.New("ValidatingAdmissionPolicy 'guard' with binding 'guard' denied request: "+releaseActivationGuardDenialMessage()),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := fixture.teardown.Teardown(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), releaseActivationGuardDenialMessage()) {
		t.Fatalf("Teardown error = %v, want the deadline together with the last refusal", err)
	}
}

func TestReleaseTeardownPreflightsCompleteInventoryBeforeMutation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*releaseTeardownFixture)
		want   string
	}{
		{
			name: "parameterized binding drift",
			mutate: func(f *releaseTeardownFixture) {
				name := RuntimeGuardPolicyName(f.guard.ReleaseSequence)
				f.bindings.objects[name].Spec.ParamRef = nil
			},
			want: "binding spec differs",
		},
		{
			name: "remaining binding foreign owner",
			mutate: func(f *releaseTeardownFixture) {
				name := HookIdentityGuardPolicyName(f.guard.ReleaseNamespace, f.guard.ReleaseName, f.guard.ReleaseSequence, f.guard.ManagerImage)
				f.bindings.objects[name].Labels[instanceLabel] = "foreign"
			},
			want: "foreign or incomplete ownership",
		},
		{
			name: "policy contract drift",
			mutate: func(f *releaseTeardownFixture) {
				name := ParentReplicaSetGuardPolicyName(f.guard.ReleaseNamespace, f.guard.ReleaseName, f.guard.ReleaseSequence, f.guard.ManagerImage)
				f.policies.objects[name].Spec.Validations[0].Expression = "true"
			},
			want: "differs from the immutable contract",
		},
		{
			name: "namespace boundary drift",
			mutate: func(f *releaseTeardownFixture) {
				name := NamespaceDeletionGuardPolicyName(f.guard.ReleaseNamespace, f.guard.ReleaseName)
				f.bindings.objects[name].Spec.ValidationActions = nil
			},
			want: "namespace deletion guard binding spec differs",
		},
		{
			name: "activation owner drift",
			mutate: func(f *releaseTeardownFixture) {
				f.configMaps.objects[ReleaseActivationName].Labels[instanceLabel] = "foreign"
			},
			want: "foreign or incomplete ownership",
		},
		{
			name: "probe marker shape drift",
			mutate: func(f *releaseTeardownFixture) {
				name := HookIdentityProbeObjectName(f.guard.ReleaseNamespace, f.guard.ReleaseName, f.guard.ReleaseSequence, f.guard.ManagerImage)
				f.configMaps.objects[name].Data["probe"] = "changed"
			},
			want: "shape is not exact",
		},
		{
			name: "readiness marker drift",
			mutate: func(f *releaseTeardownFixture) {
				name := ParentOriginReadyMarkerName(f.guard.ReleaseNamespace, f.guard.ReleaseName)
				f.configMaps.objects[name].Data = map[string]string{"foreign": "true"}
			},
			want: "differs from the exact stable contract",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newReleaseTeardownFixture(t, false)
			test.mutate(fixture)
			err := fixture.teardown.Teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Teardown error = %v, want %q", err, test.want)
			}
			if len(fixture.recorder.deletes) != 0 {
				t.Fatalf("preflight failure mutated resources: %v", fixture.recorder.deletes)
			}
		})
	}
}

func TestReleaseTeardownRejectsObjectsThatMayRemainAfterDelete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*releaseTeardownFixture)
		want   string
	}{
		{
			name: "finalizer",
			mutate: func(f *releaseTeardownFixture) {
				f.bindings.objects[RolloutGuardPolicyName(f.guard.ReleaseSequence)].Finalizers = []string{"operator.example/hold"}
			},
			want: "has finalizers",
		},
		{
			name: "deletion in progress",
			mutate: func(f *releaseTeardownFixture) {
				now := metav1.Now()
				f.policies.objects[RolloutGuardPolicyName(f.guard.ReleaseSequence)].DeletionTimestamp = &now
			},
			want: "deletion is already in progress",
		},
		{
			name: "nonzero deletion grace period",
			mutate: func(f *releaseTeardownFixture) {
				grace := int64(30)
				f.bindings.objects[RuntimeGuardPolicyName(f.guard.ReleaseSequence)].DeletionGracePeriodSeconds = &grace
			},
			want: "nonzero deletion grace period",
		},
		{
			name: "owner reference",
			mutate: func(f *releaseTeardownFixture) {
				name := NamespaceDeletionGuardPolicyName(f.guard.ReleaseNamespace, f.guard.ReleaseName)
				f.bindings.objects[name].OwnerReferences = []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "Namespace", Name: f.guard.ReleaseNamespace, UID: "owner-uid",
				}}
			},
			want: "unexpected owner references",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newReleaseTeardownFixture(t, false)
			test.mutate(fixture)

			err := fixture.teardown.Teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Teardown error = %v, want %q", err, test.want)
			}
			if len(fixture.recorder.deletes) != 0 {
				t.Fatalf("unsafe object state caused deletes: %v", fixture.recorder.deletes)
			}
		})
	}
}

func TestReleaseTeardownStopsAtFirstDeleteFailureAndResumes(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t, false)
	wantOrder := expectedReleaseTeardownOrder(fixture.guard)
	failureIndex := 8
	failureKey := wantOrder[failureIndex]
	fixture.recorder.errors[failureKey] = errors.New("injected delete failure")

	err := fixture.teardown.Teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "injected delete failure") {
		t.Fatalf("Teardown error = %v, want injected failure", err)
	}
	if want := wantOrder[:failureIndex+1]; !reflect.DeepEqual(fixture.recorder.deletes, want) {
		t.Fatalf("delete calls before failure = %v, want %v", fixture.recorder.deletes, want)
	}

	delete(fixture.recorder.errors, failureKey)
	fixture.recorder.deletes = nil
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("resume teardown: %v", err)
	}
	if want := wantOrder[failureIndex:]; !reflect.DeepEqual(fixture.recorder.deletes, want) {
		t.Fatalf("resumed delete calls = %v, want %v", fixture.recorder.deletes, want)
	}
}

// Whatever is already gone is skipped, wherever it sat in the order: an
// uninstall that stopped partway, or an object an administrator removed,
// leaves the rest for the next attempt to delete.
func TestReleaseTeardownDeletesWhateverRemains(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		removed func(order []string) []string
	}{
		{name: "a deleted prefix", removed: func(order []string) []string { return order[:6] }},
		{name: "a hole in the middle", removed: func(order []string) []string { return []string{order[5], order[20]} }},
		{name: "the activation parameter", removed: func([]string) []string {
			return []string{teardownKey("ConfigMap", ReleaseActivationName)}
		}},
		{name: "everything", removed: func(order []string) []string { return order }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newReleaseTeardownFixture(t, false)
			order := expectedReleaseTeardownOrder(fixture.guard)
			removed := map[string]bool{}
			for _, key := range test.removed(order) {
				fixture.remove(key)
				removed[key] = true
			}
			var want []string
			for _, key := range order {
				if !removed[key] {
					want = append(want, key)
				}
			}

			if err := fixture.teardown.Teardown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fixture.recorder.deletes, want) {
				t.Fatalf("delete calls = %v, want %v", fixture.recorder.deletes, want)
			}
			if remaining := fixture.remaining(); len(remaining) != 0 {
				t.Fatalf("teardown left %v behind", remaining)
			}

			fixture.recorder.deletes = nil
			if err := fixture.teardown.Teardown(context.Background()); err != nil {
				t.Fatalf("completed teardown retry: %v", err)
			}
			if len(fixture.recorder.deletes) != 0 {
				t.Fatalf("completed retry issued deletes: %v", fixture.recorder.deletes)
			}
		})
	}
}

func TestReleaseTeardownAcceptsDeleteNotFoundAfterVerification(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t, false)
	order := expectedReleaseTeardownOrder(fixture.guard)
	notFoundKey := order[4]
	fixture.recorder.notFound[notFoundKey] = true

	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.recorder.deletes, order) {
		t.Fatalf("delete calls = %v, want %v", fixture.recorder.deletes, order)
	}
}

// A precondition that no longer holds is read again: a replacement that
// still matches the contract is the release's own object and is deleted with
// its own identity, and one that does not match stops the teardown untouched.
func TestReleaseTeardownRereadsAReplacedObjectBeforeDeletingIt(t *testing.T) {
	t.Parallel()

	name := RuntimeGuardPolicyName(1)
	target := teardownKey("ValidatingAdmissionPolicyBinding", name)

	t.Run("a replacement that matches the contract", func(t *testing.T) {
		t.Parallel()
		fixture := newReleaseTeardownFixture(t, false)
		order := expectedReleaseTeardownOrder(fixture.guard)
		targetIndex := slicesIndex(order, target)
		replaced := false
		fixture.recorder.beforeDelete[target] = func() {
			if replaced {
				return
			}
			replaced = true
			fixture.bindings.objects[name].UID = "replacement-uid"
			fixture.bindings.objects[name].ResourceVersion = "replacement-version"
		}

		if err := fixture.teardown.Teardown(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := append([]string{}, order[:targetIndex+1]...)
		want = append(want, order[targetIndex:]...)
		if !reflect.DeepEqual(fixture.recorder.deletes, want) {
			t.Fatalf("delete calls = %v, want %v", fixture.recorder.deletes, want)
		}
		options := fixture.recorder.options[target]
		if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != types.UID("replacement-uid") {
			t.Fatalf("the replacement was deleted with preconditions %#v, want its own identity", options.Preconditions)
		}
	})

	t.Run("a replacement that does not", func(t *testing.T) {
		t.Parallel()
		fixture := newReleaseTeardownFixture(t, false)
		order := expectedReleaseTeardownOrder(fixture.guard)
		targetIndex := slicesIndex(order, target)
		fixture.recorder.beforeDelete[target] = func() {
			fixture.bindings.objects[name].UID = "replacement-uid"
			fixture.bindings.objects[name].ResourceVersion = "replacement-version"
			fixture.bindings.objects[name].Spec.ParamRef = nil
		}

		err := fixture.teardown.Teardown(context.Background())
		if err == nil || !strings.Contains(err.Error(), "binding spec differs") {
			t.Fatalf("Teardown error = %v, want the replacement refused", err)
		}
		if want := order[:targetIndex+1]; !reflect.DeepEqual(fixture.recorder.deletes, want) {
			t.Fatalf("delete calls = %v, want %v", fixture.recorder.deletes, want)
		}
		if fixture.bindings.objects[name] == nil {
			t.Fatal("a replacement outside the contract was deleted")
		}
	})
}

func TestReleaseTeardownValidatesDependencies(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t, false)
	stopped := *fixture.guard
	stopped.PollEvery = 0
	tests := []struct {
		name     string
		teardown *ReleaseTeardown
		want     string
	}{
		{name: "nil receiver", want: "clients and rollout identity are required"},
		{name: "nil rollout", teardown: NewReleaseTeardown(nil, fixture.policies, fixture.bindings, fixture.configMaps, fixture.secrets), want: "clients and rollout identity are required"},
		{name: "nil policies", teardown: NewReleaseTeardown(fixture.guard, nil, fixture.bindings, fixture.configMaps, fixture.secrets), want: "clients and rollout identity are required"},
		{name: "nil bindings", teardown: NewReleaseTeardown(fixture.guard, fixture.policies, nil, fixture.configMaps, fixture.secrets), want: "clients and rollout identity are required"},
		{name: "nil ConfigMaps", teardown: NewReleaseTeardown(fixture.guard, fixture.policies, fixture.bindings, nil, fixture.secrets), want: "clients and rollout identity are required"},
		{name: "nil Secrets", teardown: NewReleaseTeardown(fixture.guard, fixture.policies, fixture.bindings, fixture.configMaps, nil), want: "clients and rollout identity are required"},
		{name: "no poll interval", teardown: NewReleaseTeardown(&stopped, fixture.policies, fixture.bindings, fixture.configMaps, fixture.secrets), want: "poll interval must be positive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.teardown.Teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Teardown error = %v, want %q", err, test.want)
			}
		})
	}
}

type releaseTeardownFixture struct {
	guard      *RolloutGuard
	teardown   *ReleaseTeardown
	recorder   *teardownRecorder
	policies   *teardownPolicyClient
	bindings   *teardownBindingClient
	configMaps *teardownConfigMapClient
	secrets    *teardownSecretClient
	identities map[string]teardownIdentity
}

func newReleaseTeardownFixture(t *testing.T, certificateRuntime bool) *releaseTeardownFixture {
	t.Helper()
	guard, policySource, bindingSource, _ := readyRolloutGuard()
	guard.CertificateRuntimeEnabled = certificateRuntime
	policySource.objects = map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}
	bindingSource.objects = map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	installReleaseTeardownGuards(t, guard, policySource.objects, bindingSource.objects)

	recorder := &teardownRecorder{
		options:      map[string]metav1.DeleteOptions{},
		errors:       map[string]error{},
		notFound:     map[string]bool{},
		beforeDelete: map[string]func(){},
	}
	policies := &teardownPolicyClient{objects: policySource.objects, recorder: recorder}
	bindings := &teardownBindingClient{objects: bindingSource.objects, recorder: recorder}
	for name, object := range policies.objects {
		setTeardownObjectIdentity(object, "policy-"+name)
	}
	for name, object := range bindings.objects {
		setTeardownObjectIdentity(object, "binding-"+name)
	}

	probeName := HookIdentityProbeObjectName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	probePolicyName := HookIdentityProbeGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	probe := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: probeName, Namespace: guard.ReleaseNamespace,
			Annotations: map[string]string{
				"helm.sh/hook":                           "pre-install,pre-upgrade",
				"helm.sh/hook-weight":                    hookIdentityProbeMarkerWeight,
				"helm.sh/resource-policy":                "keep",
				"operator.ptah.run/hook-identity-policy": probePolicyName,
			},
			Labels: map[string]string{
				managedByLabel:                rolloutGuardManagedBy,
				instanceLabel:                 guard.ReleaseName,
				"app.kubernetes.io/component": "hook-identity-probe",
			},
		},
		Data: map[string]string{"probe": "ready-for-denial-proof"},
	}
	setTeardownObjectIdentity(probe, "probe-marker")
	readiness := NewParentWorkloadGuard(guard).readinessMarker()
	setTeardownObjectIdentity(readiness, "readiness-marker")
	activation := guard.ConfigMaps.(*rolloutConfigMapClient).objects[ReleaseActivationName].DeepCopy()
	setTeardownObjectIdentity(activation, "activation")
	admissionConvergenceMarker := NewAdmissionConvergenceGuard(guard).unsealedMarker()
	setTeardownObjectIdentity(admissionConvergenceMarker, "admission-convergence-marker")
	configMaps := &teardownConfigMapClient{objects: map[string]*corev1.ConfigMap{
		probeName:             probe,
		readiness.Name:        readiness,
		ReleaseActivationName: activation,
		AdmissionConvergenceMarkerName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence): admissionConvergenceMarker,
	}, recorder: recorder}
	secrets := &teardownSecretClient{present: certificateRuntime, recorder: recorder}
	guard.Policies = policies
	guard.Bindings = bindings

	fixture := &releaseTeardownFixture{
		guard: guard, recorder: recorder,
		policies: policies, bindings: bindings, configMaps: configMaps, secrets: secrets,
		identities: map[string]teardownIdentity{},
	}
	fixture.teardown = NewReleaseTeardown(guard, policies, bindings, configMaps, secrets)
	for _, key := range expectedReleaseTeardownOrder(guard) {
		fixture.identities[key] = fixture.identity(key)
	}
	return fixture
}

// installReleaseTeardownGuards stores every guard pair the release keeps, in
// the form its builder compiles for this guard's settings.
func installReleaseTeardownGuards(
	t *testing.T,
	guard *RolloutGuard,
	policies map[string]*admissionregistrationv1.ValidatingAdmissionPolicy,
	bindings map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding,
) {
	t.Helper()
	install := func(policy *admissionregistrationv1.ValidatingAdmissionPolicy, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
		policies[policy.Name] = readyPolicy(policy)
		bindings[binding.Name] = binding
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	rolloutName := RolloutGuardPolicyName(guard.ReleaseSequence)
	runtimeName := RuntimeGuardPolicyName(guard.ReleaseSequence)
	install(guard.policy(guard.ControllerStateVersion, guard.AdmissionContractVersion), guard.binding(rolloutName))
	install(guard.runtimePolicy(guard.ControllerStateVersion, guard.ReleaseSequence, guard.ManagerImage), guard.binding(runtimeName))
	runtimePodPolicy, err := guard.runtimePodIdentityPolicy()
	must(err)
	runtimePodBinding, err := guard.runtimePodIdentityBinding()
	must(err)
	install(runtimePodPolicy, runtimePodBinding)
	hookName := HookIdentityGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	install(guard.hookIdentityPolicy(), guard.binding(hookName))
	hookProbeName := HookIdentityProbeGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	install(guard.hookIdentityProbePolicy(), guard.binding(hookProbeName))
	activation := guard.releaseActivationGuard()
	install(activation.policy(), activation.binding())
	namespaceGuard := NewNamespaceDeletionGuard(guard)
	install(namespaceGuard.policy(), namespaceGuard.binding())
	controllerWrite := NewControllerWriteGuard(guard)
	install(controllerWrite.policy(), controllerWrite.binding())
	certificateWrite := NewCertificateWriteGuard(guard)
	for _, entry := range certificateWrite.entries() {
		install(certificateWrite.policy(entry), certificateWrite.binding(entry))
	}
	controllerObjects := NewControllerObjectGuard(guard)
	for _, entry := range controllerObjects.entries() {
		install(controllerObjects.policy(entry), controllerObjects.binding(entry))
	}
	for _, entry := range NewParentWorkloadGuard(guard).entries() {
		install(entry.policy, entry.binding)
	}
	if guard.CertificateRuntimeEnabled {
		stagingPolicy, stagingBinding, err := NewStagingSecretGuard(guard).ExpectedObjects()
		must(err)
		install(stagingPolicy, stagingBinding)
	}
}

func expectedReleaseTeardownOrder(guard *RolloutGuard) []string {
	activationName := ReleaseActivationGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	rolloutName := RolloutGuardPolicyName(guard.ReleaseSequence)
	runtimeName := RuntimeGuardPolicyName(guard.ReleaseSequence)
	runtimePodName := RuntimePodGuardPolicyName(guard.ReleaseSequence)
	hookName := HookIdentityGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	hookProbeName := HookIdentityProbeGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	parentReplicaSetName := ParentReplicaSetGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	parentHookOriginName := ParentHookJobOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	parentHookPodOriginName := ParentHookPodOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	parentHookContractName := ParentHookJobContractPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	controllerWriteName := ControllerWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	controllerJobWriteName := ControllerJobWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	controllerChunkWriteName := ControllerChunkWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	controllerPlanWriteName := ControllerPlanWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	controllerMigrationPlanWriteName := ControllerMigrationPlanWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	certificateMutatingWriteName := CertificateMutatingWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	certificateValidatingWriteName := CertificateValidatingWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	namespaceName := NamespaceDeletionGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	stagingName := StagingSecretGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)

	parameterized := []string{
		rolloutName, runtimeName, runtimePodName,
		controllerWriteName,
		controllerJobWriteName, controllerChunkWriteName, controllerPlanWriteName,
		controllerMigrationPlanWriteName,
	}
	remaining := []string{
		hookName, hookProbeName,
		parentReplicaSetName, parentHookOriginName, parentHookPodOriginName, parentHookContractName,
		certificateMutatingWriteName, certificateValidatingWriteName,
		namespaceName,
	}
	if guard.CertificateRuntimeEnabled {
		remaining = append(remaining, stagingName)
	}
	policies := []string{
		rolloutName, runtimeName, runtimePodName,
		hookName, hookProbeName,
		parentReplicaSetName, parentHookOriginName, parentHookPodOriginName, parentHookContractName,
		controllerWriteName,
		controllerJobWriteName, controllerChunkWriteName, controllerPlanWriteName,
		controllerMigrationPlanWriteName,
		certificateMutatingWriteName, certificateValidatingWriteName,
		namespaceName,
	}
	if guard.CertificateRuntimeEnabled {
		policies = append(policies, stagingName)
	}

	var order []string
	for _, name := range parameterized {
		order = append(order, teardownKey("ValidatingAdmissionPolicyBinding", name))
	}
	for _, name := range remaining {
		order = append(order, teardownKey("ValidatingAdmissionPolicyBinding", name))
	}
	for _, name := range policies {
		order = append(order, teardownKey("ValidatingAdmissionPolicy", name))
	}
	if guard.CertificateRuntimeEnabled {
		order = append(order, teardownKey("Secret", "ptah-webhook-cert-stage"))
	}
	order = append(order,
		teardownKey("ConfigMap", HookIdentityProbeObjectName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)),
		teardownKey("ConfigMap", ParentOriginReadyMarkerName(guard.ReleaseNamespace, guard.ReleaseName)),
		teardownKey("ValidatingAdmissionPolicyBinding", activationName),
		teardownKey("ValidatingAdmissionPolicy", activationName),
		teardownKey("ConfigMap", AdmissionConvergenceMarkerName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence)),
		teardownKey("ConfigMap", ReleaseActivationName),
	)
	return order
}

func (f *releaseTeardownFixture) identity(key string) teardownIdentity {
	kind, name := splitTeardownKey(key)
	switch kind {
	case "ValidatingAdmissionPolicyBinding":
		return objectTeardownIdentity(f.bindings.objects[name])
	case "ValidatingAdmissionPolicy":
		return objectTeardownIdentity(f.policies.objects[name])
	case "ConfigMap":
		return objectTeardownIdentity(f.configMaps.objects[name])
	case "Secret":
		return teardownIdentity{}
	default:
		panic("unknown teardown kind " + kind)
	}
}

func (f *releaseTeardownFixture) remove(key string) {
	kind, name := splitTeardownKey(key)
	switch kind {
	case "ValidatingAdmissionPolicyBinding":
		delete(f.bindings.objects, name)
	case "ValidatingAdmissionPolicy":
		delete(f.policies.objects, name)
	case "ConfigMap":
		delete(f.configMaps.objects, name)
	case "Secret":
		f.secrets.present = false
	default:
		panic("unknown teardown kind " + kind)
	}
}

// remaining names every object of the inventory the fake API still holds.
func (f *releaseTeardownFixture) remaining() []string {
	var names []string
	for name := range f.bindings.objects {
		names = append(names, teardownKey("ValidatingAdmissionPolicyBinding", name))
	}
	for name := range f.policies.objects {
		names = append(names, teardownKey("ValidatingAdmissionPolicy", name))
	}
	for name := range f.configMaps.objects {
		names = append(names, teardownKey("ConfigMap", name))
	}
	if f.secrets.present {
		names = append(names, teardownKey("Secret", "ptah-webhook-cert-stage"))
	}
	return names
}

func setTeardownObjectIdentity(object metav1.Object, value string) {
	object.SetUID(types.UID("uid-" + value))
	object.SetResourceVersion("rv-" + value)
}

func objectTeardownIdentity(object metav1.Object) teardownIdentity {
	return teardownIdentity{uid: object.GetUID(), resourceVersion: object.GetResourceVersion()}
}

func teardownKey(kind, name string) string {
	return kind + "/" + name
}

func splitTeardownKey(key string) (string, string) {
	kind, name, found := strings.Cut(key, "/")
	if !found {
		panic("invalid teardown key " + key)
	}
	return kind, name
}

func slicesIndex(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	panic("missing teardown key " + target)
}

type teardownRecorder struct {
	deletes      []string
	options      map[string]metav1.DeleteOptions
	errors       map[string]error
	notFound     map[string]bool
	beforeDelete map[string]func()
}

// record logs one delete call and answers it as the API server would: an
// injected error, a NotFound, or a precondition check against the object the
// fake still holds. A delete with no preconditions is accepted as it stands.
func (r *teardownRecorder) record(kind, name string, options metav1.DeleteOptions, object func() metav1.Object) error {
	key := teardownKey(kind, name)
	r.deletes = append(r.deletes, key)
	r.options[key] = options
	if before := r.beforeDelete[key]; before != nil {
		before()
	}
	if err := r.errors[key]; err != nil {
		return err
	}
	current := object()
	if r.notFound[key] || current == nil {
		return apierrors.NewNotFound(teardownGroupResource(kind), name)
	}
	if options.Preconditions == nil {
		return nil
	}
	if options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil ||
		*options.Preconditions.UID != current.GetUID() || *options.Preconditions.ResourceVersion != current.GetResourceVersion() {
		return apierrors.NewConflict(teardownGroupResource(kind), name, errors.New("deletion precondition failed"))
	}
	return nil
}

func teardownGroupResource(kind string) schema.GroupResource {
	switch kind {
	case "ValidatingAdmissionPolicyBinding":
		return schema.GroupResource{Group: admissionregistrationv1.GroupName, Resource: "validatingadmissionpolicybindings"}
	case "ValidatingAdmissionPolicy":
		return schema.GroupResource{Group: admissionregistrationv1.GroupName, Resource: "validatingadmissionpolicies"}
	case "ConfigMap":
		return schema.GroupResource{Resource: "configmaps"}
	case "Secret":
		return schema.GroupResource{Resource: "secrets"}
	default:
		panic(fmt.Sprintf("unknown teardown kind %q", kind))
	}
}

type teardownPolicyClient struct {
	objects  map[string]*admissionregistrationv1.ValidatingAdmissionPolicy
	recorder *teardownRecorder
}

func (c *teardownPolicyClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(teardownGroupResource("ValidatingAdmissionPolicy"), name)
	}
	return object.DeepCopy(), nil
}

func (c *teardownPolicyClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	err := c.recorder.record("ValidatingAdmissionPolicy", name, options, func() metav1.Object {
		if object := c.objects[name]; object != nil {
			return object
		}
		return nil
	})
	if err == nil || apierrors.IsNotFound(err) {
		delete(c.objects, name)
	}
	return err
}

type teardownBindingClient struct {
	objects  map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding
	recorder *teardownRecorder
}

func (c *teardownBindingClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(teardownGroupResource("ValidatingAdmissionPolicyBinding"), name)
	}
	return object.DeepCopy(), nil
}

func (c *teardownBindingClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	err := c.recorder.record("ValidatingAdmissionPolicyBinding", name, options, func() metav1.Object {
		if object := c.objects[name]; object != nil {
			return object
		}
		return nil
	})
	if err == nil || apierrors.IsNotFound(err) {
		delete(c.objects, name)
	}
	return err
}

type teardownConfigMapClient struct {
	objects  map[string]*corev1.ConfigMap
	recorder *teardownRecorder
	updates  []*corev1.ConfigMap
}

// Update stores the object under a new resource version when the one it was
// read at still holds, as the API server does.
func (c *teardownConfigMapClient) Update(_ context.Context, object *corev1.ConfigMap, _ metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	current := c.objects[object.Name]
	if current == nil {
		return nil, apierrors.NewNotFound(teardownGroupResource("ConfigMap"), object.Name)
	}
	if object.ResourceVersion != current.ResourceVersion {
		return nil, apierrors.NewConflict(teardownGroupResource("ConfigMap"), object.Name, errors.New("resource version changed"))
	}
	stored := object.DeepCopy()
	stored.ResourceVersion = current.ResourceVersion + "-updated"
	c.objects[object.Name] = stored
	c.updates = append(c.updates, stored.DeepCopy())
	return stored.DeepCopy(), nil
}

func (c *teardownConfigMapClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(teardownGroupResource("ConfigMap"), name)
	}
	return object.DeepCopy(), nil
}

func (c *teardownConfigMapClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	err := c.recorder.record("ConfigMap", name, options, func() metav1.Object {
		if object := c.objects[name]; object != nil {
			return object
		}
		return nil
	})
	if err == nil || apierrors.IsNotFound(err) {
		delete(c.objects, name)
	}
	return err
}

// teardownSecretClient can only delete, as the Secret client the teardown is
// given can: the staging Secret is never read.
type teardownSecretClient struct {
	present  bool
	recorder *teardownRecorder
}

func (c *teardownSecretClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	err := c.recorder.record("Secret", name, options, func() metav1.Object {
		if c.present {
			return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name}}
		}
		return nil
	})
	if err == nil || apierrors.IsNotFound(err) {
		c.present = false
	}
	return err
}

var (
	_ ValidatingAdmissionPolicyTeardownClient        = (*teardownPolicyClient)(nil)
	_ ValidatingAdmissionPolicyBindingTeardownClient = (*teardownBindingClient)(nil)
	_ ConfigMapTeardownClient                        = (*teardownConfigMapClient)(nil)
	_ SecretTeardownClient                           = (*teardownSecretClient)(nil)
)
