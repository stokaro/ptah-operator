package workload

import (
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planseal"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// ManagerIdentityOf reads the manager identity a live operation Job records,
// from its Pod template: the manager image and revision annotations and the
// image of the container that installs the runner. A value the template does
// not carry reads as empty.
//
// The template is the part of the Job the claim's admission snapshot digests,
// so a caller that holds the template to that digest holds this identity to
// it too. The Job's own annotations carry the same two values, and nothing
// pins them.
func ManagerIdentityOf(job *batchv1.Job) (controllerImage, controllerRevision, runnerImage string) {
	if job == nil {
		return "", "", ""
	}
	for _, container := range job.Spec.Template.Spec.InitContainers {
		if container.Name == initContainerName {
			runnerImage = container.Image
			break
		}
	}
	annotations := job.Spec.Template.Annotations
	return annotations[AnnotationControllerImage], annotations[AnnotationControllerRevision], runnerImage
}

// CarryManagerIdentity rewrites the manager identity of an expected Job to
// the one the live Job's Pod template was built with: the two annotations
// that record the manager, on the Job and on its Pod template, and the image
// of the container that installs the runner. It reports whether anything
// changed.
//
// A manager rebuilds a Job to check that the live one is what its claim
// authorized. A Job dispatched by an earlier manager of the same execution
// binding differs from that rebuild in exactly these values and in nothing a
// plan or an approval binds, so the comparison takes them from the live Job.
// Both levels are taken from the live template, which the admission snapshot
// pins, and never from the Job's own annotations, which nothing pins: a live
// Job whose own annotations disagree with its template then fails the
// comparison instead of carrying the edit into it. A caller that carried
// anything must still hold the live Pod template to the digest its claim
// persisted before dispatch.
func CarryManagerIdentity(expected, live *batchv1.Job) bool {
	if expected == nil || live == nil {
		return false
	}
	changed := false
	carry := func(target map[string]string, key, value string) {
		if target == nil {
			return
		}
		if current, present := target[key]; present && current != value {
			target[key] = value
			changed = true
		}
	}
	liveImage, liveRevision, liveRunner := ManagerIdentityOf(live)
	for key, value := range map[string]string{
		AnnotationControllerImage:    liveImage,
		AnnotationControllerRevision: liveRevision,
	} {
		if _, recorded := live.Spec.Template.Annotations[key]; !recorded {
			continue
		}
		carry(expected.Annotations, key, value)
		carry(expected.Spec.Template.Annotations, key, value)
	}
	for index := range expected.Spec.Template.Spec.InitContainers {
		container := &expected.Spec.Template.Spec.InitContainers[index]
		if container.Name == initContainerName && liveRunner != "" && container.Image != liveRunner {
			container.Image = liveRunner
			changed = true
		}
	}
	return changed
}

// CarrySealedPlanKey is CarryManagerIdentity's counterpart for the one value
// a rebuild cannot simply trust from the live Job: the public key a Plan
// Job's runner seals its plan payload to.
//
// Every process generates its own key pair at startup and never persists it
// (internal/planseal), so a live Plan Job may have been built, and its Pod
// admission snapshot resolved, by a different process of the same execution
// binding -- another replica validating the same webhook request, or the
// same replica after a restart -- than the one rebuilding expected to check
// it. Unlike the manager image and revision, which are the same across every
// replica of one release and so need no proof beyond what the live Job
// already says, a key is proof of nothing by itself: any 32 bytes decode as
// one. What makes carrying it safe is checking it first, against the digest
// recorded on the operation claim before the one Job this key could belong
// to was ever created.
//
// Called only for a Plan operation; every other operation type carries no
// seal key, and both sides have none to disagree about.
func CarrySealedPlanKey(expected, live *batchv1.Job, operation operatorv1alpha1.ActiveOperationStatus) error {
	if operation.Type != operatorv1alpha1.OperationPlan {
		return nil
	}
	if expected == nil || live == nil {
		return errors.New("plan seal key carry requires both Jobs")
	}
	liveValue, ok := containerEnvValue(live, runner.EnvPlanSealPublicKey)
	if !ok {
		return errors.New("plan Job does not carry a seal key")
	}
	key, err := planseal.DecodePublicKey(liveValue)
	if err != nil {
		return fmt.Errorf("plan Job seal key is malformed: %w", err)
	}
	if operation.PlanSealPublicKeyDigest == "" {
		return errors.New("active operation records no seal key digest for a dispatched Plan Job")
	}
	if fingerprint.DigestBytes([]byte(key.Encode())) != operation.PlanSealPublicKeyDigest {
		return errors.New("plan Job seal key does not match the digest the operation claim recorded")
	}
	if !setContainerEnvValue(expected, runner.EnvPlanSealPublicKey, liveValue) {
		return errors.New("reconstructed Plan Job does not carry a seal key to replace")
	}
	return nil
}

// containerEnvValue is the value of the named environment variable on the
// first container or init container that carries it.
func containerEnvValue(job *batchv1.Job, name string) (string, bool) {
	if job == nil {
		return "", false
	}
	spec := &job.Spec.Template.Spec
	for _, containers := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for _, container := range containers {
			for _, env := range container.Env {
				if env.Name == name {
					return env.Value, true
				}
			}
		}
	}
	return "", false
}

// setContainerEnvValue overwrites the named environment variable's value on
// every container and init container that carries it, and reports whether it
// found one to overwrite.
func setContainerEnvValue(job *batchv1.Job, name, value string) bool {
	if job == nil {
		return false
	}
	found := false
	spec := &job.Spec.Template.Spec
	for _, containers := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for index := range containers {
			for envIndex := range containers[index].Env {
				if containers[index].Env[envIndex].Name == name {
					containers[index].Env[envIndex].Value = value
					found = true
				}
			}
		}
	}
	return found
}
