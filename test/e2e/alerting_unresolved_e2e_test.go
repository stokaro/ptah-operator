//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Own the incident from before dispatch through the recovery notification.
// Each engine also proves Schema's distinct observation-based recovery.
func (a *alertingRun) unresolvedApply() {
	template := &ptahv1.PtahMigration{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.TestNamespace, Name: "e2e-migrations-postgresql"}, template), "read the native migration producer")
	registry, err := url.Parse(template.Spec.Artifact.OCIRef)
	a.check(err, "read the prepared migration registry")
	if registry.Scheme != "oci" || registry.Host == "" || registry.User != nil || template.Status.ExecutionBinding == nil || !alPinnedImage.MatchString(template.Status.ExecutionBinding.ExecutorImage) {
		a.fatalf("unresolved proof has no exact native producer")
	}
	for _, engineName := range []string{"postgresql", "mysql"} {
		engine, err := migrationEngineFor(engineName)
		a.check(err, "select the unresolved engine")
		m := &migrationRun{t: a.t, parent: a.t, ctx: a.ctx, cluster: a.cluster, engine: engine, workDir: a.workDir, registryHost: registry.Host, repository: "e2e-alert-unresolved", in: phases.MigrationsInputs{TestNamespace: a.in.TestNamespace, ExecutorImage: template.Status.ExecutionBinding.ExecutorImage}}
		credential := &corev1.Secret{}
		a.check(m.get(engine.sourceSecret, credential), "read the prepared engine credential")
		m.password = string(credential.Data["password"])
		if m.password == "" {
			a.fatalf("unresolved proof has no engine credential")
		}
		m.protect(m.password, a.credentials.Password)
		body, err := os.ReadFile(filepath.Join(m.fixtureDir("-uncertain"), "0000000003_settle_slowly.up.sql"))
		a.check(err, "read the exact migration eligible for manual repair")
		if !alUnresolvedSleepFixture(engineName, body) {
			a.fatalf("the unfinished migration no longer has an effect-free repair")
		}
		m.publish("alerts-unresolved", m.fixtureDir("-uncertain"), m.reference("-uncertain"))
		a.unresolvedMigrationCase(m, template)
		for _, object := range []client.Object{&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "e2e-push-migrations-" + engineName + "-alerts-unresolved", Namespace: a.in.TestNamespace}}, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "e2e-migrations-" + engineName + "-alerts-unresolved", Namespace: a.in.TestNamespace}}} {
			a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(object), object), "read the owned unresolved publisher")
			uid := object.GetUID()
			a.check(a.cluster.Client.Delete(a.ctx, object, client.Preconditions{UID: &uid}), "remove the owned unresolved publisher")
		}
		a.unresolvedSchemaCase(m)
	}
}

func (a *alertingRun) unresolvedMigrationCase(m *migrationRun, template *ptahv1.PtahMigration) {
	name := "e2e-alert-unresolved-" + m.engine.name
	database := "ptah_alert_unresolved_" + m.engine.name
	m.isolatedDatabase(database, name+"-db")
	secret := &corev1.Secret{}
	a.check(m.get(name+"-db", secret), "retain the isolated target identity")
	object, err := alNegativeFixture(template, name, secret.Name, ptahv1.ApplyPolicyOnApproval)
	a.check(err, "build the unresolved consumer")
	resource := object.(*ptahv1.PtahMigration)
	resource.Spec.Target.URLFrom.Key = "url"
	resource.Spec.Target.Engine = ptahv1.DatabaseEngine(m.engine.kind)
	resource.Spec.Target.CoordinationKey = "e2e/alert-unresolved/" + m.engine.name
	resource.Spec.Artifact.OCIRef = m.reference("-uncertain")
	resource.Spec.Interval.Duration = time.Hour
	resource.Spec.Execution.ActiveDeadlineSeconds = 300
	resource.Spec.Execution.NodeSelector = nil
	watcher, err := client.NewWithWatch(a.cluster.Config, client.Options{Scheme: a.cluster.Scheme})
	a.check(err, "open unresolved histories before dispatch")
	migrations := newStoredStateRecorder[*ptahv1.PtahMigration](a.t, a.ctx, watcher, name+"-migrations", a.in.TestNamespace, func() client.ObjectList { return &ptahv1.PtahMigrationList{} })
	jobs := newStoredStateRecorder[*batchv1.Job](a.t, a.ctx, watcher, name+"-jobs", a.in.TestNamespace, func() client.ObjectList { return &batchv1.JobList{} })
	pods := newStoredStateRecorder[*corev1.Pod](a.t, a.ctx, watcher, name+"-pods", a.in.TestNamespace, func() client.ObjectList { return &corev1.PodList{} })
	lease, managers := a.managerSnapshot()
	leader := haLeaderPodName(haLeaseHolder(lease))
	monitor := a.negativeMonitorIdentity()
	var names []string
	for _, p := range managers {
		names = append(names, p.Name)
	}
	checkRuntime := func() {
		l, p := a.managerSnapshot()
		if !alSameManagers(l, p, lease, managers) || !maps.Equal(monitor, a.negativeMonitorIdentity()) {
			a.fatalf("unresolved proof replaced a manager or monitoring process")
		}
		body, err := a.prometheus(a.ctx, "/api/v1/targets", nil)
		a.check(err, "read unresolved scrape targets")
		if !alTargetsReady(body, int(a.replicas)) {
			a.fatalf("unresolved proof lost a healthy target")
		}
		body, err = a.prometheus(a.ctx, "/api/v1/rules", nil)
		a.check(err, "read unresolved alert rules")
		if !alRulesLoaded(body) {
			a.fatalf("unresolved proof lost an enabled rule")
		}
	}
	check := func() {
		checkRuntime()
		for _, r := range []recorder{migrations, jobs, pods} {
			a.check(r.alive(), "retain unresolved histories")
		}
	}
	sql := func(statement string) string {
		body, err := m.sqlStatement(database, statement)
		a.check(err, "inspect the isolated database")
		return trimmedSQL(body)
	}
	a.check(a.create(resource), "create the independently approved migration")
	uid, generation := resource.UID, resource.Generation
	read := func() *ptahv1.PtahMigration {
		v := &ptahv1.PtahMigration{}
		a.check(m.get(name, v), "read the original unresolved consumer")
		if v.UID != uid || v.Generation != generation || v.Spec.Policy.Apply != ptahv1.ApplyPolicyOnApproval || v.DeletionTimestamp != nil {
			a.fatalf("unresolved consumer changed its identity or authorization policy")
		}
		return v
	}
	var approvals []*ptahv1.PtahMigrationApproval
	defer func() {
		if a.t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		for _, v := range approvals {
			u := v.UID
			a.check(a.cluster.Client.Delete(ctx, v, client.Preconditions{UID: &u}), "remove the owned unresolved approval")
		}
		a.check(a.cluster.Client.Delete(ctx, resource, client.Preconditions{UID: &uid}), "remove the recovered unresolved consumer")
		a.check(harness.Wait(ctx, "unresolved consumer finalization", 2*time.Minute, time.Second, func(ctx context.Context) (bool, string, error) {
			v := &ptahv1.PtahMigration{}
			err := a.cluster.Client.Get(ctx, client.ObjectKeyFromObject(resource), v)
			return apierrors.IsNotFound(err), "waiting for original consumer deletion", client.IgnoreNotFound(err)
		}), "finish unresolved fixture cleanup")
		statement := "DROP DATABASE " + database + " WITH (FORCE)"
		if m.engine.name == "mysql" {
			statement = "DROP DATABASE " + database
		}
		_, err := serverSQL(ctx, a.cluster, a.in.TestNamespace, m.engine, statement)
		a.check(err, "drop the owned unresolved database")
		u := secret.UID
		a.check(a.cluster.Client.Delete(ctx, secret, client.Preconditions{UID: &u}), "remove the owned unresolved target")
	}()
	var ready *ptahv1.PtahMigration
	a.check(harness.Wait(a.ctx, "the initial unresolved approval gate", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		check()
		ready = read()
		return alLockApprovalReady(ready), "waiting for native migration planning", nil
	}), "prepare the exact Apply plan")
	approve := func(v *ptahv1.PtahMigration, suffix string) *ptahv1.PtahMigrationApproval {
		plan := m.planOf(v.Status.Plan.Name)
		a.check(m.approve(name+suffix, name, plan.Name, string(plan.UID), plan.Spec.Fingerprint), "authorize only the current migration plan")
		approval := &ptahv1.PtahMigrationApproval{}
		a.check(m.get(name+suffix, approval), "retain the original approval identity")
		approvals = append(approvals, approval)
		return approval
	}
	all := &ptahv1.PtahMigrationList{}
	a.check(a.cluster.Client.List(a.ctx, all), "inventory all unresolved migrations before injection")
	query := `ALERTS{alertname="PtahOperatorUnresolvedApply",family="migration"}`
	if alUnresolvedMigrations(all.Items) != 0 || !a.noActiveAlerts(query) {
		a.fatalf("a pre-existing migration incident would mask this fault or its resolution")
	}
	from := a.deliveryCount()
	started := time.Now().UTC()
	baseline := a.unresolvedHistory(names, leader, "migration", started, "")
	if baseline.latest != 0 || !baseline.appeared.IsZero() {
		a.fatalf("no healthy native zero baseline")
	}
	originalPlan := m.planOf(ready.Status.Plan.Name)
	if originalPlan.UID != ready.Status.Plan.UID {
		a.fatalf("the initial plan was replaced")
	}
	approve(ready, "-original")
	var original *ptahv1.MigrationOperationStatus
	var originalJob *batchv1.Job
	var originalPod *corev1.Pod
	a.check(harness.Wait(a.ctx, "the real partial Apply before interruption", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		check()
		v := read()
		active := v.Status.ActiveOperation
		if active == nil || active.Type != ptahv1.MigrationOperationApply || !active.DispatchStarted || active.JobUID == "" {
			return false, "waiting for the dispatched Apply", nil
		}
		j := &batchv1.Job{}
		a.check(m.get(active.JobName, j), "read the dispatched Apply Job")
		list := &corev1.PodList{}
		a.check(m.list(list), "read the original Apply Pod")
		owned := ownedPods(list.Items, j.UID)
		if len(owned) == 0 {
			return false, "waiting for the original Pod", nil
		}
		if len(owned) != 1 || !alLockWorkload(j, &owned[0], alStalledReading(v)) {
			return false, "", fmt.Errorf("the dispatched Apply lost its unique workload")
		}
		if owned[0].Status.Phase != corev1.PodRunning {
			return false, "waiting for real SQL", nil
		}
		// The first two committed migrations prove this is a post-SQL fault.
		body, err := m.sqlStatement(database, "SELECT count(*) FROM schema_migrations WHERE version <= 2 AND state='applied'")
		if err != nil || trimmedSQL(body) != "2" {
			return false, "waiting for the first two native commits", nil
		}
		if sql("SELECT count(*) FROM schema_migrations WHERE version=3 AND state='applied'") != "0" {
			return false, "", fmt.Errorf("the slow migration finished before interruption")
		}
		original, originalJob, originalPod = active.DeepCopy(), j.DeepCopy(), owned[0].DeepCopy()
		return true, "", nil
	}), "retain a real in-flight mutation")
	jobUID := originalJob.UID
	a.check(a.cluster.Client.Delete(a.ctx, originalJob, client.Preconditions{UID: &jobUID}, client.PropagationPolicy(metav1.DeletePropagationBackground)), "remove only the interrupted Apply and its evidence")
	var unknown *ptahv1.PtahMigration
	a.check(harness.Wait(a.ctx, "the persisted unknown Apply", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		check()
		unknown = read()
		if unknown.Status.UnresolvedRun == nil {
			return false, "waiting for the native unknown result", nil
		}
		err := unresolvedRunRecorded(unknown.Status, original.JobName, string(original.JobUID))
		return err == nil, "", err
	}), "record the exact unresolved mutation")
	recorded := unknown.Status.UnresolvedRun.RecordedAt.Time
	if recorded.Before(started.Truncate(time.Second)) {
		a.fatalf("unresolved timestamp predates this fault")
	}
	labels := map[string]string{"family": "migration", "operator_namespace": a.in.OperatorNamespace, "operator_metrics_service": a.metricsService}
	firing, index := a.waitForDeliveryWithCheck(alMatch{status: "firing", alertName: alUnresolvedApply, labels: labels}, "the exact unresolved incident", alDetectionSlack, from, check)
	h := a.unresolvedHistory(names, leader, "migration", started, "firing")
	count, _ := alAlertedCount(firing.Annotations["summary"])
	if !alUnresolvedDelivered(firing, h, recorded) || count != "1" || firing.Labels["severity"] != "critical" || firing.Annotations["runbook_url"] != a.runbookBase+"#unresolved-gauges" || !alRunbookAnchor(a.operationsPage(), "unresolved-gauges") {
		a.fatalf("unresolved notification missed its persisted bound, exact count or runbook")
	}
	for _, key := range []string{"resource", "operation", "job", "pod", "plan", "execution"} {
		if _, ok := firing.Labels[key]; ok {
			a.fatalf("unresolved alert acquired an unbounded label")
		}
	}
	// A missing Job alone cannot authorize repair. Require retirement of the
	// original claim, terminal/absent Pods, and no database session of that user.
	a.check(harness.Wait(a.ctx, "the interrupted executor to become unable to write", alTimeout+time.Minute, time.Second, func(context.Context) (bool, string, error) {
		check()
		v := read()
		list := &corev1.PodList{}
		a.check(m.list(list), "read the interrupted workload boundary")
		settled, err := uncertainApplySettled(v.Status, original, list.Items)
		if err != nil || !settled {
			return false, "waiting for immutable execution and collection bounds", err
		}
		statement := "SELECT count(*) FROM pg_stat_activity WHERE datname='" + database + "' AND usename='" + migrationDatabaseUser + "' AND pid <> pg_backend_pid()"
		if m.engine.name == "mysql" {
			statement = "SELECT count(*) FROM information_schema.processlist WHERE DB='" + database + "' AND USER='" + migrationDatabaseUser + "'"
		}
		body, err := serverSQL(a.ctx, a.cluster, a.in.TestNamespace, m.engine, statement)
		return err == nil && trimmedSQL(body) == "0", "waiting for every old database session to finish", err
	}), "establish the runbook repair boundary")
	if sql("SELECT count(*) FROM schema_migrations WHERE version <= 2 AND state='applied'") != "2" || sql("SELECT count(*) FROM schema_migrations WHERE version=3 AND state='applied'") != "0" || sql("SELECT count(*) FROM e2e_migration_widgets") != "3" {
		a.fatalf("interruption changed the accounted database effects")
	}
	// Migration 3 only sleeps. Once its executor is gone it has no database
	// effects to undo; remove only its unfinished revision before replanning it.
	_, err = m.sqlStatement(database, "DELETE FROM schema_migrations WHERE version=3 AND state <> 'applied'")
	a.check(err, "repair only the accounted unfinished revision")
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, migrations, read())
	record, err := alUnresolvedMigrationRecord(migrations.snapshot(), name, uid, original, originalPlan.Spec.TargetIdentityDigest)
	a.check(err, "retain the exact unresolved record through repair")
	if !record.RecordedAt.Time.Equal(recorded) {
		a.fatalf("unresolved timestamp moved during recovery")
	}
	m.acknowledgeUnresolvedRun(name, original.ID)
	var recovered *ptahv1.PtahMigration
	a.check(harness.Wait(a.ctx, "a fresh approval after accounting for the run", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		check()
		recovered = read()
		return alUnresolvedMigrationReady(recovered, original), "waiting for a new decision on the remaining migration", nil
	}), "restore read-only progress without replay")
	if recovered.Status.ResolvedRun == nil || recovered.Status.ResolvedRun.OperationID != original.ID {
		a.fatalf("recovery lost its acknowledged operation")
	}
	accounted := recovered.Status.ResolvedRun.ResolvedAt.Time
	if versions := drillPlanVersions(m.planOf(recovered.Status.Plan.Name)); versions != "3" {
		a.fatalf("recovery tried to replay already committed migrations")
	}
	resolved, _ := a.waitForDeliveryWithCheck(alMatch{status: "resolved", alertName: alUnresolvedApply, labels: labels}, "the accounted unresolved notification", alDetectionSlack, index+1, check)
	a.check(harness.Wait(a.ctx, "the native resolved scrape history", 2*alScrapeInterval+alScrapeTimeout, time.Second, func(context.Context) (bool, string, error) {
		check()
		h = a.unresolvedHistory(names, leader, "migration", started, "")
		return !h.through.Before(resolved.ReceivedAt), "waiting for native recovery samples", nil
	}), "retain the recovery boundary")
	h = a.unresolvedHistory(names, leader, "migration", started, "resolved")
	if !alUnresolvedCleared(firing, resolved, h, recorded, accounted) || !a.noActiveAlerts(query) {
		a.fatalf("unresolved resolution missed its original persisted bound")
	}
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, migrations, recovered)
	publisher := &batchv1.Job{}
	a.check(m.get("e2e-push-migrations-"+m.engine.name+"-alerts-unresolved", publisher), "read the owned watch sentinel")
	publishers := &corev1.PodList{}
	a.check(m.list(publishers), "read the publisher Pod sentinel")
	owned := ownedPods(publishers.Items, publisher.UID)
	if len(owned) != 1 {
		a.fatalf("no unique publisher sentinel")
	}
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, jobs, publisher)
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, pods, &owned[0])
	closeRunnerWatches(a.t, []recorder{migrations, jobs, pods}, m.scan)
	if !migrationExecutorNoReplay(jobs.snapshot(), pods.snapshot(), name, string(original.JobUID), string(originalPod.UID)) {
		a.fatalf("closed unresolved history contains a replay or replacement executor")
	}
	// A new approval is required for the remaining migration. The original
	// decision and the run acknowledgment cannot authorize this Apply.
	freshJobs := newStoredStateRecorder[*batchv1.Job](a.t, a.ctx, watcher, name+"-recovery-jobs", a.in.TestNamespace, func() client.ObjectList { return &batchv1.JobList{} })
	freshPods := newStoredStateRecorder[*corev1.Pod](a.t, a.ctx, watcher, name+"-recovery-pods", a.in.TestNamespace, func() client.ObjectList { return &corev1.PodList{} })
	freshApproval := approve(recovered, "-recovered")
	converged := m.waitForGenerationInSync(name)
	if converged.UID != uid || converged.Status.UnresolvedRun != nil || converged.Status.LastRun == nil || converged.Status.LastRun.JobUID == original.JobUID || converged.Status.LastRun.Outcome != ptahv1.MigrationRunOutcomeApplied || sql("SELECT count(*) FROM schema_migrations WHERE state='applied'") != "3" || sql("SELECT count(*) FROM e2e_migration_widgets") != "3" {
		a.fatalf("fresh authorization did not converge without replay")
	}
	checkRuntime()
	freshJob := &batchv1.Job{}
	a.check(m.get(converged.Status.LastRun.JobName, freshJob), "read the authorized recovery Job")
	freshList := &corev1.PodList{}
	a.check(m.list(freshList), "read the authorized recovery Pod")
	freshOwned := ownedPods(freshList.Items, freshJob.UID)
	if freshJob.UID != converged.Status.LastRun.JobUID || freshJob.CreationTimestamp.Before(&freshApproval.CreationTimestamp) || len(freshOwned) != 1 {
		a.fatalf("recovery has no unique freshly authorized workload")
	}
	_, err = alLockTransportFinished(&freshOwned[0])
	a.check(err, "require complete successful recovery transport")
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, freshJobs, publisher)
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, freshPods, &owned[0])
	closeRunnerWatches(a.t, []recorder{freshJobs, freshPods}, m.scan)
	if !migrationExecutorNoReplay(freshJobs.snapshot(), freshPods.snapshot(), name, string(freshJob.UID), string(freshOwned[0].UID)) || !a.noActiveAlerts(query) {
		a.fatalf("fresh recovery created another Apply or unresolved incident")
	}
	a.logf("PASS %s unresolved migration: uid=%s operation=%s jobUID=%s podUID=%s recordedAt=%s firing=%s accountedAt=%s resolved=%s; fresh approval applied only version 3", m.engine.name, uid, original.ID, original.JobUID, originalPod.UID, recorded, firing.ReceivedAt, accounted, resolved.ReceivedAt)
}

func (a *alertingRun) unresolvedHistory(pods []string, leader, family string, started time.Time, label string) alUnresolvedHistory {
	at := time.Now().UTC()
	query := func(expression string) []byte {
		body, err := a.prometheus(a.ctx, "/api/v1/query", map[string]string{"query": expression, "time": at.Format(time.RFC3339Nano)})
		a.check(err, "read native unresolved history")
		return body
	}
	gauges := query(fmt.Sprintf(`%s{job=%q,family=%q}[%ds]`, alUnresolvedMetric, alScrapeJob, family, int(alAdmissionHistoryWindow/time.Second)))
	metric := func(name string) []byte {
		return query(fmt.Sprintf(`%s{job=%q}[%ds]`, name, alScrapeJob, int(alAdmissionHistoryWindow/time.Second)))
	}
	up, duration := metric("up"), metric("scrape_duration_seconds")
	h, err := alReadUnresolvedHistory(gauges, up, duration, pods, leader, family, started, at)
	a.check(err, "require a complete native unresolved history")
	if label != "" {
		a.logf("unresolved native history %s/%s: queriedAt=%s gauges=%s up=%s durations=%s", family, label, at.Format(time.RFC3339Nano), strings.TrimSpace(string(gauges)), strings.TrimSpace(string(up)), strings.TrimSpace(string(duration)))
	}
	return h
}
