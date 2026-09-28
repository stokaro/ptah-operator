package controllerwrite_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerwrite"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/plancontract"
	"github.com/stokaro/ptah-operator/internal/planseal"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const managerUsername = "system:serviceaccount:ptah-system:ptah-operator"

// The manager release every fixture here runs as. It is recorded on the Jobs
// and plans the manager writes, and binds none of them.
var (
	testControllerImage    = "example.test/controller@" + digest('1')
	testControllerRevision = "test-revision"
	testRunnerImage        = "example.test/runner@" + digest('3')
)

type staticJobBuilder struct {
	job *batchv1.Job
	err error
}

func (staticJobBuilder) ManagerIdentity() (controllerImage, controllerRevision, runnerImage string) {
	return testControllerImage, testControllerRevision, testRunnerImage
}

type barrierChunkReader struct {
	client.Reader

	release  chan struct{}
	once     sync.Once
	mu       sync.Mutex
	entered  int
	inFlight int
	maximum  int
}

func (r *barrierChunkReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if _, isChunk := object.(*corev1.ConfigMap); !isChunk {
		return r.Reader.Get(ctx, key, object, options...)
	}

	r.mu.Lock()
	r.entered++
	r.inFlight++
	if r.inFlight > r.maximum {
		r.maximum = r.inFlight
	}
	if r.entered == plancontract.MaxChunks {
		r.once.Do(func() { close(r.release) })
	}
	r.mu.Unlock()

	select {
	case <-r.release:
	case <-ctx.Done():
		r.mu.Lock()
		r.inFlight--
		r.mu.Unlock()
		return ctx.Err()
	}

	err := r.Reader.Get(ctx, key, object, options...)
	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
	return err
}

func (r *barrierChunkReader) maximumConcurrency() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maximum
}

func (b staticJobBuilder) Build(
	_ *operatorv1alpha1.PtahSchema,
	_ operatorv1alpha1.ActiveOperationStatus,
	_ *operatorv1alpha1.PtahSchemaPlan,
) (*batchv1.Job, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.job.DeepCopy(), nil
}

func (b staticJobBuilder) BuildMigration(
	_ *operatorv1alpha1.PtahMigration,
	_ operatorv1alpha1.MigrationOperationStatus,
	_ *operatorv1alpha1.PtahMigrationPlan,
) (*batchv1.Job, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.job.DeepCopy(), nil
}

func TestValidationHandlerAllowsExactJobCreate(t *testing.T) {
	t.Parallel()

	schema := schemaFixture(operatorv1alpha1.OperationResolve)
	expected := expectedJob(schema, schema.Status.ActiveOperation)
	candidate := withGeneratedJobIdentity(expected)
	handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)

	response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, candidate))
	if !response.Allowed {
		t.Fatalf("Handle() denied exact Job create: %#v", response.Result)
	}
}

func TestValidationHandlerAllowsFreshReadOnlyRetryJobCreate(t *testing.T) {
	t.Parallel()

	for _, operationType := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationResolve,
		operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve,
		operatorv1alpha1.OperationPlan,
	} {
		operationType := operationType
		t.Run(string(operationType), func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture(operationType)
			operation := schema.Status.ActiveOperation
			operation.Attempt = 2
			operation.JobUID = ""
			jobName, err := workload.NameFor(schema, *operation)
			if err != nil {
				t.Fatal(err)
			}
			operation.JobName = jobName
			expected := expectedJob(schema, operation)
			handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)

			response := handler.Handle(
				context.Background(),
				requestFor(t, admissionv1.Create, withGeneratedJobIdentity(expected)),
			)
			if !response.Allowed {
				t.Fatalf("Handle() denied fresh %s retry Job create: %#v", operationType, response.Result)
			}
		})
	}
}

func TestValidationHandlerRejectsReadOnlyRetryJobCreateWithPersistedUID(t *testing.T) {
	t.Parallel()

	for _, operationType := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationResolve,
		operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve,
		operatorv1alpha1.OperationPlan,
	} {
		operationType := operationType
		t.Run(string(operationType), func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture(operationType)
			operation := schema.Status.ActiveOperation
			operation.Attempt = 2
			operation.JobUID = ""
			jobName, err := workload.NameFor(schema, *operation)
			if err != nil {
				t.Fatal(err)
			}
			operation.JobName = jobName
			expected := expectedJob(schema, operation)
			candidate := withGeneratedJobIdentity(expected)
			operation.JobUID = candidate.UID
			handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)

			if response := handler.Handle(
				context.Background(),
				requestFor(t, admissionv1.Create, candidate),
			); response.Allowed {
				t.Fatalf("Handle() allowed %s retry Job create with a persisted UID", operationType)
			}
		})
	}
}

func TestValidationHandlerAppliesTheSameContractToDryRun(t *testing.T) {
	t.Parallel()

	schema := schemaFixture(operatorv1alpha1.OperationResolve)
	expected := expectedJob(schema, schema.Status.ActiveOperation)
	handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)
	request := requestFor(t, admissionv1.Create, withGeneratedJobIdentity(expected))
	dryRun := true
	request.DryRun = &dryRun

	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() denied exact dry-run Job create: %#v", response.Result)
	}
}

func TestValidationHandlerRejectsJobCreateOutsideReconstructedIntent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*batchv1.Job)
	}{
		{
			name: "arbitrary privileged workload",
			mutate: func(job *batchv1.Job) {
				privileged := true
				job.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &privileged}
			},
		},
		{
			name: "service account substitution",
			mutate: func(job *batchv1.Job) {
				job.Spec.Template.Spec.ServiceAccountName = "cluster-admin"
			},
		},
		{
			name: "managed label omission",
			mutate: func(job *batchv1.Job) {
				delete(job.Labels, workload.LabelManagedBy)
			},
		},
		{
			name: "Pod selector label omission",
			mutate: func(job *batchv1.Job) {
				delete(job.Spec.Template.Labels, workload.LabelComponent)
			},
		},
		{
			name: "wrong owner",
			mutate: func(job *batchv1.Job) {
				job.OwnerReferences[0].UID = "another-schema"
			},
		},
		{
			name: "extra authorization annotation",
			mutate: func(job *batchv1.Job) {
				job.Annotations["example.test/database-access"] = "granted"
			},
		},
		{
			name: "status injection",
			mutate: func(job *batchv1.Job) {
				job.Status.Succeeded = 1
			},
		},
		{
			name: "forged API selector",
			mutate: func(job *batchv1.Job) {
				job.Spec.Selector.MatchLabels[batchv1.ControllerUidLabel] = "another-job"
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture(operatorv1alpha1.OperationResolve)
			expected := expectedJob(schema, schema.Status.ActiveOperation)
			candidate := withGeneratedJobIdentity(expected)
			test.mutate(candidate)
			handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)

			response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, candidate))
			if response.Allowed {
				t.Fatal("Handle() allowed a Job outside its reconstructed intent")
			}
		})
	}
}

func TestValidationHandlerAllowsExactTerminalJobCleanup(t *testing.T) {
	t.Parallel()

	schema, expected, oldJob, job := currentCleanupFixture(t, operatorv1alpha1.OperationResolve)
	handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)

	request := requestFor(t, admissionv1.Update, job)
	request.OldObject = rawObject(t, oldJob)
	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() denied exact terminal cleanup patch: %#v", response.Result)
	}
}

func TestValidationHandlerAllowsExactRunningCurrentApplyCleanup(t *testing.T) {
	t.Parallel()

	schema, expected, oldJob, job := currentCleanupFixture(t, operatorv1alpha1.OperationApply)
	oldJob.Status.Conditions = nil
	job.Status.Conditions = nil
	handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)
	request := requestFor(t, admissionv1.Update, job)
	request.OldObject = rawObject(t, oldJob)

	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() denied exact running current Apply cleanup: %#v", response.Result)
	}
}

func TestValidationHandlerRejectsMutatedRunningCurrentApplyCleanup(t *testing.T) {
	t.Parallel()

	schema, expected, oldJob, job := currentCleanupFixture(t, operatorv1alpha1.OperationApply)
	oldJob.Status.Conditions = nil
	job.Status.Conditions = nil
	for _, candidate := range []*batchv1.Job{oldJob, job} {
		candidate.Annotations[workload.AnnotationPlanFingerprint] = digest('9')
		candidate.Spec.Template.Annotations[workload.AnnotationPlanFingerprint] = digest('9')
	}
	handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)
	request := requestFor(t, admissionv1.Update, job)
	request.OldObject = rawObject(t, oldJob)

	if response := handler.Handle(context.Background(), request); response.Allowed {
		t.Fatal("Handle() allowed a running current Apply cleanup outside its exact plan binding")
	}
}

func TestValidationHandlerAllowsExactCleanupForDeletingOwner(t *testing.T) {
	t.Parallel()

	schema, expected, oldJob, job := currentCleanupFixture(t, operatorv1alpha1.OperationApply)
	deletedAt := metav1.NewTime(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	schema.DeletionTimestamp = &deletedAt
	schema.Finalizers = []string{"operator.ptah.run/active-operation"}
	handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)
	request := requestFor(t, admissionv1.Update, job)
	request.OldObject = rawObject(t, oldJob)

	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() denied exact cleanup for a deleting owner: %#v", response.Result)
	}
}

func TestValidationHandlerRejectsCreatesForDeletingOwner(t *testing.T) {
	t.Parallel()

	deletedAt := metav1.NewTime(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	tests := map[string]struct {
		fixture func(*testing.T) (*controllerwrite.ValidationHandler, client.Object)
	}{
		"Job": {
			fixture: func(t *testing.T) (*controllerwrite.ValidationHandler, client.Object) {
				schema := schemaFixture(operatorv1alpha1.OperationResolve)
				schema.DeletionTimestamp = &deletedAt
				schema.Finalizers = []string{"operator.ptah.run/active-operation"}
				job := expectedJob(schema, schema.Status.ActiveOperation)
				return handlerFixture(t, staticJobBuilder{job: job}, schema), job
			},
		},
		"plan": {
			fixture: func(t *testing.T) (*controllerwrite.ValidationHandler, client.Object) {
				schema := schemaFixture(operatorv1alpha1.OperationPlan)
				schema.DeletionTimestamp = &deletedAt
				schema.Finalizers = []string{"operator.ptah.run/active-operation"}
				plan, _ := preparedPlanFixture(t, schema)
				return planManifestHandlerFixture(t, schema), plan
			},
		},
		"plan chunk": {
			fixture: func(t *testing.T) (*controllerwrite.ValidationHandler, client.Object) {
				schema := schemaFixture(operatorv1alpha1.OperationPlan)
				schema.DeletionTimestamp = &deletedAt
				schema.Finalizers = []string{"operator.ptah.run/active-operation"}
				plan, chunks := preparedPlanFixture(t, schema)
				plan.UID = "plan-uid"
				chunk := planChunk(plan, plan.Spec.Chunks[0], chunks[0], "")
				handler := handlerFixture(t, staticJobBuilder{job: expectedJob(schema, schema.Status.ActiveOperation)}, schema, plan)
				return handler, chunk
			},
		},
		"plan projection": {
			fixture: func(t *testing.T) (*controllerwrite.ValidationHandler, client.Object) {
				schema, plan, chunks := applyProjectionFixture(t)
				schema.DeletionTimestamp = &deletedAt
				schema.Finalizers = []string{"operator.ptah.run/active-operation"}
				projection := planProjection(plan, plan.Spec.Chunks[0], chunks[0])
				handler := handlerFixture(t, staticJobBuilder{job: expectedJob(schema, schema.Status.ActiveOperation)}, schema, plan)
				return handler, projection
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			handler, object := test.fixture(t)
			response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, object))
			if response.Allowed {
				t.Fatalf("Handle() allowed %s creation for a deleting owner", name)
			}
		})
	}
}

func TestValidationHandlerAllowsClaimBoundTerminalCleanupAfterInputsChange(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name          string
		operationType operatorv1alpha1.OperationType
		changeInputs  func(*operatorv1alpha1.PtahSchema)
	}{
		{
			name:          "read-only",
			operationType: operatorv1alpha1.OperationResolve,
			changeInputs: func(schema *operatorv1alpha1.PtahSchema) {
				schema.Spec.Execution.ServiceAccountName = "ptah-orders-next"
			},
		},
		{
			name:          "Apply",
			operationType: operatorv1alpha1.OperationApply,
			changeInputs: func(schema *operatorv1alpha1.PtahSchema) {
				schema.Spec.Policy.AllowDestructive = !schema.Spec.Policy.AllowDestructive
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema, _, oldJob, job := currentCleanupFixture(t, test.operationType)
			schema.Generation++
			test.changeInputs(schema)
			handler := handlerFixture(t, staticJobBuilder{err: errors.New("mutable inputs changed")}, schema)
			request := requestFor(t, admissionv1.Update, job)
			request.OldObject = rawObject(t, oldJob)

			response := handler.Handle(context.Background(), request)
			if !response.Allowed {
				t.Fatalf("Handle() denied claim-bound %s terminal cleanup after input change: %#v", test.name, response.Result)
			}
		})
	}
}

func TestValidationHandlerAllowsCurrentFormatRetiredReadOnlyJobCleanup(t *testing.T) {
	t.Parallel()

	schema, _, oldJob, job := currentCleanupFixture(t, operatorv1alpha1.OperationResolve)
	schema.Status.ExecutionBinding.Epoch = "v1-99999999999999999999999999999999"
	schema.Status.ExecutionBinding.ControllerStateVersion = 2
	schema.Status.ExecutionBinding.PtahVersion = "v0.4.0"
	setExecutionBindingRetirementFence(schema)
	handler := handlerFixture(t, staticJobBuilder{err: errors.New("retired execution binding")}, schema)
	request := requestFor(t, admissionv1.Update, job)
	request.OldObject = rawObject(t, oldJob)

	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() denied current-format retired read-only Job cleanup: %#v", response.Result)
	}
}

func TestValidationHandlerRejectsUnsafeClaimBoundJobCleanup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		operationType operatorv1alpha1.OperationType
		mutate        func(*operatorv1alpha1.PtahSchema, *batchv1.Job, *batchv1.Job)
	}{
		{
			name:          "operation UID mismatch",
			operationType: operatorv1alpha1.OperationResolve,
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.ActiveOperation.JobUID = "another-job-uid"
			},
		},
		{
			// The recorded manager binds nothing, but it is part of the Pod
			// template the claim's snapshot pinned, so a Job whose record
			// was rewritten after dispatch is not the claim's Job.
			name:          "recorded manager differs from the snapshot",
			operationType: operatorv1alpha1.OperationResolve,
			mutate: func(_ *operatorv1alpha1.PtahSchema, oldJob, job *batchv1.Job) {
				for _, candidate := range []*batchv1.Job{oldJob, job} {
					candidate.Annotations[workload.AnnotationControllerRevision] = "other-revision"
					candidate.Spec.Template.Annotations[workload.AnnotationControllerRevision] = "other-revision"
				}
			},
		},
		{
			name:          "Pod template differs from snapshot",
			operationType: operatorv1alpha1.OperationResolve,
			mutate: func(_ *operatorv1alpha1.PtahSchema, oldJob, job *batchv1.Job) {
				for _, candidate := range []*batchv1.Job{oldJob, job} {
					candidate.Spec.Template.Spec.Containers[0].Image = "example.test/replaced@" + digest('9')
				}
			},
		},
		{
			name:          "Apply plan digest mismatch",
			operationType: operatorv1alpha1.OperationApply,
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.Plan.Fingerprint = digest('9')
			},
		},
		{
			name:          "retired operation without durable fence",
			operationType: operatorv1alpha1.OperationResolve,
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.ExecutionBinding.Epoch = "v1-99999999999999999999999999999999"
			},
		},
		{
			name:          "retired mutating operation",
			operationType: operatorv1alpha1.OperationApply,
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.ExecutionBinding.Epoch = "v1-99999999999999999999999999999999"
				setExecutionBindingRetirementFence(schema)
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema, _, oldJob, job := currentCleanupFixture(t, test.operationType)
			test.mutate(schema, oldJob, job)
			handler := handlerFixture(t, staticJobBuilder{err: errors.New("claim fallback must stay narrow")}, schema)
			request := requestFor(t, admissionv1.Update, job)
			request.OldObject = rawObject(t, oldJob)

			if response := handler.Handle(context.Background(), request); response.Allowed {
				t.Fatal("Handle() allowed unsafe claim-bound Job cleanup")
			}
		})
	}
}

func TestValidationHandlerAllowsCurrentFormatPendingApplyJobCleanup(t *testing.T) {
	t.Parallel()

	schema, oldJob, job := pendingApplyCleanupFixture(t)
	handler := handlerFixture(t, staticJobBuilder{err: errors.New("retired Apply must not reconstruct")}, schema)
	request := requestFor(t, admissionv1.Update, job)
	request.OldObject = rawObject(t, oldJob)

	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() denied exact current-format pending Apply cleanup: %#v", response.Result)
	}
}

func TestValidationHandlerRejectsUnsafeCurrentFormatPendingApplyJobCleanup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*operatorv1alpha1.PtahSchema, *batchv1.Job, *batchv1.Job)
	}{
		{
			name: "missing persisted admission snapshot",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.PendingObservation.AdmissionSnapshot = nil
			},
		},
		{
			name: "mutated Pod template",
			mutate: func(_ *operatorv1alpha1.PtahSchema, oldJob, job *batchv1.Job) {
				for _, candidate := range []*batchv1.Job{oldJob, job} {
					candidate.Spec.Template.Spec.Containers[0].Image = "example.test/replaced@" + digest('9')
				}
			},
		},
		{
			name: "recorded manager differs from the snapshot",
			mutate: func(_ *operatorv1alpha1.PtahSchema, oldJob, job *batchv1.Job) {
				for _, candidate := range []*batchv1.Job{oldJob, job} {
					candidate.Annotations[workload.AnnotationControllerRevision] = "other-revision"
					candidate.Spec.Template.Annotations[workload.AnnotationControllerRevision] = "other-revision"
				}
			},
		},
		{
			name: "mutated snapshot annotation",
			mutate: func(_ *operatorv1alpha1.PtahSchema, oldJob, job *batchv1.Job) {
				for _, candidate := range []*batchv1.Job{oldJob, job} {
					candidate.Annotations[workload.AnnotationAdmissionSnapshotDigest] = digest('9')
					candidate.Spec.Template.Annotations[workload.AnnotationAdmissionSnapshotDigest] = digest('9')
				}
			},
		},
		{
			name: "nonterminal Job",
			mutate: func(_ *operatorv1alpha1.PtahSchema, oldJob, job *batchv1.Job) {
				oldJob.Status.Conditions = nil
				job.Status.Conditions = nil
			},
		},

		// When the permission applies. Scheduling the deletion of the Job of a
		// run nobody accounted for is allowed only at the boundary where the
		// claim has already become durable evidence and nothing is in flight
		// against it.
		{
			name: "a claim is in flight again",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
					Type: operatorv1alpha1.OperationObserve, ID: "a-fresh-observation",
					JobName: "ptah-observe-app",
				}
			},
		},
		{
			// The record is the permission. Without it the same pending
			// observation authorizes nothing, whatever the phase says.
			name: "no execution-binding retirement is pending",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.PendingBindingRetirement = nil
			},
		},
		{
			// A run whose outcome is known is not this permission's subject.
			name: "the pending outcome is no longer unknown",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.PendingObservation.Outcome =
					operatorv1alpha1.PendingObservationApplySucceeded
			},
		},
		{
			name: "the retirement owes approvals and no Job",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.PendingBindingRetirement.Job = nil
				schema.Status.PendingBindingRetirement.Plan = &operatorv1alpha1.RetiredPlanReference{
					Name: "plan-current", UID: "plan-uid", Fingerprint: digest('a'),
				}
			},
		},
		{
			name: "the retirement names a read-only claim's Job",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.PendingBindingRetirement.Job.Operation = operatorv1alpha1.OperationObserve
			},
		},
		{
			name: "the retirement names another Job",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.PendingBindingRetirement.Job.Name += "-beside-it"
			},
		},
		{
			// A UID the record has not adopted is a Job it has not proven.
			name: "the retirement has not adopted the Job's UID",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.PendingBindingRetirement.Job.UID = ""
			},
		},
		{
			name: "the retirement names another Job identity",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.PendingBindingRetirement.Job.UID = "a-job-that-ran-later"
			},
		},
		{
			name: "the retirement retired another epoch",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.PendingBindingRetirement.RetiredEpoch = "v1-" + strings.Repeat("8", 32)
			},
		},
		{
			// The Job belongs to the binding in force, so it is not a
			// predecessor and this path is not what may touch it.
			name: "the Job belongs to the binding still in force",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.PendingObservation.Plan.ExecutionBindingID =
					schema.Status.ExecutionBinding.Epoch
			},
		},

		// Which object the permission applies to. The evidence names one Job,
		// and stamping a TTL schedules its deletion, so naming another would
		// collect the only record of what some other run did.
		{
			name: "the evidence names no Apply operation",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.PendingObservation.ApplyOperationID = ""
			},
		},
		{
			name: "the evidence names another Job",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.PendingObservation.ApplyJobName += "-beside-it"
			},
		},
		{
			name: "the evidence names another Job identity",
			mutate: func(schema *operatorv1alpha1.PtahSchema, _, _ *batchv1.Job) {
				schema.Status.PendingObservation.ApplyJobUID = "a-job-that-ran-later"
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema, oldJob, job := pendingApplyCleanupFixture(t)
			test.mutate(schema, oldJob, job)
			handler := handlerFixture(t, staticJobBuilder{err: errors.New("retired Apply must not reconstruct")}, schema)
			request := requestFor(t, admissionv1.Update, job)
			request.OldObject = rawObject(t, oldJob)

			if response := handler.Handle(context.Background(), request); response.Allowed {
				t.Fatal("Handle() allowed unsafe current-format pending Apply cleanup")
			}
		})
	}
}

func TestValidationHandlerRejectsCurrentFormatPendingApplyRedispatch(t *testing.T) {
	t.Parallel()

	schema, oldJob, _ := pendingApplyCleanupFixture(t)
	candidate := oldJob.DeepCopy()
	candidate.Status = batchv1.JobStatus{}
	handler := handlerFixture(t, staticJobBuilder{job: candidate}, schema)

	if response := handler.Handle(
		context.Background(),
		requestFor(t, admissionv1.Create, candidate),
	); response.Allowed {
		t.Fatal("Handle() treated pending Apply evidence as a rerunnable operation")
	}
}

func TestValidationHandlerRejectsUnexpectedJobCleanupChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*batchv1.Job, *batchv1.Job)
	}{
		{
			name: "nonterminal Job",
			mutate: func(oldJob, _ *batchv1.Job) {
				oldJob.Status.Conditions = nil
			},
		},
		{
			name: "different TTL",
			mutate: func(_, job *batchv1.Job) {
				ttl := int32(301)
				job.Spec.TTLSecondsAfterFinished = &ttl
			},
		},
		{
			name: "image patch",
			mutate: func(_, job *batchv1.Job) {
				job.Spec.Template.Spec.Containers[0].Image = "example.test/other@" + digest('9')
			},
		},
		{
			name: "service account patch",
			mutate: func(_, job *batchv1.Job) {
				job.Spec.Template.Spec.ServiceAccountName = "cluster-admin"
			},
		},
		{
			name: "label patch",
			mutate: func(_, job *batchv1.Job) {
				job.Labels["example.test/access"] = "granted"
			},
		},
		{
			name: "owner patch",
			mutate: func(_, job *batchv1.Job) {
				job.OwnerReferences[0].UID = "another-schema"
			},
		},
		{
			name: "status patch",
			mutate: func(_, job *batchv1.Job) {
				job.Status.Succeeded++
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture(operatorv1alpha1.OperationResolve)
			expected := expectedJob(schema, schema.Status.ActiveOperation)
			oldJob := withGeneratedJobIdentity(expected)
			oldJob.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			schema.Status.ActiveOperation.JobUID = oldJob.UID
			job := oldJob.DeepCopy()
			ttl := int32(300)
			job.Spec.TTLSecondsAfterFinished = &ttl
			test.mutate(oldJob, job)
			handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)

			request := requestFor(t, admissionv1.Update, job)
			request.OldObject = rawObject(t, oldJob)
			response := handler.Handle(context.Background(), request)
			if response.Allowed {
				t.Fatal("Handle() allowed fields outside the exact cleanup TTL transition")
			}
		})
	}
}

func TestValidationHandlerRejectsJobUpdateWithoutOldObject(t *testing.T) {
	t.Parallel()

	schema := schemaFixture(operatorv1alpha1.OperationResolve)
	expected := expectedJob(schema, schema.Status.ActiveOperation)
	job := withGeneratedJobIdentity(expected)
	ttl := int32(300)
	job.Spec.TTLSecondsAfterFinished = &ttl
	handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)

	response := handler.Handle(context.Background(), requestFor(t, admissionv1.Update, job))
	if response.Allowed || response.Result == nil || response.Result.Code != 400 {
		t.Fatalf("Handle() response = %#v, want bad request", response)
	}
}

func TestValidationHandlerAllowsExactPlanManifestCreate(t *testing.T) {
	t.Parallel()

	schema := schemaFixture(operatorv1alpha1.OperationPlan)
	plan, _ := preparedPlanFixture(t, schema)
	handler := planManifestHandlerFixture(t, schema)

	response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, plan))
	if !response.Allowed {
		t.Fatalf("Handle() denied exact plan manifest: %#v", response.Result)
	}
}

func TestValidationHandlerRejectsInvalidPlanManifest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*operatorv1alpha1.PtahSchemaPlan)
	}{
		{
			name: "wrong owner",
			mutate: func(plan *operatorv1alpha1.PtahSchemaPlan) {
				plan.OwnerReferences[0].UID = "another-schema"
			},
		},
		{
			name: "extra label",
			mutate: func(plan *operatorv1alpha1.PtahSchemaPlan) {
				plan.Labels["example.test/access"] = "granted"
			},
		},
		{
			name: "status injection",
			mutate: func(plan *operatorv1alpha1.PtahSchemaPlan) {
				plan.Status.ObservedGeneration = 1
			},
		},
		{
			name: "wrong chunk name",
			mutate: func(plan *operatorv1alpha1.PtahSchemaPlan) {
				plan.Spec.Chunks[0].Name = "different-chunk"
			},
		},
		{
			name: "zero statements",
			mutate: func(plan *operatorv1alpha1.PtahSchemaPlan) {
				plan.Spec.StatementCount = 0
			},
		},
		// What the manager read out of the plan bytes is bound by the
		// fingerprint, so a plan that reports another reading under the same
		// fingerprint is not the plan that was fingerprinted.
		{
			name: "destructive reading changed under the same fingerprint",
			mutate: func(plan *operatorv1alpha1.PtahSchemaPlan) {
				plan.Spec.Destructive = !plan.Spec.Destructive
			},
		},
		{
			name: "privilege reading changed under the same fingerprint",
			mutate: func(plan *operatorv1alpha1.PtahSchemaPlan) {
				plan.Spec.PrivilegeChanges = []operatorv1alpha1.PrivilegeChange{operatorv1alpha1.PrivilegeChangeGrant}
			},
		},
		{
			name: "statement count changed under the same fingerprint",
			mutate: func(plan *operatorv1alpha1.PtahSchemaPlan) {
				plan.Spec.StatementCount++
			},
		},
		{
			name: "wrong execution epoch",
			mutate: func(plan *operatorv1alpha1.PtahSchemaPlan) {
				plan.Spec.ExecutionBindingID = "v1-99999999999999999999999999999999"
			},
		},
		{
			name: "wrong policy fingerprint",
			mutate: func(plan *operatorv1alpha1.PtahSchemaPlan) {
				plan.Spec.PolicyFingerprint = digest('9')
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture(operatorv1alpha1.OperationPlan)
			plan, _ := preparedPlanFixture(t, schema)
			test.mutate(plan)
			handler := planManifestHandlerFixture(t, schema)

			response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, plan))
			if response.Allowed {
				t.Fatal("Handle() allowed an invalid plan manifest")
			}
		})
	}
}

func TestValidationHandlerRejectsInvalidStateDigestWithRecomputedPlanFingerprint(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		mutate func(*operatorv1alpha1.PtahSchemaPlan)
	}{
		{
			name: "actual state",
			mutate: func(plan *operatorv1alpha1.PtahSchemaPlan) {
				plan.Spec.ActualStateFingerprint = "nonempty-but-not-a-digest"
			},
		},
		{
			name: "desired state",
			mutate: func(plan *operatorv1alpha1.PtahSchemaPlan) {
				plan.Spec.DesiredStateFingerprint = "nonempty-but-not-a-digest"
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture(operatorv1alpha1.OperationPlan)
			plan, _ := preparedPlanFixture(t, schema)
			test.mutate(plan)
			plan.Spec.Fingerprint = recomputePlanFingerprint(t, schema, plan)
			handler := planManifestHandlerFixture(t, schema)

			response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, plan))
			if response.Allowed {
				t.Fatal("Handle() accepted a non-digest state fingerprint after recomputing the outer fingerprint")
			}
		})
	}
}

func TestValidationHandlerRejectsPlanFromChangedTerminalJob(t *testing.T) {
	t.Parallel()

	schema := schemaFixture(operatorv1alpha1.OperationPlan)
	plan, _ := preparedPlanFixture(t, schema)
	expected := expectedJob(schema, schema.Status.ActiveOperation)
	terminal := withGeneratedJobIdentity(expected)
	terminal.UID = schema.Status.ActiveOperation.JobUID
	terminal.Spec.Selector.MatchLabels[batchv1.ControllerUidLabel] = string(terminal.UID)
	terminal.Spec.Template.Labels[batchv1.ControllerUidLabel] = string(terminal.UID)
	ttl := int32(300)
	terminal.Spec.TTLSecondsAfterFinished = &ttl
	terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	terminal.Spec.Template.Spec.Containers[0].Command = []string{"/bin/sh"}
	handler := handlerFixture(t, staticJobBuilder{job: expected}, schema, terminal)

	response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, plan))
	if response.Allowed {
		t.Fatal("Handle() allowed a plan whose harvested terminal Job changed immutable intent")
	}
}

func TestValidationHandlerRejectsPlanFromFailedTerminalJob(t *testing.T) {
	t.Parallel()

	schema := schemaFixture(operatorv1alpha1.OperationPlan)
	plan, _ := preparedPlanFixture(t, schema)
	expected := expectedJob(schema, schema.Status.ActiveOperation)
	terminal := withGeneratedJobIdentity(expected)
	terminal.UID = schema.Status.ActiveOperation.JobUID
	terminal.Spec.Selector.MatchLabels[batchv1.ControllerUidLabel] = string(terminal.UID)
	terminal.Spec.Template.Labels[batchv1.ControllerUidLabel] = string(terminal.UID)
	ttl := int32(300)
	terminal.Spec.TTLSecondsAfterFinished = &ttl
	terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	handler := handlerFixture(t, staticJobBuilder{job: expected}, schema, terminal)

	response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, plan))
	if response.Allowed {
		t.Fatal("Handle() allowed plan publication from a failed source Job")
	}
}

func TestValidationHandlerAllowsExactImmutablePlanChunk(t *testing.T) {
	t.Parallel()

	schema := schemaFixture(operatorv1alpha1.OperationPlan)
	plan, chunks := preparedPlanFixture(t, schema)
	plan.UID = "plan-uid"
	chunk := planChunk(plan, plan.Spec.Chunks[0], chunks[0], "")
	handler := handlerFixture(t, staticJobBuilder{job: expectedJob(schema, schema.Status.ActiveOperation)}, schema, plan)

	response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, chunk))
	if !response.Allowed {
		t.Fatalf("Handle() denied exact immutable plan chunk: %#v", response.Result)
	}
}

func TestValidationHandlerRejectsInvalidPlanChunk(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*operatorv1alpha1.PtahSchemaPlanChunk)
	}{
		{
			name: "payload mismatch",
			mutate: func(chunk *operatorv1alpha1.PtahSchemaPlanChunk) {
				chunk.Spec.Data[0] ^= 0xff
			},
		},
		{
			name: "short payload",
			mutate: func(chunk *operatorv1alpha1.PtahSchemaPlanChunk) {
				chunk.Spec.Data = chunk.Spec.Data[:len(chunk.Spec.Data)-1]
			},
		},
		{
			name: "wrong label",
			mutate: func(chunk *operatorv1alpha1.PtahSchemaPlanChunk) {
				chunk.Labels[planstore.LabelPlan] = "another-plan"
			},
		},
		{
			name: "extra annotation",
			mutate: func(chunk *operatorv1alpha1.PtahSchemaPlanChunk) {
				chunk.Annotations = map[string]string{"example.test/extra": "unexpected"}
			},
		},
		{
			name: "wrong owner",
			mutate: func(chunk *operatorv1alpha1.PtahSchemaPlanChunk) {
				chunk.OwnerReferences[0].UID = "another-plan"
			},
		},
		{
			name: "name outside the manifest",
			mutate: func(chunk *operatorv1alpha1.PtahSchemaPlanChunk) {
				chunk.Name += "-extra"
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture(operatorv1alpha1.OperationPlan)
			plan, chunks := preparedPlanFixture(t, schema)
			plan.UID = "plan-uid"
			chunk := planChunk(plan, plan.Spec.Chunks[0], chunks[0], "")
			test.mutate(chunk)
			handler := handlerFixture(t, staticJobBuilder{job: expectedJob(schema, schema.Status.ActiveOperation)}, schema, plan)

			response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, chunk))
			if response.Allowed {
				t.Fatal("Handle() allowed an invalid plan chunk")
			}
		})
	}
}

// TestValidationHandlerRefusesAChunkOutsidePublication holds the chunk kind to
// the Plan operation that publishes it. An exact chunk of a real plan is
// still refused while the schema is dispatching an Apply: nothing publishes
// chunks then, and a chunk created then would be SQL nobody computed.
func TestValidationHandlerRefusesAChunkOutsidePublication(t *testing.T) {
	t.Parallel()

	schema, plan, chunks := applyProjectionFixture(t)
	chunk := planChunk(plan, plan.Spec.Chunks[0], chunks[0], "")
	handler := handlerFixture(t, staticJobBuilder{job: expectedJob(schema, schema.Status.ActiveOperation)}, schema, plan)

	response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, chunk))
	if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "active Plan operation") {
		t.Fatalf("Handle() = %#v, want a refusal naming the Plan operation", response.Result)
	}
}

func TestValidationHandlerAllowsExactApplyProjection(t *testing.T) {
	t.Parallel()

	schema, plan, chunks := applyProjectionFixture(t)
	projection := planProjection(plan, plan.Spec.Chunks[0], chunks[0])
	handler := handlerFixture(t, staticJobBuilder{job: expectedJob(schema, schema.Status.ActiveOperation)}, schema, plan)

	response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, projection))
	if !response.Allowed {
		t.Fatalf("Handle() denied an exact Apply projection: %#v", response.Result)
	}
}

func TestValidationHandlerRejectsInvalidApplyProjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*corev1.ConfigMap)
		// state changes what the schema and the plan say instead of the
		// ConfigMap, so the refusal is about when it was written.
		state func(*operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan)
	}{
		{
			name: "payload mismatch",
			mutate: func(configMap *corev1.ConfigMap) {
				configMap.BinaryData[planstore.ProjectionDataKey][0] ^= 0xff
			},
		},
		{
			name: "extra binary payload",
			mutate: func(configMap *corev1.ConfigMap) {
				configMap.BinaryData["extra"] = []byte("unexpected")
			},
		},
		{
			name: "string data",
			mutate: func(configMap *corev1.ConfigMap) {
				configMap.Data = map[string]string{"extra": "unexpected"}
			},
		},
		{
			name: "mutable",
			mutate: func(configMap *corev1.ConfigMap) {
				value := false
				configMap.Immutable = &value
			},
		},
		{
			name: "wrong label",
			mutate: func(configMap *corev1.ConfigMap) {
				configMap.Labels[planstore.LabelPlan] = "another-plan"
			},
		},
		{
			name: "wrong owner",
			mutate: func(configMap *corev1.ConfigMap) {
				configMap.OwnerReferences[0].UID = "another-plan"
			},
		},
		{
			name: "during publication",
			state: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PtahSchemaPlan) {
				schema.Status.ActiveOperation.Type = operatorv1alpha1.OperationPlan
			},
		},
		{
			name: "after dispatch started",
			state: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PtahSchemaPlan) {
				schema.Status.ActiveOperation.DispatchStarted = true
			},
		},
		{
			name: "after the Job exists",
			state: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PtahSchemaPlan) {
				schema.Status.ActiveOperation.JobUID = "apply-job-uid"
			},
		},
		{
			name: "of a plan that is not current",
			state: func(schema *operatorv1alpha1.PtahSchema, _ *operatorv1alpha1.PtahSchemaPlan) {
				schema.Status.Plan.UID = "another-plan-uid"
			},
		},
		{
			name: "of uncommitted storage",
			state: func(_ *operatorv1alpha1.PtahSchema, plan *operatorv1alpha1.PtahSchemaPlan) {
				plan.Status.Conditions = nil
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema, plan, chunks := applyProjectionFixture(t)
			projection := planProjection(plan, plan.Spec.Chunks[0], chunks[0])
			if test.mutate != nil {
				test.mutate(projection)
			}
			if test.state != nil {
				test.state(schema, plan)
			}
			handler := handlerFixture(t, staticJobBuilder{job: expectedJob(schema, schema.Status.ActiveOperation)}, schema, plan)

			response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, projection))
			if response.Allowed {
				t.Fatal("Handle() allowed an invalid Apply projection")
			}
		})
	}
}

func TestValidationHandlerReadsAndValidatesApplyProjection(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name              string
		corruptProjection bool
		// dropProjection leaves the Pod nothing to mount.
		dropProjection bool
		// dropCommit leaves the plan without the record that its chunks were
		// written.
		dropCommit  bool
		wantAllowed bool
		wantMessage string
	}{
		{name: "exact projection", wantAllowed: true},
		{name: "corrupt projection", corruptProjection: true, wantMessage: "projection 0 is invalid"},
		{
			// Admitting an Apply whose plan cannot be mounted would leave an
			// executor waiting on a volume nobody writes.
			name:           "projection is missing",
			dropProjection: true,
			wantMessage:    "directly read Apply plan projection",
		},
		{
			name:        "storage was never committed",
			dropCommit:  true,
			wantMessage: "does not bind every published chunk",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema, plan, chunks := applyProjectionFixture(t)
			projection := planProjection(plan, plan.Spec.Chunks[0], chunks[0])
			expected := expectedJob(schema, schema.Status.ActiveOperation)
			candidate := withGeneratedJobIdentity(expected)
			if test.corruptProjection {
				projection.BinaryData[planstore.ProjectionDataKey][0] ^= 0xff
			}
			if test.dropCommit {
				plan.Status.PublishedChunks = nil
			}
			objects := []client.Object{schema, plan, projection}
			if test.dropProjection {
				objects = objects[:2]
			}
			handler := handlerFixture(t, staticJobBuilder{job: expected}, objects...)

			response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, candidate))
			if response.Allowed != test.wantAllowed {
				t.Fatalf("Handle() allowed = %t, want %t; result = %#v", response.Allowed, test.wantAllowed, response.Result)
			}
			if test.wantMessage != "" {
				if response.Result == nil || !strings.Contains(response.Result.Message, test.wantMessage) {
					t.Fatalf("refusal = %#v, want one naming %q", response.Result, test.wantMessage)
				}
			}
		})
	}
}

func TestValidationHandlerReadsMaximumApplyPlanChunksConcurrently(t *testing.T) {
	t.Parallel()

	schema := schemaFixture(operatorv1alpha1.OperationPlan)
	content := bytes.Repeat([]byte{'p'}, int(plancontract.MaxExecutableBytes))
	plan, chunks := preparedPlanFixtureWithContent(t, schema, content)
	plan.UID = "plan-uid"
	plan.Generation = 1
	plan.Status.ObservedGeneration = plan.Generation
	plan.Status.Conditions = []metav1.Condition{{
		Type: operatorv1alpha1.ConditionPlanStorageReady, Status: metav1.ConditionTrue,
		ObservedGeneration: plan.Generation,
	}}
	objects := []client.Object{schema, plan}
	for index, ref := range plan.Spec.Chunks {
		plan.Status.PublishedChunks = append(plan.Status.PublishedChunks, operatorv1alpha1.PublishedPlanChunkStatus{
			Name: ref.Name, UID: types.UID(ref.Name + "-uid"), Index: int32(index),
		})
		objects = append(objects, planProjection(plan, ref, chunks[index]))
	}
	if len(plan.Spec.Chunks) != plancontract.MaxChunks {
		t.Fatalf("plan chunks = %d, want contract maximum %d", len(plan.Spec.Chunks), plancontract.MaxChunks)
	}
	schema.Status.Plan = currentPlan(plan)
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationApply, ID: "operation-id", JobName: "ptah-apply-orders",
		ExecutionBindingID: schema.Status.ExecutionBinding.Epoch,
	}
	expected := expectedJob(schema, schema.Status.ActiveOperation)
	candidate := withGeneratedJobIdentity(expected)

	scheme := controllerWriteScheme(t)
	baseReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	reader := &barrierChunkReader{Reader: baseReader, release: make(chan struct{})}
	handler := handlerWithReader(staticJobBuilder{job: expected}, reader)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	response := handler.Handle(ctx, requestFor(t, admissionv1.Create, candidate))
	if !response.Allowed {
		t.Fatalf("Handle() denied exact maximum-chunk Apply Job: %#v", response.Result)
	}
	if got := reader.maximumConcurrency(); got != plancontract.MaxChunks {
		t.Fatalf("maximum concurrent chunk reads = %d, want %d", got, plancontract.MaxChunks)
	}
}

func TestValidationHandlerFailsClosedOnIdentityRoutingAndReadErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutate     func(*cradmission.Request)
		noSchema   bool
		statusCode int32
	}{
		{
			name: "unexpected username",
			mutate: func(request *cradmission.Request) {
				request.UserInfo.Username = "system:serviceaccount:other:controller"
			},
		},
		{
			name: "unexpected resource",
			mutate: func(request *cradmission.Request) {
				request.Resource.Resource = "secrets"
			},
		},
		{
			name: "subresource",
			mutate: func(request *cradmission.Request) {
				request.SubResource = "status"
			},
		},
		{
			name: "missing request UID",
			mutate: func(request *cradmission.Request) {
				request.UID = ""
			},
			statusCode: 400,
		},
		{
			name: "malformed candidate",
			mutate: func(request *cradmission.Request) {
				request.Object.Raw = []byte("{")
			},
			statusCode: 400,
		},
		{
			name: "direct read failure",
			mutate: func(_ *cradmission.Request) {
			},
			noSchema: true, statusCode: 500,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture(operatorv1alpha1.OperationResolve)
			expected := expectedJob(schema, schema.Status.ActiveOperation)
			objects := []client.Object(nil)
			if !test.noSchema {
				objects = append(objects, schema)
			}
			handler := handlerFixture(t, staticJobBuilder{job: expected}, objects...)
			request := requestFor(t, admissionv1.Create, withGeneratedJobIdentity(expected))
			test.mutate(&request)

			response := handler.Handle(context.Background(), request)
			if response.Allowed {
				t.Fatal("Handle() allowed a request outside the exact routing or read contract")
			}
			if test.statusCode != 0 && (response.Result == nil || response.Result.Code != test.statusCode) {
				t.Fatalf("Handle() status = %#v, want %d", response.Result, test.statusCode)
			}
		})
	}
}

func TestValidationHandlerRejectsUnsupportedOperations(t *testing.T) {
	t.Parallel()

	for _, operation := range []admissionv1.Operation{admissionv1.Delete, admissionv1.Connect} {
		operation := operation
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture(operatorv1alpha1.OperationResolve)
			expected := expectedJob(schema, schema.Status.ActiveOperation)
			handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)
			response := handler.Handle(context.Background(), requestFor(t, operation, expected))
			if response.Allowed {
				t.Fatalf("Handle() allowed an operator manager %s outside its write contract", operation)
			}
		})
	}
}

func TestValidationHandlerRejectsPlanAndChunkMutation(t *testing.T) {
	t.Parallel()

	schema := schemaFixture(operatorv1alpha1.OperationPlan)
	plan, chunks := preparedPlanFixture(t, schema)
	plan.UID = "plan-uid"
	chunk := planChunk(plan, plan.Spec.Chunks[0], chunks[0], "chunk-uid")
	projection := planProjection(plan, plan.Spec.Chunks[0], chunks[0])
	handler := handlerFixture(t, staticJobBuilder{job: expectedJob(schema, schema.Status.ActiveOperation)}, schema, plan)

	for _, test := range []struct {
		name   string
		object client.Object
	}{
		{name: "plan update", object: plan},
		{name: "chunk update", object: chunk},
		{name: "projection update", object: projection},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			response := handler.Handle(context.Background(), requestFor(t, admissionv1.Update, test.object))
			if response.Allowed {
				t.Fatal("Handle() allowed mutation of immutable plan storage")
			}
		})
	}
}

func TestValidationHandlerRejectsInvalidAdmissionSnapshot(t *testing.T) {
	t.Parallel()

	schema := schemaFixture(operatorv1alpha1.OperationResolve)
	expected := expectedJob(schema, schema.Status.ActiveOperation)
	candidate := withGeneratedJobIdentity(expected)
	schema.Status.ActiveOperation.AdmissionSnapshot.Digest = digest('f')
	handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)

	response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, candidate))
	if response.Allowed {
		t.Fatal("Handle() allowed a Job with an invalid durable Pod admission snapshot")
	}
}

func TestValidationHandlerFailsClosedWhenBuilderCannotReconstructJob(t *testing.T) {
	t.Parallel()

	schema := schemaFixture(operatorv1alpha1.OperationResolve)
	expected := expectedJob(schema, schema.Status.ActiveOperation)
	handler := handlerFixture(t, staticJobBuilder{job: expected, err: errors.New("stale binding")}, schema)

	response := handler.Handle(context.Background(), requestFor(t, admissionv1.Create, expected))
	if response.Allowed {
		t.Fatal("Handle() allowed a Job that the configured builder could not reconstruct")
	}
}

func handlerFixture(t *testing.T, jobs controllerwrite.JobBuilder, objects ...client.Object) *controllerwrite.ValidationHandler {
	t.Helper()

	scheme := controllerWriteScheme(t)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return handlerWithReader(jobs, reader)
}

func controllerWriteScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func handlerWithReader(jobs controllerwrite.JobBuilder, reader client.Reader) *controllerwrite.ValidationHandler {
	return &controllerwrite.ValidationHandler{Validator: &controllerwrite.Validator{
		Reader: reader, Jobs: jobs, ManagerUsername: managerUsername,
	}}
}

func planManifestHandlerFixture(
	t *testing.T,
	schema *operatorv1alpha1.PtahSchema,
) *controllerwrite.ValidationHandler {
	t.Helper()

	expected := expectedJob(schema, schema.Status.ActiveOperation)
	terminal := withGeneratedJobIdentity(expected)
	terminal.UID = schema.Status.ActiveOperation.JobUID
	terminal.Spec.Selector.MatchLabels[batchv1.ControllerUidLabel] = string(terminal.UID)
	terminal.Spec.Template.Labels[batchv1.ControllerUidLabel] = string(terminal.UID)
	ttl := int32(300)
	terminal.Spec.TTLSecondsAfterFinished = &ttl
	terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	return handlerFixture(t, staticJobBuilder{job: expected}, schema, terminal)
}

func requestFor(t *testing.T, operation admissionv1.Operation, object client.Object) cradmission.Request {
	t.Helper()

	// The API server assigns a created object's UID before validating
	// admission sees it (FillObjectMetaSystemFields runs ahead of
	// createValidation in the generic registry store), so a create request
	// never carries an object without one.
	if operation == admissionv1.Create && object.GetUID() == "" {
		object = object.DeepCopyObject().(client.Object)
		object.SetUID("created-object-uid")
	}
	request := cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "request-uid",
		Name:      object.GetName(),
		Namespace: object.GetNamespace(),
		Operation: operation,
		UserInfo:  authenticationv1.UserInfo{Username: managerUsername},
		Object:    rawObject(t, object),
	}}
	switch object.(type) {
	case *batchv1.Job:
		request.Resource = metav1.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
		request.Kind = metav1.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}
	case *corev1.ConfigMap:
		request.Resource = metav1.GroupVersionResource{Version: "v1", Resource: "configmaps"}
		request.Kind = metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	case *operatorv1alpha1.PtahSchemaPlanChunk:
		request.Resource = metav1.GroupVersionResource{Group: operatorv1alpha1.GroupVersion.Group, Version: "v1alpha1", Resource: "ptahschemaplanchunks"}
		request.Kind = metav1.GroupVersionKind{Group: operatorv1alpha1.GroupVersion.Group, Version: "v1alpha1", Kind: "PtahSchemaPlanChunk"}
	case *operatorv1alpha1.PtahSchemaPlan:
		request.Resource = metav1.GroupVersionResource{Group: operatorv1alpha1.GroupVersion.Group, Version: "v1alpha1", Resource: "ptahschemaplans"}
		request.Kind = metav1.GroupVersionKind{Group: operatorv1alpha1.GroupVersion.Group, Version: "v1alpha1", Kind: "PtahSchemaPlan"}
	default:
		t.Fatalf("unsupported test object %T", object)
	}
	return request
}

func rawObject(t *testing.T, object any) runtime.RawExtension {
	t.Helper()

	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return runtime.RawExtension{Raw: data}
}

func schemaFixture(operationType operatorv1alpha1.OperationType) *operatorv1alpha1.PtahSchema {
	schema := &operatorv1alpha1.PtahSchema{
		TypeMeta: metav1.TypeMeta{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahSchema"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant-a", Name: "orders", UID: "schema-uid",
		},
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{Engine: operatorv1alpha1.DatabaseEnginePostgreSQL},
			Execution: operatorv1alpha1.ExecutionSpec{
				ServiceAccountName: "ptah-orders",
			},
		},
		Status: operatorv1alpha1.PtahSchemaStatus{
			ExecutionBinding: &operatorv1alpha1.ExecutionBindingStatus{
				Epoch:                  "v1-11111111111111111111111111111111",
				ControllerStateVersion: 1,
				PtahVersion:            "v0.3.0",
				ExecutorImage:          "example.test/executor@" + digest('2'),
				RunnerProtocolVersion:  int32(runner.ProtocolVersion),
			},
			Source: operatorv1alpha1.SchemaSourceStatus{
				Digest:                   digest('4'),
				Verified:                 true,
				VerificationPolicyUID:    "policy-uid",
				VerificationPolicyDigest: digest('5'),
			},
			Target: operatorv1alpha1.TargetStatus{
				CoordinationDigest: digest('6'),
				IdentityDigest:     digest('7'),
			},
		},
	}
	operation := &operatorv1alpha1.ActiveOperationStatus{
		Type:               operationType,
		ID:                 "operation-id",
		InputFingerprint:   digest('8'),
		JobName:            "ptah-" + strings.ToLower(string(operationType)) + "-orders",
		ExecutionBindingID: schema.Status.ExecutionBinding.Epoch,
	}
	if operationType == operatorv1alpha1.OperationPlan {
		operation.JobUID = "job-uid"
		operation.CoordinationDigest = schema.Status.Target.CoordinationDigest
		operation.TargetIdentityDigest = schema.Status.Target.IdentityDigest
		operation.Source = &operatorv1alpha1.OCIArtifactAccessBinding{Digest: schema.Status.Source.Digest}
		operation.PlanSealPublicKeyDigest = testPlanSealPublicKeyDigest()
	}
	schema.Status.ActiveOperation = operation
	return schema
}

// testSealKey is the one key pair schemaFixture's Plan claims are recorded
// against and expectedJob's Plan Jobs are built with, so a test that wants
// two disagreeing sides -- the shape a live replica pair actually produces --
// generates and uses its own key instead of this one.
var testSealKey = mustGenerateTestSealKey()

func mustGenerateTestSealKey() planseal.KeyPair {
	key, err := planseal.Generate()
	if err != nil {
		panic(err)
	}
	return key
}

func testPlanSealPublicKeyDigest() string {
	return fingerprint.DigestBytes([]byte(testSealKey.PublicKey().Encode()))
}

// planOperationEnv is the one environment variable expectedJob's Plan Jobs
// need for CarrySealedPlanKey to have anything to check: every other
// operation type carries none, matching the real Builder.
func planOperationEnv(operationType operatorv1alpha1.OperationType) []corev1.EnvVar {
	if operationType != operatorv1alpha1.OperationPlan {
		return nil
	}
	return []corev1.EnvVar{{Name: runner.EnvPlanSealPublicKey, Value: testSealKey.PublicKey().Encode()}}
}

func expectedJob(
	schema *operatorv1alpha1.PtahSchema,
	operation *operatorv1alpha1.ActiveOperationStatus,
) *batchv1.Job {
	controller := true
	blockDeletion := true
	labels := map[string]string{}
	annotations := map[string]string{}
	// What spec.execution.podMetadata declares sits under the operator's
	// own, as the builder writes it.
	if declared := schema.Spec.Execution.PodMetadata; declared != nil {
		for key, value := range declared.Labels {
			labels[key] = string(value)
		}
		for key, value := range declared.Annotations {
			annotations[key] = string(value)
		}
	}
	for key, value := range map[string]string{
		workload.LabelManagedBy:   "ptah-operator",
		workload.LabelComponent:   "schema-operation",
		workload.LabelSchema:      schema.Name,
		workload.LabelOperation:   strings.ToLower(string(operation.Type)),
		workload.LabelOperationID: workload.OperationIDLabelValue(operation.ID),
	} {
		labels[key] = value
	}
	annotations[workload.AnnotationOperationID] = operation.ID
	annotations[workload.AnnotationExecutionBindingID] = operation.ExecutionBindingID
	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   schema.Namespace,
			Name:        operation.JobName,
			Labels:      copyStringMap(labels),
			Annotations: copyStringMap(annotations),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahSchema",
				Name: schema.Name, UID: schema.UID,
				Controller: &controller, BlockOwnerDeletion: &blockDeletion,
			}},
		},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      copyStringMap(labels),
				Annotations: copyStringMap(annotations),
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: schema.Spec.Execution.ServiceAccountName,
				RestartPolicy:      corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name: "ptah", Image: "example.test/executor@" + digest('2'), Command: []string{"/runner/ptah-runner"},
					Env: planOperationEnv(operation.Type),
				}},
			},
		}},
	}
	templateDigest, err := podintent.DigestTemplate(&job.Spec.Template)
	if err != nil {
		panic(err)
	}
	snapshot := &operatorv1alpha1.PodAdmissionSnapshot{
		Version:        podintent.SnapshotVersion,
		TemplateDigest: templateDigest,
		ServiceAccount: operatorv1alpha1.ServiceAccountAdmissionSnapshot{
			Object: operatorv1alpha1.AdmissionObjectBinding{
				Name: schema.Spec.Execution.ServiceAccountName, UID: "service-account-uid", ResourceVersion: "1",
			},
		},
	}
	snapshotDigest, err := fingerprint.DigestCanonicalJSON(*snapshot)
	if err != nil {
		panic(err)
	}
	snapshot.Digest = snapshotDigest
	operation.AdmissionSnapshot = snapshot
	job.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshot.Digest
	job.Spec.Template.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshot.Digest
	return job
}

func currentCleanupFixture(
	t *testing.T,
	operationType operatorv1alpha1.OperationType,
) (*operatorv1alpha1.PtahSchema, *batchv1.Job, *batchv1.Job, *batchv1.Job) {
	t.Helper()
	return currentCleanupFixtureWith(t, operationType, nil)
}

// currentCleanupFixtureWith is currentCleanupFixture with the schema's spec
// changed by declare before the Job the claim pins is built from it.
func currentCleanupFixtureWith(
	t *testing.T,
	operationType operatorv1alpha1.OperationType,
	declare func(*operatorv1alpha1.PtahSchema),
) (*operatorv1alpha1.PtahSchema, *batchv1.Job, *batchv1.Job, *batchv1.Job) {
	t.Helper()

	schema := schemaFixture(operationType)
	if declare != nil {
		declare(schema)
	}
	operation := schema.Status.ActiveOperation
	operation.Attempt = 1
	operation.JobUID = ""
	jobName, err := workload.NameFor(schema, *operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.JobName = jobName
	expected := expectedJob(schema, operation)
	binding := schema.Status.ExecutionBinding
	annotations := map[string]string{
		workload.AnnotationOperationID:             operation.ID,
		workload.AnnotationInputFingerprint:        operation.InputFingerprint,
		workload.AnnotationPtahVersion:             binding.PtahVersion,
		workload.AnnotationExecutionBindingID:      operation.ExecutionBindingID,
		workload.AnnotationControllerImage:         testControllerImage,
		workload.AnnotationControllerRevision:      testControllerRevision,
		workload.AnnotationControllerStateVersion:  "1",
		workload.AnnotationAdmissionSnapshotDigest: operation.AdmissionSnapshot.Digest,
	}
	if operationType == operatorv1alpha1.OperationApply {
		schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
			Name:                   "plan-current",
			UID:                    "plan-uid",
			Fingerprint:            digest('a'),
			ContentDigest:          digest('b'),
			ExecutionBindingID:     operation.ExecutionBindingID,
			ControllerImage:        testControllerImage,
			ControllerRevision:     testControllerRevision,
			ControllerStateVersion: binding.ControllerStateVersion,
			PtahVersion:            binding.PtahVersion,
		}
		annotations[workload.AnnotationPlanFingerprint] = schema.Status.Plan.Fingerprint
		annotations[workload.AnnotationPlanContentDigest] = schema.Status.Plan.ContentDigest
		workload.MarkMutatingOperation(annotations)
	}
	if declared := schema.Spec.Execution.PodMetadata; declared != nil {
		for key, value := range declared.Annotations {
			annotations[key] = string(value)
		}
	}
	expected.Annotations = copyStringMap(annotations)
	expected.Spec.Template.Annotations = copyStringMap(annotations)

	templateDigest, err := podintent.DigestTemplate(&expected.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	operation.AdmissionSnapshot.TemplateDigest = templateDigest
	operation.AdmissionSnapshot.Digest = ""
	snapshotDigest, err := fingerprint.DigestCanonicalJSON(*operation.AdmissionSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	operation.AdmissionSnapshot.Digest = snapshotDigest
	expected.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshotDigest
	expected.Spec.Template.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshotDigest

	oldJob := withGeneratedJobIdentity(expected)
	oldJob.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
	}}
	operation.JobUID = oldJob.UID
	job := oldJob.DeepCopy()
	ttl := int32(300)
	job.Spec.TTLSecondsAfterFinished = &ttl
	return schema, expected, oldJob, job
}

func pendingApplyCleanupFixture(
	t *testing.T,
) (*operatorv1alpha1.PtahSchema, *batchv1.Job, *batchv1.Job) {
	t.Helper()

	schema, _, oldJob, job := currentCleanupFixture(t, operatorv1alpha1.OperationApply)
	operation := schema.Status.ActiveOperation.DeepCopy()
	plan := *schema.Status.Plan.DeepCopy()
	schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
		Outcome:           operatorv1alpha1.PendingObservationOutcomeUnknown,
		ApplyOperationID:  operation.ID,
		ApplyJobName:      operation.JobName,
		ApplyJobUID:       operation.JobUID,
		AdmissionSnapshot: operation.AdmissionSnapshot.DeepCopy(),
		Plan:              plan,
	}
	schema.Status.ActiveOperation = nil
	schema.Status.ExecutionBinding.Epoch = "v1-99999999999999999999999999999999"
	schema.Status.ExecutionBinding.ControllerStateVersion = 2
	schema.Status.ExecutionBinding.PtahVersion = "v0.4.0"
	setExecutionBindingRetirementFence(schema)
	return schema, oldJob, job
}

// setExecutionBindingRetirementFence writes the record a rotation leaves
// behind: the epoch it retired, and the Job the retired claim dispatched as
// that claim names it. Neither the phase nor any condition is set, because
// neither is what the cleanup is authorized by.
func setExecutionBindingRetirementFence(schema *operatorv1alpha1.PtahSchema) {
	retirement := &operatorv1alpha1.BindingRetirementStatus{}
	if operation := schema.Status.ActiveOperation; operation != nil {
		retirement.RetiredEpoch = operation.ExecutionBindingID
		retirement.Job = &operatorv1alpha1.RetiredJobStatus{
			Operation: operation.Type, Name: operation.JobName, UID: operation.JobUID,
		}
	} else if pending := schema.Status.PendingObservation; pending != nil {
		retirement.RetiredEpoch = pending.Plan.ExecutionBindingID
		retirement.Job = &operatorv1alpha1.RetiredJobStatus{
			Operation: operatorv1alpha1.OperationApply, Name: pending.ApplyJobName, UID: pending.ApplyJobUID,
		}
	}
	schema.Status.PendingBindingRetirement = retirement
}

func withGeneratedJobIdentity(expected *batchv1.Job) *batchv1.Job {
	job := expected.DeepCopy()
	job.UID = "job-uid"
	job.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{
		batchv1.ControllerUidLabel: string(job.UID),
	}}
	job.Spec.Template.Labels[batchv1.ControllerUidLabel] = string(job.UID)
	job.Spec.Template.Labels[batchv1.JobNameLabel] = job.Name
	return job
}

func preparedPlanFixture(
	t *testing.T,
	schema *operatorv1alpha1.PtahSchema,
) (*operatorv1alpha1.PtahSchemaPlan, [][]byte) {
	t.Helper()

	content := []byte("CREATE TABLE orders (id bigint PRIMARY KEY);\n")
	return preparedPlanFixtureWithContent(t, schema, content)
}

func recomputePlanFingerprint(
	t *testing.T,
	schema *operatorv1alpha1.PtahSchema,
	plan *operatorv1alpha1.PtahSchemaPlan,
) string {
	t.Helper()

	binding := fingerprint.PlanBinding{
		ContractVersion:          plan.Spec.ContractVersion,
		SchemaUID:                string(schema.UID),
		PlanContentDigest:        plan.Spec.ContentDigest,
		ArtifactDigest:           plan.Spec.ArtifactDigest,
		CoordinationDigest:       plan.Spec.CoordinationDigest,
		TargetIdentityDigest:     plan.Spec.TargetIdentityDigest,
		ActualStateFingerprint:   plan.Spec.ActualStateFingerprint,
		DesiredStateFingerprint:  plan.Spec.DesiredStateFingerprint,
		PolicyFingerprint:        plan.Spec.PolicyFingerprint,
		VerificationPolicyUID:    string(plan.Spec.VerificationPolicyUID),
		VerificationPolicyDigest: plan.Spec.VerificationPolicyDigest,
		ExecutionBindingID:       plan.Spec.ExecutionBindingID,
		ControllerStateVersion:   plan.Spec.ControllerStateVersion,
		PtahVersion:              plan.Spec.PtahVersion,
		ExecutorImage:            plan.Spec.ExecutorImage,
		RunnerProtocolVersion:    plan.Spec.RunnerProtocolVersion,
		Destructive:              plan.Spec.Destructive,
		StatementCount:           plan.Spec.StatementCount,
	}
	for _, kind := range plan.Spec.PrivilegeChanges {
		binding.PrivilegeChanges = append(binding.PrivilegeChanges, string(kind))
	}
	value, err := binding.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func preparedPlanFixtureWithContent(
	t *testing.T,
	schema *operatorv1alpha1.PtahSchema,
	content []byte,
) (*operatorv1alpha1.PtahSchemaPlan, [][]byte) {
	t.Helper()

	// The admission side's own function, rather than a copy of it: a fixture
	// that spells the policy a second time agrees with nothing the day the
	// policy grows a field, which is exactly what happened when it did.
	policyDigest, err := controllerwrite.SchemaPolicyFingerprint(schema)
	if err != nil {
		t.Fatal(err)
	}
	binding := fingerprint.PlanBinding{
		ContractVersion:          fingerprint.CurrentPlanContractVersion,
		SchemaUID:                string(schema.UID),
		PlanContentDigest:        fingerprint.DigestBytes(content),
		ArtifactDigest:           schema.Status.Source.Digest,
		CoordinationDigest:       schema.Status.Target.CoordinationDigest,
		TargetIdentityDigest:     schema.Status.Target.IdentityDigest,
		ActualStateFingerprint:   digest('a'),
		DesiredStateFingerprint:  digest('b'),
		PolicyFingerprint:        policyDigest,
		VerificationPolicyUID:    string(schema.Status.Source.VerificationPolicyUID),
		VerificationPolicyDigest: schema.Status.Source.VerificationPolicyDigest,
		ExecutionBindingID:       schema.Status.ExecutionBinding.Epoch,
		ControllerStateVersion:   schema.Status.ExecutionBinding.ControllerStateVersion,
		PtahVersion:              schema.Status.ExecutionBinding.PtahVersion,
		ExecutorImage:            schema.Status.ExecutionBinding.ExecutorImage,
		RunnerProtocolVersion:    schema.Status.ExecutionBinding.RunnerProtocolVersion,
		StatementCount:           1,
	}
	planFingerprint, err := binding.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	plan, chunks, err := planstore.Prepare(schema, operatorv1alpha1.PtahSchemaPlanSpec{
		ContractVersion:          binding.ContractVersion,
		SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
		Fingerprint:              planFingerprint,
		ArtifactDigest:           binding.ArtifactDigest,
		CoordinationDigest:       binding.CoordinationDigest,
		TargetIdentityDigest:     binding.TargetIdentityDigest,
		ActualStateFingerprint:   binding.ActualStateFingerprint,
		DesiredStateFingerprint:  binding.DesiredStateFingerprint,
		PolicyFingerprint:        binding.PolicyFingerprint,
		VerificationPolicyUID:    types.UID(binding.VerificationPolicyUID),
		VerificationPolicyDigest: binding.VerificationPolicyDigest,
		ExecutionBindingID:       binding.ExecutionBindingID,
		ControllerImage:          testControllerImage,
		ControllerRevision:       testControllerRevision,
		ControllerStateVersion:   binding.ControllerStateVersion,
		PtahVersion:              binding.PtahVersion,
		ExecutorImage:            binding.ExecutorImage,
		RunnerImage:              testRunnerImage,
		RunnerProtocolVersion:    binding.RunnerProtocolVersion,
		Dialect:                  "postgres",
		StatementCount:           binding.StatementCount,
	}, content)
	if err != nil {
		t.Fatal(err)
	}
	return plan, chunks
}

func planChunk(
	plan *operatorv1alpha1.PtahSchemaPlan,
	ref operatorv1alpha1.PlanChunkReference,
	content []byte,
	uid types.UID,
) *operatorv1alpha1.PtahSchemaPlanChunk {
	chunk := planstore.DesiredChunk(plan, ref, content)
	chunk.UID = uid
	return chunk
}

func planProjection(
	plan *operatorv1alpha1.PtahSchemaPlan,
	ref operatorv1alpha1.PlanChunkReference,
	content []byte,
) *corev1.ConfigMap {
	projection := planstore.DesiredProjection(plan, ref, content)
	projection.TypeMeta = metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "ConfigMap"}
	return projection
}

// applyProjectionFixture is the state Project writes a projection in: an Apply
// claim of the schema's committed current plan, before its Job exists and
// before DispatchStarted.
func applyProjectionFixture(t *testing.T) (*operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahSchemaPlan, [][]byte) {
	t.Helper()

	schema := schemaFixture(operatorv1alpha1.OperationPlan)
	plan, chunks := preparedPlanFixture(t, schema)
	plan.UID = "plan-uid"
	plan.Generation = 1
	plan.Status.ObservedGeneration = plan.Generation
	for index, ref := range plan.Spec.Chunks {
		plan.Status.PublishedChunks = append(plan.Status.PublishedChunks, operatorv1alpha1.PublishedPlanChunkStatus{
			Name: ref.Name, UID: types.UID(ref.Name + "-uid"), Index: int32(index),
		})
	}
	plan.Status.Conditions = []metav1.Condition{{
		Type: operatorv1alpha1.ConditionPlanStorageReady, Status: metav1.ConditionTrue,
		ObservedGeneration: plan.Generation,
	}}
	schema.Status.Plan = currentPlan(plan)
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationApply, ID: "operation-id", JobName: "ptah-apply-orders",
		ExecutionBindingID: schema.Status.ExecutionBinding.Epoch,
	}
	return schema, plan, chunks
}

func currentPlan(plan *operatorv1alpha1.PtahSchemaPlan) *operatorv1alpha1.CurrentPlanStatus {
	return &operatorv1alpha1.CurrentPlanStatus{
		Name:                     plan.Name,
		UID:                      plan.UID,
		Fingerprint:              plan.Spec.Fingerprint,
		ContentDigest:            plan.Spec.ContentDigest,
		ArtifactDigest:           plan.Spec.ArtifactDigest,
		CoordinationDigest:       plan.Spec.CoordinationDigest,
		TargetIdentityDigest:     plan.Spec.TargetIdentityDigest,
		ActualStateFingerprint:   plan.Spec.ActualStateFingerprint,
		DesiredStateFingerprint:  plan.Spec.DesiredStateFingerprint,
		PolicyFingerprint:        plan.Spec.PolicyFingerprint,
		VerificationPolicyUID:    plan.Spec.VerificationPolicyUID,
		VerificationPolicyDigest: plan.Spec.VerificationPolicyDigest,
		ExecutionBindingID:       plan.Spec.ExecutionBindingID,
		ControllerImage:          plan.Spec.ControllerImage,
		ControllerRevision:       plan.Spec.ControllerRevision,
		ControllerStateVersion:   plan.Spec.ControllerStateVersion,
		PtahVersion:              plan.Spec.PtahVersion,
		ExecutorImage:            plan.Spec.ExecutorImage,
		RunnerImage:              plan.Spec.RunnerImage,
		RunnerProtocolVersion:    plan.Spec.RunnerProtocolVersion,
		Destructive:              plan.Spec.Destructive,
		StatementCount:           plan.Spec.StatementCount,
	}
}

func copyStringMap(values map[string]string) map[string]string {
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}

func digest(char byte) string { return "sha256:" + strings.Repeat(string(char), 64) }
