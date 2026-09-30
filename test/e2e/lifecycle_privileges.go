package e2e

import (
	"errors"
	"fmt"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type lifecyclePrivilegeObject struct {
	apiVersion, kind, namespace, name string
	hook                              bool
}

// Read the release's stored manifests, not the live labels an orphan can lose.
// Hook identities remain relevant even when successful hooks already deleted
// them. A recreated object with the same name must disappear too: RBAC names,
// rather than UIDs, determine which privileges a binding grants.
func lifecyclePrivilegeObjects(manifest, hooks []byte, namespace string) ([]lifecyclePrivilegeObject, error) {
	if namespace == "" {
		return nil, errors.New("release privilege inventory has no namespace")
	}
	var objects []lifecyclePrivilegeObject
	seen := map[lifecyclePrivilegeObject]bool{}
	counts := map[string]int{}
	for _, source := range []struct {
		raw  []byte
		hook bool
	}{{manifest, false}, {hooks, true}} {
		documents, err := decodeManifests(source.raw)
		if err != nil {
			return nil, errors.New("release privilege inventory contains invalid manifests")
		}
		for _, document := range documents {
			object := &unstructured.Unstructured{Object: document}
			kind := object.GetKind()
			version := "rbac.authorization.k8s.io/v1"
			switch kind {
			case "ServiceAccount":
				version = "v1"
			case "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding":
			default:
				continue
			}
			if object.GetAPIVersion() != version || object.GetName() == "" || object.GetAnnotations()["helm.sh/resource-policy"] == "keep" {
				return nil, errors.New("release privilege object has an invalid identity or retention policy")
			}
			ns := object.GetNamespace()
			if strings.HasPrefix(kind, "Cluster") {
				if ns != "" {
					return nil, errors.New("cluster privilege object has a namespace")
				}
			} else if ns == "" {
				ns = namespace
			}
			identity := lifecyclePrivilegeObject{apiVersion: version, kind: kind, namespace: ns, name: object.GetName()}
			if seen[identity] {
				return nil, errors.New("release privilege inventory repeats an object")
			}
			seen[identity] = true
			identity.hook = source.hook
			objects = append(objects, identity)
			counts[kind]++
		}
	}
	for _, kind := range []string{"ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding"} {
		if counts[kind] == 0 {
			return nil, fmt.Errorf("release privilege inventory omitted %s", kind)
		}
	}
	return objects, nil
}

// Check all namespaces and all bindings, including unlabeled or renamed ones.
// Global Kubernetes groups such as system:authenticated are not release
// identities. Direct accounts, their User spelling, and namespace-wide grants
// are; retained bindings to a removed release Role also remain unexplained.
func lifecycleBindingUsesRelease(objects []lifecyclePrivilegeObject, namespace string, subjects []rbacv1.Subject, role rbacv1.RoleRef) bool {
	for _, object := range objects {
		if role.APIGroup == rbacv1.GroupName && role.Kind == object.kind && role.Name == object.name &&
			(object.kind == "ClusterRole" || (object.kind == "Role" && object.namespace == namespace)) {
			return true
		}
		if object.kind != "ServiceAccount" {
			continue
		}
		for _, subject := range subjects {
			switch subject.Kind {
			case "ServiceAccount":
				ns := subject.Namespace
				if ns == "" {
					ns = namespace
				}
				if subject.APIGroup == "" && subject.Name == object.name && ns == object.namespace {
					return true
				}
			case "User":
				if subject.APIGroup == rbacv1.GroupName && subject.Name == "system:serviceaccount:"+object.namespace+":"+object.name {
					return true
				}
			case "Group":
				if subject.APIGroup == rbacv1.GroupName && subject.Name == "system:serviceaccounts:"+object.namespace {
					return true
				}
			}
		}
	}
	return false
}
