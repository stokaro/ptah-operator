package controller

import (
	"context"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// A result frame lives only in the container log on the node, and a log can
// be gone before this manager reads it. The runner also writes a summary of the
// frame into its termination message, which the kubelet copies into Pod status
// (runner.Summary says what it carries). This file is the one place a
// controller reads it, and the rules are narrow on purpose:
//
//   - It is read only after the log was read and held no frame, past the window
//     in which a frame may still be arriving. A frame that is there and was
//     refused is an answer, and the summary is never read in place of it.
//   - It has to name this operation and attempt, and agree with any frame
//     header the log does hold.
//   - What it says is decided by the same code that decides a frame, so it
//     cannot be taken where the frame would have been refused.
//
// Only a migration Apply is decided from it. What a summary can add is the
// outcome of a migration run: the database's own account, which the history
// read that follows then confirms. A summary also says whether a mutation
// started, but neither family narrows an unknown outcome on that, not even from
// a frame, because a Job may run more than one Pod and one Pod's account of
// itself says nothing about another; so a summary does not either. A schema
// Apply's frame is taken only whole, and a read-only operation needs a payload
// no summary carries, so both keep reading the log and nothing else.

// terminationSummaryStandIn returns the runner's summary when it may stand in
// for the frame the log did not hold. A container that wrote none is not an
// error; a summary that is there and may not stand in is, so the caller can say
// why it was set aside.
func terminationSummaryStandIn(
	evidence terminalEvidence,
	parseErr error,
	operation runner.Operation,
	operationID string,
) (runner.Summary, bool, error) {
	if !evidence.Trusted || evidence.TerminationMessage == "" {
		return runner.Summary{}, false, nil
	}
	summary, err := runner.ParseSummaryFor(evidence.TerminationMessage, operation, operationID)
	if errors.Is(err, runner.ErrSummaryNotFound) {
		return runner.Summary{}, false, nil
	}
	if err != nil {
		return runner.Summary{}, false, err
	}
	if err := summary.StandsInFor(evidence.Logs, parseErr); err != nil {
		return runner.Summary{}, false, err
	}
	return summary, true, nil
}

// settleUnreadMigrationApply is where a migration Apply goes when its log held
// no frame. Without a summary that may stand in, it is an Apply nobody
// accounted for, as it always was.
func (r *MigrationReconciler) settleUnreadMigrationApply(
	ctx context.Context,
	migration *operatorv1alpha1.PtahMigration,
	job *batchv1.Job,
	evidence terminalEvidence,
	parseErr error,
) (ctrl.Result, error) {
	operation := migration.Status.ActiveOperation
	summary, standsIn, refusal := terminationSummaryStandIn(
		evidence, parseErr, runner.OperationMigrationApply, operation.ID,
	)
	if refusal != nil {
		r.event(migration, corev1.EventTypeWarning, "TerminationSummaryRefused",
			"the Apply Pod's termination message was not read in place of its result: %s",
			bounded(refusal.Error(), 256))
	}
	if !standsIn {
		return r.finishUncertainMigrationApply(ctx, migration, job,
			fmt.Errorf("read the Apply result: %w", parseErr), "")
	}
	return r.settleMigrationApply(ctx, migration, job, summaryMigrationApply(summary))
}

// summaryMigrationApply is what a termination summary says about a migration
// Apply, in the same terms frameMigrationApply gives a frame.
//
// It is less than the frame said and never more. The outcome is the one the
// frame carried; the applied versions are kept only where the count and the
// two ends name all of them, and otherwise the message states the range; the
// plan's own list, which a frame uses to name a refused selection, is not
// carried, so that run keeps the general failure message. What settles the run
// is the history read that follows it, as it is for a frame.
func summaryMigrationApply(summary runner.Summary) reportedMigrationApply {
	reported := reportedMigrationApply{
		uncertain:            summary.Uncertain,
		coordinationDigest:   summary.CoordinationDigest,
		targetIdentityDigest: summary.TargetIdentityDigest,
	}
	source := "The Apply's frame could not be read from its log; its termination message, bound to frame " +
		summary.FrameDigest + ", reports "
	migration := summary.Migration
	if migration == nil {
		what := "no account of what the database now holds"
		if summary.ErrorCode != "" {
			what = summary.ErrorCode + " and " + what
		}
		reported.failure = errors.New(source + what)
		return reported
	}
	run := migrationRunAccount{outcome: migrationRunOutcome(migration.Outcome)}
	var applied string
	switch migration.AppliedCount {
	case 0:
		applied = "no migration recorded applied"
	case 1:
		run.applied = []int64{migration.FirstApplied}
		applied = fmt.Sprintf("version %d recorded applied", migration.FirstApplied)
	case 2:
		run.applied = []int64{migration.FirstApplied, migration.LastApplied}
		applied = fmt.Sprintf("versions %d and %d recorded applied", migration.FirstApplied, migration.LastApplied)
	default:
		applied = fmt.Sprintf("%d migrations recorded applied, from version %d to version %d",
			migration.AppliedCount, migration.FirstApplied, migration.LastApplied)
	}
	run.message = source + fmt.Sprintf("outcome %s with %s", migration.Outcome, applied)
	reported.run = &run
	return reported
}
