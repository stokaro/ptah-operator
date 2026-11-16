package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The histories below follow the shape a kind 1.37.0 cluster stored for a Job
// deleted with background propagation: the Pod, still naming the Job, loses
// the tracking finalizer, is marked for deletion, reads Failed with its
// executor terminated, and goes. Each row interleaves the manager's writes at
// the resourceVersions one branch or one defect would give them.
const (
	removalPodUID       = types.UID("0b8e6f0e-1d1c-4b79-9a31-9a7f0f1a0001")
	removalJobUID       = types.UID("0b8e6f0e-1d1c-4b79-9a31-9a7f0f1a0002")
	removalMigrationUID = types.UID("0b8e6f0e-1d1c-4b79-9a31-9a7f0f1a0003")
	removalHolder       = "sha256:holder"
	removalEpoch        = "epoch-1"
)

func removalSubject() applyRemovalSubject {
	return applyRemovalSubject{
		PodUID: removalPodUID, JobUID: removalJobUID,
		Lease:  types.NamespacedName{Namespace: "ptah-system", Name: "ptah-realm-1"},
		Holder: removalHolder, Epoch: removalEpoch,
		MigrationUID: removalMigrationUID, Claim: removalClaim(),
	}
}

func removalClaim() *ptahv1alpha1.MigrationOperationStatus {
	return &ptahv1alpha1.MigrationOperationStatus{
		Type: ptahv1alpha1.MigrationOperationApply, ID: "operation-1", JobName: "ptah-m-apply-1",
		JobUID: removalJobUID, LeaseEpoch: removalEpoch, LeaseDurationSeconds: 420, DispatchStarted: true,
	}
}

// removalPod is a state of the Apply's Pod at resourceVersion rv.
func removalPod(rv int, phase corev1.PodPhase, executor corev1.ContainerState, underJob bool) watchEvent[*corev1.Pod] {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ptah-m-apply-1-x7k2p", Namespace: "e2e", UID: removalPodUID, ResourceVersion: strconv.Itoa(rv)},
		Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "guard", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: applyExecutorContainer, State: executor},
		}},
	}
	if underJob {
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "Job", Name: "ptah-m-apply-1", UID: removalJobUID, Controller: ptr.To(true),
		}}
	}
	return watchEvent[*corev1.Pod]{Type: watch.Modified, Object: pod}
}

func removalPodGone(rv int, phase corev1.PodPhase, executor corev1.ContainerState, underJob bool) watchEvent[*corev1.Pod] {
	event := removalPod(rv, phase, executor, underJob)
	event.Type = watch.Deleted
	return event
}

var (
	executorRunning = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	executorStops   = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 143}}
)

// backgroundPodHistory is the Pod from the read before the Job's deletion to
// its removal: still running at 101 and 103, read stopped at 105, gone at 110.
// A resourceVersion is the revision of one write, so no two writes in a row
// share one.
func backgroundPodHistory() []watchEvent[*corev1.Pod] {
	return []watchEvent[*corev1.Pod]{
		removalPod(101, corev1.PodRunning, executorRunning, true),
		removalPod(103, corev1.PodRunning, executorRunning, true),
		removalPod(105, corev1.PodFailed, executorStops, true),
		removalPod(108, corev1.PodFailed, executorStops, true),
		removalPodGone(110, corev1.PodFailed, executorStops, true),
	}
}

// removalLease is a write of the realm Lease at rv, naming holder.
func removalLease(rv int, holder string) watchEvent[*coordinationv1.Lease] {
	return watchEvent[*coordinationv1.Lease]{Type: watch.Modified, Object: &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ptah-system", Name: "ptah-realm-1", ResourceVersion: strconv.Itoa(rv),
			Annotations: map[string]string{annotationLeaseEpoch: removalEpoch},
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder)},
	}}
}

// removalMigration is a write of the migration at rv, with its claim or
// without it.
func removalMigration(rv int, claimed, staged bool) watchEvent[*ptahv1alpha1.PtahMigration] {
	migration := &ptahv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{
		Namespace: "e2e", Name: "e2e-migration", UID: removalMigrationUID, ResourceVersion: strconv.Itoa(rv),
	}}
	if claimed {
		migration.Status.ActiveOperation = removalClaim()
	}
	if staged {
		migration.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{LeaseEpoch: removalEpoch}
	}
	return watchEvent[*ptahv1alpha1.PtahMigration]{Type: watch.Modified, Object: migration}
}

// requireReadingDecidesAgain holds the reading a row prints to deciding what
// the full histories decided, once it is cut down, encoded and decoded, with
// writes of objects the decision does not read added around it.
func requireReadingDecidesAgain(t *testing.T, histories applyRemovalHistories, verdict applyRemovalVerdict) {
	t.Helper()
	noise := removalPod(99, corev1.PodRunning, executorRunning, false)
	noise.Object.UID, noise.Object.Name = "0b8e6f0e-1d1c-4b79-9a31-9a7f0f1a00ff", "database-0"
	histories.Pods = append([]watchEvent[*corev1.Pod]{noise}, histories.Pods...)
	other := removalLease(98, "manager")
	other.Object.Name = "ptah-operator-leader"
	histories.Leases = append([]watchEvent[*coordinationv1.Lease]{other}, histories.Leases...)
	body, err := json.Marshal(applyRemovalReading{Subject: removalSubject(), Verdict: verdict, Histories: histories.trimmed(removalSubject())})
	if err != nil {
		t.Fatal(err)
	}
	var reading applyRemovalReading
	if err := json.Unmarshal(body, &reading); err != nil {
		t.Fatal(err)
	}
	if len(reading.Histories.Pods) != len(histories.Pods)-1 || len(reading.Histories.Leases) != len(histories.Leases)-1 {
		t.Fatalf("the reading kept %d Pod and %d Lease events, want every event of the subject's objects and no other",
			len(reading.Histories.Pods), len(reading.Histories.Leases))
	}
	again, err := readApplyRemoval(reading.Histories, reading.Subject)
	if err != nil || again != verdict || reading.Verdict != verdict {
		t.Fatalf("the printed reading decides %+v, %v; the histories decided %+v", again, err, verdict)
	}
}

func TestReadApplyRemovalDecidesTheHoldBranchByTheFirstLeaseWriteAfterThePodWent(t *testing.T) {
	t.Parallel()

	claimed := []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(104, true, false)}
	for name, row := range map[string]struct {
		leases     []watchEvent[*coordinationv1.Lease]
		migrations []watchEvent[*ptahv1alpha1.PtahMigration]
	}{
		"renewed only after the Pod went": {
			leases: []watchEvent[*coordinationv1.Lease]{removalLease(102, removalHolder), removalLease(112, removalHolder)}, migrations: claimed,
		},
		// A pass that read the Pod running wrote its renewal after the
		// kubelet's stop was stored. That is a hold, not a release.
		"renewed in the stop window as well": {
			leases: []watchEvent[*coordinationv1.Lease]{removalLease(106, removalHolder), removalLease(112, removalHolder)}, migrations: claimed,
		},
		// A release after the first held write is the hold branch too: the
		// hold rows judge it against the term.
		"released after the hold was settled": {
			leases:     []watchEvent[*coordinationv1.Lease]{removalLease(112, removalHolder), removalLease(115, "")},
			migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(104, true, false), removalMigration(114, true, true)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			histories := applyRemovalHistories{Pods: backgroundPodHistory(), Leases: row.leases, Migrations: row.migrations}
			verdict, err := readApplyRemoval(histories, removalSubject())
			if err != nil {
				t.Fatalf("readApplyRemoval() = %v", err)
			}
			requireReadingDecidesAgain(t, histories, verdict)
			if verdict.Branch != applyHeldToTerm || verdict.Gone != "110" || verdict.Settled != "112" {
				t.Fatalf("readApplyRemoval() = %+v, want the hold branch settled at 112 after the Pod went at 110", verdict)
			}
		})
	}
}

func TestReadApplyRemovalAcceptsAReleaseOnlyAfterTheStoppedState(t *testing.T) {
	t.Parallel()

	for name, row := range map[string]struct {
		leases     []watchEvent[*coordinationv1.Lease]
		migrations []watchEvent[*ptahv1alpha1.PtahMigration]
		released   string
		by         string
	}{
		// The pass that read the stop stages the release on the migration,
		// then lets the Lease go, before the Pod is gone.
		"staged and released before the Pod went": {
			leases:     []watchEvent[*coordinationv1.Lease]{removalLease(102, removalHolder), removalLease(107, "")},
			migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(106, true, true), removalMigration(109, false, false)},
			released:   "106", by: "PtahMigration",
		},
		// The Lease goes after the Pod, but the pass staged the release
		// before it, so it read the Pod while it stood.
		"staged before the Pod went and released after it": {
			leases:     []watchEvent[*coordinationv1.Lease]{removalLease(102, removalHolder), removalLease(112, "")},
			migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(106, true, true), removalMigration(113, false, false)},
			released:   "106", by: "PtahMigration",
		},
		"the Lease let go before the migration wrote": {
			leases:     []watchEvent[*coordinationv1.Lease]{removalLease(106, "")},
			migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(109, false, false)},
			released:   "106", by: "realm Lease",
		},
		"the migration removed with its finalizer": {
			leases: []watchEvent[*coordinationv1.Lease]{removalLease(109, "")},
			migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{func() watchEvent[*ptahv1alpha1.PtahMigration] {
				event := removalMigration(106, true, false)
				event.Type = watch.Deleted
				return event
			}()},
			released: "106", by: "PtahMigration",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			histories := applyRemovalHistories{Pods: backgroundPodHistory(), Leases: row.leases, Migrations: row.migrations}
			verdict, err := readApplyRemoval(histories, removalSubject())
			if err != nil {
				t.Fatalf("readApplyRemoval() = %v", err)
			}
			requireReadingDecidesAgain(t, histories, verdict)
			if verdict.Branch != applyReleasedOnStop || verdict.Stopped != "105" || verdict.Released != row.released || verdict.ReleasedBy != row.by {
				t.Fatalf("readApplyRemoval() = %+v, want a release by the %s at %s after the stop at 105", verdict, row.by, row.released)
			}
		})
	}
}

func TestReadApplyRemovalRefusesAReleaseWithNoStoppedStateBeforeIt(t *testing.T) {
	t.Parallel()

	stopped := func(pods []watchEvent[*corev1.Pod]) applyRemovalHistories {
		return applyRemovalHistories{
			Pods:       pods,
			Leases:     []watchEvent[*coordinationv1.Lease]{removalLease(104, "")},
			Migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(102, false, false)},
		}
	}
	for name, row := range map[string]struct {
		histories applyRemovalHistories
		want      string
	}{
		// The release is the manager's at 102, the stop the kubelet's at 105.
		"released while the Pod still ran": {stopped(backgroundPodHistory()), "before the Pod was stored with its executor stopped"},
		"staged while the Pod still ran": {applyRemovalHistories{
			Pods:       backgroundPodHistory(),
			Leases:     []watchEvent[*coordinationv1.Lease]{removalLease(106, "")},
			Migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(104, true, true)},
		}, "PtahMigration let go at resourceVersion 104, before"},
		// A Pod deleted with --force goes with no stopped state at all.
		"released with the Pod force-deleted": {stopped([]watchEvent[*corev1.Pod]{
			removalPod(101, corev1.PodRunning, executorRunning, true),
			removalPodGone(103, corev1.PodRunning, executorRunning, true),
		}), "no stored state of the Pod named the Job and read its executor stopped"},
		"released on a stop of a Pod that no longer named the Job": {stopped([]watchEvent[*corev1.Pod]{
			removalPod(101, corev1.PodRunning, executorRunning, true),
			removalPod(103, corev1.PodRunning, executorRunning, false),
			removalPod(105, corev1.PodFailed, executorStops, false),
			removalPodGone(107, corev1.PodFailed, executorStops, false),
		}), "no stored state of the Pod named the Job"},
		// PodGC writes Failed over a running executor.
		"released on a Failed phase over a running executor": {stopped([]watchEvent[*corev1.Pod]{
			removalPod(101, corev1.PodRunning, executorRunning, true),
			removalPod(103, corev1.PodFailed, executorRunning, true),
			removalPodGone(105, corev1.PodFailed, executorRunning, true),
		}), "no stored state of the Pod named the Job and read its executor stopped"},
		"released by a new epoch under the same holder": {applyRemovalHistories{
			Pods: backgroundPodHistory(),
			Leases: []watchEvent[*coordinationv1.Lease]{func() watchEvent[*coordinationv1.Lease] {
				event := removalLease(104, removalHolder)
				event.Object.Annotations[annotationLeaseEpoch] = "epoch-2"
				return event
			}()},
			Migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(109, false, false)},
		}, "realm Lease let go at resourceVersion 104, before"},
		// The release the hold exists to prevent: the manager let go on
		// the first pass after the Pod went. The Pod read stopped before
		// that, as every background deletion leaves it, which is why the
		// stopped state alone does not decide the branch.
		"released on the first write after the Pod went": {applyRemovalHistories{
			Pods:       backgroundPodHistory(),
			Leases:     []watchEvent[*coordinationv1.Lease]{removalLease(102, removalHolder), removalLease(112, "")},
			Migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(111, true, true)},
		}, "the PtahMigration let go at resourceVersion 111, after the Pod went at 110, with no write that held the realm Lease in between"},
		"the Lease released after the Pod went and the migration still holding": {applyRemovalHistories{
			Pods:       backgroundPodHistory(),
			Leases:     []watchEvent[*coordinationv1.Lease]{removalLease(112, "")},
			Migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(104, true, false), removalMigration(113, false, false)},
		}, "the realm Lease let go at resourceVersion 112, after the Pod went at 110, with no write"},
		// A watch delivers a deletion with the last state stored, which
		// still names the holder.
		"the Lease deleted after the Pod went": {applyRemovalHistories{
			Pods: backgroundPodHistory(),
			Leases: []watchEvent[*coordinationv1.Lease]{removalLease(102, removalHolder), func() watchEvent[*coordinationv1.Lease] {
				event := removalLease(112, removalHolder)
				event.Type = watch.Deleted
				return event
			}()},
			Migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(104, true, false), removalMigration(113, false, false)},
		}, "the realm Lease let go at resourceVersion 112, after the Pod went at 110, with no write"},
		"let go after the Pod went and renewed after that": {applyRemovalHistories{
			Pods:       backgroundPodHistory(),
			Leases:     []watchEvent[*coordinationv1.Lease]{removalLease(112, removalHolder)},
			Migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(111, true, true)},
		}, "the PtahMigration let go at resourceVersion 111, after the Pod went at 110, and the realm Lease was written still held at 112"},
		"a second Pod under the Job": {applyRemovalHistories{
			Pods: append(backgroundPodHistory(), func() watchEvent[*corev1.Pod] {
				event := removalPod(111, corev1.PodPending, corev1.ContainerState{}, true)
				event.Object.UID, event.Type = "0b8e6f0e-1d1c-4b79-9a31-9a7f0f1a0009", watch.Added
				return event
			}()),
			Leases: []watchEvent[*coordinationv1.Lease]{removalLease(112, removalHolder)},
		}, "names the Job as its controller"},
		"a Pod history that never named the Job": {applyRemovalHistories{
			Pods: []watchEvent[*corev1.Pod]{
				removalPod(101, corev1.PodRunning, executorRunning, false),
				removalPodGone(103, corev1.PodFailed, executorStops, false),
			},
			Leases: []watchEvent[*coordinationv1.Lease]{removalLease(112, removalHolder)},
		}, "no state of the Pod in the history names the Job"},
		"a resourceVersion that is not a number": {applyRemovalHistories{
			Pods: backgroundPodHistory(),
			Leases: []watchEvent[*coordinationv1.Lease]{func() watchEvent[*coordinationv1.Lease] {
				event := removalLease(112, removalHolder)
				event.Object.ResourceVersion = "r112"
				return event
			}()},
		}, "compare resourceVersions"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			verdict, err := readApplyRemoval(row.histories, removalSubject())
			if err == nil || errors.Is(err, errApplyRemovalPending) || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("readApplyRemoval() = %+v, %v; want a refusal that says %q", verdict, err, row.want)
			}
		})
	}
}

func TestReadApplyRemovalWaitsForWhatDecidesIt(t *testing.T) {
	t.Parallel()

	pods := backgroundPodHistory()
	for name, histories := range map[string]applyRemovalHistories{
		"the Pod not gone yet": {Pods: pods[:len(pods)-1], Leases: []watchEvent[*coordinationv1.Lease]{removalLease(112, removalHolder)}},
		// A release while the Pod stands is decided against the removal, so
		// it waits for it.
		"the Pod not gone after a release": {Pods: pods[:len(pods)-1],
			Leases:     []watchEvent[*coordinationv1.Lease]{removalLease(107, "")},
			Migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(106, true, true)}},
		"no Lease write since the Pod went": {Pods: pods, Leases: []watchEvent[*coordinationv1.Lease]{removalLease(102, removalHolder)}},
		"a release the migration's history has not reached": {Pods: pods,
			Leases:     []watchEvent[*coordinationv1.Lease]{removalLease(106, "")},
			Migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(104, true, false)}},
		"a staged release the Lease's history has not reached": {Pods: pods,
			Leases:     []watchEvent[*coordinationv1.Lease]{removalLease(102, removalHolder)},
			Migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(106, true, true)}},
		// The Lease is renewed after the Pod went, but the migration staged
		// a release before it, which decides the other branch once the
		// Lease's history shows it.
		"a staged release before a renewal the Lease's history holds": {Pods: pods,
			Leases:     []watchEvent[*coordinationv1.Lease]{removalLease(112, removalHolder)},
			Migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(106, true, true)}},
		"a release after the Pod went the migration's history has not reached": {Pods: pods,
			Leases:     []watchEvent[*coordinationv1.Lease]{removalLease(112, "")},
			Migrations: []watchEvent[*ptahv1alpha1.PtahMigration]{removalMigration(104, true, false)}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if verdict, err := readApplyRemoval(histories, removalSubject()); !errors.Is(err, errApplyRemovalPending) {
				t.Fatalf("readApplyRemoval() = %+v, %v; want it to wait", verdict, err)
			}
		})
	}
}

func TestSingleEtcdForEveryResourceRefusesAnotherStore(t *testing.T) {
	t.Parallel()

	apiServer := func(arguments ...string) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "kube-apiserver-control-plane"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "kube-apiserver",
				Command: append([]string{"kube-apiserver", "--etcd-servers=https://127.0.0.1:2379"}, arguments...)}}}}
	}
	if err := singleEtcdForEveryResource([]corev1.Pod{apiServer(), apiServer("--secure-port=6443")}); err != nil {
		t.Fatalf("singleEtcdForEveryResource() refused one etcd: %v", err)
	}
	other := apiServer()
	other.Spec.Containers[0].Name = "sidecar"
	for name, pods := range map[string][]corev1.Pod{
		"no kube-apiserver Pod":                  nil,
		"an override on one of them":             {apiServer(), apiServer("--etcd-servers-overrides=/events#https://127.0.0.1:2381")},
		"a Pod with no kube-apiserver container": {other},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := singleEtcdForEveryResource(pods); err == nil {
				t.Fatal("singleEtcdForEveryResource() accepted it")
			}
		})
	}
}

// TestReadApplyRemovalDecidesTheKindHistory reads the history a kind 1.37.0
// run recorded for the alert row's Apply Job deleted in the background, as
// the row printed it, and the same history with the manager letting go at a
// revision of each kind. testdata/e2e/readings/README.md says where it came
// from.
func TestReadApplyRemovalDecidesTheKindHistory(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("../../testdata/e2e/readings/apply-removal-held.json")
	if err != nil {
		t.Fatal(err)
	}
	decode := func(t *testing.T) applyRemovalReading {
		t.Helper()
		var reading applyRemovalReading
		if err := json.Unmarshal(body, &reading); err != nil {
			t.Fatal(err)
		}
		return reading
	}
	reading := decode(t)
	verdict, err := readApplyRemoval(reading.Histories, reading.Subject)
	if err != nil || verdict != reading.Verdict {
		t.Fatalf("readApplyRemoval() = %+v, %v; the row decided %+v", verdict, err, reading.Verdict)
	}
	if verdict.Branch != applyHeldToTerm || verdict.Stopped != "2973" || verdict.Gone != "2997" || verdict.Settled != "3026" {
		t.Fatalf("readApplyRemoval() = %+v, want the hold settled at 3026 after a stop at 2973 and the removal at 2997", verdict)
	}

	// letGo is the recorded history with the manager's release at rv: the
	// migration staged it, the Lease lost its holder at the next revision,
	// and no renewal followed.
	letGo := func(t *testing.T, rv int) applyRemovalReading {
		t.Helper()
		reading := decode(t)
		h := &reading.Histories
		var leases []watchEvent[*coordinationv1.Lease]
		for _, event := range h.Leases {
			if at, _ := strconv.Atoi(event.Object.ResourceVersion); at < rv {
				leases = append(leases, event)
			}
		}
		released := leases[len(leases)-1].Object.DeepCopy()
		released.ResourceVersion, released.Spec.HolderIdentity = strconv.Itoa(rv+1), ptr.To("")
		h.Leases = append(leases, watchEvent[*coordinationv1.Lease]{Type: watch.Modified, Object: released})
		staged := h.Migrations[len(h.Migrations)-1].Object.DeepCopy()
		staged.ResourceVersion, staged.Status.ActiveOperation = strconv.Itoa(rv), nil
		staged.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{LeaseEpoch: reading.Subject.Epoch}
		h.Migrations = append(h.Migrations, watchEvent[*ptahv1alpha1.PtahMigration]{Type: watch.Modified, Object: staged})
		return reading
	}
	t.Run("a release while the Pod read stopped", func(t *testing.T) {
		t.Parallel()
		reading := letGo(t, 2980)
		verdict, err := readApplyRemoval(reading.Histories, reading.Subject)
		if err != nil || verdict.Branch != applyReleasedOnStop || verdict.Released != "2980" || verdict.ReleasedBy != "PtahMigration" {
			t.Fatalf("readApplyRemoval() = %+v, %v; want the release branch at 2980", verdict, err)
		}
	})
	for name, row := range map[string]struct {
		rv   int
		want string
	}{
		"a release while the Pod still ran": {2965, "the PtahMigration let go at resourceVersion 2965, before the Pod was stored with its executor stopped under the Job at 2973"},
		"a release once the Pod was gone":   {3010, "the PtahMigration let go at resourceVersion 3010, after the Pod went at 2997, with no write that held the realm Lease in between"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reading := letGo(t, row.rv)
			verdict, err := readApplyRemoval(reading.Histories, reading.Subject)
			if err == nil || errors.Is(err, errApplyRemovalPending) || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("readApplyRemoval() = %+v, %v; want a refusal that says %q", verdict, err, row.want)
			}
		})
	}
}

// TestReadApplyRemovalAcceptsTheKindRelease reads the history the same run
// recorded for the row's MySQL lane, where a pass read the stop the kubelet
// stored and let go before the Pod went, and copies with that release moved
// before the stop and after the removal.
func TestReadApplyRemovalAcceptsTheKindRelease(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("../../testdata/e2e/readings/apply-removal-released.json")
	if err != nil {
		t.Fatal(err)
	}
	decode := func(t *testing.T) applyRemovalReading {
		t.Helper()
		var reading applyRemovalReading
		if err := json.Unmarshal(body, &reading); err != nil {
			t.Fatal(err)
		}
		return reading
	}
	reading := decode(t)
	verdict, err := readApplyRemoval(reading.Histories, reading.Subject)
	if err != nil || verdict != reading.Verdict {
		t.Fatalf("readApplyRemoval() = %+v, %v; the row decided %+v", verdict, err, reading.Verdict)
	}
	if verdict.Branch != applyReleasedOnStop || verdict.Stopped != "6179" || verdict.Released != "6180" || verdict.ReleasedBy != "PtahMigration" || verdict.Gone != "6188" {
		t.Fatalf("readApplyRemoval() = %+v, want the release by the PtahMigration at 6180 after the stop at 6179 and before the removal at 6188", verdict)
	}

	// moved is the recorded history with the writes at the revisions in to
	// rewritten to the revisions they map to, each history still in order.
	moved := func(t *testing.T, to map[string]int) applyRemovalReading {
		t.Helper()
		reading := decode(t)
		for _, event := range reading.Histories.Migrations {
			if rv, ok := to[event.Object.ResourceVersion]; ok {
				event.Object.ResourceVersion = strconv.Itoa(rv)
			}
		}
		for _, event := range reading.Histories.Leases {
			if rv, ok := to[event.Object.ResourceVersion]; ok {
				event.Object.ResourceVersion = strconv.Itoa(rv)
			}
		}
		return reading
	}
	for name, row := range map[string]struct {
		to   map[string]int
		want string
	}{
		// The staging write before the kubelet's, the Lease's after it.
		"before the stop": {map[string]int{"6180": 6178}, "the PtahMigration let go at resourceVersion 6178, before the Pod was stored with its executor stopped under the Job at 6179"},
		// Every write from the release on lands after the removal.
		"after the Pod's removal": {map[string]int{"6180": 6189, "6181": 6190, "6182": 6191, "6183": 6192},
			"the PtahMigration let go at resourceVersion 6189, after the Pod went at 6188, with no write that held the realm Lease in between"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reading := moved(t, row.to)
			verdict, err := readApplyRemoval(reading.Histories, reading.Subject)
			if err == nil || errors.Is(err, errApplyRemovalPending) || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("readApplyRemoval() = %+v, %v; want a refusal that says %q", verdict, err, row.want)
			}
		})
	}
}
