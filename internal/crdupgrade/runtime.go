package crdupgrade

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	AdmissionConfigurationName               = "ptah-operator-admission"
	ReleaseNameAnnotation                    = "operator.ptah.dev/release-name"
	ReleaseNamespaceAnnotation               = "operator.ptah.dev/release-namespace"
	CoordinationAnnotation                   = "operator.ptah.dev/coordination-namespace"
	LeaderElectionAnnotation                 = "operator.ptah.dev/leader-election"
	LeaderElectionIDAnnotation               = "operator.ptah.dev/leader-election-id"
	WebhookServiceAnnotation                 = "operator.ptah.dev/webhook-service-name"
	HookServiceAccountAnnotation             = "operator.ptah.dev/hook-service-account-name"
	ControllerServiceAccountAnnotation       = "operator.ptah.dev/controller-service-account-name"
	ControllerDeploymentAnnotation           = "operator.ptah.dev/controller-deployment-name"
	CertificateDeploymentAnnotation          = "operator.ptah.dev/certificate-deployment-name"
	AdmissionContractVersionAnnotation       = "operator.ptah.dev/admission-contract-version"
	CurrentAdmissionContractVersion    int32 = 1

	mutatingApprovalWebhookName         = "mapproval.operator.ptah.dev"
	validatingApprovalWebhookName       = "vapproval.operator.ptah.dev"
	podIntentWebhookName                = "vpodintent.operator.ptah.dev"
	mutatingApprovalPath                = "/mutate-operator-ptah-dev-v1alpha1-ptahschemaapproval"
	validatingApprovalPath              = "/validate-operator-ptah-dev-v1alpha1-ptahschemaapproval"
	podIntentPath                       = "/validate-v1-pod-ptah-operation-intent"
	podIntentMatchConditionName         = "job-owned-pod"
	podIntentMatchExpression            = `object.metadata.ownerReferences.exists(ref, ref.apiVersion == 'batch/v1' && ref.kind == 'Job' && ref.controller == true) || (request.operation == 'UPDATE' && oldObject != null && oldObject.metadata.ownerReferences.exists(ref, ref.apiVersion == 'batch/v1' && ref.kind == 'Job' && ref.controller == true))`
	storedControllerStatePageSize int64 = 500
)

type MutatingWebhookClient interface {
	Get(context.Context, string, metav1.GetOptions) (*admissionregistrationv1.MutatingWebhookConfiguration, error)
}

type ValidatingWebhookClient interface {
	Get(context.Context, string, metav1.GetOptions) (*admissionregistrationv1.ValidatingWebhookConfiguration, error)
}

// ControllerStateListClient is the read-only API surface used to scan one
// namespaced Ptah resource kind for durable controller-state versions.
type ControllerStateListClient interface {
	List(context.Context, metav1.ListOptions) (*unstructured.UnstructuredList, error)
}

// StoredControllerStateClients contains every resource collection that can
// persist controller-written state. All three clients are mandatory whenever
// the downgrade preflight is enabled.
type StoredControllerStateClients struct {
	Schemas   ControllerStateListClient
	Plans     ControllerStateListClient
	Approvals ControllerStateListClient
}

type controllerStateLocation struct {
	name string
	path []string
}

var schemaControllerStateLocations = []controllerStateLocation{
	{name: "status.executionBinding", path: []string{"status", "executionBinding", "controllerStateVersion"}},
	{name: "status.plan", path: []string{"status", "plan", "controllerStateVersion"}},
	{name: "status.applied", path: []string{"status", "applied", "controllerStateVersion"}},
	{name: "status.pendingObservation.plan", path: []string{"status", "pendingObservation", "plan", "controllerStateVersion"}},
}

var immutableControllerStateLocation = []controllerStateLocation{
	{name: "spec", path: []string{"spec", "controllerStateVersion"}},
}

// RuntimeInvariants identify the only Helm release allowed to run a manager or
// certificate rotator against the fixed admission singleton.
type RuntimeInvariants struct {
	ReleaseName                  string
	ReleaseNamespace             string
	CoordinationNamespace        string
	LeaderElection               bool
	LeaderElectionID             string
	WebhookServiceName           string
	WebhookTimeoutSeconds        int32
	HookServiceAccountName       string
	ControllerServiceAccountName string
	ControllerDeploymentName     string
	CertificateDeploymentName    string
	ControllerStateVersion       int32
	AdmissionContractVersion     int32
	ReleaseSequence              int32
}

func (i RuntimeInvariants) validate() error {
	if i.ReleaseName == "" {
		return fmt.Errorf("release name is required")
	}
	if i.ReleaseNamespace == "" {
		return fmt.Errorf("release namespace is required")
	}
	if i.CoordinationNamespace == "" {
		return fmt.Errorf("coordination namespace is required")
	}
	if i.LeaderElectionID == "" {
		return fmt.Errorf("leader-election ID is required")
	}
	if i.WebhookServiceName == "" {
		return fmt.Errorf("webhook Service name is required")
	}
	for name, value := range map[string]string{
		"hook service account name":       i.HookServiceAccountName,
		"controller service account name": i.ControllerServiceAccountName,
		"controller Deployment name":      i.ControllerDeploymentName,
		"certificate Deployment name":     i.CertificateDeploymentName,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if i.ControllerDeploymentName == i.CertificateDeploymentName {
		return fmt.Errorf("controller and certificate Deployment names must differ")
	}
	if i.WebhookTimeoutSeconds < 1 || i.WebhookTimeoutSeconds > 30 {
		return fmt.Errorf("webhook timeout seconds must be between 1 and 30")
	}
	if i.ControllerStateVersion < 1 {
		return fmt.Errorf("controller-state version must be positive")
	}
	if i.AdmissionContractVersion < 1 {
		return fmt.Errorf("admission-contract version must be positive")
	}
	if i.ReleaseSequence < 1 {
		return fmt.Errorf("release sequence must be positive")
	}
	return nil
}

func (i RuntimeInvariants) annotations() map[string]string {
	return map[string]string{
		ReleaseNameAnnotation:              i.ReleaseName,
		ReleaseNamespaceAnnotation:         i.ReleaseNamespace,
		CoordinationAnnotation:             i.CoordinationNamespace,
		LeaderElectionAnnotation:           strconv.FormatBool(i.LeaderElection),
		LeaderElectionIDAnnotation:         i.LeaderElectionID,
		WebhookServiceAnnotation:           i.WebhookServiceName,
		HookServiceAccountAnnotation:       i.HookServiceAccountName,
		ControllerServiceAccountAnnotation: i.ControllerServiceAccountName,
		ControllerDeploymentAnnotation:     i.ControllerDeploymentName,
		CertificateDeploymentAnnotation:    i.CertificateDeploymentName,
		ControllerStateVersionAnnotation:   strconv.FormatInt(int64(i.ControllerStateVersion), 10),
		AdmissionContractVersionAnnotation: strconv.FormatInt(int64(i.AdmissionContractVersion), 10),
		ReleaseSequenceAnnotation:          strconv.FormatInt(int64(i.ReleaseSequence), 10),
	}
}

func (i RuntimeInvariants) immutableAnnotations() map[string]string {
	return map[string]string{
		ReleaseNameAnnotation:              i.ReleaseName,
		ReleaseNamespaceAnnotation:         i.ReleaseNamespace,
		CoordinationAnnotation:             i.CoordinationNamespace,
		LeaderElectionAnnotation:           strconv.FormatBool(i.LeaderElection),
		LeaderElectionIDAnnotation:         i.LeaderElectionID,
		WebhookServiceAnnotation:           i.WebhookServiceName,
		ControllerServiceAccountAnnotation: i.ControllerServiceAccountName,
		ControllerDeploymentAnnotation:     i.ControllerDeploymentName,
		CertificateDeploymentAnnotation:    i.CertificateDeploymentName,
	}
}

// RuntimeVerifier prevents a manager or certificate rotator from starting
// until both the CRD schemas and fixed admission ownership match its release.
type RuntimeVerifier struct {
	CRDs        *Manager
	Mutating    MutatingWebhookClient
	Validating  ValidatingWebhookClient
	Expected    RuntimeInvariants
	PollEvery   time.Duration
	StoredState *StoredControllerStateClients
	// SupportedControllerStateVersion is used only when StoredState is non-nil.
	SupportedControllerStateVersion int64
}

func (v *RuntimeVerifier) Verify(ctx context.Context) error {
	if v.CRDs == nil || v.Mutating == nil || v.Validating == nil {
		return fmt.Errorf("CRD and admission clients are required")
	}
	if err := v.Expected.validate(); err != nil {
		return err
	}
	if v.PollEvery <= 0 {
		return fmt.Errorf("runtime verification poll interval must be positive")
	}
	if err := v.CRDs.Verify(ctx); err != nil {
		return fmt.Errorf("verify candidate CRDs: %w", err)
	}
	missing := ""
	err := wait.PollUntilContextCancel(ctx, v.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		var checkErr error
		missing, checkErr = v.verifyAdmissionSingleton(pollCtx)
		return missing == "", checkErr
	})
	if err != nil && missing != "" {
		return fmt.Errorf("fixed admission singleton is incomplete: %s/%s is missing: %w", missing, AdmissionConfigurationName, err)
	}
	if err != nil {
		return err
	}
	missing, err = v.verifyAdmissionSingleton(ctx)
	if err != nil {
		return fmt.Errorf("final admission singleton verification: %w", err)
	}
	if missing != "" {
		return fmt.Errorf("fixed admission singleton changed after becoming ready: %s/%s is missing", missing, AdmissionConfigurationName)
	}
	if err := v.CRDs.Verify(ctx); err != nil {
		return fmt.Errorf("re-verify candidate CRDs after admission became ready: %w", err)
	}
	if v.StoredState != nil {
		if err := VerifyStoredControllerState(ctx, *v.StoredState, v.SupportedControllerStateVersion); err != nil {
			return err
		}
	}
	return nil
}

func (v *RuntimeVerifier) verifyAdmissionSingleton(ctx context.Context) (string, error) {
	mutating, err := v.Mutating.Get(ctx, AdmissionConfigurationName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "MutatingWebhookConfiguration", nil
	}
	if err != nil {
		return "", fmt.Errorf("get fixed MutatingWebhookConfiguration: %w", err)
	}
	if err := verifyAnnotations("MutatingWebhookConfiguration", mutating.Name, mutating.Annotations, v.Expected.annotations()); err != nil {
		return "", err
	}
	if err := verifyMutatingWebhookContract(mutating, v.Expected); err != nil {
		return "", err
	}

	validating, err := v.Validating.Get(ctx, AdmissionConfigurationName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "ValidatingWebhookConfiguration", nil
	}
	if err != nil {
		return "", fmt.Errorf("get fixed ValidatingWebhookConfiguration: %w", err)
	}
	if err := verifyAnnotations("ValidatingWebhookConfiguration", validating.Name, validating.Annotations, v.Expected.annotations()); err != nil {
		return "", err
	}
	if err := verifyValidatingWebhookContract(validating, v.Expected); err != nil {
		return "", err
	}
	return "", nil
}

type webhookContract struct {
	name               string
	path               string
	operations         []admissionregistrationv1.OperationType
	apiGroups          []string
	apiVersions        []string
	resources          []string
	objectSelector     *metav1.LabelSelector
	matchConditionName string
	matchExpression    string
	reinvocationPolicy *admissionregistrationv1.ReinvocationPolicyType
}

type webhookView struct {
	name                    string
	admissionReviewVersions []string
	clientConfig            admissionregistrationv1.WebhookClientConfig
	rules                   []admissionregistrationv1.RuleWithOperations
	failurePolicy           *admissionregistrationv1.FailurePolicyType
	matchPolicy             *admissionregistrationv1.MatchPolicyType
	namespaceSelector       *metav1.LabelSelector
	objectSelector          *metav1.LabelSelector
	sideEffects             *admissionregistrationv1.SideEffectClass
	timeoutSeconds          *int32
	matchConditions         []admissionregistrationv1.MatchCondition
	reinvocationPolicy      *admissionregistrationv1.ReinvocationPolicyType
}

func verifyMutatingWebhookContract(configuration *admissionregistrationv1.MutatingWebhookConfiguration, expected RuntimeInvariants) error {
	if len(configuration.Webhooks) != 1 {
		return fmt.Errorf("fixed admission singleton MutatingWebhookConfiguration/%s has %d webhooks, expected exactly 1", configuration.Name, len(configuration.Webhooks))
	}
	webhook := configuration.Webhooks[0]
	never := admissionregistrationv1.NeverReinvocationPolicy
	return verifyWebhookContract("MutatingWebhookConfiguration", configuration.Name, webhookView{
		name: webhook.Name, admissionReviewVersions: webhook.AdmissionReviewVersions,
		clientConfig: webhook.ClientConfig, rules: webhook.Rules,
		failurePolicy: webhook.FailurePolicy, matchPolicy: webhook.MatchPolicy,
		namespaceSelector: webhook.NamespaceSelector, objectSelector: webhook.ObjectSelector,
		sideEffects: webhook.SideEffects, timeoutSeconds: webhook.TimeoutSeconds,
		matchConditions: webhook.MatchConditions, reinvocationPolicy: webhook.ReinvocationPolicy,
	}, webhookContract{
		name: mutatingApprovalWebhookName, path: mutatingApprovalPath,
		operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
		apiGroups:  []string{"operator.ptah.dev"}, apiVersions: []string{"v1alpha1"}, resources: []string{"ptahschemaapprovals"},
		reinvocationPolicy: &never,
	}, expected)
}

func verifyValidatingWebhookContract(configuration *admissionregistrationv1.ValidatingWebhookConfiguration, expected RuntimeInvariants) error {
	if len(configuration.Webhooks) != 2 {
		return fmt.Errorf("fixed admission singleton ValidatingWebhookConfiguration/%s has %d webhooks, expected exactly 2", configuration.Name, len(configuration.Webhooks))
	}
	want := map[string]webhookContract{
		validatingApprovalWebhookName: {
			name: validatingApprovalWebhookName, path: validatingApprovalPath,
			operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
			apiGroups:  []string{"operator.ptah.dev"}, apiVersions: []string{"v1alpha1"}, resources: []string{"ptahschemaapprovals"},
		},
		podIntentWebhookName: {
			name: podIntentWebhookName, path: podIntentPath,
			operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
			apiGroups:  []string{""}, apiVersions: []string{"v1"}, resources: []string{"pods", "pods/ephemeralcontainers", "pods/resize"},
			matchConditionName: podIntentMatchConditionName,
			matchExpression:    podIntentMatchExpression,
		},
	}
	seen := make(map[string]struct{}, len(configuration.Webhooks))
	for _, webhook := range configuration.Webhooks {
		contract, found := want[webhook.Name]
		if !found {
			return fmt.Errorf("fixed admission singleton ValidatingWebhookConfiguration/%s has unexpected webhook %q", configuration.Name, webhook.Name)
		}
		if _, duplicate := seen[webhook.Name]; duplicate {
			return fmt.Errorf("fixed admission singleton ValidatingWebhookConfiguration/%s duplicates webhook %q", configuration.Name, webhook.Name)
		}
		seen[webhook.Name] = struct{}{}
		if err := verifyWebhookContract("ValidatingWebhookConfiguration", configuration.Name, webhookView{
			name: webhook.Name, admissionReviewVersions: webhook.AdmissionReviewVersions,
			clientConfig: webhook.ClientConfig, rules: webhook.Rules,
			failurePolicy: webhook.FailurePolicy, matchPolicy: webhook.MatchPolicy,
			namespaceSelector: webhook.NamespaceSelector, objectSelector: webhook.ObjectSelector,
			sideEffects: webhook.SideEffects, timeoutSeconds: webhook.TimeoutSeconds,
			matchConditions: webhook.MatchConditions,
		}, contract, expected); err != nil {
			return err
		}
	}
	return nil
}

func verifyWebhookContract(kind, configurationName string, actual webhookView, want webhookContract, expected RuntimeInvariants) error {
	prefix := fmt.Sprintf("fixed admission singleton %s/%s webhook %s", kind, configurationName, want.name)
	if actual.name != want.name {
		return fmt.Errorf("%s has name %q", prefix, actual.name)
	}
	if !reflect.DeepEqual(actual.admissionReviewVersions, []string{"v1"}) {
		return fmt.Errorf("%s admissionReviewVersions = %v, expected [v1]", prefix, actual.admissionReviewVersions)
	}
	if actual.failurePolicy == nil || *actual.failurePolicy != admissionregistrationv1.Fail {
		return fmt.Errorf("%s failurePolicy must be Fail", prefix)
	}
	if actual.sideEffects == nil || *actual.sideEffects != admissionregistrationv1.SideEffectClassNone {
		return fmt.Errorf("%s sideEffects must be None", prefix)
	}
	if actual.matchPolicy == nil || *actual.matchPolicy != admissionregistrationv1.Equivalent {
		return fmt.Errorf("%s matchPolicy must be Equivalent", prefix)
	}
	if actual.timeoutSeconds == nil || *actual.timeoutSeconds != expected.WebhookTimeoutSeconds {
		return fmt.Errorf("%s timeoutSeconds must be %d", prefix, expected.WebhookTimeoutSeconds)
	}
	if !equalOptional(actual.reinvocationPolicy, want.reinvocationPolicy) {
		return fmt.Errorf("%s reinvocationPolicy is not the expected value", prefix)
	}
	if len(actual.clientConfig.CABundle) == 0 {
		return fmt.Errorf("%s caBundle must be nonempty", prefix)
	}
	if actual.clientConfig.URL != nil || actual.clientConfig.Service == nil {
		return fmt.Errorf("%s must use the exact webhook Service, not a URL", prefix)
	}
	service := actual.clientConfig.Service
	if service.Namespace != expected.ReleaseNamespace || service.Name != expected.WebhookServiceName ||
		service.Path == nil || *service.Path != want.path || service.Port == nil || *service.Port != 443 {
		return fmt.Errorf("%s Service target does not match %s/%s%s on port 443", prefix, expected.ReleaseNamespace, expected.WebhookServiceName, want.path)
	}
	scope := admissionregistrationv1.NamespacedScope
	wantRules := []admissionregistrationv1.RuleWithOperations{{
		Operations: want.operations,
		Rule: admissionregistrationv1.Rule{
			APIGroups: want.apiGroups, APIVersions: want.apiVersions,
			Resources: want.resources, Scope: &scope,
		},
	}}
	if !reflect.DeepEqual(actual.rules, wantRules) {
		return fmt.Errorf("%s rules do not match the exact admission scope", prefix)
	}
	if actual.namespaceSelector != nil {
		return fmt.Errorf("%s must not have a namespaceSelector", prefix)
	}
	if !reflect.DeepEqual(actual.objectSelector, want.objectSelector) {
		return fmt.Errorf("%s objectSelector does not match the exact admission scope", prefix)
	}
	if want.matchConditionName == "" {
		if len(actual.matchConditions) != 0 {
			return fmt.Errorf("%s must not have matchConditions", prefix)
		}
		return nil
	}
	if len(actual.matchConditions) != 1 || actual.matchConditions[0].Name != want.matchConditionName ||
		normalizeExpression(actual.matchConditions[0].Expression) != normalizeExpression(want.matchExpression) {
		return fmt.Errorf("%s matchConditions do not match the exact Job-owner predicate", prefix)
	}
	return nil
}

func equalOptional[T comparable](actual, expected *T) bool {
	if actual == nil || expected == nil {
		return actual == nil && expected == nil
	}
	return *actual == *expected
}

func normalizeExpression(expression string) string {
	var normalized strings.Builder
	var quote rune
	escaped := false
	for _, character := range expression {
		if quote != 0 {
			normalized.WriteRune(character)
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
				continue
			}
			if character == quote {
				quote = 0
			}
			continue
		}
		if unicode.IsSpace(character) {
			continue
		}
		normalized.WriteRune(character)
		if character == '\'' || character == '"' {
			quote = character
		}
	}
	return normalized.String()
}

// VerifyStoredControllerState rejects a candidate manager that cannot safely
// interpret any durable controller-state location stored in Ptah resources.
func VerifyStoredControllerState(ctx context.Context, clients StoredControllerStateClients, supported int64) error {
	if supported <= 0 {
		return fmt.Errorf("supported controller state version must be positive")
	}
	kinds := []struct {
		kind      string
		plural    string
		client    ControllerStateListClient
		locations []controllerStateLocation
	}{
		{kind: "PtahSchema", plural: "PtahSchemas", client: clients.Schemas, locations: schemaControllerStateLocations},
		{kind: "PtahSchemaPlan", plural: "PtahSchemaPlans", client: clients.Plans, locations: immutableControllerStateLocation},
		{kind: "PtahSchemaApproval", plural: "PtahSchemaApprovals", client: clients.Approvals, locations: immutableControllerStateLocation},
	}
	for _, resourceKind := range kinds {
		if isNilControllerStateClient(resourceKind.client) {
			return fmt.Errorf("%s client is required", resourceKind.kind)
		}
	}
	for _, resourceKind := range kinds {
		if err := verifyStoredControllerStateKind(ctx, resourceKind.kind, resourceKind.plural, resourceKind.client, resourceKind.locations, supported); err != nil {
			return err
		}
	}
	return nil
}

func isNilControllerStateClient(client ControllerStateListClient) bool {
	if client == nil {
		return true
	}
	value := reflect.ValueOf(client)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func verifyStoredControllerStateKind(
	ctx context.Context,
	kind string,
	plural string,
	client ControllerStateListClient,
	locations []controllerStateLocation,
	supported int64,
) error {
	continueToken := ""
	resourceVersion := ""
	firstPage := true
	seenContinueTokens := make(map[string]struct{})
	seenNames := make(map[string]struct{})
	seenUIDs := make(map[string]string)
	for {
		list, err := client.List(ctx, metav1.ListOptions{Limit: storedControllerStatePageSize, Continue: continueToken})
		if err != nil {
			return fmt.Errorf("list %s for controller downgrade preflight: %w", plural, err)
		}
		if list == nil {
			return fmt.Errorf("list %s for controller downgrade preflight returned no object", plural)
		}
		nextContinueToken := list.GetContinue()
		pageResourceVersion := list.GetResourceVersion()
		if pageResourceVersion == "" {
			return fmt.Errorf("%s pagination returned an empty resourceVersion", kind)
		}
		if firstPage {
			resourceVersion = pageResourceVersion
			firstPage = false
		} else if pageResourceVersion != resourceVersion {
			return fmt.Errorf(
				"%s pagination resourceVersion changed across pages from %q to %q",
				kind,
				resourceVersion,
				pageResourceVersion,
			)
		}
		if remaining := list.GetRemainingItemCount(); remaining != nil {
			if *remaining < 0 {
				return fmt.Errorf("%s pagination returned negative remainingItemCount %d", kind, *remaining)
			}
		}
		for i := range list.Items {
			object := &list.Items[i]
			objectName := object.GetNamespace() + "/" + object.GetName()
			if object.GetNamespace() == "" || object.GetName() == "" || object.GetUID() == "" {
				return fmt.Errorf("%s pagination returned a cluster-scoped or incomplete object %q", kind, objectName)
			}
			if _, found := seenNames[objectName]; found {
				return fmt.Errorf("%s pagination returned %s more than once", kind, objectName)
			}
			seenNames[objectName] = struct{}{}
			if previous, found := seenUIDs[string(object.GetUID())]; found {
				return fmt.Errorf("%s %s and %s share UID %s", plural, previous, objectName, object.GetUID())
			}
			seenUIDs[string(object.GetUID())] = objectName
			for _, location := range locations {
				version, found, nestedErr := unstructured.NestedInt64(object.Object, location.path...)
				if nestedErr != nil {
					return fmt.Errorf("%s %s has malformed stored controller state at %s.controllerStateVersion: %w", kind, objectName, location.name, nestedErr)
				}
				if !found || version == 0 {
					continue
				}
				if version < 0 {
					return fmt.Errorf("%s %s has invalid stored controller state version %d at %s.controllerStateVersion", kind, objectName, version, location.name)
				}
				if version > supported {
					return fmt.Errorf("controller downgrade refused: %s %s stores controller state version %d at %s.controllerStateVersion, but this manager supports %d", kind, objectName, version, location.name, supported)
				}
			}
		}
		if nextContinueToken == "" {
			return nil
		}
		if nextContinueToken == continueToken {
			return fmt.Errorf("%s pagination returned repeated continue token %q", kind, nextContinueToken)
		}
		if _, found := seenContinueTokens[nextContinueToken]; found {
			return fmt.Errorf("%s pagination repeated continue token %q", kind, nextContinueToken)
		}
		seenContinueTokens[nextContinueToken] = struct{}{}
		continueToken = nextContinueToken
	}
}

func verifyAnnotations(kind, name string, actual, expected map[string]string) error {
	for key, expectedValue := range expected {
		if actualValue := actual[key]; actualValue != expectedValue {
			return fmt.Errorf("fixed admission singleton %s/%s annotation %s is %q, expected %q", kind, name, key, actualValue, expectedValue)
		}
	}
	return nil
}
