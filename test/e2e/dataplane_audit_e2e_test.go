//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// recordObservedJobs adds every Job the namespace holds now to the ledger.
func (d *dataPlane) recordObservedJobs() {
	d.t.Helper()
	jobs := &batchv1.JobList{}
	d.check(d.list(jobs), "could not list Jobs while recording the observed Job ledger")
	records, err := observedJobRecords(jobs.Items)
	d.check(err, "could not validate the observed Job ledger snapshot")
	d.observed.add(records)
}

// checkpointJobs is the Jobs of one schema, and of one operation when it is
// not empty, that the ledger holds now.
func (d *dataPlane) checkpointJobs(schema, operation string) checkpoint {
	d.t.Helper()
	d.recordObservedJobs()
	return d.observed.checkpoint(schema, operation)
}

// countBetween counts the schema's Jobs of the operation, or of every
// operation when it is empty, that appeared between two checkpoints.
func (d *dataPlane) countBetween(schema, operation string, before, after checkpoint) int {
	return len(d.observed.between(schema, operation, before, after))
}

func (d *dataPlane) assertOneJobBetween(schema, operation string, before, after checkpoint) {
	d.t.Helper()
	if count := d.countBetween(schema, operation, before, after); count != 1 {
		d.fatalf("%s created %d bounded %s Jobs, expected exactly one", schema, count, operation)
	}
}

func (d *dataPlane) assertNoJobBetween(schema, operation string, before, after checkpoint) {
	d.t.Helper()
	if count := d.countBetween(schema, operation, before, after); count != 0 {
		d.fatalf("%s created %d unexpected bounded %s Jobs", schema, count, operation)
	}
}

// newJobCountSince refreshes the ledger and counts the schema's Jobs of the
// operation that appeared since the checkpoint.
func (d *dataPlane) newJobCountSince(schema, operation string, before checkpoint) int {
	d.t.Helper()
	d.recordObservedJobs()
	return len(d.observed.since(schema, operation, before))
}

// waitForOneNewJob waits for the schema's first Job of the operation since the
// checkpoint, and ends the scenario on a second.
func (d *dataPlane) waitForOneNewJob(schema, operation string, before checkpoint) {
	d.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		d.auditCompletedJobs()
		switch count := d.newJobCountSince(schema, operation, before); {
		case count > 1:
			d.fatalf("%s created more than one new %s Job", schema, operation)
		case count == 1:
			return
		}
		d.sleep(2 * time.Second)
	}
	d.fatalf("timed out waiting for a new %s Job for %s", operation, schema)
}

func (d *dataPlane) assertOneNewJob(schema, operation string, before checkpoint) {
	d.t.Helper()
	if count := d.newJobCountSince(schema, operation, before); count != 1 {
		d.fatalf("%s created %d new %s Jobs, expected exactly one", schema, count, operation)
	}
}

func (d *dataPlane) assertNoNewJobs(schema, operation string, before checkpoint) {
	d.t.Helper()
	if count := d.newJobCountSince(schema, operation, before); count != 0 {
		d.fatalf("%s created %d unexpected %s Jobs", schema, count, operation)
	}
}

// allNewJobsComplete reports whether at least minimum Jobs of the operation
// appeared since the checkpoint and every one of them, read live, completed.
func (d *dataPlane) allNewJobsComplete(schema, operation string, before checkpoint, minimum int) bool {
	d.t.Helper()
	d.recordObservedJobs()
	records := d.observed.since(schema, operation, before)
	if len(records) < minimum {
		return false
	}
	for _, record := range records {
		if record.Name == "" || record.UID == "" {
			d.fatalf("could not validate new %s Job identities for %s", operation, schema)
		}
		job := &batchv1.Job{}
		if err := d.get(record.Name, job); err != nil {
			return false
		}
		if string(job.UID) != record.UID || job.Labels[labelSchema] != schema ||
			job.Labels[labelOperation] != operation || !jobComplete(job) {
			return false
		}
	}
	return true
}

// readResultTransport reads diagnostic output and the result selected by the
// immutable Job. Legacy frames may still be arriving in the container log;
// durable receipts must be complete and bound to this exact Job and Pod.
func (d *dataPlane) readResultTransport(job *batchv1.Job, pod *corev1.Pod, operation, operationID string) ([]byte, runner.Result) {
	d.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		logs, err := d.cluster.ContainerLog(d.ctx, d.in.TestNamespace, pod.Name, "ptah")
		if err != nil {
			d.fatalf("could not read the %s result transport from %s: %v", operation, pod.Name, err)
		}
		result, err := readOperationResult(d.ctx, d.cluster.Client, job, pod, runner.Operation(operation), operationID, logs)
		if err == nil {
			return logs, result
		}
		if !operationResultPending(err) || !time.Now().Before(deadline) {
			_, _ = fmt.Fprintf(os.Stderr, "e2e data plane:   %v\n", err)
			d.fatalf("the %s result from %s could not be read", operation, pod.Name)
		}
		d.sleep(2 * time.Second)
	}
}

// auditCompletedJobs audits every Job that reached a terminal state since the
// last audit, before the controller's TTL can delete it: the Job, its exact
// Pods and every container log hold no credential, a Job admitted under the
// runtime class carries what admission gave it, and a completed operation
// Job's evidence -- the Job, its Pod, the settled transport and its result --
// is kept for the proofs that read it after the Job is gone. The first time an
// operation Pod is running, it is also sent the admission refusals only a live
// Pod can be sent.
func (d *dataPlane) auditCompletedJobs() {
	d.t.Helper()
	if !d.ephemeralTested && d.activePodAdmissionRefusals() {
		d.ephemeralTested = true
	}
	d.recordObservedJobs()
	jobs := &batchv1.JobList{}
	d.mustList(jobs)
	for index := range jobs.Items {
		listed := &jobs.Items[index]
		if !jobTerminal(listed) {
			continue
		}
		if listed.UID == "" || listed.Name == "" {
			d.fatalf("could not capture exact terminal Job identities for credential audit")
		}
		if d.fullyAudited.holds(string(listed.UID)) {
			continue
		}
		d.auditTerminalJob(listed.Name, listed.UID)
	}
}

func (d *dataPlane) auditTerminalJob(name string, uid types.UID) {
	d.t.Helper()
	job := &batchv1.Job{}
	if err := d.get(name, job); err != nil {
		d.fatalf("terminal Job %s UID %s disappeared before its audit", name, uid)
	}
	if job.UID != uid || !jobTerminal(job) {
		d.fatalf("terminal Job %s was replaced before UID %s could be audited", name, uid)
	}
	pods := &corev1.PodList{}
	d.mustList(pods)
	owned := ownedPods(pods.Items, uid)
	if len(owned) == 0 {
		d.fatalf("terminal Job %s UID %s has no exact owned Pod to audit", name, uid)
	}
	managedComplete := managedCompleteJob(job)
	if managedComplete && len(owned) != 1 {
		d.fatalf("completed managed Job %s UID %s does not own one exact Pod", name, uid)
	}
	runtimeClass := admittedUnderRuntimeClass(job)
	if runtimeClass {
		if err := jobAdmissionBinding(job, d.controller); err != nil {
			d.fatalf("%v", err)
		}
	}
	d.scan(append(d.jsonBytes(job), d.jsonBytes(owned)...), fmt.Sprintf("Job %s UID %s and its exact owned Pods", name, uid))
	var operation, operationID string
	if managedComplete {
		// The retained transport is read against the same operation binding
		// the evidence is kept under, so the frame the audit accepts is the
		// frame the evidence holds.
		operation, operationID = job.Labels[labelOperation], job.Annotations[annotationOperationID]
		if operation == "" {
			d.fatalf("completed managed Job %s UID %s has no operation to bind its result to", name, uid)
		}
		if operationID == "" {
			d.fatalf("completed managed Job %s UID %s has no operation ID to bind its result to", name, uid)
		}
	}
	var evidencePod *corev1.Pod
	var evidenceLog []byte
	var evidenceResult runner.Result
	var resultRead bool
	for ownedIndex := range owned {
		podName, podUID := owned[ownedIndex].Name, owned[ownedIndex].UID
		if podName == "" || podUID == "" {
			d.fatalf("could not capture exact owned Pod identities for credential audit")
		}
		pod := &corev1.Pod{}
		if err := d.get(podName, pod); err != nil {
			d.fatalf("Pod %s UID %s disappeared before terminal Job audit", podName, podUID)
		}
		if !terminalPodEvidence(pod, podUID, uid) {
			d.fatalf("exact Pod %s UID %s lacks complete terminal evidence", podName, podUID)
		}
		if runtimeClass && !podAdmissionApplied(job, pod, d.controller, registryPullSecret) {
			d.fatalf("managed Pod %s lacks exact LimitRange, ServiceAccount, RuntimeClass, or default-toleration admission", podName)
		}
		containers := terminatedInStatusOrder(pod)
		if len(containers) == 0 {
			d.fatalf("exact Pod %s UID %s has no terminated container logs", podName, podUID)
		}
		for _, container := range containers {
			logs, err := d.cluster.ContainerLog(d.ctx, d.in.TestNamespace, podName, container)
			if err != nil {
				d.fatalf("could not audit %s logs for exact Pod %s UID %s: %v", container, podName, podUID, err)
			}
			d.scan(logs, fmt.Sprintf("%s logs for exact Pod %s UID %s", container, podName, podUID))
			// Retain the validated result independently of the diagnostic log.
			// A durable result is read from storage even if this log is empty.
			if managedComplete && container == "ptah" {
				settled, result := d.readResultTransport(job, pod, operation, operationID)
				d.scan(settled, fmt.Sprintf("the settled ptah transport for exact Pod %s UID %s", podName, podUID))
				d.scan(d.jsonBytes(result), fmt.Sprintf("the validated %s result for exact Pod %s UID %s", operation, podName, podUID))
				evidenceLog, evidenceResult, resultRead = settled, result, true
			}
		}
		after := &corev1.Pod{}
		if err := d.get(podName, after); err != nil {
			d.fatalf("exact Pod %s UID %s disappeared during its log audit", podName, podUID)
		}
		if after.UID != podUID || !controlledByJob(after.OwnerReferences, uid) {
			d.fatalf("exact Pod %s changed identity during its log audit", podName)
		}
		if managedComplete {
			evidencePod = after
		}
	}
	jobAfter := &batchv1.Job{}
	if err := d.get(name, jobAfter); err != nil {
		d.fatalf("terminal Job %s UID %s disappeared during its exact Pod audit", name, uid)
	}
	if jobAfter.UID != uid || !jobTerminal(jobAfter) {
		d.fatalf("terminal Job %s changed identity during its exact Pod audit", name)
	}
	if managedComplete {
		if evidencePod == nil {
			d.fatalf("completed managed Job %s lost its exact Pod evidence", name)
		}
		if !resultRead {
			d.fatalf("completed managed Job %s UID %s has no UID-bounded result evidence", name, uid)
		}
		d.keepEvidence(jobAfter, evidencePod, evidenceLog, evidenceResult)
	}
	d.audited.add(string(uid))
	d.fullyAudited.add(string(uid))
}

// keepEvidence files a completed operation Job's evidence under its UID. The
// same Job offered twice must be the same Job, and anything else is a
// collision.
func (d *dataPlane) keepEvidence(job *batchv1.Job, pod *corev1.Pod, logs []byte, result runner.Result) {
	d.t.Helper()
	schema, operation := job.Labels[labelSchema], job.Labels[labelOperation]
	operationID := job.Annotations[annotationOperationID]
	schemaUID, err := schemaOwnerUID(job, schema)
	if err != nil {
		d.fatalf("%v", err)
	}
	if err := suppliedEvidenceIdentity(job, pod, schema, schemaUID, operation, operationID); err != nil {
		d.fatalf("%v", err)
	}
	supplied := &jobEvidence{job: job, pod: pod, log: logs}
	if existing, found := d.evidence[string(job.UID)]; found {
		if err := validateJobEvidence(existing, schema, operation, job.UID, schemaUID, d.runnerProtocol); err != nil {
			d.fatalf("job evidence for UID %s: %v", job.UID, err)
		}
		if err := sameEvidenceIdentity(existing, supplied, schema); err != nil {
			d.fatalf("%v", err)
		}
		return
	}
	// The result was validated against this workload where it was captured.
	// Diagnostic logs are retained and scanned without becoming its transport.
	supplied.result = result
	d.scan(d.jsonBytes(job), "staged exact Job JSON")
	d.scan(d.jsonBytes(pod), "staged exact Pod JSON")
	d.scan(logs, "staged raw ptah log")
	d.scan(d.jsonBytes(result), "staged normalized result")
	if err := validateJobEvidence(supplied, schema, operation, job.UID, schemaUID, d.runnerProtocol); err != nil {
		d.fatalf("job evidence for UID %s: %v", job.UID, err)
	}
	d.evidence[string(job.UID)] = supplied
}

// completedEvidence is the evidence kept for a completed Job, held to the
// schema and operation a proof reads it for.
func (d *dataPlane) completedEvidence(schema, operation, uid string) *jobEvidence {
	d.t.Helper()
	evidence, found := d.evidence[uid]
	if !found {
		d.fatalf("completed managed Job UID %s has no durable evidence archive", uid)
	}
	if err := validateJobEvidence(evidence, schema, operation, types.UID(uid), "", d.runnerProtocol); err != nil {
		d.fatalf("job evidence for UID %s: %v", uid, err)
	}
	d.scan(d.jsonBytes(evidence.job), "exact archived Job JSON")
	d.scan(d.jsonBytes(evidence.pod), "exact archived Pod JSON")
	d.scan(evidence.log, "archived raw ptah log")
	d.scan(d.jsonBytes(evidence.result), "archived normalized result")
	return evidence
}

// captureOneNewJobResult reads the result of the schema's one Job of the
// operation since before, and before after when it is given: the Job keeps its
// operation identity, completes, owns one Pod bound to its operation ID, and
// that Pod's ptah container exited once and cleanly with a whole frame.
func (d *dataPlane) captureOneNewJobResult(schema, operation string, before checkpoint, after *checkpoint) runner.Result {
	d.t.Helper()
	d.recordObservedJobs()
	var records []observedJob
	if after != nil {
		records = d.observed.between(schema, operation, before, *after)
	} else {
		records = d.observed.since(schema, operation, before)
	}
	if len(records) != 1 {
		d.fatalf("%s has %d new %s Jobs, expected exactly one result", schema, len(records), operation)
	}
	jobName, jobUID := records[0].Name, records[0].UID

	deadline := time.Now().Add(waitTimeout)
	var job *batchv1.Job
	for time.Now().Before(deadline) {
		candidate := &batchv1.Job{}
		if err := d.get(jobName, candidate); err == nil {
			job = candidate
			if string(job.UID) != jobUID || job.Labels[labelSchema] != schema || job.Labels[labelOperation] != operation ||
				job.Annotations[annotationOperationID] == "" ||
				job.Spec.PodReplacementPolicy == nil || *job.Spec.PodReplacementPolicy != batchv1.Failed ||
				job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
				d.fatalf("%s changed its immutable operation identity", jobName)
			}
			if conditionTrue(job.Status.Conditions, batchv1.JobFailed) {
				d.fatalf("%s failed before producing a transport-success result", jobName)
			}
			if conditionTrue(job.Status.Conditions, batchv1.JobComplete) {
				break
			}
		}
		d.sleep(time.Second)
	}
	if job == nil || !conditionTrue(job.Status.Conditions, batchv1.JobComplete) {
		d.fatalf("timed out waiting for exact result Job %s", jobName)
	}
	operationID := job.Annotations[annotationOperationID]

	pods := &corev1.PodList{}
	d.mustList(pods, client.MatchingLabels{"job-name": jobName})
	var bound []corev1.Pod
	for _, pod := range pods.Items {
		if podControlledByJobUID(pod.OwnerReferences, types.UID(jobUID)) && pod.Annotations[annotationOperationID] == operationID {
			bound = append(bound, pod)
		}
	}
	if len(bound) != 1 {
		d.fatalf("exact result Job %s does not own exactly one bound Pod", jobName)
	}
	pod := &bound[0]
	if pod.GenerateName != jobName+"-" {
		d.fatalf("%s does not preserve its exact Job generateName", pod.Name)
	}
	if !resultTransportPod(pod) {
		d.fatalf("%s did not preserve one zero-restart result transport", pod.Name)
	}
	logs, result := d.readResultTransport(job, pod, operation, operationID)
	d.scan(logs, fmt.Sprintf("the exact %s result transport", operation))
	d.scan(d.jsonBytes(result), fmt.Sprintf("the validated %s result", operation))
	if int64(result.ProtocolVersion) != d.runnerProtocol || string(result.Operation) != operation ||
		result.OperationID != operationID || result.Truncation != nil {
		d.fatalf("validated result lost its runner protocol binding or complete-output guarantee")
	}
	d.keepEvidence(job, pod, logs, result)
	d.audited.add(jobUID)
	d.captured = capturedJob{
		jobName: jobName, jobUID: jobUID, operationID: operationID,
		podName: pod.Name, podUID: string(pod.UID), podGenerateName: pod.GenerateName,
	}
	return result
}

// podControlledByJobUID is jq's `.kind == "Job" and .uid == $uid and
// .controller == true`: the UID decides, whatever group the reference names.
func podControlledByJobUID(references []metav1.OwnerReference, uid types.UID) bool {
	for _, reference := range references {
		if reference.Kind == "Job" && reference.UID == uid && isController(reference) {
			return true
		}
	}
	return false
}

// resultTransportPod is a Pod with no restart among its init and regular
// containers, and one ptah container that exited 0.
func resultTransportPod(pod *corev1.Pod) bool {
	for _, status := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		if status.RestartCount != 0 {
			return false
		}
	}
	return ptahExitedZero(pod)
}

// captureSelectedJobResult reads the kept result of one Job the ledger names,
// by UID: the Job may be gone, and its evidence is what speaks for it. Where
// the Job and its Pod are still live, they have to agree with that evidence.
func (d *dataPlane) captureSelectedJobResult(schema, operation, uid string) runner.Result {
	d.t.Helper()
	d.recordObservedJobs()
	count := 0
	for _, record := range d.observed.records {
		if record.Schema == schema && record.Operation == operation && record.UID == uid {
			count++
		}
	}
	if count != 1 {
		d.fatalf("%s has %d observed %s Jobs with selected UID %s", schema, count, operation, uid)
	}
	evidence := d.completedEvidence(schema, operation, uid)
	d.scan(d.jsonBytes(evidence.result), fmt.Sprintf("the selected archived %s result", operation))
	d.captured = capturedJob{
		jobName: evidence.job.Name, jobUID: uid, operationID: evidence.job.Annotations[annotationOperationID],
		podName: evidence.pod.Name, podUID: string(evidence.pod.UID), podGenerateName: evidence.job.Name + "-",
		evidence: evidence,
	}
	d.assertLiveEvidenceConsistent(evidence)
	return evidence.result
}

// assertLiveEvidenceConsistent holds a Job and Pod that are still live to the
// evidence kept for them. Either may be gone; a read that failed is not
// absence.
func (d *dataPlane) assertLiveEvidenceConsistent(evidence *jobEvidence) {
	d.t.Helper()
	job := &batchv1.Job{}
	err := d.get(evidence.job.Name, job)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		d.fatalf("live Job consistency read failed before exact GC absence could be established")
	case job.Name != evidence.job.Name || job.UID != evidence.job.UID ||
		job.Annotations[annotationOperationID] != evidence.job.Annotations[annotationOperationID]:
		d.fatalf("live Job conflicts with its durable evidence archive")
	}
	pod := &corev1.Pod{}
	err = d.get(evidence.pod.Name, pod)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		d.fatalf("live Pod consistency read failed before exact GC absence could be established")
	case pod.Name != evidence.pod.Name || pod.UID != evidence.pod.UID ||
		!ownedExactlyOnce(pod.OwnerReferences, "batch/v1", "Job", evidence.job.Name, evidence.job.UID):
		d.fatalf("live Pod conflicts with its durable evidence archive")
	}
}

// archivedSchemaJobs is the kept evidence of every Job of the schema since the
// checkpoint, exactly expected of them, and their UIDs sorted.
func (d *dataPlane) archivedSchemaJobs(schema string, before checkpoint, expected int) ([]string, []batchv1.Job) {
	d.t.Helper()
	records := d.observed.since(schema, "", before)
	if len(records) != expected {
		d.fatalf("%s durable Job history is not exactly %d unique UIDs", schema, expected)
	}
	slices.SortFunc(records, func(a, b observedJob) int { return strings.Compare(a.UID, b.UID) })
	uids := make([]string, 0, len(records))
	jobs := make([]batchv1.Job, 0, len(records))
	for _, record := range records {
		evidence := d.completedEvidence(schema, record.Operation, record.UID)
		uids = append(uids, record.UID)
		jobs = append(jobs, *evidence.job)
	}
	d.scanObject(jobs, fmt.Sprintf("the archived %s Job lifecycle", schema))
	return uids, jobs
}

// assertSchemaJobBoundaryUnchanged holds the schema's Jobs since before to
// exactly the expected UIDs, and returns them.
func (d *dataPlane) assertSchemaJobBoundaryUnchanged(schema string, before checkpoint, expected []string, count int) []string {
	d.t.Helper()
	actual, err := jobBoundaryUnchanged(d.observed.since(schema, "", before), expected, count)
	if err != nil {
		d.fatalf("%s durable Job boundary changed after result capture: %v", schema, err)
	}
	return actual
}

// activePodAdmissionRefusals sends a live operation Pod the three requests the
// Pod intent webhook exists to refuse: a clone of it created by a namespace
// actor, an ephemeral container bound to its exact UID, and an update that
// strips its managed identity. It reports false, and sends nothing, when no
// operation Pod is running whose Job and schema still name it; the audit asks
// again on its next pass.
func (d *dataPlane) activePodAdmissionRefusals() bool {
	d.t.Helper()
	pods := &corev1.PodList{}
	if err := d.list(pods, client.MatchingLabels{labelManagedBy: managedByOperator, labelComponent: schemaOperationComponent}); err != nil {
		return false
	}
	var pod *corev1.Pod
	var jobOwner *metav1.OwnerReference
	for index := range pods.Items {
		candidate := &pods.Items[index]
		if candidate.DeletionTimestamp != nil ||
			(candidate.Status.Phase != corev1.PodPending && candidate.Status.Phase != corev1.PodRunning) {
			continue
		}
		owners := controllerOwners(candidate.OwnerReferences, "batch/v1", "Job")
		if len(owners) == 0 {
			continue
		}
		pod, jobOwner = candidate, &owners[0]
		break
	}
	if pod == nil {
		return false
	}
	schemaName := pod.Labels[labelSchema]
	job := &batchv1.Job{}
	if schemaName == "" || d.get(jobOwner.Name, job) != nil {
		return false
	}
	template := job.Spec.Template
	if job.UID != jobOwner.UID || job.Status.Active < 1 ||
		!d.controller.stampedOn(job.Annotations) || !d.controller.stampedOn(template.Annotations) ||
		template.Annotations[annotationAdmissionDigest] == "" || pod.UID == "" {
		return false
	}
	schema := &ptahv1alpha1.PtahSchema{}
	if d.get(schemaName, schema) != nil {
		return false
	}
	active := schema.Status.ActiveOperation
	if active == nil || active.JobName != jobOwner.Name || active.JobUID != jobOwner.UID ||
		active.AdmissionSnapshot == nil || !sha256Pattern.MatchString(active.AdmissionSnapshot.Digest) {
		return false
	}

	stored := &unstructured.Unstructured{}
	stored.SetAPIVersion("v1")
	stored.SetKind("Pod")
	if d.get(pod.Name, stored) != nil || stored.GetUID() != pod.UID {
		return false
	}
	clone := podClone(stored)
	err := d.cluster.Client.Create(d.ctx, clone, client.DryRunAll)
	if err == nil {
		d.fatalf("Pod intent admission allowed a namespace actor to clone active Job Pod %s", pod.Name)
	}
	d.scan([]byte(err.Error()), "the operation Pod create-origin admission refusal")
	if !strings.Contains(err.Error(), "vpodintent.operator.ptah.run") {
		d.fatalf("operation Pod clone rejection did not come from the Pod intent webhook")
	}
	if !strings.Contains(err.Error(), "not created by the Kubernetes Job controller") {
		d.fatalf("Pod intent webhook rejected the operation Pod clone for an unexpected reason")
	}
	d.logf("PASS operation Pod create-origin enforcement")

	// The patch names the Pod's exact UID and goes to the ephemeralcontainers
	// subresource, which must stay inside the admission snapshot.
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": string(pod.UID)},
		"spec": map[string]any{"ephemeralContainers": []any{map[string]any{
			"name": "forbidden-e2e", "image": d.in.RunnerImage, "imagePullPolicy": "IfNotPresent",
			"command": []any{"/ptah-runner"}, "args": []any{"--help"},
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}},
			},
		}}},
	})
	d.check(err, "encode the ephemeral-container patch")
	target := &corev1.Pod{}
	target.Namespace, target.Name = d.in.TestNamespace, pod.Name
	err = d.cluster.Client.SubResource("ephemeralcontainers").Patch(d.ctx, target, client.RawPatch(types.MergePatchType, patch))
	if err == nil {
		d.fatalf("Pod intent admission allowed an ephemeral container on exact active Pod %s UID %s", pod.Name, pod.UID)
	}
	d.scan([]byte(err.Error()), "the ephemeral-container admission refusal")
	if !strings.Contains(err.Error(), "vpodintent.operator.ptah.run") {
		d.fatalf("ephemeral-container rejection did not come from the Pod intent webhook")
	}
	if !strings.Contains(err.Error(), "persisted admission envelope") {
		d.fatalf("Pod intent webhook rejected the active Pod for an unexpected reason")
	}
	live := &corev1.Pod{}
	if err := d.get(pod.Name, live); err != nil || live.UID != pod.UID ||
		!controlledByJob(live.OwnerReferences, jobOwner.UID) || len(live.Spec.EphemeralContainers) != 0 {
		d.fatalf("active Pod identity changed during the negative subresource test")
	}

	// The webhook scope must keep a managed Pod across an update that tries to
	// remove its identity labels.
	strip := &corev1.Pod{}
	strip.Namespace, strip.Name = d.in.TestNamespace, pod.Name
	err = d.mergePatch(strip, map[string]any{"metadata": map[string]any{"labels": map[string]any{labelManagedBy: nil}}})
	if err == nil {
		d.fatalf("Pod intent admission allowed exact active Pod %s UID %s to remove its selector identity", pod.Name, pod.UID)
	}
	d.scan([]byte(err.Error()), "the managed-identity label-removal admission refusal")
	if !strings.Contains(err.Error(), "vpodintent.operator.ptah.run") {
		d.fatalf("managed-identity label removal did not reach the Pod intent webhook")
	}
	if !strings.Contains(err.Error(), "removed its managed workload identity") {
		d.fatalf("Pod intent webhook rejected managed-identity label removal for an unexpected reason")
	}
	live = &corev1.Pod{}
	if err := d.get(pod.Name, live); err != nil || live.UID != pod.UID ||
		live.Labels[labelManagedBy] != managedByOperator || live.Labels[labelComponent] != schemaOperationComponent ||
		!controlledByJob(live.OwnerReferences, jobOwner.UID) {
		d.fatalf("active Pod identity changed during the managed-identity update test")
	}
	d.logf("PASS active Pod managed-identity enforcement")
	return true
}

// podClone is a live operation Pod as a namespace actor would copy it: its
// server-set fields and its status gone, and the two labels that select it for
// the webhook removed, under a name of its own.
func podClone(pod *unstructured.Unstructured) *unstructured.Unstructured {
	document := runtime.DeepCopyJSON(pod.Object)
	metadata, _ := document["metadata"].(map[string]any)
	for _, field := range []string{
		"creationTimestamp", "deletionGracePeriodSeconds", "deletionTimestamp", "generateName", "generation",
		"managedFields", "resourceVersion", "selfLink", "uid",
	} {
		delete(metadata, field)
	}
	if labels, ok := metadata["labels"].(map[string]any); ok {
		delete(labels, labelManagedBy)
		delete(labels, labelComponent)
	}
	metadata["name"] = pod.GetName() + "-clone"
	delete(document, "status")
	clone := &unstructured.Unstructured{Object: document}
	clone.SetAPIVersion("v1")
	clone.SetKind("Pod")
	return clone
}

// auditRuntimeCredentials holds everything readable in the two namespaces to
// carry no credential: every non-Secret object, every manager container's
// whole log and its metrics, and every test Pod's logs the Job audit did not
// already read.
func (d *dataPlane) auditRuntimeCredentials() {
	d.t.Helper()
	d.auditCompletedJobs()
	var resources bytes.Buffer
	for _, list := range []client.ObjectList{
		&ptahv1alpha1.PtahSchemaList{}, &ptahv1alpha1.PtahSchemaPlanList{}, &ptahv1alpha1.PtahSchemaApprovalList{},
		&batchv1.JobList{}, &corev1.PodList{}, &corev1.ConfigMapList{}, &corev1.EventList{},
		&appsv1.DeploymentList{}, &appsv1.ReplicaSetList{}, &corev1.ServiceList{}, &corev1.ServiceAccountList{},
	} {
		d.mustList(list)
		resources.Write(d.jsonBytes(list))
	}
	for _, list := range []client.ObjectList{
		&corev1.PodList{}, &corev1.ConfigMapList{}, &corev1.EventList{}, &appsv1.DeploymentList{},
		&appsv1.ReplicaSetList{}, &corev1.ServiceList{}, &corev1.ServiceAccountList{}, &coordinationv1.LeaseList{},
	} {
		d.check(d.cluster.Client.List(d.ctx, list, client.InNamespace(d.in.OperatorNamespace)),
			"list %T in %s", list, d.in.OperatorNamespace)
		resources.Write(d.jsonBytes(list))
	}
	d.scan(resources.Bytes(), "a non-Secret Kubernetes resource")

	managers := &corev1.PodList{}
	d.check(d.cluster.Client.List(d.ctx, managers, client.InNamespace(d.in.OperatorNamespace), client.MatchingLabels{
		"app.kubernetes.io/name": "ptah-operator", "app.kubernetes.io/instance": d.in.HelmRelease,
		"app.kubernetes.io/component": "controller",
	}), "list the manager Pods")
	var live []corev1.Pod
	for _, pod := range managers.Items {
		if pod.DeletionTimestamp == nil {
			live = append(live, pod)
		}
	}
	if len(live) == 0 {
		d.fatalf("no exact manager Pod was available for credential audit")
	}
	for index := range live {
		if !noRestarts(&live[index]) {
			reportRestartedManagerContainers(d.cluster, live)
			d.fatalf("a manager container restarted before its complete log history was audited")
		}
	}
	for index := range live {
		name := live[index].Name
		current := &corev1.Pod{}
		if err := d.cluster.Client.Get(d.ctx, types.NamespacedName{Namespace: d.in.OperatorNamespace, Name: name}, current); err != nil ||
			current.UID == "" {
			d.fatalf("manager Pod %s has no exact UID", name)
		}
		logs, stderr, err := d.cluster.Kubectl(d.ctx, "-n", d.in.OperatorNamespace, "logs", "pod/"+name, "--all-containers")
		if err != nil {
			d.fatalf("could not audit logs for exact manager Pod %s UID %s: %s", name, current.UID, strings.TrimSpace(string(stderr)))
		}
		d.scan(append(logs, stderr...), fmt.Sprintf("logs for exact manager Pod %s", name))
		// A label value is the one place an error message could reach the
		// metrics, and a scraper keeps what it reads for as long as it keeps
		// anything. An empty or refused read would scan clean, so the body has
		// to be an exposition before its absence of credentials means anything.
		metrics, err := d.cluster.Raw(d.ctx, "/api/v1/namespaces/"+d.in.OperatorNamespace+"/pods/"+name+":8080/proxy/metrics")
		if err != nil {
			d.fatalf("could not read metrics for exact manager Pod %s UID %s: %v", name, current.UID, err)
		}
		if !isPrometheusExposition(metrics) {
			d.fatalf("metrics for exact manager Pod %s are not a Prometheus exposition", name)
		}
		d.scan(metrics, fmt.Sprintf("metrics for exact manager Pod %s", name))
	}

	pods := &corev1.PodList{}
	d.mustList(pods)
	for index := range pods.Items {
		snapshot := &pods.Items[index]
		owners := controllerOwners(snapshot.OwnerReferences, "batch/v1", "Job")
		if len(owners) > 1 {
			d.fatalf("could not capture exact test Pod identities for credential audit: Pod %s has multiple controlling batch/v1 Job owners", snapshot.Name)
		}
		if (snapshot.Status.Phase == corev1.PodSucceeded || snapshot.Status.Phase == corev1.PodFailed) &&
			len(owners) == 1 && d.fullyAudited.holds(string(owners[0].UID)) {
			continue
		}
		d.auditUnauditedPod(snapshot.Name, snapshot.UID)
	}
}

func isPrometheusExposition(body []byte) bool {
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("# TYPE ")) {
			return true
		}
	}
	return false
}

// auditUnauditedPod reads every started container's log of a test Pod the Job
// audit has not covered.
func (d *dataPlane) auditUnauditedPod(name string, uid types.UID) {
	d.t.Helper()
	pod := &corev1.Pod{}
	if err := d.get(name, pod); err != nil {
		d.fatalf("unaudited exact Pod %s UID %s disappeared before log audit", name, uid)
	}
	if pod.UID != uid {
		d.fatalf("unaudited exact Pod %s changed identity before its log audit", name)
	}
	d.scanObject(pod, fmt.Sprintf("unaudited exact Pod %s UID %s", name, uid))
	if !noRestarts(pod) {
		d.fatalf("pod/%s restarted a container before complete log audit", name)
	}
	for _, container := range startedContainers(pod) {
		logs, err := d.cluster.ContainerLog(d.ctx, d.in.TestNamespace, name, container)
		if err != nil {
			d.fatalf("could not audit logs for started container %s in pod/%s: %v", container, name, err)
		}
		d.scan(logs, fmt.Sprintf("logs for container %s in pod/%s", container, name))
	}
	if (pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed) &&
		!slices.Equal(declaredContainers(pod), terminatedContainers(pod)) {
		d.fatalf("terminal pod/%s has unaudited nonterminal containers", name)
	}
	after := &corev1.Pod{}
	if err := d.get(name, after); err != nil {
		d.fatalf("unaudited exact Pod %s UID %s disappeared during log audit", name, uid)
	}
	if after.UID != uid {
		d.fatalf("unaudited exact Pod %s changed identity during its log audit", name)
	}
}

// reportRestartedManagerContainers names every manager container that
// restarted, how its previous run ended, and fixed diagnostic categories from
// its previous log. Neither the free-text termination message nor the log or
// API error is printed: each can carry credentials. This does not complete
// the credential audit, so the caller still fails the phase after reporting.
func reportRestartedManagerContainers(cluster *harness.Cluster, pods []corev1.Pod) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for index := range pods {
		pod := &pods[index]
		for _, status := range allStatuses(pod) {
			if status.RestartCount == 0 {
				continue
			}
			record := map[string]any{"pod": pod.Name, "container": status.Name, "restartCount": status.RestartCount}
			terminated := map[string]any{}
			if last := status.LastTerminationState.Terminated; last != nil {
				terminated = map[string]any{
					"reason": last.Reason, "exitCode": last.ExitCode, "signal": last.Signal,
					"startedAt": last.StartedAt, "finishedAt": last.FinishedAt,
				}
			}
			record["lastTerminated"] = terminated
			record["previousLog"] = readManagerExit(ctx, cluster.Clientset, pod, status)
			line, _ := json.Marshal(record)
			_, _ = fmt.Fprintf(os.Stderr, "e2e data plane: restarted manager container: %s\n", line)
		}
	}
}

// assertObservedJobsAudited holds every Job the ledger saw, and that
// terminated, to a completed credential audit. The audit runs when a Job
// terminates, so a Job the operator started in the phase's last seconds is
// still running and belongs to whatever finishes it; a Job that vanished is
// the failure this exists for.
func (d *dataPlane) assertObservedJobsAudited() {
	d.t.Helper()
	for _, record := range slices.Clone(d.observed.records) {
		if record.UID == "" {
			d.fatalf("could not validate observed Job UIDs before the final audit assertion")
		}
		if d.fullyAudited.holds(record.UID) {
			continue
		}
		jobs := &batchv1.JobList{}
		if err := d.list(jobs); err != nil {
			d.fatalf("could not read observed Job UID %s back for the final audit assertion", record.UID)
		}
		var job *batchv1.Job
		for index := range jobs.Items {
			if string(jobs.Items[index].UID) == record.UID {
				job = &jobs.Items[index]
				break
			}
		}
		if job == nil {
			d.fatalf("observed Job UID %s disappeared without a complete credential audit", record.UID)
		}
		if job.Status.Succeeded+job.Status.Failed == 0 && !jobTerminal(job) {
			d.logf("observed Job UID %s is still running as this phase ends; its credential audit belongs to whatever finishes it", record.UID)
			continue
		}
		// Terminal and not audited: it finished after the last audit pass and
		// before this sweep. Its Pod is still here, so it is audited now, by
		// the same routine every other terminal Job went through.
		d.auditCompletedJobs()
		if !d.fullyAudited.holds(record.UID) {
			d.fatalf("observed Job UID %s finished without a complete credential audit", record.UID)
		}
		d.logf("observed Job UID %s finished after the last audit pass and was audited in the closing sweep", record.UID)
	}
}

// emitCleanupProjection prints the credential-safe projection of what the
// namespace holds, or says it was withheld: with no scanner, on a read that
// failed, or on a projection that carries a protected pattern.
func (d *dataPlane) emitCleanupProjection() {
	suppressed := func() {
		_, _ = fmt.Fprintln(os.Stderr, "e2e data plane: credential-safe reconciliation diagnostics suppressed")
	}
	if !d.scanner.ready() {
		suppressed()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schemas, events, jobs, leases := &ptahv1alpha1.PtahSchemaList{}, &corev1.EventList{}, &batchv1.JobList{}, &coordinationv1.LeaseList{}
	inTest := client.InNamespace(d.in.TestNamespace)
	if d.cluster.Client.List(ctx, schemas, inTest) != nil || d.cluster.Client.List(ctx, events, inTest) != nil ||
		d.cluster.Client.List(ctx, jobs, inTest) != nil ||
		d.cluster.Client.List(ctx, leases, client.InNamespace(d.in.OperatorNamespace), client.MatchingLabels{
			labelManagedBy: managedByOperator, "operator.ptah.run/coordination": "database-target",
		}) != nil {
		suppressed()
		return
	}
	emitScanned(os.Stderr, d.scanner, projectCleanupDiagnostic(schemas.Items, events.Items, jobs.Items, leases.Items))
}
