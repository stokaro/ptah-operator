package runnerharness_test

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/runnerharness"
)

const (
	rowFileFamily    = "boundary-row-file"
	unparsableFamily = "boundary-unparsable-oci"
	// outsideRoot is Ptah's refusal of a row file outside the working
	// directory (stokaro/ptah#5560).
	outsideRoot = "is outside allowed root"
	// unparsableWords open the reason the operation container gives for the
	// schema hack/unparsableschemafixture publishes.
	unparsableWords = "error parsing schema file: parse HCL schema: /source/schema.hcl:"
)

// sourceBoundaries runs the desired sources whose reading has to stop where the
// operation container is: a row file that names a file outside the schema,
// and an OCI schema the fetch leaves unparsed.
func sourceBoundaries(t *testing.T, l *lab) {
	t.Helper()
	// The operation container mounts no credential: the database URL is in
	// its environment, and /proc/self/environ is where a file read reaches
	// it. The row file names that path from the schema's directory, and Ptah
	// refuses it as outside the working directory before it opens it.
	t.Run("row-file", func(t *testing.T) {
		t.Parallel()
		database := "harness_boundary_row_file"
		l.createDatabase(t, enginePostgres, database)
		volume := l.name(rowFileFamily)
		l.fillSource(t, volume, map[string][]byte{"schema/schema.hcl": harnessFixture(t, "boundaries/row-file.hcl")})
		run := l.newSchema(t, rowFileFamily, enginePostgres, l.databaseURL(enginePostgres, database),
			operatorv1alpha1.SourceSpec{Git: l.gitHTTPS("db/schema.hcl", operatorv1alpha1.GitRef{Branch: "main"})})
		commit, content := fetchShapeRevision(t)
		git := run.schema.Spec.Desired.Git
		run.schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{Git: &operatorv1alpha1.GitSourceRevision{
			URL: git.URL, Ref: git.Ref, Path: git.Path, Commit: commit, ContentDigest: content,
		}}
		shared := func(spec *rowSpec) { spec.sharedSource = volume }
		outcomes := map[string]rowOutcome{
			"Observe": run.observe("observe", shared, wantCode("child_exit"), storedAs("schema drift", "boundaries/drift-row-file.json")),
			"Plan":    run.plan("plan", nil, shared, wantCode("invalid_plan_output"), storedAs("schema plan", "boundaries/plan-row-file.json")),
		}
		for _, stage := range slices.Sorted(maps.Keys(outcomes)) {
			outcome := outcomes[stage]
			if leaked := leaks(outcome, l.databasePassword); leaked != "" {
				t.Errorf("%s: %s holds the database password", stage, leaked)
			}
			refused := false
			for _, child := range outcome.children {
				refused = refused || bytes.Contains(child.Stdout, []byte(outsideRoot))
			}
			if !refused {
				t.Errorf("%s: no Ptah command says the row file %s", stage, outsideRoot)
			}
		}
	})
	// The fetch writes the layer it checked and leaves it unparsed, so the
	// container that holds no registry credential names the fault.
	t.Run("unparsable-oci", func(t *testing.T) {
		t.Parallel()
		database := "harness_boundary_unparsable"
		l.createDatabase(t, enginePostgres, database)
		repository := "schemas/" + unparsableFamily
		l.docker.must(t, nil, "run", "--rm", "--pull", "never", "--name", l.name("unparsable-publish"), "--network", l.network,
			"--entrypoint", "/unparsable-schema", l.images.tools,
			"-reference", registryHost+"/"+repository+":v1", "-plain-http")
		run := l.newSchema(t, unparsableFamily, enginePostgres, l.databaseURL(enginePostgres, database), ociSource("oci://"+registryHost+"/"+repository+":v1"))
		run.resolve("resolve")
		run.schema.Spec.Desired.OCIRef = run.schema.Status.Source.ResolvedReference
		run.schema.Status.Source.RequestedReference = run.schema.Status.Source.ResolvedReference
		run.verify("verify", fixture(t, "verification-policy-digest-pin.yaml"))
		for stage, outcome := range map[string]rowOutcome{
			"Observe": run.observe("observe", wantCode(runner.CodeDesiredSourceUnreadable)),
			"Plan":    run.plan("plan", nil, wantCode(runner.CodeDesiredSourceUnreadable)),
		} {
			if message := outcome.result.Error.Message; !strings.HasPrefix(message, unparsableWords) {
				t.Errorf("%s: the frame says %q, want it to start %q", stage, message, unparsableWords)
			}
		}
	})
}

// leaks names what a row printed that holds the secret, or "".
func leaks(outcome rowOutcome, secret string) string {
	if bytes.Contains(outcome.logs, []byte(secret)) {
		return "the Pod log"
	}
	if outcome.result.Error != nil && strings.Contains(outcome.result.Error.Message, secret) {
		return "the frame"
	}
	for _, child := range outcome.children {
		if bytes.Contains(child.Stdout, []byte(secret)) || bytes.Contains(child.Stderr, []byte(secret)) {
			return "`ptah " + runnerharness.CommandWords(child.Args) + "`"
		}
	}
	return ""
}

// checkBoundaries says what the committed captures miss of the boundary rows.
func checkBoundaries(captures runnerharness.Captures) error {
	var problems []error
	for _, step := range []struct {
		row, command, code string
		decode             func([]byte) (string, error)
	}{
		{rowFileFamily + "/observe", "schema drift", "child_exit", func(document []byte) (string, error) {
			failure, err := dataplane.DecodeDriftFailure(document)
			return failure.Message, err
		}},
		{rowFileFamily + "/plan", "schema plan", "invalid_plan_output", func(document []byte) (string, error) {
			report, err := dataplane.DecodeSchemaPlan(document)
			if err != nil || report.Error == nil {
				return "", errors.Join(err, errors.New("the plan reports no error"))
			}
			return report.Error.Message, nil
		}},
	} {
		record, err := capturedRow(captures, step.row)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if code := frameCode(record); code != step.code {
			problems = append(problems, fmt.Errorf("%s: the frame says %q, want %s", step.row, code, step.code))
		}
		child, err := capturedChild(captures, step.row, step.command)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		message, err := step.decode(captures.Documents[child.Stdout])
		switch {
		case err != nil:
			problems = append(problems, fmt.Errorf("%s: %w", step.row, err))
		case !strings.Contains(message, outsideRoot) || !strings.Contains(message, `"../../proc/self/environ"`):
			problems = append(problems, fmt.Errorf("%s: `ptah %s` says %q, want the row file refused as outside the root", step.row, step.command, message))
		}
	}
	for _, step := range []string{"observe", "plan"} {
		name := unparsableFamily + "/" + step
		record, err := capturedRow(captures, name)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if code := frameCode(record); code != runner.CodeDesiredSourceUnreadable {
			problems = append(problems, fmt.Errorf("%s: the frame says %q, want %s", name, code, runner.CodeDesiredSourceUnreadable))
		}
		frame, _ := record.Result.(map[string]any)
		failure, _ := frame["error"].(map[string]any)
		if message, _ := failure["message"].(string); !strings.HasPrefix(message, unparsableWords) {
			problems = append(problems, fmt.Errorf("%s: the frame says %q, want it to start %q", name, message, unparsableWords))
		}
		pull, err := capturedChild(captures, name, "schema pull")
		switch {
		case err != nil:
			problems = append(problems, err)
		case pull.Exit != 0 || !slices.Contains(pull.Args, "--skip-parse"):
			problems = append(problems, fmt.Errorf("%s: the fetch ran %q and exited %d, want it to leave the schema unparsed and pass", name, pull.Args, pull.Exit))
		}
	}
	return errors.Join(problems...)
}

// The boundary rows' committed captures hold what the rows are for, so a repin
// that moves them asks for a decision in `go test`.
func TestSourceBoundaryRowsReachTheirShape(t *testing.T) {
	t.Parallel()
	_, captures := committedCaptures(t)
	if err := checkBoundaries(captures); err != nil {
		t.Error(err)
	}
}

// The check refuses a capture that misses the boundary, each built from a
// committed capture moved where it does not belong.
func TestBoundaryCheckRefusesACaptureThatCrossedIt(t *testing.T) {
	t.Parallel()
	_, committed := committedCaptures(t)
	edit := func(captures *runnerharness.Captures, name string, change func(*runnerharness.Row)) {
		t.Helper()
		index := slices.IndexFunc(captures.Rows, func(row runnerharness.Row) bool { return row.Name == name })
		if index < 0 {
			t.Fatalf("no row %s", name)
		}
		captures.Rows[index].Children = slices.Clone(captures.Rows[index].Children)
		change(&captures.Rows[index])
	}
	for _, mistake := range []struct {
		name, want string
		change     func(*runnerharness.Captures)
	}{
		{
			name: "a drift that read the row file", want: "want the row file refused",
			change: func(captures *runnerharness.Captures) {
				other, err := capturedChild(*captures, "structural-index-table/observe", "schema drift")
				if err != nil {
					t.Fatal(err)
				}
				mine, err := capturedChild(*captures, rowFileFamily+"/observe", "schema drift")
				if err != nil {
					t.Fatal(err)
				}
				captures.Documents[mine.Stdout] = captures.Documents[other.Stdout]
			},
		},
		{
			name: "a plan that went ahead", want: `want invalid_plan_output`,
			change: func(captures *runnerharness.Captures) {
				edit(captures, rowFileFamily+"/plan", func(row *runnerharness.Row) {
					row.Result = map[string]any{"planOutcome": string(runner.PlanOutcomeChanges)}
				})
			},
		},
		{
			name: "a fetch that parsed the schema", want: "want it to leave the schema unparsed",
			change: func(captures *runnerharness.Captures) {
				edit(captures, unparsableFamily+"/observe", func(row *runnerharness.Row) {
					row.Children[0].Args = slices.DeleteFunc(slices.Clone(row.Children[0].Args), func(arg string) bool { return arg == "--skip-parse" })
				})
			},
		},
		{
			name: "a fault named by the step alone", want: "want it to start",
			change: func(captures *runnerharness.Captures) {
				edit(captures, unparsableFamily+"/plan", func(row *runnerharness.Row) {
					row.Result = map[string]any{"error": map[string]any{
						"code": runner.CodeDesiredSourceUnreadable, "message": "the fetch-schema step could not read the desired source",
					}}
				})
			},
		},
	} {
		t.Run(mistake.name, func(t *testing.T) {
			t.Parallel()
			captures := runnerharness.Captures{Rows: slices.Clone(committed.Rows), Documents: maps.Clone(committed.Documents)}
			mistake.change(&captures)
			if err := checkBoundaries(captures); err == nil || !strings.Contains(err.Error(), mistake.want) {
				t.Fatalf("checkBoundaries() = %v, want a refusal holding %q", err, mistake.want)
			}
		})
	}
}
