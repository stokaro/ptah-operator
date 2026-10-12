package mutationlifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

func allAnswers() []mutationlifecycle.CreateAnswer {
	return []mutationlifecycle.CreateAnswer{
		mutationlifecycle.AnswerUnknown,
		mutationlifecycle.AnswerNameTaken,
		mutationlifecycle.AnswerRefused,
	}
}

func allStages() []mutationlifecycle.DispatchStage {
	return []mutationlifecycle.DispatchStage{
		mutationlifecycle.StageCreate,
		mutationlifecycle.StageConfirm,
		mutationlifecycle.StageIntent,
	}
}

// The property the dispatch boundary exists for, stated over every input
// rather than as a list of cases: a claim that may have changed the database
// is never told to dispatch again.
//
// A retry renames the claim and creates a second Job. Where the first one may
// be running SQL, that is two executors against one database -- which no
// evidence gathered afterwards can undo.
func TestAMutatingClaimIsNeverToldToDispatchAgain(t *testing.T) {
	t.Parallel()

	for _, stage := range allStages() {
		for _, answer := range allAnswers() {
			disposition := mutationlifecycle.DispatchFailure(stage, true, answer)
			if disposition == mutationlifecycle.DispositionRetry {
				t.Fatalf("a mutating claim failing at %q (answer %q) was told to retry", stage, answer)
			}
			// A withdrawal makes the one create again, so only the answer
			// proving that no Job was stored may lead to it.
			if disposition == mutationlifecycle.DispositionWithdraw &&
				(stage != mutationlifecycle.StageCreate || answer != mutationlifecycle.AnswerRefused) {
				t.Fatalf("a mutating claim failing at %q (answer %q) withdrew its mark", stage, answer)
			}
		}
	}
}

// The exact table, so a change to any answer is a change to this file.
func TestWhatEachDispatchFailureLeavesTheClaimOwing(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		stage    mutationlifecycle.DispatchStage
		mutating bool
		answer   mutationlifecycle.CreateAnswer
		want     mutationlifecycle.Disposition
	}{
		{
			// The write may have landed and its answer did not come back.
			name:  "a mutating create that did not answer",
			stage: mutationlifecycle.StageCreate, mutating: true,
			want: mutationlifecycle.DispositionUnaccounted,
		},
		{
			// The most dangerous answer: something holds the name, and it may
			// be this claim's own Job from a pass whose answer was lost.
			name:  "a mutating create refused because the name is taken",
			stage: mutationlifecycle.StageCreate, mutating: true, answer: mutationlifecycle.AnswerNameTaken,
			want: mutationlifecycle.DispositionUnaccounted,
		},
		{
			// Nothing was stored, so nothing can run, and the claim keeps the
			// approval it already spent.
			name:  "a mutating create the API server refused before storing it",
			stage: mutationlifecycle.StageCreate, mutating: true, answer: mutationlifecycle.AnswerRefused,
			want: mutationlifecycle.DispositionWithdraw,
		},
		{
			// dispatchStarted is already durable, so the next pass can still
			// find out. Settling here would give up on a run that is probably
			// fine.
			name:  "a mutating confirmation read that failed",
			stage: mutationlifecycle.StageConfirm, mutating: true,
			want: mutationlifecycle.DispositionRequeue,
		},
		{
			// Something stands under the reserved name that this claim did not
			// describe, and it may already be opening the database.
			name:  "a mutating Job that is not the one described",
			stage: mutationlifecycle.StageIntent, mutating: true,
			want: mutationlifecycle.DispositionUnaccounted,
		},
		{
			name:  "a read-only create that did not answer",
			stage: mutationlifecycle.StageCreate,
			want:  mutationlifecycle.DispositionRequeue,
		},
		{
			// Another attempt's Job. Re-running a read costs nothing.
			name:  "a read-only create refused because the name is taken",
			stage: mutationlifecycle.StageCreate, answer: mutationlifecycle.AnswerNameTaken,
			want: mutationlifecycle.DispositionRetry,
		},
		{
			name:  "a read-only create the API server refused before storing it",
			stage: mutationlifecycle.StageCreate, answer: mutationlifecycle.AnswerRefused,
			want: mutationlifecycle.DispositionRequeue,
		},
		{
			name:  "a read-only confirmation read that failed",
			stage: mutationlifecycle.StageConfirm,
			want:  mutationlifecycle.DispositionRequeue,
		},
		{
			name:  "a read-only Job that is not the one described",
			stage: mutationlifecycle.StageIntent,
			want:  mutationlifecycle.DispositionRetry,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			answer := row.answer
			if answer == "" {
				answer = mutationlifecycle.AnswerUnknown
			}
			got := mutationlifecycle.DispatchFailure(row.stage, row.mutating, answer)
			if got != row.want {
				t.Fatalf("DispatchFailure(%q, mutating=%t, %q) = %q, want %q",
					row.stage, row.mutating, answer, got, row.want)
			}
		})
	}
}

// An unknown stage cannot be read as permission to dispatch again. A stage
// added to the boundary without a case here is a stage nothing has decided,
// and the safe reading of that is the conservative one.
func TestAnUnknownDispatchStageIsNeverARetry(t *testing.T) {
	t.Parallel()

	for _, mutating := range []bool{false, true} {
		for _, answer := range allAnswers() {
			got := mutationlifecycle.DispatchFailure("a stage added later", mutating, answer)
			if got == mutationlifecycle.DispositionRetry || got == mutationlifecycle.DispositionWithdraw {
				t.Fatalf("an unrecognised stage (mutating=%t, answer %q) = %q", mutating, answer, got)
			}
		}
	}
}

// webhookUnreachable is the error the API server returns when an admission
// webhook that fails closed cannot be called, as the 0.2.2 restart burst
// produced it for the manager's own Job-write webhook.
func webhookUnreachable() error {
	return apierrors.NewInternalError(fmt.Errorf(`failed calling webhook "vcontrollerwrite.operator.ptah.run": failed to call webhook: ` +
		`Post "https://ptah-operator-webhook.ptah-system.svc:443/validate-operator-controller-write?timeout=10s": no endpoints available`))
}

// What each failed create proves. A refusal has to be an answer the API
// server gives before it writes; everything that can follow the write, or
// says nothing at all, stays unknown.
func TestWhatAFailedCreateProves(t *testing.T) {
	t.Parallel()

	jobs := schema.GroupResource{Group: "batch", Resource: "jobs"}
	for _, row := range []struct {
		name string
		err  error
		want mutationlifecycle.CreateAnswer
	}{
		{"an unreachable webhook that fails closed", webhookUnreachable(), mutationlifecycle.AnswerRefused},
		{"the same, wrapped by the caller", fmt.Errorf("create Apply Job: %w", webhookUnreachable()), mutationlifecycle.AnswerRefused},
		{"a webhook denial", apierrors.NewForbidden(jobs, "apply", errors.New("admission webhook denied the request")), mutationlifecycle.AnswerRefused},
		{"an invalid Job", apierrors.NewInvalid(schema.GroupKind{Group: "batch", Kind: "Job"}, "apply", nil), mutationlifecycle.AnswerRefused},
		{"a missing namespace", apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "team-a"), mutationlifecycle.AnswerRefused},
		{"shed by priority and fairness", apierrors.NewTooManyRequests("too many requests", 1), mutationlifecycle.AnswerRefused},
		{"a name already taken", apierrors.NewAlreadyExists(jobs, "apply"), mutationlifecycle.AnswerNameTaken},
		{"a server timeout", apierrors.NewServerTimeout(jobs, "create", 1), mutationlifecycle.AnswerUnknown},
		{"a gateway timeout", apierrors.NewTimeoutError("request did not complete", 1), mutationlifecycle.AnswerUnknown},
		{"an internal error from storage", apierrors.NewInternalError(errors.New("etcdserver: request timed out")), mutationlifecycle.AnswerUnknown},
		{"a webhook named only in the message", apierrors.NewInternalError(errors.New(`storage after failed calling webhook "x"`)), mutationlifecycle.AnswerUnknown},
		{"service unavailable", apierrors.NewServiceUnavailable("unavailable"), mutationlifecycle.AnswerUnknown},
		{"a conflict that is not the name", apierrors.NewConflict(jobs, "apply", errors.New("conflict")), mutationlifecycle.AnswerUnknown},
		{"a dropped connection", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, mutationlifecycle.AnswerUnknown},
		{"a deadline", context.DeadlineExceeded, mutationlifecycle.AnswerUnknown},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			if got := mutationlifecycle.ClassifyCreate(row.err); got != row.want {
				t.Fatalf("ClassifyCreate(%v) = %q, want %q", row.err, got, row.want)
			}
		})
	}
}

// The same property as the dispatch boundary, at the other end of the run: a
// claim that may have changed the database is never dropped and never retried,
// whatever its Job's evidence turned out to be. Re-reading establishes nothing
// about SQL that may already have run, and dropping the claim throws away the
// only record that it might have.
func TestAMutatingClaimIsNeverDroppedOnItsEvidence(t *testing.T) {
	t.Parallel()

	for _, fault := range []mutationlifecycle.HarvestFault{
		mutationlifecycle.FaultInputsChanged,
		mutationlifecycle.FaultPodMultiplicity,
		mutationlifecycle.FaultUnreadableResult,
		"a fault named later",
	} {
		if got := mutationlifecycle.HarvestFailure(fault, true); got != mutationlifecycle.DispositionUnaccounted {
			t.Fatalf("a mutating claim whose evidence was %q = %q, want it unaccounted for", fault, got)
		}
	}
}

// The read-only side, where the answer does depend on the fault: inputs that
// moved make the question wrong rather than the answer unreadable, so there is
// nothing to retry.
func TestWhatEachHarvestFaultLeavesAReadOnlyClaimOwing(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		fault mutationlifecycle.HarvestFault
		want  mutationlifecycle.Disposition
	}{
		{mutationlifecycle.FaultInputsChanged, mutationlifecycle.DispositionDiscard},
		{mutationlifecycle.FaultPodMultiplicity, mutationlifecycle.DispositionRetry},
		{mutationlifecycle.FaultUnreadableResult, mutationlifecycle.DispositionRetry},
	} {
		if got := mutationlifecycle.HarvestFailure(row.fault, false); got != row.want {
			t.Fatalf("HarvestFailure(%q, read-only) = %q, want %q", row.fault, got, row.want)
		}
	}
}
