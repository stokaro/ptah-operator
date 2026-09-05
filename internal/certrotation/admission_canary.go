package certrotation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/kubeapi"
)

const (
	AdmissionCanaryMutatingWebhookName   = "certificate-rotation-canary-mutate.operator.ptah.dev"
	AdmissionCanaryValidatingWebhookName = "certificate-rotation-canary-validate.operator.ptah.dev"

	AdmissionCanaryMutatingPath   = "/candidate/mutate"
	AdmissionCanaryValidatingPath = "/candidate/validate"

	AdmissionCanaryMutatingFieldManager   = "ptah-certificate-rotation-canary-mutate-v1"
	AdmissionCanaryValidatingFieldManager = "ptah-certificate-rotation-canary-validate-v1"

	AdmissionCanaryMutatingDenialMessage   = "Ptah certificate rotation mutating canary confirmed candidate trust"
	AdmissionCanaryValidatingDenialMessage = "Ptah certificate rotation validating canary confirmed candidate trust"

	AdmissionCanaryMarkerLabel      = "operator.ptah.dev/certificate-rotation-canary"
	AdmissionCanaryMarkerLabelValue = "v1"

	admissionCanaryMutatingMatchConditionName   = "exact-certificate-rotation-mutating-canary"
	admissionCanaryValidatingMatchConditionName = "exact-certificate-rotation-validating-canary"
	admissionCanaryTimeoutSeconds               = int32(5)
	admissionCanaryDenialCauseField             = "request.options.fieldManager"
)

// AdmissionCanaryConfig identifies the two chart-owned admission singletons,
// their primary and candidate Services, and the immutable dry-run marker.
// Timing values apply to one combined, all-API-server stability barrier.
type AdmissionCanaryConfig struct {
	ReleaseName                    string
	MarkerNamespace                string
	MarkerName                     string
	ServiceAccountName             string
	MutatingWebhookConfiguration   string
	MutatingWebhookNames           []string
	ValidatingWebhookConfiguration string
	ValidatingWebhookNames         []string
	PrimaryServiceName             string
	CandidateServiceName           string
	ServiceNamespace               string
	StabilityDuration              time.Duration
	PollEvery                      time.Duration
	RequestTimeout                 time.Duration
}

type admissionCanaryProductionMode uint8

const (
	admissionCanaryProductionContains admissionCanaryProductionMode = iota + 1
	admissionCanaryProductionExact
)

// AdmissionCanaryDesiredState is an immutable publication target. Construct
// it with NewAdmissionCanaryExpansion or NewAdmissionCanaryContraction.
type AdmissionCanaryDesiredState struct {
	productionBundle []byte
	canaryBundle     []byte
	productionMode   admissionCanaryProductionMode
}

// NewAdmissionCanaryExpansion requires every production entry to retain both
// the old and new CA certificates while both canaries trust exactly the new CA.
func NewAdmissionCanaryExpansion(oldCA, newCA []byte) (AdmissionCanaryDesiredState, error) {
	newCanonical, err := combineCABundles(newCA)
	if err != nil {
		return AdmissionCanaryDesiredState{}, fmt.Errorf("build admission canary expansion candidate bundle: %w", err)
	}
	production := slices.Clone(newCanonical)
	if len(oldCA) != 0 {
		oldCanonical, err := combineCABundles(oldCA)
		if err != nil {
			return AdmissionCanaryDesiredState{}, fmt.Errorf("build admission canary expansion old production bundle: %w", err)
		}
		disjoint, err := certificateBundlesDisjoint(oldCanonical, newCanonical)
		if err != nil {
			return AdmissionCanaryDesiredState{}, fmt.Errorf("verify admission canary expansion CA independence: %w", err)
		}
		if !disjoint {
			return AdmissionCanaryDesiredState{}, errors.New("admission canary expansion requires distinct old and new CA bundles")
		}
		production, err = combineCABundles(oldCanonical, newCanonical)
		if err != nil {
			return AdmissionCanaryDesiredState{}, fmt.Errorf("build admission canary expansion production bundle: %w", err)
		}
	}
	return AdmissionCanaryDesiredState{
		productionBundle: production,
		canaryBundle:     newCanonical,
		productionMode:   admissionCanaryProductionContains,
	}, nil
}

// NewAdmissionCanaryContraction requires every production entry to trust
// exactly the new CA while both canaries trust exactly an independent proof CA.
func NewAdmissionCanaryContraction(newCA, proofCA []byte) (AdmissionCanaryDesiredState, error) {
	production, err := combineCABundles(newCA)
	if err != nil {
		return AdmissionCanaryDesiredState{}, fmt.Errorf("build admission canary contraction production bundle: %w", err)
	}
	canary, err := combineCABundles(proofCA)
	if err != nil {
		return AdmissionCanaryDesiredState{}, fmt.Errorf("build admission canary contraction proof bundle: %w", err)
	}
	disjoint, err := certificateBundlesDisjoint(production, canary)
	if err != nil {
		return AdmissionCanaryDesiredState{}, fmt.Errorf("verify admission canary contraction CA independence: %w", err)
	}
	if !disjoint {
		return AdmissionCanaryDesiredState{}, errors.New("admission canary contraction proof CA must be independent from the production CA")
	}
	return AdmissionCanaryDesiredState{
		productionBundle: production,
		canaryBundle:     canary,
		productionMode:   admissionCanaryProductionExact,
	}, nil
}

// NewAdmissionCanaryParked returns the steady-state contract used after the
// independent contraction proof: production and canary entries both trust
// exactly the active production CA.
func NewAdmissionCanaryParked(ca []byte) (AdmissionCanaryDesiredState, error) {
	canonical, err := combineCABundles(ca)
	if err != nil {
		return AdmissionCanaryDesiredState{}, fmt.Errorf("build parked admission canary CA bundle: %w", err)
	}
	return AdmissionCanaryDesiredState{
		productionBundle: slices.Clone(canonical),
		canaryBundle:     slices.Clone(canonical),
		productionMode:   admissionCanaryProductionExact,
	}, nil
}

func certificateBundlesDisjoint(left, right []byte) (bool, error) {
	leftCertificates, err := parseCertificateBundle(left)
	if err != nil {
		return false, err
	}
	rightCertificates, err := parseCertificateBundle(right)
	if err != nil {
		return false, err
	}
	leftDigests := make(map[[sha256.Size]byte]struct{}, len(leftCertificates))
	for _, certificate := range leftCertificates {
		leftDigests[sha256.Sum256(certificate.RawSubjectPublicKeyInfo)] = struct{}{}
	}
	for _, certificate := range rightCertificates {
		if _, overlaps := leftDigests[sha256.Sum256(certificate.RawSubjectPublicKeyInfo)]; overlaps {
			return false, nil
		}
	}
	return true, nil
}

// AdmissionCanaryMarker returns the static, non-secret marker shape installed
// by the chart. API-server populated identity fields are intentionally empty.
func AdmissionCanaryMarker(namespace, name, releaseName string) *corev1.ConfigMap {
	immutable := true
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				AdmissionCanaryMarkerLabel: AdmissionCanaryMarkerLabelValue,
				HelmManagedByLabel:         HelmManagedByLabelValue,
			},
			Annotations: map[string]string{
				HelmReleaseNameAnnotation:      releaseName,
				HelmReleaseNamespaceAnnotation: namespace,
			},
		},
		Immutable: &immutable,
	}
}

// AdmissionCanaryMutatingDenialStatus is the exact status the candidate
// mutating listener returns after validating its dry-run request contract.
func AdmissionCanaryMutatingDenialStatus(markerName string, markerUID types.UID) *metav1.Status {
	return admissionCanaryDenialStatus(
		AdmissionCanaryMutatingDenialMessage,
		AdmissionCanaryMutatingFieldManager,
		markerName,
		markerUID,
	)
}

// AdmissionCanaryValidatingDenialStatus is the exact status the candidate
// validating listener returns after validating its dry-run request contract.
func AdmissionCanaryValidatingDenialStatus(markerName string, markerUID types.UID) *metav1.Status {
	return admissionCanaryDenialStatus(
		AdmissionCanaryValidatingDenialMessage,
		AdmissionCanaryValidatingFieldManager,
		markerName,
		markerUID,
	)
}

func admissionCanaryDenialStatus(message, fieldManager, markerName string, markerUID types.UID) *metav1.Status {
	return &metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Status"},
		Status:   metav1.StatusFailure,
		Message:  message,
		Reason:   metav1.StatusReasonInvalid,
		Details: &metav1.StatusDetails{
			Name: markerName,
			Kind: "ConfigMap",
			UID:  markerUID,
			Causes: []metav1.StatusCause{{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Message: fieldManager,
				Field:   admissionCanaryDenialCauseField,
			}},
		},
		Code: http.StatusUnprocessableEntity,
	}
}

// HasExactAdmissionCanaryMutatingDenial recognizes only the supported
// API-server envelope for the exact mutating canary response.
func HasExactAdmissionCanaryMutatingDenial(err error, markerName string, markerUID types.UID) bool {
	return hasExactAdmissionCanaryDenial(
		err,
		AdmissionCanaryMutatingWebhookName,
		AdmissionCanaryMutatingDenialStatus(markerName, markerUID),
	)
}

// HasExactAdmissionCanaryValidatingDenial recognizes only the supported
// API-server envelope for the exact validating canary response.
func HasExactAdmissionCanaryValidatingDenial(err error, markerName string, markerUID types.UID) bool {
	return hasExactAdmissionCanaryDenial(
		err,
		AdmissionCanaryValidatingWebhookName,
		AdmissionCanaryValidatingDenialStatus(markerName, markerUID),
	)
}

func hasExactAdmissionCanaryDenial(err error, webhookName string, response *metav1.Status) bool {
	var statusError apierrors.APIStatus
	if !errors.As(err, &statusError) {
		return false
	}
	status := statusError.Status()
	wantMessage := fmt.Sprintf("admission webhook %q denied the request: %s", webhookName, response.Message)
	if status.Status != response.Status || status.Message != wantMessage || status.Reason != response.Reason ||
		status.Code != response.Code || !reflect.DeepEqual(status.Details, response.Details) ||
		!reflect.DeepEqual(status.ListMeta, metav1.ListMeta{}) {
		return false
	}
	return (status.APIVersion == "" && status.Kind == "") ||
		(status.APIVersion == corev1.SchemeGroupVersion.String() && status.Kind == "Status")
}

// AdmissionCanary publishes canary entries and proves their convergence
// through every directly addressed API server.
type AdmissionCanary struct {
	client              kubernetes.Interface
	provider            kubeapi.Provider
	config              AdmissionCanaryConfig
	directClientFactory admissionCanaryDirectClientFactory
}

// NewAdmissionCanary validates and freezes the complete canary contract.
func NewAdmissionCanary(client kubernetes.Interface, provider kubeapi.Provider, config AdmissionCanaryConfig) (*AdmissionCanary, error) {
	if nilDependency(client) || nilDependency(provider) {
		return nil, errors.New("admission canary Kubernetes client and API-server provider are required")
	}
	config.MutatingWebhookNames = slices.Clone(config.MutatingWebhookNames)
	config.ValidatingWebhookNames = slices.Clone(config.ValidatingWebhookNames)
	if err := validateAdmissionCanaryConfig(config); err != nil {
		return nil, err
	}
	return &AdmissionCanary{
		client:              client,
		provider:            provider,
		config:              config,
		directClientFactory: newAdmissionCanaryDirectClient,
	}, nil
}

// PublishMutating idempotently publishes the complete mutating singleton and
// returns only after an exact update response and exact storage readback.
func (c *AdmissionCanary) PublishMutating(ctx context.Context, desired AdmissionCanaryDesiredState) error {
	if err := c.validateOperation(ctx, desired); err != nil {
		return err
	}
	client := c.client.AdmissionregistrationV1().MutatingWebhookConfigurations()
	current, err := client.Get(ctx, c.config.MutatingWebhookConfiguration, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get admission canary MutatingWebhookConfiguration: %w", err)
	}
	candidate, err := c.buildMutatingCandidate(current, desired)
	if err != nil {
		return err
	}
	if reflect.DeepEqual(current.Webhooks, candidate.Webhooks) {
		return c.verifyMutatingPublication(current, desired)
	}
	updated, err := client.Update(ctx, candidate, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update admission canary MutatingWebhookConfiguration: %w", err)
	}
	if err := verifyMutatingUpdateResponse(candidate, updated); err != nil {
		return err
	}
	readback, err := client.Get(ctx, c.config.MutatingWebhookConfiguration, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read back admission canary MutatingWebhookConfiguration: %w", err)
	}
	if err := verifyMutatingReadback(updated, readback); err != nil {
		return err
	}
	return c.verifyMutatingPublication(readback, desired)
}

// PublishValidating idempotently publishes the complete validating singleton
// and returns only after an exact update response and exact storage readback.
func (c *AdmissionCanary) PublishValidating(ctx context.Context, desired AdmissionCanaryDesiredState) error {
	if err := c.validateOperation(ctx, desired); err != nil {
		return err
	}
	client := c.client.AdmissionregistrationV1().ValidatingWebhookConfigurations()
	current, err := client.Get(ctx, c.config.ValidatingWebhookConfiguration, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get admission canary ValidatingWebhookConfiguration: %w", err)
	}
	candidate, err := c.buildValidatingCandidate(current, desired)
	if err != nil {
		return err
	}
	if reflect.DeepEqual(current.Webhooks, candidate.Webhooks) {
		return c.verifyValidatingPublication(current, desired)
	}
	updated, err := client.Update(ctx, candidate, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update admission canary ValidatingWebhookConfiguration: %w", err)
	}
	if err := verifyValidatingUpdateResponse(candidate, updated); err != nil {
		return err
	}
	readback, err := client.Get(ctx, c.config.ValidatingWebhookConfiguration, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read back admission canary ValidatingWebhookConfiguration: %w", err)
	}
	if err := verifyValidatingReadback(updated, readback); err != nil {
		return err
	}
	return c.verifyValidatingPublication(readback, desired)
}

// Wait re-reads both desired singleton contracts and the immutable marker,
// then closes one combined stability barrier. It cannot prove either singleton
// in isolation.
func (c *AdmissionCanary) Wait(ctx context.Context, desired AdmissionCanaryDesiredState) error {
	if err := c.validateOperation(ctx, desired); err != nil {
		return err
	}
	publication, err := c.capturePublication(ctx, desired)
	if err != nil {
		return err
	}
	barrier := &kubeapi.StabilityBarrier{
		Provider:          c.provider,
		Probe:             c.endpointProbe(publication.marker),
		StoredContract:    c.storedContractProbe(publication),
		StabilityDuration: c.config.StabilityDuration,
		PollEvery:         c.config.PollEvery,
		RequestTimeout:    c.config.RequestTimeout,
	}
	if err := barrier.Wait(ctx); err != nil {
		return fmt.Errorf("wait for admission canary API-server convergence: %w", err)
	}
	return nil
}

// Converge is a convenience wrapper. Crash-aware callers should invoke the
// two publication methods separately and durably record each completed phase
// before calling Wait.
func (c *AdmissionCanary) Converge(ctx context.Context, desired AdmissionCanaryDesiredState) error {
	if err := c.PublishMutating(ctx, desired); err != nil {
		return err
	}
	if err := c.PublishValidating(ctx, desired); err != nil {
		return err
	}
	return c.Wait(ctx, desired)
}

func (c *AdmissionCanary) validateOperation(ctx context.Context, desired AdmissionCanaryDesiredState) error {
	if c == nil || c.client == nil || c.provider == nil || c.directClientFactory == nil {
		return errors.New("admission canary is not initialized")
	}
	if ctx == nil {
		return errors.New("admission canary context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return validateAdmissionCanaryDesiredState(desired)
}

func validateAdmissionCanaryDesiredState(desired AdmissionCanaryDesiredState) error {
	if desired.productionMode != admissionCanaryProductionContains && desired.productionMode != admissionCanaryProductionExact {
		return errors.New("admission canary desired production mode is invalid")
	}
	if _, err := parseCertificateBundle(desired.productionBundle); err != nil {
		return fmt.Errorf("admission canary desired production bundle: %w", err)
	}
	if _, err := parseCertificateBundle(desired.canaryBundle); err != nil {
		return fmt.Errorf("admission canary desired candidate bundle: %w", err)
	}
	return nil
}

func validateAdmissionCanaryConfig(config AdmissionCanaryConfig) error {
	for label, value := range map[string]string{
		"release name":                        config.ReleaseName,
		"marker name":                         config.MarkerName,
		"MutatingWebhookConfiguration name":   config.MutatingWebhookConfiguration,
		"ValidatingWebhookConfiguration name": config.ValidatingWebhookConfiguration,
	} {
		if problems := validation.IsDNS1123Subdomain(value); len(problems) != 0 {
			return fmt.Errorf("admission canary %s is invalid: %s", label, problems[0])
		}
	}
	for label, value := range map[string]string{
		"marker namespace":       config.MarkerNamespace,
		"ServiceAccount name":    config.ServiceAccountName,
		"primary Service name":   config.PrimaryServiceName,
		"candidate Service name": config.CandidateServiceName,
		"Service namespace":      config.ServiceNamespace,
	} {
		if problems := validation.IsDNS1123Label(value); len(problems) != 0 {
			return fmt.Errorf("admission canary %s is invalid: %s", label, problems[0])
		}
	}
	if config.PrimaryServiceName == config.CandidateServiceName {
		return errors.New("admission canary candidate Service must differ from the primary Service")
	}
	if err := validateWebhookNames("mutating", config.MutatingWebhookNames); err != nil {
		return err
	}
	if err := validateWebhookNames("validating", config.ValidatingWebhookNames); err != nil {
		return err
	}
	if slices.Contains(config.MutatingWebhookNames, AdmissionCanaryMutatingWebhookName) ||
		slices.Contains(config.ValidatingWebhookNames, AdmissionCanaryValidatingWebhookName) {
		return errors.New("admission canary webhook names must not be production webhook names")
	}
	if config.StabilityDuration <= 0 || config.PollEvery <= 0 || config.RequestTimeout <= 0 {
		return errors.New("admission canary stability timing values must be positive")
	}
	return nil
}

func (c *AdmissionCanary) rotatorConfig() Config {
	return Config{
		MutatingWebhookNames:   slices.Clone(c.config.MutatingWebhookNames),
		ValidatingWebhookNames: slices.Clone(c.config.ValidatingWebhookNames),
		ServiceName:            c.config.PrimaryServiceName,
		ServiceNamespace:       c.config.ServiceNamespace,
	}
}

func (c *AdmissionCanary) buildMutatingCandidate(
	current *admissionregistrationv1.MutatingWebhookConfiguration,
	desired AdmissionCanaryDesiredState,
) (*admissionregistrationv1.MutatingWebhookConfiguration, error) {
	if current == nil {
		return nil, errors.New("admission canary MutatingWebhookConfiguration GET returned nil")
	}
	if err := verifyWebhookConfigurationIdentity("MutatingWebhookConfiguration", c.config.MutatingWebhookConfiguration, objectMeta(current)); err != nil {
		return nil, err
	}
	candidate := current.DeepCopy()
	production, err := managedMutatingWebhooks(candidate.Webhooks, c.rotatorConfig())
	if err != nil {
		return nil, fmt.Errorf("inspect admission canary production mutating webhooks: %w", err)
	}
	for _, webhook := range production {
		bundle, err := desired.productionBundleFor(webhook.ClientConfig.CABundle)
		if err != nil {
			return nil, fmt.Errorf("build production mutating webhook %q CA bundle: %w", webhook.Name, err)
		}
		webhook.ClientConfig.CABundle = bundle
	}
	want := c.mutatingWebhook(desired.canaryBundle)
	if err := replaceOrAppendMutatingCanary(&candidate.Webhooks, want, c.config); err != nil {
		return nil, err
	}
	return candidate, nil
}

func (c *AdmissionCanary) buildValidatingCandidate(
	current *admissionregistrationv1.ValidatingWebhookConfiguration,
	desired AdmissionCanaryDesiredState,
) (*admissionregistrationv1.ValidatingWebhookConfiguration, error) {
	if current == nil {
		return nil, errors.New("admission canary ValidatingWebhookConfiguration GET returned nil")
	}
	if err := verifyWebhookConfigurationIdentity("ValidatingWebhookConfiguration", c.config.ValidatingWebhookConfiguration, objectMeta(current)); err != nil {
		return nil, err
	}
	candidate := current.DeepCopy()
	production, err := managedValidatingWebhooks(candidate.Webhooks, c.rotatorConfig())
	if err != nil {
		return nil, fmt.Errorf("inspect admission canary production validating webhooks: %w", err)
	}
	for _, webhook := range production {
		bundle, err := desired.productionBundleFor(webhook.ClientConfig.CABundle)
		if err != nil {
			return nil, fmt.Errorf("build production validating webhook %q CA bundle: %w", webhook.Name, err)
		}
		webhook.ClientConfig.CABundle = bundle
	}
	want := c.validatingWebhook(desired.canaryBundle)
	if err := replaceOrAppendValidatingCanary(&candidate.Webhooks, want, c.config); err != nil {
		return nil, err
	}
	return candidate, nil
}

func (desired AdmissionCanaryDesiredState) productionBundleFor(existing []byte) ([]byte, error) {
	if desired.productionMode == admissionCanaryProductionExact {
		return slices.Clone(desired.productionBundle), nil
	}
	return perEntryTransitionBundle(existing, desired.productionBundle)
}

func replaceOrAppendMutatingCanary(
	webhooks *[]admissionregistrationv1.MutatingWebhook,
	want admissionregistrationv1.MutatingWebhook,
	config AdmissionCanaryConfig,
) error {
	found := -1
	for index := range *webhooks {
		webhook := &(*webhooks)[index]
		if webhook.Name == AdmissionCanaryMutatingWebhookName {
			if found >= 0 {
				return errors.New("mutating admission canary appears more than once")
			}
			if !sameMutatingCanaryStaticContract(*webhook, want) {
				return errors.New("existing mutating admission canary differs from the static contract")
			}
			found = index
			continue
		}
		if webhookTargetsCandidateService(webhook.ClientConfig, config) {
			return fmt.Errorf("foreign mutating webhook %q targets the dedicated candidate Service", webhook.Name)
		}
	}
	if found < 0 {
		*webhooks = append(*webhooks, want)
		return nil
	}
	(*webhooks)[found] = want
	return nil
}

func replaceOrAppendValidatingCanary(
	webhooks *[]admissionregistrationv1.ValidatingWebhook,
	want admissionregistrationv1.ValidatingWebhook,
	config AdmissionCanaryConfig,
) error {
	found := -1
	for index := range *webhooks {
		webhook := &(*webhooks)[index]
		if webhook.Name == AdmissionCanaryValidatingWebhookName {
			if found >= 0 {
				return errors.New("validating admission canary appears more than once")
			}
			if !sameValidatingCanaryStaticContract(*webhook, want) {
				return errors.New("existing validating admission canary differs from the static contract")
			}
			found = index
			continue
		}
		if webhookTargetsCandidateService(webhook.ClientConfig, config) {
			return fmt.Errorf("foreign validating webhook %q targets the dedicated candidate Service", webhook.Name)
		}
	}
	if found < 0 {
		*webhooks = append(*webhooks, want)
		return nil
	}
	(*webhooks)[found] = want
	return nil
}

func sameMutatingCanaryStaticContract(got, want admissionregistrationv1.MutatingWebhook) bool {
	got.ClientConfig.CABundle = nil
	want.ClientConfig.CABundle = nil
	return reflect.DeepEqual(got, want)
}

func sameValidatingCanaryStaticContract(got, want admissionregistrationv1.ValidatingWebhook) bool {
	got.ClientConfig.CABundle = nil
	want.ClientConfig.CABundle = nil
	return reflect.DeepEqual(got, want)
}

func webhookTargetsCandidateService(client admissionregistrationv1.WebhookClientConfig, config AdmissionCanaryConfig) bool {
	return client.Service != nil && client.Service.Name == config.CandidateServiceName &&
		client.Service.Namespace == config.ServiceNamespace
}

func (c *AdmissionCanary) mutatingWebhook(bundle []byte) admissionregistrationv1.MutatingWebhook {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	none := admissionregistrationv1.SideEffectClassNone
	never := admissionregistrationv1.NeverReinvocationPolicy
	return admissionregistrationv1.MutatingWebhook{
		Name: AdmissionCanaryMutatingWebhookName,
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			Service: &admissionregistrationv1.ServiceReference{
				Namespace: c.config.ServiceNamespace,
				Name:      c.config.CandidateServiceName,
				Path:      admissionCanaryStringPointer(AdmissionCanaryMutatingPath),
				Port:      admissionCanaryInt32Pointer(webhookServicePort),
			},
			CABundle: slices.Clone(bundle),
		},
		Rules:                   []admissionregistrationv1.RuleWithOperations{admissionCanaryRule()},
		FailurePolicy:           &fail,
		MatchPolicy:             &exact,
		NamespaceSelector:       admissionCanaryNamespaceSelector(c.config.MarkerNamespace),
		ObjectSelector:          admissionCanaryObjectSelector(),
		SideEffects:             &none,
		TimeoutSeconds:          admissionCanaryInt32Pointer(admissionCanaryTimeoutSeconds),
		AdmissionReviewVersions: []string{"v1"},
		ReinvocationPolicy:      &never,
		MatchConditions: []admissionregistrationv1.MatchCondition{{
			Name:       admissionCanaryMutatingMatchConditionName,
			Expression: c.matchExpression(AdmissionCanaryMutatingFieldManager),
		}},
	}
}

func (c *AdmissionCanary) validatingWebhook(bundle []byte) admissionregistrationv1.ValidatingWebhook {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	none := admissionregistrationv1.SideEffectClassNone
	return admissionregistrationv1.ValidatingWebhook{
		Name: AdmissionCanaryValidatingWebhookName,
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			Service: &admissionregistrationv1.ServiceReference{
				Namespace: c.config.ServiceNamespace,
				Name:      c.config.CandidateServiceName,
				Path:      admissionCanaryStringPointer(AdmissionCanaryValidatingPath),
				Port:      admissionCanaryInt32Pointer(webhookServicePort),
			},
			CABundle: slices.Clone(bundle),
		},
		Rules:                   []admissionregistrationv1.RuleWithOperations{admissionCanaryRule()},
		FailurePolicy:           &fail,
		MatchPolicy:             &exact,
		NamespaceSelector:       admissionCanaryNamespaceSelector(c.config.MarkerNamespace),
		ObjectSelector:          admissionCanaryObjectSelector(),
		SideEffects:             &none,
		TimeoutSeconds:          admissionCanaryInt32Pointer(admissionCanaryTimeoutSeconds),
		AdmissionReviewVersions: []string{"v1"},
		MatchConditions: []admissionregistrationv1.MatchCondition{{
			Name:       admissionCanaryValidatingMatchConditionName,
			Expression: c.matchExpression(AdmissionCanaryValidatingFieldManager),
		}},
	}
}

func admissionCanaryRule() admissionregistrationv1.RuleWithOperations {
	scope := admissionregistrationv1.NamespacedScope
	return admissionregistrationv1.RuleWithOperations{
		Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
		Rule: admissionregistrationv1.Rule{
			APIGroups:   []string{""},
			APIVersions: []string{"v1"},
			Resources:   []string{"configmaps"},
			Scope:       &scope,
		},
	}
}

func admissionCanaryNamespaceSelector(namespace string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: namespace}}
}

func admissionCanaryObjectSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{AdmissionCanaryMarkerLabel: AdmissionCanaryMarkerLabelValue}}
}

func (c *AdmissionCanary) matchExpression(fieldManager string) string {
	username := "system:serviceaccount:" + c.config.MarkerNamespace + ":" + c.config.ServiceAccountName
	return fmt.Sprintf(
		`request.operation == "UPDATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "configmaps" && (!has(request.subResource) || request.subResource == "") && request.namespace == %q && request.name == %q && request.userInfo.username == %q && request.dryRun == true && has(request.options) && has(request.options.fieldManager) && request.options.fieldManager == %q && has(request.options.fieldValidation) && request.options.fieldValidation == "Strict"`,
		c.config.MarkerNamespace,
		c.config.MarkerName,
		username,
		fieldManager,
	)
}

func (c *AdmissionCanary) verifyMutatingPublication(
	configuration *admissionregistrationv1.MutatingWebhookConfiguration,
	desired AdmissionCanaryDesiredState,
) error {
	if err := verifyWebhookConfigurationIdentity("MutatingWebhookConfiguration", c.config.MutatingWebhookConfiguration, objectMeta(configuration)); err != nil {
		return err
	}
	production, err := managedMutatingWebhooks(configuration.Webhooks, c.rotatorConfig())
	if err != nil {
		return fmt.Errorf("verify admission canary production mutating webhooks: %w", err)
	}
	for _, webhook := range production {
		if !desired.productionMatches(webhook.ClientConfig.CABundle) {
			return fmt.Errorf("production mutating webhook %q does not have the desired CA contract", webhook.Name)
		}
	}
	want := c.mutatingWebhook(desired.canaryBundle)
	count := 0
	for _, webhook := range configuration.Webhooks {
		if webhook.Name == AdmissionCanaryMutatingWebhookName {
			count++
			if !reflect.DeepEqual(webhook, want) {
				return errors.New("mutating admission canary does not equal the desired contract")
			}
		} else if webhookTargetsCandidateService(webhook.ClientConfig, c.config) {
			return fmt.Errorf("foreign mutating webhook %q targets the dedicated candidate Service", webhook.Name)
		}
	}
	if count != 1 {
		return fmt.Errorf("mutating admission canary count is %d, want exactly 1", count)
	}
	return nil
}

func (c *AdmissionCanary) verifyValidatingPublication(
	configuration *admissionregistrationv1.ValidatingWebhookConfiguration,
	desired AdmissionCanaryDesiredState,
) error {
	if err := verifyWebhookConfigurationIdentity("ValidatingWebhookConfiguration", c.config.ValidatingWebhookConfiguration, objectMeta(configuration)); err != nil {
		return err
	}
	production, err := managedValidatingWebhooks(configuration.Webhooks, c.rotatorConfig())
	if err != nil {
		return fmt.Errorf("verify admission canary production validating webhooks: %w", err)
	}
	for _, webhook := range production {
		if !desired.productionMatches(webhook.ClientConfig.CABundle) {
			return fmt.Errorf("production validating webhook %q does not have the desired CA contract", webhook.Name)
		}
	}
	want := c.validatingWebhook(desired.canaryBundle)
	count := 0
	for _, webhook := range configuration.Webhooks {
		if webhook.Name == AdmissionCanaryValidatingWebhookName {
			count++
			if !reflect.DeepEqual(webhook, want) {
				return errors.New("validating admission canary does not equal the desired contract")
			}
		} else if webhookTargetsCandidateService(webhook.ClientConfig, c.config) {
			return fmt.Errorf("foreign validating webhook %q targets the dedicated candidate Service", webhook.Name)
		}
	}
	if count != 1 {
		return fmt.Errorf("validating admission canary count is %d, want exactly 1", count)
	}
	return nil
}

func (desired AdmissionCanaryDesiredState) productionMatches(bundle []byte) bool {
	if desired.productionMode == admissionCanaryProductionExact {
		return bytesEqual(bundle, desired.productionBundle)
	}
	return caBundleContainsAllCertificates(bundle, desired.productionBundle)
}

func bytesEqual(left, right []byte) bool {
	return slices.Equal(left, right)
}

func verifyWebhookConfigurationIdentity(kind, name string, metadata *metav1.ObjectMeta) error {
	if metadata == nil || metadata.Name != name || metadata.Namespace != "" || metadata.UID == "" ||
		metadata.ResourceVersion == "" || metadata.ResourceVersion != strings.TrimSpace(metadata.ResourceVersion) ||
		metadata.DeletionTimestamp != nil || metadata.DeletionGracePeriodSeconds != nil {
		return fmt.Errorf("admission canary %s/%s has an incomplete or terminating live identity", kind, name)
	}
	return nil
}

func objectMeta(object metav1.Object) *metav1.ObjectMeta {
	if object == nil {
		return nil
	}
	return &metav1.ObjectMeta{
		Name:                       object.GetName(),
		Namespace:                  object.GetNamespace(),
		UID:                        object.GetUID(),
		ResourceVersion:            object.GetResourceVersion(),
		DeletionTimestamp:          object.GetDeletionTimestamp(),
		DeletionGracePeriodSeconds: object.GetDeletionGracePeriodSeconds(),
	}
}

func verifyMutatingUpdateResponse(
	want *admissionregistrationv1.MutatingWebhookConfiguration,
	got *admissionregistrationv1.MutatingWebhookConfiguration,
) error {
	if got == nil || got.Name != want.Name || got.UID != want.UID || got.ResourceVersion == "" ||
		got.ResourceVersion != strings.TrimSpace(got.ResourceVersion) ||
		!reflect.DeepEqual(normalizedMutatingConfiguration(got), normalizedMutatingConfiguration(want)) {
		return errors.New("admission canary MutatingWebhookConfiguration update returned an inexact response")
	}
	return nil
}

func verifyValidatingUpdateResponse(
	want *admissionregistrationv1.ValidatingWebhookConfiguration,
	got *admissionregistrationv1.ValidatingWebhookConfiguration,
) error {
	if got == nil || got.Name != want.Name || got.UID != want.UID || got.ResourceVersion == "" ||
		got.ResourceVersion != strings.TrimSpace(got.ResourceVersion) ||
		!reflect.DeepEqual(normalizedValidatingConfiguration(got), normalizedValidatingConfiguration(want)) {
		return errors.New("admission canary ValidatingWebhookConfiguration update returned an inexact response")
	}
	return nil
}

func verifyMutatingReadback(
	want *admissionregistrationv1.MutatingWebhookConfiguration,
	got *admissionregistrationv1.MutatingWebhookConfiguration,
) error {
	if got == nil || got.Name != want.Name || got.UID != want.UID || got.ResourceVersion != want.ResourceVersion ||
		!reflect.DeepEqual(normalizedMutatingConfiguration(got), normalizedMutatingConfiguration(want)) {
		return errors.New("admission canary MutatingWebhookConfiguration readback differs from the update response")
	}
	return nil
}

func verifyValidatingReadback(
	want *admissionregistrationv1.ValidatingWebhookConfiguration,
	got *admissionregistrationv1.ValidatingWebhookConfiguration,
) error {
	if got == nil || got.Name != want.Name || got.UID != want.UID || got.ResourceVersion != want.ResourceVersion ||
		!reflect.DeepEqual(normalizedValidatingConfiguration(got), normalizedValidatingConfiguration(want)) {
		return errors.New("admission canary ValidatingWebhookConfiguration readback differs from the update response")
	}
	return nil
}

type admissionCanaryPublication struct {
	mutating   *admissionregistrationv1.MutatingWebhookConfiguration
	validating *admissionregistrationv1.ValidatingWebhookConfiguration
	marker     *corev1.ConfigMap
	identity   string
}

func (c *AdmissionCanary) capturePublication(
	ctx context.Context,
	desired AdmissionCanaryDesiredState,
) (admissionCanaryPublication, error) {
	mutating, err := c.client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(
		ctx,
		c.config.MutatingWebhookConfiguration,
		metav1.GetOptions{},
	)
	if err != nil {
		return admissionCanaryPublication{}, fmt.Errorf("get stored admission canary MutatingWebhookConfiguration: %w", err)
	}
	if err := c.verifyMutatingPublication(mutating, desired); err != nil {
		return admissionCanaryPublication{}, err
	}
	validating, err := c.client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		ctx,
		c.config.ValidatingWebhookConfiguration,
		metav1.GetOptions{},
	)
	if err != nil {
		return admissionCanaryPublication{}, fmt.Errorf("get stored admission canary ValidatingWebhookConfiguration: %w", err)
	}
	if err := c.verifyValidatingPublication(validating, desired); err != nil {
		return admissionCanaryPublication{}, err
	}
	marker, err := c.client.CoreV1().ConfigMaps(c.config.MarkerNamespace).Get(ctx, c.config.MarkerName, metav1.GetOptions{})
	if err != nil {
		return admissionCanaryPublication{}, fmt.Errorf("get stored admission canary marker: %w", err)
	}
	if err := verifyAdmissionCanaryMarker(marker, c.config.MarkerNamespace, c.config.MarkerName, c.config.ReleaseName); err != nil {
		return admissionCanaryPublication{}, err
	}
	publication := admissionCanaryPublication{
		mutating:   mutating.DeepCopy(),
		validating: validating.DeepCopy(),
		marker:     marker.DeepCopy(),
	}
	publication.identity, err = admissionCanaryPublicationIdentity(publication)
	if err != nil {
		return admissionCanaryPublication{}, err
	}
	return publication, nil
}

func (c *AdmissionCanary) storedContractProbe(publication admissionCanaryPublication) kubeapi.StoredContractProbe {
	return func(ctx context.Context) (string, bool, error) {
		mutating, err := c.client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(
			ctx,
			c.config.MutatingWebhookConfiguration,
			metav1.GetOptions{},
		)
		if err != nil {
			return storedObservationError(ctx, publication.identity, "get mutating admission canary stored contract", err)
		}
		validating, err := c.client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
			ctx,
			c.config.ValidatingWebhookConfiguration,
			metav1.GetOptions{},
		)
		if err != nil {
			return storedObservationError(ctx, publication.identity, "get validating admission canary stored contract", err)
		}
		marker, err := c.client.CoreV1().ConfigMaps(c.config.MarkerNamespace).Get(ctx, c.config.MarkerName, metav1.GetOptions{})
		if err != nil {
			return storedObservationError(ctx, publication.identity, "get admission canary marker stored contract", err)
		}
		if !sameMutatingPublication(mutating, publication.mutating) ||
			!sameValidatingPublication(validating, publication.validating) ||
			!sameAdmissionCanaryMarker(marker, publication.marker, c.config.ReleaseName) {
			return publication.identity, false, nil
		}
		return publication.identity, true, nil
	}
}

func storedObservationError(ctx context.Context, identity, action string, err error) (string, bool, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return identity, false, contextErr
	}
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) || apierrors.IsTimeout(err) ||
		apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) {
		return identity, false, nil
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return identity, false, nil
	}
	var statusError apierrors.APIStatus
	if errors.As(err, &statusError) && statusError.Status().Code >= http.StatusInternalServerError {
		return identity, false, nil
	}
	return identity, false, fmt.Errorf("%s: %w", action, err)
}

func sameMutatingPublication(got, want *admissionregistrationv1.MutatingWebhookConfiguration) bool {
	return got != nil && want != nil && got.Name == want.Name && got.UID == want.UID &&
		got.ResourceVersion == want.ResourceVersion &&
		reflect.DeepEqual(normalizedMutatingConfiguration(got), normalizedMutatingConfiguration(want))
}

func sameValidatingPublication(got, want *admissionregistrationv1.ValidatingWebhookConfiguration) bool {
	return got != nil && want != nil && got.Name == want.Name && got.UID == want.UID &&
		got.ResourceVersion == want.ResourceVersion &&
		reflect.DeepEqual(normalizedValidatingConfiguration(got), normalizedValidatingConfiguration(want))
}

func normalizedMutatingConfiguration(
	configuration *admissionregistrationv1.MutatingWebhookConfiguration,
) *admissionregistrationv1.MutatingWebhookConfiguration {
	if configuration == nil {
		return nil
	}
	normalized := configuration.DeepCopy()
	normalized.ResourceVersion = ""
	normalized.Generation = 0
	normalized.ManagedFields = nil
	return normalized
}

func normalizedValidatingConfiguration(
	configuration *admissionregistrationv1.ValidatingWebhookConfiguration,
) *admissionregistrationv1.ValidatingWebhookConfiguration {
	if configuration == nil {
		return nil
	}
	normalized := configuration.DeepCopy()
	normalized.ResourceVersion = ""
	normalized.Generation = 0
	normalized.ManagedFields = nil
	return normalized
}

func admissionCanaryPublicationIdentity(publication admissionCanaryPublication) (string, error) {
	payload := struct {
		Mutating struct {
			Name            string                                    `json:"name"`
			UID             types.UID                                 `json:"uid"`
			ResourceVersion string                                    `json:"resourceVersion"`
			Webhooks        []admissionregistrationv1.MutatingWebhook `json:"webhooks"`
		} `json:"mutating"`
		Validating struct {
			Name            string                                      `json:"name"`
			UID             types.UID                                   `json:"uid"`
			ResourceVersion string                                      `json:"resourceVersion"`
			Webhooks        []admissionregistrationv1.ValidatingWebhook `json:"webhooks"`
		} `json:"validating"`
		Marker admissionCanaryMarkerIdentity `json:"marker"`
	}{}
	payload.Mutating.Name = publication.mutating.Name
	payload.Mutating.UID = publication.mutating.UID
	payload.Mutating.ResourceVersion = publication.mutating.ResourceVersion
	payload.Mutating.Webhooks = publication.mutating.Webhooks
	payload.Validating.Name = publication.validating.Name
	payload.Validating.UID = publication.validating.UID
	payload.Validating.ResourceVersion = publication.validating.ResourceVersion
	payload.Validating.Webhooks = publication.validating.Webhooks
	payload.Marker = markerIdentity(publication.marker)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode admission canary stored contract identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest), nil
}

type admissionCanaryMarkerIdentity struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               types.UID         `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion"`
	CreationTimestamp string            `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	Immutable         bool              `json:"immutable"`
}

func markerIdentity(marker *corev1.ConfigMap) admissionCanaryMarkerIdentity {
	identity := admissionCanaryMarkerIdentity{}
	if marker == nil {
		return identity
	}
	identity.Name = marker.Name
	identity.Namespace = marker.Namespace
	identity.UID = marker.UID
	identity.ResourceVersion = marker.ResourceVersion
	identity.CreationTimestamp = marker.CreationTimestamp.Time.UTC().Format(time.RFC3339Nano)
	identity.Labels = maps.Clone(marker.Labels)
	identity.Annotations = maps.Clone(marker.Annotations)
	identity.Immutable = marker.Immutable != nil && *marker.Immutable
	return identity
}

func verifyAdmissionCanaryMarker(marker *corev1.ConfigMap, namespace, name, releaseName string) error {
	if marker == nil || marker.UID == "" || marker.ResourceVersion == "" ||
		marker.ResourceVersion != strings.TrimSpace(marker.ResourceVersion) || marker.CreationTimestamp.IsZero() {
		return fmt.Errorf("admission canary ConfigMap/%s has an incomplete live identity", name)
	}
	wantTypeMeta := metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "ConfigMap"}
	if marker.TypeMeta != (metav1.TypeMeta{}) && marker.TypeMeta != wantTypeMeta {
		return fmt.Errorf("admission canary ConfigMap/%s has a foreign type identity", name)
	}
	observed := marker.DeepCopy()
	observed.TypeMeta = wantTypeMeta
	observed.UID = ""
	observed.ResourceVersion = ""
	observed.CreationTimestamp = metav1.Time{}
	observed.ManagedFields = nil
	if !reflect.DeepEqual(observed, AdmissionCanaryMarker(namespace, name, releaseName)) {
		return fmt.Errorf("admission canary ConfigMap/%s differs from the exact immutable marker contract", name)
	}
	return nil
}

func sameAdmissionCanaryMarker(got, want *corev1.ConfigMap, releaseName string) bool {
	if got == nil || want == nil || verifyAdmissionCanaryMarker(got, want.Namespace, want.Name, releaseName) != nil {
		return false
	}
	return reflect.DeepEqual(markerIdentity(got), markerIdentity(want))
}

func (c *AdmissionCanary) endpointProbe(expectedMarker *corev1.ConfigMap) kubeapi.EndpointProbe {
	return func(ctx context.Context, endpoint kubeapi.Endpoint) (bool, error) {
		marker, err := c.directGetMarker(ctx, endpoint)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return false, contextErr
			}
			return false, nil
		}
		if !sameAdmissionCanaryMarker(marker, expectedMarker, c.config.ReleaseName) {
			return false, nil
		}
		probes := []struct {
			fieldManager string
			matches      func(error, string, types.UID) bool
		}{
			{fieldManager: AdmissionCanaryMutatingFieldManager, matches: HasExactAdmissionCanaryMutatingDenial},
			{fieldManager: AdmissionCanaryValidatingFieldManager, matches: HasExactAdmissionCanaryValidatingDenial},
		}
		for _, probe := range probes {
			err := c.directDryRunUpdate(ctx, endpoint, marker, probe.fieldManager)
			if contextErr := ctx.Err(); contextErr != nil {
				return false, contextErr
			}
			if !probe.matches(err, marker.Name, marker.UID) {
				// An admitted update, a transport/TLS failure, or a denial from
				// any other admission configuration is only inconclusive. None
				// may prove this endpoint's candidate webhook cache.
				return false, nil
			}
		}
		return true, nil
	}
}

func (c *AdmissionCanary) directGetMarker(ctx context.Context, endpoint kubeapi.Endpoint) (*corev1.ConfigMap, error) {
	client, closeClient, err := c.directClientFactory(endpoint.RESTConfig, c.config.MarkerNamespace)
	if err != nil {
		return nil, err
	}
	defer closeClient()
	return client.Get(ctx, c.config.MarkerName, metav1.GetOptions{})
}

func (c *AdmissionCanary) directDryRunUpdate(
	ctx context.Context,
	endpoint kubeapi.Endpoint,
	marker *corev1.ConfigMap,
	fieldManager string,
) error {
	client, closeClient, err := c.directClientFactory(endpoint.RESTConfig, c.config.MarkerNamespace)
	if err != nil {
		return err
	}
	defer closeClient()
	_, err = client.Update(ctx, marker.DeepCopy(), metav1.UpdateOptions{
		DryRun:          []string{metav1.DryRunAll},
		FieldManager:    fieldManager,
		FieldValidation: metav1.FieldValidationStrict,
	})
	return err
}

type admissionCanaryMarkerClient interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error)
	Update(context.Context, *corev1.ConfigMap, metav1.UpdateOptions) (*corev1.ConfigMap, error)
}

type admissionCanaryDirectClientFactory func(*rest.Config, string) (admissionCanaryMarkerClient, func(), error)

func newAdmissionCanaryDirectClient(
	config *rest.Config,
	namespace string,
) (admissionCanaryMarkerClient, func(), error) {
	if config == nil {
		return nil, nil, errors.New("admission canary direct API-server REST configuration is nil")
	}
	if config.Transport != nil || config.WrapTransport != nil || config.Dial != nil {
		return nil, nil, errors.New("admission canary direct API-server REST configuration contains a custom transport hook")
	}
	direct := rest.CopyConfig(config)
	// A new HTTP/1.1 client is created for every GET or UPDATE. Request.Close
	// and CloseIdleConnections prevent a proof request from riding a reused
	// connection even within one directly pinned API-server endpoint.
	direct.NextProtos = []string{"http/1.1"}
	direct.WrapTransport = func(next http.RoundTripper) http.RoundTripper {
		return admissionCanaryCloseRoundTripper{next: next}
	}
	httpClient, err := rest.HTTPClientFor(direct)
	if err != nil {
		return nil, nil, fmt.Errorf("build admission canary direct API-server HTTP client: %w", err)
	}
	client, err := typedcorev1.NewForConfigAndClient(direct, httpClient)
	if err != nil {
		httpClient.CloseIdleConnections()
		return nil, nil, fmt.Errorf("build admission canary direct ConfigMap client: %w", err)
	}
	return client.ConfigMaps(namespace), httpClient.CloseIdleConnections, nil
}

type admissionCanaryCloseRoundTripper struct {
	next http.RoundTripper
}

func (transport admissionCanaryCloseRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Close = true
	return transport.next.RoundTrip(clone)
}

func admissionCanaryStringPointer(value string) *string { return &value }

func admissionCanaryInt32Pointer(value int32) *int32 { return &value }
