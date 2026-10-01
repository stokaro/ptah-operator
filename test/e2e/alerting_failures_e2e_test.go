//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (a *alertingRun) operationsFailing() {
	for _, family := range []string{"schema", "migration"} {
		a.operationsFailingFamily(family)
	}
}

func (a *alertingRun) operationsFailingFamily(family string) {
	name, database := "e2e-alert-failures-"+family, "ptah_alert_failures_"+family
	var template client.Object = &ptahv1.PtahSchema{}
	producer := "e2e-reference-postgresql"
	if family == "migration" {
		template = &ptahv1.PtahMigration{}
		producer = "e2e-migrations-postgresql"
	}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.TestNamespace, Name: producer}, template), "read the real operation-failure producer")
	var source, executor string
	switch v := template.(type) {
	case *ptahv1.PtahSchema:
		source = v.Spec.Desired.OCIRef
		if v.Status.ExecutionBinding != nil {
			executor = v.Status.ExecutionBinding.ExecutorImage
		}
	case *ptahv1.PtahMigration:
		source = v.Spec.Artifact.OCIRef
		if v.Status.ExecutionBinding != nil {
			executor = v.Status.ExecutionBinding.ExecutorImage
		}
	}
	registry, err := url.Parse(source)
	a.check(err, "read the fixture registry")
	if registry.Scheme != "oci" || registry.Host == "" || registry.User != nil || !alPinnedImage.MatchString(executor) {
		a.fatalf("operation-failure producer has no exact executor or registry")
	}
	engine, err := migrationEngineFor("postgresql")
	a.check(err, "select the prepared database")
	m := &migrationRun{t: a.t, parent: a.t, ctx: a.ctx, cluster: a.cluster, engine: engine, workDir: a.workDir, registryHost: registry.Host, repository: "e2e-alert-failures", in: phases.MigrationsInputs{TestNamespace: a.in.TestNamespace, ExecutorImage: executor}}
	password := &corev1.Secret{}
	a.check(m.get(engine.sourceSecret, password), "read the application credential")
	m.password = string(password.Data["password"])
	if m.password == "" {
		a.fatalf("the failure-recovery database has no credential")
	}
	m.protect(m.password, a.credentials.Password)
	m.isolatedDatabase(database, name+"-db")
	secret := &corev1.Secret{}
	a.check(m.get(name+"-db", secret), "retain the isolated Secret identity")
	r := &referenceRun{t: a.t, parent: a.t, ctx: a.ctx, cluster: a.cluster, engine: engine, workDir: a.workDir, scanner: m.scanner, in: phases.ReferenceDataInputs{TestNamespace: a.in.TestNamespace, ExecutorImage: executor}}
	r.names = referenceNamesFor("postgresql", registry.Host, "e2e-alert-failures")
	r.names.configMapPrefix, r.names.jobPrefix = "e2e-alert-failures-schema-", "e2e-push-alert-failures-schema-"
	reference := r.names.artifact
	if family == "migration" {
		reference = m.reference("")
	}
	object, err := alNegativeFixture(template, name, secret.Name, ptahv1.ApplyPolicyNever)
	a.check(err, "create the immutable fault inputs")
	switch v := object.(type) {
	case *ptahv1.PtahSchema:
		v.Spec.Target.URLFrom.Key = "url"
		v.Spec.Desired.OCIRef = reference
		v.Spec.Execution.NodeSelector = nil
		v.Spec.Execution.FailureRetryInterval.Duration = 10 * time.Second
	case *ptahv1.PtahMigration:
		v.Spec.Target.URLFrom.Key = "url"
		v.Spec.Artifact.OCIRef = reference
		v.Spec.Execution.NodeSelector = nil
		v.Spec.Execution.FailureRetryInterval.Duration = 10 * time.Second
	}
	watcher, err := client.NewWithWatch(a.cluster.Config, client.Options{Scheme: a.cluster.Scheme})
	a.check(err, "open operation-failure histories")
	schemas := newStoredStateRecorder[*ptahv1.PtahSchema](a.t, a.ctx, watcher, "alert-failures-schemas", a.in.TestNamespace, func() client.ObjectList { return &ptahv1.PtahSchemaList{} })
	migrations := newStoredStateRecorder[*ptahv1.PtahMigration](a.t, a.ctx, watcher, "alert-failures-migrations", a.in.TestNamespace, func() client.ObjectList { return &ptahv1.PtahMigrationList{} })
	jobs := newStoredStateRecorder[*batchv1.Job](a.t, a.ctx, watcher, "alert-failures-jobs", a.in.TestNamespace, func() client.ObjectList { return &batchv1.JobList{} })
	pods := newStoredStateRecorder[*corev1.Pod](a.t, a.ctx, watcher, "alert-failures-pods", a.in.TestNamespace, func() client.ObjectList { return &corev1.PodList{} })
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
			a.fatalf("a manager or monitoring process changed during repeated failures")
		}
		body, e := a.prometheus(a.ctx, "/api/v1/targets", nil)
		a.check(e, "read failure-proof scrapes")
		if !alTargetsReady(body, int(a.replicas)) {
			a.fatalf("a manager scrape failed during repeated operations")
		}
		body, e = a.prometheus(a.ctx, "/api/v1/rules", nil)
		a.check(e, "read failure-proof rules")
		if !alRulesLoaded(body) {
			a.fatalf("a required rule stopped evaluating during repeated operations")
		}
		for _, w := range []recorder{schemas, migrations, jobs, pods} {
			a.check(w.alive(), "retain complete repeated-operation histories")
		}
	}
	query := fmt.Sprintf(`ALERTS{alertname=%q,family=%q,stage="resolve"}`, alFailuresAlert, family)
	if !a.noActiveAlerts(query) {
		a.fatalf("the operation-failure incident already exists")
	}
	// The preceding stalled-operation row intentionally failed one Resolve.
	// Wait for a complete quiet rolling window before creating this fault;
	// failed scrapes, missing managers and counter resets are not retryable.
	a.check(harness.Wait(a.ctx, "a complete quiet pre-fault counter window", alFailuresWindow+alDetectionSlack, time.Second, func(context.Context) (bool, string, error) {
		checkRuntime()
		history, err := a.queryFailureHistory(names, leader, family, time.Now().UTC(), "")
		if errors.Is(err, errAlFailuresBaseline) {
			return false, "waiting for earlier deliberate failures to leave the baseline", nil
		}
		return err == nil && history.increments == 0, "waiting for a quiet baseline", err
	}), "establish an uncontaminated repeated-failure baseline")
	if !a.noActiveAlerts(query) {
		a.fatalf("a previous repeated-failure incident survived the quiet baseline")
	}
	from := a.deliveryCount()
	started := time.Now().UTC()
	a.failureHistory(names, leader, family, started, "")
	a.check(a.create(object), "create the resource referencing an unpublished artifact")
	initial := object.DeepCopyObject().(client.Object)
	defer func() {
		if a.t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		uid := initial.GetUID()
		if e := a.cluster.Client.Delete(ctx, initial, client.Preconditions{UID: &uid}); e != nil {
			a.t.Errorf("remove operation-failure fixture: %v", e)
			return
		}
		if e := harness.Wait(ctx, "operation-failure finalization", time.Minute, time.Second, func(ctx context.Context) (bool, string, error) {
			v := initial.DeepCopyObject().(client.Object)
			e := a.cluster.Client.Get(ctx, client.ObjectKeyFromObject(initial), v)
			return apierrors.IsNotFound(e), "waiting for fixture deletion", client.IgnoreNotFound(e)
		}); e != nil {
			a.t.Errorf("finish operation-failure cleanup: %v", e)
			return
		}
		if _, e := serverSQL(ctx, a.cluster, a.in.TestNamespace, engine, "DROP DATABASE "+database+" WITH (FORCE)"); e != nil {
			a.t.Errorf("drop operation-failure database: %v", e)
		}
		uid = secret.UID
		if e := a.cluster.Client.Delete(ctx, secret, client.Preconditions{UID: &uid}); e != nil {
			a.t.Errorf("remove operation-failure Secret: %v", e)
		}
	}()
	read := func() {
		a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(initial), object), "read the unchanged operation-failure fixture")
		a.check(alFailuresUnchanged(initial, object), "retain the unchanged failure consumer")
	}
	results := map[types.UID]alFailureResult{}
	collect := func() {
		checkRuntime()
		read()
		claims := map[types.UID]alStalledClaim{}
		claim := func(v client.Object) {
			c := alStalledReading(v)
			if c.uid != initial.GetUID() {
				return
			}
			if c.operation == "Apply" {
				a.fatalf("the Never fixture claimed an Apply")
			}
			if c.jobUID != "" {
				claims[c.jobUID] = c
			}
		}
		for _, e := range schemas.snapshot() {
			claim(e.Object)
		}
		for _, e := range migrations.snapshot() {
			claim(e.Object)
		}
		list := &batchv1.JobList{}
		a.check(a.cluster.Client.List(a.ctx, list, client.InNamespace(a.in.TestNamespace)), "read the real failed Jobs")
		for _, job := range list.Items {
			owner := metav1.GetControllerOf(&job)
			if owner == nil || owner.UID != initial.GetUID() {
				continue
			}
			if job.Labels[labelOperation] == "apply" {
				a.fatalf("the Never fixture dispatched Apply")
			}
			if job.Labels[labelOperation] != "resolve" || !jobComplete(&job) {
				continue
			}
			if _, done := results[job.UID]; done {
				continue
			}
			c, ok := claims[job.UID]
			if !ok {
				continue
			} // The independent resource watch may not have caught up yet.
			list := &corev1.PodList{}
			a.check(a.cluster.Client.List(a.ctx, list, client.InNamespace(job.Namespace)), "read Resolve transports")
			owned := ownedPods(list.Items, job.UID)
			if len(owned) != 1 || !alLockWorkload(&job, &owned[0], c) {
				a.fatalf("a Resolve has no unique original transport")
			}
			pod := &owned[0]
			finished, e := alLockTransportFinished(pod)
			a.check(e, "require complete successful result transport")
			logs, e := a.cluster.ContainerLog(a.ctx, pod.Namespace, pod.Name, "ptah")
			a.check(e, "retain Resolve before TTL collection")
			m.scan(logs, "operation-failure Resolve")
			result, e := runner.ParseResultFor(logs, runner.OperationResolve, c.id)
			a.check(e, "bind the exact Resolve result")
			if result.Uncertain || result.Truncation != nil {
				a.fatalf("Resolve returned incomplete evidence")
			}
			failed := result.Error != nil
			if failed && (result.Error.Code != "child_exit" || result.ChildExitCode <= 0) || !failed && (result.ChildExitCode != 0 || result.ResolvedDigest == "") {
				a.fatalf("Resolve did not report the expected missing artifact or a successful digest")
			}
			current := &corev1.Pod{}
			a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(pod), current), "recheck retained Resolve identity")
			if current.UID != pod.UID {
				a.fatalf("Resolve Pod was replaced while reading its result")
			}
			results[job.UID] = alFailureResult{pod.UID, finished, failed, result.ResolvedDigest}
			a.logf("operation-failure Resolve: family=%s resourceUID=%s operation=%s jobUID=%s podUID=%s finished=%s failed=%t digest=%s logSHA256=%x",
				family, initial.GetUID(), c.id, job.UID, pod.UID, finished.Format(time.RFC3339Nano), failed, result.ResolvedDigest, sha256.Sum256(logs))
		}
	}
	failures := func() int {
		count := 0
		for _, r := range results {
			if r.failed {
				count++
			}
		}
		return count
	}
	labels := map[string]string{"family": family, "stage": "resolve", "operator_namespace": a.in.OperatorNamespace, "operator_metrics_service": a.metricsService}
	firing, index := a.waitForDeliveryWithCheck(alMatch{status: "firing", alertName: alFailuresAlert, labels: labels}, "the repeated Resolve failure notification", 4*time.Minute, from, collect)
	// The fourth real failure is required even when PromQL extrapolation made
	// the alert fire at the third. Retaining it cannot extend delivery's bound.
	a.check(harness.Wait(a.ctx, "four distinct failed Resolve results", time.Minute, time.Second, func(context.Context) (bool, string, error) {
		collect()
		var finished []time.Time
		for _, r := range results {
			if r.failed {
				finished = append(finished, r.finished)
			}
		}
		return alFailuresInWindow(finished), "waiting for four original failures within the rolling window", nil
	}), "retain actual repeated failures")
	// Result completion can precede the next metrics scrape. Wait for its
	// counter sample without changing the receiver's original timestamp.
	var history alFailuresHistory
	a.check(harness.Wait(a.ctx, "the native counter's fourth failure", 2*alScrapeInterval+alScrapeTimeout, time.Second, func(context.Context) (bool, string, error) {
		collect()
		history = a.failureHistory(names, leader, family, started, "")
		return history.increments > alFailuresCount, "waiting for a native counter increment", nil
	}), "retain the counter behind the firing incident")
	history = a.failureHistory(names, leader, family, started, family+"-firing")
	if !alFailuresDelivered(firing, history, started) || firing.Labels["severity"] != "warning" || firing.Annotations["runbook_url"] != a.runbookBase+"#resource-state" || !alRunbookAnchor(a.operationsPage(), "resource-state") {
		a.fatalf("repeated failures missed their native delivery bound or runbook")
	}
	for _, label := range []string{"operation", "resource", "pod", "job", "plan", "execution", "category"} {
		if _, ok := firing.Labels[label]; ok {
			a.fatalf("failure alert acquired unbounded label %s", label)
		}
	}
	// Publishing the missing dependency leaves the resource's UID, generation,
	// source reference, policy, cadence and retry interval unchanged.
	var digest string
	if family == "schema" {
		digest = r.publishWithCheck("v1", collect)
	} else {
		digest = m.publishWithCheck("alerts-failures", m.fixtureDir(""), reference, collect)
	}
	a.check(harness.Wait(a.ctx, "native Resolve recovery and a fresh policy gate", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		collect()
		success := false
		for _, r := range results {
			if !r.failed && r.digest == digest {
				success = true
			}
		}
		return success && alNegativeReading(object).gated(), "waiting for the published digest and successful read-only progress", nil
	}), "recover the failed dependency without changing its consumer")
	resolved, _ := a.waitForDeliveryWithCheck(alMatch{status: "resolved", alertName: alFailuresAlert, labels: labels}, "the repeated-failure resolution", alFailuresWindow+alDetectionSlack, index+1, collect)
	// Keep observing the complete quiet window even if >3 became false when
	// earlier failures aged out. A later failure or counter reset cannot hide.
	history = a.failureHistory(names, leader, family, started, "")
	lastLower, lastUpper, count := history.lastLower, history.lastUpper, history.increments
	a.check(harness.Wait(a.ctx, "the complete post-failure quiet window", alFailuresWindow+alDetectionSlack, time.Second, func(context.Context) (bool, string, error) {
		collect()
		history = a.failureHistory(names, leader, family, started, "")
		if history.increments != count || !history.lastLower.Equal(lastLower) || !history.lastUpper.Equal(lastUpper) {
			return false, "", fmt.Errorf("a failure recurred after dependency recovery")
		}
		return !history.scrapedThrough.Before(lastUpper.Add(alFailuresWindow)) && !history.scrapedThrough.Before(resolved.ReceivedAt) && alNegativeReading(object).gated(), "waiting for native scrapes beyond the full failure window and a settled policy gate", nil
	}), "retain the complete recovery history")
	history = a.failureHistory(names, leader, family, started, family+"-resolved")
	if !alFailuresCleared(firing, resolved, history) || !a.noActiveAlerts(query) || history.increments != failures() {
		a.fatalf("failure resolution has a late incident, unexplained counter or unretained failed result")
	}
	read()
	switch v := object.(type) {
	case *ptahv1.PtahSchema:
		storedStateWatchBarrier(a.t, a.ctx, a.cluster, schemas, v)
	case *ptahv1.PtahMigration:
		storedStateWatchBarrier(a.t, a.ctx, a.cluster, migrations, v)
	}
	// Unmanaged publisher objects are safe watch sentinels. Runner Pod
	// metadata is protected by admission and must not be edited for a barrier.
	jobName, configName := r.names.jobPrefix+"v1", r.names.configMapPrefix+"v1"
	if family == "migration" {
		jobName = "e2e-push-migrations-postgresql-alerts-failures"
		configName = "e2e-migrations-postgresql-alerts-failures"
	}
	publisher := &batchv1.Job{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.TestNamespace, Name: jobName}, publisher), "read the recovery publisher as the Job barrier")
	publisherPods := &corev1.PodList{}
	a.check(a.cluster.Client.List(a.ctx, publisherPods, client.InNamespace(a.in.TestNamespace)), "read the recovery publisher's Pod barrier")
	owned := ownedPods(publisherPods.Items, publisher.UID)
	if len(owned) != 1 {
		a.fatalf("the recovery publisher has no unique Pod sentinel")
	}
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, jobs, publisher)
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, pods, &owned[0])
	closeRunnerWatches(a.t, []recorder{schemas, migrations, jobs, pods}, m.scan)
	// Validate changes received between direct polls and while closing watches.
	seen := 0
	validate := func(v client.Object) {
		if client.ObjectKeyFromObject(v) != client.ObjectKeyFromObject(initial) {
			return
		}
		seen++
		a.check(alFailuresUnchanged(initial, v), "retain the closed failure-consumer history")
	}
	if family == "schema" {
		for _, event := range schemas.snapshot() {
			validate(event.Object)
		}
	} else {
		for _, event := range migrations.snapshot() {
			validate(event.Object)
		}
	}
	if seen == 0 {
		a.fatalf("the closed history contains no original failure consumer")
	}
	a.check(alFailuresWorkloads(initial.GetUID(), results, jobs.snapshot(), pods.snapshot()), "account for the closed Resolve workload histories")
	// Publisher objects belong only to this case; remove their exact identities.
	for _, v := range []client.Object{&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: a.in.TestNamespace, Name: jobName}}, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: a.in.TestNamespace, Name: configName}}} {
		a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(v), v), "read the owned recovery publisher")
		uid := v.GetUID()
		a.check(a.cluster.Client.Delete(a.ctx, v, client.Preconditions{UID: &uid}), "remove the owned recovery publisher")
	}
	a.logf("PASS %s repeated Resolve failures: resourceUID=%s generation=%d failedJobs=%d digest=%s thresholdLower=%s firingReceived=%s lastFailure=(%s,%s] resolvedReceived=%s", family, initial.GetUID(), initial.GetGeneration(), failures(), digest, history.thresholdLower, firing.ReceivedAt, history.lastLower, history.lastUpper, resolved.ReceivedAt)
}

func (a *alertingRun) failureHistory(pods []string, leader, family string, started time.Time, label string) alFailuresHistory {
	history, err := a.queryFailureHistory(pods, leader, family, started, label)
	a.check(err, "validate native operation-failure history")
	return history
}

func (a *alertingRun) queryFailureHistory(pods []string, leader, family string, started time.Time, label string) (alFailuresHistory, error) {
	queriedAt := time.Now().UTC()
	query := func(expression string) []byte {
		body, err := a.prometheus(a.ctx, "/api/v1/query", map[string]string{"query": expression, "time": queriedAt.Format(time.RFC3339Nano)})
		a.check(err, "read native operation-failure history")
		return body
	}
	scrapeHistory := func(metric string) string {
		return fmt.Sprintf(`%s{job=%q}[%ds]`, metric, alScrapeJob, int(alFailuresHistoryWindow/time.Second))
	}
	counters, up, durations := query(alFailuresHistoryQuery(family)), query(scrapeHistory("up")), query(scrapeHistory("scrape_duration_seconds"))
	history, err := alReadFailuresHistory(counters, up, durations, pods, leader, family, started, queriedAt)
	if label != "" {
		a.logf("operation-failure native history %s: queriedAt=%s counters=%s up=%s durations=%s", label, queriedAt.Format(time.RFC3339Nano), counters, up, durations)
	}
	return history, err
}
