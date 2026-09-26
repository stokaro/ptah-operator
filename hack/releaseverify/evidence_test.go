package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	evidenceSourceSHA = "1111111111111111111111111111111111111111"
	evidenceRunID     = 456
	evidenceEpoch     = 1790000000
)

// evidenceInput is what the release workflow fetches before it bundles: the
// run and its jobs as the API returns them, the record, and one download per
// lifecycle.
type evidenceInput struct {
	run     map[string]any
	jobs    []map[string]any
	total   int
	record  string
	files   map[string]string // relative to artifacts/
	skipRun bool
}

func validEvidenceInput(t *testing.T, root string) *evidenceInput {
	t.Helper()
	required, err := requiredAcceptanceJobs(root)
	if err != nil {
		t.Fatal(err)
	}
	input := &evidenceInput{
		run: map[string]any{
			"id": evidenceRunID, "run_attempt": 2, "head_sha": evidenceSourceSHA, "event": "push",
			"path": ".github/workflows/ci.yml", "status": "completed", "conclusion": "success",
		},
		record: "# Acceptance record\n\n| Source commit | " + evidenceSourceSHA + " |\n",
		files:  map[string]string{},
	}
	for index, job := range required {
		// A job concluded on either attempt: re-running the failed jobs of a
		// run leaves the ones that passed on the attempt that ran them.
		attempt := 2
		if index%2 == 0 {
			attempt = 1
		}
		input.jobs = append(input.jobs, map[string]any{
			"id": 9000 + index, "run_id": evidenceRunID, "run_attempt": attempt, "name": job.name,
			"head_sha": evidenceSourceSHA, "status": "completed", "conclusion": "success",
		})
		if job.artifact == "" {
			continue
		}
		for _, name := range job.acceptanceArtifactFiles() {
			input.files[filepath.Join(job.artifact, name)] = "measured by " + job.name + " into " + name + "\n"
		}
	}
	// Jobs the release does not require are in the listing too.
	input.jobs = append(input.jobs, map[string]any{
		"id": 8999, "run_id": evidenceRunID, "run_attempt": 1, "name": "Publish lifecycle timings",
		"head_sha": evidenceSourceSHA, "status": "completed", "conclusion": "failure",
	})
	return input
}

func (input *evidenceInput) job(t *testing.T, name string) map[string]any {
	t.Helper()
	for _, job := range input.jobs {
		if job["name"] == name {
			return job
		}
	}
	t.Fatalf("the fixture has no job %q", name)
	return nil
}

func (input *evidenceInput) write(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	writeJSON := func(name string, value any) {
		document, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), document, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !input.skipRun {
		writeJSON("run.json", input.run)
	}
	total := input.total
	if total == 0 {
		total = len(input.jobs)
	}
	writeJSON("run-jobs.json", map[string]any{"total_count": total, "jobs": input.jobs})
	if err := os.WriteFile(filepath.Join(directory, acceptanceEvidenceRecord), []byte(input.record), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range input.files {
		target := filepath.Join(directory, "artifacts", name)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// validEvidenceBundle is a bundle the release would build for the fixture run.
func validEvidenceBundle(t *testing.T, root string) []byte {
	t.Helper()
	bundle, err := buildAcceptanceEvidence(root, validEvidenceInput(t, root).write(t), evidenceSourceSHA, evidenceEpoch)
	if err != nil {
		t.Fatalf("buildAcceptanceEvidence(valid) error = %v", err)
	}
	return bundle
}

func TestTheAcceptanceEvidenceIsDeterministic(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	first := validEvidenceBundle(t, root)

	// Another download of the same run: files written in another order, at
	// another time, with other permissions.
	input := validEvidenceInput(t, root)
	directory := input.write(t)
	later := time.Now().Add(time.Hour)
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if err := os.Chmod(path, 0o640); err != nil {
			return err
		}
		return os.Chtimes(path, later, later)
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildAcceptanceEvidence(root, directory, evidenceSourceSHA, evidenceEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two builds of the same evidence produced different bytes")
	}

	decompressor, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	if !decompressor.ModTime.IsZero() {
		t.Fatalf("the gzip header carries the time %s", decompressor.ModTime)
	}
	archive := tar.NewReader(decompressor)
	var names []string
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.ModTime.Unix() != evidenceEpoch || header.Uid != 0 || header.Gid != 0 || header.Mode != 0o644 {
			t.Fatalf("entry %s has mtime %d, owner %d:%d and mode %o", header.Name, header.ModTime.Unix(), header.Uid, header.Gid, header.Mode)
		}
		names = append(names, header.Name)
	}
	required, err := requiredAcceptanceJobs(root)
	if err != nil {
		t.Fatal(err)
	}
	lifecycles := 0
	for _, job := range required {
		if job.artifact != "" {
			lifecycles++
		}
	}
	// jobs.json, the record, and four files from every lifecycle.
	if len(names) != 2+4*lifecycles || lifecycles == 0 {
		t.Fatalf("the bundle holds %d entries for %d lifecycles", len(names), lifecycles)
	}
	for index := 1; index < len(names); index++ {
		if names[index-1] >= names[index] {
			t.Fatalf("entries %q and %q are out of order", names[index-1], names[index])
		}
	}
	if err := verifyAcceptanceEvidence(root, first, evidenceSourceSHA, evidenceRunID); err != nil {
		t.Fatalf("verifyAcceptanceEvidence(valid) error = %v", err)
	}
}

// The bundle is built only from a run whose every required job succeeded,
// and only from a complete set of what those jobs uploaded.
func TestBuildingTheAcceptanceEvidenceRefusesAnIncompleteRun(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	lifecycle := "Kubernetes 1.36 data-plane"
	artifact := "lifecycle-timings-1-36-data-plane"

	for _, row := range []struct {
		name    string
		mutate  func(t *testing.T, input *evidenceInput)
		problem string
	}{
		{
			name:    "a canceled lifecycle",
			mutate:  func(t *testing.T, input *evidenceInput) { input.job(t, lifecycle)["conclusion"] = "cancelled" },
			problem: `required job "Kubernetes 1.36 data-plane" concluded "cancelled"`,
		},
		{
			name:    "a skipped lifecycle",
			mutate:  func(t *testing.T, input *evidenceInput) { input.job(t, lifecycle)["conclusion"] = "skipped" },
			problem: `concluded "skipped"`,
		},
		{
			name:    "a failed gate",
			mutate:  func(t *testing.T, input *evidenceInput) { input.job(t, supportGateJobName)["conclusion"] = "failure" },
			problem: `required job "Kubernetes support gate" concluded "failure"`,
		},
		{
			name: "a lifecycle still running",
			mutate: func(t *testing.T, input *evidenceInput) {
				input.job(t, lifecycle)["status"] = "in_progress"
				input.job(t, lifecycle)["conclusion"] = nil
			},
			problem: `(status "in_progress")`,
		},
		{
			name: "a missing lifecycle",
			mutate: func(t *testing.T, input *evidenceInput) {
				input.job(t, lifecycle)["name"] = "Kubernetes 1.36 something-else"
			},
			problem: `lists required job "Kubernetes 1.36 data-plane" 0 times`,
		},
		{
			name: "a lifecycle listed twice",
			mutate: func(t *testing.T, input *evidenceInput) {
				duplicate := map[string]any{}
				for key, value := range input.job(t, lifecycle) {
					duplicate[key] = value
				}
				duplicate["id"] = 1
				input.jobs = append(input.jobs, duplicate)
			},
			problem: `lists required job "Kubernetes 1.36 data-plane" 2 times`,
		},
		{
			name:    "a job of another run",
			mutate:  func(t *testing.T, input *evidenceInput) { input.job(t, lifecycle)["run_id"] = 457 },
			problem: "belongs to run 457",
		},
		{
			name:    "a job from an attempt the run has not had",
			mutate:  func(t *testing.T, input *evidenceInput) { input.job(t, lifecycle)["run_attempt"] = 3 },
			problem: "which run attempt 2 cannot have",
		},
		{
			name:    "one page of a longer listing",
			mutate:  func(t *testing.T, input *evidenceInput) { input.total = len(input.jobs) + 20 },
			problem: "so it is a partial page",
		},
		{
			name:    "a run that failed",
			mutate:  func(t *testing.T, input *evidenceInput) { input.run["conclusion"] = "failure" },
			problem: `with conclusion "failure"`,
		},
		{
			name:    "a run of another commit",
			mutate:  func(t *testing.T, input *evidenceInput) { input.run["head_sha"] = strings.Repeat("2", 40) },
			problem: "the release needs a successful push run of " + evidenceSourceSHA,
		},
		{
			name:    "a run of another workflow",
			mutate:  func(t *testing.T, input *evidenceInput) { input.run["path"] = ".github/workflows/capacity.yml" },
			problem: `is of workflow ".github/workflows/capacity.yml"`,
		},
		{
			name:    "no run record",
			mutate:  func(t *testing.T, input *evidenceInput) { input.skipRun = true },
			problem: "run.json",
		},
		{
			name: "a lifecycle's artifact missing",
			mutate: func(t *testing.T, input *evidenceInput) {
				for name := range input.files {
					if strings.HasPrefix(name, artifact+string(filepath.Separator)) {
						delete(input.files, name)
					}
				}
			},
			problem: "lack " + artifact,
		},
		{
			name: "an artifact short of a file",
			mutate: func(t *testing.T, input *evidenceInput) {
				delete(input.files, filepath.Join(artifact, "resource-samples-1-36-data-plane.csv"))
			},
			problem: "artifact " + artifact + " holds",
		},
		{
			name: "an empty measurement",
			mutate: func(t *testing.T, input *evidenceInput) {
				input.files[filepath.Join(artifact, "timings-1-36-data-plane.jsonl")] = ""
			},
			problem: "must be a non-empty regular file",
		},
		{
			name: "an artifact no required job uploads",
			mutate: func(t *testing.T, input *evidenceInput) {
				input.files[filepath.Join("shared-task-images", "images.json")] = "{}\n"
			},
			problem: `hold "shared-task-images"`,
		},
		{
			name: "the record of another commit",
			mutate: func(t *testing.T, input *evidenceInput) {
				input.record = "| Source commit | " + strings.Repeat("2", 40) + " |\n"
			},
			problem: "is not the acceptance record at " + evidenceSourceSHA,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			input := validEvidenceInput(t, root)
			row.mutate(t, input)
			_, err := buildAcceptanceEvidence(root, input.write(t), evidenceSourceSHA, evidenceEpoch)
			if err == nil {
				t.Fatal("buildAcceptanceEvidence() accepted the run")
			}
			if !strings.Contains(err.Error(), row.problem) {
				t.Fatalf("buildAcceptanceEvidence() said %q, which does not carry %q", err, row.problem)
			}
		})
	}
}

// A published bundle is checked again on its own. Each row changes one thing
// in a valid bundle and writes it the deterministic way, so the refusal comes
// from the check the row names rather than from the encoding.
func TestVerifyingTheAcceptanceEvidenceRefusesWhatItMustNotCarry(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	files, modified := readEvidenceBundle(t, validEvidenceBundle(t, root))

	withJobs := func(mutate func(*acceptanceEvidence)) func(map[string][]byte) {
		return func(files map[string][]byte) {
			var evidence acceptanceEvidence
			if err := json.Unmarshal(files[acceptanceEvidenceJobs], &evidence); err != nil {
				t.Fatal(err)
			}
			mutate(&evidence)
			document, err := canonicalAcceptanceEvidence(evidence)
			if err != nil {
				t.Fatal(err)
			}
			files[acceptanceEvidenceJobs] = document
		}
	}
	lifecycleIndex := len(gatePrerequisiteJobs) + 2

	for _, row := range []struct {
		name    string
		mutate  func(map[string][]byte)
		runID   int64
		raw     func([]byte) []byte
		problem string
	}{
		{
			name: "a jobs.json naming a failed required job",
			mutate: withJobs(func(evidence *acceptanceEvidence) {
				evidence.Jobs[lifecycleIndex].Conclusion = "failure"
			}),
			problem: `concluded "failure"`,
		},
		{
			name: "a jobs.json naming a canceled required job",
			mutate: withJobs(func(evidence *acceptanceEvidence) {
				evidence.Jobs[len(evidence.Jobs)-1].Conclusion = "cancelled"
			}),
			problem: `required job "Kubernetes support gate" concluded "cancelled"`,
		},
		{
			name: "a jobs.json that dropped a required job",
			mutate: withJobs(func(evidence *acceptanceEvidence) {
				evidence.Jobs = evidence.Jobs[:len(evidence.Jobs)-1]
			}),
			problem: "and the release requires",
		},
		{
			name:    "the evidence of another run",
			runID:   evidenceRunID + 1,
			problem: "names CI run 456, and the release manifest names run 457",
		},
		{
			name: "the evidence of another commit",
			mutate: withJobs(func(evidence *acceptanceEvidence) {
				evidence.SourceSHA = strings.Repeat("2", 40)
			}),
			problem: "and the release is of " + evidenceSourceSHA,
		},
		{
			name: "a jobs.json written by hand",
			mutate: func(files map[string][]byte) {
				files[acceptanceEvidenceJobs] = bytes.ReplaceAll(files[acceptanceEvidenceJobs], []byte("  "), []byte("\t"))
			},
			problem: "is not canonical JSON",
		},
		{
			name: "a lifecycle's file missing",
			mutate: func(files map[string][]byte) {
				delete(files, "artifacts/lifecycle-timings-1-35-lifecycle/timing-report-1-35-lifecycle.json")
			},
			problem: "it must hold exactly jobs.json, the record and every lifecycle's artifact",
		},
		{
			name: "a file nothing asked for",
			mutate: func(files map[string][]byte) {
				files["notes.txt"] = []byte("added later\n")
			},
			problem: "it must hold exactly jobs.json, the record and every lifecycle's artifact",
		},
		{
			name: "a gzip header that carries a time",
			raw: func(bundle []byte) []byte {
				var buffer bytes.Buffer
				decompressor, err := gzip.NewReader(bytes.NewReader(bundle))
				if err != nil {
					t.Fatal(err)
				}
				archive, err := io.ReadAll(decompressor)
				if err != nil {
					t.Fatal(err)
				}
				compressor, err := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
				if err != nil {
					t.Fatal(err)
				}
				compressor.ModTime = time.Unix(evidenceEpoch, 0)
				if _, err := compressor.Write(archive); err != nil {
					t.Fatal(err)
				}
				if err := compressor.Close(); err != nil {
					t.Fatal(err)
				}
				return buffer.Bytes()
			},
			problem: "gzip header carries a time",
		},
		{
			name: "another compression of the same files",
			raw: func(bundle []byte) []byte {
				var buffer bytes.Buffer
				decompressor, err := gzip.NewReader(bytes.NewReader(bundle))
				if err != nil {
					t.Fatal(err)
				}
				archive, err := io.ReadAll(decompressor)
				if err != nil {
					t.Fatal(err)
				}
				compressor, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := compressor.Write(archive); err != nil {
					t.Fatal(err)
				}
				if err := compressor.Close(); err != nil {
					t.Fatal(err)
				}
				return buffer.Bytes()
			},
			problem: "is not the deterministic encoding",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			copied := make(map[string][]byte, len(files))
			for name, content := range files {
				copied[name] = append([]byte(nil), content...)
			}
			if row.mutate != nil {
				row.mutate(copied)
			}
			bundle, err := writeAcceptanceEvidence(copied, modified)
			if err != nil {
				t.Fatal(err)
			}
			if row.raw != nil {
				bundle = row.raw(bundle)
			}
			runID := row.runID
			if runID == 0 {
				runID = evidenceRunID
			}
			err = verifyAcceptanceEvidence(root, bundle, evidenceSourceSHA, runID)
			if err == nil {
				t.Fatal("verifyAcceptanceEvidence() accepted the bundle")
			}
			if !strings.Contains(err.Error(), row.problem) {
				t.Fatalf("verifyAcceptanceEvidence() said %q, which does not carry %q", err, row.problem)
			}
		})
	}
}

func readEvidenceBundle(t *testing.T, bundle []byte) (map[string][]byte, time.Time) {
	t.Helper()
	decompressor, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(decompressor)
	files := map[string][]byte{}
	var modified time.Time
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = content
		modified = header.ModTime
	}
	return files, modified
}

// The release requires the jobs CI runs, by the names CI gives them. Both the
// lifecycle matrix and the gate's prerequisites are written in two places, so
// this reads the other place and holds them together.
func TestTheRequiredJobsAreTheOnesCIRuns(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	required, err := requiredAcceptanceJobs(root)
	if err != nil {
		t.Fatal(err)
	}

	command := exec.Command("go", "run", "./hack/verify-kubernetes-support.go", "-output=acceptance")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatalf("hack/verify-kubernetes-support.go -output=acceptance: %v", err)
	}
	var matrix []struct {
		Minor     string `json:"minor"`
		MinorSlug string `json:"minor_slug"`
		Suite     string `json:"suite"`
		SuiteSlug string `json:"suite_slug"`
	}
	if err := json.Unmarshal(output, &matrix); err != nil {
		t.Fatal(err)
	}
	var lifecycles []acceptanceJob
	for _, job := range required {
		if job.artifact != "" {
			lifecycles = append(lifecycles, job)
		}
	}
	if len(matrix) == 0 || len(lifecycles) != len(matrix) {
		t.Fatalf("the release requires %d lifecycles, and CI runs %d", len(lifecycles), len(matrix))
	}
	for index, entry := range matrix {
		slug := entry.MinorSlug + "-" + entry.SuiteSlug
		want := acceptanceJob{
			name:     "Kubernetes " + entry.Minor + " " + entry.Suite,
			artifact: "lifecycle-timings-" + slug,
			slug:     slug,
		}
		if lifecycles[index] != want {
			t.Fatalf("lifecycle %d is %#v, and CI runs %#v", index, lifecycles[index], want)
		}
	}

	document, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Name  string             `yaml:"name"`
			Needs workflowStringList `yaml:"needs"`
			Steps []struct {
				Uses string         `yaml:"uses"`
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(document, &workflow); err != nil {
		t.Fatal(err)
	}
	gate, ok := workflow.Jobs["kubernetes-support-gate"]
	if !ok || gate.Name != supportGateJobName {
		t.Fatalf("ci.yml has no job named %q", supportGateJobName)
	}
	var prerequisites []string
	for _, need := range gate.Needs {
		if need == "kubernetes-e2e" {
			continue
		}
		prerequisites = append(prerequisites, workflow.Jobs[need].Name)
	}
	if strings.Join(prerequisites, "\n") != strings.Join(gatePrerequisiteJobs, "\n") {
		t.Fatalf("the gate needs %q besides the lifecycles, and the release requires %q", prerequisites, gatePrerequisiteJobs)
	}
	lifecycle := workflow.Jobs["kubernetes-e2e"]
	if lifecycle.Name != "Kubernetes ${{ matrix.minor }} ${{ matrix.suite }}" {
		t.Fatalf("the lifecycle jobs are named %q", lifecycle.Name)
	}
	uploads := 0
	for _, step := range lifecycle.Steps {
		if !strings.HasPrefix(step.Uses, "actions/upload-artifact@") || value(step.With, "name") != "lifecycle-timings-${{ matrix.minor_slug }}-${{ matrix.suite_slug }}" {
			continue
		}
		uploads++
		var files []string
		for _, line := range strings.Split(strings.TrimSpace(value(step.With, "path")), "\n") {
			files = append(files, strings.TrimPrefix(strings.TrimSpace(line), "${{ runner.temp }}/"))
		}
		template := acceptanceJob{slug: "${{ matrix.minor_slug }}-${{ matrix.suite_slug }}"}
		want := template.acceptanceArtifactFiles()
		got := append([]string(nil), files...)
		if len(got) != len(want) {
			t.Fatalf("the lifecycle uploads %q, and the release keeps %q", got, want)
		}
		for _, name := range want {
			found := false
			for _, file := range got {
				found = found || file == name
			}
			if !found {
				t.Fatalf("the lifecycle uploads %q, which does not include %q", got, name)
			}
		}
	}
	if uploads != 1 {
		t.Fatalf("the lifecycle job uploads its timings %d times, expected once", uploads)
	}
}
