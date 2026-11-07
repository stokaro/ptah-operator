package controllerwrite_test

import (
	"context"
	"errors"
	"fmt"
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

// TestARunThatRepeatsKeepsTheLongTTLOnlyWithAnApproval holds both families to the
// approval their claim names. Under `apply: Always` such a failure repeats, so the
// long TTL would keep one Job per cycle. A schema's uncertain Apply is one of them,
// and for a migration it takes a run that reported Failed:
// TestAMigrationRunNoApprovalNamesKeepsTheLongTTLOnlyWhenItStopsItsResource holds
// the rest of what a migration run can report.
func TestARunThatRepeatsKeepsTheLongTTLOnlyWithAnApproval(t *testing.T) {
	t.Parallel()

	long, short := jobclaim.FailedApplyRetentionTTLSeconds, jobclaim.CleanupTTLSeconds
	const (
		schemaRefusal    = "Job cleanup TTL 86400 is for the Job of an Apply a person approved, and no approval authorized this run; it keeps 300 seconds"
		migrationRefusal = "Job cleanup TTL 86400 is for an Apply a person approved or a migration run that stops its resource, " +
			"and no approval authorized this run and it reported Failed; it keeps 300 seconds"
	)
	uncertain := func(t *testing.T, job *batchv1.Job) string {
		return summaryOf(t, runner.Result{
			Operation: runner.OperationApply, OperationID: job.Annotations[workload.AnnotationOperationID],
			ChildExitCode: 2, MutationStarted: true, Uncertain: true,
			Error: &runner.ResultError{Code: "apply_outcome_unknown", Message: "unknown"},
		})
	}
	failed := func(t *testing.T, job *batchv1.Job) string {
		return summaryOf(t, migrationRunResult(job, dataplane.MigrationOutcomeFailed))
	}
	check := func(t *testing.T, response admission.Response, want string, ttl int32, approved bool) {
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
			handler := handlerFixture(t, staticJobBuilder{job: expected}, schema, jobPod("a", oldJob, oldJob.UID, uncertain(t, oldJob)))
			request := requestFor(t, admissionv1.Update, job)
			request.OldObject = rawObject(t, oldJob)
			check(t, handler.Handle(context.Background(), request), schemaRefusal, test.ttl, test.approved)
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
			handler := migrationHandlerFixture(t, migration, plan, terminal, jobPod("a", terminal, terminal.UID, failed(t, terminal)))
			cleaned := terminal.DeepCopy()
			cleaned.Spec.TTLSecondsAfterFinished = &test.ttl
			check(t, handler.Handle(context.Background(), migrationUpdateRequest(t, terminal, cleaned)), migrationRefusal, test.ttl, test.approved)
		})
	}
}

// migrationRunResult is a run that reported this outcome, with the flags the
// runner derives from it, for the attempt this Job's claim made.
func migrationRunResult(job *batchv1.Job, outcome string) runner.Result {
	return runner.Result{
		Operation: runner.OperationMigrationApply, OperationID: job.Annotations[workload.AnnotationOperationID],
		MutationStarted: outcome != dataplane.MigrationOutcomeUpToDate,
		Uncertain:       outcome == dataplane.MigrationOutcomePartial || outcome == dataplane.MigrationOutcomeUnknown,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up", Outcome: outcome,
		},
	}
}

// unapprovedMigrationRun is a finished migration Apply no approval names, and the
// Job with the long TTL the manager would ask for on it.
func unapprovedMigrationRun(t *testing.T, conditions []batchv1.JobCondition) (
	*operatorv1alpha1.PtahMigration, *operatorv1alpha1.PtahMigrationPlan, *batchv1.Job, *batchv1.Job,
) {
	t.Helper()
	migration, plan, job := migrationApplyJobFixture(t)
	terminal := withGeneratedJobIdentity(job)
	terminal.Status.Conditions = conditions
	migration.Status.ActiveOperation.JobUID = terminal.UID
	migration.Status.ActiveOperation.ApprovalRef = nil
	long := jobclaim.FailedApplyRetentionTTLSeconds
	cleaned := terminal.DeepCopy()
	cleaned.Spec.TTLSecondsAfterFinished = &long
	return migration, plan, terminal, cleaned
}

// TestAMigrationRunNoApprovalNamesKeepsTheLongTTLOnlyWhenItStopsItsResource reads the
// rule at the webhook, from what the run's Pod reported, whatever the Job's condition.
// A runner that delivered its result exits zero whatever it reports, and a run can
// report Failed under either condition. Silence is admitted, because the manager
// records such a run as unknown and blocks the resource on it.
func TestAMigrationRunNoApprovalNamesKeepsTheLongTTLOnlyWhenItStopsItsResource(t *testing.T) {
	t.Parallel()

	const (
		repeats = "and no approval authorized this run and it reported %s; it keeps 300 seconds"
		success = "this run reported success; it keeps 300 seconds"
	)
	reported := func(outcome string) func(*testing.T, *batchv1.Job) []client.Object {
		return func(t *testing.T, job *batchv1.Job) []client.Object {
			return []client.Object{jobPod("a", job, job.UID, summaryOf(t, migrationRunResult(job, outcome)))}
		}
	}
	for _, test := range []struct {
		name       string
		conditions []batchv1.JobCondition
		pods       func(*testing.T, *batchv1.Job) []client.Object
		refusal    string
	}{
		{name: "a Complete Job that reported Partial", conditions: applyComplete, pods: reported(dataplane.MigrationOutcomePartial)},
		{name: "a Complete Job that reported Unknown", conditions: applyComplete, pods: reported(dataplane.MigrationOutcomeUnknown)},
		{name: "a Failed Job that reported Partial", conditions: applyFailed, pods: reported(dataplane.MigrationOutcomePartial)},
		{name: "a Complete Job with no Pod left", conditions: applyComplete},
		{name: "a Failed Job with no Pod left", conditions: applyFailed},
		{
			name: "a Pod that left no summary", conditions: applyFailed,
			pods: func(_ *testing.T, job *batchv1.Job) []client.Object {
				return []client.Object{jobPod("a", job, job.UID, "")}
			},
		},
		{
			// A summary that names the label's hash does not read for this attempt.
			name: "a summary naming the label's hash", conditions: applyComplete,
			pods: func(t *testing.T, job *batchv1.Job) []client.Object {
				result := migrationRunResult(job, dataplane.MigrationOutcomeFailed)
				result.OperationID = job.Labels[workload.LabelOperationID]
				return []client.Object{jobPod("a", job, job.UID, summaryOf(t, result))}
			},
		},
		{
			name: "a result with no report in it", conditions: applyComplete,
			pods: func(t *testing.T, job *batchv1.Job) []client.Object {
				return []client.Object{jobPod("a", job, job.UID, summaryOf(t, runner.Result{
					Operation: runner.OperationMigrationApply, OperationID: job.Annotations[workload.AnnotationOperationID],
					ChildExitCode: 2, Error: &runner.ResultError{Code: "invalid_migration_run_output", Message: "failed strict validation"},
				}))}
			},
		},
		{
			// The manager takes a Job of more than one Pod as a run nobody accounted for.
			name: "two Pods that both reported Failed", conditions: applyFailed,
			pods: func(t *testing.T, job *batchv1.Job) []client.Object {
				failed := summaryOf(t, migrationRunResult(job, dataplane.MigrationOutcomeFailed))
				return []client.Object{jobPod("a", job, job.UID, failed), jobPod("b", job, job.UID, failed)}
			},
		},
		{
			name: "a Complete Job that reported Failed", conditions: applyComplete, pods: reported(dataplane.MigrationOutcomeFailed),
			refusal: fmt.Sprintf(repeats, "Failed"),
		},
		{
			name: "a Failed Job that reported Failed", conditions: applyFailed, pods: reported(dataplane.MigrationOutcomeFailed),
			refusal: fmt.Sprintf(repeats, "Failed"),
		},
		{
			name: "a Failed Job that reported Applied", conditions: applyFailed, pods: reported(dataplane.MigrationOutcomeApplied),
			refusal: fmt.Sprintf(repeats, "Applied"),
		},
		{
			name: "a Failed Job that reported it was up to date", conditions: applyFailed, pods: reported(dataplane.MigrationOutcomeUpToDate),
			refusal: fmt.Sprintf(repeats, "UpToDate"),
		},
		{name: "a Complete Job that reported Applied", conditions: applyComplete, pods: reported(dataplane.MigrationOutcomeApplied), refusal: success},
		{
			// The Pod's report is matched by owner, as the manager matches it.
			name: "a Pod whose job-name label was removed", conditions: applyComplete,
			pods: func(t *testing.T, job *batchv1.Job) []client.Object {
				pod := jobPod("a", job, job.UID, summaryOf(t, migrationRunResult(job, dataplane.MigrationOutcomeFailed)))
				pod.Labels = nil
				return []client.Object{pod}
			},
			refusal: fmt.Sprintf(repeats, "Failed"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, plan, terminal, cleaned := unapprovedMigrationRun(t, test.conditions)
			objects := []client.Object{plan, terminal}
			if test.pods != nil {
				objects = append(objects, test.pods(t, terminal)...)
			}
			handler := migrationHandlerFixture(t, migration, objects...)
			response := handler.Handle(context.Background(), migrationUpdateRequest(t, terminal, cleaned))
			if test.refusal == "" {
				if !response.Allowed {
					t.Fatalf("the long TTL was denied: %s", responseMessage(response))
				}
				return
			}
			if response.Allowed || !strings.Contains(responseMessage(response), test.refusal) {
				t.Fatalf("response %v %q, want a denial naming %q", response.Allowed, responseMessage(response), test.refusal)
			}
		})
	}

	t.Run("the short TTL on a run that reported Failed", func(t *testing.T) {
		t.Parallel()
		short := jobclaim.CleanupTTLSeconds
		migration, plan, terminal, cleaned := unapprovedMigrationRun(t, applyComplete)
		cleaned.Spec.TTLSecondsAfterFinished = &short
		handler := migrationHandlerFixture(t, migration, plan, terminal,
			jobPod("a", terminal, terminal.UID, summaryOf(t, migrationRunResult(terminal, dataplane.MigrationOutcomeFailed))))
		if response := handler.Handle(context.Background(), migrationUpdateRequest(t, terminal, cleaned)); !response.Allowed {
			t.Fatalf("the short TTL was denied: %s", responseMessage(response))
		}
	})
}

// TestNoEditEarnsARunTheLongTTL changes what can be edited and leaves what the run
// reported, which is the only thing the rule rests on. Each row starts from a run that
// reported Failed under no approval, or the reverse.
func TestNoEditEarnsARunTheLongTTL(t *testing.T) {
	t.Parallel()

	const repeats = "and no approval authorized this run and it reported Failed; it keeps 300 seconds"
	for _, test := range []struct {
		name string
		edit func(*operatorv1alpha1.PtahMigration, *batchv1.Job)
	}{
		{
			name: "a policy that needs an approval now",
			edit: func(migration *operatorv1alpha1.PtahMigration, _ *batchv1.Job) {
				migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
			},
		},
		{
			name: "a status that says the resource is blocked",
			edit: func(migration *operatorv1alpha1.PtahMigration, _ *batchv1.Job) {
				migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
				migration.Status.UnresolvedRun = &operatorv1alpha1.UnresolvedMigrationRunStatus{
					Outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, OperationID: migration.Status.ActiveOperation.ID,
				}
			},
		},
		{
			// Reading the Job's own annotation would make the summary read for nothing.
			name: "another operation ID on the Job",
			edit: func(_ *operatorv1alpha1.PtahMigration, job *batchv1.Job) {
				job.Annotations[workload.AnnotationOperationID] = "another-operation"
			},
		},
		{
			name: "another operation hash on the Job",
			edit: func(_ *operatorv1alpha1.PtahMigration, job *batchv1.Job) {
				job.Labels[workload.LabelOperationID] = "another-operation"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, plan, terminal, cleaned := unapprovedMigrationRun(t, applyComplete)
			pod := jobPod("a", terminal, terminal.UID, summaryOf(t, migrationRunResult(terminal, dataplane.MigrationOutcomeFailed)))
			test.edit(migration, terminal)
			test.edit(migration, cleaned)
			handler := migrationHandlerFixture(t, migration, plan, terminal, pod)
			response := handler.Handle(context.Background(), migrationUpdateRequest(t, terminal, cleaned))
			if response.Allowed || !strings.Contains(responseMessage(response), repeats) {
				t.Fatalf("response %v %q, want a denial naming %q", response.Allowed, responseMessage(response), repeats)
			}
		})
	}

	// The reverse edit: an approval stays on the claim that took it, whatever the
	// policy says by the time the run ends.
	t.Run("an approved run whose policy moved to Always", func(t *testing.T) {
		t.Parallel()
		migration, plan, terminal, cleaned := unapprovedMigrationRun(t, applyComplete)
		migration.Status.ActiveOperation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: "approval", UID: "approval-uid"}
		migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
		pod := jobPod("a", terminal, terminal.UID, summaryOf(t, migrationRunResult(terminal, dataplane.MigrationOutcomeFailed)))
		handler := migrationHandlerFixture(t, migration, plan, terminal, pod)
		if response := handler.Handle(context.Background(), migrationUpdateRequest(t, terminal, cleaned)); !response.Allowed {
			t.Fatalf("the long TTL on an approved run was denied: %s", responseMessage(response))
		}
	})
}

// TestAMigrationRunWhosePodsCannotBeReadIsNotAdmitted fails closed on the Failed Job
// the success check leaves alone. The manager gets an error it retries, not a refusal
// it would shorten the log for.
func TestAMigrationRunWhosePodsCannotBeReadIsNotAdmitted(t *testing.T) {
	t.Parallel()

	migration, plan, terminal, cleaned := unapprovedMigrationRun(t, applyFailed)
	reader := fake.NewClientBuilder().WithScheme(controllerWriteScheme(t)).
		WithObjects(migration, plan, terminal, migrationServiceAccount()).Build()
	handler := handlerWithReader(migrationJobBuilder(), failingPodReader{Reader: reader})
	response := handler.Handle(context.Background(), migrationUpdateRequest(t, terminal, cleaned))
	if response.Allowed || !strings.Contains(responseMessage(response), "read the Pods of Apply Job") {
		t.Fatalf("response %v %q, want a refusal naming the read", response.Allowed, responseMessage(response))
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
