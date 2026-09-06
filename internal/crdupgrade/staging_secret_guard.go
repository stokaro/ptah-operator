package crdupgrade

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/stokaro/ptah-operator/internal/certrotation"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	stagingSecretGuardNamePrefix    = "ptah-operator-cert-stage-guard-v1-"
	stagingSecretGuardComponent     = "certificate-staging-secret-guard"
	stagingSecretGuardPolicyWeight  = "-162"
	stagingSecretGuardBindingWeight = "-161"
)

// StagingSecretClient is the exact Secret API surface needed to drain the
// durable certificate candidate before its retained admission guard retires.
type StagingSecretClient interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.Secret, error)
	Update(context.Context, *corev1.Secret, metav1.UpdateOptions) (*corev1.Secret, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// StagingSecretGuard is the release-stable admission boundary around the
// durable certificate-rotation staging Secret. It excludes candidate release
// inputs so a retained older object remains the exact contract on upgrades.
type StagingSecretGuard struct {
	rollout *RolloutGuard
}

// NewStagingSecretGuard derives a staging Secret guard from the compiled
// rollout identity.
func NewStagingSecretGuard(rollout *RolloutGuard) *StagingSecretGuard {
	return &StagingSecretGuard{rollout: rollout}
}

// StagingSecretGuardPolicyName returns the stable release-scoped policy name.
// Policy and binding intentionally use the same deterministic identity.
func StagingSecretGuardPolicyName(releaseNamespace, releaseName string) string {
	digest := sha256.Sum256([]byte(releaseNamespace + "\n" + releaseName))
	return stagingSecretGuardNamePrefix + fmt.Sprintf("%x", digest)[:12]
}

// StagingSecretGuardBindingName returns the stable release-scoped binding
// name.
func StagingSecretGuardBindingName(releaseNamespace, releaseName string) string {
	return StagingSecretGuardPolicyName(releaseNamespace, releaseName)
}

// StagingSecretGuardInventoryNames returns the stable pair identity for RBAC
// and teardown inventories.
func StagingSecretGuardInventoryNames(releaseNamespace, releaseName string) []string {
	return []string{StagingSecretGuardPolicyName(releaseNamespace, releaseName)}
}

func stagingSecretGuardDenialMessage() string {
	return "Ptah certificate staging Secret guard rejected an unsafe lifecycle request"
}

// ExpectedPolicy constructs the immutable retained policy contract.
func (g *StagingSecretGuard) ExpectedPolicy() (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	contract, err := g.contract()
	if err != nil {
		return nil, err
	}
	fail := admissionregistrationv1.Fail
	name := StagingSecretGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	message := stagingSecretGuardDenialMessage()
	oldShape := contract.secretShape("oldObject", true)
	newShape := contract.secretShape("object", true)
	// The API server fills uid and creationTimestamp before validating
	// admission sees a CREATE, so the create shape asserts nothing about the
	// live identity: absence would refuse every real create, presence is
	// not the client's doing either way.
	createShape := contract.secretShape("object", false)
	rotator := exactServiceAccountPrincipalExpression(g.rollout.ReleaseNamespace, contract.rotatorServiceAccount)
	cleanup := contract.cleanupPrincipalExpression()
	dataPreserved := `has(dyn(object).data) == has(dyn(oldObject).data) && (!has(dyn(object).data) || dyn(object).data == dyn(oldObject).data)`
	updateIdentity := `object.metadata.uid == oldObject.metadata.uid && object.metadata.resourceVersion == oldObject.metadata.resourceVersion && has(object.metadata.creationTimestamp) == has(oldObject.metadata.creationTimestamp) && (!has(object.metadata.creationTimestamp) || object.metadata.creationTimestamp == oldObject.metadata.creationTimestamp)`

	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(name, stagingSecretGuardPolicyWeight),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			ParamKind: &admissionregistrationv1.ParamKind{
				APIVersion: "v1",
				Kind:       "ConfigMap",
			},
			MatchConstraints: g.matchResources(contract.stagingSecretName),
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name: "exact-certificate-staging-secret",
				Expression: fmt.Sprintf(
					`request.namespace == %q && request.name == %q && (!has(request.subResource) || request.subResource == "")`,
					g.rollout.ReleaseNamespace,
					contract.stagingSecretName,
				),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "isCreate", Expression: `request.operation == "CREATE"`},
				{Name: "isUpdate", Expression: `request.operation == "UPDATE"`},
				{Name: "isDelete", Expression: `request.operation == "DELETE"`},
				{Name: "isRotator", Expression: rotator},
				{Name: "isCleanup", Expression: cleanup},
			},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `variables.isCreate || variables.isUpdate || variables.isDelete`, Message: message},
				{Expression: `variables.isCreate || (` + oldShape + `)`, Message: message},
				{Expression: `variables.isDelete || (variables.isCreate && (` + createShape + `)) || (variables.isUpdate && (` + newShape + `))`, Message: message},
				{Expression: `!variables.isCreate || (!has(dyn(object).data) || dyn(object).data.size() == 0)`, Message: message},
				{Expression: `!variables.isUpdate || (` + updateIdentity + `)`, Message: message},
				{Expression: fmt.Sprintf(`!variables.isUpdate || (variables.isRotator || (variables.isCleanup && (!has(dyn(object).data) || dyn(object).data.size() == 0)) || ((!variables.isRotator && !variables.isCleanup) && (%s)))`, dataPreserved), Message: message},
				{Expression: `!variables.isDelete || (variables.isCleanup && (!has(dyn(oldObject).data) || dyn(oldObject).data.size() == 0))`, Message: message},
			},
		},
	}
	addStableAdmissionConvergenceDependencyProbe(
		policy,
		g.rollout.ReleaseNamespace,
		serviceAccountObjectGuardMarkerPattern(g.rollout.ReleaseNamespace, g.rollout.ReleaseName),
	)
	return policy, nil
}

// ExpectedBinding constructs the exact deny-only enforcement binding.
func (g *StagingSecretGuard) ExpectedBinding() (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	contract, err := g.contract()
	if err != nil {
		return nil, err
	}
	action := admissionregistrationv1.DenyAction
	name := StagingSecretGuardBindingName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.metadata(name, stagingSecretGuardBindingWeight),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:     StagingSecretGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName),
			MatchResources: g.matchResources(contract.stagingSecretName),
			ParamRef: &admissionregistrationv1.ParamRef{
				Name:                    ReleaseActivationName,
				Namespace:               g.rollout.ReleaseNamespace,
				ParameterNotFoundAction: &action,
			},
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
	addAdmissionConvergenceProbeMatchResource(binding.Spec.MatchResources, "")
	return binding, nil
}

// ExpectedObjects returns the exact retained policy and binding.
func (g *StagingSecretGuard) ExpectedObjects() (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	policy, err := g.ExpectedPolicy()
	if err != nil {
		return nil, nil, err
	}
	binding, err := g.ExpectedBinding()
	if err != nil {
		return nil, nil, err
	}
	return policy, binding, nil
}

// Verify requires the persisted policy and binding to equal the compiled
// release-stable contract, including ownership and lifecycle metadata.
func (g *StagingSecretGuard) Verify(ctx context.Context) error {
	if err := g.validate(true); err != nil {
		return err
	}
	expectedPolicy, expectedBinding, err := g.ExpectedObjects()
	if err != nil {
		return err
	}
	policy, err := g.rollout.Policies.Get(ctx, expectedPolicy.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get staging Secret guard policy: %w", err)
	}
	if err := g.verifyPolicy(policy, expectedPolicy); err != nil {
		return err
	}
	binding, err := g.rollout.Bindings.Get(ctx, expectedBinding.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get staging Secret guard binding: %w", err)
	}
	return g.verifyBinding(binding, expectedBinding)
}

// WaitReady verifies the retained pair and waits until the API server has
// type-checked the complete CEL contract without warnings.
func (g *StagingSecretGuard) WaitReady(ctx context.Context) error {
	if err := g.Verify(ctx); err != nil {
		return err
	}
	name := StagingSecretGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	return wait.PollUntilContextCancel(ctx, g.rollout.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		policy, err := g.rollout.Policies.Get(pollCtx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("read staging Secret guard policy status: %w", err)
		}
		expected, err := g.ExpectedPolicy()
		if err != nil {
			return false, err
		}
		if err := g.verifyPolicy(policy, expected); err != nil {
			return false, err
		}
		if policy.Status.ObservedGeneration != policy.Generation || policy.Status.TypeChecking == nil {
			return false, nil
		}
		if warnings := policy.Status.TypeChecking.ExpressionWarnings; len(warnings) != 0 {
			return false, fmt.Errorf("staging Secret guard policy has CEL type-check warnings: %s", warnings[0].Warning)
		}
		return true, nil
	})
}

// Cleanup atomically drains the exact staging object and deletes that same
// persisted identity. Admission independently limits both mutations to the
// teardown cleanup ServiceAccount and rejects deletion while data is present.
func (g *StagingSecretGuard) Cleanup(ctx context.Context, secrets StagingSecretClient) error {
	contract, err := g.contract()
	if err != nil {
		return err
	}
	if secrets == nil {
		return errors.New("staging Secret cleanup client is required")
	}
	if err := g.Verify(ctx); err != nil {
		return fmt.Errorf("verify staging Secret guard before cleanup: %w", err)
	}

	secret, err := secrets.Get(ctx, contract.stagingSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get staging Secret for cleanup: %w", err)
	}
	if err := contract.verifyLiveSecret(secret); err != nil {
		return err
	}
	originalUID := secret.UID
	if len(secret.Data) != 0 {
		update := secret.DeepCopy()
		update.Data = nil
		updated, updateErr := secrets.Update(ctx, update, metav1.UpdateOptions{})
		if updateErr != nil {
			return fmt.Errorf("atomically clear staging Secret data: %w", updateErr)
		}
		if err := contract.verifyLiveSecret(updated); err != nil {
			return fmt.Errorf("verify cleared staging Secret update: %w", err)
		}
		if updated.UID != originalUID || len(updated.Data) != 0 {
			return errors.New("cleared staging Secret update changed identity or retained data")
		}
	}

	secret, err = secrets.Get(ctx, contract.stagingSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("re-read cleared staging Secret: %w", err)
	}
	if err := contract.verifyLiveSecret(secret); err != nil {
		return err
	}
	if secret.UID != originalUID || len(secret.Data) != 0 {
		return errors.New("staging Secret changed identity or data before deletion")
	}
	uid := secret.UID
	resourceVersion := secret.ResourceVersion
	err = secrets.Delete(ctx, contract.stagingSecretName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID:             &uid,
		ResourceVersion: &resourceVersion,
	}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete cleared staging Secret: %w", err)
	}
	return nil
}

type stagingSecretContract struct {
	namespace              string
	releaseName            string
	stagingSecretName      string
	rotatorServiceAccount  string
	hookServiceAccountBase string
}

func (g *StagingSecretGuard) contract() (stagingSecretContract, error) {
	if err := g.validate(false); err != nil {
		return stagingSecretContract{}, err
	}
	stagingSecretName, err := uniqueRuntimeArg(g.rollout.CertificateArgs, "--staging-secret-name=")
	if err != nil {
		return stagingSecretContract{}, fmt.Errorf("derive staging Secret guard identity: %w", err)
	}
	if stagingSecretName == g.rollout.WebhookSecretName {
		return stagingSecretContract{}, errors.New("staging Secret guard target must differ from the serving Secret")
	}
	if problems := utilvalidation.IsDNS1123Subdomain(stagingSecretName); len(problems) != 0 {
		return stagingSecretContract{}, fmt.Errorf("staging Secret guard target %q is invalid: %v", stagingSecretName, problems)
	}
	identity, err := ServiceAccountObjectIdentityContractForRollout(g.rollout)
	if err != nil {
		return stagingSecretContract{}, fmt.Errorf("derive staging Secret cleanup identity: %w", err)
	}
	return stagingSecretContract{
		namespace:              g.rollout.ReleaseNamespace,
		releaseName:            g.rollout.ReleaseName,
		stagingSecretName:      stagingSecretName,
		rotatorServiceAccount:  g.rollout.CertificateDeploymentName,
		hookServiceAccountBase: identity.HookServiceAccountBase,
	}, nil
}

func (c stagingSecretContract) cleanupPrincipalExpression() string {
	parameterShape := strings.Join([]string{
		`params != null`,
		`has(params.metadata)`,
		fmt.Sprintf(`has(params.metadata.name) && params.metadata.name == %q`, ReleaseActivationName),
		fmt.Sprintf(`has(params.metadata.namespace) && params.metadata.namespace == %q`, c.namespace),
		`has(params.metadata.uid) && params.metadata.uid != ""`,
		`has(params.metadata.resourceVersion) && params.metadata.resourceVersion != ""`,
		`has(params.data) && params.data.size() == 4`,
		fmt.Sprintf(`%q in params.data && params.data[%q].matches("^(0|[1-9][0-9]*)$")`, activeReleaseDataKey, activeReleaseDataKey),
		fmt.Sprintf(`%q in params.data && params.data[%q] == %q`, controllerCredentialsDataKey, controllerCredentialsDataKey, ControllerCredentialsDraining),
		fmt.Sprintf(`%q in params.data && params.data[%q].matches("^[1-9][0-9]*$")`, controllerCredentialsTargetDataKey, controllerCredentialsTargetDataKey),
		fmt.Sprintf(`%q in params.data && params.data[%q].matches("^[0-9a-f]{64}$")`, controllerCredentialsAttemptDataKey, controllerCredentialsAttemptDataKey),
	}, " && ")
	username := fmt.Sprintf(
		`request.userInfo.username == %q + params.data[%q] + "-" + params.data[%q].substring(0, 12)`,
		"system:serviceaccount:"+c.namespace+":"+c.hookServiceAccountBase+"-cleanup-v",
		controllerCredentialsTargetDataKey,
		controllerCredentialsAttemptDataKey,
	)
	return fmt.Sprintf(`(%s) && (%s)`, parameterShape, exactServiceAccountUsernamePrincipalExpression(c.namespace, username))
}

func (c stagingSecretContract) secretShape(path string, requireLiveIdentity bool) string {
	liveIdentity := ""
	if requireLiveIdentity {
		liveIdentity = fmt.Sprintf(` && has(%[1]s.metadata.uid) && %[1]s.metadata.uid != "" && has(%[1]s.metadata.resourceVersion) && %[1]s.metadata.resourceVersion != ""`, path)
	}
	return fmt.Sprintf(
		`%[1]s != null && has(%[1]s.apiVersion) && %[1]s.apiVersion == "v1" && has(%[1]s.kind) && %[1]s.kind == "Secret" && has(%[1]s.metadata) && has(%[1]s.metadata.name) && %[1]s.metadata.name == %[2]q && has(%[1]s.metadata.namespace) && %[1]s.metadata.namespace == %[3]q && (!has(%[1]s.metadata.generateName) || %[1]s.metadata.generateName == "")%[4]s && has(%[1]s.metadata.labels) && %[1]s.metadata.labels == {%[5]q: %[6]q, "app.kubernetes.io/managed-by": "Helm"} && has(%[1]s.metadata.annotations) && %[1]s.metadata.annotations == {"meta.helm.sh/release-name": %[7]q, "meta.helm.sh/release-namespace": %[3]q} && (!has(%[1]s.metadata.ownerReferences) || %[1]s.metadata.ownerReferences.size() == 0) && (!has(%[1]s.metadata.finalizers) || %[1]s.metadata.finalizers.size() == 0) && !has(%[1]s.metadata.deletionTimestamp) && !has(%[1]s.metadata.deletionGracePeriodSeconds) && has(dyn(%[1]s).type) && dyn(%[1]s).type == "Opaque" && !has(%[1]s.immutable) && (!has(dyn(%[1]s).stringData) || dyn(%[1]s).stringData.size() == 0)`,
		path,
		c.stagingSecretName,
		c.namespace,
		liveIdentity,
		certrotation.StagingSecretLabel,
		certrotation.StagingSecretLabelValue,
		c.releaseName,
	)
}

func (c stagingSecretContract) verifyLiveSecret(secret *corev1.Secret) error {
	if secret == nil {
		return errors.New("staging Secret API returned a nil object")
	}
	if secret.APIVersion != "" && secret.APIVersion != "v1" {
		return fmt.Errorf("staging Secret has unexpected API version %q", secret.APIVersion)
	}
	if secret.Kind != "" && secret.Kind != "Secret" {
		return fmt.Errorf("staging Secret has unexpected kind %q", secret.Kind)
	}
	if secret.Name != c.stagingSecretName || secret.Namespace != c.namespace || secret.GenerateName != "" {
		return errors.New("staging Secret has an unexpected object identity")
	}
	if secret.UID == "" || secret.ResourceVersion == "" {
		return errors.New("staging Secret has no persisted identity")
	}
	if secret.DeletionTimestamp != nil || secret.DeletionGracePeriodSeconds != nil || len(secret.OwnerReferences) != 0 || len(secret.Finalizers) != 0 {
		return errors.New("staging Secret has unsafe lifecycle metadata")
	}
	if !reflect.DeepEqual(secret.Labels, map[string]string{
		certrotation.StagingSecretLabel: certrotation.StagingSecretLabelValue,
		certrotation.HelmManagedByLabel: certrotation.HelmManagedByLabelValue,
	}) || !reflect.DeepEqual(secret.Annotations, map[string]string{
		certrotation.HelmReleaseNameAnnotation:      c.releaseName,
		certrotation.HelmReleaseNamespaceAnnotation: c.namespace,
	}) {
		return errors.New("staging Secret has foreign or incomplete ownership metadata")
	}
	if secret.Type != corev1.SecretTypeOpaque || secret.Immutable != nil || len(secret.StringData) != 0 {
		return errors.New("staging Secret has an unsafe type or write-only fields")
	}
	return nil
}

func exactServiceAccountPrincipalExpression(namespace, serviceAccount string) string {
	return exactServiceAccountUsernamePrincipalExpression(
		namespace,
		fmt.Sprintf("request.userInfo.username == %q", "system:serviceaccount:"+namespace+":"+serviceAccount),
	)
}

func exactServiceAccountUsernamePrincipalExpression(namespace, usernameExpression string) string {
	return fmt.Sprintf(
		`(%s) && request.userInfo.groups.size() == 3 && "system:serviceaccounts" in request.userInfo.groups && %q in request.userInfo.groups && "system:authenticated" in request.userInfo.groups`,
		usernameExpression,
		"system:serviceaccounts:"+namespace,
	)
}

func (g *StagingSecretGuard) matchResources(stagingSecretName string) *admissionregistrationv1.MatchResources {
	exact := admissionregistrationv1.Exact
	return &admissionregistrationv1.MatchResources{
		MatchPolicy:       &exact,
		NamespaceSelector: &metav1.LabelSelector{},
		ObjectSelector:    &metav1.LabelSelector{},
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update, admissionregistrationv1.Delete},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"secrets"},
					Scope:       scopePtr(admissionregistrationv1.NamespacedScope),
				},
			},
			ResourceNames: []string{stagingSecretName},
		}},
	}
}

func (g *StagingSecretGuard) metadata(name, weight string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name,
		Annotations: map[string]string{
			"helm.sh/hook":                "pre-install,pre-upgrade",
			"helm.sh/hook-weight":         weight,
			"helm.sh/resource-policy":     "keep",
			rolloutGuardVersionAnnotation: rolloutGuardVersion,
			ReleaseNameAnnotation:         g.rollout.ReleaseName,
			ReleaseNamespaceAnnotation:    g.rollout.ReleaseNamespace,
		},
		Labels: map[string]string{
			managedByLabel:                rolloutGuardManagedBy,
			"app.kubernetes.io/instance":  g.rollout.ReleaseName,
			"app.kubernetes.io/component": stagingSecretGuardComponent,
		},
	}
}

func (g *StagingSecretGuard) verifyPolicy(actual, expected *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	if actual == nil {
		return errors.New("staging Secret guard policy is missing")
	}
	// The Go type is the identity. A typed client-go read clears TypeMeta,
	// so comparing it against a compiled expectation that sets it can only
	// ever fail against a real API server, and comparing it against one
	// that does not set it can never fail at all. The value arrived from
	// the endpoint this reader addresses; nothing else can be behind it.
	if err := verifyStagingSecretGuardMetadata("policy", actual.ObjectMeta, expected.ObjectMeta); err != nil {
		return err
	}
	if mismatch := serviceAccountObjectPolicySpecMismatch(actual.Spec, expected.Spec); mismatch != "" {
		return fmt.Errorf("staging Secret guard policy %s does not match its immutable contract: %s", expected.Name, mismatch)
	}
	return nil
}

func (g *StagingSecretGuard) verifyBinding(actual, expected *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
	if actual == nil {
		return errors.New("staging Secret guard binding is missing")
	}
	// The Go type is the identity. A typed client-go read clears TypeMeta,
	// so comparing it against a compiled expectation that sets it can only
	// ever fail against a real API server, and comparing it against one
	// that does not set it can never fail at all. The value arrived from
	// the endpoint this reader addresses; nothing else can be behind it.
	if err := verifyStagingSecretGuardMetadata("binding", actual.ObjectMeta, expected.ObjectMeta); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual.Spec, expected.Spec) {
		return fmt.Errorf("staging Secret guard binding %s does not match its immutable contract", expected.Name)
	}
	return nil
}

func verifyStagingSecretGuardMetadata(kind string, actual, expected metav1.ObjectMeta) error {
	if actual.Name != expected.Name || actual.Namespace != "" || actual.GenerateName != "" {
		return fmt.Errorf("staging Secret guard %s has an unexpected name", kind)
	}
	if actual.UID == "" || actual.ResourceVersion == "" {
		return fmt.Errorf("staging Secret guard %s has no persisted identity", kind)
	}
	if !reflect.DeepEqual(actual.Annotations, expected.Annotations) || !reflect.DeepEqual(actual.Labels, expected.Labels) {
		return fmt.Errorf("staging Secret guard %s has foreign or incomplete ownership", kind)
	}
	if len(actual.OwnerReferences) != 0 || len(actual.Finalizers) != 0 || actual.DeletionTimestamp != nil || actual.DeletionGracePeriodSeconds != nil {
		return fmt.Errorf("staging Secret guard %s has unsafe lifecycle metadata", kind)
	}
	return nil
}

func (g *StagingSecretGuard) validate(requireReaders bool) error {
	if g == nil || g.rollout == nil {
		return errors.New("staging Secret guard rollout identity is required")
	}
	if !g.rollout.CertificateRuntimeEnabled {
		return errors.New("staging Secret guard requires built-in certificate rotation")
	}
	if err := g.rollout.validateIdentity(); err != nil {
		return fmt.Errorf("validate staging Secret guard identity: %w", err)
	}
	if requireReaders && (g.rollout.Policies == nil || g.rollout.Bindings == nil) {
		return errors.New("staging Secret guard policy and binding readers are required")
	}
	if g.rollout.PollEvery <= 0 {
		return errors.New("staging Secret guard poll interval must be positive")
	}
	return nil
}
