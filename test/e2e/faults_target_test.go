package e2e

import (
	"slices"
	"strings"
	"testing"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSchemaRetargetRefusalIsBeforeChildDispatch(t *testing.T) {
	t.Parallel()
	previous := "sha256:" + strings.Repeat("a", 64)
	valid := runner.Result{TargetIdentityDigest: "sha256:" + strings.Repeat("b", 64), ChildExitCode: -1,
		Error: &runner.ResultError{Code: "target_binding_mismatch"}}
	if !schemaRetargetRefused(valid, previous) {
		t.Fatal("rejected target-binding refusal before child dispatch")
	}
	for name, edit := range map[string]func(*runner.Result){
		"same target":        func(r *runner.Result) { r.TargetIdentityDigest = previous },
		"missing target":     func(r *runner.Result) { r.TargetIdentityDigest = "" },
		"another error":      func(r *runner.Result) { r.Error = &runner.ResultError{Code: "stale_plan"} },
		"no refusal":         func(r *runner.Result) { r.Error = nil },
		"child ran":          func(r *runner.Result) { r.ChildExitCode = 0 },
		"mutation began":     func(r *runner.Result) { r.MutationStarted = true },
		"uncertain work":     func(r *runner.Result) { r.Uncertain = true },
		"truncated evidence": func(r *runner.Result) { r.Truncation = &runner.TruncationMetadata{Stdout: true} },
	} {
		t.Run(name, func(t *testing.T) {
			reading := valid
			edit(&reading)
			if schemaRetargetRefused(reading, previous) {
				t.Fatal("accepted a reading that does not prove refusal before dispatch")
			}
		})
	}
}

func TestFreshApprovalWaitsForObservedConvergence(t *testing.T) {
	t.Parallel()
	base := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Status: ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseInSync, ObservedGeneration: 2,
		Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
	}}
	if !freshApprovalConverged(base) {
		t.Fatal("rejected current observed convergence")
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchema){
		"old generation": func(r *ptahv1alpha1.PtahSchema) { r.Status.ObservedGeneration = 1 },
		"not ready":      func(r *ptahv1alpha1.PtahSchema) { r.Status.Conditions = nil },
		"still applying": func(r *ptahv1alpha1.PtahSchema) { r.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{} },
		"awaiting observation": func(r *ptahv1alpha1.PtahSchema) {
			r.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			reading := base.DeepCopy()
			edit(reading)
			if freshApprovalConverged(reading) {
				t.Fatal("accepted unfinished convergence")
			}
		})
	}
}

func TestRetargetProofKeepsTheDispatchedApplyBinding(t *testing.T) {
	t.Parallel()
	refusal := "the URL Secret holds another target: kubectl annotate ptahschema x -n y " +
		ptahv1alpha1.SettledApplyAnnotation + "=apply-original/" + ftDigest + " --overwrite"
	base := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "y"}, Status: ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseBlocked,
		PendingObservation: &ptahv1alpha1.PendingObservationStatus{
			Outcome:          ptahv1alpha1.PendingObservationOutcomeUnknown,
			ApplyOperationID: "apply-original", ApplyJobUID: "job-original",
			Plan: ptahv1alpha1.CurrentPlanStatus{TargetIdentityDigest: ftDigest},
		},
		Conditions: []metav1.Condition{{
			Type: ptahv1alpha1.ConditionReconciliationFailed, Status: metav1.ConditionTrue,
			Reason:  string(ptahv1alpha1.ReasonTargetIdentityChanged),
			Message: refusal,
		}},
	}}
	accepts := func(r *ptahv1alpha1.PtahSchema) bool {
		return retargetProofRefused(r, ptahv1alpha1.PendingObservationOutcomeUnknown, "apply-original", "job-original", ftDigest)
	}
	ftRefusesEach(t, base.DeepCopy, accepts, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"proof abandoned", func(r *ptahv1alpha1.PtahSchema) { r.Status.PendingObservation = nil }},
		{"another Apply", func(r *ptahv1alpha1.PtahSchema) { r.Status.PendingObservation.ApplyOperationID = "another-apply" }},
		{"another Job", func(r *ptahv1alpha1.PtahSchema) { r.Status.PendingObservation.ApplyJobUID = "another-job" }},
		{"substituted target accepted", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.PendingObservation.Plan.TargetIdentityDigest = "sha256:" + strings.Repeat("c", 64)
		}},
		{"success attributed", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.PendingObservation.Outcome = ptahv1alpha1.PendingObservationApplySucceeded
		}},
		{"not stopped", func(r *ptahv1alpha1.PtahSchema) { r.Status.Phase = ptahv1alpha1.PhaseFailed }},
		{"no refusal", func(r *ptahv1alpha1.PtahSchema) { r.Status.Conditions = nil }},
		{"old refusal cleared", func(r *ptahv1alpha1.PtahSchema) { r.Status.Conditions[0].Status = metav1.ConditionFalse }},
		{"another failure", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.Conditions[0].Reason = string(ptahv1alpha1.ReasonOperationFailed)
		}},
		{"no way out named", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.Conditions[0].Message = "the URL Secret holds another target"
		}},
		{"the way out of another Apply", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.Conditions[0].Message = strings.ReplaceAll(r.Status.Conditions[0].Message, "apply-original", "another-apply")
		}},
	})
	// The read goes on while this refusal remains true, and the claim doing it is
	// no evidence that the proof was settled.
	ftAcceptsEach(t, base.DeepCopy, accepts, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"reading again", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationObserve}
		}},
	})
	for _, bindings := range [][3]string{{"", "job-original", ftDigest}, {"apply-original", "", ftDigest}, {"apply-original", "job-original", ""}} {
		if retargetProofRefused(base, ptahv1alpha1.PendingObservationOutcomeUnknown, bindings[0], bindings[1], bindings[2]) {
			t.Fatal("accepted missing expected proof bindings")
		}
	}
}

// A row runs what the message prints, so the reading has to refuse anything a person
// could not run as it stands, and a command that would not replace a value already
// there.
func TestThePrintedSettleCommandIsTheOneARowRuns(t *testing.T) {
	t.Parallel()
	value := ptahv1alpha1.SettledApplyAnnotation + "=apply-original/" + ftDigest
	command := "kubectl annotate ptahschema x -n y " + value + " --overwrite"
	got, err := printedSettleCommand("reads the database under the new URL: "+command, "x", "y", "apply-original")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"annotate", "ptahschema", "x", "-n", "y", value, "--overwrite"}; !slices.Equal(got, want) {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
	for name, message := range map[string]string{
		"no command":      "the URL Secret holds another target",
		"another schema":  strings.Replace(command, " x ", " z ", 1),
		"another scope":   strings.Replace(command, " y ", " z ", 1),
		"another Apply":   strings.Replace(command, "apply-original", "another-apply", 1),
		"no target":       strings.Replace(command, "/"+ftDigest, "", 1),
		"not a digest":    strings.Replace(command, ftDigest, "sha256:short", 1),
		"no overwrite":    strings.TrimSuffix(command, " --overwrite"),
		"words after it":  command + " to settle it",
		"another command": strings.Replace(command, "annotate ptahschema", "annotate ptahmigration", 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := printedSettleCommand(message, "x", "y", "apply-original"); err == nil {
				t.Fatalf("accepted %q", message)
			}
		})
	}
}

func TestAProofReadAgainIsALaterAttemptOfTheSameProof(t *testing.T) {
	t.Parallel()
	base := &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{
		Phase:              ptahv1alpha1.PhaseBlocked,
		PendingObservation: &ptahv1alpha1.PendingObservationStatus{ApplyOperationID: "apply-original"},
		ActiveOperation:    &ptahv1alpha1.ActiveOperationStatus{Attempt: 3},
	}}
	ftRefusesEach(t, base.DeepCopy, func(r *ptahv1alpha1.PtahSchema) bool {
		return proofReadAgain(r, "apply-original", 2)
	}, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"the attempt the row saw", func(r *ptahv1alpha1.PtahSchema) { r.Status.ActiveOperation.Attempt = 2 }},
		{"no claim", func(r *ptahv1alpha1.PtahSchema) { r.Status.ActiveOperation = nil }},
		{"proof gone", func(r *ptahv1alpha1.PtahSchema) { r.Status.PendingObservation = nil }},
		{"another proof", func(r *ptahv1alpha1.PtahSchema) { r.Status.PendingObservation.ApplyOperationID = "another-apply" }},
		{"no longer stopped", func(r *ptahv1alpha1.PtahSchema) { r.Status.Phase = ptahv1alpha1.PhasePending }},
	})
}

func TestAPlanUnderAnotherTargetIsNotTheFirstPlan(t *testing.T) {
	t.Parallel()
	other := "sha256:" + strings.Repeat("c", 64)
	base := &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseAwaitingApproval,
		Plan:  &ptahv1alpha1.CurrentPlanStatus{Name: "current-plan", TargetIdentityDigest: other},
	}}
	accepts := func(r *ptahv1alpha1.PtahSchema) bool { return planUnderAnotherTarget(r, ftDigest) }
	ftRefusesEach(t, base.DeepCopy, accepts, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"the first target", func(r *ptahv1alpha1.PtahSchema) { r.Status.Plan.TargetIdentityDigest = ftDigest }},
		{"no target", func(r *ptahv1alpha1.PtahSchema) { r.Status.Plan.TargetIdentityDigest = "" }},
		{"no plan", func(r *ptahv1alpha1.PtahSchema) { r.Status.Plan = nil }},
		{"not waiting", func(r *ptahv1alpha1.PtahSchema) { r.Status.Phase = ptahv1alpha1.PhaseBlocked }},
		{"a read in flight", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationObserve}
		}},
		{"proof still owed", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{}
		}},
	})
	if planUnderAnotherTarget(base, "") {
		t.Fatal("accepted a plan with no target to compare it to")
	}
}

func TestAJournalSaysAProofWasSettledOnlyForThatPlan(t *testing.T) {
	t.Parallel()
	base := &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{Journal: &ptahv1alpha1.LifecycleJournal{
		Entries: []ptahv1alpha1.JournalEntry{
			{Type: ptahv1alpha1.JournalApplyFinished, Plan: "plan-a", Outcome: ptahv1alpha1.JournalUnknown},
			{Type: ptahv1alpha1.JournalVerified, Plan: "plan-a", Outcome: ptahv1alpha1.JournalUnknown},
		},
	}}}
	accepts := func(r *ptahv1alpha1.PtahSchema) bool { return proofSettledInJournal(r, "plan-a") }
	ftRefusesEach(t, base.DeepCopy, accepts, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"no journal", func(r *ptahv1alpha1.PtahSchema) { r.Status.Journal = nil }},
		{"only the Apply is unknown", func(r *ptahv1alpha1.PtahSchema) { r.Status.Journal.Entries = r.Status.Journal.Entries[:1] }},
		{"proved", func(r *ptahv1alpha1.PtahSchema) { r.Status.Journal.Entries[1].Outcome = ptahv1alpha1.JournalInSync }},
		{"another plan", func(r *ptahv1alpha1.PtahSchema) { r.Status.Journal.Entries[1].Plan = "plan-b" }},
	})
	if proofSettledInJournal(base, "") {
		t.Fatal("accepted no plan name")
	}
}

func TestAProofFinishedUnderItsTargetCreditsThatPlan(t *testing.T) {
	t.Parallel()
	base := &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{
		Phase:      ptahv1alpha1.PhaseInSync,
		Applied:    &ptahv1alpha1.AppliedStatus{PlanFingerprint: "first-plan"},
		Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
	}}
	accepts := func(r *ptahv1alpha1.PtahSchema) bool { return proofFinishedUnderItsTarget(r, "first-plan") }
	ftRefusesEach(t, base.DeepCopy, accepts, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"not in sync", func(r *ptahv1alpha1.PtahSchema) { r.Status.Phase = ptahv1alpha1.PhaseBlocked }},
		{"nothing credited", func(r *ptahv1alpha1.PtahSchema) { r.Status.Applied = nil }},
		{"another plan credited", func(r *ptahv1alpha1.PtahSchema) { r.Status.Applied.PlanFingerprint = "another-plan" }},
		{"proof still owed", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{}
		}},
		{"a read in flight", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationObserve}
		}},
		{"not ready", func(r *ptahv1alpha1.PtahSchema) { r.Status.Conditions = nil }},
	})
	if proofFinishedUnderItsTarget(base, "") {
		t.Fatal("accepted no fingerprint")
	}
}

func TestAURLWithAnotherLoginChangesOnlyTheLogin(t *testing.T) {
	t.Parallel()
	const first = "postgres://first:e2ePg%2F1@e2e-postgresql.ns.svc.cluster.local:5432/db?sslmode=disable"
	got, err := urlWithLogin(first, "second")
	if err != nil {
		t.Fatal(err)
	}
	if want := "postgres://second:e2ePg%2F1@e2e-postgresql.ns.svc.cluster.local:5432/db?sslmode=disable"; got != want {
		t.Fatalf("urlWithLogin() = %q, want %q", got, want)
	}
	before, err := runner.TargetIdentityDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	after, err := runner.TargetIdentityDigest(got)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("the login did not move the target identity, so the row would measure nothing")
	}
	for name, raw := range map[string]string{
		"no login":       "postgres://e2e-postgresql:5432/db",
		"no password":    "postgres://first@e2e-postgresql:5432/db",
		"the same login": "postgres://second:pw@e2e-postgresql:5432/db",
		"not a URL":      "postgres://first:pw@host:notaport/db",
	} {
		if _, err := urlWithLogin(raw, "second"); err == nil {
			t.Errorf("%s: accepted %q", name, raw)
		}
	}
	if _, err := urlWithLogin(first, ""); err == nil {
		t.Error("accepted no login")
	}
}

func TestTargetPlanWaitsForTheOriginalProofToFinish(t *testing.T) {
	t.Parallel()
	base := &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseAwaitingApproval,
		Plan:  &ptahv1alpha1.CurrentPlanStatus{Name: "current-plan", TargetIdentityDigest: ftDigest},
	}}
	ftRefusesEach(t, base.DeepCopy, func(r *ptahv1alpha1.PtahSchema) bool {
		return targetPlanAwaitingApproval(r, ftDigest)
	}, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"proof still owed", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{}
		}},
		{"another target", func(r *ptahv1alpha1.PtahSchema) {
			r.Status.Plan.TargetIdentityDigest = "sha256:" + strings.Repeat("c", 64)
		}},
		{"no plan", func(r *ptahv1alpha1.PtahSchema) { r.Status.Plan = nil }},
		{"work still active", func(r *ptahv1alpha1.PtahSchema) { r.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{} }},
		{"no approval gate", func(r *ptahv1alpha1.PtahSchema) { r.Status.Phase = ptahv1alpha1.PhasePlanning }},
	})
	if targetPlanAwaitingApproval(base, "") {
		t.Fatal("accepted a missing expected target")
	}
}
