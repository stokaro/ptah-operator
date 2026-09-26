package workload

import (
	batchv1 "k8s.io/api/batch/v1"
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
