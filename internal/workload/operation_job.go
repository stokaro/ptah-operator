package workload

import (
	"errors"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	// operationUserID is the non-root user and group every operation
	// container runs as.
	operationUserID int64 = 65532
	// defaultTerminationGracePeriodSeconds is what an operation Pod gets when
	// its family names no grace of its own: runner.DefaultTerminationGracePeriod
	// in the unit the Pod spec takes.
	defaultTerminationGracePeriodSeconds = int64(runner.DefaultTerminationGracePeriod / time.Second)
)

// operationFamily is what is fixed for every Job one resource kind dispatches.
type operationFamily struct {
	ownerKind string
	// component is the LabelComponent value, and subjectLabel the label that
	// carries the owner's name.
	component    string
	subjectLabel string
	// deadlineGrace is how long a mutating Job outlives the window that
	// authorized it; JobDeadlineGrace says why only a migration takes one.
	deadlineGrace time.Duration
}

var (
	schemaOperations = operationFamily{
		ownerKind:    "PtahSchema",
		component:    ComponentSchemaOperation,
		subjectLabel: LabelSchema,
	}
	migrationOperations = operationFamily{
		ownerKind:     "PtahMigration",
		component:     ComponentMigrationOperation,
		subjectLabel:  LabelMigration,
		deadlineGrace: JobDeadlineGrace,
	}
)

// operationJob is what one resource family decides about an operation Job:
// whose it is, which operation it runs, and what that operation reads.
//
// Everything else in the Job belongs to buildOperationJob: the hardening, the
// ServiceAccount token setting, the resources and scheduling copied from
// spec.execution, the runner install, the deadlines and termination settings,
// the labels, and the annotation envelope the controller-write guard and the
// pod-intent webhook read back. Both families build their Jobs through it, so
// neither can drift from the other on any of that.
type operationJob struct {
	family operationFamily
	owner  metav1.Object
	name   string

	// operationType is the claim's operation type, lowercased into
	// LabelOperation. runnerOperation is what the runner is told to run.
	operationType   string
	runnerOperation string

	operationID        string
	inputFingerprint   string
	executionBindingID string
	admissionSnapshot  *operatorv1alpha1.PodAdmissionSnapshot

	execution operatorv1alpha1.ExecutionSpec

	// mutating bounds the Job by the claim's execution window rather than by
	// spec.execution.activeDeadlineSeconds.
	mutating          bool
	startedAt         metav1.Time
	executionNotAfter *metav1.Time
	// terminationGracePeriodSeconds overrides the default when positive.
	terminationGracePeriodSeconds int64

	// env, volumes and mounts are the operation container's inputs, and
	// annotations the family's own additions to the envelope.
	env         []corev1.EnvVar
	volumes     []corev1.Volume
	mounts      []corev1.VolumeMount
	annotations map[string]string

	// fetch, when set, materializes an artifact before the operation starts.
	fetch *artifactFetchRequest
}

// artifactFetchRequest names the artifact an operation reads from disk and how
// the executor pulls it. artifactFetch turns it into the guard and fetch
// containers.
type artifactFetchRequest struct {
	source        operatorv1alpha1.OCIArtifactAccessBinding
	containerName string
	args          []string
}

// buildOperationJob is the one place an operation Job and its Pod are shaped.
// runnerProtocolEnv names the runner protocol this build of the manager speaks.
func runnerProtocolEnv() corev1.EnvVar {
	return literalEnv(runner.EnvRunnerProtocolVersion, strconv.Itoa(runner.ProtocolVersion))
}

func (b Builder) buildOperationJob(spec operationJob) (*batchv1.Job, error) {
	// What spec.execution.podMetadata declares goes under the operator's own
	// metadata, never over it: the envelope below is written last, and a
	// declared key in a reserved namespace is refused before that. The
	// declaration is carried whole on the Job and on its Pod template, so
	// the Pod-intent webhook, which holds a Pod to its template, admits
	// exactly the Pod the declaration describes.
	declaredLabels, declaredAnnotations, err := declaredPodMetadata(spec.execution.PodMetadata)
	if err != nil {
		return nil, err
	}
	annotations := make(map[string]string, len(declaredAnnotations)+len(spec.annotations)+8)
	maps.Copy(annotations, declaredAnnotations)
	maps.Copy(annotations, spec.annotations)
	annotations[AnnotationOperationID] = spec.operationID
	annotations[AnnotationInputFingerprint] = spec.inputFingerprint
	annotations[AnnotationPtahVersion] = b.PtahVersion
	annotations[AnnotationExecutionBindingID] = spec.executionBindingID
	annotations[AnnotationControllerImage] = b.ControllerImage
	annotations[AnnotationControllerRevision] = b.ControllerRevision
	annotations[AnnotationControllerStateVersion] = strconv.FormatInt(int64(b.ControllerStateVersion), 10)
	if snapshot := spec.admissionSnapshot; snapshot != nil {
		if !sha256Pattern.MatchString(snapshot.Digest) || !sha256Pattern.MatchString(snapshot.TemplateDigest) {
			return nil, errors.New("the Pod admission snapshot and template digests must be lowercase SHA-256 digests")
		}
		annotations[AnnotationAdmissionSnapshotDigest] = snapshot.Digest
	}
	if spec.mutating {
		MarkMutatingOperation(annotations)
	}
	labels := make(map[string]string, len(declaredLabels)+5)
	maps.Copy(labels, declaredLabels)
	maps.Copy(labels, map[string]string{
		LabelManagedBy:           "ptah-operator",
		LabelComponent:           spec.family.component,
		spec.family.subjectLabel: spec.owner.GetName(),
		LabelOperation:           strings.ToLower(spec.operationType),
		LabelOperationID:         shortLabelHash(spec.operationID),
	})

	deadline, err := boundedDeadline(
		activeDeadlineSeconds(spec.execution),
		spec.mutating,
		spec.startedAt,
		spec.executionNotAfter,
		spec.family.deadlineGrace,
	)
	if err != nil {
		return nil, err
	}
	terminationGrace := defaultTerminationGracePeriodSeconds
	if spec.terminationGracePeriodSeconds > 0 {
		terminationGrace = spec.terminationGracePeriodSeconds
	}

	resources := *spec.execution.Resources.DeepCopy()
	environment := append([]corev1.EnvVar(nil), spec.env...)
	// The protocol this manager speaks. The runner comes from
	// execution.runnerImage, which nothing ties to this manager, and it
	// refuses a Job built for a protocol other than its own before the
	// executor starts.
	environment = append(environment, runnerProtocolEnv())
	if spec.mutating {
		// The same number the Pod spec carries below. The runner sizes the
		// time it gives a stopped child against it, and refuses a mutating
		// Pod that does not say.
		environment = append(environment,
			literalEnv(runner.EnvTerminationGracePeriod, strconv.FormatInt(terminationGrace, 10)))
	}
	volumes := spec.volumes
	mounts := spec.mounts
	initContainers := []corev1.Container{{
		Name:            initContainerName,
		Image:           b.RunnerImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/ptah-runner"},
		Args:            []string{"--install-to", runnerPath},
		Resources:       resources,
		SecurityContext: hardenedContainerContext(),
		VolumeMounts:    []corev1.VolumeMount{{Name: runnerVolumeName, MountPath: "/runner"}},
	}}
	if fetch := spec.fetch; fetch != nil {
		guard, fetchContainer, fetchVolumes, err := b.artifactFetch(fetch.source, fetch.containerName, fetch.args, resources)
		if err != nil {
			return nil, err
		}
		initContainers = append(initContainers, guard, fetchContainer)
		volumes = append(volumes, fetchVolumes...)
		mounts = append(mounts, corev1.VolumeMount{Name: sourceVolumeName, MountPath: sourcePath, ReadOnly: true})
	}
	// Sorted so the rebuilt Job compares equal whatever order a family
	// appended its variables in.
	sort.Slice(environment, func(left, right int) bool { return environment[left].Name < environment[right].Name })

	backoffLimit := int32(0)
	falseValue := false
	trueValue := true
	userID := operationUserID
	fsGroupPolicy := corev1.FSGroupChangeOnRootMismatch
	controller := true
	blockDeletion := true
	podReplacementPolicy := batchv1.Failed
	execution := spec.execution
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   spec.owner.GetNamespace(),
			Name:        spec.name,
			Labels:      copyMap(labels),
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         operatorv1alpha1.GroupVersion.String(),
				Kind:               spec.family.ownerKind,
				Name:               spec.owner.GetName(),
				UID:                spec.owner.GetUID(),
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
					ServiceAccountName:            executionServiceAccountName(execution),
					ImagePullSecrets:              append([]corev1.LocalObjectReference(nil), execution.ImagePullSecrets...),
					RestartPolicy:                 corev1.RestartPolicyNever,
					TerminationGracePeriodSeconds: &terminationGrace,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:        &trueValue,
						RunAsUser:           &userID,
						RunAsGroup:          &userID,
						FSGroup:             &userID,
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
							"--operation", spec.runnerOperation,
						},
						WorkingDir:      workPath,
						Env:             environment,
						Resources:       resources,
						SecurityContext: hardenedContainerContext(),
						VolumeMounts: append([]corev1.VolumeMount{
							{Name: runnerVolumeName, MountPath: "/runner", ReadOnly: true},
							{Name: workVolumeName, MountPath: workPath},
						}, mounts...),
						// The runner writes its result summary here, and the
						// kubelet copies it into Pod status.
						TerminationMessagePath:   runner.TerminationMessagePath,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
					}},
					Volumes:           append(baseVolumes(), volumes...),
					NodeSelector:      copyMap(execution.NodeSelector),
					Tolerations:       append([]corev1.Toleration(nil), execution.Tolerations...),
					Affinity:          execution.Affinity.DeepCopy(),
					RuntimeClassName:  copyStringPointer(execution.RuntimeClassName),
					PriorityClassName: execution.PriorityClassName,
				},
			},
		},
	}
	if b.ResultEndpoint != "" {
		var err error
		if b.ResultServerTrust != nil {
			err = jobconfig.AttachPodToken(job, spec.owner.GetUID(), spec.owner.GetGeneration(), spec.operationID, b.ResultEndpoint, b.ResultServerTrust())
		} else {
			err = jobconfig.Attach(job, spec.owner.GetUID(), spec.owner.GetGeneration(), spec.operationID, b.ResultEndpoint)
		}
		if err != nil {
			return nil, err
		}
	}
	bindStableAPIDefaults(job)
	return job, nil
}

// executionServiceAccountName returns the identity an operation Job runs as.
// spec.execution.serviceAccountName is optional, and the Job write guard
// requires the Pod template to name an account, so a resource that omits it
// gets the namespace's default account written out rather than a Job that
// admission refuses. Kubernetes would bind that same account to a Pod that
// names none; writing it down is what makes the identity reviewable.
func executionServiceAccountName(execution operatorv1alpha1.ExecutionSpec) string {
	if execution.ServiceAccountName != "" {
		return execution.ServiceAccountName
	}
	return "default"
}

// bindStableAPIDefaults makes the immutable Job intent independent of
// kube-apiserver defaulting. These values are stable across the supported
// Kubernetes window and are security-relevant inputs to intent comparison.
func bindStableAPIDefaults(job *batchv1.Job) {
	one := int32(1)
	falseValue := false
	completionMode := batchv1.NonIndexedCompletion
	job.Spec.Parallelism = &one
	job.Spec.Completions = &one
	job.Spec.CompletionMode = &completionMode
	job.Spec.Suspend = &falseValue
	job.Spec.ManualSelector = &falseValue

	template := &job.Spec.Template.Spec
	if template.DNSPolicy == "" {
		template.DNSPolicy = corev1.DNSClusterFirst
	}
	if template.SchedulerName == "" {
		template.SchedulerName = corev1.DefaultSchedulerName
	}
	for i := range template.InitContainers {
		bindContainerAPIDefaults(&template.InitContainers[i])
	}
	for i := range template.Containers {
		bindContainerAPIDefaults(&template.Containers[i])
	}
	for i := range template.Volumes {
		volume := &template.Volumes[i]
		switch {
		case volume.Secret != nil && volume.Secret.DefaultMode == nil:
			mode := int32(corev1.SecretVolumeSourceDefaultMode)
			volume.Secret.DefaultMode = &mode
		case volume.ConfigMap != nil && volume.ConfigMap.DefaultMode == nil:
			mode := int32(corev1.ConfigMapVolumeSourceDefaultMode)
			volume.ConfigMap.DefaultMode = &mode
		case volume.Projected != nil && volume.Projected.DefaultMode == nil:
			mode := int32(corev1.ProjectedVolumeSourceDefaultMode)
			volume.Projected.DefaultMode = &mode
		case volume.DownwardAPI != nil && volume.DownwardAPI.DefaultMode == nil:
			mode := int32(corev1.DownwardAPIVolumeSourceDefaultMode)
			volume.DownwardAPI.DefaultMode = &mode
		}
	}
}

func bindContainerAPIDefaults(container *corev1.Container) {
	if container.TerminationMessagePath == "" {
		container.TerminationMessagePath = corev1.TerminationMessagePathDefault
	}
	if container.TerminationMessagePolicy == "" {
		container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
}

func baseVolumes() []corev1.Volume {
	return []corev1.Volume{
		{Name: runnerVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: memoryVolume(runnerVolumeBytes)}},
		{Name: workVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: memoryVolume(workVolumeBytes)}},
	}
}

// hardenedContainerContext is the security context of every operation
// container: the runner installer, the source guard, the fetch and the
// operation itself.
func hardenedContainerContext() *corev1.SecurityContext {
	falseValue := false
	trueValue := true
	userID := operationUserID
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &falseValue,
		ReadOnlyRootFilesystem:   &trueValue,
		RunAsNonRoot:             &trueValue,
		RunAsUser:                &userID,
		RunAsGroup:               &userID,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}
