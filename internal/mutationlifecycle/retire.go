package mutationlifecycle

// A claim is retired one of three ways, and what the write that drops it owes
// the database realm depends on which. The two families used to decide it at
// every site that retired a claim: by the realm's owner at some, by naming a
// type at others, and in the migration family by releasing after the write at
// one site and before it at another. So what a retirement owed depended on
// which site retired it, and one window -- a migration run retired as
// uncertain -- was not covered by a record at all.
//
// Retirement decides it once, from the claim as it stood and why it is going.
// Each family's retire path stages what it says in the same status write that
// drops the claim and writes whatever record the claim leaves, and performs
// the release after that write.

// Retirement is a claim at the moment the write that retires it is built.
type Retirement struct {
	// Claim is the claim being retired, as it stood before the write.
	Claim RealmClaim
	// Disposition is why it is going. DispositionDiscard drops a claim whose
	// result, if it has one, is not used: one that never dispatched, or a
	// read-only one. DispositionUnaccounted retires a mutating run whose
	// effect nobody established. DispositionAccounted retires a run whose own
	// result was read and recorded.
	Disposition Disposition
	// RecordHoldsRealm says the family keeps the database realm with the
	// record a mutating run leaves until a reading settles it, so the run
	// hands nothing back. A schema's pending observation does. A migration's
	// run record does not: its realm goes back once nothing the run
	// dispatched can still write, and the reading that settles the record
	// takes no Lease.
	RecordHoldsRealm bool
	// MayStillWrite says something the claim dispatched may still be
	// executing, so its Lease is left to expire rather than handed back. It
	// matters only for a mutating run the family does not keep the realm for.
	MayStillWrite bool
	// EndsProof says the same write removes the pending observation the claim
	// was carrying out, which releases the realm the proof held.
	EndsProof bool
}

// Releases reports whose epoch the write that retires the claim owes back: the
// claim's own, the pending observation's, or none.
//
// A claim carrying out a proof never releases the proof's realm on its own
// account, only when its write ends the proof. A mutating run that may have
// changed the database leaves the realm with whatever outlives it: the record
// the family keeps it for, or an executor that may still be writing, whose
// Lease is sized to outlive it. Everything else that holds the realm in its
// own right hands it back.
func (r Retirement) Releases() RealmOwner {
	switch RealmHeldBy(r.Claim) {
	case OwnerProof:
		if r.EndsProof {
			return OwnerProof
		}
	case OwnerClaim:
		ran := r.Claim.Mutating && r.Disposition != DispositionDiscard
		if ran && (r.RecordHoldsRealm || r.MayStillWrite) {
			return OwnerNone
		}
		return OwnerClaim
	}
	return OwnerNone
}
