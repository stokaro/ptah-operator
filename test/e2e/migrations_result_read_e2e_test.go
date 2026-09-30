//go:build e2e

package e2e

import (
	"maps"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func (m *migrationRun) hungResultReadProof() {
	m.t.Helper()
	r := &isolatedNodeRow{m: m, name: "e2e-hung-result-" + m.engine.name,
		database: "ptah_e2e_hung_result", secret: "e2e-" + m.engine.name + "-hung-result-db", node: m.miIsolatedNode()}
	// The admitted name ceiling must still produce valid Job labels and an
	// executable path, through the recovery observation as well as Apply.
	r.name += strings.Repeat("x", 63-len(r.name))
	r.requireWorker()
	m.isolatedDatabase(r.database, r.secret)
	healthy := "e2e-result-progress-" + m.engine.name
	healthyDB, healthySecret := "ptah_e2e_result_progress", "e2e-"+m.engine.name+"-result-progress-db"
	m.isolatedDatabase(healthyDB, healthySecret)
	m.openApplyGate()
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: r.name, secret: r.secret, reference: m.reference(""), coordinationKey: "e2e/hung-result/" + m.engine.name,
		apply: "OnApproval", interval: "1h",
		execution: map[string]any{
			"activeDeadlineSeconds": int64(300), "failureRetryInterval": "10s", "connectTimeout": "30s",
			"nodeSelector": map[string]any{isolationNodeKey: "true", applyGateLabel: "open"},
			"tolerations":  []any{map[string]any{"key": isolationNodeKey, "operator": "Equal", "value": "true", "effect": "NoSchedule"}},
		},
	}))
	m.mustCreate(m.migrationDocument(migrationSpec{
		name: healthy, secret: healthySecret, reference: m.reference(""), coordinationKey: "e2e/result-progress/" + m.engine.name,
		apply: "OnApproval", interval: "1h",
	}))
	inventory := &migrationSQLInventory{jobs: map[types.UID]batchv1.Job{}, pods: map[types.UID]corev1.Pod{}}
	waitPlan := func(name string) *ptahv1alpha1.PtahMigrationPlan {
		resource := m.waitForMigration(name, "a plan awaiting approval", migrationPoll,
			func(resource *ptahv1alpha1.PtahMigration) bool {
				if name == r.name {
					m.captureMigrationSQLInventory(name, inventory)
				}
				return resource.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval && resource.Status.Plan != nil && resource.Status.ActiveOperation == nil
			})
		return m.planOf(resource.Status.Plan.Name)
	}
	plan, healthyPlan := waitPlan(r.name), waitPlan(healthy)
	created := m.migration(r.name)
	refused := &ptahv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Name: r.name + "x", Namespace: created.Namespace}, Spec: *created.Spec.DeepCopy()}
	m.check(resourceNameRefusal(m.cluster.Client.Create(m.ctx, refused), "PtahMigration", refused.Name), "refuse the migration name immediately above its executable limit")
	if !apierrors.IsNotFound(m.get(refused.Name, &ptahv1alpha1.PtahMigration{})) {
		m.fatalf("the refused 64-byte migration name was stored")
	}
	audit := &databaseSQLAudit{t: m.t, ctx: m.ctx, cluster: m.cluster, namespace: m.in.TestNamespace, engine: m.engine.name}
	beforeSQL := audit.snapshot()
	m.closeApplyGate()
	m.check(m.approve("e2e-migration-hung-approval-"+m.engine.name, r.name, plan.Name, string(plan.UID), plan.Spec.Fingerprint), "approve the operation whose result will hang")
	r.waitForClaim()
	r.findLease()
	m.poll("the Apply Pod held off every node", time.Second, func() bool {
		pods := &corev1.PodList{}
		m.check(m.list(pods, client.MatchingLabels{"job-name": r.claim.job}), "read held Apply Pod")
		if len(pods.Items) == 1 && pods.Items[0].Spec.NodeName != "" {
			m.fatalf("Apply escaped the closed scheduling gate")
		}
		if len(pods.Items) != 1 || !gatedApplyPods(pods.Items) {
			return false
		}
		r.pod, r.podUID = pods.Items[0].Name, string(pods.Items[0].UID)
		return true
	})
	fault := (&logStall{t: m.t, ctx: m.ctx, cluster: m.cluster, dockerContext: m.in.DockerContext,
		node: m.miIsolatedNode(), workDir: m.workDir, namespace: m.in.TestNamespace, suffix: m.engine.name}).start(r.pod)
	m.openApplyGate()
	m.poll("a completed Apply and an unfinished result read", time.Second, func() bool {
		job := &batchv1.Job{}
		m.check(m.get(r.claim.job, job), "read Apply Job")
		return string(job.UID) == r.claim.jobUID && jobComplete(job) && slices.ContainsFunc(fault.readings(),
			func(reading logStallReading) bool { return reading.State == "started" })
	})
	// Approve the independent resource only after the controller is blocked
	// in the result stream. Its database must converge while that fault remains.
	m.check(m.approve(healthy+"-approval", healthy, healthyPlan.Name, string(healthyPlan.UID), healthyPlan.Spec.Fingerprint), "approve independent work during the stalled read")
	approval := &ptahv1alpha1.PtahMigrationApproval{}
	m.check(m.get(healthy+"-approval", approval), "read the independent approval timestamp")
	deadline := approval.CreationTimestamp.Add(180 * time.Second)
	timedOut, progressed := false, false
	for time.Now().Before(deadline) {
		m.captureMigrationSQLInventory(r.name, inventory)
		if !isolatedApplyHeld(m.migration(r.name), r.claim.jobUID, r.claim.epoch) {
			m.fatalf("%s discarded its claim while its result was unread", r.name)
		}
		lease := &coordinationv1.Lease{}
		m.check(m.cluster.Client.Get(m.ctx, types.NamespacedName{Namespace: r.leaseNamespace, Name: r.lease}, lease), "read the retained Apply Lease")
		if !heldAs(lease, r.holder, r.claim.epoch) {
			m.fatalf("the stalled Apply lost its Lease")
		}
		m.assertNoNewApplyJob([]string{r.claim.jobUID}, "while its result read hangs", r.name)
		events := &corev1.EventList{}
		m.check(m.list(events), "read result-read timeout Events")
		uid := m.migration(r.name).UID
		timedOut = timedOut || slices.ContainsFunc(events.Items, func(event corev1.Event) bool {
			return event.InvolvedObject.UID == uid && event.Reason == "ResultReadTimedOut"
		})
		current := m.migration(healthy)
		progressed = current.Status.Phase == ptahv1alpha1.MigrationPhaseInSync && current.Status.ObservedGeneration == current.Generation && current.Status.ActiveOperation == nil &&
			current.Status.LastRun != nil && current.Status.LastRun.FinishedAt != nil &&
			!current.Status.LastRun.FinishedAt.Time.Before(approval.CreationTimestamp.Time) &&
			!current.Status.LastRun.FinishedAt.Time.After(deadline)
		if timedOut && progressed {
			break
		}
		m.sleep(2 * time.Second)
	}
	if !timedOut || !progressed {
		m.fatalf("hung result read did not release the worker within 180s: timeout=%t independent convergence=%t", timedOut, progressed)
	}
	duration, bounded := logReadDuration(fault.readings())
	if !bounded {
		m.fatalf("the deliberately unfinished response was not canceled inside its 75s acceptance bound: %s", duration)
	}
	for _, database := range []string{r.database, healthyDB} {
		if count := m.query("SELECT count(*) FROM e2e_migration_widgets", database); count != "3" {
			m.fatalf("%s contains %s seeded rows while the result is unread", database, count)
		}
	}
	m.check(fault.stop(m.ctx), "restore real kubelet log reads")
	settled := m.waitForGenerationInSync(r.name)
	m.closeApplyGate()
	if settled.Status.LastRun == nil || string(settled.Status.LastRun.JobUID) != r.claim.jobUID {
		m.fatalf("recovery did not account for the original Apply")
	}
	m.assertNoNewApplyJob([]string{r.claim.jobUID}, "after result reads recovered", r.name)
	if count := m.query("SELECT count(*) FROM e2e_migration_widgets", r.database); count != "3" {
		m.fatalf("recovery replayed the Apply: %s rows", count)
	}
	if settled.UID != created.UID || settled.Status.LastRun.Outcome != ptahv1alpha1.MigrationRunOutcomeApplied ||
		!slices.Equal(settled.Status.LastRun.AppliedVersions, []int64{1, 2, 3}) {
		m.fatalf("maximum-name migration recovery did not account for its exact original plan")
	}
	m.poll("every maximum-name migration operation to complete with full labels", time.Second, func() bool {
		m.captureMigrationSQLInventory(r.name, inventory)
		return resourceNameWorkloads(created.Namespace, "PtahMigration", r.name, created.UID, types.UID(r.claim.jobUID),
			slices.Collect(maps.Values(inventory.jobs)), slices.Collect(maps.Values(inventory.pods))) == nil
	})
	job := &batchv1.Job{}
	m.check(m.get(r.claim.job, job), "read the maximum-name migration's original Apply")
	if string(job.UID) != r.claim.jobUID {
		m.fatalf("maximum-name migration recovery read a replacement Apply")
	}
	m.check(migrationExecutorApplyInputs(job, plan), "bind the maximum-name migration's original Apply to its exact plan")
	pod := audit.terminalPod(map[string]string{"job-name": r.claim.job}, r.claim.jobUID)
	if string(pod.UID) != r.podUID {
		m.fatalf("maximum-name migration recovery read a replacement Pod")
	}
	audit.assertRecords(beforeSQL, audit.snapshot(), pod, true)
	audit.close()
	for _, observed := range inventory.jobs {
		m.scanObject(observed, "maximum-name migration workload")
		m.scan(m.jobLogs(&observed), "maximum-name migration result")
	}
	for _, observed := range inventory.pods {
		m.scanObject(observed, "maximum-name migration Pod")
	}
	if m.query(restoreRevisionsQuery(m.engine.name), r.database) != "1,2,3" ||
		m.query("SELECT color FROM e2e_migration_widgets WHERE id = 1", r.database) != "blue" {
		m.fatalf("maximum-name migration recovery lost its expected database history or rows")
	}
	m.logf("PASS %s 63-byte migration name: unfinished result read canceled in %s; independent work converged within 180s; original Apply recovered without replay", m.engine.kind, duration)
	m.logf("PASS %s migration name boundary: 64 bytes refused at its CEL rule; every 63-byte operation retained its complete workload name; original Apply SQL and database history verified", m.engine.kind)
}
