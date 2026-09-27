package webhook_test

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	controllerWriteWebhook = "vcontrollerwrite.operator.ptah.run"
	// otherWriter is an identity that may create Jobs and is not the manager.
	otherWriter = "mallory@example.com"
)

// dispatchFixture is a schema with a Resolve operation claimed and its Pod
// admission snapshot persisted: the state the manager is in the moment before
// it creates the Job. job is what the manager's own builder makes of it.
type dispatchFixture struct {
	namespace string
	schema    *operatorv1alpha1.PtahSchema
	job       *batchv1.Job
}

func newDispatchFixture(t *testing.T, prefix string) dispatchFixture {
	t.Helper()
	ctx := context.Background()
	namespace := newNamespace(t, prefix)
	createPolicy(t, namespace)
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: executionAccount}}
	if err := admin.Create(ctx, account); err != nil {
		t.Fatalf("create ServiceAccount %s/%s: %v", namespace, executionAccount, err)
	}
	schema := createSchema(t, namespace, "orders")
	builder := manager.builder()

	operation := operatorv1alpha1.ActiveOperationStatus{
		Type:               operatorv1alpha1.OperationResolve,
		ID:                 "resolve-orders-1",
		InputFingerprint:   digest("8"),
		StartedAt:          now(),
		Attempt:            1,
		ExecutionBindingID: executionBindingID,
	}
	schema.Status = operatorv1alpha1.PtahSchemaStatus{
		ObservedGeneration: schema.Generation,
		ExecutionBinding:   executionBinding(),
	}
	name, err := workload.NameFor(schema, operation)
	if err != nil {
		t.Fatalf("name the Resolve Job for %s/%s: %v", namespace, schema.Name, err)
	}
	operation.JobName = name

	// The manager resolves the snapshot from the Job it is about to create and
	// persists it before creating anything, so the snapshot is taken from the
	// same build the Job comes from.
	unbound, err := builder.Build(schema, operation, nil)
	if err != nil {
		t.Fatalf("build the Resolve Job for %s/%s: %v", namespace, schema.Name, err)
	}
	operation.AdmissionSnapshot, err = podintent.Resolve(ctx, admin, namespace, &unbound.Spec.Template, manager.admission)
	if err != nil {
		t.Fatalf("resolve the Pod admission snapshot in %s: %v", namespace, err)
	}
	schema.Status.ActiveOperation = &operation
	writeStatus(t, schema)

	// The webhook rebuilds the Job from the stored schema, so the test does too.
	stored := &operatorv1alpha1.PtahSchema{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(schema), stored); err != nil {
		t.Fatalf("read PtahSchema %s back: %v", client.ObjectKeyFromObject(schema), err)
	}
	job, err := builder.Build(stored, *stored.Status.ActiveOperation, nil)
	if err != nil {
		t.Fatalf("rebuild the Resolve Job from the stored %s: %v", client.ObjectKeyFromObject(stored), err)
	}
	jobs := rbacv1.PolicyRule{APIGroups: []string{batchv1.GroupName}, Resources: []string{"jobs"}, Verbs: []string{"create", "get"}}
	grant(t, namespace, "manager-jobs", managerSubject(t), jobs)
	grant(t, namespace, "other-jobs", userSubject(otherWriter), jobs)
	return dispatchFixture{namespace: namespace, schema: stored, job: job}
}

// tampered is the builder's Job running another image, the smallest change
// that makes it a different workload.
func (fixture dispatchFixture) tampered() *batchv1.Job {
	job := fixture.job.DeepCopy()
	job.Spec.Template.Spec.Containers[0].Image = "example.invalid/elsewhere@sha256:" + strings.Repeat("5", 64)
	return job
}

func TestControllerWriteWebhook(t *testing.T) {
	plane.Require(t)
	fixture := newDispatchFixture(t, "dispatch")
	ctx := context.Background()
	managerAPI := clientAs(t, manager.username)

	// The three rows share one Job name, so they run in order: the refusals are
	// dry runs, and only the last row stores the Job.
	t.Run("the manager's Job with a changed image is refused", func(t *testing.T) {
		err := managerAPI.Create(ctx, fixture.tampered(), client.DryRunAll)
		requireDenied(t, err, controllerWriteWebhook, "Job is outside the active operation intent")
	})

	t.Run("the same Job from another identity is not the controller-write webhook's to judge", func(t *testing.T) {
		// The entry's matchCondition sends it the manager's writes and nothing
		// else. Nothing else in this API server objects to the Job, so it is
		// admitted; the chart's admission policies are measured elsewhere.
		err := clientAs(t, otherWriter).Create(ctx, fixture.tampered(), client.DryRunAll)
		requireNotDeniedBy(t, err, controllerWriteWebhook)
		if err != nil {
			t.Fatalf("the API server refused %s's Job %s/%s: %v", otherWriter, fixture.namespace, fixture.job.Name, err)
		}
	})

	t.Run("the Job the manager's builder makes is admitted", func(t *testing.T) {
		job := fixture.job.DeepCopy()
		if err := managerAPI.Create(ctx, job); err != nil {
			t.Fatalf("the API server refused the manager's own Job %s/%s: %v", fixture.namespace, fixture.job.Name, err)
		}
		if job.UID == "" {
			t.Fatalf("Job %s/%s was admitted but not stored", fixture.namespace, fixture.job.Name)
		}
	})
}
