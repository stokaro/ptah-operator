package e2e

import (
	"fmt"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const (
	alOverdueAlert        = "PtahOperatorResourceOverdue"
	alOverdueAfter        = 60 * time.Second
	alOverdueFaultMessage = "e2e fault: this resource cannot persist reconciliation"
)

type alOverdueState struct {
	resource  alStalledClaim
	next      time.Time
	observed  int64
	suspended bool
	refused   bool
}

func alOverdueReading(object client.Object) alOverdueState {
	r := alOverdueState{resource: alStalledReading(object)}
	switch object := object.(type) {
	case *ptahv1.PtahSchema:
		r.suspended, r.observed = object.Spec.Suspend, object.Status.ObservedGeneration
		for _, condition := range object.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == metav1.ConditionFalse && condition.Reason == "RealmNotAuthorized" && condition.ObservedGeneration == object.Generation {
				r.refused = true
			}
		}
		if object.Status.NextReconciliationTime != nil {
			r.next = object.Status.NextReconciliationTime.Time
		}
	case *ptahv1.PtahMigration:
		r.suspended, r.observed = object.Spec.Suspend, object.Status.ObservedGeneration
		for _, condition := range object.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == metav1.ConditionFalse && condition.Reason == "RealmNotAuthorized" && condition.ObservedGeneration == object.Generation {
				r.refused = true
			}
		}
		if object.Status.NextReconciliationTime != nil {
			r.next = object.Status.NextReconciliationTime.Time
		}
	}
	return r
}

func (r alOverdueState) scheduled() bool {
	c := r.resource
	return (c.family == "schema" || c.family == "migration") && c.namespace != "" && c.name != "" && c.uid != "" &&
		c.generation > 0 && r.observed == c.generation && !r.suspended && c.phase == "Blocked" && r.refused && c.id == "" && c.jobUID == "" && !r.next.IsZero()
}

// A new persisted Resolve claim clears the old deadline. Its StartedAt dates
// that transition even when the controller finishes it between test polls.
func alOverdueRecovery(history []client.Object, before alOverdueState) (alStalledClaim, bool) {
	if !before.scheduled() {
		return alStalledClaim{}, false
	}
	for _, object := range history {
		r := alOverdueReading(object)
		if before.resource.sameResource(r.resource) && !r.suspended && r.observed == before.observed && r.next.IsZero() &&
			r.resource.ready() && r.resource.id != before.resource.id && r.resource.started.After(before.next) {
			return r.resource, true
		}
	}
	return alStalledClaim{}, false
}

func alOverdueDelivered(delivery alDelivery, deadline time.Time) bool {
	return !deadline.IsZero() && !delivery.StartsAt.Before(deadline.Add(alOverdueAfter)) &&
		!delivery.ReceivedAt.Before(delivery.StartsAt) && !delivery.ReceivedAt.After(deadline.Add(alOverdueAfter+alDetectionSlack))
}

func alOverdueCleared(firing, resolved alDelivery, recovered time.Time) bool {
	return !recovered.IsZero() && !firing.StartsAt.IsZero() && firing.StartsAt.Equal(resolved.StartsAt) &&
		!resolved.EndsAt.Before(recovered) && !resolved.ReceivedAt.Before(resolved.EndsAt) &&
		!resolved.ReceivedAt.After(recovered.Add(alDetectionSlack))
}

func alOverdueDenied(err error, policy string) bool {
	return err != nil && apierrors.IsForbidden(err) && strings.Contains(err.Error(), policy) && strings.Contains(err.Error(), alOverdueFaultMessage)
}

func alOverduePolicy(before alOverdueState) (*admissionv1.ValidatingAdmissionPolicy, *admissionv1.ValidatingAdmissionPolicyBinding) {
	plural := "ptahschemas"
	if before.resource.family == "migration" {
		plural = "ptahmigrations"
	}
	name := "ptah-e2e-overdue-" + string(before.resource.uid)
	policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: admissionv1.ValidatingAdmissionPolicySpec{
		FailurePolicy: ptr.To(admissionv1.Fail),
		MatchConstraints: &admissionv1.MatchResources{ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
			Operations: []admissionv1.OperationType{admissionv1.Update}, Rule: admissionv1.Rule{APIGroups: []string{ptahv1.GroupVersion.Group},
				APIVersions: []string{ptahv1.GroupVersion.Version}, Resources: []string{plural, plural + "/status"}, Scope: ptr.To(admissionv1.NamespacedScope)},
		}}}},
		MatchConditions: []admissionv1.MatchCondition{{Name: "this-resource", Expression: fmt.Sprintf("object.metadata.uid == %q && object.metadata.namespace == %q && object.metadata.name == %q", before.resource.uid, before.resource.namespace, before.resource.name)}},
		Validations:     []admissionv1.Validation{{Expression: "false", Message: alOverdueFaultMessage, Reason: ptr.To(metav1.StatusReasonForbidden)}},
	}}
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
		PolicyName: name, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
	}}
	return policy, binding
}

func alOverdueResource(family string) map[string]any {
	object := alHeldResource(alStalledNamespace, family)
	if object == nil {
		return nil
	}
	object["metadata"].(map[string]any)["name"] = "overdue-" + family
	spec := object["spec"].(map[string]any)
	spec["interval"] = "1m"
	target := spec["target"].(map[string]any)
	delete(target, "coordinationKey")
	target["realmRef"] = map[string]any{"name": alOverdueRealmName(family)}
	execution := spec["execution"].(map[string]any)
	delete(execution, "nodeSelector")
	execution["failureRetryInterval"] = "1h"
	execution["activeDeadlineSeconds"] = int64(30)
	if family == "migration" {
		// Migration requires the lock budget to fit the shortened operation.
		policy, _ := spec["policy"].(map[string]any)
		if policy == nil {
			policy = map[string]any{}
			spec["policy"] = policy
		}
		policy["lockTimeout"] = "30s"
	}
	return object
}

func alOverdueRealmName(family string) string { return "ptah-e2e-overdue-" + family }

// An absent realm gives both controllers an eligible, persisted recheck
// deadline without relying on their different failed-operation retry states.
func alOverdueRealm(family string) *ptahv1.PtahRealm {
	return &ptahv1.PtahRealm{ObjectMeta: metav1.ObjectMeta{Name: alOverdueRealmName(family)}, Spec: ptahv1.PtahRealmSpec{
		Engine: ptahv1.DatabaseEnginePostgreSQL, Namespaces: []string{alStalledNamespace}, Sharing: ptahv1.RealmSharingExclusive,
	}}
}
