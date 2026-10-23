package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

func gitFetchEnvironment(selects, path, digest string, extra ...string) []string {
	return append([]string{
		EnvGitURL + "=" + gitTestURL,
		EnvGitBranch + "=main",
		EnvGitCommit + "=" + gitTestCommit,
		EnvGitPath + "=" + path,
		EnvGitSelects + "=" + selects,
		EnvExpectedContentDigest + "=" + digest,
	}, extra...)
}

// writesFiles is a pull that writes files to --out and prints report.
func writesFiles(t *testing.T, files []sourcecontent.File, single bool, report string) scriptedResponse {
	t.Helper()
	return scriptedResponse{stdout: report, inspect: func(spec CommandSpec) {
		out := argumentAfter(spec, "--out")
		for _, file := range files {
			target := filepath.Join(out, filepath.FromSlash(file.Path))
			if single {
				target = out
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, file.Data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}}
}

func TestFetchGitSourceDeliversTheBoundContent(t *testing.T) {
	t.Parallel()
	migrations := []sourcecontent.File{
		{Path: "0001_create.up.sql", Data: []byte("CREATE TABLE orders (id int);\n")},
		{Path: "nested/0002_index.up.sql", Data: []byte("CREATE INDEX orders_id ON orders (id);\r\n")},
	}
	schema := []sourcecontent.File{{Path: "schema.hcl", Data: []byte("table \"orders\" {}\n")}}
	for _, row := range []struct {
		name    string
		selects string
		path    string
		files   []sourcecontent.File
	}{
		{name: "a migration directory", selects: "directory", path: "db/migrations", files: migrations},
		{name: "a schema file", selects: "file", path: "db/schema.hcl", files: schema},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			report, digest := gitReport(t, row.path, gitTestCommit, row.files)
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{writesFiles(t, row.files, row.selects == "file", report)}}
			destination := filepath.Join(t.TempDir(), "source")
			err := FetchGitSource(t.Context(), "ptah", gitFetchEnvironment(row.selects, row.path, digest), destination, t.TempDir(), executor, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := argumentAfter(executor.calls[0], "--expect-digest"); got != digest {
				t.Fatalf("the pull was not held to the digest: --expect-digest %q", got)
			}
			if got := argumentAfter(executor.calls[0], "--commit"); got != gitTestCommit {
				t.Fatalf("the pull was not held to the commit: --commit %q", got)
			}
			delivered, err := sourcecontent.ReadTree(destination, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := sourcecontent.Digest(delivered); err != nil || got != digest {
				t.Fatalf("the destination gives %q, %v; want %q", got, err, digest)
			}
		})
	}
}

func TestFetchGitSourceRefusesOtherContent(t *testing.T) {
	t.Parallel()
	files := []sourcecontent.File{{Path: "0001_create.up.sql", Data: []byte("CREATE TABLE orders (id int);\n")}}
	report, digest := gitReport(t, "db", gitTestCommit, files)
	other := []sourcecontent.File{{Path: "0001_create.up.sql", Data: []byte("DROP TABLE orders;\n")}}
	otherReport, otherDigest := gitReport(t, "db", gitTestCommit, other)
	for _, row := range []struct {
		name        string
		environment []string
		response    scriptedResponse
		refusal     string
	}{
		{
			name:        "a pull that wrote other bytes than it reported",
			environment: gitFetchEnvironment("directory", "db", digest),
			response:    writesFiles(t, other, false, report),
			refusal:     "the files the pull wrote give " + otherDigest,
		},
		{
			name:        "a report of other content",
			environment: gitFetchEnvironment("directory", "db", digest),
			response:    writesFiles(t, other, false, otherReport),
			refusal:     "the pull read " + otherDigest,
		},
		{
			name:        "a pull that failed",
			environment: gitFetchEnvironment("directory", "db", digest),
			response:    scriptedResponse{exitCode: 1, stderr: "Error: object not found: the branch was force-pushed\n"},
			refusal:     "ptah exited with code 1: object not found: the branch was force-pushed",
		},
		{
			name:        "no bound commit",
			environment: []string{EnvGitURL + "=" + gitTestURL, EnvGitBranch + "=main", EnvGitPath + "=db", EnvGitSelects + "=directory", EnvExpectedContentDigest + "=" + digest},
			refusal:     "needs the resolved commit",
		},
		{
			name:        "an ungranted credential",
			environment: gitFetchEnvironment("directory", "db", digest, EnvGitPassword+"=token"),
			refusal:     "not granted for the source's host",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t}
			if row.response.stdout != "" || row.response.exitCode != 0 {
				executor.responses = []scriptedResponse{row.response}
			}
			destination := filepath.Join(t.TempDir(), "source")
			var diagnostics bytes.Buffer
			err := FetchGitSource(t.Context(), "ptah", row.environment, destination, t.TempDir(), executor, &diagnostics)
			if err == nil || !strings.Contains(err.Error(), row.refusal) || !strings.HasPrefix(err.Error(), CodeGitSourceRefused) {
				t.Fatalf("FetchGitSource() = %v, want a refusal naming %q", err, row.refusal)
			}
			if _, statErr := os.Stat(destination); statErr == nil {
				t.Fatal("a refused fetch left files for the operation to read")
			}
			if row.response.stderr != "" && !strings.Contains(diagnostics.String(), row.response.stderr) {
				t.Fatalf("Ptah's reason did not reach the Pod's log: %q", diagnostics.String())
			}
		})
	}
}

func TestFetchGitSourceRefusesALinkThePullLeft(t *testing.T) {
	t.Parallel()
	files := []sourcecontent.File{{Path: "0001_create.up.sql", Data: []byte("CREATE TABLE orders (id int);\n")}}
	report, digest := gitReport(t, "db", gitTestCommit, files)
	response := writesFiles(t, files, false, report)
	write := response.inspect
	response.inspect = func(spec CommandSpec) {
		write(spec)
		if err := os.Symlink("/etc/passwd", filepath.Join(argumentAfter(spec, "--out"), "0002_link.up.sql")); err != nil {
			t.Fatal(err)
		}
	}
	err := FetchGitSource(t.Context(), "ptah", gitFetchEnvironment("directory", "db", digest), filepath.Join(t.TempDir(), "source"), t.TempDir(),
		&scriptedExecutor{t: t, responses: []scriptedResponse{response}}, nil)
	if err == nil || !strings.Contains(err.Error(), "other than a regular file") {
		t.Fatalf("FetchGitSource() = %v, want the link refused", err)
	}
}
