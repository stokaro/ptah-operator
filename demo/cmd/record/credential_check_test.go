package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The failure-recovery scenario publishes a safety claim: the status the
// operator wrote carries no database credential. A claim like that is only
// worth what its check reads, and this one used to read the first 400 bytes of
// a serialized status and search those.
//
// The recorder's own scan cannot cover the rest, because it inspects what a
// command printed and the command had already discarded it. So the check is
// the whole proof, and this is what measures the check.
const (
	plantedCredential = "postgres://nobody:nobody@no-such-database.demo.svc.cluster.local:5432/storefront"
	credentialStepID  = "no credential in it"
)

func TestTheCredentialCheckReadsPastTheBytesItPrints(t *testing.T) {
	t.Parallel()

	script := credentialCheckScript(t)

	for _, row := range []struct {
		name    string
		status  string
		refused bool
	}{
		{
			// What the operator actually writes, padded past the excerpt so
			// the clean row is not passing for being short.
			name:   "a status with no credential in it",
			status: statusDocument(t, "", 900),
		},
		{
			// The reproduction the review described: the credential sits
			// behind an evidence field long enough to push it out of any
			// prefix a display would show.
			name:    "a credential beyond the bytes an excerpt would show",
			status:  statusDocument(t, plantedCredential, 900),
			refused: true,
		},
		{
			name:    "a credential an excerpt would have caught",
			status:  statusDocument(t, plantedCredential, 0),
			refused: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			stdout, stderr, err := runCredentialCheck(t, script, row.status)
			switch {
			case row.refused && err == nil:
				t.Fatalf("the check accepted a status carrying the credential:\n%s", stdout)
			case !row.refused && err != nil:
				t.Fatalf("the check refused a clean status: %v\n%s\n%s", err, stdout, stderr)
			}
			if !row.refused {
				for _, want := range []string{"checked the whole status", credentialStepID, "OperationFailed"} {
					if !strings.Contains(stdout, want) {
						t.Fatalf("the check printed no %q:\n%s", want, stdout)
					}
				}
			}
			// A refusal that quotes the credential publishes it, and this
			// transcript is published.
			if strings.Contains(stdout+stderr, "nobody:nobody") {
				t.Fatalf("the check printed the credential it was refusing:\n%s\n%s", stdout, stderr)
			}
		})
	}
}

// credentialCheckScript is the scenario's own step, read from the file that
// ships it. Copying it here would measure a copy.
func credentialCheckScript(t *testing.T) string {
	t.Helper()

	loaded, err := loadScenarios(filepath.Join("..", "..", "scenarios"))
	if err != nil {
		t.Fatalf("loadScenarios: %v", err)
	}
	for _, one := range loaded {
		if one.ID != "failure-recovery" {
			continue
		}
		for _, step := range one.Steps {
			if strings.Contains(step.Run, credentialStepID) {
				return step.Run
			}
		}
		t.Fatalf("failure-recovery no longer checks for %q, so this measures nothing", credentialStepID)
	}
	t.Fatal("there is no failure-recovery scenario")
	return ""
}

// statusDocument is what `kubectl get ptahschema -o json` returns. The padding
// goes before the message on purpose: it is what pushes a credential in that
// message past the bytes a display would show.
func statusDocument(t *testing.T, credential string, padding int) string {
	t.Helper()

	message := "the database refused the connection"
	if credential != "" {
		message = "dial " + credential + ": no such host"
	}
	document := `{"status":{"phase":"Failed","evidence":"` + strings.Repeat("e", padding) +
		`","conditions":[{"type":"ReconciliationFailed","status":"True","reason":"OperationFailed",` +
		`"message":"` + message + `"}]}}`
	if credential != "" && padding > 0 {
		offset := strings.Index(document, credential)
		if offset <= 400 {
			t.Fatalf("the fixture puts the credential at byte %d, which an excerpt would have shown anyway", offset)
		}
	}
	return document
}

// runCredentialCheck runs the step against a kubectl that answers with the
// document, and nothing else.
func runCredentialCheck(t *testing.T, script, status string) (stdout, stderr string, err error) {
	t.Helper()

	stubs := t.TempDir()
	stub := "#!/bin/sh\nprintf '%s' " + shellQuote(status) + "\n"
	if writeErr := os.WriteFile(filepath.Join(stubs, "kubectl"), []byte(stub), 0o700); writeErr != nil {
		t.Fatal(writeErr)
	}

	command := exec.Command("/bin/sh", "-c", script)
	command.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"NAMESPACE=demo",
	)
	var out, errs strings.Builder
	command.Stdout = &out
	command.Stderr = &errs
	err = command.Run()
	return out.String(), errs.String(), err
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
