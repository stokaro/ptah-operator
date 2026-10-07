package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

func TestMigrationExecutorWaitsForTheSQLAndPodBinding(t *testing.T) {
	t.Parallel()
	const backend = "290/10.244.3.20/288/10.244.3.20"
	running := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "apply", UID: "original"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.3.20"}}
	pending := running.DeepCopy()
	pending.Status.Phase, pending.Status.PodIP = corev1.PodPending, ""
	missingIP := running.DeepCopy()
	missingIP.Status.PodIP = ""
	reads := 0
	gotBackend, gotPod, err := waitForMigrationExecutorBackend(context.Background(), time.Second, time.Nanosecond,
		func() (string, *corev1.Pod, error) {
			reads++
			switch reads {
			case 1:
				return "", nil, nil
			case 2:
				return backend, pending, nil
			case 3:
				return backend, missingIP, nil
			case 4:
				return backend, running, nil
			default:
				t.Fatal("the matched Pod was reread")
				return "", nil, nil
			}
		})
	if err != nil || reads != 4 || gotBackend != backend || gotPod != running {
		t.Fatalf("delayed kubelet status did not reach the exact matched reading: reads=%d backend=%q pod=%v err=%v", reads, gotBackend, gotPod, err)
	}
}

func TestMigrationExecutorWaitRejectsForeignOrTerminalWorkloads(t *testing.T) {
	t.Parallel()
	const backend = "290/10.244.3.20/288/10.244.3.20"
	for name, mutate := range map[string]func(*corev1.Pod) string{
		"foreign DDL":    func(*corev1.Pod) string { return "290/10.244.3.21/288/10.244.3.20" },
		"foreign lock":   func(*corev1.Pod) string { return "290/10.244.3.20/288/10.244.3.21" },
		"ambiguous pair": func(*corev1.Pod) string { return backend + "\n" + backend },
		"missing lock":   func(*corev1.Pod) string { return "290/10.244.3.20" },
		"succeeded Pod":  func(p *corev1.Pod) string { p.Status.Phase = corev1.PodSucceeded; return backend },
		"failed Pod":     func(p *corev1.Pod) string { p.Status.Phase = corev1.PodFailed; return backend },
		"missing UID":    func(p *corev1.Pod) string { p.UID = ""; return backend },
	} {
		t.Run(name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "apply", UID: "original"},
				Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.3.20"}}
			reading := mutate(pod)
			reads := 0
			_, _, err := waitForMigrationExecutorBackend(context.Background(), time.Second, time.Nanosecond,
				func() (string, *corev1.Pod, error) { reads++; return reading, pod, nil })
			if err == nil || errors.Is(err, harness.ErrWaitTimeout) || reads != 1 {
				t.Fatalf("invalid binding should fail on its first reading: reads=%d err=%v", reads, err)
			}
		})
	}
}

func TestMigrationExecutorWaitPinsTheFirstPodUID(t *testing.T) {
	t.Parallel()
	reads := 0
	_, _, err := waitForMigrationExecutorBackend(context.Background(), time.Second, time.Nanosecond,
		func() (string, *corev1.Pod, error) {
			reads++
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "apply", UID: "original"},
				Status: corev1.PodStatus{Phase: corev1.PodPending, PodIP: "10.244.3.20"}}
			if reads > 1 {
				pod.UID, pod.Status.Phase = "replacement", corev1.PodRunning
			}
			return "290/10.244.3.20/288/10.244.3.20", pod, nil
		})
	if err == nil || !strings.Contains(err.Error(), "was replaced") || reads != 2 {
		t.Fatalf("a replacement Pod must not satisfy the wait: reads=%d err=%v", reads, err)
	}
}

func TestMigrationExecutorWaitReportsItsLastReadingAndReadErrors(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "apply", UID: "original"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, PodIP: "10.244.3.20"}}
	_, _, err := waitForMigrationExecutorBackend(context.Background(), 0, time.Second,
		func() (string, *corev1.Pod, error) { return "290/10.244.3.20/288/10.244.3.20", pod, nil })
	if !errors.Is(err, harness.ErrWaitTimeout) {
		t.Fatalf("a permanently Pending Pod must time out: %v", err)
	}
	for _, field := range []string{"290/10.244.3.20/288/10.244.3.20", "UID=original", "phase=Pending", "IP=10.244.3.20"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("timeout omitted %q: %v", field, err)
		}
	}
	readErr := errors.New("API read failed")
	_, _, err = waitForMigrationExecutorBackend(context.Background(), time.Second, time.Second,
		func() (string, *corev1.Pod, error) { return "", nil, readErr })
	if !errors.Is(err, readErr) {
		t.Fatalf("read failure was hidden: %v", err)
	}
}
