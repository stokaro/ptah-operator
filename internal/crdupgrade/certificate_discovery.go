package crdupgrade

import (
	"crypto/sha256"
	"fmt"
	"strings"

	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

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
