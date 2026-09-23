package controller

import (
	"time"

	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sync/atomic"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// planUIDAssigningClient supplies the server-generated UIDs the fake client
// omits. The plan store binds each committed chunk to the UID of the object
// that holds it, so a store built on the bare fake client can publish a plan
// and never load it back.
type planUIDAssigningClient struct {
	client.Client
	next atomic.Int64
}

func (c *planUIDAssigningClient) Create(
	ctx context.Context,
	object client.Object,
	options ...client.CreateOption,
) error {
	if object.GetUID() == "" {
		object.SetUID(types.UID(fmt.Sprintf("uid-%s-%d", object.GetName(), c.next.Add(1))))
	}
	return c.Client.Create(ctx, object, options...)
}

// dispatchableApply stands a schema up with an Apply claim that can actually
// reach dispatch: its plan bytes are published in the store, which every other
// schema fixture leaves empty.
func dispatchableApply(
	t *testing.T,
	approved bool,
) (*SchemaReconciler, client.Client, *operatorv1alpha1.PtahSchema, string) {
	t.Helper()

	ctx := context.Background()
	schema, plan, policyConfig := safetyReadyToApplyFixture(t)
	if approved {
		schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, policyConfig)
	reconciler.Client = assignCreatedJobUIDClient{Client: api, uid: "apply-job-uid"}
	reconciler.Plans = planstore.Store{Client: &planUIDAssigningClient{Client: api}, Reader: api}
	reconciler.Locks = targetlock.New(api, api, nil)

	content := []byte("CREATE TABLE widgets (id bigint primary key);\n")
	spec := plan.Spec
	spec.ContentDigest = fingerprint.DigestBytes(content)
	spec.Fingerprint = fingerprint.DigestBytes([]byte("dispatchable-apply-plan"))
	desired, chunks, err := planstore.Prepare(schema, spec, content)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	desired.UID = "dispatchable-plan-uid"
	published, err := reconciler.Plans.Publish(ctx, desired, chunks)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	stored := safetyGetSchema(t, api, schema)
	stored.Status.Plan = currentPlanStatus(published)
	if approved {
		stored.Status.Plan.Approval = recordApprovalFor(t, api, stored, published)
	}
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatalf("record the published plan: %v", err)
	}
	stored = safetyGetSchema(t, api, schema)
	if _, err := reconciler.claim(ctx, stored, operatorv1alpha1.OperationApply); err != nil {
		t.Fatalf("claim(Apply) error = %v", err)
	}
	claimed := safetyGetSchema(t, api, schema).Status.ActiveOperation
	if claimed == nil || claimed.Type != operatorv1alpha1.OperationApply || claimed.JobName == "" {
		t.Fatalf("the Apply claim reserved no Job name: %#v", claimed)
	}
	return reconciler, api, schema, claimed.JobName
}

// recordApprovalFor creates the approval a published plan needs and returns
// what the schema records about it. Every field approvalMatches reads comes
// from the plan that was actually published, so the approval stands for this
// plan and no other.
func recordApprovalFor(
	t *testing.T,
	api client.Client,
	schema *operatorv1alpha1.PtahSchema,
	plan *operatorv1alpha1.PtahSchemaPlan,
) *operatorv1alpha1.ConsumedApprovalStatus {
	t.Helper()

	approvedAt := metav1.NewTime(time.Date(2026, 8, 30, 11, 30, 0, 0, time.UTC))
	approval := &operatorv1alpha1.PtahSchemaApproval{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: schema.Namespace, Name: "dispatch-approval", UID: "dispatch-approval-uid",
		},
		Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
			SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
			PlanRef:                  operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
			PlanFingerprint:          plan.Spec.Fingerprint,
			ArtifactDigest:           plan.Spec.ArtifactDigest,
			CoordinationDigest:       plan.Spec.CoordinationDigest,
			TargetIdentityDigest:     plan.Spec.TargetIdentityDigest,
			ActualStateFingerprint:   plan.Spec.ActualStateFingerprint,
			DesiredStateFingerprint:  plan.Spec.DesiredStateFingerprint,
			PolicyFingerprint:        plan.Spec.PolicyFingerprint,
			VerificationPolicyUID:    plan.Spec.VerificationPolicyUID,
			VerificationPolicyDigest: plan.Spec.VerificationPolicyDigest,
			ExecutionBindingID:       plan.Spec.ExecutionBindingID,
			ControllerImage:          plan.Spec.ControllerImage,
			ControllerRevision:       plan.Spec.ControllerRevision,
			ControllerStateVersion:   plan.Spec.ControllerStateVersion,
			PtahVersion:              plan.Spec.PtahVersion,
			ExecutorImage:            plan.Spec.ExecutorImage,
			RunnerImage:              plan.Spec.RunnerImage,
			RunnerProtocolVersion:    plan.Spec.RunnerProtocolVersion,
			Approver:                 operatorv1alpha1.ApprovalIdentity{Username: "approver@example.com"},
			ApprovedAt:               approvedAt,
			MutationRequestUID:       "dispatch-approval-request-uid",
		},
	}
	if err := api.Create(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	return &operatorv1alpha1.ConsumedApprovalStatus{
		Name: approval.Name, UID: approval.UID,
		Approver: approval.Spec.Approver, ApprovedAt: approval.Spec.ApprovedAt,
	}
}

// applyDispatched reports whether the Job the Apply claim reserved exists. The
// fake builder labels nothing, so the reserved name is the only thing that
// distinguishes an Apply Job from the Observe the controller starts next -- and
// a filter that selects neither reads exactly like a refusal.
func applyDispatched(t *testing.T, api client.Client, namespace, jobName string) bool {
	t.Helper()

	job := &batchv1.Job{}
	err := api.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: jobName}, job)
	if err == nil {
		return true
	}
	if !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return false
}

// TestAnApplyClaimDispatchesOnlyWhatIsStillProvable drives an Apply claim
// through reconcileActive to the Job it creates, and then breaks one of the
// inputs the dispatch rests on.
//
// The claim is not the authorization. Between claiming an Apply and creating
// its Job the controller re-reads the plan manifest, the plan bytes and the
// verification policy the plan was built against, because each can move while
// the claim stands and none of them is re-checked anywhere later: once the Job
// exists, the executor runs the SQL it was handed.
//
// Nothing measured any of that. Every schema fixture left the plan store
// empty, so every Apply in this suite died at "verify plan storage" long
// before the checks below, and each of them could be removed with the whole
// suite staying green.
func TestAnApplyClaimDispatchesOnlyWhatIsStillProvable(t *testing.T) {
	t.Parallel()

	for _, approved := range []bool{false, true} {
		name := "a provable Apply reaches its Job"
		if approved {
			name = "an approved Apply reaches its Job"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			reconciler, api, schema, jobName := dispatchableApply(t, approved)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
			for pass := range 4 {
				if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
					t.Fatalf("Reconcile() pass %d error = %v", pass, err)
				}
				if applyDispatched(t, api, schema.Namespace, jobName) {
					return
				}
			}
			t.Fatalf("a provable Apply never dispatched: %#v", safetyGetSchema(t, api, schema).Status)
		})
	}

	for _, row := range []struct {
		name       string
		approved   bool
		wantReason operatorv1alpha1.ConditionReason
		break_     func(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema)
	}{
		{
			// The manifest carries the digests the Job is built from. A claim
			// that outlived it names a plan nothing can read back.
			name: "the plan manifest is gone",
			break_: func(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema) {
				stored := safetyGetSchema(t, api, schema)
				plan := &operatorv1alpha1.PtahSchemaPlan{}
				key := client.ObjectKey{Namespace: schema.Namespace, Name: stored.Status.Plan.Name}
				if err := api.Get(context.Background(), key, plan); err != nil {
					t.Fatal(err)
				}
				if err := api.Delete(context.Background(), plan); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// The manifest stands and the SQL behind it does not. Dispatching
			// here would hand the executor a plan the operator cannot read.
			name: "the published plan bytes are gone",
			break_: func(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema) {
				chunks := &corev1.ConfigMapList{}
				if err := api.List(context.Background(), chunks, client.InNamespace(schema.Namespace)); err != nil {
					t.Fatal(err)
				}
				removed := 0
				for i := range chunks.Items {
					chunk := &chunks.Items[i]
					if chunk.Labels[planstore.LabelPlan] == "" {
						continue
					}
					if err := api.Delete(context.Background(), chunk); err != nil {
						t.Fatal(err)
					}
					removed++
				}
				if removed == 0 {
					t.Fatal("no published chunk was removed, so this proves nothing")
				}
			},
		},
		{
			// The plan was verified against one policy object. Another one, or
			// none, is not the policy anybody reviewed.
			name: "the verification policy the plan was built against is gone",
			break_: func(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema) {
				policyConfig := &corev1.ConfigMap{}
				key := client.ObjectKey{
					Namespace: schema.Namespace,
					Name:      schema.Spec.Desired.VerificationPolicyFrom.Name,
				}
				if err := api.Get(context.Background(), key, policyConfig); err != nil {
					t.Fatal(err)
				}
				if err := api.Delete(context.Background(), policyConfig); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// The approval is what authorizes this exact plan. Withdrawing it
			// while the claim stands leaves an Apply nobody agreed to.
			name:       "the recorded approval is withdrawn",
			approved:   true,
			wantReason: operatorv1alpha1.ReasonApprovalRevoked,
			break_: func(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema) {
				stored := safetyGetSchema(t, api, schema)
				recorded := stored.Status.Plan.Approval
				if recorded == nil {
					t.Fatal("the fixture recorded no approval, so withdrawing one proves nothing")
				}
				approval := &operatorv1alpha1.PtahSchemaApproval{
					ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: recorded.Name},
				}
				if err := api.Delete(context.Background(), approval); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			reconciler, api, schema, jobName := dispatchableApply(t, row.approved)
			row.break_(t, api, schema)

			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
			for pass := range 4 {
				if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
					t.Fatalf("Reconcile() pass %d error = %v", pass, err)
				}
				if applyDispatched(t, api, schema.Namespace, jobName) {
					t.Fatalf("pass %d dispatched %q against an Apply that is no longer provable", pass, jobName)
				}
			}
			settled := safetyGetSchema(t, api, schema)
			if operation := settled.Status.ActiveOperation; operation != nil &&
				operation.Type == operatorv1alpha1.OperationApply {
				t.Fatalf("the unprovable Apply claim still stands: %#v", operation)
			}
			if row.wantReason != "" {
				applying := meta.FindStatusCondition(settled.Status.Conditions, operatorv1alpha1.ConditionApplying)
				if applying == nil || applying.Reason != string(row.wantReason) {
					t.Fatalf("Applying condition = %#v, want reason %q", applying, row.wantReason)
				}
			}
		})
	}
}
