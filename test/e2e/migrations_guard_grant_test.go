package e2e

import (
	"os"
	"strings"
	"testing"
)

func TestGuardAuthorGrantPendingReadsActualRefusal(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("../../testdata/e2e/readings/migration-author-rbac-denial.txt")
	if err != nil {
		t.Fatal(err)
	}
	user := "e2e-author-mysql"
	role := "e2e-desired-state-author-mysql"
	namespace := "ptah-test-a-1-36-4-ci-36591988252-1-1-36-migrations-aeab874906"
	actual := string(body)
	if !guardAuthorGrantPending(actual, user, namespace, role) {
		t.Fatal("the API server's RBAC refusal was not recognized")
	}
	// Typed clients expose the same Status message without kubectl's prefix.
	start := strings.Index(actual, "ptahmigrations.operator.ptah.run is forbidden:")
	if start < 0 || !guardAuthorGrantPending(actual[start:], user, namespace, role) {
		t.Fatal("the authorizer's Status message was not recognized")
	}
	for name, message := range unrelatedRefusals(actual, user, namespace) {
		t.Run(name, func(t *testing.T) {
			if guardAuthorGrantPending(message, user, namespace, role) {
				t.Fatal("an unrelated failure was classified for retry")
			}
		})
	}
}

// The authorizer saw the author's RoleBinding before the Role it names.
func TestGuardAuthorGrantPendingReadsARoleNotYetSeen(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("../../testdata/e2e/readings/migration-author-rbac-role-pending.txt")
	if err != nil {
		t.Fatal(err)
	}
	user := "e2e-author-postgresql"
	role := "e2e-desired-state-author-postgresql"
	namespace := "ptah-test-a-1-36-4-ci-44999887648-1-1-36-migrations-922055a49e"
	actual := string(body)
	if !guardAuthorGrantPending(actual, user, namespace, role) {
		t.Fatal("the API server's refusal naming the author's Role as not found was not recognized")
	}
	rows := unrelatedRefusals(actual, user, namespace)
	rows["another Role not found"] = strings.ReplaceAll(actual, `"`+role+`" not found`, `"another-role" not found`)
	rows["a ClusterRole not found"] = strings.ReplaceAll(actual, "role.rbac.authorization.k8s.io", "clusterrole.rbac.authorization.k8s.io")
	rows["another RBAC error"] = strings.ReplaceAll(actual, " not found", " is being deleted")
	rows["more than the Role"] = strings.TrimSpace(actual) + `, role.rbac.authorization.k8s.io "another-role" not found`
	for name, message := range rows {
		t.Run(name, func(t *testing.T) {
			if message == actual {
				t.Fatal("the row does not change the refusal")
			}
			if guardAuthorGrantPending(message, user, namespace, role) {
				t.Fatal("an unrelated failure was classified for retry")
			}
		})
	}
}

// unrelatedRefusals are the failures the first author CREATE must not
// retry: the refusal of another write, and failures that are not RBAC's.
func unrelatedRefusals(actual, user, namespace string) map[string]string {
	return map[string]string{
		"other author":    strings.ReplaceAll(actual, user, "another-author"),
		"other namespace": strings.ReplaceAll(actual, namespace, "another-namespace"),
		"other verb":      strings.ReplaceAll(actual, "cannot create resource", "cannot patch resource"),
		"other resource":  strings.ReplaceAll(actual, "ptahmigrations", "ptahmigrationapprovals"),
		"other group":     strings.ReplaceAll(actual, "operator.ptah.run", "other.example"),
		"admission":       `ptahmigrations.operator.ptah.run is forbidden: ValidatingAdmissionPolicy 'apply-policy-guard' denied request: reserves that choice`,
		"webhook":         `admission webhook "vmigration.operator.ptah.run" denied the request: invalid policy`,
		"already exists":  `ptahmigrations.operator.ptah.run "e2e-apply-guard-mysql" already exists`,
		"transport":       "context deadline exceeded",
		"empty":           "",
	}
}
