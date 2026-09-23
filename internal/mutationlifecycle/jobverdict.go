package mutationlifecycle

// What to do about the Job under a claim's reserved name is one decision, and
// both controllers made it with the same four branches in the same order:
// a missing Job, a Job whose UID is not the one recorded, a Job this resource
// does not own, and a Job with no UID recorded yet.
//
// The branches agree today. What nothing held is that they go on agreeing: the
// two families differ only in which verb they reach for, and the cost of the
// verbs is not symmetric. Retrying a read-only operation is free; retrying a
// mutating one runs SQL a second time. A branch that picked up the wrong verb
// in one family would read correctly beside its neighbours and be invisible in
// a diff.
//
// So the decision is here, as a value, and each controller maps the value to
// its own calls.

// JobClaim is what a pass knows when it looks for the Job a claim reserved.
type JobClaim struct {
	// Mutating says whether this claim may execute SQL. It is the whole of the
	// asymmetry below.
	Mutating bool
	// DispatchStarted and RecordedJobUID are the claim's own dispatch state.
	DispatchStarted bool
	RecordedJobUID  string
	// Found says whether a Job exists under the reserved name, and the rest
	// describes it.
	Found        bool
	FoundJobUID  string
	OwnedExactly bool
}

// JobCause is why the pass reached its verdict. It is separate from the
// verdict because the two controllers word their evidence for an operator, and
// collapsing three situations into one message would cost a reader the one
// fact that tells them where to look.
type JobCause string

const (
	// CauseNone: the Job is the claim's own.
	CauseNone JobCause = ""
	// CauseMissing: nothing exists under the reserved name.
	CauseMissing JobCause = "missing"
	// CauseReplaced: something exists under the name and is not the Job the
	// claim recorded.
	CauseReplaced JobCause = "replaced"
	// CauseDisowned: the Job under the name is not owned by exactly this
	// resource.
	CauseDisowned JobCause = "disowned"
)

// JobVerdict is what the pass does next.
type JobVerdict string

const (
	// VerdictDispatch: nothing was created and nothing may have been, so the
	// claim proceeds toward its one permitted create.
	VerdictDispatch JobVerdict = "dispatch"
	// VerdictUnaccounted: a mutating claim whose Job is missing, replaced or
	// disowned. What it did is a question for the database rather than for a
	// retry, and the operator never creates a second one.
	VerdictUnaccounted JobVerdict = "unaccounted"
	// VerdictRetry: the same situation for a read-only claim, where another
	// attempt costs a Job and proves the same thing.
	VerdictRetry JobVerdict = "retry"
	// VerdictAdopt: the Job under the reserved name is this claim's and the
	// claim has not recorded its UID, which is what a create whose response
	// never arrived leaves behind.
	VerdictAdopt JobVerdict = "adopt"
	// VerdictSupervise: the Job is this claim's and the claim names it.
	VerdictSupervise JobVerdict = "supervise"
)

// VerdictFor decides what a pass does about the Job it looked for.
//
// The order is the order both controllers already used, and it matters: a Job
// whose UID is not the recorded one is judged before ownership, because a
// replaced Job may legitimately be owned by this resource and is still not the
// one the claim dispatched.
func VerdictFor(claim JobClaim) (JobVerdict, JobCause) {
	if !claim.Found {
		if claim.Mutating && MayHaveDispatched(DispatchState{
			DispatchStarted: claim.DispatchStarted,
			JobUID:          claim.RecordedJobUID,
		}) {
			return VerdictUnaccounted, CauseMissing
		}
		return VerdictDispatch, CauseMissing
	}
	if claim.RecordedJobUID != "" && claim.RecordedJobUID != claim.FoundJobUID {
		return lostJob(claim), CauseReplaced
	}
	if !claim.OwnedExactly {
		return lostJob(claim), CauseDisowned
	}
	if claim.RecordedJobUID == "" {
		return VerdictAdopt, CauseNone
	}
	return VerdictSupervise, CauseNone
}

// lostJob is the asymmetry itself: the same evidence, and two costs.
func lostJob(claim JobClaim) JobVerdict {
	if claim.Mutating {
		return VerdictUnaccounted
	}
	return VerdictRetry
}
