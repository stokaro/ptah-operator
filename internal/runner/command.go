package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

const (
	// EnvOperationID binds a Job result to one controller operation claim.
	EnvOperationID = "PTAH_OPERATION_ID"
	// EnvRequestedReference is the mutable OCI reference supplied by the user.
	EnvRequestedReference = "PTAH_REQUESTED_REFERENCE"
	// EnvResolvedReference is the earlier immutable OCI resolution.
	EnvResolvedReference = "PTAH_RESOLVED_REFERENCE"
	// EnvVerificationPolicy is the mounted verification policy path.
	EnvVerificationPolicy = "PTAH_VERIFICATION_POLICY"
	// EnvExpectedArtifactType is the exact allowed OCI artifact type.
	EnvExpectedArtifactType = "PTAH_EXPECTED_ARTIFACT_TYPE"
	// EnvPlanDir is the projected directory containing ordered plan chunks.
	EnvPlanDir = "PTAH_PLAN_DIR"
	// EnvExpectedPlanContentDigest binds apply to exact reconstructed bytes.
	EnvExpectedPlanContentDigest = "PTAH_EXPECTED_PLAN_CONTENT_DIGEST"
	// EnvExpectedTargetIdentityDigest binds apply to the credential-free target
	// route identity observed before the plan was created and approved. A
	// read-only database operation that carries it is bound the same way: the
	// reads that prove a schema Apply carry the digest of that Apply's plan.
	// It is optional on a read, and a read without it reads any target.
	EnvExpectedTargetIdentityDigest = "PTAH_EXPECTED_TARGET_IDENTITY_DIGEST"
	// EnvExpectedShadowIdentityDigest binds a migration Apply to the
	// credential-free identity of the shadow database its plan was computed
	// with, and asks for the drift check: an Apply that carries it runs
	// `migrations up --check-drift` against that shadow and no other. An Apply
	// without it checks nothing, as one built before the input existed did.
	EnvExpectedShadowIdentityDigest = "PTAH_EXPECTED_SHADOW_IDENTITY_DIGEST"
	// EnvCoordinationDigest binds every database operation to a user-declared
	// physical database realm without exposing its plaintext coordination key.
	EnvCoordinationDigest = "PTAH_COORDINATION_DIGEST"
	// EnvExpectedCoordinationDigest binds Apply to the realm approved in its
	// immutable plan independently from the operation claim.
	EnvExpectedCoordinationDigest = "PTAH_EXPECTED_COORDINATION_DIGEST"
	// EnvExpectedMigrationSequence is the ordered sequence a migration Apply's
	// approved plan carries, as JSON. The runner digests it again, refuses it
	// unless it matches EnvExpectedMigrationSequenceDigest, and hands it to
	// `migrations up --expect-sequence`.
	EnvExpectedMigrationSequence = "PTAH_EXPECTED_MIGRATION_SEQUENCE"
	// EnvExpectedMigrationSequenceDigest binds a migration Apply to the exact
	// ordered sequence its approved plan carries.
	EnvExpectedMigrationSequenceDigest = "PTAH_EXPECTED_MIGRATION_SEQUENCE_DIGEST"
	// EnvExpectedMigrationHistoryFingerprint binds it to the reading of the
	// revision table that plan was computed against.
	EnvExpectedMigrationHistoryFingerprint = "PTAH_EXPECTED_MIGRATION_HISTORY_FINGERPRINT"
	// EnvDispatchNotAfter is the absolute last instant at which an Apply Pod may
	// dispatch its mutating child, even if an orphaned Pod was scheduled late.
	EnvDispatchNotAfter = "PTAH_DISPATCH_NOT_AFTER"
	// EnvExecutionNotAfter is the absolute deadline enforced on the mutating
	// child process after dispatch.
	EnvExecutionNotAfter = "PTAH_EXECUTION_NOT_AFTER"
	// EnvTerminationGracePeriod is the Pod's terminationGracePeriodSeconds, in
	// whole seconds. The builder writes the same value into the Pod spec and
	// here, and the runner derives from it how long a child it stopped may take
	// (ChildStopDelay).
	EnvTerminationGracePeriod = "PTAH_TERMINATION_GRACE_PERIOD_SECONDS"
	// EnvDatabaseURL carries the target database credential to the child only.
	EnvDatabaseURL = "PTAH_DB_URL"
	// EnvDevelopmentDatabaseURL carries an optional development database credential.
	EnvDevelopmentDatabaseURL = "PTAH_DEV_URL"
	// EnvShadowDatabaseURL carries the shadow database a migration drift
	// check replays into. Ptah reads it as --shadow-db, so it reaches a child
	// only on an Apply the runner passes --check-drift to.
	EnvShadowDatabaseURL = "PTAH_SHADOW_DB"
	// EnvOCIPassword carries a registry password to the child only.
	EnvOCIPassword = "PTAH_OCI_PASSWORD"
	// EnvOCIToken carries a registry identity token to the child only.
	EnvOCIToken = "PTAH_OCI_TOKEN"
	// EnvSchemaFile supplies the desired schema to drift and plan operations.
	EnvSchemaFile = "PTAH_SCHEMA_FILE"
	// EnvExpectedDatabaseEngine selects the supported fingerprint-probe dialect
	// without deriving it from a credential-bearing database URL.
	EnvExpectedDatabaseEngine = "PTAH_EXPECTED_DATABASE_ENGINE"
	// EnvMigrationsDir is the already-materialized migration directory a
	// migration operation reads.
	//
	// It is a local path rather than an OCI reference on purpose. Ptah accepts
	// `--migrations-dir oci://...` and fetches the artifact itself, which would
	// put the registry credentials in the same process as the database ones. A
	// fetch container holds the registry credentials and writes what it fetched
	// into a shared volume; the process that runs SQL reads that volume and
	// never reaches a registry.
	EnvMigrationsDir = "PTAH_MIGRATIONS_DIR"

	// EnvTransactionMode is the mode the resource asked Ptah to wrap the run
	// in. Empty means the resource asked for nothing and the flag is left off,
	// so Ptah chooses.
	EnvTransactionMode = "PTAH_TRANSACTION_MODE"

	// EnvPlanSealPublicKey is the manager's ephemeral public key a Plan Job
	// seals its plan payload to, base64-encoded (see internal/planseal). Only
	// a Plan operation reads it; the runner refuses before starting its
	// executor if it is missing or malformed.
	EnvPlanSealPublicKey = "PTAH_PLAN_SEAL_PUBLIC_KEY"
	// EnvSealedPlanJobName is this Job's deterministic name, known before the
	// Job exists because it is computed from the claim rather than assigned
	// by the API server. The runner binds it into the sealed plan payload
	// alongside the operation ID, so a payload sealed for one Job cannot be
	// substituted for another's at harvest, even a retried attempt of the
	// same operation under a fresh name.
	EnvSealedPlanJobName = "PTAH_SEALED_PLAN_JOB_NAME"

	envOperationID            = EnvOperationID
	envRequestedReference     = EnvRequestedReference
	envResolvedReference      = EnvResolvedReference
	envVerificationPolicy     = EnvVerificationPolicy
	envExpectedArtifactType   = EnvExpectedArtifactType
	envPlanDir                = EnvPlanDir
	envExpectedPlanDigest     = EnvExpectedPlanContentDigest
	envExpectedTargetDigest   = EnvExpectedTargetIdentityDigest
	envExpectedShadowDigest   = EnvExpectedShadowIdentityDigest
	envCoordinationDigest     = EnvCoordinationDigest
	envExpectedCoordination   = EnvExpectedCoordinationDigest
	envExpectedSequence       = EnvExpectedMigrationSequence
	envExpectedSequenceDigest = EnvExpectedMigrationSequenceDigest
	envExpectedHistory        = EnvExpectedMigrationHistoryFingerprint
	envDispatchNotAfter       = EnvDispatchNotAfter
	envExecutionNotAfter      = EnvExecutionNotAfter
	envTerminationGracePeriod = EnvTerminationGracePeriod
	envDatabaseURL            = EnvDatabaseURL
	envSchemaFile             = EnvSchemaFile
	envExpectedDatabaseEngine = EnvExpectedDatabaseEngine
	envMigrationsDir          = EnvMigrationsDir
	envTransactionMode        = EnvTransactionMode
	envPlanSealPublicKey      = EnvPlanSealPublicKey
	envSealedPlanJobName      = EnvSealedPlanJobName
)

// Inputs are the runner-specific environment values used to construct one of
// the fixed Ptah commands. Normal Ptah environment variables remain in Env.
type Inputs struct {
	OperationID                string
	RequestedReference         string
	ResolvedReference          string
	VerificationPolicyPath     string
	ExpectedArtifactType       string
	PlanDir                    string
	ExpectedPlanContentDigest  string
	ExpectedTargetDigest       string
	ExpectedShadowDigest       string
	CoordinationDigest         string
	ExpectedCoordinationDigest string
	ExpectedSequence           string
	ExpectedSequenceDigest     string
	ExpectedHistoryFingerprint string
	DispatchNotAfter           string
	ExecutionNotAfter          string
	ExpectedDatabaseEngine     string
	PlanPath                   string
	MigrationsDir              string
	TransactionMode            string
	// PlanSealPublicKey is the base64-encoded manager public key a Plan
	// operation seals its plan payload to before writing the frame.
	PlanSealPublicKey string
	// SealedPlanJobName is this Plan Job's deterministic name, bound inside
	// the sealed plan payload alongside the operation ID.
	SealedPlanJobName string
	// ExpectedSequencePath is the file the runner wrote the approved sequence
	// to for `migrations up --expect-sequence`. The runner sets it; nothing
	// reads it from the environment.
	ExpectedSequencePath string
	// PlanOutputPath is where `schema plan --output` saves the plan it
	// computes. The runner sets it to a file in a directory of its own, and
	// nothing reads it from the environment.
	PlanOutputPath string
}

func InputsFromEnvironment(environment []string) Inputs {
	values := environmentMap(environment)
	return Inputs{
		OperationID:                values[envOperationID],
		RequestedReference:         values[envRequestedReference],
		ResolvedReference:          values[envResolvedReference],
		VerificationPolicyPath:     values[envVerificationPolicy],
		ExpectedArtifactType:       values[envExpectedArtifactType],
		PlanDir:                    values[envPlanDir],
		ExpectedPlanContentDigest:  values[envExpectedPlanDigest],
		ExpectedTargetDigest:       values[envExpectedTargetDigest],
		ExpectedShadowDigest:       values[envExpectedShadowDigest],
		CoordinationDigest:         values[envCoordinationDigest],
		ExpectedCoordinationDigest: values[envExpectedCoordination],
		ExpectedSequence:           values[envExpectedSequence],
		ExpectedSequenceDigest:     values[envExpectedSequenceDigest],
		ExpectedHistoryFingerprint: values[envExpectedHistory],
		DispatchNotAfter:           values[envDispatchNotAfter],
		ExecutionNotAfter:          values[envExecutionNotAfter],
		ExpectedDatabaseEngine:     values[envExpectedDatabaseEngine],
		MigrationsDir:              values[envMigrationsDir],
		TransactionMode:            values[envTransactionMode],
		PlanSealPublicKey:          values[envPlanSealPublicKey],
		SealedPlanJobName:          values[envSealedPlanJobName],
	}
}

func environmentMap(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, item := range environment {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			value = ""
		}
		values[key] = value
	}
	return values
}

func environmentWithout(environment []string, excludedKeys ...string) []string {
	excluded := make(map[string]struct{}, len(excludedKeys))
	for _, key := range excludedKeys {
		excluded[key] = struct{}{}
	}
	filtered := make([]string, 0, len(environment))
	for _, item := range environment {
		key, _, _ := strings.Cut(item, "=")
		if _, skip := excluded[key]; skip {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

// ptahJSONEnvironment fills the --json flag of every Ptah command that has
// one. The runner chooses the output of each command it starts and passes
// --json itself wherever it reads a document, so the variable is removed from
// every child rather than left to disagree with the flag.
const ptahJSONEnvironment = "PTAH_JSON"

// Ptah reads a flag a command declares from its PTAH_ variable. Ptah v0.23.0
// binds PTAH_EXCLUDE and PTAH_INCLUDE to --exclude and --include on schema
// drift and PTAH_EXCLUDE to --exclude on schema plan, and schema apply --plan
// refuses either. The runner passes the selectors a command should obey as
// arguments it builds itself, and the operator's PTAH_EXCLUDE is an encoded
// list Ptah does not read the same way. So both are removed from every child:
// a selector reaches Ptah only as an argument the runner built, and one that a
// later Ptah starts reading cannot narrow a command with nothing in status
// saying so.
const (
	ptahExcludeEnvironment = "PTAH_EXCLUDE"
	ptahIncludeEnvironment = "PTAH_INCLUDE"
)

// ptahRunReferenceEnvironment fills --run-reference on schema apply and
// migrations up, Ptah v0.28.0's binding for the caller's identifier of a run.
// The reference an Apply records is the claim's operation ID, and the runner
// passes it as an argument it builds (withRunReference), so the variable is
// removed from every child: no other value can reach the logs through it, and
// a reference never reaches a command that writes no log entry.
const ptahRunReferenceEnvironment = "PTAH_RUN_REFERENCE"

// maxRunReferenceBytes is the widest run reference Ptah's logs store: the
// width of their run_reference column, and the bound Ptah v0.28.0 holds the
// flag to before it connects (ValidateRunReference in migration/migrator).
const maxRunReferenceBytes = 255

// withRunReference hands an Apply's child its claim's operation ID as Ptah's
// run reference, so every entry the run writes to the database's own log names
// the operation the journal and the resource's status name. The result
// document reports the reference the entries carry, and the runner holds it to
// this one (dataplane.RunLogReference.CheckRunReference).
//
// The ID is held to the form Ptah stores as given -- 1 to 255 visible ASCII
// characters -- before the child is started. Ptah refuses any other form
// itself, but only once the child is running, and a mutating child that ran
// is a run whose outcome has to be observed before anything is retried. The
// manager's IDs are SHA-256 digests, which always pass. One argument carries
// the flag and its value, so a value that starts with a dash is not read as
// a flag.
func withRunReference(spec CommandSpec, operationID string) (CommandSpec, error) {
	if operationID == "" || len(operationID) > maxRunReferenceBytes {
		return CommandSpec{}, fmt.Errorf("the operation ID is %d bytes, and a run reference is 1 to %d",
			len(operationID), maxRunReferenceBytes)
	}
	for index := range len(operationID) {
		if character := operationID[index]; character < '!' || character > '~' {
			return CommandSpec{}, errors.New("the operation ID holds a character a run reference cannot")
		}
	}
	spec.Args = append(slices.Clone(spec.Args), "--run-reference="+operationID)
	return spec, nil
}

// Ptah v0.28.0 binds PTAH_CHECK_DRIFT and PTAH_DRIFT_EXCLUDE to the drift
// check of `migrations up`. The check is asked for only by the argument the
// runner builds from the Job's expected shadow identity, so both are removed
// from every child, and PTAH_SHADOW_DB from every child but that Apply's.
const (
	ptahCheckDriftEnvironment   = "PTAH_CHECK_DRIFT"
	ptahDriftExcludeEnvironment = "PTAH_DRIFT_EXCLUDE"
)

func childEnvironment(environment []string) []string {
	return environmentWithout(environmentWithout(environment, credentialFileVariables()...),
		ptahJSONEnvironment,
		ptahExcludeEnvironment,
		ptahIncludeEnvironment,
		ptahRunReferenceEnvironment,
		ptahCheckDriftEnvironment,
		ptahDriftExcludeEnvironment,
		EnvShadowDatabaseURL,
		EnvOperationID,
		EnvRequestedReference,
		EnvResolvedReference,
		EnvVerificationPolicy,
		EnvExpectedArtifactType,
		EnvPlanDir,
		EnvExpectedPlanContentDigest,
		EnvExpectedTargetIdentityDigest,
		EnvExpectedShadowIdentityDigest,
		EnvCoordinationDigest,
		EnvExpectedCoordinationDigest,
		EnvExpectedMigrationSequence,
		EnvExpectedMigrationSequenceDigest,
		EnvExpectedMigrationHistoryFingerprint,
		EnvDispatchNotAfter,
		EnvExecutionNotAfter,
		EnvTerminationGracePeriod,
		EnvRunnerProtocolVersion,
		EnvExpectedDatabaseEngine,
		EnvOCIAuthMode,
		EnvOCIAuthRegistryGrant,
		EnvOCIAllowPlainHTTPGrant,
		EnvOCIHasCA,
		EnvOCICASourceFile,
		EnvOCICASHA256Grant,
		EnvPlanSealPublicKey,
		EnvSealedPlanJobName,
		EnvGitURL,
		EnvGitBranch,
		EnvGitTag,
		EnvGitCommit,
		EnvGitPath,
		EnvExpectedContentDigest,
		EnvGitSelects,
		EnvGitHostGrant,
		EnvGitCASHA256Grant,
		EnvGitKnownHostsSHA256Grant,
		EnvGitCASourceFile,
		EnvGitKnownHostsSourceFile,
		EnvHTTPSURL,
		EnvHTTPSDigest,
		EnvHTTPSFormat,
		EnvHTTPSPath,
		EnvHTTPSSelects,
		EnvHTTPSHostGrant,
		EnvHTTPSCASHA256Grant,
		EnvHTTPSCASourceFile,
	)
}

type CommandSpec struct {
	Path string
	Args []string
	Env  []string
}

// BuildCommand constructs only fixed commands. It never uses a shell.
func BuildCommand(ptahBinary string, operation Operation, inputs Inputs) (CommandSpec, error) {
	if ptahBinary == "" {
		return CommandSpec{}, errors.New("ptah binary path is empty")
	}
	spec := CommandSpec{Path: ptahBinary}
	switch operation {
	case OperationResolve:
		if err := validateReference(inputs.RequestedReference, "requested reference"); err != nil {
			return CommandSpec{}, err
		}
		spec.Args = []string{"oci", "resolve", inputs.RequestedReference, "--format", "json"}
	case OperationVerify:
		if err := validateReference(inputs.RequestedReference, "requested reference"); err != nil {
			return CommandSpec{}, err
		}
		if err := validateReference(inputs.ResolvedReference, "resolved reference"); err != nil {
			return CommandSpec{}, err
		}
		if inputs.VerificationPolicyPath == "" {
			return CommandSpec{}, errors.New("verification policy path is empty")
		}
		spec.Args = []string{"oci", "verify", inputs.ResolvedReference, "--policy", inputs.VerificationPolicyPath, "--format", "json"}
	case OperationObserve:
		spec.Args = []string{"schema", "drift", "--format", "json"}
	case OperationPlan:
		// The plan is read from the file --output writes, and how the run
		// ended from the document --json prints. The file holds the bytes
		// `--dry-run` printed before, so a plan's content digest does not
		// depend on which of the two this runner asked for.
		if !strings.HasPrefix(inputs.PlanOutputPath, "/") {
			return CommandSpec{}, errors.New("plan needs the absolute path to save its plan to")
		}
		spec.Args = []string{"schema", "plan", "--output", inputs.PlanOutputPath, "--json"}
	case OperationApply:
		if inputs.PlanPath == "" {
			return CommandSpec{}, errors.New("reconstructed plan path is empty")
		}
		spec.Args = []string{"schema", "apply", "--plan", inputs.PlanPath, "--auto-approve", "--json"}
	case OperationMigrationHistory, OperationMigrationApply:
		// A local path, never a reference: validateMigrationsDir says why.
		if err := validateMigrationsDir(inputs.MigrationsDir); err != nil {
			return CommandSpec{}, err
		}
		verb := "status"
		if operation == OperationMigrationApply {
			verb = "up"
		}
		spec.Args = []string{"migrations", verb, "--migrations-dir", inputs.MigrationsDir, "--json"}
		// Only on the apply, and only when the resource asked. `migrations
		// status` does not take --tx-mode -- it reads a history and wraps
		// nothing -- so passing it there is an unknown flag and the command
		// fails.
		//
		// An unset mode leaves the flag off and Ptah picks, so a resource
		// stored before this field existed keeps running unchanged.
		if mode := strings.TrimSpace(inputs.TransactionMode); mode != "" &&
			operation == OperationMigrationApply {
			if err := validateTransactionMode(mode); err != nil {
				return CommandSpec{}, err
			}
			spec.Args = append(spec.Args, "--tx-mode", mode)
		}
		// Every apply names its approved sequence. Without it Ptah runs
		// whatever it selects, and a history restored backwards after the
		// approval turns the approved migrations into a longer list.
		if operation == OperationMigrationApply {
			if !strings.HasPrefix(inputs.ExpectedSequencePath, "/") {
				return CommandSpec{}, errors.New("migration apply needs the absolute path of its approved sequence")
			}
			spec.Args = append(spec.Args, "--expect-sequence", inputs.ExpectedSequencePath)
		}
		// The check runs only where the plan bound a shadow database. The
		// shadow's URL reaches Ptah as PTAH_SHADOW_DB in the child's
		// environment, never as an argument.
		if operation == OperationMigrationApply && inputs.ExpectedShadowDigest != "" {
			spec.Args = append(spec.Args, "--check-drift")
		}
	default:
		return CommandSpec{}, fmt.Errorf("unsupported operation %q", operation)
	}
	return spec, nil
}

// validateTransactionMode refuses a mode this build does not know.
//
// The runner assembles a command line for a process it does not control, so an
// unrecognized value is refused here rather than passed through: Ptah would
// reject it too, but as a child that failed for a reason this side would then
// have to read back out of a log.
func validateTransactionMode(mode string) error {
	switch mode {
	case "file", "none":
		return nil
	default:
		return fmt.Errorf("unsupported transaction mode %q", mode)
	}
}

// validateMigrationsDir refuses anything but an absolute local path.
//
// A reference is refused by name rather than passed through: Ptah accepts one,
// and accepting it here would hand the registry to the process holding the
// database credentials.
func validateMigrationsDir(directory string) error {
	if strings.TrimSpace(directory) == "" {
		return errors.New("migration directory is empty")
	}
	if strings.Contains(directory, "://") {
		return errors.New("migration directory must be a materialized local path, not a reference")
	}
	if !strings.HasPrefix(directory, "/") {
		return errors.New("migration directory must be an absolute path")
	}
	return nil
}

func buildInspectArtifactCommand(ptahBinary, resolvedReference string) (CommandSpec, error) {
	if err := validateReference(resolvedReference, "resolved reference"); err != nil {
		return CommandSpec{}, err
	}
	return CommandSpec{
		Path: ptahBinary,
		Args: []string{"oci", "inspect", resolvedReference, "--format", "json", "--no-referrers"},
	}, nil
}

func validateReference(reference, field string) error {
	if reference == "" {
		return fmt.Errorf("%s is empty", field)
	}
	if strings.TrimSpace(reference) != reference || strings.HasPrefix(reference, "-") || strings.ContainsRune(reference, '\x00') {
		return fmt.Errorf("%s is invalid", field)
	}
	if urlPattern.MatchString(reference) && redactURLPasswords(reference) != reference {
		return fmt.Errorf("%s must not contain credentials", field)
	}
	return nil
}

type Executor interface {
	Execute(ctx context.Context, spec CommandSpec, stdout, stderr io.Writer) (int, error)
}

// OSExecutor runs a child process and stops it the way the kubelet stops a
// container: SIGTERM first, and SIGKILL only if the child is still running
// StopDelay later.
//
// Ptah cancels its own work on the first SIGTERM and writes what it did before
// it exits, so a migration stopped between two files can say which files it
// applied. SIGKILL, which is what exec.CommandContext sends by default, ends
// the child where it stands and leaves nothing to read. A zero StopDelay keeps
// that default, because a SIGTERM with no bound on what follows would let a
// child that ignores it outlive every deadline the runner enforces.
//
// spec.Env is everything the child holds, and no list means no variable. The
// copy is never nil, because exec reads a nil Env as "inherit", which would hand
// the child every credential the runner holds.
type OSExecutor struct {
	StopDelay time.Duration
}

func (e OSExecutor) Execute(ctx context.Context, spec CommandSpec, stdout, stderr io.Writer) (int, error) {
	command := exec.CommandContext(ctx, spec.Path, spec.Args...)
	command.Env = append([]string{}, spec.Env...)
	command.Stdout = stdout
	command.Stderr = stderr
	if e.StopDelay > 0 {
		// Sent once. Ptah treats a second SIGTERM as a request to stop at
		// once, and the kill that follows the delay is that request already.
		command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
		// The delay also bounds how long a child that exited may leave its
		// output pipes open through a process it started, so what the child
		// wrote is read either way.
		command.WaitDelay = e.StopDelay
	}
	err := command.Run()
	if err == nil {
		return 0, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, ctxErr
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() >= 0 {
		return exitError.ExitCode(), nil
	}
	return -1, fmt.Errorf("execute ptah process: %w", err)
}
