//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/plancontract"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (a *alertingRun) planStoreLarge() {
	const database, secretName = "ptah_alert_plan_store", "e2e-alert-plan-store-db"
	template := &ptahv1.PtahSchema{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.TestNamespace, Name: "e2e-reference-postgresql"}, template), "read the native plan producer")
	registry, err := url.Parse(template.Spec.Desired.OCIRef)
	a.check(err, "read the plan registry")
	if registry.Scheme != "oci" || registry.Host == "" || registry.User != nil || template.Status.ExecutionBinding == nil || !alPinnedImage.MatchString(template.Status.ExecutionBinding.ExecutorImage) {
		a.fatalf("plan producer lacks its registry or exact executor")
	}
	executor := template.Status.ExecutionBinding.ExecutorImage
	engine, err := migrationEngineFor("postgresql")
	a.check(err, "select the prepared plan database")
	m := &migrationRun{t: a.t, parent: a.t, ctx: a.ctx, cluster: a.cluster, engine: engine, workDir: a.workDir, in: phases.MigrationsInputs{TestNamespace: a.in.TestNamespace, ExecutorImage: executor}}
	credential := &corev1.Secret{}
	a.check(m.get(engine.sourceSecret, credential), "read the prepared database credential")
	m.password = string(credential.Data["password"])
	if m.password == "" {
		a.fatalf("the plan database has no credential")
	}
	m.protect(m.password, a.credentials.Password)
	m.isolatedDatabase(database, secretName)
	secret := &corev1.Secret{}
	a.check(m.get(secretName, secret), "retain the isolated Secret identity")
	checkEmptyDatabase := func() {
		body, err := databaseSQL(a.ctx, a.cluster, a.in.TestNamespace, engine, database, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'")
		a.check(err, "read the isolated database's actual tables")
		if strings.TrimSpace(body) != "0" {
			a.fatalf("the Never plan-store consumers changed their database")
		}
	}
	checkEmptyDatabase()
	var resources []*ptahv1.PtahSchema
	var publishers []*batchv1.Job
	defer func() {
		if a.t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		for _, s := range resources {
			uid := s.UID
			a.check(a.cluster.Client.Delete(ctx, s, client.Preconditions{UID: &uid}), "remove the owned plan-store consumer")
		}
		a.check(harness.Wait(ctx, "plan-store fixture finalization", 2*time.Minute, time.Second, func(ctx context.Context) (bool, string, error) {
			for _, s := range resources {
				v := &ptahv1.PtahSchema{}
				err := a.cluster.Client.Get(ctx, client.ObjectKeyFromObject(s), v)
				if !apierrors.IsNotFound(err) {
					return false, "waiting for original consumer deletion", err
				}
			}
			return true, "", nil
		}), "finalize the plan-store resources")
		for _, j := range publishers {
			uid := j.UID
			a.check(a.cluster.Client.Delete(ctx, j, client.Preconditions{UID: &uid}), "remove the plan-store publisher")
		}
		_, err := serverSQL(ctx, a.cluster, a.in.TestNamespace, engine, "DROP DATABASE "+database+" WITH (FORCE)")
		a.check(err, "drop the isolated plan-store database")
		uid := secret.UID
		a.check(a.cluster.Client.Delete(ctx, secret, client.Preconditions{UID: &uid}), "remove the isolated plan-store Secret")
	}()
	watcher, err := client.NewWithWatch(a.cluster.Config, client.Options{Scheme: a.cluster.Scheme})
	a.check(err, "open plan-store histories")
	schemas := newStoredStateRecorder[*ptahv1.PtahSchema](a.t, a.ctx, watcher, "alert-plan-store-schemas", a.in.TestNamespace, func() client.ObjectList { return &ptahv1.PtahSchemaList{} })
	jobs := newStoredStateRecorder[*batchv1.Job](a.t, a.ctx, watcher, "alert-plan-store-jobs", a.in.TestNamespace, func() client.ObjectList { return &batchv1.JobList{} })
	lease, managers := a.managerSnapshot()
	leader := haLeaderPodName(haLeaseHolder(lease))
	monitor := a.negativeMonitorIdentity()
	var names []string
	for _, p := range managers {
		names = append(names, p.Name)
	}
	check := func() {
		l, p := a.managerSnapshot()
		if !alSameManagers(l, p, lease, managers) || !maps.Equal(monitor, a.negativeMonitorIdentity()) {
			a.fatalf("manager or monitoring identities changed during plan storage")
		}
		body, err := a.prometheus(a.ctx, "/api/v1/targets", nil)
		a.check(err, "read plan-store scrape targets")
		if !alTargetsReady(body, int(a.replicas)) {
			a.fatalf("plan-store proof lost a healthy scrape")
		}
		body, err = a.prometheus(a.ctx, "/api/v1/rules", nil)
		a.check(err, "read plan-store rules")
		if !alRulesLoaded(body) {
			a.fatalf("plan-store proof lost an enabled rule")
		}
		a.check(schemas.alive(), "retain plan-store resource history")
		a.check(jobs.alive(), "retain plan-store Job history")
	}
	publish := func(revision string, repeated, suffix int) string {
		ref := "oci://" + registry.Host + "/e2e-alert-plan-store:" + revision
		j := planSizePublisherJob(a.in.TestNamespace, a.in.FixtureImage, executor, "postgres", "alert-store-"+revision, ref, repeated, suffix)
		a.check(a.cluster.Client.Create(a.ctx, j), "publish the real plan-store artifact")
		publishers = append(publishers, j.DeepCopy())
		a.check(harness.Wait(a.ctx, "the real plan-store publisher", 5*time.Minute, time.Second, func(context.Context) (bool, string, error) {
			check()
			current := &batchv1.Job{}
			err := a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(j), current)
			if err != nil {
				return false, "", err
			}
			if current.UID != j.UID {
				return false, "", fmt.Errorf("publisher replaced")
			}
			if conditionTrue(current.Status.Conditions, batchv1.JobFailed) {
				return false, "", fmt.Errorf("publisher failed")
			}
			return jobComplete(current), "waiting for native publication", nil
		}), "complete the real plan-store publication")
		pods := &corev1.PodList{}
		a.check(a.cluster.Client.List(a.ctx, pods, client.InNamespace(a.in.TestNamespace)), "read publisher transport")
		owned := ownedPods(pods.Items, j.UID)
		if len(owned) != 1 {
			a.fatalf("publisher has no original transport")
		}
		_, err := alLockTransportFinished(&owned[0])
		a.check(err, "require successful publication transport")
		logs, err := a.cluster.ContainerLog(a.ctx, a.in.TestNamespace, owned[0].Name, "publisher")
		a.check(err, "read native artifact digest")
		m.scan(logs, "plan-store publisher")
		digests := publishedDigests(logs)
		if len(digests) != 1 || !sha256Pattern.MatchString(digests[0]) {
			a.fatalf("publisher did not return one exact artifact digest")
		}
		return ref + "@" + digests[0]
	}
	// Each resource has its own fingerprint, but all use one isolated database
	// and one coordination key. Never permits native Plan and forbids Apply.
	create := func(suffix, reference string) *ptahv1.PtahSchema {
		object, err := alNegativeFixture(template, "e2e-alert-store-"+suffix, secretName, ptahv1.ApplyPolicyNever)
		a.check(err, "build the native plan consumer")
		s := object.(*ptahv1.PtahSchema)
		s.Spec.Target.URLFrom.Key = "url"
		s.Spec.Target.CoordinationKey = "e2e/alert-plan-store"
		s.Spec.Desired.OCIRef = reference
		s.Spec.Interval.Duration = time.Hour
		s.Spec.Execution.NodeSelector = nil
		s.Spec.Execution.Resources = corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}}
		a.check(a.cluster.Client.Create(a.ctx, s), "create the native plan consumer")
		resources = append(resources, s.DeepCopy())
		return s
	}
	read := func(s *ptahv1.PtahSchema) *ptahv1.PtahSchema {
		v := &ptahv1.PtahSchema{}
		a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(s), v), "read the native plan consumer")
		if v.UID != s.UID {
			a.fatalf("plan-store consumer replaced")
		}
		return v
	}
	suspend := func(s *ptahv1.PtahSchema) {
		before := s.DeepCopy()
		s.Spec.Suspend = true
		a.check(a.cluster.Client.Patch(a.ctx, s, client.MergeFrom(before)), "suspend the settled plan owner")
		a.check(harness.Wait(a.ctx, "a quiescent original plan owner", alTimeout, time.Second, func(context.Context) (bool, string, error) {
			check()
			v := read(s)
			return alPlanStoreQuiescent(v), "waiting for native suspension", nil
		}), "establish the pruning maintenance window")
	}
	ready := func(s *ptahv1.PtahSchema) *ptahv1.PtahSchemaPlan {
		var v *ptahv1.PtahSchema
		a.check(harness.Wait(a.ctx, "a native stored Plan", alTimeout, time.Second, func(context.Context) (bool, string, error) {
			check()
			v = read(s)
			return alLockSiblingReady(v) && v.Status.Plan != nil && v.Status.Source.Digest == s.Spec.Desired.OCIRef[strings.LastIndex(s.Spec.Desired.OCIRef, "@")+1:], "waiting for the exact native Plan", nil
		}), "retain a real stored payload")
		p := &ptahv1.PtahSchemaPlan{}
		a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: v.Namespace, Name: v.Status.Plan.Name}, p), "read the exact native plan")
		if p.UID != v.Status.Plan.UID || p.Spec.SchemaRef.UID != s.UID {
			a.fatalf("stored plan lost its original owner")
		}
		suspend(v)
		return p
	}
	var smallRef string
	var sizes []int
	for i, row := range []struct {
		revision         string
		repeated, suffix int
	}{{"small", 32, 0}, {"plus", 33, 0}, {"ascii", 32, 1}} {
		ref := publish(row.revision, row.repeated, row.suffix)
		if i == 0 {
			smallRef = ref
		}
		p := ready(create(row.revision, ref))
		e := a.readPlanStoreExport(p)
		_, err := e.document()
		a.check(err, "verify the calibration payload")
		sizes = append(sizes, int(p.Spec.Size))
	}
	calibration, err := calibratePlanSize(sizes[0], sizes[1], sizes[2])
	a.check(err, "calibrate the actual native serializer")
	repeated, suffix, err := calibration.lengths(int(plancontract.MaxExecutableBytes))
	a.check(err, "derive full-size native payloads")
	largeRef := publish("large", repeated, suffix)
	var large []*ptahv1.PtahSchemaPlan
	// Prepare below the threshold, then start the measured history before the
	// final two plans can cross it. No alert deadline starts at a later poll.
	allocate := func(start, end int) {
		for i := start; i < end; {
			var batch []*ptahv1.PtahSchema
			for len(batch) < 4 && i < end {
				batch = append(batch, create(fmt.Sprintf("large-%02d", i), largeRef))
				i++
			}
			for _, s := range batch {
				p := ready(s)
				e := a.readPlanStoreExport(p)
				doc, err := e.document()
				a.check(err, "reconstruct the actual stored payload")
				a.check(executablePlanAtLimit(p, doc, "postgres", repeated, suffix), "require real full-size executable plan bytes")
				large = append(large, p)
			}
		}
	}
	allocate(0, 15)
	query := fmt.Sprintf(`ALERTS{alertname=%q}`, alPlanStoreAlert)
	if !a.noActiveAlerts(query) {
		a.fatalf("plan store already exceeds the frozen budget before the measured fault")
	}
	baselinePlans := &ptahv1.PtahSchemaPlanList{}
	a.check(a.cluster.Client.List(a.ctx, baselinePlans), "measure the complete retained manifest census before the fault")
	var baselineBytes int64
	for _, p := range baselinePlans.Items {
		baselineBytes += p.Spec.Size
	}
	if baselineBytes > alPlanStoreLimit {
		a.fatalf("the store already crossed its budget before fault timing began")
	}
	from := a.deliveryCount()
	started := time.Now().UTC()
	baseline := a.planStoreHistory(names, leader, started, "")
	if baseline.latest > alPlanStoreLimit || !baseline.crossedLower.IsZero() {
		a.fatalf("no native below-threshold baseline")
	}
	allocate(15, alPlanStoreCount)
	firing, index := a.waitForDeliveryWithCheck(alMatch{status: "firing", alertName: alPlanStoreAlert, labels: map[string]string{"operator_namespace": a.in.OperatorNamespace, "operator_metrics_service": a.metricsService}}, "the retained-plan notification", alDetectionSlack, from, check)
	history := a.planStoreHistory(names, leader, started, "firing")
	if !alPlanStoreDelivered(firing, history) || firing.Labels["severity"] != "warning" || firing.Annotations["runbook_url"] != a.runbookBase+"#prune-plans" || !alRunbookAnchor(a.operationsPage(), "prune-plans") {
		a.fatalf("plan-store notification missed its native bound or pruning runbook")
	}
	for _, key := range []string{"family", "resource", "plan", "job", "pod", "execution"} {
		if _, ok := firing.Labels[key]; ok {
			a.fatalf("plan-store alert acquired unbounded or inapplicable label %s", key)
		}
	}
	var total int64
	for _, p := range large {
		total += p.Spec.Size
	}
	if len(large) != alPlanStoreCount || total <= alPlanStoreLimit {
		a.fatalf("test-owned native payloads do not exceed the frozen threshold")
	}
	// Publishing the last manifest can precede its next scrape. Require the
	// counter-equivalent byte reading without moving the firing deadline.
	a.check(harness.Wait(a.ctx, "all owned bytes in a native scrape", 2*alScrapeInterval+alScrapeTimeout, time.Second, func(context.Context) (bool, string, error) {
		check()
		h := a.planStoreHistory(names, leader, started, "")
		return h.latest >= total, "waiting for the final retained manifest scrape", nil
	}), "measure every owned payload in the native gauge")
	// Replace the current plan, then suspend each owner. Its old large plan
	// stays retained but no longer has an active, pending or approval pin.
	for start := 0; start < len(large); start += 4 {
		var batch []*ptahv1.PtahSchema
		for _, p := range large[start:min(start+4, len(large))] {
			s := read(&ptahv1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Spec.SchemaRef.Name, UID: p.Spec.SchemaRef.UID}})
			before := s.DeepCopy()
			s.Spec.Desired.OCIRef, s.Spec.Suspend = smallRef, false
			a.check(a.cluster.Client.Patch(a.ctx, s, client.MergeFrom(before)), "replace desired input while retaining its historical plan")
			batch = append(batch, s)
		}
		for _, s := range batch {
			replacement := ready(s)
			if replacement.Spec.Size != int64(sizes[0]) {
				a.fatalf("replacement did not release its large predecessor")
			}
		}
	}
	pins := a.planStorePins()
	retained := a.planStorePinnedBytes(pins)
	// Export before any deletion. Archives are reconstructed from disk and
	// retained in the phase log, so cleanup cannot erase the evidence.
	for _, p := range large {
		s := read(&ptahv1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Spec.SchemaRef.Name, UID: p.Spec.SchemaRef.UID}})
		a.check(alPlanMayPrune(p, s, pins), "refuse every pinned or active deletion")
		e := a.readPlanStoreExport(p)
		archive, err := e.archive()
		a.check(err, "export exact historical plan bytes")
		path := filepath.Join(a.workDir, string(p.UID)+".plan.json.gz")
		a.check(os.WriteFile(path, archive, 0o600), "write the protected plan export")
		body, err := os.ReadFile(path)
		a.check(err, "read back the protected plan export")
		_, err = alReadPlanExport(body)
		a.check(err, "reconstruct the persisted export before pruning")
		raw, err := json.Marshal(e)
		a.check(err, "encode the exported evidence")
		m.scan(raw, "plan-store export")
		a.logf("plan-store export: plan=%s uid=%s size=%d contentDigest=%s archiveSHA256=%x gzipBase64=%s", p.Name, p.UID, p.Spec.Size, p.Spec.ContentDigest, sha256.Sum256(body), base64.StdEncoding.EncodeToString(body))
	}
	beforePrune := a.planStoreHistory(names, leader, started, "")
	if !beforePrune.clearedLower.IsZero() {
		a.fatalf("plan store cleared before safe pruning")
	}
	pruningStarted := time.Now().UTC()
	for _, p := range large {
		check()
		current := &ptahv1.PtahSchemaPlan{}
		a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(p), current), "reread the original plan before deleting")
		if current.UID != p.UID || !equality.Semantic.DeepEqual(current.Spec, p.Spec) {
			a.fatalf("historical plan changed before pruning")
		}
		owner := read(&ptahv1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Spec.SchemaRef.Name, UID: p.Spec.SchemaRef.UID}})
		a.check(alPlanMayPrune(current, owner, a.planStorePins()), "recheck the complete live pin set")
		uid, rv := current.UID, current.ResourceVersion
		a.check(a.cluster.Client.Delete(a.ctx, current, client.Preconditions{UID: &uid, ResourceVersion: &rv}), "prune only the original unpinned plan")
	}
	resolved, _ := a.waitForDeliveryWithCheck(alMatch{status: "resolved", alertName: alPlanStoreAlert, labels: map[string]string{"operator_namespace": a.in.OperatorNamespace, "operator_metrics_service": a.metricsService}}, "the plan-store recovery notification", alDetectionSlack, index+1, check)
	a.check(harness.Wait(a.ctx, "native plan-store recovery samples", 2*alScrapeInterval+alScrapeTimeout, time.Second, func(context.Context) (bool, string, error) {
		check()
		history = a.planStoreHistory(names, leader, started, "")
		return !history.through.Before(resolved.ReceivedAt), "waiting for the native recovery scrape", nil
	}), "retain the original recovery bound")
	history = a.planStoreHistory(names, leader, started, "resolved")
	if !alPlanStoreResolved(firing, resolved, history, pruningStarted) || !a.noActiveAlerts(query) {
		a.fatalf("plan-store resolution missed safe pruning or its native bound")
	}
	a.check(harness.Wait(a.ctx, "plan and chunk garbage collection", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		check()
		for _, p := range large {
			v := &ptahv1.PtahSchemaPlan{}
			err := a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(p), v)
			if !apierrors.IsNotFound(err) {
				return false, "plan still retained", err
			}
			for _, r := range p.Spec.Chunks {
				c := &ptahv1.PtahSchemaPlanChunk{}
				err := a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: p.Namespace, Name: r.Name}, c)
				if !apierrors.IsNotFound(err) {
					return false, "chunk still retained", err
				}
			}
			cms := &corev1.ConfigMapList{}
			err = a.cluster.Client.List(a.ctx, cms, client.InNamespace(p.Namespace), client.MatchingLabels{"operator.ptah.run/plan": p.Name})
			if err != nil {
				return false, "", err
			}
			if len(cms.Items) != 0 {
				return false, "projection still retained", nil
			}
		}
		return true, "", nil
	}), "require native garbage collection of the pruned payloads")
	afterPins := a.planStorePins()
	if !maps.Equal(pins, afterPins) || !maps.Equal(retained, a.planStorePinnedBytes(afterPins)) {
		a.fatalf("safe pruning changed or lost a retained pinned plan")
	}
	for _, s := range resources {
		v := read(s)
		if !alPlanStoreQuiescent(v) {
			a.fatalf("a maintenance owner resumed during pruning")
		}
		storedStateWatchBarrier(a.t, a.ctx, a.cluster, schemas, v)
	}
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, jobs, publishers[len(publishers)-1])
	closeRunnerWatches(a.t, []recorder{schemas, jobs}, m.scan)
	owned := map[types.UID]bool{}
	for _, s := range resources {
		owned[s.UID] = true
	}
	seen := map[types.UID]bool{}
	for _, e := range schemas.snapshot() {
		s := e.Object
		if !owned[s.UID] {
			continue
		}
		seen[s.UID] = true
		if s.Spec.Policy.Apply != ptahv1.ApplyPolicyNever || s.Status.Applied != nil || s.Status.PendingObservation != nil || s.Status.ActiveOperation != nil && s.Status.ActiveOperation.Type == ptahv1.OperationApply {
			a.fatalf("closed plan-store history contains mutation authority")
		}
	}
	if len(seen) != len(resources) {
		a.fatalf("plan-store resource history omitted an owner")
	}
	for _, e := range jobs.snapshot() {
		owner := metav1.GetControllerOf(e.Object)
		if owner != nil && owned[owner.UID] && e.Object.Labels[labelOperation] == "apply" {
			a.fatalf("the plan-store proof dispatched Apply")
		}
	}
	checkEmptyDatabase()
	a.logf("PASS plan-store: %d native plans retained %d bytes; exact exports precede UID-bound pruning; %d pins remain readable; firing=%s resolution=%s. Migration SQL payload storage is not applicable.", len(large), total, len(pins), firing.ReceivedAt, resolved.ReceivedAt)
}

func (a *alertingRun) readPlanStoreExport(plan *ptahv1.PtahSchemaPlan) alPlanExport {
	e := alPlanExport{Plan: plan.DeepCopy()}
	for _, r := range plan.Spec.Chunks {
		c := ptahv1.PtahSchemaPlanChunk{}
		a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: plan.Namespace, Name: r.Name}, &c), "read an original retained chunk")
		e.Chunks = append(e.Chunks, c)
	}
	_, err := e.document()
	a.check(err, "reconstruct the retained plan by exact hashes and owners")
	return e
}
func (a *alertingRun) planStorePins() alPlanPinSet {
	s, m, sa, ma := &ptahv1.PtahSchemaList{}, &ptahv1.PtahMigrationList{}, &ptahv1.PtahSchemaApprovalList{}, &ptahv1.PtahMigrationApprovalList{}
	for _, list := range []client.ObjectList{s, m, sa, ma} {
		a.check(a.cluster.Client.List(a.ctx, list, client.InNamespace(a.in.TestNamespace)), "collect every documented plan pin")
	}
	pins, err := alPlanPins(s.Items, m.Items, sa.Items, ma.Items)
	a.check(err, "require the complete nonempty pin inventory")
	return pins
}
func (a *alertingRun) planStorePinnedBytes(pins alPlanPinSet) map[alPlanKey]string {
	result := map[alPlanKey]string{}
	for key, uid := range pins {
		if key.family == "schema" {
			p := &ptahv1.PtahSchemaPlan{}
			a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: key.namespace, Name: key.name}, p), "load every retained schema pin")
			if p.UID != uid {
				a.fatalf("schema pin replaced")
			}
			e := a.readPlanStoreExport(p)
			result[key] = e.Plan.Spec.ContentDigest
		} else {
			p := &ptahv1.PtahMigrationPlan{}
			a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: key.namespace, Name: key.name}, p), "load every retained migration pin")
			if p.UID != uid {
				a.fatalf("migration pin replaced")
			}
			body, err := json.Marshal(p.Spec)
			a.check(err, "retain the migration plan binding")
			result[key] = fmt.Sprintf("sha256:%x", sha256.Sum256(body))
		}
	}
	return result
}
func (a *alertingRun) planStoreHistory(pods []string, leader string, started time.Time, label string) alPlanStoreHistory {
	at := time.Now().UTC()
	query := func(metric string) []byte {
		body, err := a.prometheus(a.ctx, "/api/v1/query", map[string]string{"query": fmt.Sprintf(`%s{job=%q}[%ds]`, metric, alScrapeJob, int(alAdmissionHistoryWindow/time.Second)), "time": at.Format(time.RFC3339Nano)})
		a.check(err, "read native plan-store history")
		return body
	}
	gauge, up, duration := query(alPlanStoreMetric), query("up"), query("scrape_duration_seconds")
	h, err := alReadPlanStoreHistory(gauge, up, duration, pods, leader, started, at)
	a.check(err, "require complete native plan-store measurements")
	if label != "" {
		a.logf("plan-store history %s: queriedAt=%s bytes=%s up=%s durations=%s", label, at.Format(time.RFC3339Nano), bytes.TrimSpace(gauge), bytes.TrimSpace(up), bytes.TrimSpace(duration))
	}
	return h
}
