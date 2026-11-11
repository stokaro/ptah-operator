package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// A harvest asks for the finished Job's cleanup TTL before the pass acts for the
// claim that supervised it: before it consumes the result the Job's Pods reported,
// retries, or discards the claim. A refused TTL write used to be the pass's error,
// so a policy that refused the TTL failed every pass, and the resource never moved
// again. A refusal is now settled as everywhere else a claim asks for a TTL
// (settleHarvestedJobCleanup), while the stored claim still names the Job.
//
// Every Job here comes from the real builder, and every Job write passes the real
// controller-write webhook.

// schemaResolveFinished stands up a Resolve the current release dispatched and
// recorded, whose Pod reported its result and stopped. A result is read only
// from a Pod the Job controller made and admission admitted as the claim's
// snapshot records, so the Pod carries the generated name and what admission
// adds.
func schemaResolveFinished(t *testing.T) (dispatchedSchemaRead, *SchemaReconciler, client.WithWatch, *record.FakeRecorder) {
	t.Helper()
	current := workloadBuilderForMigrations()
	run := schemaReadDispatchedBy(t, runShape{dispatcher: current, recorded: true})
	run.pod.GenerateName = run.job.Name + "-"
	run.pod.Name = generatedTerminalPodName(run.job.Name, "abc12")
	priority := int32(0)
	preemption := corev1.PreemptLowerPriority
	seconds := int64(300)
	run.pod.Spec.Priority = &priority
	run.pod.Spec.PreemptionPolicy = &preemption
	run.pod.Spec.Tolerations = append(run.pod.Spec.Tolerations,
		corev1.Toleration{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists,
			Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
		corev1.Toleration{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists,
			Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
	)
	reconciler, api, recorder := run.reconciler(t, current)
	finishSchemaRead(t, api, run)
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve,
		OperationID: run.schema.Status.ActiveOperation.ID, ChildExitCode: 0, ResolvedDigest: testDigest,
		ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciler.Logs = staticLogs{content: frame}
	return run, reconciler, api, recorder
}

// TestASchemaHarvestSettlesTheAnswerToItsCleanup is the harvest of a finished
// Resolve, whose result the pass consumes once the Job's TTL is asked for. A
// refusal, Forbidden from the controller-write webhook or Invalid from a
// ValidatingAdmissionPolicy, says admission will not collect the Job however
// often it is asked, so the pass consumes the result, and a Warning names the
// Job. A Job deleted since the pass read it needs no TTL. Any other failure may
// pass, so the pass fails with the claim kept, and the next one asks again.
func TestASchemaHarvestSettlesTheAnswerToItsCleanup(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		answer error
		// settled is whether the pass the write failed in consumed the result.
		settled bool
		// stays is what the Warning says, or empty where there must be none.
		stays string
	}{
		{
			name:    "refused",
			answer:  apierrors.NewForbidden(batchv1.Resource("jobs"), "", errors.New("injected refusal")),
			settled: true, stays: "injected refusal",
		},
		{
			name: "refused by a policy",
			answer: apierrors.NewInvalid(batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), "", field.ErrorList{
				field.Forbidden(field.NewPath("spec", "ttlSecondsAfterFinished"), "injected policy refusal"),
			}),
			settled: true, stays: "injected policy refusal",
		},
		{name: "deleted since it was read", answer: apierrors.NewNotFound(batchv1.Resource("jobs"), ""), settled: true},
		{name: "lost", answer: errors.New("connection reset by peer")},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run, reconciler, api, recorder := schemaResolveFinished(t)
			reconciler.Client = refusingJobTTL(reconciler.Client.(client.WithWatch), row.answer)

			_, err := reconciler.Reconcile(ctx, run.request())
			stored := safetyGetSchema(t, api, run.schema)
			if row.settled {
				if err != nil {
					t.Fatalf("Reconcile(): %v, want the answer not to fail the pass", err)
				}
				assertSchemaResolveConsumed(t, stored)
				if got := storedTTL(t, api, run.job); got != nil {
					t.Fatalf("the Job's TTL = %s, want none after the write was answered %v", ttlText(got), row.answer)
				}
				events := migrationEvents(recorder)
				if row.stays == "" {
					if schemaCleanupDeferredFor(events, run.job.Name, "") {
						t.Fatalf("a Job that is gone is named as staying: %q", events)
					}
					return
				}
				if !schemaCleanupDeferredFor(events, run.job.Name, row.stays) {
					t.Fatalf("Events = %q, want a Warning that Job %q stays because %q", events, run.job.Name, row.stays)
				}
				return
			}
			if !errors.Is(err, row.answer) {
				t.Fatalf("Reconcile() error = %v, want the lost TTL write to fail the pass", err)
			}
			if !equality.Semantic.DeepEqual(stored.Status.ActiveOperation, run.schema.Status.ActiveOperation) ||
				stored.Status.Phase != run.schema.Status.Phase {
				t.Fatalf("a lost TTL write let the pass act: phase=%q operation=%#v", stored.Status.Phase, stored.Status.ActiveOperation)
			}
			if _, err := reconciler.Reconcile(ctx, run.request()); err != nil {
				t.Fatalf("Reconcile() after the lost write: %v", err)
			}
			if got := storedTTL(t, api, run.job); got == nil || *got != jobclaim.CleanupTTLSeconds {
				t.Fatalf("the Job's TTL = %s after the next pass, want %d", ttlText(got), jobclaim.CleanupTTLSeconds)
			}
			assertSchemaResolveConsumed(t, safetyGetSchema(t, api, run.schema))
			if events := migrationEvents(recorder); schemaCleanupDeferredFor(events, run.job.Name, "") {
				t.Fatalf("a lost TTL write named the Job as staying: %q", events)
			}
		})
	}
}

// TestASchemaHarvestRefusedAfterItsClaimMovedFailsThePass is the harvest of a
// finished Resolve while a second manager, holding the same stale view, has
// already consumed the result and claimed the next operation: the controller-write
// webhook reads the claim as stored and refuses the TTL of a Job the claim no
// longer names. The refusal says nothing settled here, so the pass fails rather
// than consume the result again, and no Warning names the Job.
func TestASchemaHarvestRefusedAfterItsClaimMovedFailsThePass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	run, reconciler, api, recorder := schemaResolveFinished(t)
	next := &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationVerify, ID: "verify-claimed-by-the-other-manager",
		JobName: "verify-claimed-by-the-other-manager", Attempt: 1,
		StartedAt: metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)),
	}
	var movedTo string
	reconciler.Client = interceptor.NewClient(reconciler.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if _, isJob := object.(*batchv1.Job); isJob && movedTo == "" {
				stored := safetyGetSchema(t, api, run.schema)
				stored.Status.Phase = operatorv1alpha1.PhaseVerifying
				stored.Status.ActiveOperation = next.DeepCopy()
				if err := api.Status().Update(ctx, stored); err != nil {
					t.Fatalf("move the stored claim: %v", err)
				}
				movedTo = stored.ResourceVersion
			}
			return writer.Patch(ctx, object, patch, options...)
		},
	})

	_, err := reconciler.Reconcile(ctx, run.request())
	if movedTo == "" {
		t.Fatal("the pass asked for no TTL, so nothing moved the claim")
	}
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "Job is not the exact active operation instance") {
		t.Fatalf("Reconcile() error = %v, want the webhook's refusal of a Job the stored claim no longer names", err)
	}
	stored := safetyGetSchema(t, api, run.schema)
	if stored.ResourceVersion != movedTo || !equality.Semantic.DeepEqual(stored.Status.ActiveOperation, next) {
		t.Fatalf("the pass wrote over the claim that moved: resourceVersion=%s (moved at %s) operation=%#v",
			stored.ResourceVersion, movedTo, stored.Status.ActiveOperation)
	}
	if got := storedTTL(t, api, run.job); got != nil {
		t.Fatalf("the Job's TTL = %s, want none", ttlText(got))
	}
	if events := migrationEvents(recorder); schemaCleanupDeferredFor(events, run.job.Name, "") {
		t.Fatalf("a refusal for a claim that moved named the Job as staying: %q", events)
	}
}

// TestASchemaRetryHarvestSettlesARefusedCleanup is the harvest of a finished
// Resolve whose Pod reported no result, so the claim retries under a new Job. A
// refusal of the old Job's TTL lets the claim move to its next attempt, and a
// Warning names the Job left behind. A lost write fails the pass with the claim
// kept.
func TestASchemaRetryHarvestSettlesARefusedCleanup(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		answer  error
		settled bool
	}{
		{
			name: "refused by a policy",
			answer: apierrors.NewInvalid(batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), "", field.ErrorList{
				field.Forbidden(field.NewPath("spec", "ttlSecondsAfterFinished"), "injected policy refusal"),
			}),
			settled: true,
		},
		{name: "lost", answer: errors.New("connection reset by peer")},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			run, reconciler, api, recorder := schemaResolveFinished(t)
			reconciler.Logs = staticLogs{}
			reconciler.Client = refusingJobTTL(reconciler.Client.(client.WithWatch), row.answer)

			_, err := reconciler.Reconcile(ctx, run.request())
			claimed := run.schema.Status.ActiveOperation
			operation := safetyGetSchema(t, api, run.schema).Status.ActiveOperation
			events := migrationEvents(recorder)
			if !row.settled {
				if !errors.Is(err, row.answer) {
					t.Fatalf("Reconcile() error = %v, want the lost TTL write to fail the pass", err)
				}
				if !equality.Semantic.DeepEqual(operation, claimed) {
					t.Fatalf("a lost TTL write let the claim retry: operation=%#v", operation)
				}
				if schemaCleanupDeferredFor(events, run.job.Name, "") {
					t.Fatalf("a lost TTL write named the Job as staying: %q", events)
				}
				return
			}
			if err != nil {
				t.Fatalf("Reconcile(): %v, want the refusal not to fail the pass", err)
			}
			if operation == nil || operation.Attempt != claimed.Attempt+1 || operation.JobName == run.job.Name {
				t.Fatalf("operation = %#v, want the claim retried at attempt %d under a new Job", operation, claimed.Attempt+1)
			}
			if !schemaCleanupDeferredFor(events, run.job.Name, "injected policy refusal") {
				t.Fatalf("Events = %q, want a Warning that Job %q stays", events, run.job.Name)
			}
			if got := storedTTL(t, api, run.job); got != nil {
				t.Fatalf("the Job's TTL = %s, want none after a refusal", ttlText(got))
			}
		})
	}
}

// assertSchemaResolveConsumed fails unless the Resolve's result was consumed:
// its claim retired and the resource moved on to verification.
func assertSchemaResolveConsumed(t *testing.T, stored *operatorv1alpha1.PtahSchema) {
	t.Helper()
	if stored.Status.ActiveOperation != nil || stored.Status.Phase != operatorv1alpha1.PhaseVerifying {
		t.Fatalf("the Resolve's result was not consumed: phase=%q operation=%#v conditions=%#v", stored.Status.Phase, stored.Status.ActiveOperation, stored.Status.Conditions)
	}
}
