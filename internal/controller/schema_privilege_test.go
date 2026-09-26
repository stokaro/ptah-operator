package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// What a published plan's privilege class does to the policy decision, one
// policy at a time. The class adds a requirement under Always and changes
// nothing a stricter setting already required, and it never outranks a
// destructive plan that policy refuses outright.
func TestPlanPolicyStatusForPrivilegeChanges(t *testing.T) {
	t.Parallel()

	grants := []operatorv1alpha1.PrivilegeChange{
		operatorv1alpha1.PrivilegeChangeGrant, operatorv1alpha1.PrivilegeChangeSecurityDefiner,
	}
	tests := []struct {
		name             string
		apply            operatorv1alpha1.ApplyPolicy
		allowDestructive bool
		destructive      bool
		privileges       []operatorv1alpha1.PrivilegeChange
		phase            operatorv1alpha1.ReconciliationPhase
		approvalStatus   metav1.ConditionStatus
		approvalReason   operatorv1alpha1.ConditionReason
	}{
		{
			name: "Always, no privilege change", apply: operatorv1alpha1.ApplyPolicyAlways,
			phase: operatorv1alpha1.PhaseReadyToApply, approvalStatus: metav1.ConditionFalse, approvalReason: operatorv1alpha1.ReasonNotRequired,
		},
		{
			name: "Always, privilege change", apply: operatorv1alpha1.ApplyPolicyAlways, privileges: grants,
			phase: operatorv1alpha1.PhaseAwaitingApproval, approvalStatus: metav1.ConditionTrue, approvalReason: operatorv1alpha1.ReasonPrivilegeChanges,
		},
		{
			name: "Always, allowed destructive privilege change", apply: operatorv1alpha1.ApplyPolicyAlways,
			allowDestructive: true, destructive: true, privileges: grants,
			phase: operatorv1alpha1.PhaseAwaitingApproval, approvalStatus: metav1.ConditionTrue, approvalReason: operatorv1alpha1.ReasonPrivilegeChanges,
		},
		{
			name: "Always, refused destructive privilege change", apply: operatorv1alpha1.ApplyPolicyAlways,
			destructive: true, privileges: grants,
			phase: operatorv1alpha1.PhaseBlocked, approvalStatus: metav1.ConditionFalse, approvalReason: operatorv1alpha1.ReasonDestructiveChangesDisabled,
		},
		{
			name: "OnApproval, privilege change", apply: operatorv1alpha1.ApplyPolicyOnApproval, privileges: grants,
			phase: operatorv1alpha1.PhaseAwaitingApproval, approvalStatus: metav1.ConditionTrue, approvalReason: operatorv1alpha1.ReasonPlanReady,
		},
		{
			name: "Never, privilege change", apply: operatorv1alpha1.ApplyPolicyNever, privileges: grants,
			phase: operatorv1alpha1.PhaseBlocked, approvalStatus: metav1.ConditionFalse, approvalReason: operatorv1alpha1.ReasonApplyDisabled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := schemaFixture()
			schema.Spec.Policy.Apply = test.apply
			schema.Spec.Policy.AllowDestructive = test.allowDestructive
			plan := &operatorv1alpha1.PtahSchemaPlan{Spec: operatorv1alpha1.PtahSchemaPlanSpec{
				Destructive: test.destructive, PrivilegeChanges: test.privileges,
			}}

			setPlanPolicyStatus(schema, plan)

			condition := findCondition(schema.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
			if schema.Status.Phase != test.phase || condition == nil ||
				condition.Status != test.approvalStatus || condition.Reason != string(test.approvalReason) {
				t.Fatalf("phase %q, ApprovalRequired %#v; want %q, %s/%s",
					schema.Status.Phase, condition, test.phase, test.approvalStatus, test.approvalReason)
			}
			if test.approvalReason == operatorv1alpha1.ReasonPrivilegeChanges {
				assertPrivilegeMessage(t, condition.Message, "Grant, SecurityDefiner")
			}
		})
	}
}

// A schema under Always whose plan changes privileges waits for an approval,
// keeps saying why while it waits, and reserves one when it arrives. The plan
// without the class claims Apply straight away from the same state, which is
// what makes the other two rows about the class.
func TestAlwaysSchemaWaitsForAnApprovalOfAPrivilegeChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		privileges []operatorv1alpha1.PrivilegeChange
		approval   bool
	}{
		{name: "no privilege change claims Apply"},
		{name: "privilege change waits", privileges: []operatorv1alpha1.PrivilegeChange{operatorv1alpha1.PrivilegeChangeOwnership}},
		{
			name: "privilege change with an approval reserves it", approval: true,
			privileges: []operatorv1alpha1.PrivilegeChange{operatorv1alpha1.PrivilegeChangeOwnership},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema, plan, approval, policyConfig := safetyApprovalFixture(t)
			schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
			plan.Spec.PrivilegeChanges = test.privileges
			schema.Status.Plan = currentPlanStatus(plan)
			next := metav1.NewTime(time.Date(2026, 8, 30, 12, 4, 0, 0, time.UTC))
			schema.Status.NextReconciliationTime = &next
			objects := []client.Object{schema, plan, policyConfig}
			if test.approval {
				objects = append(objects, approval)
			}
			reconciler, api := fakeReconciler(t, staticLogs{}, objects...)
			reconciler.Clock = func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) }

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := safetyGetSchema(t, api, schema)
			switch {
			case len(test.privileges) == 0:
				if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationApply {
					t.Fatalf("ActiveOperation = %#v, want an automatic Apply claim", actual.Status.ActiveOperation)
				}
			case test.approval:
				if actual.Status.ActiveOperation != nil || actual.Status.Plan == nil || actual.Status.Plan.Approval == nil ||
					actual.Status.Plan.Approval.UID != approval.UID {
					t.Fatalf("status = %#v, want the approval reserved and no claim yet", actual.Status)
				}
			default:
				if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval {
					t.Fatalf("status = %#v, want AwaitingApproval with no claim", actual.Status)
				}
				condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
				if condition == nil || condition.Status != metav1.ConditionTrue ||
					condition.Reason != string(operatorv1alpha1.ReasonPrivilegeChanges) {
					t.Fatalf("ApprovalRequired = %#v, want True/PrivilegeChanges while waiting", condition)
				}
				assertPrivilegeMessage(t, condition.Message, "Ownership")
			}
		})
	}
}

// assertPrivilegeMessage holds a condition message to naming the kinds and
// nothing a statement says. The fixtures' SQL is never in reach of the
// message, so what this checks is the shape: the kinds in parentheses, and a
// pointer to where the statements can be read.
func assertPrivilegeMessage(t *testing.T, message, kinds string) {
	t.Helper()
	if !strings.Contains(message, "("+kinds+")") || !strings.Contains(message, "kubectl ptah plan") {
		t.Fatalf("message %q does not name (%s) and where to read them", message, kinds)
	}
}
