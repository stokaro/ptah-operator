package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// waitOn runs the wait kubectl ptah wait makes, over what the reconciler stored.
func waitOn(reader client.Reader, ref operate.Ref, options operate.WaitOptions) (operate.Result, error) {
	options.Timeout = time.Minute
	options.Interval = 10 * time.Second
	options.Clock = &waitClock{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	return operate.Wait(context.Background(), reader, ref, options)
}

func assertRefusalStopsWait(t *testing.T, err error, requirement string) {
	t.Helper()
	var stop *operate.StopError
	if !errors.As(err, &stop) || stop.Reason != operate.StopPolicyRefused || stop.Phase != "Blocked" ||
		!strings.Contains(stop.Message, requirement) {
		t.Fatalf("wait = %v, want a %s stop in phase Blocked naming %s", err, operate.StopPolicyRefused, requirement)
	}
}

// assertWaitKeepsRunning holds a wait to its timeout, which names what it read.
func assertWaitKeepsRunning(t *testing.T, err error) {
	t.Helper()
	var timedOut *operate.TimeoutError
	if !errors.As(err, &timedOut) {
		t.Fatalf("wait = %v, want it to keep running to its timeout", err)
	}
}

// refusalTargets are the waits a refusal stops.
var refusalTargets = []operate.Target{operate.TargetInSync, operate.TargetApproval, operate.TargetApplied}

// A migration the reconciler refused, read by the wait the command makes: the
// refusal of the artifact the resource names now stops every wait that cannot
// follow it, on what the reconciler stored rather than on a document written here.
func TestAWaitStopsOnARefusalTheMigrationReconcilerWrote(t *testing.T) {
	t.Parallel()
	for _, target := range refusalTargets {
		t.Run(string(target), func(t *testing.T) {
			t.Parallel()
			harness := newMigrationLifecycle(t, migrationFixture())
			clock := movableMigrationClock(harness.reconciler, harness.api)
			harness.refuse(clock)
			ref := operate.Ref{Family: operate.FamilyMigration, Namespace: harness.key.Namespace, Name: harness.key.Name}

			_, err := waitOn(harness.api, ref, operate.WaitOptions{Target: target})

			assertRefusalStopsWait(t, err, "require_signature")
		})
	}
}

// Each row leaves a refusal behind and takes the resource on from it: what the
// reconciler stores then is a refusal that no longer holds, and neither wait stops
// on it. The conditions #904 keeps or clears decide nothing here, since only
// ArtifactVerified says the artifact stands refused.
func TestAWaitDoesNotStopOnARefusalOfTheMigrationThatNoLongerHolds(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		move func(*testing.T, *migrationLifecycle)
	}{
		{"the spec was edited since", func(t *testing.T, harness *migrationLifecycle) {
			t.Helper()
			edited := harness.migration()
			edited.Spec.Interval = metav1.Duration{Duration: 2 * time.Hour}
			edited.Generation++
			if err := harness.api.Update(context.Background(), edited); err != nil {
				t.Fatal(err)
			}
		}},
		{"a read request the operator has not taken", func(t *testing.T, harness *migrationLifecycle) {
			t.Helper()
			requested := harness.migration()
			requested.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: requestedAt}
			if err := harness.api.Update(context.Background(), requested); err != nil {
				t.Fatal(err)
			}
		}},
		{"the next read claimed its Resolve", func(t *testing.T, harness *migrationLifecycle) {
			t.Helper()
			harness.claimed(operatorv1alpha1.MigrationOperationResolve)
		}},
		{"the same artifact resolved, and is not verified yet", func(t *testing.T, harness *migrationLifecycle) {
			t.Helper()
			harness.claimed(operatorv1alpha1.MigrationOperationResolve)
			harness.answer(resolvedTo(testDigest))
		}},
		{"its Verify is under way", func(t *testing.T, harness *migrationLifecycle) {
			t.Helper()
			harness.claimed(operatorv1alpha1.MigrationOperationResolve)
			harness.answer(resolvedTo(testDigest))
			harness.claimed(operatorv1alpha1.MigrationOperationVerify)
		}},
		{"its Verify could not answer", func(t *testing.T, harness *migrationLifecycle) {
			t.Helper()
			harness.claimed(operatorv1alpha1.MigrationOperationResolve)
			harness.answer(resolvedTo(testDigest))
			harness.claimed(operatorv1alpha1.MigrationOperationVerify)
			harness.answer(runner.Result{
				Operation: runner.OperationVerify, ChildExitCode: 1, ResolvedDigest: testDigest,
				Error: &runner.ResultError{Code: "child_exit", Message: "ptah exited with code 1"},
			})
		}},
		{"another digest resolved", func(t *testing.T, harness *migrationLifecycle) {
			t.Helper()
			harness.claimed(operatorv1alpha1.MigrationOperationResolve)
			harness.answer(resolvedTo(movedDigest))
		}},
		{"the artifact passed", func(t *testing.T, harness *migrationLifecycle) {
			t.Helper()
			harness.claimed(operatorv1alpha1.MigrationOperationResolve)
			harness.answer(resolvedTo(testDigest))
			harness.claimed(operatorv1alpha1.MigrationOperationVerify)
			harness.answer(acceptedBy(testDigest))
		}},
		{"the artifact passed, and the history read fails", func(t *testing.T, harness *migrationLifecycle) {
			t.Helper()
			harness.claimed(operatorv1alpha1.MigrationOperationResolve)
			harness.answer(resolvedTo(testDigest))
			harness.claimed(operatorv1alpha1.MigrationOperationVerify)
			harness.answer(acceptedBy(testDigest))
			harness.claimed(operatorv1alpha1.MigrationOperationHistory)
			harness.answer(runner.Result{
				Operation: runner.OperationMigrationHistory, ChildExitCode: 1,
				Error: &runner.ResultError{Code: "child_exit", Message: "ptah exited with code 1"},
			})
		}},
	} {
		for _, target := range []operate.Target{operate.TargetInSync, operate.TargetApproval} {
			t.Run(row.name+"/"+string(target), func(t *testing.T) {
				t.Parallel()
				harness := newMigrationLifecycle(t, migrationFixture())
				clock := movableMigrationClock(harness.reconciler, harness.api)
				harness.refuse(clock)
				row.move(t, harness)
				ref := operate.Ref{Family: operate.FamilyMigration, Namespace: harness.key.Namespace, Name: harness.key.Name}

				_, err := waitOn(harness.api, ref, operate.WaitOptions{Target: target})

				assertWaitKeepsRunning(t, err)
			})
		}
	}
}

// The next read's own refusal is one that holds, so the same wait stops on it.
func TestAWaitStopsOnTheRefusalAnotherReadOfTheMigrationEndsIn(t *testing.T) {
	t.Parallel()
	harness := newMigrationLifecycle(t, migrationFixture())
	clock := movableMigrationClock(harness.reconciler, harness.api)
	harness.refuse(clock)
	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(movedDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	ref := operate.Ref{Family: operate.FamilyMigration, Namespace: harness.key.Namespace, Name: harness.key.Name}
	_, err := waitOn(harness.api, ref, operate.WaitOptions{Target: operate.TargetInSync})
	assertWaitKeepsRunning(t, err)

	harness.answer(refusedBy(movedDigest))

	_, err = waitOn(harness.api, ref, operate.WaitOptions{Target: operate.TargetInSync})
	assertRefusalStopsWait(t, err, "require_signature")
}

// A resource that recovers reaches what the wait is for, past a refusal the wait
// began on.
func TestAWaitReachesInSyncPastARefusalOfTheMigration(t *testing.T) {
	t.Parallel()
	harness := newMigrationLifecycle(t, migrationFixture())
	clock := movableMigrationClock(harness.reconciler, harness.api)
	harness.refuse(clock)
	refused := harness.migration()
	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(acceptedBy(testDigest))
	clock.now = clock.now.Add(time.Minute)
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	harness.answer(historyAnswer(harness, inSyncReport()))
	ref := operate.Ref{Family: operate.FamilyMigration, Namespace: harness.key.Namespace, Name: harness.key.Name}

	result, err := waitOn(harness.api, ref, operate.WaitOptions{Target: operate.TargetInSync, Since: refused})

	if err != nil || result.Phase != "InSync" {
		t.Fatalf("wait = %+v, %v; want it to reach in-sync", result, err)
	}
}

// The window a read request leaves. The pass that answers it writes the answer
// and brings the next read forward, and the claim comes one pass later, so one
// document shows the request answered beside the refusal the read is about to
// test again. A wait that reads it next reads the claim, and goes on.
func TestAWaitDoesNotStopBetweenAnsweringARequestAndClaimingItsRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	harness := newMigrationLifecycle(t, migrationFixture())
	clock := movableMigrationClock(harness.reconciler, harness.api)
	harness.refuse(clock)
	requested := harness.migration()
	requested.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: requestedAt}
	if err := harness.api.Update(ctx, requested); err != nil {
		t.Fatal(err)
	}
	answered := reconcileMigration(t, harness.reconciler, harness.api, requested)
	if answered.Status.LastReadRequest != requestedAt || answered.Status.ActiveOperation != nil {
		t.Fatalf("lastReadRequest %q, operation %#v; want the request answered and no claim yet",
			answered.Status.LastReadRequest, answered.Status.ActiveOperation)
	}

	// The operator's next pass runs between the wait's first read and its second.
	reads := 0
	operator := interceptor.NewClient(harness.api.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			reads++
			if reads == 2 {
				reconcileMigration(t, harness.reconciler, harness.api, requested)
			}
			return c.Get(ctx, key, object, options...)
		},
	})
	ref := operate.Ref{Family: operate.FamilyMigration, Namespace: harness.key.Namespace, Name: harness.key.Name}

	_, err := waitOn(operator, ref, operate.WaitOptions{Target: operate.TargetInSync})

	assertWaitKeepsRunning(t, err)
	if claimed := harness.migration(); claimed.Status.ActiveOperation == nil {
		t.Fatal("the row ran no claim, so it measured nothing")
	}
}

// refusedSchema is a schema the reconciler refused, from a Verify Job the policy
// answered.
func refusedSchema(t *testing.T) (*operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client) {
	t.Helper()
	policyBytes := []byte("require_signature: true\n")
	policyDigest := fingerprint.DigestBytes(policyBytes)
	schema := schemaFixture()
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseVerifying
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		RequestedReference: schema.Spec.Desired.OCIRef,
		ResolvedReference:  "oci://registry.example/team/schema@" + testDigest,
		Digest:             testDigest,
		MediaType:          "application/vnd.oci.image.manifest.v1+json",
		Size:               321,
	}
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationVerify, ID: testDigest,
		JobName: "verify-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
		VerificationPolicyUID: testPolicyUID, VerificationPolicyDigest: policyDigest,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationVerify, OperationID: testDigest,
		ChildExitCode: 2, ResolvedDigest: testDigest,
		VerificationRequirements: []string{"require_signature"}, VerificationPolicyDigest: policyDigest,
		Error: &runner.ResultError{Code: "verification_refused", Message: "artifact does not satisfy the verification policy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: schema.Spec.Desired.VerificationPolicyFrom.Name, UID: testPolicyUID},
		Immutable:  ptr(true),
		Data:       map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: string(policyBytes)},
	}
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod, policy)
	refused := reconcileSchema(t, reconciler, api, schema)
	if refused.Status.Phase != operatorv1alpha1.PhaseBlocked || refused.Status.ActiveOperation != nil {
		t.Fatalf("the fixture did not refuse: phase %s, claim %#v", refused.Status.Phase, refused.Status.ActiveOperation)
	}
	return refused, reconciler, api
}

func TestAWaitStopsOnARefusalTheSchemaReconcilerWrote(t *testing.T) {
	t.Parallel()
	for _, target := range refusalTargets {
		t.Run(string(target), func(t *testing.T) {
			t.Parallel()
			refused, _, api := refusedSchema(t)
			ref := operate.Ref{Family: operate.FamilySchema, Namespace: refused.Namespace, Name: refused.Name}

			_, err := waitOn(api, ref, operate.WaitOptions{Target: target})

			assertRefusalStopsWait(t, err, "require_signature")
		})
	}
}

func TestAWaitDoesNotStopOnARefusalOfTheSchemaThatNoLongerHolds(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		move func(*testing.T, *operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client)
	}{
		{"the spec was edited since", func(t *testing.T, schema *operatorv1alpha1.PtahSchema, _ *SchemaReconciler, api client.Client) {
			t.Helper()
			edited := storedSchema(t, api, schema)
			edited.Spec.Interval = metav1.Duration{Duration: 2 * time.Hour}
			edited.Generation++
			if err := api.Update(context.Background(), edited); err != nil {
				t.Fatal(err)
			}
		}},
		{"a read request the operator has not taken", func(t *testing.T, schema *operatorv1alpha1.PtahSchema, _ *SchemaReconciler, api client.Client) {
			t.Helper()
			annotateSchema(t, api, schema, requestedAt)
		}},
		{"the next read claimed its Resolve", func(t *testing.T, schema *operatorv1alpha1.PtahSchema, reconciler *SchemaReconciler, api client.Client) {
			t.Helper()
			clock := movableSchemaClock(reconciler, api)
			clock.now = schema.Status.NextReconciliationTime.Add(time.Second)
			claimed := reconcileSchema(t, reconciler, api, schema)
			if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
				t.Fatalf("the row claimed %#v, want a Resolve", claimed.Status.ActiveOperation)
			}
		}},
	} {
		for _, target := range []operate.Target{operate.TargetInSync, operate.TargetApproval} {
			t.Run(row.name+"/"+string(target), func(t *testing.T) {
				t.Parallel()
				refused, reconciler, api := refusedSchema(t)
				row.move(t, refused, reconciler, api)
				ref := operate.Ref{Family: operate.FamilySchema, Namespace: refused.Namespace, Name: refused.Name}

				_, err := waitOn(api, ref, operate.WaitOptions{Target: target})

				assertWaitKeepsRunning(t, err)
			})
		}
	}
}

// kubectl ptah set-source --wait from a refused artifact to another one. The answer
// to the change carries the old refusal at the old generation, and it does not
// stop the wait. The new artifact's own refusal does.
func TestSetSourceWaitStopsOnARefusalOfTheNewArtifactAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := "oci://registry.example/team/migrations@"
	migration := migrationFixture()
	migration.Spec.Artifact.OCIRef = repository + testDigest
	harness := newMigrationLifecycle(t, migration)
	clock := movableMigrationClock(harness.reconciler, harness.api)
	harness.refuse(clock)

	bumping := interceptor.NewClient(harness.api.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			obj.SetGeneration(obj.GetGeneration() + 1)
			if err := c.Update(ctx, obj); err != nil {
				return err
			}
			return c.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		},
	})
	ref := operate.Ref{Family: operate.FamilyMigration, Namespace: migration.Namespace, Name: migration.Name}
	pinned, err := operate.ParsePinnedReference(repository + movedDigest)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := operate.SetSource(ctx, bumping, ref, pinned)
	if err != nil || !changed.Changed {
		t.Fatalf("set-source = %+v, %v; want a change", changed, err)
	}
	options := operate.WaitOptions{Target: operate.TargetPlanned, Since: changed.Object, Generation: changed.Generation, Source: pinned.Reference}

	_, err = waitOn(harness.api, ref, options)
	assertWaitKeepsRunning(t, err)

	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(movedDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(refusedBy(movedDigest))

	_, err = waitOn(harness.api, ref, options)
	assertRefusalStopsWait(t, err, "require_signature")
}

// kubectl ptah approve --wait when the read that was due refuses the artifact. The
// approval goes stale with the refusal, so the run the wait names will not come.
func TestApproveWaitStopsOnARefusalThatWithdrewItsApproval(t *testing.T) {
	t.Parallel()
	harness, clock, plan := waitingForAnApproval(t)
	waiting := harness.migration()
	approval := migrationApprovalFor(waiting, plan)
	if err := harness.api.Create(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	harness.dueNow(clock)
	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(refusedBy(testDigest))
	ref := operate.Ref{Family: operate.FamilyMigration, Namespace: waiting.Namespace, Name: waiting.Name}

	_, err := waitOn(harness.api, ref, operate.WaitOptions{
		Target: operate.TargetApplied, Since: waiting,
		Decision: &operate.Decision{PlanName: plan.Name, PlanUID: plan.UID, ApprovalName: approval.Name, ApprovalUID: approval.UID},
	})

	assertRefusalStopsWait(t, err, "require_signature")
}
