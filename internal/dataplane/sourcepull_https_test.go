package dataplane_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/test/e2e/httpsfixture"
)

// The directories under testdata/https-pull are the HTTPS pulls the image
// support/ptah.json pins ran against the end-to-end download server
// (test/e2e/httpsserver), and `make runner-harness-write` regenerates them.
// The server serves the downloads test/e2e/httpsfixture builds, behind TLS
// from a CA generated for the run, on a Docker network that names it
// e2e-https.ptah-test.svc and, as another origin, e2e-https-other.ptah-test.svc.
// Each pull ran in a container with the shape of a Job's fetch container, with
// the contents of the directory's args file as its arguments; stdout, stderr
// and exit-code are what it printed and how it exited. The token is generated
// for the run and discarded.
const httpsPullCaptures = "testdata/https-pull"

// httpsPullReports is every capture that exited 0, with the download it names
// and the content it selected, read from the capture's own arguments.
var httpsPullReports = []string{
	"schema-file-basic",
	"schema-file-basic-fetch",
	"schema-file-basic-same-origin-redirect",
	"schema-zip-public",
	"migrations-tar-gzip-bearer",
	"migrations-tar-gzip-bearer-fetch",
}

func httpsPullCapture(t *testing.T, name, file string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(httpsPullCaptures, name, file))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// httpsPullAsked reads what a capture asked for from the arguments it ran
// with: the request, and the download digest it pinned.
func httpsPullAsked(t *testing.T, name string) (dataplane.PullRequest, string) {
	t.Helper()
	args := strings.Split(strings.TrimSpace(string(httpsPullCapture(t, name, "args"))), "\n")
	value := func(flag string) string {
		at := slices.Index(args, flag)
		if at < 0 || at+1 >= len(args) {
			t.Fatalf("%s ran %q without %s", name, args, flag)
		}
		return args[at+1]
	}
	if len(args) < 3 || args[1] != "pull" {
		t.Fatalf("%s ran %q, not a pull", name, args)
	}
	return dataplane.PullRequest{
		Source: dataplane.PullSourceHTTPS, Reference: args[2], Path: value("--path"), Format: value("--format"),
	}, value("--expect-download-digest")
}

func TestDecodeSourcePullHoldsAnHTTPSReportToItsPull(t *testing.T) {
	t.Parallel()
	for _, name := range httpsPullReports {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			report := string(httpsPullCapture(t, name, "stdout"))
			asked, download := httpsPullAsked(t, name)
			decoded, err := dataplane.DecodeSourcePull([]byte(report), asked)
			// The content digest Ptah reports is the one the operator computes
			// from the fixture's files, so a plan bound to it names them.
			content, contentErr := httpsfixture.MigrationContent()
			if strings.HasPrefix(name, "schema-") {
				content, contentErr = httpsfixture.SchemaContent(filepath.Base(asked.Path))
			}
			if contentErr != nil {
				t.Fatal(contentErr)
			}
			if err != nil || decoded.DownloadDigest != download || decoded.ContentDigest != content || decoded.Commit != "" || len(decoded.Files) == 0 {
				t.Fatalf("DecodeSourcePull() = %#v, %v, want the download %s with content %s", decoded, err, download, content)
			}
			replace := func(old, replacement string) func(string) string {
				return func(report string) string { return strings.Replace(report, old, replacement, 1) }
			}
			const otherPull = "answers another reference or path"
			for _, mutation := range []struct {
				name, refusal string
				mutate        func(string) string
			}{
				{"a Git report", "not a HTTPS one", replace(`"source": "https"`, `"source": "git"`)},
				{"another URL", otherPull, replace(asked.Reference, asked.Reference+"x")},
				{"another path", otherPull, replace(`"path": "`+asked.Path+`"`, `"path": "other"`)},
				{"another format", "answers another format", replace(`"format": "`+asked.Format+`"`, `"format": "other"`)},
				{"no download digest", "names no download digest", replace(`"download_digest": "`+download+`"`, `"download_digest": ""`)},
				{"no download size", "names no download size", func(report string) string {
					at := strings.Index(report, `"download_size": `)
					if at < 0 {
						return report
					}
					end := at + len(`"download_size": `)
					for end < len(report) && report[end] >= '0' && report[end] <= '9' {
						end++
					}
					return report[:at] + `"download_size": 0` + report[end:]
				}},
				{"a commit beside the download", "names a commit", replace(`"download_digest":`, `"commit": "`+strings.Repeat("a", 40)+`", "download_digest":`)},
				{"a digest the files miss", "the pull report's files give", replace(decoded.ContentDigest, "sha256:"+strings.Repeat("0", 64))},
				{"a file's other size", "the pull report's files give", replace(
					fmt.Sprintf(`"size": %d`, decoded.Files[0].Size), fmt.Sprintf(`"size": %d`, decoded.Files[0].Size+1))},
				{"two documents", "decode HTTPS pull report", func(report string) string { return report + report }},
			} {
				t.Run(mutation.name, func(t *testing.T) {
					t.Parallel()
					mutated := mutation.mutate(report)
					if mutated == report {
						t.Fatal("the mutation changed nothing")
					}
					_, err := dataplane.DecodeSourcePull([]byte(mutated), asked)
					if err == nil || !strings.Contains(err.Error(), mutation.refusal) {
						t.Fatalf("DecodeSourcePull() = %v, want a refusal naming %q", err, mutation.refusal)
					}
				})
			}
		})
	}
}

// The table above is every report under the directory, and every other
// capture is a refusal that printed none.
func TestHTTPSPullReportsAreEveryCaptureThatSucceeded(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(httpsPullCaptures)
	if err != nil {
		t.Fatal(err)
	}
	reports, refusals := 0, 0
	for _, entry := range entries {
		code, err := strconv.Atoi(strings.TrimSpace(string(httpsPullCapture(t, entry.Name(), "exit-code"))))
		if err != nil {
			t.Fatal(err)
		}
		declared := slices.Contains(httpsPullReports, entry.Name())
		switch {
		case code == 0 && !declared:
			t.Errorf("%s exited 0 and has no row", entry.Name())
		case code != 0 && declared:
			t.Errorf("%s exited %d and has a row for a report", entry.Name(), code)
		case code == 0:
			reports++
		default:
			refusals++
			if stdout, err := os.ReadFile(filepath.Join(httpsPullCaptures, entry.Name(), "stdout")); err == nil && len(stdout) != 0 {
				t.Errorf("%s refused and printed %q", entry.Name(), stdout)
			}
		}
	}
	if reports != len(httpsPullReports) || refusals != 5 {
		t.Fatalf("read %d reports and %d refusals, want %d and 5", reports, refusals, len(httpsPullReports))
	}
}
