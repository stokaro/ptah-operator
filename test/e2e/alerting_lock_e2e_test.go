//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (a *alertingRun) lockReleaseOwed() {
	schema := &ptahv1.PtahSchema{}
	migration := &ptahv1.PtahMigration{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.TestNamespace, Name: "e2e-reference-postgresql"}, schema), "read the real schema source for lock alerts")
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.TestNamespace, Name: "e2e-migrations-postgresql"}, migration), "read the real migration source for lock alerts")
	if schema.Status.ExecutionBinding == nil || migration.Status.ExecutionBinding == nil || schema.Status.ExecutionBinding.ExecutorImage != migration.Status.ExecutionBinding.ExecutorImage || !alPinnedImage.MatchString(schema.Status.ExecutionBinding.ExecutorImage) {
		a.fatalf("lock alert producers do not share an exact executor")
	}
	registry, err := url.Parse(migration.Spec.Artifact.OCIRef)
	a.check(err, "read the prepared artifact registry")
	if registry.Scheme != "oci" || registry.Host == "" || registry.User != nil {
		a.fatalf("the migration producer has no usable credential-free registry")
	}
	for _, engineName := range []string{"postgresql", "mysql"} {
		engine, err := migrationEngineFor(engineName)
		a.check(err, "select lock alert engine")
		m := &migrationRun{t: a.t, parent: a.t, ctx: a.ctx, cluster: a.cluster, engine: engine, workDir: a.workDir, registryHost: registry.Host, repository: "e2e-alert-lock", in: phases.MigrationsInputs{TestNamespace: a.in.TestNamespace, ExecutorImage: schema.Status.ExecutionBinding.ExecutorImage}}
		secret := &corev1.Secret{}
		a.check(m.get(engine.sourceSecret, secret), "read the prepared engine credential")
		m.password = string(secret.Data["password"])
		if m.password == "" {
			a.fatalf("the lock alert engine has no application credential")
		}
		m.protect(m.password, a.credentials.Password)
		// Publish each engine's own real migration directory with the installed
		// executor. The PostgreSQL suite need not have run the MySQL phase.
		m.publish("alerts-lock", m.fixtureDir(""), m.reference(""))
		for _, family := range []string{"schema", "migration"} {
			a.lockReleaseCase(m, schema, migration, family)
		}
		// These two publisher objects were created by this invocation. Registry
		// blobs stay in the task registry until the driver removes that registry.
		for _, object := range []client.Object{&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "e2e-push-migrations-" + engineName + "-alerts-lock", Namespace: a.in.TestNamespace}}, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "e2e-migrations-" + engineName + "-alerts-lock", Namespace: a.in.TestNamespace}}} {
			a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(object), object), "read the owned lock publisher identity")
			uid := object.GetUID()
			a.check(a.cluster.Client.Delete(a.ctx, object, client.Preconditions{UID: &uid}), "remove the owned lock publisher")
		}
	}
}

func (a *alertingRun) lockReleaseCase(m *migrationRun, schemaTemplate *ptahv1.PtahSchema, migrationTemplate *ptahv1.PtahMigration, family string) {
	name := "e2e-alert-lock-" + m.engine.name + "-" + family
	database := "ptah_alert_lock_" + m.engine.name + "_" + family
	secretName := name + "-db"
	m.isolatedDatabase(database, secretName)
	secret := &corev1.Secret{}
	a.check(m.get(secretName, secret), "retain the lock alert Secret identity")
	var template client.Object = schemaTemplate
	if family == "migration" {
		template = migrationTemplate
	}
	object, err := alNegativeFixture(template, name, secretName, ptahv1.ApplyPolicyOnApproval)
	a.check(err, "build the lock owner")
	coordination := "e2e/alert-lock/" + m.engine.name + "/" + family
	switch v := object.(type) {
	case *ptahv1.PtahSchema:
		v.Spec.Target.URLFrom.Key = "url"
		v.Spec.Target.Engine = ptahv1.DatabaseEngine(m.engine.kind)
		v.Spec.Target.CoordinationKey = coordination
		v.Spec.Target.SharedRealm = true
		v.Spec.Interval.Duration = time.Hour
		v.Spec.Execution.NodeSelector = map[string]string{alGateLabel: "open"}
		v.Spec.Execution.ActiveDeadlineSeconds = 300
	case *ptahv1.PtahMigration:
		v.Spec.Target.URLFrom.Key = "url"
		v.Spec.Target.Engine = ptahv1.DatabaseEngine(m.engine.kind)
		v.Spec.Target.CoordinationKey = coordination
		v.Spec.Target.SharedRealm = true
		v.Spec.Interval.Duration = time.Hour
		v.Spec.Artifact.OCIRef = m.reference("")
		v.Spec.Execution.NodeSelector = map[string]string{alGateLabel: "open"}
		v.Spec.Execution.ActiveDeadlineSeconds = 300
	}
	siblingObject, err := alNegativeFixture(schemaTemplate, name+"-sibling", secretName, ptahv1.ApplyPolicyNever)
	a.check(err, "build the authorized sibling")
	sibling := siblingObject.(*ptahv1.PtahSchema)
	sibling.Spec.Target.URLFrom.Key = "url"
	sibling.Spec.Target.Engine = ptahv1.DatabaseEngine(m.engine.kind)
	sibling.Spec.Target.CoordinationKey = coordination
	sibling.Spec.Target.SharedRealm = true
	sibling.Spec.Interval.Duration = time.Hour
	sibling.Spec.Execution.NodeSelector = nil
	watcher, err := client.NewWithWatch(a.cluster.Config, client.Options{Scheme: a.cluster.Scheme})
	a.check(err, "open lock alert watches")
	schemas := newStoredStateRecorder[*ptahv1.PtahSchema](a.t, a.ctx, watcher, "alert-lock-schemas", a.in.TestNamespace, func() client.ObjectList { return &ptahv1.PtahSchemaList{} })
	migrations := newStoredStateRecorder[*ptahv1.PtahMigration](a.t, a.ctx, watcher, "alert-lock-migrations", a.in.TestNamespace, func() client.ObjectList { return &ptahv1.PtahMigrationList{} })
	jobs := newStoredStateRecorder[*batchv1.Job](a.t, a.ctx, watcher, "alert-lock-jobs", a.in.TestNamespace, func() client.ObjectList { return &batchv1.JobList{} })
	pods := newStoredStateRecorder[*corev1.Pod](a.t, a.ctx, watcher, "alert-lock-pods", a.in.TestNamespace, func() client.ObjectList { return &corev1.PodList{} })
	leases := newStoredStateRecorder[*coordinationv1.Lease](a.t, a.ctx, watcher, "alert-lock-leases", a.in.OperatorNamespace, func() client.ObjectList { return &coordinationv1.LeaseList{} })
	var cleanup []client.Object
	defer func() {
		if a.t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for i := len(cleanup) - 1; i >= 0; i-- {
			v := cleanup[i]
			uid := v.GetUID()
			if err := a.cluster.Client.Delete(ctx, v, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
				a.t.Errorf("remove owned lock alert object: %v", err)
			}
		}
		for _, v := range []client.Object{object, sibling} {
			if err := harness.Wait(ctx, "lock alert owner finalization", time.Minute, time.Second, func(ctx context.Context) (bool, string, error) {
				copy := v.DeepCopyObject().(client.Object)
				err := a.cluster.Client.Get(ctx, client.ObjectKeyFromObject(v), copy)
				return apierrors.IsNotFound(err), "waiting for finalizer cleanup", client.IgnoreNotFound(err)
			}); err != nil {
				a.t.Errorf("finish lock alert cleanup: %v", err)
				return
			}
		}
		statement := "DROP DATABASE " + database + " WITH (FORCE)"
		if m.engine.name == "mysql" {
			statement = "DROP DATABASE " + database
		}
		if _, err := serverSQL(ctx, a.cluster, a.in.TestNamespace, m.engine, statement); err != nil {
			a.t.Errorf("drop owned lock alert database: %v", err)
		}
		uid := secret.UID
		if err := a.cluster.Client.Delete(ctx, secret, client.Preconditions{UID: &uid}); err != nil {
			a.t.Errorf("remove owned lock alert Secret: %v", err)
		}
	}()
	var ownerUID types.UID
	var ownerGeneration int64
	read := func() {
		a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(object), object), "read the lock owner")
		if ownerUID != "" && (object.GetUID() != ownerUID || object.GetGeneration() != ownerGeneration) {
			a.fatalf("the lock owner was replaced")
		}
	}
	a.gateOpened = true
	a.check(a.setGate(a.ctx, "open"), "open scheduling for initial planning")
	a.check(a.create(object), "create the real lock alert owner")
	ownerUID, ownerGeneration = object.GetUID(), object.GetGeneration()
	cleanup = append(cleanup, object.DeepCopyObject().(client.Object))
	a.check(harness.Wait(a.ctx, "the owner's real approval gate", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		read()
		return alLockApprovalReady(object), "waiting for the plan that needs approval", nil
	}), "prepare the lock owner for approval")
	a.check(a.setGate(a.ctx, ""), "hold the approved Apply off every node")
	a.gateOpened = false
	var approval client.Object
	switch v := object.(type) {
	case *ptahv1.PtahSchema:
		plan := &ptahv1.PtahSchemaPlan{}
		a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: v.Namespace, Name: v.Status.Plan.Name}, plan), "read the exact schema plan")
		approval = &unstructured.Unstructured{Object: referenceApprovalDocument(v.Namespace, name+"-approval", v.Name, plan)}
	case *ptahv1.PtahMigration:
		plan := &ptahv1.PtahMigrationPlan{}
		a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: v.Namespace, Name: v.Status.Plan.Name}, plan), "read the exact migration plan")
		approval = &unstructured.Unstructured{Object: migrationApprovalDocument(v.Namespace, name+"-approval", v.Name, string(v.UID), plan.Name, string(plan.UID), plan.Spec.Fingerprint)}
	}
	a.check(a.create(approval), "approve only the published plan")
	cleanup = append(cleanup, approval)
	var claimed alLockState
	var originalPod *corev1.Pod
	var originalJob *batchv1.Job
	workload := func(claim alStalledClaim) (*batchv1.Job, *corev1.Pod, bool) {
		job := &batchv1.Job{}
		err := a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: claim.namespace, Name: claim.jobName}, job)
		if apierrors.IsNotFound(err) {
			return nil, nil, false
		}
		a.check(err, "read the exact lock owner's Job")
		list := &corev1.PodList{}
		a.check(a.cluster.Client.List(a.ctx, list, client.InNamespace(claim.namespace)), "read lock owner workloads")
		owned := ownedPods(list.Items, job.UID)
		if len(owned) == 0 {
			return job, nil, false
		}
		if len(owned) != 1 || !alLockWorkload(job, &owned[0], claim) {
			a.fatalf("the lock owner has a replaced or ambiguous workload")
		}
		return job, owned[0].DeepCopy(), true
	}
	a.check(harness.Wait(a.ctx, "the gated Apply under its real Lease", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		read()
		claimed = alLockReading(object)
		if !claimed.claimed() {
			return false, "waiting for the exact Apply claim", nil
		}
		job, pod, found := workload(claimed.claim)
		if !found {
			return false, "waiting for the gated Apply Pod", nil
		}
		if !alStalledPodHeld(pod) {
			return false, "", fmt.Errorf("the approved Apply ran before fault installation")
		}
		originalJob, originalPod = job, pod
		return true, "", nil
	}), "bind the exact gated operation")
	allLeases := &coordinationv1.LeaseList{}
	a.check(a.cluster.Client.List(a.ctx, allLeases, client.InNamespace(a.in.OperatorNamespace)), "read target Leases")
	namespace, leaseName, _, err := leaseAtEpoch(allLeases.Items, claimed.lock.LeaseEpoch)
	a.check(err, "locate the acquired Lease")
	originalLease := &coordinationv1.Lease{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: namespace, Name: leaseName}, originalLease), "read the exact held Lease")
	if !alLockHeld(originalLease, originalLease, claimed.lock) {
		a.fatalf("the claimed binding does not match its held Lease")
	}
	managerLease, managers := a.managerSnapshot()
	account, err := sharedManagerAccount(managers)
	a.check(err, "read the manager account")
	serviceAccount := &corev1.ServiceAccount{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.OperatorNamespace, Name: account}, serviceAccount), "read the manager account UID")
	managerPod, managerPodUID, ok := runningManagerPod(managers)
	if !ok {
		a.fatalf("the release probe needs a running manager identity")
	}
	username := "system:serviceaccount:" + a.in.OperatorNamespace + ":" + account
	manager, err := a.cluster.As(rest.ImpersonationConfig{UserName: username, UID: string(serviceAccount.UID), Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + a.in.OperatorNamespace, "system:authenticated"}, Extra: map[string][]string{"authentication.kubernetes.io/pod-name": {managerPod}, "authentication.kubernetes.io/pod-uid": {managerPodUID}}})
	a.check(err, "use the real manager identity for the release probe")
	probe := func(clear bool) error {
		lease := &coordinationv1.Lease{}
		if err := a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(originalLease), lease); err != nil {
			return err
		}
		if clear {
			empty := ""
			lease.Spec.HolderIdentity = &empty
		} else {
			renewed := metav1.NewMicroTime(time.Now().UTC())
			lease.Spec.RenewTime = &renewed
		}
		return manager.Update(a.ctx, lease, client.DryRunAll)
	}
	a.check(probe(true), "admit the release before injecting its failure")
	policy, binding := alLockReleasePolicy(originalLease, username)
	var faults []client.Object
	removeFault := func(ctx context.Context) error {
		for len(faults) > 0 {
			v := faults[len(faults)-1]
			uid := v.GetUID()
			if err := a.cluster.Client.Delete(ctx, v, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			faults = faults[:len(faults)-1]
		}
		return nil
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := removeFault(ctx); err != nil {
			a.t.Errorf("restore lock release permission: %v", err)
		}
	}()
	denied := func(err error) bool {
		return apierrors.IsForbidden(err) && strings.Contains(err.Error(), policy.Name) && strings.Contains(err.Error(), releaseFaultMessage)
	}
	for _, v := range []client.Object{policy, binding} {
		a.check(a.create(v), "install the exact release fault")
		faults = append(faults, v)
	}
	a.check(harness.Wait(a.ctx, "the specific release-policy refusal", 30*time.Second, time.Second, func(context.Context) (bool, string, error) {
		err := probe(true)
		if err != nil && !denied(err) {
			return false, "", err
		}
		return denied(err), "waiting for release denial", nil
	}), "prove the release fault is effective")
	a.check(probe(false), "keep non-release Lease updates allowed")
	read()
	if alLockReading(object) != claimed {
		a.fatalf("the Apply changed while its release fault was installed")
	}
	_, held, found := workload(claimed.claim)
	if !found || held.UID != originalPod.UID || !alStalledPodHeld(held) {
		a.fatalf("the original executor escaped the Apply gate")
	}
	query := fmt.Sprintf(`ALERTS{alertname=%q,family=%q}`, alLockAlert, family)
	if !a.noActiveAlerts(query) {
		a.fatalf("a lock-release incident already exists for this family")
	}
	from := a.deliveryCount()
	a.gateOpened = true
	a.check(a.setGate(a.ctx, "open"), "run the authorized Apply with release denied")
	var owed alLockState
	a.check(harness.Wait(a.ctx, "the completed Apply and its owed release", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		read()
		owed = alLockReading(object)
		return owed.owes(claimed), "waiting for the controller's completed result and original release obligation", nil
	}), "observe the real owed release")
	// Retain the exact terminal result before the controller's Job TTL can
	// collect it. Database effects below are a separate, direct reading.
	job, pod, found := workload(claimed.claim)
	if !found || pod.UID != originalPod.UID || pod.Status.Phase != corev1.PodSucceeded || !jobComplete(job) {
		a.fatalf("the completed Apply lacks its original successful workload")
	}
	finished, err := alLockTransportFinished(pod)
	a.check(err, "verify every original container finished without a restart")
	logs, err := a.cluster.ContainerLog(a.ctx, pod.Namespace, pod.Name, "ptah")
	a.check(err, "retain the original Apply result")
	m.scan(logs, "lock alert Apply result")
	op := runner.OperationApply
	if family == "migration" {
		op = runner.OperationMigrationApply
	}
	result, err := readOperationResult(a.ctx, a.cluster.Client, job, pod, op, claimed.claim.id, logs)
	a.check(err, "bind the Apply result to its exact operation")
	m.scanObject(result, "lock alert durable Apply result")
	if result.Error != nil || result.ChildExitCode != 0 || result.Uncertain || result.Truncation != nil {
		a.fatalf("the lock alert Apply did not finish with complete success")
	}
	_, samePod, found := workload(claimed.claim)
	if !found || samePod.UID != pod.UID {
		a.fatalf("the Apply Pod changed while retaining its result")
	}
	tableFilter := "table_schema='public'"
	if m.engine.name == "mysql" {
		tableFilter = "table_schema=database()"
	}
	tables, err := databaseSQL(a.ctx, a.cluster, a.in.TestNamespace, m.engine, database, "SELECT count(*) FROM information_schema.tables WHERE "+tableFilter)
	a.check(err, "read actual Apply effects")
	tableCount, err := strconv.ParseUint(strings.TrimSpace(tables), 10, 64)
	a.check(err, "read a nonempty numeric table count")
	if tableCount == 0 {
		a.fatalf("the approved Apply left an empty database")
	}
	checkRuntime := func() {
		currentLease, currentPods := a.managerSnapshot()
		if !alSameManagers(currentLease, currentPods, managerLease, managers) {
			a.fatalf("the manager changed during the release fault")
		}
		body, err := a.prometheus(a.ctx, "/api/v1/targets", nil)
		a.check(err, "read lock alert scrapes")
		if !alTargetsReady(body, int(a.replicas)) {
			a.fatalf("a manager scrape failed during the lock release fault")
		}
		body, err = a.prometheus(a.ctx, "/api/v1/rules", nil)
		a.check(err, "read rules during the lock release proof")
		if !alRulesLoaded(body) {
			a.fatalf("a required rule stopped evaluating during the lock release proof")
		}
	}
	checkHeld := func() {
		for _, r := range []recorder{schemas, migrations, jobs, pods, leases} {
			a.check(r.alive(), "retain the complete lock alert histories")
		}
		read()
		if alLockReading(object) != owed {
			a.fatalf("the exact owed release changed while its update was refused")
		}
		lease := &coordinationv1.Lease{}
		a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(originalLease), lease), "read the held realm")
		if !alLockHeld(lease, originalLease, claimed.lock) || !denied(probe(true)) {
			a.fatalf("the release fault no longer holds the original Lease")
		}
		checkRuntime()
	}
	labels := map[string]string{"family": family, "operator_namespace": a.in.OperatorNamespace, "operator_metrics_service": a.metricsService}
	firing, index := a.waitForDeliveryWithCheck(alMatch{status: "firing", alertName: alLockAlert, labels: labels}, "the owed-lock notification", time.Until(owed.completed.Add(alLockPendingFor+alDetectionSlack)), from, checkHeld)
	if !alLockDelivered(firing, owed.completed) || firing.Labels["severity"] != "warning" || firing.Annotations["runbook_url"] != a.runbookBase+"#resource-state" || !alRunbookAnchor(a.operationsPage(), "resource-state") {
		a.fatalf("the owed-lock notification missed its native bound, severity or runbook")
	}
	for _, label := range []string{"operation", "resource", "pod", "job", "plan", "execution"} {
		if _, ok := firing.Labels[label]; ok {
			a.fatalf("lock alert acquired unbounded label %s", label)
		}
	}
	// The sibling's Plan needs this same Lease. It may resolve and observe,
	// but no Plan Job may start while the original release is still refused.
	a.check(a.create(sibling), "create the authorized sibling")
	siblingUID, siblingGeneration := sibling.UID, sibling.Generation
	readSibling := func() {
		a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(sibling), sibling), "read the unchanged sibling")
		if sibling.UID != siblingUID || sibling.Generation != siblingGeneration {
			a.fatalf("the sibling was replaced or edited")
		}
	}
	cleanup = append(cleanup, sibling.DeepCopy())
	var siblingClaim alStalledClaim
	a.check(harness.Wait(a.ctx, "a sibling Plan waiting on the same realm", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		checkHeld()
		readSibling()
		active := sibling.Status.ActiveOperation
		if active == nil || active.Type != ptahv1.OperationPlan {
			return false, "waiting for the sibling Plan claim", nil
		}
		if active.CoordinationDigest != claimed.lock.CoordinationDigest {
			return false, "", fmt.Errorf("the sibling names a different database realm")
		}
		if active.JobUID != "" {
			return false, "", fmt.Errorf("the sibling Plan ran before the release")
		}
		siblingClaim = alStalledReading(sibling)
		return true, "", nil
	}), "prove sibling contention")
	checkHeld()
	a.check(removeFault(a.ctx), "restore the exact release permission")
	var released time.Time
	a.check(harness.Wait(a.ctx, "the watched native Lease release", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		checkRuntime()
		for _, event := range leases.snapshot() {
			if at, ok := alLockReleased(event.Object, originalLease, owed.completed); ok {
				released = at
				break
			}
		}
		read()
		return !released.IsZero() && !alLockReading(object).owed, "waiting for original holder removal and cleared obligation", nil
	}), "observe release completion")
	resolved, _ := a.waitForDeliveryWithCheck(alMatch{status: "resolved", alertName: alLockAlert, labels: labels}, "the owed-lock resolution", max(time.Until(released.Add(alDetectionSlack)), alDeliveryPoll), index+1, checkRuntime)
	if !alOverdueCleared(firing, resolved, released) || !a.noActiveAlerts(query) {
		a.fatalf("owed-lock resolution missed the native release deadline")
	}
	var successor alStalledClaim
	var successorLock ptahv1.TargetLockReleaseStatus
	a.check(harness.Wait(a.ctx, "the sibling's new Plan under the released realm", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		for _, event := range schemas.snapshot() {
			v := event.Object
			active := v.Status.ActiveOperation
			if v.UID == sibling.UID && active != nil && active.ID == siblingClaim.id && active.Type == ptahv1.OperationPlan && active.JobUID != "" && active.LeaseEpoch != "" && active.LeaseEpoch != claimed.lock.LeaseEpoch {
				successor = alStalledReading(v)
				successorLock = alLockReading(v).lock
			}
		}
		readSibling()
		return successor.jobUID != "" && alLockSiblingReady(sibling), "waiting for the exact sibling Plan to complete", nil
	}), "verify authorized progress after release")
	var siblingJob *batchv1.Job
	var siblingPod *corev1.Pod
	for _, event := range jobs.snapshot() {
		if event.Object.UID == successor.jobUID {
			siblingJob = event.Object
		}
	}
	for _, event := range pods.snapshot() {
		parent := metav1.GetControllerOf(event.Object)
		if parent != nil && parent.UID == successor.jobUID {
			siblingPod = event.Object
		}
	}
	if siblingJob == nil || siblingPod == nil || !alLockWorkload(siblingJob, siblingPod, successor) || siblingPod.Status.Phase != corev1.PodSucceeded {
		a.fatalf("the sibling has no successful workload created after release")
	}
	var leaseHistory []*coordinationv1.Lease
	for _, event := range leases.snapshot() {
		leaseHistory = append(leaseHistory, event.Object)
	}
	if !alLockHandoff(leaseHistory, originalLease, successorLock, released) {
		a.fatalf("the sibling has no new acquisition after the original release")
	}
	if _, err := alLockTransportFinished(siblingPod); err != nil {
		a.fatalf("the sibling Plan has incomplete terminal transport: %v", err)
	}
	for _, status := range siblingPod.Status.ContainerStatuses {
		if status.State.Terminated.StartedAt.Time.Before(finished) {
			a.fatalf("the sibling ran before the original executor finished")
		}
	}
	remaining, err := databaseSQL(a.ctx, a.cluster, a.in.TestNamespace, m.engine, database, "SELECT count(*) FROM information_schema.tables WHERE "+tableFilter)
	a.check(err, "verify the sibling left the database unchanged")
	if strings.TrimSpace(remaining) != strings.TrimSpace(tables) {
		a.fatalf("the read-only sibling changed database tables")
	}
	// Barrier each mutable collection before evaluating its complete history.
	read()
	switch v := object.(type) {
	case *ptahv1.PtahSchema:
		storedStateWatchBarrier(a.t, a.ctx, a.cluster, schemas, v)
	case *ptahv1.PtahMigration:
		storedStateWatchBarrier(a.t, a.ctx, a.cluster, migrations, v)
	}
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, schemas, sibling)
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, jobs, siblingJob)
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, pods, siblingPod)
	currentLease := &coordinationv1.Lease{}
	a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(originalLease), currentLease), "read the final realm Lease")
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, leases, currentLease)
	closeRunnerWatches(a.t, []recorder{schemas, migrations, jobs, pods, leases}, m.scan)
	applyJobs := map[types.UID]bool{}
	for _, event := range jobs.snapshot() {
		v := event.Object
		owner := metav1.GetControllerOf(v)
		if owner == nil {
			continue
		}
		if owner.UID == object.GetUID() && v.Labels[labelOperation] == "apply" {
			applyJobs[v.UID] = true
		}
		if owner.UID == sibling.UID && v.Labels[labelOperation] == "apply" {
			a.fatalf("the Never sibling dispatched an Apply")
		}
	}
	checkClaims := func(v client.Object) {
		r := alStalledReading(v)
		if r.uid == object.GetUID() && r.operation == "Apply" && (r.id != claimed.claim.id || r.jobUID != "" && r.jobUID != claimed.claim.jobUID) {
			a.fatalf("the owner claimed another Apply")
		}
		if r.uid == sibling.UID && r.operation == "Apply" {
			a.fatalf("the sibling claimed an Apply")
		}
	}
	for _, event := range schemas.snapshot() {
		checkClaims(event.Object)
	}
	for _, event := range migrations.snapshot() {
		checkClaims(event.Object)
	}
	originalPods := map[types.UID]bool{}
	for _, event := range pods.snapshot() {
		parent := metav1.GetControllerOf(event.Object)
		if parent != nil && parent.UID == originalJob.UID {
			originalPods[event.Object.UID] = true
		}
	}
	if len(originalPods) != 1 || !originalPods[originalPod.UID] {
		a.fatalf("the Apply dispatched a replacement executor")
	}
	if len(applyJobs) != 1 || !applyJobs[originalJob.UID] {
		a.fatalf("the original Apply was missing or replayed")
	}
	a.check(a.setGate(a.ctx, ""), "close the lock alert scheduling gate")
	a.gateOpened = false
	a.logf("PASS %s %s owed lock: resourceUID=%s operation=%s jobUID=%s leaseUID=%s epoch=%s completedAt=%s firingReceived=%s releasedAt=%s resolvedReceived=%s siblingJobUID=%s", m.engine.name, family, object.GetUID(), claimed.claim.id, claimed.claim.jobUID, originalLease.UID, claimed.lock.LeaseEpoch, owed.completed, firing.ReceivedAt, released, resolved.ReceivedAt, successor.jobUID)
}
