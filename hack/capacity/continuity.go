package main

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
)

type processIdentity struct {
	PodUID             string    `json:"podUID"`
	ContainerID        string    `json:"containerID"`
	ContainerStartedAt time.Time `json:"containerStartedAt"`
	ProcessStartedAt   float64   `json:"processStartedAt"`
	RestartCount       int32     `json:"restartCount"`
}

func managerIdentity(pod *corev1.Pod) (processIdentity, error) {
	return containerIdentity(pod, "manager")
}

func containerIdentity(pod *corev1.Pod, name string) (processIdentity, error) {
	if pod.UID == "" || pod.Status.Phase != corev1.PodRunning {
		return processIdentity{}, fmt.Errorf("Pod %s has no running identity", pod.Name)
	}
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name != name {
			continue
		}
		if container.ContainerID == "" || container.State.Running == nil || container.State.Running.StartedAt.IsZero() {
			break
		}
		return processIdentity{PodUID: string(pod.UID), ContainerID: container.ContainerID,
			ContainerStartedAt: container.State.Running.StartedAt.Time, RestartCount: container.RestartCount}, nil
	}
	return processIdentity{}, fmt.Errorf("Pod %s has no running %s container identity", pod.Name, name)
}

func validProcessStart(started float64) bool {
	return started > 0 && !math.IsNaN(started) && !math.IsInf(started, 0)
}

func (m managerReading) identity() (processIdentity, bool) {
	id := processIdentity{PodUID: m.PodUID, ContainerID: m.ContainerID, ContainerStartedAt: m.ContainerStartedAt,
		ProcessStartedAt: m.ProcessStartedAt, RestartCount: m.RestartCount}
	return id, id.PodUID != "" && id.ContainerID != "" && !id.ContainerStartedAt.IsZero() && validProcessStart(id.ProcessStartedAt)
}

// Counter deltas need a continuous population and every intermediate reading.
// A reset can disappear in a first/last comparison once a new process catches up.
// A replacement or a collection gap retains its gauges, but cannot establish
// an upper bound on work done by processes that were not observed.
func managerCounterContinuity(samples []sample) []string {
	problems := managerProcessContinuity(samples)
	for i := 1; i < len(samples); i++ {
		for name, before := range samples[i-1].Managers {
			after, exists := samples[i].Managers[name]
			beforeID, beforeOK := before.identity()
			afterID, afterOK := after.identity()
			if !exists || !beforeOK || !afterOK || beforeID != afterID {
				continue
			}
			if !counterContinues(before.CPUSeconds, after.CPUSeconds) ||
				!counterContinues(before.ThrottleSeconds, after.ThrottleSeconds) ||
				!counterContinues(before.Requests429, after.Requests429) {
				problems = append(problems, fmt.Sprintf("sample %d manager %s has a reset or invalid counter", i, name))
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// Native histogram bucket limits can reset the queue histogram without a
// process restart. That gap invalidates queue latency, not the independent
// CPU and REST client counters. Process continuity still gates every family.
func managerQueueContinuity(samples []sample) []string {
	var problems []string
	for i := 1; i < len(samples); i++ {
		for name, before := range samples[i-1].Managers {
			after, exists := samples[i].Managers[name]
			if exists && !histogramContinues(before.QueueWait, after.QueueWait) {
				problems = append(problems, fmt.Sprintf("sample %d manager %s has a reset or invalid queue histogram", i, name))
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// Process continuity is shared by independent metric families. A queue
// histogram reset does not reset a continuous REST request counter.
func managerProcessContinuity(samples []sample) []string {
	if len(samples) < 2 {
		return []string{"fewer than two manager samples"}
	}
	var problems []string
	for i, reading := range samples {
		if len(reading.Managers) == 0 || slices.Contains(reading.Incomplete, sourceManagers) {
			problems = append(problems, fmt.Sprintf("sample %d has incomplete manager metrics", i))
		}
		for name, manager := range reading.Managers {
			if _, ok := manager.identity(); !ok {
				problems = append(problems, fmt.Sprintf("sample %d manager %s has no complete process identity", i, name))
			}
		}
		if i == 0 {
			continue
		}
		earlier := samples[i-1]
		if !reading.At.After(earlier.At) {
			problems = append(problems, fmt.Sprintf("sample %d has a non-increasing timestamp", i))
		}
		for name, before := range earlier.Managers {
			after, exists := reading.Managers[name]
			if !exists {
				problems = append(problems, fmt.Sprintf("sample %d lost manager %s", i, name))
				continue
			}
			beforeID, beforeOK := before.identity()
			afterID, afterOK := after.identity()
			if !beforeOK || !afterOK {
				continue
			}
			if beforeID != afterID {
				problems = append(problems, fmt.Sprintf("sample %d manager %s changed process identity", i, name))
				continue
			}

		}
		for name := range reading.Managers {
			if _, exists := earlier.Managers[name]; !exists {
				problems = append(problems, fmt.Sprintf("sample %d added manager %s without a preceding reading", i, name))
			}
		}
	}
	sort.Strings(problems)
	return problems
}

func counterContinues(before, after float64) bool {
	return before >= 0 && after >= before && !math.IsInf(before, 0) && !math.IsInf(after, 0)
}

func histogramContinues(before, after histogram) bool {
	if !validHistogram(before) || !validHistogram(after) || !counterContinues(before.count, after.count) || !counterContinues(before.sum, after.sum) {
		return false
	}
	for bound, count := range before.buckets {
		later, exists := after.buckets[bound]
		if !exists || !counterContinues(count, later) {
			return false
		}
	}
	// An uninitialized histogram can acquire its first bucket layout. A layout
	// change after observations cannot be subtracted as though it were unchanged.
	if before.count > 0 && len(before.buckets) != len(after.buckets) {
		return false
	}
	return validHistogram(after.since(before))
}

func validHistogram(h histogram) bool {
	if !counterContinues(0, h.count) || !counterContinues(0, h.sum) || h.count > 0 && len(h.buckets) == 0 {
		return false
	}
	bounds := make([]float64, 0, len(h.buckets))
	for bound := range h.buckets {
		if bound < 0 || math.IsNaN(bound) {
			return false
		}
		bounds = append(bounds, bound)
	}
	sort.Float64s(bounds)
	previous := 0.0
	for _, bound := range bounds {
		count := h.buckets[bound]
		if !counterContinues(previous, count) || count > h.count || math.IsInf(bound, 1) && count != h.count {
			return false
		}
		previous = count
	}
	return true
}
