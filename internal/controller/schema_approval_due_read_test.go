package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
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

	reconciler, api, schema, approval := waitingForApproval(t, deadline, decide)
	if now := reconciler.now(); !now.Equal(dueReadClock) || !due(deadline, now) {
		t.Fatalf("the reconciler clock reads %s and the deadline is %v: the read is not due, so nothing below is about it",
			now, deadline)
	}
	return reconciler, api, schema, approval
}

// waitingForApproval is waitingWithADueRead for any refresh deadline.
func waitingForApproval(
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

// replacePolicyOnPlanRead is the API reader of a pass during which a person
// replaces the verification policy object: after the pass held the verified
// source to the policy, and while it reads the plan, so the plan is held to
// the replacement.
type replacePolicyOnPlanRead struct {
	client.Reader
	api       client.Client
	selector  *corev1.ConfigMapKeySelector
	namespace string
	replaced  bool
}

func (r *replacePolicyOnPlanRead) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, plan := object.(*operatorv1alpha1.PtahSchemaPlan); plan && !r.replaced {
		r.replaced = true
		old := &corev1.ConfigMap{}
		if err := r.api.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: r.selector.Name}, old); err != nil {
			return err
		}
		if err := r.api.Delete(ctx, old); err != nil {
			return err
		}
		immutable := true
		if err := r.api.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: r.namespace, Name: r.selector.Name, UID: "replaced-policy-uid"},
			Immutable:  &immutable,
			Data:       map[string]string{r.selector.Key: "replaced-policy"},
		}); err != nil {
			return err
		}
	}
	return r.Reader.Get(ctx, key, object, options...)
}

// Only an approval the controller recorded for the current plan wins over a
// due read. Without one the read runs as it did before an approval could win:
// the pass claims Resolve, and no Apply is claimed on the way.
func TestADueReadRunsWithoutARecordedApprovalOfThePlan(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		decide func(*testing.T, client.Client, *operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan) *operatorv1alpha1.PtahSchemaApproval
		// arrange, when set, changes what the reconciler reads during the
		// passes.
		arrange func(*testing.T, *SchemaReconciler, client.Client, *operatorv1alpha1.PtahSchema)
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
		{
			// The controller recorded the approval, and then the plan object
			// was deleted. An approval of a plan that is not current
			// authorizes nothing, so it holds back no read.
			name: "a recorded approval of a plan that is gone",
			decide: func(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) *operatorv1alpha1.PtahSchemaApproval {
				t.Helper()
				approval := recordTheApproval(t, api, schema, plan)
				ctx := context.Background()
				if err := api.Delete(ctx, plan); err != nil {
					t.Fatal(err)
				}
				if err := api.Get(ctx, client.ObjectKeyFromObject(plan), &operatorv1alpha1.PtahSchemaPlan{}); !apierrors.IsNotFound(err) {
					t.Fatalf("read the deleted plan: %v, want it gone", err)
				}
				return approval
			},
		},
		{
			// The controller recorded the approval, and the verification
			// policy the plan was accepted under is replaced while the pass
			// runs. The plan's source authority no longer holds, so neither
			// does the approval of it.
			name:   "a recorded approval of a plan whose verification policy was replaced",
			decide: recordTheApproval,
			arrange: func(t *testing.T, reconciler *SchemaReconciler, api client.Client, schema *operatorv1alpha1.PtahSchema) {
				t.Helper()
				if schema.Spec.Desired.VerificationPolicyFrom == nil {
					t.Fatal("the schema names no verification policy to replace")
				}
				reconciler.APIReader = &replacePolicyOnPlanRead{
					Reader: reconciler.APIReader, api: api,
					selector: schema.Spec.Desired.VerificationPolicyFrom, namespace: schema.Namespace,
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			deadline := metav1.NewTime(dueReadDeadline)
			reconciler, api, schema, approval := waitingWithADueRead(t, &deadline, row.decide)
			if row.arrange != nil {
				row.arrange(t, reconciler, api, schema)
			}

			var operation *operatorv1alpha1.ActiveOperationStatus
			for pass := 1; pass <= 3 && operation == nil; pass++ {
				reconcileDueReadPass(t, reconciler, schema, "a pass with the read due")
				operation = safetyGetSchema(t, api, schema).Status.ActiveOperation
			}
			if replacing, ok := reconciler.APIReader.(*replacePolicyOnPlanRead); ok && !replacing.replaced {
				t.Fatal("no pass read the plan, so the verification policy was never replaced and the row says nothing about a replaced one")
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

// A read request brings the interval's read forward, so a recorded approval
// wins over it as it wins over the interval: the next pass claims the Apply,
// and the read is the proof after it. The Event that answers the request says
// so rather than that the read starts now. Without a recorded approval the
// read does start now, and the Event says that.
func TestAReadRequestFollowsTheApplyOfARecordedApproval(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		decide  func(*testing.T, client.Client, *operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan) *operatorv1alpha1.PtahSchemaApproval
		message string
		claim   operatorv1alpha1.OperationType
	}{
		{
			name:    "a recorded approval",
			decide:  recordTheApproval,
			message: "A read was requested; it follows the Apply the recorded approval authorizes",
			claim:   operatorv1alpha1.OperationApply,
		},
		{
			name: "no approval",
			decide: func(*testing.T, client.Client, *operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan) *operatorv1alpha1.PtahSchemaApproval {
				return nil
			},
			message: "A read was requested; it starts now",
			claim:   operatorv1alpha1.OperationResolve,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			later := metav1.NewTime(dueReadClock.Add(time.Hour))
			reconciler, api, schema, _ := waitingForApproval(t, &later, row.decide)
			recorder := record.NewFakeRecorder(16)
			reconciler.Recorder = recorder
			requested := safetyGetSchema(t, api, schema)
			requested.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: requestedAt}
			if err := api.Update(ctx, requested); err != nil {
				t.Fatalf("request a read: %v", err)
			}

			reconcileDueReadPass(t, reconciler, schema, "the pass that answers the request")
			answered := safetyGetSchema(t, api, schema)
			if answered.Status.LastReadRequest != requestedAt || answered.Status.ActiveOperation != nil ||
				answered.Status.NextReconciliationTime == nil || !answered.Status.NextReconciliationTime.Time.Equal(dueReadClock) {
				t.Fatalf("after the request: lastReadRequest %q, claim %s, next read %v, want it answered and the read brought forward to %s",
					answered.Status.LastReadRequest, claimedType(answered.Status.ActiveOperation),
					answered.Status.NextReconciliationTime, dueReadClock)
			}
			event := findEvent(recorder, corev1.EventTypeNormal+" ReadRequested")
			if want := corev1.EventTypeNormal + " ReadRequested " + row.message; event != want {
				t.Fatalf("ReadRequested Event = %q, want %q", event, want)
			}

			reconcileDueReadPass(t, reconciler, schema, "the pass after the answer")
			claimed := safetyGetSchema(t, api, schema).Status.ActiveOperation
			if claimed == nil || claimed.Type != row.claim {
				t.Fatalf("the pass after the answer claimed %s, want %s, which the Event named", claimedType(claimed), row.claim)
			}
		})
	}
}
