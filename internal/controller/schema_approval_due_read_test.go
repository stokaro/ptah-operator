package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// dueReadClock is when every pass below runs: the fakeReconciler clock.
// dueReadDeadline is a refresh deadline a minute earlier, so the read is due
// on the first pass and not merely at it.
var (
	dueReadClock    = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	dueReadDeadline = dueReadClock.Add(-time.Minute)
)

// waitingWithADueRead stands a schema up under OnApproval with a plan whose
// bytes are in the store, waiting for approval, and the refresh deadline
// given, which is due: passed, or none at all. decide writes what a person did
// about the plan into the stored status before the first pass: nothing, an
// approval of another plan, or an approval the controller recorded.
func waitingWithADueRead(
	t *testing.T,
	deadline *metav1.Time,
	decide func(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) *operatorv1alpha1.PtahSchemaApproval,
) (*SchemaReconciler, client.Client, *operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaApproval) {
	t.Helper()

	ctx := context.Background()
	schema, plan, policyConfig := safetyReadyToApplyFixture(t)
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
	schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
	schema.Status.NextReconciliationTime = deadline.DeepCopy()
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, policyConfig)
	if now := reconciler.now(); !now.Equal(dueReadClock) || !due(deadline, now) {
		t.Fatalf("the reconciler clock reads %s and the deadline is %v: the read is not due, so nothing below is about it",
			now, deadline)
	}
	reconciler.Client = assignCreatedJobUIDClient{Client: api, uid: "apply-job-uid"}
	reconciler.Plans = planstore.Store{Client: &planUIDAssigningClient{Client: api}, Reader: api}
	reconciler.Locks = targetlock.New(api, api, nil)

	content := safetyPlanDocument(t, plan.Spec.ActualStateFingerprint)
	spec := plan.Spec
	spec.ContentDigest = fingerprint.DigestBytes(content)
	spec.Fingerprint = fingerprint.DigestBytes([]byte("due-read-plan"))
	desired, chunks, err := planstore.Prepare(schema, spec, content)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	desired.UID = "due-read-plan-uid"
	published, err := reconciler.Plans.Publish(ctx, desired, chunks)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	stored := safetyGetSchema(t, api, schema)
	stored.Status.Plan = currentPlanStatus(published)
	approval := decide(t, api, stored, published)
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatalf("record the waiting plan: %v", err)
	}
	return reconciler, api, schema, approval
}

// recordTheApproval is a person's approval of the current plan, recorded by
// the controller before the read came due.
func recordTheApproval(
	t *testing.T,
	api client.Client,
	schema *operatorv1alpha1.PtahSchema,
	plan *operatorv1alpha1.PtahSchemaPlan,
) *operatorv1alpha1.PtahSchemaApproval {
	t.Helper()
	schema.Status.Plan.Approval = recordApprovalFor(t, api, schema, plan)
	return readSchemaApproval(t, api, schema.Namespace, schema.Status.Plan.Approval.Name)
}

func readSchemaApproval(t *testing.T, api client.Client, namespace, name string) *operatorv1alpha1.PtahSchemaApproval {
	t.Helper()
	approval := &operatorv1alpha1.PtahSchemaApproval{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, approval); err != nil {
		t.Fatal(err)
	}
	return approval
}

// claimedType names the operation a claim is for, or says there is none.
func claimedType(operation *operatorv1alpha1.ActiveOperationStatus) string {
	if operation == nil {
		return "no operation"
	}
	return string(operation.Type)
}

func reconcileDueReadPass(t *testing.T, reconciler *SchemaReconciler, schema *operatorv1alpha1.PtahSchema, pass string) {
	t.Helper()
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() on %s: %v", pass, err)
	}
}

// A person approved the plan before its interval ended, and the controller
// recorded the approval. The read that came due would have spent it: its
// Observe marks a recorded approval stale whatever the database says, so the
// same plan came back waiting for a second approval. The recorded approval
// wins instead. The Apply it authorizes is claimed on the pass the read was
// due on and dispatched under the approval, which is consumed and never stale,
// and the read of the database follows the Apply as its proof.
func TestARecordedApprovalWinsOverADueRead(t *testing.T) {
	t.Parallel()

	passed := metav1.NewTime(dueReadDeadline)
	for _, row := range []struct {
		name     string
		deadline *metav1.Time
	}{
		{name: "the deadline passed", deadline: &passed},
		{name: "no deadline recorded"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			recordedApprovalWinsOverADueRead(t, row.deadline)
		})
	}
}

func recordedApprovalWinsOverADueRead(t *testing.T, deadline *metav1.Time) {
	t.Helper()

	ctx := context.Background()
	reconciler, api, schema, approval := waitingWithADueRead(t, deadline, recordTheApproval)

	reconcileDueReadPass(t, reconciler, schema, "the pass the read is due on")
	claimed := safetyGetSchema(t, api, schema)
	operation := claimed.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.OperationApply {
		t.Fatalf("the pass the read was due on claimed %s in phase %q, want the Apply the recorded approval %q authorizes",
			claimedType(operation), claimed.Status.Phase, approval.Name)
	}
	if claimed.Status.Plan == nil || claimed.Status.Plan.Approval == nil || claimed.Status.Plan.Approval.UID != approval.UID {
		t.Fatalf("the Apply claim does not carry the recorded approval: %#v", claimed.Status.Plan)
	}
	if meta.IsStatusConditionTrue(readSchemaApproval(t, api, schema.Namespace, approval.Name).Status.Conditions,
		operatorv1alpha1.ConditionApprovalStale) {
		t.Fatal("the approval went stale on the pass that claimed the Apply it authorizes")
	}

	job := reconcileUntilASchemaJobExists(t, reconciler, api, schema)
	dispatched := safetyGetSchema(t, api, schema).Status.ActiveOperation
	if dispatched == nil || dispatched.ID != operation.ID || !dispatched.DispatchStarted ||
		dispatched.JobUID != job.UID || job.Name != operation.JobName {
		t.Fatalf("Job %q (UID %q) is not the dispatch of Apply claim %q: %#v", job.Name, job.UID, operation.ID, dispatched)
	}
	consumed := readSchemaApproval(t, api, schema.Namespace, approval.Name)
	if !meta.IsStatusConditionTrue(consumed.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) ||
		meta.IsStatusConditionTrue(consumed.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) {
		t.Fatalf("approval conditions = %#v, want consumed by the dispatch and not stale", consumed.Status.Conditions)
	}

	// The Apply ends. The pass that harvests it owes the database a read, and
	// the pass after it starts that read, bound to the Apply that ran.
	terminal := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), terminal); err != nil {
		t.Fatal(err)
	}
	terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := api.Status().Update(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	pod := terminalPodForJob(terminal)
	// The fake builder names no ServiceAccount, and admission fills in the
	// namespace's default one, which is what the claim's snapshot recorded.
	if pod.Spec.ServiceAccountName == "" {
		pod.Spec.ServiceAccountName = "default"
	}
	// The proof waits until no Pod of the Apply can still be running.
	pod.Status.Phase = corev1.PodSucceeded
	if err := api.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconciler.Logs = staticLogs{content: safetyRunnerFrame(t, runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationApply,
		OperationID:          operation.ID,
		ChildExitCode:        0,
		MutationStarted:      true,
		CoordinationDigest:   claimed.Status.Plan.CoordinationDigest,
		TargetIdentityDigest: claimed.Status.Plan.TargetIdentityDigest,
	})}
	reconcileDueReadPass(t, reconciler, schema, "the pass that harvests the Apply")
	harvested := safetyGetSchema(t, api, schema)
	pending := harvested.Status.PendingObservation
	if harvested.Status.ActiveOperation != nil || pending == nil ||
		pending.Outcome != operatorv1alpha1.PendingObservationApplySucceeded || pending.ApplyOperationID != operation.ID {
		t.Fatalf("after the Apply ended: claim %s, pending observation %#v, want the Apply retired and its read owed",
			claimedType(harvested.Status.ActiveOperation), pending)
	}

	reconcileDueReadPass(t, reconciler, schema, "the pass after the harvest")
	read := safetyGetSchema(t, api, schema)
	if read.Status.ActiveOperation == nil || read.Status.ActiveOperation.Type != operatorv1alpha1.OperationObserve ||
		read.Status.Phase != operatorv1alpha1.PhaseVerifyingConvergence ||
		read.Status.PendingObservation == nil || read.Status.PendingObservation.ApplyOperationID != operation.ID {
		t.Fatalf("after the Apply: phase %q, claim %s, want the read of the database that follows Apply %q",
			read.Status.Phase, claimedType(read.Status.ActiveOperation), operation.ID)
	}
}

// Only an approval the controller recorded for the current plan wins over a
// due read. Without one the read runs as it did before an approval could win:
// the pass claims Resolve, and no Apply is claimed on the way.
func TestADueReadRunsWithoutARecordedApprovalOfThePlan(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		decide func(*testing.T, client.Client, *operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan) *operatorv1alpha1.PtahSchemaApproval
	}{
		{
			name: "no approval",
			decide: func(*testing.T, client.Client, *operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan) *operatorv1alpha1.PtahSchemaApproval {
				return nil
			},
		},
		{
			// An approval of an earlier plan: another fingerprint, another
			// plan object. Nothing records it, since it names no current plan.
			name: "an approval of another plan",
			decide: func(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) *operatorv1alpha1.PtahSchemaApproval {
				t.Helper()
				approval := &operatorv1alpha1.PtahSchemaApproval{
					ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: "earlier-approval", UID: "earlier-approval-uid"},
					Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
						SchemaRef:          operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
						PlanRef:            operatorv1alpha1.ImmutableObjectReference{Name: "earlier-plan", UID: "earlier-plan-uid"},
						PlanFingerprint:    fingerprint.DigestBytes([]byte("earlier-plan")),
						Approver:           operatorv1alpha1.ApprovalIdentity{Username: "approver@example.com"},
						ApprovedAt:         metav1.NewTime(dueReadClock.Add(-time.Hour)),
						MutationRequestUID: "earlier-approval-request-uid",
					},
				}
				if approval.Spec.PlanFingerprint == plan.Spec.Fingerprint {
					t.Fatal("the earlier approval names the current plan's fingerprint")
				}
				if err := api.Create(context.Background(), approval); err != nil {
					t.Fatal(err)
				}
				return approval
			},
		},
		{
			// The controller recorded the approval, and then the person
			// withdrew it. A record of an approval is not the approval.
			name: "a recorded approval that was withdrawn",
			decide: func(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) *operatorv1alpha1.PtahSchemaApproval {
				t.Helper()
				approval := recordTheApproval(t, api, schema, plan)
				if err := api.Delete(context.Background(), approval); err != nil {
					t.Fatal(err)
				}
				return nil
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			deadline := metav1.NewTime(dueReadDeadline)
			reconciler, api, schema, approval := waitingWithADueRead(t, &deadline, row.decide)

			var operation *operatorv1alpha1.ActiveOperationStatus
			for pass := 1; pass <= 3 && operation == nil; pass++ {
				reconcileDueReadPass(t, reconciler, schema, "a pass with the read due")
				operation = safetyGetSchema(t, api, schema).Status.ActiveOperation
			}
			if operation == nil || operation.Type != operatorv1alpha1.OperationResolve {
				t.Fatalf("with the read due the controller claimed %s, want the Resolve that starts the read", claimedType(operation))
			}
			if approval != nil {
				safetyAssertApprovalNotAuthorized(t, api, approval)
			}
		})
	}
}
