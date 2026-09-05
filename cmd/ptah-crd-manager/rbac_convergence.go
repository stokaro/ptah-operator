package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	authorizationclientv1 "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
	"github.com/stokaro/ptah-operator/internal/kubeapi"
)

const (
	kubernetesServiceNamespace     = metav1.NamespaceDefault
	kubernetesServiceName          = "kubernetes"
	kubernetesServiceTLSServerName = "kubernetes.default.svc"
	authorizationPollEvery         = 250 * time.Millisecond
	authorizationStabilityDuration = 5 * time.Second
	authorizationRequestTimeout    = 5 * time.Second
)

type endpointSliceLister = kubeapi.EndpointSliceLister
type kubernetesAPIServerEndpoint = kubeapi.Endpoint
type kubernetesAPIServerEndpointSnapshot = kubeapi.Snapshot
type kubernetesAPIServerEndpointProvider = kubeapi.Provider

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
	if err := validateAuthorizationConvergenceInputs(ctx, config, endpointSlices, clientFactory); err != nil {
		return nil, err
	}

	probes, selfChecks, err := teardownAuthorizationProbes(rollout, contract)
	if err != nil {
		return nil, err
	}
	return newAuthorizationConvergenceBarrier(
		ctx,
		config,
		endpointSlices,
		probes,
		selfChecks,
		clientFactory,
		"teardown",
	)
}

func newControllerRBACConvergenceBarrier(
	ctx context.Context,
	config *rest.Config,
	clientset kubernetes.Interface,
	transition *crdupgrade.ControllerRBACTransition,
) (*crdupgrade.RBACConvergenceBarrier, error) {
	if clientset == nil {
		return nil, fmt.Errorf("kubernetes client is required for controller authorization convergence")
	}
	if transition == nil {
		return nil, fmt.Errorf("controller RBAC transition is required for authorization convergence")
	}
	probe, err := transition.PredecessorAuthorizationProbe()
	if err != nil {
		return nil, err
	}
	return newControllerRBACConvergenceBarrierWith(
		ctx,
		config,
		clientset.DiscoveryV1().EndpointSlices(kubernetesServiceNamespace),
		probe,
		newDirectAuthorizationReviewClient,
	)
}

func newControllerRBACConvergenceBarrierWith(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	probe crdupgrade.AuthorizationProbe,
	clientFactory authorizationReviewClientFactory,
) (*crdupgrade.RBACConvergenceBarrier, error) {
	if err := validateAuthorizationConvergenceInputs(ctx, config, endpointSlices, clientFactory); err != nil {
		return nil, err
	}
	return newAuthorizationConvergenceBarrier(
		ctx,
		config,
		endpointSlices,
		[]crdupgrade.AuthorizationProbe{probe},
		nil,
		clientFactory,
		"controller RBAC cutover",
	)
}

func validateAuthorizationConvergenceInputs(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	clientFactory authorizationReviewClientFactory,
) error {
	if ctx == nil {
		return fmt.Errorf("authorization convergence discovery context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := kubeapi.NewDefaultServiceProvider(config, endpointSlices, 1); err != nil {
		return err
	}
	if clientFactory == nil {
		return fmt.Errorf("authorization client factory is required")
	}
	return nil
}

func newAuthorizationConvergenceBarrier(
	ctx context.Context,
	config *rest.Config,
	endpointSlices endpointSliceLister,
	probes []crdupgrade.AuthorizationProbe,
	selfChecks []crdupgrade.AuthorizationCheck,
	clientFactory authorizationReviewClientFactory,
	description string,
) (*crdupgrade.RBACConvergenceBarrier, error) {
	sweepBurst := len(selfChecks)
	for _, probe := range probes {
		sweepBurst += len(probe.Checks)
	}
	apiEndpointProvider, err := newKubernetesAPIServerEndpointProvider(
		config,
		endpointSlices,
		sweepBurst,
	)
	if err != nil {
		return nil, err
	}
	initialSnapshot, err := kubeapi.WaitForInitialSnapshot(
		ctx,
		apiEndpointProvider,
		authorizationPollEvery,
	)
	if err != nil {
		return nil, err
	}
	seededProvider := apiEndpointProvider
	initialPending := true
	endpointProvider := newAuthorizationEndpointProvider(func(providerCtx context.Context) (kubernetesAPIServerEndpointSnapshot, error) {
		if providerCtx == nil {
			return kubernetesAPIServerEndpointSnapshot{}, fmt.Errorf("Kubernetes API endpoint discovery context is nil")
		}
		if err := providerCtx.Err(); err != nil {
			return kubernetesAPIServerEndpointSnapshot{}, err
		}
		if initialPending {
			initialPending = false
			return initialSnapshot, nil
		}
		return seededProvider(providerCtx)
	}, clientFactory)
	clients, err := endpointProvider(ctx)
	if err != nil {
		return nil, err
	}

	barrier := crdupgrade.NewRBACConvergenceBarrier(
		clients,
		probes,
		selfChecks,
		authorizationPollEvery,
		authorizationStabilityDuration,
	)
	barrier.RequestTimeout = authorizationRequestTimeout
	barrier.EndpointProvider = endpointProvider
	if err := barrier.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s authorization convergence barrier: %w", description, err)
	}
	return barrier, nil
}

func newKubernetesAPIServerEndpointProvider(
	config *rest.Config,
	endpointSlices endpointSliceLister,
	burst int,
) (kubernetesAPIServerEndpointProvider, error) {
	return kubeapi.NewDefaultServiceProvider(config, endpointSlices, burst)
}

func newAuthorizationEndpointProvider(
	apiEndpoints kubernetesAPIServerEndpointProvider,
	clientFactory authorizationReviewClientFactory,
) crdupgrade.AuthorizationEndpointProvider {
	clientsByAddress := make(map[string]crdupgrade.AuthorizationReviewClient)
	cachedInventoryIdentity := ""
	return func(ctx context.Context) ([]crdupgrade.NamedAuthorizationReviewClient, error) {
		if apiEndpoints == nil {
			return nil, fmt.Errorf("Kubernetes API endpoint provider is required")
		}
		if clientFactory == nil {
			return nil, fmt.Errorf("authorization client factory is required")
		}
		snapshot, err := apiEndpoints(ctx)
		if err != nil {
			return nil, err
		}
		if snapshot.InventoryIdentity == "" || snapshot.InventoryIdentity != strings.TrimSpace(snapshot.InventoryIdentity) {
			return nil, fmt.Errorf("Kubernetes API endpoint snapshot has an empty or padded inventory identity")
		}
		if len(snapshot.Endpoints) == 0 {
			return nil, fmt.Errorf("Kubernetes API endpoint snapshot is empty")
		}
		if snapshot.InventoryIdentity != cachedInventoryIdentity {
			clientsByAddress = make(map[string]crdupgrade.AuthorizationReviewClient)
			cachedInventoryIdentity = snapshot.InventoryIdentity
		}
		clients := make([]crdupgrade.NamedAuthorizationReviewClient, 0, len(snapshot.Endpoints))
		for _, endpoint := range snapshot.Endpoints {
			if strings.TrimSpace(endpoint.Address) == "" || endpoint.Address != strings.TrimSpace(endpoint.Address) {
				return nil, fmt.Errorf("Kubernetes API endpoint snapshot has an empty or padded address")
			}
			if endpoint.RESTConfig == nil {
				return nil, fmt.Errorf("Kubernetes API endpoint %q has a nil REST configuration", endpoint.Address)
			}
			client := clientsByAddress[endpoint.Address]
			if client == nil {
				client, err = clientFactory(endpoint.RESTConfig)
				if err != nil {
					return nil, fmt.Errorf("create authorization client for advertised API endpoint %q: %w", endpoint.Address, err)
				}
				if client == nil {
					return nil, fmt.Errorf("authorization client factory returned nil for advertised API endpoint %q", endpoint.Address)
				}
				clientsByAddress[endpoint.Address] = client
			}
			clients = append(clients, crdupgrade.NamedAuthorizationReviewClient{
				Name:             endpoint.Address,
				TopologyIdentity: snapshot.InventoryIdentity,
				Client:           client,
			})
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
		uid            string
	}
	identities := []serviceAccountIdentity{
		{name: "controller", serviceAccount: contract.ControllerServiceAccountName},
	}
	if rollout.PreviousControllerServiceAccountName != "" {
		if rollout.PreviousControllerServiceAccountUID == "" {
			return nil, fmt.Errorf("previous controller ServiceAccount UID is required for authorization convergence")
		}
		identities = append(identities, serviceAccountIdentity{
			name: "previous-controller", serviceAccount: rollout.PreviousControllerServiceAccountName,
			uid: string(rollout.PreviousControllerServiceAccountUID),
		})
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
		subjects = append(subjects, serviceAccountAuthorizationSubject(
			identity.name,
			rollout.ReleaseNamespace,
			identity.serviceAccount,
			identity.uid,
		))
	}
	return subjects, nil
}

func serviceAccountAuthorizationSubject(name, namespace, serviceAccount, uid string) crdupgrade.AuthorizationSubject {
	return crdupgrade.AuthorizationSubject{
		Name: name,
		User: "system:serviceaccount:" + namespace + ":" + serviceAccount,
		UID:  uid,
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
		"controller":          sets.controller,
		"previous-controller": sets.controller,
		"certificate":         sets.certificate,
		"hook-quiesce":        sets.hook,
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
	markerName := crdupgrade.AdmissionConvergenceMarkerName(
		rollout.ReleaseNamespace,
		rollout.ReleaseName,
		rollout.ReleaseSequence,
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
	markerTargets := teardownCheckHook | teardownCheckController
	if contract.CertificateRuntimeEnabled {
		markerTargets |= teardownCheckCertificate
	}
	appendResource(markerTargets, "update admission convergence marker ConfigMap", "", "v1", "configmaps", "", rollout.ReleaseNamespace, "update", markerName)
	if rollout.PreviousControllerReleaseSequence > 0 {
		previousMarkerName := crdupgrade.AdmissionConvergenceMarkerName(
			rollout.ReleaseNamespace,
			rollout.ReleaseName,
			rollout.PreviousControllerReleaseSequence,
		)
		appendResource(teardownCheckHook, "delete predecessor admission convergence marker ConfigMap", "", "v1", "configmaps", "", rollout.ReleaseNamespace, "delete", previousMarkerName)
	}
	appendResource(teardownCheckHook, "patch stable controller ClusterRoleBinding", "rbac.authorization.k8s.io", "v1", "clusterrolebindings", "", "", "patch", rollout.ControllerDeploymentName)
	appendResource(teardownCheckHook, "patch stable controller RoleBinding", "rbac.authorization.k8s.io", "v1", "rolebindings", "", rollout.CoordinationNamespace, "patch", rollout.ControllerDeploymentName)
	appendResource(teardownCheckHook, "patch runtime admission RoleBinding", "rbac.authorization.k8s.io", "v1", "rolebindings", "", rollout.ReleaseNamespace, "patch", rollout.ControllerDeploymentName+"-runtime-admission")
	appendResource(teardownCheckHook, "create SubjectAccessReview", "authorization.k8s.io", "v1", "subjectaccessreviews", "", "", "create", arbitraryObjectName)

	// Controller mutations. Every resource/subresource and mutating verb from
	// the installed controller roles gets a distinct probe; this does not rely
	// on one cached RBAC rule being observed atomically.
	appendResource(teardownCheckController, "patch PtahSchema", "operator.ptah.dev", "v1alpha1", "ptahschemas", "", rollout.ReleaseNamespace, "patch", arbitraryObjectName)
	if rollout.PreviousControllerServiceAccountName != "" {
		appendResource(teardownCheckController, "update PtahSchema legacy grant", "operator.ptah.dev", "v1alpha1", "ptahschemas", "", rollout.ReleaseNamespace, "update", arbitraryObjectName)
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
		certificateStagingSecretName, stagingErr := exactRuntimeArgument(rollout.CertificateArgs, "--staging-secret-name=")
		if stagingErr != nil {
			return teardownAuthorizationCheckSets{}, fmt.Errorf("certificate rotation staging Secret identity: %w", stagingErr)
		}
		if certificateStagingSecretName == rollout.WebhookSecretName {
			return teardownAuthorizationCheckSets{}, fmt.Errorf("certificate rotation staging and serving Secret identities must differ")
		}
		appendResource(teardownCheckCertificate, "update webhook Secret", "", "v1", "secrets", "", rollout.ReleaseNamespace, "update", rollout.WebhookSecretName)
		appendResource(teardownCheckCertificate, "update certificate staging Secret", "", "v1", "secrets", "", rollout.ReleaseNamespace, "update", certificateStagingSecretName)
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
