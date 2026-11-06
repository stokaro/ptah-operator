package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	databaseLineFixtureRun   = "123"
	databaseLineFixtureSHA   = "0123456789abcdef0123456789abcdef01234567"
	databaseLineFixtureImage = "postgres:18-alpine@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	databaseLineFixtureToken = "fixture-token"
)

// databaseLineFixture is a repository root and a fake of the API a run is read
// from. The catalog comes from the fixture hack's own tests read, which names a
// line that never moves. The minors and suites are the repository's, so a run
// green on this window stays described by the same files the check reads.
type databaseLineFixture struct {
	root    string
	catalog map[string]any
	run     map[string]any
	jobs    []map[string]any
	then    []map[string]any

	// What the fake answers beyond those: a status for a path that fails, a total
	// that disagrees with the jobs, and how many jobs fit on a page.
	failures  map[string]int
	totalJobs int
	pageSize  int

	mutex    sync.Mutex
	requests []string
}

func newDatabaseLineFixture(t *testing.T, state string) *databaseLineFixture {
	t.Helper()
	fixture := &databaseLineFixture{root: t.TempDir(), failures: map[string]int{}, pageSize: databaseLineJobsPage}
	if err := os.Mkdir(filepath.Join(fixture.root, "support"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kubernetes.json", "e2e-suites.json"} {
		content, err := os.ReadFile(filepath.Join("..", "..", "support", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture.root, "support", name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "database-lines", "databases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture.catalog); err != nil {
		t.Fatal(err)
	}
	line := fixture.line()
	line["state"] = state
	if state == databaseLineSupported {
		line["evidenceRun"] = databaseLineFixtureRun
	}
	fixture.writeCatalog(t)

	fixture.run = map[string]any{
		"id": 123, "run_attempt": 1, "head_sha": databaseLineFixtureSHA, "event": "pull_request",
		"path": databaseLinesWorkflow, "status": "completed", "conclusion": "success",
		"head_repository": map[string]any{"full_name": repositoryName},
	}
	lines, err := readDatabaseCatalogLines(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	required, err := requiredDatabaseLineJobs(fixture.root, lines[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range required {
		fixture.addJob(name, "success")
	}
	fixture.then = []map[string]any{{"engine": "postgresql", "line": "18.x", "image": databaseLineFixtureImage, "state": "pending"}}
	return fixture
}

func (f *databaseLineFixture) addJob(name, conclusion string) {
	f.jobs = append(f.jobs, map[string]any{
		"id": len(f.jobs) + 1, "run_id": 123, "run_attempt": 1, "name": name, "head_sha": databaseLineFixtureSHA,
		"status": "completed", "conclusion": conclusion,
	})
}

func (f *databaseLineFixture) line() map[string]any {
	return f.catalog["added"].([]any)[0].(map[string]any)
}

func (f *databaseLineFixture) writeCatalog(t *testing.T) {
	t.Helper()
	document, err := json.Marshal(f.catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, filepath.FromSlash(databaseCatalogPath)), document, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *databaseLineFixture) job(t *testing.T, fragment string) map[string]any {
	t.Helper()
	for _, job := range f.jobs {
		if strings.Contains(job["name"].(string), fragment) {
			return job
		}
	}
	t.Fatalf("no job names %q", fragment)
	return nil
}

func (f *databaseLineFixture) requested(fragment string) int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	count := 0
	for _, request := range f.requests {
		if strings.Contains(request, fragment) {
			count++
		}
	}
	return count
}

// serve answers the way the GitHub API does, and refuses a request with no token.
func (f *databaseLineFixture) serve(t *testing.T) *httptest.Server {
	t.Helper()
	prefix := "/repos/" + repositoryName
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		f.mutex.Lock()
		f.requests = append(f.requests, request.URL.RequestURI())
		f.mutex.Unlock()
		if request.Header.Get("Authorization") != "Bearer "+databaseLineFixtureToken {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		path := strings.TrimPrefix(request.URL.Path, prefix)
		if status, failing := f.failures[path]; failing {
			writer.WriteHeader(status)
			return
		}
		var answer any
		switch path {
		case "/actions/runs/" + databaseLineFixtureRun:
			answer = f.run
		case "/actions/runs/" + databaseLineFixtureRun + "/jobs":
			if request.URL.Query().Get("filter") != "latest" {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			page, err := strconv.Atoi(request.URL.Query().Get("page"))
			if err != nil || page < 1 {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			start, end := min((page-1)*f.pageSize, len(f.jobs)), min(page*f.pageSize, len(f.jobs))
			total := f.totalJobs
			if total == 0 {
				total = len(f.jobs)
			}
			answer = map[string]any{"total_count": total, "jobs": f.jobs[start:end]}
		case "/contents/" + databaseCatalogPath:
			document, err := json.Marshal(map[string]any{"added": f.then})
			if err != nil {
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			if request.URL.Query().Get("ref") != f.run["head_sha"] {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			answer = map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(document) + "\n"}
		default:
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(writer).Encode(answer); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (f *databaseLineFixture) verify(t *testing.T) error {
	t.Helper()
	server := f.serve(t)
	return verifySupportedDatabaseLines(context.Background(), server.Client(), server.URL, repositoryName, f.root, databaseLineFixtureToken)
}

func TestASupportedLineNeedsItsOwnJobsGreenAtACommitThatNamesItsImage(t *testing.T) {
	t.Parallel()
	if err := newDatabaseLineFixture(t, databaseLineSupported).verify(t); err != nil {
		t.Fatalf("a run green on the line was refused: %v", err)
	}

	for name, test := range map[string]struct {
		mutate func(*testing.T, *databaseLineFixture)
		want   string
	}{
		"a run of another workflow": {
			func(_ *testing.T, f *databaseLineFixture) { f.run["path"] = ".github/workflows/ci.yml" }, "not " + databaseLinesWorkflow,
		},
		"a run from a fork": {
			func(_ *testing.T, f *databaseLineFixture) {
				f.run["head_repository"] = map[string]any{"full_name": "someone/ptah-operator"}
			}, "it ran from",
		},
		"the record of another run": {
			func(_ *testing.T, f *databaseLineFixture) { f.run["id"] = 124 }, "the record read is of run 124",
		},
		"a run still going": {
			func(_ *testing.T, f *databaseLineFixture) { f.run["status"] = "in_progress" }, "it is in_progress",
		},
		"a commit that is not one": {
			func(_ *testing.T, f *databaseLineFixture) { f.run["head_sha"] = "master" }, "names commit",
		},
		"a commit that ran another image": {
			func(_ *testing.T, f *databaseLineFixture) {
				f.then[0]["image"] = "postgres:18-alpine@sha256:" + strings.Repeat("d", 64)
			}, "this commit pins",
		},
		"a commit that does not have the line": {
			func(_ *testing.T, f *databaseLineFixture) { f.then = nil }, "this commit pins",
		},
		"a re-pin that kept the old run": {
			func(t *testing.T, f *databaseLineFixture) {
				f.line()["image"] = "postgres:18-alpine@sha256:" + strings.Repeat("e", 64)
				f.writeCatalog(t)
			}, "this commit pins",
		},
		"a word for a run": {
			func(t *testing.T, f *databaseLineFixture) {
				f.line()["evidenceRun"] = "green"
				f.writeCatalog(t)
			}, "names run",
		},
		"a failed cell on this line": {
			func(t *testing.T, f *databaseLineFixture) {
				f.job(t, "on PostgreSQL 18 data-plane")["conclusion"] = "failure"
			}, "concluded \"failure\"",
		},
		"a canceled cell on this line": {
			func(t *testing.T, f *databaseLineFixture) {
				f.job(t, "on PostgreSQL 18 lifecycle")["conclusion"] = "cancelled"
			}, "concluded \"cancelled\"",
		},
		"a cell still going": {
			func(t *testing.T, f *databaseLineFixture) {
				job := f.job(t, "on PostgreSQL 18 lifecycle")
				job["status"], job["conclusion"] = "in_progress", nil
			}, "status \"in_progress\"",
		},
		"a failed shared job": {
			func(t *testing.T, f *databaseLineFixture) {
				f.job(t, "Build the shared task images")["conclusion"] = "failure"
			}, "concluded \"failure\"",
		},
		"a run that missed a suite": {
			func(_ *testing.T, f *databaseLineFixture) {
				var kept []map[string]any
				for _, job := range f.jobs {
					if !strings.HasSuffix(job["name"].(string), " schema-faults") {
						kept = append(kept, job)
					}
				}
				f.jobs = kept
			}, "schema-faults",
		},
		"a window that moved since the run": {
			func(_ *testing.T, f *databaseLineFixture) {
				var kept []map[string]any
				for _, job := range f.jobs {
					if !strings.Contains(job["name"].(string), "Kubernetes 1.37 ") {
						kept = append(kept, job)
					}
				}
				f.jobs = kept
			}, "a minor or a suite that moved",
		},
		"a cell listed twice": {
			func(t *testing.T, f *databaseLineFixture) {
				f.jobs = append(f.jobs, f.job(t, "on PostgreSQL 18 lifecycle"))
			}, "2 times",
		},
		"a partial page": {
			func(_ *testing.T, f *databaseLineFixture) { f.totalJobs = len(f.jobs) + 1 }, "partial page",
		},
		"a job of another run": {
			func(t *testing.T, f *databaseLineFixture) { f.job(t, "Race detector")["run_id"] = 124 }, "belongs to run 124",
		},
		"a job of another commit": {
			func(t *testing.T, f *databaseLineFixture) {
				f.job(t, "Race detector")["head_sha"] = strings.Repeat("f", 40)
			}, "belongs to run",
		},
		"a token that cannot read the run": {
			func(_ *testing.T, f *databaseLineFixture) {
				f.failures["/actions/runs/"+databaseLineFixtureRun] = http.StatusForbidden
			}, "answered 403",
		},
		"a run that no longer exists": {
			func(_ *testing.T, f *databaseLineFixture) {
				f.failures["/actions/runs/"+databaseLineFixtureRun] = http.StatusNotFound
			}, "answered 404",
		},
		"a token that cannot read its jobs": {
			func(_ *testing.T, f *databaseLineFixture) {
				f.failures["/actions/runs/"+databaseLineFixtureRun+"/jobs"] = http.StatusForbidden
			}, "answered 403",
		},
		"a catalog that cannot be read at its commit": {
			func(_ *testing.T, f *databaseLineFixture) {
				f.failures["/contents/"+databaseCatalogPath] = http.StatusNotFound
			}, "answered 404",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newDatabaseLineFixture(t, databaseLineSupported)
			test.mutate(t, fixture)
			err := fixture.verify(t)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifySupportedDatabaseLines() error = %v, want %q", err, test.want)
			}
		})
	}

	t.Run("no token", func(t *testing.T) {
		t.Parallel()
		fixture := newDatabaseLineFixture(t, databaseLineSupported)
		server := fixture.serve(t)
		err := verifySupportedDatabaseLines(context.Background(), server.Client(), server.URL, repositoryName, fixture.root, "")
		if err == nil || !strings.Contains(err.Error(), "authenticated token") {
			t.Fatalf("a supported line with no token: %v", err)
		}
	})
	t.Run("another repository", func(t *testing.T) {
		t.Parallel()
		fixture := newDatabaseLineFixture(t, databaseLineSupported)
		server := fixture.serve(t)
		err := verifySupportedDatabaseLines(context.Background(), server.Client(), server.URL, "someone/ptah-operator", fixture.root, databaseLineFixtureToken)
		if err == nil || !strings.Contains(err.Error(), "release repository") {
			t.Fatalf("another repository: %v", err)
		}
	})
}

// A failed cell on another line fails the run's gate and so the run, and neither
// says anything about this line. Both have to pass here, or one pending line would
// hold every supported one.
func TestAFailureOnAnotherLineDoesNotHoldASupportedOne(t *testing.T) {
	t.Parallel()
	fixture := newDatabaseLineFixture(t, databaseLineSupported)
	fixture.run["conclusion"] = "failure"
	fixture.addJob("Added lines / Kubernetes 1.36 on PostgreSQL 16 data-plane", "failure")
	fixture.addJob("Added lines / Kubernetes support gate", "failure")
	if err := fixture.verify(t); err != nil {
		t.Fatalf("a failure on another line held this one: %v", err)
	}
}

// No line is supported yet, and a pending line's run is nobody's evidence, so the
// preflight asks the API nothing and needs no token to do it.
func TestAPendingLineNeedsNoRun(t *testing.T) {
	t.Parallel()
	fixture := newDatabaseLineFixture(t, "pending")
	server := fixture.serve(t)
	if err := verifySupportedDatabaseLines(context.Background(), server.Client(), server.URL, repositoryName, fixture.root, ""); err != nil {
		t.Fatalf("a pending line needed a run: %v", err)
	}
	if fixture.requested("/") != 0 {
		t.Fatalf("a pending line asked the API %v", fixture.requests)
	}
}

// A run's jobs can span pages, and a check that read one would report a job on the
// second as missing.
func TestADatabaseLineRunIsReadPageByPage(t *testing.T) {
	t.Parallel()
	fixture := newDatabaseLineFixture(t, databaseLineSupported)
	fixture.pageSize = 5
	if err := fixture.verify(t); err != nil {
		t.Fatalf("a run on several pages was refused: %v", err)
	}
	if pages := fixture.requested("/jobs?"); pages < 2 {
		t.Fatalf("the jobs came from %d pages; this row measures nothing unless they span two", pages)
	}
}

// Two lines proven by one run read it once, and each is held to its own jobs.
func TestTwoLinesOnOneRunAreEachHeldToTheirOwnJobs(t *testing.T) {
	t.Parallel()
	fixture := newDatabaseLineFixture(t, databaseLineSupported)
	second := map[string]any{"engine": "postgresql", "line": "16.x", "image": "postgres:16-alpine@sha256:" + strings.Repeat("a", 64),
		"state": databaseLineSupported, "evidenceRun": databaseLineFixtureRun, "leftOut": fixture.line()["leftOut"]}
	fixture.catalog["added"] = append(fixture.catalog["added"].([]any), second)
	fixture.writeCatalog(t)
	fixture.then = append(fixture.then, map[string]any{"engine": "postgresql", "line": "16.x", "image": second["image"], "state": "pending"})

	err := fixture.verify(t)
	if err == nil || !strings.Contains(err.Error(), "on PostgreSQL 16") {
		t.Fatalf("a second line with no jobs of its own: %v", err)
	}

	lines, err := readDatabaseCatalogLines(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	required, err := requiredDatabaseLineJobs(fixture.root, lines[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range required[len(gatePrerequisiteJobs):] {
		fixture.addJob(name, "success")
	}
	before := fixture.requested("/actions/runs/" + databaseLineFixtureRun + "/jobs")
	if err := fixture.verify(t); err != nil {
		t.Fatalf("two lines green on one run: %v", err)
	}
	if reads := fixture.requested("/actions/runs/"+databaseLineFixtureRun+"/jobs") - before; reads != 1 {
		t.Fatalf("one run was read %d times", reads)
	}
}

// requiredDatabaseLineJobs and hack/verify-kubernetes-support.go derive the same
// names from the same catalogs in two programs. This holds the two together, and
// the name of the job that prefixes them, the way the acceptance jobs are held.
func TestTheDatabaseLineJobsAreTheOnesTheWorkflowRuns(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	workflow, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(databaseLinesWorkflow)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), "\n    name: "+databaseLinesCallerJob+"\n") {
		t.Fatalf("%s does not name its job %q", databaseLinesWorkflow, databaseLinesCallerJob)
	}
	lines, err := readDatabaseCatalogLines(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 {
		t.Fatal("the catalog adds no line, so this has nothing to compare; move the row to a fixture when the last line leaves")
	}
	command := exec.Command("go", "run", "./hack/verify-kubernetes-support.go", "-output=database-lines")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatalf("hack/verify-kubernetes-support.go -output=database-lines: %v", err)
	}
	var matrix []struct {
		Minor string `json:"minor"`
		Suite string `json:"suite"`
		Image string `json:"postgres_source_image"`
	}
	if err := json.Unmarshal(output, &matrix); err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		required, err := requiredDatabaseLineJobs(root, line)
		if err != nil {
			t.Fatal(err)
		}
		lifecycles := required[len(gatePrerequisiteJobs):]
		var want []string
		for _, entry := range matrix {
			if entry.Image == line.Image {
				want = append(want, databaseLinesCallerJob+" / Kubernetes "+entry.Minor+" "+entry.Suite)
			}
		}
		if len(want) == 0 || strings.Join(lifecycles, "\n") != strings.Join(want, "\n") {
			t.Fatalf("the release reads %q for %s, and the workflow runs %q", lifecycles, line.Line, want)
		}
	}
}
