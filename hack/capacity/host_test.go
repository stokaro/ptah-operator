package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestKindNodesCannotMultiplyMeasuredHostCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.json")
	if err := os.WriteFile(path, []byte(`{"dockerID":"daemon-id","name":"runner","cpus":4,"memoryBytes":17179869184,"architecture":"x86_64","os":"linux","observedAt":"2026-10-01T09:43:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := readHostCapacity(path)
	if err != nil {
		t.Fatal(err)
	}
	// Four kind nodes on this host each advertise four CPUs and 16 GiB.
	environment := map[string]any{"nodes": 4, "allocatableCPU": "16", "allocatableMemory": "64Gi"}
	recordHostCapacity(environment, host)
	if environment["hostCPUs"] != 4 || environment["hostMemoryBytes"] != int64(17179869184) || environment["allocatableCPU"] != "16" || environment["hostCapacityObserved"] != true {
		t.Fatalf("node scheduling capacity replaced the host reading: %v", environment)
	}
	if environment["hostDockerID"] != "daemon-id" || environment["hostCapacityScope"] == "" || environment["nodeAllocatableScope"] == "" {
		t.Fatal("host identity or applicability was lost")
	}
	missing := map[string]any{"nodes": 4, "allocatableCPU": "16", "allocatableMemory": "64Gi"}
	recordHostCapacity(missing, nil)
	if missing["hostCapacityObserved"] != false || missing["hostCPUs"] != nil || missing["hostMemoryBytes"] != nil {
		t.Fatal("missing host evidence was inferred from the nodes")
	}
}

func TestHostCapacityRejectsIncompleteEvidence(t *testing.T) {
	valid := map[string]any{"dockerID": "daemon-id", "name": "runner", "cpus": 4, "memoryBytes": 17179869184, "architecture": "x86_64", "os": "linux", "observedAt": "2026-10-01T09:43:00Z"}
	path := filepath.Join(t.TempDir(), "host.json")
	for name, value := range valid {
		delete(valid, name)
		body, err := json.Marshal(valid)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readHostCapacity(path); err == nil {
			t.Errorf("missing %s accepted", name)
		}
		valid[name] = value
	}
	for _, body := range []string{`{}`, `null`, `{"cpus":0}`, `{} {}`, `{"cpus":"4"}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readHostCapacity(path); err == nil {
			t.Errorf("invalid reading accepted: %s", body)
		}
	}
}
