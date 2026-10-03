//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// The rows that reach a migration's Apply from outside while it runs: the
// resource deleted, the run stopped at its deadline, the run's log lost, the
// resource suspended, a failed read retried, and the Lease release refused.

// mfPrintState prints lines that name a migration's state when a check about it
// failed. The fields carry no credential, and the lines are scanned anyway and
// withheld on a match.
func (m *migrationRun) mfPrintState(heading string, lines []string) {
	content := []byte(heading + "\n" + strings.Join(lines, "\n") + "\n")
	if !m.scanner.ready() || m.scanner.leaks(content) {
		_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: %s is withheld: it matched a protected credential\n", heading)
		return
	}
	_, _ = os.Stderr.Write([]byte("e2e migrations: " + string(content)))
}

// mfMigrationOrNil reads a migration without failing, for the waits that
// tolerated a failed read.
func (m *migrationRun) mfMigrationOrNil(name string) *ptahv1alpha1.PtahMigration {
	migration := &ptahv1alpha1.PtahMigration{}
	if m.get(name, migration) != nil {
		return nil
	}
	return migration
}

// mfWaitForDispatchedApply waits for the migration to dispatch an Apply bound
// to its own Job, and returns the Job's name and UID.
func (m *migrationRun) mfWaitForDispatchedApply(name string) (jobName, jobUID string) {
	m.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if migration := m.mfMigrationOrNil(name); migration != nil {
			if job, uid, ok := mfDispatchedApply(migration); ok {
				return job, uid
			}
		}
		m.sleep(2 * time.Second)
	}
	m.fatalf("%s did not dispatch an Apply bound to its own Job within %s", name, waitTimeout)
	return "", ""
}

// mfJobUIDs lists every Job a resource has dispatched, whatever the
// operation, by UID, sorted.
func (m *migrationRun) mfJobUIDs(name string) []string {
	m.t.Helper()
	jobs := &batchv1.JobList{}
	m.check(m.list(jobs, client.MatchingLabels{labelMigration: name}), "the Jobs of %s could not be listed", name)
	uids := make([]string, 0, len(jobs.Items))
	for _, job := range jobs.Items {
		uids = append(uids, string(job.UID))
	}
	slices.Sort(uids)
	return uids
}

// mfDeleteMigration deletes a migration and waits for its finalizer to
// let it go, as kubectl delete --wait=true did.
func (m *migrationRun) mfDeleteMigration(name string) {
	m.t.Helper()
	migration := &ptahv1alpha1.PtahMigration{}
	migration.Namespace, migration.Name = m.in.TestNamespace, name
	m.deleteAndWait(migration, name)
}

// deletionDuringApplyProof deletes a migration while its Apply is still
// running, in the background, which is kubectl's default. The resource has to
// stay, holding the claim, while the executor keeps running inside it; and
// once the Job is gone -- the case nothing wakes the controller for -- it has
// to go on its own, leaving what the run committed where it is. Foreground
// propagation removes the resource's dependents first, the Apply Job among
// them, and is documented rather than asserted.
func (m *migrationRun) deletionDuringApplyProof() {
	m.t.Helper()
	name, database := "e2e-deletion-"+m.engine.name, "ptah_e2e_deletion"
	secret := "e2e-" + m.engine.name + "-deletion-db"
	m.isolatedDatabase(database, secret)
	// The uncertain row's artifact, against a database of its own: its third
	// migration sleeps, which is the window this proof needs.
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference("-uncertain"),
		coordinationKey: "e2e/deletion/" + m.engine.name, apply: "Always", interval: "30s",
	}))
	job, _ := m.mfWaitForDispatchedApply(name)
	// Migrations 1 and 2 committed, so the executor is inside the third and
	// the database holds work a vanished resource would have stopped
	// accounting for.
	committed := "SELECT count(*) FROM schema_migrations WHERE version <= 2 AND state = 'applied'"
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) && m.query(committed, database) != "2" {
		m.sleep(2 * time.Second)
	}
	if m.query(committed, database) != "2" {
		m.fatalf("the %s deletion run did not commit its first two migrations within %s", m.engine.name, waitTimeout)
	}

	m.logf("deleting the %s PtahMigration while its Apply is still running", m.engine.kind)
	migration := &ptahv1alpha1.PtahMigration{}
	migration.Namespace, migration.Name = m.in.TestNamespace, name
	if err := m.cluster.Client.Delete(m.ctx, migration, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		m.fatalf("%s could not be marked for deletion: %v", name, err)
	}
	// The resource stays, and the executor keeps running inside it. Both
	// halves matter: without the second the retention is about nothing.
	for hold := time.Now().Add(20 * time.Second); time.Now().Before(hold); {
		m.assertDeletionRetainsItsRunningApply(name, job)
		m.sleep(5 * time.Second)
	}

	// Now take the Job away, which is the case nothing wakes the controller
	// for: no Job left to change, and a status write the primary watch
	// discards.
	m.logf("removing the %s Apply Job so only the settling pass is left", m.engine.kind)
	applyJob := &batchv1.Job{}
	applyJob.Namespace, applyJob.Name = m.in.TestNamespace, job
	m.deleteAndWait(applyJob, "the "+m.engine.name+" Apply Job")

	// The resource has to go on its own. A manager whose settling pass asked
	// for nothing to follow it would sit here holding the finalizer.
	release := time.Now().Add(waitTimeout)
	for time.Now().Before(release) {
		if err := m.get(name, &ptahv1alpha1.PtahMigration{}); apierrors.IsNotFound(err) {
			m.logf("PASS %s held a deleted migration until its Apply could not write", m.engine.kind)
			// The rows the run committed are still the database's own
			// business: deletion accounts for the run, it does not undo it.
			if m.query("SELECT count(*) FROM e2e_migration_widgets", database) != "3" {
				m.fatalf("the %s deletion changed the rows the run had already committed", m.engine.name)
			}
			return
		}
		m.sleep(5 * time.Second)
	}
	held := &unstructured.Unstructured{}
	held.SetAPIVersion(ptahSchemaAPIVersion)
	held.SetKind("PtahMigration")
	summary := ""
	if m.get(name, held) == nil {
		// The stored document, where an absent field reads as null.
		finalizers, _, _ := unstructured.NestedFieldNoCopy(held.Object, "metadata", "finalizers")
		phase, _, _ := unstructured.NestedFieldNoCopy(held.Object, "status", "phase")
		active, _, _ := unstructured.NestedFieldNoCopy(held.Object, "status", "activeOperation")
		if content, err := json.Marshal(map[string]any{"finalizers": finalizers, "phase": phase, "activeOperation": active}); err == nil {
			summary = string(content)
			if !m.scanner.ready() || m.scanner.leaks(content) {
				summary = "(withheld: it matched a protected credential)"
			}
		}
	}
	m.fatalf("%s kept its finalizer after nothing it dispatched could write: %s", name, summary)
}

// assertDeletionRetainsItsRunningApply holds everything the deletion wait is
// for: the resource, its claim, the Pod that may still be writing, and the
// Job that Pod belongs to.
func (m *migrationRun) assertDeletionRetainsItsRunningApply(name, job string) {
	m.t.Helper()
	migration := &ptahv1alpha1.PtahMigration{}
	if m.get(name, migration) != nil {
		m.fatalf("%s was released while its Apply Pod was still running", name)
	}
	if !deletionRetainsClaim(migration) {
		m.fatalf("%s dropped the claim that accounts for its running Apply", name)
	}
	pods := &corev1.PodList{}
	if m.list(pods, client.MatchingLabels{"job-name": job}) != nil || !mfOnePodRunning(pods.Items) {
		m.fatalf("the %s Apply Pod stopped, so the retention above proved nothing", m.engine.name)
	}
	if m.get(job, &batchv1.Job{}) != nil {
		m.fatalf("the %s Apply Job was collected while the deletion was still waiting on it", m.engine.name)
	}
}

// stopRowResource creates the migration the stopped and lost-log rows run:
// Always, because both rows are about a run that started and not about the
// gate that authorizes one, with the execution deadline given.
func (m *migrationRun) stopRowResource(name, secret, reference, coordinationKey string, deadline int64) {
	m.t.Helper()
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: reference, coordinationKey: coordinationKey,
		apply: "Always", interval: "30s",
		execution: map[string]any{"activeDeadlineSeconds": deadline, "failureRetryInterval": "10s", "connectTimeout": "30s"},
	}))
}

// waitForStopRowApply waits for the Apply the resource dispatched, named by
// its Job's UID, and returns the absolute deadline its claim carries.
func (m *migrationRun) waitForStopRowApply(name string) (jobUID string, notAfter time.Time) {
	m.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if migration := m.mfMigrationOrNil(name); migration != nil {
			if uid, at, ok := stopRowApply(migration); ok {
				return uid, at
			}
		}
		m.sleep(2 * time.Second)
	}
	m.fatalf("%s did not dispatch an Apply bound to its own Job within %s", name, waitTimeout)
	return "", time.Time{}
}

// readStopRowPod is the one Pod the Apply Job owns, found by the Job's UID.
func (m *migrationRun) readStopRowPod(jobUID string) *corev1.Pod {
	m.t.Helper()
	pods := &corev1.PodList{}
	m.check(m.list(pods, client.MatchingLabels{"batch.kubernetes.io/controller-uid": jobUID}),
		"the Pods of Apply Job %s could not be read", jobUID)
	if len(pods.Items) != 1 {
		m.fatalf("Apply Job %s owns %d Pods, not one", jobUID, len(pods.Items))
	}
	return &pods.Items[0]
}

func (m *migrationRun) readStopRowJob(pod *corev1.Pod, jobUID string) *batchv1.Job {
	m.t.Helper()
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "batch/v1" || owner.Kind != "Job" || string(owner.UID) != jobUID {
		m.fatalf("the Apply Pod does not belong to the original Job")
	}
	job := &batchv1.Job{}
	m.check(m.get(owner.Name, job), "read the original Apply Job")
	if string(job.UID) != jobUID {
		m.fatalf("the Apply Job was replaced before its result was checked")
	}
	return job
}

// stopRowSleepingSessions counts the sessions in one database executing a
// sleep, other than the one asking. Both fixtures the rows use end in a
// migration that does nothing but sleep, and a session in it is the evidence
// that the run is inside that migration: the statement that commits the
// migration before it has already returned.
func (m *migrationRun) stopRowSleepingSessions(database string) int {
	statement := `SELECT count(*) FROM pg_stat_activity
                     WHERE datname = current_database() AND state = 'active'
                       AND query LIKE '%pg_sleep%' AND pid <> pg_backend_pid()`
	if m.engine.name == "mysql" {
		statement = `SELECT COUNT(*) FROM information_schema.processlist
                     WHERE db = '` + database + `' AND info LIKE '%SLEEP(%' AND id <> CONNECTION_ID()`
	}
	return mfSessionCount(m.query(statement, database))
}

// endStopRowSleepingSessions ends every session still sleeping in one
// database, and fails if one remains. MySQL's driver stops a statement by
// closing its connection, and the server goes on running the statement until
// it returns, holding what that session held -- Ptah's migration lock among
// them, whose name is the same on every database of the server. PostgreSQL
// cancels the statement itself, and the query finds nothing to end.
func (m *migrationRun) endStopRowSleepingSessions(database string) {
	m.t.Helper()
	switch m.engine.name {
	case "postgresql":
		m.query(`SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
                     WHERE datname = current_database() AND query LIKE '%pg_sleep%'
                       AND pid <> pg_backend_pid()`, database)
	case "mysql":
		sessions := m.query(`SELECT COALESCE(GROUP_CONCAT(id), '') FROM information_schema.processlist
                     WHERE db = '`+database+`' AND info LIKE '%SLEEP(%' AND id <> CONNECTION_ID()`, database)
		for session := range strings.SplitSeq(sessions, ",") {
			if session != "" {
				_, _ = m.sqlStatement(database, "KILL "+session)
			}
		}
	}
	// A count that does not read as a number ended the shell's wait, which
	// tested it with -ne; the same reading ends this one.
	deadline := time.Now().Add(30 * time.Second)
	for {
		count := m.stopRowSleepingSessions(database)
		if count <= 0 {
			return
		}
		if !time.Now().Before(deadline) {
			m.fatalf("a session is still sleeping in %s on %s after it was ended", database, m.engine.name)
		}
		m.sleep(2 * time.Second)
	}
}

// waitForStopRowSleep waits until the run has committed every migration
// below sleepingIn and is sleeping in sleepingIn.
func (m *migrationRun) waitForStopRowSleep(database string, sleepingIn int) {
	m.t.Helper()
	committed := "SELECT count(*) FROM schema_migrations WHERE version < " + strconv.Itoa(sleepingIn) + " AND state = 'applied'"
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if m.query(committed, database) == strconv.Itoa(sleepingIn-1) && m.stopRowSleepingSessions(database) >= 1 {
			return
		}
		m.sleep(2 * time.Second)
	}
	m.fatalf("the %s run against %s never reached migration %d within %s", m.engine.name, database, sleepingIn, waitTimeout)
}

// waitForStopRowLastRun waits for the record of this Job's run, and returns
// the reading that carried it: the document that matched, not a later read.
func (m *migrationRun) waitForStopRowLastRun(name, jobUID string) *ptahv1alpha1.PtahMigration {
	m.t.Helper()
	var last *ptahv1alpha1.PtahMigration
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		last = m.migration(name)
		if mfRunRecordedFor(last, jobUID) {
			return last
		}
		m.sleep(2 * time.Second)
	}
	if last != nil {
		m.mfPrintLastRun(name, last)
	}
	m.fatalf("%s recorded no run for its Apply Job %s within %s", name, jobUID, waitTimeout)
	return nil
}

// mfPrintLastRun prints the phase and the runs a migration recorded, scanned.
func (m *migrationRun) mfPrintLastRun(name string, migration *ptahv1alpha1.PtahMigration) {
	content, err := json.Marshal(map[string]any{
		"phase": migration.Status.Phase, "lastRun": migration.Status.LastRun, "unresolvedRun": migration.Status.UnresolvedRun,
	})
	if err != nil {
		return
	}
	m.mfPrintState(name+" record", []string{"  " + string(content)})
}

// stoppedApplyProof stops a migration Apply part way at the execution
// deadline the runner imposes on Ptah whatever the Pod is doing, and holds
// the record to the report Ptah wrote: Failed, with the first migration
// applied (#452). The execution deadline keeps the Pod available so its
// termination timestamp can prove that SIGTERM was handled within the grace.
// An acknowledged durable result survives later Pod loss. The revision table
// must say the same, and the Pod's own record must show the runner ended after the
// deadline and inside the grace.
func (m *migrationRun) stoppedApplyProof() {
	m.t.Helper()
	name, database := "e2e-stopped-"+m.engine.name, "ptah_e2e_stopped"
	secret := "e2e-" + m.engine.name + "-stopped-db"
	m.isolatedDatabase(database, secret)
	m.publish("stopped", m.fixtureDir("-stopped"), m.reference("-stopped"))
	m.stopRowResource(name, secret, m.reference("-stopped"), "e2e/stopped/"+m.engine.name, stoppedApplyWindowSeconds)
	jobUID, notAfter := m.waitForStopRowApply(name)
	m.waitForStopRowSleep(database, 2)
	// A run that reached its second migration only after the window closed
	// was refused or cut off by something other than the stop under proof.
	if !time.Now().Before(notAfter) {
		m.fatalf("the %s run reached its second migration only after its window closed at %d; raise stoppedApplyWindowSeconds",
			m.engine.name, notAfter.Unix())
	}
	m.logf("the %s run is inside its second migration, %d seconds before its window closes",
		m.engine.kind, notAfter.Unix()-time.Now().Unix())

	recorded := m.waitForStopRowLastRun(name, jobUID)
	if !stoppedRunRecorded(recorded.Status, jobUID, 2) {
		m.mfPrintLastRun(name, recorded)
		m.fatalf("%s did not record the account its stopped run gave", name)
	}
	// The database says the same: the first migration applied, the second
	// recorded as a failure with none of its statements committed.
	if m.query("SELECT count(*) FROM schema_migrations WHERE version = 1 AND state = 'applied'", database) != "1" {
		m.fatalf("the %s database does not record the first migration of the stopped run as applied", m.engine.name)
	}
	if m.query("SELECT count(*) FROM schema_migrations WHERE version = 2 AND state <> 'applied' AND applied = 0", database) != "1" {
		m.fatalf("the %s database does not record the stopped migration as failed with nothing committed", m.engine.name)
	}
	m.endStopRowSleepingSessions(database)

	// Dated by the Pod's own record of the process, not by when this row
	// looked: the runner ended after the window closed, and inside the grace
	// the Pod had.
	pod := m.readStopRowPod(jobUID)
	if !runnerEndedInGrace(pod, notAfter) {
		var states []corev1.ContainerState
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "ptah" {
				states = append(states, status.State)
			}
		}
		if content, err := json.Marshal(map[string]any{"grace": pod.Spec.TerminationGracePeriodSeconds, "ptah": states}); err == nil {
			m.mfPrintState(pod.Name+" runner", []string{"  " + string(content)})
		}
		m.fatalf("the %s runner did not end between its deadline at %d and the end of its grace", m.engine.name, notAfter.Unix())
	}
	// Read the transport selected by this exact Job. Durable delivery writes
	// no result frame to stdout; its receipt must carry the same run report.
	job := m.readStopRowJob(pod, jobUID)
	logs, err := m.stopRowLog(pod.Name)
	if err != nil {
		m.fatalf("the stopped %s Apply Pod's log could not be read: %v", m.engine.name, err)
	}
	m.scan(logs, "the stopped Apply's log")
	_, err = readRecordedMigrationApply(m.ctx, m.cluster.Client, job, pod, recorded.Status.LastRun, logs)
	m.check(err, "the stopped %s Apply has no matching complete result", m.engine.name)
	m.logf("PASS %s run stopped at its deadline reported what it applied", m.engine.kind)
}

// stopRowLog reads the ptah container's log within twenty seconds, as kubectl
// logs --request-timeout=20s did.
func (m *migrationRun) stopRowLog(pod string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
	defer cancel()
	return m.cluster.ContainerLog(ctx, m.in.TestNamespace, pod, "ptah")
}

// lostLogProof removes a running Apply's log and verifies its durable receipt
// and termination summary. Legacy Jobs still use the summary fallback (#453).
// The log directory is replaced with a file rather than removed: the
// kubelet reopens a running container's missing log every ten seconds, and a
// directory it could recreate would let the frame reach a new log. The run is
// inside its last migration, which sleeps, when that happens, and the row
// checks it is still there afterwards, so the frame cannot have been written
// before the log went.
func (m *migrationRun) lostLogProof() {
	m.t.Helper()
	if m.in.DockerContext == "" {
		m.fatalf("E2E_DOCKER_CONTEXT is not set, and the lost-log row reaches the node through it")
	}
	name, database := "e2e-lost-log-"+m.engine.name, "ptah_e2e_lost_log"
	secret := "e2e-" + m.engine.name + "-lost-log-db"
	m.isolatedDatabase(database, secret)
	m.stopRowResource(name, secret, m.reference("-uncertain"), "e2e/lost-log/"+m.engine.name, 300)
	jobUID, _ := m.waitForStopRowApply(name)
	m.waitForStopRowSleep(database, 3)

	pod := m.readStopRowPod(jobUID)
	job := m.readStopRowJob(pod, jobUID)
	directory := "/var/log/pods/" + m.in.TestNamespace + "_" + pod.Name + "_" + string(pod.UID) + "/ptah"
	if _, err := m.mfDocker("exec", pod.Spec.NodeName, "test", "-d", directory); err != nil {
		m.fatalf("%s keeps no log directory for the %s Apply Pod at %s", pod.Spec.NodeName, m.engine.name, directory)
	}
	m.logf("removing the %s Apply log on %s while its run is still going", m.engine.kind, pod.Spec.NodeName)
	if _, err := m.mfDocker("exec", pod.Spec.NodeName, "sh", "-c", `rm -rf -- "$1" && : >"$1"`, "sh", directory); err != nil {
		m.fatalf("the %s Apply log on %s could not be removed: %v", m.engine.name, pod.Spec.NodeName, err)
	}
	if m.stopRowSleepingSessions(database) < 1 {
		m.fatalf("the %s run had left its last migration before its log was removed, so its frame may be in the log", m.engine.name)
	}

	recorded := m.waitForStopRowLastRun(name, jobUID)
	// The obstruction must still exist when the result is consumed. Absence
	// of a frame alone proves nothing for a Job that never writes one.
	if _, err := m.mfDocker("exec", pod.Spec.NodeName, "test", "-f", directory); err != nil {
		m.fatalf("the removed Apply log directory was recreated before result consumption")
	}
	logs, err := m.stopRowLog(pod.Name)
	if err != nil {
		logs = append(logs, []byte(err.Error())...)
	}
	m.scan(logs, "the lost Apply log")
	if strings.Contains(string(logs), mfResultMarker) {
		m.fatalf("the %s Apply Pod's log still holds a frame, so the row lost nothing", m.engine.name)
	}

	// The summary remains bound to the complete result even when stdout is
	// diagnostic only. Durable Jobs must not settle from the summary alone.
	pod = m.readStopRowPod(jobUID)
	message, found := mfTerminationMessage(pod)
	if !found {
		m.fatalf("the %s Apply Pod's status carries no termination message", m.engine.name)
	}
	m.scan([]byte(message), "the Apply's termination summary")
	digest, err := summaryFrameDigest(message)
	if err != nil {
		m.fatalf("the %s Apply Pod's termination message is not a runner summary: %v", m.engine.name, err)
	}
	if durableResultJob(job) {
		result, err := readRecordedMigrationApply(m.ctx, m.cluster.Client, job, pod, recorded.Status.LastRun, nil)
		m.check(err, "%s has no complete durable Apply result after log removal", name)
		if recorded.Status.LastRun.Outcome != ptahv1alpha1.MigrationRunOutcomeApplied || !slices.Equal(recorded.Status.LastRun.AppliedVersions, []int64{1, 2, 3}) {
			m.fatalf("%s did not record all three migrations from its durable result", name)
		}
		summary, err := runner.EncodeSummary(result)
		m.check(err, "summarize the exact persisted Apply result")
		bound, err := runner.ParseSummaryFor(string(summary), runner.OperationMigrationApply, job.Annotations[annotationOperationID])
		m.check(err, "read the persisted Apply result's summary binding")
		if digest != bound.FrameDigest {
			m.fatalf("the termination summary differs from the persisted Apply result")
		}
	} else if !lostLogRunRecorded(recorded.Status, jobUID, digest, 3) {
		m.mfPrintLastRun(name, recorded)
		m.fatalf("%s did not settle its legacy run from the termination summary", name)
	}
	if m.query("SELECT count(*) FROM schema_migrations WHERE state = 'applied'", database) != "3" {
		m.fatalf("the %s database does not record the three migrations the summary reported", m.engine.name)
	}
	m.logf("PASS %s run whose log was lost retained its exact Apply outcome", m.engine.kind)
}

// docker runs the Docker CLI against the daemon the kind cluster runs on.
func (m *migrationRun) mfDocker(arguments ...string) ([]byte, error) {
	command := exec.CommandContext(m.ctx, "docker", append([]string{"--context", m.in.DockerContext}, arguments...)...) //nolint:gosec // Arguments, not a shell.
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("docker %s: %w", arguments[0], err)
	}
	return output, nil
}

// retryIntervalProof points a migration at a database that does not exist and
// holds it to its failure retry interval: nothing dispatches while the
// deadline its claim persisted stands, and a replacement does once it passes.
// The claim carries the deadline, so a restarted manager and an early watch
// event both meet it; what only a cluster shows is that nothing else -- a Job
// event, a resync, the queue's own backoff -- dispatches the replacement
// early. Resolve and verify succeed, and the history read is the one that
// cannot connect.
func (m *migrationRun) retryIntervalProof() {
	m.t.Helper()
	m.logf("pointing a %s migration at a database that does not exist", m.engine.kind)
	name, secret := "e2e-retry-"+m.engine.name, "e2e-"+m.engine.name+"-retry-db"
	url := m.databaseURL("ptah_e2e_absent", "")
	m.protect(url)
	m.createDatabaseSecret(secret, "ptah_e2e_absent", url)
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference(""), coordinationKey: "e2e/retry/" + m.engine.name,
		apply: "Never", interval: "1h",
		execution: map[string]any{"activeDeadlineSeconds": int64(300), "failureRetryInterval": "120s", "connectTimeout": "15s"},
	}))

	// A retry scheduled: a second attempt carrying the deadline the resource
	// asked for. The attempt rather than a phase, because the resource passes
	// through several while it fails.
	var scheduled *ptahv1alpha1.PtahMigration
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		current := &ptahv1alpha1.PtahMigration{}
		m.check(m.get(name, current), "%s could not be read", name)
		if retryScheduled(current) {
			scheduled = current
			break
		}
		m.sleep(5 * time.Second)
	}
	if scheduled == nil {
		m.fatalf("%s never scheduled a retry carrying its own deadline within %s", name, waitTimeout)
	}
	m.scanObject(scheduled, name+" status")

	// Nothing dispatches while the deadline stands, and the hold runs against
	// the deadline the claim persisted rather than a fixed share of the
	// interval: holding sixty seconds of a hundred and twenty would pass a
	// manager that honored half the interval.
	before := m.mfJobUIDs(name)
	notBefore, ok := retryDeadline(scheduled)
	if !ok {
		m.fatalf("%s carries no readable retry deadline", name)
	}
	if notBefore.Unix() <= time.Now().Unix() {
		m.fatalf("%s scheduled its retry in the past, so this row would hold nothing", name)
	}
	// The last poll starts before the deadline and reads the Jobs a moment
	// after it, so a dispatch the operator is entitled to make is not read as
	// an early one. The dispatch check below closes that moment against the
	// same timestamp.
	for time.Now().Unix() < notBefore.Unix()-5 {
		current := &ptahv1alpha1.PtahMigration{}
		m.check(m.get(name, current), "%s could not be read", name)
		if _, standing := retryDeadline(current); !standing {
			m.fatalf("%s dropped its retry deadline while it was still standing", name)
		}
		if len(mfAddedUIDs(m.mfJobUIDs(name), before)) != 0 {
			m.fatalf("%s dispatched a replacement Job before its retry interval expired", name)
		}
		m.sleep(5 * time.Second)
	}

	// And it does run once the deadline passes, so the delay is a wait
	// rather than a stop. When the Job appeared is dated by the Job's own
	// creation time, not by the poll that noticed it: a poll every five
	// seconds can first see a Job created inside the hold.
	dispatched := false
	for window := time.Now().Add(180 * time.Second); time.Now().Before(window); {
		jobs := &batchv1.JobList{}
		m.check(m.list(jobs, client.MatchingLabels{labelMigration: name}), "the Jobs of %s could not be listed", name)
		var current []string
		created := map[string]metav1.Time{}
		for _, job := range jobs.Items {
			current = append(current, string(job.UID))
			created[string(job.UID)] = job.CreationTimestamp
		}
		slices.Sort(current)
		if added := mfAddedUIDs(current, before); len(added) > 0 {
			stamp := created[added[0]]
			if stamp.IsZero() {
				m.fatalf("%s dispatched a Job this proof cannot date", name)
			}
			if stamp.Unix() < notBefore.Unix() {
				m.fatalf("%s created its replacement Job %ds before the deadline it persisted", name, notBefore.Unix()-stamp.Unix())
			}
			dispatched = true
			break
		}
		m.sleep(5 * time.Second)
	}
	if !dispatched {
		m.fatalf("%s never dispatched after its retry interval expired", name)
	}
	m.mfDeleteMigration(name)
	m.logf("PASS %s waited out its retry interval and then ran", m.engine.kind)
}

// suspensionDuringApplyProof suspends a migration while its Apply is still
// running. The claim, its Job and its Pod stay; the run is recorded as an
// unknown outcome held unresolved; nothing is dispatched while the resource
// reads Suspended; and resumed, it settles the unresolved run from a reading
// of the database without running the Apply again.
func (m *migrationRun) suspensionDuringApplyProof() {
	m.t.Helper()
	name, database := "e2e-suspend-"+m.engine.name, "ptah_e2e_suspend"
	secret := "e2e-" + m.engine.name + "-suspend-db"
	m.isolatedDatabase(database, secret)
	// The uncertain row's artifact again, for its slow third migration.
	// Always, because the moment this row needs is inside the run rather than
	// before it.
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference("-uncertain"),
		coordinationKey: "e2e/suspend/" + m.engine.name, apply: "Always", interval: "30s",
	}))
	job, jobUID := m.mfWaitForDispatchedApply(name)
	// How many of the migrations up to a version the revision table records
	// as applied. The slow third has a row while it runs, so the state is what
	// separates committed from started.
	applied := func(upTo int) string {
		return m.query("SELECT count(*) FROM schema_migrations WHERE version <= "+strconv.Itoa(upTo)+" AND state = 'applied'", database)
	}
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) && applied(2) != "2" {
		m.sleep(2 * time.Second)
	}
	if applied(2) != "2" {
		m.fatalf("the %s suspension run did not commit its first two migrations within %s", m.engine.name, waitTimeout)
	}

	m.logf("suspending the %s PtahMigration while its Apply is still running", m.engine.kind)
	m.patchMigration(name, map[string]any{"spec": map[string]any{"suspend": true}})
	for hold := time.Now().Add(20 * time.Second); time.Now().Before(hold); {
		m.assertSuspensionRetainsItsRunningApply(name, job, jobUID)
		m.sleep(5 * time.Second)
	}

	suspendedJobs := m.waitForSuspendedAfterApply(name, jobUID)
	// The run finished what it started, once, and suspension undid none of
	// it. The database says so even though the resource does not yet know.
	if applied(3) != "3" {
		m.fatalf("the %s suspended run did not end with its three migrations applied once", m.engine.name)
	}
	if m.query("SELECT count(*) FROM e2e_migration_widgets", database) != "3" {
		m.fatalf("the %s suspension changed the rows the run committed", m.engine.name)
	}

	// Nothing is dispatched once the resource reads Suspended. The interval
	// is thirty seconds, so ninety is three refreshes a suspension that did
	// not hold would have started, and the cleanup TTL is longer than the
	// window, so a Job created inside it is still there to count. Ready's
	// transition time cannot date the suspension: Ready was already False
	// while the Apply ran. So the window starts from the Jobs that existed
	// when Suspended was first read, and every poll also requires no claim,
	// which the controller writes before it creates a Job.
	quiet := m.migration(name)
	for window := time.Now().Add(90 * time.Second); time.Now().Before(window); {
		quiet = m.migration(name)
		if !suspendedQuiet(quiet, jobUID) {
			m.reportSuspendState(quiet)
			m.fatalf("%s claimed new work while it was suspended", name)
		}
		m.sleep(10 * time.Second)
	}
	if late := mfAddedUIDs(m.mfJobUIDs(name), suspendedJobs); len(late) != 0 {
		m.reportSuspendState(quiet)
		m.fatalf("%s created %d Job(s) after it read Suspended", name, len(late))
	}

	// Resumed, the resource reads the database, finds every migration the
	// artifact carries, and that reading is the one transition that removes
	// an unresolved record. The Apply is not run again to find out.
	m.patchMigration(name, map[string]any{"spec": map[string]any{"suspend": false}})
	settled := false
	var last *ptahv1alpha1.PtahMigration
	for settle := time.Now().Add(waitTimeout); time.Now().Before(settle); {
		last = m.migration(name)
		m.assertNoNewApplyJob([]string{jobUID}, "after it was resumed from a suspended Apply", name)
		if resumedSettled(last) {
			settled = true
			break
		}
		m.sleep(5 * time.Second)
	}
	if !settled {
		m.reportSuspendState(last)
		m.fatalf("%s did not settle its unresolved run from the database once resumed", name)
	}
	if applied(3) != "3" {
		m.fatalf("the %s migrations were not applied exactly once after %s settled", m.engine.name, name)
	}
	m.mfDeleteMigration(name)
	m.logf("PASS %s held a suspended Apply unresolved, ran nothing while suspended, and settled it from the database",
		m.engine.kind)
}

// assertSuspensionRetainsItsRunningApply holds the claim, its Job and its Pod
// to the ones dispatched before the suspension. Without the Pod still
// running, the retention proves nothing.
func (m *migrationRun) assertSuspensionRetainsItsRunningApply(name, job, jobUID string) {
	m.t.Helper()
	current := m.migration(name)
	if !suspensionRetainsClaim(current, jobUID) {
		m.reportSuspendState(current)
		m.fatalf("%s dropped the claim that accounts for its running Apply when it was suspended", name)
	}
	applyJob := &batchv1.Job{}
	if m.get(job, applyJob) != nil || string(applyJob.UID) != jobUID {
		m.fatalf("the %s Apply Job was removed or replaced when its resource was suspended", m.engine.name)
	}
	pods := &corev1.PodList{}
	if m.list(pods, client.MatchingLabels{"job-name": job}) != nil || !mfOnePodRunning(pods.Items) {
		m.fatalf("the %s Apply Pod stopped, so the retention above proved nothing", m.engine.name)
	}
}

// waitForSuspendedAfterApply waits for the resource to hold the dispatched
// Job's run unresolved and settle as Suspended, and returns the Jobs that
// existed on that reading: the ones the quiet window is measured against.
func (m *migrationRun) waitForSuspendedAfterApply(name, jobUID string) []string {
	m.t.Helper()
	var last *ptahv1alpha1.PtahMigration
	for deadline := time.Now().Add(waitTimeout); time.Now().Before(deadline); {
		last = m.migration(name)
		if suspendedAfterApply(last, jobUID) {
			return m.mfJobUIDs(name)
		}
		m.sleep(5 * time.Second)
	}
	m.reportSuspendState(last)
	m.fatalf("%s did not hold its Apply unresolved and settle as Suspended within %s", name, waitTimeout)
	return nil
}

// reportSuspendState prints the suspended migration's state and its Jobs.
func (m *migrationRun) reportSuspendState(migration *ptahv1alpha1.PtahMigration) {
	if migration == nil {
		return
	}
	lines := mfStateLines(migration)
	jobs := &batchv1.JobList{}
	if m.list(jobs, client.MatchingLabels{labelMigration: migration.Name}) == nil {
		for _, job := range jobs.Items {
			lines = append(lines, fmt.Sprintf("  job %s operation=%s created=%s",
				job.Name, mfOrNone(job.Labels[labelOperation]), instantOf(job.CreationTimestamp)))
		}
	}
	m.mfPrintState(migration.Name+" state when the check failed:", lines)
}

// releaseFault is the realm Lease the release row refuses to release, and the
// manager identity the refusal is scoped to.
type releaseFault struct {
	policy                    string
	leaseNamespace, lease     string
	holder                    string
	manager, managerUID       string
	managerPod, managerPodUID string
}

// lockReleaseFaultProof refuses the one write that hands a realm back -- the
// manager emptying the holder of the realm's Lease -- and holds the resource
// to owing the release instead of forgetting it, and to handing it back once
// the write is allowed (#242, PA-03). The fault is a ValidatingAdmissionPolicy
// scoped to this row's Lease and the manager's own service account. The
// Apply is claimed with its Pod held by the apply gate, so the claim has taken
// the Lease and no SQL has run yet; the fault goes in and is proven in force
// while nothing can finish, and only then is the gate opened.
func (m *migrationRun) lockReleaseFaultProof() {
	m.t.Helper()
	name, database := "e2e-release-fault-"+m.engine.name, "ptah_e2e_release_fault"
	secret := "e2e-" + m.engine.name + "-release-fault-db"
	fault := releaseFault{policy: "ptah-e2e-release-fault-" + m.engine.name}
	m.isolatedDatabase(database, secret)
	m.openApplyGate()
	// OnApproval, so the approval chooses when the Apply is claimed, and the
	// gate in the nodeSelector of every operation, so the claimed Apply's Pod
	// can be held while the fault is installed. An hour's interval keeps a
	// refresh from landing between the gate closing and the claim.
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference(""),
		coordinationKey: "e2e/release-fault/" + m.engine.name, apply: "OnApproval", interval: "1h",
		execution: map[string]any{
			"activeDeadlineSeconds": int64(300), "failureRetryInterval": "10s", "connectTimeout": "30s",
			"nodeSelector": map[string]any{applyGateLabel: "open"},
		},
	}))
	plan := ""
	m.releaseFaultPoll(name, &fault, 5*time.Second, "did not publish a plan to approve within "+waitTimeout.String(),
		func(current *ptahv1alpha1.PtahMigration) bool {
			published, ok := planPublishedForApproval(current)
			plan = published
			return ok
		})
	m.closeApplyGate()
	m.approveReleaseFaultPlan(name, plan)
	var job, jobUID, epoch string
	m.releaseFaultPoll(name, &fault, 2*time.Second, "did not claim an Apply under a realm Lease within "+waitTimeout.String(),
		func(current *ptahv1alpha1.PtahMigration) bool {
			var ok bool
			job, jobUID, epoch, ok = applyClaimUnderLease(current)
			return ok
		})
	m.waitForReleaseFaultPodToBeGated(name, &fault, job)
	m.findReleaseFaultLease(name, &fault, epoch)
	m.applyReleaseFault(&fault)
	m.waitForReleaseFaultInForce(name, &fault)
	m.logf("opening the gate on the %s Apply with its release refused", m.engine.kind)
	m.openApplyGate()

	m.releaseFaultPoll(name, &fault, 5*time.Second,
		"did not record its run and owe the refused release within "+waitTimeout.String(),
		func(current *ptahv1alpha1.PtahMigration) bool { return releaseOwed(current, jobUID, epoch) })
	m.closeApplyGate()
	jobs := m.mfJobUIDs(name)
	// Owed and held for a minute: the record stays, the Lease still names the
	// run's holder, and the resource claims no new work while it owes the
	// realm.
	for hold := time.Now().Add(60 * time.Second); time.Now().Before(hold); {
		current := m.migration(name)
		if !releaseStillOwed(current, epoch) {
			m.reportReleaseFaultState(current, &fault)
			m.fatalf("%s stopped owing the realm, or claimed new work, while its release was refused", name)
		}
		holder, err := m.releaseFaultHolder(&fault)
		m.check(err, "read the %s realm Lease while its release was refused", m.engine.name)
		if holder != fault.holder {
			m.reportReleaseFaultState(current, &fault)
			m.fatalf("the %s realm Lease changed holder while its release was refused", m.engine.name)
		}
		// Only an addition counts: the cleanup TTL may remove a finished Job.
		if len(mfAddedUIDs(m.mfJobUIDs(name), jobs)) != 0 {
			m.reportReleaseFaultState(current, &fault)
			m.fatalf("%s created a Job while it still owed the realm", name)
		}
		m.sleep(10 * time.Second)
	}

	m.logf("lifting the release fault on the %s realm Lease", m.engine.kind)
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	binding.Name = fault.policy
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	policy.Name = fault.policy
	for _, object := range []client.Object{binding, policy} {
		m.check(m.cluster.Client.Delete(m.ctx, object), "the release fault could not be removed")
	}
	m.releaseFaultPolicy = ""
	var last *ptahv1alpha1.PtahMigration
	for release := time.Now().Add(waitTimeout); time.Now().Before(release); {
		last = m.migration(name)
		if holder, err := m.releaseFaultHolder(&fault); err == nil && last.Status.PendingLockRelease == nil && holder == "" {
			m.mfDeleteMigration(name)
			m.logf("PASS %s kept owing a refused realm release and handed it back once it could", m.engine.kind)
			return
		}
		m.sleep(5 * time.Second)
	}
	m.reportReleaseFaultState(last, &fault)
	m.fatalf("%s did not hand the realm back once its release was allowed", name)
}

// releaseFaultPoll reads the release row's migration every interval, scanned,
// until match holds, and reports its state when the wait runs out.
func (m *migrationRun) releaseFaultPoll(name string, fault *releaseFault, interval time.Duration, failure string,
	match func(*ptahv1alpha1.PtahMigration) bool,
) {
	m.t.Helper()
	var last *ptahv1alpha1.PtahMigration
	for deadline := time.Now().Add(waitTimeout); time.Now().Before(deadline); {
		last = m.migration(name)
		if match(last) {
			return
		}
		m.sleep(interval)
	}
	m.reportReleaseFaultState(last, fault)
	m.fatalf("%s %s", name, failure)
}

// approveReleaseFaultPlan approves the published plan by the identities it
// and the migration carry.
func (m *migrationRun) approveReleaseFaultPlan(name, plan string) {
	m.t.Helper()
	migration := &ptahv1alpha1.PtahMigration{}
	published := &ptahv1alpha1.PtahMigrationPlan{}
	if m.get(name, migration) != nil || m.get(plan, published) != nil || migration.UID == "" || published.UID == "" ||
		published.Spec.Fingerprint == "" {
		m.fatalf("%s or its plan %s carries no identity to approve", name, plan)
	}
	if err := m.create(migrationApprovalDocument(m.in.TestNamespace, name+"-approval", name, string(migration.UID),
		plan, string(published.UID), published.Spec.Fingerprint)); err != nil {
		m.fatalf("the %s release-fault approval could not be created: %v", m.engine.name, err)
	}
}

// waitForReleaseFaultPodToBeGated waits until the claim's Pod exists and no
// node has taken it.
func (m *migrationRun) waitForReleaseFaultPodToBeGated(name string, fault *releaseFault, job string) {
	m.t.Helper()
	for deadline := time.Now().Add(waitTimeout); time.Now().Before(deadline); {
		pods := &corev1.PodList{}
		m.check(m.list(pods, client.MatchingLabels{"job-name": job}),
			"the %s Apply Pods could not be read while the gate was closed", m.engine.name)
		if gatedApplyPods(pods.Items) {
			return
		}
		if slices.ContainsFunc(pods.Items, func(pod corev1.Pod) bool { return pod.Spec.NodeName != "" }) {
			m.fatalf("the %s Apply Pod reached a node while the gate was closed, so the gate is not what held it", m.engine.name)
		}
		m.sleep(2 * time.Second)
	}
	m.reportReleaseFaultState(m.mfMigrationOrNil(name), fault)
	m.fatalf("the %s Apply never produced a Pod held off every node", m.engine.name)
}

// findReleaseFaultLease finds the Lease the claim took by the epoch the claim
// recorded rather than by a name worked out here, and the identity the
// manager writes it as: its ServiceAccount, and the running Pod its token is
// bound to.
func (m *migrationRun) findReleaseFaultLease(name string, fault *releaseFault, epoch string) {
	m.t.Helper()
	leases := &coordinationv1.LeaseList{}
	m.check(m.cluster.Client.List(m.ctx, leases), "the Leases could not be listed")
	namespace, lease, holder, err := leaseAtEpoch(leases.Items, epoch)
	if err != nil {
		m.fatalf("no single realm Lease carries the epoch %s claimed under: %v", name, err)
	}
	if holder == "" {
		m.fatalf("the realm Lease under %s's running Apply names no holder", name)
	}
	fault.leaseNamespace, fault.lease, fault.holder = namespace, lease, holder
	managers := &corev1.PodList{}
	m.check(m.cluster.Client.List(m.ctx, managers, client.InNamespace(namespace),
		client.MatchingLabels{"app.kubernetes.io/component": "controller"}), "the manager Pods could not be read beside its Leases")
	account, err := sharedManagerAccount(managers.Items)
	if err != nil {
		m.fatalf("%v", err)
	}
	fault.manager = "system:serviceaccount:" + namespace + ":" + account
	// The dry run asks with the identity a running manager has: its
	// ServiceAccount's UID and the name and UID of the Pod its token is bound
	// to.
	serviceAccount := &corev1.ServiceAccount{}
	if m.cluster.Client.Get(m.ctx, types.NamespacedName{Namespace: namespace, Name: account}, serviceAccount) == nil {
		fault.managerUID = string(serviceAccount.UID)
	}
	pod, podUID, ok := runningManagerPod(managers.Items)
	if fault.managerUID == "" || !ok {
		m.fatalf("no running manager Pod and ServiceAccount identity to impersonate")
	}
	fault.managerPod, fault.managerPodUID = pod, podUID
}

// applyReleaseFault installs the fault. The cleanup learns of it before the
// write, so a fault half installed is removed too.
func (m *migrationRun) applyReleaseFault(fault *releaseFault) {
	m.t.Helper()
	m.releaseFaultPolicy = fault.policy
	for _, document := range releaseFaultPolicyDocuments(fault.policy, fault.lease, fault.leaseNamespace, fault.manager) {
		if err := m.apply(document); err != nil {
			m.fatalf("the release fault could not be installed: %v", err)
		}
	}
}

// waitForReleaseFaultInForce waits for a server-side dry run of the release,
// written as the manager, to be refused. The dry run changes nothing if it is
// still let through, and it is an update, the verb the manager holds on
// Leases and the one a release uses. In force, the Apply has to be still
// claimed and its gated Pod still able to start inside the claim's dispatch
// window, or the row would measure a late dispatch instead.
func (m *migrationRun) waitForReleaseFaultInForce(name string, fault *releaseFault) {
	m.t.Helper()
	manager, err := m.cluster.As(rest.ImpersonationConfig{
		UserName: fault.manager, UID: fault.managerUID,
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + fault.leaseNamespace, "system:authenticated"},
		Extra: map[string][]string{
			"authentication.kubernetes.io/pod-name": {fault.managerPod},
			"authentication.kubernetes.io/pod-uid":  {fault.managerPodUID},
		},
	})
	m.check(err, "build a client that writes as the manager")
	refusal := ""
	for deadline := time.Now().Add(waitTimeout); time.Now().Before(deadline); {
		lease := &coordinationv1.Lease{}
		// A failed read is one more poll, not the end of the wait: the policy
		// takes a while to come into force, and the read is not what is measured.
		if err := m.cluster.Client.Get(m.ctx, types.NamespacedName{Namespace: fault.leaseNamespace, Name: fault.lease}, lease); err != nil {
			refusal = fmt.Sprintf("the %s realm Lease could not be read for the dry run: %v", m.engine.name, err)
			m.sleep(2 * time.Second)
			continue
		}
		empty := ""
		lease.Spec.HolderIdentity = &empty
		err := manager.Update(m.ctx, lease, client.DryRunAll)
		refusal = "the dry run was admitted"
		if err != nil {
			refusal = err.Error()
		}
		if err != nil && strings.Contains(refusal, "may not be released") {
			current := m.migration(name)
			active := current.Status.ActiveOperation
			if active == nil || active.Type != ptahv1alpha1.MigrationOperationApply {
				m.reportReleaseFaultState(current, fault)
				m.fatalf("the %s Apply ended before the release fault was in force, so the release was never refused", m.engine.name)
			}
			if active.DispatchNotAfter == nil {
				m.fatalf("the %s Apply claim carries no readable dispatch window", m.engine.name)
			}
			if left := int64(math.Floor(time.Until(active.DispatchNotAfter.Time).Seconds())); left <= 30 {
				m.reportReleaseFaultState(current, fault)
				m.fatalf("the release fault took until %ds before the dispatch deadline to come into force", left)
			}
			return
		}
		m.sleep(2 * time.Second)
	}
	m.mfPrintState("the last dry run of the release", []string{"  " + refusal})
	m.fatalf("the release fault never refused the manager's release of the %s realm Lease", m.engine.name)
}

// releaseFaultHolder is the Lease's holder now: empty once released, and
// empty too if the Lease is gone, which a release also allows. A read that
// failed for any other reason is an error rather than an empty holder, so a
// transient failure never reads as a release.
func (m *migrationRun) releaseFaultHolder(fault *releaseFault) (string, error) {
	lease := &coordinationv1.Lease{}
	err := m.cluster.Client.Get(m.ctx, types.NamespacedName{Namespace: fault.leaseNamespace, Name: fault.lease}, lease)
	switch {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", err
	case lease.Spec.HolderIdentity == nil:
		return "", nil
	}
	return *lease.Spec.HolderIdentity, nil
}

// reportReleaseFaultState prints the release row's state and the Lease's
// holder when a check failed.
func (m *migrationRun) reportReleaseFaultState(migration *ptahv1alpha1.PtahMigration, fault *releaseFault) {
	if migration == nil {
		return
	}
	holder, err := m.releaseFaultHolder(fault)
	if err != nil {
		holder = "unreadable: " + err.Error()
	}
	lines := append(mfStateLines(migration), fmt.Sprintf("  lease %s/%s holder=%s",
		fault.leaseNamespace, fault.lease, holder))
	m.mfPrintState(migration.Name+" state when the check failed:", lines)
}
