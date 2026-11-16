package podintent_test

import (
	"context"
	"maps"
	"net/http"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const garbageCollectorUsername = "system:serviceaccount:kube-system:generic-garbage-collector"

// orphanRequest is the update the garbage collector sends to orphan pod: the
// same Pod with its Job controller reference taken off.
func orphanRequest(t *testing.T, pod *corev1.Pod) cradmission.Request {
	t.Helper()
	orphaned := pod.DeepCopy()
	orphaned.OwnerReferences = nil
	request := podUpdateRequest(t, pod, orphaned)
	request.UserInfo.Username = garbageCollectorUsername
	return request
}

// withoutIdentity is labels without the two the builder writes to say a Job or
// a Pod is an operation's.
func withoutIdentity(labels map[string]string) map[string]string {
	labels = maps.Clone(labels)
	delete(labels, workload.LabelManagedBy)
	delete(labels, workload.LabelComponent)
	return labels
}

// editStoredJob rewrites the Job the fixture's Pod names, through the
// handler's own reader.
func editStoredJob(t *testing.T, handler *podintent.ValidationHandler, pod *corev1.Pod, edit func(*batchv1.Job)) {
	t.Helper()
	api := handler.Reader.(client.Client)
	job := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: pod.OwnerReferences[0].Name}, job); err != nil {
		t.Fatal(err)
	}
	edit(job)
	if err := api.Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

// The garbage collector orphaning a Pod is decided by the Job it leaves: a Pod
// keeps the owner of a Job that carries the operation identity, on its
// template or on itself, and leaves any other Job. That is the Job the
// orphan-deletion guard refuses to orphan, and every other Job's orphaning
// deletion it admits has to finish.
func TestValidationHandlerDecidesAnOrphanedPodByItsJob(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		// job edits the stored Job; pod edits the Pod before it is orphaned.
		job     func(*batchv1.Job)
		pod     func(*corev1.Pod)
		allowed bool
		message string
	}{
		{
			name:    "a managed Pod",
			message: "managed Pod has no exact Job controller identity",
		},
		{
			// The labels of a Pod an operation Job made can change only while
			// the webhook does not see the write. Its Job still says what it is.
			name:    "an operation Job's Pod that lost its identity",
			pod:     func(pod *corev1.Pod) { pod.Labels = withoutIdentity(pod.Labels) },
			message: "Pod update removed its exact Job owner",
		},
		{
			// An orphaning deletion of its owner, before the resource guard,
			// left the Job with no owner, and a person took its own labels off.
			name: "the Pod of a Job whose template alone carries the identity",
			job: func(job *batchv1.Job) {
				job.OwnerReferences = nil
				job.Labels = withoutIdentity(job.Labels)
			},
			pod:     func(pod *corev1.Pod) { pod.Labels = withoutIdentity(pod.Labels) },
			message: "Pod update removed its exact Job owner",
		},
		{
			name:    "the Pod of a Job whose own labels alone carry the identity",
			job:     func(job *batchv1.Job) { job.Spec.Template.Labels = withoutIdentity(job.Spec.Template.Labels) },
			pod:     func(pod *corev1.Pod) { pod.Labels = withoutIdentity(pod.Labels) },
			message: "Pod update removed its exact Job owner",
		},
		{
			// A Job a person named after the command it runs, as the webhook's
			// match condition reads an operation Job's name.
			name: "the Pod of a Job the operator does not run",
			job: func(job *batchv1.Job) {
				job.OwnerReferences = nil
				job.Labels = withoutIdentity(job.Labels)
				job.Spec.Template.Labels = withoutIdentity(job.Spec.Template.Labels)
			},
			pod:     func(pod *corev1.Pod) { pod.Labels = withoutIdentity(pod.Labels) },
			allowed: true,
			message: "a Job the operator does not run",
		},
		{
			// Only both labels make the identity, as on a Pod.
			name: "the Pod of a Job of another operator component",
			job: func(job *batchv1.Job) {
				job.OwnerReferences = nil
				for _, labels := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
					labels[workload.LabelComponent] = "crd-manager"
				}
			},
			pod:     func(pod *corev1.Pod) { pod.Labels = withoutIdentity(pod.Labels) },
			allowed: true,
			message: "a Job the operator does not run",
		},
		{
			// A Job someone else's name stands under is not the Job the Pod was
			// made by, and says nothing about it.
			name: "the Pod of a Job another Job replaced",
			job: func(job *batchv1.Job) {
				job.UID = "replacement-job-uid"
				job.OwnerReferences = nil
				job.Labels = withoutIdentity(job.Labels)
				job.Spec.Template.Labels = withoutIdentity(job.Spec.Template.Labels)
			},
			pod:     func(pod *corev1.Pod) { pod.Labels = withoutIdentity(pod.Labels) },
			message: "a Job that has been replaced",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			handler, pod := validationHandlerFixture(t)
			pod.Labels = maps.Clone(pod.Labels)
			if test.job != nil {
				editStoredJob(t, handler, pod, func(job *batchv1.Job) {
					job.Labels = maps.Clone(job.Labels)
					job.Spec.Template.Labels = maps.Clone(job.Spec.Template.Labels)
					test.job(job)
				})
			}
			if test.pod != nil {
				test.pod(pod)
			}
			response := handler.Handle(context.Background(), orphanRequest(t, pod))
			if response.Allowed != test.allowed || !strings.Contains(response.Result.Message, test.message) {
				t.Fatalf("Handle() allowed = %t with %q, want allowed = %t with %q",
					response.Allowed, response.Result.Message, test.allowed, test.message)
			}
		})
	}
}

// A Job the webhook cannot read keeps the Pod, the way a Pod update that keeps
// its owner is refused: with a 409 for a Job that is gone.
func TestValidationHandlerKeepsTheOwnerOfAPodWhoseJobIsGone(t *testing.T) {
	t.Parallel()

	handler, pod := validationHandlerFixture(t)
	pod.Labels = withoutIdentity(pod.Labels)
	api := handler.Reader.(client.Client)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.OwnerReferences[0].Name}}
	if err := api.Delete(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	response := handler.Handle(context.Background(), orphanRequest(t, pod))
	if response.Allowed || response.Result.Code != http.StatusConflict ||
		!strings.Contains(response.Result.Message, "read the Job the Pod update removed") {
		t.Fatalf("Handle() allowed = %t, code %d, %q; want a refusal with code 409 that names the Job read",
			response.Allowed, response.Result.Code, response.Result.Message)
	}
}
