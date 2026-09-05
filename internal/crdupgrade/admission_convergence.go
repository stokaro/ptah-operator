package crdupgrade

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	admissionConvergenceNamePrefix        = "ptah-operator-admission-convergence-v1-"
	admissionConvergenceMarkerPrefix      = "ptah-admission-convergence-v1-"
	admissionConvergenceComponent         = "admission-convergence"
	admissionConvergenceContractVersion   = "1"
	admissionConvergenceVersionAnnotation = "operator.ptah.dev/admission-convergence-version"
	admissionConvergenceCleanupAnnotation = "operator.ptah.dev/admission-convergence-cleanup-service-account"
	admissionConvergenceExpectedDataKey   = "expected-active-release-sequence"
	admissionConvergenceAttemptDataKey    = "release-attempt"
	admissionConvergenceMarkerHookWeight  = "-165"
	admissionConvergencePolicyHookWeight  = "-38"
	admissionConvergenceBindingHookWeight = "-37"

	admissionConvergenceDenialMessagePrefix   = "Ptah admission convergence sentinel confirmed the exact controller credential fence at "
	admissionConvergenceMutationDenialMessage = "Ptah admission convergence marker rejects persistent updates"
	admissionConvergenceContractDenialMessage = "Ptah admission convergence marker or activation parameter differs from the exact release contract"

	admissionConvergenceParamKindNotSynced = "failed to configure binding: paramKind kind `/v1, Kind=ConfigMap` not yet synced to use for admission"
	admissionConvergenceParamNotFound      = "failed to configure binding: no params found for policy binding with `Deny` parameterNotFoundAction"
)

var admissionConvergenceManagerImagePattern = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)

// AdmissionConvergencePolicyName returns the stable release-owned name of the
// self-contained credential-fence sentinel. Helm recreates this object on each
// attempt, while its exact metadata and spec bind it to one immutable runtime.
// Any future semantic change to the fence must use a new name version so a
// cached older policy and binding cannot satisfy the exact endpoint proof.
func AdmissionConvergencePolicyName(releaseNamespace, releaseName string) string {
	return admissionConvergenceNamePrefix + admissionConvergenceReleaseDigest(releaseNamespace, releaseName)
}

// AdmissionConvergenceMarkerName returns the release- and sequence-owned probe
// ConfigMap name used only for unchanged, server-side dry-run updates.
func AdmissionConvergenceMarkerName(releaseNamespace, releaseName string, releaseSequence int32) string {
	return admissionConvergenceMarkerPrefix + strconv.FormatInt(int64(releaseSequence), 10) + "-" + admissionConvergenceReleaseDigest(releaseNamespace, releaseName)
}

func admissionConvergenceReleaseDigest(releaseNamespace, releaseName string) string {
	digest := sha256.Sum256([]byte(releaseNamespace + "\n" + releaseName))
	return fmt.Sprintf("%x", digest)[:12]
}

// AdmissionConvergenceMarkerClient is the exact API surface required to probe
// one directly addressed API server.
type AdmissionConvergenceMarkerClient interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error)
	Update(context.Context, *corev1.ConfigMap, metav1.UpdateOptions) (*corev1.ConfigMap, error)
}

// AdmissionConvergenceGuard verifies the final admission sentinel and probes
// its self-contained controller credential fence through directly addressed
// API servers. The fence deliberately duplicates the critical controller and
// TokenRequest phase checks instead of relying on publication order between
// independent admission objects.
type AdmissionConvergenceGuard struct {
	Policies                             ValidatingAdmissionPolicyReader
	Bindings                             ValidatingAdmissionPolicyBindingReader
	ConfigMaps                           ConfigMapWriter
	ConfigMapDeleter                     ConfigMapDeleter
	ReleaseName                          string
	ReleaseNamespace                     string
	HookServiceAccountName               string
	CleanupServiceAccountName            string
	ControllerServiceAccountName         string
	ControllerDeploymentName             string
	CertificateServiceAccountName        string
	PreviousControllerServiceAccountName string
	PreviousControllerReleaseSequence    int32
	ControllerStateVersion               int32
	AdmissionContractVersion             int32
	ReleaseSequence                      int32
	ManagerImage                         string
	dependencyRollout                    *RolloutGuard
}

// NewAdmissionConvergenceGuard derives the immutable sentinel contract from a
// rollout guard. CertificateDeploymentName is also the certificate runtime's
// ServiceAccount name in the chart contract.
func NewAdmissionConvergenceGuard(rollout *RolloutGuard) *AdmissionConvergenceGuard {
	if rollout == nil {
		return nil
	}
	dependencyRollout := *rollout
	dependencyRollout.ControllerArgs = slices.Clone(rollout.ControllerArgs)
	dependencyRollout.CertificateArgs = slices.Clone(rollout.CertificateArgs)
	dependencyRollout.RuntimeDeploymentConfigExpressions = slices.Clone(rollout.RuntimeDeploymentConfigExpressions)
	dependencyRollout.RuntimePodConfigExpressions = slices.Clone(rollout.RuntimePodConfigExpressions)
	cleanupServiceAccountName, _ := TeardownServiceAccountName(rollout.HookServiceAccountName, rollout.ReleaseSequence)
	return &AdmissionConvergenceGuard{
		Policies:                             rollout.Policies,
		Bindings:                             rollout.Bindings,
		ConfigMaps:                           rollout.ConfigMaps,
		ConfigMapDeleter:                     rollout.ConfigMapDeleter,
		ReleaseName:                          rollout.ReleaseName,
		ReleaseNamespace:                     rollout.ReleaseNamespace,
		HookServiceAccountName:               rollout.HookServiceAccountName,
		CleanupServiceAccountName:            cleanupServiceAccountName,
		ControllerServiceAccountName:         rollout.ControllerServiceAccountName,
		ControllerDeploymentName:             rollout.ControllerDeploymentName,
		CertificateServiceAccountName:        rollout.CertificateDeploymentName,
		PreviousControllerServiceAccountName: rollout.PreviousControllerServiceAccountName,
		PreviousControllerReleaseSequence:    rollout.PreviousControllerReleaseSequence,
		ControllerStateVersion:               rollout.ControllerStateVersion,
		AdmissionContractVersion:             rollout.AdmissionContractVersion,
		ReleaseSequence:                      rollout.ReleaseSequence,
		ManagerImage:                         rollout.ManagerImage,
		dependencyRollout:                    &dependencyRollout,
	}
}

// MarkerTarget exposes only the current sealed immutable admission marker and its
// exact verifier to the final teardown phase. The returned contract carries no
// ConfigMap client or generic deletion authority; the finalizer must still
// re-read the object and delete its observed UID/resourceVersion explicitly.
func (g *AdmissionConvergenceGuard) MarkerTarget() (TeardownRetirementMarkerTarget, error) {
	if err := g.validate(); err != nil {
		return TeardownRetirementMarkerTarget{}, err
	}
	name := AdmissionConvergenceMarkerName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence)
	return TeardownRetirementMarkerTarget{Name: name, Verify: func(marker *corev1.ConfigMap) error {
		_, err := g.verifySealedMarker(marker)
		return err
	}}, nil
}

// RetirePreviousMarker removes the now-inactive sequence marker only after
// candidate activation. It verifies the complete inert contract and uses UID
// and resourceVersion preconditions, making a lost delete response resumable
// without ever deleting a replacement object.
func (g *AdmissionConvergenceGuard) RetirePreviousMarker(ctx context.Context) error {
	if err := g.validate(); err != nil {
		return err
	}
	if g.PreviousControllerReleaseSequence == 0 {
		return nil
	}
	if g.ConfigMapDeleter == nil {
		return errors.New("admission convergence marker deleter is required")
	}
	sequence := g.PreviousControllerReleaseSequence
	name := AdmissionConvergenceMarkerName(g.ReleaseNamespace, g.ReleaseName, sequence)
	marker, err := g.ConfigMaps.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get previous admission convergence marker: %w", err)
	}
	managerImage := marker.Annotations[ManagerImageAnnotation]
	if !admissionConvergenceManagerImagePattern.MatchString(managerImage) {
		return fmt.Errorf("previous admission convergence ConfigMap/%s has an invalid manager identity", name)
	}
	previous := *g
	previous.ReleaseSequence = sequence
	previous.ManagerImage = managerImage
	previous.HookServiceAccountName, err = g.hookServiceAccountFor(sequence, managerImage)
	if err != nil {
		return fmt.Errorf("derive previous admission convergence hook identity: %w", err)
	}
	previous.CleanupServiceAccountName, err = TeardownServiceAccountName(previous.HookServiceAccountName, sequence)
	if err != nil {
		return fmt.Errorf("derive previous admission convergence cleanup identity: %w", err)
	}
	if _, err := previous.verifySealedMarker(marker); err != nil {
		return fmt.Errorf("verify previous admission convergence marker: %w", err)
	}
	uid := marker.UID
	resourceVersion := marker.ResourceVersion
	err = g.ConfigMapDeleter.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID: &uid, ResourceVersion: &resourceVersion,
	}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete previous admission convergence marker: %w", err)
	}
	return nil
}

// VerifyPreCutover verifies the exact stored sentinel and returns the active
// sequence it fences. Fresh bootstrap, the declared predecessor, and an
// already-activated candidate retry are the only accepted states.
func (g *AdmissionConvergenceGuard) VerifyPreCutover(ctx context.Context) (ReleaseActivationState, error) {
	state, err := g.verify(ctx, false)
	if err != nil {
		return ReleaseActivationState{}, err
	}
	if !slices.Contains(g.allowedStates(), state) {
		return ReleaseActivationState{}, fmt.Errorf("admission convergence activation state %#v is outside the exact pre-cutover contract", state)
	}
	return state, nil
}

// VerifyRuntime requires the stored sentinel to fence the candidate sequence.
func (g *AdmissionConvergenceGuard) VerifyRuntime(ctx context.Context) error {
	state, err := g.verify(ctx, true)
	if err != nil {
		return err
	}
	want := ReleaseActivationState{
		ActiveReleaseSequence: g.ReleaseSequence, ControllerCredentialPhase: ControllerCredentialsActive,
	}
	if state != want {
		return fmt.Errorf("admission convergence activation state is %#v, want candidate runtime %#v", state, want)
	}
	return nil
}

// VerifyState requires the exact stored sentinel and activation tuple expected
// by one direct-endpoint convergence barrier.
func (g *AdmissionConvergenceGuard) VerifyState(ctx context.Context, expected ReleaseActivationState) error {
	state, err := g.verify(ctx, false)
	if err != nil {
		return err
	}
	if state != expected {
		return fmt.Errorf("admission convergence activation state changed from %#v to %#v", expected, state)
	}
	return nil
}

// VerifySealedState requires the exact sealed marker and activation tuple
// expected by the post-seal direct-endpoint proof.
func (g *AdmissionConvergenceGuard) VerifySealedState(ctx context.Context, expected ReleaseActivationState) error {
	state, err := g.verify(ctx, true)
	if err != nil {
		return err
	}
	if state != expected {
		return fmt.Errorf("sealed admission convergence activation state changed from %#v to %#v", expected, state)
	}
	return nil
}

func (g *AdmissionConvergenceGuard) verify(ctx context.Context, requireSealed bool) (ReleaseActivationState, error) {
	if err := g.validate(); err != nil {
		return ReleaseActivationState{}, err
	}
	activation := &ReleaseActivationGuard{
		Policies:                 g.Policies,
		Bindings:                 g.Bindings,
		ConfigMaps:               g.ConfigMaps,
		ReleaseName:              g.ReleaseName,
		ReleaseNamespace:         g.ReleaseNamespace,
		HookServiceAccountName:   g.HookServiceAccountName,
		ControllerStateVersion:   g.ControllerStateVersion,
		AdmissionContractVersion: g.AdmissionContractVersion,
		ReleaseSequence:          g.ReleaseSequence,
		ManagerImage:             g.ManagerImage,
		PollEvery:                1,
	}
	parameter, err := g.ConfigMaps.Get(ctx, ReleaseActivationName, metav1.GetOptions{})
	if err != nil {
		return ReleaseActivationState{}, fmt.Errorf("get release activation parameter for admission convergence: %w", err)
	}
	identity, err := activation.verifyActivationObject(parameter)
	if err != nil {
		return ReleaseActivationState{}, fmt.Errorf("verify release activation parameter for admission convergence: %w", err)
	}
	if err := activation.verifyCandidateCompatibility(identity); err != nil {
		return ReleaseActivationState{}, err
	}
	state, err := releaseActivationState(identity)
	if err != nil {
		return ReleaseActivationState{}, err
	}

	name := AdmissionConvergencePolicyName(g.ReleaseNamespace, g.ReleaseName)
	policy, err := g.Policies.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ReleaseActivationState{}, fmt.Errorf("get admission convergence policy: %w", err)
	}
	if err := g.verifyPolicy(policy); err != nil {
		return ReleaseActivationState{}, err
	}
	binding, err := g.Bindings.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ReleaseActivationState{}, fmt.Errorf("get admission convergence binding: %w", err)
	}
	if err := g.verifyBinding(binding); err != nil {
		return ReleaseActivationState{}, err
	}
	if err := g.verifyDependencies(ctx); err != nil {
		return ReleaseActivationState{}, err
	}
	marker, err := g.ConfigMaps.Get(ctx, AdmissionConvergenceMarkerName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence), metav1.GetOptions{})
	if err != nil {
		return ReleaseActivationState{}, fmt.Errorf("get admission convergence marker: %w", err)
	}
	if requireSealed {
		if _, err := g.verifySealedMarker(marker); err != nil {
			return ReleaseActivationState{}, err
		}
	} else if err := g.verifyMarker(marker); err != nil {
		return ReleaseActivationState{}, err
	}
	return state, nil
}

func (g *AdmissionConvergenceGuard) verifyDependencies(ctx context.Context) error {
	rollout, err := g.rolloutForDependencies()
	if err != nil {
		return err
	}
	blueprints, err := predecessorRetirementPairBlueprints(rollout)
	if err != nil {
		return fmt.Errorf("build admission convergence candidate dependency inventory: %w", err)
	}
	for _, blueprint := range blueprints {
		policy, getErr := g.Policies.Get(ctx, blueprint.name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get admission convergence dependency policy %s: %w", blueprint.name, getErr)
		}
		if verifyErr := blueprint.verifyPolicy(policy); verifyErr != nil {
			return fmt.Errorf("verify admission convergence dependency policy %s: %w", blueprint.name, verifyErr)
		}
		if metadataErr := verifyAdmissionConvergenceDependencyMetadata(policy.TypeMeta, policy.ObjectMeta, blueprint.policy); metadataErr != nil {
			return metadataErr
		}
		binding, getErr := g.Bindings.Get(ctx, blueprint.name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get admission convergence dependency binding %s: %w", blueprint.name, getErr)
		}
		if verifyErr := blueprint.verifyBinding(binding); verifyErr != nil {
			return fmt.Errorf("verify admission convergence dependency binding %s: %w", blueprint.name, verifyErr)
		}
		if metadataErr := verifyAdmissionConvergenceDependencyMetadata(binding.TypeMeta, binding.ObjectMeta, blueprint.binding); metadataErr != nil {
			return metadataErr
		}
	}
	return nil
}

func (g *AdmissionConvergenceGuard) rolloutForDependencies() (*RolloutGuard, error) {
	if g.dependencyRollout == nil {
		return nil, errors.New("admission convergence dependency rollout contract is required")
	}
	rollout := *g.dependencyRollout
	rollout.Policies = g.Policies
	rollout.Bindings = g.Bindings
	rollout.ReleaseName = g.ReleaseName
	rollout.ReleaseNamespace = g.ReleaseNamespace
	rollout.HookServiceAccountName = g.HookServiceAccountName
	rollout.ControllerServiceAccountName = g.ControllerServiceAccountName
	rollout.PreviousControllerServiceAccountName = g.PreviousControllerServiceAccountName
	rollout.PreviousControllerReleaseSequence = g.PreviousControllerReleaseSequence
	rollout.ControllerDeploymentName = g.ControllerDeploymentName
	rollout.CertificateDeploymentName = g.CertificateServiceAccountName
	rollout.ControllerStateVersion = g.ControllerStateVersion
	rollout.AdmissionContractVersion = g.AdmissionContractVersion
	rollout.ReleaseSequence = g.ReleaseSequence
	rollout.ManagerImage = g.ManagerImage
	return &rollout, nil
}

type admissionConvergenceDependencyObject interface {
	metav1.Object
	runtime.Object
}

func verifyAdmissionConvergenceDependencyMetadata(
	typeMeta metav1.TypeMeta,
	metadata metav1.ObjectMeta,
	expected admissionConvergenceDependencyObject,
) error {
	if expected == nil {
		return errors.New("admission convergence expected dependency is nil")
	}
	expectedGVK := expected.GetObjectKind().GroupVersionKind()
	if typeMeta.APIVersion != expectedGVK.GroupVersion().String() || typeMeta.Kind != expectedGVK.Kind ||
		metadata.Name != expected.GetName() || metadata.Namespace != "" || metadata.GenerateName != "" ||
		len(metadata.OwnerReferences) != 0 || len(metadata.Finalizers) != 0 ||
		metadata.DeletionTimestamp != nil || metadata.DeletionGracePeriodSeconds != nil ||
		!reflect.DeepEqual(metadata.Annotations, expected.GetAnnotations()) ||
		!reflect.DeepEqual(metadata.Labels, expected.GetLabels()) {
		return fmt.Errorf("admission convergence dependency %s/%s has foreign or incomplete ownership", typeMeta.Kind, metadata.Name)
	}
	// UID and resourceVersion are deliberately not pinned. A byte-identical
	// replacement has the same enforcement semantics; a publication gap or a
	// stale cache is still exposed by that endpoint's direct policy probe.
	return nil
}

// Probe performs content-versioned, unchanged dry-run marker UPDATEs against
// one API server. Every bundle member must return its exact single-cause
// denial. Transient transport/server errors, the two frozen parameter-cache
// transitions, and an admitted request are inconclusive and may be retried by
// the caller; stored-object drift fails immediately.
func (g *AdmissionConvergenceGuard) Probe(ctx context.Context, client AdmissionConvergenceMarkerClient, expected ReleaseActivationState) (bool, error) {
	return g.probe(ctx, client, expected, false)
}

// ProbeSealed performs the same complete denial bundle as Probe but requires
// the directly addressed API server to return the exact sealed marker first.
// It is used for the mandatory post-seal sweep before candidate activation.
func (g *AdmissionConvergenceGuard) ProbeSealed(ctx context.Context, client AdmissionConvergenceMarkerClient, expected ReleaseActivationState) (bool, error) {
	return g.probe(ctx, client, expected, true)
}

func (g *AdmissionConvergenceGuard) probe(ctx context.Context, client AdmissionConvergenceMarkerClient, expected ReleaseActivationState, requireSealed bool) (bool, error) {
	if ctx == nil {
		return false, errors.New("admission convergence probe context is nil")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if client == nil {
		return false, errors.New("admission convergence marker client is nil")
	}
	name := AdmissionConvergenceMarkerName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence)
	marker, err := client.Get(ctx, name, metav1.GetOptions{})
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	if err != nil {
		if retryableAdmissionConvergenceError(err) {
			return false, nil
		}
		return false, fmt.Errorf("get direct admission convergence marker: %w", err)
	}
	if !slices.Contains(g.allowedStates(), expected) {
		return false, fmt.Errorf("admission convergence expected state %#v is outside the exact contract", expected)
	}
	if requireSealed {
		if _, err := g.verifySealedMarker(marker); err != nil {
			return false, err
		}
	} else if err := g.verifyMarker(marker); err != nil {
		return false, err
	}
	probes := append([]admissionConvergenceDependencyProbe{g.sentinelProbe(expected)}, g.dependencyProbes()...)
	for _, probe := range probes {
		_, err = client.Update(ctx, marker.DeepCopy(), metav1.UpdateOptions{
			DryRun:       []string{metav1.DryRunAll},
			FieldManager: probe.FieldManager,
		})
		if err == nil {
			return false, nil
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return false, contextErr
		}
		if hasExactValidatingAdmissionPolicyDenial(err, probe.PolicyName, probe.PolicyName, probe.Message) {
			continue
		}
		if g.hasOnlyKnownTransientConfigurationCauses(err, probe) {
			return false, nil
		}
		if g.hasExactSentinelDenial(err) || retryableAdmissionConvergenceError(err) {
			return false, nil
		}
		return false, fmt.Errorf("direct admission convergence probe for policy %s returned an unexpected response: %w", probe.PolicyName, err)
	}
	return true, nil
}

// hasOnlyKnownTransientConfigurationCauses recognizes the two exact
// ValidatingAdmissionPolicy parameter-cache transitions emitted throughout the
// supported Kubernetes window. Every cause must belong to the frozen sentinel
// or dependency inventory. A matching target denial mixed with one of these
// causes is inconclusive rather than proof because the server has not evaluated
// the complete policy set from one coherent parameter snapshot.
func (g *AdmissionConvergenceGuard) hasOnlyKnownTransientConfigurationCauses(
	err error,
	target admissionConvergenceDependencyProbe,
) bool {
	var statusError apierrors.APIStatus
	if !errors.As(err, &statusError) {
		return false
	}
	status := statusError.Status()
	if status.Status != metav1.StatusFailure || status.Reason != metav1.StatusReasonInvalid || status.Code != 422 ||
		status.Details == nil || len(status.Details.Causes) == 0 {
		return false
	}

	knownTransient := make(map[string]struct{})
	probes := append([]admissionConvergenceDependencyProbe{g.sentinelProbe(ReleaseActivationState{})}, g.dependencyProbes()...)
	for _, probe := range probes {
		for _, reason := range []string{admissionConvergenceParamKindNotSynced, admissionConvergenceParamNotFound} {
			knownTransient[validatingAdmissionPolicyDenialCauseMessage(probe.PolicyName, probe.PolicyName, reason)] = struct{}{}
		}
	}
	targetDenial := validatingAdmissionPolicyDenialCauseMessage(target.PolicyName, target.PolicyName, target.Message)
	hasTransient := false
	for _, cause := range status.Details.Causes {
		if cause.Type != "" || cause.Field != "" {
			return false
		}
		if cause.Message == targetDenial {
			continue
		}
		if _, known := knownTransient[cause.Message]; !known {
			return false
		}
		hasTransient = true
	}
	return hasTransient
}

// ProbeAbsent proves that one directly addressed API server no longer has an
// active sentinel binding after teardown removed it. The immutable marker and
// exact activation tuple are re-read through the same endpoint before every
// dry-run so an admitted request cannot be attributed to object drift.
func (g *AdmissionConvergenceGuard) ProbeAbsent(ctx context.Context, client AdmissionConvergenceMarkerClient, expected ReleaseActivationState) (bool, error) {
	if ctx == nil {
		return false, errors.New("admission convergence absence probe context is nil")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if client == nil {
		return false, errors.New("admission convergence marker client is nil")
	}
	if !slices.Contains(g.allowedStates(), expected) {
		return false, fmt.Errorf("admission convergence expected teardown state %#v is outside the exact contract", expected)
	}
	if err := g.verifyDirectActivationState(ctx, client, expected); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return false, contextErr
		}
		if retryableAdmissionConvergenceError(err) {
			return false, nil
		}
		return false, err
	}
	name := AdmissionConvergenceMarkerName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence)
	marker, err := client.Get(ctx, name, metav1.GetOptions{})
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	if err != nil {
		if retryableAdmissionConvergenceError(err) {
			return false, nil
		}
		return false, fmt.Errorf("get direct admission convergence marker for teardown: %w", err)
	}
	if _, err := g.verifySealedMarker(marker); err != nil {
		return false, err
	}
	_, err = client.Update(ctx, marker.DeepCopy(), metav1.UpdateOptions{
		DryRun:       []string{metav1.DryRunAll},
		FieldManager: g.sentinelProbe(expected).FieldManager,
	})
	if err == nil {
		return true, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	if g.hasOnlyKnownTransientConfigurationCauses(err, g.sentinelProbe(expected)) {
		return false, nil
	}
	if g.hasExactSentinelDenial(err) || retryableAdmissionConvergenceError(err) {
		return false, nil
	}
	return false, fmt.Errorf("direct admission convergence teardown probe returned an unexpected response: %w", err)
}

func (g *AdmissionConvergenceGuard) verifyDirectActivationState(ctx context.Context, client AdmissionConvergenceMarkerClient, expected ReleaseActivationState) error {
	parameter, err := client.Get(ctx, ReleaseActivationName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get direct release activation parameter for admission convergence: %w", err)
	}
	activation := &ReleaseActivationGuard{
		ReleaseName: g.ReleaseName, ReleaseNamespace: g.ReleaseNamespace,
		HookServiceAccountName: g.HookServiceAccountName,
		ControllerStateVersion: g.ControllerStateVersion, AdmissionContractVersion: g.AdmissionContractVersion,
		ReleaseSequence: g.ReleaseSequence, ManagerImage: g.ManagerImage,
	}
	identity, err := activation.verifyActivationObject(parameter)
	if err != nil {
		return fmt.Errorf("verify direct release activation parameter for admission convergence: %w", err)
	}
	if err := activation.verifyCandidateCompatibility(identity); err != nil {
		return err
	}
	state, err := releaseActivationState(identity)
	if err != nil {
		return err
	}
	if state != expected {
		return fmt.Errorf("direct release activation state changed from %#v to %#v", expected, state)
	}
	return nil
}

func (g *AdmissionConvergenceGuard) hasExactSentinelDenial(err error) bool {
	name := AdmissionConvergencePolicyName(g.ReleaseNamespace, g.ReleaseName)
	for _, message := range []string{admissionConvergenceMutationDenialMessage, admissionConvergenceContractDenialMessage} {
		if hasExactValidatingAdmissionPolicyDenial(err, name, name, message) {
			return true
		}
	}
	for _, state := range g.allowedStates() {
		if hasExactValidatingAdmissionPolicyDenial(err, name, name, admissionConvergenceDenialMessage(state)) {
			return true
		}
	}
	return false
}

func retryableAdmissionConvergenceError(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) {
		return true
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Code >= 500 {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func (g *AdmissionConvergenceGuard) validate() error {
	if g == nil || g.Policies == nil || g.Bindings == nil || g.ConfigMaps == nil {
		return errors.New("admission convergence clients and identity are required")
	}
	for description, value := range map[string]string{
		"release name":                    g.ReleaseName,
		"release namespace":               g.ReleaseNamespace,
		"hook ServiceAccount name":        g.HookServiceAccountName,
		"cleanup ServiceAccount name":     g.CleanupServiceAccountName,
		"controller ServiceAccount name":  g.ControllerServiceAccountName,
		"controller Deployment name":      g.ControllerDeploymentName,
		"certificate ServiceAccount name": g.CertificateServiceAccountName,
		"manager image":                   g.ManagerImage,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("admission convergence %s is empty or padded", description)
		}
	}
	if g.ReleaseSequence < 1 || g.ControllerStateVersion < 1 || g.AdmissionContractVersion < 1 {
		return errors.New("admission convergence versions must be positive")
	}
	if g.PreviousControllerReleaseSequence < 0 || g.PreviousControllerReleaseSequence >= g.ReleaseSequence {
		return errors.New("admission convergence predecessor sequence must be non-negative and lower than the candidate")
	}
	if g.PreviousControllerServiceAccountName != strings.TrimSpace(g.PreviousControllerServiceAccountName) {
		return errors.New("admission convergence predecessor ServiceAccount name is padded")
	}
	wantCleanup, err := TeardownServiceAccountName(g.HookServiceAccountName, g.ReleaseSequence)
	if err != nil {
		return fmt.Errorf("derive admission convergence cleanup ServiceAccount: %w", err)
	}
	if g.CleanupServiceAccountName != wantCleanup {
		return errors.New("admission convergence cleanup ServiceAccount does not match the candidate release identity")
	}
	activation := &ReleaseActivationGuard{
		ReleaseName: g.ReleaseName, ReleaseNamespace: g.ReleaseNamespace,
		HookServiceAccountName: g.HookServiceAccountName, ReleaseSequence: g.ReleaseSequence,
		ManagerImage: g.ManagerImage,
	}
	if _, err := activation.hookUsernamePattern(); err != nil {
		return fmt.Errorf("validate admission convergence hook identity: %w", err)
	}
	serviceAccounts := []string{
		g.HookServiceAccountName,
		g.CleanupServiceAccountName,
		g.ControllerServiceAccountName,
		g.CertificateServiceAccountName,
	}
	if g.PreviousControllerServiceAccountName != "" {
		serviceAccounts = append(serviceAccounts, g.PreviousControllerServiceAccountName)
	}
	slices.Sort(serviceAccounts)
	if len(slices.Compact(serviceAccounts)) != len(serviceAccounts) {
		return errors.New("admission convergence ServiceAccount identities must be distinct")
	}
	return nil
}

func (g *AdmissionConvergenceGuard) hookServiceAccountFor(sequence int32, managerImage string) (string, error) {
	currentDigest := hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	currentSuffix := fmt.Sprintf("-crd-v%d-%s", g.ReleaseSequence, currentDigest[:12])
	if !strings.HasSuffix(g.HookServiceAccountName, currentSuffix) {
		return "", errors.New("current hook ServiceAccount does not encode the candidate identity")
	}
	base := strings.TrimSuffix(g.HookServiceAccountName, currentSuffix)
	if base == "" {
		return "", errors.New("current hook ServiceAccount has no stable base")
	}
	digest := hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, sequence, managerImage)
	return fmt.Sprintf("%s-crd-v%d-%s", base, sequence, digest[:12]), nil
}

func (g *AdmissionConvergenceGuard) policy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := AdmissionConvergencePolicyName(g.ReleaseNamespace, g.ReleaseName)
	proof := g.proofExpression()
	markerRequest := g.markerRequestExpression()
	anyConvergenceProbeRequest := g.anyConvergenceProbeRequestExpression()
	hookBase := strings.TrimSuffix(
		g.HookServiceAccountName,
		fmt.Sprintf(
			"-crd-v%d-%s",
			g.ReleaseSequence,
			hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)[:12],
		),
	)
	hookServiceAccountPattern := "^" + regexp.QuoteMeta(hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	hookUsernamePattern := "^" + regexp.QuoteMeta("system:serviceaccount:"+g.ReleaseNamespace+":"+hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	hookPodPattern := "^" + regexp.QuoteMeta(hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}-`
	teardownServiceAccountPattern := "^" + regexp.QuoteMeta(hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	teardownUsernamePattern := "^" + regexp.QuoteMeta("system:serviceaccount:"+g.ReleaseNamespace+":"+hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	teardownPodPattern := "^" + regexp.QuoteMeta(hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}-`
	quiescePodPattern := "^" + regexp.QuoteMeta(hookBase+"-quiesce-v") + `[1-9][0-9]*-[0-9a-f]{12}-`
	hookIdentityPodPattern := `^ptah-hook-identity-v[1-9][0-9]*-[0-9a-f]{12}-`
	controllerCaller := controllerPrincipalMatchExpression(
		g.ReleaseNamespace,
		g.ControllerServiceAccountName,
		g.PreviousControllerServiceAccountName,
	)
	controllerNames := []string{g.ControllerServiceAccountName}
	if g.PreviousControllerServiceAccountName != "" {
		controllerNames = append(controllerNames, g.PreviousControllerServiceAccountName)
	}
	quotedControllerNames := make([]string, len(controllerNames))
	for index, serviceAccountName := range controllerNames {
		quotedControllerNames[index] = strconv.Quote(serviceAccountName)
	}
	controllerNameMatch := `request.name in [` + strings.Join(quotedControllerNames, ", ") + `]`
	isTokenRequest := `request.operation == "CREATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "serviceaccounts" && has(request.subResource) && request.subResource == "token"`
	controllerTokenRequest := fmt.Sprintf(`variables.isTokenRequest && request.namespace == %q && (%s)`, g.ReleaseNamespace, controllerNameMatch)
	certificateUsername := g.serviceAccountUsername(g.CertificateServiceAccountName)
	hookCaller := fmt.Sprintf(`request.userInfo.username.matches(%q) || request.userInfo.username.matches(%q)`, hookUsernamePattern, teardownUsernamePattern)
	certificateCaller := fmt.Sprintf(`request.userInfo.username == %q`, certificateUsername)
	protectedCaller := `variables.isHookCaller || variables.isControllerCaller || variables.isCertificateCaller`
	protectedTokenRequest := fmt.Sprintf(
		`variables.isTokenRequest && request.namespace == %q && ((%s) || request.name == %q || request.name.matches(%q) || request.name.matches(%q))`,
		g.ReleaseNamespace,
		controllerNameMatch,
		g.CertificateServiceAccountName,
		hookServiceAccountPattern,
		teardownServiceAccountPattern,
	)
	protectedOriginMatch := fmt.Sprintf(
		`(%s) || request.userInfo.username == %q || request.userInfo.username.matches(%q) || request.userInfo.username.matches(%q) || (request.operation == "CREATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "serviceaccounts" && has(request.subResource) && request.subResource == "token" && request.namespace == %q && ((%s) || request.name == %q || request.name.matches(%q) || request.name.matches(%q)))`,
		controllerCaller,
		certificateUsername,
		hookUsernamePattern,
		teardownUsernamePattern,
		g.ReleaseNamespace,
		controllerNameMatch,
		g.CertificateServiceAccountName,
		hookServiceAccountPattern,
		teardownServiceAccountPattern,
	)
	callerPodName := fmt.Sprintf(
		`has(request.userInfo.extra) && %q in request.userInfo.extra && request.userInfo.extra[%q].size() == 1 ? request.userInfo.extra[%q][0] : ""`,
		serviceAccountPodNameExtra,
		serviceAccountPodNameExtra,
		serviceAccountPodNameExtra,
	)
	callerPodUID := fmt.Sprintf(
		`has(request.userInfo.extra) && %q in request.userInfo.extra && request.userInfo.extra[%q].size() == 1 ? request.userInfo.extra[%q][0] : ""`,
		serviceAccountPodUIDExtra,
		serviceAccountPodUIDExtra,
		serviceAccountPodUIDExtra,
	)
	callerOrigin := fmt.Sprintf(
		`variables.isAnyConvergenceProbe || !variables.isProtectedCaller || (variables.callerPodName != "" && variables.callerPodUID != "" && ((variables.isHookCaller && (variables.callerPodName.matches(%q) || variables.callerPodName.matches(%q) || variables.callerPodName.matches(%q) || variables.callerPodName.matches(%q))) || (variables.isControllerCaller && %s) || (variables.isCertificateCaller && %s)))`,
		hookPodPattern,
		hookIdentityPodPattern,
		teardownPodPattern,
		quiescePodPattern,
		runtimePodRequestNameExpression("variables.callerPodName", g.ControllerDeploymentName),
		runtimePodRequestNameExpression("variables.callerPodName", g.CertificateServiceAccountName),
	)
	tokenOrigin := fmt.Sprintf(
		`variables.isAnyConvergenceProbe || !variables.isProtectedTokenRequest || (request.userInfo.username.matches("^system:node:.+$") && request.userInfo.groups.filter(group, group == "system:nodes").size() == 1 && has(object.spec.boundObjectRef) && has(object.spec.boundObjectRef.apiVersion) && object.spec.boundObjectRef.apiVersion == "v1" && has(object.spec.boundObjectRef.kind) && object.spec.boundObjectRef.kind == "Pod" && has(object.spec.boundObjectRef.name) && object.spec.boundObjectRef.name != "" && has(object.spec.boundObjectRef.uid) && object.spec.boundObjectRef.uid != "" && (((%s) && %s) || (request.name == %q && %s) || (request.name.matches(%q) && (object.spec.boundObjectRef.name.matches(%q) || object.spec.boundObjectRef.name.matches(%q) || object.spec.boundObjectRef.name.matches(%q))) || (request.name.matches(%q) && object.spec.boundObjectRef.name.matches(%q))))`,
		controllerNameMatch,
		runtimePodRequestNameExpression("object.spec.boundObjectRef.name", g.ControllerDeploymentName),
		g.CertificateServiceAccountName,
		runtimePodRequestNameExpression("object.spec.boundObjectRef.name", g.CertificateServiceAccountName),
		hookServiceAccountPattern,
		hookPodPattern,
		hookIdentityPodPattern,
		quiescePodPattern,
		teardownServiceAccountPattern,
		teardownPodPattern,
	)
	validations := []admissionregistrationv1.Validation{
		{Expression: `!variables.isAnyConvergenceProbe || request.dryRun == true`, Message: admissionConvergenceMutationDenialMessage},
		{Expression: `!variables.isMarkerProbe || (` + g.contractShapeExpression() + `)`, Message: admissionConvergenceContractDenialMessage},
	}
	for _, state := range g.allowedStates() {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(`!variables.isMarkerProbe || !(%s && (%s))`, proof, admissionConvergenceStateExpression(state)),
			Message:    admissionConvergenceDenialMessage(state),
		})
	}
	validations = append(validations,
		admissionregistrationv1.Validation{
			Expression: `variables.isAnyConvergenceProbe || (` + g.activationShapeExpression("params") + `)`,
			Message:    serviceAccountOriginGuardDenialMessage(),
		},
		admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(
				`variables.isAnyConvergenceProbe || !variables.isControllerCaller || (variables.controllerCredentialPhase == %q && (%s))`,
				ControllerCredentialsActive,
				controllerPrincipalAuthorityExpression(
					g.ReleaseNamespace,
					g.ControllerServiceAccountName,
					g.PreviousControllerServiceAccountName,
					g.ReleaseSequence,
					g.PreviousControllerReleaseSequence,
				),
			),
			Message: controllerPrincipalGuardDenialMessage(),
		},
		admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(
				`variables.isAnyConvergenceProbe || !variables.isControllerTokenRequest || (variables.controllerCredentialPhase == %q && (%s))`,
				ControllerCredentialsActive,
				g.controllerTokenAuthorityExpression(),
			),
			Message: controllerPrincipalGuardDenialMessage(),
		},
		admissionregistrationv1.Validation{Expression: callerOrigin, Message: serviceAccountOriginGuardDenialMessage()},
		admissionregistrationv1.Validation{Expression: tokenOrigin, Message: serviceAccountOriginGuardDenialMessage()},
	)
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(name, admissionConvergencePolicyHookWeight),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			ParamKind:     &admissionregistrationv1.ParamKind{APIVersion: "v1", Kind: "ConfigMap"},
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{
							admissionregistrationv1.Create,
							admissionregistrationv1.Update,
							admissionregistrationv1.Delete,
							admissionregistrationv1.Connect,
						},
						Rule: admissionregistrationv1.Rule{
							APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"*/*"},
							Scope: scopePtr(admissionregistrationv1.AllScopes),
						},
					},
				}},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name:       "admission-probe-or-controller-credential-boundary",
				Expression: fmt.Sprintf(`(%s) || (%s)`, anyConvergenceProbeRequest, protectedOriginMatch),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "markerExpected", Expression: decimalCEL("object", admissionConvergenceExpectedDataKey, true)},
				{Name: "markerAttempt", Expression: stringDataCEL("object", admissionConvergenceAttemptDataKey)},
				{Name: "activeRelease", Expression: decimalCEL("params", activeReleaseDataKey, true)},
				{Name: "controllerCredentialPhase", Expression: stringDataCEL("params", controllerCredentialsDataKey)},
				{Name: "paramsDrainTarget", Expression: decimalCEL("params", controllerCredentialsTargetDataKey, false)},
				{Name: "paramsDrainAttempt", Expression: stringDataCEL("params", controllerCredentialsAttemptDataKey)},
				{Name: "isAnyConvergenceProbe", Expression: anyConvergenceProbeRequest},
				{Name: "isMarkerProbe", Expression: markerRequest},
				{Name: "isTokenRequest", Expression: isTokenRequest},
				{Name: "isHookCaller", Expression: hookCaller},
				{Name: "isControllerCaller", Expression: controllerCaller},
				{Name: "isCertificateCaller", Expression: certificateCaller},
				{Name: "isProtectedCaller", Expression: protectedCaller},
				{Name: "isControllerTokenRequest", Expression: controllerTokenRequest},
				{Name: "isProtectedTokenRequest", Expression: protectedTokenRequest},
				{Name: "callerPodName", Expression: callerPodName},
				{Name: "callerPodUID", Expression: callerPodUID},
			},
			Validations: validations,
		},
	}
}

func (g *AdmissionConvergenceGuard) binding() *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	deny := admissionregistrationv1.DenyAction
	name := AdmissionConvergencePolicyName(g.ReleaseNamespace, g.ReleaseName)
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.metadata(name, admissionConvergenceBindingHookWeight),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName: name,
			ParamRef: &admissionregistrationv1.ParamRef{
				Name:                    ReleaseActivationName,
				Namespace:               g.ReleaseNamespace,
				ParameterNotFoundAction: &deny,
			},
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
}

func (g *AdmissionConvergenceGuard) marker() *corev1.ConfigMap {
	return g.unsealedMarker()
}

func (g *AdmissionConvergenceGuard) markerMetadata(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: g.ReleaseNamespace,
		Annotations: map[string]string{
			"helm.sh/hook":                        "pre-install,pre-upgrade",
			"helm.sh/hook-weight":                 admissionConvergenceMarkerHookWeight,
			"helm.sh/resource-policy":             "keep",
			admissionConvergenceVersionAnnotation: admissionConvergenceContractVersion,
			ReleaseNameAnnotation:                 g.ReleaseName,
			ReleaseNamespaceAnnotation:            g.ReleaseNamespace,
			ReleaseSequenceAnnotation:             strconv.FormatInt(int64(g.ReleaseSequence), 10),
			ManagerImageAnnotation:                g.ManagerImage,
			admissionConvergenceCleanupAnnotation: g.CleanupServiceAccountName,
		},
		Labels: map[string]string{
			managedByLabel:                rolloutGuardManagedBy,
			instanceLabel:                 g.ReleaseName,
			"app.kubernetes.io/component": admissionConvergenceComponent,
		},
	}
}

func (g *AdmissionConvergenceGuard) metadata(name, hookWeight string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name,
		Annotations: map[string]string{
			"helm.sh/hook":                        "pre-install,pre-upgrade",
			"helm.sh/hook-weight":                 hookWeight,
			"helm.sh/hook-delete-policy":          "before-hook-creation",
			admissionConvergenceVersionAnnotation: admissionConvergenceContractVersion,
			ReleaseNameAnnotation:                 g.ReleaseName,
			ReleaseNamespaceAnnotation:            g.ReleaseNamespace,
			ReleaseSequenceAnnotation:             strconv.FormatInt(int64(g.ReleaseSequence), 10),
			ManagerImageAnnotation:                g.ManagerImage,
			admissionConvergenceCleanupAnnotation: g.CleanupServiceAccountName,
		},
		Labels: map[string]string{
			managedByLabel:                rolloutGuardManagedBy,
			instanceLabel:                 g.ReleaseName,
			"app.kubernetes.io/component": admissionConvergenceComponent,
		},
	}
}

func (g *AdmissionConvergenceGuard) verifyPolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	expected := g.policy()
	if policy == nil || !exactAdmissionConvergenceClusterMetadata(policy.ObjectMeta, expected.ObjectMeta) {
		return fmt.Errorf("admission convergence ValidatingAdmissionPolicy/%s has foreign or incomplete ownership", expected.Name)
	}
	if !reflect.DeepEqual(policy.Spec, expected.Spec) {
		return fmt.Errorf("admission convergence ValidatingAdmissionPolicy/%s differs from the exact contract", expected.Name)
	}
	return nil
}

func (g *AdmissionConvergenceGuard) verifyBinding(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
	expected := g.binding()
	if binding == nil || !exactAdmissionConvergenceClusterMetadata(binding.ObjectMeta, expected.ObjectMeta) {
		return fmt.Errorf("admission convergence ValidatingAdmissionPolicyBinding/%s has foreign or incomplete ownership", expected.Name)
	}
	if !reflect.DeepEqual(binding.Spec, expected.Spec) {
		return fmt.Errorf("admission convergence ValidatingAdmissionPolicyBinding/%s differs from the exact contract", expected.Name)
	}
	return nil
}

func (g *AdmissionConvergenceGuard) verifyMarker(marker *corev1.ConfigMap) error {
	if marker != nil && marker.Immutable != nil && *marker.Immutable {
		_, err := g.verifySealedMarker(marker)
		return err
	}
	return g.verifyUnsealedMarker(marker)
}

func (g *AdmissionConvergenceGuard) markerShapeExpression(object string) string {
	return "((" + g.markerStateShapeExpression(object, false) + ") || (" + g.markerStateShapeExpression(object, true) + "))"
}

func (g *AdmissionConvergenceGuard) markerStateShapeExpression(object string, sealed bool) string {
	expected := g.unsealedMarker()
	annotationParts := make([]string, 0, len(expected.Annotations))
	for key, value := range expected.Annotations {
		annotationParts = append(annotationParts, fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, key, value))
	}
	slices.Sort(annotationParts)
	return strings.Join([]string{
		fmt.Sprintf(`%s.metadata.name == %q`, object, expected.Name),
		fmt.Sprintf(`%s.metadata.namespace == %q`, object, g.ReleaseNamespace),
		fmt.Sprintf(`(!has(%s.metadata.generateName) || %s.metadata.generateName == "")`, object, object),
		fmt.Sprintf(`has(%s.metadata.uid) && %s.metadata.uid != ""`, object, object),
		fmt.Sprintf(`has(%s.metadata.resourceVersion) && %s.metadata.resourceVersion != ""`, object, object),
		fmt.Sprintf(`has(%s.metadata.annotations) && %s.metadata.annotations.size() == %d`, object, object, len(expected.Annotations)),
		strings.Join(annotationParts, " && "),
		fmt.Sprintf(`has(%s.metadata.labels) && %s.metadata.labels.size() == 3`, object, object),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, managedByLabel, rolloutGuardManagedBy),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, instanceLabel, g.ReleaseName),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, "app.kubernetes.io/component", admissionConvergenceComponent),
		fmt.Sprintf(`has(%s.data) && %s.data.size() == %d`, object, object, 2+boolToInt(sealed)),
		fmt.Sprintf(`%s.data[%q] == %q`, object, admissionConvergenceExpectedDataKey, expected.Data[admissionConvergenceExpectedDataKey]),
		fmt.Sprintf(`%s.data[%q] == %q`, object, admissionConvergenceAttemptDataKey, expected.Data[admissionConvergenceAttemptDataKey]),
		fmt.Sprintf(`(!has(%s.binaryData) || %s.binaryData.size() == 0)`, object, object),
		admissionConvergenceMarkerImmutableExpression(object, sealed),
		fmt.Sprintf(`(!has(%s.metadata.ownerReferences) || %s.metadata.ownerReferences.size() == 0)`, object, object),
		fmt.Sprintf(`(!has(%s.metadata.finalizers) || %s.metadata.finalizers.size() == 0)`, object, object),
		fmt.Sprintf(`!has(%s.metadata.deletionTimestamp)`, object),
		fmt.Sprintf(`!has(%s.metadata.deletionGracePeriodSeconds)`, object),
	}, " && ") + admissionConvergenceMarkerInventoryExpression(object, sealed)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func admissionConvergenceMarkerImmutableExpression(object string, sealed bool) string {
	if sealed {
		return fmt.Sprintf(`has(%s.immutable) && %s.immutable == true`, object, object)
	}
	return fmt.Sprintf(`(!has(%s.immutable) || %s.immutable == false)`, object, object)
}

func admissionConvergenceMarkerInventoryExpression(object string, sealed bool) string {
	if !sealed {
		return ""
	}
	return fmt.Sprintf(` && %q in %s.data && %s.data[%q].matches(%q)`, PredecessorRetirementInventoryDataKey, object, object, PredecessorRetirementInventoryDataKey, `^\{"version":"1","entries":\[.+\]\}$`)
}

func (g *AdmissionConvergenceGuard) activationShapeExpression(object string) string {
	activation := &ReleaseActivationGuard{ReleaseName: g.ReleaseName, ReleaseNamespace: g.ReleaseNamespace}
	return activation.activationObjectShapeExpression(object)
}

func (g *AdmissionConvergenceGuard) serviceAccountUsername(name string) string {
	return "system:serviceaccount:" + g.ReleaseNamespace + ":" + name
}

func (g *AdmissionConvergenceGuard) markerRequestExpression() string {
	return fmt.Sprintf(
		`(%s) && request.userInfo.username in [%q, %q, %q, %q]`,
		admissionConvergenceProbeRequestExpression(
			g.ReleaseNamespace,
			AdmissionConvergenceMarkerName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence),
			g.sentinelProbe(ReleaseActivationState{}).FieldManager,
		),
		g.serviceAccountUsername(g.HookServiceAccountName),
		g.serviceAccountUsername(g.CleanupServiceAccountName),
		g.serviceAccountUsername(g.ControllerServiceAccountName),
		g.serviceAccountUsername(g.CertificateServiceAccountName),
	)
}

func (g *AdmissionConvergenceGuard) anyConvergenceProbeRequestExpression() string {
	return admissionConvergenceAnyProbeRequestExpression(
		g.ReleaseNamespace,
		AdmissionConvergenceMarkerName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence),
	)
}

func (g *AdmissionConvergenceGuard) sentinelProbe(state ReleaseActivationState) admissionConvergenceDependencyProbe {
	probe := newAdmissionConvergenceDependencyProbe(
		AdmissionConvergencePolicyName(g.ReleaseNamespace, g.ReleaseName),
		hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage),
	)
	probe.Message = admissionConvergenceDenialMessage(state)
	return probe
}

func (g *AdmissionConvergenceGuard) dependencyProbes() []admissionConvergenceDependencyProbe {
	attempt := hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	entries := predecessorRetirementExpectedEntries(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	probes := make([]admissionConvergenceDependencyProbe, 0, (len(entries)-1)/2)
	for index := 0; index+1 < len(entries); index += 2 {
		probes = append(probes, newAdmissionConvergenceDependencyProbe(entries[index].Name, attempt))
	}
	return probes
}

func (g *AdmissionConvergenceGuard) controllerTokenRequestExpression() string {
	serviceAccounts := []string{g.ControllerServiceAccountName}
	if g.PreviousControllerServiceAccountName != "" {
		serviceAccounts = append(serviceAccounts, g.PreviousControllerServiceAccountName)
	}
	quoted := make([]string, len(serviceAccounts))
	for index, serviceAccount := range serviceAccounts {
		quoted[index] = strconv.Quote(serviceAccount)
	}
	return fmt.Sprintf(
		`request.operation == "CREATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "serviceaccounts" && has(request.subResource) && request.subResource == "token" && request.namespace == %q && request.name in [%s]`,
		g.ReleaseNamespace,
		strings.Join(quoted, ", "),
	)
}

func (g *AdmissionConvergenceGuard) controllerTokenAuthorityExpression() string {
	authority := fmt.Sprintf(
		`request.name == %q && variables.activeRelease == %d`,
		g.ControllerServiceAccountName,
		g.ReleaseSequence,
	)
	if g.PreviousControllerServiceAccountName == "" {
		return authority
	}
	return fmt.Sprintf(
		`(%s) || (request.name == %q && variables.activeRelease == %d)`,
		authority,
		g.PreviousControllerServiceAccountName,
		g.PreviousControllerReleaseSequence,
	)
}

func (g *AdmissionConvergenceGuard) allowedActiveSequences() []int32 {
	allowed := []int32{0, g.PreviousControllerReleaseSequence, g.ReleaseSequence}
	slices.Sort(allowed)
	return slices.Compact(allowed)
}

func (g *AdmissionConvergenceGuard) allowedStates() []ReleaseActivationState {
	sequences := g.allowedActiveSequences()
	states := make([]ReleaseActivationState, 0, len(sequences)*2)
	for _, active := range sequences {
		states = append(states,
			ReleaseActivationState{
				ActiveReleaseSequence: active, ControllerCredentialPhase: ControllerCredentialsActive,
			},
			ReleaseActivationState{
				ActiveReleaseSequence: active, ControllerCredentialPhase: ControllerCredentialsDraining,
				DrainTargetReleaseSequence: g.ReleaseSequence,
				DrainAttempt:               hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage),
			},
		)
	}
	return states
}

func (g *AdmissionConvergenceGuard) proofExpression() string {
	return fmt.Sprintf(`request.dryRun == true && (%s)`, g.contractShapeExpression())
}

func (g *AdmissionConvergenceGuard) contractShapeExpression() string {
	stateParts := make([]string, 0, len(g.allowedStates()))
	for _, state := range g.allowedStates() {
		stateParts = append(stateParts, "("+admissionConvergenceStateExpression(state)+")")
	}
	return fmt.Sprintf(
		`request.operation == "UPDATE" && oldObject != null && (%s) && (%s) && object.metadata.uid == oldObject.metadata.uid && object.metadata.resourceVersion == oldObject.metadata.resourceVersion && params != null && (%s) && variables.markerExpected == %d && variables.markerAttempt == %q && (%s)`,
		g.markerShapeExpression("object"),
		g.markerShapeExpression("oldObject"),
		g.activationShapeExpression("params"),
		g.ReleaseSequence,
		hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage),
		strings.Join(stateParts, " || "),
	)
}

func admissionConvergenceStateExpression(state ReleaseActivationState) string {
	expectedTarget := state.DrainTargetReleaseSequence
	if state.ControllerCredentialPhase == ControllerCredentialsActive {
		expectedTarget = -1
	}
	return fmt.Sprintf(
		`variables.activeRelease == %d && variables.controllerCredentialPhase == %q && variables.paramsDrainTarget == %d && variables.paramsDrainAttempt == %q`,
		state.ActiveReleaseSequence,
		state.ControllerCredentialPhase,
		expectedTarget,
		state.DrainAttempt,
	)
}

func admissionConvergenceDenialMessage(state ReleaseActivationState) string {
	message := admissionConvergenceDenialMessagePrefix + "active=" + strconv.FormatInt(int64(state.ActiveReleaseSequence), 10) +
		",phase=" + string(state.ControllerCredentialPhase)
	if state.ControllerCredentialPhase == ControllerCredentialsDraining {
		message += ",target=" + strconv.FormatInt(int64(state.DrainTargetReleaseSequence), 10) + ",attempt=" + state.DrainAttempt
	}
	return message
}

func exactAdmissionConvergenceClusterMetadata(actual, expected metav1.ObjectMeta) bool {
	return actual.Name == expected.Name && actual.Namespace == "" && actual.GenerateName == "" &&
		actual.DeletionTimestamp == nil && actual.DeletionGracePeriodSeconds == nil &&
		len(actual.OwnerReferences) == 0 && len(actual.Finalizers) == 0 &&
		reflect.DeepEqual(actual.Annotations, expected.Annotations) &&
		reflect.DeepEqual(actual.Labels, expected.Labels)
}
