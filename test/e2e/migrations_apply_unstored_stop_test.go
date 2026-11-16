package e2e

import (
	"errors"
	"strings"
	"testing"
	"time"

	celgo "github.com/google/cel-go/cel"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fieldpath "k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/watch"
)

func unstoredStopPod(uid types.UID, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ptah-e2e", Name: name, UID: uid, ResourceVersion: "100"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{Name: applyExecutorContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}},
	}
}

func TestUnstoredStopPolicyRefusesStatusWritesToItsPodAlone(t *testing.T) {
	t.Parallel()
	pod := unstoredStopPod("pod-uid", "apply-1")
	policy, binding := unstoredStopPolicy(pod)
	rules := policy.Spec.MatchConstraints.ResourceRules
	if len(rules) != 1 || len(rules[0].Resources) != 1 || rules[0].Resources[0] != "pods/status" ||
		len(rules[0].APIGroups) != 1 || rules[0].APIGroups[0] != "" ||
		len(rules[0].Operations) != 1 || rules[0].Operations[0] != admissionv1.Update ||
		len(rules[0].ResourceNames) != 1 || rules[0].ResourceNames[0] != pod.Name {
		t.Fatalf("the policy matches %+v, want UPDATE of the status of Pod %s alone", rules, pod.Name)
	}
	if *policy.Spec.FailurePolicy != admissionv1.Fail || len(policy.Spec.Validations) != 1 ||
		policy.Spec.Validations[0].Expression != "false" || policy.Spec.Validations[0].Message != unstoredStopMessage {
		t.Fatalf("the policy validates %+v, want one refusal that carries its message", policy.Spec.Validations)
	}
	if binding.Name != policy.Name || binding.Spec.PolicyName != policy.Name ||
		len(binding.Spec.ValidationActions) != 1 || binding.Spec.ValidationActions[0] != admissionv1.Deny {
		t.Fatalf("the binding %s binds %s with %v, want the policy with Deny", binding.Name, binding.Spec.PolicyName, binding.Spec.ValidationActions)
	}
	// A policy a dead test binary left behind is found by these labels.
	for _, labels := range []map[string]string{policy.Labels, binding.Labels} {
		if labels["app.kubernetes.io/part-of"] != "ptah-e2e" || labels["app.kubernetes.io/component"] != "e2e-unstored-stop" {
			t.Fatalf("the policy and binding carry %v and %v, want the ptah-e2e unstored-stop labels on both", policy.Labels, binding.Labels)
		}
	}
	policy.Labels["app.kubernetes.io/component"] = "changed-policy"
	binding.Labels["app.kubernetes.io/component"] = "changed-binding"
	if policy.Labels["app.kubernetes.io/component"] != "changed-policy" || unstoredStopLabels["app.kubernetes.io/component"] != "e2e-unstored-stop" {
		t.Fatalf("the policy and binding share a labels map with each other or the package: %v, %v, %v", policy.Labels, binding.Labels, unstoredStopLabels)
	}

	environment, err := celgo.NewEnv(celgo.Variable("object", celgo.DynType))
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.MatchConditions) != 1 {
		t.Fatalf("the policy has %d match conditions, want 1", len(policy.Spec.MatchConditions))
	}
	ast, issues := environment.Compile(policy.Spec.MatchConditions[0].Expression)
	if issues != nil && issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		edit func(*corev1.Pod)
		want bool
	}{
		{name: "the Pod itself", edit: func(*corev1.Pod) {}, want: true},
		{name: "another Pod under its name", edit: func(p *corev1.Pod) { p.UID = "replacement-uid" }, want: false},
		{name: "another Pod with its UID's name elsewhere", edit: func(p *corev1.Pod) { p.Namespace = "other" }, want: false},
		{name: "a sibling Pod", edit: func(p *corev1.Pod) { p.Name, p.UID = "apply-2", "sibling-uid" }, want: false},
		{name: "its UID under another name", edit: func(p *corev1.Pod) { p.Name = "apply-2" }, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := pod.DeepCopy()
			test.edit(candidate)
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(candidate)
			if err != nil {
				t.Fatal(err)
			}
			value, _, err := program.Eval(map[string]any{"object": object})
			if err != nil {
				t.Fatal(err)
			}
			if got := value.Value(); got != test.want {
				t.Fatalf("the match condition reads %v, want %v", got, test.want)
			}
		})
	}
}

func TestUnstoredStopDeniedNamesThePolicyAndItsMessage(t *testing.T) {
	t.Parallel()
	const policy = "ptah-e2e-unstored-stop-pod-uid"
	resource := schema.GroupResource{Resource: "pods"}
	refusal := apierrors.NewForbidden(resource, "apply-1", errors.New("ValidatingAdmissionPolicy '"+policy+"' with binding '"+policy+"' denied request: "+unstoredStopMessage))
	if !unstoredStopDenied(refusal, policy) {
		t.Fatalf("the policy's own refusal was not recognized: %v", refusal)
	}
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "no error", err: nil},
		{name: "another policy's refusal", err: apierrors.NewForbidden(resource, "apply-1", errors.New("ValidatingAdmissionPolicy 'other' denied request: "+unstoredStopMessage))},
		{name: "a refusal without the message", err: apierrors.NewForbidden(resource, "apply-1", errors.New("ValidatingAdmissionPolicy '"+policy+"' denied request: no"))},
		{name: "the text without an API status", err: errors.New(refusal.Error())},
		{name: "an invalid write", err: apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "apply-1", nil)},
		{name: "an invalid write that carries the refusal's text", err: apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "apply-1",
			fieldpath.ErrorList{fieldpath.Forbidden(fieldpath.NewPath("status"), policy+": "+unstoredStopMessage)})},
	} {
		if unstoredStopDenied(test.err, policy) {
			t.Errorf("%s was read as the policy's refusal: %v", test.name, test.err)
		}
	}
}

func TestUnstoredStopRefusalHoldsTwoTicksWithoutABreak(t *testing.T) {
	t.Parallel()
	const policy = "ptah-e2e-unstored-stop-pod-uid"
	resource := schema.GroupResource{Resource: "pods"}
	refused := apierrors.NewForbidden(resource, "apply-1", errors.New("ValidatingAdmissionPolicy '"+policy+"' denied request: "+unstoredStopMessage))
	other := apierrors.NewConflict(resource, "apply-1", errors.New("the object has been modified"))
	start := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	type probe struct {
		after time.Duration
		err   error
		done  bool
	}
	for _, test := range []struct {
		name   string
		probes []probe
	}{
		{name: "admitted probes never end the wait", probes: []probe{
			{0, nil, false}, {time.Second, nil, false}, {3 * time.Second, nil, false},
		}},
		{name: "a refusal ends the wait once it has held two ticks", probes: []probe{
			{0, refused, false}, {time.Second, refused, false}, {2*time.Second - time.Millisecond, refused, false}, {2 * time.Second, refused, true},
		}},
		{name: "a refusal counts from the first probe it answered", probes: []probe{
			{0, nil, false}, {500 * time.Millisecond, refused, false}, {2 * time.Second, refused, false}, {2500 * time.Millisecond, refused, true},
		}},
		{name: "an admitted probe starts the count over", probes: []probe{
			{0, refused, false}, {1500 * time.Millisecond, nil, false}, {1750 * time.Millisecond, refused, false},
			{3 * time.Second, refused, false}, {3750 * time.Millisecond, refused, true},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			refusal := unstoredStopRefusal{policy: policy}
			for _, p := range test.probes {
				done, saw, err := refusal.observe(p.err, start.Add(p.after))
				if err != nil {
					t.Fatalf("at %s the probe's %v ended the wait with %v", p.after, p.err, err)
				}
				if done != p.done {
					t.Fatalf("at %s after %v the wait read done %t (%q), want %t", p.after, p.err, done, saw, p.done)
				}
				if !done && !strings.Contains(saw, policy) {
					t.Fatalf("at %s the wait says %q, which does not name the policy", p.after, saw)
				}
			}
		})
	}
	t.Run("a probe that failed for another reason ends the wait with its error", func(t *testing.T) {
		t.Parallel()
		refusal := unstoredStopRefusal{policy: policy}
		if done, _, err := refusal.observe(refused, start); done || err != nil {
			t.Fatalf("the first refusal read done %t with %v", done, err)
		}
		if done, _, err := refusal.observe(other, start.Add(3*time.Second)); done || !errors.Is(err, other) {
			t.Fatalf("a conflict read done %t with %v, want the conflict", done, err)
		}
	})
}

// unstoredStopHistory is the history of one Apply Pod through its removal,
// and of a sibling: the garbage collector's delete, the Job controller's
// finalizer removal, and the deletion.
func unstoredStopHistory() []watchEvent[*corev1.Pod] {
	running := unstoredStopPod("pod-uid", "apply-1")
	running.Finalizers = []string{"batch.kubernetes.io/job-tracking"}
	deleting := running.DeepCopy()
	deleting.ResourceVersion = "101"
	deleting.DeletionTimestamp = &metav1.Time{}
	grace := int64(30)
	deleting.DeletionGracePeriodSeconds = &grace
	released := deleting.DeepCopy()
	released.ResourceVersion = "102"
	released.Finalizers = nil
	sibling := unstoredStopPod("sibling-uid", "apply-2")
	sibling.Status.Phase = corev1.PodFailed
	gone := released.DeepCopy()
	gone.ResourceVersion = "104"
	return []watchEvent[*corev1.Pod]{
		{Type: watch.Modified, Object: deleting},
		{Type: watch.Bookmark, Object: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "101"}}},
		{Type: watch.Modified, Object: released},
		{Type: watch.Modified, Object: sibling},
		{Type: watch.Deleted, Object: gone},
	}
}

func TestUnstoredStopHeldAcceptsAPodThatWentRunning(t *testing.T) {
	t.Parallel()
	states, err := unstoredStopHeld(unstoredStopHistory(), "pod-uid")
	if err != nil || states != 3 {
		t.Fatalf("unstoredStopHeld = %d, %v; want 3 states and no error", states, err)
	}
	if got := unstoredStopStates(unstoredStopHistory(), "pod-uid"); strings.Count(got, "rv=") != 3 ||
		!strings.Contains(got, "MODIFIED rv=101 phase=Running deleting=grace 30s finalizers=[batch.kubernetes.io/job-tracking] executor=running") ||
		!strings.Contains(got, "DELETED rv=104") || strings.Contains(got, "sibling") {
		t.Fatalf("the states of the Pod read %s", got)
	}
	// Only the executor's state counts: another container that stopped says
	// nothing about the SQL.
	history := unstoredStopHistory()
	history[2].Object.Status.ContainerStatuses = append(history[2].Object.Status.ContainerStatuses, corev1.ContainerStatus{
		Name: "other", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}},
	})
	if _, err := unstoredStopHeld(history, "pod-uid"); err != nil {
		t.Fatalf("a Pod whose executor runs was read as stopped: %v", err)
	}
}

func TestUnstoredStopHeldRefusesAStoredStop(t *testing.T) {
	t.Parallel()
	// What a Job deleted with its Pods stores: the kubelet records the Pod
	// Failed, its executor terminated, before the Pod goes.
	failed := func(history []watchEvent[*corev1.Pod]) []watchEvent[*corev1.Pod] {
		stopped := history[2].Object.DeepCopy()
		stopped.ResourceVersion = "103"
		stopped.Status.Phase = corev1.PodFailed
		stopped.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2}}
		return append(history[:4:4], watchEvent[*corev1.Pod]{Type: watch.Modified, Object: stopped}, history[4])
	}
	for _, test := range []struct {
		name string
		edit func([]watchEvent[*corev1.Pod]) []watchEvent[*corev1.Pod]
		uid  types.UID
		want string
	}{
		{name: "stored Failed before it went", edit: failed, want: "was stored in phase Failed at resourceVersion 103"},
		{name: "the deletion itself stored Succeeded", edit: func(history []watchEvent[*corev1.Pod]) []watchEvent[*corev1.Pod] {
			history[4].Object.Status.Phase = corev1.PodSucceeded
			return history
		}, want: "was stored in phase Succeeded at resourceVersion 104"},
		{name: "stored Failed with no executor status", edit: func(history []watchEvent[*corev1.Pod]) []watchEvent[*corev1.Pod] {
			history[2].Object.Status.Phase = corev1.PodFailed
			history[2].Object.Status.ContainerStatuses = nil
			return history
		}, want: "was stored in phase Failed at resourceVersion 102"},
		{name: "the executor terminated in a running Pod", edit: func(history []watchEvent[*corev1.Pod]) []watchEvent[*corev1.Pod] {
			history[2].Object.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
			return history
		}, want: "with its executor container terminated at resourceVersion 102"},
		{name: "no deletion recorded", edit: func(history []watchEvent[*corev1.Pod]) []watchEvent[*corev1.Pod] {
			return history[:4]
		}, want: "did not record the deletion"},
		{name: "a state after the deletion", edit: func(history []watchEvent[*corev1.Pod]) []watchEvent[*corev1.Pod] {
			again := history[4].Object.DeepCopy()
			again.ResourceVersion = "105"
			return append(history, watchEvent[*corev1.Pod]{Type: watch.Added, Object: again})
		}, want: "after its deletion"},
		{name: "another Pod's history", edit: func(history []watchEvent[*corev1.Pod]) []watchEvent[*corev1.Pod] {
			return history
		}, uid: "missing-uid", want: "recorded no state of Pod missing-uid"},
		{name: "an empty history", edit: func([]watchEvent[*corev1.Pod]) []watchEvent[*corev1.Pod] { return nil }, want: "recorded no state"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			uid := test.uid
			if uid == "" {
				uid = "pod-uid"
			}
			_, err := unstoredStopHeld(test.edit(unstoredStopHistory()), uid)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unstoredStopHeld = %v, want an error saying %q", err, test.want)
			}
		})
	}
	if _, err := unstoredStopHeld(unstoredStopHistory(), ""); err == nil {
		t.Fatal("a history read for no UID was accepted")
	}
}
