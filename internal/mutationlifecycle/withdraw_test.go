package mutationlifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// conflict is the error a family's status patch returns when the resource
// moved since the version it carries, wrapped the way both families wrap it.
func conflict() error {
	return fmt.Errorf("update PtahSchema status: %w", apierrors.NewConflict(
		schema.GroupResource{Group: "ptah.run", Resource: "ptahschemas"}, "orders",
		errors.New("the object has been modified; please apply your changes to the latest version and try again")))
}

// withdrawal is a family whose writes answer from a script.
type withdrawal struct {
	writes  []error
	reads   []mutationlifecycle.MarkState
	written int
	read    int
	readErr error
}

func (w *withdrawal) write(context.Context) error {
	w.written++
	if w.written > len(w.writes) {
		return nil
	}
	return w.writes[w.written-1]
}

func (w *withdrawal) reread(context.Context) (mutationlifecycle.MarkState, error) {
	w.read++
	if w.readErr != nil {
		return mutationlifecycle.MarkState{}, w.readErr
	}
	return w.reads[w.read-1], nil
}

func TestAWithdrawalRetriesOnlyForTheClaimItWasFor(t *testing.T) {
	t.Parallel()

	const claim = "apply-1"
	marked := mutationlifecycle.MarkState{OperationID: claim, DispatchStarted: true}
	refused := errors.New("the API server refused the request")
	for _, row := range []struct {
		name    string
		family  withdrawal
		wantErr func(error) bool
		written int
		read    int
	}{
		{
			name:    "the first write lands",
			wantErr: func(err error) bool { return err == nil },
			written: 1,
		},
		{
			name:    "a write landed during admission and the claim still stands",
			family:  withdrawal{writes: []error{conflict()}, reads: []mutationlifecycle.MarkState{marked}},
			wantErr: func(err error) bool { return err == nil },
			written: 2, read: 1,
		},
		{
			name: "two writes landed, one after the re-read",
			family: withdrawal{
				writes: []error{conflict(), conflict()},
				reads:  []mutationlifecycle.MarkState{marked, marked},
			},
			wantErr: func(err error) bool { return err == nil },
			written: 3, read: 2,
		},
		{
			name: "another claim replaced it",
			family: withdrawal{writes: []error{conflict()}, reads: []mutationlifecycle.MarkState{
				{OperationID: "apply-2", DispatchStarted: true},
			}},
			wantErr: func(err error) bool { return errors.Is(err, mutationlifecycle.ErrMarkMoved) },
			written: 1, read: 1,
		},
		{
			name: "the resource holds no claim",
			family: withdrawal{writes: []error{conflict()}, reads: []mutationlifecycle.MarkState{
				{},
			}},
			wantErr: func(err error) bool { return errors.Is(err, mutationlifecycle.ErrMarkMoved) },
			written: 1, read: 1,
		},
		{
			name: "the mark is already cleared",
			family: withdrawal{writes: []error{conflict()}, reads: []mutationlifecycle.MarkState{
				{OperationID: claim},
			}},
			wantErr: func(err error) bool { return errors.Is(err, mutationlifecycle.ErrMarkMoved) },
			written: 1, read: 1,
		},
		{
			name: "the claim recorded a Job",
			family: withdrawal{writes: []error{conflict()}, reads: []mutationlifecycle.MarkState{
				{OperationID: claim, DispatchStarted: true, JobUID: "job-uid"},
			}},
			wantErr: func(err error) bool { return errors.Is(err, mutationlifecycle.ErrMarkMoved) },
			written: 1, read: 1,
		},
		{
			name: "every write conflicts",
			family: withdrawal{
				writes: []error{conflict(), conflict(), conflict(), conflict(), conflict(), conflict(), conflict()},
				reads:  []mutationlifecycle.MarkState{marked, marked, marked, marked, marked, marked, marked},
			},
			wantErr: apierrors.IsConflict,
			written: 5, read: 4,
		},
		{
			name:    "the write fails for another reason",
			family:  withdrawal{writes: []error{refused}},
			wantErr: func(err error) bool { return errors.Is(err, refused) },
			written: 1,
		},
		{
			name:    "the re-read fails",
			family:  withdrawal{writes: []error{conflict()}, readErr: refused},
			wantErr: func(err error) bool { return errors.Is(err, refused) && apierrors.IsConflict(err) },
			written: 1, read: 1,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			family := row.family
			err := mutationlifecycle.WithdrawMark(context.Background(), claim, family.write, family.reread)
			if !row.wantErr(err) {
				t.Fatalf("WithdrawMark() = %v", err)
			}
			if family.written != row.written || family.read != row.read {
				t.Fatalf("WithdrawMark() wrote %d times and re-read %d times, want %d and %d",
					family.written, family.read, row.written, row.read)
			}
		})
	}
}
