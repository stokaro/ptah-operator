package main

import (
	"fmt"
	"time"
)

// A counter was read somewhere between these instants. Keep both bounds so
// scrape latency cannot shorten a measured interval or hide its edge traffic.
type apiRequestReading struct {
	Total      float64   `json:"total"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
}

type apiRequestBound struct {
	// Every 60-second window starting in this closed interval is covered.
	StartFrom      time.Time `json:"startFrom"`
	StartThrough   time.Time `json:"startThrough"`
	CounterFrom    time.Time `json:"counterFrom"`
	CounterThrough time.Time `json:"counterThrough"`
	Requests       float64   `json:"requests"`
	PerSecondUpper float64   `json:"requestsPerSecondUpperBound"`
}

type apiDemand struct {
	Windows           []apiRequestBound `json:"windows"`
	MaxPerSecondUpper float64           `json:"maxRequestsPerSecondUpperBound"`
}

// Bound all sliding 60-second windows inside the scenario. Each subtraction
// uses a completed scrape before the earliest possible window start and a
// scrape started after its latest possible end. The extra edge traffic makes
// this an upper bound; averaging across the longer scrape span would hide it.
func managerAPIDemand(w window, samples []sample) (*apiDemand, []string) {
	const width = time.Minute
	if w.End.Sub(w.Start) < width {
		return nil, []string{"scenario has no complete 60-second API window"}
	}
	from, through := -1, -1
	for i, s := range samples {
		_, last, ok := requestReadBounds(s)
		if ok && !last.After(w.Start) {
			from = i
		}
		first, _, ok := requestReadBounds(s)
		if ok && !first.Before(w.End) && through == -1 {
			through = i
		}
	}
	if from < 0 || through <= from {
		return nil, []string{"API counters do not bracket the complete scenario"}
	}
	inside := samples[from : through+1]
	if problems := managerProcessContinuity(inside); len(problems) > 0 {
		return nil, problems
	}
	for i, s := range inside {
		if _, _, ok := requestReadBounds(s); !ok {
			return nil, []string{fmt.Sprintf("API sample %d has a missing or invalid request counter", i)}
		}
		if i == 0 {
			continue
		}
		for name, m := range s.Managers {
			before := inside[i-1].Managers[name].APIRequests
			after := m.APIRequests
			if !after.StartedAt.After(before.FinishedAt) || !counterContinues(before.Total, after.Total) {
				return nil, []string{fmt.Sprintf("API sample %d manager %s lost continuous request counters", i, name)}
			}
		}
	}
	result := &apiDemand{}
	lastStart := w.End.Add(-width)
	for i := 0; i+1 < len(inside); i++ {
		_, lower, _ := requestReadBounds(inside[i])
		_, upper, _ := requestReadBounds(inside[i+1])
		if lower.Before(w.Start) {
			lower = w.Start
		}
		if upper.After(lastStart) {
			upper = lastStart
		}
		if upper.Before(lower) {
			continue
		}
		end := upper.Add(width)
		j := i + 1
		for ; j < len(inside); j++ {
			first, _, _ := requestReadBounds(inside[j])
			if !first.Before(end) {
				break
			}
		}
		if j == len(inside) {
			return nil, []string{"API counters do not cover the last 60-second window"}
		}
		first, _, _ := requestReadBounds(inside[i])
		_, last, _ := requestReadBounds(inside[j])
		bound := apiRequestBound{StartFrom: lower, StartThrough: upper, CounterFrom: first, CounterThrough: last}
		for name, before := range inside[i].Managers {
			bound.Requests += inside[j].Managers[name].APIRequests.Total - before.APIRequests.Total
		}
		bound.PerSecondUpper = bound.Requests / width.Seconds()
		result.Windows = append(result.Windows, bound)
		result.MaxPerSecondUpper = max(result.MaxPerSecondUpper, bound.PerSecondUpper)
		if upper.Equal(lastStart) {
			return result, nil
		}
	}
	return nil, []string{"no API window was measured"}
}

func requestReadBounds(s sample) (first, last time.Time, valid bool) {
	if len(s.Managers) == 0 {
		return first, last, false
	}
	for _, manager := range s.Managers {
		r := manager.APIRequests
		if r == nil || r.StartedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) || !counterContinues(0, r.Total) {
			return first, last, false
		}
		if first.IsZero() || r.StartedAt.Before(first) {
			first = r.StartedAt
		}
		if r.FinishedAt.After(last) {
			last = r.FinishedAt
		}
	}
	return first, last, true
}
