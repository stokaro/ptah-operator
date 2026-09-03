package crdupgrade

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	runtimeAdmissionPageSize            = 500
	runtimeAdmissionMaxResourceKeys     = 64
	runtimeAdmissionMaxImagePullSecrets = 64
	runtimeAdmissionMaxSecretReferences = 64

	runtimeAdmissionEnforceMountableSecretsAnnotation = "kubernetes.io/enforce-mountable-secrets"
)

// RuntimeLimitRangeLister is the namespaced LimitRange API used by the
// runtime admission preflight. A typed core LimitRange client implements it.
type RuntimeLimitRangeLister interface {
	List(context.Context, metav1.ListOptions) (*corev1.LimitRangeList, error)
}

// RuntimeServiceAccountGetter is the namespaced ServiceAccount API used by
// the runtime admission preflight. A typed core ServiceAccount client
// implements it.
type RuntimeServiceAccountGetter interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.ServiceAccount, error)
}

// RuntimePriorityClassReader is the cluster-scoped PriorityClass API used by
// the runtime admission preflight. A typed scheduling PriorityClass client
// implements it.
type RuntimePriorityClassReader interface {
	Get(context.Context, string, metav1.GetOptions) (*schedulingv1.PriorityClass, error)
	List(context.Context, metav1.ListOptions) (*schedulingv1.PriorityClassList, error)
}

// RuntimeAdmissionContract is the exact portion of the two runtime Pod
// templates that built-in admission may mutate or reject. Both Pods share the
// same init-container resources and chart-level scheduling configuration.
type RuntimeAdmissionContract struct {
	Namespace                                       string                        `json:"namespace"`
	CommonInitContainerResources                    corev1.ResourceRequirements   `json:"commonInitContainerResources"`
	ControllerContainerResources                    corev1.ResourceRequirements   `json:"controllerContainerResources"`
	CertificateContainerResources                   corev1.ResourceRequirements   `json:"certificateContainerResources"`
	ImagePullSecrets                                []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	ControllerSecretNames                           []string                      `json:"controllerSecretNames"`
	CertificateSecretNames                          []string                      `json:"certificateSecretNames,omitempty"`
	PriorityClassName                               string                        `json:"priorityClassName,omitempty"`
	PriorityClassValue                              int32                         `json:"priorityClassValue"`
	PriorityClassPreemptionPolicy                   string                        `json:"priorityClassPreemptionPolicy"`
	ControllerServiceAccountName                    string                        `json:"controllerServiceAccountName"`
	CertificateServiceAccountName                   string                        `json:"certificateServiceAccountName"`
	ControllerServiceAccountCreate                  bool                          `json:"controllerServiceAccountCreate"`
	ControllerServiceAccountEnforceMountableSecrets bool                          `json:"controllerServiceAccountEnforceMountableSecrets"`
	CertificateRuntimeEnabled                       bool                          `json:"certificateRuntimeEnabled"`
}

// RuntimeAdmissionPreflight verifies that the covered built-in Pod admission
// paths will neither mutate nor reject either immutable runtime Pod contract
// after activation.
type RuntimeAdmissionPreflight struct {
	LimitRanges     RuntimeLimitRangeLister
	ServiceAccounts RuntimeServiceAccountGetter
	PriorityClasses RuntimePriorityClassReader
	Contract        RuntimeAdmissionContract
}

// NewRuntimeAdmissionPreflight constructs a fail-closed runtime admission
// check from narrowly scoped Kubernetes clients and an exact chart contract.
func NewRuntimeAdmissionPreflight(
	contract RuntimeAdmissionContract,
	limitRanges RuntimeLimitRangeLister,
	serviceAccounts RuntimeServiceAccountGetter,
	priorityClasses RuntimePriorityClassReader,
) *RuntimeAdmissionPreflight {
	contract.CommonInitContainerResources = *contract.CommonInitContainerResources.DeepCopy()
	contract.ControllerContainerResources = *contract.ControllerContainerResources.DeepCopy()
	contract.CertificateContainerResources = *contract.CertificateContainerResources.DeepCopy()
	contract.ImagePullSecrets = append([]corev1.LocalObjectReference(nil), contract.ImagePullSecrets...)
	contract.ControllerSecretNames = append([]string(nil), contract.ControllerSecretNames...)
	contract.CertificateSecretNames = append([]string(nil), contract.CertificateSecretNames...)
	return &RuntimeAdmissionPreflight{
		LimitRanges:     limitRanges,
		ServiceAccounts: serviceAccounts,
		PriorityClasses: priorityClasses,
		Contract:        contract,
	}
}

// Check performs the read-only runtime admission preflight. It is intended to
// run before the current controller is quiesced and before a release sequence
// is activated.
func (p *RuntimeAdmissionPreflight) Check(ctx context.Context) error {
	if err := p.validate(); err != nil {
		return err
	}

	containers := p.runtimeContainers()
	for _, container := range containers {
		if err := validateRuntimeResourceRequirements(container.name, container.resources); err != nil {
			return err
		}

		if err := rejectCorePodResourceDefaulting(container.name, container.resources); err != nil {
			return err
		}
	}

	limitRanges, err := p.listLimitRanges(ctx)
	if err != nil {
		return err
	}
	if err := p.checkLimitRanges(limitRanges, containers); err != nil {
		return err
	}
	if err := p.checkServiceAccounts(ctx); err != nil {
		return err
	}
	return p.checkPriorityClass(ctx)
}

func (p *RuntimeAdmissionPreflight) listLimitRanges(ctx context.Context) ([]corev1.LimitRange, error) {
	var result []corev1.LimitRange
	state := newInventoryPageState()
	continueToken := ""
	for {
		page, err := p.LimitRanges.List(ctx, metav1.ListOptions{Limit: runtimeAdmissionPageSize, Continue: continueToken})
		if err != nil {
			return nil, fmt.Errorf("list LimitRanges in namespace %s: %w", p.Contract.Namespace, err)
		}
		if page == nil {
			return nil, errors.New("list LimitRanges returned a nil page")
		}
		if err := state.validatePage("LimitRange", continueToken, page.ListMeta, len(page.Items), runtimeAdmissionPageSize); err != nil {
			return nil, err
		}
		for index := range page.Items {
			limitRange := &page.Items[index]
			if limitRange.Namespace != p.Contract.Namespace || limitRange.Name == "" {
				return nil, fmt.Errorf("LimitRange inventory returned foreign or incomplete object %q", limitRange.Namespace+"/"+limitRange.Name)
			}
			if err := state.observeObject("LimitRange", limitRange.Namespace, limitRange.Name, string(limitRange.UID)); err != nil {
				return nil, err
			}
		}
		result = append(result, page.Items...)
		if page.Continue == "" {
			return result, nil
		}
		continueToken = page.Continue
	}
}

func (p *RuntimeAdmissionPreflight) validate() error {
	if p == nil {
		return errors.New("runtime admission preflight is required")
	}
	if p.LimitRanges == nil {
		return errors.New("runtime admission LimitRange client is required")
	}
	if p.ServiceAccounts == nil {
		return errors.New("runtime admission ServiceAccount client is required")
	}
	if p.PriorityClasses == nil {
		return errors.New("runtime admission PriorityClass client is required")
	}
	if p.Contract.Namespace == "" || p.Contract.Namespace != strings.TrimSpace(p.Contract.Namespace) {
		return errors.New("runtime admission namespace is required and must not contain surrounding whitespace")
	}
	for _, identity := range []struct {
		description string
		value       string
	}{
		{description: "controller ServiceAccount name", value: p.Contract.ControllerServiceAccountName},
		{description: "certificate ServiceAccount name", value: p.Contract.CertificateServiceAccountName},
	} {
		if identity.value == "" || identity.value != strings.TrimSpace(identity.value) {
			return fmt.Errorf("runtime admission %s is required and must not contain surrounding whitespace", identity.description)
		}
	}
	if p.Contract.ControllerServiceAccountName == p.Contract.CertificateServiceAccountName {
		return errors.New("runtime admission controller and certificate ServiceAccount names must differ")
	}
	if p.Contract.PriorityClassName != strings.TrimSpace(p.Contract.PriorityClassName) {
		return errors.New("runtime admission PriorityClass name must not contain surrounding whitespace")
	}
	if p.Contract.PriorityClassName == "" {
		if p.Contract.PriorityClassValue != 0 {
			return fmt.Errorf("runtime priorityClassName is empty, so priorityClassValue must be 0, got %d", p.Contract.PriorityClassValue)
		}
		if p.Contract.PriorityClassPreemptionPolicy != string(corev1.PreemptLowerPriority) {
			return fmt.Errorf("runtime priorityClassName is empty, so priorityClassPreemptionPolicy must be %s, got %q", corev1.PreemptLowerPriority, p.Contract.PriorityClassPreemptionPolicy)
		}
	} else {
		switch corev1.PreemptionPolicy(p.Contract.PriorityClassPreemptionPolicy) {
		case corev1.PreemptLowerPriority, corev1.PreemptNever:
		default:
			return fmt.Errorf("runtime priorityClassPreemptionPolicy for PriorityClass %s must be %s or %s, got %q", p.Contract.PriorityClassName, corev1.PreemptLowerPriority, corev1.PreemptNever, p.Contract.PriorityClassPreemptionPolicy)
		}
	}
	if len(p.Contract.ImagePullSecrets) > runtimeAdmissionMaxImagePullSecrets {
		return fmt.Errorf("runtime Pod contract has more than %d image pull secrets", runtimeAdmissionMaxImagePullSecrets)
	}
	for index, secret := range p.Contract.ImagePullSecrets {
		if secret.Name == "" || secret.Name != strings.TrimSpace(secret.Name) {
			return fmt.Errorf("runtime Pod image pull secret at index %d has an empty or whitespace-padded name", index)
		}
	}
	if len(p.Contract.ControllerSecretNames) == 0 {
		return errors.New("runtime admission controller Secret names are required")
	}
	if err := validateRuntimeSecretNames("controller", p.Contract.ControllerSecretNames); err != nil {
		return err
	}
	if p.Contract.CertificateRuntimeEnabled {
		if err := validateRuntimeSecretNames("certificate", p.Contract.CertificateSecretNames); err != nil {
			return err
		}
	}
	return nil
}

func validateRuntimeSecretNames(runtimeName string, names []string) error {
	if len(names) > runtimeAdmissionMaxSecretReferences {
		return fmt.Errorf("%s runtime Pod contract has more than %d Secret references", runtimeName, runtimeAdmissionMaxSecretReferences)
	}
	seen := make(map[string]struct{}, len(names))
	for index, name := range names {
		if name == "" || name != strings.TrimSpace(name) {
			return fmt.Errorf("%s runtime Pod Secret reference at index %d has an empty or whitespace-padded name", runtimeName, index)
		}
		if _, found := seen[name]; found {
			return fmt.Errorf("%s runtime Pod Secret reference %s is duplicated", runtimeName, name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

type runtimeAdmissionContainer struct {
	name      string
	resources corev1.ResourceRequirements
}

func (p *RuntimeAdmissionPreflight) runtimeContainers() []runtimeAdmissionContainer {
	containers := []runtimeAdmissionContainer{
		{name: "common init container", resources: p.Contract.CommonInitContainerResources},
		{name: "controller container", resources: p.Contract.ControllerContainerResources},
	}
	if p.Contract.CertificateRuntimeEnabled {
		containers = append(containers, runtimeAdmissionContainer{name: "certificate container", resources: p.Contract.CertificateContainerResources})
	}
	return containers
}

func validateRuntimeResourceRequirements(name string, requirements corev1.ResourceRequirements) error {
	if len(requirements.Claims) != 0 {
		return fmt.Errorf("%s resources.claims must be empty; dynamic resource claims are outside the immutable runtime contract", name)
	}
	if len(requirements.Requests) > runtimeAdmissionMaxResourceKeys || len(requirements.Limits) > runtimeAdmissionMaxResourceKeys {
		return fmt.Errorf("%s has more than %d request or limit resource keys", name, runtimeAdmissionMaxResourceKeys)
	}
	for _, resourceField := range []struct {
		name   string
		values corev1.ResourceList
	}{
		{name: "requests", values: requirements.Requests},
		{name: "limits", values: requirements.Limits},
	} {
		for _, resourceName := range sortedRuntimeResourceNames(resourceField.values) {
			quantity := resourceField.values[resourceName]
			if !supportedRuntimeAdmissionResource(resourceName) {
				return fmt.Errorf("%s resources.%s[%s] is unsupported; only cpu, memory, and ephemeral-storage are allowed", name, resourceField.name, resourceName)
			}
			if quantity.Sign() < 0 {
				return fmt.Errorf("%s resources.%s[%s] must not be negative", name, resourceField.name, resourceName)
			}
			rounded := quantity.DeepCopy()
			if exact := rounded.RoundUp(resource.Milli); !exact || rounded.Cmp(quantity) != 0 {
				return fmt.Errorf("%s resources.%s[%s]=%s has more precision than the Kubernetes API preserves; it would round up to %s, so configure the rounded value explicitly", name, resourceField.name, resourceName, quantity.String(), rounded.String())
			}
		}
	}
	for _, resourceName := range sortedRuntimeResourceNames(requirements.Requests) {
		request := requirements.Requests[resourceName]
		limit, found := requirements.Limits[resourceName]
		if found && request.Cmp(limit) > 0 {
			return fmt.Errorf("%s resources.requests[%s]=%s exceeds resources.limits[%s]=%s", name, resourceName, request.String(), resourceName, limit.String())
		}
	}
	return nil
}

func supportedRuntimeAdmissionResource(name corev1.ResourceName) bool {
	switch name {
	case corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage:
		return true
	default:
		return false
	}
}

func rejectCorePodResourceDefaulting(name string, requirements corev1.ResourceRequirements) error {
	for _, resourceName := range sortedRuntimeResourceNames(requirements.Limits) {
		if _, found := requirements.Requests[resourceName]; found {
			continue
		}
		limit := requirements.Limits[resourceName]
		return fmt.Errorf("%s resources.requests[%s] is omitted while resources.limits[%s]=%s; core Pod defaulting would copy the limit into the request, so configure an explicit request", name, resourceName, resourceName, limit.String())
	}
	return nil
}

func (p *RuntimeAdmissionPreflight) checkLimitRanges(
	limitRanges []corev1.LimitRange,
	containers []runtimeAdmissionContainer,
) error {
	for limitRangeIndex := range limitRanges {
		limitRange := &limitRanges[limitRangeIndex]
		containerItems := 0
		for itemIndex := range limitRange.Spec.Limits {
			item := limitRange.Spec.Limits[itemIndex]
			if err := validateRuntimeLimitRangeItemBounds(limitRange, &item); err != nil {
				return err
			}
			switch item.Type {
			case corev1.LimitTypeContainer:
				containerItems++
				if containerItems > 1 {
					return fmt.Errorf("LimitRange %s/%s has more than one Container item; runtime admission order is ambiguous", p.Contract.Namespace, limitRange.Name)
				}
				defaultRequests, defaultLimits := normalizedRuntimeContainerDefaults(item)
				for _, container := range containers {
					if err := rejectLimitRangeResourceDefaulting(p.Contract.Namespace, limitRange.Name, container, defaultRequests, defaultLimits); err != nil {
						return err
					}
					if err := validateRuntimeLimitConstraints(p.Contract.Namespace, limitRange.Name, "Container", container.name, container.resources.Requests, container.resources.Limits, item); err != nil {
						return err
					}
				}
			case corev1.LimitTypePod:
				controllerRequests := aggregateRuntimePodResources(p.Contract.ControllerContainerResources.Requests, p.Contract.CommonInitContainerResources.Requests)
				controllerLimits := aggregateRuntimePodResources(p.Contract.ControllerContainerResources.Limits, p.Contract.CommonInitContainerResources.Limits)
				if err := validateRuntimeLimitConstraints(p.Contract.Namespace, limitRange.Name, "Pod", "controller Pod", controllerRequests, controllerLimits, item); err != nil {
					return err
				}
				if !p.Contract.CertificateRuntimeEnabled {
					continue
				}
				certificateRequests := aggregateRuntimePodResources(p.Contract.CertificateContainerResources.Requests, p.Contract.CommonInitContainerResources.Requests)
				certificateLimits := aggregateRuntimePodResources(p.Contract.CertificateContainerResources.Limits, p.Contract.CommonInitContainerResources.Limits)
				if err := validateRuntimeLimitConstraints(p.Contract.Namespace, limitRange.Name, "Pod", "certificate Pod", certificateRequests, certificateLimits, item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateRuntimeLimitRangeItemBounds(limitRange *corev1.LimitRange, item *corev1.LimitRangeItem) error {
	for _, resourceField := range []struct {
		name   string
		values corev1.ResourceList
	}{
		{name: "default", values: item.Default},
		{name: "defaultRequest", values: item.DefaultRequest},
		{name: "min", values: item.Min},
		{name: "max", values: item.Max},
		{name: "maxLimitRequestRatio", values: item.MaxLimitRequestRatio},
	} {
		if len(resourceField.values) > runtimeAdmissionMaxResourceKeys {
			return fmt.Errorf("LimitRange %s/%s %s item has more than %d %s resource keys", limitRange.Namespace, limitRange.Name, item.Type, runtimeAdmissionMaxResourceKeys, resourceField.name)
		}
	}
	return nil
}

// normalizedRuntimeContainerDefaults mirrors API defaulting for a Container
// LimitRange item. A missing default limit derives from max. A missing default
// request then derives from that normalized limit and finally from min.
func normalizedRuntimeContainerDefaults(item corev1.LimitRangeItem) (corev1.ResourceList, corev1.ResourceList) {
	requests := copyRuntimeResourceList(item.DefaultRequest)
	limits := copyRuntimeResourceList(item.Default)
	if requests == nil {
		requests = corev1.ResourceList{}
	}
	if limits == nil {
		limits = corev1.ResourceList{}
	}
	for name, quantity := range item.Max {
		if _, found := limits[name]; !found {
			limits[name] = quantity.DeepCopy()
		}
	}
	for name, quantity := range limits {
		if _, found := requests[name]; !found {
			requests[name] = quantity.DeepCopy()
		}
	}
	for name, quantity := range item.Min {
		if _, found := requests[name]; !found {
			requests[name] = quantity.DeepCopy()
		}
	}
	return requests, limits
}

func rejectLimitRangeResourceDefaulting(
	namespace string,
	limitRangeName string,
	container runtimeAdmissionContainer,
	defaultRequests corev1.ResourceList,
	defaultLimits corev1.ResourceList,
) error {
	for _, resourceName := range sortedRuntimeResourceNames(defaultLimits) {
		if _, found := container.resources.Limits[resourceName]; found {
			continue
		}
		quantity := defaultLimits[resourceName]
		return fmt.Errorf("LimitRange %s/%s would set %s resources.limits[%s] to %s; configure that limit explicitly or remove the namespace default", namespace, limitRangeName, container.name, resourceName, quantity.String())
	}
	for _, resourceName := range sortedRuntimeResourceNames(defaultRequests) {
		if _, found := container.resources.Requests[resourceName]; found {
			continue
		}
		quantity := defaultRequests[resourceName]
		return fmt.Errorf("LimitRange %s/%s would set %s resources.requests[%s] to %s; configure that request explicitly or remove the namespace default", namespace, limitRangeName, container.name, resourceName, quantity.String())
	}
	return nil
}

func validateRuntimeLimitConstraints(
	namespace string,
	limitRangeName string,
	limitType string,
	workloadName string,
	requests corev1.ResourceList,
	limits corev1.ResourceList,
	item corev1.LimitRangeItem,
) error {
	for _, resourceName := range sortedRuntimeResourceNames(item.Min) {
		minimum := item.Min[resourceName]
		request, requestFound := requests[resourceName]
		limit, limitFound := limits[resourceName]
		if !requestFound {
			return fmt.Errorf("LimitRange %s/%s requires minimum %s %s=%s, but %s has no request", namespace, limitRangeName, limitType, resourceName, minimum.String(), workloadName)
		}
		requestValue, limitValue, minimumValue := runtimeAdmissionComparableValues(request, limit, minimum)
		if requestValue < minimumValue {
			return fmt.Errorf("LimitRange %s/%s requires minimum %s %s=%s, but %s requests %s", namespace, limitRangeName, limitType, resourceName, minimum.String(), workloadName, request.String())
		}
		if limitFound && limitValue < minimumValue {
			return fmt.Errorf("LimitRange %s/%s requires minimum %s %s=%s, but %s limits it to %s", namespace, limitRangeName, limitType, resourceName, minimum.String(), workloadName, limit.String())
		}
	}
	for _, resourceName := range sortedRuntimeResourceNames(item.Max) {
		maximum := item.Max[resourceName]
		limit, limitFound := limits[resourceName]
		if !limitFound {
			return fmt.Errorf("LimitRange %s/%s requires maximum %s %s=%s, but %s has no limit", namespace, limitRangeName, limitType, resourceName, maximum.String(), workloadName)
		}
		request, requestFound := requests[resourceName]
		requestValue, limitValue, maximumValue := runtimeAdmissionComparableValues(request, limit, maximum)
		if limitValue > maximumValue {
			return fmt.Errorf("LimitRange %s/%s requires maximum %s %s=%s, but %s limits it to %s", namespace, limitRangeName, limitType, resourceName, maximum.String(), workloadName, limit.String())
		}
		if requestFound && requestValue > maximumValue {
			return fmt.Errorf("LimitRange %s/%s requires maximum %s %s=%s, but %s requests %s", namespace, limitRangeName, limitType, resourceName, maximum.String(), workloadName, request.String())
		}
	}
	for _, resourceName := range sortedRuntimeResourceNames(item.MaxLimitRequestRatio) {
		maximumRatio := item.MaxLimitRequestRatio[resourceName]
		request, requestFound := requests[resourceName]
		limit, limitFound := limits[resourceName]
		if !requestFound || request.IsZero() {
			return fmt.Errorf("LimitRange %s/%s requires a nonzero %s %s request for max limit-to-request ratio %s, but %s does not provide one", namespace, limitRangeName, limitType, resourceName, maximumRatio.String(), workloadName)
		}
		if !limitFound || limit.IsZero() {
			return fmt.Errorf("LimitRange %s/%s requires a nonzero %s %s limit for max limit-to-request ratio %s, but %s does not provide one", namespace, limitRangeName, limitType, resourceName, maximumRatio.String(), workloadName)
		}
		if runtimeLimitRequestRatioExceeds(request, limit, maximumRatio) {
			return fmt.Errorf("LimitRange %s/%s max %s %s limit-to-request ratio is %s, but %s has limit %s and request %s", namespace, limitRangeName, limitType, resourceName, maximumRatio.String(), workloadName, limit.String(), request.String())
		}
	}
	return nil
}

// runtimeAdmissionComparableValues follows LimitRanger's precision choice so
// this preflight makes the same decision for fractional CPU quantities.
func runtimeAdmissionComparableValues(request, limit, enforced resource.Quantity) (int64, int64, int64) {
	requestValue := request.Value()
	limitValue := limit.Value()
	enforcedValue := enforced.Value()
	if requestValue <= resource.MaxMilliValue && limitValue <= resource.MaxMilliValue && enforcedValue <= resource.MaxMilliValue {
		return request.MilliValue(), limit.MilliValue(), enforced.MilliValue()
	}
	return requestValue, limitValue, enforcedValue
}

func runtimeLimitRequestRatioExceeds(request, limit, maximum resource.Quantity) bool {
	requestValue, limitValue, _ := runtimeAdmissionComparableValues(request, limit, maximum)
	observedRatio := float64(limitValue) / float64(requestValue)
	maximumRatio := float64(maximum.Value())
	if maximum.Value() <= resource.MaxMilliValue {
		observedRatio *= 1000
		maximumRatio = float64(maximum.MilliValue())
	}
	return observedRatio > maximumRatio
}

func aggregateRuntimePodResources(app, init corev1.ResourceList) corev1.ResourceList {
	aggregate := copyRuntimeResourceList(app)
	if aggregate == nil {
		aggregate = corev1.ResourceList{}
	}
	for name, quantity := range init {
		current, found := aggregate[name]
		if !found || quantity.Cmp(current) > 0 {
			aggregate[name] = quantity.DeepCopy()
		}
	}
	return aggregate
}

func (p *RuntimeAdmissionPreflight) checkServiceAccounts(ctx context.Context) error {
	// ServiceAccount admission copies account imagePullSecrets only when the
	// incoming Pod list is empty. An explicit chart list suppresses injection.
	targets := []struct {
		name                    string
		willCreate              bool
		desiredEnforceMountable bool
		secretNames             []string
	}{
		{
			name:                    p.Contract.ControllerServiceAccountName,
			willCreate:              p.Contract.ControllerServiceAccountCreate,
			desiredEnforceMountable: p.Contract.ControllerServiceAccountEnforceMountableSecrets,
			secretNames:             p.Contract.ControllerSecretNames,
		},
	}
	if p.Contract.CertificateRuntimeEnabled {
		targets = append(targets, struct {
			name                    string
			willCreate              bool
			desiredEnforceMountable bool
			secretNames             []string
		}{
			name:        p.Contract.CertificateServiceAccountName,
			willCreate:  true,
			secretNames: p.Contract.CertificateSecretNames,
		})
	}
	for _, target := range targets {
		account, err := p.ServiceAccounts.Get(ctx, target.name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				if target.willCreate {
					// A desired enforce-mountable-secrets setting is part of the
					// immutable chart contract. Static chart verification must
					// render the same Secret and image-pull-secret allowlists.
					if target.desiredEnforceMountable && len(target.secretNames) == 0 {
						return fmt.Errorf("chart-created runtime ServiceAccount %s/%s enables enforce-mountable-secrets without declaring its Pod Secret references", p.Contract.Namespace, target.name)
					}
					continue
				}
				return fmt.Errorf("external runtime ServiceAccount %s/%s does not exist and serviceAccount.create is false", p.Contract.Namespace, target.name)
			}
			return fmt.Errorf("get runtime ServiceAccount %s/%s: %w", p.Contract.Namespace, target.name, err)
		}
		if account == nil {
			return fmt.Errorf("get runtime ServiceAccount %s/%s returned a nil result", p.Contract.Namespace, target.name)
		}
		if account.Name != target.name || (account.Namespace != "" && account.Namespace != p.Contract.Namespace) {
			return fmt.Errorf("runtime ServiceAccount lookup for %s/%s returned %s/%s", p.Contract.Namespace, target.name, account.Namespace, account.Name)
		}
		if len(account.ImagePullSecrets) > runtimeAdmissionMaxImagePullSecrets {
			return fmt.Errorf("runtime ServiceAccount %s/%s has more than %d image pull secrets", p.Contract.Namespace, target.name, runtimeAdmissionMaxImagePullSecrets)
		}
		if len(p.Contract.ImagePullSecrets) == 0 && len(account.ImagePullSecrets) != 0 {
			return fmt.Errorf("runtime ServiceAccount %s/%s has imagePullSecrets while the chart Pod list is empty; ServiceAccount admission would inject them, so configure chart imagePullSecrets explicitly or remove them from the ServiceAccount", p.Contract.Namespace, target.name)
		}
		if serviceAccountEnforcesMountableSecrets(account) {
			if err := validateRuntimeMountableSecrets(p.Contract.Namespace, account, target.secretNames, p.Contract.ImagePullSecrets); err != nil {
				return err
			}
		}
	}
	return nil
}

func serviceAccountEnforcesMountableSecrets(account *corev1.ServiceAccount) bool {
	value, found := account.Annotations[runtimeAdmissionEnforceMountableSecretsAnnotation]
	if !found {
		return false
	}
	enforce, _ := strconv.ParseBool(value)
	return enforce
}

func validateRuntimeMountableSecrets(
	namespace string,
	account *corev1.ServiceAccount,
	secretNames []string,
	imagePullSecrets []corev1.LocalObjectReference,
) error {
	if len(account.Secrets) > runtimeAdmissionMaxSecretReferences {
		return fmt.Errorf("runtime ServiceAccount %s/%s has more than %d mountable Secret references", namespace, account.Name, runtimeAdmissionMaxSecretReferences)
	}
	mountableSecrets := make(map[string]struct{}, len(account.Secrets))
	for _, reference := range account.Secrets {
		mountableSecrets[reference.Name] = struct{}{}
	}
	for _, secretName := range secretNames {
		if _, found := mountableSecrets[secretName]; !found {
			return fmt.Errorf("runtime ServiceAccount %s/%s enforces mountable secrets but does not list Pod Secret %s in secrets", namespace, account.Name, secretName)
		}
	}

	allowedImagePullSecrets := make(map[string]struct{}, len(account.ImagePullSecrets))
	for _, reference := range account.ImagePullSecrets {
		allowedImagePullSecrets[reference.Name] = struct{}{}
	}
	for _, reference := range imagePullSecrets {
		if _, found := allowedImagePullSecrets[reference.Name]; !found {
			return fmt.Errorf("runtime ServiceAccount %s/%s enforces mountable secrets but does not list Pod image pull Secret %s in imagePullSecrets", namespace, account.Name, reference.Name)
		}
	}
	return nil
}

func (p *RuntimeAdmissionPreflight) checkPriorityClass(ctx context.Context) error {
	if p.Contract.PriorityClassName != "" {
		priorityClass, err := p.PriorityClasses.Get(ctx, p.Contract.PriorityClassName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("configured PriorityClass %s does not exist", p.Contract.PriorityClassName)
			}
			return fmt.Errorf("get configured PriorityClass %s: %w", p.Contract.PriorityClassName, err)
		}
		if priorityClass == nil {
			return fmt.Errorf("get configured PriorityClass %s returned a nil result", p.Contract.PriorityClassName)
		}
		if priorityClass.Name != p.Contract.PriorityClassName {
			return fmt.Errorf("PriorityClass lookup for %s returned %s", p.Contract.PriorityClassName, priorityClass.Name)
		}
		if priorityClass.Value != p.Contract.PriorityClassValue {
			return fmt.Errorf("configured PriorityClass %s value is %d, but the runtime Pod contract requires %d", p.Contract.PriorityClassName, priorityClass.Value, p.Contract.PriorityClassValue)
		}
		preemptionPolicy := effectiveRuntimePriorityClassPreemptionPolicy(priorityClass)
		if string(preemptionPolicy) != p.Contract.PriorityClassPreemptionPolicy {
			return fmt.Errorf("configured PriorityClass %s effective preemptionPolicy is %s, but the runtime Pod contract requires %s", p.Contract.PriorityClassName, preemptionPolicy, p.Contract.PriorityClassPreemptionPolicy)
		}
		return nil
	}

	priorityClasses, err := p.listPriorityClasses(ctx)
	if err != nil {
		return err
	}
	globalDefaults := make([]string, 0, 2)
	for index := range priorityClasses {
		if priorityClasses[index].GlobalDefault {
			globalDefaults = append(globalDefaults, priorityClasses[index].Name)
		}
	}
	sort.Strings(globalDefaults)
	switch len(globalDefaults) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("runtime priorityClassName is empty, but global default PriorityClass %s would mutate runtime Pods; configure that class explicitly or remove its global default", globalDefaults[0])
	default:
		return fmt.Errorf("multiple global default PriorityClasses make runtime Pod admission ambiguous: %s", strings.Join(globalDefaults, ", "))
	}
}

func (p *RuntimeAdmissionPreflight) listPriorityClasses(ctx context.Context) ([]schedulingv1.PriorityClass, error) {
	var result []schedulingv1.PriorityClass
	state := newInventoryPageState()
	continueToken := ""
	for {
		page, err := p.PriorityClasses.List(ctx, metav1.ListOptions{Limit: runtimeAdmissionPageSize, Continue: continueToken})
		if err != nil {
			return nil, fmt.Errorf("list global default PriorityClasses: %w", err)
		}
		if page == nil {
			return nil, errors.New("list global default PriorityClasses returned a nil page")
		}
		if err := state.validatePage("PriorityClass", continueToken, page.ListMeta, len(page.Items), runtimeAdmissionPageSize); err != nil {
			return nil, err
		}
		for index := range page.Items {
			priorityClass := &page.Items[index]
			if priorityClass.Namespace != "" || priorityClass.Name == "" {
				return nil, fmt.Errorf("PriorityClass inventory returned namespaced or incomplete object %q", priorityClass.Namespace+"/"+priorityClass.Name)
			}
			if err := state.observeObject("PriorityClass", priorityClass.Namespace, priorityClass.Name, string(priorityClass.UID)); err != nil {
				return nil, err
			}
		}
		result = append(result, page.Items...)
		if page.Continue == "" {
			return result, nil
		}
		continueToken = page.Continue
	}
}

func effectiveRuntimePriorityClassPreemptionPolicy(priorityClass *schedulingv1.PriorityClass) corev1.PreemptionPolicy {
	if priorityClass.PreemptionPolicy == nil {
		return corev1.PreemptLowerPriority
	}
	return *priorityClass.PreemptionPolicy
}

func copyRuntimeResourceList(source corev1.ResourceList) corev1.ResourceList {
	if source == nil {
		return nil
	}
	result := make(corev1.ResourceList, len(source))
	for name, quantity := range source {
		result[name] = quantity.DeepCopy()
	}
	return result
}

func sortedRuntimeResourceNames(values corev1.ResourceList) []corev1.ResourceName {
	names := make([]corev1.ResourceName, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}
