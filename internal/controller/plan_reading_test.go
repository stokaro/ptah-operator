package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/planstore"
)

const grantStatement = `GRANT SELECT ON TABLE "public"."orders" TO PUBLIC`

// TestAnotherReadingOfTheSamePlanBytesPublishesAnotherPlan is a classifier fix
// meeting a plan an earlier build published. The earlier build read the same
// bytes as changing no privilege; this one reads a grant. The plan name comes
// from the fingerprint, and the fingerprint binds the reading, so the two
// builds publish two plans rather than colliding on one name -- a collision
// the manifest comparison refuses on every pass, forever.
func TestAnotherReadingOfTheSamePlanBytesPublishesAnotherPlan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	planDocument := safetyPlanDocumentWithStatement(t, "observed-state", grantStatement)

	// What this build publishes for the bytes, measured on a cluster of its own.
	reference, referenceAPI, referenceSchema := planHarvestFixture(t, operatorv1alpha1.ApplyPolicyAlways, planDocument)
	if _, err := reference.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(referenceSchema)}); err != nil {
		t.Fatalf("reference Reconcile() error = %v", err)
	}
	current := &operatorv1alpha1.PtahSchemaPlan{}
	currentStatus := safetyGetSchema(t, referenceAPI, referenceSchema).Status.Plan
	if currentStatus == nil {
		t.Fatal("the reference pass published no plan")
	}
	if err := referenceAPI.Get(ctx, client.ObjectKey{Namespace: referenceSchema.Namespace, Name: currentStatus.Name}, current); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(current.Spec.PrivilegeChanges, []operatorv1alpha1.PrivilegeChange{operatorv1alpha1.PrivilegeChangeGrant}) {
		t.Fatalf("this build read privilege changes %v, want the grant", current.Spec.PrivilegeChanges)
	}

	// The same bytes as the earlier build published them: every field equal
	// but its reading of the privileges.
	earlierSpec := *current.Spec.DeepCopy()
	earlierSpec.PrivilegeChanges = nil
	earlierSpec.Chunks = nil
	var err error
	earlierSpec.Fingerprint, err = planstore.Binding(referenceSchema.UID, earlierSpec).Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	earlierName, err := planstore.Name(earlierSpec.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if earlierName == current.Name {
		t.Fatalf("two readings of the same bytes share the plan name %s", earlierName)
	}

	reconciler, api, schema := planHarvestFixture(t, operatorv1alpha1.ApplyPolicyAlways, planDocument)
	earlier, chunks, err := planstore.Prepare(schema, earlierSpec, planDocument)
	if err != nil {
		t.Fatalf("Prepare(earlier) error = %v", err)
	}
	if _, err := reconciler.Plans.Publish(ctx, earlier, chunks); err != nil {
		t.Fatalf("publish the earlier build's plan: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() beside the earlier build's plan error = %v", err)
	}
	actual := safetyGetSchema(t, api, schema)
	if actual.Status.Plan == nil || actual.Status.Plan.Name != current.Name ||
		!slices.Equal(actual.Status.Plan.PrivilegeChanges, current.Spec.PrivilegeChanges) {
		t.Fatalf("status.plan = %#v, want this build's plan %s with its grant", actual.Status.Plan, current.Name)
	}
	if actual.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval {
		t.Fatalf("phase = %s, want AwaitingApproval for a grant under Always", actual.Status.Phase)
	}
}

// TestAnApplyIsReadAgainBeforeDispatch is an Apply under Always that this
// build dispatches for a plan another build published. The manifest records
// what that build read out of the bytes; this build reads them again before it
// creates the Job, and a plan it reads differently is retired rather than run
// on a decision taken about another reading. The first row reads them the same
// way and dispatches, so the refusals below are about the reading and nothing
// else in the fixture.
func TestAnApplyIsReadAgainBeforeDispatch(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		bytes    func(t *testing.T, plan *operatorv1alpha1.PtahSchemaPlan) []byte
		refusal  string
		dispatch bool
	}{
		{
			name: "this build reads the bytes as the plan records",
			bytes: func(t *testing.T, plan *operatorv1alpha1.PtahSchemaPlan) []byte {
				return safetyPlanDocument(t, plan.Spec.ActualStateFingerprint)
			},
			dispatch: true,
		},
		{
			name: "this build reads a grant the plan does not record",
			bytes: func(t *testing.T, plan *operatorv1alpha1.PtahSchemaPlan) []byte {
				return safetyPlanDocumentWithStatement(t, plan.Spec.ActualStateFingerprint, grantStatement)
			},
			refusal: "privilege changes",
		},
		{
			name: "this build reads as destructive a plan recorded as not",
			bytes: func(t *testing.T, plan *operatorv1alpha1.PtahSchemaPlan) []byte {
				return safetyPlanDocument(t, plan.Spec.ActualStateFingerprint, true)
			},
			refusal: "destructive",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			reconciler, api, schema, jobName := dispatchableApplyStoring(t, false, func(plan *operatorv1alpha1.PtahSchemaPlan) []byte {
				return row.bytes(t, plan)
			})
			if schema.Spec.Policy.Apply != operatorv1alpha1.ApplyPolicyAlways {
				t.Fatalf("the fixture's policy is %s; the row is about an Apply nobody approves", schema.Spec.Policy.Apply)
			}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
			for pass := range 4 {
				if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
					t.Fatalf("Reconcile() pass %d error = %v", pass, err)
				}
				if applyDispatched(t, api, schema.Namespace, jobName) {
					break
				}
			}
			dispatched := applyDispatched(t, api, schema.Namespace, jobName)
			if dispatched != row.dispatch {
				t.Fatalf("Apply dispatched = %t, want %t: %#v", dispatched, row.dispatch, safetyGetSchema(t, api, schema).Status)
			}
			if row.dispatch {
				return
			}
			// The retired claim gives way to the read-only work that plans
			// again, so what must be gone is the Apply, not every claim.
			actual := safetyGetSchema(t, api, schema)
			condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionPlanReady)
			if (actual.Status.ActiveOperation != nil && actual.Status.ActiveOperation.Type == operatorv1alpha1.OperationApply) ||
				actual.Status.Plan != nil || condition == nil ||
				condition.Reason != string(operatorv1alpha1.ReasonStale) || !strings.Contains(condition.Message, row.refusal) {
				t.Fatalf("status after the reading differed = %#v, want the Apply retired as stale naming %q", actual.Status, row.refusal)
			}
		})
	}
}
