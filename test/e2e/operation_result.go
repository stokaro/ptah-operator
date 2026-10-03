package e2e

import (
	"context"
	"errors"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultconsumer"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
	"github.com/stokaro/ptah-operator/test/e2e/resultframe"
)

// durableResultJob recognizes any delivery input, so a damaged projection
// cannot silently select the legacy log reader.
func durableResultJob(job *batchv1.Job) bool {
	for _, container := range job.Spec.Template.Spec.Containers {
		for _, arg := range container.Args {
			if strings.HasPrefix(arg, "--result-") {
				return true
			}
		}
		for _, env := range container.Env {
			if strings.HasPrefix(env.Name, "PTAH_RESULT_") {
				return true
			}
		}
	}
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.Name == jobconfig.VolumeName {
			return true
		}
	}
	return false
}

// resultJobWithoutProjection first validates the complete, isolated delivery
// projection, then removes only that projection from a copy. Existing exact
// workload-isolation checks can inspect the rest without accepting extra
// credentials, mounts, or result inputs.
func resultJobWithoutProjection(job *batchv1.Job) (*batchv1.Job, error) {
	owner := metav1.GetControllerOf(job)
	if owner == nil || owner.APIVersion != ptahSchemaAPIVersion ||
		(owner.Kind != "PtahSchema" && owner.Kind != "PtahMigration") {
		return nil, errors.New("result projection has no resource owner")
	}
	if _, err := jobconfig.Read(job, owner.UID, job.Annotations[annotationOperationID]); err != nil {
		return nil, err
	}
	copy := job.DeepCopy()
	spec := &copy.Spec.Template.Spec
	main := &spec.Containers[0]
	var args []string
	for i := 0; i < len(main.Args); i++ {
		if main.Args[i] == "--result-endpoint" || main.Args[i] == "--result-credentials" {
			i++ // Read already required each flag's exact value.
			continue
		}
		args = append(args, main.Args[i])
	}
	main.Args = args
	main.Env = slices.DeleteFunc(main.Env, func(env corev1.EnvVar) bool { return strings.HasPrefix(env.Name, "PTAH_RESULT_") })
	main.VolumeMounts = slices.DeleteFunc(main.VolumeMounts, func(mount corev1.VolumeMount) bool { return mount.Name == jobconfig.VolumeName })
	spec.Volumes = slices.DeleteFunc(spec.Volumes, func(volume corev1.Volume) bool { return volume.Name == jobconfig.VolumeName })
	return copy, nil
}

// readOperationResult follows the transport the immutable Job selected. Logs
// are diagnostic input for durable Jobs, never a fallback for a missing or
// invalid receipt. The archived Job and Pod fix the expected authority even
// after the API has collected the original workload.
func readOperationResult(ctx context.Context, reader client.Reader, job *batchv1.Job, pod *corev1.Pod,
	operation runner.Operation, operationID string, logs []byte,
) (runner.Result, error) {
	if job == nil {
		return runner.Result{}, resultconsumer.ErrBinding
	}
	if !durableResultJob(job) {
		return resultframe.Parse(logs, operation, operationID)
	}
	var owners []metav1.OwnerReference
	for _, owner := range job.OwnerReferences {
		if isController(owner) {
			owners = append(owners, owner)
		}
	}
	if len(owners) != 1 || owners[0].APIVersion != ptahSchemaAPIVersion ||
		(owners[0].Kind != "PtahSchema" && owners[0].Kind != "PtahMigration") ||
		job.UID == "" || pod == nil || pod.UID == "" || pod.Name == "" || pod.Namespace != job.Namespace ||
		!ownedExactlyOnce(pod.OwnerReferences, "batch/v1", "Job", job.Name, job.UID) ||
		job.Annotations[annotationOperationID] != operationID || pod.Annotations[annotationOperationID] != operationID {
		return runner.Result{}, resultconsumer.ErrBinding
	}
	owner := owners[0]
	label := string(operation)
	if owner.Kind == "PtahMigration" {
		label = strings.TrimPrefix(label, "migration-")
	}
	if job.Labels[labelOperation] != label {
		return runner.Result{}, resultconsumer.ErrBinding
	}
	config, err := jobconfig.Read(job, owner.UID, operationID)
	if err != nil {
		return runner.Result{}, err
	}
	engine := ""
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "PTAH_EXPECTED_DATABASE_ENGINE" {
			if engine != "" || env.ValueFrom != nil || env.Value == "" {
				return runner.Result{}, resultconsumer.ErrBinding
			}
			engine = strings.ToLower(env.Value)
		}
	}
	request := resultconsumer.Request{
		Namespace: job.Namespace, Kind: owner.Kind, Name: owner.Name, UID: owner.UID,
		Generation: config.Generation, ExecutionBindingID: job.Annotations[workload.AnnotationExecutionBindingID],
		InputFingerprint: job.Annotations[workload.AnnotationInputFingerprint], Operation: string(operation),
		OperationID: operationID, JobName: job.Name, JobUID: job.UID, Engine: engine,
	}
	loaded, err := (resultconsumer.StoreLoader{Store: resultstore.Store{Reader: reader}}).Load(ctx, request)
	if err != nil {
		return runner.Result{}, err
	}
	if loaded.Binding.PodName != pod.Name || loaded.Binding.PodUID != pod.UID {
		return runner.Result{}, resultconsumer.ErrBinding
	}
	return loaded.Value, nil
}

func operationResultPending(err error) bool {
	return errors.Is(err, resultstore.ErrIncomplete) || resultframe.StillArriving(err)
}

// readRecordedMigrationApply compares the original runner's full result with
// the controller's record. A diagnostic log without a frame is normal for a
// durable Job; neither that absence nor a termination summary replaces its
// receipt. readOperationResult checks the exact Job/Pod and operation binding.
func readRecordedMigrationApply(ctx context.Context, reader client.Reader, job *batchv1.Job, pod *corev1.Pod,
	run *api.MigrationRunStatus, logs []byte,
) (runner.Result, error) {
	if job == nil || job.UID == "" || job.Name == "" || run == nil || run.JobUID != job.UID || run.JobName != job.Name {
		return runner.Result{}, resultconsumer.ErrBinding
	}
	result, err := readOperationResult(ctx, reader, job, pod, runner.OperationMigrationApply, job.Annotations[annotationOperationID], logs)
	if err != nil {
		return runner.Result{}, err
	}
	if result.MigrationRun == nil || result.MigrationRun.Outcome != strings.ToLower(string(run.Outcome)) || !slices.Equal(result.MigrationRun.Applied, run.AppliedVersions) {
		return runner.Result{}, errors.New("migration Apply result differs from its recorded outcome or applied versions")
	}
	return result, nil
}
