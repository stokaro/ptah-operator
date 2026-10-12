package resultcredentials

import (
	"context"
	"testing"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Model the mandatory write guard, which authorizes the submitted public pin
// against current state before the API persists it.
type admittedPodBindingClient struct {
	client.Client
	reader client.Reader
}

func (c admittedPodBindingClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if err := (PodBindings{Reader: c.reader}).ValidateRecordCreate(ctx, object.(*api.PtahResultRecord)); err != nil {
		return err
	}
	return c.Client.Create(ctx, object, options...)
}

type podBindingReadCounter struct {
	client.Reader
	workloadChecks int
}

func (r *podBindingReadCounter) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if _, ok := list.(*corev1.PodList); ok {
		r.workloadChecks++
	}
	return r.Reader.List(ctx, list, options...)
}

func TestPodBindingDoesNotDuplicateTheAdmissionAuthorityCheck(t *testing.T) {
	for _, operation := range []string{"schema-observe", "migration-history"} {
		t.Run(operation, func(t *testing.T) {
			f := resulttest.New(t, operation)
			apiClient := &credentialAPI{Client: f.Client(t)}
			reader := &podBindingReadCounter{Reader: apiClient}
			pins := PodBindings{Writer: admittedPodBindingClient{Client: apiClient, reader: reader}, Reader: reader}
			credential, err := pins.Ensure(t.Context(), f.Identity)
			if err != nil || credential.UID == "" || apiClient.creates.Load() != 1 {
				t.Fatalf("authorized enrollment was not persisted: credential=%+v err=%v", credential, err)
			}
			// Each authority check lists Pods once. Admission must authorize
			// CREATE, and Ensure must recheck after its persisted readback.
			if reader.workloadChecks != 2 {
				t.Fatalf("one enrollment checked workload authority %d times, want admission and return only", reader.workloadChecks)
			}
		})
	}
}
