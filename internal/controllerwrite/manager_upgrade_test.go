package controllerwrite_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// recordManager writes a manager's identity onto a Job the way the builder
// does: both annotations on the Job and its Pod template, and the image of the
// container that installs the runner.
func recordManager(job *batchv1.Job, controllerImage, controllerRevision, runnerImage string) {
	for _, annotations := range []map[string]string{job.Annotations, job.Spec.Template.Annotations} {
		annotations[workload.AnnotationControllerImage] = controllerImage
		annotations[workload.AnnotationControllerRevision] = controllerRevision
	}
	job.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "install-runner", Image: runnerImage}}
}

// snapshotTemplateOf re-stamps the claim's admission snapshot for the Pod
// template of job, and the snapshot digest the Job carries with it.
func snapshotTemplateOf(t *testing.T, operation *operatorv1alpha1.ActiveOperationStatus, jobs ...*batchv1.Job) {
	t.Helper()

	digest, err := podintent.DigestTemplate(&jobs[0].Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	operation.AdmissionSnapshot.TemplateDigest = digest
	operation.AdmissionSnapshot.Digest = ""
	snapshotDigest, err := fingerprint.DigestCanonicalJSON(*operation.AdmissionSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	operation.AdmissionSnapshot.Digest = snapshotDigest
	for _, job := range jobs {
		job.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshotDigest
		job.Spec.Template.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshotDigest
	}
}

// TestPlanPublicationAcceptsAPlanJobThePreviousManagerDispatched publishes a
// plan whose Plan Job an earlier manager of the same execution binding
// dispatched. The manager publishing now rebuilds that Job with its own
// identity; the webhook takes the recorded identity from the Job, and the
// claim's snapshot, taken before the earlier manager dispatched, is what the
// Job has to match. A Job whose runner the claim never recorded is refused.
func TestPlanPublicationAcceptsAPlanJobThePreviousManagerDispatched(t *testing.T) {
	t.Parallel()

	previousImage := "example.test/controller@" + digest('8')
	previousRunner := "example.test/runner@" + digest('9')
	for _, test := range []struct {
		name   string
		mutate func(*batchv1.Job)
		allow  bool
	}{
		{name: "the Job the previous manager dispatched", allow: true},
		{name: "a runner the claim never recorded", mutate: func(job *batchv1.Job) {
			job.Spec.Template.Spec.InitContainers[0].Image = "example.test/runner@" + digest('7')
		}},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture(operatorv1alpha1.OperationPlan)
			plan, _ := preparedPlanFixture(t, schema)
			operation := schema.Status.ActiveOperation
			expected := expectedJob(schema, operation)
			recordManager(expected, testControllerImage, testControllerRevision, testRunnerImage)
			dispatched := expected.DeepCopy()
			recordManager(dispatched, previousImage, "previous-revision", previousRunner)
			snapshotTemplateOf(t, operation, dispatched, expected)

			terminal := withGeneratedJobIdentity(dispatched)
			terminal.UID = operation.JobUID
			terminal.Spec.Selector.MatchLabels[batchv1.ControllerUidLabel] = string(terminal.UID)
			terminal.Spec.Template.Labels[batchv1.ControllerUidLabel] = string(terminal.UID)
			ttl := int32(300)
			terminal.Spec.TTLSecondsAfterFinished = &ttl
			terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			if test.mutate != nil {
				test.mutate(terminal)
			}
			handler := handlerFixture(t, staticJobBuilder{job: expected}, schema, terminal)

			response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, plan))
			if response.Allowed != test.allow {
				message := ""
				if response.Result != nil {
					message = response.Result.Message
				}
				t.Fatalf("Handle() allowed = %t (%s), want %t", response.Allowed, message, test.allow)
			}
			if !test.allow && !strings.Contains(responseMessage(response), "admission snapshot") {
				t.Fatalf("refusal = %q, want one naming the admission snapshot", responseMessage(response))
			}
		})
	}
}

// TestPendingApplyCleanupAcceptsADispatcherOtherThanThePublisher schedules
// cleanup for an Apply Job whose plan one manager published and another, of
// the same execution binding, dispatched. The plan's record and the Job's
// record differ, and neither binds anything; the Job's own record is held to
// the claim's snapshot.
func TestPendingApplyCleanupAcceptsADispatcherOtherThanThePublisher(t *testing.T) {
	t.Parallel()

	schema, oldJob, job := pendingApplyCleanupFixture(t)
	schema.Status.PendingObservation.Plan.ControllerImage = "example.test/controller@" + digest('8')
	schema.Status.PendingObservation.Plan.ControllerRevision = "the-publishing-release"
	handler := handlerFixture(t, staticJobBuilder{err: errors.New("retired Apply must not reconstruct")}, schema)
	request := requestFor(t, admissionv1.Update, job)
	request.OldObject = rawObject(t, oldJob)

	if response := handler.Handle(context.Background(), request); !response.Allowed {
		t.Fatalf("Handle() refused cleanup of an Apply another manager of the binding dispatched: %s",
			responseMessage(response))
	}
}
