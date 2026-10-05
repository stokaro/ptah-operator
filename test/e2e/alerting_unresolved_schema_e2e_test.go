//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (a *alertingRun) unresolvedSchemaCase(m *migrationRun) {
	name, database := "e2e-alert-schema-"+m.engine.name, "ptah_alert_schema_"+m.engine.name
	m.isolatedDatabase(database, name+"-db")
	secret := &corev1.Secret{}
	a.check(m.get(name+"-db", secret), "retain the schema target Secret")
	sql := func(statement string) string {
		b, e := m.sqlStatement(database, statement)
		a.check(e, "read the isolated schema database")
		return trimmedSQL(b)
	}
	seed, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", m.engine.name+"-v3.sql"))
	a.check(err, "read the schema baseline")
	sql(string(seed))
	sql("INSERT INTO e2e_widgets (id,name,note) VALUES (701,'unresolved-control','preserve-this-row')")
	reference, publisher, config := a.publishUnresolvedSchema(m, name)
	template := &ptahv1.PtahSchema{}
	a.check(m.get(a.scope.schemaProducer, template), "read the original schema producer")
	object, err := alNegativeFixture(template, name, secret.Name, ptahv1.ApplyPolicyOnApproval)
	a.check(err, "build the independently authorized schema")
	resource := object.(*ptahv1.PtahSchema)
	resource.Spec.Target.URLFrom.Key = "url"
	resource.Spec.Target.Engine = ptahv1.DatabaseEngine(m.engine.kind)
	resource.Spec.Target.CoordinationKey = "e2e/alert-schema/" + m.engine.name
	resource.Spec.Desired.OCIRef = reference
	resource.Spec.Interval.Duration = time.Hour
	resource.Spec.Execution.ActiveDeadlineSeconds = 300
	resource.Spec.Execution.NodeSelector = nil
	resource.Spec.Policy.LockTimeout.Duration = 4 * time.Minute
	watcher, err := client.NewWithWatch(a.cluster.Config, client.Options{Scheme: a.cluster.Scheme})
	a.check(err, "open schema incident histories before creation")
	schemas := newStoredStateRecorder[*ptahv1.PtahSchema](a.t, a.ctx, watcher, name+"-schemas", a.in.TestNamespace, func() client.ObjectList { return &ptahv1.PtahSchemaList{} })
	jobs := newStoredStateRecorder[*batchv1.Job](a.t, a.ctx, watcher, name+"-jobs", a.in.TestNamespace, func() client.ObjectList { return &batchv1.JobList{} })
	pods := newStoredStateRecorder[*corev1.Pod](a.t, a.ctx, watcher, name+"-pods", a.in.TestNamespace, func() client.ObjectList { return &corev1.PodList{} })
	lease, managers := a.managerSnapshot()
	leader := haLeaderPodName(haLeaseHolder(lease))
	monitor := a.negativeMonitorIdentity()
	var names []string
	for _, p := range managers {
		names = append(names, p.Name)
	}
	runtimeCheck := func() {
		l, p := a.managerSnapshot()
		if !alSameManagers(l, p, lease, managers) || !maps.Equal(monitor, a.negativeMonitorIdentity()) {
			a.fatalf("schema incident replaced a manager or monitoring process")
		}
		body, e := a.prometheus(a.ctx, "/api/v1/targets", nil)
		a.check(e, "read schema incident scrape targets")
		if !alTargetsReady(body, int(a.replicas)) {
			a.fatalf("schema incident lost a healthy scrape target")
		}
		body, e = a.prometheus(a.ctx, "/api/v1/rules", nil)
		a.check(e, "read schema incident rules")
		if !alRulesLoaded(body) {
			a.fatalf("schema incident lost an enabled rule")
		}
	}
	check := func() {
		runtimeCheck()
		for _, r := range []recorder{schemas, jobs, pods} {
			a.check(r.alive(), "retain schema incident histories")
		}
	}
	a.check(a.create(resource), "create the schema incident consumer")
	uid := resource.UID
	read := func() *ptahv1.PtahSchema {
		v := &ptahv1.PtahSchema{}
		a.check(m.get(name, v), "read the exact schema incident")
		if v.UID != uid || v.Generation != resource.Generation || !equality.Semantic.DeepEqual(v.Spec, resource.Spec) || v.DeletionTimestamp != nil {
			a.fatalf("schema incident changed its identity or inputs")
		}
		return v
	}
	suspend := func(value bool) {
		v := read()
		before := v.DeepCopy()
		v.Spec.Suspend = value
		a.check(a.cluster.Client.Patch(a.ctx, v, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})), "change only schema suspension")
		resource = v.DeepCopy()
	}
	var approvals []*ptahv1.PtahSchemaApproval
	defer func() {
		if a.t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		for _, v := range approvals {
			a.check(storedStateDeleteExact(ctx, a.cluster, v), "remove the owned schema approval")
		}
		a.check(storedStateDeleteExact(ctx, a.cluster, resource), "finalize the recovered schema")
		for _, v := range []client.Object{publisher, config, secret} {
			a.check(storedStateDeleteExact(ctx, a.cluster, v), "remove the owned schema fixture")
		}
		statement := "DROP DATABASE " + database + " WITH (FORCE)"
		if m.engine.name == "mysql" {
			statement = "DROP DATABASE " + database
		}
		_, e := serverSQL(ctx, a.cluster, a.in.TestNamespace, m.engine, statement)
		a.check(e, "drop the isolated schema database")
	}()
	waitFor := func(description string, bound time.Duration, predicate func(*ptahv1.PtahSchema) bool) *ptahv1.PtahSchema {
		var v *ptahv1.PtahSchema
		a.check(harness.Wait(a.ctx, description, bound, time.Second, func(context.Context) (bool, string, error) {
			check()
			v = read()
			return predicate(v), fmt.Sprintf("%s: phase=%s pendingObservation=%t pendingLockRelease=%t", description, v.Status.Phase, v.Status.PendingObservation != nil, v.Status.PendingLockRelease != nil), nil
		}), "%s", description)
		return v
	}
	wait := func(description string, predicate func(*ptahv1.PtahSchema) bool) *ptahv1.PtahSchema {
		return waitFor(description, alTimeout+time.Minute, predicate)
	}
	ready := wait("the original schema approval gate", func(v *ptahv1.PtahSchema) bool { return alLockApprovalReady(v) })
	approve := func(v *ptahv1.PtahSchema, suffix string) *ptahv1.PtahSchemaApproval {
		p := &ptahv1.PtahSchemaPlan{}
		a.check(m.get(v.Status.Plan.Name, p), "read the exact schema plan")
		if p.UID != v.Status.Plan.UID {
			a.fatalf("the schema plan was replaced")
		}
		m.mustCreate(referenceApprovalDocument(a.in.TestNamespace, name+suffix, name, p))
		approval := &ptahv1.PtahSchemaApproval{}
		a.check(m.get(name+suffix, approval), "retain the schema approval UID")
		approvals = append(approvals, approval)
		return approval
	}
	query := `ALERTS{alertname="PtahOperatorUnresolvedApply",family="schema"}`
	from, started := a.deliveryCount(), time.Now().UTC()
	baseline := a.unresolvedHistory(names, leader, "schema", started, "")
	if baseline.latest != 0 || !baseline.appeared.IsZero() || !a.noActiveAlerts(query) {
		a.fatalf("schema incident has no clean native baseline")
	}
	release := m.runningTableBarrier(database, "e2e_widgets")
	defer release()
	approve(ready, "-original")
	var original *ptahv1.PtahSchema
	var originalJob *batchv1.Job
	var originalPod *corev1.Pod
	var backend string
	var terminatePostgres string
	wait("the original schema DDL behind its table barrier", func(v *ptahv1.PtahSchema) bool {
		op := v.Status.ActiveOperation
		if op == nil || op.Type != ptahv1.OperationApply || op.JobUID == "" {
			return false
		}
		j := &batchv1.Job{}
		a.check(m.get(op.JobName, j), "read the original schema Apply Job")
		list := &corev1.PodList{}
		a.check(m.list(list), "read schema Apply Pods")
		owned := ownedPods(list.Items, j.UID)
		if len(owned) == 0 {
			return false
		}
		if len(owned) != 1 || !alLockWorkload(j, &owned[0], alStalledReading(v)) {
			a.fatalf("schema Apply has no unique original workload")
		}
		if owned[0].Status.Phase != corev1.PodRunning {
			return false
		}
		statement := "SELECT a.pid::text || '/' || host(a.client_addr) || '/' || extract(epoch FROM a.backend_start)::text FROM pg_locks held JOIN pg_stat_activity a ON a.pid=held.pid JOIN pg_locks waiting ON waiting.pid=a.pid WHERE a.datname='" + database + "' AND a.usename='" + migrationDatabaseUser + "' AND held.locktype='advisory' AND held.granted AND held.classid=0 AND held.objid=" + strconv.Itoa(pgApplyLockKey) + " AND held.objsubid=1 AND NOT waiting.granted AND waiting.locktype='relation' AND waiting.relation='e2e_widgets'::regclass AND waiting.mode='AccessExclusiveLock'"
		if m.engine.name == "mysql" {
			statement = "SELECT CONCAT(ID,'/',SUBSTRING_INDEX(HOST,':',1)) FROM information_schema.processlist WHERE ID=IS_USED_LOCK('ptah_schema_apply') AND DB='" + database + "' AND STATE LIKE '%metadata lock%'"
		}
		backend = sql(statement)
		if backend == "" {
			return false
		}
		if m.engine.name == "postgresql" {
			var err error
			terminatePostgres, err = alPostgresBackendTermination(database, backend, &owned[0])
			a.check(err, "bind the PostgreSQL writer's original session")
		} else if !executorBackendMatchesPod(backend, &owned[0], owned[0].UID) {
			a.fatalf("blocked DDL does not belong to the original schema executor")
		}
		original, originalJob, originalPod = v.DeepCopy(), j.DeepCopy(), owned[0].DeepCopy()
		return true
	})
	suspend(true)
	jobUID := originalJob.UID
	a.check(a.cluster.Client.Delete(a.ctx, originalJob, client.Preconditions{UID: &jobUID}, client.PropagationPolicy(metav1.DeletePropagationBackground)), "remove the interrupted schema Job by UID")
	unknown := wait("the persisted unresolved schema Apply", func(v *ptahv1.PtahSchema) bool {
		return v.Status.PendingObservation != nil && v.Status.PendingObservation.Outcome == ptahv1.PendingObservationOutcomeUnknown
	})
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, schemas, unknown)
	trace, err := alReadSchemaUnresolvedTrace(schemas.snapshot(), original, false)
	a.check(err, "bind the original unknown schema and its timestamp")
	labels := map[string]string{"family": "schema", "operator_namespace": a.in.OperatorNamespace, "operator_metrics_service": a.metricsService}
	firing, index := a.waitForDeliveryWithCheck(alMatch{status: "firing", alertName: alUnresolvedApply, labels: labels}, "the exact unresolved schema notification", alDetectionSlack, from, check)
	h := a.unresolvedHistory(names, leader, "schema", started, "firing")
	count, _ := alAlertedCount(firing.Annotations["summary"])
	if !alUnresolvedDelivered(firing, h, trace.recorded) || count != "1" || firing.Labels["severity"] != "critical" || firing.Annotations["runbook_url"] != a.runbookBase+"#unresolved-gauges" || !alRunbookAnchor(a.operationsPage(), "unresolved-gauges") {
		a.fatalf("schema notification missed its original bound, count or runbook")
	}
	for _, k := range []string{"resource", "operation", "job", "pod", "plan", "execution"} {
		if _, ok := firing.Labels[k]; ok {
			a.fatalf("schema notification acquired an unbounded label")
		}
	}
	a.check(harness.Wait(a.ctx, "all original schema Pods to stop", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		check()
		list := &corev1.PodList{}
		a.check(m.list(list), "inspect original schema executors")
		for _, p := range ownedPods(list.Items, jobUID) {
			if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
				return false, "waiting for the old executor to stop", nil
			}
		}
		return true, "", nil
	}), "stop the old schema workload before database recovery")
	// A database can retain queued DDL after its client is gone. Kill only the
	// server session bound above to the original Pod before releasing its lock.
	pid := strings.Split(backend, "/")[0]
	if m.engine.name == "mysql" {
		current := sql("SELECT CONCAT(ID,'/',SUBSTRING_INDEX(HOST,':',1)) FROM information_schema.processlist WHERE ID=" + pid + " AND DB='" + database + "' AND USER='" + migrationDatabaseUser + "'")
		if current != "" {
			if current != backend {
				a.fatalf("the original schema backend identity changed")
			}
			sql("KILL " + pid)
		}
	} else {
		result := sql(terminatePostgres)
		// The session can finish between selection and signaling. A false
		// result is safe only if the disappearance check below confirms it.
		if result != "" && result != "t" && result != "f" {
			a.fatalf("the original PostgreSQL backend could not be terminated")
		}
	}
	a.check(harness.Wait(a.ctx, "the old schema database session to disappear", time.Minute, time.Second, func(context.Context) (bool, string, error) {
		check()
		statement := "SELECT count(*) FROM pg_stat_activity WHERE datname='" + database + "' AND pid=" + pid
		if m.engine.name == "mysql" {
			statement = "SELECT count(*) FROM information_schema.processlist WHERE DB='" + database + "' AND ID=" + pid
		}
		return sql(statement) == "0", "waiting for the exact old session", nil
	}), "remove every old schema writer before unlocking")
	release()
	columnQuery := "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='e2e_widgets' AND column_name='fault_token'"
	if m.engine.name == "mysql" {
		columnQuery = "SELECT count(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='e2e_widgets' AND column_name='fault_token'"
	}
	checkRows := func() {
		if sql("SELECT count(*) FROM e2e_widgets") != "1" || sql("SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='unresolved-control' AND note='preserve-this-row'") != "1" {
			a.fatalf("schema recovery changed its retained data")
		}
	}
	checkRows()
	if sql(columnQuery) != "0" {
		a.fatalf("the interrupted DDL committed before recovery")
	}
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, schemas, read())
	_, err = alReadSchemaUnresolvedTrace(schemas.snapshot(), original, false)
	a.check(err, "retain Unknown until read-only recovery is enabled")
	suspend(false)
	recoveryWait, err := alSchemaRecoveryWait(unknown.Status.PendingObservation.ObserveAfter, time.Now())
	a.check(err, "retain the original schema recovery deadline")
	recovered := waitFor("fresh schema approval after observation and planning", recoveryWait, func(v *ptahv1.PtahSchema) bool {
		return alRecoveredSchemaApprovalReady(v, original.UID, original.Status.ActiveOperation.StartedAt.Time)
	})
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, schemas, recovered)
	trace, err = alReadSchemaUnresolvedTrace(schemas.snapshot(), original, true)
	a.check(err, "retain original-target Observe and Plan before resolution")
	resolved, _ := a.waitForDeliveryWithCheck(alMatch{status: "resolved", alertName: alUnresolvedApply, labels: labels}, "the unresolved schema recovery notification", alDetectionSlack, index+1, check)
	a.check(harness.Wait(a.ctx, "native schema recovery samples", 2*alScrapeInterval+alScrapeTimeout, time.Second, func(context.Context) (bool, string, error) {
		check()
		h = a.unresolvedHistory(names, leader, "schema", started, "")
		return !h.through.Before(resolved.ReceivedAt), "waiting for recovery scrape coverage", nil
	}), "retain native schema resolution")
	h = a.unresolvedHistory(names, leader, "schema", started, "resolved")
	if !alUnresolvedCleared(firing, resolved, h, trace.recorded, trace.accounted) || !a.noActiveAlerts(query) {
		a.fatalf("schema resolution missed its persisted proof deadline")
	}
	checkRows()
	if sql(columnQuery) != "0" {
		a.fatalf("schema proof executed DDL without a new approval")
	}
	publisherPods := &corev1.PodList{}
	a.check(m.list(publisherPods), "read schema publisher sentinel")
	owned := ownedPods(publisherPods.Items, publisher.UID)
	if len(owned) != 1 {
		a.fatalf("schema publisher has no unique sentinel")
	}
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, schemas, read())
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, jobs, publisher)
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, pods, &owned[0])
	closeRunnerWatches(a.t, []recorder{schemas, jobs, pods}, m.scan)
	_, err = alReadSchemaUnresolvedTrace(schemas.snapshot(), original, true)
	a.check(err, "validate the closed schema recovery history")
	if !schemaExecutorNoReplay(jobs.snapshot(), pods.snapshot(), name, string(jobUID), string(originalPod.UID)) {
		a.fatalf("schema recovery replayed its original Apply")
	}
	freshJobs := newStoredStateRecorder[*batchv1.Job](a.t, a.ctx, watcher, name+"-fresh-jobs", a.in.TestNamespace, func() client.ObjectList { return &batchv1.JobList{} })
	freshPods := newStoredStateRecorder[*corev1.Pod](a.t, a.ctx, watcher, name+"-fresh-pods", a.in.TestNamespace, func() client.ObjectList { return &corev1.PodList{} })
	approval := approve(recovered, "-recovered")
	var converged *ptahv1.PtahSchema
	a.check(harness.Wait(a.ctx, "the freshly approved schema to converge", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		runtimeCheck()
		a.check(freshJobs.alive(), "retain fresh schema Jobs")
		a.check(freshPods.alive(), "retain fresh schema Pods")
		converged = read()
		return inSyncFor(converged, "ScopedConverged") && converged.Status.PendingObservation == nil && converged.Status.ActiveOperation == nil, "waiting for freshly authorized DDL", nil
	}), "finish schema runbook recovery")
	checkRows()
	if sql(columnQuery) != "1" || converged.Status.Applied == nil || converged.Status.Applied.PlanRef.UID != recovered.Status.Plan.UID {
		a.fatalf("fresh schema approval did not apply its exact plan")
	}
	var freshJob *batchv1.Job
	var freshPod *corev1.Pod
	list := &batchv1.JobList{}
	a.check(m.list(list), "read the fresh schema Apply")
	for i := range list.Items {
		j := &list.Items[i]
		if j.Labels[labelSchema] == name && j.Labels[labelOperation] == "apply" {
			if freshJob != nil {
				a.fatalf("fresh schema approval created multiple Apply Jobs")
			}
			freshJob = j.DeepCopy()
		}
	}
	if freshJob == nil || freshJob.UID == jobUID || freshJob.CreationTimestamp.Before(&approval.CreationTimestamp) {
		a.fatalf("schema has no newly approved Apply")
	}
	listPods := &corev1.PodList{}
	a.check(m.list(listPods), "read the fresh schema executor")
	freshOwned := ownedPods(listPods.Items, freshJob.UID)
	if len(freshOwned) != 1 {
		a.fatalf("fresh schema Apply has no unique Pod")
	}
	freshPod = &freshOwned[0]
	_, err = alLockTransportFinished(freshPod)
	a.check(err, "require successful fresh schema transport")
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, freshJobs, publisher)
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, freshPods, &owned[0])
	closeRunnerWatches(a.t, []recorder{freshJobs, freshPods}, m.scan)
	if !schemaExecutorNoReplay(freshJobs.snapshot(), freshPods.snapshot(), name, string(freshJob.UID), string(freshPod.UID)) || !a.noActiveAlerts(query) {
		a.fatalf("fresh schema recovery replayed work or remained unresolved")
	}
	a.logf("PASS %s unresolved schema: uid=%s originalJob=%s originalPod=%s recorded=%s firing=%s observed=%s accounted=%s resolved=%s; only fresh approval added fault_token", m.engine.name, uid, jobUID, originalPod.UID, trace.recorded, firing.ReceivedAt, trace.observed, trace.accounted, resolved.ReceivedAt)
}

func (a *alertingRun) publishUnresolvedSchema(m *migrationRun, name string) (string, *batchv1.Job, *corev1.ConfigMap) {
	body, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", m.engine.name+"-fault-v1.sql"))
	a.check(err, "read the native schema fault fixture")
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-source", Namespace: a.in.TestNamespace}, Data: map[string]string{"schema.sql": string(body)}}
	a.check(a.create(cm), "create the schema publication source")
	reference := "oci://" + m.registryHost + "/e2e-alert-schema:" + m.engine.name
	dialect := "postgres"
	if m.engine.name == "mysql" {
		dialect = "mysql"
	}
	// Each scenario and engine needs its own write-once version in this repository.
	document := publisherJob(a.in.TestNamespace, name+"-publisher", map[string]any{"app.kubernetes.io/component": "e2e-schema-publisher"}, map[string]any{
		"name": "publisher", "image": m.in.ExecutorImage, "imagePullPolicy": "IfNotPresent", "command": []any{"/usr/local/bin/ptah"}, "args": []any{"schema", "push", reference, "--schema-file", "/schema/schema.sql", "--dialect", dialect, "--version", name, "--plain-http"},
		"env": []any{map[string]any{"name": "HOME", "value": "/work"}, map[string]any{"name": "TMPDIR", "value": "/work"}, secretEnv("PTAH_OCI_USERNAME", registryAuthSecret, "username"), secretEnv("PTAH_OCI_PASSWORD", registryAuthSecret, "password"), secretEnv("PTAH_OCI_REGISTRY", registryAuthSecret, "registry")}, "securityContext": restrictedContainer(), "volumeMounts": []any{map[string]any{"name": "schema", "mountPath": "/schema", "readOnly": true}, map[string]any{"name": "work", "mountPath": "/work"}},
	}, []any{map[string]any{"name": "schema", "configMap": map[string]any{"name": cm.Name}}, map[string]any{"name": "work", "emptyDir": map[string]any{"sizeLimit": "64Mi"}}})
	m.mustCreate(document)
	var j *batchv1.Job
	a.check(harness.Wait(a.ctx, "the unresolved schema publisher", 5*time.Minute, time.Second, func(context.Context) (bool, string, error) {
		j = &batchv1.Job{}
		if e := m.get(name+"-publisher", j); e != nil {
			return false, "", e
		}
		if conditionTrue(j.Status.Conditions, batchv1.JobFailed) {
			return false, "", fmt.Errorf("schema publisher failed")
		}
		return jobComplete(j), "waiting for native publication", nil
	}), "publish the schema fault")
	pods := &corev1.PodList{}
	a.check(m.list(pods), "read schema publication transport")
	owned := ownedPods(pods.Items, j.UID)
	if len(owned) != 1 || !publisherJobIsolation(j, m.in.ExecutorImage, registryAuthSecret) {
		a.fatalf("schema publication lost its unique isolated transport")
	}
	logs, err := a.cluster.ContainerLog(a.ctx, a.in.TestNamespace, owned[0].Name, "publisher")
	a.check(err, "read schema publication digest")
	m.scan(logs, "unresolved schema publisher")
	digests := publishedDigests(logs)
	if len(digests) != 1 || !sha256Pattern.MatchString(digests[0]) {
		a.fatalf("schema publication has no exact digest")
	}
	return reference + "@" + digests[0], j, cm
}
