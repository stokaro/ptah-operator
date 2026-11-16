package e2e

import (
	"errors"
	"fmt"
	"strings"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/resourceversion"
	"k8s.io/apimachinery/pkg/watch"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// applyExecutorContainer is the container of an Apply Pod that runs Ptah, the
// one whose state the manager reads as the executor's.
const applyExecutorContainer = "ptah"

// applyRemovalBranch is which of the two outcomes the manager's contract
// allows a deleted Apply Job produced.
//
// A Job deleted with its Pods, as kubectl deletes it, leaves its Pod stored
// Failed, its executor terminated, while the Pod still names the Job: the
// kubelet does not let a gracefully deleted Pod go before it has stored a
// terminal phase. A manager pass that reads that state hands the realm Lease
// back at once. One that reads the Pod running, or finds it gone, keeps the
// claim and the Lease to the end of the claim's term (ApplyPodGone). Which
// pass comes first is timing, so a row that deletes the Job has to measure
// whichever branch the run produced.
type applyRemovalBranch int

const (
	// applyHeldToTerm is a run in which the Pod went and the manager still
	// held the Lease on its next write.
	applyHeldToTerm applyRemovalBranch = iota + 1
	// applyReleasedOnStop is a run in which the manager let go after the
	// kubelet stored the Pod stopped under the Job and before the Pod went.
	applyReleasedOnStop
)

func (b applyRemovalBranch) String() string {
	switch b {
	case applyHeldToTerm:
		return "held to its term"
	case applyReleasedOnStop:
		return "released on the kubelet's stop"
	}
	return fmt.Sprintf("applyRemovalBranch(%d)", int(b))
}

// applyRemovalSubject is what a removal is read against: the Apply's Pod and
// Job, the realm Lease and the holder and epoch the claim took it under, and
// the migration with its claim as it stood before the Job was deleted.
type applyRemovalSubject struct {
	PodUID, JobUID types.UID
	Lease          types.NamespacedName
	Holder, Epoch  string
	MigrationUID   types.UID
	Claim          *ptahv1alpha1.MigrationOperationStatus
}

// applyRemovalHistories are the watch histories of the Pods of the Apply's
// namespace, the Leases of the realm Lease's namespace and the PtahMigrations
// of the migration's namespace, each from a resourceVersion read before the
// Job was deleted.
type applyRemovalHistories struct {
	Pods       []watchEvent[*corev1.Pod]
	Leases     []watchEvent[*coordinationv1.Lease]
	Migrations []watchEvent[*ptahv1alpha1.PtahMigration]
}

// applyRemovalVerdict is the branch a removal took and the resourceVersions
// that decided it. Gone is the Pod's removal and Stopped the first state in
// which the Pod named the Job and read its executor stopped. On the hold
// branch Settled is the first write of the realm Lease after the removal. On
// the release branch Released is the first write that let the claim or the
// Lease go, by ReleasedBy.
type applyRemovalVerdict struct {
	Branch     applyRemovalBranch
	Gone       string
	Settled    string
	Stopped    string
	Released   string
	ReleasedBy string
}

func (v applyRemovalVerdict) String() string {
	if v.Branch == applyReleasedOnStop {
		return fmt.Sprintf("%s: the Pod read its executor stopped under the Job at resourceVersion %s, the %s let go at %s, and the Pod went at %s",
			v.Branch, v.Stopped, v.ReleasedBy, v.Released, v.Gone)
	}
	return fmt.Sprintf("%s: the Pod went at resourceVersion %s, and the realm Lease was next written at %s, still held",
		v.Branch, v.Gone, v.Settled)
}

// errApplyRemovalPending is a reading the histories cannot decide yet.
var errApplyRemovalPending = errors.New("the removal is not decided yet")

// readApplyRemoval decides which branch a removal took, from the stored
// order of the writes. Any other error is a run the manager's contract does
// not allow, or histories that cannot be read.
//
// The release branch is a release whose first write came before the Pod's
// removal. The release is dated by that first write: the migration's first
// write without the claim, with a lock release staged, or its removal, and
// the Lease's first write without the holder, whichever came first, so both
// histories have to hold theirs. It has to follow a stored state in which the
// Pod named the Job and read Succeeded or Failed with its executor
// terminated, because that is the only state the manager may release on.
//
// The hold branch is decided by the first realm Lease write after the Pod's
// removal, where it still names the claim's holder and epoch and nothing let
// go before it. The manager writes the Lease on every pass that holds it, and
// passes for one migration do not overlap, so every pass after that write
// reads the Pod gone and has to hold to the term, which the hold rows measure.
//
// A release whose first write came after the Pod's removal, with no held write
// between, is refused. That is the release the hold exists to prevent, and a
// stopped state before it does not excuse it: every background deletion
// stores one. The manager reads the Pods from the API server rather than its
// cache, and writes the migration next, so a pass that read the stop writes
// first after the removal only where the removal landed in those milliseconds,
// and that run is refused with it. The other order this cannot see is a
// defect that read the Pod running and released with a write that landed
// after the kubelet's.
//
// The resourceVersions of a Pod, a Lease and a PtahMigration are compared with
// one another. The API promises an order only within one resource
// (resourceversion.CompareResourceVersion). Each is the etcd revision of its
// write where every resource is stored in one etcd, which
// singleEtcdForEveryResource holds the cluster to before these are read. A
// watch delivers one resource in order, so the first retirement a history
// holds is the first one written.
func readApplyRemoval(h applyRemovalHistories, s applyRemovalSubject) (applyRemovalVerdict, error) {
	var verdict applyRemovalVerdict
	if s.PodUID == "" || s.JobUID == "" || s.Lease.Name == "" || s.Holder == "" || s.Epoch == "" || s.MigrationUID == "" || s.Claim == nil {
		return verdict, errors.New("the removal names no complete Pod, Job, Lease, holder, epoch and claim")
	}
	owned := 0
	for _, event := range h.Pods {
		pod := event.Object
		if pod == nil {
			continue
		}
		underJob := controlledByJob(pod.OwnerReferences, s.JobUID)
		if underJob && pod.UID != s.PodUID {
			return verdict, fmt.Errorf("another Pod, %s with UID %s, names the Job as its controller", pod.Name, pod.UID)
		}
		if pod.UID != s.PodUID {
			continue
		}
		if underJob {
			owned++
		}
		if verdict.Stopped == "" && underJob && executorStopped(pod) {
			verdict.Stopped = pod.ResourceVersion
		}
		if event.Type == watch.Deleted {
			verdict.Gone = pod.ResourceVersion
		}
	}
	if verdict.Gone == "" {
		return verdict, fmt.Errorf("%w: the Pod's removal is not in its history", errApplyRemovalPending)
	}
	// Read against another Job, every state of the Pod would pass as not the
	// Job's, and neither branch would mean anything.
	if owned == 0 {
		return verdict, errors.New("no state of the Pod in the history names the Job as its controller")
	}

	var released, retired string
	settledHeld := false
	for _, event := range h.Leases {
		lease := event.Object
		if lease == nil || lease.Namespace != s.Lease.Namespace || lease.Name != s.Lease.Name {
			continue
		}
		held := event.Type != watch.Deleted && lease.Spec.HolderIdentity != nil &&
			*lease.Spec.HolderIdentity == s.Holder && annotationIs(lease, annotationLeaseEpoch, s.Epoch)
		if !held && released == "" {
			released = lease.ResourceVersion
		}
		if verdict.Settled != "" {
			continue
		}
		after, err := resourceVersionBefore(verdict.Gone, lease.ResourceVersion)
		if err != nil {
			return verdict, err
		}
		if after {
			verdict.Settled, settledHeld = lease.ResourceVersion, held
		}
	}
	for _, event := range h.Migrations {
		migration := event.Object
		if migration == nil || migration.UID != s.MigrationUID {
			continue
		}
		if event.Type == watch.Deleted || migration.Status.PendingLockRelease != nil ||
			!equality.Semantic.DeepEqual(migration.Status.ActiveOperation, s.Claim) {
			retired = migration.ResourceVersion
			break
		}
	}

	// The release is dated by its first write, which is known once both
	// histories hold theirs.
	if released != "" && retired != "" {
		verdict.Released, verdict.ReleasedBy = released, "realm Lease"
		if first, err := resourceVersionBefore(retired, released); err != nil {
			return verdict, err
		} else if first {
			verdict.Released, verdict.ReleasedBy = retired, "PtahMigration"
		}
		beforeGone, err := resourceVersionBefore(verdict.Released, verdict.Gone)
		if err != nil {
			return verdict, err
		}
		if beforeGone {
			verdict.Branch, verdict.Settled = applyReleasedOnStop, ""
			if verdict.Stopped == "" {
				return verdict, fmt.Errorf("the %s let go at resourceVersion %s, and no stored state of the Pod named the Job and read its executor stopped",
					verdict.ReleasedBy, verdict.Released)
			}
			if stoppedFirst, err := resourceVersionBefore(verdict.Stopped, verdict.Released); err != nil {
				return verdict, err
			} else if !stoppedFirst {
				return verdict, fmt.Errorf("the %s let go at resourceVersion %s, before the Pod was stored with its executor stopped under the Job at %s",
					verdict.ReleasedBy, verdict.Released, verdict.Stopped)
			}
			return verdict, nil
		}
	}

	if verdict.Settled != "" && settledHeld {
		// Nothing may have let go between the Pod's removal and the write
		// that held the Lease after it. A release before the removal decides
		// the other branch once both histories hold it.
		for _, letGo := range []struct{ by, at string }{{"realm Lease", released}, {"PtahMigration", retired}} {
			if letGo.at == "" {
				continue
			}
			first, err := resourceVersionBefore(letGo.at, verdict.Settled)
			if err != nil {
				return verdict, err
			}
			if !first {
				continue
			}
			if beforeGone, err := resourceVersionBefore(letGo.at, verdict.Gone); err != nil {
				return verdict, err
			} else if beforeGone {
				return verdict, fmt.Errorf("%w: the %s let go at resourceVersion %s, before the Pod went, and the other history has not shown its release yet",
					errApplyRemovalPending, letGo.by, letGo.at)
			}
			return verdict, fmt.Errorf("the %s let go at resourceVersion %s, after the Pod went at %s, and the realm Lease was written still held at %s after that",
				letGo.by, letGo.at, verdict.Gone, verdict.Settled)
		}
		verdict.Branch, verdict.Released, verdict.ReleasedBy = applyHeldToTerm, "", ""
		return verdict, nil
	}

	switch {
	case verdict.Released != "":
		// The manager reads the Pods from the API server, not its cache,
		// and writes the migration next, so a pass that read the stop wrote
		// before the Pod went unless the removal landed in between.
		return verdict, fmt.Errorf("the %s let go at resourceVersion %s, after the Pod went at %s, with no write that held the realm Lease in between: "+
			"the manager released on a Pod that was gone, before the claim's term", verdict.ReleasedBy, verdict.Released, verdict.Gone)
	case released != "":
		return verdict, fmt.Errorf("%w: the realm Lease was let go at resourceVersion %s and the migration's history does not show its claim retired yet",
			errApplyRemovalPending, released)
	case retired != "":
		return verdict, fmt.Errorf("%w: the migration let its claim go at resourceVersion %s and the realm Lease's history does not show it released yet",
			errApplyRemovalPending, retired)
	}
	return verdict, fmt.Errorf("%w: the realm Lease has not been written since the Pod went", errApplyRemovalPending)
}

// executorStopped is a Pod that reads Succeeded or Failed with its executor
// terminated: the kubelet's account that the executor stopped.
func executorStopped(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == applyExecutorContainer {
			return status.State.Terminated != nil
		}
	}
	return false
}

// resourceVersionBefore reports whether resourceVersion a was written before b.
func resourceVersionBefore(a, b string) (bool, error) {
	order, err := resourceversion.CompareResourceVersion(a, b)
	if err != nil {
		return false, fmt.Errorf("compare resourceVersions %q and %q: %w", a, b, err)
	}
	return order < 0, nil
}

// singleEtcdForEveryResource is nil where every kube-apiserver Pod of the
// control plane stores every resource in the one etcd its --etcd-servers
// names. --etcd-servers-overrides moves a resource to an etcd of its own,
// whose revisions say nothing about another's, and readApplyRemoval would
// then compare numbers from two sequences.
func singleEtcdForEveryResource(apiServers []corev1.Pod) error {
	if len(apiServers) == 0 {
		return errors.New("no kube-apiserver Pod was found to read the storage flags from")
	}
	for _, pod := range apiServers {
		containers := 0
		for _, container := range pod.Spec.Containers {
			if container.Name != "kube-apiserver" {
				continue
			}
			containers++
			for _, argument := range append(append([]string(nil), container.Command...), container.Args...) {
				if strings.HasPrefix(argument, "--etcd-servers-overrides") {
					return fmt.Errorf("kube-apiserver Pod %s stores some resources elsewhere: %s", pod.Name, argument)
				}
			}
		}
		if containers != 1 {
			return fmt.Errorf("kube-apiserver Pod %s has %d kube-apiserver containers", pod.Name, containers)
		}
	}
	return nil
}

// applyRemovalReading is what a row prints once a removal is decided: the
// subject, the verdict, and the histories cut down to the objects and fields
// readApplyRemoval reads, so a stored copy decodes back into a reading it can
// decide again.
type applyRemovalReading struct {
	Subject   applyRemovalSubject   `json:"subject"`
	Verdict   applyRemovalVerdict   `json:"verdict"`
	Histories applyRemovalHistories `json:"histories"`
}

// trimmed keeps the subject's Pods, its realm Lease and its migration, and of
// each the fields the decision reads. A Pod's spec names Secrets and a
// migration's spec names its source, and neither is read.
func (h applyRemovalHistories) trimmed(s applyRemovalSubject) applyRemovalHistories {
	var out applyRemovalHistories
	for _, event := range h.Pods {
		pod := event.Object
		if pod == nil || pod.UID != s.PodUID && !controlledByJob(pod.OwnerReferences, s.JobUID) {
			continue
		}
		kept := &corev1.Pod{}
		kept.Name, kept.Namespace, kept.UID, kept.ResourceVersion = pod.Name, pod.Namespace, pod.UID, pod.ResourceVersion
		kept.OwnerReferences, kept.DeletionTimestamp = pod.OwnerReferences, pod.DeletionTimestamp
		kept.Status.Phase = pod.Status.Phase
		for _, status := range pod.Status.ContainerStatuses {
			kept.Status.ContainerStatuses = append(kept.Status.ContainerStatuses, corev1.ContainerStatus{Name: status.Name, State: status.State})
		}
		out.Pods = append(out.Pods, watchEvent[*corev1.Pod]{Type: event.Type, Object: kept})
	}
	for _, event := range h.Leases {
		lease := event.Object
		if lease == nil || lease.Namespace != s.Lease.Namespace || lease.Name != s.Lease.Name {
			continue
		}
		kept := &coordinationv1.Lease{}
		kept.Name, kept.Namespace, kept.UID, kept.ResourceVersion = lease.Name, lease.Namespace, lease.UID, lease.ResourceVersion
		if epoch, ok := lease.Annotations[annotationLeaseEpoch]; ok {
			kept.Annotations = map[string]string{annotationLeaseEpoch: epoch}
		}
		kept.Spec.HolderIdentity, kept.Spec.RenewTime = lease.Spec.HolderIdentity, lease.Spec.RenewTime
		out.Leases = append(out.Leases, watchEvent[*coordinationv1.Lease]{Type: event.Type, Object: kept})
	}
	for _, event := range h.Migrations {
		migration := event.Object
		if migration == nil || migration.UID != s.MigrationUID {
			continue
		}
		kept := &ptahv1alpha1.PtahMigration{}
		kept.Name, kept.Namespace, kept.UID, kept.ResourceVersion = migration.Name, migration.Namespace, migration.UID, migration.ResourceVersion
		kept.DeletionTimestamp = migration.DeletionTimestamp
		kept.Status.ActiveOperation, kept.Status.PendingLockRelease = migration.Status.ActiveOperation, migration.Status.PendingLockRelease
		out.Migrations = append(out.Migrations, watchEvent[*ptahv1alpha1.PtahMigration]{Type: event.Type, Object: kept})
	}
	return out
}
