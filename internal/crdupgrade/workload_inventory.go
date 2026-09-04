package crdupgrade

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	workloadInventoryPageSize                     int64 = 500
	workloadInventoryGeneratedNameMaxPrefixLength       = 58
)

var (
	workloadInventoryHashPattern   = regexp.MustCompile(`^[a-z0-9]{1,10}$`)
	workloadInventorySuffixPattern = regexp.MustCompile(`^[a-z0-9]{5}$`)
)

// WorkloadInventoryJobReader is the read-only API surface needed to detect
// hook Jobs that were staged before the admission boundaries became active.
type WorkloadInventoryJobReader interface {
	List(context.Context, metav1.ListOptions) (*batchv1.JobList, error)
}

// WorkloadInventoryReplicaSetReader resolves the live parent of a protected
// runtime Pod.
type WorkloadInventoryReplicaSetReader interface {
	List(context.Context, metav1.ListOptions) (*appsv1.ReplicaSetList, error)
}

// WorkloadInventoryDeploymentReader resolves the live, fixed Deployment at
// the root of a protected runtime Pod's controller chain.
type WorkloadInventoryDeploymentReader interface {
	Get(context.Context, string, metav1.GetOptions) (*appsv1.Deployment, error)
}

// WorkloadInventory verifies objects that might predate the admission guards.
// All operations are read-only and namespace-wide so labels chosen by an
// untrusted object cannot hide it from the inventory.
type WorkloadInventory struct {
	rollout     *RolloutGuard
	pods        PodLister
	jobs        WorkloadInventoryJobReader
	replicaSets WorkloadInventoryReplicaSetReader
	deployments WorkloadInventoryDeploymentReader
}

// NewWorkloadInventory derives all protected identities from the rollout
// contract and accepts only the narrow read interfaces used by each check.
func NewWorkloadInventory(
	rollout *RolloutGuard,
	pods PodLister,
	jobs WorkloadInventoryJobReader,
	replicaSets WorkloadInventoryReplicaSetReader,
	deployments WorkloadInventoryDeploymentReader,
) *WorkloadInventory {
	return &WorkloadInventory{
		rollout:     rollout,
		pods:        pods,
		jobs:        jobs,
		replicaSets: replicaSets,
		deployments: deployments,
	}
}

// VerifyHookBootstrap runs inside the candidate identity Job. It permits that
// one Job and its one controller-created Pod, and rejects every other Job or
// Pod using any release hook ServiceAccount identity.
func (i *WorkloadInventory) VerifyHookBootstrap(ctx context.Context) error {
	hookPattern, identityJobName, err := i.hookIdentity()
	if err != nil {
		return err
	}
	if i.jobs == nil || i.pods == nil {
		return fmt.Errorf("hook workload inventory Job and Pod clients are required")
	}

	jobs, err := i.listJobs(ctx)
	if err != nil {
		return fmt.Errorf("list hook inventory Jobs in namespace %s: %w", i.rollout.ReleaseNamespace, err)
	}

	var identityJob *batchv1.Job
	for index := range jobs {
		job := &jobs[index]
		serviceAccount := job.Spec.Template.Spec.ServiceAccountName
		if job.Name == identityJobName {
			if identityJob != nil {
				return fmt.Errorf("hook inventory returned candidate identity Job %s/%s more than once", i.rollout.ReleaseNamespace, identityJobName)
			}
			if job.UID == "" {
				return fmt.Errorf("candidate identity Job %s/%s has no UID", i.rollout.ReleaseNamespace, identityJobName)
			}
			if serviceAccount != i.rollout.HookServiceAccountName {
				return fmt.Errorf("candidate identity Job %s/%s uses ServiceAccount %q instead of %q", i.rollout.ReleaseNamespace, identityJobName, serviceAccount, i.rollout.HookServiceAccountName)
			}
			identityJob = job
			continue
		}
		if hookPattern.MatchString(serviceAccount) {
			return fmt.Errorf("pre-staged hook Job %s/%s uses protected ServiceAccount %s; only candidate identity Job %s is permitted during bootstrap", job.Namespace, job.Name, serviceAccount, identityJobName)
		}
	}
	if identityJob == nil {
		return fmt.Errorf("candidate identity Job %s/%s is missing from the complete hook inventory", i.rollout.ReleaseNamespace, identityJobName)
	}

	pods, err := i.listPods(ctx)
	if err != nil {
		return fmt.Errorf("list hook bootstrap Pods: %w", err)
	}
	identityPodCount := 0
	for index := range pods {
		pod := &pods[index]
		serviceAccount := pod.Spec.ServiceAccountName
		if !hookPattern.MatchString(serviceAccount) {
			continue
		}
		if serviceAccount != i.rollout.HookServiceAccountName {
			return fmt.Errorf("pre-staged hook Pod %s/%s uses protected non-candidate ServiceAccount %s", pod.Namespace, pod.Name, serviceAccount)
		}
		if err := verifyIdentityJobPod(pod, identityJob); err != nil {
			return err
		}
		identityPodCount++
		if identityPodCount > 1 {
			return fmt.Errorf("candidate identity Job %s/%s has more than one protected Pod", identityJob.Namespace, identityJob.Name)
		}
	}
	if identityPodCount != 1 {
		return fmt.Errorf("candidate identity Job %s/%s has no exactly linked protected Pod", identityJob.Namespace, identityJob.Name)
	}
	return nil
}

// VerifyRuntimeBeforeQuiesce verifies every Pod using either long-running
// runtime ServiceAccount, independent of its labels, before a rollout changes
// the owning Deployments.
func (i *WorkloadInventory) VerifyRuntimeBeforeQuiesce(ctx context.Context) error {
	if err := i.runtimeIdentity(); err != nil {
		return err
	}
	if i.pods == nil || i.replicaSets == nil || i.deployments == nil {
		return fmt.Errorf("runtime workload inventory Pod, ReplicaSet, and Deployment clients are required")
	}
	replicaSetList, err := i.listReplicaSets(ctx)
	if err != nil {
		return fmt.Errorf("list runtime ReplicaSets before quiescence: %w", err)
	}
	replicaSets := make(map[string]*appsv1.ReplicaSet, len(replicaSetList))
	deployments := make(map[string]*appsv1.Deployment)
	for index := range replicaSetList {
		replicaSet := &replicaSetList[index]
		if _, found := replicaSets[replicaSet.Name]; found {
			return fmt.Errorf("runtime workload inventory returned ReplicaSet %s/%s more than once", replicaSet.Namespace, replicaSet.Name)
		}
		replicaSets[replicaSet.Name] = replicaSet
		deploymentName, component, protected := i.runtimeDeploymentForServiceAccount(replicaSet.Spec.Template.Spec.ServiceAccountName)
		if !protected {
			continue
		}
		deployment, err := i.runtimeDeployment(ctx, deploymentName, "ReplicaSet "+replicaSet.Namespace+"/"+replicaSet.Name, deployments)
		if err != nil {
			return err
		}
		if err := i.verifyRuntimeReplicaSet(replicaSet, deployment, deploymentName, component); err != nil {
			return err
		}
	}

	pods, err := i.listPods(ctx)
	if err != nil {
		return fmt.Errorf("list runtime Pods before quiescence: %w", err)
	}
	for index := range pods {
		pod := &pods[index]
		deploymentName, _, protected := i.runtimeDeploymentForServiceAccount(pod.Spec.ServiceAccountName)
		if !protected {
			continue
		}
		if err := i.verifyRuntimePod(pod, deploymentName, replicaSets); err != nil {
			return err
		}
	}
	return nil
}

// ProtectedRuntimePodsRemain reports whether any Pod in the namespace still
// uses a protected runtime ServiceAccount. It intentionally ignores labels so
// it can be used as the final namespace-wide quiescence condition.
func (i *WorkloadInventory) ProtectedRuntimePodsRemain(ctx context.Context) (bool, error) {
	if err := i.runtimeIdentity(); err != nil {
		return false, err
	}
	if i.pods == nil {
		return false, fmt.Errorf("runtime workload inventory Pod client is required")
	}
	pods, err := i.listPods(ctx)
	if err != nil {
		return false, fmt.Errorf("list protected runtime Pods: %w", err)
	}
	for index := range pods {
		if _, _, protected := i.runtimeDeploymentForServiceAccount(pods[index].Spec.ServiceAccountName); protected {
			return true, nil
		}
	}
	return false, nil
}

func (i *WorkloadInventory) hookIdentity() (*regexp.Regexp, string, error) {
	if i == nil || i.rollout == nil {
		return nil, "", fmt.Errorf("hook workload inventory rollout identity is required")
	}
	g := i.rollout
	for description, value := range map[string]string{
		"release name":             g.ReleaseName,
		"release namespace":        g.ReleaseNamespace,
		"hook ServiceAccount name": g.HookServiceAccountName,
		"manager image":            g.ManagerImage,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return nil, "", fmt.Errorf("hook workload inventory %s is required and must not contain surrounding whitespace", description)
		}
	}
	if g.ReleaseSequence < 1 {
		return nil, "", fmt.Errorf("hook workload inventory release sequence must be positive")
	}
	base, err := NewServiceAccountOriginGuard(g, nil).hookServiceAccountBase()
	if err != nil {
		return nil, "", fmt.Errorf("derive hook workload inventory identity: %w", err)
	}
	pattern, err := regexp.Compile("^(?:" + regexp.QuoteMeta(base+"-crd-v") + `|` + regexp.QuoteMeta(base+"-cleanup-v") + `)[1-9][0-9]*-[0-9a-f]{12}$`)
	if err != nil {
		return nil, "", fmt.Errorf("compile hook workload inventory identity: %w", err)
	}
	return pattern, HookIdentityProbeJobName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage), nil
}

func (i *WorkloadInventory) runtimeIdentity() error {
	if i == nil || i.rollout == nil {
		return fmt.Errorf("runtime workload inventory rollout identity is required")
	}
	g := i.rollout
	for description, value := range map[string]string{
		"release name":                   g.ReleaseName,
		"release namespace":              g.ReleaseNamespace,
		"controller ServiceAccount name": g.ControllerServiceAccountName,
		"controller Deployment name":     g.ControllerDeploymentName,
		"certificate Deployment name":    g.CertificateDeploymentName,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("runtime workload inventory %s is required and must not contain surrounding whitespace", description)
		}
	}
	if g.ControllerServiceAccountName == g.CertificateDeploymentName {
		return fmt.Errorf("runtime workload inventory ServiceAccount names must differ")
	}
	if g.ControllerDeploymentName == g.CertificateDeploymentName {
		return fmt.Errorf("runtime workload inventory Deployment names must differ")
	}
	return nil
}

func (i *WorkloadInventory) listJobs(ctx context.Context) ([]batchv1.Job, error) {
	var result []batchv1.Job
	state := newInventoryPageState()
	continueToken := ""
	for {
		page, err := i.jobs.List(ctx, metav1.ListOptions{Limit: workloadInventoryPageSize, Continue: continueToken})
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, fmt.Errorf("Job inventory returned a nil page")
		}
		if err := state.validatePage("Job", continueToken, page.ListMeta, len(page.Items), workloadInventoryPageSize); err != nil {
			return nil, err
		}
		for index := range page.Items {
			job := &page.Items[index]
			if err := i.verifyListedObject("Job", job.Namespace, job.Name); err != nil {
				return nil, err
			}
			if err := state.observeObject("Job", job.Namespace, job.Name, string(job.UID)); err != nil {
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

func (i *WorkloadInventory) listPods(ctx context.Context) ([]corev1.Pod, error) {
	var result []corev1.Pod
	state := newInventoryPageState()
	continueToken := ""
	for {
		page, err := i.pods.List(ctx, metav1.ListOptions{Limit: workloadInventoryPageSize, Continue: continueToken})
		if err != nil {
			return nil, fmt.Errorf("list Pods in namespace %s: %w", i.rollout.ReleaseNamespace, err)
		}
		if page == nil {
			return nil, fmt.Errorf("list Pods in namespace %s returned a nil page", i.rollout.ReleaseNamespace)
		}
		if err := state.validatePage("Pod", continueToken, page.ListMeta, len(page.Items), workloadInventoryPageSize); err != nil {
			return nil, err
		}
		for index := range page.Items {
			pod := &page.Items[index]
			if err := i.verifyListedObject("Pod", pod.Namespace, pod.Name); err != nil {
				return nil, err
			}
			if err := state.observeObject("Pod", pod.Namespace, pod.Name, string(pod.UID)); err != nil {
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

func (i *WorkloadInventory) listReplicaSets(ctx context.Context) ([]appsv1.ReplicaSet, error) {
	var result []appsv1.ReplicaSet
	state := newInventoryPageState()
	continueToken := ""
	for {
		page, err := i.replicaSets.List(ctx, metav1.ListOptions{Limit: workloadInventoryPageSize, Continue: continueToken})
		if err != nil {
			return nil, fmt.Errorf("list ReplicaSets in namespace %s: %w", i.rollout.ReleaseNamespace, err)
		}
		if page == nil {
			return nil, fmt.Errorf("list ReplicaSets in namespace %s returned a nil page", i.rollout.ReleaseNamespace)
		}
		if err := state.validatePage("ReplicaSet", continueToken, page.ListMeta, len(page.Items), workloadInventoryPageSize); err != nil {
			return nil, err
		}
		for index := range page.Items {
			replicaSet := &page.Items[index]
			if err := i.verifyListedObject("ReplicaSet", replicaSet.Namespace, replicaSet.Name); err != nil {
				return nil, err
			}
			if err := state.observeObject("ReplicaSet", replicaSet.Namespace, replicaSet.Name, string(replicaSet.UID)); err != nil {
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

type inventoryPageState struct {
	initialized     bool
	resourceVersion string
	seenContinue    map[string]struct{}
	seenObjects     map[string]struct{}
	seenUIDs        map[string]string
}

func newInventoryPageState() *inventoryPageState {
	return &inventoryPageState{
		seenContinue: make(map[string]struct{}),
		seenObjects:  make(map[string]struct{}),
		seenUIDs:     make(map[string]string),
	}
}

func (s *inventoryPageState) validatePage(kind, currentToken string, metadata metav1.ListMeta, count int, pageSize int64) error {
	if metadata.ShardInfo != nil {
		return fmt.Errorf("%s inventory returned a sharded subset", kind)
	}
	if count > int(pageSize) {
		return fmt.Errorf("%s inventory page contains %d objects, exceeding requested limit %d", kind, count, pageSize)
	}
	if count == 0 && (currentToken != "" || metadata.Continue != "") {
		return fmt.Errorf("%s inventory returned an empty continued page", kind)
	}
	if s.initialized && metadata.ResourceVersion != s.resourceVersion {
		return fmt.Errorf("%s inventory resourceVersion changed from %q to %q across pages", kind, s.resourceVersion, metadata.ResourceVersion)
	}
	if !s.initialized {
		s.initialized = true
		s.resourceVersion = metadata.ResourceVersion
	}
	if metadata.RemainingItemCount != nil {
		remaining := *metadata.RemainingItemCount
		if remaining < 0 || (metadata.Continue == "" && remaining != 0) || (metadata.Continue != "" && remaining == 0) {
			return fmt.Errorf("%s inventory returned malformed remainingItemCount %d with continue token %q", kind, remaining, metadata.Continue)
		}
	}
	if metadata.Continue != "" {
		if metadata.Continue == currentToken {
			return fmt.Errorf("%s inventory repeated current continue token %q", kind, currentToken)
		}
		if _, found := s.seenContinue[metadata.Continue]; found {
			return fmt.Errorf("%s inventory repeated prior continue token %q", kind, metadata.Continue)
		}
		s.seenContinue[metadata.Continue] = struct{}{}
	}
	return nil
}

func (s *inventoryPageState) observeObject(kind, namespace, name, uid string) error {
	object := name
	if namespace != "" {
		object = namespace + "/" + name
	}
	key := namespace + "\x00" + name
	if _, found := s.seenObjects[key]; found {
		return fmt.Errorf("%s inventory returned %s more than once", kind, object)
	}
	s.seenObjects[key] = struct{}{}
	if uid == "" {
		return nil
	}
	if previous, found := s.seenUIDs[uid]; found {
		return fmt.Errorf("%s inventory objects %s and %s share UID %s", kind, previous, object, uid)
	}
	s.seenUIDs[uid] = object
	return nil
}

func (i *WorkloadInventory) verifyListedObject(kind, namespace, name string) error {
	if namespace != i.rollout.ReleaseNamespace || name == "" {
		return fmt.Errorf("%s inventory returned foreign or incomplete %s %q", i.rollout.ReleaseNamespace, kind, namespacedInventoryName(namespace, name))
	}
	return nil
}

func verifyIdentityJobPod(pod *corev1.Pod, job *batchv1.Job) error {
	if pod == nil || job == nil {
		return fmt.Errorf("candidate identity Job Pod or parent Job is nil")
	}
	object := pod.Namespace + "/" + pod.Name
	if pod.UID == "" {
		return fmt.Errorf("candidate identity Pod %s has no UID", object)
	}
	if pod.Spec.ServiceAccountName != job.Spec.Template.Spec.ServiceAccountName {
		return fmt.Errorf("candidate identity Pod %s uses ServiceAccount %q instead of parent Job ServiceAccount %q", object, pod.Spec.ServiceAccountName, job.Spec.Template.Spec.ServiceAccountName)
	}
	if err := verifyExactControllerReference("candidate identity Pod "+object, pod.OwnerReferences, "batch/v1", "Job", job.Name, job.UID); err != nil {
		return err
	}
	uid := string(job.UID)
	for key, expected := range map[string]string{
		batchv1.ControllerUidLabel: uid,
		batchv1.JobNameLabel:       job.Name,
		"controller-uid":           uid,
		"job-name":                 job.Name,
	} {
		if pod.Labels[key] != expected {
			return fmt.Errorf("candidate identity Pod %s label %s is %q instead of %q", object, key, pod.Labels[key], expected)
		}
	}
	if err := verifyGeneratedObjectName("candidate identity Pod "+object, pod.Name, pod.GenerateName, job.Name+"-"); err != nil {
		return err
	}
	return nil
}

func (i *WorkloadInventory) runtimeDeploymentForServiceAccount(serviceAccount string) (string, string, bool) {
	switch serviceAccount {
	case i.rollout.ControllerServiceAccountName:
		return i.rollout.ControllerDeploymentName, "controller", true
	case i.rollout.CertificateDeploymentName:
		return i.rollout.CertificateDeploymentName, "certificate-rotation", true
	default:
		return "", "", false
	}
}

func (i *WorkloadInventory) verifyRuntimePod(pod *corev1.Pod, deploymentName string, replicaSets map[string]*appsv1.ReplicaSet) error {
	object := pod.Namespace + "/" + pod.Name
	if pod.UID == "" {
		return fmt.Errorf("protected runtime Pod %s has no UID", object)
	}
	if len(pod.OwnerReferences) != 1 {
		return fmt.Errorf("protected runtime Pod %s must have exactly one ReplicaSet owner reference", object)
	}
	owner := pod.OwnerReferences[0]
	if owner.APIVersion != "apps/v1" || owner.Kind != "ReplicaSet" || owner.Name == "" || owner.UID == "" || owner.Controller == nil || !*owner.Controller || owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
		return fmt.Errorf("protected runtime Pod %s has an incomplete or non-controlling ReplicaSet owner", object)
	}

	replicaSet, found := replicaSets[owner.Name]
	if !found {
		return fmt.Errorf("protected runtime Pod %s references ReplicaSet %s/%s outside the bounded namespace inventory", object, i.rollout.ReleaseNamespace, owner.Name)
	}
	if replicaSet.Namespace != i.rollout.ReleaseNamespace || replicaSet.Name != owner.Name || replicaSet.UID == "" || replicaSet.UID != owner.UID {
		return fmt.Errorf("protected runtime Pod %s references foreign or stale ReplicaSet %s/%s UID %q", object, i.rollout.ReleaseNamespace, owner.Name, owner.UID)
	}
	if replicaSet.Spec.Template.Spec.ServiceAccountName != pod.Spec.ServiceAccountName {
		return fmt.Errorf("protected runtime Pod %s ServiceAccount %s does not match live ReplicaSet %s/%s template", object, pod.Spec.ServiceAccountName, replicaSet.Namespace, replicaSet.Name)
	}
	if !reflect.DeepEqual(pod.Labels, replicaSet.Spec.Template.Labels) {
		return fmt.Errorf("protected runtime Pod %s labels do not match live ReplicaSet %s/%s template", object, replicaSet.Namespace, replicaSet.Name)
	}
	if err := verifyGeneratedObjectName("protected runtime Pod "+object, pod.Name, pod.GenerateName, replicaSet.Name+"-"); err != nil {
		return err
	}
	if !strings.HasPrefix(replicaSet.Name, deploymentName+"-") {
		return fmt.Errorf("protected runtime Pod %s ReplicaSet %s/%s is not linked to expected Deployment %s", object, replicaSet.Namespace, replicaSet.Name, deploymentName)
	}
	return nil
}

func (i *WorkloadInventory) runtimeDeployment(
	ctx context.Context,
	deploymentName string,
	child string,
	deployments map[string]*appsv1.Deployment,
) (*appsv1.Deployment, error) {
	if deployment, found := deployments[deploymentName]; found {
		return deployment, nil
	}
	deployment, err := i.deployments.Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get expected Deployment %s/%s for %s: %w", i.rollout.ReleaseNamespace, deploymentName, child, err)
	}
	if deployment == nil {
		return nil, fmt.Errorf("get expected Deployment %s/%s for %s returned a nil result", i.rollout.ReleaseNamespace, deploymentName, child)
	}
	deployments[deploymentName] = deployment
	return deployment, nil
}

func (i *WorkloadInventory) verifyRuntimeReplicaSet(
	replicaSet *appsv1.ReplicaSet,
	deployment *appsv1.Deployment,
	deploymentName string,
	component string,
) error {
	replicaSetObject := replicaSet.Namespace + "/" + replicaSet.Name
	if deployment.Namespace != i.rollout.ReleaseNamespace || deployment.Name != deploymentName || deployment.UID == "" {
		return fmt.Errorf("protected runtime ReplicaSet %s resolves to foreign or incomplete Deployment %s/%s", replicaSetObject, i.rollout.ReleaseNamespace, deploymentName)
	}
	target := deploymentTarget{name: deploymentName, component: component}
	if err := i.rollout.verifyDeployment(target, deployment); err != nil {
		return fmt.Errorf("protected runtime ReplicaSet %s: %w", replicaSetObject, err)
	}
	if err := verifyExactControllerReference("ReplicaSet "+replicaSetObject, replicaSet.OwnerReferences, "apps/v1", "Deployment", deploymentName, deployment.UID); err != nil {
		return fmt.Errorf("protected runtime ReplicaSet %s: %w", replicaSetObject, err)
	}

	serviceAccount := replicaSet.Spec.Template.Spec.ServiceAccountName
	if deployment.Spec.Template.Spec.ServiceAccountName != serviceAccount {
		return fmt.Errorf("protected runtime ReplicaSet %s ServiceAccount %s does not match expected Deployment template", replicaSetObject, serviceAccount)
	}
	if deployment.Spec.Selector == nil || len(deployment.Spec.Selector.MatchExpressions) != 0 || len(deployment.Spec.Selector.MatchLabels) != 3 {
		return fmt.Errorf("expected Deployment %s/%s has an unsafe runtime selector", deployment.Namespace, deployment.Name)
	}
	deploymentSelector := deployment.Spec.Selector.MatchLabels
	for key, expected := range map[string]string{
		"app.kubernetes.io/instance":  i.rollout.ReleaseName,
		"app.kubernetes.io/component": component,
	} {
		if deploymentSelector[key] != expected {
			return fmt.Errorf("expected Deployment %s/%s selector %s is %q instead of %q", deployment.Namespace, deployment.Name, key, deploymentSelector[key], expected)
		}
	}
	if name := deploymentSelector["app.kubernetes.io/name"]; name == "" {
		return fmt.Errorf("expected Deployment %s/%s selector has no app.kubernetes.io/name", deployment.Namespace, deployment.Name)
	}
	if !stringMapContains(deployment.Spec.Template.Labels, deploymentSelector) {
		return fmt.Errorf("expected Deployment %s/%s template labels do not contain its exact selector", deployment.Namespace, deployment.Name)
	}

	hash := replicaSet.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
	if !workloadInventoryHashPattern.MatchString(hash) {
		return fmt.Errorf("protected runtime ReplicaSet %s has invalid pod-template-hash %q", replicaSetObject, hash)
	}
	if replicaSet.Name != deploymentName+"-"+hash || replicaSet.GenerateName != "" {
		return fmt.Errorf("ReplicaSet %s is not named from expected Deployment %s and hash %s", replicaSetObject, deploymentName, hash)
	}

	expectedSelector := copyStringMap(deploymentSelector)
	expectedSelector[appsv1.DefaultDeploymentUniqueLabelKey] = hash
	if replicaSet.Spec.Selector == nil || len(replicaSet.Spec.Selector.MatchExpressions) != 0 || !reflect.DeepEqual(replicaSet.Spec.Selector.MatchLabels, expectedSelector) {
		return fmt.Errorf("ReplicaSet %s selector does not extend expected Deployment %s by its exact hash", replicaSetObject, deploymentName)
	}
	if !reflect.DeepEqual(replicaSet.Labels, replicaSet.Spec.Template.Labels) || !stringMapContains(replicaSet.Labels, expectedSelector) {
		return fmt.Errorf("protected runtime ReplicaSet %s labels do not contain its exact Deployment selector and hash chain", replicaSetObject)
	}
	return nil
}

func verifyExactControllerReference(description string, references []metav1.OwnerReference, apiVersion, kind, name string, uid types.UID) error {
	if len(references) != 1 {
		return fmt.Errorf("%s must have exactly one %s owner reference", description, kind)
	}
	reference := references[0]
	if reference.APIVersion != apiVersion || reference.Kind != kind || reference.Name != name || reference.UID != uid || reference.UID == "" || reference.Controller == nil || !*reference.Controller || reference.BlockOwnerDeletion == nil || !*reference.BlockOwnerDeletion {
		return fmt.Errorf("%s has an incomplete, stale, or non-controlling %s owner reference", description, kind)
	}
	return nil
}

func verifyGeneratedObjectName(description, name, generateName, expectedPrefix string) error {
	if generateName != expectedPrefix {
		return fmt.Errorf("%s name %q and generateName %q do not link to parent prefix %q", description, name, generateName, expectedPrefix)
	}
	effectivePrefix := expectedPrefix
	if len(effectivePrefix) > workloadInventoryGeneratedNameMaxPrefixLength {
		// Kubernetes names are ASCII, so the API server's byte truncation also
		// preserves the first 58 characters of the submitted generateName.
		effectivePrefix = effectivePrefix[:workloadInventoryGeneratedNameMaxPrefixLength]
	}
	if !strings.HasPrefix(name, effectivePrefix) {
		return fmt.Errorf("%s name %q and generateName %q do not link to parent prefix %q", description, name, generateName, expectedPrefix)
	}
	suffix := strings.TrimPrefix(name, effectivePrefix)
	if !workloadInventorySuffixPattern.MatchString(suffix) {
		return fmt.Errorf("%s name %q does not have one exact generated-name suffix", description, name)
	}
	return nil
}

func stringMapContains(actual, required map[string]string) bool {
	if len(actual) < len(required) {
		return false
	}
	for key, value := range required {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func copyStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func namespacedInventoryName(namespace, name string) string {
	if namespace == "" {
		namespace = "<empty>"
	}
	if name == "" {
		name = "<empty>"
	}
	return namespace + "/" + name
}
