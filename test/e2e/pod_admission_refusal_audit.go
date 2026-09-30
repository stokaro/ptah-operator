package e2e

import (
	"errors"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A Job whose every Pod CREATE was refused has no container logs to read.
// Its exact admission event and a complete no-Pod inventory are positive
// evidence of that path, rather than a terminal Job that never got audited.
func podAdmissionRefusalAudit(schema *ptahv1alpha1.PtahSchema, job *batchv1.Job, pods []corev1.Pod, events []corev1.Event, policy string) error {
	if schema == nil || job == nil || schema.Name == "" || schema.Namespace == "" || schema.UID == "" ||
		job.Name == "" || job.UID == "" || job.Namespace != schema.Namespace || job.Labels[labelSchema] != schema.Name ||
		job.Labels[labelOperation] != "resolve" || policy == "" || !refusedJobIdle(job) || !podAdmissionRefusalReported(schema) ||
		!ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, "PtahSchema", schema.Name, schema.UID) {
		return errors.New("the no-Pod audit needs the exact refused schema and unfinished Resolve Job")
	}
	claim := schema.Status.ActiveOperation
	if claim.Type != ptahv1alpha1.OperationResolve || claim.ID == "" || job.Annotations[annotationOperationID] != claim.ID ||
		claim.JobName != job.Name || claim.JobUID != job.UID || !readyConditionNamesJob(schema, job.Name) {
		return errors.New("the no-Pod audit does not match the refused operation claim")
	}
	for _, pod := range pods {
		if podMatchesAdmissionRefusal(pod, schema.Namespace, schema.Name, job.UID) {
			return errors.New("the refused Job or schema has a Pod whose credential evidence still needs audit")
		}
	}
	if !slices.ContainsFunc(events, func(event corev1.Event) bool {
		ref := event.InvolvedObject
		return event.Namespace == job.Namespace && ref.APIVersion == "batch/v1" && ref.Kind == "Job" &&
			ref.Namespace == job.Namespace && ref.Name == job.Name && ref.UID == job.UID && event.Type == corev1.EventTypeWarning &&
			event.Reason == "FailedCreate" && event.Source.Component == "job-controller" &&
			strings.Contains(event.Message, "ValidatingAdmissionPolicy '"+policy+"'") &&
			strings.Contains(event.Message, "with binding '"+policy+"'") && strings.Contains(event.Message, podMetadataRefusal)
	}) {
		return errors.New("the no-Pod audit lacks the exact Job controller's policy-and-binding refusal")
	}
	return nil
}

func podMatchesAdmissionRefusal(pod corev1.Pod, namespace, schema string, jobUID types.UID) bool {
	return pod.Namespace == namespace && (pod.Labels[labelSchema] == schema ||
		slices.ContainsFunc(pod.OwnerReferences, func(owner metav1.OwnerReference) bool { return owner.UID == jobUID }))
}
