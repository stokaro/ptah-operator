package e2e

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

func resourceNameRefusal(err error, kind, name string) error {
	if len(name) != 64 || kind != "PtahSchema" && kind != "PtahMigration" || !apierrors.IsInvalid(err) {
		return errors.New("name-boundary control needs the 64-byte resource's Invalid refusal")
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return errors.New("name-boundary refusal has no API status")
	}
	details := status.Status().Details
	if details == nil || details.Group != "operator.ptah.run" || details.Kind != kind || details.Name != name || len(details.Causes) != 1 {
		return errors.New("name-boundary refusal did not identify exactly this resource and rule")
	}
	// The existing root CEL rule is reported against a nil field path.
	cause := details.Causes[0]
	if cause.Field != "<nil>" || !strings.Contains(cause.Message, "metadata.name must be at most 63 bytes") {
		return errors.New("name-boundary refusal came from another field or rule")
	}
	return nil
}

// Inspect actual completed Jobs and Pods. API storage and an empty workload
// list cannot prove the accepted name reached the execution path.
func resourceNameWorkloads(namespace, kind, name string, uid, applyUID types.UID, jobs []batchv1.Job, pods []corev1.Pod) error {
	if namespace == "" || len(name) != 63 || uid == "" || applyUID == "" || len(jobs) == 0 || len(pods) == 0 {
		return errors.New("name-boundary evidence needs the maximum accepted name and nonempty workload identities")
	}
	label := labelSchema
	required := []string{"resolve", "verify", "observe", "plan", "apply"}
	if kind == "PtahMigration" {
		label, required = labelMigration, []string{"resolve", "verify", "history", "apply"}
	} else if kind != "PtahSchema" {
		return errors.New("name-boundary evidence names another resource family")
	}
	seenJobs, seenPods := map[types.UID]bool{}, map[types.UID]bool{}
	operations, applies := map[string]bool{}, 0
	for _, job := range jobs {
		operation := job.Labels[labelOperation]
		if job.Namespace != namespace || job.Name == "" || len(job.Name) > 63 || job.UID == "" || seenJobs[job.UID] ||
			job.Labels[label] != name || job.Spec.Template.Labels[label] != name || !slices.Contains(required, operation) || job.Spec.Template.Labels[labelOperation] != operation ||
			job.Annotations[annotationOperationID] == "" || job.Spec.Template.Annotations[annotationOperationID] != job.Annotations[annotationOperationID] ||
			!ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, kind, name, uid) {
			return errors.New("name-boundary Job lost the full name, exact owner or operation")
		}
		seenJobs[job.UID] = true
		if operation == "apply" {
			applies++
			if job.UID != applyUID {
				return errors.New("name-boundary evidence includes an unrecorded Apply")
			}
		}
		completedPods := 0
		for _, pod := range pods {
			if !ownedExactlyOnce(pod.OwnerReferences, "batch/v1", "Job", job.Name, job.UID) {
				continue
			}
			if pod.Namespace != namespace || pod.UID == "" || seenPods[pod.UID] || pod.Name == "" ||
				pod.Labels[label] != name || pod.Labels[labelOperation] != operation || pod.Annotations[annotationOperationID] != job.Annotations[annotationOperationID] {
				return errors.New("name-boundary Pod lost the full name or exact operation")
			}
			seenPods[pod.UID] = true
			if pod.Status.Phase == corev1.PodSucceeded {
				completedPods++
			}
		}
		if conditionTrue(job.Status.Conditions, batchv1.JobComplete) && completedPods > 0 {
			operations[operation] = true
		}
	}
	if applies != 1 || len(seenPods) != len(pods) {
		return errors.New("name-boundary evidence needs one recorded Apply and attributable Pods")
	}
	for _, operation := range required {
		if !operations[operation] {
			return fmt.Errorf("name-boundary evidence never executed %s", operation)
		}
	}
	return nil
}
