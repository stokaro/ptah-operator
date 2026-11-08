package runnerharness_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/test/runnerharness"
)

func capturedDocument(t *testing.T, file string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(runnerharness.CapturesDirectory), file))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func kinds(differences []runnerharness.Difference) []string {
	var out []string
	for _, difference := range differences {
		out = append(out, difference.Path+" "+difference.Kind)
	}
	return out
}

// A change of shape is refused wherever it is. The base is what Ptah printed for
// a target it could not open and for a plan, and each mutation is one a release
// could make.
func TestCompareDocumentsRefusesAChangeOfShape(t *testing.T) {
	t.Parallel()
	plan := string(capturedDocument(t, "schema-plan-report.json"))
	for _, row := range []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{
			name: "a field added",
			mutate: func(document string) string {
				return strings.Replace(document, `"outcome":`, `"later": 1, "outcome":`, 1)
			},
			want: "$.later added",
		},
		{
			name: "a field added inside a statement",
			mutate: func(document string) string {
				return strings.Replace(document, `"severity":`, `"owned": true, "severity":`, 1)
			},
			want: "$.plan.statements[0].owned added",
		},
		{
			name:   "a field removed",
			mutate: func(document string) string { return strings.Replace(document, `"command": "schema plan",`, "", 1) },
			want:   "$.command removed",
		},
		{
			name: "a field retyped",
			mutate: func(document string) string {
				return strings.Replace(document, `"destructive": false`, `"destructive": "false"`, 1)
			},
			want: "$.plan.destructive retyped",
		},
		{
			name: "a field that became null",
			mutate: func(document string) string {
				return strings.Replace(document, `"outcome": "changes"`, `"outcome": null`, 1)
			},
			want: "$.outcome retyped",
		},
		{
			name: "the contract version",
			mutate: func(document string) string {
				return strings.Replace(document, `"contract_version": 3`, `"contract_version": 4`, 1)
			},
			want: "$.contract_version changed",
		},
		{
			name: "a value",
			mutate: func(document string) string {
				return strings.Replace(document, `"severity": "safe"`, `"severity": "warning"`, 1)
			},
			want: "$.plan.statements[0].severity changed",
		},
		{
			name: "a statement removed",
			mutate: func(document string) string {
				var value map[string]any
				if err := json.Unmarshal([]byte(document), &value); err != nil {
					t.Fatal(err)
				}
				value["plan"].(map[string]any)["statements"] = []any{}
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				return string(encoded)
			},
			want: "$.plan.statements[0] removed",
		},
		{
			name:   "not a document at all",
			mutate: func(string) string { return "error: it failed\n" },
			want:   "$ retyped",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			mutated := row.mutate(plan)
			if mutated == plan {
				t.Fatal("the mutation changed nothing")
			}
			got := kinds(runnerharness.CompareDocuments(runnerharness.NewMasker(), []byte(plan), []byte(mutated)))
			if !slices.Contains(got, row.want) {
				t.Fatalf("differences %q, want %q", got, row.want)
			}
		})
	}
}

// A volatile value may change. Its type may not, and the mask reaches no further
// than what it declares.
func TestCompareDocumentsIgnoresOnlyTheVolatileValues(t *testing.T) {
	t.Parallel()
	aDigest, anotherDigest := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	base := `{"contract_version":2,"applied_at":"2026-11-04T15:42:49Z","execution_time_ms":12,` +
		`"message":"dial tcp 172.20.0.2:5432: refused at 2026-11-04T15:42:49.5Z","path":"/work/ptah-plan-output-123456/first.plan.json",` +
		`"digest":"` + aDigest + `","reference":"oci://registry/a@` + aDigest + `","plan_digest":"` + aDigest + `",` +
		`"from_fingerprint":"` + aDigest + `","content_digest":"` + aDigest + `","log":"correlation_id=0a1b2c level=INFO",` +
		`"refusal":"\"/proc/14/environ\" is outside allowed root"}`
	for _, row := range []struct {
		name, document string
		accepted       bool
	}{
		{"the same document", base, true},
		{"another time under a volatile key", strings.Replace(base, `"applied_at":"2026-11-04T15:42:49Z"`, `"applied_at":"2027-01-01T00:00:00Z"`, 1), true},
		{"another duration", strings.Replace(base, `"execution_time_ms":12`, `"execution_time_ms":40`, 1), true},
		{"another address", strings.Replace(base, "172.20.0.2:5432", "10.1.2.3:5432", 1), true},
		{"another time in a sentence", strings.Replace(base, "15:42:49.5Z", "16:00:00Z", 1), true},
		{"another temporary name", strings.Replace(base, "ptah-plan-output-123456", "ptah-plan-output-99887766", 1), true},
		{"another artifact digest", strings.Replace(base, `"digest":"`+aDigest, `"digest":"`+anotherDigest, 1), true},
		{"another artifact in a reference", strings.Replace(base, "registry/a@"+aDigest, "registry/a@"+anotherDigest, 1), true},
		{"another correlation id", strings.Replace(base, "correlation_id=0a1b2c", "correlation_id=99ff", 1), true},
		{"another process id", strings.Replace(base, "/proc/14/environ", "/proc/15/environ", 1), true},
		{"another file of the process", strings.Replace(base, "/proc/14/environ", "/proc/14/cmdline", 1), false},
		{"a process named where its id was", strings.Replace(base, "/proc/14/environ", "/proc/self/environ", 1), false},
		{"another repository in a reference", strings.Replace(base, "registry/a@", "registry/b@", 1), false},
		{"another plan digest", strings.Replace(base, `"plan_digest":"`+aDigest, `"plan_digest":"`+anotherDigest, 1), false},
		{"another fingerprint", strings.Replace(base, `"from_fingerprint":"`+aDigest, `"from_fingerprint":"`+anotherDigest, 1), false},
		{"another content digest", strings.Replace(base, `"content_digest":"`+aDigest, `"content_digest":"`+anotherDigest, 1), false},
		{"a volatile key retyped", strings.Replace(base, `"execution_time_ms":12`, `"execution_time_ms":"12"`, 1), false},
		{"a volatile key removed", strings.Replace(base, `"execution_time_ms":12,`, "", 1), false},
		{"the words around an address", strings.Replace(base, "dial tcp", "dialing tcp", 1), false},
		{"another file in a temporary directory", strings.Replace(base, "first.plan.json", "second.plan.json", 1), false},
		{"a contract version that looks like a number", strings.Replace(base, `"contract_version":2`, `"contract_version":3`, 1), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			differences := runnerharness.CompareDocuments(runnerharness.NewMasker(), []byte(base), []byte(row.document))
			if row.accepted != (len(differences) == 0) {
				t.Fatalf("differences %q, accepted = %v", kinds(differences), row.accepted)
			}
		})
	}
}

// A row's whole record is compared, so an exit status, an argument, a line of
// standard error and the frame's payload are held as a document's fields are.
func TestDecodeDocumentReadsAVerificationItsOwnWay(t *testing.T) {
	t.Parallel()
	args := []string{"oci", "verify", "oci://registry/schema@sha256:" + strings.Repeat("a", 64), "--format", "json"}
	reference := args[2]
	report := fmt.Sprintf(`{"reference":%q,"digest":%q,"satisfied":[],"findings":[{"requirement":"artifact_types","detail":"refused"}]}`,
		reference, "sha256:"+strings.Repeat("a", 64))
	for _, test := range []struct {
		name   string
		exit   int
		stdout string
		want   bool
	}{
		{"a refusal's report", 2, report, true},
		{"a verification that could not run", 2, "", true},
		{"a success with no report", 0, "", false},
		{"a failure with half a report", 2, `{"reference":`, false},
		{"a failure with another document", 2, `{"findings":[]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := runnerharness.DecodeDocument(args, test.exit, []byte(test.stdout))
			if (err == nil) != test.want {
				t.Fatalf("DecodeDocument() = %v, want accepted %t", err, test.want)
			}
		})
	}
}

func TestCompareCapturesRefusesAChangedRecord(t *testing.T) {
	t.Parallel()
	row := func(mutate func(*runnerharness.Row)) runnerharness.Captures {
		value := runnerharness.Row{
			Name: "a-row", Operation: "resolve", Source: runnerharness.SourceGit,
			Children: []runnerharness.Child{{
				Command: "schema pull", Args: []string{"schema", "pull", "git+https://host/a.git"},
				Exit: 2, Stderr: "refusing to read a submodule at db/vendor\n", Stdout: "a-row/stdout",
			}},
			Result: map[string]any{"protocolVersion": json.Number("1"), "error": map[string]any{"code": "child_exit"}},
		}
		if mutate != nil {
			mutate(&value)
		}
		return runnerharness.Captures{Rows: []runnerharness.Row{value}, Documents: map[string][]byte{"a-row/stdout": []byte(`{"contract_version":1}`)}}
	}
	base := row(nil)
	for _, test := range []struct {
		name   string
		mutate func(*runnerharness.Row)
		want   string
	}{
		{"an exit status", func(value *runnerharness.Row) { value.Children[0].Exit = 3 }, "$.children[0].exit changed"},
		{"a refusal line", func(value *runnerharness.Row) { value.Children[0].Stderr = "the source is refused\n" }, "$.children[0].stderr changed"},
		{"an argument", func(value *runnerharness.Row) { value.Children[0].Args[2] = "--json" }, "$.children[0].args[2] changed"},
		{"an argument added", func(value *runnerharness.Row) { value.Children[0].Args = append(value.Children[0].Args, "--json") }, "$.children[0].args[3] added"},
		{"a child added", func(value *runnerharness.Row) {
			value.Children = append(value.Children, runnerharness.Child{Command: "oci inspect"})
		}, "$.children[1] added"},
		{"a result code", func(value *runnerharness.Row) {
			value.Result = map[string]any{"protocolVersion": json.Number("1"), "error": map[string]any{"code": "invalid_git_access"}}
		}, "$.result.error.code changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, change := range runnerharness.CompareCaptures(runnerharness.NewMasker(), base, row(test.mutate)) {
				got = append(got, change.Difference.Path+" "+change.Difference.Kind)
			}
			if !slices.Contains(got, test.want) {
				t.Fatalf("changes %q, want %q", got, test.want)
			}
		})
	}
	t.Run("a document", func(t *testing.T) {
		t.Parallel()
		mutated := row(nil)
		mutated.Documents["a-row/stdout"] = []byte(`{"contract_version":2}`)
		changes := runnerharness.CompareCaptures(runnerharness.NewMasker(), base, mutated)
		if len(changes) != 1 || !strings.Contains(changes[0].String(), "contract_version") {
			t.Fatalf("changes %v", changes)
		}
	})
	t.Run("a row added and a row removed", func(t *testing.T) {
		t.Parallel()
		added := row(nil)
		added.Rows = append(added.Rows, runnerharness.Row{Name: "b-row"})
		if changes := runnerharness.CompareCaptures(runnerharness.NewMasker(), base, added); len(changes) != 1 || changes[0].Row != "b-row" {
			t.Fatalf("changes %v", changes)
		}
		if changes := runnerharness.CompareCaptures(runnerharness.NewMasker(), added, base); len(changes) != 1 || changes[0].Row != "b-row" {
			t.Fatalf("changes %v", changes)
		}
	})
	t.Run("the same record", func(t *testing.T) {
		t.Parallel()
		if changes := runnerharness.CompareCaptures(runnerharness.NewMasker(), base, row(nil)); len(changes) != 0 {
			t.Fatalf("changes %v", changes)
		}
	})
}

// What an OCI pull prints is compared as text, with the digest it names masked
// and every other word held.
func TestCompareDocumentsReadsAPullsText(t *testing.T) {
	t.Parallel()
	aDigest, anotherDigest := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	printed := func(digest, out string) []byte {
		return []byte("Pulled oci://registry/a@" + digest + " to " + out + "\nDigest: " + digest + "\n")
	}
	if differences := runnerharness.CompareDocuments(runnerharness.NewMasker(), printed(aDigest, "/source"), printed(anotherDigest, "/source")); len(differences) != 0 {
		t.Fatalf("another digest: %q", kinds(differences))
	}
	if differences := runnerharness.CompareDocuments(runnerharness.NewMasker(), printed(aDigest, "/source"), printed(aDigest, "/elsewhere")); len(differences) == 0 {
		t.Fatal("another path was accepted")
	}
}

func TestCompareLinesNamesTheLine(t *testing.T) {
	t.Parallel()
	differences := runnerharness.CompareLines(runnerharness.NewMasker(), "first\nsecond\n", "first\nchanged\n")
	if got := kinds(differences); !slices.Equal(got, []string{"line 2 changed"}) {
		t.Fatalf("differences %q", got)
	}
	if differences := runnerharness.CompareLines(runnerharness.NewMasker(), "one", "one\ntwo"); len(differences) != 1 || differences[0].Kind != "added" {
		t.Fatalf("differences %+v", differences)
	}
	if differences := runnerharness.CompareLines(runnerharness.NewMasker(), "one\ntwo", "one"); len(differences) != 1 || differences[0].Kind != "removed" {
		t.Fatalf("differences %+v", differences)
	}
}

// A rule that masks nothing is a rule nothing needs any more, and the run says
// so.
func TestMaskerNamesTheRulesThatMaskedNothing(t *testing.T) {
	t.Parallel()
	masker := runnerharness.NewMasker()
	if len(masker.UnusedRules()) == 0 {
		t.Fatal("a fresh masker reports every rule unused")
	}
	masker.Text("dial tcp 10.0.0.1:5432")
	if unused := masker.UnusedRules(); slices.Contains(unused, "address") {
		t.Fatalf("unused %q still names the rule that matched", unused)
	}
}

func TestScrubRemovesOnlyTheFieldsThatNameTheMachine(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, document string
	}{
		{"first", "{\n  \"database_url\": \"postgres://user:secret@host/db\",\n  \"drift\": true\n}\n"},
		{"in the middle", "{\n  \"drift\": true,\n  \"sources\": [\n    \"/source/schema.hcl\"\n  ],\n  \"database_url\": \"postgres://user:secret@host/db\",\n  \"failed\": false\n}\n"},
		{"last", "{\n  \"drift\": true,\n  \"sources\": [\n    \"/source/schema.hcl\"\n  ]\n}\n"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			scrubbed, err := runnerharness.Scrub([]byte(row.document))
			if err != nil {
				t.Fatal(err)
			}
			if !json.Valid(scrubbed) || strings.Contains(string(scrubbed), "secret") || strings.Contains(string(scrubbed), "sources") {
				t.Fatalf("scrubbed to %s", scrubbed)
			}
			if !strings.Contains(string(scrubbed), `"drift": true`) {
				t.Fatalf("lost the report: %s", scrubbed)
			}
		})
	}
	for _, document := range []string{`{"drift":true}`, "not a document", ""} {
		scrubbed, err := runnerharness.Scrub([]byte(document))
		if err != nil || string(scrubbed) != document {
			t.Fatalf("Scrub(%q) = %q, %v", document, scrubbed, err)
		}
	}
	if _, err := runnerharness.Scrub([]byte(`{"sources":["a"],"drift":true}`)); err == nil {
		t.Fatal("a document printed some other way kept the field and was accepted")
	}
}
