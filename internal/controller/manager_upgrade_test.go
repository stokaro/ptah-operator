package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// nextManager is a later release of the manager fixtures run as: another
// image, another revision and another runner image, and the execution every
// fixture's plan was computed under.
func nextManager() executionBindingJobs {
	return executionBindingJobs{
		controllerImage:    "example.invalid/manager@sha256:" + strings.Repeat("d", 64),
		controllerRevision: testControllerRevision + "-patch",
		runnerImage:        "example.invalid/operator@sha256:" + strings.Repeat("d", 64),
		ptahVersion:        "v0.3.0",
		executorImage:      "example.invalid/ptah@" + testDigest,
		protocol:           int32(runner.ProtocolVersion),
	}
}

// nextManagerJobs builds what nextManager builds, with a Pod template of its
// own the way a real later release has one: the runner image and the recorded
// manager are part of the template. The fake builder writes neither, so this
// is what makes a snapshot the previous manager took disagree with the
// template the next one builds.
type nextManagerJobs struct{ executionBindingJobs }

func (jobs nextManagerJobs) Build(
	schema *operatorv1alpha1.PtahSchema,
	operation operatorv1alpha1.ActiveOperationStatus,
	plan *operatorv1alpha1.PtahSchemaPlan,
) (*batchv1.Job, error) {
	job, err := jobs.executionBindingJobs.Build(schema, operation, plan)
	if err != nil {
		return nil, err
	}
	controllerImage, _, _ := jobs.ManagerIdentity()
	job.Spec.Template.Annotations[workload.AnnotationControllerImage] = controllerImage
	return job, nil
}

// TestAManagerOnlyUpgradeAppliesAPendingSchemaApproval is a patch release of the
// manager arriving while a person's approval waits. The plan and the approval
// were written under one manager; the manager that reconciles next differs in
// its image, its revision and its runner image and in nothing the plan binds.
// The epoch stays, the approval is taken, and the plan it names is applied.
//
// Every boundary a pending approval can be at is covered: not yet looked at
// by any manager, recorded on the plan by the previous manager, and taken by
// an Apply claim whose admission snapshot the previous manager resolved from
// its own Pod template.
func TestAManagerOnlyUpgradeAppliesAPendingSchemaApproval(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"unrecorded approval", "recorded approval", "snapshotted Apply claim"} {
		mode := mode
		recorded := mode != "unrecorded approval"
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			schema, plan, policyConfig := safetyReadyToApplyFixture(t)
			schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
			schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
			refresh := metav1.NewTime(time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC))
			schema.Status.NextReconciliationTime = &refresh
			epoch := schema.Status.ExecutionBinding.Epoch
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, policyConfig)
			reconciler.Client = assignCreatedJobUIDClient{Client: api, uid: "apply-job-uid"}
			reconciler.Plans = planstore.Store{Client: &planUIDAssigningClient{Client: api}, Reader: api}
			reconciler.Locks = targetlock.New(api, api, nil)

			content := []byte("CREATE TABLE widgets (id bigint primary key);\n")
			spec := plan.Spec
			spec.ContentDigest = fingerprint.DigestBytes(content)
			spec.Fingerprint = fingerprint.DigestBytes([]byte("manager-only-upgrade-plan"))
			desired, chunks, err := planstore.Prepare(schema, spec, content)
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			desired.UID = "manager-only-upgrade-plan-uid"
			published, err := reconciler.Plans.Publish(ctx, desired, chunks)
			if err != nil {
				t.Fatalf("Publish() error = %v", err)
			}
			stored := safetyGetSchema(t, api, schema)
			stored.Status.Plan = currentPlanStatus(published)
			approval := recordApprovalFor(t, api, stored, published)
			if recorded {
				stored.Status.Plan.Approval = approval
			}
			if err := api.Status().Update(ctx, stored); err != nil {
				t.Fatalf("record the published plan: %v", err)
			}

			var claimed *operatorv1alpha1.ActiveOperationStatus
			if mode == "snapshotted Apply claim" {
				claimed = reconcileUntilTheApplyClaimIsSnapshotted(t, reconciler, api, schema)
			}

			next := nextManagerJobs{nextManager()}
			controllerImage, controllerRevision, runnerImage := next.ManagerIdentity()
			if published.Spec.ControllerImage == controllerImage || published.Spec.ControllerRevision == controllerRevision ||
				published.Spec.RunnerImage == runnerImage {
				t.Fatal("the plan already records the next manager, so the upgrade below changes nothing")
			}
			reconciler.Jobs = next

			job := reconcileUntilASchemaJobExists(t, reconciler, api, schema)
			if job.Spec.Template.Annotations[workload.AnnotationControllerImage] != controllerImage {
				t.Fatalf("the Apply Job was built by manager %q, want the next one",
					job.Spec.Template.Annotations[workload.AnnotationControllerImage])
			}
			actual := safetyGetSchema(t, api, schema)
			if actual.Status.ExecutionBinding == nil || actual.Status.ExecutionBinding.Epoch != epoch {
				t.Fatalf("a manager-only upgrade moved the execution epoch: %#v", actual.Status.ExecutionBinding)
			}
			operation := actual.Status.ActiveOperation
			if operation == nil || operation.Type != operatorv1alpha1.OperationApply ||
				operation.ExecutionBindingID != epoch || job.Name != operation.JobName {
				t.Fatalf("the Job %q does not belong to an Apply claim under epoch %q: %#v", job.Name, epoch, operation)
			}
			if actual.Status.Plan == nil || actual.Status.Plan.UID != published.UID || actual.Status.Plan.Approval == nil ||
				actual.Status.Plan.Approval.UID != approval.UID {
				t.Fatalf("the Apply does not carry the approved plan: %#v", actual.Status.Plan)
			}
			if claimed != nil {
				// The claim the previous manager took is the one that
				// dispatched, and the snapshot it dispatched under is the one
				// resolved from the next manager's template.
				digest, err := podintent.DigestTemplate(&job.Spec.Template)
				if err != nil {
					t.Fatal(err)
				}
				if operation.ID != claimed.ID || operation.AdmissionSnapshot == nil ||
					operation.AdmissionSnapshot.TemplateDigest == claimed.AdmissionSnapshot.TemplateDigest ||
					operation.AdmissionSnapshot.TemplateDigest != digest {
					t.Fatalf("dispatched claim %#v, want claim %q with a snapshot of the Job's own template",
						operation, claimed.ID)
				}
			}
			persisted := &operatorv1alpha1.PtahSchemaApproval{}
			if err := api.Get(ctx, client.ObjectKey{Namespace: schema.Namespace, Name: approval.Name}, persisted); err != nil {
				t.Fatal(err)
			}
			if !meta.IsStatusConditionTrue(persisted.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) ||
				meta.IsStatusConditionTrue(persisted.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) {
				t.Fatalf("approval conditions = %#v, want consumed by the dispatch and never stale",
					persisted.Status.Conditions)
			}
		})
	}
}

// reconcileUntilTheApplyClaimIsSnapshotted runs the reconciler until an Apply
// claim holds a persisted admission snapshot and has not crossed its dispatch
// boundary, which is where a manager release can find it.
func reconcileUntilTheApplyClaimIsSnapshotted(
	t *testing.T,
	reconciler *SchemaReconciler,
	api client.Client,
	schema *operatorv1alpha1.PtahSchema,
) *operatorv1alpha1.ActiveOperationStatus {
	t.Helper()

	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	for pass := 0; pass < 8; pass++ {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("Reconcile() pass %d error = %v", pass, err)
		}
		operation := safetyGetSchema(t, api, schema).Status.ActiveOperation
		if operation == nil || operation.Type != operatorv1alpha1.OperationApply || operation.AdmissionSnapshot == nil {
			continue
		}
		if operation.DispatchStarted {
			t.Fatalf("the Apply claim crossed its dispatch boundary in the pass that snapshotted it: %#v", operation)
		}
		return operation.DeepCopy()
	}
	t.Fatalf("no Apply claim was snapshotted after eight passes: %#v", safetyGetSchema(t, api, schema).Status)
	return nil
}

// reconcileUntilASchemaJobExists runs the reconciler until it has created a
// Job, and fails naming the status it stopped at when it never does.
func reconcileUntilASchemaJobExists(
	t *testing.T,
	reconciler *SchemaReconciler,
	api client.Client,
	schema *operatorv1alpha1.PtahSchema,
) *batchv1.Job {
	t.Helper()

	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	for pass := 0; pass < 8; pass++ {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("Reconcile() pass %d error = %v", pass, err)
		}
		jobs := &batchv1.JobList{}
		if err := api.List(context.Background(), jobs, client.InNamespace(schema.Namespace)); err != nil {
			t.Fatal(err)
		}
		if len(jobs.Items) == 1 {
			return &jobs.Items[0]
		}
		if len(jobs.Items) > 1 {
			t.Fatalf("%d Jobs were dispatched, want one", len(jobs.Items))
		}
	}
	actual := safetyGetSchema(t, api, schema)
	t.Fatalf("no Job was dispatched after eight passes: phase=%q operation=%#v conditions=%#v",
		actual.Status.Phase, actual.Status.ActiveOperation, actual.Status.Conditions)
	return nil
}

// TestAdoptedJobIntentTakesOnlyTheRecordedManager checks a Job one manager
// dispatched against the rebuild of the manager that follows it, with the
// real builder on both sides.
//
// The rebuild differs from the live Job in the recorded manager alone, and
// the check accepts it once the claim's snapshot pins the live template. It
// refuses a Job whose recorded manager was rewritten after the snapshot, a
// claim with no snapshot to pin it to, and a Job that differs in anything
// else.
func TestAdoptedJobIntentTakesOnlyTheRecordedManager(t *testing.T) {
	t.Parallel()

	previous := workload.Builder{
		ExecutorImage:          "example.invalid/ptah@" + testDigest,
		RunnerImage:            testRunnerImage,
		PtahVersion:            "v0.3.0",
		ControllerImage:        testControllerImage,
		ControllerRevision:     testControllerRevision,
		ControllerStateVersion: testControllerStateVersion,
	}
	next := previous
	next.ControllerImage, next.ControllerRevision, next.RunnerImage = nextManager().ManagerIdentity()

	schema := schemaFixture()
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationResolve, ID: "resolve-under-the-previous-manager",
		InputFingerprint: testDigest, StartedAt: metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)),
		Attempt: 1, ExecutionBindingID: schema.Status.ExecutionBinding.Epoch,
	}
	operation := schema.Status.ActiveOperation
	name, err := workload.NameFor(schema, *operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.JobName = name
	built, err := previous.Build(schema, *operation, nil)
	if err != nil {
		t.Fatal(err)
	}
	templateDigest, err := podintent.DigestTemplate(&built.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	operation.AdmissionSnapshot = &operatorv1alpha1.PodAdmissionSnapshot{
		Version: podintent.SnapshotVersion, TemplateDigest: templateDigest, Digest: testDigest,
	}
	live, err := previous.Build(schema, *operation, nil)
	if err != nil {
		t.Fatal(err)
	}
	live.UID = "live-job-uid"

	rebuild := func() *batchv1.Job {
		t.Helper()
		expected, err := next.Build(schema, *operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		return expected
	}
	if validateJobIntent(live, rebuild(), schema) == nil {
		t.Fatal("the two managers build the same Job, so nothing below proves anything")
	}
	if err := validateAdoptedJobIntent(live, rebuild(), schema, operation.AdmissionSnapshot); err != nil {
		t.Fatalf("a Job the previous manager of the same execution dispatched was refused: %v", err)
	}
	if err := validateAdoptedJobIntent(live, rebuild(), schema, nil); err == nil {
		t.Fatal("a Job built by another manager was adopted with no snapshot to hold it to")
	}

	rewritten := live.DeepCopy()
	for _, annotations := range []map[string]string{rewritten.Annotations, rewritten.Spec.Template.Annotations} {
		annotations[workload.AnnotationControllerImage] = "example.invalid/manager@sha256:" + strings.Repeat("7", 64)
	}
	if err := validateAdoptedJobIntent(rewritten, rebuild(), schema, operation.AdmissionSnapshot); err == nil ||
		!strings.Contains(err.Error(), "admission snapshot") {
		t.Fatalf("a Job whose recorded manager changed after the snapshot = %v, want a snapshot refusal", err)
	}

	moved := live.DeepCopy()
	moved.Spec.Template.Spec.Containers[0].Image = "example.invalid/ptah@sha256:" + strings.Repeat("7", 64)
	if err := validateAdoptedJobIntent(moved, rebuild(), schema, operation.AdmissionSnapshot); err == nil {
		t.Fatal("a Job running another executor was adopted as the claim's Job")
	}
}
