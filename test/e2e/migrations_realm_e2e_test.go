//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// The rival the realm row names the migration's realm from: a PtahSchema in a
// namespace of its own, created by an author holding the example author Role
// there and nothing else -- the principal #445 names, who can create a
// PtahSchema there and has no Secret and no database.
func (m *migrationRun) rivalNamespaceName() string { return "e2e-realm-rival-" + m.engine.name }
func (m *migrationRun) rivalSchemaName() string    { return "e2e-migrations-" + m.engine.name + "-rival" }
func (m *migrationRun) rivalAuthor() string        { return "e2e-realm-rival-author-" + m.engine.name }
func (m *migrationRun) rivalAuthorGroup() string   { return "e2e:realm-rival-authors-" + m.engine.name }

// printRealmDiagnostic prints a diagnostic the phase already reads, once the scanner
// has cleared it, and withholds it otherwise.
func (m *migrationRun) printRealmDiagnostic(what string, value any) {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return
	}
	if !m.scanner.ready() || m.scanner.leaks(content) {
		_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: %s is withheld: it matched a protected credential\n", what)
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: %s:\n%s\n", what, content)
}

// rivalSchema reads the rival from its own namespace.
func (m *migrationRun) rivalSchema() *ptahv1alpha1.PtahSchema {
	m.t.Helper()
	schema := &ptahv1alpha1.PtahSchema{}
	if err := m.cluster.Client.Get(m.ctx, types.NamespacedName{Namespace: m.rivalNamespaceName(), Name: m.rivalSchemaName()}, schema); err != nil {
		m.fatalf("%s could not be read: %v", m.rivalSchemaName(), err)
	}
	return schema
}

// realmNamespaces patches the migration's realm to list the namespaces given.
func (m *migrationRun) realmNamespaces(namespaces ...string) error {
	realm := &ptahv1alpha1.PtahRealm{}
	realm.Name = m.migrationRealm()
	listed := make([]any, 0, len(namespaces))
	for _, namespace := range namespaces {
		listed = append(listed, namespace)
	}
	return m.mergePatch(realm, map[string]any{"spec": map[string]any{"namespaces": listed}})
}

// grantRivalAuthorRole installs the example author Role in the rival
// namespace, bound to the rival's author group, and waits until the author may
// create a PtahSchema there: a new RoleBinding reaches each API server's
// authorizer through its own watch, so the first request after the apply can
// still be refused, and the rows that follow measure the operator and not RBAC
// propagation.
func (m *migrationRun) grantRivalAuthorRole() {
	m.t.Helper()
	example, err := os.ReadFile(filepath.Join(repositoryRoot, "examples", "desired-state-author-role.yaml"))
	if err != nil {
		m.fatalf("the desired-state author example could not be read")
	}
	objects, err := rivalAuthorGrant(example, m.rivalNamespaceName(), m.rivalAuthorGroup())
	if err != nil {
		m.fatalf("the desired-state author example is not the Role and RoleBinding this row adapts: %v", err)
	}
	for _, object := range objects {
		if err := m.apply(object); err != nil {
			m.fatalf("the desired-state author Role could not be installed in %s: %v", m.rivalNamespaceName(), err)
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		stdout, _, _ := m.kubectlAs(m.rivalAuthor(), m.rivalAuthorGroup(), "auth", "can-i", "create",
			"ptahschemas.operator.ptah.run", "-n", m.rivalNamespaceName())
		if trimmedSQL(string(stdout)) == "yes" {
			return
		}
		if !time.Now().Before(deadline) {
			m.fatalf("the desired-state author Role did not take effect in %s within 60s", m.rivalNamespaceName())
		}
		m.sleep(time.Second)
	}
}

// realmAdmitsOnlyListedClaimants is the ownership row of the matrix, and the
// authority one: who may claim a database. #45 named the combination -- a
// PtahSchema and a PtahMigration claiming one database -- and #445 the attack
// inside it: a claim anybody could make by writing a string.
//
// The migration names a PtahRealm that lists this namespace alone. A PtahSchema
// in another namespace names the same realm. The realm does not list that
// namespace, so the rival is refused itself, before it resolves anything, and
// it is not counted against the migration: the migration keeps running, which
// the row shows by giving it a new generation and watching it read the
// database again while the rival stands refused.
//
// Then the administrator lists the rival's namespace. Both are claimants now,
// the realm admits one at a time, and both are refused -- the conflict a realm
// exists to decide, across namespaces. Suspending the rival hands the database
// back without anybody editing the migration. The rival's Secrets are never
// created there, because every refusal comes before it reads any.
func (m *migrationRun) realmAdmitsOnlyListedClaimants() {
	m.t.Helper()
	rivalNamespace, rivalName := m.rivalNamespaceName(), m.rivalSchemaName()
	migration := m.migrationName()
	m.logf("naming the %s migration realm from %s, which it does not list", m.engine.kind, rivalNamespace)
	namespace := &corev1.Namespace{}
	namespace.Name = rivalNamespace
	m.check(m.cluster.Client.Create(m.ctx, namespace, client.FieldOwner(harness.FieldOwner)), "create namespace %s", rivalNamespace)
	m.rivalNamespace = rivalNamespace
	m.grantRivalAuthorRole()

	author, err := m.cluster.As(rest.ImpersonationConfig{UserName: m.rivalAuthor(), Groups: []string{m.rivalAuthorGroup()}})
	m.check(err, "build a client for the rival's author")
	rival := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": rivalNamespace, "name": rivalName},
		"spec": map[string]any{
			"target": map[string]any{
				"engine": m.engine.kind, "realmRef": map[string]any{"name": m.migrationRealm()},
				"urlFrom": map[string]any{"name": m.migrationSecret(), "key": "url"},
			},
			"desired": map[string]any{
				"ociRef": "oci://example.invalid/schema:v1",
				"registryAuthFrom": map[string]any{
					"name": registryAuthSecret, "mode": "Environment",
					"usernameKey": "username", "passwordKey": "password",
				},
				"verificationPolicyFrom": map[string]any{"name": migrationPolicy, "key": migrationPolicyKey},
				"transport":              map[string]any{"plainHTTP": true},
			},
			"interval":  "1h",
			"execution": map[string]any{"activeDeadlineSeconds": int64(300)},
		},
	}}
	if err := author.Create(m.ctx, rival, client.FieldOwner(harness.FieldOwner), client.FieldValidation("Strict")); err != nil {
		m.fatalf("the rival's author could not create a PtahSchema in its own namespace: %v", err)
	}
	// Naming a realm is all an author can do with one. Writing the grant that
	// would admit the namespace is the administrator's, and the API server
	// refuses it to the author whatever the realm says.
	widen, err := json.Marshal(map[string]any{"spec": map[string]any{"namespaces": []string{m.in.TestNamespace, rivalNamespace}}})
	m.check(err, "encode the realm patch")
	m.guardRefused(m.rivalAuthor(), m.rivalAuthorGroup(), "forbidden",
		"the rival's author listed its own namespace in "+m.migrationRealm(),
		"patch", "ptahrealm", m.migrationRealm(), "--type=merge", "--patch", string(widen))
	rivalRealm, err := json.Marshal(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahRealm",
		"metadata": map[string]any{"name": "e2e-realm-rival-" + m.engine.name},
		"spec":     map[string]any{"engine": m.engine.kind, "namespaces": []string{rivalNamespace}, "sharing": "Shared"},
	})
	m.check(err, "encode the rival's realm")
	rivalRealmFile := filepath.Join(m.workDir, "rival-realm.json")
	m.check(os.WriteFile(rivalRealmFile, rivalRealm, 0o600), "write the rival's realm")
	m.guardRefused(m.rivalAuthor(), m.rivalAuthorGroup(), "forbidden",
		"the rival's author created a PtahRealm of its own", "create", "-f", rivalRealmFile)

	// The rival is refused for the realm and for nothing else. The document
	// that matched is the one asserted below, and its own transition time is
	// what dates the refusal.
	var refused *ptahv1alpha1.PtahSchema
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		current := m.rivalSchema()
		if realmNotAuthorized(current.Status) {
			refused = current
			break
		}
		m.sleep(migrationPoll)
	}
	if refused == nil {
		m.printRealmDiagnostic(rivalName+" status", m.rivalSchema().Status)
		m.fatalf("%s was never refused for naming a realm that does not list %s within %s", rivalName, rivalNamespace, waitTimeout)
	}
	m.scanObject(refused, "the rival's realm refusal")
	// Refused as a whole -- nothing claimed, nothing approvable -- and told
	// nothing about who the realm does admit. The rival reads the realm it
	// asked for and its own namespace, and not the migration's.
	if !rivalRefusedWhole(refused.Status, m.in.TestNamespace, migration) {
		m.printRealmDiagnostic(rivalName+" conditions", refused.Status.Conditions)
		m.fatalf("%s was not refused whole, or its refusal named the claimant the realm admits", rivalName)
	}
	refusedAt, found := readyTransition(refused.Status.Conditions)
	if !found {
		m.fatalf("%s's refusal carries no transition time to date it by", rivalName)
	}

	// The migration keeps running. A new generation makes it resolve, verify
	// and read the database again, and the reading it records is dated after
	// the rival was refused -- by the migration's own timestamp, not by the
	// poll that noticed. The lock timeout is a value nothing below depends on,
	// and it is put back at the end of the row.
	m.patchMigration(migration, map[string]any{"spec": map[string]any{"policy": map[string]any{"lockTimeout": "45s"}}})
	running := m.waitForGenerationInSync(migration)
	if !migrationKeptRunning(running.Status, refusedAt) {
		m.printRealmDiagnostic(migration+" history and conditions", map[string]any{
			"history": running.Status.History, "conditions": running.Status.Conditions,
		})
		m.fatalf("%s stopped running while a namespace the realm does not list claimed it (refused at %s)",
			migration, refusedAt.UTC().Format(time.RFC3339))
	}
	// And the rival is still refused after the migration ran: it re-examines
	// its claim on a bounded cadence, and nothing it saw since admitted it.
	if !rivalStillRefused(m.rivalSchema().Status) {
		m.fatalf("%s was admitted to a realm that still does not list its namespace", rivalName)
	}
	m.logf("PASS %s refused a claim the realm does not grant, and kept the granted one running", m.engine.kind)

	// Now the administrator lists the rival's namespace. The grant is what
	// changed, and it wakes both claimants.
	m.logf("listing %s in the %s realm, which admits one claimant at a time", rivalNamespace, m.engine.kind)
	if err := m.realmNamespaces(m.in.TestNamespace, rivalNamespace); err != nil {
		m.fatalf("the PtahRealm %s could not be widened: %v", m.migrationRealm(), err)
	}

	// Both claimants, not only the newcomer: a refusal that blocked one side
	// would leave the other free to keep changing the database. The two
	// documents that matched are the ones asserted.
	var conflict *ptahv1alpha1.PtahMigration
	var rivalConflict *ptahv1alpha1.PtahSchema
	deadline = time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		current, currentRival := m.migration(migration), m.rivalSchema()
		conflict, rivalConflict = current, currentRival
		if migrationRealmConflict(current.Status) && rivalRealmConflict(currentRival.Status) {
			break
		}
		conflict = nil
		m.sleep(migrationPoll)
	}
	if conflict == nil {
		m.printRealmDiagnostic("the claimants' conditions", map[string]any{
			migration: m.migration(migration).Status.Conditions, rivalName: m.rivalSchema().Status.Conditions,
		})
		m.fatalf("the two claimants of %s were not both refused once the realm listed both namespaces", m.migrationRealm())
	}
	if !migrationConflictHeld(conflict.Status) {
		m.fatalf("%s kept managing a database a PtahSchema also claims", migration)
	}
	// The refusal names counts and the realm, and no other namespace's
	// objects. Each side is read for the other's namespace and name: the
	// claimants are different tenants. The census writes into the condition
	// messages, so that is where the other claimant is looked for; the rest of
	// the status carries image references this harness serves from the test
	// namespace.
	m.scanObject(conflict, "the realm refusal")
	if !conditionMessagesAvoid(conflict.Status.Conditions, rivalNamespace, rivalName) {
		m.fatalf("the realm refusal published the other claimant")
	}
	m.scanObject(rivalConflict, "the rival's realm refusal")
	if !rivalConflictHeld(rivalConflict.Status, m.in.TestNamespace, migration) {
		m.printRealmDiagnostic(rivalName+" conditions", rivalConflict.Status.Conditions)
		m.fatalf("%s was allowed to manage a database a PtahMigration also claims, or its refusal published the other claimant", rivalName)
	}

	// Every refusal precedes the first claim, so the rival never resolved its
	// reference and never created a Job, in its own namespace or any other.
	jobs := &batchv1.JobList{}
	m.check(m.cluster.Client.List(m.ctx, jobs, client.MatchingLabels{labelSchema: rivalName}), "list the rival's Jobs")
	if len(jobs.Items) != 0 {
		m.fatalf("%s dispatched a Job for a database it may not manage", rivalName)
	}

	// Suspending a claimant ends the conflict, and the survivor is not edited
	// to make that happen: a resource that runs nothing claims nothing, which
	// is how one database is handed to one manager without declaring anything
	// shared.
	suspended := &ptahv1alpha1.PtahSchema{}
	suspended.Namespace, suspended.Name = rivalNamespace, rivalName
	m.check(m.mergePatch(suspended, map[string]any{"spec": map[string]any{"suspend": true}}), "suspend %s", rivalName)
	m.waitForPhase(ptahv1alpha1.MigrationPhaseInSync)
	m.deleteAndWait(namespace, rivalNamespace)
	m.rivalNamespace = ""

	// Back to what the rows after this one were written against: a realm that
	// lists this namespace, and the migration's own lock timeout.
	if err := m.realmNamespaces(m.in.TestNamespace); err != nil {
		m.fatalf("the PtahRealm %s could not be narrowed back: %v", m.migrationRealm(), err)
	}
	m.patchMigration(migration, map[string]any{"spec": map[string]any{"policy": map[string]any{"lockTimeout": "30s"}}})
	m.waitForGenerationInSync(migration)
	m.logf("PASS %s realm refusal and recovery", m.engine.kind)
}

// partialRunBlocksAndRecovers is the partial-migration row of the matrix: a
// migration that committed some of its statements and not the rest.
//
// The fourth file opts out of the per-migration transaction, which makes the
// case real rather than arranged: it adds a column, then fails, and the column
// stays. Ptah records the revision as not applied with the statement count it
// reached, and the operator stops there: re-running a file that committed half
// of itself would run that half twice, and nothing reading the revision row
// can know which half. The recovery is a person's. Here the decision is to
// undo the half and take the migration out of the sequence, and nothing about
// the resource is edited to make that land.
func (m *migrationRun) partialRunBlocksAndRecovers() {
	m.t.Helper()
	migration := m.migrationName()
	m.logf("moving the %s tag to an artifact whose fourth migration commits half of itself", m.engine.kind)
	m.publish("v3", m.fixtureDir("-partial"), m.reference(""))
	m.waitForPhase(ptahv1alpha1.MigrationPhaseAwaitingApproval)
	planned := m.migration(migration)
	if planned.Status.Plan == nil {
		m.fatalf("%s published no plan for the partial migration", migration)
	}
	if !partialPlanned(planned.Status) {
		m.fatalf("%s did not plan the fourth migration alone", migration)
	}
	plan := m.planOf(planned.Status.Plan.Name)
	if plan.UID == "" || plan.Spec.Fingerprint == "" {
		m.fatalf("migration plan %s has no UID or fingerprint", plan.Name)
	}
	m.check(m.approve("e2e-migrations-"+m.engine.name+"-partial-approval", migration, plan.Name, string(plan.UID),
		plan.Spec.Fingerprint), "approve %s", plan.Name)

	m.waitForPhase(ptahv1alpha1.MigrationPhaseBlocked)
	stopped := m.migration(migration)
	// Two readings, and neither depends on where the resource is in its
	// cycle: what the run recorded, and that the refusal stands. The reason
	// the refusal carries depends on whether the next history read has
	// landed, and both are the same refusal, so neither is worth racing.
	if !partialRunRecorded(stopped.Status) {
		m.fatalf("%s did not stop on a migration that committed half of itself", migration)
	}
	if !blockedRefusalHeld(stopped.Status) {
		m.fatalf("%s did not refuse after a migration that committed half of itself", migration)
	}
	m.scanObject(stopped, "the partial-run refusal")

	// Partial is a fact about the database, not a label the run chose: the
	// first statement is committed and the revision says the file never
	// finished.
	if m.widgetColumnCount("weight", "") != "1" {
		m.fatalf("%s did not keep the statement the partial migration committed", m.engine.name)
	}
	if m.query("SELECT count(*) FROM schema_migrations WHERE state <> 'applied'", "") != "1" {
		m.fatalf("%s recorded no unfinished revision for the migration that stopped halfway", m.engine.name)
	}

	// The reading that settles it is the database's. Once the history has been
	// read again the refusal names the dirty revision, and the pending count
	// is gone: nothing is pending behind a revision nobody has accounted for.
	var dirty *ptahv1alpha1.PtahMigration
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		m.recordJobs()
		current := m.migration(migration)
		if dirtyReading(current.Status, 3) {
			dirty = current
			break
		}
		m.sleep(migrationPoll)
	}
	if dirty == nil {
		m.fatalf("%s never reported the dirty revision the partial run left", migration)
	}
	if !blockedMessageMatches(dirty.Status.Conditions, namesARevision) {
		m.fatalf("the dirty refusal does not name the revision a person has to decide about")
	}

	// A resource that stopped keeps reading and never runs again. The
	// read-only Jobs go on appearing, so what has to stand still is the set of
	// Apply ones, recorded here with the partial run's own Job already in it.
	// What is held is the refusal, not the phase.
	applies := m.applyJobUIDs(migration)
	for hold := time.Now().Add(90 * time.Second); time.Now().Before(hold); {
		m.recordJobs()
		if !blockedRefusalHeld(m.migration(migration).Status) {
			m.fatalf("%s stopped refusing while a partial migration stood unresolved", migration)
		}
		m.assertNoNewApplyJob(applies, "after a partial one", migration)
		m.sleep(10 * time.Second)
	}

	// The person's decision: the half is undone, the revision row goes with
	// it, and the sequence loses the migration that should not have run.
	m.logf("undoing the %s partial migration by hand and putting the sequence back", m.engine.kind)
	if _, err := m.sqlStatement(m.migrationDatabase(), "ALTER TABLE e2e_migration_widgets DROP COLUMN weight"); err != nil {
		m.fatalf("could not undo the column the %s partial migration committed: %v", m.engine.name, err)
	}
	if _, err := m.sqlStatement(m.migrationDatabase(), "DELETE FROM schema_migrations WHERE state <> 'applied'"); err != nil {
		m.fatalf("could not take the unfinished %s revision out of the history: %v", m.engine.name, err)
	}
	m.publish("v4", m.fixtureDir(""), m.reference(""))

	m.waitForPhase(ptahv1alpha1.MigrationPhaseInSync)
	if !recoveredAfterPartial(m.migration(migration).Status) {
		m.fatalf("%s did not recover on its own reading of a database somebody fixed", migration)
	}
	m.assertDatabaseMigrated()
	if m.widgetColumnCount("weight", "") != "0" {
		m.fatalf("%s kept the column the partial migration added after it was dropped", m.engine.name)
	}
	m.logf("PASS %s partial run blocked, and recovered without a spec edit", m.engine.kind)
}

// olderArtifactBlocksEverything is the older-artifact row of the matrix: a tag
// moved back to an artifact that ends before the database does.
//
// Nothing is pending here, and that is the trap. The database is asked what it
// holds and the artifact what it ends at, and when the first is past the
// second the resource stops. There is no automatic recovery and there must not
// be one: rolling a database back to match an older artifact is a data-loss
// decision. Putting the tag back is a person's, and the operator converges on
// its own reading once it lands.
func (m *migrationRun) olderArtifactBlocksEverything() {
	m.t.Helper()
	migration := m.migrationName()
	m.logf("moving the %s tag back to an artifact that ends before the database does", m.engine.kind)
	applies := m.applyJobUIDs(migration)
	m.publish("v5", m.fixtureDir("-older"), m.reference(""))

	var blocked *ptahv1alpha1.PtahMigration
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		m.recordJobs()
		current := m.migration(migration)
		if historyAheadBlocked(current.Status) {
			blocked = current
			break
		}
		// InSync is the answer being refused here, but only once the status
		// names the artifact that is older. Until the moved tag is resolved
		// the resource is still settled on the one it was settled on, and
		// failing on that would be failing on the poll landing early.
		if current.Status.Artifact != nil && current.Status.Artifact.Digest == m.published &&
			current.Status.Phase == ptahv1alpha1.MigrationPhaseInSync {
			m.fatalf("%s called an artifact older than its database InSync", migration)
		}
		m.sleep(migrationPoll)
	}
	if blocked == nil {
		m.fatalf("%s did not refuse an artifact that ends before its database within %s", migration, waitTimeout)
	}
	if !historyAhead(blocked.Status, m.published, 3, 2) {
		m.fatalf("%s did not report the reading that disagrees with itself", migration)
	}
	// The refusal names both numbers, because only one of them is a field.
	if !blockedMessageMatches(blocked.Status.Conditions, namesBothVersions) {
		m.fatalf("the older-artifact refusal does not name the two versions that disagree")
	}
	m.scanObject(blocked, "the older-artifact refusal")

	// Nothing ran, and above all nothing ran backwards.
	m.assertNoNewApplyJob(applies, "for an artifact older than its database", migration)
	m.assertDatabaseMigrated()

	m.logf("putting the %s tag back on the artifact the database was migrated with", m.engine.kind)
	m.publish("v6", m.fixtureDir(""), m.reference(""))
	m.waitForPhase(ptahv1alpha1.MigrationPhaseInSync)
	if !settledOnTheThree(m.migration(migration).Status) {
		m.fatalf("%s did not settle again once the artifact matched its database", migration)
	}
	m.logf("PASS %s refused an artifact older than its database, and settled when it was restored", m.engine.kind)
}

// modifiedFileBlocksEverything is the modified-file row of the matrix: an
// applied migration whose file changed afterwards is the refusal a versioned
// workflow exists to make. The tag moves to an artifact whose first migration
// is edited. Nothing about the database changed, so the refusal has to come
// from comparing the artifact against what the revision table recorded, and
// it has to leave the database exactly as the run left it.
func (m *migrationRun) modifiedFileBlocksEverything() {
	m.t.Helper()
	migration := m.migrationName()
	m.logf("moving the %s tag to an artifact whose applied file changed", m.engine.kind)
	before := m.published
	if m.publish("v2", m.fixtureDir("-modified"), m.reference("")) == before {
		m.fatalf("the edited %s artifact resolved to the digest the unedited one had", m.engine.name)
	}

	var blocked *ptahv1alpha1.PtahMigration
	observed := ptahv1alpha1.MigrationPhase("")
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		m.recordJobs()
		current := m.migration(migration)
		observed = current.Status.Phase
		if observed == ptahv1alpha1.MigrationPhaseBlocked {
			blocked = current
			break
		}
		if observed == ptahv1alpha1.MigrationPhaseFailed {
			m.fatalf("%s failed instead of refusing an edited applied migration", migration)
		}
		m.sleep(migrationPoll)
	}
	if blocked == nil {
		m.fatalf("%s did not refuse an edited applied migration within %s; it is in %s", migration, waitTimeout,
			cmpOrNone(string(observed)))
	}
	if !modifiedRefusal(blocked.Status, m.published) {
		m.fatalf("%s did not report the exact modified version and stay out of Ready", migration)
	}
	// Nothing ran, so nothing moved.
	m.assertDatabaseMigrated()
	if name := m.query("SELECT name FROM e2e_migration_widgets WHERE id = 1", ""); name != "first" {
		m.fatalf("%s re-ran an applied migration: row 1 now reads %s", m.engine.name, name)
	}
}

// The out-of-order row's own database and migration.
func (m *migrationRun) branchDatabase() string  { return "ptah_e2e_branch" }
func (m *migrationRun) branchMigration() string { return "e2e-branch-" + m.engine.name }

// branchStatus reads the branch migration as stored, scans it, and returns
// it typed beside whether its stored status carries a plan key at all.
func (m *migrationRun) branchStatus() (*ptahv1alpha1.PtahMigration, bool) {
	m.t.Helper()
	stored := &unstructured.Unstructured{}
	stored.SetAPIVersion(ptahSchemaAPIVersion)
	stored.SetKind("PtahMigration")
	m.check(m.get(m.branchMigration(), stored), "%s could not be read", m.branchMigration())
	m.scanObject(stored.Object, m.branchMigration()+" status")
	migration := &ptahv1alpha1.PtahMigration{}
	m.check(runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, migration), "decode %s", m.branchMigration())
	_, storesPlan, _ := unstructured.NestedFieldNoCopy(stored.Object, "status", "plan")
	return migration, storesPlan
}

// waitForBranchPhase waits for the branch migration to reach a phase.
func (m *migrationRun) waitForBranchPhase(phase ptahv1alpha1.MigrationPhase) {
	m.t.Helper()
	observed := ptahv1alpha1.MigrationPhase("")
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		current := &ptahv1alpha1.PtahMigration{}
		observed = ""
		if m.get(m.branchMigration(), current) == nil {
			observed = current.Status.Phase
		}
		if observed == phase {
			return
		}
		m.sleep(migrationPoll)
	}
	m.fatalf("%s did not reach %s within %s; it is in %s", m.branchMigration(), phase, waitTimeout, cmpOrNone(string(observed)))
}

// branchOutOfOrderProof is the incompatible-history row of the matrix: a
// migration that arrives below the version the database has already applied.
//
// It happens when two branches number migrations independently and the lower
// number lands second. Ptah executes in linear order and refuses the whole run
// while such a file is pending, so the operator refuses before it publishes a
// plan: a plan for it would ask a person to approve a sequence the executor
// cannot run. The proof needs versions with room between them, which the main
// fixture does not have, so it runs on a database and an artifact of its own,
// numbered 10 and 30, and the late arrival is 20.
func (m *migrationRun) branchOutOfOrderProof() {
	m.t.Helper()
	database, name := m.branchDatabase(), m.branchMigration()
	if m.databaseExists(database) != "0" {
		m.fatalf("database %s already exists on %s; the out-of-order proof needs a history that starts from nothing",
			database, m.engine.name)
	}
	m.isolatedDatabase(database, "e2e-"+m.engine.name+"-branch-db")
	m.publish("branch", m.fixtureDir("-branch"), m.reference("-branch"))
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: "e2e-" + m.engine.name + "-branch-db", reference: m.reference("-branch"),
		coordinationKey: "e2e/branch/" + m.engine.name,
	}))
	m.waitForBranchPhase(ptahv1alpha1.MigrationPhaseAwaitingApproval)
	current := &ptahv1alpha1.PtahMigration{}
	m.check(m.get(name, current), "read %s", name)
	if current.Status.Plan == nil || current.Status.Plan.Name == "" {
		m.fatalf("%s published no plan to approve", name)
	}
	plan := m.planOf(current.Status.Plan.Name)
	m.check(m.approve("e2e-branch-"+m.engine.name+"-approval", name, plan.Name, string(plan.UID), plan.Spec.Fingerprint),
		"approve %s", plan.Name)
	m.waitForBranchPhase(ptahv1alpha1.MigrationPhaseInSync)
	m.check(m.get(name, current), "read %s", name)
	if current.Status.History == nil || current.Status.History.CurrentVersion != 30 {
		m.fatalf("%s did not apply its spaced history", name)
	}
	m.assertLateBranchMigrationBlocks()
}

// assertLateBranchMigrationBlocks is the row itself: the artifact gains a
// migration numbered below what the database has applied, and the operator
// refuses before planning rather than after a Job fails.
func (m *migrationRun) assertLateBranchMigrationBlocks() {
	m.t.Helper()
	name := m.branchMigration()
	m.logf("publishing a %s migration numbered below the applied version", m.engine.kind)
	applies := m.applyJobUIDs(name)
	m.publish("branch-late", m.fixtureDir("-branch-late"), m.reference("-branch"))

	var refused *ptahv1alpha1.PtahMigration
	storesPlan := false
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		refused, storesPlan = m.branchStatus()
		if outOfOrderBlocked(refused.Status) {
			break
		}
		m.sleep(migrationPoll)
	}
	if !outOfOrderRefusal(refused.Status, storesPlan) {
		m.fatalf("%s did not refuse the out-of-order migration before planning", name)
	}
	// No plan means no approval to give and no Job to run: the refusal has to
	// stop the work rather than describe it.
	plans := &ptahv1alpha1.PtahMigrationPlanList{}
	m.check(m.list(plans, client.MatchingLabels{labelMigration: name}), "list the plans of %s", name)
	if len(plans.Items) != 1 {
		m.fatalf("%s published a plan for a sequence the executor refuses", name)
	}
	m.assertNoNewApplyJob(applies, "for a migration it refuses to plan", name)
	if m.query("SELECT count(*) FROM information_schema.columns WHERE table_name = 'e2e_branch_widgets' AND column_name = 'label'",
		m.branchDatabase()) != "0" {
		m.fatalf("the out-of-order migration reached the database")
	}
	m.logf("PASS %s refuses a migration numbered below the applied version", m.engine.kind)
}
