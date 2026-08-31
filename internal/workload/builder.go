// Package workload builds the bounded Kubernetes Jobs used by the schema
// controller. It is deliberately a pure builder: credentials are represented
// only by namespaced Kubernetes references and are never read into controller
// memory.
package workload

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/ocireference"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	// LabelManagedBy identifies Jobs managed by this operator.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelComponent identifies the bounded schema-operation component.
	LabelComponent = "app.kubernetes.io/component"
	// LabelSchema associates a Job with its namespaced PtahSchema name.
	LabelSchema = "operator.ptah.dev/schema"
	// LabelOperation identifies the fixed operation executed by a Job.
	LabelOperation = "operator.ptah.dev/operation"
	// LabelOperationID carries a label-safe hash of the full operation ID.
	LabelOperationID = "operator.ptah.dev/operation-id"

	// AnnotationOperationID binds Pod logs to the full operation claim.
	AnnotationOperationID = "operator.ptah.dev/operation-id"
	// AnnotationInputFingerprint records the operation's immutable input hash.
	AnnotationInputFingerprint = "operator.ptah.dev/input-fingerprint"
	// AnnotationPtahVersion records the configured data-plane version.
	AnnotationPtahVersion = "operator.ptah.dev/ptah-version"
	// AnnotationPlanFingerprint records the exact approved plan binding.
	AnnotationPlanFingerprint = "operator.ptah.dev/plan-fingerprint"
	// AnnotationPlanContentDigest records the reconstructed plan byte digest.
	AnnotationPlanContentDigest = "operator.ptah.dev/plan-content-digest"

	mainContainerName   = "ptah"
	initContainerName   = "install-runner"
	fetchContainerName  = "fetch-schema"
	runnerVolumeName    = "runner"
	workVolumeName      = "work"
	fetchWorkVolumeName = "fetch-work"
	policyVolumeName    = "verification-policy"
	planVolumeName      = "plan"
	sourceVolumeName    = "schema-source"
	dockerVolumeName    = "registry-docker-config"
	caVolumeName        = "registry-ca"
	tlsVolumeName       = "registry-client-tls"

	runnerPath             = "/runner/ptah-runner"
	ptahBinaryPath         = "/usr/local/bin/ptah"
	workPath               = "/work"
	verificationPolicyPath = "/verification/policy.yaml"
	planPath               = "/plan"
	sourcePath             = "/source"
	sourceFilePath         = "/source/schema.hcl"
	fetchWorkPath          = "/fetch-work"
	dockerConfigPath       = "/credentials/docker"
	caFilePath             = "/credentials/ca/ca.pem"
	clientCertificatePath  = "/credentials/tls/tls.crt"
	clientKeyPath          = "/credentials/tls/tls.key"

	defaultActiveDeadlineSeconds int64 = 900
	runnerVolumeBytes                  = 64 << 20
	workVolumeBytes                    = 128 << 20
	sourceVolumeBytes                  = 64 << 20
	fetchWorkVolumeBytes               = 64 << 20
)

var (
	imageDigestPattern = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
	sha256Pattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Builder contains immutable execution-component bindings. Both images must
// be content-addressed so a resumed operation cannot silently run new code.
type Builder struct {
	ExecutorImage string
	RunnerImage   string
	PtahVersion   string
}

// Validate checks the immutable execution configuration before the manager
// starts accepting reconciliation work.
func (b Builder) Validate() error { return b.validate() }

// NameFor delegates to the package-level deterministic naming contract.
func (b Builder) NameFor(
	schema *operatorv1alpha1.PtahSchema,
	operation operatorv1alpha1.ActiveOperationStatus,
) (string, error) {
	if err := b.validate(); err != nil {
		return "", err
	}
	return NameFor(schema, operation)
}

// ExecutionBinding returns the immutable execution identity recorded in every
// plan and approval fingerprint.
func (b Builder) ExecutionBinding() (
	ptahVersion string,
	executorImage string,
	runnerImage string,
	protocolVersion int32,
) {
	return b.PtahVersion, b.ExecutorImage, b.RunnerImage, int32(runner.ProtocolVersion)
}

// NameFor returns the Job name bound to every field that distinguishes an
// active operation attempt. The status claim can persist this name before the
// controller creates the Job.
func NameFor(schema *operatorv1alpha1.PtahSchema, operation operatorv1alpha1.ActiveOperationStatus) (string, error) {
	if schema == nil || schema.Name == "" || schema.UID == "" {
		return "", errors.New("schema name and UID are required")
	}
	if err := validateOperation(operation); err != nil {
		return "", err
	}

	input := strings.Join([]string{
		string(schema.UID),
		string(operation.Type),
		operation.ID,
		operation.InputFingerprint,
		strconv.FormatInt(int64(operation.Attempt), 10),
	}, "\x00")
	digest := sha256.Sum256([]byte(input))
	suffix := hex.EncodeToString(digest[:8])
	prefix := "ptah-" + strings.ToLower(string(operation.Type)) + "-"
	available := 63 - len(prefix) - 1 - len(suffix)
	name := strings.Trim(schema.Name, "-")
	if len(name) > available {
		name = strings.TrimRight(name[:available], "-")
	}
	if name == "" {
		return "", errors.New("schema name cannot form a Job name")
	}
	return prefix + name + "-" + suffix, nil
}

// Build creates one immutable-intent Job. plan must be nil except for Apply,
// where it must be the committed current plan.
func (b Builder) Build(
	schema *operatorv1alpha1.PtahSchema,
	operation operatorv1alpha1.ActiveOperationStatus,
	plan *operatorv1alpha1.PtahSchemaPlan,
) (*batchv1.Job, error) {
	if err := b.validate(); err != nil {
		return nil, err
	}
	if err := validateSchema(schema); err != nil {
		return nil, err
	}
	name, err := NameFor(schema, operation)
	if err != nil {
		return nil, err
	}
	if operation.JobName != "" && operation.JobName != name {
		return nil, fmt.Errorf("active operation Job name %q does not match deterministic name %q", operation.JobName, name)
	}

	jobInput := buildInput{schema: schema, operation: operation, plan: plan}
	if err := jobInput.validate(); err != nil {
		return nil, err
	}
	if operation.Type == operatorv1alpha1.OperationApply &&
		(plan.Spec.PtahVersion != b.PtahVersion ||
			plan.Spec.ExecutorImage != b.ExecutorImage ||
			plan.Spec.RunnerImage != b.RunnerImage ||
			plan.Spec.RunnerProtocolVersion != int32(runner.ProtocolVersion)) {
		return nil, errors.New("apply plan execution-component binding is stale")
	}
	environment, volumes, mounts, annotations, err := jobInput.dataPlane()
	if err != nil {
		return nil, err
	}

	labels := map[string]string{
		LabelManagedBy:   "ptah-operator",
		LabelComponent:   "schema-operation",
		LabelSchema:      schema.Name,
		LabelOperation:   strings.ToLower(string(operation.Type)),
		LabelOperationID: shortLabelHash(operation.ID),
	}
	annotations[AnnotationOperationID] = operation.ID
	annotations[AnnotationInputFingerprint] = operation.InputFingerprint
	annotations[AnnotationPtahVersion] = b.PtahVersion

	deadline := activeDeadlineSeconds(schema)
	if operation.Type == operatorv1alpha1.OperationApply && operation.ExecutionNotAfter != nil {
		deadline = int64(operation.ExecutionNotAfter.Sub(operation.StartedAt.Time) / time.Second)
	}
	backoffLimit := int32(0)
	falseValue := false
	trueValue := true
	nonRootID := int64(65532)
	terminationGrace := int64(30)
	if operation.Type == operatorv1alpha1.OperationApply && operation.TerminationGracePeriodSeconds > 0 {
		terminationGrace = operation.TerminationGracePeriodSeconds
	}
	fsGroupPolicy := corev1.FSGroupChangeOnRootMismatch
	resources := *schema.Spec.Execution.Resources.DeepCopy()
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
	if operation.Type == operatorv1alpha1.OperationObserve || operation.Type == operatorv1alpha1.OperationPlan {
		fetch, fetchVolumes, err := b.schemaFetch(schema, operation, resources, &falseValue, &trueValue, &nonRootID)
		if err != nil {
			return nil, err
		}
		initContainers = append(initContainers, fetch)
		volumes = append(volumes, fetchVolumes...)
		mounts = append(mounts, corev1.VolumeMount{Name: sourceVolumeName, MountPath: sourcePath, ReadOnly: true})
	}

	controller := true
	blockDeletion := true
	podReplacementPolicy := batchv1.Failed
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   schema.Namespace,
			Name:        name,
			Labels:      copyMap(labels),
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         operatorv1alpha1.GroupVersion.String(),
				Kind:               "PtahSchema",
				Name:               schema.Name,
				UID:                schema.UID,
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
					ServiceAccountName:            schema.Spec.Execution.ServiceAccountName,
					ImagePullSecrets:              append([]corev1.LocalObjectReference(nil), schema.Spec.Execution.ImagePullSecrets...),
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
							"--operation", strings.ToLower(string(operation.Type)),
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
					NodeSelector:      copyMap(schema.Spec.Execution.NodeSelector),
					Tolerations:       append([]corev1.Toleration(nil), schema.Spec.Execution.Tolerations...),
					Affinity:          schema.Spec.Execution.Affinity.DeepCopy(),
					RuntimeClassName:  copyStringPointer(schema.Spec.Execution.RuntimeClassName),
					PriorityClassName: schema.Spec.Execution.PriorityClassName,
				},
			},
		},
	}
	return job, nil
}

type buildInput struct {
	schema    *operatorv1alpha1.PtahSchema
	operation operatorv1alpha1.ActiveOperationStatus
	plan      *operatorv1alpha1.PtahSchemaPlan
}

func (i buildInput) validate() error {
	switch i.operation.Type {
	case operatorv1alpha1.OperationResolve:
		if i.plan != nil {
			return errors.New("resolve operation cannot carry a plan")
		}
		return validateRequestedReference(i.schema.Spec.Desired.OCIRef)
	case operatorv1alpha1.OperationVerify:
		if i.plan != nil {
			return errors.New("verify operation cannot carry a plan")
		}
		if err := validateRequestedReference(i.schema.Spec.Desired.OCIRef); err != nil {
			return err
		}
		if _, err := validatedResolvedReference(i.schema); err != nil {
			return err
		}
		if i.operation.VerificationPolicyUID == "" || !sha256Pattern.MatchString(i.operation.VerificationPolicyDigest) {
			return errors.New("verify operation requires an immutable verification policy binding")
		}
		return validatePolicyReference(i.schema.Spec.Desired.VerificationPolicyFrom)
	case operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan:
		if i.plan != nil {
			return fmt.Errorf("%s operation cannot carry a plan", i.operation.Type)
		}
		if i.operation.Target == nil || i.operation.Target.URLFrom.Name == "" || i.operation.Target.URLFrom.Key == "" {
			return fmt.Errorf("%s operation requires an immutable target Secret binding", i.operation.Type)
		}
		if i.operation.Source == nil {
			return fmt.Errorf("%s operation requires immutable artifact access", i.operation.Type)
		}
		if err := validateArtifactAccessBinding(*i.operation.Source); err != nil {
			return err
		}
		if !sha256Pattern.MatchString(i.operation.CoordinationDigest) {
			return fmt.Errorf("%s operation requires a valid coordination binding", i.operation.Type)
		}
		if i.operation.Type == operatorv1alpha1.OperationPlan &&
			(i.operation.LeaseDurationSeconds < 5 || i.operation.LeaseDurationSeconds > 86460) {
			return errors.New("plan operation lease duration is outside the supported range")
		}
		return nil
	case operatorv1alpha1.OperationApply:
		if err := validateApplyPlan(i.schema, i.plan); err != nil {
			return err
		}
		if i.operation.Target == nil || i.operation.Target.URLFrom.Name == "" || i.operation.Target.URLFrom.Key == "" {
			return errors.New("apply operation requires an immutable target Secret binding")
		}
		if !sha256Pattern.MatchString(i.operation.CoordinationDigest) ||
			i.operation.CoordinationDigest != i.plan.Spec.CoordinationDigest {
			return errors.New("apply operation coordination digest does not match the immutable plan")
		}
		if i.operation.TargetIdentityDigest != i.plan.Spec.TargetIdentityDigest {
			return errors.New("apply operation target identity does not match the immutable plan")
		}
		if i.operation.LeaseDurationSeconds < 5 || i.operation.LeaseDurationSeconds > 86460 {
			return errors.New("apply operation lease duration is outside the supported range")
		}
		if i.operation.StartedAt.IsZero() {
			return errors.New("apply operation requires an immutable start time")
		}
		if i.operation.DispatchNotAfter == nil || i.operation.ExecutionNotAfter == nil ||
			!i.operation.DispatchNotAfter.After(i.operation.StartedAt.Time) ||
			i.operation.ExecutionNotAfter.Before(i.operation.DispatchNotAfter) ||
			i.operation.TerminationGracePeriodSeconds < 1 || i.operation.TerminationGracePeriodSeconds > 300 ||
			i.operation.ExecutionNotAfter.Add(time.Duration(i.operation.TerminationGracePeriodSeconds)*time.Second).
				After(i.operation.StartedAt.Add(time.Duration(i.operation.LeaseDurationSeconds)*time.Second)) {
			return errors.New("apply operation requires a valid immutable execution horizon")
		}
		return nil
	default:
		return fmt.Errorf("unsupported operation %q", i.operation.Type)
	}
}

func (i buildInput) dataPlane() (
	[]corev1.EnvVar,
	[]corev1.Volume,
	[]corev1.VolumeMount,
	map[string]string,
	error,
) {
	environment := []corev1.EnvVar{
		literalEnv("HOME", workPath),
		literalEnv("TMPDIR", workPath),
		literalEnv(runner.EnvOperationID, i.operation.ID),
	}
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	annotations := map[string]string{}

	mainUsesRegistry := i.operation.Type == operatorv1alpha1.OperationResolve ||
		i.operation.Type == operatorv1alpha1.OperationVerify
	if mainUsesRegistry {
		environment = append(environment, literalEnv("PTAH_PLAIN_HTTP", strconv.FormatBool(i.schema.Spec.Desired.Transport.PlainHTTP)))
		var err error
		environment, volumes, mounts, err = addRegistryAccess(
			i.schema.Spec.Desired.RegistryAuthFrom,
			i.schema.Spec.Desired.Transport,
			environment,
			volumes,
			mounts,
		)
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}

	switch i.operation.Type {
	case operatorv1alpha1.OperationResolve:
		environment = append(environment, literalEnv(runner.EnvRequestedReference, i.schema.Spec.Desired.OCIRef))
	case operatorv1alpha1.OperationVerify:
		resolved, _ := validatedResolvedReference(i.schema)
		environment = append(environment,
			literalEnv(runner.EnvRequestedReference, i.schema.Spec.Desired.OCIRef),
			literalEnv(runner.EnvResolvedReference, resolved),
			literalEnv(runner.EnvVerificationPolicy, verificationPolicyPath),
			literalEnv(runner.EnvExpectedArtifactType, dataplane.SchemaArtifactType),
		)
		volumes = append(volumes, policyVolume(i.schema.Spec.Desired.VerificationPolicyFrom))
		mounts = append(mounts, corev1.VolumeMount{Name: policyVolumeName, MountPath: "/verification", ReadOnly: true})
	case operatorv1alpha1.OperationObserve:
		target := *i.operation.Target
		environment = append(environment,
			databaseEnv(runner.EnvDatabaseURL, target.URLFrom),
			literalEnv(runner.EnvExpectedDatabaseEngine, string(target.Engine)),
			literalEnv(runner.EnvCoordinationDigest, i.operation.CoordinationDigest),
			literalEnv(runner.EnvSchemaFile, sourceFilePath),
			literalEnv("PTAH_CONNECT_TIMEOUT", durationOrDefault(i.operation.ObservationConnectTimeout.Duration, 10*time.Second)),
			literalEnv("PTAH_LOCK_TIMEOUT", durationOrDefault(i.operation.ObservationLockTimeout.Duration, 30*time.Second)),
			literalEnv("PTAH_SEVERITY", stringOrDefault(i.operation.ObservationSeverity, "all")),
		)
	case operatorv1alpha1.OperationPlan:
		target := *i.operation.Target
		environment = append(environment,
			databaseEnv(runner.EnvDatabaseURL, target.URLFrom),
			literalEnv(runner.EnvExpectedDatabaseEngine, string(target.Engine)),
			literalEnv(runner.EnvCoordinationDigest, i.operation.CoordinationDigest),
			literalEnv(runner.EnvSchemaFile, sourceFilePath),
			literalEnv("PTAH_CONNECT_TIMEOUT", durationOrDefault(i.operation.ObservationConnectTimeout.Duration, 10*time.Second)),
			literalEnv("PTAH_LOCK_TIMEOUT", durationOrDefault(i.operation.ObservationLockTimeout.Duration, 30*time.Second)),
		)
		if i.operation.ObservationDev != nil {
			environment = append(environment, databaseEnv(runner.EnvDevelopmentDatabaseURL, i.operation.ObservationDev.URLFrom))
		}
		if len(i.operation.ObservationExclude) > 0 {
			environment = append(environment, literalEnv("PTAH_EXCLUDE", encodeStringArray(i.operation.ObservationExclude)))
		}
	case operatorv1alpha1.OperationApply:
		target := operatorv1alpha1.DatabaseTargetBinding{
			Engine:  i.schema.Spec.Target.Engine,
			URLFrom: i.schema.Spec.Target.URLFrom,
		}
		if i.operation.Target != nil {
			target = *i.operation.Target
		}
		projections, err := planstore.VolumeSources(i.plan)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("build plan projection: %w", err)
		}
		mode := int32(0o440)
		volumes = append(volumes, corev1.Volume{
			Name: planVolumeName,
			VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources:     projections,
				DefaultMode: &mode,
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: planVolumeName, MountPath: planPath, ReadOnly: true})
		environment = append(environment,
			databaseEnv(runner.EnvDatabaseURL, target.URLFrom),
			literalEnv(runner.EnvExpectedDatabaseEngine, string(target.Engine)),
			literalEnv(runner.EnvCoordinationDigest, i.operation.CoordinationDigest),
			literalEnv(runner.EnvPlanDir, planPath),
			literalEnv(runner.EnvExpectedPlanContentDigest, i.plan.Spec.ContentDigest),
			literalEnv(runner.EnvExpectedCoordinationDigest, i.plan.Spec.CoordinationDigest),
			literalEnv(runner.EnvExpectedTargetIdentityDigest, i.plan.Spec.TargetIdentityDigest),
			literalEnv(runner.EnvDispatchNotAfter, i.operation.DispatchNotAfter.UTC().Format(time.RFC3339Nano)),
			literalEnv(runner.EnvExecutionNotAfter, i.operation.ExecutionNotAfter.UTC().Format(time.RFC3339Nano)),
			literalEnv("PTAH_CONNECT_TIMEOUT", durationOrDefault(i.schema.Spec.Execution.ConnectTimeout.Duration, 10*time.Second)),
			literalEnv("PTAH_LOCK_TIMEOUT", durationOrDefault(i.schema.Spec.Policy.LockTimeout.Duration, 30*time.Second)),
			literalEnv("PTAH_TX_MODE", stringOrDefault(i.schema.Spec.Policy.TransactionMode, "file")),
		)
		annotations[AnnotationPlanFingerprint] = i.plan.Spec.Fingerprint
		annotations[AnnotationPlanContentDigest] = i.plan.Spec.ContentDigest
	}

	sort.Slice(environment, func(left, right int) bool { return environment[left].Name < environment[right].Name })
	return environment, volumes, mounts, annotations, nil
}

// schemaFetch isolates registry credentials from the database-bearing
// container. It materializes only the already verified, digest-pinned schema
// into a bounded shared volume.
func (b Builder) schemaFetch(
	schema *operatorv1alpha1.PtahSchema,
	operation operatorv1alpha1.ActiveOperationStatus,
	resources corev1.ResourceRequirements,
	falseValue, trueValue *bool,
	nonRootID *int64,
) (corev1.Container, []corev1.Volume, error) {
	source, err := schemaFetchBinding(schema, operation)
	if err != nil {
		return corev1.Container{}, nil, err
	}
	environment := []corev1.EnvVar{
		literalEnv("HOME", fetchWorkPath),
		literalEnv("TMPDIR", fetchWorkPath),
		literalEnv("PTAH_PLAIN_HTTP", strconv.FormatBool(source.Transport.PlainHTTP)),
	}
	volumes := []corev1.Volume{
		{Name: sourceVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: memoryVolume(sourceVolumeBytes)}},
		{Name: fetchWorkVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: memoryVolume(fetchWorkVolumeBytes)}},
	}
	mounts := []corev1.VolumeMount{
		{Name: sourceVolumeName, MountPath: sourcePath},
		{Name: fetchWorkVolumeName, MountPath: fetchWorkPath},
	}
	environment, registryVolumes, registryMounts, err := addRegistryAccess(
		source.RegistryAuthFrom,
		source.Transport,
		environment,
		nil,
		nil,
	)
	if err != nil {
		return corev1.Container{}, nil, err
	}
	volumes = append(volumes, registryVolumes...)
	mounts = append(mounts, registryMounts...)
	sort.Slice(environment, func(left, right int) bool { return environment[left].Name < environment[right].Name })
	return corev1.Container{
		Name:            fetchContainerName,
		Image:           b.ExecutorImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{ptahBinaryPath},
		Args:            []string{"schema", "pull", source.ResolvedReference, "--out", sourceFilePath},
		WorkingDir:      fetchWorkPath,
		Env:             environment,
		Resources:       resources,
		SecurityContext: hardenedContainerContext(falseValue, trueValue, nonRootID),
		VolumeMounts:    mounts,
	}, volumes, nil
}

func schemaFetchBinding(
	schema *operatorv1alpha1.PtahSchema,
	operation operatorv1alpha1.ActiveOperationStatus,
) (operatorv1alpha1.OCIArtifactAccessBinding, error) {
	if (operation.Type == operatorv1alpha1.OperationObserve || operation.Type == operatorv1alpha1.OperationPlan) && operation.Source != nil {
		source := *operation.Source.DeepCopy()
		if err := validateArtifactAccessBinding(source); err != nil {
			return operatorv1alpha1.OCIArtifactAccessBinding{}, err
		}
		if pending := schema.Status.PendingObservation; pending != nil && source.Digest != pending.Plan.ArtifactDigest {
			return operatorv1alpha1.OCIArtifactAccessBinding{}, errors.New("post-apply artifact access does not match the applied plan")
		}
		return source, nil
	}
	resolved, err := validatedVerifiedSource(schema)
	if err != nil {
		return operatorv1alpha1.OCIArtifactAccessBinding{}, err
	}
	source := operatorv1alpha1.OCIArtifactAccessBinding{
		ResolvedReference: resolved,
		Digest:            schema.Status.Source.Digest,
	}
	if schema.Spec.Desired.RegistryAuthFrom != nil {
		source.RegistryAuthFrom = schema.Spec.Desired.RegistryAuthFrom.DeepCopy()
	}
	schema.Spec.Desired.Transport.DeepCopyInto(&source.Transport)
	if err := validateArtifactAccessBinding(source); err != nil {
		return operatorv1alpha1.OCIArtifactAccessBinding{}, err
	}
	return source, nil
}

func validateArtifactAccessBinding(source operatorv1alpha1.OCIArtifactAccessBinding) error {
	if err := ocireference.ValidatePinned(source.ResolvedReference, source.Digest); err != nil {
		return errors.New("artifact access binding must contain a matching immutable SHA-256 reference")
	}
	return nil
}

func addRegistryAccess(
	auth *operatorv1alpha1.RegistryAuthSource,
	transport operatorv1alpha1.OCITransportSpec,
	environment []corev1.EnvVar,
	volumes []corev1.Volume,
	mounts []corev1.VolumeMount,
) ([]corev1.EnvVar, []corev1.Volume, []corev1.VolumeMount, error) {
	if auth != nil {
		if strings.TrimSpace(auth.Name) == "" {
			return nil, nil, nil, errors.New("registry auth Secret name is required")
		}
		mode := auth.Mode
		if mode == "" {
			mode = operatorv1alpha1.RegistryAuthEnvironment
		}
		switch mode {
		case operatorv1alpha1.RegistryAuthEnvironment:
			environment = append(environment,
				optionalSecretEnv("PTAH_OCI_USERNAME", auth.Name, stringOrDefault(auth.UsernameKey, "username")),
				optionalSecretEnv(runner.EnvOCIPassword, auth.Name, stringOrDefault(auth.PasswordKey, "password")),
				optionalSecretEnv(runner.EnvOCIToken, auth.Name, stringOrDefault(auth.TokenKey, "token")),
				optionalSecretEnv("PTAH_OCI_REGISTRY", auth.Name, stringOrDefault(auth.RegistryKey, "registry")),
			)
		case operatorv1alpha1.RegistryAuthDockerConfigJSON:
			key := stringOrDefault(auth.DockerConfigJSONKey, corev1.DockerConfigJsonKey)
			mode := int32(0o440)
			volumes = append(volumes, corev1.Volume{
				Name: dockerVolumeName,
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: auth.Name,
					Items:      []corev1.KeyToPath{{Key: key, Path: "config.json", Mode: &mode}},
				}},
			})
			mounts = append(mounts, corev1.VolumeMount{Name: dockerVolumeName, MountPath: dockerConfigPath, ReadOnly: true})
			environment = append(environment, literalEnv("DOCKER_CONFIG", dockerConfigPath))
		default:
			return nil, nil, nil, fmt.Errorf("unsupported registry auth mode %q", mode)
		}
	}

	if ca := transport.CAFrom; ca != nil {
		if ca.Name == "" || ca.Key == "" {
			return nil, nil, nil, errors.New("registry CA ConfigMap name and key are required")
		}
		mode := int32(0o440)
		volumes = append(volumes, corev1.Volume{
			Name: caVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: ca.LocalObjectReference,
				Items:                []corev1.KeyToPath{{Key: ca.Key, Path: "ca.pem", Mode: &mode}},
				Optional:             ca.Optional,
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: caVolumeName, MountPath: "/credentials/ca", ReadOnly: true})
		environment = append(environment, literalEnv("PTAH_OCI_CA_FILE", caFilePath))
	}

	if tls := transport.ClientCertificateFrom; tls != nil {
		if tls.Name == "" {
			return nil, nil, nil, errors.New("registry client-certificate Secret name is required")
		}
		certificateKey := stringOrDefault(tls.CertificateKey, corev1.TLSCertKey)
		privateKey := stringOrDefault(tls.PrivateKeyKey, corev1.TLSPrivateKeyKey)
		mode := int32(0o440)
		volumes = append(volumes, corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: tls.Name,
				Items: []corev1.KeyToPath{
					{Key: certificateKey, Path: "tls.crt", Mode: &mode},
					{Key: privateKey, Path: "tls.key", Mode: &mode},
				},
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: tlsVolumeName, MountPath: "/credentials/tls", ReadOnly: true})
		environment = append(environment,
			literalEnv("PTAH_OCI_CLIENT_CERT", clientCertificatePath),
			literalEnv("PTAH_OCI_CLIENT_KEY", clientKeyPath),
		)
	}
	return environment, volumes, mounts, nil
}

func (b Builder) validate() error {
	if !imageDigestPattern.MatchString(b.ExecutorImage) {
		return errors.New("executor image must be pinned by a lowercase SHA-256 digest")
	}
	if !imageDigestPattern.MatchString(b.RunnerImage) {
		return errors.New("runner image must be pinned by a lowercase SHA-256 digest")
	}
	if strings.TrimSpace(b.PtahVersion) == "" || strings.TrimSpace(b.PtahVersion) != b.PtahVersion {
		return errors.New("ptah version is required")
	}
	return nil
}

func validateSchema(schema *operatorv1alpha1.PtahSchema) error {
	if schema == nil {
		return errors.New("schema is required")
	}
	if schema.Namespace == "" || schema.Name == "" || schema.UID == "" {
		return errors.New("schema namespace, name, and UID are required")
	}
	if schema.DeletionTimestamp != nil {
		return errors.New("cannot create an operation Job for a deleting schema")
	}
	if schema.Spec.Target.URLFrom.Name == "" || schema.Spec.Target.URLFrom.Key == "" {
		return errors.New("target Secret name and key are required")
	}
	if _, err := schemaCoordinationDigest(schema); err != nil {
		return err
	}
	if schema.Spec.Execution.ActiveDeadlineSeconds != 0 &&
		(schema.Spec.Execution.ActiveDeadlineSeconds < 30 || schema.Spec.Execution.ActiveDeadlineSeconds > 86400) {
		return errors.New("active deadline must be between 30 and 86400 seconds")
	}
	return nil
}

func validateOperation(operation operatorv1alpha1.ActiveOperationStatus) error {
	switch operation.Type {
	case operatorv1alpha1.OperationResolve,
		operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve,
		operatorv1alpha1.OperationPlan,
		operatorv1alpha1.OperationApply:
	default:
		return fmt.Errorf("unsupported operation %q", operation.Type)
	}
	if strings.TrimSpace(operation.ID) == "" {
		return errors.New("active operation ID is required")
	}
	if !sha256Pattern.MatchString(operation.InputFingerprint) {
		return errors.New("active operation input fingerprint must be a lowercase SHA-256 digest")
	}
	if operation.Attempt < 1 {
		return errors.New("active operation attempt must be positive")
	}
	return nil
}

func validateRequestedReference(reference string) error {
	if _, err := ocireference.Parse(reference); err != nil {
		return errors.New("requested schema reference must be an OCI reference without credentials, whitespace, or query data")
	}
	return nil
}

func validatedResolvedReference(schema *operatorv1alpha1.PtahSchema) (string, error) {
	reference := schema.Status.Source.ResolvedReference
	if err := ocireference.ValidateResolution(
		schema.Status.Source.RequestedReference,
		reference,
		schema.Status.Source.Digest,
	); err != nil {
		return "", errors.New("resolved schema reference must bind the requested repository and recorded SHA-256 digest")
	}
	return reference, nil
}

func validatedVerifiedSource(schema *operatorv1alpha1.PtahSchema) (string, error) {
	reference, err := validatedResolvedReference(schema)
	if err != nil {
		return "", err
	}
	if !schema.Status.Source.Verified {
		return "", errors.New("database operation requires a verified source")
	}
	if schema.Status.Source.ArtifactType != dataplane.SchemaArtifactType {
		return "", errors.New("database operation requires the schema artifact type")
	}
	return reference, nil
}

func validatePolicyReference(selector corev1.ConfigMapKeySelector) error {
	if selector.Name == "" || selector.Key == "" {
		return errors.New("verification policy ConfigMap name and key are required")
	}
	return nil
}

func validateApplyPlan(schema *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) error {
	if plan == nil {
		return errors.New("apply operation requires a plan")
	}
	if plan.Namespace != schema.Namespace || plan.Spec.SchemaRef.Name != schema.Name || plan.Spec.SchemaRef.UID != schema.UID {
		return errors.New("apply plan does not belong to the schema UID in this namespace")
	}
	if plan.DeletionTimestamp != nil {
		return errors.New("cannot apply a deleting plan")
	}
	ready := meta.FindStatusCondition(plan.Status.Conditions, operatorv1alpha1.ConditionPlanStorageReady)
	if plan.Status.ObservedGeneration != plan.Generation || ready == nil || ready.Status != metav1.ConditionTrue ||
		ready.ObservedGeneration != plan.Generation {
		return errors.New("apply plan storage is not committed Ready for its current generation")
	}
	if len(plan.Status.PublishedChunks) != len(plan.Spec.Chunks) {
		return errors.New("apply plan does not bind every published chunk")
	}
	for index, published := range plan.Status.PublishedChunks {
		if published.Index != int32(index) || published.Name != plan.Spec.Chunks[index].Name || published.UID == "" {
			return errors.New("apply plan has an invalid published-chunk binding")
		}
	}
	if _, err := validatedVerifiedSource(schema); err != nil {
		return fmt.Errorf("apply plan source is no longer verified: %w", err)
	}
	if schema.Status.Plan == nil {
		return errors.New("schema has no current plan binding")
	}
	coordinationDigest, err := schemaCoordinationDigest(schema)
	if err != nil {
		return err
	}
	if coordinationDigest != plan.Spec.CoordinationDigest {
		return errors.New("apply plan coordination key is stale")
	}
	current := schema.Status.Plan
	applyPolicy := schema.Spec.Policy.Apply
	if applyPolicy == "" {
		applyPolicy = operatorv1alpha1.ApplyPolicyOnApproval
	}
	if applyPolicy == operatorv1alpha1.ApplyPolicyNever {
		return errors.New("current policy forbids apply operations")
	}
	if plan.Spec.Destructive && !schema.Spec.Policy.AllowDestructive {
		return errors.New("current policy forbids destructive plans")
	}
	if (applyPolicy == operatorv1alpha1.ApplyPolicyOnApproval || plan.Spec.Destructive) &&
		(current.Approval == nil || current.Approval.Name == "" || current.Approval.UID == "") {
		return errors.New("current policy requires an immutable plan approval")
	}
	if current.Name != plan.Name || current.UID != plan.UID ||
		current.Fingerprint != plan.Spec.Fingerprint || current.ContentDigest != plan.Spec.ContentDigest ||
		current.ArtifactDigest != plan.Spec.ArtifactDigest ||
		current.CoordinationDigest != plan.Spec.CoordinationDigest ||
		current.TargetIdentityDigest != plan.Spec.TargetIdentityDigest ||
		current.ActualStateFingerprint != plan.Spec.ActualStateFingerprint ||
		current.DesiredStateFingerprint != plan.Spec.DesiredStateFingerprint ||
		current.PolicyFingerprint != plan.Spec.PolicyFingerprint ||
		current.VerificationPolicyUID != plan.Spec.VerificationPolicyUID ||
		current.VerificationPolicyDigest != plan.Spec.VerificationPolicyDigest ||
		current.PtahVersion != plan.Spec.PtahVersion || current.ExecutorImage != plan.Spec.ExecutorImage ||
		current.RunnerImage != plan.Spec.RunnerImage || current.RunnerProtocolVersion != plan.Spec.RunnerProtocolVersion ||
		current.Destructive != plan.Spec.Destructive || current.StatementCount != plan.Spec.StatementCount {
		return errors.New("apply plan is not the schema current plan")
	}
	if plan.Spec.ArtifactDigest != schema.Status.Source.Digest ||
		plan.Spec.VerificationPolicyUID != schema.Status.Source.VerificationPolicyUID ||
		plan.Spec.VerificationPolicyDigest != schema.Status.Source.VerificationPolicyDigest ||
		plan.Spec.CoordinationDigest != schema.Status.Target.CoordinationDigest ||
		plan.Spec.TargetIdentityDigest != schema.Status.Target.IdentityDigest {
		return errors.New("apply plan source or target binding is stale")
	}
	if !sha256Pattern.MatchString(plan.Spec.ContentDigest) || !sha256Pattern.MatchString(plan.Spec.Fingerprint) ||
		!sha256Pattern.MatchString(plan.Spec.CoordinationDigest) {
		return errors.New("apply plan has an invalid digest binding")
	}
	return nil
}

func schemaCoordinationDigest(schema *operatorv1alpha1.PtahSchema) (string, error) {
	digest, err := fingerprint.DatabaseCoordinationDigest(
		string(schema.Spec.Target.Engine),
		schema.Spec.Target.CoordinationKey,
	)
	if err != nil {
		return "", fmt.Errorf("derive database coordination digest: %w", err)
	}
	return digest, nil
}

func policyVolume(selector corev1.ConfigMapKeySelector) corev1.Volume {
	mode := int32(0o440)
	return corev1.Volume{
		Name: policyVolumeName,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: selector.LocalObjectReference,
			Items:                []corev1.KeyToPath{{Key: selector.Key, Path: "policy.yaml", Mode: &mode}},
			Optional:             selector.Optional,
		}},
	}
}

func baseVolumes() []corev1.Volume {
	return []corev1.Volume{
		{Name: runnerVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: memoryVolume(runnerVolumeBytes)}},
		{Name: workVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: memoryVolume(workVolumeBytes)}},
	}
}

func memoryVolume(size int64) *corev1.EmptyDirVolumeSource {
	return &corev1.EmptyDirVolumeSource{
		Medium:    corev1.StorageMediumMemory,
		SizeLimit: quantity(size),
	}
}

func hardenedContainerContext(falseValue, trueValue *bool, nonRootID *int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: falseValue,
		ReadOnlyRootFilesystem:   trueValue,
		RunAsNonRoot:             trueValue,
		RunAsUser:                nonRootID,
		RunAsGroup:               nonRootID,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func databaseEnv(name string, selector corev1.SecretKeySelector) corev1.EnvVar {
	copy := *selector.DeepCopy()
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &copy}}
}

func optionalSecretEnv(name, secret, key string) corev1.EnvVar {
	optional := true
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret},
			Key:                  key,
			Optional:             &optional,
		}},
	}
}

func literalEnv(name, value string) corev1.EnvVar { return corev1.EnvVar{Name: name, Value: value} }

func durationOrDefault(value, fallback time.Duration) string {
	if value <= 0 {
		value = fallback
	}
	return value.String()
}

func stringOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func encodeStringArray(values []string) string {
	var encoded strings.Builder
	writer := csv.NewWriter(&encoded)
	_ = writer.Write(values)
	writer.Flush()
	return strings.TrimSuffix(encoded.String(), "\n")
}

func shortLabelHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:8])
}

// OperationIDLabelValue returns the bounded label value placed on every Pod
// for one full operation ID. Controllers must still verify AnnotationOperationID
// after selecting by this collision-resistant hash.
func OperationIDLabelValue(value string) string { return shortLabelHash(value) }

func activeDeadlineSeconds(schema *operatorv1alpha1.PtahSchema) int64 {
	if schema.Spec.Execution.ActiveDeadlineSeconds > 0 {
		return schema.Spec.Execution.ActiveDeadlineSeconds
	}
	return defaultActiveDeadlineSeconds
}

func quantity(bytes int64) *resource.Quantity {
	value := *resource.NewQuantity(bytes, resource.BinarySI)
	return &value
}

func copyMap[M ~map[string]string](source M) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func copyStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// ProtocolVersion exposes the runner binding used when a plan is constructed.
const ProtocolVersion = runner.ProtocolVersion
