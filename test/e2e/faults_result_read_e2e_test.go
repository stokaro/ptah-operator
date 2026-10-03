//go:build e2e

package e2e

import (
	"maps"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func (f *faultRun) hungResultReads() {
	f.t.Helper()
	nodes := &corev1.NodeList{}
	f.check(f.cluster.Client.List(f.ctx, nodes, client.MatchingLabels{isolationNodeKey: "true"}), "read the fault worker")
	if len(nodes.Items) != 1 || !isolationWorkerReady(&nodes.Items[0]) {
		f.fatalf("hung schema result reads require exactly one Ready isolation worker")
	}
	for _, engine := range []string{"postgresql", "mysql"} {
		f.hungResultRead(engine, nodes.Items[0].Name)
	}
}

func (f *faultRun) hungResultRead(engine, node string) {
	f.t.Helper()
	name, healthy := "e2e-schema-hung-"+engine, "e2e-schema-progress-"+engine
	database, healthyDB := "e2e_schema_hung", "e2e_schema_progress"
	secret, healthySecret := name+"-db", healthy+"-db"
	// Reaching post-Apply convergence proves the accepted name ceiling also
	// fits the generated Jobs, Pods, their labels and their result bindings.
	name += strings.Repeat("x", 63-len(name))
	f.createDatabase(engine, database, secret)
	f.query(engine, database, "INSERT INTO e2e_widgets (id, name, note) VALUES (701, 'name-boundary', 'preserve-this-row')")
	f.createDatabase(engine, healthyDB, healthySecret)
	kind, reference := "PostgreSQL", f.pgReference
	if engine == "mysql" {
		kind, reference = "MySQL", f.mysqlReference
	}
	f.createSchema(faultSchema{name: name, engine: kind, reference: reference, secret: secret,
		coordinationKey: "e2e/schema-hung/" + engine, isolatedNode: node})
	f.createSchema(faultSchema{name: healthy, engine: kind, reference: reference, secret: healthySecret,
		coordinationKey: "e2e/schema-progress/" + engine})
	inventory := &schemaSQLInventory{jobs: map[types.UID]batchv1.Job{}, pods: map[types.UID]corev1.Pod{}}
	created := f.waitForSchema(name, "the maximum-name schema's approval gate", func(resource *ptahv1alpha1.PtahSchema) bool {
		f.captureSchemaSQLInventory(name, inventory)
		return planAwaitingApproval(resource)
	})
	plan := f.schemaPlan(created.Status.Plan.Name)
	f.waitForPlan(healthy)
	refused := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Name: name + "x", Namespace: created.Namespace}, Spec: *created.Spec.DeepCopy()}
	f.check(resourceNameRefusal(f.cluster.Client.Create(f.ctx, refused), "PtahSchema", refused.Name), "refuse the schema name immediately above its executable limit")
	if !apierrors.IsNotFound(f.get(refused.Name, &ptahv1alpha1.PtahSchema{})) {
		f.fatalf("the refused 64-byte schema name was stored")
	}
	audit := &databaseSQLAudit{t: f.t, ctx: f.ctx, cluster: f.cluster, namespace: f.in.TestNamespace, engine: engine}
	beforeSQL, beforeApply := audit.snapshot(), f.checkpointJobs(name, "apply")
	f.startReadBarrier()
	f.createApproval(name, "e2e-schema-hung-approval-"+engine)
	claimed := f.waitForSchema(name, "an Apply held before the result-read fault", applyDispatched)
	active := claimed.Status.ActiveOperation.DeepCopy()
	f.assertReadBlocked(string(active.JobUID), "the schema Apply before its log response is replaced")
	f.loadHeldLeaseForEpoch(active.LeaseEpoch, "the unread schema Apply's Lease")
	var pod *corev1.Pod
	f.poll("the held schema Apply Pod", func() bool {
		pods := &corev1.PodList{}
		f.check(f.list(pods, client.MatchingLabels{"job-name": active.JobName}), "read held schema Apply Pod")
		if len(pods.Items) != 1 {
			return false
		}
		pod = &pods.Items[0]
		owner := metav1.GetControllerOf(pod)
		if pod.Spec.NodeName != "" || owner == nil || owner.Kind != "Job" || owner.UID != active.JobUID {
			f.fatalf("the schema Apply escaped its scheduling gate or belongs to another Job")
		}
		return true
	})
	fault := (&logStall{t: f.t, ctx: f.ctx, cluster: f.cluster, dockerContext: f.in.DockerContext,
		node: node, workDir: f.workDir, namespace: f.in.TestNamespace, suffix: "schema-" + engine}).start(pod.Name)
	f.stopReadBarrier()
	// The fault is installed before scheduling, so no post-execution log
	// can be read. Keep credential auditing off this deliberately held path.
	f.pollQuiet("the original durable schema Apply to complete", func() bool {
		job := &batchv1.Job{}
		f.check(f.get(active.JobName, job), "read the schema Apply Job")
		if !durableResultJob(job) {
			f.fatalf("log independence requires durable result delivery")
		}
		return job.UID == active.JobUID && jobComplete(job)
	})
	assertHeld, releaseDiagnostic := fault.holdDiagnostic(pod.Name)
	f.createApproval(healthy, healthy+"-approval")
	approval := &ptahv1alpha1.PtahSchemaApproval{}
	f.check(f.get(healthy+"-approval", approval), "read the independent schema approval timestamp")
	deadline := approval.CreationTimestamp.Add(180 * time.Second)
	progressed, originalConverged := false, false
	for time.Now().Before(deadline) {
		f.captureSchemaSQLInventory(name, inventory)
		if f.addedJobCount(name, "apply") > 1 {
			f.fatalf("%s replayed Apply while logs were unavailable", name)
		}
		originalConverged = schemaReadProgress(f.schema(name), active.StartedAt.Time, deadline)
		progressed = schemaReadProgress(f.schema(healthy), approval.CreationTimestamp.Time, deadline)
		if originalConverged && progressed {
			break
		}
		f.sleep(2 * time.Second)
	}
	if !originalConverged || !progressed {
		f.fatalf("durable results depended on unavailable logs: original convergence=%t independent convergence=%t within 180s", originalConverged, progressed)
	}
	assertHeld()
	for _, db := range []string{database, healthyDB} {
		f.assertColumn(engine, db, "fault_token", 1)
	}
	releaseDiagnostic()
	f.check(fault.stop(f.ctx), "restore diagnostic log reads")
	settled := f.waitForSchema(name, "the original schema Apply to converge after log recovery", freshApprovalConverged)
	if settled.Status.Applied == nil || settled.Status.Applied.PlanRef.UID != plan.UID {
		f.fatalf("schema result recovery did not account for the original approved plan")
	}
	f.waitForWatchCountAbove(name, "apply", 0, "the original schema Apply to enter the retained watch")
	if f.addedJobCount(name, "apply") != 1 {
		f.fatalf("schema result recovery replayed the Apply")
	}
	f.assertColumn(engine, database, "fault_token", 1)
	result := f.captureOneNewJobResult(name, "apply", beforeApply, nil)
	if f.captured.jobUID != string(active.JobUID) || f.captured.podUID != string(pod.UID) {
		f.fatalf("maximum-name schema recovery harvested another Apply or Pod")
	}
	f.check(automaticApplyResult(result, plan.Spec.ContentDigest, plan.Spec.CoordinationDigest, plan.Spec.TargetIdentityDigest), "read the maximum-name schema's original exact-plan result")
	f.poll("every maximum-name schema operation to complete with full labels", func() bool {
		f.captureSchemaSQLInventory(name, inventory)
		return resourceNameWorkloads(created.Namespace, "PtahSchema", name, created.UID, active.JobUID,
			slices.Collect(maps.Values(inventory.jobs)), slices.Collect(maps.Values(inventory.pods))) == nil
	})
	audit.assertRecords(beforeSQL, audit.snapshot(), audit.terminalPod(map[string]string{"job-name": active.JobName}, string(active.JobUID)), true)
	audit.close()
	if f.query(engine, database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='name-boundary' AND note='preserve-this-row'") != "1" ||
		f.query(engine, database, "SELECT count(*) FROM e2e_widgets") != "1" {
		f.fatalf("maximum-name schema recovery lost the preserved row")
	}
	f.logf("PASS %s 63-byte schema name: durable original and independent convergence within 180s while diagnostic log stays unfinished; no replay", engine)
	f.logf("PASS %s schema name boundary: 64 bytes refused at its CEL rule; every 63-byte operation retained its complete workload name; original Apply SQL and preserved data verified", engine)
}
