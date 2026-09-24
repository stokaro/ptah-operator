package mutationlifecycle_test

import (
	"testing"

	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// Retiring a claim hands back the database, and which record it hands back is
// the question every site that retires one has to answer. Getting it wrong
// costs in both directions: releasing what a proof still owns hands the
// database to the next claimant while the proof it was taken for is owed, and
// releasing nothing where the claim owned it leaves the realm held by an
// operation that no longer exists.
func TestWhichRecordHoldsTheRealm(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		claim mutationlifecycle.RealmClaim
		want  mutationlifecycle.RealmOwner
	}{
		{
			// The only kind that holds the realm in its own right.
			name:  "an Apply that took the database",
			claim: mutationlifecycle.RealmClaim{Mutating: true, Locked: true},
			want:  mutationlifecycle.OwnerClaim,
		},
		{
			name:  "a Plan with no proof outstanding",
			claim: mutationlifecycle.RealmClaim{ServesProof: true, Locked: true},
			want:  mutationlifecycle.OwnerClaim,
		},
		{
			// It borrowed the realm the proof owns.
			name: "a Plan carrying out a proof",
			claim: mutationlifecycle.RealmClaim{
				ServesProof: true, Locked: true, ProofOutstanding: true,
			},
			want: mutationlifecycle.OwnerProof,
		},
		{
			name: "an observation carrying out a proof",
			claim: mutationlifecycle.RealmClaim{
				ServesProof: true, Locked: true, ProofOutstanding: true,
			},
			want: mutationlifecycle.OwnerProof,
		},
		{
			// A proof outstanding with no claim beside it: the proof still
			// owns the realm, and nothing is being retired that could release
			// it on the proof's behalf.
			name:  "a proof with no claim beside it",
			claim: mutationlifecycle.RealmClaim{ProofOutstanding: true},
			want:  mutationlifecycle.OwnerNone,
		},
		{
			// A verification or a resolution never opens the database.
			name:  "a claim that never took the database",
			claim: mutationlifecycle.RealmClaim{},
			want:  mutationlifecycle.OwnerNone,
		},
		{
			// Recorded no epoch, so there is no holder to name even though
			// this kind of claim would hold one.
			name:  "an Apply that has not taken it yet",
			claim: mutationlifecycle.RealmClaim{Mutating: true},
			want:  mutationlifecycle.OwnerNone,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			if got := mutationlifecycle.RealmHeldBy(row.claim); got != row.want {
				t.Fatalf("RealmHeldBy(%+v) = %q, want %q", row.claim, got, row.want)
			}
		})
	}
}

// The property behind the first row of the table, stated over every claim: a
// proof that is still owed keeps the realm, whatever claim is running beside
// it, unless that claim holds an epoch of its own that the proof never took.
func TestAnOutstandingProofIsNeverReleasedByTheClaimServingIt(t *testing.T) {
	t.Parallel()

	for _, locked := range []bool{false, true} {
		claim := mutationlifecycle.RealmClaim{
			ServesProof: true, Locked: locked, ProofOutstanding: true,
		}
		if got := mutationlifecycle.RealmHeldBy(claim); got != mutationlifecycle.OwnerProof {
			t.Fatalf("a claim carrying out a proof (locked=%t) = %q, want the proof to keep it",
				locked, got)
		}
	}
}
