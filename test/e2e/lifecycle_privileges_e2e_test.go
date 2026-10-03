//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha256"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func (l *lifecycleRun) releasePrivilegeObjects() []lifecyclePrivilegeObject {
	l.t.Helper()
	manifest, _, err := l.helm("get", "manifest", l.in.helmRelease, "-n", l.in.operatorNamespace)
	l.check(err, "read the installed release's privilege manifest")
	hooks, _, err := l.helm("get", "hooks", l.in.helmRelease, "-n", l.in.operatorNamespace)
	l.check(err, "read the installed release's hook privilege manifest")
	objects, err := lifecyclePrivilegeObjects(manifest, hooks, l.in.operatorNamespace)
	l.check(err, "identify every installed release and hook privilege object")
	return objects
}

func (l *lifecycleRun) captureReleasePrivileges() []lifecyclePrivilegeObject {
	l.t.Helper()
	objects := l.releasePrivilegeObjects()
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

func (l *lifecycleRun) snapshotRuntimePrivileges() map[lifecyclePrivilegeObject][]byte {
	l.t.Helper()
	states := map[lifecyclePrivilegeObject][]byte{}
	objects := l.releasePrivilegeObjects()
	hooks := map[lifecyclePrivilegeObject]bool{}
	for _, identity := range objects {
		if identity.hook {
			identity.hook = false
			hooks[identity] = true
			continue
		}
		states[identity] = l.readRuntimePrivilege(identity)
	}
	// A new binding can broaden an unchanged Role's audience or grant a
	// different Role to the same account. Compare these bindings too, across
	// every namespace, without trusting release labels. Declared hook grants
	// are transient and are checked by the uninstall inventory separately.
	for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		bindings := &unstructured.UnstructuredList{}
		bindings.SetAPIVersion("rbac.authorization.k8s.io/v1")
		bindings.SetKind(kind + "List")
		l.check(l.cluster.Client.List(l.ctx, bindings), "list all %s objects at the privilege boundary", kind)
		for _, binding := range bindings.Items {
			var grant struct {
				Subjects []rbacv1.Subject `json:"subjects"`
				RoleRef  rbacv1.RoleRef   `json:"roleRef"`
			}
			l.check(runtime.DefaultUnstructuredConverter.FromUnstructured(binding.Object, &grant), "decode %s grant", kind)
			identity := lifecyclePrivilegeObject{apiVersion: "rbac.authorization.k8s.io/v1", kind: kind, namespace: binding.GetNamespace(), name: binding.GetName()}
			if hooks[identity] || !lifecycleBindingUsesRelease(objects, identity.namespace, grant.Subjects, grant.RoleRef) {
				continue
			}
			binding.SetAPIVersion(identity.apiVersion)
			binding.SetKind(kind)
			state, err := lifecyclePrivilegeState(&binding)
			l.check(err, "record %s grant at the privilege boundary", kind)
			states[identity] = state
		}
	}
	if len(states) == 0 {
		l.fatalf("runtime privilege comparison has no live release objects")
	}
	return states
}

func (l *lifecycleRun) readRuntimePrivilege(identity lifecyclePrivilegeObject) []byte {
	l.t.Helper()
	object := &unstructured.Unstructured{}
	object.SetAPIVersion(identity.apiVersion)
	object.SetKind(identity.kind)
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: identity.namespace, Name: identity.name}, object),
		"read runtime privilege %s %s/%s", identity.kind, identity.namespace, identity.name)
	state, err := lifecyclePrivilegeState(object)
	l.check(err, "record runtime privilege %s %s/%s", identity.kind, identity.namespace, identity.name)
	return state
}

func (l *lifecycleRun) assertRuntimePrivilegesUnchanged(before map[lifecyclePrivilegeObject][]byte, boundary string) {
	l.t.Helper()
	if len(before) == 0 {
		l.fatalf("%s has no pre-transition privilege evidence", boundary)
	}
	after := l.snapshotRuntimePrivileges()
	if len(after) != len(before) {
		l.fatalf("%s changed the runtime privilege inventory from %d to %d objects", boundary, len(before), len(after))
	}
	for identity, state := range before {
		if !bytes.Equal(state, after[identity]) {
			l.fatalf("%s changed runtime privilege %s %s/%s", boundary, identity.kind, identity.namespace, identity.name)
		}
		l.logf("unchanged privilege: boundary=%q kind=%s namespace=%s name=%s stateSHA256=%x",
			boundary, identity.kind, identity.namespace, identity.name, sha256.Sum256(state))
	}
	l.logf("%s: %d live runtime privilege identities and authority fields unchanged", boundary, len(before))
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
