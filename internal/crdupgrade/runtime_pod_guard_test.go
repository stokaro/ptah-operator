package crdupgrade

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRuntimePodIdentityPolicyPinsServiceAccountExecutableAndSubresources(t *testing.T) {
	guard := runtimePodGuardFixture()
	policy, err := guard.runtimePodIdentityPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Name != "ptah-operator-runtime-pod-identity-v1" {
		t.Fatalf("policy name = %q", policy.Name)
	}
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail ||
		policy.Spec.MatchConstraints == nil || policy.Spec.MatchConstraints.MatchPolicy == nil ||
		*policy.Spec.MatchConstraints.MatchPolicy != admissionregistrationv1.Exact ||
		policy.Spec.MatchConstraints.NamespaceSelector == nil || policy.Spec.MatchConstraints.ObjectSelector == nil {
		t.Fatal("runtime Pod policy is not explicitly fail-closed with exact matching and defaulted selectors")
	}
	serialized, err := json.Marshal(policy.Spec)
	if err != nil {
		t.Fatal(err)
	}
	contract := string(serialized)
	for _, required := range []string{
		`"pods/ephemeralcontainers"`,
		`"pods/resize"`,
		`"pods/exec"`,
		`"pods/attach"`,
		`"pods/portforward"`,
		`"pods/proxy"`,
		`!has(request.subResource) || request.subResource == \"\"`,
		`request.userInfo.username in [\"system:kube-controller-manager\", \"system:serviceaccount:kube-system:replicaset-controller\"]`,
		`object.spec.serviceAccountName == \"ptah-controller\"`,
		`object.spec.serviceAccountName == \"ptah-cert-rotator\"`,
		`variables.newRelease \u003e 1`,
		`variables.activeRelease \u003e= variables.newRelease`,
		`object.spec.automountServiceAccountToken`,
		`!object.spec.automountServiceAccountToken`,
		`v.name == \"api-access\"`,
		`expirationSeconds == 3600`,
		`object.spec.containers.size() == 1`,
		`object.spec.initContainers.size() == 1`,
		`object.spec.containers[0].command == [\"/manager\"]`,
		`object.spec.containers[0].command == [\"/ptah-cert-rotator\"]`,
		`object.spec.initContainers[0].command == [\"/ptah-crd-manager\"]`,
		`object.spec.containers[0].securityContext.capabilities.drop == [\"ALL\"]`,
		`!has(object.spec.containers[0].lifecycle)`,
		`!has(object.spec.ephemeralContainers)`,
		`!has(object.spec.hostnameOverride)`,
		`object.spec.topologySpreadConstraints.size() == 0`,
		`!has(dyn(object.spec).resources)`,
		`object.spec.securityContext.fsGroupChangePolicy`,
		`object.spec.securityContext.supplementalGroupsPolicy`,
		`object.metadata.ownerReferences[0].kind == \"ReplicaSet\"`,
		`--controller-runtime-args-b64=`,
		`--certificate-runtime-args-b64=`,
		`--runtime-deployment-config-expressions-b64=`,
		`--runtime-pod-config-expressions-b64=`,
		`--verify-controller-state=true`,
		`variables.isController ? has(object.spec.nodeSelector) : true`,
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("runtime Pod contract does not contain %q", required)
		}
	}
	if strings.Contains(contract, "kube-api-access-") {
		t.Fatal("runtime Pod contract admits API-server-injected credentials instead of the explicit projected volume")
	}
	if got := policy.Annotations[runtimePodContractDigestAnnotation]; !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64 {
		t.Fatalf("runtime Pod contract digest = %q", got)
	}
}

func TestRuntimePodIdentityBindingUsesActivationParameter(t *testing.T) {
	guard := runtimePodGuardFixture()
	binding, err := guard.runtimePodIdentityBinding()
	if err != nil {
		t.Fatal(err)
	}
	if binding.Spec.PolicyName != RuntimePodGuardPolicyName(guard.ReleaseSequence) || binding.Spec.ParamRef == nil ||
		binding.Spec.ParamRef.Name != ReleaseActivationName || binding.Spec.ParamRef.Namespace != guard.ReleaseNamespace ||
		binding.Spec.ParamRef.ParameterNotFoundAction == nil || *binding.Spec.ParamRef.ParameterNotFoundAction != admissionregistrationv1.DenyAction ||
		!reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) {
		t.Fatalf("runtime Pod binding does not use the fail-closed activation parameter: %#v", binding.Spec)
	}
}

// This is intentionally a white-box test because match scoping is part of the
// internal retained-policy contract and cannot be observed through public API.
func TestRuntimePodIdentityPolicyScopesOptionalServiceAccount(t *testing.T) {
	t.Parallel()

	guard := runtimePodGuardFixture()
	policy, err := guard.runtimePodIdentityPolicy()
	if err != nil {
		t.Fatal(err)
	}
	wantMatch := fmt.Sprintf(
		`request.namespace == %q && (((!has(request.subResource) || request.subResource == "") && ((has(object.spec.serviceAccountName) && object.spec.serviceAccountName in [%q, %q]) || (request.operation == "UPDATE" && has(oldObject.spec.serviceAccountName) && oldObject.spec.serviceAccountName in [%q, %q]))) || (has(request.subResource) && request.subResource != "" && (request.name.startsWith(%q) || request.name.startsWith(%q))))`,
		guard.ReleaseNamespace,
		guard.ControllerServiceAccountName,
		guard.CertificateDeploymentName,
		guard.ControllerServiceAccountName,
		guard.CertificateDeploymentName,
		guard.ControllerDeploymentName+"-",
		guard.CertificateDeploymentName+"-",
	)
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatal("runtime Pod identity policy is not fail-closed")
	}
	if len(policy.Spec.MatchConditions) != 1 || policy.Spec.MatchConditions[0].Expression != wantMatch {
		t.Fatalf("optional ServiceAccount match condition\n got: %#v\nwant: %q", policy.Spec.MatchConditions, wantMatch)
	}
	wantVariables := map[string]string{
		"isController":  fmt.Sprintf(`(!has(request.subResource) || request.subResource == "") && has(object.spec.serviceAccountName) && object.spec.serviceAccountName == %q`, guard.ControllerServiceAccountName),
		"isCertificate": fmt.Sprintf(`(!has(request.subResource) || request.subResource == "") && has(object.spec.serviceAccountName) && object.spec.serviceAccountName == %q`, guard.CertificateDeploymentName),
	}
	for _, variable := range policy.Spec.Variables {
		if want, ok := wantVariables[variable.Name]; ok {
			if variable.Expression != want {
				t.Fatalf("%s variable = %q, want %q", variable.Name, variable.Expression, want)
			}
			delete(wantVariables, variable.Name)
		}
	}
	if len(wantVariables) != 0 {
		t.Fatalf("runtime identity variables are missing: %#v", wantVariables)
	}
	binding, err := guard.runtimePodIdentityBinding()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) {
		t.Fatalf("binding validation actions = %#v, want Deny", binding.Spec.ValidationActions)
	}
}

func TestRuntimePodIdentityContractRejectsIncoherentRuntimeArguments(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RolloutGuard)
		want   string
	}{
		{name: "missing controller args", mutate: func(g *RolloutGuard) { g.ControllerArgs = nil }, want: "controller runtime args are required"},
		{name: "shared runtime ServiceAccount", mutate: func(g *RolloutGuard) { g.ControllerServiceAccountName = g.CertificateDeploymentName }, want: "must differ"},
		{name: "missing Deployment config", mutate: func(g *RolloutGuard) { g.RuntimeDeploymentConfigExpressions = nil }, want: "runtime Deployment config expressions are required"},
		{name: "missing Pod config", mutate: func(g *RolloutGuard) { g.RuntimePodConfigExpressions = nil }, want: "runtime Pod config expressions are required"},
		{name: "missing admission preflight contract", mutate: func(g *RolloutGuard) { g.RuntimeAdmissionContractB64 = "" }, want: "runtime admission contract is required"},
		{name: "missing Secret", mutate: func(g *RolloutGuard) { g.CertificateArgs = removeRuntimeArg(g.CertificateArgs, "--secret-name=") }, want: "runtime arg --secret-name= is required"},
		{name: "duplicate Secret", mutate: func(g *RolloutGuard) { g.CertificateArgs = append(g.CertificateArgs, "--secret-name=other") }, want: "runtime arg --secret-name= is duplicated"},
		{name: "foreign Secret", mutate: func(g *RolloutGuard) { replaceRuntimeArg(g.CertificateArgs, "--secret-name=", "--secret-name=foreign") }, want: "differs from rollout identity"},
		{name: "foreign webhook port", mutate: func(g *RolloutGuard) { replaceRuntimeArg(g.ControllerArgs, "--webhook-port=", "--webhook-port=9444") }, want: "differs from rollout identity"},
		{name: "invalid health port", mutate: func(g *RolloutGuard) {
			replaceRuntimeArg(g.CertificateArgs, "--health-bind-address=:", "--health-bind-address=:0")
		}, want: "must contain a port"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guard := runtimePodGuardFixture()
			test.mutate(guard)
			_, err := guard.runtimePodIdentityPolicy()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("runtimePodIdentityPolicy error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRuntimePodIdentityVerificationRejectsSpecOrDigestMutation(t *testing.T) {
	guard := runtimePodGuardFixture()
	policy, err := guard.runtimePodIdentityPolicy()
	if err != nil {
		t.Fatal(err)
	}
	policy.Annotations["helm.sh/hook"] = "pre-install,pre-upgrade"
	policy.Annotations["helm.sh/hook-weight"] = "-50"
	policy.Annotations["helm.sh/resource-policy"] = "keep"
	if err := guard.verifyRuntimePodIdentityPolicy(policy); err != nil {
		t.Fatalf("valid retained policy: %v", err)
	}

	tampered := policy.DeepCopy()
	tampered.Spec.Validations[0].Expression = "true"
	if err := guard.verifyRuntimePodIdentityPolicy(tampered); err == nil || !strings.Contains(err.Error(), "declared contract") {
		t.Fatalf("tampered policy error = %v", err)
	}
	tampered = policy.DeepCopy()
	tampered.Annotations[runtimePodContractDigestAnnotation] = "sha256:" + strings.Repeat("0", 64)
	if err := guard.verifyRuntimePodIdentityPolicy(tampered); err == nil || !strings.Contains(err.Error(), "candidate executable contract") {
		t.Fatalf("tampered digest error = %v", err)
	}
	changedConfig := *guard
	changedConfig.RuntimePodConfigExpressions = []string{"variables.isController ? !has(object.spec.nodeSelector) : true"}
	if err := changedConfig.verifyRuntimePodIdentityPolicy(policy); err == nil || !strings.Contains(err.Error(), "candidate executable contract") {
		t.Fatalf("changed config digest error = %v", err)
	}
	changedAdmission := *guard
	changedAdmission.RuntimeAdmissionContractB64 = "eyJjaGFuZ2VkIjp0cnVlfQ=="
	if err := changedAdmission.verifyRuntimePodIdentityPolicy(policy); err == nil || !strings.Contains(err.Error(), "candidate executable contract") {
		t.Fatalf("changed admission contract digest error = %v", err)
	}
}

func TestRenderedRuntimePodGuardMatchesCompiledContract(t *testing.T) {
	path := os.Getenv("PTAH_RUNTIME_POD_GUARD_RENDER")
	if path == "" {
		t.Skip("PTAH_RUNTIME_POD_GUARD_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	policies := map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}
	bindings := map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	deployments := map[string]*appsv1.Deployment{}
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(rendered))
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var typeMeta metav1.TypeMeta
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			t.Fatal(err)
		}
		switch typeMeta.Kind {
		case "Deployment":
			var deployment appsv1.Deployment
			if err := json.Unmarshal(raw, &deployment); err != nil {
				t.Fatal(err)
			}
			deployments[deployment.Name] = &deployment
		case "ValidatingAdmissionPolicy":
			var policy admissionregistrationv1.ValidatingAdmissionPolicy
			if err := json.Unmarshal(raw, &policy); err != nil {
				t.Fatal(err)
			}
			policies[policy.Name] = &policy
		case "ValidatingAdmissionPolicyBinding":
			var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &binding); err != nil {
				t.Fatal(err)
			}
			bindings[binding.Name] = &binding
		}
	}

	managerImage := "ghcr.io/stokaro/ptah-operator@sha256:" + strings.Repeat("2", 64)
	controller := deployments["ptah-e2e-ptah-operator"]
	certificate := deployments["ptah-e2e-ptah-operator-cert-rotator"]
	if controller == nil || certificate == nil || controller.Spec.Replicas == nil || len(controller.Spec.Template.Spec.Containers) != 1 || len(certificate.Spec.Template.Spec.Containers) != 1 {
		t.Fatal("rendered runtime Deployments are missing their single application containers")
	}
	guard := runtimePodGuardFixture()
	guard.ReleaseName = "ptah-e2e"
	guard.ReleaseNamespace = "ptah-e2e"
	guard.CoordinationNamespace = "ptah-e2e"
	guard.WebhookServiceName = "ptah-e2e-ptah-operator-webhook"
	guard.WebhookTimeoutSeconds = 5
	guard.WebhookSecretName = "ptah-e2e-ptah-operator-webhook-cert"
	guard.HookServiceAccountName = "ptah-e2e-ptah-operator-crd-v1-" + hookIdentityDigest("ptah-e2e", "ptah-e2e", 1, managerImage)[:12]
	guard.ControllerServiceAccountName = "ptah-e2e-ptah-operator"
	guard.ControllerDeploymentName = controller.Name
	guard.ControllerReplicas = *controller.Spec.Replicas
	guard.CertificateDeploymentName = certificate.Name
	guard.ManagerImage = managerImage
	guard.ControllerArgs = append([]string(nil), controller.Spec.Template.Spec.Containers[0].Args...)
	guard.CertificateArgs = append([]string(nil), certificate.Spec.Template.Spec.Containers[0].Args...)
	if len(controller.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatal("rendered controller Deployment is missing its runtime verifier")
	}
	guard.RuntimeDeploymentConfigExpressions = decodeRenderedRuntimeExpressions(t, controller.Spec.Template.Spec.InitContainers[0].Args, "--runtime-deployment-config-expressions-b64=")
	guard.RuntimePodConfigExpressions = decodeRenderedRuntimeExpressions(t, controller.Spec.Template.Spec.InitContainers[0].Args, "--runtime-pod-config-expressions-b64=")
	guard.RuntimeAdmissionContractB64 = decodedManagerStringArgument(t, controller.Spec.Template.Spec.InitContainers[0].Args, "--runtime-admission-contract-b64=")
	name := RuntimePodGuardPolicyName(guard.ReleaseSequence)
	wantPolicy, err := guard.runtimePodIdentityPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if got := policies[name]; got == nil {
		t.Fatalf("rendered runtime Pod policy %s is missing", name)
	} else if mismatch := runtimePodPolicySpecMismatch(got.Spec, wantPolicy.Spec); mismatch != "" {
		t.Fatal(mismatch)
	} else if err := guard.verifyRuntimePodIdentityPolicy(got); err != nil {
		t.Fatalf("rendered runtime Pod policy: %v", err)
	}
	if err := guard.verifyRuntimePodIdentityBinding(bindings[name]); err != nil {
		t.Fatalf("rendered runtime Pod binding: %v", err)
	}
}

func runtimePodGuardFixture() *RolloutGuard {
	managerImage := "registry.example/ptah@sha256:" + strings.Repeat("a", 64)
	guard := &RolloutGuard{
		Policies:                     &rolloutPolicyClient{},
		Bindings:                     &rolloutBindingClient{},
		ReleaseName:                  "ptah",
		ReleaseNamespace:             "ptah-system",
		CoordinationNamespace:        "ptah-system",
		LeaderElection:               true,
		LeaderElectionID:             "ptah-operator.operator.ptah.dev",
		WebhookServiceName:           "ptah-webhook",
		WebhookTimeoutSeconds:        10,
		WebhookSecretName:            "ptah-webhook-cert",
		WebhookPort:                  9443,
		CertificateHealthPort:        8081,
		ControllerServiceAccountName: "ptah-controller",
		ControllerDeploymentName:     "ptah-controller",
		ControllerReplicas:           1,
		CertificateDeploymentName:    "ptah-cert-rotator",
		ControllerStateVersion:       1,
		AdmissionContractVersion:     1,
		ReleaseSequence:              1,
		ManagerImage:                 managerImage,
		RuntimeDeploymentConfigExpressions: []string{
			`variables.isController ? has(object.spec.replicas) : true`,
		},
		RuntimePodConfigExpressions: []string{
			`variables.isController ? has(object.spec.nodeSelector) : true`,
		},
		RuntimeAdmissionContractB64: testRuntimeAdmissionContractB64,
		PollEvery:                   time.Millisecond,
	}
	guard.HookServiceAccountName = "ptah-crd-v1-" + hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)[:12]
	guard.ControllerArgs = []string{
		"--leader-elect=true",
		"--metrics-bind-address=:8080",
		"--health-probe-bind-address=:8081",
		"--webhook-port=9443",
		"--webhook-cert-dir=/certs",
		"--controller-image=" + managerImage,
		"--executor-image=registry.example/executor@sha256:" + strings.Repeat("b", 64),
		"--runner-image=registry.example/runner@sha256:" + strings.Repeat("c", 64),
		"--ptah-version=test",
		"--target-lock-namespace=ptah-system",
		"--default-tolerations-enabled=true",
		"--default-not-ready-toleration-seconds=300",
		"--default-unreachable-toleration-seconds=300",
		"--extended-resource-toleration-enabled=false",
		"--always-pull-images-enabled=false",
	}
	guard.CertificateArgs = []string{
		"--namespace=ptah-system",
		"--secret-name=ptah-webhook-cert",
		"--lease-name=ptah-cert-rotation",
		"--mutating-webhook-configuration=ptah-operator-admission",
		"--mutating-webhook-names=mapproval.operator.ptah.dev",
		"--validating-webhook-configuration=ptah-operator-admission",
		"--validating-webhook-names=vapproval.operator.ptah.dev,vpodintent.operator.ptah.dev",
		"--service-name=ptah-webhook",
		"--service-namespace=ptah-system",
		"--endpoint-port-name=https",
		"--holder-identity=$(POD_NAME)/$(POD_UID)",
		"--run-interval=6h",
		"--operation-timeout=15m",
		"--retry-initial=5s",
		"--retry-max=5m",
		"--health-bind-address=:8081",
		"--renewal-threshold=720h",
		"--serving-certificate-validity=2160h",
		"--ca-certificate-validity=26280h",
		"--probe-timeout=5m",
		"--probe-interval=2s",
		"--lease-duration=10m",
		"--lease-acquire-timeout=30s",
	}
	return guard
}

func decodeRenderedRuntimeExpressions(t *testing.T, args []string, prefix string) []string {
	t.Helper()
	encoded := ""
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			if encoded != "" {
				t.Fatalf("rendered runtime verifier argument %s is duplicated", prefix)
			}
			encoded = strings.TrimPrefix(arg, prefix)
		}
	}
	if encoded == "" {
		t.Fatalf("rendered runtime verifier argument %s is missing", prefix)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode rendered runtime verifier argument %s: %v", prefix, err)
	}
	var expressions []string
	if err := json.Unmarshal(raw, &expressions); err != nil {
		t.Fatalf("parse rendered runtime verifier argument %s: %v", prefix, err)
	}
	if len(expressions) == 0 {
		t.Fatalf("rendered runtime verifier argument %s is empty", prefix)
	}
	return expressions
}

func removeRuntimeArg(args []string, prefix string) []string {
	result := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, prefix) {
			result = append(result, arg)
		}
	}
	return result
}

func replaceRuntimeArg(args []string, prefix, replacement string) {
	for index := range args {
		if strings.HasPrefix(args[index], prefix) {
			args[index] = replacement
			return
		}
	}
}

func runtimePodPolicySpecMismatch(got, want admissionregistrationv1.ValidatingAdmissionPolicySpec) string {
	if !reflect.DeepEqual(got.ParamKind, want.ParamKind) || !reflect.DeepEqual(got.FailurePolicy, want.FailurePolicy) ||
		!reflect.DeepEqual(got.MatchConstraints, want.MatchConstraints) || !reflect.DeepEqual(got.MatchConditions, want.MatchConditions) ||
		!reflect.DeepEqual(got.AuditAnnotations, want.AuditAnnotations) {
		return "rendered runtime Pod policy fixed fields differ from the compiled contract"
	}
	if len(got.Variables) != len(want.Variables) {
		return "rendered runtime Pod policy variable count differs from the compiled contract"
	}
	for index := range want.Variables {
		if !reflect.DeepEqual(got.Variables[index], want.Variables[index]) {
			return "rendered runtime Pod policy variable " + want.Variables[index].Name + " differs from the compiled contract"
		}
	}
	if len(got.Validations) != len(want.Validations) {
		return "rendered runtime Pod policy validation count differs from the compiled contract"
	}
	for index := range want.Validations {
		if !reflect.DeepEqual(got.Validations[index], want.Validations[index]) {
			return "rendered runtime Pod policy validation at index " + string(rune('0'+index)) + " differs from the compiled contract"
		}
	}
	return ""
}
