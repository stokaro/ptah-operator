package mutationlifecycle_test

import (
	"testing"

	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

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
		for _, alreadyExists := range []bool{false, true} {
			disposition := mutationlifecycle.DispatchFailure(stage, true, alreadyExists)
			if disposition == mutationlifecycle.DispositionRetry {
				t.Fatalf("a mutating claim failing at %q (alreadyExists=%t) was told to retry",
					stage, alreadyExists)
			}
		}
	}
}

// The exact table, so a change to any answer is a change to this file.
func TestWhatEachDispatchFailureLeavesTheClaimOwing(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name          string
		stage         mutationlifecycle.DispatchStage
		mutating      bool
		alreadyExists bool
		want          mutationlifecycle.Disposition
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
			stage: mutationlifecycle.StageCreate, mutating: true, alreadyExists: true,
			want: mutationlifecycle.DispositionUnaccounted,
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
			stage: mutationlifecycle.StageCreate, alreadyExists: true,
			want: mutationlifecycle.DispositionRetry,
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

			got := mutationlifecycle.DispatchFailure(row.stage, row.mutating, row.alreadyExists)
			if got != row.want {
				t.Fatalf("DispatchFailure(%q, mutating=%t, alreadyExists=%t) = %q, want %q",
					row.stage, row.mutating, row.alreadyExists, got, row.want)
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
		got := mutationlifecycle.DispatchFailure("a stage added later", mutating, true)
		if got == mutationlifecycle.DispositionRetry {
			t.Fatalf("an unrecognised stage (mutating=%t) was told to retry", mutating)
		}
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
