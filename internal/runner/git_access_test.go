package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

const (
	gitTestURL    = "https://git.example.com/acme/app.git"
	gitTestCommit = "3f1c2b0a9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a"
)

func gitTestDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// gitReport is the document `ptah ... pull --json` prints for files.
func gitReport(t *testing.T, path, commit string, files []sourcecontent.File) (string, string) {
	t.Helper()
	digest, err := sourcecontent.Digest(files)
	if err != nil {
		t.Fatal(err)
	}
	type file struct {
		Path   string `json:"path"`
		Size   int    `json:"size"`
		SHA256 string `json:"sha256"`
	}
	listed := make([]file, 0, len(files))
	for _, f := range files {
		sum := sha256.Sum256(f.Data)
		listed = append(listed, file{Path: f.Path, Size: len(f.Data), SHA256: hex.EncodeToString(sum[:])})
	}
	slices.SortFunc(listed, func(a, b file) int { return strings.Compare(a.Path, b.Path) })
	body, err := json.Marshal(map[string]any{
		"source": "git", "reference": "git+" + gitTestURL, "commit": commit, "path": path,
		"content_digest": digest, "files": listed, "written": []string{"/work/out"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body), digest
}

func writeGitTrustSource(t *testing.T, directory, name string, data []byte) string {
	t.Helper()
	file := filepath.Join(directory, name)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestPrepareGitSourceAccessHoldsTheCredentialToItsGrants(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	ca := []byte("-----BEGIN CERTIFICATE-----\ngit\n-----END CERTIFICATE-----\n")
	knownHosts := []byte("git.example.com ssh-ed25519 AAAA\n")
	caFile := writeGitTrustSource(t, directory, "ca.pem", ca)
	knownHostsFile := writeGitTrustSource(t, directory, "known_hosts", knownHosts)
	https := GitSource{URL: gitTestURL, Branch: "main", Path: "db/schema.hcl"}
	ssh := GitSource{URL: "ssh://git@git.example.com:2222/acme/app.git", Tag: "v1", Path: "db/migrations"}
	for _, row := range []struct {
		name        string
		source      GitSource
		environment []string
		refusal     string
	}{
		{name: "anonymous https", source: https},
		{name: "anonymous https with a CA", source: https, environment: []string{EnvGitCASourceFile + "=" + caFile}},
		{name: "granted password", source: https, environment: []string{envGitPassword + "=token", EnvGitHostGrant + "=GIT.EXAMPLE.COM"}},
		{name: "granted password and CA", source: https, environment: []string{
			envGitPassword + "=token", EnvGitHostGrant + "=git.example.com",
			EnvGitCASourceFile + "=" + caFile, EnvGitCASHA256Grant + "=" + gitTestDigest(ca),
		}},
		{name: "granted ssh key", source: ssh, environment: []string{
			envGitSSHKeyFile + "=/credentials/git/ssh", EnvGitHostGrant + "=git.example.com:2222",
			EnvGitKnownHostsSourceFile + "=" + knownHostsFile, EnvGitKnownHostsSHA256Grant + "=" + gitTestDigest(knownHosts),
		}},
		{name: "credential without a grant", source: https, environment: []string{envGitPassword + "=token"},
			refusal: "not granted for the source's host"},
		{name: "credential granted for another host", source: https, environment: []string{envGitUsername + "=bot", EnvGitHostGrant + "=attacker.example"},
			refusal: "not granted for the source's host"},
		{name: "ssh grant without its port", source: ssh, environment: []string{
			envGitSSHKeyFile + "=/credentials/git/ssh", EnvGitHostGrant + "=git.example.com",
			EnvGitKnownHostsSourceFile + "=" + knownHostsFile, EnvGitKnownHostsSHA256Grant + "=" + gitTestDigest(knownHosts),
		}, refusal: "not granted for the source's host"},
		{name: "grant without a credential", source: https, environment: []string{EnvGitHostGrant + "=git.example.com"},
			refusal: "without a credential"},
		{name: "credential with an ungranted CA", source: https, environment: []string{
			envGitPassword + "=token", EnvGitHostGrant + "=git.example.com", EnvGitCASourceFile + "=" + caFile,
		}, refusal: "not the one the credential's owner granted"},
		{name: "credential with another CA", source: https, environment: []string{
			envGitPassword + "=token", EnvGitHostGrant + "=git.example.com",
			EnvGitCASourceFile + "=" + caFile, EnvGitCASHA256Grant + "=" + gitTestDigest([]byte("other")),
		}, refusal: "not the one the credential's owner granted"},
		{name: "CA grant without the CA", source: https, environment: []string{
			envGitPassword + "=token", EnvGitHostGrant + "=git.example.com", EnvGitCASHA256Grant + "=" + gitTestDigest(ca),
		}, refusal: "without the certificate authority"},
		{name: "ssh without known hosts", source: ssh, refusal: "needs known hosts"},
		{name: "unreadable CA", source: https, environment: []string{EnvGitCASourceFile + "=" + filepath.Join(directory, "missing")},
			refusal: "read the Git certificate authority"},
		{name: "credential in the URL", source: GitSource{URL: "https://bot@git.example.com/acme/app.git", Branch: "main", Path: "db"},
			refusal: "carries a credential"},
		{name: "unclean path", source: GitSource{URL: gitTestURL, Branch: "main", Path: "db/../schema.hcl"},
			refusal: "not a clean relative path"},
		{name: "abbreviated commit", source: GitSource{URL: gitTestURL, Commit: "3f1c2b0", Path: "db"},
			refusal: "not a full lowercase id"},
		{name: "branch and tag", source: GitSource{URL: gitTestURL, Branch: "main", Tag: "v1", Path: "db"},
			refusal: "names a branch and a tag"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			trust, cleanup, err := PrepareGitSourceAccess(row.source, row.environment, t.TempDir())
			if row.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), row.refusal) {
					t.Fatalf("PrepareGitSourceAccess() = %v, want a refusal naming %q", err, row.refusal)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			for _, copied := range []string{trust.caFile, trust.knownHostsFile} {
				if copied == "" {
					continue
				}
				if copied == caFile || copied == knownHostsFile {
					t.Fatalf("Ptah would read the mutable projection %s, not a private copy", copied)
				}
				if _, err := os.Stat(copied); err != nil {
					t.Fatalf("the trust copy %s: %v", copied, err)
				}
			}
		})
	}
}

// The copy Ptah reads holds the bytes the grant was checked against, and is
// removed afterwards.
func TestPrepareGitSourceAccessCopiesTheCheckedBytes(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	ca := []byte("granted CA\n")
	caFile := writeGitTrustSource(t, directory, "ca.pem", ca)
	trust, cleanup, err := PrepareGitSourceAccess(GitSource{URL: gitTestURL, Branch: "main", Path: "db"}, []string{
		envGitPassword + "=token", EnvGitHostGrant + "=git.example.com",
		EnvGitCASourceFile + "=" + caFile, EnvGitCASHA256Grant + "=" + gitTestDigest(ca),
	}, directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, []byte("swapped after the check"), 0o600); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(trust.caFile)
	if err != nil || string(copied) != string(ca) {
		t.Fatalf("the copy holds %q, %v; want the checked bytes", copied, err)
	}
	cleanup()
	if _, err := os.Stat(trust.caFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the copy survived its cleanup: %v", err)
	}
}

func TestGitPullNamesEveryCoordinate(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		source GitSource
		trust  gitTrust
		want   []string
	}{
		{
			name:   "schema by branch",
			source: GitSource{URL: gitTestURL, Branch: "main", Path: "db/schema.hcl", Schema: true},
			want:   []string{"schema", "pull", "git+" + gitTestURL, "--path", "db/schema.hcl", "--branch", "main", "--out", "/out/schema.hcl", "--json"},
		},
		{
			name: "migrations at a bound commit",
			source: GitSource{
				URL: "ssh://git@git.example.com/acme/app.git", Tag: "v1", Commit: gitTestCommit, Path: ".",
				ExpectedContentDigest: "sha256:" + strings.Repeat("a", 64),
			},
			trust: gitTrust{knownHostsFile: "/tmp/known"},
			want: []string{
				"migrations", "pull", "git+ssh://git@git.example.com/acme/app.git", "--path", ".", "--tag", "v1",
				"--commit", gitTestCommit, "--expect-digest", "sha256:" + strings.Repeat("a", 64),
				"--known-hosts-file", "/tmp/known", "--out", "/out/schema.hcl", "--json",
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			spec, err := GitPull("/usr/local/bin/ptah", row.source, "/out/schema.hcl", row.trust)
			if err != nil {
				t.Fatal(err)
			}
			if spec.Path != "/usr/local/bin/ptah" || !reflect.DeepEqual(spec.Args, row.want) {
				t.Fatalf("GitPull() = %s %q\nwant %q", spec.Path, spec.Args, row.want)
			}
		})
	}
	if _, err := GitPull("ptah", GitSource{URL: gitTestURL, Branch: "main", Path: "db"}, "relative", gitTrust{}); err == nil {
		t.Fatal("GitPull accepted a relative output path")
	}
}

func gitResolveEnvironment(extra ...string) []string {
	return withRunnerProtocol(append([]string{
		envOperationID + "=resolve-git",
		EnvGitURL + "=" + gitTestURL,
		EnvGitBranch + "=main",
		EnvGitPath + "=db/schema.hcl",
		EnvGitSelects + "=file",
	}, extra...))
}

func TestRunnerResolvesAGitSourceWithoutHandingPtahTheGrants(t *testing.T) {
	t.Parallel()
	files := []sourcecontent.File{{Path: "schema.hcl", Data: []byte("table \"orders\" {}\n")}}
	report, digest := gitReport(t, "db/schema.hcl", gitTestCommit, files)
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: report,
		inspect: func(spec CommandSpec) {
			if spec.Args[0] != "schema" || argumentAfter(spec, "--branch") != "main" || argumentAfter(spec, "--commit") != "" {
				t.Errorf("the pull ran as %q", spec.Args)
			}
			values := environmentMap(spec.Env)
			if values[envGitPassword] != "token" {
				t.Error("Ptah did not receive the credential it authenticates with")
			}
			for _, name := range []string{EnvGitHostGrant, EnvGitURL, EnvGitBranch, EnvGitPath, EnvGitSelects} {
				if _, present := values[name]; present {
					t.Errorf("Ptah received the runner-only %s", name)
				}
			}
		},
	}}}
	result := Run(t.Context(), Config{
		Operation:   OperationResolve,
		Environment: gitResolveEnvironment(envGitPassword+"=token", EnvGitHostGrant+"=git.example.com"),
		Executor:    executor,
		TempDir:     t.TempDir(),
	})
	if result.Error != nil || result.ResolvedCommit != gitTestCommit || result.ResolvedContentDigest != digest || result.ResolvedDigest != "" {
		t.Fatalf("Run() = %#v", result)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("the executor ran %d commands, want the one pull", len(executor.calls))
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := ParseResultFor(frame, OperationResolve, "resolve-git"); err != nil || !reflect.DeepEqual(parsed, result) {
		t.Fatalf("ParseResultFor() = %#v, %v", parsed, err)
	}
}

func TestRunnerRefusesAnUngrantedGitCredentialBeforePtahRuns(t *testing.T) {
	t.Parallel()
	executor := &scriptedExecutor{t: t}
	result := Run(t.Context(), Config{
		Operation:   OperationResolve,
		Environment: gitResolveEnvironment(envGitPassword+"=token", EnvGitHostGrant+"=attacker.example"),
		Executor:    executor,
		TempDir:     t.TempDir(),
	})
	if result.Error == nil || result.Error.Code != "invalid_git_access" || result.ChildExitCode != -1 || len(executor.calls) != 0 {
		t.Fatalf("Run() = %#v after %d calls", result, len(executor.calls))
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseResultFor(frame, OperationResolve, "resolve-git"); err != nil {
		t.Fatalf("the refusal frame does not parse: %v", err)
	}
	if strings.Contains(string(frame), "token") {
		t.Fatal("the frame carries the credential")
	}
}

func TestRunnerRefusesAGitReportThatAnswersAnotherPull(t *testing.T) {
	t.Parallel()
	files := []sourcecontent.File{{Path: "schema.hcl", Data: []byte("table \"orders\" {}\n")}}
	pinned := gitResolveEnvironment(EnvGitCommit + "=" + gitTestCommit)
	other := strings.Repeat("b", 40)
	for _, row := range []struct {
		name        string
		environment []string
		report      func() string
	}{
		{name: "another commit than the pinned one", environment: pinned, report: func() string {
			report, _ := gitReport(t, "db/schema.hcl", other, files)
			return report
		}},
		{name: "another path", environment: gitResolveEnvironment(), report: func() string {
			report, _ := gitReport(t, "db/other.hcl", gitTestCommit, files)
			return report
		}},
		{name: "a digest its files do not give", environment: gitResolveEnvironment(), report: func() string {
			report, digest := gitReport(t, "db/schema.hcl", gitTestCommit, files)
			return strings.Replace(report, digest, "sha256:"+strings.Repeat("c", 64), 1)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			result := Run(t.Context(), Config{
				Operation:   OperationResolve,
				Environment: row.environment,
				Executor:    &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: row.report()}}},
				TempDir:     t.TempDir(),
			})
			if result.Error == nil || result.Error.Code != "invalid_resolve_output" || result.ResolvedCommit != "" {
				t.Fatalf("Run() = %#v", result)
			}
		})
	}
}

func TestRunnerRefusesAGitSourceOutsideAResolve(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name        string
		operation   Operation
		environment []string
	}{
		{name: "beside an OCI reference", operation: OperationResolve,
			environment: gitResolveEnvironment(envRequestedReference + "=oci://registry.example/schema:1")},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t}
			result := Run(t.Context(), Config{Operation: row.operation, Environment: row.environment, Executor: executor, TempDir: t.TempDir()})
			if result.Error == nil || result.Error.Code != "invalid_configuration" || len(executor.calls) != 0 {
				t.Fatalf("Run() = %#v", result)
			}
		})
	}
}
