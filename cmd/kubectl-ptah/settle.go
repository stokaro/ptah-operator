package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// settleCommand gives up the post-Apply proof of a schema whose URL Secret
// moved to another target, by writing the value its stop prints, and waits for
// the operator to take it if asked to.
func settleCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := newCommandFlags("settle", stderr)
	timeout := flags.Duration("timeout", apiTimeout, "how long to wait for the API server")
	waiting := addWaitFlags(flags, "the operator has taken the statement and given the proof up")
	expect := flags.String("expect", "", "the value the stop printed when you read it: settle writes that value or nothing")
	ref, code, done := flags.parse(arguments, stderr)
	if done {
		return code
	}
	if *expect != "" {
		if _, err := operate.ParseSettleValue(*expect); err != nil {
			fmt.Fprintf(stderr, "kubectl-ptah: --expect: %v. Give the value after %s= in the command the stop prints.\n",
				err, operatorv1alpha1.SettledApplyAnnotation)
			return exitUsage
		}
	}
	if ref.Family != operate.FamilySchema {
		fmt.Fprintf(stderr, "kubectl-ptah: settle takes a PtahSchema, as schema/NAME. A PtahMigration owes no proof that "+
			"a statement settles; a run nobody accounted for is settled by a PtahMigrationRunAcknowledgment.\n")
		return exitUsage
	}
	if !waiting.valid(stderr) {
		return exitUsage
	}

	cluster, ref, ok := flags.connect(ref, *timeout, stderr)
	if !ok {
		return exitFailed
	}
	changeCtx, cancel := context.WithTimeout(ctx, *timeout)
	result, err := operate.Settle(changeCtx, cluster, ref, operate.SettleOptions{Expect: *expect})
	cancel()
	if err != nil {
		return settleFailed(stderr, ref, err)
	}
	if *flags.output == "text" {
		writeSettleReport(stdout, result, *waiting.wait)
	} else if !*waiting.wait {
		return printResource(stdout, stderr, ref, result.Object, *flags.output)
	}
	if !*waiting.wait {
		return exitOK
	}
	// The answer to the change is the baseline: it still owes the proof, so the
	// reading that ends it comes after.
	return waitAndReport(ctx, cluster, ref, operate.WaitOptions{
		Target: operate.TargetSettled, Timeout: *waiting.timeout, Since: result.Object, Settlement: result.Printed.Value,
	}, *flags.output, stdout, stderr)
}

// settleFailed tells the person what became of a settlement that returned err.
func settleFailed(stderr io.Writer, ref operate.Ref, err error) int {
	var (
		notStopped *operate.NotStoppedError
		noValue    *operate.NoSettleValueError
		stale      *operate.StaleSettleValueError
		unexpected *operate.UnexpectedSettleValueError
	)
	switch {
	case errors.As(err, &notStopped):
		fmt.Fprintf(stderr, "kubectl-ptah: %s. Nothing was changed.\nsettle gives up a proof only where the schema "+
			"stopped on it, with ReconciliationFailed=True and reason %s: the URL Secret names another target than the "+
			"plan its Apply ran from. %s shows where it stands.\n",
			statusview.Clean(notStopped.Error()), operatorv1alpha1.ReasonTargetIdentityChanged, viewCommand(ref))
		return exitNotDone
	case errors.As(err, &noValue):
		fmt.Fprintf(stderr, "kubectl-ptah: %s. Nothing was changed.\nThe operator takes no value but the one its stop "+
			"prints, so settle writes no other. %s shows the message.\n", statusview.Clean(noValue.Error()), viewCommand(ref))
		return exitNotDone
	case errors.As(err, &unexpected):
		fmt.Fprintf(stderr, "kubectl-ptah: %s. Nothing was changed.\nThe stop names another target than the one you read "+
			"about: a read named another target since. %s shows the stop as it stands; check what the URL Secret holds "+
			"before you settle that one.\n", statusview.Clean(unexpected.Error()), viewCommand(ref))
		return exitNotDone
	case errors.As(err, &stale):
		fmt.Fprintf(stderr, "kubectl-ptah: %s. Nothing was changed.\nRun settle again to read the stop as it stands "+
			"now; %s shows it.\n", statusview.Clean(stale.Error()), viewCommand(ref))
		return exitNotDone
	}
	return changeFailed(stderr, ref, "settle", err)
}

func writeSettleReport(out io.Writer, result operate.SettleResult, waiting bool) {
	ref, printed := result.Ref, result.Printed
	var text strings.Builder
	field := statusview.FieldWriter(&text, "  ", len("Replaced:"))
	statement := operatorv1alpha1.SettledApplyAnnotation + "=" + statusview.Clean(printed.Value)
	if result.Changed {
		fmt.Fprintf(&text, "Settlement requested for %s.\n", ref)
		field("Wrote", statement)
		if result.Held {
			field("Replaced", "another value, which settled nothing")
		}
	} else {
		fmt.Fprintf(&text, "%s of %s already holds the value its stop prints; nothing was sent.\n",
			operatorv1alpha1.SettledApplyAnnotation, ref)
		field("Holds", statement)
	}
	field("Apply", fmt.Sprintf("%s, plan %s", statusview.Clean(printed.ApplyOperationID), orNone(statusview.Clean(result.Plan))))
	field("Journal", applyEndLine(result.Journal))
	field("Target", fmt.Sprintf("the read named %s; the plan was made for %s",
		statusview.Clean(printed.TargetDigest), orNone(statusview.Clean(result.PlanTarget))))
	field("Observed", "phase "+orNone(statusview.Clean(result.Phase)))

	text.WriteString(applyAccount(result) + "\n")
	fmt.Fprintf(&text, "Taking the statement gives up the proof, the plan %s and the database lock in one status write",
		orNone(statusview.Clean(result.Plan)))
	if result.Approval != "" {
		fmt.Fprintf(&text, "; the approval %s it consumed goes Stale", statusview.Clean(result.Approval))
	}
	text.WriteString(". The journal records the proof Verified, Unknown, status.applied records no Apply, and the operator " +
		"reads the database again under the URL the Secret holds and plans from what it finds.\n")
	text.WriteString(applyPolicyLine(ref, result.Apply) + "\n")
	switch {
	case result.Suspended:
		fmt.Fprintf(&text, "The resource is suspended and starts no read, so the operator takes the statement at the "+
			"first read after kubectl ptah resume %s/%s -n %s, or from a read already under way.\n",
			ref.Family, ref.Name, ref.Namespace)
	default:
		fmt.Fprintf(&text, "The operator takes it at the next read of that target, up to "+
			"spec.execution.failureRetryInterval (%s) after the last one.\n", statusview.Duration(result.RetryInterval))
		if reason := result.ReadyReason; reason != "" && reason != string(operatorv1alpha1.ReasonTargetIdentityChanged) {
			fmt.Fprintf(&text, "Ready reports %s: a read takes the statement only once its Pod starts and it names "+
				"that target.\n", statusview.Clean(reason))
		}
	}
	fmt.Fprintf(&text, "Removing the annotation before then takes the statement back: kubectl annotate ptahschema %s -n %s %s-\n",
		ref.Name, ref.Namespace, operatorv1alpha1.SettledApplyAnnotation)
	if !waiting {
		text.WriteString("settle --wait waits in the same command until the operator has taken the statement.\n")
	}
	_, _ = io.WriteString(out, text.String())
}

// applyEndLine is the journal's record of how the Apply ended.
func applyEndLine(entry *operatorv1alpha1.JournalEntry) string {
	if entry == nil {
		return "no ApplyFinished entry for the plan any more"
	}
	return fmt.Sprintf("ApplyFinished %s at %s", orNone(statusview.Clean(string(entry.Outcome))), statusview.Time(entry.At))
}

// applyAccount says what the statement accounts for: what the Apply did to the
// target its plan was made for, as the journal records it, or the proof where
// the journal no longer holds the entry.
func applyAccount(result operate.SettleResult) string {
	outcome, source := operatorv1alpha1.JournalUnknown, "status.pendingObservation"
	switch {
	case result.Journal != nil:
		outcome, source = result.Journal.Outcome, "the journal"
	case result.Outcome == operatorv1alpha1.PendingObservationApplySucceeded:
		outcome = operatorv1alpha1.JournalSucceeded
	}
	const account = "The statement says you account for what that Apply did to the target its plan was made for. "
	if outcome == operatorv1alpha1.JournalSucceeded {
		return account + fmt.Sprintf("It Succeeded, as %s records: it reported its statements done there, and no read "+
			"of another target can say what they changed.", source)
	}
	return account + fmt.Sprintf("Its outcome is %s, as %s records: nobody established what it did there, so check "+
		"that database first if it matters.", orNone(statusview.Clean(string(outcome))), source)
}
