package main

import (
	"strings"
	"testing"
	"time"
)

// A proof in the environment is a structure, and %v printed it as pointer
// addresses and whole Job objects. The table keeps scalars and points at the
// report for everything else.
func TestSummaryEnvironmentPrintsScalarsAndPointsAtProofs(t *testing.T) {
	type proof struct{ Jobs []*int }
	one := 1
	var absent *proof
	var buffer strings.Builder
	if err := writeSummary(&buffer, report{Environment: map[string]any{
		"hostName":         "vcluster",
		"hostCPUs":         4,
		"hostCapacityOK":   true,
		"interval":         2 * time.Minute,
		"approvalBacklog":  &proof{Jobs: []*int{&one}},
		"databaseDelay":    absent,
		"inputCatalog":     map[string]string{"a": "b"},
		"retentionFault":   nil,
		"approvalActors":   proof{},
		"unrelatedObjects": []string{},
	}}); err != nil {
		t.Fatal(err)
	}
	summary := buffer.String()
	for _, want := range []string{
		"| hostName | vcluster |", "| hostCPUs | 4 |", "| hostCapacityOK | true |", "| interval | 2m0s |",
		"| approvalBacklog | in report.json at `environment.approvalBacklog` |",
		"| databaseDelay | none |", "| retentionFault | none |",
		"| inputCatalog | in report.json at `environment.inputCatalog` |",
		"| approvalActors | in report.json at `environment.approvalActors` |",
		"| unrelatedObjects | in report.json at `environment.unrelatedObjects` |",
	} {
		if !strings.Contains(summary, want+"\n") {
			t.Errorf("summary lacks %q", want)
		}
	}
	if strings.Contains(summary, "0x") || strings.Contains(summary, "&{") {
		t.Fatalf("summary renders a structure:\n%s", summary)
	}
}
