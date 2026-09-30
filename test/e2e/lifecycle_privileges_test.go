package e2e

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestLifecyclePrivilegeInventoryReadsTheShippedChart(t *testing.T) {
	t.Parallel()
	raw := []byte(alHelm(t, "template", "ptah-operator", alChart, "--namespace", "ptah-system",
		"--set-string", "image.digest=sha256:"+strings.Repeat("a", 64),
		"--set-string", "execution.runnerImage=ghcr.io/stokaro/ptah-operator@sha256:"+strings.Repeat("a", 64),
		"--set-string", "execution.executorImage=ghcr.io/stokaro/ptah@sha256:"+strings.Repeat("b", 64),
		"--set-string", "execution.ptahVersion=v0.7.0"))
	documents, err := decodeManifests(raw)
	if err != nil {
		t.Fatal(err)
	}
	var manifest, hooks bytes.Buffer
	for _, document := range documents {
		object := &unstructured.Unstructured{Object: document}
		target := &manifest
		if object.GetAnnotations()["helm.sh/hook"] != "" {
			target = &hooks
		}
		if err := json.NewEncoder(target).Encode(document); err != nil {
			t.Fatal(err)
		}
	}
	objects, err := lifecyclePrivilegeObjects(manifest.Bytes(), hooks.Bytes(), "ptah-system")
	if err != nil {
		t.Fatal(err)
	}
	hookAccounts, installedAccounts := 0, 0
	for _, object := range objects {
		if object.kind == "ServiceAccount" {
			if object.hook {
				hookAccounts++
			} else {
				installedAccounts++
			}
		}
	}
	if hookAccounts == 0 || installedAccounts == 0 {
		t.Fatal("the inventory omitted installed or hook identities")
	}
	if _, err := lifecyclePrivilegeObjects(manifest.Bytes(), nil, "ptah-system"); err == nil {
		t.Fatal("an empty hook inventory hid the hook privileges")
	}
	if _, err := lifecyclePrivilegeObjects(nil, hooks.Bytes(), "ptah-system"); err == nil {
		t.Fatal("hook privileges substituted for the installed runtime")
	}
	for name, broken := range map[string][]byte{
		"empty":            nil,
		"invalid":          []byte("not: [valid"),
		"missing accounts": bytes.ReplaceAll(manifest.Bytes(), []byte(`"kind":"ServiceAccount"`), []byte(`"kind":"ConfigMap"`)),
		"duplicate":        append(bytes.Clone(manifest.Bytes()), manifest.Bytes()...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := lifecyclePrivilegeObjects(broken, nil, "ptah-system"); err == nil {
				t.Fatal("incomplete privilege inventory passed")
			}
		})
	}
	// Objects retained by policy must not silently disappear from the removal
	// contract. Other retained kinds, such as CRDs, have their own assertions.
	kept := append(bytes.Clone(manifest.Bytes()), []byte(`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"leaked","annotations":{"helm.sh/resource-policy":"keep"}}}`)...)
	if _, err := lifecyclePrivilegeObjects(kept, hooks.Bytes(), "ptah-system"); err == nil {
		t.Fatal("a retained privilege was silently excluded")
	}
	if _, err := lifecyclePrivilegeObjects(manifest.Bytes(), hooks.Bytes(), ""); err == nil {
		t.Fatal("missing release namespace passed")
	}
	t.Logf("read %d privilege identities from the rendered chart, including %d installed and %d hook ServiceAccounts", len(objects), installedAccounts, hookAccounts)
}

func TestLifecyclePrivilegeAuditFindsUnlabeledAndRenamedGrants(t *testing.T) {
	t.Parallel()
	objects := []lifecyclePrivilegeObject{
		{kind: "ServiceAccount", namespace: "operator", name: "manager"},
		{kind: "Role", namespace: "coordination", name: "leases"},
		{kind: "ClusterRole", name: "manager"},
	}
	for name, test := range map[string]struct {
		namespace string
		subjects  []rbacv1.Subject
		role      rbacv1.RoleRef
		want      bool
	}{
		"direct in tenant namespace":  {"tenant", []rbacv1.Subject{{Kind: "ServiceAccount", Name: "manager", Namespace: "operator"}}, rbacv1.RoleRef{}, true},
		"cluster binding":             {"", []rbacv1.Subject{{Kind: "ServiceAccount", Name: "manager", Namespace: "operator"}}, rbacv1.RoleRef{}, true},
		"defaulted subject namespace": {"operator", []rbacv1.Subject{{Kind: "ServiceAccount", Name: "manager"}}, rbacv1.RoleRef{}, true},
		"username spelling":           {"tenant", []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: "system:serviceaccount:operator:manager"}}, rbacv1.RoleRef{}, true},
		"namespace group":             {"tenant", []rbacv1.Subject{{Kind: "Group", APIGroup: rbacv1.GroupName, Name: "system:serviceaccounts:operator"}}, rbacv1.RoleRef{}, true},
		"dangling cluster role":       {"tenant", nil, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "manager"}, true},
		"dangling namespaced role":    {"coordination", nil, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "leases"}, true},
		"unrelated same account name": {"tenant", []rbacv1.Subject{{Kind: "ServiceAccount", Name: "manager", Namespace: "tenant"}}, rbacv1.RoleRef{}, false},
		"unrelated same role name":    {"tenant", nil, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "leases"}, false},
		"default Kubernetes grants":   {"", []rbacv1.Subject{{Kind: "Group", APIGroup: rbacv1.GroupName, Name: "system:authenticated"}}, rbacv1.RoleRef{}, false},
		"other namespace group":       {"", []rbacv1.Subject{{Kind: "Group", APIGroup: rbacv1.GroupName, Name: "system:serviceaccounts:operator-other"}}, rbacv1.RoleRef{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := lifecycleBindingUsesRelease(objects, test.namespace, test.subjects, test.role); got != test.want {
				t.Fatalf("binding attribution=%v, want %v", got, test.want)
			}
		})
	}
}

func TestLifecyclePrivilegeSnapshotsRetainAuthorityAndIgnoreBookkeeping(t *testing.T) {
	t.Parallel()
	fixtures := map[string]string{
		"ServiceAccount":     `{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"manager","namespace":"operator","uid":"original"},"automountServiceAccountToken":false}`,
		"Role":               `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"leases","namespace":"operator","uid":"original"},"rules":[{"apiGroups":["coordination.k8s.io"],"resources":["leases"],"verbs":["get"]}]}`,
		"ClusterRole":        `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"manager","uid":"original"},"rules":[{"apiGroups":[""],"resources":["secrets"],"verbs":["get"]}]}`,
		"RoleBinding":        `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"manager","namespace":"operator","uid":"original"},"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"Role","name":"leases"},"subjects":[{"kind":"ServiceAccount","name":"manager","namespace":"operator"}]}`,
		"ClusterRoleBinding": `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRoleBinding","metadata":{"name":"manager","uid":"original"},"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"manager"},"subjects":[{"kind":"ServiceAccount","name":"manager","namespace":"operator"}]}`,
	}
	for kind, raw := range fixtures {
		t.Run(kind, func(t *testing.T) {
			original := &unstructured.Unstructured{}
			if err := original.UnmarshalJSON([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			before, err := lifecyclePrivilegeState(original)
			if err != nil {
				t.Fatal(err)
			}
			bookkeeping := original.DeepCopy()
			bookkeeping.SetResourceVersion("new-write")
			bookkeeping.Object["metadata"].(map[string]any)["managedFields"] = []any{map[string]any{"manager": "helm", "operation": "Apply"}}
			after, err := lifecyclePrivilegeState(bookkeeping)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("server bookkeeping changed the measured grant", err)
			}
			mutations := map[string]func(*unstructured.Unstructured){
				"replaced identity": func(o *unstructured.Unstructured) { o.SetUID("replacement") },
			}
			switch kind {
			case "ServiceAccount":
				mutations["automatic token grant"] = func(o *unstructured.Unstructured) { o.Object["automountServiceAccountToken"] = true }
				mutations["new pull credential"] = func(o *unstructured.Unstructured) {
					o.Object["imagePullSecrets"] = []any{map[string]any{"name": "other"}}
				}
			case "Role", "ClusterRole":
				mutations["broadened rules"] = func(o *unstructured.Unstructured) {
					o.Object["rules"].([]any)[0].(map[string]any)["verbs"] = []any{"*"}
				}
				if kind == "ClusterRole" {
					mutations["aggregation added"] = func(o *unstructured.Unstructured) {
						o.Object["aggregationRule"] = map[string]any{"clusterRoleSelectors": []any{map[string]any{}}}
					}
				}
			default:
				mutations["another account"] = func(o *unstructured.Unstructured) { o.Object["subjects"].([]any)[0].(map[string]any)["name"] = "other" }
				mutations["another role"] = func(o *unstructured.Unstructured) { o.Object["roleRef"].(map[string]any)["name"] = "cluster-admin" }
			}
			for name, mutate := range mutations {
				t.Run(name, func(t *testing.T) {
					changed := original.DeepCopy()
					mutate(changed)
					after, err := lifecyclePrivilegeState(changed)
					if err != nil || bytes.Equal(before, after) {
						t.Fatal("changed authority disappeared from the snapshot", err)
					}
				})
			}
		})
	}
	if _, err := lifecyclePrivilegeState(nil); err == nil {
		t.Fatal("missing live identity supplied a snapshot")
	}
}
