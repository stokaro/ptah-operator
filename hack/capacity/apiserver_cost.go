package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

type apiCost struct {
	apiIdentity
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	Samples          int       `json:"samples"`
	AdmissionSeconds quantiles `json:"admissionSeconds"`
	Rejected         float64   `json:"rejected"`
}

// Each process owns its cumulative histogram. A service endpoint can switch
// processes between readings; it cannot supply a meaningful counter delta.
func apiGrowth(samples []sample) (map[string]apiCost, float64, []string) {
	if len(samples) < 2 {
		return nil, 0, []string{"API metrics need at least two samples"}
	}
	var problems []string
	for i, reading := range samples {
		if slices.Contains(reading.Incomplete, sourceAPI) || len(reading.APIServers) == 0 || len(reading.APIServers) != len(reading.APITargets) {
			problems = append(problems, fmt.Sprintf("API sample %d has an incomplete population", i))
		}
		seenTargets := map[string]bool{}
		for _, target := range reading.APITargets {
			server, ok := reading.APIServers[target.Node]
			current := server.apiIdentity
			current.ProcessStartedAt = target.ProcessStartedAt
			if seenTargets[target.Node] || !ok || current != target {
				problems = append(problems, fmt.Sprintf("API sample %d target %s does not match its process", i, target.Node))
			}
			seenTargets[target.Node] = true
		}
		for name, server := range reading.APIServers {
			if name != server.Node || !validAPIIdentity(server.apiIdentity) || server.ScrapeStartedAt.IsZero() || server.ScrapeCompletedAt.Before(server.ScrapeStartedAt) || !counterContinues(0, server.Rejected) || !validHistogram(server.Admission) {
				problems = append(problems, fmt.Sprintf("API sample %d server %s has invalid identity, timing or counters", i, name))
			}
		}
		if i == 0 {
			continue
		}
		earlier := samples[i-1]
		for name, before := range earlier.APIServers {
			after, ok := reading.APIServers[name]
			if !ok || before.apiIdentity != after.apiIdentity || !after.ScrapeStartedAt.After(before.ScrapeCompletedAt) || !counterContinues(before.Rejected, after.Rejected) || !histogramContinues(before.Admission, after.Admission) {
				problems = append(problems, fmt.Sprintf("API sample %d server %s lost continuous process counters", i, name))
			}
		}
		for name := range reading.APIServers {
			if _, ok := earlier.APIServers[name]; !ok {
				problems = append(problems, fmt.Sprintf("API sample %d added server %s", i, name))
			}
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return nil, 0, problems
	}
	costs := map[string]apiCost{}
	totalRejected := 0.0
	for name, first := range samples[0].APIServers {
		last := samples[len(samples)-1].APIServers[name]
		rejected := last.Rejected - first.Rejected
		costs[name] = apiCost{apiIdentity: first.apiIdentity, From: first.ScrapeStartedAt, To: last.ScrapeCompletedAt, Samples: len(samples),
			AdmissionSeconds: histogramQuantiles(last.Admission.since(first.Admission)), Rejected: rejected}
		totalRejected += rejected
	}
	return costs, totalRejected, nil
}

func validAPIIdentity(id apiIdentity) bool {
	return id.Node != "" && id.NodeUID != "" && id.Pod != "" && id.PodUID != "" && id.ContainerID != "" && !id.ContainerStartedAt.IsZero() && validProcessStart(id.ProcessStartedAt)
}

func apiAdmissionSummary(costs map[string]apiCost) string {
	if len(costs) == 0 {
		return "n/a"
	}
	names := make([]string, 0, len(costs))
	for name := range costs {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]string, 0, len(names))
	for _, name := range names {
		q := costs[name].AdmissionSeconds
		rows = append(rows, fmt.Sprintf("%s: %s (n=%d)", name, atMost(q), q.Count))
	}
	return strings.Join(rows, "; ")
}
