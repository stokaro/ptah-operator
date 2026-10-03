package main

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func maintenanceSample(at time.Time) sample {
	r := sample{At: at, Managers: map[string]managerReading{}, APIServers: map[string]apiReading{}}
	for i := range 2 {
		name := fmt.Sprint("manager-", i)
		r.Managers[name] = managerReading{PodUID: name, ContainerID: name + "-container", ContainerStartedAt: at.Add(-time.Hour).Truncate(time.Hour), ProcessStartedAt: 1, RSSBytes: 100 * 1024 * 1024, CPUSeconds: 10}
	}
	for i := range 3 {
		name := fmt.Sprint("api-", i)
		r.APIServers[name] = apiReading{apiIdentity: apiIdentity{Node: name, NodeUID: name + "-node", Pod: name + "-pod", processIdentity: processIdentity{PodUID: name + "-uid", ContainerID: name + "-container", ContainerStartedAt: at.Add(-time.Hour), ProcessStartedAt: 1}}, ScrapeStartedAt: at, ScrapeCompletedAt: at.Add(time.Millisecond)}
	}
	return r
}

func TestMaintenanceMetricSampleMustBeComplete(t *testing.T) {
	for _, mode := range []string{"valid", "missing manager", "duplicate manager", "missing API", "duplicate API", "no process", "NaN", "RSS limit", "incomplete", "future"} {
		t.Run(mode, func(t *testing.T) {
			r := maintenanceSample(time.Now().Add(-time.Minute))
			m := r.Managers["manager-0"]
			switch mode {
			case "missing manager":
				delete(r.Managers, "manager-1")
			case "duplicate manager":
				m.PodUID = "manager-1"
			case "missing API":
				delete(r.APIServers, "api-1")
			case "duplicate API":
				a := r.APIServers["api-0"]
				a.PodUID = r.APIServers["api-1"].PodUID
				r.APIServers["api-0"] = a
			case "no process":
				m.ProcessStartedAt = 0
			case "NaN":
				m.RSSBytes = math.NaN()
			case "RSS limit":
				m.RSSBytes = 192*1024*1024 + 1
			case "incomplete":
				r.Incomplete = []string{sourceManagers}
			case "future":
				r.At = time.Now().Add(time.Hour)
			}
			r.Managers["manager-0"] = m
			if err := validateMaintenanceMetrics(r); (err == nil) != (mode == "valid") {
				t.Fatal(mode, err)
			}
		})
	}
}

func TestMaintenanceDoesNotSkipAnIncompleteEligibleSample(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	first := maintenanceSample(at)
	first.Incomplete = []string{sourceManagers}
	s := soakScenarios()
	s.sampleSnapshot = func() []sample {
		return []sample{maintenanceSample(at.Add(-time.Second)), first, maintenanceSample(at.Add(time.Second))}
	}
	got, err := s.maintenanceMetrics(t.Context(), at)
	if err == nil || !got.At.Equal(at) {
		t.Fatal("incomplete sample was hidden", got, err)
	}
}

func TestRetentionPlateauRejectsGrowthAndLostContinuity(t *testing.T) {
	for _, mode := range []string{"valid", "10 percent", "RSS growth", "payload growth", "payload limit", "negative payload", "missing round", "no deletion", "no GC", "not resumed", "missing intermediate", "counter reset", "process replacement", "no continuous samples"} {
		t.Run(mode, func(t *testing.T) {
			s := soakScenarios()
			s.load.Soak.Rounds = 3
			at := time.Now().Truncate(time.Hour).Add(-time.Hour)
			var readings []sample
			for i := range 3 {
				r := maintenanceSample(at.Add(time.Duration(i) * time.Minute))
				// Keep the same process identity through all checkpoints.
				for name, m := range r.Managers {
					m.ContainerStartedAt = at.Add(-time.Hour)
					r.Managers[name] = m
				}
				readings = append(readings, r)
				s.retentionProofs = append(s.retentionProofs, retentionProof{Round: i + 1, QuietAt: r.At.Add(-time.Second), FinishedAt: r.At.Add(time.Second), Resumed: true, Metrics: &r, RetainedPlans: 1, ChunkBytes: 100, ProjectionBytes: 100, PinsBefore: []retentionPin{{}}, PinsAfter: []retentionPin{{}}, Deleted: []retentionDeletion{{GarbageCollected: true}}})
			}
			last := &s.retentionProofs[2]
			switch mode {
			case "10 percent", "RSS growth":
				m := last.Metrics.Managers["manager-0"]
				m.RSSBytes *= 1.1
				if mode == "RSS growth" {
					m.RSSBytes++
				}
				last.Metrics.Managers["manager-0"] = m
			case "payload growth":
				last.ChunkBytes++
			case "payload limit":
				last.ChunkBytes = 128 * 1024 * 1024
			case "negative payload":
				last.ChunkBytes = -1
			case "missing round":
				s.retentionProofs = s.retentionProofs[:2]
			case "no deletion":
				last.Deleted = nil
			case "no GC":
				last.Deleted[0].GarbageCollected = false
			case "not resumed":
				last.Resumed = false
			case "missing intermediate":
				readings[1].Incomplete = []string{sourceManagers}
			case "counter reset":
				m := readings[1].Managers["manager-0"]
				m.CPUSeconds = 0
				readings[1].Managers["manager-0"] = m
			case "process replacement":
				m := readings[1].Managers["manager-0"]
				m.ProcessStartedAt++
				readings[1].Managers["manager-0"] = m
			case "no continuous samples":
				readings = nil
			}
			s.sampleSnapshot = func() []sample { return readings }
			err := s.validateRetentionPlateau()
			if (err == nil) != (mode == "valid" || mode == "10 percent") {
				t.Fatal(mode, err)
			}
		})
	}
}
