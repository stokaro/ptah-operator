package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// fenceGroup is an API group nothing serves, so a grant on it allows nothing
// but the reviews AwaitRBAC sends.
const fenceGroup = "rbac-fence.envtest.operator.ptah.run"

// AwaitRBAC returns once the API server's authorizer has seen every Role,
// RoleBinding, ClusterRole and ClusterRoleBinding written before the call.
//
// The authorizer reads RBAC through the API server's own informers, so a
// request sent right after its grant is written can be refused, and a
// refusal read then says nothing about the grant. An informer receives one
// kind's writes in the order the store made them. So AwaitRBAC writes one
// grant of each kind, a Role and a RoleBinding in namespace and a
// ClusterRole and a ClusterRoleBinding, and waits until the authorizer allows
// what each pair grants. Once it does, every earlier write of those kinds has
// arrived too, and a review then answers for the RBAC as written, whether it
// allows or refuses. The fence is removed before AwaitRBAC returns.
func (plane *ControlPlane) AwaitRBAC(ctx context.Context, namespace string) error {
	api, err := kubernetes.NewForConfig(plane.Config)
	if err != nil {
		return err
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	name := "envtest-rbac-fence-" + hex.EncodeToString(suffix)
	subject := []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}}
	rule := func(resource string) []rbacv1.PolicyRule {
		return []rbacv1.PolicyRule{{APIGroups: []string{fenceGroup}, Resources: []string{resource}, Verbs: []string{"get"}}}
	}
	meta := metav1.ObjectMeta{Name: name}
	namespaced := metav1.ObjectMeta{Name: name, Namespace: namespace}
	rbac := api.RbacV1()
	defer func() {
		// The fence grants nothing a suite reads, so a removal that fails
		// leaves nothing a row depends on.
		ctx := context.WithoutCancel(ctx)
		_ = rbac.RoleBindings(namespace).Delete(ctx, name, metav1.DeleteOptions{})
		_ = rbac.Roles(namespace).Delete(ctx, name, metav1.DeleteOptions{})
		_ = rbac.ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{})
		_ = rbac.ClusterRoles().Delete(ctx, name, metav1.DeleteOptions{})
	}()
	if _, err := rbac.ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: meta, Rules: rule("clusterfences")}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("write the ClusterRole fence: %w", err)
	}
	if _, err := rbac.ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: meta, Subjects: subject,
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
	}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("write the ClusterRoleBinding fence: %w", err)
	}
	if _, err := rbac.Roles(namespace).Create(ctx, &rbacv1.Role{ObjectMeta: namespaced, Rules: rule("fences")}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("write the Role fence: %w", err)
	}
	if _, err := rbac.RoleBindings(namespace).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: namespaced, Subjects: subject,
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
	}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("write the RoleBinding fence: %w", err)
	}
	// The cluster pair grants only clusterfences and the namespaced pair only
	// fences, so each review is allowed by its own pair alone.
	reviews := map[string]authorizationv1.ResourceAttributes{
		"ClusterRole and ClusterRoleBinding": {Group: fenceGroup, Resource: "clusterfences", Verb: "get"},
		"Role and RoleBinding":               {Namespace: namespace, Group: fenceGroup, Resource: "fences", Verb: "get"},
	}
	for pair, attributes := range reviews {
		var last authorizationv1.SubjectAccessReviewStatus
		if err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			review, err := api.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
				Spec: authorizationv1.SubjectAccessReviewSpec{User: name, ResourceAttributes: &attributes},
			}, metav1.CreateOptions{})
			if err != nil {
				return false, err
			}
			last = review.Status
			return last.Allowed, nil
		}); err != nil {
			return fmt.Errorf("the authorizer did not see the %s fence in namespace %q: %w; last review: %+v", pair, namespace, err, last)
		}
	}
	return nil
}
