package crdupgrade

// This render test intentionally uses the package under test because it
// compares the unexported teardown inventory with the exact rendered hook
// roles. A public API for mutable security contracts would be less safe.

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"slices"
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
	assertTransitionRenderRule(t, role, "rbac.authorization.k8s.io", "rolebindings", []string{controllerName, controllerName + "-runtime-admission"}, []string{"get", "patch"})
	assertTransitionRenderRule(t, role, "rbac.authorization.k8s.io", "clusterroles", []string{controllerName}, []string{"get"})
	assertTransitionRenderRule(t, role, "rbac.authorization.k8s.io", "roles", []string{controllerName, controllerName + "-runtime-admission"}, []string{"get"})
	assertTransitionRenderRule(t, role, "authorization.k8s.io", "subjectaccessreviews", nil, []string{"create"})
	assertTransitionRenderRule(t, role, "discovery.k8s.io", "endpointslices", nil, []string{"list"})
	assertTransitionRenderNoBindingCreate(t, role)

	rollout := &RolloutGuard{
		ReleaseName:                  "rbac-cutover",
		ReleaseNamespace:             "ptah-system",
		CoordinationNamespace:        "ptah-system",
		HookServiceAccountName:       hookServiceAccount,
		ControllerServiceAccountName: controllerName + "-v1-4d0b8e1c5cc7",
		ControllerDeploymentName:     controllerName,
		CertificateDeploymentName:    controllerName + "-cert-rotator",
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
	var renderedClusterRole rbacv1.ClusterRole
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(role.Object, &renderedClusterRole); err != nil {
		t.Fatalf("decode rendered controller RBAC cutover ClusterRole: %v", err)
	}
	if !reflect.DeepEqual(renderedClusterRole.Rules, clusterContract.rules) {
		t.Fatal("rendered controller RBAC cutover ClusterRole differs from the exact teardown inventory")
	}
	roleObject := findTransitionRenderObject(t, objects, "Role", hookServiceAccount)
	var renderedRole rbacv1.Role
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(roleObject.Object, &renderedRole); err != nil {
		t.Fatalf("decode rendered controller RBAC cutover Role: %v", err)
	}
	if !reflect.DeepEqual(renderedRole.Rules, roleContract.rules) {
		t.Fatal("rendered controller RBAC cutover Role differs from the exact teardown inventory")
	}

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

func renderControllerRBACCutoverChart(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for controller RBAC cutover render tests")
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, helm, args...)
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
	object := findTransitionRenderObject(t, objects, "Role", name)
	if object.GetNamespace() != namespace {
		t.Fatalf("rendered Role/%s namespace = %q, want %q", name, object.GetNamespace(), namespace)
	}
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
	object := findTransitionRenderObject(t, objects, kind, name)
	if object.GetNamespace() != namespace {
		t.Fatalf("rendered %s/%s namespace = %q, want %q", kind, name, object.GetNamespace(), namespace)
	}
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
