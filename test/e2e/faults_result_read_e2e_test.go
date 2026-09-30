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
	lease := f.loadHeldLeaseForEpoch(active.LeaseEpoch, "the unread schema Apply's Lease")
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
	// The credential audit also reads Pod logs. While this response is held,
	// only the controller may request it; the audit resumes after restoration.
	f.pollQuiet("a completed schema Apply and an unfinished result read", func() bool {
		job := &batchv1.Job{}
		f.check(f.get(active.JobName, job), "read the schema Apply Job")
		return job.UID == active.JobUID && jobComplete(job) && slices.ContainsFunc(fault.readings(),
			func(reading logStallReading) bool { return reading.State == "started" })
	})
	f.createApproval(healthy, healthy+"-approval")
	approval := &ptahv1alpha1.PtahSchemaApproval{}
	f.check(f.get(healthy+"-approval", approval), "read the independent schema approval timestamp")
	deadline := approval.CreationTimestamp.Add(180 * time.Second)
	timedOut, progressed := false, false
	for time.Now().Before(deadline) {
		f.captureSchemaSQLInventory(name, inventory)
		current := f.schema(name)
		if !activeIdentityKept(current, active.ID, active.JobName, string(active.JobUID)) || current.Status.ActiveOperation.LeaseEpoch != active.LeaseEpoch {
			f.fatalf("%s discarded its Apply claim while its result was unread", name)
		}
		f.assertLeaseIdentity(lease)
		if f.addedJobCount(name, "apply") > 1 {
			f.fatalf("%s replayed the Apply while its result was unread", name)
		}
		events := &corev1.EventList{}
		f.check(f.list(events), "read schema result-read timeout Events")
		timedOut = timedOut || slices.ContainsFunc(events.Items, func(event corev1.Event) bool {
			return event.InvolvedObject.UID == claimed.UID && event.Reason == "ResultReadTimedOut"
		})
		progressed = schemaReadProgress(f.schema(healthy), approval.CreationTimestamp.Time, deadline)
		if timedOut && progressed {
			break
		}
		f.sleep(2 * time.Second)
	}
	if !timedOut || !progressed {
		f.fatalf("hung schema result read did not release the worker within 180s: timeout=%t independent convergence=%t", timedOut, progressed)
	}
	duration, bounded := logReadDuration(fault.readings())
	if !bounded {
		f.fatalf("the unfinished schema result was not canceled inside its 75s acceptance bound: %s", duration)
	}
	establishBarrierWithPoll(f, f.leases, &coordinationv1.Lease{}, f.in.OperatorNamespace, lease.name, f.pollQuiet)
	f.assertLeaseIdentity(lease)
	if !leaseHeldWithoutRelease(f.leases.snapshot(), lease.uid, lease.holder, lease.epoch) {
		f.fatalf("the unread schema Apply's Lease was released or replaced")
	}
	for _, db := range []string{database, healthyDB} {
		f.assertColumn(engine, db, "fault_token", 1)
	}
	f.check(fault.stop(f.ctx), "restore schema result reads")
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
	f.logf("PASS %s 63-byte schema name: result read canceled in %s; independent convergence within 180s; original claim and Lease retained; recovery without replay", engine, duration)
	f.logf("PASS %s schema name boundary: 64 bytes refused at its CEL rule; every 63-byte operation retained its complete workload name; original Apply SQL and preserved data verified", engine)
}
