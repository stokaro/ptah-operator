// Package podintent resolves and validates the bounded set of safe PodSpec
// mutations performed by Kubernetes admission and scheduling.
package podintent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/workload"
)

var sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const (
	// SnapshotVersion identifies the mutation rules implemented by this package.
	SnapshotVersion = "v1"

	maxLimitRanges              = 32
	maxResourceKeys             = 64
	maxLimitRangeDefaultEntries = 64
	maxSchedulingItems          = 64
)

// Options binds cluster-configurable built-in admission behavior. Values must
// match the kube-apiserver DefaultTolerationSeconds configuration.
type Options struct {
	DefaultTolerationsEnabled           bool
	DefaultNotReadyTolerationSeconds    int64
	DefaultUnreachableTolerationSeconds int64
	ExtendedResourceTolerationEnabled   bool
	AlwaysPullImagesEnabled             bool
}

// DefaultOptions returns the upstream kube-apiserver defaults.
func DefaultOptions() Options {
	return Options{
		DefaultTolerationsEnabled:           true,
		DefaultNotReadyTolerationSeconds:    300,
		DefaultUnreachableTolerationSeconds: 300,
		ExtendedResourceTolerationEnabled:   false,
		AlwaysPullImagesEnabled:             false,
	}
}

// Validate ensures configured admission values fit the public status bounds.
func (o Options) Validate() error {
	if o.DefaultNotReadyTolerationSeconds < 0 || o.DefaultUnreachableTolerationSeconds < 0 {
		return errors.New("default NoExecute toleration seconds must be nonnegative")
	}
	return nil
}

// Resolve reads only credential-free Kubernetes resources and returns the
// immutable mutation envelope for template. It never reads Secret objects.
func Resolve(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	template *corev1.PodTemplateSpec,
	options Options,
) (*operatorv1alpha1.PodAdmissionSnapshot, error) {
	if reader == nil {
		return nil, errors.New("admission snapshot reader is required")
	}
	if namespace == "" || template == nil {
		return nil, errors.New("admission snapshot namespace and Pod template are required")
	}
	if template.Spec.Resources != nil {
		return nil, errors.New("Pod-level resources are not supported by the admission snapshot")
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}

	templateDigest, err := DigestTemplate(template)
	if err != nil {
		return nil, err
	}
	serviceAccountName := template.Spec.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = "default"
	}
	serviceAccount := &corev1.ServiceAccount{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: serviceAccountName}, serviceAccount); err != nil {
		return nil, fmt.Errorf("resolve ServiceAccount %s/%s: %w", namespace, serviceAccountName, err)
	}
	serviceAccountBinding, err := objectBinding(serviceAccount)
	if err != nil {
		return nil, fmt.Errorf("bind ServiceAccount %s/%s: %w", namespace, serviceAccountName, err)
	}
	if len(serviceAccount.ImagePullSecrets) > maxSchedulingItems {
		return nil, fmt.Errorf("ServiceAccount %s/%s has more than %d image pull secrets", namespace, serviceAccountName, maxSchedulingItems)
	}

	limitRangeList := &corev1.LimitRangeList{}
	if err := reader.List(ctx, limitRangeList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list LimitRanges in %s: %w", namespace, err)
	}
	if len(limitRangeList.Items) > maxLimitRanges {
		return nil, fmt.Errorf("namespace %s has more than %d LimitRanges", namespace, maxLimitRanges)
	}
	limitRanges := make([]operatorv1alpha1.LimitRangeAdmissionSnapshot, 0, len(limitRangeList.Items))
	totalLimitRangeDefaults := 0
	for i := range limitRangeList.Items {
		limitRange := &limitRangeList.Items[i]
		binding, err := objectBinding(limitRange)
		if err != nil {
			return nil, fmt.Errorf("bind LimitRange %s/%s: %w", namespace, limitRange.Name, err)
		}
		defaults := operatorv1alpha1.LimitRangeAdmissionSnapshot{Object: binding}
		containerItems := 0
		for _, item := range limitRange.Spec.Limits {
			if item.Type != corev1.LimitTypeContainer {
				continue
			}
			containerItems++
			if containerItems > 1 {
				return nil, fmt.Errorf("LimitRange %s/%s has more than one Container item", namespace, limitRange.Name)
			}
			defaultRequests, defaultLimits := normalizedContainerDefaults(item)
			for name, quantity := range defaultRequests {
				if defaults.DefaultRequests == nil {
					defaults.DefaultRequests = map[corev1.ResourceName]apiresource.Quantity{}
				}
				defaults.DefaultRequests[name] = quantity.DeepCopy()
			}
			for name, quantity := range defaultLimits {
				if defaults.DefaultLimits == nil {
					defaults.DefaultLimits = map[corev1.ResourceName]apiresource.Quantity{}
				}
				defaults.DefaultLimits[name] = quantity.DeepCopy()
			}
		}
		if len(defaults.DefaultRequests) > maxResourceKeys || len(defaults.DefaultLimits) > maxResourceKeys {
			return nil, fmt.Errorf("LimitRange %s/%s has more than %d container default resource keys", namespace, limitRange.Name, maxResourceKeys)
		}
		totalLimitRangeDefaults += len(defaults.DefaultRequests) + len(defaults.DefaultLimits)
		if totalLimitRangeDefaults > maxLimitRangeDefaultEntries {
			return nil, fmt.Errorf("namespace %s has more than %d aggregate LimitRange default entries", namespace, maxLimitRangeDefaultEntries)
		}
		limitRanges = append(limitRanges, defaults)
	}
	sort.Slice(limitRanges, func(i, j int) bool {
		return bindingOrder(limitRanges[i].Object) < bindingOrder(limitRanges[j].Object)
	})
	if err := validateUnambiguousLimitRangeDefaults(&template.Spec, limitRanges); err != nil {
		return nil, err
	}

	snapshot := &operatorv1alpha1.PodAdmissionSnapshot{
		Version:        SnapshotVersion,
		TemplateDigest: templateDigest,
		ServiceAccount: operatorv1alpha1.ServiceAccountAdmissionSnapshot{
			Object:           serviceAccountBinding,
			ImagePullSecrets: append([]corev1.LocalObjectReference(nil), serviceAccount.ImagePullSecrets...),
		},
		LimitRanges:                         limitRanges,
		DefaultTolerationsEnabled:           options.DefaultTolerationsEnabled,
		DefaultNotReadyTolerationSeconds:    options.DefaultNotReadyTolerationSeconds,
		DefaultUnreachableTolerationSeconds: options.DefaultUnreachableTolerationSeconds,
		ExtendedResourceTolerationEnabled:   options.ExtendedResourceTolerationEnabled,
		AlwaysPullImagesEnabled:             options.AlwaysPullImagesEnabled,
	}
	if err := resolveRuntimeClass(ctx, reader, &template.Spec, snapshot); err != nil {
		return nil, err
	}
	if err := resolvePriorityClass(ctx, reader, &template.Spec, snapshot); err != nil {
		return nil, err
	}
	if err := bindDigest(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func resolveRuntimeClass(
	ctx context.Context,
	reader client.Reader,
	template *corev1.PodSpec,
	snapshot *operatorv1alpha1.PodAdmissionSnapshot,
) error {
	if template.RuntimeClassName == nil {
		return nil
	}
	if *template.RuntimeClassName == "" {
		return errors.New("runtimeClassName cannot be empty")
	}
	runtimeClass := &nodev1.RuntimeClass{}
	if err := reader.Get(ctx, client.ObjectKey{Name: *template.RuntimeClassName}, runtimeClass); err != nil {
		return fmt.Errorf("resolve RuntimeClass %s: %w", *template.RuntimeClassName, err)
	}
	binding, err := objectBinding(runtimeClass)
	if err != nil {
		return fmt.Errorf("bind RuntimeClass %s: %w", runtimeClass.Name, err)
	}
	resolved := &operatorv1alpha1.RuntimeClassAdmissionSnapshot{
		Object:  binding,
		Handler: runtimeClass.Handler,
	}
	if runtimeClass.Overhead != nil {
		resolved.OverheadDefined = true
		resolved.Overhead = copyResourceList(runtimeClass.Overhead.PodFixed)
	}
	if runtimeClass.Scheduling != nil {
		resolved.NodeSelector = copyStringMap(runtimeClass.Scheduling.NodeSelector)
		resolved.Tolerations = append([]corev1.Toleration(nil), runtimeClass.Scheduling.Tolerations...)
	}
	if len(resolved.Overhead) > maxResourceKeys || len(resolved.NodeSelector) > maxSchedulingItems || len(resolved.Tolerations) > maxSchedulingItems {
		return fmt.Errorf("RuntimeClass %s exceeds the bounded admission snapshot", runtimeClass.Name)
	}
	for key, value := range resolved.NodeSelector {
		if current, exists := template.NodeSelector[key]; exists && current != value {
			return fmt.Errorf("RuntimeClass %s node selector %s conflicts with the Job template", runtimeClass.Name, key)
		}
	}
	snapshot.RuntimeClass = resolved
	return nil
}

func resolvePriorityClass(
	ctx context.Context,
	reader client.Reader,
	template *corev1.PodSpec,
	snapshot *operatorv1alpha1.PodAdmissionSnapshot,
) error {
	var selected *schedulingv1.PriorityClass
	if template.PriorityClassName != "" {
		selected = &schedulingv1.PriorityClass{}
		if err := reader.Get(ctx, client.ObjectKey{Name: template.PriorityClassName}, selected); err != nil {
			return fmt.Errorf("resolve PriorityClass %s: %w", template.PriorityClassName, err)
		}
	} else {
		classes := &schedulingv1.PriorityClassList{}
		if err := reader.List(ctx, classes); err != nil {
			return fmt.Errorf("list global default PriorityClasses: %w", err)
		}
		for i := range classes.Items {
			candidate := &classes.Items[i]
			if !candidate.GlobalDefault {
				continue
			}
			if selected != nil {
				return fmt.Errorf("multiple global default PriorityClasses make Pod admission ambiguous")
			}
			selected = candidate
		}
	}
	if selected == nil {
		policy := corev1.PreemptLowerPriority
		snapshot.PriorityClass = operatorv1alpha1.PriorityClassAdmissionSnapshot{
			Value: 0, PreemptionPolicy: &policy,
		}
		return nil
	}
	binding, err := objectBinding(selected)
	if err != nil {
		return fmt.Errorf("bind PriorityClass %s: %w", selected.Name, err)
	}
	snapshot.PriorityClass = operatorv1alpha1.PriorityClassAdmissionSnapshot{
		Object:           &binding,
		Name:             selected.Name,
		Value:            selected.Value,
		PreemptionPolicy: copyPreemptionPolicy(selected.PreemptionPolicy),
	}
	return nil
}

// ValidateSnapshot verifies the canonical digest and structural bounds of a
// persisted admission snapshot without consulting current cluster resources.
func ValidateSnapshot(snapshot *operatorv1alpha1.PodAdmissionSnapshot) error {
	if snapshot == nil {
		return errors.New("Pod admission snapshot is missing")
	}
	if snapshot.Version != SnapshotVersion {
		return fmt.Errorf("unsupported Pod admission snapshot version %q", snapshot.Version)
	}
	if !sha256DigestPattern.MatchString(snapshot.TemplateDigest) {
		return errors.New("Pod admission template digest is invalid")
	}
	if err := validateBinding(snapshot.ServiceAccount.Object); err != nil {
		return fmt.Errorf("invalid ServiceAccount binding: %w", err)
	}
	if len(snapshot.ServiceAccount.ImagePullSecrets) > maxSchedulingItems {
		return errors.New("Pod admission snapshot has too many image pull secrets")
	}
	if len(snapshot.LimitRanges) > maxLimitRanges {
		return errors.New("Pod admission snapshot has too many LimitRanges")
	}
	last := ""
	totalLimitRangeDefaults := 0
	for i, limitRange := range snapshot.LimitRanges {
		if err := validateBinding(limitRange.Object); err != nil {
			return fmt.Errorf("invalid LimitRange binding at index %d: %w", i, err)
		}
		order := bindingOrder(limitRange.Object)
		if i > 0 && order <= last {
			return errors.New("Pod admission LimitRange bindings are not uniquely sorted")
		}
		last = order
		if err := validateResourceList(limitRange.DefaultRequests); err != nil {
			return fmt.Errorf("invalid LimitRange default requests at index %d: %w", i, err)
		}
		if err := validateResourceList(limitRange.DefaultLimits); err != nil {
			return fmt.Errorf("invalid LimitRange default limits at index %d: %w", i, err)
		}
		totalLimitRangeDefaults += len(limitRange.DefaultRequests) + len(limitRange.DefaultLimits)
		if totalLimitRangeDefaults > maxLimitRangeDefaultEntries {
			return errors.New("Pod admission snapshot has too many aggregate LimitRange default entries")
		}
	}
	if runtimeClass := snapshot.RuntimeClass; runtimeClass != nil {
		if err := validateBinding(runtimeClass.Object); err != nil {
			return fmt.Errorf("invalid RuntimeClass binding: %w", err)
		}
		if runtimeClass.Handler == "" || len(runtimeClass.Handler) > 63 {
			return errors.New("Pod admission RuntimeClass handler is invalid")
		}
		if err := validateResourceList(runtimeClass.Overhead); err != nil {
			return fmt.Errorf("invalid RuntimeClass overhead: %w", err)
		}
		if len(runtimeClass.NodeSelector) > maxSchedulingItems || len(runtimeClass.Tolerations) > maxSchedulingItems {
			return errors.New("Pod admission RuntimeClass scheduling exceeds its bounds")
		}
	}
	priority := snapshot.PriorityClass
	if priority.Object != nil {
		if err := validateBinding(*priority.Object); err != nil {
			return fmt.Errorf("invalid PriorityClass binding: %w", err)
		}
		if priority.Name != priority.Object.Name {
			return errors.New("Pod admission PriorityClass name does not match its binding")
		}
	} else if priority.Name != "" || priority.Value != 0 {
		return errors.New("Pod admission default priority is not the Kubernetes zero default")
	}
	if priority.PreemptionPolicy != nil &&
		*priority.PreemptionPolicy != corev1.PreemptLowerPriority && *priority.PreemptionPolicy != corev1.PreemptNever {
		return errors.New("Pod admission preemption policy is invalid")
	}
	if err := (Options{
		DefaultTolerationsEnabled:           snapshot.DefaultTolerationsEnabled,
		DefaultNotReadyTolerationSeconds:    snapshot.DefaultNotReadyTolerationSeconds,
		DefaultUnreachableTolerationSeconds: snapshot.DefaultUnreachableTolerationSeconds,
		ExtendedResourceTolerationEnabled:   snapshot.ExtendedResourceTolerationEnabled,
		AlwaysPullImagesEnabled:             snapshot.AlwaysPullImagesEnabled,
	}).Validate(); err != nil {
		return fmt.Errorf("invalid default toleration snapshot: %w", err)
	}

	stored := snapshot.Digest
	copy := *snapshot
	copy.Digest = ""
	digest, err := fingerprint.DigestCanonicalJSON(copy)
	if err != nil {
		return fmt.Errorf("digest Pod admission snapshot: %w", err)
	}
	if stored != digest {
		return errors.New("Pod admission snapshot digest does not match its canonical contents")
	}
	return nil
}

// ValidatePodSpec accepts only the snapshotted built-in admission mutations
// and scheduler-owned node assignment. Every other PodSpec field remains an
// exact match for the immutable Job template.
func ValidatePodSpec(
	actualSpec *corev1.PodSpec,
	template *corev1.PodTemplateSpec,
	snapshot *operatorv1alpha1.PodAdmissionSnapshot,
) error {
	if actualSpec == nil || template == nil {
		return errors.New("actual and template Pod specs are required")
	}
	if err := ValidateSnapshot(snapshot); err != nil {
		return err
	}
	templateDigest, err := DigestTemplate(template)
	if err != nil {
		return err
	}
	if templateDigest != snapshot.TemplateDigest {
		return errors.New("Job Pod template does not match the persisted admission snapshot")
	}

	actualPod := &corev1.Pod{Spec: *actualSpec.DeepCopy()}
	expectedPod := &corev1.Pod{Spec: *template.Spec.DeepCopy()}
	clientgoscheme.Scheme.Default(actualPod)
	clientgoscheme.Scheme.Default(expectedPod)
	actual := &actualPod.Spec
	expected := &expectedPod.Spec
	extendedTolerations := extendedResourceTolerations(actual.Containers, actual.InitContainers)

	if err := validateServiceAccount(actual, expected, snapshot); err != nil {
		return err
	}
	allowedRequests, allowedLimits := allowedLimitRangeDefaults(snapshot.LimitRanges)
	if err := validateContainers(
		actual.Containers,
		expected.Containers,
		allowedRequests,
		allowedLimits,
		snapshot.AlwaysPullImagesEnabled,
	); err != nil {
		return fmt.Errorf("containers: %w", err)
	}
	if err := validateContainers(
		actual.InitContainers,
		expected.InitContainers,
		allowedRequests,
		allowedLimits,
		snapshot.AlwaysPullImagesEnabled,
	); err != nil {
		return fmt.Errorf("init containers: %w", err)
	}
	if err := validateRuntimeClass(actual, expected, snapshot.RuntimeClass); err != nil {
		return err
	}
	if err := validatePriority(actual, expected, snapshot.PriorityClass); err != nil {
		return err
	}
	if err := validateTolerations(actual, expected, snapshot, extendedTolerations); err != nil {
		return err
	}

	// NodeName is assigned by the scheduler after admission. No other
	// scheduler-owned field in PodSpec is mutable for this workload.
	actual.NodeName = expected.NodeName
	if !apiequality.Semantic.DeepEqual(actual, expected) {
		return errors.New("Pod workload spec differs outside the resolved admission envelope")
	}
	return nil
}

func validateServiceAccount(
	actual, expected *corev1.PodSpec,
	snapshot *operatorv1alpha1.PodAdmissionSnapshot,
) error {
	wantName := expected.ServiceAccountName
	if wantName == "" {
		wantName = "default"
	}
	if snapshot.ServiceAccount.Object.Name != wantName || actual.ServiceAccountName != wantName {
		return errors.New("Pod ServiceAccount does not match the admission snapshot")
	}
	// Kubernetes 1.37 writes the deprecated alias alongside the resolved
	// serviceAccountName, including for an explicitly named ServiceAccount.
	// Earlier supported releases leave the alias untouched.
	deprecatedAliasMatches := actual.DeprecatedServiceAccount == expected.DeprecatedServiceAccount ||
		actual.DeprecatedServiceAccount == wantName
	if !deprecatedAliasMatches {
		return errors.New("Pod deprecated ServiceAccount alias changed")
	}
	wantSecrets := expected.ImagePullSecrets
	if len(wantSecrets) == 0 {
		wantSecrets = snapshot.ServiceAccount.ImagePullSecrets
	}
	if !apiequality.Semantic.DeepEqual(actual.ImagePullSecrets, wantSecrets) {
		return errors.New("Pod imagePullSecrets do not match the admitted ServiceAccount binding")
	}
	actual.ServiceAccountName = expected.ServiceAccountName
	actual.DeprecatedServiceAccount = expected.DeprecatedServiceAccount
	actual.ImagePullSecrets = append([]corev1.LocalObjectReference(nil), expected.ImagePullSecrets...)
	return nil
}

func validateContainers(
	actual []corev1.Container,
	expected []corev1.Container,
	allowedRequests map[corev1.ResourceName][]apiresource.Quantity,
	allowedLimits map[corev1.ResourceName][]apiresource.Quantity,
	alwaysPullImagesEnabled bool,
) error {
	if len(actual) != len(expected) {
		return errors.New("container count changed")
	}
	for i := range expected {
		if actual[i].Name != expected[i].Name {
			return fmt.Errorf("container %d identity changed", i)
		}
		if err := validateResourceRequirements(actual[i].Resources.Limits, expected[i].Resources.Limits, allowedLimits); err != nil {
			return fmt.Errorf("container %s limits: %w", expected[i].Name, err)
		}
		if err := validateResourceRequests(
			actual[i].Resources.Requests,
			expected[i].Resources.Requests,
			actual[i].Resources.Limits,
			allowedRequests,
		); err != nil {
			return fmt.Errorf("container %s requests: %w", expected[i].Name, err)
		}
		actual[i].Resources.Requests = copyResourceList(expected[i].Resources.Requests)
		actual[i].Resources.Limits = copyResourceList(expected[i].Resources.Limits)
		wantPullPolicy := expected[i].ImagePullPolicy
		if alwaysPullImagesEnabled {
			wantPullPolicy = corev1.PullAlways
		}
		if actual[i].ImagePullPolicy != wantPullPolicy {
			return fmt.Errorf("container %s image pull policy does not match the admission snapshot", expected[i].Name)
		}
		actual[i].ImagePullPolicy = expected[i].ImagePullPolicy
	}
	return nil
}

func validateResourceRequests(
	actual corev1.ResourceList,
	expected corev1.ResourceList,
	actualLimits corev1.ResourceList,
	allowedDefaults map[corev1.ResourceName][]apiresource.Quantity,
) error {
	for name, want := range expected {
		got, ok := actual[name]
		if !ok || got.Cmp(want) != 0 {
			return fmt.Errorf("configured resource %s changed", name)
		}
	}
	for name, got := range actual {
		if _, configured := expected[name]; configured {
			continue
		}
		if quantityAllowed(got, allowedDefaults[name]) {
			continue
		}
		limit, hasLimit := actualLimits[name]
		if !hasLimit || got.Cmp(limit) != 0 {
			return fmt.Errorf("resource %s was neither an exact snapshotted LimitRange default nor its exact admitted limit", name)
		}
	}
	for name, quantities := range allowedDefaults {
		if _, configured := expected[name]; configured || len(quantities) == 0 {
			continue
		}
		if _, admitted := actual[name]; !admitted {
			return fmt.Errorf("snapshotted LimitRange default for %s was not admitted", name)
		}
	}
	for name := range actualLimits {
		if _, configured := expected[name]; configured {
			continue
		}
		if _, admitted := actual[name]; !admitted {
			return fmt.Errorf("API-defaulted request for admitted limit %s is missing", name)
		}
	}
	return nil
}

func validateResourceRequirements(
	actual corev1.ResourceList,
	expected corev1.ResourceList,
	allowed map[corev1.ResourceName][]apiresource.Quantity,
) error {
	for name, want := range expected {
		got, ok := actual[name]
		if !ok || got.Cmp(want) != 0 {
			return fmt.Errorf("configured resource %s changed", name)
		}
	}
	for name, got := range actual {
		if _, configured := expected[name]; configured {
			continue
		}
		if !quantityAllowed(got, allowed[name]) {
			return fmt.Errorf("resource %s was not an exact snapshotted LimitRange default", name)
		}
	}
	for name, quantities := range allowed {
		if _, configured := expected[name]; configured || len(quantities) == 0 {
			continue
		}
		if _, admitted := actual[name]; !admitted {
			return fmt.Errorf("snapshotted LimitRange default for %s was not admitted", name)
		}
	}
	return nil
}

func validateRuntimeClass(
	actual, expected *corev1.PodSpec,
	snapshot *operatorv1alpha1.RuntimeClassAdmissionSnapshot,
) error {
	if expected.RuntimeClassName == nil {
		if snapshot != nil || actual.RuntimeClassName != nil || actual.Overhead != nil ||
			!apiequality.Semantic.DeepEqual(actual.NodeSelector, expected.NodeSelector) {
			return errors.New("Pod has an unsnapshotted RuntimeClass mutation")
		}
		return nil
	}
	if snapshot == nil || snapshot.Object.Name != *expected.RuntimeClassName || actual.RuntimeClassName == nil ||
		*actual.RuntimeClassName != *expected.RuntimeClassName {
		return errors.New("Pod RuntimeClass does not match the admission snapshot")
	}
	wantSelector := copyStringMap(expected.NodeSelector)
	if wantSelector == nil && len(snapshot.NodeSelector) != 0 {
		wantSelector = map[string]string{}
	}
	for key, value := range snapshot.NodeSelector {
		if current, exists := wantSelector[key]; exists && current != value {
			return fmt.Errorf("RuntimeClass node selector %s conflicts with the Job template", key)
		}
		wantSelector[key] = value
	}
	if !apiequality.Semantic.DeepEqual(actual.NodeSelector, wantSelector) {
		return errors.New("Pod nodeSelector does not match the snapshotted RuntimeClass scheduling")
	}
	if snapshot.OverheadDefined {
		if !resourceListEqual(actual.Overhead, snapshot.Overhead) {
			return errors.New("Pod overhead does not match the snapshotted RuntimeClass")
		}
	} else if actual.Overhead != nil {
		return errors.New("Pod has overhead absent from the snapshotted RuntimeClass")
	}
	actual.NodeSelector = copyStringMap(expected.NodeSelector)
	actual.Overhead = copyResourceList(expected.Overhead)
	return nil
}

func validatePriority(
	actual, expected *corev1.PodSpec,
	snapshot operatorv1alpha1.PriorityClassAdmissionSnapshot,
) error {
	if expected.PriorityClassName != "" && snapshot.Name != expected.PriorityClassName {
		return errors.New("PriorityClass snapshot does not match the Job template")
	}
	if actual.PriorityClassName != snapshot.Name || actual.Priority == nil || *actual.Priority != snapshot.Value ||
		!apiequality.Semantic.DeepEqual(actual.PreemptionPolicy, snapshot.PreemptionPolicy) {
		return errors.New("Pod priority does not match the admission snapshot")
	}
	actual.PriorityClassName = expected.PriorityClassName
	actual.Priority = expected.Priority
	actual.PreemptionPolicy = expected.PreemptionPolicy
	return nil
}

func validateTolerations(
	actual, expected *corev1.PodSpec,
	snapshot *operatorv1alpha1.PodAdmissionSnapshot,
	extended []corev1.Toleration,
) error {
	runtimeTolerations := []corev1.Toleration(nil)
	if snapshot.RuntimeClass != nil {
		runtimeTolerations = snapshot.RuntimeClass.Tolerations
	}
	candidate := append([]corev1.Toleration(nil), expected.Tolerations...)
	if snapshot.DefaultTolerationsEnabled {
		notReady := snapshot.DefaultNotReadyTolerationSeconds
		if !toleratesNoExecuteKey(candidate, "node.kubernetes.io/not-ready") {
			candidate = append(candidate, corev1.Toleration{
				Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists,
				Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &notReady,
			})
		}
		unreachable := snapshot.DefaultUnreachableTolerationSeconds
		if !toleratesNoExecuteKey(candidate, "node.kubernetes.io/unreachable") {
			candidate = append(candidate, corev1.Toleration{
				Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists,
				Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &unreachable,
			})
		}
	}
	if snapshot.ExtendedResourceTolerationEnabled {
		for i := range extended {
			candidate = addOrUpdateToleration(candidate, extended[i])
		}
	}
	candidate = mergeTolerations(candidate, runtimeTolerations)
	if apiequality.Semantic.DeepEqual(actual.Tolerations, candidate) {
		actual.Tolerations = append([]corev1.Toleration(nil), expected.Tolerations...)
		return nil
	}
	return errors.New("Pod tolerations are outside default, extended-resource, and RuntimeClass admission mutations")
}

func extendedResourceTolerations(containers, initContainers []corev1.Container) []corev1.Toleration {
	names := map[string]struct{}{}
	for _, container := range append(append([]corev1.Container(nil), containers...), initContainers...) {
		for name := range container.Resources.Requests {
			if isExtendedResourceName(name) {
				names[string(name)] = struct{}{}
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	result := make([]corev1.Toleration, 0, len(ordered))
	for _, name := range ordered {
		result = append(result, corev1.Toleration{
			Key: name, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
		})
	}
	return result
}

func isExtendedResourceName(name corev1.ResourceName) bool {
	value := string(name)
	if !strings.Contains(value, "/") || strings.Contains(value, corev1.ResourceDefaultNamespacePrefix) {
		return false
	}
	return len(k8svalidation.IsQualifiedName(corev1.DefaultResourceRequestsPrefix+value)) == 0
}

func addOrUpdateToleration(values []corev1.Toleration, wanted corev1.Toleration) []corev1.Toleration {
	result := make([]corev1.Toleration, 0, len(values)+1)
	updated := false
	for _, current := range values {
		if sameTolerationIdentity(current, wanted) {
			if apiequality.Semantic.DeepEqual(current, wanted) {
				return values
			}
			result = append(result, wanted)
			updated = true
			continue
		}
		result = append(result, current)
	}
	if !updated {
		result = append(result, wanted)
	}
	return result
}

func sameTolerationIdentity(left, right corev1.Toleration) bool {
	return left.Key == right.Key && left.Effect == right.Effect && left.Operator == right.Operator && left.Value == right.Value
}

func toleratesNoExecuteKey(values []corev1.Toleration, key string) bool {
	for _, toleration := range values {
		if (toleration.Key == key || toleration.Key == "") &&
			(toleration.Effect == corev1.TaintEffectNoExecute || toleration.Effect == "") {
			return true
		}
	}
	return false
}

func mergeTolerations(first, second []corev1.Toleration) []corev1.Toleration {
	all := append(append([]corev1.Toleration(nil), first...), second...)
	if len(all) == 0 {
		return nil
	}
	merged := make([]corev1.Toleration, 0, len(all))
	for i, toleration := range all {
		redundant := false
		for _, existing := range merged {
			if tolerationSuperset(existing, toleration) {
				redundant = true
				break
			}
		}
		if redundant {
			continue
		}
		for _, later := range all[i+1:] {
			if !apiequality.Semantic.DeepEqual(toleration, later) && tolerationSuperset(later, toleration) {
				redundant = true
				break
			}
		}
		if !redundant {
			merged = append(merged, toleration)
		}
	}
	return merged
}

func tolerationSuperset(superset, value corev1.Toleration) bool {
	if apiequality.Semantic.DeepEqual(superset, value) {
		return true
	}
	if value.Key != superset.Key && (superset.Key != "" || superset.Operator != corev1.TolerationOpExists) {
		return false
	}
	if value.Effect != superset.Effect && superset.Effect != "" {
		return false
	}
	if superset.Effect == corev1.TaintEffectNoExecute && superset.TolerationSeconds != nil {
		if value.TolerationSeconds == nil || *value.TolerationSeconds > *superset.TolerationSeconds {
			return false
		}
	}
	switch superset.Operator {
	case corev1.TolerationOpEqual, "":
		return value.Operator == corev1.TolerationOpEqual && value.Value == superset.Value
	case corev1.TolerationOpExists:
		return true
	default:
		return false
	}
}

func allowedLimitRangeDefaults(
	limitRanges []operatorv1alpha1.LimitRangeAdmissionSnapshot,
) (map[corev1.ResourceName][]apiresource.Quantity, map[corev1.ResourceName][]apiresource.Quantity) {
	requests := map[corev1.ResourceName][]apiresource.Quantity{}
	limits := map[corev1.ResourceName][]apiresource.Quantity{}
	for _, limitRange := range limitRanges {
		appendUniqueQuantities(requests, limitRange.DefaultRequests)
		appendUniqueQuantities(limits, limitRange.DefaultLimits)
	}
	return requests, limits
}

// normalizedContainerDefaults mirrors the API defaulting applied to a
// Container LimitRange item. In particular, default limits derive from max,
// and default requests derive from the resulting default limit before min is
// considered. API reads normally already contain these values; repeating the
// transformation makes the snapshot contract explicit and idempotent.
func normalizedContainerDefaults(item corev1.LimitRangeItem) (corev1.ResourceList, corev1.ResourceList) {
	requests := copyResourceList(item.DefaultRequest)
	limits := copyResourceList(item.Default)
	if limits == nil {
		limits = corev1.ResourceList{}
	}
	if requests == nil {
		requests = corev1.ResourceList{}
	}
	for name, quantity := range item.Max {
		if _, exists := limits[name]; !exists {
			limits[name] = quantity.DeepCopy()
		}
	}
	for name, quantity := range limits {
		if _, exists := requests[name]; !exists {
			requests[name] = quantity.DeepCopy()
		}
	}
	for name, quantity := range item.Min {
		if _, exists := requests[name]; !exists {
			requests[name] = quantity.DeepCopy()
		}
	}
	if len(requests) == 0 {
		requests = nil
	}
	if len(limits) == 0 {
		limits = nil
	}
	return requests, limits
}

func validateUnambiguousLimitRangeDefaults(
	template *corev1.PodSpec,
	limitRanges []operatorv1alpha1.LimitRangeAdmissionSnapshot,
) error {
	requests, limits := allowedLimitRangeDefaults(limitRanges)
	containers := append(append([]corev1.Container(nil), template.Containers...), template.InitContainers...)
	for name, candidates := range requests {
		if len(candidates) < 2 {
			continue
		}
		for _, container := range containers {
			if _, configured := container.Resources.Requests[name]; !configured {
				return fmt.Errorf("conflicting LimitRange default requests for unset resource %s", name)
			}
		}
	}
	for name, candidates := range limits {
		if len(candidates) < 2 {
			continue
		}
		for _, container := range containers {
			if _, configured := container.Resources.Limits[name]; !configured {
				return fmt.Errorf("conflicting LimitRange default limits for unset resource %s", name)
			}
		}
	}
	return nil
}

func appendUniqueQuantities(
	destination map[corev1.ResourceName][]apiresource.Quantity,
	values map[corev1.ResourceName]apiresource.Quantity,
) {
	for name, quantity := range values {
		if quantityAllowed(quantity, destination[name]) {
			continue
		}
		destination[name] = append(destination[name], quantity.DeepCopy())
	}
}

func quantityAllowed(candidate apiresource.Quantity, allowed []apiresource.Quantity) bool {
	for _, quantity := range allowed {
		if candidate.Cmp(quantity) == 0 {
			return true
		}
	}
	return false
}

func resourceListEqual(left corev1.ResourceList, right map[corev1.ResourceName]apiresource.Quantity) bool {
	if len(left) != len(right) {
		return false
	}
	for name, quantity := range left {
		other, ok := right[name]
		if !ok || quantity.Cmp(other) != 0 {
			return false
		}
	}
	return true
}

func validateResourceList(resources map[corev1.ResourceName]apiresource.Quantity) error {
	if len(resources) > maxResourceKeys {
		return fmt.Errorf("resource list has more than %d keys", maxResourceKeys)
	}
	for name, quantity := range resources {
		if name == "" || quantity.Sign() < 0 {
			return errors.New("resource list has an invalid name or negative quantity")
		}
	}
	return nil
}

func objectBinding(object metav1.Object) (operatorv1alpha1.AdmissionObjectBinding, error) {
	binding := operatorv1alpha1.AdmissionObjectBinding{
		Name: object.GetName(), UID: string(object.GetUID()), ResourceVersion: object.GetResourceVersion(),
	}
	return binding, validateBinding(binding)
}

func validateBinding(binding operatorv1alpha1.AdmissionObjectBinding) error {
	if binding.Name == "" || len(binding.Name) > 253 || binding.UID == "" || len(binding.UID) > 128 ||
		binding.ResourceVersion == "" || len(binding.ResourceVersion) > 128 {
		return errors.New("object name, UID, or resourceVersion is missing or too large")
	}
	return nil
}

// DigestTemplate returns the canonical pre-admission identity bound into a
// snapshot. It applies Kubernetes API defaults and omits only the
// self-referential snapshot-digest annotation plus the four exact Job identity
// labels generated by the API server. The webhook validates those labels
// separately against the current Job name and UID.
func DigestTemplate(template *corev1.PodTemplateSpec) (string, error) {
	if template == nil {
		return "", errors.New("Job Pod template is required")
	}
	canonical := template.DeepCopy()
	delete(canonical.Annotations, workload.AnnotationAdmissionSnapshotDigest)
	for _, key := range []string{
		batchv1.ControllerUidLabel,
		batchv1.JobNameLabel,
		"controller-uid",
		"job-name",
	} {
		delete(canonical.Labels, key)
	}
	serviceAccountName := canonical.Spec.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = "default"
	}
	// Kubernetes 1.37 synchronizes the deprecated alias while older supported
	// releases leave it empty. Both stored Job templates have the same effective
	// ServiceAccount and therefore must retain one version-independent digest.
	if canonical.Spec.DeprecatedServiceAccount == serviceAccountName {
		canonical.Spec.DeprecatedServiceAccount = ""
	}
	if len(canonical.Annotations) == 0 {
		canonical.Annotations = nil
	}
	if len(canonical.Labels) == 0 {
		canonical.Labels = nil
	}

	defaulted := &corev1.Pod{ObjectMeta: *canonical.ObjectMeta.DeepCopy(), Spec: *canonical.Spec.DeepCopy()}
	clientgoscheme.Scheme.Default(defaulted)
	canonical.ObjectMeta = *defaulted.ObjectMeta.DeepCopy()
	canonical.Spec = *defaulted.Spec.DeepCopy()
	digest, err := fingerprint.DigestCanonicalJSON(canonical)
	if err != nil {
		return "", fmt.Errorf("digest canonical Job Pod template: %w", err)
	}
	return digest, nil
}

func bindDigest(snapshot *operatorv1alpha1.PodAdmissionSnapshot) error {
	snapshot.Digest = ""
	digest, err := fingerprint.DigestCanonicalJSON(*snapshot)
	if err != nil {
		return fmt.Errorf("digest Pod admission snapshot: %w", err)
	}
	snapshot.Digest = digest
	return nil
}

func bindingOrder(binding operatorv1alpha1.AdmissionObjectBinding) string {
	return binding.Name + "\x00" + binding.UID + "\x00" + binding.ResourceVersion
}

func copyResourceList(input corev1.ResourceList) corev1.ResourceList {
	if input == nil {
		return nil
	}
	result := make(corev1.ResourceList, len(input))
	for name, quantity := range input {
		result[name] = quantity.DeepCopy()
	}
	return result
}

func copyStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func copyPreemptionPolicy(input *corev1.PreemptionPolicy) *corev1.PreemptionPolicy {
	if input == nil {
		return nil
	}
	copy := *input
	return &copy
}
