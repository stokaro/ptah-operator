package e2e

import (
	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

func negativeFixtureState(t *testing.T, family string, policy ptahv1.ApplyPolicy) client.Object {
	t.Helper()
	var template client.Object = &ptahv1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "producer", UID: "producer-uid"}}
	if family == "migration" {
		template = &ptahv1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "producer", UID: "producer-uid"}}
	}
	object, err := alNegativeFixture(template, "control", "isolated-db", policy)
	if err != nil {
		t.Fatal(err)
	}
	object.SetUID("control-uid")
	object.SetGeneration(1)
	observed := metav1.NewTime(time.Unix(1800000000, 0).UTC())
	condition := metav1.Condition{Type: "ApprovalRequired", Status: metav1.ConditionTrue, ObservedGeneration: 1}
	if policy == ptahv1.ApplyPolicyNever {
		condition = metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "ApplyDisabled", ObservedGeneration: 1}
	}
	switch r := object.(type) {
	case *ptahv1.PtahSchema:
		r.Status = ptahv1.PtahSchemaStatus{ObservedGeneration: 1, Phase: ptahv1.PhaseAwaitingApproval, Target: ptahv1.TargetStatus{LastObservedAt: &observed}, Plan: &ptahv1.CurrentPlanStatus{Name: "real-plan", UID: "real-plan-uid"}, Conditions: []metav1.Condition{condition}}
		if policy == ptahv1.ApplyPolicyNever {
			r.Status.Phase = ptahv1.PhaseBlocked
		}
	case *ptahv1.PtahMigration:
		r.Status = ptahv1.PtahMigrationStatus{ObservedGeneration: 1, Phase: ptahv1.MigrationPhaseAwaitingApproval, History: &ptahv1.MigrationHistoryStatus{ObservedAt: observed}, Plan: &ptahv1.ImmutableObjectReference{Name: "real-plan", UID: "real-plan-uid"}, Conditions: []metav1.Condition{condition}}
		if policy == ptahv1.ApplyPolicyNever {
			r.Status.Phase = ptahv1.MigrationPhasePlanning
		}
	}
	return object
}

func TestAlNegativeControlsRequireRealPolicyGatesAndNoApply(t *testing.T) {
	t.Parallel()
	if alNegativeWindow != 10*time.Minute || alNegativeInterval != 2*time.Minute {
		t.Fatal("negative-control scope changed")
	}
	for _, family := range []string{"schema", "migration"} {
		for _, policy := range []ptahv1.ApplyPolicy{ptahv1.ApplyPolicyOnApproval, ptahv1.ApplyPolicyNever} {
			t.Run(family+"/"+string(policy), func(t *testing.T) {
				object := negativeFixtureState(t, family, policy)
				state := alNegativeReading(object)
				if !state.gated() {
					t.Fatal("native policy gate shape rejected")
				}
				for name, mutate := range map[string]func(*alNegativeState){
					"retry instead of ordinary wait": func(r *alNegativeState) { r.failed = true },
					"suspended":                      func(r *alNegativeState) { r.suspended = true },
					"unobserved generation":          func(r *alNegativeState) { r.observed = 0 },
					"no generation":                  func(r *alNegativeState) { r.claim.generation = 0; r.observed = 0 },
					"applied":                        func(r *alNegativeState) { r.applied = true },
					"unresolved":                     func(r *alNegativeState) { r.unresolved = true },
					"approved":                       func(r *alNegativeState) { r.approved = true },
					"automatic":                      func(r *alNegativeState) { r.policy = ptahv1.ApplyPolicyAlways },
					"cadence disabled":               func(r *alNegativeState) { r.interval = 24 * time.Hour },
					"apply claim":                    func(r *alNegativeState) { r.claim.id = "forbidden"; r.claim.operation = "Apply" },
					"failed":                         func(r *alNegativeState) { r.claim.phase = "Failed" },
				} {
					changed := state
					mutate(&changed)
					if changed.safe() || changed.gated() {
						t.Errorf("%s accepted", name)
					}
				}
				for name, mutate := range map[string]func(*alNegativeState){
					"missing plan":         func(r *alNegativeState) { r.plan = false },
					"no observed database": func(r *alNegativeState) { r.readAt = time.Time{} },
					"no decision":          func(r *alNegativeState) { r.waiting = false; r.disabled = false },
					"active read":          func(r *alNegativeState) { r.claim.id = "read"; r.claim.operation = "Observe" },
				} {
					changed := state
					mutate(&changed)
					if changed.gated() {
						t.Errorf("%s counted as an initial gate", name)
					}
				}
				operations := []string{"Resolve", "Verify", "History"}
				if family == "schema" {
					operations = []string{"Resolve", "Verify", "Observe", "Plan"}
				}
				for _, operation := range operations {
					transient := state
					transient.claim.id = "fresh-read"
					transient.claim.operation = operation
					transient.claim.phase = "Resolving"
					transient.plan = false
					if !transient.safe() {
						t.Errorf("normal %s refresh rejected", operation)
					}
				}
			})
		}
	}
}

func TestAlNegativeFixtureDoesNotInheritAnAcceptedStatus(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"schema", "migration"} {
		template := negativeFixtureState(t, family, ptahv1.ApplyPolicyOnApproval)
		object, err := alNegativeFixture(template, "fresh-control", "fresh-db", ptahv1.ApplyPolicyNever)
		if err != nil {
			t.Fatal(err)
		}
		if object.GetUID() != "" || object.GetGeneration() != 0 || alNegativeReading(object).gated() {
			t.Fatal("fixture inherited proof from its producer")
		}
		switch v := object.(type) {
		case *ptahv1.PtahSchema:
			if v.Spec.Target.URLFrom.Name != "fresh-db" || v.Spec.Target.RealmRef != nil || v.Spec.Policy.Apply != ptahv1.ApplyPolicyNever {
				t.Fatal("schema is not isolated")
			}
		case *ptahv1.PtahMigration:
			if v.Spec.Target.URLFrom.Name != "fresh-db" || v.Spec.Target.RealmRef != nil || v.Spec.Policy.Apply != ptahv1.ApplyPolicyNever {
				t.Fatal("migration is not isolated")
			}
		}
		if template.GetUID() != "control-uid" || !alNegativeReading(template).gated() {
			t.Fatal("producer was modified")
		}
	}
}

func TestAlNegativeWindowAllowsOnlyTheSameExistingIncident(t *testing.T) {
	t.Parallel()
	old := alDelivery{Receiver: "sink", Status: "firing", AlertName: alUnresolvedApply, StartsAt: time.Unix(1800000000, 0).UTC(), Labels: map[string]string{"family": "migration", "operator_namespace": "installation", "severity": "critical"}, Annotations: map[string]string{"summary": "1 unresolved migration"}}
	baseline := map[string]alDelivery{"migration": old}
	if !alNegativeRepeat(old, baseline) {
		t.Fatal("unchanged baseline notification rejected")
	}
	for name, mutate := range map[string]func(*alDelivery){
		"new incident":    func(d *alDelivery) { d.StartsAt = d.StartsAt.Add(time.Second) },
		"new rule":        func(d *alDelivery) { d.AlertName = alOperationStall },
		"resolved":        func(d *alDelivery) { d.Status = "resolved" },
		"other receiver":  func(d *alDelivery) { d.Receiver = "other" },
		"different count": func(d *alDelivery) { d.Annotations = map[string]string{"summary": "2 unresolved migrations"} },
		"other family":    func(d *alDelivery) { d.Labels = map[string]string{"family": "schema"} },
		"other installation": func(d *alDelivery) {
			d.Labels = map[string]string{"family": "migration", "operator_namespace": "other", "severity": "critical"}
		},
	} {
		changed := old
		mutate(&changed)
		if alNegativeRepeat(changed, baseline) {
			t.Errorf("%s excluded from the quiet proof", name)
		}
	}
	if alNegativeRepeat(old, nil) {
		t.Fatal("absent baseline accepted")
	}
}

func TestAlNegativeReadOnlyRetryIsNotAnOrdinaryWait(t *testing.T) {
	t.Parallel()
	object := negativeFixtureState(t, "migration", ptahv1.ApplyPolicyOnApproval).(*ptahv1.PtahMigration)
	next := metav1.NewTime(time.Unix(1800000100, 0).UTC())
	object.Status.Phase = ptahv1.MigrationPhaseResolving
	object.Status.ActiveOperation = &ptahv1.MigrationOperationStatus{ID: "retry", Type: ptahv1.MigrationOperationResolve, Attempt: 2, RetryNotBefore: &next}
	if alNegativeReading(object).safe() {
		t.Fatal("a deferred failed operation counted as normal policy waiting")
	}
}

func TestAlNegativeDirectReadRejectsAReplacedControl(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"schema", "migration"} {
		for _, policy := range []ptahv1.ApplyPolicy{ptahv1.ApplyPolicyOnApproval, ptahv1.ApplyPolicyNever} {
			t.Run(family+"/"+string(policy), func(t *testing.T) {
				object := negativeFixtureState(t, family, policy)
				before := alNegativeReading(object)
				if !before.unchanged(before) {
					t.Fatal("original control rejected")
				}
				// A replacement can carry the same name, generation and valid
				// policy-gate status. Its new UID must still invalidate the row.
				object.SetUID("replacement-uid")
				after := alNegativeReading(object)
				if !after.gated() {
					t.Fatal("replacement is not an otherwise valid gate")
				}
				if after.unchanged(before) {
					t.Fatal("replacement counted as the original control")
				}
			})
		}
	}
}
