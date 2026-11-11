package controller

import (
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// TestSettleHarvestedJobCleanup holds the answer to a harvest's TTL write to the
// rule both families keep, with the stored claim read again only for a refusal:
// a refusal goes on while the stored claim names the Job, and fails the pass
// where it does not or where it cannot be read.
func TestSettleHarvestedJobCleanup(t *testing.T) {
	t.Parallel()

	forbidden := apierrors.NewForbidden(batchv1.Resource("jobs"), "job", errors.New("injected refusal"))
	invalid := apierrors.NewInvalid(batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), "job", field.ErrorList{
		field.Forbidden(field.NewPath("spec", "ttlSecondsAfterFinished"), "injected policy refusal"),
	})
	lost := errors.New("connection reset by peer")
	unread := errors.New("the API server did not answer")
	for _, row := range []struct {
		name    string
		answer  error
		claimed bool
		readErr error
		// asks is whether the stored claim is read again.
		asks bool
		// refused is whether the caller is told to name the Job.
		refused bool
		// fails lists what the pass's error carries, and is empty where the
		// pass goes on.
		fails []error
		// keeps is the status of the refusal the pass's error keeps, if any.
		keeps func(error) bool
	}{
		{name: "admitted"},
		{name: "deleted since it was read", answer: apierrors.NewNotFound(batchv1.Resource("jobs"), "job")},
		{name: "lost", answer: lost, fails: []error{lost}},
		{name: "refused while the claim names the Job", answer: forbidden, claimed: true, asks: true, refused: true},
		{name: "refused by a policy while the claim names the Job", answer: invalid, claimed: true, asks: true, refused: true},
		{name: "refused after the claim moved", answer: forbidden, asks: true, fails: []error{forbidden}, keeps: apierrors.IsForbidden},
		{
			name: "refused when the claim cannot be read again", answer: invalid, claimed: true, readErr: unread,
			asks: true, fails: []error{invalid, unread}, keeps: apierrors.IsInvalid,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			var asked, told bool
			err := settleHarvestedJobCleanup(row.answer, func() (bool, error) {
				asked = true
				return row.claimed, row.readErr
			}, func(refusal error) {
				told = true
				if refusal != row.answer {
					t.Fatalf("refused(%v), want the answer %v", refusal, row.answer)
				}
			})
			if asked != row.asks {
				t.Fatalf("read the stored claim again = %t, want %t", asked, row.asks)
			}
			if told != row.refused {
				t.Fatalf("named the Job = %t, want %t", told, row.refused)
			}
			if len(row.fails) == 0 {
				if err != nil {
					t.Fatalf("settleHarvestedJobCleanup() = %v, want the pass to go on", err)
				}
				return
			}
			for _, want := range row.fails {
				if !errors.Is(err, want) {
					t.Fatalf("settleHarvestedJobCleanup() = %v, want it to carry %v", err, want)
				}
			}
			if row.keeps != nil && !row.keeps(err) {
				t.Fatalf("settleHarvestedJobCleanup() = %v, want the refusal's status kept", err)
			}
		})
	}
}
