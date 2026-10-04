//go:build e2e

package e2e

import (
	"bytes"
	"slices"
	"time"

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
	clients, err := inventory.clients(migration)
	m.check(err, "bind SQL audit clients to the migration")
	counts, err := inventory.postgresRefusalSQL(audit.pgPrefix[len(before):], database, clients, "")
	m.check(err, "refuse every SQL statement outside the declared history diagnostics")
	m.reportMigrationRefusalSQL(migration, clients, counts)
}

func (inventory *migrationSQLInventory) clients(migration *ptahv1alpha1.PtahMigration, predecessors ...*ptahv1alpha1.PtahMigration) (map[string]operationSQLClient, error) {
	jobs, pods := make([]batchv1.Job, 0, len(inventory.jobs)), make([]corev1.Pod, 0, len(inventory.pods))
	for _, job := range inventory.jobs {
		jobs = append(jobs, job)
	}
	for _, pod := range inventory.pods {
		pods = append(pods, pod)
	}
	return migrationSQLClients(migration, jobs, pods, predecessors...)
}

func (inventory *migrationSQLInventory) postgresRefusalSQL(raw []byte, database string, clients map[string]operationSQLClient, refusedJobUID string) (map[string]int, error) {
	lifetimes, err := operationSQLLifetimes(clients, inventory.pods)
	if err != nil {
		return nil, err
	}
	return postgresMigrationRefusalSQLForJob(raw, database, clients, refusedJobUID, lifetimes)
}

func (m *migrationRun) assertMySQLMigrationRefusalSQL(audit *databaseSQLAudit, before []mysqlStatementRecord, database, user string, migration *ptahv1alpha1.PtahMigration, inventory *migrationSQLInventory) {
	m.t.Helper()
	clients, err := inventory.clients(migration)
	m.check(err, "bind MySQL SQL audit clients to the migration")
	counts, err := mysqlMigrationRefusalSQL(before, audit.mysqlStatementSnapshot(), database, user, clients)
	m.check(err, "refuse every MySQL statement outside the declared history diagnostics")
	m.reportMigrationRefusalSQL(migration, clients, counts)
}

func (m *migrationRun) assertRestoredHistoryRefusalSQL(audit *databaseSQLAudit, pgBefore []byte, mysqlBefore []mysqlStatementRecord, database, user, jobUID string, migration *ptahv1alpha1.PtahMigration, inventory *migrationSQLInventory) {
	m.t.Helper()
	if jobUID == "" || migration.Spec.Policy.LockTimeout.Duration != 30*time.Second {
		m.fatalf("restored-history audit needs the exact refused Job and declared lock timeout")
	}
	clients, err := inventory.clients(migration)
	m.check(err, "bind restored-history SQL to its migration Jobs and Pods")
	var counts map[string]int
	if m.engine.name == "postgresql" {
		if len(pgBefore) == 0 || !bytes.HasPrefix(audit.pgPrefix, pgBefore) {
			m.fatalf("restored-history audit has no complete PostgreSQL journal window")
		}
		counts, err = inventory.postgresRefusalSQL(audit.pgPrefix[len(pgBefore):], database, clients, jobUID)
	} else {
		counts, err = mysqlMigrationRefusalSQLForJob(mysqlBefore, audit.mysqlStatementSnapshot(), database, user, clients, jobUID)
	}
	m.check(err, "refuse unauthorized SQL after history changed")
	m.reportMigrationRefusalSQL(migration, clients, counts)
}

func (m *migrationRun) reportMigrationRefusalSQL(migration *ptahv1alpha1.PtahMigration, clients map[string]operationSQLClient, counts map[string]int) {
	m.t.Helper()
	hosts := make([]string, 0, len(counts))
	for host := range counts {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	for _, host := range hosts {
		if host == "127.0.0.1" {
			m.logf("SQL refusal audit: engine=%s migrationUID=%s actor=harness allowedReadRecords=%d", m.engine.name, migration.UID, counts[host])
			continue
		}
		actor := clients[host]
		m.logf("SQL refusal audit: engine=%s migrationUID=%s jobUID=%s podUID=%s operation=%s client=%s allowedDiagnosticRecords=%d unauthorizedRecords=0",
			m.engine.name, actor.resourceUID, actor.jobUID, actor.podUID, actor.operation, host, counts[host])
	}
}

// A dedicated account scopes MySQL's database-less journal even if a client
// changes its selected database. Create it before logging, never reuse it, and
// keep its grants until the fixture cluster is removed: the migration can still
// reconcile after this scenario. The password stays inside the server container.
func (m *migrationRun) isolatedMySQLAuditDatabase(database, secret string) string {
	m.t.Helper()
	if m.engine.name != "mysql" || !mysqlAuditIdentifier.MatchString(database) || len(database) > 64 {
		m.fatalf("MySQL approval audit requires a controlled database name")
	}
	if m.databaseExists(database) != "0" {
		m.fatalf("MySQL approval audit database already exists; rerun through the phase cleanup")
	}
	if _, err := m.serverStatement("CREATE DATABASE " + database); err != nil {
		m.fatalf("could not create the isolated MySQL audit database")
	}
	audit := &databaseSQLAudit{t: m.t, ctx: m.ctx, cluster: m.cluster, namespace: m.in.TestNamespace, engine: "mysql"}
	user := audit.createMySQLAccount(database)
	url := m.databaseURL(database, user)
	m.protect(url)
	m.check(m.apply(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "immutable": true, "type": "Opaque",
		"metadata": map[string]any{"namespace": m.in.TestNamespace, "name": secret},
		"data": secretData(map[string]string{
			"username": user, "password": m.password, "database": database, "url": url,
		}),
	}), "apply isolated MySQL audit Secret")
	return user
}
