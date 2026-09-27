package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// foreignRunnerFrame is what a runner of another protocol writes. The frame
// envelope is shared by every protocol; the payload is that runner's, which
// is why this does not go through runner.MarshalFrame.
func foreignRunnerFrame(t *testing.T, document map[string]any) []byte {
	t.Helper()
	payload, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return []byte(fmt.Sprintf("PTAH_RUNNER_RESULT_V1 %d %s\n%s\nPTAH_RUNNER_RESULT_END_V1\n",
		len(payload), hex.EncodeToString(digest[:]), payload))
}

// foreignProtocolRefusal is the refusal document from a runner that speaks
// the next protocol.
func foreignProtocolRefusal(t *testing.T, operation runner.Operation, operationID string) []byte {
	t.Helper()
	return foreignRunnerFrame(t, map[string]any{
		"protocolVersion": runner.ProtocolVersion + 1, "operation": operation, "operationId": operationID,
		"childExitCode": -1, "stdout": "",
		"error": map[string]any{
			"code":    runner.CodeRunnerProtocolMismatch,
			"message": fmt.Sprintf("the Job expects runner protocol %d; this runner speaks protocol %d", runner.ProtocolVersion, runner.ProtocolVersion+1),
		},
	})
}

// TestASchemaNamesARunnerOfAnotherProtocol harvests a read-only Plan whose
// runner refused the Job. Whether that runner wrote the refusal in its own
// protocol or in this one, the failure is named for the runner image the
// installation chose and not for the operation, the attempt waits out the
// failure interval rather than looping, and an Event says so. A foreign frame
// that is not the refusal document, and a refusal for another reason, read
// as the failures they are.
func TestASchemaNamesARunnerOfAnotherProtocol(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name       string
		frame      func(t *testing.T, operationID string) []byte
		wantReason operatorv1alpha1.ConditionReason
		wantEvent  bool
	}{
		{
			name: "a runner of another protocol refused",
			frame: func(t *testing.T, operationID string) []byte {
				return foreignProtocolRefusal(t, runner.OperationPlan, operationID)
			},
			wantReason: operatorv1alpha1.ReasonRunnerProtocolMismatch,
			wantEvent:  true,
		},
		{
			name: "a runner of this protocol refused a Job built for another",
			frame: func(t *testing.T, operationID string) []byte {
				return safetyRunnerFrame(t, runner.Result{
					ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationPlan, OperationID: operationID,
					ChildExitCode: -1,
					Error:         &runner.ResultError{Code: runner.CodeRunnerProtocolMismatch, Message: "the Job expects runner protocol 4"},
				})
			},
			wantReason: operatorv1alpha1.ReasonRunnerProtocolMismatch,
			wantEvent:  true,
		},
		{
			name: "a runner of another protocol wrote a result",
			frame: func(t *testing.T, operationID string) []byte {
				return foreignRunnerFrame(t, map[string]any{
					"protocolVersion": runner.ProtocolVersion + 1, "operation": "plan", "operationId": operationID,
					"childExitCode": 0, "stdout": "{}", "planOutcome": "changes",
				})
			},
			wantReason: operatorv1alpha1.ReasonOperationFailed,
		},
		{
			name: "a runner of this protocol refused for another reason",
			frame: func(t *testing.T, operationID string) []byte {
				return safetyRunnerFrame(t, runner.Result{
					ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationPlan, OperationID: operationID,
					ChildExitCode: -1,
					Error:         &runner.ResultError{Code: "invalid_target", Message: "the target is not a database URL"},
				})
			},
			wantReason: operatorv1alpha1.ReasonOperationFailed,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema := safetyApplySchema(t)
			schema.Status.Phase = operatorv1alpha1.PhasePlanning
			schema.Status.Plan = nil
			schema.Status.Source.Verified = true
			schema.Status.Source.Digest = testDigest
			schema.Status.Source.ResolvedReference = "oci://registry.example/team/schema@" + testDigest
			schema.Status.Target.DriftReportDigest = testDigest
			schema.Status.ActiveOperation.Type = operatorv1alpha1.OperationPlan
			schema.Status.ActiveOperation.ID = "plan-operation"
			schema.Status.ActiveOperation.JobName = "plan-job"
			schema.Status.ActiveOperation.LeaseEpoch = testLeaseEpoch
			bindActiveInput(t, schema)
			attempt := schema.Status.ActiveOperation.Attempt
			job, pod := terminalWorkload(schema, batchv1.JobFailed)
			reconciler, api := fakeReconciler(t, staticLogs{content: row.frame(t, "plan-operation")}, schema, job, pod)
			reconciler.Locks = targetlock.New(api, api, nil)
			recorder := record.NewFakeRecorder(32)
			reconciler.Recorder = recorder

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := safetyGetSchema(t, api, schema)
			if !conditionMatches(actual.Status.Conditions, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, row.wantReason) ||
				!conditionMatches(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionTrue, row.wantReason) {
				t.Fatalf("conditions = %#v, want Ready and ReconciliationFailed naming %s", actual.Status.Conditions, row.wantReason)
			}
			operation := actual.Status.ActiveOperation
			if operation == nil || operation.Attempt != attempt+1 || actual.Status.NextReconciliationTime == nil {
				t.Fatalf("the refused Plan was not retried after the failure interval: %#v", actual.Status)
			}
			event := findEvent(recorder, "Warning RunnerProtocolMismatch")
			if (event != "") != row.wantEvent {
				t.Fatalf("RunnerProtocolMismatch event = %q, want one = %t", event, row.wantEvent)
			}
		})
	}
}

// TestASchemaApplyRefusedByARunnerOfAnotherProtocolOwesProof is the same
// refusal on an Apply. The refusal is that Pod's account of itself; a Job can
// run its Pod more than once, so the Apply still owes the read-only proof
// every Apply error owes, and the Event names the cause.
func TestASchemaApplyRefusedByARunnerOfAnotherProtocolOwesProof(t *testing.T) {
	t.Parallel()

	schema, plan, policyConfig := dispatchedApplyWithPlan(t)
	job, pod := terminalWorkload(schema, batchv1.JobFailed)
	frame := foreignProtocolRefusal(t, runner.OperationApply, schema.Status.ActiveOperation.ID)
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, plan, policyConfig, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)
	recorder := record.NewFakeRecorder(32)
	reconciler.Recorder = recorder

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := safetyGetSchema(t, api, schema)
	pending := actual.Status.PendingObservation
	if pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
		t.Fatalf("pending observation = %#v, want the Apply owing its proof", pending)
	}
	if event := findEvent(recorder, "Warning RunnerProtocolMismatch"); !strings.Contains(event, "protocol") {
		t.Fatalf("RunnerProtocolMismatch event = %q", event)
	}
}

// TestAMigrationNamesARunnerOfAnotherProtocol is the migration family's
// read-only half: a History run refused by a runner of another protocol is
// retried under the reason that names it.
func TestAMigrationNamesARunnerOfAnotherProtocol(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name       string
		refusal    bool
		wantReason operatorv1alpha1.ConditionReason
	}{
		{name: "a runner of another protocol refused", refusal: true, wantReason: operatorv1alpha1.ReasonRunnerProtocolMismatch},
		{name: "a runner of another protocol wrote something else", wantReason: operatorv1alpha1.ReasonOperationFailed},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration := migrationFixture()
			migration.Status.ExecutionBinding = migrationExecutionBinding()
			migration.Status.Artifact = resolvedMigrationArtifact()
			migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
			migration.Finalizers = []string{migrationOperationFinalizer}
			operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobFailed)
			frame := foreignRunnerFrame(t, map[string]any{
				"protocolVersion": runner.ProtocolVersion + 1, "operation": runner.OperationMigrationHistory,
				"operationId": operation.ID, "childExitCode": 0, "stdout": "",
			})
			if row.refusal {
				frame = foreignProtocolRefusal(t, runner.OperationMigrationHistory, operation.ID)
			}
			reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame},
				migration, job, pod, verificationPolicyConfigMap())
			recorder := record.NewFakeRecorder(32)
			reconciler.Recorder = recorder

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			progressing := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
			if progressing == nil || progressing.Reason != string(row.wantReason) {
				t.Fatalf("Progressing = %#v, want reason %s", progressing, row.wantReason)
			}
			if next := actual.Status.ActiveOperation; next == nil || next.Attempt != operation.Attempt+1 || next.RetryNotBefore == nil {
				t.Fatalf("the History run was not retried after the failure interval: %#v", next)
			}
			if event := findEvent(recorder, "Warning RunnerProtocolMismatch"); (event != "") != row.refusal {
				t.Fatalf("RunnerProtocolMismatch event = %q, want one = %t", event, row.refusal)
			}
		})
	}
}
