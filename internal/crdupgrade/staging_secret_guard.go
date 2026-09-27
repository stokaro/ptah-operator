package crdupgrade

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"

	"github.com/stokaro/ptah-operator/internal/certrotation"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
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
	dataPreserved := `has(dyn(object).data) == has(dyn(oldObject).data) && (!has(dyn(object).data) || dyn(object).data == dyn(oldObject).data)`
	updateIdentity := `object.metadata.uid == oldObject.metadata.uid && object.metadata.resourceVersion == oldObject.metadata.resourceVersion && has(object.metadata.creationTimestamp) == has(oldObject.metadata.creationTimestamp) && (!has(object.metadata.creationTimestamp) || object.metadata.creationTimestamp == oldObject.metadata.creationTimestamp)`

	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(name, stagingSecretGuardPolicyWeight),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy:    &fail,
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
			},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `variables.isCreate || variables.isUpdate || variables.isDelete`, Message: message},
				{Expression: `variables.isCreate || (` + oldShape + `)`, Message: message},
				{Expression: `variables.isDelete || (variables.isCreate && (` + createShape + `)) || (variables.isUpdate && (` + newShape + `))`, Message: message},
				{Expression: `!variables.isCreate || (!has(dyn(object).data) || dyn(object).data.size() == 0)`, Message: message},
				{Expression: `!variables.isUpdate || (` + updateIdentity + `)`, Message: message},
				{Expression: fmt.Sprintf(`!variables.isUpdate || variables.isRotator || (%s)`, dataPreserved), Message: message},
				// The uninstall deletes this Secret only after it has deleted
				// this guard, so nothing the guard sees may delete it.
				{Expression: `!variables.isDelete`, Message: message},
			},
		},
	}
	return policy, nil
}

// ExpectedBinding constructs the exact deny-only enforcement binding.
func (g *StagingSecretGuard) ExpectedBinding() (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	contract, err := g.contract()
	if err != nil {
		return nil, err
	}
	name := StagingSecretGuardBindingName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.metadata(name, stagingSecretGuardBindingWeight),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        StagingSecretGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName),
			MatchResources:    g.matchResources(contract.stagingSecretName),
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
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

type stagingSecretContract struct {
	namespace             string
	releaseName           string
	stagingSecretName     string
	rotatorServiceAccount string
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
	return stagingSecretContract{
		namespace:             g.rollout.ReleaseNamespace,
		releaseName:           g.rollout.ReleaseName,
		stagingSecretName:     stagingSecretName,
		rotatorServiceAccount: g.rollout.CertificateDeploymentName,
	}, nil
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
	if mismatch := policySpecMismatch(actual.Spec, expected.Spec); mismatch != "" {
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

// policySpecMismatch names the first part of a policy spec that differs from
// its compiled contract, so a refusal says what drifted.
func policySpecMismatch(actual, expected admissionregistrationv1.ValidatingAdmissionPolicySpec) string {
	if reflect.DeepEqual(actual, expected) {
		return ""
	}
	if !reflect.DeepEqual(actual.ParamKind, expected.ParamKind) {
		return "parameter kind differs"
	}
	if !reflect.DeepEqual(actual.FailurePolicy, expected.FailurePolicy) {
		return "failure policy differs"
	}
	if !reflect.DeepEqual(actual.MatchConstraints, expected.MatchConstraints) {
		return "match constraints differ"
	}
	if !reflect.DeepEqual(actual.MatchConditions, expected.MatchConditions) {
		return "match conditions differ"
	}
	if !reflect.DeepEqual(actual.Variables, expected.Variables) {
		for index := 0; index < len(actual.Variables) && index < len(expected.Variables); index++ {
			if !reflect.DeepEqual(actual.Variables[index], expected.Variables[index]) {
				return fmt.Sprintf("variable %d (%s) differs: got %q, want %q", index, expected.Variables[index].Name, actual.Variables[index].Expression, expected.Variables[index].Expression)
			}
		}
		return fmt.Sprintf("variable count differs: got %d, want %d", len(actual.Variables), len(expected.Variables))
	}
	if !reflect.DeepEqual(actual.Validations, expected.Validations) {
		for index := 0; index < len(actual.Validations) && index < len(expected.Validations); index++ {
			if !reflect.DeepEqual(actual.Validations[index], expected.Validations[index]) {
				return fmt.Sprintf("validation %d differs: got %q, want %q", index, actual.Validations[index].Expression, expected.Validations[index].Expression)
			}
		}
		return fmt.Sprintf("validation count differs: got %d, want %d", len(actual.Validations), len(expected.Validations))
	}
	if !reflect.DeepEqual(actual.AuditAnnotations, expected.AuditAnnotations) {
		return "audit annotations differ"
	}
	return "unknown field differs"
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
