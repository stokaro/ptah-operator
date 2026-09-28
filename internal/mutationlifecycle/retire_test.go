package mutationlifecycle_test

import (
	"testing"

	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// What the write that retires a claim hands back, one row per way a claim
// leaves. Each row is a retirement one of the two families performs, named for
// it; the ones that release nothing are the ones that matter most, because a
// release there hands the database to the next claimant under a live
// executor or under a proof that is still owed.
func TestWhatRetiringAClaimHandsBack(t *testing.T) {
	t.Parallel()

	apply := mutationlifecycle.RealmClaim{Mutating: true, Locked: true}
	plan := mutationlifecycle.RealmClaim{ServesProof: true, Locked: true}
	proving := mutationlifecycle.RealmClaim{ServesProof: true, Locked: true, ProofOutstanding: true}
	for _, row := range []struct {
		name       string
		retirement mutationlifecycle.Retirement
		want       mutationlifecycle.RealmOwner
	}{
		{
			name: "an Apply discarded before it dispatched",
			retirement: mutationlifecycle.Retirement{
				Claim: apply, Disposition: mutationlifecycle.DispositionDiscard, RecordHoldsRealm: true,
			},
			want: mutationlifecycle.OwnerClaim,
		},
		{
			// The pending observation inherits the epoch and proves the run
			// under it.
			name: "a schema Apply that completed",
			retirement: mutationlifecycle.Retirement{
				Claim: apply, Disposition: mutationlifecycle.DispositionAccounted, RecordHoldsRealm: true,
			},
			want: mutationlifecycle.OwnerNone,
		},
		{
			name: "a schema Apply nobody accounted for",
			retirement: mutationlifecycle.Retirement{
				Claim: apply, Disposition: mutationlifecycle.DispositionUnaccounted, RecordHoldsRealm: true,
			},
			want: mutationlifecycle.OwnerNone,
		},
		{
			name: "a migration run whose result was read",
			retirement: mutationlifecycle.Retirement{
				Claim: apply, Disposition: mutationlifecycle.DispositionAccounted,
			},
			want: mutationlifecycle.OwnerClaim,
		},
		{
			name: "a migration run nobody accounted for, once nothing it dispatched can write",
			retirement: mutationlifecycle.Retirement{
				Claim: apply, Disposition: mutationlifecycle.DispositionUnaccounted,
			},
			want: mutationlifecycle.OwnerClaim,
		},
		{
			// The Lease outlives the executor by its own deadline, and handing
			// it back now starts the next claimant beside a Pod that may still
			// be running SQL.
			name: "a migration run nobody accounted for, whose Pod may still write",
			retirement: mutationlifecycle.Retirement{
				Claim: apply, Disposition: mutationlifecycle.DispositionUnaccounted, MayStillWrite: true,
			},
			want: mutationlifecycle.OwnerNone,
		},
		{
			name: "a Plan that finished with no proof outstanding",
			retirement: mutationlifecycle.Retirement{
				Claim: plan, Disposition: mutationlifecycle.DispositionAccounted, RecordHoldsRealm: true,
			},
			want: mutationlifecycle.OwnerClaim,
		},
		{
			name: "a Plan discarded with no proof outstanding",
			retirement: mutationlifecycle.Retirement{
				Claim: plan, Disposition: mutationlifecycle.DispositionDiscard, RecordHoldsRealm: true,
			},
			want: mutationlifecycle.OwnerClaim,
		},
		{
			name: "a Plan that settles the proof it was carrying out",
			retirement: mutationlifecycle.Retirement{
				Claim: proving, Disposition: mutationlifecycle.DispositionAccounted, RecordHoldsRealm: true,
				EndsProof: true,
			},
			want: mutationlifecycle.OwnerProof,
		},
		{
			// An Observe hands the proof on to the Plan it requires.
			name: "an Observe that finished carrying out a proof",
			retirement: mutationlifecycle.Retirement{
				Claim: proving, Disposition: mutationlifecycle.DispositionAccounted, RecordHoldsRealm: true,
			},
			want: mutationlifecycle.OwnerNone,
		},
		{
			name: "a proof claim discarded while the proof is still owed",
			retirement: mutationlifecycle.Retirement{
				Claim: proving, Disposition: mutationlifecycle.DispositionDiscard, RecordHoldsRealm: true,
			},
			want: mutationlifecycle.OwnerNone,
		},
		{
			name: "a claim that never took the database",
			retirement: mutationlifecycle.Retirement{
				Claim: mutationlifecycle.RealmClaim{}, Disposition: mutationlifecycle.DispositionAccounted,
			},
			want: mutationlifecycle.OwnerNone,
		},
		{
			// A claim of a type this binary does not define is neither
			// mutating nor serving a proof, so it holds nothing in its own
			// right, whatever epoch it carries.
			name: "a claim of an unknown type that carries an epoch",
			retirement: mutationlifecycle.Retirement{
				Claim: mutationlifecycle.RealmClaim{Locked: true}, Disposition: mutationlifecycle.DispositionDiscard,
			},
			want: mutationlifecycle.OwnerNone,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			if got := row.retirement.Releases(); got != row.want {
				t.Fatalf("%+v releases %q, want %q", row.retirement, got, row.want)
			}
		})
	}
}
