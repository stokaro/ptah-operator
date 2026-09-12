package crdupgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRenderedServiceAccountOriginGuardMatchesCompiledContract(t *testing.T) {
	testRenderedServiceAccountOriginGuardMatchesCompiledContract(
		t,
		"PTAH_ROLLOUT_GUARD_RENDER",
		"ptah-e2e-ptah-operator",
		"ptah-e2e-ptah-operator-cert-rotator",
		"ptah-e2e-ptah-operator",
	)
}

func TestRenderedLongNameServiceAccountOriginGuardMatchesCompiledContract(t *testing.T) {
	const controllerName = "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr"
	testRenderedServiceAccountOriginGuardMatchesCompiledContract(
		t,
		"PTAH_RUNTIME_POD_GUARD_LONG_RENDER",
		controllerName,
		controllerName[:39]+"-cert-rotator",
		controllerName[:24],
	)
}

func testRenderedServiceAccountOriginGuardMatchesCompiledContract(t *testing.T, environmentVariable, controllerName, certificateName, hookBase string) {
	t.Helper()
	path := os.Getenv(environmentVariable)
	if path == "" {
		t.Skip(environmentVariable + " is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	guard := testServiceAccountOriginGuard()
	guard.ReleaseName = "ptah-e2e"
	guard.ReleaseNamespace = "ptah-e2e"
	guard.CoordinationNamespace = "ptah-e2e"
	guard.ManagerImage = renderedGuardManagerImage
	guard.ControllerDeploymentName = controllerName
	guard.CertificateDeploymentName = certificateName
	guard.AdmissionContractVersion = CurrentAdmissionContractVersion
	guard.ControllerServiceAccountName = renderedDeploymentServiceAccount(t, rendered, controllerName)
	guard.CertificateServiceAccountName = renderedDeploymentServiceAccount(t, rendered, certificateName)
	guard.HookServiceAccountName = hookBase + "-crd-v1-" + hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)[:12]
	name := ServiceAccountOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
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
		case "ValidatingAdmissionPolicy":
			var object admissionregistrationv1.ValidatingAdmissionPolicy
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if object.Name == name {
				policy = &object
			}
		case "ValidatingAdmissionPolicyBinding":
			var object admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if object.Name == name {
				binding = &object
			}
		}
	}
	if err := guard.verifyPolicy(policy); err != nil {
		t.Fatalf("rendered service account origin policy: %v", err)
	}
	if err := guard.verifyBinding(binding); err != nil {
		t.Fatalf("rendered service account origin binding: %v", err)
	}
	if policy.Annotations["helm.sh/hook-weight"] != serviceAccountOriginPolicyWeight || binding.Annotations["helm.sh/hook-weight"] != serviceAccountOriginBindingWeight {
		t.Fatal("service account origin policy and binding must precede hook identity and privileged RBAC")
	}
}

func TestServiceAccountOriginGuardCoversCallerAndTokenRequestBypasses(t *testing.T) {
	guard := testServiceAccountOriginGuard()
	policy, err := guard.policy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail ||
		policy.Spec.MatchConstraints == nil || policy.Spec.MatchConstraints.MatchPolicy == nil ||
		*policy.Spec.MatchConstraints.MatchPolicy != admissionregistrationv1.Exact {
		t.Fatal("service account origin policy is not explicitly fail-closed with exact matching")
	}
	native := stripAdmissionConvergenceDependencyProbe(t, policy)
	rules := native.Spec.MatchConstraints.ResourceRules
	if len(rules) != 1 {
		t.Fatalf("resource rules = %d, want one all-resource rule", len(rules))
	}
	rule := rules[0].RuleWithOperations
	if !reflect.DeepEqual(rule.Operations, []admissionregistrationv1.OperationType{
		admissionregistrationv1.Create,
		admissionregistrationv1.Update,
		admissionregistrationv1.Delete,
		admissionregistrationv1.Connect,
	}) || !reflect.DeepEqual(rule.APIGroups, []string{"*"}) ||
		!reflect.DeepEqual(rule.APIVersions, []string{"*"}) ||
		!reflect.DeepEqual(rule.Resources, []string{"*/*"}) ||
		rule.Scope == nil || *rule.Scope != admissionregistrationv1.AllScopes {
		t.Fatalf("all-resource origin rule differs from the required operation boundary: %#v", rule)
	}
	serialized, err := json.Marshal(native.Spec)
	if err != nil {
		t.Fatal(err)
	}
	contract := string(serialized)
	for _, required := range []string{
		`serviceaccounts`,
		`has(request.subResource)`,
		`request.subResource == \"token\"`,
		`authentication.kubernetes.io/pod-name`,
		`authentication.kubernetes.io/pod-uid`,
		`.size() == 1`,
		`[0-9a-f]{12}$`,
		`request.userInfo.username.matches(\"^system:node:.+$\")`,
		`group == \"system:nodes\"`,
		`dyn(object).spec.boundObjectRef.apiVersion == \"v1\"`,
		`dyn(object).spec.boundObjectRef.kind == \"Pod\"`,
		`dyn(object).spec.boundObjectRef.uid != \"\"`,
		`ptah-hook-identity-v[1-9][0-9]*-[0-9a-f]{12}-`,
		`ptah-quiesce-v[1-9][0-9]*-[0-9a-f]{12}-`,
		`matches(\"^[a-z0-9]{1,10}-[a-z0-9]{5}$\")`,
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("service account origin contract does not contain %q", required)
		}
	}
	quiescePodPattern := `^ptah-quiesce-v[1-9][0-9]*-[0-9a-f]{12}-`
	callerNeedle := `variables.callerPodName.matches("` + quiescePodPattern + `")`
	tokenNeedle := `dyn(object).spec.boundObjectRef.name.matches("` + quiescePodPattern + `")`
	if len(native.Spec.Validations) != 5 ||
		!strings.Contains(native.Spec.Validations[3].Expression, callerNeedle) ||
		!strings.Contains(native.Spec.Validations[4].Expression, tokenNeedle) {
		t.Fatalf("service account origin guard does not cover the quiesce Pod in both caller and bound-token branches: %#v", native.Spec.Validations)
	}
}

func TestServiceAccountOriginGuardPolicyIsReleaseDistinct(t *testing.T) {
	guard := testServiceAccountOriginGuard()
	policy, err := guard.policy()
	if err != nil {
		t.Fatal(err)
	}
	other := *guard
	other.ReleaseSequence = 2
	other.ManagerImage = "registry.example/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	other.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(other.ReleaseNamespace, other.ReleaseName, other.ReleaseSequence, other.ManagerImage)[:12]
	otherPolicy, err := other.policy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Name == otherPolicy.Name || reflect.DeepEqual(policy.Spec, otherPolicy.Spec) {
		t.Fatal("service account origin policy did not change across release identities")
	}
}

func TestServiceAccountOriginGuardPrepareUsesOnlyReadOnlyPolicyVerification(t *testing.T) {
	guard := testServiceAccountOriginGuard()
	policy, err := guard.policy()
	if err != nil {
		t.Fatal(err)
	}
	name := ServiceAccountOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	guard.Policies = &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{name: readyPolicy(policy)}}
	guard.Bindings = &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{name: guard.binding()}}
	if err := guard.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestServiceAccountOriginGuardRejectsForeignSpecAndHookIdentity(t *testing.T) {
	guard := testServiceAccountOriginGuard()
	policy, err := guard.policy()
	if err != nil {
		t.Fatal(err)
	}
	mutated := policy.DeepCopy()
	ignore := admissionregistrationv1.Ignore
	mutated.Spec.FailurePolicy = &ignore
	if err := guard.verifyPolicy(mutated); err == nil || !strings.Contains(err.Error(), "immutable contract") {
		t.Fatalf("verifyPolicy error = %v, want immutable-contract refusal", err)
	}
	guard.HookServiceAccountName = "ptah-crd-v1-000000000000"
	if _, err := guard.policy(); err == nil || !strings.Contains(err.Error(), "candidate release identity") {
		t.Fatalf("policy error = %v, want hook identity refusal", err)
	}
}

func testServiceAccountOriginGuard() *ServiceAccountOriginGuard {
	managerImage := "registry.example/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	guard := &ServiceAccountOriginGuard{
		Policies:                        &rolloutPolicyClient{},
		Bindings:                        &rolloutBindingClient{},
		ReleaseName:                     "ptah",
		ReleaseNamespace:                "ptah-system",
		CoordinationNamespace:           "ptah-system",
		ControllerServiceAccountName:    "ptah-controller",
		ControllerServiceAccountManaged: true,
		CertificateServiceAccountName:   "ptah-cert-rotator",
		ControllerDeploymentName:        "ptah-controller",
		CertificateDeploymentName:       "ptah-cert-rotator",
		ControllerStateVersion:          1,
		AdmissionContractVersion:        1,
		ReleaseSequence:                 1,
		ManagerImage:                    managerImage,
		PollEvery:                       time.Nanosecond,
	}
	guard.HookServiceAccountName = "ptah-crd-v1-" + hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)[:12]
	return guard
}

// These white-box tests evaluate the compiled admission expressions themselves.
// Fake clients do not execute CEL or the API server's bind authorization check.
func TestServiceAccountOriginBindingCutoverCEL(t *testing.T) {
	t.Parallel()
	for _, namespaces := range []struct{ release, coordination string }{
		{"ptah-system", "ptah-system"},
		{"ptah-system", "default"},
		{"ptah-system", "coordination"},
		{"default", "default"},
		{"default", "coordination"},
	} {
		for _, previous := range []int32{0, 1} {
			t.Run(fmt.Sprintf("%s/%s/%d", namespaces.release, namespaces.coordination, previous), func(t *testing.T) {
				guard := testBindingOriginGuard(previous, namespaces.release, namespaces.coordination)
				policy, err := guard.policy()
				if err != nil {
					t.Fatal(err)
				}
				for _, target := range []struct {
					kind, namespace, name string
					certificate           bool
					want                  bool
				}{
					{"ClusterRoleBinding", "", guard.ControllerDeploymentName, false, true},
					{"RoleBinding", namespaces.coordination, guard.ControllerDeploymentName, false, true},
					{"RoleBinding", namespaces.release, guard.ControllerDeploymentName + "-runtime-admission", true, previous > 0},
					{"RoleBinding", "default", controllerDiscoveryBindingName(guard.ControllerDeploymentName), true, previous > 0 && namespaces.release != "default"},
				} {
					for _, dryRun := range []bool{false, true} {
						fixture := bindingOriginCELFixture(guard, target.kind, target.namespace, target.name, target.certificate)
						fixture.request["dryRun"] = dryRun
						if got := fixture.allowed(t, policy); got != target.want {
							t.Fatalf("%s %s/%s dryRun=%t allowed=%t, want %t", target.kind, target.namespace, target.name, dryRun, got, target.want)
						}
					}
				}
			})
		}
	}
}

func TestServiceAccountOriginBindingCutoverRejectsPrivilegeChanges(t *testing.T) {
	t.Parallel()
	guard := testBindingOriginGuard(1, "ptah-system", "coordination")
	policy, err := guard.policy()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*originBindingCELFixture)
	}{
		{"extra User", func(f *originBindingCELFixture) {
			f.object["subjects"] = append(f.object["subjects"].([]any), map[string]any{"kind": "User", "apiGroup": "rbac.authorization.k8s.io", "name": "other"})
		}},
		{"extra Group", func(f *originBindingCELFixture) {
			f.object["subjects"] = append(f.object["subjects"].([]any), map[string]any{"kind": "Group", "apiGroup": "rbac.authorization.k8s.io", "name": "system:authenticated"})
		}},
		{"extra ServiceAccount", func(f *originBindingCELFixture) {
			f.object["subjects"] = append(f.object["subjects"].([]any), map[string]any{"kind": "ServiceAccount", "name": guard.HookServiceAccountName, "namespace": guard.ReleaseNamespace})
		}},
		{"self binding", func(f *originBindingCELFixture) {
			f.object["subjects"].([]any)[0].(map[string]any)["name"] = guard.HookServiceAccountName
		}},
		{"wrong old subject", func(f *originBindingCELFixture) {
			f.oldObject["subjects"].([]any)[0].(map[string]any)["name"] = "foreign"
		}},
		{"reverse cutover", func(f *originBindingCELFixture) { f.object, f.oldObject = f.oldObject, f.object }},
		{"already completed", func(f *originBindingCELFixture) { f.oldObject = k8sruntime.DeepCopyJSON(f.object) }},
		{"removed certificate", func(f *originBindingCELFixture) { f.object["subjects"] = f.object["subjects"].([]any)[:1] }},
		{"reordered certificate", func(f *originBindingCELFixture) {
			subjects := f.object["subjects"].([]any)
			subjects[0], subjects[1] = subjects[1], subjects[0]
		}},
		{"changed certificate", func(f *originBindingCELFixture) { f.object["subjects"].([]any)[1].(map[string]any)["name"] = "foreign" }},
		{"wrong subject namespace", func(f *originBindingCELFixture) {
			f.object["subjects"].([]any)[0].(map[string]any)["namespace"] = "foreign"
		}},
		{"wrong subject API group", func(f *originBindingCELFixture) {
			f.object["subjects"].([]any)[0].(map[string]any)["apiGroup"] = "rbac.authorization.k8s.io"
		}},
		{"changed roleRef", func(f *originBindingCELFixture) { f.object["roleRef"].(map[string]any)["name"] = "cluster-admin" }},
		{"foreign roleRef on both sides", func(f *originBindingCELFixture) {
			f.object["roleRef"].(map[string]any)["kind"] = "ClusterRole"
			f.oldObject["roleRef"] = k8sruntime.DeepCopyJSON(f.object["roleRef"].(map[string]any))
		}},
		{"foreign namespace", func(f *originBindingCELFixture) {
			f.request["namespace"] = "foreign"
			f.object["metadata"].(map[string]any)["namespace"] = "foreign"
			f.oldObject["metadata"].(map[string]any)["namespace"] = "foreign"
		}},
		{"foreign name", func(f *originBindingCELFixture) {
			f.request["name"] = "foreign"
			f.object["metadata"].(map[string]any)["name"] = "foreign"
			f.oldObject["metadata"].(map[string]any)["name"] = "foreign"
		}},
		{"wrong kind", func(f *originBindingCELFixture) { f.request["kind"].(map[string]any)["kind"] = "ClusterRoleBinding" }},
		{"wrong version", func(f *originBindingCELFixture) { f.request["resource"].(map[string]any)["version"] = "v1beta1" }},
		{"create", func(f *originBindingCELFixture) { f.request["operation"] = "CREATE"; f.oldObject = nil }},
		{"delete", func(f *originBindingCELFixture) { f.request["operation"] = "DELETE"; f.object = nil }},
		{"subresource", func(f *originBindingCELFixture) { f.request["subResource"] = "status" }},
		{"preflight Pod", func(f *originBindingCELFixture) {
			f.extra()[serviceAccountPodNameExtra] = []any{guard.HookServiceAccountName + "-preflight-abcde"}
		}},
		{"identity Pod", func(f *originBindingCELFixture) {
			f.extra()[serviceAccountPodNameExtra] = []any{"ptah-hook-identity-v2-aaaaaaaaaaaa-abcde"}
		}},
		{"quiesce Pod", func(f *originBindingCELFixture) {
			f.extra()[serviceAccountPodNameExtra] = []any{"ptah-quiesce-v2-aaaaaaaaaaaa-abcde"}
		}},
		{"old hook Pod", func(f *originBindingCELFixture) {
			f.extra()[serviceAccountPodNameExtra] = []any{"ptah-crd-v1-aaaaaaaaaaaa-abcde"}
		}},
		{"missing Pod UID", func(f *originBindingCELFixture) { delete(f.extra(), serviceAccountPodUIDExtra) }},
		{"empty Pod UID", func(f *originBindingCELFixture) { f.extra()[serviceAccountPodUIDExtra] = []any{""} }},
		{"multiple Pod UIDs", func(f *originBindingCELFixture) { f.extra()[serviceAccountPodUIDExtra] = []any{"first", "second"} }},
		{"multiple Pod names", func(f *originBindingCELFixture) {
			f.extra()[serviceAccountPodNameExtra] = []any{guard.HookServiceAccountName + "-abcde", guard.HookServiceAccountName + "-fghij"}
		}},
		{"active phase", func(f *originBindingCELFixture) {
			data := f.params["data"].(map[string]any)
			data[controllerCredentialsDataKey] = "active"
			delete(data, controllerCredentialsTargetDataKey)
			delete(data, controllerCredentialsAttemptDataKey)
		}},
		{"wrong active sequence", func(f *originBindingCELFixture) { f.params["data"].(map[string]any)[activeReleaseDataKey] = "0" }},
		{"wrong target", func(f *originBindingCELFixture) {
			f.params["data"].(map[string]any)[controllerCredentialsTargetDataKey] = "3"
		}},
		{"wrong attempt", func(f *originBindingCELFixture) {
			f.params["data"].(map[string]any)[controllerCredentialsAttemptDataKey] = strings.Repeat("a", 64)
		}},
		{"convergence field manager decoy", func(f *originBindingCELFixture) {
			f.object["subjects"].([]any)[0].(map[string]any)["name"] = "foreign"
			f.request["dryRun"] = true
			f.request["options"] = map[string]any{"fieldManager": newAdmissionConvergenceDependencyProbe(policy.Name, hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)).FieldManager}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := bindingOriginCELFixture(guard, "RoleBinding", guard.ReleaseNamespace, guard.ControllerDeploymentName+"-runtime-admission", true)
			test.mutate(&fixture)
			if fixture.allowed(t, policy) {
				t.Fatal("unsafe binding mutation was accepted")
			}
		})
	}
	for _, field := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "selfLink", "generateName", "labels", "annotations", "ownerReferences", "finalizers", "deletionTimestamp", "deletionGracePeriodSeconds"} {
		t.Run("metadata/"+field, func(t *testing.T) {
			fixture := bindingOriginCELFixture(guard, "RoleBinding", guard.ReleaseNamespace, guard.ControllerDeploymentName+"-runtime-admission", true)
			fixture.object["metadata"].(map[string]any)[field] = "changed"
			if fixture.allowed(t, policy) {
				t.Fatal("metadata mutation was accepted")
			}
		})
	}
}

func TestServiceAccountOriginBindingCutoverRequestNamespace(t *testing.T) {
	t.Parallel()
	for _, previous := range []int32{0, 1} {
		guard := testBindingOriginGuard(previous, "ptah-system", "coordination")
		policy, err := guard.policy()
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"ClusterRoleBinding", "RoleBinding"} {
			for _, requestNamespace := range []string{"<omitted>", "", "coordination", "foreign"} {
				t.Run(fmt.Sprintf("%d/%s/%s", previous, kind, requestNamespace), func(t *testing.T) {
					namespace := guard.CoordinationNamespace
					want := requestNamespace == namespace
					if kind == "ClusterRoleBinding" {
						namespace = ""
						want = requestNamespace == "<omitted>" || requestNamespace == ""
					}
					fixture := bindingOriginCELFixture(guard, kind, namespace, guard.ControllerDeploymentName, false)
					delete(fixture.request, "namespace")
					if requestNamespace != "<omitted>" {
						fixture.request["namespace"] = requestNamespace
					}
					if got := fixture.allowed(t, policy); got != want {
						t.Fatalf("binding scope allowed=%t, want %t", got, want)
					}
				})
			}
		}
	}
}

func TestServiceAccountOriginClusterBindingCutoverRequiresExactDrain(t *testing.T) {
	t.Parallel()
	for _, previous := range []int32{0, 1} {
		guard := testBindingOriginGuard(previous, "ptah-system", "coordination")
		policy, err := guard.policy()
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range []struct{ key, value string }{
			{controllerCredentialsAttemptDataKey, strings.Repeat("b", 64)},
			{controllerCredentialsTargetDataKey, "3"},
			{controllerCredentialsDataKey, "active"},
		} {
			for _, explicitNamespace := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/%s/explicit-namespace-%t", previous, change.key, explicitNamespace), func(t *testing.T) {
					fixture := bindingOriginCELFixture(guard, "ClusterRoleBinding", "", guard.ControllerDeploymentName, false)
					if explicitNamespace {
						fixture.request["namespace"] = ""
					}
					fixture.params["data"].(map[string]any)[change.key] = change.value
					if fixture.allowed(t, policy) {
						t.Fatal("cluster-scoped binding mutation escaped its exact credential drain")
					}
				})
			}
		}
	}
}

func TestServiceAccountOriginBindingCutoverRetainedPolicyOverlap(t *testing.T) {
	t.Parallel()
	previous := testBindingOriginGuard(0, "ptah-system", "coordination")
	candidate := testBindingOriginGuard(1, "ptah-system", "coordination")
	fixture := bindingOriginCELFixture(candidate, "ClusterRoleBinding", "", candidate.ControllerDeploymentName, false)
	for _, guard := range []*ServiceAccountOriginGuard{previous, candidate} {
		policy, err := guard.policy()
		if err != nil {
			t.Fatal(err)
		}
		if !fixture.allowed(t, policy) {
			t.Fatalf("retained sequence %d blocks the legitimate successor hook", guard.ReleaseSequence)
		}
	}
	// A new install has no cutover authority, even when another binding happens
	// to have the expected stable name and the request carries a draining tuple.
	candidate.PreviousControllerServiceAccountName = ""
	policy, err := candidate.policy()
	if err != nil {
		t.Fatal(err)
	}
	if fixture.allowed(t, policy) {
		t.Fatal("fresh install accepted a binding mutation")
	}
}

func TestServiceAccountOriginGuardCopiesAndRequiresCoordinationNamespace(t *testing.T) {
	t.Parallel()
	guard := NewServiceAccountOriginGuard(&RolloutGuard{
		ReleaseNamespace: "ptah-system", CoordinationNamespace: "coordination",
	})
	if guard.CoordinationNamespace != "coordination" {
		t.Fatal("constructor discarded the distinct coordination namespace")
	}
	guard = testServiceAccountOriginGuard()
	guard.CoordinationNamespace = ""
	if err := guard.validate(); err == nil || !strings.Contains(err.Error(), "coordination namespace") {
		t.Fatalf("validate without coordination namespace = %v, want an explicit identity error", err)
	}
}

// This exercises the production Helm template, not a second Go copy of its
// expressions. Only the cluster-read boundary supplies fixture provenance; the
// lookup discovery and provenance rules have their own chart tests. Replacing
// the release constant in this temporary chart simulates the next binary/chart
// pair without advancing the production release sequence.
func TestRenderedPredecessorServiceAccountOriginGuardMatchesCompiledContract(t *testing.T) {
	t.Parallel()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal("Helm is required for predecessor service account origin parity")
	}
	chart := newOriginParityChart(t)
	for _, namespaces := range []struct{ release, coordination string }{
		{"ptah-system", ""},
		{"ptah-system", "ptah-system"},
		{"ptah-system", "default"},
		{"ptah-system", "coordination"},
		{"default", ""},
		{"default", "coordination"},
	} {
		for _, previous := range []int32{0, 1} {
			for _, managed := range []bool{false, true} {
				for _, certificateMode := range []string{"managed", "disabled", "external-secret"} {
					name := fmt.Sprintf("%s/%s/%d-to-%d/managed-%t/%s", namespaces.release, namespaces.coordination, previous, previous+1, managed, certificateMode)
					t.Run(name, func(t *testing.T) {
						guard := originParityGuard(previous, namespaces.release, namespaces.coordination, managed, certificateMode)
						values := map[string]any{
							"image":                map[string]any{"repository": "registry.example/ptah", "digest": "sha256:" + strings.Repeat("a", 64)},
							"coordination":         map[string]any{"namespace": namespaces.coordination},
							"serviceAccount":       map[string]any{"create": managed, "name": "origin-controller"},
							"certificateRotation":  map[string]any{"enabled": certificateMode != "disabled"},
							"originParitySequence": fmt.Sprint(previous + 1),
							"originParityPredecessor": map[string]any{
								"name": guard.PreviousControllerServiceAccountName, "uid": string(guard.PreviousControllerServiceAccountUID),
								"managed": strconv.FormatBool(managed), "releaseSequence": fmt.Sprint(previous),
								"managerImage": guard.PreviousControllerManagerImage,
							},
						}
						if certificateMode == "external-secret" {
							values["webhook"] = map[string]any{"existingSecret": "external-webhook-tls"}
						}
						policy, binding := renderOriginParity(t, helm, chart, guard, values)
						if err := guard.validate(); err != nil {
							t.Fatal(err)
						}
						if err := guard.verifyPolicy(policy); err != nil {
							compiled, buildErr := guard.policy()
							if buildErr != nil {
								t.Fatal(buildErr)
							}
							t.Fatalf("rendered predecessor origin policy: %v\n%s", err, renderedSpecDifference(policy, compiled))
						}
						if err := guard.verifyBinding(binding); err != nil {
							t.Fatalf("rendered predecessor origin binding: %v", err)
						}
					})
				}
			}
		}
	}
}

func originParityGuard(previous int32, namespace, coordination string, managed bool, certificateMode string) *ServiceAccountOriginGuard {
	const release, deployment, controllerBase = "origin-parity", "origin-parity-ptah-operator", "origin-controller"
	if coordination == "" {
		coordination = namespace
	}
	managerImage := "registry.example/ptah@sha256:" + strings.Repeat("a", 64)
	previousImage := "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	controllerName := func(sequence int32, image string) string {
		if sequence == 0 {
			return controllerBase
		}
		if !managed {
			return fmt.Sprintf("%s-v%d", controllerBase, sequence)
		}
		digest := sha256.Sum256([]byte(controllerBase + "\n1\n" + hookIdentityDigest(namespace, release, sequence, image)))
		return fmt.Sprintf("%s-v%d-%s", controllerBase, sequence, fmt.Sprintf("%x", digest)[:12])
	}
	admissionVersion := int32(1)
	if certificateMode == "managed" {
		admissionVersion = CurrentAdmissionContractVersion
	}
	return NewServiceAccountOriginGuard(&RolloutGuard{
		Policies: &rolloutPolicyClient{}, Bindings: &rolloutBindingClient{},
		ReleaseName: release, ReleaseNamespace: namespace, CoordinationNamespace: coordination,
		HookServiceAccountName:       fmt.Sprintf("%s-crd-v%d-%s", deployment[:24], previous+1, hookIdentityDigest(namespace, release, previous+1, managerImage)[:12]),
		ControllerServiceAccountName: controllerName(previous+1, managerImage), ControllerServiceAccountManaged: managed,
		PreviousControllerServiceAccountName: controllerName(previous, previousImage), PreviousControllerServiceAccountManaged: managed,
		PreviousControllerServiceAccountUID: "previous-controller-uid", PreviousControllerReleaseSequence: previous, PreviousControllerManagerImage: previousImage,
		ControllerDeploymentName: deployment, CertificateDeploymentName: deployment + "-cert-rotator",
		CertificateRuntimeEnabled: certificateMode == "managed", ControllerStateVersion: 1, AdmissionContractVersion: admissionVersion,
		ReleaseSequence: previous + 1, ManagerImage: managerImage, PollEvery: time.Nanosecond,
	})
}

func newOriginParityChart(t *testing.T) string {
	t.Helper()
	source := filepath.Join("..", "..", "charts", "ptah-operator")
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	helpers := read("templates/_helpers.tpl")
	start := strings.Index(helpers, `{{- define "ptah-operator.previousControllerPrincipalJSON" -}}`)
	end := strings.Index(helpers, `{{- define "ptah-operator.previousControllerServiceAccountName" -}}`)
	if start < 0 || end <= start {
		t.Fatal("previous controller lookup wrapper boundaries changed")
	}
	helpers = helpers[:start] + `{{- define "ptah-operator.previousControllerPrincipalJSON" -}}{{- .Values.originParityPredecessor | toJson -}}{{- end -}}` + "\n" + helpers[end:]
	sequence := fmt.Sprintf(`{{- define "ptah-operator.releaseSequence" -}}%d{{- end -}}`, CurrentReleaseSequence)
	if strings.Count(helpers, sequence) != 1 {
		t.Fatal("compiled chart release sequence boundary changed")
	}
	helpers = strings.Replace(helpers, sequence, `{{- define "ptah-operator.releaseSequence" -}}{{- .Values.originParitySequence -}}{{- end -}}`, 1)
	activation := read("templates/release-activation-guard.yaml")
	end = strings.Index(activation, `{{- $guardName :=`)
	if end < 0 {
		t.Fatal("release activation shape helper boundary changed")
	}
	helpers += "\n" + activation[:end]
	runtimePod := read("templates/runtime-pod-guard.yaml")
	end = strings.Index(runtimePod, `{{- $sequence :=`)
	if end < 0 {
		t.Fatal("runtime Pod request name helper boundary changed")
	}
	helpers += "\n" + runtimePod[:end]
	chart := t.TempDir()
	if err := os.Mkdir(filepath.Join(chart, "templates"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"Chart.yaml": read("Chart.yaml"), "values.yaml": read("values.yaml"),
		"templates/_helpers.tpl":                      helpers,
		"templates/service-account-origin-guard.yaml": read("templates/service-account-origin-guard.yaml"),
	} {
		if err := os.WriteFile(filepath.Join(chart, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return chart
}

func renderOriginParity(t *testing.T, helm, chart string, guard *ServiceAccountOriginGuard, values map[string]any) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	t.Helper()
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	valuesPath := filepath.Join(directory, "values.json")
	if err := os.WriteFile(valuesPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, helm, "template", guard.ReleaseName, chart, "--namespace", guard.ReleaseNamespace, "-f", valuesPath)
	command.Env = append(os.Environ(), "HELM_CACHE_HOME="+filepath.Join(directory, "cache"), "HELM_CONFIG_HOME="+filepath.Join(directory, "config"), "HELM_DATA_HOME="+filepath.Join(directory, "data"))
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			t.Fatalf("render predecessor origin guard: %v\n%s", err, exitError.Stderr)
		}
		t.Fatalf("render predecessor origin guard: %v", err)
	}
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(output))
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var metadata metav1.TypeMeta
		if err := json.Unmarshal(raw, &metadata); err != nil {
			t.Fatal(err)
		}
		switch metadata.Kind {
		case "ValidatingAdmissionPolicy":
			if policy != nil {
				t.Fatal("origin render contains more than one policy")
			}
			if err := json.Unmarshal(raw, &policy); err != nil {
				t.Fatal(err)
			}
		case "ValidatingAdmissionPolicyBinding":
			if binding != nil {
				t.Fatal("origin render contains more than one binding")
			}
			if err := json.Unmarshal(raw, &binding); err != nil {
				t.Fatal(err)
			}
		}
	}
	if policy == nil || binding == nil {
		t.Fatal("origin render is missing its policy or binding")
	}
	return policy, binding
}

func testBindingOriginGuard(previous int32, namespace, coordination string) *ServiceAccountOriginGuard {
	guard := testServiceAccountOriginGuard()
	guard.ReleaseNamespace, guard.CoordinationNamespace = namespace, coordination
	guard.PreviousControllerReleaseSequence, guard.ReleaseSequence = previous, previous+1
	guard.PreviousControllerServiceAccountName = fmt.Sprintf("ptah-controller-v%d", previous)
	guard.ControllerServiceAccountName = fmt.Sprintf("ptah-controller-v%d", previous+1)
	guard.PreviousControllerServiceAccountUID = "previous-controller-uid"
	guard.PreviousControllerManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	guard.HookServiceAccountName = fmt.Sprintf("ptah-crd-v%d-%s", guard.ReleaseSequence, hookIdentityDigest(namespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)[:12])
	return guard
}

type originBindingCELFixture struct {
	object, oldObject, request, params map[string]any
}

func (f *originBindingCELFixture) extra() map[string]any {
	return f.request["userInfo"].(map[string]any)["extra"].(map[string]any)
}

func (f originBindingCELFixture) allowed(t *testing.T, policy *admissionregistrationv1.ValidatingAdmissionPolicy) bool {
	t.Helper()
	return !slices.Contains(evaluatePolicyValidations(t, policy, f.object, f.oldObject, f.request, f.params), false)
}

func bindingOriginCELFixture(g *ServiceAccountOriginGuard, kind, namespace, name string, certificate bool) originBindingCELFixture {
	roleKind, resource := "Role", "rolebindings"
	if kind == "ClusterRoleBinding" {
		roleKind, resource = "ClusterRole", "clusterrolebindings"
	}
	subjects := []any{map[string]any{"kind": "ServiceAccount", "name": g.PreviousControllerServiceAccountName, "namespace": g.ReleaseNamespace}}
	if certificate {
		subjects = append(subjects, map[string]any{"kind": "ServiceAccount", "name": g.CertificateServiceAccountName, "namespace": g.ReleaseNamespace})
	}
	oldObject := map[string]any{
		"metadata": map[string]any{
			"name": name, "namespace": namespace, "uid": "binding-uid", "resourceVersion": "101",
			"labels":      map[string]any{managedByLabel: "Helm", instanceLabel: g.ReleaseName},
			"annotations": map[string]any{helmReleaseNameAnnotation: g.ReleaseName, helmReleaseNamespaceAnnotation: g.ReleaseNamespace},
		},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": roleKind, "name": name},
		"subjects": subjects,
	}
	object := k8sruntime.DeepCopyJSON(oldObject)
	object["subjects"].([]any)[0].(map[string]any)["name"] = g.ControllerServiceAccountName
	params := rolloutActivationCELObject(&RolloutGuard{ReleaseName: g.ReleaseName, ReleaseNamespace: g.ReleaseNamespace}, int64(g.PreviousControllerReleaseSequence), 1, 1, max(1, int64(g.PreviousControllerReleaseSequence)), g.PreviousControllerManagerImage)
	params["data"].(map[string]any)[controllerCredentialsDataKey] = "draining"
	params["data"].(map[string]any)[controllerCredentialsTargetDataKey] = fmt.Sprint(g.ReleaseSequence)
	params["data"].(map[string]any)[controllerCredentialsAttemptDataKey] = hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage)
	fixture := originBindingCELFixture{
		object: object, oldObject: oldObject, params: params,
		request: map[string]any{
			"operation": "UPDATE", "namespace": namespace, "name": name, "dryRun": false,
			"kind":     map[string]any{"group": "rbac.authorization.k8s.io", "version": "v1", "kind": kind},
			"resource": map[string]any{"group": "rbac.authorization.k8s.io", "version": "v1", "resource": resource},
			"userInfo": map[string]any{"username": "system:serviceaccount:" + g.ReleaseNamespace + ":" + g.HookServiceAccountName, "extra": map[string]any{
				serviceAccountPodNameExtra: []any{generatedNamePrefix(g.HookServiceAccountName+"-") + "abcde"},
				serviceAccountPodUIDExtra:  []any{"reconcile-pod-uid"},
			}},
		},
	}
	if kind == "ClusterRoleBinding" {
		// The API omits these optional fields for cluster-scoped requests and
		// objects; an explicit empty string hid a native CEL field-access error.
		delete(fixture.request, "namespace")
		delete(fixture.object["metadata"].(map[string]any), "namespace")
		delete(fixture.oldObject["metadata"].(map[string]any), "namespace")
	}
	return fixture
}
