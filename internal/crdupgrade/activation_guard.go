package crdupgrade

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	releaseActivationGuardNamePrefix = "ptah-operator-release-activation-guard-v1-"
	releaseActivationGuardComponent  = "release-activation-guard"
	releaseActivationPolicyWeight    = "-168"
	releaseActivationBindingWeight   = "-167"
	releaseActivationHookWeight      = "-166"

	controllerCredentialsDataKey        = "controller-credentials"
	controllerCredentialsTargetDataKey  = "controller-credentials-target-release-sequence"
	controllerCredentialsAttemptDataKey = "controller-credentials-attempt"
)

// ControllerCredentialPhase is the persisted issuance state for controller
// identities protected by the service-account-origin policy.
type ControllerCredentialPhase string

const (
	ControllerCredentialsActive   ControllerCredentialPhase = "active"
	ControllerCredentialsDraining ControllerCredentialPhase = "draining"
)

// ReleaseActivationState is the exact durable activation and controller
// credential state observed from the retained release parameter.
type ReleaseActivationState struct {
	ActiveReleaseSequence      int32
	ControllerCredentialPhase  ControllerCredentialPhase
	DrainTargetReleaseSequence int32
	DrainAttempt               string
}

var (
	nonNegativeExactDecimalPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	positiveExactDecimalPattern    = regexp.MustCompile(`^[1-9][0-9]*$`)
	candidateAttemptPattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ReleaseActivationGuardPolicyName returns the versioned, release-owned name
// of the admission boundary around the activation parameter. It excludes the
// release sequence so one contract version protects every compatible update;
// a future incompatible contract uses a new name and explicitly retires this
// version only after the replacement is enforcing.
func ReleaseActivationGuardPolicyName(releaseNamespace, releaseName string) string {
	identity := releaseNamespace + "\n" + releaseName
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
	return releaseActivationGuardNamePrefix + digest[:12]
}

func releaseActivationGuardDenialMessage() string {
	return "Ptah release activation guard rejected an unsafe activation transition"
}

// ReleaseActivationGuard protects and advances the retained ConfigMap used as
// a parameter by every versioned rollout policy. Its admission policy is
// stable across releases and self-parameterized: an update is accepted only
// when the API server's parameter cache still agrees with oldObject. A
// successful no-op dry-run after persistence therefore proves that the cache
// has observed the new activation before the rollout continues.
type ReleaseActivationGuard struct {
	Policies                 ValidatingAdmissionPolicyReader
	Bindings                 ValidatingAdmissionPolicyBindingReader
	ConfigMaps               ConfigMapWriter
	ReleaseName              string
	ReleaseNamespace         string
	HookServiceAccountName   string
	ControllerStateVersion   int32
	AdmissionContractVersion int32
	ReleaseSequence          int32
	ManagerImage             string
	PollEvery                time.Duration
}

func (g *RolloutGuard) releaseActivationGuard() *ReleaseActivationGuard {
	if g == nil {
		return nil
	}
	return &ReleaseActivationGuard{
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
		PollEvery:                g.PollEvery,
	}
}

// Verify requires the retained policy and binding to match the stable
// compiled contract. It does not require the parameter to be activated for the
// candidate yet, because preflight necessarily runs before activation.
func (g *ReleaseActivationGuard) Verify(ctx context.Context) error {
	if err := g.validate(); err != nil {
		return err
	}
	name := ReleaseActivationGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	policy, err := g.Policies.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get release activation guard policy: %w", err)
	}
	if err := g.verifyPolicy(policy); err != nil {
		return err
	}
	binding, err := g.Bindings.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get release activation guard binding: %w", err)
	}
	return g.verifyBinding(binding)
}

// Prepare proves that the retained activation guard is type-checked and is
// actively rejecting malformed updates before any rollout state changes.
func (g *ReleaseActivationGuard) Prepare(ctx context.Context) error {
	if err := g.Verify(ctx); err != nil {
		return err
	}
	if err := g.waitPolicyReady(ctx); err != nil {
		return err
	}
	current, err := g.ConfigMaps.Get(ctx, ReleaseActivationName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get release activation parameter: %w", err)
	}
	identity, err := g.verifyActivationObject(current)
	if err != nil {
		return err
	}
	if err := g.verifyCandidateCompatibility(identity); err != nil {
		return err
	}
	return g.waitMalformedUpdateDenied(ctx, current)
}

// CurrentState verifies the retained parameter and returns its exact durable
// activation and credential-drain state.
func (g *ReleaseActivationGuard) CurrentState(ctx context.Context) (ReleaseActivationState, error) {
	if err := g.validate(); err != nil {
		return ReleaseActivationState{}, err
	}
	current, err := g.ConfigMaps.Get(ctx, ReleaseActivationName, metav1.GetOptions{})
	if err != nil {
		return ReleaseActivationState{}, fmt.Errorf("get release activation parameter: %w", err)
	}
	identity, err := g.verifyActivationObject(current)
	if err != nil {
		return ReleaseActivationState{}, err
	}
	if err := g.verifyCandidateCompatibility(identity); err != nil {
		return ReleaseActivationState{}, err
	}
	return releaseActivationState(identity)
}

// BeginDraining durably closes controller request and TokenRequest authority
// before the credential grace period starts. Repeating the exact draining
// tuple is a no-op, including after a successful update response was lost.
func (g *ReleaseActivationGuard) BeginDraining(ctx context.Context) (ReleaseActivationState, error) {
	if err := g.validate(); err != nil {
		return ReleaseActivationState{}, err
	}
	current, err := g.ConfigMaps.Get(ctx, ReleaseActivationName, metav1.GetOptions{})
	if err != nil {
		return ReleaseActivationState{}, fmt.Errorf("get release activation parameter before controller credential drain: %w", err)
	}
	identity, err := g.verifyActivationObject(current)
	if err != nil {
		return ReleaseActivationState{}, err
	}
	if err := g.verifyCandidateCompatibility(identity); err != nil {
		return ReleaseActivationState{}, err
	}
	if identity.phase == ControllerCredentialsDraining {
		if err := g.waitUpdateAllowed(ctx, current.DeepCopy(), "wait for release activation drain admission-cache propagation"); err != nil {
			return ReleaseActivationState{}, err
		}
		return releaseActivationState(identity)
	}

	candidate := current.DeepCopy()
	candidate.Data = map[string]string{
		activeReleaseDataKey:                strconv.FormatUint(identity.active, 10),
		controllerCredentialsDataKey:        string(ControllerCredentialsDraining),
		controllerCredentialsTargetDataKey:  strconv.FormatInt(int64(g.ReleaseSequence), 10),
		controllerCredentialsAttemptDataKey: g.candidateAttempt(),
	}
	candidateIdentity, err := g.verifyActivationObject(candidate)
	if err != nil {
		return ReleaseActivationState{}, fmt.Errorf("build controller credential drain state: %w", err)
	}
	if err := g.verifyCandidateCompatibility(candidateIdentity); err != nil {
		return ReleaseActivationState{}, err
	}
	if err := g.waitUpdateAllowed(ctx, candidate, "wait for release activation guard before controller credential drain"); err != nil {
		return ReleaseActivationState{}, err
	}
	_, updateErr := g.ConfigMaps.Update(ctx, candidate, metav1.UpdateOptions{})

	observed, getErr := g.ConfigMaps.Get(ctx, ReleaseActivationName, metav1.GetOptions{})
	if getErr != nil {
		if updateErr != nil {
			return ReleaseActivationState{}, errors.Join(
				fmt.Errorf("persist controller credential drain: %w", updateErr),
				fmt.Errorf("verify controller credential drain: %w", getErr),
			)
		}
		return ReleaseActivationState{}, fmt.Errorf("verify controller credential drain: %w", getErr)
	}
	observedIdentity, verifyErr := g.verifyActivationObject(observed)
	if verifyErr != nil {
		return ReleaseActivationState{}, verifyErr
	}
	if observed.UID != current.UID || !activationIdentityEqual(observed, candidate) {
		if updateErr != nil {
			return ReleaseActivationState{}, fmt.Errorf("persist controller credential drain: %w", updateErr)
		}
		return ReleaseActivationState{}, fmt.Errorf("controller credential drain state did not persist exactly")
	}
	if err := g.verifyCandidateCompatibility(observedIdentity); err != nil {
		return ReleaseActivationState{}, err
	}
	if err := g.waitUpdateAllowed(ctx, observed.DeepCopy(), "wait for release activation drain admission-cache propagation"); err != nil {
		return ReleaseActivationState{}, err
	}
	return releaseActivationState(observedIdentity)
}

// Activate advances the release parameter after quiescence. It first waits
// until a valid transition is accepted against the current parameter cache,
// persists it once, then retries a valid no-op dry-run until the admission
// cache has caught up with the persisted value.
func (g *ReleaseActivationGuard) Activate(ctx context.Context) error {
	if err := g.validate(); err != nil {
		return err
	}
	current, err := g.ConfigMaps.Get(ctx, ReleaseActivationName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get release activation parameter: %w", err)
	}
	identity, err := g.verifyActivationObject(current)
	if err != nil {
		return err
	}
	if err := g.verifyCandidateCompatibility(identity); err != nil {
		return err
	}
	candidateSequence := uint64(g.ReleaseSequence)
	if identity.phase == ControllerCredentialsActive && identity.active != 0 && identity.active != candidateSequence {
		return fmt.Errorf("release activation must drain controller credentials before advancing sequence %d to %d", identity.active, candidateSequence)
	}

	candidate := current.DeepCopy()
	candidate.Annotations[ControllerStateVersionAnnotation] = strconv.FormatInt(int64(g.ControllerStateVersion), 10)
	candidate.Annotations[AdmissionContractVersionAnnotation] = strconv.FormatInt(int64(g.AdmissionContractVersion), 10)
	candidate.Annotations[ReleaseSequenceAnnotation] = strconv.FormatInt(int64(g.ReleaseSequence), 10)
	candidate.Annotations[ManagerImageAnnotation] = g.ManagerImage
	candidate.Data = map[string]string{
		activeReleaseDataKey:         strconv.FormatInt(int64(g.ReleaseSequence), 10),
		controllerCredentialsDataKey: string(ControllerCredentialsActive),
	}
	if _, err := g.verifyActivationObject(candidate); err != nil {
		return fmt.Errorf("build candidate release activation parameter: %w", err)
	}

	if identity.active == candidateSequence {
		if !activationIdentityEqual(current, candidate) {
			return fmt.Errorf("release sequence %d is already activated with a different runtime identity", g.ReleaseSequence)
		}
	} else {
		if err := g.waitUpdateAllowed(ctx, candidate, "wait for release activation guard before persistence"); err != nil {
			return err
		}
		if _, err := g.ConfigMaps.Update(ctx, candidate, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("persist release activation: %w", err)
		}
	}

	observed, err := g.ConfigMaps.Get(ctx, ReleaseActivationName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("re-read release activation parameter: %w", err)
	}
	observedIdentity, err := g.verifyActivationObject(observed)
	if err != nil {
		return err
	}
	if observedIdentity.active != candidateSequence || !activationIdentityEqual(observed, candidate) {
		return fmt.Errorf("release activation parameter did not persist the candidate identity")
	}
	if err := g.waitUpdateAllowed(ctx, observed.DeepCopy(), "wait for release activation admission-cache propagation"); err != nil {
		return err
	}
	return nil
}

func (g *ReleaseActivationGuard) verifyCandidateCompatibility(identity releaseActivationIdentity) error {
	candidateSequence := uint64(g.ReleaseSequence)
	if identity.active > candidateSequence || identity.release > candidateSequence {
		return fmt.Errorf("release activation rollback refused: active identity %d/%d is newer than candidate %d", identity.active, identity.release, g.ReleaseSequence)
	}
	if identity.active > 0 && candidateSequence > identity.active+1 {
		return fmt.Errorf("release activation sequence gap refused: active sequence %d cannot advance directly to candidate %d", identity.active, g.ReleaseSequence)
	}
	if identity.active == 0 && identity.release != candidateSequence {
		return fmt.Errorf("release activation bootstrap attempt %d cannot be superseded by candidate %d", identity.release, g.ReleaseSequence)
	}
	if identity.state > uint64(g.ControllerStateVersion) {
		return fmt.Errorf("release activation controller-state rollback refused: active version %d is newer than candidate %d", identity.state, g.ControllerStateVersion)
	}
	if identity.admission > uint64(g.AdmissionContractVersion) {
		return fmt.Errorf("release activation admission-contract rollback refused: active version %d is newer than candidate %d", identity.admission, g.AdmissionContractVersion)
	}
	if identity.phase == ControllerCredentialsDraining {
		wantAttempt := g.candidateAttempt()
		if identity.target != candidateSequence || identity.attempt != wantAttempt {
			return fmt.Errorf(
				"release activation is draining for target %d attempt %q, want candidate %d attempt %q",
				identity.target,
				identity.attempt,
				candidateSequence,
				wantAttempt,
			)
		}
	}
	if identity.release == candidateSequence &&
		(identity.state != uint64(g.ControllerStateVersion) ||
			identity.admission != uint64(g.AdmissionContractVersion) ||
			identity.image != g.ManagerImage) {
		return fmt.Errorf("release sequence %d is already recorded with a different runtime identity", g.ReleaseSequence)
	}
	return nil
}

func (g *ReleaseActivationGuard) candidateAttempt() string {
	return hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
}

func releaseActivationState(identity releaseActivationIdentity) (ReleaseActivationState, error) {
	if identity.active > uint64(^uint32(0)>>1) || identity.target > uint64(^uint32(0)>>1) {
		return ReleaseActivationState{}, fmt.Errorf("release activation sequence exceeds the supported range")
	}
	return ReleaseActivationState{
		ActiveReleaseSequence:      int32(identity.active),
		ControllerCredentialPhase:  identity.phase,
		DrainTargetReleaseSequence: int32(identity.target),
		DrainAttempt:               identity.attempt,
	}, nil
}

type releaseActivationIdentity struct {
	active    uint64
	state     uint64
	admission uint64
	release   uint64
	image     string
	phase     ControllerCredentialPhase
	target    uint64
	attempt   string
}

func (g *ReleaseActivationGuard) verifyActivationObject(object *corev1.ConfigMap) (releaseActivationIdentity, error) {
	var identity releaseActivationIdentity
	if object == nil || object.Name != ReleaseActivationName || object.Namespace != g.ReleaseNamespace || object.GenerateName != "" {
		return identity, fmt.Errorf("release activation parameter has foreign or incomplete ownership")
	}
	wantLabels := map[string]string{
		managedByLabel:                rolloutGuardManagedBy,
		instanceLabel:                 g.ReleaseName,
		"app.kubernetes.io/component": rolloutGuardComponent,
	}
	wantFixedAnnotations := map[string]string{
		"helm.sh/hook":                "pre-install,pre-upgrade",
		"helm.sh/hook-weight":         releaseActivationHookWeight,
		"helm.sh/resource-policy":     "keep",
		rolloutGuardVersionAnnotation: rolloutGuardVersion,
		ReleaseNameAnnotation:         g.ReleaseName,
		ReleaseNamespaceAnnotation:    g.ReleaseNamespace,
	}
	if !reflect.DeepEqual(object.Labels, wantLabels) || len(object.Annotations) != len(wantFixedAnnotations)+4 {
		return identity, fmt.Errorf("release activation parameter has foreign or incomplete ownership")
	}
	for key, value := range wantFixedAnnotations {
		if object.Annotations[key] != value {
			return identity, fmt.Errorf("release activation parameter has foreign or incomplete ownership")
		}
	}
	if len(object.BinaryData) != 0 || (object.Immutable != nil && *object.Immutable) ||
		len(object.OwnerReferences) != 0 || len(object.Finalizers) != 0 || object.DeletionTimestamp != nil ||
		object.DeletionGracePeriodSeconds != nil || object.UID == "" || object.ResourceVersion == "" {
		return identity, fmt.Errorf("release activation parameter data and metadata shape is not exact")
	}
	activeValue, found := object.Data[activeReleaseDataKey]
	if !found || !nonNegativeExactDecimalPattern.MatchString(activeValue) {
		return identity, fmt.Errorf("release activation sequence is not an exact non-negative decimal")
	}
	active, err := strconv.ParseUint(activeValue, 10, 63)
	if err != nil {
		return identity, fmt.Errorf("release activation sequence: %w", err)
	}
	state, err := positiveDecimalValue(object.Annotations[ControllerStateVersionAnnotation])
	if err != nil {
		return identity, fmt.Errorf("release activation controller-state version: %w", err)
	}
	admission, err := positiveDecimalValue(object.Annotations[AdmissionContractVersionAnnotation])
	if err != nil {
		return identity, fmt.Errorf("release activation admission-contract version: %w", err)
	}
	release, err := positiveDecimalValue(object.Annotations[ReleaseSequenceAnnotation])
	if err != nil {
		return identity, fmt.Errorf("release activation release sequence: %w", err)
	}
	if active > 0 && release != active {
		return identity, fmt.Errorf("release activation data and release annotation differ")
	}
	image := object.Annotations[ManagerImageAnnotation]
	if image == "" || strings.IndexFunc(image, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }) >= 0 {
		return identity, fmt.Errorf("release activation manager image is empty or contains whitespace")
	}
	phase := ControllerCredentialPhase(object.Data[controllerCredentialsDataKey])
	var target uint64
	var attempt string
	switch phase {
	case ControllerCredentialsActive:
		if len(object.Data) != 2 {
			return identity, fmt.Errorf("active release activation parameter has unexpected drain state")
		}
	case ControllerCredentialsDraining:
		if len(object.Data) != 4 {
			return identity, fmt.Errorf("draining release activation parameter has incomplete drain state")
		}
		targetValue := object.Data[controllerCredentialsTargetDataKey]
		if !positiveExactDecimalPattern.MatchString(targetValue) {
			return identity, fmt.Errorf("release activation drain target is not an exact positive decimal")
		}
		target, err = strconv.ParseUint(targetValue, 10, 63)
		if err != nil {
			return identity, fmt.Errorf("release activation drain target: %w", err)
		}
		attempt = object.Data[controllerCredentialsAttemptDataKey]
		if !candidateAttemptPattern.MatchString(attempt) {
			return identity, fmt.Errorf("release activation drain attempt is not an exact candidate digest")
		}
	default:
		return identity, fmt.Errorf("release activation controller credential phase %q is invalid", phase)
	}
	return releaseActivationIdentity{
		active: active, state: state, admission: admission, release: release, image: image,
		phase: phase, target: target, attempt: attempt,
	}, nil
}

func activationIdentityEqual(left, right *corev1.ConfigMap) bool {
	return reflect.DeepEqual(left.Annotations, right.Annotations) &&
		reflect.DeepEqual(left.Labels, right.Labels) &&
		reflect.DeepEqual(left.Data, right.Data) &&
		reflect.DeepEqual(left.BinaryData, right.BinaryData) &&
		reflect.DeepEqual(left.Immutable, right.Immutable) &&
		reflect.DeepEqual(left.OwnerReferences, right.OwnerReferences) &&
		reflect.DeepEqual(left.Finalizers, right.Finalizers)
}

func (g *ReleaseActivationGuard) validate() error {
	if g == nil || g.Policies == nil || g.Bindings == nil || g.ConfigMaps == nil {
		return fmt.Errorf("release activation guard clients are required")
	}
	if g.ReleaseName == "" || g.ReleaseNamespace == "" || g.HookServiceAccountName == "" {
		return fmt.Errorf("release activation guard identity is required")
	}
	if g.ControllerStateVersion < 1 || g.AdmissionContractVersion < 1 || g.ReleaseSequence < 1 {
		return fmt.Errorf("release activation guard versions must be positive")
	}
	if g.ManagerImage == "" || strings.IndexFunc(g.ManagerImage, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }) >= 0 {
		return fmt.Errorf("release activation manager image is empty or contains whitespace")
	}
	if _, err := g.hookUsernamePattern(); err != nil {
		return err
	}
	if g.PollEvery <= 0 {
		return fmt.Errorf("release activation guard poll interval must be positive")
	}
	return nil
}

func (g *ReleaseActivationGuard) hookUsernamePattern() (string, error) {
	suffix := fmt.Sprintf("-crd-v%d-", g.ReleaseSequence)
	identitySuffix := suffix + hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)[:12]
	if !strings.HasSuffix(g.HookServiceAccountName, identitySuffix) {
		return "", fmt.Errorf("hook service account does not match the candidate release identity")
	}
	base := strings.TrimSuffix(g.HookServiceAccountName, identitySuffix)
	if base == "" {
		return "", fmt.Errorf("hook service account has no stable name prefix")
	}
	prefix := "system:serviceaccount:" + g.ReleaseNamespace + ":" + base + "-crd-v"
	return "^" + regexp.QuoteMeta(prefix), nil
}

func activationServiceAccountGroupsExpression(namespace string) string {
	return fmt.Sprintf(`request.userInfo.groups.size() == 3 && "system:serviceaccounts" in request.userInfo.groups && %q in request.userInfo.groups && "system:authenticated" in request.userInfo.groups`, "system:serviceaccounts:"+namespace)
}

func activationNamespaceControllerExpression() string {
	return `(request.userInfo.username == "system:kube-controller-manager" && request.userInfo.groups.size() == 1 && "system:authenticated" in request.userInfo.groups) || (request.userInfo.username == "system:serviceaccount:kube-system:namespace-controller" && request.userInfo.groups.size() == 3 && "system:serviceaccounts" in request.userInfo.groups && "system:serviceaccounts:kube-system" in request.userInfo.groups && "system:authenticated" in request.userInfo.groups)`
}

func (g *ReleaseActivationGuard) policy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := ReleaseActivationGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	denial := releaseActivationGuardDenialMessage()
	hookPattern, _ := g.hookUsernamePattern()
	metadata := g.metadata(name, releaseActivationPolicyWeight)
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
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update, admissionregistrationv1.Delete},
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
				Name:       "fixed-release-activation",
				Expression: fmt.Sprintf(`request.namespace == %q && ((request.operation == "DELETE" && oldObject != null && oldObject.metadata.name == %q) || (request.operation != "DELETE" && request.name == %q))`, g.ReleaseNamespace, ReleaseActivationName, ReleaseActivationName),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "paramsActive", Expression: decimalCEL("params", activeReleaseDataKey, true)},
				{Name: "paramsCredentialPhase", Expression: stringDataCEL("params", controllerCredentialsDataKey)},
				{Name: "newActive", Expression: guardedDecimalCEL("object", activeReleaseDataKey, true)},
				{Name: "oldActive", Expression: guardedDecimalCEL("oldObject", activeReleaseDataKey, true)},
				{Name: "newCredentialPhase", Expression: guardedStringDataCEL("object", controllerCredentialsDataKey)},
				{Name: "oldCredentialPhase", Expression: guardedStringDataCEL("oldObject", controllerCredentialsDataKey)},
				{Name: "newDrainTarget", Expression: guardedDecimalCEL("object", controllerCredentialsTargetDataKey, false)},
				{Name: "oldDrainTarget", Expression: guardedDecimalCEL("oldObject", controllerCredentialsTargetDataKey, false)},
				{Name: "newDrainAttempt", Expression: guardedStringDataCEL("object", controllerCredentialsAttemptDataKey)},
				{Name: "oldDrainAttempt", Expression: guardedStringDataCEL("oldObject", controllerCredentialsAttemptDataKey)},
				{Name: "newState", Expression: guardedAnnotationDecimalCEL("object", ControllerStateVersionAnnotation)},
				{Name: "oldState", Expression: guardedAnnotationDecimalCEL("oldObject", ControllerStateVersionAnnotation)},
				{Name: "newAdmission", Expression: guardedAnnotationDecimalCEL("object", AdmissionContractVersionAnnotation)},
				{Name: "oldAdmission", Expression: guardedAnnotationDecimalCEL("oldObject", AdmissionContractVersionAnnotation)},
				{Name: "newRelease", Expression: guardedAnnotationDecimalCEL("object", ReleaseSequenceAnnotation)},
				{Name: "oldRelease", Expression: guardedAnnotationDecimalCEL("oldObject", ReleaseSequenceAnnotation)},
				{Name: "isReleaseHook", Expression: fmt.Sprintf(`request.operation == "UPDATE" && variables.newActive > 0 && request.userInfo.username.matches(%q + string(variables.newActive) + "-[0-9a-f]{12}$") && (%s)`, hookPattern, activationServiceAccountGroupsExpression(g.ReleaseNamespace))},
				{Name: "isDrainHook", Expression: fmt.Sprintf(`request.operation == "UPDATE" && variables.newDrainTarget > 0 && variables.newDrainAttempt.matches("^[0-9a-f]{64}$") && request.userInfo.username.matches(%q + string(variables.newDrainTarget) + "-" + variables.newDrainAttempt.substring(0, 12) + "$") && (%s)`, hookPattern, activationServiceAccountGroupsExpression(g.ReleaseNamespace))},
				{Name: "isDrainedActivationHook", Expression: fmt.Sprintf(`request.operation == "UPDATE" && variables.oldDrainTarget > 0 && variables.oldDrainAttempt.matches("^[0-9a-f]{64}$") && request.userInfo.username.matches(%q + string(variables.oldDrainTarget) + "-" + variables.oldDrainAttempt.substring(0, 12) + "$") && (%s)`, hookPattern, activationServiceAccountGroupsExpression(g.ReleaseNamespace))},
				{Name: "isReleaseHookCaller", Expression: fmt.Sprintf(`request.userInfo.username.matches(%q + "[1-9][0-9]*-[0-9a-f]{12}$") && (%s)`, hookPattern, activationServiceAccountGroupsExpression(g.ReleaseNamespace))},
				{Name: "isNamespaceController", Expression: activationNamespaceControllerExpression()},
			},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `request.operation != "DELETE" || variables.isNamespaceController || variables.isReleaseHookCaller`, Message: denial},
				{Expression: fmt.Sprintf(`request.operation == "DELETE" || (%s)`, g.activationObjectShapeExpression("object")), Message: denial},
				{Expression: fmt.Sprintf(`request.operation == "CREATE" || (%s)`, g.activationObjectShapeExpression("oldObject")), Message: denial},
				{Expression: fmt.Sprintf(`request.operation == "DELETE" || (params != null && (%s) && params.data == oldObject.data && params.metadata.annotations == oldObject.metadata.annotations && params.metadata.labels == oldObject.metadata.labels && variables.paramsActive == variables.oldActive && variables.paramsCredentialPhase == variables.oldCredentialPhase)`, g.activationObjectShapeExpression("params")), Message: denial},
				{
					Expression: `request.operation != "UPDATE" || ` +
						`(object.data == oldObject.data && object.metadata.annotations == oldObject.metadata.annotations && object.metadata.labels == oldObject.metadata.labels && variables.isReleaseHookCaller) || ` +
						`(variables.oldCredentialPhase == "active" && variables.newCredentialPhase == "draining" && variables.newActive == variables.oldActive && object.metadata.annotations == oldObject.metadata.annotations && object.metadata.labels == oldObject.metadata.labels && variables.newDrainTarget >= variables.oldRelease && variables.newDrainTarget <= variables.oldRelease + 1 && ((variables.oldActive == 0 && variables.newDrainTarget == variables.oldRelease) || (variables.oldActive > 0 && variables.newDrainTarget <= variables.oldActive + 1)) && variables.isDrainHook) || ` +
						`(variables.oldCredentialPhase == "draining" && variables.newCredentialPhase == "active" && variables.newActive == variables.oldDrainTarget && variables.newRelease == variables.newActive && variables.newRelease >= variables.oldRelease && variables.newState >= variables.oldState && variables.newAdmission >= variables.oldAdmission && variables.isDrainedActivationHook) || ` +
						`(variables.oldCredentialPhase == "active" && variables.oldActive == 0 && variables.newCredentialPhase == "active" && variables.newActive == variables.oldRelease && variables.newRelease == variables.newActive && variables.newState >= variables.oldState && variables.newAdmission >= variables.oldAdmission && variables.isReleaseHook)`,
					Message: denial,
				},
			},
		},
	}
}

func (g *ReleaseActivationGuard) binding() *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	action := admissionregistrationv1.DenyAction
	exact := admissionregistrationv1.Exact
	name := ReleaseActivationGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.metadata(name, releaseActivationBindingWeight),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName: name,
			MatchResources: &admissionregistrationv1.MatchResources{
				MatchPolicy: &exact,
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					corev1.LabelMetadataName: g.ReleaseNamespace,
				}},
				ObjectSelector: &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update, admissionregistrationv1.Delete},
						Rule: admissionregistrationv1.Rule{
							APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"},
							Scope: scopePtr(admissionregistrationv1.NamespacedScope),
						},
					},
				}},
			},
			ParamRef: &admissionregistrationv1.ParamRef{
				Name:                    ReleaseActivationName,
				Namespace:               g.ReleaseNamespace,
				ParameterNotFoundAction: &action,
			},
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
}

func (g *ReleaseActivationGuard) metadata(name, hookWeight string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name,
		Annotations: map[string]string{
			"helm.sh/hook":                "pre-install,pre-upgrade",
			"helm.sh/hook-weight":         hookWeight,
			"helm.sh/resource-policy":     "keep",
			rolloutGuardVersionAnnotation: rolloutGuardVersion,
			ReleaseNameAnnotation:         g.ReleaseName,
			ReleaseNamespaceAnnotation:    g.ReleaseNamespace,
		},
		Labels: map[string]string{
			managedByLabel:                rolloutGuardManagedBy,
			instanceLabel:                 g.ReleaseName,
			"app.kubernetes.io/component": releaseActivationGuardComponent,
		},
	}
}

func (g *ReleaseActivationGuard) activationObjectShapeExpression(object string) string {
	parts := []string{
		fmt.Sprintf(`%s.metadata.name == %q`, object, ReleaseActivationName),
		fmt.Sprintf(`%s.metadata.namespace == %q`, object, g.ReleaseNamespace),
		fmt.Sprintf(`(!has(%s.metadata.generateName) || %s.metadata.generateName == "")`, object, object),
		fmt.Sprintf(`has(%s.metadata.uid) && %s.metadata.uid != ""`, object, object),
		fmt.Sprintf(`has(%s.metadata.resourceVersion) && %s.metadata.resourceVersion != ""`, object, object),
		fmt.Sprintf(`has(%s.metadata.annotations)`, object),
		fmt.Sprintf(`%s.metadata.annotations.size() == 10`, object),
		fmt.Sprintf(`%s.metadata.annotations[%q] == "pre-install,pre-upgrade"`, object, "helm.sh/hook"),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, "helm.sh/hook-weight", releaseActivationHookWeight),
		fmt.Sprintf(`%s.metadata.annotations[%q] == "keep"`, object, "helm.sh/resource-policy"),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, rolloutGuardVersionAnnotation, rolloutGuardVersion),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, ReleaseNameAnnotation, g.ReleaseName),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, ReleaseNamespaceAnnotation, g.ReleaseNamespace),
		fmt.Sprintf(`%s.metadata.annotations[%q].matches("^[1-9][0-9]*$")`, object, ControllerStateVersionAnnotation),
		fmt.Sprintf(`%s.metadata.annotations[%q].matches("^[1-9][0-9]*$")`, object, AdmissionContractVersionAnnotation),
		fmt.Sprintf(`%s.metadata.annotations[%q].matches("^[1-9][0-9]*$")`, object, ReleaseSequenceAnnotation),
		fmt.Sprintf(`%s.metadata.annotations[%q].matches("^[^[:space:]]+$")`, object, ManagerImageAnnotation),
		fmt.Sprintf(`has(%s.metadata.labels)`, object),
		fmt.Sprintf(`%s.metadata.labels.size() == 3`, object),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, managedByLabel, rolloutGuardManagedBy),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, instanceLabel, g.ReleaseName),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, "app.kubernetes.io/component", rolloutGuardComponent),
		fmt.Sprintf(`has(%s.data)`, object),
		fmt.Sprintf(`%q in %s.data`, activeReleaseDataKey, object),
		fmt.Sprintf(`%s.data[%q].matches("^(0|[1-9][0-9]*)$")`, object, activeReleaseDataKey),
		fmt.Sprintf(`%q in %s.data`, controllerCredentialsDataKey, object),
		fmt.Sprintf(
			`((%s.data[%q] == %q && %s.data.size() == 2) || (%s.data[%q] == %q && %s.data.size() == 4 && %q in %s.data && %s.data[%q].matches("^[1-9][0-9]*$") && %q in %s.data && %s.data[%q].matches("^[0-9a-f]{64}$")))`,
			object, controllerCredentialsDataKey, ControllerCredentialsActive, object,
			object, controllerCredentialsDataKey, ControllerCredentialsDraining, object,
			controllerCredentialsTargetDataKey, object, object, controllerCredentialsTargetDataKey,
			controllerCredentialsAttemptDataKey, object, object, controllerCredentialsAttemptDataKey,
		),
		fmt.Sprintf(`(!has(%s.binaryData) || %s.binaryData.size() == 0)`, object, object),
		fmt.Sprintf(`(!has(%s.immutable) || !%s.immutable)`, object, object),
		fmt.Sprintf(`(!has(%s.metadata.ownerReferences) || %s.metadata.ownerReferences.size() == 0)`, object, object),
		fmt.Sprintf(`(!has(%s.metadata.finalizers) || %s.metadata.finalizers.size() == 0)`, object, object),
		fmt.Sprintf(`!has(%s.metadata.deletionTimestamp)`, object),
		fmt.Sprintf(`!has(%s.metadata.deletionGracePeriodSeconds)`, object),
		fmt.Sprintf(`(%s.data[%q] == "0" || %s.metadata.annotations[%q] == %s.data[%q])`, object, activeReleaseDataKey, object, ReleaseSequenceAnnotation, object, activeReleaseDataKey),
	}
	return strings.Join(parts, " && ")
}

func decimalCEL(object, key string, allowZero bool) string {
	pattern := "^[1-9][0-9]*$"
	if allowZero {
		pattern = "^(0|[1-9][0-9]*)$"
	}
	return fmt.Sprintf(`%s != null && has(%s.data) && %q in %s.data && %s.data[%q].matches(%q) ? int(%s.data[%q]) : -1`, object, object, key, object, object, key, pattern, object, key)
}

func guardedDecimalCEL(object, key string, allowZero bool) string {
	return fmt.Sprintf(`request.operation == "UPDATE" ? (%s) : -1`, decimalCEL(object, key, allowZero))
}

func stringDataCEL(object, key string) string {
	return fmt.Sprintf(`%s != null && has(%s.data) && %q in %s.data ? %s.data[%q] : ""`, object, object, key, object, object, key)
}

func guardedStringDataCEL(object, key string) string {
	return fmt.Sprintf(`request.operation == "UPDATE" ? (%s) : ""`, stringDataCEL(object, key))
}

func guardedAnnotationDecimalCEL(object, key string) string {
	return fmt.Sprintf(`request.operation == "UPDATE" && has(%s.metadata.annotations) && %q in %s.metadata.annotations && %s.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(%s.metadata.annotations[%q]) : 0`, object, key, object, object, key, object, key)
}

func (g *ReleaseActivationGuard) verifyPolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	name := ReleaseActivationGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	if policy == nil || policy.Name != name {
		return fmt.Errorf("fixed release activation guard policy %s is missing", name)
	}
	if err := g.verifyMetadata("ValidatingAdmissionPolicy", policy.ObjectMeta, releaseActivationPolicyWeight); err != nil {
		return err
	}
	if !reflect.DeepEqual(policy.Spec, g.policy().Spec) {
		return fmt.Errorf("release activation guard policy spec differs from the immutable contract")
	}
	return nil
}

func (g *ReleaseActivationGuard) verifyBinding(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
	name := ReleaseActivationGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	if binding == nil || binding.Name != name {
		return fmt.Errorf("fixed release activation guard binding %s is missing", name)
	}
	if err := g.verifyMetadata("ValidatingAdmissionPolicyBinding", binding.ObjectMeta, releaseActivationBindingWeight); err != nil {
		return err
	}
	if !reflect.DeepEqual(binding.Spec, g.binding().Spec) {
		return fmt.Errorf("release activation guard binding spec differs from the immutable contract")
	}
	return nil
}

func (g *ReleaseActivationGuard) verifyMetadata(kind string, metadata metav1.ObjectMeta, hookWeight string) error {
	expected := g.metadata(ReleaseActivationGuardPolicyName(g.ReleaseNamespace, g.ReleaseName), hookWeight)
	if metadata.Name != expected.Name || metadata.Namespace != "" || metadata.GenerateName != "" ||
		metadata.DeletionTimestamp != nil || metadata.DeletionGracePeriodSeconds != nil ||
		len(metadata.OwnerReferences) != 0 || len(metadata.Finalizers) != 0 {
		return fmt.Errorf("fixed release activation guard %s has an unexpected name", kind)
	}
	if !reflect.DeepEqual(metadata.Annotations, expected.Annotations) || !reflect.DeepEqual(metadata.Labels, expected.Labels) {
		return fmt.Errorf("fixed release activation guard %s has foreign or incomplete ownership", kind)
	}
	return nil
}

func (g *ReleaseActivationGuard) waitPolicyReady(ctx context.Context) error {
	name := ReleaseActivationGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	return wait.PollUntilContextCancel(ctx, g.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		policy, err := g.Policies.Get(pollCtx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("read release activation guard policy status: %w", err)
		}
		if err := g.verifyPolicy(policy); err != nil {
			return false, err
		}
		if policy.Status.ObservedGeneration != policy.Generation || policy.Status.TypeChecking == nil {
			return false, nil
		}
		if warnings := policy.Status.TypeChecking.ExpressionWarnings; len(warnings) != 0 {
			return false, fmt.Errorf("release activation guard policy has CEL type-check warnings: %s", warnings[0].Warning)
		}
		return true, nil
	})
}

func (g *ReleaseActivationGuard) waitMalformedUpdateDenied(ctx context.Context, current *corev1.ConfigMap) error {
	return wait.PollUntilContextCancel(ctx, g.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		probe := current.DeepCopy()
		probe.Data = map[string]string{activeReleaseDataKey: current.Data[activeReleaseDataKey], "unexpected": "must-be-denied"}
		_, err := g.ConfigMaps.Update(pollCtx, probe, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
		if err == nil {
			return false, nil
		}
		if strings.Contains(err.Error(), releaseActivationGuardDenialMessage()) {
			return true, nil
		}
		if activationAdmissionMayBePropagating(err) {
			return false, nil
		}
		return false, fmt.Errorf("probe release activation guard enforcement: %w", err)
	})
}

func (g *ReleaseActivationGuard) waitUpdateAllowed(ctx context.Context, candidate *corev1.ConfigMap, operation string) error {
	err := wait.PollUntilContextCancel(ctx, g.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		_, updateErr := g.ConfigMaps.Update(pollCtx, candidate.DeepCopy(), metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
		if updateErr == nil {
			return true, nil
		}
		if activationAdmissionMayBePropagating(updateErr) {
			return false, nil
		}
		return false, updateErr
	})
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}

func activationAdmissionMayBePropagating(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(err.Error(), releaseActivationGuardDenialMessage()) ||
		strings.Contains(message, "parameter") ||
		strings.Contains(message, "validatingadmissionpolicy")
}
