package runner

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/test/e2e/gitrefusal"
)

// The directories under internal/dataplane/testdata/git-pull are the pulls the
// pinned Ptah release image ran; internal/dataplane/gitpull_test.go names the
// image digest and the command. These tests are in the package because
// gitPullExit and the trust GitPull takes are not exported, and the arguments
// reach a child with paths the runner chose, which the capture's cannot equal.

const (
	capturedHTTPS = "https://e2e-git.ptah-test.svc/acme/app.git"
	capturedSSH   = "ssh://git@e2e-git.ptah-test.svc/acme/app.git"

	capturedFirstCommit   = "ee7f9f4a97d8a24bf8f6bb6724f9e95c3f3b90a8"
	capturedTipCommit     = "7c63b477b7bc3d4eaf899d687d57d88fab63fedc"
	capturedFirstSchema   = "sha256:952d69cc98f2d0e50b972f041f86f3368b0986107ee26025e5817adddebe27ab"
	capturedTipSchema     = "sha256:51fdf83bf045136ef654e793dd439ad572c8ca52250d7c5f824ecf70e216f518"
	capturedMigrationsDir = "sha256:8a7eb2be8871d4eb80e45bf4620c00390da96a1ef1afea0aa9a6b94c87d365bc"

	capturedCAFile     = "/credentials/git-ca/ca.pem"
	capturedKnownHosts = "/credentials/git-known-hosts/known_hosts"
	capturedOtherHosts = "/credentials/git-known-hosts-other/known_hosts"
	// capturedScratch stands for the directory a fetch makes under its TMPDIR.
	capturedScratch = "/fetch-work/ptah-git-fetch-capture"
)

// capturedPull is what the manager and the runner would hand GitPull for one
// capture, and the refusal the capture's standard error has to give. A capture
// with no refusal printed a report.
type capturedPull struct {
	name    string
	source  GitSource
	trust   gitTrust
	refusal string
}

func capturedSchema(url string, at GitSource) GitSource {
	if at.Path == "" {
		at.Path = "db/schema.sql"
	}
	at.URL, at.Schema, at.selects = url, true, gitSelectsFile
	return at
}

func capturedMigrations(url string, at GitSource) GitSource {
	at.URL, at.Path, at.selects = url, "db/migrations", gitSelectsDirectory
	return at
}

func capturedPulls() []capturedPull {
	overHTTPS := gitTrust{caFile: capturedCAFile}
	overSSH := gitTrust{knownHostsFile: capturedKnownHosts}
	branch := GitSource{Branch: "main"}
	tag := GitSource{Tag: "v1"}
	schemaAtTip := GitSource{Commit: capturedTipCommit, ExpectedContentDigest: capturedTipSchema}
	migrationsAtTip := GitSource{Commit: capturedTipCommit, ExpectedContentDigest: capturedMigrationsDir}
	return []capturedPull{
		{name: "schema-https-branch", source: capturedSchema(capturedHTTPS, branch), trust: overHTTPS},
		{name: "schema-https-tag", source: capturedSchema(capturedHTTPS, tag), trust: overHTTPS},
		{name: "schema-https-commit", source: capturedSchema(capturedHTTPS, schemaAtTip), trust: overHTTPS},
		{
			name:   "schema-https-branch-commit",
			source: capturedSchema(capturedHTTPS, GitSource{Branch: "main", Commit: capturedTipCommit, ExpectedContentDigest: capturedTipSchema}),
			trust:  overHTTPS,
		},
		{name: "schema-ssh-branch", source: capturedSchema(capturedSSH, branch), trust: overSSH},
		{name: "schema-ssh-tag", source: capturedSchema(capturedSSH, tag), trust: overSSH},
		{name: "schema-ssh-commit", source: capturedSchema(capturedSSH, schemaAtTip), trust: overSSH},
		{name: "migrations-https-branch", source: capturedMigrations(capturedHTTPS, branch), trust: overHTTPS},
		{name: "migrations-https-tag", source: capturedMigrations(capturedHTTPS, tag), trust: overHTTPS},
		{name: "migrations-https-commit", source: capturedMigrations(capturedHTTPS, migrationsAtTip), trust: overHTTPS},
		{name: "migrations-ssh-branch", source: capturedMigrations(capturedSSH, branch), trust: overSSH},
		{name: "migrations-ssh-tag", source: capturedMigrations(capturedSSH, tag), trust: overSSH},
		{name: "migrations-ssh-commit", source: capturedMigrations(capturedSSH, migrationsAtTip), trust: overSSH},
		{
			name:   "migrations-ssh-tag-commit",
			source: capturedMigrations(capturedSSH, GitSource{Tag: "v1", Commit: capturedFirstCommit, ExpectedContentDigest: capturedMigrationsDir}),
			trust:  overSSH,
		},
		{
			name:    "refused-submodule",
			source:  capturedSchema(capturedHTTPS, GitSource{Branch: "main", Path: "db/vendor"}),
			trust:   overHTTPS,
			refusal: gitrefusal.Submodule + "db/vendor",
		},
		{
			name:    "refused-lfs-pointer",
			source:  capturedSchema(capturedHTTPS, GitSource{Branch: "main", Path: "db/lfs.sql"}),
			trust:   overHTTPS,
			refusal: gitrefusal.LFSPointer + "db/lfs.sql",
		},
		{
			name:    "refused-host-key",
			source:  capturedSchema(capturedSSH, branch),
			trust:   gitTrust{knownHostsFile: capturedOtherHosts},
			refusal: gitrefusal.HostKeyMismatch,
		},
		{
			name:    "refused-digest",
			source:  capturedSchema(capturedHTTPS, GitSource{Branch: "main", Commit: capturedTipCommit, ExpectedContentDigest: capturedFirstSchema}),
			trust:   overHTTPS,
			refusal: gitrefusal.DigestDiffers + "db/schema.sql",
		},
		{
			name:    "refused-commit-pinned",
			source:  capturedSchema(capturedHTTPS, GitSource{Commit: capturedTipCommit}),
			trust:   overHTTPS,
			refusal: gitrefusal.CommitNotNamed + capturedTipCommit,
		},
		{
			name:    "refused-commit-force-pushed",
			source:  capturedSchema(capturedHTTPS, GitSource{Branch: "main", Commit: capturedTipCommit, ExpectedContentDigest: capturedTipSchema}),
			trust:   overHTTPS,
			refusal: gitrefusal.CommitNotAvailable + capturedTipCommit,
		},
	}
}

func capturedFile(t *testing.T, name, file string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "dataplane", "testdata", "git-pull", name, file))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func capturedExit(t *testing.T, name string) int {
	t.Helper()
	code, err := strconv.Atoi(strings.TrimSpace(string(capturedFile(t, name, "exit-code"))))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return code
}

// The table is every capture under the directory, and a capture holds a
// refusal exactly when it exited otherwise than 0. A capture with no row would
// be a document no test here reads.
func TestCapturedPullsAreEveryCapture(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(filepath.Join("..", "dataplane", "testdata", "git-pull"))
	if err != nil {
		t.Fatal(err)
	}
	pulls := capturedPulls()
	if len(entries) != len(pulls) {
		t.Errorf("%d captures, and %d rows", len(entries), len(pulls))
	}
	for _, entry := range entries {
		if !slices.ContainsFunc(pulls, func(pull capturedPull) bool { return pull.name == entry.Name() }) {
			t.Errorf("the capture %s has no row", entry.Name())
		}
	}
	for _, pull := range pulls {
		if exit := capturedExit(t, pull.name); (exit != 0) != (pull.refusal != "") {
			t.Errorf("%s exited %d, and its row expects the refusal %q", pull.name, exit, pull.refusal)
		}
	}
}

// What GitPull builds is what the capture ran, so every flag it passes exists
// in the pinned image and means what the captured document shows.
func TestGitPullBuildsTheArgumentsTheCaptureRan(t *testing.T) {
	t.Parallel()
	for _, pull := range capturedPulls() {
		t.Run(pull.name, func(t *testing.T) {
			t.Parallel()
			// As the fetch and the Resolve choose it.
			out := path.Join(capturedScratch, "migrations")
			if pull.source.Schema {
				out = path.Join(capturedScratch, path.Base(pull.source.Path))
			}
			spec, err := GitPull("/usr/local/bin/ptah", pull.source, out, pull.trust)
			if err != nil {
				t.Fatal(err)
			}
			ran := strings.Split(strings.TrimSpace(string(capturedFile(t, pull.name, "args"))), "\n")
			if !slices.Equal(spec.Args, ran) {
				t.Fatalf("GitPull built\n%q\nand the capture ran\n%q", spec.Args, ran)
			}
		})
	}
}

// namesRefusal is what a git-sources row asks of a reason: Ptah's exit status,
// and the words the row matches in the condition message.
func namesRefusal(reason string, exit int, refusal string) error {
	if prefix := fmt.Sprintf("ptah exited with code %d: ", exit); !strings.HasPrefix(reason, prefix) {
		return fmt.Errorf("the reason %q does not start %q", reason, prefix)
	}
	if !strings.Contains(reason, refusal) {
		return fmt.Errorf("the reason %q does not name %q", reason, refusal)
	}
	return nil
}

// Each captured standard error gives the refusal the row expects, in the reason
// gitPullExit keeps and in the message of the Resolve result that reaches the
// condition. The expected words are the ones test/e2e/gitsource_e2e_test.go
// matches, so a Ptah release that rewords one fails here first.
func TestGitPullExitNamesEachCapturedRefusal(t *testing.T) {
	t.Parallel()
	examined := 0
	for _, pull := range capturedPulls() {
		if pull.refusal == "" {
			continue
		}
		examined++
		t.Run(pull.name, func(t *testing.T) {
			t.Parallel()
			exit, stderr := capturedExit(t, pull.name), capturedFile(t, pull.name, "stderr")
			redactor := NewRedactor([]string{EnvGitPassword + "=a-token-the-capture-never-printed"})
			if err := namesRefusal(gitPullExit(exit, stderr, redactor).Error(), exit, pull.refusal); err != nil {
				t.Fatal(err)
			}
			result := Run(t.Context(), Config{
				Operation:   OperationResolve,
				Environment: gitResolveEnvironment(),
				Executor:    &scriptedExecutor{t: t, responses: []scriptedResponse{{exitCode: exit, stderr: string(stderr)}}},
				TempDir:     t.TempDir(),
			})
			if result.Error == nil || result.Error.Code != "child_exit" {
				t.Fatalf("Run() error = %+v, want child_exit", result.Error)
			}
			if err := namesRefusal(result.Error.Message, exit, pull.refusal); err != nil {
				t.Fatal(err)
			}
		})
	}
	if examined != 6 {
		t.Fatalf("examined %d refusals, want the six captured", examined)
	}
}

// The check above refuses a release that rewords a refusal, and one that writes
// anything after it: gitPullExit keeps only the last line, so a refusal that
// stops being last is lost to the row as surely as a reworded one.
func TestNamesRefusalRefusesWhatARowWouldNotMatch(t *testing.T) {
	t.Parallel()
	for _, pull := range capturedPulls() {
		if pull.refusal == "" {
			continue
		}
		exit, stderr := capturedExit(t, pull.name), string(capturedFile(t, pull.name, "stderr"))
		for name, mutated := range map[string]string{
			"reworded":   strings.ReplaceAll(stderr, pull.refusal, "the source is refused"),
			"not last":   stderr + "See the documentation.\n",
			"no refusal": "",
		} {
			t.Run(pull.name+"/"+name, func(t *testing.T) {
				t.Parallel()
				if mutated == stderr {
					t.Fatal("the mutation changed nothing")
				}
				if err := namesRefusal(gitPullExit(exit, []byte(mutated), NewRedactor(nil)).Error(), exit, pull.refusal); err == nil {
					t.Fatalf("accepted standard error %q", mutated)
				}
			})
		}
		t.Run(pull.name+"/another exit status", func(t *testing.T) {
			t.Parallel()
			if err := namesRefusal(gitPullExit(exit+1, []byte(stderr), NewRedactor(nil)).Error(), exit, pull.refusal); err == nil {
				t.Fatal("accepted another exit status")
			}
		})
	}
}
