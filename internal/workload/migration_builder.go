package workload

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
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
	if binding.ControllerImage != b.ControllerImage ||
		binding.ControllerRevision != b.ControllerRevision ||
		binding.ControllerStateVersion != b.ControllerStateVersion ||
		binding.PtahVersion != b.PtahVersion ||
		binding.ExecutorImage != b.ExecutorImage ||
		binding.RunnerImage != b.RunnerImage ||
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
	deadline, err := boundedDeadline(
		activeDeadlineSeconds(migration.Spec.Execution),
		operation.Type == operatorv1alpha1.MigrationOperationApply,
		operation.StartedAt,
		operation.ExecutionNotAfter,
		JobDeadlineGrace,
	)
	if err != nil {
		return nil, err
	}

	labels := map[string]string{
		LabelManagedBy:   "ptah-operator",
		LabelComponent:   ComponentMigrationOperation,
		LabelMigration:   migration.Name,
		LabelOperation:   strings.ToLower(string(operation.Type)),
		LabelOperationID: shortLabelHash(operation.ID),
	}
	annotations[AnnotationOperationID] = operation.ID
	annotations[AnnotationInputFingerprint] = operation.InputFingerprint
	annotations[AnnotationPtahVersion] = b.PtahVersion
	annotations[AnnotationExecutionBindingID] = operation.ExecutionBindingID
	annotations[AnnotationControllerImage] = b.ControllerImage
	annotations[AnnotationControllerRevision] = b.ControllerRevision
	annotations[AnnotationControllerStateVersion] = strconv.FormatInt(int64(b.ControllerStateVersion), 10)
	if operation.AdmissionSnapshot != nil {
		if !sha256Pattern.MatchString(operation.AdmissionSnapshot.Digest) ||
			!sha256Pattern.MatchString(operation.AdmissionSnapshot.TemplateDigest) {
			return nil, errors.New("Pod admission snapshot and template digests must be lowercase SHA-256 digests")
		}
		annotations[AnnotationAdmissionSnapshotDigest] = operation.AdmissionSnapshot.Digest
	}

	backoffLimit := int32(0)
	falseValue := false
	trueValue := true
	nonRootID := int64(65532)
	terminationGrace := int64(30)
	fsGroupPolicy := corev1.FSGroupChangeOnRootMismatch
	resources := *migration.Spec.Execution.Resources.DeepCopy()

	initContainers := []corev1.Container{{
		Name:            initContainerName,
		Image:           b.RunnerImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/ptah-runner"},
		Args:            []string{"--install-to", runnerPath},
		Resources:       resources,
		SecurityContext: hardenedContainerContext(&falseValue, &trueValue, &nonRootID),
		VolumeMounts:    []corev1.VolumeMount{{Name: runnerVolumeName, MountPath: "/runner"}},
	}}
	if migrationReadsArtifactBytes(operation.Type) {
		guard, fetch, fetchVolumes, fetchErr := b.artifactFetch(
			*operation.Source,
			migrationFetchContainerName,
			[]string{"migrations", "pull", operation.Source.ResolvedReference, "--out", migrationsPath},
			resources,
			&falseValue, &trueValue, &nonRootID,
		)
		if fetchErr != nil {
			return nil, fetchErr
		}
		initContainers = append(initContainers, guard, fetch)
		volumes = append(volumes, fetchVolumes...)
		mounts = append(mounts, corev1.VolumeMount{Name: sourceVolumeName, MountPath: sourcePath, ReadOnly: true})
	}

	controller := true
	blockDeletion := true
	podReplacementPolicy := batchv1.Failed
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   migration.Namespace,
			Name:        name,
			Labels:      copyMap(labels),
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         operatorv1alpha1.GroupVersion.String(),
				Kind:               "PtahMigration",
				Name:               migration.Name,
				UID:                migration.UID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockDeletion,
			}},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &deadline,
			PodReplacementPolicy:  &podReplacementPolicy,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: copyMap(labels), Annotations: copyMap(annotations)},
				Spec: corev1.PodSpec{
					ActiveDeadlineSeconds:         &deadline,
					AutomountServiceAccountToken:  &falseValue,
					EnableServiceLinks:            &falseValue,
					ServiceAccountName:            executionServiceAccountName(migration.Spec.Execution),
					ImagePullSecrets:              append([]corev1.LocalObjectReference(nil), migration.Spec.Execution.ImagePullSecrets...),
					RestartPolicy:                 corev1.RestartPolicyNever,
					TerminationGracePeriodSeconds: &terminationGrace,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:        &trueValue,
						RunAsUser:           &nonRootID,
						RunAsGroup:          &nonRootID,
						FSGroup:             &nonRootID,
						FSGroupChangePolicy: &fsGroupPolicy,
						SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					InitContainers: initContainers,
					Containers: []corev1.Container{{
						Name:            mainContainerName,
						Image:           b.ExecutorImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{runnerPath},
						Args: []string{
							"--ptah-binary", ptahBinaryPath,
							"--max-result-bytes", strconv.FormatInt(runner.DefaultMaxResultBytes, 10),
							"--max-plan-bytes", strconv.FormatInt(runner.DefaultMaxPlanBytes, 10),
							"--operation", string(migrationRunnerOperation(operation.Type)),
						},
						WorkingDir:      workPath,
						Env:             environment,
						Resources:       resources,
						SecurityContext: hardenedContainerContext(&falseValue, &trueValue, &nonRootID),
						VolumeMounts: append([]corev1.VolumeMount{
							{Name: runnerVolumeName, MountPath: "/runner", ReadOnly: true},
							{Name: workVolumeName, MountPath: workPath},
						}, mounts...),
					}},
					Volumes:           append(baseVolumes(), volumes...),
					NodeSelector:      copyMap(migration.Spec.Execution.NodeSelector),
					Tolerations:       append([]corev1.Toleration(nil), migration.Spec.Execution.Tolerations...),
					Affinity:          migration.Spec.Execution.Affinity.DeepCopy(),
					RuntimeClassName:  copyStringPointer(migration.Spec.Execution.RuntimeClassName),
					PriorityClassName: migration.Spec.Execution.PriorityClassName,
				},
			},
		},
	}
	bindStableAPIDefaults(job)
	return job, nil
}

// migrationReadsArtifactBytes reports whether an operation needs the artifact's
// files on disk. Resolve and Verify talk to the registry themselves and hold no
// database credential; History and Apply hold the database and read what the
// fetch container left behind.
func migrationReadsArtifactBytes(operation operatorv1alpha1.MigrationOperationType) bool {
	return operation == operatorv1alpha1.MigrationOperationHistory ||
		operation == operatorv1alpha1.MigrationOperationApply
}

func migrationRunnerOperation(operation operatorv1alpha1.MigrationOperationType) runner.Operation {
	switch operation {
	case operatorv1alpha1.MigrationOperationResolve:
		return runner.OperationResolve
	case operatorv1alpha1.MigrationOperationVerify:
		return runner.OperationVerify
	case operatorv1alpha1.MigrationOperationHistory:
		return runner.OperationMigrationHistory
	default:
		return runner.OperationMigrationApply
	}
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
		// variable off, the runner leaves the flag off, and Ptah chooses --
		// which is what every migration did before the field existed.
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
			// directory -- so what travels is the plan's identity: the
			// sequence it approved and the history it approved it against.
			if err := validateMigrationApplyPlan(migration, operation, plan); err != nil {
				return nil, nil, nil, nil, err
			}
			sequenceDigest, err := MigrationSequenceDigest(plan.Spec.Migrations)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("digest the approved migration sequence: %w", err)
			}
			environment = append(environment,
				literalEnv(runner.EnvExpectedTargetIdentityDigest, plan.Spec.TargetIdentityDigest),
				literalEnv(runner.EnvExpectedCoordinationDigest, plan.Spec.CoordinationDigest),
				literalEnv(runner.EnvExpectedMigrationSequenceDigest, sequenceDigest),
				literalEnv(runner.EnvExpectedMigrationHistoryFingerprint, plan.Spec.HistoryFingerprint),
				literalEnv(runner.EnvDispatchNotAfter, operation.DispatchNotAfter.UTC().Format(time.RFC3339Nano)),
				literalEnv(runner.EnvExecutionNotAfter, operation.ExecutionNotAfter.UTC().Format(time.RFC3339Nano)),
			)
		}
	}

	sort.Slice(environment, func(left, right int) bool { return environment[left].Name < environment[right].Name })
	return environment, volumes, mounts, annotations, nil
}

// MigrationSequenceDigest binds a plan to the exact sequence it carries, in the
// order it carries it: a plan that applies the same migrations in another order
// is a different plan.
//
// It lives here rather than beside the rest of the plan derivation because the
// Job that executes a plan has to carry this digest, and internal/migrationplan
// is built on top of this package. That package's SequenceDigest calls this, so
// there is one rule rather than two copies that can drift.
func MigrationSequenceDigest(planned []operatorv1alpha1.PlannedMigration) (string, error) {
	if len(planned) == 0 {
		return "", errors.New("a plan carries at least one migration")
	}
	entries := make([]map[string]any, 0, len(planned))
	for _, migration := range planned {
		entries = append(entries, map[string]any{
			"version":          migration.Version,
			"version_key":      migration.VersionKey,
			"checksum":         migration.Checksum,
			"checkpoint":       migration.Checkpoint,
			"transaction_mode": migration.TransactionMode,
		})
	}
	return fingerprint.DigestCanonicalJSON(entries)
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
	switch operation.Type {
	case operatorv1alpha1.MigrationOperationResolve,
		operatorv1alpha1.MigrationOperationVerify,
		operatorv1alpha1.MigrationOperationHistory,
		operatorv1alpha1.MigrationOperationApply:
	default:
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
