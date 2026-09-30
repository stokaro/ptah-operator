//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// TestMigrationsPostgreSQL is the migrations-postgresql phase.
func TestMigrationsPostgreSQL(t *testing.T) {
	runMigrationPhase(t, phases.MigrationsPostgreSQL, "postgresql")
}

// TestMigrationsMySQL is the migrations-mysql phase. The transaction-mode row
// runs first, because it says which mode a MySQL sequence may name, and the
// sequence names one.
func TestMigrationsMySQL(t *testing.T) {
	runMigrationPhase(t, phases.MigrationsMySQL, "mysql")
}

func runMigrationPhase(t *testing.T, phase phases.Of[phases.MigrationsInputs], engine string) {
	run, inputs := harness.Begin(t, phase)
	m := newMigrationRun(t, run, inputs, engine)
	scenarios := []struct {
		name string
		body func()
	}{{"migration-policy", m.migrationPolicy}}
	if engine == "mysql" {
		scenarios = append(scenarios,
			struct {
				name string
				body func()
			}{"mysql-transaction-mode", m.transactionModeProof},
			struct {
				name string
				body func()
			}{"mysql-migrations", m.engineMigrations})
	} else {
		scenarios = append(scenarios, struct {
			name string
			body func()
		}{"postgresql-migrations", m.engineMigrations})
	}
	scenarios = append(scenarios,
		struct {
			name string
			body func()
		}{"approval-resource-replacement", m.approvalIdentityReplacement},
		struct {
			name string
			body func()
		}{"approval-policy-change", func() { m.approvalInputChange("policy") }},
		struct {
			name string
			body func()
		}{"approval-transaction-mode-change", func() { m.approvalInputChange("transaction-mode") }},
		struct {
			name string
			body func()
		}{"approval-artifact-change", func() { m.approvalInputChange("artifact") }},
		struct {
			name string
			body func()
		}{"approval-verification-policy-uid-change", func() { m.approvalInputChange("verification-policy-uid") }},
		struct {
			name string
			body func()
		}{"approval-verification-policy-content-change", func() { m.approvalInputChange("verification-policy-content") }},
		struct {
			name string
			body func()
		}{"approval-executor-image-change", m.executorImageChange})
	for _, scenario := range scenarios {
		if !run.Scenario(scenario.name, m.scenario(scenario.body)) {
			return
		}
	}
	run.Logf("e2e migrations: PASS %s approval gate, applied sequence, matching history, and credential isolation",
		m.engine.kind)
}

// migrationRun is what the migration proofs share. Each scenario runs as a
// subtest, and t is that subtest while it runs.
type migrationRun struct {
	t      *testing.T
	parent *testing.T
	ctx    context.Context
	in     phases.MigrationsInputs

	cluster      *harness.Cluster
	engine       migrationEngine
	controller   controllerIdentity
	stateVersion int32
	// rerun is the suffix hack/e2e-rerun-phase.sh gave this run, and
	// repository where it publishes.
	rerun, repository string
	registryHost      string

	workDir     string
	kubectlPtah string
	// password is the data plane's application password for the engine,
	// which every database this phase creates is reached with.
	password string
	patterns []string
	scanner  credentialScanner

	// jobs is every migration Job the phase recorded for the main migration,
	// by UID, as it was first seen.
	jobs map[types.UID]batchv1.Job

	// published is the digest the last publish reported.
	published string
	// The main migration's plan, its UID and its fingerprint.
	plan, planUID, planFingerprint string

	// What the cleanup puts back however the phase ends.
	isolationRulesApplied bool
	egressApplied         bool
	applyGateOpen         bool
	releaseFaultPolicy    string
	rivalNamespace        string
}

func newMigrationRun(t *testing.T, run *harness.Run, in phases.MigrationsInputs, engine string) *migrationRun {
	t.Helper()
	m := &migrationRun{t: t, parent: t, ctx: run.Context(), in: in, jobs: map[types.UID]batchv1.Job{}}
	// Registered before anything is created, so a setup that fails part way
	// still removes what it made.
	t.Cleanup(m.cleanup)
	for _, command := range []string{"kubectl", "go", "docker"} {
		if _, err := exec.LookPath(command); err != nil {
			m.fatalf("required command is not installed: %s", command)
		}
	}
	if err := migrationInputsOK(in.Engine, engine, in.ExecutorImage, in.RunnerImage, in.ControllerImage); err != nil {
		m.fatalf("%v", err)
	}
	if err := validKindCluster(in.KindClusterName); err != nil {
		m.fatalf("%v", err)
	}
	if info, err := os.Stat(in.Kubeconfig); err != nil || !info.Mode().IsRegular() {
		m.fatalf("E2E_KUBECONFIG does not name a file")
	}
	version, err := strconv.ParseInt(in.ControllerStateVersion, 10, 32)
	if err != nil || version <= 0 {
		m.fatalf("E2E_CONTROLLER_STATE_VERSION must be a positive integer")
	}
	m.stateVersion = int32(version)
	m.controller = controllerIdentity{image: in.ControllerImage, revision: in.ControllerRevision, stateVersion: in.ControllerStateVersion}
	if m.engine, err = migrationEngineFor(engine); err != nil {
		m.fatalf("%v", err)
	}
	// The fixtures the rows publish are checked before any cluster work, so
	// a missing one is refused now rather than an hour into the phase.
	for _, suffix := range []string{"", "-branch", "-partial", "-older", "-checkpoint", "-uncertain", "-stopped"} {
		if info, err := os.Stat(m.fixtureDir(suffix)); err != nil || !info.IsDir() {
			m.fatalf("migration fixtures are missing: %s", m.fixtureDir(suffix))
		}
	}
	// E2E_PHASE_RERUN is not a driver input: hack/e2e-rerun-phase.sh sets it
	// when it puts this phase back on a lab an earlier run left behind, and
	// the phase then clears what that run created before it starts.
	m.rerun = os.Getenv("E2E_PHASE_RERUN")
	if m.repository, err = migrationRepository(m.rerun); err != nil {
		m.fatalf("%v", err)
	}
	m.registryHost = in.RegistryService + "." + in.TestNamespace + ".svc.cluster.local:5000"
	if m.cluster, err = harness.Connect(in.Kubeconfig); err != nil {
		m.fatalf("%v", err)
	}
	m.workDir, err = os.MkdirTemp("", "ptah-operator-migrations-e2e.")
	m.check(err, "create the work directory")
	m.check(os.Chmod(m.workDir, 0o700), "make the work directory private")
	// The plan-inspection surface is proved from the same build a user
	// installs, against the live resource, rather than from a golden file.
	m.kubectlPtah = filepath.Join(m.workDir, "kubectl-ptah")
	build := exec.CommandContext(m.ctx, "go", "build", "-trimpath", "-o", m.kubectlPtah, "./cmd/kubectl-ptah") //nolint:gosec // Arguments, not a shell.
	build.Dir = repositoryRoot
	build.Env = append(os.Environ(), "GOCACHE="+filepath.Join(m.workDir, "go-cache"))
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	m.check(build.Run(), "build kubectl-ptah")

	// The password is read back from the Secret the data plane created rather
	// than derived a second time here: a second derivation is a second
	// definition, and the two would part company the moment either moved.
	secret := &corev1.Secret{}
	m.check(m.get(m.engine.sourceSecret, secret), "the data plane %s Secret %s could not be read", engine, m.engine.sourceSecret)
	m.password = string(secret.Data["password"])
	if m.password == "" {
		m.fatalf("%v", errNoPassword)
	}
	m.protect(m.password, m.databaseURL(m.migrationDatabase(), ""))
	return m
}

func (m *migrationRun) scenario(body func()) func(*testing.T) {
	return func(t *testing.T) {
		m.t = t
		defer func() { m.t = m.parent }()
		body()
	}
}

func (m *migrationRun) fatalf(format string, arguments ...any) {
	m.t.Helper()
	m.t.Fatalf("e2e migrations: "+format, arguments...)
}

func (m *migrationRun) logf(format string, arguments ...any) {
	m.t.Helper()
	m.t.Logf("e2e migrations: "+format, arguments...)
}

func (m *migrationRun) check(err error, format string, arguments ...any) {
	m.t.Helper()
	if err != nil {
		m.fatalf("%s: %v", fmt.Sprintf(format, arguments...), err)
	}
}

// sleep pauses between two readings, and ends the scenario when the phase's
// own bound ends first.
func (m *migrationRun) sleep(duration time.Duration) {
	m.t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-m.ctx.Done():
		m.fatalf("the phase's bound ended while it waited: %v", m.ctx.Err())
	case <-timer.C:
	}
}

// poll reads every interval until ready holds, and ends the scenario naming
// what it waited for when waitTimeout passes first.
func (m *migrationRun) poll(description string, interval time.Duration, ready func() bool) {
	m.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		m.sleep(interval)
	}
	m.fatalf("timed out after %s waiting for %s", waitTimeout, description)
}

// protect adds credentials the scanner refuses to find in anything the phase
// reads.
func (m *migrationRun) protect(values ...string) {
	m.t.Helper()
	m.patterns = append(m.patterns, values...)
	scanner, err := newCredentialScanner(m.patterns...)
	m.check(err, "build the credential scanner")
	m.scanner = scanner
}

// scan refuses content that carries a value only an operation Pod should
// hold.
func (m *migrationRun) scan(content []byte, context string) {
	m.t.Helper()
	if !m.scanner.ready() {
		m.fatalf("credential scanner has no non-empty protected patterns")
	}
	if m.scanner.leaks(content) {
		m.fatalf("%s carries a database credential", context)
	}
}

func (m *migrationRun) scanObject(object any, context string) {
	m.t.Helper()
	content, err := json.Marshal(object)
	m.check(err, "encode %T", object)
	m.scan(content, context)
}

func (m *migrationRun) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: m.in.TestNamespace, Name: name}
}

func (m *migrationRun) get(name string, object client.Object) error {
	return m.cluster.Client.Get(m.ctx, m.key(name), object)
}

func (m *migrationRun) list(list client.ObjectList, options ...client.ListOption) error {
	return m.cluster.Client.List(m.ctx, list, append([]client.ListOption{client.InNamespace(m.in.TestNamespace)}, options...)...)
}

// create sends one document as kubectl create -f did, with strict field
// validation, and returns what the API server said.
func (m *migrationRun) create(document map[string]any) error {
	return m.cluster.Client.Create(m.ctx, &unstructured.Unstructured{Object: document},
		client.FieldOwner(harness.FieldOwner), client.FieldValidation("Strict"))
}

func (m *migrationRun) mustCreate(document map[string]any) {
	m.t.Helper()
	m.check(m.create(document), "create %s %s", document["kind"], nameOf(document))
}

// apply sends one document as kubectl apply -f did: created when it is new,
// brought to what it says when it is not.
func (m *migrationRun) apply(document map[string]any) error {
	return m.cluster.Client.Patch(m.ctx, &unstructured.Unstructured{Object: document}, client.Apply, //nolint:staticcheck // The typed Apply needs generated apply configurations the API does not ship.
		client.FieldOwner(harness.FieldOwner), client.ForceOwnership, client.FieldValidation("Strict"))
}

// mergePatch sends a JSON merge patch, as kubectl patch --type=merge did.
func (m *migrationRun) mergePatch(object client.Object, patch map[string]any) error {
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return m.cluster.Client.Patch(m.ctx, object, client.RawPatch(types.MergePatchType, body),
		client.FieldOwner(harness.FieldOwner))
}

// patchMigration merge-patches a migration's document.
func (m *migrationRun) patchMigration(name string, patch map[string]any) {
	m.t.Helper()
	migration := &ptahv1alpha1.PtahMigration{}
	migration.Namespace, migration.Name = m.in.TestNamespace, name
	m.check(m.mergePatch(migration, patch), "patch PtahMigration %s", name)
}

// kubectl runs kubectl against the cluster and returns its standard output
// and standard error.
func (m *migrationRun) kubectl(arguments ...string) (stdout, stderr []byte, err error) {
	return m.cluster.Kubectl(m.ctx, arguments...)
}

// kubectlAs runs kubectl as an identity the administrator impersonates.
func (m *migrationRun) kubectlAs(user, group string, arguments ...string) (stdout, stderr []byte, err error) {
	return m.kubectl(append([]string{"--as", user, "--as-group", group}, arguments...)...)
}

// guardRefused runs a command as an identity and requires the API server to
// refuse it with the text given. A refusal for another reason -- a typo, a
// missing object -- would pass a check that only wanted a failure.
func (m *migrationRun) guardRefused(user, group, text, what string, arguments ...string) {
	m.t.Helper()
	stdout, stderr, err := m.kubectlAs(user, group, arguments...)
	output := append(stdout, stderr...)
	if err == nil {
		m.fatalf("%s; the API server accepted it", what)
	}
	m.scan(output, "the refusal when "+what)
	if !strings.Contains(strings.ToLower(string(output)), strings.ToLower(text)) {
		_, _ = os.Stderr.Write(output)
		m.fatalf("the refusal when %s did not say '%s'", what, text)
	}
}

// migrationDatabase is the main migration's own database: one the schema
// path never touched, so a history starts from nothing.
func (m *migrationRun) migrationDatabase() string { return "ptah_e2e_migrations" }

// migrationName and the names below are the main migration's.
func (m *migrationRun) migrationName() string { return "e2e-migrations-" + m.engine.name }
func (m *migrationRun) migrationSecret() string {
	return "e2e-" + m.engine.name + "-migrations-db"
}
func (m *migrationRun) migrationRealm() string { return "e2e-realm-" + m.engine.name }

// fixtureDir is a migration fixture directory under testdata/e2e/migrations:
// the engine's own, or a variant of it named by suffix ("-branch", "-older").
func (m *migrationRun) fixtureDir(suffix string) string {
	return filepath.Join(repositoryRoot, "testdata", "e2e", "migrations", m.engine.name+suffix)
}

// reference is where an artifact of the engine is published: the engine's
// own repository, or a variant of it named by suffix ("-uncertain").
func (m *migrationRun) reference(suffix string) string {
	return "oci://" + m.registryHost + "/" + m.repository + "/" + m.engine.name + suffix + ":stable"
}

// databaseURL is the URL an operation Pod reaches a database by, as the
// application user or as the user given.
func (m *migrationRun) databaseURL(database, user string) string {
	if user == "" {
		user = migrationDatabaseUser
	}
	return m.engine.databaseURL(m.in.TestNamespace, user, m.password, database)
}

// sqlStatement runs one statement on the engine's server, as its own
// administrator, in the database given, and returns what the client printed.
// Use it where the phase guards a statement rather than comparing its output.
func (m *migrationRun) sqlStatement(database, statement string) (string, error) {
	return databaseSQL(m.ctx, m.cluster, m.in.TestNamespace, m.engine, database, statement)
}

// query is sql_value: one statement for what it printed, with the whitespace
// the two clients pad a value with removed. A failed exec reads as whatever it
// printed, which a caller catches by comparing the value it did not get. An
// empty database is the main migration's.
func (m *migrationRun) query(statement, database string) string {
	if database == "" {
		database = m.migrationDatabase()
	}
	output, _ := m.sqlStatement(database, statement)
	return trimmedSQL(output)
}

// widgetColumnCount asks the catalog whether e2e_migration_widgets carries a
// column, in the main migration's database or the one given.
func (m *migrationRun) widgetColumnCount(column, database string) string {
	schema := "table_schema = current_schema()"
	if m.engine.name == "mysql" {
		schema = "table_schema = database()"
	}
	return m.query("SELECT count(*) FROM information_schema.columns WHERE "+schema+
		" AND table_name = 'e2e_migration_widgets' AND column_name = '"+column+"'", database)
}

// serverStatement runs one statement against the server's own database as
// its administrator, for the statements that create and drop databases.
func (m *migrationRun) serverStatement(statement string) (string, error) {
	return serverSQL(m.ctx, m.cluster, m.in.TestNamespace, m.engine, statement)
}

// databaseExists is the count of databases with the name on the server.
func (m *migrationRun) databaseExists(name string) string {
	m.t.Helper()
	statement := "SELECT count(*) FROM pg_database WHERE datname='" + name + "'"
	if m.engine.name == "mysql" {
		statement = "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='" + name + "'"
	}
	output, err := m.serverStatement(statement)
	m.check(err, "look database %s up on %s", name, m.engine.name)
	return trimmedSQL(output)
}

// createDatabase creates a database no earlier run created. On MySQL the
// application user owns nothing by default and grants are per schema, so the
// grant belongs to creating it.
func (m *migrationRun) createDatabase(name string) {
	m.t.Helper()
	if m.databaseExists(name) != "0" {
		m.fatalf("database %s already exists on %s; rerun this phase through hack/e2e-rerun-phase.sh, which clears it",
			name, m.engine.name)
	}
	statement := "CREATE DATABASE " + name
	if m.engine.name == "mysql" {
		statement = "CREATE DATABASE " + name + "; GRANT ALL PRIVILEGES ON " + name + ".* TO '" +
			migrationDatabaseUser + "'@'%'; FLUSH PRIVILEGES"
	}
	_, err := m.serverStatement(statement)
	m.check(err, "database %s could not be created", name)
}

// dropDatabase is the rerun's half of createDatabase. FORCE on PostgreSQL ends
// the sessions an interrupted run left open; without it the drop waits on
// them.
func (m *migrationRun) dropDatabase(name string) {
	m.t.Helper()
	statement := "DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"
	if m.engine.name == "mysql" {
		statement = "DROP DATABASE IF EXISTS " + name
	}
	_, err := m.serverStatement(statement)
	m.check(err, "database %s could not be dropped on %s", name, m.engine.name)
}

// createDatabaseSecret stores the credential a migration reaches a database
// with: immutable, applied rather than created so a rerun against a retained
// cluster reaches its proof.
func (m *migrationRun) createDatabaseSecret(name, database, url string) {
	m.t.Helper()
	m.check(m.apply(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "immutable": true, "type": "Opaque",
		"metadata": map[string]any{"namespace": m.in.TestNamespace, "name": name},
		"data": secretData(map[string]string{
			"username": migrationDatabaseUser, "password": m.password, "database": database, "url": url,
		}),
	}), "apply Secret %s", name)
}

// isolatedDatabase creates a database of its own for a proof, protects its
// URL, and stores it in the proof's Secret. It returns the URL.
func (m *migrationRun) isolatedDatabase(database, secret string) string {
	m.t.Helper()
	m.createDatabase(database)
	url := m.databaseURL(database, "")
	m.protect(url)
	m.createDatabaseSecret(secret, database, url)
	return url
}

// migration reads a migration and scans it: its status carries no row and no
// SQL by contract, and no credential either.
func (m *migrationRun) migration(name string) *ptahv1alpha1.PtahMigration {
	m.t.Helper()
	migration := &ptahv1alpha1.PtahMigration{}
	m.check(m.get(name, migration), "%s could not be read", name)
	m.scanObject(migration, name+" status")
	return migration
}

// waitForMigration reads the migration every interval until match holds,
// and returns the document that satisfied it.
func (m *migrationRun) waitForMigration(name, description string, interval time.Duration,
	match func(*ptahv1alpha1.PtahMigration) bool,
) *ptahv1alpha1.PtahMigration {
	m.t.Helper()
	var matched, last *ptahv1alpha1.PtahMigration
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		migration := &ptahv1alpha1.PtahMigration{}
		if err := m.get(name, migration); err == nil {
			m.scanObject(migration, name+" status")
			last = migration
			if match(migration) {
				matched = migration
				return matched
			}
		}
		m.sleep(interval)
	}
	m.reportMigration(last)
	m.fatalf("%s did not reach %s within %s", name, description, waitTimeout)
	return nil
}

// reportMigration prints what the last document a wait read held: the phase,
// the operation and the conditions, which carry no row and no SQL.
func (m *migrationRun) reportMigration(migration *ptahv1alpha1.PtahMigration) {
	if migration == nil {
		return
	}
	status := migration.Status
	report := map[string]any{
		"name": migration.Name, "generation": migration.Generation,
		"observedGeneration": status.ObservedGeneration, "phase": status.Phase,
		"activeOperation": status.ActiveOperation, "plan": status.Plan,
		"unresolvedRun": status.UnresolvedRun != nil, "conditions": conditionSummary(status.Conditions),
	}
	content, err := json.Marshal(report)
	if err != nil || m.scanner.leaks(content) {
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: %s when the wait ended: %s\n", migration.Name, content)
}

// waitForPhase waits for the main migration to reach a phase, archiving its
// Jobs at every reading. A migration that fails is not waited out unless
// Failed is what the wait is for.
func (m *migrationRun) waitForPhase(phase ptahv1alpha1.MigrationPhase) {
	m.t.Helper()
	name, observed := m.migrationName(), ptahv1alpha1.MigrationPhase("")
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		m.recordJobs()
		migration := &ptahv1alpha1.PtahMigration{}
		observed = ""
		if m.get(name, migration) == nil {
			observed = migration.Status.Phase
		}
		if observed == phase {
			m.recordJobs()
			return
		}
		if observed == ptahv1alpha1.MigrationPhaseFailed && phase != ptahv1alpha1.MigrationPhaseFailed {
			m.fatalf("%s failed while waiting for %s", name, phase)
		}
		m.sleep(migrationPoll)
	}
	m.fatalf("%s did not reach %s within %s; it is in %s", name, phase, waitTimeout, cmpOrNone(string(observed)))
}

// waitForGenerationInSync waits for the migration to finish a cycle for the
// spec it holds now, and returns the document that showed it: InSync for the
// current generation, with no operation in flight. A phase read alone would
// pass on the InSync the resource held before the edit it waits on.
func (m *migrationRun) waitForGenerationInSync(name string) *ptahv1alpha1.PtahMigration {
	m.t.Helper()
	return m.waitForMigration(name, "a finished cycle for its current generation", migrationPoll,
		func(migration *ptahv1alpha1.PtahMigration) bool {
			return migration.Status.ObservedGeneration == migration.Generation &&
				migration.Status.Phase == ptahv1alpha1.MigrationPhaseInSync && migration.Status.ActiveOperation == nil
		})
}

func cmpOrNone(value string) string {
	if value == "" {
		return "<none>"
	}
	return value
}

// recordJobs archives every Job the controller created for the main
// migration so far. The controller stamps a TTL on a Job it finished reading,
// and a whole lifecycle outlasts it, so the record is taken while each Job
// still exists, keyed by UID, the first reading of each kept.
func (m *migrationRun) recordJobs() {
	m.t.Helper()
	jobs := &batchv1.JobList{}
	if m.list(jobs, client.MatchingLabels{labelMigration: m.migrationName()}) != nil {
		return
	}
	for _, job := range jobs.Items {
		if _, found := m.jobs[job.UID]; !found {
			m.jobs[job.UID] = job
		}
	}
}

// jobInventory is the archived Jobs, one per UID, ordered by UID.
func (m *migrationRun) jobInventory() []batchv1.Job {
	uids := make([]string, 0, len(m.jobs))
	for uid := range m.jobs {
		uids = append(uids, string(uid))
	}
	slices.Sort(uids)
	inventory := make([]batchv1.Job, 0, len(uids))
	for _, uid := range uids {
		inventory = append(inventory, m.jobs[types.UID(uid)])
	}
	return inventory
}

// applyJobUIDs is the Apply Jobs a migration has dispatched, by UID, sorted.
// Identities rather than a count: a finished Job carries a TTL, and a count
// that fell by one deletion and rose by one dispatch is the same count.
func (m *migrationRun) applyJobUIDs(migration string) []string {
	m.t.Helper()
	jobs := &batchv1.JobList{}
	// One failed read is not a finding about the operator: a hold that reads
	// every few seconds would otherwise fail on a transient API error. A read
	// that keeps failing is.
	var err error
	for attempt := range 5 {
		if err = m.list(jobs, client.MatchingLabels{labelMigration: migration, labelOperation: "apply"}); err == nil {
			break
		}
		if attempt < 4 {
			m.sleep(2 * time.Second)
		}
	}
	m.check(err, "list the Apply Jobs of %s", migration)
	uids := make([]string, 0, len(jobs.Items))
	for _, job := range jobs.Items {
		uids = append(uids, string(job.UID))
	}
	slices.Sort(uids)
	return uids
}

// assertNoNewApplyJob fails when an Apply Job appears that the recorded set
// does not name. A Job that went away meanwhile is not a finding: the TTL
// removes them.
func (m *migrationRun) assertNoNewApplyJob(recorded []string, description, migration string) {
	m.t.Helper()
	for _, uid := range m.applyJobUIDs(migration) {
		if !slices.Contains(recorded, uid) {
			m.fatalf("%s dispatched another run %s", migration, description)
		}
	}
}

// migrationSpec is one PtahMigration a proof creates. The zero values are the
// ones most proofs use.
type migrationSpec struct {
	name, secret, reference string
	// realm names a PtahRealm; coordinationKey is used when it is empty.
	realm, coordinationKey string
	// apply is the apply policy, or empty to leave the default.
	apply string
	// lockTimeout is 30s, and interval the phase's when empty.
	lockTimeout, interval string
	// execution replaces the default execution settings when not nil.
	execution map[string]any
	// edit changes the document's spec before it is sent.
	edit func(spec map[string]any)
}

// migrationDocument is the PtahMigration a proof declares, against the
// phase's verification policy and registry credential.
func (m *migrationRun) migrationDocument(spec migrationSpec) map[string]any {
	target := map[string]any{"engine": m.engine.kind, "urlFrom": map[string]any{"name": spec.secret, "key": "url"}}
	if spec.realm != "" {
		target["realmRef"] = map[string]any{"name": spec.realm}
	} else {
		target["coordinationKey"] = spec.coordinationKey
	}
	policy := map[string]any{"lockTimeout": cmpOrDefault(spec.lockTimeout, "30s")}
	if spec.apply != "" {
		policy["apply"] = spec.apply
	}
	execution := spec.execution
	if execution == nil {
		execution = map[string]any{"activeDeadlineSeconds": int64(300), "failureRetryInterval": "10s", "connectTimeout": "30s"}
	}
	document := map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahMigration",
		"metadata": map[string]any{"namespace": m.in.TestNamespace, "name": spec.name},
		"spec": map[string]any{
			"target": target,
			"artifact": map[string]any{
				"ociRef": spec.reference,
				"registryAuthFrom": map[string]any{
					"name": registryAuthSecret, "mode": "Environment",
					"usernameKey": "username", "passwordKey": "password",
				},
				"verificationPolicyFrom": map[string]any{"name": migrationPolicy, "key": migrationPolicyKey},
				"transport":              map[string]any{"plainHTTP": true},
			},
			"policy":    policy,
			"interval":  cmpOrDefault(spec.interval, migrationInterval),
			"execution": execution,
		},
	}
	if spec.edit != nil {
		spec.edit(document["spec"].(map[string]any))
	}
	return document
}

func cmpOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// approvalDocument is an approval of one plan of one migration, as a person
// writes it.
func migrationApprovalDocument(namespace, name, migration, migrationUID, plan, planUID, fingerprint string) map[string]any {
	return map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahMigrationApproval",
		"metadata": map[string]any{"namespace": namespace, "name": name},
		"spec": map[string]any{
			"migrationRef":    map[string]any{"name": migration, "uid": migrationUID},
			"planRef":         map[string]any{"name": plan, "uid": planUID},
			"planFingerprint": fingerprint,
		},
	}
}

// approve creates an approval of the plan for the migration and returns what
// the API server said: a refusal is the claim of some proofs.
func (m *migrationRun) approve(name, migration, plan, planUID, fingerprint string) error {
	m.t.Helper()
	current := &ptahv1alpha1.PtahMigration{}
	m.check(m.get(migration, current), "read %s", migration)
	if current.UID == "" {
		m.fatalf("%s has no UID", migration)
	}
	return m.create(migrationApprovalDocument(m.in.TestNamespace, name, migration, string(current.UID), plan, planUID, fingerprint))
}

// planOf reads a migration plan and scans it.
func (m *migrationRun) planOf(name string) *ptahv1alpha1.PtahMigrationPlan {
	m.t.Helper()
	plan := &ptahv1alpha1.PtahMigrationPlan{}
	m.check(m.get(name, plan), "migration plan %s could not be read", name)
	m.scanObject(plan, "migration plan "+name)
	return plan
}

// openApplyGate puts the gate label on every node: a Pod whose nodeSelector
// names it schedules at once.
func (m *migrationRun) openApplyGate() {
	m.t.Helper()
	m.applyGateOpen = true
	m.check(m.setNodeLabel(m.ctx, applyGateLabel, "open"), "the apply gate could not be opened")
}

// closeApplyGate takes the label off every node. Closing is what the rest of
// a proof rests on, and the scheduler reads the labels rather than this
// call's result, so the check is that no node carries it any more.
func (m *migrationRun) closeApplyGate() {
	m.t.Helper()
	m.check(m.setNodeLabel(m.ctx, applyGateLabel, ""), "the apply gate could not be closed")
	nodes := &corev1.NodeList{}
	m.check(m.cluster.Client.List(m.ctx, nodes, client.HasLabels{applyGateLabel}), "the nodes carrying the apply gate could not be listed")
	if len(nodes.Items) != 0 {
		var names []string
		for _, node := range nodes.Items {
			names = append(names, node.Name)
		}
		m.fatalf("the apply gate is still open on: %s", strings.Join(names, " "))
	}
	// Only now: a gate that did not close is still the cleanup's to close.
	m.applyGateOpen = false
}

// setNodeLabel sets a label on every node, or removes it when value is empty.
func (m *migrationRun) setNodeLabel(ctx context.Context, key, value string) error {
	nodes := &corev1.NodeList{}
	if err := m.cluster.Client.List(ctx, nodes); err != nil {
		return err
	}
	for index := range nodes.Items {
		name := nodes.Items[index].Name
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			node := &corev1.Node{}
			if err := m.cluster.Client.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
				return err
			}
			if value == "" {
				if _, found := node.Labels[key]; !found {
					return nil
				}
				delete(node.Labels, key)
			} else {
				if node.Labels == nil {
					node.Labels = map[string]string{}
				}
				node.Labels[key] = value
			}
			return m.cluster.Client.Update(ctx, node, client.FieldOwner(harness.FieldOwner))
		})
		if err != nil {
			return fmt.Errorf("node %s: %w", name, err)
		}
	}
	return nil
}

// kubectlPtahView runs the plugin a reader inspects a migration with.
func (m *migrationRun) kubectlPtahView(migration string) []byte {
	m.t.Helper()
	command := exec.CommandContext(m.ctx, m.kubectlPtah, "migration", migration, //nolint:gosec // The phase's own build.
		"--kubeconfig", m.in.Kubeconfig, "-n", m.in.TestNamespace)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		m.fatalf("kubectl ptah migration could not read %s: %v", migration, err)
	}
	m.scan(stdout.Bytes(), "the kubectl ptah migration view")
	return stdout.Bytes()
}

// migrationPolicy applies the verification policy the phase's migrations
// name, after an earlier run's leftovers are cleared on a rerun.
func (m *migrationRun) migrationPolicy() {
	m.t.Helper()
	m.resetAfterAnEarlierRun()
	content, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", "verification-policy-migrations.yaml"))
	m.check(err, "migration verification policy fixture is missing")
	// Applied rather than created, so a rerun against a retained cluster
	// reaches its own proof. The object is immutable, so an apply either
	// writes it once or changes nothing.
	m.check(m.apply(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "immutable": true,
		"metadata": map[string]any{"namespace": m.in.TestNamespace, "name": migrationPolicy},
		"data":     map[string]any{migrationPolicyKey: string(content)},
	}), "apply ConfigMap %s", migrationPolicy)
	if !bytes.Contains(content, []byte(migrationArtifactType)) {
		m.fatalf("the migration verification policy does not pin the migration artifact type")
	}
}

// cleanup puts back what the phase changed however it ends: the isolation
// rules, the egress policies, the apply gate, the release fault and the rival
// namespace, and on a failure it prints what the migrations, their plans and
// their Jobs were doing. Job logs are not printed: a credential-isolation
// failure would put a database URL in them.
func (m *migrationRun) cleanup() {
	t := m.parent
	m.t = t
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	m.ctx = ctx
	if m.isolationRulesApplied {
		if err := m.removeIsolationRules(ctx); err != nil {
			t.Errorf("e2e migrations: could not remove the isolation rules: %v", err)
		}
		m.isolationRulesApplied = false
	}
	if m.cluster != nil {
		if m.egressApplied {
			if err := m.cluster.Client.DeleteAllOf(ctx, &networkingv1.NetworkPolicy{}, client.InNamespace(m.in.TestNamespace),
				client.MatchingLabels{"operator.ptah.run/e2e-proof": "egress"}); err != nil {
				t.Errorf("e2e migrations: could not remove the egress policies: %v", err)
			}
			m.egressApplied = false
		}
		if m.applyGateOpen {
			if err := m.setNodeLabel(ctx, applyGateLabel, ""); err != nil {
				t.Errorf("e2e migrations: could not close the apply gate: %v", err)
			}
			m.applyGateOpen = false
		}
		if m.releaseFaultPolicy != "" {
			binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			binding.Name = m.releaseFaultPolicy
			policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
			policy.Name = m.releaseFaultPolicy
			for _, object := range []client.Object{binding, policy} {
				if err := m.cluster.Client.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
					t.Errorf("e2e migrations: could not remove the release fault %T: %v", object, err)
				}
			}
			m.releaseFaultPolicy = ""
		}
		if m.rivalNamespace != "" {
			namespace := &corev1.Namespace{}
			namespace.Name = m.rivalNamespace
			if err := m.cluster.Client.Delete(ctx, namespace); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("e2e migrations: could not remove %s: %v", m.rivalNamespace, err)
			}
			m.rivalNamespace = ""
		}
		if t.Failed() {
			m.collectDiagnostics(ctx)
		}
	}
	if m.workDir != "" {
		if !strings.HasPrefix(filepath.Base(m.workDir), "ptah-operator-migrations-e2e.") {
			t.Errorf("e2e migrations: refusing to remove unexpected work directory %s", m.workDir)
			return
		}
		if err := os.RemoveAll(m.workDir); err != nil {
			t.Errorf("e2e migrations: remove the work directory: %v", err)
		}
	}
}

// collectDiagnostics prints the migrations' status, which carries no row and
// no SQL by contract; the plans' versions, checksums and order, by the same
// contract; and the migration Jobs' labels and status. Each is scanned and
// withheld on a match.
func (m *migrationRun) collectDiagnostics(ctx context.Context) {
	_, _ = fmt.Fprintln(os.Stderr, "e2e migrations: collecting failure diagnostics")
	emit := func(what string, value any) {
		content, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return
		}
		if m.scanner.ready() && m.scanner.leaks(content) {
			_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: the %s are withheld: they matched a protected credential\n", what)
			return
		}
		_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: %s:\n%s\n", what, content)
	}
	migrations := &ptahv1alpha1.PtahMigrationList{}
	if m.cluster.Client.List(ctx, migrations, client.InNamespace(m.in.TestNamespace)) == nil {
		var projection []any
		for _, migration := range migrations.Items {
			projection = append(projection, map[string]any{"name": migration.Name, "status": migration.Status})
		}
		emit("migrations", projection)
	}
	plans := &ptahv1alpha1.PtahMigrationPlanList{}
	if m.cluster.Client.List(ctx, plans, client.InNamespace(m.in.TestNamespace)) == nil {
		var projection []any
		for _, plan := range plans.Items {
			var migrations []any
			for _, planned := range plan.Spec.Migrations {
				migrations = append(migrations, map[string]any{
					"version": planned.Version, "description": planned.Description, "checkpoint": planned.Checkpoint,
				})
			}
			projection = append(projection, map[string]any{
				"name": plan.Name, "currentVersion": plan.Spec.CurrentVersion, "migrations": migrations,
			})
		}
		emit("migration plans", projection)
	}
	jobs := &batchv1.JobList{}
	if m.cluster.Client.List(ctx, jobs, client.InNamespace(m.in.TestNamespace),
		client.MatchingLabels{labelComponent: migrationOperationComponent}) == nil {
		var projection []any
		for _, job := range jobs.Items {
			projection = append(projection, map[string]any{"name": job.Name, "labels": job.Labels, "status": job.Status})
		}
		emit("migration Jobs", projection)
	}
	_, _ = fmt.Fprintln(os.Stderr, "e2e migrations: raw Job logs are suppressed to protect credential-isolation failures")
}

// deleteAndWait deletes an object and waits until it is gone, as kubectl
// delete --wait=true did. The propagation is kubectl's too: in the
// background, where the API's own default for a Job orphans its Pods.
func (m *migrationRun) deleteAndWait(object client.Object, description string) {
	m.t.Helper()
	if err := m.cluster.Client.Delete(m.ctx, object, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil &&
		!apierrors.IsNotFound(err) {
		m.fatalf("%s was not removed: %v", description, err)
	}
	key := client.ObjectKeyFromObject(object)
	m.poll(description+" to be removed", time.Second, func() bool {
		current := object.DeepCopyObject().(client.Object)
		err := m.cluster.Client.Get(m.ctx, key, current)
		return apierrors.IsNotFound(err) || (err == nil && current.GetUID() != object.GetUID() && object.GetUID() != "")
	})
}
