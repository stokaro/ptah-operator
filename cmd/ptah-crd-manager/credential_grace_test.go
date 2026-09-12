package main

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

func TestProtectedRuntimePodStabilityObserverUsesGapFreeListWatch(t *testing.T) {
	t.Parallel()

	rollout := admissionConvergenceTestRollout()
	firstWatch := watch.NewFakeWithChanSize(4, false)
	secondWatch := watch.NewFakeWithChanSize(4, false)
	snapshots := &scriptedProtectedRuntimePodSnapshotter{snapshots: []crdupgrade.ProtectedRuntimePodSnapshot{
		{ResourceVersion: "10"},
		{ResourceVersion: "11"},
	}}
	watcher := &recordingProtectedRuntimePodWatcher{watches: []watch.Interface{firstWatch, secondWatch}}
	observer, err := newProtectedRuntimePodStabilityObserver(snapshots, watcher, rollout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(observer.Close)

	identity, proven, err := observer.Observe(context.Background(), "topology")
	if err != nil || !proven || identity == "" {
		t.Fatalf("initial Observe() = %q, %t, %v", identity, proven, err)
	}
	if got, want := watcher.options, []metav1.ListOptions{{ResourceVersion: "10", AllowWatchBookmarks: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("watch options = %#v, want %#v", got, want)
	}

	firstWatch.Add(runtimePodForStability(rollout.ReleaseNamespace, "other", "other"))
	sameIdentity, proven, err := observer.Observe(context.Background(), "topology")
	if err != nil || !proven || sameIdentity != identity {
		t.Fatalf("unprotected event Observe() = %q, %t, %v, want unchanged identity %q", sameIdentity, proven, err, identity)
	}

	firstWatch.Add(runtimePodForStability(
		rollout.ReleaseNamespace,
		"controller-reappeared",
		rollout.ControllerServiceAccountName,
	))
	changedIdentity, proven, err := observer.Observe(context.Background(), "topology")
	if err != nil || !proven || changedIdentity == identity {
		t.Fatalf("protected event Observe() = %q, %t, %v, want a new proven identity", changedIdentity, proven, err)
	}
	if got, want := watcher.options, []metav1.ListOptions{
		{ResourceVersion: "10", AllowWatchBookmarks: true},
		{ResourceVersion: "11", AllowWatchBookmarks: true},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("watch options after restart = %#v, want %#v", got, want)
	}
}

// The last event a watch delivers can precede the Pod actually going away.
// An observer that only refreshes on an event would then hold "Pods remain"
// with nothing left to correct it, and the credential fence would wait out its
// deadline for a condition that had already been met.
func TestProtectedRuntimePodObserverRereadsWhileUnproven(t *testing.T) {
	t.Parallel()

	rollout := admissionConvergenceTestRollout()
	snapshots := &scriptedProtectedRuntimePodSnapshotter{snapshots: []crdupgrade.ProtectedRuntimePodSnapshot{
		{ResourceVersion: "10", PodsRemain: true},
		{ResourceVersion: "11", PodsRemain: true},
		{ResourceVersion: "12"},
	}}
	watcher := &recordingProtectedRuntimePodWatcher{watches: []watch.Interface{
		watch.NewFakeWithChanSize(4, false),
		watch.NewFakeWithChanSize(4, false),
		watch.NewFakeWithChanSize(4, false),
	}}
	observer, err := newProtectedRuntimePodStabilityObserver(snapshots, watcher, rollout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(observer.Close)

	if _, proven, err := observer.Observe(context.Background(), "topology"); err != nil || proven {
		t.Fatalf("initial Observe() proven = %t, err = %v, want an unproven observation", proven, err)
	}
	// No event arrives. The observation is still unproven, so the snapshot is
	// read again rather than held.
	if _, proven, err := observer.Observe(context.Background(), "topology"); err != nil || proven {
		t.Fatalf("second Observe() proven = %t, err = %v, want an unproven observation", proven, err)
	}
	identity, proven, err := observer.Observe(context.Background(), "topology")
	if err != nil || !proven || identity == "" {
		t.Fatalf("Observe() after the Pods went away = %q, %t, %v, want a proven observation", identity, proven, err)
	}
	if got := len(watcher.options); got != 3 {
		t.Fatalf("watch restarts = %d, want one per unproven sweep", got)
	}
	// Proven observations are carried by the watch alone, so the identity holds
	// still and the stability window can elapse.
	held, proven, err := observer.Observe(context.Background(), "topology")
	if err != nil || !proven || held != identity {
		t.Fatalf("Observe() after proving = %q, %t, %v, want the identity held at %q", held, proven, err, identity)
	}
}

func TestProtectedRuntimePodObserverRejectsUnsafeSnapshotsAndEvents(t *testing.T) {
	t.Parallel()

	rollout := admissionConvergenceTestRollout()
	tests := []struct {
		name      string
		snapshot  crdupgrade.ProtectedRuntimePodSnapshot
		watch     watch.Interface
		event     func(*watch.FakeWatcher)
		wantError string
	}{
		{name: "missing resourceVersion", watch: watch.NewFake(), wantError: "resourceVersion"},
		{
			name: "nil watch", snapshot: crdupgrade.ProtectedRuntimePodSnapshot{ResourceVersion: "1"},
			wantError: "returned nil",
		},
		{
			name: "nil result channel", snapshot: crdupgrade.ProtectedRuntimePodSnapshot{ResourceVersion: "1"},
			watch: nilResultChannelWatch{}, wantError: "nil result channel",
		},
		{
			name: "foreign namespace event", snapshot: crdupgrade.ProtectedRuntimePodSnapshot{ResourceVersion: "1"},
			watch: watch.NewFakeWithChanSize(1, false),
			event: func(w *watch.FakeWatcher) {
				w.Add(runtimePodForStability("foreign", "pod", rollout.ControllerServiceAccountName))
			},
			wantError: "malformed Pod",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshots := &scriptedProtectedRuntimePodSnapshotter{snapshots: []crdupgrade.ProtectedRuntimePodSnapshot{test.snapshot}}
			watcher := &recordingProtectedRuntimePodWatcher{}
			if test.watch != nil {
				watcher.watches = []watch.Interface{test.watch}
			}
			observer, err := newProtectedRuntimePodStabilityObserver(snapshots, watcher, rollout)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(observer.Close)
			if test.event != nil {
				fakeWatch := test.watch.(*watch.FakeWatcher)
				// Start the LIST->WATCH edge before submitting the event.
				if _, _, err := observer.Observe(context.Background(), "topology"); err != nil {
					t.Fatal(err)
				}
				test.event(fakeWatch)
			}
			_, _, err = observer.Observe(context.Background(), "topology")
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Observe() error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func TestContinuousCredentialWindowResetsAtSecond64OnSaturatedPodWatch(t *testing.T) {
	t.Parallel()

	rollout := admissionConvergenceTestRollout()
	firstWatch := watch.NewFakeWithChanSize(protectedRuntimePodWatchDrainLimit+2, false)
	secondWatch := watch.NewFakeWithChanSize(1, false)
	snapshots := &scriptedProtectedRuntimePodSnapshotter{snapshots: []crdupgrade.ProtectedRuntimePodSnapshot{
		{ResourceVersion: "10"},
		{ResourceVersion: "11"},
	}}
	watcher := &recordingProtectedRuntimePodWatcher{watches: []watch.Interface{firstWatch, secondWatch}}
	observer, err := newProtectedRuntimePodStabilityObserver(snapshots, watcher, rollout)
	if err != nil {
		t.Fatal(err)
	}
	clock := newAdmissionBarrierClock()
	barrier := testAdmissionBarrier(clock, admissionBarrierEndpoints("topology", map[string]*scriptedAdmissionProbe{
		"https://10.0.0.1:6443": {results: []bool{true}},
	}), nil)
	barrier.sleep = func(ctx context.Context, duration time.Duration) error {
		if err := clock.sleep(ctx, duration); err != nil {
			return err
		}
		if clock.now().Sub(clock.start) == 64*time.Second {
			for index := range protectedRuntimePodWatchDrainLimit {
				firstWatch.Add(runtimePodForStability(rollout.ReleaseNamespace, "other-"+strconv.Itoa(index), "other"))
			}
			// The protected event immediately after the bounded batch must not be
			// hidden behind an unchanged proven observer identity.
			firstWatch.Add(runtimePodForStability(
				rollout.ReleaseNamespace,
				"controller-at-expiry",
				rollout.ControllerServiceAccountName,
			))
		}
		return nil
	}
	if err := barrier.WaitWithStabilityObserver(context.Background(), retiredCredentialRevocationDelay, observer); err != nil {
		t.Fatal(err)
	}
	if got, want := clock.now().Sub(clock.start), 129*time.Second; got != want {
		t.Fatalf("joint credential window elapsed = %s, want %s after t=64s Pod-watch reset", got, want)
	}
}

func admissionConvergenceTestRollout() *crdupgrade.RolloutGuard {
	return &crdupgrade.RolloutGuard{
		ReleaseNamespace:                     "operators",
		ControllerServiceAccountName:         "ptah-controller-v2",
		PreviousControllerServiceAccountName: "ptah-controller-v1",
		CertificateDeploymentName:            "ptah-certificate",
	}
}

func runtimePodForStability(namespace, name, serviceAccount string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(name + "-uid")},
		Spec:       corev1.PodSpec{ServiceAccountName: serviceAccount},
	}
}

type scriptedProtectedRuntimePodSnapshotter struct {
	snapshots []crdupgrade.ProtectedRuntimePodSnapshot
	errors    []error
	calls     int
}

func (s *scriptedProtectedRuntimePodSnapshotter) ProtectedRuntimePodSnapshot(context.Context) (crdupgrade.ProtectedRuntimePodSnapshot, error) {
	index := s.calls
	s.calls++
	if len(s.errors) != 0 {
		if err := s.errors[min(index, len(s.errors)-1)]; err != nil {
			return crdupgrade.ProtectedRuntimePodSnapshot{}, err
		}
	}
	if len(s.snapshots) == 0 {
		return crdupgrade.ProtectedRuntimePodSnapshot{}, errors.New("unexpected snapshot")
	}
	return s.snapshots[min(index, len(s.snapshots)-1)], nil
}

type recordingProtectedRuntimePodWatcher struct {
	watches []watch.Interface
	errors  []error
	options []metav1.ListOptions
	calls   int
}

func (w *recordingProtectedRuntimePodWatcher) Watch(_ context.Context, options metav1.ListOptions) (watch.Interface, error) {
	index := w.calls
	w.calls++
	w.options = append(w.options, options)
	if len(w.errors) != 0 {
		if err := w.errors[min(index, len(w.errors)-1)]; err != nil {
			return nil, err
		}
	}
	if len(w.watches) == 0 {
		return nil, nil
	}
	return w.watches[min(index, len(w.watches)-1)], nil
}

type nilResultChannelWatch struct{}

func (nilResultChannelWatch) Stop()                          {}
func (nilResultChannelWatch) ResultChan() <-chan watch.Event { return nil }
