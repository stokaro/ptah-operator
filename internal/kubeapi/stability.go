package kubeapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// EndpointProbe tests whether one directly addressed API server proves the
// caller's convergence invariant. A false result is an inconclusive
// observation, not an error.
type EndpointProbe func(context.Context, Endpoint) (proven bool, err error)

// StoredContractProbe verifies the storage-side contract that endpoint
// observations depend on. identity must change whenever that exact contract
// changes. A false result is an inconclusive observation, not an error.
type StoredContractProbe func(context.Context) (identity string, proven bool, err error)

// StabilityBarrier waits until every endpoint from one complete API-server
// inventory proves an invariant for one uninterrupted stability window.
//
// Provider failures, request timeouts, and inconclusive observations reset the
// window and are retried. Other probe and stored-contract errors are fatal.
// The optional StoredContract probe is evaluated before every endpoint sweep
// and once more when the stability window is ready to close.
type StabilityBarrier struct {
	Provider          Provider
	Probe             EndpointProbe
	StoredContract    StoredContractProbe
	StabilityDuration time.Duration
	PollEvery         time.Duration
	RequestTimeout    time.Duration
}

// Wait blocks until the barrier closes or ctx is canceled.
func (b *StabilityBarrier) Wait(ctx context.Context) error {
	return b.wait(ctx, time.Now, sleep)
}

func (b *StabilityBarrier) wait(
	ctx context.Context,
	now func() time.Time,
	wait func(context.Context, time.Duration) error,
) error {
	if err := b.validate(ctx, now, wait); err != nil {
		return err
	}

	var stableSince time.Time
	var stableKey stabilityKey
	reset := func() {
		stableSince = time.Time{}
		stableKey = stabilityKey{}
	}
	retry := func() error {
		if err := wait(ctx, b.PollEvery); err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			return fmt.Errorf("wait for next API-server stability sweep: %w", err)
		}
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		sweepStartedAt := now()

		storedIdentity, storedProven, retryStored, err := b.observeStoredContract(ctx)
		if err != nil {
			return err
		}
		if retryStored || !storedProven {
			reset()
			if err := retry(); err != nil {
				return err
			}
			continue
		}

		snapshot, retryProvider, err := b.observeSnapshot(ctx)
		if err != nil {
			return err
		}
		if retryProvider {
			reset()
			if err := retry(); err != nil {
				return err
			}
			continue
		}
		key, err := newStabilityKey(snapshot, storedIdentity)
		if err != nil {
			return err
		}

		eligible := !stableSince.IsZero() &&
			key.equal(stableKey) &&
			!sweepStartedAt.Before(stableSince) &&
			sweepStartedAt.Sub(stableSince) >= b.StabilityDuration

		allProven := true
		for _, endpoint := range snapshot.Endpoints {
			proven, retryProbe, probeErr := b.observeEndpoint(ctx, endpoint)
			if probeErr != nil {
				return probeErr
			}
			if retryProbe || !proven {
				allProven = false
			}
		}
		closedAt := now()
		if !allProven {
			reset()
			if err := retry(); err != nil {
				return err
			}
			continue
		}

		if stableSince.IsZero() || !key.equal(stableKey) || closedAt.Before(stableSince) {
			// A complete first sweep establishes the start of the window at its
			// closing instant. Its provider, storage, and endpoint request time
			// can therefore never satisfy any part of StabilityDuration.
			stableSince = closedAt
			stableKey = key
			if err := retry(); err != nil {
				return err
			}
			continue
		}
		if !eligible {
			if err := retry(); err != nil {
				return err
			}
			continue
		}

		closingStoredIdentity, closingStoredProven, retryStored, err := b.observeStoredContract(ctx)
		if err != nil {
			return err
		}
		if retryStored || !closingStoredProven || closingStoredIdentity != storedIdentity {
			reset()
			if err := retry(); err != nil {
				return err
			}
			continue
		}

		closingSnapshot, retryProvider, err := b.observeSnapshot(ctx)
		if err != nil {
			return err
		}
		if retryProvider {
			reset()
			if err := retry(); err != nil {
				return err
			}
			continue
		}
		closingKey, err := newStabilityKey(closingSnapshot, closingStoredIdentity)
		if err != nil {
			return err
		}
		if !closingKey.equal(key) {
			reset()
			if err := retry(); err != nil {
				return err
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
}

func (b *StabilityBarrier) validate(
	ctx context.Context,
	now func() time.Time,
	wait func(context.Context, time.Duration) error,
) error {
	if b == nil {
		return errors.New("API-server stability barrier is nil")
	}
	if ctx == nil {
		return errors.New("API-server stability context is nil")
	}
	if b.Provider == nil || b.Probe == nil {
		return errors.New("API-server stability provider and probe are required")
	}
	if b.StabilityDuration <= 0 || b.PollEvery <= 0 || b.RequestTimeout <= 0 {
		return errors.New("API-server stability timing values must be positive")
	}
	if now == nil || wait == nil {
		return errors.New("API-server stability clock and sleeper are required")
	}
	return ctx.Err()
}

func (b *StabilityBarrier) observeStoredContract(ctx context.Context) (string, bool, bool, error) {
	if b.StoredContract == nil {
		return "", true, false, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, b.RequestTimeout)
	identity, proven, err := b.StoredContract(requestCtx)
	requestContextErr := requestCtx.Err()
	cancel()
	if contextErr := ctx.Err(); contextErr != nil {
		return "", false, false, contextErr
	}
	if requestContextErr != nil || requestTimedOut(err) {
		return "", false, true, nil
	}
	if err != nil {
		return "", false, false, fmt.Errorf("verify stored API-server stability contract: %w", err)
	}
	if !proven {
		return "", false, false, nil
	}
	if identity == "" || identity != strings.TrimSpace(identity) {
		return "", false, false, errors.New("stored API-server stability contract identity is empty or padded")
	}
	return identity, true, false, nil
}

func (b *StabilityBarrier) observeSnapshot(ctx context.Context) (Snapshot, bool, error) {
	requestCtx, cancel := context.WithTimeout(ctx, b.RequestTimeout)
	snapshot, err := b.Provider(requestCtx)
	requestContextErr := requestCtx.Err()
	cancel()
	if contextErr := ctx.Err(); contextErr != nil {
		return Snapshot{}, false, contextErr
	}
	if err != nil || requestContextErr != nil {
		// Discovery is a dynamic observation. Even a non-timeout provider
		// failure is recoverable until the outer operation context expires.
		return Snapshot{}, true, nil
	}
	return snapshot, false, nil
}

func (b *StabilityBarrier) observeEndpoint(ctx context.Context, endpoint Endpoint) (bool, bool, error) {
	requestCtx, cancel := context.WithTimeout(ctx, b.RequestTimeout)
	proven, err := b.Probe(requestCtx, endpoint)
	requestContextErr := requestCtx.Err()
	cancel()
	if contextErr := ctx.Err(); contextErr != nil {
		return false, false, contextErr
	}
	if requestContextErr != nil || requestTimedOut(err) {
		return false, true, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("probe API-server stability on endpoint %q: %w", endpoint.Address, err)
	}
	return proven, false, nil
}

func requestTimedOut(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

type stabilityKey struct {
	inventory      string
	addresses      []string
	storedContract string
}

func newStabilityKey(snapshot Snapshot, storedContract string) (stabilityKey, error) {
	if snapshot.InventoryResourceVersion == "" || snapshot.InventoryResourceVersion != strings.TrimSpace(snapshot.InventoryResourceVersion) {
		return stabilityKey{}, errors.New("API-server stability inventory resourceVersion is empty or padded")
	}
	if snapshot.InventoryIdentity == "" || snapshot.InventoryIdentity != strings.TrimSpace(snapshot.InventoryIdentity) {
		return stabilityKey{}, errors.New("API-server stability inventory identity is empty or padded")
	}
	if len(snapshot.Endpoints) == 0 {
		return stabilityKey{}, errors.New("API-server stability endpoint snapshot is empty")
	}
	addresses := make([]string, 0, len(snapshot.Endpoints))
	seen := make(map[string]struct{}, len(snapshot.Endpoints))
	for _, endpoint := range snapshot.Endpoints {
		if endpoint.Address == "" || endpoint.Address != strings.TrimSpace(endpoint.Address) || endpoint.RESTConfig == nil {
			return stabilityKey{}, errors.New("API-server stability endpoint snapshot contains an incomplete endpoint")
		}
		if _, duplicate := seen[endpoint.Address]; duplicate {
			return stabilityKey{}, fmt.Errorf("API-server stability endpoint %q is duplicated", endpoint.Address)
		}
		seen[endpoint.Address] = struct{}{}
		addresses = append(addresses, endpoint.Address)
	}
	slices.Sort(addresses)
	return stabilityKey{
		inventory:      snapshot.InventoryIdentity,
		addresses:      addresses,
		storedContract: storedContract,
	}, nil
}

func (k stabilityKey) equal(other stabilityKey) bool {
	return k.inventory == other.inventory &&
		k.storedContract == other.storedContract &&
		slices.Equal(k.addresses, other.addresses)
}
