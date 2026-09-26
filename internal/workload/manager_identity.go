package workload

import (
	batchv1 "k8s.io/api/batch/v1"
)

// ManagerIdentityOf reads the manager identity a live operation Job records:
// the manager image and revision annotations and the image of the container
// that installs the runner. A value the Job does not carry reads as empty.
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
	return job.Annotations[AnnotationControllerImage], job.Annotations[AnnotationControllerRevision], runnerImage
}

// CarryManagerIdentity rewrites the manager identity of an expected Job to
// the one the live Job was built with: the two annotations that record the
// manager, on the Job and on its Pod template, and the image of the container
// that installs the runner. It reports whether anything changed.
//
// A manager rebuilds a Job to check that the live one is what its claim
// authorized. A Job dispatched by an earlier manager of the same execution
// binding differs from that rebuild in exactly these values and in nothing a
// plan or an approval binds, so the comparison takes them from the live Job.
// A caller that carried anything must still hold the live Pod template to the
// digest its claim persisted before dispatch, which pins what was carried.
func CarryManagerIdentity(expected, live *batchv1.Job) bool {
	if expected == nil || live == nil {
		return false
	}
	changed := false
	carry := func(target map[string]string, source map[string]string, key string) {
		value, found := source[key]
		if !found || target == nil {
			return
		}
		if current, present := target[key]; present && current != value {
			target[key] = value
			changed = true
		}
	}
	for _, key := range []string{AnnotationControllerImage, AnnotationControllerRevision} {
		carry(expected.Annotations, live.Annotations, key)
		carry(expected.Spec.Template.Annotations, live.Spec.Template.Annotations, key)
	}
	_, _, liveRunner := ManagerIdentityOf(live)
	for index := range expected.Spec.Template.Spec.InitContainers {
		container := &expected.Spec.Template.Spec.InitContainers[index]
		if container.Name == initContainerName && liveRunner != "" && container.Image != liveRunner {
			container.Image = liveRunner
			changed = true
		}
	}
	return changed
}
