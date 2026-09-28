package e2e

import (
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// gateNow is the fixed clock the gate predicates are read against.
var gateNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func gateCondition(kind string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message}
}

func blockedSchema(deadline time.Time) *ptahv1alpha1.PtahSchema {
	next := metav1.NewTime(deadline)
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Generation = 7
	schema.Status = ptahv1alpha1.PtahSchemaStatus{
		ObservedGeneration: 7, Phase: ptahv1alpha1.PhaseBlocked, NextReconciliationTime: &next,
		Source: ptahv1alpha1.SchemaSourceStatus{Digest: testGateDigest},
		Plan: &ptahv1alpha1.CurrentPlanStatus{
			Name: "plan-v4", UID: "plan-uid", Fingerprint: testGateFingerprint, Destructive: true,
		},
		Conditions: []metav1.Condition{
			gateCondition("ApprovalRequired", metav1.ConditionFalse, "DestructiveChangesDisabled",
				"the plan is destructive; set spec.policy.allowDestructive or publish a plan that is not"),
			gateCondition("Ready", metav1.ConditionFalse, "DestructiveChangesDisabled", ""),
		},
	}
	return schema
}

const (
	testGateDigest      = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testGateFingerprint = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func resumedStored() storedSpec {
	return storedSpec{interval: blockedRefreshInterval, suspend: ptr.To(false)}
}

func TestBlockedRefreshHeadroomFollowsTheInterval(t *testing.T) {
	t.Parallel()
	capture, post := blockedRefreshHeadroom(blockedRefreshSeconds)
	if capture != 60 || post != 45 {
		t.Fatalf("headroom for %ds = %d, %d; want 60, 45", blockedRefreshSeconds, capture, post)
	}
	// The post-checkpoint headroom must stay positive for any interval the
	// phase could be given, or the stable read-back proves nothing.
	if _, post := blockedRefreshHeadroom(1); post != 0 {
		t.Fatalf("headroom for 1s = %d, want 0 for the phase to refuse", post)
	}
}

func TestBlockedRefreshBoundaryRefusesEveryUnsettledReading(t *testing.T) {
	t.Parallel()
	capture, _ := blockedRefreshHeadroom(blockedRefreshSeconds)
	ahead := gateNow.Add(80 * time.Second)
	if !blockedRefreshBoundary(blockedSchema(ahead), resumedStored(), 7, gateNow, capture) {
		t.Fatal("the settled boundary was refused")
	}
	for _, test := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchema, *storedSpec)
	}{
		{"another generation", func(s *ptahv1alpha1.PtahSchema, _ *storedSpec) { s.Generation = 8 }},
		{"generation not observed", func(s *ptahv1alpha1.PtahSchema, _ *storedSpec) { s.Status.ObservedGeneration = 6 }},
		{"suspend never written", func(_ *ptahv1alpha1.PtahSchema, stored *storedSpec) { stored.suspend = nil }},
		{"still suspended", func(_ *ptahv1alpha1.PtahSchema, stored *storedSpec) { stored.suspend = ptr.To(true) }},
		{"interval spelled another way", func(_ *ptahv1alpha1.PtahSchema, stored *storedSpec) { stored.interval = "1m30s" }},
		{"not blocked", func(s *ptahv1alpha1.PtahSchema, _ *storedSpec) { s.Status.Phase = ptahv1alpha1.PhasePlanning }},
		{"an operation in flight", func(s *ptahv1alpha1.PtahSchema, _ *storedSpec) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationResolve}
		}},
		{"an observation pending", func(s *ptahv1alpha1.PtahSchema, _ *storedSpec) {
			s.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{}
		}},
		{"a lock pending release", func(s *ptahv1alpha1.PtahSchema, _ *storedSpec) {
			s.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		}},
		{"no deadline", func(s *ptahv1alpha1.PtahSchema, _ *storedSpec) { s.Status.NextReconciliationTime = nil }},
		{"deadline inside the headroom", func(s *ptahv1alpha1.PtahSchema, _ *storedSpec) {
			inside := metav1.NewTime(gateNow.Add(59 * time.Second))
			s.Status.NextReconciliationTime = &inside
		}},
		{"refusal gives no way out", func(s *ptahv1alpha1.PtahSchema, _ *storedSpec) {
			s.Status.Conditions[0].Message = "the plan is destructive"
		}},
		{"refused for another reason", func(s *ptahv1alpha1.PtahSchema, _ *storedSpec) {
			s.Status.Conditions[0].Reason = "Waiting"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema, stored := blockedSchema(ahead), resumedStored()
			test.mutate(schema, &stored)
			if blockedRefreshBoundary(schema, stored, 7, gateNow, capture) {
				t.Fatal("accepted")
			}
		})
	}
}

func TestBlockedBoundaryStableHoldsTheCapturedDeadline(t *testing.T) {
	t.Parallel()
	_, post := blockedRefreshHeadroom(blockedRefreshSeconds)
	deadline := gateNow.Add(80 * time.Second)
	captured := metav1.NewTime(deadline)
	schema := blockedSchema(deadline)
	// The read-back does not ask for the direction the first reading did.
	schema.Status.Conditions[0].Message = ""
	if !blockedBoundaryStable(schema, resumedStored(), 7, captured, gateNow.Add(20*time.Second), post) {
		t.Fatal("the unchanged boundary was refused")
	}
	if blockedBoundaryStable(blockedSchema(deadline.Add(time.Second)), resumedStored(), 7, captured, gateNow, post) {
		t.Fatal("a moved deadline was accepted")
	}
	if blockedBoundaryStable(schema, resumedStored(), 7, captured, gateNow.Add(36*time.Second), post) {
		t.Fatal("a deadline inside the post-checkpoint headroom was accepted")
	}
	stripped := blockedSchema(deadline)
	stripped.Status.Conditions = stripped.Status.Conditions[1:]
	if blockedBoundaryStable(stripped, resumedStored(), 7, captured, gateNow, post) {
		t.Fatal("a boundary without the refusal was accepted")
	}
}

func chainRecords(starts []time.Duration, operations ...string) []observedJob {
	var records []observedJob
	for chain, start := range starts {
		for index, operation := range operations {
			records = append(records, observedJob{
				UID: operation + "-" + string(rune('a'+chain)), Name: operation, Schema: "e2e-postgresql",
				Operation: operation,
				Created:   gateNow.Add(start + time.Duration(index)*time.Second).Format(time.RFC3339),
			})
		}
	}
	return records
}

func TestOrderedRefreshChainsRequiresThreeSpacedChains(t *testing.T) {
	t.Parallel()
	operations := []string{"resolve", "verify", "observe", "plan"}
	spaced := []time.Duration{0, 95 * time.Second, 190 * time.Second}
	if err := orderedRefreshChains(chainRecords(spaced, operations...), blockedRefreshSeconds); err != nil {
		t.Fatalf("three spaced chains were refused: %v", err)
	}
	// The ledger's own order does not decide: the Jobs are dated by their
	// creation time.
	reversed := chainRecords(spaced, operations...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	if err := orderedRefreshChains(reversed, blockedRefreshSeconds); err != nil {
		t.Fatalf("three spaced chains read in reverse were refused: %v", err)
	}
	// A chain started within one second orders by operation.
	sameSecond := chainRecords(spaced, operations...)
	for index := range sameSecond {
		sameSecond[index].Created = sameSecond[index-index%4].Created
	}
	if err := orderedRefreshChains(sameSecond, blockedRefreshSeconds); err != nil {
		t.Fatalf("chains whose Jobs share a second were refused: %v", err)
	}
	for _, test := range []struct {
		name    string
		records []observedJob
	}{
		{"two chains", chainRecords(spaced[:2], operations...)},
		{"four chains", chainRecords(append(spaced, 285*time.Second), operations...)},
		{"a chain short of its Plan", chainRecords(spaced, "resolve", "verify", "observe")},
		{"an Apply among them", append(chainRecords(spaced, operations...), observedJob{
			UID: "apply", Operation: "apply", Created: gateNow.Add(300 * time.Second).Format(time.RFC3339),
		})},
		{"chains closer than the interval", chainRecords([]time.Duration{0, 89 * time.Second, 190 * time.Second}, operations...)},
		{"Verify ahead of Resolve", chainRecords(spaced, "verify", "resolve", "observe", "plan")},
		{"no creation time", func() []observedJob {
			records := chainRecords(spaced, operations...)
			records[0].Created = ""
			return records
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := orderedRefreshChains(test.records, blockedRefreshSeconds); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestBlockedEvidencePredicatesHoldTheSamePlan(t *testing.T) {
	t.Parallel()
	schema := blockedSchema(gateNow.Add(time.Minute))
	if !blockedDestructiveSettled(schema) || !blockedCadenceSettled(schema) || !blockedQuiet(schema) {
		t.Fatal("a settled blocked schema was refused")
	}
	if err := blockedPlanRetained(schema, "plan-v4", "plan-uid", testGateFingerprint, testGateDigest); err != nil {
		t.Fatalf("the retained plan was refused: %v", err)
	}
	if err := quietCadenceRestored(schema, quiescentInterval, "plan-v4", "plan-uid", testGateFingerprint, testGateDigest); err != nil {
		t.Fatalf("the restored cadence was refused: %v", err)
	}
	if err := quietCadenceRestored(schema, blockedRefreshInterval, "plan-v4", "plan-uid", testGateFingerprint, testGateDigest); err == nil {
		t.Fatal("a cadence left at the blocked interval was accepted as restored")
	}
	for _, test := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchema)
	}{
		{"another plan", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.UID = "other-plan-uid" }},
		{"another fingerprint", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.Fingerprint = testGateDigest }},
		{"not destructive", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.Destructive = false }},
		{"another digest", func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.Digest = testGateFingerprint }},
		{"no plan", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = nil }},
		{"not blocked", func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseAwaitingApproval }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mutated := blockedSchema(gateNow.Add(time.Minute))
			test.mutate(mutated)
			if blockedPlanRetained(mutated, "plan-v4", "plan-uid", testGateFingerprint, testGateDigest) == nil {
				t.Fatal("accepted")
			}
		})
	}
	unsettled := blockedSchema(gateNow)
	unsettled.Status.NextReconciliationTime = nil
	if blockedDestructiveSettled(unsettled) || blockedCadenceSettled(unsettled) {
		t.Fatal("a blocked schema with no refresh deadline was accepted as settled")
	}
	lagging := blockedSchema(gateNow)
	lagging.Status.ObservedGeneration = 6
	if blockedCadenceSettled(lagging) {
		t.Fatal("a generation not yet observed was accepted as restored")
	}
	stripped := blockedSchema(gateNow)
	stripped.Status.Conditions = stripped.Status.Conditions[1:]
	if blockedDestructiveSettled(stripped) {
		t.Fatal("a blocked schema without the destructive refusal was accepted")
	}

	plan := &ptahv1alpha1.PtahSchemaPlan{}
	plan.UID = "plan-uid"
	plan.Spec = ptahv1alpha1.PtahSchemaPlanSpec{
		Fingerprint: testGateFingerprint, ArtifactDigest: testGateDigest, Destructive: true, StatementCount: 1,
	}
	if err := destructivePlanRetained(plan, "plan-uid", testGateFingerprint, testGateDigest); err != nil {
		t.Fatalf("the destructive plan was refused: %v", err)
	}
	if err := mysqlDestructivePlan(plan, "plan-uid"); err != nil {
		t.Fatalf("the one-statement destructive plan was refused: %v", err)
	}
	plan.Spec.StatementCount = 2
	if mysqlDestructivePlan(plan, "plan-uid") == nil {
		t.Fatal("a plan grown to two statements was accepted")
	}
	plan.Spec.Destructive = false
	if destructivePlanRetained(plan, "plan-uid", testGateFingerprint, testGateDigest) == nil {
		t.Fatal("a plan no longer destructive was accepted")
	}
}

func TestMySQLDestructiveRetainedRefusesAnApprovedOrMovingSchema(t *testing.T) {
	t.Parallel()
	if err := mysqlDestructiveRetained(blockedSchema(gateNow), "plan-v4", "plan-uid", testGateDigest); err != nil {
		t.Fatalf("the refused plan was not accepted: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchema)
	}{
		{"approved", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.Approval = &ptahv1alpha1.ConsumedApprovalStatus{} }},
		{"an operation in flight", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationApply}
		}},
		{"Ready", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[1] = gateCondition("Ready", metav1.ConditionTrue, "InSync", "")
		}},
		{"refusal lifted", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[0] = gateCondition("ApprovalRequired", metav1.ConditionTrue, "Waiting", "")
		}},
		{"another plan", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.Name = "plan-v5" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := blockedSchema(gateNow)
			test.mutate(schema)
			if mysqlDestructiveRetained(schema, "plan-v4", "plan-uid", testGateDigest) == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func chainJob(uid, operation string, start, complete time.Duration, completed bool) batchv1.Job {
	job := batchv1.Job{}
	job.UID = types.UID(uid)
	job.Name = uid
	job.Labels = map[string]string{labelOperation: operation}
	started := metav1.NewTime(gateNow.Add(start))
	finished := metav1.NewTime(gateNow.Add(complete))
	job.Status.StartTime, job.Status.CompletionTime = &started, &finished
	if completed {
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	} else {
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	}
	return job
}

func readOnlyJobs() []batchv1.Job {
	return []batchv1.Job{
		chainJob("old-plan", "plan", -100*time.Second, -90*time.Second, true),
		chainJob("resolve", "resolve", 0, 5*time.Second, true),
		chainJob("verify", "verify", 5*time.Second, 9*time.Second, true),
		chainJob("observe", "observe", 10*time.Second, 20*time.Second, true),
		chainJob("plan", "plan", 20*time.Second, 30*time.Second, true),
	}
}

func TestReadOnlyChainHoldsOneSequentialChain(t *testing.T) {
	t.Parallel()
	before := checkpoint{"old-plan"}
	after := sortedCheckpoint([]string{"old-plan", "resolve", "verify", "observe", "plan"})
	if err := readOnlyChain(readOnlyJobs(), before, after); err != nil {
		t.Fatalf("one sequential chain was refused: %v", err)
	}
	for _, test := range []struct {
		name  string
		jobs  func() []batchv1.Job
		after checkpoint
	}{
		{"a Job the after checkpoint does not hold", readOnlyJobs, sortedCheckpoint([]string{"old-plan", "resolve", "verify", "observe"})},
		{"an Apply in the boundary", func() []batchv1.Job {
			return append(readOnlyJobs(), chainJob("apply", "apply", 31*time.Second, 40*time.Second, true))
		}, sortedCheckpoint([]string{"old-plan", "resolve", "verify", "observe", "plan", "apply"})},
		{"two Plans in the boundary", func() []batchv1.Job {
			return append(readOnlyJobs(), chainJob("plan-2", "plan", 31*time.Second, 40*time.Second, true))
		}, sortedCheckpoint([]string{"old-plan", "resolve", "verify", "observe", "plan", "plan-2"})},
		{"Observe overlapping Verify", func() []batchv1.Job {
			jobs := readOnlyJobs()
			jobs[3] = chainJob("observe", "observe", 8*time.Second, 20*time.Second, true)
			return jobs
		}, after},
		{"a failed Verify", func() []batchv1.Job {
			jobs := readOnlyJobs()
			jobs[2] = chainJob("verify", "verify", 5*time.Second, 9*time.Second, false)
			return jobs
		}, after},
		{"a Plan never started", func() []batchv1.Job {
			jobs := readOnlyJobs()
			jobs[4].Status.StartTime = nil
			return jobs
		}, after},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := readOnlyChain(test.jobs(), before, test.after); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func refreshFailedSchema() *ptahv1alpha1.PtahSchema {
	next := metav1.NewTime(gateNow.Add(45 * time.Second))
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Status = ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseFailed, NextReconciliationTime: &next,
		ActiveOperation: &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationResolve, Attempt: 2},
		Conditions: []metav1.Condition{
			gateCondition("ArtifactResolved", metav1.ConditionUnknown, "RefreshFailed", ""),
			gateCondition("ArtifactVerified", metav1.ConditionTrue, "PolicySatisfied", ""),
			gateCondition("PlanReady", metav1.ConditionUnknown, "SourceFreshnessUnknown", ""),
			gateCondition("InSync", metav1.ConditionUnknown, "SourceFreshnessUnknown", ""),
			gateCondition("Ready", metav1.ConditionFalse, "OperationFailed", ""),
			gateCondition("ReconciliationFailed", metav1.ConditionTrue, "OperationFailed", ""),
		},
	}
	return schema
}

func TestRegistryRefreshFailedReadsUnknownFreshness(t *testing.T) {
	t.Parallel()
	if !registryRefreshFailed(refreshFailedSchema()) {
		t.Fatal("the failed refresh was refused")
	}
	for _, test := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchema)
	}{
		{"not failed", func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseResolving }},
		{"first attempt", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.Attempt = 1 }},
		{"a Job behind the retry", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobUID = "job-uid" }},
		{"a Verify retry", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.Type = ptahv1alpha1.OperationVerify }},
		{"no operation", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil }},
		{"no refresh scheduled", func(s *ptahv1alpha1.PtahSchema) { s.Status.NextReconciliationTime = nil }},
		{"freshness lost rather than unknown", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[3] = gateCondition("InSync", metav1.ConditionFalse, "SourceFreshnessUnknown", "")
		}},
		{"verification dropped", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[1] = gateCondition("ArtifactVerified", metav1.ConditionUnknown, "RefreshFailed", "")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := refreshFailedSchema()
			test.mutate(schema)
			if registryRefreshFailed(schema) {
				t.Fatal("accepted")
			}
		})
	}
}

func outageDocuments() (map[string]any, map[string]any) {
	schema := map[string]any{
		"status": map[string]any{
			"executionBinding":             map[string]any{"epoch": "v1-00000000000000000000000000000000", "controllerStateVersion": int64(3)},
			"source":                       map[string]any{"digest": testGateDigest},
			"target":                       map[string]any{"identityDigest": testGateFingerprint},
			"applied":                      map[string]any{"artifactDigest": testGateDigest, "futureField": "kept"},
			"lastSuccessfulReconciliation": "2026-09-01T11:59:00Z",
			"conditions":                   []any{map[string]any{"type": "InSync"}},
		},
	}
	plan := map[string]any{
		"metadata": map[string]any{
			"name": "plan-v1", "uid": "plan-uid", "generation": int64(1),
			"creationTimestamp": "2026-09-01T11:00:00Z", "resourceVersion": "42",
		},
		"spec":   map[string]any{"fingerprint": testGateFingerprint},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}
	return schema, plan
}

func TestOutageEvidenceComparesWholeDocuments(t *testing.T) {
	t.Parallel()
	schema, plan := outageDocuments()
	before, err := outageEvidence(schema, plan)
	if err != nil {
		t.Fatal(err)
	}
	// What the outage is allowed to move: the conditions and the plan's
	// resourceVersion are not evidence.
	schema["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "InSync", "status": "Unknown"}}
	plan["metadata"].(map[string]any)["resourceVersion"] = "43"
	if again, err := outageEvidence(schema, plan); err != nil || string(again) != string(before) {
		t.Fatalf("evidence moved with fields it does not cover: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(schema, plan map[string]any)
	}{
		{"a field the typed API would drop", func(schema, _ map[string]any) {
			schema["status"].(map[string]any)["applied"].(map[string]any)["futureField"] = "moved"
		}},
		{"the binding", func(schema, _ map[string]any) {
			schema["status"].(map[string]any)["executionBinding"].(map[string]any)["controllerStateVersion"] = int64(4)
		}},
		{"the last success", func(schema, _ map[string]any) {
			schema["status"].(map[string]any)["lastSuccessfulReconciliation"] = "2026-09-01T12:01:00Z"
		}},
		{"the current plan", func(schema, _ map[string]any) {
			schema["status"].(map[string]any)["plan"] = map[string]any{"name": "plan-v2"}
		}},
		{"the plan status", func(_, plan map[string]any) { plan["status"] = map[string]any{} }},
		{"the plan generation", func(_, plan map[string]any) { plan["metadata"].(map[string]any)["generation"] = int64(2) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema, plan := outageDocuments()
			test.mutate(schema, plan)
			after, err := outageEvidence(schema, plan)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) == string(before) {
				t.Fatal("the change did not reach the evidence")
			}
		})
	}
}

var gateController = controllerIdentity{
	image:    "registry.invalid/ptah-operator@sha256:3333333333333333333333333333333333333333333333333333333333333333",
	revision: "abc123", stateVersion: "3",
}

func convergedBaseline() *ptahv1alpha1.PtahSchema {
	last := metav1.NewTime(gateNow)
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Status = ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseInSync, Source: ptahv1alpha1.SchemaSourceStatus{Digest: testGateDigest},
		ExecutionBinding: &ptahv1alpha1.ExecutionBindingStatus{Epoch: "v1-00000000000000000000000000000000", ControllerStateVersion: 3},
		Applied: &ptahv1alpha1.AppliedStatus{
			ArtifactDigest: testGateDigest, PlanFingerprint: testGateFingerprint, PtahVersion: "v1.2.3",
			ExecutionBindingID: "v1-00000000000000000000000000000000", ControllerImage: gateController.image,
			ControllerRevision: gateController.revision, ControllerStateVersion: 3,
		},
		LastSuccessfulReconciliation: &last,
		Conditions: []metav1.Condition{
			gateCondition("ArtifactResolved", metav1.ConditionTrue, "DigestPinned", ""),
			gateCondition("ArtifactVerified", metav1.ConditionTrue, "PolicySatisfied", ""),
			gateCondition("PlanReady", metav1.ConditionFalse, "NoChanges", ""),
			gateCondition("InSync", metav1.ConditionTrue, "ScopedConverged", ""),
			gateCondition("Ready", metav1.ConditionTrue, "InSync", ""),
			gateCondition("ReconciliationFailed", metav1.ConditionFalse, "Succeeded", ""),
		},
	}
	return schema
}

func TestOutageBaselineRequiresAFreshSuccess(t *testing.T) {
	t.Parallel()
	if err := outageBaseline(convergedBaseline(), "45s", testGateDigest, testGateFingerprint, "v1.2.3", gateController, 3); err != nil {
		t.Fatalf("the fresh baseline was refused: %v", err)
	}
	if outageBaseline(convergedBaseline(), "45000ms", testGateDigest, testGateFingerprint, "v1.2.3", gateController, 3) == nil {
		t.Fatal("a retry interval stored another way was accepted")
	}
	for _, test := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchema)
	}{
		{"a plan pending", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{} }},
		{"another Ptah version applied", func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied.PtahVersion = "v1.2.2" }},
		{"another manager applied", func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied.ControllerRevision = "def456" }},
		{"never succeeded", func(s *ptahv1alpha1.PtahSchema) { s.Status.LastSuccessfulReconciliation = nil }},
		{"resolved by tag", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[0] = gateCondition("ArtifactResolved", metav1.ConditionTrue, "Resolved", "")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := convergedBaseline()
			test.mutate(schema)
			if outageBaseline(schema, "45s", testGateDigest, testGateFingerprint, "v1.2.3", gateController, 3) == nil {
				t.Fatal("accepted")
			}
		})
	}

	plan := &ptahv1alpha1.PtahSchemaPlan{}
	plan.UID = "plan-uid"
	plan.Spec = ptahv1alpha1.PtahSchemaPlanSpec{
		ContractVersion: 3, Fingerprint: testGateFingerprint, ArtifactDigest: testGateDigest, PtahVersion: "v1.2.3",
		ExecutionBindingID: "v1-00000000000000000000000000000000", ControllerImage: gateController.image,
		ControllerRevision: gateController.revision, ControllerStateVersion: 3,
	}
	plan.Status.Conditions = []metav1.Condition{gateCondition("Ready", metav1.ConditionTrue, "Published", "")}
	if err := outageBaselinePlan(plan, "plan-uid", testGateFingerprint, testGateDigest, "v1.2.3", gateController, 3); err != nil {
		t.Fatalf("the applied plan was refused: %v", err)
	}
	if err := recoveredPlan(plan, "plan-uid", testGateFingerprint, testGateDigest); err != nil {
		t.Fatalf("the recovered plan was refused: %v", err)
	}
	plan.Spec.ContractVersion = 2
	if outageBaselinePlan(plan, "plan-uid", testGateFingerprint, testGateDigest, "v1.2.3", gateController, 3) == nil {
		t.Fatal("a plan of another contract was accepted")
	}
	plan.Status.Conditions = nil
	if recoveredPlan(plan, "plan-uid", testGateFingerprint, testGateDigest) == nil {
		t.Fatal("a plan that is not Ready was accepted after recovery")
	}
}

func TestOutageResolveFailureIsOneReadOnlyFailure(t *testing.T) {
	t.Parallel()
	failed := runner.Result{ChildExitCode: 1, Error: &runner.ResultError{Code: "registry_unreachable"}}
	if err := outageResolveFailure(failed); err != nil {
		t.Fatalf("the read-only failure was refused: %v", err)
	}
	for name, result := range map[string]runner.Result{
		"exit 0":            {Error: &runner.ResultError{Code: "registry_unreachable"}},
		"no error":          {ChildExitCode: 1},
		"printed something": {ChildExitCode: 1, Error: &runner.ResultError{}, Stdout: "x"},
		"started mutating":  {ChildExitCode: 1, Error: &runner.ResultError{}, MutationStarted: true},
		"uncertain":         {ChildExitCode: 1, Error: &runner.ResultError{}, Uncertain: true},
		"truncated":         {ChildExitCode: 1, Error: &runner.ResultError{}, Truncation: &runner.TruncationMetadata{}},
	} {
		if outageResolveFailure(result) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRestoredNoOpHoldsTheRetainedEvidence(t *testing.T) {
	t.Parallel()
	retainedSchema, plan := outageDocuments()
	status := retainedSchema["status"].(map[string]any)
	status["executionBinding"] = map[string]any{"epoch": "v1-00000000000000000000000000000000", "controllerStateVersion": 3}
	status["applied"] = map[string]any{"artifactDigest": testGateDigest, "futureField": "kept"}
	retained, err := outageEvidence(retainedSchema, plan)
	if err != nil {
		t.Fatal(err)
	}
	live := func() map[string]any {
		return map[string]any{"status": map[string]any{
			"executionBinding": map[string]any{"epoch": "v1-00000000000000000000000000000000", "controllerStateVersion": int64(3)},
			"applied":          map[string]any{"artifactDigest": testGateDigest, "futureField": "kept"},
		}}
	}
	if err := restoredNoOp(convergedBaseline(), live(), retained, testGateDigest, gateController, 3); err != nil {
		t.Fatalf("the restored no-op was refused: %v", err)
	}
	moved := live()
	moved["status"].(map[string]any)["applied"].(map[string]any)["futureField"] = "moved"
	if restoredNoOp(convergedBaseline(), moved, retained, testGateDigest, gateController, 3) == nil {
		t.Fatal("applied evidence that moved in a field the typed API drops was accepted")
	}
	rebound := live()
	rebound["status"].(map[string]any)["executionBinding"].(map[string]any)["epoch"] = "v1-11111111111111111111111111111111"
	if restoredNoOp(convergedBaseline(), rebound, retained, testGateDigest, gateController, 3) == nil {
		t.Fatal("a moved execution binding was accepted")
	}
	for _, test := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchema)
	}{
		{"planned again", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{} }},
		{"PlanReady not NoChanges", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[2] = gateCondition("PlanReady", metav1.ConditionUnknown, "SourceFreshnessUnknown", "")
		}},
		{"still failing", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[5] = gateCondition("ReconciliationFailed", metav1.ConditionTrue, "OperationFailed", "")
		}},
		{"another manager", func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied.ControllerImage = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := convergedBaseline()
			test.mutate(schema)
			if restoredNoOp(schema, live(), retained, testGateDigest, gateController, 3) == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func digestPinSchema() *ptahv1alpha1.PtahSchema {
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Status = ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseBlocked,
		Source: ptahv1alpha1.SchemaSourceStatus{
			RequestedReference: "oci://registry.ns.svc.cluster.local:5000/schemas/postgresql:stable",
			ResolvedReference:  "oci://registry.ns.svc.cluster.local:5000/schemas/postgresql@" + testGateDigest,
			Digest:             testGateDigest, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 512,
			VerificationPolicyDigest: testGateFingerprint,
		},
		Conditions: []metav1.Condition{
			gateCondition("ArtifactVerified", metav1.ConditionFalse, "PolicyRefused", "verification refused: require_digest_pin"),
		},
	}
	return schema
}

func TestDigestPinPredicatesHoldTheRefusal(t *testing.T) {
	t.Parallel()
	requested := "oci://registry.ns.svc.cluster.local:5000/schemas/postgresql:stable"
	resolved := repositoryOf(requested) + "@" + testGateDigest
	if resolved != "oci://registry.ns.svc.cluster.local:5000/schemas/postgresql@"+testGateDigest {
		t.Fatalf("repositoryOf cut %q, not the tag alone", resolved)
	}
	if !digestPinRefused(digestPinSchema()) {
		t.Fatal("the refused schema was not accepted")
	}
	if err := digestPinSourceEvidence(digestPinSchema(), requested, resolved, testGateDigest, testGateFingerprint); err != nil {
		t.Fatalf("the refused source was not accepted: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchema)
	}{
		{"verified", func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.Verified = true }},
		{"an artifact type recorded", func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.ArtifactType = schemaArtifactType }},
		{"a plan published", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{} }},
		{"no size", func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.Size = 0 }},
		{"another policy", func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.VerificationPolicyDigest = testGateDigest }},
		{"refused for another requirement", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[0].Message = "verification refused: require_signature"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := digestPinSchema()
			test.mutate(schema)
			if digestPinSourceEvidence(schema, requested, resolved, testGateDigest, testGateFingerprint) == nil {
				t.Fatal("accepted")
			}
		})
	}
	inFlight := digestPinSchema()
	inFlight.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationVerify}
	if digestPinRefused(inFlight) {
		t.Fatal("a refusal with an operation in flight was accepted as settled")
	}

	resolve := runner.Result{ResolvedDigest: testGateDigest, ResolvedReference: resolved}
	if err := digestPinResolveResult(resolve, testGateDigest, resolved); err != nil {
		t.Fatalf("the Resolve was refused: %v", err)
	}
	resolve.ResolvedReference = requested
	if digestPinResolveResult(resolve, testGateDigest, resolved) == nil {
		t.Fatal("a Resolve that kept the tag was accepted")
	}

	verify := func() runner.Result {
		return runner.Result{
			ResolvedDigest: testGateDigest, VerificationPolicyDigest: testGateFingerprint,
			VerificationRequirements: []string{"require_digest_pin"},
			Error:                    &runner.ResultError{Code: "verification_refused"},
		}
	}
	if err := digestPinVerifyResult(verify(), testGateDigest, testGateFingerprint); err != nil {
		t.Fatalf("the refusing Verify was refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*runner.Result)
	}{
		{"another requirement", func(r *runner.Result) {
			r.VerificationRequirements = []string{"require_digest_pin", "require_signature"}
		}},
		{"not refused", func(r *runner.Result) { r.Error = nil }},
		{"refused for another code", func(r *runner.Result) { r.Error.Code = "invalid_oci_access" }},
		{"read the artifact", func(r *runner.Result) { r.ObservedArtifactType = schemaArtifactType }},
		{"resolved again", func(r *runner.Result) { r.ResolvedReference = resolved }},
		{"exit status", func(r *runner.Result) { r.ChildExitCode = 1 }},
		{"another policy", func(r *runner.Result) { r.VerificationPolicyDigest = testGateDigest }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := verify()
			test.mutate(&result)
			if err := digestPinVerifyResult(result, testGateDigest, testGateFingerprint); err == nil ||
				strings.TrimSpace(err.Error()) == "" {
				t.Fatal("accepted")
			}
		})
	}
}
