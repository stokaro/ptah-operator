package controller

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// movedProofURL is a URL of another target than any fixture's plan names.
const movedProofURL = "postgres://other:secret@moved.example/app"

// jobUIDByNameClient gives every Job it creates a UID of its own, as the API
// server does, so a later attempt is not mistaken for an earlier one.
type jobUIDByNameClient struct{ client.Client }

func (c jobUIDByNameClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if job, ok := object.(*batchv1.Job); ok && job.UID == "" {
		job.UID = types.UID(job.Name + "-uid")
	}
	return c.Client.Create(ctx, object, options...)
}

// noChild is an executor for a run that must start none.
type noChild struct{ t *testing.T }

func (e noChild) Execute(_ context.Context, spec runner.CommandSpec, _, _ io.Writer) (int, error) {
	e.t.Errorf("the runner started a child: %s %v", spec.Path, spec.Args)
	return -1, context.Canceled
}

// executorEnv is the operation container's environment of a built Job, by name.
func executorEnv(t *testing.T, job *batchv1.Job) map[string]corev1.EnvVar {
	t.Helper()
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.Name != executorContainerName {
			continue
		}
		environment := make(map[string]corev1.EnvVar, len(container.Env))
		for _, variable := range container.Env {
			environment[variable.Name] = variable
		}
		return environment
	}
	t.Fatalf("Job %s has no %s container", job.Name, executorContainerName)
	return nil
}

// runBuiltRead runs the runner on what job hands its operation container, with
// the Secret the Job names holding databaseURL, and frames what it returns. The
// executor fails the test if a child starts.
func runBuiltRead(t *testing.T, job *batchv1.Job, operation operatorv1alpha1.OperationType, databaseURL string) (runner.Result, []byte) {
	t.Helper()
	// The Job names the URL by a file the kubelet would project from its
	// Secret; here the file holds the URL the Secret has moved to.
	file := filepath.Join(t.TempDir(), "url")
	if err := os.WriteFile(file, []byte(databaseURL), 0o400); err != nil {
		t.Fatal(err)
	}
	environment := []string{runner.EnvDatabaseURLFile + "=" + file}
	for name, variable := range executorEnv(t, job) {
		if variable.ValueFrom == nil && name != runner.EnvDatabaseURLFile {
			environment = append(environment, name+"="+variable.Value)
		}
	}
	if _, named := executorEnv(t, job)[runner.EnvDatabaseURLFile]; !named {
		t.Fatalf("the built read names no %s", runner.EnvDatabaseURLFile)
	}
	result := runner.Run(context.Background(), runner.Config{
		Operation:   mutationlifecycle.SchemaOperation(operation).Runner,
		Environment: environment,
		Executor:    noChild{t: t},
	})
	return result, safetyRunnerFrame(t, result)
}

// finishBuiltJob marks job complete, stands up the Pod its template runs, and
// hands the reconciler frame as that Pod's log.
func finishBuiltJob(t *testing.T, reconciler *SchemaReconciler, api client.Client, job *batchv1.Job, frame []byte) {
	t.Helper()
	terminal := &batchv1.Job{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(job), terminal); err != nil {
		t.Fatal(err)
	}
	terminal.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := api.Status().Update(context.Background(), terminal); err != nil {
		t.Fatal(err)
	}
	if err := api.Create(context.Background(), terminalPodForJob(terminal)); err != nil {
		t.Fatal(err)
	}
	reconciler.Logs = staticLogs{content: frame}
}

// reconcileUntilJobNamed takes passes until the claim's Job under name exists.
func reconcileUntilJobNamed(
	t *testing.T,
	reconciler *SchemaReconciler,
	api client.Client,
	schema *operatorv1alpha1.PtahSchema,
	name string,
) *batchv1.Job {
	t.Helper()
	for range 8 {
		reconcileSchema(t, reconciler, api, schema)
		job := &batchv1.Job{}
		err := api.Get(context.Background(), client.ObjectKey{Namespace: schema.Namespace, Name: name}, job)
		if err == nil {
			return job
		}
		if !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
	actual := storedSchemaOf(t, api, schema)
	t.Fatalf("no Job %s was dispatched after eight passes: phase=%q, ReconciliationFailed=%#v",
		name, actual.Status.Phase, findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed))
	return nil
}

// builtProofFixture is a schema whose proof is about to dispatch its read, and
// a reconciler with the real Job builder: the claim is there and no Job is.
// Each edit changes the schema before its claim is bound.
func builtProofFixture(
	t *testing.T,
	operation operatorv1alpha1.OperationType,
	edits ...func(*operatorv1alpha1.PtahSchema),
) (*operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client) {
	t.Helper()
	schema := safetyPostApplyObserveSchema(t)
	for _, edit := range edits {
		edit(schema)
	}
	schema.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: time.Minute}
	policyDigest := fingerprint.DigestBytes([]byte("policy"))
	schema.Status.Source.VerificationPolicyDigest = policyDigest
	schema.Status.PendingObservation.Plan.VerificationPolicyDigest = policyDigest
	if operation == operatorv1alpha1.OperationPlan {
		schema.Status.PendingObservation.PlanRequired = true
	}
	schema.Status.ActiveOperation.Type = operation
	schema.Status.ActiveOperation.JobUID = ""
	bindActiveInput(t, schema)
	policy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: schema.Spec.Desired.VerificationPolicyFrom.Name, UID: testPolicyUID},
		Immutable:  ptr(true),
		Data:       map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: "policy"},
	}
	builder, key := leaderBuilder(t)
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, policy)
	reconciler.Client = jobUIDByNameClient{Client: api}
	reconciler.Jobs = builder
	reconciler.SealKey = key
	reconciler.Locks = targetlock.New(api, api, nil)
	// The name follows the claim's input fingerprint, which seeding the fixture's
	// Lease has just rewritten.
	stored := storedSchemaOf(t, api, schema)
	name, err := workload.NameFor(stored, *stored.Status.ActiveOperation)
	if err != nil {
		t.Fatal(err)
	}
	stored.Status.ActiveOperation.JobName = name
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	return stored, reconciler, api
}

// The reproduction of stokaro/ptah-operator#893. A proof's read used to log in to
// whatever target the Secret held, with its credentials, at every attempt for as
// long as nobody acted. The Job the controller builds for it now names the
// applied plan's target, and the runner it runs refuses another before the child
// starts. The refusal still names the target the URL gave, so the resource stops
// as it did, keeps its proof and its Lease, prints the command, and a person's
// statement is taken at the next refused read.
func TestARefusedProofReadStopsAndIsSettledAsAReadOfAnotherTargetWas(t *testing.T) {
	t.Parallel()
	movedDigest, err := runner.TargetIdentityDigest(movedProofURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []operatorv1alpha1.OperationType{operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			schema, reconciler, api := builtProofFixture(t, operation)
			planTarget := schema.Status.PendingObservation.Plan.TargetIdentityDigest
			if planTarget == movedDigest {
				t.Fatal("the moved URL names the plan's target, so nothing is refused")
			}

			job := reconcileUntilJobNamed(t, reconciler, api, schema, schema.Status.ActiveOperation.JobName)
			if bound := executorEnv(t, job)[runner.EnvExpectedTargetIdentityDigest]; bound.ValueFrom != nil || bound.Value != planTarget {
				t.Fatalf("the proof read Job binds %#v, want the applied plan's target %q", bound, planTarget)
			}
			result, frame := runBuiltRead(t, job, operation, movedProofURL)
			if result.Error == nil || result.Error.Code != "target_binding_mismatch" || result.TargetIdentityDigest != movedDigest {
				t.Fatalf("the runner returned error %#v naming %q, want target_binding_mismatch naming %q",
					result.Error, result.TargetIdentityDigest, movedDigest)
			}
			finishBuiltJob(t, reconciler, api, job, frame)
			blocked := reconcileSchema(t, reconciler, api, schema)

			value := settledApplyValue("apply-operation", movedDigest)
			assertStopPrints(t, blocked, value, heldNothing)
			if blocked.Status.ActiveOperation == nil || blocked.Status.ActiveOperation.Type != operation ||
				blocked.Status.ActiveOperation.Attempt != schema.Status.ActiveOperation.Attempt+1 {
				t.Fatalf("the read does not go on: %#v", blocked.Status.ActiveOperation)
			}
			if proofLeaseHolder(t, reconciler, api) == "" {
				t.Fatal("the refused read released the proof's Lease")
			}

			setSettledApply(t, api, blocked, value)
			reached := blocked.Status.NextReconciliationTime.Add(time.Second)
			reconciler.Clock = func() time.Time { return reached }
			next := reconcileUntilJobNamed(t, reconciler, api, schema, blocked.Status.ActiveOperation.JobName)
			_, frame = runBuiltRead(t, next, operation, movedProofURL)
			finishBuiltJob(t, reconciler, api, next, frame)
			assertSettled(t, reconcileSchema(t, reconciler, api, schema))
		})
	}
}

// stokaro/ptah-operator#1084. The plan was computed against db.internal, and
// the Secret now names db.\u0130nternal. strings.ToLower used to read that
// host as the plan's, so the proof's read passed its target binding, logged in
// to another name and settled the Apply by what it held. The runner now
// refuses the host before the child starts and names no target, so the read
// fails as any read that names none does: the proof, its Lease and the read
// stay, the resource does not stop as for another target, and the condition
// says what to write instead.
func TestAProofReadOfAHostOutsidePrintableASCIIKeepsTheProof(t *testing.T) {
	t.Parallel()
	plannedTarget, err := runner.TargetIdentityDigest("postgres://app:secret@db.internal/app")
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []operatorv1alpha1.OperationType{operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			schema, reconciler, api := builtProofFixture(t, operation, func(schema *operatorv1alpha1.PtahSchema) {
				schema.Status.PendingObservation.Plan.TargetIdentityDigest = plannedTarget
			})
			job := reconcileUntilJobNamed(t, reconciler, api, schema, schema.Status.ActiveOperation.JobName)
			if bound := executorEnv(t, job)[runner.EnvExpectedTargetIdentityDigest]; bound.Value != plannedTarget {
				t.Fatalf("the proof read Job binds %#v, want the plan's target %q", bound, plannedTarget)
			}
			result, frame := runBuiltRead(t, job, operation, "postgres://app:secret@db.\u0130nternal/app")
			if result.Error == nil || result.Error.Code != "invalid_target" || result.TargetIdentityDigest != "" {
				t.Fatalf("the runner returned error %#v naming %q, want invalid_target naming no target",
					result.Error, result.TargetIdentityDigest)
			}
			finishBuiltJob(t, reconciler, api, job, frame)
			actual := reconcileSchema(t, reconciler, api, schema)

			pending := actual.Status.PendingObservation
			if pending == nil || pending.ApplyOperationID != schema.Status.PendingObservation.ApplyOperationID ||
				pending.Plan.TargetIdentityDigest != plannedTarget {
				t.Fatalf("the refused read ended the proof: %#v", pending)
			}
			if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operation ||
				actual.Status.ActiveOperation.Attempt != schema.Status.ActiveOperation.Attempt+1 {
				t.Fatalf("the read does not go on: %#v", actual.Status.ActiveOperation)
			}
			if proofLeaseHolder(t, reconciler, api) == "" {
				t.Fatal("the refused read released the proof's Lease")
			}
			failed := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
			if failed == nil || failed.Status != metav1.ConditionTrue || failed.Reason != string(operatorv1alpha1.ReasonOperationFailed) ||
				!strings.Contains(failed.Message, "invalid_target: ") || !strings.Contains(failed.Message, "xn-- form") {
				t.Fatalf("ReconciliationFailed = %#v, want OperationFailed naming invalid_target and the xn-- form", failed)
			}
		})
	}
}

// A read with no proof owed is how the resource learns where its URL points, so
// its Job names no target, and its runner reads whatever target the URL names.
func TestAnOrdinaryReadJobNamesNoTarget(t *testing.T) {
	t.Parallel()
	for _, operation := range []operatorv1alpha1.OperationType{operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			claim := &operatorv1alpha1.ActiveOperationStatus{
				Type: operation, ID: "ordinary-" + string(operation), Attempt: 1, StartedAt: metav1.Now(),
			}
			schema, policy := planClaimFixture(t, claim)
			builder, key := leaderBuilder(t)
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, policy)
			reconciler.Client = jobUIDByNameClient{Client: api}
			reconciler.Jobs = builder
			reconciler.SealKey = key

			job := reconcileUntilJobNamed(t, reconciler, api, schema, claim.JobName)
			if bound, ok := executorEnv(t, job)[runner.EnvExpectedTargetIdentityDigest]; ok {
				t.Fatalf("an ordinary read Job binds %#v", bound)
			}
		})
	}
}
