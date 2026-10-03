//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// uncertainMigration and the names below are the uncertain row's.
func (m *migrationRun) uncertainMigration() string { return "e2e-uncertain-" + m.engine.name }
func (m *migrationRun) uncertainDatabase() string  { return "ptah_e2e_uncertain" }
func (m *migrationRun) uncertainSecret() string    { return "e2e-" + m.engine.name + "-uncertain-db" }

// within reads every interval until ready holds or waitTimeout passes, and
// says which: the rows below fail with their own words and state reports.
func (m *migrationRun) within(interval time.Duration, ready func() bool) bool {
	m.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if ready() {
			return true
		}
		m.sleep(interval)
	}
	return false
}

// uncertainApplyProof removes the Apply Job of a run that has committed part
// of its work, and holds the operator to stopping on a run whose evidence it
// could not read, to keeping that refusal once another refusal overwrote its
// reason, and to settling it only on a person's acknowledgment. Later rows
// reuse the artifact it publishes: its third migration sleeps, which is the
// window they need.
func (m *migrationRun) uncertainApplyProof() {
	m.t.Helper()
	m.isolatedDatabase(m.uncertainDatabase(), m.uncertainSecret())
	m.publish("uncertain", m.fixtureDir("-uncertain"), m.reference("-uncertain"))
	watcher, err := client.NewWithWatch(m.cluster.Config, client.Options{Scheme: m.cluster.Scheme})
	m.check(err, "open direct API watches before the uncertain migration exists")
	jobs := migrationExecutorRecorder[*batchv1.Job](m, watcher, "uncertain-jobs", m.in.TestNamespace, func() client.ObjectList { return &batchv1.JobList{} })
	pods := migrationExecutorRecorder[*corev1.Pod](m, watcher, "uncertain-pods", m.in.TestNamespace, func() client.ObjectList { return &corev1.PodList{} })
	// Always, because the row is about a run that started and not about the
	// gate that authorizes one. An approval here would only add a step between
	// the publish and the interruption.
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: m.uncertainMigration(), secret: m.uncertainSecret(), reference: m.reference("-uncertain"),
		coordinationKey: "e2e/uncertain/" + m.engine.name, apply: "Always", interval: "30s",
	}))

	// The Job to remove is the one the resource says it dispatched, by name
	// and by UID. A Job found by label could be a later one, and removing that
	// would prove something about a run nobody was waiting on.
	var jobName, jobUID string
	if !m.within(2*time.Second, func() bool {
		var ok bool
		jobName, jobUID, ok = uncertainApplyClaimed(m.migration(m.uncertainMigration()))
		return ok
	}) {
		m.fatalf("%s did not dispatch an Apply bound to its own Job within %s", m.uncertainMigration(), waitTimeout)
	}
	// The database is what says the SQL committed, not the Job and not the
	// status. Waiting for the second migration to be recorded is waiting for
	// the part of the run that must survive the interruption.
	if !m.within(2*time.Second, func() bool {
		return m.query("SELECT count(*) FROM schema_migrations WHERE version <= 2 AND state = 'applied'", m.uncertainDatabase()) == "2"
	}) {
		m.fatalf("the %s uncertain run did not commit its first two migrations within %s", m.engine.name, waitTimeout)
	}
	m.logf("removing the %s Apply Job while its run is still going", m.engine.kind)
	// The name is reused across attempts, so the UID is what says this is the
	// Job the resource is waiting on rather than a later one under that name.
	live := &batchv1.Job{}
	if err := m.get(jobName, live); err != nil || string(live.UID) != jobUID {
		m.fatalf("the %s Apply Job under that name is not the one the resource dispatched", m.engine.name)
	}
	original := m.migration(m.uncertainMigration()).Status.ActiveOperation.DeepCopy()
	if original == nil || original.JobName != jobName || string(original.JobUID) != jobUID {
		m.fatalf("the interrupted %s Apply no longer holds its original claim", m.engine.name)
	}
	allPods := &corev1.PodList{}
	m.check(m.list(allPods), "read the original Apply's ownership before deletion")
	owned := ownedPods(allPods.Items, live.UID)
	if len(owned) != 1 || owned[0].UID == "" || owned[0].Status.Phase != corev1.PodRunning {
		m.fatalf("the interrupted %s Apply does not own exactly one running Pod", m.engine.name)
	}
	originalPodUID := string(owned[0].UID)
	migrationExecutorWatchBarrier(m, jobs, live)
	migrationExecutorPodWatchBarrier(m, pods, &owned[0])
	m.deleteAndWait(live, "the "+m.engine.name+" Apply Job")
	m.assertUncertainApplyBlocksWithoutReplaying(original)
	m.assertUnresolvedRunSurvivesAnotherRefusal(jobName, jobUID)
	// Close both watches before a person's acknowledgment can authorize any
	// later work. Their histories include the original ADDED events because
	// the resource did not exist when the watches began.
	for _, recorder := range []recorder{jobs, pods} {
		m.check(recorder.alive(), "the uncertain Apply %s watch stopped", recorder.stem())
		recorder.requestStop()
	}
	watchDeadline := time.Now().Add(35 * time.Second)
	for _, recorder := range []recorder{jobs, pods} {
		m.check(recorder.await(time.Until(watchDeadline)), "close the uncertain Apply %s watch at natural EOF", recorder.stem())
		history, count, err := recorder.history()
		m.check(err, "encode the uncertain Apply %s history", recorder.stem())
		if count == 0 {
			m.fatalf("the uncertain Apply %s watch recorded nothing", recorder.stem())
		}
		m.scan(history, recorder.stem()+" closed history")
	}
	if !migrationExecutorNoReplay(jobs.snapshot(), pods.snapshot(), m.uncertainMigration(), jobUID, originalPodUID) {
		m.fatalf("the interrupted %s Apply was replayed or overlapped another run in the complete Job/Pod history", m.engine.name)
	}
	m.assertUnresolvedRunAcknowledgedByAPerson()
	m.logf("PASS %s stopped on a run it could not read, and replayed nothing", m.engine.kind)
}

// waitForUncertainPhase waits for the uncertain migration to reach a phase
// and returns the document that showed it: a resource that stopped still
// reads its history at its interval, so a later read can land mid-cycle.
func (m *migrationRun) waitForUncertainPhase(phase ptahv1alpha1.MigrationPhase) *ptahv1alpha1.PtahMigration {
	m.t.Helper()
	observed := ptahv1alpha1.MigrationPhase("")
	var matched *ptahv1alpha1.PtahMigration
	if !m.within(migrationPoll, func() bool {
		migration := &ptahv1alpha1.PtahMigration{}
		observed = ""
		if m.get(m.uncertainMigration(), migration) == nil {
			observed = migration.Status.Phase
			if observed == phase {
				m.scanObject(migration, m.uncertainMigration()+" status")
				matched = migration
			}
		}
		return observed == phase
	}) {
		m.fatalf("%s did not reach %s within %s; it is in %s", m.uncertainMigration(), phase, waitTimeout, cmpOrNone(string(observed)))
	}
	return matched
}

// uncertainWidgetRows is how many rows the uncertain row's first migration
// left, which a replay of it would double.
func (m *migrationRun) uncertainWidgetRows() string {
	return m.query("SELECT count(*) FROM e2e_migration_widgets", m.uncertainDatabase())
}

func (m *migrationRun) assertUncertainApplyBlocksWithoutReplaying(original *ptahv1alpha1.MigrationOperationStatus) {
	m.t.Helper()
	name := m.uncertainMigration()
	if !m.within(2*time.Second, func() bool {
		current := m.migration(name)
		pods := &corev1.PodList{}
		m.check(m.list(pods), "read all Pods while the interrupted Apply settles")
		settled, err := uncertainApplySettled(current.Status, original, pods.Items)
		m.check(err, "%s violated its interrupted Apply boundary", name)
		m.assertNoNewApplyJob([]string{string(original.JobUID)}, "while its interrupted workload was settling", name)
		return settled
	}) {
		m.fatalf("%s did not stop its owned Pods, retire its original claim and preserve Unknown within %s", name, waitTimeout)
	}
	// The run is over and the database keeps what it committed. Both halves
	// matter: without the first the refusal is about nothing, and without the
	// second there would be nothing a replay could double.
	if m.uncertainWidgetRows() != "3" {
		m.fatalf("the %s uncertain run did not leave the rows its first migration inserted", m.engine.name)
	}
	// Not the absence of a row: a run that was cut off may leave a revision
	// behind, and whether it does is Ptah's business. What may not have
	// happened is the migration recording itself finished.
	if m.query("SELECT count(*) FROM schema_migrations WHERE version = 3 AND state = 'applied'", m.uncertainDatabase()) != "0" {
		m.fatalf("the %s migration that was interrupted recorded itself applied", m.engine.name)
	}
	// Nothing dispatches again. A replay would re-run the first migration,
	// whose insert is not idempotent, so this is the assertion the row exists
	// for. The window outlasts the resource's thirty-second interval: the
	// claim is about what the operator does once it has read the history
	// again.
	recorded := m.applyJobUIDs(name)
	for deadline := time.Now().Add(120 * time.Second); time.Now().Before(deadline); {
		if !blockedRefusalHeld(m.migration(name).Status) {
			m.fatalf("%s stopped refusing while its run stood unaccounted for", name)
		}
		m.assertNoNewApplyJob(recorded, "after one whose evidence it could not read", name)
		m.sleep(10 * time.Second)
	}
	if m.uncertainWidgetRows() != "3" {
		m.fatalf("the %s rows were doubled, so a run was replayed over what it had already committed", m.engine.name)
	}
}

// assertUnresolvedRunSurvivesAnotherRefusal is the row status.unresolvedRun
// exists for (#220). A run nobody could read used to be latched in the Blocked
// condition's reason alone; a later refusal rewrote the reason, and when it
// went away the resource read as resolved and replayed a migration that may
// already have committed. Here the later refusal is a second claimant on the
// same database, and what has to survive it is the refusal to run again.
func (m *migrationRun) assertUnresolvedRunSurvivesAnotherRefusal(jobName, jobUID string) {
	m.t.Helper()
	name := m.uncertainMigration()
	if err := unresolvedRunRecorded(m.migration(name).Status, jobName, jobUID); err != nil {
		m.fatalf("%s did not record the run whose effect nobody established: %v", name, err)
	}
	recorded := m.applyJobUIDs(name)
	m.logf("overwriting the %s unresolved-run refusal with a realm conflict", m.engine.kind)
	rival := "e2e-uncertain-" + m.engine.name + "-rival"
	m.mustCreate(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": m.in.TestNamespace, "name": rival},
		"spec": map[string]any{
			"target": map[string]any{
				"engine": m.engine.kind, "coordinationKey": "e2e/uncertain/" + m.engine.name,
				"urlFrom": map[string]any{"name": m.uncertainSecret(), "key": "url"},
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
	})
	// Wait for the reason to be the conflict's rather than the run's. That is
	// the state the defect needed, and reaching it is what makes the recovery
	// below a proof instead of a formality.
	var overwritten *ptahv1alpha1.PtahMigration
	if !m.within(migrationPoll, func() bool {
		current := m.migration(name)
		if realmConflictBlocked(current.Status) {
			overwritten = current
			return true
		}
		return false
	}) {
		m.fatalf("%s never took the realm refusal, so nothing overwrote the run's reason", name)
	}
	// The record is somewhere a reason cannot reach, so it is still here.
	if overwritten.Status.UnresolvedRun == nil {
		m.fatalf("the realm refusal erased the record of the run nobody accounted for")
	}
	m.logf("removing the %s rival so only the unresolved run is left", m.engine.kind)
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Namespace, schema.Name = m.in.TestNamespace, rival
	m.deleteAndWait(schema, rival)

	// The conflict is over and nothing else refuses this resource, so a
	// manager that had lost the latch would plan and dispatch here. A refusal
	// that holds because nothing is running is not the refusal this measures:
	// the record clears from a reading, so the resource has to still be taking
	// them, and its history observation has to move inside the window.
	before := m.migration(name)
	if before.Status.History == nil || before.Status.History.ObservedAt.IsZero() {
		m.fatalf("%s carries no history reading to watch for movement", name)
	}
	observedBefore := instantOf(before.Status.History.ObservedAt)
	reread := false
	for deadline := time.Now().Add(120 * time.Second); time.Now().Before(deadline); {
		current := m.migration(name)
		if !blockedRefusalHeld(current.Status) {
			m.fatalf("%s stopped refusing once the realm conflict that had overwritten its reason was gone", name)
		}
		if current.Status.UnresolvedRun == nil {
			m.fatalf("%s dropped the record of the run nobody accounted for", name)
		}
		m.assertNoNewApplyJob(recorded, "after a refusal that had overwritten its unresolved run", name)
		if current.Status.History == nil || instantOf(current.Status.History.ObservedAt) != observedBefore {
			reread = true
		}
		m.sleep(10 * time.Second)
	}
	if !reread {
		m.fatalf("%s never read its history again inside the window, so its refusal says nothing about a resource that is running", name)
	}
	// The database is the claim. A replay would re-run the first migration,
	// whose insert is not idempotent.
	if m.uncertainWidgetRows() != "3" {
		m.fatalf("the %s rows were doubled, so the run was replayed once its refusal had been overwritten", m.engine.name)
	}
	m.logf("PASS %s kept refusing a run nobody accounted for across another refusal", m.engine.kind)
}

// assertUnresolvedRunAcknowledgedByAPerson is the recovery #446 replaced. A
// person used to settle the record by writing status, which needed the
// manager's own authority and recorded nobody. The chart now refuses a status
// write from anyone but the manager -- a cluster administrator included --
// and refuses the same hand on the copy of the record the resource carries in
// its metadata, which is what a restore that drops status keeps. What settles
// the record is an acknowledgment a person creates: admission stamps who made
// it, the controller takes it only for the run it names, and the resolution
// names that person. The person is a name the cluster has never seen, in a
// group bound here to the approver ClusterRole the chart ships, so what lets
// them acknowledge is the release's own RBAC.
func (m *migrationRun) assertUnresolvedRunAcknowledgedByAPerson() {
	m.t.Helper()
	name := m.uncertainMigration()
	current := m.migration(name)
	if current.Status.UnresolvedRun == nil {
		m.fatalf("%s carries no unresolved run to acknowledge", name)
	}
	m.acknowledgeUnresolvedRun(name, current.Status.UnresolvedRun.OperationID)
}

// acknowledgeUnresolvedRun uses the installed approver role to account for
// one exact run, then verifies a fresh database reading. The caller must
// establish that the run can no longer write and inspect its database effects
// before asking a person to settle it.
func (m *migrationRun) acknowledgeUnresolvedRun(name, operation string) {
	m.t.Helper()
	current := m.migration(name)
	if operation == "" || current.Status.UnresolvedRun == nil || current.Status.UnresolvedRun.OperationID != operation {
		m.fatalf("%s no longer carries the exact unresolved run to acknowledge", name)
	}
	if current.UID == "" {
		m.fatalf("%s carries no UID", name)
	}
	// The copy names the same run, the same way.
	if !unresolvedRunCopied(current) {
		m.fatalf("%s carries no copy of its unresolved run for a restore to keep", name)
	}

	m.logf("clearing the %s unresolved run by hand, as a cluster administrator", m.engine.kind)
	target := &ptahv1alpha1.PtahMigration{}
	target.Namespace, target.Name = m.in.TestNamespace, name
	err := m.cluster.Client.Status().Patch(m.ctx, target,
		client.RawPatch(types.JSONPatchType, []byte(`[{"op":"remove","path":"/status/unresolvedRun"}]`)))
	if err == nil {
		m.fatalf("a cluster administrator cleared the unresolved run of %s through status", name)
	}
	m.scan([]byte(err.Error()), "the refused status write")
	if !strings.Contains(err.Error(), "Ptah status is written only by the operator's manager") {
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		m.fatalf("the status write on %s was refused by something other than the status guard", name)
	}
	target = &ptahv1alpha1.PtahMigration{}
	target.Namespace, target.Name = m.in.TestNamespace, name
	err = m.mergePatch(target, map[string]any{"metadata": map[string]any{
		"annotations": map[string]any{ptahv1alpha1.UnresolvedRunAnnotation: nil},
	}})
	if err == nil {
		m.fatalf("a cluster administrator removed the copy of the unresolved run from %s", name)
	}
	m.scan([]byte(err.Error()), "the refused copy removal")
	if !strings.Contains(err.Error(), "only the manager changes it") {
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		m.fatalf("the copy removal on %s was refused by something other than the unresolved-run guard", name)
	}
	if !unresolvedRunStands(m.migration(name), operation) {
		m.fatalf("a refused write moved the unresolved run of %s", name)
	}

	person := "e2e-acknowledger-" + m.engine.name + "@example.test"
	group := "e2e:acknowledgers-" + m.engine.name
	acknowledgmentName := name + "-run-accounted-for"
	roles := &rbacv1.ClusterRoleList{}
	m.check(m.cluster.Client.List(m.ctx, roles, client.MatchingLabels{"app.kubernetes.io/name": "ptah-operator"}),
		"list the release's ClusterRoles")
	var approverRoles []string
	for _, role := range roles.Items {
		if strings.HasSuffix(role.Name, "-approver") {
			approverRoles = append(approverRoles, role.Name)
		}
	}
	if len(approverRoles) != 1 {
		m.fatalf("the release installed no approver ClusterRole to bind the acknowledger to: want exactly one, found %v", approverRoles)
	}
	if err := m.apply(map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"namespace": m.in.TestNamespace, "name": "e2e-acknowledgers-" + m.engine.name},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": approverRoles[0]},
		"subjects": []any{map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Group", "name": group}},
	}); err != nil {
		m.fatalf("the acknowledger could not be bound to %s: %v", approverRoles[0], err)
	}
	acknowledger, err := m.cluster.As(rest.ImpersonationConfig{UserName: person, Groups: []string{group}})
	m.check(err, "act as %s", person)
	m.logf("acknowledging the %s run as %s", m.engine.kind, person)
	if err := harness.CreateAfterRoleBinding(m.ctx, acknowledger, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahMigrationRunAcknowledgment",
		"metadata": map[string]any{"namespace": m.in.TestNamespace, "name": acknowledgmentName},
		"spec": map[string]any{
			"migrationRef": map[string]any{"name": name, "uid": string(current.UID)},
			"operationID":  operation,
		},
	}}, client.FieldOwner(harness.FieldOwner), client.FieldValidation("Strict")); err != nil {
		m.scan([]byte(err.Error()), "the refused acknowledgment")
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		m.fatalf("%s could not acknowledge the run of %s", person, name)
	}

	// The document that matched is the one held to the rest of the claim.
	var acknowledged, last *ptahv1alpha1.PtahMigration
	if !m.within(2*time.Second, func() bool {
		last = m.migration(name)
		if runAcknowledged(last, operation, acknowledgmentName, person) {
			acknowledged = last
			return true
		}
		return false
	}) {
		m.printScanned("the unsettled run", map[string]any{
			"phase": last.Status.Phase, "unresolvedRun": last.Status.UnresolvedRun, "resolvedRun": last.Status.ResolvedRun,
			"copy": last.Annotations[ptahv1alpha1.UnresolvedRunAnnotation],
		})
		m.fatalf("%s did not settle its unresolved run in the name of %s within %s", name, person, waitTimeout)
	}
	acknowledgment := &ptahv1alpha1.PtahMigrationRunAcknowledgment{}
	m.check(m.get(acknowledgmentName, acknowledgment), "the acknowledgment of %s could not be read", name)
	// Stamped from the request that created it, and named by UID in the
	// resolution: the resolution is this acknowledgment's, not one like it.
	if !acknowledgmentNamesResolution(acknowledgment, acknowledged, person, group) {
		m.fatalf("the resolution of %s does not name the acknowledgment %s made", name, person)
	}
	if !m.within(2*time.Second, func() bool {
		acknowledgment = &ptahv1alpha1.PtahMigrationRunAcknowledgment{}
		m.check(m.get(acknowledgmentName, acknowledgment), "the acknowledgment of %s could not be read", name)
		return acknowledgmentConsumed(acknowledgment)
	}) {
		m.fatalf("the acknowledgment of %s was never answered as consumed", name)
	}
	// The acknowledgment accounts for the database; it does not say what the
	// database holds now. The resource reads it again, dated by its own
	// record, before it decides anything.
	if acknowledged.Status.ResolvedRun.ResolvedAt.IsZero() {
		m.fatalf("the resolution of %s carries no time", name)
	}
	resolvedAt := acknowledged.Status.ResolvedRun.ResolvedAt.Time
	if !m.within(migrationPoll, func() bool { return historyReadAfter(m.migration(name), resolvedAt) }) {
		m.fatalf("%s did not read its database again after the acknowledgment", name)
	}
	m.logf("PASS %s refused a status write and settled its run in the name of %s", m.engine.kind, person)
}

// printScanned prints a credential-free projection on standard error, or says
// it was withheld when it matched a protected credential.
func (m *migrationRun) printScanned(what string, value any) {
	content, err := json.Marshal(value)
	if err != nil {
		return
	}
	if m.scanner.leaks(content) {
		_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: %s is withheld: it matched a protected credential\n", what)
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: %s: %s\n", what, content)
}

// reportGatedState prints what a gated migration, its Jobs and its Pods were
// doing when a wait ran out, and where the gate was open. A row that times out
// otherwise prints only its own message, and a failure costs a full matrix
// before anyone learns which state it was stuck in.
func (m *migrationRun) reportGatedState(name string, withWorkloads bool) {
	migration := &ptahv1alpha1.PtahMigration{}
	if m.get(name, migration) != nil {
		migration = nil
	}
	var jobs []batchv1.Job
	var pods []corev1.Pod
	if withWorkloads {
		jobList := &batchv1.JobList{}
		if m.list(jobList, client.MatchingLabels{labelMigration: name}) == nil {
			jobs = jobList.Items
		}
		podList := &corev1.PodList{}
		if m.list(podList, client.MatchingLabels{labelMigration: name}) == nil {
			pods = podList.Items
		}
	}
	var open []string
	nodes := &corev1.NodeList{}
	if m.cluster.Client.List(m.ctx, nodes, client.HasLabels{applyGateLabel}) == nil {
		for _, node := range nodes.Items {
			open = append(open, node.Name)
		}
	}
	// The report cuts condition messages short, and a credential cut at the
	// boundary would no longer match the scanner. So what the report is made
	// of is scanned whole first, and the report is withheld on a match.
	whole, err := json.Marshal(map[string]any{"migration": migration, "jobs": jobs, "pods": pods})
	if err != nil || !m.scanner.ready() || m.scanner.leaks(whole) {
		_, _ = fmt.Fprintf(os.Stderr, "e2e migrations: the %s state is withheld: it could not be cleared of credentials\n", name)
		return
	}
	report := lateDispatchReport(migration, jobs, pods, open)
	if !withWorkloads {
		delete(report, "jobs")
		delete(report, "pods")
	}
	m.printScanned(name+" state when the wait ended", report)
}

// gatedExecution is the execution settings of a migration whose operation
// Pods schedule only while the apply gate is open.
func gatedExecution() map[string]any {
	return map[string]any{
		"activeDeadlineSeconds": int64(300), "failureRetryInterval": "10s", "connectTimeout": "30s",
		"nodeSelector": map[string]any{applyGateLabel: "open"},
	}
}

// approveGated approves a gated migration's plan, reading the identities it
// binds, and fails with the row's own words when one is missing.
func (m *migrationRun) approveGated(name, plan, what string) {
	m.t.Helper()
	migration := &ptahv1alpha1.PtahMigration{}
	published := &ptahv1alpha1.PtahMigrationPlan{}
	_ = m.get(name, migration)
	_ = m.get(plan, published)
	if migration.UID == "" || published.UID == "" || published.Spec.Fingerprint == "" {
		m.fatalf("%s or its plan %s carries no identity to approve", name, plan)
	}
	if err := m.create(migrationApprovalDocument(m.in.TestNamespace, name+"-approval", name, string(migration.UID),
		plan, string(published.UID), published.Spec.Fingerprint)); err != nil {
		m.scan([]byte(err.Error()), "the refused "+what+" approval")
		m.fatalf("the %s %s approval could not be created: %v", m.engine.name, what, err)
	}
}

// waitForGatedPods waits for the Apply Job's Pods to be held off every node by
// the closed gate. A Job whose Pod has not been created yet reads exactly like
// one whose Pod cannot be placed, so the reading requires a Pod.
func (m *migrationRun) waitForGatedPods(jobName string, failPlaced bool) bool {
	m.t.Helper()
	return m.within(2*time.Second, func() bool {
		pods := &corev1.PodList{}
		if err := m.list(pods, client.MatchingLabels{"job-name": jobName}); err != nil {
			m.fatalf("the %s Apply Pods could not be read while the gate was closed: %v", m.engine.name, err)
		}
		if gatedApplyPods(pods.Items) {
			return true
		}
		// A Pod that reached a node is not something waiting longer fixes.
		if failPlaced && anyPodPlaced(pods.Items) {
			m.fatalf("the %s Apply Pod reached a node while the gate was closed, so the gate is not what held it", m.engine.name)
		}
		return false
	})
}

// lateDispatchProof tells the absolute window an Apply carries apart from the
// Job's own deadline. status.activeOperation.dispatchNotAfter is stamped when
// the claim is made, and the Job outlives it by workload.JobDeadlineGrace, so
// a Pod held until the window has closed and released inside the grace starts
// in a Job that is still alive, and the only thing left to stop it is the
// runner reading the absolute deadline. The nodeSelector is what removes the
// race: without it the Apply could finish before the window closed. It
// reaches every operation Job, so the read-only chain runs with the gate open,
// and the gate closes between the plan and the approval.
func (m *migrationRun) lateDispatchProof() {
	m.t.Helper()
	name, database := "e2e-late-dispatch-"+m.engine.name, "ptah_e2e_late_dispatch"
	m.isolatedDatabase(database, "e2e-"+m.engine.name+"-late-dispatch-db")
	audit := &databaseSQLAudit{t: m.t, ctx: m.ctx, cluster: m.cluster, namespace: m.in.TestNamespace, engine: m.engine.name}
	initial := audit.snapshot()
	// Open first. The selector reaches the Resolve, Verify and History Jobs as
	// well, so a gate that is closed here strands the first of them.
	m.openApplyGate()
	// OnApproval, because the approval is what lets this proof choose the
	// moment the Apply is claimed; an hour of interval, because nothing here
	// wants a refresh between the gate closing and the claim. The deadline is
	// every operation Job's, so it is the three hundred seconds every fixture
	// uses, and the hold below waits out that same window.
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: "e2e-" + m.engine.name + "-late-dispatch-db", reference: m.reference(""),
		coordinationKey: "e2e/late-dispatch/" + m.engine.name, apply: "OnApproval", interval: "1h",
		execution: gatedExecution(),
	}))
	fixtureUID := string(m.migration(name).UID)
	// The read-only chain has to finish before there is an Apply to delay.
	var plan string
	if !m.within(migrationPoll, func() bool {
		var ok bool
		plan, ok = latePlanPublished(m.migration(name))
		return ok
	}) {
		m.reportGatedState(name, true)
		m.fatalf("%s did not publish a plan to approve within %s", name, waitTimeout)
	}
	audit.assertRecords(initial, audit.snapshot(), audit.terminalPod(map[string]string{labelMigration: name, labelOperation: "history"}, ""), true)
	m.logf("closing the gate before approving the %s plan", m.engine.kind)
	m.closeApplyGate()
	// Approving is what claims the Apply, and the gate is already closed, so
	// the Job this creates is the one whose Pod cannot start.
	m.approveGated(name, plan, "late-dispatch")
	// The Job to hold is the one the resource says it dispatched, and the
	// window to wait out is the one the resource persisted beside it, read
	// from the same document.
	var claim lateApplyClaim
	if !m.within(time.Second, func() bool {
		var ok bool
		claim, ok = lateApplyClaimed(m.migration(name))
		return ok
	}) {
		m.reportGatedState(name, true)
		m.fatalf("%s did not claim an Apply with an absolute window within %s", name, waitTimeout)
	}
	live := &batchv1.Job{}
	if err := m.get(claim.jobName, live); err != nil || string(live.UID) != claim.jobUID {
		m.fatalf("the %s Apply Job under that name is not the one the resource dispatched", m.engine.name)
	}
	if !m.waitForGatedPods(claim.jobName, true) {
		m.reportGatedState(name, true)
		m.fatalf("the %s Apply never produced a Pod held off every node, so nothing here shows the gate is what delayed it", m.engine.name)
	}
	// The hold is derived from the deadline the resource persisted rather
	// than from a fixed sleep: a window the operator computes differently
	// moves the hold with it.
	windowEnd := claim.dispatchNotAfter.Unix()
	if windowEnd <= time.Now().Unix() {
		m.fatalf("the %s Apply window had already closed when it was claimed, so this row would hold nothing", m.engine.name)
	}
	m.logf("holding the %s Apply Pod until its window closes at %s", m.engine.kind, claim.dispatchNotAfter.UTC().Format(time.RFC3339))
	// One second past it, so what follows is on the far side of a boundary
	// the runner treats as exclusive.
	for time.Now().Unix() <= windowEnd {
		m.sleep(2 * time.Second)
	}
	// The grace is the whole premise, so its absence is reported as itself
	// rather than as a refusal that never arrived.
	job := &batchv1.Job{}
	m.check(m.get(claim.jobName, job), "the %s Apply Job could not be read after its window closed", m.engine.name)
	if jobTerminal(job) {
		m.reportGatedState(name, true)
		m.fatalf("the %s Apply Job had already ended when its window closed, so it does not outlive the window by workload.JobDeadlineGrace", m.engine.name)
	}
	m.logf("opening the gate on the %s Apply Pod after its window closed", m.engine.kind)
	beforeRefusal := audit.snapshot()
	m.openApplyGate()
	m.assertLateDispatchNeverReachesTheDatabase(name, database, claim.jobUID)
	m.closeApplyGate()
	audit.assertRecords(beforeRefusal, audit.snapshot(), audit.terminalPod(map[string]string{"job-name": claim.jobName}, claim.jobUID), false)
	// The old alert phase consumed this unresolved fixture. The current phase
	// induces its own incidents from zero, so retain the completed refusal and
	// remove only this exact fixture before it can contaminate later rows.
	var retired *ptahv1alpha1.PtahMigration
	if !m.within(time.Second, func() bool {
		retired = m.migration(name)
		return lateFixtureRetirable(retired, fixtureUID, claim.jobUID)
	}) {
		m.reportGatedState(name, true)
		m.fatalf("%s is not the proved, drained late-dispatch fixture", name)
	}
	pod := audit.terminalPod(map[string]string{"job-name": claim.jobName}, claim.jobUID)
	m.check(m.get(claim.jobName, job), "retain the terminal late-dispatch Job")
	m.retainMigrationFixture(retired, job, pod, beforeRefusal, audit.snapshot())
	m.check(storedStateDeleteExact(m.ctx, m.cluster, retired), "finalize only the proved late-dispatch fixture")
	m.assertNoNewApplyJob([]string{claim.jobUID}, "while retiring the late-dispatch fixture", name)
	audit.assertRecords(beforeRefusal, audit.snapshot(), pod, false)
	m.logf("PASS %s refused an Apply Pod that started after its window closed; exact fixture finalized", m.engine.kind)
	audit.close()
}

func (m *migrationRun) assertLateDispatchNeverReachesTheDatabase(name, database, jobUID string) {
	m.t.Helper()
	// The refusal is the runner's, and the message it produced is what says
	// so.
	if !m.within(migrationPoll, func() bool { return lateRefusalRecorded(m.migration(name).Status, jobUID) }) {
		m.reportGatedState(name, true)
		m.fatalf("%s never reported that its Apply was refused for an expired dispatch window", name)
	}
	// The runner stopped before it opened the database, so the sequence is
	// still entirely unapplied, the same reading as before the Pod ever ran.
	schema := m.engine.currentSchemaFilter()
	if m.query("SELECT count(*) FROM information_schema.tables WHERE "+schema+" AND table_name='schema_migrations'", database) != "0" {
		if versions := m.query("SELECT count(*) FROM schema_migrations", database); versions != "0" {
			m.fatalf("the %s Apply that ran past its window recorded %s migrations", m.engine.name, versions)
		}
	}
	if m.query("SELECT count(*) FROM information_schema.tables WHERE "+schema+" AND table_name='e2e_migration_widgets'", database) != "0" {
		m.fatalf("the %s Apply that ran past its window created the table its first migration creates", m.engine.name)
	}
	// And nothing replaced it: a replacement would have to be a new claim,
	// the replay the unresolved record exists to prevent. The baseline is the
	// one Job this proof refused, from the UID the resource named: a listing
	// taken now would already contain any replacement.
	m.assertNoNewApplyJob([]string{jobUID}, "after its window closed", name)
}

// restoredHistoryProof restores a history backwards between the approval and
// the run. The operator checks a plan's history against its last reading
// before it dispatches; what decides is the selection Ptah makes under the
// migration lock, which the runner holds to the approved sequence. The
// resource applies the two-migration artifact, moves to the three-migration
// one, whose plan approves [3] alone, and the gate holds the Apply Pod while
// the database is restored to version 1. Released, the run selects [2 3], and
// the row asserts that it ran none of it, that the resource says so by name,
// and that only a fresh approval of [2 3] resumes the migration.
func (m *migrationRun) restoredHistoryProof() {
	m.t.Helper()
	name, database := "e2e-restore-"+m.engine.name, "ptah_e2e_restore"
	secret := "e2e-" + m.engine.name + "-restore-db"
	var auditUser string
	if m.engine.name == "mysql" {
		auditUser = m.isolatedMySQLAuditDatabase(database, secret)
	} else {
		m.isolatedDatabase(database, secret)
	}
	m.publish("restore-older", m.fixtureDir("-older"), m.reference("-restore-older"))
	m.publish("restore", m.fixtureDir(""), m.reference("-restore"))
	m.openApplyGate()
	// Always, on the two-migration artifact, so the database reaches version 2
	// with nothing to approve. The gate's selector is there from the start,
	// and the gate is open, because it reaches every operation Job.
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: name, secret: secret, reference: m.reference("-restore-older"),
		coordinationKey: "e2e/restore/" + m.engine.name, apply: "Always", interval: "1h",
		execution: gatedExecution(),
	}))
	revisions := func() string { return m.query(restoreRevisionsQuery(m.engine.name), database) }
	// Version 2, applied by the operator itself.
	initial := m.waitForGenerationInSync(name)
	if !restoreInSyncApplied(initial.Status) || initial.Status.LastRun.JobUID == "" {
		m.fatalf("%s did not retain its initial successful Apply identity", name)
	}
	initialJobUID := string(initial.Status.LastRun.JobUID)
	if applied := revisions(); applied != "1,2" {
		m.reportGatedState(name, false)
		m.fatalf("%s did not bring its database to version 2; it records [%s]", name, applied)
	}

	// The three-migration artifact, and a decision to make: the plan approves
	// exactly [3], since the history it was computed against holds 1 and 2.
	m.patchMigration(name, map[string]any{"spec": map[string]any{
		"artifact": map[string]any{"ociRef": m.reference("-restore")}, "policy": map[string]any{"apply": "OnApproval"},
	}})
	approved := m.waitForRestorePlan(name, "", nil)
	if versions := planVersionList(m.planOf(approved)); versions != "3" {
		m.fatalf("the plan %s published approves [%s], and this row needs [3]", name, versions)
	}
	m.logf("closing the gate before approving the %s plan for [3]", m.engine.kind)
	m.closeApplyGate()
	m.approveGated(name, approved, "restore")
	jobUID := m.waitForRestoreApplyHeld(name)
	// The restore: the database goes back to version 1, objects and revision
	// row both, while the approved run is held.
	m.logf("restoring the %s database to version 1 under a held Apply", m.engine.kind)
	if _, err := m.sqlStatement(database, "ALTER TABLE e2e_migration_widgets DROP COLUMN color"); err != nil {
		m.fatalf("migration 2's column could not be dropped: %v", err)
	}
	if _, err := m.sqlStatement(database, "DELETE FROM schema_migrations WHERE version = 2"); err != nil {
		m.fatalf("migration 2's revision row could not be deleted: %v", err)
	}
	if restored := revisions(); restored != "1" {
		m.fatalf("the restore left the %s database recording [%s], and this row needs [1]", m.engine.name, restored)
	}
	// The destructive restore is a harness action, completed while the Apply
	// is unscheduled. Audit every statement after it, before opening that gate.
	audit := &databaseSQLAudit{t: m.t, ctx: m.ctx, cluster: m.cluster, namespace: m.in.TestNamespace, engine: m.engine.name}
	var pgBefore []byte
	var mysqlBefore []mysqlStatementRecord
	if m.engine.name == "postgresql" {
		audit.snapshot()
		pgBefore = audit.pgPrefix
	} else {
		mysqlBefore = audit.mysqlStatementSnapshot()
	}
	inventory := &migrationSQLInventory{jobs: map[types.UID]batchv1.Job{}, pods: map[types.UID]corev1.Pod{}}
	m.captureMigrationSQLInventory(name, inventory)
	m.openApplyGate()
	// The refusal is read from the run the approval claimed, by its Job UID,
	// and by the message that names both lists.
	if !m.within(migrationPoll, func() bool {
		resource := m.migration(name)
		m.captureMigrationSQLInventory(name, inventory)
		return restoreRefused(resource.Status, jobUID)
	}) {
		m.reportGatedState(name, false)
		m.fatalf("%s never recorded that its approved [3] was refused for a selection of [2 3]", name)
	}
	// Nothing of [2 3] ran: the database is exactly where the restore left it.
	if after := revisions(); after != "1" {
		m.fatalf("after the refused run the %s database records [%s]; the selection ran", m.engine.name, after)
	}
	if m.widgetColumnCount("color", database) != "0" {
		m.fatalf("after the refused run the %s database has migration 2's column again; the selection ran", m.engine.name)
	}
	// And the resource asks again, for what the history now needs.
	next := m.waitForRestorePlan(name, approved, inventory)
	fresh := m.planOf(next)
	if versions := planVersionList(fresh); versions != "2 3" {
		m.fatalf("after the restore %s asks to approve [%s], and the history needs [2 3]", name, versions)
	}
	m.assertNoNewApplyJob([]string{initialJobUID, jobUID}, "before fresh approval of the restored history", name)
	if revisions() != "1" || m.widgetColumnCount("color", database) != "0" {
		m.fatalf("%s changed the restored database while the fresh plan awaited approval", name)
	}
	beforeApply := audit.snapshot()
	m.assertRestoredHistoryRefusalSQL(audit, pgBefore, mysqlBefore, database, auditUser, jobUID, m.migration(name), inventory)
	m.check(m.approve(name+"-current", name, fresh.Name, string(fresh.UID), fresh.Spec.Fingerprint),
		"approve the plan for the restored history")
	converged := m.waitForMigration(name, "a fresh successful Apply of the restored history", migrationPoll,
		func(resource *ptahv1alpha1.PtahMigration) bool {
			return restoredHistoryApplied(resource, initialJobUID, jobUID)
		})
	run := converged.Status.LastRun
	proofPod := audit.terminalPod(map[string]string{"job-name": run.JobName}, string(run.JobUID))
	afterApply := audit.snapshot()
	audit.assertRecords(beforeApply, afterApply, proofPod, true)
	audit.close()
	m.assertNoNewApplyJob([]string{initialJobUID, jobUID, string(run.JobUID)}, "after the restored history converged", name)
	if revisions() != "1,2,3" || m.widgetColumnCount("color", database) != "1" ||
		m.query("SELECT count(*) FROM e2e_migration_widgets", database) != "3" ||
		m.query("SELECT color FROM e2e_migration_widgets WHERE id=1", database) != "blue" {
		m.fatalf("%s did not establish the approved schema, rows and history after restore", name)
	}
	// This fixture's scheduler gate closes at the end of the row. Leaving the
	// resource behind would strand its next read and page during alert tests.
	proofJob := &batchv1.Job{}
	m.check(m.get(run.JobName, proofJob), "retain the completed restored-history Job")
	m.retainMigrationFixture(converged, proofJob, proofPod, beforeApply, afterApply)
	m.check(storedStateDeleteExact(m.ctx, m.cluster, converged), "finalize only the proved restored-history fixture")
	m.closeApplyGate()
	m.logf("PASS %s refused the stale [3] decision, then applied [2 3] only after fresh approval of the restored history; exact fixture finalized", m.engine.kind)
}

// These fixture documents outlive both their source resources and the phase's
// temporary working directory. The phase log binds the private bytes by digest.
func (m *migrationRun) retainMigrationFixture(resource *ptahv1alpha1.PtahMigration, job *batchv1.Job, pod *corev1.Pod, before, after sqlAuditCounts) {
	evidence, err := os.MkdirTemp("", "ptah-e2e-migration-fixture-evidence.")
	m.check(err, "create private retained migration fixture evidence")
	body, err := json.Marshal(map[string]any{"migration": resource, "job": job, "pod": pod, "sqlBefore": before, "sqlAfter": after})
	m.check(err, "encode the proved migration fixture")
	m.check(os.WriteFile(filepath.Join(evidence, "proof.json"), body, 0600), "retain migration fixture evidence before deleting its source")
	m.logf("retained private migration fixture evidence: %s sha256:%x", evidence, sha256.Sum256(body))
}

// waitForRestorePlan waits until the resource asks for a decision on a plan
// other than the previous one, and returns it.
func (m *migrationRun) waitForRestorePlan(name, previous string, inventory *migrationSQLInventory) string {
	m.t.Helper()
	var plan string
	if !m.within(migrationPoll, func() bool {
		var ok bool
		plan, ok = decisionOnNewPlan(m.migration(name).Status, previous)
		if inventory != nil {
			m.captureMigrationSQLInventory(name, inventory)
		}
		return ok
	}) {
		m.reportGatedState(name, false)
		m.fatalf("%s did not ask for a decision on a new plan within %s", name, waitTimeout)
	}
	return plan
}

// waitForRestoreApplyHeld waits for the approved run to be claimed and its
// Pod held off every node, and returns the claimed Job's UID.
func (m *migrationRun) waitForRestoreApplyHeld(name string) string {
	m.t.Helper()
	var jobName, jobUID string
	if !m.within(time.Second, func() bool {
		var ok bool
		jobName, jobUID, ok = uncertainApplyClaimed(m.migration(name))
		return ok
	}) {
		m.reportGatedState(name, false)
		m.fatalf("%s did not claim the approved Apply within %s", name, waitTimeout)
	}
	if !m.waitForGatedPods(jobName, false) {
		m.reportGatedState(name, false)
		m.fatalf("the %s Apply never produced a Pod held off every node, so the restore could race the run", m.engine.name)
	}
	return jobUID
}
