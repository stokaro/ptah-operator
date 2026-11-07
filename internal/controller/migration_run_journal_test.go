package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// runEnds is every RunFinished entry the migration's journal holds.
func runEnds(migration *operatorv1alpha1.PtahMigration) []operatorv1alpha1.JournalEntry {
	if migration.Status.Journal == nil {
		return nil
	}
	var ends []operatorv1alpha1.JournalEntry
	for _, entry := range migration.Status.Journal.Entries {
		if entry.Type == operatorv1alpha1.JournalRunFinished {
			ends = append(ends, entry)
		}
	}
	return ends
}

// oneRunEnd fails unless the journal holds exactly one RunFinished, and returns
// it.
func oneRunEnd(t *testing.T, migration *operatorv1alpha1.PtahMigration) operatorv1alpha1.JournalEntry {
	t.Helper()
	ends := runEnds(migration)
	if len(ends) != 1 {
		t.Fatalf("the journal holds %d RunFinished entries for one execution, want one: %#v", len(ends), ends)
	}
	return ends[0]
}

// A run the database could not account for writes lastRun and unresolvedRun in
// one status write. Through the controller, that is one end with the outcome the
// run reported and the plan it carried out.
func TestARunNobodyAccountedForEndsOnceInTheJournal(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		outcome operatorv1alpha1.MigrationRunOutcome
		want    operatorv1alpha1.JournalOutcome
	}{
		{operatorv1alpha1.MigrationRunOutcomePartial, operatorv1alpha1.JournalPartial},
		{operatorv1alpha1.MigrationRunOutcomeUnknown, operatorv1alpha1.JournalUnknown},
	} {
		t.Run(string(row.outcome), func(t *testing.T) {
			t.Parallel()

			migration := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways, row.outcome)
			end := oneRunEnd(t, migration)
			run, hold := migration.Status.LastRun, migration.Status.UnresolvedRun
			if end.Outcome != row.want || end.Plan != hold.PlanRef.Name {
				t.Fatalf("end = %#v, want outcome %q on plan %q", end, row.want, hold.PlanRef.Name)
			}
			if !end.At.Equal(run.FinishedAt) || end.StartedAt == nil || !end.StartedAt.Equal(&run.StartedAt) {
				t.Fatalf("end = %#v, want the times lastRun carries: %#v", end, run)
			}
		})
	}
}

// An Apply the controller could not read keeps its claim while its workload may
// still write, and retires it later. Every pass from the first record to the
// retirement and the one after it leaves one end.
func TestAnUncertainRetirementEndsOnceInTheJournal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	migration, plan := awaitingApprovalFixture(t)
	migration.Finalizers = []string{migrationOperationFinalizer}
	applyClaimFor(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	runningExecutorPod(pod)
	job.Status.Conditions = nil
	job.Status.Active = 1
	reconciler, api := uncertainApplyUnderBindingChange(t, migration, plan, job, pod)

	reconcile := func() *operatorv1alpha1.PtahMigration {
		t.Helper()
		if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		return readMigration(t, api, migration)
	}
	held := reconcile()
	if held.Status.ActiveOperation == nil || held.Status.UnresolvedRun == nil {
		t.Fatalf("the fixture did not hold the claim beside its record: %#v", held.Status)
	}
	first := oneRunEnd(t, held)
	if first.Outcome != operatorv1alpha1.JournalUnknown || first.Plan != plan.Name {
		t.Fatalf("end = %#v, want Unknown on plan %q", first, plan.Name)
	}
	oneRunEnd(t, reconcile())

	finished := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), finished); err != nil {
		t.Fatal(err)
	}
	finished.Status.Active = 0
	finished.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := api.Status().Update(ctx, finished); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = nil
	if err := api.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	retired := reconcile()
	if retired.Status.ActiveOperation != nil || retired.Status.UnresolvedRun == nil {
		t.Fatalf("the claim did not retire beside its record: %#v", retired.Status)
	}
	if last := oneRunEnd(t, retired); !last.At.Equal(&first.At) {
		t.Fatalf("the retirement rewrote the end: %#v, was %#v", last, first)
	}
	oneRunEnd(t, reconcile())
}

// A restore that drops status drops the journal, and the hold it brings back from
// metadata is all that is left of how the run ended. It is recorded once, under
// the outcome it holds rather than as Unknown, on the pass that restores it and
// on no pass after.
func TestARestoredHoldIsTheRunsOneEndInTheJournal(t *testing.T) {
	t.Parallel()

	recorded := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways, operatorv1alpha1.MigrationRunOutcomePartial)
	restored := restoredWithoutStatus(recorded)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, restored, verificationPolicyConfigMap())
	for pass := range 2 {
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(restored)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		actual := readMigration(t, api, restored)
		end := oneRunEnd(t, actual)
		hold := recorded.Status.UnresolvedRun
		if end.Outcome != operatorv1alpha1.JournalPartial || end.Plan != hold.PlanRef.Name || !end.At.Equal(&hold.RecordedAt) {
			t.Fatalf("pass %d: end = %#v, want Partial on plan %q at %s", pass, end, hold.PlanRef.Name, hold.RecordedAt)
		}
	}
}
