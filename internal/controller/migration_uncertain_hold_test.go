package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

func TestUncertainMigrationApplyRetainsItsClaimUntilTheExecutorStops(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name        string
		jobGone     bool
		jobRunning  bool
		deleting    bool
		suspended   bool
		readFailure string
	}{
		{name: "unfinished Job and running Pod", jobRunning: true},
		{name: "terminal Job with running Pod"},
		{name: "missing Job with running owned Pod", jobGone: true},
		{name: "deletion after the executor changed", jobRunning: true, deleting: true},
		{name: "suspension after the executor changed", jobRunning: true, suspended: true},
		{name: "Job read failure while the run is unknown", readFailure: "job"},
		{name: "Pod read failure while the run is unknown", readFailure: "pod"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			migration, plan := awaitingApprovalFixture(t)
			migration.Finalizers = []string{migrationOperationFinalizer}
			applyClaimFor(t, migration, plan)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			runningExecutorPod(pod)
			if row.jobRunning {
				job.Status.Conditions = nil
				job.Status.Active = 1
			}
			reconciler, api := uncertainApplyUnderBindingChange(t, migration, plan, job, pod)
			clock := &fixedClock{now: reconciler.now()}
			reconciler.Clock = clock.Now
			reconciler.Locks = targetlock.New(api, api, clock)
			if row.jobGone {
				if err := api.Delete(ctx, job); err != nil {
					t.Fatal(err)
				}
			}
			original := readMigration(t, api, migration)
			if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
				t.Fatal(err)
			}
			held := readMigration(t, api, migration)
			if held.Status.UnresolvedRun == nil || held.Status.LastRun == nil ||
				held.Status.LastRun.Outcome != operatorv1alpha1.MigrationRunOutcomeUnknown {
				t.Fatalf("the running Apply left no unknown-outcome evidence: %#v", held.Status)
			}
			// The deletion row checks survival directly, before requiring the
			// retained claim. Without the fix its next pass deletes the parent
			// even though the fake API still reports the original Pod running.
			if !row.deleting {
				assertUncertainMigrationHeld(t, original, held)
			}
			if row.readFailure != "" {
				reconciler.APIReader = interceptor.NewClient(api, interceptor.Funcs{
					Get: func(ctx context.Context, reader client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
						if _, job := object.(*batchv1.Job); job && row.readFailure == "job" {
							return errors.New("injected unknown Apply Job read failure")
						}
						return reader.Get(ctx, key, object, options...)
					},
					List: func(ctx context.Context, reader client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
						if _, pods := list.(*corev1.PodList); pods && row.readFailure == "pod" {
							return errors.New("injected unknown Apply Pod list failure")
						}
						return reader.List(ctx, list, options...)
					},
				})
			}
			if row.deleting {
				if err := api.Delete(ctx, held); err != nil {
					t.Fatal(err)
				}
			}
			if row.suspended {
				before := held.DeepCopy()
				held.Spec.Suspend = true
				if err := api.Patch(ctx, held, client.MergeFrom(before)); err != nil {
					t.Fatal(err)
				}
			}
			// Advance in increments shorter than the original Lease, through more
			// than its entire duration. A single kept holder cannot prove renewal.
			for i := 0; i < 4; i++ {
				clock.now = clock.now.Add(5 * time.Minute)
				result, err := reconciler.Reconcile(ctx, migrationRequest(migration))
				current := readMigration(t, api, migration)
				if err != nil || result.RequeueAfter <= 0 || result.RequeueAfter > 5*time.Second {
					t.Fatalf("the running unknown Apply was not promptly revisited: result=%#v err=%v", result, err)
				}
				assertUncertainMigrationHeld(t, original, current)
				if !equality.Semantic.DeepEqual(held.Status.LastRun, current.Status.LastRun) ||
					!equality.Semantic.DeepEqual(held.Status.UnresolvedRun, current.Status.UnresolvedRun) ||
					current.Annotations[operatorv1alpha1.UnresolvedRunAnnotation] != held.Annotations[operatorv1alpha1.UnresolvedRunAnnotation] {
					t.Fatal("waiting for the executor rewrote its terminal evidence")
				}
				leases := &coordinationv1.LeaseList{}
				if err := api.List(ctx, leases, client.InNamespace(reconciler.LockNamespace)); err != nil {
					t.Fatal(err)
				}
				if len(leases.Items) != 1 || leases.Items[0].Spec.RenewTime == nil ||
					!leases.Items[0].Spec.RenewTime.Time.Equal(clock.now) {
					t.Fatalf("the original realm was not renewed while its Pod could write: %#v", leases.Items)
				}
				assertDatabaseStillHeld(t, reconciler, api)
			}
			reconciler.APIReader = api
			if !row.jobGone {
				finished := &batchv1.Job{}
				if err := api.Get(ctx, client.ObjectKeyFromObject(job), finished); err != nil {
					t.Fatal(err)
				}
				finished.Status.Active = 0
				finished.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
				if err := api.Status().Update(ctx, finished); err != nil {
					t.Fatal(err)
				}
			}
			pod.Status.Phase = corev1.PodSucceeded
			pod.Status.ContainerStatuses = nil
			if err := api.Status().Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
			if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
				t.Fatal(err)
			}
			settled := readMigration(t, api, migration)
			if settled.Status.ActiveOperation != nil || !equality.Semantic.DeepEqual(settled.Status.LastRun, held.Status.LastRun) ||
				!equality.Semantic.DeepEqual(settled.Status.UnresolvedRun, held.Status.UnresolvedRun) {
				t.Fatalf("stopping the executor did not retire just its claim and preserve its unknown result: active=%#v lastRunEqual=%t unresolvedEqual=%t", settled.Status.ActiveOperation,
					equality.Semantic.DeepEqual(settled.Status.LastRun, held.Status.LastRun), equality.Semantic.DeepEqual(settled.Status.UnresolvedRun, held.Status.UnresolvedRun))
			}
			assertDatabaseHandedBack(t, reconciler, api)
			if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
				t.Fatal(err)
			}
			if row.deleting {
				gone := &operatorv1alpha1.PtahMigration{}
				if err := api.Get(ctx, client.ObjectKeyFromObject(migration), gone); !apierrors.IsNotFound(err) {
					t.Fatalf("the stopped executor kept the deletion: %v", err)
				}
			} else {
				rotated := readMigration(t, api, migration)
				if rotated.Status.ExecutionBinding.ExecutorImage != "example.invalid/ptah@"+testDigest ||
					rotated.Status.ExecutionBinding.Epoch == original.Status.ExecutionBinding.Epoch || rotated.Status.ActiveOperation != nil {
					t.Fatalf("the stopped executor prevented a fresh execution binding: %#v", rotated.Status)
				}
			}
		})
	}
}

func assertUncertainMigrationHeld(t *testing.T, original, current *operatorv1alpha1.PtahMigration) {
	t.Helper()
	if !equality.Semantic.DeepEqual(original.Status.ActiveOperation, current.Status.ActiveOperation) ||
		!equality.Semantic.DeepEqual(original.Status.ExecutionBinding, current.Status.ExecutionBinding) ||
		!contains(current.Finalizers, migrationOperationFinalizer) || current.Status.PendingLockRelease != nil ||
		current.Status.Plan != nil {
		t.Fatalf("the running executor lost its original claim, binding, finalizer or realm: %#v", current.Status)
	}
	if current.Status.UnresolvedRun == nil || current.Status.UnresolvedRun.RecordedAt == (metav1.Time{}) {
		t.Fatal("the retained claim carries no dated unresolved run")
	}
}
