package e2e

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func TestExecutorVariantOutputRequiresANewDigestInTheSameRepository(t *testing.T) {
	t.Parallel()
	original := "registry.test/executor@sha256:" + strings.Repeat("a", 64)
	replacement := "registry.test/executor@sha256:" + strings.Repeat("b", 64)
	if got, err := executorVariantReference(original, "Executor: "+replacement+"\n"); err != nil || got != replacement {
		t.Fatalf("publication = %q, %v", got, err)
	}
	for _, output := range []string{
		"", replacement, "Executor: " + original, "Executor: registry.test/other@sha256:" + strings.Repeat("b", 64),
		"Executor: registry.test/executor:tag", "Executor: " + replacement + "\nExecutor: " + replacement,
		"warning\nExecutor: " + replacement,
	} {
		if _, err := executorVariantReference(original, output); err == nil {
			t.Fatalf("invalid publisher output passed: %q", output)
		}
	}
	if _, err := executorVariantReference("registry.test/executor:tag", "Executor: "+replacement); err == nil {
		t.Fatal("a mutable source image passed")
	}
}

func TestMigrationExecutorDecisionPreservesTheWorkAndRotatesTheEpoch(t *testing.T) {
	t.Parallel()
	before, current, old, fresh := migrationReplacementFixture()
	current.UID, fresh.Spec.MigrationRef.UID = before.UID, before.UID
	original, replacement := "registry.test/executor@sha256:"+strings.Repeat("a", 64), "registry.test/executor@sha256:"+strings.Repeat("b", 64)
	before.Status.ExecutionBinding = &ptahv1alpha1.ExecutionBindingStatus{Epoch: "v1-" + strings.Repeat("a", 32), ExecutorImage: original,
		PtahVersion: "v0.9.0", RunnerProtocolVersion: 1, ControllerStateVersion: 1}
	current.Status.ExecutionBinding = before.Status.ExecutionBinding.DeepCopy()
	current.Status.ExecutionBinding.Epoch, current.Status.ExecutionBinding.ExecutorImage = "v1-"+strings.Repeat("b", 32), replacement
	old.Spec.ExecutionBindingID, old.Spec.ExecutorImage = before.Status.ExecutionBinding.Epoch, original
	fresh.Spec.ExecutionBindingID, fresh.Spec.ExecutorImage = current.Status.ExecutionBinding.Epoch, replacement
	old.Spec.Migrations = append(old.Spec.Migrations, ptahv1alpha1.PlannedMigration{Version: 2, Checksum: "second"}, ptahv1alpha1.PlannedMigration{Version: 3, Checksum: "third"})
	fresh.Spec.Migrations = old.DeepCopy().Spec.Migrations
	if err := changedMigrationExecutorDecision(before, current, old, fresh, original, replacement); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahMigration, *ptahv1alpha1.PtahMigrationPlan){
		"same epoch": func(r *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ExecutionBinding.Epoch, p.Spec.ExecutionBindingID = before.Status.ExecutionBinding.Epoch, old.Spec.ExecutionBindingID
		},
		"invalid epoch": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ExecutionBinding.Epoch = "other"
		},
		"same image": func(r *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ExecutionBinding.ExecutorImage, p.Spec.ExecutorImage = original, original
		},
		"version change": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ExecutionBinding.PtahVersion = "other"
		},
		"protocol change": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ExecutionBinding.RunnerProtocolVersion++
		},
		"state change": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ExecutionBinding.ControllerStateVersion++
		},
		"resource replaced":  func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.UID = "other" },
		"spec changed":       func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Spec.Suspend = true },
		"generation changed": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Generation++ },
		"stale reading": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ObservedGeneration = 0
		},
		"missing binding": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ExecutionBinding = nil
		},
		"no approval gate": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Status.Conditions = nil },
		"active operation": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{}
		},
		"wrong plan": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Status.Plan.Name = "other" },
		"old plan UID": func(r *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.Plan.UID, p.UID = old.UID, old.UID
		},
		"wrong plan epoch": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.ExecutionBindingID = old.Spec.ExecutionBindingID
		},
		"wrong plan resource": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.MigrationRef.UID = "other"
		},
		"unchanged fingerprint": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.Fingerprint = old.Spec.Fingerprint
		},
		"changed artifact": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.ArtifactDigest = "other"
		},
		"changed target": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.TargetIdentityDigest = "other"
		},
		"changed realm": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.CoordinationDigest = "other"
		},
		"changed policy": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.PolicyFingerprint = "other"
		},
		"changed history": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.HistoryFingerprint = "other"
		},
		"changed sequence": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.Migrations[0].Checksum = "other"
		},
		"changed transaction mode": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.Migrations[0].TransactionMode = "none"
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, p := current.DeepCopy(), fresh.DeepCopy()
			edit(r, p)
			if changedMigrationExecutorDecision(before, r, old, p, original, replacement) == nil {
				t.Fatal("an unrelated or incomplete executor transition passed")
			}
		})
	}
	stale := before.DeepCopy()
	stale.Status.ObservedGeneration = 0
	if changedMigrationExecutorDecision(stale, current, old, fresh, original, replacement) == nil ||
		changedMigrationExecutorDecision(nil, current, old, fresh, original, replacement) == nil {
		t.Fatal("missing or stale initial evidence passed")
	}
}

func executorHistoryFixture(t *testing.T) (runner.Result, *ptahv1alpha1.PtahMigrationPlan) {
	t.Helper()
	history := dataplane.MigrationStatusReport{ContractVersion: dataplane.SupportedMigrationStatusContract, TotalMigrations: 3,
		HasPendingChanges: true, PendingMigrations: []int64{1, 2, 3}, Migrations: []dataplane.MigrationRecord{
			{Version: 1, Checksum: "first", State: dataplane.MigrationStatePending},
			{Version: 2, Checksum: "second", State: dataplane.MigrationStatePending},
			{Version: 3, Checksum: "third", State: dataplane.MigrationStatePending},
		}}
	fingerprint, err := migrationplan.HistoryFingerprint(history)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := migrationplan.Sequence(history)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ptahv1alpha1.PtahMigrationPlan{Spec: ptahv1alpha1.PtahMigrationPlanSpec{
		RunnerProtocolVersion: 1, TargetIdentityDigest: "target", CoordinationDigest: "realm", HistoryFingerprint: fingerprint, Migrations: sequence}}
	return runner.Result{Operation: runner.OperationMigrationHistory, OperationID: "history-control", ProtocolVersion: 1,
		TargetIdentityDigest: "target", CoordinationDigest: "realm", MigrationHistory: &history}, plan
}

func TestExecutorHistoryControlRequiresTheExactCompleteReading(t *testing.T) {
	t.Parallel()
	result, plan := executorHistoryFixture(t)
	if err := migrationHistoryMatchesExecutorPlan(result, plan); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*runner.Result, *ptahv1alpha1.PtahMigrationPlan){
		"wrong operation": func(r *runner.Result, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Operation = runner.OperationMigrationApply
		},
		"missing operation identity": func(r *runner.Result, _ *ptahv1alpha1.PtahMigrationPlan) { r.OperationID = "" },
		"wrong protocol":             func(r *runner.Result, _ *ptahv1alpha1.PtahMigrationPlan) { r.ProtocolVersion++ },
		"failed child":               func(r *runner.Result, _ *ptahv1alpha1.PtahMigrationPlan) { r.ChildExitCode = 1 },
		"runner refusal": func(r *runner.Result, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Error = &runner.ResultError{Code: "refused"}
		},
		"truncated result": func(r *runner.Result, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Truncation = &runner.TruncationMetadata{Stdout: true}
		},
		"missing history":          func(r *runner.Result, _ *ptahv1alpha1.PtahMigrationPlan) { r.MigrationHistory = nil },
		"wrong target":             func(r *runner.Result, _ *ptahv1alpha1.PtahMigrationPlan) { r.TargetIdentityDigest = "other" },
		"wrong realm":              func(r *runner.Result, _ *ptahv1alpha1.PtahMigrationPlan) { r.CoordinationDigest = "other" },
		"changed database history": func(r *runner.Result, _ *ptahv1alpha1.PtahMigrationPlan) { r.MigrationHistory.CurrentVersion = 1 },
		"unknown history contract": func(r *runner.Result, _ *ptahv1alpha1.PtahMigrationPlan) { r.MigrationHistory.ContractVersion++ },
		"different checksum":       func(_ *runner.Result, p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.Migrations[0].Checksum = "other" },
		"different order": func(_ *runner.Result, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.Migrations[0], p.Spec.Migrations[1] = p.Spec.Migrations[1], p.Spec.Migrations[0]
		},
		"shorter selection": func(_ *runner.Result, p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.Migrations = p.Spec.Migrations[:2] },
	} {
		t.Run(name, func(t *testing.T) {
			r, p := executorHistoryFixture(t)
			edit(&r, p)
			if migrationHistoryMatchesExecutorPlan(r, p) == nil {
				t.Fatal("wrong or incomplete History evidence passed")
			}
		})
	}
	if migrationHistoryMatchesExecutorPlan(result, nil) == nil {
		t.Fatal("missing plan passed")
	}
}

func TestExecutorSQLControlsRequireBothExactHistoryActors(t *testing.T) {
	t.Parallel()
	initial := operationSQLClient{resourceUID: "migration", jobUID: "old-job", podUID: "old-pod", operation: "history"}
	current := operationSQLClient{resourceUID: "migration", jobUID: "new-job", podUID: "new-pod", operation: "history"}
	clients := map[string]operationSQLClient{"10.0.0.1": initial, "10.0.0.2": current}
	counts := map[string]int{"10.0.0.1": 10, "10.0.0.2": 8}
	if err := migrationExecutorSQLControls(clients, counts, initial, current); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(map[string]operationSQLClient, map[string]int){
		"no observations":           func(c map[string]operationSQLClient, _ map[string]int) { clear(c) },
		"missing first History":     func(c map[string]operationSQLClient, _ map[string]int) { delete(c, "10.0.0.1") },
		"missing fresh History":     func(c map[string]operationSQLClient, _ map[string]int) { delete(c, "10.0.0.2") },
		"no SQL from fresh History": func(_ map[string]operationSQLClient, n map[string]int) { n["10.0.0.2"] = 0 },
		"wrong Pod": func(c map[string]operationSQLClient, _ map[string]int) {
			actor := c["10.0.0.2"]
			actor.podUID = "other"
			c["10.0.0.2"] = actor
		},
		"Apply substituted for History": func(c map[string]operationSQLClient, _ map[string]int) {
			actor := c["10.0.0.2"]
			actor.operation = "apply"
			c["10.0.0.2"] = actor
		},
		"ambiguous client": func(c map[string]operationSQLClient, n map[string]int) { c["10.0.0.3"], n["10.0.0.3"] = current, 1 },
	} {
		t.Run(name, func(t *testing.T) {
			c := map[string]operationSQLClient{"10.0.0.1": initial, "10.0.0.2": current}
			n := map[string]int{"10.0.0.1": 10, "10.0.0.2": 8}
			edit(c, n)
			if migrationExecutorSQLControls(c, n, initial, current) == nil {
				t.Fatal("missing or unrelated SQL evidence passed")
			}
		})
	}
	for _, edit := range []func(*operationSQLClient){
		func(c *operationSQLClient) { c.jobUID = initial.jobUID },
		func(c *operationSQLClient) { c.podUID = initial.podUID },
		func(c *operationSQLClient) { c.resourceUID = "other" },
		func(c *operationSQLClient) { c.jobUID = "" },
	} {
		actor := current
		edit(&actor)
		if migrationExecutorSQLControls(map[string]operationSQLClient{"10.0.0.1": initial, "10.0.0.2": actor}, counts, initial, actor) == nil {
			t.Fatal("non-distinct or unidentified control passed")
		}
	}
}

func TestExecutorApplyJobCarriesEveryApprovedInput(t *testing.T) {
	t.Parallel()
	_, plan := executorHistoryFixture(t)
	plan.Spec.ExecutorImage, plan.Spec.ExecutionBindingID = "replacement-executor", "replacement-epoch"
	sequence, err := workload.EncodeMigrationSequence(plan.Spec.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := workload.MigrationSequenceDigest(plan.Spec.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelOperation: "apply"}, Annotations: map[string]string{annotationBindingID: plan.Spec.ExecutionBindingID}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: plan.Spec.ExecutorImage, Env: []corev1.EnvVar{
			{Name: runner.EnvExpectedTargetIdentityDigest, Value: plan.Spec.TargetIdentityDigest},
			{Name: runner.EnvExpectedCoordinationDigest, Value: plan.Spec.CoordinationDigest},
			{Name: runner.EnvExpectedMigrationHistoryFingerprint, Value: plan.Spec.HistoryFingerprint},
			{Name: runner.EnvExpectedMigrationSequence, Value: sequence},
			{Name: runner.EnvExpectedMigrationSequenceDigest, Value: digest},
		}}}}}}}
	if err := migrationExecutorApplyInputs(job, plan); err != nil {
		t.Fatal(err)
	}
	for index, value := range job.Spec.Template.Spec.Containers[0].Env {
		t.Run(value.Name, func(t *testing.T) {
			changed := job.DeepCopy()
			changed.Spec.Template.Spec.Containers[0].Env[index].Value = "other"
			if migrationExecutorApplyInputs(changed, plan) == nil {
				t.Fatal("changed approved input passed")
			}
			missing := job.DeepCopy()
			env := &missing.Spec.Template.Spec.Containers[0].Env
			*env = append((*env)[:index], (*env)[index+1:]...)
			if migrationExecutorApplyInputs(missing, plan) == nil {
				t.Fatal("omitted approved input passed")
			}
		})
	}
	for name, edit := range map[string]func(*batchv1.Job){
		"wrong image":     func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Image = "original" },
		"wrong epoch":     func(j *batchv1.Job) { j.Annotations[annotationBindingID] = "original" },
		"wrong operation": func(j *batchv1.Job) { j.Labels[labelOperation] = "history" },
		"indirect value":  func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Env[0].ValueFrom = &corev1.EnvVarSource{} },
		"duplicate input": func(j *batchv1.Job) { c := &j.Spec.Template.Spec.Containers[0]; c.Env = append(c.Env, c.Env[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := job.DeepCopy()
			edit(changed)
			if migrationExecutorApplyInputs(changed, plan) == nil {
				t.Fatal("an unbound Apply passed")
			}
		})
	}
	if migrationExecutorApplyInputs(nil, plan) == nil || migrationExecutorApplyInputs(job, nil) == nil {
		t.Fatal("missing evidence passed")
	}
}
