package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// The rows below drive a rotation through the reconciler and then do to the
// status what any later refusal does: write its own reason over PlanReady and
// ApprovalRequired and move the phase. The retirement used to be decided from
// exactly those, so each row fails against that code -- a second rotation, or
// a Job never cleaned up. Each also breaks the record the way a regression
// would and shows the outcome follows the record rather than the verdicts.

// rotatedJobs is a configuration whose execution binding differs from the one
// every fixture here was stored under.
var rotatedJobs = executionBindingJobs{
	ptahVersion: "v0.4.0", executorImage: "example.invalid/ptah@" + safetyOtherDigest,
	runnerImage: "example.invalid/operator@" + safetyOtherDigest,
	protocol:    int32(runner.ProtocolVersion) + 1,
}

const unretiredEpoch = "v1-44444444444444444444444444444444"

func reconcileRetirementPass(
	t *testing.T,
	reconciler *SchemaReconciler,
	request ctrl.Request,
	pass string,
) ctrl.Result {
	t.Helper()
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() to %s: %v", pass, err)
	}
	return result
}

// rewriteRetirementVerdicts overwrites the verdicts a reader sees, the way a
// later refusal does, and applies a broken record where the row asks for one.
func rewriteRetirementVerdicts(
	t *testing.T,
	api client.Client,
	schema *operatorv1alpha1.PtahSchema,
	breakRecord func(*operatorv1alpha1.BindingRetirementStatus),
) {
	t.Helper()
	stored := safetyGetSchema(t, api, schema)
	stored.Status.Phase = operatorv1alpha1.PhaseBlocked
	for _, conditionType := range []string{
		operatorv1alpha1.ConditionPlanReady,
		operatorv1alpha1.ConditionApprovalRequired,
	} {
		setCondition(stored, conditionType, metav1.ConditionFalse, operatorv1alpha1.ReasonRealmConflict,
			"a later refusal wrote its own verdict")
	}
	if breakRecord != nil {
		if stored.Status.PendingBindingRetirement == nil {
			t.Fatal("the rotation wrote no retirement record to break")
		}
		breakRecord(stored.Status.PendingBindingRetirement)
	}
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatalf("rewrite the retirement verdicts: %v", err)
	}
}

func wantRetirement(
	t *testing.T,
	schema *operatorv1alpha1.PtahSchema,
	want *operatorv1alpha1.BindingRetirementStatus,
) {
	t.Helper()
	if got := schema.Status.PendingBindingRetirement; !reflect.DeepEqual(got, want) {
		t.Fatalf("pending binding retirement = %#v, want %#v", got, want)
	}
}

func jobTTL(t *testing.T, api client.Client, job *batchv1.Job) *int32 {
	t.Helper()
	persisted := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), persisted); err != nil {
		t.Fatal(err)
	}
	return persisted.Spec.TTLSecondsAfterFinished
}

func finishJob(t *testing.T, api client.Client, job *batchv1.Job) {
	t.Helper()
	persisted := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), persisted); err != nil {
		t.Fatal(err)
	}
	persisted.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if err := api.Status().Update(context.Background(), persisted); err != nil {
		t.Fatalf("finish Job %s: %v", job.Name, err)
	}
}

// A rotation that finds an Apply claimed but not dispatched retires the claim
// and hands its database back in the same write, and records the plan whose
// approvals are still to be marked stale. The sweep reads the plan from the
// record, so a record naming another plan sweeps nothing.
func TestARotationRetiresAnUndispatchedClaimAndSweepsItsPlan(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name        string
		breakRecord func(*operatorv1alpha1.BindingRetirementStatus)
		wantStale   bool
	}{
		{name: "the record names the retired plan", wantStale: true},
		{
			name:        "a record naming another plan",
			breakRecord: func(record *operatorv1alpha1.BindingRetirementStatus) { record.Plan.UID = "another-plan-uid" },
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema, plan, approval, policyConfig := safetyApprovalFixture(t)
			future := metav1.NewTime(time.Date(2026, 8, 30, 12, 10, 0, 0, time.UTC))
			schema.Status.NextReconciliationTime = &future
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, approval, policyConfig)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
			reconcileRetirementPass(t, reconciler, request, "reserve the approval")
			reconcileRetirementPass(t, reconciler, request, "claim the Apply")
			claimed := safetyGetSchema(t, api, schema)
			if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationApply ||
				claimed.Status.ActiveOperation.DispatchStarted {
				t.Fatalf("fixture did not reach an undispatched Apply claim: %#v", claimed.Status.ActiveOperation)
			}
			retiredEpoch := claimed.Status.ExecutionBinding.Epoch

			reconciler.Jobs = rotatedJobs
			reconcileRetirementPass(t, reconciler, request, "rotate the binding")
			rotated := safetyGetSchema(t, api, schema)
			rolloutEpoch := rotated.Status.ExecutionBinding.Epoch
			if rolloutEpoch == retiredEpoch || rotated.Status.ActiveOperation != nil || rotated.Status.Plan != nil ||
				rotated.Status.PendingLockRelease == nil {
				t.Fatalf("rotation over an undispatched claim = %#v", rotated.Status)
			}
			wantRetirement(t, rotated, &operatorv1alpha1.BindingRetirementStatus{
				RetiredEpoch: retiredEpoch,
				Plan: &operatorv1alpha1.RetiredPlanReference{
					Name: plan.Name, UID: plan.UID, Fingerprint: plan.Spec.Fingerprint,
				},
			})

			rewriteRetirementVerdicts(t, api, schema, row.breakRecord)
			reconcileRetirementPass(t, reconciler, request, "release the retired claim's Lease")
			reconcileRetirementPass(t, reconciler, request, "sweep the retired plan's approvals")
			swept := safetyGetSchema(t, api, schema)
			if swept.Status.ExecutionBinding.Epoch != rolloutEpoch || swept.Status.PendingLockRelease != nil ||
				swept.Status.ActiveOperation != nil || swept.Status.PendingBindingRetirement != nil {
				t.Fatalf("the retirement did not settle under the epoch it installed (%s): %#v", rolloutEpoch, swept.Status)
			}
			persisted := &operatorv1alpha1.PtahSchemaApproval{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), persisted); err != nil {
				t.Fatal(err)
			}
			stale := findCondition(persisted.Status.Conditions, operatorv1alpha1.ConditionApprovalStale)
			gotStale := stale != nil && stale.Status == metav1.ConditionTrue &&
				stale.Reason == string(operatorv1alpha1.ReasonExecutionBindingChanged)
			if gotStale != row.wantStale {
				t.Fatalf("approval of the retired plan stale = %t (%#v), want %t", gotStale, stale, row.wantStale)
			}
		})
	}
}

// dispatchedApplyFixture is an Apply whose Job the manager dispatched and the
// Job controller has not finished, carrying the envelope the builder writes.
func dispatchedApplyFixture(t *testing.T) (*operatorv1alpha1.PtahSchema, []client.Object, *batchv1.Job) {
	t.Helper()

	schema, plan, approval, policyConfig := safetyApprovalFixture(t)
	planFingerprint := fingerprint.DigestBytes([]byte("retired Apply plan"))
	plan.Spec.Fingerprint = planFingerprint
	schema.Status.Plan.Fingerprint = planFingerprint
	approval.Spec.PlanFingerprint = planFingerprint
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseApplying
	schema.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
		Name: approval.Name, UID: approval.UID, Approver: approval.Spec.Approver, ApprovedAt: approval.Spec.ApprovedAt,
	}
	started := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	executionNotAfter := metav1.NewTime(started.Add(leaseDuration(schema) - time.Minute))
	target := databaseTargetBinding(schema.Spec.Target)
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationApply, ID: "retired-apply",
		StartedAt: metav1.NewTime(started), Attempt: 1, DispatchStarted: true,
		CoordinationDigest: testCoordinationDigest, TargetIdentityDigest: testDigest,
		LeaseDurationSeconds: int32(leaseDuration(schema) / time.Second), LeaseEpoch: testLeaseEpoch,
		ExecutionNotAfter: &executionNotAfter, TerminationGracePeriodSeconds: int64(applyTerminationGrace / time.Second),
		Target: &target, Source: artifactAccessBinding(schema),
		ObservationExclude:  append([]string(nil), schema.Spec.Policy.Exclude...),
		ObservationSeverity: schema.Spec.Policy.DriftSeverity, ObservationDev: schema.Spec.Dev.DeepCopy(),
		ObservationConnectTimeout: schema.Spec.Execution.ConnectTimeout,
		ObservationLockTimeout:    schema.Spec.Policy.LockTimeout,
	}
	bindActiveInput(t, schema)
	operation := schema.Status.ActiveOperation
	jobName, err := workload.NameFor(schema, *operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.JobName = jobName
	ensureTestAdmissionSnapshot(schema)
	applyJob, err := (fakeJobs{}).Build(schema, *operation, plan)
	if err != nil {
		t.Fatal(err)
	}
	applyJob.UID = "retired-apply-job-uid"
	operation.JobUID = applyJob.UID
	bindCurrentApplyJob(t, applyJob, schema, operation, schema.Status.Plan)
	return schema, []client.Object{schema, plan, approval, policyConfig, applyJob}, applyJob
}

// A rotation that finds an Apply already dispatched retires it into
// outcome-unknown proof, and the record names its Job. While that Job runs,
// no proof is claimed and nothing touches it, even once the Apply's own
// ObserveAfter horizon has passed; once it stops, its cleanup is scheduled and
// the record is cleared, and only then does proof start under the new epoch. A
// record that does not name the Job exactly -- another UID, another epoch --
// leaves the Job alone, and proof no longer waits for it.
func TestARetiredApplyJobIsCleanedUpOnceItStopsAndBeforeProof(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name        string
		breakRecord func(*operatorv1alpha1.BindingRetirementStatus)
		wantCleanup bool
	}{
		{name: "the record names the retired Apply Job", wantCleanup: true},
		{
			name:        "a record naming another Job identity",
			breakRecord: func(record *operatorv1alpha1.BindingRetirementStatus) { record.Job.UID = "a-job-that-ran-later" },
		},
		{
			name:        "a record that retired another epoch",
			breakRecord: func(record *operatorv1alpha1.BindingRetirementStatus) { record.RetiredEpoch = unretiredEpoch },
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema, objects, applyJob := dispatchedApplyFixture(t)
			retiredEpoch := schema.Status.ExecutionBinding.Epoch
			logs := &safetyCountingLogs{content: []byte("a retired Apply result must not be read")}
			reconciler, api := fakeReconciler(t, logs, objects...)
			reconciler.Jobs = rotatedJobs
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

			reconcileRetirementPass(t, reconciler, request, "retire the dispatched Apply")
			rotated := safetyGetSchema(t, api, schema)
			rolloutEpoch := rotated.Status.ExecutionBinding.Epoch
			pending := rotated.Status.PendingObservation
			if rolloutEpoch == retiredEpoch || rotated.Status.ActiveOperation != nil || pending == nil ||
				pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown || pending.ApplyJobUID != applyJob.UID ||
				pending.ObserveAfter == nil {
				t.Fatalf("rotation over a dispatched Apply = %#v", rotated.Status)
			}
			wantRetirement(t, rotated, &operatorv1alpha1.BindingRetirementStatus{
				RetiredEpoch: retiredEpoch,
				Plan: &operatorv1alpha1.RetiredPlanReference{
					Name: pending.Plan.Name, UID: pending.Plan.UID, Fingerprint: pending.Plan.Fingerprint,
				},
				Job: &operatorv1alpha1.RetiredJobStatus{
					Operation: operatorv1alpha1.OperationApply, Name: applyJob.Name, UID: applyJob.UID,
				},
			})
			// Past the horizon the pending observation would claim its proof at
			// once, so from here only the record holds it back.
			afterHorizon := pending.ObserveAfter.Add(time.Second)
			reconciler.Clock = func() time.Time { return afterHorizon }

			rewriteRetirementVerdicts(t, api, schema, row.breakRecord)
			reconcileRetirementPass(t, reconciler, request, "sweep the retired plan's approvals")

			// The predecessor's Job is still running, with no Pod left active.
			running := reconcileRetirementPass(t, reconciler, request, "wait for the running Apply Job")
			waiting := safetyGetSchema(t, api, schema)
			if running.RequeueAfter != maxLockContentionPoll || waiting.Status.ActiveOperation != nil ||
				waiting.Status.PendingObservation == nil || jobTTL(t, api, applyJob) != nil {
				t.Fatalf("a running retired Apply Job let proof start or was cleaned up: result %#v, status %#v",
					running, waiting.Status)
			}
			if held := waiting.Status.PendingBindingRetirement != nil; held != row.wantCleanup {
				t.Fatalf("the record still names the running Apply Job = %t, want %t: %#v",
					held, row.wantCleanup, waiting.Status.PendingBindingRetirement)
			}

			finishJob(t, api, applyJob)
			reconcileRetirementPass(t, reconciler, request, "schedule the stopped Apply Job's cleanup")
			cleaned := safetyGetSchema(t, api, schema)
			if ttl := jobTTL(t, api, applyJob); (ttl != nil) != row.wantCleanup {
				t.Fatalf("retired Apply Job cleanup TTL = %v, want scheduled %t", ttl, row.wantCleanup)
			}
			if cleaned.Status.PendingBindingRetirement != nil || cleaned.Status.ExecutionBinding.Epoch != rolloutEpoch ||
				cleaned.Status.PendingObservation == nil || cleaned.Status.Applied != nil {
				t.Fatalf("the retirement did not settle before proof: %#v", cleaned.Status)
			}
			if row.wantCleanup && cleaned.Status.ActiveOperation != nil {
				t.Fatalf("proof was claimed in the pass that scheduled the cleanup: %#v", cleaned.Status.ActiveOperation)
			}

			reconcileRetirementPass(t, reconciler, request, "claim proof under the new epoch")
			observing := safetyGetSchema(t, api, schema)
			if observing.Status.ActiveOperation == nil ||
				observing.Status.ActiveOperation.Type != operatorv1alpha1.OperationObserve ||
				observing.Status.ActiveOperation.ExecutionBindingID != rolloutEpoch || logs.reads != 0 {
				t.Fatalf("proof did not start under the new epoch after the retirement settled: %#v (log reads %d)",
					observing.Status, logs.reads)
			}
		})
	}
}

// A dispatched Apply whose claim recorded no UID may still have a create in
// flight that commits after the rotation. The record keeps looking for it
// under the claim's name until the Apply's ObserveAfter horizon, adopts its UID
// into the record and the pending observation when it appears, and gives up
// on it only once the horizon has passed.
func TestARetiredApplyCreateIsLookedForUntilItsHorizon(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		commits  bool
		wantUID  bool
		wantKept bool
	}{
		{name: "a create that commits before the horizon is adopted", commits: true, wantUID: true, wantKept: true},
		{name: "no create by the horizon"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema, objects, applyJob := dispatchedApplyFixture(t)
			schema.Status.ActiveOperation.JobUID = ""
			objects = objects[:len(objects)-1]
			reconciler, api := fakeReconciler(t, &safetyCountingLogs{}, objects...)
			reconciler.Jobs = rotatedJobs
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
			reconcileRetirementPass(t, reconciler, request, "retire the dispatched Apply")
			reconcileRetirementPass(t, reconciler, request, "sweep the retired plan's approvals")

			looking := reconcileRetirementPass(t, reconciler, request, "look for the Job before it commits")
			before := safetyGetSchema(t, api, schema)
			if retirement := before.Status.PendingBindingRetirement; looking.RequeueAfter != maxLockContentionPoll ||
				retirement == nil || retirement.Job == nil || retirement.Job.Name != applyJob.Name || retirement.Job.UID != "" ||
				before.Status.ActiveOperation != nil {
				t.Fatalf("the record stopped looking for the Apply's Job before its horizon: result %#v, status %#v",
					looking, before.Status)
			}

			if row.commits {
				if err := api.Create(context.Background(), applyJob); err != nil {
					t.Fatalf("commit the retired Apply's Job late: %v", err)
				}
			} else {
				afterHorizon := before.Status.PendingObservation.ObserveAfter.Add(time.Second)
				reconciler.Clock = func() time.Time { return afterHorizon }
			}
			reconcileRetirementPass(t, reconciler, request, "look for the Job again")
			after := safetyGetSchema(t, api, schema)
			retirement := after.Status.PendingBindingRetirement
			if kept := retirement != nil && retirement.Job != nil; kept != row.wantKept {
				t.Fatalf("the record still names the Apply's Job = %t, want %t: %#v", kept, row.wantKept, retirement)
			}
			adopted := after.Status.PendingObservation.ApplyJobUID == applyJob.UID &&
				retirement != nil && retirement.Job != nil && retirement.Job.UID == applyJob.UID
			if adopted != row.wantUID {
				t.Fatalf("the late Apply Job's UID adopted = %t, want %t: pending UID %q, record %#v",
					adopted, row.wantUID, after.Status.PendingObservation.ApplyJobUID, retirement)
			}
			if after.Status.ActiveOperation != nil {
				t.Fatalf("proof was claimed in the pass that settled the Job: %#v", after.Status.ActiveOperation)
			}
		})
	}
}

// An Apply settled as outcome-unknown without its Job in hand can leave a Job
// no pass harvested. A rotation that finds such a pending observation, and no
// claim, names the Job in the record, so it is cleaned up before the proof.
func TestARotationNamesTheJobOfAnUnknownApply(t *testing.T) {
	t.Parallel()

	schema, job := predecessorApplyCleanupMatchFixture(t)
	retiredEpoch := schema.Status.PendingObservation.Plan.ExecutionBindingID
	schema.Status.ExecutionBinding.Epoch = retiredEpoch
	schema.Status.PendingBindingRetirement = nil
	schema.Finalizers = []string{activeOperationFinalizer}
	pending := schema.Status.PendingObservation
	pending.CoordinationDigest = testCoordinationDigest
	pending.Plan.CoordinationDigest = testCoordinationDigest
	pending.LeaseDurationSeconds = 960
	pending.LeaseEpoch = testLeaseEpoch
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	reconciler, api := fakeReconciler(t, &safetyCountingLogs{}, schema, job)
	reconciler.Jobs = rotatedJobs
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}

	reconcileRetirementPass(t, reconciler, request, "rotate over outcome-unknown evidence")
	rotated := safetyGetSchema(t, api, schema)
	if rotated.Status.ExecutionBinding.Epoch == retiredEpoch || rotated.Status.PendingObservation == nil {
		t.Fatalf("rotation over outcome-unknown evidence = %#v", rotated.Status)
	}
	wantRetirement(t, rotated, &operatorv1alpha1.BindingRetirementStatus{
		RetiredEpoch: retiredEpoch,
		Job: &operatorv1alpha1.RetiredJobStatus{
			Operation: operatorv1alpha1.OperationApply, Name: job.Name, UID: job.UID,
		},
	})

	reconcileRetirementPass(t, reconciler, request, "schedule the unharvested Apply Job's cleanup")
	if ttl := jobTTL(t, api, job); ttl == nil || *ttl != jobCleanupTTLSeconds {
		t.Fatalf("unharvested Apply Job cleanup TTL = %v, want %d", ttl, jobCleanupTTLSeconds)
	}
	if settled := safetyGetSchema(t, api, schema); settled.Status.PendingBindingRetirement != nil {
		t.Fatalf("the retirement outlived the cleanup it owed: %#v", settled.Status.PendingBindingRetirement)
	}
}

// declarePodMetadata gives a built read-only Job what spec.execution.podMetadata
// declares, as the builder writes it: on the Job and on its template, pinned
// by the claim's admission snapshot.
func declarePodMetadata(
	t *testing.T,
	schema *operatorv1alpha1.PtahSchema,
	job *batchv1.Job,
	operation *operatorv1alpha1.ActiveOperationStatus,
) {
	t.Helper()
	for key, value := range schema.Spec.Execution.PodMetadata.Labels {
		job.Labels[key] = string(value)
		job.Spec.Template.Labels[key] = string(value)
	}
	for key, value := range schema.Spec.Execution.PodMetadata.Annotations {
		job.Annotations[key] = string(value)
		job.Spec.Template.Annotations[key] = string(value)
	}
	templateDigest, err := podintent.DigestTemplate(&job.Spec.Template)
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
	job.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshotDigest
	job.Spec.Template.Annotations[workload.AnnotationAdmissionSnapshotDigest] = snapshotDigest
}

// A rotation keeps a read-only claim whose Job may still reach the database,
// and the claim goes on holding the Lease it took until that Job stops,
// whatever the controller can prove about the Job's envelope. Only then is the
// claim retired and its Lease handed back, in one write that also clears the
// record; the cleanup TTL is set only on a Job whose envelope the claim and the
// record both vouch for, declared Pod metadata included.
func TestARetiredReadOnlyClaimHoldsItsLeaseUntilItsJobStops(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name        string
		declare     bool
		breakRecord func(*operatorv1alpha1.BindingRetirementStatus)
		wantCleanup bool
	}{
		{name: "the record names the retained claim", wantCleanup: true},
		{name: "a Job that carries declared Pod metadata", declare: true, wantCleanup: true},
		{
			name:        "a record that retired another epoch",
			breakRecord: func(record *operatorv1alpha1.BindingRetirementStatus) { record.RetiredEpoch = unretiredEpoch },
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture()
			schema.Finalizers = []string{activeOperationFinalizer}
			if row.declare {
				schema.Spec.Execution.PodMetadata = &operatorv1alpha1.PodMetadataSpec{
					Labels:      map[string]operatorv1alpha1.PodLabelValue{"acme.example/team": "platform"},
					Annotations: map[string]operatorv1alpha1.PodAnnotationValue{"sidecar.istio.io/inject": "false"},
				}
			}
			schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
				RequestedReference: schema.Spec.Desired.OCIRef,
				ResolvedReference:  "oci://registry.example/team/schema@" + testDigest,
				Digest:             testDigest, ArtifactType: "application/vnd.stokaro.ptah.schema.v1", Verified: true,
				VerificationPolicyUID: testPolicyUID, VerificationPolicyDigest: testDigest,
			}
			schema.Status.Target = operatorv1alpha1.TargetStatus{
				Engine: schema.Spec.Target.Engine, CoordinationDigest: testCoordinationDigest,
				IdentityDigest: testDigest, DriftReportDigest: safetyOtherDigest,
			}
			schema.Status.Phase = operatorv1alpha1.PhasePlanning
			schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
				Type: operatorv1alpha1.OperationPlan, ID: "retired-running-plan", JobUID: "job-uid",
				StartedAt: metav1.Now(), Attempt: 1,
			}
			bindActiveInput(t, schema)
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			bindRetiredReadOnlyJob(job, schema.Status.ActiveOperation)
			if row.declare {
				declarePodMetadata(t, schema, job, schema.Status.ActiveOperation)
			}
			job.Status.Conditions = nil
			pod.Status = corev1.PodStatus{Phase: corev1.PodRunning}
			retiredEpoch := schema.Status.ExecutionBinding.Epoch
			retained := schema.Status.ActiveOperation.DeepCopy()
			logs := &safetyCountingLogs{content: []byte("a retired Plan result must not be read")}
			reconciler, api := fakeReconciler(t, logs, schema, job, pod)
			reconciler.Jobs = rotatedJobs
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
			holder := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest)
			if holder == "" {
				t.Fatal("the running Plan's claim holds no Lease, so nothing below measures one")
			}

			reconcileRetirementPass(t, reconciler, request, "rotate over a running Plan")
			rotated := safetyGetSchema(t, api, schema)
			rolloutEpoch := rotated.Status.ExecutionBinding.Epoch
			if rolloutEpoch == retiredEpoch || rotated.Status.ActiveOperation == nil ||
				rotated.Status.ActiveOperation.ID != retained.ID || rotated.Status.PendingLockRelease != nil {
				t.Fatalf("rotation did not keep the running read-only claim: %#v", rotated.Status)
			}
			wantRetirement(t, rotated, &operatorv1alpha1.BindingRetirementStatus{
				RetiredEpoch: retiredEpoch,
				Job: &operatorv1alpha1.RetiredJobStatus{
					Operation: operatorv1alpha1.OperationPlan, Name: retained.JobName, UID: retained.JobUID,
				},
			})

			rewriteRetirementVerdicts(t, api, schema, row.breakRecord)
			result := reconcileRetirementPass(t, reconciler, request, "wait for the running Plan Job")
			waiting := safetyGetSchema(t, api, schema)
			if waiting.Status.ExecutionBinding.Epoch != rolloutEpoch || jobTTL(t, api, job) != nil || logs.reads != 0 {
				t.Fatalf("a running retired Plan Job was rotated again, cleaned or read: %#v", waiting.Status)
			}
			if waiting.Status.ActiveOperation == nil || waiting.Status.ActiveOperation.ID != retained.ID ||
				waiting.Status.PendingLockRelease != nil || result.RequeueAfter != maxLockContentionPoll ||
				safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest) != holder {
				t.Fatalf("the running Plan's claim let go of its Lease: result %#v, status %#v", result, waiting.Status)
			}

			finishJob(t, api, job)
			reconcileRetirementPass(t, reconciler, request, "retire the stopped Plan claim")
			retired := safetyGetSchema(t, api, schema)
			if ttl := jobTTL(t, api, job); (ttl != nil) != row.wantCleanup {
				t.Fatalf("stopped retired Plan Job cleanup TTL = %v, want scheduled %t", ttl, row.wantCleanup)
			}
			if retired.Status.ActiveOperation != nil || retired.Status.PendingBindingRetirement != nil ||
				retired.Status.PendingLockRelease == nil || retired.Status.PendingLockRelease.OperationID != retained.ID ||
				retired.Status.ExecutionBinding.Epoch != rolloutEpoch || logs.reads != 0 {
				t.Fatalf("the stopped Plan's claim was not retired with its Lease staged: %#v", retired.Status)
			}
			reconcileRetirementPass(t, reconciler, request, "hand the stopped Plan's Lease back")
			if got := safetyLeaseHolder(t, api, reconciler.LockNamespace, testCoordinationDigest); got != "" {
				t.Fatalf("the retired Plan's Lease is still held by %q", got)
			}
		})
	}
}

// One retirement at a time. A configuration rolled back while the retired
// Apply's Job still runs does not rotate the binding again until the record
// has been worked off; then it claims an epoch of its own. The record always
// describes exactly one retired epoch.
func TestASecondRotationWaitsForTheFirstToSettle(t *testing.T) {
	t.Parallel()

	schema, objects, applyJob := dispatchedApplyFixture(t)
	originalEpoch := schema.Status.ExecutionBinding.Epoch
	reconciler, api := fakeReconciler(t, &safetyCountingLogs{}, objects...)
	reconciler.Jobs = rotatedJobs
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	reconcileRetirementPass(t, reconciler, request, "retire the dispatched Apply")
	rolloutEpoch := safetyGetSchema(t, api, schema).Status.ExecutionBinding.Epoch

	reconciler.Jobs = fakeJobs{}
	for _, pass := range []string{"sweep the retired plan's approvals", "wait for the running Apply Job"} {
		reconcileRetirementPass(t, reconciler, request, pass)
		waiting := safetyGetSchema(t, api, schema)
		if waiting.Status.ExecutionBinding.Epoch != rolloutEpoch || waiting.Status.PendingBindingRetirement == nil {
			t.Fatalf("the rollback rotated while the first retirement was unsettled, at %q: %#v", pass, waiting.Status)
		}
	}

	finishJob(t, api, applyJob)
	reconcileRetirementPass(t, reconciler, request, "schedule the stopped Apply Job's cleanup")
	settled := safetyGetSchema(t, api, schema)
	if settled.Status.ExecutionBinding.Epoch != rolloutEpoch || settled.Status.PendingBindingRetirement != nil ||
		jobTTL(t, api, applyJob) == nil {
		t.Fatalf("the first retirement did not settle before the rollback: %#v", settled.Status)
	}

	reconcileRetirementPass(t, reconciler, request, "rotate to the rolled-back components")
	rolledBack := safetyGetSchema(t, api, schema)
	if epoch := rolledBack.Status.ExecutionBinding.Epoch; epoch == rolloutEpoch || epoch == originalEpoch ||
		rolledBack.Status.ExecutionBinding.ExecutorImage != "example.invalid/ptah@"+testDigest {
		t.Fatalf("the rollback did not claim an epoch of its own: %#v", rolledBack.Status.ExecutionBinding)
	}
	// Nothing of the rollout epoch is left to retire: no plan was published
	// under it and no claim was taken.
	if rolledBack.Status.PendingBindingRetirement != nil || rolledBack.Status.PendingObservation == nil {
		t.Fatalf("the rollback rotation = %#v", rolledBack.Status)
	}
}

// Proof that completes after its plan's epoch was retired keeps the Apply's
// attribution and sweeps the applied plan's approvals in the same pass: an
// approval CREATE admitted before the Apply claim may have committed after the
// rotation's own sweep. The sweep reaches approvals of that plan and no other.
func TestProofUnderANewEpochSweepsTheAppliedPlanAtOnce(t *testing.T) {
	t.Parallel()

	schema := safetyPostApplyObserveSchema(t)
	appliedEpoch := schema.Status.ExecutionBinding.Epoch
	rotated := schema.Status.ExecutionBinding.DeepCopy()
	rotated.Epoch = "v1-55555555555555555555555555555555"
	schema.Status.ExecutionBinding = rotated
	schema.Status.PendingObservation.PlanRequired = true
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationPlan, ID: "proof-plan", JobName: "proof-plan-job", JobUID: "job-uid",
		StartedAt: metav1.Now(), Attempt: 1,
	}
	bindActiveInput(t, schema)
	pending := schema.Status.PendingObservation
	approvalFor := func(name string, planUID types.UID) *operatorv1alpha1.PtahSchemaApproval {
		return &operatorv1alpha1.PtahSchemaApproval{
			ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: name, UID: types.UID(name + "-uid")},
			Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
				SchemaRef:          operatorv1alpha1.ImmutableObjectReference{Name: schema.Name, UID: schema.UID},
				PlanRef:            operatorv1alpha1.ImmutableObjectReference{Name: pending.Plan.Name, UID: planUID},
				PlanFingerprint:    pending.Plan.Fingerprint,
				Approver:           operatorv1alpha1.ApprovalIdentity{Username: "approver@example.com"},
				ApprovedAt:         metav1.NewTime(time.Date(2026, 8, 30, 11, 30, 0, 0, time.UTC)),
				MutationRequestUID: name + "-request",
			},
		}
	}
	lateApproval := approvalFor("late-applied-plan-approval", pending.Plan.UID)
	otherApproval := approvalFor("another-plan-approval", "another-plan-uid")
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, lateApproval, otherApproval)

	stored := safetyGetSchema(t, api, schema)
	if _, err := reconciler.consumeResult(context.Background(), stored, nil, runner.Result{
		Operation:            runner.OperationPlan,
		CoordinationDigest:   stored.Status.PendingObservation.CoordinationDigest,
		TargetIdentityDigest: stored.Status.PendingObservation.Plan.TargetIdentityDigest,
		PlanOutcome:          runner.PlanOutcomeNoChanges,
	}, nil, 0); err != nil {
		t.Fatalf("complete proof under the new epoch: %v", err)
	}

	proved := safetyGetSchema(t, api, schema)
	if proved.Status.PendingObservation != nil || proved.Status.ActiveOperation != nil || proved.Status.Plan != nil ||
		proved.Status.PendingBindingRetirement != nil || proved.Status.Applied == nil ||
		proved.Status.Applied.ExecutionBindingID != appliedEpoch || proved.Status.Phase != operatorv1alpha1.PhasePending {
		t.Fatalf("proof under the new epoch = %#v", proved.Status)
	}
	for _, check := range []struct {
		approval  *operatorv1alpha1.PtahSchemaApproval
		wantStale bool
	}{
		{approval: lateApproval, wantStale: true},
		{approval: otherApproval},
	} {
		persisted := &operatorv1alpha1.PtahSchemaApproval{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(check.approval), persisted); err != nil {
			t.Fatal(err)
		}
		stale := findCondition(persisted.Status.Conditions, operatorv1alpha1.ConditionApprovalStale)
		gotStale := stale != nil && stale.Status == metav1.ConditionTrue &&
			stale.Reason == string(operatorv1alpha1.ReasonExecutionBindingChanged)
		if gotStale != check.wantStale {
			t.Fatalf("approval %s stale = %t (%#v), want %t", check.approval.Name, gotStale, stale, check.wantStale)
		}
	}
}
