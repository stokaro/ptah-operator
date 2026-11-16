//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// removeApplyWorkloadUnseen takes a running Apply's Job and its Pod away so
// that nothing the API server stores says the Job's executor stopped. That is
// the premise of every row that holds a claim to its Lease term
// (ApplyPodGone).
//
// Deleting the Job with its Pods cannot give that premise. The kubelet stores
// the Pod Failed, its executor terminated, before it lets the Pod go, and the
// Pod still names the Job when it does. A manager pass that reads that state
// hands the realm Lease back at once, as the operations page says it should.
// Whether a pass lands in that second or so is timing, so a row built on it
// passes most runs and fails some against an operator keeping its contract.
//
// So the Job goes first, with its Pod orphaned: the garbage collector takes
// the Job's reference off the Pod before it lets the Job go, and a Job being
// deleted creates no Pod. The Pod, no longer the Job's, is deleted next. The
// Pod history from the read before the Job's deletion to the Pod's removal is
// held to applyPodGoneUnseen, so a run whose stimulus slipped fails here and
// names the state, rather than in a hold that blames the operator.
func (m *migrationRun) removeApplyWorkloadUnseen(jobName string, jobUID types.UID, what string) {
	m.t.Helper()
	job := &batchv1.Job{}
	m.check(m.get(jobName, job), "read %s before removing it", what)
	if jobUID == "" || job.UID != jobUID {
		m.fatalf("%s under the name %s has UID %s, not %s", what, jobName, job.UID, jobUID)
	}
	pods := &corev1.PodList{}
	m.check(m.list(pods), "read the Pods before removing %s", what)
	owned := ownedPods(pods.Items, jobUID)
	if len(owned) != 1 || owned[0].Status.Phase != corev1.PodRunning || owned[0].DeletionTimestamp != nil {
		m.fatalf("%s does not own exactly one running Pod before its removal: it owns %d", what, len(owned))
	}
	pod := owned[0].DeepCopy()

	watcher, err := client.NewWithWatch(m.cluster.Config, client.Options{Scheme: m.cluster.Scheme})
	m.check(err, "build a client to watch the Pods of %s", what)
	ctx, cancel := context.WithCancel(m.ctx)
	defer cancel()
	type recorded struct {
		events []watchEvent[*corev1.Pod]
		err    error
	}
	done := make(chan recorded, 1)
	go func() {
		events, err := podHistoryToRemoval(ctx, watcher, m.in.TestNamespace, pods.ResourceVersion, pod.UID)
		done <- recorded{events, err}
	}()

	m.check(m.cluster.Client.Delete(m.ctx, job, client.Preconditions{UID: &jobUID},
		client.PropagationPolicy(metav1.DeletePropagationOrphan)), "remove %s with its Pod orphaned", what)
	m.poll(what+" to be removed", time.Second, func() bool {
		current := &batchv1.Job{}
		err := m.get(jobName, current)
		return apierrors.IsNotFound(err) || (err == nil && current.UID != jobUID)
	})
	m.poll("the Pod "+pod.Name+" to stop naming "+what+" as its controller", time.Second, func() bool {
		current := &corev1.Pod{}
		err := m.get(pod.Name, current)
		if apierrors.IsNotFound(err) || (err == nil && current.UID != pod.UID) {
			m.fatalf("the Pod %s went while it still named %s as its controller", pod.Name, what)
		}
		return err == nil && !controlledByJob(current.OwnerReferences, jobUID)
	})
	podUID := pod.UID
	m.check(m.cluster.Client.Delete(m.ctx, pod, client.Preconditions{UID: &podUID}), "remove the Pod %s once %s was gone", pod.Name, what)

	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()
	var history recorded
	select {
	case history = <-done:
	case <-timer.C:
		m.fatalf("the Pod %s was not removed within %s of its deletion", pod.Name, waitTimeout)
	case <-m.ctx.Done():
		m.fatalf("the phase ended before the Pod %s was removed: %v", pod.Name, m.ctx.Err())
	}
	m.check(history.err, "record the Pod history of %s to the Pod's removal", what)
	events := append([]watchEvent[*corev1.Pod]{{Type: watch.Added, Object: pod}}, history.events...)
	if err := applyPodGoneUnseen(events, pod.UID, jobUID); err != nil {
		m.fatalf("the removal of %s left a state that reads its Pod %s stopped: %v", what, pod.Name, err)
	}
	m.logf("removed %s and then its Pod %s, %d Pod events with none reading it stopped under the Job", what, pod.Name, len(events))
}

// podHistoryToRemoval is every Pod event of the namespace after
// resourceVersion, up to and including the removal of the Pod with podUID. A
// watch the API server ends is resumed from the last event read; one it can no
// longer serve from that version comes back as an error.
func podHistoryToRemoval(ctx context.Context, watcher client.WithWatch, namespace, resourceVersion string, podUID types.UID) ([]watchEvent[*corev1.Pod], error) {
	var events []watchEvent[*corev1.Pod]
	for {
		stream, err := watcher.Watch(ctx, &corev1.PodList{}, &client.ListOptions{
			Namespace: namespace,
			Raw:       &metav1.ListOptions{ResourceVersion: resourceVersion},
		})
		if err != nil {
			return events, fmt.Errorf("watch the Pods of %s from resourceVersion %s: %w", namespace, resourceVersion, err)
		}
		read := 0
		for event := range stream.ResultChan() {
			read++
			if event.Type == watch.Error {
				stream.Stop()
				return events, fmt.Errorf("the Pod watch from resourceVersion %s failed: %w", resourceVersion, apierrors.FromObject(event.Object))
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok || pod.ResourceVersion == "" {
				stream.Stop()
				return events, fmt.Errorf("the Pod watch delivered an invalid %T event", event.Object)
			}
			events = append(events, watchEvent[*corev1.Pod]{Type: event.Type, Object: pod})
			resourceVersion = pod.ResourceVersion
			if event.Type == watch.Deleted && pod.UID == podUID {
				stream.Stop()
				return events, nil
			}
		}
		stream.Stop()
		if read == 0 {
			// A stream that ended with nothing read is not resumed at once,
			// so a server that keeps ending it costs a request a second.
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
		if ctx.Err() != nil {
			return events, errors.Join(errors.New("the Pod watch stopped before the Pod's removal"), ctx.Err())
		}
	}
}
