package dataplane_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// The directories under testdata/git-pull are the pulls the image
// support/ptah.json pins ran against the end-to-end Git server
// (test/e2e/gitserver), and `make runner-harness-write` regenerates them. The
// server holds the repository gitfixture.RepositorySpec(2) describes, with keys
// generated for the run, on a Docker network that names it
// e2e-git.ptah-test.svc. It takes the flags the git-sources phase gives it,
// except that it listens on 443 and 22 itself where the phase's Service maps
// those ports, so the URLs carry no port and the known-hosts line matches. The
// two refused-commit directories read gitfixture.RewrittenSpec() instead, whose
// rewrite takes the old tip of main off the server.
//
// Each pull runs in a container with the shape of a Job's fetch container, with
// the contents of the directory's args file as its arguments. stdout, stderr and
// exit-code are what it printed and how it exited, stored as printed: nothing in
// them names the capturing machine, so nothing was removed. The keys and the
// token are generated for the run and discarded.
//
// They are captured rather than written because the question is what Ptah
// emits, and a document written here could only prove that the reader agrees
// with its own author. internal/runner reads the same directories for the
// arguments and the refusals.
const gitPullCaptures = "testdata/git-pull"

// What the capture's repository holds. v1 names the first commit and main the
// third, whose schema has a column more; the migration directory does not change.
const (
	capturedFirstCommit   = "ee7f9f4a97d8a24bf8f6bb6724f9e95c3f3b90a8"
	capturedTipCommit     = "7c63b477b7bc3d4eaf899d687d57d88fab63fedc"
	capturedFirstSchema   = "sha256:952d69cc98f2d0e50b972f041f86f3368b0986107ee26025e5817adddebe27ab"
	capturedTipSchema     = "sha256:51fdf83bf045136ef654e793dd439ad572c8ca52250d7c5f824ecf70e216f518"
	capturedMigrationsDir = "sha256:8a7eb2be8871d4eb80e45bf4620c00390da96a1ef1afea0aa9a6b94c87d365bc"
)

// gitPullReports is every capture that exited 0, and the commit and content
// each one has to report. The ones that exited otherwise are refusals, which
// printed no report.
var gitPullReports = []struct{ name, commit, digest string }{
	{"schema-https-branch", capturedTipCommit, capturedTipSchema},
	{"schema-https-tag", capturedFirstCommit, capturedFirstSchema},
	{"schema-https-commit", capturedTipCommit, capturedTipSchema},
	{"schema-https-branch-commit", capturedTipCommit, capturedTipSchema},
	{"schema-ssh-branch", capturedTipCommit, capturedTipSchema},
	{"schema-ssh-tag", capturedFirstCommit, capturedFirstSchema},
	{"schema-ssh-commit", capturedTipCommit, capturedTipSchema},
	{"migrations-https-branch", capturedTipCommit, capturedMigrationsDir},
	{"migrations-https-tag", capturedFirstCommit, capturedMigrationsDir},
	{"migrations-https-commit", capturedTipCommit, capturedMigrationsDir},
	{"migrations-ssh-branch", capturedTipCommit, capturedMigrationsDir},
	{"migrations-ssh-tag", capturedFirstCommit, capturedMigrationsDir},
	{"migrations-ssh-commit", capturedTipCommit, capturedMigrationsDir},
	{"migrations-ssh-tag-commit", capturedFirstCommit, capturedMigrationsDir},
}

func gitPullCapture(t *testing.T, name, file string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(gitPullCaptures, name, file))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// gitPullAsked reads the reference and the path a capture asked for from the
// arguments it ran with.
func gitPullAsked(t *testing.T, name string) (reference, selected string) {
	t.Helper()
	args := strings.Split(strings.TrimSpace(string(gitPullCapture(t, name, "args"))), "\n")
	flag := slices.Index(args, "--path")
	if len(args) < 3 || args[1] != "pull" || flag < 0 || flag+1 >= len(args) {
		t.Fatalf("%s ran %q, not a pull of a path", name, args)
	}
	return args[2], args[flag+1]
}

var capturedFileSize = regexp.MustCompile(`"size": (\d+)`)

// gitPullMutations are the ways a report can stop answering its pull, each with
// the check that has to refuse it.
func gitPullMutations(selected, commit, digest string) []struct {
	name, refusal string
	mutate        func(string) string
} {
	replace := func(old, replacement string) func(string) string {
		return func(report string) string { return strings.Replace(report, old, replacement, 1) }
	}
	const (
		otherPull = "answers another reference or path"
		files     = "lists files the content digest refuses"
		digests   = "the pull report's files give"
	)
	return []struct {
		name, refusal string
		mutate        func(string) string
	}{
		{"an OCI report", "not a Git one", replace(`"source": "git"`, `"source": "oci"`)},
		{"another repository", otherPull, replace("acme/app", "acme/other")},
		{"another path", otherPull, replace(`"path": "`+selected+`"`, `"path": "db"`)},
		{"an abbreviated commit", "names no full commit id", replace(commit, commit[:12])},
		{"no files", files, func(report string) string {
			return report[:strings.Index(report, `"files"`)] + `"files": [], "written": []}`
		}},
		{"a digest the files miss", digests, replace(digest, "sha256:"+strings.Repeat("0", 64))},
		{"a file's other size", digests, func(report string) string {
			size := capturedFileSize.FindStringSubmatch(report)
			if size == nil {
				return report
			}
			bytes, err := strconv.Atoi(size[1])
			if err != nil {
				return report
			}
			return strings.Replace(report, size[0], fmt.Sprintf(`"size": %d`, bytes+1), 1)
		}},
		{"two documents", "decode Git pull report", func(report string) string { return report + report }},
		// A release that renames a field the decoder reads. The decoder is not
		// strict, so each of these reads as the field being absent.
		{"a renamed source", "not a Git one", replace(`"source":`, `"origin":`)},
		{"a renamed reference", otherPull, replace(`"reference":`, `"ref":`)},
		{"a renamed commit", "names no full commit id", replace(`"commit":`, `"revision":`)},
		{"a renamed path", otherPull, replace(`"path":`, `"selected":`)},
		{"a renamed digest", "names no content digest", replace(`"content_digest":`, `"digest":`)},
		{"renamed files", files, replace(`"files":`, `"entries":`)},
		{"a file's renamed size", digests, replace(`"size":`, `"bytes":`)},
		{"a file's renamed hash", files, replace(`"sha256":`, `"digest":`)},
	}
}

func TestDecodeGitPullHoldsTheReportToItsPull(t *testing.T) {
	t.Parallel()
	for _, capture := range gitPullReports {
		t.Run(capture.name, func(t *testing.T) {
			t.Parallel()
			report := string(gitPullCapture(t, capture.name, "stdout"))
			reference, selected := gitPullAsked(t, capture.name)
			decoded, err := dataplane.DecodeGitPull([]byte(report), reference, selected)
			if err != nil || decoded.Commit != capture.commit || decoded.ContentDigest != capture.digest {
				t.Fatalf("DecodeGitPull() = %#v, %v, want commit %s with content %s", decoded, err, capture.commit, capture.digest)
			}
			for _, mutation := range gitPullMutations(selected, capture.commit, capture.digest) {
				t.Run(mutation.name, func(t *testing.T) {
					t.Parallel()
					mutated := mutation.mutate(report)
					if mutated == report {
						t.Fatal("the mutation changed nothing")
					}
					_, err := dataplane.DecodeGitPull([]byte(mutated), reference, selected)
					if err == nil {
						t.Fatalf("DecodeGitPull accepted %s", mutated)
					}
					if !strings.Contains(err.Error(), mutation.refusal) {
						t.Fatalf("DecodeGitPull refused for another reason: %v, want %q", err, mutation.refusal)
					}
				})
			}
		})
	}
}

// The table above is every report under the directory: a capture that exited 0
// and has no row would be a report nothing decodes.
func TestGitPullReportsAreEveryCaptureThatSucceeded(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(gitPullCaptures)
	if err != nil {
		t.Fatal(err)
	}
	examined, reports := 0, 0
	for _, entry := range entries {
		examined++
		code := strings.TrimSpace(string(gitPullCapture(t, entry.Name(), "exit-code")))
		declared := slices.ContainsFunc(gitPullReports, func(row struct{ name, commit, digest string }) bool {
			return row.name == entry.Name()
		})
		switch {
		case code == "0" && !declared:
			t.Errorf("%s exited 0 and has no row", entry.Name())
		case code != "0" && declared:
			t.Errorf("%s exited %s and has a row for a report", entry.Name(), code)
		case code == "0":
			reports++
		}
	}
	if reports != len(gitPullReports) {
		t.Fatalf("read %d reports of the %d declared, in %d captures", reports, len(gitPullReports), examined)
	}
}
