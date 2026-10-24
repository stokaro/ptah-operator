package runner

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func convergedObservation() Result {
	return Result{
		ProtocolVersion: ProtocolVersion, Operation: OperationObserve, OperationID: "observe-inventory",
		ChildExitCode: 0, CoordinationDigest: "sha256:" + strings.Repeat("9", 64),
		TargetIdentityDigest: "sha256:" + strings.Repeat("8", 64),
		DriftReportDigest:    "sha256:" + strings.Repeat("7", 64), ObservedDialect: "postgres",
		DriftInventory: []DriftInventorySummary{
			{Kind: "columns", Desired: 87, Observed: 87},
			{Kind: "tables", Desired: 12, Observed: 12},
		},
	}
}

// A converged observation counts its objects as a drifted one does, and the
// manager reads the counts back as the runner framed them.
func TestFrameCarriesTheInventory(t *testing.T) {
	t.Parallel()
	result := convergedObservation()

	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	parsed, err := ParseResultFor(frame, result.Operation, result.OperationID)
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}

	if !reflect.DeepEqual(parsed, result) {
		t.Fatalf("parsed frame = %#v, want %#v", parsed, result)
	}
}

// asResolution turns the observation into a resolution that keeps its
// inventory.
func asResolution(r *Result) {
	r.Operation = OperationResolve
	r.ObservedDialect, r.DriftReportDigest = "", ""
	r.ResolvedDigest = "sha256:" + strings.Repeat("6", 64)
	r.ResolvedReference = "oci://registry.example/schema@" + r.ResolvedDigest
	r.ResolvedMediaType = "application/vnd.example.schema"
}

// The resolution the refusal rows build is one a frame carries once the
// inventory is gone, so the row refuses the inventory and nothing else.
func TestAResolutionWithoutTheInventoryIsFramed(t *testing.T) {
	t.Parallel()
	resolution := convergedObservation()
	asResolution(&resolution)
	resolution.DriftInventory = nil

	if _, err := MarshalFrame(resolution); err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
}

func TestFrameRefusesAnInventoryItCannotKeep(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*Result){
		"a kind no category names": func(r *Result) { r.DriftInventory[0].Kind = "widgets" },
		"a negative count":         func(r *Result) { r.DriftInventory[1].Observed = -1 },
		"kinds out of order":       func(r *Result) { r.DriftInventory[0], r.DriftInventory[1] = r.DriftInventory[1], r.DriftInventory[0] },
		"the same kind twice":      func(r *Result) { r.DriftInventory[1].Kind = "columns" },
		"too many kinds": func(r *Result) {
			r.DriftInventory = make([]DriftInventorySummary, 129)
		},
		"an inventory beside no observation": asResolution,
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			candidate := convergedObservation()
			mutate(&candidate)
			if _, err := MarshalFrame(candidate); !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("MarshalFrame() error = %v, want ErrMalformedFrame", err)
			}
		})
	}
}

// The runner frames Ptah's inventory in kind order, whatever order the
// report listed it in, and refuses a report whose inventory names a kind the
// vocabulary does not.
func TestObserveFramesTheInventory(t *testing.T) {
	t.Parallel()
	report := `{"drift":false,"failed":false,"failure_threshold":"all","highest_severity":"","dialect":"postgres",` +
		`"diff":{"tables_added":[]},"inventory":[{"kind":"tables","desired":12,"observed":12},` +
		`{"kind":"columns","desired":87,"observed":87}]}`
	result := Run(context.Background(), Config{
		Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment("observe-inventory")),
		Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: report}}},
	})
	if result.Error != nil {
		t.Fatalf("Run() error = %#v", result.Error)
	}
	if want := convergedObservation().DriftInventory; !reflect.DeepEqual(result.DriftInventory, want) {
		t.Fatalf("framed inventory = %#v, want %#v", result.DriftInventory, want)
	}
	if _, err := MarshalFrame(result); err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}

	unknown := strings.Replace(report, `"kind":"tables"`, `"kind":"widgets"`, 1)
	refused := Run(context.Background(), Config{
		Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment("observe-unknown-inventory")),
		Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: unknown}}},
	})
	if refused.Error == nil || refused.Error.Code != "invalid_observed_state" || len(refused.DriftInventory) != 0 {
		t.Fatalf("Run() = error %#v with inventory %#v, want the report refused", refused.Error, refused.DriftInventory)
	}
}
