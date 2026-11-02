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
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// A scenario step that runs `kubectl ptah migration` expects lines of its text
// output, padding included, and the recorder holds them only against a live
// lab. A renderer that moved its label column would be found by the next
// recording, an hour into it. This renders the state each such step waits for
// and requires every string the step expects in it, and every string it
// refuses out of it, before any lab exists.
//
// The states are written here, one per plugin step and keyed by the step's
// place among the scenario's plugin steps. A step that prints the plugin's
// text and has no state here fails the test, so a new one cannot go unpinned.
func TestPluginTextStepsExpectWhatTheRendererWrites(t *testing.T) {
	t.Parallel()
	loaded, err := loadScenarios(filepath.Join("..", "..", "scenarios"))
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]migrationview.View{
		"checkpoint-bootstrap#1": demoInSync(4, 3, 3, 4),
		"second-release#1":       demoInSync(1, 0, 1),
		"versioned-migration#1":  demoAwaitingApproval(),
		"versioned-migration#2":  demoInSync(2, 0, 1, 2),
	}
	checked := map[string]bool{}
	for _, one := range loaded {
		occurrence := 0
		for _, current := range one.Steps {
			if !strings.Contains(current.Run, "kubectl ptah ") || strings.Contains(current.Run, " -o ") {
				continue
			}
			occurrence++
			key := fmt.Sprintf("%s#%d", one.ID, occurrence)
			if !strings.Contains(current.Run, "kubectl ptah migration ") {
				t.Errorf("%s prints a kubectl ptah text view this test has no renderer for: %s", key, current.Run)
				continue
			}
			state, ok := states[key]
			if !ok {
				t.Errorf("%s prints kubectl ptah migration text and this test has no state for it", key)
				continue
			}
			checked[key] = true
			var text strings.Builder
			if err := migrationview.Render(&text, state, migrationview.Text); err != nil {
				t.Fatal(err)
			}
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
		t.Fatal("no scenario step runs kubectl ptah migration, so this test held nothing")
	}
	for key := range states {
		if !checked[key] {
			t.Errorf("%s names no plugin step any scenario runs", key)
		}
	}
}

// demoAwaitingApproval is versioned-migration's plan of two migrations against
// an empty history, waiting for a decision, with the conditions the
// controller writes for it.
func demoAwaitingApproval() migrationview.View {
	observed := metav1.NewTime(time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC))
	return migrationview.View{
		Namespace: "demo", Migration: "shipments", UID: "shipments-uid", Generation: 1, ObservedGeneration: 1,
		Phase:   string(operatorv1alpha1.MigrationPhaseAwaitingApproval),
		Policy:  &migrationview.PolicyView{Apply: string(operatorv1alpha1.ApplyPolicyOnApproval)},
		History: &migrationview.HistoryView{ObservedAt: observed, PendingCount: 2},
		Plan: &migrationview.PlanView{
			Name: "ptah-mplan-0123456789abcdef01234567", CreatedAt: observed,
			Migrations: []migrationview.PlannedMigrationView{
				{Version: 1, Description: "Create Shipments"},
				{Version: 2, Description: "Add Carrier"},
			},
		},
		Conditions: statusview.FromConditions([]metav1.Condition{
			{Type: "Progressing", Status: "False", Reason: "AwaitingApproval", Message: "Nothing runs until a person writes the approval", LastTransitionTime: observed},
			{Type: "Blocked", Status: "False", Reason: "HistoryMatched", Message: "The history continues this artifact", LastTransitionTime: observed},
			{Type: "Ready", Status: "False", Reason: "AwaitingApproval", Message: "The plan is waiting for the approval its policy requires", LastTransitionTime: observed},
			{Type: "ApprovalRequired", Status: "True", Reason: "AwaitingApproval", Message: "2 planned migrations need an approval naming this plan", LastTransitionTime: observed},
		}),
		Approvals:       &statusview.Decisions[migrationview.ApprovalView]{Items: []migrationview.ApprovalView{}},
		Acknowledgments: &statusview.Decisions[migrationview.AcknowledgmentView]{Items: []migrationview.AcknowledgmentView{}},
	}
}

// demoInSync is a migration whose history is at current, with checkpoint
// standing in for the versions below it where it is not zero, and whose last
// run applied versions.
func demoInSync(current, checkpoint int64, versions ...int64) migrationview.View {
	observed := metav1.NewTime(time.Date(2026, 10, 10, 21, 5, 0, 0, time.UTC))
	finished := metav1.NewTime(observed.Add(-time.Minute))
	view := demoAwaitingApproval()
	view.Phase = string(operatorv1alpha1.MigrationPhaseInSync)
	view.History = &migrationview.HistoryView{
		ObservedAt: observed, CurrentVersion: current, CheckpointVersion: checkpoint, AppliedCount: int32(current),
	}
	view.Plan = nil
	view.LastRun = &migrationview.RunView{
		Outcome: string(operatorv1alpha1.MigrationRunOutcomeApplied), JobName: "ptah-m-apply-shipments-0123456789abcdef",
		JobUID: "apply-uid", StartedAt: metav1.NewTime(observed.Add(-2 * time.Minute)), FinishedAt: &finished,
		AppliedVersions: versions,
	}
	view.Conditions = statusview.FromConditions([]metav1.Condition{
		{Type: "Progressing", Status: "False", Reason: "HistoryMatched", Message: "Nothing is pending", LastTransitionTime: observed},
		{Type: "Blocked", Status: "False", Reason: "HistoryMatched", Message: "The history continues this artifact", LastTransitionTime: observed},
		{Type: "Ready", Status: "True", Reason: "HistoryMatched", Message: "The database has every migration this artifact carries", LastTransitionTime: observed},
		{Type: "ApprovalRequired", Status: "False", Reason: "Satisfied", Message: "The plan's approval requirements are satisfied", LastTransitionTime: observed},
	})
	return view
}
