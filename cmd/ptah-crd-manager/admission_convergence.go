package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

const (
	admissionConvergencePollEvery         = 250 * time.Millisecond
	admissionConvergenceStabilityWindow   = 5 * time.Second
	admissionConvergenceRequestTimeout    = 5 * time.Second
	certificateRecoveryGuardDenialMessage = "certificate rotator Secret CREATE is outside its exact recovery contract"
)

type admissionConvergenceProbe func(context.Context) (bool, error)

type namedAdmissionConvergenceProbe struct {
	name             string
	topologyIdentity string
	probe            admissionConvergenceProbe
}

type admissionConvergenceEndpointProvider func(context.Context) ([]namedAdmissionConvergenceProbe, error)

type admissionConvergenceStabilityObserver interface {
	Observe(context.Context, string) (identity string, proven bool, err error)
	Close()
}

type admissionConvergenceBarrier struct {
	endpoints         []namedAdmissionConvergenceProbe
	endpointProvider  admissionConvergenceEndpointProvider
	verifyStored      func(context.Context) error
	pollEvery         time.Duration
	stabilityDuration time.Duration
	requestTimeout    time.Duration

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

type admissionMarkerClientFactory func(*rest.Config) (crdupgrade.AdmissionConvergenceMarkerClient, error)
type admissionMarkerProbe func(context.Context, crdupgrade.AdmissionConvergenceMarkerClient) (bool, error)

func newPreCutoverAdmissionConvergenceBarrier(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	guard *crdupgrade.AdmissionConvergenceGuard,
	serviceAccountObjectGuard *crdupgrade.ServiceAccountObjectGuard,
) (*admissionConvergenceBarrier, error) {
	if guard == nil {
		return nil, errors.New("admission convergence guard is required")
	}
	var expectedState crdupgrade.ReleaseActivationState
	err := waitForStoredAdmissionConvergence(
		ctx,
		admissionConvergencePollEvery,
		admissionConvergenceRequestTimeout,
		sleepForAdmissionConvergence,
		func(verifyCtx context.Context) error {
			var verifyErr error
			expectedState, verifyErr = guard.VerifyPreCutover(verifyCtx)
			return verifyErr
		},
	)
	if err != nil {
		return nil, fmt.Errorf("verify pre-cutover admission convergence sentinel: %w", err)
	}
	return newExpectedAdmissionConvergenceBarrier(ctx, config, endpointSlices, guard, serviceAccountObjectGuard, expectedState)
}

func newExpectedAdmissionConvergenceBarrier(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	guard *crdupgrade.AdmissionConvergenceGuard,
	serviceAccountObjectGuard *crdupgrade.ServiceAccountObjectGuard,
	expectedState crdupgrade.ReleaseActivationState,
) (*admissionConvergenceBarrier, error) {
	if guard == nil {
		return nil, errors.New("admission convergence guard is required")
	}
	verifyStored, markerProbe, err := withServiceAccountObjectConvergence(
		func(verifyCtx context.Context) error {
			return guard.VerifyState(verifyCtx, expectedState)
		},
		func(probeCtx context.Context, client crdupgrade.AdmissionConvergenceMarkerClient) (bool, error) {
			return guard.Probe(probeCtx, client, expectedState)
		},
		serviceAccountObjectGuard,
		true,
	)
	if err != nil {
		return nil, err
	}
	return newMarkerAdmissionConvergenceBarrier(ctx, config, endpointSlices, guard, verifyStored, newDirectAdmissionMarkerClientForNamespace(guard.ReleaseNamespace), markerProbe)
}

func newSealedAdmissionConvergenceBarrier(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	guard *crdupgrade.AdmissionConvergenceGuard,
	serviceAccountObjectGuard *crdupgrade.ServiceAccountObjectGuard,
	expectedState crdupgrade.ReleaseActivationState,
) (*admissionConvergenceBarrier, error) {
	if guard == nil {
		return nil, errors.New("admission convergence guard is required")
	}
	verifyStored, markerProbe, err := withServiceAccountObjectConvergence(
		func(verifyCtx context.Context) error {
			return guard.VerifySealedState(verifyCtx, expectedState)
		},
		func(probeCtx context.Context, client crdupgrade.AdmissionConvergenceMarkerClient) (bool, error) {
			return guard.ProbeSealed(probeCtx, client, expectedState)
		},
		serviceAccountObjectGuard,
		true,
	)
	if err != nil {
		return nil, err
	}
	return newMarkerAdmissionConvergenceBarrier(ctx, config, endpointSlices, guard, verifyStored, newDirectAdmissionMarkerClientForNamespace(guard.ReleaseNamespace), markerProbe)
}

func newRuntimeAdmissionConvergenceBarrier(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	guard *crdupgrade.AdmissionConvergenceGuard,
	serviceAccountObjectGuard *crdupgrade.ServiceAccountObjectGuard,
) (*admissionConvergenceBarrier, error) {
	if guard == nil {
		return nil, errors.New("admission convergence guard is required")
	}
	expectedState := crdupgrade.ReleaseActivationState{
		ActiveReleaseSequence:     guard.ReleaseSequence,
		ControllerCredentialPhase: crdupgrade.ControllerCredentialsActive,
	}
	verifyStored, markerProbe, err := withServiceAccountObjectConvergence(
		func(verifyCtx context.Context) error {
			return guard.VerifySealedState(verifyCtx, expectedState)
		},
		func(probeCtx context.Context, client crdupgrade.AdmissionConvergenceMarkerClient) (bool, error) {
			return guard.ProbeSealed(probeCtx, client, expectedState)
		},
		serviceAccountObjectGuard,
		true,
	)
	if err != nil {
		return nil, err
	}
	return newMarkerAdmissionConvergenceBarrier(ctx, config, endpointSlices, guard, verifyStored, newDirectAdmissionMarkerClientForNamespace(guard.ReleaseNamespace), markerProbe)
}

func withServiceAccountObjectConvergence(
	verifyStored func(context.Context) error,
	markerProbe admissionMarkerProbe,
	guard *crdupgrade.ServiceAccountObjectGuard,
	requireDirectProof bool,
) (func(context.Context) error, admissionMarkerProbe, error) {
	if verifyStored == nil || markerProbe == nil {
		return nil, nil, errors.New("admission convergence proof callbacks are required")
	}
	if guard == nil {
		return nil, nil, errors.New("service account object guard is required")
	}
	return func(ctx context.Context) error {
			if err := verifyStored(ctx); err != nil {
				return err
			}
			if err := guard.Verify(ctx); err != nil {
				return fmt.Errorf("verify stored service account object guard: %w", err)
			}
			return nil
		}, func(ctx context.Context, client crdupgrade.AdmissionConvergenceMarkerClient) (bool, error) {
			ready, err := markerProbe(ctx, client)
			if err != nil || !ready || !requireDirectProof {
				return ready, err
			}
			return guard.Probe(ctx, client)
		}, nil
}

func newPredecessorRetirementAdmissionBarrier(
	config *rest.Config,
	endpointSlices endpointSliceLister,
	guard *crdupgrade.AdmissionConvergenceGuard,
	retirement *crdupgrade.PredecessorRetirement,
) crdupgrade.PredecessorRetirementBarrier {
	return func(ctx context.Context, target crdupgrade.PredecessorRetirementBarrierTarget) error {
		if retirement == nil {
			return errors.New("predecessor retirement manager is required")
		}
		if guard == nil {
			return errors.New("admission convergence guard is required")
		}
		if target.MarkerName() == "" || target.Marker() == nil || len(target.Probes()) == 0 {
			return errors.New("predecessor retirement barrier target is incomplete")
		}
		barrier, err := newMarkerAdmissionConvergenceBarrier(
			ctx,
			config,
			endpointSlices,
			guard,
			retirement.Preflight,
			newDirectAdmissionMarkerClientForNamespace(guard.ReleaseNamespace),
			func(probeCtx context.Context, client crdupgrade.AdmissionConvergenceMarkerClient) (bool, error) {
				return probePredecessorRetirementTarget(probeCtx, client, target)
			},
		)
		if err != nil {
			return err
		}
		return barrier.Wait(ctx)
	}
}

func probePredecessorRetirementTarget(
	ctx context.Context,
	client crdupgrade.AdmissionConvergenceMarkerClient,
	target crdupgrade.PredecessorRetirementBarrierTarget,
) (bool, error) {
	if ctx == nil {
		return false, errors.New("predecessor retirement probe context is nil")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if client == nil {
		return false, errors.New("predecessor retirement marker client is nil")
	}
	marker, err := client.Get(ctx, target.MarkerName(), metav1.GetOptions{})
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	if err != nil {
		if retryableDirectAdmissionError(err) {
			return false, nil
		}
		return false, fmt.Errorf("get direct predecessor retirement marker: %w", err)
	}
	if err := target.VerifyMarker(marker); err != nil {
		return false, err
	}
	for _, probe := range target.Probes() {
		_, err = client.Update(ctx, marker.DeepCopy(), metav1.UpdateOptions{
			DryRun:       []string{metav1.DryRunAll},
			FieldManager: probe.FieldManager,
		})
		if contextErr := ctx.Err(); contextErr != nil {
			return false, contextErr
		}
		if err == nil {
			continue
		}
		if probe.HasExactDenial(err) || retryableDirectAdmissionError(err) {
			return false, nil
		}
		return false, fmt.Errorf("probe predecessor policy %s retirement: %w", probe.PolicyName, err)
	}
	return true, nil
}

func newTeardownAdmissionConvergenceBarrier(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	guard *crdupgrade.AdmissionConvergenceGuard,
	expectedState crdupgrade.ReleaseActivationState,
) (*admissionConvergenceBarrier, error) {
	if guard == nil {
		return nil, errors.New("admission convergence guard is required")
	}
	if err := waitForStoredAdmissionConvergence(
		ctx,
		admissionConvergencePollEvery,
		admissionConvergenceRequestTimeout,
		sleepForAdmissionConvergence,
		func(verifyCtx context.Context) error { return guard.VerifyState(verifyCtx, expectedState) },
	); err != nil {
		return nil, fmt.Errorf("verify teardown admission convergence sentinel: %w", err)
	}
	return newMarkerAdmissionConvergenceBarrier(ctx, config, endpointSlices, guard, nil, newDirectAdmissionMarkerClientForNamespace(guard.ReleaseNamespace), func(probeCtx context.Context, client crdupgrade.AdmissionConvergenceMarkerClient) (bool, error) {
		return guard.ProbeAbsent(probeCtx, client, expectedState)
	})
}

func newMarkerAdmissionConvergenceBarrier(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	guard *crdupgrade.AdmissionConvergenceGuard,
	verifyStored func(context.Context) error,
	clientFactory admissionMarkerClientFactory,
	markerProbe admissionMarkerProbe,
) (*admissionConvergenceBarrier, error) {
	if ctx == nil {
		return nil, errors.New("admission convergence discovery context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if guard == nil {
		return nil, errors.New("admission convergence guard is required")
	}
	if clientFactory == nil {
		return nil, errors.New("admission convergence marker client factory is required")
	}
	if markerProbe == nil {
		return nil, errors.New("admission convergence marker probe is required")
	}
	apiEndpoints, err := newKubernetesAPIServerEndpointProvider(config, endpointSlices, 2)
	if err != nil {
		return nil, err
	}
	provider := newMarkerAdmissionEndpointProvider(apiEndpoints, guard, clientFactory, markerProbe)
	barrier := &admissionConvergenceBarrier{
		endpointProvider:  provider,
		verifyStored:      verifyStored,
		pollEvery:         admissionConvergencePollEvery,
		stabilityDuration: admissionConvergenceStabilityWindow,
		requestTimeout:    admissionConvergenceRequestTimeout,
	}
	if err := barrier.validate(); err != nil {
		return nil, fmt.Errorf("validate admission convergence barrier: %w", err)
	}
	return barrier, nil
}

func newMarkerAdmissionEndpointProvider(
	apiEndpoints kubernetesAPIServerEndpointProvider,
	guard *crdupgrade.AdmissionConvergenceGuard,
	clientFactory admissionMarkerClientFactory,
	markerProbe admissionMarkerProbe,
) admissionConvergenceEndpointProvider {
	clientsByAddress := make(map[string]crdupgrade.AdmissionConvergenceMarkerClient)
	cachedIdentity := ""
	return func(ctx context.Context) ([]namedAdmissionConvergenceProbe, error) {
		if apiEndpoints == nil || guard == nil || clientFactory == nil || markerProbe == nil {
			return nil, errors.New("admission convergence endpoint adapter is incomplete")
		}
		snapshot, err := apiEndpoints(ctx)
		if err != nil {
			return nil, err
		}
		if snapshot.InventoryIdentity == "" || snapshot.InventoryIdentity != strings.TrimSpace(snapshot.InventoryIdentity) {
			return nil, errors.New("admission convergence endpoint snapshot has an empty or padded identity")
		}
		if len(snapshot.Endpoints) == 0 {
			return nil, errors.New("admission convergence endpoint snapshot is empty")
		}
		if snapshot.InventoryIdentity != cachedIdentity {
			clientsByAddress = make(map[string]crdupgrade.AdmissionConvergenceMarkerClient)
			cachedIdentity = snapshot.InventoryIdentity
		}
		result := make([]namedAdmissionConvergenceProbe, 0, len(snapshot.Endpoints))
		for _, endpoint := range snapshot.Endpoints {
			if endpoint.Address == "" || endpoint.Address != strings.TrimSpace(endpoint.Address) || endpoint.RESTConfig == nil {
				return nil, errors.New("admission convergence endpoint snapshot contains an incomplete endpoint")
			}
			client := clientsByAddress[endpoint.Address]
			if client == nil {
				client, err = clientFactory(endpoint.RESTConfig)
				if err != nil {
					return nil, fmt.Errorf("create admission convergence client for API endpoint %q: %w", endpoint.Address, err)
				}
				if client == nil {
					return nil, fmt.Errorf("admission convergence client factory returned nil for API endpoint %q", endpoint.Address)
				}
				clientsByAddress[endpoint.Address] = client
			}
			probeClient := client
			result = append(result, namedAdmissionConvergenceProbe{
				name:             endpoint.Address,
				topologyIdentity: snapshot.InventoryIdentity,
				probe: func(probeCtx context.Context) (bool, error) {
					return markerProbe(probeCtx, probeClient)
				},
			})
		}
		return result, nil
	}
}

func newDirectAdmissionMarkerClientForNamespace(namespace string) admissionMarkerClientFactory {
	return func(config *rest.Config) (crdupgrade.AdmissionConvergenceMarkerClient, error) {
		client, err := coreclientv1.NewForConfig(config)
		if err != nil {
			return nil, err
		}
		return client.ConfigMaps(namespace), nil
	}
}

func (b *admissionConvergenceBarrier) Wait(ctx context.Context) error {
	if b == nil {
		return errors.New("admission convergence barrier is nil")
	}
	return b.wait(ctx, b.stabilityDuration, nil)
}

func (b *admissionConvergenceBarrier) WaitWithStabilityObserver(
	ctx context.Context,
	stabilityDuration time.Duration,
	observer admissionConvergenceStabilityObserver,
) error {
	if observer == nil {
		return errors.New("admission convergence stability observer is nil")
	}
	defer observer.Close()
	return b.wait(ctx, stabilityDuration, observer)
}

func (b *admissionConvergenceBarrier) wait(
	ctx context.Context,
	stabilityDuration time.Duration,
	observer admissionConvergenceStabilityObserver,
) error {
	if err := b.validate(); err != nil {
		return err
	}
	if stabilityDuration <= 0 {
		return errors.New("admission convergence stability duration must be positive")
	}
	if ctx == nil {
		return errors.New("admission convergence context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := b.now
	if now == nil {
		now = time.Now
	}
	sleep := b.sleep
	if sleep == nil {
		sleep = sleepForAdmissionConvergence
	}

	var stableSince time.Time
	stableSetKey := ""
	stableObserverIdentity := ""
	resetStability := func() {
		stableSince = time.Time{}
		stableSetKey = ""
		stableObserverIdentity = ""
	}
	for {
		sweepStartedAt := now()
		eligible := !stableSince.IsZero() && !sweepStartedAt.Before(stableSince) && sweepStartedAt.Sub(stableSince) >= stabilityDuration

		setKey := ""
		if b.endpointProvider != nil {
			endpoints, err := b.endpointProvider(ctx)
			if err != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return contextErr
				}
				// EndpointSlice inventory and direct-client construction are dynamic
				// observations. Any failure is fail-closed and recoverable until the
				// outer context expires; static REST/lister inputs were already
				// rejected by the constructor, while stored-contract drift remains
				// immediately fatal below.
				resetStability()
				if err := sleepForNextAdmissionConvergenceSweep(ctx, sleep, b.pollEvery); err != nil {
					return fmt.Errorf("admission endpoint discovery did not recover: %w", err)
				}
				continue
			}
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			if err := validateAdmissionConvergenceEndpoints(endpoints); err != nil {
				return err
			}
			b.endpoints = endpoints
		}
		setKey = admissionConvergenceEndpointSetKey(b.endpoints)

		allProven := true
		storedProven, err := b.verifyStoredContract(ctx)
		if err != nil {
			return err
		}
		if !storedProven {
			allProven = false
		}
		for _, endpoint := range b.endpoints {
			probeCtx, cancel := context.WithTimeout(ctx, b.requestTimeout)
			proven, err := endpoint.probe(probeCtx)
			probeContextErr := probeCtx.Err()
			cancel()
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			if err == nil && probeContextErr != nil {
				err = probeContextErr
				proven = false
			}
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					allProven = false
					continue
				}
				return fmt.Errorf("probe admission convergence on API endpoint %q: %w", endpoint.name, err)
			}
			if !proven {
				allProven = false
			}
		}
		observerIdentity := ""
		if observer != nil {
			identity, proven, err := observer.Observe(ctx, setKey)
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			if err != nil {
				resetStability()
				if err := sleepForNextAdmissionConvergenceSweep(ctx, sleep, b.pollEvery); err != nil {
					return fmt.Errorf("admission convergence stability observer did not recover: %w", err)
				}
				continue
			}
			if identity == "" || identity != strings.TrimSpace(identity) {
				return errors.New("admission convergence stability observer returned an empty or padded identity")
			}
			observerIdentity = identity
			if !proven {
				allProven = false
			}
		}
		if !allProven {
			resetStability()
			if err := sleepForNextAdmissionConvergenceSweep(ctx, sleep, b.pollEvery); err != nil {
				return err
			}
			continue
		}

		// Close the sweep by re-reading every non-endpoint invariant around a
		// second discovery snapshot. A successful opening observation cannot be
		// carried across a stored-object replacement, topology change, or watch
		// event that happens while the endpoint probes are running.
		storedProven, err = b.verifyStoredContract(ctx)
		if err != nil {
			return err
		}
		if !storedProven {
			resetStability()
			if err := sleepForNextAdmissionConvergenceSweep(ctx, sleep, b.pollEvery); err != nil {
				return err
			}
			continue
		}
		if b.endpointProvider != nil {
			closingEndpoints, discoverErr := b.endpointProvider(ctx)
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			if discoverErr != nil {
				resetStability()
				if err := sleepForNextAdmissionConvergenceSweep(ctx, sleep, b.pollEvery); err != nil {
					return fmt.Errorf("closing admission endpoint discovery did not recover: %w", err)
				}
				continue
			}
			if err := validateAdmissionConvergenceEndpoints(closingEndpoints); err != nil {
				return err
			}
			b.endpoints = closingEndpoints
			if admissionConvergenceEndpointSetKey(closingEndpoints) != setKey {
				resetStability()
				if err := sleepForNextAdmissionConvergenceSweep(ctx, sleep, b.pollEvery); err != nil {
					return err
				}
				continue
			}
		}
		if observer != nil {
			closingIdentity, proven, observeErr := observer.Observe(ctx, setKey)
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			if observeErr != nil {
				resetStability()
				if err := sleepForNextAdmissionConvergenceSweep(ctx, sleep, b.pollEvery); err != nil {
					return fmt.Errorf("closing admission convergence stability observer did not recover: %w", err)
				}
				continue
			}
			if closingIdentity == "" || closingIdentity != strings.TrimSpace(closingIdentity) {
				return errors.New("admission convergence stability observer returned an empty or padded closing identity")
			}
			if !proven || closingIdentity != observerIdentity {
				resetStability()
				if err := sleepForNextAdmissionConvergenceSweep(ctx, sleep, b.pollEvery); err != nil {
					return err
				}
				continue
			}
		}

		closedAt := now()
		if err := ctx.Err(); err != nil {
			return err
		}
		if stableSince.IsZero() || stableSetKey != setKey || stableObserverIdentity != observerIdentity || closedAt.Before(stableSince) {
			// The first fully closed sweep establishes the start of the stable
			// window at its closing instant. Time spent probing this sweep can
			// never count toward convergence.
			stableSince = closedAt
			stableSetKey = setKey
			stableObserverIdentity = observerIdentity
		} else if eligible {
			return nil
		}
		if err := sleepForNextAdmissionConvergenceSweep(ctx, sleep, b.pollEvery); err != nil {
			return err
		}
	}
}

func sleepForNextAdmissionConvergenceSweep(
	ctx context.Context,
	sleep func(context.Context, time.Duration) error,
	pollEvery time.Duration,
) error {
	err := sleep(ctx, pollEvery)
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return err
}

func (b *admissionConvergenceBarrier) verifyStoredContract(ctx context.Context) (bool, error) {
	if b.verifyStored == nil {
		return true, nil
	}
	verifyCtx, cancel := context.WithTimeout(ctx, b.requestTimeout)
	err := b.verifyStored(verifyCtx)
	verifyContextErr := verifyCtx.Err()
	cancel()
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	if err == nil && verifyContextErr != nil {
		err = verifyContextErr
	}
	if err == nil {
		return true, nil
	}
	if retryableStoredAdmissionConvergenceError(err) {
		return false, nil
	}
	return false, fmt.Errorf("verify stored admission convergence contract: %w", err)
}

func waitForStoredAdmissionConvergence(
	ctx context.Context,
	pollEvery time.Duration,
	requestTimeout time.Duration,
	sleep func(context.Context, time.Duration) error,
	verify func(context.Context) error,
) error {
	if ctx == nil {
		return errors.New("admission convergence verification context is nil")
	}
	if pollEvery <= 0 || requestTimeout <= 0 || sleep == nil || verify == nil {
		return errors.New("admission convergence verification retry configuration is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for {
		verifyCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		err := verify(verifyCtx)
		verifyContextErr := verifyCtx.Err()
		cancel()
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if err == nil && verifyContextErr != nil {
			err = verifyContextErr
		}
		if err == nil {
			return nil
		}
		if !retryableStoredAdmissionConvergenceError(err) {
			return err
		}
		if sleepErr := sleep(ctx, pollEvery); sleepErr != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			return fmt.Errorf("admission convergence verification did not recover: %w", sleepErr)
		}
	}
}

func retryableStoredAdmissionConvergenceError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) {
		return true
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Code >= 500 {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func (b *admissionConvergenceBarrier) validate() error {
	if b == nil {
		return errors.New("admission convergence barrier is nil")
	}
	if b.pollEvery <= 0 || b.stabilityDuration <= 0 || b.requestTimeout <= 0 {
		return errors.New("admission convergence timing values must be positive")
	}
	if len(b.endpoints) == 0 && b.endpointProvider != nil {
		return nil
	}
	return validateAdmissionConvergenceEndpoints(b.endpoints)
}

func validateAdmissionConvergenceEndpoints(endpoints []namedAdmissionConvergenceProbe) error {
	if len(endpoints) == 0 {
		return errors.New("admission convergence endpoints are empty")
	}
	seen := make(map[string]struct{}, len(endpoints))
	topology := endpoints[0].topologyIdentity
	if topology == "" || topology != strings.TrimSpace(topology) {
		return errors.New("admission convergence topology identity is empty or padded")
	}
	for _, endpoint := range endpoints {
		if endpoint.name == "" || endpoint.name != strings.TrimSpace(endpoint.name) || endpoint.probe == nil {
			return errors.New("admission convergence endpoint is incomplete")
		}
		if endpoint.topologyIdentity != topology {
			return errors.New("admission convergence endpoints do not share one topology identity")
		}
		if _, duplicate := seen[endpoint.name]; duplicate {
			return fmt.Errorf("admission convergence endpoint %q is duplicated", endpoint.name)
		}
		seen[endpoint.name] = struct{}{}
	}
	return nil
}

func admissionConvergenceEndpointSetKey(endpoints []namedAdmissionConvergenceProbe) string {
	values := make([]string, 0, len(endpoints)+1)
	values = append(values, endpoints[0].topologyIdentity)
	for _, endpoint := range endpoints {
		values = append(values, endpoint.name)
	}
	sort.Strings(values[1:])
	return strings.Join(values, "\x00")
}

func sleepForAdmissionConvergence(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type directSecretCreateProbeClient struct {
	secrets secretCreateClient
}

type secretCreateClient interface {
	Create(context.Context, *corev1.Secret, metav1.CreateOptions) (*corev1.Secret, error)
}

func (c *directSecretCreateProbeClient) probe(
	ctx context.Context,
	policyName, bindingName, probeName string,
) (bool, error) {
	_, err := c.secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{GenerateName: probeName},
		Type:       corev1.SecretTypeOpaque,
	}, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err == nil {
		return false, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	if crdupgrade.HasExactValidatingAdmissionPolicyDenial(err, policyName, bindingName, certificateRecoveryGuardDenialMessage) {
		return true, nil
	}
	if retryableDirectAdmissionError(err) {
		return false, nil
	}
	return false, fmt.Errorf("optional certificate recovery guard returned an unexpected response: %w", err)
}

func newCertificateRecoveryConvergenceBarrier(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	namespace, policyName, bindingName, protectedSecretName string,
) (*admissionConvergenceBarrier, error) {
	if ctx == nil {
		return nil, errors.New("certificate recovery convergence discovery context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for description, value := range map[string]string{
		"namespace": namespace, "policy name": policyName, "binding name": bindingName, "protected Secret name": protectedSecretName,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return nil, fmt.Errorf("certificate recovery convergence %s is empty or padded", description)
		}
	}
	apiEndpoints, err := newKubernetesAPIServerEndpointProvider(config, endpointSlices, 1)
	if err != nil {
		return nil, err
	}
	probeName := certificateRecoveryProbeName(protectedSecretName)
	provider := newSecretCreateAdmissionEndpointProvider(apiEndpoints, namespace, policyName, bindingName, probeName)
	barrier := &admissionConvergenceBarrier{
		endpointProvider: provider,
		pollEvery:        admissionConvergencePollEvery, stabilityDuration: admissionConvergenceStabilityWindow,
		requestTimeout: admissionConvergenceRequestTimeout,
	}
	if err := barrier.validate(); err != nil {
		return nil, err
	}
	return barrier, nil
}

func newSecretCreateAdmissionEndpointProvider(
	apiEndpoints kubernetesAPIServerEndpointProvider,
	namespace, policyName, bindingName, probeName string,
) admissionConvergenceEndpointProvider {
	clients := make(map[string]*directSecretCreateProbeClient)
	identity := ""
	return func(ctx context.Context) ([]namedAdmissionConvergenceProbe, error) {
		snapshot, err := apiEndpoints(ctx)
		if err != nil {
			return nil, err
		}
		if snapshot.InventoryIdentity != identity {
			clients = make(map[string]*directSecretCreateProbeClient)
			identity = snapshot.InventoryIdentity
		}
		result := make([]namedAdmissionConvergenceProbe, 0, len(snapshot.Endpoints))
		for _, endpoint := range snapshot.Endpoints {
			client := clients[endpoint.Address]
			if client == nil {
				coreClient, createErr := coreclientv1.NewForConfig(endpoint.RESTConfig)
				if createErr != nil {
					return nil, fmt.Errorf("create certificate recovery client for API endpoint %q: %w", endpoint.Address, createErr)
				}
				client = &directSecretCreateProbeClient{secrets: coreClient.Secrets(namespace)}
				clients[endpoint.Address] = client
			}
			probeClient := client
			result = append(result, namedAdmissionConvergenceProbe{
				name: endpoint.Address, topologyIdentity: snapshot.InventoryIdentity,
				probe: func(probeCtx context.Context) (bool, error) {
					return probeClient.probe(probeCtx, policyName, bindingName, probeName)
				},
			})
		}
		return result, nil
	}
}

func certificateRecoveryProbeName(secretName string) string {
	suffix := "-admission-probe-"
	base := strings.TrimSuffix(secretName[:min(len(secretName), 63-len(suffix))], "-")
	if base == "" {
		base = "ptah"
	}
	return base + suffix
}

func retryableDirectAdmissionError(err error) bool {
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) {
		return true
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Code >= 500 {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}
