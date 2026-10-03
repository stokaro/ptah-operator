//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// exactResult is one proof Job's result as the fault injection captured it.
type exactResult struct {
	result      runner.Result
	operationID string
	podUID      string
}

// captureExactJobResult waits for the Job with the UID to finish, holds it to
// having transported one result under this manager's identity, reads that
// result from its one Pod, and waits for the watches to show the schema bound
// to that operation and the Job added for it. The Job and Pod count as
// audited broadly; only the full audit can count them as audited
// completely.
func (f *faultRun) captureExactJobResult(name, uid, operation string) exactResult {
	f.t.Helper()
	job := f.waitForExactJobTerminal(name, uid)
	if !exactResultJob(job, uid, operation, f.controller) {
		f.fatalf("exact %s Job %s did not transport one result", operation, name)
	}
	operationID := job.Annotations[annotationOperationID]
	pods := &corev1.PodList{}
	f.check(f.list(pods, client.MatchingLabels{"job-name": name}), "list the Pods of %s", name)
	pod, err := resultPod(pods.Items, uid, operationID, f.controller)
	if err != nil {
		f.fatalf("%s: %v", name, err)
	}
	logs, result := f.readResultTransport(job, pod, operation, operationID)
	f.scan(logs, fmt.Sprintf("the exact %s runner transport", operation))
	f.scanObject(result, fmt.Sprintf("the validated %s runner result", operation))
	if !resultBinding(result, f.runnerProtocol, operation, operationID) {
		f.fatalf("runner result lost its runner protocol binding or complete-output guarantee")
	}
	schema := job.Labels[labelSchema]
	f.pollWithoutAudit(fmt.Sprintf("the exact %s result to be bound to the persisted active operation and ADDED Job", operation), func() bool {
		return resultBoundInWatch(f.schemas.snapshot(), f.jobs.snapshot(), schema, operation, operationID, uid)
	})
	f.auditedJobs[uid] = true
	f.auditedPods[string(pod.UID)] = true
	f.audited.add(uid)
	return exactResult{result: result, operationID: operationID, podUID: string(pod.UID)}
}

// pollWithoutAudit is poll for the waits the shell ran checking first and
// auditing after.
func (f *faultRun) pollWithoutAudit(description string, ready func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		f.maybeAudit()
		f.sleep(time.Second)
	}
	f.fatalf("timed out waiting for %s", description)
}

// assertSuccessfulApplyResult waits for the schema watch to hold the Apply's
// result harvested into its pending observation.
func (f *faultRun) assertSuccessfulApplyResult(schema string, run applyRun, captured exactResult) {
	f.t.Helper()
	want := harvestedApply{
		schema: schema, operationID: run.operationID, jobUID: run.jobUID, podUID: run.podUID,
		controller: f.controller, stateVersion: f.stateVersion(),
	}
	var last error
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if last = successfulApplyHarvested(f.schemas.snapshot(), want, captured.result); last == nil {
			return
		}
		f.maybeAudit()
		f.sleep(time.Second)
	}
	f.fatalf("%s did not harvest one exact successful Apply result into its persisted proof snapshot: %v", schema, last)
}

// heldSnapshot reads the schema while status writes are paused and holds it
// to the proof.
func (f *faultRun) heldSnapshot(name string, proof heldProof, what string) {
	f.t.Helper()
	schema := &ptahv1alpha1.PtahSchema{}
	f.check(f.get(name, schema), "read PtahSchema %s", name)
	if err := proof.check(schema); err != nil {
		f.fatalf("%s %s: %v", name, what, err)
	}
}

// harvestedJob is one read-only proof Job a pending observation dispatched.
type harvestedJob struct {
	uid    string
	result exactResult
}

// harvestHeld lets one read-only proof Job of a pending observation run and
// finish while the controller cannot write status, captures its result, and
// holds the schema to what it was when the Job was dispatched. The Job was
// held by the scheduling barrier; status writes are paused before the barrier
// lifts, so the Job's result can only be harvested into the snapshot the
// proof reads once they come back.
func (f *faultRun) harvestHeld(schema, operation string, before checkpoint, description string) harvestedJob {
	f.t.Helper()
	uid := f.waitForOneNewWatchedJob(schema, operation, before, description)
	f.assertReadBlocked(uid, description)
	f.pauseStatusWrites()
	f.stopReadBarrier()
	name := f.liveJobName(uid, description, operationJobs(schema, operation))
	return harvestedJob{uid: uid, result: f.captureExactJobResult(name, uid, operation)}
}

// assertConvergenceResultPair proves a successful Apply converged through one
// Observe and one Plan, each harvested while the Apply's target Lease stayed
// held at its epoch, and returns the two Jobs. A contender named in blocked
// must dispatch nothing while the proof holds the Lease; the scheduling
// barrier is raised again for it before the proof lets go, so its Apply Pod
// stays unscheduled until the proof has bound it to its Lease.
func (f *faultRun) assertConvergenceResultPair(schema string, observeBefore, planBefore checkpoint, applyOperationID string,
	lease leaseIdentity, blocked string,
) (observe, plan harvestedJob) {
	f.t.Helper()
	if !f.readBarrierOn {
		f.fatalf("%s convergence proof started without its scheduling barrier", schema)
	}
	controller, stateVersion := f.controller, f.stateVersion()
	observe = f.harvestHeld(schema, "observe", observeBefore, "one exact post-Apply Observe Job for "+schema)
	f.heldSnapshot(schema, heldProof{
		stage: ptahv1alpha1.OperationObserve, operationID: observe.result.operationID, jobUID: observe.uid,
		leaseEpoch: lease.epoch, outcome: ptahv1alpha1.PendingObservationApplySucceeded,
		applyOperationID: applyOperationID, controller: &controller, stateVersion: stateVersion,
	}, "harvested its Observe while status writes were held")
	f.assertLeaseHeldWithoutRelease(lease)
	if blocked != "" && f.addedJobCount(blocked, "apply") != 0 {
		f.fatalf("%s dispatched while %s retained its Observe proof Lease", blocked, schema)
	}
	f.startReadBarrier()
	f.mustResumeStatusWrites("could not restore controller status-write RBAC after the Observe/Lease proof boundary")

	plan = f.harvestHeld(schema, "plan", planBefore, "one exact post-Apply Plan Job for "+schema)
	f.heldSnapshot(schema, heldProof{
		stage: ptahv1alpha1.OperationPlan, operationID: plan.result.operationID, jobUID: plan.uid,
		leaseEpoch: lease.epoch, outcome: ptahv1alpha1.PendingObservationApplySucceeded,
		applyOperationID: applyOperationID, controller: &controller, stateVersion: stateVersion,
	}, "harvested its Plan while status writes were held")
	f.assertLeaseHeldWithoutRelease(lease)
	if blocked != "" {
		if f.addedJobCount(blocked, "apply") != 0 {
			f.fatalf("%s dispatched while %s retained its Plan proof Lease", blocked, schema)
		}
		f.startReadBarrier()
	}
	f.mustResumeStatusWrites("could not restore controller status-write RBAC after the Plan/Lease proof boundary")
	converged := f.waitForInSync(schema, "ScopedConverged")
	if err := convergedAfterApply(converged, observe.result.result, plan.result.result, controller, stateVersion); err != nil {
		f.fatalf("%s did not carry exact successful Observe and NoChanges Plan results of the runner protocol: %v", schema, err)
	}
	if !completedBeforeAdded(f.jobs.snapshot(), observe.uid, plan.uid) {
		f.fatalf("%s Plan was not dispatched after its exact completed Observe result", schema)
	}
	return observe, plan
}

// captureUncertainReadProofPair proves an Apply whose outcome was unknown
// recovers through one read-only Observe and one Plan, each harvested while
// the Apply's Lease stayed held at its epoch and the pending observation kept
// the Apply's exact identity. The recorded Pod evidence is the Apply's one
// Pod, or, for the Apply Kubernetes ended at its deadline and whose Pod it
// deleted, that Pod or none.
func (f *faultRun) captureUncertainReadProofPair(schema string, run applyRun, lease leaseIdentity,
	observeBefore, planBefore checkpoint, deadline bool,
) (observe, plan harvestedJob) {
	f.t.Helper()
	if !f.readBarrierOn {
		f.fatalf("%s recovery proof started without its scheduling barrier", schema)
	}
	if f.rbac.paused {
		f.fatalf("%s recovery proof started while status writes were still denied", schema)
	}
	controller, stateVersion := f.controller, f.stateVersion()
	pods := &podEvidence{uids: []string{run.podUID}, optional: deadline}
	job := &jobRef{name: run.jobName, uid: run.jobUID}
	f.waitForOutcomeUnknownWatch(schema, run.operationID)
	observe = f.harvestHeld(schema, "observe", observeBefore,
		"one exact read-only Observe Job after the uncertain Apply for "+schema)
	f.heldSnapshot(schema, heldProof{
		stage: ptahv1alpha1.OperationObserve, operationID: observe.result.operationID, jobUID: observe.uid,
		leaseEpoch: lease.epoch, outcome: ptahv1alpha1.PendingObservationOutcomeUnknown,
		applyOperationID: run.operationID, applyJob: job, applyPods: pods,
		controller: &controller, stateVersion: stateVersion,
	}, "did not retain its exact uncertain Apply snapshot through Observe")
	f.assertLeaseHeldWithoutRelease(lease)
	f.startReadBarrier()
	f.mustResumeStatusWrites("could not restore controller status-write RBAC after " + schema + " Observe proof")

	plan = f.harvestHeld(schema, "plan", planBefore,
		"one exact read-only Plan Job after the uncertain Apply for "+schema)
	f.heldSnapshot(schema, heldProof{
		stage: ptahv1alpha1.OperationPlan, operationID: plan.result.operationID, jobUID: plan.uid,
		leaseEpoch: lease.epoch, outcome: ptahv1alpha1.PendingObservationOutcomeUnknown,
		applyOperationID: run.operationID, applyJob: job, applyPods: pods,
		controller: &controller, stateVersion: stateVersion,
	}, "did not retain its exact uncertain Apply snapshot through Plan")
	f.assertLeaseHeldWithoutRelease(lease)
	f.mustResumeStatusWrites("could not restore controller status-write RBAC after " + schema + " Plan proof")
	return observe, plan
}

// assertPostApplyProofHistory holds the watches to one immutable Apply Lease
// and proof snapshot through the Observe and the Plan that proved it.
func (f *faultRun) assertPostApplyProofHistory(schema, applyOperationID, applyJobUID string, lease leaseIdentity,
	observeUID, planUID string,
) {
	f.t.Helper()
	if err := postApplyProofHistory(f.schemas.snapshot(), f.jobs.snapshot(), f.leases.snapshot(), postApplyProof{
		schema: schema, applyOperationID: applyOperationID, applyJobUID: applyJobUID, lease: lease,
		observeJobUID: observeUID, planJobUID: planUID, controller: f.controller, stateVersion: f.stateVersion(),
	}); err != nil {
		f.fatalf("%s did not retain one immutable Apply Lease and proof snapshot through Observe and Plan: %v", schema, err)
	}
}

// assertUncertainApplyProofHistory holds the watches, the schema as it is now
// and the plan it ended at to one uncertain Apply Lease and an immutable proof
// snapshot through the recovery Observe and Plan.
func (f *faultRun) assertUncertainApplyProofHistory(want uncertainProof) {
	f.t.Helper()
	want.controller, want.stateVersion = f.controller, f.stateVersion()
	final := &ptahv1alpha1.PtahSchema{}
	f.check(f.get(want.schema, final), "read PtahSchema %s", want.schema)
	var fresh *ptahv1alpha1.PtahSchemaPlan
	if want.mode != uncertainNoChanges {
		if final.Status.Plan == nil {
			f.fatalf("%s ended with no plan to hold its proof to", want.schema)
		}
		fresh = &ptahv1alpha1.PtahSchemaPlan{}
		f.check(f.get(final.Status.Plan.Name, fresh), "read PtahSchemaPlan %s", final.Status.Plan.Name)
	}
	if err := uncertainApplyProofHistory(f.schemas.snapshot(), f.jobs.snapshot(), f.leases.snapshot(), want, final, fresh); err != nil {
		f.fatalf("%s did not retain one uncertain Apply Lease and immutable proof snapshot through Observe and Plan: %v",
			want.schema, err)
	}
}

// waitForManualDriftContract waits for manual drift to settle through
// OutcomeUnknown at one fresh, unapproved plan of a changed actual state.
func (f *faultRun) waitForManualDriftContract(schema, approval, operationID, oldPlanUID, oldActual string,
	observeBefore checkpoint,
) {
	f.t.Helper()
	f.poll("manual drift to converge through OutcomeUnknown to one fresh unapproved plan", func() bool {
		current := &ptahv1alpha1.PtahSchema{}
		consumed := &ptahv1alpha1.PtahSchemaApproval{}
		if f.get(schema, current) != nil || f.get(approval, consumed) != nil {
			return false
		}
		if !manualDriftSettled(current, consumed, oldPlanUID) || f.newWatchedCount(schema, "observe", observeBefore) != 1 {
			return false
		}
		fresh := &ptahv1alpha1.PtahSchemaPlan{}
		if f.get(current.Status.Plan.Name, fresh) != nil || !manualFreshPlan(fresh, oldPlanUID, oldActual) {
			return false
		}
		if !outcomeUnknownRecorded(f.schemas.snapshot(), schema, operationID) {
			f.fatalf("%s lost its required OutcomeUnknown history", schema)
		}
		return true
	})
}

// freshPlanDocument reads a fresh plan back from its chunks, as the
// controller does, scans it, and returns it with its content digest. A Plan
// result's stdout carries the plan sealed to the manager's key, so the
// chunks are where the plaintext a content digest covers can be read from.
func (f *faultRun) freshPlanDocument(plan *ptahv1alpha1.PtahSchemaPlan, context string) ([]byte, planDocument, string, int) {
	f.t.Helper()
	document, chunks := f.rebuildPlanDocument(plan)
	f.scan(document, context)
	parsed, err := parsePlanDocument(document)
	f.check(err, "%s", context)
	return document, parsed, sha256Digest(document), chunks
}

// assertSealed holds a Plan result's stdout to carrying its plan sealed
// rather than in the clear.
func (f *faultRun) assertSealed(result runner.Result, document []byte, context string) {
	f.t.Helper()
	if err := sealedPayloadLeak(result.Stdout, document); err != nil {
		f.fatalf("%s %v", context, err)
	}
}

// publishFaultSchema publishes the engine's fault fixture from a Job that
// holds the registry credential and no database credential, and requires the
// publisher to report an immutable digest.
func (f *faultRun) publishFaultSchema(engine, dialect, reference string) {
	f.t.Helper()
	file := filepath.Join(repositoryRoot, "testdata", "e2e", engine+"-fault-v1.sql")
	content, err := os.ReadFile(file)
	if err != nil {
		f.fatalf("fault schema fixture is missing: %s", file)
	}
	configMap, name := "e2e-fault-"+engine+"-source", "e2e-fault-push-"+engine
	f.check(f.create(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"namespace": f.in.TestNamespace, "name": configMap},
		"data":     map[string]any{"schema.sql": string(content)},
	}), "create ConfigMap %s", configMap)
	labels := map[string]any{"app.kubernetes.io/component": "e2e-fault-schema-publisher"}
	f.check(f.create(publisherJob(f.in.TestNamespace, name, labels, map[string]any{
		"name": "publisher", "image": f.in.ExecutorImage, "imagePullPolicy": "IfNotPresent",
		"command": []any{"/usr/local/bin/ptah"},
		"args": []any{
			"schema", "push", reference, "--schema-file", "/schema/schema.sql",
			"--dialect", dialect, "--version", "fault-v1", "--plain-http",
		},
		"env": []any{
			map[string]any{"name": "HOME", "value": "/work"},
			map[string]any{"name": "TMPDIR", "value": "/work"},
			secretEnv("PTAH_OCI_USERNAME", registryAuthSecret, "username"),
			secretEnv("PTAH_OCI_PASSWORD", registryAuthSecret, "password"),
			secretEnv("PTAH_OCI_REGISTRY", registryAuthSecret, "registry"),
		},
		"securityContext": restrictedContainer(),
		"volumeMounts": []any{
			map[string]any{"name": "schema", "mountPath": "/schema", "readOnly": true},
			map[string]any{"name": "work", "mountPath": "/work"},
		},
	}, []any{
		map[string]any{"name": "schema", "configMap": map[string]any{"name": configMap}},
		map[string]any{"name": "work", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
	})), "create Job %s", name)
	job := f.waitForPublisherJob(name)
	digests := publishedDigests(f.jobLogs(job, "publisher"))
	if len(digests) == 0 || !sha256Pattern.MatchString(digests[len(digests)-1]) {
		f.fatalf("schema publisher %s did not report an immutable digest", name)
	}
}

// publishPrincipalArtifact publishes the credential-principal artifact with
// the handcrafting fixture, which builds an OCI artifact from the file as it
// is, and returns its digest. Ptah's own publisher would refuse the file.
func (f *faultRun) publishPrincipalArtifact(reference string) string {
	f.t.Helper()
	name := "e2e-push-credential-principal"
	labels := map[string]any{"app.kubernetes.io/component": "e2e-handcrafted-schema-publisher"}
	f.check(f.create(publisherJob(f.in.TestNamespace, name, labels, map[string]any{
		"name": "publisher", "image": f.in.FixtureImage, "imagePullPolicy": "IfNotPresent",
		"command": []any{"/e2e-handcraft-oci"},
		"args":    []any{reference, "/schema/schema.hcl"},
		"env": []any{
			secretEnv("PTAH_OCI_USERNAME", registryAuthSecret, "username"),
			secretEnv("PTAH_OCI_PASSWORD", registryAuthSecret, "password"),
			secretEnv("PTAH_OCI_REGISTRY", registryAuthSecret, "registry"),
		},
		"securityContext": restrictedContainer(),
		"volumeMounts":    []any{map[string]any{"name": "schema", "mountPath": "/schema", "readOnly": true}},
	}, []any{map[string]any{"name": "schema", "secret": map[string]any{
		"secretName": principalSchemaSecret,
		"items":      []any{map[string]any{"key": "schema.hcl", "path": "schema.hcl", "mode": int64(288)}},
	}}})), "create Job %s", name)
	job := f.waitForPublisherJob(name)
	if !principalPublisherIsolated(job, f.in.FixtureImage, reference, principalSchemaSecret, registryAuthSecret) {
		f.fatalf("handcrafted publisher crossed its schema/registry isolation boundary")
	}
	logs := f.jobLogs(job, "publisher")
	f.scan(logs, "the handcrafted OCI publisher log")
	digests := publishedDigests(logs)
	if len(digests) != 1 {
		f.fatalf("handcrafted OCI publisher did not emit exactly one immutable digest")
	}
	return digests[0]
}

// waitForPublisherJob waits for a publisher Job to complete, and ends the
// scenario when it failed.
func (f *faultRun) waitForPublisherJob(name string) *batchv1.Job {
	f.t.Helper()
	var completed *batchv1.Job
	f.poll("schema publisher Job "+name, func() bool {
		job := &batchv1.Job{}
		if f.get(name, job) != nil {
			return false
		}
		if conditionTrue(job.Status.Conditions, batchv1.JobComplete) {
			completed = job
			return true
		}
		if conditionTrue(job.Status.Conditions, batchv1.JobFailed) {
			f.fatalf("schema publisher Job %s failed", name)
		}
		return false
	})
	return completed
}

// publisherJob is a one-shot publisher Job that runs as nobody, mounts no
// service account token, and holds no database credential.
func publisherJob(namespace, name string, labels, container map[string]any, volumes []any) map[string]any {
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"namespace": namespace, "name": name, "labels": labels},
		"spec": map[string]any{
			"backoffLimit": int64(0), "activeDeadlineSeconds": int64(300),
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"restartPolicy": "Never", "automountServiceAccountToken": false,
					"imagePullSecrets": []any{map[string]any{"name": registryPullSecret}},
					"securityContext": map[string]any{
						"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532), "fsGroup": int64(65532),
						"seccompProfile": map[string]any{"type": "RuntimeDefault"},
					},
					"containers": []any{container},
					"volumes":    volumes,
				},
			},
		},
	}
}

func restrictedContainer() map[string]any {
	return map[string]any{
		"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
		"capabilities": map[string]any{"drop": []any{"ALL"}},
	}
}

// recordRunningDeadlineEvidence holds the deadline Apply's Job and Pod to the
// one-shot contract while the Pod runs, scans both, and returns the instant
// its ptah container started. Nothing is counted as audited yet: that waits
// for the watch to show the Pod's deletion.
func (f *faultRun) recordRunningDeadlineEvidence(schema string, deadline int64, run applyRun) string {
	f.t.Helper()
	job := &batchv1.Job{}
	f.check(f.get(run.jobName, job), "read the running-deadline Apply Job %s", run.jobName)
	pod := &corev1.Pod{}
	f.check(f.get(run.podName, pod), "read the running-deadline Apply Pod %s", run.podName)
	if !runningDeadlineJob(job, run.jobUID, schema, run.operationID, deadline) {
		f.fatalf("%s running-deadline Apply Job lost its exact one-shot contract", schema)
	}
	startedAt, ok := runningDeadlinePod(pod, run.podUID, run.jobName, run.jobUID, run.operationID, deadline)
	if !ok {
		f.fatalf("timeout Apply Pod %s lacks exact running pre-deadline evidence", run.podName)
	}
	f.scan(append(f.jsonBytes(job), f.jsonBytes(pod)...), "the exact running pre-deadline Apply Job and Pod")
	return startedAt
}

// waitForDeadlineJobTerminalAndAudit waits for Kubernetes to fail the Job by
// its deadline, then finishes the log stream, waits for the Pod to be gone,
// audits its watch-confirmed deletion, and only then counts the Job as fully
// audited.
func (f *faultRun) waitForDeadlineJobTerminalAndAudit(run applyRun, deadline int64, startedAt string) {
	f.t.Helper()
	if run.podUID == "" {
		f.fatalf("timeout Apply Job %s has no exact fully audited Pod UID", run.jobName)
	}
	var exceeded *batchv1.Job
	f.poll("timeout Apply Job "+run.jobName+" to reach Failed/DeadlineExceeded", func() bool {
		job := &batchv1.Job{}
		if f.get(run.jobName, job) != nil {
			return false
		}
		if string(job.UID) != run.jobUID {
			f.fatalf("timeout Apply Job %s changed UID before terminal audit", run.jobName)
		}
		if deadlineJobExceeded(job, run.jobUID, deadline) {
			exceeded = job
			return true
		}
		return false
	})
	f.finishFollowLogs("the running Apply Pod logs through its Kubernetes deadline")
	f.waitForExactPodAbsence(run.podName, run.podUID)
	f.auditRunningDeadlinePodWatch(deadlinePod{
		name: run.podName, uid: run.podUID, jobName: run.jobName, jobUID: run.jobUID,
		operationID: run.operationID, startedAt: startedAt,
	})
	f.scanObject(exceeded, "the exact DeadlineExceeded Apply Job")
	if !f.fullyAuditedPods[run.podUID] {
		f.fatalf("timeout Apply Job %s lacks its fully audited Pod UID", run.jobName)
	}
	f.auditedJobs[run.jobUID] = true
	f.audited.add(run.jobUID)
	f.fullyAudited.add(run.jobUID)
}

// waitForExactPodAbsence waits for the Pod with the UID to be gone.
func (f *faultRun) waitForExactPodAbsence(name, uid string) {
	f.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		pod := &corev1.Pod{}
		if f.get(name, pod) != nil {
			return
		}
		if string(pod.UID) != uid {
			f.fatalf("timeout Apply Pod %s was replaced before deletion completed", name)
		}
		f.sleep(time.Second)
	}
	f.fatalf("timed out waiting for timeout Apply Pod %s to be deleted", name)
}

// auditRunningDeadlinePodWatch waits for the Pod watch to show the Pod
// running and then deleted, scans the deleted object, and counts the Pod as
// fully audited: its log was streamed whole while it ran.
func (f *faultRun) auditRunningDeadlinePodWatch(pod deadlinePod) {
	f.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if deleted, ok := deadlinePodWatchAudited(f.pods.snapshot(), pod); ok {
			f.scanObject(deleted, "the exact watch-deleted running-deadline Apply Pod")
			f.auditedPods[pod.uid] = true
			f.fullyAuditedPods[pod.uid] = true
			return
		}
		f.assertWatchesAlive()
		f.sleep(time.Second)
	}
	f.fatalf("running-deadline Apply Pod %s lacks exact Running-to-DELETED watch evidence", pod.name)
}

// assertEphemeralContainerRejected sends the running Apply Pod an ephemeral
// container through the subresource, as a person with Pod access could, and
// holds the Pod-intent webhook to refusing it: operation Pods run exactly
// what the builder declared.
func (f *faultRun) assertEphemeralContainerRejected(schema string, run applyRun) {
	f.t.Helper()
	if run.operationID == "" || run.jobName == "" || run.jobUID == "" || run.podName == "" || run.podUID == "" {
		f.fatalf("cannot test ephemeral-container admission without an exact active operation identity")
	}
	pod := &corev1.Pod{}
	f.check(f.get(run.podName, pod), "read the active Pod %s", run.podName)
	if !activePodForEphemeral(pod, run.podUID, run.jobName, run.jobUID, run.operationID) {
		f.fatalf("%s ephemeral-container test lost its exact active Pod identity", schema)
	}
	current := &ptahv1alpha1.PtahSchema{}
	f.check(f.get(schema, current), "read PtahSchema %s", schema)
	if !activeOperationDispatched(current, run.operationID, run.jobName, run.jobUID) {
		f.fatalf("%s changed active operation before its ephemeral-container test", schema)
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"namespace": pod.Namespace, "name": pod.Name, "uid": string(pod.UID), "resourceVersion": pod.ResourceVersion,
		},
		"spec": map[string]any{"ephemeralContainers": []any{map[string]any{
			"name":                refusedEphemeralContainer,
			"image":               "invalid.invalid/ptah-admission-must-deny@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			"imagePullPolicy":     "Never",
			"command":             []any{"/bin/sh"},
			"targetContainerName": "ptah",
		}}},
	})
	f.check(err, "encode the ephemeral-container request")
	err = f.cluster.Clientset.CoreV1().RESTClient().Put().
		AbsPath("/api/v1/namespaces/"+f.in.TestNamespace+"/pods/"+run.podName+"/ephemeralcontainers").
		SetHeader("Content-Type", "application/json").Body(body).Do(f.ctx).Error()
	if err == nil {
		f.fatalf("%s operation Pod admitted an out-of-envelope ephemeral container", schema)
	}
	if message := err.Error(); !containsAll(message, "vpodintent.operator.ptah.run", "denied the request") {
		f.fatalf("%s ephemeral-container request failed outside the Pod-intent webhook: %v", schema, err)
	}
	after := &corev1.Pod{}
	f.check(f.get(run.podName, after), "read the active Pod %s", run.podName)
	if !ephemeralContainerRefused(after, run.podUID, run.jobUID) {
		f.fatalf("%s retained the rejected ephemeral container or changed Pod identity", schema)
	}
}
