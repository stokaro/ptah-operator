package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestUnrelatedEvidenceRequiresCompleteVerifiedInventory(t *testing.T) {
	for _, mode := range []string{"complete", "no journal", "no inventory", "bad checksum", "overlap", "empty population", "wrong count", "duplicate uid", "duplicate name", "uneven split", "escaped path"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			populations := map[string][]map[string]any{}
			for _, kind := range []string{"configmaps", "jobs", "pods"} {
				for i := range 1000 {
					populations[kind] = append(populations[kind], map[string]any{"metadata": map[string]any{"name": fmt.Sprint(i), "namespace": fmt.Sprintf("background-%d", i%2), "uid": fmt.Sprintf("%s-%d", kind, i)}})
				}
			}
			counts := map[string]int{"configMaps": 1000, "completedJobs": 1000, "successfulPods": 1000, "payloadBytesPerConfigMap": 1024}
			names := []string{"work-a", "work-b"}
			switch mode {
			case "overlap":
				names[0] = "background-0"
			case "empty population":
				populations["pods"] = nil
			case "wrong count":
				counts["configMaps"] = 999
			case "duplicate uid":
				populations["jobs"][0]["metadata"].(map[string]any)["uid"] = "pods-0"
			case "duplicate name":
				populations["jobs"][0]["metadata"].(map[string]any)["name"] = "2"
			case "uneven split":
				populations["jobs"][0]["metadata"].(map[string]any)["namespace"] = "background-1"
			}
			raw, err := json.Marshal(map[string]any{"counts": counts, "namespaces": []map[string]string{{"name": "background-0", "uid": "ns-0"}, {"name": "background-1", "uid": "ns-1"}}, "objects": populations})
			if err != nil {
				t.Fatal(err)
			}
			archive := retentionArchive{Path: "unrelated-before.json", SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}
			if err := os.WriteFile(filepath.Join(dir, archive.Path), raw, 0600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "no inventory":
				archive = retentionArchive{}
			case "bad checksum":
				archive.SHA256 = "invalid"
			case "escaped path":
				archive.Path = "../unrelated-before.json"
			}
			raw, err = json.Marshal(map[string]any{"unrelated": map[string]any{"before": archive}})
			if err != nil {
				t.Fatal(err)
			}
			if mode != "no journal" {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := unrelatedEvidence(path, dir, names)
			if mode == "complete" {
				if err != nil || got != archive {
					t.Fatal(got, err)
				}
			} else if err == nil {
				t.Fatal("accepted missing or mismatched population")
			}
		})
	}
}
