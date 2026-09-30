//go:build e2e

package e2e

import (
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func (l *lifecycleRun) captureReleasePrivileges() []lifecyclePrivilegeObject {
	l.t.Helper()
	manifest, _, err := l.helm("get", "manifest", l.in.helmRelease, "-n", l.in.operatorNamespace)
	l.check(err, "read the installed release's privilege manifest")
	hooks, _, err := l.helm("get", "hooks", l.in.helmRelease, "-n", l.in.operatorNamespace)
	l.check(err, "read the installed release's hook privilege manifest")
	objects, err := lifecyclePrivilegeObjects(manifest, hooks, l.in.operatorNamespace)
	l.check(err, "identify every installed release and hook privilege object")
	live := 0
	for _, identity := range objects {
		object := &unstructured.Unstructured{}
		object.SetAPIVersion(identity.apiVersion)
		object.SetKind(identity.kind)
		err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: identity.namespace, Name: identity.name}, object)
		if identity.hook && apierrors.IsNotFound(err) {
			continue
		}
		l.check(err, "read the exact pre-uninstall %s %s/%s", identity.kind, identity.namespace, identity.name)
		if object.GetUID() == "" {
			l.fatalf("pre-uninstall privilege object has no UID")
		}
		l.logf("privilege inventory before uninstall: kind=%s namespace=%s name=%s uid=%s", identity.kind, identity.namespace, identity.name, object.GetUID())
		live++
	}
	if live == 0 {
		l.fatalf("the release privilege inventory found no live objects")
	}
	return objects
}

func (l *lifecycleRun) assertReleasePrivilegesRemoved(objects []lifecyclePrivilegeObject) {
	l.t.Helper()
	if len(objects) == 0 {
		l.fatalf("uninstall privilege audit has no pre-uninstall inventory")
	}
	for _, identity := range objects {
		object := &unstructured.Unstructured{}
		object.SetAPIVersion(identity.apiVersion)
		object.SetKind(identity.kind)
		err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: identity.namespace, Name: identity.name}, object)
		if !apierrors.IsNotFound(err) {
			l.fatalf("uninstall did not remove exact %s %s/%s: %v", identity.kind, identity.namespace, identity.name, err)
		}
	}
	roles, clusters := &rbacv1.RoleBindingList{}, &rbacv1.ClusterRoleBindingList{}
	l.check(l.cluster.Client.List(l.ctx, roles), "list all namespaced grants after uninstall")
	l.check(l.cluster.Client.List(l.ctx, clusters), "list all cluster grants after uninstall")
	for _, binding := range roles.Items {
		if lifecycleBindingUsesRelease(objects, binding.Namespace, binding.Subjects, binding.RoleRef) {
			l.fatalf("RoleBinding %s/%s retained a release identity or role after uninstall", binding.Namespace, binding.Name)
		}
	}
	for _, binding := range clusters.Items {
		if lifecycleBindingUsesRelease(objects, "", binding.Subjects, binding.RoleRef) {
			l.fatalf("ClusterRoleBinding %s retained a release identity or role after uninstall", binding.Name)
		}
	}
	l.logf("privilege inventory after uninstall: declared=%d remaining=0; examined %d RoleBindings and %d ClusterRoleBindings without label filtering", len(objects), len(roles.Items), len(clusters.Items))
}
