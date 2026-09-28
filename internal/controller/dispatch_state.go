package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/podintent"
)

// Both families answer "may a Job exist for this claim" from the same two
// fields, and answered it inline at ten sites. These adapters are the whole of
// the difference: the decision is in internal/mutationlifecycle, so a claim
// that grows a third dispatch marker is added in one place rather than found
// missing at the tenth.

func schemaMayHaveDispatched(operation *operatorv1alpha1.ActiveOperationStatus) bool {
	if operation == nil {
		return false
	}
	return mutationlifecycle.MayHaveDispatched(mutationlifecycle.DispatchState{
		DispatchStarted: operation.DispatchStarted,
		JobUID:          string(operation.JobUID),
	})
}

func migrationMayHaveDispatched(operation *operatorv1alpha1.MigrationOperationStatus) bool {
	if operation == nil {
		return false
	}
	return mutationlifecycle.MayHaveDispatched(mutationlifecycle.DispatchState{
		DispatchStarted: operation.DispatchStarted,
		JobUID:          string(operation.JobUID),
	})
}

// podsStopped reports whether every Pod the Job owned has stopped. A Job's
// deletion is not evidence that an already-created Pod has, so a read-only
// claim whose recorded Job is gone polls the exact owner UID until each old
// attempt is terminal or gone before it creates anything under a new name.
func podsStopped(
	ctx context.Context,
	reader client.Reader,
	namespace, jobName string,
	jobUID types.UID,
) (bool, error) {
	pods, err := podsOwnedByJob(ctx, reader, namespace, jobName, jobUID)
	if err != nil {
		return false, err
	}
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			return false, nil
		}
	}
	return true, nil
}

// podAdmission is podintent through the reader, namespace and options one
// reconciler pass holds. It is the same for both families.
type podAdmission struct {
	reader    client.Reader
	namespace string
	options   podintent.Options
}

func (a podAdmission) Resolve(
	ctx context.Context,
	template *corev1.PodTemplateSpec,
) (*operatorv1alpha1.PodAdmissionSnapshot, error) {
	return podintent.Resolve(ctx, a.reader, a.namespace, template, a.options)
}

func (podAdmission) Validate(snapshot *operatorv1alpha1.PodAdmissionSnapshot) error {
	return podintent.ValidateSnapshot(snapshot)
}

func (podAdmission) Digest(template *corev1.PodTemplateSpec) (string, error) {
	return podintent.DigestTemplate(template)
}

var _ mutationlifecycle.Admission = podAdmission{}
