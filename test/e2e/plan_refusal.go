package e2e

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// viewSecurityRefusal is what Ready says of a Plan Ptah refused because the
// desired schema leaves a PostgreSQL view's security_invoker unstated while
// the database's view holds it: the code, the view in its schema and the
// option, and nothing of Ptah's sentence, which the runner writes to the Plan
// Pod's log and to nothing the frame carries (internal/runner, refusePlan).
func viewSecurityRefusal(view string) string {
	return runner.CodePlanRefused + ": ptah refused to plan the change (view-security-option: public." + view + " security_invoker)"
}

// viewSecurityPlanRefused is a schema stopped on the Plan of generation, which
// Ptah refused for view's security_invoker: Ready False with PlanRefused,
// written for that generation and saying viewSecurityRefusal, in phase Blocked
// with no claim, no plan and no failure standing. DatabaseReachable is true,
// since Ptah refuses a plan only once it has compared the schema with the
// database. Ready's generation is what ties the refusal to the spec that
// asked for the plan, rather than to an earlier stop. It returns Ready's
// message.
func viewSecurityPlanRefused(schema *ptahv1alpha1.PtahSchema, view string, generation int64) (string, error) {
	message, err := schemaRefused(schema, ptahv1alpha1.ReasonPlanRefused)
	if err != nil {
		return "", err
	}
	ready := meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionReady)
	reachable := meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionDatabaseReachable)
	switch {
	case ready.ObservedGeneration != generation:
		return "", fmt.Errorf("Ready was written for generation %d, not the refused spec's %d", ready.ObservedGeneration, generation)
	case message != viewSecurityRefusal(view):
		return "", fmt.Errorf("Ready says %q, want %q", message, viewSecurityRefusal(view))
	case reachable == nil || reachable.Status != metav1.ConditionTrue || reachable.Reason != string(ptahv1alpha1.ReasonObserved):
		return "", fmt.Errorf("DatabaseReachable %+v beside the refusal, which opened the database", reachable)
	}
	return message, nil
}

// planRefusalLifted is a schema that left a refused plan behind once the edit
// of generation removed the cause: phase InSync on digest with InSync True and
// ScopedConverged, no claim, pending observation or lock release, Ready no
// longer naming the refusal, status and Ready both observing generation, and
// an attempt later than the refused one at refusedAt. It holds no Applied
// record. The Resolve that saw the source change cleared it, and only the proof
// after an Apply writes one, which the edit did not need. The attempt is the
// object's own stamp of the read that cleared the stop, so a reading the
// refusal never left, or one stamped before it, is refused.
func planRefusalLifted(schema *ptahv1alpha1.PtahSchema, digest string, generation int64, refusedAt *metav1.Time) error {
	status := schema.Status
	ready := meta.FindStatusCondition(status.Conditions, ptahv1alpha1.ConditionReady)
	switch {
	case status.Phase != ptahv1alpha1.PhaseInSync || status.Source.Digest != digest:
		return fmt.Errorf("phase %s from source %q, not InSync on %s", status.Phase, status.Source.Digest, digest)
	case !conditionIs(status.Conditions, ptahv1alpha1.ConditionInSync, metav1.ConditionTrue, string(ptahv1alpha1.ReasonScopedConverged)):
		return fmt.Errorf("InSync %+v, want True with ScopedConverged",
			meta.FindStatusCondition(status.Conditions, ptahv1alpha1.ConditionInSync))
	case status.ActiveOperation != nil || status.PendingObservation != nil || status.PendingLockRelease != nil:
		return fmt.Errorf("work in flight: claim %+v, pending observation %+v, lock release %+v",
			status.ActiveOperation, status.PendingObservation, status.PendingLockRelease)
	case status.Applied != nil:
		return fmt.Errorf("an Apply is recorded on %s, and the edit needed none", status.Applied.ArtifactDigest)
	case status.ObservedGeneration != generation:
		return fmt.Errorf("status observes generation %d, not the edit's %d", status.ObservedGeneration, generation)
	case ready == nil || ready.ObservedGeneration != generation ||
		(ready.Status == metav1.ConditionFalse && ready.Reason == string(ptahv1alpha1.ReasonPlanRefused)):
		return fmt.Errorf("Ready %+v after the edit of generation %d", ready, generation)
	case refusedAt == nil || status.LastAttemptTime == nil || !status.LastAttemptTime.After(refusedAt.Time):
		return fmt.Errorf("the last attempt %v is not after the refusal at %v", status.LastAttemptTime, refusedAt)
	}
	return nil
}
