package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// The wrapper verifies native Job/Pod completion before starting collection.
// Require its checked inventory instead of treating a requested workload flag
// as evidence that the background population exists.
func unrelatedEvidence(statePath, directory string, workloadNamespaces []string) (retentionArchive, error) {
	var state struct {
		Unrelated *struct {
			Before retentionArchive `json:"before"`
		} `json:"unrelated"`
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return retentionArchive{}, fmt.Errorf("unrelated objects require the bootstrap journal: %w", err)
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return retentionArchive{}, err
	}
	if state.Unrelated == nil || !filepath.IsLocal(state.Unrelated.Before.Path) || state.Unrelated.Before.SHA256 == "" {
		return retentionArchive{}, fmt.Errorf("unrelated objects lack the verified pre-run inventory")
	}
	archive := state.Unrelated.Before
	raw, err = os.ReadFile(filepath.Join(directory, archive.Path))
	if err != nil {
		return retentionArchive{}, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != archive.SHA256 {
		return retentionArchive{}, fmt.Errorf("unrelated inventory checksum changed")
	}
	var evidence struct {
		Namespaces []struct{ Name, UID string }                                                      `json:"namespaces"`
		Counts     struct{ ConfigMaps, CompletedJobs, SuccessfulPods, PayloadBytesPerConfigMap int } `json:"counts"`
		Objects    map[string][]struct {
			Metadata struct{ Name, Namespace, UID string } `json:"metadata"`
		} `json:"objects"`
	}
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return retentionArchive{}, err
	}
	if len(evidence.Namespaces) != 2 || evidence.Counts.ConfigMaps != 1000 || evidence.Counts.CompletedJobs != 1000 || evidence.Counts.SuccessfulPods != 1000 || evidence.Counts.PayloadBytesPerConfigMap != 1024 {
		return retentionArchive{}, fmt.Errorf("unrelated inventory does not match the frozen population")
	}
	namespaces := map[string]bool{}
	namespaceUIDs := map[string]bool{}
	for _, ns := range evidence.Namespaces {
		if ns.Name == "" || ns.UID == "" || namespaces[ns.Name] || namespaceUIDs[ns.UID] || slices.Contains(workloadNamespaces, ns.Name) {
			return retentionArchive{}, fmt.Errorf("unrelated namespaces overlap or lack identities")
		}
		namespaces[ns.Name] = true
		namespaceUIDs[ns.UID] = true
	}
	uids := map[string]bool{}
	for _, kind := range []string{"configmaps", "jobs", "pods"} {
		population := evidence.Objects[kind]
		counts := map[string]int{}
		names := map[string]bool{}
		if len(population) != 1000 {
			return retentionArchive{}, fmt.Errorf("unrelated %s population is incomplete", kind)
		}
		for _, o := range population {
			m := o.Metadata
			key := m.Namespace + "/" + m.Name
			if m.UID == "" || m.Name == "" || !namespaces[m.Namespace] || names[key] || uids[m.UID] {
				return retentionArchive{}, fmt.Errorf("unrelated %s has missing or duplicate identities", kind)
			}
			uids[m.UID] = true
			names[key] = true
			counts[m.Namespace]++
		}
		for ns := range namespaces {
			if counts[ns] != 500 {
				return retentionArchive{}, fmt.Errorf("unrelated %s are not evenly split", kind)
			}
		}
	}
	return archive, nil
}
