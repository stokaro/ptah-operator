package controller

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// One obligation, two families, and an order rather than a state.
//
// The claim is the only stored thing that names the epoch an operation took
// its Lease with. A pass that persists the claim's removal and releases
// afterwards leaves a window in which the database is held by an epoch nothing
// names: the manager can stop there, and every other claimant on that database
// then waits out the whole lease duration -- sixteen minutes by default -- for
// a run that has already finished.
//
// Coverage cannot see this and neither can a status assertion. The state each
// pass settles on is correct either way, because the release does happen, one
// statement later and only in memory. The window is between two writes, so the
// writes are what these rows read.

// A migration Apply retired as uncertain is in the table too, with both of its
// answers. It hands the database back only where a live read proves the
// dispatched Job can no longer write, and that read answers "may still be
// writing" to anything it cannot settle, a transient error included. The read
// is taken before the write, so the obligation is recorded where the answer
// was "stopped" and nowhere else: recording it ahead of the answer would write
// down a release that must not happen, and the next pass would perform it.

// retiredClaim is what one status write said about the claim and about the
// obligation that has to outlive it.
type retiredClaim struct {
	claimed bool
	owed    bool
}

// claimWriteRecorder reads every status write on its way to the API server.
type claimWriteRecorder struct {
	client.Client
	writes *[]retiredClaim
}

func (c *claimWriteRecorder) Status() client.SubResourceWriter {
	return &claimWriteWriter{SubResourceWriter: c.Client.Status(), writes: c.writes}
}

type claimWriteWriter struct {
	client.SubResourceWriter
	writes *[]retiredClaim
}

func (w *claimWriteWriter) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.SubResourcePatchOption,
) error {
	switch resource := object.(type) {
	case *operatorv1alpha1.PtahMigration:
		*w.writes = append(*w.writes, retiredClaim{
			claimed: resource.Status.ActiveOperation != nil,
			owed:    resource.Status.PendingLockRelease != nil,
		})
	case *operatorv1alpha1.PtahSchema:
		*w.writes = append(*w.writes, retiredClaim{
			claimed: resource.Status.ActiveOperation != nil,
			owed:    resource.Status.PendingLockRelease != nil,
		})
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}

// firstRetirement returns the write that gave the claim up, and whether there
// was one at all. A row whose fixture never reaches it measured nothing.
func firstRetirement(writes []retiredClaim) (retiredClaim, bool) {
	for _, write := range writes {
		if !write.claimed {
			return write, true
		}
	}
	return retiredClaim{}, false
}

// claimRetirement is one family's way into the question. Each returns the
// status writes its retirement produced, in order.
//
// The plumbing is family-local because the fixtures and the reconcilers are.
// The contract is not: every row below is asserted the same way, so a family
// that retires a claim its own way still has to hand the database back in the
// same write.
type claimRetirement struct {
	family string
	name   string
	// leased says whether the claim took a Lease. A claim that took none owes
	// nothing, and these rows are the control: an implementation that recorded
	// an obligation unconditionally would name an empty epoch, which is a
	// release no later pass can perform.
	leased bool
	// retained says a live executor still needs its claim and realm. There is
	// no retirement write until the workload stops.
	retained bool
	// owes is what the retirement must record. It is not always `leased`: a
	// claim can hold a Lease it does not owe back, because an outstanding
	// post-Apply proof inherited the epoch and still needs the realm.
	owes   bool
	retire func(t *testing.T, leased bool) []retiredClaim
}

func migrationClaimRetirements() []claimRetirement {
	retire := func(
		drive func(*MigrationReconciler, *operatorv1alpha1.PtahMigration) error,
	) func(*testing.T, bool) []retiredClaim {
		return func(t *testing.T, leased bool) []retiredClaim {
			t.Helper()

			migration, plan := awaitingApprovalFixture(t)
			operation := applyClaimFor(t, migration, plan)
			if !leased {
				operation.LeaseEpoch = ""
				operation.CoordinationDigest = ""
			}
			reconciler, api := fakeMigrationReconciler(
				t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
			writes := &[]retiredClaim{}
			reconciler.Client = &claimWriteRecorder{Client: reconciler.Client, writes: writes}
			// Releasing succeeds here: a Lease that is not there is not a
			// failure to release, so nothing falls into the best-effort record
			// and the only way an obligation reaches the wire is a retirement
			// staging it.
			reconciler.Locks = targetlock.New(api, api, nil)

			if err := drive(reconciler, migration); err != nil {
				t.Fatal(err)
			}
			return *writes
		}
	}

	cannotDispatch := retire(func(r *MigrationReconciler, migration *operatorv1alpha1.PtahMigration) error {
		_, err := r.migrationOperationFailure(
			context.Background(), migration, errors.New("the Job could not be created"))
		return err
	})
	wentStale := retire(func(r *MigrationReconciler, migration *operatorv1alpha1.PtahMigration) error {
		_, err := r.discardMigrationOperation(
			context.Background(), migration, telemetry.OperationStale, errors.New("the dispatch deadline passed"))
		return err
	})

	runFinished := retire(func(r *MigrationReconciler, migration *operatorv1alpha1.PtahMigration) error {
		operation := migration.Status.ActiveOperation
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Namespace: migration.Namespace, Name: operation.JobName, UID: "apply-job-uid",
			// Already carrying a deadline, so harvesting it writes nothing and
			// the only writes recorded below are the resource's own.
			ResourceVersion: "1",
		}}
		job.Spec.TTLSecondsAfterFinished = ptr(int32(jobCleanupTTLSeconds))
		_, err := r.consumeMigrationRun(context.Background(), migration, job, runner.Result{
			Operation:            runner.OperationMigrationApply,
			CoordinationDigest:   operation.CoordinationDigest,
			TargetIdentityDigest: testDigest,
			MigrationRun: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract,
				Direction:       "up", Outcome: dataplane.MigrationOutcomeApplied,
				Planned: []int64{3}, Applied: []int64{3},
			},
		})
		return err
	})

	// The claim recorded no UID and nothing stands under the name it
	// reserved, so nothing it dispatched can still write.
	unaccountedStopped := retire(func(r *MigrationReconciler, migration *operatorv1alpha1.PtahMigration) error {
		_, err := r.finishUncertainMigrationApply(context.Background(), migration, nil,
			errors.New("the Apply Job create result is uncertain"), "")
		return err
	})
	// The Job the claim recorded is still running.
	unaccountedRunning := retire(func(r *MigrationReconciler, migration *operatorv1alpha1.PtahMigration) error {
		operation := migration.Status.ActiveOperation
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: migration.Namespace, Name: operation.JobName}}
		if err := r.Client.Create(context.Background(), job); err != nil {
			return err
		}
		operation.JobUID = job.UID
		_, err := r.finishUncertainMigrationApply(context.Background(), migration, nil,
			errors.New("the database lock epoch changed under the dispatched run"), "")
		return err
	})
	// The binding changes under a claim that never dispatched.
	bindingChanged := retire(func(r *MigrationReconciler, migration *operatorv1alpha1.PtahMigration) error {
		migration.Status.ActiveOperation.DispatchStarted = false
		r.Jobs = executionBindingJobs{
			ptahVersion:   migration.Status.ExecutionBinding.PtahVersion,
			executorImage: "example.invalid/ptah@" + safetyOtherDigest,
			protocol:      migration.Status.ExecutionBinding.RunnerProtocolVersion,
		}
		_, _, err := r.reconcileMigrationExecutionBinding(context.Background(), migration)
		return err
	})
	// A deleting resource whose Apply never dispatched, and one whose
	// dispatched Apply has stopped.
	deleted := func(dispatched bool) func(*testing.T, bool) []retiredClaim {
		return retire(func(r *MigrationReconciler, migration *operatorv1alpha1.PtahMigration) error {
			migration.Status.ActiveOperation.DispatchStarted = dispatched
			_, err := r.reconcileMigrationDeletion(context.Background(), migration)
			return err
		})
	}

	return []claimRetirement{
		{family: "PtahMigration", name: "a run that finished", leased: true, owes: true, retire: runFinished},
		{family: "PtahMigration", name: "a run that finished, holding no Lease", leased: false, owes: false, retire: runFinished},
		{family: "PtahMigration", name: "a claim that cannot dispatch", leased: true, owes: true, retire: cannotDispatch},
		{family: "PtahMigration", name: "a claim that cannot dispatch, holding no Lease", leased: false, owes: false, retire: cannotDispatch},
		{family: "PtahMigration", name: "a claim whose dispatch deadline passed", leased: true, owes: true, retire: wentStale},
		{family: "PtahMigration", name: "a run nobody accounted for, that nothing can still write for", leased: true, owes: true, retire: unaccountedStopped},
		{family: "PtahMigration", name: "a run nobody accounted for, whose Job is still running", leased: true, retained: true, owes: false, retire: unaccountedRunning},
		{family: "PtahMigration", name: "an undispatched claim under a changed execution binding", leased: true, owes: true, retire: bindingChanged},
		{family: "PtahMigration", name: "an undispatched claim on a deleting resource", leased: true, owes: true, retire: deleted(false)},
		{family: "PtahMigration", name: "a stopped run on a deleting resource", leased: true, owes: true, retire: deleted(true)},
	}
}

// schemaRetirement arranges a schema whose claim is about to be retired and
// records the status writes the retirement produced.
//
// proof is what separates the two arms of the rule. A post-Apply observation
// carries the epoch the Apply took, so while one is outstanding the realm
// belongs to the proof and the claim retiring under it owes nothing, whatever
// it is holding.
func schemaRetirement(
	operation operatorv1alpha1.OperationType,
	proof bool,
	drive func(*SchemaReconciler, *operatorv1alpha1.PtahSchema) error,
) func(*testing.T, bool) []retiredClaim {
	return func(t *testing.T, leased bool) []retiredClaim {
		t.Helper()

		schema := safetyLockedOperationSchema(operation)
		if !leased {
			schema.Status.ActiveOperation.LeaseEpoch = ""
			schema.Status.ActiveOperation.CoordinationDigest = ""
		}
		if proof {
			schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
				Outcome:              operatorv1alpha1.PendingObservationApplySucceeded,
				ApplyOperationID:     "the-apply",
				ApplyGeneration:      schema.Generation,
				CoordinationDigest:   schema.Status.ActiveOperation.CoordinationDigest,
				LeaseEpoch:           schema.Status.ActiveOperation.LeaseEpoch,
				LeaseDurationSeconds: schema.Status.ActiveOperation.LeaseDurationSeconds,
			}
		}
		reconciler, api := fakeReconciler(t, staticLogs{}, schema)
		writes := &[]retiredClaim{}
		reconciler.Client = &claimWriteRecorder{Client: reconciler.Client, writes: writes}
		reconciler.Locks = targetlock.New(api, api, nil)

		// Read the stored object back: the fixture pointer carries the
		// resource version it had before the fake client accepted it, and the
		// status patch below takes the optimistic lock.
		stored := &operatorv1alpha1.PtahSchema{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), stored); err != nil {
			t.Fatal(err)
		}
		if err := drive(reconciler, stored); err != nil {
			t.Fatal(err)
		}
		return *writes
	}
}

// schemaApplyHarvest runs one pass over an Apply whose Job has finished, and
// records the status writes it made. The claim holds the realm under the epoch
// the fixture seeded; twoPods makes the run one nobody can account for, since
// one result frame cannot speak for a second executor.
func schemaApplyHarvest(twoPods bool) func(*testing.T, bool) []retiredClaim {
	return func(t *testing.T, _ bool) []retiredClaim {
		t.Helper()

		schema, plan, policyConfig := dispatchedApplyWithPlan(t)
		job, pod := terminalWorkload(schema, batchv1.JobComplete)
		objects := []client.Object{schema, plan, policyConfig, job, pod}
		if twoPods {
			objects = append(objects, secondExecutorPod(job, pod))
		}
		frame := safetyRunnerFrame(t, runner.Result{
			ProtocolVersion:      runner.ProtocolVersion,
			Operation:            runner.OperationApply,
			OperationID:          schema.Status.ActiveOperation.ID,
			MutationStarted:      true,
			CoordinationDigest:   schema.Status.Plan.CoordinationDigest,
			TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest,
		})
		reconciler, _ := fakeReconciler(t, staticLogs{content: frame}, objects...)
		writes := &[]retiredClaim{}
		reconciler.Client = &claimWriteRecorder{Client: reconciler.Client, writes: writes}
		if _, err := reconciler.Reconcile(context.Background(),
			ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
			t.Fatal(err)
		}
		return *writes
	}
}

// schemaProofSettled runs one pass over the post-Apply Plan that finds the
// managed scope converged, which settles the proof the Apply owed.
func schemaProofSettled(t *testing.T, _ bool) []retiredClaim {
	t.Helper()

	schema, objects := convergedProofPlan(t, operatorv1alpha1.PendingObservationApplySucceeded)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationPlan,
		OperationID:          schema.Status.ActiveOperation.ID,
		CoordinationDigest:   schema.Status.PendingObservation.CoordinationDigest,
		TargetIdentityDigest: schema.Status.PendingObservation.Plan.TargetIdentityDigest,
		PlanOutcome:          runner.PlanOutcomeNoChanges,
	})
	reconciler, _ := fakeReconciler(t, staticLogs{content: frame}, objects...)
	writes := &[]retiredClaim{}
	reconciler.Client = &claimWriteRecorder{Client: reconciler.Client, writes: writes}
	if _, err := reconciler.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatal(err)
	}
	return *writes
}

func schemaClaimRetirements() []claimRetirement {
	policyChanged := schemaRetirement(operatorv1alpha1.OperationApply, false,
		func(r *SchemaReconciler, schema *operatorv1alpha1.PtahSchema) error {
			_, err := r.verificationPolicyChanged(
				context.Background(), schema, errors.New("the verification policy bytes changed"))
			return err
		})
	staleInputs := func(proof bool) func(*testing.T, bool) []retiredClaim {
		return schemaRetirement(operatorv1alpha1.OperationPlan, proof,
			func(r *SchemaReconciler, schema *operatorv1alpha1.PtahSchema) error {
				_, err := r.discardStaleOperation(
					context.Background(), schema, errors.New("the desired inputs changed"))
				return err
			})
	}
	approvalWithdrawn := schemaRetirement(operatorv1alpha1.OperationApply, false,
		func(r *SchemaReconciler, schema *operatorv1alpha1.PtahSchema) error {
			_, err := r.approvalBecameInvalid(context.Background(), schema)
			return err
		})
	suspended := func(proof bool) func(*testing.T, bool) []retiredClaim {
		return schemaRetirement(operatorv1alpha1.OperationPlan, proof,
			func(r *SchemaReconciler, schema *operatorv1alpha1.PtahSchema) error {
				_, err := r.suspendActiveOperation(context.Background(), schema)
				return err
			})
	}

	return []claimRetirement{
		{family: "PtahSchema", name: "a Plan suspended before its Job ran", leased: true, owes: true, retire: suspended(false)},
		{family: "PtahSchema", name: "a Plan suspended before its Job ran, holding no Lease", leased: false, owes: false, retire: suspended(false)},
		{family: "PtahSchema", name: "a Plan carrying out a proof, suspended", leased: true, owes: false, retire: suspended(true)},
		// A mutating run's own retirement. The pending observation it writes
		// inherits the epoch, so the realm stays held through the reading
		// that accounts for the run, whether the run's result was read or not.
		{family: "PtahSchema", name: "an Apply whose result was read", leased: true, owes: false, retire: schemaApplyHarvest(false)},
		{family: "PtahSchema", name: "an Apply nobody accounted for", leased: true, owes: false, retire: schemaApplyHarvest(true)},
		// The write that settles a proof owes the proof's epoch back, and it
		// is the only write that does.
		{family: "PtahSchema", name: "a Plan that settles the proof it was carrying out", leased: true, owes: true, retire: schemaProofSettled},
		{family: "PtahSchema", name: "a verification policy that changed under the claim", leased: true, owes: true, retire: policyChanged},
		{family: "PtahSchema", name: "the same, holding no Lease", leased: false, owes: false, retire: policyChanged},
		{family: "PtahSchema", name: "inputs that changed under a Plan", leased: true, owes: true, retire: staleInputs(false)},
		{family: "PtahSchema", name: "inputs that changed under a Plan holding no Lease", leased: false, owes: false, retire: staleInputs(false)},
		// The other arm of the rule, and the only row here where a claim holds
		// a Lease and still owes nothing: the post-Apply proof inherited that
		// epoch and the realm is its until the proof is discharged.
		{family: "PtahSchema", name: "inputs that changed under a Plan owing proof", leased: true, owes: false, retire: staleInputs(true)},
		{family: "PtahSchema", name: "an approval withdrawn under an Apply", leased: true, owes: true, retire: approvalWithdrawn},
		{family: "PtahSchema", name: "an approval withdrawn under an Apply holding no Lease", leased: false, owes: false, retire: approvalWithdrawn},
	}
}

// TestRetiringAClaimRecordsTheReleaseItOwes holds both families to one order:
// the write that drops a claim carries the release that claim owes, and the
// release itself happens after.
func TestRetiringAClaimRecordsTheReleaseItOwes(t *testing.T) {
	t.Parallel()

	rows := append(migrationClaimRetirements(), schemaClaimRetirements()...)
	for _, row := range rows {
		t.Run(row.family+"/"+row.name, func(t *testing.T) {
			t.Parallel()

			writes := row.retire(t, row.leased)

			retirement, reached := firstRetirement(writes)
			if row.retained {
				if reached || len(writes) == 0 {
					t.Fatalf("the live executor lost its claim or produced no recorded status write: %#v", writes)
				}
				for _, write := range writes {
					if write.owed {
						t.Fatalf("the live executor recorded a premature realm release: %#v", writes)
					}
				}
				return
			}
			if !reached {
				t.Fatalf("the fixture never gave the claim up, so nothing here was measured: %#v", writes)
			}
			if retirement.owed != row.owes {
				t.Fatalf("the write that dropped the claim recorded owed=%t, want %t; writes: %#v",
					retirement.owed, row.owes, writes)
			}
		})
	}
}
