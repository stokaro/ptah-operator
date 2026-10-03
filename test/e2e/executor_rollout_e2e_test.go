//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// Both resource families keep every old manager's log through Recreate and
// require all Pod identities to change before the new executor can proceed.
func rolloutExecutorManagers(t *testing.T, ctx context.Context, cluster *harness.Cluster, key types.NamespacedName,
	expected, replacement string, scan func([]byte, string),
) {
	rolloutExecutionManagers(t, ctx, cluster, key, executionComponentChange{"executor-image", expected, replacement}, scan)
}

func rolloutExecutionManagers(t *testing.T, ctx context.Context, cluster *harness.Cluster, key types.NamespacedName,
	change executionComponentChange, scan func([]byte, string),
) {
	t.Helper()
	deployment := &appsv1.Deployment{}
	if err := cluster.Client.Get(ctx, key, deployment); err != nil {
		t.Fatal("read executor rollout Deployment:", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil || deployment.Spec.Selector == nil || selector.Empty() {
		t.Fatal("executor rollout has no exact manager selector")
	}
	want := int(ptr.Deref(deployment.Spec.Replicas, 1))
	readReady := func() ([]corev1.Pod, []string) {
		var pods []corev1.Pod
		var uids []string
		err := harness.Wait(ctx, "every executor rollout manager Pod to be ready", 3*time.Minute, time.Second,
			func(ctx context.Context) (bool, string, error) {
				list := &corev1.PodList{}
				if err := cluster.Client.List(ctx, list, client.InNamespace(key.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
					return false, "read rollout Pods", err
				}
				var ready bool
				pods = list.Items
				uids, ready = readyManagerPodUIDs(pods, want)
				return ready && want > 0 && len(pods) == want, "waiting for the exact ready replica inventory", nil
			})
		if err != nil {
			t.Fatal(err)
		}
		return pods, uids
	}
	pods, before := readReady()
	var followers []*backgroundCommand
	defer func() {
		for _, follower := range followers {
			follower.stop()
		}
	}()
	for _, pod := range pods {
		if !noRestarts(&pod) {
			reportRestartedManagerContainers(cluster, pods)
			t.Fatal("executor rollout manager restarted before its complete log audit")
		}
		followers = append(followers, startKubectlBackground(t, cluster.Kubeconfig, "-n", pod.Namespace, "logs", "-f", "pod/"+pod.Name, "--all-containers"))
	}
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
		t.Fatal("executor rollout ended before log streams started")
	}
	for index, follower := range followers {
		current := &corev1.Pod{}
		if err := cluster.Client.Get(ctx, client.ObjectKeyFromObject(&pods[index]), current); err != nil {
			t.Fatal("confirm the streamed manager Pod:", err)
		}
		if follower.exited() || current.UID != pods[index].UID || !noRestarts(current) {
			reportRestartedManagerContainers(cluster, []corev1.Pod{*current})
			t.Fatal("executor rollout lost a manager log before the destructive window")
		}
	}
	if err := setControllerExecutionComponent(ctx, cluster, key, change); err != nil {
		t.Fatal("roll out executor identity:", err)
	}
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for _, follower := range followers {
		select {
		case <-follower.done:
		case <-timer.C:
			t.Fatal("executor rollout manager log did not reach natural EOF")
		}
		if follower.err != nil {
			t.Fatal("executor rollout manager log stream failed:", follower.err)
		}
		scan(follower.output.Bytes(), "manager logs through the executor rollout")
	}
	_, after := readReady()
	if !managerPodsReplaced(before, after) {
		t.Fatal("executor rollout retained or lost a manager Pod UID")
	}
}
