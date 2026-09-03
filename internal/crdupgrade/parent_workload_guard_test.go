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
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	}

	otherRollout := *rollout
	otherRollout.ReleaseSequence = 2
	otherRollout.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	otherRollout.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(otherRollout.ReleaseNamespace, otherRollout.ReleaseName, otherRollout.ReleaseSequence, otherRollout.ManagerImage)[:12]
	other := NewParentWorkloadGuard(&otherRollout)
	if replicaSet.Name != other.replicaSetPolicy().Name || !reflect.DeepEqual(replicaSet.Spec, other.replicaSetPolicy().Spec) {
		t.Fatal("stable ReplicaSet parent contract changed across release sequences")
	}
	if hookOrigin.Name != other.hookJobOriginPolicy().Name || !reflect.DeepEqual(hookOrigin.Spec, other.hookJobOriginPolicy().Spec) {
		t.Fatal("stable hook Job origin contract changed across release sequences")
	}
	if hookPodOrigin.Name != other.hookPodOriginPolicy().Name || !reflect.DeepEqual(hookPodOrigin.Spec, other.hookPodOriginPolicy().Spec) {
		t.Fatal("stable hook Pod origin contract changed across release sequences")
	}
	if hookContract.Name == other.hookJobContractPolicy().Name || reflect.DeepEqual(hookContract.Spec, other.hookJobContractPolicy().Spec) {
		t.Fatal("candidate hook Job contract did not change with release identity")
	}
}

func TestParentHookPodOriginGuardPinsJobControllerUIDChain(t *testing.T) {
	guard := NewParentWorkloadGuard(runtimePodGuardFixture())
	policy := guard.hookPodOriginPolicy()
	serialized, err := json.Marshal(policy.Spec)
	if err != nil {
		t.Fatal(err)
	}
	contract := string(serialized)
	for _, required := range []string{
		`"operations":["CREATE"]`,
		`"resources":["pods"]`,
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
		`object.metadata.name.size() == object.metadata.generateName.size() + 5`,
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("stable hook Pod origin contract does not contain %q", required)
		}
	}
	if strings.Contains(contract, `"operations":["UPDATE"]`) || strings.Contains(contract, `"resources":["jobs"]`) {
		t.Fatal("stable hook Pod origin policy is not isolated to Pod CREATE")
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
		`object.spec.selector.matchLabels.size() == 4`,
		`object.metadata.name == variables.expectedDeployment + \"-\" + variables.hash`,
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("ReplicaSet parent contract does not contain %q", required)
		}
	}
	if strings.Contains(contract, `resources\":[\"jobs\"`) {
		t.Fatal("ReplicaSet policy mixes the Job schema into one CEL environment")
	}
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

func TestParentWorkloadGuardsScopeOptionalServiceAccounts(t *testing.T) {
	t.Parallel()

	rollout := runtimePodGuardFixture()
	guard := NewParentWorkloadGuard(rollout)
	hookPattern, teardownPattern := guard.hookServiceAccountPatterns()
	teardownServiceAccount, err := TeardownServiceAccountName(rollout.HookServiceAccountName, rollout.ReleaseSequence)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name             string
		policy           *admissionregistrationv1.ValidatingAdmissionPolicy
		match            string
		validationPrefix string
		candidate        bool
	}{
		{
			name:   "ReplicaSet",
			policy: guard.replicaSetPolicy(),
			match: fmt.Sprintf(
				`request.namespace == %q && ((has(object.spec.template.spec.serviceAccountName) && object.spec.template.spec.serviceAccountName in [%q, %q]) || (request.operation == "UPDATE" && has(oldObject.spec.template.spec.serviceAccountName) && oldObject.spec.template.spec.serviceAccountName in [%q, %q]))`,
				rollout.ReleaseNamespace, rollout.ControllerServiceAccountName, rollout.CertificateDeploymentName, rollout.ControllerServiceAccountName, rollout.CertificateDeploymentName,
			),
			validationPrefix: `has(object.spec.template.spec.serviceAccountName) && object.spec.template.spec.serviceAccountName in`,
		},
		{
			name:   "stable hook Job",
			policy: guard.hookJobOriginPolicy(),
			match: fmt.Sprintf(
				`request.namespace == %q && ((has(object.spec.template.spec.serviceAccountName) && (object.spec.template.spec.serviceAccountName.matches(%q) || object.spec.template.spec.serviceAccountName.matches(%q))) || (request.operation == "UPDATE" && has(oldObject.spec.template.spec.serviceAccountName) && (oldObject.spec.template.spec.serviceAccountName.matches(%q) || oldObject.spec.template.spec.serviceAccountName.matches(%q))))`,
				rollout.ReleaseNamespace, hookPattern, teardownPattern, hookPattern, teardownPattern,
			),
			validationPrefix: `has(object.spec.template.spec.serviceAccountName) && (object.spec.template.spec.serviceAccountName.matches`,
		},
		{
			name:   "stable hook Pod",
			policy: guard.hookPodOriginPolicy(),
			match: fmt.Sprintf(
				`request.namespace == %q && has(object.spec.serviceAccountName) && (object.spec.serviceAccountName.matches(%q) || object.spec.serviceAccountName.matches(%q))`,
				rollout.ReleaseNamespace, hookPattern, teardownPattern,
			),
			validationPrefix: `object.metadata.namespace == request.namespace && has(object.spec.serviceAccountName) && (object.spec.serviceAccountName.matches`,
		},
		{
			name:   "candidate hook Job",
			policy: guard.hookJobContractPolicy(),
			match: fmt.Sprintf(
				`request.namespace == %q && ((has(object.spec.template.spec.serviceAccountName) && object.spec.template.spec.serviceAccountName in [%q, %q]) || (request.operation == "UPDATE" && has(oldObject.spec.template.spec.serviceAccountName) && oldObject.spec.template.spec.serviceAccountName in [%q, %q]))`,
				rollout.ReleaseNamespace, rollout.HookServiceAccountName, teardownServiceAccount, rollout.HookServiceAccountName, teardownServiceAccount,
			),
			validationPrefix: `has(object.spec.template.spec.serviceAccountName) && object.spec.template.spec.serviceAccountName ==`,
			candidate:        true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.policy.Spec.FailurePolicy == nil || *test.policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
				t.Fatal("policy is not fail-closed")
			}
			if len(test.policy.Spec.MatchConditions) != 1 || test.policy.Spec.MatchConditions[0].Expression != test.match {
				t.Fatalf("optional ServiceAccount match condition\n got: %q\nwant: %q", test.policy.Spec.MatchConditions, test.match)
			}
			foundValidation := false
			for _, validation := range test.policy.Spec.Validations {
				if strings.HasPrefix(validation.Expression, test.validationPrefix) {
					foundValidation = true
					break
				}
			}
			if !foundValidation {
				t.Fatalf("policy does not explicitly reject removal of its protected ServiceAccount with prefix %q", test.validationPrefix)
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

	managerImage := "ghcr.io/stokaro/ptah-operator@sha256:" + strings.Repeat("2", 64)
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
	rollout.ControllerServiceAccountName = "ptah-e2e-ptah-operator"
	rollout.ControllerDeploymentName = controller.Name
	rollout.ControllerReplicas = *controller.Spec.Replicas
	rollout.CertificateDeploymentName = certificate.Name
	rollout.ManagerImage = managerImage
	rollout.ControllerArgs = append([]string(nil), controller.Spec.Template.Spec.Containers[0].Args...)
	rollout.CertificateArgs = append([]string(nil), certificate.Spec.Template.Spec.Containers[0].Args...)
	rollout.RuntimeDeploymentConfigExpressions = decodeRenderedRuntimeExpressions(t, controller.Spec.Template.Spec.InitContainers[0].Args, "--runtime-deployment-config-expressions-b64=")
	rollout.RuntimePodConfigExpressions = decodeRenderedRuntimeExpressions(t, controller.Spec.Template.Spec.InitContainers[0].Args, "--runtime-pod-config-expressions-b64=")
	rollout.RuntimeAdmissionContractB64 = decodedManagerStringArgument(t, controller.Spec.Template.Spec.InitContainers[0].Args, "--runtime-admission-contract-b64=")
	guard := NewParentWorkloadGuard(rollout)
	for _, entry := range guard.entries() {
		assertAdmissionPolicyCELHeadroom(t, "rendered "+entry.description+" policy", policies[entry.name])
		if err := entry.verifyPolicy(policies[entry.name]); err != nil {
			t.Fatalf("rendered %s policy: %v", entry.description, err)
		}
		if err := entry.verifyBinding(bindings[entry.name]); err != nil {
			t.Fatalf("rendered %s binding: %v", entry.description, err)
		}
	}
	weights := map[string][2]string{
		ParentReplicaSetGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName):                                                {parentReplicaSetPolicyWeight, parentReplicaSetBindingWeight},
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
