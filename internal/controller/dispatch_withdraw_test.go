package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// slowRefusingAPI refuses the first Job create the way the API server does
// when the Job-write webhook does not answer within its timeout. Before it
// answers, touch can write the owning resource, as a person or a GitOps tool
// may while admission runs: that moves the resourceVersion the mark was
// written at. It counts the status writes the API server refused as
// conflicts, so a row can show it reached the conflict it is about.
type slowRefusingAPI struct {
	client.Client
	touch func(context.Context) error

	mu        sync.Mutex
	refused   bool
	conflicts int
}

func (a *slowRefusingAPI) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	a.mu.Lock()
	_, isJob := object.(*batchv1.Job)
	refuse := isJob && !a.refused
	if refuse {
		a.refused = true
	}
	a.mu.Unlock()
	if !refuse {
		return a.Client.Create(ctx, object, options...)
	}
	if a.touch != nil {
		if err := a.touch(ctx); err != nil {
			return fmt.Errorf("write the resource during admission: %w", err)
		}
	}
	return apierrors.NewInternalError(fmt.Errorf(`failed calling webhook "vcontrollerwrite.operator.ptah.run": failed to call webhook: ` +
		`Post "https://ptah-operator-webhook.ptah-system.svc:443/validate-operator-controller-write?timeout=30s": context deadline exceeded`))
}

func (a *slowRefusingAPI) Status() client.SubResourceWriter {
	return &conflictCountingStatus{SubResourceWriter: a.Client.Status(), api: a}
}

func (a *slowRefusingAPI) fired() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refused
}

func (a *slowRefusingAPI) statusConflicts() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conflicts
}

type conflictCountingStatus struct {
	client.SubResourceWriter
	api *slowRefusingAPI
}

func (w *conflictCountingStatus) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.SubResourcePatchOption,
) error {
	err := w.SubResourceWriter.Patch(ctx, object, patch, options...)
	if apierrors.IsConflict(err) {
		w.api.mu.Lock()
		w.api.conflicts++
		w.api.mu.Unlock()
	}
	return err
}

// annotate writes one annotation on the stored object key names, through the
// API server rather than through the reconciler's client.
func annotate[T client.Object](api client.Client, key client.ObjectKey, fresh func() T) func(context.Context) error {
	return func(ctx context.Context) error {
		object := fresh()
		if err := api.Get(ctx, key, object); err != nil {
			return err
		}
		annotations := object.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["example.com/touched"] = "during-admission"
		object.SetAnnotations(annotations)
		return api.Update(ctx, object)
	}
}

// withdrawRow is one family's claim driven to a refused create.
type withdrawRow struct {
	// refusedPass runs passes until the refused create fired and returns the
	// error of the pass it fired on.
	refusedPass func(t *testing.T) error
	// pass runs one more pass.
	pass func() error
	// claim reads the stored claim's mark and the annotation touch writes.
	claim func(t *testing.T) (id string, dispatchStarted bool, jobUID string, touched bool)
	// jobs counts the Jobs stored under the claim's name.
	jobs func(t *testing.T) int
	// api is what the reconciler writes through.
	api *slowRefusingAPI
}

// A refused create leaves no Job, so its claim takes the mark back and the
// next pass dispatches again under the approval it already spent. The
// withdrawal is a status patch under an optimistic lock from the version the
// mark was written at, and admission may take the Job-write webhook's whole
// timeout. A write to the resource in that time made the patch conflict, the
// mark stayed, and once MissingJobGrace had passed the claim was retired as an
// outcome nobody established -- for a create the API server had refused.
func TestARefusedCreateIsWithdrawnAfterAWriteDuringAdmission(t *testing.T) {
	t.Parallel()

	families := map[string]func(t *testing.T, touch bool) withdrawRow{
		"PtahSchema":    schemaWithdrawRow,
		"PtahMigration": migrationWithdrawRow,
	}
	for family, build := range families {
		for _, touch := range []bool{false, true} {
			name := family + "/nothing else wrote the resource"
			if touch {
				name = family + "/an annotation landed while admission ran"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				row := build(t, touch)
				id, marked, _, _ := row.claim(t)
				if id == "" || marked {
					t.Fatalf("the fixture is not an unmarked claim: id %q, dispatchStarted %t", id, marked)
				}

				err := row.refusedPass(t)
				if err == nil || !strings.Contains(err.Error(), "dispatch withdrawn") {
					t.Fatalf("the refused pass returned %v, want the withdrawal of a refused create", err)
				}
				afterID, afterMarked, afterUID, touched := row.claim(t)
				if afterID != id || afterMarked || afterUID != "" {
					t.Fatalf("after the refused create the claim reads id %q, dispatchStarted %t, jobUID %q; "+
						"want claim %q withdrawn", afterID, afterMarked, afterUID, id)
				}
				if jobs := row.jobs(t); jobs != 0 {
					t.Fatalf("%d Jobs stand under the claim's name after a refused create", jobs)
				}
				// The row is about the conflict, so it has to have met one,
				// and the row beside it must not have.
				if touched != touch {
					t.Fatalf("annotation written during admission: %t, want %t", touched, touch)
				}
				if conflicts := row.api.statusConflicts(); (conflicts > 0) != touch {
					t.Fatalf("%d status writes conflicted; want a conflict only when the resource was written during admission", conflicts)
				}

				// What the withdrawal is for: the same claim dispatches again
				// under the approval it already spent, and its Job runs.
				for range 4 {
					if _, _, uid, _ := row.claim(t); uid != "" {
						break
					}
					if err := row.pass(); err != nil {
						t.Fatalf("a pass after the withdrawal failed: %v", err)
					}
				}
				againID, againMarked, againUID, _ := row.claim(t)
				if againID != id || !againMarked || againUID == "" || row.jobs(t) != 1 {
					t.Fatalf("after the withdrawal the claim reads id %q, dispatchStarted %t, jobUID %q with %d Jobs; "+
						"want claim %q dispatched with one Job", againID, againMarked, againUID, row.jobs(t), id)
				}
			})
		}
	}
}

func schemaWithdrawRow(t *testing.T, touch bool) withdrawRow {
	t.Helper()

	reconciler, base, schema, jobName := dispatchableApply(t, true)
	key := client.ObjectKeyFromObject(schema)
	api := &slowRefusingAPI{Client: reconciler.Client}
	if touch {
		api.touch = annotate(base, key, func() *operatorv1alpha1.PtahSchema { return &operatorv1alpha1.PtahSchema{} })
	}
	reconciler.Client = api
	pass := func() error {
		_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		return err
	}
	return withdrawRow{
		api:         api,
		pass:        pass,
		refusedPass: func(t *testing.T) error { return passUntilRefused(t, api, pass) },
		claim: func(t *testing.T) (string, bool, string, bool) {
			stored := safetyGetSchema(t, base, schema)
			operation := stored.Status.ActiveOperation
			if operation == nil {
				return "", false, "", false
			}
			_, touched := stored.Annotations["example.com/touched"]
			return operation.ID, operation.DispatchStarted, string(operation.JobUID), touched
		},
		jobs: func(t *testing.T) int { return countJobs(t, base, schema.Namespace, jobName) },
	}
}

func migrationWithdrawRow(t *testing.T, touch bool) withdrawRow {
	t.Helper()

	migration, plan := awaitingApprovalFixture(t)
	approval := migrationApprovalFor(migration, plan)
	reconciler, base := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval, verificationPolicyConfigMap())
	request := migrationRequest(migration)
	// The first pass claims the approved Apply and dispatches nothing.
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("claim the approved Apply: %v", err)
	}
	api := &slowRefusingAPI{Client: reconciler.Client}
	if touch {
		api.touch = annotate(base, request.NamespacedName,
			func() *operatorv1alpha1.PtahMigration { return &operatorv1alpha1.PtahMigration{} })
	}
	reconciler.Client = api
	pass := func() error {
		_, err := reconciler.Reconcile(context.Background(), request)
		return err
	}
	return withdrawRow{
		api:         api,
		pass:        pass,
		refusedPass: func(t *testing.T) error { return passUntilRefused(t, api, pass) },
		claim: func(t *testing.T) (string, bool, string, bool) {
			stored := readMigration(t, base, migration)
			operation := stored.Status.ActiveOperation
			if operation == nil {
				return "", false, "", false
			}
			_, touched := stored.Annotations["example.com/touched"]
			return operation.ID, operation.DispatchStarted, string(operation.JobUID), touched
		},
		jobs: func(t *testing.T) int {
			stored := readMigration(t, base, migration)
			if stored.Status.ActiveOperation == nil {
				return 0
			}
			return countJobs(t, base, migration.Namespace, stored.Status.ActiveOperation.JobName)
		},
	}
}

// passUntilRefused runs passes until the refused create fired, and fails a
// fixture that does not get there or errs on the way.
func passUntilRefused(t *testing.T, api *slowRefusingAPI, pass func() error) error {
	t.Helper()
	for range 8 {
		err := pass()
		if api.fired() {
			return err
		}
		if err != nil {
			t.Fatalf("a pass before dispatch failed: %v", err)
		}
	}
	t.Fatal("eight passes did not reach the Job create")
	return nil
}
