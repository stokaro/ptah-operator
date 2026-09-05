package crdupgrade

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	teardownRetirementContractVersion = "1"
	teardownRetirementComponent       = "teardown-retirement"
	teardownRetirementMarkerPrefix    = "ptah-teardown-probe-v1-"
	teardownRetirementFenceAPrefix    = "ptah-operator-teardown-fence-a-v1-"
	teardownRetirementFenceBPrefix    = "ptah-operator-teardown-fence-b-v1-"
	teardownRetirementBootstrapPrefix = "ptah-teardown-bootstrap-v1-"
	teardownRetirementProbeAJobPrefix = "ptah-teardown-probe-a-v1-"
	teardownRetirementGateJobPrefix   = "ptah-teardown-gate-v1-"
	teardownRetirementProbePrefix     = "ptah-teardown-v1-"

	teardownRetirementVersionAnnotation = "operator.ptah.dev/teardown-retirement-version"
	teardownRetirementAttemptAnnotation = "operator.ptah.dev/teardown-retirement-attempt"
	teardownRetirementTargetAnnotation  = "operator.ptah.dev/teardown-retirement-target"

	teardownRetirementMarkerHookWeight = "-330"
	teardownFenceAPolicyHookWeight     = "-329"
	teardownFenceABindingHookWeight    = "-328"
	teardownFenceBPolicyHookWeight     = "-310"
	teardownFenceBBindingHookWeight    = "-309"
	teardownRetirementPairFirstWeight  = 10
	teardownRetirementPairLastWeight   = 99

	teardownRetirementAttemptDataKey  = "release-attempt"
	teardownRetirementSequenceDataKey = "release-sequence"
)

// TeardownFence identifies one side of the alternating uninstall safety
// fence. The two values deliberately have different names and probe causes so
// one remains authoritative while Helm replaces the other during a retry.
type TeardownFence string

const (
	TeardownFenceA TeardownFence = "a"
	TeardownFenceB TeardownFence = "b"
)

// TeardownPairForm is the only accepted state of a policy/binding pair while
// Helm replaces release admission guards with marker-only retirement hooks.
type TeardownPairForm string

const (
	TeardownPairOriginal         TeardownPairForm = "original"
	TeardownPairReplacingPolicy  TeardownPairForm = "replacing-policy"
	TeardownPairPolicyReplaced   TeardownPairForm = "policy-replaced"
	TeardownPairReplacingBinding TeardownPairForm = "replacing-binding"
	TeardownPairReplayingPolicy  TeardownPairForm = "replaying-policy"
	TeardownPairLegacyRecovery   TeardownPairForm = "legacy-recovery"
	TeardownPairRetirement       TeardownPairForm = "retirement"
	TeardownPairAbsent           TeardownPairForm = "absent"
)

// TeardownRetirementPhase selects the only two activation states understood
// by retirement preflight. Callers derive it from an exact activation GET;
// they must never infer terminal state from a failed or unauthorized read.
type TeardownRetirementPhase string

const (
	TeardownRetirementActive   TeardownRetirementPhase = "active"
	TeardownRetirementTerminal TeardownRetirementPhase = "terminal"
)

// TeardownRetirementProbe describes one exact dry-run denial. Callers must
// match both the policy and binding names and this unique message; a generic
// Forbidden response is not evidence of endpoint convergence.
type TeardownRetirementProbe struct {
	PolicyName   string
	BindingName  string
	FieldManager string
	Message      string
}

// TeardownOriginalPairVerifier carries the exact verifier for a release guard
// that Helm is allowed to replace. Discovery is intentionally forbidden: the
// list comes from the compiled ReleaseTeardown inventory.
type TeardownOriginalPairVerifier struct {
	Name          string
	OptionalGroup string
	VerifyPolicy  func(*admissionregistrationv1.ValidatingAdmissionPolicy) error
	VerifyBinding func(*admissionregistrationv1.ValidatingAdmissionPolicyBinding) error
}

// TeardownRetirementPair is one deterministic Helm replacement. PolicyWeight
// always precedes BindingWeight, and every weight remains below the final
// retirement Job.
type TeardownRetirementPair struct {
	Original      TeardownOriginalPairVerifier
	Policy        *admissionregistrationv1.ValidatingAdmissionPolicy
	Binding       *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	Probe         TeardownRetirementProbe
	PolicyWeight  int
	BindingWeight int
}

type teardownRetirementObjectForm uint8

const (
	teardownRetirementObjectForeign teardownRetirementObjectForm = iota
	teardownRetirementObjectAbsent
	teardownRetirementObjectOriginal
	teardownRetirementObjectRetired
)

type teardownRetirementPairState struct {
	pair    TeardownRetirementPair
	policy  teardownRetirementObjectForm
	binding teardownRetirementObjectForm
}

// TeardownRetirementGuard builds and verifies the no-residue uninstall
// protocol. It never mutates admission objects; Helm owns every policy and
// binding transition, leaving the cleanup credential without VAP/VAPB verbs.
type TeardownRetirementGuard struct {
	rollout         *RolloutGuard
	additionalPairs []TeardownOriginalPairVerifier
}

// TeardownRetirementMarkerTarget is one ConfigMap that the final retirement
// phase may remove. Verify must reject a same-name replacement before the
// finalizer uses UID and resourceVersion preconditions for deletion.
type TeardownRetirementMarkerTarget struct {
	Name   string
	Verify func(*corev1.ConfigMap) error
}

// TeardownRetirementActivationReader is the read-only API surface used to
// derive retirement phase from the exact release-activation object.
type TeardownRetirementActivationReader interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error)
}

// WithOriginalPairs returns a copy extended with explicit append-only guard
// contracts that are not yet part of ReleaseTeardown. It is used for optional
// chart features such as generated-certificate recovery; callers must supply
// exact metadata and spec verifiers, not name-only predicates.
func (g *TeardownRetirementGuard) WithOriginalPairs(pairs ...TeardownOriginalPairVerifier) (*TeardownRetirementGuard, error) {
	if err := g.validate(); err != nil {
		return nil, err
	}
	copy := *g
	copy.additionalPairs = slices.Clone(g.additionalPairs)
	seen := make(map[string]struct{}, len(copy.additionalPairs)+len(pairs))
	for _, pair := range copy.additionalPairs {
		seen[pair.Name] = struct{}{}
	}
	for _, pair := range pairs {
		if pair.Name == "" || pair.VerifyPolicy == nil || pair.VerifyBinding == nil {
			return nil, errors.New("additional teardown retirement pair is incomplete")
		}
		if _, exists := seen[pair.Name]; exists {
			return nil, fmt.Errorf("additional teardown retirement pair %s is duplicated", pair.Name)
		}
		seen[pair.Name] = struct{}{}
		copy.additionalPairs = append(copy.additionalPairs, pair)
	}
	return &copy, nil
}

// NewTeardownRetirementGuard binds the retirement protocol to one immutable
// release attempt.
func NewTeardownRetirementGuard(rollout *RolloutGuard) *TeardownRetirementGuard {
	if rollout == nil {
		return nil
	}
	copy := *rollout
	copy.ControllerArgs = slices.Clone(rollout.ControllerArgs)
	copy.CertificateArgs = slices.Clone(rollout.CertificateArgs)
	copy.RuntimeDeploymentConfigExpressions = slices.Clone(rollout.RuntimeDeploymentConfigExpressions)
	copy.RuntimePodConfigExpressions = slices.Clone(rollout.RuntimePodConfigExpressions)
	return &TeardownRetirementGuard{rollout: &copy}
}

// TeardownRetirementAttempt returns the full lowercase SHA-256 identity for
// the immutable uninstall attempt.
func TeardownRetirementAttempt(releaseNamespace, releaseName string, releaseSequence int32, managerImage string) (string, error) {
	if releaseNamespace == "" || releaseNamespace != strings.TrimSpace(releaseNamespace) {
		return "", errors.New("teardown retirement release namespace is required")
	}
	if releaseName == "" || releaseName != strings.TrimSpace(releaseName) {
		return "", errors.New("teardown retirement release name is required")
	}
	if releaseSequence < 1 {
		return "", errors.New("teardown retirement release sequence must be positive")
	}
	if !admissionConvergenceManagerImagePattern.MatchString(managerImage) {
		return "", errors.New("teardown retirement manager image must be digest-pinned")
	}
	digest := sha256.Sum256([]byte(releaseNamespace + "\n" + releaseName + "\n" + strconv.FormatInt(int64(releaseSequence), 10) + "\n" + managerImage))
	return fmt.Sprintf("%x", digest), nil
}

// TeardownRetirementProbeName returns the dedicated immutable ConfigMap name.
func TeardownRetirementProbeName(releaseNamespace, releaseName string, releaseSequence int32, managerImage string) (string, error) {
	attempt, err := TeardownRetirementAttempt(releaseNamespace, releaseName, releaseSequence, managerImage)
	if err != nil {
		return "", err
	}
	return teardownRetirementMarkerPrefix + strconv.FormatInt(int64(releaseSequence), 10) + "-" + attempt[:12], nil
}

// TeardownRetirementFenceName returns the release-stable A or B fence name.
// The ordinary dormant anchors carrying these names must remain part of every
// release manifest so Helm's normal deletion phase owns their final removal.
// Sequence and image are still validated but deliberately do not affect the
// name; otherwise an upgrade could orphan the prior release's fence anchors.
func TeardownRetirementFenceName(fence TeardownFence, releaseNamespace, releaseName string, releaseSequence int32, managerImage string) (string, error) {
	if _, err := TeardownRetirementAttempt(releaseNamespace, releaseName, releaseSequence, managerImage); err != nil {
		return "", err
	}
	digest := teardownRetirementReleaseDigest(releaseNamespace, releaseName)
	switch fence {
	case TeardownFenceA:
		return teardownRetirementFenceAPrefix + digest, nil
	case TeardownFenceB:
		return teardownRetirementFenceBPrefix + digest, nil
	default:
		return "", fmt.Errorf("unknown teardown fence %q", fence)
	}
}

func teardownRetirementReleaseDigest(releaseNamespace, releaseName string) string {
	digest := sha256.Sum256([]byte(teardownRetirementContractVersion + "\n" + releaseNamespace + "\n" + releaseName))
	return fmt.Sprintf("%x", digest)[:12]
}

// TeardownRetirementFinalJobName returns the candidate-specific final proof
// Job name without extending an already maximal cleanup ServiceAccount name.
func TeardownRetirementFinalJobName(hookServiceAccountName string) (string, error) {
	parts := teardownHookIdentityPattern.FindStringSubmatch(hookServiceAccountName)
	if len(parts) != 4 || parts[1] == "" {
		return "", errors.New("hook ServiceAccount does not encode a candidate release identity")
	}
	name := parts[1] + "-retire-v" + parts[2] + "-" + parts[3]
	if len(name) > 63 {
		return "", errors.New("teardown retirement final Job name exceeds the Kubernetes DNS label limit")
	}
	return name, nil
}

func (g *TeardownRetirementGuard) validate() error {
	if g == nil || g.rollout == nil {
		return errors.New("teardown retirement rollout identity is required")
	}
	if err := g.rollout.validateIdentity(); err != nil {
		return fmt.Errorf("validate teardown retirement identity: %w", err)
	}
	if _, err := TeardownRetirementAttempt(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence, g.rollout.ManagerImage); err != nil {
		return err
	}
	if _, err := TeardownServiceAccountName(g.rollout.HookServiceAccountName, g.rollout.ReleaseSequence); err != nil {
		return err
	}
	if _, err := TeardownQuiesceJobName(g.rollout.HookServiceAccountName); err != nil {
		return err
	}
	if _, err := TeardownRetirementFinalJobName(g.rollout.HookServiceAccountName); err != nil {
		return err
	}
	return nil
}

func (g *TeardownRetirementGuard) attempt() string {
	attempt, _ := TeardownRetirementAttempt(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence, g.rollout.ManagerImage)
	return attempt
}

func (g *TeardownRetirementGuard) markerName() string {
	name, _ := TeardownRetirementProbeName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence, g.rollout.ManagerImage)
	return name
}

func (g *TeardownRetirementGuard) fenceName(fence TeardownFence) string {
	name, _ := TeardownRetirementFenceName(fence, g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence, g.rollout.ManagerImage)
	return name
}

func (g *TeardownRetirementGuard) cleanupServiceAccountName() string {
	name, _ := TeardownServiceAccountName(g.rollout.HookServiceAccountName, g.rollout.ReleaseSequence)
	return name
}

func (g *TeardownRetirementGuard) quiesceJobName() string {
	name, _ := TeardownQuiesceJobName(g.rollout.HookServiceAccountName)
	return name
}

func (g *TeardownRetirementGuard) cleanupJobName() string {
	return g.cleanupServiceAccountName()
}

func (g *TeardownRetirementGuard) finalJobName() string {
	name, _ := TeardownRetirementFinalJobName(g.rollout.HookServiceAccountName)
	return name
}

func (g *TeardownRetirementGuard) bootstrapServiceAccountName() string {
	return teardownRetirementBootstrapPrefix + teardownRetirementReleaseDigest(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
}

func (g *TeardownRetirementGuard) probeAJobName() string {
	return teardownRetirementProbeAJobPrefix + teardownRetirementReleaseDigest(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
}

func (g *TeardownRetirementGuard) gateJobName() string {
	return teardownRetirementGateJobPrefix + teardownRetirementReleaseDigest(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
}

// Marker returns the harmless, immutable object used by every direct-endpoint
// retirement probe.
func (g *TeardownRetirementGuard) Marker() (*corev1.ConfigMap, error) {
	if err := g.validate(); err != nil {
		return nil, err
	}
	immutable := true
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        g.markerName(),
			Namespace:   g.rollout.ReleaseNamespace,
			Annotations: g.markerAnnotations(),
			Labels:      g.labels(teardownRetirementComponent),
		},
		Immutable: &immutable,
		Data: map[string]string{
			teardownRetirementAttemptDataKey:  g.attempt(),
			teardownRetirementSequenceDataKey: strconv.FormatInt(int64(g.rollout.ReleaseSequence), 10),
		},
	}, nil
}

func (g *TeardownRetirementGuard) markerAnnotations() map[string]string {
	return map[string]string{
		"helm.sh/hook":                      "pre-delete",
		"helm.sh/hook-weight":               teardownRetirementMarkerHookWeight,
		"helm.sh/hook-delete-policy":        "before-hook-creation,hook-succeeded",
		teardownRetirementVersionAnnotation: teardownRetirementContractVersion,
		teardownRetirementAttemptAnnotation: g.attempt(),
		ReleaseNameAnnotation:               g.rollout.ReleaseName,
		ReleaseNamespaceAnnotation:          g.rollout.ReleaseNamespace,
		ReleaseSequenceAnnotation:           strconv.FormatInt(int64(g.rollout.ReleaseSequence), 10),
		ManagerImageAnnotation:              g.rollout.ManagerImage,
	}
}

func (g *TeardownRetirementGuard) labels(component string) map[string]string {
	return map[string]string{
		managedByLabel:                rolloutGuardManagedBy,
		instanceLabel:                 g.rollout.ReleaseName,
		"app.kubernetes.io/component": component,
	}
}

func (g *TeardownRetirementGuard) clusterMetadata(name, weight, target, deletePolicy string) metav1.ObjectMeta {
	return g.clusterMetadataForHook(name, "pre-delete", weight, target, deletePolicy)
}

func (g *TeardownRetirementGuard) clusterMetadataForHook(name, hook, weight, target, deletePolicy string) metav1.ObjectMeta {
	annotations := map[string]string{
		teardownRetirementVersionAnnotation: teardownRetirementContractVersion,
		teardownRetirementAttemptAnnotation: g.attempt(),
		teardownRetirementTargetAnnotation:  target,
		ReleaseNameAnnotation:               g.rollout.ReleaseName,
		ReleaseNamespaceAnnotation:          g.rollout.ReleaseNamespace,
		ReleaseSequenceAnnotation:           strconv.FormatInt(int64(g.rollout.ReleaseSequence), 10),
		ManagerImageAnnotation:              g.rollout.ManagerImage,
	}
	if hook != "" {
		annotations["helm.sh/hook"] = hook
		annotations["helm.sh/hook-weight"] = weight
		if deletePolicy != "" {
			annotations["helm.sh/hook-delete-policy"] = deletePolicy
		}
	}
	return metav1.ObjectMeta{Name: name, Annotations: annotations, Labels: g.labels(teardownRetirementComponent)}
}

func (g *TeardownRetirementGuard) markerShapeExpression(object string) string {
	marker, _ := g.Marker()
	annotationKeys := make([]string, 0, len(marker.Annotations))
	for key := range marker.Annotations {
		annotationKeys = append(annotationKeys, key)
	}
	slices.Sort(annotationKeys)
	annotationParts := make([]string, 0, len(annotationKeys))
	for _, key := range annotationKeys {
		annotationParts = append(annotationParts, fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, key, marker.Annotations[key]))
	}
	return strings.Join([]string{
		fmt.Sprintf(`%s.metadata.name == %q`, object, marker.Name),
		fmt.Sprintf(`%s.metadata.namespace == %q`, object, marker.Namespace),
		fmt.Sprintf(`(!has(%s.metadata.generateName) || %s.metadata.generateName == "")`, object, object),
		fmt.Sprintf(`has(%s.metadata.uid) && %s.metadata.uid != ""`, object, object),
		fmt.Sprintf(`has(%s.metadata.resourceVersion) && %s.metadata.resourceVersion != ""`, object, object),
		fmt.Sprintf(`has(%s.metadata.annotations) && %s.metadata.annotations.size() == %d`, object, object, len(marker.Annotations)),
		strings.Join(annotationParts, " && "),
		fmt.Sprintf(`has(%s.metadata.labels) && %s.metadata.labels.size() == %d`, object, object, len(marker.Labels)),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, managedByLabel, rolloutGuardManagedBy),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, instanceLabel, g.rollout.ReleaseName),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, "app.kubernetes.io/component", teardownRetirementComponent),
		fmt.Sprintf(`has(%s.data) && %s.data.size() == 2`, object, object),
		fmt.Sprintf(`%s.data[%q] == %q`, object, teardownRetirementAttemptDataKey, g.attempt()),
		fmt.Sprintf(`%s.data[%q] == %q`, object, teardownRetirementSequenceDataKey, strconv.FormatInt(int64(g.rollout.ReleaseSequence), 10)),
		fmt.Sprintf(`(!has(%s.binaryData) || %s.binaryData.size() == 0)`, object, object),
		fmt.Sprintf(`has(%s.immutable) && %s.immutable == true`, object, object),
		fmt.Sprintf(`(!has(%s.metadata.ownerReferences) || %s.metadata.ownerReferences.size() == 0)`, object, object),
		fmt.Sprintf(`(!has(%s.metadata.finalizers) || %s.metadata.finalizers.size() == 0)`, object, object),
		fmt.Sprintf(`!has(%s.metadata.deletionTimestamp)`, object),
		fmt.Sprintf(`!has(%s.metadata.deletionGracePeriodSeconds)`, object),
	}, " && ")
}

func (g *TeardownRetirementGuard) probe(policyName string) TeardownRetirementProbe {
	digest := sha256.Sum256([]byte(teardownRetirementContractVersion + "\n" + policyName + "\n" + g.attempt()))
	fieldManager := teardownRetirementProbePrefix + fmt.Sprintf("%x", digest)
	return TeardownRetirementProbe{
		PolicyName:   policyName,
		BindingName:  policyName,
		FieldManager: fieldManager,
		Message:      "Ptah teardown retirement confirmed exact policy " + policyName + " with " + fieldManager,
	}
}

func (g *TeardownRetirementGuard) markerProbeRequestExpression(probe TeardownRetirementProbe) string {
	return fmt.Sprintf(
		`request.operation == "UPDATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "configmaps" && (!has(request.subResource) || request.subResource == "") && request.namespace == %q && request.name == %q && has(request.options) && has(request.options.fieldManager) && request.options.fieldManager == %q`,
		g.rollout.ReleaseNamespace,
		g.markerName(),
		probe.FieldManager,
	)
}

func (g *TeardownRetirementGuard) markerProofValidations(probe TeardownRetirementProbe) []admissionregistrationv1.Validation {
	shape := fmt.Sprintf(
		`request.dryRun == true && oldObject != null && (%s) && (%s) && object.metadata.uid == oldObject.metadata.uid && object.metadata.resourceVersion == oldObject.metadata.resourceVersion`,
		g.markerShapeExpression("object"),
		g.markerShapeExpression("oldObject"),
	)
	return []admissionregistrationv1.Validation{
		{Expression: shape, Message: "Ptah teardown retirement probe differs from the exact immutable marker contract"},
		{Expression: "false", Message: probe.Message},
	}
}

func (g *TeardownRetirementGuard) fenceWeights(fence TeardownFence) (string, string, error) {
	switch fence {
	case TeardownFenceA:
		return teardownFenceAPolicyHookWeight, teardownFenceABindingHookWeight, nil
	case TeardownFenceB:
		return teardownFenceBPolicyHookWeight, teardownFenceBBindingHookWeight, nil
	default:
		return "", "", fmt.Errorf("unknown teardown fence %q", fence)
	}
}

func (g *TeardownRetirementGuard) exactBinding(name, weight, target, deletePolicy string) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	return g.exactBindingForHook(name, "pre-delete", weight, target, deletePolicy)
}

func (g *TeardownRetirementGuard) exactBindingForHook(name, hook, weight, target, deletePolicy string) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.clusterMetadataForHook(name, hook, weight, target, deletePolicy),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        name,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
}

func exactTeardownRetirementMetadata(actual, expected metav1.ObjectMeta) bool {
	return actual.Name == expected.Name && actual.GenerateName == "" && actual.Namespace == "" &&
		actual.DeletionTimestamp == nil && actual.DeletionGracePeriodSeconds == nil &&
		len(actual.OwnerReferences) == 0 && len(actual.Finalizers) == 0 &&
		reflect.DeepEqual(actual.Annotations, expected.Annotations) && reflect.DeepEqual(actual.Labels, expected.Labels)
}

// VerifyMarker verifies durable marker identity but deliberately ignores
// API-server-populated metadata such as UID, resourceVersion, and managedFields.
func (g *TeardownRetirementGuard) VerifyMarker(marker *corev1.ConfigMap) error {
	if err := g.validate(); err != nil {
		return err
	}
	expected, _ := g.Marker()
	if marker == nil || marker.Name != expected.Name || marker.Namespace != expected.Namespace || marker.GenerateName != "" ||
		marker.DeletionTimestamp != nil || marker.DeletionGracePeriodSeconds != nil || len(marker.OwnerReferences) != 0 || len(marker.Finalizers) != 0 ||
		!reflect.DeepEqual(marker.Annotations, expected.Annotations) || !reflect.DeepEqual(marker.Labels, expected.Labels) ||
		!reflect.DeepEqual(marker.Data, expected.Data) || len(marker.BinaryData) != 0 || marker.Immutable == nil || !*marker.Immutable {
		return fmt.Errorf("teardown retirement ConfigMap/%s differs from the exact marker contract", expected.Name)
	}
	return nil
}

// MarkerTarget exposes the dedicated probe marker as an exact finalization
// target. Additional retained markers must be supplied by their owning
// contracts; name-only deletion is deliberately unsupported.
func (g *TeardownRetirementGuard) MarkerTarget() (TeardownRetirementMarkerTarget, error) {
	if err := g.validate(); err != nil {
		return TeardownRetirementMarkerTarget{}, err
	}
	return TeardownRetirementMarkerTarget{Name: g.markerName(), Verify: g.VerifyMarker}, nil
}

// Phase derives the only authorized retirement phase from one exact
// release-activation GET. Exact NotFound is terminal. A present parameter is
// active only when its full identity is compatible with this release and its
// credential state is either active or draining for this exact attempt.
// Authorization, transport, and other read failures never imply terminal
// state.
func (g *TeardownRetirementGuard) Phase(ctx context.Context, reader TeardownRetirementActivationReader) (TeardownRetirementPhase, error) {
	if err := g.validate(); err != nil {
		return "", err
	}
	if reader == nil {
		return "", errors.New("teardown retirement activation reader is required")
	}
	object, err := reader.Get(ctx, ReleaseActivationName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return TeardownRetirementTerminal, nil
	}
	if err != nil {
		return "", fmt.Errorf("get teardown retirement activation parameter: %w", err)
	}
	activation := g.rollout.releaseActivationGuard()
	identity, err := activation.verifyActivationObject(object)
	if err != nil {
		return "", fmt.Errorf("verify teardown retirement activation parameter: %w", err)
	}
	if err := activation.verifyCandidateCompatibility(identity); err != nil {
		return "", err
	}
	state, err := releaseActivationState(identity)
	if err != nil {
		return "", err
	}
	switch state.ControllerCredentialPhase {
	case ControllerCredentialsActive:
		return TeardownRetirementActive, nil
	case ControllerCredentialsDraining:
		if state.DrainTargetReleaseSequence != g.rollout.ReleaseSequence || state.DrainAttempt != g.attempt() {
			return "", fmt.Errorf("teardown retirement activation drain tuple differs from the exact attempt")
		}
		return TeardownRetirementActive, nil
	default:
		return "", fmt.Errorf("teardown retirement activation has unknown credential phase %q", state.ControllerCredentialPhase)
	}
}

// VerifyFinalActivation verifies the exact release activation object and the
// candidate-specific draining tuple that authorizes this retirement attempt.
// The active sequence may still be bootstrap, predecessor, or candidate; the
// activation contract already restricts it to that finite compatibility set.
func (g *TeardownRetirementGuard) VerifyFinalActivation(object *corev1.ConfigMap) error {
	if err := g.validate(); err != nil {
		return err
	}
	activation := g.rollout.releaseActivationGuard()
	identity, err := activation.verifyActivationObject(object)
	if err != nil {
		return fmt.Errorf("verify teardown retirement activation parameter: %w", err)
	}
	if err := activation.verifyCandidateCompatibility(identity); err != nil {
		return err
	}
	state, err := releaseActivationState(identity)
	if err != nil {
		return err
	}
	want := ReleaseActivationState{
		ActiveReleaseSequence:      state.ActiveReleaseSequence,
		ControllerCredentialPhase:  ControllerCredentialsDraining,
		DrainTargetReleaseSequence: g.rollout.ReleaseSequence,
		DrainAttempt:               g.attempt(),
	}
	if state != want {
		return fmt.Errorf("teardown retirement activation state is %#v, want %#v", state, want)
	}
	return nil
}

// Probe submits one unchanged dry-run marker update and accepts only the
// unique denial emitted by the exact named policy/binding pair.
func (g *TeardownRetirementGuard) Probe(ctx context.Context, client AdmissionConvergenceMarkerClient, probe TeardownRetirementProbe) (bool, error) {
	if err := g.validate(); err != nil {
		return false, err
	}
	if client == nil {
		return false, errors.New("teardown retirement marker client is required")
	}
	if probe.PolicyName == "" || probe.BindingName != probe.PolicyName || probe.FieldManager == "" || probe.Message == "" {
		return false, errors.New("teardown retirement probe contract is incomplete")
	}
	marker, err := client.Get(ctx, g.markerName(), metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("get teardown retirement marker: %w", err)
	}
	if err := g.VerifyMarker(marker); err != nil {
		return false, err
	}
	_, err = client.Update(ctx, marker.DeepCopy(), metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: probe.FieldManager})
	if err == nil {
		return false, errors.New("teardown retirement marker update was admitted")
	}
	if !apierrors.IsInvalid(err) && !apierrors.IsForbidden(err) {
		return false, fmt.Errorf("probe teardown retirement policy %s: %w", probe.PolicyName, err)
	}
	if !hasExactValidatingAdmissionPolicyDenial(err, probe.PolicyName, probe.BindingName, probe.Message) {
		return false, nil
	}
	return true, nil
}

func (g *TeardownRetirementGuard) markerOnlyPolicy(name, policyWeight, target, deletePolicy string) *admissionregistrationv1.ValidatingAdmissionPolicy {
	return g.markerOnlyPolicyForHook(name, "pre-delete", policyWeight, target, deletePolicy)
}

func (g *TeardownRetirementGuard) markerOnlyPolicyForHook(name, hook, policyWeight, target, deletePolicy string) *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	probe := g.probe(name)
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.clusterMetadataForHook(name, hook, policyWeight, target, deletePolicy),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
						Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope)},
					},
				}},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{Name: "exact-retirement-probe", Expression: g.markerProbeRequestExpression(probe)}},
			Validations:     g.markerProofValidations(probe),
		},
	}
}

// OriginalFencePair builds one early durable, static safety fence.
// Both A and B carry the entire caller, TokenRequest, Job, Pod, and dangerous
// Pod-subresource closure; neither relies on the other or a parameter object
// for correctness. That independence is essential after activation deletion.
func (g *TeardownRetirementGuard) OriginalFencePair(fence TeardownFence) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding, TeardownRetirementProbe, error) {
	if err := g.validate(); err != nil {
		return nil, nil, TeardownRetirementProbe{}, err
	}
	policyWeight, bindingWeight, err := g.fenceWeights(fence)
	if err != nil {
		return nil, nil, TeardownRetirementProbe{}, err
	}
	name := g.fenceName(fence)
	policy, err := g.originalFencePolicy(name, policyWeight)
	if err != nil {
		return nil, nil, TeardownRetirementProbe{}, err
	}
	binding := g.exactBinding(name, bindingWeight, name, "before-hook-creation")
	return policy, binding, g.probe(name), nil
}

func (g *TeardownRetirementGuard) originalFencePolicy(name, policyWeight string) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	return g.fencePolicy(name, "pre-delete", policyWeight, false)
}

func (g *TeardownRetirementGuard) dormantFencePolicy(name string) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	return g.fencePolicy(name, "", "", true)
}

func (g *TeardownRetirementGuard) fencePolicy(name, hook, policyWeight string, bootstrapOnly bool) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	probe := g.probe(name)
	origin := NewServiceAccountOriginGuard(g.rollout)
	hookBase, err := origin.hookServiceAccountBase()
	if err != nil {
		return nil, err
	}
	cleanupServiceAccount := g.cleanupServiceAccountName()
	bootstrapServiceAccount := g.bootstrapServiceAccountName()
	quiesceJob := g.quiesceJobName()
	cleanupJob := g.cleanupJobName()
	finalJob := g.finalJobName()
	probeAJob := g.probeAJobName()
	gateJob := g.gateJobName()
	hookPattern := "^" + regexp.QuoteMeta(hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	cleanupPattern := "^" + regexp.QuoteMeta(hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	hookUsernamePattern := "^" + regexp.QuoteMeta("system:serviceaccount:"+g.rollout.ReleaseNamespace+":"+hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	cleanupUsernamePattern := "^" + regexp.QuoteMeta("system:serviceaccount:"+g.rollout.ReleaseNamespace+":"+hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	bootstrapUsername := "system:serviceaccount:" + g.rollout.ReleaseNamespace + ":" + bootstrapServiceAccount
	controllerMatch := controllerPrincipalMatchExpression(g.rollout.ReleaseNamespace, g.rollout.ControllerServiceAccountName, g.rollout.PreviousControllerServiceAccountName)
	controllerNames := []string{g.rollout.ControllerServiceAccountName}
	if previous := g.rollout.PreviousControllerServiceAccountName; previous != "" && previous != g.rollout.ControllerServiceAccountName {
		controllerNames = append(controllerNames, previous)
	}
	quotedControllerNames := make([]string, len(controllerNames))
	for index, controllerName := range controllerNames {
		quotedControllerNames[index] = strconv.Quote(controllerName)
	}
	controllerNameMatch := `request.name in [` + strings.Join(quotedControllerNames, ", ") + `]`
	certificateUsername := "system:serviceaccount:" + g.rollout.ReleaseNamespace + ":" + g.rollout.CertificateDeploymentName
	tokenRequest := `request.operation == "CREATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "serviceaccounts" && has(request.subResource) && request.subResource == "token"`
	protectedCaller := fmt.Sprintf(`request.userInfo.username == %q`, bootstrapUsername)
	protectedToken := fmt.Sprintf(`(%s) && request.namespace == %q && request.name == %q`, tokenRequest, g.rollout.ReleaseNamespace, bootstrapServiceAccount)
	jobs := []string{probeAJob, gateJob}
	if !bootstrapOnly {
		protectedCaller = fmt.Sprintf(`(%s) || request.userInfo.username == %q || request.userInfo.username == %q || request.userInfo.username.matches(%q) || request.userInfo.username.matches(%q)`, controllerMatch, certificateUsername, bootstrapUsername, hookUsernamePattern, cleanupUsernamePattern)
		protectedToken = fmt.Sprintf(`(%s) && request.namespace == %q && ((%s) || request.name == %q || request.name == %q || request.name.matches(%q) || request.name.matches(%q))`, tokenRequest, g.rollout.ReleaseNamespace, controllerNameMatch, g.rollout.CertificateDeploymentName, bootstrapServiceAccount, hookPattern, cleanupPattern)
		jobs = []string{quiesceJob, cleanupJob, finalJob, probeAJob, gateJob}
	}
	jobNames := celStringList(jobs)
	protectedJobMain := fmt.Sprintf(`request.resource.group == "batch" && request.resource.version == "v1" && request.resource.resource == "jobs" && (!has(request.subResource) || request.subResource == "") && request.namespace == %q`, g.rollout.ReleaseNamespace)
	protectedJob := protectedJobMain + fmt.Sprintf(` && request.name in %s && request.operation in ["CREATE", "UPDATE"]`, jobNames)
	protectedJobDelete := protectedJobMain + fmt.Sprintf(` && request.operation == "DELETE" && oldObject != null && oldObject.metadata.name in %s`, jobNames)
	protectedJobStatus := fmt.Sprintf(`request.resource.group == "batch" && request.resource.version == "v1" && request.resource.resource == "jobs" && has(request.subResource) && request.subResource == "status" && request.namespace == %q && request.operation == "UPDATE" && oldObject != null && oldObject.metadata.name in %s`, g.rollout.ReleaseNamespace, jobNames)
	podNameExpressions := make([]string, 0, len(jobs))
	oldPodNameExpressions := make([]string, 0, len(jobs))
	for _, job := range jobs {
		podNameExpressions = append(podNameExpressions, generatedPodRequestNameExpression(job))
		oldPodNameExpressions = append(oldPodNameExpressions, generatedPodRequestNameExpressionFor("oldObject.metadata.name", job))
	}
	podNames := strings.Join(podNameExpressions, " || ")
	oldPodNames := strings.Join(oldPodNameExpressions, " || ")
	protectedPodWrite := fmt.Sprintf(`request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "pods" && (!has(request.subResource) || request.subResource == "") && request.namespace == %q && (%s) && request.operation in ["CREATE", "UPDATE"]`, g.rollout.ReleaseNamespace, podNames)
	protectedPodDelete := fmt.Sprintf(`request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "pods" && (!has(request.subResource) || request.subResource == "") && request.namespace == %q && request.operation == "DELETE" && oldObject != null && (%s)`, g.rollout.ReleaseNamespace, oldPodNames)
	protectedPodStatus := fmt.Sprintf(`request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "pods" && has(request.subResource) && request.subResource == "status" && request.namespace == %q && request.operation == "UPDATE" && oldObject != null && (%s)`, g.rollout.ReleaseNamespace, oldPodNames)
	dangerousPodSubresource := fmt.Sprintf(`request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "pods" && has(request.subResource) && request.subResource in ["exec", "attach", "portforward", "proxy", "ephemeralcontainers", "resize"] && request.namespace == %q && (%s)`, g.rollout.ReleaseNamespace, podNames)
	retainedMarkers := []string{
		ReleaseActivationName,
		AdmissionConvergenceMarkerName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence),
		ParentOriginReadyMarkerName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName),
	}
	if g.rollout.PreviousControllerReleaseSequence > 0 {
		retainedMarkers = append(retainedMarkers, AdmissionConvergenceMarkerName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.PreviousControllerReleaseSequence))
	}
	protectedRetainedMarkerDelete := fmt.Sprintf(`request.operation == "DELETE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "configmaps" && (!has(request.subResource) || request.subResource == "") && request.namespace == %q && oldObject != null && oldObject.metadata.name in %s`, g.rollout.ReleaseNamespace, celStringList(retainedMarkers))
	markerRequest := g.markerProbeRequestExpression(probe)
	matchParts := []string{"(" + protectedCaller + ")", "(" + protectedToken + ")", "(" + protectedJob + ")", "(" + protectedJobDelete + ")", "(" + protectedJobStatus + ")", "(" + protectedPodWrite + ")", "(" + protectedPodDelete + ")", "(" + protectedPodStatus + ")", "(" + dangerousPodSubresource + ")"}
	if !bootstrapOnly {
		matchParts = append([]string{"(" + markerRequest + ")", "(" + protectedRetainedMarkerDelete + ")"}, matchParts...)
	}
	match := strings.Join(matchParts, " || ")

	callerPodName := fmt.Sprintf(`has(request.userInfo.extra) && %q in request.userInfo.extra && request.userInfo.extra[%q].size() == 1 ? request.userInfo.extra[%q][0] : ""`, serviceAccountPodNameExtra, serviceAccountPodNameExtra, serviceAccountPodNameExtra)
	callerPodUID := fmt.Sprintf(`has(request.userInfo.extra) && %q in request.userInfo.extra && request.userInfo.extra[%q].size() == 1 ? request.userInfo.extra[%q][0] : ""`, serviceAccountPodUIDExtra, serviceAccountPodUIDExtra, serviceAccountPodUIDExtra)
	quiesceCallerPod := generatedPodRequestNameExpressionFor("variables.callerPodName", quiesceJob)
	cleanupCallerPod := generatedPodRequestNameExpressionFor("variables.callerPodName", cleanupJob)
	finalCallerPod := generatedPodRequestNameExpressionFor("variables.callerPodName", finalJob)
	probeACallerPod := generatedPodRequestNameExpressionFor("variables.callerPodName", probeAJob)
	gateCallerPod := generatedPodRequestNameExpressionFor("variables.callerPodName", gateJob)
	quiesceBoundPod := generatedPodRequestNameExpressionFor("object.spec.boundObjectRef.name", quiesceJob)
	cleanupBoundPod := generatedPodRequestNameExpressionFor("object.spec.boundObjectRef.name", cleanupJob)
	finalBoundPod := generatedPodRequestNameExpressionFor("object.spec.boundObjectRef.name", finalJob)
	probeABoundPod := generatedPodRequestNameExpressionFor("object.spec.boundObjectRef.name", probeAJob)
	gateBoundPod := generatedPodRequestNameExpressionFor("object.spec.boundObjectRef.name", gateJob)

	markerValidations := g.markerProofValidations(probe)
	validations := make([]admissionregistrationv1.Validation, 0, len(markerValidations)+64)
	for _, validation := range markerValidations {
		validation.Expression = `!variables.isMarkerProbe || (` + validation.Expression + `)`
		validations = append(validations, validation)
	}
	validations = append(validations,
		admissionregistrationv1.Validation{
			Expression: `variables.isMarkerProbe || !variables.isControllerCaller`,
			Message:    "Ptah teardown fence rejected a controller caller after uninstall fencing",
		},
		admissionregistrationv1.Validation{
			Expression: `variables.isMarkerProbe || !variables.isCertificateCaller`,
			Message:    "Ptah teardown fence rejected a certificate caller after uninstall fencing",
		},
		admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(`variables.isMarkerProbe || !variables.isBootstrapCaller || (request.userInfo.username == %q && variables.callerPodName != "" && variables.callerPodUID != "" && ((%s) || (%s)))`, bootstrapUsername, probeACallerPod, gateCallerPod),
			Message:    "Ptah teardown fence rejected a bootstrap caller outside the exact probe or gate Pod",
		},
		admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(`variables.isMarkerProbe || !variables.isHookCaller || (request.userInfo.username == %q && variables.callerPodName != "" && variables.callerPodUID != "" && (%s))`, g.rollout.candidateHookUsername(), quiesceCallerPod),
			Message:    "Ptah teardown fence rejected a hook caller outside the exact quiesce Pod",
		},
		admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(`variables.isMarkerProbe || !variables.isCleanupCaller || (request.userInfo.username == %q && variables.callerPodName != "" && variables.callerPodUID != "" && ((%s) || (%s)))`, "system:serviceaccount:"+g.rollout.ReleaseNamespace+":"+cleanupServiceAccount, cleanupCallerPod, finalCallerPod),
			Message:    "Ptah teardown fence rejected a cleanup caller outside the exact cleanup or final Pod",
		},
		admissionregistrationv1.Validation{
			Expression: `variables.isMarkerProbe || !variables.isControllerTokenRequest`,
			Message:    "Ptah teardown fence rejected a controller TokenRequest after uninstall fencing",
		},
		admissionregistrationv1.Validation{
			Expression: `variables.isMarkerProbe || !variables.isCertificateTokenRequest`,
			Message:    "Ptah teardown fence rejected a certificate TokenRequest after uninstall fencing",
		},
		admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(`variables.isMarkerProbe || !variables.isBootstrapTokenRequest || (request.name == %q && variables.boundPod && ((%s) || (%s)))`, bootstrapServiceAccount, probeABoundPod, gateBoundPod),
			Message:    "Ptah teardown fence rejected a bootstrap TokenRequest outside the exact probe or gate Pod",
		},
		admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(`variables.isMarkerProbe || !variables.isHookTokenRequest || (request.name == %q && variables.boundPod && (%s))`, g.rollout.HookServiceAccountName, quiesceBoundPod),
			Message:    "Ptah teardown fence rejected a hook TokenRequest outside the exact quiesce Pod",
		},
		admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(`variables.isMarkerProbe || !variables.isCleanupTokenRequest || (request.name == %q && variables.boundPod && ((%s) || (%s)))`, cleanupServiceAccount, cleanupBoundPod, finalBoundPod),
			Message:    "Ptah teardown fence rejected a cleanup TokenRequest outside the exact cleanup or final Pod",
		},
		admissionregistrationv1.Validation{
			Expression: `!variables.isDangerousPodSubresource`,
			Message:    "Ptah teardown fence rejects interactive and mutable Pod subresources",
		},
		admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(`!variables.isProtectedRetainedMarkerDelete || (variables.isCleanupCaller && variables.callerPodName != "" && variables.callerPodUID != "" && (%s))`, finalCallerPod),
			Message:    "Ptah teardown fence rejected retained marker deletion outside the exact final Pod",
		},
	)
	if bootstrapOnly {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: `!variables.isProtectedJob || (` + teardownRetirementHelmAuthorizerExpression() + `)`,
			Message:    "Ptah teardown bootstrap Job requires cluster admission-management authority",
		})
	}
	for _, expression := range g.teardownJobValidationExpressions(bootstrapOnly) {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: `!variables.isProtectedJob || (` + expression + `)`,
			Message:    "Ptah teardown fence rejected a Job outside the exact uninstall contract",
		})
	}
	for _, expression := range g.teardownJobStatusValidationExpressions(bootstrapOnly) {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: `!variables.isProtectedJobStatus || (` + expression + `)`,
			Message:    "Ptah teardown fence rejected forged Job completion",
		})
	}
	validations = append(validations, admissionregistrationv1.Validation{
		Expression: `!variables.isProtectedJobDelete || (` + teardownRetirementHelmAuthorizerExpression() + `)`,
		Message:    "Ptah teardown Job deletion requires cluster admission-management authority",
	})
	for _, expression := range g.teardownJobDeletionValidationExpressions(bootstrapOnly) {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: `!variables.isProtectedJobDelete || (` + expression + `)`,
			Message:    "Ptah teardown fence rejected deletion of a nonterminal or foreign Job",
		})
	}
	for _, expression := range g.teardownPodValidationExpressions("object", bootstrapOnly) {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: `!variables.isProtectedPodWrite || (` + expression + `)`,
			Message:    "Ptah teardown fence rejected a Pod outside the exact uninstall contract",
		})
	}
	for _, expression := range g.teardownPodStatusValidationExpressions(bootstrapOnly) {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: `!variables.isProtectedPodStatus || (` + expression + `)`,
			Message:    "Ptah teardown fence rejected forged Pod completion",
		})
	}
	for _, expression := range g.teardownPodDeletionIdentityExpressions() {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: `!variables.isProtectedPodDelete || (` + expression + `)`,
			Message:    "Ptah teardown fence rejected deletion of a foreign uninstall Pod",
		})
	}

	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.clusterMetadataForHook(name, hook, policyWeight, name, "before-hook-creation"),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update, admissionregistrationv1.Delete, admissionregistrationv1.Connect},
						Rule:       admissionregistrationv1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"*/*"}, Scope: scopePtr(admissionregistrationv1.AllScopes)},
					},
				}},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{Name: "teardown-probe-or-protected-identity", Expression: match}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "isMarkerProbe", Expression: markerRequest},
				{Name: "isControllerCaller", Expression: controllerMatch},
				{Name: "isCertificateCaller", Expression: fmt.Sprintf(`request.userInfo.username == %q`, certificateUsername)},
				{Name: "isBootstrapCaller", Expression: fmt.Sprintf(`request.userInfo.username == %q`, bootstrapUsername)},
				{Name: "isHookCaller", Expression: fmt.Sprintf(`request.userInfo.username.matches(%q)`, hookUsernamePattern)},
				{Name: "isCleanupCaller", Expression: fmt.Sprintf(`request.userInfo.username.matches(%q)`, cleanupUsernamePattern)},
				{Name: "isControllerTokenRequest", Expression: fmt.Sprintf(`(%s) && (%s)`, tokenRequest, controllerNameMatch)},
				{Name: "isCertificateTokenRequest", Expression: fmt.Sprintf(`(%s) && request.name == %q`, tokenRequest, g.rollout.CertificateDeploymentName)},
				{Name: "isBootstrapTokenRequest", Expression: fmt.Sprintf(`(%s) && request.name == %q`, tokenRequest, bootstrapServiceAccount)},
				{Name: "isHookTokenRequest", Expression: fmt.Sprintf(`(%s) && request.name.matches(%q)`, tokenRequest, hookPattern)},
				{Name: "isCleanupTokenRequest", Expression: fmt.Sprintf(`(%s) && request.name.matches(%q)`, tokenRequest, cleanupPattern)},
				{Name: "callerPodName", Expression: callerPodName},
				{Name: "callerPodUID", Expression: callerPodUID},
				{Name: "boundPod", Expression: `request.userInfo.username.matches("^system:node:.+$") && request.userInfo.groups.size() == 2 && "system:nodes" in request.userInfo.groups && "system:authenticated" in request.userInfo.groups && has(object.spec.boundObjectRef) && has(object.spec.boundObjectRef.apiVersion) && object.spec.boundObjectRef.apiVersion == "v1" && has(object.spec.boundObjectRef.kind) && object.spec.boundObjectRef.kind == "Pod" && has(object.spec.boundObjectRef.name) && object.spec.boundObjectRef.name != "" && has(object.spec.boundObjectRef.uid) && object.spec.boundObjectRef.uid != ""`},
				{Name: "isProtectedJob", Expression: protectedJob},
				{Name: "isProtectedJobDelete", Expression: protectedJobDelete},
				{Name: "isProtectedJobStatus", Expression: protectedJobStatus},
				{Name: "isProtectedPodWrite", Expression: protectedPodWrite},
				{Name: "isProtectedPodDelete", Expression: protectedPodDelete},
				{Name: "isProtectedPodStatus", Expression: protectedPodStatus},
				{Name: "isDangerousPodSubresource", Expression: dangerousPodSubresource},
				{Name: "isProtectedRetainedMarkerDelete", Expression: protectedRetainedMarkerDelete},
			},
			Validations: validations,
		},
	}, nil
}

func generatedPodRequestNameExpressionFor(nameExpression, jobName string) string {
	prefix := generatedNamePrefix(jobName + "-")
	return fmt.Sprintf(`(%[1]s.startsWith(%[2]q) && %[1]s.size() == %[3]d && %[1]s.substring(%[4]d).matches("^[a-z0-9]{%[5]d}$"))`, nameExpression, prefix, len(prefix)+kubernetesGeneratedSuffixLen, len(prefix), kubernetesGeneratedSuffixLen)
}

func teardownRetirementHelmAuthorizerExpression() string {
	return `(request.operation == "CREATE" && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicies").check("create").allowed() && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicybindings").check("create").allowed()) || (request.operation == "UPDATE" && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicies").check("update").allowed() && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicybindings").check("update").allowed()) || (request.operation == "DELETE" && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicies").check("create").allowed() && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicybindings").check("create").allowed() && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicies").check("update").allowed() && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicybindings").check("update").allowed() && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicies").check("delete").allowed() && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicybindings").check("delete").allowed())`
}

func teardownRetirementExactPrincipalExpression(username string, groups ...string) string {
	parts := []string{
		fmt.Sprintf(`request.userInfo.username == %q`, username),
		fmt.Sprintf(`request.userInfo.groups.size() == %d`, len(groups)),
	}
	for _, group := range groups {
		parts = append(parts, fmt.Sprintf(`%q in request.userInfo.groups`, group))
	}
	return strings.Join(parts, " && ")
}

func teardownRetirementJobControllerPrincipalExpression() string {
	return fmt.Sprintf(
		`(%s) || (%s)`,
		teardownRetirementExactPrincipalExpression("system:kube-controller-manager", "system:authenticated"),
		teardownRetirementExactPrincipalExpression(
			"system:serviceaccount:kube-system:job-controller",
			"system:serviceaccounts",
			"system:serviceaccounts:kube-system",
			"system:authenticated",
		),
	)
}

func teardownRetirementSchedulerPrincipalExpression() string {
	return fmt.Sprintf(
		`(%s) || (%s)`,
		teardownRetirementExactPrincipalExpression("system:kube-scheduler", "system:authenticated"),
		teardownRetirementExactPrincipalExpression(
			"system:serviceaccount:kube-system:kube-scheduler",
			"system:serviceaccounts",
			"system:serviceaccounts:kube-system",
			"system:authenticated",
		),
	)
}

func teardownRetirementNodePrincipalExpression() string {
	return `request.userInfo.username.matches("^system:node:.+$") && request.userInfo.groups.size() == 2 && "system:nodes" in request.userInfo.groups && "system:authenticated" in request.userInfo.groups && has(oldObject.spec.nodeName) && oldObject.spec.nodeName != "" && request.userInfo.username == "system:node:" + oldObject.spec.nodeName`
}

func (g *TeardownRetirementGuard) teardownJobValidationExpressions(forwardCompatible bool) []string {
	pod := "object.spec.template.spec"
	templateMetadata := "object.spec.template.metadata"
	container := pod + ".containers[0]"
	volume := pod + ".volumes[0]"
	sources := volume + ".projected.sources"
	quiesceJob := g.quiesceJobName()
	cleanupJob := g.cleanupJobName()
	finalJob := g.finalJobName()
	probeAJob := g.probeAJobName()
	gateJob := g.gateJobName()
	cleanupServiceAccount := g.cleanupServiceAccountName()
	bootstrapServiceAccount := g.bootstrapServiceAccountName()
	allJobs := celStringList([]string{quiesceJob, cleanupJob, finalJob, probeAJob, gateJob})
	bootstrapJobs := celStringList([]string{probeAJob, gateJob})
	component := fmt.Sprintf(`request.name in %s ? "teardown-retirement-bootstrap" : (request.name == %q ? "crd-manager-teardown-quiesce" : (request.name == %q ? "crd-manager-teardown" : "teardown-retirement"))`, bootstrapJobs, quiesceJob, cleanupJob)
	weight := fmt.Sprintf(`request.name == %q ? "-315" : (request.name == %q ? "-305" : (request.name == %q ? "-10" : (request.name == %q ? "0" : "105")))`, probeAJob, gateJob, quiesceJob, cleanupJob)
	deadline := fmt.Sprintf(`request.name == %q ? 90 : (request.name == %q ? 120 : (request.name == %q ? 270 : 210))`, probeAJob, gateJob, finalJob)
	serviceAccount := fmt.Sprintf(`request.name in %s ? %q : (request.name == %q ? %q : %q)`, bootstrapJobs, bootstrapServiceAccount, quiesceJob, g.rollout.HookServiceAccountName, cleanupServiceAccount)
	containerName := fmt.Sprintf(`request.name == %q ? "teardown-retirement-probe-a" : (request.name == %q ? "teardown-retirement-gate" : (request.name == %q ? "crd-manager-teardown-quiesce" : (request.name == %q ? "crd-manager-teardown" : "teardown-retirement")))`, probeAJob, gateJob, quiesceJob, cleanupJob)
	image := fmt.Sprintf(`%[1]s.image == %q && %[1]s.command == ["/ptah-crd-manager"] && %[1]s.imagePullPolicy in ["Always", "IfNotPresent", "Never"]`, container, g.rollout.ManagerImage)
	probeArgs := fmt.Sprintf(`request.name != %q || %s.args == %s`, probeAJob, container, celStringList(g.hookArgsWithTimeout("teardown-retirement-probe-a", "60s")))
	gateArgs := fmt.Sprintf(`request.name != %q || %s.args == %s`, gateJob, container, celStringList(g.hookArgsWithTimeout("teardown-retirement-gate", "90s")))
	if forwardCompatible {
		image = fmt.Sprintf(`%[1]s.image.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && %[1]s.command == ["/ptah-crd-manager"] && %[1]s.imagePullPolicy in ["Always", "IfNotPresent", "Never"]`, container)
		probeArgs = g.forwardBootstrapArgsValidationExpression(container, "request.name")
		gateArgs = `true`
	}
	return []string{
		`!has(request.subResource) || request.subResource == ""`,
		fmt.Sprintf(`request.operation in ["CREATE", "UPDATE"] && request.name in %s && object.metadata.name == request.name && object.metadata.namespace == %q && (!has(object.metadata.generateName) || object.metadata.generateName == "")`, allJobs, g.rollout.ReleaseNamespace),
		`(!has(object.metadata.ownerReferences) || object.metadata.ownerReferences.size() == 0) && (!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0)`,
		fmt.Sprintf(`has(object.metadata.labels) && object.metadata.labels[%q] == %q && object.metadata.labels["app.kubernetes.io/component"] == (%s)`, instanceLabel, g.rollout.ReleaseName, component),
		fmt.Sprintf(`has(object.metadata.annotations) && object.metadata.annotations.size() == 3 && object.metadata.annotations["helm.sh/hook"] == "pre-delete" && object.metadata.annotations["helm.sh/hook-delete-policy"] == "before-hook-creation,hook-succeeded" && object.metadata.annotations["helm.sh/hook-weight"] == (%s)`, weight),
		fmt.Sprintf(`(!has(%[1]s.annotations) || %[1]s.annotations.size() == 0) && (!has(%[1]s.ownerReferences) || %[1]s.ownerReferences.size() == 0) && (!has(%[1]s.finalizers) || %[1]s.finalizers.size() == 0)`, templateMetadata),
		fmt.Sprintf(`has(object.spec.activeDeadlineSeconds) && object.spec.activeDeadlineSeconds == (%s)`, deadline),
		`has(object.spec.backoffLimit) && object.spec.backoffLimit == 0 && !has(object.spec.backoffLimitPerIndex) && !has(object.spec.maxFailedIndexes)`,
		`!has(object.spec.ttlSecondsAfterFinished) && !has(object.spec.podFailurePolicy) && !has(object.spec.successPolicy)`,
		`(!has(object.spec.suspend) || !object.spec.suspend) && (!has(object.spec.manualSelector) || !object.spec.manualSelector) && (!has(object.spec.parallelism) || object.spec.parallelism == 1) && (!has(object.spec.completions) || object.spec.completions == 1) && (!has(object.spec.completionMode) || object.spec.completionMode == "NonIndexed")`,
		fmt.Sprintf(`has(%[1]s.serviceAccountName) && %[1]s.serviceAccountName == (%[2]s) && (!has(%[1]s.serviceAccount) || %[1]s.serviceAccount == %[1]s.serviceAccountName) && has(%[1]s.automountServiceAccountToken) && !%[1]s.automountServiceAccountToken`, pod, serviceAccount),
		fmt.Sprintf(`%[1]s.restartPolicy == "Never" && (!has(%[1]s.nodeName) || %[1]s.nodeName == "") && (!has(%[1]s.nodeSelector) || %[1]s.nodeSelector.size() == 0) && !has(%[1]s.affinity) && (!has(%[1]s.tolerations) || %[1]s.tolerations.size() == 0)`, pod),
		fmt.Sprintf(`(!has(%[1]s.hostNetwork) || !%[1]s.hostNetwork) && (!has(%[1]s.hostPID) || !%[1]s.hostPID) && (!has(%[1]s.hostIPC) || !%[1]s.hostIPC) && (!has(%[1]s.shareProcessNamespace) || !%[1]s.shareProcessNamespace)`, pod),
		fmt.Sprintf(`!has(%[1]s.hostAliases) && !has(%[1]s.hostname) && !has(%[1]s.subdomain) && (!has(%[1]s.setHostnameAsFQDN) || !%[1]s.setHostnameAsFQDN) && !has(%[1]s.runtimeClassName) && (!has(%[1]s.readinessGates) || %[1]s.readinessGates.size() == 0) && (!has(%[1]s.resourceClaims) || %[1]s.resourceClaims.size() == 0) && (!has(%[1]s.schedulingGates) || %[1]s.schedulingGates.size() == 0)`, pod),
		fmt.Sprintf(`has(%[1]s.securityContext) && has(%[1]s.securityContext.runAsNonRoot) && %[1]s.securityContext.runAsNonRoot && has(%[1]s.securityContext.runAsUser) && %[1]s.securityContext.runAsUser == 65532 && has(%[1]s.securityContext.runAsGroup) && %[1]s.securityContext.runAsGroup == 65532 && !has(%[1]s.securityContext.fsGroup) && has(%[1]s.securityContext.seccompProfile) && %[1]s.securityContext.seccompProfile.type == "RuntimeDefault" && (!has(%[1]s.securityContext.sysctls) || %[1]s.securityContext.sysctls.size() == 0)`, pod),
		fmt.Sprintf(`has(%[1]s.containers) && %[1]s.containers.size() == 1 && (!has(%[1]s.initContainers) || %[1]s.initContainers.size() == 0) && (!has(%[1]s.ephemeralContainers) || %[1]s.ephemeralContainers.size() == 0)`, pod),
		fmt.Sprintf(`%[1]s.name == (%[2]s)`, container, containerName),
		image,
		fmt.Sprintf(`request.name != %q || %s`, quiesceJob, g.rollout.hookArgsValidationExpression(container, "teardown-quiesce")),
		fmt.Sprintf(`request.name != %q || %s`, cleanupJob, g.rollout.hookArgsValidationExpression(container, "teardown")),
		fmt.Sprintf(`request.name != %q || %s.args == %s`, finalJob, container, celStringList(g.hookArgsWithTimeout("teardown-retirement-final", "240s"))),
		probeArgs,
		gateArgs,
		hookContainerNoExecutionSideChannelsExpression(container),
		fmt.Sprintf(`has(%[1]s.securityContext) && has(%[1]s.securityContext.allowPrivilegeEscalation) && !%[1]s.securityContext.allowPrivilegeEscalation && has(%[1]s.securityContext.readOnlyRootFilesystem) && %[1]s.securityContext.readOnlyRootFilesystem && (!has(%[1]s.securityContext.privileged) || !%[1]s.securityContext.privileged) && !has(%[1]s.securityContext.runAsUser) && !has(%[1]s.securityContext.runAsGroup) && !has(%[1]s.securityContext.runAsNonRoot) && !has(%[1]s.securityContext.procMount) && !has(%[1]s.securityContext.seLinuxOptions) && !has(%[1]s.securityContext.windowsOptions) && !has(%[1]s.securityContext.seccompProfile) && !has(%[1]s.securityContext.appArmorProfile) && has(%[1]s.securityContext.capabilities) && (!has(%[1]s.securityContext.capabilities.add) || %[1]s.securityContext.capabilities.add.size() == 0) && has(%[1]s.securityContext.capabilities.drop) && %[1]s.securityContext.capabilities.drop == ["ALL"]`, container),
		fmt.Sprintf(`has(dyn(%[1]s.resources).requests) && dyn(%[1]s.resources).requests.size() == 2 && quantity(string(dyn(%[1]s.resources).requests["cpu"])).compareTo(quantity("5m")) == 0 && quantity(string(dyn(%[1]s.resources).requests["memory"])).compareTo(quantity("16Mi")) == 0 && has(dyn(%[1]s.resources).limits) && dyn(%[1]s.resources).limits.size() == 1 && quantity(string(dyn(%[1]s.resources).limits["memory"])).compareTo(quantity("32Mi")) == 0 && (!has(dyn(%[1]s.resources).claims) || dyn(%[1]s.resources).claims.size() == 0) && (!has(%[1]s.resizePolicy) || %[1]s.resizePolicy.size() == 0)`, container),
		fmt.Sprintf(`has(%[1]s.volumeMounts) && %[1]s.volumeMounts.size() == 1 && %[1]s.volumeMounts[0].name == "api-access" && %[1]s.volumeMounts[0].mountPath == "/var/run/secrets/kubernetes.io/serviceaccount" && has(%[1]s.volumeMounts[0].readOnly) && %[1]s.volumeMounts[0].readOnly && !has(%[1]s.volumeMounts[0].mountPropagation) && !has(%[1]s.volumeMounts[0].subPath) && !has(%[1]s.volumeMounts[0].subPathExpr) && !has(%[1]s.volumeMounts[0].recursiveReadOnly)`, container),
		fmt.Sprintf(`has(%[1]s.volumes) && %[1]s.volumes.size() == 1 && %[2]s.name == "api-access" && has(%[2]s.projected) && has(%[2]s.projected.defaultMode) && %[2]s.projected.defaultMode == 420 && %[3]s.size() == 3`, pod, volume, sources),
		fmt.Sprintf(`%s.exists(s, has(s.serviceAccountToken) && s.serviceAccountToken.path == "token" && has(s.serviceAccountToken.expirationSeconds) && s.serviceAccountToken.expirationSeconds == 3600 && !has(s.serviceAccountToken.audience))`, sources),
		fmt.Sprintf(`%s.exists(s, has(s.configMap) && s.configMap.name == "kube-root-ca.crt" && has(s.configMap.items) && s.configMap.items.size() == 1 && s.configMap.items[0].key == "ca.crt" && s.configMap.items[0].path == "ca.crt" && !has(s.configMap.items[0].mode))`, sources),
		fmt.Sprintf(`%s.exists(s, has(s.downwardAPI) && has(s.downwardAPI.items) && s.downwardAPI.items.size() == 1 && s.downwardAPI.items[0].path == "namespace" && has(s.downwardAPI.items[0].fieldRef) && s.downwardAPI.items[0].fieldRef.apiVersion == "v1" && s.downwardAPI.items[0].fieldRef.fieldPath == "metadata.namespace" && !has(s.downwardAPI.items[0].mode))`, sources),
		fmt.Sprintf(`%s.all(s, has(s.serviceAccountToken) || has(s.configMap) || has(s.downwardAPI))`, sources),
		fmt.Sprintf(`(!has(%[1]s.imagePullSecrets) || (%[1]s.imagePullSecrets.all(secret, secret.name != "") && %[1]s.imagePullSecrets.all(secret, %[1]s.imagePullSecrets.filter(other, other.name == secret.name).size() == 1)))`, pod),
		fmt.Sprintf(`(!has(%[1]s.dnsPolicy) || %[1]s.dnsPolicy == "ClusterFirst") && !has(%[1]s.dnsConfig) && (!has(%[1]s.schedulerName) || %[1]s.schedulerName == "default-scheduler") && !has(%[1]s.priority) && !has(%[1]s.priorityClassName) && !has(%[1]s.preemptionPolicy)`, pod),
		fmt.Sprintf(`(!has(%[1]s.terminationGracePeriodSeconds) || %[1]s.terminationGracePeriodSeconds == 30) && (!has(%[1]s.enableServiceLinks) || %[1]s.enableServiceLinks) && (!has(%[1]s.topologySpreadConstraints) || %[1]s.topologySpreadConstraints.size() == 0) && !has(dyn(%[1]s).overhead) && !has(%[1]s.os)`, pod),
	}
}

func (g *TeardownRetirementGuard) teardownJobStatusValidationExpressions(forwardCompatible bool) []string {
	contract := g.teardownJobValidationExpressions(forwardCompatible)
	expressions := make([]string, 0, len(contract)+2)
	expressions = append(expressions,
		teardownRetirementJobControllerPrincipalExpression(),
		teardownRetirementStatusPreservesIdentityExpression(),
	)
	for _, expression := range contract[1:] {
		expressions = append(expressions, strings.ReplaceAll(expression, "object.", "oldObject."))
	}
	return expressions
}

func (g *TeardownRetirementGuard) teardownJobDeletionValidationExpressions(forwardCompatible bool) []string {
	contract := g.teardownJobValidationExpressions(forwardCompatible)
	jobNames := celStringList([]string{g.quiesceJobName(), g.cleanupJobName(), g.finalJobName(), g.probeAJobName(), g.gateJobName()})
	expressions := []string{
		`oldObject != null && (!has(request.subResource) || request.subResource == "") && request.operation == "DELETE"`,
		fmt.Sprintf(`oldObject.metadata.name in %s && oldObject.metadata.namespace == %q && (!has(oldObject.metadata.generateName) || oldObject.metadata.generateName == "")`, jobNames, g.rollout.ReleaseNamespace),
	}
	for _, expression := range contract[2:] {
		expressions = append(expressions, strings.ReplaceAll(expression, "object.", "oldObject."))
	}
	expressions = append(expressions, `has(oldObject.status.conditions) && oldObject.status.conditions.exists(condition, condition.status == "True" && condition.type in ["Complete", "Failed"])`)
	return expressions
}

func (g *TeardownRetirementGuard) hookArgsWithTimeout(mode, timeout string) []string {
	args := g.rollout.hookArgs(mode)
	if len(args) > 1 {
		args[1] = "--timeout=" + timeout
	}
	return args
}

func (g *TeardownRetirementGuard) forwardBootstrapArgsValidationExpression(container, jobSelector string) string {
	args := container + ".args"
	mode := fmt.Sprintf(`%s == %q ? "teardown-retirement-probe-a" : "teardown-retirement-gate"`, jobSelector, g.probeAJobName())
	timeout := fmt.Sprintf(`%s == %q ? "--timeout=60s" : "--timeout=90s"`, jobSelector, g.probeAJobName())
	parts := []string{
		fmt.Sprintf(`has(%[1]s.args) && %[2]s.size() == 29`, container, args),
		fmt.Sprintf(`%s[0] == (%s)`, args, mode),
		fmt.Sprintf(`%s[1] == (%s)`, args, timeout),
		fmt.Sprintf(`%s[2] == %q`, args, "--release-name="+g.rollout.ReleaseName),
		fmt.Sprintf(`%s[3] == %q`, args, "--release-namespace="+g.rollout.ReleaseNamespace),
		fmt.Sprintf(`%s[4].matches("^--coordination-namespace=[a-z0-9]([-a-z0-9]*[a-z0-9])?$")`, args),
		fmt.Sprintf(`%s[5].matches("^--leader-election=(true|false)$")`, args),
		fmt.Sprintf(`%s[6].matches("^--leader-election-id=[a-z0-9]([-a-z0-9.]*[a-z0-9])?$")`, args),
		fmt.Sprintf(`%s[7].matches("^--webhook-service-name=[a-z0-9]([-a-z0-9]*[a-z0-9])?$")`, args),
		fmt.Sprintf(`%s[8].matches("^--webhook-timeout-seconds=[1-9][0-9]*$")`, args),
		fmt.Sprintf(`%s[9].matches("^--webhook-secret-name=[a-z0-9]([-a-z0-9.]*[a-z0-9])?$")`, args),
		fmt.Sprintf(`%s[10].matches("^--webhook-port=[1-9][0-9]*$")`, args),
		fmt.Sprintf(`%s[11].matches("^--certificate-health-port=[1-9][0-9]*$")`, args),
		fmt.Sprintf(`%s[12].matches("^--hook-service-account-name=[a-z0-9]([-a-z0-9]*[a-z0-9])?-crd-v[1-9][0-9]*-[0-9a-f]{12}$")`, args),
		fmt.Sprintf(`%s[13].matches("^--controller-service-account-name=[a-z0-9]([-a-z0-9]*[a-z0-9])?$")`, args),
		fmt.Sprintf(`%s[14].matches("^--controller-service-account-managed=(true|false)$")`, args),
		fmt.Sprintf(`%s[15].startsWith("--previous-controller-service-account-name=")`, args),
		fmt.Sprintf(`%s[16].startsWith("--previous-controller-service-account-uid=")`, args),
		fmt.Sprintf(`%s[17].matches("^--previous-controller-service-account-managed=(true|false)$")`, args),
		fmt.Sprintf(`%s[18].matches("^--previous-controller-release-sequence=(0|[1-9][0-9]*)$")`, args),
		fmt.Sprintf(`%s[19].matches("^--controller-deployment-name=[a-z0-9]([-a-z0-9]*[a-z0-9])?$")`, args),
		fmt.Sprintf(`%s[20].matches("^--controller-replicas=[1-9][0-9]*$")`, args),
		fmt.Sprintf(`%s[21].matches("^--certificate-deployment-name=[a-z0-9]([-a-z0-9]*[a-z0-9])?$")`, args),
		fmt.Sprintf(`%s[22].matches("^--release-sequence=[1-9][0-9]*$")`, args),
		fmt.Sprintf(`%s[23] == "--manager-image=" + %s.image`, args, container),
		fmt.Sprintf(`%s[24].matches("^--controller-runtime-args-b64=[A-Za-z0-9+/]+={0,2}$")`, args),
		fmt.Sprintf(`%s[25].matches("^--certificate-runtime-args-b64=[A-Za-z0-9+/]+={0,2}$")`, args),
		fmt.Sprintf(`%s[26].matches("^--runtime-deployment-config-expressions-b64=[A-Za-z0-9+/]+={0,2}$")`, args),
		fmt.Sprintf(`%s[27].matches("^--runtime-pod-config-expressions-b64=[A-Za-z0-9+/]+={0,2}$")`, args),
		fmt.Sprintf(`%s[28].matches("^--runtime-admission-contract-b64=[A-Za-z0-9+/]+={0,2}$")`, args),
	}
	return strings.Join(parts, " && ")
}

func (g *TeardownRetirementGuard) teardownPodValidationExpressions(object string, forwardCompatible bool) []string {
	pod := object + ".spec"
	metadata := object + ".metadata"
	container := pod + ".containers[0]"
	volume := pod + ".volumes[0]"
	sources := volume + ".projected.sources"
	jobLabel := metadata + `.labels["batch.kubernetes.io/job-name"]`
	owner := metadata + ".ownerReferences[0]"
	quiesceJob := g.quiesceJobName()
	cleanupJob := g.cleanupJobName()
	finalJob := g.finalJobName()
	probeAJob := g.probeAJobName()
	gateJob := g.gateJobName()
	cleanupServiceAccount := g.cleanupServiceAccountName()
	bootstrapServiceAccount := g.bootstrapServiceAccountName()
	allJobs := celStringList([]string{quiesceJob, cleanupJob, finalJob, probeAJob, gateJob})
	bootstrapJobs := celStringList([]string{probeAJob, gateJob})
	serviceAccount := fmt.Sprintf(`%s in %s ? %q : (%s == %q ? %q : %q)`, jobLabel, bootstrapJobs, bootstrapServiceAccount, jobLabel, quiesceJob, g.rollout.HookServiceAccountName, cleanupServiceAccount)
	containerName := fmt.Sprintf(`%s == %q ? "teardown-retirement-probe-a" : (%s == %q ? "teardown-retirement-gate" : (%s == %q ? "crd-manager-teardown-quiesce" : (%s == %q ? "crd-manager-teardown" : "teardown-retirement")))`, jobLabel, probeAJob, jobLabel, gateJob, jobLabel, quiesceJob, jobLabel, cleanupJob)
	image := fmt.Sprintf(`%[1]s.name == (%[2]s) && %[1]s.image == %[3]q && %[1]s.command == ["/ptah-crd-manager"]`, container, containerName, g.rollout.ManagerImage)
	probeArgs := fmt.Sprintf(`%[1]s != %[2]q || %[3]s.args == %s`, jobLabel, probeAJob, container, celStringList(g.hookArgsWithTimeout("teardown-retirement-probe-a", "60s")))
	gateArgs := fmt.Sprintf(`%[1]s != %[2]q || %[3]s.args == %s`, jobLabel, gateJob, container, celStringList(g.hookArgsWithTimeout("teardown-retirement-gate", "90s")))
	if forwardCompatible {
		image = fmt.Sprintf(`%[1]s.name == (%[2]s) && %[1]s.image.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && %[1]s.command == ["/ptah-crd-manager"]`, container, containerName)
		probeArgs = g.forwardBootstrapArgsValidationExpression(container, jobLabel)
		gateArgs = `true`
	}
	return []string{
		`!has(request.subResource) || request.subResource == ""`,
		`request.operation != "CREATE" || (` + teardownRetirementJobControllerPrincipalExpression() + `)`,
		fmt.Sprintf(`has(%[1]s.serviceAccountName) && %[1]s.serviceAccountName == (%[2]s)`, pod, serviceAccount),
		fmt.Sprintf(`has(%[1]s.labels) && "batch.kubernetes.io/job-name" in %[1]s.labels && %[2]s in %s`, metadata, jobLabel, allJobs),
		fmt.Sprintf(`has(%[1]s.ownerReferences) && %[1]s.ownerReferences.size() == 1 && %[2]s.apiVersion == "batch/v1" && %[2]s.kind == "Job" && %[2]s.name == %[3]s && has(%[2]s.controller) && %[2]s.controller`, metadata, owner, jobLabel),
		fmt.Sprintf(`has(%[1]s.uid) && %[1]s.uid != "" && has(%[1]s.blockOwnerDeletion) && %[1]s.blockOwnerDeletion && %[2]s`, owner, generatedPodNameValidationExpression(owner+".name")),
		fmt.Sprintf(`%s.restartPolicy == "Never"`, pod),
		fmt.Sprintf(`request.operation != "CREATE" || !has(%[1]s.nodeName) || %[1]s.nodeName == ""`, pod),
		fmt.Sprintf(`request.operation != "UPDATE" || ((!has(%[1]s.nodeName) && !has(oldObject.spec.nodeName)) || (has(%[1]s.nodeName) && has(oldObject.spec.nodeName) && %[1]s.nodeName == oldObject.spec.nodeName))`, pod),
		fmt.Sprintf(`has(%[1]s.automountServiceAccountToken) && !%[1]s.automountServiceAccountToken && (!has(%[1]s.hostNetwork) || !%[1]s.hostNetwork) && (!has(%[1]s.hostPID) || !%[1]s.hostPID) && (!has(%[1]s.hostIPC) || !%[1]s.hostIPC) && (!has(%[1]s.shareProcessNamespace) || !%[1]s.shareProcessNamespace)`, pod),
		fmt.Sprintf(`has(%[1]s.securityContext) && has(%[1]s.securityContext.runAsNonRoot) && %[1]s.securityContext.runAsNonRoot && has(%[1]s.securityContext.runAsUser) && %[1]s.securityContext.runAsUser == 65532 && has(%[1]s.securityContext.runAsGroup) && %[1]s.securityContext.runAsGroup == 65532 && has(%[1]s.securityContext.seccompProfile) && %[1]s.securityContext.seccompProfile.type == "RuntimeDefault" && (!has(%[1]s.securityContext.sysctls) || %[1]s.securityContext.sysctls.size() == 0)`, pod),
		fmt.Sprintf(`has(%[1]s.containers) && %[1]s.containers.size() == 1 && (!has(%[1]s.initContainers) || %[1]s.initContainers.size() == 0) && (!has(%[1]s.ephemeralContainers) || %[1]s.ephemeralContainers.size() == 0)`, pod),
		image,
		fmt.Sprintf(`%[1]s != %[2]q || %[3]s`, jobLabel, quiesceJob, g.rollout.hookArgsValidationExpression(container, "teardown-quiesce")),
		fmt.Sprintf(`%[1]s != %[2]q || %[3]s`, jobLabel, cleanupJob, g.rollout.hookArgsValidationExpression(container, "teardown")),
		fmt.Sprintf(`%[1]s != %[2]q || %[3]s.args == %s`, jobLabel, finalJob, container, celStringList(g.hookArgsWithTimeout("teardown-retirement-final", "240s"))),
		probeArgs,
		gateArgs,
		hookContainerNoExecutionSideChannelsExpression(container),
		fmt.Sprintf(`has(%[1]s.securityContext) && has(%[1]s.securityContext.allowPrivilegeEscalation) && !%[1]s.securityContext.allowPrivilegeEscalation && has(%[1]s.securityContext.readOnlyRootFilesystem) && %[1]s.securityContext.readOnlyRootFilesystem && (!has(%[1]s.securityContext.privileged) || !%[1]s.securityContext.privileged) && !has(%[1]s.securityContext.runAsUser) && !has(%[1]s.securityContext.runAsGroup) && !has(%[1]s.securityContext.runAsNonRoot) && !has(%[1]s.securityContext.procMount) && has(%[1]s.securityContext.capabilities) && (!has(%[1]s.securityContext.capabilities.add) || %[1]s.securityContext.capabilities.add.size() == 0) && has(%[1]s.securityContext.capabilities.drop) && %[1]s.securityContext.capabilities.drop == ["ALL"]`, container),
		fmt.Sprintf(`has(%[1]s.volumeMounts) && %[1]s.volumeMounts.size() == 1 && %[1]s.volumeMounts[0].name == "api-access" && %[1]s.volumeMounts[0].mountPath == "/var/run/secrets/kubernetes.io/serviceaccount" && has(%[1]s.volumeMounts[0].readOnly) && %[1]s.volumeMounts[0].readOnly`, container),
		fmt.Sprintf(`has(%[1]s.volumes) && %[1]s.volumes.size() == 1 && %[2]s.name == "api-access" && has(%[2]s.projected) && has(%[2]s.projected.defaultMode) && %[2]s.projected.defaultMode == 420 && %[3]s.size() == 3 && %[3]s.exists(s, has(s.serviceAccountToken) && s.serviceAccountToken.path == "token" && has(s.serviceAccountToken.expirationSeconds) && s.serviceAccountToken.expirationSeconds == 3600 && !has(s.serviceAccountToken.audience)) && %[3]s.exists(s, has(s.configMap) && s.configMap.name == "kube-root-ca.crt") && %[3]s.exists(s, has(s.downwardAPI))`, pod, volume, sources),
	}
}

func (g *TeardownRetirementGuard) teardownPodStatusValidationExpressions(forwardCompatible bool) []string {
	contract := g.teardownPodValidationExpressions("object", forwardCompatible)
	expressions := make([]string, 0, len(contract)+2)
	expressions = append(expressions,
		fmt.Sprintf(`(%s) || ((%s) && (%s))`, teardownRetirementNodePrincipalExpression(), teardownRetirementSchedulerPrincipalExpression(), teardownRetirementSchedulerStatusDeltaExpression()),
		teardownRetirementStatusPreservesIdentityExpression(),
	)
	for _, expression := range contract[1:] {
		expressions = append(expressions, strings.ReplaceAll(expression, "object.", "oldObject."))
	}
	return expressions
}

func teardownRetirementPodStatusFields() []string {
	return []string{
		"observedGeneration",
		"phase",
		"conditions",
		"message",
		"reason",
		"nominatedNodeName",
		"hostIP",
		"hostIPs",
		"podIP",
		"podIPs",
		"startTime",
		"initContainerStatuses",
		"containerStatuses",
		"qosClass",
		"ephemeralContainerStatuses",
		"resize",
		"resourceClaimStatuses",
		"extendedResourceClaimStatus",
		"allocatedResources",
		"resources",
		"nodeAllocatableResourceClaimStatuses",
	}
}

func teardownRetirementSchedulerStatusDeltaExpression() string {
	parts := make([]string, 0, len(teardownRetirementPodStatusFields())-1)
	for _, field := range teardownRetirementPodStatusFields() {
		if field == "nominatedNodeName" {
			continue
		}
		parts = append(parts, fmt.Sprintf(
			`((!has(dyn(object.status).%[1]s) && !has(dyn(oldObject.status).%[1]s)) || (has(dyn(object.status).%[1]s) && has(dyn(oldObject.status).%[1]s) && dyn(object.status).%[1]s == dyn(oldObject.status).%[1]s))`,
			field,
		))
	}
	return strings.Join(parts, " && ")
}

func teardownRetirementStatusPreservesIdentityExpression() string {
	return `oldObject != null && object.spec == oldObject.spec && object.metadata.name == oldObject.metadata.name && object.metadata.namespace == oldObject.metadata.namespace && ((!has(object.metadata.generateName) && !has(oldObject.metadata.generateName)) || (has(object.metadata.generateName) && has(oldObject.metadata.generateName) && object.metadata.generateName == oldObject.metadata.generateName)) && has(object.metadata.uid) && has(oldObject.metadata.uid) && object.metadata.uid != "" && object.metadata.uid == oldObject.metadata.uid && has(object.metadata.resourceVersion) && has(oldObject.metadata.resourceVersion) && object.metadata.resourceVersion != "" && object.metadata.resourceVersion == oldObject.metadata.resourceVersion && object.metadata.generation == oldObject.metadata.generation && has(object.metadata.labels) && has(oldObject.metadata.labels) && object.metadata.labels == oldObject.metadata.labels && ((!has(object.metadata.annotations) && !has(oldObject.metadata.annotations)) || (has(object.metadata.annotations) && has(oldObject.metadata.annotations) && object.metadata.annotations == oldObject.metadata.annotations)) && ((!has(object.metadata.ownerReferences) && !has(oldObject.metadata.ownerReferences)) || (has(object.metadata.ownerReferences) && has(oldObject.metadata.ownerReferences) && object.metadata.ownerReferences == oldObject.metadata.ownerReferences)) && ((!has(object.metadata.finalizers) && !has(oldObject.metadata.finalizers)) || (has(object.metadata.finalizers) && has(oldObject.metadata.finalizers) && object.metadata.finalizers == oldObject.metadata.finalizers))`
}

func (g *TeardownRetirementGuard) teardownPodDeletionIdentityExpressions() []string {
	jobLabel := `oldObject.metadata.labels["batch.kubernetes.io/job-name"]`
	owner := "oldObject.metadata.ownerReferences[0]"
	bootstrapJobs := celStringList([]string{g.probeAJobName(), g.gateJobName()})
	allJobs := celStringList([]string{g.quiesceJobName(), g.cleanupJobName(), g.finalJobName(), g.probeAJobName(), g.gateJobName()})
	serviceAccount := fmt.Sprintf(`%s in %s ? %q : (%s == %q ? %q : %q)`, jobLabel, bootstrapJobs, g.bootstrapServiceAccountName(), jobLabel, g.quiesceJobName(), g.rollout.HookServiceAccountName, g.cleanupServiceAccountName())
	return []string{
		`oldObject != null && (!has(request.subResource) || request.subResource == "")`,
		fmt.Sprintf(`has(oldObject.metadata.labels) && "batch.kubernetes.io/job-name" in oldObject.metadata.labels && %s in %s`, jobLabel, allJobs),
		fmt.Sprintf(`has(oldObject.metadata.ownerReferences) && oldObject.metadata.ownerReferences.size() == 1 && %[1]s.apiVersion == "batch/v1" && %[1]s.kind == "Job" && %[1]s.name == %[2]s && has(%[1]s.uid) && %[1]s.uid != "" && has(%[1]s.controller) && %[1]s.controller && has(%[1]s.blockOwnerDeletion) && %[1]s.blockOwnerDeletion`, owner, jobLabel),
		fmt.Sprintf(`has(oldObject.spec.serviceAccountName) && oldObject.spec.serviceAccountName == (%s)`, serviceAccount),
	}
}

// DormantFencePair builds the ordinary-manifest A/B bootstrap boundary. It
// protects the stable bootstrap credential and proof Job/Pod identities but
// deliberately does not match runtime callers or the marker probe. Pre-delete
// hooks replace the same stable identity with the broad static form, and
// Helm's normal manifest deletion later removes that form.
func (g *TeardownRetirementGuard) DormantFencePair(fence TeardownFence) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding, TeardownRetirementProbe, error) {
	if err := g.validate(); err != nil {
		return nil, nil, TeardownRetirementProbe{}, err
	}
	if fence != TeardownFenceA && fence != TeardownFenceB {
		return nil, nil, TeardownRetirementProbe{}, fmt.Errorf("unknown teardown fence %q", fence)
	}
	name := g.fenceName(fence)
	policy, err := g.dormantFencePolicy(name)
	if err != nil {
		return nil, nil, TeardownRetirementProbe{}, err
	}
	binding := g.exactBindingForHook(name, "", "", name, "")
	return policy, binding, g.probe(name), nil
}

// VerifyOriginalFences verifies each early static fence exactly. The
// caller uses this both before the first probe and on every joint convergence
// sweep so a stored-object replacement resets or fails the gate.
func (g *TeardownRetirementGuard) VerifyOriginalFences(
	ctx context.Context,
	policies ValidatingAdmissionPolicyReader,
	bindings ValidatingAdmissionPolicyBindingReader,
	fences ...TeardownFence,
) error {
	if err := g.validate(); err != nil {
		return err
	}
	if policies == nil || bindings == nil {
		return errors.New("teardown retirement fence readers are required")
	}
	if len(fences) == 0 {
		return errors.New("teardown retirement fence inventory is empty")
	}
	seen := make(map[TeardownFence]struct{}, len(fences))
	for _, fence := range fences {
		if _, duplicate := seen[fence]; duplicate {
			return fmt.Errorf("teardown retirement fence %q is duplicated", fence)
		}
		seen[fence] = struct{}{}
		expectedPolicy, expectedBinding, _, err := g.OriginalFencePair(fence)
		if err != nil {
			return err
		}
		actualPolicy, err := policies.Get(ctx, expectedPolicy.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get teardown retirement fence policy %s: %w", expectedPolicy.Name, err)
		}
		if !exactTeardownRetirementMetadata(actualPolicy.ObjectMeta, expectedPolicy.ObjectMeta) || !reflect.DeepEqual(actualPolicy.Spec, expectedPolicy.Spec) {
			return fmt.Errorf("teardown retirement fence policy %s differs from the exact original contract", expectedPolicy.Name)
		}
		actualBinding, err := bindings.Get(ctx, expectedBinding.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get teardown retirement fence binding %s: %w", expectedBinding.Name, err)
		}
		if !exactTeardownRetirementMetadata(actualBinding.ObjectMeta, expectedBinding.ObjectMeta) || !reflect.DeepEqual(actualBinding.Spec, expectedBinding.Spec) {
			return fmt.Errorf("teardown retirement fence binding %s differs from the exact original contract", expectedBinding.Name)
		}
	}
	return nil
}

// ClassifyPair classifies every hook-local state that can be observed while
// Helm replaces one policy and binding in that order. Global preflight still
// has to prove that the classified state belongs to one monotonic inventory
// frontier; recognizing a component in isolation is not authorization to
// resume an uninstall.
func (g *TeardownRetirementGuard) ClassifyPair(
	original TeardownOriginalPairVerifier,
	retirementPolicy *admissionregistrationv1.ValidatingAdmissionPolicy,
	retirementBinding *admissionregistrationv1.ValidatingAdmissionPolicyBinding,
	actualPolicy *admissionregistrationv1.ValidatingAdmissionPolicy,
	actualBinding *admissionregistrationv1.ValidatingAdmissionPolicyBinding,
) (TeardownPairForm, error) {
	if err := g.validate(); err != nil {
		return "", err
	}
	if original.Name == "" || original.VerifyPolicy == nil || original.VerifyBinding == nil || retirementPolicy == nil || retirementBinding == nil {
		return "", errors.New("teardown policy pair verifier is incomplete")
	}
	state, err := classifyTeardownRetirementPairState(TeardownRetirementPair{
		Original: original,
		Policy:   retirementPolicy,
		Binding:  retirementBinding,
	}, actualPolicy, actualBinding)
	if err != nil {
		return "", err
	}
	return state.form()
}

func classifyTeardownRetirementPairState(
	pair TeardownRetirementPair,
	actualPolicy *admissionregistrationv1.ValidatingAdmissionPolicy,
	actualBinding *admissionregistrationv1.ValidatingAdmissionPolicyBinding,
) (teardownRetirementPairState, error) {
	policy, err := classifyTeardownRetirementPolicy(pair, actualPolicy)
	if err != nil {
		return teardownRetirementPairState{}, err
	}
	binding, err := classifyTeardownRetirementBinding(pair, actualBinding)
	if err != nil {
		return teardownRetirementPairState{}, err
	}
	return teardownRetirementPairState{pair: pair, policy: policy, binding: binding}, nil
}

func classifyTeardownRetirementPolicy(pair TeardownRetirementPair, actual *admissionregistrationv1.ValidatingAdmissionPolicy) (teardownRetirementObjectForm, error) {
	if actual == nil {
		return teardownRetirementObjectAbsent, nil
	}
	matches := make([]teardownRetirementObjectForm, 0, 2)
	if pair.Original.VerifyPolicy(actual) == nil {
		matches = append(matches, teardownRetirementObjectOriginal)
	}
	if exactTeardownRetirementMetadata(actual.ObjectMeta, pair.Policy.ObjectMeta) && reflect.DeepEqual(actual.Spec, pair.Policy.Spec) {
		matches = append(matches, teardownRetirementObjectRetired)
	}
	if len(matches) != 1 {
		return teardownRetirementObjectForeign, fmt.Errorf("teardown policy %s is foreign or ambiguously matches %d exact contracts", pair.Original.Name, len(matches))
	}
	return matches[0], nil
}

func classifyTeardownRetirementBinding(pair TeardownRetirementPair, actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) (teardownRetirementObjectForm, error) {
	if actual == nil {
		return teardownRetirementObjectAbsent, nil
	}
	matches := make([]teardownRetirementObjectForm, 0, 2)
	if pair.Original.VerifyBinding(actual) == nil {
		matches = append(matches, teardownRetirementObjectOriginal)
	}
	if exactTeardownRetirementMetadata(actual.ObjectMeta, pair.Binding.ObjectMeta) && reflect.DeepEqual(actual.Spec, pair.Binding.Spec) {
		matches = append(matches, teardownRetirementObjectRetired)
	}
	if len(matches) != 1 {
		return teardownRetirementObjectForeign, fmt.Errorf("teardown binding %s is foreign or ambiguously matches %d exact contracts", pair.Original.Name, len(matches))
	}
	return matches[0], nil
}

func (s teardownRetirementPairState) form() (TeardownPairForm, error) {
	if s.pair.Original.OptionalGroup == legacyParentWorkloadOriginTeardownGroup &&
		((s.policy == teardownRetirementObjectOriginal && s.binding == teardownRetirementObjectAbsent) ||
			(s.policy == teardownRetirementObjectOriginal && s.binding == teardownRetirementObjectRetired)) {
		return TeardownPairLegacyRecovery, nil
	}
	switch {
	case s.policy == teardownRetirementObjectOriginal && s.binding == teardownRetirementObjectOriginal:
		return TeardownPairOriginal, nil
	case s.policy == teardownRetirementObjectAbsent && s.binding == teardownRetirementObjectOriginal:
		return TeardownPairReplacingPolicy, nil
	case s.policy == teardownRetirementObjectRetired && s.binding == teardownRetirementObjectOriginal:
		return TeardownPairPolicyReplaced, nil
	case s.policy == teardownRetirementObjectRetired && s.binding == teardownRetirementObjectAbsent:
		return TeardownPairReplacingBinding, nil
	case s.policy == teardownRetirementObjectAbsent && s.binding == teardownRetirementObjectRetired:
		return TeardownPairReplayingPolicy, nil
	case s.policy == teardownRetirementObjectRetired && s.binding == teardownRetirementObjectRetired:
		return TeardownPairRetirement, nil
	case s.policy == teardownRetirementObjectAbsent && s.binding == teardownRetirementObjectAbsent:
		return TeardownPairAbsent, nil
	default:
		return "", fmt.Errorf("teardown policy pair %s has an impossible component ordering", s.pair.Original.Name)
	}
}

// RetirementPairs returns the deduplicated, deterministic replacement
// inventory compiled into ReleaseTeardown. Helm must render every returned
// pair unconditionally; lookup-based discovery is not an authorization input.
func (g *TeardownRetirementGuard) RetirementPairs() ([]TeardownRetirementPair, error) {
	if err := g.validate(); err != nil {
		return nil, err
	}
	contracts, err := teardownGuardContracts(g.rollout)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]TeardownOriginalPairVerifier, len(contracts))
	for _, contract := range contracts {
		if _, exists := byName[contract.name]; exists {
			continue
		}
		byName[contract.name] = TeardownOriginalPairVerifier{
			Name:          contract.name,
			OptionalGroup: contract.optionalGroup,
			VerifyPolicy:  contract.verifyPolicy,
			VerifyBinding: contract.verifyBinding,
		}
	}
	// The retained v1 parent-origin guards are a bounded optional predecessor
	// generation. Fresh installs never create them, but an upgrade from v1 must
	// prove both exact objects before uninstall may replace either one. Keeping
	// the pair in one optional group rejects sparse or partially foreign legacy
	// state without discovering deletion targets from cluster labels.
	parentGuard := NewParentWorkloadGuard(g.rollout)
	retirementEntries := parentGuard.legacyOriginRetirementEntries()
	retirementByName := make(map[string]parentGuardEntry, len(retirementEntries))
	for _, entry := range retirementEntries {
		retirementByName[entry.name] = entry
	}
	for _, entry := range parentGuard.legacyOriginEntries() {
		if _, exists := byName[entry.name]; exists {
			return nil, fmt.Errorf("teardown retirement legacy pair %s collides with the current inventory", entry.name)
		}
		retirement, found := retirementByName[entry.name]
		if !found {
			return nil, fmt.Errorf("teardown retirement legacy pair %s has no exact post-upgrade retirement contract", entry.name)
		}
		verifyOriginalPolicy := entry.verifyPolicy
		verifyOriginalBinding := entry.verifyBinding
		byName[entry.name] = TeardownOriginalPairVerifier{
			Name:          entry.name,
			OptionalGroup: legacyParentWorkloadOriginTeardownGroup,
			VerifyPolicy: func(actual *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				if verifyOriginalPolicy(actual) == nil {
					return nil
				}
				if actual != nil && exactParentGuardObjectMetadata(actual.ObjectMeta, retirement.policy.ObjectMeta) && reflect.DeepEqual(actual.Spec, retirement.policy.Spec) {
					return nil
				}
				return fmt.Errorf("legacy parent-origin policy %s differs from the original and post-upgrade retirement contracts", entry.name)
			},
			VerifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				if verifyOriginalBinding(actual) == nil {
					return nil
				}
				if actual != nil && exactParentGuardObjectMetadata(actual.ObjectMeta, retirement.binding.ObjectMeta) && reflect.DeepEqual(actual.Spec, retirement.binding.Spec) {
					return nil
				}
				return fmt.Errorf("legacy parent-origin binding %s differs from the original and post-upgrade retirement contracts", entry.name)
			},
		}
	}
	for _, pair := range g.additionalPairs {
		if _, found := byName[pair.Name]; found {
			return nil, fmt.Errorf("teardown retirement pair %s is duplicated", pair.Name)
		}
		byName[pair.Name] = pair
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	slices.Sort(names)
	if teardownRetirementPairFirstWeight+2*len(names)-1 > teardownRetirementPairLastWeight {
		return nil, fmt.Errorf("teardown retirement inventory of %d pairs exceeds hook weights %d..%d", len(names), teardownRetirementPairFirstWeight, teardownRetirementPairLastWeight)
	}
	pairs := make([]TeardownRetirementPair, 0, len(names))
	for index, name := range names {
		policyWeight := teardownRetirementPairFirstWeight + 2*index
		bindingWeight := policyWeight + 1
		policy := g.markerOnlyPolicy(name, strconv.Itoa(policyWeight), name, "before-hook-creation,hook-succeeded")
		binding := g.exactBinding(name, strconv.Itoa(bindingWeight), name, "before-hook-creation,hook-succeeded")
		pairs = append(pairs, TeardownRetirementPair{
			Original: byName[name], Policy: policy, Binding: binding, Probe: g.probe(name),
			PolicyWeight: policyWeight, BindingWeight: bindingWeight,
		})
	}
	return pairs, nil
}

// VerifyRetirementPair verifies one stored marker-only replacement exactly.
func (g *TeardownRetirementGuard) VerifyRetirementPair(pair TeardownRetirementPair, policy *admissionregistrationv1.ValidatingAdmissionPolicy, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
	if policy == nil || binding == nil {
		return fmt.Errorf("teardown retirement pair %s is sparse", pair.Original.Name)
	}
	if !exactTeardownRetirementMetadata(policy.ObjectMeta, pair.Policy.ObjectMeta) || !reflect.DeepEqual(policy.Spec, pair.Policy.Spec) {
		return fmt.Errorf("teardown retirement policy %s differs from the exact contract", pair.Original.Name)
	}
	if !exactTeardownRetirementMetadata(binding.ObjectMeta, pair.Binding.ObjectMeta) || !reflect.DeepEqual(binding.Spec, pair.Binding.Spec) {
		return fmt.Errorf("teardown retirement binding %s differs from the exact contract", pair.Original.Name)
	}
	return nil
}

// PreflightPairs verifies the activation-present replacement state. It is a
// compatibility wrapper around PreflightPairsForPhase.
func (g *TeardownRetirementGuard) PreflightPairs(
	ctx context.Context,
	policies ValidatingAdmissionPolicyReader,
	bindings ValidatingAdmissionPolicyBindingReader,
) (map[string]TeardownPairForm, error) {
	return g.PreflightPairsForPhase(ctx, policies, bindings, TeardownRetirementActive)
}

// PreflightPairsForPhase verifies one exact, crash-reachable state of the
// deterministic policy-then-binding hook sequence. In the active phase there
// must exist a monotonic historical frontier: everything before it contains
// no original component, the frontier is an untouched pair or a policy-side
// transition, and everything after it is in one consistent baseline. Helm
// may delete prior hook-succeeded objects when a later hook fails, so any
// retired/absent combination is valid inside the completed prefix. This is
// closed under arbitrarily many retries and interrupted delete/create calls.
//
// Terminal state is selected only after an exact GET proves activation is
// absent. At that point every component must be either the exact retirement
// form or absent; an original or foreign component can never regain authority
// through this API.
func (g *TeardownRetirementGuard) PreflightPairsForPhase(
	ctx context.Context,
	policies ValidatingAdmissionPolicyReader,
	bindings ValidatingAdmissionPolicyBindingReader,
	phase TeardownRetirementPhase,
) (map[string]TeardownPairForm, error) {
	if policies == nil || bindings == nil {
		return nil, errors.New("teardown retirement pair readers are required")
	}
	if phase != TeardownRetirementActive && phase != TeardownRetirementTerminal {
		return nil, fmt.Errorf("unknown teardown retirement phase %q", phase)
	}
	pairs, err := g.RetirementPairs()
	if err != nil {
		return nil, err
	}
	states := make([]teardownRetirementPairState, 0, len(pairs))
	forms := make(map[string]TeardownPairForm, len(pairs))
	for _, pair := range pairs {
		policy, err := policies.Get(ctx, pair.Original.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			policy = nil
		} else if err != nil {
			return nil, fmt.Errorf("get teardown retirement target policy %s: %w", pair.Original.Name, err)
		} else if policy == nil {
			return nil, fmt.Errorf("get teardown retirement target policy %s returned nil without an error", pair.Original.Name)
		}
		binding, err := bindings.Get(ctx, pair.Original.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			binding = nil
		} else if err != nil {
			return nil, fmt.Errorf("get teardown retirement target binding %s: %w", pair.Original.Name, err)
		} else if binding == nil {
			return nil, fmt.Errorf("get teardown retirement target binding %s returned nil without an error", pair.Original.Name)
		}
		state, err := classifyTeardownRetirementPairState(pair, policy, binding)
		if err != nil {
			return nil, err
		}
		form, err := state.form()
		if err != nil {
			return nil, err
		}
		states = append(states, state)
		forms[pair.Original.Name] = form
	}
	if phase == TeardownRetirementTerminal {
		for _, state := range states {
			if !teardownRetirementTerminalComponent(state.policy) || !teardownRetirementTerminalComponent(state.binding) {
				return nil, fmt.Errorf("terminal teardown policy pair %s retains an original or foreign component", state.pair.Original.Name)
			}
		}
		return forms, nil
	}
	if !hasTeardownRetirementActiveFrontier(states) {
		return nil, errors.New("teardown retirement pair inventory has no crash-reachable monotonic frontier")
	}
	return forms, nil
}

func teardownRetirementTerminalComponent(form teardownRetirementObjectForm) bool {
	return form == teardownRetirementObjectAbsent || form == teardownRetirementObjectRetired
}

const (
	teardownRetirementOriginPresent uint8 = 1 << iota
	teardownRetirementOriginAbsent
	teardownRetirementOriginEither = teardownRetirementOriginPresent | teardownRetirementOriginAbsent
)

func hasTeardownRetirementActiveFrontier(states []teardownRetirementPairState) bool {
	for frontier := 0; frontier <= len(states); frontier++ {
		groupOrigins := make(map[string]uint8)
		valid := true
		for index, state := range states {
			origins := teardownRetirementAllowedOrigins(state, index, frontier)
			if origins == 0 {
				valid = false
				break
			}
			group := state.pair.Original.OptionalGroup
			if group == "" {
				if origins&teardownRetirementOriginPresent == 0 {
					valid = false
					break
				}
				continue
			}
			allowed, found := groupOrigins[group]
			if !found {
				allowed = teardownRetirementOriginEither
			}
			allowed &= origins
			if allowed == 0 {
				valid = false
				break
			}
			groupOrigins[group] = allowed
		}
		if valid {
			return true
		}
	}
	return false
}

func teardownRetirementAllowedOrigins(state teardownRetirementPairState, index, frontier int) uint8 {
	if state.pair.Original.OptionalGroup == legacyParentWorkloadOriginTeardownGroup {
		return teardownRetirementOriginEither
	}
	if index < frontier {
		if state.policy != teardownRetirementObjectOriginal && state.binding != teardownRetirementObjectOriginal {
			return teardownRetirementOriginEither
		}
		return 0
	}
	if index == frontier {
		switch {
		case state.policy == teardownRetirementObjectOriginal && state.binding == teardownRetirementObjectOriginal:
			return teardownRetirementOriginPresent
		case state.policy == teardownRetirementObjectAbsent && state.binding == teardownRetirementObjectOriginal:
			return teardownRetirementOriginPresent
		case state.policy == teardownRetirementObjectRetired && state.binding == teardownRetirementObjectOriginal:
			return teardownRetirementOriginPresent
		case state.policy == teardownRetirementObjectAbsent && state.binding == teardownRetirementObjectAbsent:
			return teardownRetirementOriginAbsent
		default:
			return 0
		}
	}
	switch {
	case state.policy == teardownRetirementObjectOriginal && state.binding == teardownRetirementObjectOriginal:
		return teardownRetirementOriginPresent
	case state.policy == teardownRetirementObjectAbsent && state.binding == teardownRetirementObjectAbsent:
		return teardownRetirementOriginAbsent
	default:
		return 0
	}
}

// VerifyRetiredPairs verifies that every stored target has reached the exact,
// unparameterized marker-only form used by the final endpoint proof.
func (g *TeardownRetirementGuard) VerifyRetiredPairs(
	ctx context.Context,
	policies ValidatingAdmissionPolicyReader,
	bindings ValidatingAdmissionPolicyBindingReader,
) error {
	if policies == nil || bindings == nil {
		return errors.New("teardown retirement pair readers are required")
	}
	pairs, err := g.RetirementPairs()
	if err != nil {
		return err
	}
	for _, pair := range pairs {
		policy, err := policies.Get(ctx, pair.Original.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get retired teardown policy %s: %w", pair.Original.Name, err)
		}
		binding, err := bindings.Get(ctx, pair.Original.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get retired teardown binding %s: %w", pair.Original.Name, err)
		}
		if err := g.VerifyRetirementPair(pair, policy, binding); err != nil {
			return err
		}
	}
	return nil
}

var teardownRetirementProbeFieldManagerPattern = regexp.MustCompile(`^ptah-teardown-v1-[0-9a-f]{64}$`)
