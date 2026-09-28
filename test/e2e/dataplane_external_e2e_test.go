//go:build e2e

package e2e

import (
	"path/filepath"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The external PostgreSQL lifecycle: the automatic safe plan, the privileged
// plan and the grant-only plan held for a person under apply: Always.

var digestSelectedReference = regexp.MustCompile(`^oci://[^[:space:]@]+@sha256:[0-9a-f]{64}$`)

// externalPostgresqlLifecycle takes a schema selected by digest through an
// automatic safe plan against the database outside the cluster, holds the
// Jobs it ran to their generated-name bounds and the realm to one Lease, and
// then proves a privileged plan and a grant-only plan wait for a person under
// the same apply: Always.
func (d *dataPlane) externalPostgresqlLifecycle() {
	d.t.Helper()
	schema := externalPGSchema
	publishReference := d.registryReference("postgresql-external")
	realm, err := coordinationDigest("postgresql", d.in.TestNamespace, externalPGCoordinationKey)
	d.check(err, "derive the %s realm", schema)
	before := d.checkpointJobs(schema, "")
	digest := d.publishSchema("postgresql-external", "v1", "postgres", publishReference,
		filepath.Join(repositoryRoot, "testdata", "e2e", "postgresql-v1.sql"))
	reference := strings.TrimSuffix(publishReference, ":stable") + "@" + digest
	if !digestSelectedReference.MatchString(reference) {
		d.fatalf("external PostgreSQL acceptance did not select its OCI source by digest")
	}
	leases := d.checkpointCoordinationLeases()
	d.createSchemaResource(schemaResource{
		name: schema, engine: "PostgreSQL", secret: externalPGSecret, reference: reference,
		coordinationKey: externalPGCoordinationKey, failureRetry: "45s", interval: quiescentInterval, apply: "Always",
	})
	observed := d.assertAutomaticExternalPostgresqlLifecycle(schema, externalPGSecret, reference, digest,
		externalPGCoordinationKey, realm, before)
	// The schema's name is long enough that the plan Job's generated name is
	// cut at the bound, and the Pod's generateName and name cross it: the
	// captured Plan Job is where that boundary is measured.
	if len(d.captured.jobName) != 58 {
		d.fatalf("external PostgreSQL plan Job did not reach the generated-name truncation boundary")
	}
	if len(d.captured.podGenerateName) != 59 {
		d.fatalf("external PostgreSQL plan Pod generateName did not cross the truncation boundary")
	}
	if len(d.captured.podName) != 63 {
		d.fatalf("external PostgreSQL plan Pod did not preserve the bounded generated name")
	}
	d.assertCoordinationLeaseBoundary(externalPGCoordinationKey, leases)
	d.assertExternalPostgresqlCatalog()
	d.suspend(schema, true)
	d.waitForSchema(schema, "external PostgreSQL acceptance to suspend after exact convergence", quiescentlySuspended)
	d.recordObservedJobs()
	d.assertSchemaJobBoundaryUnchanged(schema, before, observed, 7)
	d.assertExternalPostgresqlCatalog()
	d.auditRuntimeCredentials()
	d.assertPrivilegedPlanWaitsUnderAlways(schema, publishReference, externalPGCoordinationKey, realm)
	d.assertGrantOnlyChangePlansUnderAlways(schema, publishReference, externalPGCoordinationKey, realm)
	d.logf("PASS external PostgreSQL bridge lifecycle")
}

// assertAutomaticExternalPostgresqlLifecycle waits for the schema to apply its
// safe plan with no person involved and converge, and holds every step of that
// to the evidence it left: the final schema, the seven Jobs of one serialized
// lifecycle read from their kept evidence, each Job's result, the immutable
// plan it applied and the document its chunks hold, the Apply Job and Pod
// bound to that plan, and no approval object or approval transition anywhere.
// It returns the seven Job UIDs, which the lifecycle holds the schema to once
// it is suspended.
func (d *dataPlane) assertAutomaticExternalPostgresqlLifecycle(schemaName, secret, reference, digest, key, realm string,
	before checkpoint,
) []string {
	d.t.Helper()
	final := d.waitForSchema(schemaName, "automatic safe-plan application and independent convergence",
		func(s *ptahv1alpha1.PtahSchema) bool {
			return s.Status.Plan == nil && inSync(s, digest)
		})
	d.scanObject(final, "the automatic-policy final PtahSchema")
	want := automaticExpectation{
		reference: reference, digest: digest, coordinationKey: key, coordinationDigest: realm,
		controller: d.controller, stateVersion: d.stateVersion(), runnerImage: d.in.RunnerImage,
	}
	if err := automaticConvergenceExact(final, want); err != nil {
		d.fatalf("%s did not retain exact automatic-policy convergence evidence: %v", schemaName, err)
	}
	target := final.Status.Target

	d.auditCompletedJobs()
	d.recordObservedJobs()
	observed, jobs := d.archivedSchemaJobs(schemaName, before, 7)
	sequence, err := automaticJobSequence(jobs, before, observed, schemaName, final.UID)
	if err != nil {
		d.fatalf("%s did not preserve one exact serialized automatic-policy Job lifecycle: %v", schemaName, err)
	}
	resolveUID, verifyUID, initialObserveUID, initialPlanUID := sequence[0], sequence[1], sequence[2], sequence[3]
	applyUID, finalObserveUID, finalPlanUID := sequence[4], sequence[5], sequence[6]

	if err := automaticResolveResult(d.captureSelectedJobResult(schemaName, "resolve", resolveUID), reference, digest); err != nil {
		d.fatalf("%s Resolve result did not bind the digest-selected OCI source: %v", schemaName, err)
	}
	if err := automaticVerifyResult(d.captureSelectedJobResult(schemaName, "verify", verifyUID), digest,
		final.Status.Source.VerificationPolicyDigest); err != nil {
		d.fatalf("%s Verify result did not precede and authorize database access: %v", schemaName, err)
	}
	if err := automaticInitialObserveResult(d.captureSelectedJobResult(schemaName, "observe", initialObserveUID),
		realm, target.IdentityDigest); err != nil {
		d.fatalf("%s initial Observe result did not prove real PostgreSQL drift: %v", schemaName, err)
	}
	initialPlan := d.captureSelectedJobResult(schemaName, "plan", initialPlanUID)
	initialPlanCapture := d.captured
	if err := automaticInitialPlanResult(initialPlan, realm, target.IdentityDigest); err != nil {
		d.fatalf("%s initial Plan result did not describe a safe database change: %v", schemaName, err)
	}
	// The result's stdout is the plan sealed to the manager's key. The
	// document is rebuilt below from the applied plan's own chunks, once that
	// plan is found, and only then read for what it changed.

	plans := &ptahv1alpha1.PtahSchemaPlanList{}
	d.mustList(plans)
	var applied []ptahv1alpha1.PtahSchemaPlan
	for _, plan := range plans.Items {
		if plan.Spec.SchemaRef.Name == schemaName && plan.Spec.SchemaRef.UID == final.UID &&
			plan.Spec.Fingerprint == final.Status.Applied.PlanFingerprint {
			applied = append(applied, plan)
		}
	}
	if len(applied) != 1 {
		d.fatalf("%s did not retain one immutable automatically applied plan", schemaName)
	}
	plan := &applied[0]
	d.scanObject(plan, "the automatic-policy immutable plan resource")
	document, chunks := d.rebuildPlanDocument(plan)
	d.scan(document, "the automatic-policy native PostgreSQL plan")
	contentDigest := sha256Digest(document)
	if contentDigest != initialPlan.PlanContentDigest {
		d.fatalf("%s automatic Plan result content digest does not cover the plan document its chunks hold", schemaName)
	}
	parsed, err := parsePlanDocument(document)
	d.check(err, "%s automatic plan document", schemaName)
	if err := automaticPlanDocument(parsed); err != nil {
		d.fatalf("%s automatic plan was not an exact safe additive CREATE TABLE change: %v", schemaName, err)
	}
	if err := automaticPlanBound(plan, final, parsed, contentDigest, want); err != nil {
		d.fatalf("%s automatically applied plan lost its exact immutable bindings: %v", schemaName, err)
	}
	d.assertPlanStorageImmutable(schemaName, plan.Name, string(plan.UID))
	d.assertPlanProjected(plan)
	if err := sealedPayloadLeak(initialPlan.Stdout, document); err != nil {
		d.fatalf("%s automatic %v", schemaName, err)
	}
	d.logf("%s automatic Plan result is sealed, and its content digest covers the %d-chunk plan document", schemaName, chunks)

	applyResult := d.captureSelectedJobResult(schemaName, "apply", applyUID)
	if d.captured.jobUID != applyUID {
		d.fatalf("%s captured Apply Job UID changed before workload validation", schemaName)
	}
	applyJob, applyPod := d.captured.evidence.job, d.captured.evidence.pod
	d.scanObject(applyJob, "the automatic-policy Apply Job")
	d.scanObject(applyPod, "the automatic-policy Apply Pod")
	if err := automaticApplyWorkload(applyJob, applyPod, applyWorkload{
		schema: schemaName, jobName: d.captured.jobName, jobUID: applyUID,
		podName: d.captured.podName, podUID: d.captured.podUID,
		planFingerprint: final.Status.Applied.PlanFingerprint, contentDigest: contentDigest,
		executionBinding: plan.Spec.ExecutionBindingID, executorImage: d.in.ExecutorImage, runnerImage: d.in.RunnerImage,
	}); err != nil {
		d.fatalf("%s Apply Job and Pod lost their immutable plan or execution binding: %v", schemaName, err)
	}
	if err := automaticApplyResult(applyResult, contentDigest, realm, target.IdentityDigest); err != nil {
		d.fatalf("%s automatic Apply result did not execute the exact safe plan: %v", schemaName, err)
	}
	if err := automaticFinalObserveResult(d.captureSelectedJobResult(schemaName, "observe", finalObserveUID),
		realm, target.IdentityDigest, target.DriftReportDigest); err != nil {
		d.fatalf("%s post-Apply Observe result did not prove convergence: %v", schemaName, err)
	}
	if err := automaticFinalPlanResult(d.captureSelectedJobResult(schemaName, "plan", finalPlanUID),
		realm, target.IdentityDigest); err != nil {
		d.fatalf("%s post-Apply Plan result did not independently prove no changes: %v", schemaName, err)
	}

	approvals := &ptahv1alpha1.PtahSchemaApprovalList{}
	d.mustList(approvals)
	if approvalsFor(approvals.Items, schemaName, final.UID) != 0 {
		d.fatalf("%s automatic policy unexpectedly relied on an approval object", schemaName)
	}
	events := &corev1.EventList{}
	d.mustList(events)
	if approvalTransitions(events.Items, schemaName, final.UID) != 0 {
		d.fatalf("%s automatic policy emitted an approval transition", schemaName)
	}

	// The lifecycle measures the generated-name bounds on the initial Plan
	// Job, so that is the Job the capture names when this returns.
	d.captured = initialPlanCapture
	d.assertJobIsolation(schemaName, secret, true, jobs)
	d.logf("PASS automatic safe-plan PostgreSQL lifecycle")
	return observed
}

// assertExternalPostgresqlCatalog holds the external database to exactly the
// v1 schema, owned by the login Ptah is given, which is no superuser, on a
// container that is still the disposable database outside the cluster.
func (d *dataPlane) assertExternalPostgresqlCatalog() {
	d.t.Helper()
	d.assertExternalPGNotHostedInKubernetes()
	d.assertExternalPGContainerContract()
	d.assertExternalPGServerVersion()
	columns := removeLineBreaks(d.externalPGQuery(
		"SELECT string_agg(column_name || ':' || data_type || ':' || is_nullable, ',' ORDER BY ordinal_position) " +
			"FROM information_schema.columns WHERE table_schema='public' AND table_name='e2e_widgets'"))
	if columns != externalColumnsV1 {
		d.fatalf("external PostgreSQL columns are %s, expected the exact v1 schema", columns)
	}
	primaryKey := removeWhitespace(d.externalPGQuery(
		"SELECT count(*) FROM information_schema.table_constraints tc JOIN information_schema.key_column_usage kcu " +
			"USING (constraint_catalog, constraint_schema, constraint_name, table_catalog, table_schema, table_name) " +
			"WHERE tc.table_schema='public' AND tc.table_name='e2e_widgets' AND tc.constraint_type='PRIMARY KEY' " +
			"AND kcu.column_name='id' AND kcu.ordinal_position=1"))
	if primaryKey != "1" {
		d.fatalf("external PostgreSQL v1 primary key is not exact")
	}
	if superuser := removeWhitespace(d.externalPGQuery("SELECT rolsuper FROM pg_roles WHERE rolname = current_user")); superuser != "f" {
		d.fatalf("external PostgreSQL fixture login regained superuser authority")
	}
	if owns := removeWhitespace(d.externalPGQuery(
		"SELECT pg_get_userbyid(datdba) = current_user FROM pg_database WHERE datname = current_database()")); owns != "t" {
		d.fatalf("external PostgreSQL fixture login lost database ownership")
	}
}

// externalPrivilegedFunctionCount counts the definer function the privileged
// artifact adds, as the database answers: owned by the login Ptah is given,
// with definer rights.
func (d *dataPlane) externalPrivilegedFunctionCount() string {
	d.t.Helper()
	return removeWhitespace(d.externalPGQuery(
		"SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'public' " +
			"AND p.proname = 'e2e_widget_count' AND p.pronargs = 0 AND p.prosecdef AND pg_get_userbyid(p.proowner) = current_user"))
}

// externalPublicSelectGrant is t when PUBLIC may read the fixture table and f
// when it may not, as the database answers.
func (d *dataPlane) externalPublicSelectGrant() string {
	d.t.Helper()
	return removeWhitespace(d.externalPGQuery("SELECT has_table_privilege('public', 'public.e2e_widgets', 'SELECT')"))
}

// holdMatch is what a gate wait matched: the filter and the narrowing the row
// asks for.
type holdMatch func(*ptahv1alpha1.PtahSchema) bool

// waitForHeldPlan polls until the schema holds its plan for a person as gate
// says, and narrow when it is given, and returns the reading that matched:
// every claim about the gate is made against that document rather than a
// later read. The Apply checkpoint is read on every poll, so an Apply that
// starts while the gate is awaited fails the row where it happens rather than
// when it is next looked at.
func (d *dataPlane) waitForHeldPlan(schema string, applyCheckpoint checkpoint, gate, narrow holdMatch,
	context string, messages heldPlanMessages, reading func(*ptahv1alpha1.PtahSchema) string,
) *ptahv1alpha1.PtahSchema {
	d.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	var last *ptahv1alpha1.PtahSchema
	for time.Now().Before(deadline) {
		d.auditCompletedJobs()
		d.assertNoNewJobs(schema, "apply", applyCheckpoint)
		candidate := &ptahv1alpha1.PtahSchema{}
		if err := d.get(schema, candidate); err != nil {
			last = nil
		} else {
			last = candidate
			if gate(candidate) && (narrow == nil || narrow(candidate)) {
				d.scanObject(candidate, "the "+schema+" "+context)
				return candidate
			}
			if failedForCurrentSpec(candidate) {
				d.fatalf("%s entered Failed while waiting for %s", schema, messages.failedWhile)
			}
		}
		d.sleep(2 * time.Second)
	}
	d.fatalf("timed out waiting for %s%s; last reading: %s", schema, messages.timedOut, reading(last))
	return nil
}

// heldPlanMessages are what a gate wait says when the schema failed while it
// waited, and, after the schema's name, when it gave up.
type heldPlanMessages struct {
	failedWhile, timedOut string
}

// waitForPrivilegedGate is waitForHeldPlan for a plan that changes privileges.
func (d *dataPlane) waitForPrivilegedGate(schema, digest string, applyCheckpoint checkpoint, description string,
	narrow holdMatch,
) *ptahv1alpha1.PtahSchema {
	d.t.Helper()
	gate := func(s *ptahv1alpha1.PtahSchema) bool { return privilegedApprovalGate(s, digest) }
	return d.waitForHeldPlan(schema, applyCheckpoint, gate, narrow, "privileged approval gate",
		heldPlanMessages{failedWhile: description, timedOut: ": " + description}, privilegedGateReading)
}

// latestCompletionBetween is the latest completion time among the schema's
// Jobs of one operation that appeared between two checkpoints, read from their
// kept evidence: a completed Job may already be gone from the API.
func (d *dataPlane) latestCompletionBetween(schema, operation string, before, after checkpoint) time.Time {
	d.t.Helper()
	d.auditCompletedJobs()
	d.recordObservedJobs()
	var jobs []*batchv1.Job
	for _, record := range d.observed.between(schema, operation, before, after) {
		jobs = append(jobs, d.completedEvidence(schema, operation, record.UID).job)
	}
	if len(jobs) == 0 {
		d.fatalf("%s has no completed %s Job between the checkpoints", schema, operation)
	}
	latest, err := latestCompletion(jobs)
	if err != nil {
		d.fatalf("%s %s %v", schema, operation, err)
	}
	return latest
}

// assertAlwaysPolicyKept holds the schema, after a row's spec patch, to apply
// Always with destructive changes refused, running at the interval given. The
// stored spec is read, because the claim is that the patch left the refusal
// written down rather than defaulted away.
func (d *dataPlane) assertAlwaysPolicyKept(schema, interval string) {
	d.t.Helper()
	stored := d.unstructuredSchema(schema).Object
	apply, _, _ := unstructured.NestedString(stored, "spec", "policy", "apply")
	allowDestructive, allowFound, _ := unstructured.NestedBool(stored, "spec", "policy", "allowDestructive")
	suspended, suspendFound, _ := unstructured.NestedBool(stored, "spec", "suspend")
	storedInterval, _, _ := unstructured.NestedString(stored, "spec", "interval")
	if apply != "Always" || !allowFound || allowDestructive || !suspendFound || suspended || storedInterval != interval {
		d.fatalf("%s did not keep apply Always and allowDestructive false", schema)
	}
}

// assertPrivilegedPlanWaitsUnderAlways proves a plan that changes privileges
// waits for a person under apply: Always.
//
// The artifact adds one SECURITY DEFINER function to the converged external
// schema. Ptah rates the statement safe and the plan not destructive, so on a
// resource set to Always with allowDestructive false nothing but the privilege
// class stands between the plan and an unattended apply. The row holds the
// class to what it is for: the plan and the schema name the kinds, and the
// condition says why the resource waits; it waits through its whole persisted
// refresh deadline and the refresh after it with no Apply Job and the function
// absent from the database, dated by the Jobs' own timestamps; and one exact
// approval then applies it, and the function exists with definer rights.
//
// It starts from the suspended, converged resource the lifecycle leaves, and
// leaves it suspended again.
func (d *dataPlane) assertPrivilegedPlanWaitsUnderAlways(schema, publishReference, key, realm string) {
	d.t.Helper()
	const approval = "e2e-postgresql-external-privileged"
	if d.rbac.paused {
		d.fatalf("the privileged approval gate requires active controller status writes")
	}
	if d.externalPrivilegedFunctionCount() != "0" {
		d.fatalf("external PostgreSQL holds the definer function before any plan created it")
	}

	digest := d.publishSchema("postgresql-external", "v2", "postgres", publishReference,
		filepath.Join(repositoryRoot, "testdata", "e2e", "postgresql-external-v2-definer.sql"))
	reference := strings.TrimSuffix(publishReference, ":stable") + "@" + digest
	before := d.checkpointJobs(schema, "")
	d.patchSchema(schema, map[string]any{"spec": map[string]any{
		"suspend": false, "interval": blockedRefreshInterval, "desired": map[string]any{"ociRef": reference},
	}})
	d.assertAlwaysPolicyKept(schema, blockedRefreshInterval)

	gate := d.waitForPrivilegedGate(schema, digest, before, "the privileged plan held for a person", nil)
	gateCheckpoint := d.checkpointJobs(schema, "")
	if gate.Status.Plan == nil || gate.Status.NextReconciliationTime == nil {
		d.fatalf("%s held no plan or no refresh deadline at its privileged gate", schema)
	}
	schemaUID := gate.UID
	planName, planUID := gate.Status.Plan.Name, string(gate.Status.Plan.UID)
	fingerprint, deadline := gate.Status.Plan.Fingerprint, gate.Status.NextReconciliationTime.Time
	if err := privilegedPlanRecorded(d.schemaPlan(planName), planUID, fingerprint, digest,
		[]ptahv1alpha1.PrivilegeChange{"SecurityDefiner"}); err != nil {
		d.fatalf("%s does not record the kinds its statement changes: %v", planName, err)
	}
	if d.externalPrivilegedFunctionCount() != "0" {
		d.fatalf("the definer function reached the database while its plan waited for a person")
	}

	// The hold, measured rather than waited out: the resource must reach its
	// persisted deadline, refresh, and publish the same plan for a person
	// again, with the Apply checkpoint read on every poll in between.
	d.waitForPrivilegedGate(schema, digest, before, "the same plan held again after its refresh deadline",
		samePlanHeldAfter(planUID, fingerprint, deadline))
	heldCheckpoint := d.checkpointJobs(schema, "")
	planCompleted := d.latestCompletionBetween(schema, "plan", before, gateCheckpoint)
	refreshes := d.observed.between(schema, "resolve", gateCheckpoint, heldCheckpoint)
	if err := privilegedHoldMeasured(refreshes, deadline, planCompleted, blockedRefreshSeconds*time.Second); err != nil {
		d.fatalf("%s did not hold its privileged plan for its persisted deadline %s before refreshing: %v",
			schema, deadline.UTC().Format(time.RFC3339), err)
	}
	d.assertNoJobBetween(schema, "apply", before, heldCheckpoint)
	if d.externalPrivilegedFunctionCount() != "0" {
		d.fatalf("the definer function reached the database during the held refresh interval")
	}
	events := &corev1.EventList{}
	d.mustList(events)
	if privilegeEvents(events.Items, schemaUID, "SecurityDefiner") < 1 {
		d.fatalf("%s emitted no ApprovalRequired Event naming the kinds", schema)
	}

	// A quiet cadence before the approval, so no refresh can land between the
	// approval and its Apply and spend it on a reading.
	d.patchSchema(schema, map[string]any{"spec": map[string]any{"interval": quiescentInterval}})
	quietGeneration := d.schema(schema).Generation
	d.waitForPrivilegedGate(schema, digest, before, "the same plan held at the quiet cadence",
		samePlanHeldAtGeneration(planUID, fingerprint, quietGeneration))

	applyCheckpoint := d.checkpointJobs(schema, "")
	d.assertNoJobBetween(schema, "apply", before, applyCheckpoint)
	d.createExactApproval(schema, planName, approval, key, realm)
	d.waitForApprovedPlanConverged(schema, digest, fingerprint, planUID, "the approved privileged plan applied and converged")
	after := d.checkpointJobs(schema, "")
	d.assertOneJobBetween(schema, "apply", applyCheckpoint, after)
	d.assertApprovalConsumed(approval, planUID)
	if d.externalPrivilegedFunctionCount() != "1" {
		d.fatalf("the approved plan did not leave the definer function in external PostgreSQL")
	}
	d.assertExternalPostgresqlCatalog()

	d.suspend(schema, true)
	d.waitForSchema(schema, "external PostgreSQL acceptance to suspend after the privileged plan", quiescentlySuspended)
	d.auditRuntimeCredentials()
	d.logf("PASS privileged plan held for a person under apply Always")
}

// waitForApprovedPlanConverged waits for the schema to apply the approved plan
// and converge on the digest at its current generation, with nothing left
// planned, pending or claimed.
func (d *dataPlane) waitForApprovedPlanConverged(schema, digest, fingerprint, planUID, description string) {
	d.t.Helper()
	d.waitForSchema(schema, description, func(s *ptahv1alpha1.PtahSchema) bool {
		status := s.Status
		return status.ObservedGeneration == s.Generation && status.Source.Digest == digest &&
			status.Applied != nil && status.Applied.ArtifactDigest == digest &&
			status.Applied.PlanFingerprint == fingerprint && string(status.Applied.PlanRef.UID) == planUID &&
			status.Plan == nil && status.ActiveOperation == nil && status.PendingObservation == nil &&
			status.PendingLockRelease == nil &&
			conditionIs(status.Conditions, "InSync", "True", "ScopedConverged")
	})
}

// assertGrantOnlyChangePlansUnderAlways proves a change that touches only a
// grant is observed, planned, and held for a person under apply: Always.
//
// Ptah's drift report has no category for a grant: it says drift and lists no
// finding. The runner used to refuse that report, so the resource never left
// Observe and no grant-only change could reach a plan under any policy. The
// artifact adds one GRANT to PUBLIC to the converged external schema, which
// Ptah plans as one statement it rates safe. The row holds the path to what it
// is for: the observation is recorded as drift in no category, and the scoped
// plan that follows names Grant and nothing else; under Always the plan waits
// for a person, with no Apply and no grant in the database while it waits; and
// one exact approval applies it, the resource converges, and PUBLIC may read
// the table.
//
// It starts from the suspended, converged resource the privileged row leaves,
// and leaves it suspended again.
func (d *dataPlane) assertGrantOnlyChangePlansUnderAlways(schema, publishReference, key, realm string) {
	d.t.Helper()
	const approval = "e2e-postgresql-external-grant"
	if d.rbac.paused {
		d.fatalf("the grant-only row requires active controller status writes")
	}
	if d.externalPublicSelectGrant() != "f" {
		d.fatalf("external PostgreSQL grants PUBLIC SELECT on e2e_widgets before any plan did")
	}

	digest := d.publishSchema("postgresql-external", "v3", "postgres", publishReference,
		filepath.Join(repositoryRoot, "testdata", "e2e", "postgresql-external-v3-grant.sql"))
	reference := strings.TrimSuffix(publishReference, ":stable") + "@" + digest
	before := d.checkpointJobs(schema, "")
	d.patchSchema(schema, map[string]any{"spec": map[string]any{
		"suspend": false, "interval": quiescentInterval, "desired": map[string]any{"ociRef": reference},
	}})
	d.assertAlwaysPolicyKept(schema, quiescentInterval)

	gate := d.waitForHeldPlan(schema, before,
		func(s *ptahv1alpha1.PtahSchema) bool { return grantOnlyApprovalGate(s, digest) }, nil,
		"grant-only approval gate", heldPlanMessages{
			failedWhile: "its grant-only plan",
			timedOut:    " to observe its grant-only change as drift in no category, plan Grant and hold it for a person",
		}, grantGateReading)
	if gate.Status.Plan == nil {
		d.fatalf("%s held no plan at its grant-only gate", schema)
	}
	planName, planUID, fingerprint := gate.Status.Plan.Name, string(gate.Status.Plan.UID), gate.Status.Plan.Fingerprint
	if err := privilegedPlanRecorded(d.schemaPlan(planName), planUID, fingerprint, digest,
		[]ptahv1alpha1.PrivilegeChange{"Grant"}); err != nil {
		d.fatalf("%s does not record the Grant its statement changes: %v", planName, err)
	}
	if d.externalPublicSelectGrant() != "f" {
		d.fatalf("the grant reached the database while its plan waited for a person")
	}

	applyCheckpoint := d.checkpointJobs(schema, "")
	d.assertNoJobBetween(schema, "apply", before, applyCheckpoint)
	d.createExactApproval(schema, planName, approval, key, realm)
	d.waitForApprovedPlanConverged(schema, digest, fingerprint, planUID, "the approved grant-only plan applied and converged")
	after := d.checkpointJobs(schema, "")
	d.assertOneJobBetween(schema, "apply", applyCheckpoint, after)
	d.assertApprovalConsumed(approval, planUID)
	if d.externalPublicSelectGrant() != "t" {
		d.fatalf("the approved plan did not leave PUBLIC SELECT on e2e_widgets in external PostgreSQL")
	}
	d.assertExternalPostgresqlCatalog()

	d.suspend(schema, true)
	d.waitForSchema(schema, "external PostgreSQL acceptance to suspend after the grant-only plan", quiescentlySuspended)
	d.auditRuntimeCredentials()
	d.logf("PASS grant-only change observed, planned and held for a person under apply Always")
}
