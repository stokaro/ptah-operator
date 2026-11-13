package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

// writeCredential writes value to a file of its own and returns the path.
func writeCredential(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte(value), 0o400); err != nil {
		t.Fatal(err)
	}
	return path
}

// Every credential a Job names by file is read into the variable Ptah reads,
// and the variable naming the file goes. A file the kubelet did not write is
// an optional key the Secret lacks: the credential stays unset.
func TestResolveCredentialFilesReadsEveryCredentialTheJobNamesByFile(t *testing.T) {
	t.Parallel()
	credentials := CredentialFiles()
	if len(credentials) == 0 {
		t.Fatal("no credential is read from a file, so nothing is measured")
	}
	for _, credential := range credentials {
		t.Run(credential.Value, func(t *testing.T) {
			t.Parallel()
			value := "value-of-" + strings.ToLower(credential.Value) + "\nwith a second line"
			resolved, err := ResolveCredentialFiles([]string{
				"PTAH_OPERATOR_UNRELATED=kept",
				credential.File + "=" + writeCredential(t, value),
			})
			if err != nil {
				t.Fatalf("ResolveCredentialFiles() error = %v", err)
			}
			values := environmentMap(resolved)
			if got, set := values[credential.Value]; !set || got != value {
				t.Fatalf("%s = %q (set %t), want the file's bytes %q", credential.Value, got, set, value)
			}
			if _, named := values[credential.File]; named {
				t.Fatalf("%s survived the read: %v", credential.File, resolved)
			}
			if values["PTAH_OPERATOR_UNRELATED"] != "kept" {
				t.Fatalf("a variable that names no credential was lost: %v", resolved)
			}

			absent, err := ResolveCredentialFiles([]string{credential.File + "=" + filepath.Join(t.TempDir(), "absent")})
			if err != nil {
				t.Fatalf("ResolveCredentialFiles() for an absent file error = %v", err)
			}
			if len(absent) != 0 {
				t.Fatalf("an absent file left %v, want no variable at all", absent)
			}
		})
	}

	// A Job built before credentials moved to files names none, and runs as
	// it did.
	unchanged := []string{EnvDatabaseURL + "=postgres://app@db/app", "PTAH_OPERATOR_UNRELATED=kept"}
	resolved, err := ResolveCredentialFiles(unchanged)
	if err != nil || !slices.Equal(resolved, unchanged) {
		t.Fatalf("ResolveCredentialFiles() = %v, %v for an environment that names no file, want it unchanged", resolved, err)
	}
}

// A refusal names the variable, and never what the file holds.
func TestResolveCredentialFilesRefusesByNameNeverByContent(t *testing.T) {
	t.Parallel()
	const secret = "postgres://app:DO_NOT_PRINT@db/app"
	directory := t.TempDir()
	for _, row := range []struct {
		name    string
		path    func(t *testing.T) string
		extra   []string
		refusal string
	}{
		{
			name:    "the credential set both ways",
			path:    func(t *testing.T) string { return writeCredential(t, secret) },
			extra:   []string{EnvDatabaseURL + "=" + secret},
			refusal: EnvDatabaseURL + " is set and " + EnvDatabaseURLFile + " names a file for it as well",
		},
		{
			name:    "a relative path",
			path:    func(*testing.T) string { return "credentials/database/url" },
			refusal: EnvDatabaseURLFile + ": the credential path is not a clean absolute path",
		},
		{
			name:    "a path that climbs",
			path:    func(*testing.T) string { return directory + "/../" + filepath.Base(directory) + "/url" },
			refusal: EnvDatabaseURLFile + ": the credential path is not a clean absolute path",
		},
		{
			name:    "a directory",
			path:    func(*testing.T) string { return directory },
			refusal: EnvDatabaseURLFile + ": the credential path is not a regular file",
		},
		{
			name:    "a NUL byte",
			path:    func(t *testing.T) string { return writeCredential(t, secret+"\x00tail") },
			refusal: EnvDatabaseURLFile + ": the credential file holds a NUL byte",
		},
		{
			name: "a file larger than an environment string may be",
			path: func(t *testing.T) string {
				return writeCredential(t, secret+strings.Repeat("x", maxCredentialFileBytes))
			},
			refusal: EnvDatabaseURLFile + ": the credential file is larger than 65536 bytes",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			environment := append([]string{EnvDatabaseURLFile + "=" + row.path(t)}, row.extra...)
			resolved, err := ResolveCredentialFiles(environment)
			if err == nil || !strings.Contains(err.Error(), row.refusal) {
				t.Fatalf("ResolveCredentialFiles() = %v, %v, want a refusal saying %q", resolved, err, row.refusal)
			}
			if strings.Contains(err.Error(), "DO_NOT_PRINT") {
				t.Fatalf("the refusal carries the credential: %v", err)
			}
		})
	}
}

// The value the runner reads from a file is a value the Redactor holds: the
// child's output is redacted of it, and the child receives it in its
// environment and nowhere else.
func TestRunRedactsACredentialReadFromAFile(t *testing.T) {
	t.Parallel()

	databaseURL := "postgres://dbuser:db-password@db.example/app?sslmode=require"
	devURL := "postgres://devuser:dev-password@dev.example/app"
	registryPassword := "registry-password-from-a-file"
	output := strings.Join([]string{databaseURL, devURL, registryPassword}, " ")
	var child CommandSpec
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: output, stderr: output,
		inspect: func(spec CommandSpec) { child = spec },
	}}}
	var diagnostics bytes.Buffer
	result := Run(context.Background(), Config{
		Operation: OperationPlan,
		Environment: withRunnerProtocol([]string{
			envOperationID + "=redact-file-1",
			EnvDatabaseURLFile + "=" + writeCredential(t, databaseURL),
			EnvDevelopmentDatabaseURLFile + "=" + writeCredential(t, devURL),
			EnvOCIPasswordFile + "=" + writeCredential(t, registryPassword),
			envCoordinationDigest + "=" + testCoordinationDigest(),
			envExpectedDatabaseEngine + "=PostgreSQL",
			envPlanSealPublicKey + "=" + testPlanSealKey.PublicKey().Encode(),
			envSealedPlanJobName + "=ptah-plan-redact-file-1",
		}),
		Diagnostics: &diagnostics,
		Executor:    executor,
	})
	if len(executor.calls) != 1 {
		t.Fatalf("Run() started %d commands, want the plan alone; error %#v", len(executor.calls), result.Error)
	}
	combined := result.Stdout + diagnostics.String()
	for _, secret := range []string{databaseURL, devURL, registryPassword} {
		if strings.Contains(combined, secret) {
			t.Fatalf("sanitized output contains secret %q: %s", secret, combined)
		}
		if slices.ContainsFunc(child.Args, func(argument string) bool { return strings.Contains(argument, secret) }) {
			t.Fatalf("a credential reached the command line: %v", child.Args)
		}
	}
	values := environmentMap(child.Env)
	for name, want := range map[string]string{EnvDatabaseURL: databaseURL, EnvDevelopmentDatabaseURL: devURL, EnvOCIPassword: registryPassword} {
		if values[name] != want {
			t.Fatalf("the child's %s = %q, want what its file holds", name, values[name])
		}
	}
	for _, name := range credentialFileVariables() {
		if _, named := values[name]; named {
			t.Fatalf("the child received %s", name)
		}
	}
}

// The row above cannot tell a redacted plan from one the runner never copied:
// a plan that does not decode is refused in words of the runner's own. A Git
// pull's refusal is copied from Ptah's standard error, so this row reads a
// copy, and the token in it is the one the runner read from its file.
func TestRunRedactsAFileCredentialFromARefusalItCopies(t *testing.T) {
	t.Parallel()

	const token = "a-git-token-read-from-a-file"
	result := Run(t.Context(), Config{
		Operation: OperationResolve,
		Environment: gitResolveEnvironment(
			EnvGitHostGrant+"=git.example.com",
			EnvGitUsernameFile+"="+writeCredential(t, "deploy"),
			EnvGitPasswordFile+"="+writeCredential(t, token),
		),
		Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{
			exitCode: 1, stderr: "remote: the token " + token + " has expired\n",
		}}},
		TempDir: t.TempDir(),
	})
	if result.Error == nil || result.Error.Code != "child_exit" {
		t.Fatalf("Run() error = %+v, want child_exit", result.Error)
	}
	if want := "remote: the token " + RedactionMarker + " has expired"; !strings.Contains(result.Error.Message, want) ||
		strings.Contains(result.Error.Message, token) {
		t.Fatalf("Run() refusal = %q, want Ptah's words with the token redacted, %q", result.Error.Message, want)
	}
}

// A credential file the runner cannot use fails the run as configuration,
// and only once the protocol is known to be this runner's.
func TestRunRefusesACredentialFileItCannotRead(t *testing.T) {
	t.Parallel()
	environment := []string{
		envOperationID + "=file-refused-1",
		envRequestedReference + "=oci://registry.example/schema:main",
		EnvOCIPasswordFile + "=credentials/registry/password",
	}
	result := Run(context.Background(), Config{
		Operation: OperationResolve, Environment: withRunnerProtocol(environment), Executor: &scriptedExecutor{t: t},
	})
	if result.Error == nil || result.Error.Code != "invalid_configuration" || !strings.Contains(result.Error.Message, EnvOCIPasswordFile) {
		t.Fatalf("Run() error = %#v, want invalid_configuration naming %s", result.Error, EnvOCIPasswordFile)
	}
	foreign := Run(context.Background(), Config{
		Operation: OperationResolve, Environment: append(environment, EnvRunnerProtocolVersion+"=999"), Executor: &scriptedExecutor{t: t},
	})
	if foreign.Error == nil || foreign.Error.Code != CodeRunnerProtocolMismatch {
		t.Fatalf("Run() error = %#v for another protocol, want %s first", foreign.Error, CodeRunnerProtocolMismatch)
	}
}

// No child is told where a credential file is, whether or not the runner
// read it.
func TestChildEnvironmentCarriesNoCredentialFileVariable(t *testing.T) {
	t.Parallel()
	var environment []string
	for _, name := range credentialFileVariables() {
		environment = append(environment, name+"=/credentials/some/file")
	}
	environment = append(environment, EnvDatabaseURL+"=postgres://app@db/app")
	child := environmentMap(childEnvironment(environment))
	for _, name := range credentialFileVariables() {
		if _, named := child[name]; named {
			t.Fatalf("the child environment carries %s", name)
		}
	}
	if child[EnvDatabaseURL] == "" {
		t.Fatal("the child environment lost the credential itself")
	}
}

// The OCI fetch reads the registry credential from its files, runs the one
// pull it is given with the credential in the child's environment, and
// passes Ptah's exit status and output through.
func TestFetchOCISourceRunsThePullWithTheCredentialFromItsFile(t *testing.T) {
	t.Parallel()
	const password = "registry-password-from-a-file"
	pull := []string{"schema", "pull", "oci://registry.example/acme/orders@sha256:" + strings.Repeat("a", 64), "--out", "/work/source"}
	var child CommandSpec
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: "pulled\n", stderr: "progress\n", exitCode: 3,
		inspect: func(spec CommandSpec) { child = spec },
	}}}
	var stdout, stderr bytes.Buffer
	exitCode, err := FetchOCISource(t.Context(), "/ptah/ptah", []string{
		EnvOCIPasswordFile + "=" + writeCredential(t, password),
		EnvOCIUsernameFile + "=" + filepath.Join(t.TempDir(), "absent"),
		"PTAH_OCI_REGISTRY=registry.example",
	}, pull, executor, &stdout, &stderr)
	if err != nil || exitCode != 3 {
		t.Fatalf("FetchOCISource() = %d, %v, want Ptah's own exit status 3", exitCode, err)
	}
	if child.Path != "/ptah/ptah" || !slices.Equal(child.Args, pull) {
		t.Fatalf("the fetch ran %s %v, want the pull it was given", child.Path, child.Args)
	}
	values := environmentMap(child.Env)
	if values[EnvOCIPassword] != password || values["PTAH_OCI_REGISTRY"] != "registry.example" {
		t.Fatalf("the pull's environment = %v, want the password from its file beside the rest", child.Env)
	}
	if _, set := values[envOCIUsername]; set {
		t.Fatal("an absent username file set the username")
	}
	for _, name := range credentialFileVariables() {
		if _, named := values[name]; named {
			t.Fatalf("the pull was told %s", name)
		}
	}
	if stdout.String() != "pulled\n" || stderr.String() != "progress\n" {
		t.Fatalf("Ptah's output = %q, %q, want it passed through", stdout.String(), stderr.String())
	}
}

func TestFetchOCISourceRunsNothingElse(t *testing.T) {
	t.Parallel()
	const password = "registry-password-from-a-file"
	pull := []string{"migrations", "pull", "oci://registry.example/acme/ledger@sha256:" + strings.Repeat("b", 64)}
	for _, row := range []struct {
		name        string
		binary      string
		environment func(t *testing.T) []string
		arguments   []string
		refusal     string
	}{
		{name: "another command", binary: "/ptah/ptah", arguments: []string{"schema", "apply"}, refusal: "only schema pull and migrations pull"},
		{name: "a pull of another family", binary: "/ptah/ptah", arguments: []string{"plugins", "pull"}, refusal: "only schema pull and migrations pull"},
		{name: "nothing to run", binary: "/ptah/ptah", refusal: "only schema pull and migrations pull"},
		{name: "no Ptah binary", arguments: pull, refusal: "the Ptah binary path is empty"},
		{
			name: "a credential file it cannot read", binary: "/ptah/ptah", arguments: pull,
			environment: func(*testing.T) []string { return []string{EnvOCIPasswordFile + "=relative/password"} },
			refusal:     EnvOCIPasswordFile,
		},
		{
			name: "the credential on the command line", binary: "/ptah/ptah", arguments: append(slices.Clone(pull), "--password", password),
			environment: func(t *testing.T) []string { return []string{EnvOCIPasswordFile + "=" + writeCredential(t, password)} },
			refusal:     "credential",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var environment []string
			if row.environment != nil {
				environment = row.environment(t)
			}
			exitCode, err := FetchOCISource(t.Context(), row.binary, environment, row.arguments, &scriptedExecutor{t: t}, nil, nil)
			if err == nil || exitCode != -1 || !strings.HasPrefix(err.Error(), CodeOCIFetchRefused) || !strings.Contains(err.Error(), row.refusal) {
				t.Fatalf("FetchOCISource() = %d, %v, want a refusal naming %q", exitCode, err, row.refusal)
			}
			if strings.Contains(err.Error(), password) {
				t.Fatalf("the refusal carries the credential: %v", err)
			}
		})
	}
}

// The Git fetch reads its credential from its files before it holds the
// source to the credential owner's grants: a credential named by file is
// held to the grants as one in the environment was.
func TestFetchGitSourceHoldsACredentialFromItsFileToTheGrants(t *testing.T) {
	t.Parallel()
	files := []sourcecontent.File{{Path: "0001_create.up.sql", Data: []byte("CREATE TABLE orders (id int);\n")}}
	report, digest := gitReport(t, "db", gitTestCommit, files)

	destination := filepath.Join(t.TempDir(), "source")
	err := FetchGitSource(t.Context(), "ptah",
		gitFetchEnvironment("directory", "db", digest, EnvGitPasswordFile+"="+writeCredential(t, "token")),
		destination, t.TempDir(), &scriptedExecutor{t: t}, nil)
	if err == nil || !strings.Contains(err.Error(), "not granted for the source's host") {
		t.Fatalf("FetchGitSource() = %v, want an ungranted credential from a file refused", err)
	}

	var child CommandSpec
	response := writesFiles(t, files, false, report)
	write := response.inspect
	response.inspect = func(spec CommandSpec) { child = spec; write(spec) }
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{response}}
	granted := gitFetchEnvironment("directory", "db", digest,
		EnvGitHostGrant+"=git.example.com",
		EnvGitUsernameFile+"="+writeCredential(t, "deploy"),
		EnvGitPasswordFile+"="+writeCredential(t, "token"))
	if err := FetchGitSource(t.Context(), "ptah", granted, destination, t.TempDir(), executor, nil); err != nil {
		t.Fatalf("FetchGitSource() with a granted credential from its file: %v", err)
	}
	values := environmentMap(child.Env)
	if values[EnvGitUsername] != "deploy" || values[EnvGitPassword] != "token" {
		t.Fatalf("the pull's environment = %v, want the credential from its files", child.Env)
	}
	for _, name := range credentialFileVariables() {
		if _, named := values[name]; named {
			t.Fatalf("the pull was told %s", name)
		}
	}
}
