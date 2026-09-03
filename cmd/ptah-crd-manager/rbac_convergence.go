package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	authorizationclientv1 "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

const (
	kubernetesServiceNamespace             = metav1.NamespaceDefault
	kubernetesServiceName                  = "kubernetes"
	kubernetesServiceTLSPortName           = "https"
	kubernetesServiceTLSServerName         = "kubernetes.default.svc"
	teardownEndpointSlicePageSize  int64   = 200
	teardownRBACPollEvery                  = 250 * time.Millisecond
	teardownRBACStabilityDuration          = 5 * time.Second
	teardownRBACQueriesPerSecond   float32 = 100
)

type endpointSliceLister interface {
	List(context.Context, metav1.ListOptions) (*discoveryv1.EndpointSliceList, error)
}

type authorizationReviewClientFactory func(*rest.Config) (crdupgrade.AuthorizationReviewClient, error)

// newTeardownRBACConvergenceBarrier discovers every ready address advertised
// by the in-cluster Kubernetes Service and creates one directly addressed
// SubjectAccessReview client per address. It deliberately bypasses the Service
// virtual IP so one warm authorizer cache cannot hide a stale advertised peer.
func newTeardownRBACConvergenceBarrier(
	ctx context.Context,
	config *rest.Config,
	clientset kubernetes.Interface,
	rollout *crdupgrade.RolloutGuard,
	contract crdupgrade.RuntimeAdmissionContract,
) (*crdupgrade.RBACConvergenceBarrier, error) {
	if clientset == nil {
		return nil, fmt.Errorf("kubernetes client is required for authorization convergence")
	}
	return newTeardownRBACConvergenceBarrierWith(
		ctx,
		config,
		clientset.DiscoveryV1().EndpointSlices(kubernetesServiceNamespace),
		rollout,
		contract,
		newDirectAuthorizationReviewClient,
	)
}

func newTeardownRBACConvergenceBarrierWith(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	rollout *crdupgrade.RolloutGuard,
	contract crdupgrade.RuntimeAdmissionContract,
	clientFactory authorizationReviewClientFactory,
) (*crdupgrade.RBACConvergenceBarrier, error) {
	if ctx == nil {
		return nil, fmt.Errorf("authorization convergence discovery context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config == nil {
		return nil, fmt.Errorf("in-cluster REST configuration is required for authorization convergence")
	}
	if config.Insecure {
		return nil, fmt.Errorf("in-cluster REST configuration must verify API server TLS certificates")
	}
	if config.CAFile == "" && len(config.CAData) == 0 {
		return nil, fmt.Errorf("in-cluster REST configuration has no API server CA")
	}
	if endpointSlices == nil {
		return nil, fmt.Errorf("EndpointSlice client is required for authorization convergence")
	}
	if clientFactory == nil {
		return nil, fmt.Errorf("authorization client factory is required")
	}

	probes, selfChecks, err := teardownAuthorizationProbes(rollout, contract)
	if err != nil {
		return nil, err
	}
	sweepBurst := len(selfChecks)
	for _, probe := range probes {
		sweepBurst += len(probe.Checks)
	}
	endpointProvider := newTeardownAuthorizationEndpointProvider(
		config,
		endpointSlices,
		sweepBurst,
		clientFactory,
	)
	clients, err := endpointProvider(ctx)
	if err != nil {
		return nil, err
	}

	barrier := crdupgrade.NewRBACConvergenceBarrier(
		clients,
		probes,
		selfChecks,
		teardownRBACPollEvery,
		teardownRBACStabilityDuration,
	)
	barrier.EndpointProvider = endpointProvider
	if err := barrier.Validate(); err != nil {
		return nil, fmt.Errorf("validate teardown authorization convergence barrier: %w", err)
	}
	return barrier, nil
}

func newTeardownAuthorizationEndpointProvider(
	config *rest.Config,
	endpointSlices endpointSliceLister,
	sweepBurst int,
	clientFactory authorizationReviewClientFactory,
) crdupgrade.AuthorizationEndpointProvider {
	clientsByAddress := make(map[string]crdupgrade.AuthorizationReviewClient)
	return func(ctx context.Context) ([]crdupgrade.NamedAuthorizationReviewClient, error) {
		addresses, err := discoverKubernetesAPIServerAddresses(ctx, endpointSlices)
		if err != nil {
			return nil, err
		}
		clients := make([]crdupgrade.NamedAuthorizationReviewClient, 0, len(addresses))
		for _, address := range addresses {
			client := clientsByAddress[address]
			if client == nil {
				endpointConfig := rest.CopyConfig(config)
				endpointConfig.Host = "https://" + address
				endpointConfig.ServerName = kubernetesServiceTLSServerName
				// A process-level HTTPS proxy would collapse independently addressed
				// probes back onto shared infrastructure. Advertised addresses are
				// reached directly from the in-cluster teardown Pod.
				endpointConfig.Proxy = directEndpointProxy
				// client-go otherwise installs its conservative 5 QPS / 10 burst
				// default independently for every direct client. One convergence
				// sweep intentionally covers every subject/check pair; allow exactly
				// that bounded burst. API-server throttling remains inconclusive and
				// resets the stability window.
				endpointConfig.RateLimiter = nil
				endpointConfig.QPS = teardownRBACQueriesPerSecond
				endpointConfig.Burst = sweepBurst

				client, err = clientFactory(endpointConfig)
				if err != nil {
					return nil, fmt.Errorf("create authorization client for advertised API endpoint %q: %w", address, err)
				}
				if client == nil {
					return nil, fmt.Errorf("authorization client factory returned nil for advertised API endpoint %q", address)
				}
				clientsByAddress[address] = client
			}
			clients = append(clients, crdupgrade.NamedAuthorizationReviewClient{Name: address, Client: client})
		}
		return clients, nil
	}
}

func newDirectAuthorizationReviewClient(config *rest.Config) (crdupgrade.AuthorizationReviewClient, error) {
	client, err := authorizationclientv1.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &directAuthorizationReviewClient{client: client}, nil
}

type directAuthorizationReviewClient struct {
	client authorizationclientv1.AuthorizationV1Interface
}

func (c *directAuthorizationReviewClient) CreateSubjectAccessReview(
	ctx context.Context,
	review *authorizationv1.SubjectAccessReview,
	options metav1.CreateOptions,
) (*authorizationv1.SubjectAccessReview, error) {
	return c.client.SubjectAccessReviews().Create(ctx, review, options)
}

func (c *directAuthorizationReviewClient) CreateSelfSubjectAccessReview(
	ctx context.Context,
	review *authorizationv1.SelfSubjectAccessReview,
	options metav1.CreateOptions,
) (*authorizationv1.SelfSubjectAccessReview, error) {
	return c.client.SelfSubjectAccessReviews().Create(ctx, review, options)
}

func directEndpointProxy(*http.Request) (*url.URL, error) {
	return nil, nil
}

func discoverKubernetesAPIServerAddresses(ctx context.Context, endpointSlices endpointSliceLister) ([]string, error) {
	const selector = discoveryv1.LabelServiceName + "=" + kubernetesServiceName

	seenTokens := map[string]struct{}{"": {}}
	seenSlices := make(map[string]struct{})
	seenSliceUIDs := make(map[string]string)
	seenAddresses := make(map[string]struct{})
	var addresses []string
	continueToken := ""
	resourceVersion := ""
	firstPage := true

	for {
		page, err := endpointSlices.List(ctx, metav1.ListOptions{
			LabelSelector: selector,
			Limit:         teardownEndpointSlicePageSize,
			Continue:      continueToken,
		})
		if err != nil {
			return nil, fmt.Errorf("list default Kubernetes Service EndpointSlices: %w", err)
		}
		if page == nil {
			return nil, fmt.Errorf("list default Kubernetes Service EndpointSlices returned a nil page")
		}
		if page.ResourceVersion == "" {
			return nil, fmt.Errorf("default Kubernetes Service EndpointSlice inventory returned an empty resourceVersion")
		}
		if page.RemainingItemCount != nil && *page.RemainingItemCount < 0 {
			return nil, fmt.Errorf("default Kubernetes Service EndpointSlice inventory returned a negative remaining item count")
		}
		if firstPage {
			resourceVersion = page.ResourceVersion
			firstPage = false
		} else if page.ResourceVersion != resourceVersion {
			return nil, fmt.Errorf(
				"default Kubernetes Service EndpointSlice inventory changed resourceVersion across pages: %q then %q",
				resourceVersion,
				page.ResourceVersion,
			)
		}

		for index := range page.Items {
			slice := &page.Items[index]
			if slice.Namespace != kubernetesServiceNamespace {
				return nil, fmt.Errorf("EndpointSlice %q has namespace %q, want %q", slice.Name, slice.Namespace, kubernetesServiceNamespace)
			}
			if strings.TrimSpace(slice.Name) == "" || slice.Name != strings.TrimSpace(slice.Name) {
				return nil, fmt.Errorf("default Kubernetes Service EndpointSlice has an empty or padded name")
			}
			if slice.UID == "" {
				return nil, fmt.Errorf("default Kubernetes Service EndpointSlice %q has an empty UID", slice.Name)
			}
			if previous, duplicate := seenSliceUIDs[string(slice.UID)]; duplicate {
				return nil, fmt.Errorf(
					"default Kubernetes Service EndpointSlices %q and %q share UID %q",
					previous,
					slice.Name,
					slice.UID,
				)
			}
			seenSliceUIDs[string(slice.UID)] = slice.Name
			if slice.Labels[discoveryv1.LabelServiceName] != kubernetesServiceName {
				return nil, fmt.Errorf("EndpointSlice %q is not owned by the default Kubernetes Service", slice.Name)
			}
			if _, duplicate := seenSlices[slice.Name]; duplicate {
				return nil, fmt.Errorf("default Kubernetes Service EndpointSlice %q appeared more than once", slice.Name)
			}
			seenSlices[slice.Name] = struct{}{}

			port, portErr := endpointSliceHTTPSPort(slice)
			if portErr != nil {
				return nil, portErr
			}
			for endpointIndex := range slice.Endpoints {
				endpoint := &slice.Endpoints[endpointIndex]
				if endpointTerminating(endpoint) {
					continue
				}
				canonicalAddresses, addressErr := endpointIPAddresses(slice, endpoint, endpointIndex)
				if addressErr != nil {
					return nil, addressErr
				}
				if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
					return nil, fmt.Errorf("EndpointSlice %q endpoint %d is non-terminating but not ready", slice.Name, endpointIndex)
				}
				if endpoint.Conditions.Serving != nil && !*endpoint.Conditions.Serving {
					return nil, fmt.Errorf("EndpointSlice %q endpoint %d is non-terminating but not serving", slice.Name, endpointIndex)
				}
				for _, address := range canonicalAddresses {
					hostPort := net.JoinHostPort(address, fmt.Sprintf("%d", port))
					if _, duplicate := seenAddresses[hostPort]; duplicate {
						continue
					}
					seenAddresses[hostPort] = struct{}{}
					addresses = append(addresses, hostPort)
				}
			}
		}

		next := page.Continue
		if next == "" {
			break
		}
		if _, duplicate := seenTokens[next]; duplicate {
			return nil, fmt.Errorf("default Kubernetes Service EndpointSlice inventory repeated continue token %q", next)
		}
		seenTokens[next] = struct{}{}
		continueToken = next
	}

	if len(seenSlices) == 0 {
		return nil, fmt.Errorf("default Kubernetes Service has no EndpointSlices")
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("default Kubernetes Service has no ready, serving, non-terminating API server endpoints")
	}
	sort.Strings(addresses)
	return addresses, nil
}

func endpointSliceHTTPSPort(slice *discoveryv1.EndpointSlice) (int32, error) {
	var port int32
	found := false
	for index := range slice.Ports {
		candidate := &slice.Ports[index]
		if candidate.Name == nil || *candidate.Name != kubernetesServiceTLSPortName {
			continue
		}
		if found {
			return 0, fmt.Errorf("EndpointSlice %q has more than one %q port", slice.Name, kubernetesServiceTLSPortName)
		}
		if candidate.Port == nil || *candidate.Port < 1 || *candidate.Port > 65535 {
			return 0, fmt.Errorf("EndpointSlice %q has an invalid %q port number", slice.Name, kubernetesServiceTLSPortName)
		}
		if candidate.Protocol != nil && *candidate.Protocol != corev1.ProtocolTCP {
			return 0, fmt.Errorf("EndpointSlice %q has a non-TCP %q port", slice.Name, kubernetesServiceTLSPortName)
		}
		port = *candidate.Port
		found = true
	}
	if !found {
		return 0, fmt.Errorf("EndpointSlice %q has no named and numbered %q port", slice.Name, kubernetesServiceTLSPortName)
	}
	return port, nil
}

func endpointIPAddresses(
	slice *discoveryv1.EndpointSlice,
	endpoint *discoveryv1.Endpoint,
	endpointIndex int,
) ([]string, error) {
	if slice.AddressType != discoveryv1.AddressTypeIPv4 && slice.AddressType != discoveryv1.AddressTypeIPv6 {
		return nil, fmt.Errorf("EndpointSlice %q has unsupported address type %q", slice.Name, slice.AddressType)
	}
	if len(endpoint.Addresses) == 0 {
		return nil, fmt.Errorf("EndpointSlice %q endpoint %d has no addresses", slice.Name, endpointIndex)
	}

	addresses := make([]string, 0, len(endpoint.Addresses))
	seen := make(map[string]struct{}, len(endpoint.Addresses))
	for _, raw := range endpoint.Addresses {
		parsed := net.ParseIP(raw)
		if parsed == nil || parsed.IsUnspecified() || parsed.IsMulticast() || parsed.IsLinkLocalUnicast() {
			return nil, fmt.Errorf("EndpointSlice %q endpoint %d has invalid IP address %q", slice.Name, endpointIndex, raw)
		}
		isIPv4 := parsed.To4() != nil
		if (slice.AddressType == discoveryv1.AddressTypeIPv4) != isIPv4 {
			return nil, fmt.Errorf("EndpointSlice %q endpoint %d address %q does not match address type %q", slice.Name, endpointIndex, raw, slice.AddressType)
		}
		canonical := parsed.String()
		if _, duplicate := seen[canonical]; duplicate {
			return nil, fmt.Errorf("EndpointSlice %q endpoint %d repeats address %q", slice.Name, endpointIndex, canonical)
		}
		seen[canonical] = struct{}{}
		addresses = append(addresses, canonical)
	}
	return addresses, nil
}

func endpointTerminating(endpoint *discoveryv1.Endpoint) bool {
	return endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating
}

func teardownAuthorizationSubjects(
	rollout *crdupgrade.RolloutGuard,
	contract crdupgrade.RuntimeAdmissionContract,
) ([]crdupgrade.AuthorizationSubject, error) {
	if rollout == nil {
		return nil, fmt.Errorf("rollout guard is required for authorization convergence")
	}
	if rollout.ReleaseNamespace == "" || rollout.ReleaseNamespace != strings.TrimSpace(rollout.ReleaseNamespace) {
		return nil, fmt.Errorf("rollout release namespace is empty or padded")
	}
	if contract.Namespace != rollout.ReleaseNamespace {
		return nil, fmt.Errorf("runtime admission namespace %q differs from rollout namespace %q", contract.Namespace, rollout.ReleaseNamespace)
	}
	if contract.ControllerServiceAccountName != rollout.ControllerServiceAccountName {
		return nil, fmt.Errorf(
			"runtime admission controller ServiceAccount %q differs from rollout identity %q",
			contract.ControllerServiceAccountName,
			rollout.ControllerServiceAccountName,
		)
	}
	if contract.CertificateServiceAccountName != rollout.CertificateDeploymentName {
		return nil, fmt.Errorf(
			"runtime admission certificate ServiceAccount %q differs from rollout identity %q",
			contract.CertificateServiceAccountName,
			rollout.CertificateDeploymentName,
		)
	}

	type serviceAccountIdentity struct {
		name           string
		serviceAccount string
	}
	identities := []serviceAccountIdentity{
		{name: "controller", serviceAccount: contract.ControllerServiceAccountName},
	}
	if contract.CertificateRuntimeEnabled {
		identities = append(identities, serviceAccountIdentity{name: "certificate", serviceAccount: contract.CertificateServiceAccountName})
	}
	// The quiesce Job intentionally uses the one-shot hook identity. One exact
	// canonical SAR subject therefore proves revocation for both retired roles.
	// The live cleanup Job is deliberately absent: SelfSubjectAccessReview
	// probes its current authenticated credential without attempting to
	// synthesize token-bound UID or authenticator extras.
	identities = append(identities,
		serviceAccountIdentity{name: "hook-quiesce", serviceAccount: rollout.HookServiceAccountName},
	)

	seen := make(map[string]string, len(identities))
	subjects := make([]crdupgrade.AuthorizationSubject, 0, len(identities))
	for _, identity := range identities {
		if identity.serviceAccount == "" || identity.serviceAccount != strings.TrimSpace(identity.serviceAccount) {
			return nil, fmt.Errorf("authorization convergence %s ServiceAccount is empty or padded", identity.name)
		}
		if previous, duplicate := seen[identity.serviceAccount]; duplicate {
			return nil, fmt.Errorf(
				"authorization convergence %s and %s identities share ServiceAccount %q",
				previous,
				identity.name,
				identity.serviceAccount,
			)
		}
		seen[identity.serviceAccount] = identity.name
		subjects = append(subjects, serviceAccountAuthorizationSubject(identity.name, rollout.ReleaseNamespace, identity.serviceAccount))
	}
	return subjects, nil
}

func serviceAccountAuthorizationSubject(name, namespace, serviceAccount string) crdupgrade.AuthorizationSubject {
	return crdupgrade.AuthorizationSubject{
		Name: name,
		User: "system:serviceaccount:" + namespace + ":" + serviceAccount,
		Groups: []string{
			"system:serviceaccounts",
			"system:serviceaccounts:" + namespace,
			"system:authenticated",
		},
	}
}

func teardownAuthorizationProbes(
	rollout *crdupgrade.RolloutGuard,
	contract crdupgrade.RuntimeAdmissionContract,
) ([]crdupgrade.AuthorizationProbe, []crdupgrade.AuthorizationCheck, error) {
	subjects, err := teardownAuthorizationSubjects(rollout, contract)
	if err != nil {
		return nil, nil, err
	}
	sets, err := buildTeardownAuthorizationChecks(rollout, contract)
	if err != nil {
		return nil, nil, err
	}
	checksBySubject := map[string][]crdupgrade.AuthorizationCheck{
		"controller":   sets.controller,
		"certificate":  sets.certificate,
		"hook-quiesce": sets.hook,
	}
	probes := make([]crdupgrade.AuthorizationProbe, 0, len(subjects))
	for _, subject := range subjects {
		checks := checksBySubject[subject.Name]
		if len(checks) == 0 {
			return nil, nil, fmt.Errorf("authorization convergence subject %q has no retired privilege checks", subject.Name)
		}
		probes = append(probes, crdupgrade.AuthorizationProbe{Subject: subject, Checks: checks})
	}
	grants, err := crdupgrade.RevokedPrivilegeMutationGrants(rollout, contract)
	if err != nil {
		return nil, nil, fmt.Errorf("compile revoked privilege grants: %w", err)
	}
	if err := validateTeardownAuthorizationProbeCompleteness(probes, sets.cleanup, grants); err != nil {
		return nil, nil, err
	}
	return probes, sets.cleanup, nil
}

type teardownAuthorizationSubjectCheck struct {
	subject string
	check   crdupgrade.AuthorizationCheck
}

func validateTeardownAuthorizationProbeCompleteness(
	probes []crdupgrade.AuthorizationProbe,
	selfChecks []crdupgrade.AuthorizationCheck,
	grants []crdupgrade.RevokedPrivilegeMutationGrant,
) error {
	actual := make([]teardownAuthorizationSubjectCheck, 0, len(selfChecks))
	for _, probe := range probes {
		for _, check := range probe.Checks {
			actual = append(actual, teardownAuthorizationSubjectCheck{subject: probe.Subject.Name, check: check})
		}
	}
	for _, check := range selfChecks {
		actual = append(actual, teardownAuthorizationSubjectCheck{subject: "cleanup", check: check})
	}

	for _, grant := range grants {
		if len(grant.ResourceNames) == 0 {
			if !hasMatchingAuthorizationProbe(actual, grant, "") {
				return fmt.Errorf("authorization convergence probes omit unbounded revoked grant %s", revokedPrivilegeGrantDescription(grant, ""))
			}
			continue
		}
		for _, name := range grant.ResourceNames {
			if !hasMatchingAuthorizationProbe(actual, grant, name) {
				return fmt.Errorf("authorization convergence probes omit revoked grant %s", revokedPrivilegeGrantDescription(grant, name))
			}
		}
	}
	for _, candidate := range actual {
		matched := false
		for _, grant := range grants {
			if authorizationProbeMatchesGrant(candidate.subject, candidate.check, grant, "") {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("authorization convergence subject %q check %q is not present in an exact revoked role contract", candidate.subject, candidate.check.Name)
		}
	}
	return nil
}

func hasMatchingAuthorizationProbe(
	actual []teardownAuthorizationSubjectCheck,
	grant crdupgrade.RevokedPrivilegeMutationGrant,
	resourceName string,
) bool {
	for _, candidate := range actual {
		if authorizationProbeMatchesGrant(candidate.subject, candidate.check, grant, resourceName) {
			return true
		}
	}
	return false
}

func authorizationProbeMatchesGrant(
	subject string,
	check crdupgrade.AuthorizationCheck,
	grant crdupgrade.RevokedPrivilegeMutationGrant,
	requiredResourceName string,
) bool {
	if subject != grant.SubjectName {
		return false
	}
	if grant.NonResourceURL != "" {
		attributes := check.NonResourceAttributes
		return attributes != nil && attributes.Verb == grant.Verb && attributes.Path == grant.NonResourceURL
	}
	attributes := check.ResourceAttributes
	if attributes == nil || attributes.Verb != grant.Verb || attributes.Group != grant.APIGroup ||
		attributes.Resource != grant.Resource || attributes.Subresource != grant.Subresource {
		return false
	}
	if !grant.ClusterWide && attributes.Namespace != grant.Namespace {
		return false
	}
	if requiredResourceName != "" {
		return attributes.Name == requiredResourceName
	}
	if len(grant.ResourceNames) == 0 {
		return true
	}
	for _, name := range grant.ResourceNames {
		if attributes.Name == name {
			return true
		}
	}
	return false
}

func revokedPrivilegeGrantDescription(grant crdupgrade.RevokedPrivilegeMutationGrant, resourceName string) string {
	if grant.NonResourceURL != "" {
		return fmt.Sprintf("subject=%q verb=%q path=%q", grant.SubjectName, grant.Verb, grant.NonResourceURL)
	}
	return fmt.Sprintf(
		"subject=%q verb=%q group=%q resource=%q subresource=%q namespace=%q name=%q",
		grant.SubjectName,
		grant.Verb,
		grant.APIGroup,
		grant.Resource,
		grant.Subresource,
		grant.Namespace,
		resourceName,
	)
}

type teardownAuthorizationCheckTarget uint8

const (
	teardownCheckHook teardownAuthorizationCheckTarget = 1 << iota
	teardownCheckController
	teardownCheckCertificate
	teardownCheckCleanup
)

type teardownAuthorizationCheckSets struct {
	all         []crdupgrade.AuthorizationCheck
	hook        []crdupgrade.AuthorizationCheck
	controller  []crdupgrade.AuthorizationCheck
	certificate []crdupgrade.AuthorizationCheck
	cleanup     []crdupgrade.AuthorizationCheck
}

func teardownAuthorizationChecks(
	rollout *crdupgrade.RolloutGuard,
	contract crdupgrade.RuntimeAdmissionContract,
) ([]crdupgrade.AuthorizationCheck, error) {
	sets, err := buildTeardownAuthorizationChecks(rollout, contract)
	return sets.all, err
}

func buildTeardownAuthorizationChecks(
	rollout *crdupgrade.RolloutGuard,
	contract crdupgrade.RuntimeAdmissionContract,
) (teardownAuthorizationCheckSets, error) {
	if rollout == nil {
		return teardownAuthorizationCheckSets{}, fmt.Errorf("rollout guard is required for authorization convergence")
	}
	for name, value := range map[string]string{
		"release name":                         rollout.ReleaseName,
		"release namespace":                    rollout.ReleaseNamespace,
		"coordination namespace":               rollout.CoordinationNamespace,
		"controller Deployment":                rollout.ControllerDeploymentName,
		"certificate Deployment":               rollout.CertificateDeploymentName,
		"controller ServiceAccount":            rollout.ControllerServiceAccountName,
		"hook ServiceAccount":                  rollout.HookServiceAccountName,
		"webhook Secret":                       rollout.WebhookSecretName,
		"leader-election ID":                   rollout.LeaderElectionID,
		"manager image":                        rollout.ManagerImage,
		"runtime admission contract namespace": contract.Namespace,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return teardownAuthorizationCheckSets{}, fmt.Errorf("authorization convergence %s is empty or padded", name)
		}
	}
	if contract.Namespace != rollout.ReleaseNamespace {
		return teardownAuthorizationCheckSets{}, fmt.Errorf("runtime admission namespace %q differs from rollout namespace %q", contract.Namespace, rollout.ReleaseNamespace)
	}

	quiesceName, err := crdupgrade.TeardownQuiesceJobName(rollout.HookServiceAccountName)
	if err != nil {
		return teardownAuthorizationCheckSets{}, fmt.Errorf("derive teardown quiesce identity: %w", err)
	}
	bootstrapName, err := teardownBoundedRoleName(rollout.HookServiceAccountName, 53, "bootstrap")
	if err != nil {
		return teardownAuthorizationCheckSets{}, err
	}
	probeRoleName, err := teardownBoundedRoleName(rollout.HookServiceAccountName, 57, "probe")
	if err != nil {
		return teardownAuthorizationCheckSets{}, err
	}
	cleanupPrivilegeName, err := crdupgrade.TeardownPrivilegeRoleName(rollout.HookServiceAccountName)
	if err != nil {
		return teardownAuthorizationCheckSets{}, fmt.Errorf("derive teardown cleanup privilege identity: %w", err)
	}
	probeObjectName := crdupgrade.HookIdentityProbeObjectName(
		rollout.ReleaseNamespace,
		rollout.ReleaseName,
		rollout.ReleaseSequence,
		rollout.ManagerImage,
	)
	const arbitraryObjectName = "ptah-authorization-revocation-probe"

	sets := teardownAuthorizationCheckSets{all: make([]crdupgrade.AuthorizationCheck, 0, 64)}
	appendResource := func(targets teardownAuthorizationCheckTarget, name, group, version, resourceName, subresource, namespace, verb, objectName string) {
		check := crdupgrade.AuthorizationCheck{
			Name: name,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   namespace,
				Verb:        verb,
				Group:       group,
				Version:     version,
				Resource:    resourceName,
				Subresource: subresource,
				Name:        objectName,
			},
		}
		sets.all = append(sets.all, check)
		if targets&teardownCheckHook != 0 {
			sets.hook = append(sets.hook, check)
		}
		if targets&teardownCheckController != 0 {
			sets.controller = append(sets.controller, check)
		}
		if targets&teardownCheckCertificate != 0 {
			sets.certificate = append(sets.certificate, check)
		}
		if targets&teardownCheckCleanup != 0 {
			sets.cleanup = append(sets.cleanup, check)
		}
	}

	// Privileged schema/admission hook mutations.
	for _, crdName := range []string{
		"ptahschemaapprovals.operator.ptah.dev",
		"ptahschemaplans.operator.ptah.dev",
		"ptahschemas.operator.ptah.dev",
	} {
		appendResource(teardownCheckHook, "update CRD "+crdName, "apiextensions.k8s.io", "v1", "customresourcedefinitions", "", "", "update", crdName)
	}
	appendResource(teardownCheckHook, "update controller Deployment", "apps", "v1", "deployments", "", rollout.ReleaseNamespace, "update", rollout.ControllerDeploymentName)
	appendResource(teardownCheckHook, "update certificate Deployment", "apps", "v1", "deployments", "", rollout.ReleaseNamespace, "update", rollout.CertificateDeploymentName)
	appendResource(teardownCheckHook, "create guarded Deployment", "apps", "v1", "deployments", "", rollout.ReleaseNamespace, "create", rollout.ControllerDeploymentName)
	appendResource(teardownCheckHook|teardownCheckCertificate, "update mutating admission singleton", "admissionregistration.k8s.io", "v1", "mutatingwebhookconfigurations", "", "", "update", crdupgrade.AdmissionConfigurationName)
	appendResource(teardownCheckHook|teardownCheckCertificate, "update validating admission singleton", "admissionregistration.k8s.io", "v1", "validatingwebhookconfigurations", "", "", "update", crdupgrade.AdmissionConfigurationName)
	appendResource(teardownCheckHook, "update release activation ConfigMap", "", "v1", "configmaps", "", rollout.ReleaseNamespace, "update", crdupgrade.ReleaseActivationName)
	appendResource(teardownCheckHook, "update hook identity probe ConfigMap", "", "v1", "configmaps", "", rollout.ReleaseNamespace, "update", probeObjectName)
	appendResource(teardownCheckHook, "mint hook ServiceAccount token", "", "v1", "serviceaccounts", "token", rollout.ReleaseNamespace, "create", rollout.HookServiceAccountName)

	// Controller mutations. Every resource/subresource and mutating verb from
	// the installed controller roles gets a distinct probe; this does not rely
	// on one cached RBAC rule being observed atomically.
	for _, verb := range []string{"update", "patch"} {
		appendResource(teardownCheckController, verb+" PtahSchema", "operator.ptah.dev", "v1alpha1", "ptahschemas", "", rollout.ReleaseNamespace, verb, arbitraryObjectName)
	}
	appendResource(teardownCheckController, "update PtahSchema finalizer", "operator.ptah.dev", "v1alpha1", "ptahschemas", "finalizers", rollout.ReleaseNamespace, "update", arbitraryObjectName)
	appendResource(teardownCheckController, "update PtahSchemaPlan finalizer", "operator.ptah.dev", "v1alpha1", "ptahschemaplans", "finalizers", rollout.ReleaseNamespace, "update", arbitraryObjectName)
	for _, target := range []struct {
		name     string
		resource string
	}{
		{name: "PtahSchema", resource: "ptahschemas"},
		{name: "PtahSchemaPlan", resource: "ptahschemaplans"},
		{name: "PtahSchemaApproval", resource: "ptahschemaapprovals"},
	} {
		for _, verb := range []string{"update", "patch"} {
			appendResource(teardownCheckController, verb+" "+target.name+" status", "operator.ptah.dev", "v1alpha1", target.resource, "status", rollout.ReleaseNamespace, verb, arbitraryObjectName)
		}
	}
	appendResource(teardownCheckController, "create PtahSchemaPlan", "operator.ptah.dev", "v1alpha1", "ptahschemaplans", "", rollout.ReleaseNamespace, "create", arbitraryObjectName)
	for _, verb := range []string{"create", "patch"} {
		appendResource(teardownCheckController, verb+" operation Job", "batch", "v1", "jobs", "", rollout.ReleaseNamespace, verb, arbitraryObjectName)
	}
	appendResource(teardownCheckController, "create controller ConfigMap", "", "v1", "configmaps", "", rollout.ReleaseNamespace, "create", arbitraryObjectName)
	for _, verb := range []string{"create", "patch", "update"} {
		appendResource(teardownCheckController, verb+" controller Event", "", "v1", "events", "", rollout.ReleaseNamespace, verb, arbitraryObjectName)
	}
	for _, verb := range []string{"create", "update"} {
		appendResource(teardownCheckController, verb+" leader-election Lease", "coordination.k8s.io", "v1", "leases", "", rollout.CoordinationNamespace, verb, rollout.LeaderElectionID)
	}

	// Certificate rotation mutations only exist when that runtime is enabled.
	if contract.CertificateRuntimeEnabled {
		certificateLeaseName, leaseErr := exactRuntimeArgument(rollout.CertificateArgs, "--lease-name=")
		if leaseErr != nil {
			return teardownAuthorizationCheckSets{}, fmt.Errorf("certificate rotation lease identity: %w", leaseErr)
		}
		appendResource(teardownCheckCertificate, "update webhook Secret", "", "v1", "secrets", "", rollout.ReleaseNamespace, "update", rollout.WebhookSecretName)
		recreateMissingSecret, recreateErr := optionalExactBooleanRuntimeArgument(rollout.CertificateArgs, "--recreate-missing-secret=")
		if recreateErr != nil {
			return teardownAuthorizationCheckSets{}, recreateErr
		}
		if recreateMissingSecret {
			appendResource(teardownCheckCertificate, "create webhook Secret", "", "v1", "secrets", "", rollout.ReleaseNamespace, "create", rollout.WebhookSecretName)
		}
		appendResource(teardownCheckCertificate, "update certificate rotation Lease", "coordination.k8s.io", "v1", "leases", "", rollout.ReleaseNamespace, "update", certificateLeaseName)
	}

	// Temporary cleanup mutation is name-bounded. Probe every non-residual
	// binding name that the chart grants the cleanup identity permission to
	// delete, so a partially converged resourceNames set cannot be hidden.
	clusterRBACNames := []string{
		rollout.ControllerDeploymentName,
		rollout.HookServiceAccountName,
		bootstrapName,
		quiesceName,
	}
	namespacedRBACNames := []string{
		rollout.ControllerDeploymentName + "-runtime-admission",
		rollout.HookServiceAccountName,
		bootstrapName,
		probeRoleName,
		quiesceName,
	}
	if rollout.CoordinationNamespace == rollout.ReleaseNamespace {
		namespacedRBACNames = append(namespacedRBACNames, rollout.ControllerDeploymentName, cleanupPrivilegeName)
	} else {
		namespacedRBACNames = append(namespacedRBACNames, cleanupPrivilegeName)
	}
	if contract.CertificateRuntimeEnabled {
		clusterRBACNames = append(clusterRBACNames, rollout.CertificateDeploymentName)
		namespacedRBACNames = append(namespacedRBACNames, rollout.CertificateDeploymentName)
	}
	rbacTargets := []struct {
		kind      string
		resource  string
		namespace string
		names     []string
	}{
		{kind: "ClusterRoleBinding", resource: "clusterrolebindings", names: clusterRBACNames},
		{kind: "RoleBinding", resource: "rolebindings", namespace: rollout.ReleaseNamespace, names: namespacedRBACNames},
	}
	if rollout.CoordinationNamespace != rollout.ReleaseNamespace {
		rbacTargets = append(rbacTargets, struct {
			kind      string
			resource  string
			namespace string
			names     []string
		}{
			kind:      "RoleBinding",
			resource:  "rolebindings",
			namespace: rollout.CoordinationNamespace,
			names:     []string{rollout.ControllerDeploymentName, cleanupPrivilegeName},
		})
	}
	for _, target := range rbacTargets {
		for _, name := range target.names {
			diagnosticName := "delete " + target.kind + " " + name
			if target.resource == "rolebindings" && name == cleanupPrivilegeName {
				diagnosticName = "delete " + target.kind + " " + target.namespace + "/" + name
			}
			appendResource(teardownCheckCleanup, diagnosticName, "rbac.authorization.k8s.io", "v1", target.resource, "", target.namespace, "delete", name)
		}
	}
	serviceAccounts := []string{rollout.HookServiceAccountName}
	if contract.ControllerServiceAccountCreate {
		serviceAccounts = append(serviceAccounts, rollout.ControllerServiceAccountName)
	}
	if contract.CertificateRuntimeEnabled {
		serviceAccounts = append(serviceAccounts, rollout.CertificateDeploymentName)
	}
	for _, name := range serviceAccounts {
		appendResource(teardownCheckCleanup, "delete ServiceAccount "+name, "", "v1", "serviceaccounts", "", rollout.ReleaseNamespace, "delete", name)
	}

	return sets, nil
}

func teardownBoundedRoleName(base string, maximumBaseLength int, suffix string) (string, error) {
	if base == "" || base != strings.TrimSpace(base) || maximumBaseLength < 1 || suffix == "" {
		return "", fmt.Errorf("cannot derive bounded teardown role name")
	}
	if len(base) > maximumBaseLength {
		base = base[:maximumBaseLength]
	}
	base = strings.TrimSuffix(base, "-")
	if base == "" {
		return "", fmt.Errorf("cannot derive bounded teardown role name")
	}
	name := base + "-" + suffix
	if len(name) > 63 {
		return "", fmt.Errorf("derived teardown role name %q exceeds 63 characters", name)
	}
	return name, nil
}

func exactRuntimeArgument(arguments []string, prefix string) (string, error) {
	found := ""
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, prefix) {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("runtime argument %s is duplicated", prefix)
		}
		found = strings.TrimPrefix(argument, prefix)
		if found == "" || found != strings.TrimSpace(found) {
			return "", fmt.Errorf("runtime argument %s has an empty or padded value", prefix)
		}
	}
	if found == "" {
		return "", fmt.Errorf("runtime argument %s is required", prefix)
	}
	return found, nil
}

func optionalExactBooleanRuntimeArgument(arguments []string, prefix string) (bool, error) {
	found := false
	value := false
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, prefix) {
			continue
		}
		if found {
			return false, fmt.Errorf("runtime argument %s is duplicated", prefix)
		}
		found = true
		switch strings.TrimPrefix(argument, prefix) {
		case "true":
			value = true
		case "false":
			value = false
		default:
			return false, fmt.Errorf("runtime argument %s must be exactly true or false", prefix)
		}
	}
	return value, nil
}
