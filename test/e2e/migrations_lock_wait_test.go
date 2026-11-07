package e2e

import (
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func lockWaitMigration(mutate func(*ptahv1alpha1.PtahMigration)) *ptahv1alpha1.PtahMigration {
	migration := &ptahv1alpha1.PtahMigration{}
	migration.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationApply, JobName: "apply-job"}
	migration.Status.Conditions = []metav1.Condition{{
		Type: ptahv1alpha1.ConditionMigrationProgressing, Status: metav1.ConditionTrue, Reason: "WaitingForDatabaseLock",
		Message: "Apply is waiting for the database lock: another operation on this database holds Lease ptah-t-abc. It asks again every 5s",
	}}
	if mutate != nil {
		mutate(migration)
	}
	return migration
}

func TestMigrationLockWaitReportedIsAClaimWithNoJobNamingTheLease(t *testing.T) {
	t.Parallel()
	if !migrationLockWaitReported(lockWaitMigration(nil), "ptah-t-abc") {
		t.Fatal("migrationLockWaitReported() refused the reading the operator writes")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration){
		"no claim": func(migration *ptahv1alpha1.PtahMigration) { migration.Status.ActiveOperation = nil },
		"a read": func(migration *ptahv1alpha1.PtahMigration) {
			migration.Status.ActiveOperation.Type = ptahv1alpha1.MigrationOperationResolve
		},
		"a Job already dispatched": func(migration *ptahv1alpha1.PtahMigration) { migration.Status.ActiveOperation.JobUID = "job-uid" },
		"another reason": func(migration *ptahv1alpha1.PtahMigration) {
			migration.Status.Conditions[0].Reason = "OperationInProgress"
		},
		"the reason that replaced this one's silence": func(migration *ptahv1alpha1.PtahMigration) { migration.Status.Conditions[0].Reason = "ApprovedPlan" },
		"Progressing False": func(migration *ptahv1alpha1.PtahMigration) {
			migration.Status.Conditions[0].Status = metav1.ConditionFalse
		},
		"another condition": func(migration *ptahv1alpha1.PtahMigration) { migration.Status.Conditions[0].Type = "Ready" },
		"another Lease": func(migration *ptahv1alpha1.PtahMigration) {
			migration.Status.Conditions[0].Message = "Apply is waiting for the database lock: another operation on this database holds Lease ptah-t-xyz"
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if migrationLockWaitReported(lockWaitMigration(mutate), "ptah-t-abc") {
				t.Fatal("migrationLockWaitReported() accepted it")
			}
		})
	}
}

func TestLeaseLastWrittenByReadsTheServerClockOfOneManager(t *testing.T) {
	t.Parallel()
	first := metav1.NewTime(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	second := metav1.NewTime(first.Add(time.Minute))
	third := metav1.NewTime(first.Add(2 * time.Minute))
	lease := &coordinationv1.Lease{}
	lease.Name = "ptah-t-abc"
	lease.ManagedFields = []metav1.ManagedFieldsEntry{
		{Manager: "harness", Time: &first},
		{Manager: "harness", Time: &second},
		{Manager: "manager", Time: &third},
		{Manager: "harness"},
	}
	written, err := leaseLastWrittenBy(lease, "harness")
	if err != nil || !written.Equal(second.Time) {
		t.Fatalf("leaseLastWrittenBy() = %s, %v, want the harness's newest write %s", written, err, second)
	}
	if _, err := leaseLastWrittenBy(lease, "nobody"); err == nil {
		t.Fatal("leaseLastWrittenBy() accepted a manager with no write")
	}
	if _, err := leaseLastWrittenBy(&coordinationv1.Lease{}, "harness"); err == nil {
		t.Fatal("leaseLastWrittenBy() accepted a Lease with no managed fields")
	}
}

func TestJobCreatedPromptlyAfterHoldsTheJobToTheBound(t *testing.T) {
	t.Parallel()
	free := time.Date(2026, 11, 7, 12, 0, 0, 400000000, time.UTC)
	job := func(created time.Time) *batchv1.Job {
		object := &batchv1.Job{}
		object.Name = "apply-job"
		object.CreationTimestamp = metav1.NewTime(created)
		return object
	}
	for _, row := range []struct {
		name    string
		created time.Time
		refusal string
	}{
		{name: "seconds after", created: free.Add(7 * time.Second)},
		{name: "in the same second, to the server's whole seconds", created: free.Truncate(time.Second)},
		{name: "at the bound", created: free.Add(lockWaitBound)},
		{name: "before the Lease came free", created: free.Add(-30 * time.Second), refusal: "before the Lease came free"},
		{name: "what the Lease had left", created: free.Add(285 * time.Second), refusal: "past the"},
		{name: "just past the bound", created: free.Add(lockWaitBound + time.Second), refusal: "past the"},
		{name: "no creation time", refusal: "no creation time"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			err := jobCreatedPromptlyAfter(job(row.created), free, lockWaitBound)
			if row.refusal == "" {
				if err != nil {
					t.Fatalf("jobCreatedPromptlyAfter() = %v, want it accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), row.refusal) {
				t.Fatalf("jobCreatedPromptlyAfter() = %v, want a refusal saying %q", err, row.refusal)
			}
		})
	}
}
