package e2e

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
)

// The hold-to-term rows remove a running Apply so that its Pod leaves the API
// server with nothing stored to say its executor stopped, which is the case the
// hold exists for: a Pod deleted with --force while its node does not answer.
//
// Deleting the Job with its Pods does not get there on its own. The kubelet
// stores the Pod Failed, its executor terminated, before it lets the Pod go,
// and a pass that reads that state hands the realm Lease back at once, as it
// should; which state a pass reads is a race. Deleting the Pod with no grace
// first does not get there either: the Job controller keeps its tracking
// finalizer until the kubelet stores the Pod Failed (podReplacementPolicy
// Failed), and the Job then stands Failed, so the manager reads the result the
// runner delivered and settles the run from it.
//
// So the rows refuse status writes to that one Pod while they remove it, as a
// node the API server cannot hear from would, delete the Job with its Pods,
// and delete the Pod with no grace once the Job is gone. The Job controller
// drops the finalizer of a deleted Job's Pods whatever their phase
// (handleSingleOrphanPod), and the kubelet deletes a Pod only after its
// terminal status is stored (status manager syncPod), so the Pod goes only
// through the forced delete, and no stored state of it says it stopped.

// applyExecutorContainer is the container of an Apply Pod that runs the
// executor.
const applyExecutorContainer = "ptah"

// unstoredStopMessage is what the policy unstoredStopPolicy builds says when
// it refuses a status write.
const unstoredStopMessage = "the e2e suite keeps the stop of this Apply Pod out of the API server"

// unstoredStopLabels mark the policy and binding unstoredStopPolicy builds, so
// that one a test binary left behind when it died can be found by label.
var unstoredStopLabels = map[string]string{
	"app.kubernetes.io/part-of":   "ptah-e2e",
	"app.kubernetes.io/component": "e2e-unstored-stop",
}

// unstoredStopPolicy is the ValidatingAdmissionPolicy, with its binding, that
// refuses every status write to one Pod, named by namespace, name and UID.
// Nothing but the Pod's kubelet writes that Pod's status here: the Job
// controller and the garbage collector change its metadata, and PodGC writes
// status only for a Pod whose node is gone or out of service.
func unstoredStopPolicy(pod *corev1.Pod) (*admissionv1.ValidatingAdmissionPolicy, *admissionv1.ValidatingAdmissionPolicyBinding) {
	name := "ptah-e2e-unstored-stop-" + string(pod.UID)
	policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: maps.Clone(unstoredStopLabels)}, Spec: admissionv1.ValidatingAdmissionPolicySpec{
		FailurePolicy: ptr.To(admissionv1.Fail),
		MatchConstraints: &admissionv1.MatchResources{ResourceRules: []admissionv1.NamedRuleWithOperations{{
			ResourceNames: []string{pod.Name},
			RuleWithOperations: admissionv1.RuleWithOperations{
				Operations: []admissionv1.OperationType{admissionv1.Update},
				Rule: admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"},
					Resources: []string{"pods/status"}, Scope: ptr.To(admissionv1.NamespacedScope)},
			},
		}}},
		MatchConditions: []admissionv1.MatchCondition{{Name: "this-pod", Expression: fmt.Sprintf(
			"object.metadata.uid == %q && object.metadata.namespace == %q && object.metadata.name == %q",
			pod.UID, pod.Namespace, pod.Name)}},
		Validations: []admissionv1.Validation{{Expression: "false", Message: unstoredStopMessage, Reason: ptr.To(metav1.StatusReasonForbidden)}},
	}}
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: maps.Clone(unstoredStopLabels)}, Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
		PolicyName: name, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
	}}
	return policy, binding
}

// unstoredStopRefusalHold is how long the status probe has to stay refused
// before the Job is deleted: two of the one-second ticks on which each API
// server compiles the admission policies it has seen (policyRefreshInterval in
// k8s.io/apiserver's policy_source.go).
const unstoredStopRefusalHold = 2 * time.Second

// unstoredStopRefusal counts how long the status probe has been refused
// without a break by the policy named policy.
type unstoredStopRefusal struct {
	policy string
	since  time.Time
}

// observe takes one probe's result, read at now. It reports whether the
// refusal has held for unstoredStopRefusalHold, what it saw where it has not,
// and the error of a probe that failed for another reason. An admitted probe
// starts the count over.
func (r *unstoredStopRefusal) observe(err error, now time.Time) (bool, string, error) {
	switch {
	case err == nil:
		r.since = time.Time{}
		return false, "the policy " + r.policy + " does not refuse the probe yet", nil
	case !unstoredStopDenied(err, r.policy):
		return false, "", err
	case r.since.IsZero():
		r.since = now
	}
	if now.Sub(r.since) >= unstoredStopRefusalHold {
		return true, "", nil
	}
	return false, "the policy " + r.policy + " has refused the probe for under " + unstoredStopRefusalHold.String(), nil
}

// unstoredStopDenied reports whether err is the refusal of the policy named
// policy: a Forbidden API status that names the policy and carries its
// message.
func unstoredStopDenied(err error, policy string) bool {
	return apierrors.IsForbidden(err) &&
		strings.Contains(err.Error(), policy) && strings.Contains(err.Error(), unstoredStopMessage)
}

// storedStop says how a stored state of an Apply Pod says its executor
// stopped, and is empty where it does not: a terminal phase, or the executor
// container terminated.
func storedStop(pod *corev1.Pod) string {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return "in phase " + string(pod.Status.Phase)
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == applyExecutorContainer && status.State.Terminated != nil {
			return "with its executor container terminated"
		}
	}
	return ""
}

// unstoredStopHeld checks what a Pod watch recorded of the Pod uid through its
// removal: at least one stored state of it, its deletion last, and no state
// that says its executor stopped (storedStop). It returns how many states of
// the Pod it read. Other Pods, and bookmarks, which carry no UID, are skipped.
func unstoredStopHeld(history []watchEvent[*corev1.Pod], uid types.UID) (int, error) {
	if uid == "" {
		return 0, errors.New("no Pod UID to read the history for")
	}
	states, deleted := 0, false
	for _, event := range history {
		pod := event.Object
		if pod == nil || pod.UID != uid {
			continue
		}
		if deleted {
			return states, fmt.Errorf("Pod %s has a stored state at resourceVersion %s after its deletion", uid, pod.ResourceVersion)
		}
		states++
		if stop := storedStop(pod); stop != "" {
			return states, fmt.Errorf("Pod %s/%s was stored %s at resourceVersion %s", pod.Namespace, pod.Name, stop, pod.ResourceVersion)
		}
		deleted = event.Type == watch.Deleted
	}
	switch {
	case states == 0:
		return 0, fmt.Errorf("the watch recorded no state of Pod %s", uid)
	case !deleted:
		return states, fmt.Errorf("the watch did not record the deletion of Pod %s", uid)
	}
	return states, nil
}

// unstoredStopStates lists the stored states of the Pod uid in history, one
// entry each: the event, the resourceVersion, the phase, the deletion grace
// where it was being deleted, its finalizers, and the executor's state.
func unstoredStopStates(history []watchEvent[*corev1.Pod], uid types.UID) string {
	var states []string
	for _, event := range history {
		pod := event.Object
		if pod == nil || pod.UID != uid {
			continue
		}
		executor := "unreported"
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != applyExecutorContainer {
				continue
			}
			switch {
			case status.State.Terminated != nil:
				executor = fmt.Sprintf("terminated(%d)", status.State.Terminated.ExitCode)
			case status.State.Running != nil:
				executor = "running"
			case status.State.Waiting != nil:
				executor = "waiting"
			}
		}
		deleting := "no"
		switch {
		case pod.DeletionTimestamp != nil && pod.DeletionGracePeriodSeconds != nil:
			deleting = fmt.Sprintf("grace %ds", *pod.DeletionGracePeriodSeconds)
		case pod.DeletionTimestamp != nil:
			deleting = "yes"
		}
		states = append(states, fmt.Sprintf("%s rv=%s phase=%s deleting=%s finalizers=%v executor=%s",
			event.Type, pod.ResourceVersion, pod.Status.Phase, deleting, pod.Finalizers, executor))
	}
	return "[" + strings.Join(states, "; ") + "]"
}
