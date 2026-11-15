package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The documents driftCheckProof printed for the PostgreSQL migration whose
// approved Apply the drift check refused: the run's record, the decision asked
// for after it, and the run that applied once the schema matched.
// testdata/e2e/readings/README.md says in which run.
const (
	dcRefusedFile  = "../../testdata/e2e/readings/drift-refused-postgresql.json"
	dcDecisionFile = "../../testdata/e2e/readings/drift-decision-postgresql.json"
	dcCheckedFile  = "../../testdata/e2e/readings/drift-checked-postgresql.json"
)

// dcRead decodes a reading afresh, so a mutation changes one copy only.
func dcRead(t *testing.T, file string) *ptahv1alpha1.PtahMigration {
	t.Helper()
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var migration ptahv1alpha1.PtahMigration
	if err := json.Unmarshal(content, &migration); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	if migration.Status.LastRun == nil || migration.Status.LastRun.JobUID == "" {
		t.Fatalf("%s holds no run", file)
	}
	return &migration
}

// dcRefusedJob is the Job of the run the drift check refused, as the readings
// name it.
func dcRefusedJob(t *testing.T) string {
	t.Helper()
	return string(dcRead(t, dcRefusedFile).Status.LastRun.JobUID)
}

// The operator's record of the refused run passes, and each copy of it with
// one clause broken is refused: a run of another Job, one that applied or ran
// a statement, and the other refusals that also read Failed with nothing
// applied.
func TestDriftCheckRefusedRunRefusesEachClause(t *testing.T) {
	t.Parallel()
	refused := dcRefusedJob(t)
	if err := driftCheckRefusedRun(dcRead(t, dcRefusedFile).Status, refused); err != nil {
		t.Fatalf("the operator's record of the refused run was refused: %v", err)
	}
	// Each mutation returns the Job the row asks about.
	asked := func(edit func(*ptahv1alpha1.MigrationRunStatus)) func(*ptahv1alpha1.MigrationRunStatus) string {
		return func(run *ptahv1alpha1.MigrationRunStatus) string {
			edit(run)
			return refused
		}
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.MigrationRunStatus) string{
		"another Job's run":  func(*ptahv1alpha1.MigrationRunStatus) string { return "another-job" },
		"no Job asked about": func(*ptahv1alpha1.MigrationRunStatus) string { return "" },
		"a run with no Job":  func(run *ptahv1alpha1.MigrationRunStatus) string { run.JobUID = ""; return "" },
		"an applied run":     asked(func(run *ptahv1alpha1.MigrationRunStatus) { run.Outcome = ptahv1alpha1.MigrationRunOutcomeApplied }),
		"an applied version": asked(func(run *ptahv1alpha1.MigrationRunStatus) { run.AppliedVersions = []int64{3} }),
		"a migration failing": asked(func(run *ptahv1alpha1.MigrationRunStatus) {
			run.Failure = &ptahv1alpha1.MigrationRunFailureStatus{Version: 3, Statement: 1, Statements: 1}
		}),
		"a check that could not use its shadow": asked(func(run *ptahv1alpha1.MigrationRunStatus) {
			run.Message = "The drift check could not use its shadow database (stage claim, reason not_clean), so the run applied nothing"
		}),
		"a check that could not decide": asked(func(run *ptahv1alpha1.MigrationRunStatus) {
			run.Message = "The drift check could not decide (stage history, reason out_of_order), so the run applied nothing"
		}),
		"a selection the plan did not approve": asked(func(run *ptahv1alpha1.MigrationRunStatus) {
			run.Message = "Ptah selected [2 3] under the migration lock and the plan approved [3], so it ran nothing"
		}),
		"the finding's words later in the message": asked(func(run *ptahv1alpha1.MigrationRunStatus) { run.Message = "note: " + run.Message }),
		"a finding that does not say the run applied nothing": asked(func(run *ptahv1alpha1.MigrationRunStatus) {
			run.Message, _, _ = strings.Cut(run.Message, driftAppliedNothing)
		}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := dcRead(t, dcRefusedFile).Status
			if err := driftCheckRefusedRun(status, mutate(status.LastRun)); err == nil {
				t.Fatalf("a run with %s was accepted", name)
			}
		})
	}
	if driftCheckRefusedRun(ptahv1alpha1.PtahMigrationStatus{}, refused) == nil {
		t.Fatal("a status with no run was accepted")
	}
}

// The decision the operator asked for after the refused run passes, and the
// same reading is refused for another run, for a condition that asks for
// nothing, and with no plan to approve.
func TestDecisionAfterRunRefusesEachClause(t *testing.T) {
	t.Parallel()
	refused := dcRefusedJob(t)
	asked := dcRead(t, dcDecisionFile)
	plan, ok := decisionAfterRun(asked.Status, refused)
	if !ok || plan != asked.Status.Plan.Name {
		t.Fatalf("the decision asked for after the refused run read %q, %t", plan, ok)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigrationStatus) string{
		"another run": func(*ptahv1alpha1.PtahMigrationStatus) string { return "another-job" },
		"no run":      func(status *ptahv1alpha1.PtahMigrationStatus) string { status.LastRun = nil; return refused },
		"no plan":     func(status *ptahv1alpha1.PtahMigrationStatus) string { status.Plan = nil; return refused },
		"a plan with no name": func(status *ptahv1alpha1.PtahMigrationStatus) string {
			status.Plan.Name = ""
			return refused
		},
		"a decision already made": func(status *ptahv1alpha1.PtahMigrationStatus) string {
			setReadingCondition(status, ptahv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, "Approved")
			return refused
		},
		"another reason": func(status *ptahv1alpha1.PtahMigrationStatus) string {
			setReadingCondition(status, ptahv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue, "ApprovalStale")
			return refused
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := dcRead(t, dcDecisionFile).Status
			job := mutate(&status)
			if _, ok := decisionAfterRun(status, job); ok {
				t.Fatalf("a decision with %s was accepted", name)
			}
		})
	}
	// The record of the refusal itself asks for nothing yet.
	if _, ok := decisionAfterRun(dcRead(t, dcRefusedFile).Status, refused); ok {
		t.Fatal("the record of the refusal was read as a decision asked for after it")
	}
}

// setReadingCondition replaces one condition of a reading in place.
func setReadingCondition(status *ptahv1alpha1.PtahMigrationStatus, kind string, value metav1.ConditionStatus, reason string) {
	for index := range status.Conditions {
		if status.Conditions[index].Type == kind {
			status.Conditions[index].Status, status.Conditions[index].Reason = value, reason
		}
	}
}

// The run that applied once the schema matched passes, and each copy of it
// with one clause broken is refused, among them the refused run itself and the
// first run, which applied the two older migrations.
func TestDriftCheckedRunAppliedRefusesEachClause(t *testing.T) {
	t.Parallel()
	refused := dcRefusedJob(t)
	if err := driftCheckedRunApplied(dcRead(t, dcCheckedFile), refused, 3); err != nil {
		t.Fatalf("the operator's settled reading was refused: %v", err)
	}
	if err := driftCheckedRunApplied(dcRead(t, dcRefusedFile), refused, 3); err == nil {
		t.Fatal("the record of the refused run was read as the run that applied")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration) string{
		"no refused run": func(*ptahv1alpha1.PtahMigration) string { return "" },
		"the refused run": func(m *ptahv1alpha1.PtahMigration) string {
			m.Status.LastRun.JobUID = types.UID(refused)
			return refused
		},
		"a run with no Job": func(m *ptahv1alpha1.PtahMigration) string { m.Status.LastRun.JobUID = ""; return refused },
		"the first run": func(m *ptahv1alpha1.PtahMigration) string {
			m.Status.LastRun.AppliedVersions = []int64{1, 2}
			return refused
		},
		"more than the held-back version": func(m *ptahv1alpha1.PtahMigration) string {
			m.Status.LastRun.AppliedVersions = []int64{2, 3}
			return refused
		},
		"nothing applied": func(m *ptahv1alpha1.PtahMigration) string { m.Status.LastRun.AppliedVersions = nil; return refused },
		"a failed run": func(m *ptahv1alpha1.PtahMigration) string {
			m.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeFailed
			return refused
		},
		"no run": func(m *ptahv1alpha1.PtahMigration) string { m.Status.LastRun = nil; return refused },
		"another phase": func(m *ptahv1alpha1.PtahMigration) string {
			m.Status.Phase = ptahv1alpha1.MigrationPhaseVerifyingHistory
			return refused
		},
		"a stale status": func(m *ptahv1alpha1.PtahMigration) string { m.Status.ObservedGeneration--; return refused },
		"no generation": func(m *ptahv1alpha1.PtahMigration) string {
			m.Generation, m.Status.ObservedGeneration = 0, 0
			return refused
		},
		"work in flight": func(m *ptahv1alpha1.PtahMigration) string {
			m.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{}
			return refused
		},
		"an unresolved run": func(m *ptahv1alpha1.PtahMigration) string {
			m.Status.UnresolvedRun = &ptahv1alpha1.UnresolvedMigrationRunStatus{}
			return refused
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := dcRead(t, dcCheckedFile)
			job := mutate(migration)
			if err := driftCheckedRunApplied(migration, job, 3); err == nil {
				t.Fatalf("a reading with %s was accepted", name)
			}
		})
	}
}

// The builder's Apply Job for a migration that checks for drift mounts its
// shadow, and the Jobs of one that does not mount none; a volume of the same
// name from anything but that Secret is no shadow.
func TestMountsShadowReadsTheBuildersJobs(t *testing.T) {
	t.Parallel()
	const shadow = "e2e-postgresql-drift-shadow-db"
	migration := mtMigration()
	migration.Spec.Policy.DriftCheck = &ptahv1alpha1.MigrationDriftCheck{Shadow: ptahv1alpha1.DatabaseTargetRef{
		URLFrom: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: shadow}, Key: "url"},
	}}
	apply := func(jobs []batchv1.Job) batchv1.Job {
		for _, job := range jobs {
			if job.Labels[labelOperation] == "apply" {
				return job
			}
		}
		t.Fatal("the builder wrote no Apply Job")
		return batchv1.Job{}
	}
	checked := apply(mtBuiltJobsOf(t, migration, builderDigest('7')))
	if !mountsShadow(&checked, shadow) {
		t.Fatal("the builder's drift-checked Apply Job was read as mounting no shadow")
	}
	if mountsShadow(&checked, "another-shadow") {
		t.Fatal("the drift-checked Apply Job was read as mounting another Secret")
	}
	unchecked := apply(mtBuiltJobs(t))
	if mountsShadow(&unchecked, shadow) {
		t.Fatal("an Apply Job with no drift check was read as mounting a shadow")
	}
	for name, mutate := range map[string]func(*corev1.Volume){
		"another volume": func(volume *corev1.Volume) { volume.Name = "database-url" },
		"a ConfigMap": func(volume *corev1.Volume) {
			volume.Secret, volume.ConfigMap = nil, &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: shadow}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			job := *checked.DeepCopy()
			for index := range job.Spec.Template.Spec.Volumes {
				if job.Spec.Template.Spec.Volumes[index].Name == shadowVolume {
					mutate(&job.Spec.Template.Spec.Volumes[index])
				}
			}
			if mountsShadow(&job, shadow) {
				t.Fatalf("a Job whose shadow volume is %s was read as mounting the shadow", name)
			}
		})
	}
}

// dcLoggedLine is the line the runner writes to the Pod log for the document
// Ptah v0.28.0 printed for a column nobody migrated, which the runner harness
// captured: its prefix and the first line of Ptah's error.
func dcLoggedLine(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile("../../internal/dataplane/testdata/migrations-up-drifted-postgres.json")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(content, &document); err != nil || document.Error.Message == "" {
		t.Fatalf("the captured document carries no error: %v", err)
	}
	first, _, _ := strings.Cut(document.Error.Message, "\n")
	return reportedErrorPrefix + first + "\n"
}

// The line the runner writes for Ptah's own document passes, and a log that
// says something else, or says it twice, or says it on Ptah's own standard
// error rather than through the runner, is refused.
func TestDriftSentenceLoggedRefusesEachClause(t *testing.T) {
	t.Parallel()
	line := dcLoggedLine(t)
	if err := driftSentenceLogged([]byte("ptah-runner: running ptah migrations up\n" + line)); err != nil {
		t.Fatalf("the runner's line for Ptah's document was refused: %v", err)
	}
	for name, log := range map[string]string{
		"nothing reported": "ptah-runner: running ptah migrations up\n",
		"the line twice":   line + line,
		"another column":   strings.Replace(line, "nickname", "color", 1),
		"a shadow it could not use": reportedErrorPrefix + "the drift check could not compare the database with its applied history, " +
			"so no pending migration was run: applied history shadow check failed: connected database is not clean\n",
		"Ptah's standard error": "error: error running migrations: " + strings.TrimPrefix(line, reportedErrorPrefix),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := driftSentenceLogged([]byte(log)); err == nil {
				t.Fatalf("a log with %s was accepted", name)
			}
		})
	}
}

// carriesDriftExclude reads the list the builder writes into the Apply Job,
// and refuses one that names other selectors, fewer of them, or none.
func TestCarriesDriftExcludeReadsTheBuildersJobs(t *testing.T) {
	t.Parallel()
	const queue = "public.e2e_unmigrated_queue"
	migration := mtMigration()
	migration.Spec.Policy.DriftCheck = &ptahv1alpha1.MigrationDriftCheck{
		Shadow: ptahv1alpha1.DatabaseTargetRef{URLFrom: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "e2e-postgresql-drift-shadow-db"}, Key: "url",
		}},
		Exclude: []string{queue, "legacy,archive"},
	}
	var apply batchv1.Job
	for _, job := range mtBuiltJobsOf(t, migration, builderDigest('7')) {
		if job.Labels[labelOperation] == "apply" {
			apply = job
		}
	}
	if apply.Name == "" {
		t.Fatal("the builder wrote no Apply Job")
	}
	if !carriesDriftExclude(&apply, "legacy,archive", queue) {
		t.Fatal("the builder's Apply Job was read as carrying another exclude list")
	}
	for name, selectors := range map[string][]string{
		"one of the selectors": {queue},
		"another selector":     {"legacy,archive", "public.other"},
		"another order":        {queue, "legacy,archive"},
		"no selector":          nil,
	} {
		if carriesDriftExclude(&apply, selectors...) {
			t.Errorf("the Apply Job was read as carrying %s: %q", name, selectors)
		}
	}
	// A list a second container also holds is not the one the runner reads.
	twice := *apply.DeepCopy()
	containers := twice.Spec.Template.Spec.Containers
	for _, container := range containers {
		for _, variable := range container.Env {
			if variable.Name == driftExcludeVariable {
				containers = append(containers, corev1.Container{Name: "another", Env: []corev1.EnvVar{variable}})
			}
		}
	}
	twice.Spec.Template.Spec.Containers = containers
	if carriesDriftExclude(&twice, "legacy,archive", queue) {
		t.Error("a Job whose two containers carry the list was read as handing it to one runner")
	}
	migration.Spec.Policy.DriftCheck.Exclude = nil
	for _, job := range mtBuiltJobsOf(t, migration, builderDigest('7')) {
		if job.Labels[labelOperation] == "apply" && carriesDriftExclude(&job) {
			t.Fatal("an Apply Job whose check leaves nothing out was read as carrying an empty list")
		}
	}
}
