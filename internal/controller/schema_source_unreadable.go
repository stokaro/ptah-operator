package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// When an Observe or a Plan fails and its document names no target failure,
// the runner asks Ptah once more about the desired schema alone, in a child
// with no URL and no credential (internal/runner/desired_source.go). Where Ptah
// refuses the file, the result carries desired_source_unreadable and Ptah's
// sentence, which names the file and what in it Ptah refuses: a statement the
// parser does not read, a relative time literal as a date or time column's
// default, a row file outside Ptah's working directory. Where Ptah reads the
// file and refuses the schema as a whole, and that fault is the one the failed
// command stopped on, the result carries desired_schema_invalid and the fault:
// a foreign key into a table the schema does not declare, an index on a column
// its table does not declare, a domain whose own default is a relative time
// string.
//
// Either answer belongs to the file, so another attempt against the same
// server gets it again. An invalid schema is decided against the server, its
// version and its sql_mode, so a server change can move it; the interval and a
// read request read it again, which is how such a change is noticed. Retried
// at every failure retry, as before, the read dispatched a Job each time, took
// the database Lease each time, and reported a fault while waiting on a person
// to change the file. The read stops at Blocked instead, as a migration's
// history read does on a directory Ptah cannot read
// (MigrationDirectoryUnreadable), with a reason for each answer, since what a
// person changes differs: a file Ptah does not parse, or a declaration the file
// states.

// schemaRefusal is one of those answers: the reason the schema stops with, and
// the words its conditions use for what Ptah did with the desired schema.
type schemaRefusal struct {
	reason operatorv1alpha1.ConditionReason
	// refused, refuses and accepts complete "Ptah %s the desired schema the
	// Apply ran from", "a desired schema Ptah %s" and "until Ptah %s it".
	refused, refuses, accepts string
}

var (
	// sourceUnreadable is desired_source_unreadable: Ptah could not read the
	// file.
	sourceUnreadable = schemaRefusal{
		reason: operatorv1alpha1.ReasonSchemaSourceUnreadable, refused: "could not read", refuses: "cannot read", accepts: "can read",
	}
	// schemaInvalid is desired_schema_invalid: Ptah read the file and refused
	// the schema it states.
	schemaInvalid = schemaRefusal{
		reason: operatorv1alpha1.ReasonSchemaInvalid, refused: "refused", refuses: "refuses", accepts: "accepts",
	}
)

// schemaRefusalOf reports the refusal failure is, where it is Ptah's answer
// about the desired schema to one of the two operations that read it.
func schemaRefusalOf(operation operatorv1alpha1.OperationType, failure *runner.ResultError) (schemaRefusal, bool) {
	if failure == nil || (operation != operatorv1alpha1.OperationObserve && operation != operatorv1alpha1.OperationPlan) {
		return schemaRefusal{}, false
	}
	switch failure.Code {
	case runner.CodeDesiredSourceUnreadable:
		return sourceUnreadable, true
	case runner.CodeDesiredSchemaInvalid:
		return schemaInvalid, true
	}
	return schemaRefusal{}, false
}

// refuseSchemaSource records an Observe or a Plan whose desired schema Ptah
// could not read or refused. No plan outlives the refusal, and no approval of
// one does. The read-only chain starts again from Resolve at the interval, when
// a moved tag or branch can carry another file; at once when a ConfigMap
// source changes or the spec does, which reconcile checks before the Blocked
// phase; and on a read request, which a Blocked resource answers.
//
// An unreadable file says nothing about the target. The child that named it
// holds no URL, and the run before it may have failed before it opened the
// database, so DatabaseReachable keeps what the last read that opened it said.
// An invalid schema says the read opened it: Ptah connects before it loads the
// file, and the runner reports one only where the failed command stopped on a
// fault of the comparison or a declaration, which a connect failure never is
// (failureReadTheTarget).
//
// A read that serves a proof goes to consumeResultWithTransport instead, the
// one place a proof ends, which writes how it ended with
// endProofOnRefusedSchema.
func (r *SchemaReconciler) refuseSchemaSource(
	ctx context.Context,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
	result runner.Result,
	refusal schemaRefusal,
	podUIDs []types.UID,
	podCount int32,
	durable bool,
) (ctrl.Result, error) {
	if schemaClaimServesProof(schema) {
		return r.consumeResultWithTransport(ctx, schema, job, result, podUIDs, podCount, durable)
	}
	if err := r.harvestJob(ctx, schema, job); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.markRecordedApprovalStale(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	operation := schema.Status.ActiveOperation
	now := metav1.NewTime(r.now())
	next := cycleDeadline(schema.Status.NextReconciliationTime, r.now(), interval(schema))
	message := resultCodeMessage(result.Error.Code, result.Error.Message)
	before := schema.DeepCopy()
	schema.Status.LastAttemptTime = &now
	schema.Status.NextReconciliationTime = &next
	schema.Status.Phase = operatorv1alpha1.PhaseBlocked
	clearFailure(schema)
	if failureReadTheTarget(operation.Type, result.Error.Code) {
		setSchemaTargetObserved(schema)
	}
	setSchemaRefusalConditions(schema, refusal, message,
		"Whether the database matches the desired schema is unknown until Ptah "+refusal.accepts+" it")
	if err := r.retireClaim(ctx, before, schema, mutationlifecycle.DispositionAccounted); err != nil {
		return ctrl.Result{}, err
	}
	r.observeOperation(operation, telemetry.OperationSucceeded)
	if err := r.removeActiveFinalizer(ctx, schema); err != nil {
		return ctrl.Result{}, err
	}
	r.warnSchemaRefused(schema, refusal, message)
	return requeueAtDeadline(&next, r.now()), nil
}

// warnSchemaRefused records the Warning Event a refusal names, with its
// condition reason as the Event's. Each reason is written out rather than
// converted, so the check that holds the Events page to what the controllers
// record (hack/event_reasons_page_test.go) reads it.
func (r *SchemaReconciler) warnSchemaRefused(schema *operatorv1alpha1.PtahSchema, refusal schemaRefusal, message string) {
	switch refusal.reason {
	case operatorv1alpha1.ReasonSchemaSourceUnreadable:
		r.event(schema, corev1.EventTypeWarning, "SchemaSourceUnreadable", "%s", bounded(message, 512))
	case operatorv1alpha1.ReasonSchemaInvalid:
		r.event(schema, corev1.EventTypeWarning, "SchemaInvalid", "%s", bounded(message, 512))
	}
}

// endProofOnRefusedSchema writes how a proof ends whose read could not read
// the desired schema, or met a schema Ptah refuses; consumeResultWithTransport
// ends the proof in the same write. The proof reads the snapshot the Apply ran
// from, which no edit changes, so a retry can only get the same refusal, and it
// would hold the database Lease and dispatch a Job at every failure retry while
// doing so. Keeping the pending observation while retiring the claim is not an
// answer either: reconcile carries out a pending observation before anything
// else, so the next pass would claim the same read at once. So the proof ends,
// as a proof whose read planned nothing does (endRefusedPlan). Nothing
// established what the Apply did, so no Applied record is written. The realm
// goes back: the proof read runs only once the Apply's executor can no longer
// write.
//
// The usual way here is a Ptah upgrade between the Apply and its proof, whose
// release refuses what the release before it read. PtahVersion and
// ExecutorImage are execution-binding components, so the upgrade retired the
// binding the Apply's plan names, and the proof's read ran under the new one.
// That proof is not current: settleProofInputs records the binding change,
// the journal records the proof Verified Superseded, the resource reads
// Pending, and the next pass reads the source at once, where an ordinary read
// of the same file stops Blocked (refuseSchemaSource). The Warning Event is
// what names the file for the proof itself. A newer generation or policy ends
// the proof Superseded the same way. Only where the executor that planned the
// snapshot refuses it on the read after does the proof end current: Blocked
// until the interval, and Verified Unknown with the refusal's reason in the
// journal (internal/lifecyclejournal), which is what the wait verbs read.
//
// current says the proof ends under the policy and execution binding the Apply
// ran under.
func (r *SchemaReconciler) endProofOnRefusedSchema(
	schema *operatorv1alpha1.PtahSchema,
	pending *operatorv1alpha1.PendingObservationStatus,
	refusal schemaRefusal,
	message string,
	current bool,
) {
	setSchemaRefusalConditions(schema, refusal, message,
		"Nothing proved the Apply converged: Ptah "+refusal.refused+" the desired schema the Apply ran from")
	if pending.ApplyGeneration == schema.Generation && current {
		next := cycleDeadline(schema.Status.NextReconciliationTime, r.now(), interval(schema))
		schema.Status.NextReconciliationTime = &next
		schema.Status.Phase = operatorv1alpha1.PhaseBlocked
		return
	}
	schema.Status.NextReconciliationTime = nil
	schema.Status.Phase = operatorv1alpha1.PhasePending
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionFalse, operatorv1alpha1.ReasonDesiredStateChanged,
		"Ptah "+refusal.refused+" the desired schema the Apply ran from, so nothing proved the Apply, and newer desired inputs must be reconciled")
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonDesiredStateChanged,
		"A newer resource generation is pending")
}

// setSchemaRefusalConditions records a read whose desired schema Ptah could
// not read or refused: no plan, and nothing known about the difference, since
// nothing was compared. Ready carries the runner's code and Ptah's sentence,
// which is where a reader looks first; inSync says what the read leaves
// unknown.
func setSchemaRefusalConditions(schema *operatorv1alpha1.PtahSchema, refusal schemaRefusal, message, inSync string) {
	schema.Status.Plan = nil
	setCondition(schema, operatorv1alpha1.ConditionDriftDetected, metav1.ConditionUnknown, refusal.reason,
		"Whether the database differs is unknown until Ptah "+refusal.accepts+" the desired schema")
	setCondition(schema, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, refusal.reason,
		"Nothing is planned from a desired schema Ptah "+refusal.refuses)
	endApprovalRequirement(schema)
	setCondition(schema, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, refusal.reason, inSync)
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, refusal.reason, message)
}
