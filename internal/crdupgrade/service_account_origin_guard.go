package crdupgrade

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	serviceAccountOriginGuardNamePrefix = "ptah-operator-service-account-origin-guard-v2-"
	serviceAccountOriginGuardComponent  = "service-account-origin-guard"
	serviceAccountOriginPolicyWeight    = "-129"
	serviceAccountOriginBindingWeight   = "-128"

	serviceAccountPodNameExtra = "authentication.kubernetes.io/pod-name"
	serviceAccountPodUIDExtra  = "authentication.kubernetes.io/pod-uid"
)

// ServiceAccountOriginGuardPolicyName returns the versioned name of the
// release-owned boundary around the operator's privileged ServiceAccounts.
// Each controller release retains its own immutable policy so the candidate
// and predecessor principals overlap safely until the activation cutover.
func ServiceAccountOriginGuardPolicyName(releaseNamespace, releaseName string, releaseSequence int32, managerImage string) string {
	return serviceAccountOriginGuardNamePrefix + controllerPrincipalGuardDigest(releaseNamespace, releaseName, releaseSequence, managerImage)
}

func serviceAccountOriginGuardDenialMessage() string {
	return "Ptah service account origin guard rejected a request without workload-bound identity"
}

// ServiceAccountOriginGuard rejects privileged operator identities unless the
// authenticator proves that their token is bound to an expected Pod. It also
// prevents users from minting a token for any protected ServiceAccount: only a
// kubelet may request such a token, and it must bind that token to an expected
// workload Pod.
type ServiceAccountOriginGuard struct {
	Policies                                ValidatingAdmissionPolicyReader
	Bindings                                ValidatingAdmissionPolicyBindingReader
	ReleaseName                             string
	ReleaseNamespace                        string
	HookServiceAccountName                  string
	ControllerServiceAccountName            string
	ControllerServiceAccountManaged         bool
	PreviousControllerServiceAccountName    string
	PreviousControllerServiceAccountUID     types.UID
	PreviousControllerServiceAccountManaged bool
	PreviousControllerReleaseSequence       int32
	CertificateServiceAccountName           string
	ControllerDeploymentName                string
	CertificateDeploymentName               string
	ControllerStateVersion                  int32
	AdmissionContractVersion                int32
	ReleaseSequence                         int32
	ManagerImage                            string
	PollEvery                               time.Duration
}

// NewServiceAccountOriginGuard copies the immutable identity fields from a
// rollout guard. The certificate rotator currently uses its Deployment name
// as its ServiceAccount name; keeping the two fields separate in this type
// makes that security contract explicit.
func NewServiceAccountOriginGuard(rollout *RolloutGuard) *ServiceAccountOriginGuard {
	if rollout == nil {
		return nil
	}
	return &ServiceAccountOriginGuard{
		Policies:                                rollout.Policies,
		Bindings:                                rollout.Bindings,
		ReleaseName:                             rollout.ReleaseName,
		ReleaseNamespace:                        rollout.ReleaseNamespace,
		HookServiceAccountName:                  rollout.HookServiceAccountName,
		ControllerServiceAccountName:            rollout.ControllerServiceAccountName,
		ControllerServiceAccountManaged:         rollout.ControllerServiceAccountManaged,
		PreviousControllerServiceAccountName:    rollout.PreviousControllerServiceAccountName,
		PreviousControllerServiceAccountUID:     rollout.PreviousControllerServiceAccountUID,
		PreviousControllerServiceAccountManaged: rollout.PreviousControllerServiceAccountManaged,
		PreviousControllerReleaseSequence:       rollout.PreviousControllerReleaseSequence,
		CertificateServiceAccountName:           rollout.CertificateDeploymentName,
		ControllerDeploymentName:                rollout.ControllerDeploymentName,
		CertificateDeploymentName:               rollout.CertificateDeploymentName,
		ControllerStateVersion:                  rollout.ControllerStateVersion,
		AdmissionContractVersion:                rollout.AdmissionContractVersion,
		ReleaseSequence:                         rollout.ReleaseSequence,
		ManagerImage:                            rollout.ManagerImage,
		PollEvery:                               rollout.PollEvery,
	}
}

// Verify requires the retained policy and binding to match the compiled
// release identity exactly.
func (g *ServiceAccountOriginGuard) Verify(ctx context.Context) error {
	if err := g.validate(); err != nil {
		return err
	}
	name := ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
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

// Prepare verifies the retained contract and waits for CEL type checking.
// It deliberately never submits a TokenRequest: that API can generate a real
// bearer credential even when a client supplies dry-run options. The final
// inert admission sentinel independently carries and directly proves the
// controller TokenRequest phase fence on every API server.
func (g *ServiceAccountOriginGuard) Prepare(ctx context.Context) error {
	if err := g.validate(); err != nil {
		return err
	}
	if err := g.Verify(ctx); err != nil {
		return err
	}
	return g.waitPolicyReady(ctx)
}

func (g *ServiceAccountOriginGuard) policy() (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	hookBase, err := g.hookServiceAccountBase()
	if err != nil {
		return nil, err
	}
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	denial := serviceAccountOriginGuardDenialMessage()
	hookServiceAccountPattern := "^" + regexp.QuoteMeta(hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	hookUsernamePattern := "^" + regexp.QuoteMeta("system:serviceaccount:"+g.ReleaseNamespace+":"+hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	hookPodPattern := "^" + regexp.QuoteMeta(hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}-`
	teardownServiceAccountPattern := "^" + regexp.QuoteMeta(hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	teardownUsernamePattern := "^" + regexp.QuoteMeta("system:serviceaccount:"+g.ReleaseNamespace+":"+hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	teardownPodPattern := "^" + regexp.QuoteMeta(hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}-`
	quiescePodPattern := "^" + regexp.QuoteMeta(hookBase+"-quiesce-v") + `[1-9][0-9]*-[0-9a-f]{12}-`
	hookIdentityPodPattern := `^ptah-hook-identity-v[1-9][0-9]*-[0-9a-f]{12}-`
	controllerUsernameMatch := controllerPrincipalMatchExpression(g.ReleaseNamespace, g.ControllerServiceAccountName, g.PreviousControllerServiceAccountName)
	controllerNames := []string{g.ControllerServiceAccountName}
	if g.PreviousControllerServiceAccountName != "" && g.PreviousControllerServiceAccountName != g.ControllerServiceAccountName {
		controllerNames = append(controllerNames, g.PreviousControllerServiceAccountName)
	}
	quotedControllerNames := make([]string, len(controllerNames))
	for index, serviceAccountName := range controllerNames {
		quotedControllerNames[index] = strconv.Quote(serviceAccountName)
	}
	controllerNameMatch := `request.name in [` + strings.Join(quotedControllerNames, ", ") + `]`
	controllerTokenAuthority := fmt.Sprintf(`request.name == %q && variables.activeRelease == %d`, g.ControllerServiceAccountName, g.ReleaseSequence)
	if g.PreviousControllerServiceAccountName != "" {
		controllerTokenAuthority = fmt.Sprintf(
			`(%s) || (request.name == %q && variables.activeRelease == %d)`,
			controllerTokenAuthority,
			g.PreviousControllerServiceAccountName,
			g.PreviousControllerReleaseSequence,
		)
	}
	certificateUsername := "system:serviceaccount:" + g.ReleaseNamespace + ":" + g.CertificateServiceAccountName

	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(name),
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
					`(%s) || request.userInfo.username == %q || request.userInfo.username.matches(%q) || request.userInfo.username.matches(%q) || (request.operation == "CREATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "serviceaccounts" && has(request.subResource) && request.subResource == "token" && request.namespace == %q && ((%s) || request.name == %q || request.name.matches(%q) || request.name.matches(%q)))`,
					controllerUsernameMatch,
					certificateUsername,
					hookUsernamePattern,
					teardownUsernamePattern,
					g.ReleaseNamespace,
					controllerNameMatch,
					g.CertificateServiceAccountName,
					hookServiceAccountPattern,
					teardownServiceAccountPattern,
				),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "activeRelease", Expression: decimalCEL("params", activeReleaseDataKey, true)},
				{Name: "controllerCredentialPhase", Expression: stringDataCEL("params", controllerCredentialsDataKey)},
				{Name: "isTokenRequest", Expression: `request.operation == "CREATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "serviceaccounts" && has(request.subResource) && request.subResource == "token"`},
				{Name: "isControllerTokenRequest", Expression: fmt.Sprintf(`variables.isTokenRequest && request.namespace == %q && (%s)`, g.ReleaseNamespace, controllerNameMatch)},
				{Name: "isHookCaller", Expression: fmt.Sprintf(`request.userInfo.username.matches(%q) || request.userInfo.username.matches(%q)`, hookUsernamePattern, teardownUsernamePattern)},
				{Name: "isControllerCaller", Expression: controllerUsernameMatch},
				{Name: "isCertificateCaller", Expression: fmt.Sprintf(`request.userInfo.username == %q`, certificateUsername)},
				{Name: "isProtectedCaller", Expression: `variables.isHookCaller || variables.isControllerCaller || variables.isCertificateCaller`},
				{Name: "isProtectedTokenRequest", Expression: fmt.Sprintf(`variables.isTokenRequest && request.namespace == %q && ((%s) || request.name == %q || request.name.matches(%q) || request.name.matches(%q))`, g.ReleaseNamespace, controllerNameMatch, g.CertificateServiceAccountName, hookServiceAccountPattern, teardownServiceAccountPattern)},
				{Name: "callerPodName", Expression: fmt.Sprintf(`has(request.userInfo.extra) && %q in request.userInfo.extra && request.userInfo.extra[%q].size() == 1 ? request.userInfo.extra[%q][0] : ""`, serviceAccountPodNameExtra, serviceAccountPodNameExtra, serviceAccountPodNameExtra)},
				{Name: "callerPodUID", Expression: fmt.Sprintf(`has(request.userInfo.extra) && %q in request.userInfo.extra && request.userInfo.extra[%q].size() == 1 ? request.userInfo.extra[%q][0] : ""`, serviceAccountPodUIDExtra, serviceAccountPodUIDExtra, serviceAccountPodUIDExtra)},
			},
			Validations: []admissionregistrationv1.Validation{
				{Expression: g.activationParameterExpression(), Message: denial},
				{
					Expression: fmt.Sprintf(
						`!variables.isControllerCaller || (variables.controllerCredentialPhase == %q && (%s))`,
						ControllerCredentialsActive,
						controllerPrincipalAuthorityExpression(g.ReleaseNamespace, g.ControllerServiceAccountName, g.PreviousControllerServiceAccountName, g.ReleaseSequence, g.PreviousControllerReleaseSequence),
					),
					Message: controllerPrincipalGuardDenialMessage(),
				},
				{
					Expression: fmt.Sprintf(
						`!variables.isControllerTokenRequest || (variables.controllerCredentialPhase == %q && (%s))`,
						ControllerCredentialsActive,
						controllerTokenAuthority,
					),
					Message: controllerPrincipalGuardDenialMessage(),
				},
				{
					Expression: fmt.Sprintf(
						`!variables.isProtectedCaller || (variables.callerPodName != "" && variables.callerPodUID != "" && ((variables.isHookCaller && (variables.callerPodName.matches(%q) || variables.callerPodName.matches(%q) || variables.callerPodName.matches(%q) || variables.callerPodName.matches(%q))) || (variables.isControllerCaller && %s) || (variables.isCertificateCaller && %s)))`,
						hookPodPattern,
						hookIdentityPodPattern,
						teardownPodPattern,
						quiescePodPattern,
						runtimePodRequestNameExpression("variables.callerPodName", g.ControllerDeploymentName),
						runtimePodRequestNameExpression("variables.callerPodName", g.CertificateDeploymentName),
					),
					Message: denial,
				},
				{
					Expression: fmt.Sprintf(
						`!variables.isProtectedTokenRequest || (request.userInfo.username.matches("^system:node:.+$") && request.userInfo.groups.filter(group, group == "system:nodes").size() == 1 && has(object.spec.boundObjectRef) && has(object.spec.boundObjectRef.apiVersion) && object.spec.boundObjectRef.apiVersion == "v1" && has(object.spec.boundObjectRef.kind) && object.spec.boundObjectRef.kind == "Pod" && has(object.spec.boundObjectRef.name) && object.spec.boundObjectRef.name != "" && has(object.spec.boundObjectRef.uid) && object.spec.boundObjectRef.uid != "" && (((%s) && %s) || (request.name == %q && %s) || (request.name.matches(%q) && (object.spec.boundObjectRef.name.matches(%q) || object.spec.boundObjectRef.name.matches(%q) || object.spec.boundObjectRef.name.matches(%q))) || (request.name.matches(%q) && object.spec.boundObjectRef.name.matches(%q))))`,
						controllerNameMatch,
						runtimePodRequestNameExpression("object.spec.boundObjectRef.name", g.ControllerDeploymentName),
						g.CertificateServiceAccountName,
						runtimePodRequestNameExpression("object.spec.boundObjectRef.name", g.CertificateDeploymentName),
						hookServiceAccountPattern,
						hookPodPattern,
						hookIdentityPodPattern,
						quiescePodPattern,
						teardownServiceAccountPattern,
						teardownPodPattern,
					),
					Message: denial,
				},
			},
		},
	}
	addAdmissionConvergenceDependencyProbe(
		policy,
		g.ReleaseNamespace,
		AdmissionConvergenceMarkerName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence),
		hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage),
	)
	return policy, nil
}

func (g *ServiceAccountOriginGuard) activationParameterExpression() string {
	activation := &ReleaseActivationGuard{ReleaseName: g.ReleaseName, ReleaseNamespace: g.ReleaseNamespace}
	return activation.activationObjectShapeExpression("params")
}

func (g *ServiceAccountOriginGuard) binding() *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	name := ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	deny := admissionregistrationv1.DenyAction
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.metadata(name),
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

func (g *ServiceAccountOriginGuard) metadata(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name,
		Annotations: map[string]string{
			rolloutGuardVersionAnnotation:                     rolloutGuardVersion,
			ReleaseNameAnnotation:                             g.ReleaseName,
			ReleaseNamespaceAnnotation:                        g.ReleaseNamespace,
			ControllerStateVersionAnnotation:                  strconv.FormatInt(int64(g.ControllerStateVersion), 10),
			AdmissionContractVersionAnnotation:                strconv.FormatInt(int64(g.AdmissionContractVersion), 10),
			ReleaseSequenceAnnotation:                         strconv.FormatInt(int64(g.ReleaseSequence), 10),
			ManagerImageAnnotation:                            g.ManagerImage,
			HookServiceAccountAnnotation:                      g.HookServiceAccountName,
			ControllerServiceAccountAnnotation:                g.ControllerServiceAccountName,
			ControllerServiceAccountManagedAnnotation:         strconv.FormatBool(g.ControllerServiceAccountManaged),
			PreviousControllerServiceAccountAnnotation:        g.PreviousControllerServiceAccountName,
			PreviousControllerServiceAccountUIDAnnotation:     string(g.PreviousControllerServiceAccountUID),
			PreviousControllerServiceAccountManagedAnnotation: strconv.FormatBool(g.PreviousControllerServiceAccountManaged),
			PreviousControllerReleaseSequenceAnnotation:       strconv.FormatInt(int64(g.PreviousControllerReleaseSequence), 10),
		},
		Labels: map[string]string{
			managedByLabel:                rolloutGuardManagedBy,
			instanceLabel:                 g.ReleaseName,
			"app.kubernetes.io/component": serviceAccountOriginGuardComponent,
		},
	}
}

func (g *ServiceAccountOriginGuard) verifyPolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	name := ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
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
	name := ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
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
	expected := g.metadata(ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage))
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
	name := ServiceAccountOriginGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
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

func (g *ServiceAccountOriginGuard) validate() error {
	if g == nil || g.Policies == nil || g.Bindings == nil {
		return fmt.Errorf("service account origin guard policy clients are required")
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
	if g.ControllerStateVersion < 1 || g.AdmissionContractVersion < 1 {
		return fmt.Errorf("service account origin guard controller and admission versions must be positive")
	}
	if g.ManagerImage == "" || strings.IndexFunc(g.ManagerImage, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }) >= 0 {
		return fmt.Errorf("service account origin guard manager image is empty or contains whitespace")
	}
	if g.PreviousControllerServiceAccountName != strings.TrimSpace(g.PreviousControllerServiceAccountName) {
		return fmt.Errorf("service account origin guard previous controller ServiceAccount contains surrounding whitespace")
	}
	if g.PreviousControllerServiceAccountName == "" {
		if g.PreviousControllerServiceAccountUID != "" || g.PreviousControllerServiceAccountManaged {
			return fmt.Errorf("service account origin guard has previous controller provenance without an identity")
		}
	} else if g.PreviousControllerServiceAccountUID == "" {
		return fmt.Errorf("service account origin guard previous controller ServiceAccount UID is required")
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
