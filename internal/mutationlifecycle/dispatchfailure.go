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

// DispatchDisposition is what the claim owes after a failure at the boundary.
type DispatchDisposition string

const (
	// DispositionUnaccounted retires the claim with an outcome nobody
	// established. A mutating run may already have opened the database, and no
	// later pass can establish that it did not.
	DispositionUnaccounted DispatchDisposition = "unaccounted"
	// DispositionRetry dispatches again under a fresh attempt. Re-running a
	// read costs nothing, so a read-only claim takes this wherever it can.
	DispositionRetry DispatchDisposition = "retry"
	// DispositionRequeue keeps the claim and comes back. The next pass reads
	// the Job the boundary already recorded and decides from what it finds.
	DispositionRequeue DispatchDisposition = "requeue"
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
func DispatchFailure(stage DispatchStage, mutating, alreadyExists bool) DispatchDisposition {
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
