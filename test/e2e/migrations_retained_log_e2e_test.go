//go:build e2e

package e2e

import (
	"context"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// stoppedRunsKeepTheirLogs holds a run under `apply: Always` that stops its resource to
// the 24 hours its Pod log is for: one that committed half of a migration, and one the
// database refused at a statement. Each has its own database and artifact, and each is
// deleted once it has shown what it is for, so nothing later waits on the stop.
//
// The Job is read rather than its TTL inferred, because the webhook, the guard and the
// runner's own summary all stand between the manager's request and this value, and
// only a cluster has all of them.
func (m *migrationRun) stoppedRunsKeepTheirLogs() {
	m.t.Helper()
	m.stoppedRunKeepsItsLog("-partial", ptahv1alpha1.MigrationRunOutcomePartial)
	m.stoppedRunKeepsItsLog("-refused", ptahv1alpha1.MigrationRunOutcomeFailed)
}

func (m *migrationRun) stoppedRunKeepsItsLog(fixture string, outcome ptahv1alpha1.MigrationRunOutcome) {
	m.t.Helper()
	name := "e2e-retained-" + m.engine.name + fixture
	secret := "e2e-" + m.engine.name + "-retained" + fixture + "-db"
	database := "ptah_e2e_retained" + strings.ReplaceAll(fixture, "-", "_")
	reference := m.reference("-retained" + fixture)
	m.isolatedDatabase(database, secret)
	m.publish("retained"+fixture, m.fixtureDir(fixture), reference)
	// Always, because the row is about a run nobody paces. The interval is short so
	// the reading a Partial run waits for, and the one the hold below waits for, come
	// inside the row.
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: reference,
		coordinationKey: "e2e/retained" + fixture + "/" + m.engine.name, apply: "Always", interval: "30s",
	}))

	var stopped, last *ptahv1alpha1.PtahMigration
	if !m.within(2*time.Second, func() bool {
		last = m.migration(name)
		if stoppedRunRead(last.Status, outcome) {
			stopped = last
			return true
		}
		return false
	}) {
		m.mfPrintLastRun(name, last)
		m.fatalf("%s did not stop on a %s run within %s", name, outcome, waitTimeout)
	}
	run := stopped.Status.LastRun

	job := &batchv1.Job{}
	m.check(m.get(run.JobName, job), "read the Job of the %s run", outcome)
	if job.UID != run.JobUID {
		m.fatalf("the Job under %s is not the one the %s run names", run.JobName, outcome)
	}
	m.check(failedApplyJobRetained(job), "the %s %s run under Always", m.engine.name, outcome)
	m.check(failedApplyPointerSays(run.Message, m.in.TestNamespace, run.JobName), "status.lastRun.message of %s", name)

	// The 24 hours are for this: what the database answered is there to read, by the
	// command the run's record prints. This row runs on both engines, so it is where
	// the command meets MySQL. One Pod, so kubectl's choice among a Job's Pods is no
	// choice here.
	m.readStopRowPod(string(run.JobUID))
	logs, err := readPrintedLog(run.Message, func(arguments ...string) ([]byte, []byte, error) {
		ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
		defer cancel()
		return m.cluster.Kubectl(ctx, arguments...)
	})
	m.check(err, "run the command the %s run prints", outcome)
	m.scan(logs, "the "+name+" Apply's log")
	m.check(failedApplyLogReports(logs), "the log of the %s run", outcome)

	// The rule rests on this. A read after the one that matched is what an Apply would
	// have followed, dated by the resource's own timestamp, so nothing that holds the
	// long TTL can come around again by itself.
	if !m.within(2*time.Second, func() bool {
		last = m.migration(name)
		m.check(onlyTheRunDispatched(m.applyJobUIDs(name), string(run.JobUID)), "%s after its %s run", name, outcome)
		return readAgainSince(last.Status, stopped.Status.History.ObservedAt)
	}) {
		m.mfPrintLastRun(name, last)
		m.fatalf("%s did not read the database again within %s", name, waitTimeout)
	}
	if !blockedRefusalHeld(last.Status) {
		m.mfPrintLastRun(name, last)
		m.fatalf("%s stopped refusing after its %s run", name, outcome)
	}
	m.mfDeleteMigration(name)
	m.logf("PASS %s %s run under Always stopped its resource and kept its log", m.engine.kind, outcome)
}
