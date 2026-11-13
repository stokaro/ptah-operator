package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

const httpsTestURL = "https://files.example.com/app/1.4.0/app.tar.gz"

var (
	httpsTestDownload      = "sha256:" + strings.Repeat("d", 64)
	httpsTestOtherDownload = "sha256:" + strings.Repeat("e", 64)
)

// httpsReport is the document `ptah ... pull <https-url> --json` prints for
// files read from a download.
func httpsReport(t *testing.T, reference, format, path, download string, files []sourcecontent.File) (string, string) {
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
		"source": "https", "reference": reference, "format": format, "download_digest": download, "download_size": 4096,
		"path": path, "content_digest": digest, "files": listed, "written": []string{"/work/out"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body), digest
}

func TestPrepareHTTPSSourceAccessHoldsTheCredentialToItsGrants(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	ca := []byte("-----BEGIN CERTIFICATE-----\nhttps\n-----END CERTIFICATE-----\n")
	caFile := writeGitTrustSource(t, directory, "ca.pem", ca)
	archive := HTTPSSource{URL: httpsTestURL, Digest: httpsTestDownload, Format: "TarGzip", Path: "app/db/migrations", selects: httpsSelectsDirectory}
	file := HTTPSSource{URL: "https://files.example.com:8443/schema", Digest: httpsTestDownload, Format: "File", Path: "schema.hcl", Schema: true, selects: httpsSelectsFile}
	with := func(source HTTPSSource, change func(*HTTPSSource)) HTTPSSource {
		change(&source)
		return source
	}
	for _, row := range []struct {
		name        string
		source      HTTPSSource
		environment []string
		refusal     string
	}{
		{name: "anonymous", source: archive},
		{name: "anonymous with a CA", source: archive, environment: []string{EnvHTTPSCASourceFile + "=" + caFile}},
		{name: "granted token", source: archive, environment: []string{EnvHTTPSToken + "=token", EnvHTTPSHostGrant + "=FILES.EXAMPLE.COM"}},
		{name: "granted username and password", source: archive, environment: []string{
			EnvHTTPSUsername + "=bot", EnvHTTPSPassword + "=secret", EnvHTTPSHostGrant + "=files.example.com",
		}},
		{name: "granted token and CA", source: archive, environment: []string{
			EnvHTTPSToken + "=token", EnvHTTPSHostGrant + "=files.example.com",
			EnvHTTPSCASourceFile + "=" + caFile, EnvHTTPSCASHA256Grant + "=" + gitTestDigest(ca),
		}},
		{name: "granted for the URL's port", source: file, environment: []string{EnvHTTPSToken + "=token", EnvHTTPSHostGrant + "=files.example.com:8443"}},
		{name: "a token beside a username", source: archive, environment: []string{
			EnvHTTPSToken + "=token", EnvHTTPSUsername + "=bot", EnvHTTPSHostGrant + "=files.example.com",
		}, refusal: "a token beside a username or a password"},
		{name: "a token beside a password", source: archive, environment: []string{
			EnvHTTPSToken + "=token", EnvHTTPSPassword + "=secret", EnvHTTPSHostGrant + "=files.example.com",
		}, refusal: "a token beside a username or a password"},
		{name: "a username without a password", source: archive, environment: []string{
			EnvHTTPSUsername + "=bot", EnvHTTPSHostGrant + "=files.example.com",
		}, refusal: "a username without a password"},
		{name: "a password without a username", source: archive, environment: []string{
			EnvHTTPSPassword + "=secret", EnvHTTPSHostGrant + "=files.example.com",
		}, refusal: "a password without a username"},
		{name: "a credential without a grant", source: archive, environment: []string{EnvHTTPSToken + "=token"},
			refusal: "not granted for the source's host"},
		{name: "a credential granted for another host", source: archive, environment: []string{EnvHTTPSToken + "=token", EnvHTTPSHostGrant + "=attacker.example"},
			refusal: "not granted for the source's host"},
		{name: "a grant without the URL's port", source: file, environment: []string{EnvHTTPSToken + "=token", EnvHTTPSHostGrant + "=files.example.com"},
			refusal: "not granted for the source's host"},
		{name: "a host grant without a credential", source: archive, environment: []string{EnvHTTPSHostGrant + "=files.example.com"},
			refusal: "without a credential"},
		{name: "a CA grant without a credential", source: archive, environment: []string{
			EnvHTTPSCASourceFile + "=" + caFile, EnvHTTPSCASHA256Grant + "=" + gitTestDigest(ca),
		}, refusal: "without a credential"},
		{name: "a credential with an ungranted CA", source: archive, environment: []string{
			EnvHTTPSToken + "=token", EnvHTTPSHostGrant + "=files.example.com", EnvHTTPSCASourceFile + "=" + caFile,
		}, refusal: "not the one the credential's owner granted"},
		{name: "a credential with another CA", source: archive, environment: []string{
			EnvHTTPSToken + "=token", EnvHTTPSHostGrant + "=files.example.com",
			EnvHTTPSCASourceFile + "=" + caFile, EnvHTTPSCASHA256Grant + "=" + gitTestDigest([]byte("other")),
		}, refusal: "not the one the credential's owner granted"},
		{name: "a CA grant without the CA", source: archive, environment: []string{
			EnvHTTPSToken + "=token", EnvHTTPSHostGrant + "=files.example.com", EnvHTTPSCASHA256Grant + "=" + gitTestDigest(ca),
		}, refusal: "without the certificate authority"},
		{name: "an unreadable CA", source: archive, environment: []string{EnvHTTPSCASourceFile + "=" + filepath.Join(directory, "missing")},
			refusal: "read the HTTPS certificate authority"},
		{name: "http://", source: with(archive, func(s *HTTPSSource) { s.URL = "http://files.example.com/app.tar.gz" }),
			refusal: "not an https:// download"},
		{name: "a credential in the URL", source: with(archive, func(s *HTTPSSource) { s.URL = "https://bot:secret@files.example.com/app.tar.gz" }),
			refusal: "carries a credential"},
		{name: "a query", source: with(archive, func(s *HTTPSSource) { s.URL = httpsTestURL + "?token=1" }),
			refusal: "a query or a fragment"},
		{name: "a fragment", source: with(archive, func(s *HTTPSSource) { s.URL = httpsTestURL + "#top" }),
			refusal: "a query or a fragment"},
		{name: "no path", source: with(archive, func(s *HTTPSSource) { s.URL = "https://files.example.com/" }),
			refusal: "names no path"},
		{name: "an uppercase download digest", source: with(archive, func(s *HTTPSSource) { s.Digest = strings.ToUpper(httpsTestDownload) }),
			refusal: "not a lowercase SHA-256 digest"},
		{name: "a format the API does not name", source: with(archive, func(s *HTTPSSource) { s.Format = "tar.gz" }),
			refusal: "not File, TarGzip or Zip"},
		{name: "File with a nested path", source: with(file, func(s *HTTPSSource) { s.Path = "db/schema.hcl" }),
			refusal: "the one name the download is given"},
		{name: "File selecting the root", source: with(file, func(s *HTTPSSource) { s.Path = "." }),
			refusal: "the one name the download is given"},
		{name: "a migration directory as a File", source: with(archive, func(s *HTTPSSource) { s.Format, s.Path = "File", "migrations" }),
			refusal: "comes in an archive"},
		{name: "a schema selecting the root", source: with(archive, func(s *HTTPSSource) { s.Path, s.Schema, s.selects = ".", true, httpsSelectsFile }),
			refusal: "selects one file"},
		{name: "an unclean path", source: with(archive, func(s *HTTPSSource) { s.Path = "app/../db" }),
			refusal: "not a clean relative path"},
		{name: "an unknown selection", source: with(archive, func(s *HTTPSSource) { s.selects = "tree" }),
			refusal: "neither a file nor a directory"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			trust, cleanup, err := PrepareHTTPSSourceAccess(row.source, row.environment, t.TempDir())
			if row.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), row.refusal) {
					t.Fatalf("PrepareHTTPSSourceAccess() = %v, want a refusal naming %q", err, row.refusal)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if trust.caFile == caFile {
				t.Fatalf("Ptah would read the mutable projection %s, not a private copy", caFile)
			}
			if trust.caFile != "" {
				if copied, err := os.ReadFile(trust.caFile); err != nil || !bytes.Equal(copied, ca) {
					t.Fatalf("the CA copy holds %q, %v", copied, err)
				}
			}
		})
	}
}

func TestHTTPSPullNamesEveryCoordinate(t *testing.T) {
	t.Parallel()
	content := "sha256:" + strings.Repeat("a", 64)
	for _, row := range []struct {
		name   string
		source HTTPSSource
		trust  httpsTrust
		want   []string
	}{
		{
			name:   "a schema served as itself",
			source: HTTPSSource{URL: "https://files.example.com/schema", Digest: httpsTestDownload, Format: "File", Path: "schema.hcl", Schema: true},
			want: []string{
				"schema", "pull", "https://files.example.com/schema", "--format", "file", "--path", "schema.hcl",
				"--expect-download-digest", httpsTestDownload, "--out", "/out/schema.hcl", "--json",
			},
		},
		{
			name: "a migration directory in a tar.gz, held to its content under a CA",
			source: HTTPSSource{
				URL: httpsTestURL, Digest: httpsTestDownload, Format: "TarGzip", Path: "app/db/migrations",
				ExpectedContentDigest: content,
			},
			trust: httpsTrust{caFile: "/tmp/ca"},
			want: []string{
				"migrations", "pull", httpsTestURL, "--format", "tar.gz", "--path", "app/db/migrations",
				"--expect-download-digest", httpsTestDownload, "--expect-digest", content, "--ca-file", "/tmp/ca",
				"--out", "/out/schema.hcl", "--json",
			},
		},
		{
			name:   "the root of a zip",
			source: HTTPSSource{URL: "https://files.example.com/app.zip", Digest: httpsTestDownload, Format: "Zip", Path: "."},
			want: []string{
				"migrations", "pull", "https://files.example.com/app.zip", "--format", "zip", "--path", ".",
				"--expect-download-digest", httpsTestDownload, "--out", "/out/schema.hcl", "--json",
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			spec, err := HTTPSPull("/usr/local/bin/ptah", row.source, "/out/schema.hcl", row.trust)
			if err != nil {
				t.Fatal(err)
			}
			if spec.Path != "/usr/local/bin/ptah" || !reflect.DeepEqual(spec.Args, row.want) {
				t.Fatalf("HTTPSPull() = %s %q\nwant %q", spec.Path, spec.Args, row.want)
			}
		})
	}
	if _, err := HTTPSPull("ptah", HTTPSSource{URL: httpsTestURL, Digest: httpsTestDownload, Format: "TarGzip", Path: "db"}, "relative", httpsTrust{}); err == nil {
		t.Fatal("HTTPSPull accepted a relative output path")
	}
}

func httpsResolveEnvironment(extra ...string) []string {
	return withRunnerProtocol(append([]string{
		envOperationID + "=resolve-https",
		EnvHTTPSURL + "=" + httpsTestURL,
		EnvHTTPSDigest + "=" + httpsTestDownload,
		EnvHTTPSFormat + "=TarGzip",
		EnvHTTPSPath + "=app/db/migrations",
		EnvHTTPSSelects + "=directory",
	}, extra...))
}

func TestRunnerResolvesAnHTTPSSourceWithoutHandingPtahTheGrants(t *testing.T) {
	t.Parallel()
	files := []sourcecontent.File{
		{Path: "0001_create.up.sql", Data: []byte("CREATE TABLE orders (id int);\n")},
		{Path: "0001_create.down.sql", Data: []byte("DROP TABLE orders;\n")},
	}
	report, digest := httpsReport(t, httpsTestURL, "tar.gz", "app/db/migrations", httpsTestDownload, files)
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: report,
		inspect: func(spec CommandSpec) {
			if spec.Args[0] != "migrations" || argumentAfter(spec, "--expect-download-digest") != httpsTestDownload ||
				argumentAfter(spec, "--expect-digest") != "" {
				t.Errorf("the pull ran as %q", spec.Args)
			}
			values := environmentMap(spec.Env)
			if values[EnvHTTPSToken] != "token" {
				t.Error("Ptah did not receive the credential it authenticates with")
			}
			for _, name := range []string{EnvHTTPSHostGrant, EnvHTTPSURL, EnvHTTPSDigest, EnvHTTPSFormat, EnvHTTPSPath, EnvHTTPSSelects} {
				if _, present := values[name]; present {
					t.Errorf("Ptah received the runner-only %s", name)
				}
			}
		},
	}}}
	result := Run(t.Context(), Config{
		Operation:   OperationResolve,
		Environment: httpsResolveEnvironment(EnvHTTPSToken+"=token", EnvHTTPSHostGrant+"=files.example.com"),
		Executor:    executor,
		TempDir:     t.TempDir(),
	})
	size := int64(len(files[0].Data) + len(files[1].Data))
	if result.Error != nil || result.ResolvedDownloadDigest != httpsTestDownload || result.ResolvedContentDigest != digest ||
		result.ResolvedCommit != "" || result.ResolvedDigest != "" || result.ResolvedContentBytes != size {
		t.Fatalf("Run() = %#v", result)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("the executor ran %d commands, want the one pull", len(executor.calls))
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := ParseResultFor(frame, OperationResolve, "resolve-https"); err != nil || !reflect.DeepEqual(parsed, result) {
		t.Fatalf("ParseResultFor() = %#v, %v", parsed, err)
	}
}

func TestRunnerRefusesAnUngrantedHTTPSCredentialBeforePtahRuns(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name        string
		environment []string
	}{
		{name: "another host", environment: []string{EnvHTTPSToken + "=token", EnvHTTPSHostGrant + "=attacker.example"}},
		{name: "Basic and Bearer", environment: []string{
			EnvHTTPSToken + "=token", EnvHTTPSUsername + "=bot", EnvHTTPSPassword + "=token", EnvHTTPSHostGrant + "=files.example.com",
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t}
			result := Run(t.Context(), Config{
				Operation:   OperationResolve,
				Environment: httpsResolveEnvironment(row.environment...),
				Executor:    executor,
				TempDir:     t.TempDir(),
			})
			if result.Error == nil || result.Error.Code != "invalid_https_access" || result.ChildExitCode != -1 || len(executor.calls) != 0 {
				t.Fatalf("Run() = %#v after %d calls", result, len(executor.calls))
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseResultFor(frame, OperationResolve, "resolve-https"); err != nil {
				t.Fatalf("the refusal frame does not parse: %v", err)
			}
			if strings.Contains(string(frame), "token") {
				t.Fatal("the frame carries the credential")
			}
		})
	}
}

func TestRunnerRefusesAnHTTPSReportThatAnswersAnotherPull(t *testing.T) {
	t.Parallel()
	files := []sourcecontent.File{{Path: "0001_create.up.sql", Data: []byte("CREATE TABLE orders (id int);\n")}}
	report := func(reference, format, path, download string) func() string {
		return func() string {
			body, _ := httpsReport(t, reference, format, path, download, files)
			return body
		}
	}
	for _, row := range []struct {
		name   string
		report func() string
	}{
		{name: "another download", report: report(httpsTestURL, "tar.gz", "app/db/migrations", httpsTestOtherDownload)},
		{name: "another URL", report: report("https://files.example.com/other.tar.gz", "tar.gz", "app/db/migrations", httpsTestDownload)},
		{name: "another format", report: report(httpsTestURL, "zip", "app/db/migrations", httpsTestDownload)},
		{name: "another path", report: report(httpsTestURL, "tar.gz", "app/db", httpsTestDownload)},
		{name: "a commit beside the download", report: func() string {
			body, _ := httpsReport(t, httpsTestURL, "tar.gz", "app/db/migrations", httpsTestDownload, files)
			return strings.Replace(body, `"source":"https"`, `"source":"https","commit":"`+gitTestCommit+`"`, 1)
		}},
		{name: "a Git report", report: func() string {
			body, _ := gitReport(t, "app/db/migrations", gitTestCommit, files)
			return body
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			result := Run(t.Context(), Config{
				Operation:   OperationResolve,
				Environment: httpsResolveEnvironment(),
				Executor:    &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: row.report()}}},
				TempDir:     t.TempDir(),
			})
			if result.Error == nil || result.Error.Code != "invalid_resolve_output" || result.ResolvedDownloadDigest != "" || result.ResolvedContentDigest != "" {
				t.Fatalf("Run() = %#v", result)
			}
		})
	}
}

func TestRunnerRefusesAnHTTPSSourceOutsideAResolveOrBesideAnother(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name        string
		operation   Operation
		environment []string
	}{
		{name: "beside an OCI reference", operation: OperationResolve,
			environment: httpsResolveEnvironment(envRequestedReference + "=oci://registry.example/schema:1")},
		{name: "beside a Git source", operation: OperationResolve,
			environment: httpsResolveEnvironment(EnvGitURL+"="+gitTestURL, EnvGitBranch+"=main", EnvGitPath+"=db", EnvGitSelects+"=directory")},
		{name: "in a verify", operation: OperationVerify, environment: httpsResolveEnvironment()},
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

// Ptah's refusal of a download reaches the result as its last line: a person
// reads in the resource's status that the server served other bytes.
func TestRunnerNamesWhyAnHTTPSPullFailed(t *testing.T) {
	t.Parallel()
	stderr := "Error: download digest differs: " + httpsTestURL + " served " + httpsTestOtherDownload + ", not " + httpsTestDownload + "\n"
	result := Run(t.Context(), Config{
		Operation:   OperationResolve,
		Environment: httpsResolveEnvironment(EnvHTTPSToken+"=hunter2-token", EnvHTTPSHostGrant+"=files.example.com"),
		Executor:    &scriptedExecutor{t: t, responses: []scriptedResponse{{exitCode: 1, stderr: stderr}}},
		TempDir:     t.TempDir(),
	})
	want := "ptah exited with code 1: download digest differs: " + httpsTestURL + " served " + httpsTestOtherDownload
	if result.Error == nil || result.Error.Code != "child_exit" || !strings.HasPrefix(result.Error.Message, want) {
		t.Fatalf("Run() error = %+v, want child_exit starting %q", result.Error, want)
	}
}

func httpsFetchEnvironment(format, selects, path, digest string, extra ...string) []string {
	return append([]string{
		EnvHTTPSURL + "=" + httpsTestURL,
		EnvHTTPSDigest + "=" + httpsTestDownload,
		EnvHTTPSFormat + "=" + format,
		EnvHTTPSPath + "=" + path,
		EnvHTTPSSelects + "=" + selects,
		EnvExpectedContentDigest + "=" + digest,
	}, extra...)
}

func TestFetchHTTPSSourceDeliversTheBoundContent(t *testing.T) {
	t.Parallel()
	migrations := []sourcecontent.File{
		{Path: "0001_create.up.sql", Data: []byte("CREATE TABLE orders (id int);\n")},
		{Path: "nested/0002_index.up.sql", Data: []byte("CREATE INDEX orders_id ON orders (id);\r\n")},
	}
	schema := []sourcecontent.File{{Path: "schema.hcl", Data: []byte("table \"orders\" {}\n")}}
	for _, row := range []struct {
		name, format, ptahFormat, selects, path string
		files                                   []sourcecontent.File
	}{
		{name: "a migration directory in a tar.gz", format: "TarGzip", ptahFormat: "tar.gz", selects: "directory", path: "app/db/migrations", files: migrations},
		{name: "the root of a zip", format: "Zip", ptahFormat: "zip", selects: "directory", path: ".", files: migrations},
		{name: "a schema file", format: "File", ptahFormat: "file", selects: "file", path: "schema.hcl", files: schema},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			report, digest := httpsReport(t, httpsTestURL, row.ptahFormat, row.path, httpsTestDownload, row.files)
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{writesFiles(t, row.files, row.selects == "file", report)}}
			destination := filepath.Join(t.TempDir(), "source")
			err := FetchHTTPSSource(t.Context(), "ptah", httpsFetchEnvironment(row.format, row.selects, row.path, digest), destination, t.TempDir(), executor, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := argumentAfter(executor.calls[0], "--expect-digest"); got != digest {
				t.Fatalf("the pull was not held to the content digest: --expect-digest %q", got)
			}
			if got := argumentAfter(executor.calls[0], "--expect-download-digest"); got != httpsTestDownload {
				t.Fatalf("the pull was not held to the download: --expect-download-digest %q", got)
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

func TestFetchHTTPSSourceRefusesOtherContent(t *testing.T) {
	t.Parallel()
	files := []sourcecontent.File{{Path: "0001_create.up.sql", Data: []byte("CREATE TABLE orders (id int);\n")}}
	report, digest := httpsReport(t, httpsTestURL, "tar.gz", "db", httpsTestDownload, files)
	other := []sourcecontent.File{{Path: "0001_create.up.sql", Data: []byte("DROP TABLE orders;\n")}}
	otherReport, otherDigest := httpsReport(t, httpsTestURL, "tar.gz", "db", httpsTestDownload, other)
	otherDownload, _ := httpsReport(t, httpsTestURL, "tar.gz", "db", httpsTestOtherDownload, files)
	for _, row := range []struct {
		name        string
		environment []string
		response    scriptedResponse
		refusal     string
	}{
		{
			name:        "a pull that wrote other bytes than it reported",
			environment: httpsFetchEnvironment("TarGzip", "directory", "db", digest),
			response:    writesFiles(t, other, false, report),
			refusal:     "the files the pull wrote give " + otherDigest,
		},
		{
			name:        "a report of other content",
			environment: httpsFetchEnvironment("TarGzip", "directory", "db", digest),
			response:    writesFiles(t, other, false, otherReport),
			refusal:     "the pull read " + otherDigest,
		},
		{
			name:        "a report of another download",
			environment: httpsFetchEnvironment("TarGzip", "directory", "db", digest),
			response:    writesFiles(t, files, false, otherDownload),
			refusal:     "from the download " + httpsTestOtherDownload,
		},
		{
			name:        "a pull that failed",
			environment: httpsFetchEnvironment("TarGzip", "directory", "db", digest),
			response:    scriptedResponse{exitCode: 1, stderr: "Error: archive entry name refused: ../escape\n"},
			refusal:     "ptah exited with code 1: archive entry name refused: ../escape",
		},
		{
			name:        "no bound content digest",
			environment: httpsFetchEnvironment("TarGzip", "directory", "db", ""),
			refusal:     "needs the download digest and the resolved content digest",
		},
		{
			name:        "an ungranted credential",
			environment: httpsFetchEnvironment("TarGzip", "directory", "db", digest, EnvHTTPSToken+"=token"),
			refusal:     "not granted for the source's host",
		},
		{
			name: "a Git source as well",
			environment: httpsFetchEnvironment("TarGzip", "directory", "db", digest,
				EnvGitURL+"="+gitTestURL, EnvGitBranch+"=main", EnvGitCommit+"="+gitTestCommit, EnvGitPath+"=db", EnvGitSelects+"=directory"),
			refusal: "names a Git source as well",
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
			err := FetchHTTPSSource(t.Context(), "ptah", row.environment, destination, t.TempDir(), executor, &diagnostics)
			if err == nil || !strings.Contains(err.Error(), row.refusal) || !strings.HasPrefix(err.Error(), CodeHTTPSSourceRefused) {
				t.Fatalf("FetchHTTPSSource() = %v, want a refusal naming %q", err, row.refusal)
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

// The credential reaches the fetch through a file the kubelet projected, and
// the operation container that runs after it never sees one.
func TestFetchHTTPSSourceReadsTheCredentialFromItsFile(t *testing.T) {
	t.Parallel()
	files := []sourcecontent.File{{Path: "0001_create.up.sql", Data: []byte("CREATE TABLE orders (id int);\n")}}
	report, digest := httpsReport(t, httpsTestURL, "tar.gz", "db", httpsTestDownload, files)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("file-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	response := writesFiles(t, files, false, report)
	write := response.inspect
	response.inspect = func(spec CommandSpec) {
		write(spec)
		values := environmentMap(spec.Env)
		if values[EnvHTTPSToken] != "file-token" {
			t.Errorf("Ptah read the token %q, want the file's", values[EnvHTTPSToken])
		}
		if _, present := values[EnvHTTPSTokenFile]; present {
			t.Error("Ptah received the variable naming the credential file")
		}
	}
	err := FetchHTTPSSource(t.Context(), "ptah",
		httpsFetchEnvironment("TarGzip", "directory", "db", digest, EnvHTTPSTokenFile+"="+tokenFile, EnvHTTPSHostGrant+"=files.example.com"),
		filepath.Join(t.TempDir(), "source"), t.TempDir(), &scriptedExecutor{t: t, responses: []scriptedResponse{response}}, nil)
	if err != nil {
		t.Fatal(err)
	}
}
