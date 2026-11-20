package controller

import (
	"context"
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
)

// unreadableReason is the reason the runner reported for the file
// stokaro/ptah-operator#788 delivers, as the pinned image words it.
const unreadableReason = "error parsing schema file: read SQL schema file /source/schema/schema.sql:2:29: " +
	"unsupported table option: INHERITS"

// readOnlySchemaClaim is a schema whose read-only operation has finished.
func readOnlySchemaClaim(t *testing.T, operation operatorv1alpha1.OperationType) *operatorv1alpha1.PtahSchema {
	t.Helper()
	schema := safetyApplySchema(t)
	schema.Status.Phase = operatorv1alpha1.PhaseObserving
	schema.Status.Plan = nil
	schema.Status.Source.Verified = true
	schema.Status.Source.Digest = testDigest
	schema.Status.Source.ResolvedReference = "oci://registry.example/team/schema@" + testDigest
	schema.Status.Target.DriftReportDigest = testDigest
	schema.Status.ActiveOperation.Type = operation
	schema.Status.ActiveOperation.ID = "read-operation"
	schema.Status.ActiveOperation.JobName = "read-job"
	schema.Status.ActiveOperation.LeaseEpoch = testLeaseEpoch
	bindActiveInput(t, schema)
	return schema
}

// TestAFailedFetchIsNamedAsItsStep covers the schema an OCI artifact delivers,
// which `schema pull --skip-parse` checks in an init container: an artifact it
// refuses, such as a layer of another media type, ends the Pod there, before
// the runner can speak. The step and its exit status
// are structured fields, and what the container wrote stays in its log. A Job
// that failed with no init container to blame keeps the sentence it had.
//
// A step that checks the source's access before the fetch is the exception:
// its refusal record carries the code and the sentence a Resolve's frame
// would, and that is what the failure says, for each kind of source.
func TestAFailedFetchIsNamedAsItsStep(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		statuses []corev1.ContainerStatus
		want     string
	}{
		{
			name: "the OCI guard refused the access",
			statuses: []corev1.ContainerStatus{
				accessStepStatus(t, "validate-source-authority", runner.CodeInvalidOCIAccess, "registry credential is not granted for the OCI source authority"),
			},
			want: "the validate-source-authority step refused the source's access, so the run never started: " +
				"invalid_oci_access: registry credential is not granted for the OCI source authority",
		},
		{
			name: "the Git fetch refused the access",
			statuses: []corev1.ContainerStatus{
				{Name: "install-runner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				accessStepStatus(t, "fetch-git-source", runner.CodeInvalidGitAccess, "the Git credential is not granted for the source's host"),
			},
			want: "the fetch-git-source step refused the source's access, so the run never started: " +
				"invalid_git_access: the Git credential is not granted for the source's host",
		},
		{
			name: "the HTTPS fetch refused the access",
			statuses: []corev1.ContainerStatus{
				accessStepStatus(t, "fetch-https-source", runner.CodeInvalidHTTPSAccess, "the HTTPS credential is not granted for the source's host"),
			},
			want: "the fetch-https-source step refused the source's access, so the run never started: " +
				"invalid_https_access: the HTTPS credential is not granted for the source's host",
		},
		{
			name: "the guard's record names another kind's code",
			statuses: []corev1.ContainerStatus{
				accessStepStatus(t, "validate-source-authority", runner.CodeInvalidGitAccess, "FRAME-TEXT"),
			},
			want: "the validate-source-authority step exited 2, so the run never started",
		},
		{
			name: "the fetch failed",
			statuses: []corev1.ContainerStatus{
				{Name: "authorize-oci-source", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: "fetch-schema", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 2, Message: "error: parse schema artifact: FRAME-TEXT",
				}}},
			},
			want: "the fetch-schema step exited 2, so the run never started",
		},
		{
			name: "every init container succeeded",
			statuses: []corev1.ContainerStatus{
				{Name: "fetch-schema", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			},
			want: "read Observe result: ",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := readOnlySchemaClaim(t, operatorv1alpha1.OperationObserve)
			job, pod := terminalWorkload(schema, batchv1.JobFailed)
			pod.Status.InitContainerStatuses = test.statuses
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, job, pod)
			reconciler.Locks = targetlock.New(api, api, nil)
			recorder := record.NewFakeRecorder(32)
			reconciler.Recorder = recorder

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := safetyGetSchema(t, api, schema)
			failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
			if failed == nil || failed.Status != metav1.ConditionTrue || !strings.HasPrefix(failed.Message, test.want) {
				t.Fatalf("ReconciliationFailed = %#v, want it to start with %q", failed, test.want)
			}
			if strings.Contains(test.want, "refused the source's access") {
				if event := findEvent(recorder, "Warning OperationFailed"); !strings.Contains(event, test.want) {
					t.Fatalf("OperationFailed event = %q, want it to say %q", event, test.want)
				}
			}
			assertNoFrameText(t, recorder, actual.Status)
		})
	}
}

// A History meets the same refusal before its Git fetch, and its failure says
// what the schema's does.
func TestAMigrationSaysWhyItsSourceAccessWasRefused(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Artifact = resolvedMigrationArtifact()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobFailed)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		accessStepStatus(t, "fetch-git-source", runner.CodeInvalidGitAccess, "the Git credential is not granted for the source's host"),
	}
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, job, pod, verificationPolicyConfigMap())
	recorder := record.NewFakeRecorder(32)
	reconciler.Recorder = recorder

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	want := "the fetch-git-source step refused the source's access, so the run never started: " +
		"invalid_git_access: the Git credential is not granted for the source's host"
	progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Reason != string(operatorv1alpha1.ReasonOperationFailed) || progressing.Message != want {
		t.Fatalf("Progressing = %#v, want OperationFailed with %q", progressing, want)
	}
	if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Attempt != operation.Attempt+1 {
		t.Fatalf("the refusal did not retry the claim: %+v", actual.Status.ActiveOperation)
	}
}

// TestInitBoundaryReadsOnlyARecordFromAStepThatChecksAccess holds the reading
// to the record, from a step that checks a source's access, under that step's
// code. A record anywhere else, or anything that is not exactly a record, is
// named by the step's exit status and says nothing more.
func TestInitBoundaryReadsOnlyARecordFromAStepThatChecksAccess(t *testing.T) {
	t.Parallel()

	record := func(code, message string) string {
		encoded, err := runner.EncodeAccessRefusal(runner.AccessRefusal{Code: code, Message: message})
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	gitRecord := record(runner.CodeInvalidGitAccess, "the Git credential is not granted for the source's host")
	long := strings.Repeat("x", 1000)
	for _, row := range []struct {
		name, container, message, want string
	}{
		{name: "the record", container: "fetch-git-source", message: gitRecord,
			want: "the fetch-git-source step refused the source's access, so the run never started: invalid_git_access: the Git credential is not granted for the source's host"},
		{name: "a long sentence is bounded as a frame's is", container: "fetch-https-source", message: record(runner.CodeInvalidHTTPSAccess, long),
			want: "the fetch-https-source step refused the source's access, so the run never started: invalid_https_access: " + long[:512] + "..."},
		{name: "a record from the fetch of an OCI source", container: "fetch-schema", message: record(runner.CodeInvalidOCIAccess, "FRAME-TEXT"),
			want: "the fetch-schema step exited 2, so the run never started"},
		{name: "a record under another kind's code", container: "fetch-https-source", message: gitRecord,
			want: "the fetch-https-source step exited 2, so the run never started"},
		{name: "the runtime's text in front of the record", container: "fetch-git-source", message: ": " + gitRecord,
			want: "the fetch-git-source step exited 2, so the run never started"},
		{name: "a log tail that quotes a record", container: "fetch-git-source", message: "ptah-runner: FRAME-TEXT\n" + gitRecord,
			want: "the fetch-git-source step exited 2, so the run never started"},
		{name: "the line the runner logs", container: "fetch-git-source",
			message: "ptah-runner: git_source_refused: the Git credential is not granted for the source's host\n",
			want:    "the fetch-git-source step exited 2, so the run never started"},
		{name: "no message", container: "validate-source-authority",
			want: "the validate-source-authority step exited 2, so the run never started"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "install-runner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: gitRecord}}},
				{Name: row.container, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Message: row.message}}},
			}}}
			if got := initBoundaryOf([]*corev1.Pod{pod}); got != row.want {
				t.Fatalf("initBoundaryOf() = %q, want %q", got, row.want)
			}
		})
	}
}

// accessStepStatus is an init container that checked a source's access and
// refused it, with the record the runner leaves.
func accessStepStatus(t *testing.T, name, code, message string) corev1.ContainerStatus {
	t.Helper()
	record, err := runner.EncodeAccessRefusal(runner.AccessRefusal{Code: code, Message: message})
	if err != nil {
		t.Fatal(err)
	}
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 2, Message: string(record),
	}}}
}

// TestBoundedKeepsWholeCharacters cuts in the middle of a two-byte character. An
// Event or a condition holding half of one is text the API server may refuse.
func TestBoundedKeepsWholeCharacters(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name, value string
		limit       int
		want        string
	}{
		{name: "under the bound", value: "abc", limit: 4, want: "abc"},
		{name: "at the bound", value: "abcd", limit: 4, want: "abcd"},
		{name: "over the bound", value: "abcde", limit: 4, want: "abcd..."},
		{name: "a character across the bound", value: "abcé", limit: 4, want: "abc..."},
		{name: "a character at the bound", value: "abcdé", limit: 4, want: "abcd..."},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := bounded(test.value, test.limit); got != test.want {
				t.Fatalf("bounded(%q, %d) = %q, want %q", test.value, test.limit, got, test.want)
			}
		})
	}
}
