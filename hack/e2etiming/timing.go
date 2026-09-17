// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// e2etiming publishes what a lifecycle run spent its time on.
//
// The harness writes two files: a ledger of stages, one short JSON object per
// append, and the run's identity, written once. This joins them into one
// machine-readable report whose every record carries the identity a reader
// needs to compare two runs, and renders the same data as the job summary.
//
// It also answers the two questions a duration alone cannot. Whether a run
// waited for a runner rather than working is read from the workflow's own job
// records; whether it was short of CPU, memory or disk is read from samples
// taken while it ran. Neither is inferred from the total.
package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Context is the identity of one run: what was built, what it was built
// against, and where it ran.
type Context struct {
	RunID            string `json:"runID"`
	GithubRunID      string `json:"githubRunID,omitempty"`
	GithubRunAttempt string `json:"githubRunAttempt,omitempty"`
	OperatorRevision string `json:"operatorRevision"`
	PtahCommit       string `json:"ptahCommit,omitempty"`
	PtahVersion      string `json:"ptahVersion,omitempty"`
	Kubernetes       string `json:"kubernetes"`
	Suite            string `json:"suite"`
	Cluster          string `json:"cluster,omitempty"`
}

// Stage is one ledger row: a bootstrap step, a phase, or a scenario inside one.
type Stage struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Seconds int64  `json:"seconds"`
}

// Record is a stage with its run's identity, which is the form a reader
// comparing two runs needs and the form the published file carries.
type Record struct {
	Context
	Stage
}

// JobTiming separates waiting for a runner from working on it. GitHub records
// when a job was created, when a runner picked it up, and when it ended; the
// difference between the first two is queueing and nothing else.
type JobTiming struct {
	Name        string `json:"name"`
	Conclusion  string `json:"conclusion"`
	QueueSecond int64  `json:"queueSeconds"`
	RunSeconds  int64  `json:"runSeconds"`
}

// ResourceSummary is what the samples say about pressure, as the extremes
// rather than as an average: a run that ran out of memory once did so once.
type ResourceSummary struct {
	Samples            int     `json:"samples"`
	PeakLoad           float64 `json:"peakLoad"`
	MinAvailableMemMiB float64 `json:"minAvailableMemoryMiB"`
	PeakDiskUsePercent float64 `json:"peakDiskUsePercent"`
}

// Report is the published document.
type Report struct {
	Context   Context          `json:"context"`
	Records   []Record         `json:"records"`
	Jobs      []JobTiming      `json:"jobs,omitempty"`
	Resources *ResourceSummary `json:"resources,omitempty"`
}

func readContext(path string) (Context, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Context{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var context Context
	if err := decoder.Decode(&context); err != nil {
		return Context{}, fmt.Errorf("decode timing context: %w", err)
	}
	if strings.TrimSpace(context.Suite) == "" {
		return Context{}, errors.New("timing context names no suite")
	}
	return context, nil
}

// readLedger reads the stage rows. A truncated last line is the shape a killed
// run leaves, and it is reported rather than guessed at: the rows before it are
// still evidence, and a reader is told one was lost.
func readLedger(reader io.Reader) ([]Stage, []string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var stages []Stage
	var problems []string
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var stage Stage
		if err := json.Unmarshal([]byte(text), &stage); err != nil {
			problems = append(problems, fmt.Sprintf("ledger line %d is not a stage row", line))
			continue
		}
		if stage.Name == "" || stage.Kind == "" {
			problems = append(problems, fmt.Sprintf("ledger line %d names no stage", line))
			continue
		}
		stages = append(stages, stage)
	}
	if err := scanner.Err(); err != nil {
		return nil, problems, fmt.Errorf("read timing ledger: %w", err)
	}
	return stages, problems, nil
}

func records(context Context, stages []Stage) []Record {
	merged := make([]Record, 0, len(stages))
	for _, stage := range stages {
		merged = append(merged, Record{Context: context, Stage: stage})
	}
	return merged
}

// githubJob is the part of the workflow job record this reads. The file comes
// from the Actions API, which carries far more than three timestamps.
type githubJob struct {
	Name        string    `json:"name"`
	Conclusion  string    `json:"conclusion"`
	CreatedAt   time.Time `json:"created_at"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

func readJobs(path string) ([]JobTiming, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Jobs []githubJob `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode workflow jobs: %w", err)
	}
	timings := make([]JobTiming, 0, len(payload.Jobs))
	for _, job := range payload.Jobs {
		timing := JobTiming{Name: job.Name, Conclusion: job.Conclusion}
		if !job.CreatedAt.IsZero() && !job.StartedAt.IsZero() {
			// A job whose runner was waiting reports a start before its own
			// creation. That is not negative queueing; it is none.
			if queue := int64(job.StartedAt.Sub(job.CreatedAt).Seconds()); queue > 0 {
				timing.QueueSecond = queue
			}
		}
		if !job.StartedAt.IsZero() && !job.CompletedAt.IsZero() {
			timing.RunSeconds = int64(job.CompletedAt.Sub(job.StartedAt).Seconds())
		}
		timings = append(timings, timing)
	}
	return timings, nil
}

// readSamples reads the resource samples. The columns are named in the header,
// so a sampler that grows a column does not shift what this reads.
func readSamples(path string) (*ResourceSummary, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	reader := csv.NewReader(file)
	rows, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read resource samples: %w", err)
	}
	if len(rows) < 2 {
		return nil, errors.New("resource samples carry no rows")
	}
	column := map[string]int{}
	for index, name := range rows[0] {
		column[strings.TrimSpace(name)] = index
	}
	summary := &ResourceSummary{MinAvailableMemMiB: -1}
	for _, row := range rows[1:] {
		summary.Samples++
		if value, ok := numeric(row, column, "load1"); ok && value > summary.PeakLoad {
			summary.PeakLoad = value
		}
		if value, ok := numeric(row, column, "mem_available_mib"); ok &&
			(summary.MinAvailableMemMiB < 0 || value < summary.MinAvailableMemMiB) {
			summary.MinAvailableMemMiB = value
		}
		if value, ok := numeric(row, column, "disk_used_percent"); ok && value > summary.PeakDiskUsePercent {
			summary.PeakDiskUsePercent = value
		}
	}
	if summary.MinAvailableMemMiB < 0 {
		summary.MinAvailableMemMiB = 0
	}
	return summary, nil
}

func numeric(row []string, column map[string]int, name string) (float64, bool) {
	index, ok := column[name]
	if !ok || index >= len(row) {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(row[index]), 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func duration(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	rest := seconds % 60
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh %02dm %02ds", hours, minutes, rest)
	case minutes > 0:
		return fmt.Sprintf("%dm %02ds", minutes, rest)
	default:
		return fmt.Sprintf("%ds", rest)
	}
}

func totalOf(stages []Stage, kind string) int64 {
	var total int64
	for _, stage := range stages {
		if stage.Kind == kind {
			total += stage.Seconds
		}
	}
	return total
}

func ofKind(stages []Stage, kind string) []Stage {
	var selected []Stage
	for _, stage := range stages {
		if stage.Kind == kind {
			selected = append(selected, stage)
		}
	}
	return selected
}

func longestFirst(stages []Stage) []Stage {
	sorted := append([]Stage(nil), stages...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seconds > sorted[j].Seconds })
	return sorted
}

// summary renders the report as Markdown, which is what a GitHub Actions job
// summary reads. Phases are listed in the order they ran, because the critical
// path is a sequence; scenarios and bootstrap steps are listed longest first,
// because that is the order a reader shortens them in.
func summary(report Report, problems []string) string {
	stages := make([]Stage, 0, len(report.Records))
	for _, record := range report.Records {
		stages = append(stages, record.Stage)
	}
	var out strings.Builder
	context := report.Context
	fmt.Fprintf(&out, "### %s suite on Kubernetes %s\n\n", context.Suite, context.Kubernetes)
	fmt.Fprintf(&out, "Run `%s` · operator `%s` · Ptah `%s`", context.RunID, context.OperatorRevision, context.PtahCommit)
	if context.GithubRunID != "" {
		fmt.Fprintf(&out, " · workflow run %s attempt %s", context.GithubRunID, context.GithubRunAttempt)
	}
	out.WriteString("\n\n")

	phases := ofKind(stages, "phase")
	bootstrap := ofKind(stages, "bootstrap")
	scenarios := ofKind(stages, "scenario")
	fmt.Fprintf(&out, "Measured %s in %d phases and %s in bootstrap and teardown.\n\n",
		duration(totalOf(stages, "phase")), len(phases), duration(totalOf(stages, "bootstrap")))

	writeTable(&out, "Phases, in order", phases)
	writeTable(&out, "Bootstrap and teardown, longest first", longestFirst(bootstrap))
	writeTable(&out, "Scenarios, longest first", longestFirst(scenarios))

	if len(report.Jobs) > 0 {
		var queue, run int64
		for _, job := range report.Jobs {
			queue += job.QueueSecond
			run += job.RunSeconds
		}
		out.WriteString("#### Runner time\n\n")
		fmt.Fprintf(&out, "| Job | Queued | Ran | Outcome |\n| --- | ---: | ---: | --- |\n")
		for _, job := range report.Jobs {
			fmt.Fprintf(&out, "| %s | %s | %s | %s |\n",
				job.Name, duration(job.QueueSecond), duration(job.RunSeconds), job.Conclusion)
		}
		fmt.Fprintf(&out, "\nQueued %s in total; %s of aggregate runner time across %d jobs.\n\n",
			duration(queue), duration(run), len(report.Jobs))
	}

	if report.Resources != nil {
		out.WriteString("#### Resource pressure\n\n")
		fmt.Fprintf(&out,
			"%d samples: peak load %.2f, least available memory %.0f MiB, peak disk use %.0f%%.\n\n",
			report.Resources.Samples, report.Resources.PeakLoad,
			report.Resources.MinAvailableMemMiB, report.Resources.PeakDiskUsePercent)
	}

	for _, problem := range problems {
		fmt.Fprintf(&out, "> %s\n", problem)
	}
	return out.String()
}

func writeTable(out *strings.Builder, title string, stages []Stage) {
	if len(stages) == 0 {
		return
	}
	fmt.Fprintf(out, "#### %s\n\n| Stage | Duration | Outcome |\n| --- | ---: | --- |\n", title)
	for _, stage := range stages {
		fmt.Fprintf(out, "| %s | %s | %s |\n", stage.Name, duration(stage.Seconds), stage.Outcome)
	}
	out.WriteString("\n")
}
