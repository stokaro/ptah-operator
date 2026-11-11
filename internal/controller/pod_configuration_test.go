package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// A Secret that does not exist yet leaves the operation's Job with one active
// Pod and nothing terminal until its deadline. The resource read as an operation
// in progress, and past the deadline as a missing result frame, while the only
// words that named the Secret sat on the Pod (#821). These rows read them onto
// the resource and back off it when the Pod starts.

const (
	missingSecretText = `secret "application-database" not found`
	missingKeyText    = `couldn't find key url in Secret application/application-database`
)

// configurationErrorPod is the Pod of job, its named container waiting as the
// kubelet leaves one it cannot configure.
func configurationErrorPod(job *batchv1.Job, reason, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: job.Namespace, Name: job.Name + "-abcde", UID: "configuration-error-pod",
			Labels:          map[string]string{batchv1.ControllerUidLabel: string(job.UID)},
			OwnerReferences: []metav1.OwnerReference{jobControllerReference(job)},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  executorContainerName,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
			}},
		},
	}
}

// recordedConfigurationErrors drains the recorder and counts the
// PodConfigurationError Warnings the passes so far recorded.
func recordedConfigurationErrors(recorder *record.FakeRecorder) []string {
	var warnings []string
	for len(recorder.Events) > 0 {
		if event := <-recorder.Events; strings.HasPrefix(event, corev1.EventTypeWarning+" PodConfigurationError ") {
			warnings = append(warnings, event)
		}
	}
	return warnings
}

func TestASchemaNamesTheSecretItsPodCannotStartWithout(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, api, schema, jobName := dispatchableApply(t, false)
	recorder := record.NewFakeRecorder(16)
	reconciler.Recorder = recorder
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	reconcileOnce := func() {
		t.Helper()
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	for range 4 {
		reconcileOnce()
		if applyDispatched(t, api, schema.Namespace, jobName) {
			break
		}
	}
	job := &batchv1.Job{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: schema.Namespace, Name: jobName}, job); err != nil {
		t.Fatalf("the harness never dispatched, so nothing below proves anything: %v", err)
	}
	job.Status.Active = 1
	if err := api.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	ready := func() *metav1.Condition {
		t.Helper()
		return meta.FindStatusCondition(safetyGetSchema(t, api, schema).Status.Conditions, operatorv1alpha1.ConditionReady)
	}

	// A Pod the kubelet has not said anything about is an operation in progress.
	reconcileOnce()
	if condition := ready(); condition == nil || condition.Reason != string(operatorv1alpha1.ReasonOperationInProgress) {
		t.Fatalf("Ready before the kubelet spoke = %#v, want OperationInProgress", condition)
	}
	if warnings := recordedConfigurationErrors(recorder); len(warnings) != 0 {
		t.Fatalf("recorded %q before the kubelet spoke", warnings)
	}

	pod := configurationErrorPod(job, createContainerConfigError, missingSecretText)
	if err := api.Create(ctx, pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if err := api.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcileOnce()
	condition := ready()
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != string(operatorv1alpha1.ReasonPodConfigurationError) {
		t.Fatalf("Ready with the Secret missing = %#v, want False/PodConfigurationError", condition)
	}
	if want := fmt.Sprintf("Job %s cannot start its Pod: %s", jobName, missingSecretText); condition.Message != want {
		t.Fatalf("Ready message = %q, want %q", condition.Message, want)
	}
	if stored := safetyGetSchema(t, api, schema); stored.Status.ActiveOperation == nil || stored.Status.ActiveOperation.JobName != jobName {
		t.Fatalf("reporting the Pod retired the claim: %#v", stored.Status.ActiveOperation)
	}
	warnings := recordedConfigurationErrors(recorder)
	if len(warnings) != 1 || !strings.Contains(warnings[0], missingSecretText) {
		t.Fatalf("PodConfigurationError Warnings = %q, want one naming the Secret", warnings)
	}

	// The same words on the next pass change nothing and record nothing.
	written := safetyGetSchema(t, api, schema).ResourceVersion
	reconcileOnce()
	if stored := safetyGetSchema(t, api, schema); stored.ResourceVersion != written {
		t.Fatalf("a pass with the same words wrote status: %s to %s", written, stored.ResourceVersion)
	}
	if warnings := recordedConfigurationErrors(recorder); len(warnings) != 0 {
		t.Fatalf("the same words recorded %q again", warnings)
	}

	// A Secret that arrives without the key changes the words, and that is news.
	stored := &corev1.Pod{}
	if err := api.Get(ctx, client.ObjectKeyFromObject(pod), stored); err != nil {
		t.Fatal(err)
	}
	stored.Status.ContainerStatuses[0].State.Waiting.Message = missingKeyText
	if err := api.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	reconcileOnce()
	if condition := ready(); condition == nil || !strings.Contains(condition.Message, missingKeyText) {
		t.Fatalf("Ready with the key missing = %#v, want the new words", condition)
	}
	if warnings := recordedConfigurationErrors(recorder); len(warnings) != 1 || !strings.Contains(warnings[0], missingKeyText) {
		t.Fatalf("PodConfigurationError Warnings = %q, want one naming the key", warnings)
	}

	// The container starts, and the condition goes back to what an operation in
	// flight reports. A ready Pod is what says so, and its stale status is not read.
	ready1 := int32(1)
	job.Status.Ready = &ready1
	if err := api.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	reconcileOnce()
	if condition := ready(); condition == nil || condition.Reason != string(operatorv1alpha1.ReasonOperationInProgress) {
		t.Fatalf("Ready after the Pod started = %#v, want OperationInProgress", condition)
	}
	if warnings := recordedConfigurationErrors(recorder); len(warnings) != 0 {
		t.Fatalf("a started Pod recorded %q", warnings)
	}
}

func TestAMigrationNamesTheSecretItsPodCannotStartWithout(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
	operation.AdmissionSnapshot = nil
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration)
	reconciler.Client = assignCreatedJobUIDClient{Client: api, uid: "migration-job-uid"}
	reconciler.Jobs = workloadBuilderForMigrations()
	recorder := record.NewFakeRecorder(16)
	reconciler.Recorder = recorder
	for range 2 {
		if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	jobs := &batchv1.JobList{}
	if err := api.List(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("dispatched Jobs = %d, want the one whose Pod this row stops", len(jobs.Items))
	}
	job := &jobs.Items[0]
	if job.UID != "migration-job-uid" {
		t.Fatalf("the dispatched Job's UID = %q; the read this row proves needs one", job.UID)
	}
	job.Status.Active = 1
	if err := api.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	pod := configurationErrorPod(job, createContainerConfigError, missingSecretText)
	if err := api.Create(ctx, pod.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if err := api.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}

	if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	reported := readMigration(t, api, migration)
	progressing := meta.FindStatusCondition(reported.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionFalse ||
		progressing.Reason != string(operatorv1alpha1.ReasonPodConfigurationError) ||
		!strings.Contains(progressing.Message, missingSecretText) {
		t.Fatalf("Progressing with the Secret missing = %#v, want False/PodConfigurationError naming the Secret", progressing)
	}
	if reported.Status.ActiveOperation == nil || reported.Status.ActiveOperation.JobName != job.Name {
		t.Fatalf("reporting the Pod retired the claim: %#v", reported.Status.ActiveOperation)
	}
	if warnings := recordedConfigurationErrors(recorder); len(warnings) != 1 || !strings.Contains(warnings[0], missingSecretText) {
		t.Fatalf("PodConfigurationError Warnings = %q, want one naming the Secret", warnings)
	}

	started := int32(1)
	job.Status.Ready = &started
	if err := api.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	progressing = meta.FindStatusCondition(readMigration(t, api, migration).Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue ||
		progressing.Reason != string(operatorv1alpha1.ReasonOperationInProgress) {
		t.Fatalf("Progressing after the Pod started = %#v, want True/OperationInProgress", progressing)
	}
}

// Each row is a reading of one Job and its Pods.
func TestPodConfigurationErrorReadsOnlyThisJobsWaitingContainer(t *testing.T) {
	t.Parallel()

	ready := int32(1)
	active := func() *batchv1.Job {
		return &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ptah-observe-app", UID: "job-uid"},
			Status:     batchv1.JobStatus{Active: 1},
		}
	}
	for _, row := range []struct {
		name      string
		mutate    func(*batchv1.Job, *corev1.Pod)
		want      string
		wantAlive bool
	}{
		{name: "its container waits on a missing Secret", want: missingSecretText, wantAlive: true},
		{
			// A fetch step reads the registry credential, and starts first.
			name: "an init container waits on a missing Secret",
			mutate: func(_ *batchv1.Job, pod *corev1.Pod) {
				pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses = pod.Status.ContainerStatuses, nil
			},
			want: missingSecretText, wantAlive: true,
		},
		{
			name: "a container after another that is running",
			mutate: func(_ *batchv1.Job, pod *corev1.Pod) {
				running := corev1.ContainerStatus{Name: "other", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
				pod.Status.ContainerStatuses = append([]corev1.ContainerStatus{running}, pod.Status.ContainerStatuses...)
			},
			want: missingSecretText, wantAlive: true,
		},
		{
			name: "control characters come off and the length is bound",
			mutate: func(_ *batchv1.Job, pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].State.Waiting.Message = "secret\r\n\x1b[31m\"x\"\tnot found " + strings.Repeat("y", 2*podCreationRefusalMessageLimit)
			},
			want:      `secret [31m"x" not found ` + strings.Repeat("y", podCreationRefusalMessageLimit-len(`secret [31m"x" not found `)) + "...",
			wantAlive: true,
		},
		{
			name:   "a Job with nothing active",
			mutate: func(job *batchv1.Job, _ *corev1.Pod) { job.Status.Active = 0 },
		},
		{
			name:      "a Job whose Pod is ready",
			mutate:    func(job *batchv1.Job, _ *corev1.Pod) { job.Status.Ready = &ready },
			wantAlive: true,
		},
		{
			name: "another waiting reason",
			mutate: func(_ *batchv1.Job, pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].State.Waiting.Reason = "ImagePullBackOff"
			},
			wantAlive: true,
		},
		{
			name: "a waiting container with no words",
			mutate: func(_ *batchv1.Job, pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].State.Waiting.Message = ""
			},
			wantAlive: true,
		},
		{
			name: "a container that is not waiting",
			mutate: func(_ *batchv1.Job, pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
			},
			wantAlive: true,
		},
		{
			name: "a Pod being deleted",
			mutate: func(_ *batchv1.Job, pod *corev1.Pod) {
				now := metav1.Now()
				pod.DeletionTimestamp = &now
				pod.Finalizers = []string{"test.example/hold"}
			},
		},
		{
			name: "a Pod that carries the label and another owner",
			mutate: func(_ *batchv1.Job, pod *corev1.Pod) {
				pod.OwnerReferences[0].UID = "another-job"
			},
		},
		{
			name: "a Pod of another Job",
			mutate: func(_ *batchv1.Job, pod *corev1.Pod) {
				pod.Labels[batchv1.ControllerUidLabel] = "another-job"
			},
		},
		{
			name: "a Pod in another namespace",
			mutate: func(_ *batchv1.Job, pod *corev1.Pod) {
				pod.Namespace = "team-b"
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			job := active()
			pod := configurationErrorPod(job, createContainerConfigError, missingSecretText)
			if row.mutate != nil {
				row.mutate(job, pod)
			}
			// The words come from the kubelet: this reads no Secret, so nothing
			// it reports can have come from one.
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "application-database"},
				Data:       map[string][]byte{"url": []byte("postgres://app:DO_NOT_PRINT@db/app")},
			}
			reader := interceptor.NewClient(
				fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pod, secret).WithStatusSubresource(pod).Build(),
				interceptor.Funcs{
					Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
						if _, isSecret := object.(*corev1.Secret); isSecret {
							t.Errorf("read Secret %s", key)
						}
						return inner.Get(ctx, key, object, options...)
					},
					List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
						if _, isSecrets := list.(*corev1.SecretList); isSecrets {
							t.Error("listed Secrets")
						}
						return inner.List(ctx, list, options...)
					},
				},
			)

			reading, err := podConfigurationError(context.Background(), reader, job)
			if err != nil {
				t.Fatalf("podConfigurationError() error = %v", err)
			}
			if reading.alive != row.wantAlive {
				t.Fatalf("podConfigurationError() alive = %t, want %t", reading.alive, row.wantAlive)
			}
			if row.want == "" {
				if reading.message != "" {
					t.Fatalf("podConfigurationError() = %q, want nothing", reading.message)
				}
				return
			}
			if want := "Job ptah-observe-app cannot start its Pod: " + row.want; reading.message != want {
				t.Fatalf("podConfigurationError() = %q, want %q", reading.message, want)
			}
			if strings.Contains(reading.message, "DO_NOT_PRINT") {
				t.Fatalf("podConfigurationError() carried the Secret's value: %q", reading.message)
			}
		})
	}
}

// A ready Job costs no read: most passes over a running operation are this one.
func TestPodConfigurationErrorListsNoPodsForAReadyJob(t *testing.T) {
	t.Parallel()

	ready := int32(1)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ptah-observe-app", UID: "job-uid"},
		Status:     batchv1.JobStatus{Active: 1, Ready: &ready},
	}
	reader := interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme.Scheme).Build(), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			t.Error("listed Pods for a Job whose Pod is ready")
			return nil
		},
	})
	reading, err := podConfigurationError(context.Background(), reader, job)
	if err != nil || reading.message != "" || !reading.alive {
		t.Fatalf("podConfigurationError() = %#v, %v, want a live Pod and no words", reading, err)
	}
}

// Measured on kind: the Job's deadline leaves the Job with no active Pod and no
// terminal condition while the Pod is deleted, and a pass in that gap went back
// to OperationInProgress, so the failure that followed said only that no frame
// arrived. Only a Pod that is alive can take the words back.
func TestWhatEndsAPodConfigurationReport(t *testing.T) {
	t.Parallel()

	now := metav1.Now()
	said := "Job ptah-observe-app cannot start its Pod: " + missingSecretText
	reported := &metav1.Condition{Reason: string(operatorv1alpha1.ReasonPodConfigurationError), Message: said}
	ready := int32(1)
	job := func(active int32, ready *int32) *batchv1.Job {
		return &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ptah-observe-app", UID: "job-uid", CreationTimestamp: now},
			Status:     batchv1.JobStatus{Active: active, Ready: ready},
		}
	}
	waiting := func(job *batchv1.Job) *corev1.Pod {
		return configurationErrorPod(job, createContainerConfigError, missingSecretText)
	}
	for _, row := range []struct {
		name        string
		job         *batchv1.Job
		pod         func(*batchv1.Job) *corev1.Pod
		current     *metav1.Condition
		wantChanged bool
		wantRefused bool
	}{
		{
			name: "the same words", job: job(1, nil), pod: waiting, current: reported,
			wantRefused: true,
		},
		{
			name: "the deadline took the active count", job: job(0, nil), current: reported,
		},
		{
			name: "the deadline is deleting the Pod", job: job(1, nil), current: reported,
			pod: func(job *batchv1.Job) *corev1.Pod {
				pod := waiting(job)
				pod.DeletionTimestamp = &now
				pod.Finalizers = []string{"test.example/hold"}
				return pod
			},
		},
		{
			name: "a Pod is past the words", job: job(1, nil), current: reported, wantChanged: true,
			pod: func(job *batchv1.Job) *corev1.Pod { return configurationErrorPod(job, "PodInitializing", "") },
		},
		{
			name: "the Pod is ready", job: job(1, &ready), current: reported, wantChanged: true,
		},
		{
			// A refusal has always gone back when nothing refuses any more. The
			// Pod the Job has now is why.
			name: "a refusal nothing repeats", job: job(1, nil), wantChanged: true,
			current: &metav1.Condition{Reason: string(operatorv1alpha1.ReasonPodAdmissionRefused), Message: "refused"},
		},
		{
			name: "an operation in progress with nothing to say", job: job(1, nil),
			current: &metav1.Condition{Reason: string(operatorv1alpha1.ReasonOperationInProgress)},
		},
		{
			name: "no condition yet with words to report", job: job(1, nil), pod: waiting, wantChanged: true, wantRefused: true,
		},
		{
			name: "a refusal the kubelet now explains", job: job(1, nil), pod: waiting, wantChanged: true, wantRefused: true,
			current: &metav1.Condition{Reason: string(operatorv1alpha1.ReasonPodAdmissionRefused), Message: said},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			builder := fake.NewClientBuilder().WithScheme(scheme.Scheme)
			if row.pod != nil {
				builder = builder.WithObjects(row.pod(row.job))
			}
			change, err := judgePodAdmission(context.Background(), builder.Build(), row.job, now.Time, row.current)
			if err != nil {
				t.Fatalf("judgePodAdmission() error = %v", err)
			}
			if change.changed != row.wantChanged || change.refused != row.wantRefused {
				t.Fatalf("judgePodAdmission() = %#v, want changed=%t refused=%t", change, row.wantChanged, row.wantRefused)
			}
		})
	}
}

func TestWithUnstartedPodAddsWhatTheResourceReported(t *testing.T) {
	t.Parallel()

	reported := &metav1.Condition{
		Reason:  string(operatorv1alpha1.ReasonPodConfigurationError),
		Message: "Job ptah-observe-app cannot start its Pod: " + missingSecretText,
	}
	frameLost := fmt.Errorf("read Observe result: %w", runner.ErrFrameNotFound)
	for _, row := range []struct {
		name     string
		failure  error
		reported *metav1.Condition
		want     string
	}{
		{
			name: "a lost frame after the Pod could not start", failure: frameLost, reported: reported,
			want: "read Observe result: ptah runner result frame not found; last reported before that: Job ptah-observe-app cannot start its Pod: " + missingSecretText,
		},
		{name: "no condition", failure: frameLost, want: frameLost.Error()},
		{
			name: "another reason", failure: frameLost,
			reported: &metav1.Condition{Reason: string(operatorv1alpha1.ReasonOperationInProgress), Message: "Observe operation is in progress"},
			want:     frameLost.Error(),
		},
		{name: "a failure that is not a lost frame", failure: errors.New("the runner refused"), reported: reported, want: "the runner refused"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got := withUnstartedPod(row.failure, row.reported)
			if got.Error() != row.want {
				t.Fatalf("withUnstartedPod() = %q, want %q", got, row.want)
			}
			if !errors.Is(got, row.failure) {
				t.Fatalf("withUnstartedPod() lost the failure it was given: %v", got)
			}
		})
	}
}

// Past the Job's deadline the Pod is gone and no frame exists, and the failure
// still says why the operation never ran. The next Job reports it again.
func TestAReadPastItsDeadlineKeepsSayingWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseObserving
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationObserve, ID: "observe-deadline",
		JobName: "observe-deadline-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
	}
	bindActiveInput(t, schema)
	said := "Job observe-deadline-job cannot start its Pod: " + missingSecretText
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPodConfigurationError, said)
	job, pod := terminalWorkload(schema, batchv1.JobFailed)
	reconciler, api := fakeReconciler(t, staticLogs{content: []byte("no frame here\n")}, schema, job, pod)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	failed := meta.FindStatusCondition(safetyGetSchema(t, api, schema).Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if failed == nil || failed.Status != metav1.ConditionTrue || !strings.Contains(failed.Message, runner.ErrFrameNotFound.Error()) ||
		!strings.Contains(failed.Message, said) {
		t.Fatalf("ReconciliationFailed = %#v, want the lost frame and %q", failed, said)
	}
}

// The resource looks at a running Pod every 30 seconds. A Secret that arrives and
// a Pod that starts and dies without a result can fall inside one look, and the
// terminal pass reads the Job's result rather than its Pod's waiting state. The
// condition then still says the Pod cannot start, about a Pod that did, so the
// failure has to say whose words those are and when they were true.
func TestAFailureSaysItsPodReportIsTheLastOne(t *testing.T) {
	t.Parallel()

	const lastReported = "ptah runner result frame not found; last reported before that: "
	t.Run("a schema read", func(t *testing.T) {
		t.Parallel()
		schema := schemaFixture()
		schema.Finalizers = []string{activeOperationFinalizer}
		schema.Status.Phase = operatorv1alpha1.PhaseObserving
		schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
			Type: operatorv1alpha1.OperationObserve, ID: "observe-started",
			JobName: "observe-started-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
		}
		bindActiveInput(t, schema)
		said := "Job observe-started-job cannot start its Pod: " + missingSecretText
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPodConfigurationError, said)
		job, pod := terminalWorkload(schema, batchv1.JobFailed)
		podStarted(t, pod)
		reconciler, api := fakeReconciler(t, staticLogs{content: []byte("no frame here\n")}, schema, job, pod)

		if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		failed := meta.FindStatusCondition(safetyGetSchema(t, api, schema).Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
		if failed == nil || !strings.Contains(failed.Message, lastReported+said) {
			t.Fatalf("ReconciliationFailed = %#v, want %q", failed, lastReported+said)
		}
	})
	t.Run("a schema Apply", func(t *testing.T) {
		t.Parallel()
		schema, plan, policy := dispatchedApplyWithPlan(t)
		said := "Job " + schema.Status.ActiveOperation.JobName + " cannot start its Pod: " + missingSecretText
		setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPodConfigurationError, said)
		job, pod := terminalWorkload(schema, batchv1.JobFailed)
		podStarted(t, pod)
		reconciler, api := fakeReconciler(t, staticLogs{content: []byte("no frame here\n")}, schema, plan, policy, job, pod)
		reconciler.Locks = targetlock.New(api, api, nil)

		if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		failed := meta.FindStatusCondition(safetyGetSchema(t, api, schema).Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
		if failed == nil || !strings.Contains(failed.Message, lastReported+said) {
			t.Fatalf("ReconciliationFailed = %#v, want %q", failed, lastReported+said)
		}
	})
	t.Run("a migration read", func(t *testing.T) {
		t.Parallel()
		migration := migrationFixture()
		migration.Status.ExecutionBinding = migrationExecutionBinding()
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
		migration.Finalizers = []string{migrationOperationFinalizer}
		operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
		said := "Job " + operation.JobName + " cannot start its Pod: " + missingSecretText
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, operatorv1alpha1.ReasonPodConfigurationError, said)
		job, pod := terminalMigrationWorkload(migration, batchv1.JobFailed)
		podStarted(t, pod)
		reconciler, api := fakeMigrationReconciler(t, staticLogs{content: []byte("no frame here\n")}, migration, job, pod)

		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		progressing := meta.FindStatusCondition(readMigration(t, api, migration).Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
		if progressing == nil || !strings.Contains(progressing.Message, lastReported+said) {
			t.Fatalf("Progressing = %#v, want %q", progressing, lastReported+said)
		}
	})
}

// A migration Apply whose log held no frame is a run nobody accounted for, and
// its record is what a person reads once the Pod is gone. It keeps what the
// resource last reported about the Pod, worded as that, the way a read's failure
// and a schema Apply's do.
func TestAnUnreadMigrationApplyKeepsWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	const lastReported = "ptah runner result frame not found; last reported before that: "
	for _, row := range []struct {
		name    string
		podLeft bool
	}{
		{name: "past its Job's deadline, with the Pod gone"},
		{name: "with a Pod that started between two looks", podLeft: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			migration, plan := awaitingApprovalFixture(t)
			operation := applyClaimFor(t, migration, plan)
			approvedClaim(operation, plan)
			said := "Job " + operation.JobName + " cannot start its Pod: " + missingSecretText
			setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse,
				operatorv1alpha1.ReasonPodConfigurationError, said)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobFailed)
			objects := []client.Object{migration, plan, job, verificationPolicyConfigMap()}
			if row.podLeft {
				podStarted(t, pod)
				objects = append(objects, pod)
			}
			reconciler, api := fakeMigrationReconciler(t, staticLogs{content: []byte("no frame here\n")}, objects...)
			holdMigrationApplyLease(t, reconciler, api, migration)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			stored := readMigration(t, api, migration)
			run := stored.Status.LastRun
			if run == nil || run.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown || !strings.Contains(run.Message, lastReported+said) {
				t.Fatalf("last run = %#v, want an unknown outcome whose message carries %q", run, lastReported+said)
			}
			blocked := meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
			if blocked == nil || blocked.Status != metav1.ConditionTrue || !strings.Contains(blocked.Message, lastReported+said) {
				t.Fatalf("Blocked = %#v, want %q", blocked, lastReported+said)
			}
		})
	}
}

// podStarted makes pod one whose container ran and ended, which is what a kubelet
// leaves after the Secret arrived and the process died without a result frame.
func podStarted(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if len(pod.Status.ContainerStatuses) == 0 {
		t.Fatalf("Pod %s has no container status to mark as started", pod.Name)
	}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 137, Reason: "Error", StartedAt: metav1.Now(), FinishedAt: metav1.Now(),
	}}
}

// An Apply in the same place owes the read-only proof every Apply owes, and the
// reason its outcome is unknown says what stopped it.
func TestAnApplyPastItsDeadlineKeepsSayingWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	schema, plan, policy := dispatchedApplyWithPlan(t)
	said := "Job " + schema.Status.ActiveOperation.JobName + " cannot start its Pod: " + missingSecretText
	setCondition(schema, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPodConfigurationError, said)
	job, pod := terminalWorkload(schema, batchv1.JobFailed)
	reconciler, api := fakeReconciler(t, staticLogs{content: []byte("no frame here\n")}, schema, plan, policy, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	failed := meta.FindStatusCondition(safetyGetSchema(t, api, schema).Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if failed == nil || failed.Reason != string(operatorv1alpha1.ReasonApplyOutcomeUnknown) ||
		!strings.Contains(failed.Message, "apply result is uncertain") || !strings.Contains(failed.Message, said) {
		t.Fatalf("ReconciliationFailed = %#v, want an unknown outcome that says %q", failed, said)
	}
}

func TestAMigrationReadPastItsDeadlineKeepsSayingWhyItsPodNeverStarted(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
	said := "Job " + operation.JobName + " cannot start its Pod: " + missingSecretText
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, operatorv1alpha1.ReasonPodConfigurationError, said)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobFailed)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: []byte("no frame here\n")}, migration, job, pod)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	progressing := meta.FindStatusCondition(readMigration(t, api, migration).Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || !strings.Contains(progressing.Message, runner.ErrFrameNotFound.Error()) || !strings.Contains(progressing.Message, said) {
		t.Fatalf("Progressing = %#v, want the lost frame and %q", progressing, said)
	}
}
