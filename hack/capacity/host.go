package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// A kind node advertises the host's resources. Summing several such nodes
// measures scheduling capacity, not the machine available to the experiment.
type hostCapacity struct {
	DockerID     string    `json:"dockerID"`
	Name         string    `json:"name"`
	CPUs         int       `json:"cpus"`
	MemoryBytes  int64     `json:"memoryBytes"`
	Architecture string    `json:"architecture"`
	OS           string    `json:"os"`
	ObservedAt   time.Time `json:"observedAt"`
}

func readHostCapacity(path string) (*hostCapacity, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // Explicit measurement input.
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var host hostCapacity
	if err := decoder.Decode(&host); err != nil {
		return nil, fmt.Errorf("read Docker host capacity: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("host capacity must contain exactly one JSON object")
	}
	if host.DockerID == "" || host.Name == "" || host.CPUs < 1 || host.MemoryBytes < 1 ||
		host.Architecture == "" || host.OS != "linux" || host.ObservedAt.IsZero() {
		return nil, fmt.Errorf("host capacity lacks a Linux daemon identity, resources or timestamp")
	}
	return &host, nil
}

func recordHostCapacity(environment map[string]any, host *hostCapacity) {
	environment["nodeAllocatableScope"] = "sum across Kubernetes nodes; not physical host capacity"
	environment["hostCapacityObserved"] = host != nil
	if host == nil {
		return
	}
	environment["hostDockerID"] = host.DockerID
	environment["hostName"] = host.Name
	environment["hostCPUs"] = host.CPUs
	environment["hostMemoryBytes"] = host.MemoryBytes
	environment["hostArchitecture"] = host.Architecture
	environment["hostOS"] = host.OS
	environment["hostObservedAt"] = host.ObservedAt.Format(time.RFC3339Nano)
	environment["hostCapacityScope"] = "one Docker daemon; exclusive CPU and memory allocation is not established"
}
