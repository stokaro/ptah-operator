package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The Apply the fixture owes a proof for, the target its plan was made for,
// the one the URL Secret names now, and one a later read names.
var (
	settledApply = "sha256:" + strings.Repeat("1", 64)
	planTarget   = "sha256:" + strings.Repeat("a", 64)
	movedTo      = "sha256:" + strings.Repeat("b", 64)
	movedAgain   = "sha256:" + strings.Repeat("c", 64)
)

func settleValueOn(target string) string { return settledApply + "/" + target }

// stopOn is the message of a stop on target, as the controller writes it.
func stopOn(target string) string {
	return "the URL Secret now holds another target than the plan this Apply ran from: another login, host, port, " +
		"database or URL parameter, so no read can say what the Apply did; put the first URL back and the proof " +
		"finishes, or give the proof up, which reads the database under the new URL and, under apply: Always, applies " +
		"the next plan with no approval unless it is destructive or changes privileges: kubectl annotate ptahschema " +
		"storefront -n demo " + operatorv1alpha1.SettledApplyAnnotation + "=" + settleValueOn(target) + " --overwrite"
}

// stoppedOnMove is the resting schema after an Apply of plan ptah-plan-aaa
// whose proof read target and stopped there.
func stoppedOnMove(target string, change ...func(*operatorv1alpha1.PtahSchema)) client.Object {
	schema := resting("schema").(*operatorv1alpha1.PtahSchema)
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	schema.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: 45 * time.Second}
	status := &schema.Status
	status.Phase = operatorv1alpha1.PhaseBlocked
	status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
		Outcome: operatorv1alpha1.PendingObservationApplySucceeded, ApplyOperationID: settledApply,
		Plan: operatorv1alpha1.CurrentPlanStatus{Name: "ptah-plan-aaa", UID: "plan-uid", TargetIdentityDigest: planTarget},
	}
	status.Plan = &operatorv1alpha1.CurrentPlanStatus{Name: "ptah-plan-aaa", UID: "plan-uid",
		Approval: &operatorv1alpha1.ConsumedApprovalStatus{Name: "approve-aaa", UID: "approve-aaa-uid"}}
	status.Conditions = []metav1.Condition{
		{Type: operatorv1alpha1.ConditionReconciliationFailed, Status: metav1.ConditionTrue,
			Reason: string(operatorv1alpha1.ReasonTargetIdentityChanged), Message: stopOn(target), ObservedGeneration: 4},
		{Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionFalse,
			Reason: string(operatorv1alpha1.ReasonTargetIdentityChanged), Message: "ReconciliationFailed says what to do", ObservedGeneration: 4},
	}
	status.Journal = &operatorv1alpha1.LifecycleJournal{CoveredSince: metav1.NewTime(observedAt.Add(-time.Hour)),
		Entries: []operatorv1alpha1.JournalEntry{{Type: operatorv1alpha1.JournalApplyFinished,
			At: metav1.NewTime(observedAt.Add(-10 * time.Minute)), Plan: "ptah-plan-aaa", Outcome: operatorv1alpha1.JournalSucceeded}}}
	for _, apply := range change {
		apply(schema)
	}
	return schema
}

func settledAnnotation(t *testing.T, api *cluster) (string, bool) {
	t.Helper()
	value, held := stored(t, api, "schema").GetAnnotations()[operatorv1alpha1.SettledApplyAnnotation]
	return value, held
}

func TestSettleWritesTheValueItsStopPrintsAndSaysWhatItGivesUp(t *testing.T) {
	api := newCluster(t, stoppedOnMove(movedTo))

	code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront")

	if code != exitOK || stderr != "" {
		t.Fatalf("settle = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireSays(t, "settle's report", stdout,
		"Settlement requested for ptahschema/storefront in namespace demo.",
		"Wrote:    "+operatorv1alpha1.SettledApplyAnnotation+"="+settleValueOn(movedTo),
		"Apply:    "+settledApply+", plan ptah-plan-aaa",
		"Journal:  ApplyFinished Succeeded at 2026-10-01T08:50:00Z",
		"Target:   the read named "+movedTo+"; the plan was made for "+planTarget,
		"Observed: phase Blocked",
		"It Succeeded, as the journal records: it reported its statements done there",
		"Taking the statement gives up the proof, the plan ptah-plan-aaa and the database lock in one status write; "+
			"the approval approve-aaa it consumed goes Stale.",
		"The journal records the proof Verified, Unknown, status.applied records no Apply",
		"Under spec.policy.apply Always the operator applies that plan without a person, unless it is destructive",
		"The operator takes it at the next read of that target, up to spec.execution.failureRetryInterval (45s) after the last one.",
		"Removing the annotation before then takes the statement back: kubectl annotate ptahschema storefront -n demo "+
			operatorv1alpha1.SettledApplyAnnotation+"-",
		"settle --wait waits in the same command until the operator has taken the statement.")
	if strings.Contains(stdout, "Replaced:") || strings.Contains(stdout, "suspended") {
		t.Fatalf("settle's report names what did not happen:\n%s", stdout)
	}
	if value, _ := settledAnnotation(t, api); value != settleValueOn(movedTo) || api.patchCount() != 1 {
		t.Fatalf("the annotation holds %q after %d patches", value, api.patchCount())
	}
	schema := stored(t, api, "schema")
	if schema.GetLabels()["team"] != "shop" || schema.(*operatorv1alpha1.PtahSchema).Status.PendingObservation == nil {
		t.Fatalf("settle changed more than the annotation: %#v", schema)
	}
}

// The report says how the Apply ended from the journal, and from the proof
// where the journal no longer holds the entry.
func TestSettleSaysHowTheApplyEnded(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*operatorv1alpha1.PtahSchema)
		says   []string
	}{
		{
			name: "the journal no longer holds the entry",
			change: func(schema *operatorv1alpha1.PtahSchema) {
				schema.Status.Journal = nil
			},
			says: []string{"Journal:  no ApplyFinished entry for the plan any more",
				"It Succeeded, as status.pendingObservation records"},
		},
		{
			name: "nobody knows what the Apply did",
			change: func(schema *operatorv1alpha1.PtahSchema) {
				schema.Status.PendingObservation.Outcome = operatorv1alpha1.PendingObservationOutcomeUnknown
				schema.Status.Journal.Entries[0].Outcome = operatorv1alpha1.JournalUnknown
			},
			says: []string{"Journal:  ApplyFinished Unknown at",
				"Its outcome is Unknown, as the journal records: nobody established what it did there, so check that database first"},
		},
		{
			name: "no entry and no answer",
			change: func(schema *operatorv1alpha1.PtahSchema) {
				schema.Status.PendingObservation.Outcome = operatorv1alpha1.PendingObservationOutcomeUnknown
				schema.Status.Journal = nil
			},
			says: []string{"Its outcome is Unknown, as status.pendingObservation records"},
		},
		{
			name: "an approval policy and no approval recorded",
			change: func(schema *operatorv1alpha1.PtahSchema) {
				schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
				schema.Status.Plan.Approval = nil
			},
			says: []string{"Taking the statement gives up the proof, the plan ptah-plan-aaa and the database lock in one status write. ",
				"Under spec.policy.apply OnApproval that plan waits for an approval: kubectl ptah approve schema/storefront -n demo reviews it."},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newCluster(t, stoppedOnMove(movedTo, test.change))

			code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront")

			if code != exitOK {
				t.Fatalf("settle = %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			requireSays(t, "settle's report", stdout, test.says...)
		})
	}
}

func TestSettleSendsNothingWhenTheAnnotationHoldsTheValue(t *testing.T) {
	api := newCluster(t, stoppedOnMove(movedTo, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Annotations = map[string]string{operatorv1alpha1.SettledApplyAnnotation: settleValueOn(movedTo)}
	}))

	code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront")

	if code != exitOK || api.patchCount() != 0 {
		t.Fatalf("settle = %d after %d patches, stdout %q, stderr %q", code, api.patchCount(), stdout, stderr)
	}
	requireSays(t, "settle's report", stdout,
		operatorv1alpha1.SettledApplyAnnotation+" of ptahschema/storefront in namespace demo already holds the value its stop prints; nothing was sent.",
		"Holds:    "+operatorv1alpha1.SettledApplyAnnotation+"="+settleValueOn(movedTo))
}

func TestSettleSaysTheValueItReplacedSettledNothing(t *testing.T) {
	api := newCluster(t, stoppedOnMove(movedTo, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Annotations = map[string]string{operatorv1alpha1.SettledApplyAnnotation: settledApply}
	}))

	code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront")

	if code != exitOK {
		t.Fatalf("settle = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireSays(t, "settle's report", stdout, "Replaced: another value, which settled nothing")
	if value, _ := settledAnnotation(t, api); value != settleValueOn(movedTo) {
		t.Fatalf("the annotation holds %q", value)
	}
}

func TestSettleOnASuspendedSchemaSaysTheStatementWaitsForResume(t *testing.T) {
	api := newCluster(t, stoppedOnMove(movedTo, func(schema *operatorv1alpha1.PtahSchema) { schema.Spec.Suspend = true }))

	code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront")

	if code != exitOK {
		t.Fatalf("settle = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireSays(t, "settle's report", stdout, "The resource is suspended and starts no read, so the operator takes the "+
		"statement at the first read after kubectl ptah resume schema/storefront -n demo")
	if strings.Contains(stdout, "at the next read of that target") {
		t.Fatalf("settle on a suspended schema promises the next read:\n%s", stdout)
	}
}

// movingStop is a read that names another target landing between settle's
// read and its patch.
type movingStop struct{ *cluster }

func (m movingStop) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if m.patchCount() == 0 {
		moved := stored(m.t, m.cluster, "schema").(*operatorv1alpha1.PtahSchema)
		moved.Status.Conditions[0].Message = stopOn(movedAgain)
		if err := m.cluster.Client.Status().Update(ctx, moved); err != nil {
			m.t.Fatalf("write the stop on another target: %v", err)
		}
	}
	return m.cluster.Patch(ctx, object, patch, options...)
}

// settle changes nothing on a schema the operator would take no statement
// from, or with a value the operator would not take.
func TestSettleRefusesWhatTheOperatorWouldNotTake(t *testing.T) {
	for _, test := range []struct {
		name    string
		object  client.Object
		patches int
		says    []string
	}{
		{
			name: "not stopped", object: resting("schema"),
			says: []string{"ptahschema/storefront in namespace demo has not stopped on a proof whose target moved: it owes no proof of an Apply",
				"settle gives up a proof only where the schema stopped on it, with ReconciliationFailed=True and reason TargetIdentityChanged",
				"kubectl ptah schema storefront -n demo shows where it stands."},
		},
		{
			name: "a proof failing on its own target", object: stoppedOnMove(movedTo, func(schema *operatorv1alpha1.PtahSchema) {
				schema.Status.Conditions[0].Reason, schema.Status.Conditions[0].Message = "OperationFailed", "the server went away"
			}),
			says: []string{"ReconciliationFailed is True with reason OperationFailed, not True with reason TargetIdentityChanged"},
		},
		{
			name: "no value in the condition", object: stoppedOnMove(movedTo, func(schema *operatorv1alpha1.PtahSchema) {
				schema.Status.Conditions[0].Message = "the URL Secret now holds another target"
			}),
			says: []string{"the stop of ptahschema/storefront in namespace demo prints no value to settle it with",
				"The operator takes no value but the one its stop prints, so settle writes no other."},
		},
		{
			name: "a value for another Apply", object: stoppedOnMove(movedTo, func(schema *operatorv1alpha1.PtahSchema) {
				schema.Status.PendingObservation.ApplyOperationID = "sha256:" + strings.Repeat("2", 64)
			}),
			says: []string{"prints " + settleValueOn(movedTo) + ", which names Apply " + settledApply,
				"Run settle again to read the stop as it stands now"},
		},
		{
			name: "a value that moved after settle read it", object: stoppedOnMove(movedTo), patches: 1,
			says: []string{"prints " + settleValueOn(movedAgain) + ", which is not " + settleValueOn(movedTo) +
				", the value it printed when this command read it: a read named another target since"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newCluster(t, test.object)
			var writer client.Client = api
			if test.patches > 0 {
				writer = movingStop{api}
			}

			code, stdout, stderr := runCommand(t, writer, "settle", "schema/storefront")

			if code != exitNotDone || stdout != "" || api.patchCount() != test.patches {
				t.Fatalf("settle = %d after %d patches, stdout %q, stderr %q; want %d after %d",
					code, api.patchCount(), stdout, stderr, exitNotDone, test.patches)
			}
			requireSays(t, "settle's stderr", stderr, append(test.says, "Nothing was changed.")...)
			if value, held := settledAnnotation(t, api); held {
				t.Fatalf("a refused settlement left the annotation holding %q", value)
			}
		})
	}
}

// --expect binds the statement to the stop the person read: a read of another
// target since then prints another value, and settle writes nothing over it.
func TestSettleWritesOnlyTheValueItWasToldToExpect(t *testing.T) {
	t.Run("the stop still prints it", func(t *testing.T) {
		api := newCluster(t, stoppedOnMove(movedTo))

		code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront", "--expect", settleValueOn(movedTo))

		if value, _ := settledAnnotation(t, api); code != exitOK || value != settleValueOn(movedTo) {
			t.Fatalf("settle --expect = %d, annotation %q, stdout %q, stderr %q", code, value, stdout, stderr)
		}
	})
	t.Run("the stop moved to another target", func(t *testing.T) {
		api := newCluster(t, stoppedOnMove(movedAgain))

		code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront", "--expect", settleValueOn(movedTo))

		if code != exitNotDone || stdout != "" || api.patchCount() != 0 {
			t.Fatalf("settle --expect on a moved stop = %d after %d patches, stdout %q, stderr %q",
				code, api.patchCount(), stdout, stderr)
		}
		requireSays(t, "settle's stderr", stderr,
			"prints "+settleValueOn(movedAgain)+", not "+settleValueOn(movedTo)+", the value expected. Nothing was changed.",
			"check what the URL Secret holds before you settle that one.")
		if value, held := settledAnnotation(t, api); held {
			t.Fatalf("a refused settlement left the annotation holding %q", value)
		}
	})
	t.Run("a value that names no target", func(t *testing.T) {
		api := newCluster(t, stoppedOnMove(movedTo))

		code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront", "--expect", settledApply)

		if code != exitUsage || stdout != "" || api.patchCount() != 0 {
			t.Fatalf("settle --expect with the Apply's ID alone = %d after %d patches, stdout %q, stderr %q",
				code, api.patchCount(), stdout, stderr)
		}
		requireSays(t, "settle's stderr", stderr, "--expect:", "Give the value after "+operatorv1alpha1.SettledApplyAnnotation+"=")
	})
}

func TestSettleTakesOnlyASchema(t *testing.T) {
	api := newCluster(t, resting("migration"))

	code, stdout, stderr := runCommand(t, api, "settle", "migration/storefront")

	if code != exitUsage || stdout != "" || api.patchCount() != 0 {
		t.Fatalf("settle migration = %d after %d patches, stdout %q, stderr %q", code, api.patchCount(), stdout, stderr)
	}
	requireSays(t, "settle's stderr", stderr, "settle takes a PtahSchema, as schema/NAME.", "PtahMigrationRunAcknowledgment")
}

func TestSettleExitsByWhatTheAPIServerSaid(t *testing.T) {
	forbidden := &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusForbidden,
		Reason:  metav1.StatusReasonForbidden,
		Message: `ptahschemas.operator.ptah.run "storefront" is forbidden: User "bob" cannot patch resource "ptahschemas" in API group "operator.ptah.run" in the namespace "demo"`}}
	for _, test := range []struct {
		name   string
		refuse error
		code   int
		says   []string
	}{
		{"RBAC refuses the patch", forbidden, exitRefused,
			[]string{`User "bob" cannot patch`, "Nothing was changed. settle needs get and patch on ptahschemas in namespace demo."}},
		{"the patch has no answer", context.DeadlineExceeded, exitUnknown,
			[]string{"no answer said whether it was stored", "Running it again sends nothing when the annotation already holds the value."}},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newCluster(t, stoppedOnMove(movedTo))
			api.refuse = test.refuse

			code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront")

			if code != test.code || stdout != "" {
				t.Fatalf("settle = %d, stdout %q, stderr %q; want %d", code, stdout, stderr, test.code)
			}
			requireSays(t, "settle's stderr", stderr, test.says...)
		})
	}

	t.Run("the resource replaced", func(t *testing.T) {
		api := newCluster(t, stoppedOnMove(movedTo))
		api.replaced = true

		code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront")

		if code != exitNotDone || stdout != "" {
			t.Fatalf("settle = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		requireSays(t, "settle's stderr", stderr, "Nothing was changed: the resource under that name is not the one read.")
	})
}

// tookTheStatement is the status write that settles the proof.
func tookTheStatement(object client.Object) {
	status := &object.(*operatorv1alpha1.PtahSchema).Status
	status.PendingObservation, status.Plan, status.Phase = nil, nil, operatorv1alpha1.PhasePending
	status.Conditions = []metav1.Condition{
		{Type: operatorv1alpha1.ConditionReconciliationFailed, Status: metav1.ConditionFalse,
			Reason: string(operatorv1alpha1.ReasonProofSettled), Message: "A person settled the proof", ObservedGeneration: 4},
		{Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionFalse,
			Reason: string(operatorv1alpha1.ReasonProofSettled), Message: "A person settled the proof", ObservedGeneration: 4},
	}
	status.Journal.Entries = append(status.Journal.Entries, operatorv1alpha1.JournalEntry{Type: operatorv1alpha1.JournalVerified,
		At: metav1.NewTime(readAgainAt), Plan: "ptah-plan-aaa", Outcome: operatorv1alpha1.JournalUnknown})
}

// provedOnTheFirstTarget is a read of the target the plan was made for that
// proved the Apply before the statement was taken.
func provedOnTheFirstTarget(object client.Object) {
	tookTheStatement(object)
	status := &object.(*operatorv1alpha1.PtahSchema).Status
	status.Phase = operatorv1alpha1.PhaseInSync
	status.Conditions = nil
	status.Journal.Entries[len(status.Journal.Entries)-1].Outcome = operatorv1alpha1.JournalInSync
}

func TestSettleWaitsForTheOperatorToTakeTheStatement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newCluster(t, stoppedOnMove(movedTo))
		api.then(unchanged, unchanged, tookTheStatement)

		code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront", "--wait")

		if code != exitOK {
			t.Fatalf("settle --wait = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		requireSays(t, "settle --wait's report", stdout, "Settlement requested for",
			"ptahschema/storefront in namespace demo reached settled.",
			"Journal:    Verified Unknown at 2026-10-01T09:07:00Z, plan ptah-plan-aaa",
			"The operator gave the proof up, and status.applied records no Apply.")
		requireSays(t, "settle --wait's stderr", stderr, "Waiting up to 5m0s for ptahschema/storefront in namespace demo to reach settled.")
		if strings.Contains(stdout, "settle --wait waits") {
			t.Fatalf("settle --wait points at the wait it ran:\n%s", stdout)
		}
	})

	synctest.Test(t, func(t *testing.T) {
		api := newCluster(t, stoppedOnMove(movedTo))
		api.then(unchanged, provedOnTheFirstTarget)

		code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront", "--wait")

		if code != exitWillNotReach || strings.Contains(stdout, "reached") {
			t.Fatalf("settle --wait on a proof the first target ended = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		requireSays(t, "settle --wait's stderr", stderr, "will not reach settled: ProofEnded.", "Verified InSync: the statement was not needed")
	})

	synctest.Test(t, func(t *testing.T) {
		api := newCluster(t, stoppedOnMove(movedTo))

		code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront", "--wait", "--wait-timeout", "1m")

		if code != exitTimedOut || strings.Contains(stdout, "reached") {
			t.Fatalf("settle --wait with no read = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		requireSays(t, "the timeout", stderr, "did not reach settled within 1m0s.", "Phase:       Blocked",
			"Waiting for: the next read of target "+movedTo+", which takes the statement (spec.execution.failureRetryInterval is 45s), "+
				"unless the URL Secret holds target "+planTarget+" again, the one the plan was made for")
	})
}

func TestSettlePrintsTheResourceTheAnswerCarried(t *testing.T) {
	api := newCluster(t, stoppedOnMove(movedTo))

	code, stdout, stderr := runCommand(t, api, "settle", "schema/storefront", "-o", "json")

	var schema operatorv1alpha1.PtahSchema
	if code != exitOK || yaml.UnmarshalStrict([]byte(stdout), &schema) != nil {
		t.Fatalf("settle -o json = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if schema.Kind != "PtahSchema" || schema.Annotations[operatorv1alpha1.SettledApplyAnnotation] != settleValueOn(movedTo) || stderr != "" {
		t.Fatalf("settle -o json printed %s with annotations %v, stderr %q", schema.Kind, schema.Annotations, stderr)
	}
}
