package e2e

import (
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// lockWaitBound is how soon after a realm Lease comes free a claim waiting for
// it creates its Job. The claim asks every five seconds and needs two passes, one
// to record the epoch and one to dispatch, so this leaves room for a loaded
// cluster. The row's Lease has 300 seconds on it when the claim loses, and a
// claim that slept for what the Lease had left misses this by minutes.
const lockWaitBound = 60 * time.Second

// migrationLockWaitReported is an Apply claim that says it is waiting for the
// Lease named: Progressing True for WaitingForDatabaseLock.
func migrationLockWaitReported(migration *ptahv1alpha1.PtahMigration, lease string) bool {
	operation := migration.Status.ActiveOperation
	return operation != nil && operation.Type == ptahv1alpha1.MigrationOperationApply && operation.JobUID == "" &&
		conditionIs(migration.Status.Conditions, ptahv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue, "WaitingForDatabaseLock") &&
		strings.Contains(conditionMessage(migration.Status.Conditions, ptahv1alpha1.ConditionMigrationProgressing), lease)
}

// conditionMessage is the message of the condition of a type, empty with none.
func conditionMessage(conditions []metav1.Condition, conditionType string) string {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Message
		}
	}
	return ""
}

// leaseLastWrittenBy is when the API server recorded the last write a field
// manager made to a Lease. The instant comes from the same clock as a Job's
// creationTimestamp, so the two can be compared wherever the harness runs.
func leaseLastWrittenBy(lease *coordinationv1.Lease, manager string) (time.Time, error) {
	var written time.Time
	for _, entry := range lease.ManagedFields {
		if entry.Manager == manager && entry.Time != nil && entry.Time.After(written) {
			written = entry.Time.Time
		}
	}
	if written.IsZero() {
		return time.Time{}, fmt.Errorf("Lease %s records no write by %s", lease.Name, manager)
	}
	return written, nil
}

// jobCreatedPromptlyAfter refuses a Job created before the Lease it waited for
// came free, which would prove nothing about the wait, and one created past the
// bound.
func jobCreatedPromptlyAfter(job *batchv1.Job, free time.Time, bound time.Duration) error {
	created := job.CreationTimestamp.Time
	if created.IsZero() {
		return fmt.Errorf("Job %s has no creation time", job.Name)
	}
	// Both are whole seconds, so the same second is as good as after.
	if created.Before(free.Truncate(time.Second)) {
		return fmt.Errorf("Job %s was created at %s, before the Lease came free at %s", job.Name, created, free)
	}
	if waited := created.Sub(free); waited > bound {
		return fmt.Errorf("Job %s was created %s after the Lease came free, past the %s a claim that keeps asking takes", job.Name, waited, bound)
	}
	return nil
}
