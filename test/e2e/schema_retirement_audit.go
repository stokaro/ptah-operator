package e2e

import (
	"errors"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// Same-name replacement can garbage-collect the predecessor's latest Plan
// before the periodic credential audit. Require complete logs while every
// exact predecessor workload can still be read.
func schemaRetirementAudited(resource *ptahv1alpha1.PtahSchema, jobs []batchv1.Job, pods []corev1.Pod,
	auditedJob func(string) bool, auditedPods map[string]bool,
) (bool, error) {
	if resource == nil || resource.Name == "" || resource.Namespace == "" || resource.UID == "" || len(jobs) == 0 || len(pods) == 0 || auditedJob == nil {
		return false, errors.New("schema retirement audit needs the exact predecessor and a nonempty workload inventory")
	}
	owners := make(map[types.UID]batchv1.Job, len(jobs))
	complete := true
	for _, job := range jobs {
		if job.Name == "" || job.UID == "" || job.Namespace != resource.Namespace || job.Labels[labelSchema] != resource.Name ||
			!ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, "PtahSchema", resource.Name, resource.UID) {
			return false, errors.New("schema retirement Job belongs to another resource identity")
		}
		if _, duplicate := owners[job.UID]; duplicate {
			return false, errors.New("schema retirement inventory repeats a Job identity")
		}
		owners[job.UID] = job
		complete = complete && auditedJob(string(job.UID))
	}
	seenPods, populatedJobs := map[types.UID]bool{}, map[types.UID]bool{}
	for _, pod := range pods {
		if pod.Name == "" || pod.UID == "" || seenPods[pod.UID] || pod.Namespace != resource.Namespace || pod.Labels[labelSchema] != resource.Name {
			return false, errors.New("schema retirement Pod has no exact predecessor identity")
		}
		seenPods[pod.UID] = true
		matches := 0
		for _, job := range owners {
			if ownedExactlyOnce(pod.OwnerReferences, "batch/v1", "Job", job.Name, job.UID) {
				matches++
				populatedJobs[job.UID] = true
			}
		}
		if matches != 1 {
			return false, errors.New("schema retirement Pod does not name one exact predecessor Job")
		}
		complete = complete && auditedPods[string(pod.UID)]
	}
	if len(populatedJobs) != len(owners) {
		return false, errors.New("schema retirement inventory omitted a predecessor Job's Pods")
	}
	return complete, nil
}
