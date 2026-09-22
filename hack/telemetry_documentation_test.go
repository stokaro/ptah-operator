package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

const operationsGuide = "docs/site/src/content/docs/use/operations.md"

// documentedSeries matches a metric named with its bare label set, which is
// how the guide declares one. A matcher written with a value, such as
// `ptah_operator_applies_total{outcome="uncertain"}`, is prose about one
// series rather than a declaration of the labels, and the quote excludes it.
var documentedSeries = regexp.MustCompile("`(ptah_operator_[a-z_]+)\\{([a-z,]*)\\}`")

// The guide is what an operator writes alerts from, so a label it names has to
// be a label the operator emits.
//
// Both halves matter. A documented label the collectors do not carry sends a
// reader to write a query that matches nothing; a label the collectors carry
// and the guide omits is a dimension nobody knows to split by, which is how
// the migration family went unnoticed in the apply counter for as long as it
// did.
func TestTheDocumentedMetricsAreTheOnesTheOperatorEmits(t *testing.T) {
	t.Parallel()
	emitted := emittedSeriesLabels(t)
	guide := string(readOperationsGuide(t))
	documented := make(map[string][]string)
	for _, match := range documentedSeries.FindAllStringSubmatch(guide, -1) {
		labels := strings.Split(match[2], ",")
		sort.Strings(labels)
		documented[match[1]] = labels
	}
	if len(documented) == 0 {
		t.Fatalf("%s documents no metric, so this check would pass over nothing", operationsGuide)
	}
	for name, labels := range documented {
		actual, found := emitted[name]
		if !found {
			t.Errorf("%s documents %q, which the operator does not register", operationsGuide, name)
			continue
		}
		if strings.Join(actual, ",") != strings.Join(labels, ",") {
			t.Errorf("%s documents %q with labels %v, and it carries %v",
				operationsGuide, name, labels, actual)
		}
	}
	for name := range emitted {
		if _, found := documented[name]; !found {
			t.Errorf("the operator registers %q, which %s does not document", name, operationsGuide)
		}
	}
}

// emittedSeriesLabels observes one sample on every collector and reads the
// label names back out of the registry, so the answer comes from the
// collectors rather than from a list restated here.
func emittedSeriesLabels(t *testing.T) map[string][]string {
	t.Helper()
	registry := prometheus.NewRegistry()
	metrics := telemetry.New(registry)
	metrics.ObserveReconciliation(telemetry.FamilySchema, telemetry.ReconciliationSucceeded)
	metrics.ObserveDrift(operatorv1alpha1.DatabaseEnginePostgreSQL, telemetry.DriftInSync)
	metrics.ObservePlan(telemetry.FamilySchema, operatorv1alpha1.DatabaseEnginePostgreSQL, telemetry.PlanDestructive)
	metrics.ObserveApproval(telemetry.FamilyMigration, telemetry.ApprovalAccepted)
	metrics.ObserveApply(telemetry.FamilyMigration, telemetry.ApplyUncertain)
	metrics.ObserveOperation(telemetry.FamilyMigration, telemetry.OperationHistory,
		telemetry.OperationSucceeded, time.Second)
	metrics.ObserveFailure(telemetry.FamilyMigration, telemetry.FailureStageHistory, telemetry.FailureOperation)

	gathered, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather the operator collectors: %v", err)
	}
	emitted := make(map[string][]string, len(gathered))
	for _, family := range gathered {
		var labels []string
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				labels = append(labels, label.GetName())
			}
			break
		}
		sort.Strings(labels)
		emitted[family.GetName()] = labels
	}
	if len(emitted) == 0 {
		t.Fatal("no collector was gathered, so this check would pass over nothing")
	}
	return emitted
}

func readOperationsGuide(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(repositoryRoot(t), operationsGuide)
	content, err := os.ReadFile(path) //nolint:gosec // A path this test built from the repository root.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return content
}
