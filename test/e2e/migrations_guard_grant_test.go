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
	namespace := "ptah-test-a-1-36-4-ci-36591988252-1-1-36-migrations-aeab874906"
	actual := string(body)
	if !guardAuthorGrantPending(actual, user, namespace) {
		t.Fatal("the API server's RBAC refusal was not recognized")
	}
	// Typed clients expose the same Status message without kubectl's prefix.
	start := strings.Index(actual, "ptahmigrations.operator.ptah.run is forbidden:")
	if start < 0 || !guardAuthorGrantPending(actual[start:], user, namespace) {
		t.Fatal("the authorizer's Status message was not recognized")
	}
	for name, message := range map[string]string{
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
	} {
		t.Run(name, func(t *testing.T) {
			if guardAuthorGrantPending(message, user, namespace) {
				t.Fatal("an unrelated failure was classified for retry")
			}
		})
	}
}
