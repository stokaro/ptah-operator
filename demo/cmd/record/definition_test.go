package main

import (
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The binding exists because a recording names a scenario by id, and an id
// survives any edit to the commands underneath it. These hold the digest to
// covering what a reader watches and ignoring how it is described.

// sampleScenario allocates every pointer afresh on each call, the way two
// readings of one file do. Its await and expectation hold the pointer fields a
// real scenario has, because a digest that read an address instead of a value
// passes every row that has none.
func sampleScenario() scenario {
	return scenario{
		ID:      "first-apply",
		Title:   "A first apply",
		Tagline: "what the operator does with a new schema",
		Learn:   "the approval gate",
		Tags:    []string{"schema"},
		Reset:   []string{"kubectl delete ptahschema --all"},
		Steps: []step{{
			Note: "publish the artifact",
			Await: &observation{
				Kind:    "ptahschema",
				Name:    "storefront",
				Timeout: 6 * time.Minute,
				Condition: &condition{
					Type:   "InSync",
					Status: "True",
					Reason: "Converged",
				},
			},
			Run:   "kubectl apply -f schema.yaml",
			Retry: 30 * time.Second,
			Sync:  "Planning",
			Show:  []string{"stdout"},
			Expect: &expectation{
				Exit:           new(0),
				StdoutContains: []string{"storefront"},
				Resource: &observation{
					Kind:   "ptahschema",
					Name:   "storefront",
					Fields: []field{{Path: ".status.phase", Equals: "Ready"}},
				},
			},
		}},
	}
}

func TestTheDigestChangesWithWhatRan(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		edit func(*scenario)
	}{
		{"a changed command", func(s *scenario) { s.Steps[0].Run = "kubectl apply -f other.yaml" }},
		{"a changed reset", func(s *scenario) { s.Reset = []string{"true"} }},
		{"a changed retry window", func(s *scenario) { s.Steps[0].Retry = time.Minute }},
		{"a changed phase pill", func(s *scenario) { s.Steps[0].Sync = "Applying" }},
		{"a stream that now reaches the transcript", func(s *scenario) { s.Steps[0].Show = []string{"stdout", "stderr"} }},
		{"a step appended", func(s *scenario) { s.Steps = append(s.Steps, step{Run: "kubectl get ptahschema"}) }},
		{"a different scenario entirely", func(s *scenario) { s.ID = "drift" }},
		{"another exit status expected", func(s *scenario) { s.Steps[0].Expect.Exit = new(1) }},
		{"no exit status expected", func(s *scenario) { s.Steps[0].Expect.Exit = nil }},
		{"more output required", func(s *scenario) {
			s.Steps[0].Expect.StdoutContains = append(s.Steps[0].Expect.StdoutContains, "Ready")
		}},
		{"stderr now required", func(s *scenario) { s.Steps[0].Expect.StderrContains = []string{"refused"} }},
		{"output now forbidden", func(s *scenario) { s.Steps[0].Expect.StdoutAbsent = []string{"error"} }},
		{"a field read for another value", func(s *scenario) { s.Steps[0].Expect.Resource.Fields[0].Equals = "Blocked" }},
		{"a field only required to be set", func(s *scenario) {
			s.Steps[0].Expect.Resource.Fields[0] = field{Path: ".status.phase", NotEmpty: true}
		}},
		{"a resource now expected absent", func(s *scenario) {
			s.Steps[0].Expect.Resource = &observation{Kind: "ptahschema", Name: "storefront", Absent: true}
		}},
		{"no resource read", func(s *scenario) { s.Steps[0].Expect.Resource = nil }},
		{"another reason awaited", func(s *scenario) { s.Steps[0].Await.Condition.Reason = "ScopedConverged" }},
		{"the current generation awaited", func(s *scenario) { s.Steps[0].Await.Generation = "current" }},
		{"an observed generation awaited", func(s *scenario) { s.Steps[0].Await.Condition.ObservedGeneration = new(int64(2)) }},
		{"a longer wait", func(s *scenario) { s.Steps[0].Await.Timeout = 7 * time.Minute }},
		{"another object awaited", func(s *scenario) { s.Steps[0].Await.Name = "inventory" }},
		{"no wait at all", func(s *scenario) { s.Steps[0].Await = nil }},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			before := definitionDigest(sampleScenario())
			edited := sampleScenario()
			row.edit(&edited)
			if definitionDigest(edited) == before {
				t.Fatal("the digest did not change, so a recording would go on claiming to represent this")
			}
		})
	}
}

// Rewording a sentence does not make an old transcript a lie about what ran,
// so it must not invalidate one. A digest that moved on every edit would be
// re-recorded away and stop meaning anything.
func TestTheDigestIgnoresHowTheScenarioIsDescribed(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		edit func(*scenario)
	}{
		{"a reworded title", func(s *scenario) { s.Title = "Applying a schema for the first time" }},
		{"a reworded tagline", func(s *scenario) { s.Tagline = "the first apply, end to end" }},
		{"a reworded lesson", func(s *scenario) { s.Learn = "why approval comes first" }},
		{"a new tag", func(s *scenario) { s.Tags = append(s.Tags, "getting-started") }},
		{"reworded narration", func(s *scenario) { s.Steps[0].Note = "publish it" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			before := definitionDigest(sampleScenario())
			edited := sampleScenario()
			row.edit(&edited)
			if definitionDigest(edited) != before {
				t.Fatal("the digest moved for a change to how the scenario reads, not to what it ran")
			}
		})
	}
}

func TestStaleRecordingsSeparatesUncheckedFromDisagreeing(t *testing.T) {
	t.Parallel()

	current := []scenario{sampleScenario()}
	matching := recording{ID: "first-apply", DefinitionDigest: definitionDigest(sampleScenario())}

	if findings := staleRecordings(runRecord{Scenarios: []recording{matching}}, current); len(findings) != 0 {
		t.Fatalf("a recording of the current definition was reported stale: %v", findings)
	}

	for _, row := range []struct {
		name    string
		record  recording
		finding string
	}{
		{
			// The distinction the whole binding is for: nobody checked is not
			// the same as checked and agreed.
			name:    "a recording made before the digest existed",
			record:  recording{ID: "first-apply"},
			finding: "carries no definition digest",
		},
		{
			name:    "a scenario changed under its recording",
			record:  recording{ID: "first-apply", DefinitionDigest: "sha256:0000"},
			finding: "changed under the recording",
		},
		{
			name:    "a scenario this tree no longer declares",
			record:  recording{ID: "retired", DefinitionDigest: "sha256:0000"},
			finding: "no longer declares",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			findings := staleRecordings(runRecord{Scenarios: []recording{row.record}}, current)
			if len(findings) != 1 || !strings.Contains(findings[0], row.finding) {
				t.Fatalf("findings were %v", findings)
			}
		})
	}
}

// The recorder writes the digest in one process and -verify computes it again
// in another. Two readings of the same directory are the smallest form of that:
// each allocates its own pointers, so a digest that read an address differs
// between them. That is how every recording on master came to be reported as
// changed.
func TestTheDigestIsOfTheFileNotOfTheReading(t *testing.T) {
	t.Parallel()

	directory := filepath.Join("..", "..", "scenarios")
	first, err := loadScenarios(directory)
	if err != nil {
		t.Fatalf("loadScenarios: %v", err)
	}
	second, err := loadScenarios(directory)
	if err != nil {
		t.Fatalf("loadScenarios: %v", err)
	}
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("read %d and then %d scenarios", len(first), len(second))
	}
	for index := range first {
		if first[index].ID != second[index].ID {
			t.Fatalf("the two readings disagree on order: %s and %s", first[index].ID, second[index].ID)
		}
		if definitionDigest(first[index]) != definitionDigest(second[index]) {
			t.Errorf("%s: two readings of one file digest differently", first[index].ID)
		}
	}
}

// An empty block is something the file says, and no block is not. The
// validator refuses both today; the digest does not lean on that.
func TestTheDigestKeepsAnEmptyBlockApartFromNone(t *testing.T) {
	t.Parallel()

	without := scenario{ID: "probe", Steps: []step{{Run: "true"}}}
	with := scenario{ID: "probe", Steps: []step{{Run: "true", Await: &observation{}}}}
	if definitionDigest(without) == definitionDigest(with) {
		t.Fatal("await: {} and no await digest alike")
	}
}

// writeDeclared writes pointers, structs, slices, strings, booleans and
// integers, and panics on anything else. Walking the type rather than a value
// finds a field of another kind before any scenario sets it, and pins which
// fields are left out as narration: a digest:"-" on a command would make every
// edit to it invisible.
func TestTheDigestCanWriteEveryScenarioField(t *testing.T) {
	t.Parallel()

	plainKey := regexp.MustCompile(`^[a-z][A-Za-z_]*$`)
	visited := map[string]bool{}
	var excluded []string
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice:
			walk(path, typ.Elem())
		case reflect.Struct:
			if visited[typ.Name()] {
				return
			}
			visited[typ.Name()] = true
			keys := map[string]bool{}
			for index := range typ.NumField() {
				declared := typ.Field(index)
				key := yamlKey(declared)
				where := typ.Name() + "." + key
				if !declared.IsExported() {
					t.Errorf("%s.%s is unexported, so the decoder never fills it", typ.Name(), declared.Name)
				}
				if !plainKey.MatchString(key) {
					t.Errorf("%s.%s is written as %q, which is not a plain YAML key", typ.Name(), declared.Name, key)
				}
				if keys[key] {
					t.Errorf("%s is the key of two fields", where)
				}
				keys[key] = true
				if declared.Tag.Get("digest") == "-" {
					excluded = append(excluded, where)
					continue
				}
				walk(where, declared.Type)
			}
		case reflect.String, reflect.Bool,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		default:
			t.Errorf("%s is a %s, which the digest cannot write", path, typ.Kind())
		}
	}
	walk("scenario", reflect.TypeFor[scenario]())

	for _, name := range []string{"scenario", "step", "observation", "condition", "field", "expectation"} {
		if !visited[name] {
			t.Errorf("the walk never reached %s, so it checked less than the digest writes", name)
		}
	}
	slices.Sort(excluded)
	narration := []string{"scenario.learn", "scenario.tagline", "scenario.tags", "scenario.title", "step.note"}
	if !slices.Equal(excluded, narration) {
		t.Errorf("the digest leaves out %v; only the narration %v may be left out", excluded, narration)
	}
}

// -verify is the gate, so it is tested the way the gate runs it: a run record
// on disk, the scenario directory read afresh, and the answer on the error
// path. Every row writes the record from one reading and verifies against
// another, which is the shape that failed on master. The rows that must be
// accepted are what make the refusals mean anything: a -verify that refused
// every recording would pass all of the others.
func TestVerifyAcceptsAnUnchangedRecordingAndRefusesAChangedScenario(t *testing.T) {
	t.Parallel()

	const declared = `id: verify-probe
title: A probe
tagline: One step, waited for and checked.
learn: What -verify compares.
tags: [Lifecycle]
steps:
  - note: Wait for the schema to converge, then read it.
    await:
      kind: ptahschema
      name: storefront
      timeout: 6m
      condition:
        type: InSync
        status: "True"
        reason: Converged
    run: kubectl get ptahschema storefront
    expect:
      exit: 0
      stdout_contains: [storefront]
`
	for _, row := range []struct {
		name     string
		from, to string
		refused  bool
	}{
		{name: "the scenario as recorded"},
		{name: "reworded narration", from: "then read it.", to: "then print it."},
		{name: "a reworded title", from: "title: A probe", to: "title: A small probe"},
		{name: "another exit status", from: "exit: 0", to: "exit: 1", refused: true},
		{name: "another awaited reason", from: "reason: Converged", to: "reason: ScopedConverged", refused: true},
		{name: "a longer wait", from: "timeout: 6m", to: "timeout: 7m", refused: true},
		{name: "other output required", from: "[storefront]", to: "[inventory]", refused: true},
		{name: "another command", from: "get ptahschema storefront", to: "get ptahschema", refused: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			directory := t.TempDir()
			scenarioFile := filepath.Join(directory, "verify-probe.yaml")
			write(t, scenarioFile, declared)
			recorded, err := loadScenarios(directory)
			if err != nil {
				t.Fatalf("loadScenarios: %v", err)
			}
			recordFile := filepath.Join(directory, "runs.json")
			record := runRecord{Scenarios: []recording{{
				ID:               "verify-probe",
				DefinitionDigest: definitionDigest(recorded[0]),
			}}}
			if err := writeRecord(recordFile, record); err != nil {
				t.Fatalf("writeRecord: %v", err)
			}
			if row.from != "" {
				if !strings.Contains(declared, row.from) {
					t.Fatalf("the scenario holds no %q, so the row edits nothing", row.from)
				}
				write(t, scenarioFile, strings.Replace(declared, row.from, row.to, 1))
			}

			var diagnostics strings.Builder
			err = run([]string{"-scenarios", directory, "-verify", recordFile}, &diagnostics)
			switch {
			case !row.refused && err != nil:
				t.Fatalf("a recording of this definition was refused: %v\n%s", err, diagnostics.String())
			case !row.refused:
				if !strings.Contains(diagnostics.String(), "still represent their scenarios") {
					t.Fatalf("accepted without saying so:\n%s", diagnostics.String())
				}
			case err == nil:
				t.Fatalf("a recording of another definition was accepted:\n%s", diagnostics.String())
			case !strings.Contains(diagnostics.String(), "verify-probe: the scenario changed under the recording"):
				t.Fatalf("refused for another reason: %v\n%s", err, diagnostics.String())
			}
		})
	}
}
