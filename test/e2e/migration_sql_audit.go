package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

type operationSQLClient struct {
	jobUID, podUID, operation string
	resourceUID               string
}

// A replacement proof may retain the terminal workloads of explicitly named
// predecessors. Names alone never authorize attribution to another UID.
func migrationSQLClients(migration *ptahv1alpha1.PtahMigration, jobs []batchv1.Job, pods []corev1.Pod, predecessors ...*ptahv1alpha1.PtahMigration) (map[string]operationSQLClient, error) {
	if migration == nil || migration.UID == "" || migration.Name == "" || migration.Namespace == "" {
		return nil, errors.New("SQL audit has no migration identity")
	}
	identities := map[string]bool{string(migration.UID): true}
	for _, previous := range predecessors {
		if previous == nil || previous.UID == "" || previous.Name != migration.Name || previous.Namespace != migration.Namespace || identities[string(previous.UID)] {
			return nil, errors.New("SQL audit needs distinct same-name replacement identities")
		}
		identities[string(previous.UID)] = true
	}
	return operationSQLClientsForIdentities(migration.Namespace, migration.Name, "PtahMigration", labelMigration, identities, jobs, pods)
}

func operationSQLClientsForIdentities(namespace, name, kind, resourceLabel string, identities map[string]bool, jobs []batchv1.Job, pods []corev1.Pod) (map[string]operationSQLClient, error) {
	clients := make(map[string]operationSQLClient)
	for _, pod := range pods {
		if pod.Namespace != namespace || pod.Labels[resourceLabel] != name || pod.UID == "" {
			return nil, errors.New("SQL audit Pod belongs to another resource")
		}
		if pod.Status.PodIP == "" {
			continue // No address to attribute; any received SQL still needs a client below.
		}
		address, err := netip.ParseAddr(pod.Status.PodIP)
		if err != nil || pod.Status.Phase != corev1.PodSucceeded {
			return nil, errors.New("SQL audit needs terminal Pods with numeric addresses")
		}
		var owner *batchv1.Job
		for i := range jobs {
			job := &jobs[i]
			if job.UID != "" && ownedExactlyOnce(pod.OwnerReferences, "batch/v1", "Job", job.Name, job.UID) {
				if owner != nil {
					return nil, errors.New("SQL audit Pod has ambiguous Job ownership")
				}
				owner = job
			}
		}
		resourceUID := ""
		if owner != nil {
			for uid := range identities {
				if ownedExactlyOnce(owner.OwnerReferences, ptahSchemaAPIVersion, kind, name, types.UID(uid)) {
					if resourceUID != "" {
						return nil, errors.New("SQL audit Job has ambiguous resource ownership")
					}
					resourceUID = uid
				}
			}
		}
		if owner == nil || owner.Namespace != namespace || owner.Labels[resourceLabel] != name || resourceUID == "" ||
			owner.Labels[labelOperation] == "" || owner.Labels[labelOperation] != pod.Labels[labelOperation] {
			return nil, errors.New("SQL audit cannot bind the Pod and Job to the exact resource")
		}
		host := address.Unmap().String()
		if _, exists := clients[host]; exists {
			return nil, errors.New("SQL audit cannot distinguish Pods that shared an address")
		}
		clients[host] = operationSQLClient{jobUID: string(owner.UID), podUID: string(pod.UID), operation: owner.Labels[labelOperation], resourceUID: resourceUID}
	}
	if len(clients) == 0 {
		return nil, errors.New("SQL audit found no identified operation clients")
	}
	return clients, nil
}

// postgresMigrationRefusalSQL checks every received statement for the isolated
// database, including failed and rolled-back statements. Unknown clients and
// SQL from an operation other than History fail even if the database is equal.
// The caller supplies the complete append-only journal window, bounded before
// approval and after the replacement plan reaches its approval gate.
func postgresMigrationRefusalSQL(raw []byte, database string, clients map[string]operationSQLClient) (map[string]int, error) {
	return postgresMigrationRefusalSQLForJob(raw, database, clients, "")
}

func postgresMigrationRefusalSQLForJob(raw []byte, database string, clients map[string]operationSQLClient, refusedApplyJobUID string) (map[string]int, error) {
	refusal := migrationRefusalSQL{applyJobUID: refusedApplyJobUID}
	counts, err := postgresStatementRefusalSQL(raw, database, clients, refusal.acceptsActor, refusal.postgres,
		func(statement, parameters string) bool {
			return postgresMigrationHarnessRead(statement, parameters) ||
				(refusedApplyJobUID != "" && postgresMigrationRestoreHarnessRead(statement, parameters))
		})
	if err != nil {
		return nil, err
	}
	if !refusal.complete() {
		return nil, errors.New("PostgreSQL refusal did not record the exact Apply's lock, unresolved-history read and unlock")
	}
	return counts, nil
}

func postgresStatementRefusalSQL(raw []byte, database string, clients map[string]operationSQLClient,
	acceptsActor func(operationSQLClient) bool,
	acceptsSQL func(operationSQLClient, string, string) bool,
	harnessRead func(string, string) bool,
) (map[string]int, error) {
	if database == "" || len(clients) == 0 {
		return nil, errors.New("SQL audit needs an isolated database and identified clients")
	}
	counts := make(map[string]int)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	records := 0
	for {
		var row struct {
			Host      string `json:"remote_host"`
			Database  string `json:"dbname"`
			Message   string `json:"message"`
			Statement string `json:"statement"`
			Detail    string `json:"detail"`
		}
		if err := decoder.Decode(&row); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, errors.New("invalid or incomplete PostgreSQL statement journal")
		}
		statement := row.Statement
		messageSQL := ""
		if strings.HasPrefix(row.Message, "statement: ") {
			messageSQL = strings.TrimPrefix(row.Message, "statement: ")
			if messageSQL == "" {
				return nil, errors.New("PostgreSQL statement record lost its SQL")
			}
		} else if strings.HasPrefix(row.Message, "execute ") {
			_, messageSQL, _ = strings.Cut(row.Message, ": ")
			if messageSQL == "" {
				return nil, errors.New("PostgreSQL execution record lost its statement")
			}
		}
		if messageSQL != "" {
			if statement != "" && statement != messageSQL {
				return nil, errors.New("PostgreSQL execution and error statements disagree")
			}
			statement = messageSQL
		}
		if statement == "" {
			continue
		}
		records++
		if row.Database == "" {
			return nil, errors.New("PostgreSQL statement has no database identity")
		}
		if row.Database != database {
			if address, err := netip.ParseAddr(row.Host); err == nil {
				if _, identified := clients[address.Unmap().String()]; identified {
					return nil, errors.New("identified SQL audit client reached an undeclared database")
				}
			}
			continue
		}
		address, err := netip.ParseAddr(row.Host)
		if err != nil {
			return nil, errors.New("isolated-database SQL has no numeric client address")
		}
		host := address.Unmap().String()
		if host == "127.0.0.1" && harnessRead != nil && harnessRead(statement, row.Detail) {
			counts[host]++
			continue
		}
		client, found := clients[host]
		if !found || !acceptsActor(client) {
			return nil, fmt.Errorf("isolated-database SQL record %d has no authorized diagnostic Job and Pod", records)
		}
		if !acceptsSQL(client, statement, row.Detail) {
			// Do not include SQL or parameters: either can contain credentials.
			return nil, fmt.Errorf("isolated-database SQL record %d is outside the permitted diagnostics", records)
		}
		counts[host]++
	}
	if records == 0 {
		return nil, errors.New("PostgreSQL statement journal has no received SQL")
	}
	operatorRecords := 0
	for host, count := range counts {
		if host != "127.0.0.1" {
			operatorRecords += count
		}
	}
	if operatorRecords == 0 {
		return nil, errors.New("PostgreSQL refusal window did not observe its diagnostic control")
	}
	return counts, nil
}

// The harness checks the unchanged database from inside the server container.
// Its exact read-only queries are permitted only on loopback. They cannot
// authorize a runner's SQL or excuse other local statements.
func postgresMigrationHarnessRead(statement, parameters string) bool {
	if parameters != "" {
		return false
	}
	switch statement {
	case "SELECT count(*) FROM schema_migrations",
		"SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='schema_migrations'",
		"SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='e2e_migration_widgets'":
		return true
	}
	return false
}

// This is the exact default-table History contract used by these fixtures at
// the pinned Ptah source, not a SQL classifier. In particular SELECT, WITH,
// comments, additional statements and changed parameters receive no exception.
// Ptah's History may create its empty revision table; it may not change rows.
// See testdata/e2e/readings/postgresql-migration-history-audit.md for sources.
func postgresMigrationHistoryStatement(statement, parameters string) bool {
	if parameters == "" {
		switch statement {
		case "-- ping", "SELECT version()", "SELECT current_schema()",
			"SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')",
			postgresHistoryCreate, postgresHistoryRead:
			return true
		}
	}
	const tableParameters = "Parameters: $1 = '', $2 = 'schema_migrations'"
	if parameters == tableParameters && (statement == postgresHistoryOwner || statement == postgresHistoryVersionType) {
		return true
	}
	if statement == postgresHistoryColumn {
		for _, column := range []string{"state", "applied", "total", "error", "error_stmt", "execution_time_ms", "checksum"} {
			if parameters == tableParameters+", $3 = '"+column+"'" {
				return true
			}
		}
	}
	return false
}

const postgresHistoryOwner = `SELECT
  pg_catalog.pg_get_userbyid(c.relowner),
  current_user,
  pg_catalog.pg_get_userbyid(c.relowner) = current_user
FROM pg_catalog.pg_class AS c
JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
WHERE n.nspname = COALESCE(NULLIF($1, ''), pg_catalog.current_schema())
  AND c.relname = $2`

const postgresHistoryVersionType = `
SELECT data_type
FROM information_schema.columns
WHERE table_schema = COALESCE(NULLIF($1, ''), current_schema())
  AND table_name = $2 AND column_name = 'version'`

const postgresHistoryColumn = `
SELECT COUNT(*)
FROM information_schema.columns
WHERE table_schema = COALESCE(NULLIF($1, ''), current_schema())
  AND table_name = $2 AND column_name = $3`

const postgresHistoryCreate = `CREATE TABLE IF NOT EXISTS "schema_migrations" (
    version BIGINT PRIMARY KEY,
    description TEXT NOT NULL,
    applied_at TIMESTAMP NOT NULL,
    state VARCHAR(32) NOT NULL DEFAULT 'applied',
    applied INTEGER NOT NULL DEFAULT 1,
    total INTEGER NOT NULL DEFAULT 1,
    error TEXT NULL,
    error_stmt TEXT NULL,
    execution_time_ms BIGINT NOT NULL DEFAULT 0,
    checksum VARCHAR(64) NOT NULL DEFAULT ''
)`

const postgresHistoryRead = `SELECT version, description, state, applied, total, COALESCE(error, ''), COALESCE(error_stmt, ''), execution_time_ms, checksum, applied_at
FROM "schema_migrations"
ORDER BY version`
