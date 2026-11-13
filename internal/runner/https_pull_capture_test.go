package runner

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The directories under internal/dataplane/testdata/https-pull are the HTTPS
// pulls the pinned Ptah image ran against test/e2e/httpsserver; `make
// runner-harness-write` regenerates them. These tests are in the package
// because the trust HTTPSPull takes and pullExit are not exported.

const (
	capturedHTTPSScratch = "/fetch-work/ptah-https-fetch-capture"
	capturedHTTPSCAFile  = "/credentials/https-ca/ca.pem"
)

// capturedHTTPSPull is one capture: the source the manager and the runner
// would hand HTTPSPull for it, and the words its refusal has to give.
type capturedHTTPSPull struct {
	name    string
	source  HTTPSSource
	refusal string
}

func capturedHTTPSFile(t *testing.T, name, file string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "dataplane", "testdata", "https-pull", name, file))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// capturedHTTPSPulls reads every capture directory. The source is read from
// the arguments the capture ran with, flag by flag, so a flag HTTPSPull adds,
// drops or reorders shows as a difference in TestHTTPSPullBuildsTheArgumentsTheCaptureRan.
func capturedHTTPSPulls(t *testing.T) []capturedHTTPSPull {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "dataplane", "testdata", "https-pull"))
	if err != nil {
		t.Fatal(err)
	}
	formats := map[string]string{}
	for format, flag := range httpsFormats {
		formats[flag] = format
	}
	refusals := map[string]string{
		"refused-download-digest":       "download digest differs",
		"refused-content-digest":        "content digest differs",
		"refused-redirect-other-origin": "redirect refused",
		"refused-dotdot-entry":          "archive entry name refused",
		"refused-symlink":               "symbolic link",
	}
	var pulls []capturedHTTPSPull
	for _, entry := range entries {
		args := strings.Split(strings.TrimSpace(string(capturedHTTPSFile(t, entry.Name(), "args"))), "\n")
		value := func(flag string) string {
			at := slices.Index(args, flag)
			if at < 0 || at+1 >= len(args) {
				return ""
			}
			return args[at+1]
		}
		if len(args) < 3 || args[1] != "pull" {
			t.Fatalf("%s ran %q, not a pull", entry.Name(), args)
		}
		source := HTTPSSource{
			URL: args[2], Digest: value("--expect-download-digest"), Format: formats[value("--format")], Path: value("--path"),
			ExpectedContentDigest: value("--expect-digest"), Schema: args[0] == "schema", selects: gitSelectsDirectory,
		}
		if source.Schema {
			source.selects = gitSelectsFile
		}
		pulls = append(pulls, capturedHTTPSPull{name: entry.Name(), source: source, refusal: refusals[entry.Name()]})
	}
	if len(pulls) != 11 {
		t.Fatalf("read %d captures, want the 11 the harness writes", len(pulls))
	}
	return pulls
}

func TestHTTPSPullBuildsTheArgumentsTheCaptureRan(t *testing.T) {
	t.Parallel()
	for _, pull := range capturedHTTPSPulls(t) {
		t.Run(pull.name, func(t *testing.T) {
			t.Parallel()
			out := path.Join(capturedHTTPSScratch, "migrations")
			if pull.source.Schema {
				out = path.Join(capturedHTTPSScratch, path.Base(pull.source.Path))
			}
			spec, err := HTTPSPull("/usr/local/bin/ptah", pull.source, out, httpsTrust{caFile: capturedHTTPSCAFile})
			if err != nil {
				t.Fatal(err)
			}
			ran := strings.Split(strings.TrimSpace(string(capturedHTTPSFile(t, pull.name, "args"))), "\n")
			if !slices.Equal(spec.Args, ran) {
				t.Fatalf("HTTPSPull built\n%q\nand the capture ran\n%q", spec.Args, ran)
			}
		})
	}
}

// Each refused capture's standard error gives the words the acceptance phase
// matches, in the reason pullExit keeps and in the message of the Resolve
// result that reaches the condition, and no credential.
func TestHTTPSPullExitNamesEachCapturedRefusal(t *testing.T) {
	t.Parallel()
	examined := 0
	for _, pull := range capturedHTTPSPulls(t) {
		if pull.refusal == "" {
			continue
		}
		examined++
		t.Run(pull.name, func(t *testing.T) {
			t.Parallel()
			code, err := strconv.Atoi(strings.TrimSpace(string(capturedHTTPSFile(t, pull.name, "exit-code"))))
			if err != nil {
				t.Fatal(err)
			}
			stderr := capturedHTTPSFile(t, pull.name, "stderr")
			redactor := NewRedactor([]string{EnvHTTPSToken + "=a-token-the-capture-never-printed"})
			if err := namesRefusal(pullExit(code, stderr, redactor).Error(), code, pull.refusal); err != nil {
				t.Fatal(err)
			}
		})
	}
	if examined != 5 {
		t.Fatalf("examined %d refusals, want 5", examined)
	}
}
