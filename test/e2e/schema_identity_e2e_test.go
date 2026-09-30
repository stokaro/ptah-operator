//go:build e2e

package e2e

import (
	"bytes"
	"maps"
	"reflect"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func (f *faultRun) schemaIdentityReplacement() {
	f.t.Helper()
	for _, engine := range []string{"postgresql", "mysql"} {
		f.replaceSchemaIdentity(engine)
	}
}

func (f *faultRun) replaceSchemaIdentity(engine string) {
	f.t.Helper()
	name, database := "e2e-schema-identity-"+engine, "e2e_schema_identity"
	secret, kind, reference := name+"-db", "PostgreSQL", f.pgReference
	if engine == "mysql" {
		kind, reference = "MySQL", f.mysqlReference
	}
	f.createDatabase(engine, database, secret)
	f.query(engine, database, "INSERT INTO e2e_widgets (id, name, note) VALUES (701, 'identity-control', 'preserved-identity-row')")
	beforeDatabase := f.fingerprint(engine, database, "the database before schema replacement")
	audit := &databaseSQLAudit{t: f.t, ctx: f.ctx, cluster: f.cluster, namespace: f.in.TestNamespace, engine: engine}
	var user string
	var pgBefore []byte
	var mysqlBefore []mysqlStatementRecord
	if engine == "mysql" {
		user = f.schemaMySQLAuditAccount(audit, database, secret)
		mysqlBefore = audit.mysqlStatementSnapshot()
	} else {
		audit.snapshot()
		pgBefore = audit.pgPrefix
	}
	inventory := &schemaSQLInventory{jobs: map[types.UID]batchv1.Job{}, pods: map[types.UID]corev1.Pod{}}
	waitForPlan := func() *ptahv1alpha1.PtahSchema {
		resource := f.waitForSchema(name, "the exact schema's approval gate", func(resource *ptahv1alpha1.PtahSchema) bool {
			f.captureSchemaSQLInventory(name, inventory)
			return planAwaitingApproval(resource) && resource.Status.ObservedGeneration == resource.Generation &&
				conditionStatus(resource.Status.Conditions, "ApprovalRequired", "True")
		})
		if !exactControllerPlan(resource.Status.Plan, resource.Status.ExecutionBinding, f.controller, f.stateVersion()) {
			f.fatalf("replacement plan has no exact controller identity")
		}
		f.check(readyPlanFromController(f.schemaPlan(resource.Status.Plan.Name), f.controller, f.stateVersion()), "read the ready replacement plan")
		return resource
	}
	fixture := faultSchema{name: name, engine: kind, reference: reference, secret: secret, coordinationKey: "e2e/schema-identity/" + engine}
	f.createSchema(fixture)
	old := waitForPlan()
	oldPlan := f.schemaPlan(old.Status.Plan.Name)
	beforeApply := f.checkpointOperationWatch(name, "apply", 0)
	f.pauseStatusWrites()
	f.createApproval(name, name+"-old")
	originalApproval := &ptahv1alpha1.PtahSchemaApproval{}
	f.check(f.get(name+"-old", originalApproval), "read the admitted original schema approval")
	if originalApproval.UID == "" || f.schema(name).Status.ActiveOperation != nil {
		f.fatalf("the original approval was not held before dispatch")
	}
	f.checkpointOperationWatch(name, "apply", 0)
	f.poll("every predecessor workload's complete credential audit before deletion", func() bool {
		f.auditRuntime()
		f.captureSchemaSQLInventory(name, inventory)
		complete, err := schemaRetirementAudited(old,
			slices.Collect(maps.Values(inventory.jobs)), slices.Collect(maps.Values(inventory.pods)),
			f.fullyAudited.holds, f.fullyAuditedPods)
		f.check(err, "validate the exact predecessor's credential audit inventory")
		return complete
	})
	f.check(f.cluster.Client.Delete(f.ctx, old, client.Preconditions{UID: &old.UID}, client.PropagationPolicy(metav1.DeletePropagationBackground)), "delete the exact approved schema")
	deleting := &ptahv1alpha1.PtahSchema{}
	err := f.get(name, deleting)
	if !apierrors.IsNotFound(err) && (err != nil || deleting.UID != old.UID || deleting.DeletionTimestamp == nil) {
		f.fatalf("the approved schema was not marked for deletion before claims resumed")
	}
	f.mustResumeStatusWrites("resume normal finalization of the approved schema")
	f.poll("the approved schema to disappear", func() bool {
		f.captureSchemaSQLInventory(name, inventory)
		return apierrors.IsNotFound(f.get(name, &ptahv1alpha1.PtahSchema{}))
	})
	f.createSchema(fixture)
	current := waitForPlan()
	fresh := f.schemaPlan(current.Status.Plan.Name)
	f.check(replacedSchemaDecision(old, current, oldPlan, fresh), "bind the unchanged schema work to new identities")
	retained := &ptahv1alpha1.PtahSchemaApproval{}
	f.check(f.get(originalApproval.Name, retained), "read the surviving old schema approval")
	if retained.UID != originalApproval.UID || !reflect.DeepEqual(retained.Spec, originalApproval.Spec) || retained.DeletionTimestamp != nil || conditionStatus(retained.Status.Conditions, "Consumed", "True") {
		f.fatalf("the old schema approval did not survive unchanged and unconsumed")
	}
	for _, row := range []struct{ name, resourceUID, planUID, fingerprint, reason string }{
		{"resource-uid", string(old.UID), string(fresh.UID), fresh.Spec.Fingerprint, "approval schema reference does not match the plan"},
		{"plan-uid", string(current.UID), string(oldPlan.UID), fresh.Spec.Fingerprint, "referenced plan UID does not match; the plan was replaced"},
		{"fingerprint", string(current.UID), string(fresh.UID), oldPlan.Spec.Fingerprint, "approval plan fingerprint does not match the immutable plan"},
	} {
		approvalName := name + "-wrong-" + row.name
		err := f.create(approvalDocument(f.in.TestNamespace, approvalName, name, row.resourceUID, fresh.Name, row.planUID, row.fingerprint))
		if err != nil {
			f.scan([]byte(err.Error()), "the schema identity refusal")
		}
		if !schemaIdentityApprovalRefusal(err, row.reason) {
			f.fatalf("%s approval did not fail at its binding guard: %v", row.name, err)
		}
		if !apierrors.IsNotFound(f.get(approvalName, &ptahv1alpha1.PtahSchemaApproval{})) {
			f.fatalf("a refused schema approval was stored")
		}
		f.logf("PASS %s same-name replacement refused approval %s at its binding guard", engine, row.name)
	}
	f.captureSchemaSQLInventory(name, inventory)
	f.checkpointOperationWatch(name, "apply", 0)
	clients, err := inventory.clients(current, old)
	f.check(err, "attribute SQL to both exact schema lifetimes")
	policy, err := newSchemaSQLPolicy(engine, database)
	f.check(err, "load the pinned schema diagnostic SQL contract")
	var counts map[string]int
	if engine == "postgresql" {
		audit.snapshot()
		if len(pgBefore) == 0 || !bytes.HasPrefix(audit.pgPrefix, pgBefore) {
			f.fatalf("schema SQL audit lost its original journal window")
		}
		counts, err = postgresStatementRefusalSQL(audit.pgPrefix[len(pgBefore):], database, clients, schemaDiagnosticActor, policy.postgres, nil)
	} else {
		counts, err = mysqlStatementRefusalSQL(mysqlBefore, audit.mysqlStatementSnapshot(), database, user, clients, true, schemaDiagnosticActor, policy.mysql)
	}
	f.check(err, "refuse every statement outside schema diagnostics during replacement")
	f.check(schemaReplacementSQLControls(clients, counts, string(old.UID), string(current.UID)), "observe diagnostics from both schema identities")
	hosts := make([]string, 0, len(counts))
	for host := range counts {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	for _, host := range hosts {
		actor := clients[host]
		f.logf("SQL refusal audit: engine=%s schemaUID=%s jobUID=%s podUID=%s operation=%s client=%s allowedDiagnosticRecords=%d unauthorizedRecords=0", engine, actor.resourceUID, actor.jobUID, actor.podUID, actor.operation, host, counts[host])
	}
	audit.close()
	if f.fingerprint(engine, database, "the database under the predecessor's approval") != beforeDatabase {
		f.fatalf("schema replacement changed the unapproved database")
	}
	f.assertColumn(engine, database, "fault_token", 0)
	f.assertSchemaIdentityRow(engine, database)
	assertSQL := f.approvedSchemaSQLControl(name, engine, currentPlan{name: fresh.Name, uid: string(fresh.UID), fingerprint: fresh.Spec.Fingerprint}, beforeApply)
	f.createApproval(name, name+"-current")
	f.waitForApprovedPlanConverged(name, fresh.Spec.ArtifactDigest, fresh.Spec.Fingerprint, string(fresh.UID), "the freshly approved replacement schema to converge")
	f.assertApprovalConsumed(name+"-current", string(fresh.UID))
	assertSQL()
	f.waitForWatchCountAbove(name, "apply", 0, "the replacement Apply to enter the watch")
	f.checkpointOperationWatch(name, "apply", 1)
	jobs := &batchv1.JobList{}
	f.check(f.list(jobs, client.MatchingLabels{labelSchema: name, labelOperation: "apply"}), "read the replacement Apply owner")
	if len(jobs.Items) != 1 || !ownedExactlyOnce(jobs.Items[0].OwnerReferences, ptahSchemaAPIVersion, "PtahSchema", current.Name, current.UID) {
		f.fatalf("the successful Apply does not belong to the replacement schema UID")
	}
	f.assertColumn(engine, database, "fault_token", 1)
	f.assertSchemaIdentityRow(engine, database)
	f.logf("PASS %s same-name schema replacement: oldUID=%s newUID=%s old approval refused; fresh approval applied once and preserved the row", engine, old.UID, current.UID)
}

func (f *faultRun) assertSchemaIdentityRow(engine, database string) {
	f.t.Helper()
	if f.query(engine, database, "SELECT count(*) FROM e2e_widgets") != "1" || f.query(engine, database, "SELECT count(*) FROM e2e_widgets WHERE id=701 AND name='identity-control' AND note='preserved-identity-row'") != "1" {
		f.fatalf("%s schema replacement changed the populated control row", engine)
	}
}
