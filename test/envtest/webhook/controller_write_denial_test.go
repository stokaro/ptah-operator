package webhook_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// unreadableOwnerPrefix names the namespaces in which the Job-write validator
// cannot read a PtahSchema, as when the API server it reads through is
// overloaded. Every other read goes to the API server.
const unreadableOwnerPrefix = "unreadable-owner"

// unreadableOwners is the validator's reader: the test administrator, with
// PtahSchema reads failing in the unreadableOwnerPrefix namespaces.
type unreadableOwners struct {
	client.Reader
}

func (r unreadableOwners) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, isSchema := object.(*operatorv1alpha1.PtahSchema); isSchema && strings.HasPrefix(key.Namespace, unreadableOwnerPrefix+"-") {
		return apierrors.NewServiceUnavailable("the PtahSchema read was refused by the test")
	}
	return r.Reader.Get(ctx, key, object, options...)
}

// The Job-write webhook answers a read it could not make with 500, and the API
// server keeps that code on the webhook's denial. Admission runs before
// storage, so nothing is stored, and a dispatcher that read the 500 as an
// outcome nobody established retired its claim with the approval spent
// (#979). The row sends the manager's own Job through the API server and holds
// the answer it returns to what the dispatcher concludes from it.
func TestAControllerWriteDenialWithA500IsARefusal(t *testing.T) {
	plane.Require(t)
	fixture := newDispatchFixture(t, unreadableOwnerPrefix)
	ctx := context.Background()

	err := clientAs(t, manager.username).Create(ctx, fixture.job.DeepCopy())
	requireDenied(t, err, controllerWriteWebhook, "directly read PtahSchema "+fixture.namespace+"/"+fixture.schema.Name)
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		t.Fatalf("the API server's answer %v carries no status", err)
	}
	if code := status.Status().Code; code != http.StatusInternalServerError {
		t.Fatalf("the API server answered the webhook's denial with code %d, want the webhook's 500: %v", code, err)
	}
	if answer := mutationlifecycle.ClassifyCreate(err); answer != mutationlifecycle.AnswerRefused {
		t.Fatalf("ClassifyCreate(%v) = %q, want %q", err, answer, mutationlifecycle.AnswerRefused)
	}
	stored := &batchv1.Job{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(fixture.job), stored); !apierrors.IsNotFound(err) {
		t.Fatalf("read Job %s/%s after the denial: %v, want it absent", fixture.namespace, fixture.job.Name, err)
	}
}
