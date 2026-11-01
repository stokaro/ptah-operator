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

// pinnedForm is the shape set-source takes, as a person writes it.
const pinnedForm = "oci://HOST/REPOSITORY@sha256:<64 hex>"

// setSourceCommand points a schema or a migration at another OCI artifact,
// reports what it asked for beside what the operator had observed, and waits
// for the operator if asked to.
func setSourceCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := newCommandFlags("set-source", stderr)
	timeout := flags.Duration("timeout", apiTimeout, "how long to wait for the API server")
	oci := flags.String("oci", "", "the OCI reference to set, pinned by digest: "+pinnedForm)
	waiting := addWaitFlags(flags, "the operator has planned from the new reference and the resource reaches --for")
	waitFor := flags.String("for", string(operate.TargetInSync), "what --wait waits for: in-sync or approval")
	ref, code, done := flags.parse(arguments, stderr)
	if done {
		return code
	}
	// Everything about the reference is settled before the cluster is
	// reached: a refused one sends nothing and reads nothing.
	if *oci == "" {
		fmt.Fprintf(stderr, "kubectl-ptah: set-source needs --oci with an OCI reference pinned by digest, %s.\n%s\n",
			pinnedForm, wherePinnedReferencesComeFrom(ref.Family, ""))
		return exitUsage
	}
	pinned, err := operate.ParsePinnedReference(*oci)
	if err != nil {
		var notPinned *operate.NotPinnedError
		tag := ""
		if errors.As(err, &notPinned) && notPinned.Tag != "" {
			tag = notPinned.Value
		}
		fmt.Fprintf(stderr, "kubectl-ptah: set-source takes an OCI reference pinned by digest, %s, and %s.\n%s Nothing was sent.\n",
			pinnedForm, statusview.Clean(err.Error()), wherePinnedReferencesComeFrom(ref.Family, statusview.Clean(tag)))
		return exitUsage
	}
	target := operate.TargetInSync
	if flags.Changed("for") {
		// Naming what to wait for is asking to wait.
		*waiting.wait = true
		parsed, err := operate.ParseTarget(*waitFor)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
			return exitUsage
		case parsed != operate.TargetInSync && parsed != operate.TargetApproval:
			fmt.Fprintf(stderr, "kubectl-ptah: set-source waits for in-sync or approval, not %s\n", parsed)
			return exitUsage
		}
		target = parsed
	}
	if !waiting.valid(stderr) {
		return exitUsage
	}

	cluster, ref, ok := flags.connect(ref, *timeout, stderr)
	if !ok {
		return exitFailed
	}
	changeCtx, cancel := context.WithTimeout(ctx, *timeout)
	result, err := operate.SetSource(changeCtx, cluster, ref, pinned)
	cancel()
	if err != nil {
		return setSourceFailed(stderr, ref, pinned, err)
	}
	if *flags.output == "text" {
		writeSourceReport(stdout, result, *waiting.wait)
	} else if !*waiting.wait {
		return printResource(stdout, stderr, ref, result.Object, *flags.output)
	}
	if !*waiting.wait {
		return exitOK
	}
	// The answer to the change is the baseline, and the generation it carries
	// is the one the operator has to observe. Source holds the wait to a plan
	// or a resolution from this reference, so neither the previous artifact's
	// state nor another writer's later reference counts.
	return waitAndReport(ctx, cluster, ref, operate.WaitOptions{
		Target: target, Timeout: *waiting.timeout, Since: result.Object, Generation: result.Generation,
		Source: pinned.Reference,
	}, *flags.output, stdout, stderr)
}

// wherePinnedReferencesComeFrom names the commands that print a pinned
// reference. tag is a reference that names a tag, or empty for none.
func wherePinnedReferencesComeFrom(family operate.Family, tag string) string {
	push := "ptah schema push"
	if family == operate.FamilyMigration {
		push = "ptah migrations push"
	}
	if tag == "" {
		tag = "<reference>"
	}
	return fmt.Sprintf("%s --format reference prints the pinned reference of the artifact a push wrote, and "+
		"ptah oci resolve %s the one a tag names now.", push, tag)
}

// setSourceFailed tells the person what became of a change of source that
// returned err.
func setSourceFailed(stderr io.Writer, ref operate.Ref, pinned operate.PinnedReference, err error) int {
	var (
		kind    *operate.SourceKindError
		running *operate.ApplyRunningError
		changed *operate.SourceChangedError
	)
	switch {
	case errors.As(err, &kind):
		fmt.Fprintf(stderr, "kubectl-ptah: %s. Nothing was changed.\nset-source changes the reference of an OCI "+
			"source and nothing else. Moving to an OCI artifact replaces the %s source and its settings, and names "+
			"the verification policy the artifact is held to in %s.verificationPolicyFrom: that is an edit of the "+
			"resource.\n  kubectl edit %s %s -n %s\n", statusview.Clean(kind.Error()), kind.Kind, kind.Field,
			strings.ToLower(ref.Kind()), ref.Name, ref.Namespace)
		return exitNotDone
	case errors.As(err, &running):
		consequence := "the operator records its outcome as unknown without reading its result, and reads the " +
			"database to prove what it left before anything else runs"
		if ref.Family == operate.FamilyMigration {
			consequence = "the operator records the run as one nobody accounted for (Unknown) without reading its " +
				"result, and a reading clears that record only once nothing of the artifact is left to apply: with " +
				"the new versions pending, it takes a person's acknowledgment"
		}
		fmt.Fprintf(stderr, "kubectl-ptah: %s. Nothing was changed.\nA spec change while an Apply runs moves the "+
			"generation the Apply was decided from, so %s. The Apply cannot take the new reference anyway. Wait "+
			"for it to end, then run set-source again; %s waits for it.\n",
			statusview.Clean(running.Error()), consequence, waitLine(ref, "applied"))
		return exitNotDone
	case errors.As(err, &changed):
		again := "Run set-source again to replace it."
		if changed.Now.Kind == operate.SourceOCI && changed.Now.Reference == pinned.Reference {
			again = "It holds the reference you asked for: another writer set it first."
		} else if changed.Now.Kind != operate.SourceOCI {
			again = "set-source changes no ConfigMap or Git source."
		}
		fmt.Fprintf(stderr, "kubectl-ptah: %s. Nothing was changed: the reference it would have replaced is "+
			"not the one read. %s\n", statusview.Clean(changed.Error()), again)
		return exitNotDone
	}
	return changeFailed(stderr, ref, "set-source", err)
}

func writeSourceReport(out io.Writer, result operate.SourceResult, waiting bool) {
	ref, source := result.Ref, result.Source
	field := source.Field + ".ociRef"
	var text strings.Builder
	line := statusview.FieldWriter(&text, "  ", len("Requested:"))
	if result.Changed {
		fmt.Fprintf(&text, "Source set for %s.\n", ref)
	} else {
		fmt.Fprintf(&text, "%s of %s already holds that reference; nothing was sent.\n", field, ref)
	}
	line("Requested", fmt.Sprintf("%s %s, generation %d", field, statusview.Clean(result.Reference), result.Generation))
	if result.Changed {
		line("Replaced", statusview.Clean(result.Previous))
		line("Kept", keptSettings(source))
	}
	line("Observed", observed(result.ObservedGeneration, result.Phase))
	line("Resolved", resolved(source))
	if result.Running != "" {
		line("Running", statusview.Clean(result.Running)+" operation")
	}
	line("Apply", "spec.policy.apply "+orNone(statusview.Clean(string(source.Apply))))
	if result.Changed {
		reads := "reads the database and plans from what it finds"
		if ref.Family == operate.FamilyMigration {
			reads = "reads the migration history and plans the versions still pending"
		}
		fmt.Fprintf(&text, "Once the operator observes generation %d it resolves the new reference, checks it against "+
			"the verification policy, %s. A plan or an approval from the previous artifact does not carry over.\n",
			result.Generation, reads)
		switch {
		case result.ProofOwed:
			text.WriteString("The proof that the last Apply converged comes first: it outranks a newer generation, " +
				"and the new reference is resolved after it.\n")
		case result.Running != "":
			fmt.Fprintf(&text, "The %s operation in flight was decided from the previous spec; the operator discards "+
				"its result once its Job ends.\n", statusview.Clean(result.Running))
		}
		if result.Unresolved {
			fmt.Fprintf(&text, "A run nobody accounted for holds this migration, and while that record stands it "+
				"publishes no plan from any artifact: %s shows the record and how it clears.\n", viewCommand(ref))
		}
		text.WriteString(applyPolicyLine(ref, source.Apply) + "\n")
	}
	if !waiting {
		text.WriteString("set-source --for approval waits for the plan from the new reference in the same command, " +
			"and --for in-sync until the resource rests in sync from it; kubectl ptah wait, run later, counts only a " +
			"reading that begins after it does.\n")
	}
	_, _ = io.WriteString(out, text.String())
}

// keptSettings names the source settings the change left as they were: the
// new artifact is fetched and verified under them.
func keptSettings(source operate.Source) string {
	spec := source.Spec
	var kept []string
	if policy := spec.VerificationPolicyFrom; policy != nil {
		kept = append(kept, fmt.Sprintf("verificationPolicyFrom %s key %s", policy.Name, policy.Key))
	}
	if auth := spec.RegistryAuthFrom; auth != nil {
		kept = append(kept, "registryAuthFrom "+auth.Name)
	}
	if spec.Transport.PlainHTTP {
		kept = append(kept, "transport.plainHTTP true")
	}
	if ca := spec.Transport.CAFrom; ca != nil {
		kept = append(kept, fmt.Sprintf("transport.caFrom %s key %s", ca.Name, ca.Key))
	}
	if len(kept) == 0 {
		return "nothing else is set"
	}
	return statusview.Clean(strings.Join(kept, ", "))
}

// resolved is the artifact the operator last resolved the source to, as the
// status records it: the previous one until it observes the change.
func resolved(source operate.Source) string {
	if source.Resolved == "" {
		return "nothing yet"
	}
	verified := "verified"
	if !source.Verified {
		verified = "not verified"
	}
	return statusview.Clean(source.Resolved) + ", " + verified
}

// applyPolicyLine says what the apply policy lets the new plan do, in the
// terms the API and the approval guide give it.
func applyPolicyLine(ref operate.Ref, policy operatorv1alpha1.ApplyPolicy) string {
	switch policy {
	case operatorv1alpha1.ApplyPolicyAlways:
		if ref.Family == operate.FamilyMigration {
			return "Under spec.policy.apply Always that plan runs as soon as it is published, without a person."
		}
		return "Under spec.policy.apply Always the operator applies that plan without a person, unless it is " +
			"destructive or changes privileges: those wait for an approval."
	case operatorv1alpha1.ApplyPolicyNever:
		return "Under spec.policy.apply Never the plan is recorded and nothing applies it."
	default:
		return fmt.Sprintf("Under spec.policy.apply OnApproval that plan waits for an approval: kubectl ptah "+
			"approve %s/%s -n %s reviews it.", ref.Family, ref.Name, ref.Namespace)
	}
}
