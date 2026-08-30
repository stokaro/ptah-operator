package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
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
	// EnvDatabaseURL carries the target database credential to the child only.
	EnvDatabaseURL = "PTAH_DB_URL"
	// EnvDevelopmentDatabaseURL carries an optional development database credential.
	EnvDevelopmentDatabaseURL = "PTAH_DEV_URL"
	// EnvOCIPassword carries a registry password to the child only.
	EnvOCIPassword = "PTAH_OCI_PASSWORD"
	// EnvOCIToken carries a registry identity token to the child only.
	EnvOCIToken = "PTAH_OCI_TOKEN"
	// EnvSchemaFile supplies the desired schema to drift and plan operations.
	EnvSchemaFile = "PTAH_SCHEMA_FILE"

	envOperationID          = EnvOperationID
	envRequestedReference   = EnvRequestedReference
	envResolvedReference    = EnvResolvedReference
	envVerificationPolicy   = EnvVerificationPolicy
	envExpectedArtifactType = EnvExpectedArtifactType
	envPlanDir              = EnvPlanDir
	envExpectedPlanDigest   = EnvExpectedPlanContentDigest
	envDatabaseURL          = EnvDatabaseURL
	envSchemaFile           = EnvSchemaFile
)

// Inputs are the runner-specific environment values used to construct one of
// the fixed Ptah commands. Normal Ptah environment variables remain in Env.
type Inputs struct {
	OperationID               string
	RequestedReference        string
	ResolvedReference         string
	VerificationPolicyPath    string
	ExpectedArtifactType      string
	PlanDir                   string
	ExpectedPlanContentDigest string
	PlanPath                  string
}

func InputsFromEnvironment(environment []string) Inputs {
	values := environmentMap(environment)
	return Inputs{
		OperationID:               values[envOperationID],
		RequestedReference:        values[envRequestedReference],
		ResolvedReference:         values[envResolvedReference],
		VerificationPolicyPath:    values[envVerificationPolicy],
		ExpectedArtifactType:      values[envExpectedArtifactType],
		PlanDir:                   values[envPlanDir],
		ExpectedPlanContentDigest: values[envExpectedPlanDigest],
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

func childEnvironment(environment []string) []string {
	return environmentWithout(
		environment,
		EnvOperationID,
		EnvRequestedReference,
		EnvResolvedReference,
		EnvVerificationPolicy,
		EnvExpectedArtifactType,
		EnvPlanDir,
		EnvExpectedPlanContentDigest,
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
		if inputs.VerificationPolicyPath == "" {
			return CommandSpec{}, errors.New("verification policy path is empty")
		}
		spec.Args = []string{"oci", "verify", inputs.RequestedReference, "--policy", inputs.VerificationPolicyPath, "--format", "json"}
	case OperationObserve:
		spec.Args = []string{"schema", "drift", "--format", "json"}
	case OperationPlan:
		spec.Args = []string{"schema", "plan", "--dry-run"}
	case OperationApply:
		if inputs.PlanPath == "" {
			return CommandSpec{}, errors.New("reconstructed plan path is empty")
		}
		spec.Args = []string{"schema", "apply", "--plan", inputs.PlanPath, "--auto-approve"}
	default:
		return CommandSpec{}, fmt.Errorf("unsupported operation %q", operation)
	}
	return spec, nil
}

func buildInspectSchemaCommand(ptahBinary string) CommandSpec {
	return CommandSpec{Path: ptahBinary, Args: []string{"schema", "inspect", "--format", "json"}}
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

type OSExecutor struct{}

func (OSExecutor) Execute(ctx context.Context, spec CommandSpec, stdout, stderr io.Writer) (int, error) {
	command := exec.CommandContext(ctx, spec.Path, spec.Args...)
	command.Env = append([]string(nil), spec.Env...)
	command.Stdout = stdout
	command.Stderr = stderr
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
