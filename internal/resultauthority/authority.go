// Package resultauthority checks live Kubernetes authority for result delivery.
// It grants permission to persist evidence, never permission to execute SQL.
package resultauthority

import (
	"context"
	"errors"
	"reflect"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
)

// ErrNotReady is transient: the controller has not yet persisted the exact Job
// UID after Create. A fast runner must wait for adoption rather than lose its
// result as a definitive authority refusal.
var ErrNotReady = errors.New("result delivery is waiting for durable Job adoption")

// Authorizer must use an uncached API reader. It reads the subject again after
// checking Job and Pod so a concurrent retirement observed during the request
// cannot be mistaken for an active claim. Admission and the consumer must still
// cover changes after this read; Kubernetes offers no cross-object transaction.
type Authorizer struct{ Reader client.Reader }

func (a Authorizer) Check(ctx context.Context, identity resultdelivery.Identity) error {
	if a.Reader == nil {
		return errors.New("result authority API reader is required")
	}
	if _, err := resultdelivery.CertificateURI(identity); err != nil {
		return resultdelivery.ErrAuthority
	}
	claim, err := a.claim(ctx, identity)
	if err != nil {
		return err
	}
	b := identity.Binding
	job := &batchv1.Job{}
	if err := a.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.JobName}, job); err != nil {
		return readError(err)
	}
	if job.UID != b.JobUID || jobclaim.Match(job, claim) != nil || podintent.ValidateActiveJob(job) != nil {
		return resultdelivery.ErrAuthority
	}
	pod := &corev1.Pod{}
	if err := a.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.PodName}, pod); err != nil {
		return readError(err)
	}
	if pod.UID != b.PodUID || pod.Namespace != b.Namespace || podintent.ValidateStoredPod(pod, job, claim.Snapshot) != nil {
		return resultdelivery.ErrAuthority
	}
	// A second Pod makes the one-shot attempt ambiguous, even if the original
	// Pod is terminating. Do not let a replacement reuse its predecessor's
	// delivery identity while both objects exist. This is additional defense,
	// not a substitute for admission protecting credential projections.
	pods := &corev1.PodList{}
	if err := a.Reader.List(ctx, pods, client.InNamespace(b.Namespace), client.MatchingLabels{batchv1.ControllerUidLabel: string(job.UID)}, client.Limit(2)); err != nil {
		return readError(err)
	}
	if len(pods.Items) != 1 || pods.Continue != "" || pods.Items[0].Name != b.PodName || pods.Items[0].UID != b.PodUID {
		return resultdelivery.ErrAuthority
	}
	current, err := a.claim(ctx, identity)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, claim) {
		return resultdelivery.ErrAuthority
	}
	return ctx.Err()
}

func (a Authorizer) claim(ctx context.Context, identity resultdelivery.Identity) (jobclaim.Claim, error) {
	b := identity.Binding
	key := client.ObjectKey{Namespace: b.Namespace, Name: b.Name}
	var claim jobclaim.Claim
	var generation int64
	var engine string
	var dispatched, continuityLost bool
	switch b.Kind {
	case "PtahSchema":
		schema := &operatorv1alpha1.PtahSchema{}
		if err := a.Reader.Get(ctx, key, schema); err != nil {
			return claim, readError(err)
		}
		op := schema.Status.ActiveOperation
		if schema.UID != b.UID || op == nil || schema.Status.ExecutionBinding == nil {
			return claim, resultdelivery.ErrAuthority
		}
		generation = schema.Generation
		claim = jobclaim.SchemaOperation(schema, op)
		claim.Binding = schema.Status.ExecutionBinding
		claim.Stored = true
		if string(mutationlifecycle.SchemaOperation(op.Type).Runner) != b.Operation {
			return claim, resultdelivery.ErrAuthority
		}
		if op.Target != nil {
			engine = strings.ToLower(string(op.Target.Engine))
		}
		dispatched, continuityLost = op.DispatchStarted, op.LeaseContinuityLost
	case "PtahMigration":
		migration := &operatorv1alpha1.PtahMigration{}
		if err := a.Reader.Get(ctx, key, migration); err != nil {
			return claim, readError(err)
		}
		op := migration.Status.ActiveOperation
		if migration.UID != b.UID || op == nil || migration.Status.ExecutionBinding == nil {
			return claim, resultdelivery.ErrAuthority
		}
		generation = migration.Generation
		claim = jobclaim.MigrationOperation(migration, op)
		claim.Binding = migration.Status.ExecutionBinding
		claim.Stored = true
		if string(mutationlifecycle.MigrationOperation(op.Type).Runner) != b.Operation {
			return claim, resultdelivery.ErrAuthority
		}
		if op.Target != nil {
			engine = strings.ToLower(string(op.Target.Engine))
		}
		dispatched, continuityLost = op.DispatchStarted, op.LeaseContinuityLost
	default:
		return claim, resultdelivery.ErrAuthority
	}
	if generation != b.Generation || engine != identity.Engine || claim.ID != b.OperationID || claim.InputFingerprint != b.InputFingerprint ||
		claim.Epoch != b.ExecutionBindingID || claim.Binding.Epoch != b.ExecutionBindingID || claim.JobName != b.JobName || continuityLost {
		return claim, resultdelivery.ErrAuthority
	}
	if claim.JobUID == "" {
		return claim, ErrNotReady
	}
	if claim.JobUID != b.JobUID || claim.Mutating && !dispatched {
		return claim, resultdelivery.ErrAuthority
	}
	// Do not reject merely because executionNotAfter passed, the original Pod
	// is terminating, or the resource is suspended. Those states may be when
	// an already dispatched runner reports its outcome. No result is permission
	// to start or replay SQL; retirement or a changed epoch still refuses it.
	return claim, nil
}

func readError(err error) error {
	if apierrors.IsNotFound(err) {
		return resultdelivery.ErrAuthority
	}
	return err
}
