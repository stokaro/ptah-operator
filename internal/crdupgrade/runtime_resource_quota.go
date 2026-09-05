package crdupgrade

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilnet "k8s.io/apimachinery/pkg/util/net"
)

const (
	// A page size limits individual API responses without limiting the total
	// namespace inventory. Continue tokens are followed to exhaustion so large
	// namespaces remain supported.
	runtimeResourceQuotaPageSize int64 = 500

	runtimeResourceQuotaObjectCountPods corev1.ResourceName = "count/pods"
)

// RuntimeResourceQuotaLister is the namespaced ResourceQuota API used by the
// post-Recreate capacity preflight. A typed core ResourceQuota client
// implements it.
type RuntimeResourceQuotaLister interface {
	List(context.Context, metav1.ListOptions) (*corev1.ResourceQuotaList, error)
}

// RuntimeResourceQuotaPodLister is the namespace-wide Pod API used to remove
// the exact old protected-runtime contribution from ResourceQuota status.
type RuntimeResourceQuotaPodLister interface {
	List(context.Context, metav1.ListOptions) (*corev1.PodList, error)
}

// RuntimeResourceQuotaPreflight verifies that the two runtime Deployments fit
// every synchronized Pod ResourceQuota at a stable observation after Recreate
// removes their old Pods. The observation does not reserve capacity against
// unrelated writers. Mutable labels alone never prove that a Pod may be
// subtracted.
type RuntimeResourceQuotaPreflight struct {
	ResourceQuotas            RuntimeResourceQuotaLister
	Pods                      RuntimeResourceQuotaPodLister
	Contract                  RuntimeAdmissionContract
	ControllerReplicas        int32
	ReleaseName               string
	ReleaseNamespace          string
	ControllerDeploymentName  string
	CertificateDeploymentName string
}

// NewRuntimeResourceQuotaPreflight constructs a read-only post-Recreate
// quota projection. The candidate runtime contract is intentionally fixed to
// non-terminating Pods without cross-namespace affinity; a future configurable
// contract for either property must extend this preflight explicitly.
func NewRuntimeResourceQuotaPreflight(
	contract RuntimeAdmissionContract,
	controllerReplicas int32,
	releaseName string,
	releaseNamespace string,
	controllerDeploymentName string,
	certificateDeploymentName string,
	resourceQuotas RuntimeResourceQuotaLister,
	pods RuntimeResourceQuotaPodLister,
) *RuntimeResourceQuotaPreflight {
	contract.CommonInitContainerResources = *contract.CommonInitContainerResources.DeepCopy()
	contract.ControllerContainerResources = *contract.ControllerContainerResources.DeepCopy()
	contract.CertificateContainerResources = *contract.CertificateContainerResources.DeepCopy()
	contract.ImagePullSecrets = append([]corev1.LocalObjectReference(nil), contract.ImagePullSecrets...)
	contract.ControllerSecretNames = append([]string(nil), contract.ControllerSecretNames...)
	contract.CertificateSecretNames = append([]string(nil), contract.CertificateSecretNames...)
	return &RuntimeResourceQuotaPreflight{
		ResourceQuotas:            resourceQuotas,
		Pods:                      pods,
		Contract:                  contract,
		ControllerReplicas:        controllerReplicas,
		ReleaseName:               releaseName,
		ReleaseNamespace:          releaseNamespace,
		ControllerDeploymentName:  controllerDeploymentName,
		CertificateDeploymentName: certificateDeploymentName,
	}
}

// Check lists a complete namespace snapshot and evaluates each ResourceQuota
// as status.used - old protected runtime Pods + candidate runtime Pods. The
// caller must first run WorkloadInventory.VerifyRuntimeBeforeQuiesce while the
// parent-workload admission guards are active; a Pod list alone cannot resolve
// the ReplicaSet-to-Deployment UID chain.
func (p *RuntimeResourceQuotaPreflight) Check(ctx context.Context) error {
	return p.check(ctx, false)
}

// WaitForCapacityAfterQuiesce waits until quota-controller status no longer
// includes the deleted protected Pods and the replacement runtime fits at one
// stable observation. It retries asynchronous quota status, concurrent
// snapshots, and explicitly transient API failures; authentication,
// authorization, malformed contracts, and non-quiesced protected Pods fail
// immediately.
func (p *RuntimeResourceQuotaPreflight) WaitForCapacityAfterQuiesce(ctx context.Context, pollEvery time.Duration) error {
	if ctx == nil {
		return errors.New("post-quiesce runtime ResourceQuota context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if pollEvery <= 0 {
		return errors.New("post-quiesce runtime ResourceQuota poll interval must be positive")
	}

	var lastPending error
	for {
		err := p.check(ctx, true)
		if err == nil {
			return nil
		}
		var pending *runtimeResourceQuotaPendingError
		if !errors.As(err, &pending) {
			return err
		}
		lastPending = err

		timer := time.NewTimer(pollEvery)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return fmt.Errorf(
				"post-quiesce runtime ResourceQuota capacity did not converge; last observation: %v: %w",
				lastPending,
				ctx.Err(),
			)
		case <-timer.C:
		}
	}
}

type runtimeResourceQuotaPendingError struct {
	err error
}

func (e *runtimeResourceQuotaPendingError) Error() string {
	return e.err.Error()
}

func (e *runtimeResourceQuotaPendingError) Unwrap() error {
	return e.err
}

func runtimeResourceQuotaPending(err error) error {
	if err == nil {
		return nil
	}
	return &runtimeResourceQuotaPendingError{err: err}
}

func runtimeResourceQuotaObservationError(err error) error {
	if err == nil {
		return nil
	}
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) ||
		utilnet.IsProbableEOF(err) || utilnet.IsConnectionRefused(err) {
		return runtimeResourceQuotaPending(err)
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return runtimeResourceQuotaPending(err)
	}
	return err
}

func (p *RuntimeResourceQuotaPreflight) check(ctx context.Context, requireQuiesced bool) error {
	if err := p.validate(); err != nil {
		return err
	}

	quotasBeforePods, err := p.listResourceQuotas(ctx)
	if err != nil {
		return err
	}
	pods, err := p.listPods(ctx)
	if err != nil {
		return err
	}
	if err := p.validatePodInventory(pods); err != nil {
		return err
	}

	protected, err := p.protectedPodUsages(pods)
	if err != nil {
		return err
	}
	if requireQuiesced && len(protected) != 0 {
		return fmt.Errorf("post-quiesce runtime ResourceQuota inventory still contains %d protected runtime Pods", len(protected))
	}
	quotasAfterPods, err := p.listResourceQuotas(ctx)
	if err != nil {
		return err
	}
	if err := p.validateQuotaInventory(quotasBeforePods); err != nil {
		return err
	}
	if err := p.validateQuotaInventory(quotasAfterPods); err != nil {
		return err
	}
	if err := runtimeQuotaVerifyStableSnapshot(quotasBeforePods, quotasAfterPods); err != nil {
		return runtimeResourceQuotaPending(err)
	}
	candidates, err := p.candidatePodUsages()
	if err != nil {
		return err
	}

	for index := range quotasAfterPods {
		quota := &quotasAfterPods[index]
		if err := p.checkQuota(quota, protected, candidates); err != nil {
			return err
		}
	}
	return nil
}

func (p *RuntimeResourceQuotaPreflight) validateQuotaInventory(quotas []corev1.ResourceQuota) error {
	seenNames := make(map[string]struct{}, len(quotas))
	seenUIDs := make(map[string]string, len(quotas))
	for index := range quotas {
		if err := p.validateQuota(&quotas[index], seenNames, seenUIDs); err != nil {
			return err
		}
	}
	return nil
}

func runtimeQuotaVerifyStableSnapshot(before, after []corev1.ResourceQuota) error {
	if len(before) != len(after) {
		return fmt.Errorf(
			"ResourceQuota inventory changed while the Pod inventory was read: count changed from %d to %d",
			len(before),
			len(after),
		)
	}
	beforeByName := make(map[string]*corev1.ResourceQuota, len(before))
	for index := range before {
		quota := &before[index]
		beforeByName[quota.Name] = quota
	}
	for index := range after {
		quota := &after[index]
		previous, found := beforeByName[quota.Name]
		if !found {
			return fmt.Errorf(
				"ResourceQuota inventory changed while the Pod inventory was read: %s/%s appeared",
				quota.Namespace,
				quota.Name,
			)
		}
		if !apiequality.Semantic.DeepEqual(previous, quota) {
			return fmt.Errorf(
				"ResourceQuota inventory changed while the Pod inventory was read: %s/%s changed",
				quota.Namespace,
				quota.Name,
			)
		}
		delete(beforeByName, quota.Name)
	}
	if len(beforeByName) != 0 {
		return errors.New("ResourceQuota inventory changed while the Pod inventory was read: an object disappeared")
	}
	return nil
}

func (p *RuntimeResourceQuotaPreflight) listResourceQuotas(ctx context.Context) ([]corev1.ResourceQuota, error) {
	var result []corev1.ResourceQuota
	continueToken := ""
	resourceVersion := ""
	firstPage := true
	seenContinueTokens := make(map[string]struct{})
	for {
		page, err := p.ResourceQuotas.List(ctx, metav1.ListOptions{
			Limit:    runtimeResourceQuotaPageSize,
			Continue: continueToken,
		})
		if err != nil {
			return nil, runtimeResourceQuotaObservationError(
				fmt.Errorf("list ResourceQuotas in namespace %s: %w", p.ReleaseNamespace, err),
			)
		}
		if page == nil {
			return nil, errors.New("list ResourceQuotas returned a nil result")
		}
		if err := runtimeQuotaValidatePageMetadata(
			"ResourceQuota",
			continueToken,
			page.ListMeta,
			&resourceVersion,
			firstPage,
		); err != nil {
			return nil, err
		}
		firstPage = false
		result = append(result, page.Items...)
		if page.Continue == "" {
			return result, nil
		}
		if _, found := seenContinueTokens[page.Continue]; found {
			return nil, fmt.Errorf("ResourceQuota inventory repeated continue token %q", page.Continue)
		}
		seenContinueTokens[page.Continue] = struct{}{}
		continueToken = page.Continue
	}
}

func (p *RuntimeResourceQuotaPreflight) listPods(ctx context.Context) ([]corev1.Pod, error) {
	var result []corev1.Pod
	continueToken := ""
	resourceVersion := ""
	firstPage := true
	seenContinueTokens := make(map[string]struct{})
	for {
		page, err := p.Pods.List(ctx, metav1.ListOptions{
			Limit:    runtimeResourceQuotaPageSize,
			Continue: continueToken,
		})
		if err != nil {
			return nil, runtimeResourceQuotaObservationError(
				fmt.Errorf("list Pods in namespace %s for ResourceQuota projection: %w", p.ReleaseNamespace, err),
			)
		}
		if page == nil {
			return nil, errors.New("list Pods for ResourceQuota projection returned a nil result")
		}
		if err := runtimeQuotaValidatePageMetadata(
			"Pod",
			continueToken,
			page.ListMeta,
			&resourceVersion,
			firstPage,
		); err != nil {
			return nil, err
		}
		firstPage = false
		result = append(result, page.Items...)
		if page.Continue == "" {
			return result, nil
		}
		if _, found := seenContinueTokens[page.Continue]; found {
			return nil, fmt.Errorf("Pod inventory repeated continue token %q", page.Continue)
		}
		seenContinueTokens[page.Continue] = struct{}{}
		continueToken = page.Continue
	}
}

func runtimeQuotaValidatePageMetadata(
	resourceKind string,
	requestContinueToken string,
	metadata metav1.ListMeta,
	resourceVersion *string,
	firstPage bool,
) error {
	if metadata.Continue != "" && metadata.Continue == requestContinueToken {
		return fmt.Errorf("%s inventory repeated continue token %q", resourceKind, metadata.Continue)
	}
	if resourceVersion == nil {
		return fmt.Errorf("%s inventory resourceVersion tracker is required", resourceKind)
	}
	if metadata.ResourceVersion == "" {
		return fmt.Errorf("%s inventory returned an empty resourceVersion", resourceKind)
	}
	if firstPage {
		*resourceVersion = metadata.ResourceVersion
	} else if metadata.ResourceVersion != *resourceVersion {
		return fmt.Errorf(
			"%s inventory resourceVersion changed across pages from %q to %q",
			resourceKind,
			*resourceVersion,
			metadata.ResourceVersion,
		)
	}
	if metadata.RemainingItemCount == nil {
		return nil
	}
	if *metadata.RemainingItemCount < 0 {
		return fmt.Errorf("%s inventory returned negative remainingItemCount %d", resourceKind, *metadata.RemainingItemCount)
	}
	return nil
}

func (p *RuntimeResourceQuotaPreflight) validatePodInventory(pods []corev1.Pod) error {
	seenNames := make(map[string]struct{}, len(pods))
	seenUIDs := make(map[string]string, len(pods))
	for index := range pods {
		pod := &pods[index]
		object := pod.Namespace + "/" + pod.Name
		if pod.Namespace != p.ReleaseNamespace || pod.Name == "" || pod.UID == "" {
			return fmt.Errorf("runtime ResourceQuota inventory returned a foreign or incomplete Pod %q", object)
		}
		if _, found := seenNames[pod.Name]; found {
			return fmt.Errorf("runtime ResourceQuota inventory returned Pod %s more than once", object)
		}
		seenNames[pod.Name] = struct{}{}
		if previous, found := seenUIDs[string(pod.UID)]; found {
			return fmt.Errorf("Pods %s and %s share UID %s", previous, object, pod.UID)
		}
		seenUIDs[string(pod.UID)] = object
	}
	return nil
}

func (p *RuntimeResourceQuotaPreflight) validate() error {
	if p == nil {
		return errors.New("runtime ResourceQuota preflight is required")
	}
	if p.ResourceQuotas == nil {
		return errors.New("runtime ResourceQuota client is required")
	}
	if p.Pods == nil {
		return errors.New("runtime ResourceQuota Pod client is required")
	}
	for description, value := range map[string]string{
		"release name":                    p.ReleaseName,
		"release namespace":               p.ReleaseNamespace,
		"controller Deployment name":      p.ControllerDeploymentName,
		"certificate Deployment name":     p.CertificateDeploymentName,
		"controller ServiceAccount name":  p.Contract.ControllerServiceAccountName,
		"certificate ServiceAccount name": p.Contract.CertificateServiceAccountName,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("runtime ResourceQuota %s is required and must not contain surrounding whitespace", description)
		}
	}
	if p.Contract.Namespace != p.ReleaseNamespace {
		return fmt.Errorf("runtime admission namespace %q differs from release namespace %q", p.Contract.Namespace, p.ReleaseNamespace)
	}
	if p.ControllerDeploymentName == p.CertificateDeploymentName {
		return errors.New("runtime ResourceQuota protected Deployment names must differ")
	}
	if p.Contract.ControllerServiceAccountName == p.Contract.CertificateServiceAccountName {
		return errors.New("runtime ResourceQuota protected ServiceAccount names must differ")
	}
	if p.ControllerReplicas < 1 {
		return fmt.Errorf("runtime ResourceQuota controller replicas must be positive, got %d", p.ControllerReplicas)
	}
	containers := []runtimeAdmissionContainer{
		{name: "common init container", resources: p.Contract.CommonInitContainerResources},
		{name: "controller container", resources: p.Contract.ControllerContainerResources},
	}
	if p.Contract.CertificateRuntimeEnabled {
		containers = append(containers, runtimeAdmissionContainer{name: "certificate container", resources: p.Contract.CertificateContainerResources})
	}
	for _, container := range containers {
		if err := validateRuntimeResourceRequirements(container.name, container.resources); err != nil {
			return fmt.Errorf("runtime ResourceQuota candidate contract: %w", err)
		}
		if err := rejectCorePodResourceDefaulting(container.name, container.resources); err != nil {
			return fmt.Errorf("runtime ResourceQuota candidate contract: %w", err)
		}
	}
	if p.Contract.PriorityClassName != strings.TrimSpace(p.Contract.PriorityClassName) {
		return errors.New("runtime ResourceQuota PriorityClass name must not contain surrounding whitespace")
	}
	return nil
}

type runtimeQuotaPod struct {
	description            string
	usage                  corev1.ResourceList
	priorityClassName      string
	terminating            bool
	bestEffort             bool
	crossNamespaceAffinity bool
	multiplier             int32
	containers             []corev1.Container
	initContainers         []corev1.Container
}

func (p *RuntimeResourceQuotaPreflight) candidatePodUsages() ([]runtimeQuotaPod, error) {
	controller := p.candidatePod(
		"candidate controller Pod",
		p.Contract.ControllerContainerResources,
		p.ControllerReplicas,
	)
	candidates := []runtimeQuotaPod{controller}
	if p.Contract.CertificateRuntimeEnabled {
		candidates = append(candidates, p.candidatePod(
			"candidate certificate Pod",
			p.Contract.CertificateContainerResources,
			1,
		))
	}
	for index := range candidates {
		pod := runtimeQuotaPodObject(candidates[index])
		usage, err := runtimeQuotaPodUsage(pod)
		if err != nil {
			return nil, fmt.Errorf("measure %s quota usage: %w", candidates[index].description, err)
		}
		candidates[index].usage = usage
		bestEffort, err := runtimeQuotaPodBestEffort(pod)
		if err != nil {
			return nil, fmt.Errorf("classify %s quota scope: %w", candidates[index].description, err)
		}
		candidates[index].bestEffort = bestEffort
	}
	return candidates, nil
}

func (p *RuntimeResourceQuotaPreflight) candidatePod(
	description string,
	app corev1.ResourceRequirements,
	multiplier int32,
) runtimeQuotaPod {
	return runtimeQuotaPod{
		description:       description,
		priorityClassName: p.Contract.PriorityClassName,
		multiplier:        multiplier,
		containers: []corev1.Container{{
			Name:      "runtime",
			Resources: *app.DeepCopy(),
		}},
		initContainers: []corev1.Container{{
			Name:      "verify-candidate-runtime",
			Resources: *p.Contract.CommonInitContainerResources.DeepCopy(),
		}},
		// Both properties are fixed by the current immutable runtime Pod
		// contract. Keep them explicit so broadening that contract cannot be
		// mistaken for a quota-neutral change.
		terminating:            false,
		crossNamespaceAffinity: false,
	}
}

func runtimeQuotaPodObject(pod runtimeQuotaPod) *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers:        append([]corev1.Container(nil), pod.containers...),
			InitContainers:    append([]corev1.Container(nil), pod.initContainers...),
			PriorityClassName: pod.priorityClassName,
		},
	}
}

func (p *RuntimeResourceQuotaPreflight) protectedPodUsages(pods []corev1.Pod) ([]runtimeQuotaPod, error) {
	seenNames := make(map[string]struct{})
	seenUIDs := make(map[string]string)
	result := make([]runtimeQuotaPod, 0)
	for index := range pods {
		pod := &pods[index]
		identity, signaled, err := p.protectedPodIdentity(pod)
		if err != nil {
			return nil, err
		}
		if !signaled {
			continue
		}
		if err := p.verifyProtectedPod(pod, identity); err != nil {
			return nil, err
		}
		object := pod.Namespace + "/" + pod.Name
		if _, found := seenNames[pod.Name]; found {
			return nil, fmt.Errorf("runtime ResourceQuota inventory returned protected Pod %s more than once", object)
		}
		seenNames[pod.Name] = struct{}{}
		if previous, found := seenUIDs[string(pod.UID)]; found {
			return nil, fmt.Errorf("protected runtime Pods %s and %s share UID %s", previous, object, pod.UID)
		}
		seenUIDs[string(pod.UID)] = object

		usage, err := runtimeQuotaPodUsage(pod)
		if err != nil {
			return nil, fmt.Errorf("measure protected runtime Pod %s quota usage: %w", object, err)
		}
		bestEffort, err := runtimeQuotaPodBestEffort(pod)
		if err != nil {
			return nil, fmt.Errorf("classify protected runtime Pod %s quota scope: %w", object, err)
		}
		result = append(result, runtimeQuotaPod{
			description:            "protected runtime Pod " + object,
			usage:                  usage,
			priorityClassName:      pod.Spec.PriorityClassName,
			terminating:            runtimeQuotaPodTerminating(pod),
			bestEffort:             bestEffort,
			crossNamespaceAffinity: runtimeQuotaUsesCrossNamespaceAffinity(pod),
			multiplier:             1,
			containers:             pod.Spec.Containers,
			initContainers:         pod.Spec.InitContainers,
		})
	}
	return result, nil
}

type runtimeQuotaProtectedIdentity struct {
	deploymentName string
	serviceAccount string
	component      string
}

func (p *RuntimeResourceQuotaPreflight) protectedPodIdentity(pod *corev1.Pod) (runtimeQuotaProtectedIdentity, bool, error) {
	controller := runtimeQuotaProtectedIdentity{
		deploymentName: p.ControllerDeploymentName,
		serviceAccount: p.Contract.ControllerServiceAccountName,
		component:      "controller",
	}
	certificate := runtimeQuotaProtectedIdentity{
		deploymentName: p.CertificateDeploymentName,
		serviceAccount: p.Contract.CertificateServiceAccountName,
		component:      "certificate-rotation",
	}

	var matches []runtimeQuotaProtectedIdentity
	for _, identity := range []runtimeQuotaProtectedIdentity{controller, certificate} {
		serviceAccountMatch := pod.Spec.ServiceAccountName == identity.serviceAccount
		labelMatch := pod.Labels[instanceLabel] == p.ReleaseName && pod.Labels["app.kubernetes.io/component"] == identity.component
		ownerMatch := false
		for _, owner := range pod.OwnerReferences {
			hash := pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
			if owner.Kind == "ReplicaSet" && hash != "" && owner.Name == identity.deploymentName+"-"+hash {
				ownerMatch = true
				break
			}
		}
		if serviceAccountMatch || labelMatch || ownerMatch {
			matches = append(matches, identity)
		}
	}
	if len(matches) == 0 {
		return runtimeQuotaProtectedIdentity{}, false, nil
	}
	if len(matches) != 1 {
		return runtimeQuotaProtectedIdentity{}, true, fmt.Errorf("pod %s/%s ambiguously signals more than one protected runtime Deployment", pod.Namespace, pod.Name)
	}
	return matches[0], true, nil
}

func (p *RuntimeResourceQuotaPreflight) verifyProtectedPod(pod *corev1.Pod, identity runtimeQuotaProtectedIdentity) error {
	object := pod.Namespace + "/" + pod.Name
	if pod.Namespace != p.ReleaseNamespace || pod.Name == "" || pod.UID == "" {
		return fmt.Errorf("protected runtime Pod %q has foreign or incomplete identity", object)
	}
	if pod.Spec.ServiceAccountName != identity.serviceAccount {
		return fmt.Errorf("protected runtime Pod %s uses ServiceAccount %q instead of %q", object, pod.Spec.ServiceAccountName, identity.serviceAccount)
	}
	for key, expected := range map[string]string{
		instanceLabel:                 p.ReleaseName,
		"app.kubernetes.io/component": identity.component,
	} {
		if pod.Labels[key] != expected {
			return fmt.Errorf("protected runtime Pod %s label %s is %q instead of %q", object, key, pod.Labels[key], expected)
		}
	}
	if pod.Labels["app.kubernetes.io/name"] == "" {
		return fmt.Errorf("protected runtime Pod %s has no app.kubernetes.io/name label", object)
	}
	if len(pod.OwnerReferences) != 1 {
		return fmt.Errorf("protected runtime Pod %s must have exactly one ReplicaSet owner reference", object)
	}
	owner := pod.OwnerReferences[0]
	if owner.APIVersion != "apps/v1" || owner.Kind != "ReplicaSet" || owner.Name == "" || owner.UID == "" ||
		owner.Controller == nil || !*owner.Controller || owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
		return fmt.Errorf("protected runtime Pod %s has an incomplete or non-controlling ReplicaSet owner", object)
	}
	hash := pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
	if !workloadInventoryHashPattern.MatchString(hash) || owner.Name != identity.deploymentName+"-"+hash {
		return fmt.Errorf("protected runtime Pod %s does not bind its ReplicaSet owner to Deployment %s and pod-template-hash %q", object, identity.deploymentName, hash)
	}
	if err := verifyGeneratedObjectName("protected runtime Pod "+object, pod.Name, pod.GenerateName, owner.Name+"-"); err != nil {
		return err
	}
	if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return fmt.Errorf("protected runtime Pod %s is deleting or terminal; count/pods cannot be projected as removed by Recreate", object)
	}
	if runtimeQuotaPodTerminating(pod) {
		return fmt.Errorf("protected runtime Pod %s has activeDeadlineSeconds, but the fixed candidate ResourceQuota contract is non-terminating", object)
	}
	if runtimeQuotaUsesCrossNamespaceAffinity(pod) {
		return fmt.Errorf("protected runtime Pod %s uses cross-namespace affinity, but the fixed candidate ResourceQuota contract does not", object)
	}
	return nil
}

func (p *RuntimeResourceQuotaPreflight) validateQuota(
	quota *corev1.ResourceQuota,
	seenNames map[string]struct{},
	seenUIDs map[string]string,
) error {
	if quota.Namespace != p.ReleaseNamespace || quota.Name == "" || quota.UID == "" {
		return fmt.Errorf("runtime ResourceQuota inventory returned a foreign or incomplete ResourceQuota %q", quota.Namespace+"/"+quota.Name)
	}
	object := quota.Namespace + "/" + quota.Name
	if _, found := seenNames[quota.Name]; found {
		return fmt.Errorf("runtime ResourceQuota inventory returned %s more than once", object)
	}
	seenNames[quota.Name] = struct{}{}
	if previous, found := seenUIDs[string(quota.UID)]; found {
		return fmt.Errorf("ResourceQuotas %s and %s share UID %s", previous, object, quota.UID)
	}
	seenUIDs[string(quota.UID)] = object

	if err := runtimeQuotaResourceListsEqual(quota.Spec.Hard, quota.Status.Hard); err != nil {
		return fmt.Errorf("ResourceQuota %s status.hard is not synchronized with spec.hard: %w", object, err)
	}
	if len(quota.Status.Used) != len(quota.Status.Hard) {
		return fmt.Errorf("ResourceQuota %s status.used does not contain exactly the synchronized hard resource keys", object)
	}
	for _, name := range sortedRuntimeResourceNames(quota.Status.Hard) {
		hard := quota.Status.Hard[name]
		used, found := quota.Status.Used[name]
		if !found {
			return fmt.Errorf("ResourceQuota %s status.used is missing hard resource %s", object, name)
		}
		if hard.Sign() < 0 || used.Sign() < 0 {
			return fmt.Errorf("ResourceQuota %s has a negative hard or used value for %s", object, name)
		}
		if used.Cmp(hard) > 0 {
			return fmt.Errorf("ResourceQuota %s status.used[%s]=%s exceeds status.hard=%s", object, name, used.String(), hard.String())
		}
	}
	for name := range quota.Status.Used {
		if _, found := quota.Status.Hard[name]; !found {
			return fmt.Errorf("ResourceQuota %s status.used contains untracked resource %s", object, name)
		}
	}
	if runtimeQuotaHasPodResources(quota.Status.Hard) {
		return runtimeQuotaValidateScopes(object, quota.Spec.Scopes, quota.Spec.ScopeSelector)
	}
	return nil
}

func (p *RuntimeResourceQuotaPreflight) checkQuota(
	quota *corev1.ResourceQuota,
	protected []runtimeQuotaPod,
	candidates []runtimeQuotaPod,
) error {
	if !runtimeQuotaHasPodResources(quota.Status.Hard) {
		return nil
	}

	removed := corev1.ResourceList{}
	for index := range protected {
		matches, err := runtimeQuotaMatchesScopes(quota, protected[index])
		if err != nil {
			return fmt.Errorf("match %s against ResourceQuota %s/%s: %w", protected[index].description, quota.Namespace, quota.Name, err)
		}
		if matches {
			if err := runtimeQuotaAddMultiplied(removed, protected[index].usage, protected[index].multiplier); err != nil {
				return fmt.Errorf("sum old protected usage for ResourceQuota %s/%s: %w", quota.Namespace, quota.Name, err)
			}
		}
	}

	added := corev1.ResourceList{}
	for index := range candidates {
		matches, err := runtimeQuotaMatchesScopes(quota, candidates[index])
		if err != nil {
			return fmt.Errorf("match %s against ResourceQuota %s/%s: %w", candidates[index].description, quota.Namespace, quota.Name, err)
		}
		if !matches {
			continue
		}
		if err := runtimeQuotaCheckContainerConstraints(quota, candidates[index]); err != nil {
			return err
		}
		if err := runtimeQuotaAddMultiplied(added, candidates[index].usage, candidates[index].multiplier); err != nil {
			return fmt.Errorf("sum candidate usage for ResourceQuota %s/%s: %w", quota.Namespace, quota.Name, err)
		}
	}

	for _, name := range sortedRuntimeResourceNames(quota.Status.Hard) {
		if !runtimeResourceQuotaPodResource(name) {
			continue
		}
		used := quota.Status.Used[name].DeepCopy()
		old := removed[name]
		if used.Cmp(old) < 0 {
			return fmt.Errorf("ResourceQuota %s/%s status.used[%s]=%s is smaller than exact protected runtime Pod usage %s; quota counters are stale or malformed", quota.Namespace, quota.Name, name, used.String(), old.String())
		}
		used.Sub(old)
		used.Add(added[name])
		hard := quota.Status.Hard[name]
		if used.Cmp(hard) > 0 {
			return runtimeResourceQuotaPending(fmt.Errorf("post-Recreate runtime Pods would exceed ResourceQuota %s/%s %s: projected %s, hard %s", quota.Namespace, quota.Name, name, used.String(), hard.String()))
		}
	}
	return nil
}

func runtimeQuotaCheckContainerConstraints(quota *corev1.ResourceQuota, pod runtimeQuotaPod) error {
	for _, name := range sortedRuntimeResourceNames(quota.Status.Hard) {
		var field string
		switch name {
		case corev1.ResourceCPU, corev1.ResourceRequestsCPU:
			field = "request for cpu"
		case corev1.ResourceMemory, corev1.ResourceRequestsMemory:
			field = "request for memory"
		case corev1.ResourceLimitsCPU:
			field = "limit for cpu"
		case corev1.ResourceLimitsMemory:
			field = "limit for memory"
		default:
			continue
		}
		for _, group := range []struct {
			name       string
			containers []corev1.Container
		}{
			{name: "container", containers: pod.containers},
			{name: "init container", containers: pod.initContainers},
		} {
			for index := range group.containers {
				container := group.containers[index]
				var found bool
				switch name {
				case corev1.ResourceCPU, corev1.ResourceRequestsCPU:
					_, found = container.Resources.Requests[corev1.ResourceCPU]
				case corev1.ResourceMemory, corev1.ResourceRequestsMemory:
					_, found = container.Resources.Requests[corev1.ResourceMemory]
				case corev1.ResourceLimitsCPU:
					_, found = container.Resources.Limits[corev1.ResourceCPU]
				case corev1.ResourceLimitsMemory:
					_, found = container.Resources.Limits[corev1.ResourceMemory]
				}
				if !found {
					return fmt.Errorf("ResourceQuota %s/%s requires every %s in %s to specify a %s; %s %s omits it", quota.Namespace, quota.Name, group.name, pod.description, field, group.name, container.Name)
				}
			}
		}
	}
	return nil
}

func runtimeResourceQuotaPodResource(name corev1.ResourceName) bool {
	switch name {
	case corev1.ResourcePods,
		runtimeResourceQuotaObjectCountPods,
		corev1.ResourceCPU,
		corev1.ResourceMemory,
		corev1.ResourceEphemeralStorage,
		corev1.ResourceRequestsCPU,
		corev1.ResourceRequestsMemory,
		corev1.ResourceRequestsEphemeralStorage,
		corev1.ResourceLimitsCPU,
		corev1.ResourceLimitsMemory,
		corev1.ResourceLimitsEphemeralStorage:
		return true
	}
	value := string(name)
	if strings.HasPrefix(value, corev1.ResourceHugePagesPrefix) ||
		strings.HasPrefix(value, corev1.ResourceRequestsHugePagesPrefix) {
		return true
	}
	if !strings.HasPrefix(value, corev1.DefaultResourceRequestsPrefix) {
		return false
	}
	resourceName := strings.TrimPrefix(value, corev1.DefaultResourceRequestsPrefix)
	return runtimeQuotaExtendedResource(corev1.ResourceName(resourceName))
}

func runtimeQuotaHasPodResources(resources corev1.ResourceList) bool {
	for name := range resources {
		if runtimeResourceQuotaPodResource(name) {
			return true
		}
	}
	return false
}

func runtimeQuotaExtendedResource(name corev1.ResourceName) bool {
	value := string(name)
	if strings.HasPrefix(value, resourcev1.ResourceDeviceClassPrefix) {
		return true
	}
	return strings.Contains(value, "/") && !strings.Contains(value, "kubernetes.io/")
}

func runtimeQuotaValidateScopes(object string, scopes []corev1.ResourceQuotaScope, selector *corev1.ScopeSelector) error {
	requirements := make([]corev1.ScopedResourceSelectorRequirement, 0, len(scopes))
	for _, scope := range scopes {
		requirements = append(requirements, corev1.ScopedResourceSelectorRequirement{
			ScopeName: scope,
			Operator:  corev1.ScopeSelectorOpExists,
		})
	}
	if selector != nil {
		requirements = append(requirements, selector.MatchExpressions...)
	}
	for index, requirement := range requirements {
		switch requirement.ScopeName {
		case corev1.ResourceQuotaScopeTerminating,
			corev1.ResourceQuotaScopeNotTerminating,
			corev1.ResourceQuotaScopeBestEffort,
			corev1.ResourceQuotaScopeNotBestEffort,
			corev1.ResourceQuotaScopeCrossNamespacePodAffinity:
			if requirement.Operator != corev1.ScopeSelectorOpExists || len(requirement.Values) != 0 {
				return fmt.Errorf("ResourceQuota %s scope selector %d for %s must use Exists with no values", object, index, requirement.ScopeName)
			}
		case corev1.ResourceQuotaScopePriorityClass:
			if _, err := runtimeQuotaPriorityRequirement(requirement); err != nil {
				return fmt.Errorf("ResourceQuota %s PriorityClass scope selector %d: %w", object, index, err)
			}
		default:
			return fmt.Errorf("ResourceQuota %s scope selector %d uses unsupported scope %q", object, index, requirement.ScopeName)
		}
	}
	return nil
}

func runtimeQuotaMatchesScopes(quota *corev1.ResourceQuota, pod runtimeQuotaPod) (bool, error) {
	requirements := make([]corev1.ScopedResourceSelectorRequirement, 0, len(quota.Spec.Scopes))
	for _, scope := range quota.Spec.Scopes {
		requirements = append(requirements, corev1.ScopedResourceSelectorRequirement{ScopeName: scope, Operator: corev1.ScopeSelectorOpExists})
	}
	if quota.Spec.ScopeSelector != nil {
		requirements = append(requirements, quota.Spec.ScopeSelector.MatchExpressions...)
	}
	for _, requirement := range requirements {
		var match bool
		switch requirement.ScopeName {
		case corev1.ResourceQuotaScopeTerminating:
			match = pod.terminating
		case corev1.ResourceQuotaScopeNotTerminating:
			match = !pod.terminating
		case corev1.ResourceQuotaScopeBestEffort:
			match = pod.bestEffort
		case corev1.ResourceQuotaScopeNotBestEffort:
			match = !pod.bestEffort
		case corev1.ResourceQuotaScopePriorityClass:
			requirementSelector, err := runtimeQuotaPriorityRequirement(requirement)
			if err != nil {
				return false, err
			}
			values := labels.Set(nil)
			if pod.priorityClassName != "" {
				values = labels.Set{string(corev1.ResourceQuotaScopePriorityClass): pod.priorityClassName}
			}
			match = requirementSelector.Matches(values)
		case corev1.ResourceQuotaScopeCrossNamespacePodAffinity:
			match = pod.crossNamespaceAffinity
		default:
			return false, fmt.Errorf("unsupported scope %q", requirement.ScopeName)
		}
		if !match {
			return false, nil
		}
	}
	return true, nil
}

func runtimeQuotaPriorityRequirement(requirement corev1.ScopedResourceSelectorRequirement) (labels.Selector, error) {
	var operator selection.Operator
	switch requirement.Operator {
	case corev1.ScopeSelectorOpIn:
		operator = selection.In
	case corev1.ScopeSelectorOpNotIn:
		operator = selection.NotIn
	case corev1.ScopeSelectorOpExists:
		operator = selection.Exists
	case corev1.ScopeSelectorOpDoesNotExist:
		operator = selection.DoesNotExist
	default:
		return nil, fmt.Errorf("operator %q is unsupported", requirement.Operator)
	}
	selectorRequirement, err := labels.NewRequirement(string(corev1.ResourceQuotaScopePriorityClass), operator, requirement.Values)
	if err != nil {
		return nil, err
	}
	return labels.NewSelector().Add(*selectorRequirement), nil
}

func runtimeQuotaPodUsage(pod *corev1.Pod) (corev1.ResourceList, error) {
	requests, err := runtimeQuotaAggregateResources(pod, true)
	if err != nil {
		return nil, err
	}
	limits, err := runtimeQuotaAggregateResources(pod, false)
	if err != nil {
		return nil, err
	}
	if err := runtimeQuotaValidateResourceStatus(pod, requests, limits); err != nil {
		return nil, err
	}
	result := corev1.ResourceList{
		runtimeResourceQuotaObjectCountPods: *resource.NewQuantity(1, resource.DecimalSI),
		corev1.ResourcePods:                 *resource.NewQuantity(1, resource.DecimalSI),
	}
	for resourceName, quotaNames := range map[corev1.ResourceName][]corev1.ResourceName{
		corev1.ResourceCPU:              {corev1.ResourceCPU, corev1.ResourceRequestsCPU},
		corev1.ResourceMemory:           {corev1.ResourceMemory, corev1.ResourceRequestsMemory},
		corev1.ResourceEphemeralStorage: {corev1.ResourceEphemeralStorage, corev1.ResourceRequestsEphemeralStorage},
	} {
		if quantity, found := requests[resourceName]; found {
			for _, quotaName := range quotaNames {
				result[quotaName] = quantity.DeepCopy()
			}
		}
	}
	for resourceName, quotaName := range map[corev1.ResourceName]corev1.ResourceName{
		corev1.ResourceCPU:              corev1.ResourceLimitsCPU,
		corev1.ResourceMemory:           corev1.ResourceLimitsMemory,
		corev1.ResourceEphemeralStorage: corev1.ResourceLimitsEphemeralStorage,
	} {
		if quantity, found := limits[resourceName]; found {
			result[quotaName] = quantity.DeepCopy()
		}
	}
	for resourceName, quantity := range requests {
		value := string(resourceName)
		switch {
		case strings.HasPrefix(value, corev1.ResourceHugePagesPrefix):
			result[resourceName] = quantity.DeepCopy()
			result[corev1.ResourceName(corev1.DefaultResourceRequestsPrefix+value)] = quantity.DeepCopy()
		case runtimeQuotaExtendedResource(resourceName):
			result[corev1.ResourceName(corev1.DefaultResourceRequestsPrefix+value)] = quantity.DeepCopy()
		}
	}
	return result, nil
}

func runtimeQuotaAggregateResources(pod *corev1.Pod, requests bool) (corev1.ResourceList, error) {
	result := corev1.ResourceList{}
	for index := range pod.Spec.Containers {
		container := &pod.Spec.Containers[index]
		values, err := runtimeQuotaContainerResources(container, requests)
		if err != nil {
			return nil, fmt.Errorf("container %s: %w", container.Name, err)
		}
		runtimeQuotaAdd(result, values)
	}

	restartable := corev1.ResourceList{}
	initMaximum := corev1.ResourceList{}
	for index := range pod.Spec.InitContainers {
		container := &pod.Spec.InitContainers[index]
		isRestartable := container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways
		if container.RestartPolicy != nil && !isRestartable {
			return nil, fmt.Errorf("init container %s has unsupported restartPolicy %q", container.Name, *container.RestartPolicy)
		}
		values, err := runtimeQuotaContainerResources(container, requests)
		if err != nil {
			return nil, fmt.Errorf("init container %s: %w", container.Name, err)
		}
		if isRestartable {
			runtimeQuotaAdd(result, values)
			runtimeQuotaAdd(restartable, values)
			values = copyRuntimeResourceList(restartable)
		} else {
			withRestartable := copyRuntimeResourceList(values)
			if withRestartable == nil {
				withRestartable = corev1.ResourceList{}
			}
			runtimeQuotaAdd(withRestartable, restartable)
			values = withRestartable
		}
		runtimeQuotaMax(initMaximum, values)
	}
	runtimeQuotaMax(result, initMaximum)

	if pod.Spec.Resources != nil && (len(pod.Spec.Resources.Requests) != 0 || len(pod.Spec.Resources.Limits) != 0) {
		return nil, errors.New("pod-level resources are outside the fixed runtime quota contract")
	}
	if err := runtimeQuotaValidateResourceList("Pod overhead", pod.Spec.Overhead); err != nil {
		return nil, err
	}
	if requests {
		runtimeQuotaAdd(result, pod.Spec.Overhead)
	} else {
		for name, overhead := range pod.Spec.Overhead {
			current, found := result[name]
			if !found || current.IsZero() {
				continue
			}
			current.Add(overhead)
			result[name] = current
		}
	}
	return result, nil
}

func runtimeQuotaValidateResourceStatus(
	pod *corev1.Pod,
	expectedRequests corev1.ResourceList,
	expectedLimits corev1.ResourceList,
) error {
	if pod.Status.Resize != "" {
		return fmt.Errorf(
			"protected runtime Pod has legacy resize status %q; resized Pods cannot be projected consistently across the supported Kubernetes window",
			pod.Status.Resize,
		)
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodResizePending || condition.Type == corev1.PodResizeInProgress {
			return fmt.Errorf("protected runtime Pod has %s status; resized Pods cannot be projected consistently across the supported Kubernetes window", condition.Type)
		}
	}
	if len(pod.Status.ResourceClaimStatuses) != 0 || pod.Status.ExtendedResourceClaimStatus != nil ||
		len(pod.Status.NodeAllocatableResourceClaimStatuses) != 0 {
		return errors.New("protected runtime Pod has dynamic resource allocation status outside the fixed runtime quota contract")
	}

	// Kubernetes 1.36 and later can publish aggregate Pod resource status even
	// when PodSpec.Resources is unset. Actuated CPU values can be rounded down
	// when read back from cgroup v2. For resize-aware container fields,
	// Kubernetes charges the maximum of the spec, actuated, and allocated
	// requests, and the maximum of the spec and actuated limits. Pod aggregate
	// status is currently selected only with pod-level spec resources, which the
	// fixed contract forbids. Applying the same upper bound here keeps that
	// contract conservative as the support window advances: a known nonnegative
	// status subset no greater than the spec cannot increase quota usage, while
	// larger or unknown values can.
	if pod.Status.AllocatedResources != nil {
		if err := runtimeQuotaValidateStatusResourcesAtMost(
			"pod-level allocated requests",
			pod.Status.AllocatedResources,
			expectedRequests,
		); err != nil {
			return err
		}
	}
	if pod.Status.Resources != nil {
		if err := runtimeQuotaValidateStatusResourcesAtMost(
			"pod-level actuated requests",
			pod.Status.Resources.Requests,
			expectedRequests,
		); err != nil {
			return err
		}
		if err := runtimeQuotaValidateStatusResourcesAtMost(
			"pod-level actuated limits",
			pod.Status.Resources.Limits,
			expectedLimits,
		); err != nil {
			return err
		}
	}

	containers := make(map[string]*corev1.Container, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	for _, group := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for index := range group {
			container := &group[index]
			if container.Name == "" {
				return errors.New("container has an empty name")
			}
			if _, found := containers[container.Name]; found {
				return fmt.Errorf("container %s is duplicated", container.Name)
			}
			containers[container.Name] = container
		}
	}

	seen := make(map[string]struct{}, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	for _, group := range [][]corev1.ContainerStatus{pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses} {
		for index := range group {
			status := &group[index]
			if status.Name == "" {
				return errors.New("container status has an empty name")
			}
			if _, found := seen[status.Name]; found {
				return fmt.Errorf("container status %s is duplicated", status.Name)
			}
			container, found := containers[status.Name]
			if !found {
				return fmt.Errorf("container status %s has no matching runtime Pod container", status.Name)
			}
			if status.Resources != nil {
				if err := runtimeQuotaValidateStatusResourcesAtMost(
					"actuated requests for container "+status.Name,
					status.Resources.Requests,
					container.Resources.Requests,
				); err != nil {
					return err
				}
				if err := runtimeQuotaValidateStatusResourcesAtMost(
					"actuated limits for container "+status.Name,
					status.Resources.Limits,
					container.Resources.Limits,
				); err != nil {
					return err
				}
			}
			if err := runtimeQuotaValidateStatusResourcesAtMost(
				"allocated requests for container "+status.Name,
				status.AllocatedResources,
				container.Resources.Requests,
			); err != nil {
				return err
			}
			seen[status.Name] = struct{}{}
		}
	}
	return nil
}

func runtimeQuotaValidateStatusResourcesAtMost(description string, actual, expected corev1.ResourceList) error {
	if err := runtimeQuotaValidateResourceList(description, actual); err != nil {
		return err
	}
	for _, name := range sortedRuntimeResourceNames(actual) {
		actualQuantity := actual[name]
		expectedQuantity, expectedFound := expected[name]
		if !expectedFound {
			return fmt.Errorf(
				"%s contains resource %s outside the fixed runtime Pod spec; resized protected Pods cannot be projected consistently across the supported Kubernetes window",
				description,
				name,
			)
		}
		if actualQuantity.Cmp(expectedQuantity) > 0 {
			return fmt.Errorf("%s %s=%s exceeds the fixed runtime Pod spec value %s; resized protected Pods cannot be projected consistently across the supported Kubernetes window", description, name, actualQuantity.String(), expectedQuantity.String())
		}
	}
	return nil
}

func runtimeQuotaContainerResources(container *corev1.Container, requests bool) (corev1.ResourceList, error) {
	values := container.Resources.Limits
	if requests {
		values = container.Resources.Requests
	}
	if err := runtimeQuotaValidateResourceList("container "+container.Name, values); err != nil {
		return nil, err
	}
	result := copyRuntimeResourceList(values)
	if result == nil {
		result = corev1.ResourceList{}
	}
	return result, nil
}

func runtimeQuotaPodBestEffort(pod *corev1.Pod) (bool, error) {
	if pod.Status.QOSClass != "" {
		switch pod.Status.QOSClass {
		case corev1.PodQOSBestEffort:
			return true, nil
		case corev1.PodQOSBurstable, corev1.PodQOSGuaranteed:
			return false, nil
		default:
			return false, fmt.Errorf("unsupported status.qosClass %q", pod.Status.QOSClass)
		}
	}
	for _, containers := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for _, container := range containers {
			for _, values := range []corev1.ResourceList{container.Resources.Requests, container.Resources.Limits} {
				for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
					if quantity, found := values[name]; found && quantity.Sign() > 0 {
						return false, nil
					}
				}
			}
		}
	}
	return true, nil
}

func runtimeQuotaPodTerminating(pod *corev1.Pod) bool {
	return pod.Spec.ActiveDeadlineSeconds != nil && *pod.Spec.ActiveDeadlineSeconds >= 0
}

func runtimeQuotaUsesCrossNamespaceAffinity(pod *corev1.Pod) bool {
	if pod == nil || pod.Spec.Affinity == nil {
		return false
	}
	if affinity := pod.Spec.Affinity.PodAffinity; affinity != nil {
		if runtimeQuotaCrossNamespaceTerms(
			affinity.RequiredDuringSchedulingIgnoredDuringExecution,
			affinity.PreferredDuringSchedulingIgnoredDuringExecution,
		) {
			return true
		}
	}
	if antiAffinity := pod.Spec.Affinity.PodAntiAffinity; antiAffinity != nil {
		if runtimeQuotaCrossNamespaceTerms(
			antiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
			antiAffinity.PreferredDuringSchedulingIgnoredDuringExecution,
		) {
			return true
		}
	}
	return false
}

func runtimeQuotaCrossNamespaceTerms(required []corev1.PodAffinityTerm, preferred []corev1.WeightedPodAffinityTerm) bool {
	for index := range required {
		if len(required[index].Namespaces) != 0 || required[index].NamespaceSelector != nil {
			return true
		}
	}
	for index := range preferred {
		term := &preferred[index].PodAffinityTerm
		if len(term.Namespaces) != 0 || term.NamespaceSelector != nil {
			return true
		}
	}
	return false
}

func runtimeQuotaAdd(target, values corev1.ResourceList) {
	for name, quantity := range values {
		current := target[name]
		current.Add(quantity)
		target[name] = current
	}
}

func runtimeQuotaMax(target, values corev1.ResourceList) {
	for name, quantity := range values {
		current, found := target[name]
		if !found || quantity.Cmp(current) > 0 {
			target[name] = quantity.DeepCopy()
		}
	}
}

func runtimeQuotaAddMultiplied(target, values corev1.ResourceList, multiplier int32) error {
	if multiplier < 0 {
		return fmt.Errorf("negative Pod multiplier %d", multiplier)
	}
	for name, quantity := range values {
		quantity = quantity.DeepCopy()
		if !quantity.Mul(int64(multiplier)) {
			return fmt.Errorf("resource %s quantity overflows when multiplied by %d", name, multiplier)
		}
		current := target[name]
		current.Add(quantity)
		target[name] = current
	}
	return nil
}

func runtimeQuotaValidateResourceList(description string, values corev1.ResourceList) error {
	for name, quantity := range values {
		if quantity.Sign() < 0 {
			return fmt.Errorf("%s resource %s must not be negative", description, name)
		}
	}
	return nil
}

func runtimeQuotaResourceListsEqual(expected, actual corev1.ResourceList) error {
	if len(expected) != len(actual) {
		return fmt.Errorf("key count %d differs from %d", len(actual), len(expected))
	}
	for name, expectedQuantity := range expected {
		actualQuantity, found := actual[name]
		if !found {
			return fmt.Errorf("resource %s is missing", name)
		}
		if actualQuantity.Cmp(expectedQuantity) != 0 {
			return fmt.Errorf("resource %s is %s instead of %s", name, actualQuantity.String(), expectedQuantity.String())
		}
	}
	return nil
}
