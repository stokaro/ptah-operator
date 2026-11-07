package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
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

// A feature kind crosses the frame like a stem, in the same kind order: the
// byte order of the kinds, which puts it between columns and tables.
func TestFrameCarriesAFeatureKind(t *testing.T) {
	t.Parallel()
	result := convergedObservation()
	result.DriftInventory = []DriftInventorySummary{
		{Kind: "columns", Desired: 87, Observed: 87},
		{Kind: "ptah.run/pgextension/extension-change", Desired: 1, Observed: 0},
		{Kind: "ptah.run/pgpolicy/policy-change", Desired: 3, Observed: 3},
		{Kind: "tables", Desired: 12, Observed: 12},
	}

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

// The status bound is a frame the runner can write and the manager can read:
// every entry the bound admits, each of the longest kind a report may count.
func TestFrameHoldsTheLargestInventory(t *testing.T) {
	t.Parallel()
	result := convergedObservation()
	result.DriftInventory = make([]DriftInventorySummary, 0, dataplane.MaxDriftInventoryEntries)
	for index := range dataplane.MaxDriftInventoryEntries {
		kind := fmt.Sprintf("%s.io/%03d", strings.Repeat("a", 60), index) + strings.Repeat("b", dataplane.MaxDriftFindingKindBytes-67)
		if len(kind) != dataplane.MaxDriftFindingKindBytes {
			t.Fatalf("kind %q is %d bytes, not the bound", kind, len(kind))
		}
		result.DriftInventory = append(result.DriftInventory, DriftInventorySummary{Kind: kind, Desired: 1, Observed: 0})
	}

	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	parsed, err := ParseResultFor(frame, result.Operation, result.OperationID)
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}
	if !reflect.DeepEqual(parsed.DriftInventory, result.DriftInventory) {
		t.Fatalf("parsed %d kinds, want %d", len(parsed.DriftInventory), len(result.DriftInventory))
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
		"a feature kind with an uppercase domain": func(r *Result) {
			r.DriftInventory[0].Kind = "Ptah.run/pgextension/extension-change"
		},
		"a feature kind with a space": func(r *Result) {
			r.DriftInventory[0].Kind = "ptah.run/pgextension/extension change"
		},
		"a feature kind with no path": func(r *Result) { r.DriftInventory[0].Kind = "ptah.run" },
		"a feature kind out of order": func(r *Result) {
			r.DriftInventory = []DriftInventorySummary{
				{Kind: "tables", Desired: 1, Observed: 1},
				{Kind: "ptah.run/pgextension/extension-change", Desired: 1, Observed: 1},
			}
		},
		"a feature kind past the bound": func(r *Result) {
			r.DriftInventory[0].Kind = "ptah.run/" + strings.Repeat("a", dataplane.MaxDriftFindingKindBytes)
		},
		"kinds out of order":  func(r *Result) { r.DriftInventory[0], r.DriftInventory[1] = r.DriftInventory[1], r.DriftInventory[0] },
		"the same kind twice": func(r *Result) { r.DriftInventory[1].Kind = "columns" },
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
	report := `{"contract_version":1,"command":"schema drift","drift":false,"failed":false,"failure_threshold":"all","highest_severity":"","dialect":"postgres",` +
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

// A report the Ptah v0.22.0 release image printed reaches the frame with its
// feature kinds, and the manager reads them back. Drift exits 1, which is a
// domain outcome rather than a failure.
func TestObserveFramesTheFeatureKindsPtahPrinted(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		report string
		want   []DriftInventorySummary
	}{
		{"../dataplane/testdata/drift-feature-extension.json", []DriftInventorySummary{
			{Kind: "columns", Desired: 2, Observed: 2},
			{Kind: "constraints", Desired: 1, Observed: 1},
			{Kind: "ptah.run/pgextension/extension-change", Desired: 1, Observed: 0},
			{Kind: "tables", Desired: 1, Observed: 1},
		}},
		{"../dataplane/testdata/drift-feature-policy.json", []DriftInventorySummary{
			{Kind: "columns", Desired: 2, Observed: 2},
			{Kind: "constraints", Desired: 1, Observed: 1},
			{Kind: "ptah.run/pgpolicy/policy-change", Desired: 1, Observed: 0},
			{Kind: "ptah.run/pgpolicy/table-state-change", Desired: 1, Observed: 1},
			{Kind: "tables", Desired: 1, Observed: 1},
		}},
	} {
		t.Run(test.report, func(t *testing.T) {
			t.Parallel()
			report, err := os.ReadFile(test.report)
			if err != nil {
				t.Fatal(err)
			}
			result := Run(context.Background(), Config{
				Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment("observe-feature-kinds")),
				Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: string(report), exitCode: 1}}},
			})
			if result.Error != nil {
				t.Fatalf("Run() error = %#v", result.Error)
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			parsed, err := ParseResultFor(frame, OperationObserve, "observe-feature-kinds")
			if err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
			if !reflect.DeepEqual(parsed.DriftInventory, test.want) {
				t.Fatalf("framed inventory = %#v, want %#v", parsed.DriftInventory, test.want)
			}
		})
	}
}
