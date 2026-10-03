package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/stokaro/ptah-operator/internal/certrotation"
)

type rotationRunner interface {
	Run(context.Context) (certrotation.Result, error)
}

type supervisorConfig struct {
	RunInterval      time.Duration
	OperationTimeout time.Duration
	RetryInitial     time.Duration
	RetryMax         time.Duration
	ResultRotation   resultRotationOptions
}

func (c supervisorConfig) validate() error {
	switch {
	case c.RunInterval <= 0:
		return errors.New("run interval must be positive")
	case c.OperationTimeout <= 0:
		return errors.New("operation timeout must be positive")
	case c.RetryInitial <= 0:
		return errors.New("initial retry delay must be positive")
	case c.RetryMax <= 0:
		return errors.New("maximum retry delay must be positive")
	case c.RetryMax < c.RetryInitial:
		return errors.New("maximum retry delay must not be less than the initial retry delay")
	default:
		return nil
	}
}

func validateRuntimeRelationships(supervisor supervisorConfig, rotation certrotation.Config) error {
	if rotation.RenewalThreshold <= 0 || rotation.ServingCertificateValidity <= rotation.RenewalThreshold {
		return errors.New("serving certificate validity must exceed the positive renewal threshold")
	}
	rotationWindow := rotation.ServingCertificateValidity - rotation.RenewalThreshold
	if rotation.AcquireTimeout <= 0 || rotation.ProbeTimeout <= 0 {
		return errors.New("Lease acquire timeout and probe timeout must be positive")
	}
	const maximumDuration = time.Duration(1<<63 - 1)
	// The pass that switches a recovered CA probes twice: once to prove the
	// recovered CA signs the live certificate, once to prove the new one is
	// served.
	if rotation.ProbeTimeout > (maximumDuration-rotation.AcquireTimeout)/2 {
		return errors.New("the Lease acquire timeout plus two probe timeouts exceeds the supported duration")
	}
	minimumOperationTimeout := rotation.AcquireTimeout + 2*rotation.ProbeTimeout
	if supervisor.OperationTimeout <= minimumOperationTimeout {
		return fmt.Errorf(
			"operation timeout must exceed Lease acquire timeout plus two probe timeouts (%s)",
			minimumOperationTimeout,
		)
	}
	if supervisor.RunInterval > maximumDuration-supervisor.OperationTimeout {
		return errors.New("run interval plus operation timeout exceeds the supported duration")
	}
	scheduledCycle := supervisor.RunInterval + supervisor.OperationTimeout
	if scheduledCycle >= rotationWindow {
		return fmt.Errorf(
			"run interval plus operation timeout must be shorter than serving certificate validity minus renewal threshold (%s)",
			rotationWindow,
		)
	}
	// A pass may notice a certificate inside the renewal threshold up to one
	// scheduled cycle late, and the CA switch follows the delay after that.
	// Both have to fit before the certificate that started it expires.
	if rotation.CASwitchDelay <= 0 || scheduledCycle > maximumDuration-rotation.CASwitchDelay {
		return errors.New("CA switch delay must be positive and fit the supported duration")
	}
	if scheduledCycle+rotation.CASwitchDelay >= rotation.RenewalThreshold {
		return fmt.Errorf(
			"run interval plus operation timeout plus CA switch delay must be shorter than the renewal threshold (%s)",
			rotation.RenewalThreshold,
		)
	}
	return nil
}

type rotationSupervisor struct {
	runner rotationRunner
	config supervisorConfig
	probes *probeState
	logger *slog.Logger
	wait   func(context.Context, time.Duration) bool
	jitter func(time.Duration, time.Duration) time.Duration
}

func newSupervisor(
	runner rotationRunner,
	config supervisorConfig,
	probes *probeState,
	logger *slog.Logger,
) *rotationSupervisor {
	return &rotationSupervisor{
		runner: runner,
		config: config,
		probes: probes,
		logger: logger,
		wait:   waitFor,
		jitter: retryDelayWithJitter,
	}
}

func (s *rotationSupervisor) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("supervisor context is required")
	}
	if s.runner == nil {
		return errors.New("certificate rotator is required")
	}
	if s.probes == nil {
		return errors.New("probe state is required")
	}
	if s.logger == nil {
		return errors.New("logger is required")
	}
	if s.wait == nil {
		return errors.New("wait function is required")
	}
	if s.jitter == nil {
		return errors.New("retry jitter function is required")
	}
	if err := s.config.validate(); err != nil {
		return err
	}

	s.probes.setLive(true)
	defer func() {
		s.probes.setReady(false)
		s.probes.setLive(false)
	}()

	retryDelay := s.config.RetryInitial
	for {
		if ctx.Err() != nil {
			return nil
		}

		s.logger.Info("reconciling generated certificates")
		operationCtx, cancel := context.WithTimeout(ctx, s.config.OperationTimeout)
		result, err := s.runner.Run(operationCtx)
		operationErr := operationCtx.Err()
		cancel()

		if ctx.Err() != nil {
			return nil
		}
		if err == nil && operationErr != nil {
			err = operationErr
		}

		if err == nil {
			s.probes.setReady(!result.Pending)
			retryDelay = s.config.RetryInitial
			next := s.config.RunInterval
			if result.RequeueAfter > 0 {
				next = min(next, result.RequeueAfter)
				s.logger.Info("CA transition waits for its switch", "switch_in", result.RequeueAfter)
			} else {
				s.logger.Info("generated certificates are current")
			}
			if !s.wait(ctx, next) {
				return nil
			}
			continue
		}

		s.probes.setReady(false)
		delay := clampRetryDelay(s.jitter(retryDelay, s.config.RetryMax), retryDelay, s.config.RetryMax)
		s.logger.Error(
			"certificate reconciliation failed; retrying",
			"error", sanitizedReconcileError(err),
			"retry_after", delay,
		)
		if !s.wait(ctx, delay) {
			return nil
		}
		retryDelay = nextRetryDelay(retryDelay, s.config.RetryMax)
	}
}

func waitFor(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func retryDelayWithJitter(base, maximum time.Duration) time.Duration {
	if base >= maximum {
		return maximum
	}
	maximumExtra := min(base/5, maximum-base)
	if maximumExtra <= 0 {
		return base
	}
	return base + time.Duration(rand.Int64N(int64(maximumExtra)+1))
}

func clampRetryDelay(delay, base, maximum time.Duration) time.Duration {
	return min(max(delay, base), maximum)
}

func nextRetryDelay(current, maximum time.Duration) time.Duration {
	if current >= maximum || current > maximum-current {
		return maximum
	}
	return min(current*2, maximum)
}

// sanitizedReconcileError deliberately returns only fixed classifications.
// Kubernetes API errors can include serialized object values, and certificate
// reconciliation handles private key material that must never reach logs.
func sanitizedReconcileError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "operation timed out"
	case errors.Is(err, context.Canceled):
		return "operation was canceled"
	case apierrors.IsUnauthorized(err):
		return "Kubernetes API request was unauthorized"
	case apierrors.IsForbidden(err):
		return "Kubernetes API request was forbidden"
	case apierrors.IsNotFound(err):
		return "required Kubernetes object was not found"
	case apierrors.IsConflict(err):
		return "Kubernetes API update conflicted"
	case apierrors.IsTooManyRequests(err):
		return "Kubernetes API rate limit was reached"
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return "Kubernetes API request timed out"
	case apierrors.IsServiceUnavailable(err):
		return "Kubernetes API service was unavailable"
	default:
		return fmt.Sprintf("reconciliation failed (%T)", err)
	}
}
