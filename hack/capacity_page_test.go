package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The capacity page states what a resource costs before anything is measured.
// Every number on it is read from somewhere in this tree, and a number that
// drifts from its source is worse than no page: a deployment sizes itself on
// arithmetic that stopped being the operator's.
//
// So the four that decide the arithmetic are checked against where they come
// from. The page is allowed to say a thing has not been measured -- that is
// most of what it says -- but it is not allowed to be wrong about a default.

const capacityPage = "docs/site/src/content/docs/support/capacity.md"

// A refresh runs these operations per family, and the Jobs-per-day figures on
// the page are that count against the default interval.
var refreshOperations = map[string][]string{
	"PtahSchema":    {"Resolve", "Verify", "Observe", "Plan"},
	"PtahMigration": {"Resolve", "Verify", "History"},
}

// Every figure the page derives, with the source it has to agree with.
func TestTheCapacityPageAgreesWithItsSources(t *testing.T) {
	t.Parallel()
	page := collapsed(string(readRepositoryFile(t, capacityPage)))

	// The default interval, from the API markers rather than from prose.
	for _, types := range []string{
		"api/v1alpha1/ptahschema_types.go",
		"api/v1alpha1/ptahmigration_types.go",
	} {
		if !strings.Contains(string(readRepositoryFile(t, types)), `+kubebuilder:default="10m"`) {
			t.Errorf("%s no longer defaults an interval to 10m, which the capacity page's arithmetic assumes", types)
		}
	}

	// The Jobs per day each family's refresh produces at that interval, as one
	// row in the family column order. Looking for each number anywhere on the
	// page passes when one family's figure happens to equal another's, which is
	// what an earlier version of this did.
	perDay := 24 * 60 / 10
	wantRow := fmt.Sprintf("| Jobs per resource per day | %d | %d |",
		perDay*len(refreshOperations["PtahSchema"]),
		perDay*len(refreshOperations["PtahMigration"]))
	if !strings.Contains(page, wantRow) {
		t.Errorf("the page does not carry %q, which the two refreshes produce at a 10m interval", wantRow)
	}
	for family, operations := range refreshOperations {
		for _, operation := range operations {
			if !strings.Contains(page, operation) {
				t.Errorf("the page does not name %s among what a %s refresh runs", operation, family)
			}
		}
	}

	// The chart's own manager defaults.
	values := string(readRepositoryFile(t, "charts/ptah-operator/values.yaml"))
	for _, want := range []string{"96Mi", "256Mi", "50m"} {
		if !strings.Contains(values, want) {
			t.Errorf("charts/ptah-operator/values.yaml no longer carries %s", want)
			continue
		}
		if !strings.Contains(page, want) {
			t.Errorf("the page does not carry the chart's %s manager default", want)
		}
	}

	// The plan ceilings, from the contract rather than from memory.
	limits := string(readRepositoryFile(t, "internal/plancontract/limits.go"))
	for source, rendered := range map[string]string{
		"MaxExecutableBytes int64 = 8 << 20": "8 MiB",
		"ChunkBytes":                         "512 KiB",
	} {
		if !strings.Contains(limits, source) {
			t.Errorf("internal/plancontract no longer declares %q", source)
			continue
		}
		if !strings.Contains(page, rendered) {
			t.Errorf("the page does not carry %s from the plan contract", rendered)
		}
	}
}

// The page says plainly that nothing is measured. A later edit that turns the
// arithmetic into a capacity claim is the failure worth refusing: the numbers
// are a lower bound on cost, and reading them as a limit is how a deployment
// sizes itself on nothing.
// #224's first acceptance criterion is a declaration rather than a
// measurement: name the workload dimensions a supported envelope covers. They
// are listed here because the page cannot derive them from anything in the
// tree, and because a measurement that quietly drops one describes an
// installation nobody has.
var workloadDimensions = []struct {
	name  string
	marks []string
}{
	{"resource and realm count", []string{"realms across them"}},
	{"refresh interval", []string{"`spec.interval`"}},
	{"plan size", []string{"Plan size"}},
	{"migration history length", []string{"Migration history length"}},
	{"approval backlog", []string{"Approvals waiting"}},
	{"concurrent changes", []string{"Changes at once"}},
}

func TestTheCapacityPageNamesEveryWorkloadDimension(t *testing.T) {
	t.Parallel()
	page := collapsed(string(readRepositoryFile(t, capacityPage)))
	if !strings.Contains(page, "The dimensions an envelope has to be stated over") {
		t.Fatal("the page no longer names the axes a measured envelope would vary")
	}
	for _, dimension := range workloadDimensions {
		found := false
		for _, mark := range dimension.marks {
			if strings.Contains(page, mark) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the page names no dimension for %s, so a measurement could hold it fixed and read as complete",
				dimension.name)
		}
	}
	// The section says what a measurement would have to produce, and must not
	// start producing it. The no-claim gate covers the page; this covers the
	// one place a maximum would most plausibly be typed.
	if regexp.MustCompile(`(?i)(maximum|up to) [0-9]`).MatchString(page) {
		t.Error("a dimension grew a number, which is a capacity claim nothing here measured")
	}
}

func TestTheCapacityPageClaimsNoMeasurement(t *testing.T) {
	t.Parallel()
	page := collapsed(string(readRepositoryFile(t, capacityPage)))
	if !strings.Contains(page, "No capacity envelope has been measured") {
		t.Error("the page no longer opens by saying no envelope has been measured")
	}
	if !strings.Contains(page, "has not been measured") {
		t.Error("the page no longer has a section naming what was not measured")
	}
	for _, forbidden := range []string{
		"supports up to",
		"tested up to",
		"scales to",
	} {
		if regexp.MustCompile(`(?i)` + regexp.QuoteMeta(forbidden)).MatchString(page) {
			t.Errorf("the page says %q, which is a capacity claim nothing here measured", forbidden)
		}
	}
}
