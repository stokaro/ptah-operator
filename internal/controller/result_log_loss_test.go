package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// apiServerLogAnswer is what an API server answers a pods/log request with,
// written the way it writes it: an error is a Status document under its code.
type apiServerLogAnswer struct {
	code int
	// status is the error document, or nil for a 200 whose body is text.
	status *metav1.Status
	body   string
	// stall holds the request until the client gives up on it.
	stall bool
}

func statusOf(err error) *metav1.Status {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		panic(fmt.Sprintf("%v carries no Status", err))
	}
	document := status.Status()
	document.Kind, document.APIVersion = "Status", "v1"
	return &document
}

// readThroughAPIServer reads one executor log through the reader the manager
// runs, a clientset, against a server answering as the API server does.
func readThroughAPIServer(t *testing.T, answer apiServerLogAnswer, closed bool) ([]byte, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/namespaces/team-a/pods/apply-pod/log" ||
			request.URL.Query().Get("container") != executorContainerName {
			t.Errorf("the reader asked for %s", request.URL)
		}
		if answer.stall {
			<-request.Context().Done()
			return
		}
		if answer.status != nil {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(answer.code)
			_ = json.NewEncoder(writer).Encode(answer.status)
			return
		}
		writer.WriteHeader(answer.code)
		_, _ = writer.Write([]byte(answer.body))
	}))
	if closed {
		server.Close()
	} else {
		defer server.Close()
	}
	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return ClientsetPodLogs{Client: clientset}.Read(ctx, "team-a", "apply-pod", executorContainerName)
}

// Every answer an API server gives a pods/log read, as a clientset receives it,
// is sorted by what it says about the log. Each row is the answer a real
// failure produces; the kubelet and API server code each one comes from is
// named beside it.
func TestAFailedResultLogReadIsClassifiedByWhatTheAPIServerSaid(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		answer apiServerLogAnswer
		closed bool
		want   resultLogFailure
	}{
		{
			// pod.LogLocation looks up the Node to reach its kubelet; a deleted
			// node is a NotFound for "nodes".
			name: "the node the Pod ran on was deleted",
			answer: apiServerLogAnswer{code: http.StatusNotFound, status: statusOf(
				apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, "kind-worker2"))},
			want: resultLogGone,
		},
		{
			// The kubelet answers 400 when the container status it reads names
			// no container it can serve, and the API server passes it on.
			name: "the kubelet can no longer serve the container's log",
			answer: apiServerLogAnswer{code: http.StatusBadRequest, status: statusOf(apierrors.NewBadRequest(
				`container "ptah" in pod "apply-pod" is terminated`))},
			want: resultLogGone,
		},
		{
			name: "the Pod itself was deleted",
			answer: apiServerLogAnswer{code: http.StatusNotFound, status: statusOf(
				apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "apply-pod"))},
			want: resultLogPodGone,
		},
		{
			// A kubelet that has just restarted does not know the Pod until it
			// resyncs; the API server wraps its 404 for pods/log.
			name: "the kubelet does not know the Pod yet",
			answer: apiServerLogAnswer{code: http.StatusNotFound, status: statusOf(apierrors.NewGenericServerResponse(
				http.StatusNotFound, "", schema.GroupResource{Resource: "pods/log"}, "apply-pod",
				`pod "apply-pod" does not exist`, 0, false))},
			want: resultLogTransient,
		},
		{
			// The API server could not dial the kubelet: the node is away.
			name: "the kubelet cannot be reached",
			answer: apiServerLogAnswer{code: http.StatusInternalServerError, status: statusOf(apierrors.NewInternalError(errors.New(
				`Get "https://172.18.0.3:10250/containerLogs/team-a/apply-pod/ptah": dial tcp 172.18.0.3:10250: connect: no route to host`)))},
			want: resultLogTransient,
		},
		{
			name:   "the API server is shedding load",
			answer: apiServerLogAnswer{code: http.StatusTooManyRequests, status: statusOf(apierrors.NewTooManyRequests("slow down", 1))},
			want:   resultLogTransient,
		},
		{
			name:   "the API server is unavailable",
			answer: apiServerLogAnswer{code: http.StatusServiceUnavailable, status: statusOf(apierrors.NewServiceUnavailable("unavailable"))},
			want:   resultLogTransient,
		},
		{
			name:   "the API server cannot be reached at all",
			closed: true,
			want:   resultLogTransient,
		},
		{
			name:   "the read ran out of time",
			answer: apiServerLogAnswer{stall: true},
			want:   resultLogTransient,
		},
		{
			name: "this manager may not read logs",
			answer: apiServerLogAnswer{code: http.StatusForbidden, status: statusOf(apierrors.NewForbidden(
				schema.GroupResource{Resource: "pods/log"}, "apply-pod", errors.New("RBAC denied")))},
			want: resultLogDenied,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := readThroughAPIServer(t, row.answer, row.closed)
			if err == nil {
				t.Fatal("the read succeeded, so this row classified nothing")
			}
			if got := classifyResultLogError(err); got != row.want {
				t.Fatalf("classifyResultLogError(%v) = %d, want %d", err, got, row.want)
			}
		})
	}
}

// Once the kubelet has started answering, a container that was garbage
// collected or a log file that was removed ends the stream with the reason as
// its body under a 200 (kubelet GetKubeletContainerLogs writes an empty chunk
// before it asks the runtime). That is a read that succeeded and a log that
// holds no frame, and the frame parser, not the classifier, is what answers it.
func TestALogTheKubeletLostAfterAnsweringReadsAsALogWithNoFrame(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		"unable to retrieve container logs for containerd://3f2a1b",
		`failed to try resolving symlinks in path "/var/log/pods/team-a_apply-pod_uid/ptah/0.log": lstat /var/log/pods/team-a_apply-pod_uid: no such file or directory`,
	} {
		logs, err := readThroughAPIServer(t, apiServerLogAnswer{code: http.StatusOK, body: body}, false)
		if err != nil || string(logs) != body {
			t.Fatalf("read = %q, %v, want the kubelet's text as the log", logs, err)
		}
		if _, parseErr := runner.ParseResultFor(logs, runner.OperationMigrationApply, testDigest); !errors.Is(parseErr, runner.ErrFrameNotFound) {
			t.Fatalf("ParseResultFor(%q) = %v, want no frame", body, parseErr)
		}
	}
}

type failingLogs struct {
	err   error
	calls int
}

func (l *failingLogs) Read(context.Context, string, string, string) ([]byte, error) {
	l.calls++
	return nil, l.err
}

// A transient failure is given resultLogLossWindow, measured from the first
// failure, and a read that succeeds in between starts the measure again. A
// permanent one is lost at once, and one that says nothing about the log is
// neither counted nor lost.
func TestAResultLogIsGivenUpOnlyWhenItIsGoneOrHasFailedForTheWholeWindow(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "apply-pod", UID: "apply-pod-uid"}}
	unreachable := apierrors.NewInternalError(errors.New("dial tcp 172.18.0.3:10250: connect: no route to host"))
	start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	read := func(failures *resultLogFailures, reader PodLogReader, at time.Duration) ([]byte, error) {
		return readResultLog(context.Background(), reader, failures, start.Add(at), 0, 0, pod)
	}

	failures := &resultLogFailures{}
	transient := &failingLogs{err: unreachable}
	for _, at := range []time.Duration{0, time.Minute, resultLogLossWindow - time.Second} {
		if _, err := read(failures, transient, at); !errors.Is(err, errResultReadRetry) {
			t.Fatalf("a transient failure %s after the first = %v, want a retry", at, err)
		}
	}
	_, err := read(failures, transient, resultLogLossWindow)
	var lost *resultLogLost
	if !errors.As(err, &lost) || lost.failingFor != resultLogLossWindow || !errors.Is(err, runner.ErrFrameNotFound) {
		t.Fatalf("a transient failure after the whole window = %v, want the log lost after %s", err, resultLogLossWindow)
	}

	// A success in between: the next failure starts the window again.
	failures = &resultLogFailures{}
	if _, err := read(failures, transient, 0); !errors.Is(err, errResultReadRetry) {
		t.Fatalf("first failure = %v", err)
	}
	if logs, err := read(failures, staticLogs{content: []byte("log")}, time.Minute); err != nil || string(logs) != "log" {
		t.Fatalf("a read that succeeds = %q, %v", logs, err)
	}
	if _, err := read(failures, transient, resultLogLossWindow); !errors.Is(err, errResultReadRetry) {
		t.Fatalf("a failure after a success = %v, want the window started again", err)
	}

	// Gone at once.
	failures = &resultLogFailures{}
	nodeGone := &failingLogs{err: apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, "kind-worker2")}
	if _, err := read(failures, nodeGone, 0); !errors.As(err, &lost) || lost.failingFor != 0 {
		t.Fatalf("a deleted node = %v, want the log lost at once", err)
	}

	// Neither counted nor lost.
	for name, err := range map[string]error{
		"the Pod is gone": apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "apply-pod"),
		"reads are denied": apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "apply-pod",
			errors.New("RBAC denied")),
	} {
		failures = &resultLogFailures{}
		for _, at := range []time.Duration{0, 10 * resultLogLossWindow} {
			got, readErr := read(failures, &failingLogs{err: err}, at)
			if got != nil || !errors.Is(readErr, err) || errors.Is(readErr, errResultReadRetry) || errors.As(readErr, &lost) {
				t.Fatalf("%s at %s = %v, want the read error itself", name, at, readErr)
			}
		}
	}

	// A process that is stopping learned nothing about the log.
	failures = &resultLogFailures{}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readResultLog(canceled, transient, failures, start, 0, 0, pod); errors.Is(err, errResultReadRetry) || errors.As(err, &lost) {
		t.Fatalf("a read canceled by shutdown = %v, want the read error itself", err)
	}
	if len(failures.first) != 0 {
		t.Fatalf("a read canceled by shutdown was counted as a failure: %v", failures.first)
	}
}

// A migration Apply whose log is gone is settled from its termination summary,
// by the same decision a frame reaches, and one whose log is merely failing is
// waited on for the window first. Without a summary it is unknown, as it was.
func TestAMigrationApplyWhoseLogIsGoneSettlesFromItsTerminationSummary(t *testing.T) {
	t.Parallel()

	nodeGone := apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, "kind-worker2")
	unreachable := apierrors.NewInternalError(errors.New("dial tcp 172.18.0.3:10250: connect: no route to host"))
	podGone := apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "apply-pod")
	for _, row := range []struct {
		name        string
		err         error
		noSummary   bool
		wait        bool
		wantOutcome operatorv1alpha1.MigrationRunOutcome
		wantPhase   operatorv1alpha1.MigrationPhase
		wantMessage string
	}{
		{
			name: "the node is gone", err: nodeGone,
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeFailed, wantPhase: operatorv1alpha1.MigrationPhaseVerifyingHistory,
			wantMessage: "termination message",
		},
		{
			name: "the kubelet stays unreachable for the whole window", err: unreachable, wait: true,
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeFailed, wantPhase: operatorv1alpha1.MigrationPhaseVerifyingHistory,
			wantMessage: "termination message",
		},
		{
			name: "the node is gone and the container wrote no summary", err: nodeGone, noSummary: true,
			wantOutcome: operatorv1alpha1.MigrationRunOutcomeUnknown, wantPhase: operatorv1alpha1.MigrationPhaseBlocked,
			wantMessage: "the result log is gone",
		},
		{
			// The Pod is gone: nothing is decided on this pass, and the next
			// one finds no Pod.
			name: "the Pod is gone", err: podGone,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, plan := awaitingApprovalFixture(t)
			operation := applyClaimFor(t, migration, plan)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			if !row.noSummary {
				encoded, err := runner.EncodeResult(runner.Result{
					ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
					OperationID: operation.ID, ChildExitCode: -1, MutationStarted: true,
					CoordinationDigest:   operation.CoordinationDigest,
					TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
					MigrationRun: &dataplane.MigrationRunReport{
						ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up",
						Outcome: dataplane.MigrationOutcomeFailed, Planned: []int64{3, 4}, Applied: []int64{3},
					},
					Error: &runner.ResultError{Code: "execution_error", Message: "stopped"},
				})
				if err != nil || encoded.SummaryErr != nil {
					t.Fatalf("EncodeResult() = %v, %v", err, encoded.SummaryErr)
				}
				pod.Status.ContainerStatuses[0].State.Terminated.Message = string(encoded.Summary)
			}
			logs := &failingLogs{err: row.err}
			reconciler, api := fakeMigrationReconciler(t, logs, migration, plan, job, pod, verificationPolicyConfigMap())
			recorder := record.NewFakeRecorder(64)
			reconciler.Recorder = recorder
			clock := movableMigrationClock(reconciler, api)
			holdMigrationApplyLease(t, reconciler, api, migration)

			result, err := reconciler.Reconcile(context.Background(), migrationRequest(migration))
			if row.wantOutcome == "" {
				if err == nil || readMigration(t, api, migration).Status.ActiveOperation == nil {
					t.Fatalf("Reconcile() = %v, %v; want the error returned and the claim kept", result, err)
				}
				return
			}
			if row.wait {
				if err != nil || result.RequeueAfter != resultReadRetryInterval {
					t.Fatalf("a first transient failure = %v, %v; want a requeue after %s", result, err, resultReadRetryInterval)
				}
				if held := readMigration(t, api, migration); held.Status.ActiveOperation == nil || held.Status.LastRun != nil {
					t.Fatalf("a transient failure inside the window decided the run: %#v", held.Status.LastRun)
				}
				clock.now = clock.now.Add(resultLogLossWindow - time.Second)
				if result, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil ||
					result.RequeueAfter != resultReadRetryInterval {
					t.Fatalf("a failure just inside the window = %v, %v; want another requeue", result, err)
				}
				clock.now = clock.now.Add(time.Second)
				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
					t.Fatalf("Reconcile() past the window error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			run := actual.Status.LastRun
			if run == nil || run.Outcome != row.wantOutcome || actual.Status.Phase != row.wantPhase {
				t.Fatalf("last run = %#v in phase %q, want %q in %q", run, actual.Status.Phase, row.wantOutcome, row.wantPhase)
			}
			if !strings.Contains(run.Message, row.wantMessage) {
				t.Fatalf("run message = %q, want it to say %q", run.Message, row.wantMessage)
			}
			if row.wait && logs.calls != 3 {
				t.Fatalf("the log was read %d times, want once per pass", logs.calls)
			}
		})
	}
}

// Every family reads a lost log as a log that holds no frame: a schema Apply
// is unknown and a read-only operation is tried again, instead of retrying the
// read for as long as the Pod exists.
func TestTheSchemaFamilyReadsALostLogAsAMissingFrame(t *testing.T) {
	t.Parallel()

	nodeGone := apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, "kind-worker2")

	schemaApply, plan, policyConfig := dispatchedApplyWithPlan(t)
	job, pod := terminalWorkload(schemaApply, batchv1.JobComplete)
	reconciler, api := fakeReconciler(t, &failingLogs{err: nodeGone}, schemaApply, plan, policyConfig, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schemaApply)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	pending := safetyGetSchema(t, api, schemaApply).Status.PendingObservation
	if pending == nil || pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
		t.Fatalf("pending observation = %#v, want an Apply whose outcome is unknown", pending)
	}

	resolve := schemaFixture()
	resolve.Finalizers = []string{activeOperationFinalizer}
	resolve.Status.Phase = operatorv1alpha1.PhaseResolving
	resolve.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationResolve, ID: testDigest, InputFingerprint: testDigest,
		JobName: "resolve-job", JobUID: "job-uid", StartedAt: metav1.NewTime(time.Date(2026, 9, 17, 16, 0, 0, 0, time.UTC)), Attempt: 1,
	}
	bindActiveInput(t, resolve)
	job, pod = terminalWorkload(resolve, batchv1.JobComplete)
	reconciler, api = fakeReconciler(t, &failingLogs{err: nodeGone}, resolve, job, pod)
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(resolve)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	retried := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(resolve), retried); err != nil {
		t.Fatal(err)
	}
	if operation := retried.Status.ActiveOperation; operation == nil || operation.Attempt != 2 || operation.JobUID != "" ||
		retried.Status.Phase != operatorv1alpha1.PhaseFailed {
		t.Fatalf("a read-only operation whose log is gone = %#v in phase %q, want its second attempt",
			retried.Status.ActiveOperation, retried.Status.Phase)
	}
}
