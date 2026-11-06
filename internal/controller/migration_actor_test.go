package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	// A person whose username is awkward in every way the path could break it.
	stampedApprover = "oidc:José García"
	// loggedApprover is that username as the run records it.
	loggedApprover = "oidc:Jos%C3%A9 Garc%C3%ADa"
)

func executorActor(t *testing.T, job *batchv1.Job) (string, bool) {
	t.Helper()
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.Name != executorContainerName {
			continue
		}
		for _, variable := range container.Env {
			if variable.Name == workload.EnvMigrationActor {
				return variable.Value, true
			}
		}
		return "", false
	}
	t.Fatalf("Job %s has no executor container", job.Name)
	return "", false
}

// The name comes from the approval the claim consumes, and the Job carries what
// the claim says, read with the real builder on the way to dispatch.
func TestAnApprovedApplyCarriesTheIdentityAdmissionStamped(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	approval := migrationApprovalFor(migration, plan)
	approval.Spec.Approver.Username = stampedApprover
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval, verificationPolicyConfigMap())
	reconciler.Jobs = workloadBuilderForMigrations()

	job := reconcileUntilAMigrationJobExists(t, reconciler, api, migration)
	operation := readMigration(t, api, migration).Status.ActiveOperation
	if operation == nil || operation.ApprovalRef == nil || operation.ApprovalRef.UID != approval.UID {
		t.Fatalf("the claim does not name the approval it consumes: %#v", operation)
	}
	if operation.Actor != loggedApprover {
		t.Fatalf("the claim records %q, want %q for the stamped %q", operation.Actor, loggedApprover, stampedApprover)
	}
	if carried, found := executorActor(t, job); !found || carried != loggedApprover {
		t.Fatalf("the Apply Job carries %q (present %t), want %q", carried, found, loggedApprover)
	}
}

// Two approvals of one plan: the oldest is the decision, and the run records
// who made it rather than whoever approved last.
func TestAnApplyRecordsTheApprovalItConsumes(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	decision := migrationApprovalFor(migration, plan)
	decision.Spec.Approver.Username = stampedApprover
	later := migrationApprovalFor(migration, plan)
	later.Name, later.UID = "orders-approval-later", "later-approval-uid"
	later.CreationTimestamp.Time = decision.CreationTimestamp.Add(60_000_000_000)
	later.Spec.Approver.Username = "someone-who-approved-later"
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, decision, later, verificationPolicyConfigMap())

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	operation := readMigration(t, api, migration).Status.ActiveOperation
	if operation == nil || operation.ApprovalRef == nil || operation.ApprovalRef.UID != decision.UID {
		t.Fatalf("the claim does not name the oldest approval: %#v", operation)
	}
	if operation.Actor != loggedApprover {
		t.Fatalf("the claim records %q, want %q for the approval it consumes", operation.Actor, loggedApprover)
	}
}

// Under apply: Always nothing was approved. The log says so instead of naming
// whoever the process runs as, and the Job carries the name like any other.
func TestAnAlwaysApplyNamesThePolicyAndNoPerson(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	migration.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyAlways
	migration.Status.Phase = operatorv1alpha1.MigrationPhasePlanning
	repolicyPlan(t, migration, plan)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
	reconciler.Jobs = workloadBuilderForMigrations()

	job := reconcileUntilAMigrationJobExists(t, reconciler, api, migration)
	operation := readMigration(t, api, migration).Status.ActiveOperation
	if operation == nil || operation.ApprovalRef != nil {
		t.Fatalf("the claim names an approval nobody gave: %#v", operation)
	}
	const want = "system:ptah-operator:apply-always:team-a/orders"
	if operation.Actor != want {
		t.Fatalf("the claim records %q, want %q", operation.Actor, want)
	}
	if carried, found := executorActor(t, job); !found || carried != want {
		t.Fatalf("the Apply Job carries %q (present %t), want %q", carried, found, want)
	}
}

// Deleting the approval is what losing the cluster does to it, and it must not
// reach the run that was already authorized: the claim holds the name, so the
// Job still rebuilds, the manager still confirms it, and the claim is the one
// that was written.
func TestDeletingTheApprovalLeavesTheRunAndItsNameAlone(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	approval := migrationApprovalFor(migration, plan)
	approval.Spec.Approver.Username = stampedApprover
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval, verificationPolicyConfigMap())
	reconciler.Jobs = workloadBuilderForMigrations()
	job := reconcileUntilAMigrationJobExists(t, reconciler, api, migration)
	claimed := readMigration(t, api, migration).Status.ActiveOperation

	if err := api.Delete(context.Background(), approval); err != nil {
		t.Fatalf("delete the approval: %v", err)
	}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), &operatorv1alpha1.PtahMigrationApproval{}); err == nil {
		t.Fatal("the approval is still there, so this row proves nothing about its absence")
	}
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	actual := readMigration(t, api, migration)
	operation := actual.Status.ActiveOperation
	if operation == nil || operation.ID != claimed.ID || operation.JobUID != job.UID || operation.Actor != loggedApprover {
		t.Fatalf("the claim did not survive deleting its approval: %#v, lastRun %#v, conditions %#v",
			operation, actual.Status.LastRun, actual.Status.Conditions)
	}
	if actual.Status.LastRun != nil || actual.Status.UnresolvedRun != nil {
		t.Fatalf("deleting the approval settled a running Apply: lastRun %#v, unresolved %#v",
			actual.Status.LastRun, actual.Status.UnresolvedRun)
	}
}

// An approval withdrawn between the claim and the Job authorizes nothing. Nothing
// has dispatched at that step, so the claim goes and no Job is made, where before
// the run went ahead under the withdrawn approval and wrote that person's name to
// the database for good. A person who changes their mind has until the Job exists.
func TestAnApprovalWithdrawnBeforeDispatchRetiresTheClaim(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		withdraw func(*testing.T, client.Client, *operatorv1alpha1.PtahMigrationApproval)
	}{
		{"deleted", func(t *testing.T, api client.Client, approval *operatorv1alpha1.PtahMigrationApproval) {
			t.Helper()
			if err := api.Delete(context.Background(), approval); err != nil {
				t.Fatalf("delete the approval: %v", err)
			}
		}},
		{"replaced under its name", func(t *testing.T, api client.Client, approval *operatorv1alpha1.PtahMigrationApproval) {
			t.Helper()
			if err := api.Delete(context.Background(), approval); err != nil {
				t.Fatalf("delete the approval: %v", err)
			}
			replacement := approval.DeepCopy()
			replacement.UID, replacement.ResourceVersion = "another-approval-uid", ""
			replacement.Spec.Approver.Username = "someone-else"
			if err := api.Create(context.Background(), replacement); err != nil {
				t.Fatalf("replace the approval: %v", err)
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, plan := awaitingApprovalFixture(t)
			approval := migrationApprovalFor(migration, plan)
			approval.Spec.Approver.Username = stampedApprover
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval, verificationPolicyConfigMap())
			reconciler.Jobs = workloadBuilderForMigrations()

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			claimed := readMigration(t, api, migration).Status.ActiveOperation
			if claimed == nil || claimed.Type != operatorv1alpha1.MigrationOperationApply || claimed.Actor != loggedApprover {
				t.Fatalf("the approved plan was not claimed under the approver's name: %#v", claimed)
			}
			row.withdraw(t, api, approval)

			var actual *operatorv1alpha1.PtahMigration
			for pass := 0; pass < 8; pass++ {
				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
					t.Fatalf("Reconcile() pass %d error = %v", pass, err)
				}
				actual = readMigration(t, api, migration)
				if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.ID != claimed.ID {
					break
				}
			}
			if operation := actual.Status.ActiveOperation; operation != nil && operation.ID == claimed.ID {
				t.Fatalf("the claim outlived its withdrawn approval: %#v, conditions %#v", operation, actual.Status.Conditions)
			}
			jobs := &batchv1.JobList{}
			if err := api.List(context.Background(), jobs); err != nil {
				t.Fatal(err)
			}
			if len(jobs.Items) != 0 {
				t.Fatalf("a Job was made under a withdrawn approval: %s", jobs.Items[0].Name)
			}
			if actual.Status.LastRun != nil || actual.Status.UnresolvedRun != nil {
				t.Fatalf("nothing dispatched, and the resource records a run: lastRun %#v, unresolved %#v",
					actual.Status.LastRun, actual.Status.UnresolvedRun)
			}
			progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
			if progressing == nil || !strings.Contains(progressing.Message, "deleted or replaced before dispatch") {
				t.Fatalf("Progressing = %#v, want it to say why the claim went", progressing)
			}
			stored := &operatorv1alpha1.PtahMigrationApproval{}
			err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), stored)
			if err == nil && meta.IsStatusConditionTrue(stored.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) {
				t.Fatal("a replacement approval nobody claimed under was spent")
			}
		})
	}
}

// A run dispatched across an upgrade that moves the executor. The binding changes
// under it, so it is not adopted: it is recorded Unknown and stops the resource
// until a History reading or an acknowledgment settles it, and it carries no name,
// because the release that dispatched it wrote none. The release notes say this.
func TestAnExecutorUpgradeRecordsANamelessRunningApplyAsUnknown(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, runShape{dispatcher: current, recorded: true})
	if run.migration.Status.ActiveOperation.Actor != "" {
		t.Fatal("the dispatched claim carries a name, so this row proves nothing about one without")
	}
	next := current
	next.ExecutorImage = "example.invalid/ptah@sha256:" + strings.Repeat("e", 64)
	reconciler, api := run.reconciler(t, next)

	for pass := 0; pass < 4; pass++ {
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(run.migration)); err != nil {
			t.Fatalf("Reconcile() pass %d error = %v", pass, err)
		}
	}
	actual := readMigration(t, api, run.migration)
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
		t.Fatalf("phase = %q, want Blocked; conditions %#v", actual.Status.Phase, actual.Status.Conditions)
	}
	if actual.Status.LastRun == nil || actual.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown {
		t.Fatalf("last run = %#v, want an unknown outcome", actual.Status.LastRun)
	}
	if actual.Status.UnresolvedRun == nil || actual.Status.UnresolvedRun.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown {
		t.Fatalf("unresolved run = %#v, want the run recorded as unknown", actual.Status.UnresolvedRun)
	}
}

// The controller-write webhook refuses the cleanup of a Job whose template is
// not the one its claim's admission snapshot recorded. A name in the template has
// to pass that on the way out, or a run that applied would sit in Applying on
// every cluster while the fake API server, which runs no admission, passed it.
func TestAFinishedNamedApplySettlesThroughTheControllerWriteWebhook(t *testing.T) {
	t.Parallel()

	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.Actor = loggedApprover
	builder := workloadBuilderForMigrations()
	job, pod := builtTerminalMigrationWorkload(t, builder, migration, plan)
	if carried, found := executorActor(t, job); !found || carried != loggedApprover {
		t.Fatalf("the finished Job carries %q (present %t), so this row proves nothing about a named one", carried, found)
	}
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationApply,
		OperationID: operation.ID, ChildExitCode: 0,
		CoordinationDigest:   operation.CoordinationDigest,
		TargetIdentityDigest: migration.Status.History.TargetIdentityDigest,
		MigrationRun: &dataplane.MigrationRunReport{
			ContractVersion: dataplane.SupportedMigrationRunContract,
			Direction:       "up",
			Outcome:         dataplane.MigrationOutcomeApplied,
			Planned:         []int64{3},
			Applied:         []int64{3},
		},
	})
	reconciler, api := fakeMigrationReconciler(
		t, staticLogs{content: frame}, migration, plan, job, pod, verificationPolicyConfigMap(),
	)
	reconciler.Jobs = builder
	reconciler.Client = admittedJobWrites(api, builder)
	holdMigrationApplyLease(t, reconciler, api, migration)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.LastRun == nil || actual.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomeApplied {
		t.Fatalf("the finished Apply was not recorded: phase=%q lastRun=%#v", actual.Status.Phase, actual.Status.LastRun)
	}
	if actual.Status.ActiveOperation != nil {
		t.Fatal("the finished Apply claim was retained")
	}
}

// An upgrade under a running Apply. The release before this one wrote a claim
// with no name and a Job with no variable, and the manager that takes over
// adopts it. A name derived again at build time would fail this: the run would
// read as outcome unknown across every upgrade.
func TestAManagerAdoptsTheNamelessRunItsPredecessorDispatched(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, row := range []struct {
		name   string
		policy operatorv1alpha1.ApplyPolicy
	}{
		{"an approved Apply", operatorv1alpha1.ApplyPolicyOnApproval},
		{"an Apply under the Always policy", operatorv1alpha1.ApplyPolicyAlways},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, runShape{
				dispatcher: managerOnlyPredecessor(current), policy: row.policy,
			})
			if run.migration.Status.ActiveOperation.Actor != "" {
				t.Fatal("the predecessor's claim carries a name, so this row proves nothing about one without")
			}
			if carried, found := executorActor(t, run.job); found {
				t.Fatalf("the predecessor's Job carries %q, so this row proves nothing about one without", carried)
			}
			reconciler, api := run.reconciler(t, current)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			assertAdopted(t, api, run)
			if actor := readMigration(t, api, run.migration).Status.ActiveOperation.Actor; actor != "" {
				t.Fatalf("adopting the run wrote the name %q into a claim that had none", actor)
			}
		})
	}
}

// The Job's template is immutable and its identity is held to the claim, so a
// Job under the reserved name that carries another name is not this claim's Job
// and is not trusted to have written the log the claim expects.
func TestAJobCarryingAnotherNameIsNotItsClaims(t *testing.T) {
	t.Parallel()

	current := workloadBuilderForMigrations()
	for _, claim := range []struct {
		name     string
		recorded bool
	}{
		{name: "adopting"},
		{name: "supervising", recorded: true},
	} {
		t.Run(claim.name, func(t *testing.T) {
			t.Parallel()

			run := runDispatchedBy(t, operatorv1alpha1.MigrationOperationApply, runShape{
				dispatcher: current,
				claim: func(operation *operatorv1alpha1.MigrationOperationStatus) {
					operation.Actor = loggedApprover
				},
				alter: func(job *batchv1.Job) {
					containers := job.Spec.Template.Spec.Containers
					for index := range containers {
						if containers[index].Name != executorContainerName {
							continue
						}
						for variable := range containers[index].Env {
							if containers[index].Env[variable].Name == workload.EnvMigrationActor {
								containers[index].Env[variable] = corev1.EnvVar{
									Name: workload.EnvMigrationActor, Value: "someone-else",
								}
								return
							}
						}
					}
					panic("the built Job carries no actor to change")
				},
				recorded: claim.recorded,
			})
			reconciler, api := run.reconciler(t, current)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(run.migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			assertRefused(t, api, run, "the Job's spec is not the one its claim builds")
		})
	}
}
