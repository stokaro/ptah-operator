package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/migrationview"
	"github.com/stokaro/ptah-operator/internal/schemaview"
	"github.com/stokaro/ptah-operator/internal/sourceview"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// A scenario step that runs `kubectl ptah schema` or `kubectl ptah migration`
// expects lines of its text output, padding included, and the recorder holds
// them only against a live lab. A renderer that moved its label column would
// be found by the next recording, an hour into it. This renders the state each
// such step reads and requires every string the step expects in it, and every
// string it refuses out of it, before any lab exists.
//
// The states are written here, one per view step and keyed by the step's
// place among the scenario's view steps. A step that prints a view and has no
// state here fails the test, so a new one cannot go unpinned.
//
// The verbs -- approve, wait, suspend, resume, refresh, set-source -- print
// from cmd/kubectl-ptah, which this package cannot import. Their steps expect
// the verdict lines alone ("reached in-sync", "Source set for"), and the
// recorder reads the resource itself to check what the verdict claims.
func TestPluginTextStepsExpectWhatTheRendererWrites(t *testing.T) {
	t.Parallel()
	loaded, err := loadScenarios(filepath.Join("..", "..", "scenarios"))
	if err != nil {
		t.Fatal(err)
	}
	schemas := map[string]schemaview.View{
		"first-apply#1":       demoSchemaInSync(),
		"destructive-block#1": demoSchemaInSync(),
		"destructive-block#2": demoSchemaBlocked(),
		"stale-approval#1":    demoSchemaAwaitingApproval("ptah-plan-0aa1"),
		"stale-approval#2":    withApproval(demoSchemaInSync(), demoConsumedApproval(true)),
		"stale-approval#3":    withApproval(demoSchemaAwaitingApproval("ptah-plan-0bb2"), demoConsumedApproval(true)),
		"suspend-resume#1":    demoSchemaInSync(),
		"drift#1":             demoSchemaInSync(),
		"failure-recovery#1":  demoSchemaFailed(),
		"manual-approval#1":   demoSchemaAwaitingApproval("ptah-plan-0aa1"),
		"manual-approval#2":   withApproval(demoSchemaInSync(), demoConsumedApproval(false)),
	}
	migrations := map[string]migrationview.View{
		"checkpoint-bootstrap#1":    withCheckpoint(demoInSync(4, 3, 4)),
		"second-release#1":          demoInSync(1, 1),
		"second-release#2":          demoAwaitingFrom(1, demoPlanned(2, "Add Carrier")),
		"second-release#3":          demoInSync(2, 2),
		"second-release#4":          demoInSync(2, 2),
		"versioned-migration#1":     demoAwaitingApproval(),
		"versioned-migration#2":     withMigrationApproval(demoInSync(2, 1, 2), "shipments-v1"),
		"adopt-existing-database#1": demoAwaitingFrom(1, demoPlanned(2, "Add Carrier")),
		"mysql-partial-failure#1":   demoPartial(),
		"mysql-partial-failure#2":   demoPartialSettled(),
	}
	checked := map[string]bool{}
	for _, one := range loaded {
		occurrence := 0
		for _, current := range one.Steps {
			family := viewFamily(current.Run)
			if family == "" {
				continue
			}
			occurrence++
			key := fmt.Sprintf("%s#%d", one.ID, occurrence)
			var text strings.Builder
			switch family {
			case "schema":
				state, ok := schemas[key]
				if !ok {
					t.Errorf("%s prints kubectl ptah schema text and this test has no state for it", key)
					continue
				}
				if err := schemaview.Render(&text, state, schemaview.Text); err != nil {
					t.Fatal(err)
				}
			case "migration":
				state, ok := migrations[key]
				if !ok {
					t.Errorf("%s prints kubectl ptah migration text and this test has no state for it", key)
					continue
				}
				if err := migrationview.Render(&text, state, migrationview.Text); err != nil {
					t.Fatal(err)
				}
			}
			checked[key] = true
			for _, want := range current.Expect.StdoutContains {
				if !strings.Contains(text.String(), want) {
					t.Errorf("%s expects %q, which the renderer does not write for that state:\n%s", key, want, text.String())
				}
			}
			for _, refused := range current.Expect.StdoutAbsent {
				if strings.Contains(text.String(), refused) {
					t.Errorf("%s refuses %q, which the renderer writes for that state:\n%s", key, refused, text.String())
				}
			}
		}
	}
	if len(checked) == 0 {
		t.Fatal("no scenario step prints a kubectl ptah view, so this test held nothing")
	}
	for key := range schemas {
		if !checked[key] {
			t.Errorf("%s names no schema view step any scenario runs", key)
		}
	}
	for key := range migrations {
		if !checked[key] {
			t.Errorf("%s names no migration view step any scenario runs", key)
		}
	}
}

// viewFamily names the view a step prints, or nothing for a step that prints
// none: a step that runs the view and nothing else, in text.
func viewFamily(run string) string {
	command := strings.TrimSpace(run)
	if strings.Contains(command, "\n") || strings.Contains(command, " -o ") {
		return ""
	}
	switch {
	case strings.HasPrefix(command, "kubectl ptah schema "):
		return "schema"
	case strings.HasPrefix(command, "kubectl ptah migration "):
		return "migration"
	}
	return ""
}

func TestViewFamilyNamesOnlyAViewStep(t *testing.T) {
	t.Parallel()
	for run, want := range map[string]string{
		`kubectl ptah schema storefront -n "$NAMESPACE"`:              "schema",
		`kubectl ptah migration shipments -n "$NAMESPACE"`:            "migration",
		`kubectl ptah schema storefront -n "$NAMESPACE" -o json`:      "",
		`kubectl ptah wait schema/storefront --for in-sync`:           "",
		`kubectl ptah approve migration/shipments --wait`:             "",
		"kubectl ptah schema storefront\nkubectl events --for x":      "",
		`kubectl ptah plan storefront --applied -n "$NAMESPACE"`:      "",
		`kubectl -n "$NAMESPACE" get ptahmigration shipments -o yaml`: "",
	} {
		if got := viewFamily(run); got != want {
			t.Errorf("viewFamily(%q) = %q, expected %q", run, got, want)
		}
	}
}

var demoObserved = metav1.NewTime(time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC))

func demoConditions(conditions ...metav1.Condition) []statusview.Condition {
	for index := range conditions {
		conditions[index].LastTransitionTime = demoObserved
	}
	return statusview.FromConditions(conditions)
}

func demoSchema(phase string) schemaview.View {
	return schemaview.View{
		Namespace: "demo", Schema: "storefront", UID: "storefront-uid", Generation: 1, ObservedGeneration: 1,
		Phase:  phase,
		Policy: &schemaview.PolicyView{Apply: "Always", DriftSeverity: "all"},
		Source: &sourceview.View{
			Kind:      sourceview.KindOCI,
			Reference: "oci://e2e-registry.demo.svc.cluster.local:5000/schemas/demo@sha256:0123",
			Digest:    "sha256:0123", Verified: true,
		},
		Observation: &schemaview.ObservationView{ObservedAt: &demoObserved},
		Approvals:   &statusview.Decisions[schemaview.ApprovalView]{Items: []schemaview.ApprovalView{}},
	}
}

func demoSchemaInSync() schemaview.View {
	view := demoSchema(string(operatorv1alpha1.PhaseInSync))
	view.Applied = &schemaview.AppliedView{PlanName: "ptah-plan-0aa1", PlanFingerprint: "sha256:0aa1", CompletedAt: demoObserved}
	view.Conditions = demoConditions(
		metav1.Condition{Type: "Ready", Status: "True", Reason: "InSync", Message: "Schema is in sync"},
		metav1.Condition{Type: "ReconciliationFailed", Status: "False", Reason: "Succeeded", Message: "The latest operation completed"},
		metav1.Condition{Type: "DriftDetected", Status: "False", Reason: "ScopedConverged", Message: "The authoritative managed scope has no changes"},
		metav1.Condition{Type: "InSync", Status: "True", Reason: "ScopedConverged", Message: "A stable scoped plan proves convergence"},
		metav1.Condition{Type: "Suspended", Status: "False", Reason: "Active", Message: "Reconciliation is active"},
		metav1.Condition{Type: "Applying", Status: "False", Reason: "JobCompleted", Message: "Apply Job completed; convergence observation is pending"},
	)
	return view
}

func demoSchemaAwaitingApproval(plan string) schemaview.View {
	view := demoSchema(string(operatorv1alpha1.PhaseAwaitingApproval))
	view.Policy.Apply = "OnApproval"
	view.Plan = &schemaview.PlanView{Name: plan, Fingerprint: "sha256:" + plan, StatementCount: 1, CreatedAt: demoObserved}
	view.Conditions = demoConditions(
		metav1.Condition{Type: "Ready", Status: "False", Reason: "AwaitingApproval", Message: "Plan is waiting for approval"},
		metav1.Condition{Type: "ApprovalRequired", Status: "True", Reason: "Waiting", Message: "Create an approval bound to the current plan fingerprint"},
		metav1.Condition{Type: "Suspended", Status: "False", Reason: "Active", Message: "Reconciliation is active"},
	)
	return view
}

func demoSchemaBlocked() schemaview.View {
	view := demoSchema(string(operatorv1alpha1.PhaseBlocked))
	view.Plan = &schemaview.PlanView{Name: "ptah-plan-0cc3", Fingerprint: "sha256:0cc3", StatementCount: 1, Destructive: true, CreatedAt: demoObserved}
	view.Conditions = demoConditions(
		metav1.Condition{Type: "Ready", Status: "False", Reason: "PolicyBlocked", Message: "Plan is blocked by destructive-change policy"},
		metav1.Condition{Type: "ApprovalRequired", Status: "False", Reason: "DestructiveChangesDisabled",
			Message: "Plan contains destructive changes and policy disallows them; read them with kubectl ptah plan, then either narrow the desired schema or set spec.policy.allowDestructive"},
	)
	return view
}

func demoSchemaFailed() schemaview.View {
	view := demoSchema(string(operatorv1alpha1.PhaseFailed))
	view.Source = &sourceview.View{Kind: sourceview.KindConfigMap, ConfigMap: "storefront-v1", Snapshot: "storefront-snapshot", ContentDigest: "sha256:0dd4"}
	view.Observation = nil
	view.Conditions = demoConditions(
		metav1.Condition{Type: "Ready", Status: "False", Reason: "OperationFailed", Message: "Reconciliation did not complete"},
		metav1.Condition{Type: "ReconciliationFailed", Status: "True", Reason: "OperationFailed", Message: "child_exit: ptah exited with code 2"},
	)
	return view
}

func demoConsumedApproval(stale bool) schemaview.ApprovalView {
	approval := schemaview.ApprovalView{
		Name: "storefront-v1", CreatedAt: demoObserved,
		PlanRef:         operatorv1alpha1.ImmutableObjectReference{Name: "ptah-plan-0aa1", UID: "plan-uid"},
		PlanFingerprint: "sha256:0aa1", AppliedPlan: true,
		Approver:   operatorv1alpha1.ApprovalIdentity{Username: "kubernetes-admin"},
		ApprovedAt: demoObserved,
	}
	conditions := []metav1.Condition{
		{Type: "Consumed", Status: "True", Reason: "DispatchCommitted", Message: "The approval was consumed by the Apply dispatch"},
	}
	if stale {
		conditions = append(conditions, metav1.Condition{Type: "Stale", Status: "True", Reason: "PlanNoLongerCurrent",
			Message: "The approved plan no longer matches the current database state"})
	}
	approval.Conditions = demoConditions(conditions...)
	return approval
}

func withApproval(view schemaview.View, approval schemaview.ApprovalView) schemaview.View {
	view.Approvals = &statusview.Decisions[schemaview.ApprovalView]{Total: 1, Items: []schemaview.ApprovalView{approval}}
	return view
}

// demoAwaitingApproval is versioned-migration's plan of two migrations against
// an empty history, waiting for a decision, with the conditions the
// controller writes for it.
func demoAwaitingApproval() migrationview.View {
	view := demoAwaitingFrom(0, demoPlanned(1, "Create Shipments"), demoPlanned(2, "Add Carrier"))
	view.LastRun = nil
	return view
}

func demoPlanned(version int64, description string) migrationview.PlannedMigrationView {
	return migrationview.PlannedMigrationView{Version: version, Description: description, Checksum: fmt.Sprintf("sha256:%04d", version)}
}

// demoAwaitingFrom is a migration whose history is at current, with planned
// waiting for a decision. A history above zero was written by a run that
// applied current, or by baseline; the last run is the run's.
func demoAwaitingFrom(current int64, planned ...migrationview.PlannedMigrationView) migrationview.View {
	view := demoMigration(string(operatorv1alpha1.MigrationPhaseAwaitingApproval))
	view.Policy = &migrationview.PolicyView{Apply: string(operatorv1alpha1.ApplyPolicyOnApproval)}
	view.History = &migrationview.HistoryView{
		ObservedAt: demoObserved, CurrentVersion: current, AppliedCount: int32(current), PendingCount: int32(len(planned)),
	}
	view.Plan = &migrationview.PlanView{
		Name: "ptah-mplan-0123456789abcdef01234567", CurrentVersion: current, CreatedAt: demoObserved, Migrations: planned,
	}
	view.Conditions = demoConditions(
		metav1.Condition{Type: "Progressing", Status: "False", Reason: "AwaitingApproval", Message: "Nothing runs until a person writes the approval"},
		metav1.Condition{Type: "Blocked", Status: "False", Reason: "HistoryMatched", Message: "The history continues this artifact"},
		metav1.Condition{Type: "Ready", Status: "False", Reason: "AwaitingApproval", Message: "The plan is waiting for the approval its policy requires"},
		metav1.Condition{Type: "ApprovalRequired", Status: "True", Reason: "AwaitingApproval",
			Message: fmt.Sprintf("%d planned migrations need an approval naming this plan", len(planned))},
	)
	return view
}

func demoMigration(phase string) migrationview.View {
	return migrationview.View{
		Namespace: "demo", Migration: "shipments", UID: "shipments-uid", Generation: 1, ObservedGeneration: 1,
		Phase:           phase,
		Approvals:       &statusview.Decisions[migrationview.ApprovalView]{Items: []migrationview.ApprovalView{}},
		Acknowledgments: &statusview.Decisions[migrationview.AcknowledgmentView]{Items: []migrationview.AcknowledgmentView{}},
	}
}

// demoInSync is a migration whose history is at current, and whose last run
// applied versions.
func demoInSync(current int64, versions ...int64) migrationview.View {
	finished := metav1.NewTime(demoObserved.Add(-time.Minute))
	view := demoMigration(string(operatorv1alpha1.MigrationPhaseInSync))
	view.Policy = &migrationview.PolicyView{Apply: string(operatorv1alpha1.ApplyPolicyOnApproval)}
	view.History = &migrationview.HistoryView{ObservedAt: demoObserved, CurrentVersion: current, AppliedCount: int32(current)}
	view.LastRun = &migrationview.RunView{
		Outcome: string(operatorv1alpha1.MigrationRunOutcomeApplied), JobName: "ptah-m-apply-shipments-0123456789abcdef",
		JobUID: "apply-uid", StartedAt: metav1.NewTime(demoObserved.Add(-2 * time.Minute)), FinishedAt: &finished,
		AppliedVersions: versions,
	}
	view.Conditions = demoConditions(
		metav1.Condition{Type: "Progressing", Status: "False", Reason: "HistoryMatched", Message: "Nothing is pending"},
		metav1.Condition{Type: "Blocked", Status: "False", Reason: "HistoryMatched", Message: "The history continues this artifact"},
		metav1.Condition{Type: "Ready", Status: "True", Reason: "HistoryMatched", Message: "The database has every migration this artifact carries"},
		metav1.Condition{Type: "ApprovalRequired", Status: "False", Reason: "Satisfied", Message: "The plan's approval requirements are satisfied"},
	)
	return view
}

func withCheckpoint(view migrationview.View) migrationview.View {
	view.History.CheckpointVersion = 3
	return view
}

func withMigrationApproval(view migrationview.View, name string) migrationview.View {
	view.Approvals = &statusview.Decisions[migrationview.ApprovalView]{Total: 1, Items: []migrationview.ApprovalView{{
		Name: name, CreatedAt: demoObserved,
		PlanRef:    operatorv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-0123456789abcdef01234567", UID: "plan-uid"},
		Approver:   operatorv1alpha1.ApprovalIdentity{Username: "kubernetes-admin"},
		ApprovedAt: demoObserved,
		Conditions: demoConditions(metav1.Condition{Type: "Consumed", Status: "True", Reason: "DispatchCommitted", Message: "The approval was consumed"}),
	}}}
	return view
}

// demoPartial is mysql-partial-failure's deliveries after the run of version 2
// stopped at its third statement: the last run ended Partial, and the same run
// holds the resource as unresolved.
func demoPartial() migrationview.View {
	finished := metav1.NewTime(demoObserved.Add(-time.Minute))
	view := demoMigration(string(operatorv1alpha1.MigrationPhaseBlocked))
	view.Migration = "deliveries"
	view.Policy = &migrationview.PolicyView{Apply: string(operatorv1alpha1.ApplyPolicyOnApproval)}
	view.History = &migrationview.HistoryView{ObservedAt: demoObserved, CurrentVersion: 1, AppliedCount: 1, PendingCount: 1}
	view.LastRun = &migrationview.RunView{
		Outcome: string(operatorv1alpha1.MigrationRunOutcomePartial), JobName: "ptah-m-apply-deliveries-0123456789abcdef",
		JobUID: "apply-uid", StartedAt: metav1.NewTime(demoObserved.Add(-2 * time.Minute)), FinishedAt: &finished,
		Message: "migration 2 failed at statement 3 of 3",
		Failure: &operatorv1alpha1.MigrationRunFailureStatus{Version: 2, Statement: 3, Statements: 3, ErrorNumber: 1054},
	}
	view.UnresolvedRun = &migrationview.UnresolvedRunView{
		Outcome: string(operatorv1alpha1.MigrationRunOutcomePartial), OperationID: "sha256:0ee5",
		JobName: "ptah-m-apply-deliveries-0123456789abcdef", JobUID: "apply-uid",
		PlanRef:    operatorv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-0123456789abcdef01234567", UID: "plan-uid"},
		RecordedAt: demoObserved,
	}
	view.Conditions = demoConditions(
		metav1.Condition{Type: "Blocked", Status: "True", Reason: "ApplyOutcomeUnknown", Message: "migration 2 failed at statement 3 of 3"},
		metav1.Condition{Type: "Ready", Status: "False", Reason: "ApplyOutcomeUnknown", Message: "The database has to be read by a person before anything else runs"},
	)
	return view
}

// demoPartialSettled is the same resource once a reading of the history found
// nothing of release 1 left to apply and settled the run.
func demoPartialSettled() migrationview.View {
	view := demoPartial()
	view.Phase = string(operatorv1alpha1.MigrationPhaseInSync)
	view.History.PendingCount = 0
	view.UnresolvedRun = nil
	view.ResolvedRun = &migrationview.ResolvedRunView{
		OperationID: "sha256:0ee5", Outcome: string(operatorv1alpha1.MigrationRunOutcomePartial),
		Resolution: string(operatorv1alpha1.MigrationRunResolvedByHistoryRead), ResolvedAt: demoObserved,
	}
	view.Conditions = demoConditions(
		metav1.Condition{Type: "Blocked", Status: "False", Reason: "HistoryMatched", Message: "The history continues this artifact"},
		metav1.Condition{Type: "Ready", Status: "True", Reason: "HistoryMatched", Message: "The database has every migration this artifact carries"},
	)
	return view
}
