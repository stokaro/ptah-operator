package crd_test

import (
	"context"
	"fmt"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// harness.AwaitRBAC is what the chart's RBAC rows and the admission policy
// suite's setup rely on: after it returns, a single review answers for every
// grant written before it. Each round writes grants of every shape a chart
// renders, fences once, and sends one review per grant, with no retry. A
// round sent without the fence is refused now and then under load, which is
// the race the fence closes.
func TestAwaitRBACAnswersForEveryEarlierGrant(t *testing.T) {
	plane.Require(t)
	ctx := context.Background()
	namespace := newNamespace(t, "rbac-fence")
	for round := range 25 {
		user := fmt.Sprintf("%s-user-%d", namespace, round)
		name := fmt.Sprintf("%s-%d", namespace, round)
		subjects := []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}}
		rule := func(resource string) []rbacv1.PolicyRule {
			return []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{resource}, Verbs: []string{"get"}}}
		}
		objects := []client.Object{
			&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Rules: rule("configmaps")},
			&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Subjects: subjects,
				RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}},
			&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: rule("secrets")},
			&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, Subjects: subjects,
				RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}},
			&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name + "-local"}, Rules: rule("services")},
			&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name + "-local"}, Subjects: subjects,
				RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name + "-local"}},
		}
		for _, object := range objects {
			if err := api.Create(ctx, object); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = api.Delete(context.Background(), object) })
		}
		if err := plane.AwaitRBAC(ctx, namespace); err != nil {
			t.Fatal(err)
		}
		for _, row := range []struct {
			grant, resource, namespace string
			allowed                    bool
		}{
			{"a Role through a RoleBinding", "configmaps", namespace, true},
			{"a ClusterRole through a ClusterRoleBinding", "secrets", "", true},
			{"a ClusterRole through a RoleBinding", "services", namespace, true},
			// So an allowed review is not one that allows anything.
			{"nothing", "pods", namespace, false},
		} {
			review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User:               user,
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: row.namespace, Resource: row.resource, Verb: "get"},
			}}
			if err := api.Create(ctx, review); err != nil {
				t.Fatal(err)
			}
			if review.Status.Allowed != row.allowed {
				t.Fatalf("round %d: %s: allowed=%t, want %t: %+v", round, row.grant, review.Status.Allowed, row.allowed, review.Status)
			}
		}
	}
}
