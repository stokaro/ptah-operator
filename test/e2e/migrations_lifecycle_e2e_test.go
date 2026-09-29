//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// engineMigrations takes the engine's migration from a database nothing has
// migrated through its approval gate, its run and the history it leaves, and
// then runs every row that holds the path to a refusal or a fault.
func (m *migrationRun) engineMigrations() {
	m.t.Helper()
	m.logf("starting the %s lifecycle on a database nothing has migrated", m.engine.kind)
	m.jobs = map[types.UID]batchv1.Job{}
	m.createDatabase(m.migrationDatabase())
	m.createDatabaseSecret(m.migrationSecret(), m.migrationDatabase(), m.databaseURL(m.migrationDatabase(), ""))
	m.publish("v1", m.fixtureDir(""), m.reference(""))

	m.createMigrationRealm()
	m.createMigrationResource()
	m.waitForPhase(ptahv1alpha1.MigrationPhaseAwaitingApproval)
	m.assertAwaitingApproval()
	m.assertPlanSequence()
	m.assertKubectlPtahMigration(ptahv1alpha1.MigrationPhaseAwaitingApproval)
	plan := m.planOf(m.plan)
	if plan.UID == "" || plan.Spec.Fingerprint == "" {
		m.fatalf("migration plan %s has no UID or fingerprint", m.plan)
	}
	m.planUID, m.planFingerprint = string(plan.UID), plan.Spec.Fingerprint

	approval := "e2e-migrations-" + m.engine.name + "-approval"
	m.check(m.approve(approval, m.migrationName(), m.plan, m.planUID, m.planFingerprint), "approve %s", m.plan)
	m.assertApprovalStamped(approval)
	m.waitForPhase(ptahv1alpha1.MigrationPhaseInSync)
	settled := m.assertInSync()
	m.assertDatabaseMigrated()
	m.assertRepeatedReconciliationRunsNothing(settled)
	m.assertMigrationJobIsolation()
	m.assertReplacedPlanApprovalRefused()
	// The shortened interval means the resource may be mid-cycle by now; the
	// settled view is a statement about the settled state.
	m.waitForPhase(ptahv1alpha1.MigrationPhaseInSync)
	m.assertKubectlPtahMigration(ptahv1alpha1.MigrationPhaseInSync)

	m.realmAdmitsOnlyListedClaimants()
	m.partialRunBlocksAndRecovers()
	m.olderArtifactBlocksEverything()
	m.modifiedFileBlocksEverything()
	m.applyPolicyGuardProof()
	m.branchOutOfOrderProof()
	m.existingSchemaAdoptionProof()
	m.checkpointBootstrapProof()
	m.uncertainApplyProof()
	m.lateDispatchProof()
	m.restoredHistoryProof()
	m.deletionDuringApplyProof()
	m.stoppedApplyProof()
	m.lostLogProof()
	m.retryIntervalProof()
	m.suspensionDuringApplyProof()
	m.lockReleaseFaultProof()
	m.isolatedNodeProof()
	m.unknownLayerProof()
	m.egressPolicyProof()
	m.retargetBeforeDispatchProof()
	m.rebuildDrill()
	m.logf("PASS %s approval gate, applied sequence, and matching history", m.engine.kind)
}

// publish publishes a migration directory with the Ptah the operator runs,
// with the command a person would use, from a Job that holds the registry
// credential and no database credential, and returns the digest it reported.
// The harness owns no migration-directory format of its own.
func (m *migrationRun) publish(version, directory, reference string) string {
	m.t.Helper()
	configMap := "e2e-migrations-" + m.engine.name + "-" + version
	name := "e2e-push-migrations-" + m.engine.name + "-" + version
	entries, err := os.ReadDir(directory)
	if err != nil {
		m.fatalf("migration fixtures are missing: %s", directory)
	}
	m.logf("publishing the %s migration directory as %s", m.engine.kind, version)
	data := map[string]any{}
	var files []string
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		m.check(err, "read %s", entry.Name())
		data[entry.Name()] = string(content)
		if strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	slices.Sort(files)
	if len(files) == 0 {
		m.fatalf("no migration files to publish from %s", directory)
	}
	m.mustCreate(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"namespace": m.in.TestNamespace, "name": configMap},
		"data":     data,
	})
	// A ConfigMap volume is not a directory of files: the kubelet writes the
	// keys into a timestamped directory, points ..data at it, and leaves one
	// symlink per key beside it, so a walker that descends into directories
	// finds every migration twice. Ptah's Discover does exactly that. A
	// subPath mount per file gives a plain directory, and the list comes from
	// the fixtures so it cannot fall behind them.
	var mounts []any
	for _, file := range files {
		mounts = append(mounts, map[string]any{"name": "migrations", "mountPath": "/migrations/" + file, "subPath": file, "readOnly": true})
	}
	mounts = append(mounts, map[string]any{"name": "work", "mountPath": "/work"})
	labels := map[string]any{"app.kubernetes.io/component": "e2e-migration-publisher"}
	m.mustCreate(map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"namespace": m.in.TestNamespace, "name": name, "labels": labels},
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
						"name": "publisher", "image": m.in.ExecutorImage, "imagePullPolicy": "IfNotPresent",
						"command": []any{"/usr/local/bin/ptah"},
						"args": []any{
							"migrations", "push", reference, "--migrations-dir", "/migrations",
							"--dir-format", "ptah", "--version", version, "--plain-http",
						},
						"env": []any{
							map[string]any{"name": "HOME", "value": "/work"},
							map[string]any{"name": "TMPDIR", "value": "/work"},
							secretEnv("PTAH_OCI_USERNAME", registryAuthSecret, "username"),
							secretEnv("PTAH_OCI_PASSWORD", registryAuthSecret, "password"),
							secretEnv("PTAH_OCI_REGISTRY", registryAuthSecret, "registry"),
						},
						"securityContext": map[string]any{
							"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
							"capabilities": map[string]any{"drop": []any{"ALL"}},
						},
						"volumeMounts": mounts,
					}},
					"volumes": []any{
						map[string]any{"name": "migrations", "configMap": map[string]any{"name": configMap}},
						map[string]any{"name": "work", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
					},
				},
			},
		},
	})
	job := &batchv1.Job{}
	m.poll("the migration publisher Job "+name, 3*time.Second, func() bool {
		if m.get(name, job) != nil {
			return false
		}
		if job.Status.Succeeded > 0 {
			return true
		}
		if job.Status.Failed > 0 {
			// The publisher reaches the registry and never the database, so its
			// own words are safe to print once scanned. A log that cannot be
			// read does not stand in for the reason the row fails.
			if logs, err := m.readJobLogs(job); err == nil {
				m.scan(logs, "the migration publisher log")
				_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: the publisher said:\n%s\n", logs)
			}
			m.fatalf("the migration publisher Job failed")
		}
		return false
	})
	// The publisher holds registry credentials and must hold no database
	// credential: the boundary the schema publisher keeps.
	if !publisherJobIsolation(job, m.in.ExecutorImage, registryAuthSecret) {
		m.fatalf("the migration publisher Job did not preserve the no-database-credential boundary")
	}
	digests := publishedDigests(m.jobLogs(job))
	if len(digests) == 0 {
		m.fatalf("could not read the published migration digest from Job %s", name)
	}
	m.published = digests[len(digests)-1]
	return m.published
}

// jobLogs reads the log of a Job's first Pod, as kubectl logs job/<name> did.
func (m *migrationRun) jobLogs(job *batchv1.Job) []byte {
	m.t.Helper()
	logs, err := m.readJobLogs(job)
	m.check(err, "read the logs of Job %s", job.Name)
	return logs
}

func (m *migrationRun) readJobLogs(job *batchv1.Job) ([]byte, error) {
	pods := &corev1.PodList{}
	if err := m.list(pods, client.MatchingLabels{"job-name": job.Name}); err != nil {
		return nil, err
	}
	owned := ownedPods(pods.Items, job.UID)
	if len(owned) == 0 {
		return nil, fmt.Errorf("no Pod of Job %s to read logs from", job.Name)
	}
	return m.cluster.ContainerLog(m.ctx, m.in.TestNamespace, owned[0].Name, owned[0].Spec.Containers[0].Name)
}

// createMigrationRealm names the migration database by a PtahRealm rather
// than a key, so the lifecycle runs on a claim an administrator granted, and
// the realm row can name the same database from a namespace the realm does
// not list. It admits this namespace alone and one claimant at a time.
func (m *migrationRun) createMigrationRealm() {
	m.t.Helper()
	if err := m.create(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahRealm",
		"metadata": map[string]any{"name": m.migrationRealm()},
		"spec": map[string]any{
			"engine": m.engine.kind, "namespaces": []any{m.in.TestNamespace}, "sharing": "Exclusive",
		},
	}); err != nil {
		m.fatalf("the PtahRealm %s could not be created: %v", m.migrationRealm(), err)
	}
}

// createMigrationResource creates the main migration, and holds the stored
// document to the conservative default an artifact of arbitrary SQL gets
// whether or not its author wrote one down.
func (m *migrationRun) createMigrationResource() {
	m.t.Helper()
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: m.migrationName(), secret: m.migrationSecret(), reference: m.reference(""), realm: m.migrationRealm(),
	}))
	stored := &unstructured.Unstructured{}
	stored.SetAPIVersion(ptahSchemaAPIVersion)
	stored.SetKind("PtahMigration")
	m.check(m.get(m.migrationName(), stored), "read %s", m.migrationName())
	apply, _, _ := unstructured.NestedString(stored.Object, "spec", "policy", "apply")
	suspend, found, _ := unstructured.NestedBool(stored.Object, "spec", "suspend")
	if apply != "OnApproval" || !found || suspend {
		m.fatalf("%s did not persist the safe apply-policy default", m.migrationName())
	}
}

func (m *migrationRun) assertAwaitingApproval() {
	m.t.Helper()
	migration := m.migration(m.migrationName())
	if err := awaitingApprovalGate(migration, m.published, m.stateVersion); err != nil {
		m.fatalf("%s did not reach an exact three-migration approval gate: %v", m.migrationName(), err)
	}
	m.plan = migration.Status.Plan.Name
	m.logf("plan %s awaits approval", m.plan)
}

func (m *migrationRun) assertPlanSequence() {
	m.t.Helper()
	plan := m.planOf(m.plan)
	digest, err := realmDigest(m.engine.name, m.migrationRealm())
	m.check(err, "derive the realm's coordination digest")
	if err := planSequence(plan, m.migrationName(), m.published, digest, m.controller, m.stateVersion); err != nil {
		m.fatalf("migration plan %s is not the exact pending sequence: %v", m.plan, err)
	}
	// A plan says which migrations run and in what order. It never carries
	// the SQL, so a reader who may see the order still may not read the
	// statements.
	document, err := asJSON(plan)
	m.check(err, "decode plan %s", m.plan)
	if carriesMigrationSQL(document) {
		m.fatalf("migration plan %s carries SQL text", m.plan)
	}
}

func (m *migrationRun) assertKubectlPtahMigration(phase ptahv1alpha1.MigrationPhase) {
	m.t.Helper()
	if err := kubectlPtahMigrationView(m.kubectlPtahView(m.migrationName()), m.in.TestNamespace, m.migrationName(), phase, m.plan); err != nil {
		m.fatalf("kubectl ptah migration: %v", err)
	}
}

// assertApprovalStamped reads the stored approval back beside the plan it
// names. The history, the artifact and the execution binding the decision
// was made under are on the plan the fingerprint names.
func (m *migrationRun) assertApprovalStamped(approval string) {
	m.t.Helper()
	stored := &unstructured.Unstructured{}
	stored.SetAPIVersion(ptahSchemaAPIVersion)
	stored.SetKind("PtahMigrationApproval")
	m.check(m.get(approval, stored), "read approval %s", approval)
	m.scanObject(stored.Object, "approval "+approval)
	if err := migrationApprovalStamped(stored, m.migrationName(), m.planOf(m.plan)); err != nil {
		m.fatalf("approval %s was not stamped and bound to the exact plan: %v", approval, err)
	}
}

// assertInSync holds the settled status, and returns it.
func (m *migrationRun) assertInSync() *ptahv1alpha1.PtahMigration {
	m.t.Helper()
	migration := m.migration(m.migrationName())
	if err := settledInSync(migration, m.published); err != nil {
		m.fatalf("%s did not settle on a history that matches the artifact: %v", m.migrationName(), err)
	}
	// A run's evidence explains what happened without reproducing what ran.
	document, err := asJSON(migration.Status)
	m.check(err, "decode %s status", m.migrationName())
	if carriesMigrationSQL(document) {
		m.fatalf("%s status carries SQL text", m.migrationName())
	}
	return migration
}

// assertDatabaseMigrated checks what the migrations claim to have done in the
// database rather than in the status that reports it.
func (m *migrationRun) assertDatabaseMigrated() {
	m.t.Helper()
	schema := m.engine.currentSchemaFilter()
	if count := m.query("SELECT count(*) FROM information_schema.tables WHERE "+schema+" AND table_name='e2e_migration_widgets'", ""); count != "1" {
		m.fatalf("%s migration 1 did not create its table; information_schema reports %s", m.engine.name, count)
	}
	// Migration 1 seeds three rows. A run the history already recorded must
	// not execute again, and a repeated INSERT is the one kind of DML that
	// says so without an observer: the count proves it did not.
	if count := m.query("SELECT count(*) FROM e2e_migration_widgets", ""); count != "3" {
		m.fatalf("%s has %s seeded rows, not the three migration 1 inserted once", m.engine.name, count)
	}
	if count := m.query("SELECT count(*) FROM information_schema.columns WHERE "+schema+" AND table_name='e2e_migration_widgets' AND column_name='color'", ""); count != "1" {
		m.fatalf("%s migration 2 did not add its column; information_schema reports %s", m.engine.name, count)
	}
	// Migration 2 adds the column, fills it, and only then constrains it. A
	// reordered run leaves the constraint refused or the column nullable.
	if nullable := m.query("SELECT is_nullable FROM information_schema.columns WHERE "+schema+" AND table_name='e2e_migration_widgets' AND column_name='color'", ""); nullable != "NO" {
		m.fatalf("%s migration 2 left its column nullable: is_nullable=%s", m.engine.name, nullable)
	}
	// Migration 3 is DML with an empty schema diff. It has to run and be
	// recorded like any other version.
	if color := m.query("SELECT color FROM e2e_migration_widgets WHERE id = 1", ""); color != "blue" {
		m.fatalf("%s migration 3 did not apply its data-only change: color=%s", m.engine.name, color)
	}
	if untouched := m.query("SELECT count(*) FROM e2e_migration_widgets WHERE color = 'unset'", ""); untouched != "2" {
		m.fatalf("%s migration 3 changed rows other than the one it names: %s left unset", m.engine.name, untouched)
	}
}

// assertMigrationJobIsolation reads the isolation row from the Jobs the
// controller created rather than from the builder that wrote them.
func (m *migrationRun) assertMigrationJobIsolation() {
	m.t.Helper()
	jobs := m.jobInventory()
	if !migrationJobIsolation(jobs, m.migrationSecret(), registryAuthSecret, m.in.ExecutorImage, m.in.RunnerImage, "default") {
		m.fatalf("migration Jobs did not keep registry access out of the process that runs SQL")
	}
	if operations := jobOperations(jobs); operations != "apply,history,resolve,verify" {
		m.fatalf("the archived migration Jobs cover %s, not the whole lifecycle", operations)
	}
}

// assertRepeatedReconciliationRunsNothing holds a history the database already
// holds to producing no second run. The interval is shortened rather than
// waited out, which also makes a new generation: the whole read-only chain
// runs again from resolution.
func (m *migrationRun) assertRepeatedReconciliationRunsNothing(settled *ptahv1alpha1.PtahMigration) {
	m.t.Helper()
	name := m.migrationName()
	if settled.Status.LastRun == nil || settled.Status.LastRun.JobUID == "" || settled.Status.History == nil ||
		settled.Status.History.ObservedAt.IsZero() {
		m.fatalf("%s settled without run and history evidence to compare against", name)
	}
	runUID, observed := string(settled.Status.LastRun.JobUID), instantOf(settled.Status.History.ObservedAt)
	m.patchMigration(name, map[string]any{"spec": map[string]any{"interval": "30s"}})
	var repeated *ptahv1alpha1.PtahMigration
	m.poll(name+" to read its history again", migrationPoll, func() bool {
		m.recordJobs()
		current := m.migration(name)
		phase := current.Status.Phase
		if phase == ptahv1alpha1.MigrationPhaseInSync && current.Status.History != nil &&
			instantOf(current.Status.History.ObservedAt) != observed {
			repeated = current
			return true
		}
		if phase == ptahv1alpha1.MigrationPhaseFailed || phase == ptahv1alpha1.MigrationPhaseBlocked {
			m.fatalf("%s left InSync on a repeated reconciliation: %s", name, phase)
		}
		return false
	})
	if err := reconciledWithoutWork(repeated, runUID); err != nil {
		m.fatalf("%s started new work for a history it already matched: %v", name, err)
	}
	m.assertDatabaseMigrated()
}

// assertReplacedPlanApprovalRefused holds admission to refusing a second
// approval of the plan a run consumed: a decision authorizes one run.
func (m *migrationRun) assertReplacedPlanApprovalRefused() {
	m.t.Helper()
	err := m.approve("e2e-migrations-"+m.engine.name+"-stale-approval", m.migrationName(), m.plan, m.planUID, m.planFingerprint)
	if err == nil {
		m.fatalf("an approval naming the consumed plan was accepted")
	}
	m.scan([]byte(err.Error()), "the consumed-plan approval refusal")
	if !consumedPlanRefusal.MatchString(err.Error()) {
		m.fatalf("the refusal of a consumed-plan approval did not say what it refused: %v", err)
	}
}

// resetAfterAnEarlierRun clears what an earlier run of this phase left on a
// retained lab, when hack/e2e-rerun-phase.sh put the phase back on it. The
// data plane's schemas, Secrets and databases stay, and so does the
// verification policy. The deletions wait for the controller's finalizers: a
// rerun that raced them would find the realm still claimed by a resource on
// its way out.
func (m *migrationRun) resetAfterAnEarlierRun() {
	m.t.Helper()
	if m.rerun == "" {
		return
	}
	m.logf("rerun %s: removing what an earlier run of this phase left behind", m.rerun)
	// First, because a run that died with the isolation worker cut off left an
	// Apply its resource still holds, and the deletions below would wait on it.
	if err := m.removeIsolationRules(m.ctx); err != nil {
		m.fatalf("the isolation rules an earlier run left could not be removed: %v", err)
	}
	// A run that died with the apply gate closed left an Apply Pod waiting for
	// a node, and its resource keeps its finalizer until that Job ends.
	// Opening the gate lets the Pod start and be refused for its window.
	_ = m.setNodeLabel(m.ctx, applyGateLabel, "open")
	m.deleteAllAndWait(&ptahv1alpha1.PtahMigrationList{}, "the PtahMigrations an earlier run left behind")
	m.deleteAllAndWait(&ptahv1alpha1.PtahMigrationApprovalList{}, "the approvals an earlier run left behind")
	m.deleteAllAndWait(&ptahv1alpha1.PtahMigrationPlanList{}, "the plans an earlier run left behind")
	rival := &corev1.Namespace{}
	rival.Name = "e2e-realm-rival-" + m.engine.name
	m.deleteAndWait(rival, rival.Name)
	// After the migrations that name it, so none of them is refused on the
	// way out; the lifecycle creates it again.
	realm := &ptahv1alpha1.PtahRealm{}
	realm.Name = m.migrationRealm()
	m.deleteAndWait(realm, "the PtahRealm "+realm.Name)
	// Every row's Secret, including those the script's own reset missed: a
	// rerun would otherwise stop at the first row whose database survived.
	for _, secret := range []string{
		m.migrationSecret(), "branch", "adopt", "checkpoint", "txmode", "uncertain", "unknown-layer",
		"egress", "late-dispatch", "isolated-node", "stopped", "lost-log", "apply-guard", "release-fault",
		"restore", "deletion", "drill", "suspend", "retry", "retarget", "approval-policy", "approval-transaction-mode",
	} {
		name := secret
		if !strings.HasPrefix(name, "e2e-") {
			name = "e2e-" + m.engine.name + "-" + secret + "-db"
		}
		object := &corev1.Secret{}
		object.Namespace, object.Name = m.in.TestNamespace, name
		m.deleteAndWait(object, "Secret "+name)
	}
	// The publisher objects carry the version they published in their names,
	// so they are found by the prefix publish gives them.
	configMaps := &corev1.ConfigMapList{}
	m.check(m.list(configMaps), "list the ConfigMaps an earlier run left behind")
	for index := range configMaps.Items {
		if strings.HasPrefix(configMaps.Items[index].Name, "e2e-migrations-"+m.engine.name+"-") {
			m.deleteAndWait(&configMaps.Items[index], "ConfigMap "+configMaps.Items[index].Name)
		}
	}
	jobs := &batchv1.JobList{}
	m.check(m.list(jobs), "list the Jobs an earlier run left behind")
	for index := range jobs.Items {
		name := jobs.Items[index].Name
		if strings.HasPrefix(name, "e2e-push-migrations-"+m.engine.name+"-") || name == "e2e-adopt-baseline-"+m.engine.name {
			m.deleteAndWait(&jobs.Items[index], "Job "+name)
		}
	}
	for _, database := range []string{
		m.migrationDatabase(), "ptah_e2e_branch", "ptah_e2e_adopt", "ptah_e2e_adopt_shadow", "ptah_e2e_checkpoint",
		"ptah_e2e_txmode", "ptah_e2e_uncertain", "ptah_e2e_unknown_layer", "ptah_e2e_egress",
		"ptah_e2e_late_dispatch", "ptah_e2e_isolated_node", "ptah_e2e_stopped", "ptah_e2e_lost_log",
		"ptah_e2e_apply_guard", "ptah_e2e_release_fault", "ptah_e2e_restore", "ptah_e2e_deletion",
		"ptah_e2e_drill", "ptah_e2e_suspend", "ptah_e2e_retarget", "ptah_e2e_retarget_other",
		"ptah_e2e_approval_policy", "ptah_e2e_approval_transaction_mode",
	} {
		m.dropDatabase(database)
	}
	// A release fault an earlier run left in force would refuse every later
	// release of that row's Lease.
	faultBinding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	faultBinding.Name = "ptah-e2e-release-fault-" + m.engine.name
	m.deleteAndWait(faultBinding, "the release fault binding an earlier run left behind")
	faultPolicy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	faultPolicy.Name = faultBinding.Name
	m.deleteAndWait(faultPolicy, "the release fault policy an earlier run left behind")
	// An earlier run that died inside the egress proof may have left its
	// policies, which would isolate every operation Pod this run starts, and
	// its probes, whose names this run reuses.
	m.check(m.cluster.Client.DeleteAllOf(m.ctx, &networkingv1.NetworkPolicy{}, client.InNamespace(m.in.TestNamespace),
		client.MatchingLabels{"operator.ptah.run/e2e-proof": "egress"}), "remove the egress policies an earlier run left behind")
	// The probes are waited out: this run reuses their names, and one still
	// terminating would refuse its replacement.
	probes := &corev1.PodList{}
	m.check(m.list(probes, client.MatchingLabels{"operator.ptah.run/e2e-probe": "egress"}), "list the egress probes an earlier run left behind")
	for index := range probes.Items {
		m.deleteAndWait(&probes.Items[index], "the egress probe "+probes.Items[index].Name)
	}
	// A gate left open would let the next gated Pod schedule at once, and the
	// late-dispatch proof would pass without having held anything.
	_ = m.setNodeLabel(m.ctx, applyGateLabel, "")
}

// deleteAllAndWait deletes every object of a kind in the test namespace and
// waits until none is left.
func (m *migrationRun) deleteAllAndWait(list client.ObjectList, description string) {
	m.t.Helper()
	m.check(m.list(list), "list %s", description)
	objects, err := apimeta.ExtractList(list)
	m.check(err, "read %T", list)
	for _, object := range objects {
		item, ok := object.(client.Object)
		if !ok {
			m.fatalf("%T is not an object", object)
		}
		m.deleteAndWait(item, description)
	}
}
