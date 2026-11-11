package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/coordination"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/lifecyclejournal"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/ocireference"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/policy"
	"github.com/stokaro/ptah-operator/internal/resultconsumer"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/telemetry"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	migrationOperationFinalizer = "operator.ptah.run/migration-operation"
	defaultMigrationInterval    = 10 * time.Minute
	// migrationPolicyIndex finds the migrations a ConfigMap change concerns:
	// the verification policy of an OCI source, or a ConfigMap source itself.
	migrationPolicyIndex = "spec.artifact.configMaps"
)

// MigrationJobBuilder turns one already-persisted migration claim into a
// deterministic Job. It must never read Secret content.
type MigrationJobBuilder interface {
	NameForMigration(
		migration *operatorv1alpha1.PtahMigration,
		operation operatorv1alpha1.MigrationOperationStatus,
	) (string, error)
	BuildMigration(
		migration *operatorv1alpha1.PtahMigration,
		operation operatorv1alpha1.MigrationOperationStatus,
		plan *operatorv1alpha1.PtahMigrationPlan,
	) (*batchv1.Job, error)
	// ExecutionBinding is what this manager executes with: the components a
	// plan and an approval bind, and whose change starts a new epoch.
	ExecutionBinding() (
		controllerStateVersion int32,
		ptahVersion string,
		executorImage string,
		runnerProtocolVersion int32,
	)
	// ManagerIdentity is the manager's own release and the runner image built
	// beside it. What the manager publishes records it; nothing binds it.
	ManagerIdentity() (controllerImage, controllerRevision, runnerImage string)
}

// MigrationReconciler carries one PtahMigration through Resolve -> Verify ->
// History -> Apply, and reads the history back afterwards to confirm what the
// run did.
//
// Only the Apply mutates a database, and only what an approved plan named. The
// three readings before it are what that plan is computed from, so the
// evidence and the decision stay separable: a plan is published from a reading
// the resource persisted, and an Apply executes a plan somebody or some policy
// authorized.
type MigrationReconciler struct {
	client.Client
	APIReader client.Reader
	// CachedReader reads what the manager's watches hold. A ConfigMap there
	// carries no payload (see managercache), so it serves only a read that
	// needs an object's name, labels and owner and may be a little behind:
	// listing the source snapshots to retire after every resolution. Nil
	// reads through Client.
	CachedReader      client.Reader
	Scheme            *runtime.Scheme
	Recorder          record.EventRecorder
	Logs              PodLogReader
	Results           OperationResults
	ResultCredentials ResultCredentialIssuer
	enrollmentHints   *resultEnrollmentHints
	// ResultReadTimeout bounds the pod/log read of one terminal operation.
	// Zero means defaultResultReadTimeout, which is what the manager runs.
	ResultReadTimeout time.Duration
	// resultLogs is when this process first failed to read each result log
	// it is still waiting on; resultLogLossWindow is measured on it. It is made
	// on first use, by resultLogFailuresOf.
	resultLogs *resultLogFailures
	Jobs       MigrationJobBuilder
	Locks      *targetlock.Locker
	// LockNamespace is one shared coordination namespace for every managed
	// resource, including resources that live in different namespaces: two
	// namespaces that address the same database must not run at the same time.
	LockNamespace    string
	Clock            func() time.Time
	Telemetry        telemetry.Observer
	AdmissionOptions podintent.Options

	// dispatch takes a claim to its one permitted create. Its zero value runs
	// the order mutationlifecycle writes down; tests swap two of its steps to
	// show which boundary each of them holds.
	dispatch mutationlifecycle.Driver
	// missingJobs is when this process first found each marked Apply with no
	// Job; mutationlifecycle.MissingJobGrace is measured on it. It is made on
	// first use, by missingJobsOf.
	missingJobs *mutationlifecycle.MissingJobs
}

func (r *MigrationReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	logger := ctrl.LoggerFrom(ctx)
	logger.V(1).Info("migration reconciliation started")
	defer func() {
		if err != nil {
			logger.Error(err, "migration reconciliation failed")
		}
		if r.Telemetry == nil {
			return
		}
		if err != nil {
			r.Telemetry.ObserveReconciliation(telemetry.FamilyMigration, telemetry.ReconciliationFailed)
			r.Telemetry.ObserveFailure(telemetry.FamilyMigration, telemetry.FailureStageController, telemetry.FailureInfrastructure)
			return
		}
		r.Telemetry.ObserveReconciliation(telemetry.FamilyMigration, telemetry.ReconciliationSucceeded)
	}()
	return r.reconcile(ctx, request)
}

func (r *MigrationReconciler) reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	migration := &operatorv1alpha1.PtahMigration{}
	if err := r.directReader().Get(ctx, request.NamespacedName, migration); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if err := r.rejectUnsupportedStoredControllerState(migration); err != nil {
		return ctrl.Result{}, err
	}
	// A realm this resource still owes back is handed over before anything
	// else in the pass, including deletion. Every other claimant of that
	// database is waiting on it, and nothing below needs the Lease.
	if migration.Status.PendingLockRelease != nil {
		if err := r.completeMigrationPendingLockRelease(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
	}
	// An unknown result does not stop the executor. Keep the original claim
	// ahead of deletion, suspension and component rotation until its workload
	// is terminal; otherwise the finalizer and the renewable realm go with it.
	if operation, unresolved := migration.Status.ActiveOperation, migration.Status.UnresolvedRun; migrationOperation(operation).Mutating && unresolved != nil && unresolved.OperationID == operation.ID {
		return r.reconcileUnaccountedMigrationApply(ctx, migration)
	}
	if migration.DeletionTimestamp != nil {
		return r.reconcileMigrationDeletion(ctx, migration)
	}
	// The record of a run nobody accounted for is put back before anything
	// else reads status: a resource restored without its status carries the
	// record only in its metadata, and every step below would read a status
	// that says nothing is outstanding.
	if handled, err := r.reconcileUnresolvedRunCopy(ctx, migration); handled || err != nil {
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
	}
	if result, handled, err := r.reconcileMigrationExecutionBinding(ctx, migration); handled || err != nil {
		return result, err
	}
	if migration.Status.ActiveOperation != nil {
		return r.reconcileActiveMigration(ctx, migration)
	}
	if controllerutil.ContainsFinalizer(migration, migrationOperationFinalizer) {
		if err := r.removeMigrationFinalizer(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
		// The metadata patch advanced resourceVersion; reload before the status
		// write below so an optimistic conflict is seen here rather than later.
		if err := r.directReader().Get(ctx, request.NamespacedName, migration); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		// The reload can bring state a newer manager wrote between the two
		// reads, so the fence is applied to what was read rather than once per
		// pass.
		if err := r.rejectUnsupportedStoredControllerState(migration); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Between operations, and ahead of every gate below: an acknowledgment
	// touches no database, so a suspended resource or one another claimant
	// holds still settles the record a person accounted for.
	if err := r.reconcileRunAcknowledgments(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	if !databaseEngineSupported(migration.Spec.Target.Engine) {
		return r.migrationBlocked(
			ctx, migration, operatorv1alpha1.ReasonUnsupportedEngine,
			fmt.Sprintf("Database engine %q is not supported by this operator", migration.Spec.Target.Engine),
		)
	}
	if migration.Spec.Suspend {
		before := migration.DeepCopy()
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseSuspended
		migration.Status.ObservedGeneration = migration.Generation
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, operatorv1alpha1.ReasonSuspended, "New migration operations are suspended")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, operatorv1alpha1.ReasonSuspended, "Reconciliation is suspended")
		return ctrl.Result{}, r.patchMigrationStatus(ctx, before, migration)
	}

	// After suspension and before any claim: a suspended resource runs nothing
	// and needs no verdict about the realm, and a resource already carrying an
	// operation returned above. A dispatched Apply is never abandoned for this.
	verdict, censusErr := takeRealmCensus(ctx, r.Client, migration.Namespace, migration.Spec.Target)
	if censusErr != nil {
		return ctrl.Result{}, censusErr
	}
	if refusal, refused := verdict.refusal(); refused {
		return r.migrationRealmBlocked(ctx, migration, refusal)
	}

	now := r.now()
	if migration.Status.ObservedGeneration != migration.Generation {
		return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationResolve)
	}
	if moved, movedErr := configMapSourceMoved(ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact.ConfigMap, migration.Status.ConfigMap); movedErr != nil {
		return ctrl.Result{}, movedErr
	} else if moved {
		// An edit to the source ConfigMap is read now rather than at the next
		// interval; unchanged selected files reach the same plan.
		return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationResolve)
	}
	if moved, movedErr := sourcePolicyMoved(ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact,
		migration.Status.SourcePolicy, migration.Status.Artifact != nil); movedErr != nil {
		return ctrl.Result{}, movedErr
	} else if moved {
		// Another source policy is read now, and the source checked against
		// it: a plan made under the old one does not apply.
		return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationResolve)
	}
	if request := readRequestOf(migration.Annotations, migration.Status.LastReadRequest); request.pending {
		on := migrationReadOnRequest(migration.Status.Phase, due(migration.Status.NextReconciliationTime, now))
		if on.answer || !request.instant {
			return r.answerMigrationReadRequest(ctx, migration, request, on.schedule)
		}
	}
	switch migration.Status.Phase {
	case operatorv1alpha1.MigrationPhaseVerifying:
		return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationVerify)
	case operatorv1alpha1.MigrationPhaseReading, operatorv1alpha1.MigrationPhaseVerifyingHistory:
		return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationHistory)
	case operatorv1alpha1.MigrationPhaseAwaitingApproval:
		if due(migration.Status.NextReconciliationTime, now) {
			// Refresh the whole evidence chain before consulting even an exact
			// approval: a moved tag or a history somebody else advanced is what
			// makes an approval stale, and only a fresh reading shows it.
			return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationResolve)
		}
		return r.reconcileMigrationApplyDecision(ctx, migration)
	case operatorv1alpha1.MigrationPhasePlanning:
		if due(migration.Status.NextReconciliationTime, now) || migrationPublicationUnfinished(migration) {
			return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationResolve)
		}
		return r.reconcileMigrationApplyDecision(ctx, migration)
	case operatorv1alpha1.MigrationPhaseInSync,
		operatorv1alpha1.MigrationPhaseBlocked,
		operatorv1alpha1.MigrationPhaseFailed:
		if !due(migration.Status.NextReconciliationTime, now) {
			return requeueAtDeadline(migration.Status.NextReconciliationTime, now), nil
		}
	}
	// A moved tag, a changed policy, or a history someone else advanced is only
	// observable by starting the read-only chain again from resolution.
	return r.claimMigration(ctx, migration, operatorv1alpha1.MigrationOperationResolve)
}

// migrationPublicationUnfinished reports a history read whose plan was never
// published: Planning, with the Progressing reason the harvest writes and the
// publication rewrites. A pass that stopped between the two writes leaves it, and
// with it the plan the resource named before, if any, and the full interval the
// harvest gave a plan about to be published. Under OnApproval that is the gate
// closed for an interval, since admission takes an approval only at
// AwaitingApproval, so the chain is read again instead. The claim rewrites
// Progressing, so the next pass cannot claim again until another publication
// fails.
func migrationPublicationUnfinished(migration *operatorv1alpha1.PtahMigration) bool {
	progressing := meta.FindStatusCondition(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	return progressing != nil && progressing.Reason == string(operatorv1alpha1.ReasonMigrationsPending)
}

// answerMigrationReadRequest records a read request as answered, as
// answerReadRequest does for a schema.
func (r *MigrationReconciler) answerMigrationReadRequest(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	request readRequest,
	schedule bool,
) (ctrl.Result, error) {
	before := migration.DeepCopy()
	migration.Status.LastReadRequest = request.token
	scheduled := request.instant && schedule
	if scheduled {
		now := metav1.NewTime(r.now())
		migration.Status.NextReconciliationTime = &now
	}
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case !request.instant:
		r.event(migration, corev1.EventTypeWarning, "ReadRequestInvalid",
			"%s is %q, which is not an RFC 3339 instant; no read was started", operatorv1alpha1.ReadRequestAnnotation, request.token)
	case scheduled:
		r.event(migration, corev1.EventTypeNormal, "ReadRequested", "A read was requested; it starts now")
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// reconcileMigrationDeletion releases the resource once no Job of its claim can
// still be running. A read-only claim is discarded rather than waited on: its
// Job reads and reports, and a result nobody is waiting for costs nothing.
//
// An Apply is not that. Its Job is owned by this resource, so removing the
// finalizer hands the Job to cascading deletion, which stops an executor in the
// middle of a statement; and the claim dropped with it is the only record that
// the run may have changed the database, and the only thing that hands the
// database back. So the claim is kept until nothing it dispatched can write.
//
// Whether that is still possible is the same question an uncertain outcome
// asks before it releases the Lease, and it is asked here through the same
// function rather than a second one: a Job that is not terminal, a Pod that has
// not stopped, and a read that could not say all keep the resource. The Job's
// activeDeadlineSeconds ends a run that hangs, so that much of the wait is
// bounded; a Pod on a node the API server cannot reach is not, and it takes
// the node going, the Pod force-deleted, or a person removing the finalizer.
func (r *MigrationReconciler) reconcileMigrationDeletion(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (ctrl.Result, error) {
	if operation := migration.Status.ActiveOperation; operation != nil {
		if migrationOperation(operation).Mutating {
			job, err := r.dispatchedMigrationApplyJob(ctx, migration, operation)
			if err != nil {
				return ctrl.Result{}, err
			}
			if r.dispatchedApplyMayStillWrite(ctx, migration.Namespace, operation, job) {
				// Renew while waiting, the way every other pass over a live
				// Apply does. The Lease is sized to outlive the Job's own
				// deadline and no further, and this wait can outlast that: a
				// Pod on a node the API server cannot reach stays Running with
				// no bound at all. A Lease that lapses under that Pod hands the
				// realm to the next claimant, which then runs DDL beside an
				// executor that never stopped -- the one thing the Lease is for.
				//
				// What the renewal returns does not change what happens next.
				// The Pod is still there either way, so the resource is kept
				// either way; a realm somebody else has taken is recorded on
				// the claim rather than acted on, and the release below names
				// the claim's epoch, so it cannot clear a holder that is not
				// this one.
				if _, _, err := r.acquireMigrationApplyLock(ctx, migration); err != nil {
					return ctrl.Result{}, err
				}
				// The look Apply supervision takes at a Job in flight. The
				// record this wait ends in carries what Progressing last said
				// about the Pod, so a Pod that starts while the resource waits
				// has to take those words off before the run ends without a
				// result. A look that fails leaves them standing, still worded
				// as the last report, and is not returned: an error requeue
				// backs off, and this wait is what renews the Lease.
				if job != nil && operation.JobUID != "" && !jobTerminal(job) {
					if err := r.reportMigrationPodAdmission(ctx, migration, job); err != nil {
						ctrl.LoggerFrom(ctx).Info("could not look at the dispatched Apply Pod during deletion", "error", err.Error())
					}
				}
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			// A dispatch this claim started, or a Job it can account for under
			// the name it reserved. The JobUID is the companion this file gives
			// DispatchStarted everywhere it asks that, rather than a third
			// case: DispatchStarted is written before the create and never
			// cleared, and a UID is only recorded afterwards, so no state holds
			// one without the other and no mutation can separate them.
			//
			// A deleting migration does not wait MissingJobGrace for a missing
			// Job: the Job-write webhook refuses a create for an owner being
			// deleted.
			if migrationMayHaveDispatched(operation) || job != nil {
				// Something ran under this claim and nothing read what it did.
				// The unknown outcome and the database go back first; the next
				// pass finds no claim and lets the resource go.
				//
				// That pass has to be asked for. The primary watch takes a
				// generation, a label or an annotation, so the status this
				// writes wakes nothing, and a deleting resource whose Job has
				// stopped changing gets no other event. Without the requeue the
				// finalizer would sit until the manager restarted.
				//
				// The retirement is told what this pass already proved rather
				// than asking again. A second read that fails answers "may still
				// be writing" -- the right answer to a question it could not
				// settle, and the wrong outcome here, because the claim that
				// names the epoch goes in this write and every other resource on
				// that database would wait out the full lease for a run this pass
				// watched stop.
				//
				// The resource and this record go on the next pass, and the
				// MigrationRunUncertain Event outlives them, so the failure
				// carries what the kubelet last said about a Pod that never
				// started.
				//
				// A claim that never recorded its Job's UID records it first, as
				// every other pass that settles an Apply nobody read does
				// (adoptUnrecordedApplyJob), so the run names the Job and
				// admission admits its cleanup TTL. That TTL is what collects a
				// Job the deletion orphans.
				if job != nil {
					if job, _, err = r.adoptUnrecordedApplyJob(ctx, migration, job); err != nil {
						return ctrl.Result{}, err
					}
				}
				if _, err := r.retireUncertainMigrationApply(ctx, migration, job,
					withLastPodReport(errors.New("the PtahMigration was deleted while a dispatched Apply was in flight"),
						migrationPodReport(migration, job)),
					"", false); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			// Nothing stands under the name the claim reserved, so nothing can
			// have written. The claim is discarded, and the database goes back
			// in the same write, without a verdict.
		}
		if err := r.retireMigrationClaim(
			ctx, migration.DeepCopy(), migration, mutationlifecycle.DispositionDiscard, false,
		); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.reportDiscardedUnresolvedRun(ctx, migration)
	if err := r.removeMigrationFinalizer(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reportDiscardedUnresolvedRun says what a deletion is about to destroy.
//
// status.unresolvedRun is the record that an Apply may have changed the
// database and nobody established what it did. A History reading of the
// database the run addressed, with nothing left to apply, settles it: that is
// the proof the record was waiting for. A person can also clear it, and
// deleting the resource is one of the ways a person can. The operator does not
// refuse a deletion: a deleting resource reads no more history, so the refusal
// would be one the operator could never lift, and a resource nobody can
// remove.
//
// What it does refuse to do is lose the record quietly. The object is going
// away and the record with it, so the last place the run can be named is an
// Event and the log -- which is where whoever finds an unaccounted-for change
// in that database will be looking.
func (r *MigrationReconciler) reportDiscardedUnresolvedRun(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) {
	unresolved := migration.Status.UnresolvedRun
	if unresolved == nil {
		return
	}
	job := unresolved.JobName
	if job == "" {
		job = "no Job this claim recorded"
	}
	r.event(migration, corev1.EventTypeWarning, "UnresolvedRunDiscarded",
		"Deleting this resource discards the record of a %s run nobody accounted for: %s, plan %s, database %s%s",
		unresolved.Outcome, job, unresolved.PlanRef.Name, bounded(unresolved.TargetIdentityDigest, 80),
		r.discardedRunJobGoes(ctx, migration, unresolved))
	ctrl.LoggerFrom(ctx).Info(
		"deleting a migration discards an unresolved run",
		"outcome", unresolved.Outcome,
		"operation", unresolved.OperationID,
		"job", unresolved.JobName,
		"targetIdentityDigest", unresolved.TargetIdentityDigest,
	)
}

// discardedRunJobGoes says that the Job a discarded run ran as goes with the
// resource, and its Pod log with it, when it still stands and does
// (migrationJobGoesWithResource). An earlier MigrationRunUncertain Event may point
// at that log for a day; this is the last word on it. A Job under the name with
// another UID is a later run's, and a read that fails says nothing.
func (r *MigrationReconciler) discardedRunJobGoes(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	unresolved *operatorv1alpha1.UnresolvedMigrationRunStatus,
) string {
	if unresolved.JobName == "" || unresolved.JobUID == "" {
		return ""
	}
	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: unresolved.JobName}
	if err := r.directReader().Get(ctx, key, job); err != nil || job.UID != unresolved.JobUID ||
		!migrationJobGoesWithResource(migration, job) {
		return ""
	}
	return ". The Job and its Pod log are deleted with the PtahMigration"
}

// dispatchedMigrationApplyJob returns the Job this Apply claim dispatched, and
// nil when nothing the claim can account for stands under the name it reserved.
//
// A Job under that name carrying another UID belongs to a later attempt: this
// claim's own Job is gone, and only the Pods it owned can still be running. A
// claim that recorded no UID is not evidence that nothing was created either,
// so the name is read rather than assumed empty.
func (r *MigrationReconciler) dispatchedMigrationApplyJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: operation.JobName}
	switch err := r.directReader().Get(ctx, key, job); {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read the dispatched Apply Job during deletion: %w", err)
	}
	if operation.JobUID != "" && job.UID != operation.JobUID {
		return nil, nil
	}
	return job, nil
}

// reconcileMigrationExecutionBinding publishes the component identity this
// resource's work is bound to, and retires a claim authorized under an older
// one. A rollout that changed the executor, the Ptah version, the runner
// protocol or the controller-state version must invalidate work in flight
// rather than let it finish under new semantics.
//
// The manager's own image and revision and the runner image built beside it
// are not compared. A release that changes only them keeps the epoch, the
// plan and the approval. A run the previous manager dispatched is adopted only
// when this release builds the same Job apart from that identity
// (holdMigrationJobToItsClaim); a release that also changed the Job or its Pod
// template records a dispatched Apply as outcome unknown and runs a read-only
// claim again.
func (r *MigrationReconciler) reconcileMigrationExecutionBinding(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (ctrl.Result, bool, error) {
	configured, err := r.configuredMigrationBinding()
	if err != nil {
		result, failureErr := r.migrationOperationFailure(ctx, migration, err)
		return result, true, failureErr
	}
	current := migration.Status.ExecutionBinding
	if current != nil && executionBindingComponentsEqual(current, configured) {
		return ctrl.Result{}, false, nil
	}
	binding, err := newExecutionBinding(configured)
	if err != nil {
		result, failureErr := r.migrationOperationFailure(ctx, migration, err)
		return result, true, failureErr
	}
	if operation := migration.Status.ActiveOperation; migrationOperation(operation).Mutating &&
		migrationMayHaveDispatched(operation) {
		// A dispatched Apply is not retired by a rollout. Its Job may already
		// have changed the database, and dropping the claim would drop the only
		// record that it might have.
		//
		// The evidence is written first, under the binding that authorized the
		// run, and the binding moves on the next pass once no claim is in
		// flight. Two writes in that order, rather than one that would report a
		// run against components it never had.
		return r.applyUncertainUnderBindingChange(ctx, migration)
	}
	if result, held, err := r.letGoOfRetiredReadOnlyMigrationJob(ctx, migration); held || err != nil {
		return result, true, err
	}
	before := migration.DeepCopy()
	migration.Status.ExecutionBinding = binding
	if current != nil {
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationDatabaseReachable, metav1.ConditionUnknown,
			operatorv1alpha1.ReasonExecutionBindingChanged, "The last reading of the target is historical evidence from the previous execution binding")
	}
	if migration.Status.ActiveOperation == nil {
		if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, true, nil
	}
	r.event(migration, corev1.EventTypeWarning, "ExecutionBindingChanged",
		"Discarding the %s operation claim: an execution component changed", migration.Status.ActiveOperation.Type)
	migration.Status.Phase = operatorv1alpha1.MigrationPhasePending
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
		operatorv1alpha1.ReasonExecutionBindingChanged, "The operation was retired because an execution component changed")
	// Nothing this claim dispatched can be running: a dispatched Apply became
	// an unresolved run above instead. So an undispatched Apply's Lease goes
	// back in the same write.
	if err := r.retireMigrationClaim(ctx, before, migration, mutationlifecycle.DispositionDiscard, false); err != nil {
		return ctrl.Result{}, true, err
	}
	// The next pass works under the binding this write installed, so it has
	// to start from a fresh read of it.
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, true, nil
}

// applyUncertainUnderBindingChange records that a dispatched Apply outlived the
// components that authorized it. It is the same answer as any other Apply the
// controller cannot read: the run's evidence is unknown, the resource is
// blocked, and nothing dispatches again until a person has looked.
func (r *MigrationReconciler) applyUncertainUnderBindingChange(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (ctrl.Result, bool, error) {
	job, err := r.recordedMigrationApplyJob(ctx, migration)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	result, err := r.finishUncertainMigrationApply(ctx, migration, job,
		withLastPodReport(errors.New("an execution component changed while the Apply was dispatched"),
			migrationPodReport(migration, job)), "")
	return result, true, err
}

// letGoOfRetiredReadOnlyMigrationJob settles what a read-only claim a rotation
// retires owes the Job under its reserved name (letGoOfReadOnlyMigrationJob),
// and reports held while the claim has to stay over it. The rotation waits under
// the binding status still records, since that is the binding admission holds the
// Job's cleanup TTL to, and the claim is retired with the rotation's own write
// once it is not held. It does not wait for a suspended resource. A failed read
// fails the pass: retiring the claim on it would let go of a Job that may still
// be owed its cleanup.
func (r *MigrationReconciler) letGoOfRetiredReadOnlyMigrationJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (ctrl.Result, bool, error) {
	operation := migration.Status.ActiveOperation
	if !migrationOperation(operation).ReadOnly() || operation.JobName == "" {
		return ctrl.Result{}, false, nil
	}
	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: operation.JobName}
	switch err := r.directReader().Get(ctx, key, job); {
	case apierrors.IsNotFound(err):
		return ctrl.Result{}, false, nil
	case err != nil:
		return ctrl.Result{}, false, fmt.Errorf("read the %s Job of the retired claim: %w", operation.Type, err)
	}
	waiting := fmt.Sprintf("Waiting for Job %s to stop before the %s operation is retired: an execution component changed",
		job.Name, operation.Type)
	return r.letGoOfReadOnlyMigrationJob(ctx, migration, job, waiting)
}

// recordedMigrationApplyJob reads the Job the active claim recorded by UID, for
// a pass that settles the claim without supervising its Job. It is nil when the
// claim recorded no UID or the name it reserved holds no Job with that UID. A
// claim with no UID records one before its run is settled
// (adoptUnrecordedApplyJob), so the Job its dispatch created is handed on there.
//
// Handing the Job on is what lets the settling pass schedule its cleanup TTL
// while the claim still names it, the one moment admission accepts it. A
// finished Job gets it in that pass; a running one keeps the claim, and the
// pass that retires the claim once the Job has stopped schedules it.
func (r *MigrationReconciler) recordedMigrationApplyJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (*batchv1.Job, error) {
	operation := migration.Status.ActiveOperation
	if operation.JobUID == "" {
		return nil, nil
	}
	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: operation.JobName}
	switch err := r.directReader().Get(ctx, key, job); {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read the dispatched Apply Job: %w", err)
	}
	if job.UID != operation.JobUID {
		return nil, nil
	}
	return job, nil
}

// rejectUnsupportedStoredControllerState is the first check after the direct
// API read, and the migration family's half of the contract PtahSchema has
// held since it gained one.
//
// A manager that meets state a newer controller wrote cannot know what that
// state means, so it must not act on it: no binding rotated, no finalizer
// added or removed, no Lease renewed or released, and no operation claim
// interpreted. The startup and Helm preflight scans cover the kinds that exist
// when a manager starts; this is the runtime boundary, for state restored or
// otherwise introduced afterwards.
//
// Refusing is safe because containment is already paid for elsewhere: a
// dispatched Job carries its own absolute deadlines, and a Lease expires on
// its own. Both outlive this refusal and neither needs this manager to act.
func (r *MigrationReconciler) rejectUnsupportedStoredControllerState(
	migration *operatorv1alpha1.PtahMigration,
) error {
	if migration == nil {
		return errors.New("migration is unavailable")
	}
	binding := migration.Status.ExecutionBinding
	if binding == nil {
		return nil
	}
	configured, err := r.configuredMigrationBinding()
	if err != nil {
		return err
	}
	if binding.ControllerStateVersion < 0 {
		return fmt.Errorf(
			"stored status.executionBinding controller state version %d is invalid; refusing to interpret or write PtahMigration state",
			binding.ControllerStateVersion,
		)
	}
	if binding.ControllerStateVersion > configured.ControllerStateVersion {
		return fmt.Errorf(
			"stored status.executionBinding controller state version %d exceeds supported version %d; refusing to interpret or write PtahMigration state",
			binding.ControllerStateVersion,
			configured.ControllerStateVersion,
		)
	}
	return nil
}

func (r *MigrationReconciler) configuredMigrationBinding() (*operatorv1alpha1.ExecutionBindingStatus, error) {
	if r.Jobs == nil {
		return nil, errors.New("Job builder is not configured")
	}
	controllerStateVersion, ptahVersion, executorImage, protocolVersion := r.Jobs.ExecutionBinding()
	return &operatorv1alpha1.ExecutionBindingStatus{
		ControllerStateVersion: controllerStateVersion,
		PtahVersion:            ptahVersion,
		ExecutorImage:          executorImage,
		RunnerProtocolVersion:  protocolVersion,
	}, nil
}

// claimMigration persists the decision before the Job exists. The claim carries
// the Job's deterministic name, which is what lets a controller that restarted
// mid-dispatch tell the Job it created from one it has not.
func (r *MigrationReconciler) claimMigration(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	operationType operatorv1alpha1.MigrationOperationType,
) (ctrl.Result, error) {
	if migration.Status.ActiveOperation != nil {
		// Unreachable in the normal dispatch: reconcile's own ActiveOperation
		// check sends a resource carrying one to reconcileActiveMigration before
		// this function is ever called. Requeuing rather than erroring keeps
		// that true if it ever stops being true.
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
	}
	if migration.Status.ExecutionBinding == nil {
		// Unreachable for the same reason: reconcileMigrationExecutionBinding
		// runs first and only reports itself unhandled once the binding is
		// current, which requires it to be non-nil.
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
	}
	if operationType == operatorv1alpha1.MigrationOperationResolve && migration.Spec.Artifact.ConfigMap != nil {
		// A ConfigMap source is read by the manager: no registry, no
		// credential, so no Resolve Job and no Verify step.
		return r.resolveMigrationConfigMapSource(ctx, migration)
	}
	if git := migration.Spec.Artifact.Git; operationType == operatorv1alpha1.MigrationOperationResolve && git != nil {
		// A host the source policy does not allow gets no Job: the
		// credential is never sent there.
		policy, _, err := sourcePolicyFor(ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact)
		if err == nil {
			err = checkGitSource(policy, git, 0)
		}
		if err != nil {
			if errors.Is(err, errSourceRefused) {
				return r.claimRefused(ctx, migration, err)
			}
			return ctrl.Result{}, err
		}
	}
	inputFingerprint, err := r.migrationInputFingerprint(ctx, migration, operationType)
	if err != nil {
		return r.claimRefused(ctx, migration, err)
	}
	id, err := r.newMigrationOperationID(inputFingerprint)
	if err != nil {
		return ctrl.Result{}, err
	}
	operation := &operatorv1alpha1.MigrationOperationStatus{
		Type:               operationType,
		ID:                 id,
		InputFingerprint:   inputFingerprint,
		StartedAt:          metav1.NewTime(r.now()),
		Attempt:            1,
		ExecutionBindingID: migration.Status.ExecutionBinding.Epoch,
	}
	if operationType != operatorv1alpha1.MigrationOperationResolve {
		if migration.Status.Artifact == nil {
			return r.claimRefused(ctx, migration, errors.New("the artifact has not been resolved to a digest"))
		}
		operation.Source = migrationSourceBinding(migration)
	}
	if operationType == operatorv1alpha1.MigrationOperationHistory {
		coordinationDigest, digestErr := coordination.Digest(migration.Namespace, migration.Spec.Target)
		if digestErr != nil {
			return r.claimRefused(ctx, migration, fmt.Errorf("derive coordination digest: %w", digestErr))
		}
		operation.CoordinationDigest = coordinationDigest
		operation.Target = &operatorv1alpha1.DatabaseTargetBinding{
			Engine:  migration.Spec.Target.Engine,
			URLFrom: *migration.Spec.Target.URLFrom.DeepCopy(),
		}
	}
	if r.Jobs == nil {
		return r.claimRefused(ctx, migration, errors.New("Job builder is not configured"))
	}
	operation.JobName, err = r.Jobs.NameForMigration(migration, *operation)
	if err != nil {
		return r.claimRefused(ctx, migration, fmt.Errorf("name %s Job: %w", operationType, err))
	}
	if err := r.ensureMigrationFinalizer(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	before := migration.DeepCopy()
	migration.Status.ActiveOperation = operation
	migration.Status.ObservedGeneration = migration.Generation
	slot, slotted := refreshSlot(ctx, r.Client, migration.UID, migrationInterval(migration))
	migration.Status.NextReconciliationTime = claimDeadline(migration.Status.NextReconciliationTime,
		operationType == operatorv1alpha1.MigrationOperationResolve, mutationlifecycle.MigrationOperation(operationType).Mutating,
		operation.StartedAt.Time, migrationInterval(migration), slot, slotted)
	migration.Status.Phase = mutationlifecycle.MigrationOperation(operationType).Phase
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		operatorv1alpha1.ReasonOperationInProgress, fmt.Sprintf("%s operation is in progress", operationType))
	endMigrationRealmRefusal(migration, operatorv1alpha1.ReasonOperationInProgress, migrationRealmReadMessage)
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	ctrl.LoggerFrom(ctx).Info("migration operation claimed", "operation", operationType, "phase", migration.Status.Phase)
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

func (r *MigrationReconciler) reconcileActiveMigration(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) (ctrl.Result, error) {
	operation := migration.Status.ActiveOperation
	if operation.LeaseContinuityLost {
		job, err := r.recordedMigrationApplyJob(ctx, migration)
		if err != nil {
			return ctrl.Result{}, err
		}
		// The continuity write leaves a Pod the kubelet cannot start on
		// Progressing, so the words are still there to carry.
		return r.finishUncertainMigrationApply(ctx, migration, job,
			withLastPodReport(errors.New("the database lock epoch changed under the dispatched run"),
				migrationPodReport(migration, job)), "")
	}
	kind := migrationOperation(operation)
	mutating := kind.Mutating
	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: operation.JobName}
	err := r.directReader().Get(ctx, key, job)
	found := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("read active migration Job: %w", err)
	}
	// Suspension is judged only against a Job that exists. A claim with none
	// goes to dispatch, which refuses a suspended resource itself and is where
	// that refusal has always lived. The claim settles at once what it owes that
	// Job, since it is what lets admission accept the Job's cleanup TTL, and it
	// does not wait: suspension is the stop button, whether the claim was
	// supervising the Job or waiting for one it refused. A claim of a suspended
	// resource never waits (letGoOfReadOnlyMigrationJob), so it has no words for
	// a wait.
	if found && migration.Spec.Suspend && !mutating {
		if _, _, err := r.letGoOfReadOnlyMigrationJob(ctx, migration, job, ""); err != nil {
			return ctrl.Result{}, err
		}
		return r.discardMigrationOperation(ctx, migration, telemetry.OperationCanceled,
			errors.New("reconciliation was suspended while the operation ran"))
	}
	var standing *batchv1.Job
	if found {
		standing = job
	}
	verdict, cause, wait := missingJobsOf(&r.missingJobs).Verdict(client.ObjectKeyFromObject(migration), operation.ID,
		migrationJobClaim(migration, standing), monotonicNow(r.Clock))
	switch verdict {
	case mutationlifecycle.VerdictDispatch:
		return r.dispatch.Dispatch(ctx, &migrationDispatch{r: r, migration: migration})
	case mutationlifecycle.VerdictAwait:
		return awaitMarkedCreate(ctx, operation.JobName, wait), nil
	}
	// Every other verdict is about a Job that may be running under the realm
	// the claim holds, so the Lease is renewed before the pass acts on it.
	if kind.HoldsLock(false) {
		acquired, requeue, lockErr := r.waitForMigrationApplyLock(ctx, migration)
		if lockErr != nil {
			return ctrl.Result{}, lockErr
		}
		if !acquired {
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
		operation = migration.Status.ActiveOperation
	}
	switch verdict {
	case mutationlifecycle.VerdictUnaccounted:
		// A dispatched Apply is never recreated. Whether it ran is a question
		// for the database, not for a retry.
		var lost *batchv1.Job
		if found {
			lost = job
		}
		// A replacement under the name is not the claim's Job, and the report
		// does not take its success as the claim's Pod having run.
		return r.finishUncertainMigrationApply(ctx, migration, lost,
			withLastPodReport(errors.New(unaccountedMigrationJobReason(cause)),
				migrationPodReport(migration, lost)), "")
	case mutationlifecycle.VerdictRetry:
		// A replaced or disowned Job is not the claim's, so the claim owes it no
		// cleanup TTL, and the controller-write webhook would refuse one: it
		// admits a TTL only on a stopped Job the claim names by UID and owns
		// exactly. Asking anyway failed every pass before the retry was written.
		return r.retryMigrationOperation(ctx, migration, nil, errors.New(retriedMigrationJobReason(cause)))
	}
	if result, settled, err := r.holdMigrationJobToItsClaim(ctx, migration, job); settled || err != nil {
		return result, err
	}
	if verdict == mutationlifecycle.VerdictAdopt {
		if _, err := r.adoptMigrationJob(ctx, migration, job); err != nil {
			return ctrl.Result{}, err
		}
		operation = migration.Status.ActiveOperation
	}
	if !jobTerminal(job) {
		if err := r.reportMigrationPodAdmission(ctx, migration, job); err != nil {
			return ctrl.Result{}, err
		}
		if durableDeliveryRequested(job) {
			engine := ""
			if operation.Target != nil {
				engine = string(operation.Target.Engine)
			}
			issued, issueErr := issueResultCredential(ctx, r.directReader(), r.ResultCredentials, resultEnrollmentHintsOf(&r.enrollmentHints), migration, "PtahMigration", job, operation.AdmissionSnapshot, operation.ExecutionBindingID, operation.InputFingerprint, string(migrationOperation(operation).Runner), operation.ID, engine)
			if issueErr != nil {
				r.event(migration, corev1.EventTypeWarning, "ResultCredentialFailed", "Result delivery credential is not ready; issuance will be retried")
			}
			if issueErr != nil || !issued {
				return ctrl.Result{RequeueAfter: resultReadRetryInterval}, nil
			}
		}
		return ctrl.Result{RequeueAfter: activeJobPollInterval(kind.HoldsLock(false))}, nil
	}
	current, currentErr := r.migrationInputFingerprint(ctx, migration, operation.Type)
	if currentErr != nil || current != operation.InputFingerprint {
		if currentErr == nil {
			currentErr = errors.New("the operation inputs changed while the Job was running")
		}
		if mutationlifecycle.HarvestFailure(
			mutationlifecycle.FaultInputsChanged, mutating,
		) == mutationlifecycle.DispositionUnaccounted {
			// An Apply Job that exists may already have changed the database,
			// whatever its formerly exact inputs now say.
			return r.finishUncertainMigrationApply(ctx, migration, job, withLastPodReport(currentErr,
				migrationPodReport(migration, job)), "")
		}
		if err := r.harvestMigrationJob(ctx, migration, job); err != nil {
			return ctrl.Result{}, err
		}
		return r.discardMigrationOperation(ctx, migration, telemetry.OperationStale, currentErr)
	}
	evidence, err := r.migrationTerminalLogs(ctx, migration, job)
	if err != nil {
		if errors.Is(err, errResultReadCooling) {
			return ctrl.Result{RequeueAfter: resultReadRetryInterval}, nil
		}
		if errors.Is(err, errTerminalPodPending) {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		if errors.Is(err, errTerminalPodMultiplicity) || errors.Is(err, errTerminalPodIntent) {
			if mutationlifecycle.HarvestFailure(
				mutationlifecycle.FaultPodMultiplicity, mutating,
			) == mutationlifecycle.DispositionUnaccounted {
				return r.finishUncertainMigrationApply(ctx, migration, job,
					withLastPodReport(err, migrationPodReport(migration, job).withPodsRead(evidence)), "")
			}
			return r.retryMigrationOperation(ctx, migration, job, err)
		}
		if errors.Is(err, errResultReadRetry) {
			if errors.Is(err, context.DeadlineExceeded) {
				r.event(migration, corev1.EventTypeWarning, "ResultReadTimedOut",
					"reading the %s result took longer than its bound: %s",
					operation.Type, bounded(err.Error(), 512))
			} else {
				r.event(migration, corev1.EventTypeWarning, "ResultReadFailed",
					"reading the %s result failed and will be tried again: %s",
					operation.Type, bounded(err.Error(), 512))
			}
			return ctrl.Result{RequeueAfter: resultReadRetryInterval}, nil
		}
		return ctrl.Result{}, err
	}
	result, parseErr := evidence.parseResult(kind.Runner, operation.ID)
	if requeue, wait := awaitFrameArrival(job, parseErr, r.now()); wait {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	if refusal := runnerProtocolRefusal(result, parseErr); refusal != nil {
		r.event(migration, corev1.EventTypeWarning, "RunnerProtocolMismatch",
			"the %s runner refused the Job before starting the executor: %s", operation.Type, bounded(refusal.Error(), 512))
		if !mutating {
			return r.retryMigrationOperationAs(ctx, migration, job, operatorv1alpha1.ReasonRunnerProtocolMismatch, refusal)
		}
		// An Apply is settled from its own evidence as every Apply is. A
		// refusal from a runner of another protocol is not a frame of this
		// protocol, so it reaches the unread path and is recorded as a run
		// nobody accounted for; one from a runner of this protocol is its
		// frame, and says the child never started.
	}
	if mutating {
		// The run's own evidence settles an Apply, whatever the Job's exit
		// status said: a run that stopped is exactly the run whose controller
		// has to be told what the database now holds.
		if parseErr != nil {
			return r.settleUnreadMigrationApply(ctx, migration, job, evidence, parseErr)
		}
		return r.settleMigrationApply(ctx, migration, job, r.frameMigrationApply(ctx, migration, result))
	}
	if parseErr != nil {
		if boundary, boundaryErr := failedInitBoundary(ctx, r.directReader(), job); boundaryErr != nil {
			return ctrl.Result{}, boundaryErr
		} else if boundary != "" {
			return r.retryMigrationOperation(ctx, migration, job, errors.New(boundary))
		}
		return r.retryMigrationOperation(ctx, migration, job, withUnstartedPod(fmt.Errorf("read %s result: %w", operation.Type, parseErr),
			migrationPodReport(migration, job)))
	}
	if !jobSucceeded(job) {
		return r.retryMigrationOperation(ctx, migration, job, fmt.Errorf("the %s Job failed", operation.Type))
	}
	if result.Error != nil {
		if operation.Type == operatorv1alpha1.MigrationOperationVerify && result.Error.Code == verificationRefusedCode {
			return r.refuseMigrationArtifact(ctx, migration, job, result)
		}
		return r.retryMigrationOperation(ctx, migration, job,
			resultFailure(result.Error, operationURLSecret(operation.Target, migration.Spec.Target.URLFrom)))
	}
	if result.Truncation != nil && result.Truncation.Stdout {
		return r.retryMigrationOperation(ctx, migration, job, fmt.Errorf("the %s result was truncated", operation.Type))
	}
	return r.consumeMigrationResult(ctx, migration, job, result)
}

// holdMigrationJobToItsClaim asks whether the Job standing under the active
// claim's reserved name, owned by this resource, is the Job that claim built,
// and settles the claim when it cannot say yes. It reports whether it settled
// the claim.
//
// The question is the schema family's, asked the same way and at the same
// points: before the claim adopts a Job a stopped pass created, and on every
// pass that supervises one. The Job the claim builds now is held to the live
// one by jobclaim.Match, with the manager's recorded identity taken from the
// live Pod template (validateAdoptedMigrationJobIntent says why that is safe).
// That is asked while the inputs the claim was made from still hold. Once they
// have moved, the claim cannot rebuild its Job, so the Job is held to what the
// claim fixes without a rebuild (validateMigrationJobEnvelope): its epoch, its
// labels and annotations, and the Pod template its admission snapshot
// recorded. Such a Job is never harvested either: the terminal branch reads
// the same inputs and settles the claim as stale or as outcome unknown
// without reading a result.
//
// A Job the claim cannot confirm is settled by the asymmetry every lost Job
// is. A mutating claim's Job may already be running SQL, so the run is
// recorded as outcome unknown, naming the Job, and nothing is dispatched
// beside it. A claim that never recorded the Job's UID records it first
// (adoptMigrationJob), and the Job gets its cleanup TTL once it stops, before
// the claim goes (scheduleStoppedMigrationJobCleanup): the webhook holds a
// terminal Job to what its claim fixes without a rebuild, so a Job another
// release built gets it, and one that fails that match is refused and
// reported.
//
// A read-only claim moves to a new attempt under a new name, and the Job it
// refused gets the cleanup TTL a harvested read-only Job gets, once it stops
// (retryPastRefusedMigrationJob). The claim is what lets admission accept that
// TTL, so the claim stays until the Job stops, as it would for a Job it
// supervised, and no longer than the Job's own deadline and the grace its Pods
// are given to stop. A Job whose TTL admission would refuse is left at once, and
// so is one that has not stopped by then, and a Warning names it.
//
// A release that changes the Job or its Pod template therefore cannot adopt a
// run its predecessor dispatched: a running Apply is recorded unknown, and a
// read-only claim is run again.
func (r *MigrationReconciler) holdMigrationJobToItsClaim(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
) (ctrl.Result, bool, error) {
	operation := migration.Status.ActiveOperation
	mutating := migrationOperation(operation).Mutating
	current, currentErr := r.migrationInputFingerprint(ctx, migration, operation.Type)
	if currentErr != nil || current != operation.InputFingerprint {
		if err := validateMigrationJobEnvelope(job, migration); err != nil {
			if mutating {
				result, settleErr := r.finishUncertainMigrationApply(ctx, migration, job,
					withLastPodReport(fmt.Errorf("dispatched Apply Job is not its claim's: %w", err),
						migrationPodReport(migration, job)), "")
				return result, true, settleErr
			}
			result, settleErr := r.retryPastRefusedMigrationJob(ctx, migration, job,
				fmt.Errorf("active Job is not its claim's: %w", err))
			return result, true, settleErr
		}
		return ctrl.Result{}, false, nil
	}
	expected, err := r.expectedMigrationJob(ctx, migration, operation)
	if err != nil {
		if mutating {
			result, settleErr := r.finishUncertainMigrationApply(ctx, migration, job,
				withLastPodReport(fmt.Errorf("rebuild immutable Apply Job intent: %w", err),
					migrationPodReport(migration, job)), "")
			return result, true, settleErr
		}
		result, settleErr := r.retryPastRefusedMigrationJob(ctx, migration, job,
			fmt.Errorf("rebuild immutable Job intent: %w", err))
		return result, true, settleErr
	}
	if err := validateAdoptedMigrationJobIntent(job, expected, migration); err != nil {
		if mutating {
			result, settleErr := r.finishUncertainMigrationApply(ctx, migration, job,
				withLastPodReport(fmt.Errorf("dispatched Apply Job intent changed: %w", err),
					migrationPodReport(migration, job)), "")
			return result, true, settleErr
		}
		result, settleErr := r.retryPastRefusedMigrationJob(ctx, migration, job,
			fmt.Errorf("active Job intent changed: %w", err))
		return result, true, settleErr
	}
	return ctrl.Result{}, false, nil
}

// retryPastRefusedMigrationJob moves a read-only claim to its next attempt past
// the Job holdMigrationJobToItsClaim refused, once the claim owes that Job
// nothing more (letGoOfReadOnlyMigrationJob).
func (r *MigrationReconciler) retryPastRefusedMigrationJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	failure error,
) (ctrl.Result, error) {
	waiting := fmt.Sprintf("Waiting for Job %s, which this release cannot confirm, to stop before the %s operation runs again",
		job.Name, migration.Status.ActiveOperation.Type)
	if result, held, err := r.letGoOfReadOnlyMigrationJob(ctx, migration, job, waiting); held || err != nil {
		return result, err
	}
	return r.retryMigrationOperation(ctx, migration, nil, failure)
}

// letGoOfReadOnlyMigrationJob settles what a read-only claim owes the Job under
// its reserved name before the claim lets go of it, by the rule both families
// keep (releaseReadOnlyJob). It reports held, with the pass's result, while the
// claim has to stay over the Job. The caller lets go once it is not held: it
// retries past a Job it refused, discards the claim of a suspended resource, or
// retires it under a changed execution binding. Every caller holds a read-only
// claim, and decides that before it asks.
//
// A claim of a suspended resource is never held, whichever of those passes asks:
// suspension is the stop button, and a rotation runs before the pass that
// discards a suspended claim, so the rule is kept here rather than by each
// caller.
//
// The Job is held to what the claim fixes without a rebuild under the execution
// binding status records (validateMigrationJobEnvelope), the match the
// controller-write webhook holds a migration Job's cleanup TTL to. A claim that
// never recorded the Job's UID records it first (adoptMigrationJob). A Warning
// names a Job admission would refuse a TTL (migrationJobCleanupNotScheduled). A
// read-only claim holds no Lease while it waits, and Progressing says what it
// waits for in the words of waiting.
func (r *MigrationReconciler) letGoOfReadOnlyMigrationJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	waiting string,
) (result ctrl.Result, held bool, err error) {
	mayWait := !migration.Spec.Suspend
	release := func() (readOnlyJobRelease, error) {
		verdict, _ := mutationlifecycle.VerdictFor(migrationJobClaim(migration, job))
		return releaseReadOnlyJob(job, verdict, validateMigrationJobEnvelope(job, migration), r.now(), mayWait)
	}
	step, reason := release()
	if step == recordReadOnlyJob {
		if _, err := r.adoptMigrationJob(ctx, migration, job); err != nil {
			return ctrl.Result{}, false, fmt.Errorf("record the %s Job before its cleanup: %w",
				migration.Status.ActiveOperation.Type, err)
		}
		step, reason = release()
	}
	operation := migration.Status.ActiveOperation
	switch step {
	case waitForReadOnlyJob:
		result, err := r.waitForReadOnlyMigrationJob(ctx, migration, waiting)
		return result, true, err
	case collectReadOnlyJob:
		return ctrl.Result{}, false, r.settleMigrationJobCleanupAnswer(migration, operation, job, r.markJobHarvested(ctx, job))
	case leaveReadOnlyJobUncollected:
		r.migrationJobCleanupNotScheduled(migration, operation, job, reason)
	}
	return ctrl.Result{}, false, nil
}

// waitForReadOnlyMigrationJob keeps the claim over the running Job it owes a
// cleanup, and says so in Progressing with waiting. The words name the Job and
// nothing that changes from one pass to the next, so a pass that finds them
// already written writes nothing (patchMigrationStatus).
func (r *MigrationReconciler) waitForReadOnlyMigrationJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	waiting string,
) (ctrl.Result, error) {
	before := migration.DeepCopy()
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		operatorv1alpha1.ReasonOperationInProgress, waiting)
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: activeJobPollInterval(false)}, nil
}

// migrationJobClaim describes the Job standing under the name the active claim
// reserved, or nil where nothing stands there, to the decision about what a
// pass does with it (mutationlifecycle.VerdictFor).
func migrationJobClaim(migration *operatorv1alpha1.PtahMigration, job *batchv1.Job) mutationlifecycle.JobClaim {
	operation := migration.Status.ActiveOperation
	claim := mutationlifecycle.JobClaim{
		Mutating:        migrationOperation(operation).Mutating,
		DispatchStarted: operation.DispatchStarted,
		RecordedJobUID:  string(operation.JobUID),
	}
	if job != nil {
		claim.Found = true
		claim.FoundJobUID = string(job.UID)
		claim.OwnedExactly = exactControllerOwner(job.OwnerReferences,
			operatorv1alpha1.GroupVersion.String(), "PtahMigration", migration.Name, migration.UID)
	}
	return claim
}

// adoptMigrationJob records job as the Job the active claim dispatched, where
// the claim never recorded one: the pass that created the Job stopped, or lost
// the create's answer, before it wrote the UID. It reports whether it recorded
// the Job, and writes nothing for a Job the claim cannot adopt.
//
// The claim adopts only the Job mutationlifecycle.VerdictFor gives
// VerdictAdopt: one this resource owns exactly as controller, under the name the
// claim reserved, while the claim names no UID. Owning it is not enough. The
// controller-write webhook holds only the manager's own creates to the claim
// (validateMigrationJobCreate), and a Job anyone else created under that name is
// held to nothing, so every caller first holds the Job to what the claim fixes. A
// pass that supervises the Job does that in holdMigrationJobToItsClaim, and a pass
// that settles an Apply nobody accounted for in unrecordedMigrationJobRefusal,
// with the match admission applies when it admits the Job's cleanup TTL. A Job
// that passes is the one this claim's dispatch created, whichever release built
// it.
//
// Every claim records its Job's UID here or in the pass that created the Job, and
// only once. So an Apply is also recorded as dispatched here and counted as
// started, the transition the creating pass would have made.
//
// The UID is what the rest of the claim's life reads. A pass that supervises the
// Job binds to it, the pass that asks whether the Job can still write lists the
// Pods it owned, and admission admits a cleanup TTL only on the Job a claim names
// by UID. A run this claim already recorded unknown without naming its Job, as
// when the pass that recorded it could not read the name, names the Job in the
// same write (nameRecordedMigrationRun), so the record points at the log the TTL
// keeps.
func (r *MigrationReconciler) adoptMigrationJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
) (bool, error) {
	if verdict, _ := mutationlifecycle.VerdictFor(migrationJobClaim(migration, job)); verdict != mutationlifecycle.VerdictAdopt {
		return false, nil
	}
	before := migration.DeepCopy()
	operation := migration.Status.ActiveOperation
	mutating := migrationOperation(operation).Mutating
	operation.JobUID = job.UID
	if mutating {
		operation.DispatchStarted = true
	}
	nameRecordedMigrationRun(migration, operation, job)
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return false, err
	}
	if mutating && r.Telemetry != nil {
		r.Telemetry.ObserveApply(telemetry.FamilyMigration, telemetry.ApplyStarted)
	}
	return true, nil
}

// nameRecordedMigrationRun names job in the records of the claim's run that name
// no Job: status.lastRun and status.unresolvedRun, where either is the record of
// operation. A record of another run, or one that names a Job, is left as it is.
// The copy of the unresolved run on metadata follows the record in the same
// status write (patchMigrationStatus).
func nameRecordedMigrationRun(
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
	job *batchv1.Job,
) {
	if run := migration.Status.LastRun; run != nil && run.OperationID == operation.ID &&
		run.JobName == "" && run.JobUID == "" {
		run.JobName, run.JobUID = job.Name, job.UID
		if run.DispatchedBy == nil {
			run.DispatchedBy = dispatcherRecord(job)
		}
	}
	if run := migration.Status.UnresolvedRun; run != nil && run.OperationID == operation.ID &&
		run.JobName == "" && run.JobUID == "" {
		run.JobName, run.JobUID = job.Name, job.UID
		if run.DispatchedBy == nil {
			run.DispatchedBy = dispatcherRecord(job)
		}
	}
}

// expectedMigrationJob is the Job the active claim builds now: the plan an
// Apply names read again, and the Job built from it and the claim.
func (r *MigrationReconciler) expectedMigrationJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
) (*batchv1.Job, error) {
	if r.Jobs == nil {
		return nil, errors.New("Job builder is not configured")
	}
	plan, err := r.migrationPlanForJob(ctx, migration, operation)
	if err != nil {
		return nil, err
	}
	return r.Jobs.BuildMigration(migration, *operation, plan)
}

// migrationPlanForJob re-reads the immutable plan an Apply claim named. Every
// other migration operation carries none, and gets nil.
//
// The Job the builder assembles carries that plan's identity into the runner,
// which refuses to open the database without it, so the plan is read here
// rather than remembered from the pass that claimed the Apply.
func (r *MigrationReconciler) migrationPlanForJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	operation *operatorv1alpha1.MigrationOperationStatus,
) (*operatorv1alpha1.PtahMigrationPlan, error) {
	if operation.Type != operatorv1alpha1.MigrationOperationApply {
		return nil, nil
	}
	if operation.PlanRef == nil || operation.PlanRef.Name == "" || operation.PlanRef.UID == "" {
		return nil, errors.New("the Apply claim names no immutable plan")
	}
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: operation.PlanRef.Name}
	if err := r.directReader().Get(ctx, key, plan); err != nil {
		return nil, fmt.Errorf("read the Apply plan: %w", err)
	}
	if plan.UID != operation.PlanRef.UID || plan.DeletionTimestamp != nil {
		return nil, errors.New("the Apply plan was replaced or is being deleted")
	}
	return plan, nil
}

func (r *MigrationReconciler) consumeMigrationResult(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	result runner.Result,
) (ctrl.Result, error) {
	operation := migration.Status.ActiveOperation
	before := migration.DeepCopy()
	// The history document a plan is computed from, held until the status it
	// was read into has been persisted. Nothing else may set it.
	var pendingPlanReport *dataplane.MigrationStatusReport
	switch operation.Type {
	case operatorv1alpha1.MigrationOperationResolve:
		if git := migration.Spec.Artifact.Git; git != nil {
			revision, err := gitResolution(git, result)
			if err != nil {
				return r.retryMigrationOperation(ctx, migration, job, err)
			}
			policy, policyBinding, err := sourcePolicyFor(ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact)
			if err == nil {
				err = checkGitSource(policy, git, result.ResolvedContentBytes)
			}
			if err != nil {
				if errors.Is(err, errSourceRefused) {
					return r.retryMigrationOperation(ctx, migration, job, err)
				}
				return ctrl.Result{}, err
			}
			migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{Git: sourcebinding.GitAccess(*git, revision)}
			migration.Status.ConfigMap = nil
			migration.Status.SourcePolicy = policyBinding
			// A Git source has no verification policy, so there is no Verify
			// step: the history is read next.
			migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
			meta.RemoveStatusCondition(&migration.Status.Conditions, operatorv1alpha1.ConditionMigrationArtifactVerified)
			endMigrationRefusal(migration, operatorv1alpha1.ReasonCommitResolved, migrationSourceResolvedMessage)
			setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
				operatorv1alpha1.ReasonCommitResolved,
				fmt.Sprintf("The Git ref resolved to commit %s; the history is read next", revision.Commit))
			break
		}
		if !sha256DigestPattern.MatchString(result.ResolvedDigest) || result.ResolvedReference == "" {
			return r.retryMigrationOperation(ctx, migration, job, errors.New("the resolve result carries no immutable reference"))
		}
		if _, err := ocireference.Parse(result.ResolvedReference); err != nil {
			return r.retryMigrationOperation(ctx, migration, job, errors.New("the resolved reference is not a credential-free OCI reference"))
		}
		// A refusal is an answer about one artifact. Another digest under the
		// reference is another artifact, whatever Verify goes on to say of it.
		moved := migration.Status.Artifact != nil && migration.Status.Artifact.Digest != result.ResolvedDigest
		migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{
			ResolvedReference: result.ResolvedReference,
			Digest:            result.ResolvedDigest,
			RegistryAuthFrom:  migration.Spec.Artifact.RegistryAuthFrom.DeepCopy(),
		}
		// An OCI source reads no ConfigMap and takes no source policy, so
		// what an earlier ConfigMap or Git source recorded goes with it.
		migration.Status.ConfigMap = nil
		migration.Status.SourcePolicy = nil
		migration.Spec.Artifact.Transport.DeepCopyInto(&migration.Status.Artifact.Transport)
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseVerifying
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationArtifactVerified, metav1.ConditionUnknown,
			operatorv1alpha1.ReasonDigestPinned, "The artifact resolved to a digest and has not been verified yet")
		if moved {
			endMigrationRefusal(migration, operatorv1alpha1.ReasonDigestPinned,
				"The reference resolved to another digest, and nothing has refused that artifact")
		}
	case operatorv1alpha1.MigrationOperationVerify:
		if result.ObservedArtifactType != dataplane.MigrationArtifactType {
			return r.retryMigrationOperation(ctx, migration, job,
				fmt.Errorf("the verified artifact is %q, not a migration artifact", bounded(result.ObservedArtifactType, 128)))
		}
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationArtifactVerified, metav1.ConditionTrue,
			operatorv1alpha1.ReasonPolicySatisfied, "The resolved artifact satisfied its verification policy")
		endMigrationRefusal(migration, operatorv1alpha1.ReasonPolicySatisfied,
			"The resolved artifact satisfied its verification policy, and nothing refuses it")
	case operatorv1alpha1.MigrationOperationHistory:
		if result.MigrationHistory == nil {
			return r.retryMigrationOperation(ctx, migration, job, errors.New("the history result carries no status document"))
		}
		if !sha256DigestPattern.MatchString(result.TargetIdentityDigest) {
			return r.retryMigrationOperation(ctx, migration, job, errors.New("the history result carries no target identity"))
		}
		if err := r.recordMigrationHistory(migration, *result.MigrationHistory, result.TargetIdentityDigest); err != nil {
			return r.retryMigrationOperation(ctx, migration, job, err)
		}
		recordMigrationRunVerification(migration)
		setMigrationTargetOpened(migration, "The migration history was read")
		// The plan is published after this status is persisted, not here. A
		// plan names the evidence it was computed from, and the admission
		// guard re-derives it from the migration's own stored status: a plan
		// created from a status that exists only in this process references
		// evidence nobody else can see, and the guard refuses it -- which is
		// the guard being right.
		if migration.Status.Phase == operatorv1alpha1.MigrationPhasePlanning {
			pendingPlanReport = result.MigrationHistory
		} else {
			migration.Status.Plan = nil
			endMigrationApprovalRequirement(migration)
		}
	default:
		return r.retryMigrationOperation(ctx, migration, job, fmt.Errorf("unsupported migration operation %q", operation.Type))
	}
	// Intermediate steps are due now, unless the cycle carries a later deadline
	// for the next refresh. Keep the deadline in the same status write that
	// retires the claim so the handoff remains measurable.
	if migration.Status.Phase == operatorv1alpha1.MigrationPhaseVerifying ||
		migration.Status.Phase == operatorv1alpha1.MigrationPhaseReading {
		migration.Status.NextReconciliationTime = handoffDeadline(migration.Status.NextReconciliationTime, metav1.NewTime(r.now()))
	} else if migration.Status.Phase == operatorv1alpha1.MigrationPhaseInSync ||
		migration.Status.Phase == operatorv1alpha1.MigrationPhaseBlocked {
		next := cycleDeadline(migration.Status.NextReconciliationTime, r.now(), migrationInterval(migration))
		migration.Status.NextReconciliationTime = &next
	} else {
		// A plan about to be published waits a full interval for its approval,
		// so the approver's window is not shortened by the cycle's own length.
		next := metav1.NewTime(r.now().Add(migrationInterval(migration)))
		migration.Status.NextReconciliationTime = &next
	}
	if err := r.harvestMigrationJob(ctx, migration, job); err != nil {
		return ctrl.Result{}, err
	}
	// A reading that leaves nothing to apply, or a history nothing may run
	// against, leaves no plan for an approval to name. The plan can still come
	// back, and a decision does not wait for it.
	if operation.Type == operatorv1alpha1.MigrationOperationHistory && migration.Status.Phase != operatorv1alpha1.MigrationPhasePlanning {
		if err := r.markMigrationApprovalsStale(ctx, migration, nil, operatorv1alpha1.ReasonPlanNoLongerCurrent, migrationPlanLeftMessage); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.retireMigrationClaim(ctx, before, migration, mutationlifecycle.DispositionAccounted, false); err != nil {
		return ctrl.Result{}, err
	}
	if operation.Type == operatorv1alpha1.MigrationOperationResolve {
		// A Git or OCI resolution names no snapshot, so one a ConfigMap
		// source left goes now that the status no longer names it.
		r.retireMigrationSnapshots(ctx, migration)
	}
	r.observeMigrationOperation(operation, telemetry.OperationSucceeded)
	if pendingPlanReport != nil {
		// A second write, deliberately. Between the two the resource is
		// Planning with Progressing reason MigrationsPending and the plan it had
		// before, and a controller that stopped there reads again on the next
		// pass (migrationPublicationUnfinished) rather than acting on a plan
		// nobody published or waiting out an interval with the gate closed.
		planned := migration.DeepCopy()
		if err := r.publishMigrationPlan(ctx, migration, *pendingPlanReport); err != nil {
			return r.migrationOperationFailure(ctx, migration, err)
		}
		// Before the status names the plan, for the reason a refusal does. A pass
		// that stops here finds the old plan's evidence moved and reads again.
		if err := r.markMigrationApprovalsStale(ctx, migration, migration.Status.Plan, operatorv1alpha1.ReasonPlanNoLongerCurrent, migrationPlanLeftMessage); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.patchMigrationStatus(ctx, planned, migration); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// refuseMigrationArtifact records an artifact its verification policy refused.
// Another attempt reads the same answer from the same artifact and policy, so
// the resource stops at Blocked with the requirements the policy named, and
// starts the read-only chain again from resolution at its interval, when a moved
// tag or a changed policy can give another. No plan outlives the refusal, and
// no decision does: the same artifact accepted later publishes the same plan
// under the same fingerprint, so an approval left standing would run it with
// nobody having looked again.
//
// The harvest compared the claim's inputs a moment ago, so a policy read here
// that differs from the result's digest is a policy that just moved, and the
// claim's retry reads it.
//
// The approvals go before the status that says why. A pass that stops between
// the two harvests this Job again and finds them stale already; in the other
// order a resource reads as refused with a decision still good.
func (r *MigrationReconciler) refuseMigrationArtifact(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	result runner.Result,
) (ctrl.Result, error) {
	binding, err := policy.ConfigMapBinding(ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact.VerificationPolicyFrom)
	if err != nil || migration.Status.Artifact == nil ||
		!verificationRefusalHolds(result, migration.Status.Artifact.Digest, binding.Digest) {
		return r.retryMigrationOperation(ctx, migration, job, errVerificationRefusalEvidence)
	}
	if err := r.harvestMigrationJob(ctx, migration, job); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.markMigrationApprovalsStale(ctx, migration, nil, operatorv1alpha1.ReasonPolicyRefused,
		"The verification policy refused the artifact after this approval was written, so a plan published after the refusal needs a new approval"); err != nil {
		return ctrl.Result{}, err
	}
	operation := migration.Status.ActiveOperation
	before := migration.DeepCopy()
	next := cycleDeadline(migration.Status.NextReconciliationTime, r.now(), migrationInterval(migration))
	migration.Status.NextReconciliationTime = &next
	migration.Status.ObservedGeneration = migration.Generation
	migration.Status.Plan = nil
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
	message := bounded(fmt.Sprintf("Verification policy refused the artifact with %d finding(s): %s",
		len(result.VerificationRequirements), strings.Join(result.VerificationRequirements, ", ")), 512)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationArtifactVerified, metav1.ConditionFalse,
		operatorv1alpha1.ReasonPolicyRefused, message)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
		operatorv1alpha1.ReasonPolicyRefused, message)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonPolicyRefused, "Artifact verification policy must be satisfied before database access")
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
		operatorv1alpha1.ReasonPolicyRefused, "Nothing runs while the verification policy refuses the artifact")
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse,
		operatorv1alpha1.ReasonPolicyRefused, "No plan may be used for an artifact refused by policy")
	if err := r.retireMigrationClaim(ctx, before, migration, mutationlifecycle.DispositionAccounted, false); err != nil {
		return ctrl.Result{}, err
	}
	r.observeMigrationOperation(operation, telemetry.OperationSucceeded)
	r.event(migration, corev1.EventTypeWarning, "ArtifactVerificationRefused", "%s", message)
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// migrationPlanLeftMessage is what an approval says once a reading ended somewhere
// other than the plan it names.
const migrationPlanLeftMessage = "A reading of the database ended somewhere other than the plan this approval names, so that plan needs a new approval if it is published again"

// migrationSourceResolvedMessage is what a refusal's conditions say once a Git
// or ConfigMap source was read: neither has a verification policy to refuse it.
const migrationSourceResolvedMessage = "The source was read, and nothing refuses it"

// endMigrationRefusal takes the verification policy's refusal off the
// conditions it was written on, once the read-only chain got past it: the
// artifact passed Verify, or the source is another one. Nothing later does it.
// The history read rewrites Blocked and Ready only when it answers, so a read
// that keeps failing would leave a refusal named beside a verified artifact.
func endMigrationRefusal(
	migration *operatorv1alpha1.PtahMigration,
	reason operatorv1alpha1.ConditionReason,
	message string,
) {
	liftMigrationRefusal(migration, []operatorv1alpha1.ConditionReason{operatorv1alpha1.ReasonPolicyRefused}, reason, message)
}

// endMigrationRealmRefusal takes a realm's refusal off the conditions, in the
// write that follows a census that admits the resource: the claim of the read it
// lets through, a ConfigMap source's read, or that read's failure before any claim
// (claimRefused). A refusal of the artifact ends later, at Verify, because only
// Verify answers it; a realm's is over once the census passes, and a read that
// keeps failing would otherwise leave a lifted realm named where a wait's timeout
// and kubectl ptah migration read it.
func endMigrationRealmRefusal(migration *operatorv1alpha1.PtahMigration, reason operatorv1alpha1.ConditionReason, message string) {
	liftMigrationRefusal(migration,
		[]operatorv1alpha1.ConditionReason{operatorv1alpha1.ReasonRealmConflict, operatorv1alpha1.ReasonRealmNotAuthorized},
		reason, message)
}

// migrationRealmReadMessage is what a lifted realm's conditions say once the read
// the census let through is claimed.
const migrationRealmReadMessage = "The database realm admits this resource again, and it is read again"

// liftMigrationRefusal rewrites Blocked, Ready and ApprovalRequired where they
// carry one of the refusal's reasons. A condition with any other reason belongs
// to whoever set it: a plan waiting for a person keeps ApprovalRequired True
// through the read that refreshes it. Progressing is not here because the claim
// of that read rewrote it.
//
// A run nobody accounted for is a hold the refusal wrote over, and still holds:
// nothing runs while status.unresolvedRun stands, so Blocked says so again rather
// than False.
func liftMigrationRefusal(
	migration *operatorv1alpha1.PtahMigration,
	refusals []operatorv1alpha1.ConditionReason,
	reason operatorv1alpha1.ConditionReason,
	message string,
) {
	held := func(conditionType string) bool {
		condition := meta.FindStatusCondition(migration.Status.Conditions, conditionType)
		return condition != nil && slices.Contains(refusals, operatorv1alpha1.ConditionReason(condition.Reason))
	}
	unresolved := migration.Status.UnresolvedRun
	if held(operatorv1alpha1.ConditionMigrationBlocked) {
		if unresolved != nil {
			setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
				operatorv1alpha1.ReasonApplyOutcomeUnknown, unresolvedRunHoldMessage(unresolved))
		} else {
			setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse, reason, message)
		}
	}
	if held(operatorv1alpha1.ConditionMigrationReady) {
		if unresolved != nil {
			setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
				operatorv1alpha1.ReasonApplyOutcomeUnknown, unresolvedRunReadyMessage)
		} else {
			setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, reason, message)
		}
	}
	if held(operatorv1alpha1.ConditionMigrationApprovalRequired) {
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, reason, message)
	}
}

// endMigrationApprovalRequirement is a reading that publishes no plan: nothing
// is left to approve, whatever ApprovalRequired said. A plan that waited for a
// person would otherwise leave it True beside Ready True, and the next plan would
// count no requirement, since only a transition to True counts one. It takes
// the reason Ready carries, which the reading just wrote, and it writes nothing
// on a resource that never had the condition.
func endMigrationApprovalRequirement(migration *operatorv1alpha1.PtahMigration) {
	if meta.FindStatusCondition(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationApprovalRequired) == nil {
		return
	}
	reason := operatorv1alpha1.ReasonPlanNoLongerCurrent
	if ready := meta.FindStatusCondition(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationReady); ready != nil {
		reason = operatorv1alpha1.ConditionReason(ready.Reason)
	}
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse,
		reason, "This reading published no plan, so nothing waits for an approval")
}

// unresolvedRunReadyMessage is what Ready says while a run nobody accounted for
// stands.
const unresolvedRunReadyMessage = "What the last run did is unknown until the database is read by a person"

// unresolvedRunHoldMessage is what Blocked says while a run nobody accounted for
// stands and no reading has said more.
func unresolvedRunHoldMessage(unresolved *operatorv1alpha1.UnresolvedMigrationRunStatus) string {
	return fmt.Sprintf("A %s run nobody accounted for is recorded against this database; "+
		"establish what it did before another Apply", unresolved.Outcome)
}

// migrationUnresolvedRunSettledBy reports that this reading is the proof the
// unresolved run needed: the same database, with nothing of this artifact left
// to apply.
//
// A record that names no database cannot be contradicted by one, so any reading
// with nothing pending settles it. Requiring a match against an identity nobody
// recorded would latch the resource with no way out.
//
// Nothing pending is a statement about the artifact resolved today, which is
// why an artifact pointed at a shorter sequence is not a way around this: the
// database being ahead of the artifact is its own refusal, taken before the
// branch that removes the record, so a tag moved back blocks rather than
// settles. What it cannot separate is a sequence the migration was taken out
// of -- the documented recovery for a run that half-applied one -- from the
// same gesture without the repair, because the revision table does not record
// the half that was committed.
func migrationUnresolvedRunSettledBy(
	unresolved *operatorv1alpha1.UnresolvedMigrationRunStatus,
	history *operatorv1alpha1.MigrationHistoryStatus,
	pending []int64,
) bool {
	if len(pending) > 0 {
		return false
	}
	if unresolved == nil || unresolved.TargetIdentityDigest == "" {
		return true
	}
	return history != nil && unresolved.TargetIdentityDigest == history.TargetIdentityDigest
}

// migrationsPending starts a sentence about the pending count.
func migrationsPending(count int) string {
	return fmt.Sprintf("%d %s pending", count, byCount(count, "migration is", "migrations are"))
}

// recordMigrationHistory turns the database's own account into status. The
// classification is the database's, not this controller's: a dirty row and a
// modified applied migration are refusals Ptah reported, and neither is ever
// resolved by reconciling again.
func (r *MigrationReconciler) recordMigrationHistory(
	migration *operatorv1alpha1.PtahMigration,
	report dataplane.MigrationStatusReport,
	targetIdentityDigest string,
) error {
	historyFingerprint, err := migrationplan.HistoryFingerprint(report)
	if err != nil {
		return err
	}
	pending := report.Pending()
	modified := report.Modified()
	outOfOrder := report.OutOfOrder()
	artifactVersion := report.LastVersion()
	// The record of a run nobody accounted for decides whether a pending
	// migration may be planned at all, and it is the only thing that decides
	// it. The conditions below rewrite whatever reason the resource carried;
	// the record is somewhere a reason cannot reach.
	unresolved := migration.Status.UnresolvedRun
	history := &operatorv1alpha1.MigrationHistoryStatus{
		ObservedAt:           metav1.NewTime(r.now()),
		ContractVersion:      int32(report.ContractVersion),
		CurrentVersion:       report.CurrentVersion,
		CheckpointVersion:    report.CheckpointVersion,
		Fingerprint:          historyFingerprint,
		TargetIdentityDigest: targetIdentityDigest,
		PendingCount:         int32(len(pending)),
		Dirty:                report.DirtyRevision != nil,
	}
	for _, record := range report.Migrations {
		if record.State == dataplane.MigrationStateApplied || record.State == dataplane.MigrationStateCheckpointCovered {
			history.AppliedCount++
		}
	}
	if len(modified) > 0 {
		history.ModifiedVersions = boundedVersions(modified, 64)
	}
	if len(outOfOrder) > 0 {
		history.OutOfOrderVersions = boundedVersions(outOfOrder, 64)
	}
	history.Revisions = revisionRows(report.Migrations)
	migration.Status.History = history

	switch {
	case history.Dirty:
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
			operatorv1alpha1.ReasonHistoryDirty,
			fmt.Sprintf("Revision %d is recorded dirty; a person has to decide what the interrupted run did", report.DirtyRevision.Version))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryDirty, "The revision table holds a dirty row")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryDirty, "Nothing runs while a revision is recorded dirty")
	case len(modified) > 0:
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
			operatorv1alpha1.ReasonHistoryModified,
			fmt.Sprintf(
				"%d %s the database recorded, or publish an artifact whose history matches it",
				len(modified), byCount(len(modified),
					"applied migration no longer matches its file; restore the file",
					"applied migrations no longer match their files; restore the files"),
			))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryModified, "An applied migration was modified after it ran")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryModified, "Nothing runs while an applied migration no longer matches its file")
	case len(outOfOrder) > 0:
		// Ptah executes in linear order and refuses the whole run while a
		// pending migration sorts below the current version. Publishing a plan
		// for it would ask a person to approve a sequence the executor cannot
		// run, and the refusal would arrive as a failed Job instead of as the
		// answer it is.
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
			operatorv1alpha1.ReasonHistoryOutOfOrder,
			fmt.Sprintf(
				"%[1]d %[2]s below applied version %[3]d; linear execution refuses %[4]s, so renumber %[4]s above that version "+
					"or publish %[4]s to a database that has not passed that version",
				len(outOfOrder), byCount(len(outOfOrder), "migration sorts", "migrations sort"), report.CurrentVersion,
				byCount(len(outOfOrder), "it", "them"),
			))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryOutOfOrder, "A migration arrived below the version the database has applied")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryOutOfOrder, "Nothing runs while a migration sorts below the applied version")
	// The database records work this artifact has never heard of. Nothing is
	// pending, because pending is a statement about the artifact's own
	// migrations, and a revision the artifact does not carry is in no state at
	// all -- which is exactly how this used to read as success. The operator
	// will not roll a database back to match an older artifact, and which of
	// the two is wrong is a question for whoever moved the tag.
	// A run whose outcome nobody could read may have executed the migration
	// that is pending now. Planning it again is the blind replay the versioned
	// workflow exists to refuse: re-running a file that may have committed
	// would run it twice, and the history cannot say which.
	//
	// A reading of that same database with nothing pending is the read-only
	// proof that settles it, and it takes the branch below, which removes the
	// record. Anything else -- work still pending, or a reading of a database
	// this run never addressed -- waits for a person, exactly as the run's own
	// condition said it would.
	case unresolved != nil && !migrationUnresolvedRunSettledBy(unresolved, history, pending):
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
		refusal := fmt.Sprintf("%s and the last run's outcome was %s, so %s run again",
			migrationsPending(len(pending)), unresolved.Outcome, byCount(len(pending), "it may not", "none may"))
		if len(pending) == 0 {
			refusal = fmt.Sprintf(
				"Nothing is pending here, but this is a different database from the one the %s run addressed, "+
					"so it does not say what that run did",
				unresolved.Outcome)
		}
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
			operatorv1alpha1.ReasonApplyOutcomeUnknown, refusal)
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonApplyOutcomeUnknown, unresolvedRunReadyMessage)
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonApplyOutcomeUnknown, "The dispatched run is over and may not be retried")
	case report.CurrentVersion > artifactVersion:
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
			operatorv1alpha1.ReasonHistoryAhead,
			fmt.Sprintf(
				"The database is at version %d and this artifact ends at version %d; publish an artifact that carries "+
					"the versions the database already applied, because nothing here rolls it back",
				report.CurrentVersion, artifactVersion))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryAhead, "The database records a migration this artifact does not carry")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryAhead, "Nothing runs while the database is ahead of the artifact")
	case len(pending) == 0:
		// The one reading that settles an unresolved run: this database has
		// every migration the artifact carries, so nothing is left for that run
		// to have half-done. It is the only place a reading removes the record,
		// and it sits after the refusals above -- a database ahead of its
		// artifact never reaches it. The other way a record goes is a person's
		// acknowledgment, in reconcileRunAcknowledgments.
		if unresolved != nil {
			migration.Status.ResolvedRun = &operatorv1alpha1.ResolvedMigrationRunStatus{
				OperationID: unresolved.OperationID,
				Outcome:     unresolved.Outcome,
				Resolution:  operatorv1alpha1.MigrationRunResolvedByHistoryRead,
				ResolvedAt:  history.ObservedAt,
			}
		}
		migration.Status.UnresolvedRun = nil
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseInSync
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryMatched, "The history continues this artifact")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionTrue,
			operatorv1alpha1.ReasonHistoryMatched, "The database has every migration this artifact carries")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryMatched, "Nothing is pending")
	default:
		migration.Status.Phase = operatorv1alpha1.MigrationPhasePlanning
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse,
			operatorv1alpha1.ReasonHistoryMatched, "The history continues this artifact")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonMigrationsPending,
			migrationsPending(len(pending)))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
			operatorv1alpha1.ReasonMigrationsPending,
			fmt.Sprintf("A plan for %d pending %s is being published", len(pending), byCount(len(pending), "migration", "migrations")))
	}
	return nil
}

// publishMigrationPlan writes the immutable manifest of the pending sequence
// and records it. The plan's name is derived from its fingerprint, so
// publishing the same decision twice is the same object rather than a second
// copy of one decision.
func (r *MigrationReconciler) publishMigrationPlan(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	report dataplane.MigrationStatusReport,
) error {
	binding := migration.Status.ExecutionBinding
	history := migration.Status.History
	if binding == nil || history == nil || migration.Status.Artifact == nil {
		return errors.New("a plan needs a resolved artifact, a read history, and an execution binding")
	}
	planned, err := migrationplan.Sequence(report)
	if err != nil {
		return fmt.Errorf("select the pending sequence: %w", err)
	}
	sequenceDigest, err := migrationplan.SequenceDigest(planned)
	if err != nil {
		return err
	}
	artifactDigest, source := sourcebinding.MigrationPlanSource(migration.Status.Artifact, migration.Status.ConfigMap, migration.Status.SourcePolicy)
	var policyBinding policy.Binding
	if source == nil {
		// Only an OCI source has a verification policy to bind.
		policyBinding, err = policy.ConfigMapBinding(
			ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact.VerificationPolicyFrom,
		)
		if err != nil {
			return err
		}
	}
	coordinationDigest, err := coordination.Digest(migration.Namespace, migration.Spec.Target)
	if err != nil {
		return fmt.Errorf("derive coordination digest: %w", err)
	}
	policyFingerprint, err := migrationPolicyFingerprint(migration)
	if err != nil {
		return err
	}
	if r.Jobs == nil {
		return errors.New("the Job builder is not configured")
	}
	controllerImage, controllerRevision, runnerImage := r.Jobs.ManagerIdentity()
	planBinding := migrationplan.Binding{
		MigrationUID:             migration.UID,
		HistoryFingerprint:       history.Fingerprint,
		SequenceDigest:           sequenceDigest,
		ArtifactDigest:           artifactDigest,
		Source:                   sourcebinding.Fingerprint(source),
		CoordinationDigest:       coordinationDigest,
		TargetIdentityDigest:     history.TargetIdentityDigest,
		PolicyFingerprint:        policyFingerprint,
		VerificationPolicyUID:    policyBinding.UID,
		VerificationPolicyDigest: policyBinding.Digest,
		ExecutionBindingID:       binding.Epoch,
		ControllerStateVersion:   binding.ControllerStateVersion,
		PtahVersion:              binding.PtahVersion,
		ExecutorImage:            binding.ExecutorImage,
		RunnerProtocolVersion:    binding.RunnerProtocolVersion,
	}
	planFingerprint, err := planBinding.Fingerprint()
	if err != nil {
		return fmt.Errorf("derive the plan fingerprint: %w", err)
	}
	desired, err := migrationplan.Desired(migration, operatorv1alpha1.PtahMigrationPlanSpec{
		ContractVersion:          planBinding.ContractVersion(),
		MigrationRef:             operatorv1alpha1.ImmutableObjectReference{Name: migration.Name, UID: migration.UID},
		Fingerprint:              planFingerprint,
		Migrations:               planned,
		HistoryFingerprint:       history.Fingerprint,
		CurrentVersion:           history.CurrentVersion,
		ArtifactDigest:           planBinding.ArtifactDigest,
		Source:                   source,
		CoordinationDigest:       planBinding.CoordinationDigest,
		TargetIdentityDigest:     planBinding.TargetIdentityDigest,
		PolicyFingerprint:        policyFingerprint,
		VerificationPolicyUID:    policyBinding.UID,
		VerificationPolicyDigest: policyBinding.Digest,
		ExecutionBindingID:       binding.Epoch,
		ControllerImage:          controllerImage,
		ControllerRevision:       controllerRevision,
		ControllerStateVersion:   binding.ControllerStateVersion,
		PtahVersion:              binding.PtahVersion,
		ExecutorImage:            binding.ExecutorImage,
		RunnerImage:              runnerImage,
		RunnerProtocolVersion:    binding.RunnerProtocolVersion,
		CreatedAt:                metav1.NewTime(r.now()),
	})
	if err != nil {
		return err
	}
	published := desired.DeepCopy()
	if err := r.Client.Create(ctx, published); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("publish the migration plan: %w", err)
		}
		existing := &operatorv1alpha1.PtahMigrationPlan{}
		key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
		if err := r.directReader().Get(ctx, key, existing); err != nil {
			return fmt.Errorf("read the already published migration plan: %w", err)
		}
		if existing.Spec.Fingerprint != planFingerprint || existing.Spec.MigrationRef.UID != migration.UID {
			return errors.New("a different plan already holds this plan's deterministic name")
		}
		published = existing
	}
	migration.Status.Plan = &operatorv1alpha1.ImmutableObjectReference{Name: published.Name, UID: published.UID}
	r.applyMigrationPolicyPhase(migration, len(planned))
	return nil
}

// applyMigrationPolicyPhase reports what the policy says happens next. Nothing
// here executes: the phase and the conditions are what an operator reads, and
// what the approval an operator writes is judged against.
func (r *MigrationReconciler) applyMigrationPolicyPhase(migration *operatorv1alpha1.PtahMigration, planned int) {
	switch migration.Spec.Policy.Apply {
	case operatorv1alpha1.ApplyPolicyNever:
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse,
			operatorv1alpha1.ReasonNotRequired, "The apply policy is Never, so no approval is consulted")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonApplyDisabled,
			migrationsPending(planned)+" and the apply policy is Never")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonApplyDisabled, "The apply policy is Never, so nothing runs from here")
	case operatorv1alpha1.ApplyPolicyAlways:
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse,
			operatorv1alpha1.ReasonNotRequired, "The apply policy is Always, so no approval is required")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonApplyPending,
			fmt.Sprintf("%d %s planned", planned, byCount(planned, "migration is", "migrations are")))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
			operatorv1alpha1.ReasonApplyPending,
			fmt.Sprintf("An Apply of %d planned %s is what happens next", planned, byCount(planned, "migration", "migrations")))
	default:
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseAwaitingApproval
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue,
			operatorv1alpha1.ReasonAwaitingApproval,
			fmt.Sprintf("%d planned %s an approval naming this plan", planned, byCount(planned, "migration needs", "migrations need")))
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
			operatorv1alpha1.ReasonAwaitingApproval, "The plan is waiting for the approval its policy requires")
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
			operatorv1alpha1.ReasonAwaitingApproval, "Nothing runs until a person writes the approval")
	}
}

// migrationPolicyFingerprint binds a plan to the terms it was planned under. A
// policy that changed after the plan was made invalidates it rather than
// executing under terms nobody approved.
func migrationPolicyFingerprint(migration *operatorv1alpha1.PtahMigration) (string, error) {
	return fingerprint.DigestCanonicalJSON(map[string]string{
		"apply":        string(migration.Spec.Policy.Apply),
		"lock_timeout": migration.Spec.Policy.LockTimeout.Duration.String(),
		// The mode decides how the run is wrapped, so a sequence approved under
		// one mode is not the same execution under another. Unset is carried as
		// the empty string and is its own value: it means the operator passes
		// no mode and Ptah chooses, which is a different run from one that
		// asked for "file" even where Ptah would have picked it.
		"transaction_mode": migration.Spec.Policy.TransactionMode,
	})
}

// migrationInputFingerprint is what the operation was decided from. An input
// that changed while the Job ran is what makes its result stale rather than
// wrong.
func (r *MigrationReconciler) migrationInputFingerprint(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	operationType operatorv1alpha1.MigrationOperationType,
) (string, error) {
	if migration.Spec.Artifact.ConfigMap == nil && migration.Spec.Artifact.Git == nil {
		if _, err := ocireference.Parse(migration.Spec.Artifact.OCIRef); err != nil {
			return "", errors.New("the artifact reference must be an OCI reference without credentials, whitespace, or query data")
		}
	}
	policyObject, policyKey := "", ""
	if selector := migration.Spec.Artifact.VerificationPolicyFrom; selector != nil {
		policyObject, policyKey = selector.Name, selector.Key
	}
	inputs := map[string]string{
		"operation":       string(operationType),
		"generation":      strconv.FormatInt(migration.Generation, 10),
		"oci_ref":         migration.Spec.Artifact.OCIRef,
		"engine":          string(migration.Spec.Target.Engine),
		"target_secret":   migration.Spec.Target.URLFrom.Name,
		"target_key":      migration.Spec.Target.URLFrom.Key,
		"policy_object":   policyObject,
		"policy_key":      policyKey,
		"plain_http":      strconv.FormatBool(migration.Spec.Artifact.Transport.PlainHTTP),
		"execution_id":    migrationBindingEpoch(migration),
		"lock_timeout":    migration.Spec.Policy.LockTimeout.Duration.String(),
		"connect_timeout": migration.Spec.Execution.ConnectTimeout.Duration.String(),
		// The Pod the claim names carries this mode, so it belongs among the
		// inputs the claim was decided from, beside lock_timeout. It refuses
		// nothing by itself: generation is in this map too, and the API server
		// bumps it on every spec edit, so an edited mode already retired an
		// undispatched claim before this entry existed.
		"transaction_mode": migration.Spec.Policy.TransactionMode,
	}
	if operationType != operatorv1alpha1.MigrationOperationResolve {
		if migration.Status.Artifact == nil {
			return "", errors.New("the artifact has not been resolved to a digest")
		}
		inputs["resolved_digest"] = migration.Status.Artifact.Digest
		// A ConfigMap source adds what it was read into; an OCI source adds
		// nothing, so its inputs are the ones computed before this source
		// existed.
		if snapshot := migration.Status.Artifact.Snapshot; snapshot != nil {
			inputs["source_snapshot"] = snapshot.Name
			inputs["source_snapshot_uid"] = string(snapshot.UID)
			inputs["source_content_digest"] = snapshot.ContentDigest
			if revision := migration.Status.ConfigMap; revision != nil {
				inputs["source_configmap_uid"] = string(revision.UID)
			}
		}
		// A source policy adds the policy the source was checked against.
		if policy := migration.Status.SourcePolicy; policy != nil {
			inputs["source_policy_uid"] = string(policy.UID)
			inputs["source_policy_digest"] = policy.Digest
		}
		// A Git source adds the commit it was resolved to and its content.
		if git := migration.Status.Artifact.Git; git != nil {
			inputs["source_git_url"] = git.URL
			inputs["source_git_path"] = git.Path
			inputs["source_git_commit"] = git.Commit
			inputs["source_content_digest"] = git.ContentDigest
		}
	}
	if git := migration.Spec.Artifact.Git; git != nil {
		// The repository, ref, path, credential and trust a Resolve Job is
		// built from: an edit to any of them makes a running Resolve's result
		// stale.
		source, err := fingerprint.DigestCanonicalJSON(git)
		if err != nil {
			return "", fmt.Errorf("digest the Git source: %w", err)
		}
		inputs["git_source"] = source
	}
	if operationType == operatorv1alpha1.MigrationOperationVerify {
		binding, err := policy.ConfigMapBinding(ctx, r.directReader(), migration.Namespace, migration.Spec.Artifact.VerificationPolicyFrom)
		if err != nil {
			return "", err
		}
		inputs["policy_uid"] = string(binding.UID)
		inputs["policy_digest"] = binding.Digest
	}
	return fingerprint.DigestCanonicalJSON(inputs)
}

// failedInitBoundary names the init container that ended the Pod before the
// runner could speak, and returns an empty string when none did.
//
// Without it every such failure reads as "result frame not found", which is
// also what a crashed runner, an evicted node and a truncated log produce. The
// one distinction a reader needs is whether the run failed before it began,
// and at which boundary: the runner was never installed, the source authority
// was refused, or the artifact never arrived. Each of those has a different
// answer, and none of them is a retry.
//
// The container's own words are deliberately not carried. The container that
// fetches an artifact holds registry credentials, so its output is not
// evidence this controller puts in a status; the name and the exit code are
// structured fields Kubernetes sets, and they are enough to say which boundary
// failed.
func failedInitBoundary(ctx context.Context, reader client.Reader, job *batchv1.Job) (string, error) {
	if job == nil {
		return "", nil
	}
	pods, err := podsOwnedByJob(ctx, reader, job.Namespace, job.Name, job.UID)
	if err != nil {
		return "", err
	}
	return initBoundaryOf(pods), nil
}

// initBoundaryOf is failedInitBoundary over Pods already listed.
func initBoundaryOf(pods []*corev1.Pod) string {
	for _, pod := range pods {
		for _, status := range pod.Status.InitContainerStatuses {
			terminated := status.State.Terminated
			if terminated == nil || terminated.ExitCode == 0 {
				continue
			}
			return fmt.Sprintf(
				"the %s step exited %d, so the run never started",
				status.Name, terminated.ExitCode,
			)
		}
	}
	return ""
}

// retryMigrationOperation advances to a fresh attempt with a fresh
// deterministic name. A Job that already exists is never reused: the claim it
// answered is the one that failed.
func (r *MigrationReconciler) retryMigrationOperation(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	failure error,
) (ctrl.Result, error) {
	return r.retryMigrationOperationAs(ctx, migration, job, operatorv1alpha1.ReasonOperationFailed, failure)
}

// retryMigrationOperationAs is retryMigrationOperation with the failure named
// on the condition a reader looks at.
func (r *MigrationReconciler) retryMigrationOperationAs(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	reason operatorv1alpha1.ConditionReason,
	failure error,
) (ctrl.Result, error) {
	if job != nil {
		if err := r.harvestMigrationJob(ctx, migration, job); err != nil {
			return ctrl.Result{}, err
		}
	}
	operation := migration.Status.ActiveOperation
	if operation == nil {
		// Unreachable in the normal dispatch: every caller of retryMigrationOperation
		// and retryMigrationOperationAs is already inside the ActiveOperation
		// handling reconcile's own guard sends a claimed resource to. Requeuing
		// rather than erroring keeps that true if it ever stops being true.
		return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
	}
	before := migration.DeepCopy()
	next := operation.DeepCopy()
	next.Attempt = operation.Attempt + 1
	next.JobUID = ""
	next.AdmissionSnapshot = nil
	next.StartedAt = metav1.NewTime(r.now())
	// The delay the resource asked for, carried on the claim so a restart or
	// an early watch event cannot skip it.
	notBefore := metav1.NewTime(r.now().Add(migrationFailureRetry(migration)))
	next.RetryNotBefore = &notBefore
	name, err := r.Jobs.NameForMigration(migration, *next)
	if err != nil {
		return r.migrationOperationFailure(ctx, migration, fmt.Errorf("name the retried %s Job: %w", operation.Type, err))
	}
	next.JobName = name
	migration.Status.ActiveOperation = next
	// A read with no refresh deadline, the one after an Apply, starts the
	// interval that makes its source due again from its first failure.
	if migration.Status.NextReconciliationTime == nil {
		deadline := metav1.NewTime(r.now().Add(migrationInterval(migration)))
		migration.Status.NextReconciliationTime = &deadline
	}
	// The condition keeps 1024 bytes, as a schema's does, so a failure to open
	// the target is whole in status whatever the Secret name and key; the
	// Event below keeps 512.
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		reason, bounded(failure.Error(), 1024))
	setMigrationTargetFailure(migration, failure)
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	r.event(migration, corev1.EventTypeWarning, "OperationRetried", "%s attempt %d: %s",
		operation.Type, operation.Attempt, bounded(failure.Error(), 512))
	if r.Telemetry != nil {
		r.Telemetry.ObserveFailure(telemetry.FamilyMigration,
			telemetry.StageForMigrationOperation(operation.Type), telemetry.FailureOperation)
	}
	return requeueAtDeadline(next.RetryNotBefore, r.now()), nil
}

// discardMigrationOperation drops a claim whose inputs no longer hold. Nothing
// durable was produced, so the next reconciliation starts the read-only chain
// again from resolution.
func (r *MigrationReconciler) discardMigrationOperation(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	outcome telemetry.OperationOutcome,
	failure error,
) (ctrl.Result, error) {
	before := migration.DeepCopy()
	operation := migration.Status.ActiveOperation
	migration.Status.Phase = operatorv1alpha1.MigrationPhasePending
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
		operatorv1alpha1.ReasonInputsChanged, bounded(failure.Error(), 512))
	if err := r.retireMigrationClaim(ctx, before, migration, mutationlifecycle.DispositionDiscard, false); err != nil {
		return ctrl.Result{}, err
	}
	r.observeMigrationOperation(operation, outcome)
	if outcome == telemetry.OperationStale && r.Telemetry != nil && operation != nil {
		r.Telemetry.ObserveFailure(telemetry.FamilyMigration,
			telemetry.StageForMigrationOperation(operation.Type), telemetry.FailureStaleInput)
	}
	return ctrl.Result{RequeueAfter: statusPatchRequeue}, nil
}

// migrationOperationFailure records a configuration or dispatch failure the
// controller cannot resolve by retrying immediately.
//
// It leaves a realm's reason as it is, because reconcileMigrationExecutionBinding
// reaches it before the census has said anything; a failure after the census
// goes through claimRefused.
func (r *MigrationReconciler) migrationOperationFailure(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	failure error,
) (ctrl.Result, error) {
	return r.recordMigrationOperationFailure(ctx, migration.DeepCopy(), migration, failure)
}

// claimRefused is migrationOperationFailure for a read the census let through
// that failed before its claim was written: a source the source policy refuses,
// inputs that cannot be read, a Job that cannot be named. The census admitted
// the resource, so a realm's reason goes in the same write, and the failure is
// what Blocked, Ready and a wait's timeout name rather than a realm that let go.
func (r *MigrationReconciler) claimRefused(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	failure error,
) (ctrl.Result, error) {
	before := migration.DeepCopy()
	endMigrationRealmRefusal(migration, operatorv1alpha1.ReasonOperationFailed,
		"The database realm admits this resource again; the read it let through failed before it started")
	return r.recordMigrationOperationFailure(ctx, before, migration, failure)
}

// recordMigrationOperationFailure writes the failure over before.
func (r *MigrationReconciler) recordMigrationOperationFailure(
	ctx context.Context,
	before, migration *operatorv1alpha1.PtahMigration,
	failure error,
) (ctrl.Result, error) {
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseFailed
	migration.Status.ObservedGeneration = migration.Generation
	next := metav1.NewTime(r.now().Add(migrationFailureRetry(migration)))
	migration.Status.NextReconciliationTime = &next
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse,
		operatorv1alpha1.ReasonOperationFailed, bounded(failure.Error(), 512))
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
		operatorv1alpha1.ReasonOperationFailed, bounded(failure.Error(), 512))
	var err error
	if before.Status.ActiveOperation != nil {
		err = r.retireMigrationClaim(ctx, before, migration, mutationlifecycle.DispositionDiscard, false)
	} else {
		err = r.patchMigrationStatus(ctx, before, migration)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	r.event(migration, corev1.EventTypeWarning, "OperationFailed", "%s", bounded(failure.Error(), 512))
	if r.Telemetry != nil {
		r.Telemetry.ObserveFailure(telemetry.FamilyMigration, telemetry.FailureStageController, telemetry.FailureConfiguration)
	}
	return requeueAtDeadline(migration.Status.NextReconciliationTime, r.now()), nil
}

// migrationBlocked reports a state reconciliation cannot leave on its own.
// migrationRealmBlocked refuses a claim the realm census does not allow: a
// database more than one resource claims, or a PtahRealm that does not admit
// this resource.
//
// What ends this is another object's change -- a peer's spec, or the realm's
// grant -- and a peer's events do not reach this one, so the verdict is
// re-taken on a bounded cadence rather than waited on: the shorter of this
// resource's interval and a minute. The claim of the read that follows takes the
// reason off the conditions, in endMigrationRealmRefusal.
func (r *MigrationReconciler) migrationRealmBlocked(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	refusal realmRefusal,
) (ctrl.Result, error) {
	now := r.now()
	next := realmBlockDeadline(migration.Status.NextReconciliationTime, now, migration.Spec.Interval.Duration)
	before := migration.DeepCopy()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
	migration.Status.ObservedGeneration = migration.Generation
	migration.Status.NextReconciliationTime = &next
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, refusal.Reason, refusal.Message)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, refusal.Reason, refusal.Message)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, refusal.Reason, refusal.Message)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, refusal.Reason, refusal.Approval)
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return ctrl.Result{}, err
	}
	return requeueAtDeadline(&next, r.now()), nil
}

func (r *MigrationReconciler) migrationBlocked(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	reason operatorv1alpha1.ConditionReason,
	message string,
) (ctrl.Result, error) {
	before := migration.DeepCopy()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
	migration.Status.ObservedGeneration = migration.Generation
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, reason, message)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, reason, message)
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, reason, message)
	if reason == operatorv1alpha1.ReasonUnsupportedEngine {
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationDatabaseReachable, metav1.ConditionUnknown, reason,
			"Database access is disabled for an unsupported engine")
	}
	return ctrl.Result{}, r.patchMigrationStatus(ctx, before, migration)
}

func (r *MigrationReconciler) migrationTerminalLogs(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
) (terminalEvidence, error) {
	operation := migration.Status.ActiveOperation
	if operation == nil {
		return terminalEvidence{}, errors.New("the active migration operation is missing")
	}
	if durableDeliveryRequested(job) {
		engine := ""
		if operation.Target != nil {
			engine = strings.ToLower(string(operation.Target.Engine))
		}
		return durableTerminalResult(ctx, r.directReader(), r.Results, migration, "PtahMigration", job, operation.AdmissionSnapshot, resultconsumer.Request{ExecutionBindingID: operation.ExecutionBindingID, InputFingerprint: operation.InputFingerprint, Operation: string(migrationOperation(operation).Runner), OperationID: operation.ID, JobUID: operation.JobUID, Engine: engine})
	}
	evidence, selected, err := collectTerminalPodEvidence(
		ctx, r.directReader(), migration.Namespace, job, operation.AdmissionSnapshot,
	)
	if err != nil {
		return evidence, err
	}
	if selected == nil {
		if r.now().Sub(operation.StartedAt.Time) < terminalPodGrace {
			return evidence, errTerminalPodPending
		}
		return evidence, nil
	}
	if !evidence.Trusted {
		return evidence, nil
	}
	if executorLogsLost([]*corev1.Pod{selected}) {
		evidence.LogLost = lostExecutorLog(selected)
		return evidence, nil
	}
	if r.Logs == nil {
		return evidence, errors.New("pod log reader is not configured")
	}
	logs, err := readResultLog(ctx, r.Logs, resultLogFailuresOf(&r.resultLogs), r.now(), r.ResultReadTimeout,
		leaseReadBudget(operation.LeaseDurationSeconds), selected)
	var lost *resultLogLost
	if errors.As(err, &lost) {
		evidence.LogLost = lost
		return evidence, nil
	}
	if err != nil {
		return evidence, err
	}
	evidence.Logs = logs
	return evidence, nil
}

// ensureMigrationFinalizer keeps the resource around while a claim is in
// flight, so a deletion cannot strand a Job nothing will account for.
func (r *MigrationReconciler) ensureMigrationFinalizer(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) error {
	if controllerutil.ContainsFinalizer(migration, migrationOperationFinalizer) {
		return nil
	}
	before := migration.DeepCopy()
	controllerutil.AddFinalizer(migration, migrationOperationFinalizer)
	if err := r.Client.Patch(ctx, migration, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("add migration operation finalizer: %w", err)
	}
	return nil
}

// randomNonce makes one operation attempt distinct from every other attempt of
// the same operation, so a retry is a new claim rather than a second result for
// the old one.
func randomNonce() (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("create operation nonce: %w", err)
	}
	return hex.EncodeToString(nonce), nil
}

func (r *MigrationReconciler) removeMigrationFinalizer(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
) error {
	if !controllerutil.ContainsFinalizer(migration, migrationOperationFinalizer) {
		return nil
	}
	// Both callers clear the claim before asking for this, and the finalizer
	// is what holds the resource while one is live, so a third caller that
	// forgot would hand a deleting resource to the garbage collector with an
	// Apply still dispatched. The contract is cheap to state here and the
	// schema family states it, so state it.
	//
	// An unresolved result may outlive every workload. Its retained active
	// claim protects a live executor; the record alone cannot hold deletion
	// indefinitely while waiting for a History reading or acknowledgment.
	if migration.Status.ActiveOperation != nil {
		return fmt.Errorf("the migration operation finalizer still protects a live %s claim",
			migration.Status.ActiveOperation.Type)
	}
	// A release the resource still owes is the only record of the epoch the
	// realm is held under, and it would go with the object. The top of the
	// next pass performs it, and the finalizer comes off after that.
	if migration.Status.PendingLockRelease != nil {
		return errors.New("the migration operation finalizer still protects a database release it owes")
	}
	before := migration.DeepCopy()
	controllerutil.RemoveFinalizer(migration, migrationOperationFinalizer)
	if err := r.Client.Patch(ctx, migration, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("remove migration operation finalizer: %w", err)
	}
	return nil
}

func (r *MigrationReconciler) markJobHarvested(ctx context.Context, job *batchv1.Job) error {
	return scheduleJobCleanup(ctx, r.Client, job, jobCleanupTTLSeconds)
}

// harvestMigrationJob asks for the cleanup TTL of the finished Job the active
// claim supervised, at a harvest: before the pass records the run or consumes the
// result the Job's Pods reported, retries the operation, or discards the claim
// because its inputs moved. Every caller runs after the claim recorded the Job's
// UID and held the Job to what the claim fixes, and a caller that records a run
// or consumes a result read it from that Job's Pods first, so going on after a
// refusal acts on nothing the claim did not prove. A refusal names the Job in a
// MigrationJobCleanupDeferred Warning, and the Job stays until it is deleted by
// hand or with its owner. The pass goes on only while the stored claim still
// names the Job (settleHarvestedJobCleanup).
func (r *MigrationReconciler) harvestMigrationJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
) error {
	_, err := r.settleMigrationJobCleanup(ctx, migration, job, r.markJobHarvested(ctx, job))
	return err
}

// settleMigrationJobCleanup settles written, the answer to a cleanup TTL write
// on job, the Job the active claim names, made before the pass acts for that
// claim: a harvest's (harvestMigrationJob), or a failed or uncertain Apply's by
// the rule its Pod log is kept by (retainFailedApplyJob). A refusal names the
// Job in a MigrationJobCleanupDeferred Warning and the pass goes on, while the
// stored claim still names the Job (settleHarvestedJobCleanup). stays reports
// that refusal: nothing collects the Job, so it and its Pod log stay until
// someone deletes it.
func (r *MigrationReconciler) settleMigrationJobCleanup(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	written error,
) (stays bool, err error) {
	operation := migration.Status.ActiveOperation
	err = settleHarvestedJobCleanup(written, func() (bool, error) {
		if operation == nil {
			return false, nil
		}
		return r.storedMigrationClaimNamesJob(ctx, migration, job)
	}, func(refusal error) {
		stays = true
		r.migrationJobCleanupNotScheduled(migration, operation, job, refusal)
	})
	return stays, err
}

// storedMigrationClaimNamesJob reports whether the PtahMigration as stored holds
// a claim that names job by the name and UID admission holds the Job's cleanup
// to. A Job's UID is recorded once, by the attempt that made the Job, so that
// claim is the attempt the pass acts for. It reads the API server, as the
// admission that refused the Job's cleanup did: the cache can trail a claim
// another manager just moved.
func (r *MigrationReconciler) storedMigrationClaimNamesJob(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
) (bool, error) {
	stored := &operatorv1alpha1.PtahMigration{}
	if err := r.directReader().Get(ctx, client.ObjectKeyFromObject(migration), stored); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	active := stored.Status.ActiveOperation
	return active != nil && active.JobName == job.Name && active.JobUID == job.UID, nil
}

func (r *MigrationReconciler) patchMigrationStatus(
	ctx context.Context,
	before, after *operatorv1alpha1.PtahMigration,
) error {
	if reflect.DeepEqual(before.Status, after.Status) {
		return nil
	}
	// The journal is stamped by the wall clock, as conditions are: the
	// injected clock decides deadlines, and a journal entry decides nothing.
	now := metav1.Now().Time
	after.Status.Journal = lifecyclejournal.Record(after.Status.Journal,
		lifecyclejournal.MigrationEntries(&before.Status, &after.Status, before.Status.Journal, now), now)
	// A record of a run nobody accounted for is copied onto metadata before
	// status stores it, and the copy comes off only after status has settled
	// it. migration_unresolved_run.go says why the order is this one.
	base, err := r.copyUnresolvedRunBeforeStatus(ctx, before, after)
	if err != nil {
		return err
	}
	if err := r.Client.Status().Patch(ctx, after, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch migration status: %w", err)
	}
	r.observeMigrationStatusTransitions(before, after)
	return r.dropSettledUnresolvedRunCopy(ctx, before, after)
}

// observeMigrationStatusTransitions counts what changed, not what is true.
//
// A resource waiting for a person sits in the same status for a whole interval
// and is reconciled repeatedly while it waits, so a counter incremented from
// the status a pass read would climb once per requeue and say nothing about
// how many plans were published. Every increment here is guarded by a
// transition, and this runs only after a patch that changed something.
func (r *MigrationReconciler) observeMigrationStatusTransitions(before, after *operatorv1alpha1.PtahMigration) {
	if r.Telemetry == nil {
		return
	}
	if migrationPlanPublished(before.Status.Plan, after.Status.Plan) {
		// Nothing computes whether a hand-written migration destroys data, so
		// the impact is reported as unexamined rather than as safe.
		r.Telemetry.ObservePlan(telemetry.FamilyMigration, after.Spec.Target.Engine, telemetry.PlanImpactUnknown)
	}
	if migrationConditionBecame(before, after, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue) {
		r.Telemetry.ObserveApproval(telemetry.FamilyMigration, telemetry.ApprovalRequired)
	}
	// An accepted approval is counted where the decision is consumed, and a stale
	// one where it is marked, not here. This condition also goes False with
	// Satisfied under an Always policy, where the requirement was waived and
	// nobody approved anything, and with PlanNoLongerCurrent for a plan discarded
	// before anyone decided, which costs nobody an approval.
}

// observeMigrationOperation records how long one logical operation took. An
// operation with no start instant is one this controller did not time, and a
// zero duration would read as an operation that took no time at all.
func (r *MigrationReconciler) observeMigrationOperation(
	operation *operatorv1alpha1.MigrationOperationStatus,
	outcome telemetry.OperationOutcome,
) {
	if r.Telemetry == nil || operation == nil || operation.StartedAt.IsZero() {
		return
	}
	r.Telemetry.ObserveOperation(telemetry.FamilyMigration, telemetry.OperationForMigration(operation.Type),
		outcome, r.now().Sub(operation.StartedAt.Time))
}

// migrationPlanPublished reports a plan reference that names an object the
// previous status did not.
func migrationPlanPublished(before, after *operatorv1alpha1.ImmutableObjectReference) bool {
	if after == nil {
		return false
	}
	return before == nil || before.UID != after.UID
}

// migrationConditionBecame reports a condition that now holds the given status
// and did not before.
func migrationConditionBecame(
	before, after *operatorv1alpha1.PtahMigration,
	conditionType string,
	status metav1.ConditionStatus,
) bool {
	current := meta.FindStatusCondition(after.Status.Conditions, conditionType)
	if current == nil || current.Status != status {
		return false
	}
	previous := meta.FindStatusCondition(before.Status.Conditions, conditionType)
	return previous == nil || previous.Status != status
}

func (r *MigrationReconciler) directReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *MigrationReconciler) cachedReader() client.Reader {
	if r.CachedReader != nil {
		return r.CachedReader
	}
	return r.Client
}

func (r *MigrationReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *MigrationReconciler) event(object client.Object, eventType, reason, message string, arguments ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(object, eventType, reason, message, arguments...)
}

// SetupWithManager registers the migration controller. Jobs are watched by
// owner so a terminal Job wakes its migration instead of waiting for the poll,
// an approval wakes the migration it names for the same reason, and the
// verification policy ConfigMap wakes every migration that names it.
//
// Without the second watch an approval waited for the migration's next
// scheduled reading, up to spec.interval, before the controller so much as
// looked at it, while the schema controller acts on its approvals at once. The
// wake does not skip the evidence: a migration whose reading is due still
// refreshes the whole chain first, and one that is not due checks the approval
// against the plan, history, artifact and binding it was written for.
//
// The third watch is how soon a replaced policy is noticed, and nothing more.
// It decides nothing: the plan and the undispatched claim are each re-checked
// against the live policy where they are acted on, so a wake that never
// arrives -- a missed event, a restarted manager, a cache that lagged -- delays
// the refusal to the next reading rather than losing it.
func (r *MigrationReconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.CachedReader == nil {
		r.CachedReader = manager.GetCache()
	}
	if err := manager.GetFieldIndexer().IndexField(
		context.Background(),
		&operatorv1alpha1.PtahMigration{},
		migrationPolicyIndex,
		func(object client.Object) []string {
			migration, ok := object.(*operatorv1alpha1.PtahMigration)
			if !ok {
				return nil
			}
			return sourceConfigMapNames(migration.Spec.Artifact)
		},
	); err != nil {
		return fmt.Errorf("index migrations by the ConfigMaps their source reads: %w", err)
	}
	return ctrl.NewControllerManagedBy(manager).
		WithOptions(queueOptions()).
		For(&operatorv1alpha1.PtahMigration{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
			predicate.LabelChangedPredicate{},
		))).
		Owns(&batchv1.Job{}, builder.WithPredicates(operationJobEvents())).
		Watches(&operatorv1alpha1.PtahMigrationApproval{}, handler.EnqueueRequestsFromMapFunc(migrationForApproval)).
		Watches(&operatorv1alpha1.PtahMigrationRunAcknowledgment{}, handler.EnqueueRequestsFromMapFunc(migrationForAcknowledgment)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.migrationsForVerificationPolicy)).
		Watches(&operatorv1alpha1.PtahRealm{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, object client.Object) []reconcile.Request {
				return realmClaimantRequests(ctx, r.Client,
					func() client.ObjectList { return &operatorv1alpha1.PtahMigrationList{} }, object)
			})).
		Complete(r)
}

// migrationsForVerificationPolicy names every migration that reads this
// ConfigMap as its verification policy.
func (r *MigrationReconciler) migrationsForVerificationPolicy(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	configMap, ok := object.(*corev1.ConfigMap)
	if !ok || configMap.Name == "" {
		return nil
	}
	migrations := &operatorv1alpha1.PtahMigrationList{}
	if err := r.Client.List(
		ctx,
		migrations,
		client.InNamespace(configMap.Namespace),
		client.MatchingFields{migrationPolicyIndex: configMap.Name},
	); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(migrations.Items))
	for index := range migrations.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&migrations.Items[index]),
		})
	}
	return requests
}

// migrationForApproval names the migration an approval was written for. The
// name is enough to wake it: which approval counts is decided in the reconcile,
// by UID and by every fingerprint the plan carries, so a stale or foreign
// approval that wakes a migration changes nothing about what it runs.
func migrationForApproval(_ context.Context, object client.Object) []reconcile.Request {
	approval, ok := object.(*operatorv1alpha1.PtahMigrationApproval)
	if !ok || approval.Spec.MigrationRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: approval.Namespace,
		Name:      approval.Spec.MigrationRef.Name,
	}}}
}

func migrationSourceBinding(migration *operatorv1alpha1.PtahMigration) *operatorv1alpha1.SourceAccessBinding {
	if snapshot := migration.Status.Artifact.Snapshot; snapshot != nil {
		return &operatorv1alpha1.SourceAccessBinding{Snapshot: snapshot.DeepCopy()}
	}
	if git := migration.Status.Artifact.Git; git != nil {
		// The commit and content a resolution found, with the credential
		// and trust selectors the spec names now, as an OCI source takes its
		// registry selectors from the spec.
		binding := git.DeepCopy()
		if spec := migration.Spec.Artifact.Git; spec != nil {
			binding.AuthFrom = spec.AuthFrom.DeepCopy()
			binding.Transport = *spec.Transport.DeepCopy()
		}
		return &operatorv1alpha1.SourceAccessBinding{Git: binding}
	}
	source := &operatorv1alpha1.SourceAccessBinding{
		ResolvedReference: migration.Status.Artifact.ResolvedReference,
		Digest:            migration.Status.Artifact.Digest,
		RegistryAuthFrom:  migration.Spec.Artifact.RegistryAuthFrom.DeepCopy(),
	}
	migration.Spec.Artifact.Transport.DeepCopyInto(&source.Transport)
	return source
}

func migrationBindingEpoch(migration *operatorv1alpha1.PtahMigration) string {
	if migration.Status.ExecutionBinding == nil {
		return ""
	}
	return migration.Status.ExecutionBinding.Epoch
}

func migrationInterval(migration *operatorv1alpha1.PtahMigration) time.Duration {
	if migration.Spec.Interval.Duration > 0 {
		return migration.Spec.Interval.Duration
	}
	return defaultMigrationInterval
}

func migrationFailureRetry(migration *operatorv1alpha1.PtahMigration) time.Duration {
	if migration.Spec.Execution.FailureRetryInterval.Duration > 0 {
		return migration.Spec.Execution.FailureRetryInterval.Duration
	}
	return defaultFailureRetry
}

func setMigrationCondition(
	migration *operatorv1alpha1.PtahMigration,
	conditionType string,
	status metav1.ConditionStatus,
	reason operatorv1alpha1.ConditionReason,
	message string,
) {
	meta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
		Type: conditionType, Status: status, Reason: string(reason), Message: bounded(message, 1024),
		ObservedGeneration: migration.Generation, LastTransitionTime: metav1.Now(),
	})
}

// setMigrationTargetFailure moves DatabaseReachable when failure names why the
// target could not be opened, and otherwise leaves it as it was.
func setMigrationTargetFailure(migration *operatorv1alpha1.PtahMigration, failure error) {
	if unreached, message, named := databaseUnreached(failure); named {
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationDatabaseReachable, metav1.ConditionFalse, unreached, message)
	}
}

// setMigrationTargetOpened records an operation that opened the target.
func setMigrationTargetOpened(migration *operatorv1alpha1.PtahMigration, message string) {
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationDatabaseReachable, metav1.ConditionTrue,
		operatorv1alpha1.ReasonObserved, message)
}

// validateMigrationJobIntent holds a Job the controller read back to the Job
// the migration's active claim builds, by the rule the controller-write
// webhook applied when the Job was created.
func validateMigrationJobIntent(actual, expected *batchv1.Job, migration *operatorv1alpha1.PtahMigration) error {
	if migration == nil {
		return errors.New("no migration claims the Job")
	}
	claim := jobclaim.MigrationOperation(migration, migration.Status.ActiveOperation)
	claim.Binding = migration.Status.ExecutionBinding
	claim.Built, claim.Stored = expected, true
	return jobclaim.Match(actual, claim)
}

// validateAdoptedMigrationJobIntent is validateMigrationJobIntent for a live
// Job an earlier manager of the same execution binding may have built.
//
// The manager's recorded identity -- the controller image and revision
// annotations and the runner image -- binds nothing, so the rebuild takes it
// from the live Pod template and compares everything else exactly. The live
// template must still be the one the claim's admission snapshot recorded
// before dispatch, as every Job a claim accepts must, which pins what was
// taken to the claim rather than to the object being checked.
//
// That identity is the only value two managers of one execution binding write
// differently into a migration Job. Two processes of one release write
// nothing differently: a schema Plan Job carries its process's own seal key
// (workload.CarrySealedPlanKey), and no migration operation seals anything.
func validateAdoptedMigrationJobIntent(actual, expected *batchv1.Job, migration *operatorv1alpha1.PtahMigration) error {
	workload.CarryManagerIdentity(expected, actual)
	return validateMigrationJobIntent(actual, expected, migration)
}

// validateMigrationJobEnvelope holds a live Job to the migration's active
// claim where the claim can no longer rebuild it, because the inputs it was
// made from have moved since dispatch. The Job is held to what the claim fixes
// without a rebuild: its name, owner and recorded UID, the epoch the claim was
// made under, the labels and annotations the claim fixes, and the Pod template
// its admission snapshot recorded. It is the match the controller-write
// webhook applies when it admits the Job's cleanup TTL.
func validateMigrationJobEnvelope(actual *batchv1.Job, migration *operatorv1alpha1.PtahMigration) error {
	if migration == nil {
		return errors.New("no migration claims the Job")
	}
	claim := jobclaim.MigrationOperation(migration, migration.Status.ActiveOperation)
	claim.Binding = migration.Status.ExecutionBinding
	return jobclaim.Match(actual, claim)
}

func boundedVersions(versions []int64, limit int) []int64 {
	if len(versions) > limit {
		versions = versions[:limit]
	}
	return append([]int64(nil), versions...)
}

// unaccountedMigrationJobReason and retriedMigrationJobReason word the same
// three situations for an operator. The verdict decides what happens; these
// decide what the resource says happened, and the wording is what tells
// somebody reading a condition where to go and look.
func unaccountedMigrationJobReason(cause mutationlifecycle.JobCause) string {
	switch cause {
	case mutationlifecycle.CauseReplaced:
		return "the dispatched Apply Job was replaced"
	case mutationlifecycle.CauseDisowned:
		return "the dispatched Apply Job lost its owner"
	default:
		return "the dispatched Apply Job is missing and will not be recreated"
	}
}

func retriedMigrationJobReason(cause mutationlifecycle.JobCause) string {
	if cause == mutationlifecycle.CauseDisowned {
		return "the active Job is not owned by this migration"
	}
	return "the active Job was replaced"
}

// reportMigrationPodAdmission is reportPodAdmission for a migration, on the
// condition that family reports an operation in flight through.
func (r *MigrationReconciler) reportMigrationPodAdmission(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
) error {
	operation := migration.Status.ActiveOperation
	if operation == nil {
		return nil
	}
	change, err := judgePodAdmission(ctx, r.directReader(), job, r.now(),
		meta.FindStatusCondition(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing))
	if err != nil || !change.changed {
		return err
	}
	before := migration.DeepCopy()
	if change.refused {
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, change.reason, change.message)
	} else {
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
			operatorv1alpha1.ReasonOperationInProgress, fmt.Sprintf("%s operation is in progress", operation.Type))
	}
	if err := r.patchMigrationStatus(ctx, before, migration); err != nil {
		return err
	}
	switch {
	case !change.refused:
	case change.reason == operatorv1alpha1.ReasonPodConfigurationError:
		r.event(migration, corev1.EventTypeWarning, "PodConfigurationError", "%s", change.message)
	default:
		r.event(migration, corev1.EventTypeWarning, "PodAdmissionRefused", "%s", change.message)
	}
	return nil
}
