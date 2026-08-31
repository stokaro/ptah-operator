package certrotation

import (
	"context"
	"errors"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

type leaseConfig struct {
	Namespace      string
	Name           string
	HolderIdentity string
	Duration       time.Duration
	AcquireTimeout time.Duration
	Now            func() time.Time
}

type leaseGuard struct {
	client          kubernetes.Interface
	config          leaseConfig
	ctx             context.Context
	cancel          context.CancelCauseFunc
	renewalFinished chan struct{}
}

func acquireLease(ctx context.Context, client kubernetes.Interface, config leaseConfig) (*leaseGuard, error) {
	acquireCtx, cancel := context.WithTimeout(ctx, config.AcquireTimeout)
	defer cancel()

	var lastAPIError error
	err := wait.PollUntilContextTimeout(acquireCtx, 250*time.Millisecond, config.AcquireTimeout, true, func(ctx context.Context) (bool, error) {
		leases := client.CoordinationV1().Leases(config.Namespace)
		lease, err := leases.Get(ctx, config.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, fmt.Errorf("chart-managed Lease %q does not exist", config.Name)
		}
		if err != nil {
			lastAPIError = err
			if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) {
				return false, err
			}
			return false, nil
		}
		if !leaseAvailable(lease, config.HolderIdentity, config.Now()) {
			return false, nil
		}
		claimLease(lease, config.HolderIdentity, config.Duration, config.Now())
		if _, err := leases.Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			lastAPIError = err
			if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) {
				return false, err
			}
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		if lastAPIError != nil && !errors.Is(err, lastAPIError) {
			return nil, errors.Join(err, lastAPIError)
		}
		return nil, err
	}

	guardCtx, guardCancel := context.WithCancelCause(ctx)
	guard := &leaseGuard{
		client:          client,
		config:          config,
		ctx:             guardCtx,
		cancel:          guardCancel,
		renewalFinished: make(chan struct{}),
	}
	go guard.renew()
	return guard, nil
}

func (guard *leaseGuard) Context() context.Context {
	return guard.ctx
}

func (guard *leaseGuard) Close(ctx context.Context) error {
	guard.cancel(context.Canceled)
	select {
	case <-guard.renewalFinished:
	case <-ctx.Done():
		return ctx.Err()
	}

	leases := guard.client.CoordinationV1().Leases(guard.config.Namespace)
	lease, err := leases.Get(ctx, guard.config.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != guard.config.HolderIdentity {
		return nil
	}
	lease.Spec.HolderIdentity = nil
	lease.Spec.AcquireTime = nil
	lease.Spec.RenewTime = nil
	lease.Spec.LeaseDurationSeconds = nil
	_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
	return err
}

func (guard *leaseGuard) renew() {
	defer close(guard.renewalFinished)
	interval := guard.config.Duration / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-guard.ctx.Done():
			return
		case <-ticker.C:
			if err := guard.renewOnce(); err != nil {
				guard.cancel(fmt.Errorf("renew Lease %q: %w", guard.config.Name, err))
				return
			}
		}
	}
}

func (guard *leaseGuard) renewOnce() error {
	leases := guard.client.CoordinationV1().Leases(guard.config.Namespace)
	lease, err := leases.Get(guard.ctx, guard.config.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != guard.config.HolderIdentity {
		return errors.New("holder identity changed")
	}
	now := metav1.NewMicroTime(guard.config.Now())
	lease.Spec.RenewTime = &now
	durationSeconds := durationSeconds(guard.config.Duration)
	lease.Spec.LeaseDurationSeconds = &durationSeconds
	_, err = leases.Update(guard.ctx, lease, metav1.UpdateOptions{})
	return err
}

func leaseAvailable(lease *coordinationv1.Lease, holder string, now time.Time) bool {
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" || *lease.Spec.HolderIdentity == holder {
		return true
	}
	if lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 0 {
		return false
	}
	renewed := lease.Spec.RenewTime
	if renewed == nil {
		renewed = lease.Spec.AcquireTime
	}
	if renewed == nil {
		return false
	}
	expires := renewed.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	return !now.Before(expires)
}

func claimLease(lease *coordinationv1.Lease, holder string, duration time.Duration, now time.Time) {
	transition := lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder
	lease.Spec.HolderIdentity = &holder
	durationValue := durationSeconds(duration)
	lease.Spec.LeaseDurationSeconds = &durationValue
	nowValue := metav1.NewMicroTime(now)
	if transition || lease.Spec.AcquireTime == nil {
		lease.Spec.AcquireTime = &nowValue
		transitions := int32(1)
		if lease.Spec.LeaseTransitions != nil {
			transitions += *lease.Spec.LeaseTransitions
		}
		lease.Spec.LeaseTransitions = &transitions
	}
	lease.Spec.RenewTime = &nowValue
}

func durationSeconds(duration time.Duration) int32 {
	seconds := (duration + time.Second - 1) / time.Second
	return int32(seconds)
}
