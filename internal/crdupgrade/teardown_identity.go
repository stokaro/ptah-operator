package crdupgrade

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var teardownIdentityDigestPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

var teardownHookIdentityPattern = regexp.MustCompile(`^(.*)-crd-v([1-9][0-9]*)-([0-9a-f]{12})$`)

// TeardownServiceAccountName derives the name of the uninstall cleanup
// ServiceAccount from the CRD hook identity. No chart creates that account any
// more; the retained guards and the admission inventory marker still carry the
// name, so it is derived here until they go.
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

// TeardownQuiesceJobName returns the bounded name of the pre-delete Job that
// stops the runtime and deletes the retained release inventory.
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
