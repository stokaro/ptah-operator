package crdupgrade

// This render test intentionally uses the package under test because it
// compares the unexported teardown inventory with the exact rendered hook
// roles. A public API for mutable security contracts would be less safe.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestControllerRBACCutoverHookRenderHasExactBoundedAuthority(t *testing.T) {
	t.Parallel()
	objects := renderControllerRBACCutoverChart(t)
	job := findControllerRBACCutoverJob(t, objects)

	deadline, found, err := unstructured.NestedInt64(job.Object, "spec", "activeDeadlineSeconds")
	if err != nil || !found || deadline != 390 {
		t.Fatalf("controller RBAC cutover Job activeDeadlineSeconds = %d, found=%t, error=%v; want 390", deadline, found, err)
	}
	containers, found, err := unstructured.NestedSlice(job.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		t.Fatalf("controller RBAC cutover Job containers = %d, found=%t, error=%v; want 1", len(containers), found, err)
	}
	args := transitionRenderStringSlice(containers[0].(map[string]any)["args"])
	if !slices.Contains(args, "--timeout=360s") {
		t.Fatalf("controller RBAC cutover Job args omit --timeout=360s")
	}

	hookServiceAccount, found, err := unstructured.NestedString(job.Object, "spec", "template", "spec", "serviceAccountName")
	if err != nil || !found || hookServiceAccount == "" {
		t.Fatalf("controller RBAC cutover Job ServiceAccount = %q, found=%t, error=%v", hookServiceAccount, found, err)
	}
	binding := findTransitionRenderObject(t, objects, "ClusterRoleBinding", job.GetName())
	subjects, found, err := unstructured.NestedSlice(binding.Object, "subjects")
	if err != nil || !found || len(subjects) != 1 || subjects[0].(map[string]any)["name"] != hookServiceAccount {
		t.Fatalf("controller RBAC cutover ClusterRoleBinding subjects differ from the exact hook identity")
	}
	roleName, found, err := unstructured.NestedString(binding.Object, "roleRef", "name")
	if err != nil || !found || roleName == "" {
		t.Fatalf("controller RBAC cutover ClusterRoleBinding roleRef = %q, found=%t, error=%v", roleName, found, err)
	}
	role := findTransitionRenderObject(t, objects, "ClusterRole", roleName)
	controllerName := "rbac-cutover-ptah-operator"

	assertTransitionRenderRule(t, role, "rbac.authorization.k8s.io", "clusterrolebindings", nil, []string{"list"})
	assertTransitionRenderRule(t, role, "rbac.authorization.k8s.io", "clusterrolebindings", []string{controllerName}, []string{"get", "patch"})
	assertTransitionRenderRule(t, role, "rbac.authorization.k8s.io", "rolebindings", nil, []string{"list"})
	assertTransitionRenderRule(t, role, "rbac.authorization.k8s.io", "clusterroles", []string{controllerName}, []string{"get"})
	assertTransitionRenderNoResourceVerb(t, role, "rbac.authorization.k8s.io", "rolebindings", "get")
	assertTransitionRenderNoResourceVerb(t, role, "rbac.authorization.k8s.io", "rolebindings", "patch")
	assertTransitionRenderNoResourceVerb(t, role, "rbac.authorization.k8s.io", "roles", "get")
	assertTransitionRenderNoResourceVerb(t, role, "rbac.authorization.k8s.io", "clusterroles", "bind")
	assertTransitionRenderRule(t, role, "authorization.k8s.io", "subjectaccessreviews", nil, []string{"create"})
	assertTransitionRenderNoResourceVerb(t, role, "discovery.k8s.io", "endpointslices", "list")
	assertTransitionRenderNoBindingCreate(t, role)

	rollout := &RolloutGuard{
		ReleaseName:                  "rbac-cutover",
		ReleaseNamespace:             "ptah-system",
		CoordinationNamespace:        "ptah-system",
		HookServiceAccountName:       hookServiceAccount,
		ControllerServiceAccountName: controllerName + "-v1-4d0b8e1c5cc7",
		ControllerDeploymentName:     controllerName,
		CertificateDeploymentName:    controllerName + "-cert-rotator",
		CertificateRuntimeEnabled:    true,
		ReleaseSequence:              1,
		ManagerImage:                 "ghcr.io/stokaro/ptah-operator@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	runtimeContract := RuntimeAdmissionContract{
		Namespace:                      rollout.ReleaseNamespace,
		ControllerServiceAccountName:   rollout.ControllerServiceAccountName,
		CertificateServiceAccountName:  rollout.CertificateDeploymentName,
		ControllerServiceAccountCreate: true,
		CertificateRuntimeEnabled:      true,
	}
	teardown := &PrivilegeTeardown{rollout: rollout, contract: runtimeContract}
	clusterContract := findTransitionAuthorizationContract(t, teardown.retiredAuthorizationContracts(), hookServiceAccount, "", true)
	roleContract := findTransitionAuthorizationContract(t, teardown.retiredAuthorizationContracts(), hookServiceAccount, rollout.ReleaseNamespace, false)
	discoveryContract := findTransitionAuthorizationContract(t, teardown.retiredAuthorizationContracts(), hookServiceAccount, "default", false)
	var renderedClusterRole rbacv1.ClusterRole
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(role.Object, &renderedClusterRole); err != nil {
		t.Fatalf("decode rendered controller RBAC cutover ClusterRole: %v", err)
	}
	if !reflect.DeepEqual(renderedClusterRole.Rules, clusterContract.rules) {
		t.Fatal("rendered controller RBAC cutover ClusterRole differs from the exact teardown inventory")
	}
	assertTransitionRenderedRoleRules(t, objects, rollout.ReleaseNamespace, hookServiceAccount, roleContract.rules)
	assertTransitionRenderedRoleRules(t, objects, "default", hookServiceAccount, discoveryContract.rules)
	assertTransitionRenderedBinding(
		t, objects, "RoleBinding", "default", hookServiceAccount, "Role", hookServiceAccount,
		map[string]any{"kind": "ServiceAccount", "name": hookServiceAccount, "namespace": rollout.ReleaseNamespace},
	)

	assertTransitionRenderedClusterRoleRules(t, objects, controllerName, currentControllerClusterRoleRules(rollout))
	assertTransitionRenderedRoleRules(t, objects, rollout.ReleaseNamespace, controllerName, currentControllerCoordinationRoleRules())
	assertTransitionRenderedRoleRules(
		t,
		objects,
		rollout.ReleaseNamespace,
		controllerName+"-runtime-admission",
		currentControllerRuntimeRoleRules(rollout, runtimeContract),
	)
	candidateSubject := map[string]any{
		"kind": "ServiceAccount", "name": rollout.ControllerServiceAccountName, "namespace": rollout.ReleaseNamespace,
	}
	assertTransitionRenderedBinding(t, objects, "ClusterRoleBinding", "", controllerName, "ClusterRole", controllerName, candidateSubject)
	assertTransitionRenderedRoleRules(t, objects, "default", controllerName+"-runtime-discovery", currentControllerDiscoveryRoleRules())
	assertTransitionRenderedBinding(
		t,
		objects,
		"RoleBinding",
		"default",
		controllerName+"-runtime-discovery",
		"Role",
		controllerName+"-runtime-discovery",
		candidateSubject,
		map[string]any{
			"kind": "ServiceAccount", "name": runtimeContract.CertificateServiceAccountName, "namespace": rollout.ReleaseNamespace,
		},
	)
	assertTransitionRenderedBinding(t, objects, "RoleBinding", rollout.ReleaseNamespace, controllerName, "Role", controllerName, candidateSubject)
	assertTransitionRenderedBinding(
		t,
		objects,
		"RoleBinding",
		rollout.ReleaseNamespace,
		controllerName+"-runtime-admission",
		"Role",
		controllerName+"-runtime-admission",
		candidateSubject,
		map[string]any{
			"kind": "ServiceAccount", "name": runtimeContract.CertificateServiceAccountName, "namespace": rollout.ReleaseNamespace,
		},
	)
}

func TestControllerRBACCutoverHookNamespacesMatchRetirementInventory(t *testing.T) {
	t.Parallel()
	for _, namespaces := range [][2]string{
		{"ptah-system", "ptah-system"},
		{"ptah-system", "ptah-coordination"},
		{"ptah-system", "default"},
		{"default", "default"},
		{"default", "ptah-coordination"},
	} {
		t.Run(namespaces[0]+"/"+namespaces[1], func(t *testing.T) {
			t.Parallel()
			objects := renderControllerRBACCutoverChart(t,
				"--namespace", namespaces[0], "--set-string", "coordination.namespace="+namespaces[1],
			)
			job := findControllerRBACCutoverJob(t, objects)
			hook, _, _ := unstructured.NestedString(job.Object, "spec", "template", "spec", "serviceAccountName")
			rollout := &RolloutGuard{
				ReleaseName: "rbac-cutover", ReleaseNamespace: namespaces[0], CoordinationNamespace: namespaces[1],
				HookServiceAccountName: hook, ControllerDeploymentName: "rbac-cutover-ptah-operator",
			}
			teardown := &PrivilegeTeardown{rollout: rollout}
			wantNamespaces := map[string]bool{namespaces[0]: true, namespaces[1]: true, "default": true}
			seenRoles, seenBindings := map[string]bool{}, map[string]bool{}
			for _, object := range objects {
				if object.GetName() != hook {
					continue
				}
				namespace := object.GetNamespace()
				switch object.GetKind() {
				case "ClusterRole":
					assertTransitionRenderNoResourceVerb(t, object, "rbac.authorization.k8s.io", "roles", "get")
					assertTransitionRenderNoResourceVerb(t, object, "rbac.authorization.k8s.io", "rolebindings", "patch")
					assertTransitionRenderNoResourceVerb(t, object, "rbac.authorization.k8s.io", "clusterroles", "bind")
				case "Role":
					if !wantNamespaces[namespace] || seenRoles[namespace] {
						t.Fatalf("unexpected or duplicate hook Role namespace %q", namespace)
					}
					seenRoles[namespace] = true
					if namespace == rollout.ReleaseNamespace {
						assertTransitionRenderRule(t, object, "", "pods", nil, []string{"list", "watch"})
					} else {
						assertTransitionRenderNoResourceVerb(t, object, "", "pods", "list")
						assertTransitionRenderNoResourceVerb(t, object, "", "pods", "watch")
					}
					assertTransitionRenderNoResourceVerb(t, object, "rbac.authorization.k8s.io", "roles", "bind")
					assertTransitionRenderNoBindingCreate(t, object)
					for _, rule := range teardown.hookBindingTransitionRules(namespace) {
						assertTransitionRenderRule(t, object, rbacv1.GroupName, rule.Resources[0], rule.ResourceNames, rule.Verbs)
					}
					// No other RBAC resources, names, or verbs are allowed in this namespace.
					var role rbacv1.Role
					if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &role); err != nil {
						t.Fatal(err)
					}
					var rbacRules []rbacv1.PolicyRule
					for _, rule := range role.Rules {
						if slices.Contains(rule.APIGroups, rbacv1.GroupName) {
							rbacRules = append(rbacRules, rule)
						}
					}
					if !reflect.DeepEqual(rbacRules, teardown.hookBindingTransitionRules(namespace)) {
						t.Fatalf("hook Role in %q has unexpected RBAC authority", namespace)
					}
				case "RoleBinding":
					if !wantNamespaces[namespace] || seenBindings[namespace] {
						t.Fatalf("unexpected or duplicate hook RoleBinding namespace %q", namespace)
					}
					seenBindings[namespace] = true
					assertTransitionRenderedBinding(t, objects, "RoleBinding", namespace, hook, "Role", hook,
						map[string]any{"kind": "ServiceAccount", "name": hook, "namespace": namespaces[0]})
				}
			}
			if !reflect.DeepEqual(seenRoles, wantNamespaces) || !reflect.DeepEqual(seenBindings, wantNamespaces) {
				t.Fatalf("hook roles/bindings = %v/%v, want namespaces %v", seenRoles, seenBindings, wantNamespaces)
			}
			residual, err := TeardownGuardRoleName(hook)
			if err != nil {
				t.Fatal(err)
			}
			residualRole := findTransitionRenderObjectInNamespace(t, objects, "Role", namespaces[0], residual)
			assertTransitionRenderRule(t, residualRole, "", "pods", nil, []string{"list", "watch"})
			for _, namespace := range []string{namespaces[0], namespaces[1], "default"} {
				findTransitionAuthorizationContract(t, teardown.retiredAuthorizationContracts(), hook, namespace, false)
				found := false
				for _, contract := range teardown.bindingContracts() {
					if !contract.cluster && contract.name == hook && contract.namespace == namespace {
						found = true
					}
				}
				if !found {
					t.Fatalf("hook RoleBinding in %q missing from retirement inventory", namespace)
				}
			}
			if namespaces[1] != namespaces[0] {
				cleanup, err := TeardownPrivilegeRoleName(hook)
				if err != nil {
					t.Fatal(err)
				}
				role := findTransitionRenderObjectInNamespace(t, objects, "Role", namespaces[1], cleanup)
				var rendered rbacv1.Role
				if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(role.Object, &rendered); err != nil {
					t.Fatal(err)
				}
				if len(rendered.Rules) != 1 || !slices.Contains(rendered.Rules[0].ResourceNames, hook) {
					t.Fatal("coordination cleanup Role omits the exact hook RoleBinding")
				}
			}
		})
	}
}

func TestControllerRBACCutoverPredecessorHookRolesMatchRetirementInventory(t *testing.T) {
	t.Parallel()
	chart := newControllerRBACPredecessorChart(t)
	for _, namespaces := range [][2]string{
		{"ptah-system", "ptah-system"}, {"ptah-system", "coordination"},
		{"ptah-system", "default"}, {"default", "default"}, {"default", "coordination"},
	} {
		for _, previous := range []int32{0, 1} {
			t.Run(fmt.Sprintf("%s/%s/previous=%d", namespaces[0], namespaces[1], previous), func(t *testing.T) {
				guard := originParityGuard(previous, namespaces[0], namespaces[1], true, "managed")
				objects := renderControllerRBACPredecessorChart(t, chart, guard)
				rollout := &RolloutGuard{
					ReleaseName: guard.ReleaseName, ReleaseNamespace: guard.ReleaseNamespace, CoordinationNamespace: guard.CoordinationNamespace,
					HookServiceAccountName: guard.HookServiceAccountName, ControllerServiceAccountName: guard.ControllerServiceAccountName,
					ControllerDeploymentName: guard.ControllerDeploymentName, CertificateDeploymentName: guard.CertificateDeploymentName,
					ControllerServiceAccountManaged: true, CertificateRuntimeEnabled: true,
					PreviousControllerServiceAccountName:    guard.PreviousControllerServiceAccountName,
					PreviousControllerServiceAccountUID:     guard.PreviousControllerServiceAccountUID,
					PreviousControllerServiceAccountManaged: true, PreviousControllerReleaseSequence: previous,
					PreviousControllerManagerImage: guard.PreviousControllerManagerImage,
					ControllerStateVersion:         guard.ControllerStateVersion, AdmissionContractVersion: guard.AdmissionContractVersion,
					ReleaseSequence: guard.ReleaseSequence, ManagerImage: guard.ManagerImage,
				}
				teardown := &PrivilegeTeardown{rollout: rollout, contract: RuntimeAdmissionContract{
					Namespace: guard.ReleaseNamespace, ControllerServiceAccountName: guard.ControllerServiceAccountName,
					CertificateServiceAccountName: guard.CertificateServiceAccountName, CertificateRuntimeEnabled: true,
				}}
				for _, contract := range teardown.retiredAuthorizationContracts() {
					if contract.name != guard.HookServiceAccountName {
						continue
					}
					if contract.cluster {
						assertTransitionRenderedClusterRoleRules(t, objects, contract.name, contract.rules)
					} else {
						assertTransitionRenderedRoleRules(t, objects, contract.namespace, contract.name, contract.rules)
					}
				}
				clusterRole := findTransitionRenderObject(t, objects, "ClusterRole", guard.HookServiceAccountName)
				assertTransitionRenderRule(t, clusterRole, rbacv1.GroupName, "clusterroles", []string{guard.ControllerDeploymentName}, []string{"get", "bind"})
				assertTransitionRenderNoBindingCreate(t, clusterRole)
				assertTransitionRenderNoResourceVerb(t, clusterRole, rbacv1.GroupName, "rolebindings", "patch")
				for _, object := range objects {
					if object.GetName() != guard.HookServiceAccountName || object.GetKind() != "Role" {
						continue
					}
					assertTransitionRenderNoBindingCreate(t, object)
					assertTransitionRenderNoResourceVerb(t, object, rbacv1.GroupName, "roles", "escalate")
				}
			})
		}
	}
}

func newControllerRBACPredecessorChart(t *testing.T) string {
	t.Helper()
	chart := newOriginParityChart(t)
	data, err := os.ReadFile(filepath.Join("..", "..", "charts", "ptah-operator", "templates", "crd-upgrade.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	prefixEnd := strings.Index(source, "\napiVersion: batch/v1\n")
	start := strings.Index(source, "\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: {{ $clusterRoleName }}")
	if prefixEnd < 0 || start < prefixEnd {
		t.Fatal("production reconcile hook RBAC boundaries changed")
	}
	end := strings.Index(source[start:], "\napiVersion: batch/v1\n")
	if end < 0 {
		t.Fatal("production reconcile hook RBAC end boundary changed")
	}
	// Retain the actual template variable declarations and exact RBAC objects.
	// Workload templates are unrelated to these privilege contracts.
	rbac := source[:prefixEnd] + source[start:start+end]
	if err := os.WriteFile(filepath.Join(chart, "templates", "crd-rbac.yaml"), []byte(rbac), 0o600); err != nil {
		t.Fatal(err)
	}
	return chart
}

func renderControllerRBACPredecessorChart(t *testing.T, chart string, guard *ServiceAccountOriginGuard) []*unstructured.Unstructured {
	t.Helper()
	values := map[string]any{
		"image":          map[string]any{"repository": "registry.example/ptah", "digest": "sha256:" + strings.Repeat("a", 64)},
		"coordination":   map[string]any{"namespace": guard.CoordinationNamespace},
		"serviceAccount": map[string]any{"create": true, "name": "origin-controller"},
		"execution": map[string]any{
			"executorImage": "registry.example/ptah@sha256:" + strings.Repeat("c", 64),
			"runnerImage":   "registry.example/ptah@sha256:" + strings.Repeat("d", 64), "ptahVersion": "rbac-predecessor-test",
		},
		"originParitySequence": fmt.Sprint(guard.ReleaseSequence),
		"originParityPredecessor": map[string]any{
			"name": guard.PreviousControllerServiceAccountName, "uid": string(guard.PreviousControllerServiceAccountUID),
			"managed": "true", "releaseSequence": fmt.Sprint(guard.PreviousControllerReleaseSequence),
			"managerImage": guard.PreviousControllerManagerImage,
		},
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return renderControllerRBACChartObjects(t, []string{"template", guard.ReleaseName, chart, "--namespace", guard.ReleaseNamespace, "-f", "-"}, encoded)
}

func renderControllerRBACCutoverChart(t *testing.T, extraArgs ...string) []*unstructured.Unstructured {
	t.Helper()
	_, filename, _, _ := goruntime.Caller(0)
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	args := []string{
		"template", "rbac-cutover", filepath.Join(repositoryRoot, "charts", "ptah-operator"),
		"--namespace", "ptah-system",
		"--set-string", "image.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"--set-string", "execution.executorImage=example.invalid/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"--set-string", "execution.runnerImage=example.invalid/operator@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"--set-string", "execution.ptahVersion=rbac-cutover",
	}
	args = append(args, extraArgs...)
	return renderControllerRBACChartObjects(t, args, nil)
}

func renderControllerRBACChartObjects(t *testing.T, args []string, values []byte) []*unstructured.Unstructured {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for controller RBAC cutover render tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, helm, args...)
	command.Stdin = bytes.NewReader(values)
	temporaryHome := t.TempDir()
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(temporaryHome, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(temporaryHome, "config"),
		"HELM_DATA_HOME="+filepath.Join(temporaryHome, "data"),
	)
	output, err := command.Output()
	if err != nil {
		// The render contains generated private key material, so it must never be
		// included in a test failure.
		t.Fatalf("helm template failed: %v", err)
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	objects := []*unstructured.Unstructured{}
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode Helm object: %v", err)
		}
		if object.Object != nil && object.GetKind() != "" {
			objects = append(objects, object)
		}
	}
	return objects
}

func findTransitionAuthorizationContract(
	t *testing.T,
	contracts []privilegeAuthorizationContract,
	name, namespace string,
	cluster bool,
) privilegeAuthorizationContract {
	t.Helper()
	for _, contract := range contracts {
		if contract.name == name && contract.namespace == namespace && contract.cluster == cluster {
			return contract
		}
	}
	t.Fatalf("exact transition authorization contract for cluster=%t namespace=%q name=%q was not found", cluster, namespace, name)
	return privilegeAuthorizationContract{}
}

func assertTransitionRenderedClusterRoleRules(
	t *testing.T,
	objects []*unstructured.Unstructured,
	name string,
	want []rbacv1.PolicyRule,
) {
	t.Helper()
	object := findTransitionRenderObject(t, objects, "ClusterRole", name)
	var role rbacv1.ClusterRole
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &role); err != nil {
		t.Fatalf("decode rendered ClusterRole/%s: %v", name, err)
	}
	if !reflect.DeepEqual(role.Rules, want) {
		t.Fatalf("rendered ClusterRole/%s differs from the exact controller transition contract", name)
	}
}

func assertTransitionRenderedRoleRules(
	t *testing.T,
	objects []*unstructured.Unstructured,
	namespace, name string,
	want []rbacv1.PolicyRule,
) {
	t.Helper()
	object := findTransitionRenderObjectInNamespace(t, objects, "Role", namespace, name)
	var role rbacv1.Role
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &role); err != nil {
		t.Fatalf("decode rendered Role/%s/%s: %v", namespace, name, err)
	}
	if !reflect.DeepEqual(role.Rules, want) {
		t.Fatalf("rendered Role/%s/%s differs from the exact controller transition contract", namespace, name)
	}
}

func assertTransitionRenderedBinding(
	t *testing.T,
	objects []*unstructured.Unstructured,
	kind, namespace, name, roleKind, roleName string,
	wantSubjects ...map[string]any,
) {
	t.Helper()
	object := findTransitionRenderObjectInNamespace(t, objects, kind, namespace, name)
	roleRef, found, err := unstructured.NestedMap(object.Object, "roleRef")
	if err != nil || !found || !reflect.DeepEqual(roleRef, map[string]any{
		"apiGroup": "rbac.authorization.k8s.io", "kind": roleKind, "name": roleName,
	}) {
		t.Fatalf("rendered %s/%s roleRef = %#v, found=%t, error=%v", kind, name, roleRef, found, err)
	}
	subjects, found, err := unstructured.NestedSlice(object.Object, "subjects")
	want := make([]any, 0, len(wantSubjects))
	for _, subject := range wantSubjects {
		want = append(want, subject)
	}
	if err != nil || !found || !reflect.DeepEqual(subjects, want) {
		t.Fatalf("rendered %s/%s subjects = %#v, found=%t, error=%v", kind, name, subjects, found, err)
	}
}

func findControllerRBACCutoverJob(t *testing.T, objects []*unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() != "Job" {
			continue
		}
		containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
		if err != nil || !found || len(containers) != 1 {
			continue
		}
		args := transitionRenderStringSlice(containers[0].(map[string]any)["args"])
		if slices.Contains(args, "--timeout=360s") {
			return object
		}
	}
	t.Fatal("rendered controller RBAC cutover Job was not found")
	return nil
}

func findTransitionRenderObject(t *testing.T, objects []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind && object.GetName() == name {
			return object
		}
	}
	t.Fatalf("rendered %s/%s was not found", kind, name)
	return nil
}

func findTransitionRenderObjectInNamespace(
	t *testing.T,
	objects []*unstructured.Unstructured,
	kind, namespace, name string,
) *unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind && object.GetNamespace() == namespace && object.GetName() == name {
			return object
		}
	}
	t.Fatalf("rendered %s/%s/%s was not found", kind, namespace, name)
	return nil
}

func assertTransitionRenderRule(
	t *testing.T,
	role *unstructured.Unstructured,
	apiGroup, resource string,
	resourceNames, verbs []string,
) {
	t.Helper()
	rawRules, found, err := unstructured.NestedSlice(role.Object, "rules")
	if err != nil || !found {
		t.Fatalf("%s/%s has no rules", role.GetKind(), role.GetName())
	}
	wantNames := slices.Clone(resourceNames)
	wantVerbs := slices.Clone(verbs)
	slices.Sort(wantNames)
	slices.Sort(wantVerbs)
	for _, rawRule := range rawRules {
		rule := rawRule.(map[string]any)
		if !slices.Contains(transitionRenderStringSlice(rule["apiGroups"]), apiGroup) ||
			!slices.Contains(transitionRenderStringSlice(rule["resources"]), resource) {
			continue
		}
		gotNames := transitionRenderStringSlice(rule["resourceNames"])
		gotVerbs := transitionRenderStringSlice(rule["verbs"])
		slices.Sort(gotNames)
		slices.Sort(gotVerbs)
		if slices.Equal(gotNames, wantNames) && slices.Equal(gotVerbs, wantVerbs) {
			return
		}
	}
	t.Fatalf("%s/%s has no exact %s/%s rule with resourceNames=%v verbs=%v", role.GetKind(), role.GetName(), apiGroup, resource, resourceNames, verbs)
}

func assertTransitionRenderNoBindingCreate(t *testing.T, role *unstructured.Unstructured) {
	t.Helper()
	rawRules, found, err := unstructured.NestedSlice(role.Object, "rules")
	if err != nil || !found {
		t.Fatalf("%s/%s has no rules", role.GetKind(), role.GetName())
	}
	for _, rawRule := range rawRules {
		rule := rawRule.(map[string]any)
		if !slices.Contains(transitionRenderStringSlice(rule["apiGroups"]), "rbac.authorization.k8s.io") ||
			!slices.Contains(transitionRenderStringSlice(rule["verbs"]), "create") {
			continue
		}
		for _, resource := range transitionRenderStringSlice(rule["resources"]) {
			if resource == "rolebindings" || resource == "clusterrolebindings" {
				t.Fatalf("%s/%s grants forbidden create on %s", role.GetKind(), role.GetName(), resource)
			}
		}
	}
}

func assertTransitionRenderNoResourceVerb(
	t *testing.T,
	role *unstructured.Unstructured,
	apiGroup, resource, verb string,
) {
	t.Helper()
	rawRules, found, err := unstructured.NestedSlice(role.Object, "rules")
	if err != nil || !found {
		t.Fatalf("%s/%s has no rules", role.GetKind(), role.GetName())
	}
	for _, rawRule := range rawRules {
		rule := rawRule.(map[string]any)
		if slices.Contains(transitionRenderStringSlice(rule["apiGroups"]), apiGroup) &&
			slices.Contains(transitionRenderStringSlice(rule["resources"]), resource) &&
			slices.Contains(transitionRenderStringSlice(rule["verbs"]), verb) {
			t.Fatalf("%s/%s grants forbidden %s on %s/%s", role.GetKind(), role.GetName(), verb, apiGroup, resource)
		}
	}
}

func transitionRenderStringSlice(value any) []string {
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, item := range values {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
