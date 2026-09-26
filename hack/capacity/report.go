package main

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// window is one scenario's span, and what that scenario established on its own
// beyond the shared series.
type window struct {
	Name    string            `json:"name"`
	Start   time.Time         `json:"start"`
	End     time.Time         `json:"end"`
	Outcome map[string]string `json:"outcome,omitempty"`
}

// report is the whole measurement: what ran, where, and what it cost.
type report struct {
	Workload    workload       `json:"workload"`
	Environment map[string]any `json:"environment"`
	Scenarios   []scenarioCost `json:"scenarios"`
	Jobs        []jobRecord    `json:"jobs"`
	Samples     []sample       `json:"samples"`
}

// scenarioCost is one window reduced to the figures the capacity page names.
type scenarioCost struct {
	window
	Samples               int                `json:"samples"`
	JobsCreated           int                `json:"jobsCreated"`
	JobsFailed            int                `json:"jobsFailed"`
	JobsPerMinuteAverage  float64            `json:"jobsPerMinuteAverage"`
	JobsPerMinutePeak     int                `json:"jobsPerMinutePeak"`
	JobStartSeconds       quantiles          `json:"jobStartSeconds"`
	JobCompletionSeconds  quantiles          `json:"jobCompletionSeconds"`
	PodsPendingMax        int                `json:"podsPendingMax"`
	PodsRunningMax        int                `json:"podsRunningMax"`
	ObservationAgeMax     float64            `json:"observationAgeMaxSeconds"`
	OverdueMax            float64            `json:"overdueMaxSeconds"`
	ManagerRSSMaxBytes    float64            `json:"managerRSSMaxBytes"`
	ManagerCPUCores       float64            `json:"managerCPUCoresAverage"`
	WorkqueueDepthMax     map[string]float64 `json:"workqueueDepthMax"`
	QueueWaitSeconds      quantiles          `json:"queueWaitSeconds"`
	ClientThrottleSeconds float64            `json:"clientThrottleSeconds"`
	Requests429           float64            `json:"requests429"`
	AdmissionSeconds      quantiles          `json:"admissionSeconds"`
	APIRejected           float64            `json:"apiRejected"`
	PlansAtEnd            int                `json:"plansAtEnd"`
	ChunkBytesAtEnd       int64              `json:"chunkBytesAtEnd"`
}

// quantiles are over the observations a window holds. For a histogram they
// are bucket bounds -- "at most" -- because that is all a histogram knows.
type quantiles struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	Max   float64 `json:"max,omitempty"`
}

func exactQuantiles(values []float64) quantiles {
	if len(values) == 0 {
		return quantiles{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	at := func(q float64) float64 {
		index := int(math.Ceil(q*float64(len(sorted)))) - 1
		return sorted[max(0, min(index, len(sorted)-1))]
	}
	return quantiles{Count: len(sorted), P50: at(0.5), P95: at(0.95), Max: sorted[len(sorted)-1]}
}

func histogramQuantiles(h histogram) quantiles {
	p50, ok := h.quantile(0.5)
	if !ok {
		return quantiles{}
	}
	p95, _ := h.quantile(0.95)
	return quantiles{Count: int(h.count), P50: p50, P95: p95}
}

// cost reduces the samples and Jobs inside one window.
func cost(w window, samples []sample, jobs []jobRecord) scenarioCost {
	out := scenarioCost{window: w, WorkqueueDepthMax: map[string]float64{}}
	var inside []sample
	for _, reading := range samples {
		if !reading.At.Before(w.Start) && !reading.At.After(w.End) {
			inside = append(inside, reading)
		}
	}
	out.Samples = len(inside)
	for _, reading := range inside {
		out.PodsPendingMax = max(out.PodsPendingMax, reading.PodsPending)
		out.PodsRunningMax = max(out.PodsRunningMax, reading.PodsRunning)
		out.ObservationAgeMax = math.Max(out.ObservationAgeMax, reading.ObservationAgeMax.Seconds())
		out.OverdueMax = math.Max(out.OverdueMax, reading.OverdueMax.Seconds())
		for _, manager := range reading.Managers {
			out.ManagerRSSMaxBytes = math.Max(out.ManagerRSSMaxBytes, manager.RSSBytes)
			for queue, depth := range manager.WorkqueueDepth {
				out.WorkqueueDepthMax[queue] = math.Max(out.WorkqueueDepthMax[queue], depth)
			}
		}
	}
	if len(inside) > 0 {
		last := inside[len(inside)-1]
		out.PlansAtEnd, out.ChunkBytesAtEnd = last.Plans, last.ChunkBytes
	}
	out.ManagerCPUCores, out.ClientThrottleSeconds, out.Requests429, out.QueueWaitSeconds = managerGrowth(inside)
	out.AdmissionSeconds, out.APIRejected = apiGrowth(inside)

	perMinute := map[int64]int{}
	var starts, completions []float64
	for _, job := range jobs {
		if job.Created.Before(w.Start) || job.Created.After(w.End) {
			continue
		}
		out.JobsCreated++
		perMinute[job.Created.Unix()/60]++
		if job.Failed {
			out.JobsFailed++
		}
		if job.Started != nil {
			starts = append(starts, job.Started.Sub(job.Created).Seconds())
		}
		if job.Finished != nil && !job.Failed {
			completions = append(completions, job.Finished.Sub(job.Created).Seconds())
		}
	}
	for _, count := range perMinute {
		out.JobsPerMinutePeak = max(out.JobsPerMinutePeak, count)
	}
	if minutes := w.End.Sub(w.Start).Minutes(); minutes > 0 {
		out.JobsPerMinuteAverage = float64(out.JobsCreated) / minutes
	}
	out.JobStartSeconds = exactQuantiles(starts)
	out.JobCompletionSeconds = exactQuantiles(completions)
	return out
}

// managerGrowth is what the manager processes spent inside a window, summed
// over the Pods that served in it. Each Pod's counters are compared with its
// own first reading, so a restart in the window starts a new process rather
// than a negative delta.
func managerGrowth(inside []sample) (cores, throttle, too float64, wait quantiles) {
	type span struct {
		first, last managerReading
		from, to    time.Time
	}
	spans := map[string]*span{}
	for _, reading := range inside {
		for pod, manager := range reading.Managers {
			if existing, ok := spans[pod]; ok {
				existing.last, existing.to = manager, reading.At
			} else {
				spans[pod] = &span{first: manager, last: manager, from: reading.At, to: reading.At}
			}
		}
	}
	if len(inside) < 2 {
		return 0, 0, 0, quantiles{}
	}
	cpu := 0.0
	total := histogram{buckets: map[float64]float64{}}
	for _, s := range spans {
		cpu += counterDelta(s.last.CPUSeconds, s.first.CPUSeconds)
		throttle += counterDelta(s.last.ThrottleSeconds, s.first.ThrottleSeconds)
		too += counterDelta(s.last.Requests429, s.first.Requests429)
		total = total.add(s.last.queueWait.since(s.first.queueWait))
	}
	elapsed := inside[len(inside)-1].At.Sub(inside[0].At).Seconds()
	if elapsed > 0 {
		cores = cpu / elapsed
	}
	return cores, throttle, too, histogramQuantiles(total)
}

func apiGrowth(inside []sample) (quantiles, float64) {
	var first, last *apiReading
	for index := range inside {
		if inside[index].APIServer == nil {
			continue
		}
		if first == nil {
			first = inside[index].APIServer
		}
		last = inside[index].APIServer
	}
	if first == nil || last == first {
		return quantiles{}, 0
	}
	return histogramQuantiles(last.admission.since(first.admission)), counterDelta(last.Rejected, first.Rejected)
}

// writeSummary is the report as a reader of the capacity page reads it.
func writeSummary(out io.Writer, r report) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Capacity measurement: %s\n\n%s\n\n", r.Workload.Name, r.Workload.Description)
	fmt.Fprintf(&b, "%d PtahSchemas and %d PtahMigrations, each on a database of its own, at an interval of %s.\n\n",
		r.Workload.Schemas, r.Workload.Migrations, r.Workload.Interval)
	keys := make([]string, 0, len(r.Environment))
	for key := range r.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	b.WriteString("| Environment | |\n| --- | --- |\n")
	for _, key := range keys {
		fmt.Fprintf(&b, "| %s | %v |\n", key, r.Environment[key])
	}
	b.WriteString("\n| Scenario | Seconds | Jobs/min avg (peak) | Job done p50/p95 s | Pending Pods max | Oldest reading s | Overdue s | Manager RSS MiB | Manager cores | Queue wait p95 s | Throttled s | Admission p95 s | Outcome |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, s := range r.Scenarios {
		outcome := make([]string, 0, len(s.Outcome))
		for key, value := range s.Outcome {
			outcome = append(outcome, key+"="+value)
		}
		sort.Strings(outcome)
		fmt.Fprintf(&b, "| %s | %.0f | %.1f (%d) | %.0f / %.0f | %d | %.0f | %.0f | %.0f | %.2f | %s | %.1f | %s | %s |\n",
			s.Name, s.End.Sub(s.Start).Seconds(), s.JobsPerMinuteAverage, s.JobsPerMinutePeak,
			s.JobCompletionSeconds.P50, s.JobCompletionSeconds.P95, s.PodsPendingMax,
			s.ObservationAgeMax, s.OverdueMax, s.ManagerRSSMaxBytes/(1<<20), s.ManagerCPUCores,
			atMost(s.QueueWaitSeconds), s.ClientThrottleSeconds, atMost(s.AdmissionSeconds), strings.Join(outcome, ", "))
	}
	_, err := io.WriteString(out, b.String())
	return err
}

func atMost(q quantiles) string {
	if q.Count == 0 {
		return "n/a"
	}
	if math.IsInf(q.P95, 1) {
		return "> largest bucket"
	}
	return fmt.Sprintf("<= %g", q.P95)
}
