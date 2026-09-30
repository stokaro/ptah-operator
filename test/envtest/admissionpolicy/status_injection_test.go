package admissionpolicy_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// The unsupported-state acceptance control patches one version field. Hold
// that exact request to the installed chart: a manager may inject it, an
// otherwise authorized user may not. An unset policy reason returns Invalid,
// not Forbidden; checking Forbidden alone hid a successful policy refusal.
func TestStatusVersionInjectionRequiresManager(t *testing.T) {
	plane.Require(t)
	ctx := context.Background()
	if err := env.WaitFor(ctx, buildCatalog(t).rows, time.Minute); err != nil {
		t.Fatal(err)
	}
	ordinary, err := env.As(policyenv.User())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := env.As(env.Manager())
	if err != nil {
		t.Fatal(err)
	}
	policyName := policy(t, "ptah-operator-status-write-guard-")
	for _, fixture := range []client.Object{tenantSchema, tenantMigration} {
		live := fixture.DeepCopyObject().(client.Object)
		if err := env.Admin.Get(ctx, client.ObjectKeyFromObject(live), live); err != nil {
			t.Fatal(err)
		}
		patch := fmt.Sprintf(`[{"op":"test","path":"/metadata/resourceVersion","value":%q},{"op":"test","path":"/status/executionBinding/controllerStateVersion","value":1},{"op":"replace","path":"/status/executionBinding/controllerStateVersion","value":2}]`, live.GetResourceVersion())
		err := ordinary.Status().Patch(ctx, live.DeepCopyObject().(client.Object), client.RawPatch(types.JSONPatchType, []byte(patch)), client.DryRunAll)
		verdict := policyenv.Decide(err)
		var status apierrors.APIStatus
		if verdict.Policy != policyName || verdict.Binding != policyName || !errors.As(err, &status) {
			t.Fatalf("ordinary status writer: %s", verdict)
		}
		if err := manager.Status().Patch(ctx, live.DeepCopyObject().(client.Object), client.RawPatch(types.JSONPatchType, []byte(patch)), client.DryRunAll); err != nil {
			t.Fatalf("manager did not admit the identical request: %v", err)
		}
		response := status.Status()
		if response.Status != metav1.StatusFailure || response.Reason != metav1.StatusReasonInvalid || response.Code != 422 || response.Details == nil || response.Details.Name != live.GetName() {
			t.Fatal("the default policy denial lost its actual API response or resource identity")
		}
		t.Logf("%s: reason=%s code=%d, exact policy and binding denied; manager admitted", response.Details.Kind, response.Reason, response.Code)
	}
}
