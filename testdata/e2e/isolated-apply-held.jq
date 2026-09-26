# An Apply whose node the API server cannot reach, still held by its resource.
#
# The claim is what a replacement would have to get past: while it names this
# run's Job and the Lease epoch it took, nothing else is dispatched against the
# database, and the realm is not handed back. Each clause is one way a
# controller could let go of a run that may still be writing:
#
# - a retired or replaced claim, which frees the resource to claim again;
# - a new Lease epoch or a continuity loss, which means the realm changed hands;
# - an owed release, which hands the realm back on the next pass;
# - a run recorded, unresolved or finished, for this Job, which is the claim
#   being closed on evidence nobody could read yet;
# - the finalizer gone, which lets a deletion collect the Job.
#
# The phase is not read: the claim is what the controller acts on.
.status as $status
| ($status.activeOperation // {}) as $claim
| $claim.type == "Apply"
and $claim.jobUID == $job
and $claim.leaseEpoch == $epoch
and (($claim.leaseContinuityLost // false) | not)
and ($status.pendingLockRelease // null) == null
and ($status.unresolvedRun // null) == null
and ((($status.lastRun // {}).jobUID // "") != $job)
and any(.metadata.finalizers[]?; . == "operator.ptah.run/migration-operation")
