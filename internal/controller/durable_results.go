package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultconsumer"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// OperationResults polls bounded background reads; it must never perform a
// blocking payload read in the reconcile worker. The manager supplies a Reader.
type OperationResults interface {
	Poll(context.Context, resultconsumer.Request) (resultconsumer.Result, error)
}

func durableDeliveryRequested(job *batchv1.Job) bool {
	if job == nil {
		return false
	}
	for _, container := range job.Spec.Template.Spec.Containers {
		for _, arg := range container.Args {
			if strings.HasPrefix(arg, "--result-") {
				return true
			}
		}
	}
	return false
}

func durableTerminalResult(ctx context.Context, reader client.Reader, results OperationResults, subject metav1.Object, kind string, job *batchv1.Job, snapshot *api.PodAdmissionSnapshot, request resultconsumer.Request) (terminalEvidence, error) {
	if snapshot == nil {
		return terminalEvidence{Durable: true}, fmt.Errorf("%w: active admission binding is missing", errTerminalPodIntent)
	}
	evidence, selected, err := collectTerminalPodEvidence(ctx, reader, subject.GetNamespace(), job, snapshot)
	evidence.Durable = true
	if err != nil {
		return evidence, err
	}
	config, err := jobconfig.Read(job, subject.GetUID(), request.OperationID)
	if err != nil {
		return evidence, fmt.Errorf("%w: durable delivery projection is invalid", errTerminalPodIntent)
	}
	if config.Generation != subject.GetGeneration() || job.UID != request.JobUID {
		evidence.ResultError = resultconsumer.ErrBinding
		return evidence, nil
	}
	request.Namespace = subject.GetNamespace()
	request.Name = subject.GetName()
	request.UID = subject.GetUID()
	request.Kind = kind
	request.Generation = config.Generation
	request.JobName = job.Name
	if selected != nil && !evidence.Trusted && selected.Status.Phase != corev1.PodFailed {
		return evidence, errTerminalPodPending
	}
	// A failed init container leaves the executor unstarted. The terminal Pod
	// cannot finish it later: read any durable receipt, or expose its absence
	// to the existing read-only retry and uncertain-Apply recovery paths.
	// Its identity is retained, but it is not trusted executor termination.
	if results == nil {
		return evidence, fmt.Errorf("durable result reader is not configured")
	}
	loaded, err := results.Poll(ctx, request)
	switch {
	case errors.Is(err, resultconsumer.ErrPending), errors.Is(err, resultconsumer.ErrStopped):
		return evidence, errResultReadCooling
	case errors.Is(err, resultstore.ErrIncomplete):
		// The usual arrival grace and unknown-outcome paths apply. No log or
		// termination-summary fallback can manufacture a durable receipt.
		evidence.ResultError = runner.ErrFrameNotFound
		return evidence, nil
	case errors.Is(err, resultstore.ErrConflict), errors.Is(err, resultstore.ErrInvalid), errors.Is(err, resultconsumer.ErrBinding), errors.Is(err, resultdelivery.ErrPayload):
		evidence.ResultError = err
		return evidence, nil
	case err != nil:
		return evidence, fmt.Errorf("%w: %w", errResultReadRetry, err)
	}
	publicationName, bindingErr := resultstore.Name(loaded.Binding)
	if bindingErr != nil || !request.Matches(loaded.Binding) || (loaded.Receipt.Name != publicationName && loaded.Receipt.Name != publicationName+"-complete") || loaded.Receipt.UID == "" || loaded.Receipt.Digest == "" || loaded.Receipt.Size <= 0 {
		evidence.ResultError = resultconsumer.ErrBinding
		return evidence, nil
	}
	if selected != nil && (selected.Name != loaded.Binding.PodName || selected.UID != loaded.Binding.PodUID) {
		return evidence, fmt.Errorf("%w: stored result belongs to another Pod", errTerminalPodIntent)
	}
	if selected == nil {
		// This is historical identity from the authenticated publication, not a
		// claim that a missing Pod proves SQL quiescence. Trusted stays false.
		evidence.PodUIDs = []types.UID{loaded.Binding.PodUID}
		evidence.PodCount = 1
	}
	evidence.Result = &loaded.Value
	return evidence, nil
}

func (e terminalEvidence) parseResult(operation runner.Operation, id string) (runner.Result, error) {
	if e.Durable {
		if e.ResultError != nil {
			return runner.Result{}, e.ResultError
		}
		if e.Result == nil {
			return runner.Result{}, runner.ErrFrameNotFound
		}
		if err := runner.ValidateResultFor(*e.Result, operation, id); err != nil {
			return runner.Result{}, err
		}
		return *e.Result, nil
	}
	result, err := runner.ParseResultFor(e.Logs, operation, id)
	if e.LogLost != nil {
		err = e.LogLost
	}
	return result, err
}
