package controller

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// TestAnUnconfirmedApplyDispatchIsNeverRetried covers the three ways the
// dispatch boundary can fail to confirm what it just did, and the reason the
// boundary exists at all: an API call that does not answer is not an API call
// that did not happen.
//
// A create whose response was lost leaves a Job running SQL under the name the
// claim reserved. A read that fails afterwards leaves the operator unable to
// say whether that Job is its own. A Job whose shape does not match the
// immutable claim is a Job the operator did not describe, standing where it
// expected its own. A read-only operation retries through all three, because
// re-running a read costs nothing; an Apply cannot, so each one settles the
// claim with an unknown outcome instead.
//
// Every one of these branches was uncovered. Nothing in the package could
// dispatch a schema Apply until the fixture below existed.
func TestAnUnconfirmedApplyDispatchIsNeverRetried(t *testing.T) {
	t.Parallel()

	lost := errors.New("connection reset by peer")

	for _, row := range []struct {
		name    string
		install func(t *testing.T, reconciler *SchemaReconciler, api client.Client, jobName string)
	}{
		{
			// The write may have landed. Its answer did not.
			name: "the create never answered",
			install: func(_ *testing.T, reconciler *SchemaReconciler, _ client.Client, jobName string) {
				reconciler.Client = &dispatchFaultClient{
					Client: reconciler.Client, jobName: jobName,
					created: func(*batchv1.Job) error { return lost },
				}
			},
		},
		{
			// The Job exists and the operator cannot say whether it is the one
			// it created.
			name: "the confirming read failed",
			install: func(_ *testing.T, reconciler *SchemaReconciler, api client.Client, jobName string) {
				dispatched := new(bool)
				reconciler.Client = &dispatchFaultClient{
					Client: reconciler.Client, jobName: jobName,
					created: func(*batchv1.Job) error { *dispatched = true; return nil },
				}
				reconciler.APIReader = &jobReadFaultReader{
					Reader: api, jobName: jobName, failure: lost, after: dispatched,
				}
			},
		},
		{
			// Something stands under the reserved name that the operator did
			// not describe. It may already be executing SQL.
			name: "the created Job is not the one the claim describes",
			install: func(_ *testing.T, reconciler *SchemaReconciler, _ client.Client, jobName string) {
				reconciler.Client = &dispatchFaultClient{
					Client: reconciler.Client, jobName: jobName,
					mutate: func(job *batchv1.Job) {
						job.Annotations[workload.AnnotationExecutionBindingID] =
							"v1-99999999999999999999999999999999"
					},
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			reconciler, api, schema, jobName := dispatchableApply(t, false)
			row.install(t, reconciler, api, jobName)

			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
			settled := false
			for pass := range 4 {
				if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
					t.Fatalf("Reconcile() pass %d error = %v", pass, err)
				}
				current := safetyGetSchema(t, api, schema)
				if current.Status.PendingObservation != nil {
					if current.Status.PendingObservation.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
						t.Fatalf("pending observation = %#v, want an unknown outcome",
							current.Status.PendingObservation)
					}
					if current.Status.ActiveOperation != nil {
						t.Fatalf("the settled claim outlived its own settlement: %#v",
							current.Status.ActiveOperation)
					}
					settled = true
					break
				}
				if operation := current.Status.ActiveOperation; operation != nil && operation.Attempt > 1 {
					t.Fatalf("an unconfirmed Apply dispatch was retried as attempt %d", operation.Attempt)
				}
			}
			if !settled {
				t.Fatalf("an unconfirmed Apply dispatch was never settled: %#v",
					safetyGetSchema(t, api, schema).Status)
			}
		})
	}
}

// dispatchFaultClient interferes with the one Create the dispatch boundary is
// permitted to make: mutate rewrites the Job that actually lands, created
// reports a failure after it has landed.
type dispatchFaultClient struct {
	client.Client
	jobName string
	mutate  func(*batchv1.Job)
	created func(*batchv1.Job) error
}

func (c *dispatchFaultClient) Create(
	ctx context.Context,
	object client.Object,
	options ...client.CreateOption,
) error {
	job, ok := object.(*batchv1.Job)
	if !ok || job.Name != c.jobName {
		return c.Client.Create(ctx, object, options...)
	}
	if c.mutate != nil {
		c.mutate(job)
	}
	if err := c.Client.Create(ctx, job, options...); err != nil {
		return err
	}
	if c.created != nil {
		return c.created(job)
	}
	return nil
}

// jobReadFaultReader refuses to read back one Job by name.
type jobReadFaultReader struct {
	client.Reader
	jobName string
	failure error
	after   *bool
}

func (r *jobReadFaultReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if _, ok := object.(*batchv1.Job); ok && key.Name == r.jobName && (r.after == nil || *r.after) {
		return r.failure
	}
	return r.Reader.Get(ctx, key, object, options...)
}
