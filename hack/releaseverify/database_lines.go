package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// A supported added database line is in the support table on the strength of a
// run in a workflow of its own. The catalog names that run, and
// hack/verify-kubernetes-support.go can only hold the name's shape, because it
// reads no network. This reads what the name points at, for the release preflight.
const (
	databaseCatalogPath   = "support/databases.json"
	databaseLinesWorkflow = ".github/workflows/database-lines.yml"
	// databaseLinesCallerJob is the job in that workflow that calls ci.yml, which
	// prefixes the name of every job it runs.
	databaseLinesCallerJob = "Added lines"
	databaseLineSupported  = "supported"
	// databaseLineJobsPage is what the API allows on one page of a run's jobs.
	databaseLineJobsPage = 100
)

var databaseEvidenceRunPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

type databaseCatalogLine struct {
	Engine      string `json:"engine"`
	Line        string `json:"line"`
	Image       string `json:"image"`
	State       string `json:"state"`
	EvidenceRun string `json:"evidenceRun"`
	LeftOut     []struct {
		Suite string `json:"suite"`
	} `json:"leftOut"`
}

// databaseLineEvidence is what the API says about one run: the run, its jobs on
// the attempt that last ran each, and the catalog at the commit it ran.
type databaseLineEvidence struct {
	run       workflowRunRecord
	jobs      []workflowJobRecord
	totalJobs int
	catalog   []databaseCatalogLine
}

func parseDatabaseCatalogLines(document []byte) ([]databaseCatalogLine, error) {
	var catalog struct {
		Added []databaseCatalogLine `json:"added"`
	}
	if err := json.Unmarshal(document, &catalog); err != nil {
		return nil, fmt.Errorf("parse %s: %w", databaseCatalogPath, err)
	}
	return catalog.Added, nil
}

func readDatabaseCatalogLines(root string) ([]databaseCatalogLine, error) {
	document, err := readEvidenceFile(filepath.Join(root, filepath.FromSlash(databaseCatalogPath)))
	if err != nil {
		return nil, err
	}
	return parseDatabaseCatalogLines(document)
}

// verifySupportedDatabaseLines holds each supported line in the catalog at root to
// the run it names. With no supported line it reads nothing and needs no token.
func verifySupportedDatabaseLines(ctx context.Context, client *http.Client, apiURL, repository, root, token string) error {
	lines, err := readDatabaseCatalogLines(root)
	if err != nil {
		return err
	}
	var supported []databaseCatalogLine
	for _, line := range lines {
		if line.State == databaseLineSupported {
			supported = append(supported, line)
		}
	}
	if len(supported) == 0 {
		return nil
	}
	base, err := releaseAPIBase(client, apiURL, repository, token)
	if err != nil {
		return err
	}
	fetched := map[string]databaseLineEvidence{}
	for _, line := range supported {
		if !databaseEvidenceRunPattern.MatchString(line.EvidenceRun) {
			return fmt.Errorf("%s %s is supported and names run %q", line.Engine, line.Line, line.EvidenceRun)
		}
		required, err := requiredDatabaseLineJobs(root, line)
		if err != nil {
			return err
		}
		evidence, read := fetched[line.EvidenceRun]
		if !read {
			evidence, err = fetchDatabaseLineEvidence(ctx, client, base, token, repository, line.EvidenceRun)
			if err != nil {
				return fmt.Errorf("%s %s names run %s, which could not be read: %w", line.Engine, line.Line, line.EvidenceRun, err)
			}
			fetched[line.EvidenceRun] = evidence
		}
		if err := checkDatabaseLineEvidence(evidence, line, required); err != nil {
			return fmt.Errorf("%s %s names run %s, which cannot be its evidence: %w", line.Engine, line.Line, line.EvidenceRun, err)
		}
	}
	return nil
}

// requiredDatabaseLineJobs are the jobs a run has to have concluded in success
// for this line: the ones every line of the run shares, and one lifecycle per
// minor and suite this commit supports. The gate is not among them and the run's
// own conclusion is not read, because a failed cell on another line fails both. A
// window or a suite that moved since the run leaves a job missing.
func requiredDatabaseLineJobs(root string, line databaseCatalogLine) ([]string, error) {
	major, err := phases.PostgresImageMajor(line.Image)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", databaseCatalogPath, err)
	}
	window, err := repositoryKubernetesSupportWindow(root)
	if err != nil {
		return nil, err
	}
	suites, err := readAcceptanceSuites(root)
	if err != nil {
		return nil, err
	}
	leftOut := map[string]bool{}
	for _, suite := range line.LeftOut {
		leftOut[suite.Suite] = true
	}
	jobs := make([]string, 0, len(gatePrerequisiteJobs)+len(suites)*3)
	for _, name := range gatePrerequisiteJobs {
		jobs = append(jobs, databaseLinesCallerJob+" / "+name)
	}
	for _, minor := range strings.Split(window, ",") {
		for _, suite := range suites {
			if !leftOut[suite.Name] {
				jobs = append(jobs, fmt.Sprintf("%s / Kubernetes %s on PostgreSQL %d %s", databaseLinesCallerJob, minor, major, suite.Name))
			}
		}
	}
	return jobs, nil
}

func fetchDatabaseLineEvidence(ctx context.Context, client *http.Client, base *url.URL, token, repository, run string) (databaseLineEvidence, error) {
	read := func(path string, target any) error {
		body, status, err := readReleaseAPI(ctx, client, base, token, path)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("%s answered %d", path, status)
		}
		if err := json.Unmarshal(body, target); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		return nil
	}
	prefix := "repos/" + repository
	var evidence databaseLineEvidence
	if err := read(prefix+"/actions/runs/"+run, &evidence.run); err != nil {
		return evidence, err
	}
	// The commit goes into a URL below, so its shape is settled first.
	if !commitPattern.MatchString(evidence.run.HeadSHA) {
		return evidence, fmt.Errorf("it names commit %q", evidence.run.HeadSHA)
	}
	for page := 1; ; page++ {
		var listing struct {
			TotalCount int                 `json:"total_count"`
			Jobs       []workflowJobRecord `json:"jobs"`
		}
		path := fmt.Sprintf("%s/actions/runs/%s/jobs?filter=latest&per_page=%d&page=%d", prefix, run, databaseLineJobsPage, page)
		if err := read(path, &listing); err != nil {
			return evidence, err
		}
		evidence.jobs = append(evidence.jobs, listing.Jobs...)
		evidence.totalJobs = listing.TotalCount
		if len(listing.Jobs) == 0 || len(evidence.jobs) >= listing.TotalCount {
			break
		}
	}
	var file struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := read(prefix+"/contents/"+databaseCatalogPath+"?ref="+evidence.run.HeadSHA, &file); err != nil {
		return evidence, err
	}
	if file.Encoding != "base64" {
		return evidence, fmt.Errorf("%s at %s came in encoding %q", databaseCatalogPath, evidence.run.HeadSHA, file.Encoding)
	}
	document, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return evidence, fmt.Errorf("decode %s at %s: %w", databaseCatalogPath, evidence.run.HeadSHA, err)
	}
	evidence.catalog, err = parseDatabaseCatalogLines(document)
	return evidence, err
}

func checkDatabaseLineEvidence(evidence databaseLineEvidence, line databaseCatalogLine, required []string) error {
	run := evidence.run
	if strconv.FormatInt(run.ID, 10) != line.EvidenceRun {
		return fmt.Errorf("the record read is of run %d", run.ID)
	}
	// A pull request from a fork runs that fork's copy of the workflow, so its
	// green says nothing about the jobs this repository runs.
	if run.HeadRepository.FullName != repositoryName {
		return fmt.Errorf("it ran from %q, not %s", run.HeadRepository.FullName, repositoryName)
	}
	if run.Path != databaseLinesWorkflow && !strings.HasPrefix(run.Path, databaseLinesWorkflow+"@") {
		return fmt.Errorf("it is a run of %q, not %s", run.Path, databaseLinesWorkflow)
	}
	if run.Status != "completed" {
		return fmt.Errorf("it is %s", run.Status)
	}
	image := ""
	for _, candidate := range evidence.catalog {
		if candidate.Engine == line.Engine && candidate.Line == line.Line {
			image = candidate.Image
		}
	}
	if image != line.Image {
		return fmt.Errorf("commit %s runs %q for this line, and this commit pins %s", run.HeadSHA, image, line.Image)
	}
	if len(evidence.jobs) != evidence.totalJobs {
		return fmt.Errorf("the job listing holds %d of %d jobs, so it is a partial page", len(evidence.jobs), evidence.totalJobs)
	}
	byName := make(map[string][]workflowJobRecord, len(evidence.jobs))
	for _, job := range evidence.jobs {
		byName[job.Name] = append(byName[job.Name], job)
	}
	for _, name := range required {
		matches := byName[name]
		if len(matches) != 1 {
			return fmt.Errorf("it lists job %q %d times, expected once; a minor or a suite that moved since the run needs a new one", name, len(matches))
		}
		job := matches[0]
		conclusion := ""
		if job.Conclusion != nil {
			conclusion = *job.Conclusion
		}
		if job.RunID != run.ID || job.HeadSHA != run.HeadSHA {
			return fmt.Errorf("job %q belongs to run %d of %s", name, job.RunID, job.HeadSHA)
		}
		if job.Status != "completed" || conclusion != "success" {
			return fmt.Errorf("job %q concluded %q (status %q); only a success passes", name, conclusion, job.Status)
		}
	}
	return nil
}
