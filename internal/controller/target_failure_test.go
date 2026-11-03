package controller

import (
	"context"
	"encoding/json"
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
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// frameText stands for what a runner frame's message could carry: text the
// child or the database wrote, had it reached the frame. For a target
// failure the controller writes its own sentence and reads none of it.
const frameText = "FRAME-TEXT failed to connect to `user=app_owner database=orders`: db.internal.example"

// TestEachTargetFailureKindHasItsSentence holds each kind to the sentence a
// condition, an Event and kubectl ptah show for it: the kind, what it means,
// and the Secret key the operation read its URL from.
func TestEachTargetFailureKindHasItsSentence(t *testing.T) {
	t.Parallel()

	url := corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "demo-database-broken"}, Key: "url"}
	want := map[dataplane.TargetFailureKind]string{
		dataplane.TargetDatabaseUnreachable: "database_unreachable: the target database did not answer: its host did not " +
			"resolve, refused the connection, or did not respond in time. Check the host and port in the URL that " +
			`Secret "demo-database-broken" holds under key "url", and that the database is running and accepts ` +
			"connections from this namespace",
		dataplane.TargetAuthenticationFailed: "authentication_failed: the target database refused the credentials. " +
			`Check the user and password in the URL that Secret "demo-database-broken" holds under key "url"`,
		dataplane.TargetDatabaseNotFound: "database_not_found: the target database server has no database by the name " +
			`the URL gives. Check the database name in the URL that Secret "demo-database-broken" holds under key "url", ` +
			"or create the database",
		dataplane.TargetPermissionDenied: "permission_denied: the target database server accepted the credentials and " +
			`refused the user access to the database. Check the user in the URL that Secret "demo-database-broken" holds ` +
			`under key "url", or grant that user access to the database`,
		dataplane.TargetTLSFailed: "tls_failed: the TLS handshake with the target database failed. Check the TLS settings " +
			`in the URL that Secret "demo-database-broken" holds under key "url" against the certificate the server presents`,
	}
	if len(want) != len(dataplane.TargetFailureKinds()) {
		t.Fatalf("the table names %d kinds, the vocabulary %d", len(want), len(dataplane.TargetFailureKinds()))
	}
	for _, kind := range dataplane.TargetFailureKinds() {
		message := targetFailureMessage(kind, url)
		if message != want[kind] {
			t.Errorf("targetFailureMessage(%s) =\n%q\nwant\n%q", kind, message, want[kind])
		}
		// An Event keeps 512 bytes of it, and a condition 1024.
		if len(message) > 512 {
			t.Errorf("targetFailureMessage(%s) is %d bytes, more than an Event keeps", kind, len(message))
		}
		failure := resultFailure(&runner.ResultError{Code: string(kind), Message: frameText}, url)
		if failure.Error() != want[kind] {
			t.Errorf("resultFailure(%s) = %q, want the kind's sentence", kind, failure.Error())
		}
	}
}

// TestResultFailureReadsOnlyTheKindOfATargetFailure holds the one place a
// frame's error becomes a status message: a kind is written in the operator's
// words whatever the frame's message says, and every other code keeps the
// runner's bounded message as before.
func TestResultFailureReadsOnlyTheKindOfATargetFailure(t *testing.T) {
	t.Parallel()

	url := corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url"}
	for _, test := range []struct {
		name    string
		failure runner.ResultError
		want    string
	}{
		{
			name:    "a kind",
			failure: runner.ResultError{Code: "database_not_found", Message: frameText},
			want:    targetFailureMessage(dataplane.TargetDatabaseNotFound, url),
		},
		{
			name:    "an exit status",
			failure: runner.ResultError{Code: "child_exit", Message: "ptah exited with code 2"},
			want:    "child_exit: ptah exited with code 2",
		},
		{
			name:    "a code that only resembles a kind",
			failure: runner.ResultError{Code: "Database_Unreachable", Message: "ptah exited with code 2"},
			want:    "Database_Unreachable: ptah exited with code 2",
		},
		{
			name:    "a long message",
			failure: runner.ResultError{Code: "invalid_input", Message: strings.Repeat("x", 600)},
			want:    "invalid_input: " + strings.Repeat("x", 512) + "...",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := resultFailure(&test.failure, url).Error(); got != test.want {
				t.Fatalf("resultFailure() = %q, want %q", got, test.want)
			}
		})
	}
}

// TestTheSecretIsTheOneTheOperationRead names the Secret the claim bound when
// it dispatched, which a later spec edit does not change, and the resource's
// own only when a claim bound none.
func TestTheSecretIsTheOneTheOperationRead(t *testing.T) {
	t.Parallel()

	spec := corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "edited"}, Key: "dsn"}
	bound := &operatorv1alpha1.DatabaseTargetBinding{URLFrom: corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "dispatched"}, Key: "url"}}
	if got := operationURLSecret(bound, spec); got.Name != "dispatched" || got.Key != "url" {
		t.Fatalf("operationURLSecret(bound) = %+v, want the binding's", got)
	}
	for name, binding := range map[string]*operatorv1alpha1.DatabaseTargetBinding{
		"no binding":            nil,
		"a binding with no URL": {Engine: operatorv1alpha1.DatabaseEnginePostgreSQL},
	} {
		if got := operationURLSecret(binding, spec); got.Name != "edited" || got.Key != "dsn" {
			t.Fatalf("operationURLSecret(%s) = %+v, want the spec's", name, got)
		}
	}
}

// assertNoFrameText fails when anything the controller wrote repeats the
// frame's message.
func assertNoFrameText(t *testing.T, recorder *record.FakeRecorder, status any) {
	t.Helper()
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "FRAME-TEXT") || strings.Contains(string(encoded), "db.internal.example") {
		t.Fatalf("status repeats the frame's message: %s", encoded)
	}
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "FRAME-TEXT") || strings.Contains(event, "db.internal.example") {
				t.Fatalf("an Event repeats the frame's message: %s", event)
			}
		default:
			return
		}
	}
}

// TestASchemaNamesWhyItsTargetCouldNotBeOpened harvests a Plan whose runner
// could not open the target, and reads the sentence where a person reads it:
// the ReconciliationFailed condition and the Warning Event.
func TestASchemaNamesWhyItsTargetCouldNotBeOpened(t *testing.T) {
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
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationPlan, OperationID: "plan-operation",
		ChildExitCode:      2,
		CoordinationDigest: schema.Status.ActiveOperation.CoordinationDigest,
		Error:              &runner.ResultError{Code: string(dataplane.TargetDatabaseUnreachable), Message: frameText},
	})
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)
	recorder := record.NewFakeRecorder(32)
	reconciler.Recorder = recorder

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := safetyGetSchema(t, api, schema)
	want := targetFailureMessage(dataplane.TargetDatabaseUnreachable, schema.Spec.Target.URLFrom)
	failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if failed == nil || failed.Status != metav1.ConditionTrue || failed.Reason != string(operatorv1alpha1.ReasonOperationFailed) ||
		failed.Message != want {
		t.Fatalf("ReconciliationFailed = %#v, want OperationFailed with %q", failed, want)
	}
	if !strings.Contains(want, `Secret "database" holds under key "url"`) {
		t.Fatalf("the sentence %q does not name the fixture's Secret", want)
	}
	if event := findEvent(recorder, "Warning OperationFailed"); event != "Warning OperationFailed "+want {
		t.Fatalf("OperationFailed event = %q, want the sentence", event)
	}
	assertNoFrameText(t, recorder, actual.Status)
}

// TestAMigrationHistoryNamesWhyItsTargetCouldNotBeOpened is the same reading
// on a migration's History run, where the retry names the failure on
// Progressing and in the OperationRetried Event.
func TestAMigrationHistoryNamesWhyItsTargetCouldNotBeOpened(t *testing.T) {
	t.Parallel()

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
		Error: &runner.ResultError{Code: string(dataplane.TargetPermissionDenied), Message: frameText},
	})
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, job, pod, verificationPolicyConfigMap())
	recorder := record.NewFakeRecorder(32)
	reconciler.Recorder = recorder

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	want := targetFailureMessage(dataplane.TargetPermissionDenied, migration.Spec.Target.URLFrom)
	progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Reason != string(operatorv1alpha1.ReasonOperationFailed) || progressing.Message != want {
		t.Fatalf("Progressing = %#v, want OperationFailed with %q", progressing, want)
	}
	if event := findEvent(recorder, "Warning OperationRetried"); !strings.HasSuffix(event, ": "+want) {
		t.Fatalf("OperationRetried event = %q, want it to end with the sentence", event)
	}
	assertNoFrameText(t, recorder, actual.Status)
}

// TestAMigrationRunThatNeverSelectedSaysWhy records the run of an Apply that
// stopped before it selected anything. With a kind the run's message is the
// kind's sentence; without one it says the run ran nothing, rather than
// naming a migration that never started or a selection that moved.
func TestAMigrationRunThatNeverSelectedSaysWhy(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		failure *runner.ResultError
		want    func(migration *operatorv1alpha1.PtahMigration) string
	}{
		{
			name:    "a target that refused the credentials",
			failure: &runner.ResultError{Code: string(dataplane.TargetAuthenticationFailed), Message: frameText},
			want: func(migration *operatorv1alpha1.PtahMigration) string {
				return targetFailureMessage(dataplane.TargetAuthenticationFailed, migration.Spec.Target.URLFrom)
			},
		},
		{
			name:    "a failure with no kind",
			failure: &runner.ResultError{Code: "child_exit", Message: "ptah exited with code 2"},
			want: func(*operatorv1alpha1.PtahMigration) string {
				return "The run stopped before it selected any migration, and ran none"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, plan := awaitingApprovalFixture(t)
			operation := applyClaimFor(t, migration, plan)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			frame := migrationFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
				OperationID: operation.ID, ChildExitCode: 2, MutationStarted: true,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
				MigrationRun: &dataplane.MigrationRunReport{
					ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up",
					Outcome: dataplane.MigrationOutcomeFailed,
				},
				Error: test.failure,
			})
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{content: frame}, migration, plan, job, pod, verificationPolicyConfigMap(),
			)
			holdMigrationApplyLease(t, reconciler, api, migration)
			recorder := record.NewFakeRecorder(32)
			reconciler.Recorder = recorder

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			want := test.want(migration)
			if run := actual.Status.LastRun; run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeFailed ||
				run.Message != want || len(run.AppliedVersions) != 0 {
				t.Fatalf("LastRun = %#v, want a failed run that says %q", run, want)
			}
			if actual.Status.UnresolvedRun != nil || actual.Status.Phase != operatorv1alpha1.MigrationPhaseVerifyingHistory {
				t.Fatalf("phase %q unresolved %#v, want the history read that follows a failed run",
					actual.Status.Phase, actual.Status.UnresolvedRun)
			}
			if event := findEvent(recorder, "Warning MigrationRunFinished"); !strings.HasSuffix(event, bounded(want, 256)) {
				t.Fatalf("MigrationRunFinished event = %q, want it to end with the run's message", event)
			}
			assertNoFrameText(t, recorder, actual.Status)
		})
	}
}
