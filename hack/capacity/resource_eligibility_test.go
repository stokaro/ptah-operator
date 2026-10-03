package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func nativeSuspendedFreshness(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile("testdata/retention-suspended-freshness.json")
	if err != nil {
		t.Fatal(err)
	}
	o := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw, o); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestFreshnessRetainsNativeSuspensionAndEligiblePopulation(t *testing.T) {
	o := nativeSuspendedFreshness(t)
	at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	paused, err := readResourceFreshness("schema", o, at, []string{"status", "target", "lastObservedAt"})
	if err != nil {
		t.Fatal(err)
	}
	if !paused.Suspended || paused.UID != string(o.GetUID()) || paused.ObservedAt == nil || paused.NextReconciliationTime == nil {
		t.Fatalf("lost native reading: %+v", paused)
	}
	active := paused
	active.Name = "active"
	active.UID = "active-uid"
	active.Suspended = false
	observed, next := at.Add(-30*time.Second), at.Add(-5*time.Second)
	active.ObservedAt = &observed
	active.NextReconciliationTime = &next
	s := sample{At: at, ResourceReadStartedAt: at, ResourceReadFinishedAt: at, Resources: 2, ResourceFreshness: []resourceFreshness{paused, active}}
	c := cost(window{Start: at.Add(-time.Second), End: at.Add(time.Second)}, []sample{s}, nil).EligibleFreshness
	if c == nil || c.EligibleReadings != 1 || c.SuspendedReadings != 1 || c.ObservationAgeMaxSeconds == nil || *c.ObservationAgeMaxSeconds != 30 || c.OverdueMaxSeconds == nil || *c.OverdueMaxSeconds != 5 {
		t.Fatalf("suspension inflated eligible bounds: %+v", c)
	}
	// Round-trip the report inputs, then recompute: serialized evidence carries
	// the exclusion and timestamps, not only an already reduced maximum.
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var restored sample
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	got := eligibleFreshness([]sample{restored})
	if got == nil || *got.OverdueMaxSeconds != 5 {
		t.Fatal(got)
	}
}

func TestEligibleFreshnessCannotPassMissingOrEmptyEvidence(t *testing.T) {
	at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	observed, next := at.Add(-time.Minute), at.Add(time.Minute)
	row := resourceFreshness{Family: "schema", Namespace: "work", Name: "schema", UID: "uid", ResourceVersion: "1", Generation: 1, ReadAt: at, ObservedAt: &observed, NextReconciliationTime: &next}
	for _, mode := range []string{"complete", "older report", "partial population", "failed list", "duplicate", "suspended", "deleting", "no observation", "no deadline", "future observation", "zero resources"} {
		t.Run(mode, func(t *testing.T) {
			s := sample{At: at, Resources: 1, ResourceFreshness: []resourceFreshness{row}}
			wantNil := false
			switch mode {
			case "older report":
				s.ResourceFreshness = nil
				wantNil = true
			case "partial population":
				s.Resources = 2
				wantNil = true
			case "failed list":
				s.Incomplete = []string{sourceResources}
				wantNil = true
			case "duplicate":
				s.Resources = 2
				s.ResourceFreshness = append(s.ResourceFreshness, row)
				wantNil = true
			case "suspended":
				s.ResourceFreshness[0].Suspended = true
			case "deleting":
				s.ResourceFreshness[0].Deleting = true
			case "no observation":
				s.ResourceFreshness[0].ObservedAt = nil
			case "no deadline":
				s.ResourceFreshness[0].NextReconciliationTime = nil
			case "future observation":
				s.ResourceFreshness[0].ObservedAt = &next
			case "zero resources":
				s.Resources = 0
				s.ResourceFreshness = []resourceFreshness{}
			}
			c := eligibleFreshness([]sample{s})
			if (c == nil) != wantNil {
				t.Fatalf("cost %+v, nil=%v", c, wantNil)
			}
			if c == nil {
				return
			}
			missingAge := mode == "suspended" || mode == "deleting" || mode == "no observation" || mode == "future observation" || mode == "zero resources"
			missingDue := mode == "suspended" || mode == "deleting" || mode == "no deadline" || mode == "zero resources"
			if (c.ObservationAgeMaxSeconds == nil) != missingAge || (c.OverdueMaxSeconds == nil) != missingDue {
				t.Fatal(c)
			}
		})
	}
	if eligibleFreshness(nil) != nil {
		t.Fatal("no samples became evidence")
	}
}

func TestFreshnessRejectsMalformedInputs(t *testing.T) {
	for _, mode := range []string{"no uid", "no resource version", "no generation", "invalid suspend", "invalid observed", "wrong timestamp type"} {
		t.Run(mode, func(t *testing.T) {
			o := nativeSuspendedFreshness(t)
			switch mode {
			case "no uid":
				o.SetUID("")
			case "no resource version":
				o.SetResourceVersion("")
			case "no generation":
				o.SetGeneration(0)
			case "invalid suspend":
				_ = unstructured.SetNestedField(o.Object, "false", "spec", "suspend")
			case "invalid observed":
				_ = unstructured.SetNestedField(o.Object, "bad", "status", "target", "lastObservedAt")
			case "wrong timestamp type":
				_ = unstructured.SetNestedField(o.Object, true, "status", "nextReconciliationTime")
			}
			if _, err := readResourceFreshness("schema", o, time.Now(), []string{"status", "target", "lastObservedAt"}); err == nil {
				t.Fatal("accepted malformed evidence")
			}
		})
	}
}

func TestSamplerRetainsFreshnessWithoutExcludingApprovalGates(t *testing.T) {
	o := nativeSuspendedFreshness(t)
	o.SetNamespace("work")
	o.SetDeletionTimestamp(nil)
	_ = unstructured.SetNestedField(o.Object, false, "spec", "suspend")
	_ = unstructured.SetNestedField(o.Object, "AwaitingApproval", "status", "phase")
	s := measurementFixture(t, "", completeProcessMetrics, func(path string, list map[string]any) {
		if strings.HasSuffix(path, "/ptahschemas") {
			list["items"] = []any{o.Object}
		}
	})
	var got sample
	if err := s.readResources(context.Background(), time.Now().UTC(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.ResourceFreshness) != 1 || got.ResourceFreshness[0].Suspended || got.ResourceFreshness[0].UID != string(o.GetUID()) {
		t.Fatal(got)
	}
	if c := eligibleFreshness([]sample{got}); c == nil || c.EligibleReadings != 1 {
		t.Fatal(c)
	}
	o.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	r, err := readResourceFreshness("schema", o, time.Now(), []string{"status", "target", "lastObservedAt"})
	if err != nil || !r.Deleting {
		t.Fatal(r, err)
	}
}

func TestFreshnessSummaryPreservesUnavailableBounds(t *testing.T) {
	var text strings.Builder
	if err := writeSummary(&text, report{Scenarios: []scenarioCost{
		{window: window{Name: "older"}},
		{window: window{Name: "paused"}, EligibleFreshness: &freshnessCost{SuspendedReadings: 20}},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{
		"| older | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a |",
		"| paused | 0 | 20 | 0 | 0 | 0 | 0 | 0 | n/a | n/a | n/a |",
	} {
		if strings.Count(text.String(), row) != 1 {
			t.Fatalf("missing or duplicated freshness row %q", row)
		}
	}
}

func nativeActiveFreshness(t *testing.T, family string) (*unstructured.Unstructured, resourceFreshness) {
	t.Helper()
	raw, err := os.ReadFile("testdata/soak-active-" + family + "-freshness.json")
	if err != nil {
		t.Fatal(err)
	}
	o := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw, o); err != nil {
		t.Fatal(err)
	}
	started, _, err := unstructured.NestedString(o.Object, "status", "activeOperation", "startedAt")
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		t.Fatal(err)
	}
	path := []string{"status", "target", "lastObservedAt"}
	if family == "migration" {
		path = []string{"status", "history", "observedAt"}
	}
	r, err := readResourceFreshness(family, o, at.Add(17*time.Second), path)
	if err != nil {
		t.Fatal(err)
	}
	return o, r
}

func TestFreshnessRetainsNativeActiveClaims(t *testing.T) {
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			_, r := nativeActiveFreshness(t, family)
			if r.ActiveOperation == nil || r.NextReconciliationTime != nil || r.ObservedAt == nil {
				t.Fatalf("fixture does not reproduce the in-flight reading: %+v", r)
			}
			s := sample{Resources: 1, ResourceFreshness: []resourceFreshness{r}}
			// Replay from retained JSON, as a later report audit does.
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			var restored sample
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			c := eligibleFreshness([]sample{restored})
			if c == nil || c.EligibleReadings != 1 || c.InFlightReadings != 1 || c.ScheduledReadings != 0 || c.MissingDeadlines != 0 || c.OverdueMaxSeconds != nil || c.ActiveOperationAgeMaxSeconds == nil || *c.ActiveOperationAgeMaxSeconds != 17 || c.ObservationAgeMaxSeconds == nil || *c.ObservationAgeMaxSeconds != r.ReadAt.Sub(*r.ObservedAt).Seconds() {
				t.Fatalf("active work lost its age or fabricated a scheduled bound: %+v", c)
			}

			// Recovery may retain a scheduled retry beside the active claim.
			// Keep measuring that deadline rather than exempting all claims.
			due := r.ReadAt.Add(-42 * time.Second)
			s.ResourceFreshness[0].NextReconciliationTime = &due
			c = eligibleFreshness([]sample{s})
			if c == nil || c.ScheduledReadings != 1 || c.InFlightReadings != 1 || c.OverdueMaxSeconds == nil || *c.OverdueMaxSeconds != 42 {
				t.Fatalf("active claim hid its overdue retry: %+v", c)
			}
			// A hung claim must increase age, not vanish from freshness.
			s.ResourceFreshness[0].ReadAt = r.ReadAt.Add(time.Hour)
			c = eligibleFreshness([]sample{s})
			if c == nil || *c.ActiveOperationAgeMaxSeconds != 3617 || *c.ObservationAgeMaxSeconds != r.ReadAt.Sub(*r.ObservedAt).Seconds()+3600 {
				t.Fatalf("hung claim hid stale observations: %+v", c)
			}
		})
	}
}

func TestFreshnessRequiresAnActualClaim(t *testing.T) {
	for _, family := range []string{"schema", "migration"} {
		for _, mode := range []string{"phase only", "empty", "no id", "no job", "no start", "future start", "unknown type", "wrong family", "wrong field type", "before job creation"} {
			t.Run(family+"/"+mode, func(t *testing.T) {
				o, r := nativeActiveFreshness(t, family)
				claim, _, _ := unstructured.NestedMap(o.Object, "status", "activeOperation")
				switch mode {
				case "phase only":
					unstructured.RemoveNestedField(o.Object, "status", "activeOperation")
				case "empty":
					claim = map[string]any{}
				case "no id":
					delete(claim, "id")
				case "no job":
					delete(claim, "jobName")
				case "no start":
					delete(claim, "startedAt")
				case "future start":
					claim["startedAt"] = r.ReadAt.Add(time.Second).Format(time.RFC3339Nano)
				case "unknown type":
					claim["type"] = "FutureOperation"
				case "wrong family":
					claim["type"] = "History"
					if family == "migration" {
						claim["type"] = "Plan"
					}
				case "wrong field type":
					claim["id"] = true
				case "before job creation":
					delete(claim, "jobUID")
				}
				if mode != "phase only" {
					if err := unstructured.SetNestedMap(o.Object, claim, "status", "activeOperation"); err != nil {
						t.Fatal(err)
					}
				}
				got, err := readResourceFreshness(family, o, r.ReadAt, []string{"status", "unusedObservation"})
				if mode != "phase only" && mode != "before job creation" {
					if err == nil {
						t.Fatal("malformed claim became in-flight evidence")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				c := eligibleFreshness([]sample{{Resources: 1, ResourceFreshness: []resourceFreshness{got}}})
				if c == nil || c.OverdueMaxSeconds != nil {
					t.Fatalf("fabricated a scheduled bound: %+v", c)
				}
				if mode == "phase only" && (c.MissingDeadlines != 1 || c.InFlightReadings != 0 || c.ActiveOperationAgeMaxSeconds != nil) {
					t.Fatalf("phase label invented a claim: %+v", c)
				}
				if mode == "before job creation" && (c.MissingDeadlines != 0 || c.InFlightReadings != 1) {
					t.Fatalf("discarded claim before Job creation: %+v", c)
				}
			})
		}
	}
	_, r := nativeActiveFreshness(t, "schema")
	r.ActiveOperation.ID = ""
	if eligibleFreshness([]sample{{Resources: 1, ResourceFreshness: []resourceFreshness{r}}}) != nil {
		t.Fatal("replayed malformed claim became evidence")
	}
}

func TestFreshnessUsesActualReadTimeAtScenarioBoundaries(t *testing.T) {
	at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	observed, next := at.Add(-time.Minute), at.Add(-time.Second)
	row := resourceFreshness{Family: "schema", Namespace: "work", Name: "resource", UID: "uid", ResourceVersion: "1", Generation: 1, ReadAt: at, ObservedAt: &observed, NextReconciliationTime: &next}
	for _, mode := range []string{"inside", "read after end", "read before start", "batch started earlier", "partial boundary batch", "failed boundary batch", "missing actual time"} {
		t.Run(mode, func(t *testing.T) {
			s := sample{At: at.Add(-4 * time.Second), ResourceReadStartedAt: at.Add(-3 * time.Second), ResourceReadFinishedAt: at.Add(3 * time.Second), Resources: 1, ResourceFreshness: []resourceFreshness{row}}
			w := window{Start: at.Add(-time.Second), End: at.Add(time.Second)}
			switch mode {
			case "read after end":
				s.ResourceFreshness[0].ReadAt = w.End.Add(time.Second)
			case "read before start":
				s.ResourceFreshness[0].ReadAt = w.Start.Add(-time.Second)
			case "batch started earlier":
				s.At = w.Start.Add(-time.Second)
			case "partial boundary batch":
				s.At = w.Start.Add(-time.Second)
				s.Resources = 2
			case "failed boundary batch":
				s.At = w.Start.Add(-time.Second)
				s.Incomplete = []string{sourceResources}
			case "missing actual time":
				s.ResourceFreshness[0].ReadAt = time.Time{}
			}
			result := cost(w, []sample{s}, nil)
			c := result.EligibleFreshness
			encoded := jsonObject(t, result)
			if (string(encoded["eligibleFreshness"]) == "null") != (c == nil) {
				t.Fatal("JSON changed the actual-read completeness verdict")
			}
			if mode == "partial boundary batch" || mode == "failed boundary batch" || mode == "missing actual time" {
				if c != nil {
					t.Fatal("partial or undated evidence became a bound", c)
				}
				return
			}
			if c == nil {
				t.Fatal("lost retained population")
			}
			if mode == "read after end" || mode == "read before start" {
				if c.OutsideWindowReadings != 1 || c.EligibleReadings != 0 || c.ObservationAgeMaxSeconds != nil || c.OverdueMaxSeconds != nil {
					t.Fatal("outside reading entered the scenario", c)
				}
			} else if c.EligibleReadings != 1 || c.OutsideWindowReadings != 0 || c.ObservationAgeMaxSeconds == nil || *c.ObservationAgeMaxSeconds != 60 || c.OverdueMaxSeconds == nil || *c.OverdueMaxSeconds != 1 {
				t.Fatal("actual in-window reading was lost", c)
			}
		})
	}
}

func TestFreshnessCannotDropFailedReadsAcrossTheStartBoundary(t *testing.T) {
	at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	observed, next := at.Add(-time.Minute), at.Add(time.Minute)
	good := sample{At: at, ResourceReadStartedAt: at, ResourceReadFinishedAt: at.Add(time.Second), Resources: 1, ResourceFreshness: []resourceFreshness{{Family: "schema", Namespace: "work", Name: "resource", UID: "uid", ResourceVersion: "1", Generation: 1, ReadAt: at, ObservedAt: &observed, NextReconciliationTime: &next}}}
	for _, mode := range []string{"failed list", "partial list", "missing span", "reversed span", "reading outside span"} {
		t.Run(mode, func(t *testing.T) {
			bad := sample{At: at.Add(-5 * time.Second), ResourceReadStartedAt: at.Add(-4 * time.Second), ResourceReadFinishedAt: at.Add(time.Second), ResourceFreshness: []resourceFreshness{}}
			switch mode {
			case "failed list":
				bad.Incomplete = []string{sourceResources}
			case "partial list":
				bad.Resources = 1
			case "missing span":
				bad.ResourceReadFinishedAt = time.Time{}
			case "reversed span":
				bad.ResourceReadFinishedAt = at.Add(-6 * time.Second)
			case "reading outside span":
				bad.Resources = 1
				bad.ResourceFreshness = append(bad.ResourceFreshness, good.ResourceFreshness[0])
				bad.ResourceFreshness[0].ReadAt = at.Add(2 * time.Second)
			}
			result := cost(window{Start: at, End: at.Add(3 * time.Second)}, []sample{bad, good}, nil)
			if result.EligibleFreshness != nil || string(jsonObject(t, result)["eligibleFreshness"]) != "null" {
				t.Fatal("a failed boundary-crossing list became passing evidence")
			}
		})
	}
}

func TestSamplerRetainsTheFailedResourceReadInterval(t *testing.T) {
	s := measurementFixture(t, "/apis/operator.ptah.run/v1alpha1/namespaces/work/ptahmigrations", completeProcessMetrics)
	if s.take(t.Context()) == nil {
		t.Fatal("the failed LIST was not exercised")
	}
	rows, _ := s.snapshot()
	if len(rows) != 1 || rows[0].ResourceReadStartedAt.IsZero() || rows[0].ResourceReadFinishedAt.Before(rows[0].ResourceReadStartedAt) || rows[0].ResourceReadStartedAt.Before(rows[0].At) {
		t.Fatal("failed LIST lost its actual collection interval", rows)
	}
	encoded := jsonObject(t, rows[0])
	if string(encoded["resourceFreshness"]) != "null" || string(encoded["resourceReadStartedAt"]) == "null" || string(encoded["resourceReadFinishedAt"]) == "null" {
		t.Fatal("serialization erased failed-read timing")
	}
}
