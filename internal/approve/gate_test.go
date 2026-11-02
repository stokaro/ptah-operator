package approve_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/approve"
	"github.com/stokaro/ptah-operator/pkg/planread/planreadtest"
)

// operating is the API server with the operator writing the resource between
// Submit's reads of it: each read of the schema or the migration first
// applies the next step, the way the controller's writes land between two
// polls.
type operating struct {
	*answering
	t     *testing.T
	mu    sync.Mutex
	steps []func(context.Context, client.Client) error
}

func (o *operating) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	var step func(context.Context, client.Client) error
	switch object.(type) {
	case *operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahMigration:
		o.mu.Lock()
		if len(o.steps) > 0 {
			step, o.steps = o.steps[0], o.steps[1:]
		}
		o.mu.Unlock()
	}
	if step != nil {
		if err := step(ctx, o.answering.Client); err != nil {
			var failed readFails
			if errors.As(err, &failed) {
				return failed.err
			}
			o.t.Fatalf("write what the operator would: %v", err)
		}
	}
	return o.answering.Get(ctx, key, object, options...)
}

// readFails is a step that answers the read with err instead.
type readFails struct{ err error }

func (r readFails) Error() string { return r.err.Error() }

func failing(err error) func(context.Context, client.Client) error {
	return func(context.Context, client.Client) error { return readFails{err} }
}

func (o *operating) left() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.steps)
}

// schemaStatus is a step that writes the schema's status.
func schemaStatus(change func(*operatorv1alpha1.PtahSchemaStatus)) func(context.Context, client.Client) error {
	return func(ctx context.Context, c client.Client) error {
		schema := &operatorv1alpha1.PtahSchema{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: planreadtest.Namespace, Name: "storefront"}, schema); err != nil {
			return err
		}
		change(&schema.Status)
		return c.Status().Update(ctx, schema)
	}
}

// specEdited is a person editing the schema's spec, which moves its
// generation by edits. The fake API server leaves generation to its caller.
func specEdited(edits int64, change func(*operatorv1alpha1.PtahSchemaSpec)) func(context.Context, client.Client) error {
	return func(ctx context.Context, c client.Client) error {
		schema := &operatorv1alpha1.PtahSchema{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: planreadtest.Namespace, Name: "storefront"}, schema); err != nil {
			return err
		}
		schema.Generation += edits
		change(&schema.Spec)
		return c.Update(ctx, schema)
	}
}

// The steps of the read the operator takes at its interval while a plan
// waits: each claim writes its phase and operation, the Observe harvest
// clears the plan, and the Plan harvest publishes it again.
func claimed(phase operatorv1alpha1.ReconciliationPhase, operation operatorv1alpha1.OperationType) func(*operatorv1alpha1.PtahSchemaStatus) {
	return func(s *operatorv1alpha1.PtahSchemaStatus) {
		s.Phase = phase
		s.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{Type: operation, ID: "read-" + string(operation)}
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "OperationInProgress",
			Message: string(operation) + " operation is in progress",
		})
	}
}

func observed(s *operatorv1alpha1.PtahSchemaStatus) {
	s.Phase, s.ActiveOperation, s.Plan = operatorv1alpha1.PhasePlanning, nil, nil
}

// published is the Plan harvest: plan back at the gate, as it puts it.
func published(plan operatorv1alpha1.CurrentPlanStatus) func(*operatorv1alpha1.PtahSchemaStatus) {
	return func(s *operatorv1alpha1.PtahSchemaStatus) {
		s.Phase, s.ActiveOperation, s.Plan = operatorv1alpha1.PhaseAwaitingApproval, nil, plan.DeepCopy()
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "AwaitingApproval",
			Message: "Plan is waiting for approval",
		})
	}
}

// A person's answer that meets the operator reading the schema again at its
// interval waits for that read to end, and is sent only when the reviewed
// plan, under the reviewed spec, is back at the approval gate. Every read in
// between is held to the same identity, and a read that ends anywhere else
// is answered as it would have been at once.
func TestSubmitWaitsForTheGateWhileTheOperatorReadsAgain(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		steps func(reviewed operatorv1alpha1.CurrentPlanStatus) []func(context.Context, client.Client) error
		// outcome and says are the answer; a step left over would be a read
		// the decision did not need.
		outcome approve.Outcome
		says    string
	}{
		{
			name: "the read ends at the gate with the reviewed plan",
			steps: func(reviewed operatorv1alpha1.CurrentPlanStatus) []func(context.Context, client.Client) error {
				return []func(context.Context, client.Client) error{
					schemaStatus(claimed(operatorv1alpha1.PhaseResolving, operatorv1alpha1.OperationResolve)),
					schemaStatus(claimed(operatorv1alpha1.PhaseVerifying, operatorv1alpha1.OperationVerify)),
					schemaStatus(claimed(operatorv1alpha1.PhaseObserving, operatorv1alpha1.OperationObserve)),
					schemaStatus(observed),
					schemaStatus(claimed(operatorv1alpha1.PhasePlanning, operatorv1alpha1.OperationPlan)),
					schemaStatus(published(reviewed)),
				}
			},
			outcome: approve.Admitted,
		},
		{
			// A read the API server may answer next time does not end the
			// wait.
			name: "a read fails once while the operator reads",
			steps: func(reviewed operatorv1alpha1.CurrentPlanStatus) []func(context.Context, client.Client) error {
				return []func(context.Context, client.Client) error{
					schemaStatus(claimed(operatorv1alpha1.PhaseResolving, operatorv1alpha1.OperationResolve)),
					failing(apierrors.NewServiceUnavailable("the API server is restarting")),
					schemaStatus(published(reviewed)),
				}
			},
			outcome: approve.Admitted,
		},
		{
			// Suspending and resuming moves the generation and changes no
			// desired state (desiredspec, the rule admission applies).
			name: "a suspend and a resume between two reads",
			steps: func(reviewed operatorv1alpha1.CurrentPlanStatus) []func(context.Context, client.Client) error {
				return []func(context.Context, client.Client) error{
					schemaStatus(claimed(operatorv1alpha1.PhaseResolving, operatorv1alpha1.OperationResolve)),
					specEdited(2, func(*operatorv1alpha1.PtahSchemaSpec) {}),
					schemaStatus(published(reviewed)),
				}
			},
			outcome: approve.Admitted,
		},
		{
			name: "the read ends with a newer plan",
			steps: func(reviewed operatorv1alpha1.CurrentPlanStatus) []func(context.Context, client.Client) error {
				newer := reviewed
				newer.Name, newer.UID, newer.Fingerprint = "ptah-plan-newer", "uid-plan-newer", "sha256:newer"
				return []func(context.Context, client.Client) error{
					schemaStatus(claimed(operatorv1alpha1.PhaseResolving, operatorv1alpha1.OperationResolve)),
					schemaStatus(observed),
					schemaStatus(published(newer)),
				}
			},
			outcome: approve.Changed, says: "A newer plan, ptah-plan-newer, replaced",
		},
		{
			name: "the read ends with the plan under another fingerprint",
			steps: func(reviewed operatorv1alpha1.CurrentPlanStatus) []func(context.Context, client.Client) error {
				another := reviewed
				another.Fingerprint = "sha256:another"
				return []func(context.Context, client.Client) error{
					schemaStatus(claimed(operatorv1alpha1.PhaseResolving, operatorv1alpha1.OperationResolve)),
					schemaStatus(published(another)),
				}
			},
			outcome: approve.Changed, says: "carries fingerprint sha256:another",
		},
		{
			// Refused at the read that shows it, without waiting for the
			// read to end.
			name: "the spec changed while the operator read",
			steps: func(operatorv1alpha1.CurrentPlanStatus) []func(context.Context, client.Client) error {
				return []func(context.Context, client.Client) error{
					schemaStatus(claimed(operatorv1alpha1.PhaseResolving, operatorv1alpha1.OperationResolve)),
					specEdited(1, func(spec *operatorv1alpha1.PtahSchemaSpec) { spec.Policy.AllowDestructive = true }),
				}
			},
			outcome: approve.Changed, says: "The spec of schema/storefront changed after you reviewed its plan",
		},
		{
			name: "a key named suspend changed deeper in the spec",
			steps: func(operatorv1alpha1.CurrentPlanStatus) []func(context.Context, client.Client) error {
				return []func(context.Context, client.Client) error{
					schemaStatus(claimed(operatorv1alpha1.PhaseResolving, operatorv1alpha1.OperationResolve)),
					specEdited(1, func(spec *operatorv1alpha1.PtahSchemaSpec) {
						spec.Execution.NodeSelector = map[string]string{"suspend": "true"}
					}),
				}
			},
			outcome: approve.Changed, says: "The spec of schema/storefront changed after you reviewed its plan",
		},
		{
			name: "every read fails until the wait is over",
			steps: func(operatorv1alpha1.CurrentPlanStatus) []func(context.Context, client.Client) error {
				steps := []func(context.Context, client.Client) error{
					schemaStatus(claimed(operatorv1alpha1.PhaseResolving, operatorv1alpha1.OperationResolve)),
				}
				// A read every two seconds after the first, up to and at the
				// deadline five minutes on.
				for range 150 {
					steps = append(steps, failing(apierrors.NewServiceUnavailable("the API server is restarting")))
				}
				return steps
			},
			outcome: approve.Failed,
			says: "No read of schema/storefront found the approval gate open within 5m, and the last one failed: " +
				"read PtahSchema application/storefront: the API server is restarting",
		},
		{
			name: "someone else approved the plan when it was back at the gate",
			steps: func(reviewed operatorv1alpha1.CurrentPlanStatus) []func(context.Context, client.Client) error {
				approved := reviewed
				approved.Approval = &operatorv1alpha1.ConsumedApprovalStatus{Name: "first",
					Approver: operatorv1alpha1.ApprovalIdentity{Username: "bob"}}
				return []func(context.Context, client.Client) error{
					schemaStatus(claimed(operatorv1alpha1.PhaseVerifying, operatorv1alpha1.OperationVerify)),
					schemaStatus(published(approved)),
				}
			},
			outcome: approve.Closed, says: "approved already, by bob",
		},
		{
			name: "the read ends with the plan no longer requiring an approval",
			steps: func(reviewed operatorv1alpha1.CurrentPlanStatus) []func(context.Context, client.Client) error {
				return []func(context.Context, client.Client) error{
					schemaStatus(claimed(operatorv1alpha1.PhaseResolving, operatorv1alpha1.OperationResolve)),
					schemaStatus(func(s *operatorv1alpha1.PtahSchemaStatus) {
						published(reviewed)(s)
						s.Phase = operatorv1alpha1.PhaseBlocked
						meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: operatorv1alpha1.ConditionApprovalRequired,
							Status: metav1.ConditionFalse, Reason: "DestructiveChangesDisabled"})
					}),
				}
			},
			outcome: approve.Closed, says: "The schema is Blocked, not waiting for an approval",
		},
		{
			name: "the read ends with no plan",
			steps: func(operatorv1alpha1.CurrentPlanStatus) []func(context.Context, client.Client) error {
				return []func(context.Context, client.Client) error{
					schemaStatus(claimed(operatorv1alpha1.PhaseObserving, operatorv1alpha1.OperationObserve)),
					schemaStatus(func(s *operatorv1alpha1.PtahSchemaStatus) {
						s.Phase, s.ActiveOperation, s.Plan = operatorv1alpha1.PhaseInSync, nil, nil
					}),
				}
			},
			outcome: approve.Changed, says: "has no current plan any more",
		},
		{
			name: "the read does not end within the wait",
			steps: func(operatorv1alpha1.CurrentPlanStatus) []func(context.Context, client.Client) error {
				return []func(context.Context, client.Client) error{
					schemaStatus(claimed(operatorv1alpha1.PhaseObserving, operatorv1alpha1.OperationObserve)),
				}
			},
			outcome: approve.Closed, says: "The operator was still reading schema/storefront again after 5m.",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				lab, _ := waiting(t)
				review, err := approve.Load(context.Background(), lab.Client(), approve.Schema, planreadtest.Namespace, "storefront")
				if err != nil {
					t.Fatalf("Load() error = %v", err)
				}
				reviewed := *lab.Schema().Status.Plan
				api := &operating{answering: &answering{Client: lab.Client()}, t: t, steps: test.steps(reviewed)}
				var waited []string

				result := approve.Submit(context.Background(), api, review, approve.Options{
					Await: 5 * time.Minute, CallTimeout: 30 * time.Second, Waiting: func(why string) { waited = append(waited, why) },
				})

				if result.Outcome != test.outcome || !strings.Contains(result.Message, test.says) || api.left() != 0 {
					t.Fatalf("Submit() = %+v with %d steps left, want %s naming %q", result, api.left(), test.outcome, test.says)
				}
				if len(waited) != 1 || !strings.Contains(waited[0], "reading") {
					t.Fatalf("Submit() said it waited %q, want once, naming the read", waited)
				}
				created := 0
				if test.outcome == approve.Admitted {
					created = 1
				}
				if len(api.created) != created {
					t.Fatalf("Submit() sent %d approvals, want %d", len(api.created), created)
				}
				if created == 1 {
					spec := api.created[0].(*operatorv1alpha1.PtahSchemaApproval).Spec
					if spec.PlanRef.Name != reviewed.Name || spec.PlanRef.UID != reviewed.UID || spec.PlanFingerprint != reviewed.Fingerprint {
						t.Fatalf("the approval binds %+v, want the reviewed plan %s", spec, reviewed.Name)
					}
				}
			})
		})
	}
}

// A migration's read of its history leaves the reviewed plan in status, and
// the gate reopens on the same plan.
func TestSubmitWaitsForAMigrationsReadOfItsHistory(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		base := &answering{Client: planreadtest.NewClient(t, waitingMigration(nil)...)}
		review, err := approve.Load(context.Background(), base, approve.Migration, "application", "shipments")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		migrationStatus := func(change func(*operatorv1alpha1.PtahMigrationStatus)) func(context.Context, client.Client) error {
			return func(ctx context.Context, c client.Client) error {
				migration := &operatorv1alpha1.PtahMigration{}
				if err := c.Get(ctx, client.ObjectKey{Namespace: "application", Name: "shipments"}, migration); err != nil {
					return err
				}
				change(&migration.Status)
				// This fixture's API server registers no status subresource.
				return c.Update(ctx, migration)
			}
		}
		api := &operating{answering: base, t: t, steps: []func(context.Context, client.Client) error{
			migrationStatus(func(s *operatorv1alpha1.PtahMigrationStatus) {
				s.Phase = operatorv1alpha1.MigrationPhaseVerifying
				s.ActiveOperation = &operatorv1alpha1.MigrationOperationStatus{Type: operatorv1alpha1.MigrationOperationVerify, ID: "verify"}
			}),
			migrationStatus(func(s *operatorv1alpha1.PtahMigrationStatus) {
				s.Phase = operatorv1alpha1.MigrationPhaseReading
				s.ActiveOperation = &operatorv1alpha1.MigrationOperationStatus{Type: operatorv1alpha1.MigrationOperationHistory, ID: "read"}
			}),
			migrationStatus(func(s *operatorv1alpha1.PtahMigrationStatus) {
				s.Phase, s.ActiveOperation = operatorv1alpha1.MigrationPhasePlanning, nil
			}),
			migrationStatus(func(s *operatorv1alpha1.PtahMigrationStatus) {
				s.Phase = operatorv1alpha1.MigrationPhaseAwaitingApproval
			}),
		}}

		result := approve.Submit(context.Background(), api, review, approve.Options{Name: "shipments-14", Await: 5 * time.Minute})

		if result.Outcome != approve.Admitted || len(base.created) != 1 || api.left() != 0 {
			t.Fatalf("Submit() = %+v after %d creates with %d steps left, want admitted", result, len(base.created), api.left())
		}
	})
}

// A gate no read reopens is answered at once, as it was before Submit learned
// to wait: the read is not one that ends at the approval gate, because
// ApprovalRequired was not True when it began.
func TestSubmitAnswersAtOnceAGateNoReadReopens(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		load func(t *testing.T) (client.Client, approve.Family, string, string)
		says string
	}{
		{
			// Under apply policy Never a migration rests in Planning with its
			// plan published (applyMigrationPolicyPhase).
			name: "a migration under apply Never at rest",
			load: func(t *testing.T) (client.Client, approve.Family, string, string) {
				return planreadtest.NewClient(t, waitingMigration(func(m *operatorv1alpha1.PtahMigration, _ *operatorv1alpha1.PtahMigrationPlan) {
					m.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyNever
					m.Status.Phase = operatorv1alpha1.MigrationPhasePlanning
					m.Status.Conditions = []metav1.Condition{{
						Type: operatorv1alpha1.ConditionMigrationApprovalRequired, Status: metav1.ConditionFalse, Reason: "NotRequired",
						Message: "The apply policy is Never, so no approval is consulted",
					}}
				})...), approve.Migration, "application", "shipments"
			},
			says: "The migration is Planning, not waiting for an approval.",
		},
		{
			// The interval's Resolve of a schema whose plan the policy holds:
			// the claim leaves the plan and ApprovalRequired False, and the
			// read ends Blocked again.
			name: "a schema the policy holds, read at its interval",
			load: func(t *testing.T) (client.Client, approve.Family, string, string) {
				lab, _ := waiting(t)
				setStatus(t, lab, func(s *operatorv1alpha1.PtahSchemaStatus) {
					claimed(operatorv1alpha1.PhaseResolving, operatorv1alpha1.OperationResolve)(s)
					meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: operatorv1alpha1.ConditionApprovalRequired,
						Status: metav1.ConditionFalse, Reason: "DestructiveChangesDisabled", Message: "Plan contains destructive changes"})
				})
				return lab.Client(), approve.Schema, planreadtest.Namespace, "storefront"
			},
			says: "The operator is running Resolve for this schema.",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				c, family, namespace, name := test.load(t)
				review, err := approve.Load(context.Background(), c, family, namespace, name)
				if err != nil {
					t.Fatalf("Load() error = %v", err)
				}
				if review.Open || review.Reading || !strings.HasPrefix(review.Closed, test.says) {
					t.Fatalf("Load() open = %t, reading = %t, closed = %q; want closed at once naming %q",
						review.Open, review.Reading, review.Closed, test.says)
				}
				api := &answering{Client: c}
				began := time.Now()
				var waited []string

				result := approve.Submit(context.Background(), api, review, approve.Options{
					Await: 5 * time.Minute, Waiting: func(why string) { waited = append(waited, why) },
				})

				if result.Outcome != approve.Closed || !strings.HasPrefix(result.Message, test.says) || len(api.created) != 0 {
					t.Fatalf("Submit() = %+v after %d creates, want closed naming %q", result, len(api.created), test.says)
				}
				if spent := time.Since(began); spent != 0 || len(waited) != 0 {
					t.Fatalf("Submit() waited %s and said %q for a gate no read reopens", spent, waited)
				}
			})
		})
	}
}

// The plan a migration names is no longer stored when Submit reads it again:
// the plan is gone, not the migration.
func TestSubmitSaysAMigrationsPlanIsGone(t *testing.T) {
	t.Parallel()
	api := &answering{Client: planreadtest.NewClient(t, waitingMigration(nil)...)}
	review, err := approve.Load(context.Background(), api, approve.Migration, "application", "shipments")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: "application", Name: "shipments-p-14"}, plan); err != nil {
		t.Fatalf("read the plan: %v", err)
	}
	if err := api.Delete(context.Background(), plan); err != nil {
		t.Fatalf("delete the plan: %v", err)
	}

	result := approve.Submit(context.Background(), api, review, approve.Options{Await: 5 * time.Minute})

	if result.Outcome != approve.Changed || !strings.Contains(result.Message, "migration/shipments has no current plan any more") ||
		strings.Contains(result.Message, "created again") || len(api.created) != 0 {
		t.Fatalf("Submit() = %+v after %d creates, want the plan named gone", result, len(api.created))
	}
}

// Before Submit waits for anything, a failed read is answered as one: nothing
// was promised, and the person learns at once that the cluster did not answer.
func TestSubmitAnswersAFailedReadAtOnceBeforeItWaits(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lab, _ := waiting(t)
		review, err := approve.Load(context.Background(), lab.Client(), approve.Schema, planreadtest.Namespace, "storefront")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		api := &operating{answering: &answering{Client: lab.Client()}, t: t, steps: []func(context.Context, client.Client) error{
			failing(apierrors.NewServiceUnavailable("the API server is restarting")),
		}}
		began := time.Now()

		result := approve.Submit(context.Background(), api, review, approve.Options{Await: 5 * time.Minute})

		if result.Outcome != approve.Failed || len(api.created) != 0 || time.Since(began) != 0 ||
			result.Message != "Nothing was approved: read PtahSchema application/storefront: the API server is restarting" {
			t.Fatalf("Submit() = %+v after %s, want the failed read answered at once", result, time.Since(began))
		}
	})
}
