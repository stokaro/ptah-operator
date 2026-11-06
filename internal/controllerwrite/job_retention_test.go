package controllerwrite_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

var (
	applyComplete = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	applyFailed   = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
)

// summaryOf is what a runner leaves in its termination message.
func summaryOf(t *testing.T, result runner.Result) string {
	t.Helper()
	result.ProtocolVersion = runner.ProtocolVersion
	result.CoordinationDigest = digest('c')
	result.TargetIdentityDigest = digest('d')
	encoded, err := runner.EncodeSummary(result)
	if err != nil {
		t.Fatalf("EncodeSummary() error = %v", err)
	}
	return string(encoded)
}

// jobPod is a Pod the Job with this name and UID owns, terminated with message.
func jobPod(name string, job *batchv1.Job, owner types.UID, message string) *corev1.Pod {
	controller := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: job.Namespace, Name: name, Labels: map[string]string{"job-name": job.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: owner, Controller: &controller}},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "ptah", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
		}}},
	}
}

// TestTheLongTTLBelongsToAnApplyThatDidNotSucceed holds the choice on a schema
// Apply Job to what its Pods say. Only a run that reported success, by itself,
// is refused: a failed Apply usually leaves a Complete Job, and silence is what
// an unknown outcome looks like.
func TestTheLongTTLBelongsToAnApplyThatDidNotSucceed(t *testing.T) {
	t.Parallel()

	long, short := jobclaim.FailedApplyRetentionTTLSeconds, jobclaim.CleanupTTLSeconds
	const refusesSuccess = "this run reported success; it keeps 300 seconds"
	for _, test := range []struct {
		name       string
		ttl        int32
		conditions []batchv1.JobCondition
		pods       func(t *testing.T, job *batchv1.Job, id string) []client.Object
		message    string
	}{
		{name: "a Job Kubernetes failed", ttl: long, conditions: applyFailed},
		{
			// A Pod's report is read only of a Job that completed: this one's other
			// Pod may have failed, and no one Pod speaks for the Job.
			name: "a Job Kubernetes failed whose Pod reported success", ttl: long, conditions: applyFailed,
			pods: func(t *testing.T, job *batchv1.Job, id string) []client.Object {
				return []client.Object{jobPod("a", job, job.UID, summaryOf(t, runner.Result{
					Operation: runner.OperationApply, OperationID: id, MutationStarted: true,
				}))}
			},
		},
		{
			// The hash on the Job's label is not the ID the runner wrote, so a
			// summary naming it does not read for this operation.
			name: "a summary naming the label's hash", ttl: long, conditions: applyComplete,
			pods: func(t *testing.T, job *batchv1.Job, _ string) []client.Object {
				return []client.Object{jobPod("a", job, job.UID, summaryOf(t, runner.Result{
					Operation: runner.OperationApply, OperationID: job.Labels[workload.LabelOperationID], MutationStarted: true,
				}))}
			},
		},
		// Losing database lock continuity retires a running Apply.
		{name: "a running Job", ttl: long},
		{name: "a complete Job with no Pod left", ttl: long, conditions: applyComplete},
		{
			name: "a run that reported an error", ttl: long, conditions: applyComplete,
			pods: func(t *testing.T, job *batchv1.Job, id string) []client.Object {
				return []client.Object{jobPod("a", job, job.UID, summaryOf(t, runner.Result{
					Operation: runner.OperationApply, OperationID: id, ChildExitCode: 2, MutationStarted: true, Uncertain: true,
					Error: &runner.ResultError{Code: "apply_outcome_unknown", Message: "unknown"},
				}))}
			},
		},
		{
			name: "a Pod that left no summary", ttl: long, conditions: applyComplete,
			pods: func(_ *testing.T, job *batchv1.Job, _ string) []client.Object {
				return []client.Object{jobPod("a", job, job.UID, "")}
			},
		},
		{
			name: "a summary of another operation", ttl: long, conditions: applyComplete,
			pods: func(t *testing.T, job *batchv1.Job, _ string) []client.Object {
				return []client.Object{jobPod("a", job, job.UID, summaryOf(t, runner.Result{
					Operation: runner.OperationApply, OperationID: "another-operation", MutationStarted: true,
				}))}
			},
		},
		{
			// No one Pod's report speaks for another.
			name: "two Pods that both reported success", ttl: long, conditions: applyComplete,
			pods: func(t *testing.T, job *batchv1.Job, id string) []client.Object {
				success := summaryOf(t, runner.Result{Operation: runner.OperationApply, OperationID: id, MutationStarted: true})
				return []client.Object{jobPod("a", job, job.UID, success), jobPod("b", job, job.UID, success)}
			},
		},
		{
			name: "a success another Job's Pod reported", ttl: long, conditions: applyComplete,
			pods: func(t *testing.T, job *batchv1.Job, id string) []client.Object {
				return []client.Object{jobPod("a", job, "another-job", summaryOf(t, runner.Result{
					Operation: runner.OperationApply, OperationID: id, MutationStarted: true,
				}))}
			},
		},
		{
			name: "a run that reported success", ttl: long, conditions: applyComplete, message: refusesSuccess,
			pods: func(t *testing.T, job *batchv1.Job, id string) []client.Object {
				return []client.Object{jobPod("a", job, job.UID, summaryOf(t, runner.Result{
					Operation: runner.OperationApply, OperationID: id, MutationStarted: true,
				}))}
			},
		},
		{
			name: "the short TTL on a run that reported success", ttl: short, conditions: applyComplete,
			pods: func(t *testing.T, job *batchv1.Job, id string) []client.Object {
				return []client.Object{jobPod("a", job, job.UID, summaryOf(t, runner.Result{
					Operation: runner.OperationApply, OperationID: id, MutationStarted: true,
				}))}
			},
		},
		{name: "the short TTL on a failed run", ttl: short, conditions: applyFailed},
		{
			name: "a TTL that is neither", ttl: 600, conditions: applyComplete,
			message: "Job cleanup TTL 600 is neither the 300 seconds every finished Job keeps nor the 86400 seconds",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema, expected, oldJob, job := currentCleanupFixture(t, operatorv1alpha1.OperationApply)
			schema.Status.Plan.Approval = consumedApproval()
			oldJob.Status.Conditions = test.conditions
			job.Status.Conditions = test.conditions
			job.Spec.TTLSecondsAfterFinished = &test.ttl
			objects := []client.Object{schema}
			if test.pods != nil {
				// What the runner writes is the full ID, the annotation's. The
				// label holds a hash of it.
				objects = append(objects, test.pods(t, oldJob, oldJob.Annotations[workload.AnnotationOperationID])...)
			}
			handler := handlerFixture(t, staticJobBuilder{job: expected}, objects...)
			request := requestFor(t, admissionv1.Update, job)
			request.OldObject = rawObject(t, oldJob)
			response := handler.Handle(context.Background(), request)
			if test.message == "" {
				if !response.Allowed {
					t.Fatalf("the cleanup TTL %d was denied: %s", test.ttl, responseMessage(response))
				}
				return
			}
			if response.Allowed || !strings.Contains(responseMessage(response), test.message) {
				t.Fatalf("response %v %q, want a denial naming %q", response.Allowed, responseMessage(response), test.message)
			}
		})
	}
}

// TestTheLongTTLIsNotForAReadOnlyJob refuses it on every read-only operation,
// in both families, whatever the Job's status.
func TestTheLongTTLIsNotForAReadOnlyJob(t *testing.T) {
	t.Parallel()

	long := jobclaim.FailedApplyRetentionTTLSeconds
	const want = "Job cleanup TTL 86400 is for the Job of an Apply that failed"
	for _, operationType := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationResolve, operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan,
	} {
		t.Run("schema "+string(operationType), func(t *testing.T) {
			t.Parallel()
			schema, expected, oldJob, job := currentCleanupFixture(t, operationType)
			job.Spec.TTLSecondsAfterFinished = &long
			handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)
			request := requestFor(t, admissionv1.Update, job)
			request.OldObject = rawObject(t, oldJob)
			response := handler.Handle(context.Background(), request)
			if response.Allowed || !strings.Contains(responseMessage(response), want) {
				t.Fatalf("response %v %q, want a denial naming %q", response.Allowed, responseMessage(response), want)
			}
		})
	}
	for _, operationType := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationResolve, operatorv1alpha1.MigrationOperationVerify,
		operatorv1alpha1.MigrationOperationHistory,
	} {
		t.Run("migration "+string(operationType), func(t *testing.T) {
			t.Parallel()
			migration, job := migrationJobFixture(t, operationType)
			terminal := withGeneratedJobIdentity(job)
			terminal.Status.Conditions = applyComplete
			migration.Status.ActiveOperation.JobUID = terminal.UID
			handler := migrationHandlerFixture(t, migration, terminal)
			cleaned := terminal.DeepCopy()
			cleaned.Spec.TTLSecondsAfterFinished = &long
			response := handler.Handle(context.Background(), migrationUpdateRequest(t, terminal, cleaned))
			if response.Allowed || !strings.Contains(responseMessage(response), want) {
				t.Fatalf("response %v %q, want a denial naming %q", response.Allowed, responseMessage(response), want)
			}
		})
	}
}

// TestAMigrationApplyKeepsTheLongTTLByTheOutcomeItReported reads a migration
// run's outcome out of the summary, since a Failed or a Partial run can report
// no error code of its own to read.
func TestAMigrationApplyKeepsTheLongTTLByTheOutcomeItReported(t *testing.T) {
	t.Parallel()

	long := jobclaim.FailedApplyRetentionTTLSeconds
	const refusesSuccess = "this run reported success; it keeps 300 seconds"
	for _, test := range []struct {
		name    string
		result  runner.Result
		message string
	}{
		{
			name: "applied", message: refusesSuccess,
			result: runner.Result{MutationStarted: true, MigrationRun: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up", Outcome: dataplane.MigrationOutcomeApplied,
			}},
		},
		{
			name: "up to date", message: refusesSuccess,
			result: runner.Result{MigrationRun: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up", Outcome: dataplane.MigrationOutcomeUpToDate,
			}},
		},
		{
			// The summary reads without a code, so the outcome is what says it failed.
			name: "failed with no error code",
			result: runner.Result{MutationStarted: true, MigrationRun: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up", Outcome: dataplane.MigrationOutcomeFailed,
			}},
		},
		{
			name: "failed",
			result: runner.Result{ChildExitCode: 2, MutationStarted: true, Error: &runner.ResultError{Code: "child_exit", Message: "ptah exited with code 2"},
				MigrationRun: &dataplane.MigrationRunReport{
					ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up", Outcome: dataplane.MigrationOutcomeFailed,
				}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, plan, job := migrationApplyJobFixture(t)
			terminal := withGeneratedJobIdentity(job)
			terminal.Status.Conditions = applyComplete
			migration.Status.ActiveOperation.JobUID = terminal.UID
			migration.Status.ActiveOperation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: "approval", UID: "approval-uid"}
			result := test.result
			result.Operation = runner.OperationMigrationApply
			result.OperationID = terminal.Annotations[workload.AnnotationOperationID]
			handler := migrationHandlerFixture(t, migration, plan, terminal, jobPod("a", terminal, terminal.UID, summaryOf(t, result)))
			cleaned := terminal.DeepCopy()
			cleaned.Spec.TTLSecondsAfterFinished = &long
			response := handler.Handle(context.Background(), migrationUpdateRequest(t, terminal, cleaned))
			if test.message == "" {
				if !response.Allowed {
					t.Fatalf("the long TTL on a %s run was denied: %s", test.name, responseMessage(response))
				}
				return
			}
			if response.Allowed || !strings.Contains(responseMessage(response), test.message) {
				t.Fatalf("response %v %q, want a denial naming %q", response.Allowed, responseMessage(response), test.message)
			}
		})
	}
}

// consumedApproval is the record of a person authorizing a plan.
func consumedApproval() *operatorv1alpha1.ConsumedApprovalStatus {
	return &operatorv1alpha1.ConsumedApprovalStatus{Name: "approval", UID: "approval-uid"}
}

// TestOnlyAnApprovedApplyKeepsTheLongTTL holds both families to the approval their
// claim names. Under `apply: Always` the same failure repeats every cycle, so the
// long TTL would keep one Job per cycle.
func TestOnlyAnApprovedApplyKeepsTheLongTTL(t *testing.T) {
	t.Parallel()

	long, short := jobclaim.FailedApplyRetentionTTLSeconds, jobclaim.CleanupTTLSeconds
	const want = "Job cleanup TTL 86400 is for the Job of an Apply a person approved, and no approval authorized this run; it keeps 300 seconds"
	failed := func(t *testing.T, job *batchv1.Job, operation runner.Operation) string {
		return summaryOf(t, runner.Result{
			Operation: operation, OperationID: job.Annotations[workload.AnnotationOperationID],
			ChildExitCode: 2, MutationStarted: true, Uncertain: true,
			Error: &runner.ResultError{Code: "apply_outcome_unknown", Message: "unknown"},
		})
	}
	check := func(t *testing.T, response admission.Response, ttl int32, approved bool) {
		t.Helper()
		if ttl == long && !approved {
			if response.Allowed || !strings.Contains(responseMessage(response), want) {
				t.Fatalf("response %v %q, want a denial naming %q", response.Allowed, responseMessage(response), want)
			}
			return
		}
		if !response.Allowed {
			t.Fatalf("TTL %d, approved %v was denied: %s", ttl, approved, responseMessage(response))
		}
	}
	for _, test := range []struct {
		name     string
		ttl      int32
		approved bool
	}{
		{name: "the long TTL with an approval", ttl: long, approved: true},
		{name: "the long TTL with none", ttl: long},
		{name: "the short TTL with none", ttl: short},
	} {
		t.Run("schema: "+test.name, func(t *testing.T) {
			t.Parallel()
			schema, expected, oldJob, job := currentCleanupFixture(t, operatorv1alpha1.OperationApply)
			schema.Status.Plan.Approval = nil
			if test.approved {
				schema.Status.Plan.Approval = consumedApproval()
			}
			oldJob.Status.Conditions = applyComplete
			job.Status.Conditions = applyComplete
			job.Spec.TTLSecondsAfterFinished = &test.ttl
			handler := handlerFixture(t, staticJobBuilder{job: expected}, schema, jobPod("a", oldJob, oldJob.UID, failed(t, oldJob, runner.OperationApply)))
			request := requestFor(t, admissionv1.Update, job)
			request.OldObject = rawObject(t, oldJob)
			check(t, handler.Handle(context.Background(), request), test.ttl, test.approved)
		})
		t.Run("migration: "+test.name, func(t *testing.T) {
			t.Parallel()
			migration, plan, job := migrationApplyJobFixture(t)
			terminal := withGeneratedJobIdentity(job)
			terminal.Status.Conditions = applyComplete
			migration.Status.ActiveOperation.JobUID = terminal.UID
			if test.approved {
				migration.Status.ActiveOperation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: "approval", UID: "approval-uid"}
			}
			handler := migrationHandlerFixture(t, migration, plan, terminal, jobPod("a", terminal, terminal.UID, failed(t, terminal, runner.OperationMigrationApply)))
			cleaned := terminal.DeepCopy()
			cleaned.Spec.TTLSecondsAfterFinished = &test.ttl
			check(t, handler.Handle(context.Background(), migrationUpdateRequest(t, terminal, cleaned)), test.ttl, test.approved)
		})
	}
}

// failingPodReader answers everything but a list of Pods.
type failingPodReader struct {
	client.Reader
}

func (r failingPodReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if _, pods := list.(*corev1.PodList); pods {
		return errors.New("the API server did not answer")
	}
	return r.Reader.List(ctx, list, options...)
}

// TestACompleteJobWhosePodsCannotBeReadIsNotAdmitted fails closed. An admitted long
// TTL on a run that reported success would keep a Job a day for nothing, and the
// manager falls back to the short TTL on a refusal.
func TestACompleteJobWhosePodsCannotBeReadIsNotAdmitted(t *testing.T) {
	t.Parallel()

	schema, expected, oldJob, job := currentCleanupFixture(t, operatorv1alpha1.OperationApply)
	schema.Status.Plan.Approval = consumedApproval()
	long := jobclaim.FailedApplyRetentionTTLSeconds
	oldJob.Status.Conditions = applyComplete
	job.Status.Conditions = applyComplete
	job.Spec.TTLSecondsAfterFinished = &long
	reader := fake.NewClientBuilder().WithScheme(controllerWriteScheme(t)).WithObjects(schema).Build()
	handler := handlerWithReader(staticJobBuilder{job: expected}, failingPodReader{Reader: reader})
	request := requestFor(t, admissionv1.Update, job)
	request.OldObject = rawObject(t, oldJob)
	response := handler.Handle(context.Background(), request)
	if response.Allowed || !strings.Contains(responseMessage(response), "read the Pods of Apply Job") {
		t.Fatalf("response %v %q, want a refusal naming the read", response.Allowed, responseMessage(response))
	}
}
