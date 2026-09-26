package main

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The lab profile is the one declared profile this repository carries. Its
// owner fixed it to the lab: nothing here justifies a recovery point or time
// for an installation, and a figure that described one runner would be read
// as a promise about every cluster. So the profile says what one reading
// measured, on what, and nothing more.
//
// Every figure in it is the reading's own. The text is rebuilt from the
// reading here and compared whole, so a new reading, a retyped number or a
// dropped qualifier fails rather than drifting.

const (
	labProfilePath = "support/acceptance/lab-20.json"
	labReadingPath = "support/capacity/readings/lab-20-2026-09-26.json"
)

type labReading struct {
	Workload struct {
		Name       string `json:"name"`
		Schemas    int    `json:"schemas"`
		Migrations int    `json:"migrations"`
		Interval   string `json:"interval"`
		Outage     string `json:"outage"`
	} `json:"workload"`
	Environment struct {
		Kubernetes         string `json:"kubernetes"`
		ManagerReplicas    int    `json:"managerReplicas"`
		ManagerMemoryLimit string `json:"managerMemoryLimit"`
		ManagerCPURequest  string `json:"managerCPURequest"`
		MeasuredAt         string `json:"measuredAt"`
	} `json:"environment"`
	Scenarios []struct {
		Name                 string            `json:"name"`
		Outcome              map[string]string `json:"outcome"`
		ManagerRSSMaxBytes   float64           `json:"managerRSSMaxBytes"`
		JobCompletionSeconds struct {
			P95 float64 `json:"p95"`
		} `json:"jobCompletionSeconds"`
	} `json:"scenarios"`
}

func (r labReading) scenario(t *testing.T, name string) (map[string]string, float64) {
	t.Helper()
	for _, entry := range r.Scenarios {
		if entry.Name == name {
			return entry.Outcome, entry.JobCompletionSeconds.P95
		}
	}
	t.Fatalf("the reading has no %q scenario", name)
	return nil, 0
}

func (r labReading) outcome(t *testing.T, scenario, key string) string {
	t.Helper()
	outcome, _ := r.scenario(t, scenario)
	value := outcome[key]
	if value == "" {
		t.Fatalf("the reading's %q scenario records no %s", scenario, key)
	}
	return value
}

func (r labReading) peakRSSMiB() int {
	peak := 0.0
	for _, entry := range r.Scenarios {
		peak = math.Max(peak, entry.ManagerRSSMaxBytes)
	}
	return int(math.Ceil(peak / (1 << 20)))
}

// short writes a whole number of minutes the way the capacity page does, 2m
// rather than Go's 2m0s, and leaves any other duration as Go writes it.
func short(value string) string {
	duration, err := time.ParseDuration(value)
	if err != nil || duration == 0 || duration%time.Minute != 0 {
		return value
	}
	return fmt.Sprintf("%dm", duration/time.Minute)
}

func (r labReading) scope() string {
	return fmt.Sprintf(
		"Lab-scoped: measured once by the %s reading (%s) on one GitHub-hosted runner, Kubernetes %s, "+
			"%d manager replicas with the chart's resource defaults (%s CPU request, %s memory limit), "+
			"with %d PtahSchemas and %d PtahMigrations on a database each at a %s interval. "+
			"It describes that lab and is not a target for any installation.",
		r.Workload.Name, r.Environment.MeasuredAt, r.Environment.Kubernetes,
		r.Environment.ManagerReplicas, r.Environment.ManagerCPURequest, r.Environment.ManagerMemoryLimit,
		r.Workload.Schemas, r.Workload.Migrations, short(r.Workload.Interval))
}

func expectedLabTargets(t *testing.T, r labReading) string {
	_, steadyP95 := r.scenario(t, "steady state")
	return r.scope() + " " + fmt.Sprintf(
		"Converged from a cold start in %s. Operation Jobs finished within %gs at p95 in the steady state. "+
			"A change to %s resources converged in %s. Manager memory peaked at %d MiB.",
		r.outcome(t, "cold start", "converged"), steadyP95,
		r.outcome(t, "change batch", "moved"), r.outcome(t, "change batch", "converged"),
		r.peakRSSMiB())
}

func expectedLabRecovery(t *testing.T, r labReading) string {
	return r.scope() + " " + fmt.Sprintf(
		"Operator process: with every manager replaced at once the workload reconverged in %s, "+
			"and after a %s registry outage it reconverged in %s. "+
			"Operator state: a rebuild from backed-up specs runs nothing a lost approval authorized "+
			"(run_rebuild_drill); status and run evidence newer than the backup are lost, "+
			"so its recovery point is the age of the backup, and no recovery time is measured. "+
			"Database: none is claimed. The lab restores no database, and its recovery point and "+
			"recovery time belong to the deployment's own backup system.",
		r.outcome(t, "restart burst", "converged"), short(r.Workload.Outage),
		r.outcome(t, "recovery", "converged"))
}

func TestTheLabProfileIsTheReadingItNames(t *testing.T) {
	t.Parallel()
	declared, err := readProfile(filepath.Join(repositoryRoot, labProfilePath))
	if err != nil {
		t.Fatalf("the lab profile is refused: %v", err)
	}
	var reading labReading
	if err := json.Unmarshal([]byte(readRepositoryFile(t, labReadingPath)), &reading); err != nil {
		t.Fatalf("read %s: %v", labReadingPath, err)
	}
	if want := expectedLabTargets(t, reading); declared.OperatingTargets != want {
		t.Errorf("operatingTargets is not the reading's own:\n got: %s\nwant: %s", declared.OperatingTargets, want)
	}
	if want := expectedLabRecovery(t, reading); declared.RecoveryObjectives != want {
		t.Errorf("recoveryObjectives is not the reading's own:\n got: %s\nwant: %s", declared.RecoveryObjectives, want)
	}
	// Scoped to the lab, and deciding nothing: the candidate's digests are
	// the release's to fill, and no requirement has evidence about it yet.
	if declared.Decision != dispositionNotAssessed {
		t.Errorf("the lab profile decides %q; nothing it carries is evidence about a candidate", declared.Decision)
	}
	for id, verdict := range declared.Requirements {
		if verdict.Disposition != dispositionNotAssessed {
			t.Errorf("the lab profile disposes of %s as %q from a lab reading", id, verdict.Disposition)
		}
	}
	if !strings.Contains(declared.RunEvidence, "capacity") || !strings.Contains(declared.InstallationValues, "lab") {
		t.Error("the lab profile no longer says its evidence and its values are the lab's")
	}
}
