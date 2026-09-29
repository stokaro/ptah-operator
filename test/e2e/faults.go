package e2e

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The bounds the fault injection runs under. TestFaultBoundsHoldTogether holds
// them to the relationships each proof relies on.
const (
	// faultActiveDeadlineSeconds is the Apply deadline of every fault schema
	// but the one whose deadline the phase lets Kubernetes enforce: long enough
	// that no Apply a barrier holds can reach it.
	faultActiveDeadlineSeconds = 7200
	// faultBarrierSeconds is how long a database barrier holds its lock. It
	// outlives the Apply deadline, so a barrier never lets go of an Apply on
	// its own.
	faultBarrierSeconds = 7800
	// faultTimeoutDeadlineSeconds is the one Apply deadline the phase lets
	// Kubernetes enforce, on a Pod the phase keeps running until then.
	faultTimeoutDeadlineSeconds = 45
	// faultAuditCadence is how often a wait audits the namespace for
	// credentials while it polls.
	faultAuditCadence = 30 * time.Second
	// watchSegmentSeconds is how long one watch request lasts before the
	// recorder continues it from the last resourceVersion it read.
	watchSegmentSeconds = 30
	// heartbeatInterval is how often the heartbeat writes to one object of
	// every watched kind, so a watch segment that recorded nothing is a
	// stopped watch rather than a quiet namespace.
	heartbeatInterval = 5 * time.Second
	// heartbeatFailureLimit is how many rounds in a row may fail before the
	// heartbeat reports that it could not write.
	heartbeatFailureLimit = 3
)

// The objects and keys the fault injection reads and writes.
const (
	leaderLeaseName          = "ptah-operator.operator.ptah.run"
	principalSchemaSecret    = "e2e-credential-principal-schema"
	pgApplyLockKey           = 1237737229
	readWorkloadBarrierKey   = "operator.ptah.run/e2e-read-workload-barrier"
	watchHeartbeatLease      = "e2e-fault-watch-heartbeat"
	faultHeartbeatJob        = "e2e-fault-push-postgresql"
	heartbeatSchema          = "e2e-suspended-schema"
	heartbeatApproval        = "e2e-approval"
	annotationWatchHeartbeat = "operator.ptah.run/e2e-watch-heartbeat"
	annotationWatchBarrier   = "operator.ptah.run/e2e-watch-barrier"
	annotationLeaseEpoch     = "operator.ptah.run/lease-epoch"
	annotationBindingID      = "operator.ptah.run/execution-binding-id"
)

var (
	leaseEpochPattern = regexp.MustCompile(`^v1-[0-9a-f]{32}$`)
	safeDatabaseName  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	schemaFingerprint = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// publishedDigest is the line a schema publisher reports its artifact's
	// digest on.
	publishedDigest        = regexp.MustCompile(`^Digest: (sha256:[0-9a-f]{64})$`)
	coordinationLeaseLabel = client.MatchingLabels{
		labelManagedBy:                   managedByOperator,
		"operator.ptah.run/coordination": "database-target",
	}
)

// watchEvent is one event a fault watch recorded, in the order the API server
// sent it. A bookmark carries an object with a resourceVersion and nothing
// else, so it matches no name, UID or label a proof asks for.
type watchEvent[T client.Object] struct {
	Type   watch.EventType
	Object T
}

// uidIs is an object whose UID is the one given. An empty UID names nothing,
// as jq's comparison with a UID read from an absent field named nothing.
func uidIs(object metav1.Object, uid string) bool {
	return uid != "" && string(object.GetUID()) == uid
}

// labelIs and annotationIs are a key present with the value given: an absent
// key is null to jq and equal to no string, the empty one included.
func labelIs(object metav1.Object, key, value string) bool {
	found, ok := object.GetLabels()[key]
	return ok && found == value
}

func annotationIs(object metav1.Object, key, value string) bool {
	found, ok := object.GetAnnotations()[key]
	return ok && found == value
}

// operationOf is an object labeled for the schema, and for the operation when
// operation is not empty.
func operationOf(object metav1.Object, schema, operation string) bool {
	return labelIs(object, labelSchema, schema) && (operation == "" || labelIs(object, labelOperation, operation))
}

// addedUIDs is every distinct UID the watch saw added for the schema and the
// operation, or any operation when operation is empty, sorted.
func addedUIDs[T client.Object](events []watchEvent[T], schema, operation string) []string {
	var uids []string
	for _, event := range events {
		if event.Type == watch.Added && operationOf(event.Object, schema, operation) {
			uids = append(uids, string(event.Object.GetUID()))
		}
	}
	slices.Sort(uids)
	return slices.Compact(uids)
}

// newAddedUIDs is addedUIDs without the UIDs the checkpoint holds.
func newAddedUIDs[T client.Object](events []watchEvent[T], schema, operation string, before checkpoint) []string {
	var uids []string
	for _, uid := range addedUIDs(events, schema, operation) {
		if !before.holds(uid) {
			uids = append(uids, uid)
		}
	}
	return uids
}

// firstIndex is the index of the first event that matches, or -1.
func firstIndex[T client.Object](events []watchEvent[T], match func(watchEvent[T]) bool) int {
	return slices.IndexFunc(events, match)
}

// completedBeforeAdded is a Job that the watch first saw complete before it
// saw another added: the order a proof Job and the Job that has to wait for
// its result come in.
func completedBeforeAdded(jobs []watchEvent[*batchv1.Job], completedUID, addedUID string) bool {
	completed := firstIndex(jobs, func(event watchEvent[*batchv1.Job]) bool {
		return uidIs(event.Object, completedUID) && conditionTrue(event.Object.Status.Conditions, batchv1.JobComplete)
	})
	added := firstIndex(jobs, func(event watchEvent[*batchv1.Job]) bool {
		return event.Type == watch.Added && uidIs(event.Object, addedUID)
	})
	return completed >= 0 && added >= 0 && completed < added
}

// readChainOrdered is a schema whose first Resolve Job completed before its
// first Verify Job was added, and whose first Verify Job completed before its
// first Observe Job was added: the database is read only once the artifact
// was resolved and verified. Each of the three carries an operation ID.
func readChainOrdered(jobs []watchEvent[*batchv1.Job], schema string) bool {
	added := func(operation string) int {
		return firstIndex(jobs, func(event watchEvent[*batchv1.Job]) bool {
			return event.Type == watch.Added && operationOf(event.Object, schema, operation)
		})
	}
	resolve, verify, observe := added("resolve"), added("verify"), added("observe")
	if resolve < 0 || verify < 0 || observe < 0 {
		return false
	}
	completed := func(index int) int {
		uid := string(jobs[index].Object.UID)
		return firstIndex(jobs, func(event watchEvent[*batchv1.Job]) bool {
			return uidIs(event.Object, uid) && conditionTrue(event.Object.Status.Conditions, batchv1.JobComplete)
		})
	}
	resolveComplete, verifyComplete := completed(resolve), completed(verify)
	for _, index := range []int{resolve, verify, observe} {
		if jobs[index].Object.Annotations[annotationOperationID] == "" {
			return false
		}
	}
	return resolveComplete >= 0 && verifyComplete >= 0 &&
		resolve < resolveComplete && resolveComplete < verify && verify < verifyComplete && verifyComplete < observe
}

// operationJobsOverlap reports whether the watch ever saw two unfinished Jobs
// of one schema at once. A Job leaves the set when it is deleted or finishes.
func operationJobsOverlap(jobs []watchEvent[*batchv1.Job]) bool {
	return overlapping(jobs, func(job *batchv1.Job) bool { return jobTerminal(job) })
}

// operationPodsOverlap reports whether the watch ever saw two unfinished Pods
// of one schema at once. A Pod leaves the set when it is deleted or its phase
// is Succeeded or Failed.
func operationPodsOverlap(pods []watchEvent[*corev1.Pod]) bool {
	return overlapping(pods, func(pod *corev1.Pod) bool {
		return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
	})
}

func overlapping[T client.Object](events []watchEvent[T], finished func(T) bool) bool {
	active := map[string]map[string]bool{}
	overlap := false
	for _, event := range events {
		schema, uid := event.Object.GetLabels()[labelSchema], string(event.Object.GetUID())
		if schema == "" || uid == "" {
			continue
		}
		if event.Type == watch.Deleted || finished(event.Object) {
			delete(active[schema], uid)
			continue
		}
		if active[schema] == nil {
			active[schema] = map[string]bool{}
		}
		active[schema][uid] = true
		if len(active[schema]) > 1 {
			overlap = true
		}
	}
	return overlap
}

// outcomeUnknownRecorded is a schema watch that saw the schema record the
// Apply's outcome as unknown: the pending observation says so, and the
// Applying condition gives it as its reason.
func outcomeUnknownRecorded(schemas []watchEvent[*ptahv1alpha1.PtahSchema], name, applyOperationID string) bool {
	return slices.ContainsFunc(schemas, func(event watchEvent[*ptahv1alpha1.PtahSchema]) bool {
		status := event.Object.Status
		pending := status.PendingObservation
		return event.Type == watch.Modified && event.Object.Name == name && pending != nil &&
			pending.Outcome == ptahv1alpha1.PendingObservationOutcomeUnknown &&
			pending.ApplyOperationID == applyOperationID &&
			conditionIs(status.Conditions, ptahv1alpha1.ConditionApplying, metav1.ConditionFalse, "OutcomeUnknown")
	})
}

// activeApply is the schema's active operation when it is the Apply bound to
// the Job UID given.
func activeApply(schema *ptahv1alpha1.PtahSchema, jobUID string) *ptahv1alpha1.ActiveOperationStatus {
	active := schema.Status.ActiveOperation
	if active == nil || active.Type != ptahv1alpha1.OperationApply || jobUID == "" || string(active.JobUID) != jobUID {
		return nil
	}
	return active
}

// applyBindingInWatch reads the Apply operation a Job was dispatched for out
// of the schema watch: the distinct operation ID and lease epoch pairs whose
// binding is complete, and, when there is exactly one, that operation and its
// epoch. An epoch that is empty in any document naming the operation is
// refused, as the jq that read it refused a null.
func applyBindingInWatch(schemas []watchEvent[*ptahv1alpha1.PtahSchema], name, jobUID string) (count int, operationID, leaseEpoch string, err error) {
	type binding struct{ id, epoch string }
	bindings := map[binding]bool{}
	var ids []string
	for _, event := range schemas {
		if event.Object.Name != name {
			continue
		}
		active := activeApply(event.Object, jobUID)
		if active == nil {
			continue
		}
		ids = append(ids, active.ID)
		if active.ID != "" && leaseEpochPattern.MatchString(active.LeaseEpoch) &&
			active.CoordinationDigest == event.Object.Status.Target.CoordinationDigest {
			bindings[binding{active.ID, active.LeaseEpoch}] = true
		}
	}
	if len(bindings) != 1 {
		return len(bindings), "", "", nil
	}
	slices.Sort(ids)
	operationID = ids[0]
	var epochs []string
	for _, event := range schemas {
		if active := activeApply(event.Object, jobUID); event.Object.Name == name && active != nil && active.ID == operationID {
			epochs = append(epochs, active.LeaseEpoch)
		}
	}
	slices.Sort(epochs)
	if len(epochs) == 0 || epochs[0] == "" {
		return 1, operationID, "", fmt.Errorf("the Apply binding of Job UID %s names no lease epoch in every document", jobUID)
	}
	return 1, operationID, epochs[0], nil
}

// resultBoundInWatch is a result the schema watch saw the schema bind: the
// active operation of its type, ID and Job, and the Job added with the same
// schema, operation and operation ID.
func resultBoundInWatch(schemas []watchEvent[*ptahv1alpha1.PtahSchema], jobs []watchEvent[*batchv1.Job],
	name, operation, operationID, jobUID string,
) bool {
	bound := slices.ContainsFunc(schemas, func(event watchEvent[*ptahv1alpha1.PtahSchema]) bool {
		active := event.Object.Status.ActiveOperation
		return event.Object.Name == name && active != nil && strings.ToLower(string(active.Type)) == operation &&
			active.ID == operationID && jobUID != "" && string(active.JobUID) == jobUID
	})
	added := slices.ContainsFunc(jobs, func(event watchEvent[*batchv1.Job]) bool {
		return event.Type == watch.Added && uidIs(event.Object, jobUID) && operationOf(event.Object, name, operation) &&
			annotationIs(event.Object, annotationOperationID, operationID)
	})
	return bound && added
}

// holderIs is a Lease whose holder is the identity given. An absent holder is
// no identity at all.
func holderIs(lease *coordinationv1.Lease, holder string) bool {
	return lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == holder
}

// holderEmpty is a Lease nobody holds.
func holderEmpty(lease *coordinationv1.Lease) bool {
	return lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == ""
}

// heldAs is a Lease held by the holder at the epoch.
func heldAs(lease *coordinationv1.Lease, holder, epoch string) bool {
	return holderIs(lease, holder) && annotationIs(lease, annotationLeaseEpoch, epoch)
}

// leaseHeldWithoutRelease is a Lease watch in which, from the first event
// that shows the Lease held by the holder at the epoch, every event of that
// Lease still shows it so, and none deletes it.
func leaseHeldWithoutRelease(leases []watchEvent[*coordinationv1.Lease], uid, holder, epoch string) bool {
	acquired := firstIndex(leases, func(event watchEvent[*coordinationv1.Lease]) bool {
		return uidIs(event.Object, uid) && heldAs(event.Object, holder, epoch)
	})
	if acquired < 0 {
		return false
	}
	for _, event := range leases[acquired:] {
		if uidIs(event.Object, uid) && (event.Type == watch.Deleted || !heldAs(event.Object, holder, epoch)) {
			return false
		}
	}
	return true
}

// leaseReacquisition is what sameLeaseReacquired holds a contender to.
type leaseReacquisition struct {
	uid, firstHolder, firstEpoch                string
	schema, operationID, jobUID, contenderEpoch string
}

// sameLeaseReacquired is a target Lease the first holder released and the
// contender then acquired, on the same UID: the first event held by the first
// holder at its epoch, the first later event of that UID with no holder, and
// the first later event of that name with a holder, which modifies the same
// UID to another holder at the contender's epoch. Between the release and the
// reacquisition the Lease stays, unheld, on its UID. The schema watch saw the
// contender bound to its Apply at that epoch, and the Job watch saw its Apply
// Job added.
func sameLeaseReacquired(leases []watchEvent[*coordinationv1.Lease], schemas []watchEvent[*ptahv1alpha1.PtahSchema],
	jobs []watchEvent[*batchv1.Job], want leaseReacquisition,
) bool {
	first := firstIndex(leases, func(event watchEvent[*coordinationv1.Lease]) bool {
		return uidIs(event.Object, want.uid) && heldAs(event.Object, want.firstHolder, want.firstEpoch)
	})
	if first < 0 {
		return false
	}
	name := leases[first].Object.Name
	released := -1
	for index := first + 1; index < len(leases); index++ {
		if uidIs(leases[index].Object, want.uid) && holderEmpty(leases[index].Object) {
			released = index
			break
		}
	}
	if released < 0 {
		return false
	}
	reacquired := -1
	for index := released + 1; index < len(leases); index++ {
		if leases[index].Object.Name == name && !holderEmpty(leases[index].Object) {
			reacquired = index
			break
		}
	}
	if reacquired < 0 {
		return false
	}
	lease := leases[reacquired]
	if lease.Type != watch.Modified || !uidIs(lease.Object, want.uid) || holderIs(lease.Object, want.firstHolder) ||
		!annotationIs(lease.Object, annotationLeaseEpoch, want.contenderEpoch) ||
		annotationIs(lease.Object, annotationLeaseEpoch, want.firstEpoch) {
		return false
	}
	for _, between := range leases[released+1 : reacquired] {
		if between.Object.Name == name &&
			(between.Type == watch.Deleted || !uidIs(between.Object, want.uid) || !holderEmpty(between.Object)) {
			return false
		}
	}
	active := slices.ContainsFunc(schemas, func(event watchEvent[*ptahv1alpha1.PtahSchema]) bool {
		apply := activeApply(event.Object, want.jobUID)
		return event.Object.Name == want.schema && apply != nil && apply.ID == want.operationID &&
			apply.LeaseEpoch == want.contenderEpoch &&
			apply.CoordinationDigest == event.Object.Status.Target.CoordinationDigest
	})
	added := slices.ContainsFunc(jobs, func(event watchEvent[*batchv1.Job]) bool {
		return event.Type == watch.Added && uidIs(event.Object, want.jobUID) &&
			operationOf(event.Object, want.schema, "apply") &&
			annotationIs(event.Object, annotationOperationID, want.operationID)
	})
	return active && added
}

// instantOf is a time as the API server writes it: seconds, UTC, RFC 3339.
func instantOf(instant metav1.Time) string {
	if instant.IsZero() {
		return ""
	}
	return instant.UTC().Format(time.RFC3339)
}

// exactJobOwner is a Pod owned by exactly one reference: the Job named, as
// its controller.
func exactJobOwner(references []metav1.OwnerReference, jobName, jobUID string) bool {
	return len(references) == 1 && references[0].APIVersion == "batch/v1" && references[0].Kind == "Job" &&
		references[0].Name == jobName && jobUID != "" && string(references[0].UID) == jobUID &&
		isController(references[0])
}

// ptahRunningSince is a Pod whose ptah container runs since the instant given.
// An empty instant names none, as a start time that was never written is null
// to jq and equal to no string.
func ptahRunningSince(pod *corev1.Pod, startedAt string) bool {
	return startedAt != "" && slices.ContainsFunc(pod.Status.ContainerStatuses, func(status corev1.ContainerStatus) bool {
		return status.Name == "ptah" && status.State.Running != nil && instantOf(status.State.Running.StartedAt) == startedAt
	})
}

// deadlinePod is the Apply Pod whose Job Kubernetes ends at its deadline.
type deadlinePod struct {
	name, uid, jobName, jobUID, operationID, startedAt string
}

// runningThenDeleted reads the Pod's own events out of the watch: the first
// that shows it running on a node since the instant recorded, and the last
// that deletes it by name. Every event of the Pod has to keep its name, its
// operation, its one Job owner and a record of no restart.
func (p deadlinePod) runningThenDeleted(pods []watchEvent[*corev1.Pod]) (deleted *corev1.Pod, ok bool) {
	running, last := -1, -1
	for index, event := range pods {
		pod := event.Object
		if !uidIs(pod, p.uid) {
			continue
		}
		if pod.Name != p.name || !annotationIs(pod, annotationOperationID, p.operationID) ||
			!exactJobOwner(pod.OwnerReferences, p.jobName, p.jobUID) || !noRestarts(pod) {
			return nil, false
		}
		if running < 0 && pod.Status.Phase == corev1.PodRunning && pod.Spec.NodeName != "" && ptahRunningSince(pod, p.startedAt) {
			running = index
		}
		if event.Type == watch.Deleted {
			last = index
		}
	}
	if running < 0 || last < 0 || running >= last {
		return nil, false
	}
	return pods[last].Object, true
}

// deadlinePodWatchAudited is the watch evidence the running-deadline Pod is
// audited by once Kubernetes has deleted it: the Pod ran and was then deleted,
// and it is the one Pod added for its operation under its Job.
func deadlinePodWatchAudited(pods []watchEvent[*corev1.Pod], pod deadlinePod) (*corev1.Pod, bool) {
	deleted, ok := pod.runningThenDeleted(pods)
	if !ok {
		return nil, false
	}
	var added []string
	for _, event := range pods {
		object := event.Object
		if event.Type == watch.Added && labelIs(object, labelOperation, "apply") &&
			annotationIs(object, annotationOperationID, pod.operationID) &&
			slices.ContainsFunc(object.OwnerReferences, func(reference metav1.OwnerReference) bool {
				return reference.APIVersion == "batch/v1" && reference.Kind == "Job" && reference.Name == pod.jobName &&
					string(reference.UID) == pod.jobUID && isController(reference)
			}) {
			added = append(added, string(object.UID))
		}
	}
	slices.Sort(added)
	if !slices.Equal(slices.Compact(added), []string{pod.uid}) {
		return nil, false
	}
	return deleted, true
}

// deadlinePodHistory is the whole watch still telling the running-deadline
// Pod's story at the end: the one Apply Pod the schema ever had, running and
// then deleted.
func deadlinePodHistory(pods []watchEvent[*corev1.Pod], schema string, pod deadlinePod) bool {
	if !slices.Equal(addedUIDs(pods, schema, "apply"), []string{pod.uid}) {
		return false
	}
	_, ok := pod.runningThenDeleted(pods)
	return ok
}

// deadlineJobHistory is the one Apply Job the schema ever had, seen failed by
// its deadline with the deadline it was given on the Job and its template.
func deadlineJobHistory(jobs []watchEvent[*batchv1.Job], schema, uid string, deadline int64) bool {
	if !slices.Equal(addedUIDs(jobs, schema, "apply"), []string{uid}) {
		return false
	}
	return slices.ContainsFunc(jobs, func(event watchEvent[*batchv1.Job]) bool {
		job := event.Object
		return uidIs(job, uid) && deadlineIs(job, deadline) && deadlineExceededCondition(job) != nil
	})
}

func deadlineIs(job *batchv1.Job, deadline int64) bool {
	return int64PointerIs(job.Spec.ActiveDeadlineSeconds, deadline) &&
		int64PointerIs(job.Spec.Template.Spec.ActiveDeadlineSeconds, deadline)
}

func deadlineExceededCondition(job *batchv1.Job) *batchv1.JobCondition {
	for index := range job.Status.Conditions {
		condition := &job.Status.Conditions[index]
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue &&
			condition.Reason == batchv1.JobReasonDeadlineExceeded {
			return condition
		}
	}
	return nil
}

// deadlineJobExceeded is the Job failed by its deadline, no sooner than a
// second before the deadline measured from its own start.
func deadlineJobExceeded(job *batchv1.Job, uid string, deadline int64) bool {
	condition := deadlineExceededCondition(job)
	return uidIs(job, uid) && deadlineIs(job, deadline) && job.Status.StartTime != nil && condition != nil &&
		condition.LastTransitionTime.Sub(job.Status.StartTime.Time) >= time.Duration(deadline-1)*time.Second
}

// runningDeadlineJob is the one-shot Apply Job the deadline proof lets run:
// live, its operation's, the deadline on the Job and its template, one Pod,
// no retry and no replacement, started.
func runningDeadlineJob(job *batchv1.Job, uid, schema, operationID string, deadline int64) bool {
	spec := job.Spec
	return uidIs(job, uid) && job.DeletionTimestamp == nil && operationOf(job, schema, "apply") &&
		annotationIs(job, annotationOperationID, operationID) && deadlineIs(job, deadline) &&
		int32PointerIs(spec.Parallelism, 1) && int32PointerIs(spec.Completions, 1) &&
		int32PointerIs(spec.BackoffLimit, 0) &&
		spec.PodReplacementPolicy != nil && *spec.PodReplacementPolicy == batchv1.Failed &&
		job.Status.StartTime != nil
}

// runningDeadlinePod is that Job's one Pod, live and running on a node with
// its ptah container started once, and the instant it started.
func runningDeadlinePod(pod *corev1.Pod, podUID, jobName, jobUID, operationID string, deadline int64) (string, bool) {
	var started []string
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "ptah" && status.State.Running != nil && !status.State.Running.StartedAt.IsZero() {
			started = append(started, instantOf(status.State.Running.StartedAt))
		}
	}
	ok := uidIs(pod, podUID) && pod.DeletionTimestamp == nil && annotationIs(pod, annotationOperationID, operationID) &&
		exactJobOwner(pod.OwnerReferences, jobName, jobUID) &&
		int64PointerIs(pod.Spec.ActiveDeadlineSeconds, deadline) && pod.Spec.NodeName != "" &&
		pod.Status.Phase == corev1.PodRunning && pod.Status.StartTime != nil && len(started) == 1 && noRestarts(pod)
	if !ok {
		return "", false
	}
	return started[0], true
}

// readyManagerPodUIDs is the sorted UIDs of the manager's live Pods when there
// is one ready Pod for every replica and no other.
func readyManagerPodUIDs(pods []corev1.Pod, replicas int) ([]string, bool) {
	var uids []string
	for index := range pods {
		pod := &pods[index]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if !podReadyTrue(pod) {
			return nil, false
		}
		uids = append(uids, string(pod.UID))
	}
	if len(uids) != replicas {
		return nil, false
	}
	slices.Sort(uids)
	if len(slices.Compact(slices.Clone(uids))) != replicas {
		return nil, false
	}
	return uids, true
}

// managerPodsReplaced is a rollout that replaced every manager Pod: as many
// new Pods as old, at least one, and no old UID among the new.
func managerPodsReplaced(before, after []string) bool {
	if len(before) != len(after) || len(before) == 0 {
		return false
	}
	for _, uid := range before {
		if slices.Contains(after, uid) {
			return false
		}
	}
	return true
}

// leaderPodName is the Pod a leader-election holder identity names: the
// identity without its last underscore and what follows.
func leaderPodName(holder string) string {
	if index := strings.LastIndex(holder, "_"); index >= 0 {
		return holder[:index]
	}
	return holder
}

// leaderPodReady is a live, ready manager Pod of the release, other than the
// one the previous UID names.
func leaderPodReady(pod *corev1.Pod, previousUID, release string) bool {
	return pod.DeletionTimestamp == nil && string(pod.UID) != previousUID &&
		labelIs(pod, "app.kubernetes.io/instance", release) &&
		labelIs(pod, "app.kubernetes.io/component", "controller") && podReadyTrue(pod)
}

// replaceDatabaseURLPath points a database URL at another database on the
// same server: the last path segment is replaced, and the query or fragment
// kept. A URL with no database path is refused.
func replaceDatabaseURLPath(original, database string) (string, error) {
	path, suffix := original, ""
	if index := strings.Index(original, "?"); index >= 0 {
		path, suffix = original[:index], original[index:]
	} else if index := strings.Index(original, "#"); index >= 0 {
		path, suffix = original[:index], original[index:]
	}
	// The shell matched the path against the pattern *://*/?*: a slash after
	// the scheme separator with at least one byte after it. The earliest
	// separator leaves the most room for one.
	scheme := strings.Index(path, "://")
	if scheme < 0 || len(path)-1 <= scheme+3 || !strings.Contains(path[scheme+3:len(path)-1], "/") {
		return "", errors.New("database URL does not contain a non-empty database path")
	}
	return path[:strings.LastIndex(path, "/")] + "/" + database + suffix, nil
}

// shortServiceURL spells a database URL's host by its Service name alone
// rather than the Service's cluster domain name: the same database, another
// route.
func shortServiceURL(url, service, namespace string) (string, error) {
	long := service + "." + namespace + ".svc.cluster.local"
	aliased := strings.Replace(url, long, service, 1)
	if aliased == url {
		return "", errors.New("database URL alias did not change the route spelling")
	}
	return aliased, nil
}

// leaseIdentity is a target Lease as a proof pins it.
type leaseIdentity struct {
	name, uid, holder, epoch string
}

func (l leaseIdentity) String() string {
	return fmt.Sprintf("Lease %s UID %s held by %q at %s", l.name, l.uid, l.holder, l.epoch)
}

// coordinationLeaseUIDs is the checkpoint of the target Leases that exist now.
func coordinationLeaseUIDs(leases []coordinationv1.Lease) checkpoint {
	uids := make([]string, 0, len(leases))
	for index := range leases {
		uids = append(uids, string(leases[index].UID))
	}
	return sortedCheckpoint(uids)
}

// newReleasedLease is the one target Lease the checkpoint does not hold, when
// there is exactly one: it has to be released, at a valid epoch. The count of
// new Leases is returned whatever it is.
func newReleasedLease(leases []coordinationv1.Lease, before checkpoint) (leaseIdentity, int, error) {
	var fresh []*coordinationv1.Lease
	for index := range leases {
		if !before.holds(string(leases[index].UID)) {
			fresh = append(fresh, &leases[index])
		}
	}
	if len(fresh) != 1 {
		return leaseIdentity{}, len(fresh), nil
	}
	lease := fresh[0]
	identity := leaseIdentity{name: lease.Name, uid: string(lease.UID), epoch: lease.Annotations[annotationLeaseEpoch]}
	if !holderEmpty(lease) {
		return identity, 1, errors.New("was not released after its initial Plan")
	}
	if !leaseEpochPattern.MatchString(identity.epoch) {
		return identity, 1, errors.New("has no valid released acquisition epoch")
	}
	return identity, 1, nil
}

// reacquiredLease is the released Lease held again: the same UID, a holder,
// and a valid epoch other than the one it was released at.
func reacquiredLease(lease *coordinationv1.Lease, uid, previousEpoch string) (leaseIdentity, bool) {
	epoch := lease.Annotations[annotationLeaseEpoch]
	if !uidIs(lease, uid) || holderEmpty(lease) || !leaseEpochPattern.MatchString(epoch) || epoch == previousEpoch {
		return leaseIdentity{}, false
	}
	return leaseIdentity{name: lease.Name, uid: uid, holder: *lease.Spec.HolderIdentity, epoch: epoch}, true
}

// heldLeasesAtEpoch is every target Lease held at the epoch.
func heldLeasesAtEpoch(leases []coordinationv1.Lease, epoch string) []leaseIdentity {
	var held []leaseIdentity
	for index := range leases {
		lease := &leases[index]
		if annotationIs(lease, annotationLeaseEpoch, epoch) && !holderEmpty(lease) {
			held = append(held, leaseIdentity{
				name: lease.Name, uid: string(lease.UID), holder: *lease.Spec.HolderIdentity, epoch: epoch,
			})
		}
	}
	return held
}

// leaseIs is a Lease with the identity pinned: its UID, its holder and its
// epoch.
func leaseIs(lease *coordinationv1.Lease, identity leaseIdentity) bool {
	return uidIs(lease, identity.uid) && heldAs(lease, identity.holder, identity.epoch)
}

// watchedUIDs is every distinct UID a watch saw added, each one a usable
// ledger line: not empty, at most 128 characters, as jq's length counts them,
// and without a line break or a tab.
func watchedUIDs[T client.Object](events []watchEvent[T]) ([]string, error) {
	var uids []string
	for _, event := range events {
		if event.Type != watch.Added {
			continue
		}
		uid := string(event.Object.GetUID())
		if uid == "" || utf8.RuneCountInString(uid) > 128 || strings.ContainsAny(uid, "\n\r\t") {
			return nil, errors.New("watch contains an invalid object UID")
		}
		uids = append(uids, uid)
	}
	slices.Sort(uids)
	return slices.Compact(uids), nil
}

// missingWatchedAudits keeps every added UID in the audit obligation, even
// when the watch ended before the object finished or saw it deleted. Only
// the credential audit can discharge that obligation.
func missingWatchedAudits[T client.Object](events []watchEvent[T], audited map[string]bool) ([]string, error) {
	uids, err := watchedUIDs(events)
	if err != nil {
		return nil, err
	}
	if len(uids) == 0 {
		return nil, errors.New("audit watch contains no added objects")
	}
	var missing []string
	for _, uid := range uids {
		if !audited[uid] {
			missing = append(missing, uid)
		}
	}
	return missing, nil
}

// addedJobs is every Job the watch saw added, once each, in the order it
// first saw them.
func addedJobs(jobs []watchEvent[*batchv1.Job]) []batchv1.Job {
	seen := map[types.UID]bool{}
	var added []batchv1.Job
	for _, event := range jobs {
		if event.Type == watch.Added && !seen[event.Object.UID] {
			seen[event.Object.UID] = true
			added = append(added, *event.Object)
		}
	}
	return added
}

// tolerationMatchesBarrier is a toleration that would let a Pod past the
// read-workload barrier's NoSchedule taint.
func tolerationMatchesBarrier(toleration corev1.Toleration) bool {
	if toleration.Effect != "" && toleration.Effect != corev1.TaintEffectNoSchedule {
		return false
	}
	if toleration.Operator == corev1.TolerationOpExists {
		return toleration.Key == "" || toleration.Key == readWorkloadBarrierKey
	}
	return toleration.Key == readWorkloadBarrierKey && toleration.Value == "held"
}

// jobHeldByBarrier is a Job the barrier holds: unfinished, and its Pods
// tolerate no taint the barrier puts on a node.
func jobHeldByBarrier(job *batchv1.Job) bool {
	return !jobTerminal(job) && !slices.ContainsFunc(job.Spec.Template.Spec.Tolerations, tolerationMatchesBarrier)
}

// podsUnscheduled are Pods on no node whose containers neither run nor ran.
func podsUnscheduled(pods []corev1.Pod) bool {
	for index := range pods {
		pod := &pods[index]
		if pod.Spec.NodeName != "" {
			return false
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Running != nil || status.State.Terminated != nil {
				return false
			}
		}
	}
	return true
}

// podsNeverStarted are podsUnscheduled, counting init and ephemeral
// containers too: a Pod that ran nothing wrote no log.
func podsNeverStarted(pods []corev1.Pod) bool {
	for index := range pods {
		pod := &pods[index]
		if pod.Spec.NodeName != "" || len(startedContainers(pod)) != 0 {
			return false
		}
	}
	return true
}

func barrierTaints(node *corev1.Node) []corev1.Taint {
	var taints []corev1.Taint
	for _, taint := range node.Spec.Taints {
		if taint.Key == readWorkloadBarrierKey {
			taints = append(taints, taint)
		}
	}
	return taints
}

// nodesCarryBarrier is every node, of at least one, carrying the barrier's
// exact taint once: its key, the value held and the NoSchedule effect. A taint
// of the same key with another value or effect is not that taint, and the
// shell counted only exact ones.
func nodesCarryBarrier(nodes []corev1.Node) bool {
	if len(nodes) == 0 {
		return false
	}
	for index := range nodes {
		exact := 0
		for _, taint := range barrierTaints(&nodes[index]) {
			if taint.Value == "held" && taint.Effect == corev1.TaintEffectNoSchedule {
				exact++
			}
		}
		if exact != 1 {
			return false
		}
	}
	return true
}

// nodesFreeOfBarrier is no node carrying the barrier's taint.
func nodesFreeOfBarrier(nodes []corev1.Node) bool {
	for index := range nodes {
		if len(barrierTaints(&nodes[index])) != 0 {
			return false
		}
	}
	return true
}

// managedOperation is an object the operator made for a schema operation.
func managedOperation(object metav1.Object) bool {
	return labelIs(object, labelManagedBy, managedByOperator) && labelIs(object, labelComponent, schemaOperationComponent)
}

// executionIdentityOnPod is a Pod stamped with this manager's identity and an
// execution binding.
func executionIdentityOnPod(pod *corev1.Pod, controller controllerIdentity) bool {
	return controller.stampedOn(pod.Annotations) && leaseEpochPattern.MatchString(pod.Annotations[annotationBindingID])
}

// executionIdentityOnJob is a Job stamped with this manager's identity and an
// execution binding, on the Job and on its Pod template alike.
func executionIdentityOnJob(job *batchv1.Job, controller controllerIdentity) bool {
	binding, ok := job.Annotations[annotationBindingID]
	template := job.Spec.Template.Annotations
	return ok && leaseEpochPattern.MatchString(binding) && controller.stampedOn(job.Annotations) &&
		template[annotationBindingID] == binding && controller.stampedOn(template)
}

// faultApprovalConsumed is an approval, as stored, that dispatched the plan
// named and was retired with it: nothing in its spec but what a person wrote
// and admission stamped, and the history of both kept.
func faultApprovalConsumed(stored *unstructured.Unstructured, planUID string) error {
	spec, found, err := unstructured.NestedMap(stored.Object, "spec")
	if err != nil || !found {
		return errors.New("it has no spec")
	}
	if keys := slices.Sorted(maps.Keys(spec)); !slices.Equal(keys,
		[]string{"approvedAt", "approver", "mutationRequestUID", "planFingerprint", "planRef", "schemaRef"}) {
		return fmt.Errorf("its spec carries %v", keys)
	}
	approval := &ptahv1alpha1.PtahSchemaApproval{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, approval); err != nil {
		return fmt.Errorf("it does not decode: %w", err)
	}
	conditions := approval.Status.Conditions
	switch {
	case planUID == "" || string(approval.Spec.PlanRef.UID) != planUID:
		return fmt.Errorf("it approves plan UID %s", approval.Spec.PlanRef.UID)
	case !conditionIs(conditions, "Consumed", metav1.ConditionTrue, "DispatchCommitted"):
		return errors.New("it is not Consumed by a committed dispatch")
	case !conditionIs(conditions, "Accepted", metav1.ConditionFalse, "PlanNoLongerCurrent"):
		return errors.New("it is still Accepted")
	case !conditionIs(conditions, "Stale", metav1.ConditionTrue, "PlanNoLongerCurrent"):
		return errors.New("it is not Stale")
	}
	return nil
}

// approvalRetired is an approval whose dispatch was committed and whose plan
// is no longer current.
func approvalRetired(approval *ptahv1alpha1.PtahSchemaApproval) bool {
	conditions := approval.Status.Conditions
	return conditionIs(conditions, "Consumed", metav1.ConditionTrue, "DispatchCommitted") &&
		conditionIs(conditions, "Stale", metav1.ConditionTrue, "PlanNoLongerCurrent")
}

// principalCredentials are the role and password the credential-principal
// artifact declares, derived from the namespace as cksum derived them.
func principalCredentials(namespace string) (role, password string) {
	suffix := strconv.FormatUint(uint64(posixCksum([]byte(namespace+"-principal"))), 10)
	return "e2e_credential_principal_" + suffix, "e2ePrincipal" + suffix + "Q7"
}

// principalSchemaHCL is the artifact that declares a login role with a
// password: a plan of it would carry the credential, so the runner has to
// refuse it before any plan output leaves the Pod.
func principalSchemaHCL(role, password string) string {
	return fmt.Sprintf("schema \"public\" {}\n\nrole \"%s\" {\n  login = true\n  password = \"%s\"\n}\n", role, password)
}

// publishedDigests is every digest a publisher log reports, in order.
func publishedDigests(log []byte) []string {
	var digests []string
	for line := range strings.SplitSeq(string(log), "\n") {
		if match := publishedDigest.FindStringSubmatch(line); match != nil {
			digests = append(digests, match[1])
		}
	}
	return digests
}
