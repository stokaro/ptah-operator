package webhook_test

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	podIntentWebhook = "vpodintent.operator.ptah.run"
	// jobController is the identity the Kubernetes Job controller creates Pods
	// as. envtest runs no Job controller, so the test acts as it; the API
	// server's bootstrap RBAC already grants it Pod creation.
	jobController = "system:serviceaccount:kube-system:job-controller"
)

// podFor is the Pod the Job controller creates for job: the template's
// metadata and spec, a generated name, the Job's controller reference and the
// Job tracking finalizer.
func podFor(job *batchv1.Job) *corev1.Pod {
	controller := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    job.Namespace,
			GenerateName: job.Name + "-",
			Labels:       job.Spec.Template.Labels,
			Annotations:  job.Spec.Template.Annotations,
			Finalizers:   []string{batchv1.JobTrackingFinalizer},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job",
				Name: job.Name, UID: job.UID,
				Controller: &controller, BlockOwnerDeletion: &controller,
			}},
		},
		Spec: *job.Spec.Template.Spec.DeepCopy(),
	}
}

func TestPodIntentWebhook(t *testing.T) {
	plane.Require(t)
	ctx := context.Background()
	fixture := newDispatchFixture(t, "pods")

	// The Job is created by the administrator, whom the controller-write
	// webhook does not judge, so these rows depend on nothing but the Pod
	// webhook. What the API server stores -- the generated selector and the
	// Job identity labels it adds to the template -- is what the Job
	// controller would copy into each Pod, so the Pod is built from that.
	job := fixture.job.DeepCopy()
	if err := admin.Create(ctx, job); err != nil {
		t.Fatalf("create Job %s/%s: %v", fixture.namespace, fixture.job.Name, err)
	}
	if job.Spec.Template.Labels[batchv1.ControllerUidLabel] != string(job.UID) {
		t.Fatalf("the stored Job %s/%s carries no generated identity labels: %v", job.Namespace, job.Name, job.Spec.Template.Labels)
	}
	// A Resolve is read-only, and its Job is created with its cleanup TTL, which
	// is what the first row admits a Pod of.
	if ttl := job.Spec.TTLSecondsAfterFinished; ttl == nil || *ttl != workload.CleanupTTLSeconds {
		t.Fatalf("the stored Job %s/%s carries cleanup TTL %v, want the read-only Job's %d", job.Namespace, job.Name, ttl, workload.CleanupTTLSeconds)
	}
	grant(t, fixture.namespace, "other-pods", userSubject(otherWriter),
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"create"}})

	t.Run("the Pod the Job controller creates from the admitted Job is admitted", func(t *testing.T) {
		t.Parallel()
		pod := podFor(job)
		if err := clientAs(t, jobController).Create(ctx, pod); err != nil {
			t.Fatalf("the API server refused the Job controller's Pod for %s/%s: %v", job.Namespace, job.Name, err)
		}
		// The API server's own admission plugins add to the Pod before the
		// webhook sees it -- default tolerations, the priority -- and the
		// webhook admitted the result. That is the part a unit test models
		// and this measures.
		if !hasToleration(pod.Spec.Tolerations, "node.kubernetes.io/not-ready") {
			t.Fatalf("the stored Pod %s/%s has tolerations %v; the DefaultTolerationSeconds plugin did not run, so the webhook judged an unmutated Pod",
				pod.Namespace, pod.Name, pod.Spec.Tolerations)
		}
	})

	t.Run("a Pod running another image is refused", func(t *testing.T) {
		t.Parallel()
		pod := podFor(job)
		pod.Spec.Containers[0].Image = "example.invalid/elsewhere@sha256:" + strings.Repeat("5", 64)
		err := clientAs(t, jobController).Create(ctx, pod, client.DryRunAll)
		requireDenied(t, err, podIntentWebhook, "managed Pod is outside the persisted admission envelope")
	})

	t.Run("the Job's Pod created by anyone but the Job controller is refused", func(t *testing.T) {
		t.Parallel()
		err := clientAs(t, otherWriter).Create(ctx, podFor(job), client.DryRunAll)
		requireDenied(t, err, podIntentWebhook, "managed Pod was not created by the Kubernetes Job controller")
	})

	// The webhook holds the Job to its envelope before it reads the claim, so
	// these Jobs need no claim of their own: each is the Resolve with one thing
	// changed, under a name of its own.
	stored := func(t *testing.T, suffix string, edit func(*batchv1.Job)) *batchv1.Job {
		t.Helper()
		job := fixture.job.DeepCopy()
		job.Name += "-" + suffix
		edit(job)
		if err := admin.Create(ctx, job); err != nil {
			t.Fatalf("create Job %s/%s: %v", job.Namespace, job.Name, err)
		}
		return job
	}
	t.Run("a Pod of an Apply Job created with a cleanup TTL is refused", func(t *testing.T) {
		t.Parallel()
		apply := stored(t, "apply", func(job *batchv1.Job) {
			job.Labels[workload.LabelOperation] = "apply"
			job.Spec.Template.Labels[workload.LabelOperation] = "apply"
		})
		err := clientAs(t, jobController).Create(ctx, podFor(apply), client.DryRunAll)
		requireDenied(t, err, podIntentWebhook, "Job cleanup policy must not change before Pod admission")
	})
	t.Run("a Pod of a read-only Job created with another cleanup TTL is refused", func(t *testing.T) {
		t.Parallel()
		long := stored(t, "long", func(job *batchv1.Job) { job.Spec.TTLSecondsAfterFinished = ptr.To(int32(86400)) })
		err := clientAs(t, jobController).Create(ctx, podFor(long), client.DryRunAll)
		requireDenied(t, err, podIntentWebhook, "a read-only Job's cleanup TTL is 300 seconds")
	})
}

func hasToleration(tolerations []corev1.Toleration, key string) bool {
	for _, toleration := range tolerations {
		if toleration.Key == key {
			return true
		}
	}
	return false
}
