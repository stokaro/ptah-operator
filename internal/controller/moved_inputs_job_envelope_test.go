package controller

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// A claim whose inputs moved after its Job was dispatched -- a spec edit, as
// here, or a plan or policy that changed -- cannot rebuild that Job, so no
// pass can hold the Job under its reserved name to a rebuild. Both families
// used to take such a Job on its UID and owner alone and record its UID. They
// now hold it to what the claim fixes without a rebuild, by jobclaim.Match:
// its epoch, the labels and annotations the claim fixes, and its Pod template
// against the admission snapshot. That is the match the controller-write
// webhook applies when it admits the Job's cleanup TTL.
//
// Every Job here comes from the real builder, for the reason
// migration_job_adoption_test.go gives: the fixture builder writes no labels,
// so a Job it built carries no claim to tell apart from another's.

// underAnotherClaim is a Job whose labels and annotations name another claim's
// operation, on the Job and its Pod template alike, as a Job built for that
// claim would.
func underAnotherClaim(job *batchv1.Job) {
	const another = "another-operation"
	for _, labels := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
		labels[workload.LabelOperationID] = workload.OperationIDLabelValue(another)
	}
	for _, annotations := range []map[string]string{job.Annotations, job.Spec.Template.Annotations} {
		annotations[workload.AnnotationOperationID] = another
	}
}

// refusedAfterTheInputsMoved is every Job a claim whose inputs moved must
// refuse, with the reason jobclaim.Match gives for it.
func refusedAfterTheInputsMoved() []struct {
	name  string
	alter func(*batchv1.Job)
	want  string
} {
	return []struct {
		name  string
		alter func(*batchv1.Job)
		want  string
	}{
		{name: "carrying another claim's labels", alter: underAnotherClaim, want: "the Job's labels are not its claim's"},
		{name: "under another epoch", alter: underAnotherEpoch, want: "the Job runs under another execution epoch than its claim"},
	}
}

// ownJobsAfterTheInputsMoved is every Job a claim whose inputs moved still
// takes as its own. The last is the limit of a match without a rebuild: the
// claim cannot see that another release built a different Pod template, only
// that the template is the one its admission snapshot recorded at dispatch.
// Such a Job is never harvested either; the terminal pass reads the moved
// inputs and settles the claim without reading its result.
func ownJobsAfterTheInputsMoved(current workload.Builder) []struct {
	name       string
	dispatcher workload.Builder
	alter      func(*batchv1.Job)
} {
	predecessor := managerOnlyPredecessor(current)
	return []struct {
		name       string
		dispatcher workload.Builder
		alter      func(*batchv1.Job)
	}{
		{name: "built by this manager", dispatcher: current},
		{name: "built by a release that differs in the recorded manager alone", dispatcher: predecessor},
		{name: "built by a release that also changed the Pod template", dispatcher: predecessor, alter: withAnExecutorSetting},
	}
}

// moveMigrationInputs edits the spec under a dispatched run the way a person
// does, with the generation the API server stamps on the edit.
func moveMigrationInputs(migration *operatorv1alpha1.PtahMigration) {
	migration.Spec.Interval = metav1.Duration{Duration: 2 * time.Hour}
	migration.Generation++
}

// requireMigrationInputsMoved fails unless the claim's inputs no longer hold.
// While they hold, the pass rebuilds the Job, and a row would measure the
// rebuild rather than what the claim fixes without one.
func requireMigrationInputsMoved(t *testing.T, reconciler *MigrationReconciler, migration *operatorv1alpha1.PtahMigration) {
	t.Helper()

	operation := migration.Status.ActiveOperation
	current, err := reconciler.migrationInputFingerprint(context.Background(), migration, operation.Type)
	if err == nil && current == operation.InputFingerprint {
		t.Fatal("the claim's inputs still hold, so the pass rebuilds its Job and the row proves nothing without a rebuild")
	}
}

// TestAMigrationWhoseInputsMovedRefusesAJobThatIsNotItsClaims holds the Job
// under a claim's reserved name to the claim once the claim can no longer
// rebuild it. An Apply records the run as outcome unknown and a History
// reading moves to a new attempt under a new name; neither claim records the
// UID of the Job it refused.
func TestAMigrationWhoseInputsMovedRefusesAJobThatIsNotItsClaims(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, claim := range claimsAndVerdicts() {
		for _, row := range refusedAfterTheInputsMoved() {
			t.Run(claim.name+"/"+row.name, func(t *testing.T) {
				t.Parallel()

				run := runDispatchedBy(t, claim.operation, runShape{
					dispatcher: current, alter: row.alter, recorded: claim.recorded,
				})
				moveMigrationInputs(run.migration)
				reconciler, api := run.reconciler(t, current)
				requireMigrationInputsMoved(t, reconciler, run.migration)

				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(run.migration)); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				assertRefused(t, api, run, "Job is not its claim's: "+row.want)
			})
		}
	}
}

// TestAMigrationWhoseInputsMovedStillAdoptsItsOwnJob is the control for the
// refusals above: a Job the claim built is adopted, or kept, exactly as it
// was before the claim held it to anything once its inputs moved.
func TestAMigrationWhoseInputsMovedStillAdoptsItsOwnJob(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, claim := range claimsAndVerdicts() {
		for _, row := range ownJobsAfterTheInputsMoved(current) {
			t.Run(claim.name+"/"+row.name, func(t *testing.T) {
				t.Parallel()

				run := runDispatchedBy(t, claim.operation, runShape{
					dispatcher: row.dispatcher, alter: row.alter, recorded: claim.recorded,
				})
				moveMigrationInputs(run.migration)
				reconciler, api := run.reconciler(t, current)
				requireMigrationInputsMoved(t, reconciler, run.migration)

				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(run.migration)); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				assertAdopted(t, api, run)
			})
		}
	}
}

// dispatchedSchemaRun is what a stopped pass leaves behind for a schema: an
// active claim and the running Job a manager built and created for it, in the
// API server the reconciler reads.
type dispatchedSchemaRun struct {
	schema     *operatorv1alpha1.PtahSchema
	job        *batchv1.Job
	reconciler *SchemaReconciler
	api        client.Client
}

// schemaRunDispatchedBy stands up a dispatched run of operationType. The steps
// a real dispatch takes before its create are the controller's own: the claim,
// the plan an Apply runs as the store publishes it, and the realm Lease an
// Apply holds. The Job is built by shape.dispatcher under the name the claim
// reserved, and the reconciler builds with current.
func schemaRunDispatchedBy(
	t *testing.T,
	operationType operatorv1alpha1.OperationType,
	shape runShape,
	current JobBuilder,
) dispatchedSchemaRun {
	t.Helper()

	ctx := context.Background()
	var reconciler *SchemaReconciler
	var api client.Client
	var schema *operatorv1alpha1.PtahSchema
	switch operationType {
	case operatorv1alpha1.OperationApply:
		reconciler, api, schema, _ = dispatchableApply(t, false)
	case operatorv1alpha1.OperationResolve:
		schema = schemaFixture()
		reconciler, api = fakeReconciler(t, staticLogs{}, schema)
		if _, err := reconciler.claim(ctx, safetyGetSchema(t, api, schema), operationType); err != nil {
			t.Fatalf("claim(%s) error = %v", operationType, err)
		}
	default:
		t.Fatalf("no dispatched-run fixture for a %s claim", operationType)
	}
	// The first acquisition records the epoch the API server gave the Lease
	// and reports it not yet held; the next one holds it, as the dispatch
	// driver's next pass would.
	for attempt := 0; schemaClaimHoldsLock(safetyGetSchema(t, api, schema)); attempt++ {
		acquired, _, err := reconciler.acquireOperationLock(ctx, safetyGetSchema(t, api, schema))
		if err != nil || attempt > 1 {
			t.Fatalf("take the realm the %s claim holds: acquired=%t err=%v", operationType, acquired, err)
		}
		if acquired {
			break
		}
	}
	claimed := safetyGetSchema(t, api, schema)
	operation := claimed.Status.ActiveOperation
	if operation == nil || operation.Type != operationType {
		t.Fatalf("the %s claim was not made: %#v", operationType, claimed.Status)
	}
	// The fixture builder the claim was made with names a Job by its type
	// alone; a real claim reserves the name the real builder derives.
	name, err := workload.NameFor(claimed, *operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.JobName = name
	var plan *operatorv1alpha1.PtahSchemaPlan
	if schemaOperation(operation).Mutating {
		// The builder holds an Apply's source to the reference the schema
		// requested, which a Resolve records and the fixture does not.
		claimed.Status.Source.RequestedReference = claimed.Spec.Desired.OCIRef
		if plan, err = reconciler.currentPlan(ctx, claimed); err != nil {
			t.Fatalf("read the plan the Apply runs: %v", err)
		}
	}

	build := func() *batchv1.Job {
		t.Helper()
		job, err := shape.dispatcher.Build(claimed, *operation, plan)
		if err != nil {
			t.Fatalf("build the dispatched %s Job: %v", operationType, err)
		}
		if shape.alter != nil {
			shape.alter(job)
		}
		return job
	}
	operation.AdmissionSnapshot = testAdmissionSnapshotOf(build().Spec.Template.DeepCopy())
	job := build()
	job.UID = types.UID("dispatched-job-uid")
	job.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{
		batchv1.ControllerUidLabel: string(job.UID),
	}}
	job.Spec.Template.Labels[batchv1.ControllerUidLabel] = string(job.UID)
	job.Spec.Template.Labels[batchv1.JobNameLabel] = job.Name
	job.Status.Active = 1

	// The dispatch marker precedes the create, so a claim that lost the
	// create's answer carries it without the UID.
	operation.DispatchStarted = true
	if shape.recorded {
		operation.JobUID = job.UID
	}
	if err := api.Status().Update(ctx, claimed); err != nil {
		t.Fatalf("record the dispatched claim: %v", err)
	}
	if err := api.Create(ctx, job); err != nil {
		t.Fatalf("create the dispatched Job: %v", err)
	}
	reconciler.Jobs = current
	return dispatchedSchemaRun{schema: safetyGetSchema(t, api, schema), job: job, reconciler: reconciler, api: api}
}

// moveSchemaInputs edits the spec under a dispatched run the way a person
// does, with the generation the API server stamps on the edit, and returns
// the schema as stored after it.
func moveSchemaInputs(t *testing.T, run dispatchedSchemaRun) *operatorv1alpha1.PtahSchema {
	t.Helper()

	stored := safetyGetSchema(t, run.api, run.schema)
	stored.Spec.Interval = metav1.Duration{Duration: 2 * time.Hour}
	stored.Generation++
	if err := run.api.Update(context.Background(), stored); err != nil {
		t.Fatalf("edit the schema under its dispatched run: %v", err)
	}
	return safetyGetSchema(t, run.api, run.schema)
}

// requireSchemaInputsMoved is requireMigrationInputsMoved for a schema claim.
func requireSchemaInputsMoved(t *testing.T, reconciler *SchemaReconciler, schema *operatorv1alpha1.PtahSchema) {
	t.Helper()

	operation := schema.Status.ActiveOperation
	current, err := reconciler.operationInputFingerprint(schema, operation.Type)
	if err == nil && current == operation.InputFingerprint {
		t.Fatal("the claim's inputs still hold, so the pass rebuilds its Job and the row proves nothing without a rebuild")
	}
}

// assertSchemaAdopted fails unless the pass kept the claim and bound it to the
// run's Job.
func assertSchemaAdopted(t *testing.T, run dispatchedSchemaRun) {
	t.Helper()

	claimed := run.schema.Status.ActiveOperation
	actual := safetyGetSchema(t, run.api, run.schema)
	operation := actual.Status.ActiveOperation
	if operation == nil || operation.ID != claimed.ID || operation.Attempt != claimed.Attempt {
		t.Fatalf("the claim did not survive adopting its own Job: operation=%#v pending=%#v conditions=%#v",
			operation, actual.Status.PendingObservation, actual.Status.Conditions)
	}
	if operation.JobUID != run.job.UID {
		t.Fatalf("claim records Job UID %q, want the adopted Job's %q", operation.JobUID, run.job.UID)
	}
	if actual.Status.PendingObservation != nil {
		t.Fatalf("adopting a running Job settled the claim: %#v", actual.Status.PendingObservation)
	}
}

// assertSchemaRefused fails unless the pass settled the claim the way its
// operation settles a Job it cannot confirm, for the reason want names: an
// Apply is recorded as outcome unknown, naming no Job the claim had not
// already recorded, and a read-only claim moves to a new attempt under a new
// name, leaving the Job as it found it.
func assertSchemaRefused(t *testing.T, run dispatchedSchemaRun, want string) {
	t.Helper()

	claimed := run.schema.Status.ActiveOperation
	actual := safetyGetSchema(t, run.api, run.schema)
	failed := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if failed == nil || failed.Status != metav1.ConditionTrue || !strings.Contains(failed.Message, want) {
		t.Fatalf("ReconciliationFailed = %#v, want it to say %q", failed, want)
	}
	if schemaOperation(claimed).Mutating {
		if actual.Status.ActiveOperation != nil {
			t.Fatalf("an Apply claim kept a Job it cannot confirm: operation=%#v", actual.Status.ActiveOperation)
		}
		pending := actual.Status.PendingObservation
		if pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown ||
			pending.ApplyOperationID != claimed.ID {
			t.Fatalf("pending observation = %#v, want the claim's run recorded with an unknown outcome", pending)
		}
		if pending.ApplyJobUID != claimed.JobUID {
			t.Fatalf("the unknown run records Job UID %q, want only what the claim had recorded, %q",
				pending.ApplyJobUID, claimed.JobUID)
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
	left := &batchv1.Job{}
	if err := run.api.Get(context.Background(), client.ObjectKeyFromObject(run.job), left); err != nil {
		t.Fatalf("read the Job the claim left: %v", err)
	}
	if left.UID != run.job.UID || left.Spec.TTLSecondsAfterFinished != nil {
		t.Fatalf("the Job the claim could not confirm was changed: UID %q, TTL %v", left.UID, left.Spec.TTLSecondsAfterFinished)
	}
}

// schemaClaimsAndVerdicts is every way a schema claim meets its Job: an Apply
// and a read-only Resolve, each adopting a Job whose UID it never recorded and
// supervising one whose UID it did.
func schemaClaimsAndVerdicts() []struct {
	name      string
	operation operatorv1alpha1.OperationType
	recorded  bool
} {
	return []struct {
		name      string
		operation operatorv1alpha1.OperationType
		recorded  bool
	}{
		{name: "an Apply adopting", operation: operatorv1alpha1.OperationApply},
		{name: "an Apply supervising", operation: operatorv1alpha1.OperationApply, recorded: true},
		{name: "a Resolve adopting", operation: operatorv1alpha1.OperationResolve},
		{name: "a Resolve supervising", operation: operatorv1alpha1.OperationResolve, recorded: true},
	}
}

// TestASchemaWhoseInputsMovedRefusesAJobThatIsNotItsClaims is the schema
// family's row for what the migration row above proves.
func TestASchemaWhoseInputsMovedRefusesAJobThatIsNotItsClaims(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, claim := range schemaClaimsAndVerdicts() {
		for _, row := range refusedAfterTheInputsMoved() {
			t.Run(claim.name+"/"+row.name, func(t *testing.T) {
				t.Parallel()

				run := schemaRunDispatchedBy(t, claim.operation, runShape{
					dispatcher: current, alter: row.alter, recorded: claim.recorded,
				}, current)
				requireSchemaInputsMoved(t, run.reconciler, moveSchemaInputs(t, run))

				request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run.schema)}
				if _, err := run.reconciler.Reconcile(context.Background(), request); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				assertSchemaRefused(t, run, "Job is not its claim's: "+row.want)
			})
		}
	}
}

// countingApplyCreates counts the Apply Jobs a reconciler creates.
type countingApplyCreates struct {
	client.Client
	applies *atomic.Int64
}

func (c countingApplyCreates) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if job, ok := object.(*batchv1.Job); ok && job.Labels[workload.LabelOperation] == "apply" {
		c.applies.Add(1)
	}
	return c.Client.Create(ctx, object, options...)
}

// TestAnUnconfirmableSchemaApplyWhoseInputsMovedIsNeverDispatchedAgain is the
// schema family's row for the safety TestAnUnconfirmableMigrationApplyIsNeverDispatchedAgain
// holds: an Apply that refuses the Job under its name records the run as
// outcome unknown, keeps the database held, and no pass creates another Apply
// Job beside the one that may be running SQL.
func TestAnUnconfirmableSchemaApplyWhoseInputsMovedIsNeverDispatchedAgain(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, recorded := range []bool{false, true} {
		name := "adopting"
		if recorded {
			name = "supervising"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			run := schemaRunDispatchedBy(t, operatorv1alpha1.OperationApply, runShape{
				dispatcher: current, alter: underAnotherEpoch, recorded: recorded,
			}, current)
			requireSchemaInputsMoved(t, run.reconciler, moveSchemaInputs(t, run))
			var applies atomic.Int64
			run.reconciler.Client = countingApplyCreates{Client: run.reconciler.Client, applies: &applies}

			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run.schema)}
			for pass := range 4 {
				if _, err := run.reconciler.Reconcile(context.Background(), request); err != nil {
					t.Fatalf("Reconcile() pass %d error = %v", pass, err)
				}
				if pass == 0 {
					assertSchemaRefused(t, run, "dispatched Apply Job is not its claim's")
				}
			}
			pending := safetyGetSchema(t, run.api, run.schema).Status.PendingObservation
			if pending == nil || pending.ApplyOperationID != run.schema.Status.ActiveOperation.ID ||
				pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
				t.Fatalf("pending observation = %#v, want the refused Apply's run to stand as outcome unknown", pending)
			}
			if created := applies.Load(); created != 0 {
				t.Fatalf("%d Apply Job creates were attempted after the claim could not confirm its Job, want none", created)
			}
			jobs := &batchv1.JobList{}
			if err := run.api.List(context.Background(), jobs, client.InNamespace(run.schema.Namespace)); err != nil {
				t.Fatal(err)
			}
			var standing []types.UID
			for index := range jobs.Items {
				if jobs.Items[index].Labels[workload.LabelOperation] == "apply" {
					standing = append(standing, jobs.Items[index].UID)
				}
			}
			if len(standing) != 1 || standing[0] != run.job.UID {
				t.Fatalf("Apply Jobs %v stand in the namespace, want only the one the claim could not confirm, %q",
					standing, run.job.UID)
			}
			// The Job may still be running, so the database is not handed back.
			leaseName, err := targetlock.LeaseName(run.schema.Status.ActiveOperation.CoordinationDigest)
			if err != nil {
				t.Fatal(err)
			}
			lease := &coordinationv1.Lease{}
			key := client.ObjectKey{Namespace: run.reconciler.LockNamespace, Name: leaseName}
			if err := run.api.Get(context.Background(), key, lease); err != nil {
				t.Fatalf("read the database Lease: %v", err)
			}
			if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
				t.Fatal("the database Lease was handed back while the refused Apply Job may still write")
			}
		})
	}
}

// TestASchemaWhoseInputsMovedStillAdoptsItsOwnJob is the control for the
// schema refusals.
func TestASchemaWhoseInputsMovedStillAdoptsItsOwnJob(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, claim := range schemaClaimsAndVerdicts() {
		for _, row := range ownJobsAfterTheInputsMoved(current) {
			t.Run(claim.name+"/"+row.name, func(t *testing.T) {
				t.Parallel()

				run := schemaRunDispatchedBy(t, claim.operation, runShape{
					dispatcher: row.dispatcher, alter: row.alter, recorded: claim.recorded,
				}, current)
				requireSchemaInputsMoved(t, run.reconciler, moveSchemaInputs(t, run))

				request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run.schema)}
				if _, err := run.reconciler.Reconcile(context.Background(), request); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				assertSchemaAdopted(t, run)
			})
		}
	}
}
