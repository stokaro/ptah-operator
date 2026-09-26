package controller

import (
	"context"
	"maps"
	"reflect"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// dispatcherRecordOf is the record a Job built by manager carries.
func dispatcherRecordOf(manager executionBindingJobs) *operatorv1alpha1.ManagerRecord {
	return &operatorv1alpha1.ManagerRecord{
		ControllerImage:    manager.controllerImage,
		ControllerRevision: manager.controllerRevision,
		RunnerImage:        manager.runnerImage,
	}
}

// stampJob writes a manager's identity into a Job the way the builder does: on
// the Job, on its Pod template, and as the image of the container that
// installs the runner.
func stampJob(job *batchv1.Job, manager executionBindingJobs) {
	job.Annotations = maps.Clone(job.Annotations)
	job.Annotations[workload.AnnotationControllerImage] = manager.controllerImage
	job.Annotations[workload.AnnotationControllerRevision] = manager.controllerRevision
	job.Spec.Template.Annotations = maps.Clone(job.Spec.Template.Annotations)
	job.Spec.Template.Annotations[workload.AnnotationControllerImage] = manager.controllerImage
	job.Spec.Template.Annotations[workload.AnnotationControllerRevision] = manager.controllerRevision
	job.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "install-runner", Image: manager.runnerImage}}
}

// stampDispatcher stamps a harvested Job and the Pod it ran, which carries the
// template's annotations and containers, and takes the claim's admission
// snapshot again from the stamped template, as the dispatching manager did.
func stampDispatcher(
	t *testing.T,
	snapshot *operatorv1alpha1.PodAdmissionSnapshot,
	job *batchv1.Job,
	pod *corev1.Pod,
	manager executionBindingJobs,
) {
	t.Helper()
	stampJob(job, manager)
	templateDigest, err := podintent.DigestTemplate(&job.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.TemplateDigest = templateDigest
	snapshot.Digest = ""
	if snapshot.Digest, err = fingerprint.DigestCanonicalJSON(*snapshot); err != nil {
		t.Fatal(err)
	}
	for _, annotations := range []map[string]string{job.Annotations, job.Spec.Template.Annotations} {
		annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshot.Digest
	}
	pod.Annotations = maps.Clone(job.Spec.Template.Annotations)
	pod.Spec.InitContainers = []corev1.Container{{Name: "install-runner", Image: manager.runnerImage}}
}

// stampedJobs is the fake builder writing a manager's identity where the real
// one writes it, so the Job it rebuilds is the Job that manager dispatched.
type stampedJobs struct {
	fakeJobs
	manager executionBindingJobs
}

func (jobs stampedJobs) Build(
	schema *operatorv1alpha1.PtahSchema,
	operation operatorv1alpha1.ActiveOperationStatus,
	plan *operatorv1alpha1.PtahSchemaPlan,
) (*batchv1.Job, error) {
	job, err := jobs.fakeJobs.Build(schema, operation, plan)
	if err != nil {
		return nil, err
	}
	stampJob(job, jobs.manager)
	return job, nil
}

func (jobs stampedJobs) ManagerIdentity() (string, string, string) {
	return jobs.manager.controllerImage, jobs.manager.controllerRevision, jobs.manager.runnerImage
}

// TestTheApplyHarvestRecordsTheManagerThatDispatchedIt harvests an Apply Job a
// later manager dispatched for a plan an earlier one published. The Job has a
// cleanup TTL, so once it is collected the record on the schema is the only
// place that names its builder. It must name the dispatcher, and keep the
// publisher the plan names beside it.
func TestTheApplyHarvestRecordsTheManagerThatDispatchedIt(t *testing.T) {
	t.Parallel()

	schema, plan, policyConfig := dispatchedApplyWithPlan(t)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	stampDispatcher(t, schema.Status.ActiveOperation.AdmissionSnapshot, job, pod, nextManager())
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationApply,
		OperationID:          schema.Status.ActiveOperation.ID,
		ChildExitCode:        0,
		MutationStarted:      true,
		CoordinationDigest:   schema.Status.Plan.CoordinationDigest,
		TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest,
	})
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, plan, policyConfig, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)
	// The manager harvesting the Job is the one that dispatched it; the plan
	// is still the one its predecessor published.
	reconciler.Jobs = stampedJobs{manager: nextManager()}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	harvested := safetyGetSchema(t, api, schema)
	pending := harvested.Status.PendingObservation
	if pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationApplySucceeded {
		t.Fatalf("pending observation = %#v, want the succeeded Apply harvested from its Job; conditions %#v",
			pending, harvested.Status.Conditions)
	}
	want := dispatcherRecordOf(nextManager())
	if !reflect.DeepEqual(pending.DispatchedBy, want) {
		t.Fatalf("pending observation records dispatcher %#v, want %#v", pending.DispatchedBy, want)
	}
	if pending.Plan.ControllerImage != plan.Spec.ControllerImage || pending.Plan.ControllerImage == want.ControllerImage {
		t.Fatalf("pending plan records publisher %q, want the plan's %q", pending.Plan.ControllerImage, plan.Spec.ControllerImage)
	}
}

// TestAMigrationRunRecordsTheManagerThatDispatchedIt is the migration
// family's half. The record lands on status.lastRun and, for a run nobody can
// account for, on status.unresolvedRun, which is what a person reads to find
// out what ran. A Job whose template records no complete identity still
// settles, with no record.
func TestAMigrationRunRecordsTheManagerThatDispatchedIt(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name       string
		outcome    string
		stamp      bool
		unresolved bool
	}{
		{name: "an applied run", outcome: dataplane.MigrationOutcomeApplied, stamp: true},
		{name: "a partial run", outcome: dataplane.MigrationOutcomePartial, stamp: true, unresolved: true},
		{name: "a Job that records no manager", outcome: dataplane.MigrationOutcomeApplied},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, plan := awaitingApprovalFixture(t)
			operation := applyClaimFor(t, migration, plan)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			var want *operatorv1alpha1.ManagerRecord
			if row.stamp {
				stampDispatcher(t, migration.Status.ActiveOperation.AdmissionSnapshot, job, pod, nextManager())
				want = dispatcherRecordOf(nextManager())
				if plan.Spec.ControllerImage == want.ControllerImage {
					t.Fatal("the fixture's plan publisher is the dispatcher, so the record proves nothing")
				}
			}
			report := &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract,
				Direction:       "up",
				Outcome:         row.outcome,
				Planned:         []int64{3},
			}
			if row.outcome == dataplane.MigrationOutcomeApplied {
				report.Applied = []int64{3}
			}
			logs := migrationFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
				OperationID: operation.ID, ChildExitCode: 0,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
				MigrationRun:         report,
			})
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{content: logs}, migration, plan, job, pod, verificationPolicyConfigMap(),
			)
			holdMigrationApplyLease(t, reconciler, api, migration)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.LastRun == nil {
				t.Fatalf("the run left no record: %#v", actual.Status)
			}
			if !reflect.DeepEqual(actual.Status.LastRun.DispatchedBy, want) {
				t.Fatalf("lastRun records dispatcher %#v, want %#v", actual.Status.LastRun.DispatchedBy, want)
			}
			if !row.unresolved {
				return
			}
			if actual.Status.UnresolvedRun == nil ||
				!reflect.DeepEqual(actual.Status.UnresolvedRun.DispatchedBy, want) {
				t.Fatalf("unresolvedRun = %#v, want it to name dispatcher %#v", actual.Status.UnresolvedRun, want)
			}
		})
	}
}
