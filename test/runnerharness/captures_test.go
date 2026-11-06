package runnerharness_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/runnerharness"
)

func committedCaptures(t *testing.T) (string, runnerharness.Captures) {
	t.Helper()
	root := filepath.Join(repositoryRoot, filepath.FromSlash(runnerharness.CapturesDirectory))
	captures, err := runnerharness.LoadCaptures(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(captures.Rows) == 0 {
		t.Fatal("no committed row: the harness has nothing to hold a run to")
	}
	return root, captures
}

// Every committed document decodes with the decoder the runner uses for its
// command, so a decoder that stops reading what the pinned image prints fails in
// `go test`, with no Docker and before anything else.
func TestCommittedDocumentsDecode(t *testing.T) {
	t.Parallel()
	_, captures := committedCaptures(t)
	read := 0
	for _, row := range captures.Rows {
		for _, child := range row.Children {
			if err := runnerharness.DecodeDocument(child.Args, child.Exit, captures.Documents[child.Stdout]); err != nil {
				t.Errorf("%s: `ptah %s`: %v", row.Name, child.Command, err)
			}
			read++
		}
		if row.SavedPlan != "" {
			if _, err := dataplane.DecodePlan(captures.Documents[row.SavedPlan], row.Engine); err != nil {
				t.Errorf("%s: the saved plan: %v", row.Name, err)
			}
			read++
		}
	}
	if read != countCommands(captures)+countPlans(captures) || read == 0 {
		t.Fatalf("read %d documents", read)
	}
}

func countCommands(captures runnerharness.Captures) int {
	total := 0
	for _, row := range captures.Rows {
		total += len(row.Children)
	}
	return total
}

func countPlans(captures runnerharness.Captures) int {
	total := 0
	for _, row := range captures.Rows {
		if row.SavedPlan != "" {
			total++
		}
	}
	return total
}

// The captures answer the pin. A repin that changes the catalog and leaves the
// captures behind fails here, offline, rather than on a job an hour later.
func TestCommittedCapturesComeFromThePin(t *testing.T) {
	t.Parallel()
	_, captures := committedCaptures(t)
	image, commit, release := pinnedExecutor(t)
	want := runnerharness.Provenance{PtahRelease: release, PtahCommit: commit, PtahImage: image}
	if captures.Provenance != want {
		t.Fatalf("the captures were taken from %+v, and the catalog pins %+v: run `make runner-harness-write` and review the diff",
			captures.Provenance, want)
	}
}

// What is committed is what a complete run counted.
func TestCommittedCapturesCoverTheDeclaredSet(t *testing.T) {
	t.Parallel()
	_, captures := committedCaptures(t)
	ran := runnerharness.Ran{Operations: map[runnerharness.OperationCase]int{}, Commands: map[string]int{}}
	for _, row := range captures.Rows {
		ran.Rows++
		if row.Operation != "" {
			ran.Operations[runnerharness.OperationCase{Operation: runner.Operation(row.Operation), Source: row.Source}]++
		}
		for _, child := range row.Children {
			ran.Commands[child.Command]++
		}
	}
	if err := runnerharness.CheckCount(runnerharness.Declared.Operations, runnerharness.Declared.Commands, ran); err != nil {
		t.Fatal(err)
	}
}

// Every capture under the directory belongs to a row, or nothing would ever read
// it again.
func TestCommittedCapturesHaveNoStrayFile(t *testing.T) {
	t.Parallel()
	root, captures := committedCaptures(t)
	stray, err := runnerharness.StrayFiles(root, captures)
	if err != nil {
		t.Fatal(err)
	}
	if len(stray) != 0 {
		t.Fatalf("no row produced %q", stray)
	}
}

// A capture holds no credential. The keys and passwords of a run die with it,
// and this is what would show one that did not.
func TestCommittedCapturesHoldNoCredential(t *testing.T) {
	t.Parallel()
	_, captures := committedCaptures(t)
	// Ptah prints a connection string with *** for its password.
	connectionPassword := regexp.MustCompile(`(postgres|mysql)://[^/:@\s"]+:[^*@\s"][^@\s"]*@`)
	scan := func(where string, data []byte) {
		for _, refused := range []string{"PRIVATE KEY", "database_url"} {
			if strings.Contains(string(data), refused) {
				t.Errorf("%s holds %q", where, refused)
			}
		}
		if match := connectionPassword.Find(data); match != nil {
			t.Errorf("%s holds a connection string with a password", where)
		}
	}
	for file, document := range captures.Documents {
		scan(file, document)
	}
	for _, row := range captures.Rows {
		scan("row "+row.Name, []byte(fmt.Sprintf("%+v", row)))
	}
}

func TestReadGateFailsARequiredRunWithNoContext(t *testing.T) {
	t.Parallel()
	lookup := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	if gate, err := runnerharness.ReadGate(lookup(nil)); err != nil || gate.Required {
		t.Fatalf("a plain go test: %+v, %v, want a skip", gate, err)
	}
	if gate, err := runnerharness.ReadGate(lookup(map[string]string{runnerharness.ContextVariable: "somewhere"})); err != nil || gate.Required {
		t.Fatalf("a Docker context set in a shell: %+v, %v, want a skip", gate, err)
	}
	if _, err := runnerharness.ReadGate(lookup(map[string]string{runnerharness.RequireVariable: "1"})); err == nil {
		t.Fatal("a required run with no Docker context was accepted")
	}
	gate, err := runnerharness.ReadGate(lookup(map[string]string{
		runnerharness.RequireVariable: "1", runnerharness.ContextVariable: "somewhere", runnerharness.WriteVariable: "1",
	}))
	if err != nil || !gate.Required || !gate.Write || gate.Context != "somewhere" {
		t.Fatalf("a write run: %+v, %v", gate, err)
	}
}

func TestPinnedExecutorReadsOnlyTheEdgeRow(t *testing.T) {
	t.Parallel()
	const pinned = "ghcr.io/stokaro/ptah@sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	row := func(operator, image string, builds int) string {
		verified := `{"ptahRelease":"v1","ptahCommit":"c","ptahImage":"` + image + `"}`
		list := strings.TrimSuffix(strings.Repeat(verified+",", builds), ",")
		return `{"operator":"` + operator + `","verified":[` + list + `]}`
	}
	image, commit, release, err := runnerharness.PinnedExecutor([]byte(`{"releases":[` + row("v9", "other", 1) + `,` + row("edge", pinned, 1) + `]}`))
	if err != nil || image != pinned || commit != "c" || release != "v1" {
		t.Fatalf("PinnedExecutor() = %q, %q, %q, %v", image, commit, release, err)
	}
	for name, document := range map[string]string{
		"no edge row":   `{"releases":[` + row("v9", pinned, 1) + `]}`,
		"two builds":    `{"releases":[` + row("edge", pinned, 2) + `]}`,
		"not a digest":  `{"releases":[` + row("edge", "ghcr.io/stokaro/ptah:v1", 1) + `]}`,
		"not a catalog": `[`,
	} {
		if _, _, _, err := runnerharness.PinnedExecutor([]byte(document)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestDrivenImagesReadsTheDriversPins(t *testing.T) {
	t.Parallel()
	driver, err := os.ReadFile(filepath.Join(repositoryRoot, "hack", "e2e-kind.sh"))
	if err != nil {
		t.Fatal(err)
	}
	postgres, mysql, registry, err := runnerharness.DrivenImages(driver)
	if err != nil {
		t.Fatal(err)
	}
	for image, family := range map[string]string{postgres: "postgres:17", mysql: "mysql:8.4", registry: "registry:"} {
		if !strings.HasPrefix(image, family) || !strings.Contains(image, "@sha256:") {
			t.Errorf("read %q, want %s by digest", image, family)
		}
	}
	const missing = `E2E_POSTGRES_SOURCE_IMAGE=${E2E_POSTGRES_SOURCE_IMAGE:-postgres:17}`
	if _, _, _, err := runnerharness.DrivenImages([]byte(missing)); err == nil {
		t.Fatal("a driver with no digest was accepted")
	}
}

// Write mode leaves a document alone when only its volatile values moved, so a
// repin's diff holds what the release changed. It rewrites one that changed
// shape, and it removes a capture no row produces.
func TestWriteCapturesChangesOnlyWhatChangedShape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	row := func(document string) runnerharness.Captures {
		return runnerharness.Captures{
			Rows: []runnerharness.Row{{
				Name: "a-row", Operation: "resolve", Source: runnerharness.SourceOCI,
				Children: []runnerharness.Child{{Command: "oci resolve", Args: []string{"oci", "resolve"}, Stdout: "a-row/stdout.json"}},
			}},
			Documents: map[string][]byte{"a-row/stdout.json": []byte(document)},
		}
	}
	const first = `{"applied_at":"2026-01-01T00:00:00Z","contract_version":1}`
	write := func(fresh runnerharness.Captures) {
		t.Helper()
		committed, err := runnerharness.LoadCaptures(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := runnerharness.WriteCaptures(root, runnerharness.NewMasker(), committed, fresh); err != nil {
			t.Fatal(err)
		}
	}
	stored := func() string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, "a-row", "stdout.json"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	write(row(first))
	if got := stored(); got != first {
		t.Fatalf("stored %s", got)
	}
	write(row(`{"applied_at":"2027-02-02T00:00:00Z","contract_version":1}`))
	if got := stored(); got != first {
		t.Fatalf("a time alone rewrote the document: %s", got)
	}
	const moved = `{"applied_at":"2027-02-02T00:00:00Z","contract_version":2}`
	write(row(moved))
	if got := stored(); got != moved {
		t.Fatalf("a contract version did not rewrite the document: %s", got)
	}
	stray := filepath.Join(root, "another-row", "stdout.json")
	if err := os.MkdirAll(filepath.Dir(stray), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	write(row(moved))
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("a capture no row produces stayed: %v", err)
	}
	reread, err := runnerharness.LoadCaptures(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(reread.Rows) != 1 || reread.Rows[0].Name != "a-row" || string(reread.Documents["a-row/stdout.json"]) != moved {
		t.Fatalf("read back %+v", reread)
	}
}
