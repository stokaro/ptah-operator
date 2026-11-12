package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// A PtahMigration harvest has no horizon wait for an executor the kubelet ended
// with ContainerStatusUnknown, where a PtahSchema Apply waits for its execution
// horizon before it reads the database to prove the outcome. The migration
// takes nothing from the database here: the pass that reads the lost executor
// records the run Unknown and unresolved at once, reads no log, and dispatches
// nothing. The next cycle, a Resolve and then the History read, waits for the
// resource's interval, and an unresolved run is settled only by a reading with
// nothing of the artifact left to apply (migrationUnresolvedRunSettledBy), or
// by a person.
func TestAMigrationHarvestOfALostExecutorSettlesUnknownAndReadsNothing(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		end  func(t *testing.T, run unstartedMigration)
	}{
		{
			name: "an executor that never started",
			end: func(t *testing.T, run unstartedMigration) {
				deadlineStoppedAWaitingPod(t, run.api, run.job, run.pod)
			},
		},
		{
			name: "an executor the runtime lost while it ran",
			end: func(t *testing.T, run unstartedMigration) {
				firstPodStarted(t, run.api, run.pod, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
				reconcileMigration(t, run.reconciler, run.api, run.migration)
				lostWhileItRan(t, run.api, run.job, run.pod)
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run := unstartedMigrationApplyRun(t)
			logs := &countedLogs{err: kubeletRefusesALostContainer()}
			run.reconciler.Logs = logs
			row.end(t, run)
			claim := readMigration(t, run.api, run.migration).Status.ActiveOperation
			now := run.reconciler.now()
			if claim == nil || claim.ExecutionNotAfter == nil || !now.Before(claim.ExecutionNotAfter.Time) {
				t.Fatalf("the harvest runs at %s, not before the Apply's horizon, so it cannot show it did not wait: claim %#v", now, claim)
			}

			result, err := run.reconciler.Reconcile(ctx, migrationRequest(run.migration))
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			settled := readMigration(t, run.api, run.migration)
			record := settledMigrationRun(t, run.api, run.migration)
			switch {
			case settled.Status.UnresolvedRun.OperationID != claim.ID:
				t.Errorf("the unresolved run names operation %q, want the claim's %q", settled.Status.UnresolvedRun.OperationID, claim.ID)
			case record.Message == "":
				t.Error("the record says nothing of how the run ended")
			case settled.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked:
				t.Errorf("phase = %q, want %q until the run is accounted for", settled.Status.Phase, operatorv1alpha1.MigrationPhaseBlocked)
			}
			if reads := logs.count(); reads != 0 {
				t.Errorf("the harvest read the log of an executor the kubelet lost %d times, want none", reads)
			}
			next := settled.Status.NextReconciliationTime
			if next == nil || !next.Time.Equal(now.Add(migrationInterval(settled))) {
				t.Fatalf("the next reading is due at %v, want the interval from the harvest, %s", next, now.Add(migrationInterval(settled)))
			}
			if want := next.Sub(now); result.RequeueAfter != want {
				t.Errorf("the harvest requeues after %s, want %s, when the next reading is due", result.RequeueAfter, want)
			}
			onlyTheApplyJob(t, run, "the harvest")

			// A pass before the reading is due dispatches nothing either. The
			// passes once it is due start the next cycle with the Resolve the
			// History read follows, which shows the check above sees a Job a
			// pass dispatches.
			early := next.Add(-time.Second)
			run.reconciler.Clock = func() time.Time { return early }
			reconcileMigration(t, run.reconciler, run.api, run.migration)
			onlyTheApplyJob(t, run, "a pass before the next reading is due")
			due := next.Time
			run.reconciler.Clock = func() time.Time { return due }
			for range 3 {
				reconcileMigration(t, run.reconciler, run.api, run.migration)
			}
			jobs := &batchv1.JobList{}
			if err := run.api.List(ctx, jobs); err != nil {
				t.Fatal(err)
			}
			resolves := 0
			for _, job := range jobs.Items {
				if job.UID != run.job.UID && job.Labels[workload.LabelOperation] == strings.ToLower(string(operatorv1alpha1.MigrationOperationResolve)) {
					resolves++
				}
			}
			if len(jobs.Items) != 2 || resolves != 1 {
				t.Fatalf("the passes once the reading is due left %d Jobs, %d of them a Resolve, want the Apply's and the next cycle's Resolve",
					len(jobs.Items), resolves)
			}
		})
	}
}

// onlyTheApplyJob fails unless the Apply's Job is the only Job in the
// migration's namespace, so after names a pass that dispatched no read.
func onlyTheApplyJob(t *testing.T, run unstartedMigration, after string) {
	t.Helper()

	jobs := &batchv1.JobList{}
	if err := run.api.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 || jobs.Items[0].UID != run.job.UID {
		names := make([]string, 0, len(jobs.Items))
		for _, job := range jobs.Items {
			names = append(names, job.Name)
		}
		t.Fatalf("after %s the cluster holds Jobs %q, want the Apply's %s alone", after, names, run.job.Name)
	}
}
