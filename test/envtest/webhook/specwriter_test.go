package webhook_test

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const (
	specAuthorName = "author@example.com"
	specAuthorUID  = "idp-author"
)

func specAuthorClient(t *testing.T) client.Client {
	t.Helper()
	config := plane.Impersonate(specAuthorName, "schema-authors")
	config.Impersonate.UID = specAuthorUID
	return clientFor(t, config)
}

// requireDistinctApproverForTest turns the four-eyes control on for every
// approval handler this process serves, for the duration of t, and restores
// each handler's previous value afterward. The control is the installation's
// own flag rather than anything a resource's spec can reach, so proving it
// means flipping the same field cmd/manager/main.go sets from the chart's
// value, on the exact handlers already serving real requests, rather than
// standing up a second manager.
func requireDistinctApproverForTest(t *testing.T) {
	t.Helper()
	previous := make([]bool, len(approvalHandlers))
	for i, handler := range approvalHandlers {
		previous[i] = handler.RequireDistinctApprover
		handler.RequireDistinctApprover = true
	}
	migrationPrevious := make([]bool, len(migrationApprovalHandlers))
	for i, handler := range migrationApprovalHandlers {
		migrationPrevious[i] = handler.RequireDistinctApprover
		handler.RequireDistinctApprover = true
	}
	t.Cleanup(func() {
		for i, handler := range approvalHandlers {
			handler.RequireDistinctApprover = previous[i]
		}
		for i, handler := range migrationApprovalHandlers {
			handler.RequireDistinctApprover = migrationPrevious[i]
		}
	})
}

// TestSpecWriterStampingAndFourEyes exercises the mutating webhook that
// records the last spec writer, and the approval webhook's refusal of a
// self-approval once the installation requires a distinct one. It reuses
// newApprovalFixture's schema, plan and "approvers" grant rather than
// building a second plan-and-status fixture: the point of this row is the
// identity path, and the binding checks are already proven in
// TestApprovalWebhooks. It does not run in parallel with the rest of this
// package, because requireDistinctApproverForTest changes what every
// approval handler in this process refuses for as long as it runs.
func TestSpecWriterStampingAndFourEyes(t *testing.T) {
	plane.Require(t)
	ctx := context.Background()
	fixture := newApprovalFixture(t)
	requireDistinctApproverForTest(t)

	grant(t, fixture.namespace, "schema-author",
		rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: specAuthorName},
		rbacv1.PolicyRule{
			APIGroups: []string{operatorv1alpha1.GroupVersion.Group},
			Resources: []string{"ptahschemas"}, Verbs: []string{"patch"},
		},
		rbacv1.PolicyRule{
			APIGroups: []string{operatorv1alpha1.GroupVersion.Group},
			Resources: []string{"ptahschemaapprovals"}, Verbs: []string{"create"},
		},
	)

	// The author edits their own schema and, in the same request, tries to
	// name someone else as the writer. Because this update changes spec, the
	// mutating webhook stamps the authenticated requester regardless of what
	// the payload's annotations claimed. The four-eyes control itself is
	// already on for this test, from the installation's own flag rather than
	// from anything this edit could reach.
	patch := []byte(`{
		"metadata": {"annotations": {"operator.ptah.run/last-spec-writer-username": "someone-else"}},
		"spec": {"policy": {"allowDestructive": true}}
	}`)
	target := &operatorv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: fixture.namespace, Name: fixture.schema.Name}}
	if err := specAuthorClient(t).Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatalf("author's spec-changing patch of %s was refused: %v", fixture.schema.Name, err)
	}

	stored := &operatorv1alpha1.PtahSchema{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(fixture.schema), stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Spec.Policy.AllowDestructive {
		t.Fatal("the author's patch did not persist allowDestructive")
	}
	wantUsername := "operator.ptah.run/last-spec-writer-username"
	wantUID := "operator.ptah.run/last-spec-writer-uid"
	if stored.Annotations[wantUsername] != specAuthorName || stored.Annotations[wantUID] != specAuthorUID {
		t.Fatalf("stored spec-writer annotations = %v, want %s (uid %s), not the forged identity",
			stored.Annotations, specAuthorName, specAuthorUID)
	}

	// A metadata-only update -- what the manager's own finalizer and status
	// writes look like from admission's point of view -- must not move the
	// recorded writer, even though it is made by someone else entirely.
	labeled := &operatorv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: fixture.namespace, Name: fixture.schema.Name}}
	labelPatch := []byte(`{"metadata": {"labels": {"reviewed": "true"}}}`)
	if err := admin.Patch(ctx, labeled, client.RawPatch(types.MergePatchType, labelPatch)); err != nil {
		t.Fatalf("metadata-only label patch of %s was refused: %v", fixture.schema.Name, err)
	}
	afterLabel := &operatorv1alpha1.PtahSchema{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(fixture.schema), afterLabel); err != nil {
		t.Fatal(err)
	}
	if afterLabel.Annotations[wantUsername] != specAuthorName || afterLabel.Annotations[wantUID] != specAuthorUID {
		t.Fatalf("a metadata-only update changed the recorded writer: %v", afterLabel.Annotations)
	}

	// The installation requires a distinct approver, and the author is the
	// one who last changed this schema's spec: their own approval is refused.
	selfApproval := fixture.approval("approve-by-author", fixture.plan.Spec.Fingerprint)
	err := specAuthorClient(t).Create(ctx, selfApproval)
	requireDenied(t, err, "mapproval.operator.ptah.run", "requires a distinct approver")

	// A different, authenticated approver is admitted, and the stored
	// approval names them rather than the author.
	distinctApproval := fixture.approval("approve-by-someone-else", fixture.plan.Spec.Fingerprint)
	if err := approverClient(t).Create(ctx, distinctApproval); err != nil {
		t.Fatalf("a distinct approver's approval was refused: %v", err)
	}
	storedApproval := &operatorv1alpha1.PtahSchemaApproval{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(distinctApproval), storedApproval); err != nil {
		t.Fatal(err)
	}
	if storedApproval.Spec.Approver.Username != approverName {
		t.Fatalf("stored approval names approver %q, want %q", storedApproval.Spec.Approver.Username, approverName)
	}
}
