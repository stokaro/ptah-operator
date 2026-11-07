package e2e

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// stoppedRunRead is the reading that shows a run stopped its resource. The run is
// recorded with this outcome against a Job it names, the database was read after the
// run ended, and the read refuses on the revision row the run left. The phase is left
// out on purpose: a resource that stopped still resolves and reads at its interval.
func stoppedRunRead(status ptahv1alpha1.PtahMigrationStatus, outcome ptahv1alpha1.MigrationRunOutcome) bool {
	run, history := status.LastRun, status.History
	return run != nil && run.Outcome == outcome && run.JobName != "" && run.JobUID != "" && run.FinishedAt != nil &&
		history != nil && history.Dirty && history.ObservedAt.After(run.FinishedAt.Time) &&
		conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "HistoryDirty")
}

// readAgainSince is a reading of the database after the one given, which is what an
// Apply under `apply: Always` would have followed, were anything left to run.
func readAgainSince(status ptahv1alpha1.PtahMigrationStatus, previous metav1.Time) bool {
	return !previous.IsZero() && status.History != nil && status.History.ObservedAt.After(previous.Time)
}

// onlyTheRunDispatched holds a resource's Apply Jobs to the one run the row is about.
// Nothing at all means the list read nothing, and a second one is the pile-up the long
// TTL may not cost.
func onlyTheRunDispatched(applies []string, jobUID string) error {
	if len(applies) != 1 || applies[0] != jobUID {
		return fmt.Errorf("the Apply Jobs are %v, want only %s", applies, jobUID)
	}
	return nil
}
