package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/certrotation"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

const teardownRetirementCredentialTimeout = 75 * time.Second

func newConfiguredTeardownRetirementGuard(
	rollout *crdupgrade.RolloutGuard,
	contract crdupgrade.RuntimeAdmissionContract,
) (*crdupgrade.TeardownRetirementGuard, error) {
	if rollout == nil {
		return nil, errors.New("teardown retirement rollout identity is required")
	}
	guard := crdupgrade.NewTeardownRetirementGuard(rollout)
	recreateMissingSecret, err := optionalExactBooleanRuntimeArgument(rollout.CertificateArgs, "--recreate-missing-secret=")
	if err != nil {
		return nil, fmt.Errorf("read certificate recovery retirement contract: %w", err)
	}
	if !contract.CertificateRuntimeEnabled {
		if recreateMissingSecret {
			return nil, errors.New("certificate recovery is enabled while the certificate runtime is disabled")
		}
		return guard, nil
	}
	if contract.CertificateServiceAccountName != rollout.CertificateDeploymentName {
		return nil, fmt.Errorf(
			"runtime admission certificate ServiceAccount %q differs from rollout identity %q",
			contract.CertificateServiceAccountName,
			rollout.CertificateDeploymentName,
		)
	}
	if !recreateMissingSecret {
		return guard, nil
	}

	policyName, err := exactRuntimeArgument(rollout.CertificateArgs, "--secret-create-policy-name=")
	if err != nil {
		return nil, err
	}
	bindingName, err := exactRuntimeArgument(rollout.CertificateArgs, "--secret-create-policy-binding-name=")
	if err != nil {
		return nil, err
	}
	serviceAccountName, err := exactRuntimeArgument(rollout.CertificateArgs, "--secret-create-service-account-name=")
	if err != nil {
		return nil, err
	}
	if policyName != bindingName {
		return nil, errors.New("certificate recovery policy and binding names must be identical for retirement")
	}
	if serviceAccountName != rollout.CertificateDeploymentName || policyName != rollout.CertificateDeploymentName {
		return nil, fmt.Errorf(
			"certificate recovery policy, binding, and ServiceAccount must equal the exact certificate runtime identity %q",
			rollout.CertificateDeploymentName,
		)
	}
	config := certrotation.Config{
		Namespace:                      rollout.ReleaseNamespace,
		SecretName:                     rollout.WebhookSecretName,
		SecretCreatePolicyName:         policyName,
		SecretCreatePolicyBindingName:  bindingName,
		SecretCreateServiceAccountName: serviceAccountName,
		RecreateMissingSecret:          true,
	}
	pair := crdupgrade.TeardownOriginalPairVerifier{
		Name: policyName,
		VerifyPolicy: func(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
			if policy == nil {
				return errors.New("certificate recovery retirement policy is nil")
			}
			if err := verifyCertificateRecoveryRetirementMetadata("ValidatingAdmissionPolicy", policyName, rollout, policy); err != nil {
				return err
			}
			return certrotation.VerifySecretCreatePolicyContract(policy, config)
		},
		VerifyBinding: func(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
			if binding == nil {
				return errors.New("certificate recovery retirement binding is nil")
			}
			if err := verifyCertificateRecoveryRetirementMetadata("ValidatingAdmissionPolicyBinding", bindingName, rollout, binding); err != nil {
				return err
			}
			return certrotation.VerifySecretCreateBindingContract(binding, config)
		},
	}
	return guard.WithOriginalPairs(pair)
}

type teardownRetirementMetadataObject interface {
	metav1.Object
}

func verifyCertificateRecoveryRetirementMetadata(
	kind, name string,
	rollout *crdupgrade.RolloutGuard,
	object teardownRetirementMetadataObject,
) error {
	if rollout == nil || object == nil {
		return fmt.Errorf("certificate recovery retirement %s/%s is nil", kind, name)
	}
	annotations := object.GetAnnotations()
	labels := object.GetLabels()
	if object.GetName() != name || object.GetNamespace() != "" || object.GetGenerateName() != "" ||
		object.GetUID() == "" || object.GetResourceVersion() == "" || object.GetDeletionTimestamp() != nil ||
		object.GetDeletionGracePeriodSeconds() != nil || len(object.GetOwnerReferences()) != 0 || len(object.GetFinalizers()) != 0 ||
		len(annotations) != 2 || annotations["meta.helm.sh/release-name"] != rollout.ReleaseName ||
		annotations["meta.helm.sh/release-namespace"] != rollout.ReleaseNamespace || len(labels) != 6 ||
		labels["app.kubernetes.io/managed-by"] != "Helm" || labels["app.kubernetes.io/instance"] != rollout.ReleaseName ||
		labels["app.kubernetes.io/component"] != "certificate-rotation" ||
		!exactNonemptyMetadataValue(labels["helm.sh/chart"]) || !exactNonemptyMetadataValue(labels["app.kubernetes.io/name"]) ||
		!exactNonemptyMetadataValue(labels["app.kubernetes.io/version"]) {
		return fmt.Errorf("certificate recovery retirement %s/%s has foreign or incomplete Helm ownership", kind, name)
	}
	return nil
}

func exactNonemptyMetadataValue(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

// newTeardownRetirementFenceBarrier proves one or both early fences on every
// directly addressed API server. Callers bind their exact phase-aware target
// inventory separately so every stored sweep remains mode-specific.
func newTeardownRetirementFenceBarrier(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	guard *crdupgrade.TeardownRetirementGuard,
	policies crdupgrade.ValidatingAdmissionPolicyReader,
	bindings crdupgrade.ValidatingAdmissionPolicyBindingReader,
	fences ...crdupgrade.TeardownFence,
) (*admissionConvergenceBarrier, error) {
	if guard == nil {
		return nil, errors.New("teardown retirement guard is required")
	}
	probes := make([]crdupgrade.TeardownRetirementProbe, 0, len(fences))
	for _, fence := range fences {
		_, _, probe, err := guard.OriginalFencePair(fence)
		if err != nil {
			return nil, err
		}
		probes = append(probes, probe)
	}
	verifyStored := func(verifyCtx context.Context) error {
		if err := guard.VerifyOriginalFences(verifyCtx, policies, bindings, fences...); err != nil {
			return err
		}
		return nil
	}
	return newTeardownRetirementAdmissionBarrier(
		ctx,
		config,
		endpointSlices,
		guard,
		probes,
		verifyStored,
	)
}

// newTeardownRetirementFinalBarrier joins exact stored retirement objects to
// the per-endpoint denial proofs. The caller must join this barrier to the
// authorization and protected-Pod observer for the uninterrupted 65-second
// final window; separate waits are not equivalent evidence.
func newTeardownRetirementFinalBarrier(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	guard *crdupgrade.TeardownRetirementGuard,
	policies crdupgrade.ValidatingAdmissionPolicyReader,
	bindings crdupgrade.ValidatingAdmissionPolicyBindingReader,
) (*admissionConvergenceBarrier, error) {
	if guard == nil {
		return nil, errors.New("teardown retirement guard is required")
	}
	pairs, err := guard.RetirementPairs()
	if err != nil {
		return nil, err
	}
	probes := make([]crdupgrade.TeardownRetirementProbe, 0, len(pairs)+2)
	for _, fence := range []crdupgrade.TeardownFence{crdupgrade.TeardownFenceA, crdupgrade.TeardownFenceB} {
		_, _, probe, pairErr := guard.OriginalFencePair(fence)
		if pairErr != nil {
			return nil, pairErr
		}
		probes = append(probes, probe)
	}
	for _, pair := range pairs {
		probes = append(probes, pair.Probe)
	}
	verifyStored := func(verifyCtx context.Context) error {
		if err := guard.VerifyOriginalFences(
			verifyCtx,
			policies,
			bindings,
			crdupgrade.TeardownFenceA,
			crdupgrade.TeardownFenceB,
		); err != nil {
			return err
		}
		return guard.VerifyRetiredPairs(verifyCtx, policies, bindings)
	}
	return newTeardownRetirementAdmissionBarrier(
		ctx,
		config,
		endpointSlices,
		guard,
		probes,
		verifyStored,
	)
}

func newTeardownRetirementAdmissionBarrier(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	guard *crdupgrade.TeardownRetirementGuard,
	probes []crdupgrade.TeardownRetirementProbe,
	verifyStored func(context.Context) error,
) (*admissionConvergenceBarrier, error) {
	marker, err := guard.Marker()
	if err != nil {
		return nil, err
	}
	return newTeardownRetirementAdmissionBarrierWith(
		ctx,
		config,
		endpointSlices,
		guard,
		probes,
		verifyStored,
		newDirectAdmissionMarkerClientForNamespace(marker.Namespace),
	)
}

func newTeardownRetirementAdmissionBarrierWith(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	guard *crdupgrade.TeardownRetirementGuard,
	probes []crdupgrade.TeardownRetirementProbe,
	verifyStored func(context.Context) error,
	clientFactory admissionMarkerClientFactory,
) (*admissionConvergenceBarrier, error) {
	if ctx == nil {
		return nil, errors.New("teardown retirement discovery context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if guard == nil || clientFactory == nil || verifyStored == nil {
		return nil, errors.New("teardown retirement admission dependencies are required")
	}
	if len(probes) == 0 {
		return nil, errors.New("teardown retirement admission probes are empty")
	}
	seen := make(map[string]struct{}, len(probes))
	for _, probe := range probes {
		if probe.PolicyName == "" || probe.BindingName != probe.PolicyName ||
			probe.FieldManager == "" || probe.Message == "" {
			return nil, errors.New("teardown retirement admission probe is incomplete")
		}
		if _, duplicate := seen[probe.PolicyName]; duplicate {
			return nil, fmt.Errorf("teardown retirement admission policy %q is duplicated", probe.PolicyName)
		}
		seen[probe.PolicyName] = struct{}{}
	}
	apiEndpoints, err := newKubernetesAPIServerEndpointProvider(config, endpointSlices, 2*len(probes))
	if err != nil {
		return nil, err
	}
	provider := newTeardownRetirementEndpointProvider(apiEndpoints, guard, probes, clientFactory)
	barrier := &admissionConvergenceBarrier{
		endpointProvider:  provider,
		verifyStored:      verifyStored,
		pollEvery:         admissionConvergencePollEvery,
		stabilityDuration: admissionConvergenceStabilityWindow,
		requestTimeout:    admissionConvergenceRequestTimeout,
	}
	if err := barrier.validate(); err != nil {
		return nil, fmt.Errorf("validate teardown retirement admission barrier: %w", err)
	}
	return barrier, nil
}

func newTeardownRetirementEndpointProvider(
	apiEndpoints kubernetesAPIServerEndpointProvider,
	guard *crdupgrade.TeardownRetirementGuard,
	probes []crdupgrade.TeardownRetirementProbe,
	clientFactory admissionMarkerClientFactory,
) admissionConvergenceEndpointProvider {
	clientsByAddress := make(map[string]crdupgrade.AdmissionConvergenceMarkerClient)
	cachedIdentity := ""
	return func(ctx context.Context) ([]namedAdmissionConvergenceProbe, error) {
		if apiEndpoints == nil || guard == nil || len(probes) == 0 || clientFactory == nil {
			return nil, errors.New("teardown retirement endpoint adapter is incomplete")
		}
		snapshot, err := apiEndpoints(ctx)
		if err != nil {
			return nil, err
		}
		if snapshot.InventoryIdentity == "" || snapshot.InventoryIdentity != strings.TrimSpace(snapshot.InventoryIdentity) {
			return nil, errors.New("teardown retirement endpoint snapshot has an empty or padded identity")
		}
		if len(snapshot.Endpoints) == 0 {
			return nil, errors.New("teardown retirement endpoint snapshot is empty")
		}
		if snapshot.InventoryIdentity != cachedIdentity {
			clientsByAddress = make(map[string]crdupgrade.AdmissionConvergenceMarkerClient)
			cachedIdentity = snapshot.InventoryIdentity
		}
		result := make([]namedAdmissionConvergenceProbe, 0, len(snapshot.Endpoints))
		for _, endpoint := range snapshot.Endpoints {
			if endpoint.Address == "" || endpoint.Address != strings.TrimSpace(endpoint.Address) || endpoint.RESTConfig == nil {
				return nil, errors.New("teardown retirement endpoint snapshot contains an incomplete endpoint")
			}
			client := clientsByAddress[endpoint.Address]
			if client == nil {
				client, err = clientFactory(endpoint.RESTConfig)
				if err != nil {
					return nil, fmt.Errorf("create teardown retirement client for API endpoint %q: %w", endpoint.Address, err)
				}
				if client == nil {
					return nil, fmt.Errorf("teardown retirement client factory returned nil for API endpoint %q", endpoint.Address)
				}
				clientsByAddress[endpoint.Address] = client
			}
			probeClient := client
			result = append(result, namedAdmissionConvergenceProbe{
				name:             endpoint.Address,
				topologyIdentity: snapshot.InventoryIdentity,
				probe: func(probeCtx context.Context) (bool, error) {
					for _, probe := range probes {
						proven, probeErr := guard.Probe(probeCtx, probeClient, probe)
						if probeErr != nil || !proven {
							return proven, probeErr
						}
					}
					return true, nil
				},
			})
		}
		return result, nil
	}
}

type endpointSliceInventoryClient interface {
	endpointSliceLister
	Watch(context.Context, metav1.ListOptions) (watch.Interface, error)
}

type endpointSliceWatchStarter func(context.Context, metav1.ListOptions) (watch.Interface, error)

type teardownRetirementCredentialEndpoint struct {
	name   string
	client crdupgrade.AdmissionConvergenceMarkerClient
}

// teardownRetirementCredentialObserver freezes one coherent API-server
// endpoint inventory before the final mutations. Its watch makes that
// snapshot fail closed if the selected EndpointSlice inventory changes while
// the cleanup credential is being retired.
type teardownRetirementCredentialObserver struct {
	guard     *crdupgrade.TeardownRetirementGuard
	probes    []crdupgrade.TeardownRetirementProbe
	endpoints []teardownRetirementCredentialEndpoint

	topologyWatch  watch.Interface
	topologyEvents <-chan watch.Event
	lifecycleCtx   context.Context
	cancel         context.CancelFunc
	monitorDone    chan struct{}
	checkpoints    chan chan struct{}
	closeOnce      sync.Once

	failureMu sync.Mutex
	failure   error

	pollEvery         time.Duration
	stabilityDuration time.Duration
	retirementTimeout time.Duration
	requestTimeout    time.Duration
	now               func() time.Time
	sleep             func(context.Context, time.Duration) error
}

func newTeardownRetirementCredentialObserver(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceInventoryClient,
	guard *crdupgrade.TeardownRetirementGuard,
) (*teardownRetirementCredentialObserver, error) {
	if ctx == nil {
		return nil, errors.New("teardown retirement credential discovery context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if endpointSlices == nil || guard == nil {
		return nil, errors.New("teardown retirement credential observer dependencies are required")
	}
	marker, err := guard.Marker()
	if err != nil {
		return nil, err
	}
	probes, err := teardownRetirementFenceProbes(guard)
	if err != nil {
		return nil, err
	}
	provider, err := newKubernetesAPIServerEndpointProvider(config, endpointSlices, 1+2*len(probes))
	if err != nil {
		return nil, err
	}
	snapshot, err := waitForInitialKubernetesAPIServerEndpointSnapshot(
		ctx,
		provider,
		authorizationPollEvery,
		sleepForKubernetesAPIServerEndpointDiscovery,
	)
	if err != nil {
		return nil, err
	}
	return newTeardownRetirementCredentialObserverForSnapshot(
		ctx,
		snapshot,
		guard,
		probes,
		newDirectAdmissionMarkerClientForNamespace(marker.Namespace),
		endpointSlices.Watch,
	)
}

func teardownRetirementFenceProbes(guard *crdupgrade.TeardownRetirementGuard) ([]crdupgrade.TeardownRetirementProbe, error) {
	if guard == nil {
		return nil, errors.New("teardown retirement guard is required")
	}
	probes := make([]crdupgrade.TeardownRetirementProbe, 0, 2)
	for _, fence := range []crdupgrade.TeardownFence{crdupgrade.TeardownFenceA, crdupgrade.TeardownFenceB} {
		_, _, probe, err := guard.OriginalFencePair(fence)
		if err != nil {
			return nil, err
		}
		probes = append(probes, probe)
	}
	return probes, nil
}

func newTeardownRetirementCredentialObserverForSnapshot(
	ctx context.Context,
	snapshot kubernetesAPIServerEndpointSnapshot,
	guard *crdupgrade.TeardownRetirementGuard,
	probes []crdupgrade.TeardownRetirementProbe,
	clientFactory admissionMarkerClientFactory,
	startWatch endpointSliceWatchStarter,
) (*teardownRetirementCredentialObserver, error) {
	if ctx == nil {
		return nil, errors.New("teardown retirement credential observer context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if guard == nil || clientFactory == nil || startWatch == nil {
		return nil, errors.New("teardown retirement credential observer dependencies are required")
	}
	if len(probes) != 2 {
		return nil, fmt.Errorf("teardown retirement credential observer has %d fence probes, want 2", len(probes))
	}
	if err := validateTeardownRetirementCredentialSnapshot(snapshot); err != nil {
		return nil, err
	}

	endpoints := make([]teardownRetirementCredentialEndpoint, 0, len(snapshot.Endpoints))
	for _, endpoint := range snapshot.Endpoints {
		endpointConfig := rest.CopyConfig(endpoint.RESTConfig)
		// InClusterConfig already loaded this token. Disable file reloads so all
		// direct clients prove retirement of the exact credential frozen before
		// the cleanup ServiceAccount is deleted.
		endpointConfig.BearerTokenFile = ""
		client, err := clientFactory(endpointConfig)
		if err != nil {
			return nil, fmt.Errorf("create cleanup credential observer client for API endpoint %q: %w", endpoint.Address, err)
		}
		if client == nil {
			return nil, fmt.Errorf("cleanup credential observer client factory returned nil for API endpoint %q", endpoint.Address)
		}
		endpoints = append(endpoints, teardownRetirementCredentialEndpoint{name: endpoint.Address, client: client})
	}

	lifecycleCtx, cancel := context.WithCancel(ctx)
	topologyWatch, err := startWatch(lifecycleCtx, metav1.ListOptions{
		LabelSelector:       discoveryv1.LabelServiceName + "=" + kubernetesServiceName,
		ResourceVersion:     snapshot.InventoryResourceVersion,
		AllowWatchBookmarks: true,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("watch frozen Kubernetes API EndpointSlice inventory: %w", err)
	}
	if topologyWatch == nil {
		cancel()
		return nil, errors.New("watch frozen Kubernetes API EndpointSlice inventory returned nil")
	}
	topologyEvents := topologyWatch.ResultChan()
	if topologyEvents == nil {
		cancel()
		topologyWatch.Stop()
		return nil, errors.New("watch frozen Kubernetes API EndpointSlice inventory returned a nil result channel")
	}

	observer := &teardownRetirementCredentialObserver{
		guard:             guard,
		probes:            append([]crdupgrade.TeardownRetirementProbe(nil), probes...),
		endpoints:         endpoints,
		topologyWatch:     topologyWatch,
		topologyEvents:    topologyEvents,
		lifecycleCtx:      lifecycleCtx,
		cancel:            cancel,
		monitorDone:       make(chan struct{}),
		checkpoints:       make(chan chan struct{}),
		pollEvery:         admissionConvergencePollEvery,
		stabilityDuration: admissionConvergenceStabilityWindow,
		retirementTimeout: teardownRetirementCredentialTimeout,
		requestTimeout:    admissionConvergenceRequestTimeout,
		now:               time.Now,
		sleep:             sleepForAdmissionConvergence,
	}
	go observer.monitorTopology()
	return observer, nil
}

func validateTeardownRetirementCredentialSnapshot(snapshot kubernetesAPIServerEndpointSnapshot) error {
	if snapshot.InventoryResourceVersion == "" || snapshot.InventoryResourceVersion != strings.TrimSpace(snapshot.InventoryResourceVersion) {
		return errors.New("cleanup credential endpoint snapshot has an empty or padded resourceVersion")
	}
	if snapshot.InventoryIdentity == "" || snapshot.InventoryIdentity != strings.TrimSpace(snapshot.InventoryIdentity) {
		return errors.New("cleanup credential endpoint snapshot has an empty or padded identity")
	}
	if len(snapshot.Endpoints) == 0 {
		return errors.New("cleanup credential endpoint snapshot is empty")
	}
	seen := make(map[string]struct{}, len(snapshot.Endpoints))
	frozenToken := ""
	for _, endpoint := range snapshot.Endpoints {
		if endpoint.Address == "" || endpoint.Address != strings.TrimSpace(endpoint.Address) || endpoint.RESTConfig == nil {
			return errors.New("cleanup credential endpoint snapshot contains an incomplete endpoint")
		}
		if _, duplicate := seen[endpoint.Address]; duplicate {
			return fmt.Errorf("cleanup credential endpoint snapshot duplicates API endpoint %q", endpoint.Address)
		}
		seen[endpoint.Address] = struct{}{}
		if endpoint.RESTConfig.Host != "https://"+endpoint.Address || endpoint.RESTConfig.ServerName != kubernetesServiceTLSServerName {
			return fmt.Errorf("cleanup credential API endpoint %q is not pinned to its advertised address and Service TLS name", endpoint.Address)
		}
		if endpoint.RESTConfig.Insecure || (endpoint.RESTConfig.CAFile == "" && len(endpoint.RESTConfig.CAData) == 0) {
			return fmt.Errorf("cleanup credential API endpoint %q does not have a verified CA", endpoint.Address)
		}
		if endpoint.RESTConfig.BearerToken == "" {
			return fmt.Errorf("cleanup credential API endpoint %q has no frozen bearer token", endpoint.Address)
		}
		if frozenToken == "" {
			frozenToken = endpoint.RESTConfig.BearerToken
		} else if endpoint.RESTConfig.BearerToken != frozenToken {
			return errors.New("cleanup credential API endpoints do not share one frozen bearer token")
		}
	}
	return nil
}

func (o *teardownRetirementCredentialObserver) monitorTopology() {
	defer close(o.monitorDone)
	for {
		select {
		case <-o.lifecycleCtx.Done():
			return
		case event, ok := <-o.topologyEvents:
			if err := o.validateTopologyResult(event, ok); err != nil {
				o.failTopology(err)
				return
			}
		case checkpoint := <-o.checkpoints:
			if err := o.drainTopologyEvents(); err != nil {
				o.failTopology(err)
				close(checkpoint)
				return
			}
			close(checkpoint)
		}
	}
}

func (o *teardownRetirementCredentialObserver) drainTopologyEvents() error {
	for {
		select {
		case event, ok := <-o.topologyEvents:
			if err := o.validateTopologyResult(event, ok); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (o *teardownRetirementCredentialObserver) validateTopologyResult(event watch.Event, ok bool) error {
	if !ok {
		if o.lifecycleCtx.Err() != nil {
			return o.lifecycleCtx.Err()
		}
		return errors.New("Kubernetes API EndpointSlice inventory watch closed")
	}
	return validateTeardownRetirementTopologyEvent(event)
}

func validateTeardownRetirementTopologyEvent(event watch.Event) error {
	switch event.Type {
	case watch.Bookmark:
		bookmark, ok := event.Object.(*discoveryv1.EndpointSlice)
		if !ok || bookmark == nil || bookmark.ResourceVersion == "" || bookmark.ResourceVersion != strings.TrimSpace(bookmark.ResourceVersion) {
			return errors.New("Kubernetes API EndpointSlice inventory watch returned a malformed bookmark")
		}
		return nil
	case watch.Added, watch.Modified, watch.Deleted:
		slice, ok := event.Object.(*discoveryv1.EndpointSlice)
		if !ok || slice == nil || slice.Namespace != kubernetesServiceNamespace ||
			slice.Name == "" || slice.Name != strings.TrimSpace(slice.Name) ||
			slice.Labels[discoveryv1.LabelServiceName] != kubernetesServiceName {
			return fmt.Errorf("Kubernetes API EndpointSlice inventory watch returned a malformed %s event", event.Type)
		}
		return fmt.Errorf("Kubernetes API EndpointSlice inventory changed: %s EndpointSlice/%s", event.Type, slice.Name)
	case watch.Error:
		if event.Object == nil {
			return errors.New("Kubernetes API EndpointSlice inventory watch returned a nil error event")
		}
		return fmt.Errorf("Kubernetes API EndpointSlice inventory watch failed: %w", apierrors.FromObject(event.Object))
	default:
		return fmt.Errorf("Kubernetes API EndpointSlice inventory watch returned unknown event type %q", event.Type)
	}
}

func (o *teardownRetirementCredentialObserver) failTopology(err error) {
	o.failureMu.Lock()
	if o.failure == nil {
		o.failure = err
	}
	o.failureMu.Unlock()
	o.cancel()
}

func (o *teardownRetirementCredentialObserver) topologyError() error {
	o.failureMu.Lock()
	defer o.failureMu.Unlock()
	return o.failure
}

func (o *teardownRetirementCredentialObserver) verifyTopology() error {
	if o == nil || o.lifecycleCtx == nil || o.checkpoints == nil {
		return errors.New("teardown retirement credential observer is incomplete")
	}
	if err := o.topologyError(); err != nil {
		return err
	}
	if err := o.lifecycleCtx.Err(); err != nil {
		return err
	}
	checkpoint := make(chan struct{})
	select {
	case o.checkpoints <- checkpoint:
	case <-o.lifecycleCtx.Done():
		if err := o.topologyError(); err != nil {
			return err
		}
		return o.lifecycleCtx.Err()
	}
	select {
	case <-checkpoint:
	case <-o.lifecycleCtx.Done():
	}
	if err := o.topologyError(); err != nil {
		return err
	}
	if err := o.lifecycleCtx.Err(); err != nil {
		return err
	}
	return nil
}

// Wait proves that the deleted cleanup ServiceAccount credential is rejected
// by every frozen API endpoint continuously for the stability window. Reads of
// the terminal activation parameter may continue after Unauthorized, but no
// marker mutation is attempted for an endpoint once Unauthorized is observed.
func (o *teardownRetirementCredentialObserver) Wait(ctx context.Context) error {
	if o == nil || o.guard == nil || len(o.probes) != 2 || len(o.endpoints) == 0 ||
		o.topologyWatch == nil || o.topologyEvents == nil || o.lifecycleCtx == nil || o.cancel == nil || o.monitorDone == nil ||
		o.checkpoints == nil || o.pollEvery <= 0 || o.stabilityDuration <= 0 || o.retirementTimeout <= o.stabilityDuration ||
		o.requestTimeout <= 0 || o.now == nil || o.sleep == nil {
		return errors.New("teardown retirement credential observer is incomplete")
	}
	if ctx == nil {
		return errors.New("teardown retirement credential observer context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.verifyTopology(); err != nil {
		return fmt.Errorf("verify frozen Kubernetes API endpoint topology: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, o.retirementTimeout)
	stopTopologyCancellation := context.AfterFunc(o.lifecycleCtx, cancel)
	defer func() {
		stopTopologyCancellation()
		cancel()
	}()

	everUnauthorized := make(map[string]bool, len(o.endpoints))
	var allUnauthorizedSince time.Time
	for {
		if err := o.verifyTopology(); err != nil {
			return fmt.Errorf("verify frozen Kubernetes API endpoint topology: %w", err)
		}
		allUnauthorized := true
		for _, endpoint := range o.endpoints {
			requestCtx, requestCancel := context.WithTimeout(waitCtx, o.requestTimeout)
			unauthorized, err := o.observeEndpoint(requestCtx, endpoint, everUnauthorized[endpoint.name])
			requestCancel()
			if err != nil {
				if topologyErr := o.topologyError(); topologyErr != nil {
					return fmt.Errorf("verify frozen Kubernetes API endpoint topology: %w", topologyErr)
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				if waitErr := waitCtx.Err(); waitErr != nil {
					return o.waitFailure(waitErr)
				}
				return fmt.Errorf("observe cleanup credential retirement at API endpoint %q: %w", endpoint.name, err)
			}
			if unauthorized {
				everUnauthorized[endpoint.name] = true
				continue
			}
			allUnauthorized = false
		}
		if err := o.verifyTopology(); err != nil {
			return fmt.Errorf("verify frozen Kubernetes API endpoint topology: %w", err)
		}

		closedAt := o.now()
		if allUnauthorized {
			if allUnauthorizedSince.IsZero() {
				allUnauthorizedSince = closedAt
			} else if closedAt.Sub(allUnauthorizedSince) >= o.stabilityDuration {
				return nil
			}
		} else {
			allUnauthorizedSince = time.Time{}
		}

		if err := o.sleep(waitCtx, o.pollEvery); err != nil {
			if topologyErr := o.topologyError(); topologyErr != nil {
				return fmt.Errorf("verify frozen Kubernetes API endpoint topology: %w", topologyErr)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return o.waitFailure(err)
		}
	}
}

func (o *teardownRetirementCredentialObserver) observeEndpoint(
	ctx context.Context,
	endpoint teardownRetirementCredentialEndpoint,
	everUnauthorized bool,
) (bool, error) {
	phase, err := o.guard.Phase(ctx, endpoint.client)
	if apierrors.IsUnauthorized(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("derive post-retirement phase: %w", err)
	}
	if everUnauthorized {
		return false, errors.New("cleanup credential became authenticated after an Unauthorized response")
	}
	if phase != crdupgrade.TeardownRetirementTerminal {
		return false, fmt.Errorf("cleanup credential observed foreign teardown retirement phase %q", phase)
	}
	for _, probe := range o.probes {
		proven, err := o.guard.Probe(ctx, endpoint.client, probe)
		if apierrors.IsUnauthorized(err) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("probe post-retirement fence %s: %w", probe.PolicyName, err)
		}
		if !proven {
			return false, fmt.Errorf("post-retirement fence %s did not return its exact denial", probe.PolicyName)
		}
	}
	return false, nil
}

func (o *teardownRetirementCredentialObserver) waitFailure(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf(
			"cleanup credential was not continuously Unauthorized on every frozen API endpoint for %s within %s: %w",
			o.stabilityDuration,
			o.retirementTimeout,
			context.DeadlineExceeded,
		)
	}
	return err
}

func (o *teardownRetirementCredentialObserver) Close() {
	if o == nil {
		return
	}
	o.closeOnce.Do(func() {
		if o.cancel != nil {
			o.cancel()
		}
		if o.topologyWatch != nil {
			o.topologyWatch.Stop()
		}
		if o.monitorDone != nil {
			<-o.monitorDone
		}
	})
}

type teardownRetirementConfigMapClient interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

type teardownRetirementFinalizer struct {
	configMaps       teardownRetirementConfigMapClient
	markers          []crdupgrade.TeardownRetirementMarkerTarget
	activationName   string
	verifyActivation func(*corev1.ConfigMap) error
}

// newTeardownRetirementFinalizer creates the only mutating component in the
// final phase. Its client surface cannot mutate VAP/VAPB resources. Supplied
// secondary markers are deleted first and activation is the final mutation.
// The dedicated retirement marker is Helm-owned and deliberately remains
// available for direct fence probes after this finalizer returns.
func newTeardownRetirementFinalizer(
	configMaps teardownRetirementConfigMapClient,
	guard *crdupgrade.TeardownRetirementGuard,
	markers ...crdupgrade.TeardownRetirementMarkerTarget,
) (*teardownRetirementFinalizer, error) {
	if configMaps == nil || guard == nil {
		return nil, errors.New("teardown retirement finalizer dependencies are required")
	}
	dedicated, err := guard.MarkerTarget()
	if err != nil {
		return nil, fmt.Errorf("derive dedicated teardown retirement marker: %w", err)
	}
	seen := map[string]struct{}{dedicated.Name: {}}
	for _, marker := range markers {
		if marker.Name == "" || marker.Name != strings.TrimSpace(marker.Name) || marker.Verify == nil {
			return nil, errors.New("teardown retirement finalizer marker is incomplete")
		}
		if marker.Name == crdupgrade.ReleaseActivationName {
			return nil, errors.New("teardown retirement marker collides with release activation")
		}
		if _, duplicate := seen[marker.Name]; duplicate {
			return nil, fmt.Errorf("teardown retirement marker %q is duplicated", marker.Name)
		}
		seen[marker.Name] = struct{}{}
	}
	return &teardownRetirementFinalizer{
		configMaps:       configMaps,
		markers:          append([]crdupgrade.TeardownRetirementMarkerTarget(nil), markers...),
		activationName:   crdupgrade.ReleaseActivationName,
		verifyActivation: guard.VerifyFinalActivation,
	}, nil
}

func (f *teardownRetirementFinalizer) Finalize(ctx context.Context) error {
	if f == nil || f.configMaps == nil || f.activationName == "" || f.verifyActivation == nil {
		return errors.New("teardown retirement finalizer is incomplete")
	}
	if ctx == nil {
		return errors.New("teardown retirement finalizer context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	present, err := f.preflight(ctx)
	if err != nil {
		return err
	}
	if !present[len(present)-1] {
		return nil
	}
	for index, marker := range f.markers {
		if !present[index] {
			continue
		}
		if _, err := f.getExactActivation(ctx); err != nil {
			return fmt.Errorf("reverify release activation before marker deletion: %w", err)
		}
		object, err := f.configMaps.Get(ctx, marker.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("re-read teardown retirement marker %s: %w", marker.Name, err)
		}
		if err := verifyTeardownRetirementConfigMapIdentity(marker.Name, object, marker.Verify); err != nil {
			return err
		}
		if err := f.configMaps.Delete(ctx, marker.Name, teardownRetirementDeleteOptions(object)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete teardown retirement marker %s: %w", marker.Name, err)
		}
	}
	activation, err := f.getExactActivation(ctx)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("re-read release activation for final deletion: %w", err)
	}
	if err := f.configMaps.Delete(ctx, f.activationName, teardownRetirementDeleteOptions(activation)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete release activation as final API mutation: %w", err)
	}
	return nil
}

func (f *teardownRetirementFinalizer) preflight(ctx context.Context) ([]bool, error) {
	targets := make([]crdupgrade.TeardownRetirementMarkerTarget, 0, len(f.markers)+1)
	targets = append(targets, f.markers...)
	targets = append(targets, crdupgrade.TeardownRetirementMarkerTarget{Name: f.activationName, Verify: f.verifyActivation})
	present := make([]bool, len(targets))
	for index, target := range targets {
		object, err := f.configMaps.Get(ctx, target.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get teardown retirement ConfigMap/%s: %w", target.Name, err)
		}
		if err := verifyTeardownRetirementConfigMapIdentity(target.Name, object, target.Verify); err != nil {
			return nil, err
		}
		present[index] = true
	}
	activationPresent := present[len(present)-1]
	if !activationPresent {
		for index := range f.markers {
			if present[index] {
				return nil, fmt.Errorf("teardown retirement terminal state retains ConfigMap/%s after release activation is absent", f.markers[index].Name)
			}
		}
		return present, nil
	}
	sawPresent := false
	for index := range f.markers {
		if present[index] {
			sawPresent = true
			continue
		}
		if sawPresent {
			return nil, fmt.Errorf("teardown retirement deletion state has a non-contiguous absence at ConfigMap/%s", f.markers[index].Name)
		}
	}
	return present, nil
}

func (f *teardownRetirementFinalizer) getExactActivation(ctx context.Context) (*corev1.ConfigMap, error) {
	object, err := f.configMaps.Get(ctx, f.activationName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := verifyTeardownRetirementConfigMapIdentity(f.activationName, object, f.verifyActivation); err != nil {
		return nil, err
	}
	return object, nil
}

func verifyTeardownRetirementConfigMapIdentity(name string, object *corev1.ConfigMap, verify func(*corev1.ConfigMap) error) error {
	if object == nil || object.Name != name || object.UID == "" || object.ResourceVersion == "" {
		return fmt.Errorf("teardown retirement ConfigMap/%s has incomplete immutable deletion identity", name)
	}
	if err := verify(object); err != nil {
		return fmt.Errorf("verify teardown retirement ConfigMap/%s: %w", name, err)
	}
	return nil
}

func teardownRetirementDeleteOptions(object *corev1.ConfigMap) metav1.DeleteOptions {
	uid := object.UID
	resourceVersion := object.ResourceVersion
	return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}}
}
