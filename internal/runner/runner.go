package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/ocireference"
	"github.com/stokaro/ptah-operator/internal/plancontract"
	"github.com/stokaro/ptah-operator/internal/schemaselector"
)

const (
	DefaultMaxResultBytes int64 = plancontract.MaxExecutableBytes
	maxErrorMessageBytes        = 4 << 10
)

var (
	digestPattern            = regexp.MustCompile(`(?i)sha256:[0-9a-f]{64}`)
	runnerRequirementPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

const planNoChangesOutput = "Schema is synced, no changes to be made.\n"

type Config struct {
	Operation      Operation
	PtahBinary     string
	MaxResultBytes int64
	MaxPlanBytes   int64
	Environment    []string
	Diagnostics    io.Writer
	Executor       Executor
	TempDir        string
	Clock          func() time.Time
}

type commandOutcome struct {
	exitCode int
	err      error
	stdout   *boundedBuffer
	stderr   *boundedBuffer
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
	var applyDispatchDeadline time.Time
	var applyExecutionDeadline time.Time

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
	if config.MaxResultBytes <= 0 {
		config.MaxResultBytes = DefaultMaxResultBytes
	}
	if config.MaxPlanBytes <= 0 {
		config.MaxPlanBytes = DefaultMaxPlanBytes
	}
	if config.MaxResultBytes > DefaultMaxResultBytes || config.MaxPlanBytes > DefaultMaxPlanBytes {
		setResultError(&result, "invalid_configuration", errors.New("runner byte limits exceed the supported execution contract"), redactor, config.Diagnostics)
		return result
	}
	if (config.Operation == OperationPlan || config.Operation == OperationApply) && config.MaxPlanBytes > config.MaxResultBytes {
		setResultError(&result, "invalid_configuration", errors.New("plan byte limit exceeds the child-output retention limit"), redactor, config.Diagnostics)
		return result
	}
	if operationNeedsDatabase(config.Operation) {
		switch {
		case inputs.CoordinationDigest == "":
			setResultError(&result, "missing_coordination_binding", errors.New("PTAH_COORDINATION_DIGEST is required"), redactor, config.Diagnostics)
			return result
		case !validProtocolDigest(inputs.CoordinationDigest):
			setResultError(&result, "invalid_coordination_binding", errors.New("PTAH_COORDINATION_DIGEST must be a lowercase SHA-256 digest"), redactor, config.Diagnostics)
			return result
		default:
			result.CoordinationDigest = inputs.CoordinationDigest
		}
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
	if config.Operation == OperationApply {
		if !validProtocolDigest(inputs.ExpectedCoordinationDigest) {
			setResultError(&result, "missing_coordination_binding", errors.New("expected coordination digest is required"), redactor, config.Diagnostics)
			return result
		}
		if result.CoordinationDigest != inputs.ExpectedCoordinationDigest {
			setResultError(&result, "coordination_binding_mismatch", errors.New("database coordination realm changed after planning"), redactor, config.Diagnostics)
			return result
		}
		if !validProtocolDigest(inputs.ExpectedTargetDigest) {
			setResultError(&result, "missing_target_binding", errors.New("expected target identity digest is required"), redactor, config.Diagnostics)
			return result
		}
		if result.TargetIdentityDigest != inputs.ExpectedTargetDigest {
			setResultError(&result, "target_binding_mismatch", errors.New("database target identity changed after planning"), redactor, config.Diagnostics)
			return result
		}
		dispatchNotAfter, err := time.Parse(time.RFC3339Nano, inputs.DispatchNotAfter)
		if err != nil {
			setResultError(&result, "missing_dispatch_deadline", errors.New("absolute Apply dispatch deadline is required"), redactor, config.Diagnostics)
			return result
		}
		now := time.Now().UTC()
		if config.Clock != nil {
			now = config.Clock().UTC()
		}
		if !now.Before(dispatchNotAfter) {
			setResultError(&result, "dispatch_deadline_expired", errors.New("Apply dispatch deadline expired before child execution"), redactor, config.Diagnostics)
			return result
		}
		applyDispatchDeadline = dispatchNotAfter
		executionNotAfter, err := time.Parse(time.RFC3339Nano, inputs.ExecutionNotAfter)
		if err != nil || executionNotAfter.Before(dispatchNotAfter) {
			setResultError(&result, "missing_execution_deadline", errors.New("valid absolute Apply execution deadline is required"), redactor, config.Diagnostics)
			return result
		}
		if !now.Before(executionNotAfter) {
			setResultError(&result, "execution_deadline_expired", errors.New("Apply execution deadline expired before child execution"), redactor, config.Diagnostics)
			return result
		}
		applyExecutionDeadline = executionNotAfter
	}

	if config.PtahBinary == "" {
		config.PtahBinary = "ptah"
	}
	if config.Diagnostics == nil {
		config.Diagnostics = io.Discard
	}
	if config.Executor == nil {
		config.Executor = OSExecutor{}
	}
	if config.Operation == OperationResolve || config.Operation == OperationVerify {
		preparedEnvironment, cleanupCA, err := PrepareOCISourceAccess(
			inputs.RequestedReference,
			environment,
			config.TempDir,
		)
		if err != nil {
			setResultError(&result, "invalid_oci_access", err, redactor, config.Diagnostics)
			return result
		}
		defer cleanupCA()
		environment = preparedEnvironment
	}

	if config.Operation == OperationVerify {
		return runVerify(ctx, config, environment, inputs, redactor, result)
	}
	if config.Operation == OperationObserve {
		return runObserve(ctx, config, environment, inputs, redactor, result)
	}
	if config.Operation == OperationPlan {
		return runPlan(ctx, config, environment, inputs, redactor, result)
	}

	var cleanup func()
	applyPlanFromFingerprint := ""
	var applyExpectedOutput []byte
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
		if strings.TrimSpace(inputs.ExpectedDatabaseEngine) == "" {
			setResultError(&result, "invalid_plan", errors.New("expected database engine is required for exact plan validation"), redactor, config.Diagnostics)
			return result
		}
		decoded, decodeErr := dataplane.DecodePlan(plan, inputs.ExpectedDatabaseEngine)
		if decodeErr != nil {
			setResultError(&result, "invalid_plan", errors.New("approved plan failed strict validation"), redactor, config.Diagnostics)
			return result
		}
		if validProtocolDigest(decoded.FromFingerprint) {
			applyPlanFromFingerprint = decoded.FromFingerprint
		}
		applyExpectedOutput = planApplyOutput(decoded)
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
	if config.Operation == OperationApply {
		// Plan reconstruction and temporary-file I/O may take a meaningful
		// fraction of a short dispatch window, and a suspended process may resume
		// after the Lease has moved to another operation. The check immediately
		// adjacent to executor dispatch is therefore authoritative; the earlier
		// check only avoids doing unnecessary preparation.
		now := time.Now().UTC()
		if config.Clock != nil {
			now = config.Clock().UTC()
		}
		if !now.Before(applyDispatchDeadline) {
			setResultError(&result, "dispatch_deadline_expired", errors.New("Apply dispatch deadline expired before child execution"), redactor, config.Diagnostics)
			return result
		}
		if !now.Before(applyExecutionDeadline) {
			setResultError(&result, "execution_deadline_expired", errors.New("Apply execution deadline expired before child execution"), redactor, config.Diagnostics)
			return result
		}
	}
	executionContext := ctx
	cancelExecution := func() {}
	if config.Operation == OperationApply {
		executionContext, cancelExecution = context.WithDeadline(ctx, applyExecutionDeadline)
	}
	defer cancelExecution()
	outcome := executeCommand(executionContext, config, spec)
	if config.Operation == OperationApply {
		// Dispatching a mutating child is enough to require observation before
		// any retry. Even an executable-start ambiguity is handled fail-safe.
		result.MutationStarted = true
	}
	consumeOutcome(
		&result,
		outcome,
		redactor,
		config.Diagnostics,
		config.MaxResultBytes,
		false,
		false,
	)
	if config.Operation == OperationApply {
		finishApplyCommandResult(&result, outcome, redactor, config.Diagnostics)
	} else if outcome.err != nil {
		setResultError(&result, "execution_error", errors.New("ptah resolve process could not be completed"), redactor, config.Diagnostics)
	} else {
		finishSingleCommandResult(&result, outcome, redactor, config.Diagnostics)
	}
	if config.Operation == OperationApply && applyPlanFromFingerprint != "" && outcome.exitCode == 2 && outcome.err == nil &&
		outcome.stdout.dropped() == 0 && outcome.stderr.dropped() == 0 {
		if databaseFingerprint, staleErr := parseStalePlanDiagnostic(applyPlanFromFingerprint, outcome.stdout.bytes(), outcome.stderr.bytes()); staleErr == nil && databaseFingerprint != applyPlanFromFingerprint {
			setResultError(&result, "stale_plan", errors.New("database state no longer matches the approved plan"), redactor, config.Diagnostics)
		}
	}
	if config.Operation == OperationApply && result.Error == nil && !bytes.Equal(outcome.stdout.bytes(), applyExpectedOutput) {
		setResultError(&result, "invalid_apply_output", errors.New("native apply output does not match the approved plan transcript"), redactor, config.Diagnostics)
	}
	if result.Error == nil {
		switch config.Operation {
		case OperationResolve:
			if len(outcome.stderr.bytes()) != 0 {
				setResultError(&result, "invalid_resolve_output", errors.New("resolve command emitted unexpected diagnostics"), redactor, config.Diagnostics)
				break
			}
			resolved, err := dataplane.DecodeResolve(outcome.stdout.bytes())
			if err != nil || ocireference.MatchRequested(inputs.RequestedReference, resolved.Reference) != nil {
				setResultError(&result, "invalid_resolve_output", errors.New("resolve output failed strict validation"), redactor, config.Diagnostics)
				break
			}
			result.ResolvedDigest = resolved.Digest
			result.ResolvedReference = resolved.PinnedReference
			result.ResolvedMediaType = resolved.MediaType
			result.ResolvedSize = resolved.Size
		}
	}
	if config.Operation == OperationApply && result.Error != nil {
		result.Uncertain = true
	}
	return result
}

// runPlan accepts a plan only when two uninterrupted native reads return the
// exact same bytes and the native apply path can parse and rehearse those
// bytes without diagnostics. The controller keeps the database-realm Lease
// throughout this function and through publication of the returned bytes.
func runPlan(
	ctx context.Context,
	config Config,
	environment []string,
	inputs Inputs,
	redactor Redactor,
	result Result,
) Result {
	planExcludes, err := planExcludeSelectors(environment)
	if err != nil {
		setResultError(&result, "invalid_input", err, redactor, config.Diagnostics)
		return result
	}
	if strings.TrimSpace(inputs.ExpectedDatabaseEngine) == "" {
		setResultError(&result, "invalid_input", errors.New("PTAH_EXPECTED_DATABASE_ENGINE is required"), redactor, config.Diagnostics)
		return result
	}

	spec, err := BuildCommand(config.PtahBinary, OperationPlan, inputs)
	if err != nil {
		setResultError(&result, "invalid_input", err, redactor, config.Diagnostics)
		return result
	}
	spec.Env = environmentWithout(childEnvironment(environment), "PTAH_EXCLUDE")
	for _, selector := range planExcludes {
		spec.Args = append(spec.Args, "--exclude="+selector)
	}
	if err := ensureNoCredentialsInArguments(spec.Args, environmentMap(environment)); err != nil {
		setResultError(&result, "credential_in_arguments", err, redactor, config.Diagnostics)
		return result
	}

	first := executeCommand(ctx, config, spec)
	if !validPlanCommandOutcome(&result, first, redactor, config.Diagnostics) {
		return result
	}
	second := executeCommand(ctx, config, spec)
	if !validPlanCommandOutcome(&result, second, redactor, config.Diagnostics) {
		return result
	}
	if !bytes.Equal(first.stdout.bytes(), second.stdout.bytes()) {
		result.ChildExitCode = second.exitCode
		setResultError(&result, "unstable_plan", errors.New("consecutive plan reads did not return identical bytes"), redactor, config.Diagnostics)
		return result
	}

	rawPlan := first.stdout.bytes()
	result.ChildExitCode = second.exitCode
	if string(rawPlan) == planNoChangesOutput {
		result.PlanOutcome = PlanOutcomeNoChanges
		return result
	}
	if int64(len(rawPlan)) > config.MaxPlanBytes {
		setResultError(&result, "invalid_plan_output", errors.New("plan output exceeds the configured plan limit"), redactor, config.Diagnostics)
		return result
	}
	if !utf8.Valid(rawPlan) {
		setResultError(&result, "invalid_plan_output", errors.New("plan output is not valid UTF-8"), redactor, config.Diagnostics)
		return result
	}
	decoded, err := dataplane.DecodePlan(rawPlan, inputs.ExpectedDatabaseEngine)
	if err != nil {
		setResultError(&result, "invalid_plan_output", errors.New("plan output failed strict validation for the configured database engine"), redactor, config.Diagnostics)
		return result
	}
	if !slices.Equal(normalizedSelectors(decoded.Exclude), planExcludes) {
		setResultError(&result, "invalid_plan_output", errors.New("plan output does not bind the requested exclusion scope"), redactor, config.Diagnostics)
		return result
	}
	if redactor.Redact(string(rawPlan)) != string(rawPlan) {
		setResultError(&result, "credential_leak", errors.New("plan output contains a protected credential or URL password"), redactor, config.Diagnostics)
		return result
	}

	planFile, err := os.CreateTemp(config.TempDir, "ptah-plan-validation-*.json")
	if err != nil {
		setResultError(&result, "prepare_plan", errors.New("create temporary plan validation file"), redactor, config.Diagnostics)
		return result
	}
	planPath := planFile.Name()
	defer func() { _ = os.Remove(planPath) }()
	if _, err := planFile.Write(rawPlan); err != nil {
		_ = planFile.Close()
		setResultError(&result, "prepare_plan", errors.New("write temporary plan validation file"), redactor, config.Diagnostics)
		return result
	}
	if err := planFile.Close(); err != nil {
		setResultError(&result, "prepare_plan", errors.New("close temporary plan validation file"), redactor, config.Diagnostics)
		return result
	}

	validationInputs := inputs
	validationInputs.PlanPath = planPath
	validationSpec, err := BuildCommand(config.PtahBinary, OperationApply, validationInputs)
	if err != nil {
		setResultError(&result, "invalid_input", err, redactor, config.Diagnostics)
		return result
	}
	validationSpec.Args = append(validationSpec.Args, "--dry-run")
	validationSpec.Env = environmentWithout(
		childEnvironment(environment),
		envSchemaFile,
		"PTAH_DEV_URL",
		"PTAH_EXCLUDE",
	)
	if err := ensureNoCredentialsInArguments(validationSpec.Args, environmentMap(environment)); err != nil {
		setResultError(&result, "credential_in_arguments", err, redactor, config.Diagnostics)
		return result
	}
	validation := executeCommand(ctx, config, validationSpec)
	if !validPlanCommandOutcome(&result, validation, redactor, config.Diagnostics) {
		return result
	}
	if !bytes.Equal(validation.stdout.bytes(), planDryRunOutput(decoded)) {
		setResultError(&result, "invalid_plan_output", errors.New("native dry-run output does not match the reviewed plan statements"), redactor, config.Diagnostics)
		return result
	}

	result.ChildExitCode = validation.exitCode
	result.Stdout = string(rawPlan)
	result.PlanContentDigest = sha256Digest(rawPlan)
	result.PlanOutcome = PlanOutcomeChanges
	return result
}

func validPlanCommandOutcome(result *Result, outcome commandOutcome, redactor Redactor, diagnostics io.Writer) bool {
	result.ChildExitCode = outcome.exitCode
	if outcome.stdout.dropped() != 0 || outcome.stderr.dropped() != 0 {
		result.Truncation = &TruncationMetadata{
			Stdout:             outcome.stdout.dropped() != 0,
			StdoutBytesDropped: outcome.stdout.dropped(),
			Stderr:             outcome.stderr.dropped() != 0,
			StderrBytesDropped: outcome.stderr.dropped(),
		}
		setResultError(result, "invalid_plan_output", errors.New("plan validation output exceeded the configured result limit"), redactor, diagnostics)
		return false
	}
	if outcome.err != nil || outcome.exitCode != 0 {
		setResultError(result, "invalid_plan_output", errors.New("plan validation command did not complete successfully"), redactor, diagnostics)
		return false
	}
	if len(outcome.stderr.bytes()) != 0 {
		setResultError(result, "invalid_plan_output", errors.New("plan validation command emitted unexpected diagnostics"), redactor, diagnostics)
		return false
	}
	return true
}

func planDryRunOutput(plan dataplane.PlanFile) []byte {
	var sql strings.Builder
	for _, statement := range plan.Statements {
		text := strings.TrimSpace(statement.SQL)
		if text == "" {
			continue
		}
		sql.WriteString(strings.TrimSuffix(text, ";"))
		sql.WriteString(";\n")
	}
	return []byte("Planned schema changes:\n" + strings.TrimSpace(sql.String()) + "\n")
}

func planApplyOutput(plan dataplane.PlanFile) []byte {
	output := append([]byte(nil), planDryRunOutput(plan)...)
	output = append(output, "Auto-approval enabled; applying schema changes.\n"...)
	output = append(output, "Schema apply completed successfully.\n"...)
	return output
}

func planExcludeSelectors(environment []string) ([]string, error) {
	selectors, err := decodeEnvironmentList(environmentMap(environment)["PTAH_EXCLUDE"])
	if err != nil {
		return nil, errors.New("PTAH_EXCLUDE is not a valid encoded selector list")
	}
	if err := schemaselector.Validate(selectors); err != nil {
		return nil, errors.New("PTAH_EXCLUDE contains an invalid selector list")
	}
	return normalizedSelectors(selectors), nil
}

func normalizedSelectors(selectors []string) []string {
	normalized := append([]string(nil), selectors...)
	slices.Sort(normalized)
	return slices.Compact(normalized)
}

func runVerify(ctx context.Context, config Config, environment []string, inputs Inputs, redactor Redactor, result Result) Result {
	requestedReference, err := ocireference.Parse(inputs.RequestedReference)
	if err != nil {
		setResultError(&result, "invalid_input", errors.New("PTAH_REQUESTED_REFERENCE must contain the original credential-free OCI reference"), redactor, config.Diagnostics)
		return result
	}
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
	if err := ocireference.ValidateResolution(inputs.RequestedReference, inputs.ResolvedReference, resolvedDigest); err != nil {
		setResultError(&result, "invalid_input", errors.New("requested and resolved OCI references do not form one immutable source binding"), redactor, config.Diagnostics)
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
	consumeOutcome(&result, verifyOutcome, redactor, config.Diagnostics, config.MaxResultBytes, false, false)
	if verifyOutcome.err != nil {
		setResultError(&result, "execution_error", errors.New("ptah verify process could not be completed"), redactor, config.Diagnostics)
		return result
	}
	if outputWasTruncated(&result) {
		setResultError(&result, "output_truncated", errors.New("ptah output exceeded the configured result limit"), redactor, config.Diagnostics)
		return result
	}
	if verifyOutcome.exitCode != 0 && verifyOutcome.exitCode != 2 {
		setResultError(&result, "child_exit", fmt.Errorf("ptah exited with code %d", verifyOutcome.exitCode), redactor, config.Diagnostics)
		return result
	}
	if len(verifyOutcome.stderr.bytes()) != 0 {
		setResultError(&result, "invalid_verification_output", errors.New("verification command emitted unexpected diagnostics"), redactor, config.Diagnostics)
		return result
	}

	report, err := dataplane.DecodeVerify(verifyOutcome.stdout.bytes())
	if err != nil {
		setResultError(&result, "invalid_verification_output", errors.New("verification output failed strict validation"), redactor, config.Diagnostics)
		return result
	}
	if report.Reference != inputs.ResolvedReference || report.Digest != resolvedDigest {
		setResultError(&result, "stale_source", errors.New("verified source digest no longer matches the resolved source"), redactor, config.Diagnostics)
		return result
	}
	nativeRefusal := verifyOutcome.exitCode == 2
	if nativeRefusal && len(report.Findings) == 0 {
		setResultError(&result, "invalid_verification_output", errors.New("verification refusal contains no policy findings"), redactor, config.Diagnostics)
		return result
	}
	if !nativeRefusal && len(report.Findings) != 0 {
		setResultError(&result, "invalid_verification_output", errors.New("successful verification contains policy findings"), redactor, config.Diagnostics)
		return result
	}
	requestedPinRefusal := !requestedReference.IsDigest && slices.Contains(report.Satisfied, "require_digest_pin")
	if nativeRefusal || requestedPinRefusal {
		result.VerificationRequirements = make([]string, 0, len(report.Findings)+1)
		for _, finding := range report.Findings {
			result.VerificationRequirements = append(result.VerificationRequirements, finding.Requirement)
		}
		if requestedPinRefusal {
			result.VerificationRequirements = append(result.VerificationRequirements, "require_digest_pin")
		}
		slices.Sort(result.VerificationRequirements)
		result.VerificationRequirements = slices.Compact(result.VerificationRequirements)
		if len(result.VerificationRequirements) > 64 {
			setResultError(&result, "invalid_verification_output", errors.New("verification refusal exceeds the supported requirement count"), redactor, config.Diagnostics)
			return result
		}
		setResultError(&result, "verification_refused", errors.New("artifact does not satisfy the verification policy"), redactor, config.Diagnostics)
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
	consumeOutcome(&result, inspectOutcome, redactor, config.Diagnostics, config.MaxResultBytes, false, false)
	if inspectOutcome.err != nil {
		setResultError(&result, "execution_error", errors.New("ptah inspect process could not be completed"), redactor, config.Diagnostics)
		return result
	}
	if !outcomeSucceeded(&result, inspectOutcome, redactor, config.Diagnostics) {
		return result
	}
	if len(inspectOutcome.stderr.bytes()) != 0 {
		setResultError(&result, "invalid_artifact_output", errors.New("artifact inspection emitted unexpected diagnostics"), redactor, config.Diagnostics)
		return result
	}

	inspection, err := dataplane.DecodeInspect(inspectOutcome.stdout.bytes())
	if err != nil {
		setResultError(&result, "invalid_artifact_output", errors.New("artifact inspection output is missing required top-level fields"), redactor, config.Diagnostics)
		return result
	}
	if inspection.Digest != resolvedDigest || ocireference.MatchRequested(inputs.ResolvedReference, inspection.Reference) != nil {
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
	// Exclusions belong to the managed planning scope. The drift command does
	// not share that selector language, so it intentionally observes the raw
	// target and Plan performs the authoritative scoped classification.
	driftSpec.Env = environmentWithout(childEnvironment(environment), "PTAH_EXCLUDE")
	driftOutcome := executeCommand(ctx, config, driftSpec)
	// The raw structural diff may contain arbitrary schema literals. Validate
	// it in-process, but never copy it into the framed Pod log result.
	consumeOutcome(&result, driftOutcome, redactor, config.Diagnostics, config.MaxResultBytes, false, false)
	if driftOutcome.err != nil {
		setResultError(&result, "execution_error", errors.New("ptah drift process could not be completed"), redactor, config.Diagnostics)
		return result
	}
	if outputWasTruncated(&result) {
		setResultError(&result, "output_truncated", errors.New("ptah output exceeded the configured result limit"), redactor, config.Diagnostics)
		return result
	}
	if driftOutcome.exitCode != 0 && driftOutcome.exitCode != 1 {
		setResultError(&result, "child_exit", fmt.Errorf("ptah exited with code %d", driftOutcome.exitCode), redactor, config.Diagnostics)
		return result
	}

	report, err := dataplane.DecodeDrift(driftOutcome.stdout.bytes(), driftOutcome.exitCode)
	if err != nil {
		setResultError(&result, "invalid_observed_state", errors.New("native drift output failed strict validation"), redactor, config.Diagnostics)
		return result
	}
	if !dataplane.DialectMatches(inputs.ExpectedDatabaseEngine, report.Dialect) {
		setResultError(&result, "invalid_observed_state", errors.New("drift output dialect does not match the expected database engine"), redactor, config.Diagnostics)
		return result
	}
	severity, count, findings, findingsTruncated, err := normalizeDriftSummary(report)
	if err != nil {
		setResultError(&result, "invalid_observed_state", err, redactor, config.Diagnostics)
		return result
	}
	reportDigest, err := dataplane.DriftReportDigest(report)
	if err != nil {
		setResultError(&result, "invalid_observed_state", err, redactor, config.Diagnostics)
		return result
	}
	result.DriftReportDigest = reportDigest
	result.ObservedDialect = strings.ToLower(strings.TrimSpace(report.Dialect))
	result.ObservedDrift = report.Drift
	result.HighestDriftSeverity = severity
	result.DriftFindingCount = count
	result.DriftFindings = findings
	result.DriftFindingsTruncated = findingsTruncated
	// The native drift command uses exit 1 as a domain outcome. Once its exact
	// report has been validated, the framed operation itself is successful and
	// must use the protocol-wide success exit code.
	result.ChildExitCode = 0
	return result
}

func normalizeDriftSummary(report dataplane.DriftReport) (string, int32, []DriftFindingSummary, bool, error) {
	severity := strings.ToLower(strings.TrimSpace(report.HighestSeverity))
	if !report.Drift {
		if len(report.Findings) != 0 {
			return "", 0, nil, false, errors.New("converged drift report contains findings")
		}
		switch severity {
		case "", "safe":
			return "", 0, nil, false, nil
		default:
			return "", 0, nil, false, errors.New("converged drift report contains a drift severity")
		}
	}
	if !validDriftSeverity(severity) {
		return "", 0, nil, false, errors.New("drift report contains an invalid highest severity")
	}
	if len(report.Findings) == 0 {
		return "", 0, nil, false, errors.New("drift report contains no finding summaries")
	}
	count, err := driftFindingCount(report)
	if err != nil {
		return "", 0, nil, false, err
	}
	findings := make([]DriftFindingSummary, len(report.Findings))
	for index, finding := range report.Findings {
		findings[index] = DriftFindingSummary{
			Category: finding.Category,
			Count:    finding.Count,
			Severity: strings.ToLower(strings.TrimSpace(finding.Severity)),
		}
	}
	slices.SortFunc(findings, func(left, right DriftFindingSummary) int {
		if leftRank, rightRank := driftSeverityRank(left.Severity), driftSeverityRank(right.Severity); leftRank != rightRank {
			return rightRank - leftRank
		}
		return strings.Compare(left.Category, right.Category)
	})
	if findings[0].Severity != severity {
		return "", 0, nil, false, errors.New("drift report highest severity does not match its findings")
	}
	const maxFindings = 64
	truncated := len(findings) > maxFindings
	if truncated {
		findings = append([]DriftFindingSummary(nil), findings[:maxFindings]...)
	}
	return severity, count, findings, truncated, nil
}

func driftFindingCount(report dataplane.DriftReport) (int32, error) {
	var total int64
	for _, finding := range report.Findings {
		total += int64(finding.Count)
		if total > int64(^uint32(0)>>1) {
			return 0, errors.New("drift finding count exceeds the supported range")
		}
	}
	return int32(total), nil
}

func decodeEnvironmentList(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	reader := csv.NewReader(strings.NewReader(value))
	reader.FieldsPerRecord = -1
	values, err := reader.Read()
	if err != nil {
		return nil, err
	}
	if _, err := reader.Read(); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple CSV records")
		}
		return nil, err
	}
	return values, nil
}

func parseStalePlanDiagnostic(candidate string, stdout, stderr []byte) (string, error) {
	if len(stdout) != 0 {
		return "", errors.New("native fingerprint probe produced unexpected stdout")
	}
	prefix := "error: pre-planned migration is stale: the target database schema does not match the plan's source fingerprint " +
		"(plan " + candidate + ", database "
	suffix := "); the database changed since the plan was computed, so re-run `schema plan` " +
		"against the current database and review the fresh plan\n"
	if !bytes.HasPrefix(stderr, []byte(prefix)) || !bytes.HasSuffix(stderr, []byte(suffix)) ||
		len(stderr) != len(prefix)+len("sha256:")+sha256.Size*2+len(suffix) {
		return "", errors.New("native fingerprint probe produced an unexpected diagnostic")
	}
	fingerprint := string(stderr[len(prefix) : len(stderr)-len(suffix)])
	if !validProtocolDigest(fingerprint) {
		return "", errors.New("native fingerprint probe produced an invalid database fingerprint")
	}
	return fingerprint, nil
}

func executeCommand(ctx context.Context, config Config, spec CommandSpec) commandOutcome {
	stdout := newBoundedBuffer(config.MaxResultBytes)
	stderr := newBoundedBuffer(config.MaxResultBytes)
	exitCode, err := config.Executor.Execute(ctx, spec, stdout, stderr)
	return commandOutcome{exitCode: exitCode, err: err, stdout: stdout, stderr: stderr}
}

func consumeOutcome(
	result *Result,
	outcome commandOutcome,
	redactor Redactor,
	diagnostics io.Writer,
	maxResultBytes int64,
	exposeStdout bool,
	exposeStderr bool,
) {
	result.ChildExitCode = outcome.exitCode
	stdoutDropped := outcome.stdout.dropped()
	stderrDropped := outcome.stderr.dropped()
	if exposeStdout {
		var sanitizedDropped int64
		result.Stdout, sanitizedDropped = sanitizedCapturedText(outcome.stdout.bytes(), redactor, stdoutDropped > 0, maxResultBytes)
		stdoutDropped += sanitizedDropped
	}
	if exposeStderr {
		sanitizedStderr, sanitizedStderrDropped := sanitizedCapturedText(outcome.stderr.bytes(), redactor, stderrDropped > 0, maxResultBytes)
		if diagnostics != nil {
			_, _ = io.WriteString(diagnostics, sanitizedStderr)
		}
		stderrDropped += sanitizedStderrDropped
	}
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
			if diagnostics != nil {
				_, _ = io.WriteString(diagnostics, fmt.Sprintf("\nptah-runner: stderr truncated; %d bytes omitted\n", stderrDropped))
			}
		}
	}
}

func finishApplyCommandResult(result *Result, outcome commandOutcome, redactor Redactor, diagnostics io.Writer) {
	if outcome.err != nil {
		setResultError(result, "execution_error", errors.New("ptah apply process could not be completed"), redactor, diagnostics)
		return
	}
	finishSingleCommandResult(result, outcome, redactor, diagnostics)
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
	if outputWasTruncated(result) {
		setResultError(result, "output_truncated", errors.New("ptah output exceeded the configured result limit"), redactor, diagnostics)
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
	if outputWasTruncated(result) {
		setResultError(result, "output_truncated", errors.New("ptah output exceeded the configured result limit"), redactor, diagnostics)
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

func outputWasTruncated(result *Result) bool {
	return result.Truncation != nil
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

func digestFromReference(reference string) (string, error) {
	matches := digestPattern.FindAllString(reference, -1)
	if len(matches) != 1 {
		return "", errors.New("SHA-256 digest not found")
	}
	return strings.ToLower(matches[0]), nil
}
