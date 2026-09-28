package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The acceptance phases used to compute the realm digest themselves, in jq,
// to hold a plan, an approval or a Lease to the realm the operator derives.
// Each copy was a second definition, and one of them was not even a copy:
// hack/e2e-assert.sh carried the digest as a literal. When the namespace joined
// the derivation (#483) the literal kept the old value, the approval webhook
// refused the plan it bound as naming a target that had changed, and the assert
// phase failed on every minor (run 36267860770).
//
// Every phase that derives a realm is a Go phase now, and test/e2e holds its
// derivation to the operator's. What is left here is the refusal of a digest
// written as a literal in a phase script that remains.

// literalRealmDigest is a realm digest assigned as a literal in shell.
var literalRealmDigest = regexp.MustCompile(`(?im)^\s*[a-z_]*coordination[a-z_]*=["']?sha256:[0-9a-f]{64}`)

func TestNoPhaseScriptWritesARealmDigestAsALiteral(t *testing.T) {
	t.Parallel()
	// The line the assert phase carried, which this check exists to refuse.
	if !literalRealmDigest.MatchString("coordination_digest=sha256:a039cc1dcc29e539b94d48451d5b4b5fa2b2eebc7927f5c4e106679b98479725\n") {
		t.Fatal("the literal check does not refuse the line that broke the assert phase")
	}
	// And a derived one, which it has to let through.
	if literalRealmDigest.MatchString(`coordination_digest=$(derive_coordination_digest postgresql "$TEST_NAMESPACE" "$KEY")` + "\n") {
		t.Fatal("the literal check refuses a derived digest")
	}

	scripts, err := filepath.Glob("e2e-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, script := range scripts {
		// A self-test feeds its subject fixtures, and a fixture digest there
		// is compared with nothing the operator derives.
		if strings.Contains(script, "selftest") {
			continue
		}
		contents, err := os.ReadFile(script) //nolint:gosec // A path globbed in this directory.
		if err != nil {
			t.Fatal(err)
		}
		checked++
		if match := literalRealmDigest.Find(contents); match != nil {
			t.Errorf("%s writes a realm digest as a literal, %q; derive it as the operator does", script, strings.TrimSpace(string(match)))
		}
	}
	if checked == 0 {
		t.Fatal("found no phase script to check, so the check measured nothing")
	}
}
