//go:build e2e

package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
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

func (f *dataPlane) schemaMySQLAuditAccount(audit *databaseSQLAudit, database, secretName string) string {
	f.t.Helper()
	if secretName == mysqlSecret {
		f.fatalf("a schema audit must not replace the shared MySQL fixture credential")
	}
	user := audit.createMySQLAccount(database)
	secret := &corev1.Secret{}
	f.check(f.get(secretName, secret), "read the isolated schema target Secret")
	updated, err := replaceSchemaMySQLAuditUser(string(secret.Data["url"]), f.credentials.mysqlPassword, user)
	f.check(err, "scope the MySQL schema fixture credential")
	secret.Data["url"] = []byte(updated)
	f.check(f.cluster.Client.Update(f.ctx, secret), "scope the schema target to its audit account")
	return user
}

func (f *dataPlane) captureSchemaSQLInventory(name string, inventory *schemaSQLInventory) {
	f.t.Helper()
	jobs, pods := &batchv1.JobList{}, &corev1.PodList{}
	f.check(f.list(jobs, client.MatchingLabels{labelSchema: name}), "read schema SQL audit Jobs")
	f.check(f.list(pods, client.MatchingLabels{labelSchema: name}), "read schema SQL audit Pods")
	f.check(inventory.record(jobs.Items, pods.Items), "retain schema SQL audit identities")
}

type schemaRefusalWindow struct {
	f                    *dataPlane
	name, database, user string
	audit                *databaseSQLAudit
	policy               *schemaSQLPolicy
	inventory            *schemaSQLInventory
	pgBefore             []byte
	mysqlBefore          []mysqlStatementRecord
	wait                 func(string, string, func(*ptahv1alpha1.PtahSchema) bool) *ptahv1alpha1.PtahSchema
	resourceUID          types.UID
	unusedMySQLAccount   bool
	// Only the runner-refusal case sets this exact task image. Failed init
	// Pods keep their own client identities and are allowed to send no SQL.
	refusedRunnerImage string
}

// Fault waits allow the refusal under test to enter Failed and retain their
// periodic credential audit. Lifecycle waits keep their own failure behavior.
func (f *faultRun) startSchemaRefusalWindow(name, engine, database, secret string) *schemaRefusalWindow {
	w := f.dataPlane.startSchemaRefusalWindow(name, engine, database, secret)
	w.wait = f.waitForSchema
	return w
}

// Start after fixture setup but before its resource exists. The dedicated
// MySQL credential has never connected, so database-less records remain scoped.
// Database assertions must run after assert closes this uninterrupted window.
func (f *dataPlane) startSchemaRefusalWindow(name, engine, database, secret string) *schemaRefusalWindow {
	f.t.Helper()
	if !apierrors.IsNotFound(f.get(name, &ptahv1alpha1.PtahSchema{})) {
		f.fatalf("schema SQL refusal audit must start before its resource exists")
	}
	policy, err := newSchemaSQLPolicy(engine, database)
	f.check(err, "load the pinned schema diagnostic SQL contract")
	w := &schemaRefusalWindow{f: f, name: name, database: database, policy: policy, wait: f.waitForSchema, unusedMySQLAccount: true,
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

// Reopen only after a completed audit and while status writes hold a quiescent
// resource. The target credential is unchanged. Old Jobs are not new controls;
// any SQL they send in this new window is rejected as unidentified traffic.
func (w *schemaRefusalWindow) reopen() {
	w.f.t.Helper()
	resource := w.f.schema(w.name)
	if w.audit.started || w.resourceUID == "" || resource.UID != w.resourceUID || resource.Status.ActiveOperation != nil || !w.f.rbac.paused {
		w.f.fatalf("schema SQL refusal audit needs the same quiescent resource under a status-write barrier")
	}
	w.inventory = &schemaSQLInventory{jobs: map[types.UID]batchv1.Job{}, pods: map[types.UID]corev1.Pod{}, excludedJobs: w.f.checkpointJobs(w.name, "")}
	w.unusedMySQLAccount = false
	if w.audit.engine == "mysql" {
		w.mysqlBefore = w.audit.mysqlStatementSnapshot()
	} else {
		w.audit.snapshot()
		w.pgBefore = w.audit.pgPrefix
	}
}

func (w *schemaRefusalWindow) waitForSchema(description string, match func(*ptahv1alpha1.PtahSchema) bool) *ptahv1alpha1.PtahSchema {
	w.f.t.Helper()
	return w.wait(w.name, description, func(resource *ptahv1alpha1.PtahSchema) bool {
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
	w.assertSQL(resource, nil, controls...)
}

func (w *schemaRefusalWindow) assertStale(resource *ptahv1alpha1.PtahSchema, refused operationSQLClient, controls ...operationSQLClient) {
	w.f.t.Helper()
	if resource == nil || refused.resourceUID != string(resource.UID) {
		w.f.fatalf("stale-plan SQL audit changed the refused resource identity")
	}
	stale, err := newSchemaStaleSQL(w.policy, refused)
	w.f.check(err, "bind the stale-plan SQL exception to its refused Apply")
	w.assertSQL(resource, stale, controls...)
}

func (w *schemaRefusalWindow) assertSQL(resource *ptahv1alpha1.PtahSchema, stale *schemaStaleSQL, controls ...operationSQLClient) {
	w.f.t.Helper()
	f := w.f
	if resource == nil || resource.Name != w.name || resource.Namespace != f.in.TestNamespace || resource.UID == "" || (w.resourceUID != "" && resource.UID != w.resourceUID) {
		f.fatalf("schema SQL refusal audit changed resource identity")
	}
	f.captureSchemaSQLInventory(w.name, w.inventory)
	clients, err := w.inventory.clients(resource)
	var refusedJobs map[types.UID]bool
	if w.refusedRunnerImage != "" {
		clients, refusedJobs, err = runnerRefusalSQLClients(f.t, f.ctx, f.cluster, resource, "PtahSchema", resource.Status.ExecutionBinding,
			f.controller, w.refusedRunnerImage, w.inventory.jobs, w.inventory.pods, f.scan)
	}
	f.check(err, "bind schema refusal SQL to the exact resource Jobs and Pods")
	acceptsActor, pg, my := schemaDiagnosticActor, w.policy.postgres, w.policy.mysql
	if len(refusedJobs) > 0 {
		acceptsActor = func(actor operationSQLClient) bool {
			return !refusedJobs[types.UID(actor.jobUID)] && schemaDiagnosticActor(actor)
		}
	}
	var harnessRead func(string, string) bool
	if stale != nil {
		acceptsActor, pg, my = stale.acceptsActor, stale.postgres, stale.mysql
		harnessRead = postgresSchemaDriftHarnessRead
	}
	var counts map[string]int
	if w.audit.engine == "postgresql" {
		w.audit.snapshot()
		if len(w.pgBefore) == 0 || !bytes.HasPrefix(w.audit.pgPrefix, w.pgBefore) {
			f.fatalf("schema refusal audit lost its original PostgreSQL journal")
		}
		counts, err = postgresStatementRefusalSQL(w.audit.pgPrefix[len(w.pgBefore):], w.database, clients, acceptsActor, pg, harnessRead)
	} else {
		counts, err = mysqlStatementRefusalSQL(w.mysqlBefore, w.audit.mysqlStatementSnapshot(), w.database, w.user, clients, w.unusedMySQLAccount, acceptsActor, my)
	}
	f.check(err, "refuse SQL outside the exact schema diagnostic contract")
	if stale != nil {
		f.check(stale.complete(), "observe the refused Apply's exact lock, column read and unlock")
	}
	f.check(schemaRequiredSQLControls(clients, counts, controls), "observe SQL from every required diagnostic result")
	hosts := make([]string, 0, len(counts))
	for host := range counts {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	for _, host := range hosts {
		actor, found := clients[host]
		if !found {
			f.logf("SQL refusal audit: engine=%s harnessClient=%s allowedReadRecords=%d unauthorizedRecords=0", w.audit.engine, host, counts[host])
			continue
		}
		f.logf("SQL refusal audit: engine=%s schemaUID=%s jobUID=%s podUID=%s operation=%s client=%s allowedDiagnosticRecords=%d unauthorizedRecords=0", w.audit.engine, actor.resourceUID, actor.jobUID, actor.podUID, actor.operation, host, counts[host])
	}
	w.audit.close()
	w.resourceUID = resource.UID
}

// The harness has already changed the schema; status writes are held and the
// old Apply either does not exist yet or is stopped by the scheduling barrier.
// Keep the target credential unchanged across the old and replacement plans.
func (f *faultRun) startSchemaDriftWindow(name, database, user string, audit *databaseSQLAudit, heldApplyUID string) *schemaRefusalWindow {
	f.t.Helper()
	resource := f.schema(name)
	if audit.started || !f.rbac.paused || resource.UID == "" {
		f.fatalf("schema drift SQL audit needs an identified resource under the status-write barrier")
	}
	active := resource.Status.ActiveOperation
	if heldApplyUID == "" && active != nil || heldApplyUID != "" && (active == nil || string(active.JobUID) != heldApplyUID || active.Type != ptahv1alpha1.OperationApply) {
		f.fatalf("schema drift SQL audit lost the held Apply boundary")
	}
	policy, err := newSchemaSQLPolicy(audit.engine, database)
	f.check(err, "load the stale-plan SQL contract")
	excluded := slices.DeleteFunc(f.checkpointJobs(name, ""), func(uid string) bool { return uid == heldApplyUID })
	w := &schemaRefusalWindow{f: f.dataPlane, name: name, database: database, user: user, audit: audit, policy: policy,
		wait: f.waitForSchema, resourceUID: resource.UID,
		inventory: &schemaSQLInventory{jobs: map[types.UID]batchv1.Job{}, pods: map[types.UID]corev1.Pod{}, excludedJobs: excluded},
	}
	if audit.engine == "mysql" {
		if !mysqlAuditIdentifier.MatchString(user) || user == mysqlUser {
			f.fatalf("schema drift SQL audit needs its dedicated target account")
		}
		w.mysqlBefore = audit.mysqlStatementSnapshot()
	} else {
		audit.snapshot()
		w.pgBefore = audit.pgPrefix
	}
	f.captureSchemaSQLInventory(name, w.inventory)
	return w
}

// A closed watch retains terminal identities even after Job TTL cleanup.
// Records from the preceding initial-plan window remain explicitly excluded.
func (f *faultRun) retainSchemaSQLWatch(w *schemaRefusalWindow) {
	f.t.Helper()
	for _, event := range f.jobs.snapshot() {
		if event.Object.Labels[labelSchema] == w.name {
			f.check(w.inventory.record([]batchv1.Job{*event.Object}, nil), "retain the schema SQL Job watch")
		}
	}
	for _, event := range f.pods.snapshot() {
		if event.Object.Labels[labelSchema] == w.name {
			f.check(w.inventory.record(nil, []corev1.Pod{*event.Object}), "retain the schema SQL Pod watch")
		}
	}
}
