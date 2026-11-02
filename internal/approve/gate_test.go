package approve_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

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
			o.t.Fatalf("write what the operator would: %v", err)
		}
	}
	return o.answering.Get(ctx, key, object, options...)
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
// generation. The fake API server leaves generation to its caller.
func specEdited(ctx context.Context, c client.Client) error {
	schema := &operatorv1alpha1.PtahSchema{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: planreadtest.Namespace, Name: "storefront"}, schema); err != nil {
		return err
	}
	schema.Generation++
	schema.Spec.Policy.AllowDestructive = true
	return c.Update(ctx, schema)
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
					specEdited,
				}
			},
			outcome: approve.Changed, says: "The spec of schema/storefront changed after you reviewed its plan",
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
			outcome: approve.Closed, says: "The operator was still reading schema/storefront again after 5m0s",
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
