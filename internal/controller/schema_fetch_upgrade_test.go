package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// fetchReasonMessage is what the runner leaves in the fetch step's termination
// message, spelled out rather than built, because it is what a Pod in flight
// across an upgrade or a rollback holds.
const (
	fetchReasonSentence = "parse schema artifact: parse HCL schema: schema.hcl:3,31-32: Invalid single-argument block definition"
	fetchReasonMessage  = "PTAH_RUNNER_FETCH_REASON_V1 " + fetchReasonSentence + "\n"
)

// ptahAsTheFetch is the fetch step before the runner went around it: Ptah itself,
// with no runner on its volumes and no protocol to hold to. It is what a Job the
// previous release dispatched runs, and what the previous release rebuilds after
// a rollback.
func ptahAsTheFetch(t *testing.T, job *batchv1.Job) {
	t.Helper()
	found := false
	for index := range job.Spec.Template.Spec.InitContainers {
		fetch := &job.Spec.Template.Spec.InitContainers[index]
		if fetch.Name != workload.SchemaFetchContainerName {
			continue
		}
		found = true
		fetch.Command = []string{"/usr/local/bin/ptah"}
		fetch.Args = []string{"schema", "pull", fetch.Args[1], "--out", fetch.Args[3]}
		fetch.Env = slices.DeleteFunc(fetch.Env, func(variable corev1.EnvVar) bool {
			return variable.Name == runner.EnvRunnerProtocolVersion
		})
		fetch.VolumeMounts = slices.DeleteFunc(fetch.VolumeMounts, func(mount corev1.VolumeMount) bool {
			return mount.Name == "runner"
		})
	}
	if !found {
		t.Fatal("the Job has no schema fetch to rewrite")
	}
}

// TestAReadAcrossTheFetchChangeIsRunAgain builds the Job on both sides of the
// release that put the runner around the fetch, with the real builder, and holds
// the check that decides what a manager does with a Job it did not build. A
// release that changed the Pod template cannot adopt it, in either direction:
// the claim is read-only, so the read runs again under a new attempt.
func TestAReadAcrossTheFetchChangeIsRunAgain(t *testing.T) {
	t.Parallel()

	builder := workload.Builder{
		ExecutorImage:          "example.invalid/ptah@" + testDigest,
		RunnerImage:            testRunnerImage,
		PtahVersion:            "v0.3.0",
		ControllerImage:        testControllerImage,
		ControllerRevision:     testControllerRevision,
		ControllerStateVersion: testControllerStateVersion,
	}
	claim := func(t *testing.T) (*operatorv1alpha1.PtahSchema, *operatorv1alpha1.ActiveOperationStatus) {
		t.Helper()
		schema := readOnlySchemaClaim(t, operatorv1alpha1.OperationObserve)
		operation := schema.Status.ActiveOperation
		operation.AdmissionSnapshot = nil
		operation.Source = &operatorv1alpha1.SourceAccessBinding{
			ResolvedReference: schema.Status.Source.ResolvedReference, Digest: schema.Status.Source.Digest,
		}
		name, err := workload.NameFor(schema, *operation)
		if err != nil {
			t.Fatal(err)
		}
		operation.JobName = name
		return schema, operation
	}
	// live is the Job as dispatched: the template its claim's snapshot recorded.
	live := func(t *testing.T, schema *operatorv1alpha1.PtahSchema, operation *operatorv1alpha1.ActiveOperationStatus, shape func(*testing.T, *batchv1.Job)) *batchv1.Job {
		t.Helper()
		built, err := builder.Build(schema, *operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		shape(t, built)
		operation.AdmissionSnapshot = testAdmissionSnapshotOf(&built.Spec.Template)
		job, err := builder.Build(schema, *operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		shape(t, job)
		job.UID = "live-job-uid"
		return job
	}
	asBuilt := func(*testing.T, *batchv1.Job) {}

	t.Run("the same release adopts its own Job", func(t *testing.T) {
		t.Parallel()
		schema, operation := claim(t)
		job := live(t, schema, operation, asBuilt)
		rebuilt, err := builder.Build(schema, *operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateAdoptedJobIntent(job, rebuilt, schema, operation); err != nil {
			t.Fatalf("a Job of this shape was refused, so the rows below would prove nothing: %v", err)
		}
	})
	t.Run("an upgrade finds the previous release's fetch", func(t *testing.T) {
		t.Parallel()
		schema, operation := claim(t)
		job := live(t, schema, operation, ptahAsTheFetch)
		rebuilt, err := builder.Build(schema, *operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateAdoptedJobIntent(job, rebuilt, schema, operation); err == nil ||
			!strings.Contains(err.Error(), "spec is not the one its claim builds") {
			t.Fatalf("validateAdoptedJobIntent() = %v, want a refusal of the Job's spec", err)
		}
	})
	t.Run("a rollback finds this release's fetch", func(t *testing.T) {
		t.Parallel()
		schema, operation := claim(t)
		job := live(t, schema, operation, asBuilt)
		rebuilt, err := builder.Build(schema, *operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		ptahAsTheFetch(t, rebuilt)
		if err := validateAdoptedJobIntent(job, rebuilt, schema, operation); err == nil ||
			!strings.Contains(err.Error(), "spec is not the one its claim builds") {
			t.Fatalf("validateAdoptedJobIntent() = %v, want a refusal of the Job's spec", err)
		}
	})
}

// fetchMovedJobs builds a Job whose fetch step is not the one the live Job runs,
// as a release that moved the step does.
type fetchMovedJobs struct{ fakeJobs }

func (fetchMovedJobs) Build(
	schema *operatorv1alpha1.PtahSchema,
	operation operatorv1alpha1.ActiveOperationStatus,
	plan *operatorv1alpha1.PtahSchemaPlan,
) (*batchv1.Job, error) {
	job, err := (fakeJobs{}).Build(schema, operation, plan)
	if err != nil {
		return nil, err
	}
	job.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: workload.SchemaFetchContainerName, Command: []string{"/runner/ptah-runner"}}}
	return job, nil
}

// TestAReadInFlightAcrossTheFetchChangeMovesToANewAttempt is what the refusal above
// does in a pass: the claim goes on under a new attempt, it reports why, and it
// says nothing about the file. The Job is left to its own deadline and is not
// written to, since the webhook would judge a cleanup TTL by the same matcher.
func TestAReadInFlightAcrossTheFetchChangeMovesToANewAttempt(t *testing.T) {
	t.Parallel()

	for _, operation := range []operatorv1alpha1.OperationType{operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			schema := readOnlySchemaClaim(t, operation)
			attempt := schema.Status.ActiveOperation.Attempt
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			job.Status.Conditions = nil
			pod.Status = corev1.PodStatus{Phase: corev1.PodRunning}
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, job, pod)
			reconciler.Locks = targetlock.New(api, api, nil)
			reconciler.Recorder = record.NewFakeRecorder(32)
			reconciler.Jobs = fetchMovedJobs{}

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := safetyGetSchema(t, api, schema)
			if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Attempt != attempt+1 ||
				actual.Status.ActiveOperation.JobUID != "" {
				t.Fatalf("the claim = %+v, want attempt %d with no Job", actual.Status.ActiveOperation, attempt+1)
			}
			failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
			if failed == nil || failed.Status != metav1.ConditionTrue || !strings.HasPrefix(failed.Message, "active Job intent changed: ") {
				t.Fatalf("ReconciliationFailed = %#v, want the reason the Job could not be adopted", failed)
			}
			left := &batchv1.Job{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), left); err != nil {
				t.Fatal(err)
			}
			if left.Spec.TTLSecondsAfterFinished != nil {
				t.Fatalf("the Job was given a cleanup TTL of %d", *left.Spec.TTLSecondsAfterFinished)
			}
		})
	}
}

// TestAFetchThatCouldNotParseSaysWhy reads the reason where a person reads it, with
// the Pods a schema Job can leave: this release's, a previous release's, and the
// fetch step of another source. Only the schema fetch's own message crosses.
func TestAFetchThatCouldNotParseSaysWhy(t *testing.T) {
	t.Parallel()

	stopped := func(step, message string) []corev1.ContainerStatus {
		return []corev1.ContainerStatus{
			{Name: "install-runner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: step, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Message: message}}},
		}
	}
	const (
		carried = "desired_source_unreadable: " + fetchReasonSentence
		named   = " step exited 2, so the run never started"
	)
	for _, row := range []struct {
		name     string
		statuses []corev1.ContainerStatus
		want     string
	}{
		{name: "this release's fetch", statuses: stopped("fetch-schema", fetchReasonMessage), want: carried},
		{name: "behind the runtime's words", statuses: stopped("fetch-schema", "runtime said: "+fetchReasonMessage), want: carried},
		{name: "a fetch that wrote nothing, as a previous release's does", statuses: stopped("fetch-schema", ""), want: "the fetch-schema" + named},
		{name: "a refusal of the registry", statuses: stopped("fetch-schema", "error: pull oci://registry.example: unauthorized\n"), want: "the fetch-schema" + named},
		{name: "another sentence behind the marker", statuses: stopped("fetch-schema", "PTAH_RUNNER_FETCH_REASON_V1 pull oci://registry.example: unauthorized\n"), want: "the fetch-schema" + named},
		{name: "a Git fetch, whose message is the end of its log", statuses: stopped("fetch-git-source", fetchReasonMessage), want: "the fetch-git-source" + named},
		{name: "a migration's fetch", statuses: stopped("fetch-migrations", fetchReasonMessage), want: "the fetch-migrations" + named},
		{name: "the guard", statuses: stopped("validate-source-authority", fetchReasonMessage), want: "the validate-source-authority" + named},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema := readOnlySchemaClaim(t, operatorv1alpha1.OperationObserve)
			job, pod := terminalWorkload(schema, batchv1.JobFailed)
			pod.Status.InitContainerStatuses = row.statuses
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, job, pod)
			reconciler.Locks = targetlock.New(api, api, nil)
			recorder := record.NewFakeRecorder(32)
			reconciler.Recorder = recorder

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := safetyGetSchema(t, api, schema)
			failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
			if failed == nil || failed.Status != metav1.ConditionTrue || failed.Message != row.want {
				t.Fatalf("ReconciliationFailed = %#v, want the message %q", failed, row.want)
			}
			if event := findEvent(recorder, "Warning OperationFailed"); !strings.HasSuffix(event, row.want) {
				t.Fatalf("OperationFailed event = %q, want it to end with %q", event, row.want)
			}
		})
	}
}
