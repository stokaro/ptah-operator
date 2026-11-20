package e2e

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controller"
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

// runningSchemaExecutorReadings is the readings a rollout of the executor
// leaves around a running schema Apply, in order: the claim the dispatching
// manager wrote, the replacement's wait, the write that reads the Apply under
// the binding that authorized it, the write that moves the binding, a proof
// claim under the new binding, the proof crediting the original plan, and a
// fresh claim of the new binding.
type runningSchemaExecutorReadings struct {
	before, waiting, read, rotated, proof, proved, fresh *ptahv1alpha1.PtahSchema
	replacement                                          string
}

func runningSchemaExecutorFixture() runningSchemaExecutorReadings {
	plan, active := proofPlan(), proofApplyActive()
	active.AdmissionSnapshot = &ptahv1alpha1.PodAdmissionSnapshot{Version: "v1", Digest: proofDigest("a"), TemplateDigest: proofDigest("b")}
	conditions := append(proofVerifyingConditions()[:2], metav1.Condition{Type: ptahv1alpha1.ConditionApplying,
		Status: metav1.ConditionTrue, Reason: string(ptahv1alpha1.ReasonApprovedPlan), Message: "Applying the exact current plan"})
	before := proofSchema(proofSchemaName, ptahv1alpha1.PtahSchemaStatus{
		ExecutionBinding: proofBinding(), ActiveOperation: active, Plan: &plan, ObservedGeneration: 3, Conditions: conditions,
	})
	before.Generation, before.ResourceVersion = 3, "100"
	before.Finalizers = []string{lifecycleGuardActiveOperationFinalizer}

	waiting := before.DeepCopy()
	waiting.ResourceVersion = "150"
	waiting.Status.Conditions[2].Message += controller.RotationWaitWords

	read := waiting.DeepCopy()
	read.ResourceVersion = "200"
	read.Status.ActiveOperation = nil
	read.Status.PendingObservation = proofPending(ptahv1alpha1.PendingObservationApplySucceeded)
	read.Status.PendingObservation.ObserveAfter = nil
	read.Status.PendingObservation.AdmissionSnapshot = active.AdmissionSnapshot.DeepCopy()
	read.Status.PendingObservation.DispatchedBy = &ptahv1alpha1.ManagerRecord{}
	read.Status.Conditions[2] = metav1.Condition{Type: ptahv1alpha1.ConditionApplying, Status: metav1.ConditionFalse, Reason: "JobCompleted"}

	replacement := "registry.invalid/ptah@" + proofDigest("c")
	rotated := read.DeepCopy()
	rotated.ResourceVersion = "300"
	rotated.Status.Plan = nil
	rotated.Status.ExecutionBinding.Epoch, rotated.Status.ExecutionBinding.ExecutorImage = proofOtherEpoch, replacement
	rotated.Status.PendingBindingRetirement = &ptahv1alpha1.BindingRetirementStatus{RetiredEpoch: proofEpoch}

	proof := rotated.DeepCopy()
	proof.ResourceVersion = "400"
	proof.Status.PendingBindingRetirement = nil
	proof.Status.ActiveOperation = proofActive(ptahv1alpha1.OperationObserve, "proof", "proof-job")
	proof.Status.ActiveOperation.ExecutionBindingID = proofOtherEpoch
	proof.Status.ActiveOperation.StartedAt = proofTime(640)

	proved := proof.DeepCopy()
	proved.ResourceVersion = "500"
	proved.Status.ActiveOperation, proved.Status.PendingObservation = nil, nil
	proved.Status.Applied = &ptahv1alpha1.AppliedStatus{
		PlanRef:         ptahv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
		PlanFingerprint: plan.Fingerprint, ExecutionBindingID: plan.ExecutionBindingID, ExecutorImage: plan.ExecutorImage,
		DispatchedBy: &ptahv1alpha1.ManagerRecord{}, CompletedAt: proofTime(660),
	}

	fresh := proved.DeepCopy()
	fresh.ResourceVersion = "600"
	fresh.Status.Source.Verified = true
	fresh.Status.ActiveOperation = proofActive(ptahv1alpha1.OperationObserve, "fresh", "fresh-job")
	fresh.Status.ActiveOperation.JobName = "fresh-" + fresh.Status.ActiveOperation.JobName
	fresh.Status.ActiveOperation.ExecutionBindingID = proofOtherEpoch
	fresh.Status.ActiveOperation.StartedAt = proofTime(700)
	return runningSchemaExecutorReadings{before: before, waiting: waiting, read: read, rotated: rotated,
		proof: proof, proved: proved, fresh: fresh, replacement: replacement}
}

func TestRunningSchemaExecutorWaitKeepsTheEntireClaim(t *testing.T) {
	t.Parallel()
	readings := runningSchemaExecutorFixture()
	before, waiting := readings.before, readings.waiting
	if err := schemaExecutorWaiting(before, waiting, proofApplyPodUID); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"different resource": func(s *ptahv1alpha1.PtahSchema) { s.UID = "other" },
		"spec edit":          func(s *ptahv1alpha1.PtahSchema) { s.Spec.Suspend = true },
		"generation change":  func(s *ptahv1alpha1.PtahSchema) { s.Generation++ },
		"lost claim":         func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil },
		"new Apply":          func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.ID = "other" },
		"replaced job":       func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobUID = "other" },
		"shortened horizon":  func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.ExecutionNotAfter = proofTimePointer(599) },
		"lost finalizer":     func(s *ptahv1alpha1.PtahSchema) { s.Finalizers = nil },
		"rotated binding":    func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.Epoch = proofOtherEpoch },
		"new image":          func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.ExecutorImage = readings.replacement },
		"dropped plan":       func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = nil },
		"read before it ended": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation = readings.read.Status.PendingObservation
		},
		"retirement recorded": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingBindingRetirement = readings.rotated.Status.PendingBindingRetirement
		},
		"premature release": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		},
		"attributed success": func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied = readings.proved.Status.Applied },
		"the dispatching manager's own words": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[2].Message = "Applying the exact current plan"
		},
		"not applying":     func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[2].Status = metav1.ConditionFalse },
		"another reason":   func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[2].Reason = "JobCompleted" },
		"no applying":      func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions = s.Status.Conditions[:2] },
		"words in another": func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[2].Type = ptahv1alpha1.ConditionPlanReady },
	} {
		t.Run(name, func(t *testing.T) {
			changed := waiting.DeepCopy()
			mutate(changed)
			if schemaExecutorWaiting(before, changed, proofApplyPodUID) == nil {
				t.Fatal("a reading that is not the replacement's wait for the original claim passed")
			}
		})
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"missing original claim":     func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil },
		"undispatched claim":         func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.DispatchStarted = false },
		"missing original job":       func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobUID = "" },
		"missing original admission": func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.AdmissionSnapshot = nil },
		"missing original horizon":   func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.ExecutionNotAfter = nil },
		"claim of another epoch":     func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.ExecutionBindingID = proofOtherEpoch },
		"plan of another epoch":      func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.ExecutionBindingID = proofOtherEpoch },
	} {
		t.Run(name, func(t *testing.T) {
			changed := before.DeepCopy()
			mutate(changed)
			waitingFor := waiting.DeepCopy()
			mutate(waitingFor)
			if schemaExecutorWaiting(changed, waitingFor, proofApplyPodUID) == nil {
				t.Fatal("unbound original Apply passed")
			}
		})
	}
	if schemaExecutorWaiting(before, waiting, "") == nil {
		t.Fatal("a wait with no Apply Pod passed")
	}
}

func TestRunningSchemaExecutorWaitChecksEveryWatchedTransition(t *testing.T) {
	t.Parallel()
	readings := runningSchemaExecutorFixture()
	before, waiting := readings.before, readings.waiting
	held := before.DeepCopy()
	held.ResourceVersion = "120"
	history := func(objects ...*ptahv1alpha1.PtahSchema) []watchEvent[*ptahv1alpha1.PtahSchema] {
		var events []watchEvent[*ptahv1alpha1.PtahSchema]
		for _, object := range objects {
			events = append(events, ftEvent(watch.Modified, object))
		}
		return events
	}
	check := func(events []watchEvent[*ptahv1alpha1.PtahSchema]) error {
		return schemaExecutorWaitWindow(events, before, proofApplyPodUID, waiting.ResourceVersion)
	}
	if err := check(history(before, held, waiting)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"brief claim loss":         func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil },
		"brief job substitution":   func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobUID = "other" },
		"brief horizon shortening": func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.ExecutionNotAfter = proofTimePointer(599) },
		"brief finalizer loss":     func(s *ptahv1alpha1.PtahSchema) { s.Finalizers = nil },
		"brief spec change":        func(s *ptahv1alpha1.PtahSchema) { s.Spec.Suspend = true },
		"brief epoch change":       func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.Epoch = proofOtherEpoch },
		"brief early read": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation = readings.read.Status.PendingObservation
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := held.DeepCopy()
			mutate(bad)
			if check(history(before, bad, waiting)) == nil {
				t.Fatal("a later correct status hid lost evidence")
			}
		})
	}
	deleted := history(before, waiting)
	deleted[1].Type = watch.Deleted
	for i, events := range [][]watchEvent[*ptahv1alpha1.PtahSchema]{
		nil, history(waiting), history(before), history(before, held), deleted,
	} {
		t.Run("invalid boundary "+strconv.Itoa(i), func(t *testing.T) {
			if check(events) == nil {
				t.Fatal("incomplete or invalid history passed")
			}
		})
	}
	if schemaExecutorWaitWindow(history(before, held), before, proofApplyPodUID, held.ResourceVersion) == nil {
		t.Fatal("a window that ends before the replacement's wait passed")
	}
}

func TestRunningSchemaExecutorReadWindowKeepsTheOrder(t *testing.T) {
	t.Parallel()
	readings := runningSchemaExecutorFixture()
	before, replacement := readings.before, readings.replacement
	ordered := []*ptahv1alpha1.PtahSchema{before, readings.waiting, readings.read, readings.rotated, readings.proof, readings.proved, readings.fresh}
	history := func(objects []*ptahv1alpha1.PtahSchema) []watchEvent[*ptahv1alpha1.PtahSchema] {
		var events []watchEvent[*ptahv1alpha1.PtahSchema]
		for _, object := range objects {
			events = append(events, ftEvent(watch.Modified, object))
		}
		return events
	}
	end := readings.fresh.ResourceVersion
	if err := schemaExecutorReadWindow(history(ordered), before, proofApplyPodUID, replacement, end); err != nil {
		t.Fatal(err)
	}
	newBinding := readings.rotated.Status.ExecutionBinding
	for name, at := range map[string]struct {
		index  int
		mutate func(*ptahv1alpha1.PtahSchema)
	}{
		"binding moved while the claim stood": {1, func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding = newBinding.DeepCopy() }},
		"claim rewritten while held":          {1, func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.LeaseEpoch = proofOtherEpoch }},
		"binding moved in the write that read the Apply": {2, func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ExecutionBinding = newBinding.DeepCopy()
		}},
		"claim kept in the write that read the Apply": {2, func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = before.Status.ActiveOperation.DeepCopy()
		}},
		"the Apply recorded unknown": {2, func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation.Outcome = ptahv1alpha1.PendingObservationOutcomeUnknown
		}},
		"the Apply recorded failed": {2, func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation.ApplyFailure = &ptahv1alpha1.SchemaApplyFailureStatus{}
		}},
		"another Job read": {2, func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.ApplyJobUID = "other" }},
		"another Pod read": {2, func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.ApplyPodUIDs = []types.UID{"other"} }},
		"another plan read": {2, func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation.Plan.ExecutionBindingID = proofOtherEpoch
		}},
		"new work claimed under the retired binding": {3, func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ExecutionBinding = before.Status.ExecutionBinding.DeepCopy()
			s.Status.ActiveOperation = proofActive(ptahv1alpha1.OperationObserve, "early", "early-job")
		}},
		"proof claimed under another binding": {4, func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation.ExecutionBindingID = proofEpoch
		}},
		"credited before its proof": {4, func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied = readings.proved.Status.Applied.DeepCopy() }},
		"what the Apply reported rewritten": {4, func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation.ApplyOperationID = "other"
		}},
		"credited under the new binding": {5, func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied.ExecutionBindingID = proofOtherEpoch }},
		"credited to another plan":       {5, func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied.PlanRef.UID = "other" }},
		"credited to nobody":             {5, func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied.DispatchedBy = nil }},
		"proof that credits nothing":     {5, func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied = nil }},
		"binding moved back": {5, func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ExecutionBinding = before.Status.ExecutionBinding.DeepCopy()
		}},
		"binding moved elsewhere": {5, func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.ExecutorImage = "third" }},
		"binding moved twice":     {5, func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.Epoch = proofLeaseEpoch }},
		"credit rewritten":        {6, func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied.CompletedAt = proofTime(900) }},
		"proof owed again after the credit": {6, func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation = readings.rotated.Status.PendingObservation.DeepCopy()
		}},
		"fresh claim under another binding": {6, func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.ExecutionBindingID = proofEpoch }},
	} {
		t.Run(name, func(t *testing.T) {
			broken := make([]*ptahv1alpha1.PtahSchema, len(ordered))
			for i, s := range ordered {
				broken[i] = s.DeepCopy()
			}
			at.mutate(broken[at.index])
			if schemaExecutorReadWindow(history(broken), before, proofApplyPodUID, replacement, end) == nil {
				t.Fatal("a window out of the order the rollout owes passed")
			}
		})
	}
	for name, last := range map[string]int{"ends at the read": 2, "ends at the rotation": 3, "ends before the credit": 4} {
		t.Run(name, func(t *testing.T) {
			if schemaExecutorReadWindow(history(ordered[:last+1]), before, proofApplyPodUID, replacement, ordered[last].ResourceVersion) == nil {
				t.Fatal("a window that never read the Apply, moved the binding after it and credited it passed")
			}
		})
	}
	deleted := history(ordered)
	deleted[len(deleted)-1].Type = watch.Deleted
	for i, events := range [][]watchEvent[*ptahv1alpha1.PtahSchema]{nil, history(ordered[1:]), history(ordered[:6]), deleted} {
		t.Run("invalid boundary "+strconv.Itoa(i), func(t *testing.T) {
			if schemaExecutorReadWindow(events, before, proofApplyPodUID, replacement, end) == nil {
				t.Fatal("a missing watch barrier or deletion passed")
			}
		})
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

func TestExecutorRecoveryRequiresTheOriginalProofAndFreshSource(t *testing.T) {
	t.Parallel()
	for _, operation := range []ptahv1alpha1.OperationType{ptahv1alpha1.OperationObserve, ptahv1alpha1.OperationPlan} {
		t.Run(string(operation), func(t *testing.T) {
			readings := runningSchemaExecutorFixture()
			before, replacement := readings.before, readings.replacement
			finished := proofTime(635).Time
			proof, fresh := readings.proof.DeepCopy(), readings.fresh.DeepCopy()
			for _, s := range []*ptahv1alpha1.PtahSchema{proof, fresh} {
				s.Status.ActiveOperation.Type = operation
			}
			jobFor := func(s *ptahv1alpha1.PtahSchema) *batchv1.Job {
				active := s.Status.ActiveOperation
				job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: active.JobName, Namespace: s.Namespace, UID: active.JobUID,
					CreationTimestamp: active.StartedAt, Labels: map[string]string{labelSchema: s.Name, labelOperation: strings.ToLower(string(operation))},
					Annotations: map[string]string{annotationOperationID: active.ID, annotationBindingID: active.ExecutionBindingID}}}
				job.Spec.Template.Spec.Containers = []corev1.Container{{Name: "ptah", Image: replacement}}
				return job
			}
			jobs := []*batchv1.Job{jobFor(proof), jobFor(fresh)}
			events := []watchEvent[*ptahv1alpha1.PtahSchema]{{Type: watch.Modified, Object: proof}, {Type: watch.Modified, Object: fresh}}
			check := func(e []watchEvent[*ptahv1alpha1.PtahSchema], j []*batchv1.Job) error {
				return schemaExecutorRecoveryPair(e, before, j, replacement, finished)
			}
			if err := check(events, jobs); err != nil {
				t.Fatal(err)
			}
			if err := check(events, []*batchv1.Job{jobs[1], jobs[0]}); err != nil {
				t.Fatalf("UID order must not select recovery stage: %v", err)
			}
			for name, at := range map[string]struct {
				index  int
				mutate func(*ptahv1alpha1.PtahSchema)
			}{
				"proof of an unknown outcome": {0, func(s *ptahv1alpha1.PtahSchema) {
					s.Status.PendingObservation.Outcome = ptahv1alpha1.PendingObservationOutcomeUnknown
				}},
				"proof of another Apply Job": {0, func(s *ptahv1alpha1.PtahSchema) { s.Status.PendingObservation.ApplyJobUID = "other" }},
				"credited before its proof":  {0, func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied = readings.proved.Status.Applied.DeepCopy() }},
				"proof under the old binding": {0, func(s *ptahv1alpha1.PtahSchema) {
					s.Status.ExecutionBinding = before.Status.ExecutionBinding.DeepCopy()
					s.Status.ActiveOperation.ExecutionBindingID = proofEpoch
				}},
				"proof before the Apply finished": {0, func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.StartedAt = proofTime(634) }},
				"wrong resource":                  {1, func(s *ptahv1alpha1.PtahSchema) { s.UID = "other" }},
				"wrong generation":                {1, func(s *ptahv1alpha1.PtahSchema) { s.Generation++ }},
				"repeated proof": {1, func(s *ptahv1alpha1.PtahSchema) {
					s.Status.PendingObservation = proof.Status.PendingObservation.DeepCopy()
					s.Status.Applied = nil
				}},
				"unverified source":               {1, func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.Verified = false }},
				"no credit for the original":      {1, func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied = nil }},
				"credit under the new binding":    {1, func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied.ExecutionBindingID = proofOtherEpoch }},
				"credit for another plan":         {1, func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied.PlanFingerprint = proofDigest("9") }},
				"old binding":                     {1, func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.ExecutionBindingID = proofEpoch }},
				"no claim":                        {1, func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil }},
				"fresh before the Apply finished": {1, func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.StartedAt = proofTime(634) }},
			} {
				t.Run(name, func(t *testing.T) {
					changed := slices.Clone(events)
					bad := changed[at.index].Object.DeepCopy()
					at.mutate(bad)
					changed[at.index].Object = bad
					// The Job is rebuilt from the changed claim, so a refusal
					// cannot come from a Job that no longer matches it.
					rebuilt := jobs
					if bad.Status.ActiveOperation != nil {
						rebuilt = slices.Clone(jobs)
						rebuilt[at.index] = jobFor(bad)
					}
					if check(changed, rebuilt) == nil {
						t.Fatal("incomplete recovery passed")
					}
				})
			}
			if check(events[:1], jobs) == nil || check(events, jobs[:1]) == nil || check(nil, jobs) == nil {
				t.Fatal("missing recovery evidence passed")
			}
			premature := jobs[0].DeepCopy()
			premature.CreationTimestamp = proofTime(634)
			oldExecutor := jobs[1].DeepCopy()
			oldExecutor.Spec.Template.Spec.Containers[0].Image = before.Status.ExecutionBinding.ExecutorImage
			for name, broken := range map[string][]*batchv1.Job{
				"premature job":     {premature, jobs[1]},
				"old executor":      {jobs[0], oldExecutor},
				"one stage twice":   {jobs[0], jobs[0]},
				"unfinished Apply":  nil,
				"one job of a pair": {jobs[0], nil},
			} {
				t.Run(name, func(t *testing.T) {
					if check(events, broken) == nil {
						t.Fatal("a recovery job that is not the claim's passed")
					}
				})
			}
			if schemaExecutorRecoveryPair(events, before, jobs, replacement, time.Time{}) == nil {
				t.Fatal("a recovery with no finished Apply passed")
			}
		})
	}
}
