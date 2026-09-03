package crdupgrade

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	serviceAccountOriginGuardNamePrefix = "ptah-operator-service-account-origin-guard-v1-"
	serviceAccountOriginGuardComponent  = "service-account-origin-guard"
	serviceAccountOriginPolicyWeight    = "-129"
	serviceAccountOriginBindingWeight   = "-128"

	serviceAccountPodNameExtra = "authentication.kubernetes.io/pod-name"
	serviceAccountPodUIDExtra  = "authentication.kubernetes.io/pod-uid"
)

// ServiceAccountOriginGuardPolicyName returns the versioned name of the
// release-owned boundary around the operator's privileged ServiceAccounts.
// It excludes the release sequence so one contract version protects every
// compatible hook identity; incompatible future contracts use a new name and
// retire this version only after replacement enforcement is proven.
func ServiceAccountOriginGuardPolicyName(releaseNamespace, releaseName string) string {
	identity := releaseNamespace + "\n" + releaseName
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
	return serviceAccountOriginGuardNamePrefix + digest[:12]
}

func serviceAccountOriginGuardDenialMessage() string {
	return "Ptah service account origin guard rejected a request without workload-bound identity"
}

// ServiceAccountTokenRequester is the narrow serviceaccounts/token API used
// for the live admission-enforcement proof. A typed core ServiceAccount client
// implements this interface directly.
type ServiceAccountTokenRequester interface {
	CreateToken(context.Context, string, *authenticationv1.TokenRequest, metav1.CreateOptions) (*authenticationv1.TokenRequest, error)
}

// ServiceAccountOriginGuard rejects privileged operator identities unless the
// authenticator proves that their token is bound to an expected Pod. It also
// prevents users from minting a token for any protected ServiceAccount: only a
// kubelet may request such a token, and it must bind that token to an expected
// workload Pod.
type ServiceAccountOriginGuard struct {
	Policies                      ValidatingAdmissionPolicyReader
	Bindings                      ValidatingAdmissionPolicyBindingReader
	TokenRequests                 ServiceAccountTokenRequester
	ReleaseName                   string
	ReleaseNamespace              string
	HookServiceAccountName        string
	ControllerServiceAccountName  string
	CertificateServiceAccountName string
	ControllerDeploymentName      string
	CertificateDeploymentName     string
	ReleaseSequence               int32
	ManagerImage                  string
	PollEvery                     time.Duration
}

// NewServiceAccountOriginGuard copies the immutable identity fields from a
// rollout guard. The certificate rotator currently uses its Deployment name
// as its ServiceAccount name; keeping the two fields separate in this type
// makes that security contract explicit.
func NewServiceAccountOriginGuard(rollout *RolloutGuard, tokenRequests ServiceAccountTokenRequester) *ServiceAccountOriginGuard {
	if rollout == nil {
		return nil
	}
	return &ServiceAccountOriginGuard{
		Policies:                      rollout.Policies,
		Bindings:                      rollout.Bindings,
		TokenRequests:                 tokenRequests,
		ReleaseName:                   rollout.ReleaseName,
		ReleaseNamespace:              rollout.ReleaseNamespace,
		HookServiceAccountName:        rollout.HookServiceAccountName,
		ControllerServiceAccountName:  rollout.ControllerServiceAccountName,
		CertificateServiceAccountName: rollout.CertificateDeploymentName,
		ControllerDeploymentName:      rollout.ControllerDeploymentName,
		CertificateDeploymentName:     rollout.CertificateDeploymentName,
		ReleaseSequence:               rollout.ReleaseSequence,
		ManagerImage:                  rollout.ManagerImage,
		PollEvery:                     rollout.PollEvery,
	}
}

// Verify requires the retained policy and binding to match the compiled
// release identity exactly. It is safe to call without a TokenRequest client.
func (g *ServiceAccountOriginGuard) Verify(ctx context.Context) error {
	if err := g.validate(false); err != nil {
		return err
	}
	name := ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	policy, err := g.Policies.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get service account origin guard policy: %w", err)
	}
	if err := g.verifyPolicy(policy); err != nil {
		return err
	}
	binding, err := g.Bindings.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get service account origin guard binding: %w", err)
	}
	return g.verifyBinding(binding)
}

// Prepare verifies the retained contract, waits for CEL type checking, and
// then proves that the live API server rejects an unbound TokenRequest for the
// exact candidate hook ServiceAccount. The returned token is never inspected
// or retained if an API server accepts the dry-run while its admission cache
// is still converging.
func (g *ServiceAccountOriginGuard) Prepare(ctx context.Context) error {
	if err := g.validate(true); err != nil {
		return err
	}
	if err := g.Verify(ctx); err != nil {
		return err
	}
	if err := g.waitPolicyReady(ctx); err != nil {
		return err
	}
	return g.waitUnboundTokenRequestDenied(ctx)
}

func (g *ServiceAccountOriginGuard) policy() (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	hookBase, err := g.hookServiceAccountBase()
	if err != nil {
		return nil, err
	}
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	denial := serviceAccountOriginGuardDenialMessage()
	hookServiceAccountPattern := "^" + regexp.QuoteMeta(hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	hookUsernamePattern := "^" + regexp.QuoteMeta("system:serviceaccount:"+g.ReleaseNamespace+":"+hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	hookPodPattern := "^" + regexp.QuoteMeta(hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}-`
	teardownServiceAccountPattern := "^" + regexp.QuoteMeta(hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	teardownUsernamePattern := "^" + regexp.QuoteMeta("system:serviceaccount:"+g.ReleaseNamespace+":"+hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	teardownPodPattern := "^" + regexp.QuoteMeta(hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}-`
	hookIdentityPodPattern := `^ptah-hook-identity-v[1-9][0-9]*-[0-9a-f]{12}-`
	controllerUsername := "system:serviceaccount:" + g.ReleaseNamespace + ":" + g.ControllerServiceAccountName
	certificateUsername := "system:serviceaccount:" + g.ReleaseNamespace + ":" + g.CertificateServiceAccountName

	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(name),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
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
							APIGroups:   []string{"*"},
							APIVersions: []string{"*"},
							Resources:   []string{"*/*"},
							Scope:       scopePtr(admissionregistrationv1.AllScopes),
						},
					},
				}},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name: "protected-service-account-origin",
				Expression: fmt.Sprintf(
					`request.userInfo.username in [%q, %q] || request.userInfo.username.matches(%q) || request.userInfo.username.matches(%q) || (request.operation == "CREATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "serviceaccounts" && request.subResource == "token" && request.namespace == %q && (request.name in [%q, %q] || request.name.matches(%q) || request.name.matches(%q)))`,
					controllerUsername,
					certificateUsername,
					hookUsernamePattern,
					teardownUsernamePattern,
					g.ReleaseNamespace,
					g.ControllerServiceAccountName,
					g.CertificateServiceAccountName,
					hookServiceAccountPattern,
					teardownServiceAccountPattern,
				),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "isTokenRequest", Expression: `request.operation == "CREATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "serviceaccounts" && request.subResource == "token"`},
				{Name: "isHookCaller", Expression: fmt.Sprintf(`request.userInfo.username.matches(%q) || request.userInfo.username.matches(%q)`, hookUsernamePattern, teardownUsernamePattern)},
				{Name: "isControllerCaller", Expression: fmt.Sprintf(`request.userInfo.username == %q`, controllerUsername)},
				{Name: "isCertificateCaller", Expression: fmt.Sprintf(`request.userInfo.username == %q`, certificateUsername)},
				{Name: "isProtectedCaller", Expression: `variables.isHookCaller || variables.isControllerCaller || variables.isCertificateCaller`},
				{Name: "isProtectedTokenRequest", Expression: fmt.Sprintf(`variables.isTokenRequest && request.namespace == %q && (request.name in [%q, %q] || request.name.matches(%q) || request.name.matches(%q))`, g.ReleaseNamespace, g.ControllerServiceAccountName, g.CertificateServiceAccountName, hookServiceAccountPattern, teardownServiceAccountPattern)},
				{Name: "callerPodName", Expression: fmt.Sprintf(`has(request.userInfo.extra) && %q in request.userInfo.extra && request.userInfo.extra[%q].size() == 1 ? request.userInfo.extra[%q][0] : ""`, serviceAccountPodNameExtra, serviceAccountPodNameExtra, serviceAccountPodNameExtra)},
				{Name: "callerPodUID", Expression: fmt.Sprintf(`has(request.userInfo.extra) && %q in request.userInfo.extra && request.userInfo.extra[%q].size() == 1 ? request.userInfo.extra[%q][0] : ""`, serviceAccountPodUIDExtra, serviceAccountPodUIDExtra, serviceAccountPodUIDExtra)},
			},
			Validations: []admissionregistrationv1.Validation{
				{
					Expression: fmt.Sprintf(
						`!variables.isProtectedCaller || (variables.callerPodName != "" && variables.callerPodUID != "" && ((variables.isHookCaller && (variables.callerPodName.matches(%q) || variables.callerPodName.matches(%q) || variables.callerPodName.matches(%q))) || (variables.isControllerCaller && variables.callerPodName.startsWith(%q)) || (variables.isCertificateCaller && variables.callerPodName.startsWith(%q))))`,
						hookPodPattern,
						hookIdentityPodPattern,
						teardownPodPattern,
						g.ControllerDeploymentName+"-",
						g.CertificateDeploymentName+"-",
					),
					Message: denial,
				},
				{
					Expression: fmt.Sprintf(
						`!variables.isProtectedTokenRequest || (request.userInfo.username.matches("^system:node:.+$") && request.userInfo.groups.filter(group, group == "system:nodes").size() == 1 && has(object.spec.boundObjectRef) && has(object.spec.boundObjectRef.apiVersion) && object.spec.boundObjectRef.apiVersion == "v1" && has(object.spec.boundObjectRef.kind) && object.spec.boundObjectRef.kind == "Pod" && has(object.spec.boundObjectRef.name) && object.spec.boundObjectRef.name != "" && has(object.spec.boundObjectRef.uid) && object.spec.boundObjectRef.uid != "" && ((request.name == %q && object.spec.boundObjectRef.name.startsWith(%q)) || (request.name == %q && object.spec.boundObjectRef.name.startsWith(%q)) || (request.name.matches(%q) && (object.spec.boundObjectRef.name.matches(%q) || object.spec.boundObjectRef.name.matches(%q))) || (request.name.matches(%q) && object.spec.boundObjectRef.name.matches(%q))))`,
						g.ControllerServiceAccountName,
						g.ControllerDeploymentName+"-",
						g.CertificateServiceAccountName,
						g.CertificateDeploymentName+"-",
						hookServiceAccountPattern,
						hookPodPattern,
						hookIdentityPodPattern,
						teardownServiceAccountPattern,
						teardownPodPattern,
					),
					Message: denial,
				},
			},
		},
	}, nil
}

func (g *ServiceAccountOriginGuard) binding() *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	name := ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.metadata(name),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        name,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
}

func (g *ServiceAccountOriginGuard) metadata(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name,
		Annotations: map[string]string{
			rolloutGuardVersionAnnotation: rolloutGuardVersion,
			ReleaseNameAnnotation:         g.ReleaseName,
			ReleaseNamespaceAnnotation:    g.ReleaseNamespace,
		},
		Labels: map[string]string{
			managedByLabel:                rolloutGuardManagedBy,
			instanceLabel:                 g.ReleaseName,
			"app.kubernetes.io/component": serviceAccountOriginGuardComponent,
		},
	}
}

func (g *ServiceAccountOriginGuard) verifyPolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	name := ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	if policy == nil || policy.Name != name {
		return fmt.Errorf("fixed service account origin guard policy %s is missing", name)
	}
	if err := g.verifyMetadata("ValidatingAdmissionPolicy", policy.ObjectMeta); err != nil {
		return err
	}
	expected, err := g.policy()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(policy.Spec, expected.Spec) {
		return fmt.Errorf("service account origin guard policy spec differs from the immutable contract")
	}
	return nil
}

func (g *ServiceAccountOriginGuard) verifyBinding(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
	name := ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	if binding == nil || binding.Name != name {
		return fmt.Errorf("fixed service account origin guard binding %s is missing", name)
	}
	if err := g.verifyMetadata("ValidatingAdmissionPolicyBinding", binding.ObjectMeta); err != nil {
		return err
	}
	if !reflect.DeepEqual(binding.Spec, g.binding().Spec) {
		return fmt.Errorf("service account origin guard binding spec differs from the immutable contract")
	}
	return nil
}

func (g *ServiceAccountOriginGuard) verifyMetadata(kind string, metadata metav1.ObjectMeta) error {
	expected := g.metadata(ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName))
	if metadata.Name != expected.Name {
		return fmt.Errorf("fixed service account origin guard %s has an unexpected name", kind)
	}
	for key, value := range expected.Annotations {
		if metadata.Annotations[key] != value {
			return fmt.Errorf("fixed service account origin guard %s has foreign or incomplete ownership", kind)
		}
	}
	for key, value := range expected.Labels {
		if metadata.Labels[key] != value {
			return fmt.Errorf("fixed service account origin guard %s has foreign or incomplete ownership", kind)
		}
	}
	return nil
}

func (g *ServiceAccountOriginGuard) waitPolicyReady(ctx context.Context) error {
	name := ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	return wait.PollUntilContextCancel(ctx, g.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		policy, err := g.Policies.Get(pollCtx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("read service account origin guard policy status: %w", err)
		}
		if err := g.verifyPolicy(policy); err != nil {
			return false, err
		}
		if policy.Status.ObservedGeneration != policy.Generation || policy.Status.TypeChecking == nil {
			return false, nil
		}
		if warnings := policy.Status.TypeChecking.ExpressionWarnings; len(warnings) != 0 {
			return false, fmt.Errorf("service account origin guard policy has CEL type-check warnings: %s", warnings[0].Warning)
		}
		return true, nil
	})
}

func (g *ServiceAccountOriginGuard) waitUnboundTokenRequestDenied(ctx context.Context) error {
	return wait.PollUntilContextCancel(ctx, g.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		request := &authenticationv1.TokenRequest{
			TypeMeta: metav1.TypeMeta{APIVersion: authenticationv1.SchemeGroupVersion.String(), Kind: "TokenRequest"},
		}
		response, err := g.TokenRequests.CreateToken(
			pollCtx,
			g.HookServiceAccountName,
			request,
			metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}},
		)
		if response != nil {
			// Do not retain an opaque credential if an API server accepts the
			// request while its admission cache is still converging.
			response.Status.Token = ""
		}
		if err == nil {
			return false, nil
		}
		if strings.Contains(err.Error(), serviceAccountOriginGuardDenialMessage()) {
			return true, nil
		}
		if serviceAccountOriginAdmissionMayBePropagating(err) {
			return false, nil
		}
		return false, fmt.Errorf("probe service account origin guard enforcement: %w", err)
	})
}

func serviceAccountOriginAdmissionMayBePropagating(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "validatingadmissionpolicy") ||
		strings.Contains(message, "validating admission policy")
}

func (g *ServiceAccountOriginGuard) validate(requireTokenRequests bool) error {
	if g == nil || g.Policies == nil || g.Bindings == nil {
		return fmt.Errorf("service account origin guard policy clients are required")
	}
	if requireTokenRequests && g.TokenRequests == nil {
		return fmt.Errorf("service account origin guard TokenRequest client is required")
	}
	for name, value := range map[string]string{
		"release name":                     g.ReleaseName,
		"release namespace":                g.ReleaseNamespace,
		"hook service account name":        g.HookServiceAccountName,
		"controller service account name":  g.ControllerServiceAccountName,
		"certificate service account name": g.CertificateServiceAccountName,
		"controller Deployment name":       g.ControllerDeploymentName,
		"certificate Deployment name":      g.CertificateDeploymentName,
	} {
		if value == "" {
			return fmt.Errorf("service account origin guard %s is required", name)
		}
	}
	if g.ReleaseSequence < 1 {
		return fmt.Errorf("service account origin guard release sequence must be positive")
	}
	if g.ManagerImage == "" || strings.IndexFunc(g.ManagerImage, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }) >= 0 {
		return fmt.Errorf("service account origin guard manager image is empty or contains whitespace")
	}
	if g.PollEvery <= 0 {
		return fmt.Errorf("service account origin guard poll interval must be positive")
	}
	protectedNames := []string{g.HookServiceAccountName, g.ControllerServiceAccountName, g.CertificateServiceAccountName}
	for left := range protectedNames {
		for right := left + 1; right < len(protectedNames); right++ {
			if protectedNames[left] == protectedNames[right] {
				return fmt.Errorf("service account origin guard protected ServiceAccount names must differ")
			}
		}
	}
	if g.ControllerDeploymentName == g.CertificateDeploymentName {
		return fmt.Errorf("service account origin guard workload names must differ")
	}
	_, err := g.hookServiceAccountBase()
	return err
}

func (g *ServiceAccountOriginGuard) hookServiceAccountBase() (string, error) {
	if g == nil || g.ReleaseSequence < 1 || g.ReleaseNamespace == "" || g.ReleaseName == "" || g.ManagerImage == "" || g.HookServiceAccountName == "" {
		return "", fmt.Errorf("service account origin guard hook identity is incomplete")
	}
	suffix := "-crd-v" + strconv.FormatInt(int64(g.ReleaseSequence), 10) + "-" + hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)[:12]
	if !strings.HasSuffix(g.HookServiceAccountName, suffix) {
		return "", fmt.Errorf("service account origin guard hook ServiceAccount does not match the candidate release identity")
	}
	base := strings.TrimSuffix(g.HookServiceAccountName, suffix)
	if base == "" {
		return "", fmt.Errorf("service account origin guard hook ServiceAccount has no stable name prefix")
	}
	return base, nil
}
