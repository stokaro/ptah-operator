package controller

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// TestAnUnconfirmedMigrationApplyDispatchIsNeverRetried is the migration half
// of TestAnUnconfirmedApplyDispatchIsNeverRetried. The table beside this one
// covers every refusal that happens before the create; these are the two that
// happen at it and after it, and neither was covered.
//
// AlreadyExists belongs here rather than with the refusals. A Job standing
// under the name this claim reserved is one this claim may have created on a
// pass whose answer was lost, so retrying would rename the claim and dispatch
// a second executor beside the first. What the standing Job did is a question
// for the database.
//
// This family has two rows where the schema family has three, and the
// difference is not an omission: DispatchStarted is persisted before the
// create, so a failed read-back returns an error and the next pass re-enters
// through the claim's own verdict rather than settling here.
func TestAnUnconfirmedMigrationApplyDispatchIsNeverRetried(t *testing.T) {
	t.Parallel()

	lost := errors.New("connection reset by peer")

	for _, row := range []struct {
		name  string
		fault func(jobName string) *dispatchFaultClient
	}{
		{
			name: "the create never answered",
			fault: func(jobName string) *dispatchFaultClient {
				return &dispatchFaultClient{
					jobName: jobName,
					created: func(*batchv1.Job) error { return lost },
				}
			},
		},
		{
			name: "the created Job is not the one the claim describes",
			fault: func(jobName string) *dispatchFaultClient {
				return &dispatchFaultClient{
					jobName: jobName,
					mutate: func(job *batchv1.Job) {
						job.Annotations[workload.AnnotationExecutionBindingID] =
							"v1-99999999999999999999999999999999"
					},
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, plan := awaitingApprovalFixture(t)
			operation := undispatchedApplyClaim(t, migration, plan)
			if operation.JobName == "" {
				t.Fatal("the Apply claim reserved no Job name, so this proves nothing")
			}
			reconciler, api := fakeMigrationReconciler(t, staticLogs{},
				migration, plan, verificationPolicyConfigMap())
			fault := row.fault(operation.JobName)
			fault.Client = reconciler.Client
			reconciler.Client = fault

			actual := reconcileUntilTheApplyClaimIsGone(t, reconciler, api, migration)
			if actual.Status.ActiveOperation != nil {
				t.Fatalf("an unconfirmed Apply dispatch kept its claim: %#v", actual.Status.ActiveOperation)
			}
			if actual.Status.LastRun == nil ||
				actual.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown {
				t.Fatalf("last run = %#v, want an unknown outcome", actual.Status.LastRun)
			}
			if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked {
				t.Fatalf("phase = %q, want Blocked", actual.Status.Phase)
			}
			// The Job the create left behind is not renamed and not dispatched
			// beside: exactly one stands under the reserved name.
			jobs := &batchv1.JobList{}
			if err := api.List(context.Background(), jobs, client.InNamespace(migration.Namespace)); err != nil {
				t.Fatal(err)
			}
			if len(jobs.Items) > 1 {
				t.Fatalf("an unconfirmed dispatch produced %d Jobs", len(jobs.Items))
			}
		})
	}
}
