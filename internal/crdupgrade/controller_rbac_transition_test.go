package crdupgrade

// These tests intentionally use the package under test because the security
// boundary is the unexported cursor/contract state machine, not another public
// API that could expose mutable RBAC contract construction to callers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestControllerRBACTransitionLegacyCutPointsAndRetries(t *testing.T) {
	t.Parallel()
	for initialCursor := 0; initialCursor <= 2; initialCursor++ {
		initialCursor := initialCursor
		t.Run(fmt.Sprintf("cursor-%d", initialCursor), func(t *testing.T) {
			t.Parallel()
			fixture := newControllerRBACTransitionFixture(t, initialCursor)
			if err := fixture.transition.Preflight(context.Background()); err != nil {
				t.Fatalf("Preflight() error = %v", err)
			}
			if len(fixture.client.patchCalls) != 0 {
				t.Fatalf("preflight made patch calls: %#v", fixture.client.patchCalls)
			}
			if err := fixture.transition.Transition(context.Background()); err != nil {
				t.Fatalf("Transition() error = %v", err)
			}
			if err := fixture.transition.VerifyComplete(context.Background()); err != nil {
				t.Fatalf("VerifyComplete() error = %v", err)
			}
			wantTargets := []string{}
			for _, contract := range fixture.transition.contract.bindings[initialCursor:] {
				key := controllerRBACBindingKey(contract)
				wantTargets = append(wantTargets, key, key)
			}
			gotTargets := make([]string, 0, len(fixture.client.patchCalls))
			for index, call := range fixture.client.patchCalls {
				gotTargets = append(gotTargets, call.key)
				if call.dryRun != (index%2 == 0) {
					t.Errorf("patch call %d dryRun = %t, want alternating dry/persist", index, call.dryRun)
				}
				assertControllerRBACJSONPatch(t, call.patch, fixture.guard, fixture.previousSubject(), fixture.candidateSubject())
			}
			if !reflect.DeepEqual(gotTargets, wantTargets) {
				t.Fatalf("patch targets = %#v, want %#v", gotTargets, wantTargets)
			}

			// A new hook process resumes an already complete transition without
			// rolling any binding back or issuing another patch.
			retry, err := NewControllerRBACTransition(fixture.guard, fixture.runtimeContract, fixture.client)
			if err != nil {
				t.Fatal(err)
			}
			if err := retry.Preflight(context.Background()); err != nil {
				t.Fatalf("retry Preflight() error = %v", err)
			}
			calls := len(fixture.client.patchCalls)
			if err := retry.Transition(context.Background()); err != nil {
				t.Fatalf("retry Transition() error = %v", err)
			}
			if len(fixture.client.patchCalls) != calls {
				t.Fatalf("complete retry made %d additional patch calls", len(fixture.client.patchCalls)-calls)
			}
		})
	}
}

func TestControllerRBACTransitionRequiresPreflightAndStableIdentity(t *testing.T) {
	t.Parallel()
	fixture := newControllerRBACTransitionFixture(t, 0)
	if err := fixture.transition.Transition(context.Background()); err == nil || !strings.Contains(err.Error(), "preflight has not completed") {
		t.Fatalf("Transition() error = %v, want preflight refusal", err)
	}
	if err := fixture.transition.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.client.clusterRoles[fixture.guard.ControllerDeploymentName].ResourceVersion = "changed-after-grace"
	if err := fixture.transition.Transition(context.Background()); err == nil || !strings.Contains(err.Error(), "identity changed after preflight") {
		t.Fatalf("Transition() error = %v, want role resourceVersion refusal", err)
	}
}

func TestControllerRBACTransitionAcceptsSingleSubjectWithNilFixedSubjects(t *testing.T) {
	t.Parallel()
	fixture := newControllerRBACTransitionFixture(t, 0)
	contract := fixture.transition.contract.bindings[0]
	if contract.fixedSubjects != nil {
		t.Fatalf("test contract fixed subjects = %#v, want nil", contract.fixedSubjects)
	}
	subjects := []rbacv1.Subject{fixture.previousSubject()}
	if _, err := fixture.transition.bindingState(contract, contract.roleRef, subjects, "uid", "1"); err != nil {
		t.Fatalf("bindingState() error = %v for exact one-subject binding", err)
	}
}

func TestControllerRBACTransitionCredentialGraceDecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		state   ReleaseActivationState
		mutate  func(*ControllerRBACTransition)
		pods    bool
		want    bool
		wantErr string
	}{
		{
			name:  "pristine managed bootstrap",
			state: ReleaseActivationState{ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsActive},
			want:  false,
		},
		{
			name:  "protected candidate Pod remains",
			state: ReleaseActivationState{ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsActive},
			pods:  true,
			want:  true,
		},
		{
			name:  "candidate ServiceAccount exists",
			state: ReleaseActivationState{ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsActive},
			mutate: func(transition *ControllerRBACTransition) {
				transition.preflight.serviceAccountUIDs[privilegeServiceAccountKey(transition.rollout.ReleaseNamespace, transition.rollout.ControllerServiceAccountName)] = "candidate"
			},
			want: true,
		},
		{
			name:  "candidate grant exists",
			state: ReleaseActivationState{ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsActive},
			mutate: func(transition *ControllerRBACTransition) {
				transition.preflight.bindingUIDs["grant"] = "binding"
			},
			want: true,
		},
		{
			name:  "predecessor declared",
			state: ReleaseActivationState{ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsActive},
			mutate: func(transition *ControllerRBACTransition) {
				transition.rollout.PreviousControllerServiceAccountName = "previous"
			},
			want: true,
		},
		{
			name:  "external candidate",
			state: ReleaseActivationState{ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsActive},
			mutate: func(transition *ControllerRBACTransition) {
				transition.rollout.ControllerServiceAccountManaged = false
			},
			want: true,
		},
		{
			name:  "activated retry",
			state: ReleaseActivationState{ActiveReleaseSequence: 1, ControllerCredentialPhase: ControllerCredentialsActive},
			want:  true,
		},
		{
			name: "exact draining retry",
			state: ReleaseActivationState{
				ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsDraining,
				DrainTargetReleaseSequence: 1,
			},
			mutate: func(transition *ControllerRBACTransition) {
				// Filled below from the immutable candidate identity.
			},
			want: true,
		},
		{
			name: "same DNS digest prefix but different full attempt",
			state: ReleaseActivationState{
				ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsDraining,
				DrainTargetReleaseSequence: 1,
			},
			wantErr: "differs from the candidate attempt",
		},
		{
			name: "ambiguous active tuple",
			state: ReleaseActivationState{
				ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsActive,
				DrainTargetReleaseSequence: 1,
			},
			wantErr: "contains drain identity",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newControllerRBACTransitionFixture(t, 0)
			transition := fixture.transition
			transition.rollout.PreviousControllerServiceAccountName = ""
			transition.rollout.PreviousControllerServiceAccountUID = ""
			transition.preflight = &controllerRBACTransitionState{
				bindingUIDs:        map[string]types.UID{},
				serviceAccountUIDs: map[string]types.UID{},
			}
			if test.mutate != nil {
				test.mutate(transition)
			}
			state := test.state
			if state.ControllerCredentialPhase == ControllerCredentialsDraining {
				attempt := hookIdentityDigest(transition.rollout.ReleaseNamespace, transition.rollout.ReleaseName, transition.rollout.ReleaseSequence, transition.rollout.ManagerImage)
				if test.wantErr != "" {
					last := byte('0')
					if attempt[len(attempt)-1] == last {
						last = '1'
					}
					attempt = attempt[:len(attempt)-1] + string(last)
				}
				state.DrainAttempt = attempt
			}
			got, err := transition.RequiresCredentialGrace(state, test.pods)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("RequiresCredentialGrace() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("RequiresCredentialGrace() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("RequiresCredentialGrace() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestControllerRBACTransitionCredentialGraceRequiresPreflight(t *testing.T) {
	t.Parallel()
	fixture := newControllerRBACTransitionFixture(t, 0)
	_, err := fixture.transition.RequiresCredentialGrace(ReleaseActivationState{
		ControllerCredentialPhase: ControllerCredentialsActive,
	}, false)
	if err == nil || !strings.Contains(err.Error(), "preflight has not completed") {
		t.Fatalf("RequiresCredentialGrace() error = %v, want preflight refusal", err)
	}
}

func TestControllerRBACTransitionRejectsPaddedPredecessorName(t *testing.T) {
	t.Parallel()
	fixture := newControllerRBACTransitionFixture(t, 0)
	fixture.guard.PreviousControllerServiceAccountName += " "
	_, err := NewControllerRBACTransition(fixture.guard, fixture.runtimeContract, fixture.client)
	if err == nil || !strings.Contains(err.Error(), "predecessor ServiceAccount name is padded") {
		t.Fatalf("NewControllerRBACTransition() error = %v, want padded predecessor refusal", err)
	}
}

func TestControllerRBACTransitionSnapshotsConstructorIdentity(t *testing.T) {
	t.Parallel()
	fixture := newControllerRBACTransitionFixture(t, 0)
	fixture.guard.PreviousControllerServiceAccountName = ""
	fixture.guard.PreviousControllerServiceAccountUID = ""
	fixture.guard.ReleaseSequence = 2
	if !fixture.transition.HasPredecessor() {
		t.Fatal("transition identity changed after caller mutated the source RolloutGuard")
	}
	if err := fixture.transition.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight() with immutable constructor snapshot error = %v", err)
	}
}

func TestControllerRBACTransitionRejectsUnfrozenFuturePredecessor(t *testing.T) {
	t.Parallel()
	fixture := newControllerRBACTransitionFixture(t, 0)
	fixture.guard.ReleaseSequence = 2
	fixture.guard.PreviousControllerReleaseSequence = 1
	fixture.guard.HookServiceAccountName = "ptah-e2e-operator-crd-v2-0123456789ab"
	_, err := NewControllerRBACTransition(fixture.guard, fixture.runtimeContract, fixture.client)
	if err == nil || !strings.Contains(err.Error(), "requires an explicit frozen predecessor role contract") {
		t.Fatalf("NewControllerRBACTransition() error = %v, want unfrozen predecessor refusal", err)
	}
}

func TestControllerRBACTransitionLostResponseResumesForward(t *testing.T) {
	t.Parallel()
	fixture := newControllerRBACTransitionFixture(t, 0)
	fixture.client.persistError = errors.New("response stream reset")
	fixture.client.persistBeforeError = true
	if err := fixture.transition.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := fixture.transition.Transition(context.Background()); err != nil {
		t.Fatalf("Transition() after lost responses error = %v", err)
	}
	if got, want := len(fixture.client.patchCalls), 4; got != want {
		t.Fatalf("patch calls = %d, want %d dry/persist calls", got, want)
	}
}

func TestControllerRBACTransitionRejectsFailedOrMutatingDryRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*fakeControllerRBACClient)
		want   string
	}{
		{
			name: "dry-run error",
			mutate: func(client *fakeControllerRBACClient) {
				client.dryRunError = errors.New("denied")
			},
			want: "dry-run controller RBAC transition",
		},
		{
			name: "dry-run returned nil",
			mutate: func(client *fakeControllerRBACClient) {
				client.nilDryRunResult = true
			},
			want: "returned a nil object",
		},
		{
			name: "dry-run mutated storage",
			mutate: func(client *fakeControllerRBACClient) {
				client.mutateDuringDryRun = true
			},
			want: "cursor changed",
		},
		{
			name: "role changed after dry-run",
			mutate: func(client *fakeControllerRBACClient) {
				client.afterDryRun = func() {
					client.clusterRoles["ptah-e2e-operator"].ResourceVersion = "changed-during-dry-run"
				}
			},
			want: "identity changed during dry-run",
		},
		{
			name: "persistent conflict without mutation",
			mutate: func(client *fakeControllerRBACClient) {
				client.persistError = apierrors.NewConflict(
					schema.GroupResource{Group: rbacv1.GroupName, Resource: "clusterrolebindings"},
					"ptah-e2e-operator",
					errors.New("changed"),
				)
			},
			want: "persist controller RBAC transition",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newControllerRBACTransitionFixture(t, 0)
			test.mutate(fixture.client)
			if err := fixture.transition.Preflight(context.Background()); err != nil {
				t.Fatal(err)
			}
			err := fixture.transition.Transition(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Transition() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestControllerRBACTransitionRejectsInvalidBindingStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*controllerRBACTransitionFixture)
		want   string
	}{
		{
			name: "non-prefix permutation",
			mutate: func(f *controllerRBACTransitionFixture) {
				f.setBindingSubject(f.transition.contract.bindings[0], f.previousSubject())
				f.setBindingSubject(f.transition.contract.bindings[1], f.candidateSubject())
			},
			want: "valid candidate prefix",
		},
		{
			name: "missing target",
			mutate: func(f *controllerRBACTransitionFixture) {
				delete(f.client.clusterBindings, f.guard.ControllerDeploymentName)
			},
			want: "required predecessor ClusterRoleBinding",
		},
		{
			name: "multiple subjects",
			mutate: func(f *controllerRBACTransitionFixture) {
				binding := f.client.clusterBindings[f.guard.ControllerDeploymentName]
				binding.Subjects = append(binding.Subjects, f.candidateSubject())
			},
			want: "exactly one subject",
		},
		{
			name: "wrong roleRef",
			mutate: func(f *controllerRBACTransitionFixture) {
				f.client.clusterBindings[f.guard.ControllerDeploymentName].RoleRef.Name = "foreign"
			},
			want: "unexpected roleRef",
		},
		{
			name: "deleting target",
			mutate: func(f *controllerRBACTransitionFixture) {
				now := metav1.Now()
				f.client.clusterBindings[f.guard.ControllerDeploymentName].DeletionTimestamp = &now
			},
			want: "deleting Helm ownership",
		},
		{
			name: "foreign ownership",
			mutate: func(f *controllerRBACTransitionFixture) {
				f.client.clusterBindings[f.guard.ControllerDeploymentName].Labels[instanceLabel] = "foreign"
			},
			want: "foreign, incomplete, or deleting",
		},
		{
			name: "wrong role rules",
			mutate: func(f *controllerRBACTransitionFixture) {
				f.client.clusterRoles[f.guard.ControllerDeploymentName].Rules[0].Verbs = append(
					f.client.clusterRoles[f.guard.ControllerDeploymentName].Rules[0].Verbs,
					"delete",
				)
			},
			want: "rules differ",
		},
		{
			name: "foreign protected binding",
			mutate: func(f *controllerRBACTransitionFixture) {
				f.client.roleBindings["foreign\x00grant"] = &rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: "grant", Namespace: "foreign"},
					Subjects:   []rbacv1.Subject{f.previousSubject()},
				}
			},
			want: "foreign RoleBinding",
		},
		{
			name: "predecessor UID reused",
			mutate: func(f *controllerRBACTransitionFixture) {
				f.client.serviceAccounts[f.guard.PreviousControllerServiceAccountName].UID = "replacement"
			},
			want: "UID changed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newControllerRBACTransitionFixture(t, 0)
			test.mutate(fixture)
			err := fixture.transition.Preflight(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Preflight() error = %v, want containing %q", err, test.want)
			}
			if len(fixture.client.patchCalls) != 0 {
				t.Fatalf("invalid preflight made patch calls: %#v", fixture.client.patchCalls)
			}
		})
	}
}

func TestControllerRBACTransitionFutureTargetSetSupportsEveryPrefix(t *testing.T) {
	t.Parallel()
	for cursor := 0; cursor <= 3; cursor++ {
		cursor := cursor
		t.Run(strconv.Itoa(cursor), func(t *testing.T) {
			t.Parallel()
			fixture := newControllerRBACTransitionFixture(t, 0)
			fixture.guard.ReleaseSequence = 2
			fixture.guard.PreviousControllerReleaseSequence = 1
			fixture.guard.HookServiceAccountName = "ptah-e2e-operator-crd-v2-0123456789ab"
			// This white-box test exercises the already-structured three-target
			// state machine. The public constructor still rejects this sequence
			// until its predecessor role contract is frozen.
			fixture.transition.rollout = cloneControllerRBACRollout(fixture.guard)
			runtime := controllerRBACBindingContract{
				name:      fixture.guard.ControllerDeploymentName + "-runtime-admission",
				namespace: fixture.guard.ReleaseNamespace,
				roleRef:   controllerRBACRoleRef("Role", fixture.guard.ControllerDeploymentName+"-runtime-admission"),
				fixedSubjects: []rbacv1.Subject{
					controllerRBACServiceAccountSubject(fixture.guard.ReleaseNamespace, fixture.runtimeContract.CertificateServiceAccountName),
				},
			}
			fixture.transition.contract.bindings = []controllerRBACBindingContract{
				fixture.transition.contract.bindings[0],
				runtime,
				fixture.transition.contract.bindings[1],
			}
			fixture.transition.contract.postApplyBinding = nil
			fixture.transition.contract.postApplyRole = nil
			runtimeRole := controllerRBACRoleContract{
				name:             runtime.name,
				namespace:        runtime.namespace,
				predecessorRules: currentControllerRuntimeRoleRules(fixture.guard, fixture.runtimeContract),
				candidateRules:   currentControllerRuntimeRoleRules(fixture.guard, fixture.runtimeContract),
			}
			fixture.transition.contract.roles = append(fixture.transition.contract.roles, runtimeRole)
			fixture.client.roleBindings[privilegeBindingKey(runtime.namespace, runtime.name)] = controllerRBACRoleBinding(
				fixture.guard,
				runtime,
				fixture.previousSubject(),
				"binding-runtime",
				"13",
			)
			fixture.client.roles[privilegeBindingKey(runtime.namespace, runtime.name)] = &rbacv1.Role{
				ObjectMeta: controllerRBACObjectMeta(fixture.guard, runtime.name, runtime.namespace, "role-runtime", "8"),
				Rules:      append([]rbacv1.PolicyRule(nil), runtimeRole.predecessorRules...),
			}
			for index, contract := range fixture.transition.contract.bindings {
				if index < cursor {
					fixture.setBindingSubject(contract, fixture.candidateSubject())
				} else {
					fixture.setBindingSubject(contract, fixture.previousSubject())
				}
			}
			if err := fixture.transition.Preflight(context.Background()); err != nil {
				t.Fatalf("future Preflight() error = %v", err)
			}
			if err := fixture.transition.Transition(context.Background()); err != nil {
				t.Fatalf("future Transition() error = %v", err)
			}
			if got, want := len(fixture.client.patchCalls), 2*(3-cursor); got != want {
				t.Fatalf("patch calls = %d, want %d", got, want)
			}
		})
	}
}

func TestControllerRBACTransitionFreshInstallDoesNotCreateBindings(t *testing.T) {
	t.Parallel()
	fixture := newControllerRBACTransitionFixture(t, 0)
	fixture.guard.PreviousControllerServiceAccountName = ""
	fixture.guard.PreviousControllerServiceAccountUID = ""
	fixture.guard.PreviousControllerReleaseSequence = 0
	delete(fixture.client.serviceAccounts, "legacy-controller")
	fixture.client.roleBindings = map[string]*rbacv1.RoleBinding{}
	fixture.client.clusterBindings = map[string]*rbacv1.ClusterRoleBinding{}
	fixture.client.roles = map[string]*rbacv1.Role{}
	fixture.client.clusterRoles = map[string]*rbacv1.ClusterRole{}
	transition, err := NewControllerRBACTransition(fixture.guard, fixture.runtimeContract, fixture.client)
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Transition(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fixture.client.patchCalls) != 0 {
		t.Fatalf("fresh install made patch calls: %#v", fixture.client.patchCalls)
	}
}

func TestControllerRBACTransitionExternalCandidateMustRemainExact(t *testing.T) {
	t.Parallel()
	fixture := newControllerRBACTransitionFixture(t, 0)
	fixture.guard.ControllerServiceAccountManaged = false
	fixture.client.serviceAccounts[fixture.guard.ControllerServiceAccountName] = controllerRBACServiceAccount(
		fixture.guard,
		fixture.guard.ControllerServiceAccountName,
		"external-candidate-uid",
		"21",
		false,
	)
	transition, err := NewControllerRBACTransition(fixture.guard, fixture.runtimeContract, fixture.client)
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.client.serviceAccounts[fixture.guard.ControllerServiceAccountName].ResourceVersion = "22"
	if err := transition.Transition(context.Background()); err == nil || !strings.Contains(err.Error(), "identity changed after preflight") {
		t.Fatalf("Transition() error = %v, want external candidate identity refusal", err)
	}
}

func TestControllerRBACTransitionAcceptsExactSameCandidateRetryStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*controllerRBACTransitionFixture)
	}{
		{
			name: "post-hook before ordinary apply",
			mutate: func(f *controllerRBACTransitionFixture) {
				// The predecessor may still exist and stable roles may still carry
				// their frozen rules after the hook moved both core bindings.
			},
		},
		{
			name: "ordinary apply replaced every normal resource",
			mutate: func(f *controllerRBACTransitionFixture) {
				delete(f.client.serviceAccounts, f.guard.PreviousControllerServiceAccountName)
				f.client.serviceAccounts[f.guard.ControllerServiceAccountName] = controllerRBACServiceAccount(
					f.guard,
					f.guard.ControllerServiceAccountName,
					"candidate-uid",
					"31",
					true,
				)
				f.installExactCandidateRolesAndRuntimeBinding()
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newControllerRBACTransitionFixture(t, 2)
			test.mutate(fixture)
			retry, err := NewControllerRBACTransition(fixture.guard, fixture.runtimeContract, fixture.client)
			if err != nil {
				t.Fatal(err)
			}
			if err := retry.Preflight(context.Background()); err != nil {
				t.Fatalf("Preflight() error = %v", err)
			}
			if err := retry.Transition(context.Background()); err != nil {
				t.Fatalf("Transition() error = %v", err)
			}
			if err := retry.VerifyComplete(context.Background()); err != nil {
				t.Fatalf("VerifyComplete() error = %v", err)
			}
			if len(fixture.client.patchCalls) != 0 {
				t.Fatalf("same-candidate retry issued patches: %#v", fixture.client.patchCalls)
			}
		})
	}
}

func TestControllerRBACTransitionRejectsUnsafePostApplyMixtures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		cursor int
		mutate func(*controllerRBACTransitionFixture)
		want   string
	}{
		{
			name:   "candidate-only binding before core cutover",
			cursor: 1,
			mutate: func(f *controllerRBACTransitionFixture) {
				f.installExactCandidateRuntimeRoleAndBinding()
			},
			want: "before the stable controller binding cutover is complete",
		},
		{
			name:   "candidate-only role before core cutover",
			cursor: 1,
			mutate: func(f *controllerRBACTransitionFixture) {
				f.installExactCandidateRuntimeRoleAndBinding()
				delete(f.client.roleBindings, privilegeBindingKey(f.guard.ReleaseNamespace, f.guard.ControllerDeploymentName+"-runtime-admission"))
			},
			want: "candidate-only Role/ptah-system/ptah-e2e-operator-runtime-admission exists before the stable controller binding cutover is complete",
		},
		{
			name:   "candidate stable rules before core cutover",
			cursor: 1,
			mutate: func(f *controllerRBACTransitionFixture) {
				f.client.clusterRoles[f.guard.ControllerDeploymentName].Rules = currentControllerClusterRoleRules(f.guard)
			},
			want: "before the stable binding cutover is complete",
		},
		{
			name:   "candidate-only binding has predecessor subject",
			cursor: 2,
			mutate: func(f *controllerRBACTransitionFixture) {
				f.installExactCandidateRuntimeRoleAndBinding()
				binding := f.client.roleBindings[privilegeBindingKey(f.guard.ReleaseNamespace, f.guard.ControllerDeploymentName+"-runtime-admission")]
				binding.Subjects = append([]rbacv1.Subject{f.previousSubject()}, f.transition.contract.postApplyBinding.fixedSubjects...)
			},
			want: "does not name the exact candidate controller",
		},
		{
			name:   "candidate-only role is missing",
			cursor: 2,
			mutate: func(f *controllerRBACTransitionFixture) {
				f.installExactCandidateRuntimeRoleAndBinding()
				delete(f.client.roles, privilegeBindingKey(f.guard.ReleaseNamespace, f.guard.ControllerDeploymentName+"-runtime-admission"))
			},
			want: "get Role",
		},
		{
			name:   "candidate-only role is foreign",
			cursor: 2,
			mutate: func(f *controllerRBACTransitionFixture) {
				f.installExactCandidateRuntimeRoleAndBinding()
				role := f.client.roles[privilegeBindingKey(f.guard.ReleaseNamespace, f.guard.ControllerDeploymentName+"-runtime-admission")]
				role.Rules[0].Verbs = append(role.Rules[0].Verbs, "list")
			},
			want: "exact predecessor and candidate contracts",
		},
		{
			name:   "managed candidate ServiceAccount has foreign ownership",
			cursor: 2,
			mutate: func(f *controllerRBACTransitionFixture) {
				f.client.serviceAccounts[f.guard.ControllerServiceAccountName] = controllerRBACServiceAccount(
					f.guard, f.guard.ControllerServiceAccountName, "candidate-uid", "31", false,
				)
			},
			want: "lacks exact Helm ownership",
		},
		{
			name:   "managed candidate ServiceAccount is deleting",
			cursor: 2,
			mutate: func(f *controllerRBACTransitionFixture) {
				account := controllerRBACServiceAccount(f.guard, f.guard.ControllerServiceAccountName, "candidate-uid", "31", true)
				now := metav1.Now()
				account.DeletionTimestamp = &now
				f.client.serviceAccounts[f.guard.ControllerServiceAccountName] = account
			},
			want: "incomplete or deleting identity",
		},
		{
			name:   "fully cut over stable role has foreign rules",
			cursor: 2,
			mutate: func(f *controllerRBACTransitionFixture) {
				f.client.clusterRoles[f.guard.ControllerDeploymentName].Rules = []rbacv1.PolicyRule{{Verbs: []string{"*"}}}
			},
			want: "exact predecessor and candidate contracts",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newControllerRBACTransitionFixture(t, test.cursor)
			test.mutate(fixture)
			retry, err := NewControllerRBACTransition(fixture.guard, fixture.runtimeContract, fixture.client)
			if err != nil {
				t.Fatal(err)
			}
			err = retry.Preflight(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Preflight() error = %v, want containing %q", err, test.want)
			}
			if len(fixture.client.patchCalls) != 0 {
				t.Fatalf("unsafe post-apply mixture issued patches: %#v", fixture.client.patchCalls)
			}
		})
	}
}

func TestControllerRBACTransitionRechecksPostApplyServiceAccountAndRoleVersions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*controllerRBACTransitionFixture)
		want   string
	}{
		{
			name: "candidate ServiceAccount changed",
			mutate: func(f *controllerRBACTransitionFixture) {
				f.client.serviceAccounts[f.guard.ControllerServiceAccountName].ResourceVersion = "32"
			},
			want: "ServiceAccount identity changed",
		},
		{
			name: "candidate role changed",
			mutate: func(f *controllerRBACTransitionFixture) {
				f.client.clusterRoles[f.guard.ControllerDeploymentName].ResourceVersion = "41"
			},
			want: "role resourceVersions changed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newControllerRBACTransitionFixture(t, 2)
			fixture.client.serviceAccounts[fixture.guard.ControllerServiceAccountName] = controllerRBACServiceAccount(
				fixture.guard, fixture.guard.ControllerServiceAccountName, "candidate-uid", "31", true,
			)
			fixture.installExactCandidateRolesAndRuntimeBinding()
			retry, err := NewControllerRBACTransition(fixture.guard, fixture.runtimeContract, fixture.client)
			if err != nil {
				t.Fatal(err)
			}
			if err := retry.Preflight(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := retry.Transition(context.Background()); err != nil {
				t.Fatal(err)
			}
			test.mutate(fixture)
			err = retry.VerifyComplete(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyComplete() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestControllerRBACPredecessorAuthorizationProbeCoversExactLegacyUnion(t *testing.T) {
	t.Parallel()
	fixture := newControllerRBACTransitionFixture(t, 2)
	probe, err := fixture.transition.PredecessorAuthorizationProbe()
	if err != nil {
		t.Fatal(err)
	}
	if probe.Subject.Name != "previous-controller" ||
		probe.Subject.User != "system:serviceaccount:ptah-system:legacy-controller" ||
		probe.Subject.UID != string(fixture.guard.PreviousControllerServiceAccountUID) {
		t.Fatalf("probe subject = %#v", probe.Subject)
	}
	if got, want := len(probe.Checks), 47; got != want {
		t.Fatalf("legacy authorization checks = %d, want complete %d-check union", got, want)
	}
	assertControllerRBACCheck(t, probe.Checks, "ptah-system", "list", "operator.ptah.dev", "ptahschemas", "", "")
	assertControllerRBACCheck(t, probe.Checks, "ptah-system", "watch", "batch", "jobs", "", "")
	assertControllerRBACCheck(t, probe.Checks, "ptah-system", "get", "", "pods", "log", "ptah-controller-rbac-revocation-probe")
	assertControllerRBACCheck(t, probe.Checks, "ptah-system", "update", "", "events", "", "ptah-controller-rbac-revocation-probe")
	assertControllerRBACCheck(t, probe.Checks, "ptah-coordination", "create", "coordination.k8s.io", "leases", "", "")
	for _, check := range probe.Checks {
		if check.ResourceAttributes == nil {
			t.Fatalf("check %q is not a resource check", check.Name)
		}
	}
}

func assertControllerRBACCheck(
	t *testing.T,
	checks []AuthorizationCheck,
	namespace, verb, group, resource, subresource, name string,
) {
	t.Helper()
	for _, check := range checks {
		attributes := check.ResourceAttributes
		if attributes != nil && attributes.Namespace == namespace && attributes.Verb == verb &&
			attributes.Group == group && attributes.Resource == resource &&
			attributes.Subresource == subresource && attributes.Name == name {
			return
		}
	}
	t.Fatalf("missing authorization check namespace=%q verb=%q group=%q resource=%q subresource=%q name=%q", namespace, verb, group, resource, subresource, name)
}

func assertControllerRBACJSONPatch(
	t *testing.T,
	patch []byte,
	guard *RolloutGuard,
	previous, candidate rbacv1.Subject,
) {
	t.Helper()
	var operations []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(patch, &operations); err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"/metadata/uid", "/metadata/resourceVersion", "/roleRef", "/subjects", "/subjects"}
	wantOps := []string{"test", "test", "test", "test", "replace"}
	if len(operations) != len(wantOps) {
		t.Fatalf("JSON patch operations = %d, want %d", len(operations), len(wantOps))
	}
	for index := range operations {
		if operations[index].Op != wantOps[index] || operations[index].Path != wantPaths[index] {
			t.Fatalf("JSON patch operation %d = %s %s, want %s %s", index, operations[index].Op, operations[index].Path, wantOps[index], wantPaths[index])
		}
	}
	var oldSubjects, newSubjects []rbacv1.Subject
	if err := json.Unmarshal(operations[3].Value, &oldSubjects); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(operations[4].Value, &newSubjects); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(oldSubjects, []rbacv1.Subject{previous}) ||
		!reflect.DeepEqual(newSubjects, []rbacv1.Subject{candidate}) {
		t.Fatalf("JSON patch subjects = %#v -> %#v", oldSubjects, newSubjects)
	}
	if !strings.Contains(string(operations[2].Value), guard.ControllerDeploymentName) {
		t.Fatalf("JSON patch roleRef does not name stable controller role: %s", operations[2].Value)
	}
}

type controllerRBACTransitionFixture struct {
	guard           *RolloutGuard
	runtimeContract RuntimeAdmissionContract
	client          *fakeControllerRBACClient
	transition      *ControllerRBACTransition
}

func newControllerRBACTransitionFixture(t *testing.T, cursor int) *controllerRBACTransitionFixture {
	t.Helper()
	guard := &RolloutGuard{
		ReleaseName:                          "ptah-e2e",
		ReleaseNamespace:                     "ptah-system",
		CoordinationNamespace:                "ptah-coordination",
		LeaderElection:                       true,
		LeaderElectionID:                     "ptah-operator.operator.ptah.dev",
		WebhookServiceName:                   "ptah-e2e-webhook",
		WebhookTimeoutSeconds:                5,
		WebhookSecretName:                    "ptah-e2e-webhook-cert",
		WebhookPort:                          9443,
		CertificateHealthPort:                8081,
		HookServiceAccountName:               "ptah-e2e-operator-crd-v1-0123456789ab",
		ControllerServiceAccountName:         "ptah-e2e-operator-v1-candidate",
		ControllerServiceAccountManaged:      true,
		PreviousControllerServiceAccountName: "legacy-controller",
		PreviousControllerServiceAccountUID:  "legacy-controller-uid",
		PreviousControllerReleaseSequence:    0,
		ControllerDeploymentName:             "ptah-e2e-operator",
		ControllerReplicas:                   1,
		CertificateDeploymentName:            "ptah-e2e-operator-cert-rotator",
		ControllerStateVersion:               1,
		AdmissionContractVersion:             1,
		ReleaseSequence:                      1,
		ManagerImage:                         "registry.example/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ControllerArgs:                       []string{"--leader-elect=true"},
		CertificateArgs:                      []string{"--namespace=ptah-system"},
		RuntimeDeploymentConfigExpressions:   []string{"true"},
		RuntimePodConfigExpressions:          []string{"true"},
		RuntimeAdmissionContractB64:          "e30=",
		PollEvery:                            time.Millisecond,
	}
	client := &fakeControllerRBACClient{
		roleBindings:    make(map[string]*rbacv1.RoleBinding),
		clusterBindings: make(map[string]*rbacv1.ClusterRoleBinding),
		roles:           make(map[string]*rbacv1.Role),
		clusterRoles:    make(map[string]*rbacv1.ClusterRole),
		serviceAccounts: make(map[string]*corev1.ServiceAccount),
	}
	runtimeContract := RuntimeAdmissionContract{
		Namespace:                     guard.ReleaseNamespace,
		ControllerServiceAccountName:  guard.ControllerServiceAccountName,
		CertificateServiceAccountName: guard.CertificateDeploymentName,
	}
	transition, err := NewControllerRBACTransition(guard, runtimeContract, client)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &controllerRBACTransitionFixture{
		guard: guard, runtimeContract: runtimeContract, client: client, transition: transition,
	}
	for index, contract := range transition.contract.bindings {
		subject := fixture.previousSubject()
		if index < cursor {
			subject = fixture.candidateSubject()
		}
		if contract.cluster {
			client.clusterBindings[contract.name] = controllerRBACClusterRoleBinding(guard, contract, subject, "binding-cluster", "11")
		} else {
			client.roleBindings[privilegeBindingKey(contract.namespace, contract.name)] = controllerRBACRoleBinding(guard, contract, subject, "binding-coordination", "12")
		}
	}
	for _, contract := range transition.contract.roles {
		metadata := controllerRBACObjectMeta(guard, contract.name, contract.namespace, types.UID("role-"+contract.name), "7")
		if contract.cluster {
			client.clusterRoles[contract.name] = &rbacv1.ClusterRole{ObjectMeta: metadata, Rules: append([]rbacv1.PolicyRule(nil), contract.predecessorRules...)}
		} else {
			client.roles[privilegeBindingKey(contract.namespace, contract.name)] = &rbacv1.Role{ObjectMeta: metadata, Rules: append([]rbacv1.PolicyRule(nil), contract.predecessorRules...)}
		}
	}
	client.serviceAccounts[guard.PreviousControllerServiceAccountName] = controllerRBACServiceAccount(
		guard,
		guard.PreviousControllerServiceAccountName,
		guard.PreviousControllerServiceAccountUID,
		"5",
		false,
	)
	return fixture
}

func (f *controllerRBACTransitionFixture) previousSubject() rbacv1.Subject {
	return controllerRBACServiceAccountSubject(f.guard.ReleaseNamespace, f.guard.PreviousControllerServiceAccountName)
}

func (f *controllerRBACTransitionFixture) candidateSubject() rbacv1.Subject {
	return controllerRBACServiceAccountSubject(f.guard.ReleaseNamespace, f.guard.ControllerServiceAccountName)
}

func (f *controllerRBACTransitionFixture) setBindingSubject(contract controllerRBACBindingContract, subject rbacv1.Subject) {
	subjects := append([]rbacv1.Subject{subject}, contract.fixedSubjects...)
	if contract.cluster {
		f.client.clusterBindings[contract.name].Subjects = subjects
		return
	}
	f.client.roleBindings[privilegeBindingKey(contract.namespace, contract.name)].Subjects = subjects
}

func (f *controllerRBACTransitionFixture) installExactCandidateRolesAndRuntimeBinding() {
	f.client.clusterRoles[f.guard.ControllerDeploymentName].Rules = currentControllerClusterRoleRules(f.guard)
	f.client.clusterRoles[f.guard.ControllerDeploymentName].ResourceVersion = "40"
	f.installExactCandidateRuntimeRoleAndBinding()
}

func (f *controllerRBACTransitionFixture) installExactCandidateRuntimeRoleAndBinding() {
	roleContract := *f.transition.contract.postApplyRole
	bindingContract := *f.transition.contract.postApplyBinding
	f.client.roles[privilegeBindingKey(roleContract.namespace, roleContract.name)] = &rbacv1.Role{
		ObjectMeta: controllerRBACObjectMeta(f.guard, roleContract.name, roleContract.namespace, "runtime-role-uid", "42"),
		Rules:      append([]rbacv1.PolicyRule(nil), roleContract.candidateRules...),
	}
	f.client.roleBindings[privilegeBindingKey(bindingContract.namespace, bindingContract.name)] = controllerRBACRoleBinding(
		f.guard,
		bindingContract,
		f.candidateSubject(),
		"runtime-binding-uid",
		"43",
	)
}

func controllerRBACObjectMeta(
	guard *RolloutGuard,
	name, namespace string,
	uid types.UID,
	resourceVersion string,
) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:            name,
		Namespace:       namespace,
		UID:             uid,
		ResourceVersion: resourceVersion,
		Annotations: map[string]string{
			helmReleaseNameAnnotation:      guard.ReleaseName,
			helmReleaseNamespaceAnnotation: guard.ReleaseNamespace,
		},
		Labels: map[string]string{
			managedByLabel: "Helm",
			instanceLabel:  guard.ReleaseName,
		},
	}
}

func controllerRBACClusterRoleBinding(
	guard *RolloutGuard,
	contract controllerRBACBindingContract,
	subject rbacv1.Subject,
	uid types.UID,
	resourceVersion string,
) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: controllerRBACObjectMeta(guard, contract.name, "", uid, resourceVersion),
		RoleRef:    contract.roleRef,
		Subjects:   append([]rbacv1.Subject{subject}, contract.fixedSubjects...),
	}
}

func controllerRBACRoleBinding(
	guard *RolloutGuard,
	contract controllerRBACBindingContract,
	subject rbacv1.Subject,
	uid types.UID,
	resourceVersion string,
) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: controllerRBACObjectMeta(guard, contract.name, contract.namespace, uid, resourceVersion),
		RoleRef:    contract.roleRef,
		Subjects:   append([]rbacv1.Subject{subject}, contract.fixedSubjects...),
	}
}

func controllerRBACServiceAccount(
	guard *RolloutGuard,
	name string,
	uid types.UID,
	resourceVersion string,
	managed bool,
) *corev1.ServiceAccount {
	metadata := metav1.ObjectMeta{Name: name, Namespace: guard.ReleaseNamespace, UID: uid, ResourceVersion: resourceVersion}
	if managed {
		metadata.Annotations = map[string]string{
			helmReleaseNameAnnotation:      guard.ReleaseName,
			helmReleaseNamespaceAnnotation: guard.ReleaseNamespace,
		}
		metadata.Labels = map[string]string{managedByLabel: "Helm", instanceLabel: guard.ReleaseName}
	}
	return &corev1.ServiceAccount{ObjectMeta: metadata}
}

type controllerRBACPatchCall struct {
	key    string
	dryRun bool
	patch  []byte
}

type fakeControllerRBACClient struct {
	roleBindings       map[string]*rbacv1.RoleBinding
	clusterBindings    map[string]*rbacv1.ClusterRoleBinding
	roles              map[string]*rbacv1.Role
	clusterRoles       map[string]*rbacv1.ClusterRole
	serviceAccounts    map[string]*corev1.ServiceAccount
	patchCalls         []controllerRBACPatchCall
	dryRunError        error
	nilDryRunResult    bool
	mutateDuringDryRun bool
	afterDryRun        func()
	persistError       error
	persistBeforeError bool
}

func (c *fakeControllerRBACClient) ListRoleBindings(context.Context, metav1.ListOptions) (*rbacv1.RoleBindingList, error) {
	items := make([]rbacv1.RoleBinding, 0, len(c.roleBindings))
	for _, binding := range c.roleBindings {
		items = append(items, *binding.DeepCopy())
	}
	sortRoleBindings(items)
	return &rbacv1.RoleBindingList{ListMeta: metav1.ListMeta{ResourceVersion: "inventory-rv"}, Items: items}, nil
}

func sortRoleBindings(items []rbacv1.RoleBinding) {
	sort.Slice(items, func(i, j int) bool {
		return privilegeBindingKey(items[i].Namespace, items[i].Name) < privilegeBindingKey(items[j].Namespace, items[j].Name)
	})
}

func (c *fakeControllerRBACClient) ListClusterRoleBindings(context.Context, metav1.ListOptions) (*rbacv1.ClusterRoleBindingList, error) {
	items := make([]rbacv1.ClusterRoleBinding, 0, len(c.clusterBindings))
	for _, binding := range c.clusterBindings {
		items = append(items, *binding.DeepCopy())
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return &rbacv1.ClusterRoleBindingList{ListMeta: metav1.ListMeta{ResourceVersion: "inventory-rv"}, Items: items}, nil
}

func (c *fakeControllerRBACClient) GetRole(_ context.Context, namespace, name string, _ metav1.GetOptions) (*rbacv1.Role, error) {
	role := c.roles[privilegeBindingKey(namespace, name)]
	if role == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: rbacv1.GroupName, Resource: "roles"}, name)
	}
	return role.DeepCopy(), nil
}

func (c *fakeControllerRBACClient) GetClusterRole(_ context.Context, name string, _ metav1.GetOptions) (*rbacv1.ClusterRole, error) {
	role := c.clusterRoles[name]
	if role == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: rbacv1.GroupName, Resource: "clusterroles"}, name)
	}
	return role.DeepCopy(), nil
}

func (c *fakeControllerRBACClient) GetServiceAccount(_ context.Context, _, name string, _ metav1.GetOptions) (*corev1.ServiceAccount, error) {
	account := c.serviceAccounts[name]
	if account == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, name)
	}
	return account.DeepCopy(), nil
}

func (c *fakeControllerRBACClient) PatchRoleBinding(
	_ context.Context,
	namespace, name string,
	patchType types.PatchType,
	patch []byte,
	options metav1.PatchOptions,
) (*rbacv1.RoleBinding, error) {
	key := privilegeBindingKey(namespace, name)
	binding := c.roleBindings[key]
	if binding == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: rbacv1.GroupName, Resource: "rolebindings"}, name)
	}
	result, err := c.applyRoleBindingPatch(controllerRBACObjectKey(false, namespace, name), binding.RoleRef, &binding.ObjectMeta, &binding.Subjects, patchType, patch, options)
	if result == nil {
		return nil, err
	}
	copy := binding.DeepCopy()
	copy.Subjects = result
	return copy, err
}

func (c *fakeControllerRBACClient) PatchClusterRoleBinding(
	_ context.Context,
	name string,
	patchType types.PatchType,
	patch []byte,
	options metav1.PatchOptions,
) (*rbacv1.ClusterRoleBinding, error) {
	binding := c.clusterBindings[name]
	if binding == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: rbacv1.GroupName, Resource: "clusterrolebindings"}, name)
	}
	result, err := c.applyRoleBindingPatch(controllerRBACObjectKey(true, "", name), binding.RoleRef, &binding.ObjectMeta, &binding.Subjects, patchType, patch, options)
	if result == nil {
		return nil, err
	}
	copy := binding.DeepCopy()
	copy.Subjects = result
	return copy, err
}

func (c *fakeControllerRBACClient) applyRoleBindingPatch(
	key string,
	roleRef rbacv1.RoleRef,
	metadata *metav1.ObjectMeta,
	subjects *[]rbacv1.Subject,
	patchType types.PatchType,
	patch []byte,
	options metav1.PatchOptions,
) ([]rbacv1.Subject, error) {
	if patchType != types.JSONPatchType {
		return nil, fmt.Errorf("patch type = %q", patchType)
	}
	dryRun := len(options.DryRun) != 0
	c.patchCalls = append(c.patchCalls, controllerRBACPatchCall{key: key, dryRun: dryRun, patch: append([]byte(nil), patch...)})
	if dryRun && c.dryRunError != nil {
		return nil, c.dryRunError
	}
	var operations []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(patch, &operations); err != nil {
		return nil, err
	}
	if len(operations) != 5 {
		return nil, fmt.Errorf("patch has %d operations", len(operations))
	}
	var uid, resourceVersion string
	var testedRoleRef rbacv1.RoleRef
	var testedSubjects, candidate []rbacv1.Subject
	if err := json.Unmarshal(operations[0].Value, &uid); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(operations[1].Value, &resourceVersion); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(operations[2].Value, &testedRoleRef); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(operations[3].Value, &testedSubjects); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(operations[4].Value, &candidate); err != nil {
		return nil, err
	}
	if uid != string(metadata.UID) || resourceVersion != metadata.ResourceVersion ||
		!reflect.DeepEqual(testedRoleRef, roleRef) || !reflect.DeepEqual(testedSubjects, *subjects) {
		return nil, apierrors.NewConflict(schema.GroupResource{Group: rbacv1.GroupName, Resource: "bindings"}, metadata.Name, errors.New("JSON test failed"))
	}
	if dryRun {
		if c.mutateDuringDryRun {
			*subjects = append([]rbacv1.Subject(nil), candidate...)
			metadata.ResourceVersion = bumpControllerRBACResourceVersion(metadata.ResourceVersion)
		}
		if c.afterDryRun != nil {
			after := c.afterDryRun
			c.afterDryRun = nil
			after()
		}
		if c.nilDryRunResult {
			return nil, nil
		}
		return append([]rbacv1.Subject(nil), candidate...), nil
	}
	if c.persistError != nil && !c.persistBeforeError {
		return nil, c.persistError
	}
	*subjects = append([]rbacv1.Subject(nil), candidate...)
	metadata.ResourceVersion = bumpControllerRBACResourceVersion(metadata.ResourceVersion)
	if c.persistError != nil {
		return nil, c.persistError
	}
	return append([]rbacv1.Subject(nil), candidate...), nil
}

func bumpControllerRBACResourceVersion(value string) string {
	number, err := strconv.Atoi(value)
	if err != nil {
		return value + "-next"
	}
	return strconv.Itoa(number + 1)
}

var _ ControllerRBACClient = (*fakeControllerRBACClient)(nil)
