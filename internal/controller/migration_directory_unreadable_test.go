package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// capturedDirectoryReason is the reason the pinned Ptah image gave for a
// migration directory, read from the document the runner harness captured, the
// way the runner reads it.
func capturedDirectoryReason(t *testing.T, fixture string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "dataplane", "testdata", "desired-source", fixture))
	if err != nil {
		t.Fatal(err)
	}
	reason, found := dataplane.MigrationFilesProblem(data)
	if !found {
		t.Fatalf("%s holds no reason about the files", fixture)
	}
	return reason
}

// A history read whose directory Ptah could not read stops the resource: the
// same files give the same answer, so a retry would dispatch a Job every
// failure retry and say nothing new. It stops Blocked with Ptah's words, names
// the file, publishes no plan, and reads again at its interval. Since Ptah
// v0.33.1 that covers a SQL file the directory's format does not read, which
// v0.33.0 left out while it ran the files beside it. A failure with another
// code is a failed read as before, and is tried again.
func TestAHistoryReadOfAnUnreadableDirectoryStopsBlocked(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, code, message, file string
		blocked                   bool
	}{
		{
			name: "a SQL file the Ptah format does not read", code: runner.CodeDesiredSourceUnreadable,
			message: capturedDirectoryReason(t, "scratch-unread.json"), file: "seed.sql", blocked: true,
		},
		{
			name: "an Atlas repeatable file", code: runner.CodeDesiredSourceUnreadable,
			message: capturedDirectoryReason(t, "scratch-atlas-repeatable.json"), file: "R__seed.sql", blocked: true,
		},
		{
			name: "a directive the loader refuses", code: runner.CodeDesiredSourceUnreadable,
			message: capturedDirectoryReason(t, "scratch-directive.json"), file: "0000000001_init.up.sql", blocked: true,
		},
		{
			name: "an exit status with no reason", code: "child_exit", message: "ptah exited with code 2",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Spec.Interval = metav1.Duration{Duration: 37 * time.Minute}
			migration.Status.ExecutionBinding = migrationExecutionBinding()
			migration.Status.Artifact = resolvedMigrationArtifact()
			migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
			migration.Finalizers = []string{migrationOperationFinalizer}
			operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			frame := migrationFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
				OperationID: operation.ID, ChildExitCode: 2,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: testDigest,
				Error:                &runner.ResultError{Code: row.code, Message: row.message},
			})
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{content: frame}, migration, job, pod, verificationPolicyConfigMap(),
			)
			recorder := record.NewFakeRecorder(16)
			reconciler.Recorder = recorder
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			plans := &operatorv1alpha1.PtahMigrationPlanList{}
			if err := api.List(context.Background(), plans); err != nil {
				t.Fatal(err)
			}
			if actual.Status.History != nil || actual.Status.Plan != nil || len(plans.Items) != 0 {
				t.Fatalf("the refused read was recorded: history %#v, plan %#v, %d plans", actual.Status.History, actual.Status.Plan, len(plans.Items))
			}
			if !row.blocked {
				if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Attempt != operation.Attempt+1 {
					t.Fatalf("the failed read was not retried: %#v", actual.Status.ActiveOperation)
				}
				if blocked := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked); blocked != nil &&
					blocked.Reason == string(operatorv1alpha1.ReasonMigrationDirectoryUnreadable) {
					t.Fatalf("Blocked = %#v for a failure that is not about the files", blocked)
				}
				return
			}
			if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked ||
				actual.Status.ObservedGeneration != migration.Generation {
				t.Fatalf("status after the refusal: phase %s, claim %#v, observed %d",
					actual.Status.Phase, actual.Status.ActiveOperation, actual.Status.ObservedGeneration)
			}
			// One interval from the read's end, on the reconciler's clock: the
			// failure retry would be minutes or seconds.
			want := reconciler.now().Add(37 * time.Minute)
			if next := actual.Status.NextReconciliationTime; next == nil || !next.Time.Equal(want) {
				t.Fatalf("NextReconciliationTime = %v, want %v", next, want)
			}
			said := runner.CodeDesiredSourceUnreadable + ": " + row.message
			for _, condition := range []struct {
				conditionType string
				status        metav1.ConditionStatus
				says          string
			}{
				{operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, said},
				{operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, "could not read the migration directory"},
				{operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, "Nothing runs"},
			} {
				found := meta.FindStatusCondition(actual.Status.Conditions, condition.conditionType)
				if found == nil || found.Status != condition.status ||
					found.Reason != string(operatorv1alpha1.ReasonMigrationDirectoryUnreadable) || !strings.Contains(found.Message, condition.says) {
					t.Fatalf("%s = %#v, want %s for MigrationDirectoryUnreadable saying %q", condition.conditionType, found, condition.status, condition.says)
				}
			}
			if !strings.Contains(said, row.file) {
				t.Fatalf("the reason %q does not name %s", said, row.file)
			}
			event := <-recorder.Events
			if want := corev1.EventTypeWarning + " MigrationDirectoryUnreadable " + said; event != want {
				t.Fatalf("Event = %q, want %q", event, want)
			}

			// The next pass before the interval claims nothing: the same files
			// would give the same answer.
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(actual)); err != nil {
				t.Fatalf("second Reconcile() error = %v", err)
			}
			if again := readMigration(t, api, migration); again.Status.ActiveOperation != nil {
				t.Fatalf("the pass before the interval claimed %#v", again.Status.ActiveOperation)
			}
		})
	}
}
