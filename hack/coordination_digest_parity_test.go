package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

// The acceptance phases compute the realm digest themselves, in jq, to hold a
// plan, an approval or a Lease to the realm the operator derives. Each copy is
// a second definition, and one of them was not even a copy: hack/e2e-assert.sh
// carried the digest as a literal. When the namespace joined the derivation
// (#483) the literal kept the old value, the approval webhook refused the plan
// it bound as naming a target that had changed, and the assert phase failed on
// every minor (run 36267860770).
//
// So each shell derivation is run here and compared with the operator's, and
// a digest written as a literal in a phase script is refused outright.

// shellDerivation is one phase script's realm digest: the helper that hashes
// stdin, the function that builds the canonical document, and what it takes.
type shellDerivation struct {
	script   string
	hash     string
	function string
	realm    bool
}

var shellDerivations = []shellDerivation{
	{script: "e2e-assert.sh", hash: "sha256_stdin", function: "derive_coordination_digest"},
	{script: "e2e-dataplane.sh", hash: "sha256", function: "coordination_digest"},
	{script: "e2e-reference-data.sh", hash: "sha256", function: "coordination_digest"},
	{script: "e2e-migrations.sh", hash: "sha256", function: "realm_digest", realm: true},
}

func TestEveryShellRealmDigestIsTheOperators(t *testing.T) {
	t.Parallel()
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal(err)
	}
	for _, derivation := range shellDerivations {
		t.Run(derivation.script, func(t *testing.T) {
			t.Parallel()
			source := readE2ESource(t, derivation.script)
			functions := extractE2EShellFunction(t, source, derivation.hash) + "\n" +
				extractE2EShellFunction(t, source, derivation.function) + "\n"

			for _, sample := range []struct {
				engine, canonical, namespace, name string
			}{
				{engine: "PostgreSQL", canonical: "postgresql", namespace: "ptah-test-a-1234", name: "e2e/admission/postgresql"},
				{engine: "MySQL", canonical: "mysql", namespace: "team-b", name: "e2e/migrations/mysql"},
			} {
				var want string
				var arguments []string
				if derivation.realm {
					// A realm is named by a DNS subdomain, not by a key.
					name := strings.ReplaceAll(sample.name, "/", "-")
					want, err = fingerprint.DatabaseRealmDigest(sample.engine, name)
					arguments = []string{sample.canonical, name}
				} else {
					want, err = fingerprint.DatabaseCoordinationDigest(sample.engine, sample.namespace, sample.name)
					arguments = []string{sample.canonical, sample.namespace, sample.name}
				}
				if err != nil {
					t.Fatal(err)
				}
				script := functions + derivation.function + ` "$@"`
				output, err := exec.Command(shPath, append([]string{"-c", script, "derivation"}, arguments...)...).CombinedOutput() //nolint:gosec // The script is read from this repository.
				if err != nil {
					t.Fatalf("%s %v: %v\n%s", derivation.function, arguments, err, output)
				}
				if got := strings.TrimSpace(string(output)); got != want {
					t.Fatalf("%s's %s %v = %s, the operator derives %s", derivation.script, derivation.function, arguments, got, want)
				}
			}
		})
	}
}

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
	if checked < len(shellDerivations) {
		t.Fatalf("checked %d phase scripts, fewer than the %d that derive a realm", checked, len(shellDerivations))
	}
}
