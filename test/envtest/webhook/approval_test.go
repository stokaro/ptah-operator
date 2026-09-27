package webhook_test

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	approverName = "alice@example.com"
	approverUID  = "idp-alice"
	approverRole = "database-approvers"
	planName     = "ptah-plan-0123456789abcdef01234567"
)

// approvalFixture is a schema waiting for a person: a committed plan, and the
// status a manager writes when the plan needs an approval. There is no manager
// here, so the fixture writes that status itself.
type approvalFixture struct {
	namespace string
	schema    *operatorv1alpha1.PtahSchema
	plan      *operatorv1alpha1.PtahSchemaPlan
	policy    *corev1.ConfigMap
}

func newApprovalFixture(t *testing.T) approvalFixture {
	t.Helper()
	ctx := context.Background()
	namespace := newNamespace(t, "approvals")
	policy := createPolicy(t, namespace)
	schema := createSchema(t, namespace, "orders")
	coordination := coordinationDigest(t, schema)

	plan := &operatorv1alpha1.PtahSchemaPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: planName},
		Spec: operatorv1alpha1.PtahSchemaPlanSpec{
			ContractVersion:          fingerprint.CurrentPlanContractVersion,
			SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
			Fingerprint:              digest("a"),
			ContentDigest:            digest("b"),
			Size:                     64,
			ArtifactDigest:           digest("c"),
			CoordinationDigest:       coordination,
			TargetIdentityDigest:     digest("d"),
			ActualStateFingerprint:   digest("e"),
			DesiredStateFingerprint:  digest("f"),
			PolicyFingerprint:        digest("1"),
			VerificationPolicyUID:    policy.UID,
			VerificationPolicyDigest: policyDigest(),
			ExecutionBindingID:       executionBindingID,
			ControllerImage:          manager.controllerImage,
			ControllerRevision:       controllerRevision,
			ControllerStateVersion:   controllerstate.CurrentVersion,
			PtahVersion:              manager.ptahVersion,
			ExecutorImage:            manager.executorImage,
			RunnerImage:              manager.runnerImage,
			RunnerProtocolVersion:    int32(runner.ProtocolVersion),
			Dialect:                  "postgresql",
			Destructive:              true,
			StatementCount:           1,
			Chunks: []operatorv1alpha1.PlanChunkReference{{
				Name: planName + "-000", Key: "chunk", Index: 0, Digest: digest("b"), Size: 64,
			}},
		},
	}
	if err := admin.Create(ctx, plan); err != nil {
		t.Fatalf("create PtahSchemaPlan %s/%s: %v", namespace, planName, err)
	}
	plan.Status = operatorv1alpha1.PtahSchemaPlanStatus{
		ObservedGeneration: plan.Generation,
		Conditions: []metav1.Condition{{
			Type: operatorv1alpha1.ConditionPlanStorageReady, Status: metav1.ConditionTrue,
			Reason: "Published", Message: "every chunk is stored", LastTransitionTime: now(),
		}},
	}
	writeStatus(t, plan)

	schema.Status = operatorv1alpha1.PtahSchemaStatus{
		ObservedGeneration: schema.Generation,
		Phase:              operatorv1alpha1.PhaseAwaitingApproval,
		ExecutionBinding:   executionBinding(),
		Source: operatorv1alpha1.SchemaSourceStatus{
			Digest:                   plan.Spec.ArtifactDigest,
			VerificationPolicyUID:    policy.UID,
			VerificationPolicyDigest: policyDigest(),
		},
		Target: operatorv1alpha1.TargetStatus{
			CoordinationDigest: coordination,
			IdentityDigest:     plan.Spec.TargetIdentityDigest,
		},
		Plan: &operatorv1alpha1.CurrentPlanStatus{
			Name: plan.Name, UID: plan.UID,
			Fingerprint:              plan.Spec.Fingerprint,
			ContentDigest:            plan.Spec.ContentDigest,
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
			Destructive:              plan.Spec.Destructive,
			StatementCount:           plan.Spec.StatementCount,
			CreatedAt:                now(),
		},
		Conditions: []metav1.Condition{{
			Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionTrue,
			Reason: "DestructivePlan", Message: "the plan drops something", LastTransitionTime: now(),
		}},
	}
	writeStatus(t, schema)

	grant(t, namespace, "approvers", rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: approverRole},
		rbacv1.PolicyRule{
			APIGroups: []string{operatorv1alpha1.GroupVersion.Group},
			Resources: []string{"ptahschemaapprovals", "ptahmigrationapprovals"},
			Verbs:     []string{"create", "get"},
		})
	return approvalFixture{namespace: namespace, schema: schema, plan: plan, policy: policy}
}

// approval is what a person writes: which schema, which plan, and the plan's
// fingerprint. Every other binding is the mutating webhook's to fill in.
func (fixture approvalFixture) approval(name, planFingerprint string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": operatorv1alpha1.GroupVersion.String(),
		"kind":       "PtahSchemaApproval",
		"metadata":   map[string]any{"namespace": fixture.namespace, "name": name},
		"spec": map[string]any{
			"schemaRef":       map[string]any{"name": fixture.schema.Name, "uid": string(fixture.schema.UID)},
			"planRef":         map[string]any{"name": fixture.plan.Name, "uid": string(fixture.plan.UID)},
			"planFingerprint": planFingerprint,
		},
	}}
}

func approverClient(t *testing.T) client.Client {
	t.Helper()
	config := plane.Impersonate(approverName, approverRole)
	config.Impersonate.UID = approverUID
	return clientFor(t, config)
}

func TestApprovalWebhooks(t *testing.T) {
	plane.Require(t)
	fixture := newApprovalFixture(t)
	ctx := context.Background()

	t.Run("a minimal approval is stamped with the authenticated identity and the plan's bindings", func(t *testing.T) {
		t.Parallel()
		approval := fixture.approval("approve-orders", fixture.plan.Spec.Fingerprint)
		// A person cannot sign for someone else: whatever the request says the
		// approver is, the stored approver is who the API server authenticated.
		if err := unstructured.SetNestedField(approval.Object, map[string]any{
			"username": "someone-else", "uid": "forged", "groups": []any{"system:masters"},
		}, "spec", "approver"); err != nil {
			t.Fatal(err)
		}
		if err := approverClient(t).Create(ctx, approval); err != nil {
			t.Fatalf("create PtahSchemaApproval %s/%s as %s: %v", fixture.namespace, approval.GetName(), approverName, err)
		}

		stored := &operatorv1alpha1.PtahSchemaApproval{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(approval), stored); err != nil {
			t.Fatalf("read PtahSchemaApproval %s back: %v", client.ObjectKeyFromObject(approval), err)
		}
		spec := stored.Spec
		wantGroups := []string{approverRole, "system:authenticated"}
		if spec.Approver.Username != approverName || spec.Approver.UID != approverUID ||
			!slices.Equal(spec.Approver.Groups, wantGroups) {
			t.Fatalf("stored approver = %+v, want %s (uid %s) in %v", spec.Approver, approverName, approverUID, wantGroups)
		}
		if spec.ApprovedAt.IsZero() || spec.MutationRequestUID == "" {
			t.Fatalf("stored approval has approvedAt %v and mutationRequestUID %q; the mutating webhook stamps both",
				spec.ApprovedAt, spec.MutationRequestUID)
		}
		plan := fixture.plan.Spec
		for field, pair := range map[string][2]string{
			"artifactDigest":           {spec.ArtifactDigest, plan.ArtifactDigest},
			"coordinationDigest":       {spec.CoordinationDigest, plan.CoordinationDigest},
			"targetIdentityDigest":     {spec.TargetIdentityDigest, plan.TargetIdentityDigest},
			"actualStateFingerprint":   {spec.ActualStateFingerprint, plan.ActualStateFingerprint},
			"desiredStateFingerprint":  {spec.DesiredStateFingerprint, plan.DesiredStateFingerprint},
			"policyFingerprint":        {spec.PolicyFingerprint, plan.PolicyFingerprint},
			"verificationPolicyUID":    {string(spec.VerificationPolicyUID), string(plan.VerificationPolicyUID)},
			"verificationPolicyDigest": {spec.VerificationPolicyDigest, plan.VerificationPolicyDigest},
			"executionBindingID":       {spec.ExecutionBindingID, plan.ExecutionBindingID},
			"controllerImage":          {spec.ControllerImage, plan.ControllerImage},
			"controllerRevision":       {spec.ControllerRevision, plan.ControllerRevision},
			"ptahVersion":              {spec.PtahVersion, plan.PtahVersion},
			"executorImage":            {spec.ExecutorImage, plan.ExecutorImage},
			"runnerImage":              {spec.RunnerImage, plan.RunnerImage},
		} {
			if pair[0] != pair[1] {
				t.Errorf("stored approval %s = %q, want the plan's %q", field, pair[0], pair[1])
			}
		}
		if spec.RunnerProtocolVersion != plan.RunnerProtocolVersion || spec.ControllerStateVersion != plan.ControllerStateVersion {
			t.Errorf("stored approval runner protocol %d and state version %d, want the plan's %d and %d",
				spec.RunnerProtocolVersion, spec.ControllerStateVersion, plan.RunnerProtocolVersion, plan.ControllerStateVersion)
		}
	})

	t.Run("an approval naming a fingerprint the plan does not have is refused", func(t *testing.T) {
		t.Parallel()
		approval := fixture.approval("approve-another-plan", digest("9"))
		err := approverClient(t).Create(ctx, approval)
		requireDenied(t, err, "mapproval.operator.ptah.run", "approval plan fingerprint does not match the immutable plan")
	})

	t.Run("an approval for a replaced plan is refused", func(t *testing.T) {
		t.Parallel()
		approval := fixture.approval("approve-replaced-plan", fixture.plan.Spec.Fingerprint)
		if err := unstructured.SetNestedField(approval.Object, "a-plan-that-was-deleted", "spec", "planRef", "uid"); err != nil {
			t.Fatal(err)
		}
		err := approverClient(t).Create(ctx, approval)
		requireDenied(t, err, "mapproval.operator.ptah.run", "referenced plan UID does not match; the plan was replaced")
	})

	t.Run("a migration approval for a plan that does not exist is refused", func(t *testing.T) {
		t.Parallel()
		// The migration family has handlers of its own behind entries of its
		// own; this row is what shows the chart routes to them.
		approval := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": operatorv1alpha1.GroupVersion.String(),
			"kind":       "PtahMigrationApproval",
			"metadata":   map[string]any{"namespace": fixture.namespace, "name": "approve-a-missing-plan"},
			"spec": map[string]any{
				"migrationRef":    map[string]any{"name": "orders", "uid": "migration-uid"},
				"planRef":         map[string]any{"name": "ptah-mplan-0123456789abcdef01234567", "uid": "plan-uid"},
				"planFingerprint": digest("a"),
			},
		}}
		err := approverClient(t).Create(ctx, approval)
		requireDenied(t, err, "mmigrationapproval.operator.ptah.run", "read referenced migration plan for approval defaults")
	})

	t.Run("a stored approval takes a metadata update and keeps its decision", func(t *testing.T) {
		t.Parallel()
		// The validating webhook matches UPDATE as well as CREATE. A label is
		// how a person marks an approval reviewed, and the webhook has to let
		// it through: it refuses only a changed spec, which the CRD's own CEL
		// rule refuses first.
		approval := fixture.approval("approve-then-label", fixture.plan.Spec.Fingerprint)
		if err := approverClient(t).Create(ctx, approval); err != nil {
			t.Fatalf("create PtahSchemaApproval %s: %v", client.ObjectKeyFromObject(approval), err)
		}
		stored := &operatorv1alpha1.PtahSchemaApproval{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(approval), stored); err != nil {
			t.Fatalf("read PtahSchemaApproval %s back: %v", client.ObjectKeyFromObject(approval), err)
		}
		stored.Labels = map[string]string{"reviewed": "true"}
		if err := admin.Update(ctx, stored); err != nil {
			t.Fatalf("a metadata-only update of %s was refused: %v", client.ObjectKeyFromObject(stored), err)
		}
	})
}
