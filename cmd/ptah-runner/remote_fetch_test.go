package main

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// The Git and the HTTPS fetch modes take the directory and the Ptah binary,
// refuse every other flag and each other, and reach the fetch of their own
// kind: an HTTPS fetch with no HTTPS source configured is refused by the
// HTTPS fetch, which names its code.
func TestRunRefusesARemoteFetchItWasNotBuiltFor(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name        string
		arguments   func(binary, destination string) []string
		environment []string
		refusal     string
	}{
		{
			name: "an HTTPS fetch beside another mode's flag",
			arguments: func(binary, destination string) []string {
				return []string{"--fetch-https-source", destination, "--ptah-binary", binary, "--operation", "resolve"}
			},
			environment: []string{runnerProtocol},
			refusal:     "HTTPS fetch mode takes only --fetch-https-source and --ptah-binary",
		},
		{
			name: "a Git and an HTTPS fetch together",
			arguments: func(binary, destination string) []string {
				return []string{"--fetch-git-source", destination, "--fetch-https-source", destination, "--ptah-binary", binary}
			},
			environment: []string{runnerProtocol},
			refusal:     "fetch mode takes only",
		},
		{
			name: "an HTTPS fetch with an argument",
			arguments: func(binary, destination string) []string {
				return []string{"--fetch-https-source", destination, "--ptah-binary", binary, "extra"}
			},
			environment: []string{runnerProtocol},
			refusal:     "HTTPS fetch mode takes only",
		},
		{
			name: "an HTTPS fetch for another protocol",
			arguments: func(binary, destination string) []string {
				return []string{"--fetch-https-source", destination, "--ptah-binary", binary}
			},
			environment: []string{runner.EnvRunnerProtocolVersion + "=" + strconv.Itoa(runner.ProtocolVersion+1)},
			refusal:     runner.CodeRunnerProtocolMismatch,
		},
		{
			name: "an HTTPS fetch with no HTTPS source",
			arguments: func(binary, destination string) []string {
				return []string{"--fetch-https-source", destination, "--ptah-binary", binary}
			},
			environment: []string{runnerProtocol},
			refusal:     runner.CodeHTTPSSourceRefused + ": no HTTPS source is configured",
		},
		{
			name: "a Git fetch with no Git source",
			arguments: func(binary, destination string) []string {
				return []string{"--fetch-git-source", destination, "--ptah-binary", binary}
			},
			environment: []string{runnerProtocol},
			refusal:     runner.CodeGitSourceRefused,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			binary, record := fetchScript(t, 0)
			var stdout, stderr bytes.Buffer
			exitCode := run(context.Background(), row.arguments(binary, t.TempDir()+"/source"), &stdout, &stderr, row.environment, "")
			if exitCode != 2 || !strings.Contains(stderr.String(), row.refusal) {
				t.Fatalf("run() = exit %d, stderr %q, want 2 and a refusal naming %q", exitCode, stderr.String(), row.refusal)
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatalf("Ptah ran for a fetch the runner refused (stat error %v)", err)
			}
		})
	}
}
