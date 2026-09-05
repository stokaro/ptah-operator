package crdupgrade

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	runtimePodGuardNamePrefix          = "ptah-operator-runtime-pod-identity-v"
	runtimePodContractDigestAnnotation = "operator.ptah.dev/runtime-pod-contract-digest"
	runtimeReplicaSetHashMinLength     = 1
	runtimeReplicaSetHashMaxLength     = 10
)

// RuntimePodGuardPolicyName returns the append-only Pod identity boundary for
// one release sequence. Older policies admit only a higher activated sequence,
// whose own retained policy then validates its exact executable contract.
func RuntimePodGuardPolicyName(sequence int32) string {
	return runtimePodGuardNamePrefix + strconv.FormatInt(int64(sequence), 10)
}

func runtimePodGuardDenialMessage(sequence int32) string {
	return fmt.Sprintf("Ptah runtime Pod identity guard v%d rejected an unsafe workload", sequence)
}

type runtimePodContract struct {
	controllerArgsJSON  string
	certificateArgsJSON string
	webhookSecretName   string
	controllerPort      int64
	certificatePort     int64
	digest              string
}

// runtimePodActivationMatchExpression leaves the exact predecessor Pod path
// to its retained policy before this sequence activates. Candidate, future,
// and malformed identities still enter this policy and fail its validations.
func (g *RolloutGuard) runtimePodActivationMatchExpression() string {
	activationShape := g.releaseActivationParameterShapeExpression()
	activeRelease := decimalCEL("params", activeReleaseDataKey, true)
	bootstrapIdentity := fmt.Sprintf(
		`params.data[%q] == "0" && %s && %s`,
		activeReleaseDataKey,
		annotationAbsentExpression("object", ControllerStateVersionAnnotation),
		annotationAbsentExpression("object", ReleaseSequenceAnnotation),
	)
	activeIdentity := fmt.Sprintf(
		`params.data[%[1]q] != "0" && has(object.metadata.annotations) && %[2]q in object.metadata.annotations && object.metadata.annotations[%[2]q] == params.metadata.annotations[%[2]q] && %[3]q in object.metadata.annotations && object.metadata.annotations[%[3]q] == params.data[%[1]q]`,
		activeReleaseDataKey,
		ControllerStateVersionAnnotation,
		ReleaseSequenceAnnotation,
	)
	predecessorIdentity := fmt.Sprintf(`(%s) || (%s)`, bootstrapIdentity, activeIdentity)
	return fmt.Sprintf(
		`!(%[1]s) || (%[2]s) >= %[3]d || ((!has(request.subResource) || request.subResource == "") && !(%[4]s))`,
		activationShape,
		activeRelease,
		g.ReleaseSequence,
		predecessorIdentity,
	)
}

// runtimePodIdentityPolicy returns the immutable Pod-level executable and
// privilege boundary for the two long-lived runtime ServiceAccounts.
func (g *RolloutGuard) runtimePodIdentityPolicy() (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	contract, err := g.runtimePodContract()
	if err != nil {
		return nil, err
	}
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := RuntimePodGuardPolicyName(g.ReleaseSequence)
	metadata := g.runtimePodGuardMetadata(name, contract.digest)
	message := runtimePodGuardDenialMessage(g.ReleaseSequence)
	controllerContract := g.controllerPodContractExpressions(contract)
	certificateContract := g.certificatePodContractExpressions(contract)
	validations := []admissionregistrationv1.Validation{
		{Expression: `variables.activationValid`, Message: message},
		{Expression: `!has(request.subResource) || request.subResource == ""`, Message: message},
		{Expression: `request.operation != "CREATE" || request.userInfo.username in ["system:kube-controller-manager", "system:serviceaccount:kube-system:replicaset-controller"]`, Message: message},
		{Expression: `!variables.isPod || variables.newRelease == variables.activeRelease`, Message: message},
		{Expression: `!variables.isPod || variables.newState == variables.activeState`, Message: message},
	}
	for index := range controllerContract {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(
				`!variables.isPod || variables.newRelease != %d || (variables.newState == %d && ((variables.isController && (%s)) || (variables.isCertificate && (%s))))`,
				g.ReleaseSequence, g.ControllerStateVersion, controllerContract[index], certificateContract[index],
			),
			Message: message,
		})
	}
	for _, expression := range g.RuntimePodConfigExpressions {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: fmt.Sprintf(
				`!variables.isPod || variables.newRelease != %d || (variables.newState == %d && (%s))`,
				g.ReleaseSequence, g.ControllerStateVersion, expression,
			),
			Message: message,
		})
	}

	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
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
						Rule: admissionregistrationv1.Rule{
							APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"},
							Scope: scopePtr(admissionregistrationv1.NamespacedScope),
						},
					}},
					{RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
						Rule: admissionregistrationv1.Rule{
							APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods/ephemeralcontainers", "pods/resize"},
							Scope: scopePtr(admissionregistrationv1.NamespacedScope),
						},
					}},
					{RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Connect},
						Rule: admissionregistrationv1.Rule{
							APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods/exec", "pods/attach", "pods/portforward", "pods/proxy"},
							Scope: scopePtr(admissionregistrationv1.NamespacedScope),
						},
					}},
				},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{
				{
					Name: "runtime-service-account-or-pod",
					Expression: fmt.Sprintf(
						`request.namespace == %q && (((!has(request.subResource) || request.subResource == "") && ((has(object.spec.serviceAccountName) && object.spec.serviceAccountName in [%q, %q]) || (request.operation == "UPDATE" && has(oldObject.spec.serviceAccountName) && oldObject.spec.serviceAccountName in [%q, %q]))) || (has(request.subResource) && request.subResource != "" && (%s || %s)))`,
						g.ReleaseNamespace,
						g.ControllerServiceAccountName,
						g.CertificateDeploymentName,
						g.ControllerServiceAccountName,
						g.CertificateDeploymentName,
						runtimePodRequestNameExpression("request.name", g.ControllerDeploymentName),
						runtimePodRequestNameExpression("request.name", g.CertificateDeploymentName),
					),
				},
				{Name: "activation-gated-runtime-pod", Expression: g.runtimePodActivationMatchExpression()},
			},
			Variables: []admissionregistrationv1.Variable{
				{Name: "isPod", Expression: `!has(request.subResource) || request.subResource == ""`},
				{Name: "isController", Expression: fmt.Sprintf(`(!has(request.subResource) || request.subResource == "") && has(object.spec.serviceAccountName) && object.spec.serviceAccountName == %q`, g.ControllerServiceAccountName)},
				{Name: "isCertificate", Expression: fmt.Sprintf(`(!has(request.subResource) || request.subResource == "") && has(object.spec.serviceAccountName) && object.spec.serviceAccountName == %q`, g.CertificateDeploymentName)},
				{Name: "newState", Expression: fmt.Sprintf(`(!has(request.subResource) || request.subResource == "") && has(object.metadata.annotations) && %q in object.metadata.annotations && object.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(object.metadata.annotations[%q]) : -1`, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation)},
				{Name: "newRelease", Expression: fmt.Sprintf(`(!has(request.subResource) || request.subResource == "") && has(object.metadata.annotations) && %q in object.metadata.annotations && object.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(object.metadata.annotations[%q]) : -1`, ReleaseSequenceAnnotation, ReleaseSequenceAnnotation, ReleaseSequenceAnnotation)},
				{Name: "activationValid", Expression: g.releaseActivationParameterShapeExpression()},
				{Name: "activeRelease", Expression: fmt.Sprintf(`params != null && has(params.data) && %q in params.data && params.data[%q].matches("^(0|[1-9][0-9]*)$") ? int(params.data[%q]) : -1`, activeReleaseDataKey, activeReleaseDataKey, activeReleaseDataKey)},
				{Name: "activeState", Expression: fmt.Sprintf(`params != null && has(params.metadata.annotations) && %q in params.metadata.annotations && params.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(params.metadata.annotations[%q]) : -1`, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation)},
				{Name: "apiVolumes", Expression: `(!has(request.subResource) || request.subResource == "") && has(object.spec.volumes) ? object.spec.volumes.filter(v, v.name == "api-access") : []`},
			},
			Validations: validations,
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

func (g *RolloutGuard) runtimePodIdentityBinding() (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	contract, err := g.runtimePodContract()
	if err != nil {
		return nil, err
	}
	name := RuntimePodGuardPolicyName(g.ReleaseSequence)
	action := admissionregistrationv1.DenyAction
	exact := admissionregistrationv1.Exact
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.runtimePodGuardMetadata(name, contract.digest),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName: name,
			MatchResources: &admissionregistrationv1.MatchResources{
				MatchPolicy: &exact,
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					corev1.LabelMetadataName: g.ReleaseNamespace,
				}},
				ObjectSelector: &metav1.LabelSelector{},
			},
			ParamRef: &admissionregistrationv1.ParamRef{
				Name:                    ReleaseActivationName,
				Namespace:               g.ReleaseNamespace,
				ParameterNotFoundAction: &action,
			},
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}, nil
}

func (g *RolloutGuard) verifyRuntimePodIdentityPolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	expected, err := g.runtimePodIdentityPolicy()
	if err != nil {
		return err
	}
	if policy == nil || policy.Name != expected.Name {
		return fmt.Errorf("fixed runtime Pod identity guard policy %s is missing", expected.Name)
	}
	if err := g.verifyGuardMetadata("ValidatingAdmissionPolicy", policy.ObjectMeta, expected.Name); err != nil {
		return err
	}
	if policy.Annotations[ControllerStateVersionAnnotation] != strconv.FormatInt(int64(g.ControllerStateVersion), 10) ||
		policy.Annotations[ManagerImageAnnotation] != g.ManagerImage ||
		policy.Annotations[runtimePodContractDigestAnnotation] != expected.Annotations[runtimePodContractDigestAnnotation] {
		return fmt.Errorf("runtime Pod identity guard policy %s differs from the candidate executable contract", expected.Name)
	}
	if !reflect.DeepEqual(policy.Spec, expected.Spec) {
		return fmt.Errorf("runtime Pod identity guard policy %s differs from its declared contract", expected.Name)
	}
	return nil
}

func (g *RolloutGuard) verifyRuntimePodIdentityBinding(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
	expected, err := g.runtimePodIdentityBinding()
	if err != nil {
		return err
	}
	if binding == nil || binding.Name != expected.Name {
		return fmt.Errorf("fixed runtime Pod identity guard binding %s is missing", expected.Name)
	}
	if err := g.verifyGuardMetadata("ValidatingAdmissionPolicyBinding", binding.ObjectMeta, expected.Name); err != nil {
		return err
	}
	if binding.Annotations[ControllerStateVersionAnnotation] != strconv.FormatInt(int64(g.ControllerStateVersion), 10) ||
		binding.Annotations[ManagerImageAnnotation] != g.ManagerImage ||
		binding.Annotations[runtimePodContractDigestAnnotation] != expected.Annotations[runtimePodContractDigestAnnotation] {
		return fmt.Errorf("runtime Pod identity guard binding %s differs from the candidate executable contract", expected.Name)
	}
	if !reflect.DeepEqual(binding.Spec, expected.Spec) {
		return fmt.Errorf("runtime Pod identity guard binding %s differs from its declared contract", expected.Name)
	}
	return nil
}

func (g *RolloutGuard) runtimePodGuardMetadata(name, digest string) metav1.ObjectMeta {
	metadata := g.guardMetadata(name)
	metadata.Annotations[ControllerStateVersionAnnotation] = strconv.FormatInt(int64(g.ControllerStateVersion), 10)
	metadata.Annotations[ManagerImageAnnotation] = g.ManagerImage
	metadata.Annotations[runtimePodContractDigestAnnotation] = digest
	return metadata
}

func (g *RolloutGuard) runtimePodContract() (runtimePodContract, error) {
	if g.ControllerServiceAccountName == g.CertificateDeploymentName {
		return runtimePodContract{}, fmt.Errorf("controller and certificate runtime ServiceAccounts must differ")
	}
	if g.RuntimeAdmissionContractB64 == "" {
		return runtimePodContract{}, fmt.Errorf("runtime admission contract is required")
	}
	controllerArgsJSON, err := runtimeArgsJSON(g.ControllerArgs, "controller")
	if err != nil {
		return runtimePodContract{}, err
	}
	certificateArgsJSON, err := runtimeArgsJSON(g.CertificateArgs, "certificate rotator")
	if err != nil {
		return runtimePodContract{}, err
	}
	deploymentConfigJSON, err := runtimeConfigExpressionsJSON(g.RuntimeDeploymentConfigExpressions, "runtime Deployment config")
	if err != nil {
		return runtimePodContract{}, err
	}
	podConfigJSON, err := runtimeConfigExpressionsJSON(g.RuntimePodConfigExpressions, "runtime Pod config")
	if err != nil {
		return runtimePodContract{}, err
	}
	webhookSecretName, err := uniqueRuntimeArg(g.CertificateArgs, "--secret-name=")
	if err != nil {
		return runtimePodContract{}, err
	}
	if webhookSecretName != g.WebhookSecretName {
		return runtimePodContract{}, fmt.Errorf("certificate runtime Secret %q differs from rollout identity %q", webhookSecretName, g.WebhookSecretName)
	}
	controllerPort, err := runtimePortArg(g.ControllerArgs, "--webhook-port=")
	if err != nil {
		return runtimePodContract{}, err
	}
	if controllerPort != int64(g.WebhookPort) {
		return runtimePodContract{}, fmt.Errorf("controller runtime webhook port %d differs from rollout identity %d", controllerPort, g.WebhookPort)
	}
	certificatePort, err := runtimePortArg(g.CertificateArgs, "--health-bind-address=:")
	if err != nil {
		return runtimePodContract{}, err
	}
	if certificatePort != int64(g.CertificateHealthPort) {
		return runtimePodContract{}, fmt.Errorf("certificate runtime health port %d differs from rollout identity %d", certificatePort, g.CertificateHealthPort)
	}
	identity := strings.Join([]string{
		g.ReleaseNamespace,
		g.ReleaseName,
		strconv.FormatInt(int64(g.ReleaseSequence), 10),
		strconv.FormatInt(int64(g.ControllerStateVersion), 10),
		g.ManagerImage,
		g.ControllerServiceAccountName,
		g.CertificateDeploymentName,
		g.ControllerDeploymentName,
		g.CertificateDeploymentName,
		controllerArgsJSON,
		certificateArgsJSON,
		deploymentConfigJSON,
		podConfigJSON,
		g.RuntimeAdmissionContractB64,
	}, "\n")
	return runtimePodContract{
		controllerArgsJSON:  controllerArgsJSON,
		certificateArgsJSON: certificateArgsJSON,
		webhookSecretName:   webhookSecretName,
		controllerPort:      controllerPort,
		certificatePort:     certificatePort,
		digest:              fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(identity))),
	}, nil
}

func runtimeConfigExpressionsJSON(expressions []string, name string) (string, error) {
	if len(expressions) == 0 {
		return "", fmt.Errorf("%s expressions are required", name)
	}
	for index, expression := range expressions {
		if expression == "" || expression != strings.TrimSpace(expression) || len(expression) > 16*1024 {
			return "", fmt.Errorf("%s expression %d is empty, padded, or too large", name, index)
		}
	}
	encoded, err := json.Marshal(expressions)
	if err != nil {
		return "", fmt.Errorf("encode %s expressions: %w", name, err)
	}
	return string(encoded), nil
}

func runtimeArgsJSON(args []string, component string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("%s runtime args are required", component)
	}
	for index, arg := range args {
		if arg == "" {
			return "", fmt.Errorf("%s runtime arg %d is empty", component, index)
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("encode %s runtime args: %w", component, err)
	}
	return string(encoded), nil
}

func uniqueRuntimeArg(args []string, prefix string) (string, error) {
	value := ""
	found := false
	for _, arg := range args {
		if !strings.HasPrefix(arg, prefix) {
			continue
		}
		if found {
			return "", fmt.Errorf("runtime arg %s is duplicated", prefix)
		}
		found = true
		value = strings.TrimPrefix(arg, prefix)
	}
	if !found || value == "" {
		return "", fmt.Errorf("runtime arg %s is required", prefix)
	}
	return value, nil
}

func runtimePortArg(args []string, prefix string) (int64, error) {
	value, err := uniqueRuntimeArg(args, prefix)
	if err != nil {
		return 0, err
	}
	port, err := strconv.ParseInt(value, 10, 32)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("runtime arg %s must contain a port from 1 through 65535", prefix)
	}
	return port, nil
}

func (g *RolloutGuard) controllerPodContractExpressions(contract runtimePodContract) []string {
	return []string{
		g.runtimePodMetadataExpression(g.ControllerDeploymentName, "controller"),
		g.runtimePodSpecExpression(true),
		g.runtimeInitContainerExpression(true),
		g.runtimeApplicationContainerExpression("manager", "/manager", contract.controllerArgsJSON),
		`(!has(object.spec.containers[0].env) || object.spec.containers[0].env.size() == 0)`,
		g.controllerPortsAndProbesExpression(contract.controllerPort),
		g.controllerVolumesExpression(contract.webhookSecretName),
	}
}

func (g *RolloutGuard) certificatePodContractExpressions(contract runtimePodContract) []string {
	return []string{
		g.runtimePodMetadataExpression(g.CertificateDeploymentName, "certificate-rotation"),
		g.runtimePodSpecExpression(false),
		g.runtimeInitContainerExpression(false),
		g.runtimeApplicationContainerExpression("certificate-rotator", "/ptah-cert-rotator", contract.certificateArgsJSON),
		runtimeCertificateEnvironmentExpression(),
		g.certificatePortsAndProbesExpression(contract.certificatePort),
		runtimeCertificateVolumesExpression(),
	}
}

func (g *RolloutGuard) runtimePodMetadataExpression(deploymentName, component string) string {
	owner := "object.metadata.ownerReferences[0]"
	return fmt.Sprintf(
		`%s && has(object.metadata.labels) && %q in object.metadata.labels && object.metadata.labels[%q] == %q && %q in object.metadata.labels && object.metadata.labels[%q] == %q && has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1 && %s.apiVersion == "apps/v1" && %s.kind == "ReplicaSet" && %s && %s.uid != "" && has(%s.controller) && %s.controller && has(%s.blockOwnerDeletion) && %s.blockOwnerDeletion`,
		generatedPodNameValidationExpression(owner+".name"),
		instanceLabel, instanceLabel, g.ReleaseName,
		"app.kubernetes.io/component", "app.kubernetes.io/component", component,
		owner, owner, runtimeReplicaSetNameExpression(owner+".name", deploymentName),
		owner, owner, owner, owner, owner,
	)
}

func runtimeReplicaSetNameExpression(nameExpression, deploymentName string) string {
	prefix := deploymentName + "-"
	return fmt.Sprintf(
		`%[1]s.startsWith(%[2]q) && %[1]s.substring(%[3]d).matches("^[a-z0-9]{%[4]d,%[5]d}$")`,
		nameExpression, prefix, len(prefix), runtimeReplicaSetHashMinLength, runtimeReplicaSetHashMaxLength,
	)
}

// runtimePodRequestNameExpression recognizes exactly the names the API server
// can generate from a ReplicaSet owned by deploymentName. For long
// generateName values, the API server keeps only the first 58 ASCII bytes
// before appending its five-character random suffix.
func runtimePodRequestNameExpression(nameExpression, deploymentName string) string {
	replicaSetPrefix := deploymentName + "-"
	maxGeneratedPrefixLength := kubernetesDNSLabelMaxLength - kubernetesGeneratedSuffixLen
	if len(replicaSetPrefix) >= maxGeneratedPrefixLength {
		effectivePrefix := replicaSetPrefix[:maxGeneratedPrefixLength]
		return fmt.Sprintf(
			`(%[1]s.startsWith(%[2]q) && %[1]s.size() == %[3]d && %[1]s.substring(%[4]d).matches("^[a-z0-9]{%[5]d}$"))`,
			nameExpression,
			effectivePrefix,
			len(effectivePrefix)+kubernetesGeneratedSuffixLen,
			len(effectivePrefix),
			kubernetesGeneratedSuffixLen,
		)
	}

	alternatives := make([]string, 0, 2)
	maxUntruncatedHashLength := min(runtimeReplicaSetHashMaxLength, maxGeneratedPrefixLength-len(replicaSetPrefix)-1)
	if maxUntruncatedHashLength >= runtimeReplicaSetHashMinLength {
		alternatives = append(alternatives, fmt.Sprintf(
			`(%[1]s.startsWith(%[2]q) && %[1]s.substring(%[3]d).matches("^[a-z0-9]{%[4]d,%[5]d}-[a-z0-9]{%[6]d}$"))`,
			nameExpression,
			replicaSetPrefix,
			len(replicaSetPrefix),
			runtimeReplicaSetHashMinLength,
			maxUntruncatedHashLength,
			kubernetesGeneratedSuffixLen,
		))
	}

	truncatedHashCharacters := maxGeneratedPrefixLength - len(replicaSetPrefix)
	if truncatedHashCharacters <= runtimeReplicaSetHashMaxLength {
		alternatives = append(alternatives, fmt.Sprintf(
			`(%[1]s.startsWith(%[2]q) && %[1]s.size() == %[3]d && %[1]s.substring(%[4]d).matches("^[a-z0-9]{%[5]d}$"))`,
			nameExpression,
			replicaSetPrefix,
			kubernetesDNSLabelMaxLength,
			len(replicaSetPrefix),
			kubernetesDNSLabelMaxLength-len(replicaSetPrefix),
		))
	}

	return "(" + strings.Join(alternatives, " || ") + ")"
}

func (g *RolloutGuard) runtimePodSpecExpression(controller bool) string {
	fsGroup := `!has(object.spec.securityContext.fsGroup) && !has(object.spec.securityContext.fsGroupChangePolicy)`
	if controller {
		fsGroup = `has(object.spec.securityContext.fsGroup) && object.spec.securityContext.fsGroup == 65532 && (!has(object.spec.securityContext.fsGroupChangePolicy) || object.spec.securityContext.fsGroupChangePolicy == "Always")`
	}
	return fmt.Sprintf(
		`has(object.spec.automountServiceAccountToken) && !object.spec.automountServiceAccountToken && has(object.spec.enableServiceLinks) && !object.spec.enableServiceLinks && object.spec.restartPolicy == "Always" && object.spec.dnsPolicy == "ClusterFirst" && object.spec.schedulerName == "default-scheduler" && has(object.spec.terminationGracePeriodSeconds) && object.spec.terminationGracePeriodSeconds == 30 && %s && (!has(object.spec.hostNetwork) || !object.spec.hostNetwork) && (!has(object.spec.hostPID) || !object.spec.hostPID) && (!has(object.spec.hostIPC) || !object.spec.hostIPC) && (!has(object.spec.hostUsers) || object.spec.hostUsers) && (!has(object.spec.shareProcessNamespace) || !object.spec.shareProcessNamespace) && !has(object.spec.runtimeClassName) && !has(object.spec.activeDeadlineSeconds) && (!has(object.spec.hostAliases) || object.spec.hostAliases.size() == 0) && !has(object.spec.dnsConfig) && !has(object.spec.hostname) && !has(object.spec.hostnameOverride) && !has(object.spec.subdomain) && (!has(object.spec.setHostnameAsFQDN) || !object.spec.setHostnameAsFQDN) && (!has(object.spec.readinessGates) || object.spec.readinessGates.size() == 0) && (!has(object.spec.schedulingGates) || object.spec.schedulingGates.size() == 0) && (!has(object.spec.topologySpreadConstraints) || object.spec.topologySpreadConstraints.size() == 0) && !has(dyn(object.spec).overhead) && !has(object.spec.os) && !has(dyn(object.spec).resources) && (!has(object.spec.resourceClaims) || object.spec.resourceClaims.size() == 0) && has(object.spec.securityContext) && has(object.spec.securityContext.runAsNonRoot) && object.spec.securityContext.runAsNonRoot && has(object.spec.securityContext.runAsUser) && object.spec.securityContext.runAsUser == 65532 && has(object.spec.securityContext.runAsGroup) && object.spec.securityContext.runAsGroup == 65532 && has(object.spec.securityContext.seccompProfile) && object.spec.securityContext.seccompProfile.type == "RuntimeDefault" && !has(object.spec.securityContext.seccompProfile.localhostProfile) && %s && (!has(object.spec.securityContext.supplementalGroups) || object.spec.securityContext.supplementalGroups.size() == 0) && (!has(object.spec.securityContext.supplementalGroupsPolicy) || object.spec.securityContext.supplementalGroupsPolicy == "Merge") && (!has(object.spec.securityContext.sysctls) || object.spec.securityContext.sysctls.size() == 0) && !has(object.spec.securityContext.seLinuxOptions) && !has(object.spec.securityContext.seLinuxChangePolicy) && !has(object.spec.securityContext.windowsOptions) && !has(object.spec.securityContext.appArmorProfile) && has(object.spec.containers) && object.spec.containers.size() == 1 && has(object.spec.initContainers) && object.spec.initContainers.size() == 1 && (!has(object.spec.ephemeralContainers) || object.spec.ephemeralContainers.size() == 0) && has(object.spec.volumes) && variables.apiVolumes.size() == 1 && %s`,
		runtimePodNodeNameExpression(), fsGroup, runtimeAPIVolumeExpression("variables.apiVolumes[0]"),
	)
}

func runtimePodNodeNameExpression() string {
	return `(request.operation != "CREATE" || !has(object.spec.nodeName) || object.spec.nodeName == "") && (request.operation != "UPDATE" || ((has(object.spec.nodeName) == has(oldObject.spec.nodeName)) && (!has(object.spec.nodeName) || object.spec.nodeName == oldObject.spec.nodeName)))`
}

func (g *RolloutGuard) runtimeInitContainerExpression(controller bool) string {
	args, _ := runtimeArgsJSON(g.verifierArgs(controller), "runtime verifier")
	container := "object.spec.initContainers[0]"
	return strings.Join([]string{
		fmt.Sprintf(`%s.name == "verify-candidate-runtime"`, container),
		fmt.Sprintf(`%s.image == %q`, container, g.ManagerImage),
		fmt.Sprintf(`%s.command == ["/ptah-crd-manager"]`, container),
		fmt.Sprintf(`%s.args == %s`, container, args),
		runtimeContainerSecurityExpression(container),
		runtimeContainerPassiveExpression(container),
		runtimeResourceKeysExpression(container),
		fmt.Sprintf(`has(%s.volumeMounts) && %s.volumeMounts.size() == 1 && %s`, container, container, runtimeAPIMountExpression(container+".volumeMounts[0]")),
	}, " && ")
}

func (g *RolloutGuard) runtimeApplicationContainerExpression(name, command, argsJSON string) string {
	container := "object.spec.containers[0]"
	return strings.Join([]string{
		fmt.Sprintf(`%s.name == %q`, container, name),
		fmt.Sprintf(`%s.image == %q`, container, g.ManagerImage),
		fmt.Sprintf(`%s.command == [%q]`, container, command),
		fmt.Sprintf(`%s.args == %s`, container, argsJSON),
		runtimeContainerSecurityExpression(container),
		fmt.Sprintf(`!has(%[1]s.lifecycle) && (!has(%[1]s.envFrom) || %[1]s.envFrom.size() == 0) && (!has(%[1]s.volumeDevices) || %[1]s.volumeDevices.size() == 0) && (!has(%[1]s.stdin) || !%[1]s.stdin) && (!has(%[1]s.stdinOnce) || !%[1]s.stdinOnce) && (!has(%[1]s.tty) || !%[1]s.tty) && (!has(%[1]s.workingDir) || %[1]s.workingDir == "") && %[1]s.terminationMessagePath == "/dev/termination-log" && %[1]s.terminationMessagePolicy == "File" && !has(%[1]s.restartPolicy) && (!has(%[1]s.restartPolicyRules) || %[1]s.restartPolicyRules.size() == 0)`, container),
		runtimeResourceKeysExpression(container),
	}, " && ")
}

func runtimeContainerSecurityExpression(container string) string {
	return fmt.Sprintf(
		`has(%[1]s.securityContext) && has(%[1]s.securityContext.allowPrivilegeEscalation) && !%[1]s.securityContext.allowPrivilegeEscalation && has(%[1]s.securityContext.readOnlyRootFilesystem) && %[1]s.securityContext.readOnlyRootFilesystem && (!has(%[1]s.securityContext.privileged) || !%[1]s.securityContext.privileged) && !has(%[1]s.securityContext.procMount) && !has(%[1]s.securityContext.runAsUser) && !has(%[1]s.securityContext.runAsGroup) && !has(%[1]s.securityContext.runAsNonRoot) && !has(%[1]s.securityContext.seLinuxOptions) && !has(%[1]s.securityContext.windowsOptions) && !has(%[1]s.securityContext.seccompProfile) && !has(%[1]s.securityContext.appArmorProfile) && has(%[1]s.securityContext.capabilities) && (!has(%[1]s.securityContext.capabilities.add) || %[1]s.securityContext.capabilities.add.size() == 0) && has(%[1]s.securityContext.capabilities.drop) && %[1]s.securityContext.capabilities.drop == ["ALL"]`,
		container,
	)
}

func runtimeContainerPassiveExpression(container string) string {
	return fmt.Sprintf(
		`!has(%[1]s.lifecycle) && (!has(%[1]s.env) || %[1]s.env.size() == 0) && (!has(%[1]s.envFrom) || %[1]s.envFrom.size() == 0) && (!has(%[1]s.ports) || %[1]s.ports.size() == 0) && !has(%[1]s.livenessProbe) && !has(%[1]s.readinessProbe) && !has(%[1]s.startupProbe) && (!has(%[1]s.volumeDevices) || %[1]s.volumeDevices.size() == 0) && (!has(%[1]s.stdin) || !%[1]s.stdin) && (!has(%[1]s.stdinOnce) || !%[1]s.stdinOnce) && (!has(%[1]s.tty) || !%[1]s.tty) && (!has(%[1]s.workingDir) || %[1]s.workingDir == "") && %[1]s.terminationMessagePath == "/dev/termination-log" && %[1]s.terminationMessagePolicy == "File" && !has(%[1]s.restartPolicy) && (!has(%[1]s.restartPolicyRules) || %[1]s.restartPolicyRules.size() == 0)`,
		container,
	)
}

func runtimeResourceKeysExpression(container string) string {
	return fmt.Sprintf(
		`(!has(dyn(%[1]s.resources).limits) || dyn(%[1]s.resources).limits.all(key, key in ["cpu", "memory", "ephemeral-storage"])) && (!has(dyn(%[1]s.resources).requests) || dyn(%[1]s.resources).requests.all(key, key in ["cpu", "memory", "ephemeral-storage"])) && (!has(dyn(%[1]s.resources).claims) || dyn(%[1]s.resources).claims.size() == 0) && (!has(%[1]s.resizePolicy) || %[1]s.resizePolicy.size() == 0)`,
		container,
	)
}

func runtimeAPIVolumeExpression(volume string) string {
	return fmt.Sprintf(
		`%[1]s.name == "api-access" && has(%[1]s.projected) && has(%[1]s.projected.defaultMode) && %[1]s.projected.defaultMode == 420 && %[1]s.projected.sources.size() == 3 && %[1]s.projected.sources.exists(s, has(s.serviceAccountToken) && s.serviceAccountToken.path == "token" && has(s.serviceAccountToken.expirationSeconds) && s.serviceAccountToken.expirationSeconds == 3600 && !has(s.serviceAccountToken.audience)) && %[1]s.projected.sources.exists(s, has(s.configMap) && s.configMap.name == "kube-root-ca.crt" && (!has(s.configMap.optional) || !s.configMap.optional) && has(s.configMap.items) && s.configMap.items.size() == 1 && s.configMap.items[0].key == "ca.crt" && s.configMap.items[0].path == "ca.crt" && !has(s.configMap.items[0].mode)) && %[1]s.projected.sources.exists(s, has(s.downwardAPI) && has(s.downwardAPI.items) && s.downwardAPI.items.size() == 1 && s.downwardAPI.items[0].path == "namespace" && !has(s.downwardAPI.items[0].mode) && has(s.downwardAPI.items[0].fieldRef) && s.downwardAPI.items[0].fieldRef.apiVersion == "v1" && s.downwardAPI.items[0].fieldRef.fieldPath == "metadata.namespace") && %[1]s.projected.sources.all(s, has(s.serviceAccountToken) || has(s.configMap) || has(s.downwardAPI))`,
		volume,
	)
}

func runtimeAPIMountExpression(mount string) string {
	return fmt.Sprintf(
		`%s.name == variables.apiVolumes[0].name && %s.mountPath == "/var/run/secrets/kubernetes.io/serviceaccount" && has(%s.readOnly) && %s.readOnly && !has(%s.mountPropagation) && !has(%s.subPath) && !has(%s.subPathExpr)`,
		mount, mount, mount, mount, mount, mount, mount,
	)
}

func (g *RolloutGuard) controllerVolumesExpression(secretName string) string {
	container := "object.spec.containers[0]"
	return fmt.Sprintf(
		`object.spec.volumes.size() == 3 && object.spec.volumes.all(v, v.name in ["webhook-cert", "tmp"] || v.name == variables.apiVolumes[0].name) && object.spec.volumes.exists(v, v.name == "webhook-cert" && has(v.secret) && v.secret.secretName == %q && has(v.secret.defaultMode) && v.secret.defaultMode == 420 && (!has(v.secret.optional) || !v.secret.optional) && has(v.secret.items) && v.secret.items.size() == 2 && v.secret.items.exists(i, i.key == "tls.crt" && i.path == "tls.crt" && !has(i.mode)) && v.secret.items.exists(i, i.key == "tls.key" && i.path == "tls.key" && !has(i.mode)) && v.secret.items.all(i, i.key in ["tls.crt", "tls.key"])) && object.spec.volumes.exists(v, v.name == "tmp" && has(v.emptyDir) && (!has(v.emptyDir.medium) || v.emptyDir.medium == "") && has(dyn(v.emptyDir).sizeLimit)) && has(%s.volumeMounts) && %s.volumeMounts.size() == 3 && %s.volumeMounts.exists(m, m.name == "webhook-cert" && m.mountPath == "/certs" && has(m.readOnly) && m.readOnly && !has(m.mountPropagation) && !has(m.subPath) && !has(m.subPathExpr)) && %s.volumeMounts.exists(m, m.name == "tmp" && m.mountPath == "/tmp" && (!has(m.readOnly) || !m.readOnly) && !has(m.mountPropagation) && !has(m.subPath) && !has(m.subPathExpr)) && %s.volumeMounts.exists(m, %s) && %s.volumeMounts.all(m, m.name in ["webhook-cert", "tmp"] || m.name == variables.apiVolumes[0].name)`,
		secretName, container, container, container, container, container,
		runtimeAPIMountExpression("m"), container,
	)
}

func runtimeCertificateVolumesExpression() string {
	container := "object.spec.containers[0]"
	return fmt.Sprintf(
		`object.spec.volumes.size() == 1 && object.spec.volumes[0].name == variables.apiVolumes[0].name && has(%s.volumeMounts) && %s.volumeMounts.size() == 1 && %s`,
		container, container, runtimeAPIMountExpression(container+".volumeMounts[0]"),
	)
}

func (g *RolloutGuard) controllerPortsAndProbesExpression(webhookPort int64) string {
	container := "object.spec.containers[0]"
	return fmt.Sprintf(
		`has(%s.ports) && %s.ports.size() == 3 && %s.ports.all(p, p.protocol == "TCP" && (!has(p.hostIP) || p.hostIP == "") && (!has(p.hostPort) || p.hostPort == 0)) && %s.ports.exists(p, p.name == "metrics" && p.containerPort == 8080) && %s.ports.exists(p, p.name == "health" && p.containerPort == 8081) && %s.ports.exists(p, p.name == "webhook" && p.containerPort == %d) && %s && %s && !has(%s.startupProbe)`,
		container, container, container, container, container, container, webhookPort,
		runtimeHTTPProbeExpression(container+".livenessProbe", "/healthz", 10, 20, 1, 3),
		runtimeHTTPProbeExpression(container+".readinessProbe", "/readyz", 5, 10, 1, 3),
		container,
	)
}

func (g *RolloutGuard) certificatePortsAndProbesExpression(healthPort int64) string {
	container := "object.spec.containers[0]"
	return fmt.Sprintf(
		`has(%s.ports) && %s.ports.size() == 1 && %s.ports[0].name == "health" && %s.ports[0].containerPort == %d && %s.ports[0].protocol == "TCP" && (!has(%s.ports[0].hostIP) || %s.ports[0].hostIP == "") && (!has(%s.ports[0].hostPort) || %s.ports[0].hostPort == 0) && %s && %s && !has(%s.startupProbe)`,
		container, container, container, container, healthPort, container,
		container, container, container, container,
		runtimeHTTPProbeExpression(container+".livenessProbe", "/healthz", 5, 10, 1, 3),
		runtimeHTTPProbeExpression(container+".readinessProbe", "/readyz", 0, 5, 1, 1),
		container,
	)
}

func runtimeHTTPProbeExpression(probe, path string, initialDelay, period, timeout, failure int32) string {
	return fmt.Sprintf(
		`has(%[1]s) && has(%[1]s.httpGet) && %[1]s.httpGet.path == %[2]q && %[1]s.httpGet.port == "health" && %[1]s.httpGet.scheme == "HTTP" && (!has(%[1]s.httpGet.host) || %[1]s.httpGet.host == "") && (!has(%[1]s.httpGet.httpHeaders) || %[1]s.httpGet.httpHeaders.size() == 0) && !has(%[1]s.exec) && !has(%[1]s.tcpSocket) && !has(%[1]s.grpc) && (has(%[1]s.initialDelaySeconds) ? %[1]s.initialDelaySeconds : 0) == %[3]d && %[1]s.periodSeconds == %[4]d && %[1]s.timeoutSeconds == %[5]d && %[1]s.successThreshold == 1 && %[1]s.failureThreshold == %[6]d`,
		probe, path, initialDelay, period, timeout, failure,
	)
}

func runtimeCertificateEnvironmentExpression() string {
	container := "object.spec.containers[0]"
	return fmt.Sprintf(
		`has(%s.env) && %s.env.size() == 2 && %s.env.all(e, e.name in ["POD_NAME", "POD_UID"] && (!has(e.value) || e.value == "") && has(e.valueFrom) && has(e.valueFrom.fieldRef) && (!has(e.valueFrom.fieldRef.apiVersion) || e.valueFrom.fieldRef.apiVersion == "v1") && !has(e.valueFrom.resourceFieldRef) && !has(e.valueFrom.configMapKeyRef) && !has(e.valueFrom.secretKeyRef)) && %s.env.exists(e, e.name == "POD_NAME" && e.valueFrom.fieldRef.fieldPath == "metadata.name") && %s.env.exists(e, e.name == "POD_UID" && e.valueFrom.fieldRef.fieldPath == "metadata.uid")`,
		container, container, container, container, container,
	)
}
