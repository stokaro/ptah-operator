package mutationlifecycle

// The dispatch boundary is three steps and both families take all three: create
// the one Job the claim reserved a name for, read it back, and check that what
// stands there is what the claim describes. Each can fail, and what a failure
// means depends on one thing beyond the step itself -- whether the claim may
// have changed the database.
//
// The two controllers wrote that decision separately and drifted: the schema
// family used to settle an Apply whose confirming read failed, where the
// migration family requeued. Nothing about the resource kinds made that
// difference; it was two copies of one rule. This is the one copy.

// DispatchStage is the step of the dispatch boundary that failed.
type DispatchStage string

const (
	// StageCreate is the single Create a claim is permitted.
	StageCreate DispatchStage = "create"
	// StageConfirm is the read that says what the Create left behind.
	StageConfirm DispatchStage = "confirm"
	// StageIntent is the check that the Job found is the one described.
	StageIntent DispatchStage = "intent"
)

// Disposition is what a claim owes after something goes wrong with it.
type Disposition string

const (
	// DispositionUnaccounted retires the claim with an outcome nobody
	// established. A mutating run may already have opened the database, and no
	// later pass can establish that it did not.
	DispositionUnaccounted Disposition = "unaccounted"
	// DispositionRetry dispatches again under a fresh attempt. Re-running a
	// read costs nothing, so a read-only claim takes this wherever it can.
	DispositionRetry Disposition = "retry"
	// DispositionRequeue keeps the claim and comes back. The next pass reads
	// the Job the boundary already recorded and decides from what it finds.
	DispositionRequeue Disposition = "requeue"
	// DispositionDiscard drops the claim. Nothing it did needs accounting for,
	// because a read-only run changed nothing and its result is about inputs
	// that have since moved.
	DispositionDiscard Disposition = "discard"
)

// DispatchFailure reports what a failure at the boundary leaves the claim
// owing.
//
// alreadyExists says the API server refused the Create because something holds
// the name. For a read-only claim that is another attempt's Job and retrying
// under a new name is safe. For a mutating claim it is the most dangerous
// answer there is: the Job standing there may be one this claim created on a
// pass whose answer was lost, so retrying would dispatch a second executor
// beside an executor that may be running SQL.
func DispatchFailure(stage DispatchStage, mutating, alreadyExists bool) Disposition {
	if mutating {
		switch stage {
		case StageCreate, StageIntent:
			return DispositionUnaccounted
		case StageConfirm:
			// dispatchStarted is durable before the Create, so the claim
			// already records that a Job may stand under its name. The next
			// pass adopts it if it is there and settles the run as unaccounted
			// for if it is not, which is strictly more than this pass can say.
			return DispositionRequeue
		}
		return DispositionUnaccounted
	}
	switch stage {
	case StageCreate:
		if alreadyExists {
			return DispositionRetry
		}
		return DispositionRequeue
	case StageIntent:
		return DispositionRetry
	}
	return DispositionRequeue
}

// HarvestFault names what a terminal Job's evidence turned out to be.
type HarvestFault string

const (
	// FaultInputsChanged means the claim's inputs moved while its Job ran, so
	// the result describes something the resource no longer asks for.
	FaultInputsChanged HarvestFault = "inputs-changed"
	// FaultPodMultiplicity means the Job ran more than one executor Pod, or
	// one whose intent could not be established. One result frame is one Pod's
	// account and cannot speak for the other.
	FaultPodMultiplicity HarvestFault = "pod-multiplicity"
	// FaultUnreadableResult means the run left no result the controller can
	// read, or the Job did not succeed.
	FaultUnreadableResult HarvestFault = "unreadable-result"
)

// HarvestFailure reports what a claim owes when its terminal Job's evidence
// cannot settle it.
//
// The asymmetry is the whole of it, and it is the same one the dispatch
// boundary has. A read-only run changed nothing, so a result that cannot be
// used costs a second read; the claim is retried, or dropped where its inputs
// have moved and the answer would be about the wrong question. A mutating run
// may already have changed the database, and no amount of re-reading
// establishes that it did not -- so every fault retires it with an outcome
// nobody established, including the one that looks most like staleness.
func HarvestFailure(fault HarvestFault, mutating bool) Disposition {
	if mutating {
		return DispositionUnaccounted
	}
	if fault == FaultInputsChanged {
		return DispositionDiscard
	}
	return DispositionRetry
}
