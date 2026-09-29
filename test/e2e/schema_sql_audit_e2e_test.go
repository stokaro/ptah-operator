//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The journal does not name a database on every MySQL record. A previously
// unused account makes all its traffic attributable even if a client switches
// databases. Credentials remain inside the fixture's server container.
func (a *databaseSQLAudit) createMySQLAccount(database string) string {
	a.t.Helper()
	if a.engine != "mysql" || a.started || !mysqlAuditIdentifier.MatchString(database) || len(database) > 64 {
		a.t.Fatal("MySQL audit account needs an isolated database before its journal starts")
	}
	var identity [8]byte
	if _, err := rand.Read(identity[:]); err != nil {
		a.t.Fatal("could not allocate an isolated MySQL audit account")
	}
	user := "audit_" + hex.EncodeToString(identity[:])
	a.exec("sh", "-ec", `credential=$(printf '%s' "$MYSQL_PASSWORD" | sed "s/'/''/g")
printf "SET SESSION sql_mode='NO_BACKSLASH_ESCAPES';\nCREATE USER '%s'@'%%' IDENTIFIED BY '%s';\nGRANT ALL PRIVILEGES ON %s.* TO '%s'@'%%';\n" "$1" "$credential" "$2" "$1" |
MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot >/dev/null`, "sh", user, database)
	return user
}

func (f *faultRun) schemaMySQLAuditAccount(audit *databaseSQLAudit, database, secretName string) string {
	f.t.Helper()
	user := audit.createMySQLAccount(database)
	secret := &corev1.Secret{}
	f.check(f.get(secretName, secret), "read the isolated schema target Secret")
	parsed, err := url.Parse(string(secret.Data["url"]))
	if err != nil || parsed.User == nil {
		f.fatalf("could not read the isolated schema URL")
	}
	password, ok := parsed.User.Password()
	if !ok || password == "" {
		f.fatalf("the schema target URL has no credential")
	}
	parsed.User = url.UserPassword(user, password)
	secret.Data["url"] = []byte(parsed.String())
	f.check(f.cluster.Client.Update(f.ctx, secret), "scope the schema target to its audit account")
	return user
}

type schemaSQLInventory struct {
	jobs map[types.UID]batchv1.Job
	pods map[types.UID]corev1.Pod
}

func (f *faultRun) captureSchemaSQLInventory(name string, inventory *schemaSQLInventory) {
	f.t.Helper()
	jobs, pods := &batchv1.JobList{}, &corev1.PodList{}
	f.check(f.list(jobs, client.MatchingLabels{labelSchema: name}), "read schema SQL audit Jobs")
	f.check(f.list(pods, client.MatchingLabels{labelSchema: name}), "read schema SQL audit Pods")
	for _, job := range jobs.Items {
		if job.UID == "" {
			f.fatalf("schema SQL audit Job has no UID")
		}
		inventory.jobs[job.UID] = job
	}
	for _, pod := range pods.Items {
		if pod.UID == "" {
			f.fatalf("schema SQL audit Pod has no UID")
		}
		inventory.pods[pod.UID] = pod
	}
}

func (inventory *schemaSQLInventory) clients(schema *ptahv1alpha1.PtahSchema, predecessors ...*ptahv1alpha1.PtahSchema) (map[string]operationSQLClient, error) {
	jobs, pods := make([]batchv1.Job, 0, len(inventory.jobs)), make([]corev1.Pod, 0, len(inventory.pods))
	for _, job := range inventory.jobs {
		jobs = append(jobs, job)
	}
	for _, pod := range inventory.pods {
		pods = append(pods, pod)
	}
	return schemaSQLClients(schema, jobs, pods, predecessors...)
}
