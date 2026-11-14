package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// testShadowDigest is the identity a history read reported for the shadow.
var testShadowDigest = "sha256:" + strings.Repeat("5", 64)

func withDriftCheck(migration *operatorv1alpha1.PtahMigration) {
	migration.Spec.Policy.DriftCheck = driftCheckPolicy()
	migration.Status.History.ShadowIdentityDigest = testShadowDigest
}

func driftCheckPolicy() *operatorv1alpha1.MigrationDriftCheck {
	return &operatorv1alpha1.MigrationDriftCheck{Shadow: operatorv1alpha1.DatabaseTargetRef{
		URLFrom: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "orders-shadow"}, Key: "url"},
	}}
}

// driftRefusedApply is an Apply under a drift check whose runner reported the
// check stopping the run: failed, the plan's selection planned, nothing
// applied, the history read after, and the summary the runner framed.
func driftRefusedApply(
	t *testing.T,
	policy operatorv1alpha1.ApplyPolicy,
	summary dataplane.MigrationDriftSummary,
) (*operatorv1alpha1.PtahMigration, *MigrationReconciler, client.WithWatch, *record.FakeRecorder) {
	t.Helper()
	migration, plan := awaitingApprovalFixture(t)
	withDriftCheck(migration)
	migration.Spec.Policy.Apply = policy
	repolicyPlan(t, migration, plan)
	if plan.Spec.ShadowIdentityDigest != testShadowDigest {
		t.Fatalf("the plan binds shadow %q, want %q", plan.Spec.ShadowIdentityDigest, testShadowDigest)
	}
	operation := applyClaimFor(t, migration, plan)
	if policy != operatorv1alpha1.ApplyPolicyAlways {
		operation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: "approval", UID: "approval-uid"}
	}
	history := pendingMigrationHistory()
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
		OperationID: operation.ID, ChildExitCode: 1, MutationStarted: true,
		CoordinationDigest:   operation.CoordinationDigest,
		TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
		ShadowIdentityDigest: testShadowDigest,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up",
			Outcome: dataplane.MigrationOutcomeFailed, Planned: []int64{3}, Status: &history,
			DriftSummary: &summary,
		},
		Error: &runner.ResultError{Code: "child_exit", Message: frameText},
	})
	reconciler, api := fakeMigrationReconciler(
		t, staticLogs{content: frame}, migration, plan, job, pod, verificationPolicyConfigMap(),
	)
	holdMigrationApplyLease(t, reconciler, api, migration)
	recorder := record.NewFakeRecorder(32)
	reconciler.Recorder = recorder
	return migration, reconciler, api, recorder
}

// A run the drift check stopped is recorded as the failed run it is, with a
// reason for each way the check can refuse and a message made of counts and
// codes: Ptah's words and the frame's text reach neither status nor Events.
// The resource waits the failure interval rather than reading the history
// back, because the history is the one the plan read.
func TestARunTheDriftCheckStoppedSaysWhichRefusal(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		summary dataplane.MigrationDriftSummary
		reason  operatorv1alpha1.ConditionReason
		says    string
	}{
		{name: "drifted", summary: dataplane.MigrationDriftSummary{Outcome: "drifted", Findings: 2},
			reason: operatorv1alpha1.ReasonSchemaDrifted, says: "found 2 differences"},
		{name: "one difference", summary: dataplane.MigrationDriftSummary{Outcome: "drifted", Findings: 1},
			reason: operatorv1alpha1.ReasonSchemaDrifted, says: "found 1 difference "},
		{name: "a history that does not replay", summary: dataplane.MigrationDriftSummary{Outcome: "unable", Stage: "history", Reason: "out_of_order"},
			reason: operatorv1alpha1.ReasonDriftCheckUnsupported, says: "(stage history, reason out_of_order)"},
		{name: "a comparison that cannot describe an object", summary: dataplane.MigrationDriftSummary{Outcome: "unable", Stage: "schema-match", Reason: "incomplete_coverage", Findings: 1},
			reason: operatorv1alpha1.ReasonDriftCheckUnsupported, says: "(stage schema-match, reason incomplete_coverage)"},
		{name: "a shadow that holds something", summary: dataplane.MigrationDriftSummary{Outcome: "unable", Stage: "claim", Reason: "not_clean"},
			reason: operatorv1alpha1.ReasonShadowDatabaseUnavailable, says: "(stage claim, reason not_clean)"},
		{name: "a shadow that is the target", summary: dataplane.MigrationDriftSummary{Outcome: "unable", Stage: "realm-check", Reason: "target_shadow_same_realm"},
			reason: operatorv1alpha1.ReasonShadowDatabaseUnavailable, says: "(stage realm-check, reason target_shadow_same_realm)"},
		{name: "a stage this build does not know", summary: dataplane.MigrationDriftSummary{Outcome: "unable", Stage: "a-later-stage", Reason: "a_later_reason"},
			reason: operatorv1alpha1.ReasonShadowDatabaseUnavailable, says: "(stage a-later-stage, reason a_later_reason)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			migration, reconciler, api, recorder := driftRefusedApply(t, operatorv1alpha1.ApplyPolicyOnApproval, row.summary)
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.Phase != operatorv1alpha1.MigrationPhaseFailed {
				t.Fatalf("phase = %s, want Failed", actual.Status.Phase)
			}
			if run := actual.Status.LastRun; run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeFailed ||
				len(run.AppliedVersions) != 0 || !strings.Contains(run.Message, row.says) {
				t.Fatalf("lastRun = %#v, want a failed run that applied nothing and says %q", run, row.says)
			}
			for _, kind := range []string{operatorv1alpha1.ConditionMigrationReady, operatorv1alpha1.ConditionMigrationProgressing} {
				condition := meta.FindStatusCondition(actual.Status.Conditions, kind)
				if condition == nil || condition.Status != "False" || condition.Reason != string(row.reason) ||
					!strings.Contains(condition.Message, row.says) {
					t.Fatalf("%s = %#v, want False with reason %s saying %q", kind, condition, row.reason, row.says)
				}
			}
			due := reconciler.now().Add(migrationFailureRetry(migration))
			if next := actual.Status.NextReconciliationTime; next == nil || !next.Time.Equal(due) {
				t.Fatalf("nextReconciliationTime = %v, want the failure interval %v", next, due)
			}
			if actual.Status.UnresolvedRun != nil {
				t.Fatal("a run that ran nothing was recorded unresolved")
			}
			assertNoFrameText(t, recorder, actual.Status)
		})
	}
}

// Under the Always policy a refusal is not replayed at once: the pass at the
// instant the run ended claims nothing, and the pass after the failure
// interval starts again from resolution, which reads the history and runs the
// check again before anything applies.
func TestADriftRefusalUnderAlwaysClaimsNothingBeforeTheInterval(t *testing.T) {
	t.Parallel()
	migration, reconciler, api, _ := driftRefusedApply(t, operatorv1alpha1.ApplyPolicyAlways,
		dataplane.MigrationDriftSummary{Outcome: "drifted", Findings: 1})
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	current := readMigration(t, api, migration)
	ended := reconciler.now()
	current.SetResourceVersion("")
	policy := verificationPolicyConfigMap()
	next, nextAPI := fakeMigrationReconciler(t, staticLogs{}, current.DeepCopy(), policy)
	next.Clock = func() time.Time { return ended }
	if _, err := next.Reconcile(context.Background(), migrationRequest(current)); err != nil {
		t.Fatalf("Reconcile() at the instant the run ended: %v", err)
	}
	waiting := readMigration(t, nextAPI, current)
	if active := waiting.Status.ActiveOperation; active != nil {
		t.Fatalf("the pass at the instant the run ended claimed %s, want nothing before %v", active.Type, waiting.Status.NextReconciliationTime)
	}
	next.Clock = func() time.Time { return ended.Add(migrationFailureRetry(migration)) }
	if _, err := next.Reconcile(context.Background(), migrationRequest(waiting)); err != nil {
		t.Fatalf("Reconcile() when the interval passed: %v", err)
	}
	if active := readMigration(t, nextAPI, waiting).Status.ActiveOperation; active == nil ||
		active.Type != operatorv1alpha1.MigrationOperationResolve {
		t.Fatalf("the pass when the interval passed claimed %#v, want a Resolve", active)
	}
}

// A plan binds the shadow the history read named, so a read that named
// another one, or none, leaves the plan no longer current, and nothing is
// claimed under it.
func TestAPlanIsCurrentOnlyForTheShadowItBound(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		shadow string
	}{
		{name: "another shadow", shadow: "sha256:" + strings.Repeat("4", 64)},
		{name: "no shadow", shadow: ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			migration, plan := awaitingApprovalFixture(t)
			withDriftCheck(migration)
			repolicyPlan(t, migration, plan)
			reconciler, _ := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
			if _, err := reconciler.currentMigrationPlan(context.Background(), migration); err != nil {
				t.Fatalf("the plan for the shadow it bound is not current: %v", err)
			}
			migration.Status.History.ShadowIdentityDigest = row.shadow
			if _, err := reconciler.currentMigrationPlan(context.Background(), migration); err == nil ||
				!strings.Contains(err.Error(), "shadow") {
				t.Fatalf("currentMigrationPlan() = %v, want the shadow named as what moved", err)
			}
		})
	}
}

// The policy fingerprint of a resource without a check is the one a manager
// before the check computed, so its plans stay current; turning the check on,
// or moving it to another Secret key, is another policy.
func TestTheDriftCheckIsInsideThePolicyFingerprintOnlyWhenSet(t *testing.T) {
	t.Parallel()
	migration := migrationFixture()
	before, err := migrationPolicyFingerprint(migration)
	if err != nil {
		t.Fatal(err)
	}
	recomputed, err := fingerprint.DigestCanonicalJSON(map[string]string{
		"apply":            string(migration.Spec.Policy.Apply),
		"lock_timeout":     migration.Spec.Policy.LockTimeout.Duration.String(),
		"transaction_mode": migration.Spec.Policy.TransactionMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if before != recomputed {
		t.Fatalf("a policy without a check hashes %s, and the map a manager before the check hashed gives %s", before, recomputed)
	}
	migration.Status.History = &operatorv1alpha1.MigrationHistoryStatus{}
	withDriftCheck(migration)
	checked, err := migrationPolicyFingerprint(migration)
	if err != nil {
		t.Fatal(err)
	}
	migration.Spec.Policy.DriftCheck.Shadow.URLFrom.Key = "other"
	moved, err := migrationPolicyFingerprint(migration)
	if err != nil {
		t.Fatal(err)
	}
	migration.Spec.Policy.DriftCheck = driftCheckPolicy()
	migration.Spec.Policy.DriftCheck.Shadow.URLFrom.Name = "other-shadow"
	renamed, err := migrationPolicyFingerprint(migration)
	if err != nil {
		t.Fatal(err)
	}
	if checked == before || moved == checked || renamed == checked {
		t.Fatalf("fingerprints without, with, with another shadow key and with another shadow Secret: %s %s %s %s",
			before, checked, moved, renamed)
	}
}

// The history read and the Apply mount the shadow, so its Secret and key are
// inputs of the claim beside the target's, even at one generation; a resource
// without a check adds nothing to them.
func TestTheShadowIsAnInputOfAClaimOnlyWhenSet(t *testing.T) {
	t.Parallel()
	migration, _ := awaitingApprovalFixture(t)
	reconciler := &MigrationReconciler{Jobs: fakeJobs{}, APIReader: migrationPolicyReader(t)}
	inputs := func(edit func(*operatorv1alpha1.MigrationDriftCheck)) string {
		t.Helper()
		migration.Spec.Policy.DriftCheck = nil
		if edit != nil {
			migration.Spec.Policy.DriftCheck = driftCheckPolicy()
			edit(migration.Spec.Policy.DriftCheck)
		}
		value, err := reconciler.migrationInputFingerprint(
			context.Background(), migration, operatorv1alpha1.MigrationOperationApply)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	unchecked := inputs(nil)
	checked := inputs(func(*operatorv1alpha1.MigrationDriftCheck) {})
	renamed := inputs(func(check *operatorv1alpha1.MigrationDriftCheck) { check.Shadow.URLFrom.Name = "other-shadow" })
	rekeyed := inputs(func(check *operatorv1alpha1.MigrationDriftCheck) { check.Shadow.URLFrom.Key = "other" })
	if again := inputs(nil); again != unchecked {
		t.Fatalf("a resource without a check moved its inputs from %s to %s", unchecked, again)
	}
	for name, value := range map[string]string{"no check": unchecked, "another Secret": renamed, "another key": rekeyed} {
		if value == checked {
			t.Errorf("the claim's inputs with %s are the ones with the check: %s", name, value)
		}
	}
}

// A history read names the shadow exactly when the policy asks for a check.
// The read that names it records the shadow beside the history, and the plan
// published from that history binds it; a read that names none under a check,
// or one under no check, is retried rather than planned from, since a plan from
// it would run with another check than the one asked for.
func TestAHistoryReadNamesTheShadowThePolicyAsksFor(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		checked bool
		shadow  string
		planned bool
	}{
		{name: "a check and its shadow", checked: true, shadow: testShadowDigest, planned: true},
		{name: "a check and no shadow", checked: true},
		{name: "a shadow and no check", shadow: testShadowDigest},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			if row.checked {
				migration.Spec.Policy.DriftCheck = driftCheckPolicy()
			}
			migration.Status.ExecutionBinding = migrationExecutionBinding()
			migration.Status.Artifact = resolvedMigrationArtifact()
			migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
			migration.Finalizers = []string{migrationOperationFinalizer}
			operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			report := pendingMigrationHistory()
			frame := migrationFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
				OperationID: operation.ID, ChildExitCode: 0,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: testDigest,
				ShadowIdentityDigest: row.shadow,
				MigrationHistory:     &report,
			})
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{content: frame}, migration, job, pod, verificationPolicyConfigMap(),
			)
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			plans := &operatorv1alpha1.PtahMigrationPlanList{}
			if err := api.List(context.Background(), plans); err != nil {
				t.Fatal(err)
			}
			if !row.planned {
				if actual.Status.History != nil || len(plans.Items) != 0 {
					t.Fatalf("the read was recorded: history %#v, %d plans", actual.Status.History, len(plans.Items))
				}
				if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Attempt != operation.Attempt+1 {
					t.Fatalf("the read was not retried: %#v", actual.Status.ActiveOperation)
				}
				if !slices.ContainsFunc(actual.Status.Conditions, func(condition metav1.Condition) bool {
					return strings.Contains(condition.Message, "does not name the shadow database the drift check policy asks for")
				}) {
					t.Fatalf("no condition says why the read was retried: %#v", actual.Status.Conditions)
				}
				return
			}
			if actual.Status.History == nil || actual.Status.History.ShadowIdentityDigest != testShadowDigest {
				t.Fatalf("the history records shadow %#v, want %s", actual.Status.History, testShadowDigest)
			}
			if len(plans.Items) != 1 || plans.Items[0].Spec.ShadowIdentityDigest != testShadowDigest {
				t.Fatalf("published plans %#v, want one binding shadow %s", plans.Items, testShadowDigest)
			}
		})
	}
}
