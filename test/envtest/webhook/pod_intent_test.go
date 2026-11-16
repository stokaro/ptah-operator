package webhook_test

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	// garbageCollector is the identity the garbage collector writes as. The
	// API server's bootstrap RBAC grants it patch on every resource.
	garbageCollector = "system:serviceaccount:kube-system:generic-garbage-collector"
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

// orphanPatch is the patch the garbage collector's orphanDependents sends to
// orphan pod, a dependent of job of a built-in kind, built by
// GenerateDeleteOwnerRefStrategicMergeBytes in
// pkg/controller/controller_ref_manager.go.
func orphanPatch(job *batchv1.Job, pod *corev1.Pod) client.Patch {
	return client.RawPatch(types.StrategicMergePatchType,
		fmt.Appendf(nil, `{"metadata":{"ownerReferences":[{"$patch":"delete","uid":%q}],"uid":%q}}`, job.UID, pod.UID))
}

// ownJob is a Job a person runs under name. Nothing about it is the
// operator's but, for a name such as ptah-apply-nightly, the prefix the
// webhook's match condition routes its Pods by.
func ownJob(namespace, name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: map[string]string{"app.kubernetes.io/name": "nightly"}},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "nightly"}},
				Spec: corev1.PodSpec{
					RestartPolicy:   corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name: "ptah", Image: "registry.example/ptah@sha256:" + strings.Repeat("9", 64), Args: []string{"apply"},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
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

	// A Job deleted with the orphan propagation policy is held by the orphan
	// finalizer until the garbage collector has taken the Job's owner
	// reference off every Pod, and the webhook refuses that write, so such a
	// Job stays Terminating. A Job deleted in the background is not: the
	// garbage collector deletes the Pod, which the webhook does not judge, and
	// the kubelet records the Pod stopped through pods/status, which it does
	// not judge either: the chart's webhook matches CREATE and UPDATE of pods,
	// pods/ephemeralcontainers and pods/resize. The e2e rows that delete a
	// running Apply refuse that stopped status themselves, with a policy of
	// their own, and rest on the webhook leaving pods/status alone only for
	// the dry-run status probe that shows the policy is in force.
	t.Run("the garbage collector orphaning a managed Pod is refused, its stopped status is not", func(t *testing.T) {
		t.Parallel()
		pod := podFor(job)
		if err := clientAs(t, jobController).Create(ctx, pod); err != nil {
			t.Fatalf("the API server refused the Job controller's Pod for %s/%s: %v", job.Namespace, job.Name, err)
		}
		// The patch the garbage collector's orphanDependents sends to orphan a
		// dependent of a built-in kind, built by
		// GenerateDeleteOwnerRefStrategicMergeBytes in
		// pkg/controller/controller_ref_manager.go.
		orphan := fmt.Sprintf(`{"metadata":{"ownerReferences":[{"$patch":"delete","uid":%q}],"uid":%q}}`, job.UID, pod.UID)
		err := clientAs(t, garbageCollector).Patch(ctx, pod.DeepCopy(), client.RawPatch(types.StrategicMergePatchType, []byte(orphan)), client.DryRunAll)
		requireDenied(t, err, podIntentWebhook, "managed Pod has no exact Job controller identity")

		pod.Status.Phase = corev1.PodFailed
		for _, container := range pod.Spec.Containers {
			pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
				Name: container.Name, Image: container.Image,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 143, Reason: "Error"}},
			})
		}
		err = admin.Status().Update(ctx, pod)
		requireNotDeniedBy(t, err, podIntentWebhook)
		if err != nil {
			t.Fatalf("the API server refused the stopped status of %s/%s: %v", pod.Namespace, pod.Name, err)
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

	// Whether the garbage collector may orphan a Pod that carries no operation
	// labels is the Job's to say, by the identity the Job orphan-deletion guard
	// reads: on the Job's template or on the Job. A Job only named like an
	// operation Job, which the match condition routes its Pods by, has none,
	// so its Pods are orphaned as any Job's are, and an orphaning deletion of
	// it, which the guard admits, finishes. That patch is the request this row
	// sends.
	t.Run("the garbage collector orphaning a Pod of a Job only named like an operation Job is admitted", func(t *testing.T) {
		t.Parallel()
		job := ownJob(fixture.namespace, "ptah-apply-nightly")
		if err := admin.Create(ctx, job); err != nil {
			t.Fatalf("create Job %s/%s: %v", job.Namespace, job.Name, err)
		}
		pod := podFor(job)
		if err := clientAs(t, jobController).Create(ctx, pod); err != nil {
			t.Fatalf("the API server refused the Job controller's Pod for %s/%s: %v", job.Namespace, job.Name, err)
		}
		// The name is what routes the Pod to the webhook, so it is the handler
		// that admits the patch below: a Pod that names this Job under another
		// UID is refused there.
		impostor := podFor(job)
		impostor.OwnerReferences[0].UID = "another-job-uid"
		err := clientAs(t, jobController).Create(ctx, impostor, client.DryRunAll)
		requireDenied(t, err, podIntentWebhook, "managed Pod owner does not match the current Job UID")

		orphaned := pod.DeepCopy()
		err = clientAs(t, garbageCollector).Patch(ctx, orphaned, orphanPatch(job, pod), client.DryRunAll)
		requireNotDeniedBy(t, err, podIntentWebhook)
		if err != nil {
			t.Fatalf("the API server refused the garbage collector orphaning %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		if len(orphaned.OwnerReferences) != 0 {
			t.Fatalf("the admitted patch left %s/%s with owner references %v, so it did not orphan the Pod", pod.Namespace, pod.Name, orphaned.OwnerReferences)
		}
	})

	// A Pod an operation Job made can lose its labels only while the webhook
	// does not see the write, and the webhook refuses the Job controller
	// creating one of a Job whose template carries them. So templateOnly makes
	// such a Pod the one way the webhook admits it: from a suspended Job that
	// has not started, before its template gains the labels, which the API
	// server allows on such a Job. The Job names no operator owner and carries
	// no labels of its own, like one whose PtahSchema an orphaning deletion
	// took off before the resource guard existed and whose labels a person
	// took off.
	templateOnly := func(t *testing.T, suffix string) (*batchv1.Job, *corev1.Pod) {
		t.Helper()
		job := stored(t, suffix, func(job *batchv1.Job) {
			job.OwnerReferences = nil
			job.Spec.Suspend = ptr.To(true)
			for _, labels := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
				delete(labels, workload.LabelManagedBy)
				delete(labels, workload.LabelComponent)
			}
		})
		pod := podFor(job)
		pod.Labels = maps.Clone(pod.Labels)
		if err := clientAs(t, jobController).Create(ctx, pod); err != nil {
			t.Fatalf("the API server refused the Job controller's Pod for %s/%s: %v", job.Namespace, job.Name, err)
		}
		current := &batchv1.Job{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(job), current); err != nil {
			t.Fatal(err)
		}
		current.Spec.Template.Labels[workload.LabelManagedBy] = "ptah-operator"
		current.Spec.Template.Labels[workload.LabelComponent] = workload.ComponentSchemaOperation
		if err := admin.Update(ctx, current); err != nil {
			t.Fatalf("give the template of the suspended Job %s/%s the operation labels: %v", current.Namespace, current.Name, err)
		}
		return current, pod
	}
	// moveTo is pod moved to a Job of a person's own under name, which the
	// garbage collector or the Job controller never does: it takes the old
	// Job's owner reference off as an orphaning does.
	moveTo := func(t *testing.T, pod *corev1.Pod, name string) *corev1.Pod {
		t.Helper()
		target := ownJob(pod.Namespace, name)
		if err := admin.Create(ctx, target); err != nil {
			t.Fatalf("create Job %s/%s: %v", target.Namespace, target.Name, err)
		}
		moved := pod.DeepCopy()
		moved.OwnerReferences = podFor(target).OwnerReferences
		return moved
	}

	t.Run("the Job controller creating a Pod without the operation labels of a Job whose template carries them is refused", func(t *testing.T) {
		t.Parallel()
		job, _ := templateOnly(t, "template-create")
		pod := podFor(job)
		pod.Labels = maps.Clone(pod.Labels)
		delete(pod.Labels, workload.LabelManagedBy)
		delete(pod.Labels, workload.LabelComponent)
		err := clientAs(t, jobController).Create(ctx, pod, client.DryRunAll)
		requireDenied(t, err, podIntentWebhook, "managed Pod Job has no exact operator controller identity")
	})

	t.Run("the garbage collector orphaning a Pod without the operation labels of a Job whose template carries them is refused", func(t *testing.T) {
		t.Parallel()
		job, pod := templateOnly(t, "template-orphan")
		err := clientAs(t, garbageCollector).Patch(ctx, pod.DeepCopy(), orphanPatch(job, pod), client.DryRunAll)
		requireDenied(t, err, podIntentWebhook, "Pod update removed its exact Job owner")
	})

	t.Run("moving a Pod without the operation labels of a Job whose template carries them to another Job is refused", func(t *testing.T) {
		t.Parallel()
		_, pod := templateOnly(t, "template-move")
		err := admin.Update(ctx, moveTo(t, pod, "nightly-report-template-move"), client.DryRunAll)
		requireDenied(t, err, podIntentWebhook, "Pod update removed its exact Job owner")
	})

	t.Run("moving a Pod of a Job only named like an operation Job to another Job is admitted", func(t *testing.T) {
		t.Parallel()
		job := ownJob(fixture.namespace, "ptah-apply-nightly-move")
		if err := admin.Create(ctx, job); err != nil {
			t.Fatalf("create Job %s/%s: %v", job.Namespace, job.Name, err)
		}
		pod := podFor(job)
		if err := clientAs(t, jobController).Create(ctx, pod); err != nil {
			t.Fatalf("the API server refused the Job controller's Pod for %s/%s: %v", job.Namespace, job.Name, err)
		}
		moved := moveTo(t, pod, "nightly-report-name-only-move")
		err := admin.Update(ctx, moved, client.DryRunAll)
		requireNotDeniedBy(t, err, podIntentWebhook)
		if err != nil {
			t.Fatalf("the API server refused moving %s/%s to another Job: %v", pod.Namespace, pod.Name, err)
		}
		if len(moved.OwnerReferences) != 1 || moved.OwnerReferences[0].Name != "nightly-report-name-only-move" {
			t.Fatalf("the admitted update left %s/%s with owner references %v", pod.Namespace, pod.Name, moved.OwnerReferences)
		}
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
