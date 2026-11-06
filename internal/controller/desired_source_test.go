package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// unreadableReason is the reason the runner reported for the file
// stokaro/ptah-operator#788 delivers, as the pinned image words it.
const unreadableReason = "error parsing schema file: read SQL schema file /source/schema/schema.sql: " +
	"expected '(' after table name: expected Operator, got Identifier at position 180"

// readOnlySchemaClaim is a schema whose read-only operation has finished.
func readOnlySchemaClaim(t *testing.T, operation operatorv1alpha1.OperationType) *operatorv1alpha1.PtahSchema {
	t.Helper()
	schema := safetyApplySchema(t)
	schema.Status.Phase = operatorv1alpha1.PhaseObserving
	schema.Status.Plan = nil
	schema.Status.Source.Verified = true
	schema.Status.Source.Digest = testDigest
	schema.Status.Source.ResolvedReference = "oci://registry.example/team/schema@" + testDigest
	schema.Status.Target.DriftReportDigest = testDigest
	schema.Status.ActiveOperation.Type = operation
	schema.Status.ActiveOperation.ID = "read-operation"
	schema.Status.ActiveOperation.JobName = "read-job"
	schema.Status.ActiveOperation.LeaseEpoch = testLeaseEpoch
	bindActiveInput(t, schema)
	return schema
}

// TestASchemaSaysWhyItsDesiredFileCouldNotBeRead reads the reason where a person
// reads it, and holds the failure to the path every other read-only failure
// takes: the next attempt of the same claim after the retry interval.
func TestASchemaSaysWhyItsDesiredFileCouldNotBeRead(t *testing.T) {
	t.Parallel()

	for name, operation := range map[string]struct {
		claim  operatorv1alpha1.OperationType
		runner runner.Operation
	}{
		"Observe": {operatorv1alpha1.OperationObserve, runner.OperationObserve},
		"Plan":    {operatorv1alpha1.OperationPlan, runner.OperationPlan},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := readOnlySchemaClaim(t, operation.claim)
			attempt := schema.Status.ActiveOperation.Attempt
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			frame := safetyRunnerFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: operation.runner, OperationID: "read-operation",
				ChildExitCode:      2,
				CoordinationDigest: schema.Status.ActiveOperation.CoordinationDigest,
				Error:              &runner.ResultError{Code: runner.CodeDesiredSourceUnreadable, Message: unreadableReason},
			})
			reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod)
			reconciler.Locks = targetlock.New(api, api, nil)
			recorder := record.NewFakeRecorder(32)
			reconciler.Recorder = recorder

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := safetyGetSchema(t, api, schema)
			want := runner.CodeDesiredSourceUnreadable + ": " + unreadableReason
			failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
			if failed == nil || failed.Status != metav1.ConditionTrue || failed.Reason != string(operatorv1alpha1.ReasonOperationFailed) ||
				failed.Message != want {
				t.Fatalf("ReconciliationFailed = %#v, want OperationFailed with %q", failed, want)
			}
			if event := findEvent(recorder, "Warning OperationFailed"); event != "Warning OperationFailed "+want {
				t.Fatalf("OperationFailed event = %q, want %q", event, want)
			}
			if actual.Status.Phase != operatorv1alpha1.PhaseFailed || actual.Status.ActiveOperation == nil ||
				actual.Status.ActiveOperation.Type != operation.claim || actual.Status.ActiveOperation.Attempt != attempt+1 ||
				result.RequeueAfter <= 0 {
				t.Fatalf("the failure did not retry the claim: phase %s, operation %+v, result %+v",
					actual.Status.Phase, actual.Status.ActiveOperation, result)
			}
		})
	}
}

// TestAMigrationSaysWhyItsFilesCouldNotBeRead is the same reading on a
// migration's History run, where the retry names the failure on Progressing and
// in the OperationRetried Event.
func TestAMigrationSaysWhyItsFilesCouldNotBeRead(t *testing.T) {
	t.Parallel()

	reason := `failed to load up migration 0000000001_init.up.sql: invalid migration directives in ` +
		`0000000001_init.up.sql: invalid +ptah no_transaction value "sometimes": expected true or false`
	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Artifact = resolvedMigrationArtifact()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
		OperationID: operation.ID, ChildExitCode: 2,
		Error: &runner.ResultError{Code: runner.CodeDesiredSourceUnreadable, Message: reason},
	})
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, job, pod, verificationPolicyConfigMap())
	recorder := record.NewFakeRecorder(32)
	reconciler.Recorder = recorder

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	want := runner.CodeDesiredSourceUnreadable + ": " + reason
	progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Reason != string(operatorv1alpha1.ReasonOperationFailed) || progressing.Message != want {
		t.Fatalf("Progressing = %#v, want OperationFailed with %q", progressing, want)
	}
	if event := findEvent(recorder, "Warning OperationRetried"); !strings.HasSuffix(event, ": "+want) {
		t.Fatalf("OperationRetried event = %q, want it to end with %q", event, want)
	}
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.MigrationOperationHistory ||
		actual.Status.ActiveOperation.Attempt != operation.Attempt+1 {
		t.Fatalf("the failure did not retry the claim: %+v", actual.Status.ActiveOperation)
	}
}

// TestAFailedFetchIsNamedAsItsStep covers the schema an OCI artifact delivers,
// which `schema pull` reads in an init container: a file that does not parse
// ends the Pod there, before the runner can speak. The step and its exit status
// are structured fields, and what the container wrote stays in its log. A Job
// that failed with no init container to blame keeps the sentence it had.
func TestAFailedFetchIsNamedAsItsStep(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		statuses []corev1.ContainerStatus
		want     string
	}{
		{
			name: "the fetch failed",
			statuses: []corev1.ContainerStatus{
				{Name: "authorize-oci-source", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: "fetch-schema", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 2, Message: "error: parse schema artifact: FRAME-TEXT",
				}}},
			},
			want: "the fetch-schema step exited 2, so the run never started",
		},
		{
			name: "every init container succeeded",
			statuses: []corev1.ContainerStatus{
				{Name: "fetch-schema", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			},
			want: "read Observe result: ",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := readOnlySchemaClaim(t, operatorv1alpha1.OperationObserve)
			job, pod := terminalWorkload(schema, batchv1.JobFailed)
			pod.Status.InitContainerStatuses = test.statuses
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, job, pod)
			reconciler.Locks = targetlock.New(api, api, nil)
			recorder := record.NewFakeRecorder(32)
			reconciler.Recorder = recorder

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := safetyGetSchema(t, api, schema)
			failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
			if failed == nil || failed.Status != metav1.ConditionTrue || !strings.HasPrefix(failed.Message, test.want) {
				t.Fatalf("ReconciliationFailed = %#v, want it to start with %q", failed, test.want)
			}
			assertNoFrameText(t, recorder, actual.Status)
		})
	}
}

// TestBoundedKeepsWholeCharacters cuts in the middle of a two-byte character. An
// Event or a condition holding half of one is text the API server may refuse.
func TestBoundedKeepsWholeCharacters(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name, value string
		limit       int
		want        string
	}{
		{name: "under the bound", value: "abc", limit: 4, want: "abc"},
		{name: "at the bound", value: "abcd", limit: 4, want: "abcd"},
		{name: "over the bound", value: "abcde", limit: 4, want: "abcd..."},
		{name: "a character across the bound", value: "abcé", limit: 4, want: "abc..."},
		{name: "a character at the bound", value: "abcdé", limit: 4, want: "abcd..."},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := bounded(test.value, test.limit); got != test.want {
				t.Fatalf("bounded(%q, %d) = %q, want %q", test.value, test.limit, got, test.want)
			}
		})
	}
}
