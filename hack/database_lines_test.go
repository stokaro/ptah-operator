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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// A date well after every pin in the catalog, so the age rule stays out of the
// way of the rows that are about something else.
var databaseCatalogNow = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func loadRepositoryDatabaseCatalog(t *testing.T) databaseCatalog {
	t.Helper()
	catalog, err := loadDatabaseCatalog(repositoryFile(t, databaseCatalogPath), databaseCatalogNow)
	if err != nil {
		t.Fatalf("load the database catalog: %v", err)
	}
	return catalog
}

func writeDatabaseCatalog(t *testing.T, old, replacement string) string {
	t.Helper()
	raw, err := os.ReadFile(repositoryFile(t, databaseCatalogPath))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), old) != 1 {
		t.Fatalf("the catalog holds %q %d times, want once", old, strings.Count(string(raw), old))
	}
	path := filepath.Join(t.TempDir(), "databases.json")
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), old, replacement, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheDatabaseCatalogDescribesTheDriverAndThePage(t *testing.T) {
	t.Parallel()
	catalog := loadRepositoryDatabaseCatalog(t)
	suites, err := loadE2ESuites(repositoryFile(t, e2eSuitesPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyDatabaseCatalog(catalog, suites, repositoryFile(t, e2eHarnessPath), repositoryFile(t, databasePagePath)); err != nil {
		t.Fatalf("verifyDatabaseCatalog() error = %v", err)
	}
	if len(catalog.Baseline) != 2 {
		t.Fatalf("the catalog names %d baseline lines; the driver defaults PostgreSQL and MySQL", len(catalog.Baseline))
	}
}

// Every refusal the catalog can give, each named by what it says. A catalog that
// was merely unreadable would pass a row that only asks for an error.
func TestADatabaseCatalogThatClaimsTooMuchIsRefused(t *testing.T) {
	t.Parallel()
	const (
		image   = `"image": "postgres:18-alpine@sha256:898c60cb390d1085bc97bdd6a1ae968757421df394b071848b52b1c09c89d6a5"`
		state   = `"state": "pending"`
		created = `"imageCreated": "2026-10-21"`
	)
	digest := "@sha256:" + strings.Repeat("a", 64)
	tests := map[string]struct {
		old, replacement, want string
	}{
		"schema version from the future": {`"schemaVersion": 1`, `"schemaVersion": 2`, "reads 1"},
		"an unknown field":               {state, state + `, "supportedBy": "me"`, "supportedBy"},
		"an added image with no digest":  {image, `"image": "postgres:18-alpine"`, "pinned by digest"},
		"a baseline image with no digest": {
			`"image": "mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb"`,
			`"image": "mysql:8.4"`, "pinned by digest",
		},
		"a tag that names no major":       {image, `"image": "postgres:alpine` + digest + `"`, "major version"},
		"a tag that names another line":   {image, `"image": "postgres:19-alpine` + digest + `"`, "names 19.x"},
		"an engine the page cannot name":  {`"engine": "mysql"`, `"engine": "mariadb"`, "not one the support page names"},
		"a line its image does not name":  {`"line": "18.x"`, `"line": "8.4.x LTS"`, "names 18.x"},
		"no created date":                 {created, `"imageCreated": ""`, "not a date"},
		"a state that is neither":         {state, `"state": "green"`, "neither pending nor supported"},
		"pending with a run":              {state, state + `, "evidenceRun": "123"`, "is pending and names run"},
		"supported with no run":           {state, `"state": "supported"`, "names no run"},
		"supported with a word for a run": {state, `"state": "supported", "evidenceRun": "green"`, "names no run"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := loadDatabaseCatalog(writeDatabaseCatalog(t, test.old, test.replacement), databaseCatalogNow)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadDatabaseCatalog() error = %v, want %q", err, test.want)
			}
		})
	}
	t.Run("only PostgreSQL can be added", func(t *testing.T) {
		t.Parallel()
		path := writeDatabaseCatalog(t, `"line": "18.x"`, `"line": "9.7.x"`)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		mutated := strings.Replace(string(raw), `"added": [
    {
      "engine": "postgresql"`, `"added": [
    {
      "engine": "mysql"`, 1)
		if mutated == string(raw) {
			t.Fatal("the added line's engine was not found")
		}
		if err := os.WriteFile(path, []byte(mutated), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = loadDatabaseCatalog(path, databaseCatalogNow)
		if err == nil || !strings.Contains(err.Error(), "cannot be added yet") {
			t.Fatalf("loadDatabaseCatalog() error = %v, want the engine refused", err)
		}
	})
}

// The rule is about the digest's age on the day it is read, so the row has to
// sit on either side of the boundary rather than on a date that merely passes.
func TestADatabaseDigestYoungerThanTwoWeeksIsRefused(t *testing.T) {
	t.Parallel()
	path := repositoryFile(t, databaseCatalogPath)
	created := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	if _, err := loadDatabaseCatalog(path, created.Add(databaseImageMinimumAge)); err != nil {
		t.Fatalf("a digest exactly two weeks old was refused: %v", err)
	}
	_, err := loadDatabaseCatalog(path, created.Add(databaseImageMinimumAge-time.Hour))
	if err == nil || !strings.Contains(err.Error(), "less than two weeks old") {
		t.Fatalf("a digest younger than two weeks was accepted: %v", err)
	}
}

// The table is what a reader takes for the support contract, so the rows that
// would put a line there before its run are the ones this has to refuse.
func TestTheDatabasePageIsHeldToTheCatalog(t *testing.T) {
	t.Parallel()
	page, err := os.ReadFile(repositoryFile(t, databasePagePath))
	if err != nil {
		t.Fatal(err)
	}
	catalog := loadRepositoryDatabaseCatalog(t)
	added := catalog.Added[0]
	baselineRow := "| MySQL | 8.4.x LTS | `" + catalog.Baseline[1].Image + "` |\n"
	addedRow := "| PostgreSQL | " + added.Line + " | `" + added.Image + "` |\n"
	write := func(t *testing.T, contents string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "databases.md")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	mutate := func(t *testing.T, old, replacement string) string {
		t.Helper()
		if !strings.Contains(string(page), old) {
			t.Fatalf("the page does not hold %q", old)
		}
		return write(t, strings.Replace(string(page), old, replacement, 1))
	}

	for name, path := range map[string]func(*testing.T) string{
		"a pending line in the table": func(t *testing.T) string {
			return mutate(t, baselineRow, baselineRow+addedRow)
		},
		"a pending line marked as pending": func(t *testing.T) string {
			return mutate(t, baselineRow, baselineRow+"| PostgreSQL | 18.x (pending) | `"+added.Image+"` |\n")
		},
		"a baseline row removed": func(t *testing.T) string { return mutate(t, baselineRow, "") },
		"a baseline digest changed": func(t *testing.T) string {
			return mutate(t, catalog.Baseline[1].Image, "mysql:8.4@sha256:"+strings.Repeat("a", 64))
		},
		"a row that names another release": func(t *testing.T) string {
			return mutate(t, "| MySQL | 8.4.x LTS |", "| MySQL | 8.x |")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := verifyDatabasePage(catalog, path(t))
			if err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("verifyDatabasePage() error = %v, want the table refused", err)
			}
		})
	}

	supported := loadRepositoryDatabaseCatalog(t)
	supported.Added[0].State = databaseLineSupported
	supported.Added[0].EvidenceRun = "123"
	t.Run("a supported line with no row", func(t *testing.T) {
		t.Parallel()
		if err := verifyDatabasePage(supported, write(t, string(page))); err == nil {
			t.Fatal("a supported line missing from the table was accepted")
		}
	})
	t.Run("a supported line with its row", func(t *testing.T) {
		t.Parallel()
		if err := verifyDatabasePage(supported, mutate(t, baselineRow, baselineRow+addedRow)); err != nil {
			t.Fatalf("a supported line in the table was refused: %v", err)
		}
	})
}

func TestTheDatabaseBaselineIsWhatTheDriverDefaultsTo(t *testing.T) {
	t.Parallel()
	catalog := loadRepositoryDatabaseCatalog(t)
	driver, err := os.ReadFile(repositoryFile(t, e2eHarnessPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyDatabaseBaselineDefaults(catalog, e2eHarnessPath, driver); err != nil {
		t.Fatalf("verifyDatabaseBaselineDefaults() error = %v", err)
	}
	for _, line := range catalog.Baseline {
		moved := strings.Replace(string(driver), line.Image, line.Image[:len(line.Image)-1]+"0", 1)
		if moved == string(driver) {
			t.Fatalf("the driver does not hold %s", line.Image)
		}
		if err := verifyDatabaseBaselineDefaults(catalog, e2eHarnessPath, []byte(moved)); err == nil {
			t.Fatalf("a driver that moved off %s was accepted", line.Image)
		}
	}
}

// An exclusion saves a job per minor, so it is exactly the edit that could quietly
// stop proving a line. The driver's own engine bindings decide which can stand.
func TestOnlyASuiteOnAnotherEngineCanBeLeftOut(t *testing.T) {
	t.Parallel()
	suites, err := loadE2ESuites(repositoryFile(t, e2eSuitesPath))
	if err != nil {
		t.Fatal(err)
	}
	driver, err := os.ReadFile(repositoryFile(t, e2eHarnessPath))
	if err != nil {
		t.Fatal(err)
	}
	const reason = "it proves nothing here"
	tests := map[string]struct {
		leftOut []databaseLeftOutSuite
		want    string
	}{
		"the suite on the line's own engine": {
			[]databaseLeftOutSuite{{"migrations-postgresql", reason}}, "can run on postgresql",
		},
		"a suite on both engines": {
			[]databaseLeftOutSuite{{"migration-authority", reason}}, "can run on postgresql",
		},
		"a suite that names no engine": {
			[]databaseLeftOutSuite{{"data-plane", reason}}, "can run on postgresql",
		},
		"a suite the catalog does not have": {
			[]databaseLeftOutSuite{{"nonexistent", reason}}, "not a suite",
		},
		"the same suite twice": {
			[]databaseLeftOutSuite{{"migrations-mysql", reason}, {"migrations-mysql", reason}}, "twice",
		},
		"no reason": {
			[]databaseLeftOutSuite{{"migrations-mysql", "  "}}, "does not say why",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			catalog := loadRepositoryDatabaseCatalog(t)
			catalog.Added[0].LeftOut = test.leftOut
			err := verifyDatabaseLeftOutSuites(catalog, suites, e2eHarnessPath, driver)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyDatabaseLeftOutSuites() error = %v, want %q", err, test.want)
			}
		})
	}
}

// The added lines run every supported minor against every suite but the ones the
// catalog leaves out, and no job takes a name or an artifact from the acceptance
// matrix, which is what a release reads.
func TestTheDatabaseLineMatrixIsEveryMinorAgainstEverySuiteItKeeps(t *testing.T) {
	t.Parallel()
	suites, err := loadE2ESuites(repositoryFile(t, e2eSuitesPath))
	if err != nil {
		t.Fatal(err)
	}
	catalog := loadRepositoryDatabaseCatalog(t)
	minors := []matrixEntry{
		{Minor: "1.36", MinorSlug: "1-36", KubernetesVersion: "1.36.4"},
		{Minor: "1.37", MinorSlug: "1-37", KubernetesVersion: "1.37.0"},
	}
	entries, err := databaseLineMatrix(minors, suites, catalog)
	if err != nil {
		t.Fatal(err)
	}
	line := catalog.Added[0]
	if want := len(minors) * (len(suites.Suites) - len(line.LeftOut)); len(entries) != want {
		t.Fatalf("the matrix holds %d jobs, want %d", len(entries), want)
	}
	acceptance := map[string]bool{}
	for _, entry := range e2eSuiteMatrix(minors, suites) {
		acceptance["Kubernetes "+entry.Minor+" "+entry.Suite] = true
		acceptance[entry.MinorSlug+"-"+entry.SuiteSlug] = true
		if entry.PostgresSourceImage != "" {
			t.Fatalf("an acceptance job names a PostgreSQL line: %+v", entry)
		}
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		name, artifact := "Kubernetes "+entry.Minor+" "+entry.Suite, entry.MinorSlug+"-"+entry.SuiteSlug
		if acceptance[name] || acceptance[artifact] {
			t.Fatalf("job %q takes a name or an artifact from the acceptance matrix", name)
		}
		if seen[name] || seen[artifact] {
			t.Fatalf("the matrix runs %q twice", name)
		}
		seen[name], seen[artifact] = true, true
		if entry.PostgresSourceImage != line.Image {
			t.Fatalf("job %q runs %q, want %s", name, entry.PostgresSourceImage, line.Image)
		}
		if !strings.Contains(name, "on PostgreSQL 18") {
			t.Fatalf("job %q does not say which line it runs", name)
		}
		for _, leftOut := range line.LeftOut {
			if entry.Suite == leftOut.Suite {
				t.Fatalf("the matrix runs %s, which the line leaves out", entry.Suite)
			}
		}
	}
	empty := catalog
	empty.Added = nil
	if _, err := databaseLineMatrix(minors, suites, empty); err == nil || !strings.Contains(err.Error(), "no added line") {
		t.Fatalf("databaseLineMatrix() with no line error = %v", err)
	}

	// The field stays off an ordinary job's document altogether: the plain
	// matrix's shape is what the release workflow reads.
	encoded, err := json.Marshal(e2eSuiteMatrix(minors, suites))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "postgres_source_image") {
		t.Fatalf("the acceptance matrix carries the PostgreSQL image: %s", encoded)
	}
}

func TestVerifyDatabaseLinesWorkflowRefusesWhatWouldCostAnOrdinaryPush(t *testing.T) {
	t.Parallel()
	path := repositoryFile(t, databaseLinesWorkflowPath)
	if err := verifyDatabaseLinesWorkflow(path); err != nil {
		t.Fatalf("verifyDatabaseLinesWorkflow() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(raw)
	tests := map[string]struct {
		old, replacement, want string
	}{
		"a push trigger": {
			"  workflow_dispatch:\n", "  workflow_dispatch:\n  push:\n    branches: [master]\n", "on nothing else",
		},
		"every pull request": {
			"  pull_request:\n    paths:\n      - .github/workflows/database-lines.yml\n      - support/databases.json\n",
			"  pull_request:\n", "limited to this workflow",
		},
		"a path outside the two": {
			"      - support/databases.json\n", "      - support/databases.json\n      - hack/**\n", "limited to this workflow",
		},
		"a branch filter beside the paths": {
			"    paths:\n", "    branches: [master]\n    paths:\n", "limited to this workflow",
		},
		"no schedule": {
			"  schedule:\n    - cron: \"43 3 * * 6\"\n", "", "on nothing else",
		},
		"a dispatch input that could narrow the run": {
			"  workflow_dispatch:\n", "  workflow_dispatch:\n    inputs:\n      suite:\n        type: string\n", "takes no input",
		},
		"a wider token": {"  actions: read\n", "  actions: write\n", "contents and actions, read"},
		"the group CI's own runs use": {
			"database-lines-${{ github.ref }}", "ci-Added database lines-${{ github.ref }}", "group of its own",
		},
		"a run that lets the older one finish": {"cancel-in-progress: true", "cancel-in-progress: false", "group of its own"},
		"a copy of the jobs":                   {"uses: ./.github/workflows/ci.yml", "uses: ./.github/workflows/other.yml", "must run ci.yml"},
		"ci.yml from another commit": {
			"uses: ./.github/workflows/ci.yml", "uses: stokaro/ptah-operator/.github/workflows/ci.yml@master", "must run ci.yml",
		},
		"the ordinary matrix":  {"database_lines: true", "database_lines: false", "database_lines: true"},
		"every secret":         {"    with:\n", "    secrets: inherit\n    with:\n", "one job"},
		"a job beside the run": {"jobs:\n", "jobs:\n  extra:\n    runs-on: ubuntu-latest\n    steps: []\n", "one job"},
		"a workflow-level key": {"permissions:\n", "env:\n  E2E_SUITE: data-plane\npermissions:\n", "outside the audited shape"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := verifyDatabaseLinesWorkflow(writeMutatedWorkflow(t, workflow, test.old, test.replacement))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyDatabaseLinesWorkflow() error = %v, want %q", err, test.want)
			}
		})
	}
}

// The call has to leave an ordinary run exactly as it was, so these are the edits
// that would reach the acceptance matrix or let a caller steer more than the one
// choice it has.
func TestVerifyWorkflowHoldsTheDatabaseLinesCall(t *testing.T) {
	t.Parallel()
	path := repositoryFile(t, workflowPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(raw)
	const input = "      database_lines:\n        description: Run the added database lines instead of the acceptance matrix\n" +
		"        type: boolean\n        required: false\n        default: false\n"
	tests := map[string]struct {
		old, replacement, want string
	}{
		"no way to call it": {
			"  workflow_call:\n    inputs:\n" + input, "", "must accept workflow_call",
		},
		"selected by default": {
			"        type: boolean\n        required: false\n        default: false\n",
			"        type: boolean\n        required: false\n        default: true\n", "defaults to false",
		},
		"a required input": {
			"        type: boolean\n        required: false\n", "        type: boolean\n        required: true\n", "defaults to false",
		},
		"another input": {
			input, input + "      suite:\n        type: string\n", "one optional boolean",
		},
		"a free-form input": {
			"        type: boolean\n", "        type: string\n", "one optional boolean",
		},
		"the added lines on every run": {
			"ACCEPTANCE_OUTPUT: ${{ inputs.database_lines && 'database-lines' || 'acceptance' }}",
			"ACCEPTANCE_OUTPUT: database-lines", "database_lines input",
		},
		"the output named outside the input": {
			"ACCEPTANCE_OUTPUT: ${{ inputs.database_lines && 'database-lines' || 'acceptance' }}",
			"ACCEPTANCE_OUTPUT: ${{ github.event.inputs.output }}", "database_lines input",
		},
		"the image the driver defaults to, on every job": {
			"E2E_POSTGRES_SOURCE_IMAGE: ${{ matrix.postgres_source_image }}",
			"E2E_POSTGRES_SOURCE_IMAGE: postgres:18-alpine", "lifecycle environment bindings",
		},
		"no image handed to the driver": {
			"          # Empty on the acceptance matrix, where the driver uses its own default.\n" +
				"          E2E_POSTGRES_SOURCE_IMAGE: ${{ matrix.postgres_source_image }}\n",
			"", "lifecycle environment bindings",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := verifyWorkflow(writeMutatedWorkflow(t, workflow, test.old, test.replacement))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyWorkflow() error = %v, want %q", err, test.want)
			}
		})
	}
}

// The driver and the phases read the same two facts in two languages. Running the
// driver's own functions over the table the Go ones answer keeps them one rule.
func TestTheDriverAndThePhasesReadThePostgresImageAlike(t *testing.T) {
	t.Parallel()
	driver, err := os.ReadFile(repositoryFile(t, e2eHarnessPath))
	if err != nil {
		t.Fatal(err)
	}
	functions := ""
	for _, name := range []string{"postgres_image_major", "postgres_data_directory"} {
		body := regexp.MustCompile(`(?ms)^` + name + `\(\) \{\n.*?^\}\n`).Find(driver)
		if body == nil {
			t.Fatalf("%s does not define %s", e2eHarnessPath, name)
		}
		functions += string(body)
	}
	shell := func(t *testing.T, call string, arguments ...string) string {
		t.Helper()
		command := exec.Command("sh", append([]string{"-c", functions + call, "sh"}, arguments...)...) //nolint:gosec // The repository's own driver.
		output, err := command.Output()
		if err != nil {
			t.Fatalf("sh: %v", err)
		}
		return strings.TrimSpace(string(output))
	}
	digest := "@sha256:" + strings.Repeat("a", 64)
	for _, image := range []string{
		"postgres:17-alpine" + digest,
		"postgres:18-alpine" + digest,
		"postgres:18" + digest,
		"postgres:18.6-alpine3.24" + digest,
		"registry.example:5000/mirror/postgres:18-alpine" + digest,
		"postgres" + digest,
		"registry.example:5000/postgres" + digest,
		"postgres:alpine" + digest,
		"postgres:latest" + digest,
		"postgres:alpine-18" + digest,
		"postgres:0" + digest,
		"postgres:18-alpine",
		"postgres:18-alpine@sha256:abc",
		"postgres:18-alpine@sha256:" + strings.Repeat("A", 64),
		"",
	} {
		t.Run(image, func(t *testing.T) {
			t.Parallel()
			major, err := phases.PostgresImageMajor(image)
			read := shell(t, `postgres_image_major "$1"`, image)
			if err != nil {
				if read != "" {
					t.Fatalf("the phases refuse %q and the driver reads %q", image, read)
				}
				return
			}
			if read != strconv.Itoa(major) {
				t.Fatalf("the phases read %d from %q and the driver reads %q", major, image, read)
			}
		})
	}
	for _, major := range []int{14, 16, 17, 18, 19} {
		want := phases.PostgresDataDirectory(major)
		if got := shell(t, `postgres_data_directory "$1"`, strconv.Itoa(major)); got != want {
			t.Fatalf("for %d the driver mounts %q and the phases hold %q", major, got, want)
		}
	}
}
