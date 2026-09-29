//go:build e2e

package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

type schemaRefusalWindow struct {
	f                    *faultRun
	name, database, user string
	audit                *databaseSQLAudit
	policy               *schemaSQLPolicy
	inventory            *schemaSQLInventory
	pgBefore             []byte
	mysqlBefore          []mysqlStatementRecord
}

// Start after fixture setup but before its resource exists. The dedicated
// MySQL credential has never connected, so database-less records remain scoped.
// Database assertions must run after assert closes this uninterrupted window.
func (f *faultRun) startSchemaRefusalWindow(name, engine, database, secret string) *schemaRefusalWindow {
	f.t.Helper()
	if !apierrors.IsNotFound(f.get(name, &ptahv1alpha1.PtahSchema{})) {
		f.fatalf("schema SQL refusal audit must start before its resource exists")
	}
	policy, err := newSchemaSQLPolicy(engine, database)
	f.check(err, "load the pinned schema diagnostic SQL contract")
	w := &schemaRefusalWindow{f: f, name: name, database: database, policy: policy,
		audit:     &databaseSQLAudit{t: f.t, ctx: f.ctx, cluster: f.cluster, namespace: f.in.TestNamespace, engine: engine},
		inventory: &schemaSQLInventory{jobs: map[types.UID]batchv1.Job{}, pods: map[types.UID]corev1.Pod{}},
	}
	if engine == "mysql" {
		w.user = f.schemaMySQLAuditAccount(w.audit, database, secret)
		w.mysqlBefore = w.audit.mysqlStatementSnapshot()
	} else {
		w.audit.snapshot()
		w.pgBefore = w.audit.pgPrefix
	}
	return w
}

func (w *schemaRefusalWindow) waitForSchema(description string, match func(*ptahv1alpha1.PtahSchema) bool) *ptahv1alpha1.PtahSchema {
	w.f.t.Helper()
	return w.f.waitForSchema(w.name, description, func(resource *ptahv1alpha1.PtahSchema) bool {
		w.f.captureSchemaSQLInventory(w.name, w.inventory)
		return match(resource)
	})
}

// Read the diagnostic's actual runner result before its Job TTL can remove it.
// A control carries the exact Job and Pod whose SQL the closing audit requires.
func (w *schemaRefusalWindow) resultControl(resource *ptahv1alpha1.PtahSchema, operation string, before checkpoint) operationSQLClient {
	w.f.t.Helper()
	f := w.f
	previous := f.captured
	defer func() { f.captured = previous }()
	if resource == nil || resource.Name != w.name || resource.Namespace != f.in.TestNamespace || resource.UID == "" {
		f.fatalf("schema SQL result control has no exact resource identity")
	}
	result := f.captureOneNewJobResult(w.name, operation, before, nil)
	dialect := "postgres"
	if w.audit.engine == "mysql" {
		dialect = "mysql"
	}
	switch operation {
	case "observe":
		f.check(observedDriftBound(result, resource.Status.Target, dialect), "bind the SQL control to its observed state")
	case "plan":
		if !exactControllerPlan(resource.Status.Plan, resource.Status.ExecutionBinding, f.controller, f.stateVersion()) {
			f.fatalf("schema SQL plan control lost its controller binding")
		}
		f.check(committedPlan(f.schemaPlan(resource.Status.Plan.Name), w.name, resource.Status.Source.Digest, dialect,
			resource.Status.Plan.Destructive, f.controller, f.stateVersion()), "read the published SQL control plan")
		f.check(changedPlanBound(result, resource.Status.Plan), "bind the SQL control to its published plan")
	default:
		f.fatalf("unsupported schema SQL diagnostic control %s", operation)
	}
	w.f.captureSchemaSQLInventory(w.name, w.inventory)
	return operationSQLClient{resourceUID: string(resource.UID), jobUID: f.captured.jobUID, podUID: f.captured.podUID, operation: operation}
}

func (w *schemaRefusalWindow) assert(resource *ptahv1alpha1.PtahSchema, controls ...operationSQLClient) {
	w.f.t.Helper()
	f := w.f
	f.captureSchemaSQLInventory(w.name, w.inventory)
	clients, err := w.inventory.clients(resource)
	f.check(err, "bind schema refusal SQL to the exact resource Jobs and Pods")
	var counts map[string]int
	if w.audit.engine == "postgresql" {
		w.audit.snapshot()
		if len(w.pgBefore) == 0 || !bytes.HasPrefix(w.audit.pgPrefix, w.pgBefore) {
			f.fatalf("schema refusal audit lost its original PostgreSQL journal")
		}
		counts, err = postgresStatementRefusalSQL(w.audit.pgPrefix[len(w.pgBefore):], w.database, clients, schemaDiagnosticActor, w.policy.postgres, nil)
	} else {
		counts, err = mysqlStatementRefusalSQL(w.mysqlBefore, w.audit.mysqlStatementSnapshot(), w.database, w.user, clients, true, schemaDiagnosticActor, w.policy.mysql)
	}
	f.check(err, "refuse SQL outside the exact schema diagnostic contract")
	f.check(schemaRequiredSQLControls(clients, counts, controls), "observe SQL from every required diagnostic result")
	hosts := make([]string, 0, len(counts))
	for host := range counts {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	for _, host := range hosts {
		actor := clients[host]
		f.logf("SQL refusal audit: engine=%s schemaUID=%s jobUID=%s podUID=%s operation=%s client=%s allowedDiagnosticRecords=%d unauthorizedRecords=0", w.audit.engine, actor.resourceUID, actor.jobUID, actor.podUID, actor.operation, host, counts[host])
	}
	w.audit.close()
}
