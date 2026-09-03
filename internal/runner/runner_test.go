package runner

import (
	"bytes"
	"context"
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
)

type scriptedResponse struct {
	stdout   string
	stderr   string
	exitCode int
	err      error
	inspect  func(CommandSpec)
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
	_, _ = io.WriteString(stdout, response.stdout)
	_, _ = io.WriteString(stderr, response.stderr)
	return response.exitCode, response.err
}

func TestBuildCommand(t *testing.T) {
	t.Parallel()

	inputs := Inputs{
		RequestedReference:     "oci://registry.example/team/schema:main",
		ResolvedReference:      "oci://registry.example/team/schema@sha256:" + strings.Repeat("a", 64),
		VerificationPolicyPath: "/policy/verification.json",
		PlanPath:               "/tmp/approved-plan.hcl",
	}
	tests := []struct {
		name      string
		operation Operation
		want      []string
	}{
		{name: "resolve", operation: OperationResolve, want: []string{"oci", "resolve", inputs.RequestedReference, "--format", "json"}},
		{name: "verify immutable resolution", operation: OperationVerify, want: []string{"oci", "verify", inputs.ResolvedReference, "--policy", inputs.VerificationPolicyPath, "--format", "json"}},
		{name: "observe", operation: OperationObserve, want: []string{"schema", "drift", "--format", "json"}},
		{name: "plan", operation: OperationPlan, want: []string{"schema", "plan", "--dry-run"}},
		{name: "apply", operation: OperationApply, want: []string{"schema", "apply", "--plan", inputs.PlanPath, "--auto-approve"}},
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
		Environment: []string{
			envOperationID + "=resolve-1",
			envRequestedReference + "=oci://registry.example/schema:main",
		},
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
		Environment: []string{
			envOperationID + "=resolve-digest-redirect",
			envRequestedReference + "=oci://registry.example/schema@" + requestedDigest,
		},
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
		Environment: databaseEnvironment("plan-1"),
		Executor:    executor,
	})
	if result.CoordinationDigest != testCoordinationDigest() {
		t.Fatalf("coordination digest = %q, want %q", result.CoordinationDigest, testCoordinationDigest())
	}
	if result.Error != nil || result.Stdout != plan || result.PlanContentDigest != sha256Digest([]byte(plan)) ||
		result.PlanOutcome != PlanOutcomeChanges {
		t.Fatalf("Run() = %#v", result)
	}
}

func TestExecutablePlanSizeBoundary(t *testing.T) {
	// This test intentionally exercises multi-megabyte buffers serially. Its
	// exact-limit plan is dominated by '<' bytes, forcing encoding/json's
	// worst-case six-byte HTML escape in the framed result.
	exactPlan := exactSizePlanDocument(t, int(DefaultMaxPlanBytes))
	if exactPlan[len(exactPlan)-1] != '\n' {
		t.Fatal("exact-limit plan does not count its trailing newline")
	}

	planExecutor := &scriptedExecutor{t: t, responses: stablePlanResponses(t, exactPlan)}
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: databaseEnvironment("plan-exact-limit"),
		Executor:    planExecutor,
	})
	if result.Error != nil || result.PlanOutcome != PlanOutcomeChanges ||
		len(result.Stdout) != int(DefaultMaxPlanBytes) || result.PlanContentDigest != sha256Digest([]byte(exactPlan)) {
		t.Fatalf("exact-limit Run(Plan) = error %#v, outcome %q, stdout bytes %d, digest %q",
			result.Error, result.PlanOutcome, len(result.Stdout), result.PlanContentDigest)
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
	if parsed.Stdout != exactPlan || parsed.PlanContentDigest != result.PlanContentDigest {
		t.Fatal("frame round trip changed exact-limit plan bytes")
	}

	planDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(planDir, "000.plan"), []byte(parsed.Stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	applyExecutor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout: string(planApplyOutput(mustDecodePlan(t, exactPlan))),
	}}}
	applyEnvironment := append(databaseEnvironment("apply-exact-limit"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+parsed.PlanContentDigest,
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
	)
	applyResult := Run(context.Background(), Config{
		Operation:   OperationApply,
		Environment: applyEnvironment,
		Executor:    applyExecutor,
		TempDir:     t.TempDir(),
	})
	if applyResult.Error != nil || !applyResult.MutationStarted || applyResult.Uncertain ||
		applyResult.PlanContentDigest != parsed.PlanContentDigest {
		t.Fatalf("exact-limit Run(Apply) = %#v", applyResult)
	}

	oversizedPlan := exactSizePlanDocument(t, int(DefaultMaxPlanBytes)+1)
	oversizedExecutor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: oversizedPlan}}}
	oversizedResult := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: databaseEnvironment("plan-over-limit"),
		Executor:    oversizedExecutor,
	})
	if oversizedResult.Error == nil || oversizedResult.Error.Code != "invalid_plan_output" ||
		oversizedResult.Stdout != "" || oversizedResult.PlanContentDigest != "" || oversizedResult.PlanOutcome != "" {
		t.Fatalf("limit+1 Run(Plan) = %#v", oversizedResult)
	}
	if oversizedResult.Truncation == nil || !oversizedResult.Truncation.Stdout ||
		oversizedResult.Truncation.StdoutBytesDropped != 1 || len(oversizedExecutor.calls) != 1 {
		t.Fatalf("limit+1 truncation = %#v, calls = %d; want one-byte fail-closed refusal",
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
			test.config.Environment = databaseEnvironment("invalid-size-contract")
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
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: first}, {stdout: second}}}
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: databaseEnvironment("plan-unstable"), Executor: executor,
	})
	if result.Error == nil || result.Error.Code != "unstable_plan" || result.Stdout != "" || result.PlanOutcome != "" {
		t.Fatalf("Run() = %#v", result)
	}
	if len(executor.calls) != 2 {
		t.Fatalf("commands = %d, want two independent plan reads", len(executor.calls))
	}
}

func TestPlanRejectsMixedChangesAndNoChangesSnapshots(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE example (id bigint);")
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: planNoChangesOutput}, {stdout: plan}}}
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: databaseEnvironment("plan-mixed"), Executor: executor,
	})
	if result.Error == nil || result.Error.Code != "unstable_plan" || result.PlanOutcome != "" {
		t.Fatalf("Run() = %#v", result)
	}
}

func TestPlanValidatesDialectAgainstConfiguredDatabaseEngine(t *testing.T) {
	t.Parallel()

	plan := strings.Replace(validPlanDocument("CREATE TABLE example (id bigint);"), `"dialect":"postgres"`, `"dialect":"mysql"`, 1)
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: plan}, {stdout: plan}}}
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: databaseEnvironment("plan-wrong-dialect"), Executor: executor,
	})
	if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.Stdout != "" || len(executor.calls) != 2 {
		t.Fatalf("Run() = %#v, calls = %d", result, len(executor.calls))
	}
}

func TestPlanRequiresNativeDryRunToMatchReviewedStatements(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE example (id bigint);")
	responses := stablePlanResponses(t, plan)
	responses[2].stdout = "Planned schema changes:\nCREATE TABLE other (id bigint);\n"
	executor := &scriptedExecutor{t: t, responses: responses}
	result := Run(context.Background(), Config{
		Operation: OperationPlan, Environment: databaseEnvironment("plan-validation-mismatch"), Executor: executor,
	})
	if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.Stdout != "" || result.PlanContentDigest != "" {
		t.Fatalf("Run() = %#v", result)
	}
	if len(executor.calls) != 3 {
		t.Fatalf("commands = %d, want two plan reads and one native dry-run", len(executor.calls))
	}
}

func TestPlanRejectsNativeDryRunDiagnosticsAndTruncation(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE example (id bigint);")
	for _, test := range []struct {
		name          string
		validation    scriptedResponse
		maxResultSize int64
	}{
		{
			name:       "diagnostics",
			validation: scriptedResponse{stdout: string(planDryRunOutput(mustDecodePlan(t, plan))), stderr: "warning\n"},
		},
		{
			name:          "truncation",
			validation:    scriptedResponse{stdout: strings.Repeat("x", 1024)},
			maxResultSize: 512,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			responses := []scriptedResponse{{stdout: plan}, {stdout: plan}, test.validation}
			executor := &scriptedExecutor{t: t, responses: responses}
			result := Run(context.Background(), Config{
				Operation: OperationPlan, Environment: databaseEnvironment("plan-validation-" + test.name),
				Executor: executor, MaxResultBytes: test.maxResultSize, MaxPlanBytes: test.maxResultSize,
			})
			if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.Stdout != "" {
				t.Fatalf("Run() = %#v", result)
			}
			if test.name == "truncation" && (result.Truncation == nil || !result.Truncation.Stdout) {
				t.Fatalf("truncation = %#v", result.Truncation)
			}
		})
	}
}

func TestPlanWithCredentialSubstringFailsWithoutEmittingMutatedPayload(t *testing.T) {
	t.Parallel()

	secret := "credential-substring"
	plan := validPlanDocument("COMMENT ON TABLE example IS '" + secret + "';")
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: plan}, {stdout: plan}}}
	environment := append(databaseEnvironment("plan-secret"), EnvOCIToken+"="+secret)
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: environment,
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
		Environment: append(databaseEnvironment("observe-1"), "PTAH_EXCLUDE=audit.*,\"legacy,archive\""),
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
		Operation: OperationObserve, Environment: databaseEnvironment("observe-bounded-findings"),
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

func TestObserveRejectsUnknownIdentifierFindingCategory(t *testing.T) {
	t.Parallel()

	report := `{"drift":true,"failed":true,"failure_threshold":"all","highest_severity":"warning","dialect":"postgres","findings":[{"category":"private_schema_name","count":1,"severity":"warning"}],"diff":{}}`
	result := Run(context.Background(), Config{
		Operation: OperationObserve, Environment: databaseEnvironment("observe-unknown-finding"),
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
		Operation: OperationObserve, Environment: databaseEnvironment("observe-inconsistent-finding-severity"),
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
		Operation: OperationObserve, Environment: databaseEnvironment("observe-converged-safe"), Executor: executor,
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
		Operation: OperationObserve, Environment: databaseEnvironment("observe-real-safe-drift"), Executor: executor,
	})
	if result.Error != nil || result.ChildExitCode != 0 || result.DriftReportDigest == "" ||
		!result.ObservedDrift || result.HighestDriftSeverity != "safe" || result.DriftFindingCount != 1 {
		t.Fatalf("Run() = %#v, want real drift severity preserved", result)
	}
	if _, err := MarshalFrame(result); err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
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
				Operation: OperationObserve, Environment: databaseEnvironment(operationID), Executor: executor,
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
		Operation: OperationObserve, Environment: databaseEnvironment("observe-private-failure"),
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
		Environment: []string{
			envOperationID + "=redact-1",
			envDatabaseURL + "=" + databaseURL,
			envCoordinationDigest + "=" + testCoordinationDigest(),
			"PTAH_DEV_URL=" + devURL,
			"PTAH_OCI_PASSWORD=" + registryPassword,
			"PTAH_OCI_TOKEN=" + registryToken,
			envExpectedDatabaseEngine + "=PostgreSQL",
		},
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
		Environment: []string{
			envOperationID + "=bounded-1",
			envRequestedReference + "=oci://registry.example/schema:main",
		},
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
		stdout: string(planApplyOutput(mustDecodePlan(t, string(content)))),
		inspect: func(spec CommandSpec) {
			if !reflect.DeepEqual(spec.Args[:2], []string{"schema", "apply"}) {
				t.Fatalf("apply args = %v", spec.Args)
			}
			tempPlanPath = spec.Args[3]
			got, err := os.ReadFile(tempPlanPath)
			if err != nil {
				t.Fatalf("read temporary plan: %v", err)
			}
			if !bytes.Equal(got, content) {
				t.Fatalf("temporary plan = %q, want %q", got, content)
			}
			if _, present := environmentMap(spec.Env)[envSchemaFile]; present {
				t.Fatalf("exact-plan apply received %s", envSchemaFile)
			}
		},
	}}}
	environment := append(databaseEnvironment("apply-1"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+wantedDigest,
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
		envSchemaFile+"=oci://registry.example/schema@sha256:"+strings.Repeat("c", 64),
	)
	result := Run(context.Background(), Config{
		Operation:   OperationApply,
		Environment: environment,
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

func TestApplyValidatesButNeverPublishesNativeSQLTranscript(t *testing.T) {
	t.Parallel()

	const sensitiveLiteral = "operator-private-schema-literal"
	plan := []byte(validPlanDocument("CREATE TABLE private_defaults (value text DEFAULT '" + sensitiveLiteral + "')"))
	decoded := mustDecodePlan(t, string(plan))
	planDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	diagnostics := &bytes.Buffer{}
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: string(planApplyOutput(decoded))}}}
	environment := append(databaseEnvironment("apply-private-transcript"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+sha256Digest(plan),
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
	)
	result := Run(context.Background(), Config{
		Operation: OperationApply, Environment: environment, Executor: executor, Diagnostics: diagnostics,
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
	tests := []struct {
		name      string
		response  scriptedResponse
		errorCode string
	}{
		{
			name: "child stderr",
			response: scriptedResponse{
				stderr:   "migration failed\nSQL: CREATE TABLE private_defaults (value text DEFAULT '" + sensitiveLiteral + "')\n",
				exitCode: 1,
			},
			errorCode: "child_exit",
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
				Operation: OperationApply, Environment: environment, Executor: executor, Diagnostics: diagnostics,
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

func TestApplyRejectsUnexpectedNativeTranscriptAsUncertain(t *testing.T) {
	t.Parallel()

	plan := []byte(validPlanDocument("CREATE TABLE transcript_guard (id bigint)"))
	planDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: "unexpected success output\n"}}}
	environment := append(databaseEnvironment("apply-transcript-mismatch"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+sha256Digest(plan),
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
	)
	result := Run(context.Background(), Config{Operation: OperationApply, Environment: environment, Executor: executor})
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
	result := Run(context.Background(), Config{Operation: OperationApply, Environment: environment, Executor: executor})
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
				executor.responses = []scriptedResponse{{stdout: string(planApplyOutput(mustDecodePlan(t, string(plan))))}}
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
			}
			result := Run(context.Background(), Config{Operation: OperationApply, Environment: environment, Executor: executor})
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
			result := Run(context.Background(), Config{Operation: test.operation, Environment: environment, Executor: executor})
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
					Environment: []string{
						envOperationID + "=mysql-connection-sql",
						envDatabaseURL + "=" + databaseURL,
						envCoordinationDigest + "=" + testCoordinationDigest(),
					},
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
				Environment: environment,
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
	}
	result := Run(context.Background(), Config{
		Operation:   OperationApply,
		Environment: environment,
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
	result := Run(context.Background(), Config{Operation: OperationApply, Environment: environment, Executor: executor})
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
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{exitCode: 2, stderr: "connection lost"}}}
	environment := append(databaseEnvironment("apply-uncertain"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+sha256Digest(plan),
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
	)
	result := Run(context.Background(), Config{Operation: OperationApply, Environment: environment, Executor: executor})
	if result.Error == nil || result.Error.Code != "child_exit" || !result.MutationStarted || !result.Uncertain {
		t.Fatalf("Run() = %#v, want an uncertain dispatched mutation", result)
	}
}

func TestApplyNativeStalePlanAfterDispatchStillHasUnknownOutcome(t *testing.T) {
	t.Parallel()

	planDir := t.TempDir()
	plan := []byte(validPlanDocument("SELECT 1"))
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	fromFingerprint := "sha256:" + strings.Repeat("a", 64)
	databaseFingerprint := "sha256:" + strings.Repeat("c", 64)
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stderr:   observationProbeDiagnostic(fromFingerprint, databaseFingerprint),
		exitCode: 2,
	}}}
	environment := append(databaseEnvironment("apply-native-stale"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+sha256Digest(plan),
		envExpectedCoordination+"="+testCoordinationDigest(),
		envExpectedTargetDigest+"="+databaseTargetDigest(t),
	)
	result := Run(context.Background(), Config{Operation: OperationApply, Environment: environment, Executor: executor})
	if result.Error == nil || result.Error.Code != "stale_plan" || !result.MutationStarted || !result.Uncertain {
		t.Fatalf("Run() = %#v", result)
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
		want := []string{"schema", "plan", "--dry-run", "--exclude=audit.*", "--exclude=legacy,archive"}
		if !reflect.DeepEqual(spec.Args, want) {
			t.Fatalf("plan args = %q, want %q", spec.Args, want)
		}
		if _, present := environmentMap(spec.Env)["PTAH_EXCLUDE"]; present {
			t.Fatal("encoded exclusion adapter leaked to the Ptah child")
		}
	}
	responses := stablePlanResponses(t, plan)
	responses[0].inspect = inspectPlan
	responses[1].inspect = inspectPlan
	responses[2].inspect = func(spec CommandSpec) {
		want := []string{"schema", "apply", "--plan", spec.Args[3], "--auto-approve", "--dry-run"}
		if !reflect.DeepEqual(spec.Args, want) {
			t.Fatalf("plan validation args = %q, want %q", spec.Args, want)
		}
		values := environmentMap(spec.Env)
		for _, key := range []string{envSchemaFile, "PTAH_DEV_URL", "PTAH_EXCLUDE"} {
			if _, present := values[key]; present {
				t.Fatalf("plan-only environment key %s reached native plan validation", key)
			}
		}
	}
	executor := &scriptedExecutor{t: t, responses: responses}
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: append(databaseEnvironment("plan-scope"), "PTAH_EXCLUDE=\"legacy,archive\",audit.*"),
		Executor:    executor,
	})
	if result.Error != nil || result.PlanOutcome != PlanOutcomeChanges {
		t.Fatalf("Run() = %#v", result)
	}
}

func TestPlanRejectsOutputThatDoesNotBindRequestedScope(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("SELECT 1")
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: plan}, {stdout: plan}}}
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: append(databaseEnvironment("plan-wrong-scope"), "PTAH_EXCLUDE=audit.*"),
		Executor:    executor,
	})
	if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.Stdout != "" {
		t.Fatalf("Run() = %#v", result)
	}
}

func TestPlanNoChangesRequiresExactSilentContract(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		responses []scriptedResponse
		valid     bool
	}{
		{name: "exact", responses: []scriptedResponse{{stdout: planNoChangesOutput}, {stdout: planNoChangesOutput}}, valid: true},
		{name: "changed text", responses: []scriptedResponse{{stdout: "Schema is synced, no changes to be made\n"}, {stdout: "Schema is synced, no changes to be made\n"}}},
		{name: "diagnostic", responses: []scriptedResponse{{stdout: planNoChangesOutput, stderr: "warning: selector matched nothing\n"}}},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t, responses: test.responses}
			result := Run(context.Background(), Config{
				Operation: OperationPlan, Environment: databaseEnvironment("plan-no-change"), Executor: executor,
			})
			if test.valid {
				if result.Error != nil || result.PlanOutcome != PlanOutcomeNoChanges || result.Stdout != "" || result.PlanContentDigest != "" {
					t.Fatalf("Run() = %#v", result)
				}
				return
			}
			if result.Error == nil || result.Error.Code != "invalid_plan_output" || result.PlanOutcome != "" {
				t.Fatalf("Run() = %#v", result)
			}
		})
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
			Operation: OperationObserve, Environment: databaseEnvironment("observe-invalid-report"), Executor: executor,
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
		Environment: []string{
			envOperationID + "=verify-1",
			envRequestedReference + "=" + requested,
			envResolvedReference + "=" + resolved,
			envVerificationPolicy + "=" + policyPath,
			envExpectedArtifactType + "=" + artifactType,
		},
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
				Environment: []string{
					envOperationID + "=verify-tag-pin-policy",
					envRequestedReference + "=" + requested,
					envResolvedReference + "=" + resolved,
					envVerificationPolicy + "=" + policyPath,
					envExpectedArtifactType + "=" + artifactType,
				},
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
			Environment: []string{
				envOperationID + "=verify-digest-pin-policy",
				envRequestedReference + "=" + resolved,
				envResolvedReference + "=" + resolved,
				envVerificationPolicy + "=" + policyPath,
				envExpectedArtifactType + "=" + artifactType,
			},
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
				Environment: []string{
					envOperationID + "=verify-invalid-requested-binding",
					envRequestedReference + "=" + test.requested,
					envResolvedReference + "=" + resolved,
					envVerificationPolicy + "=" + policyPath,
					envExpectedArtifactType + "=" + dataplane.SchemaArtifactType,
				},
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
		result := Run(context.Background(), Config{Operation: OperationVerify, Environment: baseEnvironment, Executor: executor})
		if result.Error == nil || result.Error.Code != "stale_source" || len(executor.calls) != 1 {
			t.Fatalf("Run() = %#v, commands = %d", result, len(executor.calls))
		}
	})

	t.Run("wrong artifact type", func(t *testing.T) {
		executor := &scriptedExecutor{t: t, responses: []scriptedResponse{
			{stdout: fmt.Sprintf(`{"reference":%q,"digest":%q,"satisfied":[],"findings":[]}`, "oci://registry.example/schema@"+digestA, digestA)},
			{stdout: inspectReport("oci://registry.example/schema@"+digestA, digestA, "application/octet-stream")},
		}}
		result := Run(context.Background(), Config{Operation: OperationVerify, Environment: baseEnvironment, Executor: executor})
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
		Environment: []string{
			envOperationID + "=verify-refused",
			envRequestedReference + "=" + resolved,
			envResolvedReference + "=" + resolved,
			envVerificationPolicy + "=" + policyPath,
			envExpectedArtifactType + "=" + dataplane.SchemaArtifactType,
		},
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
		Environment: []string{
			envOperationID + "=verify-combined-refusal",
			envRequestedReference + "=oci://registry.example/schema:stable",
			envResolvedReference + "=" + resolved,
			envVerificationPolicy + "=" + policyPath,
			envExpectedArtifactType + "=" + dataplane.SchemaArtifactType,
		},
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
		Environment: []string{
			envOperationID + "=verify-malformed-refusal",
			envRequestedReference + "=oci://registry.example/schema@" + digest,
			envResolvedReference + "=oci://registry.example/schema@" + digest,
			envVerificationPolicy + "=" + policyPath,
			envExpectedArtifactType + "=" + dataplane.SchemaArtifactType,
		},
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
				Operation: test.operation, Environment: environment,
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
	}
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

func stablePlanResponses(t *testing.T, plan string) []scriptedResponse {
	t.Helper()
	decoded := mustDecodePlan(t, plan)
	return []scriptedResponse{
		{stdout: plan},
		{stdout: plan},
		{stdout: string(planDryRunOutput(decoded))},
	}
}

func mustDecodePlan(t *testing.T, plan string) dataplane.PlanFile {
	t.Helper()
	decoded, err := dataplane.DecodePlan([]byte(plan), "PostgreSQL")
	if err != nil {
		t.Fatalf("decode plan fixture: %v", err)
	}
	return decoded
}

func observationProbeDiagnostic(candidate, databaseFingerprint string) string {
	return "error: pre-planned migration is stale: the target database schema does not match the plan's source fingerprint " +
		"(plan " + candidate + ", database " + databaseFingerprint +
		"); the database changed since the plan was computed, so re-run `schema plan` " +
		"against the current database and review the fresh plan\n"
}
