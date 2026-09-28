package mutationlifecycle_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	claim mutationlifecycle.Claim
	calls []string
	stop  string
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
func (f *familyTrace) Create(_ context.Context, job *batchv1.Job) error {
	f.calls = append(f.calls, "create")
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
