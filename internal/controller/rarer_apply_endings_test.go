package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// The endings #976 left out (#981): a Job that stops being its claim's, a
// claim that can no longer rebuild its Job, a release that builds a different
// one, a Job whose Pods the result cannot be attributed to, a schema's lost
// database Lease, a deleted schema, and a Pod the API server refused to create
// until the Job's deadline. Each row starts from the report the resource wrote,
// ends the Apply that way, and reads the record that stays.

const frameNotFound = "ptah runner result frame not found"

// releasedWith is the builder of a release that writes the Pod template
// differently from the one that dispatched the Job.
type releasedWith struct {
	workload.Builder
	alter func(*batchv1.Job)
}

func (b releasedWith) Build(
	schema *operatorv1alpha1.PtahSchema,
	operation operatorv1alpha1.ActiveOperationStatus,
	plan *operatorv1alpha1.PtahSchemaPlan,
) (*batchv1.Job, error) {
	job, err := b.Builder.Build(schema, operation, plan)
	if err == nil {
		b.alter(job)
	}
	return job, err
}

func (b releasedWith) BuildMigration(
	migration *operatorv1alpha1.PtahMigration,
	operation operatorv1alpha1.MigrationOperationStatus,
	plan *operatorv1alpha1.PtahMigrationPlan,
) (*batchv1.Job, error) {
	job, err := b.Builder.BuildMigration(migration, operation, plan)
	if err == nil {
		b.alter(job)
	}
	return job, err
}

// unstartedMigration is a migration Apply after the pass that reported why
// its Pod is not running.
type unstartedMigration struct {
	reconciler *MigrationReconciler
	api        client.WithWatch
	recorder   *record.FakeRecorder
	builder    workload.Builder
	migration  *operatorv1alpha1.PtahMigration
	plan       *operatorv1alpha1.PtahMigrationPlan
	job        *batchv1.Job
	// pod is the Pod the kubelet cannot start, and nil when the API server
	// refused to create one.
	pod *corev1.Pod
	// said is what Progressing said about the Pod.
	said string
}

// migrationApplyUnderReport dispatches a migration Apply with the real builder,
// lets stand leave the Job's Pod as the row needs it, and runs the pass that
// reports it. It fails unless Progressing then carries reason with words.
func migrationApplyUnderReport(
	t *testing.T,
	stand func(t *testing.T, run *unstartedMigration),
	reason operatorv1alpha1.ConditionReason,
	words string,
) unstartedMigration {
	t.Helper()

	builder := workloadBuilderForMigrations()
	dispatched := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, runShape{dispatcher: builder, recorded: true})
	reconciler, api := dispatched.reconciler(t, builder)
	run := unstartedMigration{
		reconciler: reconciler, api: api, recorder: record.NewFakeRecorder(256), builder: builder,
		migration: dispatched.migration, plan: dispatched.plan, job: dispatched.job, pod: dispatched.pod,
	}
	reconciler.Recorder = run.recorder
	stand(t, &run)

	stored := reconcileMigration(t, reconciler, api, run.migration)
	progressing := meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Reason != string(reason) || !strings.Contains(progressing.Message, words) {
		t.Fatalf("Progressing while the Pod is not running = %#v, want %s with %q, "+
			"or nothing below has words to keep", progressing, reason, words)
	}
	if stored.Status.ActiveOperation == nil || stored.Status.ActiveOperation.JobUID != run.job.UID {
		t.Fatalf("the report retired the claim: %#v", stored.Status.ActiveOperation)
	}
	run.said = progressing.Message
	return run
}

// unstartedMigrationApplyRun is a migration Apply whose Pod waits on a Secret
// that does not exist.
func unstartedMigrationApplyRun(t *testing.T) unstartedMigration {
	t.Helper()

	return migrationApplyUnderReport(t, func(t *testing.T, run *unstartedMigration) {
		t.Helper()
		stored := &corev1.Pod{}
		if err := run.api.Get(context.Background(), client.ObjectKeyFromObject(run.pod), stored); err != nil {
			t.Fatal(err)
		}
		waitingOnASecret(stored)
		if err := run.api.Status().Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
	}, operatorv1alpha1.ReasonPodConfigurationError, missingSecretText)
}

// refusedPodCreation leaves job as the Job controller leaves one whose Pod the
// API server refused: no Pod, nothing active or terminal, and the FailedCreate
// Event that says why. pod is the Pod the fixture made, which the API server
// never admitted, or nil.
func refusedPodCreation(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod, at metav1.Time) {
	t.Helper()

	ctx := context.Background()
	if pod != nil {
		if err := api.Delete(ctx, pod.DeepCopy()); err != nil {
			t.Fatal(err)
		}
	}
	stored := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	stored.Status = batchv1.JobStatus{}
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := api.Create(ctx, failedCreateEvent(stored, "refused", refusalText, at.Time)); err != nil {
		t.Fatal(err)
	}
}

// pastTheDeadlineWithoutAPod is pastTheDeadline for a Job that never had a
// Pod: the Job fails, and there is nothing to delete.
func pastTheDeadlineWithoutAPod(t *testing.T, api client.Client, job *batchv1.Job) {
	t.Helper()

	ctx := context.Background()
	stored := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	stored.Status = batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
	}}}
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
}

// aSecondPod is a replacement the Job controller made for a Pod that did not
// go: the first stands Terminating on a node that stopped answering, and the
// second waits on the same Secret. The Job then fails at its deadline with both
// still there, which is a Job whose result cannot be attributed to one Pod.
func aSecondPod(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod) {
	t.Helper()

	ctx := context.Background()
	second := pod.DeepCopy()
	second.ResourceVersion = ""
	second.Name = generatedTerminalPodName(job.Name, "def34")
	second.UID = "replacement-pod-uid"
	waitingOnASecret(second)
	if err := api.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	stored := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	stored.Status = batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
	}}}
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
}

// podsGone is the Pods a Job left going at last: the node that stopped
// answering came back, or a person force-deleted them.
func podsGone(t *testing.T, api client.Client, job *batchv1.Job) {
	t.Helper()

	ctx := context.Background()
	pods := &corev1.PodList{}
	if err := api.List(ctx, pods, client.InNamespace(job.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) == 0 {
		t.Fatal("the Job left no Pods, so removing them proves nothing")
	}
	for index := range pods.Items {
		if err := api.Delete(ctx, &pods.Items[index]); err != nil {
			t.Fatal(err)
		}
	}
}

// settle reconciles until the claim is gone, at most passes times, and
// fails if it is still there: an ending that records the run while its Pod can
// still start holds the claim until the Pod stops.
func (run unstartedMigration) settle(t *testing.T, passes int) *operatorv1alpha1.MigrationRunStatus {
	t.Helper()

	for range passes {
		if reconcileMigration(t, run.reconciler, run.api, run.migration).Status.ActiveOperation == nil {
			break
		}
	}
	return settledMigrationRun(t, run.api, run.migration)
}

func TestRarerMigrationApplyEndingsKeepWhyItsPodNeverRan(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		// refused is a Pod the API server refused to create, rather than one the
		// kubelet could not start.
		refused bool
		// end ends the Apply, and stop is what stops what it left running, if
		// anything.
		end, stop func(t *testing.T, run unstartedMigration)
		ended     string
	}{
		{
			name:  "its Job stopped being its claim's after the inputs moved",
			ended: "dispatched Apply Job is not its claim's: the Job's labels are not its claim's",
			end: func(t *testing.T, run unstartedMigration) {
				ctx := context.Background()
				edited := readMigration(t, run.api, run.migration)
				moveMigrationInputs(edited)
				if err := run.api.Update(ctx, edited); err != nil {
					t.Fatal(err)
				}
				requireMigrationInputsMoved(t, run.reconciler, edited)
				relabeled := &batchv1.Job{}
				if err := run.api.Get(ctx, client.ObjectKeyFromObject(run.job), relabeled); err != nil {
					t.Fatal(err)
				}
				underAnotherClaim(relabeled)
				if err := run.api.Update(ctx, relabeled); err != nil {
					t.Fatal(err)
				}
			},
			stop: func(t *testing.T, run unstartedMigration) { pastTheDeadline(t, run.api, run.job, run.pod) },
		},
		{
			name:  "its plan was deleted",
			ended: "rebuild immutable Apply Job intent: ",
			end: func(t *testing.T, run unstartedMigration) {
				if err := run.api.Delete(context.Background(), run.plan.DeepCopy()); err != nil {
					t.Fatal(err)
				}
			},
			stop: func(t *testing.T, run unstartedMigration) { pastTheDeadline(t, run.api, run.job, run.pod) },
		},
		{
			name:  "a release builds another Pod template",
			ended: "dispatched Apply Job intent changed: ",
			end: func(t *testing.T, run unstartedMigration) {
				run.reconciler.Jobs = releasedWith{Builder: run.builder, alter: withAnExecutorSetting}
			},
			stop: func(t *testing.T, run unstartedMigration) { pastTheDeadline(t, run.api, run.job, run.pod) },
		},
		{
			name:  "its Job left two Pods",
			ended: errTerminalPodMultiplicity.Error(),
			end: func(t *testing.T, run unstartedMigration) {
				aSecondPod(t, run.api, run.job, run.pod)
			},
			stop: func(t *testing.T, run unstartedMigration) { podsGone(t, run.api, run.job) },
		},
		{
			name: "its Pod was refused until the deadline", refused: true,
			ended: "read the Apply result: " + frameNotFound,
			end: func(t *testing.T, run unstartedMigration) {
				pastTheDeadlineWithoutAPod(t, run.api, run.job)
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			var run unstartedMigration
			if row.refused {
				run = migrationApplyUnderReport(t, func(t *testing.T, run *unstartedMigration) {
					t.Helper()
					refusedPodCreation(t, run.api, run.job, run.pod, metav1.NewTime(run.reconciler.now()))
					run.pod = nil
				}, operatorv1alpha1.ReasonPodAdmissionRefused, "denied request")
			} else {
				run = unstartedMigrationApplyRun(t)
			}
			row.end(t, run)
			if row.stop != nil {
				// The ending records the run while the Pod stands, and the
				// claim stays until the Pod stops.
				stored := reconcileMigration(t, run.reconciler, run.api, run.migration)
				if stored.Status.UnresolvedRun == nil {
					t.Fatalf("the ending did not record the run: claim %#v, conditions %#v",
						stored.Status.ActiveOperation, stored.Status.Conditions)
				}
				row.stop(t, run)
			}

			record := run.settle(t, 3)
			if want := row.ended; !strings.Contains(record.Message, want) {
				t.Fatalf("last run message = %q, want the sentence this ending writes, %q", record.Message, want)
			}
			if want := lastReportedBefore + run.said; !strings.Contains(record.Message, want) {
				t.Fatalf("last run message = %q, want it to carry %q", record.Message, want)
			}
		})
	}
}

// settledSchemaEvent is the ApplyOutcomeUnknown Event the passes so far
// recorded, and fails when there is none.
func settledSchemaEvent(t *testing.T, recorder *record.FakeRecorder) string {
	t.Helper()

	event := findEvent(recorder, corev1.EventTypeWarning+" ApplyOutcomeUnknown ")
	if event == "" {
		t.Fatal("no ApplyOutcomeUnknown Event was recorded")
	}
	return event
}

func TestRarerSchemaApplyEndingsKeepWhyItsPodNeverRan(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		refused bool
		end     func(t *testing.T, run unstartedSchema)
		// passes is how many passes the ending takes to leave the record.
		passes int
		ended  string
	}{
		{
			name:  "its Job stopped being its claim's after the inputs moved",
			ended: "dispatched Apply Job is not its claim's: the Job's labels are not its claim's", passes: 1,
			end: func(t *testing.T, run unstartedSchema) {
				ctx := context.Background()
				moved := moveSchemaInputs(t, dispatchedSchemaRun{schema: run.schema, job: run.job, reconciler: run.reconciler, api: run.api})
				requireSchemaInputsMoved(t, run.reconciler, moved)
				relabeled := &batchv1.Job{}
				if err := run.api.Get(ctx, client.ObjectKeyFromObject(run.job), relabeled); err != nil {
					t.Fatal(err)
				}
				underAnotherClaim(relabeled)
				if err := run.api.Update(ctx, relabeled); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "its plan was deleted", ended: "rebuild immutable Apply Job intent: ", passes: 1,
			end: func(t *testing.T, run unstartedSchema) {
				ctx := context.Background()
				plans := &operatorv1alpha1.PtahSchemaPlanList{}
				if err := run.api.List(ctx, plans, client.InNamespace(run.schema.Namespace)); err != nil {
					t.Fatal(err)
				}
				if len(plans.Items) == 0 {
					t.Fatal("the Apply runs no stored plan, so deleting one proves nothing")
				}
				for index := range plans.Items {
					if err := run.api.Delete(ctx, &plans.Items[index]); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "a release builds another Pod template", ended: "dispatched Apply Job intent changed: ", passes: 1,
			end: func(_ *testing.T, run unstartedSchema) {
				run.reconciler.Jobs = releasedWith{Builder: workloadBuilderForMigrations(), alter: withAnExecutorSetting}
			},
		},
		{
			name: "its Job left two Pods", ended: "Job produced multiple executor Pods", passes: 1,
			end: func(t *testing.T, run unstartedSchema) {
				aSecondPod(t, run.api, run.job, run.pod)
			},
		},
		{
			// One pass records the lost continuity, and the next ends the Apply.
			name: "its database Lease changed hands", ended: "database lock continuity was lost during Apply", passes: 2,
			end: func(t *testing.T, run unstartedSchema) {
				databaseLeasesGone(t, run.api, run.reconciler.LockNamespace)
			},
		},
		{
			name: "its Pod was refused until the deadline", refused: true, passes: 1,
			ended: "apply result is uncertain: " + frameNotFound,
			end: func(t *testing.T, run unstartedSchema) {
				pastTheDeadlineWithoutAPod(t, run.api, run.job)
				// The deadline has passed for the reconciler too: a Job without
				// a Pod is read as one only after the grace a Pod gets to appear.
				deadline := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation.ExecutionNotAfter.Add(time.Second)
				run.reconciler.Clock = func() time.Time { return deadline }
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			var run unstartedSchema
			if row.refused {
				run = schemaApplyUnderReport(t, func(api client.Client, job *batchv1.Job) *corev1.Pod {
					refusedPodCreation(t, api, job, nil, metav1.Now())
					return nil
				}, operatorv1alpha1.ReasonPodAdmissionRefused, "denied request")
			} else {
				run = unstartedSchemaApplyRun(t)
			}
			row.end(t, run)
			for range row.passes {
				run.reconcileOnce(t)
			}

			failed := settledSchemaFailure(t, run.api, run.schema)
			if !strings.Contains(failed.Message, row.ended) {
				t.Fatalf("ReconciliationFailed = %q, want the sentence this ending writes, %q", failed.Message, row.ended)
			}
			if want := lastReportedBefore + run.said; !strings.Contains(failed.Message, want) {
				t.Fatalf("ReconciliationFailed = %q, want it to carry %q", failed.Message, want)
			}
			if event, want := settledSchemaEvent(t, run.recorder), uncertainApplyFixedEvent+lastReportedBefore+run.said; !strings.Contains(event, want) {
				t.Fatalf("ApplyOutcomeUnknown = %q, want it to carry %q", event, want)
			}
		})
	}
}

// A deleted schema takes its status with it, so what stays is the Event the
// settling pass recorded. The deletion waits for a running Apply and looks at
// its Pod while it waits, as supervision does.
func TestADeletedSchemasLastEventKeepsWhyItsApplyPodNeverStarted(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		end   func(t *testing.T, run unstartedSchema)
		ended string
	}{
		{
			name: "its Job was deleted while the schema waited", ended: "dispatched Apply Job identity was lost during deletion",
			end: func(t *testing.T, run unstartedSchema) {
				if err := run.api.Delete(context.Background(), run.job.DeepCopy()); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "its Job passed its deadline", ended: "Apply executor termination is not proven during deletion",
			end: func(t *testing.T, run unstartedSchema) { pastTheDeadline(t, run.api, run.job, run.pod) },
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := unstartedSchemaApplyRun(t)
			deleteSchemaWhileItsApplyRuns(t, run)
			row.end(t, run)
			run.reconcileOnce(t)

			event := settledSchemaEvent(t, run.recorder)
			if want := uncertainApplyFixedEvent + lastReportedBefore + run.said; !strings.Contains(event, want) {
				t.Fatalf("ApplyOutcomeUnknown = %q, want it to carry %q", event, want)
			}
			failed := settledSchemaFailure(t, run.api, run.schema)
			if want := row.ended + lastReportedBefore + run.said; !strings.Contains(failed.Message, want) {
				t.Fatalf("ReconciliationFailed = %q, want it to carry %q", failed.Message, want)
			}
		})
	}
}

// deleteSchemaWhileItsApplyRuns deletes the schema and runs the pass that
// waits for its Apply, which keeps the claim.
func deleteSchemaWhileItsApplyRuns(t *testing.T, run unstartedSchema) {
	t.Helper()

	if err := run.api.Delete(context.Background(), safetyGetSchema(t, run.api, run.schema)); err != nil {
		t.Fatal(err)
	}
	run.reconcileOnce(t)
	if waiting := safetyGetSchema(t, run.api, run.schema); waiting.DeletionTimestamp == nil ||
		waiting.Status.ActiveOperation == nil || waiting.Status.PendingObservation != nil {
		t.Fatalf("the deletion did not wait for its running Apply: claim %#v, pending %#v",
			waiting.Status.ActiveOperation, waiting.Status.PendingObservation)
	}
}

// A Pod that starts while the deleted schema waits and is then stopped by the
// Job's deadline does not leave the Event a report the look found out of date.
func TestADeletedSchemaWhosePodStartedWhileItWaitedRepeatsNoStaleReport(t *testing.T) {
	t.Parallel()

	run := unstartedSchemaApplyRun(t)
	deleteSchemaWhileItsApplyRuns(t, run)
	startedAfterAll(t, run.api, run.job, run.pod)
	run.reconcileOnce(t)
	if ready := meta.FindStatusCondition(safetyGetSchema(t, run.api, run.schema).Status.Conditions, operatorv1alpha1.ConditionReady); ready == nil ||
		ready.Reason == string(operatorv1alpha1.ReasonPodConfigurationError) {
		t.Fatalf("the deletion wait did not look at the Pod that started: Ready %#v", ready)
	}

	pastTheDeadline(t, run.api, run.job, run.pod)
	run.reconcileOnce(t)

	if event := settledSchemaEvent(t, run.recorder); event != uncertainApplyFixedEvent {
		t.Fatalf("ApplyOutcomeUnknown = %q, want the fixed text alone", event)
	}
	if failed := settledSchemaFailure(t, run.api, run.schema); !strings.Contains(failed.Message, "Apply executor termination is not proven during deletion") ||
		strings.Contains(failed.Message, run.said) {
		t.Fatalf("ReconciliationFailed = %q, want the deletion's sentence without the report the look found out of date", failed.Message)
	}
}

// A Job under the reserved name that the claim never recorded is not shown to
// be the claim's: owning it is not enough, since anyone who may create a Job
// can create one with the schema as its owner. The deletion wait does not take
// a report from it, so the ending says nothing about its Pod.
func TestADeletedSchemaTakesNoReportFromAJobItsClaimNeverRecorded(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	builder := workloadBuilderForMigrations()
	dispatched := schemaRunDispatchedBy(t, operatorv1alpha1.OperationApply, runShape{dispatcher: builder}, builder)
	run := unstartedSchema{
		reconciler: dispatched.reconciler, api: dispatched.api, recorder: record.NewFakeRecorder(256),
		schema: dispatched.schema, job: dispatched.job,
	}
	run.reconciler.Recorder = run.recorder
	if operation := run.schema.Status.ActiveOperation; operation == nil || !operation.DispatchStarted || operation.JobUID != "" {
		t.Fatalf("the claim = %#v, want a dispatch that never recorded its Job", operation)
	}
	run.pod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: run.job.Namespace, Name: generatedTerminalPodName(run.job.Name, "abc12"), UID: "unstarted-pod-uid",
			Labels: maps.Clone(run.job.Spec.Template.Labels), Annotations: maps.Clone(run.job.Spec.Template.Annotations),
			OwnerReferences: []metav1.OwnerReference{jobControllerReference(run.job)},
		},
		Spec: *run.job.Spec.Template.Spec.DeepCopy(),
	}
	waitingOnASecret(run.pod)
	if err := run.api.Create(ctx, run.pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}

	deleteSchemaWhileItsApplyRuns(t, run)
	if ready := meta.FindStatusCondition(safetyGetSchema(t, run.api, run.schema).Status.Conditions, operatorv1alpha1.ConditionReady); ready != nil &&
		ready.Reason == string(operatorv1alpha1.ReasonPodConfigurationError) {
		t.Fatalf("the deletion wait took a report from a Job its claim never recorded: Ready %#v", ready)
	}

	pastTheDeadline(t, run.api, run.job, run.pod)
	run.reconcileOnce(t)

	if event := settledSchemaEvent(t, run.recorder); event != uncertainApplyFixedEvent {
		t.Fatalf("ApplyOutcomeUnknown = %q, want the fixed text alone", event)
	}
	if failed := settledSchemaFailure(t, run.api, run.schema); !strings.Contains(failed.Message, "Apply executor termination is not proven during deletion") ||
		strings.Contains(failed.Message, lastReportedBefore) {
		t.Fatalf("ReconciliationFailed = %q, want the deletion's sentence and no report", failed.Message)
	}
}

// The write that records a lost Lease leaves Ready's report standing for the
// pass that ends the Apply, and still says the lock changed.
func TestASchemaLosingItsLeaseKeepsReadysReportForTheEnding(t *testing.T) {
	t.Parallel()

	run := unstartedSchemaApplyRun(t)
	databaseLeasesGone(t, run.api, run.reconciler.LockNamespace)
	run.reconcileOnce(t)

	recorded := safetyGetSchema(t, run.api, run.schema)
	if operation := recorded.Status.ActiveOperation; operation == nil || !operation.LeaseContinuityLost {
		t.Fatalf("the pass did not record the lost continuity: claim %#v", operation)
	}
	ready := meta.FindStatusCondition(recorded.Status.Conditions, operatorv1alpha1.ConditionReady)
	if ready == nil || ready.Message != run.said {
		t.Fatalf("Ready after the lost Lease = %#v, want the report %q", ready, run.said)
	}
	inSync := meta.FindStatusCondition(recorded.Status.Conditions, operatorv1alpha1.ConditionInSync)
	if inSync == nil || inSync.Reason != string(operatorv1alpha1.ReasonLeaseContinuityLost) {
		t.Fatalf("InSync after the lost Lease = %#v, want LeaseContinuityLost", inSync)
	}
	if findEvent(run.recorder, corev1.EventTypeWarning+" LeaseContinuityLost ") == "" {
		t.Fatal("the lost Lease recorded no LeaseContinuityLost Event")
	}
}

func TestAnUncertainApplyEventCarriesTheLastPodReport(t *testing.T) {
	t.Parallel()

	const (
		fixed = "Apply outcome is uncertain; observing database state"
		job   = "ptah-apply-orders"
	)
	report := lastPodReport{jobName: job, reported: &metav1.Condition{
		Reason:  string(operatorv1alpha1.ReasonPodConfigurationError),
		Message: "Job " + job + " cannot start its Pod: " + missingSecretText,
	}}
	long := lastPodReport{jobName: job, reported: &metav1.Condition{
		Reason:  string(operatorv1alpha1.ReasonPodAdmissionRefused),
		Message: "Job " + job + " cannot create its Pod; the API server refused it: " + strings.Repeat("r", 1000),
	}}
	ended := errors.New("dispatched Apply Job identity was lost during deletion")
	for _, row := range []struct {
		name    string
		failure error
		want    string
	}{
		{name: "no report", failure: ended, want: fixed},
		{
			name: "a report", failure: withLastPodReport(ended, report),
			want: fixed + lastReportedBefore + report.reported.Message,
		},
		{
			name: "a report under another wrap", failure: fmt.Errorf("apply result is uncertain: %w", withLastPodReport(ended, report)),
			want: fixed + lastReportedBefore + report.reported.Message,
		},
		{
			name: "a long report", failure: withLastPodReport(ended, long),
			want: fixed + lastReportedBefore + bounded(long.reported.Message, 512),
		},
		{
			name: "a report the claim's Job ran past", failure: withLastPodReport(ended, lastPodReport{
				jobName: report.jobName, reported: report.reported, ran: true,
			}),
			want: fixed,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := uncertainApplyEvent(row.failure); got != row.want {
				t.Fatalf("uncertainApplyEvent() = %q, want %q", got, row.want)
			}
		})
	}
}

// failingPodLooks refuses the Pod and Event lists a look at a Pod reads.
type failingPodLooks struct{ client.Reader }

func (r failingPodLooks) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	switch list.(type) {
	case *corev1.PodList, *corev1.EventList:
		return errors.New("the API server did not answer the list")
	}
	return r.Reader.List(ctx, list, opts...)
}

// databaseLeaseVersions is every database Lease by name and resourceVersion,
// which a renewal moves.
func databaseLeaseVersions(t *testing.T, api client.Client, namespace string) string {
	t.Helper()

	leases := &coordinationv1.LeaseList{}
	if err := api.List(context.Background(), leases, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	if len(leases.Items) == 0 {
		t.Fatal("no database Lease exists, so a renewal cannot be seen")
	}
	versions := make([]string, 0, len(leases.Items))
	for _, lease := range leases.Items {
		versions = append(versions, lease.Name+"@"+lease.ResourceVersion)
	}
	return strings.Join(versions, ",")
}

// The deletion wait renews the database Lease and looks at the Pod. A look that
// fails is logged and leaves the wait as it was: returned, it would back the
// pass off, and the Lease the wait renews could lapse under a running Apply.
func TestADeletedSchemasFailedLookKeepsTheWait(t *testing.T) {
	t.Parallel()

	run := unstartedSchemaApplyRun(t)
	deleteSchemaWhileItsApplyRuns(t, run)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run.schema)}
	base := run.reconciler.now()
	pass := func(at time.Duration) {
		t.Helper()
		run.reconciler.Clock = func() time.Time { return base.Add(at) }
		result, err := run.reconciler.Reconcile(context.Background(), request)
		if err != nil || result.RequeueAfter != 5*time.Second {
			t.Fatalf("the deletion wait = %+v, %v, want the 5s wait and no error", result, err)
		}
	}

	before := databaseLeaseVersions(t, run.api, run.reconciler.LockNamespace)
	pass(3 * time.Second)
	renewed := databaseLeaseVersions(t, run.api, run.reconciler.LockNamespace)
	if renewed == before {
		t.Fatalf("the wait did not renew the Lease (%s), so a look that stops the renewal would go unseen", renewed)
	}

	run.reconciler.APIReader = failingPodLooks{Reader: run.api}
	pass(6 * time.Second)
	if after := databaseLeaseVersions(t, run.api, run.reconciler.LockNamespace); after == renewed {
		t.Fatalf("the pass whose look failed did not renew the Lease: %s", after)
	}
	stored := safetyGetSchema(t, run.api, run.schema)
	if stored.Status.ActiveOperation == nil {
		t.Fatal("the pass whose look failed dropped the claim")
	}
	if ready := meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionReady); ready == nil || ready.Message != run.said {
		t.Fatalf("Ready after the failed look = %#v, want the last report %q standing", ready, run.said)
	}
}

// The write that records a lost Lease replaces Ready when Ready holds no report
// the ending will carry: an Apply whose Pod a look found started.
func TestASchemaApplyLosingItsLeaseWithNoReportSaysTheLockChanged(t *testing.T) {
	t.Parallel()

	run := unstartedSchemaApplyRun(t)
	startedAfterAll(t, run.api, run.job, run.pod)
	run.reconcileOnce(t)
	if ready := meta.FindStatusCondition(safetyGetSchema(t, run.api, run.schema).Status.Conditions, operatorv1alpha1.ConditionReady); ready == nil ||
		ready.Reason != string(operatorv1alpha1.ReasonOperationInProgress) {
		t.Fatalf("Ready after the look at the Pod that started = %#v, want OperationInProgress", ready)
	}

	databaseLeasesGone(t, run.api, run.reconciler.LockNamespace)
	run.reconcileOnce(t)

	recorded := safetyGetSchema(t, run.api, run.schema)
	if operation := recorded.Status.ActiveOperation; operation == nil || !operation.LeaseContinuityLost {
		t.Fatalf("the pass did not record the lost continuity: claim %#v", operation)
	}
	if ready := meta.FindStatusCondition(recorded.Status.Conditions, operatorv1alpha1.ConditionReady); ready == nil ||
		ready.Reason != string(operatorv1alpha1.ReasonLeaseContinuityLost) {
		t.Fatalf("Ready after the lost Lease = %#v, want LeaseContinuityLost", ready)
	}
}

// A read-only claim's report does not stand through a lost Lease: the pass that
// waits for a read-only Job does not look at its Pod again, so the words would
// stay after the Pod started. The write says the lock changed instead.
func TestAPlanLosingItsLeaseDropsReadysReport(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	schema := safetyLockedOperationSchema(operatorv1alpha1.OperationPlan)
	operation := schema.Status.ActiveOperation
	operation.JobName = "ptah-plan-orders"
	operation.JobUID = "plan-job-uid"
	controller := true
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace, Name: operation.JobName, UID: operation.JobUID,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahSchema",
			Name: schema.Name, UID: schema.UID, Controller: &controller,
		}},
	}, Status: batchv1.JobStatus{Active: 1}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: job.Namespace, Name: generatedTerminalPodName(job.Name, "abc12"), UID: "plan-pod-uid",
		Labels:          map[string]string{batchv1.ControllerUidLabel: string(job.UID)},
		OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
	}}
	waitingOnASecret(pod)
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, job, pod)

	if err := reconciler.reportPodAdmission(ctx, safetyGetSchema(t, api, schema), job); err != nil {
		t.Fatalf("reportPodAdmission() = %v", err)
	}
	if ready := meta.FindStatusCondition(safetyGetSchema(t, api, schema).Status.Conditions, operatorv1alpha1.ConditionReady); ready == nil ||
		ready.Reason != string(operatorv1alpha1.ReasonPodConfigurationError) {
		t.Fatalf("Ready while the Plan's Pod waits = %#v, want the kubelet's words, or the Lease has no report to drop", ready)
	}

	databaseLeasesGone(t, api, reconciler.LockNamespace)
	if _, _, err := reconciler.acquireOperationLock(ctx, safetyGetSchema(t, api, schema)); err != nil {
		t.Fatalf("acquireOperationLock() = %v", err)
	}
	recorded := safetyGetSchema(t, api, schema)
	if operation := recorded.Status.ActiveOperation; operation == nil || !operation.LeaseContinuityLost {
		t.Fatalf("the lock did not record the lost continuity: claim %#v", operation)
	}
	if ready := meta.FindStatusCondition(recorded.Status.Conditions, operatorv1alpha1.ConditionReady); ready == nil ||
		ready.Reason != string(operatorv1alpha1.ReasonLeaseContinuityLost) {
		t.Fatalf("Ready after the Plan lost its Lease = %#v, want LeaseContinuityLost", ready)
	}
}

// A deleted schema's Job that a person disowned after its Pod ran to the end
// loses its identity, and the ending reads the Job it found: the claim's own,
// counting a succeeded Pod, so the report is out of date.
func TestADeletedSchemaWhoseDisownedJobRanRepeatsNoStaleReport(t *testing.T) {
	t.Parallel()

	run := unstartedSchemaApplyRun(t)
	deleteSchemaWhileItsApplyRuns(t, run)
	ranToSuccess(t, run.api, run.job, run.pod)
	disowned(t, run.api, run.job)
	run.reconcileOnce(t)

	if event := settledSchemaEvent(t, run.recorder); event != uncertainApplyFixedEvent {
		t.Fatalf("ApplyOutcomeUnknown = %q, want the fixed text alone", event)
	}
	if failed := settledSchemaFailure(t, run.api, run.schema); !strings.Contains(failed.Message, "dispatched Apply Job identity was lost during deletion") ||
		strings.Contains(failed.Message, run.said) {
		t.Fatalf("ReconciliationFailed = %q, want the identity sentence without the report the Job's success proved out of date", failed.Message)
	}
}

// createdAfterAll is a refusal that lifted between two looks: the Job's Pod
// was created, its executor ran and failed, and the Job used up its backoff
// limit. pod is the Pod the Job controller would have made from the template.
func createdAfterAll(t *testing.T, api client.Client, job *batchv1.Job, pod *corev1.Pod) {
	t.Helper()

	created := pod.DeepCopy()
	created.ResourceVersion = ""
	created.Name = generatedTerminalPodName(job.Name, "ghi56")
	created.UID = "created-after-all-pod-uid"
	created.Status = corev1.PodStatus{}
	if err := api.Create(context.Background(), created); err != nil {
		t.Fatal(err)
	}
	endedWithoutAResult(t, api, job, created)
}

// A refusal is out of date once the Job counts a Pod: the API server created
// one after all. The Pod ran and failed without a result between two looks, so
// the condition still holds the refusal, and the record must not repeat it.
func TestARefusalTheJobsPodOutlivedIsNotRepeated(t *testing.T) {
	t.Parallel()

	t.Run("a PtahSchema", func(t *testing.T) {
		t.Parallel()

		run := schemaApplyUnderReport(t, func(api client.Client, job *batchv1.Job) *corev1.Pod {
			refusedPodCreation(t, api, job, nil, metav1.Now())
			return nil
		}, operatorv1alpha1.ReasonPodAdmissionRefused, "denied request")
		createdAfterAll(t, run.api, run.job, terminalPodForJob(run.job))
		run.reconcileOnce(t)

		failed := settledSchemaFailure(t, run.api, run.schema)
		if !strings.Contains(failed.Message, "apply result is uncertain: "+frameNotFound) || strings.Contains(failed.Message, lastReportedBefore) {
			t.Fatalf("ReconciliationFailed = %q, want the missing result without the refusal the Pod outlived", failed.Message)
		}
		if event := settledSchemaEvent(t, run.recorder); strings.Contains(event, lastReportedBefore) {
			t.Fatalf("ApplyOutcomeUnknown = %q repeats the refusal the Pod outlived", event)
		}
	})
	t.Run("a PtahMigration", func(t *testing.T) {
		t.Parallel()

		var template *corev1.Pod
		run := migrationApplyUnderReport(t, func(t *testing.T, run *unstartedMigration) {
			t.Helper()
			template = run.pod.DeepCopy()
			refusedPodCreation(t, run.api, run.job, run.pod, metav1.NewTime(run.reconciler.now()))
			run.pod = nil
		}, operatorv1alpha1.ReasonPodAdmissionRefused, "denied request")
		createdAfterAll(t, run.api, run.job, template)

		record := run.settle(t, 3)
		if !strings.Contains(record.Message, "read the Apply result: "+frameNotFound) || strings.Contains(record.Message, lastReportedBefore) {
			t.Fatalf("last run message = %q, want the missing result without the refusal the Pod outlived", record.Message)
		}
	})
}

// firstPodStarted leaves the Job's first Pod with its executor in state: it got
// the Secret and started, and its node then stopped answering, so it stands
// with the last status the kubelet wrote.
func firstPodStarted(t *testing.T, api client.Client, pod *corev1.Pod, state corev1.ContainerState) {
	t.Helper()

	stored := &corev1.Pod{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(pod), stored); err != nil {
		t.Fatal(err)
	}
	stored.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: executorContainerName, State: state,
	}}}
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
}

// Two Pods whose result cannot be attributed: the first started between two
// looks, may have run DDL, and lost its node, and the second waits on the
// Secret the first was once refused. The report is about the first Pod's wait,
// which ended, so the record does not repeat it. ContainerStatusUnknown says
// nothing about a start, and the report stands.
func TestTwoPodsOneOfWhichStartedRepeatNoStaleReport(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		state corev1.ContainerState
		kept  bool
	}{
		{name: "the first executor running", state: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		{
			name:  "the first executor failed",
			state: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		},
		{
			name: "the first executor lost to the kubelet", kept: true,
			state: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "ContainerStatusUnknown"}},
		},
	} {
		check := func(t *testing.T, message, said string) {
			t.Helper()
			if kept := strings.Contains(message, lastReportedBefore+said); kept != row.kept {
				t.Fatalf("the record = %q, carries the report: %t, want %t", message, kept, row.kept)
			}
		}
		t.Run("a PtahSchema with "+row.name, func(t *testing.T) {
			t.Parallel()

			run := unstartedSchemaApplyRun(t)
			firstPodStarted(t, run.api, run.pod, row.state)
			aSecondPod(t, run.api, run.job, run.pod)
			run.reconcileOnce(t)

			failed := settledSchemaFailure(t, run.api, run.schema)
			if !strings.Contains(failed.Message, "Job produced multiple executor Pods") {
				t.Fatalf("ReconciliationFailed = %q, want the multiplicity sentence", failed.Message)
			}
			check(t, failed.Message, run.said)
		})
		t.Run("a PtahMigration with "+row.name, func(t *testing.T) {
			t.Parallel()

			run := unstartedMigrationApplyRun(t)
			firstPodStarted(t, run.api, run.pod, row.state)
			aSecondPod(t, run.api, run.job, run.pod)
			if stored := reconcileMigration(t, run.reconciler, run.api, run.migration); stored.Status.UnresolvedRun == nil {
				t.Fatalf("the ending did not record the run: claim %#v", stored.Status.ActiveOperation)
			}
			podsGone(t, run.api, run.job)

			record := run.settle(t, 3)
			if !strings.Contains(record.Message, errTerminalPodMultiplicity.Error()) {
				t.Fatalf("last run message = %q, want the multiplicity sentence", record.Message)
			}
			check(t, record.Message, run.said)
		})
	}
}

// An Apply the API server refused a Pod until the Job's deadline has no Pod
// log, so neither record points at one.
func TestARefusedApplyPointsAtNoPodLog(t *testing.T) {
	t.Parallel()

	t.Run("a PtahSchema", func(t *testing.T) {
		t.Parallel()

		run := schemaApplyUnderReport(t, func(api client.Client, job *batchv1.Job) *corev1.Pod {
			refusedPodCreation(t, api, job, nil, metav1.Now())
			return nil
		}, operatorv1alpha1.ReasonPodAdmissionRefused, "denied request")
		pastTheDeadlineWithoutAPod(t, run.api, run.job)
		deadline := safetyGetSchema(t, run.api, run.schema).Status.ActiveOperation.ExecutionNotAfter.Add(time.Second)
		run.reconciler.Clock = func() time.Time { return deadline }
		run.reconcileOnce(t)

		event := settledSchemaEvent(t, run.recorder)
		if !strings.Contains(event, lastReportedBefore+run.said) {
			t.Fatalf("ApplyOutcomeUnknown = %q, want the refusal, or this is not the refused Apply's Event", event)
		}
		if strings.Contains(event, "Pod log") || strings.Contains(event, "kubectl logs") {
			t.Fatalf("ApplyOutcomeUnknown = %q points at the log of a Pod that never existed", event)
		}
		if failed := settledSchemaFailure(t, run.api, run.schema); strings.Contains(failed.Message, "kubectl logs") {
			t.Fatalf("ReconciliationFailed = %q points at the log of a Pod that never existed", failed.Message)
		}
	})
	t.Run("a PtahMigration", func(t *testing.T) {
		t.Parallel()

		run := migrationApplyUnderReport(t, func(t *testing.T, run *unstartedMigration) {
			t.Helper()
			refusedPodCreation(t, run.api, run.job, run.pod, metav1.NewTime(run.reconciler.now()))
			run.pod = nil
		}, operatorv1alpha1.ReasonPodAdmissionRefused, "denied request")
		pastTheDeadlineWithoutAPod(t, run.api, run.job)

		record := run.settle(t, 3)
		if !strings.Contains(record.Message, lastReportedBefore+run.said) {
			t.Fatalf("last run message = %q, want the refusal, or this is not the refused Apply's record", record.Message)
		}
		if event := findEvent(run.recorder, corev1.EventTypeWarning+" MigrationRunUncertain "); event == "" ||
			strings.Contains(event, "Pod log") || strings.Contains(event, "kubectl logs") {
			t.Fatalf("MigrationRunUncertain = %q, want one that points at no Pod log", event)
		}
	})
}
