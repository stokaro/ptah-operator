package webhook_test

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestAcceptedEmptyNodeSelectorCanCreateItsJob(t *testing.T) {
	plane.Require(t)
	ctx := context.Background()
	fixture := newDispatchFixture(t, "empty-selector")
	// Preserve the explicit empty object through the real CRD API. Encoding a
	// typed resource first would omit it and would not exercise the native bug.
	if err := admin.Patch(ctx, fixture.schema, client.RawPatch(types.MergePatchType,
		[]byte(`{"spec":{"execution":{"nodeSelector":{}}}}`))); err != nil {
		t.Fatalf("store an explicit empty selector: %v", err)
	}
	stored := &operatorv1alpha1.PtahSchema{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(fixture.schema), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Execution.NodeSelector == nil || len(stored.Spec.Execution.NodeSelector) != 0 {
		t.Fatal("the API did not preserve the accepted empty selector")
	}
	job, err := manager.builder().Build(stored, *stored.Status.ActiveOperation, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientAs(t, manager.username).Create(ctx, job); err != nil {
		t.Fatalf("the accepted selector prevented the manager's Job: %v", err)
	}
	if job.UID == "" {
		t.Fatal("the admitted Job was not stored")
	}
}
