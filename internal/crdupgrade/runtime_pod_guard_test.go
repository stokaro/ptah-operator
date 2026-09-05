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
	"strconv"
	"strings"
	"testing"
	"time"

	celgo "github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
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
		`object.spec.serviceAccountName == \"ptah-controller-v1\"`,
		`object.spec.serviceAccountName == \"ptah-cert-rotator\"`,
		`variables.activationValid`,
		`variables.newRelease == variables.activeRelease`,
		`variables.newState == variables.activeState`,
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

// This is intentionally a white-box test because probe default normalization
// is part of the generated admission contract rather than an exported Go API.
func TestRuntimeHTTPProbeExpressionNormalizesOmittedInitialDelay(t *testing.T) {
	t.Parallel()

	expression := runtimeHTTPProbeExpression("object.spec.containers[0].readinessProbe", "/readyz", 0, 5, 1, 1)
	tests := []struct {
		name         string
		includeDelay bool
		delay        int64
		want         bool
	}{
		{name: "absent", want: true},
		{name: "explicit zero", includeDelay: true, want: true},
		{name: "nonzero", includeDelay: true, delay: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe := map[string]any{
				"httpGet": map[string]any{
					"path":   "/readyz",
					"port":   "health",
					"scheme": "HTTP",
				},
				"periodSeconds":    int64(5),
				"timeoutSeconds":   int64(1),
				"successThreshold": int64(1),
				"failureThreshold": int64(1),
			}
			if test.includeDelay {
				probe["initialDelaySeconds"] = test.delay
			}
			object := map[string]any{
				"spec": map[string]any{
					"containers": []any{map[string]any{"readinessProbe": probe}},
				},
			}
			result := evaluateRolloutCEL(t, expression, map[string]any{"object": object}, nil)
			got, ok := result.(bool)
			if !ok {
				t.Fatalf("probe CEL result = %T(%v), want bool", result, result)
			}
			if got != test.want {
				t.Fatalf("probe CEL decision = %t, want %t", got, test.want)
			}
		})
	}
}

// This is intentionally a white-box test because node assignment immutability
// is encoded in an internal admission expression rather than an exported API.
func TestRuntimePodNodeNameExpressionPreservesPresenceAndValue(t *testing.T) {
	t.Parallel()

	nodeName := func(value string) *string { return &value }
	tests := []struct {
		name        string
		oldNodeName *string
		newNodeName *string
		want        bool
	}{
		{name: "both absent", want: true},
		{name: "added", newNodeName: nodeName("worker-b")},
		{name: "removed", oldNodeName: nodeName("worker-a")},
		{name: "changed", oldNodeName: nodeName("worker-a"), newNodeName: nodeName("worker-b")},
		{name: "unchanged", oldNodeName: nodeName("worker-a"), newNodeName: nodeName("worker-a"), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldSpec := map[string]any{}
			newSpec := map[string]any{}
			if test.oldNodeName != nil {
				oldSpec["nodeName"] = *test.oldNodeName
			}
			if test.newNodeName != nil {
				newSpec["nodeName"] = *test.newNodeName
			}
			result := evaluateRolloutCEL(t, runtimePodNodeNameExpression(), map[string]any{
				"object":    map[string]any{"spec": newSpec},
				"oldObject": map[string]any{"spec": oldSpec},
				"request":   map[string]any{"operation": "UPDATE"},
			}, nil)
			got, ok := result.(bool)
			if !ok {
				t.Fatalf("node-name CEL result = %T(%v), want bool", result, result)
			}
			if got != test.want {
				t.Fatalf("node-name CEL decision = %t, want %t", got, test.want)
			}
		})
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
	shortNameShape := func(deploymentName string) string {
		prefix := deploymentName + "-"
		return fmt.Sprintf(
			`((request.name.startsWith(%q) && request.name.substring(%d).matches("^[a-z0-9]{1,10}-[a-z0-9]{5}$")))`,
			prefix,
			len(prefix),
		)
	}
	wantMatch := fmt.Sprintf(
		`request.namespace == %q && (((!has(request.subResource) || request.subResource == "") && ((has(object.spec.serviceAccountName) && object.spec.serviceAccountName in [%q, %q]) || (request.operation == "UPDATE" && has(oldObject.spec.serviceAccountName) && oldObject.spec.serviceAccountName in [%q, %q]))) || (has(request.subResource) && request.subResource != "" && (%s || %s)))`,
		guard.ReleaseNamespace,
		guard.ControllerServiceAccountName,
		guard.CertificateDeploymentName,
		guard.ControllerServiceAccountName,
		guard.CertificateDeploymentName,
		shortNameShape(guard.ControllerDeploymentName),
		shortNameShape(guard.CertificateDeploymentName),
	)
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatal("runtime Pod identity policy is not fail-closed")
	}
	native := stripAdmissionConvergenceDependencyProbe(t, policy)
	if len(native.Spec.MatchConditions) != 2 || native.Spec.MatchConditions[0].Expression != wantMatch ||
		native.Spec.MatchConditions[1].Name != "activation-gated-runtime-pod" ||
		native.Spec.MatchConditions[1].Expression != guard.runtimePodActivationMatchExpression() {
		t.Fatalf("optional ServiceAccount match condition\n got: %#v\nwant: %q", native.Spec.MatchConditions, wantMatch)
	}
	wantVariables := map[string]string{
		"isController":    fmt.Sprintf(`(!has(request.subResource) || request.subResource == "") && has(object.spec.serviceAccountName) && object.spec.serviceAccountName == %q`, guard.ControllerServiceAccountName),
		"isCertificate":   fmt.Sprintf(`(!has(request.subResource) || request.subResource == "") && has(object.spec.serviceAccountName) && object.spec.serviceAccountName == %q`, guard.CertificateDeploymentName),
		"activationValid": guard.releaseActivationParameterShapeExpression(),
	}
	for _, variable := range native.Spec.Variables {
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

// This is intentionally a white-box test because skipping the candidate
// policy for predecessor Pods before activation is an API-server overlap
// contract, not an exported Go behavior.
func TestRuntimePodActivationTruthTable(t *testing.T) {
	t.Parallel()

	guard := runtimePodGuardFixture()
	guard.ReleaseSequence = 2
	guard.ControllerStateVersion = 2
	guard.AdmissionContractVersion = 2
	guard.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("2", 64)
	guard.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)[:12]
	policy, err := guard.runtimePodIdentityPolicy()
	if err != nil {
		t.Fatal(err)
	}
	assertAdmissionPolicyCELHeadroom(t, "activation-aware runtime Pod policy", policy)

	tests := []struct {
		name        string
		active      int64
		activeState int64
		marker      int64
		markerState int64
		operation   string
		subresource string
		actor       string
		params      func(map[string]any) any
		wantMatch   bool
		wantAllow   bool
	}{
		{name: "bootstrap annotation-free create is unaffected", operation: "CREATE", actor: "unrelated"},
		{name: "bootstrap candidate create is denied", marker: 2, markerState: 2, operation: "CREATE", actor: "system:serviceaccount:kube-system:replicaset-controller", wantMatch: true},
		{name: "active predecessor create is unaffected", active: 1, activeState: 1, marker: 1, markerState: 1, operation: "CREATE", actor: "unrelated"},
		{name: "active predecessor update is unaffected", active: 1, activeState: 1, marker: 1, markerState: 1, operation: "UPDATE", actor: "system:node:test"},
		{name: "active predecessor connect is unaffected", active: 1, activeState: 1, marker: 1, markerState: 1, operation: "CONNECT", subresource: "exec", actor: "developer"},
		{name: "candidate create before activation is denied", active: 1, activeState: 1, marker: 2, markerState: 2, operation: "CREATE", actor: "system:serviceaccount:kube-system:replicaset-controller", wantMatch: true},
		{name: "future create before activation is denied", active: 1, activeState: 1, marker: 3, markerState: 3, operation: "CREATE", actor: "system:serviceaccount:kube-system:replicaset-controller", wantMatch: true},
		{name: "forged predecessor state is denied", active: 1, activeState: 1, marker: 1, markerState: 9, operation: "UPDATE", actor: "system:node:test", wantMatch: true},
		{name: "activated candidate reaches exact contract", active: 2, activeState: 2, marker: 2, markerState: 2, operation: "CREATE", actor: "system:serviceaccount:kube-system:replicaset-controller", wantMatch: true, wantAllow: true},
		{name: "activated predecessor is denied", active: 2, activeState: 2, marker: 1, markerState: 1, operation: "UPDATE", actor: "system:node:test", wantMatch: true},
		{name: "activated connect is denied", active: 2, activeState: 2, marker: 2, markerState: 2, operation: "CONNECT", subresource: "exec", actor: "developer", wantMatch: true},
		{name: "future active release passes retained gate", active: 3, activeState: 3, marker: 3, markerState: 3, operation: "UPDATE", actor: "system:node:test", wantMatch: true, wantAllow: true},
		{name: "malformed activation fails closed", active: 1, activeState: 1, marker: 1, markerState: 1, operation: "UPDATE", actor: "system:node:test", params: func(params map[string]any) any {
			params["metadata"].(map[string]any)["labels"].(map[string]any)["app.kubernetes.io/component"] = "foreign"
			return params
		}, wantMatch: true},
		{name: "missing activation fails closed", marker: 2, markerState: 2, operation: "CREATE", actor: "system:serviceaccount:kube-system:replicaset-controller", params: func(map[string]any) any { return nil }, wantMatch: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			activeState := test.activeState
			if activeState == 0 {
				activeState = int64(guard.ControllerStateVersion)
			}
			paramsRelease := test.active
			if paramsRelease == 0 {
				paramsRelease = int64(guard.ReleaseSequence)
			}
			params := rolloutActivationCELObject(guard, test.active, activeState, activeState, paramsRelease, guard.ManagerImage)
			var parameter any = params
			if test.params != nil {
				parameter = test.params(params)
			}
			object := runtimePodActivationCELObject(guard, test.marker, test.markerState)
			oldObject := runtimePodActivationCELObject(guard, test.marker, test.markerState)
			request := runtimePodActivationCELRequest(guard, test.operation, test.subresource, test.actor)
			matched := evaluatePolicyMatchConditions(t, policy, object, oldObject, request, parameter)
			if matched != test.wantMatch {
				t.Fatalf("policy match = %t, want %t", matched, test.wantMatch)
			}
			if !matched {
				return
			}
			if got := evaluatePolicyValidationPrefix(t, policy, 5, object, oldObject, request, parameter); got != test.wantAllow {
				t.Fatalf("activation gate decision = %t, want %t", got, test.wantAllow)
			}
		})
	}
}

func runtimePodActivationCELObject(g *RolloutGuard, marker, state int64) map[string]any {
	metadata := map[string]any{
		"name": g.ControllerDeploymentName + "-abc12-xy789",
	}
	if marker > 0 {
		metadata["annotations"] = map[string]any{
			ControllerStateVersionAnnotation: strconv.FormatInt(state, 10),
			ReleaseSequenceAnnotation:        strconv.FormatInt(marker, 10),
		}
	}
	return map[string]any{
		"metadata": metadata,
		"spec": map[string]any{
			"serviceAccountName": g.ControllerServiceAccountName,
		},
	}
}

func runtimePodActivationCELRequest(g *RolloutGuard, operation, subresource, actor string) map[string]any {
	request := map[string]any{
		"operation": operation,
		"namespace": g.ReleaseNamespace,
		"name":      g.ControllerDeploymentName + "-abc12-xy789",
		"userInfo":  map[string]any{"username": actor},
	}
	if subresource != "" {
		request["subResource"] = subresource
	}
	return request
}

// This is intentionally a white-box test because subresource requests do not
// carry the parent ReplicaSet object, so their retained-policy name boundary is
// observable only through the compiled CEL expression.
func TestRuntimePodRequestNameExpressionMatchesGeneratedNameBoundaries(t *testing.T) {
	t.Parallel()

	environment, err := celgo.NewEnv(
		celgo.Variable("podName", celgo.StringType),
		ext.Strings(),
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name           string
		deploymentName string
		podName        string
		want           bool
	}{
		{name: "short name", deploymentName: "ptah-controller", podName: "ptah-controller-abc12-xy789", want: true},
		{name: "minimum hash", deploymentName: "ptah-controller", podName: "ptah-controller-a-012ab", want: true},
		{name: "maximum hash", deploymentName: "ptah-controller", podName: "ptah-controller-abcdefghij-012ab", want: true},
		{name: "empty hash", deploymentName: "ptah-controller", podName: "ptah-controller--012ab"},
		{name: "long hash", deploymentName: "ptah-controller", podName: "ptah-controller-abcdefghijk-012ab"},
		{name: "short suffix", deploymentName: "ptah-controller", podName: "ptah-controller-abc12-012a"},
		{name: "invalid suffix alphabet", deploymentName: "ptah-controller", podName: "ptah-controller-abc12-012A_"},
		{name: "extra suffix", deploymentName: "ptah-controller", podName: "ptah-controller-abc12-012abc"},
		{
			name:           "58-character untruncated generateName",
			deploymentName: strings.Repeat("a", 46),
			podName:        strings.Repeat("a", 46) + "-abcdefghij-012ab",
			want:           true,
		},
		{
			name:           "truncated at ReplicaSet hash boundary",
			deploymentName: strings.Repeat("b", 47),
			podName:        strings.Repeat("b", 47) + "-abcdefghij012ab",
			want:           true,
		},
		{
			name:           "untruncated alternative beside boundary",
			deploymentName: strings.Repeat("b", 47),
			podName:        strings.Repeat("b", 47) + "-abcdefghi-012ab",
			want:           true,
		},
		{
			name:           "unexpected separator after truncation",
			deploymentName: strings.Repeat("b", 47),
			podName:        strings.Repeat("b", 47) + "-abcdefghij-012ab",
		},
		{
			name:           "long Deployment",
			deploymentName: strings.Repeat("c", 60),
			podName:        strings.Repeat("c", 58) + "012ab",
			want:           true,
		},
		{
			name:           "wrong long Deployment prefix",
			deploymentName: strings.Repeat("c", 60),
			podName:        strings.Repeat("c", 57) + "x012ab",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ast, issues := environment.Compile(runtimePodRequestNameExpression("podName", test.deploymentName))
			if issues != nil && issues.Err() != nil {
				t.Fatalf("compile runtime Pod request-name CEL: %v", issues.Err())
			}
			program, err := environment.Program(ast)
			if err != nil {
				t.Fatalf("build runtime Pod request-name CEL program: %v", err)
			}
			result, _, err := program.Eval(map[string]any{"podName": test.podName})
			if err != nil {
				t.Fatalf("evaluate runtime Pod request-name CEL: %v", err)
			}
			got, ok := result.Value().(bool)
			if !ok {
				t.Fatalf("runtime Pod request-name CEL result = %T(%v), want bool", result.Value(), result.Value())
			}
			if got != test.want {
				t.Fatalf("runtime Pod request-name CEL = %t, want %t for %q", got, test.want, test.podName)
			}
		})
	}
}

// This is intentionally a white-box test because the complete generated-name
// and owner chain is an internal retained-policy invariant.
func TestRuntimePodMetadataExpressionPinsReplicaSetGeneratedName(t *testing.T) {
	t.Parallel()

	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		ext.Strings(),
	)
	if err != nil {
		t.Fatal(err)
	}
	object := func(replicaSetName, generateName, podName string, includeGenerateName bool) map[string]any {
		metadata := map[string]any{
			"name": podName,
			"labels": map[string]any{
				instanceLabel:                 "ptah",
				"app.kubernetes.io/component": "controller",
			},
			"ownerReferences": []any{map[string]any{
				"apiVersion":         "apps/v1",
				"kind":               "ReplicaSet",
				"name":               replicaSetName,
				"uid":                "replicaset-uid",
				"controller":         true,
				"blockOwnerDeletion": true,
			}},
		}
		if includeGenerateName {
			metadata["generateName"] = generateName
		}
		return map[string]any{"metadata": metadata}
	}
	tests := []struct {
		name           string
		deploymentName string
		object         map[string]any
		want           bool
	}{
		{
			name:           "short generated name",
			deploymentName: "ptah-controller",
			object:         object("ptah-controller-abc12", "ptah-controller-abc12-", "ptah-controller-abc12-xy789", true),
			want:           true,
		},
		{
			name:           "long generated name",
			deploymentName: strings.Repeat("d", 60),
			object: object(
				strings.Repeat("d", 60)+"-abc12",
				strings.Repeat("d", 60)+"-abc12-",
				strings.Repeat("d", 58)+"xy789",
				true,
			),
			want: true,
		},
		{
			name:           "missing generateName",
			deploymentName: "ptah-controller",
			object:         object("ptah-controller-abc12", "", "ptah-controller-abc12-xy789", false),
		},
		{
			name:           "foreign generateName tail",
			deploymentName: "ptah-controller",
			object:         object("ptah-controller-abc12", "ptah-controller-foreign-", "ptah-controller-foreign-xy789", true),
		},
		{
			name:           "invalid ReplicaSet hash",
			deploymentName: "ptah-controller",
			object:         object("ptah-controller-ab_12", "ptah-controller-ab_12-", "ptah-controller-ab_12-xy789", true),
		},
		{
			name:           "invalid generated suffix",
			deploymentName: "ptah-controller",
			object:         object("ptah-controller-abc12", "ptah-controller-abc12-", "ptah-controller-abc12-xy78_", true),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			guard := runtimePodGuardFixture()
			guard.ControllerDeploymentName = test.deploymentName
			ast, issues := environment.Compile(guard.runtimePodMetadataExpression(test.deploymentName, "controller"))
			if issues != nil && issues.Err() != nil {
				t.Fatalf("compile runtime Pod metadata CEL: %v", issues.Err())
			}
			program, err := environment.Program(ast)
			if err != nil {
				t.Fatalf("build runtime Pod metadata CEL program: %v", err)
			}
			result, _, err := program.Eval(map[string]any{"object": test.object})
			if err != nil {
				t.Fatalf("evaluate runtime Pod metadata CEL: %v", err)
			}
			got, ok := result.Value().(bool)
			if !ok {
				t.Fatalf("runtime Pod metadata CEL result = %T(%v), want bool", result.Value(), result.Value())
			}
			if got != test.want {
				t.Fatalf("runtime Pod metadata CEL = %t, want %t", got, test.want)
			}
		})
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
	testRenderedRuntimePodGuardMatchesCompiledContract(
		t,
		"PTAH_RUNTIME_POD_GUARD_RENDER",
		"ptah-e2e-ptah-operator",
		"ptah-e2e-ptah-operator-cert-rotator",
		"ptah-e2e-ptah-operator",
	)
}

func TestRenderedLongNameRuntimePodGuardMatchesCompiledContract(t *testing.T) {
	const controllerName = "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr"
	testRenderedRuntimePodGuardMatchesCompiledContract(
		t,
		"PTAH_RUNTIME_POD_GUARD_LONG_RENDER",
		controllerName,
		controllerName[:39]+"-cert-rotator",
		controllerName[:24],
	)
}

func testRenderedRuntimePodGuardMatchesCompiledContract(t *testing.T, environmentVariable, controllerName, certificateName, hookBase string) {
	t.Helper()
	path := os.Getenv(environmentVariable)
	if path == "" {
		t.Skip(environmentVariable + " is set by the chart contract gate")
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
	controller := deployments[controllerName]
	certificate := deployments[certificateName]
	if controller == nil || certificate == nil || controller.Spec.Replicas == nil || len(controller.Spec.Template.Spec.Containers) != 1 || len(certificate.Spec.Template.Spec.Containers) != 1 {
		t.Fatal("rendered runtime Deployments are missing their single application containers")
	}
	guard := runtimePodGuardFixture()
	guard.ReleaseName = "ptah-e2e"
	guard.ReleaseNamespace = "ptah-e2e"
	guard.CoordinationNamespace = "ptah-e2e"
	guard.WebhookServiceName = truncateTestResourceBase(controllerName, 55) + "-webhook"
	guard.WebhookTimeoutSeconds = 5
	guard.WebhookSecretName = truncateTestResourceBase(controllerName, 50) + "-webhook-cert"
	guard.HookServiceAccountName = hookBase + "-crd-v1-" + hookIdentityDigest("ptah-e2e", "ptah-e2e", 1, managerImage)[:12]
	guard.ControllerServiceAccountName = controller.Spec.Template.Spec.ServiceAccountName
	guard.ControllerServiceAccountManaged = true
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
	assertRuntimeDeploymentMinReadySecondsNormalization(t, guard.RuntimeDeploymentConfigExpressions, controller.Name)
	if environmentVariable == "PTAH_RUNTIME_POD_GUARD_RENDER" {
		assertRuntimeTolerationNormalization(t, guard.RuntimeDeploymentConfigExpressions, "object.spec.template.spec.tolerations", false)
		assertRuntimeTolerationNormalization(t, guard.RuntimePodConfigExpressions, "object.spec.tolerations", true)
	}
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

func assertRuntimeDeploymentMinReadySecondsNormalization(t *testing.T, expressions []string, controllerName string) {
	t.Helper()
	var expression string
	for _, candidate := range expressions {
		if !strings.Contains(candidate, "object.spec.minReadySeconds") {
			continue
		}
		if expression != "" {
			t.Fatal("runtime Deployment config contains multiple minimum-ready-seconds expressions")
		}
		expression = candidate
	}
	if expression == "" {
		t.Fatal("runtime Deployment config lacks a minimum-ready-seconds expression")
	}

	tests := []struct {
		name         string
		includeValue bool
		value        int64
		want         bool
	}{
		{name: "absent", want: true},
		{name: "explicit zero", includeValue: true, want: true},
		{name: "nonzero", includeValue: true, value: 1},
	}
	for _, test := range tests {
		t.Run("minimum ready seconds "+test.name, func(t *testing.T) {
			spec := map[string]any{
				"strategy":                map[string]any{"type": "Recreate"},
				"revisionHistoryLimit":    int64(10),
				"progressDeadlineSeconds": int64(600),
			}
			if test.includeValue {
				spec["minReadySeconds"] = test.value
			}
			result := evaluateRolloutCEL(t, expression, map[string]any{
				"object":  map[string]any{"spec": spec},
				"request": map[string]any{"name": controllerName},
			}, nil)
			got, ok := result.(bool)
			if !ok {
				t.Fatalf("runtime Deployment CEL result = %T(%v), want bool", result, result)
			}
			if got != test.want {
				t.Fatalf("runtime Deployment CEL decision = %t, want %t", got, test.want)
			}
		})
	}
}

func assertRuntimeTolerationNormalization(t *testing.T, expressions []string, path string, includeDefaults bool) {
	t.Helper()
	var expression string
	for _, candidate := range expressions {
		if !strings.Contains(candidate, path) {
			continue
		}
		if expression != "" {
			t.Fatalf("runtime config contains multiple toleration expressions for %s", path)
		}
		expression = candidate
	}
	if expression == "" {
		t.Fatalf("runtime config lacks a toleration expression for %s", path)
	}

	base := func(user map[string]any, explicitDefaultValues bool) []any {
		items := []any{user}
		if !includeDefaults {
			return items
		}
		for _, key := range []string{"node.kubernetes.io/not-ready", "node.kubernetes.io/unreachable"} {
			item := map[string]any{
				"key":               key,
				"operator":          "Exists",
				"effect":            "NoExecute",
				"tolerationSeconds": int64(300),
			}
			if explicitDefaultValues {
				item["value"] = ""
			}
			items = append(items, item)
		}
		return items
	}
	type testCase struct {
		name  string
		items func() []any
		want  bool
	}
	tests := []testCase{
		{
			name:  "omitted legal strings",
			items: func() []any { return base(map[string]any{"key": "dedicated"}, false) },
			want:  true,
		},
		{
			name: "explicit empty strings",
			items: func() []any {
				return base(map[string]any{"key": "dedicated", "operator": "", "value": "", "effect": ""}, true)
			},
			want: true,
		},
		{
			name: "explicit operator default",
			items: func() []any {
				return base(map[string]any{"key": "dedicated", "operator": "Equal", "value": "", "effect": ""}, true)
			},
			want: true,
		},
		{
			name:  "non-equivalent user operator",
			items: func() []any { return base(map[string]any{"key": "dedicated", "operator": "Exists"}, false) },
		},
	}
	if includeDefaults {
		tests = append(tests,
			testCase{
				name: "non-equivalent injected value",
				items: func() []any {
					items := base(map[string]any{"key": "dedicated"}, false)
					items[1].(map[string]any)["value"] = "foreign"
					return items
				},
			},
			testCase{
				name: "injected order changed",
				items: func() []any {
					items := base(map[string]any{"key": "dedicated"}, false)
					items[1], items[2] = items[2], items[1]
					return items
				},
			},
		)
	}
	for _, test := range tests {
		t.Run(path+" "+test.name, func(t *testing.T) {
			items := test.items()
			object := map[string]any{
				"spec": map[string]any{
					"tolerations": items,
					"template": map[string]any{
						"spec": map[string]any{"tolerations": items},
					},
				},
			}
			result := evaluateRolloutCEL(t, expression, map[string]any{"object": object}, nil)
			got, ok := result.(bool)
			if !ok {
				t.Fatalf("toleration CEL result = %T(%v), want bool", result, result)
			}
			if got != test.want {
				t.Fatalf("toleration CEL decision = %t, want %t", got, test.want)
			}
		})
	}
}

func truncateTestResourceBase(name string, limit int) string {
	if len(name) > limit {
		name = name[:limit]
	}
	return strings.TrimSuffix(name, "-")
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
		ControllerServiceAccountName: "ptah-controller-v1",
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
		"--controller-service-account-username=system:serviceaccount:ptah-system:ptah-controller-v1",
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
		"--validating-webhook-names=vapproval.operator.ptah.dev,vpodintent.operator.ptah.dev,vcontrollerwrite.operator.ptah.dev",
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
