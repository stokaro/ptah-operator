package crdupgrade

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

// The controller RBAC transition reads the complete binding inventory in pages
// of this size.
const privilegeBindingPageSize = 256

// privilegeProtectedSubjects names the release identities a foreign binding
// must not grant anything to.
type privilegeProtectedSubjects struct {
	serviceAccounts map[string]struct{}
	users           map[string]struct{}
	namespaceGroup  string
}

func privilegeBindingTouchesProtected(subjects []rbacv1.Subject, protected privilegeProtectedSubjects) bool {
	for _, subject := range subjects {
		switch subject.Kind {
		case rbacv1.ServiceAccountKind:
			if _, found := protected.serviceAccounts[privilegeServiceAccountKey(subject.Namespace, subject.Name)]; found {
				return true
			}
		case rbacv1.UserKind:
			if _, found := protected.users[subject.Name]; found {
				return true
			}
		case rbacv1.GroupKind:
			// A namespace-specific ServiceAccount group is a release-local
			// privilege grant and must be absent. Cluster-wide ambient groups
			// such as system:authenticated and system:serviceaccounts are not
			// release-owned identities and remain outside this exact teardown.
			if subject.Name == protected.namespaceGroup {
				return true
			}
		}
	}
	return false
}

func privilegeServiceAccountKey(namespace, name string) string {
	return namespace + "\x00" + name
}

func privilegePolicyRule(apiGroups, resources, resourceNames, verbs []string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{
		APIGroups:     apiGroups,
		Resources:     resources,
		ResourceNames: resourceNames,
		Verbs:         verbs,
	}
}

func privilegeBindingKey(namespace, name string) string {
	return namespace + "\x00" + name
}

func paginatePrivilegeBindings(
	ctx context.Context,
	description string,
	readPage func(context.Context, metav1.ListOptions) (metav1.ListMeta, int, error),
) error {
	continueToken := ""
	seenTokens := map[string]struct{}{}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("list %s: %w", description, err)
		}
		metadata, count, err := readPage(ctx, metav1.ListOptions{
			Limit:    privilegeBindingPageSize,
			Continue: continueToken,
		})
		if err != nil {
			return fmt.Errorf("list %s: %w", description, err)
		}
		if count > privilegeBindingPageSize {
			return fmt.Errorf("list %s returned an oversized page with %d objects", description, count)
		}
		next := metadata.Continue
		if next == "" {
			if metadata.RemainingItemCount != nil && *metadata.RemainingItemCount > 0 {
				return fmt.Errorf("list %s ended with %d unreturned objects", description, *metadata.RemainingItemCount)
			}
			return nil
		}
		if count == 0 {
			return fmt.Errorf("list %s returned an empty page with a continuation token", description)
		}
		if next == continueToken {
			return fmt.Errorf("list %s repeated its current continuation token", description)
		}
		if _, found := seenTokens[next]; found {
			return fmt.Errorf("list %s repeated a previous continuation token", description)
		}
		seenTokens[next] = struct{}{}
		continueToken = next
	}
}

// CertificateDiscoveryRoleName returns the cross-namespace Role and
// RoleBinding name used to discover the default Kubernetes Service API
// endpoints. Its release identity digest prevents two equal release names in
// different namespaces from sharing authority in the default namespace.
func CertificateDiscoveryRoleName(releaseNamespace, releaseName string) (string, error) {
	if problems := utilvalidation.IsDNS1123Label(releaseNamespace); len(problems) > 0 {
		return "", fmt.Errorf("release namespace is not a DNS label: %s", strings.Join(problems, "; "))
	}
	if problems := utilvalidation.IsDNS1123Subdomain(releaseName); len(problems) > 0 {
		return "", fmt.Errorf("release name is not a DNS subdomain: %s", strings.Join(problems, "; "))
	}
	digest := sha256.Sum256([]byte(releaseNamespace + "\n" + releaseName))
	return fmt.Sprintf("ptah-cert-discovery-v1-%x", digest[:20]), nil
}
