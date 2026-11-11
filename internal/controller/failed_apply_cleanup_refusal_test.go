package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// An Apply that failed, or whose outcome is unknown, keeps its Job by the rule its
// Pod log is kept by (retainFailedApplyJob): 24 hours where a person approved it or
// the run stops its resource, and the five minutes every other finished Job keeps
// otherwise, with the short TTL as the fallback when admission refuses the long
// one. A refusal of the fallback used to be the pass's error, so under a policy
// that refuses every Job TTL the run was never recorded and the resource stayed in
// Applying with its Lease held. A refusal is now settled as a harvest's is
// (settleHarvestedJobCleanup), while the stored record still names the Job.
//
// Each row approves the run, or has it stop its resource, so the long TTL is asked
// for first and the fallback second, and both are refused. No TTL was written, so
// nothing may say the log was shortened to five minutes.

// ttlRefusal answers every Job TTL patch with answer, as a policy that refuses the
// manager's TTL writes does, and records the TTL each patch asked for.
type ttlRefusal struct {
	answer error
	asked  []int32
}

func (refusal *ttlRefusal) client(writer client.WithWatch) client.WithWatch {
	return interceptor.NewClient(writer, interceptor.Funcs{
		Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if job, isJob := object.(*batchv1.Job); isJob && job.Spec.TTLSecondsAfterFinished != nil {
				refusal.asked = append(refusal.asked, *job.Spec.TTLSecondsAfterFinished)
				return refusal.answer
			}
			return writer.Patch(ctx, object, patch, options...)
		},
	})
}

// assertBothTTLsAsked fails unless the pass asked for the long TTL and then for
// the short fallback, and for nothing more.
func (refusal *ttlRefusal) assertBothTTLsAsked(t *testing.T) {
	t.Helper()
	if want := []int32{failedApplyTTLSeconds, jobCleanupTTLSeconds}; !slices.Equal(refusal.asked, want) {
		t.Fatalf("the TTLs asked for = %v, want %v: the long one, then the fallback", refusal.asked, want)
	}
}

// failedApplyRefusals are the answers a retry cannot change: Forbidden from the
// controller-write webhook, Invalid from a ValidatingAdmissionPolicy.
func failedApplyRefusals() []struct {
	name   string
	answer error
	says   string
} {
	return []struct {
		name   string
		answer error
		says   string
	}{
		{
			name:   "refused",
			answer: apierrors.NewForbidden(batchv1.Resource("jobs"), "", errors.New("injected refusal")),
			says:   "injected refusal",
		},
		{
			name: "refused by a policy",
			answer: apierrors.NewInvalid(batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), "", field.ErrorList{
				field.Forbidden(field.NewPath("spec", "ttlSecondsAfterFinished"), "injected policy refusal"),
			}),
			says: "injected policy refusal",
		},
	}
}

// eventsNamed reports whether any Event carries reason.
func eventsNamed(events []string, reason string) bool {
	return slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, " "+reason+" ") })
}

// stayingLogPointer is how a record ends that points at the Pod log of a Job whose
// cleanup failed for good: the log stays with the Job until someone deletes it.
func stayingLogPointer(job *batchv1.Job) string {
	return " is in the Pod log until the Job is deleted: kubectl logs -n " +
		job.Namespace + " job/" + job.Name + " -c " + executorContainerName
}

// eventEndingWith reports whether an Event of the reason ends with the text.
func eventEndingWith(events []string, reason, ending string) bool {
	return slices.ContainsFunc(events, func(event string) bool {
		return strings.Contains(event, " "+reason+" ") && strings.HasSuffix(event, ending)
	})
}

// schemaApplyFailed stands up an approved schema Apply whose Pod reported that it
// sent statements and failed, so the pass records its outcome as unknown and hands
// the database to read-only observation.
func schemaApplyFailed(t *testing.T) (*operatorv1alpha1.PtahSchema, *batchv1.Job, *SchemaReconciler, client.WithWatch, *record.FakeRecorder) {
	t.Helper()
	schema, plan, policyConfig := dispatchedApplyWithPlan(t)
	schema.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{Name: "approval", UID: "approval-uid"}
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply, OperationID: schema.Status.ActiveOperation.ID,
		ChildExitCode: 2, MutationStarted: true, Uncertain: true,
		CoordinationDigest:   schema.Status.Plan.CoordinationDigest,
		TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest,
		Error: &runner.ResultError{
			Code: "apply_outcome_unknown", Message: "ptah sent the statements and returned an error, so how far they got is unknown",
		},
	})
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, plan, policyConfig, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	return schema, job, reconciler, api.(client.WithWatch), recorder
}

// TestAnUncertainSchemaApplyIsRecordedUnderARefusedCleanup is an approved Apply
// whose Job failed with no result. A refusal of both TTLs leaves the Job in place:
// the pass still records the outcome as unknown, names the Job in a Warning, and
// hands the database to read-only observation. The record points at the log until
// the Job is deleted, since nothing collects it. A lost write fails the pass with
// the claim kept and nothing recorded, and the next pass keeps the log for 24
// hours. A refusal that comes after the stored claim moved, as under a second
// manager, fails the pass and names nothing. The fake API server runs no webhook,
// so the refusals here are injected.
func TestAnUncertainSchemaApplyIsRecordedUnderARefusedCleanup(t *testing.T) {
	t.Parallel()

	for _, row := range failedApplyRefusals() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			schema, job, reconciler, api, recorder := schemaApplyFailed(t)
			refusal := &ttlRefusal{answer: row.answer}
			reconciler.Client = refusal.client(reconciler.Client.(client.WithWatch))

			if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
				t.Fatalf("Reconcile(): %v, want a refused cleanup not to fail the pass", err)
			}
			refusal.assertBothTTLsAsked(t)
			stored := safetyGetSchema(t, api, schema)
			pending := stored.Status.PendingObservation
			if stored.Status.ActiveOperation != nil || stored.Status.Phase != operatorv1alpha1.PhaseVerifyingConvergence ||
				pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown ||
				pending.ApplyJobName != job.Name || pending.ApplyJobUID != job.UID {
				t.Fatalf("the uncertain Apply was not recorded: phase=%q operation=%#v pending=%#v",
					stored.Status.Phase, stored.Status.ActiveOperation, pending)
			}
			if got := storedTTL(t, api, job); got != nil {
				t.Fatalf("the Job's TTL = %s, want none after a refusal", ttlText(got))
			}
			if applying := meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionApplying); applying == nil ||
				applying.Reason != string(operatorv1alpha1.ReasonOutcomeUnknown) || !strings.HasSuffix(applying.Message, stayingLogPointer(job)) {
				t.Fatalf("Applying = %#v, want the outcome unknown and the log pointed at until the Job is deleted", applying)
			}
			events := migrationEvents(recorder)
			if !schemaCleanupDeferredFor(events, job.Name, row.says) {
				t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, job.Name, row.says)
			}
			if !eventEndingWith(events, "ApplyOutcomeUnknown", stayingLogPointer(job)) {
				t.Fatalf("Events = %q, want the ApplyOutcomeUnknown Event to point at the log until the Job is deleted", events)
			}
			assertNoShortening(t, events)
		})
	}
	t.Run("lost", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()

		schema, job, reconciler, api, recorder := schemaApplyFailed(t)
		lost := errors.New("connection reset by peer")
		reconciler.Client = refusingJobTTL(reconciler.Client.(client.WithWatch), lost)

		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
		if !errors.Is(err, lost) {
			t.Fatalf("Reconcile() error = %v, want the lost TTL write to fail the pass", err)
		}
		// The pass took the claim's Lease before it read the result, so the claim
		// is held to the Apply and the Job it names rather than to every field.
		stored := safetyGetSchema(t, api, schema)
		if kept := stored.Status.ActiveOperation; kept == nil || kept.ID != schema.Status.ActiveOperation.ID ||
			kept.JobName != job.Name || kept.JobUID != job.UID || stored.Status.PendingObservation != nil {
			t.Fatalf("a lost TTL write let the pass act: operation=%#v pending=%#v",
				stored.Status.ActiveOperation, stored.Status.PendingObservation)
		}
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
			t.Fatalf("Reconcile() after the lost write: %v", err)
		}
		if got := storedTTL(t, api, job); got == nil || *got != failedApplyTTLSeconds {
			t.Fatalf("the Job's TTL = %s after the next pass, want %d", ttlText(got), failedApplyTTLSeconds)
		}
		if pending := safetyGetSchema(t, api, schema).Status.PendingObservation; pending == nil ||
			pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
			t.Fatalf("the next pass did not record the uncertain Apply: pending=%#v", pending)
		}
		if events := migrationEvents(recorder); schemaCleanupDeferredFor(events, job.Name, "") {
			t.Fatalf("a lost TTL write named the Job as staying: %q", events)
		}
	})
	t.Run("refused after the claim moved", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()

		schema, job, reconciler, api, recorder := schemaApplyFailed(t)
		next := &operatorv1alpha1.ActiveOperationStatus{
			Type: operatorv1alpha1.OperationPlan, ID: "plan-claimed-by-the-other-manager",
			JobName: "plan-claimed-by-the-other-manager", Attempt: 1,
			StartedAt: metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)),
		}
		refusal := apierrors.NewForbidden(batchv1.Resource("jobs"), job.Name, errors.New("Job is not the exact active operation instance"))
		var movedTo string
		reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
			Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
				if _, isJob := object.(*batchv1.Job); !isJob {
					return writer.Patch(ctx, object, patch, options...)
				}
				if movedTo == "" {
					stored := safetyGetSchema(t, api, schema)
					stored.Status.ActiveOperation = next.DeepCopy()
					if err := api.Status().Update(ctx, stored); err != nil {
						t.Fatalf("move the stored claim: %v", err)
					}
					movedTo = stored.ResourceVersion
				}
				return refusal
			},
		})

		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
		if movedTo == "" {
			t.Fatal("the pass asked for no TTL, so nothing moved the claim")
		}
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "the record that names the Job changed") {
			t.Fatalf("Reconcile() error = %v, want the refusal of a Job the stored claim no longer names", err)
		}
		stored := safetyGetSchema(t, api, schema)
		if stored.ResourceVersion != movedTo || !equality.Semantic.DeepEqual(stored.Status.ActiveOperation, next) ||
			stored.Status.PendingObservation != nil {
			t.Fatalf("the pass wrote over the claim that moved: resourceVersion=%s (moved at %s) operation=%#v pending=%#v",
				stored.ResourceVersion, movedTo, stored.Status.ActiveOperation, stored.Status.PendingObservation)
		}
		if events := migrationEvents(recorder); schemaCleanupDeferredFor(events, job.Name, "") {
			t.Fatalf("a refusal for a claim that moved named the Job as staying: %q", events)
		}
	})
}

// retiredApplyStopped stands up an approved Apply that a binding change retired
// while it ran and that has since finished, with its UID adopted into the
// retirement record and the pending observation. Every Job write passes the real
// controller-write webhook.
func retiredApplyStopped(t *testing.T) (*operatorv1alpha1.PtahSchema, *batchv1.Job, *SchemaReconciler, client.WithWatch, *record.FakeRecorder) {
	t.Helper()
	job, snapshot := builtSchemaApplyJob(t)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	schema := schemaOwning(job)
	schema.Status.ActiveOperation = nil
	schema.Status.ExecutionBinding.Epoch = "v1-99999999999999999999999999999999"
	schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
		Outcome:           operatorv1alpha1.PendingObservationOutcomeUnknown,
		ApplyOperationID:  job.Annotations[workload.AnnotationOperationID],
		ApplyJobName:      job.Name,
		AdmissionSnapshot: snapshot,
		Plan:              planBindingOf(t, job),
	}
	schema.Status.PendingObservation.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{Name: "approval", UID: "approval-uid"}
	schema.Status.PendingBindingRetirement = &operatorv1alpha1.BindingRetirementStatus{
		RetiredEpoch: schema.Status.PendingObservation.Plan.ExecutionBindingID,
		Job:          &operatorv1alpha1.RetiredJobStatus{Operation: operatorv1alpha1.OperationApply, Name: job.Name},
	}
	// The Job ran its Pod, so its log holds something to point at.
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, job, terminalPodForJob(job))
	reconciler.Client = admittedJobWrites(api.(client.WithWatch), refusingJobBuilder{})
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	stored := safetyGetSchema(t, api, schema)
	if err := reconciler.adoptRetiredApplyJobUID(context.Background(), stored, stored.Status.PendingObservation); err != nil {
		t.Fatalf("adoptRetiredApplyJobUID() = %v", err)
	}
	stored = safetyGetSchema(t, api, schema)
	if retired := retiredApplyJob(stored, stored.Status.PendingObservation); retired == nil || retired.UID != job.UID {
		t.Fatalf("the retirement record did not adopt the Job's UID: %#v", stored.Status.PendingBindingRetirement)
	}
	return stored, job, reconciler, api.(client.WithWatch), recorder
}

// TestARetiredSchemaApplyIsSettledUnderARefusedCleanup is the cleanup of an
// approved Apply a binding change retired. No claim names the Job any more:
// admission admits its TTL by the retirement record. A refusal of both TTLs
// settles the record with the Job left in place, a Warning names the Job, and the
// log is pointed at until the Job is deleted, since nothing collects it. A lost
// write fails with the record kept, and the next call keeps the log for 24 hours.
// A refusal that comes after the stored record was settled, or moved to another
// Apply, fails and names nothing.
func TestARetiredSchemaApplyIsSettledUnderARefusedCleanup(t *testing.T) {
	t.Parallel()

	for _, row := range failedApplyRefusals() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			stored, job, reconciler, api, recorder := retiredApplyStopped(t)
			refusal := &ttlRefusal{answer: row.answer}
			reconciler.Client = refusal.client(reconciler.Client.(client.WithWatch))

			if err := reconciler.cleanupRetiredApplyJob(ctx, stored, stored.Status.PendingObservation); err != nil {
				t.Fatalf("cleanupRetiredApplyJob() = %v, want a refused cleanup to settle the record", err)
			}
			refusal.assertBothTTLsAsked(t)
			settled := safetyGetSchema(t, api, stored)
			if settled.Status.PendingBindingRetirement != nil {
				t.Fatalf("the retirement outlived the refused cleanup: %#v", settled.Status.PendingBindingRetirement)
			}
			if pending := settled.Status.PendingObservation; pending == nil ||
				pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown || pending.ApplyJobUID != job.UID {
				t.Fatalf("the refused cleanup moved the pending observation: %#v", pending)
			}
			if got := storedTTL(t, api, job); got != nil {
				t.Fatalf("the Job's TTL = %s, want none after a refusal", ttlText(got))
			}
			events := migrationEvents(recorder)
			if !schemaCleanupDeferredFor(events, job.Name, row.says) ||
				!slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, `Apply Job "`+job.Name+`"`) }) {
				t.Fatalf("Events = %q, want a Warning that Apply Job %q stays because %q", events, job.Name, row.says)
			}
			assertNoShortening(t, events)
			if !eventEndingWith(events, "RetiredApplyLogKept", stayingLogPointer(job)) {
				t.Fatalf("Events = %q, want the RetiredApplyLogKept Event to point at the log until the Job is deleted", events)
			}
		})
	}
	t.Run("lost", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()

		stored, job, reconciler, api, recorder := retiredApplyStopped(t)
		lost := errors.New("connection reset by peer")
		reconciler.Client = refusingJobTTL(reconciler.Client.(client.WithWatch), lost)

		if err := reconciler.cleanupRetiredApplyJob(ctx, stored, stored.Status.PendingObservation); !errors.Is(err, lost) {
			t.Fatalf("cleanupRetiredApplyJob() = %v, want the lost TTL write returned", err)
		}
		kept := safetyGetSchema(t, api, stored)
		if !equality.Semantic.DeepEqual(kept.Status.PendingBindingRetirement, stored.Status.PendingBindingRetirement) {
			t.Fatalf("a lost TTL write settled the record: %#v", kept.Status.PendingBindingRetirement)
		}
		if err := reconciler.cleanupRetiredApplyJob(ctx, kept, kept.Status.PendingObservation); err != nil {
			t.Fatalf("cleanupRetiredApplyJob() after the lost write = %v", err)
		}
		assertCleanupScheduled(t, api, job, failedApplyTTLSeconds)
		events := migrationEvents(recorder)
		if schemaCleanupDeferredFor(events, job.Name, "") {
			t.Fatalf("a lost TTL write named the Job as staying: %q", events)
		}
		if !eventsNamed(events, "RetiredApplyLogKept") {
			t.Fatalf("Events = %q, want the pointer to the log the retried call kept", events)
		}
	})
	// A second manager settled the record, or a later rotation retired another
	// Apply, so the stored record names another Job. The webhook refuses both.
	for _, move := range []struct {
		name string
		move func(*operatorv1alpha1.PtahSchema)
	}{
		{name: "refused after the record was settled", move: func(schema *operatorv1alpha1.PtahSchema) {
			schema.Status.PendingBindingRetirement = nil
		}},
		{name: "refused after the record moved to another Apply", move: func(schema *operatorv1alpha1.PtahSchema) {
			schema.Status.PendingBindingRetirement.Job.Name = "another-apply"
			schema.Status.PendingBindingRetirement.Job.UID = "another-apply-uid"
			schema.Status.PendingObservation.ApplyJobName = "another-apply"
			schema.Status.PendingObservation.ApplyJobUID = "another-apply-uid"
		}},
	} {
		t.Run(move.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			stored, job, reconciler, api, recorder := retiredApplyStopped(t)
			var movedTo string
			reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
					if _, isJob := object.(*batchv1.Job); isJob && movedTo == "" {
						moved := safetyGetSchema(t, api, stored)
						move.move(moved)
						if err := api.Status().Update(ctx, moved); err != nil {
							t.Fatalf("move the stored record: %v", err)
						}
						movedTo = moved.ResourceVersion
					}
					return writer.Patch(ctx, object, patch, options...)
				},
			})

			err := reconciler.cleanupRetiredApplyJob(ctx, stored, stored.Status.PendingObservation)
			if movedTo == "" {
				t.Fatal("the call asked for no TTL, so nothing moved the record")
			}
			if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "the record that names the Job changed") {
				t.Fatalf("cleanupRetiredApplyJob() = %v, want the webhook's refusal of a Job the stored record no longer names", err)
			}
			if settled := safetyGetSchema(t, api, stored); settled.ResourceVersion != movedTo {
				t.Fatalf("the call wrote over the record that moved: resourceVersion=%s, moved at %s", settled.ResourceVersion, movedTo)
			}
			if got := storedTTL(t, api, job); got != nil {
				t.Fatalf("the Job's TTL = %s, want none", ttlText(got))
			}
			if events := migrationEvents(recorder); schemaCleanupDeferredFor(events, job.Name, "") {
				t.Fatalf("a refusal for a record that moved named the Job as staying: %q", events)
			}
		})
	}
}

// migrationApplyFailedAtAStatement stands up an Apply whose Pod reported that it
// failed at a statement and left a dirty revision, so the run stops its resource
// and keeps its log for 24 hours.
func migrationApplyFailedAtAStatement(t *testing.T) (
	*operatorv1alpha1.PtahMigration, *batchv1.Job, *MigrationReconciler, client.WithWatch, *record.FakeRecorder,
) {
	t.Helper()
	return migrationApplyEnded(t, runner.Result{
		ChildExitCode: 2, MutationStarted: true,
		Error: &runner.ResultError{Code: "child_exit", Message: "ptah exited with code 2"},
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract,
			Direction:       "up",
			Outcome:         dataplane.MigrationOutcomeFailed,
			Planned:         []int64{3},
			Status:          &dataplane.MigrationStatusReport{DirtyRevision: &dataplane.MigrationDirty{Version: 3, Total: 2}},
		},
	})
}

// TestAFailedMigrationRunIsRecordedUnderARefusedCleanup is an Apply whose run
// failed and left a dirty revision. A refusal of both TTLs leaves the Job in
// place: the pass still records the run and retires its claim, a Warning names the
// Job, and the run points at the log until the Job is deleted, since nothing
// collects it. A lost write fails the pass with the claim kept and no run
// recorded. A refusal from the controller-write webhook after the stored claim
// moved fails the pass and names nothing.
func TestAFailedMigrationRunIsRecordedUnderARefusedCleanup(t *testing.T) {
	t.Parallel()

	for _, row := range failedApplyRefusals() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			migration, job, reconciler, api, recorder := migrationApplyFailedAtAStatement(t)
			refusal := &ttlRefusal{answer: row.answer}
			reconciler.Client = refusal.client(reconciler.Client.(client.WithWatch))

			if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile(): %v, want a refused cleanup not to fail the pass", err)
			}
			refusal.assertBothTTLsAsked(t)
			stored := readMigration(t, api, migration)
			if stored.Status.LastRun == nil || stored.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomeFailed ||
				stored.Status.LastRun.JobUID != job.UID || stored.Status.ActiveOperation != nil {
				t.Fatalf("the failed run was not recorded: phase=%q operation=%#v lastRun=%#v",
					stored.Status.Phase, stored.Status.ActiveOperation, stored.Status.LastRun)
			}
			if !strings.HasSuffix(stored.Status.LastRun.Message, stayingLogPointer(job)) {
				t.Fatalf("LastRun.Message = %q, want it to point at the log until the Job is deleted", stored.Status.LastRun.Message)
			}
			if got := storedTTL(t, api, job); got != nil {
				t.Fatalf("the Job's TTL = %s, want none after a refusal", ttlText(got))
			}
			events := migrationEvents(recorder)
			if !cleanupDeferredFor(events, job.Name, row.says) {
				t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, job.Name, row.says)
			}
			assertNoShortening(t, events)
		})
	}
	t.Run("lost", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()

		migration, job, reconciler, api, recorder := migrationApplyFailedAtAStatement(t)
		lost := errors.New("connection reset by peer")
		reconciler.Client = refusingJobTTL(reconciler.Client.(client.WithWatch), lost)

		_, err := reconciler.Reconcile(ctx, migrationRequest(migration))
		if !errors.Is(err, lost) {
			t.Fatalf("Reconcile() error = %v, want the lost TTL write to fail the pass", err)
		}
		stored := readMigration(t, api, migration)
		if !equality.Semantic.DeepEqual(stored.Status.ActiveOperation, migration.Status.ActiveOperation) ||
			stored.Status.LastRun != nil {
			t.Fatalf("a lost TTL write let the pass act: operation=%#v lastRun=%#v",
				stored.Status.ActiveOperation, stored.Status.LastRun)
		}
		if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() after the lost write: %v", err)
		}
		if got := storedTTL(t, api, job); got == nil || *got != jobclaim.FailedApplyRetentionTTLSeconds {
			t.Fatalf("the Job's TTL = %s after the next pass, want %d", ttlText(got), jobclaim.FailedApplyRetentionTTLSeconds)
		}
		if run := readMigration(t, api, migration).Status.LastRun; run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeFailed {
			t.Fatalf("the next pass did not record the failed run: %#v", run)
		}
		if events := migrationEvents(recorder); cleanupDeferredFor(events, job.Name, "") {
			t.Fatalf("a lost TTL write named the Job as staying: %q", events)
		}
	})
	t.Run("refused after the claim moved", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()

		migration, job, reconciler, api, recorder := migrationApplyFailedAtAStatement(t)
		next := &operatorv1alpha1.MigrationOperationStatus{
			Type: operatorv1alpha1.MigrationOperationHistory, ID: "history-claimed-by-the-other-manager",
			JobName: "history-claimed-by-the-other-manager", Attempt: 1,
			StartedAt: metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)),
		}
		var movedTo string
		reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
			Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
				if _, isJob := object.(*batchv1.Job); isJob && movedTo == "" {
					stored := readMigration(t, api, migration)
					stored.Status.ActiveOperation = next.DeepCopy()
					if err := api.Status().Update(ctx, stored); err != nil {
						t.Fatalf("move the stored claim: %v", err)
					}
					movedTo = stored.ResourceVersion
				}
				return writer.Patch(ctx, object, patch, options...)
			},
		})

		_, err := reconciler.Reconcile(ctx, migrationRequest(migration))
		if movedTo == "" {
			t.Fatal("the pass asked for no TTL, so nothing moved the claim")
		}
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "the record that names the Job changed") ||
			!strings.Contains(err.Error(), "Job is not the exact active migration operation instance") {
			t.Fatalf("Reconcile() error = %v, want the webhook's refusal of a Job the stored claim no longer names", err)
		}
		stored := readMigration(t, api, migration)
		if stored.ResourceVersion != movedTo || !equality.Semantic.DeepEqual(stored.Status.ActiveOperation, next) ||
			stored.Status.LastRun != nil {
			t.Fatalf("the pass wrote over the claim that moved: resourceVersion=%s (moved at %s) operation=%#v lastRun=%#v",
				stored.ResourceVersion, movedTo, stored.Status.ActiveOperation, stored.Status.LastRun)
		}
		if events := migrationEvents(recorder); cleanupDeferredFor(events, job.Name, "") {
			t.Fatalf("a refusal for a claim that moved named the Job as staying: %q", events)
		}
	})
}

// TestAnUncertainMigrationRunPointsAtTheLogItsFailedCleanupLeaves is the path that
// blocks a migration on a run nobody accounted for, which a deletion takes as well.
// It records the run whatever the Job's cleanup is answered. A TTL write that fails
// leaves a Job nothing collects, so a Warning says the Job stays until it is
// deleted, and the record points at the log for as long rather than dropping the
// pointer where the log is kept longest.
func TestAnUncertainMigrationRunPointsAtTheLogItsFailedCleanupLeaves(t *testing.T) {
	t.Parallel()

	type answer struct {
		name   string
		answer error
		says   string
		asked  []int32
	}
	var rows []answer
	for _, row := range failedApplyRefusals() {
		rows = append(rows, answer{row.name, row.answer, row.says, []int32{failedApplyTTLSeconds, jobCleanupTTLSeconds}})
	}
	// Any other failure leaves the Job without a TTL as well: the record is still
	// written, and nothing asks for the Job's cleanup again.
	rows = append(rows, answer{"lost", errors.New("connection reset by peer"), "connection reset by peer", []int32{failedApplyTTLSeconds}})
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			migration, plan := awaitingApprovalFixture(t)
			operation := applyClaimFor(t, migration, plan)
			operation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: "approval", UID: "approval-uid"}
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, job, pod, verificationPolicyConfigMap())
			recorder := record.NewFakeRecorder(32)
			reconciler.Recorder = recorder
			refusal := &ttlRefusal{answer: row.answer}
			reconciler.Client = refusal.client(reconciler.Client.(client.WithWatch))

			unread := errors.New("the Apply produced no readable account")
			if _, err := reconciler.retireUncertainMigrationApply(context.Background(), migration, job, unread, "", false); err != nil {
				t.Fatalf("retireUncertainMigrationApply() error = %v, want the run recorded under a failed cleanup", err)
			}
			if !slices.Equal(refusal.asked, row.asked) {
				t.Fatalf("the TTLs asked for = %v, want %v", refusal.asked, row.asked)
			}
			if got := storedTTL(t, api, job); got != nil {
				t.Fatalf("the Job's TTL = %s, want none after the write failed", ttlText(got))
			}
			settled := readMigration(t, api, migration)
			if settled.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked || settled.Status.LastRun == nil {
				t.Fatalf("phase = %q, last run = %#v, want the resource blocked on the recorded run", settled.Status.Phase, settled.Status.LastRun)
			}
			pointer := "Anything the run wrote" + stayingLogPointer(job)
			if message := settled.Status.LastRun.Message; !strings.Contains(message, pointer) {
				t.Fatalf("LastRun.Message = %q, want %q", message, pointer)
			}
			blocked := meta.FindStatusCondition(settled.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
			if blocked == nil || !strings.Contains(blocked.Message, pointer) {
				t.Fatalf("Blocked = %#v, want it to say %q", blocked, pointer)
			}
			events := migrationEvents(recorder)
			if !cleanupDeferredFor(events, job.Name, row.says) {
				t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, job.Name, row.says)
			}
			assertNoShortening(t, events)
		})
	}
}

// TestAStoppedMigrationJobsRefusedCleanupAfterItsClaimMovedFailsThePass is an Apply
// whose outcome went unknown while it ran, once its Job has stopped, while a second
// manager has already moved the claim: the controller-write webhook refuses the TTL
// of a Job the stored claim no longer names. The refusal says nothing settled here,
// so the pass fails rather than retire the claim, and no Warning names the Job.
func TestAStoppedMigrationJobsRefusedCleanupAfterItsClaimMovedFailsThePass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	run, reconciler, api, recorder := executorRolledOut().reach(t)
	stopMigrationWorkload(t, api, run, "")
	next := &operatorv1alpha1.MigrationOperationStatus{
		Type: operatorv1alpha1.MigrationOperationHistory, ID: "history-claimed-by-the-other-manager",
		JobName: "history-claimed-by-the-other-manager", Attempt: 1,
		StartedAt: metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)),
	}
	var movedTo string
	reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if job, isJob := object.(*batchv1.Job); isJob && job.Spec.TTLSecondsAfterFinished != nil && movedTo == "" {
				stored := readMigration(t, api, run.migration)
				stored.Status.ActiveOperation = next.DeepCopy()
				if err := api.Status().Update(ctx, stored); err != nil {
					t.Fatalf("move the stored claim: %v", err)
				}
				movedTo = stored.ResourceVersion
			}
			return writer.Patch(ctx, object, patch, options...)
		},
	})

	_, err := reconciler.Reconcile(ctx, migrationRequest(run.migration))
	if movedTo == "" {
		t.Fatal("the pass asked for no TTL, so nothing moved the claim")
	}
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "the record that names the Job changed") ||
		!strings.Contains(err.Error(), "Job is not the exact active migration operation instance") {
		t.Fatalf("Reconcile() error = %v, want the webhook's refusal of a Job the stored claim no longer names", err)
	}
	stored := readMigration(t, api, run.migration)
	if stored.ResourceVersion != movedTo || !equality.Semantic.DeepEqual(stored.Status.ActiveOperation, next) {
		t.Fatalf("the pass wrote over the claim that moved: resourceVersion=%s (moved at %s) operation=%#v",
			stored.ResourceVersion, movedTo, stored.Status.ActiveOperation)
	}
	if got := storedTTL(t, api, run.job); got != nil {
		t.Fatalf("the Job's TTL = %s, want none", ttlText(got))
	}
	if events := migrationEvents(recorder); cleanupDeferredFor(events, run.job.Name, "") {
		t.Fatalf("a refusal for a claim that moved named the Job as staying: %q", events)
	}
}
