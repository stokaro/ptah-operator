package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestRecoveredSchemaMayReuseItsImmutablePlan(t *testing.T) {
	var recovered ptahv1.PtahSchema
	var approval ptahv1.PtahSchemaApproval
	for name, into := range map[string]any{"alert-schema-recovered.json": &recovered, "alert-schema-original-approval.json": &approval} {
		raw, err := os.ReadFile(filepath.Join("../..", "testdata/e2e/readings", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatal(err)
		}
	}
	if recovered.Status.Plan == nil || recovered.Status.Plan.UID != approval.Spec.PlanRef.UID {
		t.Fatal("native reading no longer reproduces immutable plan reuse")
	}
	after := approval.Spec.ApprovedAt.Time
	if !alRecoveredSchemaApprovalReady(&recovered, approval.Spec.SchemaRef.UID, after) {
		t.Fatal("fresh native approval gate refused because the plan was reused")
	}
	for name, mutate := range map[string]func(*ptahv1.PtahSchema){
		"replacement":                func(v *ptahv1.PtahSchema) { v.UID = "replacement" },
		"stale observation":          func(v *ptahv1.PtahSchema) { v.Status.Target.LastObservedAt = &metav1.Time{Time: after} },
		"missing observation":        func(v *ptahv1.PtahSchema) { v.Status.Target.LastObservedAt = nil },
		"unobserved generation":      func(v *ptahv1.PtahSchema) { v.Status.ObservedGeneration-- },
		"pending observation":        func(v *ptahv1.PtahSchema) { v.Status.PendingObservation = &ptahv1.PendingObservationStatus{} },
		"pending release":            func(v *ptahv1.PtahSchema) { v.Status.PendingLockRelease = &ptahv1.TargetLockReleaseStatus{} },
		"active Apply":               func(v *ptahv1.PtahSchema) { v.Status.ActiveOperation = proofApplyActive() },
		"applied without approval":   func(v *ptahv1.PtahSchema) { v.Status.Applied = &ptahv1.AppliedStatus{} },
		"consumed approval retained": func(v *ptahv1.PtahSchema) { v.Status.Plan.Approval = &ptahv1.ConsumedApprovalStatus{} },
		"suspended":                  func(v *ptahv1.PtahSchema) { v.Spec.Suspend = true },
		"no approval gate":           func(v *ptahv1.PtahSchema) { v.Status.Conditions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			v := recovered.DeepCopy()
			mutate(v)
			if alRecoveredSchemaApprovalReady(v, approval.Spec.SchemaRef.UID, after) {
				t.Fatal("unsafe or stale recovery accepted")
			}
		})
	}
	if alRecoveredSchemaApprovalReady(&recovered, approval.Spec.SchemaRef.UID, time.Time{}) {
		t.Fatal("missing incident time accepted")
	}
}

func alSchemaTraceFixture() (*ptahv1.PtahSchema, []watchEvent[*ptahv1.PtahSchema]) {
	plan := proofPlan()
	before := proofSchema("incident", ptahv1.PtahSchemaStatus{Plan: &plan, ActiveOperation: proofApplyActive(), ObservedGeneration: 3,
		Conditions: []metav1.Condition{{Type: ptahv1.ConditionApplying, Status: metav1.ConditionTrue, LastTransitionTime: proofTime(1)}}})
	before.Namespace, before.Generation = "ns", 3
	before.Spec.Policy.Apply = ptahv1.ApplyPolicyOnApproval
	unknown := before.DeepCopy()
	unknown.Generation, unknown.Spec.Suspend = 4, true
	unknown.Status.ActiveOperation = nil
	unknown.Status.PendingObservation = proofPending(ptahv1.PendingObservationOutcomeUnknown)
	unknown.Status.PendingObservation.ObserveAfter = proofTimePointer(630)
	unknown.Status.Conditions = []metav1.Condition{{Type: ptahv1.ConditionApplying, Status: metav1.ConditionFalse, Reason: "OutcomeUnknown", LastTransitionTime: proofTime(10)}}
	observeClaim := unknown.DeepCopy()
	observeClaim.Generation, observeClaim.Spec.Suspend = 5, false
	observeClaim.Status.ActiveOperation = proofActive(ptahv1.OperationObserve, "observe", "")
	observeClaim.Status.ActiveOperation.StartedAt = proofTime(640)
	observe := observeClaim.DeepCopy()
	observe.Status.ActiveOperation.JobUID = "observe-uid"
	planClaim := observe.DeepCopy()
	planClaim.Status.PendingObservation.PlanRequired = true
	planClaim.Status.Target = proofTarget()
	planClaim.Status.Target.LastObservedAt = proofTimePointer(650)
	planClaim.Status.ActiveOperation = proofActive(ptahv1.OperationPlan, "proof-plan", "")
	planClaim.Status.ActiveOperation.StartedAt = proofTime(660)
	proof := planClaim.DeepCopy()
	proof.Status.ActiveOperation.JobUID = "proof-plan-uid"
	settled := proof.DeepCopy()
	settled.Status.ActiveOperation, settled.Status.PendingObservation = nil, nil
	settled.Status.LastAttemptTime = proofTimePointer(670)
	var events []watchEvent[*ptahv1.PtahSchema]
	for _, v := range []*ptahv1.PtahSchema{before.DeepCopy(), unknown, observeClaim, observe, planClaim, proof, settled} {
		events = append(events, watchEvent[*ptahv1.PtahSchema]{Type: watch.Modified, Object: v})
	}
	return before, events
}

func TestAlSchemaUnresolvedTraceBindsReadOnlyRecovery(t *testing.T) {
	before, events := alSchemaTraceFixture()
	h, err := alReadSchemaUnresolvedTrace(events, before, true)
	if err != nil || !h.recorded.Equal(proofTime(10).Time) || !h.observed.Equal(proofTime(650).Time) || !h.accounted.Equal(proofTime(670).Time) {
		t.Fatalf("recovery=%+v: %v", h, err)
	}
	// Later refresh bookkeeping must not move the original resolution bound.
	later := events[len(events)-1].Object.DeepCopy()
	later.Status.LastAttemptTime = proofTimePointer(900)
	events = append(events, watchEvent[*ptahv1.PtahSchema]{Type: watch.Modified, Object: later})
	h, err = alReadSchemaUnresolvedTrace(events, before, true)
	if err != nil || !h.accounted.Equal(proofTime(670).Time) {
		t.Fatalf("deadline moved: %+v, %v", h, err)
	}
	if _, err := alReadSchemaUnresolvedTrace(events[:2], before, false); err != nil {
		t.Fatal(err)
	}
	if _, err := alReadSchemaUnresolvedTrace(events[:2], before, true); err == nil {
		t.Fatal("unrecovered incident accepted")
	}
	if _, err := alReadSchemaUnresolvedTrace(events, before, false); err == nil {
		t.Fatal("resolved incident counted as held")
	}
	// Discovery after the first Unknown write is legitimate; a new executor is not.
	before, events = alSchemaTraceFixture()
	events[1].Object.Status.PendingObservation.ApplyPodUIDs = nil
	events[1].Object.Status.PendingObservation.ApplyPodCount = 0
	if _, err := alReadSchemaUnresolvedTrace(events, before, true); err != nil {
		t.Fatal(err)
	}
}

func TestAlSchemaUnresolvedTraceRejectsMissingOrCorruptProof(t *testing.T) {
	type history = []watchEvent[*ptahv1.PtahSchema]
	for name, mutate := range map[string]func(history) history{
		"empty history":         func(h history) history { return nil },
		"no Unknown":            func(h history) history { return h[:1] },
		"no executed Observe":   func(h history) history { return append(h[:3], h[4:]...) },
		"no executed Plan":      func(h history) history { return append(h[:5], h[6:]...) },
		"early clearing":        func(h history) history { return append(h[:2], h[6:]...) },
		"replaced resource":     func(h history) history { h[2].Object.UID = "replacement"; return h },
		"deleted resource":      func(h history) history { h[2].Type = watch.Deleted; return h },
		"changed desired state": func(h history) history { h[2].Object.Spec.Desired.OCIRef = "other"; return h },
		"extra generation":      func(h history) history { h[2].Object.Generation++; h[2].Object.Generation++; return h },
		"wrong original plan":   func(h history) history { h[1].Object.Status.PendingObservation.Plan.UID = "other"; return h },
		"wrong original target": func(h history) history { h[1].Object.Status.PendingObservation.Target.URLFrom.Name = "other"; return h },
		"wrong original source": func(h history) history {
			h[1].Object.Status.PendingObservation.Source.Digest = proofDigest("b")
			return h
		},
		"wrong protected tables": func(h history) history {
			h[1].Object.Status.PendingObservation.ProtectedTables = []string{"other"}
			return h
		},
		"shortened execution horizon": func(h history) history {
			h[1].Object.Status.PendingObservation.ObserveAfter = proofTimePointer(20)
			return h
		},
		"missing horizon": func(h history) history { h[1].Object.Status.PendingObservation.ObserveAfter = nil; return h },
		"no transition":   func(h history) history { h[1].Object.Status.Conditions[0].LastTransitionTime = proofTime(1); return h },
		"successful outcome": func(h history) history {
			h[1].Object.Status.PendingObservation.Outcome = ptahv1.PendingObservationApplySucceeded
			return h
		},
		"lost Pod evidence": func(h history) history {
			h[2].Object.Status.PendingObservation.ApplyPodUIDs = nil
			h[2].Object.Status.PendingObservation.ApplyPodCount = 0
			return h
		},
		"new Pod": func(h history) history {
			h[2].Object.Status.PendingObservation.ApplyPodUIDs[0] = "replacement"
			return h
		},
		"new lease epoch":         func(h history) history { h[2].Object.Status.PendingObservation.LeaseEpoch = "other"; return h },
		"Observe while suspended": func(h history) history { h[2].Object.Spec.Suspend = true; return h },
		"Observe before horizon":  func(h history) history { h[2].Object.Status.ActiveOperation.StartedAt = proofTime(620); return h },
		"Observe another target":  func(h history) history { h[3].Object.Status.ActiveOperation.Target.URLFrom.Name = "other"; return h },
		"Observe excludes protected data": func(h history) history {
			h[3].Object.Status.ActiveOperation.ObservationProtectedTables = []string{"other"}
			return h
		},
		"replayed Apply": func(h history) history {
			h[3].Object.Status.ActiveOperation = proofApplyActive()
			h[3].Object.Status.ActiveOperation.ID = "replay"
			return h
		},
		"stale observation":            func(h history) history { h[4].Object.Status.Target.LastObservedAt = proofTimePointer(1); return h },
		"observation predates Observe": func(h history) history { h[4].Object.Status.Target.LastObservedAt = proofTimePointer(635); return h },
		"Plan predates observation":    func(h history) history { h[4].Object.Status.ActiveOperation.StartedAt = proofTime(649); return h },
		"wrong observed identity":      func(h history) history { h[4].Object.Status.Target.IdentityDigest = proofDigest("b"); return h },
		"no required Plan":             func(h history) history { h[4].Object.Status.PendingObservation.PlanRequired = false; return h },
		"resolution without time":      func(h history) history { h[6].Object.Status.LastAttemptTime = nil; return h },
		"resolution before Plan":       func(h history) history { h[6].Object.Status.LastAttemptTime = proofTimePointer(655); return h },
		"resolution while suspended":   func(h history) history { h[6].Object.Spec.Suspend = true; return h },
		"recurring incident":           func(h history) history { return append(h, h[1]) },
		"Apply after clearing": func(h history) history {
			v := h[6].Object.DeepCopy()
			v.Status.ActiveOperation = proofApplyActive()
			return append(h, watchEvent[*ptahv1.PtahSchema]{Object: v})
		},
	} {
		t.Run(name, func(t *testing.T) {
			before, events := alSchemaTraceFixture()
			if _, err := alReadSchemaUnresolvedTrace(mutate(events), before, true); err == nil {
				t.Fatal("corrupt recovery accepted")
			}
		})
	}
}
