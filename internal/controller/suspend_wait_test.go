package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// waitClock is the clock kubectl ptah wait measures its timeout by. Its
// sleeps return at once, so a wait that cannot end reports its timeout
// without taking it.
type waitClock struct{ now time.Time }

func (c *waitClock) Now() time.Time { return c.now }

func (c *waitClock) After(d time.Duration) <-chan time.Time {
	c.now = c.now.Add(d)
	fired := make(chan time.Time, 1)
	fired <- c.now
	return fired
}

// kubectl ptah suspend --wait on a schema that owes, or is about to owe, the
// proof of an Apply. The reconciler runs from the moment the suspension is
// stored into the state it settles in, and the wait the command makes then
// reads that state. In each the controller writes phase Suspended and the
// Suspended condition for the suspension's generation and leaves
// status.observedGeneration at the Apply's, where the proof keeps it; a wait
// that read observedGeneration could only time out.
//
// Each row also holds operate's account of what the suspension does to the
// work in flight (SuspendResult.Effect) to what the controller did.
func TestSuspendWaitReachesASchemaThatOwesProof(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		fixture func(t *testing.T) (*operatorv1alpha1.PtahSchema, []client.Object, staticLogs)
		effect  operate.Effect
		passes  int
		// settled holds what the controller did to the work in flight.
		settled func(t *testing.T, actual *operatorv1alpha1.PtahSchema)
	}{
		{
			// The suspension moves the generation the running Apply was
			// decided from: the harvest records it unknown and owes a proof.
			name: "an Apply running when the suspension arrived",
			fixture: func(t *testing.T) (*operatorv1alpha1.PtahSchema, []client.Object, staticLogs) {
				schema := schemaFixture()
				schema.Finalizers = []string{activeOperationFinalizer}
				schema.Status.Phase = operatorv1alpha1.PhaseApplying
				schema.Status.Target.CoordinationDigest = testCoordinationDigest
				schema.Status.Target.IdentityDigest = testDigest
				schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
					Fingerprint: testDigest, ContentDigest: testDigest, CoordinationDigest: testCoordinationDigest,
				}
				schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
					Type: operatorv1alpha1.OperationApply, ID: testDigest, InputFingerprint: testDigest,
					JobName: "apply-job", JobUID: "job-uid", StartedAt: metav1.NewTime(time.Now().Add(-time.Minute)), Attempt: 1,
				}
				bindActiveInput(t, schema)
				schema.Generation++
				schema.Spec.Suspend = true
				job, pod := terminalWorkload(schema, batchv1.JobComplete)
				return schema, []client.Object{schema, job, pod}, staticLogs{}
			},
			effect: operate.EffectApplyRuns,
			passes: 4,
			settled: func(t *testing.T, actual *operatorv1alpha1.PtahSchema) {
				t.Helper()
				if pending := actual.Status.PendingObservation; pending == nil ||
					pending.Outcome != operatorv1alpha1.PendingObservationOutcomeUnknown || actual.Status.Applied != nil {
					t.Fatalf("the Apply was not recorded unknown with its proof owed and no applied plan: %#v", actual.Status)
				}
			},
		},
		{
			name: "the proof owed and not claimed",
			fixture: func(t *testing.T) (*operatorv1alpha1.PtahSchema, []client.Object, staticLogs) {
				schema := safetyPostApplyObserveSchema(t)
				schema.Status.ActiveOperation = nil
				schema.Generation++
				schema.Spec.Suspend = true
				return schema, []client.Object{schema}, staticLogs{}
			},
			effect: operate.EffectProofWaits,
			passes: 2,
			settled: func(t *testing.T, actual *operatorv1alpha1.PtahSchema) {
				t.Helper()
				if actual.Status.ActiveOperation != nil || actual.Status.PendingObservation == nil {
					t.Fatalf("a proof Job was claimed while suspended: %#v", actual.Status)
				}
			},
		},
		{
			// A running proof Observe outranks suspension: it ends, and the
			// Plan that completes the proof waits for resume.
			name: "the proof's Observe running",
			fixture: func(t *testing.T) (*operatorv1alpha1.PtahSchema, []client.Object, staticLogs) {
				schema := safetyPostApplyObserveSchema(t)
				schema.Generation++
				schema.Spec.Suspend = true
				job, pod := terminalWorkload(schema, batchv1.JobComplete)
				frame := safetyRunnerFrame(t, runner.Result{
					ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationObserve,
					OperationID: schema.Status.ActiveOperation.ID, ChildExitCode: 0,
					CoordinationDigest:   schema.Status.PendingObservation.CoordinationDigest,
					TargetIdentityDigest: schema.Status.PendingObservation.Plan.TargetIdentityDigest,
					DriftReportDigest:    testDigest, ObservedDialect: "postgresql",
				})
				return schema, []client.Object{schema, job, pod}, staticLogs{content: frame}
			},
			effect: operate.EffectProofObserveRuns,
			passes: 3,
			settled: func(t *testing.T, actual *operatorv1alpha1.PtahSchema) {
				t.Helper()
				if pending := actual.Status.PendingObservation; actual.Status.ActiveOperation != nil || pending == nil ||
					!pending.PlanRequired || actual.Status.Target.LastObservedAt == nil {
					t.Fatalf("the proof's Observe did not end with its Plan left for resume: %#v", actual.Status)
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema, objects, logs := row.fixture(t)
			if effect := (operate.SuspendResult{Object: schema.DeepCopy()}).Effect(); effect != row.effect {
				t.Fatalf("operate says the suspension means %s, want %s", effect, row.effect)
			}
			reconciler, api := fakeReconciler(t, logs, objects...)
			reconciler.Locks = targetlock.New(api, api, nil)
			baseline := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), baseline); err != nil {
				t.Fatal(err)
			}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
			for range row.passes {
				if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
			}

			actual := &operatorv1alpha1.PtahSchema{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
				t.Fatal(err)
			}
			row.settled(t, actual)
			suspended := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionSuspended)
			if actual.Status.Phase != operatorv1alpha1.PhaseSuspended || suspended == nil ||
				suspended.Status != metav1.ConditionTrue || suspended.ObservedGeneration != actual.Generation ||
				actual.Status.ObservedGeneration >= actual.Generation || actual.Status.PendingObservation == nil {
				t.Fatalf("the fixture did not settle Suspended with the proof owed and an older observedGeneration: "+
					"phase %s, observedGeneration %d of %d, Suspended %#v",
					actual.Status.Phase, actual.Status.ObservedGeneration, actual.Generation, suspended)
			}

			// What kubectl ptah suspend --wait asks for: the generation its
			// patch answered, from that answer as the baseline.
			result, err := operate.Wait(context.Background(), api,
				operate.Ref{Family: operate.FamilySchema, Namespace: schema.Namespace, Name: schema.Name},
				operate.WaitOptions{
					Target: operate.TargetSuspended, Timeout: 10 * time.Minute, Since: baseline, Generation: baseline.Generation,
					Clock: &waitClock{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
				})
			if err != nil || result.Reads != 1 {
				t.Fatalf("suspend --wait on this state = %+v, %v; want suspended at the first read", result, err)
			}
		})
	}
}
