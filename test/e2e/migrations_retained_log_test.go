package e2e

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func retainedRunReading() ptahv1alpha1.PtahMigrationStatus {
	finished := metav1.NewTime(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	return ptahv1alpha1.PtahMigrationStatus{
		LastRun: &ptahv1alpha1.MigrationRunStatus{
			Outcome: ptahv1alpha1.MigrationRunOutcomeFailed, JobName: "ptah-apply", JobUID: "job-uid", FinishedAt: &finished,
		},
		History: &ptahv1alpha1.MigrationHistoryStatus{Dirty: true, ObservedAt: metav1.NewTime(finished.Add(10 * time.Second))},
		Conditions: []metav1.Condition{{
			Type: ptahv1alpha1.ConditionMigrationBlocked, Status: metav1.ConditionTrue, Reason: "HistoryDirty",
		}},
	}
}

func TestStoppedRunReadRefusesWhatDoesNotShowTheStop(t *testing.T) {
	t.Parallel()

	if !stoppedRunRead(retainedRunReading(), ptahv1alpha1.MigrationRunOutcomeFailed) {
		t.Fatal("the reading after a run that left a row was refused")
	}
	if stoppedRunRead(retainedRunReading(), ptahv1alpha1.MigrationRunOutcomePartial) {
		t.Fatal("a run of another outcome was accepted")
	}
	for name, change := range map[string]func(*ptahv1alpha1.PtahMigrationStatus){
		"no run recorded":   func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil },
		"a run with no Job": func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.JobUID = "" },
		"a run not over":    func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.FinishedAt = nil },
		"no reading":        func(s *ptahv1alpha1.PtahMigrationStatus) { s.History = nil },
		"a clean reading":   func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.Dirty = false },
		"a reading from before the run": func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.History.ObservedAt = metav1.NewTime(s.LastRun.FinishedAt.Add(-time.Second))
		},
		"a reading in the same instant": func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.History.ObservedAt = *s.LastRun.FinishedAt
		},
		"blocked for another reason": func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.Conditions[0].Reason = "ApplyOutcomeUnknown"
		},
		"no longer blocked": func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.Conditions[0].Status = metav1.ConditionFalse
		},
		"no condition": func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := retainedRunReading()
			change(&status)
			if stoppedRunRead(status, ptahv1alpha1.MigrationRunOutcomeFailed) {
				t.Fatalf("stoppedRunRead() accepted %s", name)
			}
		})
	}
}

func TestReadAgainSinceDatesTheReadingByItsOwnTimestamp(t *testing.T) {
	t.Parallel()

	status := retainedRunReading()
	first := status.History.ObservedAt
	if readAgainSince(status, first) {
		t.Error("the same reading counted as another")
	}
	if !readAgainSince(status, metav1.NewTime(first.Add(-time.Second))) {
		t.Error("a later reading was refused")
	}
	if readAgainSince(status, metav1.Time{}) {
		t.Error("a reading with nothing to compare it to counted")
	}
	status.History = nil
	if readAgainSince(status, first) {
		t.Error("no reading counted")
	}
}

func TestOnlyTheRunDispatchedHoldsTheCount(t *testing.T) {
	t.Parallel()

	if err := onlyTheRunDispatched([]string{"job-uid"}, "job-uid"); err != nil {
		t.Errorf("the one run: %v", err)
	}
	for name, applies := range map[string][]string{
		"nothing read": nil,
		"another Job":  {"another"},
		"a second run": {"another", "job-uid"},
	} {
		if err := onlyTheRunDispatched(applies, "job-uid"); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
