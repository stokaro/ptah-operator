package crdupgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	celgo "github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestParentWorkloadGuardSeparatesStableOriginAndExactCandidateContracts(t *testing.T) {
	rollout := runtimePodGuardFixture()
	guard := NewParentWorkloadGuard(rollout)
	replicaSet := guard.replicaSetPolicy()
	hookOrigin := guard.hookJobOriginPolicy()
	hookPodOrigin := guard.hookPodOriginPolicy()
	hookContract := guard.hookJobContractPolicy()

	for name, policy := range map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{
		"ReplicaSet":        replicaSet,
		"hook Job origin":   hookOrigin,
		"hook Pod origin":   hookPodOrigin,
		"hook Job contract": hookContract,
	} {
		if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail ||
			policy.Spec.MatchConstraints == nil || policy.Spec.MatchConstraints.MatchPolicy == nil ||
			*policy.Spec.MatchConstraints.MatchPolicy != admissionregistrationv1.Exact {
			t.Fatalf("%s policy is not explicitly fail-closed with exact matching", name)
		}
		native := stripParentAdmissionConvergenceDependencyProbe(t, policy)
		if len(native.Spec.Validations) == 0 {
			t.Fatalf("%s policy has no fail-closed validation contract", name)
		}
	}

	otherRollout := *rollout
	otherRollout.ReleaseSequence = 2
	otherRollout.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	otherRollout.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(otherRollout.ReleaseNamespace, otherRollout.ReleaseName, otherRollout.ReleaseSequence, otherRollout.ManagerImage)[:12]
	otherRollout.ControllerServiceAccountName = "ptah-controller-v2"
	other := NewParentWorkloadGuard(&otherRollout)
	if replicaSet.Name == other.replicaSetPolicy().Name || reflect.DeepEqual(stripParentAdmissionConvergenceDependencyProbe(t, replicaSet).Spec, stripParentAdmissionConvergenceDependencyProbe(t, other.replicaSetPolicy()).Spec) {
		t.Fatal("candidate ReplicaSet parent contract did not change across release identities")
	}
	if hookOrigin.Name != other.hookJobOriginPolicy().Name || !reflect.DeepEqual(hookOrigin.Spec, other.hookJobOriginPolicy().Spec) {
		t.Fatal("stable hook Job origin contract changed across release sequences")
	}
	if hookPodOrigin.Name != other.hookPodOriginPolicy().Name || !reflect.DeepEqual(hookPodOrigin.Spec, other.hookPodOriginPolicy().Spec) {
		t.Fatal("stable hook Pod origin contract changed across release sequences")
	}
	if hookContract.Name == other.hookJobContractPolicy().Name || reflect.DeepEqual(stripParentAdmissionConvergenceDependencyProbe(t, hookContract).Spec, stripParentAdmissionConvergenceDependencyProbe(t, other.hookJobContractPolicy()).Spec) {
		t.Fatal("candidate hook Job contract did not change with release identity")
	}
}

func TestParentOriginV2SpecsAreByteStableAcrossReleaseAttempts(t *testing.T) {
	t.Parallel()

	firstRollout := runtimePodGuardFixture()
	secondRollout := *firstRollout
	secondRollout.ReleaseSequence = firstRollout.ReleaseSequence + 1
	secondRollout.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("c", 64)
	secondRollout.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(secondRollout.ReleaseNamespace, secondRollout.ReleaseName, secondRollout.ReleaseSequence, secondRollout.ManagerImage)[:12]
	secondRollout.ControllerServiceAccountName = "ptah-controller-v2"
	first := NewParentWorkloadGuard(firstRollout)
	second := NewParentWorkloadGuard(&secondRollout)

	for _, test := range []struct {
		name   string
		first  *admissionregistrationv1.ValidatingAdmissionPolicy
		second *admissionregistrationv1.ValidatingAdmissionPolicy
	}{
		{name: "Job origin", first: first.hookJobOriginPolicy(), second: second.hookJobOriginPolicy()},
		{name: "Pod origin", first: first.hookPodOriginPolicy(), second: second.hookPodOriginPolicy()},
	} {
		firstPolicy, err := json.Marshal(test.first.Spec)
		if err != nil {
			t.Fatal(err)
		}
		secondPolicy, err := json.Marshal(test.second.Spec)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(firstPolicy, secondPolicy) {
			t.Fatalf("stable v2 %s policy spec changed across release attempts", test.name)
		}
		firstBinding, err := json.Marshal(first.binding(test.first.Name, false).Spec)
		if err != nil {
			t.Fatal(err)
		}
		secondBinding, err := json.Marshal(second.binding(test.second.Name, false).Spec)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(firstBinding, secondBinding) {
			t.Fatalf("stable v2 %s binding spec changed across release attempts", test.name)
		}
	}

	firstCandidatePolicy, err := json.Marshal(first.hookJobContractPolicy().Spec)
	if err != nil {
		t.Fatal(err)
	}
	secondCandidate := second.hookJobContractPolicy()
	secondCandidatePolicy, err := json.Marshal(secondCandidate.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstCandidatePolicy, secondCandidatePolicy) {
		t.Fatal("candidate Job policy spec did not change across release attempts")
	}
	firstCandidateBinding, err := json.Marshal(first.binding(first.hookJobContractPolicy().Name, true).Spec)
	if err != nil {
		t.Fatal(err)
	}
	secondCandidateBinding, err := json.Marshal(second.binding(secondCandidate.Name, true).Spec)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstCandidateBinding, secondCandidateBinding) {
		t.Fatal("candidate Job binding spec did not change across release attempts")
	}
}

func stripParentAdmissionConvergenceDependencyProbe(
	t *testing.T,
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
) *admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	if policy == nil {
		t.Fatal("parent workload guard policy is nil")
	}
	if strings.HasPrefix(policy.Name, parentHookOriginGuardPrefix) ||
		strings.HasPrefix(policy.Name, parentHookPodOriginPrefix) {
		if len(policy.Spec.Variables) >= 2 &&
			policy.Spec.Variables[0].Name == "isAnyAdmissionConvergenceProbe" &&
			policy.Spec.Variables[1].Name == "isAdmissionConvergenceProbe" {
			t.Fatal("release-stable parent origin guard unexpectedly embeds a candidate convergence probe")
		}
		return policy.DeepCopy()
	}
	if len(policy.Spec.Variables) < 2 ||
		policy.Spec.Variables[0].Name != "isAnyAdmissionConvergenceProbe" ||
		policy.Spec.Variables[1].Name != "isAdmissionConvergenceProbe" ||
		policy.Spec.Variables[0].Expression != policy.Spec.Variables[1].Expression {
		return stripAdmissionConvergenceDependencyProbe(t, policy)
	}

	stripped := policy.DeepCopy()
	probeExpression := stripped.Spec.Variables[0].Expression
	if len(stripped.Spec.MatchConstraints.ResourceRules) == 0 || len(stripped.Spec.Validations) < 2 {
		t.Fatal("stable admission convergence wrapper is incomplete")
	}
	last := stripped.Spec.Validations[len(stripped.Spec.Validations)-2:]
	if last[0].Expression != `!variables.isAnyAdmissionConvergenceProbe || request.dryRun == true` ||
		last[0].Message != admissionConvergenceProbePersistenceMessage ||
		last[1].Expression != `!variables.isAdmissionConvergenceProbe` ||
		last[1].MessageExpression != `"Ptah admission convergence confirmed exact workload guard " + request.options.fieldManager` {
		t.Fatal("stable admission convergence validations differ from the exact wrapper")
	}
	matchPrefix := "(" + probeExpression + ") || ("
	for index := range stripped.Spec.MatchConditions {
		expression := stripped.Spec.MatchConditions[index].Expression
		if !strings.HasPrefix(expression, matchPrefix) || !strings.HasSuffix(expression, ")") {
			t.Fatalf("stable admission convergence match condition %d differs from the exact wrapper", index)
		}
		stripped.Spec.MatchConditions[index].Expression = strings.TrimSuffix(strings.TrimPrefix(expression, matchPrefix), ")")
	}
	validationPrefix := "variables.isAnyAdmissionConvergenceProbe || ("
	for index := range stripped.Spec.Validations[:len(stripped.Spec.Validations)-2] {
		expression := stripped.Spec.Validations[index].Expression
		if !strings.HasPrefix(expression, validationPrefix) || !strings.HasSuffix(expression, ")") {
			t.Fatalf("stable admission convergence validation %d differs from the exact wrapper", index)
		}
		stripped.Spec.Validations[index].Expression = strings.TrimSuffix(strings.TrimPrefix(expression, validationPrefix), ")")
	}
	stripped.Spec.MatchConstraints.ResourceRules = stripped.Spec.MatchConstraints.ResourceRules[:len(stripped.Spec.MatchConstraints.ResourceRules)-1]
	stripped.Spec.Variables = stripped.Spec.Variables[2:]
	stripped.Spec.Validations = stripped.Spec.Validations[:len(stripped.Spec.Validations)-2]
	return stripped
}

func TestParentHookPodOriginGuardPinsJobControllerUIDChain(t *testing.T) {
	guard := NewParentWorkloadGuard(runtimePodGuardFixture())
	policy := stripParentAdmissionConvergenceDependencyProbe(t, guard.hookPodOriginPolicy())
	serialized, err := json.Marshal(policy.Spec)
	if err != nil {
		t.Fatal(err)
	}
	contract := string(serialized)
	for _, required := range []string{
		`"operations":["CREATE","UPDATE"]`,
		`"resources":["pods"]`,
		`"resources":["pods/status"]`,
		`system:kube-controller-manager`,
		`system:serviceaccount:kube-system:job-controller`,
		`object.metadata.ownerReferences.size() == 1`,
		`variables.owner.apiVersion == \"batch/v1\"`,
		`variables.owner.kind == \"Job\"`,
		`variables.owner.uid != \"\"`,
		`variables.owner.controller`,
		`variables.owner.blockOwnerDeletion`,
		`batch.kubernetes.io/job-name`,
		`batch.kubernetes.io/controller-uid`,
		`object.metadata.generateName == variables.owner.name + \"-\"`,
		`object.metadata.generateName.substring(0, 58)`,
		`object.metadata.name.substring(`,
		`matches(\"^[a-z0-9]{5}$\")`,
		`variables.isMainUpdate`,
		`batch.kubernetes.io/job-tracking`,
		`oldObject.metadata.labels`,
		`oldObject.metadata.ownerReferences`,
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("stable hook Pod origin contract does not contain %q", required)
		}
	}
	if strings.Contains(contract, `"resources":["jobs"]`) {
		t.Fatal("stable hook Pod origin policy mixes the Job schema into one CEL environment")
	}
}

func TestParentHookControllerPrincipalsRequireExactGroups(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		username string
		groups   []string
		want     bool
	}{
		{name: "controller manager", username: "system:kube-controller-manager", groups: []string{"system:authenticated"}, want: true},
		{name: "Job controller ServiceAccount", username: "system:serviceaccount:kube-system:job-controller", groups: []string{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"}, want: true},
		{name: "controller manager injected group", username: "system:kube-controller-manager", groups: []string{"system:authenticated", "system:masters"}},
		{name: "Job controller missing namespace group", username: "system:serviceaccount:kube-system:job-controller", groups: []string{"system:serviceaccounts", "system:authenticated"}},
		{name: "namespace writer", username: "developer", groups: []string{"system:authenticated"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := evaluateRolloutCEL(t, parentHookJobControllerPrincipalExpression(), map[string]any{
				"request": map[string]any{"userInfo": map[string]any{"username": test.username, "groups": test.groups}},
			}, nil)
			if got != test.want {
				t.Fatalf("Job controller principal = %v, want %v", got, test.want)
			}
		})
	}
}

func TestParentHookJobDeleteRequiresTerminalStatusAndAdmissionAuthority(t *testing.T) {
	t.Parallel()

	guard := NewParentWorkloadGuard(runtimePodGuardFixture())
	namespaceGuard := NamespaceDeletionGuardPolicyName(guard.rollout.ReleaseNamespace, guard.rollout.ReleaseName)
	expression := "(" + parentHookTerminalJobExpression("oldObject") + ") && (" + parentHookAdmissionAuthorityExpression(namespaceGuard) + ")"
	tests := []struct {
		name       string
		condition  string
		authorized bool
		want       bool
	}{
		{name: "running Job denied", condition: "Running", authorized: true},
		{name: "failed Job namespace writer denied", condition: "Failed"},
		{name: "complete Job namespace writer denied", condition: "Complete"},
		{name: "failed Job trusted Helm caller allowed", condition: "Failed", authorized: true, want: true},
		{name: "complete Job trusted Helm caller allowed", condition: "Complete", authorized: true, want: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			resolved := resolveParentHookAuthorizerChecks(expression, namespaceGuard, test.authorized)
			got := evaluateRolloutCEL(t, resolved, map[string]any{
				"oldObject": map[string]any{"status": map[string]any{"conditions": []any{map[string]any{"type": test.condition, "status": "True"}}}},
			}, nil)
			if got != test.want {
				t.Fatalf("Job DELETE admission = %v, want %v", got, test.want)
			}
		})
	}

	for _, policy := range []*admissionregistrationv1.ValidatingAdmissionPolicy{guard.hookJobOriginPolicy(), guard.hookJobContractPolicy()} {
		native := stripParentAdmissionConvergenceDependencyProbe(t, policy)
		encoded, err := json.Marshal(native.Spec)
		if err != nil {
			t.Fatal(err)
		}
		contract := string(encoded)
		for _, required := range []string{`"operations":["CREATE","UPDATE","DELETE"]`, `"resources":["jobs/status"]`, `condition.type in [\"Complete\", \"Failed\"]`, `validatingadmissionpolicybindings`} {
			if !strings.Contains(contract, required) {
				t.Fatalf("Job guard %s lacks DELETE/status fragment %q", policy.Name, required)
			}
		}
	}
}

func resolveParentHookAuthorizerChecks(expression, namespaceGuard string, allowed bool) string {
	replacements := []string{
		`authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicies").check("create").allowed()`,
		`authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicybindings").check("create").allowed()`,
		fmt.Sprintf(`authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicies").name(%q).check("delete").allowed()`, namespaceGuard),
		fmt.Sprintf(`authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicybindings").name(%q).check("delete").allowed()`, namespaceGuard),
	}
	for _, check := range replacements {
		expression = strings.ReplaceAll(expression, check, strconv.FormatBool(allowed))
	}
	return expression
}

func TestParentHookPodStatusWritersAreExact(t *testing.T) {
	t.Parallel()

	expression := fmt.Sprintf(`((%s) || ((%s) && (%s))) && (%s)`, parentHookNodePrincipalExpression(), parentHookSchedulerPrincipalExpression(), parentHookSchedulerStatusDeltaExpression(), parentHookStatusPreservesIdentityExpression())
	tests := []struct {
		name      string
		username  string
		groups    []string
		nodeName  string
		oldStatus map[string]any
		newStatus map[string]any
		want      bool
	}{
		{name: "bound node reports success", username: "system:node:worker-1", groups: []string{"system:nodes", "system:authenticated"}, nodeName: "worker-1", oldStatus: map[string]any{"phase": "Running"}, newStatus: map[string]any{"phase": "Succeeded"}, want: true},
		{name: "different node denied", username: "system:node:worker-2", groups: []string{"system:nodes", "system:authenticated"}, nodeName: "worker-1", oldStatus: map[string]any{"phase": "Running"}, newStatus: map[string]any{"phase": "Succeeded"}},
		{name: "scheduler nominates", username: "system:kube-scheduler", groups: []string{"system:authenticated"}, oldStatus: map[string]any{"phase": "Pending"}, newStatus: map[string]any{"phase": "Pending", "nominatedNodeName": "worker-1"}, want: true},
		{name: "scheduler ServiceAccount nominates", username: "system:serviceaccount:kube-system:kube-scheduler", groups: []string{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"}, oldStatus: map[string]any{"phase": "Pending"}, newStatus: map[string]any{"phase": "Pending", "nominatedNodeName": "worker-1"}, want: true},
		{name: "scheduler cannot forge success", username: "system:kube-scheduler", groups: []string{"system:authenticated"}, oldStatus: map[string]any{"phase": "Pending"}, newStatus: map[string]any{"phase": "Succeeded"}},
		{name: "scheduler injected group denied", username: "system:kube-scheduler", groups: []string{"system:authenticated", "system:masters"}, oldStatus: map[string]any{"phase": "Pending"}, newStatus: map[string]any{"phase": "Pending", "nominatedNodeName": "worker-1"}},
		{name: "namespace writer denied", username: "developer", groups: []string{"system:authenticated"}, oldStatus: map[string]any{"phase": "Pending"}, newStatus: map[string]any{"phase": "Succeeded"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			oldObject := parentHookPodStatusObject(test.nodeName, test.oldStatus)
			object := parentGuardCELClone(t, oldObject)
			object["status"] = test.newStatus
			got := evaluateRolloutCEL(t, expression, map[string]any{
				"request":   map[string]any{"userInfo": map[string]any{"username": test.username, "groups": test.groups}},
				"object":    object,
				"oldObject": oldObject,
			}, nil)
			if got != test.want {
				t.Fatalf("Pod status admission = %v, want %v", got, test.want)
			}
		})
	}
}

func TestParentHookPodStatusFieldInventoryIsComplete(t *testing.T) {
	t.Parallel()

	typeOfStatus := reflect.TypeOf(corev1.PodStatus{})
	got := make([]string, 0, typeOfStatus.NumField())
	for index := range typeOfStatus.NumField() {
		name := strings.Split(typeOfStatus.Field(index).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			got = append(got, name)
		}
	}
	if want := parentHookPodStatusFields(); !slices.Equal(got, want) {
		t.Fatalf("PodStatus field inventory changed\n got: %v\nwant: %v", got, want)
	}
}

func parentHookPodStatusObject(nodeName string, status map[string]any) map[string]any {
	spec := map[string]any{"containers": []any{map[string]any{"name": "image-check"}}}
	if nodeName != "" {
		spec["nodeName"] = nodeName
	}
	return map[string]any{
		"metadata": map[string]any{
			"name":            "image-check-abc12",
			"namespace":       "ptah-system",
			"uid":             "pod-uid",
			"resourceVersion": "7",
			"generation":      int64(1),
			"labels":          map[string]any{"app.kubernetes.io/instance": "ptah", "app.kubernetes.io/component": "crd-manager-image-check"},
			"ownerReferences": []any{map[string]any{"apiVersion": "batch/v1", "kind": "Job", "name": "image-check", "uid": "job-uid", "controller": true, "blockOwnerDeletion": true}},
		},
		"spec":   spec,
		"status": status,
	}
}

func TestParentHookPodMainUpdateCannotEraseProtectionBoundary(t *testing.T) {
	t.Parallel()

	guard := NewParentWorkloadGuard(runtimePodGuardFixture())
	native := stripParentAdmissionConvergenceDependencyProbe(t, guard.hookPodOriginPolicy())
	oldObject := parentHookImageCheckPod(guard)
	object := parentGuardCELClone(t, oldObject)
	objectMetadata := object["metadata"].(map[string]any)
	objectMetadata["finalizers"] = []any{"example.test/preserve"}
	objectMetadata["labels"].(map[string]any)["app.kubernetes.io/component"] = "unprotected"
	request := map[string]any{
		"namespace": guard.rollout.ReleaseNamespace,
		"operation": "UPDATE",
		"userInfo": map[string]any{
			"username": "system:serviceaccount:kube-system:job-controller",
			"groups":   []string{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"},
		},
	}
	match := evaluateRolloutCEL(t, native.Spec.MatchConditions[0].Expression, map[string]any{
		"request": request, "object": object, "oldObject": oldObject,
	}, nil)
	if match != true {
		t.Fatal("main Pod UPDATE escaped the v2 match after changing its protected label")
	}

	variables := map[string]any{"isCreate": false, "isMainUpdate": true, "isStatusUpdate": false, "owner": objectMetadata["ownerReferences"].([]any)[0]}
	for _, validation := range native.Spec.Validations {
		if !strings.Contains(validation.Expression, "variables.isMainUpdate") || !strings.Contains(validation.Expression, "job-tracking") {
			continue
		}
		if got := evaluateRolloutCEL(t, validation.Expression, map[string]any{
			"request": request, "object": object, "oldObject": oldObject,
		}, variables); got != false {
			t.Fatalf("protected-label removal through main Pod UPDATE = %v, want false", got)
		}
		mutatedOld := parentGuardCELClone(t, object)
		delete(mutatedOld["metadata"].(map[string]any), "ownerReferences")
		statusRequest := map[string]any{"namespace": guard.rollout.ReleaseNamespace, "operation": "UPDATE", "subResource": "status"}
		if got := evaluateRolloutCEL(t, native.Spec.MatchConditions[0].Expression, map[string]any{
			"request": statusRequest, "object": mutatedOld, "oldObject": mutatedOld,
		}, nil); got != false {
			t.Fatalf("hypothetical second-step forged status match = %v, want false after erased labels and owner", got)
		}
		return
	}
	t.Fatal("stable Pod guard has no tracking-finalizer-only main UPDATE validation")
}

func TestLegacyParentOriginContractsRemainFrozenAndCoexistWithImageCheck(t *testing.T) {
	t.Parallel()

	guard := NewParentWorkloadGuard(runtimePodGuardFixture())
	hookPattern, teardownPattern := guard.hookServiceAccountPatterns()
	legacyJob := guard.legacyHookJobOriginPolicy()
	legacyPod := guard.legacyHookPodOriginPolicy()
	if legacyJob.Name != legacyParentHookJobOriginGuardPolicyName(guard.rollout.ReleaseNamespace, guard.rollout.ReleaseName) ||
		legacyPod.Name != legacyParentHookPodOriginGuardPolicyName(guard.rollout.ReleaseNamespace, guard.rollout.ReleaseName) {
		t.Fatal("legacy parent-origin names differ from the frozen v1 identities")
	}
	for _, entry := range guard.legacyOriginEntries() {
		if entry.policy.Annotations["helm.sh/resource-policy"] != "keep" || entry.binding.Annotations["helm.sh/resource-policy"] != "keep" ||
			entry.policy.Annotations["helm.sh/hook"] != "pre-install,pre-upgrade" || entry.binding.Annotations["helm.sh/hook"] != "pre-install,pre-upgrade" {
			t.Fatalf("legacy parent-origin pair %s no longer proves Helm retention across upgrade", entry.name)
		}
	}
	if len(legacyJob.Spec.MatchConstraints.ResourceRules) != 1 ||
		!reflect.DeepEqual(legacyJob.Spec.MatchConstraints.ResourceRules[0].Operations, []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update}) ||
		!reflect.DeepEqual(legacyJob.Spec.MatchConstraints.ResourceRules[0].Resources, []string{"jobs"}) ||
		len(legacyJob.Spec.Variables) != 0 || len(legacyJob.Spec.Validations) != 3 {
		t.Fatalf("legacy Job origin contract drifted: %#v", legacyJob.Spec)
	}
	wantJobMatch := fmt.Sprintf(
		`request.namespace == %q && ((has(object.spec.template.spec.serviceAccountName) && (object.spec.template.spec.serviceAccountName.matches(%q) || object.spec.template.spec.serviceAccountName.matches(%q))) || (request.operation == "UPDATE" && has(oldObject.spec.template.spec.serviceAccountName) && (oldObject.spec.template.spec.serviceAccountName.matches(%q) || oldObject.spec.template.spec.serviceAccountName.matches(%q))))`,
		guard.rollout.ReleaseNamespace, hookPattern, teardownPattern, hookPattern, teardownPattern,
	)
	if len(legacyJob.Spec.MatchConditions) != 1 || legacyJob.Spec.MatchConditions[0].Expression != wantJobMatch ||
		legacyJob.Spec.Validations[0].Expression != `!has(request.subResource) || request.subResource == ""` ||
		legacyJob.Spec.Validations[2].Expression != parentHookAdmissionAuthorityExpression(NamespaceDeletionGuardPolicyName(guard.rollout.ReleaseNamespace, guard.rollout.ReleaseName)) {
		t.Fatal("legacy Job origin CEL differs from the frozen v1 contract")
	}
	if len(legacyPod.Spec.MatchConstraints.ResourceRules) != 1 ||
		!reflect.DeepEqual(legacyPod.Spec.MatchConstraints.ResourceRules[0].Operations, []admissionregistrationv1.OperationType{admissionregistrationv1.Create}) ||
		!reflect.DeepEqual(legacyPod.Spec.MatchConstraints.ResourceRules[0].Resources, []string{"pods"}) ||
		len(legacyPod.Spec.Variables) != 1 || legacyPod.Spec.Variables[0].Name != "owner" || len(legacyPod.Spec.Validations) != 7 {
		t.Fatalf("legacy Pod origin contract drifted: %#v", legacyPod.Spec)
	}
	wantPodMatch := fmt.Sprintf(`request.namespace == %q && has(object.spec.serviceAccountName) && (object.spec.serviceAccountName.matches(%q) || object.spec.serviceAccountName.matches(%q))`, guard.rollout.ReleaseNamespace, hookPattern, teardownPattern)
	if len(legacyPod.Spec.MatchConditions) != 1 || legacyPod.Spec.MatchConditions[0].Expression != wantPodMatch ||
		legacyPod.Spec.Validations[1].Expression != `request.userInfo.username in ["system:kube-controller-manager", "system:serviceaccount:kube-system:job-controller"]` ||
		legacyPod.Spec.Validations[6].Expression != generatedPodNameValidationExpression("variables.owner.name") {
		t.Fatal("legacy Pod origin CEL differs from the frozen v1 contract")
	}

	imageCheckJob := map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"automountServiceAccountToken": false}}}}
	jobRequest := map[string]any{
		"namespace": guard.rollout.ReleaseNamespace,
		"operation": "CREATE",
		"name":      guard.hookImageCheckJobName(),
		"resource":  map[string]any{"group": "batch", "version": "v1", "resource": "jobs"},
	}
	if got := evaluateRolloutCEL(t, legacyJob.Spec.MatchConditions[0].Expression, map[string]any{"request": jobRequest, "object": imageCheckJob, "oldObject": nil}, nil); got != false {
		t.Fatalf("legacy v1 Job origin unexpectedly matches credentialless image-check: %v", got)
	}
	currentJob := stripParentAdmissionConvergenceDependencyProbe(t, guard.hookJobOriginPolicy())
	if got := evaluateRolloutCEL(t, currentJob.Spec.MatchConditions[0].Expression, map[string]any{"request": jobRequest, "object": imageCheckJob, "oldObject": nil}, nil); got != true {
		t.Fatalf("v2 Job origin does not match credentialless image-check: %v", got)
	}

	imageCheckPod := parentHookImageCheckPod(guard)
	podRequest := map[string]any{"namespace": guard.rollout.ReleaseNamespace, "operation": "CREATE"}
	if got := evaluateRolloutCEL(t, legacyPod.Spec.MatchConditions[0].Expression, map[string]any{"request": podRequest, "object": imageCheckPod, "oldObject": nil}, nil); got != false {
		t.Fatalf("legacy v1 Pod origin unexpectedly matches credentialless image-check Pod: %v", got)
	}
	currentPod := stripParentAdmissionConvergenceDependencyProbe(t, guard.hookPodOriginPolicy())
	if got := evaluateRolloutCEL(t, currentPod.Spec.MatchConditions[0].Expression, map[string]any{"request": podRequest, "object": imageCheckPod, "oldObject": nil}, nil); got != true {
		t.Fatalf("v2 Pod origin does not match credentialless image-check Pod: %v", got)
	}
}

func parentHookImageCheckPod(guard *ParentWorkloadGuard) map[string]any {
	jobName := guard.hookImageCheckJobName()
	return map[string]any{
		"metadata": map[string]any{
			"name":              jobName + "-abc12",
			"namespace":         guard.rollout.ReleaseNamespace,
			"uid":               "pod-uid",
			"resourceVersion":   "7",
			"generation":        int64(1),
			"creationTimestamp": "2026-09-05T10:00:00Z",
			"generateName":      jobName + "-",
			"labels": map[string]any{
				"app.kubernetes.io/instance":         guard.rollout.ReleaseName,
				"app.kubernetes.io/component":        "crd-manager-image-check",
				"batch.kubernetes.io/job-name":       jobName,
				"batch.kubernetes.io/controller-uid": "job-uid",
			},
			"ownerReferences": []any{map[string]any{"apiVersion": "batch/v1", "kind": "Job", "name": jobName, "uid": "job-uid", "controller": true, "blockOwnerDeletion": true}},
			"finalizers":      []any{"batch.kubernetes.io/job-tracking", "example.test/preserve"},
		},
		"spec":   map[string]any{"containers": []any{map[string]any{"name": "image-check"}}},
		"status": map[string]any{"phase": "Pending"},
	}
}

func TestGeneratedPodNameValidationMatchesAPIServerTruncation(t *testing.T) {
	t.Parallel()

	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("ownerName", celgo.StringType),
		ext.Strings(),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := environment.Compile(generatedPodNameValidationExpression("ownerName"))
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile generated Pod name CEL: %v", issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatalf("build generated Pod name CEL program: %v", err)
	}

	ownerAtLimit := strings.Repeat("a", 57)
	longOwner := strings.Repeat("b", 60)
	tests := []struct {
		name         string
		ownerName    string
		generateName string
		podName      string
		want         bool
	}{
		{name: "short name", ownerName: "hook-job", generateName: "hook-job-", podName: "hook-job-abc12", want: true},
		{name: "58-character base", ownerName: ownerAtLimit, generateName: ownerAtLimit + "-", podName: ownerAtLimit + "-abc12", want: true},
		{name: "truncated base", ownerName: longOwner, generateName: longOwner + "-", podName: strings.Repeat("b", 58) + "abc12", want: true},
		{name: "untruncated long base", ownerName: longOwner, generateName: longOwner + "-", podName: longOwner + "-abc12"},
		{name: "wrong truncated prefix", ownerName: longOwner, generateName: longOwner + "-", podName: strings.Repeat("b", 57) + "xabc12"},
		{name: "short suffix", ownerName: "hook-job", generateName: "hook-job-", podName: "hook-job-abc1"},
		{name: "invalid suffix alphabet", ownerName: "hook-job", generateName: "hook-job-", podName: "hook-job-ab-c1"},
		{name: "foreign generateName", ownerName: "hook-job", generateName: "foreign-", podName: "foreign-abc12"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, _, err := program.Eval(map[string]any{
				"ownerName": test.ownerName,
				"object": map[string]any{"metadata": map[string]any{
					"generateName": test.generateName,
					"name":         test.podName,
				}},
			})
			if err != nil {
				t.Fatalf("evaluate generated Pod name CEL: %v", err)
			}
			got, ok := result.Value().(bool)
			if !ok {
				t.Fatalf("generated Pod name CEL result = %T(%v), want bool", result.Value(), result.Value())
			}
			if got != test.want {
				t.Fatalf("generated Pod name CEL = %t, want %t", got, test.want)
			}
		})
	}
}

func TestParentWorkloadGuardWeightsPrecedeHookServiceAccount(t *testing.T) {
	for name, weight := range map[string]string{
		"ReplicaSet policy":     parentReplicaSetPolicyWeight,
		"ReplicaSet binding":    parentReplicaSetBindingWeight,
		"hook Job policy":       parentHookOriginPolicyWeight,
		"hook Job binding":      parentHookOriginBindingWeight,
		"hook Pod policy":       parentHookPodOriginPolicyWeight,
		"hook Pod binding":      parentHookPodOriginBindingWeight,
		"candidate Job policy":  parentHookContractPolicyWeight,
		"candidate Job binding": parentHookContractBindingWeight,
	} {
		parsed, err := strconv.Atoi(weight)
		if err != nil {
			t.Fatalf("%s weight %q is invalid: %v", name, weight, err)
		}
		if parsed >= -110 {
			t.Fatalf("%s weight %d does not precede hook ServiceAccount weight -110", name, parsed)
		}
	}
}

func TestParentReplicaSetGuardPinsControllerOriginAndHashChain(t *testing.T) {
	policy := NewParentWorkloadGuard(runtimePodGuardFixture()).replicaSetPolicy()
	serialized, err := json.Marshal(policy.Spec)
	if err != nil {
		t.Fatal(err)
	}
	contract := string(serialized)
	for _, required := range []string{
		`system:kube-controller-manager`,
		`system:serviceaccount:kube-system:deployment-controller`,
		`object.metadata.ownerReferences.size() == 1`,
		`variables.owner.kind == \"Deployment\"`,
		`variables.owner.uid != \"\"`,
		`variables.owner.blockOwnerDeletion`,
		`pod-template-hash`,
		`matches(\"^[a-z0-9]{1,10}$\")`,
		`object.spec.selector.matchLabels.size() == 4`,
		`object.metadata.name == variables.expectedDeployment + \"-\" + variables.hash`,
		`system:serviceaccount:kube-system:generic-garbage-collector`,
		`variables.isGarbageCollectorCleanup`,
		`object.spec == oldObject.spec`,
		`object.status == oldObject.status`,
		`oldObject.metadata.finalizers.filter(finalizer, finalizer == \"foregroundDeletion\").size() == 1`,
		`object.metadata.finalizers == oldObject.metadata.finalizers.filter(finalizer, finalizer != \"foregroundDeletion\")`,
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("ReplicaSet parent contract does not contain %q", required)
		}
	}
	if strings.Contains(contract, `resources\":[\"jobs\"`) {
		t.Fatal("ReplicaSet policy mixes the Job schema into one CEL environment")
	}
}

func TestReplicaSetGarbageCollectorCleanupIsFinalizerRemovalOnly(t *testing.T) {
	t.Parallel()

	expression := replicaSetGarbageCollectorCleanupExpression()
	for _, apiManagedField := range []string{"resourceVersion", "managedFields"} {
		if strings.Contains(expression, apiManagedField) {
			t.Fatalf("garbage-collector cleanup pins API-managed field %q", apiManagedField)
		}
	}
	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("oldObject", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := environment.Compile(expression)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile ReplicaSet garbage-collector cleanup CEL: %v", issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatalf("build ReplicaSet garbage-collector cleanup CEL program: %v", err)
	}

	base := map[string]any{
		"metadata": map[string]any{
			"name":                       "ptah-controller-abc12",
			"namespace":                  "ptah-system",
			"uid":                        "replicaset-uid",
			"generateName":               "",
			"creationTimestamp":          "2026-09-04T12:00:00Z",
			"generation":                 int64(3),
			"deletionTimestamp":          "2026-09-04T13:00:00Z",
			"deletionGracePeriodSeconds": int64(0),
			"resourceVersion":            "17",
			"managedFields":              []any{map[string]any{"manager": "deployment-controller"}},
			"labels":                     map[string]any{"pod-template-hash": "abc12"},
			"annotations":                map[string]any{"deployment.kubernetes.io/revision": "3"},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"name":       "ptah-controller",
				"uid":        "deployment-uid",
			}},
			"finalizers": []any{"foregroundDeletion", "operator.ptah.dev/protect"},
		},
		"spec":   map[string]any{"replicas": int64(0), "selector": map[string]any{"matchLabels": map[string]any{"pod-template-hash": "abc12"}}},
		"status": map[string]any{"replicas": int64(0)},
	}
	tests := []struct {
		name   string
		mutate func(object, oldObject, request map[string]any)
		want   bool
	}{
		{name: "remove one finalizer", want: true},
		{name: "remove all finalizers including a foreign finalizer", mutate: func(object, _, _ map[string]any) {
			delete(object["metadata"].(map[string]any), "finalizers")
		}},
		{name: "remove sole foreground finalizer", mutate: func(object, oldObject, _ map[string]any) {
			oldObject["metadata"].(map[string]any)["finalizers"] = []any{"foregroundDeletion"}
			delete(object["metadata"].(map[string]any), "finalizers")
		}, want: true},
		{name: "remove foreign finalizer instead", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["finalizers"] = []any{"foregroundDeletion"}
		}},
		{name: "reorder preserved finalizers", mutate: func(object, oldObject, _ map[string]any) {
			oldObject["metadata"].(map[string]any)["finalizers"] = []any{"before.example/finalizer", "foregroundDeletion", "after.example/finalizer"}
			object["metadata"].(map[string]any)["finalizers"] = []any{"after.example/finalizer", "before.example/finalizer"}
		}},
		{name: "same finalizers", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["finalizers"] = []any{"foregroundDeletion", "operator.ptah.dev/protect"}
		}},
		{name: "add finalizer", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["finalizers"] = []any{"foregroundDeletion", "operator.ptah.dev/protect", "foreign.example/finalizer"}
		}},
		{name: "replace finalizer", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["finalizers"] = []any{"foreign.example/finalizer"}
		}},
		{name: "change spec", mutate: func(object, _, _ map[string]any) {
			object["spec"].(map[string]any)["replicas"] = int64(1)
		}},
		{name: "change status", mutate: func(object, _, _ map[string]any) {
			object["status"].(map[string]any)["replicas"] = int64(1)
		}},
		{name: "change labels", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["labels"] = map[string]any{"pod-template-hash": "other"}
		}},
		{name: "change annotations", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["annotations"] = map[string]any{"deployment.kubernetes.io/revision": "4"}
		}},
		{name: "change owner references", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"uid": "foreign"}}
		}},
		{name: "change generated-name identity", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["generateName"] = "foreign-"
		}},
		{name: "change creation identity", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["creationTimestamp"] = "2026-09-04T12:01:00Z"
		}},
		{name: "change generation", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["generation"] = int64(4)
		}},
		{name: "change UID", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["uid"] = "foreign"
		}},
		{name: "change deletion timestamp", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-09-04T13:01:00Z"
		}},
		{name: "change deletion grace period", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["deletionGracePeriodSeconds"] = int64(1)
		}},
		{name: "not deleting", mutate: func(object, oldObject, _ map[string]any) {
			delete(object["metadata"].(map[string]any), "deletionTimestamp")
			delete(oldObject["metadata"].(map[string]any), "deletionTimestamp")
		}},
		{name: "wrong principal", mutate: func(_, _, request map[string]any) {
			request["userInfo"].(map[string]any)["username"] = "system:kube-controller-manager"
		}},
		{name: "create operation", mutate: func(_, _, request map[string]any) {
			request["operation"] = "CREATE"
		}},
		{name: "API-managed resource version", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["resourceVersion"] = "18"
		}, want: true},
		{name: "API-managed field ownership", mutate: func(object, _, _ map[string]any) {
			object["metadata"].(map[string]any)["managedFields"] = []any{map[string]any{"manager": "generic-garbage-collector"}}
		}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			oldObject := parentGuardCELClone(t, base)
			object := parentGuardCELClone(t, oldObject)
			object["metadata"].(map[string]any)["finalizers"] = []any{"operator.ptah.dev/protect"}
			request := map[string]any{
				"operation": "UPDATE",
				"userInfo":  map[string]any{"username": "system:serviceaccount:kube-system:generic-garbage-collector"},
			}
			if test.mutate != nil {
				test.mutate(object, oldObject, request)
			}
			result, _, err := program.Eval(map[string]any{"object": object, "oldObject": oldObject, "request": request})
			if err != nil {
				t.Fatalf("evaluate ReplicaSet garbage-collector cleanup CEL: %v", err)
			}
			got, ok := result.Value().(bool)
			if !ok {
				t.Fatalf("ReplicaSet garbage-collector cleanup CEL result = %T(%v), want bool", result.Value(), result.Value())
			}
			if got != test.want {
				t.Fatalf("ReplicaSet garbage-collector cleanup CEL = %t, want %t", got, test.want)
			}
		})
	}
}

func parentGuardCELClone(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func TestParentHookJobGuardsCloseFutureGapAndPinExecutable(t *testing.T) {
	guard := NewParentWorkloadGuard(runtimePodGuardFixture())
	originJSON, err := json.Marshal(guard.hookJobOriginPolicy().Spec)
	if err != nil {
		t.Fatal(err)
	}
	origin := string(originJSON)
	for _, required := range []string{
		`[1-9][0-9]*-[0-9a-f]{12}$`,
		`validatingadmissionpolicies`,
		`validatingadmissionpolicybindings`,
		`check(\"create\").allowed()`,
	} {
		if !strings.Contains(origin, required) {
			t.Fatalf("stable hook parent origin contract does not contain %q", required)
		}
	}

	contractJSON, err := json.Marshal(guard.hookJobContractPolicy().Spec)
	if err != nil {
		t.Fatal(err)
	}
	contract := string(contractJSON)
	for _, required := range []string{
		guard.rollout.ManagerImage,
		guard.rollout.HookServiceAccountName,
		HookIdentityProbeJobName(guard.rollout.ReleaseNamespace, guard.rollout.ReleaseName, guard.rollout.ReleaseSequence, guard.rollout.ManagerImage),
		guard.rollout.hookJobName("preflight"),
		guard.rollout.hookJobName("reconcile"),
		`object.metadata.annotations.size() == 3`,
		`object.spec.backoffLimit == 0`,
		`object.spec.template.spec.containers.size() == 1`,
		`quantity(\"5m\")`,
		`object.spec.template.spec.volumes.size() == 1`,
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("candidate hook parent contract does not contain %q", required)
		}
	}
	if strings.Contains(contract, `resources\":[\"replicasets\"`) {
		t.Fatal("hook Job policy mixes the ReplicaSet schema into one CEL environment")
	}
}

func TestParentHookIdentityProbeDeadlineLeavesTerminationMargin(t *testing.T) {
	t.Parallel()

	rollout := runtimePodGuardFixture()
	policy := stripParentAdmissionConvergenceDependencyProbe(t, NewParentWorkloadGuard(rollout).hookJobContractPolicy())
	deadlineExpression := ""
	for _, validation := range policy.Spec.Validations {
		if strings.Contains(validation.Expression, "object.spec.activeDeadlineSeconds") && !strings.Contains(validation.Expression, "oldObject.spec.activeDeadlineSeconds") {
			deadlineExpression = validation.Expression
			break
		}
	}
	if deadlineExpression == "" {
		t.Fatal("candidate hook Job contract has no active deadline validation")
	}

	tests := []struct {
		name       string
		deadline   int64
		variables  map[string]any
		wantAccept bool
	}{
		{
			name:     "identity receives full budget",
			deadline: 210,
			variables: map[string]any{"isMainWrite": true, "isImageCheck": false, "isIdentity": true,
				"isPreflight": false, "isQuiesce": false, "isTeardown": false},
			wantAccept: true,
		},
		{
			name:     "identity rejects obsolete deadline",
			deadline: 120,
			variables: map[string]any{"isMainWrite": true, "isImageCheck": false, "isIdentity": true,
				"isPreflight": false, "isQuiesce": false, "isTeardown": false},
		},
		{
			name:     "image check keeps short deadline",
			deadline: 120,
			variables: map[string]any{"isMainWrite": true, "isImageCheck": true, "isIdentity": false,
				"isPreflight": false, "isQuiesce": false, "isTeardown": false},
			wantAccept: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := evaluateRolloutCEL(t, deadlineExpression, map[string]any{
				"object": map[string]any{"spec": map[string]any{"activeDeadlineSeconds": test.deadline}},
			}, test.variables)
			if got != test.wantAccept {
				t.Fatalf("active deadline contract = %v, want %t", got, test.wantAccept)
			}
		})
	}

	const identityDeadline = 210 * time.Second
	var managerTimeout time.Duration
	for _, argument := range rollout.hookArgs("identity-probe") {
		value, found := strings.CutPrefix(argument, "--timeout=")
		if !found {
			continue
		}
		parsed, err := time.ParseDuration(value)
		if err != nil {
			t.Fatalf("parse identity-probe timeout %q: %v", argument, err)
		}
		managerTimeout = parsed
		break
	}
	if managerTimeout == 0 {
		t.Fatal("identity-probe args have no positive manager timeout")
	}
	if margin := identityDeadline - managerTimeout; margin != 30*time.Second {
		t.Fatalf("identity-probe scheduling and termination margin = %s, want 30s", margin)
	}
}

func TestRenderedHookIdentityProbeDeadlineLeavesTerminationMargin(t *testing.T) {
	t.Parallel()

	objects := renderControllerRBACCutoverChart(t)
	var identityJob *unstructured.Unstructured
	var identityArgs []string
	for _, object := range objects {
		if object.GetKind() != "Job" {
			continue
		}
		containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
		if err != nil || !found || len(containers) != 1 {
			continue
		}
		args := transitionRenderStringSlice(containers[0].(map[string]any)["args"])
		if !slices.Contains(args, "identity-probe") {
			continue
		}
		if identityJob != nil {
			t.Fatal("rendered chart contains more than one identity-probe Job")
		}
		identityJob = object
		identityArgs = args
	}
	if identityJob == nil {
		t.Fatal("rendered chart has no identity-probe Job")
	}

	deadlineSeconds, found, err := unstructured.NestedInt64(identityJob.Object, "spec", "activeDeadlineSeconds")
	if err != nil || !found {
		t.Fatalf("rendered identity-probe active deadline is missing: found=%t, error=%v", found, err)
	}
	if !slices.Contains(identityArgs, "--timeout=180s") {
		t.Fatal("rendered identity-probe manager timeout differs from 180s")
	}
	const managerTimeout = 180 * time.Second
	deadline := time.Duration(deadlineSeconds) * time.Second
	if margin := deadline - managerTimeout; margin != 30*time.Second {
		t.Fatalf("rendered identity-probe scheduling and termination margin = %s, want 30s", margin)
	}
}

// This is intentionally a white-box test because the generated CEL expression
// is the immutable admission boundary for candidate hook Job templates.
func TestParentHookJobPriorityClassContract(t *testing.T) {
	rollout := runtimePodGuardFixture()
	rollout.PriorityClassName = "runtime-critical"
	guard := NewParentWorkloadGuard(rollout)
	reconcileJob := rollout.hookJobName("reconcile")
	policy := stripParentAdmissionConvergenceDependencyProbe(t, guard.hookJobContractPolicy())

	var expression string
	for _, validation := range policy.Spec.Validations {
		if strings.Contains(validation.Expression, ".priorityClassName") && strings.Contains(validation.Expression, "variables.isMainWrite") && !strings.Contains(validation.Expression, "oldObject.") {
			if expression != "" {
				t.Fatal("candidate hook Job contract has more than one PriorityClass validation")
			}
			expression = validation.Expression
		}
	}
	if expression == "" {
		t.Fatal("candidate hook Job contract has no PriorityClass validation")
	}
	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("variables", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := environment.Compile(expression)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile hook Job PriorityClass CEL: %v", issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatalf("build hook Job PriorityClass CEL: %v", err)
	}

	tests := []struct {
		name             string
		jobName          string
		priorityClass    string
		priority         bool
		preemptionPolicy bool
		want             bool
	}{
		{name: "exact reconcile class", jobName: reconcileJob, priorityClass: "runtime-critical", want: true},
		{name: "wrong reconcile class", jobName: reconcileJob, priorityClass: "other"},
		{name: "omitted reconcile class", jobName: reconcileJob},
		{name: "identity remains classless", jobName: HookIdentityProbeJobName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage), want: true},
		{name: "preflight remains classless", jobName: rollout.hookJobName("preflight"), want: true},
		{name: "quiesce remains classless", jobName: rollout.hookJobName("teardown-quiesce"), want: true},
		{name: "teardown remains classless", jobName: rollout.hookJobName("teardown"), want: true},
		{name: "bootstrap class is rejected", jobName: rollout.hookJobName("preflight"), priorityClass: "runtime-critical"},
		{name: "Pod priority output is rejected", jobName: reconcileJob, priorityClass: "runtime-critical", priority: true},
		{name: "Pod preemption output is rejected", jobName: reconcileJob, priorityClass: "runtime-critical", preemptionPolicy: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pod := map[string]any{}
			if test.priorityClass != "" {
				pod["priorityClassName"] = test.priorityClass
			}
			if test.priority {
				pod["priority"] = int64(1000)
			}
			if test.preemptionPolicy {
				pod["preemptionPolicy"] = "PreemptLowerPriority"
			}
			result, _, err := program.Eval(map[string]any{
				"object":    map[string]any{"spec": map[string]any{"template": map[string]any{"spec": pod}}},
				"request":   map[string]any{"name": test.jobName},
				"variables": map[string]any{"isMainWrite": true, "effectiveName": test.jobName},
			})
			if err != nil {
				t.Fatalf("evaluate hook Job PriorityClass CEL: %v", err)
			}
			got, ok := result.Value().(bool)
			if !ok {
				t.Fatalf("hook Job PriorityClass CEL result = %T(%v), want bool", result.Value(), result.Value())
			}
			if got != test.want {
				t.Fatalf("hook Job PriorityClass CEL = %t, want %t", got, test.want)
			}
		})
	}
}

func TestParentWorkloadGuardRejectsPaddedPriorityClass(t *testing.T) {
	rollout := runtimePodGuardFixture()
	rollout.PriorityClassName = " runtime-critical"
	if err := NewParentWorkloadGuard(rollout).validate(); err == nil || !strings.Contains(err.Error(), "priority class name must not contain surrounding whitespace") {
		t.Fatalf("parent workload guard validation error = %v, want padded PriorityClass refusal", err)
	}
}

// This is intentionally a white-box test because these generated CEL
// expressions are an internal admission boundary rather than an exported API.
func TestHookAdmissionContractsCloseContainerStatusAndRestartSideChannels(t *testing.T) {
	t.Parallel()

	rollout := runtimePodGuardFixture()
	tests := []struct {
		name      string
		policy    *admissionregistrationv1.ValidatingAdmissionPolicy
		container string
	}{
		{
			name:      "hook Job template",
			policy:    NewParentWorkloadGuard(rollout).hookJobContractPolicy(),
			container: "object.spec.template.spec.containers[0]",
		},
		{
			name:      "hook Pod",
			policy:    rollout.hookIdentityPolicy(),
			container: "object.spec.containers[0]",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			expressions := make([]string, 0, len(test.policy.Spec.Validations))
			for _, validation := range test.policy.Spec.Validations {
				expressions = append(expressions, validation.Expression)
			}
			contract := strings.Join(expressions, "\n")
			for _, required := range []string{
				fmt.Sprintf(`(!has(%[1]s.stdinOnce) || !%[1]s.stdinOnce)`, test.container),
				fmt.Sprintf(`(!has(%[1]s.workingDir) || %[1]s.workingDir == "") && %[1]s.terminationMessagePath == "/dev/termination-log" && %[1]s.terminationMessagePolicy == "File" && !has(%[1]s.restartPolicy) && (!has(%[1]s.restartPolicyRules) || %[1]s.restartPolicyRules.size() == 0)`, test.container),
			} {
				if !strings.Contains(contract, required) {
					t.Fatalf("hook admission contract does not contain %q", required)
				}
			}
		})
	}
}

func TestParentWorkloadGuardsScopeOptionalServiceAccounts(t *testing.T) {
	t.Parallel()

	rollout := runtimePodGuardFixture()
	guard := NewParentWorkloadGuard(rollout)
	teardownServiceAccount, err := TeardownServiceAccountName(rollout.HookServiceAccountName, rollout.ReleaseSequence)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		policy    *admissionregistrationv1.ValidatingAdmissionPolicy
		required  []string
		candidate bool
	}{
		{
			name: "ReplicaSet", policy: guard.replicaSetPolicy(),
			required: []string{rollout.ControllerServiceAccountName, rollout.CertificateDeploymentName, "oldObject.spec.template.spec.serviceAccountName"},
		},
		{
			name: "stable hook Job", policy: guard.hookJobOriginPolicy(),
			required: []string{"object.spec.template.spec.serviceAccountName.matches", "oldObject.spec.template.spec.serviceAccountName.matches", guard.hookImageCheckJobPattern()},
		},
		{
			name: "stable hook Pod", policy: guard.hookPodOriginPolicy(),
			required: []string{"object.spec.serviceAccountName.matches", "oldObject.spec.serviceAccountName.matches", "variables.isMainUpdate", "crd-manager-image-check"},
		},
		{
			name: "candidate hook Job", policy: guard.hookJobContractPolicy(), candidate: true,
			required: []string{rollout.HookServiceAccountName, teardownServiceAccount, guard.hookImageCheckJobName(), "oldObject.spec.template.spec.serviceAccountName"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.policy.Spec.FailurePolicy == nil || *test.policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
				t.Fatal("policy is not fail-closed")
			}
			native := stripParentAdmissionConvergenceDependencyProbe(t, test.policy)
			encoded, err := json.Marshal(native.Spec)
			if err != nil {
				t.Fatal(err)
			}
			for _, required := range test.required {
				if !strings.Contains(string(encoded), required) {
					t.Fatalf("policy does not contain protected identity fragment %q", required)
				}
			}
			binding := guard.binding(test.policy.Name, test.candidate)
			if !reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) {
				t.Fatalf("binding validation actions = %#v, want Deny", binding.Spec.ValidationActions)
			}
		})
	}
}

func TestParentWorkloadGuardVerifyAndPrepare(t *testing.T) {
	rollout := runtimePodGuardFixture()
	rollout.PollEvery = time.Nanosecond
	guard := NewParentWorkloadGuard(rollout)
	policies := map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}
	bindings := map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	for _, entry := range guard.entries() {
		policies[entry.name] = readyPolicy(entry.policy)
		bindings[entry.name] = entry.binding
	}
	rollout.Policies = &rolloutPolicyClient{objects: policies}
	rollout.Bindings = &rolloutBindingClient{objects: bindings}
	if err := guard.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}

	name := ParentHookJobContractPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	mutated := policies[name].DeepCopy()
	mutated.Spec.Validations[0].Expression = "true"
	policies[name] = mutated
	if err := guard.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable contract") {
		t.Fatalf("Verify error = %v, want immutable-contract refusal", err)
	}
}

func TestRenderedParentWorkloadGuardsMatchCompiledContracts(t *testing.T) {
	path := os.Getenv("PTAH_ROLLOUT_GUARD_RENDER")
	if path == "" {
		t.Skip("PTAH_ROLLOUT_GUARD_RENDER is set by the chart contract gate")
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
			var object appsv1.Deployment
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			deployments[object.Name] = &object
		case "ValidatingAdmissionPolicy":
			var object admissionregistrationv1.ValidatingAdmissionPolicy
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			policies[object.Name] = &object
		case "ValidatingAdmissionPolicyBinding":
			var object admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			bindings[object.Name] = &object
		}
	}

	managerImage := renderedGuardManagerImage
	controller := deployments["ptah-e2e-ptah-operator"]
	certificate := deployments["ptah-e2e-ptah-operator-cert-rotator"]
	if controller == nil || certificate == nil || controller.Spec.Replicas == nil || len(controller.Spec.Template.Spec.Containers) != 1 || len(certificate.Spec.Template.Spec.Containers) != 1 {
		t.Fatal("rendered runtime Deployments are missing")
	}
	rollout := runtimePodGuardFixture()
	rollout.ReleaseName = "ptah-e2e"
	rollout.ReleaseNamespace = "ptah-e2e"
	rollout.CoordinationNamespace = "ptah-e2e"
	rollout.WebhookServiceName = "ptah-e2e-ptah-operator-webhook"
	rollout.WebhookTimeoutSeconds = 5
	rollout.WebhookSecretName = "ptah-e2e-ptah-operator-webhook-cert"
	rollout.HookServiceAccountName = "ptah-e2e-ptah-operator-crd-v1-" + hookIdentityDigest("ptah-e2e", "ptah-e2e", 1, managerImage)[:12]
	rollout.ControllerServiceAccountName = controller.Spec.Template.Spec.ServiceAccountName
	rollout.ControllerServiceAccountManaged = true
	rollout.ControllerDeploymentName = controller.Name
	rollout.ControllerReplicas = *controller.Spec.Replicas
	rollout.CertificateDeploymentName = certificate.Name
	rollout.ManagerImage = managerImage
	rollout.ControllerArgs = append([]string(nil), controller.Spec.Template.Spec.Containers[0].Args...)
	rollout.CertificateArgs = append([]string(nil), certificate.Spec.Template.Spec.Containers[0].Args...)
	rollout.RuntimeDeploymentConfigExpressions = decodeRenderedRuntimeExpressions(t, controller.Spec.Template.Spec.InitContainers[0].Args, "--runtime-deployment-config-expressions-b64=")
	rollout.RuntimePodConfigExpressions = decodeRenderedRuntimeExpressions(t, controller.Spec.Template.Spec.InitContainers[0].Args, "--runtime-pod-config-expressions-b64=")
	rollout.PriorityClassName = controller.Spec.Template.Spec.PriorityClassName
	rollout.RuntimeAdmissionContractB64 = decodedManagerStringArgument(t, controller.Spec.Template.Spec.InitContainers[0].Args, "--runtime-admission-contract-b64=")
	guard := NewParentWorkloadGuard(rollout)
	for _, entry := range guard.entries() {
		assertAdmissionPolicyCELHeadroom(t, "rendered "+entry.description+" policy", policies[entry.name])
		if err := entry.verifyPolicy(policies[entry.name]); err != nil {
			t.Fatalf("rendered %s policy: %v: %s", entry.description, err, admissionConvergencePolicyDifference(policies[entry.name].Spec, entry.policy.Spec))
		}
		if err := entry.verifyBinding(bindings[entry.name]); err != nil {
			t.Fatalf("rendered %s binding: %v", entry.description, err)
		}
	}
	for _, legacy := range guard.legacyOriginEntries() {
		if policies[legacy.name] != nil || bindings[legacy.name] != nil {
			t.Fatalf("fresh chart unexpectedly renders legacy v1 parent-origin pair %s", legacy.name)
		}
	}
	weights := map[string][2]string{
		ParentReplicaSetGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage): {parentReplicaSetPolicyWeight, parentReplicaSetBindingWeight},
		ParentHookJobOriginGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName):                                             {parentHookOriginPolicyWeight, parentHookOriginBindingWeight},
		ParentHookPodOriginGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName):                                             {parentHookPodOriginPolicyWeight, parentHookPodOriginBindingWeight},
		ParentHookJobContractPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage): {parentHookContractPolicyWeight, parentHookContractBindingWeight},
	}
	for name, expected := range weights {
		if policies[name].Annotations["helm.sh/hook-weight"] != expected[0] || bindings[name].Annotations["helm.sh/hook-weight"] != expected[1] {
			t.Fatalf("guard %s weights do not precede protected ServiceAccounts", name)
		}
	}
}
