package controller

import (
	"context"
	"maps"
	"strings"
	"sync/atomic"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// A migration claim that finds its own resource's Job under the name it
// reserved used to take it on the UID and the owner alone. It now asks
// jobclaim.Match, as a schema claim does: the Job the claim builds now, with
// the manager identity carried from the live Pod template, is held to the Job
// standing there, and that Job's epoch and Pod template are held to the claim.
//
// Every Job here comes from the real builder. The fixture builder writes no
// labels and no manager identity, so a Job it built matches another manager's
// Job as readily as its own, and the question these tests ask would have
// nothing to tell apart.

// dispatchedRun is what a stopped pass leaves behind: an active migration
// claim, and the running Job a manager built and created for it, with the Pod
// that Job made.
type dispatchedRun struct {
	migration *operatorv1alpha1.PtahMigration
	plan      *operatorv1alpha1.PtahMigrationPlan
	job       *batchv1.Job
	pod       *corev1.Pod
}

// runShape says who built a dispatched run's Job and what the claim knows
// about it.
type runShape struct {
	dispatcher workload.Builder
	// alter changes the Job as the dispatching release wrote it, before the
	// claim's admission snapshot is resolved, so the snapshot pins the altered
	// template the way a real dispatch pins whatever its builder wrote.
	alter func(*batchv1.Job)
	// misrecord changes the template the snapshot is resolved from and
	// leaves the Job as built, so the snapshot records a template the Job
	// does not run.
	misrecord func(*corev1.PodTemplateSpec)
	// recorded says the pass that created the Job also recorded its UID, so
	// the claim supervises the Job rather than adopting it.
	recorded bool
}

// runDispatchedBy stands up a dispatched run of operationType.
func runDispatchedBy(
	t *testing.T,
	operationType operatorv1alpha1.MigrationOperationType,
	shape runShape,
) dispatchedRun {
	t.Helper()

	var migration *operatorv1alpha1.PtahMigration
	var plan *operatorv1alpha1.PtahMigrationPlan
	switch operationType {
	case operatorv1alpha1.MigrationOperationApply:
		migration, plan = awaitingApprovalFixture(t)
		applyClaimFor(t, migration, plan)
	case operatorv1alpha1.MigrationOperationHistory:
		migration = migrationFixture()
		migration.Finalizers = []string{migrationOperationFinalizer}
		migration.Status.ExecutionBinding = migrationExecutionBinding()
		migration.Status.Artifact = resolvedMigrationArtifact()
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
		migrationClaim(t, migration, operationType)
	default:
		t.Fatalf("no dispatched-run fixture for a %s claim", operationType)
	}
	operation := migration.Status.ActiveOperation
	build := func() *batchv1.Job {
		t.Helper()
		job, err := shape.dispatcher.BuildMigration(migration, *operation, plan)
		if err != nil {
			t.Fatalf("build the dispatched %s Job: %v", operationType, err)
		}
		if shape.alter != nil {
			shape.alter(job)
		}
		return job
	}
	operation.AdmissionSnapshot = nil
	template := build().Spec.Template.DeepCopy()
	if shape.misrecord != nil {
		shape.misrecord(template)
	}
	operation.AdmissionSnapshot = testAdmissionSnapshotOf(template)

	job := build()
	job.UID = types.UID("dispatched-job-uid")
	job.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{
		batchv1.ControllerUidLabel: string(job.UID),
	}}
	job.Spec.Template.Labels[batchv1.ControllerUidLabel] = string(job.UID)
	job.Spec.Template.Labels[batchv1.JobNameLabel] = job.Name
	job.Status.Active = 1

	// The hand-built workload supplies a Pod's spec and status; its identity
	// is replaced with the one the built Job gives it.
	_, reference := terminalMigrationWorkload(migration, batchv1.JobComplete)
	operation.JobUID = ""
	if shape.recorded {
		operation.JobUID = job.UID
	}
	pod := reference.DeepCopy()
	pod.Labels = maps.Clone(job.Spec.Template.Labels)
	pod.Annotations = maps.Clone(job.Spec.Template.Annotations)
	pod.OwnerReferences = []metav1.OwnerReference{jobControllerReference(job)}
	pod.Spec = *job.Spec.Template.Spec.DeepCopy()
	pod.Spec.Priority = reference.Spec.Priority
	pod.Spec.PreemptionPolicy = reference.Spec.PreemptionPolicy
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, reference.Spec.Tolerations...)
	runningExecutorPod(pod)
	return dispatchedRun{migration: migration, plan: plan, job: job, pod: pod}
}

// reconciler serves the run to a reconciler that builds its Jobs with jobs.
// A claim that holds the realm holds it already, as a dispatched one does.
func (run dispatchedRun) reconciler(t *testing.T, jobs MigrationJobBuilder) (*MigrationReconciler, client.WithWatch) {
	t.Helper()

	objects := []client.Object{run.migration, run.job, run.pod, verificationPolicyConfigMap()}
	if run.plan != nil {
		objects = append(objects, run.plan)
	}
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, objects...)
	reconciler.Jobs = jobs
	if migrationOperation(run.migration.Status.ActiveOperation).HoldsLock(false) {
		holdMigrationApplyLease(t, reconciler, api, run.migration)
	}
	return reconciler, api
}

// managerOnlyPredecessor is the release before current when the two differ
// only in what a Job records about the manager that built it: its image, its
// revision and its runner image. The execution binding is the same.
func managerOnlyPredecessor(current workload.Builder) workload.Builder {
	previous := current
	previous.ControllerImage = "example.invalid/manager@sha256:" + strings.Repeat("c", 64)
	previous.ControllerRevision = testControllerRevision + "-previous"
	previous.RunnerImage = "example.invalid/operator@sha256:" + strings.Repeat("c", 64)
	return previous
}

// withAnExecutorSetting is what a release that changed the Pod template
// wrote: one more environment variable on the executor.
func withAnExecutorSetting(job *batchv1.Job) {
	containers := job.Spec.Template.Spec.Containers
	for index := range containers {
		if containers[index].Name == executorContainerName {
			containers[index].Env = append(containers[index].Env,
				corev1.EnvVar{Name: "PTAH_RELEASE_SETTING", Value: "previous"})
			return
		}
	}
	panic("the built Job has no executor container to change")
}

// underAnotherEpoch is a Job that carries an execution epoch its claim was
// not made under.
func underAnotherEpoch(job *batchv1.Job) {
	for _, annotations := range []map[string]string{job.Annotations, job.Spec.Template.Annotations} {
		annotations[workload.AnnotationExecutionBindingID] = "v1-" + strings.Repeat("4", 32)
	}
}

// recordedForAnotherTemplate is a snapshot resolved for a Pod template the
// Job does not run.
func recordedForAnotherTemplate(template *corev1.PodTemplateSpec) {
	template.Annotations = maps.Clone(template.Annotations)
	template.Annotations["operator.ptah.run/another-template"] = "true"
}

// assertAdopted fails unless the pass kept the claim and bound it to the run's
// Job.
func assertAdopted(t *testing.T, api client.Client, run dispatchedRun) {
	t.Helper()

	claimed := run.migration.Status.ActiveOperation
	actual := readMigration(t, api, run.migration)
	operation := actual.Status.ActiveOperation
	if operation == nil || operation.ID != claimed.ID || operation.Attempt != claimed.Attempt {
		t.Fatalf("the claim did not survive adopting its own Job: operation=%#v lastRun=%#v conditions=%#v",
			operation, actual.Status.LastRun, actual.Status.Conditions)
	}
	if operation.JobUID != run.job.UID {
		t.Fatalf("claim records Job UID %q, want the adopted Job's %q", operation.JobUID, run.job.UID)
	}
	if migrationOperation(operation).Mutating && !operation.DispatchStarted {
		t.Fatal("an adopted Apply is recorded as never dispatched")
	}
	if actual.Status.LastRun != nil {
		t.Fatalf("adopting a running Job settled a run: %#v", actual.Status.LastRun)
	}
}

// assertRefused fails unless the pass settled the claim the way its operation
// settles a Job it cannot confirm, for the reason want names: an Apply is
// recorded as outcome unknown, naming the Job, and a read-only claim moves to
// a new attempt under a new name, leaving the Job as it found it.
func assertRefused(t *testing.T, api client.Client, run dispatchedRun, want string) {
	t.Helper()

	claimed := run.migration.Status.ActiveOperation
	actual := readMigration(t, api, run.migration)
	if migrationOperation(claimed).Mutating {
		if actual.Status.ActiveOperation != nil {
			t.Fatalf("an Apply claim kept a Job it cannot confirm: operation=%#v", actual.Status.ActiveOperation)
		}
		last := actual.Status.LastRun
		if last == nil || last.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown {
			t.Fatalf("last run = %#v, want an unknown outcome", last)
		}
		if last.JobUID != run.job.UID {
			t.Fatalf("the unknown run names Job UID %q, want the Job that may have run, %q", last.JobUID, run.job.UID)
		}
		if !strings.Contains(last.Message, want) {
			t.Fatalf("the unknown run says %q, want it to say %q", last.Message, want)
		}
		unresolved := actual.Status.UnresolvedRun
		if unresolved == nil || unresolved.OperationID != claimed.ID || unresolved.JobUID != run.job.UID {
			t.Fatalf("unresolved run = %#v, want the claim's run on Job %q", unresolved, run.job.UID)
		}
		if !meta.IsStatusConditionTrue(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked) {
			t.Fatalf("conditions = %#v, want the resource blocked on the unknown run", actual.Status.Conditions)
		}
		return
	}
	operation := actual.Status.ActiveOperation
	if operation == nil || operation.ID != claimed.ID || operation.Attempt != claimed.Attempt+1 {
		t.Fatalf("operation = %#v, want the claim moved to attempt %d", operation, claimed.Attempt+1)
	}
	if operation.JobUID != "" || operation.JobName == claimed.JobName {
		t.Fatalf("the retried claim names Job %q (UID %q), want a new name and no UID", operation.JobName, operation.JobUID)
	}
	progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || !strings.Contains(progressing.Message, want) {
		t.Fatalf("Progressing = %#v, want it to say %q", progressing, want)
	}
	left := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(run.job), left); err != nil {
		t.Fatalf("read the Job the claim left: %v", err)
	}
	if left.UID != run.job.UID || left.Spec.TTLSecondsAfterFinished != nil {
		t.Fatalf("the Job the claim could not confirm was changed: UID %q, TTL %v", left.UID, left.Spec.TTLSecondsAfterFinished)
	}
}

// claimsAndVerdicts is every way a claim meets its Job: an Apply and a
// read-only History reading, each adopting a Job whose UID it never recorded
// and supervising one whose UID it did.
func claimsAndVerdicts() []struct {
	name      string
	operation operatorv1alpha1.MigrationOperationType
	recorded  bool
} {
	return []struct {
		name      string
		operation operatorv1alpha1.MigrationOperationType
		recorded  bool
	}{
		{name: "an Apply adopting", operation: operatorv1alpha1.MigrationOperationApply},
		{name: "an Apply supervising", operation: operatorv1alpha1.MigrationOperationApply, recorded: true},
		{name: "a History reading adopting", operation: operatorv1alpha1.MigrationOperationHistory},
		{name: "a History reading supervising", operation: operatorv1alpha1.MigrationOperationHistory, recorded: true},
	}
}

// TestAMigrationAdoptsARunOnlyWhereItsReleaseBuildsTheSameJob is the upgrade
// the matcher changes. A manager replaced under a running claim adopts the
// Job its predecessor dispatched when it builds the same Job apart from the
// manager identity, which it takes from the live Pod template. A predecessor
// that built a different Pod template left a run this manager cannot
// confirm: an Apply is recorded as outcome unknown, and a read-only claim is
// run again.
func TestAMigrationAdoptsARunOnlyWhereItsReleaseBuildsTheSameJob(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	predecessor := managerOnlyPredecessor(current)
	for _, claim := range claimsAndVerdicts() {
		for _, row := range []struct {
			name       string
			dispatcher workload.Builder
			alter      func(*batchv1.Job)
			adopted    bool
		}{
			{name: "built by this manager", dispatcher: current, adopted: true},
			{name: "built by a release that differs in the recorded manager alone", dispatcher: predecessor, adopted: true},
			{name: "built by a release that also changed the Pod template", dispatcher: predecessor, alter: withAnExecutorSetting},
		} {
			t.Run(claim.name+"/"+row.name, func(t *testing.T) {
				t.Parallel()

				run := runDispatchedBy(t, claim.operation, runShape{
					dispatcher: row.dispatcher, alter: row.alter, recorded: claim.recorded,
				})
				if row.dispatcher.ControllerImage != current.ControllerImage &&
					run.job.Spec.Template.Annotations[workload.AnnotationControllerImage] == current.ControllerImage {
					t.Fatal("the predecessor's Job records this manager, so the row proves nothing about carrying it")
				}
				reconciler, api := run.reconciler(t, current)

				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(run.migration)); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				if row.adopted {
					assertAdopted(t, api, run)
					return
				}
				assertRefused(t, api, run, "the Job's spec is not the one its claim builds")
			})
		}
	}
}

// TestAMigrationRefusesAJobUnderAnotherEpoch holds the Job under a claim's
// reserved name to the execution epoch the claim was made under. The Job is
// otherwise consistent: its admission snapshot was resolved from the template
// it runs.
func TestAMigrationRefusesAJobUnderAnotherEpoch(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, claim := range claimsAndVerdicts() {
		t.Run(claim.name, func(t *testing.T) {
			t.Parallel()

			run := runDispatchedBy(t, claim.operation, runShape{
				dispatcher: current, alter: underAnotherEpoch, recorded: claim.recorded,
			})
			if epoch := run.job.Annotations[workload.AnnotationExecutionBindingID]; epoch == run.migration.Status.ActiveOperation.ExecutionBindingID {
				t.Fatalf("the Job runs under the claim's own epoch %q, so the row proves nothing", epoch)
			}
			reconciler, api := run.reconciler(t, current)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			assertRefused(t, api, run, "another execution epoch than its claim")
		})
	}
}

// TestAMigrationRefusesAJobWhosePodTemplateIsNotItsSnapshots holds the Job to
// the Pod template the claim's admission snapshot recorded before dispatch.
// The Job is the one the claim rebuilds, so nothing but the snapshot can tell
// the two apart.
func TestAMigrationRefusesAJobWhosePodTemplateIsNotItsSnapshots(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, claim := range claimsAndVerdicts() {
		t.Run(claim.name, func(t *testing.T) {
			t.Parallel()

			run := runDispatchedBy(t, claim.operation, runShape{
				dispatcher: current, misrecord: recordedForAnotherTemplate, recorded: claim.recorded,
			})
			digest, err := podintent.DigestTemplate(&run.job.Spec.Template)
			if err != nil {
				t.Fatal(err)
			}
			if digest == run.migration.Status.ActiveOperation.AdmissionSnapshot.TemplateDigest {
				t.Fatal("the snapshot records the template the Job runs, so the row proves nothing")
			}
			reconciler, api := run.reconciler(t, current)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			assertRefused(t, api, run, "does not match the persisted admission snapshot")
		})
	}
}

// TestAMigrationClaimThatCannotRebuildItsJobDoesNotAdoptIt is the matcher's
// precondition. A claim that cannot build its Job has nothing to hold the Job
// under its name to, so it cannot confirm it, and settles it as it settles
// any Job it cannot confirm. An Apply whose plan is gone is the case a person
// can cause: the plan names the sequence the Job was built to run.
func TestAMigrationClaimThatCannotRebuildItsJobDoesNotAdoptIt(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, row := range []struct {
		name      string
		operation operatorv1alpha1.MigrationOperationType
		jobs      MigrationJobBuilder
		want      string
	}{
		{name: "an Apply whose plan was deleted", operation: operatorv1alpha1.MigrationOperationApply,
			jobs: current, want: "rebuild immutable Apply Job intent"},
		{name: "a History reading the builder refuses", operation: operatorv1alpha1.MigrationOperationHistory,
			jobs: failingMigrationBuildJobs{}, want: "rebuild immutable Job intent"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := runDispatchedBy(t, row.operation, runShape{dispatcher: current, recorded: true})
			served := run
			served.plan = nil
			reconciler, api := served.reconciler(t, row.jobs)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			assertRefused(t, api, run, row.want)
		})
	}
}

// TestAnUnconfirmableMigrationApplyIsNeverDispatchedAgain is the safety the
// asymmetry exists for. An Apply claim that cannot confirm the Job under its
// name may be looking at an executor that is running SQL, so the run goes to
// the uncertain path: it is recorded as outcome unknown, the database stays
// held while the Job can still write, and no pass creates a second Job. That
// holds whether the claim refused the Job against its rebuild or, once its
// inputs moved, against what it fixes without one.
func TestAnUnconfirmableMigrationApplyIsNeverDispatchedAgain(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, row := range []struct {
		name  string
		shape runShape
		moved bool
		want  string
	}{
		{
			name:  "adopting a Job another release built",
			shape: runShape{dispatcher: managerOnlyPredecessor(current), alter: withAnExecutorSetting},
			want:  "dispatched Apply Job intent changed",
		},
		{
			name:  "supervising a Job another release built",
			shape: runShape{dispatcher: managerOnlyPredecessor(current), alter: withAnExecutorSetting, recorded: true},
			want:  "dispatched Apply Job intent changed",
		},
		{
			name:  "adopting a Job under another epoch after the inputs moved",
			shape: runShape{dispatcher: current, alter: underAnotherEpoch},
			moved: true,
			want:  "dispatched Apply Job is not its claim's",
		},
		{
			name:  "supervising a Job under another epoch after the inputs moved",
			shape: runShape{dispatcher: current, alter: underAnotherEpoch, recorded: true},
			moved: true,
			want:  "dispatched Apply Job is not its claim's",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, row.shape)
			if row.moved {
				moveMigrationInputs(run.migration)
			}
			reconciler, api := run.reconciler(t, current)
			if row.moved {
				requireMigrationInputsMoved(t, reconciler, run.migration)
			}
			var creates, applyCreates atomic.Int64
			reconciler.Client = interceptor.NewClient(api, interceptor.Funcs{
				Create: func(
					ctx context.Context,
					writer client.WithWatch,
					object client.Object,
					options ...client.CreateOption,
				) error {
					if job, ok := object.(*batchv1.Job); ok {
						creates.Add(1)
						if job.Labels[workload.LabelOperation] == "apply" {
							applyCreates.Add(1)
						}
					}
					return writer.Create(ctx, object, options...)
				},
			})

			// The first pass refuses the Job, and the rest must not dispatch
			// beside it. An edit is a new generation, which the resource
			// resolves again, so a moved row may create that read-only Job and
			// nothing else.
			for pass := range 4 {
				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(run.migration)); err != nil {
					t.Fatalf("Reconcile() pass %d error = %v", pass, err)
				}
				if pass == 0 {
					assertRefused(t, api, run, row.want)
				}
			}
			if unresolved := readMigration(t, api, run.migration).Status.UnresolvedRun; unresolved == nil ||
				unresolved.OperationID != run.migration.Status.ActiveOperation.ID {
				t.Fatalf("unresolved run = %#v, want the refused Apply's run to stand", unresolved)
			}
			if created := applyCreates.Load(); created != 0 {
				t.Fatalf("%d Apply Job creates were attempted after the claim could not confirm its Job, want none", created)
			}
			if created := creates.Load(); !row.moved && created != 0 {
				t.Fatalf("%d Job creates were attempted after the claim could not confirm its Job, want none", created)
			}
			jobs := &batchv1.JobList{}
			if err := api.List(context.Background(), jobs, client.InNamespace(run.migration.Namespace)); err != nil {
				t.Fatal(err)
			}
			var applies []types.UID
			for index := range jobs.Items {
				if jobs.Items[index].Labels[workload.LabelOperation] == "apply" {
					applies = append(applies, jobs.Items[index].UID)
				}
			}
			if len(applies) != 1 || applies[0] != run.job.UID || (!row.moved && len(jobs.Items) != 1) {
				t.Fatalf("the namespace holds %d Jobs (Apply Jobs %v), want only the one the claim could not confirm, %q",
					len(jobs.Items), applies, run.job.UID)
			}
			// The Job is still running, so the database is not handed back.
			assertDatabaseStillHeld(t, reconciler, api)
		})
	}
}
