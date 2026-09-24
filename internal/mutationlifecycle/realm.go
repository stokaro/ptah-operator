package mutationlifecycle

// A resource holds the database realm through one record at a time, and
// retiring a claim has to hand back the right one. Which record that is was
// spelled out at every site that retires a claim -- seven of them in the
// schema controller alone, in three shapes -- and both defects found in this
// area were one spelling that had drifted from its neighbours rather than a
// rule anybody got wrong.

// RealmOwner is the record that holds the database realm on a resource's
// behalf.
type RealmOwner string

const (
	// OwnerNone means the resource holds nothing. A claim that never took the
	// database has nothing to hand back.
	OwnerNone RealmOwner = "none"
	// OwnerClaim means the active claim's own epoch holds it.
	OwnerClaim RealmOwner = "claim"
	// OwnerProof means a pending observation holds it. The claim running
	// beside it is carrying out that proof, so retiring the claim releases
	// nothing: the realm is owed to the proof until the proof is settled.
	OwnerProof RealmOwner = "proof"
)

// RealmClaim describes the claim being retired and the proof it may be
// serving.
//
// Mutating says the claim may change the database, which is the only kind that
// holds the realm in its own right. ServesProof says the claim is the sort a
// pending observation carries out -- a read of the database, or the plan that
// read requires. Locked says the claim recorded an epoch of its own.
type RealmClaim struct {
	Mutating         bool
	ServesProof      bool
	Locked           bool
	ProofOutstanding bool
}

// RealmHeldBy reports which record a resource holds the database through, and
// therefore what retiring this claim owes.
//
// The proof comes first on purpose. A claim carrying out a proof borrows the
// realm the proof owns, so releasing on its behalf would hand the database to
// the next claimant while the proof it was taken for is still owed -- which is
// the one thing the realm is held for.
func RealmHeldBy(claim RealmClaim) RealmOwner {
	if claim.ProofOutstanding && claim.ServesProof {
		return OwnerProof
	}
	if claim.Locked && (claim.Mutating || claim.ServesProof) {
		return OwnerClaim
	}
	return OwnerNone
}
