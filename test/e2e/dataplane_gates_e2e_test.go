//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The destructive gate, the blocked refresh cadence, the digest-pin refusal
// and the registry outage.

// schemaAsStored reads a schema once and returns it both as the typed object
// and as the document the API stored, for the proofs that hold a field to the
// string the phase wrote rather than to what it means.
func (d *dataPlane) schemaAsStored(name string) (*ptahv1alpha1.PtahSchema, map[string]any, error) {
	object := &unstructured.Unstructured{}
	object.SetAPIVersion(ptahSchemaAPIVersion)
	object.SetKind("PtahSchema")
	if err := d.get(name, object); err != nil {
		return nil, nil, err
	}
	schema := &ptahv1alpha1.PtahSchema{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, schema); err != nil {
		return nil, nil, err
	}
	return schema, object.Object, nil
}

func (d *dataPlane) mustSchemaAsStored(name string) (*ptahv1alpha1.PtahSchema, map[string]any) {
	d.t.Helper()
	schema, document, err := d.schemaAsStored(name)
	d.check(err, "read PtahSchema %s", name)
	return schema, document
}

func storedSpecOf(document map[string]any) storedSpec {
	stored := storedSpec{}
	stored.interval, _, _ = unstructured.NestedString(document, "spec", "interval")
	if suspend, found, err := unstructured.NestedBool(document, "spec", "suspend"); found && err == nil {
		stored.suspend = &suspend
	}
	return stored
}

// prepareBlockedRefreshCadence suspends the blocked schema at the blocked
// refresh interval, resumes it, and captures the boundary its scheduled
// refreshes are counted from, leaving it in d.blockedGate.
func (d *dataPlane) prepareBlockedRefreshCadence(schema string) {
	d.t.Helper()
	d.suspendSchemaForTagMove(schema, blockedRefreshInterval)
	generationCheckpoint := d.checkpointJobs(schema, "")
	d.captureBlockedRefreshBoundary(schema, generationCheckpoint)
	d.auditRuntimeCredentials()
}

// captureBlockedRefreshBoundary resumes the schema and waits for the one
// read-only chain its generation change starts to settle Blocked with a
// persisted deadline far enough ahead, then checkpoints its Jobs at once and
// reads the deadline back unchanged. Nothing between the reading and the
// checkpoint audits or waits, so the checkpoint cannot straddle the next
// scheduled refresh the gate is about to count.
func (d *dataPlane) captureBlockedRefreshBoundary(schema string, generationCheckpoint checkpoint) {
	d.t.Helper()
	captureHeadroom, postHeadroom := blockedRefreshHeadroom(blockedRefreshSeconds)
	if postHeadroom <= 0 {
		d.fatalf("blocked refresh boundary requires positive post-checkpoint headroom")
	}
	d.resumeSchemaAfterTagMove(schema)
	generation := d.schema(schema).Generation
	if generation <= 0 {
		d.fatalf("%s resumed without an exact positive generation", schema)
	}

	var deadline *metav1.Time
	stateDeadline := time.Now().Add(waitTimeout)
	for time.Now().Before(stateDeadline) {
		if candidate, document, err := d.schemaAsStored(schema); err == nil {
			if blockedRefreshBoundary(candidate, storedSpecOf(document), generation, time.Now(), captureHeadroom) {
				deadline = candidate.Status.NextReconciliationTime
				break
			}
			if failedForCurrentSpec(candidate) {
				d.fatalf("%s entered Failed before the blocked refresh boundary", schema)
			}
		}
		d.sleep(time.Second)
	}
	if deadline == nil {
		d.fatalf("timed out waiting for %s blocked refresh boundary with future headroom", schema)
	}

	d.blockedGate = d.checkpointJobs(schema, "")
	d.assertReadOnlyCycleBetween(schema, generationCheckpoint, d.blockedGate)
	stable, document := d.mustSchemaAsStored(schema)
	if !blockedBoundaryStable(stable, storedSpecOf(document), generation, *deadline, time.Now(), postHeadroom) {
		d.fatalf("%s crossed or changed its persisted blocked refresh boundary during capture", schema)
	}
}

// reportBlockedRefreshDiagnostics prints the order the schema's Jobs appeared
// in since the checkpoint, projected to names, UIDs, operations and creation
// times and scanned before it is printed, so a failed count says which chain
// started early and which never ran.
func (d *dataPlane) reportBlockedRefreshDiagnostics(schema string, before checkpoint) {
	if len(d.observed.records) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, cleanupSuppressed)
		return
	}
	emitScanned(os.Stderr, d.scanner, timelineSince(d.observed.since(schema, "", before)))
}

// assertDestructiveGate holds a schema whose destructive plan the policy
// refuses to refreshing on its schedule and doing nothing else: exactly three
// complete read-only chains after the refresh checkpoint, each started at
// least one interval after the last, never an Apply, and the same destructive
// plan throughout.
func (d *dataPlane) assertDestructiveGate(schema string, applyCheckpoint checkpoint, plan currentPlan, digest string,
	refresh checkpoint,
) {
	d.t.Helper()
	if d.schemaInterval(schema) != blockedRefreshInterval {
		d.fatalf("%s destructive gate requires interval %s", schema, blockedRefreshInterval)
	}
	if plan.name == "" || plan.uid == "" || plan.fingerprint == "" || digest == "" {
		d.fatalf("%s destructive gate lacks its immutable starting evidence", schema)
	}
	if refresh == nil {
		d.fatalf("%s destructive gate lacks its atomic refresh checkpoint", schema)
	}
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		d.auditCompletedJobs()
		// The audit above reads the ledger before it walks every terminal
		// Job, and walking them takes longer than the refresh cadence.
		// Refresh the ledger here, before the counts below.
		d.recordObservedJobs()
		d.assertNoNewJobs(schema, "apply", applyCheckpoint)
		counts := map[string]int{}
		for _, operation := range []string{"resolve", "verify", "observe", "plan"} {
			counts[operation] = d.newJobCountSince(schema, operation, refresh)
		}
		if counts["resolve"] > 3 || counts["verify"] > 3 || counts["observe"] > 3 || counts["plan"] > 3 {
			d.reportBlockedRefreshDiagnostics(schema, refresh)
			d.fatalf("%s created work beyond three exact blocked refresh chains", schema)
		}
		if counts["resolve"] == 3 && counts["verify"] == 3 && counts["observe"] == 3 && counts["plan"] == 3 &&
			d.allNewJobsComplete(schema, "resolve", refresh, 3) &&
			d.allNewJobsComplete(schema, "verify", refresh, 3) &&
			d.allNewJobsComplete(schema, "observe", refresh, 3) &&
			d.allNewJobsComplete(schema, "plan", refresh, 3) {
			if err := orderedRefreshChains(d.observed.since(schema, "", refresh), blockedRefreshSeconds); err != nil {
				d.reportBlockedRefreshDiagnostics(schema, refresh)
				d.fatalf("%s did not preserve ordered interval-spaced blocked refresh cycles: %v", schema, err)
			}
			d.closeBlockedRefreshBoundary(schema, applyCheckpoint, plan, digest, refresh)
			return
		}
		d.sleep(2 * time.Second)
	}
	d.reportBlockedRefreshDiagnostics(schema, refresh)
	d.fatalf("%s did not complete three scheduled blocked refresh cycles", schema)
}

// closeBlockedRefreshBoundary closes the boundary that counted the three
// chains where they were just measured: a blocked schema refreshes for as long
// as it stays blocked, and closing it after the assertions below would count
// whatever the next scheduled cycle started while they ran.
func (d *dataPlane) closeBlockedRefreshBoundary(schema string, applyCheckpoint checkpoint, plan currentPlan,
	digest string, refresh checkpoint,
) {
	d.t.Helper()
	after := d.checkpointJobs(schema, "")
	d.waitForSchema(schema, "three complete scheduled blocked refresh cycles", blockedDestructiveSettled)
	if err := blockedPlanRetained(d.schema(schema), plan.name, plan.uid, plan.fingerprint, digest); err != nil {
		d.fatalf("%s blocked refresh changed its current plan evidence: %v", schema, err)
	}
	if err := destructivePlanRetained(d.schemaPlan(plan.name), plan.uid, plan.fingerprint, digest); err != nil {
		d.fatalf("%s immutable destructive plan changed during refresh: %v", schema, err)
	}
	d.assertNoNewJobs(schema, "apply", applyCheckpoint)
	for _, operation := range []string{"resolve", "verify", "observe", "plan"} {
		if count := d.countBetween(schema, operation, refresh, after); count != 3 {
			d.reportBlockedRefreshDiagnostics(schema, refresh)
			d.fatalf("%s crossed the exact three-chain success boundary with %d %s Jobs", schema, count, operation)
		}
	}
	if count := d.countBetween(schema, "apply", refresh, after); count != 0 {
		d.reportBlockedRefreshDiagnostics(schema, refresh)
		d.fatalf("%s crossed the exact three-chain success boundary with an Apply Job", schema)
	}
	if count := d.countBetween(schema, "", refresh, after); count != 12 {
		d.reportBlockedRefreshDiagnostics(schema, refresh)
		d.fatalf("%s crossed the exact three-chain success boundary with %d total Jobs", schema, count)
	}
}

// restoreBlockedRefreshCadence moves the blocked schema back to the quiescent
// interval, proves the one read-only chain that change starts is all it
// starts, and holds its blocked evidence to what it was.
func (d *dataPlane) restoreBlockedRefreshCadence(schema string, plan currentPlan, digest string) {
	d.t.Helper()
	d.suspendSchemaForTagMove(schema, quiescentInterval)
	before := d.checkpointJobs(schema, "")
	d.resumeSchemaAfterTagMove(schema)
	for _, operation := range []string{"resolve", "verify", "observe", "plan"} {
		d.waitForOneNewJob(schema, operation, before)
	}
	d.waitForSchema(schema, "the quiescent blocked cadence restore", blockedCadenceSettled)
	d.pauseStatusWrites()
	after := d.checkpointJobs(schema, "")
	d.assertReadOnlyCycleBetween(schema, before, after)
	restored, document := d.mustSchemaAsStored(schema)
	if err := quietCadenceRestored(restored, storedSpecOf(document).interval, plan.name, plan.uid, plan.fingerprint,
		digest); err != nil {
		d.fatalf("%s changed blocked evidence while restoring a quiet cadence: %v", schema, err)
	}
	d.auditRuntimeCredentials()
	d.mustResumeStatusWrites("could not restore controller status-write RBAC")
}

// assertMySQLDestructiveRefusalDurable holds the MySQL schema, after the fault
// injection, to still refusing the DROP INDEX plan the lifecycle left it on:
// no Apply since, and the indexes and columns still there.
func (d *dataPlane) assertMySQLDestructiveRefusalDurable() {
	d.t.Helper()
	evidence := d.mysqlDestructive
	if !evidence.retained || evidence.schema == "" || evidence.plan == "" || evidence.planUID == "" ||
		evidence.applyCheckpoint == nil || evidence.digest == "" {
		d.fatalf("MySQL destructive-refusal evidence was not retained")
	}
	matched := d.waitForSchema(evidence.schema, "the long-window MySQL DROP INDEX refusal", blockedQuiet)
	if err := mysqlDestructiveRetained(matched, evidence.plan, evidence.planUID, evidence.digest); err != nil {
		d.fatalf("MySQL DROP INDEX did not remain durably blocked: %v", err)
	}
	if err := mysqlDestructivePlan(d.schemaPlan(evidence.plan), evidence.planUID); err != nil {
		d.fatalf("MySQL DROP INDEX destructive plan identity changed: %v", err)
	}
	d.assertNoNewJobs(evidence.schema, "apply", evidence.applyCheckpoint)
	d.assertDatabaseColumn("mysql", "note", 1)
	d.assertDatabaseColumn("mysql", "enabled", 1)
	d.assertMySQLUniqueIndex(1)
	d.assertMySQLPlainIndex(1)
}

// assertRequestedDigestPinRefusal creates a schema that reads a mutable tag
// through a Docker config credential under a policy requiring the requested
// reference to be pinned. The runner resolves the tag, the policy refuses it at
// Verify, and nothing reaches the database: one Resolve, one Verify, no
// Observe, Plan or Apply.
func (d *dataPlane) assertRequestedDigestPinRefusal(reference, digest, engine, secret string) {
	d.t.Helper()
	const schema, key = "e2e-digest-pin-refusal", "e2e/digest-pin/refusal"
	resolved := repositoryOf(reference) + "@" + digest
	policy, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", "verification-policy-digest-pin.yaml"))
	d.check(err, "read the digest-pin verification policy")
	policyDigest := sha256Digest(policy)

	d.logf("checking requested-reference digest-pin enforcement")
	before := d.checkpointJobs(schema, "")
	d.createSchemaResource(schemaResource{
		name: schema, engine: engine, secret: secret, reference: reference, coordinationKey: key,
		policy: digestPinPolicyName, registryAuthSecret: digestPinDockerAuthSecret, registryAuthMode: "DockerConfigJSON",
	})
	d.waitForSchema(schema, "a mutable requested reference to be refused by the digest-pin policy", digestPinRefused)
	after := d.checkpointJobs(schema, "")
	d.assertOneJobBetween(schema, "resolve", before, after)
	d.assertOneJobBetween(schema, "verify", before, after)
	for _, operation := range []string{"observe", "plan", "apply"} {
		d.assertNoJobBetween(schema, operation, before, after)
	}
	resolveResult := d.captureOneNewJobResult(schema, "resolve", before, &after)
	if err := digestPinResolveResult(resolveResult, digest, resolved); err != nil {
		d.fatalf("%s did not complete native Resolve through DockerConfigJSON access: %v", schema, err)
	}
	if err := digestPinSourceEvidence(d.schema(schema), reference, resolved, digest, policyDigest); err != nil {
		d.fatalf("%s lost immutable source evidence or reached database work: %v", schema, err)
	}
	verifyResult := d.captureOneNewJobResult(schema, "verify", before, &after)
	if err := digestPinVerifyResult(verifyResult, digest, policyDigest); err != nil {
		d.fatalf("%s did not preserve the exact runner-enforced refusal contract: %v", schema, err)
	}
	d.assertSourceJobIsolation(schema, secret, digestPinDockerAuthSecret, "DockerConfigJSON", digestPinPolicyName,
		reference, resolved)
	d.suspend(schema, true)
	d.waitForSchema(schema, "the digest-pin refusal fixture to suspend before its refresh interval",
		func(s *ptahv1alpha1.PtahSchema) bool {
			return s.Status.Phase == ptahv1alpha1.PhaseSuspended && s.Status.ActiveOperation == nil
		})
}

// snapshotRegistryOutageEvidence reads the schema and its applied plan as
// stored, scans both, and returns the evidence an outage must leave
// untouched.
func (d *dataPlane) snapshotRegistryOutageEvidence(schema, plan string) []byte {
	d.t.Helper()
	_, schemaDocument := d.mustSchemaAsStored(schema)
	planDocument := d.unstructuredObject("PtahSchemaPlan", plan).Object
	d.scanObject(schemaDocument, "registry-outage schema evidence input")
	d.scanObject(planDocument, "registry-outage plan evidence input")
	evidence, err := outageEvidence(schemaDocument, planDocument)
	d.check(err, "project the registry-outage evidence")
	d.scan(evidence, "registry-outage retained evidence")
	return evidence
}

// waitForRegistryRefreshFailure waits for the one failed Resolve retry a
// registry outage leaves, with freshness unknown. The schema is expected to
// enter Failed, so this reads it rather than waiting through waitForSchema.
func (d *dataPlane) waitForRegistryRefreshFailure(schema string) {
	d.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		d.auditCompletedJobs()
		candidate := &ptahv1alpha1.PtahSchema{}
		if err := d.get(schema, candidate); err == nil && registryRefreshFailed(candidate) {
			return
		}
		d.sleep(time.Second)
	}
	d.fatalf("timed out waiting for one failed registry refresh with unknown freshness")
}

// waitForRegistryHTTPReady waits for the restarted registry to answer its API
// as a registry that requires a login does: 401.
func (d *dataPlane) waitForRegistryHTTPReady() {
	d.t.Helper()
	probe := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		},
	}
	url := "http://127.0.0.1:" + d.in.RegistryPort + "/v2/"
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(d.ctx, http.MethodGet, url, nil)
		d.check(err, "build the registry readiness request")
		if response, err := probe.Do(request); err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusUnauthorized {
				return
			}
		}
		d.sleep(time.Second)
	}
	d.fatalf("authenticated registry HTTP API did not become ready after restart")
}

// assertRegistryOutageAndRecovery stops the registry under a converged schema
// and holds the operator to what an outage should do: one Resolve retry that
// fails, freshness reported unknown, nothing verified, observed, planned or
// applied, and every piece of retained evidence exactly as it was. With the
// same registry container started again, the schema converges on the same
// digest through one read-only chain, without an Apply, and with the same
// binding and applied evidence.
func (d *dataPlane) assertRegistryOutageAndRecovery(schema, digest string, plan currentPlan) {
	d.t.Helper()
	if !d.rbac.paused {
		d.fatalf("registry outage proof requires the periodic no-op checkpoint barrier")
	}
	baseline, document := d.mustSchemaAsStored(schema)
	failureRetry, _, _ := unstructured.NestedString(document, "spec", "execution", "failureRetryInterval")
	if err := outageBaseline(baseline, failureRetry, digest, plan.fingerprint, d.in.PtahVersion, d.controller,
		d.stateVersion()); err != nil {
		d.fatalf("%s lacks a fresh successful baseline before registry outage: %v", schema, err)
	}
	if err := outageBaselinePlan(d.schemaPlan(plan.name), plan.uid, plan.fingerprint, digest, d.in.PtahVersion,
		d.controller, d.stateVersion()); err != nil {
		d.fatalf("%s lacks its exact durable applied Plan before outage: %v", schema, err)
	}
	retained := d.snapshotRegistryOutageEvidence(schema, plan.name)
	outageBefore := d.checkpointJobs(schema, "")
	d.assertRegistryContainerContract(true)
	_, err := d.docker("stop", "--time=10", d.in.RegistryContainerID)
	d.check(err, "stop the registry container")
	d.assertRegistryContainerContract(false)
	d.mustResumeStatusWrites("could not release the registry-outage timer barrier")
	d.waitForOneNewJob(schema, "resolve", outageBefore)
	d.waitForRegistryRefreshFailure(schema)
	d.pauseStatusWrites()
	failedAfter := d.checkpointJobs(schema, "")
	d.assertOneJobBetween(schema, "resolve", outageBefore, failedAfter)
	for _, operation := range []string{"verify", "observe", "plan", "apply"} {
		d.assertNoJobBetween(schema, operation, outageBefore, failedAfter)
	}
	if total := d.countBetween(schema, "", outageBefore, failedAfter); total != 1 {
		d.fatalf("%s created %d Jobs during its one-failure outage boundary", schema, total)
	}
	result := d.captureOneNewJobResult(schema, "resolve", outageBefore, &failedAfter)
	if err := outageResolveFailure(result); err != nil {
		d.fatalf("%s registry outage did not produce one exact read-only Resolve failure: %v", schema, err)
	}
	if !bytes.Equal(d.snapshotRegistryOutageEvidence(schema, plan.name), retained) {
		d.fatalf("%s registry outage changed retained execution, source, target, plan, applied, or success evidence", schema)
	}

	recoveryBefore := d.checkpointJobs(schema, "")
	_, err = d.docker("start", d.in.RegistryContainerID)
	d.check(err, "start the registry container")
	d.assertRegistryContainerContract(true)
	d.waitForRegistryHTTPReady()
	d.assertRegistryContainerContract(true)
	d.mustResumeStatusWrites("could not release the registry-recovery timer barrier")
	for _, operation := range []string{"resolve", "verify", "observe", "plan"} {
		d.waitForOneNewJob(schema, operation, recoveryBefore)
	}
	d.waitForSchema(schema, "same-digest registry recovery to converge without Apply", func(s *ptahv1alpha1.PtahSchema) bool {
		return s.Status.Phase == ptahv1alpha1.PhaseInSync && s.Status.Source.Digest == digest &&
			s.Status.ActiveOperation == nil && s.Status.Plan == nil
	})
	recoveryAfter := d.checkpointJobs(schema, "")
	d.assertReadOnlyCycleBetween(schema, recoveryBefore, recoveryAfter)
	d.assertReadOnlyChainBetween(schema, recoveryBefore, recoveryAfter)
	d.assertConvergenceResultPair(schema, recoveryBefore, recoveryBefore, &recoveryAfter)
	recovered, recoveredDocument := d.mustSchemaAsStored(schema)
	if err := restoredNoOp(recovered, recoveredDocument, retained, digest, d.controller, d.stateVersion()); err != nil {
		d.fatalf("%s did not restore exact fresh no-op conditions: %v", schema, err)
	}
	if err := recoveredPlan(d.schemaPlan(plan.name), plan.uid, plan.fingerprint, digest); err != nil {
		d.fatalf("%s did not retain its exact durable Plan through recovery: %v", schema, err)
	}
	d.auditRuntimeCredentials()
	d.logf("PASS registry outage freshness and exact recovery")
}

// assertReadOnlyChainBetween holds the schema's live Jobs between two
// checkpoints to one sequential Resolve, Verify, Observe and Plan chain.
func (d *dataPlane) assertReadOnlyChainBetween(schema string, before, after checkpoint) {
	d.t.Helper()
	jobs := &batchv1.JobList{}
	d.mustList(jobs, client.MatchingLabels{labelSchema: schema})
	if err := readOnlyChain(jobs.Items, before, after); err != nil {
		d.fatalf("%s did not preserve one exact sequential Resolve, Verify, Observe, Plan chain: %v", schema, err)
	}
}
