package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

const (
	// testShadowURL is a shadow database on its own server, and its password
	// is what no frame may carry.
	testShadowURL      = "postgres://shadow:shadow-secret@shadow.example/app_shadow"
	testShadowPassword = "shadow-secret"
	// driftMessage stands for what Ptah says about the shadow or a finding,
	// which can name a host, a user or an object, and stays out of the frame.
	driftMessage = "CHECK-TEXT shadow.example refused user shadow"
)

func shadowDigest(t *testing.T, url string) string {
	t.Helper()
	digest, err := TargetIdentityDigest(url)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// driftApplyEnvironment is a migration Apply whose plan bound the shadow at
// expected, with the Secret's URL resolved into the environment as the
// runner reads it.
func driftApplyEnvironment(t *testing.T, operationID, shadowURL, expected string) []string {
	t.Helper()
	environment := migrationApplyEnvironment(t, operationID)
	if shadowURL != "" {
		environment = append(environment, EnvShadowDatabaseURL+"="+shadowURL)
	}
	if expected != "" {
		environment = append(environment, envExpectedShadowDigest+"="+expected)
	}
	return withRunnerProtocol(environment)
}

// refusedRunDocument is a run the drift check stopped, as Ptah v0.28.0 writes
// one: failed, the selection planned, nothing applied, the history read after.
func refusedRunDocument(check string) string {
	return `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"failed","planned":[3,4],` +
		`"error":{"message":"` + driftMessage + `"},"drift_check":` + check + `,` +
		`"status":{"contract_version":2,"current_version":2,"total_migrations":4,"has_pending_changes":true}}`
}

const (
	driftedCheck = `{"outcome":"drifted","replayed":[1,2],"replayed_keys":["1","2"],"findings":[` +
		`{"kind":"extra_column","table":"widgets","column":"note","message":"` + driftMessage + `"},` +
		`{"kind":"extra_table","object":"audit_trail","message":"` + driftMessage + `"}]}`
	unableShadowCheck = `{"outcome":"unable","stage":"claim","reason":"not_clean","message":"` + driftMessage + `"}`
	cleanCheck        = `{"outcome":"clean","replayed":[1,2],"replayed_keys":["1","2"]}`
)

func appliedCheckedDocument(check string) string {
	document := `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"applied","planned":[3,4],"applied":[3,4],`
	if check != "" {
		document += `"drift_check":` + check + `,`
	}
	return document + `"status":{"contract_version":2,"current_version":4,"total_migrations":4,"has_pending_changes":false}}`
}

// An Apply whose plan bound a shadow runs `migrations up --check-drift`, hands
// the shadow's URL to that child alone in its environment, and frames what the
// check concluded as outcome, codes and a count: Ptah's sentences, the objects
// it names and the shadow's password stay out of the frame.
func TestAMigrationApplyBoundToAShadowRunsTheDriftCheck(t *testing.T) {
	t.Parallel()
	expected := shadowDigest(t, testShadowURL)
	for _, row := range []struct {
		name     string
		document string
		exitCode int
		want     dataplane.MigrationDriftSummary
	}{
		{name: "drifted", document: refusedRunDocument(driftedCheck), exitCode: 1,
			want: dataplane.MigrationDriftSummary{Outcome: "drifted", Findings: 2}},
		{name: "unable", document: refusedRunDocument(unableShadowCheck), exitCode: 2,
			want: dataplane.MigrationDriftSummary{Outcome: "unable", Stage: "claim", Reason: "not_clean"}},
		{name: "clean", document: appliedCheckedDocument(cleanCheck),
			want: dataplane.MigrationDriftSummary{Outcome: "clean"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: row.document, exitCode: row.exitCode}}}
			operationID := "drift-apply-" + row.name
			result := Run(context.Background(), Config{
				Operation:   OperationMigrationApply,
				Environment: driftApplyEnvironment(t, operationID, testShadowURL, expected),
				Executor:    executor,
			})
			if len(executor.calls) != 1 {
				t.Fatalf("the child ran %d times, want once", len(executor.calls))
			}
			call := executor.calls[0]
			if !slices.Contains(call.Args, "--check-drift") {
				t.Fatalf("the Apply was not asked for the check: %q", call.Args)
			}
			if !slices.Contains(call.Env, EnvShadowDatabaseURL+"="+testShadowURL) {
				t.Fatal("the child was not given the shadow database")
			}
			for _, argument := range call.Args {
				if strings.Contains(argument, testShadowPassword) || strings.Contains(argument, "shadow.example") {
					t.Fatalf("the shadow database reached the command line: %q", call.Args)
				}
			}
			if result.ShadowIdentityDigest != expected {
				t.Fatalf("ShadowIdentityDigest = %q, want %q", result.ShadowIdentityDigest, expected)
			}
			if result.MigrationRun == nil || result.MigrationRun.DriftCheck != nil ||
				result.MigrationRun.DriftSummary == nil || *result.MigrationRun.DriftSummary != row.want {
				t.Fatalf("the run frames drift check %#v, want summary %+v and none of Ptah's text", result.MigrationRun, row.want)
			}
			if result.Uncertain {
				t.Fatal("a run the database accounted for is framed as uncertain")
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			for _, leaked := range []string{"CHECK-TEXT", testShadowPassword, "shadow.example", "audit_trail", "widgets"} {
				if bytes.Contains(frame, []byte(leaked)) {
					t.Fatalf("the frame carries %q: %s", leaked, frame)
				}
			}
			parsed, err := ParseResultFor(frame, OperationMigrationApply, operationID)
			if err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
			if parsed.MigrationRun == nil || parsed.MigrationRun.DriftSummary == nil || *parsed.MigrationRun.DriftSummary != row.want {
				t.Fatalf("the manager parsed %#v, want summary %+v", parsed.MigrationRun, row.want)
			}
		})
	}
}

// An Apply whose shadow is not the one its plan bound is refused before its
// child, the way one on a moved target is: a shadow the plan did not name, a
// plan shadow the Job does not hold, a shadow that is the target under another
// password, and a URL the runner cannot derive an identity from. None of the
// refusals names the URL.
func TestAMigrationApplyRefusesAShadowItsPlanDidNotBindBeforeItsChild(t *testing.T) {
	t.Parallel()
	expected := shadowDigest(t, testShadowURL)
	const otherShadow = "postgres://shadow:shadow-secret@elsewhere.example/app_shadow"
	for _, row := range []struct {
		name, url, expected, code string
	}{
		{name: "another shadow", url: otherShadow, expected: expected, code: "shadow_binding_mismatch"},
		{name: "a shadow the plan does not bind", url: testShadowURL, code: "shadow_binding_mismatch"},
		{name: "no shadow for a plan that binds one", expected: expected, code: "missing_shadow"},
		{name: "a binding that is not a digest", url: testShadowURL, expected: "sha256:short", code: "missing_shadow_binding"},
		{name: "the target under another password", url: "postgres://app:shadow-secret@db.example/app", expected: expected, code: "shadow_is_target"},
		{name: "a URL with no identity", url: "docker://postgres/18?password=shadow-secret", expected: expected, code: "invalid_shadow"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t}
			operationID := "drift-refused-" + strings.ReplaceAll(row.name, " ", "-")
			result := Run(context.Background(), Config{
				Operation:   OperationMigrationApply,
				Environment: driftApplyEnvironment(t, operationID, row.url, row.expected),
				Executor:    executor,
			})
			if result.Error == nil || result.Error.Code != row.code {
				t.Fatalf("error = %#v, want %s", result.Error, row.code)
			}
			if len(executor.calls) != 0 || result.ChildExitCode != -1 || result.MutationStarted || result.Uncertain {
				t.Fatalf("the child ran or the refusal claims a mutation: calls=%d %#v", len(executor.calls), result)
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			if bytes.Contains(frame, []byte(testShadowPassword)) || bytes.Contains(frame, []byte("example")) {
				t.Fatalf("the refusal carries the shadow URL: %s", frame)
			}
			if _, err := ParseResultFor(frame, OperationMigrationApply, operationID); err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
		})
	}
}

// The history read reports the shadow's identity for the plan to bind, and its
// child, `migrations status`, is given neither the shadow nor the check.
func TestAMigrationHistoryReportsTheShadowIdentityAndHidesTheShadow(t *testing.T) {
	t.Parallel()
	document := `{"contract_version":2,"command":"migrations status","current_version":2,"total_migrations":4,"has_pending_changes":true,"pending_migrations":[3,4]}`
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: document}}}
	result := Run(context.Background(), Config{
		Operation:   OperationMigrationHistory,
		Environment: withRunnerProtocol(append(migrationEnvironment("drift-history"), EnvShadowDatabaseURL+"="+testShadowURL)),
		Executor:    executor,
	})
	if result.Error != nil || result.MigrationHistory == nil {
		t.Fatalf("Run() = %#v, want the history", result)
	}
	if want := shadowDigest(t, testShadowURL); result.ShadowIdentityDigest != want {
		t.Fatalf("ShadowIdentityDigest = %q, want %q", result.ShadowIdentityDigest, want)
	}
	call := executor.calls[0]
	if slices.Contains(call.Args, "--check-drift") {
		t.Fatalf("the history read was asked for a drift check: %q", call.Args)
	}
	for _, variable := range call.Env {
		if strings.HasPrefix(variable, EnvShadowDatabaseURL+"=") || strings.Contains(variable, testShadowPassword) {
			t.Fatalf("the history read's child holds the shadow database: %q", variable)
		}
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	if _, err := ParseResultFor(frame, OperationMigrationHistory, "drift-history"); err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}
}

// A shadow reaches no operation but the two that bind it.
func TestOnlyTheMigrationOperationsReadAShadow(t *testing.T) {
	t.Parallel()
	executor := &scriptedExecutor{t: t}
	result := Run(context.Background(), Config{
		Operation:   OperationObserve,
		Environment: withRunnerProtocol(append(databaseEnvironment("drift-observe"), EnvShadowDatabaseURL+"="+testShadowURL)),
		Executor:    executor,
	})
	if result.Error == nil || result.Error.Code != "invalid_configuration" || len(executor.calls) != 0 {
		t.Fatalf("an Observe holding a shadow ran: calls=%d error=%#v", len(executor.calls), result.Error)
	}
}

// What a run says about the check has to answer what the runner asked. A run
// asked for a check that reports none and ran migrations, or one asked for
// none that reports one, is no account of whether the check stood in front of
// the migrations, so it reaches the frame as no report and the mutation stays
// uncertain. A run asked for the check that stopped before it, as the
// expected-sequence guard does, ran nothing and is read as the failure it is.
func TestADriftCheckAnswersWhatTheRunnerAsked(t *testing.T) {
	t.Parallel()
	expected := shadowDigest(t, testShadowURL)
	sequenceRefusal := `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"failed","planned":[3,4,5],` +
		`"error":{"message":"selected sequence differs"},` +
		`"status":{"contract_version":2,"current_version":2,"total_migrations":5,"has_pending_changes":true}}`
	for _, row := range []struct {
		name        string
		document    string
		exitCode    int
		shadow      bool
		wantRefused bool
	}{
		{name: "asked, none reported, migrations ran", document: appliedCheckedDocument(""), shadow: true, wantRefused: true},
		{name: "not asked, one reported", document: appliedCheckedDocument(cleanCheck), wantRefused: true},
		{name: "asked, stopped by the sequence guard before it", document: sequenceRefusal, exitCode: 1, shadow: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			url, digest := "", ""
			if row.shadow {
				url, digest = testShadowURL, expected
			}
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: row.document, exitCode: row.exitCode}}}
			result := Run(context.Background(), Config{
				Operation:   OperationMigrationApply,
				Environment: driftApplyEnvironment(t, "drift-answer", url, digest),
				Executor:    executor,
			})
			if row.wantRefused {
				if result.Error == nil || result.Error.Code != "invalid_migration_run_output" || result.MigrationRun != nil ||
					!result.MutationStarted || !result.Uncertain {
					t.Fatalf("Run() = %#v, want no report and an uncertain mutation", result)
				}
				return
			}
			if result.MigrationRun == nil || result.MigrationRun.DriftSummary != nil || result.Uncertain {
				t.Fatalf("Run() = %#v, want the failed run with no drift summary", result)
			}
		})
	}
}

// The frame is held to the same account: Ptah's own drift check never crosses
// it, a summary comes only from an Apply that held a shadow, a refusing one
// only beside a run that ran nothing, and a shadow identity only from the two
// operations that read one.
func TestAFrameHoldsItsDriftCheckToTheRunAroundIt(t *testing.T) {
	t.Parallel()
	shadow := "sha256:" + strings.Repeat("5", 64)
	base := func() Result {
		result := Result{
			ProtocolVersion: ProtocolVersion, Operation: OperationMigrationApply, OperationID: "drift-frame",
			ChildExitCode: 1, CoordinationDigest: testCoordinationDigest(), TargetIdentityDigest: "sha256:" + strings.Repeat("6", 64),
			ShadowIdentityDigest: shadow, MutationStarted: true,
			Error: &ResultError{Code: "child_exit", Message: "exit status 1"},
			MigrationRun: &dataplane.MigrationRunReport{
				ContractVersion: 2, Direction: "up", Outcome: dataplane.MigrationOutcomeFailed, Planned: []int64{3},
				DriftSummary: &dataplane.MigrationDriftSummary{Outcome: dataplane.MigrationDriftDrifted, Findings: 1},
			},
		}
		return result
	}
	if _, err := ParseResultFor(mustFrame(t, base()), OperationMigrationApply, "drift-frame"); err != nil {
		t.Fatalf("the frame of a drifted refusal was refused: %v", err)
	}
	for _, row := range []struct {
		name string
		edit func(*Result)
	}{
		{name: "Ptah's drift check", edit: func(r *Result) {
			r.MigrationRun.DriftCheck = &dataplane.MigrationDriftCheck{Outcome: "drifted", Message: driftMessage}
		}},
		{name: "a summary with no shadow", edit: func(r *Result) { r.ShadowIdentityDigest = "" }},
		{name: "a refusal beside applied migrations", edit: func(r *Result) {
			r.MigrationRun.Outcome, r.MigrationRun.Applied = dataplane.MigrationOutcomePartial, []int64{3}
		}},
		{name: "a drifted summary with no finding", edit: func(r *Result) { r.MigrationRun.DriftSummary.Findings = 0 }},
		{name: "a shadow identity that is not a digest", edit: func(r *Result) { r.ShadowIdentityDigest = "sha256:short" }},
		{name: "a shadow identity on a Resolve", edit: func(r *Result) {
			r.Operation, r.MigrationRun, r.MutationStarted, r.CoordinationDigest, r.TargetIdentityDigest = OperationResolve, nil, false, "", ""
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			result := base()
			row.edit(&result)
			// MarshalFrame refuses each of these too, so the frame is built
			// the way a runner that wrote one would build it.
			if _, err := MarshalFrame(result); err == nil {
				t.Fatalf("MarshalFrame() framed a result with %s", row.name)
			}
			payload, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseResult(foreignFrame(payload)); err == nil {
				t.Fatalf("a frame with %s was accepted", row.name)
			}
		})
	}
}

func mustFrame(t *testing.T, result Result) []byte {
	t.Helper()
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	return frame
}

// The check is asked for by the argument alone, on the Apply alone, and only
// when the plan bound a shadow.
func TestOnlyAnApplyWithAShadowBindingIsAskedForTheCheck(t *testing.T) {
	t.Parallel()
	inputs := Inputs{MigrationsDir: "/source/migrations", ExpectedSequencePath: "/work/sequence.json"}
	for _, row := range []struct {
		operation Operation
		shadow    string
		want      bool
	}{
		{operation: OperationMigrationApply, shadow: "sha256:" + strings.Repeat("5", 64), want: true},
		{operation: OperationMigrationApply},
		{operation: OperationMigrationHistory, shadow: "sha256:" + strings.Repeat("5", 64)},
	} {
		in := inputs
		in.ExpectedShadowDigest = row.shadow
		spec, err := BuildCommand("/ptah", row.operation, in)
		if err != nil {
			t.Fatalf("BuildCommand(%s) error = %v", row.operation, err)
		}
		if got := slices.Contains(spec.Args, "--check-drift"); got != row.want {
			t.Errorf("BuildCommand(%s, shadow %t) asks for the check: %t, want %t", row.operation, row.shadow != "", got, row.want)
		}
	}
	child := childEnvironment([]string{
		EnvShadowDatabaseURL + "=" + testShadowURL, "PTAH_CHECK_DRIFT=true", "PTAH_DRIFT_EXCLUDE=public.audit",
		envExpectedShadowDigest + "=sha256:x", "PTAH_DB_URL=postgres://app:secret@db.example/app",
	})
	if !slices.Equal(child, []string{"PTAH_DB_URL=postgres://app:secret@db.example/app"}) {
		t.Fatalf("childEnvironment() = %q, want the shadow and the check's variables removed", child)
	}
}

// The shadow's URL and its password are redacted wherever the runner prints.
func TestTheShadowDatabaseIsRedacted(t *testing.T) {
	t.Parallel()
	redactor := NewRedactor([]string{EnvShadowDatabaseURL + "=" + testShadowURL})
	got := redactor.Redact("dial " + testShadowURL + " as shadow with " + testShadowPassword)
	if strings.Contains(got, testShadowPassword) || strings.Contains(got, testShadowURL) {
		t.Fatalf("Redact() = %q, want the shadow URL and its password redacted", got)
	}
}

// What Ptah v0.28.0 printed for a target holding a column nobody migrated, and
// for a shadow holding a table (see internal/dataplane/testdata): the first
// line of Ptah's error reaches the Pod log, where the manager's message points,
// and nothing Ptah named reaches the frame.
func TestPtahsDriftSentenceReachesThePodLogAndNotTheFrame(t *testing.T) {
	t.Parallel()
	expected := shadowDigest(t, testShadowURL)
	for _, row := range []struct {
		document, logged, named string
		exitCode                int
		want                    dataplane.MigrationDriftSummary
	}{
		{
			document: "migrations-up-drifted-postgres.json", exitCode: 1,
			logged: "ptah-runner: ptah reported: the database schema differs from the schema its 2 applied migrations produce, " +
				"in 1 place, so no pending migration was run; first: extra column e2e_migration_widgets.nickname\n",
			named: "nickname", want: dataplane.MigrationDriftSummary{Outcome: "drifted", Findings: 1},
		},
		{
			document: "migrations-up-drift-shadow-not-empty-postgres.json", exitCode: 2,
			logged: "ptah-runner: ptah reported: the drift check could not compare the database with its applied history, " +
				"so no pending migration was run: applied history shadow check failed: connected database is not clean: " +
				"found table \"left_behind\" in schema \"public\"",
			named: "left_behind", want: dataplane.MigrationDriftSummary{Outcome: "unable", Stage: "claim", Reason: "not_clean"},
		},
	} {
		t.Run(row.document, func(t *testing.T) {
			t.Parallel()
			document, err := os.ReadFile(filepath.Join("..", "dataplane", "testdata", row.document))
			if err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: string(document), exitCode: row.exitCode}}}
			result := Run(context.Background(), Config{
				Operation:   OperationMigrationApply,
				Environment: driftApplyEnvironment(t, "drift-sentence", testShadowURL, expected),
				Executor:    executor,
				Diagnostics: &log,
			})
			if !strings.Contains(log.String(), row.logged) {
				t.Fatalf("the Pod log reads %q, want Ptah's sentence %q", log.String(), row.logged)
			}
			if result.MigrationRun == nil || result.MigrationRun.DriftSummary == nil || *result.MigrationRun.DriftSummary != row.want {
				t.Fatalf("the run frames %#v, want summary %+v", result.MigrationRun, row.want)
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			if bytes.Contains(frame, []byte(row.named)) {
				t.Fatalf("the frame carries %q, which Ptah named: %s", row.named, frame)
			}
		})
	}
}
