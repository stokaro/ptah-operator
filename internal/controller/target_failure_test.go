package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	"github.com/stokaro/ptah-operator/internal/targetfailure"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// frameText stands for what a runner frame's message could carry: text the
// child or the database wrote, had it reached the frame. For a target
// failure the controller writes its own sentence and reads none of it.
const frameText = "FRAME-TEXT failed to connect to `user=app_owner database=orders`: db.internal.example"

// TestResultFailureWritesTheKindsSentence holds a frame whose code is a kind
// to the operator's sentence for it, whatever the frame's message says.
func TestResultFailureWritesTheKindsSentence(t *testing.T) {
	t.Parallel()

	url := corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "demo-database-broken"}, Key: "url"}
	for _, kind := range dataplane.TargetFailureKinds() {
		want := targetfailure.Message(kind, url)
		if failure := resultFailure(&runner.ResultError{Code: string(kind), Message: frameText}, url); failure.Error() != want {
			t.Errorf("resultFailure(%s) = %q, want %q", kind, failure.Error(), want)
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
			want:    targetfailure.Message(dataplane.TargetDatabaseNotFound, url),
		},
		{
			name:    "an exit status",
			failure: runner.ResultError{Code: "child_exit", Message: "ptah exited with code 2"},
			want:    "child_exit: ptah exited with code 2",
		},
		{
			name:    "a desired file that does not load",
			failure: runner.ResultError{Code: runner.CodeDesiredSourceUnreadable, Message: "read SQL schema file schema.sql: expected '('"},
			want:    "desired_source_unreadable: read SQL schema file schema.sql: expected '('",
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
	want := targetfailure.Message(dataplane.TargetDatabaseUnreachable, schema.Spec.Target.URLFrom)
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

	for _, test := range []struct {
		name string
		url  *corev1.SecretKeySelector
	}{
		{name: "the fixture's Secret"},
		{
			// Status keeps the sentence whole for the longest name and key
			// Kubernetes admits; the Event keeps 512 bytes of it.
			name: "the longest Secret name and key",
			url: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: strings.Repeat("n", 253)},
				Key:                  strings.Repeat("k", 253),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			if test.url != nil {
				migration.Spec.Target.URLFrom = *test.url
			}
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
			want := targetfailure.Message(dataplane.TargetPermissionDenied, migration.Spec.Target.URLFrom)
			progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
			if progressing == nil || progressing.Reason != string(operatorv1alpha1.ReasonOperationFailed) || progressing.Message != want {
				t.Fatalf("Progressing = %#v, want OperationFailed with %q", progressing, want)
			}
			if event := findEvent(recorder, "Warning OperationRetried"); !strings.HasSuffix(event, ": "+bounded(want, 512)) {
				t.Fatalf("OperationRetried event = %q, want it to end with the sentence", event)
			}
			assertNoFrameText(t, recorder, actual.Status)
		})
	}
}

// notStartedApply is a migration whose Apply claim's Pod reported a run that
// stopped before it selected anything, with the error the frame carried, and
// the reconciler that harvests it at the fixture's fixed instant.
func notStartedApply(
	t *testing.T,
	policy operatorv1alpha1.ApplyPolicy,
	failure *runner.ResultError,
) (*operatorv1alpha1.PtahMigration, *MigrationReconciler, client.WithWatch, *record.FakeRecorder) {
	t.Helper()
	migration, plan := awaitingApprovalFixture(t)
	if policy == operatorv1alpha1.ApplyPolicyAlways {
		migration.Spec.Policy.Apply = policy
		repolicyPlan(t, migration, plan)
	}
	operation := applyClaimFor(t, migration, plan)
	if policy != operatorv1alpha1.ApplyPolicyAlways {
		operation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: "approval", UID: "approval-uid"}
	}
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
		Error: failure,
	})
	reconciler, api := fakeMigrationReconciler(
		t, staticLogs{content: frame}, migration, plan, job, pod, verificationPolicyConfigMap(),
	)
	holdMigrationApplyLease(t, reconciler, api, migration)
	recorder := record.NewFakeRecorder(32)
	reconciler.Recorder = recorder
	return migration, reconciler, api, recorder
}

// TestAMigrationRunThatNeverSelectedSaysWhy records the run of an Apply that
// stopped before it selected anything. With a kind the run's message is the
// kind's sentence; without one it says the run ran nothing, rather than
// naming a migration that never started or a selection that moved. The
// resource waits the failure interval, with Ready and Progressing saying why,
// and the Event carries the whole message.
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
				return targetfailure.Message(dataplane.TargetAuthenticationFailed, migration.Spec.Target.URLFrom)
			},
		},
		{
			name:    "a target that did not answer",
			failure: &runner.ResultError{Code: string(dataplane.TargetDatabaseUnreachable), Message: frameText},
			want: func(migration *operatorv1alpha1.PtahMigration) string {
				return targetfailure.Message(dataplane.TargetDatabaseUnreachable, migration.Spec.Target.URLFrom)
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
			migration, reconciler, api, recorder := notStartedApply(t, operatorv1alpha1.ApplyPolicyOnApproval, test.failure)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			want := test.want(migration)
			// A failure with a kind has its sentence whole. Any other says where
			// Ptah's own account is.
			if run := actual.Status.LastRun; run != nil && test.failure.Code == "child_exit" {
				want += ". What Ptah reported is in the Pod log for 24 hours after the Job finishes: kubectl logs -n " +
					migration.Namespace + " job/" + run.JobName + " -c ptah"
			}
			if run := actual.Status.LastRun; run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeFailed ||
				run.Message != want || len(run.AppliedVersions) != 0 {
				t.Fatalf("LastRun = %#v, want a failed run that says %q", run, want)
			}
			due := reconciler.now().Add(migrationFailureRetry(migration))
			if actual.Status.UnresolvedRun != nil || actual.Status.Phase != operatorv1alpha1.MigrationPhaseFailed ||
				actual.Status.NextReconciliationTime == nil || !actual.Status.NextReconciliationTime.Time.Equal(due) {
				t.Fatalf("phase %q next %v unresolved %#v, want Failed until %v",
					actual.Status.Phase, actual.Status.NextReconciliationTime, actual.Status.UnresolvedRun, due)
			}
			for _, conditionType := range []string{
				operatorv1alpha1.ConditionMigrationReady, operatorv1alpha1.ConditionMigrationProgressing,
			} {
				condition := meta.FindStatusCondition(actual.Status.Conditions, conditionType)
				if condition == nil || condition.Status != metav1.ConditionFalse ||
					condition.Reason != string(operatorv1alpha1.ReasonOperationFailed) || condition.Message != want {
					t.Fatalf("%s = %#v, want False OperationFailed with %q", conditionType, condition, want)
				}
			}
			if event := findEvent(recorder, "Warning MigrationRunFinished"); event != "Warning MigrationRunFinished Failed: "+want {
				t.Fatalf("MigrationRunFinished event = %q, want the whole message", event)
			}
			assertNoFrameText(t, recorder, actual.Status)
		})
	}
}

// TestARunThatNeverSelectedClaimsNoApplyBeforeTheInterval follows a run that
// stopped before it selected anything under the Always policy, at the instant
// it ended: whatever the resource does next, it claims no Apply until the
// failure interval has passed, and then starts again from resolution.
//
// Ptah refuses some runs before selecting for a reason the history read does
// not share, so a history read straight after would publish the same plan and
// Always would claim the same refused Apply at once, once per pair of Job
// lifetimes, with a Warning and a failure counted each time.
func TestARunThatNeverSelectedClaimsNoApplyBeforeTheInterval(t *testing.T) {
	t.Parallel()

	migration, reconciler, api, _ := notStartedApply(t, operatorv1alpha1.ApplyPolicyAlways,
		&runner.ResultError{Code: "child_exit", Message: "ptah exited with code 2"})
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	current := readMigration(t, api, migration)
	ended := reconciler.now()

	// Without pacing the next pass reads the history back: let it, and carry
	// the plan that read publishes, so the pass after it is the one that
	// would claim the Apply.
	objects := []client.Object{verificationPolicyConfigMap()}
	if current.Status.Phase == operatorv1alpha1.MigrationPhaseVerifyingHistory {
		read, readAPI := readMigrationHistory(t, current, pendingMigrationHistory())
		plans := &operatorv1alpha1.PtahMigrationPlanList{}
		if err := readAPI.List(context.Background(), plans); err != nil {
			t.Fatal(err)
		}
		for i := range plans.Items {
			objects = append(objects, plans.Items[i].DeepCopy())
		}
		current = read
	}
	objects = append(objects, current.DeepCopy())
	for _, object := range objects {
		object.SetResourceVersion("")
	}
	next, nextAPI := fakeMigrationReconciler(t, staticLogs{}, objects...)
	next.Clock = func() time.Time { return ended }
	if _, err := next.Reconcile(context.Background(), migrationRequest(current)); err != nil {
		t.Fatalf("Reconcile() at the instant the run ended: %v", err)
	}
	waiting := readMigration(t, nextAPI, current)
	if active := waiting.Status.ActiveOperation; active != nil {
		t.Fatalf("the pass at the instant the run ended claimed %s attempt %d, want nothing before %v",
			active.Type, active.Attempt, waiting.Status.NextReconciliationTime)
	}

	due := ended.Add(migrationFailureRetry(migration))
	next.Clock = func() time.Time { return due }
	if _, err := next.Reconcile(context.Background(), migrationRequest(waiting)); err != nil {
		t.Fatalf("Reconcile() when the interval passed: %v", err)
	}
	resumed := readMigration(t, nextAPI, waiting)
	if active := resumed.Status.ActiveOperation; active == nil || active.Type != operatorv1alpha1.MigrationOperationResolve {
		t.Fatalf("the pass when the interval passed claimed %#v, want a Resolve", active)
	}
}

// TestAFrameWithAKindAndNoRunIsNotRepeated holds the one frame the runner does
// not write -- a target failure with no run report -- to the operator's
// sentence, on the path that records an Apply nobody accounted for.
func TestAFrameWithAKindAndNoRunIsNotRepeated(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	reported := (&MigrationReconciler{}).frameMigrationApply(context.Background(), migration, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
		OperationID: operation.ID, ChildExitCode: 2, MutationStarted: true, Uncertain: true,
		Error: &runner.ResultError{Code: string(dataplane.TargetTLSFailed), Message: frameText},
	})
	want := targetfailure.Message(dataplane.TargetTLSFailed, migration.Spec.Target.URLFrom)
	if reported.run != nil || reported.failure == nil || reported.failure.Error() != want {
		t.Fatalf("frameMigrationApply() = %+v, want no run and the failure %q", reported, want)
	}
}

// TestAMigrationNamesTheSecretItsClaimBound reads the Secret from the active
// claim, which a spec edited after dispatch does not change.
func TestAMigrationNamesTheSecretItsClaimBound(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Spec.Target.URLFrom = corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "edited"}, Key: "dsn"}
	if got := migrationURLSecret(migration); got.Name != "edited" || got.Key != "dsn" {
		t.Fatalf("migrationURLSecret() with no claim = %+v, want the spec's", got)
	}
	migration.Status.ActiveOperation = &operatorv1alpha1.MigrationOperationStatus{
		Target: &operatorv1alpha1.DatabaseTargetBinding{URLFrom: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "dispatched"}, Key: "url"}},
	}
	if got := migrationURLSecret(migration); got.Name != "dispatched" || got.Key != "url" {
		t.Fatalf("migrationURLSecret() = %+v, want the claim's", got)
	}
}

// TestATargetFailureSentenceFitsStatusWhole holds every kind's sentence, for
// the longest Secret name and key Kubernetes admits, under the 1024 bytes the
// conditions and a run's message keep, and, for a name and key of a hundred
// bytes each, under the 512 every failure Event keeps.
func TestATargetFailureSentenceFitsStatusWhole(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		length int
		limit  int
	}{
		{"the longest name and key", 253, 1024},
		{"a name and key of a hundred bytes", 100, migrationRunEventBytes},
	} {
		url := corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: strings.Repeat("n", test.length)},
			Key:                  strings.Repeat("k", test.length),
		}
		for _, kind := range dataplane.TargetFailureKinds() {
			if message := targetfailure.Message(kind, url); len(message) > test.limit {
				t.Errorf("%s: %s is %d bytes, more than %d", test.name, kind, len(message), test.limit)
			}
		}
	}
}
