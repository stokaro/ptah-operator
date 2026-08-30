package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
		VerificationPolicyPath: "/policy/verification.json",
		PlanPath:               "/tmp/approved-plan.hcl",
	}
	tests := []struct {
		name      string
		operation Operation
		want      []string
	}{
		{name: "resolve", operation: OperationResolve, want: []string{"oci", "resolve", inputs.RequestedReference, "--format", "json"}},
		{name: "verify mutable request", operation: OperationVerify, want: []string{"oci", "verify", inputs.RequestedReference, "--policy", inputs.VerificationPolicyPath, "--format", "json"}},
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

func TestResolveRecordsStrictTopLevelDigest(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("e", 64)
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: fmt.Sprintf(`{"reference":"oci://registry.example/schema:main","digest":%q}`, digest)}}}
	result := Run(context.Background(), Config{
		Operation: OperationResolve,
		Environment: []string{
			envOperationID + "=resolve-1",
			envRequestedReference + "=oci://registry.example/schema:main",
		},
		Executor: executor,
	})
	if result.Error != nil || result.ResolvedDigest != digest {
		t.Fatalf("Run() = %#v", result)
	}
	childValues := environmentMap(executor.calls[0].Env)
	for _, key := range []string{EnvOperationID, EnvRequestedReference, EnvResolvedReference, EnvVerificationPolicy, EnvExpectedArtifactType, EnvPlanDir, EnvExpectedPlanContentDigest} {
		if _, present := childValues[key]; present {
			t.Fatalf("runner-only environment key %s was forwarded to the child", key)
		}
	}
}

func TestPlanRecordsExactContentDigest(t *testing.T) {
	t.Parallel()

	plan := validPlanDocument("CREATE TABLE example (id bigint);")
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: plan}}}
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: databaseEnvironment("plan-1"),
		Executor:    executor,
	})
	if result.Error != nil || result.Stdout != plan || result.PlanContentDigest != sha256Digest([]byte(plan)) {
		t.Fatalf("Run() = %#v", result)
	}
}

func TestPlanWithCredentialSubstringFailsWithoutEmittingMutatedPayload(t *testing.T) {
	t.Parallel()

	secret := "credential-substring"
	plan := validPlanDocument("COMMENT ON TABLE example IS '" + secret + "';")
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: plan}}}
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

	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
		stdout:   `{"drift":true}`,
		exitCode: 1,
	}, {stdout: `{"schemas":[{"name":"public"}]}`}}}
	result := Run(context.Background(), Config{
		Operation:   OperationObserve,
		Environment: databaseEnvironment("observe-1"),
		Executor:    executor,
	})
	if result.ChildExitCode != 1 || result.Error != nil {
		t.Fatalf("Run() = %#v, want expected drift exit 1 without runner error", result)
	}
	if len(executor.calls) != 2 {
		t.Fatalf("commands = %d, want drift plus live inspect", len(executor.calls))
	}
	if result.ObservedStateFingerprint == "" {
		t.Fatal("drift-present observation lacks a live-state fingerprint")
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	parsed, err := ParseResultFor(frame, OperationObserve, "observe-1")
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}
	if parsed.ChildExitCode != 1 || parsed.Stdout != `{"drift":true}` {
		t.Fatalf("parsed result = %#v", parsed)
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
			"PTAH_DEV_URL=" + devURL,
			"PTAH_OCI_PASSWORD=" + registryPassword,
			"PTAH_OCI_TOKEN=" + registryToken,
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
	if !strings.Contains(combined, RedactionMarker) {
		t.Fatalf("sanitized output = %q, want redaction marker", combined)
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
	if result.Stdout != strings.Repeat("s", 8) {
		t.Fatalf("stdout = %q, want retained 8-byte prefix", result.Stdout)
	}
	if result.Error == nil || result.Error.Code != "output_truncated" {
		t.Fatalf("error = %#v, want output_truncated", result.Error)
	}
	if result.Truncation == nil || !result.Truncation.Stdout || result.Truncation.StdoutBytesDropped != 56 || !result.Truncation.Stderr || result.Truncation.StderrBytesDropped != 32 {
		t.Fatalf("truncation = %#v", result.Truncation)
	}
	if strings.Count(diagnostics.String(), "e") < 8 || !strings.Contains(diagnostics.String(), "stderr truncated") {
		t.Fatalf("diagnostics = %q", diagnostics.String())
	}
}

func TestApplyReconstructsChunksInLexicalOrderAndRemovesTempPlan(t *testing.T) {
	t.Parallel()

	planDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(planDir, "chunk-010"), []byte("world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), []byte("hello "), 0o600); err != nil {
		t.Fatal(err)
	}
	content := []byte("hello world\n")
	wantedDigest := sha256Digest(content)
	var tempPlanPath string
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{inspect: func(spec CommandSpec) {
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
	}}}}
	environment := append(databaseEnvironment("apply-1"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+wantedDigest,
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

func TestApplyFailureIsUncertainAndMustNotBeReplayed(t *testing.T) {
	t.Parallel()

	planDir := t.TempDir()
	plan := []byte("apply this plan")
	if err := os.WriteFile(filepath.Join(planDir, "chunk-001"), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{exitCode: 2, stderr: "connection lost"}}}
	environment := append(databaseEnvironment("apply-uncertain"),
		envPlanDir+"="+planDir,
		envExpectedPlanDigest+"="+sha256Digest(plan),
	)
	result := Run(context.Background(), Config{Operation: OperationApply, Environment: environment, Executor: executor})
	if result.Error == nil || result.Error.Code != "child_exit" || !result.MutationStarted || !result.Uncertain {
		t.Fatalf("Run() = %#v, want an uncertain dispatched mutation", result)
	}
}

func TestObserveDerivesCanonicalFingerprintWhenNoDrift(t *testing.T) {
	t.Parallel()

	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{
		{stdout: `{"drift":false}`},
		{stdout: "{\n  \"b\": 2, \"a\": 1\n}\n"},
	}}
	environment := append(databaseEnvironment("observe-clean"), envSchemaFile+"=oci://registry.example/schema@sha256:"+strings.Repeat("d", 64))
	result := Run(context.Background(), Config{
		Operation:   OperationObserve,
		Environment: environment,
		Executor:    executor,
	})
	wantedFingerprint := sha256Digest([]byte(`{"a":1,"b":2}`))
	if result.Error != nil || result.ObservedStateFingerprint != wantedFingerprint {
		t.Fatalf("Run() = %#v, want fingerprint %s", result, wantedFingerprint)
	}
	if len(executor.calls) != 2 || !reflect.DeepEqual(executor.calls[1].Args, []string{"schema", "inspect", "--format", "json"}) {
		t.Fatalf("commands = %#v", executor.calls)
	}
	if environmentMap(executor.calls[0].Env)[envSchemaFile] == "" {
		t.Fatalf("drift command did not receive %s", envSchemaFile)
	}
	if _, present := environmentMap(executor.calls[1].Env)[envSchemaFile]; present {
		t.Fatalf("live schema inspect received %s", envSchemaFile)
	}
}

func TestVerifyBindsMutableRequestToResolvedDigestAndArtifactType(t *testing.T) {
	t.Parallel()

	policyPath := filepath.Join(t.TempDir(), "policy.json")
	policy := []byte(`{"require":["signature"]}`)
	if err := os.WriteFile(policyPath, policy, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	requested := "oci://registry.example/team/schema:main"
	resolved := "oci://registry.example/team/schema@" + digest
	artifactType := "application/vnd.stokaro.ptah.schema.v1"
	var snapshottedPolicyPath string
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{
		{stdout: fmt.Sprintf(`{"reference":%q,"digest":%q,"satisfied":[],"findings":[]}`, requested, digest), inspect: func(spec CommandSpec) {
			snapshottedPolicyPath = spec.Args[4]
			gotPolicy, err := os.ReadFile(snapshottedPolicyPath)
			if err != nil || !bytes.Equal(gotPolicy, policy) {
				t.Fatalf("snapshotted policy = %q, %v", gotPolicy, err)
			}
		}},
		{stdout: fmt.Sprintf(`{"reference":%q,"digest":%q,"artifact_type":%q}`, resolved, digest, artifactType)},
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
	if executor.calls[0].Args[2] != requested {
		t.Fatalf("verify used %q, want mutable requested reference %q", executor.calls[0].Args[2], requested)
	}
	if executor.calls[1].Args[2] != resolved || executor.calls[1].Args[len(executor.calls[1].Args)-1] != "--no-referrers" {
		t.Fatalf("inspect args = %v", executor.calls[1].Args)
	}
	if snapshottedPolicyPath == "" || snapshottedPolicyPath == policyPath {
		t.Fatalf("verify policy path = %q, want an immutable snapshot", snapshottedPolicyPath)
	}
	if _, err := os.Stat(snapshottedPolicyPath); !os.IsNotExist(err) {
		t.Fatalf("snapshotted policy still exists or stat failed unexpectedly: %v", err)
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
		executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: fmt.Sprintf(`{"digest":%q}`, digestB)}}}
		result := Run(context.Background(), Config{Operation: OperationVerify, Environment: baseEnvironment, Executor: executor})
		if result.Error == nil || result.Error.Code != "stale_source" || len(executor.calls) != 1 {
			t.Fatalf("Run() = %#v, commands = %d", result, len(executor.calls))
		}
	})

	t.Run("wrong artifact type", func(t *testing.T) {
		executor := &scriptedExecutor{t: t, responses: []scriptedResponse{
			{stdout: fmt.Sprintf(`{"digest":%q}`, digestA)},
			{stdout: fmt.Sprintf(`{"digest":%q,"artifact_type":"application/octet-stream"}`, digestA)},
		}}
		result := Run(context.Background(), Config{Operation: OperationVerify, Environment: baseEnvironment, Executor: executor})
		if result.Error == nil || result.Error.Code != "artifact_type_mismatch" || len(executor.calls) != 2 {
			t.Fatalf("Run() = %#v, commands = %d", result, len(executor.calls))
		}
	})
}

func TestTargetIdentityDigestIgnoresPasswordRotationButBindsEndpointAndDatabase(t *testing.T) {
	t.Parallel()

	base, err := TargetIdentityDigest("postgres://app:old-password@DB.Example:5432/accounts?schema=public&connect_timeout=5&password=query-old")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := TargetIdentityDigest("postgres://app:new-password@db.example/accounts?schema=public&connect_timeout=60&password=query-new")
	if err != nil {
		t.Fatal(err)
	}
	if base != rotated {
		t.Fatalf("password-only rotation changed identity: %s != %s", base, rotated)
	}
	for name, changedURL := range map[string]string{
		"endpoint": "postgres://app:new-password@other.example/accounts?schema=public",
		"database": "postgres://app:new-password@db.example/ledger?schema=public",
		"scope":    "postgres://app:new-password@db.example/accounts?schema=private",
		"username": "postgres://other:new-password@db.example/accounts?schema=public",
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

	optionsBase, err := TargetIdentityDigest("postgres://app:password@db.example/accounts?options=-c%20search_path%3Dpublic%20-c%20password%3Done")
	if err != nil {
		t.Fatal(err)
	}
	optionsRotated, err := TargetIdentityDigest("postgres://app:password@db.example/accounts?options=-c%20search_path%3Dpublic%20-c%20password%3Dtwo")
	if err != nil {
		t.Fatal(err)
	}
	if optionsBase != optionsRotated {
		t.Fatal("password rotation inside options changed target identity")
	}
	optionsScopeChanged, err := TargetIdentityDigest("postgres://app:password@db.example/accounts?options=-c%20search_path%3Dprivate%20-c%20password%3Dtwo")
	if err != nil {
		t.Fatal(err)
	}
	if optionsScopeChanged == optionsBase {
		t.Fatal("search_path change inside options did not change target identity")
	}
}

func TestTargetIdentityDigestPreservesEncodedPathAndRepeatedScopeOrder(t *testing.T) {
	t.Parallel()

	encodedSlash, err := TargetIdentityDigest("postgres://db.example/db%2Ftenant")
	if err != nil {
		t.Fatal(err)
	}
	literalSlash, err := TargetIdentityDigest("postgres://db.example/db/tenant")
	if err != nil {
		t.Fatal(err)
	}
	if encodedSlash == literalSlash {
		t.Fatal("encoded and literal database path separators produced the same target identity")
	}

	firstThenSecond, err := TargetIdentityDigest("postgres://db.example/app?search_path=first&search_path=second")
	if err != nil {
		t.Fatal(err)
	}
	secondThenFirst, err := TargetIdentityDigest("postgres://db.example/app?search_path=second&search_path=first")
	if err != nil {
		t.Fatal(err)
	}
	if firstThenSecond == secondThenFirst {
		t.Fatal("reordered repeated scope values produced the same target identity")
	}
}

func TestTargetIdentityDigestPreservesQuotedOptionValues(t *testing.T) {
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
		t.Fatal("different quoted scope values produced the same target identity")
	}
}

func databaseEnvironment(operationID string) []string {
	return []string{
		envOperationID + "=" + operationID,
		envDatabaseURL + "=postgres://app:secret@db.example/app",
	}
}

func validPlanDocument(sql string) string {
	return fmt.Sprintf(
		`{"format_version":1,"name":"operator-plan","dialect":"postgres","from_fingerprint":"sha256:from","to_fingerprint":"sha256:to","destructive":false,"statements":[{"sql":%q,"severity":"safe","reason":"schema change"}]}`+"\n",
		sql,
	)
}
