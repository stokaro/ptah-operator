package e2e

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func overdueFixture(t *testing.T, family string) client.Object {
	t.Helper()
	raw, err := json.Marshal(alOverdueResource(family))
	if err != nil {
		t.Fatal(err)
	}
	var object client.Object = &ptahv1.PtahSchema{}
	if family == "migration" {
		object = &ptahv1.PtahMigration{}
	}
	if err := json.Unmarshal(raw, object); err != nil {
		t.Fatal(err)
	}
	object.SetUID("overdue-uid")
	object.SetGeneration(2)
	deadline := metav1.NewTime(time.Unix(1800000000, 0).UTC())
	conditions := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "RealmNotAuthorized", ObservedGeneration: 2}}
	switch r := object.(type) {
	case *ptahv1.PtahSchema:
		r.Status = ptahv1.PtahSchemaStatus{Phase: ptahv1.PhaseBlocked, ObservedGeneration: 2, NextReconciliationTime: &deadline, Conditions: conditions}
		if r.Spec.Target.RealmRef == nil || r.Spec.Target.CoordinationKey != "" {
			t.Fatal("fixture cannot reach the realm refusal")
		}
	case *ptahv1.PtahMigration:
		r.Status = ptahv1.PtahMigrationStatus{Phase: ptahv1.MigrationPhaseBlocked, ObservedGeneration: 2, NextReconciliationTime: &deadline, Conditions: conditions}
		if r.Spec.Target.RealmRef == nil || r.Spec.Target.CoordinationKey != "" {
			t.Fatal("fixture cannot reach the realm refusal")
		}
	}
	return object
}

func TestOverdueNeedsAnEligibleDeadlineAndANewNativeClaim(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			object := overdueFixture(t, family)
			before := alOverdueReading(object)
			if !before.scheduled() {
				t.Fatal("eligible refusal deadline rejected")
			}
			for _, mutate := range []func(*alOverdueState){
				func(r *alOverdueState) { r.suspended = true }, func(r *alOverdueState) { r.observed-- },
				func(r *alOverdueState) { r.next = time.Time{} }, func(r *alOverdueState) { r.refused = false },
				func(r *alOverdueState) { r.resource.id = "active" }, func(r *alOverdueState) { r.resource.phase = "Failed" },
			} {
				bad := before
				mutate(&bad)
				if bad.scheduled() {
					t.Fatal("ineligible resource accepted")
				}
			}
			recovered := object.DeepCopyObject().(client.Object)
			started := metav1.NewTime(before.next.Add(2 * time.Minute))
			switch r := recovered.(type) {
			case *ptahv1.PtahSchema:
				r.Status.Phase = ptahv1.PhaseResolving
				r.Status.NextReconciliationTime = nil
				r.Status.ActiveOperation = &ptahv1.ActiveOperationStatus{ID: "new", Type: ptahv1.OperationResolve, StartedAt: started, JobName: "new-job", JobUID: "new-job-uid"}
			case *ptahv1.PtahMigration:
				r.Status.Phase = ptahv1.MigrationPhaseResolving
				r.Status.NextReconciliationTime = nil
				r.Status.ActiveOperation = &ptahv1.MigrationOperationStatus{ID: "new", Type: ptahv1.MigrationOperationResolve, StartedAt: started, JobName: "new-job", JobUID: "new-job-uid"}
			}
			got, ok := alOverdueRecovery([]client.Object{object, recovered}, before)
			if !ok || !got.started.Equal(started.Time) {
				t.Fatal("recovery did not use the actual new claim's timestamp")
			}
			if _, ok := alOverdueRecovery([]client.Object{object}, before); ok {
				t.Fatal("unchanged deadline counted as recovery")
			}
			recovered.SetUID("replacement")
			if _, ok := alOverdueRecovery([]client.Object{recovered}, before); ok {
				t.Fatal("replacement resource counted as recovery")
			}
		})
	}
}

func TestOverdueFaultMatchesOnlyItsExactResourceAndBothWritePaths(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"schema", "migration"} {
		state := alOverdueReading(overdueFixture(t, family))
		policy, binding := alOverduePolicy(state)
		rules := policy.Spec.MatchConstraints.ResourceRules
		plural := "ptahschemas"
		if family == "migration" {
			plural = "ptahmigrations"
		}
		if len(rules) != 1 || len(rules[0].Resources) != 2 || rules[0].Resources[0] != plural || rules[0].Resources[1] != plural+"/status" {
			t.Fatal("fault does not hold root and status writes")
		}
		expression := policy.Spec.MatchConditions[0].Expression
		for _, identity := range []string{string(state.resource.uid), state.resource.namespace, state.resource.name} {
			if !strings.Contains(expression, identity) {
				t.Fatal("fault omitted a resource identity")
			}
		}
		if binding.Spec.PolicyName != policy.Name {
			t.Fatal("unbound write fault")
		}
		matching := apierrors.NewForbidden(schema.GroupResource{Group: ptahv1.GroupVersion.Group, Resource: plural}, state.resource.name, errors.New(policy.Name+": "+alOverdueFaultMessage))
		if !alOverdueDenied(matching, policy.Name) || alOverdueDenied(matching, "different-policy") {
			t.Fatal("refusal attribution is wrong")
		}
		if alOverdueDenied(errors.New(matching.Error()), policy.Name) {
			t.Fatal("text without a Forbidden API status passed")
		}
	}
}

func TestOverdueNotificationsUsePersistedDeadlineAndNativeRecovery(t *testing.T) {
	t.Parallel()
	deadline := time.Unix(1800000000, 500000000).UTC()
	firing := alDelivery{StartsAt: deadline.Add(60 * time.Second), ReceivedAt: deadline.Add(105 * time.Second)}
	if !alOverdueDelivered(firing, deadline) {
		t.Fatal("frozen boundary rejected")
	}
	late := firing
	late.ReceivedAt = late.ReceivedAt.Add(time.Nanosecond)
	early := firing
	early.StartsAt = early.StartsAt.Add(-time.Nanosecond)
	if alOverdueDelivered(late, deadline) || alOverdueDelivered(early, deadline) {
		t.Fatal("late or premature alert passed")
	}
	recovery := deadline.Add(3 * time.Minute)
	resolved := alDelivery{StartsAt: firing.StartsAt, EndsAt: recovery.Add(time.Second), ReceivedAt: recovery.Add(45 * time.Second)}
	if !alOverdueCleared(firing, resolved, recovery) {
		t.Fatal("native recovery rejected")
	}
	resolved.ReceivedAt = resolved.ReceivedAt.Add(time.Nanosecond)
	if alOverdueCleared(firing, resolved, recovery) {
		t.Fatal("late resolution passed")
	}
}
