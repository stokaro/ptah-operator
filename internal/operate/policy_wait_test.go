package operate_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
)

// refusedPlan is the plan the destructive-block scenario of demo.yml run
// 41377670772 recorded the schema blocked on.
var refusedPlan = &planDoc{
	name: "ptah-plan-0132bf7579a7c04e68e35110", uid: "uid-plan-refused",
	fingerprint: "sha256:0132bf7579a7c04e68e35110893b9af3441e181891a013c1720c042421eca338",
}

const destructiveRefusal = "Plan contains destructive changes and policy disallows them; read them with kubectl ptah plan, " +
	"then either narrow the desired schema or set spec.policy.allowDestructive"

// destructiveBlocked is a schema's conditions once the operator published a
// destructive plan that spec.policy.allowDestructive does not allow, as that
// run printed them (setPlanPolicyStatus in internal/controller).
func destructiveBlocked(generation int64) []metav1.Condition {
	return []metav1.Condition{
		condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "PolicyBlocked", "Plan is blocked by destructive-change policy", generation),
		condition(operatorv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "ScopedChanges",
			"Managed scope requires 1 schema statements; highest severity destructive", generation),
		condition(operatorv1alpha1.ConditionInSync, metav1.ConditionFalse, "ScopedChanges",
			"The authoritative managed scope differs from the verified artifact", generation),
		condition(operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, "DestructiveChangesDisabled", destructiveRefusal, generation),
		condition(operatorv1alpha1.ConditionPlanReady, metav1.ConditionTrue, "Published", "Exact plan bytes are stored in immutable chunks", generation),
	}
}

// neverApplied is the same plan published under spec.policy.apply Never.
func neverApplied(generation int64) []metav1.Condition {
	return rewritten(destructiveBlocked(generation),
		condition(operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, "ApplyDisabled",
			"Policy records plans but does not apply them", generation),
		condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "ApplyDisabled", "Plan is ready but apply is disabled", generation))
}

// resolvingAgain is what the claim of the next cycle's Resolve writes over
// conditions: Ready in progress, and the source, the plan and convergence
// unknown until the cycle ends. ApprovalRequired is left as it was.
func resolvingAgain(conditions []metav1.Condition) []metav1.Condition {
	return rewritten(conditions,
		condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "OperationInProgress", "Resolve operation is in progress", 3),
		condition(operatorv1alpha1.ConditionPlanReady, metav1.ConditionUnknown, "SourceRefreshPending",
			"Plan currentness is unknown until source refresh and read-only reconciliation complete", 3),
		condition(operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, "SourceRefreshPending",
			"Convergence is unknown until source refresh and read-only reconciliation complete", 3))
}

// observedAgain is what the harvest of that cycle's Observe writes over
// conditions, in the write that records the new reading and clears
// status.plan. ApprovalRequired is left as it was.
func observedAgain(conditions []metav1.Condition) []metav1.Condition {
	return rewritten(conditions,
		condition(operatorv1alpha1.ConditionDriftDetected, metav1.ConditionUnknown, "ScopedPlanPending",
			"Raw drift was recorded; the authoritative managed scope is being planned", 3),
		condition(operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, "ScopedPlanPending",
			"Convergence is unknown until scoped planning completes", 3),
		condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "ScopedPlanPending", "A read-only scoped plan is required", 3))
}

// realmContested is a schema a peer's claim on its database blocks
// (schemaRealmBlocked), which leaves its plan in place.
func realmContested(generation int64) []metav1.Condition {
	message := "This database realm is claimed by 2 resources (2 PtahSchema, 0 PtahMigration) and 1 of them have not set " +
		"spec.target.sharedRealm. Nothing runs against the database until every claimant declares the realm shared, " +
		"or until the others stop claiming it."
	return []metav1.Condition{
		condition(operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, "RealmConflict",
			"No plan is approvable while the database realm is contested", generation),
		condition(operatorv1alpha1.ConditionApplying, metav1.ConditionFalse, "RealmConflict",
			"No Apply operation is authorized while the database realm is contested", generation),
		condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "RealmConflict", message, generation),
	}
}

// rewritten is conditions with each of changes set over the one of its type.
func rewritten(conditions []metav1.Condition, changes ...metav1.Condition) []metav1.Condition {
	out := make([]metav1.Condition, len(conditions))
	copy(out, conditions)
	for _, change := range changes {
		meta.SetStatusCondition(&out, change)
	}
	return out
}

// A wait for in-sync, approval or applied stops with StopPolicyBlocked when
// the schema's current plan is one its own policy holds: decided from
// ApprovalRequired for the generation the resource has now, beside a plan
// computed from a reading that began after the wait did. The phase decides
// nothing.
func TestWaitStopsOnAPlanThePolicyHolds(t *testing.T) {
	t.Parallel()

	blocked := doc{phase: "Blocked", observedAt: at(0), plan: refusedPlan, conditions: destructiveBlocked(3)}
	// One read-only cycle after the wait began, read at every step, ending in
	// the same refusal from a reading at 30 seconds.
	cycle := []doc{
		blocked,
		{phase: "Resolving", operation: "Resolve", observedAt: at(0), plan: refusedPlan, conditions: resolvingAgain(destructiveBlocked(3))},
		{phase: "Planning", observedAt: at(30), conditions: observedAgain(resolvingAgain(destructiveBlocked(3)))},
		{phase: "Planning", operation: "Plan", observedAt: at(30), conditions: observedAgain(resolvingAgain(destructiveBlocked(3)))},
		{phase: "Blocked", observedAt: at(30), plan: refusedPlan, conditions: destructiveBlocked(3)},
	}

	rows := []struct {
		name   string
		target operate.Target
		docs   []doc
		// wantStop is the read the wait stops at with StopPolicyBlocked, in
		// wantPhase; wantReached the read that meets the target. Neither
		// expects a timeout.
		wantStop    int
		wantPhase   string
		wantReached int
		wantMessage string
	}{
		{
			name: "in-sync: the refusal the cycle after the wait began ends in stops it", target: operate.TargetInSync,
			docs: cycle, wantStop: 5, wantPhase: "Blocked",
			wantMessage: "spec.policy holds plan " + refusedPlan.name + " (DestructiveChangesDisabled): " + destructiveRefusal,
		},
		{
			name: "approval: the same refusal stops a wait for an approval", target: operate.TargetApproval,
			docs: cycle, wantStop: 5, wantPhase: "Blocked",
		},
		{
			name: "applied: the same refusal stops a wait for an Apply", target: operate.TargetApplied,
			docs: cycle, wantStop: 5, wantPhase: "Blocked",
		},
		{
			// The poll missed the Blocked rest of the cycle that refused,
			// and lands in the next one: Ready reads OperationInProgress, the
			// phase is Resolving, and the refusal stands.
			name: "in-sync: a refusal read mid-cycle, out of Blocked, still stops it", target: operate.TargetInSync,
			docs: []doc{
				blocked,
				{phase: "Resolving", operation: "Resolve", observedAt: at(30), plan: refusedPlan, conditions: resolvingAgain(destructiveBlocked(3))},
			},
			wantStop: 2, wantPhase: "Resolving",
		},
		{
			name: "in-sync: a plan spec.policy.apply Never records stops it", target: operate.TargetInSync,
			docs: []doc{
				{phase: "Blocked", observedAt: at(0), plan: refusedPlan, conditions: neverApplied(3)},
				{phase: "Blocked", observedAt: at(30), plan: refusedPlan, conditions: neverApplied(3)},
			},
			wantStop: 2, wantPhase: "Blocked",
			wantMessage: "(ApplyDisabled): Policy records plans but does not apply them",
		},
		{
			// A no-change plan leaves ApprovalRequired as the refusal wrote
			// it, and clears status.plan: the database was changed by hand.
			name: "in-sync: in sync after a refusal is in sync, though ApprovalRequired still names it", target: operate.TargetInSync,
			docs: []doc{
				blocked,
				{phase: "InSync", observedAt: at(30), conditions: append(inSyncConditions(operate.FamilySchema, 3),
					condition(operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, "DestructiveChangesDisabled", destructiveRefusal, 3))},
			},
			wantReached: 2,
		},
		{
			name: "in-sync: a refusal decided before the wait began does not stop it", target: operate.TargetInSync,
			docs: []doc{blocked},
		},
		{
			// The reading is the wait's, and the refusal beside it is the
			// previous plan's: the plan from this reading is not computed
			// yet, and the harvest cleared status.plan.
			name: "in-sync: a reading after the wait began, before its plan, does not stop it", target: operate.TargetInSync,
			docs: cycle[:3],
		},
		{
			// The cycle that refused ended after the wait began, and the
			// spec moved before the wait read it: the edit may be the one
			// that allows the plan.
			name: "in-sync: a refusal for an older generation does not stop it", target: operate.TargetInSync,
			docs: []doc{
				blocked,
				{generation: 4, observedGeneration: 3, phase: "Blocked", observedAt: at(30), plan: refusedPlan, conditions: destructiveBlocked(3)},
			},
		},
		{
			// Another resource's change or the realm's grant ends this
			// refusal, not this resource's spec.
			name: "in-sync: a database realm that blocks the resource does not stop it", target: operate.TargetInSync,
			docs: []doc{
				{phase: "AwaitingApproval", observedAt: at(0), plan: refusedPlan, conditions: approvalConditions(metav1.ConditionTrue)},
				{phase: "Blocked", observedAt: at(30), plan: refusedPlan, conditions: realmContested(3)},
			},
			wantMessage: "Ready=False (RealmConflict)",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			tl := &timeline{family: operate.FamilySchema, docs: row.docs}
			result, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilySchema), operate.WaitOptions{
				Target: row.target, Timeout: timeout, Interval: interval, Clock: newStepClock(),
			})

			var stop *operate.StopError
			var timedOut *operate.TimeoutError
			switch {
			case row.wantStop > 0:
				if !errors.As(err, &stop) || stop.Reason != operate.StopPolicyBlocked || stop.Phase != row.wantPhase ||
					tl.readCount() != row.wantStop || !strings.Contains(stop.Error(), row.wantMessage) {
					t.Fatalf("err = %v after %d reads, want a %s stop in phase %s at read %d naming %q",
						err, tl.readCount(), operate.StopPolicyBlocked, row.wantPhase, row.wantStop, row.wantMessage)
				}
			case row.wantReached > 0:
				if err != nil || result.Reads != row.wantReached || result.Target != row.target {
					t.Fatalf("wait = %+v, %v; want %s at read %d", result, err, row.target, row.wantReached)
				}
			default:
				if !errors.As(err, &timedOut) || !strings.Contains(timedOut.Error(), row.wantMessage) {
					t.Fatalf("err = %v, want a timeout naming %q", err, row.wantMessage)
				}
			}
		})
	}
}

// A wait for the reference set-source wrote stops on a refusal of the plan
// computed from it, whichever target it waits for, and not on the refusal of
// the previous artifact's plan the answer to the change still carried.
func TestWaitForASourceStopsOnAPlanThePolicyHolds(t *testing.T) {
	t.Parallel()

	// The previous artifact was refused too, at generation 3; the change made
	// generation 4.
	answer := doc{generation: 4, observedGeneration: 3, phase: "Blocked", observedAt: at(0), plan: refusedPlan,
		conditions: destructiveBlocked(3)}
	resolving := doc{generation: 4, phase: "Resolving", operation: "Resolve", observedAt: at(0), plan: refusedPlan,
		conditions: resolvingAgain(destructiveBlocked(3))}
	refused := doc{generation: 4, phase: "Blocked", observedAt: at(30), plan: refusedPlan, conditions: destructiveBlocked(4)}
	for _, target := range []operate.Target{operate.TargetPlanned, operate.TargetApproval, operate.TargetInSync} {
		t.Run(string(target), func(t *testing.T) {
			t.Parallel()

			tl := &served{objects: []client.Object{
				pointedAt(answer, operate.FamilySchema, pinnedNew, digestOld, digestOld),
				pointedAt(resolving, operate.FamilySchema, pinnedNew, digestOld, digestOld),
				pointedAt(refused, operate.FamilySchema, pinnedNew, digestNew, digestNew),
			}}
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilySchema), operate.WaitOptions{
				Target: target, Timeout: timeout, Interval: interval, Clock: newStepClock(),
				Since: pointedAt(answer, operate.FamilySchema, pinnedNew, digestOld, digestOld), Generation: 4, Source: pinnedNew,
			})

			var stop *operate.StopError
			if !errors.As(err, &stop) || stop.Reason != operate.StopPolicyBlocked || tl.reads != 3 ||
				!strings.Contains(stop.Message, refusedPlan.name+" (DestructiveChangesDisabled)") {
				t.Fatalf("err = %v after %d reads, want a %s stop at read 3", err, tl.reads, operate.StopPolicyBlocked)
			}
		})
	}
}

// neverPublished is a migration's conditions once the operator published a
// plan under spec.policy.apply Never (applyMigrationPolicyPhase).
func neverPublished(generation int64) []metav1.Condition {
	return []metav1.Condition{
		condition(operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, "NotRequired",
			"The apply policy is Never, so no approval is consulted", generation),
		condition(operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, "ApplyDisabled",
			"1 migration is pending and the apply policy is Never", generation),
		condition(operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, "ApplyDisabled",
			"The apply policy is Never, so nothing runs from here", generation),
	}
}

// migrationClaimed is what a claim writes over conditions: Progressing in
// progress. Ready is left as it was.
func migrationClaimed(operation string, conditions []metav1.Condition) []metav1.Condition {
	return rewritten(conditions, condition(operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		"OperationInProgress", operation+" operation is in progress", 3))
}

// historyHarvested is what the harvest of the history writes over conditions
// when versions are pending, in the write that records the reading.
func historyHarvested(conditions []metav1.Condition) []metav1.Condition {
	return rewritten(conditions,
		condition(operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, "MigrationsPending", "1 migration is pending", 3),
		condition(operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue, "MigrationsPending",
			"A plan for 1 pending migration is being published", 3))
}

// A migration under apply policy Never rests in Planning with its plan
// published, and nothing applies it: a wait for in-sync or applied stops on
// it as on a schema plan the policy holds, by Ready, which a claim leaves
// alone and the harvest of the history rewrites with the reading.
func TestWaitStopsOnAMigrationPlanUnderApplyNever(t *testing.T) {
	t.Parallel()

	plan := &planDoc{name: "shipments-p-14", uid: "uid-p-14"}
	resting := doc{phase: "Planning", observedAt: at(0), plan: plan, conditions: neverPublished(3)}
	cycle := []doc{
		resting,
		{phase: "Resolving", operation: "Resolve", observedAt: at(0), plan: plan, conditions: migrationClaimed("Resolve", neverPublished(3))},
		{phase: "Reading", operation: "History", observedAt: at(0), plan: plan, conditions: migrationClaimed("History", neverPublished(3))},
		// The harvest keeps the previous plan and rewrites Ready.
		{phase: "Planning", observedAt: at(30), plan: plan, conditions: historyHarvested(neverPublished(3))},
		{phase: "Planning", observedAt: at(30), plan: plan, conditions: neverPublished(3)},
	}
	for _, row := range []struct {
		name      string
		target    operate.Target
		docs      []doc
		wantStop  int
		wantPhase string
	}{
		{name: "in-sync: the plan the read after the wait began published", target: operate.TargetInSync, docs: cycle,
			wantStop: 5, wantPhase: "Planning"},
		{name: "applied: the same plan", target: operate.TargetApplied, docs: cycle, wantStop: 5, wantPhase: "Planning"},
		{name: "in-sync: read mid-cycle, after a publication the wait saw no rest of", target: operate.TargetInSync,
			docs: []doc{resting, {phase: "Resolving", operation: "Resolve", observedAt: at(30), plan: plan,
				conditions: migrationClaimed("Resolve", neverPublished(3))}},
			wantStop: 2, wantPhase: "Resolving"},
		{name: "in-sync: a publication before the wait began", target: operate.TargetInSync, docs: []doc{resting}},
		{name: "in-sync: a reading after the wait began, before its plan", target: operate.TargetInSync, docs: cycle[:4]},
		{name: "in-sync: a refusal for an older generation", target: operate.TargetInSync, docs: []doc{
			resting, {generation: 4, observedGeneration: 3, phase: "Planning", observedAt: at(30), plan: plan, conditions: neverPublished(3)},
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			tl := &timeline{family: operate.FamilyMigration, docs: row.docs}
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilyMigration), operate.WaitOptions{
				Target: row.target, Timeout: timeout, Interval: interval, Clock: newStepClock(),
			})

			if row.wantStop == 0 {
				var timedOut *operate.TimeoutError
				if !errors.As(err, &timedOut) {
					t.Fatalf("err = %v, want a timeout", err)
				}
				return
			}
			var stop *operate.StopError
			if !errors.As(err, &stop) || stop.Reason != operate.StopPolicyBlocked || stop.Phase != row.wantPhase ||
				tl.readCount() != row.wantStop ||
				!strings.Contains(stop.Message, "spec.policy holds plan shipments-p-14 (ApplyDisabled): 1 migration is pending and the apply policy is Never") {
				t.Fatalf("err = %v after %d reads, want a %s stop in phase %s at read %d", err, tl.readCount(),
					operate.StopPolicyBlocked, row.wantPhase, row.wantStop)
			}
		})
	}
}
