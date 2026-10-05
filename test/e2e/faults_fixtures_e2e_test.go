//go:build e2e

package e2e

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// execInWith runs a command in a Deployment's Pod of the test namespace under
// the context given, and returns what it wrote on standard output.
func (f *faultRun) execInWith(ctx context.Context, deployment string, command ...string) (string, error) {
	stdout, stderr, err := f.cluster.Kubectl(ctx, append([]string{"-n", f.in.TestNamespace, "exec",
		"deployment/" + deployment, "--"}, command...)...)
	if err != nil {
		return string(stdout), fmt.Errorf("exec in deployment/%s: %w: %s", deployment, err, strings.TrimSpace(string(stderr)))
	}
	return string(stdout), nil
}

// psqlWith runs one query in a database of the PostgreSQL server as its own
// user.
func (f *faultRun) psqlWith(ctx context.Context, database, query string) (string, error) {
	return f.execInWith(ctx, pgService, "sh", "-ec",
		`PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -Atqc "$2"`,
		"sh", database, query)
}

// mysqlRootWith runs one query in a database of the MySQL server as root.
func (f *faultRun) mysqlRootWith(ctx context.Context, database, query string) (string, error) {
	return f.execInWith(ctx, mysqlService, "sh", "-ec",
		`MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1" -Nse "$2"`,
		"sh", database, query)
}

// query runs one query on the engine's server and ends the scenario when it
// fails. It returns the output with its whitespace removed.
func (f *faultRun) query(engine, database, query string) string {
	f.t.Helper()
	var output string
	var err error
	switch engine {
	case "postgresql":
		output, err = f.psqlWith(f.ctx, database, query)
	case "mysql":
		output, err = f.mysqlRootWith(f.ctx, database, query)
	default:
		f.fatalf("unsupported query engine %s", engine)
	}
	f.check(err, "query %s database %s", engine, database)
	return removeWhitespace(output)
}

// waitQueryEquals polls a query until its answer is the one expected. A
// failed read is a reading that did not match yet.
func (f *faultRun) waitQueryEquals(engine, database, query, expected, description string) {
	f.t.Helper()
	f.poll(description, func() bool {
		var output string
		var err error
		switch engine {
		case "postgresql":
			output, err = f.psqlWith(f.ctx, database, query)
		case "mysql":
			output, err = f.mysqlRootWith(f.ctx, database, query)
		default:
			f.fatalf("unsupported query engine %s", engine)
		}
		return err == nil && removeWhitespace(output) == expected
	})
}

// secretValue reads one key of a Secret in the test namespace.
func (f *faultRun) secretValue(name, key string) string {
	f.t.Helper()
	secret := &corev1.Secret{}
	f.check(f.get(name, secret), "read Secret %s", name)
	return string(secret.Data[key])
}

// createPrincipalSecret stores the credential-principal artifact and its
// password in a Secret only the handcrafted publisher mounts.
func (f *faultRun) createPrincipalSecret() {
	f.t.Helper()
	secret := &corev1.Secret{Type: corev1.SecretTypeOpaque, StringData: map[string]string{
		"schema.hcl": principalSchemaHCL(f.principalRole, f.principalPassword),
		"password":   f.principalPassword,
	}}
	secret.Namespace, secret.Name = f.in.TestNamespace, f.principalSecretName()
	f.check(f.cluster.Client.Create(f.ctx, secret, client.FieldOwner(harness.FieldOwner)),
		"create Secret %s", f.principalSecretName())
}

// buildScanner reads the credentials the fault injection protects from the
// Secrets that hold them: the registry password, the PostgreSQL and MySQL
// passwords and URLs, MySQL's root password, and the principal's password.
func (f *faultRun) buildScanner() {
	f.t.Helper()
	var patterns []string
	for _, source := range [][2]string{
		{registryAuthSecret, "password"},
		{pgSecret, "password"}, {pgSecret, "url"},
		{mysqlSecret, "password"}, {mysqlSecret, "rootPassword"}, {mysqlSecret, "url"},
		{f.principalSecretName(), "password"},
	} {
		value := f.secretValue(source[0], source[1])
		if value == "" {
			f.fatalf("a protected credential is empty")
		}
		patterns = append(patterns, value)
	}
	scanner, err := newCredentialScanner(patterns...)
	f.check(err, "build the fault credential scanner")
	f.scanner = scanner
}

// databaseURLFor is the engine's base URL pointed at another database.
func (f *faultRun) databaseURLFor(engine, database string) string {
	f.t.Helper()
	base := ""
	switch engine {
	case "postgresql":
		base = f.secretValue(pgSecret, "url")
	case "mysql":
		base = f.secretValue(mysqlSecret, "url")
	default:
		f.fatalf("unsupported URL engine %s", engine)
	}
	url, err := replaceDatabaseURLPath(base, database)
	if err != nil || url == "" || url == base {
		f.fatalf("could not derive an isolated %s database URL", engine)
	}
	return url
}

// createURLSecret stores a database's URL in a Secret, spelled through the
// Service's cluster domain name, or through the Service name alone for the
// short alias.
func (f *faultRun) createURLSecret(engine, database, name string, short bool) {
	f.t.Helper()
	url := f.databaseURLFor(engine, database)
	if short {
		service := pgService
		if engine == "mysql" {
			service = mysqlService
		}
		aliased, err := shortServiceURL(url, service, f.in.TestNamespace)
		f.check(err, "alias the %s URL of %s", engine, database)
		url = aliased
	}
	secret := &corev1.Secret{Type: corev1.SecretTypeOpaque, StringData: map[string]string{"url": url}}
	secret.Namespace, secret.Name = f.in.TestNamespace, name
	f.check(f.cluster.Client.Create(f.ctx, secret, client.FieldOwner(harness.FieldOwner)), "create Secret %s", name)
}

// createDatabase creates an isolated database on the engine's server, seeds
// it with the v3 fixture, and stores its URL in a Secret.
func (f *faultRun) createDatabase(engine, database, secret string) {
	f.t.Helper()
	if !safeDatabaseName.MatchString(database) {
		f.fatalf("unsafe generated database or barrier name: %s", database)
	}
	switch engine {
	case "postgresql":
		if f.query("postgresql", "postgres", "SELECT count(*) FROM pg_database WHERE datname='"+database+"'") != "0" {
			f.fatalf("PostgreSQL database %s already exists", database)
		}
		f.query("postgresql", "postgres", "CREATE DATABASE "+database)
		f.createURLSecret(engine, database, secret, false)
		seed, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", "postgresql-v3.sql"))
		f.check(err, "read the PostgreSQL seed")
		f.check(f.execInWithInput(pgService, seed, "sh", "-ec",
			`PGPASSWORD="$POSTGRES_PASSWORD" psql -v ON_ERROR_STOP=1 -q -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1"`,
			"sh", database), "could not seed PostgreSQL database %s", database)
	case "mysql":
		if f.query("mysql", "mysql", "SELECT count(*) FROM information_schema.schemata WHERE schema_name='"+database+"'") != "0" {
			f.fatalf("MySQL database %s already exists", database)
		}
		f.query("mysql", "mysql", "CREATE DATABASE "+database+"; GRANT ALL PRIVILEGES ON "+database+
			".* TO '"+mysqlUser+"'@'%'; FLUSH PRIVILEGES")
		f.createURLSecret(engine, database, secret, false)
		seed, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", "mysql-v3.sql"))
		f.check(err, "read the MySQL seed")
		f.check(f.execInWithInput(mysqlService, seed, "sh", "-ec",
			`MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1"`,
			"sh", database), "could not seed MySQL database %s", database)
	default:
		f.fatalf("unsupported database engine %s", engine)
	}
}

// assertColumn holds the count of one e2e_widgets column in a database.
func (f *faultRun) assertColumn(engine, database, column string, expected int) {
	f.t.Helper()
	query := "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='e2e_widgets' AND column_name='" + column + "'"
	if engine == "mysql" {
		query = "SELECT count(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='e2e_widgets' AND column_name='" + column + "'"
	}
	if actual := f.query(engine, database, query); actual != strconv.Itoa(expected) {
		f.fatalf("%s database %s column %s count is %s, expected %d", engine, database, column, actual, expected)
	}
}

// pgFingerprint is an MD5 of every public column's name, type and
// nullability.
func (f *faultRun) pgFingerprint(database string) string {
	f.t.Helper()
	return f.query("postgresql", database, postgresSchemaFingerprintSQL)
}

// mysqlFingerprint is an MD5 of every column and index of the database.
func (f *faultRun) mysqlFingerprint(database string) string {
	f.t.Helper()
	return f.query("mysql", database, `
    SELECT MD5(COALESCE(GROUP_CONCAT(entry ORDER BY entry SEPARATOR '\n'), ''))
    FROM (
      SELECT CONCAT(
        'column:', table_name, ':', LPAD(ordinal_position, 6, '0'), ':', column_name, ':',
        column_type, ':', is_nullable, ':', COALESCE(column_default, '<NULL>'), ':',
        extra, ':', COALESCE(collation_name, '<NULL>'), ':', COALESCE(generation_expression, '<NULL>')) AS entry
      FROM information_schema.columns
      WHERE table_schema = DATABASE()
      UNION ALL
      SELECT CONCAT(
        'index:', table_name, ':', index_name, ':', LPAD(seq_in_index, 6, '0'), ':',
        non_unique, ':', COALESCE(column_name, '<NULL>'), ':', COALESCE(sub_part, 0), ':',
        index_type, ':', COALESCE(collation, '<NULL>'), ':', COALESCE(expression, '<NULL>')) AS entry
      FROM information_schema.statistics
      WHERE table_schema = DATABASE()
    ) AS managed_schema`)
}

// fingerprint is the engine's fingerprint, held to being an MD5.
func (f *faultRun) fingerprint(engine, database, description string) string {
	f.t.Helper()
	value := ""
	if engine == "mysql" {
		value = f.mysqlFingerprint(database)
	} else {
		value = f.pgFingerprint(database)
	}
	if !schemaFingerprint.MatchString(value) {
		f.fatalf("could not fingerprint %s", description)
	}
	return value
}

// mysqlBarrier is the one MySQL session holding a table's metadata lock.
type mysqlBarrier struct {
	session         *backgroundCommand
	ready, database string
}

// startPGBarrier opens a session that holds an ACCESS SHARE lock on
// e2e_widgets for faultBarrierSeconds, so an Apply that takes its advisory
// lock blocks on its first DDL, and waits until the session holds it.
func (f *faultRun) startPGBarrier(database, token string) {
	f.t.Helper()
	if !safeDatabaseName.MatchString(token) {
		f.fatalf("unsafe generated database or barrier name: %s", token)
	}
	if _, active := f.pgBarriers[token]; active {
		f.fatalf("PostgreSQL metadata barrier %s already exists", token)
	}
	f.pgBarriers[token] = f.startKubectl("-n", f.in.TestNamespace, "exec", "deployment/"+pgService, "--",
		"sh", "-ec",
		`PGAPPNAME="$2" PGPASSWORD="$POSTGRES_PASSWORD" psql -v ON_ERROR_STOP=1 -q -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -c "BEGIN; LOCK TABLE e2e_widgets IN ACCESS SHARE MODE; SELECT pg_sleep($3); ROLLBACK"`,
		"sh", database, token, strconv.Itoa(faultBarrierSeconds))
	f.waitQueryEquals("postgresql", "postgres",
		"SELECT count(*) FROM pg_stat_activity WHERE application_name='"+token+"' AND wait_event='PgSleep'",
		"1", "the PostgreSQL metadata barrier to hold its table lock")
}

// stopPGBarrier terminates the barrier's backend and waits until it is gone.
func (f *faultRun) stopPGBarrier(token string) {
	f.t.Helper()
	barrier, active := f.pgBarriers[token]
	if !active {
		f.fatalf("PostgreSQL metadata barrier %s is not active", token)
	}
	terminated, _ := f.psqlWith(f.ctx, "postgres", "SELECT count(*) FROM (SELECT pg_terminate_backend(pid) AS terminated FROM pg_stat_activity WHERE application_name='"+token+"') AS attempts WHERE terminated")
	if removeWhitespace(terminated) != "1" {
		f.fatalf("could not terminate PostgreSQL metadata barrier %s", token)
	}
	f.waitQueryEquals("postgresql", "postgres",
		"SELECT count(*) FROM pg_stat_activity WHERE application_name='"+token+"'",
		"0", "the PostgreSQL metadata barrier backend to terminate")
	barrier.stop()
	delete(f.pgBarriers, token)
}

// startMySQLBarrier opens a session that holds a READ table lock on
// e2e_widgets for faultBarrierSeconds, marked by a named lock it takes once
// the table lock is held, and waits for that mark.
func (f *faultRun) startMySQLBarrier(database, token string) {
	f.t.Helper()
	if !safeDatabaseName.MatchString(token) {
		f.fatalf("unsafe generated database or barrier name: %s", token)
	}
	if f.mysqlBarrier != nil {
		f.fatalf("a MySQL metadata barrier is already active")
	}
	guard, ready := token+"_guard", token+"_ready"
	statement := fmt.Sprintf("SELECT GET_LOCK('%s', 0); LOCK TABLES e2e_widgets READ; SELECT GET_LOCK('%s', 0); "+
		"DO SLEEP(%d); UNLOCK TABLES; SELECT RELEASE_LOCK('%s'); SELECT RELEASE_LOCK('%s')",
		guard, ready, faultBarrierSeconds, ready, guard)
	session := f.startKubectl("-n", f.in.TestNamespace, "exec", "deployment/"+mysqlService, "--",
		"sh", "-ec", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1" -Nse "$2"`,
		"sh", database, statement)
	f.mysqlBarrier = &mysqlBarrier{session: session, ready: ready, database: database}
	f.waitQueryEquals("mysql", "mysql", "SELECT IF(IS_USED_LOCK('"+ready+"') IS NULL, 0, 1)",
		"1", "the MySQL metadata barrier to hold its table lock")
}

// stopMySQLBarrier ends the barrier. MySQL runs a queued DDL after the client
// that sent it is gone: a killed Apply Pod leaves its ALTER waiting on this
// barrier's table lock, and releasing the lock would apply it long after the
// Job died, which is not what a killed Job means to the proofs that follow.
// So what waits for this barrier's table lock is killed first, and the
// barrier releases into an idle database. The recovery Observe reads through
// a READ lock without waiting, so it is not among them.
func (f *faultRun) stopMySQLBarrier() {
	f.t.Helper()
	barrier := f.mysqlBarrier
	if barrier == nil {
		return
	}
	abandoned, err := f.mysqlRootWith(f.ctx, "mysql", "SELECT ID FROM information_schema.processlist WHERE USER = '"+
		mysqlUser+"' AND DB = '"+barrier.database+"' AND STATE LIKE 'Waiting for table%'")
	f.check(err, "list the MySQL sessions waiting on the barrier")
	for _, thread := range strings.Fields(abandoned) {
		if decimalCount.MatchString(thread) && thread != "0" {
			_, _ = f.mysqlRootWith(f.ctx, "mysql", "KILL "+thread)
		}
	}
	f.releaseMySQLBarrier()
}

// A running-Apply rollout keeps the original client alive. Release only the
// barrier so that client can complete its already authorized DDL.
func (f *faultRun) releaseMySQLBarrier() {
	f.t.Helper()
	barrier := f.mysqlBarrier
	if barrier == nil {
		f.fatalf("no MySQL metadata barrier is active")
	}
	id := f.query("mysql", "mysql", "SELECT IS_USED_LOCK('"+barrier.ready+"')")
	if !decimalCount.MatchString(id) || id == "0" {
		f.fatalf("could not identify the MySQL metadata barrier connection")
	}
	f.query("mysql", "mysql", "KILL "+id)
	barrier.session.stop()
	f.mysqlBarrier = nil
}

// assertPGApplyLockWait holds the database to one Apply backend that holds
// Ptah's advisory lock and waits, in the same session, for the barrier's
// table.
func (f *faultRun) assertPGApplyLockWait(database string) {
	f.t.Helper()
	key := strconv.Itoa(pgApplyLockKey)
	f.waitQueryEquals("postgresql", database,
		"SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE l.locktype='advisory' AND l.classid=0 AND l.objid="+key+" AND l.objsubid=1 AND l.granted AND a.datname='"+database+"'",
		"1", "the exact PostgreSQL Ptah advisory lock")
	f.waitQueryEquals("postgresql", database,
		"SELECT count(*) FROM pg_locks advisory JOIN pg_stat_activity activity ON activity.pid=advisory.pid JOIN pg_locks ddl ON ddl.pid=advisory.pid WHERE advisory.locktype='advisory' AND advisory.classid=0 AND advisory.objid="+key+" AND advisory.objsubid=1 AND advisory.granted AND activity.datname='"+database+"' AND ddl.locktype='relation' AND ddl.relation='public.e2e_widgets'::regclass AND ddl.mode='AccessExclusiveLock' AND NOT ddl.granted",
		"1", "the PostgreSQL Apply backend to block on the metadata barrier after acquiring its advisory lock")
}

// assertMySQLApplyLockWait holds the server to one session that holds Ptah's
// advisory lock and waits, in the same session, for the barrier's metadata
// lock on the database.
func (f *faultRun) assertMySQLApplyLockWait(database string) {
	f.t.Helper()
	f.waitQueryEquals("mysql", "mysql", "SELECT IF(IS_USED_LOCK('ptah_schema_apply') IS NULL, 0, 1)",
		"1", "the exact MySQL Ptah advisory lock")
	f.waitQueryEquals("mysql", "mysql",
		"SELECT count(*) FROM information_schema.processlist WHERE ID=IS_USED_LOCK('ptah_schema_apply') AND DB='"+database+"' AND STATE LIKE '%metadata lock%'",
		"1", "the MySQL Apply backend to block on the metadata barrier after acquiring its advisory lock")
}

// setReadBarrier puts the scheduling barrier's NoSchedule taint on every node,
// or takes it off. A Pod that tolerates nothing stays unscheduled while it is
// on, so a Job created then is held before it runs.
func (f *faultRun) setReadBarrier(ctx context.Context, on bool) error {
	nodes := &corev1.NodeList{}
	if err := f.cluster.Client.List(ctx, nodes); err != nil {
		return err
	}
	for index := range nodes.Items {
		name := nodes.Items[index].Name
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			node := &corev1.Node{}
			if err := f.cluster.Client.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
				return err
			}
			taints := slices.DeleteFunc(slices.Clone(node.Spec.Taints), func(taint corev1.Taint) bool {
				return taint.Key == readWorkloadBarrierKey
			})
			if on {
				taints = append(taints, corev1.Taint{Key: readWorkloadBarrierKey, Value: "held", Effect: corev1.TaintEffectNoSchedule})
			}
			node.Spec.Taints = taints
			return f.cluster.Client.Update(ctx, node, client.FieldOwner(harness.FieldOwner))
		})
		if err != nil {
			return fmt.Errorf("node %s: %w", name, err)
		}
	}
	return nil
}

func (f *faultRun) nodes() []corev1.Node {
	f.t.Helper()
	nodes := &corev1.NodeList{}
	f.check(f.cluster.Client.List(f.ctx, nodes), "list the nodes")
	return nodes.Items
}

// startReadBarrier raises the scheduling barrier. It records that it did
// before the first write, so the cleanup also covers a barrier half raised.
func (f *faultRun) startReadBarrier() {
	f.t.Helper()
	if f.readBarrierOn {
		f.fatalf("the read-workload scheduling barrier is already active")
	}
	if nodes := f.nodes(); len(nodes) == 0 || !nodesFreeOfBarrier(nodes) {
		f.fatalf("the test scheduling taint already exists on a cluster node")
	}
	f.readBarrierOn = true
	f.check(f.setReadBarrier(f.ctx, true), "install the test scheduling barrier")
	if !nodesCarryBarrier(f.nodes()) {
		f.fatalf("the test scheduling barrier was not installed on every node")
	}
}

// stopReadBarrier lowers the scheduling barrier.
func (f *faultRun) stopReadBarrier() {
	f.t.Helper()
	if !f.readBarrierOn {
		f.fatalf("the read-workload scheduling barrier is not active")
	}
	f.check(f.setReadBarrier(f.ctx, false), "remove the test scheduling barrier")
	if !nodesFreeOfBarrier(f.nodes()) {
		f.fatalf("the test scheduling barrier remained on a cluster node")
	}
	f.readBarrierOn = false
}

// assertReadBlocked waits for the Job and holds it, and its Pods, to the
// barrier: unfinished, tolerating nothing the barrier sets, and on no node.
func (f *faultRun) assertReadBlocked(jobUID, description string) {
	f.t.Helper()
	if !f.readBarrierOn {
		f.fatalf("%s was checked without the scheduling barrier", description)
	}
	f.poll("the scheduling barrier to hold "+description, func() bool {
		jobs := &batchv1.JobList{}
		if f.list(jobs) != nil {
			return false
		}
		index := slices.IndexFunc(jobs.Items, func(job batchv1.Job) bool { return uidIs(&job, jobUID) })
		if index < 0 {
			return false
		}
		if !jobHeldByBarrier(&jobs.Items[index]) {
			f.fatalf("%s bypasses or completed through the scheduling barrier", description)
		}
		pods := &corev1.PodList{}
		f.check(f.list(pods, client.MatchingLabels{"batch.kubernetes.io/controller-uid": jobUID}), "list the Pods of %s", description)
		if !podsUnscheduled(pods.Items) {
			f.fatalf("%s scheduled or started before status-write revocation", description)
		}
		return true
	})
}

// faultSchema is a PtahSchema a fault proof creates. The zero values are the
// ones most proofs use.
type faultSchema struct {
	name, engine, secret, reference, coordinationKey string
	verificationPolicy                               string
	allowDestructive                                 bool
	// failureRetry is 5s, activeDeadline faultActiveDeadlineSeconds and
	// lockTimeout 60s when unset.
	failureRetry   string
	activeDeadline int64
	lockTimeout    string
	// sharedRealm is the declaration the shared-alias proof makes. A
	// database only one resource claims needs none.
	sharedRealm bool
	// isolatedNode places this row's operations on the dedicated fault worker.
	isolatedNode string
}

// createSchema creates the schema under approval, every hour, against the
// fault registry, as the fault proofs declare it.
func (f *faultRun) createSchema(schema faultSchema) {
	f.t.Helper()
	failureRetry := cmp.Or(schema.failureRetry, "5s")
	lockTimeout := cmp.Or(schema.lockTimeout, "60s")
	activeDeadline := schema.activeDeadline
	if activeDeadline == 0 {
		activeDeadline = faultActiveDeadlineSeconds
	}
	execution := map[string]any{
		"activeDeadlineSeconds": activeDeadline, "failureRetryInterval": failureRetry, "connectTimeout": "30s",
		"imagePullSecrets": []any{map[string]any{"name": registryPullSecret}},
	}
	if schema.isolatedNode != "" {
		execution["nodeSelector"] = map[string]any{"kubernetes.io/hostname": schema.isolatedNode, isolationNodeKey: "true"}
		execution["tolerations"] = []any{map[string]any{"key": isolationNodeKey, "operator": "Equal", "value": "true", "effect": "NoSchedule"}}
	}
	f.check(f.create(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": f.in.TestNamespace, "name": schema.name},
		"spec": map[string]any{
			"target": map[string]any{
				"engine": schema.engine, "coordinationKey": schema.coordinationKey,
				"sharedRealm": schema.sharedRealm,
				"urlFrom":     map[string]any{"name": schema.secret, "key": "url"},
			},
			"desired": map[string]any{
				"ociRef": schema.reference,
				"registryAuthFrom": map[string]any{
					"name": registryAuthSecret, "mode": "Environment",
					"usernameKey": "username", "passwordKey": "password",
				},
				"verificationPolicyFrom": map[string]any{"name": cmp.Or(schema.verificationPolicy, verificationPolicyName), "key": verificationPolicyKey},
				"transport":              map[string]any{"plainHTTP": true},
			},
			"policy": map[string]any{
				"apply": "OnApproval", "allowDestructive": schema.allowDestructive, "driftSeverity": "all",
				"lockTimeout": lockTimeout, "transactionMode": "file",
			},
			"interval":  "1h",
			"execution": execution,
		},
	}), "create PtahSchema %s", schema.name)
}

// waitForPlan waits for a plan waiting for exact approval, and holds it and
// the schema's current plan to this manager. It returns the plan's name.
func (f *faultRun) waitForPlan(name string) string {
	f.t.Helper()
	schema := f.waitForSchema(name, "a non-destructive plan awaiting exact approval", planAwaitingApproval)
	stateVersion := f.stateVersion()
	if !exactControllerPlan(schema.Status.Plan, schema.Status.ExecutionBinding, f.controller, stateVersion) {
		f.fatalf("%s current plan lacks its exact controller identity", name)
	}
	plan := &ptahv1alpha1.PtahSchemaPlan{}
	f.check(f.get(schema.Status.Plan.Name, plan), "read PtahSchemaPlan %s", schema.Status.Plan.Name)
	if err := readyPlanFromController(plan, f.controller, stateVersion); err != nil {
		f.fatalf("%s did not produce a ready non-destructive plan: %v", name, err)
	}
	return plan.Name
}

// createApproval approves the schema's current plan by name, UID and
// fingerprint, after holding the plan and the schema to this manager, and
// holds the stored approval to the stamps admission gives it.
func (f *faultRun) createApproval(schemaName, approval string) {
	f.t.Helper()
	schema := &ptahv1alpha1.PtahSchema{}
	f.check(f.get(schemaName, schema), "read PtahSchema %s", schemaName)
	if schema.Status.Plan == nil || schema.Status.Plan.Name == "" {
		f.fatalf("%s has no plan to approve", schemaName)
	}
	plan := &ptahv1alpha1.PtahSchemaPlan{}
	f.check(f.get(schema.Status.Plan.Name, plan), "read PtahSchemaPlan %s", schema.Status.Plan.Name)
	stateVersion := f.stateVersion()
	if !approvalBindsCurrentPlan(schema, plan, f.controller, stateVersion) {
		f.fatalf("%s current plan is not bound to the exact controller identity", schemaName)
	}
	if !approvablePlan(plan, f.runnerProtocol, f.controller, stateVersion) {
		f.fatalf("%s is not a current-contract plan with the exact controller identity", plan.Name)
	}
	f.check(f.create(approvalDocument(f.in.TestNamespace, approval, schemaName, string(schema.UID), plan.Name,
		string(plan.UID), plan.Spec.Fingerprint)), "create PtahSchemaApproval %s", approval)
	if err := approvalStampedExact(f.unstructuredApproval(approval),
		ptahv1alpha1.ImmutableObjectReference{Name: schemaName, UID: schema.UID},
		ptahv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
		plan.Spec.Fingerprint); err != nil {
		f.fatalf("%s was not stamped against the exact current plan: %v", approval, err)
	}
}

// applyRun is one dispatched Apply as a proof pins it: its operation, its Job
// and its one Pod.
type applyRun struct {
	operationID, jobName, jobUID, podName, podUID string
}

// waitForApplyPod waits for the schema to dispatch its Apply, holds the Job
// to the single-Pod contract, and waits for its one Pod to run.
func (f *faultRun) waitForApplyPod(name string) applyRun {
	f.t.Helper()
	schema := f.waitForSchema(name, "one dispatched Apply Job", applyDispatched)
	active := schema.Status.ActiveOperation
	run := applyRun{operationID: active.ID, jobName: active.JobName, jobUID: string(active.JobUID)}
	job := &batchv1.Job{}
	f.check(f.get(run.jobName, job), "read Apply Job %s", run.jobName)
	if !singlePodApplyJob(job, run.jobUID, name, run.operationID) {
		f.fatalf("%s Apply Job does not preserve the single-Pod failure contract", name)
	}
	f.poll("the running Apply Pod for "+name, func() bool {
		pods := &corev1.PodList{}
		f.check(f.list(pods, client.MatchingLabels{"job-name": run.jobName}), "list the Pods of %s", run.jobName)
		pod, err := runningApplyPod(pods.Items)
		if err != nil {
			f.fatalf("%s has %v", name, err)
		}
		if pod == nil {
			return false
		}
		if !annotationIs(pod, annotationOperationID, run.operationID) {
			f.fatalf("%s executor Pod lost its full operation identity", name)
		}
		run.podName, run.podUID = pod.Name, string(pod.UID)
		return true
	})
	return run
}

// assertActiveIdentity holds the schema to the Apply it ran before the
// manager restarted, and its Pod to the same UID.
func (f *faultRun) assertActiveIdentity(name string, run applyRun) {
	f.t.Helper()
	f.waitForSchema(name, "the original active operation after the controller restart",
		func(schema *ptahv1alpha1.PtahSchema) bool {
			return activeIdentityKept(schema, run.operationID, run.jobName, run.jobUID)
		})
	pod := &corev1.Pod{}
	f.check(f.get(run.podName, pod), "read executor Pod %s", run.podName)
	if string(pod.UID) != run.podUID {
		f.fatalf("%s executor Pod was replaced across the controller restart", name)
	}
}

// waitForInSync waits for the schema to converge for the reason the scenario
// earned. An Apply whose outcome was unknown converges under its own reason,
// so a scenario that never lost attribution cannot pass on the weaker one.
func (f *faultRun) waitForInSync(name, reason string) *ptahv1alpha1.PtahSchema {
	f.t.Helper()
	return f.waitForSchema(name, "post-apply observation to prove convergence as "+reason,
		func(schema *ptahv1alpha1.PtahSchema) bool { return inSyncFor(schema, reason) })
}

// coordinationLeases lists the target Leases in the release namespace.
func (f *faultRun) coordinationLeases() []coordinationv1.Lease {
	f.t.Helper()
	leases := &coordinationv1.LeaseList{}
	f.check(f.cluster.Client.List(f.ctx, leases, client.InNamespace(f.in.OperatorNamespace), coordinationLeaseLabel),
		"list the target Leases")
	return leases.Items
}

// checkpointLeases is the target Leases that exist now.
func (f *faultRun) checkpointLeases() checkpoint {
	f.t.Helper()
	return coordinationLeaseUIDs(f.coordinationLeases())
}

// loadNewReleasedLease waits for the one target Lease a schema's first Plan
// took and released.
func (f *faultRun) loadNewReleasedLease(before checkpoint, description string) leaseIdentity {
	f.t.Helper()
	var found leaseIdentity
	f.poll(description, func() bool {
		identity, count, released, err := newReleasedLease(f.coordinationLeases(), before)
		switch {
		case count > 1:
			f.fatalf("%s created %d target Leases, expected exactly one", description, count)
		case count == 1 && err != nil:
			f.fatalf("%s %v", description, err)
		case count == 1 && released:
			found = identity
			return true
		}
		return false
	})
	return found
}

// waitForLeaseReacquisition waits for the idle Lease to be held again on its
// UID at a new epoch.
func (f *faultRun) waitForLeaseReacquisition(idle leaseIdentity, description string) leaseIdentity {
	f.t.Helper()
	var found leaseIdentity
	f.poll(description+" to reuse its released target Lease", func() bool {
		lease := &coordinationv1.Lease{}
		if err := f.cluster.Client.Get(f.ctx, f.operatorKey(idle.name), lease); err != nil {
			return false
		}
		identity, ok := reacquiredLease(lease, idle.uid, idle.epoch)
		found = identity
		return ok
	})
	return found
}

// loadHeldLeaseForEpoch waits for the one target Lease held at the epoch.
func (f *faultRun) loadHeldLeaseForEpoch(epoch, description string) leaseIdentity {
	f.t.Helper()
	var found leaseIdentity
	f.poll(description, func() bool {
		held := heldLeasesAtEpoch(f.coordinationLeases(), epoch)
		if len(held) > 1 {
			f.fatalf("%s matched multiple live target Leases for one epoch", description)
		}
		if len(held) == 1 {
			found = held[0]
			return true
		}
		return false
	})
	f.assertLeaseIdentity(found)
	return found
}

// assertLeaseIdentity holds the Lease to the UID, holder and epoch pinned.
func (f *faultRun) assertLeaseIdentity(identity leaseIdentity) {
	f.t.Helper()
	lease := &coordinationv1.Lease{}
	f.check(f.cluster.Client.Get(f.ctx, f.operatorKey(identity.name), lease), "read %s", identity)
	if !leaseIs(lease, identity) {
		f.fatalf("target Lease identity or holder changed across a controller restart: want %s", identity)
	}
}

// assertLeaseHeldWithoutRelease holds the Lease to its identity now and, from
// the moment the watch first saw it so, at every event up to a barrier just
// written: it was never released or replaced in between.
func (f *faultRun) assertLeaseHeldWithoutRelease(identity leaseIdentity) {
	f.t.Helper()
	f.assertLeaseIdentity(identity)
	establishBarrier(f, f.leases, &coordinationv1.Lease{}, f.in.OperatorNamespace, identity.name)
	f.assertLeaseIdentity(identity)
	if !leaseHeldWithoutRelease(f.leases.snapshot(), identity.uid, identity.holder, identity.epoch) {
		f.fatalf("target Lease was released or replaced before the controlled proof boundary: %s", identity)
	}
}

// checkpointOperationWatch is the schema's Jobs of one operation the Job
// watch holds at a barrier, held to the count the proof expects.
func (f *faultRun) checkpointOperationWatch(schema, operation string, expected int) checkpoint {
	f.t.Helper()
	f.jobBarrier()
	uids := addedUIDs(f.jobs.snapshot(), schema, operation)
	if len(uids) != expected {
		f.fatalf("%s has %d historical %s Jobs at its exact watch checkpoint, expected %d", schema, len(uids), operation, expected)
	}
	return sortedCheckpoint(uids)
}

// checkpointSchemaJobWatch is every Job of the schema the Job watch holds at
// a barrier. A schema with none has no lifecycle to delete.
func (f *faultRun) checkpointSchemaJobWatch(schema string) checkpoint {
	f.t.Helper()
	f.jobBarrier()
	uids := addedUIDs(f.jobs.snapshot(), schema, "")
	if len(uids) == 0 {
		f.fatalf("%s has no historical lifecycle Jobs at its exact deletion checkpoint", schema)
	}
	return sortedCheckpoint(uids)
}

// newWatchedCount counts the schema's Jobs of the operation the watch saw
// added since the checkpoint.
func (f *faultRun) newWatchedCount(schema, operation string, before checkpoint) int {
	return len(newAddedUIDs(f.jobs.snapshot(), schema, operation, before))
}

// addedJobCount and addedPodCount count every Job or Pod of the schema's
// operation the watch ever saw added.
func (f *faultRun) addedJobCount(schema, operation string) int {
	return len(addedUIDs(f.jobs.snapshot(), schema, operation))
}

func (f *faultRun) addedPodCount(schema, operation string) int {
	return len(addedUIDs(f.pods.snapshot(), schema, operation))
}

// onlyAdded holds the watch to having seen exactly the one UID added for the
// schema's operation.
func onlyAdded[T client.Object](events []watchEvent[T], schema, operation, uid string) bool {
	return slices.Equal(addedUIDs(events, schema, operation), []string{uid})
}

// singleNewWatchedJobUID is the one Job of the operation added since the
// checkpoint.
func (f *faultRun) singleNewWatchedJobUID(schema, operation string, before checkpoint) string {
	f.t.Helper()
	uids := newAddedUIDs(f.jobs.snapshot(), schema, operation, before)
	if len(uids) != 1 {
		f.fatalf("expected exactly one new watched %s Job UID for %s, found %d", operation, schema, len(uids))
	}
	return uids[0]
}

// waitForOneNewWatchedJob waits for the first Job of the operation added
// since the checkpoint, and ends the scenario on a second, and returns it.
func (f *faultRun) waitForOneNewWatchedJob(schema, operation string, before checkpoint, description string) string {
	f.t.Helper()
	f.poll(description, func() bool {
		count := f.newWatchedCount(schema, operation, before)
		if count > 1 {
			f.fatalf("%s created more than one new %s Job after the exact checkpoint", schema, operation)
		}
		return count == 1
	})
	return f.singleNewWatchedJobUID(schema, operation, before)
}

// waitForWatchCountAbove waits for the watch to see more of the schema's Jobs
// of the operation added than before.
func (f *faultRun) waitForWatchCountAbove(schema, operation string, before int, description string) {
	f.t.Helper()
	f.poll(description, func() bool { return f.addedJobCount(schema, operation) > before })
}

// waitForOutcomeUnknownWatch waits for the schema watch to see the Apply's
// outcome recorded as unknown.
func (f *faultRun) waitForOutcomeUnknownWatch(schema, applyOperationID string) {
	f.t.Helper()
	f.poll("schema watch to record durable OutcomeUnknown proof for "+schema, func() bool {
		return outcomeUnknownRecorded(f.schemas.snapshot(), schema, applyOperationID)
	})
}

// waitForApplyBindingInSchemaWatch waits for the schema watch to name the
// one Apply operation and lease epoch the Job was dispatched for.
func (f *faultRun) waitForApplyBindingInSchemaWatch(schema, jobUID, description string) (operationID, leaseEpoch string) {
	f.t.Helper()
	f.poll(description+" in the schema watch", func() bool {
		count, id, epoch, err := applyBindingInWatch(f.schemas.snapshot(), schema, jobUID)
		switch {
		case err != nil:
			f.fatalf("%s: %v", description, err)
		case count > 1:
			f.fatalf("%s recorded multiple active-operation bindings", description)
		case count == 1:
			operationID, leaseEpoch = id, epoch
			return true
		}
		return false
	})
	return operationID, leaseEpoch
}

// waitForExactJobTerminal waits for the Job with the UID to become terminal
// and returns the document that showed it. A Job is removed once terminal,
// by its TTL or by the controller that owns it, so the document that proved
// terminality is what every caller reads.
func (f *faultRun) waitForExactJobTerminal(name, uid string) *batchv1.Job {
	f.t.Helper()
	var terminal *batchv1.Job
	f.poll("exact Job "+name+" to become terminal", func() bool {
		job := &batchv1.Job{}
		if err := f.get(name, job); err != nil {
			return false
		}
		if string(job.UID) != uid {
			f.fatalf("Job %s changed UID before reaching a terminal state", name)
		}
		if jobTerminal(job) {
			terminal = job
			return true
		}
		return false
	})
	return terminal
}

// waitForOperationJobTerminal waits for the schema's one Job of the operation
// to become terminal, and returns its operation ID, name and UID.
func (f *faultRun) waitForOperationJobTerminal(schema, operation string) (operationID, name, uid string) {
	f.t.Helper()
	f.poll("the terminal "+operation+" Job for "+schema, func() bool {
		jobs := &batchv1.JobList{}
		f.check(f.list(jobs, operationJobs(schema, operation)), "list the %s Jobs of %s", operation, schema)
		if len(jobs.Items) > 1 {
			f.fatalf("%s created more than one %s Job", schema, operation)
		}
		if len(jobs.Items) != 1 || !jobTerminal(&jobs.Items[0]) {
			return false
		}
		job := &jobs.Items[0]
		operationID, name, uid = job.Annotations[annotationOperationID], job.Name, string(job.UID)
		if operationID == "" {
			f.fatalf("%s terminal Job lacks its operation identity", schema)
		}
		return true
	})
	return operationID, name, uid
}
