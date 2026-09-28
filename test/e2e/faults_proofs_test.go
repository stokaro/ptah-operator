package e2e

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	proofEpoch        = "v1-0123456789abcdef0123456789abcdef"
	proofLeaseEpoch   = "v1-11111111111111111111111111111111"
	proofOtherEpoch   = "v1-22222222222222222222222222222222"
	proofStateVersion = int32(4)
	proofSchemaName   = "e2e-fault-pg-restart"
	proofApplyID      = "apply-operation"
	proofApplyJob     = "apply-job"
	proofApplyJobUID  = "apply-job-uid"
	proofApplyPodUID  = "apply-pod-uid"
	proofObserveID    = "observe-operation"
	proofObserveUID   = "observe-job-uid"
	proofPlanID       = "plan-operation"
	proofPlanUID      = "plan-job-uid"
	proofLeaseUID     = "lease-uid"
	proofLeaseName    = "ptah-target-lease"
	proofLeaseHolder  = "manager-a_1234"
)

var proofController = controllerIdentity{
	image:        "registry.invalid/operator@sha256:" + strings.Repeat("a", 64),
	revision:     "revision-1",
	stateVersion: "4",
}

// proofCase is one mutation a proof has to refuse.
type proofCase[F any] struct {
	name   string
	mutate func(*F)
}

// proofRefusesEach holds a proof to its fixture and to every mutation: the
// fixture passes, and each mutation, applied to a fresh fixture, fails.
func proofRefusesEach[F any](t *testing.T, build func() *F, check func(*F) error, cases []proofCase[F]) {
	t.Helper()
	t.Run("accepts the fixture", func(t *testing.T) {
		t.Parallel()
		if err := check(build()); err != nil {
			t.Fatalf("the fixture was refused: %v", err)
		}
	})
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := build()
			test.mutate(fixture)
			if err := check(fixture); err == nil {
				t.Fatal("the mutation was accepted")
			}
		})
	}
}

// proofBool turns a predicate into the error proofRefusesEach reads.
func proofBool(ok bool) error {
	if ok {
		return nil
	}
	return errProofRefused
}

var errProofRefused = errors.New("refused")

func proofDigest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}

func proofTime(seconds int) metav1.Time {
	return metav1.NewTime(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC).Add(time.Duration(seconds) * time.Second))
}

func proofTimePointer(seconds int) *metav1.Time {
	instant := proofTime(seconds)
	return &instant
}

func proofPlan() ptahv1alpha1.CurrentPlanStatus {
	return ptahv1alpha1.CurrentPlanStatus{
		Name: "e2e-fault-pg-restart-plan-a", UID: "plan-uid-a",
		Fingerprint: proofDigest("f"), ContentDigest: proofDigest("c"), ArtifactDigest: proofDigest("a"),
		CoordinationDigest: proofDigest("d"), TargetIdentityDigest: proofDigest("e"),
		ActualStateFingerprint: proofDigest("1"), DesiredStateFingerprint: proofDigest("2"),
		PolicyFingerprint: proofDigest("3"), VerificationPolicyUID: "policy-uid",
		VerificationPolicyDigest: proofDigest("4"), ExecutionBindingID: proofEpoch,
		ControllerImage: proofController.image, ControllerRevision: proofController.revision,
		ControllerStateVersion: proofStateVersion, PtahVersion: "v1.2.3",
		ExecutorImage: "registry.invalid/ptah@sha256:" + strings.Repeat("b", 64),
		RunnerImage:   "registry.invalid/runner@sha256:" + strings.Repeat("9", 64), RunnerProtocolVersion: 7,
		StatementCount: 2, CreatedAt: proofTime(0),
	}
}

func proofBinding() *ptahv1alpha1.ExecutionBindingStatus {
	return &ptahv1alpha1.ExecutionBindingStatus{
		Epoch: proofEpoch, ControllerStateVersion: proofStateVersion, PtahVersion: "v1.2.3",
		ExecutorImage: "registry.invalid/ptah@sha256:" + strings.Repeat("b", 64), RunnerProtocolVersion: 7,
	}
}

func proofTargetBinding() ptahv1alpha1.DatabaseTargetBinding {
	return ptahv1alpha1.DatabaseTargetBinding{
		Engine: "PostgreSQL",
		URLFrom: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "e2e-fault-pg-restart-db"}, Key: "url",
		},
	}
}

func proofSourceBinding() ptahv1alpha1.OCIArtifactAccessBinding {
	return ptahv1alpha1.OCIArtifactAccessBinding{
		ResolvedReference: "oci://registry.invalid/schemas/fault-postgresql@" + proofDigest("a"),
		Digest:            proofDigest("a"),
	}
}

func proofPending(outcome ptahv1alpha1.PendingObservationOutcome) *ptahv1alpha1.PendingObservationStatus {
	return &ptahv1alpha1.PendingObservationStatus{
		Outcome: outcome, ApplyOperationID: proofApplyID, ApplyJobName: proofApplyJob, ApplyJobUID: proofApplyJobUID,
		ApplyPodUIDs: []types.UID{proofApplyPodUID}, ApplyPodCount: 1, ApplyGeneration: 3,
		ObserveAfter: proofTimePointer(40), Plan: proofPlan(), Target: proofTargetBinding(),
		CoordinationDigest: proofDigest("d"), Source: proofSourceBinding(), Exclude: []string{"audit_log"},
		DriftSeverity: "all", ConnectTimeout: metav1.Duration{Duration: 30 * time.Second},
		LockTimeout: metav1.Duration{Duration: time.Minute}, LeaseDurationSeconds: 30, LeaseEpoch: proofLeaseEpoch,
	}
}

func proofActive(kind ptahv1alpha1.OperationType, id, jobUID string) *ptahv1alpha1.ActiveOperationStatus {
	target, source := proofTargetBinding(), proofSourceBinding()
	return &ptahv1alpha1.ActiveOperationStatus{
		Type: kind, ID: id, JobName: strings.ToLower(string(kind)) + "-job", JobUID: types.UID(jobUID),
		ExecutionBindingID: proofEpoch, CoordinationDigest: proofDigest("d"), TargetIdentityDigest: proofDigest("e"),
		Target: &target, Source: &source, ObservationExclude: []string{"audit_log"}, ObservationSeverity: "all",
		ObservationConnectTimeout: metav1.Duration{Duration: 30 * time.Second},
		ObservationLockTimeout:    metav1.Duration{Duration: time.Minute},
		LeaseDurationSeconds:      30, LeaseEpoch: proofLeaseEpoch,
	}
}

func proofApplyActive() *ptahv1alpha1.ActiveOperationStatus {
	active := proofActive(ptahv1alpha1.OperationApply, proofApplyID, proofApplyJobUID)
	active.JobName = proofApplyJob
	active.DispatchStarted = true
	active.DispatchNotAfter, active.ExecutionNotAfter = proofTimePointer(600), proofTimePointer(600)
	active.TerminationGracePeriodSeconds = 30
	return active
}

func proofVerifyingConditions() []metav1.Condition {
	return []metav1.Condition{
		{Type: ptahv1alpha1.ConditionInSync, Status: metav1.ConditionFalse, Reason: "VerifyingConvergence"},
		{Type: ptahv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "VerifyingConvergence"},
		{Type: ptahv1alpha1.ConditionApplying, Status: metav1.ConditionFalse, Reason: "OutcomeUnknown"},
	}
}

func proofSchema(name string, status ptahv1alpha1.PtahSchemaStatus) *ptahv1alpha1.PtahSchema {
	return &ptahv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: "schema-uid"},
		Status:     status,
	}
}

func proofTarget() ptahv1alpha1.TargetStatus {
	return ptahv1alpha1.TargetStatus{
		CoordinationDigest: proofDigest("d"), IdentityDigest: proofDigest("e"), DriftReportDigest: proofDigest("7"),
		LastObservedAt: proofTimePointer(120),
	}
}

func proofJob(uid, operationID string, complete bool) *batchv1.Job {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: uid + "-name", UID: types.UID(uid), Annotations: map[string]string{annotationOperationID: operationID},
	}}
	if complete {
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	}
	return job
}

func proofLease(uid, holder, epoch string) *coordinationv1.Lease {
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: proofLeaseName, UID: types.UID(uid), Annotations: map[string]string{annotationLeaseEpoch: epoch},
	}}
	if holder != "" {
		lease.Spec.HolderIdentity = ptr.To(holder)
	}
	return lease
}

func proofLeaseIdentity() leaseIdentity {
	return leaseIdentity{name: proofLeaseName, uid: proofLeaseUID, holder: proofLeaseHolder, epoch: proofLeaseEpoch}
}

// heldFixture is a schema a heldProof reads, and the proof.
type heldFixture struct {
	schema *ptahv1alpha1.PtahSchema
	proof  heldProof
}

func heldConvergenceFixture(stage ptahv1alpha1.OperationType) func() *heldFixture {
	return func() *heldFixture {
		pending := proofPending(ptahv1alpha1.PendingObservationApplySucceeded)
		jobUID, id := proofObserveUID, proofObserveID
		if stage == ptahv1alpha1.OperationPlan {
			pending.PlanRequired = true
			jobUID, id = proofPlanUID, proofPlanID
		}
		controller := proofController
		return &heldFixture{
			schema: proofSchema(proofSchemaName, ptahv1alpha1.PtahSchemaStatus{
				Phase: ptahv1alpha1.PhaseVerifyingConvergence, ExecutionBinding: proofBinding(),
				PendingObservation: pending, ActiveOperation: proofActive(stage, id, jobUID),
				Conditions: proofVerifyingConditions(),
			}),
			proof: heldProof{
				stage: stage, operationID: id, jobUID: jobUID, leaseEpoch: proofLeaseEpoch,
				outcome: ptahv1alpha1.PendingObservationApplySucceeded, applyOperationID: proofApplyID,
				controller: &controller, stateVersion: proofStateVersion,
			},
		}
	}
}

func heldUncertainFixture(stage ptahv1alpha1.OperationType, optional bool) func() *heldFixture {
	return func() *heldFixture {
		fixture := heldConvergenceFixture(stage)()
		fixture.schema.Status.PendingObservation.Outcome = ptahv1alpha1.PendingObservationOutcomeUnknown
		fixture.proof.outcome = ptahv1alpha1.PendingObservationOutcomeUnknown
		fixture.proof.applyJob = &jobRef{name: proofApplyJob, uid: proofApplyJobUID}
		fixture.proof.applyPods = &podEvidence{uids: []string{proofApplyPodUID}, optional: optional}
		return fixture
	}
}

func TestHeldProofHoldsTheProofSnapshot(t *testing.T) {
	t.Parallel()
	check := func(f *heldFixture) error { return f.proof.check(f.schema) }
	common := []proofCase[heldFixture]{
		{"no active operation", func(f *heldFixture) { f.schema.Status.ActiveOperation = nil }},
		{"another operation type", func(f *heldFixture) { f.schema.Status.ActiveOperation.Type = ptahv1alpha1.OperationApply }},
		{"another operation ID", func(f *heldFixture) { f.schema.Status.ActiveOperation.ID = "other" }},
		{"another Job UID", func(f *heldFixture) { f.schema.Status.ActiveOperation.JobUID = "other" }},
		{"no Job UID against an empty one", func(f *heldFixture) {
			f.schema.Status.ActiveOperation.JobUID, f.proof.jobUID = "", ""
		}},
		{"another active epoch", func(f *heldFixture) { f.schema.Status.ActiveOperation.LeaseEpoch = proofOtherEpoch }},
		{"no pending observation", func(f *heldFixture) { f.schema.Status.PendingObservation = nil }},
		{"another outcome", func(f *heldFixture) {
			if f.proof.outcome == ptahv1alpha1.PendingObservationApplySucceeded {
				f.schema.Status.PendingObservation.Outcome = ptahv1alpha1.PendingObservationOutcomeUnknown
			} else {
				f.schema.Status.PendingObservation.Outcome = ptahv1alpha1.PendingObservationApplySucceeded
			}
		}},
		{"another Apply operation", func(f *heldFixture) { f.schema.Status.PendingObservation.ApplyOperationID = "other" }},
		{"another pending epoch", func(f *heldFixture) { f.schema.Status.PendingObservation.LeaseEpoch = proofOtherEpoch }},
		{"no execution binding", func(f *heldFixture) { f.schema.Status.ExecutionBinding = nil }},
		{"a plan bound elsewhere", func(f *heldFixture) { f.schema.Status.PendingObservation.Plan.ExecutionBindingID = proofOtherEpoch }},
		{"another manager image", func(f *heldFixture) { f.schema.Status.PendingObservation.Plan.ControllerImage = "other" }},
		{"another manager revision", func(f *heldFixture) { f.schema.Status.PendingObservation.Plan.ControllerRevision = "other" }},
		{"another controller state", func(f *heldFixture) { f.schema.Status.PendingObservation.Plan.ControllerStateVersion = 3 }},
		{"planRequired flipped", func(f *heldFixture) {
			f.schema.Status.PendingObservation.PlanRequired = !f.schema.Status.PendingObservation.PlanRequired
		}},
		{"another phase", func(f *heldFixture) { f.schema.Status.Phase = ptahv1alpha1.PhaseInSync }},
		{"an applied plan", func(f *heldFixture) { f.schema.Status.Applied = &ptahv1alpha1.AppliedStatus{} }},
		{"a lock release", func(f *heldFixture) { f.schema.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{} }},
		{"InSync true", func(f *heldFixture) { f.schema.Status.Conditions[0].Status = metav1.ConditionTrue }},
		{"Ready true", func(f *heldFixture) { f.schema.Status.Conditions[1].Status = metav1.ConditionTrue }},
		{"no conditions", func(f *heldFixture) { f.schema.Status.Conditions = nil }},
		{"a stage that is no proof", func(f *heldFixture) {
			f.proof.stage = ptahv1alpha1.OperationApply
			f.schema.Status.ActiveOperation.Type = ptahv1alpha1.OperationApply
		}},
	}
	uncertain := append(slices.Clone(common),
		proofCase[heldFixture]{"another Apply Job name", func(f *heldFixture) { f.schema.Status.PendingObservation.ApplyJobName = "other" }},
		proofCase[heldFixture]{"no Apply Job name", func(f *heldFixture) { f.schema.Status.PendingObservation.ApplyJobName = "" }},
		proofCase[heldFixture]{"another Apply Job UID", func(f *heldFixture) { f.schema.Status.PendingObservation.ApplyJobUID = "other" }},
		proofCase[heldFixture]{"another Apply Pod", func(f *heldFixture) {
			f.schema.Status.PendingObservation.ApplyPodUIDs = []types.UID{"other"}
		}},
		proofCase[heldFixture]{"a Pod count that disagrees", func(f *heldFixture) { f.schema.Status.PendingObservation.ApplyPodCount = 2 }},
		proofCase[heldFixture]{"a second Apply Pod", func(f *heldFixture) {
			f.schema.Status.PendingObservation.ApplyPodUIDs = []types.UID{proofApplyPodUID, "other"}
			f.schema.Status.PendingObservation.ApplyPodCount = 2
		}},
	)
	t.Run("convergence Observe", func(t *testing.T) {
		t.Parallel()
		proofRefusesEach(t, heldConvergenceFixture(ptahv1alpha1.OperationObserve), check, common)
	})
	t.Run("convergence Plan", func(t *testing.T) {
		t.Parallel()
		proofRefusesEach(t, heldConvergenceFixture(ptahv1alpha1.OperationPlan), check, common)
	})
	t.Run("uncertain Observe", func(t *testing.T) {
		t.Parallel()
		proofRefusesEach(t, heldUncertainFixture(ptahv1alpha1.OperationObserve, false), check, append(slices.Clone(uncertain),
			proofCase[heldFixture]{"no Pod evidence where it is required", func(f *heldFixture) {
				f.schema.Status.PendingObservation.ApplyPodUIDs = nil
				f.schema.Status.PendingObservation.ApplyPodCount = 0
			}}))
	})
	t.Run("uncertain Plan with optional Pod evidence", func(t *testing.T) {
		t.Parallel()
		proofRefusesEach(t, heldUncertainFixture(ptahv1alpha1.OperationPlan, true), check, append(slices.Clone(uncertain),
			proofCase[heldFixture]{"no Pods counted as one", func(f *heldFixture) {
				f.schema.Status.PendingObservation.ApplyPodUIDs = nil
			}}))
	})
	t.Run("optional Pod evidence accepts none", func(t *testing.T) {
		t.Parallel()
		fixture := heldUncertainFixture(ptahv1alpha1.OperationObserve, true)()
		fixture.schema.Status.PendingObservation.ApplyPodUIDs = nil
		fixture.schema.Status.PendingObservation.ApplyPodCount = 0
		if err := fixture.proof.check(fixture.schema); err != nil {
			t.Fatalf("no Pod evidence was refused where it is optional: %v", err)
		}
	})
	t.Run("the runner-termination proof reads no plan binding", func(t *testing.T) {
		t.Parallel()
		fixture := heldUncertainFixture(ptahv1alpha1.OperationObserve, false)()
		fixture.proof.applyJob, fixture.proof.controller = nil, nil
		fixture.schema.Status.ExecutionBinding = nil
		fixture.schema.Status.PendingObservation.ApplyJobName = ""
		if err := fixture.proof.check(fixture.schema); err != nil {
			t.Fatalf("a proof without a controller or an Apply Job read them anyway: %v", err)
		}
	})
}

// harvestFixture is the schema watch, and the result, of one successful
// Apply harvested into a pending observation.
type harvestFixture struct {
	events         []watchEvent[*ptahv1alpha1.PtahSchema]
	apply, harvest *ptahv1alpha1.PtahSchema
	result         runner.Result
	want           harvestedApply
}

func buildHarvestFixture() *harvestFixture {
	plan := proofPlan()
	apply := proofSchema(proofSchemaName, ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseApplying, ExecutionBinding: proofBinding(), Plan: &plan,
		ActiveOperation: proofApplyActive(),
	})
	harvest := proofSchema(proofSchemaName, ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseVerifyingConvergence, ExecutionBinding: proofBinding(),
		PendingObservation: proofPending(ptahv1alpha1.PendingObservationApplySucceeded),
		Conditions:         proofVerifyingConditions(),
	})
	other := proofSchema("another-schema", *harvest.Status.DeepCopy())
	other.Status.PendingObservation.ApplyPodCount = 9
	return &harvestFixture{
		events: []watchEvent[*ptahv1alpha1.PtahSchema]{
			{Type: watch.Bookmark, Object: &ptahv1alpha1.PtahSchema{}},
			{Type: watch.Modified, Object: apply},
			{Type: watch.Modified, Object: other},
			{Type: watch.Modified, Object: harvest},
		},
		apply: apply, harvest: harvest,
		result: runner.Result{
			OperationID: proofApplyID, MutationStarted: true, PlanContentDigest: proofDigest("c"),
			CoordinationDigest: proofDigest("d"), TargetIdentityDigest: proofDigest("e"),
		},
		want: harvestedApply{
			schema: proofSchemaName, operationID: proofApplyID, jobUID: proofApplyJobUID, podUID: proofApplyPodUID,
			controller: proofController, stateVersion: proofStateVersion,
		},
	}
}

func TestSuccessfulApplyHarvestedHoldsTheApplyAndItsHarvest(t *testing.T) {
	t.Parallel()
	pending := func(f *harvestFixture) *ptahv1alpha1.PendingObservationStatus {
		return f.harvest.Status.PendingObservation
	}
	proofRefusesEach(t, buildHarvestFixture, func(f *harvestFixture) error {
		return successfulApplyHarvested(f.events, f.want, f.result)
	}, []proofCase[harvestFixture]{
		{"only a bookmark", func(f *harvestFixture) { f.events = f.events[:1] }},
		{"an Apply without a plan", func(f *harvestFixture) { f.apply.Status.Plan = nil }},
		{"an Apply on another Job", func(f *harvestFixture) { f.apply.Status.ActiveOperation.JobUID = "other" }},
		{"another schema's name", func(f *harvestFixture) { f.want.schema = "another-schema" }},
		{"no harvest", func(f *harvestFixture) { pending(f).Outcome = ptahv1alpha1.PendingObservationOutcomeUnknown }},
		{"a harvest of another Job", func(f *harvestFixture) { pending(f).ApplyJobUID = "other" }},
		{"no execution binding", func(f *harvestFixture) { f.apply.Status.ExecutionBinding = nil }},
		{"an invalid binding epoch", func(f *harvestFixture) { f.apply.Status.ExecutionBinding.Epoch = "v1-short" }},
		{"another controller state", func(f *harvestFixture) { f.apply.Status.ExecutionBinding.ControllerStateVersion = 3 }},
		{"an Apply under another binding", func(f *harvestFixture) { f.apply.Status.ActiveOperation.ExecutionBindingID = proofOtherEpoch }},
		{"a plan under another binding", func(f *harvestFixture) { f.apply.Status.Plan.ExecutionBindingID = proofOtherEpoch }},
		{"a plan of another image", func(f *harvestFixture) { f.apply.Status.Plan.ControllerImage = "other" }},
		{"a plan of another revision", func(f *harvestFixture) { f.apply.Status.Plan.ControllerRevision = "other" }},
		{"a plan of another state", func(f *harvestFixture) { f.apply.Status.Plan.ControllerStateVersion = 3 }},
		{"a harvest under another binding", func(f *harvestFixture) { f.harvest.Status.ExecutionBinding.PtahVersion = "v9" }},
		{"no dispatch deadline", func(f *harvestFixture) { f.apply.Status.ActiveOperation.DispatchNotAfter = nil }},
		{"no execution deadline", func(f *harvestFixture) { f.apply.Status.ActiveOperation.ExecutionNotAfter = nil }},
		{"an execution deadline apart", func(f *harvestFixture) {
			f.apply.Status.ActiveOperation.ExecutionNotAfter = proofTimePointer(601)
		}},
		{"another grace period", func(f *harvestFixture) { f.apply.Status.ActiveOperation.TerminationGracePeriodSeconds = 29 }},
		{"a result of another operation", func(f *harvestFixture) { f.result.OperationID = "other" }},
		{"a result with an error", func(f *harvestFixture) { f.result.Error = &runner.ResultError{Code: "apply_failed"} }},
		{"a nonzero exit", func(f *harvestFixture) { f.result.ChildExitCode = 1 }},
		{"no mutation", func(f *harvestFixture) { f.result.MutationStarted = false }},
		{"an uncertain result", func(f *harvestFixture) { f.result.Uncertain = true }},
		{"a plan outcome", func(f *harvestFixture) { f.result.PlanOutcome = runner.PlanOutcomeChanges }},
		{"a truncated result", func(f *harvestFixture) { f.result.Truncation = &runner.TruncationMetadata{Stdout: true} }},
		{"other content", func(f *harvestFixture) { f.result.PlanContentDigest = proofDigest("0") }},
		{"no content", func(f *harvestFixture) {
			f.result.PlanContentDigest, f.apply.Status.Plan.ContentDigest, pending(f).Plan.ContentDigest = "", "", ""
		}},
		{"another realm than the Apply", func(f *harvestFixture) { f.apply.Status.ActiveOperation.CoordinationDigest = proofDigest("0") }},
		{"another realm than the plan", func(f *harvestFixture) {
			f.result.CoordinationDigest = proofDigest("0")
			f.apply.Status.ActiveOperation.CoordinationDigest = proofDigest("0")
		}},
		{"another target than the Apply", func(f *harvestFixture) { f.apply.Status.ActiveOperation.TargetIdentityDigest = proofDigest("0") }},
		{"another target than the plan", func(f *harvestFixture) {
			f.result.TargetIdentityDigest = proofDigest("0")
			f.apply.Status.ActiveOperation.TargetIdentityDigest = proofDigest("0")
		}},
		{"a harvest of another plan", func(f *harvestFixture) { pending(f).Plan.StatementCount = 3 }},
		{"a harvest of another Apply Job name", func(f *harvestFixture) { pending(f).ApplyJobName = "other" }},
		{"a harvest of no Apply Pod", func(f *harvestFixture) { pending(f).ApplyPodUIDs, pending(f).ApplyPodCount = nil, 0 }},
		{"a harvest of another Apply Pod", func(f *harvestFixture) { pending(f).ApplyPodUIDs = []types.UID{"other"} }},
		{"a harvest counting two Pods", func(f *harvestFixture) { pending(f).ApplyPodCount = 2 }},
		{"a harvest in another realm", func(f *harvestFixture) { pending(f).CoordinationDigest = proofDigest("0") }},
		{"a harvest at another epoch", func(f *harvestFixture) { pending(f).LeaseEpoch = proofOtherEpoch }},
		{"a harvest in another phase", func(f *harvestFixture) { f.harvest.Status.Phase = ptahv1alpha1.PhaseInSync }},
		{"a harvest that applied", func(f *harvestFixture) { f.harvest.Status.Applied = &ptahv1alpha1.AppliedStatus{} }},
		{"a harvest releasing its lock", func(f *harvestFixture) {
			f.harvest.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		}},
		{"a harvest already Ready", func(f *harvestFixture) { f.harvest.Status.Conditions[1].Status = metav1.ConditionTrue }},
		{"a harvest with no conditions", func(f *harvestFixture) { f.harvest.Status.Conditions = nil }},
		{"an earlier Apply document bound elsewhere", func(f *harvestFixture) {
			earlier := f.apply.DeepCopy()
			earlier.Status.ActiveOperation.ExecutionBindingID = proofOtherEpoch
			f.events = slices.Insert(f.events, 1, watchEvent[*ptahv1alpha1.PtahSchema]{Type: watch.Added, Object: earlier})
		}},
	})
}

// resultFixture is a schema and the Observe and Plan results read for it.
type resultFixture struct {
	schema        *ptahv1alpha1.PtahSchema
	observe, plan runner.Result
}

func buildConvergedFixture() *resultFixture {
	return &resultFixture{
		schema: proofSchema(proofSchemaName, ptahv1alpha1.PtahSchemaStatus{
			Phase: ptahv1alpha1.PhaseInSync, ExecutionBinding: proofBinding(), Target: proofTarget(),
			Applied: &ptahv1alpha1.AppliedStatus{
				ExecutionBindingID: proofEpoch, ControllerImage: proofController.image,
				ControllerRevision: proofController.revision, ControllerStateVersion: proofStateVersion,
			},
			Conditions: []metav1.Condition{{
				Type: ptahv1alpha1.ConditionInSync, Status: metav1.ConditionTrue, Reason: "ScopedConverged",
			}},
		}),
		observe: runner.Result{
			ObservedDialect: "postgresql", CoordinationDigest: proofDigest("d"), TargetIdentityDigest: proofDigest("e"),
			DriftReportDigest: proofDigest("7"),
		},
		plan: runner.Result{
			PlanOutcome: runner.PlanOutcomeNoChanges, CoordinationDigest: proofDigest("d"),
			TargetIdentityDigest: proofDigest("e"),
		},
	}
}

// resultCases are the refusals a clean Observe and a NoChanges Plan owe.
func resultCases() []proofCase[resultFixture] {
	return []proofCase[resultFixture]{
		{"an Observe error", func(f *resultFixture) { f.observe.Error = &runner.ResultError{Code: "observe_failed"} }},
		{"an Observe exit", func(f *resultFixture) { f.observe.ChildExitCode = 1 }},
		{"Observe output", func(f *resultFixture) { f.observe.Stdout = "{}" }},
		{"observed drift", func(f *resultFixture) { f.observe.ObservedDrift = true }},
		{"a drift severity", func(f *resultFixture) { f.observe.HighestDriftSeverity = "info" }},
		{"drift findings", func(f *resultFixture) { f.observe.DriftFindingCount = 1 }},
		{"a MySQL dialect", func(f *resultFixture) { f.observe.ObservedDialect = "mysql" }},
		{"no dialect", func(f *resultFixture) { f.observe.ObservedDialect = "" }},
		{"an Observe of another realm", func(f *resultFixture) { f.observe.CoordinationDigest = proofDigest("0") }},
		{"an Observe of another target", func(f *resultFixture) { f.observe.TargetIdentityDigest = proofDigest("0") }},
		{"an Observe of another drift report", func(f *resultFixture) { f.observe.DriftReportDigest = proofDigest("0") }},
		{"a Plan error", func(f *resultFixture) { f.plan.Error = &runner.ResultError{Code: "plan_failed"} }},
		{"a Plan exit", func(f *resultFixture) { f.plan.ChildExitCode = 1 }},
		{"a Plan with changes", func(f *resultFixture) { f.plan.PlanOutcome = runner.PlanOutcomeChanges }},
		{"Plan output", func(f *resultFixture) { f.plan.Stdout = "sealed" }},
		{"a Plan content digest", func(f *resultFixture) { f.plan.PlanContentDigest = proofDigest("c") }},
		{"a Plan of another realm", func(f *resultFixture) { f.plan.CoordinationDigest = proofDigest("0") }},
		{"a Plan of another target", func(f *resultFixture) { f.plan.TargetIdentityDigest = proofDigest("0") }},
	}
}

func TestConvergedAfterApplyHoldsTheAppliedEvidenceAndTheResults(t *testing.T) {
	t.Parallel()
	check := func(f *resultFixture) error {
		return convergedAfterApply(f.schema, f.observe, f.plan, proofController, proofStateVersion)
	}
	proofRefusesEach(t, buildConvergedFixture, check, append(resultCases(),
		proofCase[resultFixture]{"another phase", func(f *resultFixture) { f.schema.Status.Phase = ptahv1alpha1.PhaseVerifyingConvergence }},
		proofCase[resultFixture]{"a pending observation", func(f *resultFixture) {
			f.schema.Status.PendingObservation = proofPending(ptahv1alpha1.PendingObservationApplySucceeded)
		}},
		proofCase[resultFixture]{"an active operation", func(f *resultFixture) {
			f.schema.Status.ActiveOperation = proofActive(ptahv1alpha1.OperationObserve, "id", "uid")
		}},
		proofCase[resultFixture]{"a plan", func(f *resultFixture) { f.schema.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{} }},
		proofCase[resultFixture]{"nothing applied", func(f *resultFixture) { f.schema.Status.Applied = nil }},
		proofCase[resultFixture]{"no execution binding", func(f *resultFixture) { f.schema.Status.ExecutionBinding = nil }},
		proofCase[resultFixture]{"applied under another binding", func(f *resultFixture) {
			f.schema.Status.Applied.ExecutionBindingID = proofOtherEpoch
		}},
		proofCase[resultFixture]{"applied by another image", func(f *resultFixture) { f.schema.Status.Applied.ControllerImage = "other" }},
		proofCase[resultFixture]{"applied by another revision", func(f *resultFixture) { f.schema.Status.Applied.ControllerRevision = "other" }},
		proofCase[resultFixture]{"applied by another state", func(f *resultFixture) { f.schema.Status.Applied.ControllerStateVersion = 3 }},
		proofCase[resultFixture]{"InSync for another reason", func(f *resultFixture) {
			f.schema.Status.Conditions[0].Reason = "ConvergedAfterUnknownOutcome"
		}},
	))
	t.Run("either PostgreSQL dialect", func(t *testing.T) {
		t.Parallel()
		fixture := buildConvergedFixture()
		fixture.observe.ObservedDialect = "postgres"
		if err := check(fixture); err != nil {
			t.Fatalf("the postgres dialect was refused: %v", err)
		}
	})
	t.Run("a lock release is not this proof's claim", func(t *testing.T) {
		t.Parallel()
		fixture := buildConvergedFixture()
		fixture.schema.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		if err := check(fixture); err != nil {
			t.Fatalf("a pending lock release was refused though the shell never read it: %v", err)
		}
	})
}

func TestContenderRecoveredHoldsTheReadOnlyRecovery(t *testing.T) {
	t.Parallel()
	build := func() *resultFixture {
		fixture := buildConvergedFixture()
		fixture.schema.Status.Applied = nil
		fixture.schema.Status.Conditions = nil
		return fixture
	}
	proofRefusesEach(t, build, func(f *resultFixture) error {
		return contenderRecovered(f.schema, f.observe, f.plan)
	}, append(resultCases(),
		proofCase[resultFixture]{"another phase", func(f *resultFixture) { f.schema.Status.Phase = ptahv1alpha1.PhaseAwaitingApproval }},
		proofCase[resultFixture]{"an active operation", func(f *resultFixture) {
			f.schema.Status.ActiveOperation = proofActive(ptahv1alpha1.OperationObserve, "id", "uid")
		}},
		proofCase[resultFixture]{"a pending observation", func(f *resultFixture) {
			f.schema.Status.PendingObservation = proofPending(ptahv1alpha1.PendingObservationOutcomeUnknown)
		}},
		proofCase[resultFixture]{"a lock release", func(f *resultFixture) {
			f.schema.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		}},
		proofCase[resultFixture]{"an applied plan", func(f *resultFixture) { f.schema.Status.Applied = &ptahv1alpha1.AppliedStatus{} }},
		proofCase[resultFixture]{"a plan", func(f *resultFixture) { f.schema.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{} }},
	))
}

// principalFixture is the refused credential-principal schema, its Plan
// result, and the instant the wait reads it at.
type principalFixture struct {
	schema *ptahv1alpha1.PtahSchema
	result runner.Result
	now    time.Time
}

func buildPrincipalFixture() *principalFixture {
	schema := proofSchema("e2e-fault-pg-principal", ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseFailed,
		Source: ptahv1alpha1.SchemaSourceStatus{
			Digest: proofDigest("a"), Verified: true, ArtifactType: schemaArtifactType,
		},
		Target: proofTarget(),
		ActiveOperation: &ptahv1alpha1.ActiveOperationStatus{
			Type: ptahv1alpha1.OperationPlan, ID: proofPlanID, Attempt: 2,
		},
		NextReconciliationTime: proofTimePointer(3600),
		Conditions: []metav1.Condition{{
			Type: ptahv1alpha1.ConditionReconciliationFailed, Status: metav1.ConditionTrue, Reason: "OperationFailed",
		}},
	})
	schema.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: time.Hour}
	return &principalFixture{
		schema: schema,
		result: runner.Result{
			OperationID: proofPlanID, Error: &runner.ResultError{Code: "invalid_plan_output"},
			CoordinationDigest: proofDigest("d"), TargetIdentityDigest: proofDigest("e"),
		},
		now: proofTime(0).Time,
	}
}

func TestPrincipalFailedClosedWaitsForTheLongBarrier(t *testing.T) {
	t.Parallel()
	proofRefusesEach(t, buildPrincipalFixture, func(f *principalFixture) error {
		return proofBool(principalFailedClosed(f.schema, f.now))
	}, []proofCase[principalFixture]{
		{"a short retry interval", func(f *principalFixture) {
			f.schema.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: 5 * time.Second}
		}},
		{"another phase", func(f *principalFixture) { f.schema.Status.Phase = ptahv1alpha1.PhasePlanning }},
		{"no active operation", func(f *principalFixture) { f.schema.Status.ActiveOperation = nil }},
		{"another operation", func(f *principalFixture) { f.schema.Status.ActiveOperation.Type = ptahv1alpha1.OperationObserve }},
		{"the first attempt", func(f *principalFixture) { f.schema.Status.ActiveOperation.Attempt = 1 }},
		{"a Job bound", func(f *principalFixture) { f.schema.Status.ActiveOperation.JobUID = "plan-job" }},
		{"no failure condition", func(f *principalFixture) { f.schema.Status.Conditions = nil }},
		{"a failure for another reason", func(f *principalFixture) { f.schema.Status.Conditions[0].Reason = "ConfigurationError" }},
		{"no next reconciliation", func(f *principalFixture) { f.schema.Status.NextReconciliationTime = nil }},
		{"a retry exactly 3500 seconds out", func(f *principalFixture) {
			f.schema.Status.NextReconciliationTime = proofTimePointer(3500)
		}},
		{"a retry due sooner", func(f *principalFixture) { f.schema.Status.NextReconciliationTime = proofTimePointer(60) }},
	})
}

func TestPrincipalRefusalIsTheInvalidPlanOutputRefusal(t *testing.T) {
	t.Parallel()
	proofRefusesEach(t, buildPrincipalFixture, func(f *principalFixture) error {
		return principalRefusal(f.schema, f.result, proofDigest("a"))
	}, []proofCase[principalFixture]{
		{"another artifact", func(f *principalFixture) { f.schema.Status.Source.Digest = proofDigest("0") }},
		{"no artifact", func(f *principalFixture) { f.schema.Status.Source.Digest = "" }},
		{"an unverified artifact", func(f *principalFixture) { f.schema.Status.Source.Verified = false }},
		{"another artifact type", func(f *principalFixture) { f.schema.Status.Source.ArtifactType = "application/octet-stream" }},
		{"no drift report", func(f *principalFixture) { f.schema.Status.Target.DriftReportDigest = "" }},
		{"a published plan", func(f *principalFixture) { f.schema.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{} }},
		{"no active operation", func(f *principalFixture) { f.schema.Status.ActiveOperation = nil }},
		{"a result of another operation", func(f *principalFixture) { f.result.OperationID = "other" }},
		{"a nonzero exit", func(f *principalFixture) { f.result.ChildExitCode = 1 }},
		{"output", func(f *principalFixture) { f.result.Stdout = "plan" }},
		{"a content digest", func(f *principalFixture) { f.result.PlanContentDigest = proofDigest("c") }},
		{"a plan outcome", func(f *principalFixture) { f.result.PlanOutcome = runner.PlanOutcomeChanges }},
		{"a mutation", func(f *principalFixture) { f.result.MutationStarted = true }},
		{"uncertainty", func(f *principalFixture) { f.result.Uncertain = true }},
		{"truncation", func(f *principalFixture) { f.result.Truncation = &runner.TruncationMetadata{Stdout: true} }},
		{"no error", func(f *principalFixture) { f.result.Error = nil }},
		{"another error", func(f *principalFixture) { f.result.Error.Code = "plan_failed" }},
		{"another realm", func(f *principalFixture) { f.result.CoordinationDigest = proofDigest("0") }},
		{"another target", func(f *principalFixture) { f.result.TargetIdentityDigest = proofDigest("0") }},
	})
}

func TestPrincipalSuspendedIsIdleByRequest(t *testing.T) {
	t.Parallel()
	build := func() *principalFixture {
		fixture := buildPrincipalFixture()
		fixture.schema.Spec.Suspend = true
		fixture.schema.Status.Phase = ptahv1alpha1.PhaseSuspended
		fixture.schema.Status.ActiveOperation = nil
		fixture.schema.Status.Conditions = []metav1.Condition{{
			Type: ptahv1alpha1.ConditionSuspended, Status: metav1.ConditionTrue, Reason: "Requested",
		}}
		return fixture
	}
	proofRefusesEach(t, build, func(f *principalFixture) error {
		return proofBool(principalSuspended(f.schema))
	}, []proofCase[principalFixture]{
		{"not suspended", func(f *principalFixture) { f.schema.Spec.Suspend = false }},
		{"another phase", func(f *principalFixture) { f.schema.Status.Phase = ptahv1alpha1.PhaseFailed }},
		{"an active operation", func(f *principalFixture) {
			f.schema.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationPlan}
		}},
		{"a pending observation", func(f *principalFixture) {
			f.schema.Status.PendingObservation = proofPending(ptahv1alpha1.PendingObservationApplySucceeded)
		}},
		{"no Suspended condition", func(f *principalFixture) { f.schema.Status.Conditions = nil }},
		{"suspended for another reason", func(f *principalFixture) { f.schema.Status.Conditions[0].Reason = "Other" }},
	})
}

func TestStalePlanResultIsTheConservativeRefusal(t *testing.T) {
	t.Parallel()
	build := func() *runner.Result {
		return &runner.Result{
			ChildExitCode: 2, PlanContentDigest: proofDigest("c"), CoordinationDigest: proofDigest("d"),
			TargetIdentityDigest: proofDigest("e"), MutationStarted: true, Uncertain: true,
			Error: &runner.ResultError{Code: "stale_plan"},
		}
	}
	proofRefusesEach(t, build, func(r *runner.Result) error {
		return stalePlanResult(*r, proofDigest("c"), proofDigest("d"), proofDigest("e"))
	}, []proofCase[runner.Result]{
		{"exit zero", func(r *runner.Result) { r.ChildExitCode = 0 }},
		{"output", func(r *runner.Result) { r.Stdout = "x" }},
		{"other content", func(r *runner.Result) { r.PlanContentDigest = proofDigest("0") }},
		{"no content", func(r *runner.Result) { r.PlanContentDigest = "" }},
		{"another realm", func(r *runner.Result) { r.CoordinationDigest = proofDigest("0") }},
		{"another target", func(r *runner.Result) { r.TargetIdentityDigest = proofDigest("0") }},
		{"no mutation", func(r *runner.Result) { r.MutationStarted = false }},
		{"a certain outcome", func(r *runner.Result) { r.Uncertain = false }},
		{"a plan outcome", func(r *runner.Result) { r.PlanOutcome = runner.PlanOutcomeNoChanges }},
		{"truncation", func(r *runner.Result) { r.Truncation = &runner.TruncationMetadata{Stderr: true} }},
		{"no error", func(r *runner.Result) { r.Error = nil }},
		{"another error", func(r *runner.Result) { r.Error.Code = "apply_failed" }},
	})
}

func TestDriftedObserveBoundFindsDriftOnTheTarget(t *testing.T) {
	t.Parallel()
	build := func() *resultFixture {
		fixture := buildConvergedFixture()
		fixture.observe.ObservedDrift, fixture.observe.DriftFindingCount = true, 2
		fixture.observe.ChildExitCode = 1
		fixture.observe.ObservedDialect = "mariadb"
		return fixture
	}
	proofRefusesEach(t, build, func(f *resultFixture) error {
		return driftedObserveBound(f.schema, f.observe, "mysql", "mariadb")
	}, []proofCase[resultFixture]{
		{"an error", func(f *resultFixture) { f.observe.Error = &runner.ResultError{Code: "observe_failed"} }},
		{"output", func(f *resultFixture) { f.observe.Stdout = "x" }},
		{"exit two", func(f *resultFixture) { f.observe.ChildExitCode = 2 }},
		{"no drift", func(f *resultFixture) { f.observe.ObservedDrift = false }},
		{"no findings", func(f *resultFixture) { f.observe.DriftFindingCount = 0 }},
		{"another dialect", func(f *resultFixture) { f.observe.ObservedDialect = "postgresql" }},
		{"another realm", func(f *resultFixture) { f.observe.CoordinationDigest = proofDigest("0") }},
		{"another target", func(f *resultFixture) { f.observe.TargetIdentityDigest = proofDigest("0") }},
		{"another drift report", func(f *resultFixture) { f.observe.DriftReportDigest = proofDigest("0") }},
	})
	t.Run("exit zero", func(t *testing.T) {
		t.Parallel()
		fixture := build()
		fixture.observe.ChildExitCode = 0
		if err := driftedObserveBound(fixture.schema, fixture.observe, "mysql", "mariadb"); err != nil {
			t.Fatalf("a drifted Observe that exited zero was refused: %v", err)
		}
	})
}

func TestObservationAdvancedReadsAfterTheFault(t *testing.T) {
	t.Parallel()
	build := func() *resultFixture {
		fixture := buildConvergedFixture()
		plan := proofPlan()
		fixture.schema.Status.Plan = &plan
		return fixture
	}
	before := proofTime(119).Time
	proofRefusesEach(t, build, func(f *resultFixture) error {
		return observationAdvanced(f.schema, before)
	}, []proofCase[resultFixture]{
		{"never observed", func(f *resultFixture) { f.schema.Status.Target.LastObservedAt = nil }},
		{"observed at the same instant", func(f *resultFixture) { f.schema.Status.Target.LastObservedAt = proofTimePointer(119) }},
		{"observed before", func(f *resultFixture) { f.schema.Status.Target.LastObservedAt = proofTimePointer(10) }},
		{"an approved plan", func(f *resultFixture) {
			f.schema.Status.Plan.Approval = &ptahv1alpha1.ConsumedApprovalStatus{Name: "approval"}
		}},
		{"no drift report", func(f *resultFixture) { f.schema.Status.Target.DriftReportDigest = "" }},
	})
	t.Run("no plan at all", func(t *testing.T) {
		t.Parallel()
		fixture := build()
		fixture.schema.Status.Plan = nil
		if err := observationAdvanced(fixture.schema, before); err != nil {
			t.Fatalf("a schema with no plan was refused: %v", err)
		}
	})
}

// freshPlanFixture is a schema, its fresh plan, the Plan result and the
// document rebuilt from the plan's chunks.
type freshPlanFixture struct {
	schema   *ptahv1alpha1.PtahSchema
	plan     *ptahv1alpha1.PtahSchemaPlan
	result   runner.Result
	document planDocument
}

func buildFreshPlanFixture() *freshPlanFixture {
	current := proofPlan()
	return &freshPlanFixture{
		schema: proofSchema(proofSchemaName, ptahv1alpha1.PtahSchemaStatus{Plan: &current}),
		plan: &ptahv1alpha1.PtahSchemaPlan{
			ObjectMeta: metav1.ObjectMeta{Name: current.Name, UID: current.UID},
			Spec: ptahv1alpha1.PtahSchemaPlanSpec{
				ContentDigest: proofDigest("c"), CoordinationDigest: proofDigest("d"), TargetIdentityDigest: proofDigest("e"),
				ActualStateFingerprint: proofDigest("1"), DesiredStateFingerprint: proofDigest("2"),
			},
		},
		result: runner.Result{
			PlanOutcome: runner.PlanOutcomeChanges, PlanContentDigest: proofDigest("c"), Stdout: "sealed",
			CoordinationDigest: proofDigest("d"), TargetIdentityDigest: proofDigest("e"),
		},
		document: planDocument{FromFingerprint: proofDigest("1"), ToFingerprint: proofDigest("2")},
	}
}

func TestFreshPlanBoundHoldsOneContentDigest(t *testing.T) {
	t.Parallel()
	proofRefusesEach(t, buildFreshPlanFixture, func(f *freshPlanFixture) error {
		return freshPlanBound(f.schema, f.plan, f.result, f.document, proofDigest("c"))
	}, []proofCase[freshPlanFixture]{
		{"an error", func(f *freshPlanFixture) { f.result.Error = &runner.ResultError{Code: "plan_failed"} }},
		{"a nonzero exit", func(f *freshPlanFixture) { f.result.ChildExitCode = 1 }},
		{"no changes", func(f *freshPlanFixture) { f.result.PlanOutcome = runner.PlanOutcomeNoChanges }},
		{"a result of other content", func(f *freshPlanFixture) { f.result.PlanContentDigest = proofDigest("0") }},
		{"no current plan", func(f *freshPlanFixture) { f.schema.Status.Plan = nil }},
		{"a current plan of other content", func(f *freshPlanFixture) { f.schema.Status.Plan.ContentDigest = proofDigest("0") }},
		{"a plan of other content", func(f *freshPlanFixture) { f.plan.Spec.ContentDigest = proofDigest("0") }},
		{"a result in another realm", func(f *freshPlanFixture) { f.result.CoordinationDigest = proofDigest("0") }},
		{"a result without a realm", func(f *freshPlanFixture) {
			f.result.CoordinationDigest, f.plan.Spec.CoordinationDigest = "", ""
		}},
		{"a result of another target", func(f *freshPlanFixture) { f.result.TargetIdentityDigest = proofDigest("0") }},
		{"a plan from another actual state", func(f *freshPlanFixture) { f.plan.Spec.ActualStateFingerprint = proofDigest("0") }},
		{"a document without its actual state", func(f *freshPlanFixture) {
			f.document.FromFingerprint, f.plan.Spec.ActualStateFingerprint = "", ""
		}},
		{"a plan toward another desired state", func(f *freshPlanFixture) {
			f.plan.Spec.DesiredStateFingerprint = proofDigest("0")
		}},
		{"a document toward another desired state", func(f *freshPlanFixture) { f.document.ToFingerprint = proofDigest("0") }},
	})
}

// manualDriftFixture is the manual-drift schema, its old approval, and the
// fresh plan.
type manualDriftFixture struct {
	schema   *ptahv1alpha1.PtahSchema
	approval *ptahv1alpha1.PtahSchemaApproval
	plan     *ptahv1alpha1.PtahSchemaPlan
}

func buildManualDriftFixture() *manualDriftFixture {
	current := proofPlan()
	current.UID = "plan-uid-fresh"
	return &manualDriftFixture{
		schema: proofSchema("e2e-fault-pg-manual", ptahv1alpha1.PtahSchemaStatus{
			Phase: ptahv1alpha1.PhaseAwaitingApproval, Plan: &current,
		}),
		approval: &ptahv1alpha1.PtahSchemaApproval{Status: ptahv1alpha1.PtahSchemaApprovalStatus{
			Conditions: []metav1.Condition{
				{Type: "Consumed", Status: metav1.ConditionTrue, Reason: "DispatchCommitted"},
				{Type: "Stale", Status: metav1.ConditionTrue, Reason: "PlanNoLongerCurrent"},
			},
		}},
		plan: &ptahv1alpha1.PtahSchemaPlan{
			ObjectMeta: metav1.ObjectMeta{UID: "plan-uid-fresh"},
			Spec:       ptahv1alpha1.PtahSchemaPlanSpec{ActualStateFingerprint: proofDigest("5")},
		},
	}
}

func TestManualDriftSettledOnAFreshUnapprovedPlan(t *testing.T) {
	t.Parallel()
	proofRefusesEach(t, buildManualDriftFixture, func(f *manualDriftFixture) error {
		return proofBool(manualDriftSettled(f.schema, f.approval, "plan-uid-a"))
	}, []proofCase[manualDriftFixture]{
		{"no plan", func(f *manualDriftFixture) { f.schema.Status.Plan = nil }},
		{"a plan without a name", func(f *manualDriftFixture) { f.schema.Status.Plan.Name = "" }},
		{"a pending observation", func(f *manualDriftFixture) {
			f.schema.Status.PendingObservation = proofPending(ptahv1alpha1.PendingObservationOutcomeUnknown)
		}},
		{"an active operation", func(f *manualDriftFixture) {
			f.schema.Status.ActiveOperation = proofActive(ptahv1alpha1.OperationPlan, "id", "uid")
		}},
		{"a lock release", func(f *manualDriftFixture) {
			f.schema.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		}},
		{"an applied plan", func(f *manualDriftFixture) { f.schema.Status.Applied = &ptahv1alpha1.AppliedStatus{} }},
		{"another phase", func(f *manualDriftFixture) { f.schema.Status.Phase = ptahv1alpha1.PhaseVerifyingConvergence }},
		{"the old plan", func(f *manualDriftFixture) { f.schema.Status.Plan.UID = "plan-uid-a" }},
		{"an approved plan", func(f *manualDriftFixture) {
			f.schema.Status.Plan.Approval = &ptahv1alpha1.ConsumedApprovalStatus{Name: "approval"}
		}},
		{"an approval not consumed", func(f *manualDriftFixture) { f.approval.Status.Conditions[0].Status = metav1.ConditionFalse }},
		{"an approval not stale", func(f *manualDriftFixture) { f.approval.Status.Conditions[1].Reason = "Other" }},
	})
	proofRefusesEach(t, buildManualDriftFixture, func(f *manualDriftFixture) error {
		return proofBool(manualFreshPlan(f.plan, "plan-uid-a", proofDigest("1")))
	}, []proofCase[manualDriftFixture]{
		{"the old plan UID", func(f *manualDriftFixture) { f.plan.UID = "plan-uid-a" }},
		{"the old actual state", func(f *manualDriftFixture) { f.plan.Spec.ActualStateFingerprint = proofDigest("1") }},
		{"no actual state", func(f *manualDriftFixture) { f.plan.Spec.ActualStateFingerprint = "" }},
	})
}

// historyFixture is the schema, Job and Lease watches of one Apply and its
// proof Observe and Plan, and the documents in them a mutation edits.
type historyFixture struct {
	schemas []watchEvent[*ptahv1alpha1.PtahSchema]
	jobs    []watchEvent[*batchv1.Job]
	leases  []watchEvent[*coordinationv1.Lease]

	apply, origin, observe, plan, release, final *ptahv1alpha1.PtahSchema
	fresh                                        *ptahv1alpha1.PtahSchemaPlan

	post      postApplyProof
	uncertain uncertainProof
}

// buildHistory is the common history: the Apply, a harvest of the outcome
// given, an Observe and a Plan proof of that harvest, the lock release, and a
// last document; the proof Jobs in order; the Lease held once and released.
func buildHistory(outcome ptahv1alpha1.PendingObservationOutcome) *historyFixture {
	plan := proofPlan()
	apply := proofSchema(proofSchemaName, ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseApplying, ExecutionBinding: proofBinding(), Plan: &plan,
		ActiveOperation: proofApplyActive(),
	})
	verifying := func(active *ptahv1alpha1.ActiveOperationStatus, planRequired bool) *ptahv1alpha1.PtahSchema {
		pending := proofPending(outcome)
		pending.PlanRequired = planRequired
		return proofSchema(proofSchemaName, ptahv1alpha1.PtahSchemaStatus{
			Phase: ptahv1alpha1.PhaseVerifyingConvergence, ExecutionBinding: proofBinding(),
			PendingObservation: pending, ActiveOperation: active, Conditions: proofVerifyingConditions(),
		})
	}
	origin := verifying(nil, false)
	observe := verifying(proofActive(ptahv1alpha1.OperationObserve, proofObserveID, proofObserveUID), false)
	planning := verifying(proofActive(ptahv1alpha1.OperationPlan, proofPlanID, proofPlanUID), true)
	release := proofSchema(proofSchemaName, ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseVerifyingConvergence, ExecutionBinding: proofBinding(),
		PendingLockRelease: &ptahv1alpha1.TargetLockReleaseStatus{
			CoordinationDigest: proofDigest("d"), OperationID: proofApplyID, LeaseDurationSeconds: 30,
			LeaseEpoch: proofLeaseEpoch,
		},
		Conditions: proofVerifyingConditions(),
	})
	return &historyFixture{
		schemas: []watchEvent[*ptahv1alpha1.PtahSchema]{
			{Type: watch.Bookmark, Object: &ptahv1alpha1.PtahSchema{}},
			{Type: watch.Modified, Object: apply},
			{Type: watch.Modified, Object: origin},
			{Type: watch.Modified, Object: observe},
			{Type: watch.Modified, Object: planning},
			{Type: watch.Modified, Object: release},
		},
		jobs: []watchEvent[*batchv1.Job]{
			{Type: watch.Bookmark, Object: &batchv1.Job{}},
			{Type: watch.Added, Object: proofJob(proofObserveUID, proofObserveID, false)},
			{Type: watch.Modified, Object: proofJob(proofObserveUID, proofObserveID, true)},
			{Type: watch.Added, Object: proofJob(proofPlanUID, proofPlanID, false)},
			{Type: watch.Modified, Object: proofJob(proofPlanUID, proofPlanID, true)},
		},
		leases: []watchEvent[*coordinationv1.Lease]{
			{Type: watch.Bookmark, Object: &coordinationv1.Lease{}},
			{Type: watch.Added, Object: proofLease(proofLeaseUID, proofLeaseHolder, proofLeaseEpoch)},
			{Type: watch.Modified, Object: proofLease("other-lease-uid", "", proofOtherEpoch)},
			{Type: watch.Modified, Object: proofLease(proofLeaseUID, proofLeaseHolder, proofLeaseEpoch)},
			{Type: watch.Modified, Object: proofLease(proofLeaseUID, "", proofLeaseEpoch)},
		},
		apply: apply, origin: origin, observe: observe, plan: planning, release: release,
	}
}

// buildPostApplyHistory adds the InSync document the shell required at the
// end of a successful Apply's proof.
func buildPostApplyHistory() *historyFixture {
	fixture := buildHistory(ptahv1alpha1.PendingObservationApplySucceeded)
	plan := proofPlan()
	converged := proofSchema(proofSchemaName, ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseInSync, ExecutionBinding: proofBinding(),
		Applied: &ptahv1alpha1.AppliedStatus{
			ArtifactDigest: plan.ArtifactDigest, PlanFingerprint: plan.Fingerprint,
			CoordinationDigest: plan.CoordinationDigest, TargetIdentityDigest: plan.TargetIdentityDigest,
			ExecutionBindingID: plan.ExecutionBindingID, ControllerImage: proofController.image,
			ControllerRevision: proofController.revision, ControllerStateVersion: proofStateVersion,
			PtahVersion: plan.PtahVersion, ExecutorImage: plan.ExecutorImage, RunnerImage: plan.RunnerImage,
			RunnerProtocolVersion: plan.RunnerProtocolVersion,
		},
		Conditions: []metav1.Condition{{
			Type: ptahv1alpha1.ConditionInSync, Status: metav1.ConditionTrue, Reason: "ScopedConverged",
		}},
	})
	fixture.schemas = append(fixture.schemas, watchEvent[*ptahv1alpha1.PtahSchema]{Type: watch.Modified, Object: converged})
	fixture.final = converged
	fixture.post = postApplyProof{
		schema: proofSchemaName, applyOperationID: proofApplyID, applyJobUID: proofApplyJobUID,
		lease: proofLeaseIdentity(), observeJobUID: proofObserveUID, planJobUID: proofPlanUID,
		controller: proofController, stateVersion: proofStateVersion,
	}
	return fixture
}

// historyCases are the refusals both history proofs owe: the documents they
// select, how the proof operations read the pending observation, the Jobs'
// order and the Lease's one acquisition.
func historyCases() []proofCase[historyFixture] {
	activeOf := func(schema *ptahv1alpha1.PtahSchema) *ptahv1alpha1.ActiveOperationStatus {
		return schema.Status.ActiveOperation
	}
	// Every document after the Apply carries the pending observation, so a
	// mutation that removes the record has to remove it from each: the jq
	// took the first document that matched, and a later one matches as well.
	everyPending := func(f *historyFixture, edit func(*ptahv1alpha1.PendingObservationStatus)) {
		for _, schema := range []*ptahv1alpha1.PtahSchema{f.origin, f.observe, f.plan} {
			edit(schema.Status.PendingObservation)
		}
	}
	cases := []proofCase[historyFixture]{
		{"no Apply at the epoch", func(f *historyFixture) { f.apply.Status.ActiveOperation.LeaseEpoch = proofOtherEpoch }},
		{"an Apply on another Job", func(f *historyFixture) { f.apply.Status.ActiveOperation.JobUID = "other" }},
		{"no record of the Apply", func(f *historyFixture) {
			everyPending(f, func(pending *ptahv1alpha1.PendingObservationStatus) { pending.ApplyJobUID = "other" })
		}},
		{"no record at the epoch", func(f *historyFixture) {
			everyPending(f, func(pending *ptahv1alpha1.PendingObservationStatus) { pending.LeaseEpoch = proofOtherEpoch })
		}},
		{"no Observe proof", func(f *historyFixture) { f.observe.Status.ActiveOperation.JobUID = "other" }},
		{"no required Plan proof", func(f *historyFixture) { f.plan.Status.PendingObservation.PlanRequired = false }},
		{"no lock release", func(f *historyFixture) { f.release.Status.PendingLockRelease.OperationID = "other" }},
		{"a lock release in another realm", func(f *historyFixture) {
			f.release.Status.PendingLockRelease.CoordinationDigest = proofDigest("0")
		}},
		{"a lock release with an observation open", func(f *historyFixture) {
			f.release.Status.PendingObservation = proofPending(ptahv1alpha1.PendingObservationApplySucceeded)
			f.release.Status.PendingObservation.ApplyOperationID = "unrelated"
		}},
		{"a lock release with an operation open", func(f *historyFixture) {
			f.release.Status.ActiveOperation = proofActive(ptahv1alpha1.OperationObserve, "later", "later-uid")
		}},
		{"an Apply plan bound elsewhere", func(f *historyFixture) { f.apply.Status.Plan.ExecutionBindingID = proofOtherEpoch }},
		{"an Apply under an invalid binding", func(f *historyFixture) { f.apply.Status.ExecutionBinding.Epoch = "v1-bad" }},
		{"a harvest of another controller state", func(f *historyFixture) {
			f.origin.Status.ExecutionBinding.ControllerStateVersion = 3
		}},
		{"an Apply Job name the harvest lacks", func(f *historyFixture) { f.apply.Status.ActiveOperation.JobName = "other" }},
		{"an Observe in another realm", func(f *historyFixture) { activeOf(f.observe).CoordinationDigest = proofDigest("0") }},
		{"an Observe without a realm", func(f *historyFixture) { activeOf(f.observe).CoordinationDigest = "" }},
		{"an Observe of another target identity", func(f *historyFixture) {
			activeOf(f.observe).TargetIdentityDigest = proofDigest("0")
		}},
		{"an Observe of another target binding", func(f *historyFixture) { activeOf(f.observe).Target.Engine = "MySQL" }},
		{"an Observe without a target binding", func(f *historyFixture) { activeOf(f.observe).Target = nil }},
		{"an Observe of another source", func(f *historyFixture) { activeOf(f.observe).Source.Digest = proofDigest("0") }},
		{"an Observe with a dev database", func(f *historyFixture) {
			activeOf(f.observe).ObservationDev = &ptahv1alpha1.DatabaseTargetRef{}
		}},
		{"an Observe with other exclusions", func(f *historyFixture) { activeOf(f.observe).ObservationExclude = nil }},
		{"an Observe of another severity", func(f *historyFixture) { activeOf(f.observe).ObservationSeverity = "error" }},
		{"an Observe with another connect timeout", func(f *historyFixture) {
			activeOf(f.observe).ObservationConnectTimeout = metav1.Duration{}
		}},
		{"an Observe with another lock timeout", func(f *historyFixture) {
			activeOf(f.observe).ObservationLockTimeout = metav1.Duration{Duration: time.Second}
		}},
		{"an Observe with no lease duration", func(f *historyFixture) { activeOf(f.observe).LeaseDurationSeconds = 0 }},
		{"an Observe at another epoch", func(f *historyFixture) { activeOf(f.observe).LeaseEpoch = proofOtherEpoch }},
		{"an Observe that lost lease continuity", func(f *historyFixture) { activeOf(f.observe).LeaseContinuityLost = true }},
		{"a Plan of another severity", func(f *historyFixture) { activeOf(f.plan).ObservationSeverity = "error" }},
		{"a proof document in another phase", func(f *historyFixture) { f.observe.Status.Phase = ptahv1alpha1.PhaseApplying }},
		{"a proof document that applied", func(f *historyFixture) { f.plan.Status.Applied = &ptahv1alpha1.AppliedStatus{} }},
		{"a proof document releasing its lock", func(f *historyFixture) {
			f.plan.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		}},
		{"a proof document already InSync", func(f *historyFixture) {
			f.observe.Status.Conditions[0].Status = metav1.ConditionTrue
		}},
		{"a proof document with no conditions", func(f *historyFixture) { f.origin.Status.Conditions = nil }},
		{"a proof document of another manager", func(f *historyFixture) {
			f.plan.Status.PendingObservation.Plan.ControllerRevision = "other"
		}},
		{"no Observe Job added", func(f *historyFixture) { f.jobs[1].Type = watch.Modified }},
		{"no Observe Job completed", func(f *historyFixture) { f.jobs[2].Object.Status.Conditions = nil }},
		{"no Plan Job added", func(f *historyFixture) { f.jobs[3].Type = watch.Modified }},
		{"no Plan Job completed", func(f *historyFixture) { f.jobs[4].Object.Status.Conditions = nil }},
		{"an Observe Job of another operation", func(f *historyFixture) {
			f.jobs[1].Object.Annotations[annotationOperationID] = "other"
		}},
		{"a Plan Job of another operation", func(f *historyFixture) {
			f.jobs[3].Object.Annotations[annotationOperationID] = "other"
		}},
		{"a Plan Job added before the Observe completed", func(f *historyFixture) {
			f.jobs[2], f.jobs[3] = f.jobs[3], f.jobs[2]
		}},
		{"proof operations without IDs", func(f *historyFixture) {
			activeOf(f.observe).ID = ""
			f.jobs[1].Object.Annotations[annotationOperationID] = ""
		}},
		{"a Lease never held by the holder", func(f *historyFixture) {
			f.leases[1].Object.Spec.HolderIdentity = ptr.To("manager-b_1")
			f.leases[3].Object.Spec.HolderIdentity = ptr.To("manager-b_1")
		}},
		{"a Lease held at another epoch", func(f *historyFixture) {
			f.leases[1].Object.Annotations[annotationLeaseEpoch] = proofOtherEpoch
			f.leases[3].Object.Annotations[annotationLeaseEpoch] = proofOtherEpoch
		}},
		{"a Lease never released", func(f *historyFixture) { f.leases = f.leases[:4] }},
		{"a Lease released by deletion", func(f *historyFixture) { f.leases[4].Type = watch.Deleted }},
		{"a Lease released at another epoch", func(f *historyFixture) {
			f.leases[4].Object.Annotations[annotationLeaseEpoch] = proofOtherEpoch
		}},
		{"a Lease that changed hands before its release", func(f *historyFixture) {
			f.leases[3].Object.Spec.HolderIdentity = ptr.To("manager-b_1")
		}},
		{"a Lease deleted while held", func(f *historyFixture) { f.leases[3].Type = watch.Deleted }},
		{"a Lease never seen", func(f *historyFixture) { f.leases = f.leases[:1] }},
	}
	return cases
}

func TestPostApplyProofHistoryHoldsOneImmutableProof(t *testing.T) {
	t.Parallel()
	check := func(f *historyFixture) error { return postApplyProofHistory(f.schemas, f.jobs, f.leases, f.post) }
	proofRefusesEach(t, buildPostApplyHistory, check, append(historyCases(),
		proofCase[historyFixture]{"an Apply that recorded an applied plan", func(f *historyFixture) {
			f.apply.Status.Applied = &ptahv1alpha1.AppliedStatus{}
		}},
		proofCase[historyFixture]{"an Apply releasing its lock", func(f *historyFixture) {
			f.apply.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		}},
		proofCase[historyFixture]{"an uncertain harvest", func(f *historyFixture) {
			for _, schema := range []*ptahv1alpha1.PtahSchema{f.origin, f.observe, f.plan} {
				schema.Status.PendingObservation.Outcome = ptahv1alpha1.PendingObservationOutcomeUnknown
			}
		}},
		proofCase[historyFixture]{"a harvest of another plan than the Apply ran", func(f *historyFixture) {
			for _, schema := range []*ptahv1alpha1.PtahSchema{f.origin, f.observe, f.plan} {
				schema.Status.PendingObservation.Plan.StatementCount = 9
			}
		}},
		proofCase[historyFixture]{"an Apply in another realm than the harvest", func(f *historyFixture) {
			f.apply.Status.ActiveOperation.CoordinationDigest = proofDigest("0")
		}},
		proofCase[historyFixture]{"an Apply of another target binding", func(f *historyFixture) {
			f.apply.Status.ActiveOperation.Target.URLFrom.Key = "other"
		}},
		proofCase[historyFixture]{"an Apply of another source", func(f *historyFixture) {
			f.apply.Status.ActiveOperation.Source.ResolvedReference = "other"
		}},
		proofCase[historyFixture]{"an Apply with a dev database", func(f *historyFixture) {
			f.apply.Status.ActiveOperation.ObservationDev = &ptahv1alpha1.DatabaseTargetRef{}
		}},
		proofCase[historyFixture]{"an Apply of other observation settings", func(f *historyFixture) {
			f.apply.Status.ActiveOperation.ObservationLockTimeout = metav1.Duration{}
		}},
		proofCase[historyFixture]{"an Apply with no lease duration", func(f *historyFixture) {
			f.apply.Status.ActiveOperation.LeaseDurationSeconds = 0
		}},
		proofCase[historyFixture]{"no lease duration anywhere", func(f *historyFixture) {
			// An omitted duration is null to jq and equal to none recorded,
			// so agreeing zeros still fail.
			for _, schema := range []*ptahv1alpha1.PtahSchema{f.apply, f.origin, f.observe, f.plan} {
				if schema.Status.ActiveOperation != nil {
					schema.Status.ActiveOperation.LeaseDurationSeconds = 0
				}
				if schema.Status.PendingObservation != nil {
					schema.Status.PendingObservation.LeaseDurationSeconds = 0
				}
			}
		}},
		proofCase[historyFixture]{"an Observe reading and recording no lease duration", func(f *historyFixture) {
			f.observe.Status.ActiveOperation.LeaseDurationSeconds = 0
			f.observe.Status.PendingObservation.LeaseDurationSeconds = 0
		}},
		proofCase[historyFixture]{"an Apply that lost lease continuity", func(f *historyFixture) {
			f.apply.Status.ActiveOperation.LeaseContinuityLost = true
		}},
		proofCase[historyFixture]{"never InSync", func(f *historyFixture) { f.schemas = f.schemas[:len(f.schemas)-1] }},
		proofCase[historyFixture]{"InSync for another reason", func(f *historyFixture) {
			f.final.Status.Conditions[0].Reason = "ConvergedAfterUnknownOutcome"
		}},
		proofCase[historyFixture]{"InSync with an operation open", func(f *historyFixture) {
			f.final.Status.ActiveOperation = proofActive(ptahv1alpha1.OperationObserve, "later", "later-uid")
		}},
		proofCase[historyFixture]{"InSync on another artifact", func(f *historyFixture) {
			f.final.Status.Applied.ArtifactDigest = proofDigest("0")
		}},
		proofCase[historyFixture]{"InSync on another plan", func(f *historyFixture) {
			f.final.Status.Applied.PlanFingerprint = proofDigest("0")
		}},
		proofCase[historyFixture]{"InSync by another manager", func(f *historyFixture) {
			f.final.Status.Applied.ControllerImage = "other"
		}},
		proofCase[historyFixture]{"InSync by another runner", func(f *historyFixture) {
			f.final.Status.Applied.RunnerProtocolVersion = 6
		}},
		proofCase[historyFixture]{"InSync with nothing applied", func(f *historyFixture) { f.final.Status.Applied = nil }},
	))
	t.Run("a document of another schema is not read", func(t *testing.T) {
		t.Parallel()
		fixture := buildPostApplyHistory()
		stray := fixture.observe.DeepCopy()
		stray.Name = "another-schema"
		stray.Status.Phase = ptahv1alpha1.PhaseApplying
		fixture.schemas = slices.Insert(fixture.schemas, 1, watchEvent[*ptahv1alpha1.PtahSchema]{
			Type: watch.Modified, Object: stray,
		})
		if err := check(fixture); err != nil {
			t.Fatalf("another schema's document was read: %v", err)
		}
	})
}

// buildUncertainHistory is the history of an uncertain Apply, ending in the
// mode given.
func buildUncertainHistory(mode uncertainMode) func() *historyFixture {
	return func() *historyFixture {
		fixture := buildHistory(ptahv1alpha1.PendingObservationOutcomeUnknown)
		fixture.uncertain = uncertainProof{
			schema: proofSchemaName, applyOperationID: proofApplyID, applyJobUID: proofApplyJobUID,
			lease: proofLeaseIdentity(), observeJobUID: proofObserveUID, planJobUID: proofPlanUID,
			freshPlanUID: "plan-uid-fresh", applyPods: podEvidence{uids: []string{proofApplyPodUID}},
			mode: mode, controller: proofController, stateVersion: proofStateVersion,
		}
		switch mode {
		case uncertainNoChanges:
			fixture.final = proofSchema(proofSchemaName, ptahv1alpha1.PtahSchemaStatus{
				Phase: ptahv1alpha1.PhaseInSync, ExecutionBinding: proofBinding(),
				Conditions: []metav1.Condition{{
					Type: ptahv1alpha1.ConditionInSync, Status: metav1.ConditionTrue, Reason: "ConvergedAfterUnknownOutcome",
				}},
			})
			return fixture
		case uncertainManualDrift:
			fixture.uncertain.oldActual = proofDigest("1")
		}
		current := proofPlan()
		current.Name, current.UID, current.CreatedAt = "e2e-fault-pg-restart-plan-b", "plan-uid-fresh", proofTime(300)
		if mode == uncertainManualDrift {
			current.ActualStateFingerprint, current.Fingerprint, current.ContentDigest =
				proofDigest("5"), proofDigest("6"), proofDigest("8")
		}
		fixture.final = proofSchema(proofSchemaName, ptahv1alpha1.PtahSchemaStatus{
			Phase: ptahv1alpha1.PhaseAwaitingApproval, ExecutionBinding: proofBinding(), Plan: &current,
		})
		fixture.fresh = &ptahv1alpha1.PtahSchemaPlan{
			ObjectMeta: metav1.ObjectMeta{Name: current.Name, UID: current.UID, CreationTimestamp: proofTime(300)},
			Spec: ptahv1alpha1.PtahSchemaPlanSpec{
				ContractVersion: 3,
				SchemaRef:       ptahv1alpha1.ImmutableObjectReference{Name: proofSchemaName, UID: "schema-uid"},
				Fingerprint:     current.Fingerprint, ContentDigest: current.ContentDigest,
				ArtifactDigest: current.ArtifactDigest, CoordinationDigest: current.CoordinationDigest,
				TargetIdentityDigest: current.TargetIdentityDigest, ActualStateFingerprint: current.ActualStateFingerprint,
				DesiredStateFingerprint: current.DesiredStateFingerprint, PolicyFingerprint: current.PolicyFingerprint,
				VerificationPolicyUID: current.VerificationPolicyUID, VerificationPolicyDigest: current.VerificationPolicyDigest,
				ExecutionBindingID: current.ExecutionBindingID, ControllerImage: current.ControllerImage,
				ControllerRevision: current.ControllerRevision, ControllerStateVersion: current.ControllerStateVersion,
				PtahVersion: current.PtahVersion, ExecutorImage: current.ExecutorImage, RunnerImage: current.RunnerImage,
				RunnerProtocolVersion: current.RunnerProtocolVersion, Destructive: current.Destructive,
				StatementCount: current.StatementCount,
			},
		}
		return fixture
	}
}

// uncertainCases are the refusals an uncertain Apply's history owes beyond
// the common ones.
func uncertainCases() []proofCase[historyFixture] {
	pendingOf := func(schema *ptahv1alpha1.PtahSchema) *ptahv1alpha1.PendingObservationStatus {
		return schema.Status.PendingObservation
	}
	return append(historyCases(),
		proofCase[historyFixture]{"a successful harvest", func(f *historyFixture) {
			for _, schema := range []*ptahv1alpha1.PtahSchema{f.origin, f.observe, f.plan} {
				pendingOf(schema).Outcome = ptahv1alpha1.PendingObservationApplySucceeded
			}
		}},
		proofCase[historyFixture]{"the Observe before the uncertain record", func(f *historyFixture) {
			f.schemas[2], f.schemas[3] = f.schemas[3], f.schemas[2]
		}},
		proofCase[historyFixture]{"the Plan before the Observe", func(f *historyFixture) {
			f.schemas[3], f.schemas[4] = f.schemas[4], f.schemas[3]
		}},
		proofCase[historyFixture]{"the lock release before the Plan", func(f *historyFixture) {
			f.schemas[4], f.schemas[5] = f.schemas[5], f.schemas[4]
		}},
		proofCase[historyFixture]{"no dispatch deadline", func(f *historyFixture) {
			f.apply.Status.ActiveOperation.DispatchNotAfter = nil
		}},
		proofCase[historyFixture]{"no execution deadline", func(f *historyFixture) {
			f.apply.Status.ActiveOperation.ExecutionNotAfter = nil
		}},
		proofCase[historyFixture]{"an execution deadline apart", func(f *historyFixture) {
			f.apply.Status.ActiveOperation.ExecutionNotAfter = proofTimePointer(900)
		}},
		proofCase[historyFixture]{"another grace period", func(f *historyFixture) {
			f.apply.Status.ActiveOperation.TerminationGracePeriodSeconds = 0
		}},
		proofCase[historyFixture]{"a record of no Apply Pod", func(f *historyFixture) {
			for _, schema := range []*ptahv1alpha1.PtahSchema{f.origin, f.observe, f.plan} {
				pendingOf(schema).ApplyPodUIDs, pendingOf(schema).ApplyPodCount = nil, 0
			}
		}},
		proofCase[historyFixture]{"a record counting two Pods", func(f *historyFixture) {
			for _, schema := range []*ptahv1alpha1.PtahSchema{f.origin, f.observe, f.plan} {
				pendingOf(schema).ApplyPodCount = 2
			}
		}},
		proofCase[historyFixture]{"a record in another phase", func(f *historyFixture) {
			f.origin.Status.Phase = ptahv1alpha1.PhaseApplying
		}},
		proofCase[historyFixture]{"a record that applied", func(f *historyFixture) {
			f.origin.Status.Applied = &ptahv1alpha1.AppliedStatus{}
		}},
		proofCase[historyFixture]{"an Observe recording another Apply Job", func(f *historyFixture) {
			pendingOf(f.observe).ApplyJobName = "other"
		}},
		proofCase[historyFixture]{"an Observe recording other Pods", func(f *historyFixture) {
			pendingOf(f.observe).ApplyPodUIDs = []types.UID{"other"}
		}},
		proofCase[historyFixture]{"an Observe recording another generation", func(f *historyFixture) {
			pendingOf(f.observe).ApplyGeneration = 4
		}},
		proofCase[historyFixture]{"an Observe observing after another instant", func(f *historyFixture) {
			pendingOf(f.observe).ObserveAfter = nil
		}},
		proofCase[historyFixture]{"a Plan recording another plan", func(f *historyFixture) {
			pendingOf(f.plan).Plan.Fingerprint = proofDigest("0")
		}},
		proofCase[historyFixture]{"an Observe recording another target", func(f *historyFixture) {
			pendingOf(f.observe).Target.URLFrom.Key = "other"
			f.observe.Status.ActiveOperation.Target.URLFrom.Key = "other"
		}},
		proofCase[historyFixture]{"an Observe recording another source", func(f *historyFixture) {
			pendingOf(f.observe).Source.Digest = proofDigest("0")
			f.observe.Status.ActiveOperation.Source.Digest = proofDigest("0")
		}},
		proofCase[historyFixture]{"an Observe recording a dev database", func(f *historyFixture) {
			pendingOf(f.observe).Dev = &ptahv1alpha1.DatabaseTargetRef{}
			f.observe.Status.ActiveOperation.ObservationDev = &ptahv1alpha1.DatabaseTargetRef{}
		}},
		proofCase[historyFixture]{"an Observe recording other settings", func(f *historyFixture) {
			pendingOf(f.observe).DriftSeverity = "error"
			f.observe.Status.ActiveOperation.ObservationSeverity = "error"
		}},
		proofCase[historyFixture]{"an Observe recording another lease duration", func(f *historyFixture) {
			pendingOf(f.observe).LeaseDurationSeconds = 60
			f.observe.Status.ActiveOperation.LeaseDurationSeconds = 60
		}},
		proofCase[historyFixture]{"an applied plan anywhere", func(f *historyFixture) {
			f.release.Status.Applied = &ptahv1alpha1.AppliedStatus{}
		}},
		proofCase[historyFixture]{"no final schema", func(f *historyFixture) { f.final = nil }},
		proofCase[historyFixture]{"another mode", func(f *historyFixture) { f.uncertain.mode = "rebuilt" }},
	)
}

func TestUncertainApplyProofHistoryHoldsTheProofToItsEnd(t *testing.T) {
	t.Parallel()
	check := func(f *historyFixture) error {
		return uncertainApplyProofHistory(f.schemas, f.jobs, f.leases, f.uncertain, f.final, f.fresh)
	}
	t.Run("no changes", func(t *testing.T) {
		t.Parallel()
		proofRefusesEach(t, buildUncertainHistory(uncertainNoChanges), check, append(uncertainCases(),
			proofCase[historyFixture]{"a final schema in another phase", func(f *historyFixture) {
				f.final.Status.Phase = ptahv1alpha1.PhaseAwaitingApproval
			}},
			proofCase[historyFixture]{"a final schema with an operation", func(f *historyFixture) {
				f.final.Status.ActiveOperation = proofActive(ptahv1alpha1.OperationObserve, "later", "later-uid")
			}},
			proofCase[historyFixture]{"a final schema without a binding", func(f *historyFixture) {
				f.final.Status.ExecutionBinding = nil
			}},
			proofCase[historyFixture]{"a final schema of another controller state", func(f *historyFixture) {
				f.final.Status.ExecutionBinding.ControllerStateVersion = 3
			}},
			proofCase[historyFixture]{"a final schema with a plan", func(f *historyFixture) {
				f.final.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{}
			}},
			proofCase[historyFixture]{"a final schema that applied", func(f *historyFixture) {
				f.final.Status.Applied = &ptahv1alpha1.AppliedStatus{}
			}},
			proofCase[historyFixture]{"a final schema with a pending observation", func(f *historyFixture) {
				f.final.Status.PendingObservation = proofPending(ptahv1alpha1.PendingObservationOutcomeUnknown)
			}},
			proofCase[historyFixture]{"a final schema InSync by ScopedConverged", func(f *historyFixture) {
				f.final.Status.Conditions[0].Reason = "ScopedConverged"
			}},
		))
	})
	freshCases := []proofCase[historyFixture]{
		{"no fresh plan", func(f *historyFixture) { f.fresh = nil }},
		{"a final schema in another phase", func(f *historyFixture) { f.final.Status.Phase = ptahv1alpha1.PhaseInSync }},
		{"a final schema that applied", func(f *historyFixture) { f.final.Status.Applied = &ptahv1alpha1.AppliedStatus{} }},
		{"a final schema with a lock release", func(f *historyFixture) {
			f.final.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		}},
		{"another fresh plan UID asked for", func(f *historyFixture) { f.uncertain.freshPlanUID = "other" }},
		{"a current plan of another name", func(f *historyFixture) { f.final.Status.Plan.Name = "other" }},
		{"an approved fresh plan", func(f *historyFixture) {
			f.final.Status.Plan.Approval = &ptahv1alpha1.ConsumedApprovalStatus{Name: "approval"}
		}},
		{"a fresh plan of another schema", func(f *historyFixture) { f.fresh.Spec.SchemaRef.UID = "other" }},
		{"a current plan of another content", func(f *historyFixture) { f.final.Status.Plan.ContentDigest = proofDigest("0") }},
		{"a current plan of another policy", func(f *historyFixture) {
			f.final.Status.Plan.VerificationPolicyUID = "other"
		}},
		{"a fresh plan of another contract", func(f *historyFixture) { f.fresh.Spec.ContractVersion = 2 }},
		{"a final schema under another binding", func(f *historyFixture) {
			f.final.Status.ExecutionBinding.Epoch = proofOtherEpoch
		}},
		{"a fresh plan of another runner", func(f *historyFixture) { f.fresh.Spec.RunnerImage = "other" }},
		{"a fresh plan of other statements", func(f *historyFixture) { f.fresh.Spec.StatementCount = 5 }},
		{"a fresh plan created at another instant", func(f *historyFixture) {
			f.fresh.CreationTimestamp = proofTime(301)
		}},
		{"a fresh plan from another artifact", func(f *historyFixture) {
			f.fresh.Spec.ArtifactDigest = proofDigest("0")
			f.final.Status.Plan.ArtifactDigest = proofDigest("0")
		}},
		{"a fresh plan of another Ptah", func(f *historyFixture) {
			f.fresh.Spec.PtahVersion = "v9.9.9"
			f.final.Status.Plan.PtahVersion = "v9.9.9"
		}},
	}
	t.Run("same plan", func(t *testing.T) {
		t.Parallel()
		proofRefusesEach(t, buildUncertainHistory(uncertainSamePlan), check, append(append(uncertainCases(), freshCases...),
			proofCase[historyFixture]{"a fresh plan of another fingerprint", func(f *historyFixture) {
				f.fresh.Spec.Fingerprint = proofDigest("0")
				f.final.Status.Plan.Fingerprint = proofDigest("0")
			}},
			proofCase[historyFixture]{"a fresh plan from another actual state", func(f *historyFixture) {
				f.fresh.Spec.ActualStateFingerprint = proofDigest("0")
				f.final.Status.Plan.ActualStateFingerprint = proofDigest("0")
			}},
			proofCase[historyFixture]{"a destructive fresh plan", func(f *historyFixture) {
				f.fresh.Spec.Destructive = true
				f.final.Status.Plan.Destructive = true
			}},
			proofCase[historyFixture]{"a fresh plan of other statements than the Apply ran", func(f *historyFixture) {
				for _, schema := range []*ptahv1alpha1.PtahSchema{f.origin, f.observe, f.plan} {
					schema.Status.PendingObservation.Plan.StatementCount = 9
				}
			}},
		))
	})
	t.Run("manual drift", func(t *testing.T) {
		t.Parallel()
		proofRefusesEach(t, buildUncertainHistory(uncertainManualDrift), check, append(append(uncertainCases(), freshCases...),
			proofCase[historyFixture]{"no old actual state", func(f *historyFixture) { f.uncertain.oldActual = "" }},
			proofCase[historyFixture]{"an Apply of another actual state", func(f *historyFixture) {
				f.uncertain.oldActual = proofDigest("0")
			}},
			proofCase[historyFixture]{"a fresh plan from the old actual state", func(f *historyFixture) {
				f.fresh.Spec.ActualStateFingerprint = proofDigest("1")
				f.final.Status.Plan.ActualStateFingerprint = proofDigest("1")
			}},
		))
	})
	t.Run("an optional Apply Pod may be absent", func(t *testing.T) {
		t.Parallel()
		fixture := buildUncertainHistory(uncertainSamePlan)()
		fixture.uncertain.applyPods.optional = true
		for _, schema := range []*ptahv1alpha1.PtahSchema{fixture.origin, fixture.observe, fixture.plan} {
			schema.Status.PendingObservation.ApplyPodUIDs, schema.Status.PendingObservation.ApplyPodCount = nil, 0
		}
		if err := check(fixture); err != nil {
			t.Fatalf("an absent optional Apply Pod was refused: %v", err)
		}
	})
}
