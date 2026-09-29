package e2e

import (
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestStatusBarrierSelectsOnlyItsFamily(t *testing.T) {
	t.Parallel()
	role := &rbacv1.ClusterRole{Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{"operator.ptah.run"}, Resources: []string{"ptahschemas/status", "ptahschemaplans/status"}, Verbs: []string{"get", "update", "patch"}},
		{APIGroups: []string{"operator.ptah.run"}, Resources: []string{"ptahmigrations/status"}, Verbs: []string{"get", "update", "patch"}},
	}}
	for index, resource := range []string{"ptahschemas/status", "ptahmigrations/status"} {
		if got, err := statusRuleIndex(role, resource); err != nil || got != index {
			t.Fatalf("%s: rule=%d error=%v", resource, got, err)
		}
	}
	if _, err := statusRuleIndex(role, "missing/status"); err == nil {
		t.Fatal("accepted a missing rule")
	}
	role.Rules = append(role.Rules, role.Rules[0])
	if _, err := statusRuleIndex(role, "ptahschemas/status"); err == nil {
		t.Fatal("accepted overlapping grants that a single rule edit cannot pause")
	}
}

func TestStatusBarrierPatchRefusesConcurrentRoleEdits(t *testing.T) {
	t.Parallel()
	original := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "controller"}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{"operator.ptah.run"}, Resources: []string{"ptahschemas/status"}, Verbs: []string{"get", "update", "patch"}},
		{APIGroups: []string{"operator.ptah.run"}, Resources: []string{"ptahmigrations/status"}, Verbs: []string{"get", "update", "patch"}},
	}}
	rule := original.Rules[1]
	patch, err := ruleVerbsPatch(1, rule.APIGroups, rule.Resources, rule.Verbs, []string{"get"})
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*rbacv1.ClusterRole){
		"unchanged": func(*rbacv1.ClusterRole) {},
		"reordered": func(r *rbacv1.ClusterRole) { r.Rules[0], r.Rules[1] = r.Rules[1], r.Rules[0] },
		"new grant": func(r *rbacv1.ClusterRole) { r.Rules[1].Verbs = append(r.Rules[1].Verbs, "delete") },
		"new group": func(r *rbacv1.ClusterRole) { r.Rules[1].APIGroups = []string{"another.example"} },
	} {
		t.Run(name, func(t *testing.T) {
			role := original.DeepCopy()
			change(role)
			scheme := runtime.NewScheme()
			if err := rbacv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(role).Build()
			err := c.Patch(t.Context(), role.DeepCopy(), client.RawPatch(types.JSONPatchType, patch))
			if (err == nil) != (name == "unchanged") {
				t.Fatalf("patch result: %v", err)
			}
			live := &rbacv1.ClusterRole{}
			if err := c.Get(t.Context(), types.NamespacedName{Name: role.Name}, live); err != nil {
				t.Fatal(err)
			}
			want := role.DeepCopy()
			if name == "unchanged" {
				want.Rules[1].Verbs = []string{"get"}
			}
			if !reflect.DeepEqual(live.Rules, want.Rules) {
				t.Fatal("patch changed unrelated grants or overwrote a concurrent edit")
			}
		})
	}
}
