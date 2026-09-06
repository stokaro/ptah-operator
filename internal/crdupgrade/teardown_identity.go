package crdupgrade

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var teardownIdentityDigestPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

var teardownHookIdentityPattern = regexp.MustCompile(`^(.*)-crd-v([1-9][0-9]*)-([0-9a-f]{12})$`)

const teardownRoleBaseMaxLength = 24

// TeardownServiceAccountName derives the candidate-specific, cleanup-only
// identity from the privileged CRD hook identity. Keeping a different name
// prevents uninstall cleanup from inheriting schema-mutation permissions.
func TeardownServiceAccountName(hookServiceAccountName string, releaseSequence int32) (string, error) {
	if releaseSequence < 1 {
		return "", fmt.Errorf("teardown release sequence must be positive")
	}
	marker := "-crd-v" + strconv.FormatInt(int64(releaseSequence), 10) + "-"
	index := strings.LastIndex(hookServiceAccountName, marker)
	if index <= 0 {
		return "", fmt.Errorf("hook ServiceAccount does not encode the teardown release sequence")
	}
	digest := hookServiceAccountName[index+len(marker):]
	if !teardownIdentityDigestPattern.MatchString(digest) {
		return "", fmt.Errorf("hook ServiceAccount does not encode a 12-character release digest")
	}
	name := hookServiceAccountName[:index] + "-cleanup-v" + strconv.FormatInt(int64(releaseSequence), 10) + "-" + digest
	if len(name) > 63 {
		return "", fmt.Errorf("teardown ServiceAccount name exceeds 63 characters")
	}
	return name, nil
}

// TeardownQuiesceJobName returns the bounded Job name used by the read/update
// phase that runs before the cleanup-only identity is allowed to delete guards.
func TeardownQuiesceJobName(hookServiceAccountName string) (string, error) {
	if hookServiceAccountName == "" || hookServiceAccountName != strings.TrimSpace(hookServiceAccountName) {
		return "", fmt.Errorf("hook ServiceAccount name is required")
	}
	parts := teardownHookIdentityPattern.FindStringSubmatch(hookServiceAccountName)
	if len(parts) != 4 || parts[1] == "" {
		return "", fmt.Errorf("hook ServiceAccount does not encode a candidate release identity")
	}
	name := parts[1] + "-quiesce-v" + parts[2] + "-" + parts[3]
	if len(name) > 63 {
		return "", fmt.Errorf("teardown quiesce Job name exceeds 63 characters")
	}
	return name, nil
}

// TeardownPrivilegeRoleName returns the candidate-specific temporary
// ClusterRole and ClusterRoleBinding name used to remove normal privileges.
func TeardownPrivilegeRoleName(hookServiceAccountName string) (string, error) {
	return teardownRoleName(hookServiceAccountName, "cleanup-priv")
}

// TeardownGuardRoleName returns the candidate-specific residual ClusterRole
// and ClusterRoleBinding name used after broad cleanup access is self-revoked.
func TeardownGuardRoleName(hookServiceAccountName string) (string, error) {
	return teardownRoleName(hookServiceAccountName, "cleanup-guard")
}

func teardownRoleName(hookServiceAccountName, phase string) (string, error) {
	parts := teardownHookIdentityPattern.FindStringSubmatch(hookServiceAccountName)
	if len(parts) != 4 || parts[1] == "" {
		return "", fmt.Errorf("hook ServiceAccount does not encode a candidate release identity")
	}
	base := parts[1]
	if len(base) > teardownRoleBaseMaxLength {
		base = strings.TrimSuffix(base[:teardownRoleBaseMaxLength], "-")
	}
	if base == "" {
		return "", fmt.Errorf("hook ServiceAccount cannot form a teardown role name")
	}
	name := base + "-" + phase + "-v" + parts[2] + "-" + parts[3]
	if len(name) > 63 {
		return "", fmt.Errorf("teardown role name exceeds 63 characters")
	}
	return name, nil
}
