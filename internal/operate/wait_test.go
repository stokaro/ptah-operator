package operate_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
)

const (
	// request is the read request the read rows wait on: five seconds after
	// t0, so an observation at t0 is older than it.
	request      = "2026-10-01T09:00:05.123456789Z"
	otherRequest = "2026-10-01T09:00:07Z"
	oldRequest   = "2026-09-30T08:00:00Z"
	timeout      = 10 * time.Second
	interval     = time.Second
)

var resource = schema.GroupResource{Group: "ptah.run", Resource: "ptahschemas"}

func inSyncConditions(family operate.Family, generation int64) []metav1.Condition {
	if family == operate.FamilyMigration {
		return []metav1.Condition{condition(operatorv1alpha1.ConditionMigrationReady, metav1.ConditionTrue, "HistoryMatched", "The database has every migration this artifact carries", generation)}
	}
	return []metav1.Condition{
		condition(operatorv1alpha1.ConditionInSync, metav1.ConditionTrue, "ScopedConverged", "A stable scoped plan proves convergence", generation),
		condition(operatorv1alpha1.ConditionReady, metav1.ConditionTrue, "InSync", "Schema is in sync", generation),
	}
}

func approvalConditions(status metav1.ConditionStatus) []metav1.Condition {
	return []metav1.Condition{
		condition(operatorv1alpha1.ConditionApprovalRequired, status, "AwaitingApproval", "The plan needs an approval", 3),
		condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "AwaitingApproval", "The plan is waiting for approval", 3),
	}
}

// failedConditions are what a failure that leaves no claim writes: a
// schema's ReconciliationFailed and Ready, a migration's Ready and
// Progressing (migrationOperationFailure).
func failedConditions(family operate.Family, reason, message string) []metav1.Condition {
	ready := condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, reason, message, 3)
	if family == operate.FamilyMigration {
		return []metav1.Condition{ready, condition(operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, reason, message, 3)}
	}
	return []metav1.Condition{
		condition(operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionTrue, reason, message, 3),
		condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, reason, "Reconciliation did not complete", 3),
	}
}

// retriedConditions are what an attempt the operator retries under the same
// claim writes: a schema's ReconciliationFailed in phase Failed, a
// migration's Progressing, True, in whatever phase it was in.
func retriedConditions(family operate.Family, message string) []metav1.Condition {
	if family == operate.FamilyMigration {
		return []metav1.Condition{condition(operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue, "OperationFailed", message, 3)}
	}
	return []metav1.Condition{
		condition(operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionTrue, "OperationFailed", message, 3),
		condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "OperationFailed", "Reconciliation did not complete", 3),
	}
}

// claimFailure is the operation a failure that leaves no claim is journaled
// under: a schema fails before its claim is written and names none, a
// migration retires the claim it names.
func claimFailure(family operate.Family) string {
	if family == operate.FamilyMigration {
		return "Resolve"
	}
	return ""
}

func suspendedConditions(family operate.Family, generation int64) []metav1.Condition {
	ready := condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "Suspended", "Reconciliation is suspended", generation)
	if family == operate.FamilyMigration {
		return []metav1.Condition{ready}
	}
	return []metav1.Condition{
		condition(operatorv1alpha1.ConditionSuspended, metav1.ConditionTrue, "Requested", "New database operations are suspended", generation),
		ready,
	}
}

// readPhase and readOperation are a read cycle part way through: a schema
// observing the database, a migration reading its history.
func readPhase(family operate.Family) (string, string) {
	if family == operate.FamilyMigration {
		return "Reading", "History"
	}
	return "Observing", "Observe"
}

func failure(seconds int, operation, reason string) operatorv1alpha1.JournalEntry {
	return operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalOperationFailed, At: metav1.NewTime(at(seconds)), Operation: operation, Reason: reason,
	}
}

type waitRow struct {
	name   string
	only   operate.Family
	target operate.Target
	// since is the baseline passed as WaitOptions.Since; nil makes the
	// first read the baseline.
	since       func(operate.Family) *doc
	generation  int64
	readRequest string
	plan        string
	docs        func(operate.Family) []doc
	// wantReads is the read whose document meets the target. Zero expects
	// no success.
	wantReads   int
	wantStop    operate.StopReason
	wantTimeout bool
	// wantMessage is text the stop or timeout error has to carry.
	wantMessage string
}

func TestWaitTargets(t *testing.T) {
	t.Parallel()

	rows := []waitRow{
		// read
		{
			name: "read: a request taken while nothing ran is answered by the next read", target: operate.TargetRead,
			readRequest: request,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "InSync", observedAt: at(0), annotation: request, lastRead: oldRequest, conditions: inSyncConditions(f, 3)},
					{phase: "Resolving", operation: "Resolve", observedAt: at(0), annotation: request, lastRead: request},
					{phase: "InSync", observedAt: at(30), annotation: request, lastRead: request, conditions: inSyncConditions(f, 3)},
				}
			},
			wantReads: 3,
		},
		{
			name: "read: an answered request with no newer reading is not its answer", target: operate.TargetRead,
			readRequest: request,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "InSync", observedAt: at(0), annotation: request, lastRead: oldRequest, conditions: inSyncConditions(f, 3)},
					{phase: "InSync", observedAt: at(0), annotation: request, lastRead: request, conditions: inSyncConditions(f, 3)},
				}
			},
			wantTimeout: true, wantMessage: "a read of the database that began after request",
		},
		{
			name: "read: a read already running when the request arrived does not answer it", target: operate.TargetRead,
			readRequest: request,
			docs: func(f operate.Family) []doc {
				phase, operation := readPhase(f)
				return []doc{
					{phase: phase, operation: operation, observedAt: at(0), annotation: request, lastRead: oldRequest},
					// That read is harvested after the request was written.
					{phase: "InSync", observedAt: at(10), annotation: request, lastRead: oldRequest, conditions: inSyncConditions(f, 3)},
					// The operator takes the request only now, and the reading
					// it shows is still the one that began before it.
					{phase: "InSync", observedAt: at(10), annotation: request, lastRead: request, conditions: inSyncConditions(f, 3)},
				}
			},
			wantTimeout: true, wantMessage: "a read of the database that began after request",
		},
		{
			name: "read: the read after one already running is the answer", target: operate.TargetRead,
			readRequest: request,
			docs: func(f operate.Family) []doc {
				phase, operation := readPhase(f)
				return []doc{
					{phase: phase, operation: operation, observedAt: at(0), annotation: request, lastRead: oldRequest},
					{phase: "InSync", observedAt: at(10), annotation: request, lastRead: oldRequest, conditions: inSyncConditions(f, 3)},
					{phase: "InSync", observedAt: at(10), annotation: request, lastRead: request, conditions: inSyncConditions(f, 3)},
					{phase: "Resolving", operation: "Resolve", observedAt: at(10), annotation: request, lastRead: request},
					{phase: "InSync", observedAt: at(40), annotation: request, lastRead: request, conditions: inSyncConditions(f, 3)},
				}
			},
			wantReads: 5,
		},
		{
			name: "read: a newer reading with an operation still in flight is not the end", target: operate.TargetRead,
			readRequest: request,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "InSync", observedAt: at(0), annotation: request, lastRead: oldRequest},
					{phase: "Applying", operation: "Apply", observedAt: at(30), annotation: request, lastRead: request},
				}
			},
			wantTimeout: true, wantMessage: "the read to end: the Apply operation (phase Applying)",
		},
		{
			name: "read: a schema step retried under its claim does not stop the wait, and the timeout names it", target: operate.TargetRead,
			only: operate.FamilySchema, readRequest: request,
			docs: func(f operate.Family) []doc {
				// Planning failed after the observation. retryOperationAs keeps
				// the claim, writes Failed and journals the attempt.
				return []doc{
					{phase: "InSync", observedAt: at(0), annotation: request, lastRead: oldRequest},
					{
						phase: "Failed", operation: "Plan", observedAt: at(30), annotation: request, lastRead: request,
						conditions: retriedConditions(f, "the plan Job was evicted"),
						journal:    []operatorv1alpha1.JournalEntry{failure(40, "Plan", "OperationFailed")},
					},
				}
			},
			wantTimeout: true, wantMessage: "ReconciliationFailed=True (OperationFailed): the plan Job was evicted; still waiting for the read to end: the Plan operation (phase Failed)",
		},
		{
			name: "read: a migration step retried under its claim does not stop the wait, and the timeout names it", target: operate.TargetRead,
			only: operate.FamilyMigration, readRequest: request,
			docs: func(f operate.Family) []doc {
				// retryMigrationOperationAs keeps the claim and the phase and
				// journals nothing; Progressing carries the failure.
				return []doc{
					{phase: "InSync", observedAt: at(0), annotation: request, lastRead: oldRequest},
					{
						phase: "Reading", operation: "History", observedAt: at(0), annotation: request, lastRead: request,
						conditions: retriedConditions(f, "the history Job was evicted"),
					},
				}
			},
			wantTimeout: true, wantMessage: "Progressing=True (OperationFailed): the history Job was evicted",
		},
		{
			name: "read: a newer reading in a phase a cycle passes through is not the end", target: operate.TargetRead,
			readRequest: request,
			docs: func(f operate.Family) []doc {
				// A schema planning, or a migration that recorded its history
				// and has not yet published the plan it read.
				return []doc{
					{phase: "InSync", observedAt: at(0), annotation: request, lastRead: oldRequest},
					{phase: "Planning", observedAt: at(30), annotation: request, lastRead: request},
				}
			},
			wantTimeout: true, wantMessage: "the read to end: phase Planning",
		},
		{
			name: "read: a migration planning with its plan published has ended its read", target: operate.TargetRead,
			only: operate.FamilyMigration, readRequest: request,
			docs: func(f operate.Family) []doc {
				// Policy Never: publishing the plan replaced Progressing's
				// MigrationsPending with ApplyDisabled and left Planning.
				return []doc{
					{phase: "InSync", observedAt: at(0), annotation: request, lastRead: oldRequest},
					{
						phase: "Planning", observedAt: at(30), annotation: request, lastRead: request, plan: &planDoc{name: "plan-1", uid: "plan-uid-1"},
						conditions: []metav1.Condition{condition(operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
							"ApplyDisabled", "The apply policy is Never, so nothing runs from here", 3)},
					},
				}
			},
			wantReads: 2,
		},
		{
			name: "read: a migration between recording its history and publishing the plan has not ended its read", target: operate.TargetRead,
			only: operate.FamilyMigration, readRequest: request,
			docs: func(f operate.Family) []doc {
				// The first write of a history harvest leaves the previous
				// cycle's plan in status.plan.
				previous := &planDoc{name: "plan-0", uid: "plan-uid-0"}
				between := []metav1.Condition{
					condition(operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, "MigrationsPending", "2 migrations are pending", 3),
					condition(operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue, "MigrationsPending",
						"A plan for 2 pending migrations is being published", 3),
				}
				return []doc{
					{phase: "AwaitingApproval", observedAt: at(0), annotation: request, lastRead: oldRequest, plan: previous},
					{phase: "Reading", operation: "History", observedAt: at(0), annotation: request, lastRead: request, plan: previous},
					{phase: "Planning", observedAt: at(30), annotation: request, lastRead: request, plan: previous, conditions: between},
				}
			},
			wantTimeout: true, wantMessage: "the read to end: phase Planning",
		},
		{
			name: "read: a migration ends its read once the plan it read is published", target: operate.TargetRead,
			only: operate.FamilyMigration, readRequest: request,
			docs: func(f operate.Family) []doc {
				previous := &planDoc{name: "plan-0", uid: "plan-uid-0"}
				between := []metav1.Condition{
					condition(operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue, "MigrationsPending",
						"A plan for 2 pending migrations is being published", 3),
				}
				return []doc{
					{phase: "AwaitingApproval", observedAt: at(0), annotation: request, lastRead: oldRequest, plan: previous},
					{phase: "Reading", operation: "History", observedAt: at(0), annotation: request, lastRead: request, plan: previous},
					{phase: "Planning", observedAt: at(30), annotation: request, lastRead: request, plan: previous, conditions: between},
					{phase: "AwaitingApproval", observedAt: at(30), annotation: request, lastRead: request, plan: &planDoc{name: "plan-1", uid: "plan-uid-1"}, conditions: approvalConditions(metav1.ConditionTrue)},
				}
			},
			wantReads: 4,
		},
		{
			name: "read: a read that fails with no claim left stops the wait", target: operate.TargetRead,
			readRequest: request,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "InSync", observedAt: at(0), annotation: request, lastRead: oldRequest},
					{
						phase: "Failed", observedAt: at(0), annotation: request, lastRead: request,
						conditions: failedConditions(f, "OperationFailed", "registry.example is unreachable"),
						journal:    []operatorv1alpha1.JournalEntry{failure(20, claimFailure(f), "OperationFailed")},
					},
				}
			},
			wantStop: operate.StopReadFailed, wantMessage: "registry.example is unreachable",
		},
		{
			name: "read: a failure recorded before the request does not stop the wait", target: operate.TargetRead,
			readRequest: request,
			docs: func(f operate.Family) []doc {
				failed := []operatorv1alpha1.JournalEntry{failure(-30, "Resolve", "OperationFailed")}
				return []doc{
					{
						phase: "Failed", observedAt: at(0), annotation: request, lastRead: oldRequest,
						conditions: failedConditions(f, "OperationFailed", "registry.example is unreachable"), journal: failed,
					},
					{
						phase: "Failed", observedAt: at(0), annotation: request, lastRead: request,
						conditions: failedConditions(f, "OperationFailed", "registry.example is unreachable"), journal: failed,
					},
				}
			},
			wantTimeout: true,
		},
		{
			name: "read: another failed attempt folded into the same entry stops the wait", target: operate.TargetRead,
			readRequest: request,
			docs: func(f operate.Family) []doc {
				failed := []operatorv1alpha1.JournalEntry{failure(-30, claimFailure(f), "OperationFailed")}
				folded := failure(-30, claimFailure(f), "OperationFailed")
				folded.Count = 2
				folded.LastAt = &metav1.Time{Time: at(40)}
				conditions := failedConditions(f, "OperationFailed", "registry.example is unreachable")
				return []doc{
					{phase: "Failed", observedAt: at(0), annotation: request, lastRead: oldRequest, conditions: conditions, journal: failed},
					// Taken once the retry delay ended; the read it starts
					// fails the same way.
					{phase: "Failed", observedAt: at(0), annotation: request, lastRead: request, conditions: conditions, journal: failed},
					{phase: "Resolving", operation: "Resolve", observedAt: at(0), annotation: request, lastRead: request, journal: failed},
					{
						phase: "Failed", observedAt: at(0), annotation: request, lastRead: request,
						conditions: conditions, journal: []operatorv1alpha1.JournalEntry{folded},
					},
				}
			},
			wantStop: operate.StopReadFailed, wantMessage: "registry.example is unreachable",
		},
		{
			name: "read: a cycle under way when the request arrived is not its answer, the read it starts is", target: operate.TargetRead,
			readRequest: request,
			docs: func(f operate.Family) []doc {
				// The cycle had not reached the database when the request was
				// written; the operator still takes the request only after it
				// and answers with a read of its own.
				return []doc{
					{phase: "Resolving", operation: "Resolve", observedAt: at(0), annotation: request, lastRead: oldRequest},
					{phase: "InSync", observedAt: at(10), annotation: request, lastRead: oldRequest, conditions: inSyncConditions(f, 3)},
					{phase: "InSync", observedAt: at(10), annotation: request, lastRead: request, conditions: inSyncConditions(f, 3)},
					// Read again before the next claim: that cycle's reading
					// still does not count.
					{phase: "InSync", observedAt: at(10), annotation: request, lastRead: request, conditions: inSyncConditions(f, 3)},
					{phase: "Resolving", operation: "Resolve", observedAt: at(10), annotation: request, lastRead: request},
					{phase: "InSync", observedAt: at(40), annotation: request, lastRead: request, conditions: inSyncConditions(f, 3)},
				}
			},
			wantReads: 6,
		},
		{
			name: "read: a failure of the cycle under way when the request arrived does not stop the wait", target: operate.TargetRead,
			readRequest: request,
			docs: func(f operate.Family) []doc {
				failed := []operatorv1alpha1.JournalEntry{failure(20, claimFailure(f), "OperationFailed")}
				conditions := failedConditions(f, "OperationFailed", "registry.example is unreachable")
				return []doc{
					{phase: "Resolving", operation: "Resolve", observedAt: at(0), annotation: request, lastRead: oldRequest},
					// That cycle fails and leaves no claim. The operator takes
					// the request once the retry delay is over.
					{phase: "Failed", observedAt: at(0), annotation: request, lastRead: oldRequest, conditions: conditions, journal: failed},
					{phase: "Failed", observedAt: at(0), annotation: request, lastRead: request, conditions: conditions, journal: failed},
					{phase: "Resolving", operation: "Resolve", observedAt: at(0), annotation: request, lastRead: request, journal: failed},
					{phase: "InSync", observedAt: at(40), annotation: request, lastRead: request, conditions: inSyncConditions(f, 3), journal: failed},
				}
			},
			wantReads: 5,
		},
		{
			name: "read: a claim in flight after the operator took the request is its answer", target: operate.TargetRead,
			readRequest: request,
			since: func(f operate.Family) *doc {
				// The request was taken before this baseline was read, so the
				// read in flight began after it.
				phase, operation := readPhase(f)
				return &doc{phase: phase, operation: operation, observedAt: at(0), annotation: request, lastRead: request}
			},
			docs: func(f operate.Family) []doc {
				return []doc{{phase: "InSync", observedAt: at(10), annotation: request, lastRead: request, conditions: inSyncConditions(f, 3)}}
			},
			wantReads: 1,
		},
		{
			name: "read: a request replaced before it was taken stops the wait", target: operate.TargetRead,
			readRequest: request,
			since: func(operate.Family) *doc {
				return &doc{phase: "InSync", observedAt: at(0), annotation: request, lastRead: oldRequest}
			},
			docs: func(f operate.Family) []doc {
				return []doc{{phase: "InSync", observedAt: at(0), annotation: otherRequest, lastRead: oldRequest}}
			},
			wantStop: operate.StopReadReplaced, wantMessage: otherRequest,
		},

		// in-sync
		{
			name: "in-sync: a newer reading in sync at the generation is the target", target: operate.TargetInSync,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "InSync", observedAt: at(0), conditions: inSyncConditions(f, 3)},
					{phase: "Resolving", operation: "Resolve", observedAt: at(0), conditions: inSyncConditions(f, 3)},
					{phase: "InSync", observedAt: at(30), conditions: inSyncConditions(f, 3)},
				}
			},
			wantReads: 3,
		},
		{
			name: "in-sync: an old InSync with no newer reading is not the target", target: operate.TargetInSync,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "InSync", observedAt: at(0), conditions: inSyncConditions(f, 3)},
					{phase: "InSync", observedAt: at(0), conditions: inSyncConditions(f, 3)},
				}
			},
			wantTimeout: true, wantMessage: "a read of the database that began after the wait did (the last one is from 2026-10-01T09:00:00Z)",
		},
		{
			name: "in-sync: the read in flight when the wait began is not the target", target: operate.TargetInSync,
			docs: func(f operate.Family) []doc {
				phase, operation := readPhase(f)
				return []doc{
					// An interval read whose Job may already have read the
					// database before the change the wait is about.
					{phase: phase, operation: operation, observedAt: at(0), conditions: inSyncConditions(f, 3)},
					// Its harvest is stamped after the baseline. A migration's
					// History harvest writes InSync in the same write.
					{phase: "InSync", observedAt: at(10), conditions: inSyncConditions(f, 3)},
				}
			},
			wantTimeout: true, wantMessage: "a read of the database that began after the wait did (the last one is from 2026-10-01T09:00:10Z)",
		},
		{
			name: "in-sync: the read after the one in flight when the wait began is the target", target: operate.TargetInSync,
			docs: func(f operate.Family) []doc {
				phase, operation := readPhase(f)
				return []doc{
					{phase: phase, operation: operation, observedAt: at(0), conditions: inSyncConditions(f, 3)},
					{phase: "InSync", observedAt: at(10), conditions: inSyncConditions(f, 3)},
					{phase: "Resolving", operation: "Resolve", observedAt: at(10), conditions: inSyncConditions(f, 3)},
					{phase: "InSync", observedAt: at(40), conditions: inSyncConditions(f, 3)},
				}
			},
			wantReads: 4,
		},
		{
			name: "in-sync: a refresh's baseline with a read in flight does not let that read count", target: operate.TargetInSync,
			since: func(f operate.Family) *doc {
				phase, operation := readPhase(f)
				return &doc{phase: phase, operation: operation, observedAt: at(0), annotation: request, lastRead: oldRequest, conditions: inSyncConditions(f, 3)}
			},
			docs: func(f operate.Family) []doc {
				return []doc{{phase: "InSync", observedAt: at(10), annotation: request, lastRead: oldRequest, conditions: inSyncConditions(f, 3)}}
			},
			wantTimeout: true, wantMessage: "a read of the database that began after the wait did",
		},
		{
			name: "in-sync: a read in flight for an older generation is not skipped", target: operate.TargetInSync,
			generation: 4,
			docs: func(f operate.Family) []doc {
				// The read was claimed for generation 3. Its inputs no longer
				// hold, so the operator discards it, and the first reading at
				// generation 4 is a new read's.
				phase, operation := readPhase(f)
				return []doc{
					{generation: 4, observedGeneration: 3, phase: phase, operation: operation, observedAt: at(0), conditions: inSyncConditions(f, 3)},
					{generation: 4, observedGeneration: 4, phase: "InSync", observedAt: at(30), conditions: inSyncConditions(f, 4)},
				}
			},
			wantReads: 2,
		},
		{
			name: "in-sync: a reading at an older generation, or an older verdict, is not the target", target: operate.TargetInSync,
			generation: 4,
			docs: func(f operate.Family) []doc {
				return []doc{
					{generation: 4, observedGeneration: 3, phase: "InSync", observedAt: at(0), conditions: inSyncConditions(f, 3)},
					{generation: 4, observedGeneration: 3, phase: "InSync", observedAt: at(30), conditions: inSyncConditions(f, 3)},
					{generation: 4, observedGeneration: 4, phase: "InSync", observedAt: at(60), conditions: inSyncConditions(f, 3)},
					{generation: 4, observedGeneration: 4, phase: "InSync", observedAt: at(90), conditions: inSyncConditions(f, 4)},
				}
			},
			wantReads: 4,
		},
		{
			name: "in-sync: without a generation the baseline's is required", target: operate.TargetInSync,
			docs: func(f operate.Family) []doc {
				return []doc{
					{generation: 4, observedGeneration: 3, phase: "InSync", observedAt: at(0), conditions: inSyncConditions(f, 3)},
					{generation: 4, observedGeneration: 3, phase: "InSync", observedAt: at(30), conditions: inSyncConditions(f, 3)},
				}
			},
			wantTimeout: true, wantMessage: "the operator to observe generation 4 (status.observedGeneration is 3)",
		},

		// approval
		{
			name: "approval: a plan published after the wait began is the target", target: operate.TargetApproval,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "InSync", observedAt: at(0), conditions: inSyncConditions(f, 3)},
					{phase: "Resolving", operation: "Resolve", observedAt: at(0)},
					{phase: "AwaitingApproval", observedAt: at(30), plan: &planDoc{name: "plan-1", uid: "plan-uid-1"}, conditions: approvalConditions(metav1.ConditionTrue)},
				}
			},
			wantReads: 3,
		},
		{
			name: "approval: a plan whose approval is already recorded is not the target", target: operate.TargetApproval,
			only: operate.FamilySchema,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "InSync", observedAt: at(0), conditions: inSyncConditions(f, 3)},
					{phase: "AwaitingApproval", observedAt: at(30), plan: &planDoc{name: "plan-1", uid: "plan-uid-1", approved: true}, conditions: approvalConditions(metav1.ConditionTrue)},
				}
			},
			wantTimeout: true, wantMessage: "plan plan-1 already has one",
		},
		{
			name: "approval: the gate as it stood when the wait began is not the target, a newer reading is", target: operate.TargetApproval,
			docs: func(f operate.Family) []doc {
				waiting := doc{phase: "AwaitingApproval", observedAt: at(0), plan: &planDoc{name: "plan-1", uid: "plan-uid-1"}, conditions: approvalConditions(metav1.ConditionTrue)}
				reread := waiting
				reread.observedAt = at(600)
				return []doc{waiting, waiting, reread}
			},
			wantReads: 3,
		},
		{
			name: "approval: the gate as it stood when the wait began never becomes the target", target: operate.TargetApproval,
			docs: func(f operate.Family) []doc {
				return []doc{{phase: "AwaitingApproval", observedAt: at(0), plan: &planDoc{name: "plan-1", uid: "plan-uid-1"}, conditions: approvalConditions(metav1.ConditionTrue)}}
			},
			wantTimeout: true, wantMessage: "a read of the database that began after the wait did",
		},
		{
			name: "approval: a plan republished from the read in flight when the wait began is not the target", target: operate.TargetApproval,
			docs: func(f operate.Family) []doc {
				phase, operation := readPhase(f)
				plan := &planDoc{name: "plan-1", uid: "plan-uid-1"}
				return []doc{
					{phase: phase, operation: operation, observedAt: at(0), plan: plan},
					{phase: "AwaitingApproval", observedAt: at(10), plan: plan, conditions: approvalConditions(metav1.ConditionTrue)},
				}
			},
			wantTimeout: true, wantMessage: "a read of the database that began after the wait did",
		},
		{
			name: "approval: a new plan computed from the read in flight when the wait began is not the target", target: operate.TargetApproval,
			docs: func(f operate.Family) []doc {
				phase, operation := readPhase(f)
				return []doc{
					{phase: phase, operation: operation, observedAt: at(0)},
					{phase: "AwaitingApproval", observedAt: at(10), plan: &planDoc{name: "plan-2", uid: "plan-uid-2"}, conditions: approvalConditions(metav1.ConditionTrue)},
				}
			},
			wantTimeout: true, wantMessage: "a read of the database that began after the wait did",
		},
		{
			name: "approval: the plan from the read after the one in flight is the target", target: operate.TargetApproval,
			docs: func(f operate.Family) []doc {
				phase, operation := readPhase(f)
				plan := &planDoc{name: "plan-1", uid: "plan-uid-1"}
				return []doc{
					{phase: phase, operation: operation, observedAt: at(0), plan: plan},
					{phase: "AwaitingApproval", observedAt: at(10), plan: plan, conditions: approvalConditions(metav1.ConditionTrue)},
					{phase: "Resolving", operation: "Resolve", observedAt: at(10), plan: plan},
					{phase: "AwaitingApproval", observedAt: at(40), plan: plan, conditions: approvalConditions(metav1.ConditionTrue)},
				}
			},
			wantReads: 4,
		},
		{
			name: "approval: a gate with an operation in flight is not the target", target: operate.TargetApproval,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "InSync", observedAt: at(0)},
					{phase: "AwaitingApproval", operation: "Resolve", observedAt: at(30), plan: &planDoc{name: "plan-1", uid: "plan-uid-1"}, conditions: approvalConditions(metav1.ConditionTrue)},
				}
			},
			wantTimeout: true, wantMessage: "the Resolve operation (phase AwaitingApproval) to end",
		},
		{
			name: "approval: a gate that does not require approval is not the target", target: operate.TargetApproval,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "InSync", observedAt: at(0)},
					{phase: "AwaitingApproval", observedAt: at(30), plan: &planDoc{name: "plan-1", uid: "plan-uid-1"}, conditions: approvalConditions(metav1.ConditionFalse)},
				}
			},
			wantTimeout: true, wantMessage: "ApprovalRequired True",
		},
		{
			name: "approval: a migration with no history reading is not the target", target: operate.TargetApproval,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "Pending"},
					{phase: "AwaitingApproval", plan: &planDoc{name: "plan-1", uid: "plan-uid-1"}, conditions: approvalConditions(metav1.ConditionTrue)},
				}
			},
			wantTimeout: true, wantMessage: "a reading of the migration history",
		},

		// applied, PtahSchema
		{
			name: "applied: a schema Apply recorded after the wait began is the target", target: operate.TargetApplied,
			only: operate.FamilySchema,
			docs: func(f operate.Family) []doc {
				earlier := &appliedDoc{plan: "plan-0", uid: "plan-uid-0", fingerprint: "sha256:0", completedAt: at(-3600)}
				return []doc{
					{phase: "AwaitingApproval", observedAt: at(0), plan: &planDoc{name: "plan-1", uid: "plan-uid-1"}, applied: earlier},
					{phase: "Applying", operation: "Apply", observedAt: at(0), applied: earlier},
					{phase: "VerifyingConvergence", operation: "Observe", proof: true, observedAt: at(0), applied: earlier},
					{phase: "InSync", observedAt: at(90), applied: &appliedDoc{plan: "plan-1", uid: "plan-uid-1", fingerprint: "sha256:1", completedAt: at(90)}},
				}
			},
			wantReads: 4,
		},
		{
			name: "applied: the Apply recorded before the wait began is not the target", target: operate.TargetApplied,
			only: operate.FamilySchema,
			docs: func(f operate.Family) []doc {
				earlier := &appliedDoc{plan: "plan-0", uid: "plan-uid-0", fingerprint: "sha256:0", completedAt: at(-3600)}
				return []doc{
					{phase: "InSync", observedAt: at(0), applied: earlier},
					{phase: "InSync", observedAt: at(30), applied: earlier},
				}
			},
			wantTimeout: true, wantMessage: "an Apply that ends after the wait began",
		},
		{
			// The Apply ended before the wait did: the baseline owes its
			// proof. That proof writes status.applied after the wait began,
			// and a migration run recorded before the wait is not counted
			// when its read-back ends either.
			name: "applied: a schema Apply that ended before the wait began is not the target", target: operate.TargetApplied,
			only: operate.FamilySchema,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "VerifyingConvergence", operation: "Observe", dispatched: true, proof: true, proofPlan: "plan-1", observedAt: at(0)},
					{phase: "InSync", observedAt: at(90), applied: &appliedDoc{plan: "plan-1", uid: "plan-1-uid", fingerprint: "sha256:plan-1", completedAt: at(90)}},
				}
			},
			wantTimeout: true, wantMessage: "plan plan-1 was applied before it",
		},
		{
			name: "applied: the proof of an Apply that ended before the wait began does not stop it", target: operate.TargetApplied,
			only: operate.FamilySchema,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "VerifyingConvergence", operation: "Plan", dispatched: true, proof: true, proofPlan: "plan-1", observedAt: at(0)},
					{
						phase: "AwaitingApproval", observedAt: at(80),
						journal: []operatorv1alpha1.JournalEntry{
							{Type: operatorv1alpha1.JournalVerified, At: metav1.NewTime(at(80)), Plan: "plan-1", Outcome: operatorv1alpha1.JournalDriftRemains},
						},
					},
				}
			},
			wantTimeout: true, wantMessage: "an Apply that ends after the wait began",
		},
		{
			name: "applied: a schema Apply with an unknown outcome stops the wait", target: operate.TargetApplied,
			only: operate.FamilySchema,
			docs: func(f operate.Family) []doc {
				// finishUncertainApply: the claim retired, the proof owed,
				// and both entries in one write.
				published := operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalPlanPublished, At: metav1.NewTime(at(-10)), Plan: "plan-1"}
				return []doc{
					{phase: "AwaitingApproval", observedAt: at(0), journal: []operatorv1alpha1.JournalEntry{published}},
					{
						phase: "VerifyingConvergence", proof: true, observedAt: at(0),
						journal: []operatorv1alpha1.JournalEntry{
							published,
							{Type: operatorv1alpha1.JournalApprovalAccepted, At: metav1.NewTime(at(5)), Plan: "plan-1", Approval: "approval"},
							{Type: operatorv1alpha1.JournalApplyFinished, At: metav1.NewTime(at(60)), Plan: "plan-1", Outcome: operatorv1alpha1.JournalUnknown},
							failure(60, "Apply", "ApplyOutcomeUnknown"),
						},
					},
				}
			},
			wantStop: operate.StopUnresolved, wantMessage: "plan-1",
		},
		{
			name: "applied: a schema Apply whose proof found drift stops the wait", target: operate.TargetApplied,
			only: operate.FamilySchema,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "Applying", operation: "Apply", observedAt: at(0)},
					{
						phase: "AwaitingApproval", observedAt: at(80),
						journal: []operatorv1alpha1.JournalEntry{
							{Type: operatorv1alpha1.JournalApplyFinished, At: metav1.NewTime(at(60)), Plan: "plan-1", Outcome: operatorv1alpha1.JournalSucceeded},
							{Type: operatorv1alpha1.JournalVerified, At: metav1.NewTime(at(80)), Plan: "plan-1", Outcome: operatorv1alpha1.JournalDriftRemains},
						},
					},
				}
			},
			wantStop: operate.StopNotConverged, wantMessage: "still differs",
		},
		{
			name: "applied: outcomes the journal held before the wait began do not stop it", target: operate.TargetApplied,
			only: operate.FamilySchema,
			docs: func(f operate.Family) []doc {
				history := []operatorv1alpha1.JournalEntry{
					{Type: operatorv1alpha1.JournalApplyFinished, At: metav1.NewTime(at(-600)), Plan: "plan-0", Outcome: operatorv1alpha1.JournalUnknown},
					{Type: operatorv1alpha1.JournalVerified, At: metav1.NewTime(at(-500)), Plan: "plan-0", Outcome: operatorv1alpha1.JournalDriftRemains},
					failure(-600, "Apply", "ApplyOutcomeUnknown"),
				}
				return []doc{
					{phase: "AwaitingApproval", observedAt: at(0), journal: history},
					{phase: "Applying", operation: "Apply", observedAt: at(0), journal: history},
				}
			},
			wantTimeout: true,
		},
		{
			name: "applied: an Apply Pod admission refuses leaves the wait running, and the timeout names it", target: operate.TargetApplied,
			docs: func(f operate.Family) []doc {
				// reportPodAdmission keeps the claim in flight and moves Ready
				// (schema) or Progressing (migration); it journals nothing.
				refusal := `pods "apply-1" is forbidden: violates PodSecurity "restricted:latest"`
				conditionType := operatorv1alpha1.ConditionReady
				if f == operate.FamilyMigration {
					conditionType = operatorv1alpha1.ConditionMigrationProgressing
				}
				return []doc{
					{phase: "AwaitingApproval", observedAt: at(0)},
					{
						phase: "Applying", operation: "Apply", observedAt: at(0),
						conditions: []metav1.Condition{condition(conditionType, metav1.ConditionFalse, "PodAdmissionRefused", refusal, 3)},
					},
				}
			},
			wantTimeout: true, wantMessage: `(PodAdmissionRefused): pods "apply-1" is forbidden`,
		},
		{
			name: "applied: a schema Apply whose dispatch was refused keeps its claim, and the timeout names it", target: operate.TargetApplied,
			only: operate.FamilySchema,
			docs: func(f operate.Family) []doc {
				// schemaDispatch.Refuse for a mutating claim: operationFailure
				// writes Failed and leaves the Apply claim to be tried again.
				return []doc{
					{phase: "AwaitingApproval", observedAt: at(0)},
					{
						phase: "Failed", operation: "Apply", observedAt: at(0),
						conditions: []metav1.Condition{
							condition(operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionTrue, "ConfigurationError", "the Apply Job template could not be read", 3),
							condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "ConfigurationError", "Reconciliation did not complete", 3),
						},
						journal: []operatorv1alpha1.JournalEntry{failure(30, "Apply", "ConfigurationError")},
					},
				}
			},
			wantTimeout: true, wantMessage: "ReconciliationFailed=True (ConfigurationError): the Apply Job template could not be read",
		},
		{
			name: "applied: a migration Apply the operator gave up before it ran stops the wait", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				// migrationOperationFailure retires the Apply claim and
				// journals the failure under it.
				return []doc{
					{phase: "AwaitingApproval", observedAt: at(0)},
					{
						phase: "Failed", observedAt: at(0), conditions: failedConditions(f, "OperationFailed", "build Apply Job: the runner image is not configured"),
						journal: []operatorv1alpha1.JournalEntry{failure(30, "Apply", "OperationFailed")},
					},
				}
			},
			wantStop: operate.StopApplyFailed, wantMessage: "the runner image is not configured",
		},
		{
			name: "applied: a migration Apply given up and claimed again before the wait saw it keeps the wait", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				// The Failed document fell between two reads; by the next one
				// the operator has claimed the Apply again, and that run
				// applies.
				failed := []operatorv1alpha1.JournalEntry{failure(30, "Apply", "OperationFailed")}
				run := &runDoc{outcome: "Applied", job: "run-2", jobUID: "job-uid-2", startedAt: at(60), finishedAt: at(80)}
				return []doc{
					{phase: "AwaitingApproval", observedAt: at(0)},
					{phase: "Applying", operation: "Apply", observedAt: at(0), journal: failed},
					{phase: "VerifyingHistory", observedAt: at(0), lastRun: run, journal: failed},
					{phase: "InSync", observedAt: at(90), lastRun: run, conditions: inSyncConditions(f, 3), journal: failed},
				}
			},
			wantReads: 4,
		},
		{
			name: "applied: a migration Apply given up again, folded into the entry before the wait, stops it", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				folded := failure(-400, "Apply", "OperationFailed")
				folded.Count = 2
				folded.LastAt = &metav1.Time{Time: at(30)}
				conditions := failedConditions(f, "OperationFailed", "build Apply Job: the runner image is not configured")
				earlier := []operatorv1alpha1.JournalEntry{failure(-400, "Apply", "OperationFailed")}
				return []doc{
					{phase: "Failed", observedAt: at(0), conditions: conditions, journal: earlier},
					{phase: "Applying", operation: "Apply", observedAt: at(0), journal: earlier},
					{phase: "Failed", observedAt: at(0), conditions: conditions, journal: []operatorv1alpha1.JournalEntry{folded}},
				}
			},
			wantStop: operate.StopApplyFailed, wantMessage: "the runner image is not configured",
		},
		{
			name: "applied: another plan applied than the one named stops the wait", target: operate.TargetApplied,
			only: operate.FamilySchema, plan: "plan-1",
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "AwaitingApproval", observedAt: at(0)},
					{phase: "InSync", observedAt: at(90), applied: &appliedDoc{plan: "plan-9", uid: "plan-uid-9", fingerprint: "sha256:9", completedAt: at(90)}},
				}
			},
			wantStop: operate.StopOtherPlanApplied, wantMessage: "plan plan-9 was applied, not plan-1",
		},
		{
			name: "applied: a plan named by its fingerprint is the target", target: operate.TargetApplied,
			only: operate.FamilySchema, plan: "sha256:1",
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "AwaitingApproval", observedAt: at(0)},
					{phase: "InSync", observedAt: at(90), applied: &appliedDoc{plan: "plan-1", uid: "plan-uid-1", fingerprint: "sha256:1", completedAt: at(90)}},
				}
			},
			wantReads: 2,
		},

		// applied, PtahMigration
		{
			name: "applied: a migration run that finished applied after the wait began is the target", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				earlier := &runDoc{outcome: "Applied", job: "run-0", jobUID: "job-uid-0", startedAt: at(-3700), finishedAt: at(-3600)}
				run := &runDoc{outcome: "Applied", job: "run-1", jobUID: "job-uid-1", startedAt: at(40), finishedAt: at(80)}
				return []doc{
					{phase: "AwaitingApproval", observedAt: at(0), plan: &planDoc{name: "plan-1", uid: "plan-uid-1"}, lastRun: earlier},
					{phase: "Applying", operation: "Apply", observedAt: at(0), lastRun: earlier},
					// recordMigrationRun: the run's own account and
					// VerifyingHistory in one write, the claim retired.
					{phase: "VerifyingHistory", observedAt: at(0), lastRun: run},
					{phase: "VerifyingHistory", operation: "History", observedAt: at(0), lastRun: run},
					// The history read back settles it.
					{phase: "InSync", observedAt: at(90), lastRun: run, conditions: inSyncConditions(f, 3)},
				}
			},
			wantReads: 5,
		},
		{
			name: "applied: a run's own account is not the target before the history is read back", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				run := &runDoc{outcome: "Applied", job: "run-1", jobUID: "job-uid-1", startedAt: at(40), finishedAt: at(80)}
				return []doc{
					{phase: "Applying", operation: "Apply", observedAt: at(0)},
					{phase: "VerifyingHistory", observedAt: at(0), lastRun: run},
				}
			},
			wantTimeout: true, wantMessage: "the history read back after run run-1; now phase VerifyingHistory",
		},
		{
			name: "applied: a read back that finds more pending settles once the next plan is published", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				// The artifact moved on while the run ran. The history harvest
				// writes Planning with MigrationsPending before it publishes
				// the plan for what is still pending.
				run := &runDoc{outcome: "Applied", job: "run-1", jobUID: "job-uid-1", startedAt: at(40), finishedAt: at(80)}
				return []doc{
					{phase: "Applying", operation: "Apply", observedAt: at(0)},
					{phase: "VerifyingHistory", observedAt: at(0), lastRun: run},
					{phase: "Planning", observedAt: at(90), lastRun: run, conditions: []metav1.Condition{
						condition(operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue, "MigrationsPending",
							"A plan for 1 pending migrations is being published", 3),
					}},
					{phase: "AwaitingApproval", observedAt: at(90), lastRun: run, plan: &planDoc{name: "plan-2", uid: "plan-uid-2"}, conditions: approvalConditions(metav1.ConditionTrue)},
				}
			},
			wantReads: 4,
		},
		{
			name: "applied: a history read that failed after an applied run is not the target", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				// The read back could not be dispatched: the resource rests in
				// Failed with the reading from before the run.
				run := &runDoc{outcome: "Applied", job: "run-1", jobUID: "job-uid-1", startedAt: at(40), finishedAt: at(80)}
				return []doc{
					{phase: "Applying", operation: "Apply", observedAt: at(0)},
					{phase: "VerifyingHistory", observedAt: at(0), lastRun: run},
					{
						phase: "Failed", observedAt: at(0), lastRun: run,
						conditions: failedConditions(f, "OperationFailed", "build History Job: the runner image is not configured"),
						journal:    []operatorv1alpha1.JournalEntry{failure(90, "History", "OperationFailed")},
					},
				}
			},
			wantTimeout: true, wantMessage: "the history read back after run run-1; now phase Failed",
		},
		{
			name: "applied: a history read back blocked after an applied run stops the wait", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				run := &runDoc{outcome: "Applied", job: "run-1", jobUID: "job-uid-1", startedAt: at(40), finishedAt: at(80)}
				return []doc{
					{phase: "Applying", operation: "Apply", observedAt: at(0)},
					{phase: "VerifyingHistory", observedAt: at(0), lastRun: run},
					{phase: "Blocked", observedAt: at(90), lastRun: run, conditions: []metav1.Condition{
						condition(operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "HistoryDirty", "Revision 7 is recorded dirty", 3),
					}},
				}
			},
			wantStop: operate.StopNotConverged, wantMessage: "Revision 7 is recorded dirty",
		},
		{
			name: "applied: the run that finished before the wait began is not the target", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				earlier := &runDoc{outcome: "Applied", job: "run-0", jobUID: "job-uid-0", startedAt: at(-3700), finishedAt: at(-3600)}
				return []doc{
					{phase: "InSync", observedAt: at(0), lastRun: earlier},
					{phase: "InSync", observedAt: at(30), lastRun: earlier},
				}
			},
			wantTimeout: true, wantMessage: "a run that finishes after the wait began",
		},
		{
			// The schema rule's twin: the run ended before the wait began,
			// and its read-back ends after.
			name: "applied: a migration run that ended before the wait began is not the target", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				run := &runDoc{outcome: "Applied", job: "run-1", jobUID: "job-uid-1", startedAt: at(-60), finishedAt: at(-10)}
				return []doc{
					{phase: "VerifyingHistory", operation: "History", dispatched: true, observedAt: at(-100), lastRun: run},
					{phase: "InSync", observedAt: at(30), lastRun: run, conditions: inSyncConditions(f, 3)},
				}
			},
			wantTimeout: true, wantMessage: "a run that finishes after the wait began",
		},
		{
			name: "applied: an unresolved migration run stops the wait", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "Applying", operation: "Apply", observedAt: at(0)},
					{
						phase: "Blocked", observedAt: at(0),
						lastRun:    &runDoc{outcome: "Partial", job: "run-1", jobUID: "job-uid-1", startedAt: at(40), finishedAt: at(80)},
						unresolved: &unresolvedDoc{operationID: "sha256:op", outcome: "Partial", job: "run-1", plan: "plan-1"},
					},
				}
			},
			wantStop: operate.StopUnresolved, wantMessage: "ended Partial",
		},
		{
			name: "applied: an unresolved run recorded before the wait began does not stop it", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				blocked := doc{
					phase: "Blocked", observedAt: at(0),
					lastRun:    &runDoc{outcome: "Unknown", job: "run-1", jobUID: "job-uid-1", startedAt: at(-100), finishedAt: at(-80)},
					unresolved: &unresolvedDoc{operationID: "sha256:op", outcome: "Unknown", job: "run-1", plan: "plan-1"},
				}
				return []doc{blocked}
			},
			wantTimeout: true,
		},
		{
			name: "applied: a failed migration run stops the wait", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "Applying", operation: "Apply", observedAt: at(0)},
					{phase: "VerifyingHistory", observedAt: at(0), lastRun: &runDoc{outcome: "Failed", job: "run-1", jobUID: "job-uid-1", startedAt: at(40), finishedAt: at(80), message: "migration 7 failed at statement 2 of 3"}},
				}
			},
			wantStop: operate.StopApplyFailed, wantMessage: "migration 7 failed at statement 2 of 3",
		},
		{
			name: "applied: a migration run that found nothing to apply stops the wait", target: operate.TargetApplied,
			only: operate.FamilyMigration,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "Applying", operation: "Apply", observedAt: at(0)},
					{phase: "VerifyingHistory", observedAt: at(0), lastRun: &runDoc{outcome: "UpToDate", job: "run-1", jobUID: "job-uid-1", startedAt: at(40), finishedAt: at(80)}},
				}
			},
			wantStop: operate.StopNothingApplied,
		},

		// suspended
		{
			// What the controller writes after a suspension that met a running
			// Apply. The suspension moved the generation the Apply was decided
			// from, so the harvest records it unknown: a schema owes a proof
			// and keeps status.observedGeneration at the Apply's generation,
			// and writes Suspended with the condition for the new one
			// (reconcilePendingObservation); a migration records the run
			// unresolved and observes the new generation.
			name: "suspended: Suspended for the generation of the change, once the Apply in flight ended", target: operate.TargetSuspended,
			generation: 5,
			docs: func(f operate.Family) []doc {
				applying := doc{generation: 5, observedGeneration: 4, suspend: true, phase: "Applying", operation: "Apply", dispatched: true, observedAt: at(0)}
				if f == operate.FamilyMigration {
					return []doc{applying, applying, {
						generation: 5, observedGeneration: 5, suspend: true, phase: "Suspended", observedAt: at(0),
						conditions: suspendedConditions(f, 5),
						unresolved: &unresolvedDoc{operationID: "attempt", outcome: "Unknown", job: "job", plan: "plan-1"},
					}}
				}
				return []doc{
					applying,
					{generation: 5, observedGeneration: 4, suspend: true, phase: "VerifyingConvergence", proof: true, proofPlan: "plan-1", observedAt: at(0)},
					{generation: 5, observedGeneration: 4, suspend: true, phase: "Suspended", proof: true, proofPlan: "plan-1", observedAt: at(0), conditions: suspendedConditions(f, 5)},
				}
			},
			wantReads: 3,
		},
		{
			// A resource whose spec changed while it was suspended: Suspended,
			// for the generation before.
			name: "suspended: Suspended at an older generation is not the target", target: operate.TargetSuspended,
			docs: func(f operate.Family) []doc {
				return []doc{{generation: 5, observedGeneration: 4, suspend: true, phase: "Suspended", observedAt: at(0), conditions: suspendedConditions(f, 4)}}
			},
			wantTimeout: true, wantMessage: "generation 5",
		},
		{
			name: "suspended: an Apply in flight is observed to its end first", target: operate.TargetSuspended,
			docs: func(f operate.Family) []doc {
				return []doc{{generation: 5, observedGeneration: 5, suspend: true, phase: "Applying", operation: "Apply", dispatched: true, observedAt: at(0)}}
			},
			wantTimeout: true, wantMessage: "the Apply operation (phase Applying) to end: suspension starts no new Job",
		},
		{
			// A suspension that reached the schema after its Apply was
			// harvested and before the proof was claimed, or while the
			// proof's Observe ran: reconcilePendingObservation writes
			// Suspended and the condition, and leaves observedGeneration at
			// the Apply's generation, where the proof keeps it.
			name: "suspended: a schema whose post-Apply proof waits for resume is suspended", target: operate.TargetSuspended,
			only:       operate.FamilySchema,
			generation: 5,
			docs: func(f operate.Family) []doc {
				return []doc{
					{generation: 5, observedGeneration: 4, suspend: true, phase: "VerifyingConvergence", proof: true, proofPlan: "plan-1", observedAt: at(0)},
					{generation: 5, observedGeneration: 4, suspend: true, phase: "Suspended", proof: true, proofPlan: "plan-1", observedAt: at(0), conditions: suspendedConditions(f, 5)},
				}
			},
			wantReads: 2,
		},
		{
			name: "in-sync: a schema whose post-Apply proof is still owed is not the target", target: operate.TargetInSync,
			only: operate.FamilySchema,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "InSync", observedAt: at(0), conditions: inSyncConditions(f, 3)},
					{phase: "InSync", proof: true, observedAt: at(30), conditions: inSyncConditions(f, 3)},
				}
			},
			wantTimeout: true, wantMessage: "nothing in flight: the post-Apply proof (phase InSync)",
		},
		{
			name: "any target: a resource being deleted stops the wait", target: operate.TargetInSync,
			docs: func(f operate.Family) []doc {
				return []doc{
					{phase: "InSync", observedAt: at(0), conditions: inSyncConditions(f, 3)},
					{deleting: true, phase: "InSync", observedAt: at(0), conditions: inSyncConditions(f, 3)},
				}
			},
			wantStop: operate.StopDeleting, wantMessage: "metadata.deletionTimestamp is set",
		},
		{
			name: "suspended: a resource whose spec does not ask for it stops the wait", target: operate.TargetSuspended,
			docs: func(f operate.Family) []doc {
				return []doc{{phase: "InSync", observedAt: at(0), conditions: inSyncConditions(f, 3)}}
			},
			wantStop: operate.StopNotSuspended, wantMessage: "spec.suspend is false",
		},
	}

	for _, row := range rows {
		for _, family := range families {
			if row.only != "" && row.only != family {
				continue
			}
			t.Run(string(family)+"/"+row.name, func(t *testing.T) {
				t.Parallel()

				tl := &timeline{family: family, docs: row.docs(family)}
				options := operate.WaitOptions{
					Target: row.target, Timeout: timeout, Interval: interval, Clock: newStepClock(),
					Generation: row.generation, ReadRequest: row.readRequest, Plan: row.plan,
				}
				if row.since != nil {
					options.Since = row.since(family).object(family)
				}
				result, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), options)
				switch {
				case row.wantReads > 0:
					if err != nil {
						t.Fatalf("wait: %v", err)
					}
					if result.Reads != row.wantReads || tl.readCount() != row.wantReads {
						t.Fatalf("met the target at read %d (served %d), want read %d", result.Reads, tl.readCount(), row.wantReads)
					}
					if result.Target != row.target || result.Object == nil {
						t.Fatalf("result = %+v", result)
					}
				case row.wantStop != "":
					var stop *operate.StopError
					if !errors.As(err, &stop) {
						t.Fatalf("err = %v, want a StopError", err)
					}
					if stop.Reason != row.wantStop || !strings.Contains(stop.Error(), row.wantMessage) {
						t.Fatalf("stop = %v, want reason %s naming %q", stop, row.wantStop, row.wantMessage)
					}
				case row.wantTimeout:
					var timedOut *operate.TimeoutError
					if !errors.As(err, &timedOut) {
						t.Fatalf("err = %v, result = %+v, want a TimeoutError", err, result)
					}
					if !strings.Contains(timedOut.Error(), row.wantMessage) {
						t.Fatalf("timeout = %q, want it to name %q", timedOut, row.wantMessage)
					}
					// Every read up to the timeout was refused, the last
					// documents included.
					if want := int(timeout/interval) + 1; tl.readCount() != want {
						t.Fatalf("read %d times before the timeout, want %d", tl.readCount(), want)
					}
				default:
					t.Fatal("row expects nothing")
				}
			})
		}
	}
}

func TestWaitTimeoutNamesPhaseBlockerAndWhatIsMissing(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()

			conditions := failedConditions(family, "OperationFailed", "dial tcp 10.0.0.7:5432: connection refused")
			if family == operate.FamilyMigration {
				conditions = append(conditions, condition(operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
					"HistoryDirty", "Revision 7 is recorded dirty", 3))
			}
			tl := &timeline{family: family, docs: []doc{{phase: "Failed", observedAt: at(0), conditions: conditions}}}
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
				Target: operate.TargetInSync, Timeout: timeout, Interval: 3 * time.Second, Clock: newStepClock(),
			})
			var timedOut *operate.TimeoutError
			if !errors.As(err, &timedOut) {
				t.Fatalf("err = %v, want a TimeoutError", err)
			}
			wantBlocker := "dial tcp 10.0.0.7:5432: connection refused"
			if family == operate.FamilyMigration {
				wantBlocker = "Revision 7 is recorded dirty"
			}
			if timedOut.Phase != "Failed" || timedOut.Blocker == nil || timedOut.Blocker.Message != wantBlocker ||
				timedOut.Waited != timeout || timedOut.Waiting == "" {
				t.Fatalf("timeout = %+v", timedOut)
			}
			for _, part := range []string{"phase Failed", wantBlocker, "still waiting for", "ptah" + strings.ToLower(string(family)) + "/storefront"} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("%q does not name %q", err, part)
				}
			}
			// Reads at 0, 3, 6 and 9 seconds, and one at the deadline.
			if tl.readCount() != 5 {
				t.Fatalf("read %d times, want 5", tl.readCount())
			}
		})
	}
}

func TestWaitRefusesAResourceReplacedUnderItsName(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()

			tl := &timeline{family: family, docs: []doc{
				{phase: "InSync", observedAt: at(0)},
				{uid: uidB, phase: "InSync", observedAt: at(30), conditions: inSyncConditions(family, 3)},
			}}
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
				Target: operate.TargetInSync, Timeout: timeout, Interval: interval, Clock: newStepClock(),
			})
			if !errors.Is(err, operate.ErrReplaced) || tl.readCount() != 2 {
				t.Fatalf("err = %v after %d reads, want ErrReplaced after 2", err, tl.readCount())
			}
		})
	}
}

func TestWaitErrorsFromTheAPIServer(t *testing.T) {
	t.Parallel()

	suspended := doc{suspend: true, phase: "Suspended", observedAt: at(0), conditions: suspendedConditions(operate.FamilySchema, 3)}
	rows := []struct {
		name      string
		errs      map[int]error
		wantReads int
		check     func(error) bool
	}{
		{
			name: "a resource that is gone ends the wait", errs: map[int]error{1: apierrors.NewNotFound(resource, name)},
			wantReads: 2, check: apierrors.IsNotFound,
		},
		{
			name:      "a read RBAC refuses ends the wait with the API server's message",
			errs:      map[int]error{0: apierrors.NewForbidden(resource, name, errors.New(`user "dev" cannot get resource`))},
			wantReads: 1, check: func(err error) bool {
				return apierrors.IsForbidden(err) && strings.Contains(err.Error(), `user "dev" cannot get resource`)
			},
		},
		{
			name: "a kind the API server does not serve ends the wait",
			errs: map[int]error{0: &meta.NoKindMatchError{
				GroupKind: schema.GroupKind{Group: "operator.ptah.run", Kind: "PtahSchema"}, SearchedVersions: []string{"v1alpha1"},
			}},
			wantReads: 1, check: meta.IsNoMatchError,
		},
		{
			name: "a kind this client cannot encode ends the wait",
			errs: map[int]error{0: runtime.NewNotRegisteredErrForKind("operate", schema.GroupVersionKind{
				Group: "operator.ptah.run", Version: "v1alpha1", Kind: "PtahSchema",
			})},
			wantReads: 1, check: func(err error) bool {
				for ; err != nil; err = errors.Unwrap(err) {
					if runtime.IsNotRegisteredError(err) {
						return true
					}
				}
				return false
			},
		},
		{
			name:      "a read the server may answer next time is retried",
			errs:      map[int]error{0: apierrors.NewServerTimeout(resource, "get", 1), 1: apierrors.NewTooManyRequests("slow down", 1)},
			wantReads: 3, check: func(err error) bool { return err == nil },
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			tl := &timeline{family: operate.FamilySchema, docs: []doc{suspended}, errs: row.errs}
			if row.wantReads == 2 {
				// The resource has to be read once before it can vanish.
				tl.docs = []doc{{suspend: true, phase: "Applying", operation: "Apply"}, suspended}
			}
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilySchema), operate.WaitOptions{
				Target: operate.TargetSuspended, Timeout: timeout, Interval: interval, Clock: newStepClock(),
			})
			if !row.check(err) || tl.readCount() != row.wantReads {
				t.Fatalf("err = %v after %d reads, want %d reads", err, tl.readCount(), row.wantReads)
			}
		})
	}

	t.Run("a timeout after failed reads names the last failure", func(t *testing.T) {
		t.Parallel()

		errs := map[int]error{}
		for read := range 20 {
			errs[read] = apierrors.NewServiceUnavailable("the API server is restarting")
		}
		tl := &timeline{family: operate.FamilySchema, docs: []doc{suspended}, errs: errs}
		_, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilySchema), operate.WaitOptions{
			Target: operate.TargetSuspended, Timeout: timeout, Interval: interval, Clock: newStepClock(),
		})
		var timedOut *operate.TimeoutError
		if !errors.As(err, &timedOut) || timedOut.Phase != "" || !apierrors.IsServiceUnavailable(timedOut.LastError) ||
			!strings.Contains(err.Error(), "the API server is restarting") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestWaitRefusesOptionsItCannotHonor(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name    string
		family  operate.Family
		options operate.WaitOptions
		want    string
	}{
		{name: "no timeout", options: operate.WaitOptions{Target: operate.TargetInSync}, want: "timeout"},
		{name: "unknown target", options: operate.WaitOptions{Target: "ready", Timeout: timeout}, want: "unknown wait target"},
		{name: "an empty target", options: operate.WaitOptions{Timeout: timeout}, want: "unknown wait target"},
		{name: "a read without its request", options: operate.WaitOptions{Target: operate.TargetRead, Timeout: timeout}, want: "read request value"},
		{
			name:    "a request that is not an instant starts no read",
			options: operate.WaitOptions{Target: operate.TargetRead, Timeout: timeout, ReadRequest: "now"}, want: "not an RFC 3339 instant",
		},
		{
			name:    "a request with another target",
			options: operate.WaitOptions{Target: operate.TargetInSync, Timeout: timeout, ReadRequest: request}, want: "only waited on",
		},
		{
			name: "a plan for a migration", family: operate.FamilyMigration,
			options: operate.WaitOptions{Target: operate.TargetApplied, Timeout: timeout, Plan: "plan-1"}, want: "records no plan",
		},
		{
			name: "a read baseline from before the request was written",
			options: operate.WaitOptions{
				Target: operate.TargetRead, Timeout: timeout, ReadRequest: request,
				Since: doc{phase: "InSync", annotation: oldRequest}.object(operate.FamilySchema),
			},
			want: "read before read request",
		},
		{
			name: "a baseline of the other family",
			options: operate.WaitOptions{
				Target: operate.TargetInSync, Timeout: timeout, Since: doc{phase: "InSync"}.object(operate.FamilyMigration),
			},
			want: "baseline is a migration",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			family := row.family
			if family == "" {
				family = operate.FamilySchema
			}
			tl := &timeline{family: family, docs: []doc{{phase: "InSync"}}}
			row.options.Clock = newStepClock()
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), row.options)
			if err == nil || !strings.Contains(err.Error(), row.want) || tl.readCount() != 0 {
				t.Fatalf("err = %v after %d reads, want %q before any read", err, tl.readCount(), row.want)
			}
		})
	}
}

func TestWaitReadsTheTargetAsACommandLineSpellsIt(t *testing.T) {
	t.Parallel()

	// A read target spelled in capitals is still a wait for a read, not
	// for whatever predicate a case-sensitive comparison falls through to.
	tl := &timeline{family: operate.FamilySchema, docs: []doc{
		{phase: "InSync", observedAt: at(0), annotation: request, lastRead: oldRequest},
		{phase: "InSync", observedAt: at(30), annotation: request, lastRead: request},
	}}
	result, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilySchema), operate.WaitOptions{
		Target: "READ", Timeout: timeout, Interval: interval, Clock: newStepClock(), ReadRequest: request,
	})
	if err != nil || result.Target != operate.TargetRead || result.Reads != 2 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestWaitWithTheRealClockBoundsAReadThatHangs(t *testing.T) {
	t.Parallel()

	reader := fake.NewClientBuilder().WithScheme(newScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}).Build()
	began := time.Now()
	_, err := operate.Wait(context.Background(), reader, refOf(operate.FamilySchema), operate.WaitOptions{
		Target: operate.TargetInSync, Timeout: 200 * time.Millisecond, Interval: time.Hour,
	})
	elapsed := time.Since(began)
	var timedOut *operate.TimeoutError
	if !errors.As(err, &timedOut) || !errors.Is(timedOut.LastError, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a TimeoutError after the read hit the deadline", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("a read that hangs held the wait %s past a 200ms timeout", elapsed)
	}
}

func TestWaitStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tl := &timeline{family: operate.FamilySchema, docs: []doc{{phase: "InSync", observedAt: at(0)}}}
	_, err := operate.Wait(ctx, tl.reader(t), refOf(operate.FamilySchema), operate.WaitOptions{
		Target: operate.TargetInSync, Timeout: timeout, Interval: interval, Clock: blockedClock{},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// blockedClock never wakes a sleep, so only the context can end it.
type blockedClock struct{}

func (blockedClock) Now() time.Time                       { return clockStart }
func (blockedClock) After(time.Duration) <-chan time.Time { return nil }

// A timeout says whether a read of the database was in flight when the wait
// gave up: a wait begun after it would skip that read's harvest, so the
// person is better served by a read request than by waiting again.
func TestWaitTimeoutSaysWhetherAReadWasUnderWay(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		phase, operation := readPhase(family)
		for _, row := range []struct {
			name  string
			last  doc
			under bool
		}{
			{"a read in flight", doc{phase: phase, operation: operation, dispatched: true, observedAt: at(0)}, true},
			{"a cycle resting", doc{phase: "AwaitingApproval", observedAt: at(0), conditions: approvalConditions(metav1.ConditionTrue)}, false},
			{"a claim that reads no database", doc{phase: "Resolving", operation: "Resolve", observedAt: at(0)}, false},
		} {
			t.Run(string(family)+"/"+row.name, func(t *testing.T) {
				t.Parallel()

				tl := &timeline{family: family, docs: []doc{row.last}}
				_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
					Target: operate.TargetInSync, Timeout: timeout, Interval: interval, Clock: newStepClock(),
				})
				var timedOut *operate.TimeoutError
				if !errors.As(err, &timedOut) || timedOut.ReadUnderWay != row.under ||
					strings.Contains(err.Error(), "a read of the database was under way") != row.under {
					t.Fatalf("err = %v, want a timeout with ReadUnderWay %t", err, row.under)
				}
			})
		}
	}
}
