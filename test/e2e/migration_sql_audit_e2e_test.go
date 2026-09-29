//go:build e2e

package e2e

import (
	"bytes"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// Preserve terminal identities during polling: the controller's Job TTL may
// remove the initial History before the replacement plan is ready. SQL from
// a client never observed here fails attribution; it is not silently dropped.
type migrationSQLInventory struct {
	jobs map[types.UID]batchv1.Job
	pods map[types.UID]corev1.Pod
}

func (m *migrationRun) captureMigrationSQLInventory(name string, inventory *migrationSQLInventory) {
	m.t.Helper()
	jobs, pods := &batchv1.JobList{}, &corev1.PodList{}
	m.check(m.list(jobs, client.MatchingLabels{labelMigration: name}), "read SQL audit Jobs")
	m.check(m.list(pods, client.MatchingLabels{labelMigration: name}), "read SQL audit Pods")
	for _, job := range jobs.Items {
		if job.UID == "" {
			m.fatalf("SQL audit Job has no UID")
		}
		inventory.jobs[job.UID] = job
	}
	for _, pod := range pods.Items {
		if pod.UID == "" {
			m.fatalf("SQL audit Pod has no UID")
		}
		inventory.pods[pod.UID] = pod
	}
}

// assertPostgresMigrationRefusalSQL covers the initial History, admitted old
// approval and changed-input refusal with one uninterrupted server journal.
// before must precede creating the isolated migration so its initial History
// supplies a positive diagnostic control. snapshot supplies the flush marker.
func (m *migrationRun) assertPostgresMigrationRefusalSQL(audit *databaseSQLAudit, before []byte, database string, migration *ptahv1alpha1.PtahMigration, inventory *migrationSQLInventory) {
	m.t.Helper()
	if m.engine.name != "postgresql" || audit.engine != "postgresql" || len(before) == 0 || !bytes.HasPrefix(audit.pgPrefix, before) {
		m.fatalf("migration refusal SQL audit has no complete PostgreSQL journal window")
	}
	jobs, pods := make([]batchv1.Job, 0, len(inventory.jobs)), make([]corev1.Pod, 0, len(inventory.pods))
	for _, job := range inventory.jobs {
		jobs = append(jobs, job)
	}
	for _, pod := range inventory.pods {
		pods = append(pods, pod)
	}
	clients, err := migrationSQLClients(migration, jobs, pods)
	m.check(err, "bind SQL audit clients to the migration")
	counts, err := postgresMigrationRefusalSQL(audit.pgPrefix[len(before):], database, clients)
	m.check(err, "refuse every SQL statement outside the declared history diagnostics")
	hosts := make([]string, 0, len(counts))
	for host := range counts {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	for _, host := range hosts {
		if host == "127.0.0.1" {
			m.logf("SQL refusal audit: engine=postgresql migrationUID=%s actor=harness allowedReadRecords=%d", migration.UID, counts[host])
			continue
		}
		actor := clients[host]
		m.logf("SQL refusal audit: engine=postgresql migrationUID=%s jobUID=%s podUID=%s client=%s allowedHistoryRecords=%d unauthorizedRecords=0",
			migration.UID, actor.jobUID, actor.podUID, host, counts[host])
	}
}
