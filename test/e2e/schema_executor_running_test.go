package e2e

import (
	"slices"
	"strconv"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestExecutorBackendRequiresTheExactRunningPodAddress(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "apply-pod"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.3.161"}}
	for name, backend := range map[string]string{
		"inet cast retains a netmask": "42/10.244.3.161/32",
		"another Pod":                 "42/10.244.3.162",
		"no PID":                      "/10.244.3.161",
		"zero PID":                    "0/10.244.3.161",
		"invalid PID":                 "+42/10.244.3.161",
		"PID overflow":                "18446744073709551616/10.244.3.161",
		"multiple processes":          "42/10.244.3.16143/10.244.3.161",
		"no client address":           "42/",
	} {
		t.Run(name, func(t *testing.T) {
			if executorBackendMatchesPod(backend, pod, pod.UID) {
				t.Fatal("a process other than one exact live Apply client passed")
			}
		})
	}
	if !executorBackendMatchesPod("42/10.244.3.161", pod, pod.UID) {
		t.Fatal("the PostgreSQL host() address did not match its actual Pod IP")
	}
	pod.Status.PodIP = "2001:db8::1"
	if !executorBackendMatchesPod("42/2001:0db8:0:0:0:0:0:1", pod, pod.UID) {
		t.Fatal("the same IPv6 address failed because of its spelling")
	}
	for _, uid := range []types.UID{"", "replacement"} {
		if executorBackendMatchesPod("42/2001:db8::1", pod, uid) {
			t.Fatal("an absent or replacement Pod identity passed")
		}
	}
	pod.Status.Phase = corev1.PodSucceeded
	if executorBackendMatchesPod("42/2001:db8::1", pod, pod.UID) {
		t.Fatal("a stopped workload became the held SQL process")
	}
	if executorBackendMatchesPod("42/2001:db8::1", nil, "apply-pod") {
		t.Fatal("an absent Pod passed")
	}
}

func runningExecutorFixture() (before, retired *ptahv1alpha1.PtahSchema, replacement string) {
	plan, active := proofPlan(), proofApplyActive()
	active.AdmissionSnapshot = &ptahv1alpha1.PodAdmissionSnapshot{Version: "v1", Digest: proofDigest("a"), TemplateDigest: proofDigest("b")}
	before = proofSchema(proofSchemaName, ptahv1alpha1.PtahSchemaStatus{
		ExecutionBinding: proofBinding(), ActiveOperation: active, Plan: &plan, ObservedGeneration: 3,
		Conditions: proofVerifyingConditions(),
	})
	before.Generation, before.ResourceVersion = 3, "100"
	before.Finalizers = []string{lifecycleGuardActiveOperationFinalizer}
	retired = before.DeepCopy()
	retired.ResourceVersion = "200"
	retired.Status.ActiveOperation, retired.Status.Plan = nil, nil
	replacement = "registry.invalid/ptah@" + proofDigest("c")
	retired.Status.ExecutionBinding.Epoch = proofOtherEpoch
	retired.Status.ExecutionBinding.ExecutorImage = replacement
	retired.Status.PendingObservation = proofPending(ptahv1alpha1.PendingObservationOutcomeUnknown)
	retired.Status.PendingObservation.AdmissionSnapshot = active.AdmissionSnapshot.DeepCopy()
	retired.Status.PendingObservation.ObserveAfter = proofTimePointer(630)
	return
}

func TestRunningExecutorRetirementPreservesTheOriginalApply(t *testing.T) {
	t.Parallel()
	before, retired, replacement := runningExecutorFixture()
	if err := schemaExecutorRetirement(before, retired, proofApplyPodUID, replacement); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"different resource": func(s *ptahv1alpha1.PtahSchema) { s.UID = "other" },
		"spec edit":          func(s *ptahv1alpha1.PtahSchema) { s.Spec.Suspend = true },
		"generation change":  func(s *ptahv1alpha1.PtahSchema) { s.Generation++ },
		"binding lost":       func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding = nil },
		"old epoch":          func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.Epoch = proofEpoch },
		"invalid epoch":      func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.Epoch = "changed" },
		"old image": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ExecutionBinding.ExecutorImage = before.Status.ExecutionBinding.ExecutorImage
		},
		"changed version":  func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.PtahVersion += "other" },
		"changed protocol": func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.RunnerProtocolVersion++ },
		"changed state":    func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.ControllerStateVersion++ },
		"no finalizer":     func(s *ptahv1alpha1.PtahSchema) { s.Finalizers = nil },
		"new active work": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = proofActive(ptahv1alpha1.OperationObserve, "new", "new")
		},
		"attributed success": func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied = &ptahv1alpha1.AppliedStatus{} },
		"new plan":           func(s *ptahv1alpha1.PtahSchema) { p := proofPlan(); s.Status.Plan = &p },
		"premature ready":    func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[1].Status = metav1.ConditionTrue },
		"no conditions":      func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions = nil },
		"forgotten apply":    func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation = nil },
		"reported success": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation.Outcome = ptahv1alpha1.PendingObservationApplySucceeded
		},
		"changed operation":        func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.ApplyOperationID = "other" },
		"changed job name":         func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.ApplyJobName = "other" },
		"replaced job":             func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.ApplyJobUID = "other" },
		"replaced pod":             func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.ApplyPodUIDs[0] = "other" },
		"no pod":                   func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.ApplyPodUIDs = nil },
		"wrong pod count":          func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.ApplyPodCount++ },
		"changed apply generation": func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.ApplyGeneration++ },
		"changed approved plan":    func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.Plan.ContentDigest = proofDigest("e") },
		"rewritten old plan epoch": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation.Plan.ExecutionBindingID = proofOtherEpoch
		},
		"lost admission": func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.AdmissionSnapshot = nil },
		"changed admission": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation.AdmissionSnapshot.TemplateDigest = proofDigest("f")
		},
		"changed target":         func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.Target.URLFrom.Name = "other" },
		"changed source":         func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.Source.Digest = proofDigest("e") },
		"changed exclusions":     func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.Exclude = nil },
		"changed protection":     func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.ProtectedTables = []string{"new"} },
		"changed timeout":        func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.LockTimeout.Duration++ },
		"changed realm":          func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.CoordinationDigest = "other" },
		"changed lease":          func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.LeaseEpoch = proofOtherEpoch },
		"shortened lease":        func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.LeaseDurationSeconds-- },
		"early observation":      func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.ObserveAfter = proofTimePointer(629) },
		"no observation horizon": func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.ObserveAfter = nil },
		"already observed":       func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.PlanRequired = true },
	} {
		t.Run(name, func(t *testing.T) {
			changed := retired.DeepCopy()
			mutate(changed)
			if schemaExecutorRetirement(before, changed, proofApplyPodUID, replacement) == nil {
				t.Fatal("unsafe retirement passed")
			}
		})
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"missing original claim":     func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil },
		"undispatched claim":         func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.DispatchStarted = false },
		"missing original job":       func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobUID = "" },
		"missing original admission": func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.AdmissionSnapshot = nil },
		"missing original horizon":   func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.ExecutionNotAfter = nil },
	} {
		t.Run(name, func(t *testing.T) {
			changed := before.DeepCopy()
			mutate(changed)
			if schemaExecutorRetirement(changed, retired, proofApplyPodUID, replacement) == nil {
				t.Fatal("unbound original Apply passed")
			}
		})
	}
}

func TestRunningExecutorWindowRejectsTemporaryEvidenceLoss(t *testing.T) {
	t.Parallel()
	before, retired, replacement := runningExecutorFixture()
	history := func(objects ...*ptahv1alpha1.PtahSchema) []watchEvent[*ptahv1alpha1.PtahSchema] {
		var events []watchEvent[*ptahv1alpha1.PtahSchema]
		for _, object := range objects {
			events = append(events, ftEvent(watch.Modified, object))
		}
		return events
	}
	check := func(events []watchEvent[*ptahv1alpha1.PtahSchema]) error {
		return schemaExecutorHeldWindow(events, before, proofApplyPodUID, replacement, retired.ResourceVersion)
	}
	if err := check(history(before, retired)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"brief claim loss":         func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil },
		"brief job substitution":   func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobUID = "other" },
		"brief horizon shortening": func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.ExecutionNotAfter = proofTimePointer(599) },
		"brief finalizer loss":     func(s *ptahv1alpha1.PtahSchema) { s.Finalizers = nil },
		"brief spec change":        func(s *ptahv1alpha1.PtahSchema) { s.Spec.Suspend = true },
		"brief epoch change":       func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.Epoch = proofOtherEpoch },
	} {
		t.Run(name, func(t *testing.T) {
			bad := before.DeepCopy()
			bad.ResourceVersion = "150"
			mutate(bad)
			if check(history(before, bad, retired)) == nil {
				t.Fatal("a later correct status hid lost evidence")
			}
		})
	}
	extraRotation := retired.DeepCopy()
	extraRotation.ResourceVersion = "175"
	extraRotation.Status.ExecutionBinding.Epoch = proofLeaseEpoch
	deleted := history(before, retired)
	deleted[1].Type = watch.Deleted
	for i, events := range [][]watchEvent[*ptahv1alpha1.PtahSchema]{
		nil, history(retired), history(before), history(before, extraRotation, retired), deleted,
	} {
		t.Run("invalid boundary "+strconv.Itoa(i), func(t *testing.T) {
			if check(events) == nil {
				t.Fatal("incomplete or invalid history passed")
			}
		})
	}
	unchanged := before.DeepCopy()
	unchanged.ResourceVersion = retired.ResourceVersion
	if check(history(before, unchanged)) == nil {
		t.Fatal("an unchanged binding passed")
	}
}

func TestRunningExecutorWorkloadHistoryRejectsReplayAndOverlap(t *testing.T) {
	t.Parallel()
	job, readJob := ftJob("apply", ftSchema, "apply"), ftJob("observe", ftSchema, "observe")
	pod, readPod := ftPod("apply-pod", ftSchema, "apply", corev1.PodRunning), ftPod("read-pod", ftSchema, "observe", corev1.PodRunning)
	finished := pod.DeepCopy()
	finished.Status.Phase = corev1.PodSucceeded
	jobs := []watchEvent[*batchv1.Job]{ftEvent(watch.Added, job), ftEvent(watch.Modified, ftComplete(job)), ftEvent(watch.Added, readJob)}
	pods := []watchEvent[*corev1.Pod]{ftEvent(watch.Added, pod), ftEvent(watch.Modified, finished), ftEvent(watch.Added, readPod)}
	if !schemaExecutorNoReplay(jobs, pods, ftSchema, "apply", "apply-pod") {
		t.Fatal("serialized original Apply and diagnostic were refused")
	}
	for name, changed := range map[string][]watchEvent[*batchv1.Job]{
		"no jobs":                       nil,
		"another Apply":                 append(slices.Clone(jobs), ftEvent(watch.Added, ftJob("replay", ftSchema, "apply"))),
		"overlapping diagnostic":        {jobs[0], jobs[2], jobs[1]},
		"original Apply not seen added": {jobs[1], jobs[2]},
	} {
		t.Run(name, func(t *testing.T) {
			if schemaExecutorNoReplay(changed, pods, ftSchema, "apply", "apply-pod") {
				t.Fatal("invalid Job history passed")
			}
		})
	}
	for name, changed := range map[string][]watchEvent[*corev1.Pod]{
		"no pods":                nil,
		"replacement Pod":        append(slices.Clone(pods), ftEvent(watch.Added, ftPod("replay", ftSchema, "apply", corev1.PodRunning))),
		"overlapping diagnostic": {pods[0], pods[2], pods[1]},
	} {
		t.Run(name, func(t *testing.T) {
			if schemaExecutorNoReplay(jobs, changed, ftSchema, "apply", "apply-pod") {
				t.Fatal("invalid Pod history passed")
			}
		})
	}
}
