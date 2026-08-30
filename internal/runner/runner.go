package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

const (
	DefaultMaxResultBytes int64 = 8 << 20
	maxErrorMessageBytes        = 4 << 10
)

var digestPattern = regexp.MustCompile(`(?i)sha256:[0-9a-f]{64}`)

type Config struct {
	Operation      Operation
	PtahBinary     string
	MaxResultBytes int64
	MaxPlanBytes   int64
	Environment    []string
	Diagnostics    io.Writer
	Executor       Executor
	TempDir        string
}

type commandOutcome struct {
	exitCode int
	err      error
	stdout   *boundedBuffer
	stderr   *boundedBuffer
}

type verificationReport struct {
	Digest string `json:"digest"`
}

type resolveReport struct {
	Digest string `json:"digest"`
}

type artifactInspection struct {
	Digest       string `json:"digest"`
	ArtifactType string `json:"artifact_type"`
}

// Run executes one fixed operation and always returns a frameable result.
// Child failures are represented in Result rather than returned as Go errors.
func Run(ctx context.Context, config Config) Result {
	environment := config.Environment
	if environment == nil {
		environment = os.Environ()
	}
	redactor := NewRedactor(environment)
	values := environmentMap(environment)
	inputs := InputsFromEnvironment(environment)
	result := Result{
		ProtocolVersion: ProtocolVersion,
		Operation:       config.Operation,
		OperationID:     inputs.OperationID,
		ChildExitCode:   -1,
		Stdout:          "",
	}

	if !config.Operation.Valid() {
		setResultError(&result, "invalid_operation", fmt.Errorf("unsupported operation %q", config.Operation), redactor, config.Diagnostics)
		return result
	}
	if inputs.OperationID == "" {
		// Keep the in-memory result useful to callers. It cannot be framed until
		// the required binding is supplied.
		setResultError(&result, "missing_operation_id", errors.New("PTAH_OPERATION_ID is required"), redactor, config.Diagnostics)
		return result
	}

	if databaseURL := values[envDatabaseURL]; databaseURL != "" {
		targetDigest, err := TargetIdentityDigest(databaseURL)
		if err != nil {
			setResultError(&result, "invalid_target", err, redactor, config.Diagnostics)
			return result
		}
		result.TargetIdentityDigest = targetDigest
	} else if operationNeedsDatabase(config.Operation) {
		setResultError(&result, "missing_target", errors.New("PTAH_DB_URL is required"), redactor, config.Diagnostics)
		return result
	}

	if config.PtahBinary == "" {
		config.PtahBinary = "ptah"
	}
	if config.MaxResultBytes <= 0 {
		config.MaxResultBytes = DefaultMaxResultBytes
	}
	if config.MaxPlanBytes <= 0 {
		config.MaxPlanBytes = DefaultMaxPlanBytes
	}
	if config.Diagnostics == nil {
		config.Diagnostics = io.Discard
	}
	if config.Executor == nil {
		config.Executor = OSExecutor{}
	}

	if config.Operation == OperationVerify {
		return runVerify(ctx, config, environment, inputs, redactor, result)
	}
	if config.Operation == OperationObserve {
		return runObserve(ctx, config, environment, inputs, redactor, result)
	}

	var cleanup func()
	if config.Operation == OperationApply {
		plan, planDigest, err := reconstructPlan(inputs.PlanDir, inputs.ExpectedPlanContentDigest, config.MaxPlanBytes)
		result.PlanContentDigest = planDigest
		if err != nil {
			code := "invalid_plan"
			if strings.Contains(err.Error(), "does not match") {
				code = "plan_digest_mismatch"
			}
			setResultError(&result, code, err, redactor, config.Diagnostics)
			return result
		}
		planFile, err := os.CreateTemp(config.TempDir, "ptah-plan-*.hcl")
		if err != nil {
			setResultError(&result, "prepare_plan", errors.New("create temporary plan file"), redactor, config.Diagnostics)
			return result
		}
		planPath := planFile.Name()
		cleanup = func() { _ = os.Remove(planPath) }
		defer cleanup()
		if _, err := planFile.Write(plan); err != nil {
			_ = planFile.Close()
			setResultError(&result, "prepare_plan", errors.New("write temporary plan file"), redactor, config.Diagnostics)
			return result
		}
		if err := planFile.Close(); err != nil {
			setResultError(&result, "prepare_plan", errors.New("close temporary plan file"), redactor, config.Diagnostics)
			return result
		}
		inputs.PlanPath = planPath
	}

	spec, err := BuildCommand(config.PtahBinary, config.Operation, inputs)
	if err != nil {
		setResultError(&result, "invalid_input", err, redactor, config.Diagnostics)
		return result
	}
	spec.Env = childEnvironment(environment)
	if config.Operation == OperationApply {
		// An exact, digest-checked plan is the only desired input to apply.
		spec.Env = environmentWithout(spec.Env, envSchemaFile)
	}
	if err := ensureNoCredentialsInArguments(spec.Args, values); err != nil {
		setResultError(&result, "credential_in_arguments", err, redactor, config.Diagnostics)
		return result
	}
	outcome := executeCommand(ctx, config, spec)
	if config.Operation == OperationApply {
		// Dispatching a mutating child is enough to require observation before
		// any retry. Even an executable-start ambiguity is handled fail-safe.
		result.MutationStarted = true
	}
	consumeOutcome(&result, outcome, redactor, config.Diagnostics, config.MaxResultBytes, config.Operation != OperationPlan)
	finishSingleCommandResult(&result, outcome, redactor, config.Diagnostics)
	if result.Error == nil {
		switch config.Operation {
		case OperationResolve:
			var resolved resolveReport
			if err := decodeSingleJSON(outcome.stdout.bytes(), &resolved); err != nil || resolved.Digest == "" {
				setResultError(&result, "invalid_resolve_output", errors.New("resolve output is missing a valid top-level digest"), redactor, config.Diagnostics)
			} else if digest, err := normalizeDigest(resolved.Digest); err != nil {
				setResultError(&result, "invalid_resolve_output", errors.New("resolve output contains an invalid top-level digest"), redactor, config.Diagnostics)
			} else {
				result.ResolvedDigest = digest
			}
		case OperationPlan:
			rawPlan := outcome.stdout.bytes()
			if !utf8.Valid(rawPlan) {
				setResultError(&result, "invalid_plan_output", errors.New("plan output is not valid UTF-8"), redactor, config.Diagnostics)
				break
			}
			var header struct {
				Dialect string `json:"dialect"`
			}
			if err := decodeSingleJSON(rawPlan, &header); err != nil {
				setResultError(&result, "invalid_plan_output", errors.New("plan output is not a valid plan document"), redactor, config.Diagnostics)
				break
			}
			if _, err := dataplane.DecodePlan(rawPlan, header.Dialect); err != nil {
				setResultError(&result, "invalid_plan_output", errors.New("plan output failed strict validation"), redactor, config.Diagnostics)
				break
			}
			if redactor.Redact(string(rawPlan)) != string(rawPlan) {
				setResultError(&result, "credential_leak", errors.New("plan output contains a protected credential or URL password"), redactor, config.Diagnostics)
				break
			}
			result.Stdout = string(rawPlan)
			if result.Stdout != "" {
				result.PlanContentDigest = sha256Digest(rawPlan)
			}
		}
	}
	if config.Operation == OperationApply && result.Error != nil {
		result.Uncertain = true
	}
	return result
}

func runVerify(ctx context.Context, config Config, environment []string, inputs Inputs, redactor Redactor, result Result) Result {
	if inputs.ResolvedReference == "" {
		setResultError(&result, "invalid_input", errors.New("PTAH_RESOLVED_REFERENCE is required"), redactor, config.Diagnostics)
		return result
	}
	if inputs.ExpectedArtifactType == "" {
		setResultError(&result, "invalid_input", errors.New("PTAH_EXPECTED_ARTIFACT_TYPE is required"), redactor, config.Diagnostics)
		return result
	}
	resolvedDigest, err := digestFromReference(inputs.ResolvedReference)
	if err != nil {
		setResultError(&result, "invalid_input", errors.New("PTAH_RESOLVED_REFERENCE does not contain a SHA-256 digest"), redactor, config.Diagnostics)
		return result
	}
	result.ResolvedDigest = resolvedDigest
	policyPath, policyDigest, cleanupPolicy, err := snapshotFile(
		inputs.VerificationPolicyPath,
		config.TempDir,
		"ptah-verification-policy",
		maxVerificationPolicyBytes,
	)
	if err != nil {
		setResultError(&result, "verification_policy", errors.New("read verification policy"), redactor, config.Diagnostics)
		return result
	}
	defer cleanupPolicy()
	inputs.VerificationPolicyPath = policyPath
	result.VerificationPolicyDigest = policyDigest

	verifySpec, err := BuildCommand(config.PtahBinary, OperationVerify, inputs)
	if err != nil {
		setResultError(&result, "invalid_input", err, redactor, config.Diagnostics)
		return result
	}
	verifySpec.Env = childEnvironment(environment)
	if err := ensureNoCredentialsInArguments(verifySpec.Args, environmentMap(environment)); err != nil {
		setResultError(&result, "credential_in_arguments", err, redactor, config.Diagnostics)
		return result
	}
	verifyOutcome := executeCommand(ctx, config, verifySpec)
	consumeOutcome(&result, verifyOutcome, redactor, config.Diagnostics, config.MaxResultBytes, true)
	if !outcomeSucceeded(&result, verifyOutcome, redactor, config.Diagnostics) {
		return result
	}

	var report verificationReport
	if err := decodeSingleJSON(verifyOutcome.stdout.bytes(), &report); err != nil || report.Digest == "" {
		setResultError(&result, "invalid_verification_output", errors.New("verification output is missing a valid top-level digest"), redactor, config.Diagnostics)
		return result
	}
	reportDigest, err := normalizeDigest(report.Digest)
	if err != nil || reportDigest != resolvedDigest {
		setResultError(&result, "stale_source", errors.New("verified source digest no longer matches the resolved source"), redactor, config.Diagnostics)
		return result
	}

	inspectSpec, err := buildInspectArtifactCommand(config.PtahBinary, inputs.ResolvedReference)
	if err != nil {
		setResultError(&result, "invalid_input", err, redactor, config.Diagnostics)
		return result
	}
	inspectSpec.Env = childEnvironment(environment)
	if err := ensureNoCredentialsInArguments(inspectSpec.Args, environmentMap(environment)); err != nil {
		setResultError(&result, "credential_in_arguments", err, redactor, config.Diagnostics)
		return result
	}
	inspectOutcome := executeCommand(ctx, config, inspectSpec)
	consumeOutcome(&result, inspectOutcome, redactor, config.Diagnostics, config.MaxResultBytes, false)
	if !outcomeSucceeded(&result, inspectOutcome, redactor, config.Diagnostics) {
		return result
	}

	var inspection artifactInspection
	if err := decodeSingleJSON(inspectOutcome.stdout.bytes(), &inspection); err != nil || inspection.Digest == "" || inspection.ArtifactType == "" {
		setResultError(&result, "invalid_artifact_output", errors.New("artifact inspection output is missing required top-level fields"), redactor, config.Diagnostics)
		return result
	}
	inspectionDigest, err := normalizeDigest(inspection.Digest)
	if err != nil || inspectionDigest != resolvedDigest {
		setResultError(&result, "stale_source", errors.New("inspected source digest no longer matches the resolved source"), redactor, config.Diagnostics)
		return result
	}
	result.ObservedArtifactType = inspection.ArtifactType
	if inspection.ArtifactType != inputs.ExpectedArtifactType {
		setResultError(&result, "artifact_type_mismatch", errors.New("artifact type does not match the required schema artifact type"), redactor, config.Diagnostics)
		return result
	}
	return result
}

func runObserve(ctx context.Context, config Config, environment []string, inputs Inputs, redactor Redactor, result Result) Result {
	driftSpec, err := BuildCommand(config.PtahBinary, OperationObserve, inputs)
	if err != nil {
		setResultError(&result, "invalid_input", err, redactor, config.Diagnostics)
		return result
	}
	driftSpec.Env = childEnvironment(environment)
	driftOutcome := executeCommand(ctx, config, driftSpec)
	consumeOutcome(&result, driftOutcome, redactor, config.Diagnostics, config.MaxResultBytes, true)
	if driftOutcome.err != nil {
		setResultError(&result, "execution_error", driftOutcome.err, redactor, config.Diagnostics)
		return result
	}
	if stdoutWasTruncated(&result) {
		setResultError(&result, "output_truncated", errors.New("ptah stdout exceeded the configured result limit"), redactor, config.Diagnostics)
		return result
	}
	if driftOutcome.exitCode != 0 && driftOutcome.exitCode != 1 {
		setResultError(&result, "child_exit", fmt.Errorf("ptah exited with code %d", driftOutcome.exitCode), redactor, config.Diagnostics)
		return result
	}

	driftExitCode := driftOutcome.exitCode
	inspectSpec := buildInspectSchemaCommand(config.PtahBinary)
	// schema inspect must fingerprint only the live database. Leaving a
	// desired source in the environment can inspect that source or create a
	// conflicting-input error instead.
	inspectSpec.Env = environmentWithout(childEnvironment(environment), envSchemaFile)
	inspectOutcome := executeCommand(ctx, config, inspectSpec)
	consumeOutcome(&result, inspectOutcome, redactor, config.Diagnostics, config.MaxResultBytes, false)
	// The exposed report and exit code always belong to schema drift. Schema
	// inspect is auxiliary evidence required for both converged and drifted
	// states, and its failures are represented by Error.
	result.ChildExitCode = driftExitCode
	if !outcomeSucceeded(&result, inspectOutcome, redactor, config.Diagnostics) {
		return result
	}
	fingerprint, err := fingerprintJSON(inspectOutcome.stdout.bytes())
	if err != nil {
		setResultError(&result, "invalid_observed_state", errors.New("schema inspection output is not valid JSON"), redactor, config.Diagnostics)
		return result
	}
	result.ObservedStateFingerprint = fingerprint
	return result
}

func executeCommand(ctx context.Context, config Config, spec CommandSpec) commandOutcome {
	stdout := newBoundedBuffer(config.MaxResultBytes)
	stderr := newBoundedBuffer(config.MaxResultBytes)
	exitCode, err := config.Executor.Execute(ctx, spec, stdout, stderr)
	return commandOutcome{exitCode: exitCode, err: err, stdout: stdout, stderr: stderr}
}

func consumeOutcome(result *Result, outcome commandOutcome, redactor Redactor, diagnostics io.Writer, maxResultBytes int64, exposeStdout bool) {
	result.ChildExitCode = outcome.exitCode
	stdoutDropped := outcome.stdout.dropped()
	stderrDropped := outcome.stderr.dropped()
	if exposeStdout {
		var sanitizedDropped int64
		result.Stdout, sanitizedDropped = sanitizedCapturedText(outcome.stdout.bytes(), redactor, stdoutDropped > 0, maxResultBytes)
		stdoutDropped += sanitizedDropped
	}
	sanitizedStderr, sanitizedStderrDropped := sanitizedCapturedText(outcome.stderr.bytes(), redactor, stderrDropped > 0, maxResultBytes)
	if diagnostics != nil {
		_, _ = io.WriteString(diagnostics, sanitizedStderr)
	}
	stderrDropped += sanitizedStderrDropped
	if stdoutDropped > 0 || stderrDropped > 0 {
		if result.Truncation == nil {
			result.Truncation = &TruncationMetadata{}
		}
		if stdoutDropped > 0 {
			result.Truncation.Stdout = true
			result.Truncation.StdoutBytesDropped += stdoutDropped
		}
		if stderrDropped > 0 {
			result.Truncation.Stderr = true
			result.Truncation.StderrBytesDropped += stderrDropped
			_, _ = io.WriteString(diagnostics, fmt.Sprintf("\nptah-runner: stderr truncated; %d bytes omitted\n", stderrDropped))
		}
	}
}

func finishSingleCommandResult(result *Result, outcome commandOutcome, redactor Redactor, diagnostics io.Writer) {
	if outcome.err != nil {
		setResultError(result, "execution_error", outcome.err, redactor, diagnostics)
		return
	}
	if outcome.exitCode != 0 {
		setResultError(result, "child_exit", fmt.Errorf("ptah exited with code %d", outcome.exitCode), redactor, diagnostics)
		return
	}
	if stdoutWasTruncated(result) {
		setResultError(result, "output_truncated", errors.New("ptah stdout exceeded the configured result limit"), redactor, diagnostics)
	}
}

func outcomeSucceeded(result *Result, outcome commandOutcome, redactor Redactor, diagnostics io.Writer) bool {
	if outcome.err != nil {
		setResultError(result, "execution_error", outcome.err, redactor, diagnostics)
		return false
	}
	if outcome.exitCode != 0 {
		setResultError(result, "child_exit", fmt.Errorf("ptah exited with code %d", outcome.exitCode), redactor, diagnostics)
		return false
	}
	if stdoutWasTruncated(result) {
		setResultError(result, "output_truncated", errors.New("ptah stdout exceeded the configured result limit"), redactor, diagnostics)
		return false
	}
	return true
}

func setResultError(result *Result, code string, err error, redactor Redactor, diagnostics io.Writer) {
	message, _ := sanitizedCapturedText([]byte(err.Error()), redactor, false, maxErrorMessageBytes)
	result.Error = &ResultError{Code: code, Message: message}
	if diagnostics != nil {
		_, _ = io.WriteString(diagnostics, "ptah-runner: "+message+"\n")
	}
}

func sanitizedCapturedText(content []byte, redactor Redactor, truncated bool, limit int64) (string, int64) {
	value := strings.ToValidUTF8(redactor.RedactCaptured(string(content), truncated), "\uFFFD")
	if limit <= 0 || int64(len(value)) <= limit {
		return value, 0
	}
	retained := value[:int(limit)]
	for !utf8.ValidString(retained) {
		retained = retained[:len(retained)-1]
	}
	return retained, int64(len(value) - len(retained))
}

func stdoutWasTruncated(result *Result) bool {
	return result.Truncation != nil && result.Truncation.Stdout
}

func operationNeedsDatabase(operation Operation) bool {
	switch operation {
	case OperationObserve, OperationPlan, OperationApply:
		return true
	default:
		return false
	}
}

func ensureNoCredentialsInArguments(arguments []string, environment map[string]string) error {
	for _, key := range protectedEnvironmentKeys {
		secret := environment[key]
		if secret == "" {
			continue
		}
		for _, argument := range arguments {
			if strings.Contains(argument, secret) {
				return errors.New("a protected credential would be exposed in process arguments")
			}
		}
	}
	return nil
}

func fingerprintJSON(content []byte) (string, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return "", errors.New("multiple JSON values")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return sha256Digest(canonical), nil
}

func decodeSingleJSON(content []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

func digestFromReference(reference string) (string, error) {
	matches := digestPattern.FindAllString(reference, -1)
	if len(matches) != 1 {
		return "", errors.New("SHA-256 digest not found")
	}
	return strings.ToLower(matches[0]), nil
}

func normalizeDigest(value string) (string, error) {
	if !digestPattern.MatchString(value) || len(value) != len("sha256:")+sha256.Size*2 {
		return "", errors.New("invalid SHA-256 digest")
	}
	return strings.ToLower(value), nil
}
