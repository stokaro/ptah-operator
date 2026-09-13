package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestNormalizerKeepsColumnsWhereTheCommandPutThem(t *testing.T) {
	t.Parallel()
	// The name a bootstrap generated, and the name the chart derived from it
	// by appending a suffix and truncating to a label's 63 characters.
	generated := "ptah-runtime-generated-name-prefix-boundary-proof-206dc31ea3"
	derived := "ptah-runtime-generated-name-prefix-boun-cert-rotator"
	rewriter := normalizer{rules: []rewrite{{
		pattern:     regexp.MustCompile(prefixPattern(generated, 24) + `((?:-[a-z0-9]+)*)`),
		replacement: "ptah-operator$1",
	}}}

	table := strings.Join([]string{
		"NAME                                                           READY",
		derived + "           1/1",
		generated + "   2/2",
	}, "\n")
	got := strings.Split(rewriter.apply(table), "\n")

	if !strings.Contains(got[1], "ptah-operator-cert-rotator") {
		t.Fatalf("the derived name became %q", got[1])
	}
	if !strings.HasPrefix(got[2], "ptah-operator ") {
		t.Fatalf("the generated name became %q", got[2])
	}
	for index, line := range got {
		if column := strings.Index(line, "READY"); index == 0 && column < 0 {
			t.Fatal("the header lost its column")
		}
	}
	// The column the header sets is where both rows still put their value.
	column := strings.Index(got[0], "READY")
	for _, line := range got[1:] {
		if !strings.HasPrefix(line[column:], "1/1") && !strings.HasPrefix(line[column:], "2/2") {
			t.Fatalf("a row moved out of its column: %q", line[column:])
		}
	}
}

func TestNormalizerLeavesAlignmentAloneInProse(t *testing.T) {
	t.Parallel()
	rewriter := normalizer{rules: []rewrite{{
		pattern:     regexp.MustCompile(`ptah-test-a-1-37-0-run`),
		replacement: "demo",
	}}}
	got := rewriter.apply(`namespace "ptah-test-a-1-37-0-run" is where it runs`)
	want := `namespace "demo" is where it runs`
	if got != want {
		t.Fatalf("apply wrote %q, expected %q", got, want)
	}
}

func TestNormalizerRewritesTheIdentifiersThatChangeEveryRun(t *testing.T) {
	t.Parallel()
	rewriter := normalizer{rules: technicalIdentifiers}
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "a uid", text: "uid: 1a2b3c4d-5e6f-4a1b-8c2d-3e4f5a6b7c8d", want: "uid: <uid>"},
		{name: "a timestamp", text: "approvedAt: 2026-09-12T15:07:18Z", want: "approvedAt: <time>"},
		{
			name: "a digest, which means something and is left alone",
			text: "artifactDigest: sha256:a949efd52e641e89a0c5c1ad828650fb2d70ca355c857fe92ddfd8dfdacd7546",
			want: "artifactDigest: sha256:a949efd52e641e89a0c5c1ad828650fb2d70ca355c857fe92ddfd8dfdacd7546",
		},
		{
			name: "a reason, which is the whole claim and is left alone",
			text: "InSync True ScopedConverged",
			want: "InSync True ScopedConverged",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := rewriter.apply(test.text); got != test.want {
				t.Fatalf("apply wrote %q, expected %q", got, test.want)
			}
		})
	}
}

// A recording is committed and published. This is the refusal that keeps a
// credential the scenario asked for out of it.
func TestNormalizerAuditRefusesACredential(t *testing.T) {
	t.Parallel()
	rewriter := normalizer{secrets: []string{"e2eRegistryQ7", "postgres://user:pw@host/db"}}
	if err := rewriter.audit("a step", "username: ptah_e2e"); err != nil {
		t.Fatalf("audit refused text carrying no credential: %v", err)
	}
	err := rewriter.audit("scenario x step 3", "PTAH_OCI_PASSWORD=e2eRegistryQ7")
	if err == nil {
		t.Fatal("audit accepted a transcript carrying the registry password")
	}
	if !strings.Contains(err.Error(), "scenario x step 3") {
		t.Fatalf("audit did not name the step: %v", err)
	}
	if strings.Contains(err.Error(), "e2eRegistryQ7") {
		t.Fatal("audit put the credential in its own message")
	}
}

func TestPrefixPatternPrefersTheLongestName(t *testing.T) {
	t.Parallel()
	name := "ptah-runtime-generated-name-prefix-boundary-proof-206dc31ea3"
	pattern := regexp.MustCompile(prefixPattern(name, 24))
	if got := pattern.FindString(name); got != name {
		t.Fatalf("the pattern matched %q of the full name", got)
	}
	truncated := name[:39]
	if got := pattern.FindString(truncated + "-cert-rotator"); got != truncated {
		t.Fatalf("the pattern matched %q of the truncated name", got)
	}
	if pattern.MatchString("ptah-runtime-generated-x") {
		t.Fatal("the pattern matched a name shorter than its floor")
	}
}
