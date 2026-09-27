package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The acceptance evidence is what a release keeps of the CI run that proved
// it: the run's required jobs as the GitHub API reported them, the timing
// artifacts every acceptance job uploaded, and the acceptance record at the
// release commit. The run's own artifacts expire; this bundle is an asset of
// the immutable release, which does not.
const (
	acceptanceEvidenceAsset    = "acceptance-evidence.tar.gz"
	acceptanceEvidenceJobs     = "jobs.json"
	acceptanceEvidenceRecord   = "acceptance-record.md"
	acceptanceEvidenceWorkflow = ".github/workflows/ci.yml"
	acceptanceSuitesPath       = "support/e2e-suites.json"
	supportGateJobName         = "Kubernetes support gate"
	// acceptanceEvidenceLimit bounds what one file in the bundle may hold. A
	// lifecycle's timing ledger and samples are kilobytes; this is far above
	// them and far below anything a runner would struggle to read.
	acceptanceEvidenceLimit = 64 << 20
)

// gatePrerequisiteJobs are the jobs the Kubernetes support gate needs besides
// the acceptance matrix, by the names .github/workflows/ci.yml gives them.
var gatePrerequisiteJobs = []string{
	"Build Kubernetes support matrix",
	"Verify source and generated files",
	"Race detector",
	"Build the shared task images",
}

var acceptanceSlugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// acceptanceJob is one CI job a release requires to have succeeded.
type acceptanceJob struct {
	name string
	// artifact is the timing artifact the job uploads, and slug the part of
	// its file names that identifies the job. Both are empty for a job that
	// is not an acceptance lifecycle.
	artifact string
	slug     string
}

// acceptanceArtifactFiles are the files each lifecycle job uploads, in the
// names ci.yml gives them.
func (job acceptanceJob) acceptanceArtifactFiles() []string {
	return []string{
		"resource-samples-" + job.slug + ".csv",
		"timing-context-" + job.slug + ".json",
		"timing-report-" + job.slug + ".json",
		"timings-" + job.slug + ".jsonl",
	}
}

// requiredAcceptanceJobs are the jobs of the CI run a release requires, in a
// fixed order: what the support gate needs, one lifecycle per supported minor
// and suite, and the gate itself.
//
// The lifecycle jobs come from the same two catalogs
// hack/verify-kubernetes-support.go builds the matrix from, and
// TestTheRequiredJobsAreTheOnesCIRuns holds the two derivations together.
func requiredAcceptanceJobs(root string) ([]acceptanceJob, error) {
	window, err := repositoryKubernetesSupportWindow(root)
	if err != nil {
		return nil, err
	}
	document, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(acceptanceSuitesPath)))
	if err != nil {
		return nil, fmt.Errorf("read the acceptance suites: %w", err)
	}
	var catalog struct {
		Suites []struct {
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"suites"`
	}
	if err := json.Unmarshal(document, &catalog); err != nil {
		return nil, fmt.Errorf("parse %s: %w", acceptanceSuitesPath, err)
	}
	if len(catalog.Suites) == 0 {
		return nil, fmt.Errorf("%s declares no suite, so a release would require no lifecycle", acceptanceSuitesPath)
	}
	jobs := make([]acceptanceJob, 0, len(gatePrerequisiteJobs)+len(catalog.Suites)*3+1)
	for _, name := range gatePrerequisiteJobs {
		jobs = append(jobs, acceptanceJob{name: name})
	}
	seen := make(map[string]struct{})
	for _, minor := range strings.Split(window, ",") {
		minorSlug := strings.ReplaceAll(minor, ".", "-")
		for _, suite := range catalog.Suites {
			if !acceptanceSlugPattern.MatchString(suite.Slug) || strings.TrimSpace(suite.Name) != suite.Name || suite.Name == "" {
				return nil, fmt.Errorf("%s declares suite %q with slug %q, which cannot name a job and its artifact", acceptanceSuitesPath, suite.Name, suite.Slug)
			}
			slug := minorSlug + "-" + suite.Slug
			if _, duplicate := seen[slug]; duplicate {
				return nil, fmt.Errorf("%s declares suite slug %q twice", acceptanceSuitesPath, suite.Slug)
			}
			seen[slug] = struct{}{}
			jobs = append(jobs, acceptanceJob{
				name:     "Kubernetes " + minor + " " + suite.Name,
				artifact: "lifecycle-timings-" + slug,
				slug:     slug,
			})
		}
	}
	return append(jobs, acceptanceJob{name: supportGateJobName}), nil
}

// acceptanceEvidence is jobs.json: which run proved the release, and each
// required job as the API reported it on the attempt that concluded it.
type acceptanceEvidence struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Repository    string                  `json:"repository"`
	Workflow      string                  `json:"workflow"`
	SourceSHA     string                  `json:"sourceSha"`
	RunID         int64                   `json:"runId"`
	RunAttempt    int                     `json:"runAttempt"`
	Jobs          []acceptanceEvidenceJob `json:"jobs"`
}

type acceptanceEvidenceJob struct {
	Name       string `json:"name"`
	ID         int64  `json:"id"`
	Attempt    int    `json:"attempt"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Artifact   string `json:"artifact,omitempty"`
}

// checkAcceptanceJobs holds jobs.json to the required jobs. Every one must be
// there once, in order, and have concluded in success: a canceled or skipped
// job did not run what it names, so it is no pass.
func checkAcceptanceJobs(evidence acceptanceEvidence, required []acceptanceJob) error {
	if len(evidence.Jobs) != len(required) {
		return fmt.Errorf("acceptance evidence records %d jobs, and the release requires %d", len(evidence.Jobs), len(required))
	}
	for index, want := range required {
		job := evidence.Jobs[index]
		if job.Name != want.name {
			return fmt.Errorf("acceptance evidence job %d is %q, expected %q", index+1, job.Name, want.name)
		}
		if job.Status != "completed" || job.Conclusion != "success" {
			return fmt.Errorf("required job %q concluded %q (status %q) on attempt %d; only a success passes",
				job.Name, job.Conclusion, job.Status, job.Attempt)
		}
		if job.ID <= 0 || job.Attempt < 1 || job.Attempt > evidence.RunAttempt {
			return fmt.Errorf("required job %q has job ID %d and attempt %d, which run attempt %d cannot have",
				job.Name, job.ID, job.Attempt, evidence.RunAttempt)
		}
		if job.Artifact != want.artifact {
			return fmt.Errorf("required job %q names artifact %q, expected %q", job.Name, job.Artifact, want.artifact)
		}
	}
	return nil
}

type workflowRunRecord struct {
	ID         int64   `json:"id"`
	RunAttempt int     `json:"run_attempt"`
	HeadSHA    string  `json:"head_sha"`
	Event      string  `json:"event"`
	Path       string  `json:"path"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
}

type workflowJobRecord struct {
	ID         int64   `json:"id"`
	RunID      int64   `json:"run_id"`
	RunAttempt int     `json:"run_attempt"`
	Name       string  `json:"name"`
	HeadSHA    string  `json:"head_sha"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
}

// buildAcceptanceEvidence assembles the bundle from what the release workflow
// fetched into input: run.json and run-jobs.json from the GitHub API,
// acceptance-record.md, and artifacts/<name>/ for each lifecycle's download.
// It refuses a run or a required job that did not succeed, and an artifact
// set that is not exactly one complete download per lifecycle.
func buildAcceptanceEvidence(root, input, sourceSHA string, epoch int64) ([]byte, error) {
	if !commitPattern.MatchString(sourceSHA) {
		return nil, fmt.Errorf("source SHA %q is not a full lowercase commit SHA", sourceSHA)
	}
	if epoch <= 0 {
		return nil, fmt.Errorf("acceptance evidence timestamp %d is not a commit time", epoch)
	}
	required, err := requiredAcceptanceJobs(root)
	if err != nil {
		return nil, err
	}
	var run workflowRunRecord
	if err := readJSONFile(filepath.Join(input, "run.json"), &run); err != nil {
		return nil, err
	}
	conclusion := ""
	if run.Conclusion != nil {
		conclusion = *run.Conclusion
	}
	if run.ID <= 0 || run.RunAttempt < 1 {
		return nil, fmt.Errorf("the CI run record names run %d attempt %d", run.ID, run.RunAttempt)
	}
	if run.HeadSHA != sourceSHA || run.Event != "push" || run.Status != "completed" || conclusion != "success" {
		return nil, fmt.Errorf("CI run %d attempt %d is a %s run of %s that is %s with conclusion %q; the release needs a successful push run of %s",
			run.ID, run.RunAttempt, run.Event, run.HeadSHA, run.Status, conclusion, sourceSHA)
	}
	if run.Path != acceptanceEvidenceWorkflow && !strings.HasPrefix(run.Path, acceptanceEvidenceWorkflow+"@") {
		return nil, fmt.Errorf("CI run %d is of workflow %q, not %s", run.ID, run.Path, acceptanceEvidenceWorkflow)
	}
	var listing struct {
		TotalCount int                 `json:"total_count"`
		Jobs       []workflowJobRecord `json:"jobs"`
	}
	if err := readJSONFile(filepath.Join(input, "run-jobs.json"), &listing); err != nil {
		return nil, err
	}
	if len(listing.Jobs) != listing.TotalCount {
		return nil, fmt.Errorf("the job listing holds %d of %d jobs, so it is a partial page", len(listing.Jobs), listing.TotalCount)
	}
	byName := make(map[string][]workflowJobRecord, len(listing.Jobs))
	for _, job := range listing.Jobs {
		byName[job.Name] = append(byName[job.Name], job)
	}
	evidence := acceptanceEvidence{
		SchemaVersion: 1,
		Repository:    repositoryName,
		Workflow:      acceptanceEvidenceWorkflow,
		SourceSHA:     sourceSHA,
		RunID:         run.ID,
		RunAttempt:    run.RunAttempt,
	}
	for _, want := range required {
		matches := byName[want.name]
		if len(matches) != 1 {
			return nil, fmt.Errorf("CI run %d lists required job %q %d times, expected once", run.ID, want.name, len(matches))
		}
		job := matches[0]
		if job.RunID != run.ID || job.HeadSHA != sourceSHA {
			return nil, fmt.Errorf("required job %q belongs to run %d of %s, not run %d of %s", want.name, job.RunID, job.HeadSHA, run.ID, sourceSHA)
		}
		jobConclusion := ""
		if job.Conclusion != nil {
			jobConclusion = *job.Conclusion
		}
		evidence.Jobs = append(evidence.Jobs, acceptanceEvidenceJob{
			Name:       job.Name,
			ID:         job.ID,
			Attempt:    job.RunAttempt,
			Status:     job.Status,
			Conclusion: jobConclusion,
			Artifact:   want.artifact,
		})
	}
	if err := checkAcceptanceJobs(evidence, required); err != nil {
		return nil, err
	}
	jobsDocument, err := canonicalAcceptanceEvidence(evidence)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{acceptanceEvidenceJobs: jobsDocument}
	record, err := readEvidenceFile(filepath.Join(input, acceptanceEvidenceRecord))
	if err != nil {
		return nil, err
	}
	files[acceptanceEvidenceRecord] = record
	if err := collectAcceptanceArtifacts(filepath.Join(input, "artifacts"), required, files); err != nil {
		return nil, err
	}
	bundle, err := writeAcceptanceEvidence(files, time.Unix(epoch, 0))
	if err != nil {
		return nil, err
	}
	// The bundle is read back by the check the release applies to it, so a
	// build this program would refuse later is refused now.
	if err := verifyAcceptanceEvidence(root, bundle, sourceSHA, run.ID); err != nil {
		return nil, err
	}
	return bundle, nil
}

// collectAcceptanceArtifacts reads exactly one download per lifecycle, holding
// exactly the files that lifecycle uploads, and nothing else.
func collectAcceptanceArtifacts(directory string, required []acceptanceJob, files map[string][]byte) error {
	want := make(map[string]acceptanceJob)
	for _, job := range required {
		if job.artifact != "" {
			want[job.artifact] = job
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read the downloaded acceptance artifacts: %w", err)
	}
	found := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		job, ok := want[entry.Name()]
		if !ok || !entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("the downloaded acceptance artifacts hold %q, which no required job uploads", entry.Name())
		}
		found[entry.Name()] = struct{}{}
		artifactEntries, err := os.ReadDir(filepath.Join(directory, entry.Name()))
		if err != nil {
			return fmt.Errorf("read artifact %s: %w", entry.Name(), err)
		}
		names := make([]string, 0, len(artifactEntries))
		for _, file := range artifactEntries {
			names = append(names, file.Name())
		}
		wantFiles := job.acceptanceArtifactFiles()
		if strings.Join(names, "\n") != strings.Join(wantFiles, "\n") {
			return fmt.Errorf("artifact %s holds %v, expected exactly %v", entry.Name(), names, wantFiles)
		}
		for _, name := range wantFiles {
			content, err := readEvidenceFile(filepath.Join(directory, entry.Name(), name))
			if err != nil {
				return err
			}
			files[path.Join("artifacts", entry.Name(), name)] = content
		}
	}
	for artifact := range want {
		if _, ok := found[artifact]; !ok {
			return fmt.Errorf("the downloaded acceptance artifacts lack %s", artifact)
		}
	}
	return nil
}

// readEvidenceFile reads one regular, non-empty file. A link could point
// anywhere, and an empty file is a measurement nobody took.
func readEvidenceFile(name string) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("read acceptance evidence: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > acceptanceEvidenceLimit {
		return nil, fmt.Errorf("acceptance evidence %s must be a non-empty regular file within %d bytes", name, acceptanceEvidenceLimit)
	}
	return os.ReadFile(name) //nolint:gosec // A path under the evidence directory the workflow wrote.
}

func readJSONFile(name string, target any) error {
	document, err := readEvidenceFile(name)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(document, target); err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}
	return nil
}

func canonicalAcceptanceEvidence(evidence acceptanceEvidence) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(evidence); err != nil {
		return nil, fmt.Errorf("encode %s: %w", acceptanceEvidenceJobs, err)
	}
	return buffer.Bytes(), nil
}

// writeAcceptanceEvidence writes the bundle so the same files always make the
// same bytes: entries sorted by name, regular files only, one fixed mtime,
// root ownership with no names, mode 0644, and a gzip header with no time.
func writeAcceptanceEvidence(files map[string][]byte, modified time.Time) ([]byte, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var buffer bytes.Buffer
	compressor, err := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	archive := tar.NewWriter(compressor)
	for _, name := range names {
		header := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(files[name])),
			ModTime:  modified.UTC(),
		}
		if err := archive.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("write %s into the acceptance evidence: %w", name, err)
		}
		if _, err := archive.Write(files[name]); err != nil {
			return nil, fmt.Errorf("write %s into the acceptance evidence: %w", name, err)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	if err := compressor.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// verifyAcceptanceEvidence is the check a release applies to its bundle: it
// holds the files required, jobs.json names this commit and this run and a
// success for every required job, the record is the one at this commit, and
// writing the same files again produces the same bytes.
func verifyAcceptanceEvidence(root string, bundle []byte, sourceSHA string, runID int64) error {
	required, err := requiredAcceptanceJobs(root)
	if err != nil {
		return err
	}
	decompressor, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return fmt.Errorf("read the acceptance evidence: %w", err)
	}
	if !decompressor.ModTime.IsZero() || decompressor.Name != "" || decompressor.Comment != "" || len(decompressor.Extra) != 0 {
		return errors.New("the acceptance evidence gzip header carries a time, a name or extra data")
	}
	archive := tar.NewReader(decompressor)
	files := make(map[string][]byte)
	var modified time.Time
	previous := ""
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read the acceptance evidence: %w", err)
		}
		if header.Typeflag != tar.TypeReg || header.Mode != 0o644 || header.Uid != 0 || header.Gid != 0 ||
			header.Uname != "" || header.Gname != "" {
			return fmt.Errorf("acceptance evidence entry %q is not a plain root-owned 0644 file", header.Name)
		}
		if header.Name <= previous || path.Clean(header.Name) != header.Name || path.IsAbs(header.Name) || strings.HasPrefix(header.Name, "../") {
			return fmt.Errorf("acceptance evidence entry %q is out of order or not a clean relative name", header.Name)
		}
		previous = header.Name
		if modified.IsZero() {
			modified = header.ModTime
		} else if !header.ModTime.Equal(modified) {
			return fmt.Errorf("acceptance evidence entry %q has another modification time", header.Name)
		}
		if header.Size <= 0 || header.Size > acceptanceEvidenceLimit {
			return fmt.Errorf("acceptance evidence entry %q is empty or too large", header.Name)
		}
		content, err := io.ReadAll(io.LimitReader(archive, acceptanceEvidenceLimit+1))
		if err != nil {
			return fmt.Errorf("read acceptance evidence entry %q: %w", header.Name, err)
		}
		files[header.Name] = content
	}
	want := []string{acceptanceEvidenceRecord, acceptanceEvidenceJobs}
	for _, job := range required {
		if job.artifact == "" {
			continue
		}
		for _, name := range job.acceptanceArtifactFiles() {
			want = append(want, path.Join("artifacts", job.artifact, name))
		}
	}
	sort.Strings(want)
	got := make([]string, 0, len(files))
	for name := range files {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		return fmt.Errorf("the acceptance evidence holds %d files, and the required jobs make %d; it must hold exactly jobs.json, the record and every lifecycle's artifact", len(got), len(want))
	}

	var evidence acceptanceEvidence
	decoder := json.NewDecoder(bytes.NewReader(files[acceptanceEvidenceJobs]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return fmt.Errorf("parse %s: %w", acceptanceEvidenceJobs, err)
	}
	canonical, err := canonicalAcceptanceEvidence(evidence)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, files[acceptanceEvidenceJobs]) {
		return fmt.Errorf("%s is not canonical JSON", acceptanceEvidenceJobs)
	}
	if evidence.SchemaVersion != 1 || evidence.Repository != repositoryName || evidence.Workflow != acceptanceEvidenceWorkflow {
		return fmt.Errorf("%s describes schema %d of %s in %s", acceptanceEvidenceJobs, evidence.SchemaVersion, evidence.Workflow, evidence.Repository)
	}
	if evidence.SourceSHA != sourceSHA {
		return fmt.Errorf("%s names source %s, and the release is of %s", acceptanceEvidenceJobs, evidence.SourceSHA, sourceSHA)
	}
	if evidence.RunID != runID {
		return fmt.Errorf("%s names CI run %d, and the release manifest names run %d", acceptanceEvidenceJobs, evidence.RunID, runID)
	}
	if evidence.RunAttempt < 1 {
		return fmt.Errorf("%s names run attempt %d", acceptanceEvidenceJobs, evidence.RunAttempt)
	}
	if err := checkAcceptanceJobs(evidence, required); err != nil {
		return err
	}
	if !bytes.Contains(files[acceptanceEvidenceRecord], []byte("| Source commit | "+sourceSHA+" |")) {
		return fmt.Errorf("%s is not the acceptance record at %s", acceptanceEvidenceRecord, sourceSHA)
	}
	rewritten, err := writeAcceptanceEvidence(files, modified)
	if err != nil {
		return err
	}
	if !bytes.Equal(rewritten, bundle) {
		return errors.New("the acceptance evidence is not the deterministic encoding of the files it holds")
	}
	return nil
}
