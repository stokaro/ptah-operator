package operate_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
)

const refusalMessage = "Verification policy refused the artifact with 1 finding(s): require_signature"

// refused is a resource's conditions once the policy refused its artifact
// (blockVerification and refuseMigrationArtifact in internal/controller).
func refused(family operate.Family, generation int64) []metav1.Condition {
	verified := condition(operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, "PolicyRefused", refusalMessage, generation)
	if family == operate.FamilyMigration {
		return []metav1.Condition{
			verified,
			condition(operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "PolicyRefused", refusalMessage, generation),
			condition(operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, "PolicyRefused",
				"Artifact verification policy must be satisfied before database access", generation),
			condition(operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, "PolicyRefused",
				"Nothing runs while the verification policy refuses the artifact", generation),
			condition(operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, "PolicyRefused",
				"No plan may be used for an artifact refused by policy", generation),
		}
	}
	return []metav1.Condition{
		verified,
		condition(operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, "ArtifactUnverified", "No plan may be used for an artifact refused by policy", generation),
		condition(operatorv1alpha1.ConditionInSync, metav1.ConditionFalse, "PolicyRefused",
			"In-sync status requires an artifact accepted by the current verification policy", generation),
		condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "PolicyRefused",
			"Artifact verification policy must be satisfied before database access", generation),
	}
}

// resolvedAgain is what the harvest of a Resolve writes over a refusal: a schema's
// ArtifactVerified False with Pending, a migration's Unknown with DigestPinned.
func resolvedAgain(family operate.Family) []metav1.Condition {
	if family == operate.FamilyMigration {
		return rewritten(refused(family, 3),
			condition(operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionUnknown, "DigestPinned", "The artifact resolved to a digest and has not been verified yet", 3))
	}
	return rewritten(refused(family, 3),
		condition(operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, "Pending", "Resolved content has not been verified", 3))
}

// A refusal stops a wait when it is the last word on the source the resource
// names now: ArtifactVerified False with PolicyRefused, for the generation it has
// now, at rest in Blocked, on two reads in a row. Each row below that names no stop
// is a document the filter has to refuse. The phase, Ready and every other condition
// that can carry the reason decide nothing.
func TestWaitStopsOnARefusalThatHolds(t *testing.T) {
	t.Parallel()

	resting := func(family operate.Family) doc {
		return doc{phase: "Blocked", observedAt: at(0), conditions: refused(family, 3)}
	}
	for _, row := range []struct {
		name string
		// only is the family the row is about, when one family alone can read so.
		only operate.Family
		docs func(operate.Family) []doc
		// wantStop is the read the wait stops at. Zero expects a timeout.
		wantStop int
	}{
		{
			name:     "a refusal that stands when the wait begins stops it, on the second read",
			docs:     func(family operate.Family) []doc { return []doc{resting(family)} },
			wantStop: 2,
		},
		{
			name: "a request answered long ago leaves the refusal standing",
			docs: func(family operate.Family) []doc {
				answered := resting(family)
				answered.annotation, answered.lastRead = request, request
				return []doc{answered}
			},
			wantStop: 2,
		},
		{
			name: "a value that starts no read holds it up only until the operator answers it",
			docs: func(family operate.Family) []doc {
				pending := resting(family)
				pending.annotation = "now please"
				answered := resting(family)
				answered.annotation, answered.lastRead = "now please", "now please"
				return []doc{pending, answered}
			},
			wantStop: 3,
		},
		{
			name: "one read of a refusal is not enough: the next read found a claim",
			docs: func(family operate.Family) []doc {
				claimed := resting(family)
				claimed.phase, claimed.operation = "Resolving", "Resolve"
				return []doc{resting(family), claimed}
			},
		},
		{
			name: "the count starts again after a read that found another state",
			docs: func(family operate.Family) []doc {
				claimed := resting(family)
				claimed.phase, claimed.operation = "Resolving", "Resolve"
				return []doc{resting(family), claimed, resting(family)}
			},
			wantStop: 4,
		},
		{
			name: "a spec edited since the refusal",
			docs: func(family operate.Family) []doc {
				edited := resting(family)
				edited.generation, edited.observedGeneration = 4, 3
				return []doc{edited}
			},
		},
		{
			name: "a read request the operator has not taken",
			docs: func(family operate.Family) []doc {
				requested := resting(family)
				requested.annotation = request
				return []doc{requested}
			},
		},
		{
			name: "a request replaced by another the operator has not taken",
			docs: func(family operate.Family) []doc {
				requested := resting(family)
				requested.annotation, requested.lastRead = otherRequest, request
				return []doc{requested}
			},
		},
		{
			name: "the claim of the read that asks again",
			docs: func(family operate.Family) []doc {
				claimed := resting(family)
				claimed.phase, claimed.operation = "Resolving", "Resolve"
				return []doc{claimed}
			},
		},
		{
			name: "a claim in flight though the phase still reads Blocked",
			docs: func(family operate.Family) []doc {
				claimed := resting(family)
				claimed.operation = "Resolve"
				return []doc{claimed}
			},
		},
		{
			name: "a proof owed in Blocked",
			only: operate.FamilySchema,
			docs: func(family operate.Family) []doc {
				owed := resting(family)
				owed.proof = true
				return []doc{owed}
			},
		},
		{
			name: "a phase that is not Blocked",
			docs: func(family operate.Family) []doc {
				failed := resting(family)
				failed.phase = "Failed"
				return []doc{failed}
			},
		},
		{
			name: "the same artifact resolved again, not verified yet",
			docs: func(family operate.Family) []doc {
				return []doc{{phase: "Verifying", observedAt: at(0), conditions: resolvedAgain(family)}}
			},
		},
		{
			name: "the same conditions at rest in Blocked with the verdict gone",
			docs: func(family operate.Family) []doc {
				return []doc{{phase: "Blocked", observedAt: at(0), conditions: resolvedAgain(family)}}
			},
		},
		{
			// What #866 left behind and #904 cleared: a stale reason beside an
			// artifact that passed. A resource written by an older manager can
			// still read this way.
			name: "the reason left on other conditions, beside an artifact that passed",
			docs: func(family operate.Family) []doc {
				return []doc{{phase: "Blocked", observedAt: at(0), conditions: rewritten(refused(family, 3),
					condition(operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionTrue, "PolicySatisfied", "The resolved artifact satisfied its verification policy", 3))}}
			},
		},
		{
			name: "another reason on ArtifactVerified False",
			docs: func(family operate.Family) []doc {
				return []doc{{phase: "Blocked", observedAt: at(0), conditions: rewritten(refused(family, 3),
					condition(operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, "PolicyChanged", "The verification policy changed", 3))}}
			},
		},
		{
			name: "the reason on ArtifactVerified Unknown",
			docs: func(family operate.Family) []doc {
				return []doc{{phase: "Blocked", observedAt: at(0), conditions: rewritten(refused(family, 3),
					condition(operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionUnknown, "PolicyRefused", refusalMessage, 3))}}
			},
		},
		{
			name: "a source with no ArtifactVerified: a Git or ConfigMap source removes it",
			docs: func(family operate.Family) []doc {
				return []doc{{phase: "Blocked", observedAt: at(0), conditions: refused(family, 3)[1:]}}
			},
		},
	} {
		for _, family := range []operate.Family{operate.FamilySchema, operate.FamilyMigration} {
			if row.only != "" && row.only != family {
				continue
			}
			for _, target := range []operate.Target{operate.TargetInSync, operate.TargetApproval, operate.TargetApplied} {
				t.Run(row.name+"/"+string(family)+"/"+string(target), func(t *testing.T) {
					t.Parallel()

					tl := &timeline{family: family, docs: row.docs(family)}
					_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
						Target: target, Timeout: timeout, Interval: interval, Clock: newStepClock(),
					})

					var stop *operate.StopError
					var timedOut *operate.TimeoutError
					switch {
					case row.wantStop > 0:
						if !errors.As(err, &stop) || stop.Reason != operate.StopPolicyRefused || stop.Phase != "Blocked" ||
							tl.readCount() != row.wantStop || !strings.Contains(stop.Message, refusalMessage) {
							t.Fatalf("err = %v after %d reads, want a %s stop at read %d naming %q",
								err, tl.readCount(), operate.StopPolicyRefused, row.wantStop, refusalMessage)
						}
					case !errors.As(err, &timedOut):
						t.Fatalf("err = %v, want a timeout", err)
					}
				})
			}
		}
	}
}

// A wait for a read or a suspension asks something a refusal does not answer.
func TestARefusalDoesNotStopAWaitForAReadOrASuspension(t *testing.T) {
	t.Parallel()
	for _, family := range []operate.Family{operate.FamilySchema, operate.FamilyMigration} {
		t.Run(string(family)+"/read", func(t *testing.T) {
			t.Parallel()
			at0 := doc{phase: "Blocked", observedAt: at(0), annotation: request, lastRead: request, conditions: refused(family, 3)}
			tl := &timeline{family: family, docs: []doc{at0}}
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
				Target: operate.TargetRead, Timeout: timeout, Interval: interval, Clock: newStepClock(), ReadRequest: request,
			})
			var timedOut *operate.TimeoutError
			if !errors.As(err, &timedOut) || !strings.Contains(timedOut.Error(), "PolicyRefused") {
				t.Fatalf("err = %v, want a timeout that names the refusal", err)
			}
		})
		t.Run(string(family)+"/suspended", func(t *testing.T) {
			t.Parallel()
			suspended := doc{suspend: true, phase: "Suspended", observedAt: at(0), conditions: append(refused(family, 3),
				suspendedConditions(family, 3)...)}
			tl := &timeline{family: family, docs: []doc{suspended}}
			if _, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
				Target: operate.TargetSuspended, Timeout: timeout, Interval: interval, Clock: newStepClock(),
			}); err != nil {
				t.Fatalf("wait for suspension = %v, want it reached", err)
			}
		})
	}
}

// A wait for the reference set-source wrote stops on a refusal of that artifact
// alone: the refusal the answer to the change still carries is the previous
// artifact's, and a refusal at the new generation of another digest is not this one's.
func TestWaitForASourceStopsOnARefusalOfThatSource(t *testing.T) {
	t.Parallel()

	for _, family := range []operate.Family{operate.FamilySchema, operate.FamilyMigration} {
		answer := doc{generation: 4, observedGeneration: 3, phase: "Blocked", observedAt: at(0), conditions: refused(family, 3)}
		resolving := doc{generation: 4, phase: "Resolving", operation: "Resolve", observedAt: at(0), conditions: refused(family, 3)}
		nowRefused := doc{generation: 4, phase: "Blocked", observedAt: at(0), conditions: refused(family, 4)}
		for _, row := range []struct {
			name     string
			objects  []client.Object
			wantStop int
		}{
			{
				name: "its own refusal stops the wait",
				objects: []client.Object{
					pointedAt(answer, family, pinnedNew, digestOld, digestOld),
					pointedAt(resolving, family, pinnedNew, digestOld, digestOld),
					pointedAt(nowRefused, family, pinnedNew, digestNew, digestNew),
				},
				wantStop: 4,
			},
			{
				name:    "the previous artifact's refusal does not",
				objects: []client.Object{pointedAt(answer, family, pinnedNew, digestOld, digestOld)},
			},
			{
				name:    "a refusal at the new generation of another digest does not",
				objects: []client.Object{pointedAt(nowRefused, family, pinnedNew, digestOld, digestOld)},
			},
		} {
			for _, target := range []operate.Target{operate.TargetPlanned, operate.TargetApproval, operate.TargetInSync} {
				t.Run(string(family)+"/"+row.name+"/"+string(target), func(t *testing.T) {
					t.Parallel()

					tl := &served{objects: row.objects}
					_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
						Target: target, Timeout: timeout, Interval: interval, Clock: newStepClock(),
						Since: pointedAt(answer, family, pinnedNew, digestOld, digestOld), Generation: 4, Source: pinnedNew,
					})

					var stop *operate.StopError
					var timedOut *operate.TimeoutError
					switch {
					case row.wantStop > 0:
						if !errors.As(err, &stop) || stop.Reason != operate.StopPolicyRefused || tl.reads != row.wantStop {
							t.Fatalf("err = %v after %d reads, want a %s stop at read %d", err, tl.reads, operate.StopPolicyRefused, row.wantStop)
						}
					case !errors.As(err, &timedOut):
						t.Fatalf("err = %v, want a timeout", err)
					}
				})
			}
		}
	}
}

// An approved plan's wait hears the refusal too: the refusal took the approval with it.
func TestWaitForAPlanStopsOnARefusal(t *testing.T) {
	t.Parallel()
	for _, family := range []operate.Family{operate.FamilySchema, operate.FamilyMigration} {
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()
			tl := &timeline{family: family, docs: []doc{{phase: "Blocked", observedAt: at(0), conditions: refused(family, 3)}}}
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
				Target: operate.TargetApplied, Timeout: timeout, Interval: interval, Clock: newStepClock(), Plan: "plan-a",
			})
			var stop *operate.StopError
			if !errors.As(err, &stop) || stop.Reason != operate.StopPolicyRefused {
				t.Fatalf("err = %v, want a %s stop", err, operate.StopPolicyRefused)
			}
		})
	}
}

// A refusal that comes after the run a wait for applied is about has ended does not
// say the target will not come: the run changed the database. It holds back the
// reading after the run, so the wait names the run, says it applied, and says no
// reading of its database has followed. The reading the resource shows is the one
// from before the run, dated before it, or in the same second, or after it by
// another manager's clock; none of those dates may read as a history read back.
func TestARefusalAfterAnAppliedRunSaysTheRunApplied(t *testing.T) {
	t.Parallel()

	const job = "run-1"
	baseline := doc{phase: "AwaitingApproval", observedAt: at(0), plan: &planDoc{name: "plan-a", uid: "plan-uid-a"}}
	applied := func(finishedAt int) *runDoc {
		return &runDoc{
			outcome: operatorv1alpha1.MigrationRunOutcomeApplied, job: job, jobUID: "job-uid-1",
			startedAt: at(40), finishedAt: at(finishedAt), operationID: "op-1",
			plan: "plan-a", planUID: "plan-uid-a", fingerprint: "sha256:a", approval: "approval-a", approvalUID: "approval-uid-a",
		}
	}
	refusedAfter := func(run *runDoc, observedAt int) doc {
		return doc{phase: "Blocked", observedAt: at(observedAt), lastRun: run, conditions: refused(operate.FamilyMigration, 3)}
	}
	claimed := func(run *runDoc) doc {
		return doc{phase: "Resolving", operation: "Resolve", observedAt: at(0), lastRun: run, conditions: refused(operate.FamilyMigration, 3)}
	}
	waits := []struct {
		name    string
		options operate.WaitOptions
	}{
		{"any run", operate.WaitOptions{}},
		{"a plan", operate.WaitOptions{Plan: "plan-a"}},
		{"a decision", operate.WaitOptions{Decision: &operate.Decision{
			PlanName: "plan-a", PlanUID: "plan-uid-a", ApprovalName: "approval-a", ApprovalUID: "approval-uid-a",
		}}},
	}
	for _, row := range []struct {
		name string
		docs []doc
		// wantStop is the read the wait stops at. Zero expects a timeout.
		wantStop int
	}{
		{
			name:     "the reading from before the run",
			docs:     []doc{baseline, refusedAfter(applied(80), 0)},
			wantStop: 3,
		},
		{
			name:     "the reading from before the run, in the run's second",
			docs:     []doc{baseline, refusedAfter(applied(0), 0)},
			wantStop: 3,
		},
		{
			name:     "the reading from before the run, dated after it",
			docs:     []doc{baseline, refusedAfter(applied(80), 90)},
			wantStop: 3,
		},
		{
			name: "one read of the refusal is not enough: the next read found a claim",
			docs: []doc{baseline, refusedAfter(applied(80), 0), claimed(applied(80))},
		},
	} {
		for _, wait := range waits {
			t.Run(row.name+"/"+wait.name, func(t *testing.T) {
				t.Parallel()

				tl := &timeline{family: operate.FamilyMigration, docs: row.docs}
				options := wait.options
				options.Target, options.Timeout, options.Interval, options.Clock = operate.TargetApplied, timeout, interval, newStepClock()
				_, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilyMigration), options)

				var stop *operate.StopError
				var timedOut *operate.TimeoutError
				switch {
				case row.wantStop > 0:
					if !errors.As(err, &stop) || stop.Reason != operate.StopNotProved || tl.readCount() != row.wantStop ||
						!strings.Contains(stop.Message, "run "+job+" recorded its migrations applied, and no reading of its database has followed") ||
						!strings.Contains(stop.Message, refusalMessage) {
						t.Fatalf("err = %v after %d reads, want a %s stop at read %d that names run %s and %q",
							err, tl.readCount(), operate.StopNotProved, row.wantStop, job, refusalMessage)
					}
				case !errors.As(err, &timedOut):
					t.Fatalf("err = %v, want a timeout", err)
				}
			})
		}
	}
}

// A run whose reading afterwards is recorded on it was read back, so a refusal that
// comes later stops no wait for it, whatever date that reading carries. Here another
// manager's clock dated it before the run ended: a wait for a plan or a decision
// goes by the record and is done, and a wait for any run, which dates the reading,
// keeps waiting for one after the run. Neither may say the target will not come, or
// that no reading followed the run.
func TestARefusalAfterARunReadBackStopsNoWait(t *testing.T) {
	t.Parallel()

	run := &runDoc{
		outcome: operatorv1alpha1.MigrationRunOutcomeApplied, job: "run-1", jobUID: "job-uid-1",
		startedAt: at(40), finishedAt: at(80), operationID: "op-1",
		plan: "plan-a", planUID: "plan-uid-a", fingerprint: "sha256:a", approval: "approval-a", approvalUID: "approval-uid-a",
		verification: operatorv1alpha1.MigrationRunVerifiedInSync, verifiedAt: at(70),
	}
	docs := []doc{
		{phase: "AwaitingApproval", observedAt: at(0), plan: &planDoc{name: "plan-a", uid: "plan-uid-a"}},
		{phase: "Blocked", observedAt: at(70), lastRun: run, conditions: refused(operate.FamilyMigration, 3)},
	}
	for _, wait := range []struct {
		name    string
		options operate.WaitOptions
		// done is a wait the record ends; any other keeps waiting to its timeout.
		done bool
	}{
		{name: "any run"},
		{name: "a plan", options: operate.WaitOptions{Plan: "plan-a"}, done: true},
		{name: "a decision", options: operate.WaitOptions{Decision: &operate.Decision{
			PlanName: "plan-a", PlanUID: "plan-uid-a", ApprovalName: "approval-a", ApprovalUID: "approval-uid-a",
		}}, done: true},
	} {
		t.Run(wait.name, func(t *testing.T) {
			t.Parallel()

			tl := &timeline{family: operate.FamilyMigration, docs: docs}
			options := wait.options
			options.Target, options.Timeout, options.Interval, options.Clock = operate.TargetApplied, timeout, interval, newStepClock()
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilyMigration), options)

			var timedOut *operate.TimeoutError
			switch {
			case wait.done && err != nil:
				t.Fatalf("err = %v, want the run recorded as read back to end the wait", err)
			case !wait.done && (!errors.As(err, &timedOut) || !strings.Contains(timedOut.Waiting, "the history read back after run run-1")):
				t.Fatalf("err = %v, want a timeout still waiting for the history read back after run run-1", err)
			}
		})
	}
}
