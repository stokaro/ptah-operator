package webhook_test

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	controllerWriteWebhook = "vcontrollerwrite.operator.ptah.run"
	// otherWriter is an identity that may create Jobs and is not the manager.
	otherWriter = "mallory@example.com"
)

// dispatchFixture is a schema with a Resolve operation claimed and its Pod
// admission snapshot persisted: the state the manager is in the moment before
// it creates the Job. job is what the manager's own builder makes of it.
type dispatchFixture struct {
	namespace string
	schema    *operatorv1alpha1.PtahSchema
	job       *batchv1.Job
}

func newDispatchFixture(t *testing.T, prefix string, resultEndpoint ...string) dispatchFixture {
	t.Helper()
	ctx := context.Background()
	namespace := newNamespace(t, prefix)
	createPolicy(t, namespace)
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: executionAccount}}
	if err := admin.Create(ctx, account); err != nil {
		t.Fatalf("create ServiceAccount %s/%s: %v", namespace, executionAccount, err)
	}
	schema := createSchema(t, namespace, "orders")
	builder := manager.builder()
	if len(resultEndpoint) != 0 {
		builder.ResultEndpoint = resultEndpoint[0]
	}

	operation := operatorv1alpha1.ActiveOperationStatus{
		Type:               operatorv1alpha1.OperationResolve,
		ID:                 "resolve-orders-1",
		InputFingerprint:   digest("8"),
		StartedAt:          now(),
		Attempt:            1,
		ExecutionBindingID: executionBindingID,
	}
	schema.Status = operatorv1alpha1.PtahSchemaStatus{
		ObservedGeneration: schema.Generation,
		ExecutionBinding:   executionBinding(),
	}
	name, err := workload.NameFor(schema, operation)
	if err != nil {
		t.Fatalf("name the Resolve Job for %s/%s: %v", namespace, schema.Name, err)
	}
	operation.JobName = name

	// The manager resolves the snapshot from the Job it is about to create and
	// persists it before creating anything, so the snapshot is taken from the
	// same build the Job comes from.
	unbound, err := builder.Build(schema, operation, nil)
	if err != nil {
		t.Fatalf("build the Resolve Job for %s/%s: %v", namespace, schema.Name, err)
	}
	operation.AdmissionSnapshot, err = podintent.Resolve(ctx, admin, namespace, &unbound.Spec.Template, manager.admission)
	if err != nil {
		t.Fatalf("resolve the Pod admission snapshot in %s: %v", namespace, err)
	}
	schema.Status.ActiveOperation = &operation
	writeStatus(t, schema)

	// The webhook rebuilds the Job from the stored schema, so the test does too.
	stored := &operatorv1alpha1.PtahSchema{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(schema), stored); err != nil {
		t.Fatalf("read PtahSchema %s back: %v", client.ObjectKeyFromObject(schema), err)
	}
	job, err := builder.Build(stored, *stored.Status.ActiveOperation, nil)
	if err != nil {
		t.Fatalf("rebuild the Resolve Job from the stored %s: %v", client.ObjectKeyFromObject(stored), err)
	}
	jobs := rbacv1.PolicyRule{APIGroups: []string{batchv1.GroupName}, Resources: []string{"jobs"}, Verbs: []string{"create", "get"}}
	grant(t, namespace, "manager-jobs", managerSubject(t), jobs)
	grant(t, namespace, "other-jobs", userSubject(otherWriter), jobs)
	return dispatchFixture{namespace: namespace, schema: stored, job: job}
}

// tampered is the builder's Job running another image, the smallest change
// that makes it a different workload.
func (fixture dispatchFixture) tampered() *batchv1.Job {
	job := fixture.job.DeepCopy()
	job.Spec.Template.Spec.Containers[0].Image = "example.invalid/elsewhere@sha256:" + strings.Repeat("5", 64)
	return job
}

func TestControllerWriteWebhook(t *testing.T) {
	plane.Require(t)
	fixture := newDispatchFixture(t, "dispatch")
	ctx := context.Background()
	managerAPI := clientAs(t, manager.username)

	// The three rows share one Job name, so they run in order: the refusals are
	// dry runs, and only the last row stores the Job.
	t.Run("the manager's Job with a changed image is refused", func(t *testing.T) {
		err := managerAPI.Create(ctx, fixture.tampered(), client.DryRunAll)
		requireDenied(t, err, controllerWriteWebhook, "Job is outside the active operation intent")
	})

	t.Run("the same Job from another identity is not the controller-write webhook's to judge", func(t *testing.T) {
		// The entry's matchCondition sends it the manager's writes and nothing
		// else. Nothing else in this API server objects to the Job, so it is
		// admitted; the chart's admission policies are measured elsewhere.
		err := clientAs(t, otherWriter).Create(ctx, fixture.tampered(), client.DryRunAll)
		requireNotDeniedBy(t, err, controllerWriteWebhook)
		if err != nil {
			t.Fatalf("the API server refused %s's Job %s/%s: %v", otherWriter, fixture.namespace, fixture.job.Name, err)
		}
	})

	t.Run("the Job the manager's builder makes is admitted", func(t *testing.T) {
		job := fixture.job.DeepCopy()
		if err := managerAPI.Create(ctx, job); err != nil {
			t.Fatalf("the API server refused the manager's own Job %s/%s: %v", fixture.namespace, fixture.job.Name, err)
		}
		if job.UID == "" {
			t.Fatalf("Job %s/%s was admitted but not stored", fixture.namespace, fixture.job.Name)
		}
	})
}

// TestControllerWriteWebhookHoldsTheCleanupTTLToItsReason serves what the chart's
// policy cannot judge. The policy sees only the Job, and a failed Apply usually
// leaves a Complete Job, because a runner that delivered its frame exits zero. The
// webhook reads the Pod's termination summary, so it is what refuses the long TTL
// on a run that reported success.
func TestControllerWriteWebhookHoldsTheCleanupTTLToItsReason(t *testing.T) {
	plane.Require(t)
	fixture := newDispatchFixture(t, "cleanup-ttl")
	ctx := context.Background()
	managerAPI := clientAs(t, manager.username)
	grant(t, fixture.namespace, "manager-job-cleanup", managerSubject(t),
		rbacv1.PolicyRule{APIGroups: []string{batchv1.GroupName}, Resources: []string{"jobs"}, Verbs: []string{"get", "patch"}})
	start := metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	finish := metav1.NewTime(start.Add(time.Minute))
	finished := batchv1.JobStatus{
		StartTime: &start, CompletionTime: &finish, Succeeded: 1,
		Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: finish},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: finish},
		},
	}
	store := func(job *batchv1.Job) *batchv1.Job {
		t.Helper()
		if err := admin.Create(ctx, job); err != nil {
			t.Fatalf("store Job %s: %v", job.Name, err)
		}
		job.Status = finished
		if err := admin.Status().Update(ctx, job); err != nil {
			t.Fatalf("finish Job %s: %v", job.Name, err)
		}
		return job
	}
	setTTL := func(job *batchv1.Job, seconds int32) error {
		patched := job.DeepCopy()
		patched.Spec.TTLSecondsAfterFinished = &seconds
		return managerAPI.Patch(ctx, patched, client.MergeFrom(job), client.DryRunAll)
	}
	// finishedApply stores an Apply Job and, when a summary is given, the one Pod
	// that left it. The Pod webhook holds an operator Job's Pod to the claim that
	// names it, and these Jobs are no claim's, so the Pod is stored while its Job
	// does not yet say whose it is. Nothing below reads the order.
	finishedApplyOf := func(owners []metav1.OwnerReference, operationID, suffix string, summaryFor func(*batchv1.Job) string) *batchv1.Job {
		t.Helper()
		built := fixture.job.DeepCopy()
		built.Name = "ptah-apply-" + suffix + "-" + strings.TrimPrefix(built.Name, "ptah-resolve-")
		built.Labels[workload.LabelOperation] = "apply"
		built.Spec.Template.Labels[workload.LabelOperation] = "apply"
		built.Annotations[workload.AnnotationOperationID] = operationID
		built.OwnerReferences = owners
		job := built.DeepCopy()
		// Not empty: an empty set is filled in from the Pod template.
		job.Labels, job.OwnerReferences = map[string]string{"fixture": "unowned"}, nil
		if err := admin.Create(ctx, job); err != nil {
			t.Fatalf("store Job %s: %v", job.Name, err)
		}
		if summaryFor != nil {
			controller := true
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: job.Namespace, Name: job.Name + "-pod", Labels: map[string]string{"job-name": job.Name},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: job.Name, UID: job.UID,
						Controller: &controller, BlockOwnerDeletion: &controller,
					}},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "ptah", Image: "example.invalid/ptah:1"}}},
			}
			if err := admin.Create(ctx, pod); err != nil {
				t.Fatalf("store Pod %s: %v", pod.Name, err)
			}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "ptah", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: summaryFor(built)}},
			}}
			if err := admin.Status().Update(ctx, pod); err != nil {
				t.Fatalf("finish Pod %s: %v", pod.Name, err)
			}
		}
		job.Labels, job.OwnerReferences = built.Labels, built.OwnerReferences
		if err := admin.Update(ctx, job); err != nil {
			t.Fatalf("give Job %s its owner: %v", job.Name, err)
		}
		job.Status = finished
		if err := admin.Status().Update(ctx, job); err != nil {
			t.Fatalf("finish Job %s: %v", job.Name, err)
		}
		return job
	}
	finishedApply := func(suffix string, summaryFor func(*batchv1.Job) string) *batchv1.Job {
		t.Helper()
		return finishedApplyOf(fixture.job.OwnerReferences, fixture.job.Annotations[workload.AnnotationOperationID], suffix, summaryFor)
	}
	summary := func(result runner.Result) func(*batchv1.Job) string {
		return func(job *batchv1.Job) string {
			t.Helper()
			result.ProtocolVersion = runner.ProtocolVersion
			result.Operation = runner.OperationApply
			// The full ID, as the runner writes it. The label holds a hash.
			result.OperationID = job.Annotations[workload.AnnotationOperationID]
			result.CoordinationDigest = digest("c")
			result.TargetIdentityDigest = digest("d")
			result.MutationStarted = true
			encoded, err := runner.EncodeSummary(result)
			if err != nil {
				t.Fatalf("EncodeSummary() error = %v", err)
			}
			return string(encoded)
		}
	}

	// The webhook judges a Job's CREATE and UPDATE. A person who has read a failed
	// Apply's log deletes the Job, and nothing here stands in the way.
	t.Run("a person deletes a retained Job", func(t *testing.T) {
		grant(t, fixture.namespace, "person-job-delete", rbacv1.Subject{Kind: rbacv1.UserKind, Name: "retained-job-reader"},
			rbacv1.PolicyRule{APIGroups: []string{batchv1.GroupName}, Resources: []string{"jobs"}, Verbs: []string{"delete"}})
		job := finishedApply("deleted", nil)
		seconds := int32(86400)
		patched := job.DeepCopy()
		patched.Spec.TTLSecondsAfterFinished = &seconds
		if err := admin.Patch(ctx, patched, client.MergeFrom(job)); err != nil {
			t.Fatalf("give Job %s its TTL: %v", job.Name, err)
		}
		if err := clientAs(t, "retained-job-reader").Delete(ctx, patched, client.DryRunAll); err != nil {
			t.Fatalf("delete Job %s: %v", patched.Name, err)
		}
	})
	t.Run("a TTL that is neither value is refused by naming both", func(t *testing.T) {
		requireDenied(t, setTTL(store(fixture.job.DeepCopy()), 600), controllerWriteWebhook,
			"Job cleanup TTL 600 is neither the 300 seconds every finished Job keeps nor the 86400 seconds")
	})
	t.Run("the long TTL on a read-only Job is refused by naming its operation", func(t *testing.T) {
		job := fixture.job.DeepCopy()
		job.Name += "-read-only"
		requireDenied(t, setTTL(store(job), 86400), controllerWriteWebhook, `this is a "resolve" Job; it keeps 300 seconds`)
	})
	t.Run("the long TTL on an Apply that reported success is refused", func(t *testing.T) {
		job := finishedApply("succeeded", summary(runner.Result{}))
		refused := setTTL(job, 86400)
		requireDenied(t, refused, controllerWriteWebhook, "this run reported success; it keeps 300 seconds")
		// The manager falls back to the short TTL on this class of error, which
		// is how a failure of the manager's own, on a run that succeeded, settles.
		if !apierrors.IsForbidden(refused) && !apierrors.IsInvalid(refused) {
			t.Fatalf("the webhook's refusal is %T %v, which the manager would not fall back from", refused, refused)
		}
	})
	// A Job past the outcome check is judged by the approval its claim names next,
	// and these are not any claim's, so none names one. That refusal is the proof
	// they passed this one, and the long TTL's refusal without an approval as the
	// API server returns it.
	const pastTheOutcome = "no approval authorized this run; it keeps 300 seconds"
	t.Run("the long TTL on an Apply that reported an error passes the outcome check", func(t *testing.T) {
		job := finishedApply("failed", summary(runner.Result{
			ChildExitCode: 2, Uncertain: true, Error: &runner.ResultError{Code: "apply_outcome_unknown", Message: "unknown"},
		}))
		requireDenied(t, setTTL(job, 86400), controllerWriteWebhook, pastTheOutcome)
	})
	t.Run("the long TTL on an Apply whose Pod is gone passes the outcome check", func(t *testing.T) {
		requireDenied(t, setTTL(finishedApply("collected", nil), 86400), controllerWriteWebhook, pastTheOutcome)
	})

	// A migration run no approval names, as `apply: Always` claims one: the claim names
	// the Job, and the Job's Pod says what the run reported. These Jobs are not what
	// the claim builds, so the last check refuses each one, and that refusal is the
	// proof a row passed the outcome.
	migration := createMigration(t, fixture.namespace, "ledger")
	controller := true
	claimedRun := func(suffix string, summaryFor func(*batchv1.Job) string) *batchv1.Job {
		t.Helper()
		job := finishedApplyOf([]metav1.OwnerReference{{
			APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahMigration", Name: migration.Name, UID: migration.UID,
			Controller: &controller, BlockOwnerDeletion: &controller,
		}}, digest("9"), suffix, summaryFor)
		stored := &operatorv1alpha1.PtahMigration{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(migration), stored); err != nil {
			t.Fatalf("read PtahMigration %s: %v", migration.Name, err)
		}
		stored.Status.ObservedGeneration = stored.Generation
		stored.Status.ExecutionBinding = executionBinding()
		stored.Status.ActiveOperation = &operatorv1alpha1.MigrationOperationStatus{
			Type: operatorv1alpha1.MigrationOperationApply, ID: job.Annotations[workload.AnnotationOperationID],
			InputFingerprint: digest("8"), StartedAt: now(), Attempt: 1, ExecutionBindingID: executionBindingID,
			JobName: job.Name, JobUID: job.UID,
		}
		writeStatus(t, stored)
		return job
	}
	migrationRun := func(outcome string) func(*batchv1.Job) string {
		return func(job *batchv1.Job) string {
			t.Helper()
			encoded, err := runner.EncodeSummary(runner.Result{
				ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
				// The full ID, as the runner writes it, and as the claim keeps it.
				OperationID:        job.Annotations[workload.AnnotationOperationID],
				CoordinationDigest: digest("c"), TargetIdentityDigest: digest("d"),
				MutationStarted: true,
				Uncertain:       outcome == dataplane.MigrationOutcomePartial || outcome == dataplane.MigrationOutcomeUnknown,
				MigrationRun: &dataplane.MigrationRunReport{
					ContractVersion: dataplane.SupportedMigrationRunContract, Direction: "up", Outcome: outcome,
				},
			})
			if err != nil {
				t.Fatalf("EncodeSummary() error = %v", err)
			}
			return string(encoded)
		}
	}
	const pastTheRun = "migration Job cleanup is outside the persisted operation claim"
	t.Run("the long TTL on a migration run that reported Failed is refused", func(t *testing.T) {
		refused := setTTL(claimedRun("run-failed", migrationRun(dataplane.MigrationOutcomeFailed)), 86400)
		requireDenied(t, refused, controllerWriteWebhook, "no approval authorized this run and it reported Failed; it keeps 300 seconds")
		// The manager falls back to the short TTL on this class of error, so the claim
		// settles even when it asked before the Pod's report arrived.
		if !apierrors.IsForbidden(refused) && !apierrors.IsInvalid(refused) {
			t.Fatalf("the webhook's refusal is %T %v, which the manager would not fall back from", refused, refused)
		}
	})
	t.Run("the long TTL on a migration run that reported Applied is refused", func(t *testing.T) {
		requireDenied(t, setTTL(claimedRun("run-applied", migrationRun(dataplane.MigrationOutcomeApplied)), 86400),
			controllerWriteWebhook, "this run reported success; it keeps 300 seconds")
	})
	t.Run("the long TTL on a migration run that reported Partial passes the outcome", func(t *testing.T) {
		requireDenied(t, setTTL(claimedRun("run-partial", migrationRun(dataplane.MigrationOutcomePartial)), 86400),
			controllerWriteWebhook, pastTheRun)
	})
	t.Run("the long TTL on a migration run that reported Unknown passes the outcome", func(t *testing.T) {
		requireDenied(t, setTTL(claimedRun("run-unknown", migrationRun(dataplane.MigrationOutcomeUnknown)), 86400),
			controllerWriteWebhook, pastTheRun)
	})
	t.Run("the long TTL on a migration run whose Pod is gone passes the outcome", func(t *testing.T) {
		requireDenied(t, setTTL(claimedRun("run-collected", nil), 86400), controllerWriteWebhook, pastTheRun)
	})
}

// The manager writes a plan's bytes twice: as the chunks it publishes, and as
// the ConfigMaps an Apply mounts them through. The chart sends both to the
// controller-write webhook, which reads the owning plan straight from the API
// server before anything else. A chunk and a projection naming a plan that
// does not exist are both refused there, which is what shows the webhook
// configuration routes each resource to the handler.
func TestControllerWriteWebhookJudgesPlanChunksAndProjections(t *testing.T) {
	plane.Require(t)
	ctx := context.Background()
	namespace := newNamespace(t, "chunks")
	grant(t, namespace, "manager-chunks", managerSubject(t),
		rbacv1.PolicyRule{APIGroups: []string{operatorv1alpha1.GroupVersion.Group}, Resources: []string{"ptahschemaplanchunks"}, Verbs: []string{"create"}},
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"create"}},
	)
	managerAPI := clientAs(t, manager.username)

	const planName = "ptah-plan-111111111111111111111111"
	controller := true
	metadata := metav1.ObjectMeta{
		Namespace: namespace,
		Name:      planName + "-000",
		Labels:    map[string]string{"operator.ptah.run/plan": planName, "operator.ptah.run/schema": "orders"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahSchemaPlan",
			Name: planName, UID: "11111111-1111-1111-1111-111111111111",
			Controller: &controller, BlockOwnerDeletion: &controller,
		}},
	}

	t.Run("a chunk of a plan that does not exist", func(t *testing.T) {
		chunk := &operatorv1alpha1.PtahSchemaPlanChunk{
			ObjectMeta: *metadata.DeepCopy(),
			Spec:       operatorv1alpha1.PtahSchemaPlanChunkSpec{Data: []byte("probe")},
		}
		requireDenied(t, managerAPI.Create(ctx, chunk, client.DryRunAll), controllerWriteWebhook, "directly read plan manifest")
	})

	t.Run("a projection of a plan that does not exist", func(t *testing.T) {
		immutable := true
		projection := &corev1.ConfigMap{
			ObjectMeta: *metadata.DeepCopy(),
			Immutable:  &immutable,
			BinaryData: map[string][]byte{"chunk": []byte("probe")},
		}
		requireDenied(t, managerAPI.Create(ctx, projection, client.DryRunAll), controllerWriteWebhook, "directly read plan manifest")
	})
}
