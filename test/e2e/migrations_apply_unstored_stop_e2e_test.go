//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// unstoredStopProbe is the status write that shows whether the API server
// admits status writes to a Pod. It is sent as a dry run, so it stores
// nothing either way.
var unstoredStopProbe = []byte(`{"status":{"conditions":[{"type":"e2e.ptah.run/StatusProbe","status":"True"}]}}`)

// removeApplyWithItsStopUnstored removes a running Apply's Job and its one Pod
// so that no stored state of the Pod ever says its executor stopped
// (migrations_apply_unstored_stop.go says why it takes this): it refuses
// status writes to the Pod, deletes the Job with its Pods, deletes the Pod
// with no grace once the Job is gone, and lifts the refusal once the Pod is
// gone. A watch of the Pod's namespace, begun before any of it, has to show
// the Pod's deletion and no state of it that says it stopped. job is the Job
// as read, with its UID.
func (m *migrationRun) removeApplyWithItsStopUnstored(job *batchv1.Job, what string) {
	m.t.Helper()
	if job == nil || job.UID == "" {
		m.fatalf("%s has no UID to remove it by", what)
	}
	watcher, err := newRecorderClient(m.cluster.Config, m.cluster.Scheme)
	m.check(err, "build a client to watch the Pod of %s", what)
	recorded := newStoredStateRecorder[*corev1.Pod](m.t, m.ctx, watcher, "unstored-stop Pod", job.Namespace,
		func() client.ObjectList { return &corev1.PodList{} })
	defer recorded.abort()
	listed := &corev1.PodList{}
	m.check(m.cluster.Client.List(m.ctx, listed, client.InNamespace(job.Namespace)), "read the Pods of %s before removing it", what)
	owned := ownedPods(listed.Items, job.UID)
	if len(owned) != 1 || owned[0].Status.Phase != corev1.PodRunning || owned[0].DeletionTimestamp != nil || storedStop(&owned[0]) != "" {
		m.fatalf("%s does not own exactly one running Pod whose executor runs before its removal: it owns %d", what, len(owned))
	}
	pod := owned[0].DeepCopy()
	probe := func(ctx context.Context) error {
		return m.cluster.Client.Status().Patch(ctx, pod.DeepCopy(), client.RawPatch(types.StrategicMergePatchType, unstoredStopProbe), client.DryRunAll)
	}
	// Without the policy the probe is admitted, so the refusal below is the
	// policy's and nothing else's.
	m.check(probe(m.ctx), "admit a status write to Pod %s before its stop is kept out", pod.Name)

	policy, binding := unstoredStopPolicy(pod)
	var created []client.Object
	lift := func(ctx context.Context) error {
		for len(created) > 0 {
			object := created[len(created)-1]
			uid := object.GetUID()
			if err := m.cluster.Client.Delete(ctx, object, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete %s: %w", object.GetName(), err)
			}
			created = created[:len(created)-1]
		}
		return nil
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := lift(ctx); err != nil {
			m.t.Errorf("lift the refusal of status writes to Pod %s: %v", pod.Name, err)
		}
	}()
	for _, object := range []client.Object{policy, binding} {
		m.check(m.cluster.Client.Create(m.ctx, object), "install %s", object.GetName())
		created = append(created, object)
	}
	m.check(harness.Wait(m.ctx, "the refusal of status writes to Pod "+pod.Name, 30*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			err := probe(ctx)
			if err != nil && !unstoredStopDenied(err, policy.Name) {
				return false, "", err
			}
			return err != nil, "the policy " + policy.Name + " does not refuse the probe yet", nil
		}), "keep the stop of %s out of the API server", what)

	jobUID := job.UID
	m.check(m.cluster.Client.Delete(m.ctx, job, client.Preconditions{UID: &jobUID}, client.PropagationPolicy(metav1.DeletePropagationBackground)),
		"delete %s with its Pods", what)
	m.poll(what+" to be removed", time.Second, func() bool {
		current := &batchv1.Job{}
		err := m.cluster.Client.Get(m.ctx, client.ObjectKeyFromObject(job), current)
		return apierrors.IsNotFound(err) || err == nil && current.UID != jobUID
	})
	// The Job controller drops the tracking finalizer of a deleted Job's Pods,
	// so the Pod goes with this delete; until then it stands Terminating.
	podUID := pod.UID
	if err := m.cluster.Client.Delete(m.ctx, pod, client.Preconditions{UID: &podUID}, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
		m.fatalf("Pod %s of %s was not deleted with no grace: %v", pod.Name, what, err)
	}
	var history []watchEvent[*corev1.Pod]
	m.check(harness.Wait(m.ctx, "the watch to record the deletion of Pod "+pod.Name, 60*time.Second, time.Second,
		func(context.Context) (bool, string, error) {
			if err := recorded.alive(); err != nil {
				return false, "", err
			}
			history = recorded.snapshot()
			gone := slices.ContainsFunc(history, func(event watchEvent[*corev1.Pod]) bool {
				return event.Type == watch.Deleted && event.Object != nil && event.Object.UID == podUID
			})
			return gone, "waiting for the Pod's DELETED event", nil
		}), "remove Pod %s of %s", pod.Name, what)
	states, err := unstoredStopHeld(history, podUID)
	if err != nil {
		m.fatalf("the removal of %s left a stored stop, so the hold it exists for cannot be read: %v; the Pod read %s",
			what, err, unstoredStopStates(history, podUID))
	}
	m.check(lift(m.ctx), "lift the refusal of status writes to Pod %s", pod.Name)
	m.logf("Pod %s of %s went with %d stored states, none of them stopped: %s", pod.Name, what, states, unstoredStopStates(history, podUID))
}
