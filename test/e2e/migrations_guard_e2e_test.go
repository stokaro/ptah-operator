//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// guardRow is the names the apply-policy guard row uses for one engine: its
// migration and database, the author and approver it grants roles to, and
// the administrator it admits by the exempt group alone.
type guardRow struct {
	migration, database, secret, coordinationKey string
	administrator                                string
	author, authorGroup                          string
	approver, approverGroup                      string
	authorRole, approverRole                     string
}

func (m *migrationRun) guardRow() guardRow {
	engine := m.engine.name
	return guardRow{
		migration: "e2e-apply-guard-" + engine, database: "ptah_e2e_apply_guard",
		secret: "e2e-" + engine + "-apply-guard-db", coordinationKey: "e2e/apply-guard/" + engine,
		administrator: "e2e-apply-policy-administrator-" + engine,
		author:        "e2e-author-" + engine, authorGroup: "e2e:desired-state-authors-" + engine,
		approver: "e2e-approver-" + engine, approverGroup: "e2e:migration-approvers-" + engine,
		authorRole: "e2e-desired-state-author-" + engine, approverRole: "e2e-migration-approver-" + engine,
	}
}

// applyPolicyGuardProof holds the chart's installed apply-policy guard to the
// three people it separates. The author creates a migration and may neither
// approve it nor select Always, whether by editing it or by creating it that
// way; the approver approves it and may not edit it; the administrator, a
// name the cluster has never seen carrying the exempt group alone, selects
// Always, and an author's later edit that leaves it there is admitted, as is
// the author moving back to OnApproval.
func (m *migrationRun) applyPolicyGuardProof() {
	m.t.Helper()
	g := m.guardRow()
	m.logf("holding the installed apply-policy guard to the %s author, approver and administrator", m.engine.kind)
	m.isolatedDatabase(g.database, g.secret)
	m.grantGuardRoles(g)
	administratorGroup := m.readApplyPolicyGuard()

	// Created by the author, so the author's Role is what lets it in.
	m.guardAuthorCreate(g)
	awaiting := m.waitForGuard(g.migration, "a plan awaiting approval", guardPlanAwaiting)
	plan := awaiting.Status.Plan.Name
	m.guardRefused(g.author, g.authorGroup, "forbidden", "the author approved its own migration",
		"create", "-f", m.guardApprovalFile(g, plan, g.migration+"-by-author"))

	m.guardRefused(g.author, g.authorGroup, "reserves that choice", "the author selected Always",
		"-n", m.in.TestNamespace, "patch", "ptahmigration", g.migration, "--type=merge",
		"--patch", `{"spec":{"policy":{"apply":"Always"}}}`)
	if m.migration(g.migration).Spec.Policy.Apply != ptahv1alpha1.ApplyPolicyOnApproval {
		m.fatalf("%s left OnApproval although the author's change was refused", g.migration)
	}
	if len(m.applyJobUIDs(g.migration)) != 0 {
		m.fatalf("%s ran an Apply before anybody approved it", g.migration)
	}
	// A guard that watched only updates is bypassed by creating the resource
	// with Always already set. A server-side dry run asks the API server the
	// question and leaves nothing behind when the answer is the refusal.
	unattended := g.migration + "-unattended"
	m.guardRefused(g.author, g.authorGroup, "reserves that choice",
		"the author created a migration with Always already set",
		"create", "--dry-run=server", "-f", m.guardDocumentFile(unattended, m.guardMigrationDocument(g, unattended, "Always")))

	m.guardAs(g.approver, g.approverGroup, fmt.Sprintf("the approver could not approve %s", g.migration),
		"create", "-f", m.guardApprovalFile(g, plan, g.migration+"-by-approver"))
	approval := &ptahv1alpha1.PtahMigrationApproval{}
	if err := m.get(g.migration+"-by-approver", approval); err != nil {
		m.fatalf("the approver's approval of %s could not be read: %v", g.migration, err)
	}
	if !approvalNamesApprover(approval, g.approver, g.approverGroup) {
		m.fatalf("the approval of %s does not name the approver who made it", g.migration)
	}
	m.waitForGuard(g.migration, "the approved plan applied", guardPlanApplied)
	m.guardRefused(g.approver, g.approverGroup, "forbidden", "the approver edited the migration",
		"-n", m.in.TestNamespace, "patch", "ptahmigration", g.migration, "--type=merge",
		"--patch", `{"spec":{"interval":"2h"}}`)

	// The administrator is a name the cluster has never seen, carrying the
	// exempt group and nothing else: what admits the change is the group.
	m.guardAs(g.administrator, administratorGroup,
		fmt.Sprintf("the guard refused %s, a member of the exempt group %s, selecting Always", g.administrator, administratorGroup),
		"-n", m.in.TestNamespace, "patch", "ptahmigration", g.migration, "--type=merge",
		"--patch", `{"spec":{"policy":{"apply":"Always"}}}`)
	if m.migration(g.migration).Spec.Policy.Apply != ptahv1alpha1.ApplyPolicyAlways {
		m.fatalf("%s does not read Always after the administrator selected it", g.migration)
	}
	m.guardAs(g.author, g.authorGroup, "the guard refused an author's edit that left Always where an administrator put it",
		"-n", m.in.TestNamespace, "patch", "ptahmigration", g.migration, "--type=merge",
		"--patch", `{"spec":{"interval":"2h"}}`)
	// The generation the author's edit produced, so the convergence below is
	// the operator's answer to that edit and not the state before it.
	edited := &ptahv1alpha1.PtahMigration{}
	if err := m.get(g.migration, edited); err != nil || edited.Generation <= 0 {
		m.fatalf("%s carries no generation after the author's edit", g.migration)
	}
	generation := edited.Generation
	m.waitForGuard(g.migration, "the edited resource converged with Always in place",
		func(migration *ptahv1alpha1.PtahMigration) bool {
			return guardConvergedUnderAlways(migration, generation)
		})
	m.guardAs(g.author, g.authorGroup, fmt.Sprintf("the guard refused an author moving %s back to OnApproval", g.migration),
		"-n", m.in.TestNamespace, "patch", "ptahmigration", g.migration, "--type=merge",
		"--patch", `{"spec":{"policy":{"apply":"OnApproval"}}}`)

	migration := &ptahv1alpha1.PtahMigration{}
	migration.Namespace, migration.Name = m.in.TestNamespace, g.migration
	m.deleteAndWait(migration, g.migration)
	m.retireFixtureApproval(approval.Name, approval.Spec.MigrationRef.UID)
	for _, name := range []string{g.authorRole, g.approverRole} {
		binding := &rbacv1.RoleBinding{}
		binding.Namespace, binding.Name = m.in.TestNamespace, name
		role := &rbacv1.Role{}
		role.Namespace, role.Name = m.in.TestNamespace, name
		for _, object := range []client.Object{binding, role} {
			_ = m.cluster.Client.Delete(m.ctx, object)
		}
	}
	m.logf("PASS %s author refused Always and approval, approver applied, administrator chose Always", m.engine.kind)
}

// guardAuthorCreate retries the first authorized write while the API server's
// RBAC cache observes the new grant. Retry only a definite RBAC refusal: an
// admission refusal, transport error, or ambiguous write result ends the row.
// A separate access review could reach a different API server from the write.
func (m *migrationRun) guardAuthorCreate(g guardRow) {
	m.t.Helper()
	author, err := m.cluster.As(rest.ImpersonationConfig{UserName: g.author, Groups: []string{g.authorGroup}})
	m.check(err, "build the desired-state author's client")
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	err = harness.Wait(ctx, "the author's first migration creation after its RoleBinding", 30*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			object := &unstructured.Unstructured{Object: m.guardMigrationDocument(g, g.migration, "OnApproval")}
			err := author.Create(ctx, object, client.FieldOwner(harness.FieldOwner))
			if err == nil {
				return true, "", nil
			}
			m.scan([]byte(err.Error()), "the author's first migration creation")
			if apierrors.IsForbidden(err) && guardAuthorGrantPending(err.Error(), g.author, m.in.TestNamespace) {
				return false, "the desired-state author's grant is not effective yet", nil
			}
			return false, "", err
		})
	m.check(err, "the author could not create %s with the desired-state Role", g.migration)
}

// guardAs runs kubectl as an identity and ends the scenario with the failure
// given when the API server refuses. What it said is scanned before it is
// printed.
func (m *migrationRun) guardAs(user, group, failure string, arguments ...string) {
	m.t.Helper()
	_, stderr, err := m.kubectlAs(user, group, arguments...)
	if err != nil {
		m.scan(stderr, "the refusal of "+user)
		m.fatalf("%s: %s", failure, strings.TrimSpace(string(stderr)))
	}
}

// guardDocumentFile writes a document for kubectl -f to read. The documents
// the guard row sends carry no credential.
func (m *migrationRun) guardDocumentFile(name string, document map[string]any) string {
	m.t.Helper()
	content, err := json.Marshal(document)
	m.check(err, "encode %s", name)
	path := filepath.Join(m.workDir, "guard-"+name+".json")
	m.check(os.WriteFile(path, content, 0o600), "write %s", path)
	return path
}

// guardMigrationDocument is a migration on the row's own database and the
// engine's artifact, with the apply policy named.
func (m *migrationRun) guardMigrationDocument(g guardRow, name, apply string) map[string]any {
	return m.migrationDocument(migrationSpec{
		name: name, secret: g.secret, reference: m.reference(""), coordinationKey: g.coordinationKey,
		apply: apply, interval: "1h",
	})
}

// guardApprovalFile is an approval of the guard migration's current plan,
// written for whoever is asked to create it.
func (m *migrationRun) guardApprovalFile(g guardRow, plan, name string) string {
	m.t.Helper()
	migration := &ptahv1alpha1.PtahMigration{}
	planObject := &ptahv1alpha1.PtahMigrationPlan{}
	if m.get(g.migration, migration) != nil || m.get(plan, planObject) != nil ||
		migration.UID == "" || planObject.UID == "" || planObject.Spec.Fingerprint == "" {
		m.fatalf("%s or its plan %s carries no identity to approve", g.migration, plan)
	}
	return m.guardDocumentFile(name, migrationApprovalDocument(m.in.TestNamespace, name, g.migration,
		string(migration.UID), plan, string(planObject.UID), planObject.Spec.Fingerprint))
}

// waitForGuard reads the guard migration every five seconds until match
// holds, and returns the document that satisfied it. A wait that runs out
// prints the policy, the interval, the phase and the conditions.
func (m *migrationRun) waitForGuard(name, what string, match func(*ptahv1alpha1.PtahMigration) bool) *ptahv1alpha1.PtahMigration {
	m.t.Helper()
	var last *ptahv1alpha1.PtahMigration
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		last = m.migration(name)
		if match(last) {
			return last
		}
		m.sleep(migrationPoll)
	}
	projection, err := json.Marshal(map[string]any{
		"spec":       map[string]any{"policy": last.Spec.Policy, "interval": last.Spec.Interval},
		"phase":      last.Status.Phase,
		"conditions": conditionSummary(last.Status.Conditions),
	})
	if err == nil && !m.scanner.leaks(projection) {
		_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: %s\n", projection)
	}
	m.fatalf("%s did not reach %s within %s", name, what, waitTimeout)
	return nil
}

// grantGuardRoles installs the author's Role, the example's with its
// namespace, name and group replaced, and the approver's, written here.
func (m *migrationRun) grantGuardRoles(g guardRow) {
	m.t.Helper()
	content, err := os.ReadFile(filepath.Join(repositoryRoot, "examples", "desired-state-author-role.yaml"))
	if err != nil {
		m.fatalf("the desired-state author example could not be read: %v", err)
	}
	documents, err := decodeManifests(content)
	if err != nil {
		m.fatalf("the desired-state author example could not be read: %v", err)
	}
	author, err := guardAuthorRole(documents, m.in.TestNamespace, m.engine.name, g.authorGroup)
	if err != nil {
		m.fatalf("the desired-state author example is not the Role and RoleBinding this row adapts: %v", err)
	}
	for _, document := range author {
		if err := m.apply(document); err != nil {
			m.fatalf("the desired-state author Role could not be installed: %v", err)
		}
	}
	for _, document := range guardApproverRole(m.in.TestNamespace, g.approverRole, g.approverGroup) {
		if err := m.apply(document); err != nil {
			m.fatalf("the migration approver Role could not be installed: %v", err)
		}
	}
}

// readApplyPolicyGuard reads the release's own guard and the group this
// row's administrator carries. The bootstrap read the harness identity's
// groups from the API server into the release values; this reads the same
// answer and then checks that the installed policy names it, so admitting the
// administrator below is a fact about the value the chart rendered rather than
// about a group the guard never saw.
func (m *migrationRun) readApplyPolicyGuard() string {
	m.t.Helper()
	review, err := m.cluster.Clientset.AuthenticationV1().SelfSubjectReviews().Create(m.ctx,
		&authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		m.fatalf("the harness identity could not be read: %v", err)
	}
	group, err := guardAdministratorGroup(review.Status.UserInfo.Groups)
	if err != nil {
		m.fatalf("%v", err)
	}
	selector := client.MatchingLabels{"app.kubernetes.io/component": "apply-policy-guard"}
	policies := &admissionregistrationv1.ValidatingAdmissionPolicyList{}
	bindings := &admissionregistrationv1.ValidatingAdmissionPolicyBindingList{}
	if err := m.cluster.Client.List(m.ctx, policies, selector); err != nil {
		m.fatalf("the installed apply-policy guard could not be read: %v", err)
	}
	if err := m.cluster.Client.List(m.ctx, bindings, selector); err != nil {
		m.fatalf("the installed apply-policy guard could not be read: %v", err)
	}
	if err := applyPolicyGuardInstalled(policies.Items, bindings.Items, group); err != nil {
		m.fatalf("the release does not install one apply-policy guard, bound to Deny, that exempts %s: %v", group, err)
	}
	return group
}

// adoptRow is the names the adoption row uses for one engine.
type adoptRow struct {
	migration, database, shadowDatabase, shadowUser, secret, coordinationKey string
	reference, configMap, baselineJob                                        string
}

func (m *migrationRun) adoptRow() adoptRow {
	engine := m.engine.name
	return adoptRow{
		migration: "e2e-adopt-" + engine, database: "ptah_e2e_adopt", shadowDatabase: "ptah_e2e_adopt_shadow",
		shadowUser: "ptah_e2e_shadow", secret: "e2e-" + engine + "-adopt-db", coordinationKey: "e2e/adopt/" + engine,
		reference: m.reference("-adopt"), configMap: "e2e-migrations-" + engine + "-adopt",
		baselineJob: "e2e-adopt-baseline-" + engine,
	}
}

// existingSchemaAdoptionProof is the matrix row "existing schema with an
// empty revision table": no implicit bootstrap or baseline, and an adoption
// path a person can take. Ptah reports every migration pending on a database
// with an empty revision table whether or not it already carries the schema,
// so the operator cannot tell the two apart and must not guess: it neither
// records the migrations as applied nor calls the database up to date. The
// adoption is a person's, run with Ptah's own baseline against a disposable
// shadow database.
func (m *migrationRun) existingSchemaAdoptionProof() {
	m.t.Helper()
	a := m.adoptRow()
	// Recorded before the resource exists, so the set compared against is
	// the empty one. status.lastRun is the durable half of the same
	// statement: an Apply that ran and was collected leaves no Job, where the
	// run it recorded never expires.
	var applies []string
	m.createAdoptDatabases(a)
	m.buildAdoptSchemaWithoutTheOperator(a)
	// The adoption row reads the artifact this engine already publishes: a
	// second copy of the same three migrations would be a second thing to keep
	// in step with the schema the proof builds out of them by hand.
	m.publish("adopt", m.fixtureDir(""), a.reference)
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: a.migration, secret: a.secret, reference: a.reference, coordinationKey: a.coordinationKey,
		// This row waits for a history refresh after manual adoption; it does
		// not measure the main lifecycle's five-minute refresh interval.
		interval: "1m",
	}))
	held := m.waitForMigration(a.migration, string(ptahv1alpha1.MigrationPhaseAwaitingApproval), migrationPoll,
		func(migration *ptahv1alpha1.PtahMigration) bool {
			return migration.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval
		})
	// The row itself. What must not happen is a recorded revision, not a
	// created table: the read that establishes the history is also what
	// creates the table, so demanding its absence would measure Ptah's reader
	// and report it as an adoption the operator never performed.
	if !existingSchemaHeld(held) {
		m.fatalf("%s did not hold the whole sequence at the approval gate", a.migration)
	}
	if m.adoptRecordedRevisions(a) != "0" {
		m.fatalf("the operator recorded a migration as applied in a database it was never approved to migrate")
	}
	m.assertNoNewApplyJob(applies, "against a database that already carries the schema", a.migration)
	m.logf("%s held an existing schema at the approval gate and recorded nothing", m.engine.kind)
	m.runAdoptionBaseline(a)
	m.assertAdoptedHistoryMatches(a, applies)
	m.finishFixture(a.migration)
	m.logf("PASS %s refuses to adopt an existing schema, and settles once a person does", m.engine.kind)
}

// createAdoptDatabases creates the database the schema is built in by hand,
// and the shadow database baseline replays the migrations into to compare the
// schema they produce with the one the target already has. Without it Ptah
// falls back to reading Go entities from a working copy an executor image
// does not carry, and refuses.
func (m *migrationRun) createAdoptDatabases(a adoptRow) {
	m.t.Helper()
	m.createDatabase(a.database)
	m.createDatabase(a.shadowDatabase)
	shadowUser := migrationDatabaseUser
	if m.engine.name == "mysql" {
		m.createMySQLShadowUser(a)
		shadowUser = a.shadowUser
	}
	url := m.databaseURL(a.database, "")
	shadowURL := m.databaseURL(a.shadowDatabase, shadowUser)
	m.protect(url, shadowURL)
	m.check(m.apply(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "immutable": true, "type": "Opaque",
		"metadata": map[string]any{"namespace": m.in.TestNamespace, "name": a.secret},
		"data": secretData(map[string]string{
			"username": migrationDatabaseUser, "password": m.password, "database": a.database,
			"url": url, "shadowUrl": shadowURL,
		}),
	}), "apply Secret %s", a.secret)
}

// createMySQLShadowUser gives the MySQL shadow database its own user. Before
// baseline replays the migrations into the shadow database, Ptah empties it,
// and on MySQL it refuses to drop objects unless the user holds the global
// SELECT, DROP, ALTER, ALTER ROUTINE, EVENT, LOCK TABLES, PROCESS, SHOW_ROUTINE
// and TRIGGER privileges. The Jobs' own user keeps its per-schema grants. The
// password is the one the database container already holds, so no
// credential crosses the exec.
func (m *migrationRun) createMySQLShadowUser(a adoptRow) {
	m.t.Helper()
	script := `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -e "
			DROP USER IF EXISTS '$1'@'%';
			CREATE USER '$1'@'%' IDENTIFIED BY '$MYSQL_PASSWORD';
			GRANT SELECT, DROP, ALTER, ALTER ROUTINE, EVENT, LOCK TABLES, PROCESS, SHOW_ROUTINE, TRIGGER
				ON *.* TO '$1'@'%';
			GRANT ALL PRIVILEGES ON $2.* TO '$1'@'%';
			FLUSH PRIVILEGES"`
	if _, _, err := m.kubectl("-n", m.in.TestNamespace, "exec", "deployment/"+m.engine.service, "--",
		"sh", "-ec", script, "sh", a.shadowUser, a.shadowDatabase); err != nil {
		m.fatalf("the MySQL shadow user %s could not be created", a.shadowUser)
	}
}

// buildAdoptSchemaWithoutTheOperator replays the artifact's own migration SQL
// into a database the operator has never seen. The SQL comes from the
// fixtures rather than a copy written here, since the row is about a database
// whose schema already matches that artifact exactly.
func (m *migrationRun) buildAdoptSchemaWithoutTheOperator(a adoptRow) {
	m.t.Helper()
	files, err := filepath.Glob(filepath.Join(m.fixtureDir(""), "*.up.sql"))
	if err != nil || len(files) == 0 {
		m.fatalf("migration fixtures are missing: %s", m.fixtureDir(""))
	}
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			m.fatalf("migration fixtures are missing: %s", m.fixtureDir(""))
		}
		command := []string{"sh", "-ec",
			`PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -v ON_ERROR_STOP=1 -qc "$2"`,
			"sh", a.database, strings.TrimRight(string(content), "\n")}
		if m.engine.name == "mysql" {
			command = []string{"sh", "-ec",
				`MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1" -e "$2"`,
				"sh", a.database, strings.TrimRight(string(content), "\n")}
		}
		if _, _, err := m.kubectl(append([]string{"-n", m.in.TestNamespace, "exec", "deployment/" + m.engine.service, "--"}, command...)...); err != nil {
			m.fatalf("%s could not be replayed into %s", filepath.Base(file), a.database)
		}
	}
	if len(files) != 3 {
		m.fatalf("the adoption proof replayed %d migrations, and the artifact carries three", len(files))
	}
	if m.query("SELECT count(*) FROM e2e_migration_widgets", a.database) != "3" {
		m.fatalf("the hand-built schema in %s does not carry the rows its migrations insert", a.database)
	}
	if m.adoptRevisionTables(a) != "0" {
		m.fatalf("the hand-built schema in %s already carries a revision table", a.database)
	}
}

// adoptRevisionTables counts the revision table Ptah records history in. It
// separates a database nothing has touched from one a history read has
// reached, which is not the same question as whether anything was adopted.
func (m *migrationRun) adoptRevisionTables(a adoptRow) string {
	statement := "SELECT count(*) FROM information_schema.tables WHERE table_name = 'schema_migrations'"
	if m.engine.name == "mysql" {
		statement = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = '" + a.database +
			"' AND table_name = 'schema_migrations'"
	}
	return m.query(statement, a.database)
}

// adoptRecordedRevisions counts what the revision table records. Reading the
// history is what creates the table, so its presence says a read happened and
// nothing else. Adoption is a row in it, a version recorded as applied that
// nothing ran, and a table that is not there answers zero the same way an
// empty one does.
func (m *migrationRun) adoptRecordedRevisions(a adoptRow) string {
	if m.adoptRevisionTables(a) == "0" {
		return "0"
	}
	return m.query("SELECT count(*) FROM schema_migrations", a.database)
}

// runAdoptionBaseline is the path the refusal leaves open, run the way a
// person runs it: Ptah's own baseline, verified against a shadow database
// before a single row is recorded. Nothing in the operator takes part, and
// the Job holds a database credential and no registry credential.
func (m *migrationRun) runAdoptionBaseline(a adoptRow) {
	m.t.Helper()
	entries, err := os.ReadDir(m.fixtureDir(""))
	m.check(err, "read %s", m.fixtureDir(""))
	var files []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	slices.Sort(files)
	if len(files) == 0 {
		m.fatalf("no migration files to baseline from %s", m.fixtureDir(""))
	}
	var mounts []any
	for _, file := range files {
		mounts = append(mounts, map[string]any{"name": "migrations", "mountPath": "/migrations/" + file, "subPath": file, "readOnly": true})
	}
	mounts = append(mounts, map[string]any{"name": "work", "mountPath": "/work"})
	secretURL := func(name, key string) map[string]any {
		return map[string]any{"name": name, "valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": a.secret, "key": key},
		}}
	}
	labels := map[string]any{"app.kubernetes.io/component": "e2e-migration-adopter"}
	m.check(m.create(map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"namespace": m.in.TestNamespace, "name": a.baselineJob, "labels": labels},
		"spec": map[string]any{
			"backoffLimit": int64(0), "activeDeadlineSeconds": int64(300),
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"restartPolicy": "Never", "automountServiceAccountToken": false,
					"imagePullSecrets": []any{map[string]any{"name": registryPullSecret}},
					"securityContext": map[string]any{
						"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532), "fsGroup": int64(65532),
						"seccompProfile": map[string]any{"type": "RuntimeDefault"},
					},
					"containers": []any{map[string]any{
						"name": "adopter", "image": m.in.ExecutorImage, "imagePullPolicy": "IfNotPresent",
						"command": []any{"/usr/local/bin/ptah"},
						"args": []any{
							"migrations", "baseline", "--migrations-dir", "/migrations",
							"--dir-format", "ptah",
							"--db-url", "$(PTAH_E2E_TARGET_URL)",
							"--shadow-db", "$(PTAH_E2E_SHADOW_URL)",
						},
						"env": []any{
							map[string]any{"name": "HOME", "value": "/work"},
							map[string]any{"name": "TMPDIR", "value": "/work"},
							secretURL("PTAH_E2E_TARGET_URL", "url"),
							secretURL("PTAH_E2E_SHADOW_URL", "shadowUrl"),
						},
						"securityContext": map[string]any{
							"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
							"capabilities": map[string]any{"drop": []any{"ALL"}},
						},
						"volumeMounts": mounts,
					}},
					"volumes": []any{
						map[string]any{"name": "migrations", "configMap": map[string]any{"name": a.configMap}},
						map[string]any{"name": "work", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
					},
				},
			},
		},
	}), "create Job %s", a.baselineJob)
	// The adopter reaches the database and must reach no registry: the mirror
	// of the boundary the publisher keeps, read off the object that was
	// created rather than off the text that asked for it.
	job := &batchv1.Job{}
	m.check(m.get(a.baselineJob, job), "read Job %s", a.baselineJob)
	if !adopterIsolated(job, a.secret) {
		m.fatalf("the adoption Job did not keep registry access out of the process that runs SQL")
	}
	m.poll("the adoption baseline Job to finish", 3*time.Second, func() bool {
		current := &batchv1.Job{}
		if m.get(a.baselineJob, current) != nil {
			return false
		}
		if current.Status.Succeeded > 0 {
			return true
		}
		if current.Status.Failed > 0 {
			// The adopter holds a database URL, so its words are printed only
			// through the scanner that refuses one.
			logs := m.adopterLog(current)
			m.scan(logs, "the migration adopter log")
			_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: the adopter said:\n%s\n", logs)
			m.fatalf("the baseline a person runs did not record the existing schema")
		}
		return false
	})
}

// adopterLog reads what the adopter printed, or nothing when its Pod cannot
// be read: a failure diagnostic that fails itself would hide the failure.
func (m *migrationRun) adopterLog(job *batchv1.Job) []byte {
	pods := &corev1.PodList{}
	if m.list(pods, client.MatchingLabels{"job-name": job.Name}) != nil {
		return nil
	}
	owned := ownedPods(pods.Items, job.UID)
	if len(owned) == 0 {
		return nil
	}
	logs, _ := m.cluster.ContainerLog(m.ctx, m.in.TestNamespace, owned[0].Name, "adopter")
	return logs
}

// assertAdoptedHistoryMatches closes the row: once a person has recorded the
// history, the operator settles on it without running anything, and the rows
// the database already held are still the rows it holds.
func (m *migrationRun) assertAdoptedHistoryMatches(a adoptRow, applies []string) {
	m.t.Helper()
	if m.adoptRevisionTables(a) != "1" {
		m.fatalf("the baseline recorded no revision table in %s", a.database)
	}
	if m.query("SELECT count(*) FROM schema_migrations", a.database) != "3" {
		m.fatalf("the baseline did not record every migration the artifact carries")
	}
	settled := m.waitForMigration(a.migration, string(ptahv1alpha1.MigrationPhaseInSync), migrationPoll,
		func(migration *ptahv1alpha1.PtahMigration) bool {
			return migration.Status.Phase == ptahv1alpha1.MigrationPhaseInSync
		})
	if !adoptedHistorySettled(settled) {
		m.fatalf("%s did not settle on the history a person recorded", a.migration)
	}
	m.assertNoNewApplyJob(applies, "after a person adopted the database", a.migration)
	if m.query("SELECT color FROM e2e_migration_widgets WHERE id = 1", a.database) != "blue" {
		m.fatalf("the adopted database lost the rows its hand-built schema carried")
	}
}

// checkpointBootstrapProof is the checkpoint row. A checkpoint carries the
// schema and the rows its predecessors produce, so a database that has run
// nothing starts from it instead of replaying them, and has to end up
// indistinguishable from one that did. That equivalence is the claim, and it
// is checked against the database that took the long way, on the engine's own
// catalog rather than on a schema dump, which would be a third opinion about
// what the two hold.
func (m *migrationRun) checkpointBootstrapProof() {
	m.t.Helper()
	engine := m.engine.name
	name, database := "e2e-checkpoint-"+engine, "ptah_e2e_checkpoint"
	secret, approval := "e2e-"+engine+"-checkpoint-db", "e2e-checkpoint-"+engine+"-approval"
	reference := m.reference("-checkpoint")
	m.isolatedDatabase(database, secret)
	m.publish("checkpoint", m.fixtureDir("-checkpoint"), reference)
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: reference, coordinationKey: "e2e/checkpoint/" + engine,
	}))
	gate := m.waitForMigration(name, string(ptahv1alpha1.MigrationPhaseAwaitingApproval), migrationPoll,
		func(migration *ptahv1alpha1.PtahMigration) bool {
			return migration.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval
		})
	// The gate says what the checkpoint changed: the two migrations it
	// carries are accounted for rather than pending, and what a person is
	// asked to approve is the checkpoint and the migration after it.
	if !checkpointGate(gate) || gate.Status.Plan == nil || gate.Status.Plan.Name == "" {
		m.fatalf("%s did not hold a checkpoint bootstrap at the approval gate", name)
	}
	plan := m.planOf(gate.Status.Plan.Name)
	if !checkpointPlanVersions(plan) {
		m.fatalf("the %s checkpoint plan is not the checkpoint and the migration after it", engine)
	}
	m.logf("%s starts a fresh database at checkpoint 3, with 1 and 2 accounted for", m.engine.kind)
	if plan.UID == "" || plan.Spec.Fingerprint == "" {
		m.fatalf("checkpoint plan %s has no UID or fingerprint", plan.Name)
	}
	m.check(m.approve(approval, name, plan.Name, string(plan.UID), plan.Spec.Fingerprint), "approve %s", plan.Name)
	settled := m.waitForMigration(name, string(ptahv1alpha1.MigrationPhaseInSync), migrationPoll,
		func(migration *ptahv1alpha1.PtahMigration) bool {
			return migration.Status.Phase == ptahv1alpha1.MigrationPhaseInSync
		})
	m.assertCheckpointEqualsTheLongWay(name, database, settled)
	m.patchMigration(name, map[string]any{"spec": map[string]any{"interval": "30s"}})
	m.assertCheckpointBootstrapStaysSettled(name)
	m.finishFixture(name)
	m.logf("PASS %s bootstrapped from a checkpoint and matches the database that replayed everything", m.engine.kind)
}

// assertCheckpointEqualsTheLongWay is the row itself: what the bootstrapped
// database holds is what the replayed one holds. The checkpoint stays in the
// reading afterwards, because it goes on describing what it replaced.
func (m *migrationRun) assertCheckpointEqualsTheLongWay(name, database string, settled *ptahv1alpha1.PtahMigration) {
	m.t.Helper()
	if !checkpointSettled(settled) {
		m.fatalf("%s did not settle on the history its bootstrap produced", name)
	}
	engine := m.engine.name
	columns, replayedColumns := m.checkpointColumnShape(database), m.checkpointColumnShape(m.migrationDatabase())
	if columns == "" {
		m.fatalf("the bootstrapped %s database has no table to compare", engine)
	}
	if columns != replayedColumns {
		m.fatalf("the bootstrapped %s schema is [%s], and the replayed one is [%s]", engine, columns, replayedColumns)
	}
	rows, replayedRows := m.checkpointRowShape(database), m.checkpointRowShape(m.migrationDatabase())
	if rows == "" {
		m.fatalf("the bootstrapped %s database carries none of the rows its checkpoint seeds", engine)
	}
	if rows != replayedRows {
		m.fatalf("the bootstrapped %s rows are [%s], and the replayed ones are [%s]", engine, rows, replayedRows)
	}
	// The migration after the checkpoint depends on the data the checkpoint
	// seeded, so a bootstrap that skipped the seeding would leave this row
	// unrecolored rather than fail outright.
	if m.query("SELECT color FROM e2e_migration_widgets WHERE id = 1", database) != "blue" {
		m.fatalf("the %s migration after the checkpoint did not run against the rows the checkpoint seeded", engine)
	}
}

// assertCheckpointBootstrapStaysSettled holds a second reading to changing
// nothing. The covered migrations report themselves pending once the
// bootstrap is behind the database, and a resource that recounted them would
// ask for an approval to run them on every pass. What is watched is the plan
// and the approval, not the phase: a settled resource still resolves,
// verifies and reads at its interval, so it is legitimately out of InSync for
// part of every cycle.
func (m *migrationRun) assertCheckpointBootstrapStaysSettled(name string) {
	m.t.Helper()
	before := m.migration(name)
	if before.Status.History == nil {
		m.fatalf("%s carries no history observation to compare against", name)
	}
	observed := instantOf(before.Status.History.ObservedAt)
	var reread *ptahv1alpha1.PtahMigration
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		current := m.migration(name)
		if !checkpointNotReapproving(current) {
			m.fatalf("%s asked for another approval after its bootstrap settled", name)
		}
		if current.Status.History == nil {
			m.fatalf("%s lost its history observation", name)
		}
		if instantOf(current.Status.History.ObservedAt) != observed && current.Status.Phase == ptahv1alpha1.MigrationPhaseInSync {
			reread = current
			break
		}
		m.sleep(migrationPoll)
	}
	if reread == nil {
		m.fatalf("%s did not read its history again within %s", name, waitTimeout)
	}
	if !checkpointResettled(reread) {
		m.fatalf("%s did not settle again on the history its bootstrap produced", name)
	}
}

// checkpointColumnShape is every column of e2e_migration_widgets with its
// type and nullability, ordered by name.
func (m *migrationRun) checkpointColumnShape(database string) string {
	statement := `SELECT string_agg(column_name || '/' || data_type || '/' || is_nullable, ','
                       ORDER BY column_name)
                     FROM information_schema.columns
                     WHERE table_schema = current_schema()
                       AND table_name = 'e2e_migration_widgets'`
	if m.engine.name == "mysql" {
		statement = `SELECT GROUP_CONCAT(CONCAT(column_name, '/', data_type, '/', is_nullable)
                       ORDER BY column_name SEPARATOR ',')
                     FROM information_schema.columns
                     WHERE table_schema = database()
                       AND table_name = 'e2e_migration_widgets'`
	}
	return m.query(statement, database)
}

// checkpointRowShape is every row of e2e_migration_widgets, ordered by id.
func (m *migrationRun) checkpointRowShape(database string) string {
	statement := `SELECT string_agg(id || '/' || name || '/' || color, ',' ORDER BY id)
                     FROM e2e_migration_widgets`
	if m.engine.name == "mysql" {
		statement = `SELECT GROUP_CONCAT(CONCAT(id, '/', name, '/', color)
                       ORDER BY id SEPARATOR ',')
                     FROM e2e_migration_widgets`
	}
	return m.query(statement, database)
}
