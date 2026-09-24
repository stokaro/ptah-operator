package controller

import (
	"context"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A migration plan records the execution components it was computed under.
// Before dispatching an Apply the controller re-reads them and refuses if any
// has moved, because the plan says what to do and the components are what will
// do it: a plan decided against one executor, runner or controller build is
// not a plan for another.
//
// The eight fields are compared in one disjunction, so one of them drifting
// marks the whole branch measured. Each was unmeasured, and a part that stops
// working does not fail loudly -- it dispatches a run under components that
// never saw the plan being decided.
func TestAPlanIsNotAppliedUnderComponentsItWasNotDecidedUnder(t *testing.T) {
	t.Parallel()

	const refusal = "an execution component changed after the plan was published"
	other := "sha256:" + strings.Repeat("e", 64)

	t.Run("a plan under its own components is applied", func(t *testing.T) {
		t.Parallel()

		migration, plan := awaitingApprovalFixture(t)
		approval := migrationApprovalFor(migration, plan)
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval,
			verificationPolicyConfigMap())

		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if readMigration(t, api, migration).Status.ActiveOperation == nil {
			t.Fatal("a plan under its own components did not dispatch, so nothing below proves anything")
		}
	})

	for _, row := range []struct {
		name   string
		change func(*operatorv1alpha1.PtahMigrationPlanSpec)
	}{
		{
			name: "another execution binding epoch",
			change: func(spec *operatorv1alpha1.PtahMigrationPlanSpec) {
				spec.ExecutionBindingID = "v1-" + strings.Repeat("e", 32)
			},
		},
		{
			name: "another controller image",
			change: func(spec *operatorv1alpha1.PtahMigrationPlanSpec) {
				spec.ControllerImage = "example.invalid/manager@" + other
			},
		},
		{
			name: "another controller revision",
			change: func(spec *operatorv1alpha1.PtahMigrationPlanSpec) {
				spec.ControllerRevision += "-from-another-rollout"
			},
		},
		{
			name: "another controller state version",
			change: func(spec *operatorv1alpha1.PtahMigrationPlanSpec) {
				spec.ControllerStateVersion++
			},
		},
		{
			name: "another data-plane version",
			change: func(spec *operatorv1alpha1.PtahMigrationPlanSpec) {
				spec.PtahVersion += "-rc1"
			},
		},
		{
			// The executor is what opens the database and runs the SQL.
			name: "another executor image",
			change: func(spec *operatorv1alpha1.PtahMigrationPlanSpec) {
				spec.ExecutorImage = "example.invalid/ptah@" + other
			},
		},
		{
			name: "another runner image",
			change: func(spec *operatorv1alpha1.PtahMigrationPlanSpec) {
				spec.RunnerImage = "example.invalid/operator@" + other
			},
		},
		{
			// The protocol is how the run reports what it did; a different one
			// makes the report unreadable by the controller that asked for it.
			name: "another runner protocol version",
			change: func(spec *operatorv1alpha1.PtahMigrationPlanSpec) {
				spec.RunnerProtocolVersion++
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, plan := awaitingApprovalFixture(t)
			approval := migrationApprovalFor(migration, plan)
			row.change(&plan.Spec)
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval,
				verificationPolicyConfigMap())

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if operation := actual.Status.ActiveOperation; operation != nil {
				t.Fatalf("an Apply was dispatched under components the plan was not decided under: %#v",
					operation)
			}
			condition := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationReady)
			if condition == nil || !strings.Contains(condition.Message, refusal) {
				t.Fatalf("Ready condition = %#v, want one naming a changed execution component",
					condition)
			}
		})
	}
}
