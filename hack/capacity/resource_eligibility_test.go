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
	s := sample{At: at, Resources: 2, ResourceFreshness: []resourceFreshness{paused, active}}
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
		"| older | n/a | n/a | n/a | n/a | n/a | n/a | n/a |",
		"| paused | 0 | 20 | 0 | 0 | 0 | n/a | n/a |",
	} {
		if strings.Count(text.String(), row) != 1 {
			t.Fatalf("missing or duplicated freshness row %q", row)
		}
	}
}
