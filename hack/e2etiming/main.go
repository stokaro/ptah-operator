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
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

func main() {
	ledgerPath := flag.String("ledger", "", "stage ledger written by the harness (JSON Lines, required)")
	contextPath := flag.String("context", "", "run identity written by the harness (JSON, required)")
	outputPath := flag.String("output", "", "write the joined report here (JSON)")
	summaryPath := flag.String("summary", "", "write the Markdown summary here, or - for standard output")
	jobsPath := flag.String("jobs", "", "workflow job records from the Actions API, to separate queueing from execution")
	samplesPath := flag.String("samples", "", "resource samples taken while the run executed (CSV)")
	flag.Parse()

	if *ledgerPath == "" || *contextPath == "" {
		fail("both -ledger and -context are required")
	}

	runContext, err := readContext(*contextPath)
	if err != nil {
		fail("%v", err)
	}
	ledger, err := os.Open(*ledgerPath)
	if err != nil {
		fail("%v", err)
	}
	stages, problems, err := readLedger(ledger)
	_ = ledger.Close()
	if err != nil {
		fail("%v", err)
	}
	if len(stages) == 0 {
		// A run that recorded nothing is a reportable fact, not a report: the
		// exit status says so, so a summary step cannot pass over it silently.
		fail("the timing ledger %s holds no stages", *ledgerPath)
	}

	report := Report{Context: runContext, Records: records(runContext, stages)}
	if *jobsPath != "" {
		jobs, err := readJobs(*jobsPath)
		if err != nil {
			problems = append(problems, fmt.Sprintf("workflow job records were not read: %v", err))
		} else {
			report.Jobs = jobs
		}
	}
	if *samplesPath != "" {
		resources, err := readSamples(*samplesPath)
		if err != nil {
			problems = append(problems, fmt.Sprintf("resource samples were not read: %v", err))
		} else {
			report.Resources = resources
		}
	}

	if *outputPath != "" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fail("encode report: %v", err)
		}
		encoded = append(encoded, '\n')
		if err := os.WriteFile(*outputPath, encoded, 0o600); err != nil {
			fail("write report: %v", err)
		}
	}
	if *summaryPath != "" {
		rendered := summary(report, problems)
		if *summaryPath == "-" {
			fmt.Print(rendered)
		} else if err := os.WriteFile(*summaryPath, []byte(rendered), 0o600); err != nil {
			fail("write summary: %v", err)
		}
	}
	for _, problem := range problems {
		_, _ = fmt.Fprintf(os.Stderr, "e2etiming: %s\n", problem)
	}
}

func fail(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "e2etiming: "+format+"\n", args...)
	os.Exit(1)
}
