package crdupgrade

// This file freezes the controller-principal guards emitted by the release
// immediately preceding the versioned-principal design. They are verify-only
// teardown contracts: new installs never create them, and deletion never
// relies on label or prefix discovery.

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
)

const legacyControllerTeardownGroup = "controller-principal-v1"

func legacyControllerGuardDigest(releaseNamespace, releaseName string) string {
	sum := sha256.Sum256([]byte(releaseNamespace + "\n" + releaseName))
	return fmt.Sprintf("%x", sum)[:12]
}

func legacyControllerGuardNames(releaseNamespace, releaseName string) []string {
	digest := legacyControllerGuardDigest(releaseNamespace, releaseName)
	return []string{
		"ptah-operator-controller-write-guard-v1-" + digest,
		"ptah-operator-job-write-guard-v1-" + digest,
		"ptah-operator-chunk-write-guard-v1-" + digest,
		"ptah-operator-plan-write-guard-v1-" + digest,
		"ptah-operator-service-account-origin-guard-v1-" + digest,
		"ptah-operator-runtime-parent-guard-v1-" + digest,
	}
}

func legacyControllerTeardownContracts(guard *RolloutGuard) []teardownGuardContract {
	names := legacyControllerGuardNames(guard.ReleaseNamespace, guard.ReleaseName)
	objects, buildErr := legacyControllerGuardObjects(guard, names)
	contracts := make([]teardownGuardContract, len(names))
	for index, name := range names {
		index := index
		contracts[index] = teardownGuardContract{
			name:          name,
			parameterized: index >= 1 && index <= 3,
			optionalGroup: legacyControllerTeardownGroup,
			verifyPolicy: func(actual *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				if buildErr != nil {
					return buildErr
				}
				return verifyLegacyControllerPolicy(actual, objects[index].policy)
			},
			verifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				if buildErr != nil {
					return buildErr
				}
				return verifyLegacyControllerBinding(actual, objects[index].binding)
			},
		}
	}
	return contracts
}

type legacyControllerGuardObjectsPair struct {
	policy  *admissionregistrationv1.ValidatingAdmissionPolicy
	binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
}

func legacyControllerGuardObjects(guard *RolloutGuard, names []string) ([]legacyControllerGuardObjectsPair, error) {
	if guard.PreviousControllerServiceAccountName == "" {
		return nil, fmt.Errorf("legacy controller guard exists without an exact predecessor ServiceAccount identity")
	}
	if len(names) != 6 {
		return nil, fmt.Errorf("legacy controller guard inventory has %d names, want 6", len(names))
	}
	previous := *guard
	previous.ControllerServiceAccountName = guard.PreviousControllerServiceAccountName
	previous.PreviousControllerServiceAccountName = ""
	previous.PreviousControllerReleaseSequence = 0

	objects := make([]legacyControllerGuardObjectsPair, 0, len(names))
	write := NewControllerWriteGuard(&previous)
	writePolicy := write.policy()
	writeBinding := write.binding()
	if err := removeAdmissionConvergenceDependencyProbe(writePolicy); err != nil {
		return nil, fmt.Errorf("restore legacy controller write policy: %w", err)
	}
	if err := removeAdmissionConvergenceBindingProbe(writeBinding); err != nil {
		return nil, fmt.Errorf("restore legacy controller write binding: %w", err)
	}
	if err := requireLegacyVariableNames("controller write", writePolicy.Spec.Variables, []string{
		"activeRelease", "oldFinalizers", "newFinalizers", "activeFinalizer", "oldActiveCount", "newActiveCount",
	}); err != nil {
		return nil, err
	}
	if len(writePolicy.Spec.Validations) != 6 {
		return nil, fmt.Errorf("controller write guard has %d validations, want 6 before legacy conversion", len(writePolicy.Spec.Validations))
	}
	renameLegacyControllerGuard(writePolicy, writeBinding, names[0])
	writePolicy.Spec.ParamKind = nil
	writePolicy.Spec.MatchConditions = legacyExactControllerMatch(guard.ReleaseNamespace, guard.PreviousControllerServiceAccountName)
	writePolicy.Spec.Variables = append([]admissionregistrationv1.Variable(nil), writePolicy.Spec.Variables[1:]...)
	writePolicy.Spec.Validations = append([]admissionregistrationv1.Validation(nil), writePolicy.Spec.Validations[2:]...)
	writeBinding.Spec.ParamRef = nil
	objects = append(objects, legacyControllerGuardObjectsPair{policy: writePolicy, binding: writeBinding})

	objectGuard := NewControllerObjectGuard(&previous)
	objectEntries := objectGuard.entries()
	if len(objectEntries) != 3 {
		return nil, fmt.Errorf("controller object guard has %d entries, want 3 before legacy conversion", len(objectEntries))
	}
	if len(objectEntries[0].validations) < 2 || objectEntries[0].validations[1].Expression != controllerJobAnnotationContractExpression() {
		return nil, fmt.Errorf("controller Job guard validation layout changed before legacy conversion")
	}
	if len(objectEntries[2].validations) < 2 || objectEntries[2].validations[1].Expression != controllerPlanContractExpression() {
		return nil, fmt.Errorf("controller Plan guard validation layout changed before legacy conversion")
	}
	objectNames := names[1:4]
	for index := range objectEntries {
		entry := objectEntries[index]
		entry.name = objectNames[index]
		switch index {
		case 0:
			entry.validations[1].Expression = legacyControllerJobAnnotationContractExpression()
		case 2:
			entry.validations[1].Expression = legacyControllerPlanContractExpression()
		}
		policy := objectGuard.policy(entry)
		binding := objectGuard.binding(entry)
		if err := removeAdmissionConvergenceDependencyProbe(policy); err != nil {
			return nil, fmt.Errorf("restore legacy controller object policy %s: %w", entry.name, err)
		}
		if err := removeAdmissionConvergenceBindingProbe(binding); err != nil {
			return nil, fmt.Errorf("restore legacy controller object binding %s: %w", entry.name, err)
		}
		if err := requireLegacyVariableNames("controller object", policy.Spec.Variables, []string{
			"activeRelease", "activeControllerStateString", "activeControllerState", "activeControllerImage", "candidateRelease", "previousRelease",
		}); err != nil {
			return nil, err
		}
		if len(policy.Spec.Validations) != len(entry.validations)+2 {
			return nil, fmt.Errorf("controller object guard validation layout changed before legacy conversion")
		}
		policy.Spec.MatchConditions = legacyExactControllerMatch(guard.ReleaseNamespace, guard.PreviousControllerServiceAccountName)
		policy.Spec.Variables = legacyControllerObjectActivationVariables()
		policy.Spec.Validations = append(policy.Spec.Validations[:1:1], policy.Spec.Validations[2:]...)
		policy.Spec.Validations[0].Expression = legacyReleaseActivationParameterShapeExpression(guard.ReleaseNamespace, guard.ReleaseName)
		objects = append(objects, legacyControllerGuardObjectsPair{policy: policy, binding: binding})
	}

	originPolicy, originBinding, err := legacyServiceAccountOriginObjects(&previous, names[4])
	if err != nil {
		return nil, err
	}
	objects = append(objects, legacyControllerGuardObjectsPair{policy: originPolicy, binding: originBinding})

	parent := NewParentWorkloadGuard(&previous)
	parentPolicy := parent.replicaSetPolicy()
	if err := removeAdmissionConvergenceDependencyProbe(parentPolicy); err != nil {
		return nil, fmt.Errorf("restore legacy runtime parent policy: %w", err)
	}
	parentBinding := parent.binding(parentPolicy.Name, false)
	renameLegacyControllerGuard(parentPolicy, parentBinding, names[5])
	objects = append(objects, legacyControllerGuardObjectsPair{policy: parentPolicy, binding: parentBinding})
	return objects, nil
}

func removeAdmissionConvergenceBindingProbe(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
	if binding == nil || binding.Spec.MatchResources == nil {
		return fmt.Errorf("admission convergence dependency binding or match resources are nil")
	}
	rules := binding.Spec.MatchResources.ResourceRules
	want := admissionConvergenceProbeResourceRule()
	if len(rules) == 0 || !reflect.DeepEqual(rules[len(rules)-1], want) {
		return fmt.Errorf("admission convergence dependency binding marker rule differs from the exact wrapper")
	}
	binding.Spec.MatchResources.ResourceRules = rules[:len(rules)-1]
	return nil
}

func legacyServiceAccountOriginObjects(rollout *RolloutGuard, name string) (
	*admissionregistrationv1.ValidatingAdmissionPolicy,
	*admissionregistrationv1.ValidatingAdmissionPolicyBinding,
	error,
) {
	guard := NewServiceAccountOriginGuard(rollout)
	policy, err := guard.policy()
	if err != nil {
		return nil, nil, err
	}
	if err := removeAdmissionConvergenceDependencyProbe(policy); err != nil {
		return nil, nil, fmt.Errorf("restore legacy service account origin policy: %w", err)
	}
	binding := guard.binding()
	renameLegacyControllerGuard(policy, binding, name)
	for _, annotation := range []string{
		ControllerStateVersionAnnotation,
		AdmissionContractVersionAnnotation,
		ReleaseSequenceAnnotation,
		ManagerImageAnnotation,
		HookServiceAccountAnnotation,
		ControllerServiceAccountAnnotation,
		ControllerServiceAccountManagedAnnotation,
		PreviousControllerServiceAccountAnnotation,
		PreviousControllerServiceAccountUIDAnnotation,
		PreviousControllerServiceAccountManagedAnnotation,
		PreviousControllerReleaseSequenceAnnotation,
	} {
		delete(policy.Annotations, annotation)
		delete(binding.Annotations, annotation)
	}
	policy.Spec.ParamKind = nil
	binding.Spec.ParamRef = nil
	if len(policy.Spec.MatchConditions) != 1 {
		return nil, nil, fmt.Errorf("service account origin guard has %d match conditions, want 1 before legacy conversion", len(policy.Spec.MatchConditions))
	}
	if err := requireLegacyVariableNames("service account origin", policy.Spec.Variables, []string{
		"activeRelease", "controllerCredentialPhase", "isTokenRequest", "isControllerTokenRequest", "isHookCaller", "isControllerCaller", "isCertificateCaller", "isProtectedCaller", "isProtectedTokenRequest", "callerPodName", "callerPodUID",
	}); err != nil {
		return nil, nil, err
	}
	if len(policy.Spec.Validations) != 5 {
		return nil, nil, fmt.Errorf("service account origin guard has %d validations, want 5 before legacy conversion", len(policy.Spec.Validations))
	}

	hookBase, err := guard.hookServiceAccountBase()
	if err != nil {
		return nil, nil, err
	}
	hookServiceAccountPattern := "^" + regexp.QuoteMeta(hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	hookUsernamePattern := "^" + regexp.QuoteMeta("system:serviceaccount:"+guard.ReleaseNamespace+":"+hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	hookPodPattern := "^" + regexp.QuoteMeta(hookBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}-`
	teardownServiceAccountPattern := "^" + regexp.QuoteMeta(hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	teardownUsernamePattern := "^" + regexp.QuoteMeta("system:serviceaccount:"+guard.ReleaseNamespace+":"+hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	teardownPodPattern := "^" + regexp.QuoteMeta(hookBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}-`
	quiescePodPattern := "^" + regexp.QuoteMeta(hookBase+"-quiesce-v") + `[1-9][0-9]*-[0-9a-f]{12}-`
	controllerUsername := "system:serviceaccount:" + guard.ReleaseNamespace + ":" + guard.ControllerServiceAccountName
	certificateUsername := "system:serviceaccount:" + guard.ReleaseNamespace + ":" + guard.CertificateServiceAccountName

	policy.Spec.MatchConditions[0].Expression = fmt.Sprintf(
		`request.userInfo.username in [%q, %q] || request.userInfo.username.matches(%q) || request.userInfo.username.matches(%q) || (request.operation == "CREATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "serviceaccounts" && has(request.subResource) && request.subResource == "token" && request.namespace == %q && (request.name in [%q, %q] || request.name.matches(%q) || request.name.matches(%q)))`,
		controllerUsername, certificateUsername, hookUsernamePattern, teardownUsernamePattern,
		guard.ReleaseNamespace, guard.ControllerServiceAccountName, guard.CertificateServiceAccountName,
		hookServiceAccountPattern, teardownServiceAccountPattern,
	)
	policy.Spec.Variables = append(
		[]admissionregistrationv1.Variable{policy.Spec.Variables[2]},
		policy.Spec.Variables[4:]...,
	)
	policy.Spec.Variables[2].Expression = fmt.Sprintf(`request.userInfo.username == %q`, controllerUsername)
	policy.Spec.Variables[5].Expression = fmt.Sprintf(
		`variables.isTokenRequest && request.namespace == %q && (request.name in [%q, %q] || request.name.matches(%q) || request.name.matches(%q))`,
		guard.ReleaseNamespace, guard.ControllerServiceAccountName, guard.CertificateServiceAccountName,
		hookServiceAccountPattern, teardownServiceAccountPattern,
	)
	policy.Spec.Validations = append([]admissionregistrationv1.Validation(nil), policy.Spec.Validations[3:]...)
	policy.Spec.Validations[1].Expression = fmt.Sprintf(
		`!variables.isProtectedTokenRequest || (request.userInfo.username.matches("^system:node:.+$") && request.userInfo.groups.filter(group, group == "system:nodes").size() == 1 && has(object.spec.boundObjectRef) && has(object.spec.boundObjectRef.apiVersion) && object.spec.boundObjectRef.apiVersion == "v1" && has(object.spec.boundObjectRef.kind) && object.spec.boundObjectRef.kind == "Pod" && has(object.spec.boundObjectRef.name) && object.spec.boundObjectRef.name != "" && has(object.spec.boundObjectRef.uid) && object.spec.boundObjectRef.uid != "" && ((request.name == %q && %s) || (request.name == %q && %s) || (request.name.matches(%q) && (object.spec.boundObjectRef.name.matches(%q) || object.spec.boundObjectRef.name.matches(%q) || object.spec.boundObjectRef.name.matches(%q))) || (request.name.matches(%q) && object.spec.boundObjectRef.name.matches(%q))))`,
		guard.ControllerServiceAccountName,
		runtimePodRequestNameExpression("object.spec.boundObjectRef.name", guard.ControllerDeploymentName),
		guard.CertificateServiceAccountName,
		runtimePodRequestNameExpression("object.spec.boundObjectRef.name", guard.CertificateDeploymentName),
		hookServiceAccountPattern, hookPodPattern, `^ptah-hook-identity-v[1-9][0-9]*-[0-9a-f]{12}-`, quiescePodPattern,
		teardownServiceAccountPattern, teardownPodPattern,
	)
	return policy, binding, nil
}

func requireLegacyVariableNames(kind string, variables []admissionregistrationv1.Variable, names []string) error {
	if len(variables) != len(names) {
		return fmt.Errorf("%s guard has %d variables, want %d before legacy conversion", kind, len(variables), len(names))
	}
	for index, name := range names {
		if variables[index].Name != name {
			return fmt.Errorf("%s guard variable %d is %q, want %q before legacy conversion", kind, index, variables[index].Name, name)
		}
	}
	return nil
}

func legacyExactControllerMatch(namespace, serviceAccount string) []admissionregistrationv1.MatchCondition {
	return []admissionregistrationv1.MatchCondition{{
		Name:       "exact-controller-service-account",
		Expression: fmt.Sprintf(`request.userInfo.username == %q`, "system:serviceaccount:"+namespace+":"+serviceAccount),
	}}
}

func legacyControllerObjectActivationVariables() []admissionregistrationv1.Variable {
	return []admissionregistrationv1.Variable{
		{Name: "activeRelease", Expression: decimalCEL("params", activeReleaseDataKey, true)},
		{Name: "activeControllerStateString", Expression: fmt.Sprintf(`params != null && has(params.metadata.annotations) && %q in params.metadata.annotations ? params.metadata.annotations[%q] : ""`, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation)},
		{Name: "activeControllerState", Expression: fmt.Sprintf(`params != null && has(params.metadata.annotations) && %q in params.metadata.annotations && params.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(params.metadata.annotations[%q]) : 0`, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation, ControllerStateVersionAnnotation)},
		{Name: "activeControllerImage", Expression: fmt.Sprintf(`params != null && has(params.metadata.annotations) && %q in params.metadata.annotations ? params.metadata.annotations[%q] : ""`, ManagerImageAnnotation, ManagerImageAnnotation)},
		{Name: "isBootstrap", Expression: `variables.activeRelease == 0`},
	}
}

// legacyReleaseActivationParameterShapeExpression is intentionally spelled
// out instead of calling the current activation constructor. Retained v1
// controller-object policies keep validating the predecessor parameter shape
// byte-for-byte even as the current activation state machine evolves.
func legacyReleaseActivationParameterShapeExpression(releaseNamespace, releaseName string) string {
	const legacyReleaseActivationHookWeight = "-150"
	object := "params"
	parts := []string{
		fmt.Sprintf(`%s.metadata.name == %q`, object, ReleaseActivationName),
		fmt.Sprintf(`%s.metadata.namespace == %q`, object, releaseNamespace),
		fmt.Sprintf(`(!has(%s.metadata.generateName) || %s.metadata.generateName == "")`, object, object),
		fmt.Sprintf(`has(%s.metadata.annotations)`, object),
		fmt.Sprintf(`%s.metadata.annotations.size() == 10`, object),
		fmt.Sprintf(`%s.metadata.annotations[%q] == "pre-install,pre-upgrade"`, object, "helm.sh/hook"),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, "helm.sh/hook-weight", legacyReleaseActivationHookWeight),
		fmt.Sprintf(`%s.metadata.annotations[%q] == "keep"`, object, "helm.sh/resource-policy"),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, rolloutGuardVersionAnnotation, rolloutGuardVersion),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, ReleaseNameAnnotation, releaseName),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, ReleaseNamespaceAnnotation, releaseNamespace),
		fmt.Sprintf(`%s.metadata.annotations[%q].matches("^[1-9][0-9]*$")`, object, ControllerStateVersionAnnotation),
		fmt.Sprintf(`%s.metadata.annotations[%q].matches("^[1-9][0-9]*$")`, object, AdmissionContractVersionAnnotation),
		fmt.Sprintf(`%s.metadata.annotations[%q].matches("^[1-9][0-9]*$")`, object, ReleaseSequenceAnnotation),
		fmt.Sprintf(`%s.metadata.annotations[%q].matches("^[^[:space:]]+$")`, object, ManagerImageAnnotation),
		fmt.Sprintf(`has(%s.metadata.labels)`, object),
		fmt.Sprintf(`%s.metadata.labels.size() == 3`, object),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, managedByLabel, rolloutGuardManagedBy),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, instanceLabel, releaseName),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, "app.kubernetes.io/component", rolloutGuardComponent),
		fmt.Sprintf(`has(%s.data)`, object),
		fmt.Sprintf(`%s.data.size() == 1`, object),
		fmt.Sprintf(`%q in %s.data`, activeReleaseDataKey, object),
		fmt.Sprintf(`%s.data[%q].matches("^(0|[1-9][0-9]*)$")`, object, activeReleaseDataKey),
		fmt.Sprintf(`(!has(%s.binaryData) || %s.binaryData.size() == 0)`, object, object),
		fmt.Sprintf(`(!has(%s.immutable) || !%s.immutable)`, object, object),
		fmt.Sprintf(`(!has(%s.metadata.ownerReferences) || %s.metadata.ownerReferences.size() == 0)`, object, object),
		fmt.Sprintf(`(!has(%s.metadata.finalizers) || %s.metadata.finalizers.size() == 0)`, object, object),
		fmt.Sprintf(`(%s.data[%q] == "0" || %s.metadata.annotations[%q] == %s.data[%q])`, object, activeReleaseDataKey, object, ReleaseSequenceAnnotation, object, activeReleaseDataKey),
	}
	return strings.Join(parts, " && ")
}

func legacyControllerJobAnnotationContractExpression() string {
	legacyReadOnly := `object.metadata.labels["operator.ptah.dev/operation"] in ["resolve", "verify", "observe", "plan"] && has(object.metadata.annotations) && object.metadata.annotations.size() == 5 && ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/admission-snapshot-digest"].all(key, key in object.metadata.annotations) && object.metadata.annotations["operator.ptah.dev/operation-id"] != "" && object.metadata.annotations["operator.ptah.dev/input-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/ptah-version"] != "" && object.metadata.annotations["operator.ptah.dev/execution-binding-id"].matches("^v1-[0-9a-f]{32}$") && object.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"].matches("^sha256:[0-9a-f]{64}$")`
	legacyApply := `object.metadata.labels["operator.ptah.dev/operation"] == "apply" && has(object.metadata.annotations) && object.metadata.annotations.size() == 7 && ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/plan-fingerprint", "operator.ptah.dev/plan-content-digest", "operator.ptah.dev/admission-snapshot-digest"].all(key, key in object.metadata.annotations) && object.metadata.annotations["operator.ptah.dev/operation-id"] != "" && object.metadata.annotations["operator.ptah.dev/input-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/ptah-version"] != "" && object.metadata.annotations["operator.ptah.dev/execution-binding-id"].matches("^v1-[0-9a-f]{32}$") && object.metadata.annotations["operator.ptah.dev/plan-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/plan-content-digest"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"].matches("^sha256:[0-9a-f]{64}$")`
	current := `has(object.metadata.annotations) && ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/controller-image", "operator.ptah.dev/controller-revision", "operator.ptah.dev/controller-state-version", "operator.ptah.dev/admission-snapshot-digest"].all(key, key in object.metadata.annotations) && object.metadata.annotations.all(key, key in ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/controller-image", "operator.ptah.dev/controller-revision", "operator.ptah.dev/controller-state-version", "operator.ptah.dev/admission-snapshot-digest", "operator.ptah.dev/plan-fingerprint", "operator.ptah.dev/plan-content-digest"]) && object.metadata.annotations["operator.ptah.dev/operation-id"] != "" && object.metadata.annotations["operator.ptah.dev/input-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/ptah-version"] != "" && object.metadata.annotations["operator.ptah.dev/execution-binding-id"].matches("^v1-[0-9a-f]{32}$") && object.metadata.annotations["operator.ptah.dev/controller-image"].matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/controller-revision"] != "" && object.metadata.annotations["operator.ptah.dev/controller-state-version"].matches("^[1-9][0-9]*$") && object.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"].matches("^sha256:[0-9a-f]{64}$") && ((object.metadata.labels["operator.ptah.dev/operation"] == "apply" && "operator.ptah.dev/plan-fingerprint" in object.metadata.annotations && object.metadata.annotations["operator.ptah.dev/plan-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && "operator.ptah.dev/plan-content-digest" in object.metadata.annotations && object.metadata.annotations["operator.ptah.dev/plan-content-digest"].matches("^sha256:[0-9a-f]{64}$")) || (object.metadata.labels["operator.ptah.dev/operation"] != "apply" && !("operator.ptah.dev/plan-fingerprint" in object.metadata.annotations) && !("operator.ptah.dev/plan-content-digest" in object.metadata.annotations)))`
	activeIdentity := `object.metadata.annotations["operator.ptah.dev/controller-image"] == variables.activeControllerImage && object.metadata.annotations["operator.ptah.dev/controller-state-version"] == variables.activeControllerStateString`
	return fmt.Sprintf(`((request.operation == "UPDATE" || (request.operation == "CREATE" && variables.isBootstrap)) && ((%s) || (%s))) || (variables.activeRelease > 0 && (%s) && (request.operation == "UPDATE" || (request.operation == "CREATE" && (%s))))`, legacyReadOnly, legacyApply, current, activeIdentity)
}

func legacyControllerPlanContractExpression() string {
	common := `object.spec.fingerprint.matches("^sha256:[0-9a-f]{64}$") && object.spec.contentDigest.matches("^sha256:[0-9a-f]{64}$") && object.spec.artifactDigest.matches("^sha256:[0-9a-f]{64}$") && object.spec.coordinationDigest.matches("^sha256:[0-9a-f]{64}$") && object.spec.targetIdentityDigest.matches("^sha256:[0-9a-f]{64}$") && object.spec.actualStateFingerprint.matches("^sha256:[0-9a-f]{64}$") && object.spec.desiredStateFingerprint.matches("^sha256:[0-9a-f]{64}$") && object.spec.policyFingerprint.matches("^sha256:[0-9a-f]{64}$") && object.spec.verificationPolicyUID != "" && object.spec.verificationPolicyDigest.matches("^sha256:[0-9a-f]{64}$") && object.spec.executionBindingID.matches("^v1-[0-9a-f]{32}$") && object.spec.ptahVersion != "" && object.spec.executorImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && object.spec.runnerImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && object.spec.runnerProtocolVersion >= 1 && object.spec.dialect != "" && object.spec.statementCount >= 1 && object.spec.size >= 1 && object.spec.size <= 8388608`
	legacy := `variables.isBootstrap && object.spec.contractVersion == 2 && !has(dyn(object.spec).controllerImage) && !has(dyn(object.spec).controllerRevision) && !has(dyn(object.spec).controllerStateVersion)`
	current := `variables.activeRelease > 0 && object.spec.contractVersion == 3 && has(dyn(object.spec).controllerImage) && dyn(object.spec).controllerImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && dyn(object.spec).controllerImage == variables.activeControllerImage && has(dyn(object.spec).controllerRevision) && dyn(object.spec).controllerRevision != "" && has(dyn(object.spec).controllerStateVersion) && dyn(object.spec).controllerStateVersion >= 1 && dyn(object.spec).controllerStateVersion == variables.activeControllerState`
	return fmt.Sprintf(`(%s) && ((%s) || (%s))`, common, legacy, current)
}

func renameLegacyControllerGuard(
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding,
	name string,
) {
	policy.Name = name
	binding.Name = name
	binding.Spec.PolicyName = name
}

func verifyLegacyControllerPolicy(actual, expected *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	if actual == nil || actual.Name != expected.Name {
		return fmt.Errorf("fixed legacy controller guard policy %s is missing", expected.Name)
	}
	if err := verifyLegacyControllerMetadata("ValidatingAdmissionPolicy", actual.Name, actual.Annotations, actual.Labels, expected); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual.Spec, expected.Spec) {
		return fmt.Errorf("legacy controller guard policy %s differs from the immutable contract", expected.Name)
	}
	return nil
}

func verifyLegacyControllerBinding(actual, expected *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
	if actual == nil || actual.Name != expected.Name {
		return fmt.Errorf("fixed legacy controller guard binding %s is missing", expected.Name)
	}
	if err := verifyLegacyControllerMetadata("ValidatingAdmissionPolicyBinding", actual.Name, actual.Annotations, actual.Labels, expected); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual.Spec, expected.Spec) {
		return fmt.Errorf("legacy controller guard binding %s differs from the immutable contract", expected.Name)
	}
	return nil
}

func verifyLegacyControllerMetadata(kind, name string, annotations, labels map[string]string, expected interface {
	GetAnnotations() map[string]string
	GetLabels() map[string]string
}) error {
	for key, value := range expected.GetAnnotations() {
		if annotations[key] != value {
			return fmt.Errorf("fixed legacy controller guard %s/%s has foreign or incomplete ownership", kind, name)
		}
	}
	for key, value := range expected.GetLabels() {
		if labels[key] != value {
			return fmt.Errorf("fixed legacy controller guard %s/%s has foreign or incomplete ownership", kind, name)
		}
	}
	return nil
}
