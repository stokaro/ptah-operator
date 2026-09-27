package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planseal"
)

type scriptedResponse struct {
	stdout   string
	stderr   string
	exitCode int
	err      error
	inspect  func(CommandSpec)
	// savePlan, when set, is written to the path the command's --output
	// names, the way `schema plan --output` saves the plan it computed.
	savePlan string
	// stdoutFor, when set, prints a document that names something only the
	// command line carries, such as the path the plan was saved to.
	stdoutFor func(CommandSpec) string
}

type scriptedExecutor struct {
	t         *testing.T
	responses []scriptedResponse
	calls     []CommandSpec
}

type contextDeadlineExecutor struct {
	calls int
}

func inspectReport(reference, digest, artifactType string) string {
	return fmt.Sprintf(
		`{"reference":%q,"pinned_reference":%q,"digest":%q,"media_type":"application/vnd.oci.image.manifest.v1+json","size":42,"artifact_type":%q,"annotations":{"private":"discarded"},"layers":[]}`,
		reference, reference, digest, artifactType,
	)
}

func (e *contextDeadlineExecutor) Execute(ctx context.Context, _ CommandSpec, _, _ io.Writer) (int, error) {
	e.calls++
	<-ctx.Done()
	return -1, ctx.Err()
}

func (e *scriptedExecutor) Execute(_ context.Context, spec CommandSpec, stdout, stderr io.Writer) (int, error) {
	e.t.Helper()
	if len(e.calls) >= len(e.responses) {
		e.t.Fatalf("unexpected command: %s %v", spec.Path, spec.Args)
	}
	response := e.responses[len(e.calls)]
	e.calls = append(e.calls, spec)
	if response.inspect != nil {
		response.inspect(spec)
	}
	if response.savePlan != "" {
		outputPath := argumentAfter(spec, "--output")
		if outputPath == "" {
			e.t.Fatalf("a plan response ran without --output: %v", spec.Args)
		}
		if err := os.WriteFile(outputPath, []byte(response.savePlan), 0o600); err != nil {
			e.t.Fatalf("save the plan to %s: %v", outputPath, err)
		}
	}
	output := response.stdout
	if response.stdoutFor != nil {
		output = response.stdoutFor(spec)
	}
	_, _ = io.WriteString(stdout, output)
	_, _ = io.WriteString(stderr, response.stderr)
	return response.exitCode, response.err
}

// argumentAfter is the argument that follows flag on the command line, and
// empty when the flag is absent.
func argumentAfter(spec CommandSpec, flag string) string {
	for index, argument := range spec.Args {
		if argument == flag && index+1 < len(spec.Args) {
			return spec.Args[index+1]
		}
	}
	return ""
}

func TestBuildCommand(t *testing.T) {
	t.Parallel()

	inputs := Inputs{
		RequestedReference:     "oci://registry.example/team/schema:main",
		ResolvedReference:      "oci://registry.example/team/schema@sha256:" + strings.Repeat("a", 64),
		VerificationPolicyPath: "/policy/verification.json",
		PlanPath:               "/tmp/approved-plan.hcl",
		PlanOutputPath:         "/tmp/ptah-plan-output-1/first.plan.json",
	}
	tests := []struct {
		name      string
		operation Operation
		want      []string
	}{
		{name: "resolve", operation: OperationResolve, want: []string{"oci", "resolve", inputs.RequestedReference, "--format", "json"}},
		{name: "verify immutable resolution", operation: OperationVerify, want: []string{"oci", "verify", inputs.ResolvedReference, "--policy", inputs.VerificationPolicyPath, "--format", "json"}},
		{name: "observe", operation: OperationObserve, want: []string{"schema", "drift", "--format", "json"}},
		{name: "plan", operation: OperationPlan, want: []string{"schema", "plan", "--output", inputs.PlanOutputPath, "--json"}},
		{name: "apply", operation: OperationApply, want: []string{"schema", "apply", "--plan", inputs.PlanPath, "--auto-approve", "--json"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := BuildCommand("/opt/ptah", test.operation, inputs)
			if err != nil {
				t.Fatalf("BuildCommand() error = %v", err)
			}
			if got.Path != "/opt/ptah" || !reflect.DeepEqual(got.Args, test.want) {
				t.Fatalf("BuildCommand() = %#v, want path /opt/ptah and args %v", got, test.want)
			}
		})
	}
}

// A plan saves where the runner says, and nowhere a relative path or a flag
// would take it.
func TestBuildPlanCommandRequiresAnAbsoluteOutputPath(t *testing.T) {
	t.Parallel()

	for _, outputPath := range []string{"", "first.plan.json", "--dry-run"} {
		if spec, err := BuildCommand("/opt/ptah", OperationPlan, Inputs{PlanOutputPath: outputPath}); err == nil {
			t.Fatalf("BuildCommand() with output path %q = %v, want a refusal", outputPath, spec.Args)
		}
	}
}

func TestBuildVerifyCommandRequiresBothSourceBindings(t *testing.T) {
	t.Parallel()

	validInputs := Inputs{
		RequestedReference:     "oci://registry.example/team/schema:main",
		ResolvedReference:      "oci://registry.example/team/schema@sha256:" + strings.Repeat("a", 64),
		VerificationPolicyPath: "/policy/verification.yaml",
	}
	for name, mutate := range map[string]func(*Inputs){
		"missing requested reference": func(inputs *Inputs) { inputs.RequestedReference = "" },
		"invalid requested reference": func(inputs *Inputs) { inputs.RequestedReference = "-selector" },
		"missing resolved reference":  func(inputs *Inputs) { inputs.ResolvedReference = "" },
	} {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inputs := validInputs
			mutate(&inputs)
			if _, err := BuildCommand("/opt/ptah", OperationVerify, inputs); err == nil {
				t.Fatal("BuildCommand() succeeded without both requested and resolved source bindings")
			}
		})
	}
}

func TestResolveRecordsStrictTopLevelDigest(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("e", 64)
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: fmt.Sprintf(
		`{"reference":"oci://registry.example/schema:main","pinned_reference":"oci://registry.example/schema@%s","digest":%q,"media_type":"application/vnd.oci.image.manifest.v1+json","size":42}`,
		digest, digest,
	)}}}
	result := Run(context.Background(), Config{
		Operation: OperationResolve,
		Environment: withRunnerProtocol([]string{
			envOperationID + "=resolve-1",
			envRequestedReference + "=oci://registry.example/schema:main",
		}),
		Executor: executor,
	})
	if result.Error != nil || result.ResolvedDigest != digest || result.Stdout != "" ||
		result.ResolvedReference != "oci://registry.example/schema@"+digest {
		t.Fatalf("Run() = %#v", result)
	}
	childValues := environmentMap(executor.calls[0].Env)
	for _, key := range []string{EnvOperationID, EnvRequestedReference, EnvResolvedReference, EnvVerificationPolicy, EnvExpectedArtifactType, EnvPlanDir, EnvExpectedPlanContentDigest, EnvExpectedTargetIdentityDigest, EnvCoordinationDigest, EnvExpectedCoordinationDigest, EnvDispatchNotAfter, EnvExecutionNotAfter, EnvExpectedDatabaseEngine} {
		if _, present := childValues[key]; present {
			t.Fatalf("runner-only environment key %s was forwarded to the child", key)
		}
	}
}

func TestResolveCannotRedirectDigestSelectedRequest(t *testing.T) {
	t.Parallel()

	requestedDigest := "sha256:" + strings.Repeat("a", 64)
	otherDigest := "sha256:" + strings.Repeat("b", 64)
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: fmt.Sprintf(
		`{"reference":"oci://registry.example/schema@%s","pinned_reference":"oci://registry.example/schema@%s","digest":%q,"media_type":"application/vnd.oci.image.manifest.v1+json","size":42}`,
		requestedDigest, otherDigest, otherDigest,
	)}}}
	result := Run(context.Background(), Config{
		Operation: OperationResolve,
		Environment: withRunnerProtocol([]string{
			envOperationID + "=resolve-digest-redirect",
			envRequestedReference + "=oci://registry.example/schema@" + requestedDigest,
		}),
		Executor: executor,
	})
	if result.Error == nil || result.Error.Code != "invalid_resolve_output" || result.ResolvedDigest != "" || result.ResolvedReference != "" {
		t.Fatalf("Run() = %#v, want a fail-closed digest redirect refusal", result)
	}
}

func TestPlanRecordsExactContentDigest(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE example (id bigint);")
	executor := &scriptedExecutor{t: t, responses: stablePlanResponses(t, plan)}
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: withRunnerProtocol(databaseEnvironment("plan-1")),
		Executor:    executor,
	})
	if result.CoordinationDigest != testCoordinationDigest() {
		t.Fatalf("coordination digest = %q, want %q", result.CoordinationDigest, testCoordinationDigest())
	}
	if result.Error != nil || result.PlanContentDigest != sha256Digest([]byte(plan)) ||
		result.PlanOutcome != PlanOutcomeChanges {
		t.Fatalf("Run() = %#v", result)
	}
	if got := openSealedPlan(t, result.Stdout); got != plan {
		t.Fatalf("sealed plan opened to %q, want %q", got, plan)
	}
}

// TestPlanStdoutNeverCarriesThePlaintextPlan is the leak #449 closes: a
// successful Plan frame's Stdout must not contain the plan text (or any
// declared row value in it) in a form a Pod-log reader could recover without
// the manager's private key.
func TestPlanStdoutNeverCarriesThePlaintextPlan(t *testing.T) {
	t.Parallel()

	const declaredRowValue = "alice@example.com"
	plan := validPlanDocument("INSERT INTO users (email) VALUES ('" + declaredRowValue + "')")
	executor := &scriptedExecutor{t: t, responses: stablePlanResponses(t, plan)}
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: withRunnerProtocol(databaseEnvironment("plan-leak-check")),
		Executor:    executor,
	})
	if result.Error != nil || result.PlanOutcome != PlanOutcomeChanges {
		t.Fatalf("Run() = %#v", result)
	}
	if strings.Contains(result.Stdout, declaredRowValue) || strings.Contains(result.Stdout, plan) {
		t.Fatalf("Stdout carries the plaintext plan or its declared row value: %s", result.Stdout)
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	if bytes.Contains(frame, []byte(declaredRowValue)) {
		t.Fatalf("the framed result -- what reaches the Pod log -- carries the declared row value: %s", frame)
	}
	if got := openSealedPlan(t, result.Stdout); got != plan {
		t.Fatalf("sealed plan opened to %q, want %q", got, plan)
	}
}

// A Plan Job that is not given a well-formed manager public key refuses
// before it starts its executor: no plan is computed and no child runs, so
// there is nothing to seal and nothing that could leak unsealed.
func TestPlanRefusesBeforeStartingItsExecutorWithoutAWellFormedSealKey(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "missing", value: ""},
		{name: "not base64", value: "not-base64!!"},
		{name: "wrong length", value: base64.StdEncoding.EncodeToString([]byte("too short"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			environment := withRunnerProtocol(environmentWithout(databaseEnvironment("plan-bad-key"), EnvPlanSealPublicKey))
			if test.value != "" {
				environment = append(environment, EnvPlanSealPublicKey+"="+test.value)
			}
			executor := &scriptedExecutor{t: t}
			result := Run(context.Background(), Config{
				Operation:   OperationPlan,
				Environment: environment,
				Executor:    executor,
			})
			if result.Error == nil || result.Error.Code != "missing_plan_seal_key" {
				t.Fatalf("Run() error = %#v, want a missing_plan_seal_key refusal", result.Error)
			}
			if len(executor.calls) != 0 {
				t.Fatalf("executor was called %d times, want the refusal before any child dispatch", len(executor.calls))
			}
		})
	}
}

// canonicalPlanDocument is a plan in the shape Ptah's MarshalPlanFile writes:
// indented, HTML-escaped, and closed by one newline. Those are the bytes
// `schema plan --dry-run` printed to the runner before it read reports, and
// the bytes `--output` saves now; Ptah renders both from one value.
const canonicalPlanDocument = `{
  "format_version": 1,
  "name": "plan_1f0c3a9b2e4d",
  "dialect": "postgres",
  "from_fingerprint": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "to_fingerprint": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "destructive": false,
  "statements": [
    {
      "sql": "CREATE TABLE \"orders\" (\n  \"id\" bigint NOT NULL,\n  \"note\" text DEFAULT '\u003cnone\u003e'\n)",
      "severity": "safe",
      "reason": "does not remove data or tighten constraints"
    }
  ]
}
`

// canonicalPlanContentDigest is the SHA-256 of canonicalPlanDocument, taken
// with shasum rather than with the function under test.
const canonicalPlanContentDigest = "sha256:548640925a5f37ee677eae12710ee1c70c27f98822491bc7f33316e51d4519f1"

// A plan stored and approved before the runner read reports carries the digest
// of the bytes the dry run printed. Reading the saved file instead must publish
// those bytes unchanged, under the same digest, and an Apply bound to that
// digest must still run: otherwise every approval recorded before the change
// would stop matching the plan it approved.
func TestPlanContentDigestIsTheDigestOfTheSavedBytes(t *testing.T) {
	t.Parallel()

	executor := &scriptedExecutor{t: t, responses: stablePlanResponses(t, canonicalPlanDocument)}
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: withRunnerProtocol(databaseEnvironment("plan-canonical")),
		Executor:    executor,
		TempDir:     t.TempDir(),
	})
	if result.Error != nil || result.PlanOutcome != PlanOutcomeChanges {
		t.Fatalf("Run(Plan) = %#v", result)
	}
	if got := openSealedPlan(t, result.Stdout); got != canonicalPlanDocument {
		t.Fatalf("published plan = %q, want the saved bytes unchanged", got)
	}
	if result.PlanContentDigest != canonicalPlanContentDigest {
		t.Fatalf("plan content digest = %s, want %s", result.PlanContentDigest, canonicalPlanContentDigest)
	}

	planDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), []byte(canonicalPlanDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	applyExecutor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: applyReport(t, dataplane.SchemaApplyOutcomeApplied, canonicalPlanDocument),
	}}}
	applied := Run(context.Background(), Config{
		Operation: OperationApply,
		Environment: withRunnerProtocol(append(databaseEnvironment("apply-canonical"),
			envPlanDir+"="+planDir,
			envExpectedPlanDigest+"="+canonicalPlanContentDigest,
			envExpectedCoordination+"="+testCoordinationDigest(),
			envExpectedTargetDigest+"="+databaseTargetDigest(t),
		)),
		Executor: applyExecutor,
		TempDir:  t.TempDir(),
	})
	if applied.Error != nil || !applied.MutationStarted || applied.Uncertain ||
		applied.PlanContentDigest != canonicalPlanContentDigest {
		t.Fatalf("Run(Apply) of a plan approved under the old digest = %#v", applied)
	}
}

func TestExecutablePlanSizeBoundary(t *testing.T) {
	// This test intentionally exercises multi-megabyte buffers serially. Its
	// exact-limit plan is dominated by '<' bytes. Before sealing, that forced
	// encoding/json's worst-case six-byte HTML escape in the framed result;
	// the frame now carries the sealed plan as base64, whose alphabet has
	// nothing for JSON to escape, so the byte that dominates the plaintext no
	// longer dominates the frame. The exact-limit plan itself is still worth
	// generating this way: it is still the boundary the plan-read and
	// plan-save paths below have to hold exactly, sealing aside.
	exactPlan := exactSizePlanDocument(t, int(DefaultMaxPlanBytes))
	if exactPlan[len(exactPlan)-1] != '\n' {
		t.Fatal("exact-limit plan does not count its trailing newline")
	}
	// The report a plan read prints embeds the plan, so at the limit it is
	// larger than the result limit. A report bounded by the result limit would
	// refuse this plan as truncated output; this row is what says it is not.
	if len(planReport("/tmp/plan.json", exactPlan)) <= int(DefaultMaxResultBytes) {
		t.Fatal("the exact-limit plan report fits the result limit, so this row no longer measures the report bound")
	}

	planExecutor := &scriptedExecutor{t: t, responses: stablePlanResponses(t, exactPlan)}
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: withRunnerProtocol(databaseEnvironment("plan-exact-limit")),
		Executor:    planExecutor,
		TempDir:     t.TempDir(),
	})
	if result.Error != nil || result.PlanOutcome != PlanOutcomeChanges ||
		result.PlanContentDigest != sha256Digest([]byte(exactPlan)) {
		t.Fatalf("exact-limit Run(Plan) = error %#v, outcome %q, digest %q",
			result.Error, result.PlanOutcome, result.PlanContentDigest)
	}
	// The sealed box adds a fixed 48 bytes to the plaintext -- a prepended
	// 32-byte ephemeral public key and a 16-byte Poly1305 tag -- whatever the
	// plaintext is, and base64 then expands that by exactly 4/3, rounded up
	// to a multiple of 4. An exact-limit plan is the one input that pins both
	// terms of that arithmetic at once.
	wantSealedLength := base64.StdEncoding.EncodedLen(int(DefaultMaxPlanBytes) + 48)
	if len(result.Stdout) != wantSealedLength {
		t.Fatalf("sealed plan bytes = %d, want %d (plaintext %d plus the fixed sealed-box and base64 overhead)",
			len(result.Stdout), wantSealedLength, DefaultMaxPlanBytes)
	}
	if got := openSealedPlan(t, result.Stdout); got != exactPlan {
		t.Fatal("sealed plan opened to different bytes than the exact-limit plaintext")
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame(exact-limit plan) error = %v", err)
	}
	if int64(len(frame)) >= DefaultMaxFrameBytes {
		t.Fatalf("worst-case frame bytes = %d, parser cap = %d; want strict headroom", len(frame), DefaultMaxFrameBytes)
	}
	parsed, err := ParseResultFor(frame, OperationPlan, result.OperationID)
	if err != nil {
		t.Fatalf("ParseResultFor(exact-limit plan) error = %v", err)
	}
	if parsed.Stdout != result.Stdout || parsed.PlanContentDigest != result.PlanContentDigest {
		t.Fatal("frame round trip changed exact-limit plan bytes")
	}
	openedPlan := openSealedPlan(t, parsed.Stdout)
	if openedPlan != exactPlan {
		t.Fatal("frame round trip changed the sealed plan's opened bytes")
	}

	planDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(planDir, "000.plan"), []byte(openedPlan), 0o600); err != nil {
		t.Fatal(err)
	}
	applyExecutor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: applyReport(t, dataplane.SchemaApplyOutcomeApplied, exactPlan),
	}}}
	applyEnvironment := append(databaseEnvironment("apply-exact-limit"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+parsed.PlanContentDigest,
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
	)
	applyResult := Run(context.Background(), Config{
		Operation:   OperationApply,
		Environment: withRunnerProtocol(applyEnvironment),
		Executor:    applyExecutor,
		TempDir:     t.TempDir(),
	})
	if applyResult.Error != nil || !applyResult.MutationStarted || applyResult.Uncertain ||
		applyResult.PlanContentDigest != parsed.PlanContentDigest {
		t.Fatalf("exact-limit Run(Apply) = %#v", applyResult)
	}

	// One byte over, and the saved file is refused by its size on the first
	// read, before anything reads past the limit.
	oversizedPlan := exactSizePlanDocument(t, int(DefaultMaxPlanBytes)+1)
	oversizedExecutor := &scriptedExecutor{t: t, responses: []scriptedResponse{planResponse(oversizedPlan)}}
	oversizedResult := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: withRunnerProtocol(databaseEnvironment("plan-over-limit")),
		Executor:    oversizedExecutor,
		TempDir:     t.TempDir(),
	})
	if oversizedResult.Error == nil || oversizedResult.Error.Code != "invalid_plan_output" ||
		!strings.Contains(oversizedResult.Error.Message, "plan limit") ||
		oversizedResult.Stdout != "" || oversizedResult.PlanContentDigest != "" || oversizedResult.PlanOutcome != "" {
		t.Fatalf("limit+1 Run(Plan) = %#v", oversizedResult)
	}
	if oversizedResult.Truncation != nil || len(oversizedExecutor.calls) != 1 {
		t.Fatalf("limit+1 truncation = %#v, calls = %d; want the saved file refused on the first read",
			oversizedResult.Truncation, len(oversizedExecutor.calls))
	}
}

func TestRunRejectsSizeContractDrift(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		config Config
	}{
		{
			name: "plan exceeds capture",
			config: Config{
				Operation: OperationPlan, MaxResultBytes: 1024, MaxPlanBytes: 1025,
			},
		},
		{
			name: "capture exceeds supported contract",
			config: Config{
				Operation: OperationPlan, MaxResultBytes: DefaultMaxResultBytes + 1,
			},
		},
		{
			name: "plan exceeds supported contract",
			config: Config{
				Operation: OperationPlan, MaxPlanBytes: DefaultMaxPlanBytes + 1,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			test.config.Environment = withRunnerProtocol(databaseEnvironment("invalid-size-contract"))
			test.config.Executor = &scriptedExecutor{t: t}
			result := Run(context.Background(), test.config)
			if result.Error == nil || result.Error.Code != "invalid_configuration" {
				t.Fatalf("Run() error = %#v, want invalid_configuration", result.Error)
			}
		})
	}
}

func TestPlanRejectsUnstableConsecutiveSnapshots(t *testing.T) {
	t.Parallel()

	first := validPlanDocument("CREATE TABLE example (id bigint);")
	second := validPlanDocument("CREATE TABLE example (id bigint, name text);")
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{planResponse(first), planResponse(second)}}
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("plan-unstable")), Executor: executor,
	})
	if result.Error == nil || result.Error.Code != "unstable_plan" || result.Stdout != "" || result.PlanOutcome != "" {
		t.Fatalf("Run() = %#v", result)
	}
	if len(executor.calls) != 2 {
		t.Fatalf("commands = %d, want two independent plan reads", len(executor.calls))
	}
	if argumentAfter(executor.calls[0], "--output") == argumentAfter(executor.calls[1], "--output") {
		t.Fatal("both plan reads saved to one path, so the second could be read as the first")
	}
}

func TestPlanRejectsMixedChangesAndNoChangesSnapshots(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE example (id bigint);")
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{planNoChangesResponse(), planResponse(plan)}}
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("plan-mixed")), Executor: executor,
	})
	if result.Error == nil || result.Error.Code != "unstable_plan" || result.PlanOutcome != "" {
		t.Fatalf("Run() = %#v", result)
	}
}

func TestPlanValidatesDialectAgainstConfiguredDatabaseEngine(t *testing.T) {
	t.Parallel()

	plan := strings.Replace(validPlanDocument("CREATE TABLE example (id bigint);"), `"dialect":"postgres"`, `"dialect":"mysql"`, 1)
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{planResponse(plan), planResponse(plan)}}
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("plan-wrong-dialect")), Executor: executor,
	})
	if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.Stdout != "" || len(executor.calls) != 2 {
		t.Fatalf("Run() = %#v, calls = %d", result, len(executor.calls))
	}
}

// The dry run is accepted only when its report says it read the saved plan and
// listed that plan's statements in order. Each row is a dry run that must be
// refused; the rows that publish a plan elsewhere in this file are the other
// half.
func TestPlanRequiresNativeDryRunToMatchReviewedStatements(t *testing.T) {
	t.Parallel()

	plan := fmt.Sprintf(
		`{"format_version":1,"name":"operator-plan","dialect":"postgres","from_fingerprint":"sha256:%s",`+
			`"to_fingerprint":"sha256:%s","destructive":false,"statements":[`+
			`{"sql":"CREATE TABLE first_table (id bigint)","severity":"safe","reason":"new table"},`+
			`{"sql":"CREATE TABLE second_table (id bigint)","severity":"safe","reason":"new table"}]}`+"\n",
		strings.Repeat("a", 64), strings.Repeat("b", 64),
	)
	dryRun := func(edit func(*dataplane.SchemaApplyReport)) scriptedResponse {
		report := dataplane.SchemaApplyReport{
			ContractVersion: 1,
			Outcome:         dataplane.SchemaApplyOutcomeDryRun,
			PlanName:        "operator-plan",
			PlanDigest:      sha256Digest([]byte(plan)),
			Statements:      []string{"CREATE TABLE first_table (id bigint)", "CREATE TABLE second_table (id bigint)"},
		}
		edit(&report)
		return scriptedResponse{stdout: encodeApplyReport(t, report)}
	}
	for _, test := range []struct {
		name       string
		validation scriptedResponse
	}{
		{name: "another statement", validation: dryRun(func(report *dataplane.SchemaApplyReport) {
			report.Statements[1] = "CREATE TABLE other_table (id bigint)"
		})},
		{name: "the statements reordered", validation: dryRun(func(report *dataplane.SchemaApplyReport) {
			report.Statements[0], report.Statements[1] = report.Statements[1], report.Statements[0]
		})},
		{name: "a statement missing", validation: dryRun(func(report *dataplane.SchemaApplyReport) {
			report.Statements = report.Statements[:1]
		})},
		{name: "another plan read", validation: dryRun(func(report *dataplane.SchemaApplyReport) {
			report.PlanDigest = sha256Digest([]byte("another plan"))
		})},
		{name: "a plan that went stale", validation: func() scriptedResponse {
			response := dryRun(func(report *dataplane.SchemaApplyReport) {
				report.Outcome = dataplane.SchemaApplyOutcomeRefused
				report.Statements = nil
				report.Refusal = &dataplane.SchemaRefusal{Code: dataplane.SchemaRefusalStalePlan, Changed: "schema"}
			})
			response.exitCode = 2
			return response
		}()},
		{name: "a run that applied instead", validation: dryRun(func(report *dataplane.SchemaApplyReport) {
			report.Outcome = dataplane.SchemaApplyOutcomeApplied
		})},
		{name: "the human transcript", validation: scriptedResponse{
			stdout: "Planned schema changes:\nCREATE TABLE first_table (id bigint);\nCREATE TABLE second_table (id bigint);\n",
		}},
		{name: "no report at all", validation: scriptedResponse{stderr: "error: unknown flag: --json\n", exitCode: 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{planResponse(plan), planResponse(plan), test.validation}}
			result := Run(context.Background(), Config{
				Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("plan-validation-mismatch")), Executor: executor,
			})
			if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.Stdout != "" ||
				result.PlanContentDigest != "" || result.PlanOutcome != "" {
				t.Fatalf("Run() = %#v", result)
			}
			if len(executor.calls) != 3 {
				t.Fatalf("commands = %d, want two plan reads and one native dry-run", len(executor.calls))
			}
		})
	}
}

// Under --json Ptah writes everything meant for a person to standard error: the
// planned statements, a note, even a sentence that reads like a refusal. The
// runner reads none of it, so a plan whose reports say it succeeded is
// published whatever standard error carried.
func TestPlanIgnoresWhatPtahWritesForAPerson(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE example (id bigint);")
	responses := stablePlanResponses(t, plan)
	for index := range responses {
		responses[index].stderr = "warning: selector matched nothing\n" +
			"error: refusing to change protected table(s) countries\n" +
			"Planned schema changes:\nCREATE TABLE example (id bigint);\n"
	}
	executor := &scriptedExecutor{t: t, responses: responses}
	var diagnostics bytes.Buffer
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("plan-person-text")),
		Executor: executor, Diagnostics: &diagnostics,
	})
	if result.Error != nil || result.PlanOutcome != PlanOutcomeChanges {
		t.Fatalf("Run() = %#v", result)
	}
	if got := openSealedPlan(t, result.Stdout); got != plan {
		t.Fatalf("sealed plan opened to %q, want %q", got, plan)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("diagnostics = %q, want nothing from Ptah's standard error", diagnostics.String())
	}
}

func TestPlanRejectsATruncatedValidationReport(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE example (id bigint);")
	responses := []scriptedResponse{planResponse(plan), planResponse(plan), {stdout: strings.Repeat("x", 1024)}}
	executor := &scriptedExecutor{t: t, responses: responses}
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("plan-validation-truncation")),
		Executor: executor, MaxResultBytes: 512, MaxPlanBytes: 512,
	})
	if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.Stdout != "" {
		t.Fatalf("Run() = %#v", result)
	}
	if result.Truncation == nil || !result.Truncation.Stdout || result.Truncation.StdoutBytesDropped != 512 {
		t.Fatalf("truncation = %#v", result.Truncation)
	}
}

func TestPlanWithCredentialSubstringFailsWithoutEmittingMutatedPayload(t *testing.T) {
	t.Parallel()

	secret := "credential-substring"
	plan := validPlanDocument("COMMENT ON TABLE example IS '" + secret + "';")
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{planResponse(plan), planResponse(plan)}}
	environment := append(databaseEnvironment("plan-secret"), EnvOCIToken+"="+secret)
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: withRunnerProtocol(environment),
		Executor:    executor,
	})
	if result.Error == nil || result.Error.Code != "credential_leak" {
		t.Fatalf("error = %#v, want credential_leak", result.Error)
	}
	if result.Stdout != "" || result.PlanContentDigest != "" {
		t.Fatalf("runner emitted or hashed a rewritten executable plan: %#v", result)
	}
}

func TestObserveDriftExitOneIsAFramedResult(t *testing.T) {
	t.Parallel()

	report := `{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","findings":[{"category":"columns_added","count":1,"severity":"warning"}],"diff":{"columns_added":["app.users.email"]}}`
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: report, exitCode: 1}}}
	result := Run(context.Background(), Config{
		Operation:   OperationObserve,
		Environment: withRunnerProtocol(append(databaseEnvironment("observe-1"), "PTAH_EXCLUDE=audit.*,\"legacy,archive\"")),
		Executor:    executor,
	})
	if result.ChildExitCode != 0 || result.Error != nil {
		t.Fatalf("Run() = %#v, want validated drift as protocol success", result)
	}
	if len(executor.calls) != 1 || !reflect.DeepEqual(executor.calls[0].Args, []string{"schema", "drift", "--format", "json"}) {
		t.Fatalf("commands = %#v, want one authoritative drift read", executor.calls)
	}
	if result.DriftReportDigest == "" || !result.ObservedDrift || result.ObservedDialect != "postgres" ||
		result.HighestDriftSeverity != "warning" || result.DriftFindingCount != 1 ||
		!reflect.DeepEqual(result.DriftFindings, []DriftFindingSummary{{Category: "columns_added", Count: 1, Severity: "warning"}}) ||
		result.DriftFindingsTruncated {
		t.Fatal("drift-present observation lacks a report digest")
	}
	if result.Stdout != "" {
		t.Fatal("raw drift report was copied into the credential-free frame")
	}
	if _, present := environmentMap(executor.calls[0].Env)["PTAH_EXCLUDE"]; present {
		t.Fatal("raw drift command received planning-only exclusion selectors")
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	parsed, err := ParseResultFor(frame, OperationObserve, "observe-1")
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}
	if parsed.ChildExitCode != 0 || parsed.Stdout != "" || parsed.DriftReportDigest != result.DriftReportDigest ||
		!parsed.ObservedDrift || parsed.DriftFindingCount != 1 || !reflect.DeepEqual(parsed.DriftFindings, result.DriftFindings) {
		t.Fatalf("parsed result = %#v", parsed)
	}
}

func TestObservePublishesCanonicalFindingSummaries(t *testing.T) {
	t.Parallel()

	findings := []dataplane.DriftFinding{
		{Category: "tables_added", Count: 3, Severity: "safe"},
		{Category: "columns_removed", Count: 1, Severity: " destructive "},
		{Category: "indexes_added", Count: 2, Severity: "warning"},
	}
	report, err := json.Marshal(dataplane.DriftReport{
		Drift: true, Failed: true, FailureThreshold: "all", HighestSeverity: " DESTRUCTIVE ",
		Dialect: "postgres", Findings: findings, Diff: json.RawMessage(`{"changed":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := Run(context.Background(), Config{
		Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment("observe-bounded-findings")),
		Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: string(report), exitCode: 1}}},
	})
	if result.Error != nil || result.DriftFindingCount != 6 || len(result.DriftFindings) != 3 ||
		result.DriftFindingsTruncated {
		t.Fatalf("Run() findings = %#v", result)
	}
	if got := result.DriftFindings; !reflect.DeepEqual(got, []DriftFindingSummary{
		{Category: "columns_removed", Count: 1, Severity: "destructive"},
		{Category: "indexes_added", Count: 2, Severity: "warning"},
		{Category: "tables_added", Count: 3, Severity: "safe"},
	}) {
		t.Fatalf("canonical findings = %#v", got)
	}
	if _, err := MarshalFrame(result); err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
}

// Ptah counts FORCE ROW LEVEL SECURITY apart from ENABLE. Turning it on rates
// safe; turning it off rates destructive, because the table's owner then reads
// and writes past every policy. The operator knew neither category, and a
// report of either failed Observe.
func TestObserveFramesTheForcedRowSecurityCategories(t *testing.T) {
	t.Parallel()

	for _, test := range []struct{ category, severity string }{
		{category: "rls_force_added", severity: "safe"},
		{category: "rls_force_removed", severity: "destructive"},
	} {
		t.Run(test.category, func(t *testing.T) {
			t.Parallel()
			operationID := "observe-" + strings.ReplaceAll(test.category, "_", "-")
			report := fmt.Sprintf(`{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":%q,"dialect":"postgres","findings":[{"category":%q,"count":1,"severity":%q}],"diff":{"changed":true}}`,
				test.severity, test.category, test.severity)
			result := Run(context.Background(), Config{
				Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment(operationID)),
				Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: report, exitCode: 1}}},
			})
			want := []DriftFindingSummary{{Category: test.category, Count: 1, Severity: test.severity}}
			if result.Error != nil || !result.ObservedDrift || result.HighestDriftSeverity != test.severity ||
				result.DriftFindingCount != 1 || !reflect.DeepEqual(result.DriftFindings, want) {
				t.Fatalf("Run() = error %#v, drift %t, highest %q, count %d, findings %#v; want %#v",
					result.Error, result.ObservedDrift, result.HighestDriftSeverity, result.DriftFindingCount,
					result.DriftFindings, want)
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			parsed, err := ParseResultFor(frame, OperationObserve, operationID)
			if err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
			if !reflect.DeepEqual(parsed.DriftFindings, want) {
				t.Fatalf("parsed findings = %#v, want %#v", parsed.DriftFindings, want)
			}
		})
	}
}

// Every category the pinned Ptah can emit passes the runner and the frame on
// its own. The list is support/ptah-drift-categories.json, which
// hack/ptahdriftcategories holds to the pinned source, so a category a Ptah
// bump adds fails here by name until the vocabulary takes it.
func TestObserveFramesEveryCategoryThePinnedPtahEmits(t *testing.T) {
	t.Parallel()

	for _, category := range pinnedPtahDriftCategories(t) {
		t.Run(category, func(t *testing.T) {
			t.Parallel()
			operationID := "observe-pinned-" + strings.ReplaceAll(category, "_", "-")
			report := fmt.Sprintf(`{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","findings":[{"category":%q,"count":3,"severity":"warning"}],"diff":{"changed":true}}`,
				category)
			result := Run(context.Background(), Config{
				Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment(operationID)),
				Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: report, exitCode: 1}}},
			})
			if result.Error != nil || len(result.DriftFindings) != 1 || result.DriftFindings[0].Category != category {
				t.Fatalf("Run() = error %#v, findings %#v; want %s framed", result.Error, result.DriftFindings, category)
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			if _, err := ParseResultFor(frame, OperationObserve, operationID); err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
		})
	}
}

// pinnedPtahDriftCategories reads the categories the pinned Ptah can emit, and
// refuses a list with nothing in it: a loop over no category proves nothing.
func pinnedPtahDriftCategories(t *testing.T) []string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", "support", "ptah-drift-categories.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vendored struct {
		Categories []string `json:"categories"`
	}
	if err := json.Unmarshal(content, &vendored); err != nil {
		t.Fatal(err)
	}
	if len(vendored.Categories) == 0 {
		t.Fatal("support/ptah-drift-categories.json lists no category")
	}
	return vendored.Categories
}

func TestObserveRejectsUnknownIdentifierFindingCategory(t *testing.T) {
	t.Parallel()

	report := `{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","findings":[{"category":"private_schema_name","count":1,"severity":"warning"}],"diff":{}}`
	result := Run(context.Background(), Config{
		Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment("observe-unknown-finding")),
		Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: report, exitCode: 1}}},
	})
	if result.Error == nil || result.Error.Code != "invalid_observed_state" || len(result.DriftFindings) != 0 {
		t.Fatalf("Run() = %#v, want an invalid_observed_state without findings", result)
	}
}

func TestObserveRejectsFindingSeverityInconsistentWithHighest(t *testing.T) {
	t.Parallel()

	report := `{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","findings":[{"category":"tables_added","count":1,"severity":"safe"}],"diff":{}}`
	result := Run(context.Background(), Config{
		Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment("observe-inconsistent-finding-severity")),
		Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: report, exitCode: 1}}},
	})
	if result.Error == nil || result.Error.Code != "invalid_observed_state" || len(result.DriftFindings) != 0 {
		t.Fatalf("Run() = %#v, want an invalid_observed_state without findings", result)
	}
}

func TestObserveNormalizesNativeConvergedSafeSeverity(t *testing.T) {
	t.Parallel()

	report := `{"drift":false,"failed":false,"failure_threshold":"all","highest_severity":"safe","dialect":"postgres","findings":[],"diff":{"tables_added":[],"tables_removed":[],"columns_added":[],"columns_removed":[],"columns_changed":[]}}`
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: report}}}
	result := Run(context.Background(), Config{
		Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment("observe-converged-safe")), Executor: executor,
	})
	if result.Error != nil || result.ChildExitCode != 0 || result.DriftReportDigest == "" ||
		result.ObservedDialect != "postgres" || result.ObservedDrift ||
		result.HighestDriftSeverity != "" || result.DriftFindingCount != 0 {
		t.Fatalf("Run() = %#v, want a normalized converged observation", result)
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	if _, err := ParseResultFor(frame, OperationObserve, "observe-converged-safe"); err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}
}

func TestObservePreservesSafeSeverityForRealDrift(t *testing.T) {
	t.Parallel()

	report := `{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":" SAFE ","dialect":"postgres","findings":[{"category":"tables_added","count":1,"severity":"safe"}],"diff":{"tables_added":["app.audit"]}}`
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: report, exitCode: 1}}}
	result := Run(context.Background(), Config{
		Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment("observe-real-safe-drift")), Executor: executor,
	})
	if result.Error != nil || result.ChildExitCode != 0 || result.DriftReportDigest == "" ||
		!result.ObservedDrift || result.HighestDriftSeverity != "safe" || result.DriftFindingCount != 1 {
		t.Fatalf("Run() = %#v, want real drift severity preserved", result)
	}
	if _, err := MarshalFrame(result); err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
}

// A grant is drift the report has no category for. Ptah says drift, rates the
// empty list safe, and lists no finding; the pinned build leaves the list out
// altogether. The observation is framed as drift with a zero count, so Plan
// still runs, and the frame carries nothing the grant says.
func TestObserveFramesDriftTheReportHasNoCategoryFor(t *testing.T) {
	t.Parallel()

	const grantOnlyDiff = `{"grants_added":[{"role":"reporting","privilege":"SELECT","object_type":"TABLE","object_name":"orders","with_option":false}]}`
	tests := []struct {
		name   string
		report string
	}{
		{
			name:   "findings absent",
			report: `{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":"safe","dialect":"postgres","diff":` + grantOnlyDiff + `}`,
		},
		{
			name:   "findings empty",
			report: `{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":"safe","dialect":"postgres","findings":[],"diff":` + grantOnlyDiff + `}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			operationID := "observe-grant-only-" + strings.ReplaceAll(test.name, " ", "-")
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: test.report, exitCode: 1}}}
			result := Run(context.Background(), Config{
				Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment(operationID)), Executor: executor,
			})
			if result.Error != nil || result.ChildExitCode != 0 || result.DriftReportDigest == "" || result.Stdout != "" {
				t.Fatalf("Run() = %#v, want a framed observation of drift", result)
			}
			if !result.ObservedDrift || result.HighestDriftSeverity != "safe" || result.DriftFindingCount != 0 ||
				len(result.DriftFindings) != 0 || result.DriftFindingsTruncated {
				t.Fatalf("drift summary = drift %t, highest %q, count %d, findings %#v, truncated %t; want drift in no category",
					result.ObservedDrift, result.HighestDriftSeverity, result.DriftFindingCount,
					result.DriftFindings, result.DriftFindingsTruncated)
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			for _, disclosed := range []string{"reporting", "orders", "SELECT", "grants_added"} {
				if bytes.Contains(frame, []byte(disclosed)) {
					t.Fatalf("frame carries %q from the drift report:\n%s", disclosed, frame)
				}
			}
			parsed, err := ParseResultFor(frame, OperationObserve, operationID)
			if err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
			if !parsed.ObservedDrift || parsed.HighestDriftSeverity != "safe" || parsed.DriftFindingCount != 0 ||
				len(parsed.DriftFindings) != 0 || parsed.DriftReportDigest != result.DriftReportDigest {
				t.Fatalf("parsed result = %#v", parsed)
			}
		})
	}
}

// A report with no findings has nothing to rate above safe. One that claims a
// higher severity describes findings it does not hold, and is refused the way a
// list whose first entry disagrees with the highest severity is.
func TestObserveRefusesAnUncategorizedDriftAboveSafe(t *testing.T) {
	t.Parallel()

	report := `{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","diff":{"grants_added":[]}}`
	result := Run(context.Background(), Config{
		Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment("observe-uncategorized-warning")),
		Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: report, exitCode: 1}}},
	})
	if result.Error == nil || result.Error.Code != "invalid_observed_state" || result.ObservedDrift ||
		result.HighestDriftSeverity != "" || result.DriftReportDigest != "" {
		t.Fatalf("Run() = %#v, want an invalid_observed_state with no summary", result)
	}
}

func TestObserveFramesInconsistentConvergedSummaryAsInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		report string
	}{
		{
			name:   "drift severity",
			report: `{"drift":false,"failed":false,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","findings":[],"diff":{}}`,
		},
		{
			name:   "positive finding",
			report: `{"drift":false,"failed":false,"failure_threshold":"all","highest_severity":"safe","dialect":"postgres","findings":[{"category":"columns_added","count":1,"severity":"safe"}],"diff":{}}`,
		},
		{
			name:   "zero-count finding",
			report: `{"drift":false,"failed":false,"failure_threshold":"all","highest_severity":"","dialect":"postgres","findings":[{"category":"columns_added","count":0,"severity":"safe"}],"diff":{}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			operationID := "observe-inconsistent-" + strings.ReplaceAll(test.name, " ", "-")
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: test.report}}}
			result := Run(context.Background(), Config{
				Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment(operationID)), Executor: executor,
			})
			if result.Error == nil || result.Error.Code != "invalid_observed_state" ||
				result.DriftReportDigest != "" || result.ObservedDialect != "" || result.ObservedDrift ||
				result.HighestDriftSeverity != "" || result.DriftFindingCount != 0 {
				t.Fatalf("Run() = %#v, want a frameable invalid_observed_state", result)
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			parsed, err := ParseResultFor(frame, OperationObserve, operationID)
			if err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
			if parsed.Error == nil || parsed.Error.Code != "invalid_observed_state" {
				t.Fatalf("parsed result = %#v", parsed)
			}
		})
	}
}

func TestObserveNeverPublishesNativeFailureDetails(t *testing.T) {
	t.Parallel()

	const sensitiveLiteral = "operator-private-schema-literal"
	report := `{"drift":false,"failed":false,"failure_threshold":"all","highest_severity":"","dialect":"postgres","findings":[],"diff":{},"error":"failed near ` + sensitiveLiteral + `"}`
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: report,
		stderr: "native parse failure: DEFAULT '" + sensitiveLiteral + "'\n",
	}}}
	diagnostics := &bytes.Buffer{}
	result := Run(context.Background(), Config{
		Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment("observe-private-failure")),
		Executor: executor, Diagnostics: diagnostics,
	})
	if result.Error == nil || result.Error.Code != "invalid_observed_state" || result.Stdout != "" {
		t.Fatalf("Run() = %#v", result)
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	if bytes.Contains(frame, []byte(sensitiveLiteral)) || strings.Contains(diagnostics.String(), sensitiveLiteral) {
		t.Fatal("native Observe failure disclosed a protected schema literal")
	}
}

func TestRunRedactsCredentialsAndURLPasswords(t *testing.T) {
	t.Parallel()

	databaseURL := "postgres://dbuser:db-password@db.example/app?sslmode=require"
	devURL := "postgres://devuser:dev-password@dev.example/app"
	registryPassword := "registry-password"
	registryToken := "registry-token"
	output := strings.Join([]string{
		databaseURL,
		devURL,
		registryPassword,
		registryToken,
		"https://alice:unrelated-password@example.net/private",
	}, " ")
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: output, stderr: output}}}
	var diagnostics bytes.Buffer
	result := Run(context.Background(), Config{
		Operation: OperationPlan,
		Environment: withRunnerProtocol([]string{
			envOperationID + "=redact-1",
			envDatabaseURL + "=" + databaseURL,
			envCoordinationDigest + "=" + testCoordinationDigest(),
			"PTAH_DEV_URL=" + devURL,
			"PTAH_OCI_PASSWORD=" + registryPassword,
			"PTAH_OCI_TOKEN=" + registryToken,
			envExpectedDatabaseEngine + "=PostgreSQL",
			envPlanSealPublicKey + "=" + testPlanSealKey.PublicKey().Encode(),
		}),
		Diagnostics: &diagnostics,
		Executor:    executor,
	})
	combined := result.Stdout + diagnostics.String()
	for _, secret := range []string{databaseURL, devURL, registryPassword, registryToken, "unrelated-password"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("sanitized output contains secret %q: %s", secret, combined)
		}
	}
	if result.Error == nil || result.Error.Code != "invalid_plan_output" {
		t.Fatalf("Run() error = %#v, want a generic invalid_plan_output refusal", result.Error)
	}
	for _, argument := range executor.calls[0].Args {
		if strings.Contains(argument, "password") || strings.Contains(argument, "token") {
			t.Fatalf("credential leaked into argv: %v", executor.calls[0].Args)
		}
	}
}

func TestRunBoundsOutputWithoutTreatingWritesAsShort(t *testing.T) {
	t.Parallel()

	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: strings.Repeat("s", 64),
		stderr: strings.Repeat("e", 40),
	}}}
	var diagnostics bytes.Buffer
	result := Run(context.Background(), Config{
		Operation: OperationResolve,
		Environment: withRunnerProtocol([]string{
			envOperationID + "=bounded-1",
			envRequestedReference + "=oci://registry.example/schema:main",
		}),
		MaxResultBytes: 8,
		Diagnostics:    &diagnostics,
		Executor:       executor,
	})
	if result.Stdout != "" {
		t.Fatalf("stdout = %q, want no native output", result.Stdout)
	}
	if result.Error == nil || result.Error.Code != "output_truncated" {
		t.Fatalf("error = %#v, want output_truncated", result.Error)
	}
	if result.Truncation == nil || !result.Truncation.Stdout || result.Truncation.StdoutBytesDropped != 56 || !result.Truncation.Stderr || result.Truncation.StderrBytesDropped != 32 {
		t.Fatalf("truncation = %#v", result.Truncation)
	}
	if strings.Contains(diagnostics.String(), strings.Repeat("e", 8)) || !strings.Contains(diagnostics.String(), "stderr truncated") {
		t.Fatalf("diagnostics = %q", diagnostics.String())
	}
}

func TestApplyReconstructsChunksInLexicalOrderAndRemovesTempPlan(t *testing.T) {
	t.Parallel()

	planDir := t.TempDir()
	content := []byte(validPlanDocument("CREATE TABLE apply_order (id bigint)"))
	split := len(content) / 2
	if err := os.WriteFile(filepath.Join(planDir, "chunk-010"), content[split:], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), content[:split], 0o600); err != nil {
		t.Fatal(err)
	}
	wantedDigest := sha256Digest(content)
	var tempPlanPath string
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: applyReport(t, dataplane.SchemaApplyOutcomeApplied, string(content)),
		inspect: func(spec CommandSpec) {
			want := []string{"schema", "apply", "--plan", spec.Args[3], "--auto-approve", "--json"}
			if !reflect.DeepEqual(spec.Args, want) {
				t.Fatalf("apply args = %v, want %v", spec.Args, want)
			}
			tempPlanPath = spec.Args[3]
			got, err := os.ReadFile(tempPlanPath)
			if err != nil {
				t.Fatalf("read temporary plan: %v", err)
			}
			if !bytes.Equal(got, content) {
				t.Fatalf("temporary plan = %q, want %q", got, content)
			}
			values := environmentMap(spec.Env)
			if _, present := values[envSchemaFile]; present {
				t.Fatalf("exact-plan apply received %s", envSchemaFile)
			}
			// --json is on the command line, and a PTAH_JSON the Pod carries
			// never reaches the child to argue with it.
			if _, present := values["PTAH_JSON"]; present {
				t.Fatal("PTAH_JSON reached the apply child")
			}
		},
	}}}
	environment := append(databaseEnvironment("apply-1"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+wantedDigest,
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
		envSchemaFile+"=oci://registry.example/schema@sha256:"+strings.Repeat("c", 64),
		"PTAH_JSON=false",
	)
	result := Run(context.Background(), Config{
		Operation:   OperationApply,
		Environment: withRunnerProtocol(environment),
		Executor:    executor,
		TempDir:     t.TempDir(),
	})
	if result.Error != nil || result.PlanContentDigest != wantedDigest || !result.MutationStarted || result.Uncertain {
		t.Fatalf("Run() = %#v", result)
	}
	if tempPlanPath == "" {
		t.Fatal("apply command did not receive a temporary plan path")
	}
	if _, err := os.Stat(tempPlanPath); !os.IsNotExist(err) {
		t.Fatalf("temporary plan still exists or stat failed unexpectedly: %v", err)
	}
}

// The report of a successful Apply lists every statement it ran, and standard
// error repeats them. Neither reaches the frame or the runner's diagnostics.
func TestApplyNeverPublishesTheStatementsItsReportLists(t *testing.T) {
	t.Parallel()

	const sensitiveLiteral = "operator-private-schema-literal"
	plan := validPlanDocument("CREATE TABLE private_defaults (value text DEFAULT '" + sensitiveLiteral + "')")
	planDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	report := applyReport(t, dataplane.SchemaApplyOutcomeApplied, plan)
	if !strings.Contains(report, sensitiveLiteral) {
		t.Fatal("the report does not list the statement, so this row measures nothing")
	}
	diagnostics := &bytes.Buffer{}
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: report,
		stderr: "Planned schema changes:\nCREATE TABLE private_defaults (value text DEFAULT '" + sensitiveLiteral + "');\n",
	}}}
	environment := append(databaseEnvironment("apply-private-transcript"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+sha256Digest([]byte(plan)),
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
	)
	result := Run(context.Background(), Config{
		Operation: OperationApply, Environment: withRunnerProtocol(environment), Executor: executor, Diagnostics: diagnostics,
	})
	if result.Error != nil || result.Stdout != "" || !result.MutationStarted || result.Uncertain {
		t.Fatalf("Run() = %#v", result)
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	if bytes.Contains(frame, []byte(sensitiveLiteral)) || strings.Contains(diagnostics.String(), sensitiveLiteral) {
		t.Fatal("approved schema literal escaped the protected plan storage")
	}
}

func TestApplyNeverPublishesNativeFailureDiagnostics(t *testing.T) {
	t.Parallel()

	const sensitiveLiteral = "operator-private-schema-literal"
	failedStatement := "SQL: CREATE TABLE private_defaults (value text DEFAULT '" + sensitiveLiteral + "')"
	tests := []struct {
		name      string
		response  scriptedResponse
		errorCode string
	}{
		{
			name: "child stderr and no report",
			response: scriptedResponse{
				stderr:   "migration failed\n" + failedStatement + "\n",
				exitCode: 1,
			},
			errorCode: "invalid_apply_output",
		},
		{
			// The report's error is the sentence Ptah printed, and it may
			// quote the statement it stopped at.
			name: "a failure report quoting the statement",
			response: scriptedResponse{
				stdout: encodeApplyReport(t, dataplane.SchemaApplyReport{
					ContractVersion: 1, Outcome: dataplane.SchemaApplyOutcomeFailed,
					Error: "the planned changes cannot run inside a transaction\n" + failedStatement,
				}),
				exitCode: 1,
			},
			errorCode: "apply_failed",
		},
		{
			name: "an unknown outcome quoting the statement",
			response: scriptedResponse{
				stdout: encodeApplyReport(t, dataplane.SchemaApplyReport{
					ContractVersion: 1, Outcome: dataplane.SchemaApplyOutcomeUnknown,
					Statements: []string{failedStatement}, Error: "apply schema changes: " + failedStatement,
				}),
				exitCode: 1,
			},
			errorCode: "apply_outcome_unknown",
		},
		{
			name: "executor error",
			response: scriptedResponse{
				err: fmt.Errorf("executor failed while handling %s", sensitiveLiteral),
			},
			errorCode: "execution_error",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			plan := []byte(validPlanDocument("CREATE TABLE private_defaults (value text DEFAULT '" + sensitiveLiteral + "')"))
			planDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), plan, 0o600); err != nil {
				t.Fatal(err)
			}
			diagnostics := &bytes.Buffer{}
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{test.response}}
			environment := append(databaseEnvironment("apply-private-failure"),
				envPlanDir+"="+planDir,
				envExpectedPlanDigest+"="+sha256Digest(plan),
				envExpectedCoordination+"="+testCoordinationDigest(),
				envExpectedTargetDigest+"="+databaseTargetDigest(t),
			)
			result := Run(context.Background(), Config{
				Operation: OperationApply, Environment: withRunnerProtocol(environment), Executor: executor, Diagnostics: diagnostics,
			})
			if result.Error == nil || result.Error.Code != test.errorCode || result.Stdout != "" ||
				!result.MutationStarted || !result.Uncertain {
				t.Fatalf("Run() = %#v", result)
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			if bytes.Contains(frame, []byte(sensitiveLiteral)) || strings.Contains(diagnostics.String(), sensitiveLiteral) {
				t.Fatal("native Apply failure disclosed a protected schema literal")
			}
		})
	}
}

// The transcript a person reads is not a report. It is what this runner used to
// compare against, and a run that prints it on standard output is refused as
// output nothing can validate, and left uncertain.
func TestApplyRejectsTheHumanTranscriptAsUncertain(t *testing.T) {
	t.Parallel()

	plan := []byte(validPlanDocument("CREATE TABLE transcript_guard (id bigint)"))
	planDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: "Planned schema changes:\nCREATE TABLE transcript_guard (id bigint);\n" +
			"Auto-approval enabled; applying schema changes.\nSchema apply completed successfully.\n",
	}}}
	environment := append(databaseEnvironment("apply-transcript-mismatch"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+sha256Digest(plan),
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
	)
	result := Run(context.Background(), Config{Operation: OperationApply, Environment: withRunnerProtocol(environment), Executor: executor})
	if result.Error == nil || result.Error.Code != "invalid_apply_output" || result.Stdout != "" ||
		!result.MutationStarted || !result.Uncertain {
		t.Fatalf("Run() = %#v", result)
	}
}

func TestApplyRejectsDigestMismatchBeforeExecution(t *testing.T) {
	t.Parallel()

	planDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), []byte("plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &scriptedExecutor{t: t}
	environment := append(databaseEnvironment("apply-stale"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+sha256Digest([]byte("different plan")),
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
	)
	result := Run(context.Background(), Config{Operation: OperationApply, Environment: withRunnerProtocol(environment), Executor: executor})
	if result.Error == nil || result.Error.Code != "plan_digest_mismatch" {
		t.Fatalf("error = %#v, want plan_digest_mismatch", result.Error)
	}
	if len(executor.calls) != 0 {
		t.Fatalf("executed %d commands after digest mismatch", len(executor.calls))
	}
	if result.MutationStarted || result.Uncertain {
		t.Fatalf("mutation outcome = started %v, uncertain %v", result.MutationStarted, result.Uncertain)
	}
}

func TestApplyChecksTargetIdentityBeforeDispatch(t *testing.T) {
	t.Parallel()

	plannedURL := "postgres://app:old-password@db.example/app"
	plannedDigest, err := TargetIdentityDigest(plannedURL)
	if err != nil {
		t.Fatal(err)
	}
	planDir := t.TempDir()
	plan := []byte(validPlanDocument("CREATE TABLE target_binding (id bigint)"))
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), plan, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		database  string
		wantError bool
	}{
		{name: "password rotation", database: "postgres://app:new-password@db.example/app"},
		{name: "endpoint changed", database: "postgres://app:new-password@other.example/app", wantError: true},
		{name: "database changed", database: "postgres://app:new-password@db.example/other", wantError: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t}
			if !test.wantError {
				executor.responses = []scriptedResponse{{stdout: applyReport(t, dataplane.SchemaApplyOutcomeApplied, string(plan))}}
			}
			environment := []string{
				envOperationID + "=apply-target-binding",
				envDatabaseURL + "=" + test.database,
				envCoordinationDigest + "=" + testCoordinationDigest(),
				envExpectedCoordination + "=" + testCoordinationDigest(),
				envPlanDir + "=" + planDir,
				envExpectedPlanDigest + "=" + sha256Digest(plan),
				envExpectedTargetDigest + "=" + plannedDigest,
				envExpectedDatabaseEngine + "=PostgreSQL",
				envDispatchNotAfter + "=2099-01-01T00:00:00Z",
				envExecutionNotAfter + "=2099-01-01T00:00:00Z",
				envTerminationGracePeriod + "=30",
			}
			result := Run(context.Background(), Config{Operation: OperationApply, Environment: withRunnerProtocol(environment), Executor: executor})
			if test.wantError {
				if result.Error == nil || result.Error.Code != "target_binding_mismatch" {
					t.Fatalf("error = %#v, want target_binding_mismatch", result.Error)
				}
				if len(executor.calls) != 0 || result.MutationStarted || result.Uncertain {
					t.Fatalf("changed target dispatched mutation: calls=%d result=%#v", len(executor.calls), result)
				}
				return
			}
			if result.Error != nil || len(executor.calls) != 1 || !result.MutationStarted {
				t.Fatalf("password rotation result = %#v, calls=%d", result, len(executor.calls))
			}
		})
	}
}

func TestDatabaseOperationsRejectInvalidOrMismatchedCoordinationBinding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		operation    Operation
		coordination string
		expected     string
		wantCode     string
	}{
		{name: "missing", operation: OperationPlan, wantCode: "missing_coordination_binding"},
		{name: "malformed", operation: OperationPlan, coordination: "sha256:not-a-digest", wantCode: "invalid_coordination_binding"},
		{
			name: "apply mismatch", operation: OperationApply,
			coordination: testCoordinationDigest(), expected: "sha256:" + strings.Repeat("8", 64),
			wantCode: "coordination_binding_mismatch",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			environment := []string{
				envOperationID + "=coordination-check",
				envDatabaseURL + "=postgres://app:secret@db.example/app",
			}
			if test.coordination != "" {
				environment = append(environment, envCoordinationDigest+"="+test.coordination)
			}
			if test.expected != "" {
				environment = append(environment, envExpectedCoordination+"="+test.expected)
			}
			executor := &scriptedExecutor{t: t}
			result := Run(context.Background(), Config{Operation: test.operation, Environment: withRunnerProtocol(environment), Executor: executor})
			if result.Error == nil || result.Error.Code != test.wantCode {
				t.Fatalf("Run() error = %#v, want %s", result.Error, test.wantCode)
			}
			if len(executor.calls) != 0 || result.MutationStarted || result.Uncertain {
				t.Fatalf("invalid coordination binding dispatched work: calls=%d result=%#v", len(executor.calls), result)
			}
		})
	}
}

func TestReadOnlyOperationsRejectMySQLConnectionSQLBeforeDispatch(t *testing.T) {
	t.Parallel()

	attacks := []string{
		"app@tcp(db.example:3306)/accounts?multiStatements=true&sql_mode=%27%27%3BDROP%20TABLE%20victim",
		"app@tcp(db.example:3306)/accounts?sql_mode=side_effecting_function%28%29",
	}
	for _, operation := range []Operation{OperationObserve, OperationPlan} {
		operation := operation
		for index, databaseURL := range attacks {
			databaseURL := databaseURL
			t.Run(fmt.Sprintf("%s-%d", operation, index), func(t *testing.T) {
				t.Parallel()
				executor := &scriptedExecutor{t: t}
				result := Run(context.Background(), Config{
					Operation: operation,
					Environment: withRunnerProtocol([]string{
						envOperationID + "=mysql-connection-sql",
						envDatabaseURL + "=" + databaseURL,
						envCoordinationDigest + "=" + testCoordinationDigest(),
					}),
					Executor: executor,
				})
				if result.Error == nil || result.Error.Code != "invalid_target" || len(executor.calls) != 0 {
					t.Fatalf("Run() = %#v, calls = %d", result, len(executor.calls))
				}
				if strings.Contains(strings.ToUpper(result.Error.Message), "DROP TABLE") ||
					strings.Contains(result.Error.Message, "side_effecting_function") {
					t.Fatalf("error disclosed rejected connection SQL: %q", result.Error.Message)
				}
			})
		}
	}
}

func TestApplyRejectsLateOrMissingDispatchDeadlineBeforeExecution(t *testing.T) {
	t.Parallel()

	targetDigest := databaseTargetDigest(t)
	for _, test := range []struct {
		name     string
		deadline string
		code     string
	}{
		{name: "missing", code: "missing_dispatch_deadline"},
		{name: "expired", deadline: "2026-08-30T12:00:00Z", code: "dispatch_deadline_expired"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t}
			environment := []string{
				envOperationID + "=late-apply",
				envDatabaseURL + "=postgres://app:secret@db.example/app",
				envCoordinationDigest + "=" + testCoordinationDigest(),
				envExpectedCoordination + "=" + testCoordinationDigest(),
				envExpectedTargetDigest + "=" + targetDigest,
			}
			if test.deadline != "" {
				environment = append(environment, envDispatchNotAfter+"="+test.deadline)
			}
			result := Run(context.Background(), Config{
				Operation:   OperationApply,
				Environment: withRunnerProtocol(environment),
				Executor:    executor,
				Clock: func() time.Time {
					return time.Date(2026, 8, 30, 12, 0, 1, 0, time.UTC)
				},
			})
			if result.Error == nil || result.Error.Code != test.code {
				t.Fatalf("error = %#v, want %s", result.Error, test.code)
			}
			if len(executor.calls) != 0 || result.MutationStarted || result.Uncertain {
				t.Fatalf("expired dispatch executed mutation: calls=%d result=%#v", len(executor.calls), result)
			}
		})
	}
}

func TestApplyRechecksDispatchDeadlineImmediatelyBeforeExecution(t *testing.T) {
	t.Parallel()

	planDir := t.TempDir()
	plan := []byte(validPlanDocument("CREATE TABLE deadline_race (id bigint)"))
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Date(2026, 8, 30, 12, 0, 1, 0, time.UTC)
	times := []time.Time{
		deadline.Add(-time.Second),
		deadline,
	}
	clockCalls := 0
	executor := &scriptedExecutor{t: t}
	environment := []string{
		envOperationID + "=deadline-race",
		envDatabaseURL + "=postgres://app:secret@db.example/app",
		envCoordinationDigest + "=" + testCoordinationDigest(),
		envExpectedCoordination + "=" + testCoordinationDigest(),
		envExpectedTargetDigest + "=" + databaseTargetDigest(t),
		envExpectedDatabaseEngine + "=PostgreSQL",
		envPlanDir + "=" + planDir,
		envExpectedPlanDigest + "=" + sha256Digest(plan),
		envDispatchNotAfter + "=" + deadline.Format(time.RFC3339Nano),
		envExecutionNotAfter + "=" + deadline.Format(time.RFC3339Nano),
		envTerminationGracePeriod + "=30",
	}
	result := Run(context.Background(), Config{
		Operation:   OperationApply,
		Environment: withRunnerProtocol(environment),
		Executor:    executor,
		Clock: func() time.Time {
			if clockCalls >= len(times) {
				t.Fatalf("clock called more than %d times", len(times))
			}
			now := times[clockCalls]
			clockCalls++
			return now
		},
	})
	if result.Error == nil || result.Error.Code != "dispatch_deadline_expired" {
		t.Fatalf("error = %#v, want dispatch_deadline_expired", result.Error)
	}
	if len(executor.calls) != 0 || result.MutationStarted || result.Uncertain {
		t.Fatalf("expired dispatch executed mutation: calls=%d result=%#v", len(executor.calls), result)
	}
	if clockCalls != 2 {
		t.Fatalf("clock calls = %d, want 2", clockCalls)
	}
}

func TestApplyExecutionDeadlineCancelsAStartedChild(t *testing.T) {
	t.Parallel()

	planDir := t.TempDir()
	plan := []byte(validPlanDocument("CREATE TABLE deadline_guard (id bigint)"))
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Add(250 * time.Millisecond)
	executor := &contextDeadlineExecutor{}
	environment := append(
		databaseEnvironment("execution-deadline"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+sha256Digest(plan),
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
		envDispatchNotAfter+"="+deadline.Format(time.RFC3339Nano),
		envExecutionNotAfter+"="+deadline.Format(time.RFC3339Nano),
	)
	started := time.Now()
	result := Run(context.Background(), Config{Operation: OperationApply, Environment: withRunnerProtocol(environment), Executor: executor})
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("deadline cancellation took %s", elapsed)
	}
	if executor.calls != 1 || result.Error == nil || !result.MutationStarted || !result.Uncertain {
		t.Fatalf("Run() = %#v, calls = %d", result, executor.calls)
	}
}

func TestApplyFailureIsUncertainAndMustNotBeReplayed(t *testing.T) {
	t.Parallel()

	planDir := t.TempDir()
	plan := []byte(validPlanDocument("CREATE TABLE uncertain_apply (id bigint)"))
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: encodeApplyReport(t, dataplane.SchemaApplyReport{
			ContractVersion: 1, Outcome: dataplane.SchemaApplyOutcomeUnknown,
			PlanDigest: sha256Digest(plan), Error: "connection lost",
		}),
		stderr:   "connection lost",
		exitCode: 1,
	}}}
	environment := append(databaseEnvironment("apply-uncertain"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+sha256Digest(plan),
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
	)
	result := Run(context.Background(), Config{Operation: OperationApply, Environment: withRunnerProtocol(environment), Executor: executor})
	if result.Error == nil || result.Error.Code != "apply_outcome_unknown" || !result.MutationStarted || !result.Uncertain {
		t.Fatalf("Run() = %#v, want an uncertain dispatched mutation", result)
	}
}

// A stale plan is refused before any statement is sent, and the report names
// what moved: the structure, or the declared rows, which the transcript this
// runner used to parse never named at all. The frame is still uncertain,
// because another Pod of the same Job may have run.
func TestApplyNativeStalePlanAfterDispatchStillHasUnknownOutcome(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		changed string
		message string
	}{
		{changed: "schema", message: "database schema no longer matches"},
		{changed: "rows", message: "declared rows no longer match"},
	} {
		t.Run(test.changed, func(t *testing.T) {
			t.Parallel()
			planDir := t.TempDir()
			plan := []byte(validPlanDocument("SELECT 1"))
			if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), plan, 0o600); err != nil {
				t.Fatal(err)
			}
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
				stdout: encodeApplyReport(t, dataplane.SchemaApplyReport{
					ContractVersion: 1,
					Outcome:         dataplane.SchemaApplyOutcomeRefused,
					PlanName:        "operator-plan",
					PlanDigest:      sha256Digest(plan),
					Refusal: &dataplane.SchemaRefusal{
						Code:                dataplane.SchemaRefusalStalePlan,
						Changed:             test.changed,
						PlanFingerprint:     "sha256:" + strings.Repeat("a", 64),
						DatabaseFingerprint: "sha256:" + strings.Repeat("c", 64),
					},
					Error: "pre-planned migration is stale",
				}),
				exitCode: 2,
			}}}
			environment := append(databaseEnvironment("apply-native-stale"),
				envPlanDir+"="+planDir,
				envExpectedPlanDigest+"="+sha256Digest(plan),
				envExpectedCoordination+"="+testCoordinationDigest(),
				envExpectedTargetDigest+"="+databaseTargetDigest(t),
			)
			result := Run(context.Background(), Config{Operation: OperationApply, Environment: withRunnerProtocol(environment), Executor: executor})
			if result.Error == nil || result.Error.Code != "stale_plan" || !result.MutationStarted || !result.Uncertain ||
				result.ChildExitCode != 2 {
				t.Fatalf("Run() = %#v", result)
			}
			if !strings.Contains(result.Error.Message, test.message) {
				t.Fatalf("stale message = %q, want it to say %q", result.Error.Message, test.message)
			}
		})
	}
}

// The report decides how an Apply ended, on every exit status, and only a
// report of applied that names the approved plan is a success. Every other
// ending is an error, and every Apply error is uncertain: the child's claim
// that nothing was sent speaks for one Pod, and a Job may have run another.
func TestApplyReadsItsReportOnEveryExit(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE report_rows (id bigint)")
	approved := sha256Digest([]byte(plan))
	report := func(edit func(*dataplane.SchemaApplyReport)) string {
		value := dataplane.SchemaApplyReport{
			ContractVersion: 1,
			Outcome:         dataplane.SchemaApplyOutcomeApplied,
			PlanName:        "operator-plan",
			PlanDigest:      approved,
			Statements:      []string{"CREATE TABLE report_rows (id bigint)"},
		}
		edit(&value)
		return encodeApplyReport(t, value)
	}
	refusal := func(code string) string {
		return report(func(value *dataplane.SchemaApplyReport) {
			value.Outcome = dataplane.SchemaApplyOutcomeRefused
			value.Statements = nil
			value.Refusal = &dataplane.SchemaRefusal{Code: code}
			value.Error = "refused"
		})
	}
	outcome := func(outcome string) string {
		return report(func(value *dataplane.SchemaApplyReport) { value.Outcome = outcome })
	}
	for _, test := range []struct {
		name     string
		stdout   string
		exitCode int
		wantCode string
		wantText string
	}{
		{name: "applied", stdout: outcome(dataplane.SchemaApplyOutcomeApplied)},
		// A SIGTERM between the commit and the exit returns 143 with the report
		// already written. The report says what happened.
		{name: "applied, then terminated", stdout: outcome(dataplane.SchemaApplyOutcomeApplied), exitCode: 143},
		{
			name: "applied, naming another plan", wantCode: "invalid_apply_output",
			stdout: report(func(value *dataplane.SchemaApplyReport) { value.PlanDigest = sha256Digest([]byte("another plan")) }),
		},
		{
			name: "applied, naming no plan", wantCode: "invalid_apply_output",
			stdout: report(func(value *dataplane.SchemaApplyReport) { value.PlanDigest = "" }),
		},
		{name: "a stale plan", stdout: refusal(dataplane.SchemaRefusalStalePlan), exitCode: 2, wantCode: "stale_plan"},
		{name: "a lock timeout", stdout: refusal("lock-timeout"), exitCode: 2, wantCode: "apply_refused", wantText: "lock-timeout"},
		{
			name: "a refusal code this runner does not know", stdout: refusal("quota-exceeded"), exitCode: 2,
			wantCode: "apply_refused", wantText: "quota-exceeded",
		},
		{name: "a failure", stdout: outcome(dataplane.SchemaApplyOutcomeFailed), exitCode: 1, wantCode: "apply_failed"},
		{name: "an unknown outcome", stdout: outcome(dataplane.SchemaApplyOutcomeUnknown), exitCode: 1, wantCode: "apply_outcome_unknown"},
		{name: "a dry run", stdout: outcome(dataplane.SchemaApplyOutcomeDryRun), wantCode: "invalid_apply_output"},
		{name: "a canceled prompt", stdout: outcome(dataplane.SchemaApplyOutcomeCanceled), wantCode: "invalid_apply_output"},
		{name: "no changes", stdout: outcome(dataplane.SchemaApplyOutcomeNoChanges), wantCode: "invalid_apply_output"},
		{
			name: "a newer contract version", wantCode: "invalid_apply_output",
			stdout: report(func(value *dataplane.SchemaApplyReport) { value.ContractVersion = 2 }),
		},
		{name: "an outcome nobody declared", stdout: outcome("partial"), wantCode: "invalid_apply_output"},
		{
			name: "a second JSON value", wantCode: "invalid_apply_output",
			stdout: outcome(dataplane.SchemaApplyOutcomeApplied) + outcome(dataplane.SchemaApplyOutcomeApplied),
		},
		{name: "text that is not JSON", stdout: "Schema apply completed successfully.\n", wantCode: "invalid_apply_output"},
		// A build without --json, a flag error and a panic all leave standard
		// output empty, and a panic may follow the statements. It is an outcome
		// nobody knows, never a run that changed nothing.
		{name: "no report at all", exitCode: 2, wantCode: "invalid_apply_output", wantText: "printed no report"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			planDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), []byte(plan), 0o600); err != nil {
				t.Fatal(err)
			}
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
				stdout: test.stdout, stderr: "Planned schema changes:\n", exitCode: test.exitCode,
			}}}
			environment := append(databaseEnvironment("apply-report"),
				envPlanDir+"="+planDir,
				envExpectedPlanDigest+"="+approved,
				envExpectedCoordination+"="+testCoordinationDigest(),
				envExpectedTargetDigest+"="+databaseTargetDigest(t),
			)
			result := Run(context.Background(), Config{Operation: OperationApply, Environment: withRunnerProtocol(environment), Executor: executor})
			if _, err := MarshalFrame(result); err != nil {
				t.Fatalf("MarshalFrame() error = %v for %#v", err, result)
			}
			if test.wantCode == "" {
				if result.Error != nil || !result.MutationStarted || result.Uncertain || result.ChildExitCode != 0 {
					t.Fatalf("Run() = %#v, want an exact completed mutation", result)
				}
				return
			}
			if result.Error == nil || result.Error.Code != test.wantCode || !result.MutationStarted || !result.Uncertain {
				t.Fatalf("Run() = %#v, want %s and an uncertain mutation", result, test.wantCode)
			}
			if !strings.Contains(result.Error.Message, test.wantText) {
				t.Fatalf("message = %q, want it to name %q", result.Error.Message, test.wantText)
			}
		})
	}
}

func TestPlanPassesEveryExactExclusionAsASeparateArgument(t *testing.T) {
	t.Parallel()

	plan := strings.Replace(
		validPlanDocument("CREATE TABLE example (id bigint);"),
		`"destructive":false`,
		`"exclude":["audit.*","legacy,archive"],"destructive":false`,
		1,
	)
	inspectPlan := func(spec CommandSpec) {
		outputPath := argumentAfter(spec, "--output")
		want := []string{"schema", "plan", "--output", outputPath, "--json", "--exclude=audit.*", "--exclude=legacy,archive"}
		if !reflect.DeepEqual(spec.Args, want) || !filepath.IsAbs(outputPath) {
			t.Fatalf("plan args = %q, want %q with an absolute output path", spec.Args, want)
		}
		values := environmentMap(spec.Env)
		if _, present := values["PTAH_EXCLUDE"]; present {
			t.Fatal("encoded exclusion adapter leaked to the Ptah child")
		}
		if _, present := values["PTAH_JSON"]; present {
			t.Fatal("PTAH_JSON reached a plan read")
		}
	}
	responses := stablePlanResponses(t, plan)
	responses[0].inspect = inspectPlan
	responses[1].inspect = inspectPlan
	responses[2].inspect = func(spec CommandSpec) {
		want := []string{"schema", "apply", "--plan", spec.Args[3], "--auto-approve", "--json", "--dry-run"}
		if !reflect.DeepEqual(spec.Args, want) {
			t.Fatalf("plan validation args = %q, want %q", spec.Args, want)
		}
		values := environmentMap(spec.Env)
		for _, key := range []string{envSchemaFile, "PTAH_DEV_URL", "PTAH_EXCLUDE", "PTAH_JSON"} {
			if _, present := values[key]; present {
				t.Fatalf("plan-only environment key %s reached native plan validation", key)
			}
		}
	}
	executor := &scriptedExecutor{t: t, responses: responses}
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: withRunnerProtocol(append(databaseEnvironment("plan-scope"), "PTAH_EXCLUDE=\"legacy,archive\",audit.*", "PTAH_JSON=false")),
		Executor:    executor,
	})
	if result.Error != nil || result.PlanOutcome != PlanOutcomeChanges {
		t.Fatalf("Run() = %#v", result)
	}
	if got := argumentAfter(executor.calls[2], "--plan"); got != argumentAfter(executor.calls[0], "--output") {
		t.Fatalf("plan validation read %s, want the first read's saved plan %s", got, argumentAfter(executor.calls[0], "--output"))
	}
}

// The fence reaches the plan as one argument per table, and reaches nothing
// else: an Apply executes a plan that was already refused or approved.
func TestPlanPassesEveryProtectedTableAsASeparateArgument(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE example (id bigint);")
	inspectPlan := func(spec CommandSpec) {
		want := []string{
			"schema", "plan", "--output", argumentAfter(spec, "--output"), "--json",
			"--protected-table=countries", "--protected-table=ref.regions",
		}
		if !reflect.DeepEqual(spec.Args, want) {
			t.Fatalf("plan args = %q, want %q", spec.Args, want)
		}
		if _, present := environmentMap(spec.Env)["PTAH_PROTECTED_TABLES"]; present {
			t.Fatal("the encoded fence adapter leaked to the Ptah child")
		}
	}
	responses := stablePlanResponses(t, plan)
	responses[0].inspect = inspectPlan
	responses[1].inspect = inspectPlan
	responses[2].inspect = func(spec CommandSpec) {
		if _, present := environmentMap(spec.Env)["PTAH_PROTECTED_TABLES"]; present {
			t.Fatal("the fence reached native plan validation, which applies a plan rather than computing one")
		}
		for _, argument := range spec.Args {
			if strings.HasPrefix(argument, "--protected-table") {
				t.Fatalf("plan validation args = %q, which asks the fence a second time", spec.Args)
			}
		}
	}
	executor := &scriptedExecutor{t: t, responses: responses}
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: withRunnerProtocol(append(databaseEnvironment("plan-fence"), "PTAH_PROTECTED_TABLES=ref.regions,countries")),
		Executor:    executor,
	})
	if result.Error != nil || result.PlanOutcome != PlanOutcomeChanges {
		t.Fatalf("Run() = %#v", result)
	}
}

// A plan Ptah refused because of the fence is reported under its own code, with
// the report's own sentence. Every other ending keeps a code of its own, so a
// reader cannot take a fault for a refusal or the other way round -- and the
// decision comes from the report alone, never from what standard error says.
func TestPlanReportsAFencedRefusalAsItsOwnOutcome(t *testing.T) {
	t.Parallel()

	const fenceSentence = "refusing to change protected table(s) countries, ref.regions: a protected table is fenced " +
		"off from the declarative path, which has no override; drop the entry to plan the change"
	for _, test := range []struct {
		name     string
		response scriptedResponse
		wantCode string
		wantText string
	}{
		{
			name: "the fence refused the plan",
			response: scriptedResponse{
				stdout: `{"contract_version":1,"outcome":"refused","refusal":{"code":"protected-table",` +
					`"tables":["countries","ref.regions"]},"error":"` + fenceSentence + `"}`,
				stderr:   "error: " + fenceSentence + "\n",
				exitCode: 2,
			},
			wantCode: "protected_table",
			wantText: "refusing to change protected table(s) countries, ref.regions",
		},
		{
			name: "the fence refused the plan and gave no sentence",
			response: scriptedResponse{
				stdout: `{"contract_version":1,"outcome":"refused","refusal":{"code":"protected-table",` +
					`"tables":["countries","ref.regions"]}}`,
				exitCode: 2,
			},
			wantCode: "protected_table",
			wantText: "countries, ref.regions",
		},
		{
			name: "a refusal of a kind this runner does not know",
			response: scriptedResponse{
				stdout:   `{"contract_version":1,"outcome":"refused","refusal":{"code":"quota-exceeded"},"error":"quota"}`,
				exitCode: 2,
			},
			wantCode: "plan_refused",
			wantText: "quota-exceeded",
		},
		{
			name: "the plan failed while standard error reads like a fence",
			response: scriptedResponse{
				stdout:   `{"contract_version":1,"outcome":"failed","error":"connect to --db-url: dial tcp: connection refused"}`,
				stderr:   "error: " + fenceSentence + "\n",
				exitCode: 1,
			},
			wantCode: "invalid_plan_output",
		},
		{
			name: "an executor that does not accept --json",
			response: scriptedResponse{
				stderr:   "error: unknown flag: --json\n",
				exitCode: 2,
			},
			wantCode: "invalid_plan_output",
			wantText: "does not accept --json",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{test.response}}
			result := Run(context.Background(), Config{
				Operation:   OperationPlan,
				Environment: withRunnerProtocol(append(databaseEnvironment("plan-refused"), "PTAH_PROTECTED_TABLES=countries")),
				Executor:    executor,
			})
			if result.Error == nil || result.Error.Code != test.wantCode {
				t.Fatalf("Run() = %#v, want code %s", result, test.wantCode)
			}
			if !strings.Contains(result.Error.Message, test.wantText) {
				t.Fatalf("message = %q, want it to name %q", result.Error.Message, test.wantText)
			}
			if result.Stdout != "" || len(executor.calls) != 1 {
				t.Fatalf("a refused plan carries executable content or read again: %#v, calls %d", result, len(executor.calls))
			}
		})
	}
}

// A fence value that is not an identifier is an input error rather than a
// fence: the runner is the process that hands it to a command line.
func TestPlanRefusesAFenceThatIsNotATableName(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"countries; DROP TABLE users", "--allow-prod", "ref.regions.extra", "ref."} {
		executor := &scriptedExecutor{t: t, responses: nil}
		result := Run(context.Background(), Config{
			Operation:   OperationPlan,
			Environment: withRunnerProtocol(append(databaseEnvironment("plan-bad-fence"), "PTAH_PROTECTED_TABLES="+value)),
			Executor:    executor,
		})
		if result.Error == nil || result.Error.Code != "invalid_input" {
			t.Fatalf("Run() with fence %q = %#v, want invalid_input", value, result)
		}
	}
}

func TestPlanRejectsOutputThatDoesNotBindRequestedScope(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("SELECT 1")
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{planResponse(plan), planResponse(plan)}}
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: withRunnerProtocol(append(databaseEnvironment("plan-wrong-scope"), "PTAH_EXCLUDE=audit.*")),
		Executor:    executor,
	})
	if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.Stdout != "" {
		t.Fatalf("Run() = %#v", result)
	}
}

// A synced database is the no-changes outcome of the report, and nothing else:
// the sentence Ptah prints for a person is not it, and neither is a report that
// says no-changes while naming a saved plan.
func TestPlanNoChangesComesFromTheReport(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		responses []scriptedResponse
		valid     bool
	}{
		{name: "the report", responses: []scriptedResponse{planNoChangesResponse(), planNoChangesResponse()}, valid: true},
		{
			name: "the sentence on standard output",
			responses: []scriptedResponse{
				{stdout: "Schema is synced, no changes to be made.\n"},
			},
		},
		{
			name: "a report naming a saved plan",
			responses: []scriptedResponse{
				{stdout: `{"contract_version":1,"outcome":"no-changes","plan_path":"/tmp/plan.json"}`},
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t, responses: test.responses}
			result := Run(context.Background(), Config{
				Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("plan-no-change")), Executor: executor,
			})
			if test.valid {
				if result.Error != nil || result.PlanOutcome != PlanOutcomeNoChanges || result.Stdout != "" ||
					result.PlanContentDigest != "" || len(executor.calls) != 2 {
					t.Fatalf("Run() = %#v, calls = %d", result, len(executor.calls))
				}
				return
			}
			if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.PlanOutcome != "" {
				t.Fatalf("Run() = %#v", result)
			}
		})
	}
}

// Each row is a plan read whose report must be refused before anything reads
// the plan it names. The rows that publish a plan are the other half.
func TestPlanRefusesAReportItCannotAccountFor(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE example (id bigint);")
	respond := func(stdoutFor func(outputPath string) string) scriptedResponse {
		return scriptedResponse{
			savePlan:  plan,
			stdoutFor: func(spec CommandSpec) string { return stdoutFor(argumentAfter(spec, "--output")) },
		}
	}
	for _, test := range []struct {
		name     string
		response scriptedResponse
	}{
		{name: "a newer contract version", response: respond(func(outputPath string) string {
			return strings.Replace(planReport(outputPath, plan), `"contract_version":1`, `"contract_version":2`, 1)
		})},
		{name: "an outcome nobody declared", response: respond(func(outputPath string) string {
			return strings.Replace(planReport(outputPath, plan), `"outcome":"changes"`, `"outcome":"partial"`, 1)
		})},
		{name: "a digest of another plan", response: respond(func(outputPath string) string {
			return planReport(outputPath, validPlanDocument("CREATE TABLE other (id bigint);"))
		})},
		{name: "a plan saved somewhere else", response: respond(func(outputPath string) string {
			return planReport(outputPath+".elsewhere", plan)
		})},
		{name: "a second JSON value", response: respond(func(outputPath string) string {
			return planReport(outputPath, plan) + planNoChangesReport
		})},
		{name: "the plan document instead of a report", response: respond(func(string) string { return plan })},
		{name: "a line for a person before the report", response: respond(func(outputPath string) string {
			return "Planned schema changes:\nCREATE TABLE example (id bigint);\n" + planReport(outputPath, plan)
		})},
		{name: "a report of a plan that was never saved", response: scriptedResponse{
			stdoutFor: func(spec CommandSpec) string { return planReport(argumentAfter(spec, "--output"), plan) },
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{test.response}}
			result := Run(context.Background(), Config{
				Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("plan-bad-report")), Executor: executor,
			})
			if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.Stdout != "" ||
				result.PlanContentDigest != "" || len(executor.calls) != 1 {
				t.Fatalf("Run() = %#v, calls = %d", result, len(executor.calls))
			}
		})
	}
}

// A plan read that saves and reports its plan and still exits nonzero -- a
// SIGTERM after the report was written -- is read by its report. The frame
// carries the success with exit code zero, which is the only exit code the
// protocol accepts beside one.
func TestPlanReadsItsReportWhateverTheExitStatus(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE example (id bigint);")
	responses := stablePlanResponses(t, plan)
	for index := range responses {
		responses[index].exitCode = 143
	}
	var diagnostics bytes.Buffer
	executor := &scriptedExecutor{t: t, responses: responses}
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: withRunnerProtocol(databaseEnvironment("plan-terminated")),
		Executor: executor, Diagnostics: &diagnostics,
	})
	if result.Error != nil || result.PlanOutcome != PlanOutcomeChanges || result.ChildExitCode != 0 {
		t.Fatalf("Run() = %#v", result)
	}
	if _, err := MarshalFrame(result); err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	if !strings.Contains(diagnostics.String(), "exited with code 143") {
		t.Fatalf("diagnostics = %q, want the exit status written down", diagnostics.String())
	}
}

func TestObserveRejectsAReportWithoutSameReadDiffIdentity(t *testing.T) {
	t.Parallel()

	for _, report := range []string{
		`{"drift":false,"failed":false,"failure_threshold":"all","highest_severity":"","dialect":"postgres","findings":[]}`,
		`{"drift":false,"failed":false,"failure_threshold":"all","highest_severity":"","dialect":"mysql","findings":[],"diff":{}}`,
	} {
		executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: report}}}
		result := Run(context.Background(), Config{
			Operation: OperationObserve, Environment: withRunnerProtocol(databaseEnvironment("observe-invalid-report")), Executor: executor,
		})
		if result.Error == nil || result.Error.Code != "invalid_observed_state" || result.DriftReportDigest != "" {
			t.Fatalf("Run() = %#v", result)
		}
	}
}

func TestVerifyUsesResolvedDigestForMutableReferenceWhenPolicyPermitsTags(t *testing.T) {
	t.Parallel()

	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	policy := []byte("version: 1\nartifact_types:\n  - application/vnd.stokaro.ptah.schema.v1\n")
	if err := os.WriteFile(policyPath, policy, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	requested := "oci://registry.example/team/schema:main"
	resolved := "oci://registry.example/team/schema@" + digest
	artifactType := "application/vnd.stokaro.ptah.schema.v1"
	var snapshottedPolicyPath string
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{
		{stdout: fmt.Sprintf(`{"reference":%q,"digest":%q,"satisfied":["artifact_types"],"findings":[]}`, resolved, digest), inspect: func(spec CommandSpec) {
			snapshottedPolicyPath = spec.Args[4]
			gotPolicy, err := os.ReadFile(snapshottedPolicyPath)
			if err != nil || !bytes.Equal(gotPolicy, policy) {
				t.Fatalf("snapshotted policy = %q, %v", gotPolicy, err)
			}
		}},
		{stdout: inspectReport(resolved, digest, artifactType)},
	}}
	result := Run(context.Background(), Config{
		Operation: OperationVerify,
		Environment: withRunnerProtocol([]string{
			envOperationID + "=verify-1",
			envRequestedReference + "=" + requested,
			envResolvedReference + "=" + resolved,
			envVerificationPolicy + "=" + policyPath,
			envExpectedArtifactType + "=" + artifactType,
		}),
		Executor: executor,
	})
	if result.Error != nil || result.ResolvedDigest != digest || result.ObservedArtifactType != artifactType || result.VerificationPolicyDigest != sha256Digest(policy) {
		t.Fatalf("Run() = %#v", result)
	}
	if len(executor.calls) != 2 {
		t.Fatalf("commands = %d, want 2", len(executor.calls))
	}
	if executor.calls[0].Args[2] != resolved {
		t.Fatalf("verify used %q, want immutable resolved reference %q", executor.calls[0].Args[2], resolved)
	}
	if executor.calls[1].Args[2] != resolved || executor.calls[1].Args[len(executor.calls[1].Args)-1] != "--no-referrers" {
		t.Fatalf("inspect args = %v", executor.calls[1].Args)
	}
	for index, call := range executor.calls {
		childValues := environmentMap(call.Env)
		if _, found := childValues[EnvRequestedReference]; found {
			t.Fatalf("command %d received the runner-only requested reference", index+1)
		}
		if _, found := childValues[EnvResolvedReference]; found {
			t.Fatalf("command %d received the runner-only resolved reference", index+1)
		}
	}
	if snapshottedPolicyPath == "" || snapshottedPolicyPath == policyPath {
		t.Fatalf("verify policy path = %q, want an immutable snapshot", snapshottedPolicyPath)
	}
	if _, err := os.Stat(snapshottedPolicyPath); !os.IsNotExist(err) {
		t.Fatalf("snapshotted policy still exists or stat failed unexpectedly: %v", err)
	}
}

func TestVerifyAppliesDigestPinPolicyToOriginalRequestedReference(t *testing.T) {
	t.Parallel()

	policy := []byte("version: 1\nrequire_digest_pin: true\n")
	digest := "sha256:" + strings.Repeat("a", 64)
	resolved := "oci://registry.example/team/schema@" + digest
	artifactType := dataplane.SchemaArtifactType
	report := fmt.Sprintf(
		`{"reference":%q,"digest":%q,"satisfied":["require_digest_pin"],"findings":[]}`,
		resolved, digest,
	)

	for name, requested := range map[string]string{
		"explicit tag":    "oci://registry.example/team/schema:stable",
		"implicit latest": "oci://registry.example/team/schema",
	} {
		name, requested := name, requested
		t.Run(name+" refused after immutable native verification", func(t *testing.T) {
			t.Parallel()
			policyPath := filepath.Join(t.TempDir(), "policy.yaml")
			if err := os.WriteFile(policyPath, policy, 0o600); err != nil {
				t.Fatal(err)
			}
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: report}}}
			result := Run(context.Background(), Config{
				Operation: OperationVerify,
				Environment: withRunnerProtocol([]string{
					envOperationID + "=verify-tag-pin-policy",
					envRequestedReference + "=" + requested,
					envResolvedReference + "=" + resolved,
					envVerificationPolicy + "=" + policyPath,
					envExpectedArtifactType + "=" + artifactType,
				}),
				Executor: executor,
			})
			if result.Error == nil || result.Error.Code != "verification_refused" || result.ChildExitCode != 0 ||
				result.ResolvedDigest != digest || result.VerificationPolicyDigest != sha256Digest(policy) ||
				!reflect.DeepEqual(result.VerificationRequirements, []string{"require_digest_pin"}) ||
				result.ObservedArtifactType != "" || result.Stdout != "" {
				t.Fatalf("Run() = %#v", result)
			}
			if len(executor.calls) != 1 || executor.calls[0].Args[2] != resolved {
				t.Fatalf("verify commands = %#v, want one immutable verify", executor.calls)
			}
		})
	}

	t.Run("digest request accepted", func(t *testing.T) {
		policyPath := filepath.Join(t.TempDir(), "policy.yaml")
		if err := os.WriteFile(policyPath, policy, 0o600); err != nil {
			t.Fatal(err)
		}
		executor := &scriptedExecutor{t: t, responses: []scriptedResponse{
			{stdout: report},
			{stdout: inspectReport(resolved, digest, artifactType)},
		}}
		result := Run(context.Background(), Config{
			Operation: OperationVerify,
			Environment: withRunnerProtocol([]string{
				envOperationID + "=verify-digest-pin-policy",
				envRequestedReference + "=" + resolved,
				envResolvedReference + "=" + resolved,
				envVerificationPolicy + "=" + policyPath,
				envExpectedArtifactType + "=" + artifactType,
			}),
			Executor: executor,
		})
		if result.Error != nil || result.ChildExitCode != 0 || result.ResolvedDigest != digest ||
			result.ObservedArtifactType != artifactType || len(executor.calls) != 2 {
			t.Fatalf("Run() = %#v, commands = %d", result, len(executor.calls))
		}
	})
}

func TestVerifyRejectsInvalidRequestedBindingBeforeChild(t *testing.T) {
	t.Parallel()

	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(policyPath, []byte("version: 1\nrequire_digest_pin: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	resolved := "oci://registry.example/team/schema@" + digest
	for _, test := range []struct {
		name      string
		requested string
		wantCode  string
	}{
		{name: "missing", wantCode: "invalid_oci_access"},
		{name: "not OCI", requested: "registry.example/team/schema:stable", wantCode: "invalid_oci_access"},
		{name: "credentials", requested: "oci://user:password@registry.example/team/schema:stable", wantCode: "invalid_oci_access"},
		{name: "different repository", requested: "oci://registry.example/other/schema:stable", wantCode: "invalid_input"},
		{name: "different digest", requested: "oci://registry.example/team/schema@sha256:" + strings.Repeat("b", 64), wantCode: "invalid_input"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t}
			result := Run(context.Background(), Config{
				Operation: OperationVerify,
				Environment: withRunnerProtocol([]string{
					envOperationID + "=verify-invalid-requested-binding",
					envRequestedReference + "=" + test.requested,
					envResolvedReference + "=" + resolved,
					envVerificationPolicy + "=" + policyPath,
					envExpectedArtifactType + "=" + dataplane.SchemaArtifactType,
				}),
				Executor: executor,
			})
			if result.Error == nil || result.Error.Code != test.wantCode || result.ChildExitCode != -1 || len(executor.calls) != 0 ||
				result.ResolvedDigest != "" || result.VerificationPolicyDigest != "" {
				t.Fatalf("Run() = %#v, error = %#v, commands = %d", result, result.Error, len(executor.calls))
			}
			if test.requested != "" && strings.Contains(result.Error.Message, test.requested) {
				t.Fatalf("invalid binding error disclosed the requested reference: %#v", result.Error)
			}
		})
	}
}

func TestVerifyFailsClosedOnMovedSourceAndArtifactTypeMismatch(t *testing.T) {
	t.Parallel()

	policyPath := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policyPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	baseEnvironment := []string{
		envOperationID + "=verify-stale",
		envRequestedReference + "=oci://registry.example/schema:latest",
		envResolvedReference + "=oci://registry.example/schema@" + digestA,
		envVerificationPolicy + "=" + policyPath,
		envExpectedArtifactType + "=application/vnd.stokaro.ptah.schema.v1",
	}

	t.Run("moved source", func(t *testing.T) {
		executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: fmt.Sprintf(
			`{"reference":%q,"digest":%q,"satisfied":[],"findings":[]}`,
			"oci://registry.example/schema@"+digestB, digestB,
		)}}}
		result := Run(context.Background(), Config{Operation: OperationVerify, Environment: withRunnerProtocol(baseEnvironment), Executor: executor})
		if result.Error == nil || result.Error.Code != "stale_source" || len(executor.calls) != 1 {
			t.Fatalf("Run() = %#v, commands = %d", result, len(executor.calls))
		}
	})

	t.Run("wrong artifact type", func(t *testing.T) {
		executor := &scriptedExecutor{t: t, responses: []scriptedResponse{
			{stdout: fmt.Sprintf(`{"reference":%q,"digest":%q,"satisfied":[],"findings":[]}`, "oci://registry.example/schema@"+digestA, digestA)},
			{stdout: inspectReport("oci://registry.example/schema@"+digestA, digestA, "application/octet-stream")},
		}}
		result := Run(context.Background(), Config{Operation: OperationVerify, Environment: withRunnerProtocol(baseEnvironment), Executor: executor})
		if result.Error == nil || result.Error.Code != "artifact_type_mismatch" || len(executor.calls) != 2 {
			t.Fatalf("Run() = %#v, commands = %d", result, len(executor.calls))
		}
	})
}

func TestVerifyModelsPolicyRefusalAsTypedNonRetryableEvidence(t *testing.T) {
	t.Parallel()

	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(policyPath, []byte("version: 1\nrequire_signature: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	resolved := "oci://registry.example/schema@" + digest
	report := fmt.Sprintf(
		`{"reference":%q,"digest":%q,"satisfied":["require_digest_pin"],"findings":[{"requirement":"require_signature","detail":"no signature is attached"}]}`,
		resolved, digest,
	)
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: report, exitCode: 2}}}
	result := Run(context.Background(), Config{
		Operation: OperationVerify,
		Environment: withRunnerProtocol([]string{
			envOperationID + "=verify-refused",
			envRequestedReference + "=" + resolved,
			envResolvedReference + "=" + resolved,
			envVerificationPolicy + "=" + policyPath,
			envExpectedArtifactType + "=" + dataplane.SchemaArtifactType,
		}),
		Executor: executor,
	})
	if result.Error == nil || result.Error.Code != "verification_refused" || result.Uncertain {
		t.Fatalf("Run() = %#v, want typed verification_refused evidence", result)
	}
	if result.Stdout != "" || result.ChildExitCode != 2 || result.ResolvedDigest != digest ||
		!reflect.DeepEqual(result.VerificationRequirements, []string{"require_signature"}) || len(executor.calls) != 1 {
		t.Fatalf("refusal evidence = %#v, calls=%d", result, len(executor.calls))
	}
}

func TestVerifyUnionsRequestedDigestPinRefusalWithNativeFindings(t *testing.T) {
	t.Parallel()

	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	policy := []byte("version: 1\nrequire_digest_pin: true\nrequire_signature: true\n")
	if err := os.WriteFile(policyPath, policy, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	resolved := "oci://registry.example/schema@" + digest
	report := fmt.Sprintf(
		`{"reference":%q,"digest":%q,"satisfied":["require_digest_pin"],"findings":[{"requirement":"require_signature","detail":"no signature is attached"}]}`,
		resolved, digest,
	)
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: report, exitCode: 2}}}
	result := Run(context.Background(), Config{
		Operation: OperationVerify,
		Environment: withRunnerProtocol([]string{
			envOperationID + "=verify-combined-refusal",
			envRequestedReference + "=oci://registry.example/schema:stable",
			envResolvedReference + "=" + resolved,
			envVerificationPolicy + "=" + policyPath,
			envExpectedArtifactType + "=" + dataplane.SchemaArtifactType,
		}),
		Executor: executor,
	})
	if result.Error == nil || result.Error.Code != "verification_refused" || result.ChildExitCode != 2 ||
		result.ResolvedDigest != digest || result.VerificationPolicyDigest != sha256Digest(policy) ||
		!reflect.DeepEqual(result.VerificationRequirements, []string{"require_digest_pin", "require_signature"}) ||
		result.ObservedArtifactType != "" || result.Stdout != "" || len(executor.calls) != 1 {
		t.Fatalf("Run() = %#v, commands = %d", result, len(executor.calls))
	}
}

func TestVerifyRejectsMalformedExitTwoAsInfrastructureFailure(t *testing.T) {
	t.Parallel()

	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(policyPath, []byte("version: 1\nrequire_signature: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: `{"findings":[]}`, exitCode: 2}}}
	result := Run(context.Background(), Config{
		Operation: OperationVerify,
		Environment: withRunnerProtocol([]string{
			envOperationID + "=verify-malformed-refusal",
			envRequestedReference + "=oci://registry.example/schema@" + digest,
			envResolvedReference + "=oci://registry.example/schema@" + digest,
			envVerificationPolicy + "=" + policyPath,
			envExpectedArtifactType + "=" + dataplane.SchemaArtifactType,
		}),
		Executor: executor,
	})
	if result.Error == nil || result.Error.Code != "invalid_verification_output" {
		t.Fatalf("Run() = %#v, want invalid_verification_output", result)
	}
}

func TestNativeResolveAndVerifyTextNeverCrossesTheFrameBoundary(t *testing.T) {
	t.Parallel()

	const privateMarker = "derived-private-marker-7d146b"
	digest := "sha256:" + strings.Repeat("a", 64)
	resolved := "oci://registry.example/schema@" + digest
	validResolve := fmt.Sprintf(
		`{"reference":"oci://registry.example/schema:main","pinned_reference":%q,"digest":%q,"media_type":"application/vnd.oci.image.manifest.v1+json","size":42}`,
		resolved, digest,
	)
	validVerify := fmt.Sprintf(`{"reference":%q,"digest":%q,"satisfied":[],"findings":[]}`, resolved, digest)
	refusal := fmt.Sprintf(
		`{"reference":%q,"digest":%q,"satisfied":[],"findings":[{"requirement":"require_signature","detail":%q}]}`,
		resolved, digest, privateMarker,
	)

	tests := map[string]struct {
		operation   Operation
		responses   []scriptedResponse
		wantError   string
		wantRefusal bool
	}{
		"malformed resolve stdout": {
			operation: OperationResolve,
			responses: []scriptedResponse{{stdout: `{"invalid":"` + privateMarker + `"}`}},
			wantError: "invalid_resolve_output",
		},
		"resolve stderr": {
			operation: OperationResolve,
			responses: []scriptedResponse{{stdout: validResolve, stderr: privateMarker}},
			wantError: "invalid_resolve_output",
		},
		"executor error": {
			operation: OperationResolve,
			responses: []scriptedResponse{{err: errors.New(privateMarker)}},
			wantError: "execution_error",
		},
		"malformed verify stdout": {
			operation: OperationVerify,
			responses: []scriptedResponse{{stdout: `{"invalid":"` + privateMarker + `"}`}},
			wantError: "invalid_verification_output",
		},
		"verify stderr": {
			operation: OperationVerify,
			responses: []scriptedResponse{{stdout: validVerify, stderr: privateMarker}},
			wantError: "invalid_verification_output",
		},
		"inspect stderr": {
			operation: OperationVerify,
			responses: []scriptedResponse{{stdout: validVerify}, {stdout: inspectReport(resolved, digest, dataplane.SchemaArtifactType), stderr: privateMarker}},
			wantError: "invalid_artifact_output",
		},
		"verification refusal detail": {
			operation: OperationVerify,
			responses: []scriptedResponse{{stdout: refusal, exitCode: 2}},
			wantError: "verification_refused", wantRefusal: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			policyPath := filepath.Join(t.TempDir(), "policy.yaml")
			if err := os.WriteFile(policyPath, []byte("version: 1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			environment := []string{
				envOperationID + "=private-boundary",
				envRequestedReference + "=oci://registry.example/schema:main",
			}
			if test.operation == OperationVerify {
				environment = append(environment,
					envResolvedReference+"="+resolved,
					envVerificationPolicy+"="+policyPath,
					envExpectedArtifactType+"="+dataplane.SchemaArtifactType,
				)
			}
			var diagnostics bytes.Buffer
			result := Run(context.Background(), Config{
				Operation: test.operation, Environment: withRunnerProtocol(environment),
				Executor: &scriptedExecutor{t: t, responses: test.responses}, Diagnostics: &diagnostics,
			})
			if result.Error == nil || result.Error.Code != test.wantError || result.Stdout != "" {
				t.Fatalf("Run() = %#v", result)
			}
			if test.wantRefusal && !reflect.DeepEqual(result.VerificationRequirements, []string{"require_signature"}) {
				t.Fatalf("verification requirements = %#v", result.VerificationRequirements)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), privateMarker) || strings.Contains(diagnostics.String(), privateMarker) {
				t.Fatalf("private native text crossed the boundary: result=%s diagnostics=%q", encoded, diagnostics.String())
			}
		})
	}
}

func TestTargetIdentityDigestBindsRouteAndExecutionScope(t *testing.T) {
	t.Parallel()

	base, err := TargetIdentityDigest("postgres://app:old-password@DB.Example:5432/accounts?schema=public&connect_timeout=5&password=query-old&sslpassword=old-tls-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, equivalentURL := range []string{
		"postgres://app:new-password@db.example/accounts?schema=public&connect_timeout=60&password=query-new&sslpassword=new-tls-password",
	} {
		equivalent, err := TargetIdentityDigest(equivalentURL)
		if err != nil {
			t.Fatal(err)
		}
		if base != equivalent {
			t.Fatalf("credential or connection-only change %q changed identity: %s != %s", equivalentURL, base, equivalent)
		}
	}
	for name, changedURL := range map[string]string{
		"endpoint":                    "postgres://app:new-password@other.example/accounts?schema=public",
		"database":                    "postgres://app:new-password@db.example/ledger?schema=public",
		"username":                    "postgres://other:new-password@db.example/accounts?schema=public",
		"schema":                      "postgres://app:new-password@db.example/accounts?schema=private",
		"role":                        "postgres://app:new-password@db.example/accounts?schema=public&role=owner",
		"search path":                 "postgres://app:new-password@db.example/accounts?schema=public&options=-c%20search_path%3Dprivate%20-c%20password_encryption%3Dscram-sha-256",
		"standard conforming strings": "postgres://app:new-password@db.example/accounts?schema=public&standard_conforming_strings=off",
		"time zone":                   "postgres://app:new-password@db.example/accounts?schema=public&TimeZone=Europe%2FPrague",
		"password encryption":         "postgres://app:new-password@db.example/accounts?schema=public&password_encryption=md5",
		"TLS mode":                    "postgres://app:new-password@db.example/accounts?schema=public&sslmode=verify-full",
		"TLS root path":               "postgres://app:new-password@db.example/accounts?schema=public&sslrootcert=%2Ftls%2Fca.crt",
		"TLS client identity":         "postgres://app:new-password@db.example/accounts?schema=public&sslcert=%2Ftls%2Fclient.crt&sslkey=%2Ftls%2Fclient.key",
		"TLS negotiation":             "postgres://app:new-password@db.example/accounts?schema=public&sslnegotiation=direct&sslsni=0",
		"channel binding":             "postgres://app:new-password@db.example/accounts?schema=public&channel_binding=require",
		"authentication policy":       "postgres://app:new-password@db.example/accounts?schema=public&require_auth=scram-sha-256",
		"protocol policy":             "postgres://app:new-password@db.example/accounts?schema=public&min_protocol_version=3.0&max_protocol_version=latest",
		"Kerberos identity":           "postgres://app:new-password@db.example/accounts?schema=public&krbspn=postgres%2Fdb.example&krbsrvname=postgres",
		"password file path":          "postgres://app:new-password@db.example/accounts?schema=public&passfile=%2Fcredentials%2Fpgpass",
	} {
		t.Run(name, func(t *testing.T) {
			changed, err := TargetIdentityDigest(changedURL)
			if err != nil {
				t.Fatal(err)
			}
			if changed == base {
				t.Fatalf("identity did not change for %s", changedURL)
			}
		})
	}

}

func TestTargetIdentityDigestRejectsTransportAndAuthenticationDowngrade(t *testing.T) {
	t.Parallel()

	tests := [][2]string{
		{
			"postgres://app:old@db.example/accounts?sslmode=verify-full&channel_binding=require&require_auth=scram-sha-256&sslrootcert=%2Ftls%2Fca.crt",
			"postgres://app:new@db.example/accounts?sslmode=disable&channel_binding=disable&require_auth=none&sslrootcert=%2Ftls%2Fca.crt",
		},
		{
			"mysql://app:old@tcp(db.example:3306)/accounts?tls=true&allowCleartextPasswords=false&allowFallbackToPlaintext=false&allowOldPasswords=false&serverPubKey=production",
			"mysql://app:new@tcp(db.example:3306)/accounts?tls=skip-verify&allowCleartextPasswords=true&allowFallbackToPlaintext=true&allowOldPasswords=true&serverPubKey=development",
		},
	}
	for _, pair := range tests {
		secure, err := TargetIdentityDigest(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		downgraded, err := TargetIdentityDigest(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if secure == downgraded {
			t.Fatalf("security downgrade retained target identity for %q", pair[1])
		}
	}
}

func TestTargetIdentityDigestSupportsPtahMySQLNetworkDSN(t *testing.T) {
	t.Parallel()

	wrapped, err := TargetIdentityDigest("mysql://app:old-password@tcp(DB.EXAMPLE:3306)/accounts")
	if err != nil {
		t.Fatal(err)
	}
	for _, equivalent := range []string{
		"app:new-password@tcp(db.example:3306)/accounts",
		"mysql://app:new-password@db.example/accounts",
		"mariadb://app:new-password@db.example:3306/accounts",
		"mysql://app:p?%@/)ss@tcp(db.example:3306)/accounts",
	} {
		got, err := TargetIdentityDigest(equivalent)
		if err != nil {
			t.Fatalf("TargetIdentityDigest(%q): %v", equivalent, err)
		}
		if got != wrapped {
			t.Fatalf("equivalent MySQL target %q produced %q, want %q", equivalent, got, wrapped)
		}
	}
	for _, changed := range []string{
		"mysql://app:new-password@tcp(other.example:3306)/accounts",
		"mysql://app:new-password@tcp(db.example:3306)/other",
		"mysql://app:new-password@tcp(db.example:3306)/ACCOUNTS",
		"mysql://other:new-password@tcp(db.example:3306)/accounts",
		"mysql://app:new-password@tcp(db.example:3306)/accounts?parseTime=true",
	} {
		got, err := TargetIdentityDigest(changed)
		if err != nil {
			t.Fatalf("TargetIdentityDigest(%q): %v", changed, err)
		}
		if got == wrapped {
			t.Fatalf("changed MySQL target %q retained identity", changed)
		}
	}
	for _, rejected := range []string{
		"mysql://app:new-password@tcp(db.example:3306)/accounts?sql_mode=ANSI",
		"mysql://app:new-password@tcp(db.example:3306)/accounts?foreign_key_checks=0",
		"mysql://app:new-password@tcp(db.example:3306)/accounts?unique_checks=0",
		"mysql://app:new-password@tcp(db.example:3306)/accounts?database=other",
		"mysql://app:new-password@tcp(db.example:3306)/accounts?multiStatements=true",
	} {
		if _, err := TargetIdentityDigest(rejected); err == nil {
			t.Fatalf("TargetIdentityDigest(%q) accepted a server session parameter", rejected)
		}
	}
}

func TestTargetIdentityDigestSupportsCredentialFreeMySQLNetworks(t *testing.T) {
	t.Parallel()

	tcpIdentity, err := TargetIdentityDigest("mysql://tcp(DB.EXAMPLE:3306)/accounts")
	if err != nil {
		t.Fatal(err)
	}
	for _, equivalent := range []string{
		"tcp(db.example:3306)/accounts",
		"mariadb://tcp(db.example:3306)/accounts",
		"mysql://db.example/accounts",
	} {
		got, err := TargetIdentityDigest(equivalent)
		if err != nil {
			t.Fatalf("TargetIdentityDigest(%q): %v", equivalent, err)
		}
		if got != tcpIdentity {
			t.Fatalf("equivalent credential-free target %q produced %q, want %q", equivalent, got, tcpIdentity)
		}
	}
	changedTCP, err := TargetIdentityDigest("tcp(other.example:3306)/accounts")
	if err != nil {
		t.Fatal(err)
	}
	if changedTCP == tcpIdentity {
		t.Fatal("credential-free TCP endpoint change retained identity")
	}

	unixIdentity, err := TargetIdentityDigest("mysql://unix(/var/run/mysql.sock)/accounts")
	if err != nil {
		t.Fatal(err)
	}
	equivalentUnix, err := TargetIdentityDigest("unix(/var/run/mysql.sock)/accounts")
	if err != nil {
		t.Fatal(err)
	}
	if equivalentUnix != unixIdentity {
		t.Fatalf("scheme-less unix identity = %q, want %q", equivalentUnix, unixIdentity)
	}
	changedUnix, err := TargetIdentityDigest("unix(/var/run/other.sock)/accounts")
	if err != nil {
		t.Fatal(err)
	}
	if changedUnix == unixIdentity {
		t.Fatal("MySQL unix socket change retained identity")
	}
}

func TestTargetIdentityDigestRejectsAmbiguousConventionalMySQLIPv6(t *testing.T) {
	t.Parallel()

	want, err := TargetIdentityDigest("tcp(::1)/accounts")
	if err != nil {
		t.Fatal(err)
	}
	for _, equivalent := range []string{
		"tcp([::1]:3306)/accounts",
		"mysql://[::1]:3306/accounts",
	} {
		got, err := TargetIdentityDigest(equivalent)
		if err != nil {
			t.Fatalf("TargetIdentityDigest(%q): %v", equivalent, err)
		}
		if got != want {
			t.Fatalf("equivalent IPv6 target %q produced %q, want %q", equivalent, got, want)
		}
	}
	if _, err := TargetIdentityDigest("mysql://[::1]/accounts"); err == nil {
		t.Fatal("conventional MySQL IPv6 URL without a port was accepted")
	}
}

func TestTargetIdentityRejectsMySQLURLPathsThatChangeDuringDriverConversion(t *testing.T) {
	t.Parallel()

	for _, conventional := range []string{
		"mysql://app@db.example/foo%3Fbar",
		"mysql://app@db.example/foo%2Fbar",
		"mysql://app@db.example/foo%253Fbar",
	} {
		if _, err := TargetIdentityDigest(conventional); err == nil {
			t.Fatalf("TargetIdentityDigest(%q) accepted a path whose driver target changes", conventional)
		}
	}
	network, err := TargetIdentityDigest("mysql://app@tcp(db.example:3306)/foo%3Fbar")
	if err != nil {
		t.Fatalf("network-form escaped database was rejected: %v", err)
	}
	plain, err := TargetIdentityDigest("mysql://app@db.example/foo")
	if err != nil {
		t.Fatal(err)
	}
	if network == plain {
		t.Fatal("escaped network-form database collapsed onto the conventional driver target")
	}
}

func TestTargetIdentityDigestNormalizesPostgreSQLAliases(t *testing.T) {
	t.Parallel()

	base, err := TargetIdentityDigest("postgres://app:secret@db.example:5432/accounts")
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{
		"postgresql://app:rotated@db.example/accounts",
		"pgx://app:rotated@db.example:5432/accounts",
	} {
		got, err := TargetIdentityDigest(alias)
		if err != nil {
			t.Fatal(err)
		}
		if got != base {
			t.Fatalf("alias %q produced %q, want %q", alias, got, base)
		}
	}
}

func TestTargetIdentityDigestNormalizesDatabaseAndRejectsAmbiguousSessionScope(t *testing.T) {
	t.Parallel()

	encodedSlash, err := TargetIdentityDigest("postgres://db.example/db%2Ftenant")
	if err != nil {
		t.Fatal(err)
	}
	queryDatabase, err := TargetIdentityDigest("postgres://db.example/ignored?dbname=db%2Ftenant")
	if err != nil {
		t.Fatal(err)
	}
	if encodedSlash != queryDatabase {
		t.Fatal("equivalent encoded database names produced different target identities")
	}
	if _, err := TargetIdentityDigest("postgres://db.example/db/tenant"); err == nil {
		t.Fatal("unescaped multi-segment database path was accepted")
	}

	for _, ambiguous := range []string{
		"postgres://db.example/app?search_path=first&search_path=second",
		"postgres://db.example/app?role=first&role=second",
		"postgres://db.example/app?dbname=first&database=second",
		"postgres://db.example/app?dbname=same&database=same",
	} {
		if _, err := TargetIdentityDigest(ambiguous); err == nil {
			t.Fatalf("ambiguous session scope %q was accepted", ambiguous)
		}
	}
}

func TestTargetIdentityDigestBindsQuotedSessionOptionValues(t *testing.T) {
	t.Parallel()

	fooBar, err := TargetIdentityDigest(`postgres://db.example/app?options=-c%20search_path%3D%22foo%20bar%22`)
	if err != nil {
		t.Fatal(err)
	}
	fooBaz, err := TargetIdentityDigest(`postgres://db.example/app?options=-c%20search_path%3D%22foo%20baz%22`)
	if err != nil {
		t.Fatal(err)
	}
	if fooBar == fooBaz {
		t.Fatal("different quoted session scope values retained target identity")
	}
}

func TestTargetIdentityDigestPreservesPostgreSQLOptionQuotes(t *testing.T) {
	t.Parallel()

	unquoted, err := TargetIdentityDigest(`postgres://db.example/app?options=-c%20search_path%3DFoo`)
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := TargetIdentityDigest(`postgres://db.example/app?options=-c%20search_path%3D%22Foo%22`)
	if err != nil {
		t.Fatal(err)
	}
	if unquoted == quoted {
		t.Fatal("quoted and unquoted PostgreSQL options produced the same target identity")
	}
}

func TestTargetIdentityDigestPreservesRuntimeParameterKeySemantics(t *testing.T) {
	t.Parallel()

	assertDifferent := func(left, right string) {
		t.Helper()
		leftDigest, err := TargetIdentityDigest(left)
		if err != nil {
			t.Fatal(err)
		}
		rightDigest, err := TargetIdentityDigest(right)
		if err != nil {
			t.Fatal(err)
		}
		if leftDigest == rightDigest {
			t.Fatalf("distinct runtime keys collapsed: %q and %q", left, right)
		}
	}

	assertDifferent(
		"postgres://db.example/app?foo.bar=one",
		"postgres://db.example/app?foob.ar=one",
	)
	assertDifferent(
		"postgres://db.example/app",
		"postgres://db.example/app?ssl.mode=require",
	)
	assertDifferent(
		"postgres://db.example/app?options=-c%20foo.bar%3Done",
		"postgres://db.example/app?options=-c%20foob.ar%3Done",
	)
	if _, err := TargetIdentityDigest("mysql://db.example/app?parsetime=true"); err == nil {
		t.Fatal("case-mismatched unknown MySQL parameter was accepted")
	}
}

func TestTargetIdentityDigestUsesEffectivePostgreSQLRoute(t *testing.T) {
	t.Parallel()

	base, err := TargetIdentityDigest("postgres://app:secret@localhost/app")
	if err != nil {
		t.Fatal(err)
	}
	for _, equivalent := range []string{
		"postgres://app:rotated@localhost:5432/app",
		"postgres://app:rotated@ignored.invalid/ignored?host=localhost&port=5432&dbname=app",
	} {
		got, err := TargetIdentityDigest(equivalent)
		if err != nil {
			t.Fatal(err)
		}
		if got != base {
			t.Fatalf("effective route %q produced %q, want %q", equivalent, got, base)
		}
	}

	for _, invalid := range []string{
		"postgres://db.example/app?host=one.example,two.example",
		"postgres://db.example/app?host=one.example&host=two.example",
		"postgres://db.example/app?port=5432,5433",
		"postgres://db.example/app?host=",
		"postgres://db.example/app?port=",
	} {
		if _, err := TargetIdentityDigest(invalid); err == nil {
			t.Fatalf("multi-endpoint target %q was accepted", invalid)
		}
	}
}

func TestTargetIdentityDigestBindsPostgreSQLUnixSocketPort(t *testing.T) {
	t.Parallel()

	defaultPort, err := TargetIdentityDigest("postgres://app@ignored/app?host=%2Fvar%2Frun%2Fpostgresql")
	if err != nil {
		t.Fatal(err)
	}
	explicitDefault, err := TargetIdentityDigest("postgres://app@ignored/app?host=%2Fvar%2Frun%2Fpostgresql&port=5432")
	if err != nil {
		t.Fatal(err)
	}
	if explicitDefault != defaultPort {
		t.Fatalf("explicit default Unix-socket port produced %q, want %q", explicitDefault, defaultPort)
	}

	alternatePort, err := TargetIdentityDigest("postgres://app@ignored/app?host=%2Fvar%2Frun%2Fpostgresql&port=5433")
	if err != nil {
		t.Fatal(err)
	}
	if alternatePort == defaultPort {
		t.Fatal("distinct PostgreSQL Unix-socket ports produced one target identity")
	}

	if _, err := TargetIdentityDigest("postgres://app@ignored/app?host=%2Fvar%2Frun%2Fpostgresql&port=invalid"); err == nil {
		t.Fatal("invalid PostgreSQL Unix-socket port was accepted")
	}
}

func TestTargetIdentityDigestPreservesUnprovenHostAliases(t *testing.T) {
	t.Parallel()

	for _, pair := range [][2]string{
		{"postgres://db.example/app", "postgres://db.example./app"},
		{"postgres://127.0.0.1/app", "postgres://[::1]/app"},
		{"postgres://localhost.localdomain/app", "postgres://127.0.0.1/app"},
	} {
		left, err := TargetIdentityDigest(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		right, err := TargetIdentityDigest(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if left == right {
			t.Fatalf("unproven host aliases produced one identity: %q and %q", pair[0], pair[1])
		}
	}
}

func TestTargetIdentityDigestNormalizesIPv6Routes(t *testing.T) {
	t.Parallel()

	compressed, err := TargetIdentityDigest("postgres://app:secret@[2001:db8::1]:5432/app")
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := TargetIdentityDigest("postgres://app:rotated@[2001:0DB8:0:0:0:0:0:1]/app")
	if err != nil {
		t.Fatal(err)
	}
	if compressed != expanded {
		t.Fatalf("equivalent IPv6 routes produced %q and %q", compressed, expanded)
	}
}

func databaseEnvironment(operationID string) []string {
	return []string{
		envOperationID + "=" + operationID,
		envDatabaseURL + "=postgres://app:secret@db.example/app",
		envExpectedDatabaseEngine + "=PostgreSQL",
		envCoordinationDigest + "=" + testCoordinationDigest(),
		envDispatchNotAfter + "=2099-01-01T00:00:00Z",
		envExecutionNotAfter + "=2099-01-01T00:00:00Z",
		envTerminationGracePeriod + "=30",
		envPlanSealPublicKey + "=" + testPlanSealKey.PublicKey().Encode(),
	}
}

// testPlanSealKey is the one key pair every test environment built by
// databaseEnvironment carries the public half of. Only a Plan operation reads
// it; every other operation ignores it the same way it ignores any other
// input it has no use for.
var testPlanSealKey = mustGenerateTestPlanSealKey()

func mustGenerateTestPlanSealKey() planseal.KeyPair {
	key, err := planseal.Generate()
	if err != nil {
		panic(err)
	}
	return key
}

// openSealedPlan decrypts a Plan result's Stdout with testPlanSealKey, the
// key databaseEnvironment gives every Plan Job under test, and fails the test
// if it cannot: a plan the runner sealed correctly always opens with the key
// it was sealed to.
func openSealedPlan(t *testing.T, sealed string) string {
	t.Helper()
	plaintext, err := testPlanSealKey.Open(sealed)
	if err != nil {
		t.Fatalf("Open(sealed plan) error = %v", err)
	}
	return string(plaintext)
}

func testCoordinationDigest() string { return "sha256:" + strings.Repeat("9", 64) }

func databaseTargetDigest(t *testing.T) string {
	t.Helper()
	digest, err := TargetIdentityDigest("postgres://app:secret@db.example/app")
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func validPlanDocument(sql string) string {
	return fmt.Sprintf(
		`{"format_version":1,"name":"operator-plan","dialect":"postgres","from_fingerprint":"sha256:%s","to_fingerprint":"sha256:%s","destructive":false,"statements":[{"sql":%q,"severity":"safe","reason":"schema change"}]}`+"\n",
		strings.Repeat("a", 64),
		strings.Repeat("b", 64),
		sql,
	)
}

func exactSizePlanDocument(t *testing.T, size int) string {
	t.Helper()
	const sqlPrefix = "CREATE TABLE exact_plan_limit (payload text DEFAULT '"
	const sqlSuffix = "');"
	base := validPlanDocument(sqlPrefix + sqlSuffix)
	padding := size - len(base)
	if padding < 0 {
		t.Fatalf("plan size %d is smaller than fixture envelope %d", size, len(base))
	}
	plan := validPlanDocument(sqlPrefix + strings.Repeat("<", padding) + sqlSuffix)
	if len(plan) != size {
		t.Fatalf("exact plan bytes = %d, want %d", len(plan), size)
	}
	return plan
}

// stablePlanResponses are two plan reads that save the same plan, and the dry
// run that reads it back and lists its statements.
func stablePlanResponses(t *testing.T, plan string) []scriptedResponse {
	t.Helper()
	return []scriptedResponse{
		planResponse(plan),
		planResponse(plan),
		{stdout: applyReport(t, dataplane.SchemaApplyOutcomeDryRun, plan), stderr: "Planned schema changes:\n"},
	}
}

// planResponse is one `schema plan --output <path> --json` read: it saves the
// plan where the command line says and prints the report that names it.
// Standard error carries what a person reads.
func planResponse(plan string) scriptedResponse {
	return scriptedResponse{
		savePlan:  plan,
		stdoutFor: func(spec CommandSpec) string { return planReport(argumentAfter(spec, "--output"), plan) },
		stderr:    "Planned schema changes:\nCREATE TABLE example (id bigint);\nPlan saved to file://plan.json\n",
	}
}

// planReport is the document a plan read prints for a saved plan, with the
// plan embedded as it is.
func planReport(outputPath, plan string) string {
	return fmt.Sprintf(
		`{"contract_version":1,"outcome":"changes","plan_digest":%q,"plan_path":%q,"plan":%s}`+"\n",
		sha256Digest([]byte(plan)), outputPath, strings.TrimSpace(plan),
	)
}

// planNoChangesReport is what a plan read prints when the database already
// matches: no plan, and nothing saved.
const planNoChangesReport = `{"contract_version":1,"outcome":"no-changes"}` + "\n"

func planNoChangesResponse() scriptedResponse {
	return scriptedResponse{stdout: planNoChangesReport, stderr: "Schema is synced, no changes to be made.\n"}
}

// applyReport is the document `schema apply --plan <path> --json` prints for
// plan.
func applyReport(t *testing.T, outcome, plan string) string {
	t.Helper()
	decoded := mustDecodePlan(t, plan)
	return encodeApplyReport(t, dataplane.SchemaApplyReport{
		ContractVersion: 1,
		Outcome:         outcome,
		PlanName:        decoded.Name,
		PlanDigest:      sha256Digest([]byte(plan)),
		Statements:      planStatements(decoded),
	})
}

// encodeApplyReport indents a report the way Ptah does. It leaves HTML
// characters as they are, as the plan fixtures do, so a report is no larger
// against its fixture than Ptah's is against a plan it wrote.
func encodeApplyReport(t *testing.T, report dataplane.SchemaApplyReport) string {
	t.Helper()
	var document bytes.Buffer
	encoder := json.NewEncoder(&document)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		t.Fatal(err)
	}
	return document.String()
}

func mustDecodePlan(t *testing.T, plan string) dataplane.PlanFile {
	t.Helper()
	decoded, err := dataplane.DecodePlan([]byte(plan), "PostgreSQL")
	if err != nil {
		t.Fatalf("decode plan fixture: %v", err)
	}
	return decoded
}

// migrationRunDocument is what `ptah migrations up --json` writes: the outcome
// the database accounted for, which is the only thing that says what a stopped
// run left behind.
// migrationRunDocumentWithDirtyError is migrationRunDocument, with a database
// error at both places Ptah's own account of a dirty revision can carry one:
// the run's own top-level error, and the dirty revision's.
func migrationRunDocumentWithDirtyError(outcome, runError, dirtyError string) string {
	return fmt.Sprintf(
		`{"contract_version":1,"direction":"up","outcome":%q,"planned":[3],"applied":[],`+
			`"error":%q,`+
			`"status":{"contract_version":1,"current_version":2,"total_migrations":3,"has_pending_changes":true,`+
			`"dirty_revision":{"version":3,"applied":2,"total":5,"error":%q}}}`,
		outcome, runError, dirtyError,
	)
}

// TestMigrationRunRedactsTheDatabasesFreeTextError proves the runner drops
// Ptah's own free-text account of why a migration run left a dirty revision,
// at both places it can appear, before the frame is ever written. Neither
// string is read by anything downstream of this frame -- the controller
// names the outcome and the affected version, never Ptah's sentence -- so
// this is a redaction rather than a seal: there is no reader on the other
// end for a key to protect.
func TestMigrationRunRedactsTheDatabasesFreeTextError(t *testing.T) {
	t.Parallel()

	const declaredRowValue = "duplicate key value violates unique constraint \"users_email_key\": Key (email)=(alice@example.com) already exists"
	document := migrationRunDocumentWithDirtyError("partial", "failed to apply migration 3: "+declaredRowValue, declaredRowValue)
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: document, exitCode: 1}}}
	result := Run(context.Background(), Config{
		Operation:   OperationMigrationApply,
		Environment: withRunnerProtocol(migrationApplyEnvironment(t, "migration-redact-error")),
		Executor:    executor,
	})
	if result.MigrationRun == nil || result.MigrationRun.Status == nil || result.MigrationRun.Status.DirtyRevision == nil {
		t.Fatalf("Run() = %#v, want the run report the child wrote", result)
	}
	if result.MigrationRun.Error != "" {
		t.Fatalf("MigrationRun.Error = %q, want it redacted", result.MigrationRun.Error)
	}
	if result.MigrationRun.Status.DirtyRevision.Error != "" {
		t.Fatalf("MigrationRun.Status.DirtyRevision.Error = %q, want it redacted", result.MigrationRun.Status.DirtyRevision.Error)
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	if bytes.Contains(frame, []byte(declaredRowValue)) {
		t.Fatalf("the framed result -- what reaches the Pod log -- carries the database's free-text error: %s", frame)
	}
}

// TestMigrationHistoryRedactsTheDirtyRevisionError is the read-only twin: a
// history read that finds an existing dirty revision must not carry forward
// whatever database error left it dirty, since a migration-history operation
// runs on every reconciliation and would otherwise repeat the leak on every
// pass rather than once per failed apply.
func TestMigrationHistoryRedactsTheDirtyRevisionError(t *testing.T) {
	t.Parallel()

	const declaredRowValue = "check constraint \"countries_code_check\" is violated by some row"
	document := fmt.Sprintf(
		`{"contract_version":1,"current_version":2,"total_migrations":3,"has_pending_changes":true,`+
			`"pending_migrations":[3],"dirty_revision":{"version":3,"applied":2,"total":5,"error":%q}}`,
		declaredRowValue,
	)
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: document}}}
	result := Run(context.Background(), Config{
		Operation:   OperationMigrationHistory,
		Environment: withRunnerProtocol(migrationEnvironment("migration-history-redact-error")),
		Executor:    executor,
	})
	if result.MigrationHistory == nil || result.MigrationHistory.DirtyRevision == nil {
		t.Fatalf("Run() = %#v, want the history report the child wrote", result)
	}
	if result.MigrationHistory.DirtyRevision.Error != "" {
		t.Fatalf("MigrationHistory.DirtyRevision.Error = %q, want it redacted", result.MigrationHistory.DirtyRevision.Error)
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	if bytes.Contains(frame, []byte(declaredRowValue)) {
		t.Fatalf("the framed result -- what reaches the Pod log -- carries the dirty revision's free-text error: %s", frame)
	}
}

func migrationRunDocument(outcome string) string {
	return fmt.Sprintf(
		`{"contract_version":1,"direction":"up","outcome":%q,"planned":[3],"applied":[],`+
			`"error":"failed to apply migration 3",`+
			`"status":{"contract_version":1,"current_version":2,"total_migrations":3,"has_pending_changes":true,`+
			`"dirty_revision":{"version":3,"applied":2,"total":5}}}`,
		outcome,
	)
}

func migrationEnvironment(operationID string) []string {
	// A materialized directory rather than a reference: the fetch container
	// holds the registry credentials, and this process holds the database ones.
	return append(databaseEnvironment(operationID), envMigrationsDir+"=/source/migrations")
}

// testSequence is the approved sequence a migration Apply Job carries, and
// testSequenceDigest the digest its plan bound. The runner digests the one and
// compares it with the other, so the pair has to agree.
func testSequence() string {
	return `[{"version":3,"version_key":"3","checksum":"sha256:` + strings.Repeat("e", 64) +
		`","checkpoint":false,"transaction_mode":""},{"version":4,"version_key":"4","checksum":"sha256:` +
		strings.Repeat("f", 64) + `","checkpoint":false,"transaction_mode":"file"}]`
}

func testSequenceDigest() string {
	var entries []fingerprint.SequenceEntry
	if err := json.Unmarshal([]byte(testSequence()), &entries); err != nil {
		panic(err)
	}
	digest, err := fingerprint.MigrationSequenceDigest(entries)
	if err != nil {
		panic(err)
	}
	return digest
}

// testHistoryFingerprint stands for the history the plan was computed
// against. The runner cannot re-derive it -- it reads no revision table -- so
// what it checks is that the plan named one.

func testHistoryFingerprint() string { return "sha256:" + strings.Repeat("d", 64) }

// migrationApplyEnvironment is what a migration Apply Job actually carries: the
// bindings of the plan that authorized it, without which the runner refuses to
// open the database.
func migrationApplyEnvironment(t *testing.T, operationID string) []string {
	t.Helper()
	return append(migrationEnvironment(operationID),
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
		envExpectedSequence+"="+testSequence(),
		envExpectedSequenceDigest+"="+testSequenceDigest(),
		envExpectedHistory+"="+testHistoryFingerprint(),
	)
}

// A migration that stopped is the run whose report matters most: the exit
// status says only that Ptah stopped, and the document says what the database
// now holds.
func TestMigrationApplyKeepsItsReportWhenPtahStops(t *testing.T) {
	t.Parallel()

	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout:   migrationRunDocument("partial"),
		exitCode: 1,
	}}}
	result := Run(context.Background(), Config{
		Operation:   OperationMigrationApply,
		Environment: withRunnerProtocol(migrationApplyEnvironment(t, "migration-apply-stopped")),
		Executor:    executor,
	})

	if result.Error == nil || result.Error.Code != "child_exit" {
		t.Fatalf("Run() error = %#v, want the child exit", result.Error)
	}
	if result.MigrationRun == nil {
		t.Fatalf("Run() = %#v, want the run report the child wrote", result)
	}
	if result.MigrationRun.Status == nil || result.MigrationRun.Status.DirtyRevision == nil {
		t.Fatal("the run report reached the frame without the database's account of it")
	}
	if !result.MutationStarted || !result.Uncertain {
		t.Fatalf("a partial run reported MutationStarted=%t Uncertain=%t", result.MutationStarted, result.Uncertain)
	}
	if _, err := MarshalFrame(result); err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
}

// The approved sequence reaches Ptah as the file `--expect-sequence` names,
// holding the versions and keys the plan approved in its order. It exists while
// the child runs and not after, and the child does not inherit the variable it
// came from.
func TestMigrationApplyHandsPtahTheApprovedSequence(t *testing.T) {
	t.Parallel()

	temporary := t.TempDir()
	var sequencePath, sequence string
	var childEnvironment []string
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: migrationRunDocument("applied"),
		inspect: func(spec CommandSpec) {
			childEnvironment = spec.Env
			for index, argument := range spec.Args {
				if argument == "--expect-sequence" && index+1 < len(spec.Args) {
					sequencePath = spec.Args[index+1]
				}
			}
			content, err := os.ReadFile(sequencePath)
			if err != nil {
				t.Errorf("read the approved sequence while the child runs: %v", err)
			}
			sequence = string(content)
		},
	}}}
	Run(context.Background(), Config{
		Operation:   OperationMigrationApply,
		Environment: withRunnerProtocol(migrationApplyEnvironment(t, "migration-apply-sequence")),
		Executor:    executor,
		TempDir:     temporary,
	})

	if len(executor.calls) != 1 {
		t.Fatalf("executor calls = %d, want the child to have run", len(executor.calls))
	}
	if filepath.Dir(sequencePath) != temporary {
		t.Fatalf("--expect-sequence names %q, want a file in the runner's temporary directory", sequencePath)
	}
	if want := `{"migrations":[{"version":3,"version_key":"3"},{"version":4,"version_key":"4"}]}`; sequence != want {
		t.Fatalf("approved sequence file = %s, want %s", sequence, want)
	}
	if _, err := os.Stat(sequencePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("approved sequence file after the run: %v, want it removed", err)
	}
	for _, variable := range childEnvironment {
		if strings.HasPrefix(variable, envExpectedSequence+"=") {
			t.Fatalf("the child inherited %s", envExpectedSequence)
		}
	}
}

// A migration child that cannot be read is the ambiguous case: it may have
// committed statements, and nothing outside the database can say.
func TestMigrationApplyClaimsTheMutationWhenTheChildCannotBeRead(t *testing.T) {
	t.Parallel()

	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{err: errors.New("context canceled")}}}
	result := Run(context.Background(), Config{
		Operation:   OperationMigrationApply,
		Environment: withRunnerProtocol(migrationApplyEnvironment(t, "migration-apply-ambiguous")),
		Executor:    executor,
	})

	if result.Error == nil {
		t.Fatalf("Run() = %#v, want an execution failure", result)
	}
	if result.MigrationRun != nil {
		t.Fatalf("Run() = %#v, want no report from a child that was not read", result)
	}
	if !result.MutationStarted || !result.Uncertain {
		t.Fatalf("an unread migration child reported MutationStarted=%t Uncertain=%t", result.MutationStarted, result.Uncertain)
	}
	if _, err := MarshalFrame(result); err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
}

// The migration operations never reach a registry. They read a directory a
// fetch container already materialized, so the process holding the database
// credentials holds no registry credentials -- and a reference offered to one
// of them is refused rather than fetched.
func TestMigrationOperationsNeverReachTheRegistry(t *testing.T) {
	t.Parallel()

	for _, operation := range []Operation{OperationMigrationHistory, OperationMigrationApply} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t}
			// A credential-bearing reference in the environment would have made
			// the old shape prepare registry access. Nothing reads it now.
			environment := append(databaseEnvironment("migration-registry-access"),
				envResolvedReference+"=oci://user:password@registry.example/team/app-migrations@sha256:"+strings.Repeat("a", 64),
			)
			if operation == OperationMigrationApply {
				// An Apply is refused before the command is built unless the
				// plan that authorized it is bound, so this case carries that
				// binding and still reaches the missing directory.
				environment = append(environment,
					envExpectedCoordination+"="+testCoordinationDigest(),
					envExpectedTargetDigest+"="+databaseTargetDigest(t),
					envExpectedSequence+"="+testSequence(),
					envExpectedSequenceDigest+"="+testSequenceDigest(),
					envExpectedHistory+"="+testHistoryFingerprint(),
				)
			}
			result := Run(context.Background(), Config{
				Operation: operation, Environment: withRunnerProtocol(environment), Executor: executor,
			})
			if result.Error == nil || result.Error.Code != "invalid_input" || len(executor.calls) != 0 {
				t.Fatalf("Run() = %#v, commands = %d", result, len(executor.calls))
			}
			if !strings.Contains(result.Error.Message, "migration directory is empty") {
				t.Fatalf("error message = %q, want the missing materialized directory", result.Error.Message)
			}
		})
	}
}

func TestAcceptanceReviewMigrationRejectsChangedTargetBeforeDispatch(t *testing.T) {
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: migrationRunDocument("partial"), exitCode: 1}}}
	environment := append(migrationEnvironment("review-changed-target"),
		envExpectedTargetDigest+"=sha256:"+strings.Repeat("a", 64),
		envExpectedCoordination+"="+testCoordinationDigest(),
	)
	result := Run(context.Background(), Config{Operation: OperationMigrationApply, Environment: withRunnerProtocol(environment), Executor: executor})
	if len(executor.calls) != 0 {
		t.Fatalf("migration child was invoked despite a mismatched approved target digest; MutationStarted=%t", result.MutationStarted)
	}
}

func TestAcceptanceReviewMigrationRejectsExpiredDispatch(t *testing.T) {
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: migrationRunDocument("partial"), exitCode: 1}}}
	environment := environmentWithout(migrationEnvironment("review-expired-dispatch"), envDispatchNotAfter, envExecutionNotAfter)
	environment = append(environment, envDispatchNotAfter+"=2000-01-01T00:00:00Z", envExecutionNotAfter+"=2000-01-01T00:00:00Z")
	result := Run(context.Background(), Config{Operation: OperationMigrationApply, Environment: withRunnerProtocol(environment), Executor: executor,
		Clock: func() time.Time { return time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC) },
	})
	if len(executor.calls) != 0 {
		t.Fatalf("migration child was invoked after both absolute deadlines expired; MutationStarted=%t", result.MutationStarted)
	}
}

func TestAcceptanceReviewMigrationRequiresApprovedPlanBeforeDispatch(t *testing.T) {
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: migrationRunDocument("partial"), exitCode: 1}}}
	result := Run(context.Background(), Config{Operation: OperationMigrationApply, Environment: withRunnerProtocol(migrationEnvironment("review-unbound-plan")), Executor: executor})
	if len(executor.calls) != 0 {
		t.Fatalf("migration child was invoked without an approved sequence or history fingerprint: args=%v MutationStarted=%t", executor.calls[0].Args, result.MutationStarted)
	}
}

// A migration Apply is the last gate before SQL, so every way its authority can
// be absent, malformed or stale is a refusal before the child exists. Each row
// is one such way.
func TestMigrationApplyRefusesEveryUnauthorizedDispatch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		mutate  func(t *testing.T, environment []string) []string
		wantErr string
	}{
		{
			name: "missing coordination binding",
			mutate: func(_ *testing.T, environment []string) []string {
				return environmentWithout(environment, envExpectedCoordination)
			},
			wantErr: "missing_coordination_binding",
		},
		{
			name: "changed coordination realm",
			mutate: func(_ *testing.T, environment []string) []string {
				return append(environment, envExpectedCoordination+"=sha256:"+strings.Repeat("1", 64))
			},
			wantErr: "coordination_binding_mismatch",
		},
		{
			name: "missing target binding",
			mutate: func(_ *testing.T, environment []string) []string {
				return environmentWithout(environment, envExpectedTargetDigest)
			},
			wantErr: "missing_target_binding",
		},
		{
			name: "malformed target binding",
			mutate: func(_ *testing.T, environment []string) []string {
				return append(environment, envExpectedTargetDigest+"=sha256:NOTADIGEST")
			},
			wantErr: "missing_target_binding",
		},
		{
			name: "changed target identity",
			mutate: func(_ *testing.T, environment []string) []string {
				return append(environment, envExpectedTargetDigest+"=sha256:"+strings.Repeat("a", 64))
			},
			wantErr: "target_binding_mismatch",
		},
		{
			name: "missing approved sequence",
			mutate: func(_ *testing.T, environment []string) []string {
				return environmentWithout(environment, envExpectedSequenceDigest)
			},
			wantErr: "missing_plan_binding",
		},
		{
			name: "malformed approved sequence",
			mutate: func(_ *testing.T, environment []string) []string {
				return append(environment, envExpectedSequenceDigest+"=3")
			},
			wantErr: "missing_plan_binding",
		},
		{
			name: "missing approved sequence list",
			mutate: func(_ *testing.T, environment []string) []string {
				return environmentWithout(environment, envExpectedSequence)
			},
			wantErr: "plan_binding_mismatch",
		},
		{
			name: "a sequence another digest names",
			mutate: func(_ *testing.T, environment []string) []string {
				return append(environment, envExpectedSequence+"="+strings.Replace(testSequence(), `"version":4`, `"version":5`, 1))
			},
			wantErr: "plan_binding_mismatch",
		},
		{
			name: "a sequence with a field the digest does not bind",
			mutate: func(_ *testing.T, environment []string) []string {
				return append(environment, envExpectedSequence+"="+strings.Replace(testSequence(), `"version":3,`, `"version":3,"extra":1,`, 1))
			},
			wantErr: "plan_binding_mismatch",
		},
		{
			name: "missing history fingerprint",
			mutate: func(_ *testing.T, environment []string) []string {
				return environmentWithout(environment, envExpectedHistory)
			},
			wantErr: "missing_plan_binding",
		},
		{
			name: "malformed history fingerprint",
			mutate: func(_ *testing.T, environment []string) []string {
				return append(environment, envExpectedHistory+"=sha256:"+strings.Repeat("D", 64))
			},
			wantErr: "missing_plan_binding",
		},
		{
			name: "missing dispatch deadline",
			mutate: func(_ *testing.T, environment []string) []string {
				return environmentWithout(environment, envDispatchNotAfter)
			},
			wantErr: "missing_dispatch_deadline",
		},
		{
			name: "expired dispatch deadline",
			mutate: func(_ *testing.T, environment []string) []string {
				return append(environment, envDispatchNotAfter+"="+now.Add(-time.Second).Format(time.RFC3339Nano))
			},
			wantErr: "dispatch_deadline_expired",
		},
		{
			name: "missing execution deadline",
			mutate: func(_ *testing.T, environment []string) []string {
				return environmentWithout(environment, envExecutionNotAfter)
			},
			wantErr: "missing_execution_deadline",
		},
		{
			name: "execution deadline before dispatch deadline",
			mutate: func(_ *testing.T, environment []string) []string {
				return append(environment, envExecutionNotAfter+"="+now.Add(-time.Hour).Format(time.RFC3339Nano))
			},
			wantErr: "missing_execution_deadline",
		},
		{
			// Named for what it measures. Both deadlines are behind now, and the
			// refusal is the dispatch one: no pair of deadlines reaches
			// execution_deadline_expired, because that check runs after this one
			// and after the guard that refuses an execution deadline behind the
			// dispatch deadline. TestNoDeadlinePairProducesTheExecutionRefusal
			// walks the pairs and says so.
			name: "both deadlines behind now",
			mutate: func(_ *testing.T, environment []string) []string {
				return append(environment,
					envDispatchNotAfter+"="+now.Add(-2*time.Hour).Format(time.RFC3339Nano),
					envExecutionNotAfter+"="+now.Add(-time.Hour).Format(time.RFC3339Nano),
				)
			},
			wantErr: "dispatch_deadline_expired",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: migrationRunDocument("partial"), exitCode: 1}}}
			result := Run(context.Background(), Config{
				Operation:   OperationMigrationApply,
				Environment: withRunnerProtocol(test.mutate(t, migrationApplyEnvironment(t, "unauthorized-migration-apply"))),
				Executor:    executor,
				Clock:       func() time.Time { return now },
			})
			if result.Error == nil || result.Error.Code != test.wantErr {
				t.Fatalf("error = %#v, want %s", result.Error, test.wantErr)
			}
			if len(executor.calls) != 0 || result.MutationStarted || result.Uncertain {
				t.Fatalf("unauthorized migration Apply reached the database: calls=%d MutationStarted=%t Uncertain=%t",
					len(executor.calls), result.MutationStarted, result.Uncertain)
			}
		})
	}
}

// What the plan binds is the route, not the credential that reaches it. A
// password rotated between planning and dispatch leaves the bound identity
// unchanged, and a migration Apply that refused it would turn every rotation
// into a migration that can no longer run. This is the schema family's
// password-rotation row for the migration family.
func TestMigrationApplyDispatchesWhenOnlyThePasswordRotated(t *testing.T) {
	t.Parallel()

	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: migrationRunDocument("partial"), exitCode: 1}}}
	// The environment every refusal row starts from, with one change: the
	// Pod's database URL carries a new password, while the expected target
	// digest is still the one derived from the URL the plan was computed
	// against.
	environment := append(
		environmentWithout(migrationApplyEnvironment(t, "migration-apply-rotated-password"), envDatabaseURL),
		envDatabaseURL+"=postgres://app:rotated@db.example/app",
	)
	result := Run(context.Background(), Config{
		Operation:   OperationMigrationApply,
		Environment: withRunnerProtocol(environment),
		Executor:    executor,
	})

	if len(executor.calls) != 1 {
		t.Fatalf("a rotated password stopped an authorized migration Apply: calls=%d error=%#v", len(executor.calls), result.Error)
	}
	// The child ran and exited; the only refusal is the one it reported.
	if result.Error == nil || result.Error.Code != "child_exit" {
		t.Fatalf("error = %#v, want the scripted child's exit rather than a dispatch refusal", result.Error)
	}
	if !result.MutationStarted {
		t.Fatalf("the child ran and the frame did not claim the mutation: %#v", result)
	}
}

// A Pod that was authorized and then sat can expire between the first check and
// the child, so the check adjacent to dispatch is the one that decides.
func TestMigrationApplyRechecksDeadlinesImmediatelyBeforeExecution(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	times := []time.Time{deadline.Add(-time.Second), deadline}
	clockCalls := 0
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: migrationRunDocument("partial"), exitCode: 1}}}
	environment := append(migrationApplyEnvironment(t, "migration-deadline-race"),
		envDispatchNotAfter+"="+deadline.Format(time.RFC3339Nano),
		envExecutionNotAfter+"="+deadline.Format(time.RFC3339Nano),
	)
	result := Run(context.Background(), Config{
		Operation:   OperationMigrationApply,
		Environment: withRunnerProtocol(environment),
		Executor:    executor,
		Clock: func() time.Time {
			if clockCalls >= len(times) {
				t.Fatalf("clock called more than %d times", len(times))
			}
			now := times[clockCalls]
			clockCalls++
			return now
		},
	})
	if result.Error == nil || result.Error.Code != "dispatch_deadline_expired" {
		t.Fatalf("error = %#v, want dispatch_deadline_expired", result.Error)
	}
	if len(executor.calls) != 0 || result.MutationStarted || result.Uncertain {
		t.Fatalf("a migration Apply past its window still ran: calls=%d result=%#v", len(executor.calls), result)
	}
	if clockCalls != 2 {
		t.Fatalf("clock calls = %d, want the check adjacent to dispatch as well", clockCalls)
	}
}

// The execution deadline is the child's, not only the dispatch decision's: a
// migration that started inside its window is cancelled when the window ends.
func TestMigrationApplyExecutionDeadlineCancelsAStartedChild(t *testing.T) {
	t.Parallel()

	deadline := time.Now().UTC().Add(250 * time.Millisecond)
	executor := &contextDeadlineExecutor{}
	environment := append(migrationApplyEnvironment(t, "migration-execution-deadline"),
		envDispatchNotAfter+"="+deadline.Format(time.RFC3339Nano),
		envExecutionNotAfter+"="+deadline.Format(time.RFC3339Nano),
	)
	started := time.Now()
	result := Run(context.Background(), Config{
		Operation:   OperationMigrationApply,
		Environment: withRunnerProtocol(environment),
		Executor:    executor,
	})
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want the child to have started", executor.calls)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the child ran %s past its execution deadline", elapsed)
	}
	if result.Error == nil {
		t.Fatalf("Run() = %#v, want the cancelled child reported", result)
	}
	// A cancelled migration may already have committed statements, and nothing
	// outside the database can say which.
	if !result.MutationStarted || !result.Uncertain {
		t.Fatalf("a cancelled migration child reported MutationStarted=%t Uncertain=%t", result.MutationStarted, result.Uncertain)
	}
}
