package mutationlifecycle

// DispatchState is what a claim records about whether its Job may exist.
//
// Either field alone is enough. DispatchStarted is written before the one
// permitted create and is never cleared for a mutating claim, so it is the
// statement that a create may have committed even when no response came back.
// A Job UID is only ever written after a create returned, so a claim carrying
// one has certainly dispatched. No state holds a UID without DispatchStarted.
type DispatchState struct {
	DispatchStarted bool
	JobUID          string
}

// MayHaveDispatched reports whether a Job may exist for this claim.
//
// It decides whether a missing Job is created or is an outcome nobody can
// account for, which is the difference between running a migration twice and
// asking a person to go and look. Answering it from anything else -- a phase,
// a condition, whether a Job is there now -- reads correctly and is wrong on
// exactly the pass where the create committed and the response did not arrive.
func MayHaveDispatched(state DispatchState) bool {
	return state.DispatchStarted || state.JobUID != ""
}
