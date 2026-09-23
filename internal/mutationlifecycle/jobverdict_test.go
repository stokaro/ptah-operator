package mutationlifecycle_test

import (
	"testing"

	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// The whole table, both costs. Each row is a state a pass can be in, and the
// verdict is what both families do about it.
//
// The rows that earn the table are the ones where Mutating alone changes the
// answer: a missing Job, a replaced Job, a disowned Job. Those are the three
// places a wrong verb runs SQL a second time, and they are the three places a
// reader comparing two controllers by eye would not notice one.
func TestTheJobVerdictTable(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		claim mutationlifecycle.JobClaim
		want  mutationlifecycle.JobVerdict
		cause mutationlifecycle.JobCause
	}{
		{
			name:  "nothing created and nothing may have been",
			claim: mutationlifecycle.JobClaim{Mutating: true},
			want:  mutationlifecycle.VerdictDispatch, cause: mutationlifecycle.CauseMissing,
		},
		{
			name:  "a read-only claim whose Job is missing",
			claim: mutationlifecycle.JobClaim{DispatchStarted: true},
			want:  mutationlifecycle.VerdictDispatch, cause: mutationlifecycle.CauseMissing,
		},
		{
			name:  "a create whose response never arrived",
			claim: mutationlifecycle.JobClaim{Mutating: true, DispatchStarted: true},
			want:  mutationlifecycle.VerdictUnaccounted,
			cause: mutationlifecycle.CauseMissing,
		},
		{
			name:  "a mutating claim whose confirmed Job is gone",
			claim: mutationlifecycle.JobClaim{Mutating: true, RecordedJobUID: "job"},
			want:  mutationlifecycle.VerdictUnaccounted, cause: mutationlifecycle.CauseMissing,
		},
		{
			name: "a mutating claim whose Job was replaced",
			claim: mutationlifecycle.JobClaim{
				Mutating: true, RecordedJobUID: "job",
				Found: true, FoundJobUID: "other", OwnedExactly: true,
			},
			want:  mutationlifecycle.VerdictUnaccounted,
			cause: mutationlifecycle.CauseReplaced,
		},
		{
			name: "a read-only claim whose Job was replaced",
			claim: mutationlifecycle.JobClaim{
				RecordedJobUID: "job", Found: true, FoundJobUID: "other", OwnedExactly: true,
			},
			want:  mutationlifecycle.VerdictRetry,
			cause: mutationlifecycle.CauseReplaced,
		},
		{
			name: "a mutating claim whose Job lost its owner",
			claim: mutationlifecycle.JobClaim{
				Mutating: true, RecordedJobUID: "job",
				Found: true, FoundJobUID: "job", OwnedExactly: false,
			},
			want:  mutationlifecycle.VerdictUnaccounted,
			cause: mutationlifecycle.CauseDisowned,
		},
		{
			name: "a read-only claim whose Job lost its owner",
			claim: mutationlifecycle.JobClaim{
				RecordedJobUID: "job", Found: true, FoundJobUID: "job", OwnedExactly: false,
			},
			want:  mutationlifecycle.VerdictRetry,
			cause: mutationlifecycle.CauseDisowned,
		},
		{
			name: "a Job created before its UID was recorded",
			claim: mutationlifecycle.JobClaim{
				Mutating: true, DispatchStarted: true,
				Found: true, FoundJobUID: "job", OwnedExactly: true,
			},
			want: mutationlifecycle.VerdictAdopt,
		},
		{
			name: "the ordinary supervising pass",
			claim: mutationlifecycle.JobClaim{
				Mutating: true, DispatchStarted: true, RecordedJobUID: "job",
				Found: true, FoundJobUID: "job", OwnedExactly: true,
			},
			want: mutationlifecycle.VerdictSupervise,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, cause := mutationlifecycle.VerdictFor(row.claim)
			if got != row.want {
				t.Errorf("VerdictFor() = %q, want %q", got, row.want)
			}
			if cause != row.cause {
				t.Errorf("VerdictFor() cause = %q, want %q", cause, row.cause)
			}
		})
	}
}

// A replaced Job is judged before ownership, because a replacement can be
// owned by this resource and still not be the Job the claim dispatched.
// Judging ownership first would call it owned and supervise something else.
func TestAReplacedJobIsJudgedBeforeOwnership(t *testing.T) {
	t.Parallel()
	verdict, cause := mutationlifecycle.VerdictFor(mutationlifecycle.JobClaim{
		Mutating: true, RecordedJobUID: "job",
		Found: true, FoundJobUID: "replacement", OwnedExactly: true,
	})
	if verdict != mutationlifecycle.VerdictUnaccounted || cause != mutationlifecycle.CauseReplaced {
		t.Fatalf("a replaced but owned Job produced %q/%q, want %q/%q",
			verdict, cause, mutationlifecycle.VerdictUnaccounted, mutationlifecycle.CauseReplaced)
	}
}
