package dataplane_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// Every refusal the harness captured from the pinned image is the text
// VerifyRefusalDiagnostics makes of its report, and a verification that wrote no
// report is not read as one.
func TestVerifyRefusalDiagnosticsIsWhatThePinnedPtahWrote(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "runner-harness", "transcript.json"))
	if err != nil {
		t.Fatal(err)
	}
	var transcript []struct {
		Name     string `json:"name"`
		Children []struct {
			Command string `json:"command"`
			Exit    int    `json:"exit"`
			Stderr  string `json:"stderr"`
			Stdout  string `json:"stdout"`
		} `json:"children"`
	}
	if err := json.Unmarshal(data, &transcript); err != nil {
		t.Fatal(err)
	}
	refusals, failures := 0, 0
	for _, row := range transcript {
		for _, child := range row.Children {
			if child.Command != "oci verify" || child.Stderr == "" {
				continue
			}
			if child.Stdout == "" {
				failures++
				if strings.Contains(child.Stderr, "does not satisfy the verification policy") {
					t.Errorf("%s: a verification with no report wrote a refusal: %q", row.Name, child.Stderr)
				}
				continue
			}
			refusals++
			document, err := os.ReadFile(filepath.Join("testdata", child.Stdout))
			if err != nil {
				t.Fatal(err)
			}
			report, err := dataplane.DecodeVerify(document)
			if err != nil {
				t.Fatalf("%s: %v", row.Name, err)
			}
			want := dataplane.VerifyRefusalDiagnostics(report)
			if got := strings.ReplaceAll(child.Stderr, "<digest>", report.Digest); child.Exit != 2 || got != want {
				t.Errorf("%s: exit %d, standard error\n%q\nwant\n%q", row.Name, child.Exit, got, want)
			}
		}
	}
	// schema-verify-refused holds two refusals and one failure, and
	// migration-verify-refused one refusal.
	if refusals != 3 || failures != 1 {
		t.Fatalf("read %d refusals and %d failures in the transcript, want 3 and 1", refusals, failures)
	}
}
