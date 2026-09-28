package runner

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/ocireference"
	"github.com/stokaro/ptah-operator/internal/plancontract"
	"github.com/stokaro/ptah-operator/internal/planseal"
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
	var mutationDispatchDeadline time.Time
	var mutationExecutionDeadline time.Time
	var approvedSequence []byte

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
	// First, and before anything that reads another input: a Job built for
	// another protocol is refused by what this runner enforces, not by the
	// contract the Job was built under. The result is the protocol refusal
	// document and nothing else, which a manager of any protocol reads.
	if err := checkProtocolBinding(values); err != nil {
		setResultError(&result, CodeRunnerProtocolMismatch, err, redactor, config.Diagnostics)
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
	// One authority block for both mutating families. A schema Apply and a
	// migration Apply reach the same database through the same kind of Pod, so
	// a boundary that holds for one and not the other is not a boundary: it
	// leaves the other free to dispatch on a changed target, past both
	// deadlines, with no approved plan named at all.
	if config.Operation.Mutating() {
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
		if config.Operation == OperationMigrationApply {
			// A migration Apply that names no approved sequence, or no
			// history that sequence was computed against, has nothing behind
			// it. The sequence itself is what bounds the child: it goes to
			// `migrations up --expect-sequence`, which compares it with what
			// Ptah selects under the migration lock and runs nothing unless
			// the two are the same. It is digested again first, so the list
			// Ptah enforces is the one the plan's digest names.
			if !validProtocolDigest(inputs.ExpectedSequenceDigest) {
				setResultError(&result, "missing_plan_binding", errors.New("the approved migration sequence digest is required"), redactor, config.Diagnostics)
				return result
			}
			if !validProtocolDigest(inputs.ExpectedHistoryFingerprint) {
				setResultError(&result, "missing_plan_binding", errors.New("the approved history fingerprint is required"), redactor, config.Diagnostics)
				return result
			}
			sequence, err := approvedSequenceDocument(inputs.ExpectedSequence, inputs.ExpectedSequenceDigest)
			if err != nil {
				setResultError(&result, "plan_binding_mismatch", err, redactor, config.Diagnostics)
				return result
			}
			approvedSequence = sequence
		}
		dispatchNotAfter, err := time.Parse(time.RFC3339Nano, inputs.DispatchNotAfter)
		if err != nil {
			setResultError(&result, "missing_dispatch_deadline", errors.New("an absolute dispatch deadline is required"), redactor, config.Diagnostics)
			return result
		}
		now := time.Now().UTC()
		if config.Clock != nil {
			now = config.Clock().UTC()
		}
		if !now.Before(dispatchNotAfter) {
			setResultError(&result, "dispatch_deadline_expired", errors.New("the dispatch deadline expired before child execution"), redactor, config.Diagnostics)
			return result
		}
		mutationDispatchDeadline = dispatchNotAfter
		executionNotAfter, err := time.Parse(time.RFC3339Nano, inputs.ExecutionNotAfter)
		if err != nil || executionNotAfter.Before(dispatchNotAfter) {
			setResultError(&result, "missing_execution_deadline", errors.New("a valid absolute execution deadline is required"), redactor, config.Diagnostics)
			return result
		}
		if !now.Before(executionNotAfter) {
			setResultError(&result, "execution_deadline_expired", errors.New("the execution deadline expired before child execution"), redactor, config.Diagnostics)
			return result
		}
		mutationExecutionDeadline = executionNotAfter
	}
	// How long a stopped child may take is part of the same authority. The
	// deadlines above say when the child has to be stopped; the grace says how
	// long stopping it may take before the Pod is killed around it.
	terminationGrace, err := terminationGracePeriod(values, config.Operation)
	if err != nil {
		setResultError(&result, "missing_termination_grace", err, redactor, config.Diagnostics)
		return result
	}

	if config.PtahBinary == "" {
		config.PtahBinary = "ptah"
	}
	if config.Diagnostics == nil {
		config.Diagnostics = io.Discard
	}
	if config.Executor == nil {
		config.Executor = OSExecutor{StopDelay: ChildStopDelay(terminationGrace)}
	}
	// Every operation that reaches the registry prepares its access first: the
	// authority check, the authenticated-plain-HTTP refusal and the verified CA
	// snapshot the child reads.
	if ociReference, reachesRegistry := operationOCIReference(config.Operation, inputs); reachesRegistry {
		preparedEnvironment, cleanupCA, err := PrepareOCISourceAccess(
			ociReference,
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
		if _, decodeErr := dataplane.DecodePlan(plan, inputs.ExpectedDatabaseEngine); decodeErr != nil {
			setResultError(&result, "invalid_plan", errors.New("approved plan failed strict validation"), redactor, config.Diagnostics)
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

	if config.Operation == OperationMigrationApply {
		sequenceFile, err := os.CreateTemp(config.TempDir, "ptah-expected-sequence-*.json")
		if err != nil {
			setResultError(&result, "prepare_plan", errors.New("create the approved sequence file"), redactor, config.Diagnostics)
			return result
		}
		sequencePath := sequenceFile.Name()
		defer func() { _ = os.Remove(sequencePath) }()
		_, writeErr := sequenceFile.Write(approvedSequence)
		closeErr := sequenceFile.Close()
		if writeErr != nil || closeErr != nil {
			setResultError(&result, "prepare_plan", errors.New("write the approved sequence file"), redactor, config.Diagnostics)
			return result
		}
		inputs.ExpectedSequencePath = sequencePath
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
	if config.Operation.Mutating() {
		// Plan reconstruction and temporary-file I/O may take a meaningful
		// fraction of a short dispatch window, and a suspended process may resume
		// after the Lease has moved to another operation. The check immediately
		// adjacent to executor dispatch is therefore authoritative; the earlier
		// check only avoids doing unnecessary preparation.
		now := time.Now().UTC()
		if config.Clock != nil {
			now = config.Clock().UTC()
		}
		if !now.Before(mutationDispatchDeadline) {
			setResultError(&result, "dispatch_deadline_expired", errors.New("the dispatch deadline expired before child execution"), redactor, config.Diagnostics)
			return result
		}
		if !now.Before(mutationExecutionDeadline) {
			setResultError(&result, "execution_deadline_expired", errors.New("the execution deadline expired before child execution"), redactor, config.Diagnostics)
			return result
		}
	}
	executionContext := ctx
	cancelExecution := func() {}
	if config.Operation.Mutating() {
		executionContext, cancelExecution = context.WithDeadline(ctx, mutationExecutionDeadline)
	}
	defer cancelExecution()
	var outcome commandOutcome
	if config.Operation == OperationApply {
		outcome = executeReportCommand(executionContext, config, spec, config.MaxResultBytes)
		// Dispatching a mutating child is enough to require observation before
		// any retry. Even an executable-start ambiguity is handled fail-safe.
		result.MutationStarted = true
	} else {
		outcome = executeCommand(executionContext, config, spec)
	}
	if config.Operation == OperationMigrationApply {
		// The same rule, and one more: a migration child that is cancelled, or
		// that fails to be executed at all, may already have committed
		// statements, and nothing outside the database can say. So the frame
		// claims both until a validated run report narrows it.
		result.MutationStarted = true
		result.Uncertain = true
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
		finishSchemaApply(&result, outcome, redactor, config.Diagnostics)
	} else if outcome.err != nil {
		setResultError(&result, "execution_error", childFailure(config.Operation, outcome.err), redactor, config.Diagnostics)
	} else {
		finishSingleCommandResult(&result, outcome, redactor, config.Diagnostics)
	}
	// The migration document is read on both paths, and before the exit status
	// is allowed to classify anything. A run that stopped is exactly the run
	// whose controller has to be told what the database now holds, and a
	// nonzero exit is how Ptah reports that it stopped -- reading the report
	// only on the clean path drops the evidence precisely when it decides the
	// next move. That holds for a run this process stopped as well: Ptah
	// answers SIGTERM by cancelling the statement it is running, which rolls
	// back a file it runs in a transaction, and then reads the history and
	// writes the same document. Truncated output is not a document, so it is
	// not read.
	if config.Operation == OperationMigrationHistory || config.Operation == OperationMigrationApply {
		decodeMigrationReport(&result, config, outcome, redactor)
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

// decodeMigrationReport reads the typed document a migration command wrote,
// whatever the child's exit status said, and lets a validated report settle
// what the frame claims about the database.
//
// An unreadable document is only this function's error to report when nothing
// else already failed: a child that exited nonzero and wrote nothing is
// reported as the child exit it was, not as malformed output.
//
// A child this process stopped is read too, whether it was stopped at its
// execution deadline or because the Pod is terminating. Its document is still
// the database's account, taken after the run returned, and the report is what
// separates a run that stopped cleanly between two files from one that stopped
// inside one. A child that was killed before it wrote one leaves nothing that
// decodes, and the frame keeps claiming a started, uncertain mutation. Any
// other failure to run the child leaves no document at all, and is not read.
func decodeMigrationReport(result *Result, config Config, outcome commandOutcome, redactor Redactor) {
	if (outcome.err != nil && !stoppedByContext(outcome.err)) || outcome.stdout.dropped() != 0 {
		return
	}
	if config.Operation == OperationMigrationHistory {
		report, err := dataplane.DecodeMigrationStatus(outcome.stdout.bytes())
		if err != nil {
			if result.Error == nil {
				setResultError(result, "invalid_migration_history_output",
					errors.New("migration history output failed strict validation"), redactor, config.Diagnostics)
			}
			return
		}
		redactMigrationHistoryError(&report)
		result.MigrationHistory = &report
		return
	}
	report, err := dataplane.DecodeMigrationRun(outcome.stdout.bytes())
	if err != nil {
		if result.Error == nil {
			setResultError(result, "invalid_migration_run_output",
				errors.New("migration run output failed strict validation"), redactor, config.Diagnostics)
		}
		return
	}
	report.Error = ""
	redactMigrationHistoryError(report.Status)
	result.MigrationRun = &report
	// The report is the only thing that may narrow what was claimed before the
	// child ran: an outcome that moved nothing releases the mutation claim, and
	// only an outcome the database accounted for releases the uncertainty.
	result.MutationStarted = report.Outcome != dataplane.MigrationOutcomeUpToDate &&
		report.Outcome != dataplane.MigrationOutcomeDryRun
	// Partial and unknown are the two outcomes no retry may follow.
	result.Uncertain = report.Outcome == dataplane.MigrationOutcomePartial ||
		report.Outcome == dataplane.MigrationOutcomeUnknown
}

// redactMigrationHistoryError clears the free-text error a dirty revision
// carries. Ptah's own account of why a revision is dirty may quote the row
// value that violated a constraint, and nothing downstream of this frame ever
// reads the field: the controller names the dirty version and asks a person to
// read the database directly, never Ptah's sentence about why (see
// migrationRunMessage and recordMigrationHistory in internal/controller). This
// clears it at the source instead of sealing it, because sealing a value
// nothing ever decrypts would add a key to manage for no reader it protects.
func redactMigrationHistoryError(report *dataplane.MigrationStatusReport) {
	if report != nil && report.DirtyRevision != nil {
		report.DirtyRevision.Error = ""
	}
}

// operationOCIReference is the reference an operation fetches, and whether it
// reaches a registry at all. Resolve and verify are told a requested reference;
// the migration operations are told the digest the controller already resolved,
// and read that artifact as their migration directory. An empty reference is
// still handed over, so the refusal stays the registry access one rather than
// becoming a different error depending on which field was blank.
// operationOCIReference names the operations that reach a registry at all.
//
// The migration operations are deliberately absent. They read a directory a
// fetch container already materialized, so the process that holds the database
// credentials holds no registry credentials -- the same separation the schema
// path keeps, for the same reason.
func operationOCIReference(operation Operation, inputs Inputs) (string, bool) {
	switch operation {
	case OperationResolve, OperationVerify:
		return inputs.RequestedReference, true
	default:
		return "", false
	}
}

// runPlan accepts a plan only when two uninterrupted native reads save the
// exact same bytes, and the native apply path reads those bytes and lists the
// same statements in a dry run. Every read goes through the document Ptah
// prints under --json; nothing is matched in the text it writes for a person.
// The controller keeps the database-realm Lease throughout this function and
// through publication of the returned bytes.
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
	protectedTables, err := planProtectedTables(environment)
	if err != nil {
		setResultError(&result, "invalid_input", err, redactor, config.Diagnostics)
		return result
	}
	if strings.TrimSpace(inputs.ExpectedDatabaseEngine) == "" {
		setResultError(&result, "invalid_input", errors.New("PTAH_EXPECTED_DATABASE_ENGINE is required"), redactor, config.Diagnostics)
		return result
	}
	// A missing or malformed seal key refuses before the executor starts: a
	// plan this runner could compute but could not seal is not worth the
	// child dispatch it would take to find that out.
	sealKey, err := planseal.DecodePublicKey(inputs.PlanSealPublicKey)
	if err != nil {
		setResultError(&result, "missing_plan_seal_key", fmt.Errorf("%s: %w", EnvPlanSealPublicKey, err), redactor, config.Diagnostics)
		return result
	}
	if strings.TrimSpace(inputs.SealedPlanJobName) == "" {
		setResultError(&result, "missing_plan_seal_key",
			fmt.Errorf("%s is required", EnvSealedPlanJobName), redactor, config.Diagnostics)
		return result
	}
	sealEnvelope := planseal.Envelope{OperationID: inputs.OperationID, JobName: inputs.SealedPlanJobName}

	// Each read saves into a directory only this process writes, under a name
	// no earlier read used, so a file found there after a read is that read's.
	outputDir, err := os.MkdirTemp(config.TempDir, "ptah-plan-output-*")
	if err != nil {
		setResultError(&result, "prepare_plan", errors.New("create the plan output directory"), redactor, config.Diagnostics)
		return result
	}
	defer func() { _ = os.RemoveAll(outputDir) }()
	if outputDir, err = filepath.Abs(outputDir); err != nil {
		setResultError(&result, "prepare_plan", errors.New("resolve the plan output directory"), redactor, config.Diagnostics)
		return result
	}

	planSpec := func(outputPath string) (CommandSpec, error) {
		planInputs := inputs
		planInputs.PlanOutputPath = outputPath
		spec, err := BuildCommand(config.PtahBinary, OperationPlan, planInputs)
		if err != nil {
			return CommandSpec{}, err
		}
		spec.Env = environmentWithout(childEnvironment(environment), "PTAH_EXCLUDE", "PTAH_PROTECTED_TABLES")
		for _, selector := range planExcludes {
			spec.Args = append(spec.Args, "--exclude="+selector)
		}
		// The fence is passed to the plan and to nothing else. Ptah refuses a
		// plan that would change a fenced table rather than saving one, so a
		// fenced change never reaches an approval or an Apply.
		for _, table := range protectedTables {
			spec.Args = append(spec.Args, "--protected-table="+table)
		}
		return spec, nil
	}
	var reads [2]nativePlan
	for index, name := range []string{"first.plan.json", "second.plan.json"} {
		outputPath := filepath.Join(outputDir, name)
		spec, err := planSpec(outputPath)
		if err != nil {
			setResultError(&result, "invalid_input", err, redactor, config.Diagnostics)
			return result
		}
		if err := ensureNoCredentialsInArguments(spec.Args, environmentMap(environment)); err != nil {
			setResultError(&result, "credential_in_arguments", err, redactor, config.Diagnostics)
			return result
		}
		read, ok := readNativePlan(ctx, config, spec, outputPath, &result, redactor)
		if !ok {
			return result
		}
		reads[index] = read
	}
	if reads[0].outcome != reads[1].outcome || !bytes.Equal(reads[0].document, reads[1].document) {
		setResultError(&result, "unstable_plan", errors.New("consecutive plan reads did not return identical bytes"), redactor, config.Diagnostics)
		return result
	}
	if reads[0].outcome == dataplane.SchemaPlanOutcomeNoChanges {
		result.ChildExitCode = 0
		result.PlanOutcome = PlanOutcomeNoChanges
		return result
	}

	rawPlan := reads[0].document
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

	// The first read's file holds rawPlan, and the dry run's report names the
	// digest of what it read, so validation reads that file rather than a
	// second copy of it.
	validationInputs := inputs
	validationInputs.PlanPath = filepath.Join(outputDir, "first.plan.json")
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
		"PTAH_PROTECTED_TABLES",
	)
	if err := ensureNoCredentialsInArguments(validationSpec.Args, environmentMap(environment)); err != nil {
		setResultError(&result, "credential_in_arguments", err, redactor, config.Diagnostics)
		return result
	}
	contentDigest := sha256Digest(rawPlan)
	if !validatePlanDryRun(ctx, config, validationSpec, decoded, contentDigest, &result, redactor) {
		return result
	}

	// The digest binds the approved plan to these exact plaintext bytes; the
	// frame carries them sealed. A reader of the Pod log, or of anything that
	// copies it, holds ciphertext -- only the manager that holds the matching
	// private key, generated in memory and never persisted, can read the plan.
	// The envelope binds the sealed bytes to this operation and this Job, so a
	// validly sealed plan from a different operation or a different attempt
	// of this one cannot be substituted for this result at harvest.
	sealed, err := planseal.SealPlan(rawPlan, sealEnvelope, sealKey)
	if err != nil {
		setResultError(&result, "plan_seal_failed", err, redactor, config.Diagnostics)
		return result
	}
	result.ChildExitCode = 0
	result.Stdout = sealed
	result.PlanContentDigest = contentDigest
	result.PlanOutcome = PlanOutcomeChanges
	return result
}

// nativePlan is what one `schema plan --json` read produced: how it ended, and
// for a plan with changes the exact bytes it saved.
type nativePlan struct {
	outcome  string
	document []byte
}

// readNativePlan runs one plan and reads it back through its report.
//
// The plan is the file --output saved, not the copy the report embeds. The
// file holds the bytes `schema plan --dry-run` printed before this runner read
// reports, so the content digest of every plan already stored and approved is
// the digest a new read of the same plan produces. The report's digest of that
// file is what binds the two.
//
// A fence refusal is reported as protected_table with the report's own
// sentence, which names the tables and no row. Any other refusal is reported
// by its code, and a failure, a report this runner cannot read, a plan saved
// somewhere else and a file whose digest is not the reported one are all
// invalid plan output. The exit status decides nothing; see
// noteReportedSuccess.
func readNativePlan(
	ctx context.Context,
	config Config,
	spec CommandSpec,
	outputPath string,
	result *Result,
	redactor Redactor,
) (nativePlan, bool) {
	outcome := executeReportCommand(ctx, config, spec, planReportLimit(config.MaxPlanBytes))
	result.ChildExitCode = outcome.exitCode
	if outcome.err != nil {
		setResultError(result, "invalid_plan_output", childFailure(OperationPlan, outcome.err), redactor, config.Diagnostics)
		return nativePlan{}, false
	}
	if dropped := outcome.stdout.dropped(); dropped != 0 {
		result.Truncation = &TruncationMetadata{Stdout: true, StdoutBytesDropped: dropped}
		setResultError(result, "invalid_plan_output", errors.New("plan report exceeded the configured result limit"), redactor, config.Diagnostics)
		return nativePlan{}, false
	}
	report, err := dataplane.DecodeSchemaPlan(outcome.stdout.bytes())
	if err != nil {
		setResultError(result, "invalid_plan_output", reportDecodeError("plan", outcome), redactor, config.Diagnostics)
		return nativePlan{}, false
	}
	switch report.Outcome {
	case dataplane.SchemaPlanOutcomeRefused:
		if report.Refusal.Code == dataplane.SchemaRefusalProtectedTable {
			setResultError(result, "protected_table", errors.New(fenceRefusalMessage(report)), redactor, config.Diagnostics)
			return nativePlan{}, false
		}
		setResultError(result, "plan_refused", fmt.Errorf("ptah refused to plan the change (%s)", report.Refusal.Code), redactor, config.Diagnostics)
		return nativePlan{}, false
	case dataplane.SchemaPlanOutcomeFailed:
		setResultError(result, "invalid_plan_output", errors.New("plan command did not complete successfully"), redactor, config.Diagnostics)
		return nativePlan{}, false
	}
	noteReportedSuccess(outcome.exitCode, config.Diagnostics)
	if report.Outcome == dataplane.SchemaPlanOutcomeNoChanges {
		return nativePlan{outcome: report.Outcome}, true
	}
	if report.PlanPath != outputPath {
		setResultError(result, "invalid_plan_output", errors.New("ptah reported saving the plan somewhere other than the path it was given"), redactor, config.Diagnostics)
		return nativePlan{}, false
	}
	document, err := readSavedPlan(outputPath, config.MaxPlanBytes)
	if err != nil {
		setResultError(result, "invalid_plan_output", err, redactor, config.Diagnostics)
		return nativePlan{}, false
	}
	if sha256Digest(document) != report.PlanDigest {
		setResultError(result, "invalid_plan_output", errors.New("the saved plan does not match the digest its report names"), redactor, config.Diagnostics)
		return nativePlan{}, false
	}
	return nativePlan{outcome: report.Outcome, document: document}, true
}

// validatePlanDryRun runs the saved plan through `schema apply --dry-run` and
// accepts it only when the report says a dry run read exactly this plan and
// listed its statements in order. A dry run verifies the plan's source
// fingerprint before it lists anything, so this is also a check that the plan
// was not already stale when it was read.
func validatePlanDryRun(
	ctx context.Context,
	config Config,
	spec CommandSpec,
	plan dataplane.PlanFile,
	contentDigest string,
	result *Result,
	redactor Redactor,
) bool {
	outcome := executeReportCommand(ctx, config, spec, config.MaxResultBytes)
	result.ChildExitCode = outcome.exitCode
	if outcome.err != nil {
		setResultError(result, "invalid_plan_output", childFailure(OperationPlan, outcome.err), redactor, config.Diagnostics)
		return false
	}
	if dropped := outcome.stdout.dropped(); dropped != 0 {
		result.Truncation = &TruncationMetadata{Stdout: true, StdoutBytesDropped: dropped}
		setResultError(result, "invalid_plan_output", errors.New("plan validation output exceeded the configured result limit"), redactor, config.Diagnostics)
		return false
	}
	report, err := dataplane.DecodeSchemaApply(outcome.stdout.bytes())
	if err != nil {
		setResultError(result, "invalid_plan_output", reportDecodeError("apply", outcome), redactor, config.Diagnostics)
		return false
	}
	switch {
	case report.Outcome != dataplane.SchemaApplyOutcomeDryRun:
		reason := report.Outcome
		if report.Refusal != nil {
			reason += " (" + report.Refusal.Code + ")"
		}
		setResultError(result, "invalid_plan_output", fmt.Errorf("plan validation reported %s rather than a dry run", reason), redactor, config.Diagnostics)
		return false
	case report.PlanDigest != contentDigest:
		setResultError(result, "invalid_plan_output", errors.New("plan validation read a plan other than the one saved"), redactor, config.Diagnostics)
		return false
	case !slices.Equal(report.Statements, planStatements(plan)):
		setResultError(result, "invalid_plan_output", errors.New("native dry run does not list the reviewed plan statements"), redactor, config.Diagnostics)
		return false
	}
	noteReportedSuccess(outcome.exitCode, config.Diagnostics)
	return true
}

// planStatements is the SQL of each statement, in the plan's order and as the
// plan stores it.
func planStatements(plan dataplane.PlanFile) []string {
	statements := make([]string, 0, len(plan.Statements))
	for _, statement := range plan.Statements {
		statements = append(statements, statement.SQL)
	}
	return statements
}

// fenceRefusalMessage is the sentence a fence refusal is reported with: the
// report's own, which names the fenced tables and no row.
func fenceRefusalMessage(report dataplane.SchemaPlanReport) string {
	if message := strings.TrimSpace(report.Error); message != "" {
		return message
	}
	if len(report.Refusal.Tables) != 0 {
		return "refusing to change protected table(s) " + strings.Join(report.Refusal.Tables, ", ")
	}
	return "refusing to change a protected table"
}

// planReportEnvelopeBytes is room for the report fields around the plan a
// `schema plan --json` report embeds: the version, the outcome, the digest and
// the path of the saved file.
const planReportEnvelopeBytes = 64 << 10

// planReportLimit bounds what `schema plan --json` may print for a plan the
// plan limit admits.
//
// The report embeds the plan it saved, indented one level deeper than the
// file, so it is always larger than the plan. Every line of the file but its
// outer braces carries at least two spaces of indentation, one character and
// a newline, so two more bytes per line is at most half the file again; twice
// the plan limit leaves room above that. Bounding the report by the plan limit
// itself would refuse every plan near the limit as truncated output.
func planReportLimit(maxPlanBytes int64) int64 {
	return 2*maxPlanBytes + planReportEnvelopeBytes
}

// readSavedPlan reads the plan file a plan read saved, and refuses one larger
// than the executable plan limit without reading past it.
func readSavedPlan(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("the saved plan could not be opened")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("the saved plan is not a regular file")
	}
	if info.Size() > limit {
		return nil, errors.New("plan output exceeds the configured plan limit")
	}
	document, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, errors.New("the saved plan could not be read")
	}
	if int64(len(document)) > limit {
		return nil, errors.New("plan output exceeds the configured plan limit")
	}
	return document, nil
}

// planProtectedTables reads the tables this plan may not change. An entry is a
// table, or a schema and a table, as the declaration names it; anything else is
// refused here rather than passed to the child, because a flag value that is
// not an identifier is an input error and not a fence.
func planProtectedTables(environment []string) ([]string, error) {
	tables, err := decodeEnvironmentList(environmentMap(environment)["PTAH_PROTECTED_TABLES"])
	if err != nil {
		return nil, errors.New("PTAH_PROTECTED_TABLES is not a valid encoded table list")
	}
	for _, table := range tables {
		if !protectedTablePattern.MatchString(table) {
			return nil, errors.New("PTAH_PROTECTED_TABLES contains a name that is not a table or schema.table")
		}
	}
	return normalizedSelectors(tables), nil
}

// protectedTablePattern is the shape the API already enforces, repeated here
// because the runner is the process that hands the value to a command line and
// cannot assume what admitted it.
var protectedTablePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*(\.[A-Za-z_][A-Za-z0-9_$]*)?$`)

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
	severity, count, findings, err := normalizeDriftSummary(report)
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
	// The native drift command uses exit 1 as a domain outcome. Once its exact
	// report has been validated, the framed operation itself is successful and
	// must use the protocol-wide success exit code.
	result.ChildExitCode = 0
	return result
}

func normalizeDriftSummary(report dataplane.DriftReport) (string, int32, []DriftFindingSummary, error) {
	severity := strings.ToLower(strings.TrimSpace(report.HighestSeverity))
	if !report.Drift {
		if len(report.Findings) != 0 {
			return "", 0, nil, errors.New("converged drift report contains findings")
		}
		switch severity {
		case "", "safe":
			return "", 0, nil, nil
		default:
			return "", 0, nil, errors.New("converged drift report contains a drift severity")
		}
	}
	if !validDriftSeverity(severity) {
		return "", 0, nil, errors.New("drift report contains an invalid highest severity")
	}
	// Ptah finds drift in objects its report has no category for: grants,
	// default privileges, views and triggers among them. Such a report says
	// drift with no findings, and its highest severity is the floor a list with
	// nothing in it rates. It is still drift, and Plan still has to read it; the
	// frame carries it as drift with a zero count.
	if len(report.Findings) == 0 {
		if severity != "safe" {
			return "", 0, nil, errors.New("drift report highest severity does not match its findings")
		}
		return severity, 0, nil, nil
	}
	count, err := driftFindingCount(report)
	if err != nil {
		return "", 0, nil, err
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
		return "", 0, nil, errors.New("drift report highest severity does not match its findings")
	}
	return severity, count, findings, nil
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

// executeReportCommand runs a Ptah command that prints one JSON document on
// standard output and writes everything meant for a person to standard error,
// which is what `schema plan --json` and `schema apply --json` do.
//
// Standard error is discarded, never parsed and never forwarded. It lists the
// planned statements, and a statement can carry a declared row value or a
// credential that the plan gate keeps out of every frame, so the runner's own
// diagnostics may not carry it either. The document says how the run ended.
func executeReportCommand(ctx context.Context, config Config, spec CommandSpec, stdoutLimit int64) commandOutcome {
	stdout := newBoundedBuffer(stdoutLimit)
	exitCode, err := config.Executor.Execute(ctx, spec, stdout, io.Discard)
	return commandOutcome{exitCode: exitCode, err: err, stdout: stdout, stderr: newBoundedBuffer(0)}
}

// reportDecodeError says why a report could not be read. Empty standard output
// is named apart from a malformed document. An executor that predates --json
// leaves it, because it refuses the flag before it runs anything, and so does
// any flag error and a process that stopped before writing its report -- which
// for an Apply may be after its statements were sent, so the caller treats it
// as an outcome nobody knows rather than as a run that changed nothing.
func reportDecodeError(command string, outcome commandOutcome) error {
	if len(outcome.stdout.bytes()) == 0 {
		return fmt.Errorf(
			"ptah schema %s exited with code %d and printed no report; a build whose schema %s does not accept --json "+
				"exits this way, and so does a run that stopped before writing its report",
			command, outcome.exitCode, command,
		)
	}
	return fmt.Errorf("ptah schema %s report failed strict validation", command)
}

// finishSchemaApply reads the document `schema apply --json` printed, on every
// exit status, and settles the result from it.
//
// Only a report of applied that names the approved plan's digest is a success,
// whatever status the child exited with. Every other ending is an error, and
// Run marks every Apply error uncertain whatever the report says: Kubernetes
// may start more than one Pod for the Job, so no one child's claim that
// nothing reached the database speaks for the others, and only the
// observation that follows can. That holds for a failed report too, although
// in this contract failed means nothing was sent -- unlike a migration run,
// whose failed may follow committed migrations -- so no reading here is shared
// with decodeMigrationReport. The report still names the ending, so a refusal
// reaches the resource as a refusal.
//
// A child this process stopped -- at its execution deadline, or because the Pod
// is terminating -- is not read, unlike a migration child: its report could
// only move the frame from uncertain to a success, and a run stopped by its own
// authority is left for the observation to settle.
func finishSchemaApply(result *Result, outcome commandOutcome, redactor Redactor, diagnostics io.Writer) {
	if outcome.err != nil {
		setResultError(result, "execution_error", childFailure(OperationApply, outcome.err), redactor, diagnostics)
		return
	}
	if outputWasTruncated(result) {
		setResultError(result, "output_truncated", errors.New("ptah output exceeded the configured result limit"), redactor, diagnostics)
		return
	}
	report, err := dataplane.DecodeSchemaApply(outcome.stdout.bytes())
	if err != nil {
		setResultError(result, "invalid_apply_output", reportDecodeError("apply", outcome), redactor, diagnostics)
		return
	}
	switch report.Outcome {
	case dataplane.SchemaApplyOutcomeApplied:
		if report.PlanDigest != result.PlanContentDigest {
			setResultError(result, "invalid_apply_output",
				errors.New("ptah reported applying a plan other than the approved one"), redactor, diagnostics)
			return
		}
		noteReportedSuccess(outcome.exitCode, diagnostics)
		result.ChildExitCode = 0
	case dataplane.SchemaApplyOutcomeRefused:
		if report.Refusal.Code == dataplane.SchemaRefusalStalePlan {
			setResultError(result, "stale_plan", staleApplyMessage(report.Refusal), redactor, diagnostics)
			return
		}
		setResultError(result, "apply_refused",
			fmt.Errorf("ptah refused the approved plan (%s)", report.Refusal.Code), redactor, diagnostics)
	case dataplane.SchemaApplyOutcomeFailed:
		setResultError(result, "apply_failed",
			errors.New("ptah reported that the apply failed before it sent any statement"), redactor, diagnostics)
	case dataplane.SchemaApplyOutcomeUnknown:
		setResultError(result, "apply_outcome_unknown",
			errors.New("ptah sent the statements and returned an error, so how far they got is unknown"), redactor, diagnostics)
	default:
		setResultError(result, "invalid_apply_output",
			fmt.Errorf("ptah reported %s, which an approved plan applied with --auto-approve cannot end in", report.Outcome),
			redactor, diagnostics)
	}
}

// noteReportedSuccess writes down a nonzero exit beside a report of success.
//
// The report decides how a run ended, and the exit status does not overrule
// it. Ptah writes the report once the work is done, so a child can report
// success and still exit nonzero: a SIGTERM that lands between the commit and
// the exit is 143 with applied on standard output. The frame protocol reads a
// success with a nonzero exit as malformed, so the caller frames it with exit
// code zero, and the status the child actually returned stays in the log.
func noteReportedSuccess(exitCode int, diagnostics io.Writer) {
	if exitCode != 0 && diagnostics != nil {
		_, _ = fmt.Fprintf(diagnostics, "ptah-runner: ptah reported success and exited with code %d; the report decides\n", exitCode)
	}
}

// staleApplyMessage says what moved under a stale plan. Ptah names structure
// and declared rows apart; a value this runner does not know is left out
// rather than repeated.
func staleApplyMessage(refusal *dataplane.SchemaRefusal) error {
	switch refusal.Changed {
	case "schema":
		return errors.New("database schema no longer matches the approved plan")
	case "rows":
		return errors.New("declared rows no longer match the approved plan")
	default:
		return errors.New("database state no longer matches the approved plan")
	}
}

// stoppedByContext reports that a child produced no exit status of its own
// because this process stopped it: its execution deadline passed, or the Pod
// is terminating and the runner passed the signal on.
func stoppedByContext(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// childFailure says why a child has no exit status to report. The operation
// is a fixed vocabulary and the reason is one of three sentences, so nothing
// the child wrote reaches the frame through it.
func childFailure(operation Operation, err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("the ptah %s process was stopped at its execution deadline", operation)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("the ptah %s process was stopped because the Pod is terminating", operation)
	default:
		return fmt.Errorf("the ptah %s process could not be completed", operation)
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
	case OperationObserve, OperationPlan, OperationApply,
		OperationMigrationHistory, OperationMigrationApply:
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
