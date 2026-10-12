package mutationlifecycle_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// The order is the contract the mutation-lifecycle page states, written out
// here so that moving a step is a change to this table as well.
func TestTheDispatchOrderIsTheOneThePageStates(t *testing.T) {
	t.Parallel()

	want := []mutationlifecycle.Step{
		mutationlifecycle.StepAuthorize,
		mutationlifecycle.StepLease,
		mutationlifecycle.StepStage,
		mutationlifecycle.StepSnapshot,
		mutationlifecycle.StepConsume,
		mutationlifecycle.StepMark,
		mutationlifecycle.StepCreate,
		mutationlifecycle.StepConfirm,
		mutationlifecycle.StepRecord,
	}
	if got := mutationlifecycle.Order(); !slices.Equal(got, want) {
		t.Fatalf("Order() = %v, want %v", got, want)
	}
	// A caller cannot reorder the driver through the slice it was handed.
	handed := mutationlifecycle.Order()
	handed[0], handed[1] = handed[1], handed[0]
	if got := mutationlifecycle.Order(); !slices.Equal(got, want) {
		t.Fatalf("Order() after a caller changed its copy = %v, want %v", got, want)
	}
}

// familyTrace is a family whose every step proceeds and whose Job is created,
// read back and recorded, and which writes down the order it was asked in.
type familyTrace struct {
	claim       mutationlifecycle.Claim
	calls       []string
	stop        string
	createErr   error
	withdrawErr error
}

func (f *familyTrace) called(name string) mutationlifecycle.Outcome {
	f.calls = append(f.calls, name)
	if name == f.stop {
		return mutationlifecycle.Stop(reconcile.Result{}, errors.New("stopped by the test"))
	}
	return mutationlifecycle.Proceed()
}

func (f *familyTrace) Claim() mutationlifecycle.Claim { return f.claim }
func (f *familyTrace) Authorize(context.Context) mutationlifecycle.Outcome {
	return f.called("authorize")
}
func (f *familyTrace) AcquireLease(context.Context) mutationlifecycle.Outcome {
	return f.called("lease")
}
func (f *familyTrace) Stage(context.Context) mutationlifecycle.Outcome { return f.called("stage") }
func (f *familyTrace) Build(context.Context) (*batchv1.Job, mutationlifecycle.Outcome) {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "job"}}, f.called("build")
}
func (f *familyTrace) Admission() mutationlifecycle.Admission { return traceAdmission{} }
func (f *familyTrace) SaveSnapshot(context.Context, *operatorv1alpha1.PodAdmissionSnapshot) (reconcile.Result, error) {
	f.calls = append(f.calls, "save snapshot")
	return reconcile.Result{}, nil
}
func (f *familyTrace) RefreshSnapshot(context.Context) (reconcile.Result, error) {
	f.calls = append(f.calls, "refresh snapshot")
	return reconcile.Result{}, nil
}
func (f *familyTrace) Refuse(context.Context, mutationlifecycle.Refusal, error) (reconcile.Result, error) {
	f.calls = append(f.calls, "refuse")
	return reconcile.Result{}, nil
}
func (f *familyTrace) Consume(context.Context) mutationlifecycle.Outcome { return f.called("consume") }
func (f *familyTrace) Mark(context.Context) error {
	f.called("mark")
	if f.stop == "mark" {
		return errors.New("stopped by the test")
	}
	return nil
}
func (f *familyTrace) Withdraw(context.Context) error {
	f.calls = append(f.calls, "withdraw")
	return f.withdrawErr
}
func (f *familyTrace) Create(_ context.Context, job *batchv1.Job) error {
	f.calls = append(f.calls, "create")
	if f.createErr != nil {
		return f.createErr
	}
	job.UID = "job-uid"
	return nil
}
func (f *familyTrace) Confirm(_ context.Context, job *batchv1.Job) (*batchv1.Job, error) {
	f.calls = append(f.calls, "confirm")
	return job.DeepCopy(), nil
}
func (f *familyTrace) Intent(*batchv1.Job, *batchv1.Job) error { return nil }
func (f *familyTrace) Record(context.Context, *batchv1.Job) (reconcile.Result, error) {
	f.calls = append(f.calls, "record")
	return reconcile.Result{}, nil
}
func (f *familyTrace) Unaccounted(context.Context, *batchv1.Job, error) (reconcile.Result, error) {
	f.calls = append(f.calls, "unaccounted")
	return reconcile.Result{}, nil
}
func (f *familyTrace) Retry(context.Context, error) (reconcile.Result, error) {
	f.calls = append(f.calls, "retry")
	return reconcile.Result{}, nil
}

type traceAdmission struct{}

func (traceAdmission) Resolve(context.Context, *corev1.PodTemplateSpec) (*operatorv1alpha1.PodAdmissionSnapshot, error) {
	return &operatorv1alpha1.PodAdmissionSnapshot{}, nil
}
func (traceAdmission) Validate(*operatorv1alpha1.PodAdmissionSnapshot) error { return nil }
func (traceAdmission) Digest(*corev1.PodTemplateSpec) (string, error)        { return "template", nil }

func mutatingClaim() mutationlifecycle.Claim {
	return mutationlifecycle.Claim{
		Type: "Apply", Mutating: true, HoldsLock: true,
		Snapshot: &operatorv1alpha1.PodAdmissionSnapshot{TemplateDigest: "template"},
	}
}

func TestADispatchCallsItsFamilyInOrder(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		driver mutationlifecycle.Driver
		want   []string
	}{
		{
			name:   "the order",
			driver: mutationlifecycle.Driver{},
			want:   []string{"authorize", "lease", "stage", "build", "consume", "mark", "create", "confirm", "record"},
		},
		{
			name:   "consume and mark exchanged",
			driver: mutationlifecycle.Driver{}.Swapped(mutationlifecycle.StepConsume, mutationlifecycle.StepMark),
			want:   []string{"authorize", "lease", "stage", "build", "mark", "consume", "create", "confirm", "record"},
		},
		{
			// A step the sequence does not have leaves it as it was.
			name:   "a step that is not in the sequence",
			driver: mutationlifecycle.Driver{}.Swapped(mutationlifecycle.StepMark, mutationlifecycle.Step(99)),
			want:   []string{"authorize", "lease", "stage", "build", "consume", "mark", "create", "confirm", "record"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			family := &familyTrace{claim: mutatingClaim()}
			if _, err := row.driver.Dispatch(context.Background(), family); err != nil {
				t.Fatalf("Dispatch() error = %v", err)
			}
			if !slices.Equal(family.calls, row.want) {
				t.Fatalf("the family was asked %v, want %v", family.calls, row.want)
			}
		})
	}
}

// A step that stops the pass ends it: nothing after it runs, least of all a
// create.
func TestAStepThatStopsEndsThePass(t *testing.T) {
	t.Parallel()

	family := &familyTrace{claim: mutatingClaim(), stop: "mark"}
	if _, err := (mutationlifecycle.Driver{}).Dispatch(context.Background(), family); err == nil {
		t.Fatal("a mark that failed let the pass report success")
	}
	if slices.Contains(family.calls, "create") {
		t.Fatalf("a pass that stopped at the mark went on to %v", family.calls)
	}
}

// A mutating claim whose Job may exist is an outcome nobody established, and
// the driver refuses it before asking its family anything.
func TestAMutatingClaimThatMayHaveDispatchedIsNeverDispatchedAgain(t *testing.T) {
	t.Parallel()

	for _, dispatch := range []mutationlifecycle.DispatchState{
		{DispatchStarted: true},
		{JobUID: "job-uid"},
	} {
		claim := mutatingClaim()
		claim.Dispatch = dispatch
		family := &familyTrace{claim: claim}
		if _, err := (mutationlifecycle.Driver{}).Dispatch(context.Background(), family); !errors.Is(err, mutationlifecycle.ErrMayHaveDispatched) {
			t.Fatalf("Dispatch(%+v) error = %v, want ErrMayHaveDispatched", dispatch, err)
		}
		if len(family.calls) != 0 {
			t.Fatalf("Dispatch(%+v) asked its family %v", dispatch, family.calls)
		}
	}
	// The control: a read-only claim that recorded a UID is the family's to
	// retry, and the driver asks.
	claim := mutationlifecycle.Claim{Type: "Plan", HoldsLock: true, Dispatch: mutationlifecycle.DispatchState{JobUID: "job-uid"}}
	family := &familyTrace{claim: claim}
	if _, err := (mutationlifecycle.Driver{}).Dispatch(context.Background(), family); err != nil {
		t.Fatalf("Dispatch(read-only) error = %v", err)
	}
	if len(family.calls) == 0 {
		t.Fatal("a read-only claim was refused like a mutating one")
	}
}

// shutdownTrace cancels the reconcile's context the moment the claim is
// marked, as a manager stopping right then does, and remembers whether the
// steps after the mark still had a live context.
type shutdownTrace struct {
	familyTrace
	cancel      context.CancelFunc
	createAlive bool
}

func (f *shutdownTrace) Mark(ctx context.Context) error {
	err := f.familyTrace.Mark(ctx)
	f.cancel()
	return err
}

func (f *shutdownTrace) Create(ctx context.Context, job *batchv1.Job) error {
	f.createAlive = ctx.Err() == nil
	if !f.createAlive {
		f.calls = append(f.calls, "create interrupted")
		return ctx.Err()
	}
	return f.familyTrace.Create(ctx, job)
}

// The 0.2.1 restart burst left an approved migration Blocked: a manager that
// was stopping marked the Apply and lost its create, and a marked claim with
// no Job is a run only a person can account for. A dispatch that has started
// now finishes, and a manager already stopping does not start one.
func TestADispatchThatStartedFinishesWhenTheManagerStops(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	family := &shutdownTrace{familyTrace: familyTrace{claim: mutatingClaim()}, cancel: cancel}
	if _, err := (mutationlifecycle.Driver{}).Dispatch(ctx, family); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if !family.createAlive || slices.Contains(family.calls, "unaccounted") {
		t.Fatalf("the shutdown interrupted a dispatch it had already marked: %v", family.calls)
	}
	if want := []string{"authorize", "lease", "stage", "build", "consume", "mark", "create", "confirm", "record"}; !slices.Equal(family.calls, want) {
		t.Fatalf("the family was asked %v, want %v", family.calls, want)
	}
}

func TestAStoppingManagerDoesNotStartADispatch(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	family := &familyTrace{claim: mutatingClaim()}
	if _, err := (mutationlifecycle.Driver{}).Dispatch(ctx, family); !errors.Is(err, context.Canceled) {
		t.Fatalf("Dispatch() on a stopping manager error = %v, want context.Canceled", err)
	}
	if slices.Contains(family.calls, "mark") || slices.Contains(family.calls, "create") {
		t.Fatalf("a stopping manager started a dispatch: %v", family.calls)
	}
}

// The 0.2.2 restart burst replaced every manager while an approval landed:
// the stopping leader marked the Apply and its create was refused, because
// the manager's own Job-write webhook had no endpoint left. A refused create
// stored nothing, so the claim takes its mark back instead of becoming a run
// only a person can account for, and the next pass makes the create under the
// approval it already spent.
func TestARefusedCreateWithdrawsTheMark(t *testing.T) {
	t.Parallel()

	family := &familyTrace{claim: mutatingClaim(), createErr: webhookUnreachable()}
	if _, err := (mutationlifecycle.Driver{}).Dispatch(context.Background(), family); err == nil {
		t.Fatal("a refused create let the pass report success")
	}
	if want := []string{"authorize", "lease", "stage", "build", "consume", "mark", "create", "withdraw"}; !slices.Equal(family.calls, want) {
		t.Fatalf("the family was asked %v, want %v", family.calls, want)
	}
}

// Everything else a create can answer may follow the write, and the claim is
// retired as before: withdrawing would let the next pass create beside a Job
// that may be running.
func TestAnUncertainCreateIsStillUnaccounted(t *testing.T) {
	t.Parallel()

	for _, createErr := range []error{
		apierrors.NewServerTimeout(schema.GroupResource{Group: "batch", Resource: "jobs"}, "create", 1),
		apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, "job"),
		context.DeadlineExceeded,
	} {
		family := &familyTrace{claim: mutatingClaim(), createErr: createErr}
		if _, err := (mutationlifecycle.Driver{}).Dispatch(context.Background(), family); err != nil {
			t.Fatalf("Dispatch() with create error %v = %v, want the claim retired", createErr, err)
		}
		if slices.Contains(family.calls, "withdraw") || !slices.Contains(family.calls, "unaccounted") {
			t.Fatalf("a create that answered %v led to %v", createErr, family.calls)
		}
	}
}

// A withdrawal that does not land leaves the mark, and nothing else in this
// pass decides the claim: the next pass finds a marked claim with no Job and
// retires it, which is what a refused create did before withdrawal existed.
func TestAWithdrawalThatFailsLeavesTheClaimToTheNextPass(t *testing.T) {
	t.Parallel()

	family := &familyTrace{claim: mutatingClaim(), createErr: webhookUnreachable(), withdrawErr: errors.New("status write failed")}
	if _, err := (mutationlifecycle.Driver{}).Dispatch(context.Background(), family); err == nil {
		t.Fatal("a failed withdrawal let the pass report success")
	}
	if slices.Contains(family.calls, "unaccounted") || slices.Contains(family.calls, "retry") {
		t.Fatalf("a pass whose withdrawal failed went on to %v", family.calls)
	}
}

// refusedShutdownTrace is the restart burst itself: the manager starts
// stopping the moment the claim is marked, and the create is refused.
type refusedShutdownTrace struct {
	familyTrace
	cancel        context.CancelFunc
	withdrawAlive bool
}

func (f *refusedShutdownTrace) Mark(ctx context.Context) error {
	err := f.familyTrace.Mark(ctx)
	f.cancel()
	return err
}

func (f *refusedShutdownTrace) Withdraw(ctx context.Context) error {
	f.withdrawAlive = ctx.Err() == nil
	return f.familyTrace.Withdraw(ctx)
}

func TestAStoppingManagerStillWithdrawsARefusedDispatch(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	family := &refusedShutdownTrace{familyTrace: familyTrace{claim: mutatingClaim(), createErr: webhookUnreachable()}, cancel: cancel}
	if _, err := (mutationlifecycle.Driver{}).Dispatch(ctx, family); err == nil {
		t.Fatal("a refused create let the pass report success")
	}
	if !family.withdrawAlive || slices.Contains(family.calls, "unaccounted") {
		t.Fatalf("a stopping manager could not take back its refused dispatch: %v", family.calls)
	}
}
