package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// The fault predicates port the jq filters of the fault script. Each test
// below builds the reading a predicate exists to accept, then breaks one
// clause of the filter at a time and requires the predicate to refuse the
// result, the way the shell self-tests held a filter to the mistakes it
// guards against.

const (
	ftSchema       = "e2e-fault-pg-restart"
	ftEpoch        = "v1-0123456789abcdef0123456789abcdef"
	ftOtherEpoch   = "v1-fedcba9876543210fedcba9876543210"
	ftThirdEpoch   = "v1-00000000000000000000000000000000"
	ftStateVersion = int32(3)
	ftStartedAt    = "2026-09-28T10:00:05Z"
)

var (
	ftDigest     = "sha256:" + strings.Repeat("ab", 32)
	ftController = controllerIdentity{
		image:        "registry.invalid/ptah-operator@sha256:" + strings.Repeat("cd", 32),
		revision:     "rev-e2e",
		stateVersion: "3",
	}
	ftStart = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
)

// ftMutation is one change a predicate has to refuse, or, among variants,
// one it has to keep accepting.
type ftMutation[T any] struct {
	name   string
	mutate func(T)
}

// ftRefusesEach requires the predicate to accept a fresh fixture and to
// refuse the fixture after each mutation.
func ftRefusesEach[T any](t *testing.T, fresh func() T, accepts func(T) bool, mutations []ftMutation[T]) {
	t.Helper()
	if !accepts(fresh()) {
		t.Fatal("the predicate refused the fixture it exists to accept")
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := fresh()
			test.mutate(value)
			if accepts(value) {
				t.Fatal("the predicate accepted a fixture it has to refuse")
			}
		})
	}
}

// ftAcceptsEach requires the predicate to keep accepting the fixture after
// each variant: a change the filter it ports did not look at.
func ftAcceptsEach[T any](t *testing.T, fresh func() T, accepts func(T) bool, variants []ftMutation[T]) {
	t.Helper()
	for _, test := range variants {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := fresh()
			test.mutate(value)
			if !accepts(value) {
				t.Fatal("the predicate refused a fixture it has to accept")
			}
		})
	}
}

func ftEvent[T client.Object](eventType watch.EventType, object T) watchEvent[T] {
	return watchEvent[T]{Type: eventType, Object: object}
}

// ftJob is an operation Job of the schema, bound to an operation ID derived
// from its UID.
func ftJob(uid, schema, operation string) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "job-" + uid, UID: types.UID(uid),
		Labels:      map[string]string{labelSchema: schema, labelOperation: operation},
		Annotations: map[string]string{annotationOperationID: "op-" + uid},
	}}
}

func ftWithCondition(job *batchv1.Job, kind batchv1.JobConditionType, status corev1.ConditionStatus, reason string) *batchv1.Job {
	copied := job.DeepCopy()
	copied.Status.Conditions = append(copied.Status.Conditions,
		batchv1.JobCondition{Type: kind, Status: status, Reason: reason})
	return copied
}

func ftComplete(job *batchv1.Job) *batchv1.Job {
	return ftWithCondition(job, batchv1.JobComplete, corev1.ConditionTrue, "")
}

func ftJobBookmark() watchEvent[*batchv1.Job] {
	return watchEvent[*batchv1.Job]{Type: watch.Bookmark,
		Object: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "42"}}}
}

func ftPod(uid, schema, operation string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-" + uid, UID: types.UID(uid),
			Labels: map[string]string{labelSchema: schema, labelOperation: operation},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func ftSchemaNamed(name string) *ptahv1alpha1.PtahSchema {
	return &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "schema-uid"}}
}

// ftLease is a target Lease; an empty holder or epoch leaves the field out.
func ftLease(name, uid, holder, epoch string) *coordinationv1.Lease {
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)}}
	if holder != "" {
		lease.Spec.HolderIdentity = ptr.To(holder)
	}
	if epoch != "" {
		lease.Annotations = map[string]string{annotationLeaseEpoch: epoch}
	}
	return lease
}

func TestFaultBoundsHoldTogether(t *testing.T) {
	t.Parallel()
	deadline, barrier := int64(faultActiveDeadlineSeconds), int64(faultBarrierSeconds)
	timeoutDeadline := int64(faultTimeoutDeadlineSeconds)
	if deadline < 7200 || deadline > 86400 {
		t.Errorf("the fault Apply active deadline is %d seconds, outside 7200 to 86400", deadline)
	}
	if barrier <= deadline {
		t.Errorf("the database barrier holds %d seconds and does not outlive the %d-second Apply deadline", barrier, deadline)
	}
	if timeoutDeadline < 30 || timeoutDeadline > 120 {
		t.Errorf("the timeout acceptance Apply deadline is %d seconds, outside 30 to 120", timeoutDeadline)
	}
	if wait := waitTimeout; wait < time.Duration(timeoutDeadline+60)*time.Second {
		t.Errorf("a wait of %s leaves less than 60 seconds after the %d-second timeout deadline", wait, timeoutDeadline)
	}
}

func TestFaultObjectMatchesNeedAPresentValue(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		UID:         "job-a",
		Labels:      map[string]string{labelSchema: "s", labelOperation: "plan", "empty": ""},
		Annotations: map[string]string{annotationOperationID: "op", "empty": ""},
	}}
	bookmark := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "7"}}
	for _, test := range []struct {
		name      string
		got, want bool
	}{
		{"the UID given", uidIs(job, "job-a"), true},
		{"another UID", uidIs(job, "job-b"), false},
		{"an empty UID names nothing", uidIs(job, ""), false},
		{"a bookmark's absent UID is not the empty one", uidIs(bookmark, ""), false},
		{"a label with its value", labelIs(job, labelSchema, "s"), true},
		{"a label present and empty is the empty value", labelIs(job, "empty", ""), true},
		{"an absent label is not the empty value", labelIs(job, "absent", ""), false},
		{"a label with another value", labelIs(job, labelSchema, "t"), false},
		{"an annotation with its value", annotationIs(job, annotationOperationID, "op"), true},
		{"an annotation present and empty is the empty value", annotationIs(job, "empty", ""), true},
		{"an absent annotation is not the empty value", annotationIs(job, "absent", ""), false},
		{"an annotation with another value", annotationIs(job, annotationOperationID, "other"), false},
		{"the schema and any operation", operationOf(job, "s", ""), true},
		{"the schema and its operation", operationOf(job, "s", "plan"), true},
		{"the schema and another operation", operationOf(job, "s", "apply"), false},
		{"another schema", operationOf(job, "t", ""), false},
		{"a bookmark belongs to no schema", operationOf(bookmark, "", ""), false},
	} {
		if test.got != test.want {
			t.Errorf("%s: got %t, want %t", test.name, test.got, test.want)
		}
	}
}

func TestAddedUIDsCountAdditionsOfTheSchemaAndOperation(t *testing.T) {
	t.Parallel()
	jobs := []watchEvent[*batchv1.Job]{
		ftEvent(watch.Added, ftJob("z1", ftSchema, "observe")),
		ftEvent(watch.Modified, ftJob("m1", ftSchema, "observe")),
		ftEvent(watch.Added, ftJob("c1", "other-schema", "observe")),
		ftEvent(watch.Added, ftJob("d1", ftSchema, "plan")),
		ftJobBookmark(),
		ftEvent(watch.Added, ftJob("a1", ftSchema, "observe")),
		ftEvent(watch.Added, ftJob("z1", ftSchema, "observe")),
		ftEvent(watch.Deleted, ftJob("x1", ftSchema, "observe")),
	}
	pods := []watchEvent[*corev1.Pod]{
		ftEvent(watch.Added, ftPod("p2", ftSchema, "apply", corev1.PodPending)),
		ftEvent(watch.Added, ftPod("p1", ftSchema, "apply", corev1.PodPending)),
		ftEvent(watch.Modified, ftPod("p3", ftSchema, "apply", corev1.PodRunning)),
	}
	for _, test := range []struct {
		name      string
		got, want []string
	}{
		{"one operation, sorted and once each", addedUIDs(jobs, ftSchema, "observe"), []string{"a1", "z1"}},
		{"every operation of the schema", addedUIDs(jobs, ftSchema, ""), []string{"a1", "d1", "z1"}},
		{"another schema", addedUIDs(jobs, "other-schema", "observe"), []string{"c1"}},
		{"an operation never added", addedUIDs(jobs, ftSchema, "apply"), nil},
		{"Pods as well as Jobs", addedUIDs(pods, ftSchema, "apply"), []string{"p1", "p2"}},
		{"new since a checkpoint", newAddedUIDs(jobs, ftSchema, "observe", sortedCheckpoint([]string{"z1"})), []string{"a1"}},
		{"nothing new", newAddedUIDs(jobs, ftSchema, "observe", sortedCheckpoint([]string{"a1", "z1"})), nil},
	} {
		if !slices.Equal(test.got, test.want) {
			t.Errorf("%s: got %v, want %v", test.name, test.got, test.want)
		}
	}
}

type ftOrder struct {
	jobs               []watchEvent[*batchv1.Job]
	completed, addedID string
}

func TestCompletedBeforeAdded(t *testing.T) {
	t.Parallel()
	observe := func() *batchv1.Job { return ftJob("o1", ftSchema, "observe") }
	fresh := func() *ftOrder {
		return &ftOrder{jobs: []watchEvent[*batchv1.Job]{
			ftEvent(watch.Added, observe()),
			ftEvent(watch.Modified, ftComplete(observe())),
			ftEvent(watch.Added, ftJob("p1", ftSchema, "plan")),
		}, completed: "o1", addedID: "p1"}
	}
	accepts := func(o *ftOrder) bool { return completedBeforeAdded(o.jobs, o.completed, o.addedID) }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ftOrder]{
		{"the Plan added before the Observe completed", func(o *ftOrder) { o.jobs[1], o.jobs[2] = o.jobs[2], o.jobs[1] }},
		{"the Observe never completed", func(o *ftOrder) { o.jobs = slices.Delete(o.jobs, 1, 2) }},
		{"the Observe failed rather than completed", func(o *ftOrder) {
			o.jobs[1] = ftEvent(watch.Modified, ftWithCondition(observe(), batchv1.JobFailed, corev1.ConditionTrue, "BackoffLimitExceeded"))
		}},
		{"the completion condition is False", func(o *ftOrder) {
			o.jobs[1] = ftEvent(watch.Modified, ftWithCondition(observe(), batchv1.JobComplete, corev1.ConditionFalse, ""))
		}},
		{"the proof names another completed Job", func(o *ftOrder) { o.completed = "o2" }},
		{"the Plan was only ever modified", func(o *ftOrder) { o.jobs[2].Type = watch.Modified }},
		{"a later addition does not move the first one", func(o *ftOrder) {
			o.jobs = slices.Insert(o.jobs, 0, ftEvent(watch.Added, ftJob("p1", ftSchema, "plan")))
		}},
		{"an empty completed UID names nothing", func(o *ftOrder) {
			o.completed = ""
			o.jobs = slices.Insert(o.jobs, 0, ftEvent(watch.Modified, ftComplete(ftJob("", ftSchema, "observe"))))
		}},
		{"an addition cannot follow the completion it is", func(o *ftOrder) {
			o.jobs = []watchEvent[*batchv1.Job]{ftEvent(watch.Added, ftComplete(observe()))}
			o.addedID = "o1"
		}},
	})
}

func TestReadChainOrdered(t *testing.T) {
	t.Parallel()
	resolve := func() *batchv1.Job { return ftJob("r1", ftSchema, "resolve") }
	verify := func() *batchv1.Job { return ftJob("v1", ftSchema, "verify") }
	fresh := func() *[]watchEvent[*batchv1.Job] {
		return &[]watchEvent[*batchv1.Job]{
			ftEvent(watch.Added, resolve()),
			ftEvent(watch.Modified, ftComplete(resolve())),
			ftEvent(watch.Added, verify()),
			ftEvent(watch.Modified, ftComplete(verify())),
			ftEvent(watch.Added, ftJob("o1", ftSchema, "observe")),
		}
	}
	accepts := func(jobs *[]watchEvent[*batchv1.Job]) bool { return readChainOrdered(*jobs, ftSchema) }
	type chain = *[]watchEvent[*batchv1.Job]
	ftRefusesEach(t, fresh, accepts, []ftMutation[chain]{
		{"no Resolve Job", func(jobs chain) { *jobs = slices.Delete(*jobs, 0, 2) }},
		{"no Verify Job", func(jobs chain) { *jobs = slices.Delete(*jobs, 2, 4) }},
		{"no Observe Job", func(jobs chain) { *jobs = slices.Delete(*jobs, 4, 5) }},
		{"Verify added before Resolve completed", func(jobs chain) { (*jobs)[1], (*jobs)[2] = (*jobs)[2], (*jobs)[1] }},
		{"Observe added before Verify completed", func(jobs chain) { (*jobs)[3], (*jobs)[4] = (*jobs)[4], (*jobs)[3] }},
		{"Resolve never completed", func(jobs chain) { *jobs = slices.Delete(*jobs, 1, 2) }},
		{"Verify never completed", func(jobs chain) { *jobs = slices.Delete(*jobs, 3, 4) }},
		{"Resolve failed", func(jobs chain) {
			(*jobs)[1] = ftEvent(watch.Modified, ftWithCondition(resolve(), batchv1.JobFailed, corev1.ConditionTrue, ""))
		}},
		{"Resolve carries no operation ID", func(jobs chain) { delete((*jobs)[0].Object.Annotations, annotationOperationID) }},
		{"Verify's operation ID is empty", func(jobs chain) { (*jobs)[2].Object.Annotations[annotationOperationID] = "" }},
		{"Observe carries no operation ID", func(jobs chain) { delete((*jobs)[4].Object.Annotations, annotationOperationID) }},
		{"the first Resolve never completed", func(jobs chain) {
			*jobs = slices.Insert(*jobs, 0, ftEvent(watch.Added, ftJob("r0", ftSchema, "resolve")))
		}},
		{"the Observe is another schema's", func(jobs chain) { (*jobs)[4].Object.Labels[labelSchema] = "other" }},
		{"a bookmark stands where the Observe was added", func(jobs chain) { (*jobs)[4] = ftJobBookmark() }},
		{"the Resolve completion precedes its own addition", func(jobs chain) { (*jobs)[0], (*jobs)[1] = (*jobs)[1], (*jobs)[0] }},
	})
}

func TestOperationJobsOverlap(t *testing.T) {
	t.Parallel()
	a, b := ftJob("a", ftSchema, "observe"), ftJob("b", ftSchema, "plan")
	unlabeled := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "x", UID: "x"}}
	emptyLabel := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "y", UID: "y", Labels: map[string]string{labelSchema: ""}}}
	for _, test := range []struct {
		name string
		jobs []watchEvent[*batchv1.Job]
		want bool
	}{
		{"one Job at a time", []watchEvent[*batchv1.Job]{
			ftEvent(watch.Added, a), ftEvent(watch.Modified, ftComplete(a)),
			ftEvent(watch.Added, b), ftEvent(watch.Deleted, b),
			ftEvent(watch.Added, ftJob("c", "other", "observe")), ftEvent(watch.Added, ftJob("d", ftSchema, "apply")),
		}, false},
		{"a failed Job leaves the set", []watchEvent[*batchv1.Job]{
			ftEvent(watch.Added, a), ftEvent(watch.Modified, ftWithCondition(a, batchv1.JobFailed, corev1.ConditionTrue, "")),
			ftEvent(watch.Added, b),
		}, false},
		{"two unfinished Jobs of one schema", []watchEvent[*batchv1.Job]{ftEvent(watch.Added, a), ftEvent(watch.Added, b)}, true},
		{"unfinished Jobs of two schemas", []watchEvent[*batchv1.Job]{
			ftEvent(watch.Added, a), ftEvent(watch.Added, ftJob("c", "other", "plan")),
		}, false},
		{"a Job without a schema label", []watchEvent[*batchv1.Job]{ftEvent(watch.Added, a), ftEvent(watch.Added, unlabeled)}, false},
		{"a Job whose schema label is empty", []watchEvent[*batchv1.Job]{ftEvent(watch.Added, a), ftEvent(watch.Added, emptyLabel)}, false},
		{"a bookmark", []watchEvent[*batchv1.Job]{ftEvent(watch.Added, a), ftJobBookmark()}, false},
		{"an overlap stays reported once one finishes", []watchEvent[*batchv1.Job]{
			ftEvent(watch.Added, a), ftEvent(watch.Added, b), ftEvent(watch.Modified, ftComplete(a)),
		}, true},
		{"a completion that is False keeps the Job", []watchEvent[*batchv1.Job]{
			ftEvent(watch.Added, a), ftEvent(watch.Modified, ftWithCondition(a, batchv1.JobComplete, corev1.ConditionFalse, "")),
			ftEvent(watch.Added, b),
		}, true},
		{"a finished Job read unfinished again rejoins", []watchEvent[*batchv1.Job]{
			ftEvent(watch.Added, a), ftEvent(watch.Modified, ftComplete(a)), ftEvent(watch.Modified, a), ftEvent(watch.Added, b),
		}, true},
	} {
		if got := operationJobsOverlap(test.jobs); got != test.want {
			t.Errorf("%s: got %t, want %t", test.name, got, test.want)
		}
	}
}

func TestOperationPodsOverlap(t *testing.T) {
	t.Parallel()
	pod := func(uid string, phase corev1.PodPhase) *corev1.Pod { return ftPod(uid, ftSchema, "apply", phase) }
	for _, test := range []struct {
		name string
		pods []watchEvent[*corev1.Pod]
		want bool
	}{
		{"one Pod at a time", []watchEvent[*corev1.Pod]{
			ftEvent(watch.Added, pod("a", corev1.PodPending)), ftEvent(watch.Modified, pod("a", corev1.PodSucceeded)),
			ftEvent(watch.Added, pod("b", corev1.PodRunning)), ftEvent(watch.Modified, pod("b", corev1.PodFailed)),
			ftEvent(watch.Added, pod("c", corev1.PodRunning)), ftEvent(watch.Deleted, pod("c", corev1.PodRunning)),
			ftEvent(watch.Added, pod("d", corev1.PodPending)),
		}, false},
		{"two running Pods of one schema", []watchEvent[*corev1.Pod]{
			ftEvent(watch.Added, pod("a", corev1.PodRunning)), ftEvent(watch.Added, pod("b", corev1.PodRunning)),
		}, true},
		{"a pending Pod beside a running one", []watchEvent[*corev1.Pod]{
			ftEvent(watch.Added, pod("a", corev1.PodPending)), ftEvent(watch.Added, pod("b", corev1.PodRunning)),
		}, true},
		{"Pods of two schemas", []watchEvent[*corev1.Pod]{
			ftEvent(watch.Added, pod("a", corev1.PodRunning)),
			ftEvent(watch.Added, ftPod("b", "other", "apply", corev1.PodRunning)),
		}, false},
	} {
		if got := operationPodsOverlap(test.pods); got != test.want {
			t.Errorf("%s: got %t, want %t", test.name, got, test.want)
		}
	}
}

type ftSchemaHistory struct {
	schemas           []watchEvent[*ptahv1alpha1.PtahSchema]
	name, operationID string
}

func TestOutcomeUnknownRecorded(t *testing.T) {
	t.Parallel()
	fresh := func() *ftSchemaHistory {
		recorded := ftSchemaNamed(ftSchema)
		recorded.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{
			Outcome: ptahv1alpha1.PendingObservationOutcomeUnknown, ApplyOperationID: "op-apply",
		}
		recorded.Status.Conditions = []metav1.Condition{{Type: "Applying", Status: metav1.ConditionFalse, Reason: "OutcomeUnknown"}}
		return &ftSchemaHistory{schemas: []watchEvent[*ptahv1alpha1.PtahSchema]{
			ftEvent(watch.Added, ftSchemaNamed(ftSchema)), ftEvent(watch.Modified, recorded),
		}, name: ftSchema, operationID: "op-apply"}
	}
	accepts := func(h *ftSchemaHistory) bool { return outcomeUnknownRecorded(h.schemas, h.name, h.operationID) }
	recorded := func(h *ftSchemaHistory) *ptahv1alpha1.PtahSchema { return h.schemas[1].Object }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ftSchemaHistory]{
		{"only an addition records it", func(h *ftSchemaHistory) { h.schemas[1].Type = watch.Added }},
		{"another schema", func(h *ftSchemaHistory) { h.name = "other" }},
		{"no pending observation", func(h *ftSchemaHistory) { recorded(h).Status.PendingObservation = nil }},
		{"the Apply succeeded", func(h *ftSchemaHistory) {
			recorded(h).Status.PendingObservation.Outcome = ptahv1alpha1.PendingObservationApplySucceeded
		}},
		{"another Apply operation", func(h *ftSchemaHistory) { h.operationID = "op-other" }},
		{"an empty operation names nothing", func(h *ftSchemaHistory) { h.operationID = "" }},
		{"the Applying condition is True", func(h *ftSchemaHistory) { recorded(h).Status.Conditions[0].Status = metav1.ConditionTrue }},
		{"the Applying condition gives another reason", func(h *ftSchemaHistory) {
			recorded(h).Status.Conditions[0].Reason = string(ptahv1alpha1.ReasonApplyOutcomeUnknown)
		}},
		{"no Applying condition", func(h *ftSchemaHistory) { recorded(h).Status.Conditions[0].Type = "Ready" }},
	})
}

// ftApplySchema is a schema whose active Apply names the operation, the Job,
// the lease epoch and the coordination digest given, against a target whose
// coordination digest is the last.
func ftApplySchema(name, operationID, jobUID, epoch, coordination, target string) *ptahv1alpha1.PtahSchema {
	schema := ftSchemaNamed(name)
	schema.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{
		Type: ptahv1alpha1.OperationApply, ID: operationID, JobUID: types.UID(jobUID),
		LeaseEpoch: epoch, CoordinationDigest: coordination,
	}
	schema.Status.Target.CoordinationDigest = target
	return schema
}

func TestApplyBindingInWatch(t *testing.T) {
	t.Parallel()
	bound := func(operationID, epoch string) watchEvent[*ptahv1alpha1.PtahSchema] {
		return ftEvent(watch.Modified, ftApplySchema(ftSchema, operationID, "j1", epoch, ftDigest, ftDigest))
	}
	unbound := func(operationID, epoch string) watchEvent[*ptahv1alpha1.PtahSchema] {
		return ftEvent(watch.Modified, ftApplySchema(ftSchema, operationID, "j1", epoch, ftDigest, "sha256:other"))
	}
	planning := ftApplySchema(ftSchema, "op-a", "j1", ftEpoch, ftDigest, ftDigest)
	planning.Status.ActiveOperation.Type = ptahv1alpha1.OperationPlan
	for _, test := range []struct {
		name             string
		schemas          []watchEvent[*ptahv1alpha1.PtahSchema]
		jobUID           string
		count            int
		operation, epoch string
		fails            bool
	}{
		{"one complete binding", []watchEvent[*ptahv1alpha1.PtahSchema]{bound("op-a", ftEpoch)}, "j1", 1, "op-a", ftEpoch, false},
		{"one binding read twice", []watchEvent[*ptahv1alpha1.PtahSchema]{bound("op-a", ftEpoch), bound("op-a", ftEpoch)},
			"j1", 1, "op-a", ftEpoch, false},
		{"two epochs of one Job", []watchEvent[*ptahv1alpha1.PtahSchema]{bound("op-a", ftEpoch), bound("op-a", ftOtherEpoch)},
			"j1", 2, "", "", false},
		{"an invalid epoch binds nothing", []watchEvent[*ptahv1alpha1.PtahSchema]{bound("op-a", "v1-not-hex")}, "j1", 0, "", "", false},
		{"an absent epoch binds nothing", []watchEvent[*ptahv1alpha1.PtahSchema]{bound("op-a", "")}, "j1", 0, "", "", false},
		{"an empty operation ID binds nothing", []watchEvent[*ptahv1alpha1.PtahSchema]{bound("", ftEpoch)}, "j1", 0, "", "", false},
		{"a coordination digest other than the target's", []watchEvent[*ptahv1alpha1.PtahSchema]{unbound("op-a", ftEpoch)},
			"j1", 0, "", "", false},
		{"no coordination digest on either side still binds", []watchEvent[*ptahv1alpha1.PtahSchema]{
			ftEvent(watch.Modified, ftApplySchema(ftSchema, "op-a", "j1", ftEpoch, "", "")),
		}, "j1", 1, "op-a", ftEpoch, false},
		{"another Job", []watchEvent[*ptahv1alpha1.PtahSchema]{bound("op-a", ftEpoch)}, "j2", 0, "", "", false},
		{"an empty Job UID names nothing", []watchEvent[*ptahv1alpha1.PtahSchema]{
			ftEvent(watch.Modified, ftApplySchema(ftSchema, "op-a", "", ftEpoch, ftDigest, ftDigest)),
		}, "", 0, "", "", false},
		{"another schema", []watchEvent[*ptahv1alpha1.PtahSchema]{
			ftEvent(watch.Modified, ftApplySchema("other", "op-a", "j1", ftEpoch, ftDigest, ftDigest)),
		}, "j1", 0, "", "", false},
		{"a Plan operation", []watchEvent[*ptahv1alpha1.PtahSchema]{ftEvent(watch.Modified, planning)}, "j1", 0, "", "", false},
		{"a document of the operation without an epoch is refused",
			[]watchEvent[*ptahv1alpha1.PtahSchema]{bound("op-a", ftEpoch), unbound("op-a", "")}, "j1", 1, "op-a", "", true},
		{"the smallest operation ID the Job was bound to is read, as unique | .[0] read it",
			[]watchEvent[*ptahv1alpha1.PtahSchema]{bound("op-b", ftEpoch), unbound("op-a", "v1-not-hex")},
			"j1", 1, "op-a", "v1-not-hex", false},
		{"the smallest epoch of the operation is read",
			[]watchEvent[*ptahv1alpha1.PtahSchema]{bound("op-a", ftOtherEpoch), unbound("op-a", ftThirdEpoch)},
			"j1", 1, "op-a", ftThirdEpoch, false},
	} {
		count, operation, epoch, err := applyBindingInWatch(test.schemas, ftSchema, test.jobUID)
		if count != test.count || operation != test.operation || epoch != test.epoch || (err != nil) != test.fails {
			t.Errorf("%s: got %d %q %q %v, want %d %q %q failing %t",
				test.name, count, operation, epoch, err, test.count, test.operation, test.epoch, test.fails)
		}
	}
}

type ftResultBinding struct {
	schemas                              []watchEvent[*ptahv1alpha1.PtahSchema]
	jobs                                 []watchEvent[*batchv1.Job]
	name, operation, operationID, jobUID string
}

func TestResultBoundInWatch(t *testing.T) {
	t.Parallel()
	fresh := func() *ftResultBinding {
		schema := ftSchemaNamed(ftSchema)
		schema.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{
			Type: ptahv1alpha1.OperationObserve, ID: "op-o", JobUID: "j1",
		}
		job := ftJob("j1", ftSchema, "observe")
		job.Annotations[annotationOperationID] = "op-o"
		return &ftResultBinding{
			schemas: []watchEvent[*ptahv1alpha1.PtahSchema]{ftEvent(watch.Modified, schema)},
			jobs:    []watchEvent[*batchv1.Job]{ftJobBookmark(), ftEvent(watch.Added, job)},
			name:    ftSchema, operation: "observe", operationID: "op-o", jobUID: "j1",
		}
	}
	accepts := func(r *ftResultBinding) bool {
		return resultBoundInWatch(r.schemas, r.jobs, r.name, r.operation, r.operationID, r.jobUID)
	}
	active := func(r *ftResultBinding) *ptahv1alpha1.ActiveOperationStatus {
		return r.schemas[0].Object.Status.ActiveOperation
	}
	job := func(r *ftResultBinding) *batchv1.Job { return r.jobs[1].Object }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ftResultBinding]{
		{"the schema bound a Plan", func(r *ftResultBinding) { active(r).Type = ptahv1alpha1.OperationPlan }},
		{"the schema bound another operation ID", func(r *ftResultBinding) { active(r).ID = "op-other" }},
		{"the schema bound another Job", func(r *ftResultBinding) { active(r).JobUID = "j2" }},
		{"another schema bound it", func(r *ftResultBinding) { r.schemas[0].Object.Name = "other" }},
		{"no operation was active", func(r *ftResultBinding) { r.schemas[0].Object.Status.ActiveOperation = nil }},
		{"the Job was only ever modified", func(r *ftResultBinding) { r.jobs[1].Type = watch.Modified }},
		{"the added Job is another", func(r *ftResultBinding) { job(r).UID = "j2" }},
		{"the added Job is another operation's", func(r *ftResultBinding) { job(r).Labels[labelOperation] = "plan" }},
		{"the added Job is another schema's", func(r *ftResultBinding) { job(r).Labels[labelSchema] = "other" }},
		{"the added Job names another operation ID", func(r *ftResultBinding) { job(r).Annotations[annotationOperationID] = "op-x" }},
		{"the added Job names no operation ID", func(r *ftResultBinding) { delete(job(r).Annotations, annotationOperationID) }},
		{"an empty Job UID names nothing", func(r *ftResultBinding) {
			r.jobUID, active(r).JobUID, job(r).UID = "", "", ""
		}},
	})
}

type ftLeaseHistory struct {
	leases             []watchEvent[*coordinationv1.Lease]
	uid, holder, epoch string
}

func TestLeaseHeldWithoutRelease(t *testing.T) {
	t.Parallel()
	fresh := func() *ftLeaseHistory {
		return &ftLeaseHistory{leases: []watchEvent[*coordinationv1.Lease]{
			ftEvent(watch.Added, ftLease("lease-a", "L", "", "")),
			ftEvent(watch.Modified, ftLease("lease-a", "L", "h1", ftEpoch)),
			ftEvent(watch.Modified, ftLease("lease-a", "L", "h1", ftEpoch)),
			ftEvent(watch.Added, ftLease("lease-b", "L2", "hx", ftOtherEpoch)),
			ftEvent(watch.Deleted, ftLease("lease-b", "L2", "", ftOtherEpoch)),
		}, uid: "L", holder: "h1", epoch: ftEpoch}
	}
	accepts := func(h *ftLeaseHistory) bool { return leaseHeldWithoutRelease(h.leases, h.uid, h.holder, h.epoch) }
	later := func(h *ftLeaseHistory, eventType watch.EventType, holder, epoch string) {
		h.leases = append(h.leases, ftEvent(eventType, ftLease("lease-a", "L", holder, epoch)))
	}
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ftLeaseHistory]{
		{"released after it was acquired", func(h *ftLeaseHistory) { later(h, watch.Modified, "", ftEpoch) }},
		{"its holder cleared to empty", func(h *ftLeaseHistory) {
			lease := ftLease("lease-a", "L", "", ftEpoch)
			lease.Spec.HolderIdentity = ptr.To("")
			h.leases = append(h.leases, ftEvent(watch.Modified, lease))
		}},
		{"taken by another holder", func(h *ftLeaseHistory) { later(h, watch.Modified, "h2", ftEpoch) }},
		{"acquired again at another epoch", func(h *ftLeaseHistory) { later(h, watch.Modified, "h1", ftOtherEpoch) }},
		{"deleted", func(h *ftLeaseHistory) { later(h, watch.Deleted, "h1", ftEpoch) }},
		{"never held by the holder", func(h *ftLeaseHistory) { h.holder = "h9" }},
		{"an empty UID names nothing", func(h *ftLeaseHistory) { h.uid = "" }},
		{"an empty epoch names no absent annotation", func(h *ftLeaseHistory) {
			h.epoch = ""
			for _, event := range h.leases {
				event.Object.Annotations = nil
			}
		}},
	})
	ftAcceptsEach(t, fresh, accepts, []ftMutation[*ftLeaseHistory]{
		{"a release before the acquisition does not count", func(h *ftLeaseHistory) {
			h.leases = slices.Insert(h.leases, 1, ftEvent(watch.Modified, ftLease("lease-a", "L", "h0", ftOtherEpoch)))
		}},
		{"a bookmark does not count", func(h *ftLeaseHistory) {
			h.leases = append(h.leases, watchEvent[*coordinationv1.Lease]{Type: watch.Bookmark,
				Object: &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "9"}}})
		}},
	})
}

type ftReacquisition struct {
	leases  []watchEvent[*coordinationv1.Lease]
	schemas []watchEvent[*ptahv1alpha1.PtahSchema]
	jobs    []watchEvent[*batchv1.Job]
	want    leaseReacquisition
}

func TestSameLeaseReacquired(t *testing.T) {
	t.Parallel()
	const contender = "e2e-fault-pg-alias-b"
	fresh := func() *ftReacquisition {
		job := ftJob("jB", contender, "apply")
		job.Annotations[annotationOperationID] = "opB"
		return &ftReacquisition{
			leases: []watchEvent[*coordinationv1.Lease]{
				ftEvent(watch.Added, ftLease("lease-a", "L", "", "")),
				ftEvent(watch.Modified, ftLease("lease-a", "L", "h1", ftEpoch)),
				ftEvent(watch.Modified, ftLease("lease-a", "L", "", ftEpoch)),
				ftEvent(watch.Modified, ftLease("lease-b", "L9", "h9", ftThirdEpoch)),
				ftEvent(watch.Modified, ftLease("lease-a", "L", "", ftEpoch)),
				ftEvent(watch.Modified, ftLease("lease-a", "L", "h2", ftOtherEpoch)),
			},
			schemas: []watchEvent[*ptahv1alpha1.PtahSchema]{
				ftEvent(watch.Modified, ftApplySchema(contender, "opB", "jB", ftOtherEpoch, ftDigest, ftDigest)),
			},
			jobs: []watchEvent[*batchv1.Job]{ftEvent(watch.Added, job)},
			want: leaseReacquisition{
				uid: "L", firstHolder: "h1", firstEpoch: ftEpoch,
				schema: contender, operationID: "opB", jobUID: "jB", contenderEpoch: ftOtherEpoch,
			},
		}
	}
	accepts := func(r *ftReacquisition) bool { return sameLeaseReacquired(r.leases, r.schemas, r.jobs, r.want) }
	lease := func(r *ftReacquisition, index int) *coordinationv1.Lease { return r.leases[index].Object }
	active := func(r *ftReacquisition) *ptahv1alpha1.ActiveOperationStatus {
		return r.schemas[0].Object.Status.ActiveOperation
	}
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ftReacquisition]{
		{"never held by the first holder", func(r *ftReacquisition) { lease(r, 1).Spec.HolderIdentity = ptr.To("h7") }},
		{"first held at another epoch", func(r *ftReacquisition) { lease(r, 1).Annotations[annotationLeaseEpoch] = ftThirdEpoch }},
		{"never released", func(r *ftReacquisition) {
			r.leases = []watchEvent[*coordinationv1.Lease]{r.leases[0], r.leases[1], r.leases[3], r.leases[5]}
		}},
		{"reacquired by recreation", func(r *ftReacquisition) { r.leases[5].Type = watch.Added }},
		{"reacquired under another UID", func(r *ftReacquisition) { lease(r, 5).UID = "L2" }},
		{"reacquired by the first holder", func(r *ftReacquisition) { lease(r, 5).Spec.HolderIdentity = ptr.To("h1") }},
		{"reacquired at an epoch other than the contender's", func(r *ftReacquisition) {
			lease(r, 5).Annotations[annotationLeaseEpoch] = ftThirdEpoch
		}},
		{"reacquired at the first epoch", func(r *ftReacquisition) {
			lease(r, 5).Annotations[annotationLeaseEpoch] = ftEpoch
			r.want.contenderEpoch = ftEpoch
			active(r).LeaseEpoch = ftEpoch
		}},
		{"deleted between the release and the reacquisition", func(r *ftReacquisition) { r.leases[4].Type = watch.Deleted }},
		{"recreated under another UID between them", func(r *ftReacquisition) { lease(r, 4).UID = "L2" }},
		{"never reacquired", func(r *ftReacquisition) { r.leases = r.leases[:5] }},
		{"the contender was never bound", func(r *ftReacquisition) { r.schemas = nil }},
		{"the contender was bound at another epoch", func(r *ftReacquisition) { active(r).LeaseEpoch = ftThirdEpoch }},
		{"the contender's coordination is not its target's", func(r *ftReacquisition) {
			r.schemas[0].Object.Status.Target.CoordinationDigest = "sha256:other"
		}},
		{"the contender was bound to another Job", func(r *ftReacquisition) { active(r).JobUID = "jX" }},
		{"the contender was bound to another operation", func(r *ftReacquisition) { active(r).ID = "opX" }},
		{"the contender's Apply Job was never added", func(r *ftReacquisition) { r.jobs[0].Type = watch.Modified }},
		{"the Apply Job carries another operation", func(r *ftReacquisition) {
			r.jobs[0].Object.Annotations[annotationOperationID] = "opX"
		}},
		{"the Apply Job is another schema's", func(r *ftReacquisition) { r.jobs[0].Object.Labels[labelSchema] = "other" }},
		{"an empty UID names nothing", func(r *ftReacquisition) { r.want.uid = "" }},
	})
	ftAcceptsEach(t, fresh, accepts, []ftMutation[*ftReacquisition]{
		{"a release to an empty holder", func(r *ftReacquisition) { lease(r, 2).Spec.HolderIdentity = ptr.To("") }},
		{"a Lease of another name held between them", func(r *ftReacquisition) {
			r.leases = slices.Insert(r.leases, 4, ftEvent(watch.Modified, ftLease("lease-c", "L8", "h8", ftThirdEpoch)))
		}},
	})
}

func TestInstantOfIsTheAPIServersSpelling(t *testing.T) {
	t.Parallel()
	zone := time.FixedZone("east", 3*3600)
	if got := instantOf(metav1.Time{}); got != "" {
		t.Errorf("a zero time spells %q, want empty", got)
	}
	if got := instantOf(metav1.NewTime(time.Date(2026, 9, 28, 13, 0, 5, 999, zone))); got != ftStartedAt {
		t.Errorf("got %q, want %q", got, ftStartedAt)
	}
}

type ftOwners struct {
	references      []metav1.OwnerReference
	jobName, jobUID string
}

func TestExactJobOwner(t *testing.T) {
	t.Parallel()
	fresh := func() *ftOwners {
		return &ftOwners{references: []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "Job", Name: "apply-job", UID: "j1", Controller: ptr.To(true),
		}}, jobName: "apply-job", jobUID: "j1"}
	}
	accepts := func(o *ftOwners) bool { return exactJobOwner(o.references, o.jobName, o.jobUID) }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ftOwners]{
		{"a second owner", func(o *ftOwners) {
			o.references = append(o.references, metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "c"})
		}},
		{"no owner", func(o *ftOwners) { o.references = nil }},
		{"another API version", func(o *ftOwners) { o.references[0].APIVersion = "batch/v1beta1" }},
		{"another kind", func(o *ftOwners) { o.references[0].Kind = "CronJob" }},
		{"another name", func(o *ftOwners) { o.jobName = "other-job" }},
		{"another UID", func(o *ftOwners) { o.jobUID = "j2" }},
		{"not the controller", func(o *ftOwners) { o.references[0].Controller = ptr.To(false) }},
		{"no controller field", func(o *ftOwners) { o.references[0].Controller = nil }},
		{"an empty UID names nothing", func(o *ftOwners) { o.jobUID, o.references[0].UID = "", "" }},
	})
}

func ftDeadlinePod() deadlinePod {
	return deadlinePod{
		name: "apply-pod", uid: "p1", jobName: "apply-job", jobUID: "j1", operationID: "op-apply", startedAt: ftStartedAt,
	}
}

// ftApplyPod is the running-deadline Apply Pod in the phase given: running
// adds the node and the ptah container started at ftStartedAt.
func ftApplyPod(phase corev1.PodPhase) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "apply-pod", UID: "p1",
			Labels:      map[string]string{labelSchema: ftSchema, labelOperation: "apply"},
			Annotations: map[string]string{annotationOperationID: "op-apply"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: "apply-job", UID: "j1", Controller: ptr.To(true),
			}},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	if phase == corev1.PodRunning {
		pod.Spec.NodeName = "kind-worker"
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "ptah", State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(ftStart.Add(5 * time.Second))},
		}}}
	}
	return pod
}

type ftDeadlineHistory struct {
	pods []watchEvent[*corev1.Pod]
	pod  deadlinePod
}

func ftDeadlineFresh() *ftDeadlineHistory {
	stranger := ftPod("p9", ftSchema, "plan", corev1.PodRunning)
	stranger.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "ptah", RestartCount: 3}}
	failed := ftApplyPod(corev1.PodFailed)
	failed.Spec.NodeName = "kind-worker"
	failed.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "ptah", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 137},
	}}}
	deleted := failed.DeepCopy()
	return &ftDeadlineHistory{pods: []watchEvent[*corev1.Pod]{
		ftEvent(watch.Added, ftApplyPod(corev1.PodPending)),
		ftEvent(watch.Modified, ftApplyPod(corev1.PodRunning)),
		ftEvent(watch.Modified, ftApplyPod(corev1.PodRunning)),
		ftEvent(watch.Modified, failed),
		ftEvent(watch.Deleted, deleted),
		ftEvent(watch.Added, stranger),
	}, pod: ftDeadlinePod()}
}

// ftDeadlineMutations break the Pod's own story, which both deadline readings
// hold.
func ftDeadlineMutations() []ftMutation[*ftDeadlineHistory] {
	running := func(h *ftDeadlineHistory, change func(*corev1.Pod)) {
		change(h.pods[1].Object)
		change(h.pods[2].Object)
	}
	return []ftMutation[*ftDeadlineHistory]{
		{"never running", func(h *ftDeadlineHistory) { running(h, func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }) }},
		{"running on no node", func(h *ftDeadlineHistory) { running(h, func(p *corev1.Pod) { p.Spec.NodeName = "" }) }},
		{"running since another instant", func(h *ftDeadlineHistory) { h.pod.startedAt = "2026-09-28T10:00:06Z" }},
		{"ptah never ran", func(h *ftDeadlineHistory) {
			running(h, func(p *corev1.Pod) {
				p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
			})
		}},
		{"another container ran", func(h *ftDeadlineHistory) {
			running(h, func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Name = "runner" })
		}},
		{"an empty start instant names none", func(h *ftDeadlineHistory) {
			h.pod.startedAt = ""
			running(h, func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.Time{} })
		}},
		{"never deleted", func(h *ftDeadlineHistory) { h.pods = slices.Delete(h.pods, 4, 5) }},
		{"deleted before it ran", func(h *ftDeadlineHistory) {
			deleted := h.pods[4]
			h.pods = slices.Insert(slices.Delete(h.pods, 4, 5), 1, deleted)
		}},
		{"an event renamed it", func(h *ftDeadlineHistory) { h.pods[3].Object.Name = "other" }},
		{"an event lost its operation", func(h *ftDeadlineHistory) {
			delete(h.pods[2].Object.Annotations, annotationOperationID)
		}},
		{"an event gained a second owner", func(h *ftDeadlineHistory) {
			h.pods[3].Object.OwnerReferences = append(h.pods[3].Object.OwnerReferences,
				metav1.OwnerReference{APIVersion: "v1", Kind: "Node", Name: "n", UID: "n"})
		}},
		{"its owner is not the controller", func(h *ftDeadlineHistory) {
			h.pods[0].Object.OwnerReferences[0].Controller = ptr.To(false)
		}},
		{"an init container restarted", func(h *ftDeadlineHistory) {
			h.pods[3].Object.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "fetch", RestartCount: 1}}
		}},
		{"an ephemeral container restarted", func(h *ftDeadlineHistory) {
			h.pods[2].Object.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debug", RestartCount: 1}}
		}},
	}
}

func TestDeadlinePodWatchAudited(t *testing.T) {
	t.Parallel()
	accepts := func(h *ftDeadlineHistory) bool {
		_, ok := deadlinePodWatchAudited(h.pods, h.pod)
		return ok
	}
	ftRefusesEach(t, ftDeadlineFresh, accepts, append(ftDeadlineMutations(), []ftMutation[*ftDeadlineHistory]{
		{"a second Apply Pod under its Job", func(h *ftDeadlineHistory) {
			second := ftApplyPod(corev1.PodPending)
			second.Name, second.UID = "apply-pod-2", "p2"
			h.pods = append(h.pods, ftEvent(watch.Added, second))
		}},
		{"its addition is not an Apply's", func(h *ftDeadlineHistory) { h.pods[0].Object.Labels[labelOperation] = "plan" }},
		{"its addition names another Job", func(h *ftDeadlineHistory) {
			h.pods[0].Object.OwnerReferences[0].Name = "other-job"
		}},
		{"it was only ever modified", func(h *ftDeadlineHistory) { h.pods[0].Type = watch.Modified }},
	}...))
	ftAcceptsEach(t, ftDeadlineFresh, accepts, []ftMutation[*ftDeadlineHistory]{
		{"its addition carries another schema label", func(h *ftDeadlineHistory) { h.pods[0].Object.Labels[labelSchema] = "other" }},
	})

	history := ftDeadlineFresh()
	again := history.pods[4].Object.DeepCopy()
	history.pods = append(history.pods, ftEvent(watch.Deleted, again))
	deleted, ok := deadlinePodWatchAudited(history.pods, history.pod)
	if !ok || deleted != again {
		t.Errorf("the audit did not return the last deletion: %t, %p, want %p", ok, deleted, again)
	}
}

func TestDeadlinePodHistory(t *testing.T) {
	t.Parallel()
	accepts := func(h *ftDeadlineHistory) bool { return deadlinePodHistory(h.pods, ftSchema, h.pod) }
	ftRefusesEach(t, ftDeadlineFresh, accepts, append(ftDeadlineMutations(), []ftMutation[*ftDeadlineHistory]{
		{"a second Apply Pod for the schema", func(h *ftDeadlineHistory) {
			h.pods = append(h.pods, ftEvent(watch.Added, ftPod("p2", ftSchema, "apply", corev1.PodPending)))
		}},
		{"its addition is another schema's", func(h *ftDeadlineHistory) { h.pods[0].Object.Labels[labelSchema] = "other" }},
		{"it was only ever modified", func(h *ftDeadlineHistory) { h.pods[0].Type = watch.Modified }},
	}...))
	ftAcceptsEach(t, ftDeadlineFresh, accepts, []ftMutation[*ftDeadlineHistory]{
		{"another Pod under its Job without the schema's labels", func(h *ftDeadlineHistory) {
			second := ftApplyPod(corev1.PodPending)
			second.Name, second.UID, second.Labels = "apply-pod-2", "p2", nil
			h.pods = append(h.pods, ftEvent(watch.Added, second))
		}},
	})
}

// ftDeadlineJob is the Apply Job with the deadline given on the Job and its
// template.
func ftDeadlineJob(uid string, deadline int64) *batchv1.Job {
	job := ftJob(uid, ftSchema, "apply")
	job.Spec.ActiveDeadlineSeconds = ptr.To(deadline)
	job.Spec.Template.Spec.ActiveDeadlineSeconds = ptr.To(deadline)
	return job
}

type ftDeadlineJobs struct {
	jobs     []watchEvent[*batchv1.Job]
	uid      string
	deadline int64
}

func TestDeadlineJobHistory(t *testing.T) {
	t.Parallel()
	exceeded := func(job *batchv1.Job) *batchv1.Job {
		return ftWithCondition(job, batchv1.JobFailed, corev1.ConditionTrue, batchv1.JobReasonDeadlineExceeded)
	}
	fresh := func() *ftDeadlineJobs {
		return &ftDeadlineJobs{jobs: []watchEvent[*batchv1.Job]{
			ftEvent(watch.Added, ftDeadlineJob("j1", 45)),
			ftEvent(watch.Added, ftJob("p1", ftSchema, "plan")),
			ftEvent(watch.Modified, exceeded(ftDeadlineJob("j1", 45))),
		}, uid: "j1", deadline: 45}
	}
	accepts := func(h *ftDeadlineJobs) bool { return deadlineJobHistory(h.jobs, ftSchema, h.uid, h.deadline) }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ftDeadlineJobs]{
		{"a second Apply Job for the schema", func(h *ftDeadlineJobs) {
			h.jobs = append(h.jobs, ftEvent(watch.Added, ftDeadlineJob("j2", 45)))
		}},
		{"the Job was only ever modified", func(h *ftDeadlineJobs) { h.jobs[0].Type = watch.Modified }},
		{"it failed for another reason", func(h *ftDeadlineJobs) { h.jobs[2].Object.Status.Conditions[0].Reason = "BackoffLimitExceeded" }},
		{"its failure condition is False", func(h *ftDeadlineJobs) {
			h.jobs[2].Object.Status.Conditions[0].Status = corev1.ConditionFalse
		}},
		{"it failed under another deadline", func(h *ftDeadlineJobs) {
			h.jobs[2].Object.Spec.ActiveDeadlineSeconds = ptr.To(int64(44))
		}},
		{"its template lost the deadline", func(h *ftDeadlineJobs) {
			h.jobs[2].Object.Spec.Template.Spec.ActiveDeadlineSeconds = nil
		}},
		{"the deadline failure is another Job's", func(h *ftDeadlineJobs) { h.jobs[2].Object.UID = "j3" }},
		{"an empty UID names nothing", func(h *ftDeadlineJobs) {
			h.uid, h.jobs[0].Object.UID, h.jobs[2].Object.UID = "", "", ""
		}},
	})
}

type ftExceeded struct {
	job      *batchv1.Job
	uid      string
	deadline int64
}

func TestDeadlineJobExceeded(t *testing.T) {
	t.Parallel()
	fresh := func() *ftExceeded {
		job := ftWithCondition(ftDeadlineJob("j1", 45), batchv1.JobFailed, corev1.ConditionTrue, batchv1.JobReasonDeadlineExceeded)
		job.Status.StartTime = ptr.To(metav1.NewTime(ftStart))
		job.Status.Conditions[0].LastTransitionTime = metav1.NewTime(ftStart.Add(45 * time.Second))
		return &ftExceeded{job: job, uid: "j1", deadline: 45}
	}
	accepts := func(d *ftExceeded) bool { return deadlineJobExceeded(d.job, d.uid, d.deadline) }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ftExceeded]{
		{"another Job", func(d *ftExceeded) { d.uid = "j2" }},
		{"an empty UID names nothing", func(d *ftExceeded) { d.uid, d.job.UID = "", "" }},
		{"another deadline on the Job", func(d *ftExceeded) { d.job.Spec.ActiveDeadlineSeconds = ptr.To(int64(30)) }},
		{"no deadline on its template", func(d *ftExceeded) { d.job.Spec.Template.Spec.ActiveDeadlineSeconds = nil }},
		{"never started", func(d *ftExceeded) { d.job.Status.StartTime = nil }},
		{"never failed", func(d *ftExceeded) { d.job.Status.Conditions = nil }},
		{"failed for another reason", func(d *ftExceeded) { d.job.Status.Conditions[0].Reason = "BackoffLimitExceeded" }},
		{"its failure is False", func(d *ftExceeded) { d.job.Status.Conditions[0].Status = corev1.ConditionFalse }},
		{"failed more than a second before its deadline", func(d *ftExceeded) {
			d.job.Status.Conditions[0].LastTransitionTime = metav1.NewTime(ftStart.Add(43 * time.Second))
		}},
	})
	ftAcceptsEach(t, fresh, accepts, []ftMutation[*ftExceeded]{
		{"failed one second before its deadline", func(d *ftExceeded) {
			d.job.Status.Conditions[0].LastTransitionTime = metav1.NewTime(ftStart.Add(44 * time.Second))
		}},
	})
}

func TestRunningDeadlineJob(t *testing.T) {
	t.Parallel()
	fresh := func() *batchv1.Job {
		job := ftDeadlineJob("j1", 45)
		job.Annotations[annotationOperationID] = "op-apply"
		job.Spec.Parallelism, job.Spec.Completions, job.Spec.BackoffLimit = ptr.To(int32(1)), ptr.To(int32(1)), ptr.To(int32(0))
		job.Spec.PodReplacementPolicy = ptr.To(batchv1.Failed)
		job.Status.StartTime = ptr.To(metav1.NewTime(ftStart))
		return job
	}
	accepts := func(job *batchv1.Job) bool { return runningDeadlineJob(job, "j1", ftSchema, "op-apply", 45) }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*batchv1.Job]{
		{"another Job", func(job *batchv1.Job) { job.UID = "j2" }},
		{"being deleted", func(job *batchv1.Job) { job.DeletionTimestamp = ptr.To(metav1.NewTime(ftStart)) }},
		{"another schema's", func(job *batchv1.Job) { job.Labels[labelSchema] = "other" }},
		{"not an Apply", func(job *batchv1.Job) { job.Labels[labelOperation] = "plan" }},
		{"another operation", func(job *batchv1.Job) { job.Annotations[annotationOperationID] = "op-other" }},
		{"another deadline", func(job *batchv1.Job) { job.Spec.ActiveDeadlineSeconds = ptr.To(int64(7200)) }},
		{"no deadline on its template", func(job *batchv1.Job) { job.Spec.Template.Spec.ActiveDeadlineSeconds = nil }},
		{"two Pods at once", func(job *batchv1.Job) { job.Spec.Parallelism = ptr.To(int32(2)) }},
		{"no parallelism", func(job *batchv1.Job) { job.Spec.Parallelism = nil }},
		{"no completions", func(job *batchv1.Job) { job.Spec.Completions = nil }},
		{"a retry", func(job *batchv1.Job) { job.Spec.BackoffLimit = ptr.To(int32(1)) }},
		{"a Pod replaced while terminating", func(job *batchv1.Job) {
			job.Spec.PodReplacementPolicy = ptr.To(batchv1.TerminatingOrFailed)
		}},
		{"no replacement policy", func(job *batchv1.Job) { job.Spec.PodReplacementPolicy = nil }},
		{"never started", func(job *batchv1.Job) { job.Status.StartTime = nil }},
	})
}

func TestRunningDeadlinePod(t *testing.T) {
	t.Parallel()
	fresh := func() *corev1.Pod {
		pod := ftApplyPod(corev1.PodRunning)
		pod.Spec.ActiveDeadlineSeconds = ptr.To(int64(45))
		pod.Status.StartTime = ptr.To(metav1.NewTime(ftStart))
		return pod
	}
	accepts := func(pod *corev1.Pod) bool {
		startedAt, ok := runningDeadlinePod(pod, "p1", "apply-job", "j1", "op-apply", 45)
		return ok && startedAt == ftStartedAt
	}
	ftRefusesEach(t, fresh, accepts, []ftMutation[*corev1.Pod]{
		{"another Pod", func(pod *corev1.Pod) { pod.UID = "p2" }},
		{"being deleted", func(pod *corev1.Pod) { pod.DeletionTimestamp = ptr.To(metav1.NewTime(ftStart)) }},
		{"another operation", func(pod *corev1.Pod) { pod.Annotations[annotationOperationID] = "op-other" }},
		{"a second owner", func(pod *corev1.Pod) {
			pod.OwnerReferences = append(pod.OwnerReferences, metav1.OwnerReference{APIVersion: "v1", Kind: "Node", Name: "n"})
		}},
		{"its owner is not the controller", func(pod *corev1.Pod) { pod.OwnerReferences[0].Controller = nil }},
		{"another deadline", func(pod *corev1.Pod) { pod.Spec.ActiveDeadlineSeconds = ptr.To(int64(7200)) }},
		{"no node", func(pod *corev1.Pod) { pod.Spec.NodeName = "" }},
		{"pending", func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodPending }},
		{"never started", func(pod *corev1.Pod) { pod.Status.StartTime = nil }},
		{"ptah not running", func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
		}},
		{"ptah running with no start time", func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.Time{}
		}},
		{"ptah reported twice", func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, pod.Status.ContainerStatuses[0])
		}},
		{"an init container restarted", func(pod *corev1.Pod) {
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "fetch", RestartCount: 1}}
		}},
	})
}

// ftManagerPod is a manager Pod of the release, ready or not.
func ftManagerPod(uid string, ready bool) corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "manager-" + uid, UID: types.UID(uid), Labels: map[string]string{
			"app.kubernetes.io/instance": "ptah-e2e", "app.kubernetes.io/component": "controller",
		}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

func TestReadyManagerPodUIDs(t *testing.T) {
	t.Parallel()
	terminating := ftManagerPod("old", false)
	terminating.DeletionTimestamp = ptr.To(metav1.NewTime(ftStart))
	for _, test := range []struct {
		name     string
		pods     []corev1.Pod
		replicas int
		want     []string
		ok       bool
	}{
		{"two ready HA replicas, sorted", []corev1.Pod{ftManagerPod("manager-b", true), ftManagerPod("manager-a", true)},
			2, []string{"manager-a", "manager-b"}, true},
		{"a stale replica that is not terminating", []corev1.Pod{
			ftManagerPod("old-manager", false), ftManagerPod("new-manager-a", true), ftManagerPod("new-manager-b", true),
		}, 2, nil, false},
		{"a terminating replica is left out", []corev1.Pod{
			terminating, ftManagerPod("new-b", true), ftManagerPod("new-a", true),
		}, 2, []string{"new-a", "new-b"}, true},
		{"a replica short", []corev1.Pod{ftManagerPod("a", true)}, 2, nil, false},
		{"a live replica not ready", []corev1.Pod{ftManagerPod("a", true), ftManagerPod("b", false)}, 2, nil, false},
		{"a live replica with no Ready condition", []corev1.Pod{ftManagerPod("a", true), {
			ObjectMeta: metav1.ObjectMeta{UID: "b"},
		}}, 2, nil, false},
		{"one UID twice", []corev1.Pod{ftManagerPod("a", true), ftManagerPod("a", true)}, 2, nil, false},
		{"more ready Pods than replicas", []corev1.Pod{ftManagerPod("a", true), ftManagerPod("b", true)}, 1, nil, false},
		{"no replica and no Pod", nil, 0, nil, true},
	} {
		got, ok := readyManagerPodUIDs(test.pods, test.replicas)
		if ok != test.ok || !slices.Equal(got, test.want) {
			t.Errorf("%s: got %v %t, want %v %t", test.name, got, ok, test.want, test.ok)
		}
	}
}

func TestManagerPodsReplaced(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		before, after []string
		want          bool
	}{
		{"disjoint sets of one size", []string{"old-a", "old-b"}, []string{"new-a", "new-b"}, true},
		{"an old Pod kept", []string{"old-a", "old-b"}, []string{"old-b", "new-a"}, false},
		{"a replica lost", []string{"old-a", "old-b"}, []string{"new-a"}, false},
		{"a replica gained", []string{"old-a"}, []string{"new-a", "new-b"}, false},
		{"no Pod on either side", nil, nil, false},
	} {
		if got := managerPodsReplaced(test.before, test.after); got != test.want {
			t.Errorf("%s: got %t, want %t", test.name, got, test.want)
		}
	}
}

func TestLeaderPodNameDropsTheLastSuffix(t *testing.T) {
	t.Parallel()
	for holder, want := range map[string]string{
		"ptah-operator-7d9f-abcde_3c1e2a4b-uuid": "ptah-operator-7d9f-abcde",
		"a_b_c":                                  "a_b",
		"pod_":                                   "pod",
		"nounderscore":                           "nounderscore",
		"":                                       "",
	} {
		if got := leaderPodName(holder); got != want {
			t.Errorf("%q: got %q, want %q", holder, got, want)
		}
	}
}

type ftLeader struct {
	pod                  *corev1.Pod
	previousUID, release string
}

func TestLeaderPodReady(t *testing.T) {
	t.Parallel()
	fresh := func() *ftLeader {
		pod := ftManagerPod("new", true)
		return &ftLeader{pod: &pod, previousUID: "old", release: "ptah-e2e"}
	}
	accepts := func(l *ftLeader) bool { return leaderPodReady(l.pod, l.previousUID, l.release) }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ftLeader]{
		{"terminating", func(l *ftLeader) { l.pod.DeletionTimestamp = ptr.To(metav1.NewTime(ftStart)) }},
		{"the previous leader", func(l *ftLeader) { l.previousUID = "new" }},
		{"another release", func(l *ftLeader) { l.release = "other" }},
		{"not a controller Pod", func(l *ftLeader) { l.pod.Labels["app.kubernetes.io/component"] = "certificate-rotator" }},
		{"no labels", func(l *ftLeader) { l.pod.Labels = nil }},
		{"not ready", func(l *ftLeader) { l.pod.Status.Conditions[0].Status = corev1.ConditionFalse }},
	})
	ftAcceptsEach(t, fresh, accepts, []ftMutation[*ftLeader]{
		{"no previous leader named", func(l *ftLeader) { l.previousUID = "" }},
	})
}

func TestReplaceDatabaseURLPath(t *testing.T) {
	t.Parallel()
	// The results are what the shell's replace_database_url_path printed for
	// each input, the database replaced by newdb.
	for _, test := range []struct {
		input, want string
	}{
		{"postgres://user:password@db.example/original", "postgres://user:password@db.example/newdb"},
		{`postgres://user:password@db.example/original?sslrootcert=/tmp/root&ampersand=a&path=one\\two`,
			`postgres://user:password@db.example/newdb?sslrootcert=/tmp/root&ampersand=a&path=one\\two`},
		{"mysql://user:password@db.example/original#client-fragment", "mysql://user:password@db.example/newdb#client-fragment"},
		{"postgres://h/db/", "postgres://h/db/newdb"},
		{"postgres://h/a/b", "postgres://h/a/newdb"},
		{"postgres://h/db?x=1#frag", "postgres://h/newdb?x=1#frag"},
		{"postgres://h/db#frag?x=1", "postgres://h/newdb?x=1"},
		{"a://b://c/d", "a://b://c/newdb"},
		{"://x/y", "://x/newdb"},
		{"postgres://h/d", "postgres://h/newdb"},
	} {
		got, err := replaceDatabaseURLPath(test.input, "newdb")
		if err != nil || got != test.want {
			t.Errorf("%s: got %q %v, want %q", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{"postgres://db.example?sslmode=disable", "postgres://h/", "no-scheme/db", "postgres://"} {
		if got, err := replaceDatabaseURLPath(input, "newdb"); err == nil {
			t.Errorf("%s: accepted a URL without a database path as %q", input, got)
		}
	}
}

func TestShortServiceURL(t *testing.T) {
	t.Parallel()
	long := "postgres://u:p@e2e-postgresql.ptah-e2e.svc.cluster.local:5432/db?host=e2e-postgresql.ptah-e2e.svc.cluster.local"
	got, err := shortServiceURL(long, "e2e-postgresql", "ptah-e2e")
	if want := "postgres://u:p@e2e-postgresql:5432/db?host=e2e-postgresql.ptah-e2e.svc.cluster.local"; err != nil || got != want {
		t.Errorf("got %q %v, want %q: only the first spelling is replaced, as sed replaced it", got, err, want)
	}
	if got, err := shortServiceURL("postgres://u:p@e2e-postgresql:5432/db", "e2e-postgresql", "ptah-e2e"); err == nil {
		t.Errorf("an unchanged route was accepted as %q", got)
	}
}

func TestCoordinationLeaseUIDs(t *testing.T) {
	t.Parallel()
	got := coordinationLeaseUIDs([]coordinationv1.Lease{*ftLease("b", "L2", "", ""), *ftLease("a", "L1", "", ""), *ftLease("c", "L2", "", "")})
	if !slices.Equal(got, checkpoint{"L1", "L2"}) {
		t.Errorf("got %v, want [L1 L2]", got)
	}
}

func TestNewReleasedLease(t *testing.T) {
	t.Parallel()
	before := sortedCheckpoint([]string{"L1"})
	emptyHolder := ftLease("lease-2", "L2", "", ftEpoch)
	emptyHolder.Spec.HolderIdentity = ptr.To("")
	for _, test := range []struct {
		name   string
		leases []coordinationv1.Lease
		want   leaseIdentity
		count  int
		fails  bool
	}{
		{"one new released Lease", []coordinationv1.Lease{*ftLease("lease-1", "L1", "h", ftOtherEpoch), *ftLease("lease-2", "L2", "", ftEpoch)},
			leaseIdentity{name: "lease-2", uid: "L2", epoch: ftEpoch}, 1, false},
		{"released to an empty holder", []coordinationv1.Lease{*emptyHolder}, leaseIdentity{name: "lease-2", uid: "L2", epoch: ftEpoch}, 1, false},
		{"still held", []coordinationv1.Lease{*ftLease("lease-2", "L2", "h1", ftEpoch)},
			leaseIdentity{name: "lease-2", uid: "L2", epoch: ftEpoch}, 1, true},
		{"an invalid epoch", []coordinationv1.Lease{*ftLease("lease-2", "L2", "", "v1-bad")},
			leaseIdentity{name: "lease-2", uid: "L2", epoch: "v1-bad"}, 1, true},
		{"no epoch", []coordinationv1.Lease{*ftLease("lease-2", "L2", "", "")}, leaseIdentity{name: "lease-2", uid: "L2"}, 1, true},
		{"two new Leases", []coordinationv1.Lease{*ftLease("lease-2", "L2", "", ftEpoch), *ftLease("lease-3", "L3", "", ftEpoch)},
			leaseIdentity{}, 2, false},
		{"none new", []coordinationv1.Lease{*ftLease("lease-1", "L1", "", ftEpoch)}, leaseIdentity{}, 0, false},
	} {
		got, count, err := newReleasedLease(test.leases, before)
		if got != test.want || count != test.count || (err != nil) != test.fails {
			t.Errorf("%s: got %v %d %v, want %v %d failing %t", test.name, got, count, err, test.want, test.count, test.fails)
		}
	}
}

type ftReacquired struct {
	lease                *coordinationv1.Lease
	uid, previousEpochID string
}

func TestReacquiredLease(t *testing.T) {
	t.Parallel()
	fresh := func() *ftReacquired {
		return &ftReacquired{lease: ftLease("lease-a", "L", "h2", ftOtherEpoch), uid: "L", previousEpochID: ftEpoch}
	}
	accepts := func(r *ftReacquired) bool {
		identity, ok := reacquiredLease(r.lease, r.uid, r.previousEpochID)
		return ok && identity == leaseIdentity{name: "lease-a", uid: "L", holder: "h2", epoch: ftOtherEpoch}
	}
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ftReacquired]{
		{"another UID", func(r *ftReacquired) { r.uid = "L2" }},
		{"an empty UID names nothing", func(r *ftReacquired) { r.uid, r.lease.UID = "", "" }},
		{"no holder", func(r *ftReacquired) { r.lease.Spec.HolderIdentity = nil }},
		{"an empty holder", func(r *ftReacquired) { r.lease.Spec.HolderIdentity = ptr.To("") }},
		{"an invalid epoch", func(r *ftReacquired) { r.lease.Annotations[annotationLeaseEpoch] = "v1-bad" }},
		{"the epoch it was released at", func(r *ftReacquired) { r.previousEpochID = ftOtherEpoch }},
		{"no epoch", func(r *ftReacquired) { r.lease.Annotations = nil }},
	})
}

func TestHeldLeasesAtEpoch(t *testing.T) {
	t.Parallel()
	empty := ftLease("lease-f", "F", "", ftEpoch)
	empty.Spec.HolderIdentity = ptr.To("")
	leases := []coordinationv1.Lease{
		*ftLease("lease-a", "A", "h1", ftEpoch), *ftLease("lease-b", "B", "", ftEpoch),
		*ftLease("lease-c", "C", "h3", ftOtherEpoch), *ftLease("lease-d", "D", "h4", ftEpoch), *empty,
		*ftLease("lease-g", "G", "h7", ""),
	}
	got := heldLeasesAtEpoch(leases, ftEpoch)
	want := []leaseIdentity{{"lease-a", "A", "h1", ftEpoch}, {"lease-d", "D", "h4", ftEpoch}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := heldLeasesAtEpoch(leases, ""); len(got) != 0 {
		t.Errorf("an empty epoch named Leases without one: %v", got)
	}
}

func TestLeaseHolderReadings(t *testing.T) {
	t.Parallel()
	held, released := ftLease("a", "L", "h1", ftEpoch), ftLease("a", "L", "", ftEpoch)
	emptied := ftLease("a", "L", "", ftEpoch)
	emptied.Spec.HolderIdentity = ptr.To("")
	for _, test := range []struct {
		name      string
		got, want bool
	}{
		{"held by its holder", holderIs(held, "h1"), true},
		{"held by another", holderIs(held, "h2"), false},
		{"an absent holder is not the empty one", holderIs(released, ""), false},
		{"an empty holder is the empty one", holderIs(emptied, ""), true},
		{"an absent holder is empty", holderEmpty(released), true},
		{"an empty holder is empty", holderEmpty(emptied), true},
		{"a holder is not empty", holderEmpty(held), false},
		{"held at its epoch", heldAs(held, "h1", ftEpoch), true},
		{"held at another epoch", heldAs(held, "h1", ftOtherEpoch), false},
		{"the identity pinned", leaseIs(held, leaseIdentity{name: "x", uid: "L", holder: "h1", epoch: ftEpoch}), true},
		{"another UID", leaseIs(held, leaseIdentity{uid: "L2", holder: "h1", epoch: ftEpoch}), false},
		{"another holder", leaseIs(held, leaseIdentity{uid: "L", holder: "h2", epoch: ftEpoch}), false},
		{"another epoch", leaseIs(held, leaseIdentity{uid: "L", holder: "h1", epoch: ftOtherEpoch}), false},
	} {
		if test.got != test.want {
			t.Errorf("%s: got %t, want %t", test.name, test.got, test.want)
		}
	}
	identity := leaseIdentity{name: "lease-a", uid: "L", holder: "h1", epoch: ftEpoch}
	if got, want := identity.String(), `Lease lease-a UID L held by "h1" at `+ftEpoch; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWatchedUIDs(t *testing.T) {
	t.Parallel()
	job := func(uid string) *batchv1.Job { return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid)}} }
	valid := []watchEvent[*batchv1.Job]{
		ftEvent(watch.Added, job("b")), ftEvent(watch.Added, job("a")), ftEvent(watch.Added, job("b")),
		ftEvent(watch.Modified, job("")), ftEvent(watch.Deleted, job("c\n")), ftJobBookmark(),
		ftEvent(watch.Added, job(strings.Repeat("u", 128))), ftEvent(watch.Added, job(strings.Repeat("é", 128))),
	}
	got, err := watchedUIDs(valid)
	if want := []string{"a", "b", strings.Repeat("u", 128), strings.Repeat("é", 128)}; err != nil || !slices.Equal(got, want) {
		t.Errorf("got %v %v, want %v", got, err, want)
	}
	for name, uid := range map[string]string{
		"an empty UID": "", "a line feed": "a\nb", "a carriage return": "a\rb", "a tab": "a\tb",
		"a UID of 129 characters": strings.Repeat("u", 129),
	} {
		if _, err := watchedUIDs([]watchEvent[*batchv1.Job]{ftEvent(watch.Added, job(uid))}); err == nil {
			t.Errorf("%s was accepted as a ledger line", name)
		}
	}
	pods := []watchEvent[*corev1.Pod]{ftEvent(watch.Added, ftPod("p1", ftSchema, "apply", corev1.PodPending))}
	if got, err := watchedUIDs(pods); err != nil || !slices.Equal(got, []string{"p1"}) {
		t.Errorf("Pods: got %v %v", got, err)
	}
}

func TestAddedJobsKeepsTheFirstAdditionOfEach(t *testing.T) {
	t.Parallel()
	first, again := ftJob("b", ftSchema, "plan"), ftJob("b", ftSchema, "plan")
	again.Name = "renamed"
	got := addedJobs([]watchEvent[*batchv1.Job]{
		ftEvent(watch.Added, first), ftEvent(watch.Modified, ftJob("m", ftSchema, "plan")),
		ftEvent(watch.Added, ftJob("a", ftSchema, "observe")), ftEvent(watch.Added, again), ftJobBookmark(),
	})
	names := make([]string, 0, len(got))
	for _, job := range got {
		names = append(names, job.Name)
	}
	if want := []string{"job-b", "job-a"}; !slices.Equal(names, want) {
		t.Errorf("got %v, want %v", names, want)
	}
}

func TestTolerationMatchesBarrier(t *testing.T) {
	t.Parallel()
	key := readWorkloadBarrierKey
	for _, test := range []struct {
		name       string
		toleration corev1.Toleration
		want       bool
	}{
		{"exists for every key", corev1.Toleration{Operator: corev1.TolerationOpExists}, true},
		{"exists for the barrier key", corev1.Toleration{Operator: corev1.TolerationOpExists, Key: key}, true},
		{"exists for another key", corev1.Toleration{Operator: corev1.TolerationOpExists, Key: "other"}, false},
		{"exists for another effect", corev1.Toleration{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}, false},
		{"equal to the barrier", corev1.Toleration{Operator: corev1.TolerationOpEqual, Key: key, Value: "held"}, true},
		{"the default operator is Equal", corev1.Toleration{Key: key, Value: "held"}, true},
		{"the NoSchedule effect named", corev1.Toleration{Key: key, Value: "held", Effect: corev1.TaintEffectNoSchedule}, true},
		{"a PreferNoSchedule effect", corev1.Toleration{Key: key, Value: "held", Effect: corev1.TaintEffectPreferNoSchedule}, false},
		{"another value", corev1.Toleration{Key: key, Value: "free"}, false},
		{"no value", corev1.Toleration{Key: key}, false},
		{"another key", corev1.Toleration{Key: "other", Value: "held"}, false},
	} {
		if got := tolerationMatchesBarrier(test.toleration); got != test.want {
			t.Errorf("%s: got %t, want %t", test.name, got, test.want)
		}
	}
}

func TestJobHeldByBarrier(t *testing.T) {
	t.Parallel()
	fresh := func() *batchv1.Job {
		job := ftJob("j1", ftSchema, "observe")
		job.Spec.Template.Spec.Tolerations = []corev1.Toleration{{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists}}
		return job
	}
	accepts := jobHeldByBarrier
	ftRefusesEach(t, fresh, accepts, []ftMutation[*batchv1.Job]{
		{"completed", func(job *batchv1.Job) {
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		}},
		{"failed", func(job *batchv1.Job) {
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
		}},
		{"tolerates every taint", func(job *batchv1.Job) {
			job.Spec.Template.Spec.Tolerations = append(job.Spec.Template.Spec.Tolerations,
				corev1.Toleration{Operator: corev1.TolerationOpExists})
		}},
	})
	ftAcceptsEach(t, fresh, accepts, []ftMutation[*batchv1.Job]{
		{"a completion that is False", func(job *batchv1.Job) {
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionFalse}}
		}},
		{"suspended", func(job *batchv1.Job) {
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuspended, Status: corev1.ConditionTrue}}
		}},
	})
}

func TestPodsUnscheduledAndNeverStarted(t *testing.T) {
	t.Parallel()
	waiting := corev1.ContainerStatus{Name: "ptah", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}}
	running := corev1.ContainerStatus{Name: "x", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	terminated := corev1.ContainerStatus{Name: "x", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}
	held := func(change func(*corev1.Pod)) []corev1.Pod {
		pod := corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{waiting}}}
		change(&pod)
		return []corev1.Pod{{}, pod}
	}
	for _, test := range []struct {
		name                    string
		pods                    []corev1.Pod
		unscheduled, notStarted bool
	}{
		{"unscheduled and waiting", held(func(*corev1.Pod) {}), true, true},
		{"no Pod", nil, true, true},
		{"on a node", held(func(p *corev1.Pod) { p.Spec.NodeName = "kind-worker" }), false, false},
		{"a container running", held(func(p *corev1.Pod) {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, running)
		}), false, false},
		{"a container terminated", held(func(p *corev1.Pod) {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, terminated)
		}), false, false},
		{"an init container running", held(func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{running}
		}), true, false},
		{"an ephemeral container terminated", held(func(p *corev1.Pod) {
			p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{terminated}
		}), true, false},
	} {
		if got := podsUnscheduled(test.pods); got != test.unscheduled {
			t.Errorf("%s: unscheduled %t, want %t", test.name, got, test.unscheduled)
		}
		if got := podsNeverStarted(test.pods); got != test.notStarted {
			t.Errorf("%s: never started %t, want %t", test.name, got, test.notStarted)
		}
	}
}

func TestNodesCarryAndFreeTheBarrier(t *testing.T) {
	t.Parallel()
	exact := corev1.Taint{Key: readWorkloadBarrierKey, Value: "held", Effect: corev1.TaintEffectNoSchedule}
	other := corev1.Taint{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}
	node := func(taints ...corev1.Taint) corev1.Node { return corev1.Node{Spec: corev1.NodeSpec{Taints: taints}} }
	for _, test := range []struct {
		name          string
		nodes         []corev1.Node
		carry, isFree bool
	}{
		{"every node carries the exact taint", []corev1.Node{node(exact, other), node(exact)}, true, false},
		{"no node", nil, false, true},
		{"one node without it", []corev1.Node{node(exact), node(other)}, false, false},
		{"the exact taint twice", []corev1.Node{node(exact, exact)}, false, false},
		{"another value only", []corev1.Node{node(corev1.Taint{Key: exact.Key, Value: "free", Effect: exact.Effect})}, false, false},
		{"another effect only", []corev1.Node{node(corev1.Taint{Key: exact.Key, Value: "held", Effect: corev1.TaintEffectNoExecute})}, false, false},
		{"the exact taint beside the key with another effect", []corev1.Node{
			node(exact, corev1.Taint{Key: exact.Key, Value: "held", Effect: corev1.TaintEffectNoExecute}),
		}, true, false},
		{"no barrier anywhere", []corev1.Node{node(other), node()}, false, true},
	} {
		if got := nodesCarryBarrier(test.nodes); got != test.carry {
			t.Errorf("%s: carry %t, want %t", test.name, got, test.carry)
		}
		if got := nodesFreeOfBarrier(test.nodes); got != test.isFree {
			t.Errorf("%s: free %t, want %t", test.name, got, test.isFree)
		}
	}
}

func TestManagedOperation(t *testing.T) {
	t.Parallel()
	fresh := func() *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			labelManagedBy: managedByOperator, labelComponent: schemaOperationComponent,
		}}}
	}
	ftRefusesEach(t, fresh, func(job *batchv1.Job) bool { return managedOperation(job) }, []ftMutation[*batchv1.Job]{
		{"managed by another", func(job *batchv1.Job) { job.Labels[labelManagedBy] = "helm" }},
		{"another component", func(job *batchv1.Job) { job.Labels[labelComponent] = "e2e-fault-schema-publisher" }},
		{"no labels", func(job *batchv1.Job) { job.Labels = nil }},
	})
}

func ftStamped() map[string]string {
	return map[string]string{
		annotationControllerImage: ftController.image, annotationControllerRev: ftController.revision,
		annotationControllerState: ftController.stateVersion, annotationBindingID: ftEpoch,
	}
}

func TestExecutionIdentityOnPod(t *testing.T) {
	t.Parallel()
	fresh := func() *corev1.Pod { return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: ftStamped()}} }
	accepts := func(pod *corev1.Pod) bool { return executionIdentityOnPod(pod, ftController) }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*corev1.Pod]{
		{"another image", func(pod *corev1.Pod) { pod.Annotations[annotationControllerImage] = "other" }},
		{"another revision", func(pod *corev1.Pod) { pod.Annotations[annotationControllerRev] = "other" }},
		{"another state version", func(pod *corev1.Pod) { pod.Annotations[annotationControllerState] = "4" }},
		{"no state version", func(pod *corev1.Pod) { delete(pod.Annotations, annotationControllerState) }},
		{"an invalid binding", func(pod *corev1.Pod) { pod.Annotations[annotationBindingID] = "v2-" + ftEpoch[3:] }},
		{"no binding", func(pod *corev1.Pod) { delete(pod.Annotations, annotationBindingID) }},
	})
}

func TestExecutionIdentityOnJob(t *testing.T) {
	t.Parallel()
	fresh := func() *batchv1.Job {
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Annotations: ftStamped()}}
		job.Spec.Template.Annotations = ftStamped()
		return job
	}
	accepts := func(job *batchv1.Job) bool { return executionIdentityOnJob(job, ftController) }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*batchv1.Job]{
		{"no binding on the Job", func(job *batchv1.Job) { delete(job.Annotations, annotationBindingID) }},
		{"an invalid binding on the Job and its template", func(job *batchv1.Job) {
			job.Annotations[annotationBindingID], job.Spec.Template.Annotations[annotationBindingID] = "bad", "bad"
		}},
		{"another image on the Job", func(job *batchv1.Job) { job.Annotations[annotationControllerImage] = "other" }},
		{"another binding on the template", func(job *batchv1.Job) {
			job.Spec.Template.Annotations[annotationBindingID] = ftOtherEpoch
		}},
		{"no binding on the template", func(job *batchv1.Job) { delete(job.Spec.Template.Annotations, annotationBindingID) }},
		{"another image on the template", func(job *batchv1.Job) { job.Spec.Template.Annotations[annotationControllerImage] = "other" }},
		{"another revision on the template", func(job *batchv1.Job) { job.Spec.Template.Annotations[annotationControllerRev] = "other" }},
		{"another state version on the template", func(job *batchv1.Job) {
			job.Spec.Template.Annotations[annotationControllerState] = "4"
		}},
	})
}

type ftStoredApproval struct {
	stored  *unstructured.Unstructured
	planUID string
}

func ftApprovalCondition(kind, status, reason string) map[string]any {
	return map[string]any{"type": kind, "status": status, "reason": reason, "message": "m",
		"lastTransitionTime": "2026-09-28T10:00:00Z"}
}

func TestFaultApprovalConsumed(t *testing.T) {
	t.Parallel()
	fresh := func() *ftStoredApproval {
		return &ftStoredApproval{stored: &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchemaApproval",
			"metadata": map[string]any{"name": "approval", "namespace": "ns"},
			"spec": map[string]any{
				"schemaRef":          map[string]any{"name": ftSchema, "uid": "schema-uid"},
				"planRef":            map[string]any{"name": "plan", "uid": "plan-uid"},
				"planFingerprint":    "fp",
				"approver":           map[string]any{"username": "kubernetes-admin"},
				"approvedAt":         "2026-09-28T10:00:00Z",
				"mutationRequestUID": "request-uid",
			},
			"status": map[string]any{"conditions": []any{
				ftApprovalCondition("Consumed", "True", "DispatchCommitted"),
				ftApprovalCondition("Accepted", "False", "PlanNoLongerCurrent"),
				ftApprovalCondition("Stale", "True", "PlanNoLongerCurrent"),
			}},
		}}, planUID: "plan-uid"}
	}
	accepts := func(a *ftStoredApproval) bool { return faultApprovalConsumed(a.stored, a.planUID) == nil }
	spec := func(a *ftStoredApproval) map[string]any { return a.stored.Object["spec"].(map[string]any) }
	conditions := func(a *ftStoredApproval) []any {
		return a.stored.Object["status"].(map[string]any)["conditions"].([]any)
	}
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ftStoredApproval]{
		{"a field a person may not write", func(a *ftStoredApproval) { spec(a)["coordinationKey"] = "e2e/fault" }},
		{"no approval time stamped", func(a *ftStoredApproval) { delete(spec(a), "approvedAt") }},
		{"no spec", func(a *ftStoredApproval) { delete(a.stored.Object, "spec") }},
		{"another plan", func(a *ftStoredApproval) { a.planUID = "other-plan" }},
		{"an empty plan UID names nothing", func(a *ftStoredApproval) {
			a.planUID = ""
			spec(a)["planRef"].(map[string]any)["uid"] = ""
		}},
		{"not consumed", func(a *ftStoredApproval) { conditions(a)[0] = ftApprovalCondition("Consumed", "False", "Pending") }},
		{"consumed for another reason", func(a *ftStoredApproval) {
			conditions(a)[0] = ftApprovalCondition("Consumed", "True", "Other")
		}},
		{"still accepted", func(a *ftStoredApproval) {
			conditions(a)[1] = ftApprovalCondition("Accepted", "True", "PlanNoLongerCurrent")
		}},
		{"not stale", func(a *ftStoredApproval) {
			conditions(a)[2] = ftApprovalCondition("Stale", "False", "PlanNoLongerCurrent")
		}},
		{"no stale condition", func(a *ftStoredApproval) {
			a.stored.Object["status"].(map[string]any)["conditions"] = conditions(a)[:2]
		}},
		{"a spec that does not decode", func(a *ftStoredApproval) { spec(a)["planFingerprint"] = int64(5) }},
	})
}

func TestApprovalRetired(t *testing.T) {
	t.Parallel()
	fresh := func() *ptahv1alpha1.PtahSchemaApproval {
		return &ptahv1alpha1.PtahSchemaApproval{Status: ptahv1alpha1.PtahSchemaApprovalStatus{Conditions: []metav1.Condition{
			{Type: "Consumed", Status: metav1.ConditionTrue, Reason: "DispatchCommitted"},
			{Type: "Stale", Status: metav1.ConditionTrue, Reason: "PlanNoLongerCurrent"},
		}}}
	}
	ftRefusesEach(t, fresh, approvalRetired, []ftMutation[*ptahv1alpha1.PtahSchemaApproval]{
		{"not consumed", func(a *ptahv1alpha1.PtahSchemaApproval) { a.Status.Conditions[0].Status = metav1.ConditionFalse }},
		{"consumed for another reason", func(a *ptahv1alpha1.PtahSchemaApproval) { a.Status.Conditions[0].Reason = "Other" }},
		{"not stale", func(a *ptahv1alpha1.PtahSchemaApproval) { a.Status.Conditions[1].Status = metav1.ConditionFalse }},
		{"stale for another reason", func(a *ptahv1alpha1.PtahSchemaApproval) { a.Status.Conditions[1].Reason = "SupersededApproval" }},
	})
}

func TestPrincipalCredentialsMatchCksum(t *testing.T) {
	t.Parallel()
	// The suffixes are what `printf '%s-principal' <namespace> | cksum`
	// printed for these namespaces.
	for namespace, suffix := range map[string]string{"e2e": "1583211877", "ptah-e2e-test": "1839454200"} {
		role, password := principalCredentials(namespace)
		if role != "e2e_credential_principal_"+suffix || password != "e2ePrincipal"+suffix+"Q7" {
			t.Errorf("%s: got %s %s, want suffix %s", namespace, role, password, suffix)
		}
	}
}

func TestPrincipalSchemaHCLIsTheShellsPrintf(t *testing.T) {
	t.Parallel()
	// The shell wrote its arguments with %s, so they reach the file as they
	// are: a quote or a backslash is not escaped.
	for _, test := range []struct{ role, password, want string }{
		{"role", "pw", "schema \"public\" {}\n\nrole \"role\" {\n  login = true\n  password = \"pw\"\n}\n"},
		{`a\b`, `p"w`, "schema \"public\" {}\n\nrole \"a\\b\" {\n  login = true\n  password = \"p\"w\"\n}\n"},
	} {
		if got := principalSchemaHCL(test.role, test.password); got != test.want {
			t.Errorf("got %q, want %q", got, test.want)
		}
	}
}

func TestPublishedDigests(t *testing.T) {
	t.Parallel()
	first, second := "sha256:"+strings.Repeat("0a", 32), "sha256:"+strings.Repeat("1b", 32)
	log := strings.Join([]string{
		"Pushing schema", "Digest: " + first, "Digest: sha256:abc", "  Digest: " + second,
		"Digest: " + strings.ToUpper(second), "Digest: " + second, "",
	}, "\n")
	if got := publishedDigests([]byte(log)); !slices.Equal(got, []string{first, second}) {
		t.Errorf("got %v, want [%s %s]", got, first, second)
	}
	if got := publishedDigests(nil); got != nil {
		t.Errorf("an empty log reported %v", got)
	}
}

// ftBoundSchema is a schema whose current plan, plan-a, was published by this
// manager under a valid execution binding.
func ftBoundSchema() *ptahv1alpha1.PtahSchema {
	schema := ftSchemaNamed(ftSchema)
	schema.Status.ExecutionBinding = &ptahv1alpha1.ExecutionBindingStatus{Epoch: ftEpoch, ControllerStateVersion: ftStateVersion}
	schema.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{
		Name: "plan-a", UID: "plan-uid", Fingerprint: "fp", ExecutionBindingID: ftEpoch,
		ControllerImage: ftController.image, ControllerRevision: ftController.revision, ControllerStateVersion: ftStateVersion,
	}
	return schema
}

// ftPlanObject is plan-a as stored: ready, non-destructive, current contract,
// this manager's.
func ftPlanObject() *ptahv1alpha1.PtahSchemaPlan {
	return &ptahv1alpha1.PtahSchemaPlan{
		ObjectMeta: metav1.ObjectMeta{Name: "plan-a", UID: "plan-uid"},
		Spec: ptahv1alpha1.PtahSchemaPlanSpec{
			ContractVersion: fingerprint.CurrentPlanContractVersion, Fingerprint: "fp", ExecutionBindingID: ftEpoch,
			ControllerImage: ftController.image, ControllerRevision: ftController.revision, ControllerStateVersion: ftStateVersion,
			RunnerProtocolVersion: 7, ArtifactDigest: ftDigest, CoordinationDigest: ftDigest, StatementCount: 2,
		},
		Status: ptahv1alpha1.PtahSchemaPlanStatus{Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}},
	}
}

func TestPlanAwaitingApproval(t *testing.T) {
	t.Parallel()
	fresh := func() *ptahv1alpha1.PtahSchema {
		schema := ftBoundSchema()
		schema.Status.Phase = ptahv1alpha1.PhaseAwaitingApproval
		return schema
	}
	ftRefusesEach(t, fresh, planAwaitingApproval, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"another phase", func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseApplying }},
		{"an operation running", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationObserve}
		}},
		{"no plan", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = nil }},
		{"a plan with no name", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.Name = "" }},
	})
}

func TestExactControllerPlan(t *testing.T) {
	t.Parallel()
	fresh := ftBoundSchema
	accepts := func(s *ptahv1alpha1.PtahSchema) bool {
		return exactControllerPlan(s.Status.Plan, s.Status.ExecutionBinding, ftController, ftStateVersion)
	}
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"no binding", func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding = nil }},
		{"an invalid binding epoch", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ExecutionBinding.Epoch, s.Status.Plan.ExecutionBindingID = "v1-bad", "v1-bad"
		}},
		{"another controller state", func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.ControllerStateVersion = 4 }},
		{"no plan", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = nil }},
		{"a plan under another binding", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.ExecutionBindingID = ftOtherEpoch }},
		{"another image", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.ControllerImage = "other" }},
		{"another revision", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.ControllerRevision = "other" }},
		{"a plan of another state", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.ControllerStateVersion = 4 }},
	})
	if exactControllerBinding(&ptahv1alpha1.ExecutionBindingStatus{Epoch: ftEpoch, ControllerStateVersion: 4}, ftStateVersion) {
		t.Error("a binding of another controller state was accepted")
	}
}

func TestReadyPlanFromController(t *testing.T) {
	t.Parallel()
	accepts := func(p *ptahv1alpha1.PtahSchemaPlan) bool {
		return readyPlanFromController(p, ftController, ftStateVersion) == nil
	}
	ftRefusesEach(t, ftPlanObject, accepts, []ftMutation[*ptahv1alpha1.PtahSchemaPlan]{
		{"another contract", func(p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.ContractVersion = fingerprint.CurrentPlanContractVersion + 1
		}},
		{"an invalid binding", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ExecutionBindingID = "bad" }},
		{"another image", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ControllerImage = "other" }},
		{"another revision", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ControllerRevision = "other" }},
		{"another state", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ControllerStateVersion = 4 }},
		{"destructive", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Destructive = true }},
		{"no statement", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.StatementCount = 0 }},
		{"not ready", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Status.Conditions[0].Status = metav1.ConditionFalse }},
		{"no condition", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Status.Conditions = nil }},
	})
}

type ftApprovalTarget struct {
	schema *ptahv1alpha1.PtahSchema
	plan   *ptahv1alpha1.PtahSchemaPlan
}

func TestApprovalBindsCurrentPlan(t *testing.T) {
	t.Parallel()
	fresh := func() *ftApprovalTarget { return &ftApprovalTarget{schema: ftBoundSchema(), plan: ftPlanObject()} }
	accepts := func(a *ftApprovalTarget) bool {
		return approvalBindsCurrentPlan(a.schema, a.plan, ftController, ftStateVersion)
	}
	current := func(a *ftApprovalTarget) *ptahv1alpha1.CurrentPlanStatus { return a.schema.Status.Plan }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ftApprovalTarget]{
		{"no binding", func(a *ftApprovalTarget) { a.schema.Status.ExecutionBinding = nil }},
		{"a binding the plan does not carry", func(a *ftApprovalTarget) { a.schema.Status.ExecutionBinding.Epoch = ftOtherEpoch }},
		{"another controller state", func(a *ftApprovalTarget) { a.schema.Status.ExecutionBinding.ControllerStateVersion = 4 }},
		{"no current plan", func(a *ftApprovalTarget) { a.schema.Status.Plan = nil }},
		{"another plan name", func(a *ftApprovalTarget) { current(a).Name = "plan-b" }},
		{"another plan UID", func(a *ftApprovalTarget) { current(a).UID = "other-uid" }},
		{"another fingerprint", func(a *ftApprovalTarget) { current(a).Fingerprint = "other" }},
		{"the current plan under another binding", func(a *ftApprovalTarget) { current(a).ExecutionBindingID = ftOtherEpoch }},
		{"another image", func(a *ftApprovalTarget) { current(a).ControllerImage = "other" }},
		{"another revision", func(a *ftApprovalTarget) { current(a).ControllerRevision = "other" }},
		{"the current plan of another state", func(a *ftApprovalTarget) { current(a).ControllerStateVersion = 4 }},
	})
}

func TestApprovablePlan(t *testing.T) {
	t.Parallel()
	accepts := func(p *ptahv1alpha1.PtahSchemaPlan) bool { return approvablePlan(p, 7, ftController, ftStateVersion) }
	ftRefusesEach(t, ftPlanObject, accepts, []ftMutation[*ptahv1alpha1.PtahSchemaPlan]{
		{"another contract", func(p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.ContractVersion = fingerprint.CurrentPlanContractVersion + 1
		}},
		{"another image", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ControllerImage = "other" }},
		{"another revision", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ControllerRevision = "other" }},
		{"another state", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ControllerStateVersion = 4 }},
		{"another runner protocol", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.RunnerProtocolVersion = 6 }},
		{"no artifact digest", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ArtifactDigest = "" }},
		{"a malformed coordination digest", func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.CoordinationDigest = "sha256:abc" }},
	})
}

func ftActiveSchema(kind ptahv1alpha1.OperationType) *ptahv1alpha1.PtahSchema {
	schema := ftSchemaNamed(ftSchema)
	schema.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{
		Type: kind, ID: "op-a", JobName: "job-a", JobUID: "j1", DispatchStarted: true,
	}
	return schema
}

func TestApplyDispatched(t *testing.T) {
	t.Parallel()
	fresh := func() *ptahv1alpha1.PtahSchema {
		schema := ftActiveSchema(ptahv1alpha1.OperationApply)
		schema.Status.Phase = ptahv1alpha1.PhaseApplying
		return schema
	}
	ftRefusesEach(t, fresh, applyDispatched, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"another phase", func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseAwaitingApproval }},
		{"nothing active", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil }},
		{"a Plan", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.Type = ptahv1alpha1.OperationPlan }},
		{"not dispatched", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.DispatchStarted = false }},
		{"no Job bound", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobUID = "" }},
	})
}

func TestSinglePodApplyJob(t *testing.T) {
	t.Parallel()
	fresh := func() *batchv1.Job {
		job := ftJob("j1", ftSchema, "apply")
		job.Annotations[annotationOperationID] = "op-a"
		job.Spec.PodReplacementPolicy, job.Spec.BackoffLimit = ptr.To(batchv1.Failed), ptr.To(int32(0))
		return job
	}
	accepts := func(job *batchv1.Job) bool { return singlePodApplyJob(job, "j1", ftSchema, "op-a") }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*batchv1.Job]{
		{"another Job", func(job *batchv1.Job) { job.UID = "j2" }},
		{"another schema", func(job *batchv1.Job) { job.Labels[labelSchema] = "other" }},
		{"not an Apply", func(job *batchv1.Job) { job.Labels[labelOperation] = "plan" }},
		{"another operation", func(job *batchv1.Job) { job.Annotations[annotationOperationID] = "op-b" }},
		{"no replacement policy", func(job *batchv1.Job) { job.Spec.PodReplacementPolicy = nil }},
		{"replaced while terminating", func(job *batchv1.Job) {
			job.Spec.PodReplacementPolicy = ptr.To(batchv1.TerminatingOrFailed)
		}},
		{"no backoff limit", func(job *batchv1.Job) { job.Spec.BackoffLimit = nil }},
		{"a retry", func(job *batchv1.Job) { job.Spec.BackoffLimit = ptr.To(int32(1)) }},
	})
}

func TestRunningApplyPod(t *testing.T) {
	t.Parallel()
	running := func(name string) corev1.Pod {
		pod := ftApplyPod(corev1.PodRunning)
		pod.Name = name
		return *pod
	}
	deleting := running("deleting")
	deleting.DeletionTimestamp = ptr.To(metav1.NewTime(ftStart))
	pending := *ftApplyPod(corev1.PodPending)
	waiting := running("waiting")
	waiting.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
	helper := running("helper")
	helper.Status.ContainerStatuses[0].Name = "fetch"
	noStatuses := running("no-statuses")
	noStatuses.Status.ContainerStatuses = nil
	for _, test := range []struct {
		name  string
		pods  []corev1.Pod
		want  string
		fails bool
	}{
		{"no Pod yet", nil, "", false},
		{"one running Pod", []corev1.Pod{running("a")}, "a", false},
		{"two live Pods", []corev1.Pod{running("a"), pending}, "", true},
		{"only a deleting Pod", []corev1.Pod{deleting}, "", false},
		{"the first item is read, deleting or not, as the shell read it", []corev1.Pod{deleting, running("b")}, "deleting", false},
		{"a pending Pod", []corev1.Pod{pending}, "", false},
		{"ptah not running yet", []corev1.Pod{waiting}, "", false},
		{"another container running", []corev1.Pod{helper}, "", false},
		{"no container status", []corev1.Pod{noStatuses}, "", false},
	} {
		pod, err := runningApplyPod(test.pods)
		name := ""
		if pod != nil {
			name = pod.Name
		}
		if name != test.want || (err != nil) != test.fails {
			t.Errorf("%s: got %q %v, want %q failing %t", test.name, name, err, test.want, test.fails)
		}
	}
}

func TestExactResultJob(t *testing.T) {
	t.Parallel()
	fresh := func() *batchv1.Job {
		job := ftComplete(ftJob("j1", ftSchema, "observe"))
		job.Annotations = ftStamped()
		job.Annotations[annotationOperationID] = "op-o"
		job.Spec.Template.Annotations = ftStamped()
		job.Spec.PodReplacementPolicy, job.Spec.BackoffLimit = ptr.To(batchv1.Failed), ptr.To(int32(0))
		return job
	}
	accepts := func(job *batchv1.Job) bool { return exactResultJob(job, "j1", "observe", ftController) }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*batchv1.Job]{
		{"another Job", func(job *batchv1.Job) { job.UID = "j2" }},
		{"another operation", func(job *batchv1.Job) { job.Labels[labelOperation] = "plan" }},
		{"no operation ID", func(job *batchv1.Job) { delete(job.Annotations, annotationOperationID) }},
		{"another manager on the Job", func(job *batchv1.Job) { job.Annotations[annotationControllerRev] = "other" }},
		{"another manager on its template", func(job *batchv1.Job) { job.Spec.Template.Annotations[annotationControllerImage] = "other" }},
		{"no replacement policy", func(job *batchv1.Job) { job.Spec.PodReplacementPolicy = nil }},
		{"a retry", func(job *batchv1.Job) { job.Spec.BackoffLimit = ptr.To(int32(2)) }},
		{"not complete", func(job *batchv1.Job) { job.Status.Conditions = nil }},
		{"failed", func(job *batchv1.Job) {
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
		}},
	})
}

func TestResultPod(t *testing.T) {
	t.Parallel()
	fresh := func() *[]corev1.Pod {
		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "result", UID: "p1", Annotations: ftStamped(),
				OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "job-j1", UID: "j1", Controller: ptr.To(true)}}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "ptah", State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
			}}}},
		}
		pod.Annotations[annotationOperationID] = "op-o"
		stranger := *pod.DeepCopy()
		stranger.Name, stranger.UID, stranger.OwnerReferences[0].UID = "stranger", "p9", "j9"
		return &[]corev1.Pod{stranger, pod}
	}
	accepts := func(pods *[]corev1.Pod) bool {
		pod, err := resultPod(*pods, "j1", "op-o", ftController)
		return err == nil && pod.Name == "result"
	}
	target := func(pods *[]corev1.Pod) *corev1.Pod { return &(*pods)[1] }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*[]corev1.Pod]{
		{"a second Pod bound to the operation", func(pods *[]corev1.Pod) {
			second := *target(pods).DeepCopy()
			second.Name = "second"
			*pods = append(*pods, second)
		}},
		{"its owner is not the controller", func(pods *[]corev1.Pod) { target(pods).OwnerReferences[0].Controller = ptr.To(false) }},
		{"another operation", func(pods *[]corev1.Pod) { target(pods).Annotations[annotationOperationID] = "op-x" }},
		{"ptah exited non-zero", func(pods *[]corev1.Pod) {
			target(pods).Status.ContainerStatuses[0].State.Terminated.ExitCode = 1
		}},
		{"ptah still running", func(pods *[]corev1.Pod) {
			target(pods).Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		}},
		{"ptah reported twice", func(pods *[]corev1.Pod) {
			status := target(pods).Status.ContainerStatuses[0]
			target(pods).Status.ContainerStatuses = append(target(pods).Status.ContainerStatuses, status)
		}},
		{"an init container restarted", func(pods *[]corev1.Pod) {
			target(pods).Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "fetch", RestartCount: 1}}
		}},
		{"ptah restarted", func(pods *[]corev1.Pod) { target(pods).Status.ContainerStatuses[0].RestartCount = 1 }},
		{"another manager", func(pods *[]corev1.Pod) { target(pods).Annotations[annotationControllerState] = "4" }},
	})
	ftAcceptsEach(t, fresh, accepts, []ftMutation[*[]corev1.Pod]{
		{"an ephemeral container restart is not counted, as the shell did not", func(pods *[]corev1.Pod) {
			target(pods).Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debug", RestartCount: 2}}
		}},
	})
}

func TestResultBinding(t *testing.T) {
	t.Parallel()
	fresh := func() *runner.Result {
		return &runner.Result{ProtocolVersion: 7, Operation: runner.Operation("plan"), OperationID: "op-p"}
	}
	accepts := func(result *runner.Result) bool { return resultBinding(*result, 7, "plan", "op-p") }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*runner.Result]{
		{"another protocol", func(result *runner.Result) { result.ProtocolVersion = 6 }},
		{"another operation", func(result *runner.Result) { result.Operation = runner.Operation("observe") }},
		{"another operation ID", func(result *runner.Result) { result.OperationID = "op-x" }},
		{"truncated", func(result *runner.Result) { result.Truncation = &runner.TruncationMetadata{} }},
	})
}

func TestRecoveryJobComplete(t *testing.T) {
	t.Parallel()
	fresh := func() *batchv1.Job { return ftComplete(ftJob("j1", ftSchema, "observe")) }
	accepts := func(job *batchv1.Job) bool { return recoveryJobComplete(job, "j1") }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*batchv1.Job]{
		{"another Job", func(job *batchv1.Job) { job.UID = "j2" }},
		{"not complete", func(job *batchv1.Job) { job.Status.Conditions[0].Status = corev1.ConditionFalse }},
		{"no operation ID", func(job *batchv1.Job) { delete(job.Annotations, annotationOperationID) }},
		{"an empty operation ID", func(job *batchv1.Job) { job.Annotations[annotationOperationID] = "" }},
	})
}

func ftRecoveredSchema() *ptahv1alpha1.PtahSchema {
	schema := ftBoundSchema()
	schema.Status.Phase = ptahv1alpha1.PhaseAwaitingApproval
	return schema
}

func TestRecoveredAwaitingApproval(t *testing.T) {
	t.Parallel()
	mutations := []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"an observation pending", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{}
		}},
		{"an operation active", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationObserve}
		}},
		{"another phase", func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseVerifyingConvergence }},
		{"no plan", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = nil }},
		{"a plan with no name", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.Name = "" }},
		{"something applied", func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied = &ptahv1alpha1.AppliedStatus{} }},
		{"a lock release pending", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		}},
	}
	approved := ftMutation[*ptahv1alpha1.PtahSchema]{"the plan approved", func(s *ptahv1alpha1.PtahSchema) {
		s.Status.Plan.Approval = &ptahv1alpha1.ConsumedApprovalStatus{Name: "approval"}
	}}
	t.Run("recovered", func(t *testing.T) {
		t.Parallel()
		ftRefusesEach(t, ftRecoveredSchema, recoveredAwaitingApproval, mutations)
		ftAcceptsEach(t, ftRecoveredSchema, recoveredAwaitingApproval, []ftMutation[*ptahv1alpha1.PtahSchema]{approved})
	})
	t.Run("fresh", func(t *testing.T) {
		t.Parallel()
		ftRefusesEach(t, ftRecoveredSchema, freshPlanAwaitingApproval, append(slices.Clone(mutations), approved))
	})
}

func TestRealmConflict(t *testing.T) {
	t.Parallel()
	fresh := func() *ptahv1alpha1.PtahSchema {
		schema := ftSchemaNamed(ftSchema)
		schema.Status.Phase = ptahv1alpha1.PhaseBlocked
		schema.Status.Conditions = []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionFalse, Reason: "RealmConflict"},
			{Type: "ApprovalRequired", Status: metav1.ConditionFalse, Reason: "RealmConflict"},
		}
		return schema
	}
	held := []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"not blocked", func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseAwaitingApproval }},
		{"an operation active", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationResolve}
		}},
		{"ready", func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Status = metav1.ConditionTrue }},
		{"not ready for another reason", func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Reason = "RealmNotAuthorized" }},
	}
	approvalRequired := ftMutation[*ptahv1alpha1.PtahSchema]{"approval not refused for the conflict", func(s *ptahv1alpha1.PtahSchema) {
		s.Status.Conditions[1].Reason = "AwaitingApproval"
	}}
	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		ftRefusesEach(t, fresh, realmConflictRefused, append(slices.Clone(held), approvalRequired))
	})
	t.Run("held", func(t *testing.T) {
		t.Parallel()
		ftRefusesEach(t, fresh, realmConflictHeld, held)
		ftAcceptsEach(t, fresh, realmConflictHeld, []ftMutation[*ptahv1alpha1.PtahSchema]{approvalRequired})
	})
}

func TestUndispatchedApplyClaim(t *testing.T) {
	t.Parallel()
	fresh := func() *ptahv1alpha1.PtahSchema {
		schema := ftActiveSchema(ptahv1alpha1.OperationApply)
		schema.Status.ActiveOperation.JobUID, schema.Status.ActiveOperation.DispatchStarted = "", false
		return schema
	}
	ftRefusesEach(t, fresh, undispatchedApplyClaim, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"nothing active", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil }},
		{"a Plan", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.Type = ptahv1alpha1.OperationPlan }},
		{"a Job bound", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobUID = "j1" }},
		{"dispatch started", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.DispatchStarted = true }},
	})
}

func TestHeldReadOperation(t *testing.T) {
	t.Parallel()
	for _, kind := range []ptahv1alpha1.OperationType{
		ptahv1alpha1.OperationResolve, ptahv1alpha1.OperationVerify, ptahv1alpha1.OperationObserve, ptahv1alpha1.OperationPlan,
	} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			ftRefusesEach(t, func() *ptahv1alpha1.PtahSchema { return ftActiveSchema(kind) }, heldReadOperation,
				[]ftMutation[*ptahv1alpha1.PtahSchema]{
					{"an Apply", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.Type = ptahv1alpha1.OperationApply }},
					{"no Job name", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobName = "" }},
					{"no Job UID", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobUID = "" }},
					{"nothing active", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil }},
				})
		})
	}
}

func TestReadOperationRetried(t *testing.T) {
	t.Parallel()
	fresh := func() *ptahv1alpha1.PtahSchema { return ftActiveSchema(ptahv1alpha1.OperationResolve) }
	accepts := func(s *ptahv1alpha1.PtahSchema) bool {
		return readOperationRetried(s, ptahv1alpha1.OperationResolve, "removed-uid")
	}
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"nothing active", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil }},
		{"another operation", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.Type = ptahv1alpha1.OperationVerify }},
		{"no Job name", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobName = "" }},
		{"no Job UID yet", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobUID = "" }},
		{"the removed Job", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobUID = "removed-uid" }},
	})
}

func TestSuspendedReadDiscarded(t *testing.T) {
	t.Parallel()
	fresh := func() *ptahv1alpha1.PtahSchema {
		schema := ftSchemaNamed(ftSchema)
		schema.Spec.Suspend = true
		return schema
	}
	ftRefusesEach(t, fresh, suspendedReadDiscarded, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"not suspended", func(s *ptahv1alpha1.PtahSchema) { s.Spec.Suspend = false }},
		{"still active", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationResolve}
		}},
		{"an observation pending", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{}
		}},
	})
}

func TestInSyncFor(t *testing.T) {
	t.Parallel()
	fresh := func() *ptahv1alpha1.PtahSchema {
		schema := ftSchemaNamed(ftSchema)
		schema.Status.Phase = ptahv1alpha1.PhaseInSync
		schema.Status.Conditions = []metav1.Condition{{Type: "InSync", Status: metav1.ConditionTrue, Reason: "ScopedConverged"}}
		return schema
	}
	accepts := func(s *ptahv1alpha1.PtahSchema) bool { return inSyncFor(s, "ScopedConverged") }
	ftRefusesEach(t, fresh, accepts, []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"another phase", func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseVerifyingConvergence }},
		{"an observation pending", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{}
		}},
		{"an operation active", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationObserve}
		}},
		{"a lock release pending", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		}},
		{"in sync for the weaker reason", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[0].Reason = "ConvergedAfterUnknownOutcome"
		}},
		{"not in sync", func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Status = metav1.ConditionFalse }},
	})
}

func TestUncertainAndTimeoutRecoveryStaySafe(t *testing.T) {
	t.Parallel()
	shared := []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"an operation active", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationApply}
		}},
		{"an observation pending", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{}
		}},
		{"a lock release pending", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		}},
		{"no plan", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = nil }},
		{"a plan with no UID", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.UID = "" }},
		{"the plan approved again", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Plan.Approval = &ptahv1alpha1.ConsumedApprovalStatus{Name: "approval"}
		}},
	}
	timeoutOnly := []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"something applied", func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied = &ptahv1alpha1.AppliedStatus{} }},
		{"another phase", func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseInSync }},
	}
	t.Run("uncertain", func(t *testing.T) {
		t.Parallel()
		ftRefusesEach(t, ftRecoveredSchema, uncertainStillSafe, shared)
		ftAcceptsEach(t, ftRecoveredSchema, uncertainStillSafe, timeoutOnly)
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		ftRefusesEach(t, ftRecoveredSchema, timeoutRecoveryStillSafe, append(slices.Clone(shared), timeoutOnly...))
	})
}

func TestActiveIdentityKept(t *testing.T) {
	t.Parallel()
	mutations := []ftMutation[*ptahv1alpha1.PtahSchema]{
		{"nothing active", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil }},
		{"another operation", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.ID = "op-b" }},
		{"another Job name", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobName = "job-b" }},
		{"another Job UID", func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.JobUID = "j2" }},
	}
	notDispatched := ftMutation[*ptahv1alpha1.PtahSchema]{"not dispatched", func(s *ptahv1alpha1.PtahSchema) {
		s.Status.ActiveOperation.DispatchStarted = false
	}}
	fresh := func() *ptahv1alpha1.PtahSchema { return ftActiveSchema(ptahv1alpha1.OperationApply) }
	kept := func(s *ptahv1alpha1.PtahSchema) bool { return activeIdentityKept(s, "op-a", "job-a", "j1") }
	dispatched := func(s *ptahv1alpha1.PtahSchema) bool { return activeOperationDispatched(s, "op-a", "job-a", "j1") }
	t.Run("kept", func(t *testing.T) {
		t.Parallel()
		ftRefusesEach(t, fresh, kept, mutations)
		ftAcceptsEach(t, fresh, kept, []ftMutation[*ptahv1alpha1.PtahSchema]{notDispatched})
	})
	t.Run("dispatched", func(t *testing.T) {
		t.Parallel()
		ftRefusesEach(t, fresh, dispatched, append(slices.Clone(mutations), notDispatched))
	})
}

func TestEphemeralContainerRows(t *testing.T) {
	t.Parallel()
	fresh := func() *corev1.Pod { return ftApplyPod(corev1.PodRunning) }
	before := func(pod *corev1.Pod) bool { return activePodForEphemeral(pod, "p1", "apply-job", "j1", "op-apply") }
	after := func(pod *corev1.Pod) bool { return ephemeralContainerRefused(pod, "p1", "j1") }
	refused := ftMutation[*corev1.Pod]{"the refused container admitted", func(pod *corev1.Pod) {
		pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
			EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: refusedEphemeralContainer},
		}}
	}}
	t.Run("before", func(t *testing.T) {
		t.Parallel()
		ftRefusesEach(t, fresh, before, []ftMutation[*corev1.Pod]{
			{"another Pod", func(pod *corev1.Pod) { pod.UID = "p2" }},
			{"another operation", func(pod *corev1.Pod) { pod.Annotations[annotationOperationID] = "op-b" }},
			{"a second owner", func(pod *corev1.Pod) {
				pod.OwnerReferences = append(pod.OwnerReferences, metav1.OwnerReference{Kind: "Node"})
			}},
			{"another Job", func(pod *corev1.Pod) { pod.OwnerReferences[0].Name = "other-job" }},
			refused,
			{"another ephemeral container already", func(pod *corev1.Pod) {
				pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
					EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug"},
				}}
			}},
		})
	})
	t.Run("after", func(t *testing.T) {
		t.Parallel()
		ftRefusesEach(t, fresh, after, []ftMutation[*corev1.Pod]{
			{"another Pod", func(pod *corev1.Pod) { pod.UID = "p2" }},
			{"another Job", func(pod *corev1.Pod) { pod.OwnerReferences[0].UID = "j2" }},
			{"its owner is not the controller", func(pod *corev1.Pod) { pod.OwnerReferences[0].Controller = nil }},
			refused,
		})
		ftAcceptsEach(t, fresh, after, []ftMutation[*corev1.Pod]{
			{"a second owner beside the Job", func(pod *corev1.Pod) {
				pod.OwnerReferences = append(pod.OwnerReferences, metav1.OwnerReference{Kind: "Node"})
			}},
			{"another ephemeral container", func(pod *corev1.Pod) {
				pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
					EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug"},
				}}
			}},
		})
	})
}
