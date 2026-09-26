package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// A migration plan records the execution components it was computed under.
// Before dispatching an Apply the controller re-reads them and refuses if any
// has moved, because the plan says what to do and the components are what will
// do it: a plan decided against one executor or runner protocol is not a plan
// for another.
//
// The five bound fields are compared in one disjunction, so one of them
// drifting marks the whole branch measured. Each was unmeasured, and a part
// that stops working does not fail loudly -- it dispatches a run under
// components that never saw the plan being decided.
//
// The manager's own image, its revision and the runner image built beside it
// are recorded on the plan and are not among them;
// TestAManagerOnlyUpgradeAppliesAPendingMigrationApproval holds that side.
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
			// The protocol is what the runner enforces and how the run reports
			// what it did; a different one is a different set of checks and a
			// report the controller that asked for it cannot read.
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

// A rollout that changes an execution component retires a read-only claim and
// installs the new binding in one status write. Neither primary watch passes a
// status-only update, so that write wakes nothing: the pass has to ask for the
// next one itself, or the resource waits out an interval before it works under
// the binding it just installed.
func TestABindingRotationAsksForTheNextPass(t *testing.T) {
	t.Parallel()

	migration, _ := awaitingApprovalFixture(t)
	migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
	oldEpoch := migration.Status.ExecutionBinding.Epoch
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, verificationPolicyConfigMap())
	rolledOut := "example.invalid/ptah@" + safetyOtherDigest
	reconciler.Jobs = executionBindingJobs{
		ptahVersion:   migration.Status.ExecutionBinding.PtahVersion,
		executorImage: rolledOut,
		protocol:      migration.Status.ExecutionBinding.RunnerProtocolVersion,
	}

	result, err := reconciler.Reconcile(context.Background(), migrationRequest(migration))
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	binding := actual.Status.ExecutionBinding
	if binding == nil || binding.Epoch == oldEpoch || binding.ExecutorImage != rolledOut ||
		actual.Status.ActiveOperation != nil {
		t.Fatalf("the rollout did not retire the claim under a new binding, so the result below "+
			"is about some other pass: binding %#v, claim %#v", binding, actual.Status.ActiveOperation)
	}
	if result.RequeueAfter != statusPatchRequeue {
		t.Fatalf("binding rotation result = %#v, want the next pass after %s", result, statusPatchRequeue)
	}
}

// TestAManagerOnlyUpgradeAppliesAPendingMigrationApproval is a patch release of
// the manager arriving while a person's approval waits. The plan and the
// approval were written under one manager; the manager that reconciles next
// has another image, another revision and another runner image, and the same
// executor, Ptah version, runner protocol and controller-state version. The
// epoch stays, the approval is taken, and the Apply Job is built by, and
// records, the manager that dispatched it.
func TestAManagerOnlyUpgradeAppliesAPendingMigrationApproval(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	approval := migrationApprovalFor(migration, plan)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval,
		verificationPolicyConfigMap())
	next := workloadBuilderForMigrations()
	next.ControllerImage = "example.invalid/manager@sha256:" + strings.Repeat("d", 64)
	next.ControllerRevision = testControllerRevision + "-patch"
	next.RunnerImage = "example.invalid/operator@sha256:" + strings.Repeat("d", 64)
	if plan.Spec.ControllerImage == next.ControllerImage || plan.Spec.ControllerRevision == next.ControllerRevision ||
		plan.Spec.RunnerImage == next.RunnerImage {
		t.Fatal("the plan already records the next manager, so the upgrade below changes nothing")
	}
	reconciler.Jobs = next

	job := reconcileUntilAMigrationJobExists(t, reconciler, api, migration)
	actual := readMigration(t, api, migration)
	if actual.Status.ExecutionBinding == nil || actual.Status.ExecutionBinding.Epoch != testExecutionBindingID {
		t.Fatalf("a manager-only upgrade moved the execution epoch: %#v", actual.Status.ExecutionBinding)
	}
	operation := actual.Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationApply ||
		operation.ExecutionBindingID != testExecutionBindingID ||
		operation.PlanRef == nil || operation.PlanRef.UID != plan.UID ||
		operation.ApprovalRef == nil || operation.ApprovalRef.UID != approval.UID {
		t.Fatalf("the Apply claim does not carry the approved plan under its epoch: %#v", operation)
	}
	if job.Name != operation.JobName || job.Annotations[workload.AnnotationExecutionBindingID] != testExecutionBindingID {
		t.Fatalf("Apply Job %q under epoch %q, want %q under %q",
			job.Name, job.Annotations[workload.AnnotationExecutionBindingID], operation.JobName, testExecutionBindingID)
	}
	controllerImage, controllerRevision, runnerImage := workload.ManagerIdentityOf(job)
	if controllerImage != next.ControllerImage || controllerRevision != next.ControllerRevision || runnerImage != next.RunnerImage {
		t.Fatalf("the Apply Job records manager %q, %q, runner %q; want the manager that dispatched it",
			controllerImage, controllerRevision, runnerImage)
	}
	persisted := &operatorv1alpha1.PtahMigrationApproval{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), persisted); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(persisted.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) ||
		meta.IsStatusConditionTrue(persisted.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) {
		t.Fatalf("approval conditions = %#v, want consumed by the dispatch and never stale", persisted.Status.Conditions)
	}
}

// TestAnExecutionUpgradeRetiresAPendingMigrationApproval is the other side of
// the same rollout: the manager that reconciles next runs another executor,
// another Ptah version, another runner protocol or another controller-state
// version. The epoch moves, the plan the approval named is dropped rather than
// applied, and the approval is never taken.
//
// The control row changes nothing and has to dispatch within the same number
// of passes, or the refusals below would hold for a controller that was merely
// slow.
func TestAnExecutionUpgradeRetiresAPendingMigrationApproval(t *testing.T) {
	t.Parallel()

	const passes = 6
	t.Run("control: the same execution dispatches", func(t *testing.T) {
		t.Parallel()

		migration, plan := awaitingApprovalFixture(t)
		approval := migrationApprovalFor(migration, plan)
		reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval,
			verificationPolicyConfigMap())
		reconciler.Jobs = executionBindingJobs{
			ptahVersion:   "v0.3.0",
			executorImage: "example.invalid/ptah@" + testDigest,
			protocol:      int32(runner.ProtocolVersion),
		}
		for pass := 0; pass < passes; pass++ {
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
		}
		persisted := &operatorv1alpha1.PtahMigrationApproval{}
		if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), persisted); err != nil {
			t.Fatal(err)
		}
		if !meta.IsStatusConditionTrue(persisted.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) {
			t.Fatalf("the unchanged execution did not take the approval in %d passes: %#v",
				passes, readMigration(t, api, migration).Status)
		}
	})

	for name, change := range map[string]func(*executionBindingJobs){
		"executor image": func(jobs *executionBindingJobs) {
			jobs.executorImage = "example.invalid/ptah@sha256:" + strings.Repeat("d", 64)
		},
		"Ptah version":             func(jobs *executionBindingJobs) { jobs.ptahVersion = "v0.4.0" },
		"runner protocol":          func(jobs *executionBindingJobs) { jobs.protocol++ },
		"controller state version": func(jobs *executionBindingJobs) { jobs.controllerStateVersion = testControllerStateVersion + 1 },
	} {
		change := change
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			migration, plan := awaitingApprovalFixture(t)
			approval := migrationApprovalFor(migration, plan)
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval,
				verificationPolicyConfigMap())
			jobs := executionBindingJobs{
				ptahVersion:   "v0.3.0",
				executorImage: "example.invalid/ptah@" + testDigest,
				protocol:      int32(runner.ProtocolVersion),
			}
			change(&jobs)
			reconciler.Jobs = jobs

			for pass := 0; pass < passes; pass++ {
				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
			}
			actual := readMigration(t, api, migration)
			if actual.Status.ExecutionBinding == nil || actual.Status.ExecutionBinding.Epoch == testExecutionBindingID {
				t.Fatalf("an execution upgrade kept the epoch: %#v", actual.Status.ExecutionBinding)
			}
			if actual.Status.Plan != nil && actual.Status.Plan.UID == plan.UID {
				t.Fatalf("the plan decided under the retired execution is still current: %#v", actual.Status.Plan)
			}
			if operation := actual.Status.ActiveOperation; operation != nil && operation.Type == operatorv1alpha1.MigrationOperationApply {
				t.Fatalf("an Apply was claimed under the retired approval: %#v", operation)
			}
			// The new epoch starts the read-only chain again, so read-only Jobs
			// are expected; an Apply Job is not.
			dispatched := &batchv1.JobList{}
			if err := api.List(context.Background(), dispatched); err != nil {
				t.Fatal(err)
			}
			for _, job := range dispatched.Items {
				if job.Labels[workload.LabelOperation] == "apply" {
					t.Fatalf("an Apply Job %q was dispatched across an execution upgrade", job.Name)
				}
			}
			persisted := &operatorv1alpha1.PtahMigrationApproval{}
			if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), persisted); err != nil {
				t.Fatal(err)
			}
			if meta.IsStatusConditionTrue(persisted.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) {
				t.Fatalf("the approval was consumed across an execution upgrade: %#v", persisted.Status.Conditions)
			}
		})
	}
}

// reconcileUntilAMigrationJobExists runs the reconciler until it has created a
// Job, and fails naming the status it stopped at when it never does.
func reconcileUntilAMigrationJobExists(
	t *testing.T,
	reconciler *MigrationReconciler,
	api client.Client,
	migration *operatorv1alpha1.PtahMigration,
) *batchv1.Job {
	t.Helper()

	for pass := 0; pass < 8; pass++ {
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() pass %d error = %v", pass, err)
		}
		jobs := &batchv1.JobList{}
		if err := api.List(context.Background(), jobs); err != nil {
			t.Fatal(err)
		}
		if len(jobs.Items) == 1 {
			return &jobs.Items[0]
		}
		if len(jobs.Items) > 1 {
			t.Fatalf("%d Jobs were dispatched, want one", len(jobs.Items))
		}
	}
	actual := readMigration(t, api, migration)
	t.Fatalf("no Job was dispatched after eight passes: phase=%q operation=%#v conditions=%#v",
		actual.Status.Phase, actual.Status.ActiveOperation, actual.Status.Conditions)
	return nil
}
