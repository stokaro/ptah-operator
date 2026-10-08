package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

func TestQueueResetPreservesIndependentManagerCounters(t *testing.T) {
	base := time.Date(2026, 10, 8, 11, 32, 33, 0, time.UTC)
	// The observed queue reset kept the same process and increasing CPU
	// counter. It cannot prove queue latency across the lost interval.
	queues := []histogram{
		testHistogram(map[float64]float64{0.1: 10657, 1: 13054, 10: 13199}, 13199, 969.0964452100014),
		testHistogram(map[float64]float64{0.1: 12, 1: 13, 10: 13}, 13, 0.168416559),
		testHistogram(map[float64]float64{0.1: 35, 1: 39, 10: 39}, 39, 1.232993651),
	}
	var rows []sample
	for i, cpu := range []float64{209.57, 209.98, 210.36} {
		rows = append(rows, sample{At: base.Add(time.Duration(i) * 5 * time.Second), Managers: map[string]managerReading{
			"manager": identifiedManager("manager", managerReading{CPUSeconds: cpu, ThrottleSeconds: float64(i) / 4, Requests429: float64(i), QueueWait: queues[i]}),
		}})
	}
	result := cost(window{Name: "queue reset", Start: base, End: base.Add(10 * time.Second)}, rows, nil)
	if len(result.CounterProblems) != 0 || len(result.QueueCounterProblems) != 1 || !result.missing(sourceQueueContinuity) || result.missing(sourceManagerContinuity) {
		t.Fatalf("queue reset invalidated unrelated counters: %+v", result)
	}
	object := jsonObject(t, result)
	if math.Abs(result.ManagerCPUCores-0.079) > 1e-9 || string(object["clientThrottleSeconds"]) != "0.5" || string(object["requests429"]) != "2" || string(object["managerCPUCoresAverage"]) == "null" || string(object["queueWaitSeconds"]) != "null" {
		t.Fatalf("continuous counters or missing queue evidence were misreported: %s", object)
	}
	var summary bytes.Buffer
	if err := writeSummary(&summary, report{Scenarios: []scenarioCost{result}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary.String(), "| 0.08 | n/a | 0.5 |") {
		t.Fatal("Markdown did not preserve CPU/throttling while refusing the queue percentile")
	}
}

func TestCounterResetBetweenEndpointsInvalidatesTheWindow(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	samples := []sample{}
	for i, cpu := range []float64{10, 2, 15} {
		samples = append(samples, sample{At: base.Add(time.Duration(i) * time.Minute), Managers: map[string]managerReading{"manager": identifiedManager("manager", managerReading{RSSBytes: 100, CPUSeconds: cpu})}})
	}
	result := cost(window{Start: base, End: base.Add(2 * time.Minute)}, samples, nil)
	if len(result.CounterProblems) != 1 || !strings.Contains(result.CounterProblems[0], "reset or invalid counter") {
		t.Fatalf("the intermediate reset itself was not detected: %v", result.CounterProblems)
	}
	object := jsonObject(t, result)
	for _, field := range []string{"managerCPUCoresAverage", "clientThrottleSeconds", "requests429", "queueWaitSeconds"} {
		if string(object[field]) != "null" {
			t.Errorf("%s = %s despite an intermediate reset", field, object[field])
		}
	}
	if string(object["managerRSSMaxBytes"]) != "100" {
		t.Fatal("counter reset erased a valid RSS maximum")
	}
}

func identifiedManager(name string, reading managerReading) managerReading {
	reading.PodUID = name + "-uid"
	reading.ContainerID = "containerd://" + name
	reading.ContainerStartedAt = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	reading.ProcessStartedAt = float64(reading.ContainerStartedAt.Unix())
	return reading
}

func TestManagerContinuityRequiresEveryProcessAndCounter(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	readings := func() []sample {
		rows := []sample{}
		for i := range 2 {
			managers := map[string]managerReading{}
			for _, name := range []string{"first", "second"} {
				managers[name] = identifiedManager(name, managerReading{RSSBytes: 200, CPUSeconds: 10 + float64(6*i), ThrottleSeconds: float64(i),
					QueueWait: testHistogram(map[float64]float64{0.1: float64(i * 20), 1: float64(i * 20)}, float64(i*20), float64(i*2))})
			}
			rows = append(rows, sample{At: base.Add(time.Duration(i) * time.Minute), Managers: managers})
		}
		return rows
	}
	w := window{Start: base, End: base.Add(time.Minute)}
	good := cost(w, readings(), nil)
	if good.missing(sourceManagerContinuity) || good.ManagerCPUCores != 0.2 || good.ClientThrottleSeconds != 2 || good.QueueWaitSeconds.Count != 40 {
		t.Fatalf("continuous replicas refused or omitted: %+v", good)
	}
	cases := map[string]func(*managerReading){
		"Pod replacement":          func(m *managerReading) { m.PodUID = "replacement" },
		"container restart":        func(m *managerReading) { m.ContainerID = "replacement" },
		"container start changed":  func(m *managerReading) { m.ContainerStartedAt = m.ContainerStartedAt.Add(time.Second) },
		"process start changed":    func(m *managerReading) { m.ProcessStartedAt++ },
		"restart count changed":    func(m *managerReading) { m.RestartCount++ },
		"missing Pod UID":          func(m *managerReading) { m.PodUID = "" },
		"missing process start":    func(m *managerReading) { m.ProcessStartedAt = 0 },
		"CPU reset":                func(m *managerReading) { m.CPUSeconds = 9 },
		"throttle reset":           func(m *managerReading) { m.ThrottleSeconds = -1 },
		"429 invalid":              func(m *managerReading) { m.Requests429 = -1 },
		"histogram sum reset":      func(m *managerReading) { m.QueueWait.sum = -1 },
		"histogram count reset":    func(m *managerReading) { m.QueueWait.count = -1 },
		"histogram bucket reset":   func(m *managerReading) { m.QueueWait.buckets[0.1] = -1 },
		"histogram bucket missing": func(m *managerReading) { delete(m.QueueWait.buckets, 0.1) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			rows := readings()
			manager := rows[1].Managers["second"]
			change(&manager)
			rows[1].Managers["second"] = manager
			if strings.HasPrefix(name, "histogram ") {
				result := cost(w, rows, nil)
				if len(result.QueueCounterProblems) == 0 || result.missing(sourceManagerContinuity) || string(jsonObject(t, result)["queueWaitSeconds"]) != "null" {
					t.Fatal("queue failure did not isolate the histogram's missing evidence")
				}
				return
			}
			assertCounterEvidenceMissing(t, cost(w, rows, nil))
		})
	}
	for name, change := range map[string]func([]sample){
		"replica disappeared":       func(s []sample) { delete(s[1].Managers, "second") },
		"replica appeared":          func(s []sample) { delete(s[0].Managers, "second") },
		"partial collection":        func(s []sample) { s[1].Incomplete = []string{sourceManagers} },
		"timestamp did not advance": func(s []sample) { s[1].At = s[0].At },
	} {
		t.Run(name, func(t *testing.T) {
			rows := readings()
			change(rows)
			assertCounterEvidenceMissing(t, cost(w, rows, nil))
		})
	}
	t.Run("one sample", func(t *testing.T) { assertCounterEvidenceMissing(t, cost(w, readings()[:1], nil)) })
}

func assertCounterEvidenceMissing(t *testing.T, result scenarioCost) {
	t.Helper()
	if len(result.CounterProblems) == 0 || !result.missing(sourceManagerContinuity) {
		t.Fatal("invalid counter evidence was accepted")
	}
	object := jsonObject(t, result)
	for _, field := range scenarioFields[sourceManagerContinuity] {
		if string(object[field]) != "null" {
			t.Errorf("%s still reports %s", field, object[field])
		}
	}
}
