package crdupgrade

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

const (
	// Guard policy names are versioned and append-only. Older policies permit a
	// higher release sequence, while the newest policy binds its own sequence
	// to one exact image. A rollback cannot remove a newer guard through Helm.
	rolloutGuardNamePrefix = "ptah-operator-rollout-guard-v"
	runtimeGuardNamePrefix = "ptah-operator-runtime-guard-v"
	hookGuardNamePrefix    = "ptah-operator-hook-identity-v"
	hookProbeGuardPrefix   = "ptah-operator-hook-probe-guard-v"

	// ReleaseSequenceAnnotation must increase for every published operator
	// release, even when its stored-state and admission contracts stay stable.
	ReleaseSequenceAnnotation = "operator.ptah.dev/release-sequence"
	// ManagerImageAnnotation records the exact image accepted by the runtime
	// guard at the current release sequence.
	ManagerImageAnnotation = "operator.ptah.dev/manager-image"
	// CurrentReleaseSequence is mirrored by the Helm helper and release gates.
	CurrentReleaseSequence int32 = 1

	rolloutGuardVersionAnnotation   = "operator.ptah.dev/rollout-guard-version"
	rolloutGuardVersion             = "1"
	rolloutGuardComponent           = "rollout-guard"
	rolloutGuardManagedBy           = "ptah-operator"
	guardEnforcementProbeAnnotation = "operator.ptah.dev/guard-enforcement-probe"
	kubernetesDNSLabelMaxLength     = 63
	kubernetesGeneratedSuffixLen    = 5

	// ReleaseActivationName is the retained namespaced parameter consulted by
	// every rollout/runtime guard before accepting a higher release sequence.
	ReleaseActivationName = "ptah-operator-release-activation"
	activeReleaseDataKey  = "active-release-sequence"
)

// RolloutGuardPolicyName returns the append-only metadata/admission ratchet
// name for a release sequence.
func RolloutGuardPolicyName(sequence int32) string {
	return rolloutGuardNamePrefix + strconv.FormatInt(int64(sequence), 10)
}

// RuntimeGuardPolicyName returns the append-only exact-image ratchet name for
// a release sequence.
func RuntimeGuardPolicyName(sequence int32) string {
	return runtimeGuardNamePrefix + strconv.FormatInt(int64(sequence), 10)
}

// HookIdentityGuardPolicyName returns the immutable policy name for one
// release/image hook identity. Hook ServiceAccounts use the same digest so a
// failed hook can never leave a reusable credential for a later candidate.
func HookIdentityGuardPolicyName(releaseNamespace, releaseName string, sequence int32, managerImage string) string {
	digest := hookIdentityDigest(releaseNamespace, releaseName, sequence, managerImage)
	return fmt.Sprintf("%s%d-%s", hookGuardNamePrefix, sequence, digest[:12])
}

// HookIdentityProbeGuardPolicyName returns the immutable name of the
// ConfigMap-only policy used to prove admission enforcement for one hook
// identity without mixing unrelated object schemas in the Pod policy.
func HookIdentityProbeGuardPolicyName(releaseNamespace, releaseName string, sequence int32, managerImage string) string {
	digest := hookIdentityDigest(releaseNamespace, releaseName, sequence, managerImage)
	return fmt.Sprintf("%s%d-%s", hookProbeGuardPrefix, sequence, digest[:12])
}

// HookIdentityProbeJobName returns the unprivileged enforcement-probe Job
// name for one hook identity.
func HookIdentityProbeJobName(releaseNamespace, releaseName string, sequence int32, managerImage string) string {
	digest := hookIdentityDigest(releaseNamespace, releaseName, sequence, managerImage)
	return fmt.Sprintf("ptah-hook-identity-v%d-%s", sequence, digest[:12])
}

// HookIdentityProbeObjectName returns the harmless dry-run ConfigMap name used
// to prove that the hook identity binding is actively denying requests.
func HookIdentityProbeObjectName(releaseNamespace, releaseName string, sequence int32, managerImage string) string {
	digest := hookIdentityDigest(releaseNamespace, releaseName, sequence, managerImage)
	return fmt.Sprintf("ptah-hook-probe-v%d-%s", sequence, digest[:12])
}

func hookIdentityDigest(releaseNamespace, releaseName string, sequence int32, managerImage string) string {
	identity := strings.Join([]string{releaseNamespace, releaseName, strconv.FormatInt(int64(sequence), 10), managerImage}, "\n")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
}

func rolloutGuardDenialMessage(sequence int32) string {
	return fmt.Sprintf("Ptah rollout guard v%d rejected an unsafe release transition", sequence)
}

func runtimeGuardDenialMessage(sequence int32) string {
	return fmt.Sprintf("Ptah runtime guard v%d rejected a non-candidate controller image", sequence)
}

func rolloutGuardProbeDenialMessage(sequence int32) string {
	return fmt.Sprintf("Ptah rollout guard v%d rejected its exact enforcement probe", sequence)
}

func runtimeGuardProbeDenialMessage(sequence int32) string {
	return fmt.Sprintf("Ptah runtime guard v%d rejected its exact enforcement probe", sequence)
}

func rolloutGuardNameBoundaryDenialMessage(sequence int32) string {
	return fmt.Sprintf("Ptah rollout guard v%d rejected an arbitrary hook Deployment name", sequence)
}

func hookIdentityGuardDenialMessage(sequence int32) string {
	return fmt.Sprintf("Ptah hook identity guard v%d rejected an unsafe privileged hook Pod", sequence)
}

func hookIdentityProbeGuardDenialMessage(sequence int32) string {
	return fmt.Sprintf("Ptah hook identity probe guard v%d rejected the enforcement probe", sequence)
}

// ValidatingAdmissionPolicyReader is the read-only API surface used to verify
// the Helm-actor-created persistent release ratchet.
type ValidatingAdmissionPolicyReader interface {
	Get(context.Context, string, metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicy, error)
}

// ValidatingAdmissionPolicyBindingReader is the read-only API surface used to
// verify the Helm-actor-created persistent release ratchet.
type ValidatingAdmissionPolicyBindingReader interface {
	Get(context.Context, string, metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error)
}

// DeploymentWriter is the namespaced API surface required to adopt and
// quiesce the operator Deployments and probe their admission boundaries.
type DeploymentWriter interface {
	Get(context.Context, string, metav1.GetOptions) (*appsv1.Deployment, error)
	Create(context.Context, *appsv1.Deployment, metav1.CreateOptions) (*appsv1.Deployment, error)
	Update(context.Context, *appsv1.Deployment, metav1.UpdateOptions) (*appsv1.Deployment, error)
}

// PodLister is used to prove that no old release Pod can remain behind the
// admission Service when Helm begins applying ordinary resources.
type PodLister interface {
	List(context.Context, metav1.ListOptions) (*corev1.PodList, error)
}

// ConfigMapWriter is the narrow API surface used for the harmless dry-run
// denial probe and the durable release-activation ratchet.
type ConfigMapWriter interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error)
	Create(context.Context, *corev1.ConfigMap, metav1.CreateOptions) (*corev1.ConfigMap, error)
	Update(context.Context, *corev1.ConfigMap, metav1.UpdateOptions) (*corev1.ConfigMap, error)
}

// RolloutGuard verifies the persistent admission-policy ratchet and performs
// the explicit old-runtime quiescence step. Helm creates each versioned guard
// as an earlier, retained hook resource; this process has read-only access to
// the guards and cannot weaken them.
type RolloutGuard struct {
	Policies                           ValidatingAdmissionPolicyReader
	Bindings                           ValidatingAdmissionPolicyBindingReader
	Deployments                        DeploymentWriter
	Pods                               PodLister
	ConfigMaps                         ConfigMapWriter
	ReleaseName                        string
	ReleaseNamespace                   string
	CoordinationNamespace              string
	LeaderElection                     bool
	LeaderElectionID                   string
	WebhookServiceName                 string
	WebhookTimeoutSeconds              int32
	WebhookSecretName                  string
	WebhookPort                        int32
	CertificateHealthPort              int32
	HookServiceAccountName             string
	ControllerServiceAccountName       string
	ControllerDeploymentName           string
	ControllerReplicas                 int32
	CertificateDeploymentName          string
	ControllerStateVersion             int32
	AdmissionContractVersion           int32
	ReleaseSequence                    int32
	ManagerImage                       string
	ControllerArgs                     []string
	CertificateArgs                    []string
	RuntimeDeploymentConfigExpressions []string
	RuntimePodConfigExpressions        []string
	PriorityClassName                  string
	RuntimeAdmissionContractB64        string
	PollEvery                          time.Duration
}

// Prepare establishes and proves the API-server-side ratchet before adopting
// or changing any release-owned object.
func (g *RolloutGuard) Prepare(ctx context.Context) error {
	if err := g.validate(); err != nil {
		return err
	}
	if err := g.Verify(ctx); err != nil {
		return err
	}
	if err := NewControllerWriteGuard(g).WaitReady(ctx); err != nil {
		return fmt.Errorf("wait for controller write guard: %w", err)
	}
	if err := NewCertificateWriteGuard(g).WaitReady(ctx); err != nil {
		return fmt.Errorf("wait for certificate write guards: %w", err)
	}
	if err := NewControllerObjectGuard(g).WaitReady(ctx); err != nil {
		return fmt.Errorf("wait for controller object guards: %w", err)
	}
	if err := NewParentWorkloadGuard(g).WaitReady(ctx); err != nil {
		return fmt.Errorf("wait for parent workload guards: %w", err)
	}
	if err := g.releaseActivationGuard().Prepare(ctx); err != nil {
		return err
	}
	if err := g.waitPoliciesReady(ctx); err != nil {
		return err
	}
	rolloutName := RolloutGuardPolicyName(g.ReleaseSequence)
	if err := g.waitEnforced(ctx, rolloutName, rolloutGuardProbeDenialMessage(g.ReleaseSequence)); err != nil {
		return err
	}
	if err := g.waitRolloutCreateBoundaryEnforced(ctx); err != nil {
		return err
	}
	runtimeName := RuntimeGuardPolicyName(g.ReleaseSequence)
	if err := g.waitEnforced(ctx, runtimeName, runtimeGuardProbeDenialMessage(g.ReleaseSequence)); err != nil {
		return err
	}
	return nil
}

// Verify requires both persistent policies and bindings to match this exact
// runtime. It is used by every long-running Pod init container, so bypassing
// Helm hooks cannot start an unguarded manager or certificate rotator.
func (g *RolloutGuard) Verify(ctx context.Context) error {
	if err := g.validateIdentity(); err != nil {
		return err
	}
	if err := NewNamespaceDeletionGuard(g).Verify(ctx); err != nil {
		return fmt.Errorf("verify namespace deletion guard: %w", err)
	}
	if err := NewControllerWriteGuard(g).Verify(ctx); err != nil {
		return fmt.Errorf("verify controller write guard: %w", err)
	}
	if err := NewCertificateWriteGuard(g).Verify(ctx); err != nil {
		return fmt.Errorf("verify certificate write guards: %w", err)
	}
	if err := NewControllerObjectGuard(g).Verify(ctx); err != nil {
		return fmt.Errorf("verify controller object guards: %w", err)
	}
	if err := NewParentWorkloadGuard(g).Verify(ctx); err != nil {
		return fmt.Errorf("verify parent workload guards: %w", err)
	}
	if err := NewServiceAccountOriginGuard(g, nil).Verify(ctx); err != nil {
		return fmt.Errorf("verify service account origin guard: %w", err)
	}
	if err := g.releaseActivationGuard().Verify(ctx); err != nil {
		return err
	}
	rolloutName := RolloutGuardPolicyName(g.ReleaseSequence)
	runtimeName := RuntimeGuardPolicyName(g.ReleaseSequence)
	policy, err := g.Policies.Get(ctx, rolloutName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get rollout guard policy: %w", err)
	}
	state, admission, err := g.verifyPolicy(policy)
	if err != nil {
		return err
	}
	if state != g.ControllerStateVersion || admission != g.AdmissionContractVersion {
		return fmt.Errorf("rollout guard policy floors do not match the candidate runtime")
	}
	runtimePolicy, err := g.Policies.Get(ctx, runtimeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get runtime guard policy: %w", err)
	}
	state, sequence, image, err := g.verifyRuntimePolicy(runtimePolicy)
	if err != nil {
		return err
	}
	if state != g.ControllerStateVersion || sequence != g.ReleaseSequence || image != g.ManagerImage {
		return fmt.Errorf("runtime guard policy identity does not match the candidate runtime")
	}
	runtimePodName := RuntimePodGuardPolicyName(g.ReleaseSequence)
	runtimePodPolicy, err := g.Policies.Get(ctx, runtimePodName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get runtime Pod identity guard policy: %w", err)
	}
	if err := g.verifyRuntimePodIdentityPolicy(runtimePodPolicy); err != nil {
		return err
	}
	hookName := HookIdentityGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	hookPolicy, err := g.Policies.Get(ctx, hookName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get hook identity guard policy: %w", err)
	}
	if err := g.verifyHookIdentityPolicy(hookPolicy); err != nil {
		return err
	}
	hookProbeName := HookIdentityProbeGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	hookProbePolicy, err := g.Policies.Get(ctx, hookProbeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get hook identity probe guard policy: %w", err)
	}
	if err := g.verifyHookIdentityProbePolicy(hookProbePolicy); err != nil {
		return err
	}
	for _, name := range []string{rolloutName, runtimeName, hookName, hookProbeName} {
		binding, err := g.Bindings.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get %s binding: %w", name, err)
		}
		if err := g.verifyBinding(binding, name); err != nil {
			return err
		}
	}
	runtimePodBinding, err := g.Bindings.Get(ctx, runtimePodName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get runtime Pod identity guard binding: %w", err)
	}
	if err := g.verifyRuntimePodIdentityBinding(runtimePodBinding); err != nil {
		return err
	}
	return nil
}

// VerifyHookIdentity checks the retained Pod admission boundary before a hook
// process uses its narrowly privileged ServiceAccount.
func (g *RolloutGuard) VerifyHookIdentity(ctx context.Context) error {
	if err := g.validateIdentity(); err != nil {
		return err
	}
	if err := NewNamespaceDeletionGuard(g).Verify(ctx); err != nil {
		return fmt.Errorf("verify namespace deletion guard: %w", err)
	}
	if err := NewControllerWriteGuard(g).Verify(ctx); err != nil {
		return fmt.Errorf("verify controller write guard: %w", err)
	}
	if err := NewCertificateWriteGuard(g).Verify(ctx); err != nil {
		return fmt.Errorf("verify certificate write guards: %w", err)
	}
	if err := NewControllerObjectGuard(g).Verify(ctx); err != nil {
		return fmt.Errorf("verify controller object guards: %w", err)
	}
	if err := NewParentWorkloadGuard(g).Verify(ctx); err != nil {
		return fmt.Errorf("verify parent workload guards: %w", err)
	}
	if err := NewServiceAccountOriginGuard(g, nil).Verify(ctx); err != nil {
		return fmt.Errorf("verify service account origin guard: %w", err)
	}
	name := HookIdentityGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	policy, err := g.Policies.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get hook identity guard policy: %w", err)
	}
	if err := g.verifyHookIdentityPolicy(policy); err != nil {
		return err
	}
	probeName := HookIdentityProbeGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	probePolicy, err := g.Policies.Get(ctx, probeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get hook identity probe guard policy: %w", err)
	}
	if err := g.verifyHookIdentityProbePolicy(probePolicy); err != nil {
		return err
	}
	for _, policyName := range []string{name, probeName} {
		binding, err := g.Bindings.Get(ctx, policyName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get %s binding: %w", policyName, err)
		}
		if err := g.verifyBinding(binding, policyName); err != nil {
			return err
		}
	}
	return nil
}

// PrepareHookIdentity proves both policy readiness and live denial before Helm
// grants this ServiceAccount any schema or admission mutation permission.
func (g *RolloutGuard) PrepareHookIdentity(ctx context.Context) error {
	if g.ConfigMaps == nil {
		return fmt.Errorf("hook identity ConfigMap client is required")
	}
	if err := g.VerifyHookIdentity(ctx); err != nil {
		return err
	}
	if err := NewNamespaceDeletionGuard(g).WaitReady(ctx); err != nil {
		return fmt.Errorf("wait for namespace deletion guard: %w", err)
	}
	if err := NewControllerWriteGuard(g).WaitReady(ctx); err != nil {
		return fmt.Errorf("wait for controller write guard: %w", err)
	}
	if err := NewCertificateWriteGuard(g).WaitReady(ctx); err != nil {
		return fmt.Errorf("wait for certificate write guards: %w", err)
	}
	if err := NewControllerObjectGuard(g).WaitReady(ctx); err != nil {
		return fmt.Errorf("wait for controller object guards: %w", err)
	}
	if err := NewParentWorkloadGuard(g).WaitReady(ctx); err != nil {
		return fmt.Errorf("wait for parent workload guards: %w", err)
	}
	name := HookIdentityGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	if err := g.waitPolicyReady(ctx, name); err != nil {
		return err
	}
	if err := g.waitPolicyReady(ctx, HookIdentityProbeGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)); err != nil {
		return err
	}
	probeName := HookIdentityProbeObjectName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	probe, err := g.ConfigMaps.Get(ctx, probeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get hook identity denial marker: %w", err)
	}
	probe = probe.DeepCopy()
	probe.Data = map[string]string{"probe": "must-be-denied"}
	_, err = g.ConfigMaps.Update(ctx, probe, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
	if err == nil {
		return fmt.Errorf("hook identity guard denial probe unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), hookIdentityProbeGuardDenialMessage(g.ReleaseSequence)) {
		return fmt.Errorf("probe hook identity guard enforcement: %w", err)
	}
	return nil
}

// PreflightQuiesce validates ownership and dry-runs every atomic stamp-and-stop
// update without changing a Deployment. Helm runs it before guard activation.
func (g *RolloutGuard) PreflightQuiesce(ctx context.Context) error {
	return g.quiesce(ctx, false)
}

// Quiesce first stamps the durable state version onto legacy Deployments, then
// scales both long-running binaries to zero and waits until every selected Pod
// is gone. If Helm later fails, the cluster remains safely stopped and a retry
// with the candidate release can resume the transition.
func (g *RolloutGuard) Quiesce(ctx context.Context) error {
	return g.quiesce(ctx, true)
}

// Activate advances the durable release parameter only after every candidate
// guard is present, type-checked, and proven to deny an invalid transition.
// All retained older policies consult the same parameter, so a Deployment
// updater cannot bypass them by merely inventing a future annotation.
func (g *RolloutGuard) Activate(ctx context.Context) error {
	return g.releaseActivationGuard().Activate(ctx)
}

func (g *RolloutGuard) quiesce(ctx context.Context, apply bool) error {
	if err := g.validate(); err != nil {
		return err
	}
	targets := []deploymentTarget{
		{name: g.CertificateDeploymentName, component: "certificate-rotation"},
		{name: g.ControllerDeploymentName, component: "controller"},
	}
	existing := make([]deploymentTarget, 0, len(targets))
	for _, target := range targets {
		deployment, err := g.Deployments.Get(ctx, target.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("get %s Deployment %s/%s: %w", target.component, g.ReleaseNamespace, target.name, err)
		}
		if err := g.verifyDeployment(target, deployment); err != nil {
			return err
		}
		target.selector, err = metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
		if err != nil {
			return fmt.Errorf("convert %s Deployment selector: %w", target.component, err)
		}
		existing = append(existing, target)
	}

	// Prove every atomic stamp-and-stop transition through admission before the
	// first persistent Deployment mutation. A predecessor image is never marked
	// as current while it is allowed to keep running.
	for _, target := range existing {
		deployment, err := g.Deployments.Get(ctx, target.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("re-read %s Deployment before quiescence dry-run: %w", target.component, err)
		}
		candidate, _, err := g.quiescedDeployment(target, deployment)
		if err != nil {
			return err
		}
		if _, err := g.Deployments.Update(ctx, candidate, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
			return fmt.Errorf("dry-run %s Deployment quiescence: %w", target.component, err)
		}
	}
	if !apply {
		return nil
	}

	for _, target := range existing {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			deployment, err := g.Deployments.Get(ctx, target.name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			candidate, changed, err := g.quiescedDeployment(target, deployment)
			if err != nil || !changed {
				return err
			}
			_, err = g.Deployments.Update(ctx, candidate, metav1.UpdateOptions{})
			return err
		}); err != nil {
			return fmt.Errorf("quiesce %s Deployment: %w", target.component, err)
		}
		if err := g.waitDeploymentStopped(ctx, target); err != nil {
			return err
		}
	}
	return nil
}

type deploymentTarget struct {
	name      string
	component string
	selector  labels.Selector
}

func (g *RolloutGuard) validate() error {
	if g == nil || g.Policies == nil || g.Bindings == nil || g.Deployments == nil || g.Pods == nil || g.ConfigMaps == nil {
		return fmt.Errorf("rollout guard clients are required")
	}
	return g.validateIdentity()
}

func (g *RolloutGuard) validateIdentity() error {
	if g == nil || g.Policies == nil || g.Bindings == nil {
		return fmt.Errorf("rollout guard policy clients are required")
	}
	for name, value := range map[string]string{
		"release name":                    g.ReleaseName,
		"release namespace":               g.ReleaseNamespace,
		"coordination namespace":          g.CoordinationNamespace,
		"leader-election ID":              g.LeaderElectionID,
		"webhook Service name":            g.WebhookServiceName,
		"webhook Secret name":             g.WebhookSecretName,
		"hook service account name":       g.HookServiceAccountName,
		"controller service account name": g.ControllerServiceAccountName,
		"controller Deployment name":      g.ControllerDeploymentName,
		"certificate Deployment name":     g.CertificateDeploymentName,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if g.ControllerDeploymentName == g.CertificateDeploymentName {
		return fmt.Errorf("controller and certificate Deployment names must differ")
	}
	if g.ControllerReplicas < 1 {
		return fmt.Errorf("controller replicas must be positive")
	}
	if g.ControllerStateVersion < 1 {
		return fmt.Errorf("controller-state version must be positive")
	}
	if g.AdmissionContractVersion < 1 {
		return fmt.Errorf("admission-contract version must be positive")
	}
	if g.WebhookTimeoutSeconds < 1 || g.WebhookTimeoutSeconds > 30 {
		return fmt.Errorf("webhook timeout seconds must be between 1 and 30")
	}
	if g.WebhookPort < 1 || g.WebhookPort > 65535 || g.CertificateHealthPort < 1 || g.CertificateHealthPort > 65535 {
		return fmt.Errorf("runtime container ports must be between 1 and 65535")
	}
	if g.ReleaseSequence < 1 {
		return fmt.Errorf("release sequence must be positive")
	}
	if g.ManagerImage == "" {
		return fmt.Errorf("manager image is required")
	}
	if len(g.ControllerArgs) == 0 || len(g.CertificateArgs) == 0 {
		return fmt.Errorf("controller and certificate runtime arguments are required")
	}
	for name, expressions := range map[string][]string{
		"runtime Deployment config": g.RuntimeDeploymentConfigExpressions,
		"runtime Pod config":        g.RuntimePodConfigExpressions,
	} {
		if len(expressions) == 0 {
			return fmt.Errorf("%s expressions are required", name)
		}
		for index, expression := range expressions {
			if expression == "" || expression != strings.TrimSpace(expression) || len(expression) > 16*1024 {
				return fmt.Errorf("%s expression %d is empty, padded, or too large", name, index)
			}
		}
	}
	if g.RuntimeAdmissionContractB64 == "" {
		return fmt.Errorf("runtime admission contract is required")
	}
	if g.PriorityClassName != strings.TrimSpace(g.PriorityClassName) {
		return fmt.Errorf("priority class name must not contain surrounding whitespace")
	}
	if g.releaseHookUsernamePrefix() == "" {
		return fmt.Errorf("hook service account does not encode the candidate release sequence")
	}
	if _, err := TeardownServiceAccountName(g.HookServiceAccountName, g.ReleaseSequence); err != nil {
		return err
	}
	if _, err := TeardownQuiesceJobName(g.HookServiceAccountName); err != nil {
		return err
	}
	if g.PollEvery <= 0 {
		return fmt.Errorf("rollout guard poll interval must be positive")
	}
	return nil
}

func (g *RolloutGuard) verifyPolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) (int32, int32, error) {
	name := RolloutGuardPolicyName(g.ReleaseSequence)
	if policy == nil || policy.Name != name {
		return 0, 0, fmt.Errorf("fixed rollout guard policy %s is missing", name)
	}
	if err := g.verifyGuardMetadata("ValidatingAdmissionPolicy", policy.ObjectMeta, name); err != nil {
		return 0, 0, err
	}
	state, err := positiveDecimalValue(policy.Annotations[ControllerStateVersionAnnotation])
	if err != nil || state != uint64(g.ControllerStateVersion) {
		return 0, 0, fmt.Errorf("rollout guard policy controller-state version is not compatible with candidate %d", g.ControllerStateVersion)
	}
	admission, err := positiveDecimalValue(policy.Annotations[AdmissionContractVersionAnnotation])
	if err != nil || admission != uint64(g.AdmissionContractVersion) {
		return 0, 0, fmt.Errorf("rollout guard policy admission-contract version is not compatible with candidate %d", g.AdmissionContractVersion)
	}
	expectedAtStoredFloor := g.policy(int32(state), int32(admission))
	if !reflect.DeepEqual(policy.Spec, expectedAtStoredFloor.Spec) {
		return 0, 0, fmt.Errorf("rollout guard policy spec differs from its declared contract")
	}
	return int32(state), int32(admission), nil
}

func (g *RolloutGuard) verifyRuntimePolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) (int32, int32, string, error) {
	name := RuntimeGuardPolicyName(g.ReleaseSequence)
	if policy == nil || policy.Name != name {
		return 0, 0, "", fmt.Errorf("fixed runtime guard policy %s is missing", name)
	}
	if err := g.verifyGuardMetadata("ValidatingAdmissionPolicy", policy.ObjectMeta, name); err != nil {
		return 0, 0, "", err
	}
	state, err := positiveDecimalValue(policy.Annotations[ControllerStateVersionAnnotation])
	if err != nil || state != uint64(g.ControllerStateVersion) {
		return 0, 0, "", fmt.Errorf("runtime guard policy controller-state version is not compatible with candidate %d", g.ControllerStateVersion)
	}
	sequence, err := positiveDecimalValue(policy.Annotations[ReleaseSequenceAnnotation])
	if err != nil || sequence != uint64(g.ReleaseSequence) {
		return 0, 0, "", fmt.Errorf("runtime guard policy release sequence is not compatible with candidate %d", g.ReleaseSequence)
	}
	managerImage := policy.Annotations[ManagerImageAnnotation]
	if managerImage == "" {
		return 0, 0, "", fmt.Errorf("runtime guard policy manager image is empty")
	}
	expectedAtStoredIdentity := g.runtimePolicy(int32(state), int32(sequence), managerImage)
	if !reflect.DeepEqual(policy.Spec, expectedAtStoredIdentity.Spec) {
		return 0, 0, "", fmt.Errorf("runtime guard policy spec differs from its declared contract")
	}
	return int32(state), int32(sequence), managerImage, nil
}

func (g *RolloutGuard) verifyHookIdentityPolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	name := HookIdentityGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	if policy == nil || policy.Name != name {
		return fmt.Errorf("fixed hook identity guard policy %s is missing", name)
	}
	if err := g.verifyGuardMetadata("ValidatingAdmissionPolicy", policy.ObjectMeta, name); err != nil {
		return err
	}
	if policy.Annotations[ManagerImageAnnotation] != g.ManagerImage {
		return fmt.Errorf("hook identity guard policy manager image differs from candidate")
	}
	if !reflect.DeepEqual(policy.Spec, g.hookIdentityPolicy().Spec) {
		return fmt.Errorf("hook identity guard policy spec differs from its declared contract")
	}
	return nil
}

func (g *RolloutGuard) verifyHookIdentityProbePolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	name := HookIdentityProbeGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	if policy == nil || policy.Name != name {
		return fmt.Errorf("fixed hook identity probe guard policy %s is missing", name)
	}
	if err := g.verifyGuardMetadata("ValidatingAdmissionPolicy", policy.ObjectMeta, name); err != nil {
		return err
	}
	if policy.Annotations[ManagerImageAnnotation] != g.ManagerImage {
		return fmt.Errorf("hook identity probe guard policy manager image differs from candidate")
	}
	if !reflect.DeepEqual(policy.Spec, g.hookIdentityProbePolicy().Spec) {
		return fmt.Errorf("hook identity probe guard policy spec differs from its declared contract")
	}
	return nil
}

func (g *RolloutGuard) verifyBinding(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding, name string) error {
	if binding == nil {
		return fmt.Errorf("fixed %s binding is missing", name)
	}
	if err := g.verifyGuardMetadata("ValidatingAdmissionPolicyBinding", binding.ObjectMeta, name); err != nil {
		return err
	}
	if !reflect.DeepEqual(binding.Spec, g.binding(name).Spec) {
		return fmt.Errorf("%s binding spec differs from the immutable candidate contract", name)
	}
	return nil
}

func (g *RolloutGuard) verifyGuardMetadata(kind string, metadata metav1.ObjectMeta, name string) error {
	if metadata.Name != name ||
		metadata.Annotations[rolloutGuardVersionAnnotation] != rolloutGuardVersion ||
		metadata.Annotations[ReleaseNameAnnotation] != g.ReleaseName ||
		metadata.Annotations[ReleaseNamespaceAnnotation] != g.ReleaseNamespace ||
		metadata.Annotations[ReleaseSequenceAnnotation] != strconv.FormatInt(int64(g.ReleaseSequence), 10) ||
		metadata.Labels[managedByLabel] != rolloutGuardManagedBy ||
		metadata.Labels[instanceLabel] != g.ReleaseName ||
		metadata.Labels["app.kubernetes.io/component"] != rolloutGuardComponent {
		return fmt.Errorf("fixed guard %s/%s has foreign or incomplete ownership", kind, metadata.Name)
	}
	return nil
}

func (g *RolloutGuard) waitPoliciesReady(ctx context.Context) error {
	rolloutName := RolloutGuardPolicyName(g.ReleaseSequence)
	runtimeName := RuntimeGuardPolicyName(g.ReleaseSequence)
	runtimePodName := RuntimePodGuardPolicyName(g.ReleaseSequence)
	hookName := HookIdentityGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	hookProbeName := HookIdentityProbeGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	for _, name := range []string{rolloutName, runtimeName, runtimePodName, hookName, hookProbeName} {
		if err := g.waitPolicyReady(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

func (g *RolloutGuard) waitPolicyReady(ctx context.Context, name string) error {
	rolloutName := RolloutGuardPolicyName(g.ReleaseSequence)
	runtimeName := RuntimeGuardPolicyName(g.ReleaseSequence)
	runtimePodName := RuntimePodGuardPolicyName(g.ReleaseSequence)
	hookName := HookIdentityGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	hookProbeName := HookIdentityProbeGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	return wait.PollUntilContextCancel(ctx, g.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		policy, err := g.Policies.Get(pollCtx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("read %s policy status: %w", name, err)
		}
		switch name {
		case rolloutName:
			_, _, err = g.verifyPolicy(policy)
		case runtimeName:
			_, _, _, err = g.verifyRuntimePolicy(policy)
		case runtimePodName:
			err = g.verifyRuntimePodIdentityPolicy(policy)
		case hookName:
			err = g.verifyHookIdentityPolicy(policy)
		case hookProbeName:
			err = g.verifyHookIdentityProbePolicy(policy)
		}
		if err != nil {
			return false, err
		}
		if policy.Status.ObservedGeneration != policy.Generation || policy.Status.TypeChecking == nil {
			return false, nil
		}
		if warnings := policy.Status.TypeChecking.ExpressionWarnings; len(warnings) != 0 {
			return false, fmt.Errorf("%s policy has CEL type-check warnings: %s", name, warnings[0].Warning)
		}
		return true, nil
	})
}

// waitEnforced first proves that an unchanged Deployment is accepted, then
// adds only the target policy's reserved token. This makes the resulting
// single-cause denial attributable to one policy even while retained guards
// overlap during an upgrade.
func (g *RolloutGuard) waitEnforced(ctx context.Context, policyName, denialMessage string) error {
	return wait.PollUntilContextCancel(ctx, g.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		deployment, create, err := g.enforcementProbeDeployment(pollCtx)
		if err != nil {
			if retryableDeploymentProbeRace(err) {
				return false, nil
			}
			return false, err
		}
		if err := g.dryRunDeployment(pollCtx, deployment, create); err != nil {
			if retryableDeploymentProbeRace(err) {
				return false, nil
			}
			return false, fmt.Errorf("prove baseline Deployment is accepted before probing %s: %w", policyName, err)
		}

		probe := deployment.DeepCopy()
		if probe.Annotations == nil {
			probe.Annotations = map[string]string{}
		}
		probe.Annotations[guardEnforcementProbeAnnotation] = policyName
		err = g.dryRunDeployment(pollCtx, probe, create)
		if err == nil {
			return false, nil
		}
		if retryableDeploymentProbeRace(err) {
			return false, nil
		}
		if hasExactValidatingAdmissionPolicyDenial(err, policyName, policyName, denialMessage) {
			return true, nil
		}
		return false, fmt.Errorf("probe %s enforcement: %w", policyName, err)
	})
}

// waitRolloutCreateBoundaryEnforced proves that the candidate hook's
// namespace-wide CREATE grant cannot escape the two fixed Deployment names.
func (g *RolloutGuard) waitRolloutCreateBoundaryEnforced(ctx context.Context) error {
	policyName := RolloutGuardPolicyName(g.ReleaseSequence)
	denialMessage := rolloutGuardNameBoundaryDenialMessage(g.ReleaseSequence)
	return wait.PollUntilContextCancel(ctx, g.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		probe, err := g.rolloutCreateBoundaryProbe(pollCtx)
		if err != nil {
			if retryableDeploymentProbeRace(err) {
				return false, nil
			}
			return false, err
		}
		_, err = g.Deployments.Create(pollCtx, probe, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if err == nil {
			return false, nil
		}
		if retryableDeploymentProbeRace(err) {
			return false, nil
		}
		if hasExactValidatingAdmissionPolicyDenial(err, policyName, policyName, denialMessage) {
			return true, nil
		}
		return false, fmt.Errorf("probe %s arbitrary-name CREATE boundary: %w", policyName, err)
	})
}

func (g *RolloutGuard) enforcementProbeDeployment(ctx context.Context) (*appsv1.Deployment, bool, error) {
	var lastNotFound error
	for _, name := range []string{g.ControllerDeploymentName, g.CertificateDeploymentName} {
		deployment, err := g.Deployments.Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			return deployment, false, nil
		case apierrors.IsNotFound(err):
			lastNotFound = err
		default:
			return nil, false, fmt.Errorf("get baseline Deployment %s for admission enforcement probe: %w", name, err)
		}
	}

	identity, err := g.releaseActivationIdentity(ctx)
	if err != nil {
		return nil, false, err
	}
	if identity.active != 0 {
		return nil, false, fmt.Errorf("cannot safely create a bootstrap enforcement probe while release sequence %d is active and both runtime Deployments are missing: %w", identity.active, lastNotFound)
	}
	return g.bootstrapProbeDeployment(g.ControllerDeploymentName), true, nil
}

func (g *RolloutGuard) rolloutCreateBoundaryProbe(ctx context.Context) (*appsv1.Deployment, error) {
	identity, err := g.releaseActivationIdentity(ctx)
	if err != nil {
		return nil, err
	}
	probe := g.bootstrapProbeDeployment(g.rolloutCreateBoundaryProbeName())
	if identity.active > 0 {
		probe.Annotations = map[string]string{
			ControllerStateVersionAnnotation: strconv.FormatUint(identity.state, 10),
			ReleaseSequenceAnnotation:        strconv.FormatUint(identity.release, 10),
		}
	}
	return probe, nil
}

func (g *RolloutGuard) releaseActivationIdentity(ctx context.Context) (releaseActivationIdentity, error) {
	var identity releaseActivationIdentity
	activation := g.releaseActivationGuard()
	object, err := g.ConfigMaps.Get(ctx, ReleaseActivationName, metav1.GetOptions{})
	if err != nil {
		return identity, fmt.Errorf("get release activation parameter for Deployment enforcement probe: %w", err)
	}
	identity, err = activation.verifyActivationObject(object)
	if err != nil {
		return releaseActivationIdentity{}, fmt.Errorf("verify release activation parameter for Deployment enforcement probe: %w", err)
	}
	return identity, nil
}

func (g *RolloutGuard) dryRunDeployment(ctx context.Context, deployment *appsv1.Deployment, create bool) error {
	if create {
		_, err := g.Deployments.Create(ctx, deployment, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		return err
	}
	_, err := g.Deployments.Update(ctx, deployment, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
	return err
}

func retryableDeploymentProbeRace(err error) bool {
	return apierrors.IsNotFound(err) || apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
}

func (g *RolloutGuard) bootstrapProbeDeployment(name string) *appsv1.Deployment {
	labels := map[string]string{"operator.ptah.dev/rollout-probe": "true"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: g.ReleaseNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(0),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: maps.Clone(labels)},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:    "manager",
					Image:   "invalid.example/ptah-rollout-probe:never",
					Command: []string{"/never-run"},
				}}},
			},
		},
	}
}

func (g *RolloutGuard) rolloutCreateBoundaryProbeName() string {
	digest := hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage+"\nrollout-create-boundary")
	base := fmt.Sprintf("ptah-rollout-scope-v%d-%s", g.ReleaseSequence, digest[:12])
	for _, suffix := range []string{"", "-a", "-b"} {
		name := base + suffix
		if name != g.ControllerDeploymentName && name != g.CertificateDeploymentName {
			return name
		}
	}
	return base + "-c"
}

func (g *RolloutGuard) verifierArgs(verifyControllerState bool) []string {
	args := []string{
		"runtime-verify",
		"--timeout=60s",
		"--release-name=" + g.ReleaseName,
		"--release-namespace=" + g.ReleaseNamespace,
		"--coordination-namespace=" + g.CoordinationNamespace,
		"--leader-election=" + strconv.FormatBool(g.LeaderElection),
		"--leader-election-id=" + g.LeaderElectionID,
		"--webhook-service-name=" + g.WebhookServiceName,
		"--webhook-timeout-seconds=" + strconv.FormatInt(int64(g.WebhookTimeoutSeconds), 10),
		"--webhook-secret-name=" + g.WebhookSecretName,
		"--webhook-port=" + strconv.FormatInt(int64(g.WebhookPort), 10),
		"--certificate-health-port=" + strconv.FormatInt(int64(g.CertificateHealthPort), 10),
		"--hook-service-account-name=" + g.HookServiceAccountName,
		"--controller-service-account-name=" + g.ControllerServiceAccountName,
		"--controller-deployment-name=" + g.ControllerDeploymentName,
		"--controller-replicas=" + strconv.FormatInt(int64(g.ControllerReplicas), 10),
		"--certificate-deployment-name=" + g.CertificateDeploymentName,
		"--release-sequence=" + strconv.FormatInt(int64(g.ReleaseSequence), 10),
		"--manager-image=" + g.ManagerImage,
		"--controller-runtime-args-b64=" + encodeRuntimeArgs(g.ControllerArgs),
		"--certificate-runtime-args-b64=" + encodeRuntimeArgs(g.CertificateArgs),
		"--runtime-deployment-config-expressions-b64=" + encodeRuntimeArgs(g.RuntimeDeploymentConfigExpressions),
		"--runtime-pod-config-expressions-b64=" + encodeRuntimeArgs(g.RuntimePodConfigExpressions),
		"--runtime-admission-contract-b64=" + g.RuntimeAdmissionContractB64,
	}
	if verifyControllerState {
		args = append(args, "--verify-controller-state=true")
	}
	return args
}

func (g *RolloutGuard) verifyDeployment(target deploymentTarget, deployment *appsv1.Deployment) error {
	if deployment == nil || deployment.Name != target.name || deployment.Namespace != g.ReleaseNamespace ||
		deployment.Annotations[helmReleaseNameAnnotation] != g.ReleaseName ||
		deployment.Annotations[helmReleaseNamespaceAnnotation] != g.ReleaseNamespace ||
		deployment.Labels[managedByLabel] != "Helm" ||
		deployment.Labels[instanceLabel] != g.ReleaseName ||
		deployment.Labels["app.kubernetes.io/component"] != target.component {
		return fmt.Errorf("%s Deployment %s/%s has foreign or incomplete Helm ownership", target.component, g.ReleaseNamespace, target.name)
	}
	return nil
}

func (g *RolloutGuard) quiescedDeployment(target deploymentTarget, deployment *appsv1.Deployment) (*appsv1.Deployment, bool, error) {
	if err := g.verifyDeployment(target, deployment); err != nil {
		return nil, false, err
	}
	state, found, err := positiveAnnotation(deployment.Annotations, ControllerStateVersionAnnotation)
	if err != nil {
		return nil, false, fmt.Errorf("%s Deployment controller-state annotation: %w", target.component, err)
	}
	if found && state > uint64(g.ControllerStateVersion) {
		return nil, false, fmt.Errorf("%s Deployment controller-state rollback refused: existing version %d is newer than candidate %d", target.component, state, g.ControllerStateVersion)
	}
	sequence, sequenceFound, err := positiveAnnotation(deployment.Annotations, ReleaseSequenceAnnotation)
	if err != nil {
		return nil, false, fmt.Errorf("%s Deployment release-sequence annotation: %w", target.component, err)
	}
	if sequenceFound && sequence > uint64(g.ReleaseSequence) {
		return nil, false, fmt.Errorf("%s Deployment release rollback refused: existing sequence %d is newer than candidate %d", target.component, sequence, g.ReleaseSequence)
	}
	candidate := deployment.DeepCopy()
	if candidate.Annotations == nil {
		candidate.Annotations = map[string]string{}
	}
	candidate.Annotations[ControllerStateVersionAnnotation] = strconv.FormatInt(int64(g.ControllerStateVersion), 10)
	candidate.Annotations[ReleaseSequenceAnnotation] = strconv.FormatInt(int64(g.ReleaseSequence), 10)
	candidate.Spec.Replicas = int32Ptr(0)
	changed := !found || state != uint64(g.ControllerStateVersion) ||
		!sequenceFound || sequence != uint64(g.ReleaseSequence) ||
		deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0
	return candidate, changed, nil
}

func (g *RolloutGuard) waitDeploymentStopped(ctx context.Context, target deploymentTarget) error {
	return wait.PollUntilContextCancel(ctx, g.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		deployment, err := g.Deployments.Get(pollCtx, target.name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("read quiescing %s Deployment: %w", target.component, err)
		}
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 ||
			deployment.Status.Replicas != 0 || deployment.Status.ReadyReplicas != 0 ||
			deployment.Status.AvailableReplicas != 0 || deployment.Status.UpdatedReplicas != 0 {
			return false, nil
		}
		pods, err := g.Pods.List(pollCtx, metav1.ListOptions{LabelSelector: target.selector.String()})
		if err != nil {
			return false, fmt.Errorf("list %s Deployment Pods: %w", target.component, err)
		}
		return len(pods.Items) == 0, nil
	})
}

func (g *RolloutGuard) releaseActivationParameterShapeExpression() string {
	return fmt.Sprintf(`params != null && (%s)`, g.releaseActivationGuard().activationObjectShapeExpression("params"))
}

func annotationAbsentExpression(object, annotation string) string {
	return fmt.Sprintf(`(!has(%[1]s.metadata.annotations) || !(%[2]q in %[1]s.metadata.annotations))`, object, annotation)
}

func guardEnforcementProbeValidationExpression(policyName string) string {
	return fmt.Sprintf(
		`!(variables.isDeployment && (!has(request.subResource) || request.subResource == "") && request.operation in ["CREATE", "UPDATE"] && request.dryRun == true && variables.isCandidateHook && has(object.metadata.annotations) && %[1]q in object.metadata.annotations && object.metadata.annotations[%[1]q] == %[2]q)`,
		guardEnforcementProbeAnnotation,
		policyName,
	)
}

func guardEnforcementProbePersistenceExpression() string {
	return fmt.Sprintf(
		`request.dryRun == true || !has(object.metadata.annotations) || !(%[1]q in object.metadata.annotations)`,
		guardEnforcementProbeAnnotation,
	)
}

// deploymentStopTransitionExpression permits the candidate hook to stamp and
// stop an active Deployment without changing its executable or ownership. It
// deliberately becomes false as soon as that candidate sequence is active.
func deploymentStopTransitionExpression() string {
	stateAnnotation := strconv.Quote(ControllerStateVersionAnnotation)
	releaseAnnotation := strconv.Quote(ReleaseSequenceAnnotation)
	return fmt.Sprintf(
		`variables.isDeployment && (!has(request.subResource) || request.subResource == "") && request.operation == "UPDATE" && oldObject != null && variables.activationValid && variables.newRelease > variables.activeRelease && variables.newState > 0 && variables.isReleaseHook && has(object.spec.replicas) && object.spec.replicas == 0 && object.metadata.name == oldObject.metadata.name && object.metadata.namespace == oldObject.metadata.namespace && has(object.metadata.uid) == has(oldObject.metadata.uid) && (!has(object.metadata.uid) || object.metadata.uid == oldObject.metadata.uid) && object.metadata.labels == oldObject.metadata.labels && has(object.metadata.ownerReferences) == has(oldObject.metadata.ownerReferences) && (!has(object.metadata.ownerReferences) || object.metadata.ownerReferences == oldObject.metadata.ownerReferences) && has(object.metadata.finalizers) == has(oldObject.metadata.finalizers) && (!has(object.metadata.finalizers) || object.metadata.finalizers == oldObject.metadata.finalizers) && has(object.metadata.generateName) == has(oldObject.metadata.generateName) && (!has(object.metadata.generateName) || object.metadata.generateName == oldObject.metadata.generateName) && has(object.metadata.deletionTimestamp) == has(oldObject.metadata.deletionTimestamp) && (!has(object.metadata.deletionTimestamp) || object.metadata.deletionTimestamp == oldObject.metadata.deletionTimestamp) && has(object.metadata.annotations) && %[1]s in object.metadata.annotations && object.metadata.annotations[%[1]s] == string(variables.newState) && %[2]s in object.metadata.annotations && object.metadata.annotations[%[2]s] == string(variables.newRelease) && object.metadata.annotations.all(key, key in [%[1]s, %[2]s] || (has(oldObject.metadata.annotations) && key in oldObject.metadata.annotations && object.metadata.annotations[key] == oldObject.metadata.annotations[key])) && (!has(oldObject.metadata.annotations) || oldObject.metadata.annotations.all(key, key in [%[1]s, %[2]s] || (key in object.metadata.annotations && oldObject.metadata.annotations[key] == object.metadata.annotations[key]))) && object.spec.template == oldObject.spec.template && object.spec.selector == oldObject.spec.selector && object.spec.strategy == oldObject.spec.strategy && object.spec.minReadySeconds == oldObject.spec.minReadySeconds && (has(object.spec.revisionHistoryLimit) == has(oldObject.spec.revisionHistoryLimit)) && (!has(object.spec.revisionHistoryLimit) || object.spec.revisionHistoryLimit == oldObject.spec.revisionHistoryLimit) && (has(object.spec.paused) == has(oldObject.spec.paused)) && (!has(object.spec.paused) || object.spec.paused == oldObject.spec.paused) && (has(object.spec.progressDeadlineSeconds) == has(oldObject.spec.progressDeadlineSeconds)) && (!has(object.spec.progressDeadlineSeconds) || object.spec.progressDeadlineSeconds == oldObject.spec.progressDeadlineSeconds)`,
		stateAnnotation,
		releaseAnnotation,
	)
}

func rolloutActiveIdentityExpression() string {
	bootstrap := strings.Join([]string{
		`variables.activeRelease == 0`,
		annotationAbsentExpression("object", ControllerStateVersionAnnotation),
		annotationAbsentExpression("object", ReleaseSequenceAnnotation),
		fmt.Sprintf(`(!variables.isAdmission || %s)`, annotationAbsentExpression("object", AdmissionContractVersionAnnotation)),
	}, " && ")
	active := `variables.activeRelease > 0 && variables.newState == variables.activeState && variables.newRelease == variables.activeRelease && (!variables.isAdmission || variables.newAdmission == variables.activeAdmission)`
	return fmt.Sprintf(`variables.activationValid && ((%s) || (%s))`, bootstrap, active)
}

func (g *RolloutGuard) policy(stateVersion, admissionVersion int32) *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := RolloutGuardPolicyName(g.ReleaseSequence)
	metadata := g.guardMetadata(name)
	metadata.Annotations[ControllerStateVersionAnnotation] = strconv.FormatInt(int64(stateVersion), 10)
	metadata.Annotations[AdmissionContractVersionAnnotation] = strconv.FormatInt(int64(admissionVersion), 10)
	denialMessage := rolloutGuardDenialMessage(g.ReleaseSequence)
	probeDenialMessage := rolloutGuardProbeDenialMessage(g.ReleaseSequence)
	admissionIdentity := exactAnnotationExpression(map[string]string{
		ReleaseNameAnnotation:              g.ReleaseName,
		ReleaseNamespaceAnnotation:         g.ReleaseNamespace,
		CoordinationAnnotation:             g.CoordinationNamespace,
		LeaderElectionAnnotation:           strconv.FormatBool(g.LeaderElection),
		LeaderElectionIDAnnotation:         g.LeaderElectionID,
		WebhookServiceAnnotation:           g.WebhookServiceName,
		HookServiceAccountAnnotation:       g.HookServiceAccountName,
		ControllerServiceAccountAnnotation: g.ControllerServiceAccountName,
		ControllerDeploymentAnnotation:     g.ControllerDeploymentName,
		CertificateDeploymentAnnotation:    g.CertificateDeploymentName,
	})
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: metadata,
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			ParamKind:     &admissionregistrationv1.ParamKind{APIVersion: "v1", Kind: "ConfigMap"},
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
					{RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
						Rule:       admissionregistrationv1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope)},
					}},
					{RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
						Rule:       admissionregistrationv1.Rule{APIGroups: []string{"admissionregistration.k8s.io"}, APIVersions: []string{"v1"}, Resources: []string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"}, Scope: scopePtr(admissionregistrationv1.ClusterScope)},
					}},
				},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name: "fixed-operator-resource",
				Expression: fmt.Sprintf(
					`(request.resource.group == "apps" && request.namespace == %q && (request.name in [%q, %q] || request.userInfo.username == %q)) || (request.resource.group == "admissionregistration.k8s.io" && request.name == %q)`,
					g.ReleaseNamespace, g.ControllerDeploymentName, g.CertificateDeploymentName, g.candidateHookUsername(), AdmissionConfigurationName,
				),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "isDeployment", Expression: `request.resource.group == "apps"`},
				{Name: "isAdmission", Expression: `request.resource.group == "admissionregistration.k8s.io"`},
				{Name: "newState", Expression: fmt.Sprintf(`has(object.metadata.annotations) && %q in object.metadata.annotations && object.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(object.metadata.annotations[%q]) : 0`, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation)},
				{Name: "newRelease", Expression: fmt.Sprintf(`has(object.metadata.annotations) && %q in object.metadata.annotations && object.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(object.metadata.annotations[%q]) : 0`, ReleaseSequenceAnnotation, ReleaseSequenceAnnotation, ReleaseSequenceAnnotation)},
				{Name: "activationValid", Expression: g.releaseActivationParameterShapeExpression()},
				{Name: "activeRelease", Expression: fmt.Sprintf(`params != null && has(params.data) && %q in params.data && params.data[%q].matches("^(0|[1-9][0-9]*)$") ? int(params.data[%q]) : -1`, activeReleaseDataKey, activeReleaseDataKey, activeReleaseDataKey)},
				{Name: "activeState", Expression: fmt.Sprintf(`params != null && has(params.metadata.annotations) && %q in params.metadata.annotations && params.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(params.metadata.annotations[%q]) : -1`, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation)},
				{Name: "activeAdmission", Expression: fmt.Sprintf(`params != null && has(params.metadata.annotations) && %q in params.metadata.annotations && params.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(params.metadata.annotations[%q]) : -1`, AdmissionContractVersionAnnotation, AdmissionContractVersionAnnotation, AdmissionContractVersionAnnotation)},
				{Name: "isReleaseHook", Expression: g.releaseHookUsernameExpression()},
				{Name: "isCandidateHook", Expression: fmt.Sprintf(`request.userInfo.username == %q`, g.candidateHookUsername())},
				{Name: "newAdmission", Expression: fmt.Sprintf(`variables.isAdmission && has(object.metadata.annotations) && %q in object.metadata.annotations && object.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(object.metadata.annotations[%q]) : 0`, AdmissionContractVersionAnnotation, AdmissionContractVersionAnnotation, AdmissionContractVersionAnnotation)},
				{Name: "isActiveIdentity", Expression: rolloutActiveIdentityExpression()},
				{Name: "stopTransition", Expression: deploymentStopTransitionExpression()},
			},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `variables.activationValid`, Message: denialMessage},
				{Expression: `!has(request.subResource) || request.subResource != "scale"`, Message: denialMessage},
				{Expression: fmt.Sprintf(`!variables.isDeployment || !variables.isCandidateHook || request.name in [%q, %q]`, g.ControllerDeploymentName, g.CertificateDeploymentName), Message: rolloutGuardNameBoundaryDenialMessage(g.ReleaseSequence)},
				{Expression: `!variables.isDeployment || request.operation != "CREATE" || !variables.isCandidateHook || request.dryRun == true`, Message: denialMessage},
				{Expression: `variables.isActiveIdentity || variables.stopTransition`, Message: denialMessage},
				{Expression: fmt.Sprintf(`variables.newRelease != %d || variables.newState == %d`, g.ReleaseSequence, stateVersion), Message: denialMessage},
				{Expression: fmt.Sprintf(`!variables.isAdmission || variables.newRelease != %d || (variables.newAdmission == %d && %s)`, g.ReleaseSequence, admissionVersion, admissionIdentity), Message: denialMessage},
				{Expression: guardEnforcementProbeValidationExpression(name), Message: probeDenialMessage},
				{Expression: guardEnforcementProbePersistenceExpression(), Message: denialMessage},
			},
		},
	}
}

func (g *RolloutGuard) hookIdentityPolicy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := HookIdentityGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	metadata := g.guardMetadata(name)
	metadata.Annotations[ManagerImageAnnotation] = g.ManagerImage
	preflightJob := g.hookJobName("preflight")
	reconcileJob := g.hookJobName("reconcile")
	quiesceJob := g.hookJobName("teardown-quiesce")
	teardownJob := g.hookJobName("teardown")
	teardownServiceAccount, _ := TeardownServiceAccountName(g.HookServiceAccountName, g.ReleaseSequence)
	identityJob := HookIdentityProbeJobName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	validations := make([]admissionregistrationv1.Validation, 0, len(g.hookPodValidationExpressions(identityJob, preflightJob, reconcileJob, quiesceJob, teardownJob, teardownServiceAccount)))
	for _, expression := range g.hookPodValidationExpressions(identityJob, preflightJob, reconcileJob, quiesceJob, teardownJob, teardownServiceAccount) {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: expression,
			Message:    hookIdentityGuardDenialMessage(g.ReleaseSequence),
		})
	}
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: metadata,
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
					{
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
							Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope)},
						},
					},
					{
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
							Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods/ephemeralcontainers", "pods/resize"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope)},
						},
					},
					{
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Connect},
							Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods/exec", "pods/attach", "pods/portforward", "pods/proxy"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope)},
						},
					},
				},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name: "fixed-hook-identity",
				Expression: fmt.Sprintf(
					`request.namespace == %q && (((!has(request.subResource) || request.subResource == "") && ((has(object.spec.serviceAccountName) && object.spec.serviceAccountName in [%q, %q]) || (request.operation == "UPDATE" && has(oldObject.spec.serviceAccountName) && oldObject.spec.serviceAccountName in [%q, %q]))) || (has(request.subResource) && request.subResource != "" && (%s || %s || %s || %s || %s)))`,
					g.ReleaseNamespace, g.HookServiceAccountName, teardownServiceAccount, g.HookServiceAccountName, teardownServiceAccount,
					generatedPodRequestNameExpression(identityJob), generatedPodRequestNameExpression(preflightJob), generatedPodRequestNameExpression(reconcileJob), generatedPodRequestNameExpression(quiesceJob), generatedPodRequestNameExpression(teardownJob),
				),
			}},
			Validations: validations,
		},
	}
}

func (g *RolloutGuard) hookIdentityProbePolicy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := HookIdentityProbeGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	metadata := g.guardMetadata(name)
	metadata.Annotations[ManagerImageAnnotation] = g.ManagerImage
	probeObject := HookIdentityProbeObjectName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: metadata,
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
						Rule: admissionregistrationv1.Rule{
							APIGroups:   []string{""},
							APIVersions: []string{"v1"},
							Resources:   []string{"configmaps"},
							Scope:       scopePtr(admissionregistrationv1.NamespacedScope),
						},
					},
				}},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name: "fixed-hook-identity-probe",
				Expression: fmt.Sprintf(
					`request.namespace == %q && request.name == %q && request.userInfo.username == %q`,
					g.ReleaseNamespace, probeObject, "system:serviceaccount:"+g.ReleaseNamespace+":"+g.HookServiceAccountName,
				),
			}},
			Validations: []admissionregistrationv1.Validation{{
				Expression: `false`,
				Message:    hookIdentityProbeGuardDenialMessage(g.ReleaseSequence),
			}},
		},
	}
}

func (g *RolloutGuard) hookJobName(mode string) string {
	switch mode {
	case "preflight":
		return g.HookServiceAccountName + "-preflight"
	case "teardown-quiesce":
		name, _ := TeardownQuiesceJobName(g.HookServiceAccountName)
		return name
	case "teardown":
		name, _ := TeardownServiceAccountName(g.HookServiceAccountName, g.ReleaseSequence)
		return name
	default:
		return g.HookServiceAccountName
	}
}

func generatedNamePrefix(generateName string) string {
	maxPrefixLength := kubernetesDNSLabelMaxLength - kubernetesGeneratedSuffixLen
	if len(generateName) > maxPrefixLength {
		return generateName[:maxPrefixLength]
	}
	return generateName
}

func generatedPodRequestNameExpression(jobName string) string {
	prefix := generatedNamePrefix(jobName + "-")
	return fmt.Sprintf(
		`(request.name.startsWith(%[1]q) && request.name.size() == %[2]d && request.name.substring(%[3]d).matches("^[a-z0-9]{%[4]d}$"))`,
		prefix, len(prefix)+kubernetesGeneratedSuffixLen, len(prefix), kubernetesGeneratedSuffixLen,
	)
}

func generatedPodNameValidationExpression(ownerName string) string {
	generateName := "object.metadata.generateName"
	name := "object.metadata.name"
	maxPrefixLength := kubernetesDNSLabelMaxLength - kubernetesGeneratedSuffixLen
	prefix := fmt.Sprintf(`(%[1]s.size() > %[2]d ? %[1]s.substring(0, %[2]d) : %[1]s)`, generateName, maxPrefixLength)
	return fmt.Sprintf(
		`has(%[1]s) && %[1]s == %[3]s + "-" && %[2]s.size() == %[4]s.size() + %[5]d && %[2]s.startsWith(%[4]s) && %[2]s.substring(%[4]s.size()).matches("^[a-z0-9]{%[5]d}$")`,
		generateName, name, ownerName, prefix, kubernetesGeneratedSuffixLen,
	)
}

func (g *RolloutGuard) hookPodValidationExpressions(identityJob, preflightJob, reconcileJob, quiesceJob, teardownJob, teardownServiceAccount string) []string {
	pod := "object.spec"
	container := pod + ".containers[0]"
	volume := pod + ".volumes[0]"
	sources := volume + ".projected.sources"
	jobLabel := `object.metadata.labels["batch.kubernetes.io/job-name"]`
	owner := "object.metadata.ownerReferences[0]"
	return []string{
		`!has(request.subResource) || request.subResource == ""`,
		`request.operation != "CREATE" || request.userInfo.username in ["system:kube-controller-manager", "system:serviceaccount:kube-system:job-controller"]`,
		fmt.Sprintf(`has(%[1]s.serviceAccountName) && %[1]s.serviceAccountName == (%[2]s == %[3]q ? %[4]q : %[5]q)`, pod, jobLabel, teardownJob, teardownServiceAccount, g.HookServiceAccountName),
		fmt.Sprintf(`has(object.metadata.labels) && "batch.kubernetes.io/job-name" in object.metadata.labels && %s in [%q, %q, %q, %q, %q]`, jobLabel, identityJob, preflightJob, reconcileJob, quiesceJob, teardownJob),
		fmt.Sprintf(`has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1 && %s.apiVersion == "batch/v1" && %s.kind == "Job" && %s.name == %s && has(%s.controller) && %s.controller`, owner, owner, owner, jobLabel, owner, owner),
		fmt.Sprintf(`has(%[1]s.uid) && %[1]s.uid != "" && has(%[1]s.blockOwnerDeletion) && %[1]s.blockOwnerDeletion && %[2]s`, owner, generatedPodNameValidationExpression(owner+".name")),
		fmt.Sprintf(`%s.restartPolicy == "Never"`, pod),
		fmt.Sprintf(`request.operation != "CREATE" || !has(%[1]s.nodeName) || %[1]s.nodeName == ""`, pod),
		fmt.Sprintf(`request.operation != "UPDATE" || ((!has(%[1]s.nodeName) && !has(oldObject.spec.nodeName)) || (has(%[1]s.nodeName) && has(oldObject.spec.nodeName) && %[1]s.nodeName == oldObject.spec.nodeName))`, pod),
		fmt.Sprintf(`has(%s.automountServiceAccountToken) && !%s.automountServiceAccountToken`, pod, pod),
		fmt.Sprintf(`!has(%s.hostNetwork) || !%s.hostNetwork`, pod, pod),
		fmt.Sprintf(`!has(%s.hostPID) || !%s.hostPID`, pod, pod),
		fmt.Sprintf(`!has(%s.hostIPC) || !%s.hostIPC`, pod, pod),
		fmt.Sprintf(`!has(%s.shareProcessNamespace) || !%s.shareProcessNamespace`, pod, pod),
		fmt.Sprintf(`has(%s.securityContext) && has(%s.securityContext.runAsNonRoot) && %s.securityContext.runAsNonRoot && has(%s.securityContext.runAsUser) && %s.securityContext.runAsUser == 65532 && has(%s.securityContext.runAsGroup) && %s.securityContext.runAsGroup == 65532 && has(%s.securityContext.seccompProfile) && %s.securityContext.seccompProfile.type == "RuntimeDefault" && (!has(%s.securityContext.sysctls) || %s.securityContext.sysctls.size() == 0)`, pod, pod, pod, pod, pod, pod, pod, pod, pod, pod, pod),
		fmt.Sprintf(`%s.containers.size() == 1 && (!has(%s.initContainers) || %s.initContainers.size() == 0) && (!has(%s.ephemeralContainers) || %s.ephemeralContainers.size() == 0)`, pod, pod, pod, pod, pod),
		fmt.Sprintf(`%s.name == (%s == %q ? "identity-probe" : (%s == %q ? "crd-manager-preflight" : (%s == %q ? "crd-manager-teardown-quiesce" : (%s == %q ? "crd-manager-teardown" : "crd-manager"))))`, container, jobLabel, identityJob, jobLabel, preflightJob, jobLabel, quiesceJob, jobLabel, teardownJob),
		fmt.Sprintf(`%s.image == %q && %s.command == ["/ptah-crd-manager"]`, container, g.ManagerImage, container),
		fmt.Sprintf(`%s != %q || %s`, jobLabel, identityJob, g.hookArgsValidationExpression(container, "identity-probe")),
		fmt.Sprintf(`%s != %q || %s`, jobLabel, preflightJob, g.hookArgsValidationExpression(container, "preflight")),
		fmt.Sprintf(`%s != %q || %s`, jobLabel, reconcileJob, g.hookArgsValidationExpression(container, "reconcile")),
		fmt.Sprintf(`%s != %q || %s`, jobLabel, quiesceJob, g.hookArgsValidationExpression(container, "teardown-quiesce")),
		fmt.Sprintf(`%s != %q || %s`, jobLabel, teardownJob, g.hookArgsValidationExpression(container, "teardown")),
		hookContainerNoExecutionSideChannelsExpression(container),
		fmt.Sprintf(`has(%s.securityContext) && has(%s.securityContext.allowPrivilegeEscalation) && !%s.securityContext.allowPrivilegeEscalation && has(%s.securityContext.readOnlyRootFilesystem) && %s.securityContext.readOnlyRootFilesystem && (!has(%s.securityContext.privileged) || !%s.securityContext.privileged) && !has(%s.securityContext.runAsUser) && !has(%s.securityContext.runAsGroup) && !has(%s.securityContext.procMount) && has(%s.securityContext.capabilities) && (!has(%s.securityContext.capabilities.add) || %s.securityContext.capabilities.add.size() == 0) && has(%s.securityContext.capabilities.drop) && %s.securityContext.capabilities.drop == ["ALL"]`, container, container, container, container, container, container, container, container, container, container, container, container, container, container, container),
		fmt.Sprintf(`has(%s.volumeMounts) && %s.volumeMounts.size() == 1 && %s.volumeMounts[0].name == "api-access" && %s.volumeMounts[0].mountPath == "/var/run/secrets/kubernetes.io/serviceaccount" && has(%s.volumeMounts[0].readOnly) && %s.volumeMounts[0].readOnly && !has(%s.volumeMounts[0].mountPropagation) && !has(%s.volumeMounts[0].subPath) && !has(%s.volumeMounts[0].subPathExpr) && !has(%s.volumeMounts[0].recursiveReadOnly)`, container, container, container, container, container, container, container, container, container, container),
		fmt.Sprintf(`has(%s.volumes) && %s.volumes.size() == 1 && %s.name == "api-access" && has(%s.projected) && has(%s.projected.defaultMode) && %s.projected.defaultMode == 420 && %s.size() == 3`, pod, pod, volume, volume, volume, volume, sources),
		fmt.Sprintf(`%s.exists(s, has(s.serviceAccountToken) && s.serviceAccountToken.path == "token" && has(s.serviceAccountToken.expirationSeconds) && s.serviceAccountToken.expirationSeconds == 3600 && !has(s.serviceAccountToken.audience))`, sources),
		fmt.Sprintf(`%s.exists(s, has(s.configMap) && s.configMap.name == "kube-root-ca.crt" && has(s.configMap.items) && s.configMap.items.size() == 1 && s.configMap.items[0].key == "ca.crt" && s.configMap.items[0].path == "ca.crt" && !has(s.configMap.items[0].mode))`, sources),
		fmt.Sprintf(`%s.exists(s, has(s.downwardAPI) && has(s.downwardAPI.items) && s.downwardAPI.items.size() == 1 && s.downwardAPI.items[0].path == "namespace" && has(s.downwardAPI.items[0].fieldRef) && s.downwardAPI.items[0].fieldRef.apiVersion == "v1" && s.downwardAPI.items[0].fieldRef.fieldPath == "metadata.namespace" && !has(s.downwardAPI.items[0].mode))`, sources),
		fmt.Sprintf(`%s.all(s, has(s.serviceAccountToken) || has(s.configMap) || has(s.downwardAPI))`, sources),
	}
}

func (g *RolloutGuard) hookArgsValidationExpression(container, mode string) string {
	return fmt.Sprintf(`%s.args == %s`, container, celStringList(g.hookArgs(mode)))
}

func hookContainerNoExecutionSideChannelsExpression(container string) string {
	return fmt.Sprintf(
		`!has(%[1]s.lifecycle) && (!has(%[1]s.env) || %[1]s.env.size() == 0) && (!has(%[1]s.envFrom) || %[1]s.envFrom.size() == 0) && (!has(%[1]s.ports) || %[1]s.ports.size() == 0) && !has(%[1]s.livenessProbe) && !has(%[1]s.readinessProbe) && !has(%[1]s.startupProbe) && (!has(%[1]s.volumeDevices) || %[1]s.volumeDevices.size() == 0) && (!has(%[1]s.stdin) || !%[1]s.stdin) && (!has(%[1]s.stdinOnce) || !%[1]s.stdinOnce) && (!has(%[1]s.tty) || !%[1]s.tty) && (!has(%[1]s.workingDir) || %[1]s.workingDir == "") && %[1]s.terminationMessagePath == "/dev/termination-log" && %[1]s.terminationMessagePolicy == "File" && !has(%[1]s.restartPolicy) && (!has(%[1]s.restartPolicyRules) || %[1]s.restartPolicyRules.size() == 0)`,
		container,
	)
}

func (g *RolloutGuard) hookArgs(mode string) []string {
	return []string{
		mode,
		"--timeout=180s",
		"--release-name=" + g.ReleaseName,
		"--release-namespace=" + g.ReleaseNamespace,
		"--coordination-namespace=" + g.CoordinationNamespace,
		"--leader-election=" + strconv.FormatBool(g.LeaderElection),
		"--leader-election-id=" + g.LeaderElectionID,
		"--webhook-service-name=" + g.WebhookServiceName,
		"--webhook-timeout-seconds=" + strconv.FormatInt(int64(g.WebhookTimeoutSeconds), 10),
		"--webhook-secret-name=" + g.WebhookSecretName,
		"--webhook-port=" + strconv.FormatInt(int64(g.WebhookPort), 10),
		"--certificate-health-port=" + strconv.FormatInt(int64(g.CertificateHealthPort), 10),
		"--hook-service-account-name=" + g.HookServiceAccountName,
		"--controller-service-account-name=" + g.ControllerServiceAccountName,
		"--controller-deployment-name=" + g.ControllerDeploymentName,
		"--controller-replicas=" + strconv.FormatInt(int64(g.ControllerReplicas), 10),
		"--certificate-deployment-name=" + g.CertificateDeploymentName,
		"--release-sequence=" + strconv.FormatInt(int64(g.ReleaseSequence), 10),
		"--manager-image=" + g.ManagerImage,
		"--controller-runtime-args-b64=" + encodeRuntimeArgs(g.ControllerArgs),
		"--certificate-runtime-args-b64=" + encodeRuntimeArgs(g.CertificateArgs),
		"--runtime-deployment-config-expressions-b64=" + encodeRuntimeArgs(g.RuntimeDeploymentConfigExpressions),
		"--runtime-pod-config-expressions-b64=" + encodeRuntimeArgs(g.RuntimePodConfigExpressions),
		"--runtime-admission-contract-b64=" + g.RuntimeAdmissionContractB64,
	}
}

func encodeRuntimeArgs(args []string) string {
	encoded, err := json.Marshal(args)
	if err != nil {
		panic(fmt.Sprintf("encode runtime arguments: %v", err))
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

func celStringList(values []string) string {
	encoded, err := json.Marshal(values)
	if err != nil {
		panic(fmt.Sprintf("encode CEL string list: %v", err))
	}
	return string(encoded)
}

func (g *RolloutGuard) releaseHookUsernamePrefix() string {
	marker := "-crd-v" + strconv.FormatInt(int64(g.ReleaseSequence), 10) + "-"
	index := strings.LastIndex(g.HookServiceAccountName, marker)
	if index < 0 {
		return ""
	}
	return "system:serviceaccount:" + g.ReleaseNamespace + ":" + g.HookServiceAccountName[:index] + "-crd-v"
}

func (g *RolloutGuard) candidateHookUsername() string {
	return "system:serviceaccount:" + g.ReleaseNamespace + ":" + g.HookServiceAccountName
}

func (g *RolloutGuard) releaseHookUsernameExpression() string {
	prefix := g.releaseHookUsernamePrefix()
	return fmt.Sprintf(`request.userInfo.username.matches(%q + string(variables.newRelease) + "-[0-9a-f]{12}$")`, "^"+regexp.QuoteMeta(prefix))
}

func (g *RolloutGuard) runtimeDeploymentValidationExpressions(managerImage string) []string {
	pod := "object.spec.template.spec"
	initContainer := pod + ".initContainers[0]"
	container := pod + ".containers[0]"
	isController := fmt.Sprintf(`request.name == %q`, g.ControllerDeploymentName)
	return []string{
		`object.spec.strategy.type == "Recreate" && variables.templateState == string(variables.newState) && variables.templateRelease == string(variables.newRelease)`,
		fmt.Sprintf(`%[1]s.serviceAccountName == variables.runtimeServiceAccount && has(%[1]s.automountServiceAccountToken) && !%[1]s.automountServiceAccountToken && has(%[1]s.enableServiceLinks) && !%[1]s.enableServiceLinks`, pod),
		fmt.Sprintf(`(!has(%[1]s.hostNetwork) || !%[1]s.hostNetwork) && (!has(%[1]s.hostPID) || !%[1]s.hostPID) && (!has(%[1]s.hostIPC) || !%[1]s.hostIPC) && (!has(%[1]s.shareProcessNamespace) || !%[1]s.shareProcessNamespace) && !has(%[1]s.runtimeClassName) && !has(%[1]s.activeDeadlineSeconds)`, pod),
		fmt.Sprintf(`has(%[1]s.securityContext) && has(%[1]s.securityContext.runAsNonRoot) && %[1]s.securityContext.runAsNonRoot && has(%[1]s.securityContext.runAsUser) && %[1]s.securityContext.runAsUser == 65532 && has(%[1]s.securityContext.runAsGroup) && %[1]s.securityContext.runAsGroup == 65532 && has(%[1]s.securityContext.seccompProfile) && %[1]s.securityContext.seccompProfile.type == "RuntimeDefault" && !has(%[1]s.securityContext.seLinuxOptions) && !has(%[1]s.securityContext.windowsOptions) && (!has(%[1]s.securityContext.sysctls) || %[1]s.securityContext.sysctls.size() == 0) && (%[2]s ? (has(%[1]s.securityContext.fsGroup) && %[1]s.securityContext.fsGroup == 65532) : !has(%[1]s.securityContext.fsGroup))`, pod, isController),
		fmt.Sprintf(`%[1]s.containers.size() == 1 && has(%[1]s.initContainers) && %[1]s.initContainers.size() == 1 && (!has(%[1]s.ephemeralContainers) || %[1]s.ephemeralContainers.size() == 0) && (!has(%[1]s.resourceClaims) || %[1]s.resourceClaims.size() == 0)`, pod),
		fmt.Sprintf(`%[1]s.name == "verify-candidate-runtime" && %[1]s.image == %[2]q && %[1]s.command == ["/ptah-crd-manager"] && %[3]s`, initContainer, managerImage, g.verifierArgsValidationExpression()),
		containerNoExecutionSideChannelsExpression(initContainer, false),
		containerSecurityExpression(initContainer),
		resourceKeysExpression(initContainer),
		fmt.Sprintf(`%s`, apiAccessMountExpression(initContainer, 1)),
		fmt.Sprintf(`%[1]s.name == variables.runtimeContainerName && %[1]s.image == %[2]q && %[1]s.command == [variables.runtimeCommand] && %[1]s.args == (%[3]s ? %[4]s : %[5]s)`, container, managerImage, isController, celStringList(g.ControllerArgs), celStringList(g.CertificateArgs)),
		fmt.Sprintf(`(%[1]s ? (!has(%[2]s.env) || %[2]s.env.size() == 0) : (%[2]s.env.size() == 2 && %[2]s.env[0].name == "POD_NAME" && has(%[2]s.env[0].valueFrom) && has(%[2]s.env[0].valueFrom.fieldRef) && %[2]s.env[0].valueFrom.fieldRef.fieldPath == "metadata.name" && %[2]s.env[1].name == "POD_UID" && has(%[2]s.env[1].valueFrom) && has(%[2]s.env[1].valueFrom.fieldRef) && %[2]s.env[1].valueFrom.fieldRef.fieldPath == "metadata.uid")) && (!has(%[2]s.envFrom) || %[2]s.envFrom.size() == 0)`, isController, container),
		containerNoExecutionSideChannelsExpression(container, true),
		containerSecurityExpression(container),
		resourceKeysExpression(container),
		fmt.Sprintf(`%s ? (%s) : (%s)`, isController, controllerMountsExpression(container), apiAccessMountExpression(container, 1)),
		fmt.Sprintf(`%s ? (%s) : (%s)`, isController, controllerPortsAndProbesExpression(container, g.WebhookPort), certificatePortsAndProbesExpression(container, g.CertificateHealthPort)),
		fmt.Sprintf(`%s ? (%s) : (%s)`, isController, controllerVolumesExpression(pod, g.WebhookSecretName), apiAccessVolumesExpression(pod, 1)),
	}
}

func containerNoExecutionSideChannelsExpression(container string, allowEnv bool) string {
	env := fmt.Sprintf(`(!has(%[1]s.env) || %[1]s.env.size() == 0) && (!has(%[1]s.envFrom) || %[1]s.envFrom.size() == 0) && `, container)
	if allowEnv {
		env = ""
	}
	return fmt.Sprintf(`%[2]s!has(%[1]s.lifecycle) && !has(%[1]s.startupProbe) && (!has(%[1]s.volumeDevices) || %[1]s.volumeDevices.size() == 0) && (!has(%[1]s.stdin) || !%[1]s.stdin) && (!has(%[1]s.stdinOnce) || !%[1]s.stdinOnce) && (!has(%[1]s.tty) || !%[1]s.tty) && (!has(%[1]s.workingDir) || %[1]s.workingDir == "") && %[1]s.terminationMessagePath == "/dev/termination-log" && %[1]s.terminationMessagePolicy == "File" && !has(%[1]s.restartPolicy) && (!has(%[1]s.restartPolicyRules) || %[1]s.restartPolicyRules.size() == 0)`, container, env)
}

func containerSecurityExpression(container string) string {
	return fmt.Sprintf(`has(%[1]s.securityContext) && has(%[1]s.securityContext.allowPrivilegeEscalation) && !%[1]s.securityContext.allowPrivilegeEscalation && has(%[1]s.securityContext.readOnlyRootFilesystem) && %[1]s.securityContext.readOnlyRootFilesystem && (!has(%[1]s.securityContext.privileged) || !%[1]s.securityContext.privileged) && !has(%[1]s.securityContext.runAsUser) && !has(%[1]s.securityContext.runAsGroup) && !has(%[1]s.securityContext.runAsNonRoot) && !has(%[1]s.securityContext.procMount) && !has(%[1]s.securityContext.seLinuxOptions) && !has(%[1]s.securityContext.windowsOptions) && !has(%[1]s.securityContext.seccompProfile) && !has(%[1]s.securityContext.appArmorProfile) && has(%[1]s.securityContext.capabilities) && (!has(%[1]s.securityContext.capabilities.add) || %[1]s.securityContext.capabilities.add.size() == 0) && has(%[1]s.securityContext.capabilities.drop) && %[1]s.securityContext.capabilities.drop == ["ALL"]`, container)
}

func resourceKeysExpression(container string) string {
	return fmt.Sprintf(`(!has(dyn(%[1]s.resources).limits) || dyn(%[1]s.resources).limits.all(key, key in ["cpu", "memory", "ephemeral-storage"])) && (!has(dyn(%[1]s.resources).requests) || dyn(%[1]s.resources).requests.all(key, key in ["cpu", "memory", "ephemeral-storage"])) && (!has(dyn(%[1]s.resources).claims) || dyn(%[1]s.resources).claims.size() == 0)`, container)
}

func apiAccessMountExpression(container string, count int) string {
	return fmt.Sprintf(`has(%[1]s.volumeMounts) && %[1]s.volumeMounts.size() == %[2]d && %[1]s.volumeMounts.exists(m, m.name == "api-access" && m.mountPath == "/var/run/secrets/kubernetes.io/serviceaccount" && has(m.readOnly) && m.readOnly && !has(m.mountPropagation) && !has(m.subPath) && !has(m.subPathExpr) && !has(m.recursiveReadOnly))`, container, count)
}

func controllerMountsExpression(container string) string {
	return fmt.Sprintf(`has(%[1]s.volumeMounts) && %[1]s.volumeMounts.size() == 3 && %[1]s.volumeMounts.all(m, m.name in ["api-access", "webhook-cert", "tmp"]) && %[1]s.volumeMounts.exists(m, m.name == "api-access" && m.mountPath == "/var/run/secrets/kubernetes.io/serviceaccount" && has(m.readOnly) && m.readOnly && !has(m.mountPropagation) && !has(m.subPath) && !has(m.subPathExpr) && !has(m.recursiveReadOnly)) && %[1]s.volumeMounts.exists(m, m.name == "webhook-cert" && m.mountPath == "/certs" && has(m.readOnly) && m.readOnly && !has(m.mountPropagation) && !has(m.subPath) && !has(m.subPathExpr) && !has(m.recursiveReadOnly)) && %[1]s.volumeMounts.exists(m, m.name == "tmp" && m.mountPath == "/tmp" && (!has(m.readOnly) || !m.readOnly) && !has(m.mountPropagation) && !has(m.subPath) && !has(m.subPathExpr) && !has(m.recursiveReadOnly))`, container)
}

func controllerPortsAndProbesExpression(container string, webhookPort int32) string {
	return fmt.Sprintf(`%[1]s.ports.size() == 3 && %[1]s.ports.exists(p, p.name == "metrics" && p.containerPort == 8080 && p.protocol == "TCP" && (!has(p.hostPort) || p.hostPort == 0) && (!has(p.hostIP) || p.hostIP == "")) && %[1]s.ports.exists(p, p.name == "health" && p.containerPort == 8081 && p.protocol == "TCP" && (!has(p.hostPort) || p.hostPort == 0) && (!has(p.hostIP) || p.hostIP == "")) && %[1]s.ports.exists(p, p.name == "webhook" && p.containerPort == %d && p.protocol == "TCP" && (!has(p.hostPort) || p.hostPort == 0) && (!has(p.hostIP) || p.hostIP == "")) && has(%[1]s.livenessProbe) && has(%[1]s.livenessProbe.httpGet) && %[1]s.livenessProbe.httpGet.path == "/healthz" && %[1]s.livenessProbe.httpGet.port == "health" && has(%[1]s.readinessProbe) && has(%[1]s.readinessProbe.httpGet) && %[1]s.readinessProbe.httpGet.path == "/readyz" && %[1]s.readinessProbe.httpGet.port == "health"`, container, webhookPort)
}

func certificatePortsAndProbesExpression(container string, healthPort int32) string {
	return fmt.Sprintf(`%[1]s.ports.size() == 1 && %[1]s.ports[0].name == "health" && %[1]s.ports[0].containerPort == %d && %[1]s.ports[0].protocol == "TCP" && (!has(%[1]s.ports[0].hostPort) || %[1]s.ports[0].hostPort == 0) && (!has(%[1]s.ports[0].hostIP) || %[1]s.ports[0].hostIP == "") && has(%[1]s.livenessProbe) && has(%[1]s.livenessProbe.httpGet) && %[1]s.livenessProbe.httpGet.path == "/healthz" && %[1]s.livenessProbe.httpGet.port == "health" && has(%[1]s.readinessProbe) && has(%[1]s.readinessProbe.httpGet) && %[1]s.readinessProbe.httpGet.path == "/readyz" && %[1]s.readinessProbe.httpGet.port == "health"`, container, healthPort)
}

func apiAccessVolumesExpression(pod string, count int) string {
	volume := pod + `.volumes.filter(v, v.name == "api-access")[0]`
	sources := volume + ".projected.sources"
	return fmt.Sprintf(`has(%[1]s.volumes) && %[1]s.volumes.size() == %[2]d && %[3]s.name == "api-access" && has(%[3]s.projected) && has(%[3]s.projected.defaultMode) && %[3]s.projected.defaultMode == 420 && %[4]s.size() == 3 && %[4]s.exists(s, has(s.serviceAccountToken) && s.serviceAccountToken.path == "token" && has(s.serviceAccountToken.expirationSeconds) && s.serviceAccountToken.expirationSeconds == 3600 && !has(s.serviceAccountToken.audience)) && %[4]s.exists(s, has(s.configMap) && s.configMap.name == "kube-root-ca.crt" && has(s.configMap.items) && s.configMap.items.size() == 1 && s.configMap.items[0].key == "ca.crt" && s.configMap.items[0].path == "ca.crt" && !has(s.configMap.items[0].mode)) && %[4]s.exists(s, has(s.downwardAPI) && has(s.downwardAPI.items) && s.downwardAPI.items.size() == 1 && s.downwardAPI.items[0].path == "namespace" && has(s.downwardAPI.items[0].fieldRef) && s.downwardAPI.items[0].fieldRef.apiVersion == "v1" && s.downwardAPI.items[0].fieldRef.fieldPath == "metadata.namespace" && !has(s.downwardAPI.items[0].mode)) && %[4]s.all(s, has(s.serviceAccountToken) || has(s.configMap) || has(s.downwardAPI))`, pod, count, volume, sources)
}

func controllerVolumesExpression(pod, secretName string) string {
	apiAccess := apiAccessVolumesExpression(pod, 3)
	return fmt.Sprintf(`%s && %[2]s.volumes.all(v, v.name in ["api-access", "webhook-cert", "tmp"]) && %[2]s.volumes.exists(v, v.name == "webhook-cert" && has(v.secret) && v.secret.secretName == %q && (!has(v.secret.optional) || !v.secret.optional) && has(v.secret.items) && v.secret.items.size() == 2 && v.secret.items.exists(i, i.key == "tls.crt" && i.path == "tls.crt" && !has(i.mode)) && v.secret.items.exists(i, i.key == "tls.key" && i.path == "tls.key" && !has(i.mode))) && %[2]s.volumes.exists(v, v.name == "tmp" && has(v.emptyDir) && (!has(v.emptyDir.medium) || v.emptyDir.medium == ""))`, apiAccess, pod, secretName)
}

func runtimeActiveDeploymentIdentityExpression() string {
	bootstrap := strings.Join([]string{
		`variables.activeRelease == 0`,
		annotationAbsentExpression("object", ControllerStateVersionAnnotation),
		annotationAbsentExpression("object", ReleaseSequenceAnnotation),
		annotationAbsentExpression("object.spec.template", ControllerStateVersionAnnotation),
		annotationAbsentExpression("object.spec.template", ReleaseSequenceAnnotation),
	}, " && ")
	active := `variables.activeRelease > 0 && variables.newState == variables.activeState && variables.newRelease == variables.activeRelease && variables.templateState == string(variables.activeState) && variables.templateRelease == string(variables.activeRelease) && object.spec.template.spec.containers.size() == 1 && object.spec.template.spec.containers[0].image == variables.activeImage && has(object.spec.template.spec.initContainers) && object.spec.template.spec.initContainers.size() == 1 && object.spec.template.spec.initContainers[0].image == variables.activeImage`
	return fmt.Sprintf(`variables.activationValid && ((%s) || (%s))`, bootstrap, active)
}

func (g *RolloutGuard) runtimePolicy(stateVersion, releaseSequence int32, managerImage string) *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := RuntimeGuardPolicyName(g.ReleaseSequence)
	metadata := g.guardMetadata(name)
	metadata.Annotations[ControllerStateVersionAnnotation] = strconv.FormatInt(int64(stateVersion), 10)
	metadata.Annotations[ReleaseSequenceAnnotation] = strconv.FormatInt(int64(releaseSequence), 10)
	metadata.Annotations[ManagerImageAnnotation] = managerImage
	denialMessage := runtimeGuardDenialMessage(g.ReleaseSequence)
	probeDenialMessage := runtimeGuardProbeDenialMessage(g.ReleaseSequence)
	validations := []admissionregistrationv1.Validation{
		{Expression: `variables.activationValid`, Message: denialMessage},
		{Expression: `variables.isActiveIdentity || variables.stopTransition`, Message: denialMessage},
		{Expression: fmt.Sprintf(`variables.newRelease != %d || variables.newState == %d`, releaseSequence, stateVersion), Message: denialMessage},
	}
	for _, expression := range g.runtimeDeploymentValidationExpressions(managerImage) {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(`variables.stopTransition || variables.newRelease != %d || (%s)`, releaseSequence, expression),
			Message:    denialMessage,
		})
	}
	for _, expression := range g.RuntimeDeploymentConfigExpressions {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(`variables.stopTransition || variables.newRelease != %d || (%s)`, releaseSequence, expression),
			Message:    denialMessage,
		})
	}
	validations = append(validations,
		admissionregistrationv1.Validation{
			Expression: guardEnforcementProbeValidationExpression(name),
			Message:    probeDenialMessage,
		},
		admissionregistrationv1.Validation{
			Expression: guardEnforcementProbePersistenceExpression(),
			Message:    denialMessage,
		},
	)
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: metadata,
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			ParamKind:     &admissionregistrationv1.ParamKind{APIVersion: "v1", Kind: "ConfigMap"},
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
						Rule:       admissionregistrationv1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"deployments"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope)},
					},
				}},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name: "fixed-runtime-deployment",
				Expression: fmt.Sprintf(
					`request.namespace == %q && request.name in [%q, %q]`,
					g.ReleaseNamespace, g.ControllerDeploymentName, g.CertificateDeploymentName,
				),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "isDeployment", Expression: `true`},
				{Name: "newState", Expression: fmt.Sprintf(`has(object.metadata.annotations) && %q in object.metadata.annotations && object.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(object.metadata.annotations[%q]) : 0`, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation)},
				{Name: "newRelease", Expression: fmt.Sprintf(`has(object.metadata.annotations) && %q in object.metadata.annotations && object.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(object.metadata.annotations[%q]) : 0`, ReleaseSequenceAnnotation, ReleaseSequenceAnnotation, ReleaseSequenceAnnotation)},
				{Name: "activationValid", Expression: g.releaseActivationParameterShapeExpression()},
				{Name: "activeRelease", Expression: fmt.Sprintf(`params != null && has(params.data) && %q in params.data && params.data[%q].matches("^(0|[1-9][0-9]*)$") ? int(params.data[%q]) : -1`, activeReleaseDataKey, activeReleaseDataKey, activeReleaseDataKey)},
				{Name: "activeState", Expression: fmt.Sprintf(`params != null && has(params.metadata.annotations) && %q in params.metadata.annotations && params.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(params.metadata.annotations[%q]) : -1`, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation)},
				{Name: "activeImage", Expression: fmt.Sprintf(`params != null && has(params.metadata.annotations) && %q in params.metadata.annotations ? params.metadata.annotations[%q] : ""`, ManagerImageAnnotation, ManagerImageAnnotation)},
				{Name: "isReleaseHook", Expression: g.releaseHookUsernameExpression()},
				{Name: "isCandidateHook", Expression: fmt.Sprintf(`request.userInfo.username == %q`, g.candidateHookUsername())},
				{Name: "runtimeContainerName", Expression: fmt.Sprintf(`request.name == %q ? "manager" : "certificate-rotator"`, g.ControllerDeploymentName)},
				{Name: "runtimeCommand", Expression: fmt.Sprintf(`request.name == %q ? "/manager" : "/ptah-cert-rotator"`, g.ControllerDeploymentName)},
				{Name: "runtimeServiceAccount", Expression: fmt.Sprintf(`request.name == %q ? %q : %q`, g.ControllerDeploymentName, g.ControllerServiceAccountName, g.CertificateDeploymentName)},
				{Name: "templateState", Expression: fmt.Sprintf(`has(object.spec.template.metadata.annotations) && %q in object.spec.template.metadata.annotations ? object.spec.template.metadata.annotations[%q] : ""`, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation)},
				{Name: "templateRelease", Expression: fmt.Sprintf(`has(object.spec.template.metadata.annotations) && %q in object.spec.template.metadata.annotations ? object.spec.template.metadata.annotations[%q] : ""`, ReleaseSequenceAnnotation, ReleaseSequenceAnnotation)},
				{Name: "isActiveIdentity", Expression: runtimeActiveDeploymentIdentityExpression()},
				{Name: "stopTransition", Expression: deploymentStopTransitionExpression()},
			},
			Validations: validations,
		},
	}
}

func (g *RolloutGuard) binding(name string) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.guardMetadata(name),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        name,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
	if name == RolloutGuardPolicyName(g.ReleaseSequence) || name == RuntimeGuardPolicyName(g.ReleaseSequence) {
		action := admissionregistrationv1.DenyAction
		binding.Spec.MatchResources = g.parameterizedBindingMatchResources(name)
		binding.Spec.ParamRef = &admissionregistrationv1.ParamRef{
			Name:                    ReleaseActivationName,
			Namespace:               g.ReleaseNamespace,
			ParameterNotFoundAction: &action,
		}
	}
	return binding
}

func (g *RolloutGuard) parameterizedBindingMatchResources(name string) *admissionregistrationv1.MatchResources {
	exact := admissionregistrationv1.Exact
	match := &admissionregistrationv1.MatchResources{
		MatchPolicy: &exact,
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			corev1.LabelMetadataName: g.ReleaseNamespace,
		}},
		ObjectSelector: &metav1.LabelSelector{},
	}
	deploymentRule := admissionregistrationv1.NamedRuleWithOperations{
		RuleWithOperations: admissionregistrationv1.RuleWithOperations{
			Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
			Rule: admissionregistrationv1.Rule{
				APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"deployments"},
				Scope: scopePtr(admissionregistrationv1.NamespacedScope),
			},
		},
	}
	if name == RuntimeGuardPolicyName(g.ReleaseSequence) {
		deploymentRule.ResourceNames = []string{g.ControllerDeploymentName, g.CertificateDeploymentName}
		match.ResourceRules = []admissionregistrationv1.NamedRuleWithOperations{deploymentRule}
		return match
	}
	deploymentRule.RuleWithOperations.Rule.Resources = []string{"deployments", "deployments/scale"}
	match.ResourceRules = []admissionregistrationv1.NamedRuleWithOperations{
		deploymentRule,
		{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
				Rule: admissionregistrationv1.Rule{
					APIGroups: []string{"admissionregistration.k8s.io"}, APIVersions: []string{"v1"},
					Resources: []string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"},
					Scope:     scopePtr(admissionregistrationv1.ClusterScope),
				},
			},
			ResourceNames: []string{AdmissionConfigurationName},
		},
	}
	return match
}

func (g *RolloutGuard) guardMetadata(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name,
		Annotations: map[string]string{
			rolloutGuardVersionAnnotation: rolloutGuardVersion,
			ReleaseNameAnnotation:         g.ReleaseName,
			ReleaseNamespaceAnnotation:    g.ReleaseNamespace,
			ReleaseSequenceAnnotation:     strconv.FormatInt(int64(g.ReleaseSequence), 10),
		},
		Labels: map[string]string{
			managedByLabel:                rolloutGuardManagedBy,
			instanceLabel:                 g.ReleaseName,
			"app.kubernetes.io/component": rolloutGuardComponent,
		},
	}
}

func positiveAnnotation(annotations map[string]string, key string) (uint64, bool, error) {
	value, found := annotations[key]
	if !found {
		return 0, false, nil
	}
	parsed, err := positiveDecimalValue(value)
	if err != nil {
		return 0, true, err
	}
	return parsed, true, nil
}

func scopePtr(scope admissionregistrationv1.ScopeType) *admissionregistrationv1.ScopeType {
	return &scope
}

func (g *RolloutGuard) verifierArgsValidationExpression() string {
	return fmt.Sprintf(
		`object.spec.template.spec.initContainers[0].args == (request.name == %q ? %s : %s)`,
		g.ControllerDeploymentName,
		celStringList(g.verifierArgs(true)),
		celStringList(g.verifierArgs(false)),
	)
}

func exactAnnotationExpression(values map[string]string) string {
	keys := []string{
		ReleaseNameAnnotation,
		ReleaseNamespaceAnnotation,
		CoordinationAnnotation,
		LeaderElectionAnnotation,
		LeaderElectionIDAnnotation,
		WebhookServiceAnnotation,
		HookServiceAccountAnnotation,
		ControllerServiceAccountAnnotation,
		ControllerDeploymentAnnotation,
		CertificateDeploymentAnnotation,
	}
	parts := []string{"has(object.metadata.annotations)"}
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf(`%q in object.metadata.annotations && object.metadata.annotations[%q] == %q`, key, key, values[key]))
	}
	return "(" + strings.Join(parts, " && ") + ")"
}

func int32Ptr(value int32) *int32 {
	return &value
}
