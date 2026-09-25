package main

import (
	"strings"
	"testing"
	"time"
)

// The binding exists because a recording names a scenario by id, and an id
// survives any edit to the commands underneath it. These hold the digest to
// covering what a reader watches and ignoring how it is described.

func sampleScenario() scenario {
	return scenario{
		ID:      "first-apply",
		Title:   "A first apply",
		Tagline: "what the operator does with a new schema",
		Learn:   "the approval gate",
		Tags:    []string{"schema"},
		Reset:   []string{"kubectl delete ptahschema --all"},
		Steps: []step{{
			Note:  "publish the artifact",
			Run:   "kubectl apply -f schema.yaml",
			Retry: 30 * time.Second,
			Sync:  "Planning",
			Show:  []string{"stdout"},
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
