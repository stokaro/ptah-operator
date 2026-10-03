package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkpointDocument(t *testing.T, catalog *inputCatalog, round int) []byte {
	t.Helper()
	slots := []map[string]any{}
	for i, row := range catalog.InitialRows {
		version := 0
		if i%10 < 5 {
			version = round
		}
		slot := map[string]any{"slot": row.Slot, "rows": row.Rows, "sha256": row.SHA256, "round": version}
		if i < 10 {
			slot["band"] = catalog.Schemas[i].Band
			slot["defaultsVerified"] = []int{1, 4, 16}[i%3]
		} else {
			slot["historyLength"] = catalog.Migrations[i-10].HistoryLength + version
		}
		slots = append(slots, slot)
	}
	raw, err := json.Marshal(map[string]any{"engine": catalog.Engine, "ptahCommit": catalog.PtahCommit, "changeBatch": 5, "round": round, "checkpoint": "round-09-after-update", "bundleSHA256": strings.Repeat("b", 64), "slots": slots})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCheckpointBindsEverySlotAndRound(t *testing.T) {
	catalog := testInputCatalog()
	raw := checkpointDocument(t, catalog, 9)
	validate := func(raw []byte) error {
		return validateCheckpoint(raw, catalog, strings.Repeat("b", 64), "round-09-after-update", 9)
	}
	if err := validate(raw); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"engine", "pin", "round", "name", "bundle", "batch", "missing slot", "duplicate slot", "rows", "digest", "stale version", "band", "defaults", "history"} {
		t.Run(name, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal(raw, &value); err != nil {
				t.Fatal(err)
			}
			slots := value["slots"].([]any)
			slot := slots[0].(map[string]any)
			switch name {
			case "engine":
				value["engine"] = "MySQL"
			case "pin":
				value["ptahCommit"] = "wrong"
			case "round":
				value["round"] = 1
			case "name":
				value["checkpoint"] = "before-update"
			case "bundle":
				value["bundleSHA256"] = strings.Repeat("c", 64)
			case "batch":
				value["changeBatch"] = 0
			case "missing slot":
				value["slots"] = slots[:19]
			case "duplicate slot":
				slots[1] = slots[0]
			case "rows":
				slot["rows"] = 9999
			case "digest":
				slot["sha256"] = strings.Repeat("d", 64)
			case "stale version":
				slot["round"] = 1
			case "band":
				slot["band"] = "large"
			case "defaults":
				slot["defaultsVerified"] = 0
			case "history":
				slots[10].(map[string]any)["historyLength"] = 2
			}
			bad, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if validate(bad) == nil {
				t.Fatal("incomplete or stale checkpoint accepted")
			}
		})
	}
}

func TestCheckpointProbeRequiresFreshSuccessArtifact(t *testing.T) {
	directory := t.TempDir()
	state := filepath.Join(directory, "state.json")
	probe := filepath.Join(directory, "probe.py")
	for _, path := range []string{state, filepath.Join(directory, "bundle.json")} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A process that exits zero without doing the database checks is not proof.
	if err := os.WriteFile(probe, []byte("print('no checkpoint')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	execute, err := checkpointProbe(probe, state, directory, testInputCatalog())
	if err != nil {
		t.Fatal(err)
	}
	proof, err := execute(t.Context(), 9, "round-09-after-update")
	if err == nil || proof.SHA256 != "" || proof.StartedAt.IsZero() || !proof.FinishedAt.IsZero() {
		t.Fatalf("empty probe became success: %+v %v", proof, err)
	}
	if _, err := execute(t.Context(), 9, "round-09-after-update"); err == nil {
		t.Fatal("probe diagnostics overwritten")
	}
	if _, err := checkpointProbe("", state, directory, testInputCatalog()); err == nil {
		t.Fatal("missing probe accepted")
	}
}

func TestCheckpointProbeRecordsActualVerifierOutput(t *testing.T) {
	directory := t.TempDir()
	state := filepath.Join(directory, "state.json")
	probe := filepath.Join(directory, "probe.py")
	for _, path := range []string{state, filepath.Join(directory, "bundle.json")} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw := checkpointDocument(t, testInputCatalog(), 9)
	// The process fixture checks argv and emits the success document. The real
	// Python verifier's row, history and default checks have their own tests.
	script := fmt.Sprintf(`import sys,json,pathlib,hashlib
assert sys.argv[1:] == ['--state', %q, '--directory', %q, '--verify-changed', '5', '--verify-round', '9', '--checkpoint', 'round-09-after-update']
root=pathlib.Path(%q)
value=json.loads(%q)
value['bundleSHA256']=hashlib.sha256((root/'bundle.json').read_bytes()).hexdigest()
output=root/'checkpoints'/'round-09-after-update'
output.mkdir(parents=True)
(output/'database-verification.json').write_text(json.dumps(value))
`, state, directory, directory, string(raw))
	if err := os.WriteFile(probe, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	execute, err := checkpointProbe(probe, state, directory, testInputCatalog())
	if err != nil {
		t.Fatal(err)
	}
	proof, err := execute(t.Context(), 9, "round-09-after-update")
	if err != nil {
		t.Fatal(err)
	}
	if !capacityHexDigest.MatchString(proof.SHA256) || !proof.FinishedAt.After(proof.StartedAt) {
		t.Fatal("missing checkpoint identity or time", proof)
	}
}
