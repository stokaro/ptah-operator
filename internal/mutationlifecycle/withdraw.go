package mutationlifecycle

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// MarkState is a claim's dispatch mark as a direct read of its resource finds
// it.
type MarkState struct {
	// OperationID is the claim's ID, or empty when the resource holds no
	// claim.
	OperationID     string
	DispatchStarted bool
	JobUID          string
}

// ErrMarkMoved is a withdrawal that found the claim changed under it: another
// claim, a mark already cleared, or a recorded Job. Nothing is written then,
// and the next pass decides from the claim as it stands.
var ErrMarkMoved = errors.New("the dispatch mark changed before it was withdrawn")

// withdrawAttempts bounds the writes one withdrawal makes. Each conflict means
// another write landed between the re-read and the patch, so a resource
// written that often keeps its mark, and the claim is retired as an outcome
// nobody established once MissingJobGrace has passed.
const withdrawAttempts = 5

// WithdrawMark clears the dispatch mark of the claim operationID names, after
// the API server refused that claim's one create.
//
// The status patch carries the resourceVersion the mark was written at, and
// the create's admission can take as long as the Job-write webhook's timeout.
// Anything that writes the resource in that time -- an annotation, a label
// from a GitOps tool, a refresh request -- makes the patch conflict. Giving up
// there would keep the mark, and a mark without a Job is retired as unknown
// once the grace has passed, with the approval spent, although the API server
// said nothing was stored. So a conflict re-reads the resource and clears the
// mark again, as long as the read shows the same claim, still marked, with no
// Job recorded. A claim that moved is not written at all.
//
// withdraw sets the mark false on the resource in hand and patches it under an
// optimistic lock. reread replaces the resource in hand with a direct read,
// not a cached one, which may still hold the version that conflicted, and
// reports the mark it carries. Both are the family's own, because the two
// families keep their status in different fields.
func WithdrawMark(
	ctx context.Context,
	operationID string,
	withdraw func(context.Context) error,
	reread func(context.Context) (MarkState, error),
) error {
	for attempt := 1; ; attempt++ {
		err := withdraw(ctx)
		if err == nil || !apierrors.IsConflict(err) {
			return err
		}
		if attempt == withdrawAttempts {
			return fmt.Errorf("withdraw the dispatch mark: %d writes conflicted: %w", attempt, err)
		}
		mark, readErr := reread(ctx)
		if readErr != nil {
			return fmt.Errorf("re-read the claim after a conflicting withdrawal: %w", errors.Join(readErr, err))
		}
		if mark.OperationID != operationID || !mark.DispatchStarted || mark.JobUID != "" {
			return fmt.Errorf("%w: operation %q was refused, the resource holds operation %q with dispatchStarted %t and jobUID %q",
				ErrMarkMoved, operationID, mark.OperationID, mark.DispatchStarted, mark.JobUID)
		}
	}
}
