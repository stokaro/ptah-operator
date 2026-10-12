package mutationlifecycle

import (
	"errors"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

// Disposition is what a claim owes when it stops: after something goes wrong
// with it, or, for DispositionAccounted, when its own result was read.
type Disposition string

const (
	// DispositionAccounted retires the claim with the outcome its own result
	// established.
	DispositionAccounted Disposition = "accounted"
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
	// DispositionWithdraw takes back the mark of a claim whose one create the
	// API server refused before storing anything. No Job stands under the name
	// or ever did, so the claim is where a process that stopped between
	// spending its approval and marking leaves it, and the next pass makes the
	// create under the approval already spent.
	DispositionWithdraw Disposition = "withdraw"
)

// CreateAnswer is what a failed Create says about whether the API server
// stored the Job.
type CreateAnswer string

const (
	// AnswerUnknown: the Job may have been stored and the answer lost. A
	// timeout, a dropped connection, or a server error nothing narrows down.
	AnswerUnknown CreateAnswer = "unknown"
	// AnswerNameTaken: the API server refused the Create because something
	// already holds the name.
	AnswerNameTaken CreateAnswer = "name-taken"
	// AnswerRefused: the API server turned the request away before storing it.
	// Authentication, authorization, admission or validation refused it, or
	// priority and fairness shed it.
	AnswerRefused CreateAnswer = "refused"
)

// ClassifyCreate reads a failed Create's error for what it proves.
//
// Only an answer the API server gives before it writes to storage is a
// refusal. An admission webhook it could not call, with failurePolicy Fail, is
// one: the 0.2.1 and 0.2.2 restart bursts replaced every manager, which
// removed every endpoint of the manager's own Job-write webhook while the
// stopping leader was dispatching. Every other server error, a timeout among
// them, may come after the write and stays unknown.
func ClassifyCreate(err error) CreateAnswer {
	if apierrors.IsAlreadyExists(err) {
		return AnswerNameTaken
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return AnswerUnknown
	}
	switch s := status.Status(); s.Code {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusMethodNotAllowed, http.StatusNotAcceptable, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return AnswerRefused
	case http.StatusInternalServerError:
		if s.Reason == metav1.StatusReasonInternalError && webhookCallFailed(s.Details) {
			return AnswerRefused
		}
	}
	return AnswerUnknown
}

// webhookCallFailed recognizes the error the API server's admission
// dispatchers build when a webhook that fails closed cannot be called: an
// internal error whose one cause is the webhook call's own error.
func webhookCallFailed(details *metav1.StatusDetails) bool {
	return details != nil && len(details.Causes) == 1 &&
		strings.HasPrefix(details.Causes[0].Message, `failed calling webhook "`)
}

// DispatchFailure reports what a failure at the boundary leaves the claim
// owing.
//
// A name already taken is, for a read-only claim, another attempt's Job, and
// retrying under a new name is safe. For a mutating claim it is the most
// dangerous answer there is: the Job standing there may be one this claim
// created on a pass whose answer was lost, so retrying would dispatch a second
// executor beside an executor that may be running SQL. A refused create is the
// one failure that proves no Job exists, so a mutating claim withdraws its mark
// instead of retiring.
func DispatchFailure(stage DispatchStage, mutating bool, answer CreateAnswer) Disposition {
	if mutating {
		switch stage {
		case StageCreate:
			if answer == AnswerRefused {
				return DispositionWithdraw
			}
			return DispositionUnaccounted
		case StageIntent:
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
		if answer == AnswerNameTaken {
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
