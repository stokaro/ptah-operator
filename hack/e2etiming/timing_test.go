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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ledgerSample = `{"kind":"bootstrap","name":"operator-image","outcome":"pass","start":"2026-09-17T10:00:00Z","end":"2026-09-17T10:04:10Z","seconds":250}
{"kind":"phase","name":"dataplane","outcome":"pass","start":"2026-09-17T10:10:00Z","end":"2026-09-17T11:38:00Z","seconds":5280}
{"kind":"scenario","name":"mysql-lifecycle","outcome":"pass","start":"2026-09-17T10:40:00Z","end":"2026-09-17T11:10:00Z","seconds":1800}
{"kind":"scenario","name":"postgresql-lifecycle","outcome":"pass","start":"2026-09-17T10:10:00Z","end":"2026-09-17T10:40:00Z","seconds":1795}
{"kind":"phase","name":"migrations","outcome":"fail","start":"2026-09-17T11:38:00Z","end":"2026-09-17T11:50:00Z","seconds":720}
`

func TestALedgerRowCarriesTheRunItBelongsTo(t *testing.T) {
	t.Parallel()
	stages, problems, err := readLedger(strings.NewReader(ledgerSample))
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("a well-formed ledger reported problems: %v", problems)
	}
	runContext := Context{RunID: "ci-7-1-1-37", OperatorRevision: "abc1234", PtahCommit: "def5678",
		Kubernetes: "1.37.0", Suite: "lifecycle"}
	merged := records(runContext, stages)
	if len(merged) != len(stages) {
		t.Fatalf("joined %d rows from %d stages", len(merged), len(stages))
	}
	for _, record := range merged {
		// The identity is what makes two runs comparable, so every row must be
		// readable on its own rather than only next to the context file.
		if record.RunID != runContext.RunID || record.Kubernetes != runContext.Kubernetes ||
			record.OperatorRevision != runContext.OperatorRevision || record.Suite != runContext.Suite {
			t.Fatalf("row %s does not carry its run: %+v", record.Name, record)
		}
	}
}

// A truncated last line is what a killed run leaves behind. The rows before it
// are still measurements, and the loss is reported rather than passed over.
func TestATruncatedLedgerKeepsTheRowsItFinished(t *testing.T) {
	t.Parallel()
	truncated := ledgerSample + `{"kind":"phase","name":"reference-d`
	stages, problems, err := readLedger(strings.NewReader(truncated))
	if err != nil {
		t.Fatalf("read the truncated ledger: %v", err)
	}
	if len(stages) != 5 {
		t.Fatalf("kept %d rows, want the 5 that finished", len(stages))
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "line 6") {
		t.Fatalf("the truncated row was not reported: %v", problems)
	}
}

func TestTheSummaryNamesThePhasesInOrderAndTheScenariosByCost(t *testing.T) {
	t.Parallel()
	stages, _, err := readLedger(strings.NewReader(ledgerSample))
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	runContext := Context{RunID: "ci-7-1-1-37", OperatorRevision: "abc1234", PtahCommit: "def5678",
		Kubernetes: "1.37.0", Suite: "lifecycle", GithubRunID: "7", GithubRunAttempt: "1"}
	rendered := summary(Report{Context: runContext, Records: records(runContext, stages)}, nil)

	dataplane := strings.Index(rendered, "| dataplane |")
	migrations := strings.Index(rendered, "| migrations |")
	if dataplane < 0 || migrations < 0 || dataplane > migrations {
		t.Fatalf("the phases are not listed in the order they ran:\n%s", rendered)
	}
	mysql := strings.Index(rendered, "| mysql-lifecycle |")
	postgresql := strings.Index(rendered, "| postgresql-lifecycle |")
	if mysql < 0 || postgresql < 0 || mysql > postgresql {
		t.Fatalf("the scenarios are not listed longest first:\n%s", rendered)
	}
	if !strings.Contains(rendered, "1h 28m 00s") {
		t.Fatalf("the longest phase is not reported as its duration:\n%s", rendered)
	}
	// A failed stage reads as failed in the summary. A timing report that
	// rendered every row as a pass would describe a run nobody had.
	if !strings.Contains(rendered, "| migrations | 12m 00s | fail |") {
		t.Fatalf("the failed phase is not reported as failed:\n%s", rendered)
	}
	if !strings.Contains(rendered, "ci-7-1-1-37") || !strings.Contains(rendered, "workflow run 7 attempt 1") {
		t.Fatalf("the summary does not identify the run:\n%s", rendered)
	}
}

func TestQueueingIsReadFromTheJobRecordsRatherThanTheTotal(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "jobs.json")
	payload := `{"jobs":[
		{"name":"Kubernetes 1.37 full lifecycle","conclusion":"success",
		 "created_at":"2026-09-17T09:58:00Z","started_at":"2026-09-17T10:00:00Z","completed_at":"2026-09-17T12:10:00Z"},
		{"name":"Race detector","conclusion":"success",
		 "created_at":"2026-09-17T10:00:00Z","started_at":"2026-09-17T09:59:00Z","completed_at":"2026-09-17T10:40:00Z"}
	]}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("write the job records: %v", err)
	}
	jobs, err := readJobs(path)
	if err != nil {
		t.Fatalf("read the job records: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("read %d jobs, want 2", len(jobs))
	}
	if jobs[0].QueueSecond != 120 || jobs[0].RunSeconds != 7800 {
		t.Fatalf("the lifecycle job reports queue %ds and run %ds", jobs[0].QueueSecond, jobs[0].RunSeconds)
	}
	// A runner that was already waiting starts before the job record is
	// created. That is no queueing, not negative queueing.
	if jobs[1].QueueSecond != 0 {
		t.Fatalf("a job that never queued reports %ds of queueing", jobs[1].QueueSecond)
	}
}

func TestResourceSamplesReportTheExtremesTheyMeasured(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "samples.csv")
	payload := "timestamp,load1,mem_available_mib,disk_used_percent\n" +
		"2026-09-17T10:00:00Z,1.20,9000,41\n" +
		"2026-09-17T10:00:15Z,5.90,1800,74\n" +
		"2026-09-17T10:00:30Z,3.10,4200,73\n"
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("write the samples: %v", err)
	}
	resources, err := readSamples(path)
	if err != nil {
		t.Fatalf("read the samples: %v", err)
	}
	if resources.Samples != 3 || resources.PeakLoad != 5.9 ||
		resources.MinAvailableMemMiB != 1800 || resources.PeakDiskUsePercent != 74 {
		t.Fatalf("the samples were summarized as %+v", resources)
	}
}
