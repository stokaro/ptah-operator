package operate_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
)

// The targets the fixtures' proof knows: the one its plan was made for, the one
// the URL Secret names now, and one the store moves on to.
var (
	planTarget  = "sha256:" + strings.Repeat("a", 64)
	movedTarget = "sha256:" + strings.Repeat("b", 64)
	thirdTarget = "sha256:" + strings.Repeat("c", 64)
)

const (
	applyID   = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	proofPlan = "storefront-plan"
)

func settleValue(target string) string { return applyID + "/" + target }

// stopMessage is what a stop on target prints for the fixture: the words the
// controller writes, then the command.
func stopMessage(target string) string {
	return "the URL Secret now holds another target than the plan this Apply ran from: another login, host, port, " +
		"database or URL parameter, so no read can say what the Apply did; put the first URL back and the proof " +
		"finishes, or give the proof up, which reads the database under the new URL and, under apply: Always, applies " +
		"the next plan with no approval unless it is destructive or changes privileges: kubectl annotate ptahschema " +
		name + " -n " + namespace + " " + operatorv1alpha1.SettledApplyAnnotation + "=" + settleValue(target) + " --overwrite"
}

// stoppedSchema is a schema whose proof read target and stopped there, with
// something in every part a settlement must leave alone.
func stoppedSchema(target string, annotations map[string]string) *operatorv1alpha1.PtahSchema {
	schema := &operatorv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: name, UID: uidA, Generation: 3,
		Labels: map[string]string{"app.kubernetes.io/part-of": "shop"}, Annotations: annotations,
	}}
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	schema.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: 45 * time.Second}
	schema.Spec.Desired.OCIRef = "oci://registry.example/shop/schema@" + planTarget
	status := &schema.Status
	status.ObservedGeneration, status.Phase = 3, operatorv1alpha1.PhaseBlocked
	status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
		Outcome: operatorv1alpha1.PendingObservationApplySucceeded, ApplyOperationID: applyID,
		Plan: operatorv1alpha1.CurrentPlanStatus{Name: proofPlan, UID: proofPlan + "-uid", TargetIdentityDigest: planTarget},
	}
	status.Plan = &operatorv1alpha1.CurrentPlanStatus{Name: proofPlan, UID: proofPlan + "-uid",
		Approval: &operatorv1alpha1.ConsumedApprovalStatus{Name: "first-approval", UID: "first-approval-uid"}}
	status.Conditions = []metav1.Condition{
		condition(operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionTrue,
			string(operatorv1alpha1.ReasonTargetIdentityChanged), stopMessage(target), 3),
		condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, string(operatorv1alpha1.ReasonTargetIdentityChanged),
			"The proof of the last Apply needs a person: ReconciliationFailed says what to do", 3),
	}
	status.Journal = &operatorv1alpha1.LifecycleJournal{CoveredSince: metav1.NewTime(at(-86400)), Entries: []operatorv1alpha1.JournalEntry{
		{Type: operatorv1alpha1.JournalApplyFinished, At: metav1.NewTime(at(-600)), Plan: proofPlan, Outcome: operatorv1alpha1.JournalSucceeded},
		{Type: operatorv1alpha1.JournalOperationFailed, At: metav1.NewTime(at(-300)), Operation: "Observe",
			Reason: string(operatorv1alpha1.ReasonTargetIdentityChanged)},
	}}
	return schema
}

func schemaRef() operate.Ref { return refOf(operate.FamilySchema) }

// settleClient is the API server with a hook that runs before each patch, the way
// another writer lands between Settle's read and its write.
type settleClient struct {
	client.Client
	mu      sync.Mutex
	patches int
	// before runs before the next patch, and every before each one.
	before func(context.Context, client.Client)
	every  func(context.Context, client.Client)
}

func newSettleClient(t *testing.T, objects ...client.Object) *settleClient {
	t.Helper()
	c := &settleClient{}
	c.Client = fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithStatusSubresource(&operatorv1alpha1.PtahSchema{}, &operatorv1alpha1.PtahMigration{}).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, inner client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
				c.mu.Lock()
				c.patches++
				before, every := c.before, c.every
				c.before = nil
				c.mu.Unlock()
				if before != nil {
					before(ctx, inner)
				}
				if every != nil {
					every(ctx, inner)
				}
				return inner.Patch(ctx, object, patch, options...)
			},
		}).Build()
	return c
}

func (c *settleClient) patchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.patches
}

func (c *settleClient) stored(t *testing.T) *operatorv1alpha1.PtahSchema {
	t.Helper()
	schema := &operatorv1alpha1.PtahSchema{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// rewriteStatus is another writer's status write between Settle's read and its
// patch.
func rewriteStatus(t *testing.T, change func(*operatorv1alpha1.PtahSchemaStatus)) func(context.Context, client.Client) {
	return func(ctx context.Context, c client.Client) {
		schema := &operatorv1alpha1.PtahSchema{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, schema); err != nil {
			t.Error(err)
			return
		}
		change(&schema.Status)
		if err := c.Status().Update(ctx, schema); err != nil {
			t.Error(err)
		}
	}
}

func TestPrintedSettleValueReadsTheCommandTheStopEndsWith(t *testing.T) {
	t.Parallel()
	value, err := operate.PrintedSettleValue(stopMessage(movedTarget), name, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if want := (operate.SettleValue{Value: settleValue(movedTarget), ApplyOperationID: applyID, TargetDigest: movedTarget}); value != want {
		t.Fatalf("read %#v, want %#v", value, want)
	}
	command := "kubectl annotate ptahschema " + name + " -n " + namespace + " " +
		operatorv1alpha1.SettledApplyAnnotation + "=" + settleValue(movedTarget) + " --overwrite"
	for _, row := range []struct{ name, message string }{
		{"no command", "the URL Secret now holds another target"},
		{"another schema", strings.Replace(command, " "+name+" ", " checkout ", 1)},
		{"another namespace", strings.Replace(command, " -n "+namespace+" ", " -n shop ", 1)},
		{"another kind", strings.Replace(command, "ptahschema", "ptahmigration", 1)},
		{"another annotation", strings.Replace(command, operatorv1alpha1.SettledApplyAnnotation, "operator.ptah.run/read-requested-at", 1)},
		{"no overwrite", strings.TrimSuffix(command, " --overwrite")},
		{"words after it", command + " to settle it"},
		{"a message cut short", command[:len(command)-len("b --overwrite")]},
		{"no target", strings.Replace(command, "/"+movedTarget, "", 1)},
		{"no Apply", strings.Replace(command, applyID, "", 1)},
		{"a digest that is not one", strings.Replace(command, movedTarget, "sha256:short", 1)},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if value, err := operate.PrintedSettleValue(row.message, name, namespace); err == nil {
				t.Fatalf("read %#v from %q", value, row.message)
			}
		})
	}
}

// Settle writes the value the stop prints into the annotation and nothing else,
// over a value that settled nothing, and sends nothing when the annotation holds it.
func TestSettleWritesTheValueTheStopPrints(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name        string
		annotations map[string]string
		replaced    bool
	}{
		{name: "no annotations at all"},
		{name: "beside another annotation", annotations: map[string]string{"team": "shop"}},
		{name: "over a value that settled nothing", annotations: map[string]string{
			"team": "shop", operatorv1alpha1.SettledApplyAnnotation: applyID}, replaced: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			api := newSettleClient(t, stoppedSchema(movedTarget, row.annotations))

			result, err := operate.Settle(context.Background(), api, schemaRef(), operate.SettleOptions{})

			if err != nil {
				t.Fatal(err)
			}
			if !result.Changed || result.Held != row.replaced || result.Printed.Value != settleValue(movedTarget) ||
				result.Plan != proofPlan || result.Approval != "first-approval" || result.PlanTarget != planTarget ||
				result.Apply != operatorv1alpha1.ApplyPolicyAlways || result.RetryInterval != 45*time.Second ||
				result.Journal == nil || result.Journal.Outcome != operatorv1alpha1.JournalSucceeded {
				t.Fatalf("Settle = %#v", result)
			}
			schema := api.stored(t)
			want := map[string]string{operatorv1alpha1.SettledApplyAnnotation: settleValue(movedTarget)}
			for key, value := range row.annotations {
				if key != operatorv1alpha1.SettledApplyAnnotation {
					want[key] = value
				}
			}
			if len(schema.Annotations) != len(want) {
				t.Fatalf("annotations = %v, want %v", schema.Annotations, want)
			}
			for key, value := range want {
				if schema.Annotations[key] != value {
					t.Fatalf("annotations = %v, want %v", schema.Annotations, want)
				}
			}
			if schema.Labels["app.kubernetes.io/part-of"] != "shop" || schema.Spec.Policy.Apply != operatorv1alpha1.ApplyPolicyAlways ||
				schema.Status.PendingObservation == nil {
				t.Fatalf("Settle changed more than the annotation: %#v", schema)
			}

			again, err := operate.Settle(context.Background(), api, schemaRef(), operate.SettleOptions{})
			if err != nil || again.Changed || !again.Held || api.patchCount() != 1 {
				t.Fatalf("Settle again = %#v, %v after %d patches; want nothing sent", again, err, api.patchCount())
			}
		})
	}
}

// Each row is a resource the controller would take no statement from, or a
// value it would not take. Settle sends nothing to any of them.
func TestSettleRefusesWhatTheControllerWouldNotTake(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		change func(*operatorv1alpha1.PtahSchema)
		refuse func(error) bool
		says   string
	}{
		{
			name:   "no proof owed",
			change: func(s *operatorv1alpha1.PtahSchema) { s.Status.PendingObservation = nil },
			refuse: notStopped, says: "it owes no proof of an Apply",
		},
		{
			name:   "no ReconciliationFailed",
			change: func(s *operatorv1alpha1.PtahSchema) { s.Status.Conditions = s.Status.Conditions[1:] },
			refuse: notStopped, says: "reports no ReconciliationFailed condition",
		},
		{
			name:   "the stop cleared",
			change: func(s *operatorv1alpha1.PtahSchema) { s.Status.Conditions[0].Status = metav1.ConditionFalse },
			refuse: notStopped, says: "ReconciliationFailed is False with reason TargetIdentityChanged",
		},
		{
			name: "a proof failing on its own target",
			change: func(s *operatorv1alpha1.PtahSchema) {
				s.Status.Conditions[0].Reason, s.Status.Conditions[0].Message = string(operatorv1alpha1.ReasonOperationFailed), "the server went away"
			},
			refuse: notStopped, says: "ReconciliationFailed is True with reason OperationFailed",
		},
		{
			name: "no command in the message",
			change: func(s *operatorv1alpha1.PtahSchema) {
				s.Status.Conditions[0].Message = "the URL Secret now holds another target"
			},
			refuse: noValue, says: "prints no kubectl annotate command",
		},
		{
			name: "a message cut short",
			change: func(s *operatorv1alpha1.PtahSchema) {
				s.Status.Conditions[0].Message = strings.TrimSuffix(s.Status.Conditions[0].Message, " --overwrite")
			},
			refuse: noValue, says: "another command than the one that settles",
		},
		{
			name: "the value of another Apply",
			change: func(s *operatorv1alpha1.PtahSchema) {
				s.Status.PendingObservation.ApplyOperationID = "sha256:" + strings.Repeat("2", 64)
			},
			refuse: stale, says: "and the proof owed is Apply sha256:2222",
		},
		{
			name: "a value for the plan's own target",
			change: func(s *operatorv1alpha1.PtahSchema) {
				s.Status.PendingObservation.Plan.TargetIdentityDigest = movedTarget
			},
			refuse: stale, says: "names the target the plan was made for",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema := stoppedSchema(movedTarget, nil)
			row.change(schema)
			api := newSettleClient(t, schema)

			_, err := operate.Settle(context.Background(), api, schemaRef(), operate.SettleOptions{})

			if !row.refuse(err) || !strings.Contains(err.Error(), row.says) {
				t.Fatalf("Settle = %v, want a refusal that says %q", err, row.says)
			}
			if api.patchCount() != 0 {
				t.Fatalf("a refused settlement sent %d patches", api.patchCount())
			}
			if operate.Classify(err) != operate.NotRead {
				t.Fatalf("Classify = %s for a refusal nothing sent", operate.Classify(err))
			}
		})
	}

	t.Run("a migration", func(t *testing.T) {
		t.Parallel()
		api := newSettleClient(t)
		if _, err := operate.Settle(context.Background(), api, refOf(operate.FamilyMigration), operate.SettleOptions{}); err == nil || api.patchCount() != 0 {
			t.Fatalf("Settle on a migration = %v after %d patches", err, api.patchCount())
		}
	})
}

// An expected value binds the statement to the stop the person read. The stop
// the command reads has to print it, on every read Settle makes.
func TestSettleWritesOnlyTheValueExpected(t *testing.T) {
	t.Parallel()
	t.Run("the stop prints it", func(t *testing.T) {
		t.Parallel()
		api := newSettleClient(t, stoppedSchema(movedTarget, nil))

		result, err := operate.Settle(context.Background(), api, schemaRef(), operate.SettleOptions{Expect: settleValue(movedTarget)})

		if err != nil || !result.Changed || api.stored(t).Annotations[operatorv1alpha1.SettledApplyAnnotation] != settleValue(movedTarget) {
			t.Fatalf("Settle = %#v, %v; want the expected value written", result, err)
		}
	})
	for _, row := range []struct {
		name   string
		stored *operatorv1alpha1.PtahSchema
		before func(context.Context, client.Client)
		sent   int
	}{
		{name: "the stop prints another target", stored: stoppedSchema(thirdTarget, nil)},
		{name: "the stop prints another target and the annotation already holds it",
			stored: stoppedSchema(thirdTarget, map[string]string{operatorv1alpha1.SettledApplyAnnotation: settleValue(thirdTarget)})},
		{name: "a read moved the stop after Settle read it", stored: stoppedSchema(movedTarget, nil), sent: 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			api := newSettleClient(t, row.stored)
			if row.sent > 0 {
				api.before = rewriteStatus(t, func(status *operatorv1alpha1.PtahSchemaStatus) {
					status.Conditions[0].Message = stopMessage(thirdTarget)
				})
			}
			held := row.stored.Annotations[operatorv1alpha1.SettledApplyAnnotation]

			_, err := operate.Settle(context.Background(), api, schemaRef(), operate.SettleOptions{Expect: settleValue(movedTarget)})

			var unexpected *operate.UnexpectedSettleValueError
			if !errors.As(err, &unexpected) || unexpected.Printed != settleValue(thirdTarget) || unexpected.Expected != settleValue(movedTarget) {
				t.Fatalf("Settle = %v, want the value of target c refused against the one expected", err)
			}
			if got := api.stored(t).Annotations[operatorv1alpha1.SettledApplyAnnotation]; got != held || api.patchCount() != row.sent {
				t.Fatalf("Settle left %q after %d patches, want %q after %d", got, api.patchCount(), held, row.sent)
			}
		})
	}
	t.Run("a value that names no target", func(t *testing.T) {
		t.Parallel()
		api := newSettleClient(t, stoppedSchema(movedTarget, nil))

		_, err := operate.Settle(context.Background(), api, schemaRef(), operate.SettleOptions{Expect: applyID})

		if err == nil || api.patchCount() != 0 {
			t.Fatalf("Settle with the Apply's ID alone expected = %v after %d patches", err, api.patchCount())
		}
	})
}

func notStopped(err error) bool {
	var target *operate.NotStoppedError
	return errors.As(err, &target)
}

func noValue(err error) bool {
	var target *operate.NoSettleValueError
	return errors.As(err, &target)
}

func stale(err error) bool {
	var target *operate.StaleSettleValueError
	return errors.As(err, &target)
}

// Another writer between the read and the patch: the patch carries the
// resourceVersion it read, so the API server refuses it, and Settle decides again
// from a fresh read. A stop that prints another value now is not the one this
// command read, and nothing is written over it.
func TestSettleDecidesAgainAfterAConflict(t *testing.T) {
	t.Parallel()
	t.Run("a status write that changed nothing it reads", func(t *testing.T) {
		t.Parallel()
		api := newSettleClient(t, stoppedSchema(movedTarget, nil))
		api.before = rewriteStatus(t, func(status *operatorv1alpha1.PtahSchemaStatus) { status.ObservedGeneration++ })

		result, err := operate.Settle(context.Background(), api, schemaRef(), operate.SettleOptions{})

		if err != nil || !result.Changed || api.patchCount() != 2 {
			t.Fatalf("Settle = %#v, %v after %d patches; want it written on the second", result, err, api.patchCount())
		}
		if held := api.stored(t).Annotations[operatorv1alpha1.SettledApplyAnnotation]; held != settleValue(movedTarget) {
			t.Fatalf("the annotation holds %q", held)
		}
	})
	t.Run("a read that stopped on another target", func(t *testing.T) {
		t.Parallel()
		api := newSettleClient(t, stoppedSchema(movedTarget, nil))
		api.before = rewriteStatus(t, func(status *operatorv1alpha1.PtahSchemaStatus) {
			status.Conditions[0].Message = stopMessage(thirdTarget)
		})

		_, err := operate.Settle(context.Background(), api, schemaRef(), operate.SettleOptions{})

		if !stale(err) || !strings.Contains(err.Error(), "a read named another target since") {
			t.Fatalf("Settle = %v, want the moved stop refused", err)
		}
		if operate.Classify(err) != operate.Refused {
			t.Fatalf("Classify = %s, want the conflict read as refused", operate.Classify(err))
		}
		if _, held := api.stored(t).Annotations[operatorv1alpha1.SettledApplyAnnotation]; held || api.patchCount() != 1 {
			t.Fatalf("Settle wrote over a stop it did not read, after %d patches", api.patchCount())
		}
	})
	t.Run("a resource that keeps changing", func(t *testing.T) {
		t.Parallel()
		api := newSettleClient(t, stoppedSchema(movedTarget, nil))
		api.every = rewriteStatus(t, func(status *operatorv1alpha1.PtahSchemaStatus) { status.ObservedGeneration++ })

		_, err := operate.Settle(context.Background(), api, schemaRef(), operate.SettleOptions{})

		if !apierrors.IsConflict(err) || operate.Classify(err) != operate.Refused || api.patchCount() != 3 {
			t.Fatalf("Settle = %v (%s) after %d patches; want the third conflict refused", err, operate.Classify(err), api.patchCount())
		}
		if _, held := api.stored(t).Annotations[operatorv1alpha1.SettledApplyAnnotation]; held {
			t.Fatal("Settle wrote the annotation through three conflicts")
		}
	})
	t.Run("a proof that ended", func(t *testing.T) {
		t.Parallel()
		api := newSettleClient(t, stoppedSchema(movedTarget, nil))
		api.before = rewriteStatus(t, func(status *operatorv1alpha1.PtahSchemaStatus) { status.PendingObservation = nil })

		_, err := operate.Settle(context.Background(), api, schemaRef(), operate.SettleOptions{})

		if !notStopped(err) || operate.Classify(err) != operate.Refused {
			t.Fatalf("Settle = %v (%s), want it refused as not stopped", err, operate.Classify(err))
		}
	})
	t.Run("another person wrote the same value", func(t *testing.T) {
		t.Parallel()
		api := newSettleClient(t, stoppedSchema(movedTarget, map[string]string{"team": "shop"}))
		api.before = func(ctx context.Context, c client.Client) {
			schema := &operatorv1alpha1.PtahSchema{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, schema); err != nil {
				t.Error(err)
				return
			}
			schema.Annotations[operatorv1alpha1.SettledApplyAnnotation] = settleValue(movedTarget)
			if err := c.Update(ctx, schema); err != nil {
				t.Error(err)
			}
		}

		result, err := operate.Settle(context.Background(), api, schemaRef(), operate.SettleOptions{})

		if err != nil || result.Changed || !result.Held || api.patchCount() != 1 {
			t.Fatalf("Settle = %#v, %v after %d patches; want the value found there", result, err, api.patchCount())
		}
	})
	t.Run("the resource replaced", func(t *testing.T) {
		t.Parallel()
		api := newSettleClient(t, stoppedSchema(movedTarget, nil))
		api.before = func(ctx context.Context, c client.Client) {
			old := stoppedSchema(movedTarget, nil)
			if err := c.Delete(ctx, old); err != nil {
				t.Error(err)
				return
			}
			again := stoppedSchema(movedTarget, nil)
			again.UID = uidB
			if err := c.Create(ctx, again); err != nil {
				t.Error(err)
			}
		}

		_, err := operate.Settle(context.Background(), api, schemaRef(), operate.SettleOptions{})

		if !errors.Is(err, operate.ErrReplaced) || operate.Classify(err) != operate.Replaced {
			t.Fatalf("Settle = %v, want the replaced resource refused", err)
		}
	})
}

// schemaTimeline serves one document per read, and the last one to every read
// after it.
type schemaTimeline struct {
	mu    sync.Mutex
	docs  []*operatorv1alpha1.PtahSchema
	reads int
}

func (tl *schemaTimeline) reader(t *testing.T) client.Reader {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithStatusSubresource(&operatorv1alpha1.PtahSchema{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				tl.mu.Lock()
				next := tl.docs[min(tl.reads, len(tl.docs)-1)]
				tl.reads++
				tl.mu.Unlock()
				if err := replace(ctx, c, next.DeepCopy()); err != nil {
					return err
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
}

// settled is the status write that gives the proof up, journal entry and all.
func settled(schema *operatorv1alpha1.PtahSchema) *operatorv1alpha1.PtahSchema {
	schema = schema.DeepCopy()
	status := &schema.Status
	status.PendingObservation, status.Plan, status.Phase = nil, nil, operatorv1alpha1.PhasePending
	status.Conditions = []metav1.Condition{
		condition(operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionFalse, string(operatorv1alpha1.ReasonProofSettled),
			"A person settled the proof of the last Apply; no read established what that Apply did", 3),
		condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, string(operatorv1alpha1.ReasonProofSettled),
			"A person settled the proof of the last Apply; no read established what that Apply did", 3),
	}
	status.Journal.Entries = append(status.Journal.Entries, operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalVerified, At: metav1.NewTime(at(60)), Plan: proofPlan, Outcome: operatorv1alpha1.JournalUnknown,
	})
	return schema
}

func waitSettled(t *testing.T, docs ...*operatorv1alpha1.PtahSchema) (operate.Result, error, int) {
	t.Helper()
	timeline := &schemaTimeline{docs: docs[1:]}
	result, err := operate.Wait(context.Background(), timeline.reader(t), schemaRef(), operate.WaitOptions{
		Target: operate.TargetSettled, Timeout: time.Minute, Clock: newStepClock(), Since: docs[0],
		Settlement: settleValue(movedTarget),
	})
	return result, err, timeline.reads
}

func TestAWaitForASettlementEndsWhereTheOperatorTookIt(t *testing.T) {
	t.Parallel()
	written := stoppedSchema(movedTarget, map[string]string{operatorv1alpha1.SettledApplyAnnotation: settleValue(movedTarget)})

	result, err, reads := waitSettled(t, written, written, written, settled(written))

	if err != nil || result.Target != operate.TargetSettled || reads != 3 {
		t.Fatalf("Wait = %#v, %v after %d reads; want settled on the third", result, err, reads)
	}
}

// A proof can end without the statement, and a statement can be taken back. Each
// stops the wait, since no read will settle the proof after it.
func TestAWaitForASettlementStopsWhereNothingWillSettle(t *testing.T) {
	t.Parallel()
	written := stoppedSchema(movedTarget, map[string]string{operatorv1alpha1.SettledApplyAnnotation: settleValue(movedTarget)})
	ended := func(outcome operatorv1alpha1.JournalOutcome) *operatorv1alpha1.PtahSchema {
		document := settled(written)
		document.Status.Journal.Entries[len(document.Status.Journal.Entries)-1].Outcome = outcome
		document.Status.Conditions = nil
		return document
	}
	withdrawn := written.DeepCopy()
	withdrawn.Annotations = nil
	for _, row := range []struct {
		name     string
		document *operatorv1alpha1.PtahSchema
		reason   operate.StopReason
		says     string
	}{
		{"the first target proved the Apply", ended(operatorv1alpha1.JournalInSync), operate.StopProofEnded, "Verified InSync"},
		{"the first target still drifts", ended(operatorv1alpha1.JournalDriftRemains), operate.StopProofEnded, "Verified DriftRemains"},
		{"a newer generation", ended(operatorv1alpha1.JournalSuperseded), operate.StopProofEnded, "Verified Superseded"},
		{"the annotation came off", withdrawn, operate.StopStatementWithdrawn, "no longer holds " + settleValue(movedTarget)},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			_, err, _ := waitSettled(t, written, row.document)

			var stop *operate.StopError
			if !errors.As(err, &stop) || stop.Reason != row.reason || !strings.Contains(stop.Message, row.says) {
				t.Fatalf("Wait = %v, want %s saying %q", err, row.reason, row.says)
			}
		})
	}

	t.Run("a settled Ready with no journal entry", func(t *testing.T) {
		t.Parallel()
		// The status write that settles records the entry; Ready alone is not
		// that write.
		gone := settled(written)
		gone.Status.Journal.Entries = gone.Status.Journal.Entries[:len(gone.Status.Journal.Entries)-1]

		_, err, _ := waitSettled(t, written, gone)

		var stop *operate.StopError
		if !errors.As(err, &stop) || stop.Reason != operate.StopProofEnded || !strings.Contains(stop.Message, "records no reading") {
			t.Fatalf("Wait = %v, want a proof gone with no entry stopped", err)
		}
	})

	t.Run("an entry the baseline already held", func(t *testing.T) {
		t.Parallel()
		// An earlier Apply of the same plan settled long ago: its entry is no news.
		earlier := settled(written)
		earlier.Status.PendingObservation, earlier.Status.Conditions = written.Status.PendingObservation, written.Status.Conditions
		gone := earlier.DeepCopy()
		gone.Status.PendingObservation, gone.Status.Conditions = nil, nil

		_, err, _ := waitSettled(t, earlier, gone)

		var stop *operate.StopError
		if !errors.As(err, &stop) || stop.Reason != operate.StopProofEnded || !strings.Contains(stop.Message, "records no reading") {
			t.Fatalf("Wait = %v, want the old entry not counted", err)
		}
	})
}

// A timeout names what the statement still waits for.
func TestAWaitForASettlementSaysWhatItWaitsFor(t *testing.T) {
	t.Parallel()
	written := stoppedSchema(movedTarget, map[string]string{operatorv1alpha1.SettledApplyAnnotation: settleValue(movedTarget)})
	suspended := written.DeepCopy()
	suspended.Spec.Suspend = true
	elsewhere := written.DeepCopy()
	elsewhere.Status.Conditions[0].Message = stopMessage(thirdTarget)
	failing := written.DeepCopy()
	failing.Status.Conditions[0].Reason = string(operatorv1alpha1.ReasonOperationFailed)
	for _, row := range []struct {
		name     string
		document *operatorv1alpha1.PtahSchema
		says     string
	}{
		{"the stop that prints it", written, "the next read of target " + movedTarget + ", which takes the statement " +
			"(spec.execution.failureRetryInterval is 45s), unless the URL Secret holds target " + planTarget +
			" again, the one the plan was made for: a read of it finishes the proof instead"},
		{"suspended", suspended, "resume: a suspended resource starts no read"},
		{"stopped on another target", elsewhere, "a stop on target " + movedTarget + " again"},
		{"a read that stopped nowhere", failing, "a read that names target " + movedTarget + " and stops there: the last read " +
			"did not stop on a moved target. It failed and named no target, or it read target " + planTarget +
			", the one the plan was made for, where the proof finishes without the statement"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			_, err, _ := waitSettled(t, written, row.document)

			var timeout *operate.TimeoutError
			if !errors.As(err, &timeout) || !strings.Contains(timeout.Waiting, row.says) {
				t.Fatalf("Wait = %v, want a timeout waiting for %q", err, row.says)
			}
		})
	}
}

func TestASettlementIsWaitedOnAloneAndOnASchema(t *testing.T) {
	t.Parallel()
	reader := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	for _, row := range []struct {
		name    string
		ref     operate.Ref
		options operate.WaitOptions
	}{
		{"no value", schemaRef(), operate.WaitOptions{Target: operate.TargetSettled, Timeout: time.Minute}},
		{"another target", schemaRef(), operate.WaitOptions{Target: operate.TargetInSync, Timeout: time.Minute, Settlement: settleValue(movedTarget)}},
		{"a migration", refOf(operate.FamilyMigration), operate.WaitOptions{Target: operate.TargetSettled, Timeout: time.Minute, Settlement: settleValue(movedTarget)}},
		{"a value that names no target", schemaRef(), operate.WaitOptions{Target: operate.TargetSettled, Timeout: time.Minute, Settlement: applyID}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if _, err := operate.Wait(context.Background(), reader, row.ref, row.options); err == nil || apierrors.IsNotFound(err) {
				t.Fatalf("Wait = %v, want the options refused", err)
			}
		})
	}
	if _, err := operate.ParseTarget(string(operate.TargetSettled)); err == nil {
		t.Fatal("a command line can name the settled target, which needs the value settle wrote")
	}
}
