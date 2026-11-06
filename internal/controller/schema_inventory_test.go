package controller

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// A report the Ptah v0.18.0 release image printed crosses the decoder and the
// frame, and status keeps what the manager read, feature kinds included. The
// reading the extension row's predicate is held to is derived from the same
// capture, and this says the operator would write that inventory.
func TestObjectInventoryKeepsTheFramedReport(t *testing.T) {
	t.Parallel()

	captured, err := os.ReadFile("../dataplane/testdata/drift-feature-extension.json")
	if err != nil {
		t.Fatal(err)
	}
	report, err := dataplane.DecodeDrift(captured, 1)
	if err != nil {
		t.Fatalf("DecodeDrift() error = %v", err)
	}
	inventory := make([]runner.DriftInventorySummary, 0, len(report.Inventory))
	for _, entry := range report.Inventory {
		inventory = append(inventory, runner.DriftInventorySummary{Kind: entry.Kind, Desired: entry.Desired, Observed: entry.Observed})
	}
	digest := "sha256:" + strings.Repeat("7", 64)
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationObserve, OperationID: "observe-extension",
		CoordinationDigest: digest, TargetIdentityDigest: digest, DriftReportDigest: digest, ObservedDialect: "postgres",
		DriftInventory: inventory,
	})
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	parsed, err := runner.ParseResultFor(frame, runner.OperationObserve, "observe-extension")
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}

	reading, err := os.ReadFile("../../testdata/e2e/readings/postgresql-extension-drift-inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	var want operatorv1alpha1.PtahSchema
	if err := json.Unmarshal(reading, &want); err != nil {
		t.Fatal(err)
	}
	if len(want.Status.Target.Inventory) == 0 {
		t.Fatal("the reading counts nothing, so this test would hold the copy to nothing")
	}
	if got := objectInventory(parsed.DriftInventory); !reflect.DeepEqual(got, want.Status.Target.Inventory) {
		t.Fatalf("status would keep %+v, and the reading holds %+v", got, want.Status.Target.Inventory)
	}
}
