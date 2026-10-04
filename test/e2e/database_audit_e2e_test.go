//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

type databaseSQLAudit struct {
	t         *testing.T
	ctx       context.Context
	cluster   *harness.Cluster
	namespace string
	engine    string
	pgPrefix  []byte
	started   bool
	// External lifecycle databases use Docker exec; data-plane databases use
	// the cluster's Deployment. Both retain the same journal checks.
	serverExec func(context.Context, ...string) ([]byte, error)
}

// Record only the controlled approval window; a multi-hour lifecycle does
// not need to keep every unrelated introspection query in its temporary DB.
func (a *databaseSQLAudit) start() {
	a.t.Helper()
	if a.started {
		return
	}
	a.started = true
	a.t.Cleanup(a.close)
	if a.engine == "mysql" {
		a.exec("sh", "-ec", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -e "SET GLOBAL general_log=ON"`)
	} else {
		a.exec("sh", "-ec", `PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d postgres -v ON_ERROR_STOP=1 -Atq -c "ALTER SYSTEM SET log_statement='all'" -c "SELECT pg_reload_conf()"`)
	}
}

func (a *databaseSQLAudit) close() {
	if !a.started {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := `PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d postgres -v ON_ERROR_STOP=1 -Atq -c "ALTER SYSTEM RESET log_statement" -c "SELECT pg_reload_conf()"`
	if a.engine == "mysql" {
		command = `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -e "SET GLOBAL general_log=OFF"`
	}
	if _, err := a.execute(ctx, "sh", "-ec", command); err != nil {
		a.t.Errorf("could not stop %s SQL audit: %v", a.engine, err)
		return
	}
	a.started, a.pgPrefix = false, nil
}

func (a *databaseSQLAudit) exec(command ...string) []byte {
	a.t.Helper()
	stdout, err := a.execute(a.ctx, command...)
	if err != nil {
		// The SQL journal can carry credentials. Do not include command output.
		a.t.Fatalf("%s SQL audit could not read its server: %v", a.engine, err)
	}
	return stdout
}

func (a *databaseSQLAudit) execute(ctx context.Context, command ...string) ([]byte, error) {
	if a.serverExec != nil {
		return a.serverExec(ctx, command...)
	}
	service := pgService
	if a.engine == "mysql" {
		service = mysqlService
	}
	stdout, _, err := a.cluster.Kubectl(ctx, append([]string{"-n", a.namespace, "exec", "deployment/" + service, "--"}, command...)...)
	return stdout, err
}

func (a *databaseSQLAudit) snapshot() sqlAuditCounts {
	a.t.Helper()
	a.start()
	if a.engine == "mysql" {
		raw := a.exec("sh", "-ec", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -NB -e "$1"`, "sh",
			`SELECT IF(@@general_log=1 AND FIND_IN_SET('TABLE',@@log_output)>0,1,0);
SELECT JSON_OBJECT('client',user_host,'count',COUNT(*)) FROM mysql.general_log WHERE command_type IN ('Query','Execute') GROUP BY user_host`)
		if !bytes.HasPrefix(raw, []byte("1\n")) {
			a.t.Fatal("MySQL SQL audit logging is not enabled")
		}
		counts, err := mysqlAuditCounts(raw[2:])
		if err != nil {
			a.t.Fatal(err)
		}
		return counts
	}
	if a.engine != "postgresql" {
		a.t.Fatalf("unsupported SQL audit engine %s", a.engine)
	}
	marker := fmt.Sprintf("ptah_e2e_audit_%d", time.Now().UnixNano())
	var raw []byte
	ready := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		raw = a.exec("sh", "-ec", `PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d postgres -Atq -c "$1" -c "$2"`, "sh",
			`SELECT current_setting('logging_collector'),current_setting('log_destination'),current_setting('log_statement'),current_setting('log_min_error_statement'),current_setting('log_rotation_age'),current_setting('log_rotation_size'),current_setting('log_timezone')`,
			"SELECT '"+marker+"'")
		if string(raw) == "on|jsonlog|all|error|0|0|UTC\n"+marker+"\n" {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		a.t.Fatal("PostgreSQL SQL audit logging was not enabled before its marker")
	}
	// A statement logged after the completed Job is a collector flush barrier.
	// Wait for it, rather than treating a temporarily empty log as a refusal.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		raw = a.exec("cat", "/tmp/ptah-sql-audit/statements.json")
		if err := postgresAuditPrefixError(a.pgPrefix, raw); err != nil {
			a.t.Fatal(err)
		}
		counts, found, err := postgresAuditCounts(raw, marker)
		if err == nil && found {
			a.pgPrefix = raw
			return counts
		}
		select {
		case <-a.ctx.Done():
			a.t.Fatal("PostgreSQL SQL audit canceled before the collector reached its marker")
		case <-time.After(100 * time.Millisecond):
		}
	}
	a.t.Fatal("PostgreSQL SQL audit did not produce a complete journal through its marker")
	return sqlAuditCounts{}
}

// MySQL's general log records Prepare and Execute separately. Preserve both,
// including exact bound argument bytes, so counts cannot hide unauthorized SQL.
func (a *databaseSQLAudit) mysqlStatementSnapshot() []mysqlStatementRecord {
	a.t.Helper()
	if a.engine != "mysql" {
		a.t.Fatal("MySQL statement snapshot requested for another engine")
	}
	a.start()
	raw := a.exec("sh", "-ec", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -NBr -e "$1"`, "sh",
		`SELECT IF(@@general_log=1 AND FIND_IN_SET('TABLE',@@log_output)>0,1,0);
SELECT JSON_OBJECT('time',DATE_FORMAT(event_time,'%Y-%m-%dT%H:%i:%s.%f'),'thread',thread_id,'client',user_host,'type',command_type,'argumentHex',HEX(argument))
FROM mysql.general_log ORDER BY event_time,thread_id`)
	if !bytes.HasPrefix(raw, []byte("1\n")) {
		a.t.Fatal("MySQL statement journal logging is not enabled")
	}
	records, err := mysqlStatementJournal(raw[2:])
	if err != nil {
		a.t.Fatal(err)
	}
	return records
}

func (a *databaseSQLAudit) assertRecords(before, after sqlAuditCounts, pod *corev1.Pod, positive bool) {
	a.t.Helper()
	if pod == nil || pod.Namespace != a.namespace || pod.UID == "" || pod.Status.Phase != corev1.PodSucceeded {
		a.t.Fatal("SQL audit needs the exact completed runner Pod")
	}
	if _, err := netip.ParseAddr(pod.Status.PodIP); err != nil {
		a.t.Fatal("SQL audit runner Pod has no numeric IP")
	}
	jobUID := ""
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Job" && owner.Controller != nil && *owner.Controller {
			jobUID = string(owner.UID)
		}
	}
	if jobUID == "" {
		a.t.Fatal("SQL audit runner Pod has no controlling Job UID")
	}
	count, err := sqlAuditDelta(before, after, pod.Status.PodIP)
	if err != nil {
		a.t.Fatal(err)
	}
	if (positive && count == 0) || (!positive && count != 0) {
		a.t.Fatalf("%s server received %d SQL records from Job %s Pod %s; positive control=%t",
			a.engine, count, jobUID, pod.UID, positive)
	}
	a.t.Logf("SQL audit: engine=%s jobUID=%s podUID=%s client=%s receivedRecords=%d positiveControl=%t serverRecords=%d",
		a.engine, jobUID, pod.UID, pod.Status.PodIP, count, positive, after.records)
}

// terminalPod reads a Pod still retained by the Job TTL. An empty result or
// several attempts cannot prove which client the database audit measured.
func (a *databaseSQLAudit) terminalPod(labels map[string]string, jobUID string) *corev1.Pod {
	a.t.Helper()
	pods := &corev1.PodList{}
	if err := a.cluster.Client.List(a.ctx, pods, client.InNamespace(a.namespace), client.MatchingLabels(labels)); err != nil {
		a.t.Fatalf("SQL audit could not list runner Pods: %v", err)
	}
	if len(pods.Items) != 1 {
		a.t.Fatalf("SQL audit found %d runner Pods, expected exactly one", len(pods.Items))
	}
	pod := &pods.Items[0]
	if jobUID != "" && !podControlledByJobUID(pod.OwnerReferences, types.UID(jobUID)) {
		a.t.Fatal("SQL audit Pod belongs to another Job")
	}
	if strings.TrimSpace(pod.Labels[labelOperation]) == "" {
		a.t.Fatal("SQL audit Pod has no operation label")
	}
	return pod
}
