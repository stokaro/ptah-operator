//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// maybeAudit runs the fault audit when the last one is faultAuditCadence
// old, so a long wait still audits what finished while it waited, before a
// TTL can delete it.
func (f *faultRun) maybeAudit() {
	f.t.Helper()
	if time.Since(f.lastAudit) >= faultAuditCadence {
		f.auditRuntime()
	}
}

// managerPods lists the manager's Pods by the release's labels.
func (f *faultRun) managerPods() []corev1.Pod {
	f.t.Helper()
	pods := &corev1.PodList{}
	f.check(f.cluster.Client.List(f.ctx, pods, client.InNamespace(f.in.OperatorNamespace), client.MatchingLabels{
		"app.kubernetes.io/name": "ptah-operator", "app.kubernetes.io/instance": f.in.HelmRelease,
		"app.kubernetes.io/component": "controller",
	}), "list the manager Pods")
	return pods.Items
}

// auditRuntime is the fault injection's credential audit. It scans every
// non-Secret object in both namespaces and every manager log; it scans each
// test Pod and the log of every container that started, and records a Pod as
// fully audited once all its containers terminated; and it scans each
// terminal Job with its Pods once every one of them was fully audited, and
// records the Job in the data plane's ledgers too. A Pod or Job already
// fully audited is skipped, and only by a complete ledger. An object that
// disappears before its audit ends the phase, unless it is the one Pod the
// protected log follower is streaming.
func (f *faultRun) auditRuntime() {
	f.t.Helper()
	var resources bytes.Buffer
	for _, list := range []client.ObjectList{
		&ptahv1alpha1.PtahSchemaList{}, &ptahv1alpha1.PtahSchemaPlanList{}, &ptahv1alpha1.PtahSchemaApprovalList{},
		&batchv1.JobList{}, &corev1.PodList{}, &corev1.ConfigMapList{}, &corev1.EventList{},
		&appsv1.DeploymentList{}, &appsv1.ReplicaSetList{}, &corev1.ServiceList{}, &corev1.ServiceAccountList{},
	} {
		f.check(f.list(list), "list %T in %s", list, f.in.TestNamespace)
		resources.Write(f.jsonBytes(list))
	}
	for _, list := range []client.ObjectList{
		&corev1.PodList{}, &corev1.ConfigMapList{}, &corev1.EventList{}, &appsv1.DeploymentList{},
		&appsv1.ReplicaSetList{}, &corev1.ServiceList{}, &corev1.ServiceAccountList{}, &coordinationv1.LeaseList{},
	} {
		f.check(f.cluster.Client.List(f.ctx, list, client.InNamespace(f.in.OperatorNamespace)),
			"list %T in %s", list, f.in.OperatorNamespace)
		resources.Write(f.jsonBytes(list))
	}
	f.scan(resources.Bytes(), "a fault-test non-Secret Kubernetes resource")
	f.auditManagerLogs()
	f.auditTestPods()
	f.auditTerminalJobs()
	f.lastAudit = time.Now()
}

// auditManagerLogs scans the complete log of every live manager Pod. A
// container that restarted has lost the history the scan would have to
// cover, so a restart ends the phase.
func (f *faultRun) auditManagerLogs() {
	f.t.Helper()
	var live []corev1.Pod
	for _, pod := range f.managerPods() {
		if pod.DeletionTimestamp == nil {
			live = append(live, pod)
		}
	}
	if len(live) == 0 {
		f.fatalf("no exact manager Pod was available for credential audit")
	}
	for index := range live {
		if !noRestarts(&live[index]) {
			reportRestartedManagerContainers(f.cluster, live)
			f.fatalf("a manager container restarted before its complete log history was audited")
		}
	}
	for index := range live {
		name := live[index].Name
		current := &corev1.Pod{}
		if err := f.cluster.Client.Get(f.ctx, f.operatorKey(name), current); err != nil || current.UID == "" {
			f.fatalf("manager Pod %s has no exact UID", name)
		}
		logs, stderr, err := f.cluster.Kubectl(f.ctx, "-n", f.in.OperatorNamespace, "logs", "pod/"+name, "--all-containers")
		if err != nil {
			f.fatalf("could not audit logs for exact manager Pod %s UID %s: %s", name, current.UID, strings.TrimSpace(string(stderr)))
		}
		f.scan(append(logs, stderr...), "logs for exact manager Pod "+name)
	}
}

// controllingJobUID is the one batch/v1 Job that controls a Pod, or "" when
// none does. A Pod with two is refused.
func (f *faultRun) controllingJobUID(pod *corev1.Pod) string {
	f.t.Helper()
	owners := controllerOwners(pod.OwnerReferences, "batch/v1", "Job")
	switch len(owners) {
	case 0:
		return ""
	case 1:
		if owners[0].UID == "" {
			f.fatalf("controlling Job owner of Pod %s has no exact UID", pod.Name)
		}
		return string(owners[0].UID)
	}
	f.fatalf("Pod %s has multiple controlling batch/v1 Job owners", pod.Name)
	return ""
}

func (f *faultRun) auditTestPods() {
	f.t.Helper()
	pods := &corev1.PodList{}
	f.check(f.list(pods), "list the fault-test Pods")
	for index := range pods.Items {
		listed := &pods.Items[index]
		name, uid := listed.Name, string(listed.UID)
		if name == "" || uid == "" {
			f.fatalf("could not capture exact fault-test Pod identities for credential audit")
		}
		jobUID := f.controllingJobUID(listed)
		terminal := listed.Status.Phase == corev1.PodSucceeded || listed.Status.Phase == corev1.PodFailed
		if terminal && jobUID != "" && f.fullyAudited.holds(jobUID) {
			f.auditedPods[uid] = true
			f.fullyAuditedPods[uid] = true
			f.auditedJobs[jobUID] = true
			continue
		}
		f.auditTestPod(name, uid)
	}
}

// auditTestPod audits one Pod the way the shell's loop did, and records it as
// fully audited only once every container it declares terminated.
func (f *faultRun) auditTestPod(name, uid string) {
	f.t.Helper()
	pod := &corev1.Pod{}
	if err := f.get(name, pod); err != nil {
		if f.followed(uid) {
			return
		}
		f.fatalf("unaudited fault-test Pod %s UID %s disappeared before its log audit", name, uid)
	}
	if string(pod.UID) != uid {
		f.fatalf("unaudited fault-test Pod %s was replaced before UID %s was audited", name, uid)
	}
	if managedOperation(pod) && !executionIdentityOnPod(pod, f.controller) {
		f.fatalf("managed fault-test Pod %s lacks its exact controller execution identity", name)
	}
	f.scanObject(pod, fmt.Sprintf("the exact live fault-test Pod %s UID %s", name, uid))
	if !noRestarts(pod) {
		f.fatalf("fault-test Pod %s restarted a container before complete log audit", name)
	}
	for _, container := range startedContainers(pod) {
		logs, err := f.cluster.ContainerLog(f.ctx, f.in.TestNamespace, name, container)
		if err != nil {
			if f.followed(uid) {
				return
			}
			f.fatalf("could not audit logs for started container %s in fault-test Pod %s: %v", container, name, err)
		}
		f.scan(logs, fmt.Sprintf("logs for container %s in fault-test Pod %s", container, name))
	}
	after := &corev1.Pod{}
	if err := f.get(name, after); err != nil {
		if f.followed(uid) {
			return
		}
		f.fatalf("unaudited fault-test Pod %s UID %s disappeared during its log audit", name, uid)
	}
	if string(after.UID) != uid {
		f.fatalf("unaudited fault-test Pod %s changed identity during its log audit", name)
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		if !terminalPodLogsComplete(pod) {
			f.fatalf("terminal fault-test Pod %s has unaudited nonterminal containers", name)
		}
		f.auditedPods[uid] = true
		f.fullyAuditedPods[uid] = true
	}
}

func (f *faultRun) auditTerminalJobs() {
	f.t.Helper()
	jobs := &batchv1.JobList{}
	f.check(f.list(jobs), "list the fault-test Jobs")
	for index := range jobs.Items {
		listed := &jobs.Items[index]
		if !jobTerminal(listed) {
			continue
		}
		uid, name := string(listed.UID), listed.Name
		if uid == "" || name == "" {
			f.fatalf("could not capture exact terminal Job identities for fault credential audit")
		}
		if f.fullyAudited.holds(uid) {
			f.auditedJobs[uid] = true
			continue
		}
		f.auditTerminalJob(name, uid)
	}
}

// auditTerminalJob promotes a terminal Job to fully audited once every Pod it
// controls is, and leaves it for a later pass otherwise.
func (f *faultRun) auditTerminalJob(name, uid string) {
	f.t.Helper()
	job := &batchv1.Job{}
	if err := f.get(name, job); err != nil {
		f.fatalf("terminal fault-test Job %s UID %s disappeared before audit", name, uid)
	}
	if string(job.UID) != uid || !jobTerminal(job) {
		f.fatalf("terminal fault-test Job %s was replaced before UID %s was audited", name, uid)
	}
	if managedOperation(job) && !executionIdentityOnJob(job, f.controller) {
		f.fatalf("managed fault-test Job %s lacks its exact controller execution identity", name)
	}
	pods := &corev1.PodList{}
	f.check(f.list(pods), "list the Pods of Job %s", name)
	owned := ownedPods(pods.Items, types.UID(uid))
	if len(owned) == 0 {
		return
	}
	for index := range owned {
		if owned[index].UID == "" {
			f.fatalf("owned Pod of Job %s has no exact UID", name)
		}
		if !f.fullyAuditedPods[string(owned[index].UID)] {
			return
		}
	}
	f.scan(append(f.jsonBytes(job), f.jsonBytes(owned)...),
		fmt.Sprintf("the exact terminal fault-test Job %s and its exact owned Pods", name))
	after := &batchv1.Job{}
	if err := f.get(name, after); err != nil || string(after.UID) != uid {
		f.fatalf("terminal fault-test Job %s changed UID during its Pod audit", name)
	}
	f.auditedJobs[uid] = true
	f.audited.add(uid)
	f.fullyAudited.add(uid)
}

// auditBlockedReadJob audits a Job the scheduling barrier holds where it
// stands. Such a Job never becomes terminal, so no audit pass reaches it, and
// a proof that removes it, or suspends the schema that owns it, would leave
// UIDs the watch recorded and nothing accounted for. A Job whose Pods never
// started wrote no log, so reading it and every Pod it owns, proving they
// never ran and scanning both is the whole audit it owes.
func (f *faultRun) auditBlockedReadJob(name, uid string) {
	f.t.Helper()
	if !f.readBarrierOn {
		f.fatalf("held Job %s was audited without the scheduling barrier", name)
	}
	job := &batchv1.Job{}
	f.check(f.get(name, job), "read held Job %s", name)
	if !uidIs(job, uid) || jobTerminal(job) {
		f.fatalf("held Job %s is not the unfinished Job UID %s", name, uid)
	}
	if !managedOperation(job) || !executionIdentityOnJob(job, f.controller) {
		f.fatalf("held fault-test Job %s lacks its exact controller execution identity", name)
	}
	// The Job controller creates the Pod at once and the barrier leaves it
	// unscheduled, but a Pod that appeared after this audit would reach the
	// watch with nothing in the ledger. So wait for the Pod this Job owns
	// rather than auditing whatever exists at this instant.
	pods := &corev1.PodList{}
	deadline := time.Now().Add(waitTimeout)
	for {
		f.check(f.list(pods, client.MatchingLabels{"batch.kubernetes.io/controller-uid": uid}), "list the Pods of held Job %s", name)
		if len(pods.Items) > 0 {
			break
		}
		if !time.Now().Before(deadline) {
			f.fatalf("held Job %s never created the Pod its audit has to cover", name)
		}
		f.sleep(time.Second)
	}
	if !podsNeverStarted(pods.Items) {
		f.fatalf("a Pod of held Job %s started before its audit could stand for the whole Job", name)
	}
	f.scan(append(f.jsonBytes(job), f.jsonBytes(pods)...),
		fmt.Sprintf("the held fault-test Job %s UID %s and its unstarted Pods", name, uid))
	for index := range pods.Items {
		podUID := string(pods.Items[index].UID)
		if podUID == "" {
			f.fatalf("owned Pod of held Job %s has no exact UID", name)
		}
		f.auditedPods[podUID] = true
		f.fullyAuditedPods[podUID] = true
	}
	f.auditedJobs[uid] = true
	f.audited.add(uid)
	f.fullyAudited.add(uid)
}

// auditProtectedTerminalJob accounts for the Apply Job whose Pod the
// runner-termination proof ended: its whole log was followed through the
// termination, so the Job is audited broadly once its Pod's stream was.
func (f *faultRun) auditProtectedTerminalJob(name, jobUID, podUID string) {
	f.t.Helper()
	job := &batchv1.Job{}
	f.check(f.get(name, job), "read protected Job %s", name)
	if !uidIs(job, jobUID) || !jobTerminal(job) {
		f.fatalf("protected Job %s did not become terminal with its original UID", name)
	}
	f.scanObject(job, "protected terminal Job "+name)
	if !f.auditedPods[podUID] {
		f.fatalf("protected Job %s was terminal before its exact Pod log stream was audited", name)
	}
	f.auditedJobs[jobUID] = true
	f.audited.add(jobUID)
}

// waitForAuditComplete drains the audit of the closed watch history. A
// periodic read can start just before the watches close, so its Job and Pod
// can still be running when the final audit first reaches them. Keep auditing
// until every recorded UID is covered; stopping the watch is not evidence
// that the workloads it recorded have finished.
func (f *faultRun) waitForAuditComplete() {
	f.t.Helper()
	jobs, pods := f.jobs.snapshot(), f.pods.snapshot()
	deadline := time.Now().Add(waitTimeout)
	for {
		f.auditRuntime()
		missingJobs, err := missingWatchedAudits(jobs, f.auditedJobs)
		f.check(err, "could not validate the closed fault-test jobs watch")
		missingPods, err := missingWatchedAudits(pods, f.auditedPods)
		f.check(err, "could not validate the closed fault-test pods watch")
		if len(missingJobs) == 0 && len(missingPods) == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			f.fatalf("timed out after %s waiting for credential audits of watched Job UIDs %v and Pod UIDs %v",
				waitTimeout, missingJobs, missingPods)
		}
		f.sleep(time.Second)
	}
}

// recordJobsForParent adds every Job the watch saw to the data plane's
// observed ledger, which its closing audit holds to having been audited.
func (f *faultRun) recordJobsForParent() {
	f.t.Helper()
	jobs := f.jobs.snapshot()
	if _, err := watchedUIDs(jobs); err != nil {
		f.fatalf("could not validate the fault Job watch before updating the observed ledger: %v", err)
	}
	records, err := observedJobRecords(addedJobs(jobs))
	f.check(err, "could not validate the fault Job watch before updating the observed ledger")
	f.observed.add(records)
}

// logFollower streams one Pod's whole log while a proof destroys or ends the
// Pod, so the history a later audit could not read is scanned anyway.
type logFollower struct {
	command *backgroundCommand
	podUID  string
	// recordPod is set for a Pod in the test namespace, whose UID the
	// follower's scan counts as audited.
	recordPod bool
}

// startFollowLogs starts streaming every container of the Pod. The Pod has to
// be the one named by UID, and have restarted nothing, so the stream starts at
// the beginning of every container's history.
func (f *faultRun) startFollowLogs(namespace, pod, podUID string) {
	f.t.Helper()
	if f.follower != nil {
		f.fatalf("a protected log follower is already running")
	}
	current := &corev1.Pod{}
	f.check(f.cluster.Client.Get(f.ctx, types.NamespacedName{Namespace: namespace, Name: pod}, current),
		"read protected log Pod %s", pod)
	if current.UID == "" {
		f.fatalf("protected log Pod %s has no UID", pod)
	}
	if podUID != "" && string(current.UID) != podUID {
		f.fatalf("protected log Pod %s changed identity before the destructive window", pod)
	}
	if !noRestarts(current) {
		f.fatalf("protected log Pod %s restarted before its continuous audit window", pod)
	}
	f.follower = &logFollower{
		command:   f.startKubectl("-n", namespace, "logs", "-f", "pod/"+pod, "--all-containers"),
		podUID:    podUID,
		recordPod: namespace == f.in.TestNamespace,
	}
	f.sleep(2 * time.Second)
	if f.follower.command.exited() {
		f.fatalf("protected log follower for Pod %s exited before the destructive window", pod)
	}
}

// finishFollowLogs waits for the stream to reach its natural end, scans it,
// and counts the Pod as audited.
func (f *faultRun) finishFollowLogs(context string) {
	f.t.Helper()
	if f.follower == nil {
		f.fatalf("protected log follower is not running")
	}
	command := f.follower.command
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()
	select {
	case <-command.done:
	case <-timer.C:
		f.fatalf("protected log follower did not reach natural EOF after %s", context)
	}
	if command.err != nil {
		f.fatalf("protected log follower failed after %s: %v", context, command.err)
	}
	follower := f.follower
	f.follower = nil
	f.scan(command.output.Bytes(), context)
	if follower.recordPod && follower.podUID != "" {
		f.auditedPods[follower.podUID] = true
	}
}

// followed reports whether the Pod is the one the live follower streams.
// Kubernetes deletes the Pods of a Job that exceeds its active deadline, so
// the running-deadline proof destroys, on purpose, a Pod the periodic audit
// may have listed a moment earlier. That is the one Pod whose disappearance
// loses no evidence: the follower is streaming its complete history, and
// the watch-confirmed deletion is scanned before its UID reaches any ledger.
// Every other Pod that vanishes unaudited still ends the phase.
func (f *faultRun) followed(podUID string) bool {
	return f.follower != nil && f.follower.recordPod && f.follower.podUID != "" && f.follower.podUID == podUID
}

// managerReplicas is the replica count the manager's Deployment asks for.
func (f *faultRun) managerReplicas() (int, bool) {
	deployment := &appsv1.Deployment{}
	if err := f.cluster.Client.Get(f.ctx, f.operatorKey(f.controllerName), deployment); err != nil ||
		deployment.Spec.Replicas == nil || *deployment.Spec.Replicas <= 0 {
		return 0, false
	}
	return int(*deployment.Spec.Replicas), true
}

// loadReadyManagerPodUIDs waits for every manager replica to have one ready,
// live Pod, and returns their UIDs.
func (f *faultRun) loadReadyManagerPodUIDs() []string {
	f.t.Helper()
	var uids []string
	f.pollQuiet("every manager replica to have one ready non-terminating Pod", func() bool {
		replicas, ok := f.managerReplicas()
		if !ok {
			return false
		}
		pods := &corev1.PodList{}
		if err := f.cluster.Client.List(f.ctx, pods, client.InNamespace(f.in.OperatorNamespace), client.MatchingLabels{
			"app.kubernetes.io/name": "ptah-operator", "app.kubernetes.io/instance": f.in.HelmRelease,
			"app.kubernetes.io/component": "controller",
		}); err != nil {
			return false
		}
		uids, ok = readyManagerPodUIDs(pods.Items, replicas)
		return ok
	})
	return uids
}

// loadReadyManagerLeader waits for the leader Lease to name a ready manager
// Pod of the release other than the one previousUID names, and returns it.
func (f *faultRun) loadReadyManagerLeader(previousUID string) (name, uid string) {
	f.t.Helper()
	f.pollQuiet("the manager leader Lease to identify a ready replacement Pod", func() bool {
		lease := &coordinationv1.Lease{}
		if err := f.cluster.Client.Get(f.ctx, f.operatorKey(leaderLeaseName), lease); err != nil ||
			lease.Spec.HolderIdentity == nil {
			return false
		}
		candidate := leaderPodName(*lease.Spec.HolderIdentity)
		if candidate == "" {
			return false
		}
		pod := &corev1.Pod{}
		if err := f.cluster.Client.Get(f.ctx, f.operatorKey(candidate), pod); err != nil ||
			!leaderPodReady(pod, previousUID, f.in.HelmRelease) {
			return false
		}
		name, uid = candidate, string(pod.UID)
		return true
	})
	return name, uid
}

// replaceManagerPods deletes the manager's Pods one at a time and waits for
// the rollout after each. Replacing the Pods is what the proof needs, rather
// than a rollout restart that would change the Pod template. One at a time,
// because the manager serves the admission webhooks, and deleting every
// replica at once leaves the API server with no backend for a request they
// gate until a replacement is ready.
func (f *faultRun) replaceManagerPods() {
	f.t.Helper()
	var names []string
	for _, pod := range f.managerPods() {
		names = append(names, pod.Name)
	}
	if len(names) == 0 {
		f.fatalf("no manager Pod was available to replace")
	}
	for _, name := range names {
		pod := &corev1.Pod{}
		err := f.cluster.Client.Get(f.ctx, f.operatorKey(name), pod)
		if apierrors.IsNotFound(err) {
			f.fatalf("manager Pod %s disappeared before the phase could replace it", name)
		}
		f.check(err, "read manager Pod %s", name)
		f.check(f.cluster.Client.Delete(f.ctx, pod), "delete manager Pod %s", name)
		deadline := time.Now().Add(waitTimeout)
		for {
			current := &corev1.Pod{}
			err := f.cluster.Client.Get(f.ctx, f.operatorKey(name), current)
			if apierrors.IsNotFound(err) || (err == nil && current.UID != pod.UID) {
				break
			}
			if !time.Now().Before(deadline) {
				f.fatalf("manager Pod %s was not deleted", name)
			}
			f.sleep(time.Second)
		}
		f.check(f.cluster.WaitForRollout(f.ctx, f.in.OperatorNamespace, f.controllerName, waitTimeout),
			"wait for the manager rollout after deleting %s", name)
		f.loadReadyManagerPodUIDs()
	}
}
