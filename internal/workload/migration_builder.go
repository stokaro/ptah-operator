package workload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	migrationFetchContainerName = "fetch-migrations"

	// migrationsPath is the directory the fetch container writes the migration
	// artifact into, inside the same shared memory volume the schema path uses.
	// The container that holds the database URL reads it and never reaches the
	// registry itself.
	migrationsPath = sourcePath + "/migrations"

	// LabelMigration associates a Job with its namespaced PtahMigration name,
	// as LabelSchema does for a schema operation.
	LabelMigration = "operator.ptah.run/migration"
)

// NameForMigration returns the Job name bound to every field that
// distinguishes one migration operation attempt from another.
//
// The claim persists this name before the Job exists, which is what lets a
// controller that restarted mid-dispatch tell the Job it created from one it
// has not created yet.
func NameForMigration(
	migration *operatorv1alpha1.PtahMigration,
	operation operatorv1alpha1.MigrationOperationStatus,
) (string, error) {
	if migration == nil || migration.Name == "" || migration.UID == "" {
		return "", errors.New("migration name and UID are required")
	}
	if err := validateMigrationOperation(operation); err != nil {
		return "", err
	}

	input := strings.Join([]string{
		string(migration.UID),
		string(operation.Type),
		operation.ID,
		operation.ExecutionBindingID,
		operation.InputFingerprint,
		strconv.FormatInt(int64(operation.Attempt), 10),
	}, "\x00")
	digest := sha256.Sum256([]byte(input))
	suffix := hex.EncodeToString(digest[:8])
	prefix := "ptah-m-" + strings.ToLower(string(operation.Type)) + "-"
	available := 63 - len(prefix) - 1 - len(suffix)
	name := strings.Trim(migration.Name, "-")
	if len(name) > available {
		name = strings.TrimRight(name[:available], "-")
	}
	if name == "" {
		return "", errors.New("migration name cannot form a Job name")
	}
	return prefix + name + "-" + suffix, nil
}

// BuildMigration creates the Job for one already-persisted migration claim.
//
// It reads no Secret. Every credential reaches the Pod as a selector the
// kubelet projects, and the container holding the database URL holds no
// registry credentials: what it reads was written by the fetch container beside
// it.
func (b Builder) BuildMigration(
	migration *operatorv1alpha1.PtahMigration,
	operation operatorv1alpha1.MigrationOperationStatus,
	plan *operatorv1alpha1.PtahMigrationPlan,
) (*batchv1.Job, error) {
	if err := b.validate(); err != nil {
		return nil, err
	}
	if err := validateMigration(migration); err != nil {
		return nil, err
	}
	binding := migration.Status.ExecutionBinding
	if binding.ControllerStateVersion != b.ControllerStateVersion ||
		binding.PtahVersion != b.PtahVersion ||
		binding.ExecutorImage != b.ExecutorImage ||
		binding.RunnerProtocolVersion != int32(runner.ProtocolVersion) ||
		operation.ExecutionBindingID != binding.Epoch {
		return nil, errors.New("migration operation execution binding is stale")
	}
	name, err := NameForMigration(migration, operation)
	if err != nil {
		return nil, err
	}
	if operation.JobName != "" && operation.JobName != name {
		return nil, fmt.Errorf(
			"migration operation Job name %q does not match deterministic name %q",
			operation.JobName, name,
		)
	}

	environment, volumes, mounts, annotations, err := migrationDataPlane(migration, operation, plan)
	if err != nil {
		return nil, err
	}

	job := operationJob{
		family:             migrationOperations,
		owner:              migration,
		name:               name,
		operationType:      string(operation.Type),
		runnerOperation:    string(mutationlifecycle.MigrationOperation(operation.Type).Runner),
		operationID:        operation.ID,
		inputFingerprint:   operation.InputFingerprint,
		executionBindingID: operation.ExecutionBindingID,
		admissionSnapshot:  operation.AdmissionSnapshot,
		execution:          migration.Spec.Execution,
		mutating:           mutationlifecycle.MigrationOperation(operation.Type).Mutating,
		startedAt:          operation.StartedAt,
		executionNotAfter:  operation.ExecutionNotAfter,
		env:                environment,
		volumes:            volumes,
		mounts:             mounts,
		annotations:        annotations,
	}
	if migrationReadsArtifactBytes(operation.Type) {
		job.fetch = &artifactFetchRequest{
			source:        *operation.Source,
			containerName: migrationFetchContainerName,
			args:          []string{"migrations", "pull", operation.Source.ResolvedReference, "--out", migrationsPath},
		}
	}
	return b.buildOperationJob(job)
}

// migrationReadsArtifactBytes reports whether an operation needs the artifact's
// files on disk. Resolve and Verify talk to the registry themselves and hold no
// database credential; History and Apply hold the database and read what the
// fetch container left behind.
func migrationReadsArtifactBytes(operation operatorv1alpha1.MigrationOperationType) bool {
	return operation == operatorv1alpha1.MigrationOperationHistory ||
		operation == operatorv1alpha1.MigrationOperationApply
}

func migrationDataPlane(
	migration *operatorv1alpha1.PtahMigration,
	operation operatorv1alpha1.MigrationOperationStatus,
	plan *operatorv1alpha1.PtahMigrationPlan,
) ([]corev1.EnvVar, []corev1.Volume, []corev1.VolumeMount, map[string]string, error) {
	environment := []corev1.EnvVar{
		literalEnv("HOME", workPath),
		literalEnv("TMPDIR", workPath),
		literalEnv(runner.EnvOperationID, operation.ID),
	}
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	annotations := map[string]string{}

	switch operation.Type {
	case operatorv1alpha1.MigrationOperationResolve, operatorv1alpha1.MigrationOperationVerify:
		// Only these two reach the registry from the main container, and
		// neither is given a database credential: the registry and the database
		// are never open to the same process.
		environment = append(environment,
			literalEnv("PTAH_PLAIN_HTTP", strconv.FormatBool(migration.Spec.Artifact.Transport.PlainHTTP)),
			literalEnv(runner.EnvRequestedReference, migration.Spec.Artifact.OCIRef),
		)
		var err error
		environment, volumes, mounts, err = addRegistryAccess(
			migration.Spec.Artifact.OCIRef,
			migration.Spec.Artifact.RegistryAuthFrom,
			migration.Spec.Artifact.Transport,
			environment, volumes, mounts,
		)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if operation.Type == operatorv1alpha1.MigrationOperationVerify {
			if operation.Source == nil {
				return nil, nil, nil, nil, errors.New("migration verify carries no resolved source binding")
			}
			if err := validatePolicyReference(migration.Spec.Artifact.VerificationPolicyFrom); err != nil {
				return nil, nil, nil, nil, err
			}
			environment = append(environment,
				literalEnv(runner.EnvResolvedReference, operation.Source.ResolvedReference),
				literalEnv(runner.EnvVerificationPolicy, verificationPolicyPath),
				literalEnv(runner.EnvExpectedArtifactType, dataplane.MigrationArtifactType),
			)
			volumes = append(volumes, policyVolume(migration.Spec.Artifact.VerificationPolicyFrom))
			mounts = append(mounts, corev1.VolumeMount{Name: policyVolumeName, MountPath: "/verification", ReadOnly: true})
		}
	case operatorv1alpha1.MigrationOperationHistory, operatorv1alpha1.MigrationOperationApply:
		target := operation.Target
		if target == nil {
			return nil, nil, nil, nil, errors.New("migration operation reaching the database carries no target binding")
		}
		environment = append(environment,
			databaseEnv(runner.EnvDatabaseURL, target.URLFrom),
			literalEnv(runner.EnvExpectedDatabaseEngine, string(target.Engine)),
			literalEnv(runner.EnvCoordinationDigest, operation.CoordinationDigest),
			literalEnv(runner.EnvMigrationsDir, migrationsPath),
			literalEnv("PTAH_CONNECT_TIMEOUT", durationOrDefault(migration.Spec.Execution.ConnectTimeout.Duration, 10*time.Second)),
			// The advisory lock the engine takes for the whole run, which is
			// what policy.lockTimeout is about. Ptah's PTAH_LOCK_TIMEOUT is a
			// different bound -- the per-migration lock wait -- and setting
			// that one here would leave the documented field doing something
			// else than it says.
			literalEnv("PTAH_MIGRATION_LOCK_TIMEOUT", durationOrDefault(migration.Spec.Policy.LockTimeout.Duration, 5*time.Minute)),
		)
		// Only when the resource asked for one, and the runner carries it only
		// to the apply -- `migrations status` has no --tx-mode. An unset mode leaves the
		// variable off, the runner leaves the flag off, and Ptah chooses.
		if mode := strings.TrimSpace(migration.Spec.Policy.TransactionMode); mode != "" {
			environment = append(environment, literalEnv(runner.EnvTransactionMode, mode))
		}
		if operation.Type == operatorv1alpha1.MigrationOperationApply {
			if operation.PlanRef == nil {
				return nil, nil, nil, nil, errors.New("migration apply carries no plan reference")
			}
			if operation.DispatchNotAfter == nil || operation.ExecutionNotAfter == nil {
				return nil, nil, nil, nil, errors.New("migration apply carries no dispatch and execution bounds")
			}
			// The runner refuses a mutating child that names none of this, so
			// the Job that carries it is where the approved plan reaches the
			// data plane. The schema Apply mounts its plan bytes; a migration
			// has no bytes to mount -- `migrations up` reads the artifact
			// directory -- so what travels is the sequence it approved, its
			// digest, and the history it was approved against. The runner
			// digests the sequence again and hands it to `migrations up
			// --expect-sequence`, which compares it with what it selects under
			// the migration lock.
			if err := validateMigrationApplyPlan(migration, operation, plan); err != nil {
				return nil, nil, nil, nil, err
			}
			entries := migrationSequenceEntries(plan.Spec.Migrations)
			sequenceDigest, err := fingerprint.MigrationSequenceDigest(entries)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("digest the approved migration sequence: %w", err)
			}
			sequence, err := encodeMigrationSequence(entries)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			environment = append(environment,
				literalEnv(runner.EnvExpectedTargetIdentityDigest, plan.Spec.TargetIdentityDigest),
				literalEnv(runner.EnvExpectedCoordinationDigest, plan.Spec.CoordinationDigest),
				literalEnv(runner.EnvExpectedMigrationSequence, sequence),
				literalEnv(runner.EnvExpectedMigrationSequenceDigest, sequenceDigest),
				literalEnv(runner.EnvExpectedMigrationHistoryFingerprint, plan.Spec.HistoryFingerprint),
				literalEnv(runner.EnvDispatchNotAfter, operation.DispatchNotAfter.UTC().Format(time.RFC3339Nano)),
				literalEnv(runner.EnvExecutionNotAfter, operation.ExecutionNotAfter.UTC().Format(time.RFC3339Nano)),
			)
		}
	}
	return environment, volumes, mounts, annotations, nil
}

// MigrationSequenceDigest is fingerprint.MigrationSequenceDigest over a plan's
// sequence.
//
// It lives here rather than beside the rest of the plan derivation because the
// Job that executes a plan has to carry this digest, and internal/migrationplan
// is built on top of this package. That package's SequenceDigest calls this,
// and the runner digests the sequence it receives with the same function, so
// there is one rule rather than copies that can drift.
func MigrationSequenceDigest(planned []operatorv1alpha1.PlannedMigration) (string, error) {
	return fingerprint.MigrationSequenceDigest(migrationSequenceEntries(planned))
}

// MaxEncodedMigrationSequence is the most bytes the approved sequence may take
// in the Apply Job's environment. The kernel refuses to start a process with
// any one environment string over 128 KiB, and a Job whose container cannot
// start leaves a run nobody can account for, so the bound sits below that with
// room for the variable's name. A plan is refused at publication rather than
// here, by EncodeMigrationSequence.
const MaxEncodedMigrationSequence = 120 << 10

// EncodeMigrationSequence is the approved sequence as the Apply Job carries it,
// or an error when it would not fit. Plan publication calls it so a sequence
// no Job could carry never becomes a plan anyone approves.
func EncodeMigrationSequence(planned []operatorv1alpha1.PlannedMigration) (string, error) {
	return encodeMigrationSequence(migrationSequenceEntries(planned))
}

func encodeMigrationSequence(entries []fingerprint.SequenceEntry) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	// Escaping <, > and & would make the same key six bytes a character for
	// nothing: the value is read by the runner, never by a browser.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(entries); err != nil {
		return "", fmt.Errorf("encode the approved migration sequence: %w", err)
	}
	encoded := strings.TrimSuffix(buffer.String(), "\n")
	if len(encoded) > MaxEncodedMigrationSequence {
		return "", fmt.Errorf("the approved sequence encodes to %d bytes, over the %d an Apply Job can carry",
			len(encoded), MaxEncodedMigrationSequence)
	}
	return encoded, nil
}

// migrationSequenceEntries is a plan's sequence in the shape the digest binds.
func migrationSequenceEntries(planned []operatorv1alpha1.PlannedMigration) []fingerprint.SequenceEntry {
	entries := make([]fingerprint.SequenceEntry, 0, len(planned))
	for _, migration := range planned {
		entries = append(entries, fingerprint.SequenceEntry{
			Version:         migration.Version,
			VersionKey:      migration.VersionKey,
			Checksum:        migration.Checksum,
			Checkpoint:      migration.Checkpoint,
			TransactionMode: migration.TransactionMode,
		})
	}
	return entries
}

// validateMigrationApplyPlan refuses to build an Apply Job whose plan is not
// the immutable one the claim named.
//
// A Job is the only thing that reaches the database, so the plan it carries is
// checked here rather than trusted from whoever called: a claim pointing at one
// plan and a Job carrying another would put an unapproved sequence's identity
// in front of the runner.
func validateMigrationApplyPlan(
	migration *operatorv1alpha1.PtahMigration,
	operation operatorv1alpha1.MigrationOperationStatus,
	plan *operatorv1alpha1.PtahMigrationPlan,
) error {
	if plan == nil {
		return errors.New("migration apply requires the plan that authorized it")
	}
	if plan.DeletionTimestamp != nil {
		return errors.New("cannot apply a deleting migration plan")
	}
	if plan.Namespace != migration.Namespace ||
		plan.Name != operation.PlanRef.Name ||
		plan.UID != operation.PlanRef.UID {
		return errors.New("migration apply plan is not the one the claim named")
	}
	if plan.Spec.MigrationRef.Name != migration.Name || plan.Spec.MigrationRef.UID != migration.UID {
		return errors.New("migration apply plan belongs to another migration")
	}
	if !sha256Pattern.MatchString(plan.Spec.TargetIdentityDigest) {
		return errors.New("migration apply plan carries no valid target identity digest")
	}
	if !sha256Pattern.MatchString(plan.Spec.CoordinationDigest) ||
		plan.Spec.CoordinationDigest != operation.CoordinationDigest {
		return errors.New("migration apply coordination digest does not match the immutable plan")
	}
	if !sha256Pattern.MatchString(plan.Spec.HistoryFingerprint) {
		return errors.New("migration apply plan carries no valid history fingerprint")
	}
	return nil
}

func validateMigration(migration *operatorv1alpha1.PtahMigration) error {
	if migration == nil {
		return errors.New("migration is required")
	}
	if migration.Name == "" || migration.Namespace == "" || migration.UID == "" {
		return errors.New("migration name, namespace and UID are required")
	}
	if migration.Status.ExecutionBinding == nil {
		return errors.New("migration has no durable execution binding")
	}
	if !executionBindingIDPattern.MatchString(migration.Status.ExecutionBinding.Epoch) {
		return errors.New("migration execution binding epoch is invalid")
	}
	return validateRequestedReference(migration.Spec.Artifact.OCIRef)
}

func validateMigrationOperation(operation operatorv1alpha1.MigrationOperationStatus) error {
	if !mutationlifecycle.MigrationOperation(operation.Type).Known {
		return fmt.Errorf("unsupported migration operation %q", operation.Type)
	}
	if !sha256Pattern.MatchString(operation.ID) {
		return errors.New("migration operation ID must be a lowercase SHA-256 digest")
	}
	if !sha256Pattern.MatchString(operation.InputFingerprint) {
		return errors.New("migration operation input fingerprint must be a lowercase SHA-256 digest")
	}
	if operation.Attempt < 1 {
		return errors.New("migration operation attempt must be positive")
	}
	if !executionBindingIDPattern.MatchString(operation.ExecutionBindingID) {
		return errors.New("migration operation execution binding ID is invalid")
	}
	if migrationReadsArtifactBytes(operation.Type) {
		if operation.Source == nil {
			return errors.New("migration operation reading the artifact carries no source binding")
		}
		if err := validateArtifactAccessBinding(*operation.Source); err != nil {
			return err
		}
	}
	return nil
}
