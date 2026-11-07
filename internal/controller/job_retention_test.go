package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// finishedJob is a stored Job a cleanup patch can reach.
func finishedJob(ttl *int32) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ptah-apply-orders-0123", UID: "job-uid"},
		Spec:       batchv1.JobSpec{TTLSecondsAfterFinished: ttl},
	}
}

// refusingCleanup answers each cleanup patch whose TTL refuse names with err,
// the way a guard does, and writes the others.
func refusingCleanup(t *testing.T, job *batchv1.Job, refuse map[int32]error) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(jobScheme(t)).WithObjects(job).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, api client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if stored, isJob := object.(*batchv1.Job); isJob && stored.Spec.TTLSecondsAfterFinished != nil {
				if err := refuse[*stored.Spec.TTLSecondsAfterFinished]; err != nil {
					return err
				}
			}
			return api.Patch(ctx, object, patch, options...)
		},
	}).Build()
}

func jobScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func storedTTL(t *testing.T, api client.Client, job *batchv1.Job) *int32 {
	t.Helper()
	stored := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), stored); err != nil {
		t.Fatal(err)
	}
	return stored.Spec.TTLSecondsAfterFinished
}

// TestAFailedApplysJobKeepsItsLogAndAGuardCannotStopItsCleanup holds what the
// write does against each answer a guard can give, and what it says when it falls
// back: a guard left behind by a failed upgrade would otherwise shorten every failed
// Apply's log in silence.
func TestAFailedApplysJobKeepsItsLogAndAGuardCannotStopItsCleanup(t *testing.T) {
	t.Parallel()

	long := jobclaim.FailedApplyRetentionTTLSeconds
	short := jobclaim.CleanupTTLSeconds
	jobs := schema.GroupResource{Group: "batch", Resource: "jobs"}
	forbidden := apierrors.NewForbidden(jobs, "ptah-apply-orders-0123", errors.New("the guard refused it"))
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: "batch", Kind: "Job"}, "ptah-apply-orders-0123", nil)
	conflict := apierrors.NewConflict(jobs, "ptah-apply-orders-0123", errors.New("modified"))

	for _, test := range []struct {
		name    string
		keepLog bool
		refuse  map[int32]error
		want    *int32
		wantErr bool
		noted   bool
	}{
		{name: "a run that keeps its log", keepLog: true, want: &long},
		// The one that bounds how many Jobs a namespace holds: the long TTL is not
		// tried, so a refusal of it could not matter.
		{name: "a run that repeats", refuse: map[int32]error{long: conflict}, want: &short},
		{name: "a run that repeats, with its cleanup refused", refuse: map[int32]error{short: forbidden}, wantErr: true},
		// The guard of the release before this one, for the moment an upgrade
		// leaves it in place, and the webhook on a run that reported success.
		{name: "a webhook refuses the long TTL", keepLog: true, refuse: map[int32]error{long: forbidden}, want: &short, noted: true},
		{name: "a policy refuses the long TTL", keepLog: true, refuse: map[int32]error{long: invalid}, want: &short, noted: true},
		// A refusal of the cleanup itself is not about the TTL, and is reported.
		{name: "the guard refuses both", keepLog: true, refuse: map[int32]error{long: forbidden, short: forbidden}, wantErr: true},
		// A conflict owes a fresh read, not a shorter retention.
		{name: "a conflict", keepLog: true, refuse: map[int32]error{long: conflict}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			job := finishedJob(nil)
			api := refusingCleanup(t, job, test.refuse)
			note := ""
			err := retainFailedApplyJob(context.Background(), api, job, test.keepLog, func(message string) { note = message })
			if (err != nil) != test.wantErr {
				t.Fatalf("retainFailedApplyJob() error = %v, want an error: %v", err, test.wantErr)
			}
			got := storedTTL(t, api, job)
			switch {
			case test.want == nil && got != nil:
				t.Fatalf("the stored TTL = %d, want none", *got)
			case test.want != nil && (got == nil || *got != *test.want):
				t.Fatalf("the stored TTL = %v, want %d", got, *test.want)
			}
			if test.want == nil && job.Spec.TTLSecondsAfterFinished != nil {
				t.Fatalf("the in-memory Job claims TTL %d the server never took", *job.Spec.TTLSecondsAfterFinished)
			}
			if test.noted != (note != "") {
				t.Fatalf("the fallback was noted as %q, want a note: %v", note, test.noted)
			}
			if test.noted && (!strings.Contains(note, "Admission refused the 24 hours") ||
				!strings.Contains(note, "stays for 5 minutes") || !strings.Contains(note, job.Name)) {
				t.Fatalf("the note %q does not say what happened to which Job", note)
			}
		})
	}
}

// TestAJobHarvestedByAPreviousReleaseKeepsItsTTL is the upgrade: a Job already
// holding 300 seconds gets no write, which the guard would refuse, and what the
// resource says of it is what the Job carries.
func TestAJobHarvestedByAPreviousReleaseKeepsItsTTL(t *testing.T) {
	t.Parallel()

	short := jobclaim.CleanupTTLSeconds
	job := finishedJob(&short)
	refused := errors.New("no write reaches a Job that has a TTL")
	api := refusingCleanup(t, job, map[int32]error{
		jobclaim.FailedApplyRetentionTTLSeconds: refused, jobclaim.CleanupTTLSeconds: refused,
	})
	if err := retainFailedApplyJob(context.Background(), api, job, true, func(string) {}); err != nil {
		t.Fatalf("retainFailedApplyJob() error = %v", err)
	}
	if got := storedTTL(t, api, job); got == nil || *got != short {
		t.Fatalf("the stored TTL = %v, want %d", got, short)
	}
	const want = "What Ptah reported is in the Pod log for 5 minutes after the Job finishes: " +
		"kubectl logs -n team-a job/ptah-apply-orders-0123 -c ptah"
	if got := failedApplyLogPointer(job, applyLogHoldsReport); got != want {
		t.Fatalf("failedApplyLogPointer() = %q, want %q", got, want)
	}
}

func TestTheLogPointerSaysOnlyWhatTheReadingAllows(t *testing.T) {
	t.Parallel()

	long := jobclaim.FailedApplyRetentionTTLSeconds
	const (
		where  = " is in the Pod log for 24 hours after the Job finishes: kubectl logs -n team-a job/ptah-apply-orders-0123 -c ptah"
		firm   = "What Ptah reported" + where
		unread = "Anything the run wrote" + where
	)
	if got := failedApplyLogPointer(finishedJob(&long), applyLogHoldsReport); got != firm {
		t.Fatalf("failedApplyLogPointer() = %q, want %q", got, firm)
	}
	if got := failedApplyLogPointer(finishedJob(&long), applyLogUnread); got != unread {
		t.Fatalf("failedApplyLogPointer() = %q, want %q", got, unread)
	}
	if got := withLogPointer("Apply failed", finishedJob(&long), applyLogHoldsReport); got != "Apply failed. "+firm {
		t.Fatalf("withLogPointer() = %q", got)
	}
	if got := failedApplyLogPointer(finishedJob(&long), applyLogSaysNothing); got != "" {
		t.Errorf("a log with nothing to read: failedApplyLogPointer() = %q", got)
	}
	if got := withLogPointer("Apply failed", finishedJob(&long), applyLogSaysNothing); got != "Apply failed" {
		t.Errorf("a log with nothing to read: withLogPointer() = %q", got)
	}
	for name, job := range map[string]*batchv1.Job{"no Job": nil, "a Job no TTL reached": finishedJob(nil)} {
		if got := failedApplyLogPointer(job, applyLogHoldsReport); got != "" {
			t.Errorf("%s: failedApplyLogPointer() = %q, want nothing to promise", name, got)
		}
		if got := withLogPointer("Apply failed", job, applyLogHoldsReport); got != "Apply failed" {
			t.Errorf("%s: withLogPointer() = %q", name, got)
		}
	}
	for seconds, want := range map[int32]string{86400: "24 hours", 7200: "2 hours", 300: "5 minutes", 90: "90 seconds", 0: "0 seconds"} {
		if got := retentionWords(seconds); got != want {
			t.Errorf("retentionWords(%d) = %q, want %q", seconds, got, want)
		}
	}
}

// TestAFailureSaysWhatItsLogHolds reads the choke point. A result's code decides,
// because the runner writes Ptah's sentence for two codes and no others; a failure
// with no result may have written anything, unless an init container ended the Pod.
func TestAFailureSaysWhatItsLogHolds(t *testing.T) {
	t.Parallel()

	failedInit := func(job *batchv1.Job) *corev1.Pod {
		controller := true
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: job.Namespace, Name: job.Name + "-pod", Labels: map[string]string{"job-name": job.Name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller, BlockOwnerDeletion: &controller,
				}},
			},
			Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "fetch-migrations", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2}},
			}}},
		}
	}
	coded := func(code string) error {
		return resultFailure(&runner.ResultError{Code: code, Message: "the runner's sentence"}, corev1.SecretKeySelector{})
	}
	for _, test := range []struct {
		name    string
		failure error
		noJob   bool
		initEnd bool
		want    applyLogReading
	}{
		{name: "an outcome that is unknown", failure: coded("apply_outcome_unknown"), want: applyLogHoldsReport},
		{name: "a failure before any statement", failure: coded("apply_failed"), want: applyLogHoldsReport},
		{name: "a wrapped result", failure: fmt.Errorf("while settling: %w", coded("apply_failed")), want: applyLogHoldsReport},
		{name: "a target that could not be opened", failure: coded("database_unreachable"), want: applyLogSaysNothing},
		{name: "a refusal", failure: coded("apply_refused"), want: applyLogSaysNothing},
		{name: "a stale plan", failure: coded("stale_plan"), want: applyLogSaysNothing},
		{name: "a report that does not decode", failure: coded("invalid_apply_output"), want: applyLogSaysNothing},
		{name: "no result", failure: errors.New("apply result is uncertain"), want: applyLogUnread},
		{name: "no result and no Job", failure: errors.New("apply result is uncertain"), noJob: true, want: applyLogSaysNothing},
		{name: "no result and a failed init container", failure: errors.New("apply result is uncertain"), initEnd: true, want: applyLogSaysNothing},
		{name: "no failure to read, as a retired Apply has", want: applyLogUnread},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			job := finishedJob(nil)
			builder := fake.NewClientBuilder().WithScheme(jobAndPodScheme(t))
			if test.initEnd {
				builder = builder.WithObjects(failedInit(job))
			}
			reader := builder.Build()
			if test.noJob {
				job = nil
			}
			if got := failedApplyLogReading(context.Background(), reader, job, test.failure); got != test.want {
				t.Fatalf("failedApplyLogReading() = %v, want %v", got, test.want)
			}
		})
	}
	t.Run("Pods that cannot be read promise nothing", func(t *testing.T) {
		t.Parallel()
		reader := fake.NewClientBuilder().WithScheme(jobAndPodScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("the API server did not answer")
			},
		}).Build()
		if got := failedApplyLogReading(context.Background(), reader, finishedJob(nil), errors.New("uncertain")); got != applyLogSaysNothing {
			t.Fatalf("failedApplyLogReading() = %v", got)
		}
	})
}

func jobAndPodScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := jobScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// schemaApplyRow runs one finished Apply through a reconcile and reads the Job's TTL,
// the Applying condition and the ApplyOutcomeUnknown Event.
func schemaApplyRow(t *testing.T, approved bool, failure *runner.ResultError, condition batchv1.JobConditionType, logs string) (*int32, string, string, *batchv1.Job) {
	t.Helper()
	apply, plan, policyConfig := dispatchedApplyWithPlan(t)
	apply.Status.Plan.Approval = nil
	if approved {
		apply.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{Name: "approval", UID: "approval-uid"}
	}
	job, pod := terminalWorkload(apply, condition)
	content := []byte(logs)
	if failure != nil {
		content = safetyRunnerFrame(t, runner.Result{
			ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply, OperationID: apply.Status.ActiveOperation.ID,
			ChildExitCode: 2, MutationStarted: true, Uncertain: true,
			CoordinationDigest:   apply.Status.Plan.CoordinationDigest,
			TargetIdentityDigest: apply.Status.Plan.TargetIdentityDigest,
			Error:                failure,
		})
	}
	reconciler, api := fakeReconciler(t, staticLogs{content: content}, apply, plan, policyConfig, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)
	recorder := record.NewFakeRecorder(32)
	reconciler.Recorder = recorder
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apply)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	applying := meta.FindStatusCondition(safetyGetSchema(t, api, apply).Status.Conditions, operatorv1alpha1.ConditionApplying)
	if applying == nil {
		t.Fatal("no Applying condition")
	}
	return storedTTL(t, api, job), applying.Message, findEvent(recorder, "Warning ApplyOutcomeUnknown"), job
}

// TestASchemaApplyKeepsItsLogByHowItEnded reads the Job's TTL and the pointer where a
// person looks. Applying stays once the observation has settled the record, so that
// is where a person arriving later finds it.
func TestASchemaApplyKeepsItsLogByHowItEnded(t *testing.T) {
	t.Parallel()

	long, short := jobclaim.FailedApplyRetentionTTLSeconds, jobclaim.CleanupTTLSeconds
	unknown := &runner.ResultError{Code: "apply_outcome_unknown", Message: "ptah sent the statements and returned an error, so how far they got is unknown"}
	const (
		condition = "Apply outcome is uncertain; only read-only observation is permitted"
		event     = "Warning ApplyOutcomeUnknown Apply outcome is uncertain; observing database state"
	)
	for _, test := range []struct {
		name     string
		approved bool
		failure  *runner.ResultError
		ttl      int32
		pointer  string
	}{
		{name: "a run a person approved", approved: true, failure: unknown, ttl: long, pointer: "What Ptah reported is in the Pod log for 24 hours"},
		// The failure repeats under Always, so each run keeps only what any Job keeps,
		// and the pointer says so.
		{name: "a run no approval names", failure: unknown, ttl: short, pointer: "What Ptah reported is in the Pod log for 5 minutes"},
		{
			name: "a failure before any statement", approved: true, ttl: long, pointer: "What Ptah reported is in the Pod log for 24 hours",
			failure: &runner.ResultError{Code: "apply_failed", Message: "ptah reported that the apply failed before it sent any statement"},
		},
		// The runner writes no line for these, so the status promises none.
		{
			name: "a target that could not be opened", approved: true, ttl: long,
			failure: &runner.ResultError{Code: string(dataplane.TargetDatabaseUnreachable), Message: frameText},
		},
		{
			name: "a refused plan", approved: true, ttl: long,
			failure: &runner.ResultError{Code: "apply_refused", Message: "ptah refused the approved plan (plan_contract)"},
		},
		{
			name: "a stale plan", approved: true, ttl: long,
			failure: &runner.ResultError{Code: "stale_plan", Message: "the plan is stale"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ttl, applying, recorded, job := schemaApplyRow(t, test.approved, test.failure, batchv1.JobComplete, "")
			if ttl == nil || *ttl != test.ttl {
				t.Fatalf("the Job's TTL = %v, want %d", ttl, test.ttl)
			}
			if test.pointer == "" {
				if applying != condition || !strings.HasPrefix(recorded, event) || strings.Contains(recorded, "kubectl logs") {
					t.Fatalf("Applying = %q, event = %q, want no pointer to a line that is not there", applying, recorded)
				}
				return
			}
			pointer := test.pointer + " after the Job finishes: kubectl logs -n " + job.Namespace + " job/" + job.Name + " -c ptah"
			if applying != condition+". "+pointer || recorded != event+". "+pointer {
				t.Fatalf("Applying = %q, event = %q, want them to end with %q", applying, recorded, pointer)
			}
		})
	}
	t.Run("a run whose result nobody read", func(t *testing.T) {
		t.Parallel()
		ttl, applying, recorded, job := schemaApplyRow(t, true, nil, batchv1.JobFailed, "")
		if ttl == nil || *ttl != long {
			t.Fatalf("the Job's TTL = %v, want %d", ttl, long)
		}
		pointer := "Anything the run wrote is in the Pod log for 24 hours after the Job finishes: kubectl logs -n " + job.Namespace + " job/" + job.Name + " -c ptah"
		if applying != condition+". "+pointer || recorded != event+". "+pointer {
			t.Fatalf("Applying = %q, event = %q, want them to end with %q", applying, recorded, pointer)
		}
	})
	t.Run("a run that reported success", func(t *testing.T) {
		t.Parallel()
		apply, plan, policyConfig := dispatchedApplyWithPlan(t)
		apply.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{Name: "approval", UID: "approval-uid"}
		job, pod := terminalWorkload(apply, batchv1.JobComplete)
		frame := safetyRunnerFrame(t, runner.Result{
			ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply, OperationID: apply.Status.ActiveOperation.ID,
			MutationStarted: true, CoordinationDigest: apply.Status.Plan.CoordinationDigest,
			TargetIdentityDigest: apply.Status.Plan.TargetIdentityDigest,
		})
		reconciler, api := fakeReconciler(t, staticLogs{content: frame}, apply, plan, policyConfig, job, pod)
		reconciler.Locks = targetlock.New(api, api, nil)

		if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apply)}); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if got := storedTTL(t, api, job); got == nil || *got != jobclaim.CleanupTTLSeconds {
			t.Fatalf("the Job's TTL = %v, want %d", got, jobclaim.CleanupTTLSeconds)
		}
		applying := meta.FindStatusCondition(safetyGetSchema(t, api, apply).Status.Conditions, operatorv1alpha1.ConditionApplying)
		if applying == nil || strings.Contains(applying.Message, "kubectl logs") {
			t.Fatalf("Applying = %#v, want no pointer to a log", applying)
		}
	})
}

// TestAnUncertainSchemaApplyUnderAlwaysLeadsToTheNextApply is why a schema's long TTL
// stays with an approval. Its proof plans again, the plan is ready at once under
// `apply: Always`, and nothing is left to stop the next Apply, so a failure that goes
// on being uncertain holds one Job per cycle. If this stops being true, the migration
// family's rule in jobclaim is the one a schema could take too.
func TestAnUncertainSchemaApplyUnderAlwaysLeadsToTheNextApply(t *testing.T) {
	t.Parallel()

	schema, objects := convergedProofPlan(t, operatorv1alpha1.PendingObservationOutcomeUnknown)
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	// The fixture's record differs from its schema on purpose, and a proof of an
	// older policy would go back to Pending instead of publishing what it planned.
	currentPolicy, err := policyFingerprint(schema)
	if err != nil {
		t.Fatal(err)
	}
	schema.Status.PendingObservation.Plan.PolicyFingerprint = currentPolicy
	schema.Status.Target = operatorv1alpha1.TargetStatus{
		CoordinationDigest: testCoordinationDigest, IdentityDigest: testDigest, DriftReportDigest: safetyOtherDigest,
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, objects...)
	reconciler.Plans = planstore.Store{Client: api, Reader: api}
	stored := safetyGetSchema(t, api, schema)
	document := safetyPlanDocument(t, "observed-state")
	if _, err := reconciler.consumeResultWithTransport(context.Background(), stored, nil, runner.Result{
		Operation:            runner.OperationPlan,
		CoordinationDigest:   stored.Status.PendingObservation.CoordinationDigest,
		TargetIdentityDigest: stored.Status.PendingObservation.Plan.TargetIdentityDigest,
		Stdout:               string(document),
		PlanContentDigest:    fingerprint.DigestBytes(document),
		PlanOutcome:          runner.PlanOutcomeChanges,
	}, nil, 0, true); err != nil {
		t.Fatalf("consumeResultWithTransport() error = %v", err)
	}
	proved := safetyGetSchema(t, api, schema)
	if proved.Status.PendingObservation != nil {
		t.Fatalf("PendingObservation = %#v, want the proof settled, which is all that held the next Apply", proved.Status.PendingObservation)
	}
	if proved.Status.Plan == nil || proved.Status.Phase != operatorv1alpha1.PhaseReadyToApply {
		t.Fatalf("phase = %q, plan = %#v, want a plan ready to Apply with nobody asked", proved.Status.Phase, proved.Status.Plan)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	next := safetyGetSchema(t, api, schema)
	if next.Status.ActiveOperation == nil || next.Status.ActiveOperation.Type != operatorv1alpha1.OperationApply {
		t.Fatalf("phase = %q, active operation = %#v, want the next Apply claimed", next.Status.Phase, next.Status.ActiveOperation)
	}
}

// migrationEvents drains what a run's reconcile recorded.
func migrationEvents(recorder *record.FakeRecorder) []string {
	close(recorder.Events)
	var events []string
	for event := range recorder.Events {
		events = append(events, event)
	}
	return events
}

// assertNoShortening holds the manager to not asking for what admission refuses: the
// Warning says a log was cut short, and a run no rule gave the long TTL lost nothing.
func assertNoShortening(t *testing.T, events []string) {
	t.Helper()
	if slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, "FailedApplyLogShortened") }) {
		t.Fatalf("the Events %q say admission shortened a log", events)
	}
}

// TestAMigrationRunKeepsItsLogByItsOutcome runs each outcome through the same frame,
// with and without an approval on the claim. A run that succeeded keeps the short TTL
// and says nothing of a log. A run that did not keeps the long TTL when a person
// approved it or when it stopped the resource, and the short one when it did neither,
// and each says where and for how long, except a target that could not be opened,
// whose sentence is whole and whose log holds nothing of Ptah's. Phase is what ties
// the TTL to the claim: only a run that leaves the resource Blocked keeps its log
// under `apply: Always`.
func TestAMigrationRunKeepsItsLogByItsOutcome(t *testing.T) {
	t.Parallel()

	failed := &runner.ResultError{Code: "child_exit", Message: "ptah exited with code 2"}
	const (
		kept      = "What Ptah reported is in the Pod log for 24 hours"
		collected = "What Ptah reported is in the Pod log for 5 minutes"
	)
	long, short := jobclaim.FailedApplyRetentionTTLSeconds, jobclaim.CleanupTTLSeconds
	for _, test := range []struct {
		name     string
		outcome  string
		planned  []int64
		failure  *runner.ResultError
		code     int
		approved bool
		ttl      int32
		pointer  string
		phase    operatorv1alpha1.MigrationPhase
	}{
		{name: "applied", outcome: dataplane.MigrationOutcomeApplied, approved: true, ttl: short, phase: operatorv1alpha1.MigrationPhaseVerifyingHistory},
		{name: "up to date", outcome: dataplane.MigrationOutcomeUpToDate, approved: true, ttl: short, phase: operatorv1alpha1.MigrationPhaseVerifyingHistory},
		{name: "applied, under Always", outcome: dataplane.MigrationOutcomeApplied, ttl: short, phase: operatorv1alpha1.MigrationPhaseVerifyingHistory},
		{name: "up to date, under Always", outcome: dataplane.MigrationOutcomeUpToDate, ttl: short, phase: operatorv1alpha1.MigrationPhaseVerifyingHistory},
		{
			name: "failed, approved", outcome: dataplane.MigrationOutcomeFailed, code: 2, failure: failed, approved: true,
			ttl: long, pointer: kept, phase: operatorv1alpha1.MigrationPhaseFailed,
		},
		{
			// It starts again from resolution after the failure interval.
			name: "failed before it selected a migration, under Always", outcome: dataplane.MigrationOutcomeFailed, code: 2, failure: failed,
			ttl: short, pointer: collected, phase: operatorv1alpha1.MigrationPhaseFailed,
		},
		{
			// The TTL is set here, from the outcome, ahead of the reading that decides
			// what comes next.
			name: "failed at a migration, under Always", outcome: dataplane.MigrationOutcomeFailed, planned: []int64{3}, code: 2, failure: failed,
			ttl: short, pointer: collected, phase: operatorv1alpha1.MigrationPhaseVerifyingHistory,
		},
		{
			name: "a target that could not be opened", outcome: dataplane.MigrationOutcomeFailed, code: 2, approved: true,
			failure: &runner.ResultError{Code: string(dataplane.TargetDatabaseUnreachable), Message: frameText},
			ttl:     long, phase: operatorv1alpha1.MigrationPhaseFailed,
		},
		{
			name: "a target that could not be opened, under Always", outcome: dataplane.MigrationOutcomeFailed, code: 2,
			failure: &runner.ResultError{Code: string(dataplane.TargetDatabaseUnreachable), Message: frameText},
			ttl:     short, phase: operatorv1alpha1.MigrationPhaseFailed,
		},
		{
			name: "partial, approved", outcome: dataplane.MigrationOutcomePartial, planned: []int64{3}, code: 2, failure: failed, approved: true,
			ttl: long, pointer: kept, phase: operatorv1alpha1.MigrationPhaseBlocked,
		},
		{
			name: "partial, under Always", outcome: dataplane.MigrationOutcomePartial, planned: []int64{3}, code: 2, failure: failed,
			ttl: long, pointer: kept, phase: operatorv1alpha1.MigrationPhaseBlocked,
		},
		{
			name: "unknown, under Always", outcome: dataplane.MigrationOutcomeUnknown, planned: []int64{3}, code: 2, failure: failed,
			ttl: long, pointer: kept, phase: operatorv1alpha1.MigrationPhaseBlocked,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, plan := awaitingApprovalFixture(t)
			operation := applyClaimFor(t, migration, plan)
			if test.approved {
				operation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: "approval", UID: "approval-uid"}
			}
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			report := &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up", Outcome: test.outcome, Planned: test.planned,
			}
			frame := migrationFrame(t, runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
				OperationID: operation.ID, ChildExitCode: test.code, MutationStarted: test.failure != nil,
				CoordinationDigest:   operation.CoordinationDigest,
				TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
				MigrationRun:         report,
				Error:                test.failure,
			})
			reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, plan, job, pod, verificationPolicyConfigMap())
			holdMigrationApplyLease(t, reconciler, api, migration)
			recorder := record.NewFakeRecorder(32)
			reconciler.Recorder = recorder
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if got := storedTTL(t, api, job); got == nil || *got != test.ttl {
				t.Fatalf("the Job's TTL = %v, want %d", got, test.ttl)
			}
			settled := readMigration(t, api, migration)
			run := settled.Status.LastRun
			if run == nil {
				t.Fatal("no run was recorded")
			}
			if settled.Status.Phase != test.phase {
				t.Fatalf("phase = %q, want %q", settled.Status.Phase, test.phase)
			}
			events := migrationEvents(recorder)
			assertNoShortening(t, events)
			where := "kubectl logs -n " + job.Namespace + " job/" + job.Name + " -c ptah"
			if test.pointer == "" {
				if strings.Contains(run.Message, where) {
					t.Fatalf("LastRun.Message = %q, want no pointer", run.Message)
				}
				return
			}
			pointer := test.pointer + " after the Job finishes: " + where
			if !strings.Contains(run.Message, pointer) {
				t.Fatalf("LastRun.Message = %q, want %q", run.Message, pointer)
			}
			if !slices.ContainsFunc(events, func(event string) bool {
				return strings.Contains(event, "MigrationRunFinished") && strings.Contains(event, pointer)
			}) {
				t.Fatalf("the Events %q do not say %q", events, pointer)
			}
			if test.phase == operatorv1alpha1.MigrationPhaseBlocked {
				blocked := meta.FindStatusCondition(settled.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
				if blocked == nil || !strings.Contains(blocked.Message, pointer) {
					t.Fatalf("Blocked = %#v, want it to say %q", blocked, pointer)
				}
			}
		})
	}
}

// reportingPod leaves what a runner leaves on a run that reported this outcome.
func reportingPod(t *testing.T, pod *corev1.Pod, operation *operatorv1alpha1.MigrationOperationStatus, outcome string) {
	t.Helper()
	encoded, err := runner.EncodeSummary(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply, OperationID: operation.ID,
		CoordinationDigest: operation.CoordinationDigest, TargetIdentityDigest: testDigest,
		MutationStarted: outcome != dataplane.MigrationOutcomeUpToDate,
		Uncertain:       outcome == dataplane.MigrationOutcomePartial || outcome == dataplane.MigrationOutcomeUnknown,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up", Outcome: outcome,
		},
	})
	if err != nil {
		t.Fatalf("EncodeSummary() error = %v", err)
	}
	pod.Status.ContainerStatuses[0].State.Terminated.Message = string(encoded)
}

// TestAnUncertainMigrationApplyKeepsItsLogByWhatItReported is the path that blocks the
// resource without taking the run's report: nothing read it, or something other than
// the run stopped the resource. Blocked is not what earns the long TTL, because an edit
// can do that much. What the run's Pod reported is, as it is at the webhook. A run that
// reported nothing is one nobody accounted for, and its log is all there is.
func TestAnUncertainMigrationApplyKeepsItsLogByWhatItReported(t *testing.T) {
	t.Parallel()

	long, short := jobclaim.FailedApplyRetentionTTLSeconds, jobclaim.CleanupTTLSeconds
	unread := errors.New("the Apply produced no readable account")
	const (
		kept      = "Anything the run wrote is in the Pod log for 24 hours"
		collected = "Anything the run wrote is in the Pod log for 5 minutes"
	)
	for _, test := range []struct {
		name     string
		approved bool
		reported string
		failure  error
		ttl      int32
		pointer  string
	}{
		{name: "approved", approved: true, failure: unread, ttl: long, pointer: kept},
		{name: "under Always, with nothing reported", failure: unread, ttl: long, pointer: kept},
		{
			name: "a result that wrote no line", approved: true,
			failure: resultFailure(&runner.ResultError{Code: "invalid_migration_run_output", Message: "failed strict validation"}, corev1.SecretKeySelector{}),
			ttl:     long,
		},
		{
			name:    "a result that wrote no line, under Always",
			failure: resultFailure(&runner.ResultError{Code: "invalid_migration_run_output", Message: "failed strict validation"}, corev1.SecretKeySelector{}),
			ttl:     long,
		},
		{name: "under Always, reported Partial", reported: dataplane.MigrationOutcomePartial, failure: unread, ttl: long, pointer: kept},
		{name: "under Always, reported Unknown", reported: dataplane.MigrationOutcomeUnknown, failure: unread, ttl: long, pointer: kept},
		{name: "under Always, reported Failed", reported: dataplane.MigrationOutcomeFailed, failure: unread, ttl: short, pointer: collected},
		{name: "under Always, reported Applied", reported: dataplane.MigrationOutcomeApplied, failure: unread, ttl: short, pointer: collected},
		{name: "approved, reported Failed", approved: true, reported: dataplane.MigrationOutcomeFailed, failure: unread, ttl: long, pointer: kept},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, plan := awaitingApprovalFixture(t)
			operation := applyClaimFor(t, migration, plan)
			if test.approved {
				operation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: "approval", UID: "approval-uid"}
			}
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			if test.reported != "" {
				reportingPod(t, pod, operation, test.reported)
			}
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, job, pod, verificationPolicyConfigMap())
			recorder := record.NewFakeRecorder(32)
			reconciler.Recorder = recorder
			if _, err := reconciler.retireUncertainMigrationApply(context.Background(), migration, job, test.failure, "", false); err != nil {
				t.Fatalf("retireUncertainMigrationApply() error = %v", err)
			}
			if got := storedTTL(t, api, job); got == nil || *got != test.ttl {
				t.Fatalf("the Job's TTL = %v, want %d", got, test.ttl)
			}
			settled := readMigration(t, api, migration)
			if settled.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked || settled.Status.UnresolvedRun == nil {
				t.Fatalf("phase = %q, unresolved run = %#v, want the resource blocked on the run", settled.Status.Phase, settled.Status.UnresolvedRun)
			}
			events := migrationEvents(recorder)
			assertNoShortening(t, events)
			message := settled.Status.LastRun.Message
			where := "kubectl logs -n " + job.Namespace + " job/" + job.Name + " -c ptah"
			if test.pointer == "" {
				if strings.Contains(message, where) {
					t.Fatalf("LastRun.Message = %q, want no pointer", message)
				}
				return
			}
			pointer := test.pointer + " after the Job finishes: " + where
			if !strings.Contains(message, pointer) {
				t.Fatalf("LastRun.Message = %q, want %q", message, pointer)
			}
			blocked := meta.FindStatusCondition(settled.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
			if blocked == nil || !strings.Contains(blocked.Message, pointer) {
				t.Fatalf("Blocked = %#v, want it to say %q", blocked, pointer)
			}
			if !slices.ContainsFunc(events, func(event string) bool {
				return strings.Contains(event, "MigrationRunUncertain") && strings.Contains(event, pointer)
			}) {
				t.Fatalf("the Events %q do not say %q", events, pointer)
			}
		})
	}
}

// TestAnEditThatStopsAMigrationEarnsNoLongTTL takes the way a person reaches that path:
// an edit while the run is out, so the claim's inputs moved and the run's report is not
// taken. The resource is Blocked either way, and what the run reported decides.
func TestAnEditThatStopsAMigrationEarnsNoLongTTL(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		reported string
		ttl      int32
	}{
		{name: "a run that reported Failed", reported: dataplane.MigrationOutcomeFailed, ttl: jobclaim.CleanupTTLSeconds},
		{name: "a run that reported Applied", reported: dataplane.MigrationOutcomeApplied, ttl: jobclaim.CleanupTTLSeconds},
		{name: "a run that reported Partial", reported: dataplane.MigrationOutcomePartial, ttl: jobclaim.FailedApplyRetentionTTLSeconds},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			migration, plan := awaitingApprovalFixture(t)
			migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
			operation := applyClaimFor(t, migration, plan)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			reportingPod(t, pod, operation, test.reported)
			migration.Spec.Suspend = true
			migration.Generation++
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, job, pod, verificationPolicyConfigMap())
			holdMigrationApplyLease(t, reconciler, api, migration)
			recorder := record.NewFakeRecorder(32)
			reconciler.Recorder = recorder
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			settled := readMigration(t, api, migration)
			if settled.Status.LastRun == nil || settled.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown ||
				settled.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked || settled.Status.UnresolvedRun == nil {
				t.Fatalf("phase = %q, last run = %#v, unresolved run = %#v, want the edit to have stopped the resource",
					settled.Status.Phase, settled.Status.LastRun, settled.Status.UnresolvedRun)
			}
			if got := storedTTL(t, api, job); got == nil || *got != test.ttl {
				t.Fatalf("the Job's TTL = %v, want %d", got, test.ttl)
			}
			assertNoShortening(t, migrationEvents(recorder))
		})
	}
}

// TestABlockedRunAGuardHeldToFiveMinutesSaysSo is the upgrade: the webhook and the
// guard of the release before this one refuse the long TTL on a run no approval names.
// The claim settles, the Job is collected on the short TTL, and everything that names
// the log gives the time the Job holds rather than the time it was due.
func TestABlockedRunAGuardHeldToFiveMinutesSaysSo(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
		OperationID: operation.ID, ChildExitCode: 2, MutationStarted: true, Uncertain: true,
		CoordinationDigest:   operation.CoordinationDigest,
		TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up", Outcome: dataplane.MigrationOutcomePartial, Planned: []int64{3},
		},
		Error: &runner.ResultError{Code: "child_exit", Message: "ptah exited with code 2"},
	})
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, plan, job, pod, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)
	refused := apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, job.Name, errors.New("no approval authorized this run"))
	reconciler.Client = interceptor.NewClient(api, interceptor.Funcs{
		Patch: func(ctx context.Context, writer client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if stored, isJob := object.(*batchv1.Job); isJob && stored.Spec.TTLSecondsAfterFinished != nil &&
				*stored.Spec.TTLSecondsAfterFinished == jobclaim.FailedApplyRetentionTTLSeconds {
				return refused
			}
			return writer.Patch(ctx, object, patch, options...)
		},
	})
	recorder := record.NewFakeRecorder(32)
	reconciler.Recorder = recorder
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := storedTTL(t, api, job); got == nil || *got != jobclaim.CleanupTTLSeconds {
		t.Fatalf("the Job's TTL = %v, want %d", got, jobclaim.CleanupTTLSeconds)
	}
	settled := readMigration(t, api, migration)
	if settled.Status.ActiveOperation != nil || settled.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, claim = %#v, want the claim settled and the resource blocked", settled.Status.Phase, settled.Status.ActiveOperation)
	}
	const pointer = "What Ptah reported is in the Pod log for 5 minutes after the Job finishes"
	blocked := meta.FindStatusCondition(settled.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
	if blocked == nil || !strings.Contains(blocked.Message, pointer) || !strings.Contains(settled.Status.LastRun.Message, pointer) {
		t.Fatalf("Blocked = %#v, LastRun.Message = %q, want them to say %q", blocked, settled.Status.LastRun.Message, pointer)
	}
	events := migrationEvents(recorder)
	if !slices.ContainsFunc(events, func(event string) bool {
		return strings.Contains(event, "MigrationRunFinished") && strings.Contains(event, pointer)
	}) {
		t.Fatalf("the Events %q do not say %q", events, pointer)
	}
	if !slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, "FailedApplyLogShortened") }) {
		t.Fatalf("the Events %q do not say the log was shortened", events)
	}
}

// TestAJobHoldingTheLongTTLAfterARollbackGetsNoWrite is the other direction: a Job this
// release gave 24 hours under no approval carries a TTL, so the manager that comes next
// writes nothing to it, whatever it would have asked for, and no older guard judges it.
func TestAJobHoldingTheLongTTLAfterARollbackGetsNoWrite(t *testing.T) {
	t.Parallel()

	long := jobclaim.FailedApplyRetentionTTLSeconds
	job := finishedJob(&long)
	refused := errors.New("no write reaches a Job that has a TTL")
	api := refusingCleanup(t, job, map[int32]error{long: refused, jobclaim.CleanupTTLSeconds: refused})
	if err := retainFailedApplyJob(context.Background(), api, job, false, func(string) {}); err != nil {
		t.Fatalf("retainFailedApplyJob() error = %v", err)
	}
	if got := storedTTL(t, api, job); got == nil || *got != long {
		t.Fatalf("the stored TTL = %v, want %d", got, long)
	}
}

// TestARetainedJobCanBeDeletedOnceItsRunIsSettled is what the security page tells a
// person: having read the reason, delete the Job. Status names the log in the write
// that retires the claim, so by then nothing waits on the Job, and the deletion
// changes nothing about what the resource goes on to do.
func TestARetainedJobCanBeDeletedOnceItsRunIsSettled(t *testing.T) {
	t.Parallel()

	t.Run("a schema", func(t *testing.T) {
		t.Parallel()
		apply, plan, policyConfig := dispatchedApplyWithPlan(t)
		apply.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{Name: "approval", UID: "approval-uid"}
		job, pod := terminalWorkload(apply, batchv1.JobComplete)
		frame := safetyRunnerFrame(t, runner.Result{
			ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply, OperationID: apply.Status.ActiveOperation.ID,
			ChildExitCode: 2, MutationStarted: true, Uncertain: true,
			CoordinationDigest:   apply.Status.Plan.CoordinationDigest,
			TargetIdentityDigest: apply.Status.Plan.TargetIdentityDigest,
			Error:                &runner.ResultError{Code: "apply_outcome_unknown", Message: "how far they got is unknown"},
		})
		reconciler, api := fakeReconciler(t, staticLogs{content: frame}, apply, plan, policyConfig, job, pod)
		reconciler.Locks = targetlock.New(api, api, nil)
		reconciler.Recorder = record.NewFakeRecorder(32)
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(apply)}
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		settled := safetyGetSchema(t, api, apply)
		applying := meta.FindStatusCondition(settled.Status.Conditions, operatorv1alpha1.ConditionApplying)
		if applying == nil || !strings.Contains(applying.Message, "kubectl logs") {
			t.Fatalf("Applying = %#v, want the pointer a person acts on", applying)
		}
		if settled.Status.ActiveOperation != nil {
			t.Fatalf("the claim %#v still stands where status names the log", settled.Status.ActiveOperation)
		}
		if settled.Status.PendingObservation == nil || settled.Status.PendingObservation.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown {
			t.Fatalf("PendingObservation = %#v, want the record the Job is no longer needed for", settled.Status.PendingObservation)
		}
		if err := api.Delete(context.Background(), job); err != nil {
			t.Fatalf("delete the Job: %v", err)
		}
		for pass := range 2 {
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("pass %d after the deletion: Reconcile() error = %v", pass, err)
			}
		}
		after := safetyGetSchema(t, api, apply)
		if after.Status.ActiveOperation != nil && after.Status.ActiveOperation.Type == operatorv1alpha1.OperationApply {
			t.Fatalf("the deletion led to another Apply: %#v", after.Status.ActiveOperation)
		}
		if after.Status.PendingObservation == nil {
			t.Fatal("the deletion settled the record without a reading of the database")
		}
	})
	t.Run("a migration", func(t *testing.T) {
		t.Parallel()
		migration, plan := awaitingApprovalFixture(t)
		operation := applyClaimFor(t, migration, plan)
		operation.ApprovalRef = &operatorv1alpha1.ImmutableObjectReference{Name: "approval", UID: "approval-uid"}
		job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
		frame := migrationFrame(t, runner.Result{
			ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
			OperationID: operation.ID, ChildExitCode: 2, MutationStarted: true,
			CoordinationDigest:   operation.CoordinationDigest,
			TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
			MigrationRun: &dataplane.MigrationRunReport{
				ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up", Outcome: dataplane.MigrationOutcomeFailed,
			},
			Error: &runner.ResultError{Code: "child_exit", Message: "ptah exited with code 2"},
		})
		reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, plan, job, pod, verificationPolicyConfigMap())
		holdMigrationApplyLease(t, reconciler, api, migration)
		reconciler.Recorder = record.NewFakeRecorder(32)
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		settled := readMigration(t, api, migration)
		if settled.Status.LastRun == nil || !strings.Contains(settled.Status.LastRun.Message, "kubectl logs") {
			t.Fatalf("LastRun = %#v, want the pointer a person acts on", settled.Status.LastRun)
		}
		if settled.Status.ActiveOperation != nil {
			t.Fatalf("the claim %#v still stands where status names the log", settled.Status.ActiveOperation)
		}
		if err := api.Delete(context.Background(), job); err != nil {
			t.Fatalf("delete the Job: %v", err)
		}
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("after the deletion: Reconcile() error = %v", err)
		}
		after := readMigration(t, api, migration)
		if after.Status.LastRun == nil || after.Status.LastRun.Outcome != settled.Status.LastRun.Outcome ||
			after.Status.LastRun.Message != settled.Status.LastRun.Message {
			t.Fatalf("the deletion moved the record: %#v", after.Status.LastRun)
		}
	})
}
