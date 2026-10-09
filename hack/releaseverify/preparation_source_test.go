package main

import (
	"os"
	"strings"
	"testing"
)

// Exercise both complete state formats through their ordinary verifiers.
// Choosing another producer must never change the required source commit.
func checkReleaseSourceBindings(t *testing.T, path, tag, sha string, verify func(string, string) error) {
	t.Helper()
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tagRef := "refs/tags/" + tag
	old := "source-ref=" + tagRef + "\n"
	if strings.Count(string(valid), old) != 1 {
		t.Fatal("state fixture must contain exactly one producer ref")
	}
	for _, row := range []struct {
		name, declaredRef, expectedRef, sourceSHA string
		wantFailure                               bool
	}{
		{"tag remains the default", tagRef, "", sha, false},
		{"explicit prepared source", preparationSourceRef, preparationSourceRef, sha, false},
		{"prepared source is not implicit", preparationSourceRef, "", sha, true},
		{"prepared source still needs the exact commit", preparationSourceRef, preparationSourceRef, strings.Repeat("f", 40), true},
		{"tag cannot impersonate branch", tagRef, preparationSourceRef, sha, true},
		{"branch cannot impersonate tag", preparationSourceRef, tagRef, sha, true},
		{"unreviewed branch", "refs/heads/feature", "refs/heads/feature", sha, true},
		{"pull request ref", "refs/pull/600/merge", "refs/pull/600/merge", sha, true},
		{"another release tag", "refs/tags/v99.0.0", "refs/tags/v99.0.0", sha, true},
	} {
		t.Run("source binding/"+row.name, func(t *testing.T) {
			document := strings.Replace(string(valid), old, "source-ref="+row.declaredRef+"\n", 1)
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verify(row.expectedRef, row.sourceSHA); (err != nil) != row.wantFailure {
				t.Fatalf("source verification = %v; want failure %t", err, row.wantFailure)
			}
		})
	}
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatal(err)
	}
}
