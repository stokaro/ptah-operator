package controller

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
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

// missingJobsMade guards making a reconciler's MissingJobs on first use, for
// the reason resultLogFailuresMade gives.
var missingJobsMade sync.Mutex

func missingJobsOf(field **mutationlifecycle.MissingJobs) *mutationlifecycle.MissingJobs {
	missingJobsMade.Lock()
	defer missingJobsMade.Unlock()
	if *field == nil {
		*field = &mutationlifecycle.MissingJobs{}
	}
	return *field
}

// monotonicNow reads clock, or time.Now when clock is nil, for a span this
// process measures between two of its own readings. It keeps time.Now's
// monotonic reading, which UTC would drop, so a step of the wall clock does
// not shorten the span.
func monotonicNow(clock func() time.Time) time.Time {
	if clock != nil {
		return clock()
	}
	return time.Now()
}

// awaitMarkedCreate ends a pass that found a marked Apply with no recorded Job
// and no Job before mutationlifecycle.MissingJobGrace has passed. Another
// manager may have marked it and still be creating the Job, so nothing is
// written and nothing is retired: the pass comes back when the grace ends, or
// sooner, when the Job's create wakes the owner.
//
// It ends the pass before the realm Lease is renewed. The wait acts on
// nothing, and a manager that marked the claim renewed the Lease just before,
// for far longer than the grace. For a claim whose dispatcher stopped long
// ago, the Lease may be near its end. A lapse is recorded as
// LeaseContinuityLost, the outcome is still unknown, and nothing can land
// behind it: that dispatcher sends no create.
func awaitMarkedCreate(ctx context.Context, jobName string, wait time.Duration) ctrl.Result {
	ctrl.LoggerFrom(ctx).Info("the marked Apply has no Job yet; waiting for a create another manager may still be making",
		"job", jobName, "wait", wait)
	return ctrl.Result{RequeueAfter: wait}
}
