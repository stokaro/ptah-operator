package operate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A PtahSchema proves an Apply by reading the target that Apply was planned
// for. When the URL Secret the proof reads names another target, no read can
// prove the Apply, and the schema stops: ReconciliationFailed=True with reason
// TargetIdentityChanged, and a message that ends with the command a person runs
// to give the proof up (movedTargetFailure in internal/controller):
//
//	kubectl annotate ptahschema NAME -n NAMESPACE operator.ptah.run/settled-apply=VALUE --overwrite
//
// VALUE is the Apply's operation ID and the digest of the target the read named,
// joined by a slash. The controller takes the statement at a read that names
// that target, when the annotation holds VALUE and the condition still prints it
// (proofSettlementAsked). Settle writes that annotation and nothing else, with
// the value read from the condition, so what reaches the controller is what the
// printed command writes.

// settleCommandStart is where the command a stop prints begins.
const settleCommandStart = "kubectl annotate "

// SettleValue is the value a stop prints and what it names.
type SettleValue struct {
	// Value is what the settled-apply annotation has to hold.
	Value string
	// ApplyOperationID is the Apply whose proof it gives up.
	ApplyOperationID string
	// TargetDigest is the target the read named, the one the URL Secret holds.
	TargetDigest string
}

// PrintedSettleValue reads the value a stop's message prints for the PtahSchema
// name names in namespace. The message has to end with the command the stop
// prints, as it prints it, for that resource and nothing else: a message cut
// short, a command for another resource, or words after it print no value a
// person could run.
func PrintedSettleValue(message, name, namespace string) (SettleValue, error) {
	at := strings.LastIndex(message, settleCommandStart)
	if at < 0 {
		return SettleValue{}, errors.New("the message prints no kubectl annotate command")
	}
	fields := strings.Fields(message[at:])
	key := operatorv1alpha1.SettledApplyAnnotation + "="
	if len(fields) != 8 || fields[2] != "ptahschema" || fields[3] != name || fields[4] != "-n" || fields[5] != namespace ||
		!strings.HasPrefix(fields[6], key) || fields[7] != "--overwrite" {
		return SettleValue{}, fmt.Errorf("the message ends with another command than the one that settles ptahschema/%s in namespace %s",
			name, namespace)
	}
	return splitSettleValue(strings.TrimPrefix(fields[6], key))
}

// ParseSettleValue reads a settled-apply value a person gives, as a stop
// prints it: the Apply's operation ID, a slash, and the digest of a target.
func ParseSettleValue(value string) (SettleValue, error) { return splitSettleValue(value) }

// splitSettleValue reads what a settled-apply value names: the Apply's
// operation ID, a slash, and the digest of a target.
func splitSettleValue(value string) (SettleValue, error) {
	slash := strings.LastIndex(value, "/")
	if slash <= 0 || !sha256Digest.MatchString(value[slash+1:]) {
		return SettleValue{}, fmt.Errorf("%q is not an Apply's operation ID and a target's digest", value)
	}
	return SettleValue{Value: value, ApplyOperationID: value[:slash], TargetDigest: value[slash+1:]}, nil
}

// SettleResult is what Settle wrote and what the API server answered.
type SettleResult struct {
	Ref Ref
	// Printed is the value the stop prints. The annotation holds it now.
	Printed SettleValue
	// PlanTarget is the digest of the target the Apply's plan was made for.
	PlanTarget string
	// Plan is the plan the Apply ran, and Approval the approval it consumed,
	// empty when none is recorded.
	Plan     string
	Approval string
	// Outcome is how the Apply ended, as status.pendingObservation records it.
	Outcome operatorv1alpha1.PendingObservationOutcome
	// Journal is the journal's ApplyFinished entry for that Apply, nil when the
	// journal holds none for its plan.
	Journal *operatorv1alpha1.JournalEntry
	// Previous is the value the annotation held before, and Held whether it held
	// one at all.
	Previous string
	Held     bool
	// Changed is false when the annotation already held the printed value and
	// nothing was sent.
	Changed bool
	// Apply is spec.policy.apply, which decides what the next plan may do.
	Apply operatorv1alpha1.ApplyPolicy
	// RetryInterval is spec.execution.failureRetryInterval, the pace of the reads
	// that take the statement.
	RetryInterval time.Duration
	// Suspended reports spec.suspend: a suspended resource starts no read, so the
	// statement is taken after it is resumed, or by a read already under way.
	Suspended bool
	Phase     string
	// ReadyReason is the reason of the Ready condition. Ready alone reports a Pod
	// that cannot start while the stop's condition keeps its command.
	ReadyReason string
	UID         types.UID
	// Object is the resource as the answer carried it, the baseline of a wait.
	Object client.Object
}

// NotStoppedError is a PtahSchema that has not stopped on a proof whose target
// moved, so a statement would settle nothing. Settle sends nothing.
type NotStoppedError struct {
	Ref   Ref
	Phase string
	// Why says what the resource shows instead.
	Why string
	// sent is the refusal of a patch that met the change, when there was one.
	sent error
}

func (e *NotStoppedError) Error() string {
	return fmt.Sprintf("%s has not stopped on a proof whose target moved: %s", e.Ref, e.Why)
}

// Unwrap is the refusal, so Classify reads the change as refused.
func (e *NotStoppedError) Unwrap() error { return e.sent }

// NoSettleValueError is a stop whose condition prints no value Settle can read.
// Settle sends nothing: a value it worked out itself would not be one the stop
// printed, and the controller takes no other.
type NoSettleValueError struct {
	Ref Ref
	// Problem says what the message prints instead.
	Problem string
	sent    error
}

func (e *NoSettleValueError) Error() string {
	return fmt.Sprintf("the stop of %s prints no value to settle it with: %s", e.Ref, e.Problem)
}

// Unwrap is the refusal, so Classify reads the change as refused.
func (e *NoSettleValueError) Unwrap() error { return e.sent }

// StaleSettleValueError is a value the stop prints that no longer matches the
// resource: it names another Apply than the one whose proof is owed, or the
// target the plan was made for, or it is not the value Settle read before the API
// server refused its patch. Settle sends nothing, or nothing more.
type StaleSettleValueError struct {
	Ref     Ref
	Printed string
	// Why says what it no longer matches.
	Why  string
	sent error
}

func (e *StaleSettleValueError) Error() string {
	return fmt.Sprintf("the stop of %s prints %s, which %s", e.Ref, e.Printed, e.Why)
}

// Unwrap is the refusal, so Classify reads the change as refused.
func (e *StaleSettleValueError) Unwrap() error { return e.sent }

// UnexpectedSettleValueError is a stop that prints another value than the one
// the person expected, as when the URL Secret moved again after they read the
// stop. Settle sends nothing: the statement is about one target, and the
// person read about another.
type UnexpectedSettleValueError struct {
	Ref      Ref
	Printed  string
	Expected string
	sent     error
}

func (e *UnexpectedSettleValueError) Error() string {
	return fmt.Sprintf("the stop of %s prints %s, not %s, the value expected", e.Ref, e.Printed, e.Expected)
}

// Unwrap is the refusal, so Classify reads the change as refused.
func (e *UnexpectedSettleValueError) Unwrap() error { return e.sent }

// SettleOptions narrows a settlement.
type SettleOptions struct {
	// Expect is the value the person read from the stop. When it is set, Settle
	// writes that value or nothing: a stop that prints another one is refused
	// with *UnexpectedSettleValueError. Without it, Settle settles the stop it
	// reads, whichever target that stop names.
	Expect string
}

// stopped reads a schema's stop: the proof it owes and the value its condition
// prints, or the reason it has none.
func stopped(ref Ref, schema *operatorv1alpha1.PtahSchema) (SettleValue, error) {
	pending := schema.Status.PendingObservation
	if pending == nil {
		return SettleValue{}, &NotStoppedError{Ref: ref, Phase: string(schema.Status.Phase),
			Why: "it owes no proof of an Apply (status.pendingObservation is empty)"}
	}
	failed := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	switch {
	case failed == nil:
		return SettleValue{}, &NotStoppedError{Ref: ref, Phase: string(schema.Status.Phase),
			Why: "it owes the proof of an Apply and reports no ReconciliationFailed condition"}
	case failed.Status != metav1.ConditionTrue || failed.Reason != string(operatorv1alpha1.ReasonTargetIdentityChanged):
		return SettleValue{}, &NotStoppedError{Ref: ref, Phase: string(schema.Status.Phase),
			Why: fmt.Sprintf("it owes the proof of an Apply, and ReconciliationFailed is %s with reason %s, not True with reason %s",
				failed.Status, orNone(failed.Reason), operatorv1alpha1.ReasonTargetIdentityChanged)}
	}
	printed, err := PrintedSettleValue(failed.Message, schema.Name, schema.Namespace)
	if err != nil {
		return SettleValue{}, &NoSettleValueError{Ref: ref, Problem: err.Error()}
	}
	switch {
	case printed.ApplyOperationID != pending.ApplyOperationID:
		return SettleValue{}, &StaleSettleValueError{Ref: ref, Printed: printed.Value,
			Why: fmt.Sprintf("names Apply %s, and the proof owed is Apply %s's", printed.ApplyOperationID, pending.ApplyOperationID)}
	case printed.TargetDigest == pending.Plan.TargetIdentityDigest:
		return SettleValue{}, &StaleSettleValueError{Ref: ref, Printed: printed.Value,
			Why: "names the target the plan was made for, and a read of that target proves the Apply rather than settling it"}
	}
	return printed, nil
}

// Settle gives up the post-Apply proof of the PtahSchema ref names, as a person
// does by running the command its stop prints: it sets the settled-apply
// annotation to the value the stop's condition prints and changes nothing else.
// The controller takes that statement at the next read of the target the value
// names, and then gives up the proof, its plan and its database lock and reads
// the database again under the URL the Secret holds.
//
// It refuses a resource that has not stopped on a proof whose target moved
// (*NotStoppedError), a stop that prints no value it can read
// (*NoSettleValueError), and a printed value that names another Apply than the
// one whose proof is owed (*StaleSettleValueError), and, with
// options.Expect, a printed value other than the one expected
// (*UnexpectedSettleValueError). It sends nothing when the annotation already
// holds the value.
//
// The patch tests metadata.uid and carries the resourceVersion of the read, as
// SetSource's does, since the value is decided from status: a read that named
// another target in between prints another value. A conflict is read again,
// and a stop that prints another value than the first read is refused rather
// than written, since the report describes the stop this command read. That
// binds the statement to this command's own read, not to whatever the person
// inspected before running it; options.Expect is what binds it to that.
// Anything else is decided again from the fresh read, three times at most.
func Settle(ctx context.Context, c client.Client, ref Ref, options SettleOptions) (SettleResult, error) {
	if ref.Family != FamilySchema {
		return SettleResult{}, fmt.Errorf("%s owes no proof that a statement settles: only a PtahSchema does", ref)
	}
	if options.Expect != "" {
		if _, err := splitSettleValue(options.Expect); err != nil {
			return SettleResult{}, fmt.Errorf("the value expected: %w", err)
		}
	}
	const attempts = 3
	var (
		first    *SettleValue
		firstUID types.UID
		lastErr  error
	)
	for range attempts {
		schema := &operatorv1alpha1.PtahSchema{}
		if err := c.Get(ctx, ref.key(), schema); err != nil {
			if lastErr != nil {
				return SettleResult{}, &rereadError{refused: lastErr, read: ref.failed("read", err)}
			}
			return SettleResult{}, ref.failed("read", err)
		}
		if first != nil && schema.UID != firstUID {
			return SettleResult{}, ref.failed("change", ErrReplaced)
		}
		printed, err := stopped(ref, schema)
		if err != nil {
			return SettleResult{}, withRefusal(err, lastErr)
		}
		if options.Expect != "" && printed.Value != options.Expect {
			return SettleResult{}, &UnexpectedSettleValueError{Ref: ref, Printed: printed.Value, Expected: options.Expect, sent: lastErr}
		}
		switch {
		case first == nil:
			first, firstUID = &printed, schema.UID
		case printed.Value != first.Value:
			return SettleResult{}, &StaleSettleValueError{Ref: ref, Printed: printed.Value, sent: lastErr,
				Why: fmt.Sprintf("is not %s, the value it printed when this command read it: a read named another target since", first.Value)}
		}
		previous, held := schema.Annotations[operatorv1alpha1.SettledApplyAnnotation]
		if held && previous == printed.Value {
			return settleResult(ref, printed, previous, held, false, schema), nil
		}
		// The API server leaves an empty map out of what it returns, so a resource
		// with no annotations has no member to add a key to.
		bare := len(schema.Annotations) == 0
		operations := []patchOperation{
			{Op: "test", Path: "/metadata/uid", Value: schema.UID},
			{Op: "replace", Path: "/metadata/resourceVersion", Value: schema.ResourceVersion},
		}
		if bare {
			operations = append(operations,
				patchOperation{Op: "test", Path: "/metadata/annotations", Value: nil},
				patchOperation{Op: "add", Path: "/metadata/annotations",
					Value: map[string]string{operatorv1alpha1.SettledApplyAnnotation: printed.Value}},
			)
		} else {
			operations = append(operations, patchOperation{
				Op: "add", Path: "/metadata/annotations/" + pointerEscape(operatorv1alpha1.SettledApplyAnnotation), Value: printed.Value,
			})
		}
		patch, err := json.Marshal(operations)
		if err != nil {
			return SettleResult{}, err
		}
		uid := schema.UID
		err = c.Patch(ctx, schema, client.RawPatch(types.JSONPatchType, patch), client.FieldOwner(FieldManager))
		if err == nil {
			return settleResult(ref, printed, previous, held, true, schema), nil
		}
		lastErr = classifyPatchError(ctx, c, ref, uid, &bare, err)
		if (!apierrors.IsConflict(err) && !errors.Is(lastErr, errAnnotationsChanged)) ||
			errors.Is(lastErr, ErrReplaced) || apierrors.IsNotFound(lastErr) {
			return SettleResult{}, lastErr
		}
	}
	return SettleResult{}, lastErr
}

// withRefusal puts the refusal of an earlier patch on a refusal Settle decided
// after reading again, so Classify reads the change as refused.
func withRefusal(err, sent error) error {
	var (
		notStopped *NotStoppedError
		noValue    *NoSettleValueError
		stale      *StaleSettleValueError
	)
	switch {
	case errors.As(err, &notStopped):
		notStopped.sent = sent
	case errors.As(err, &noValue):
		noValue.sent = sent
	case errors.As(err, &stale):
		stale.sent = sent
	}
	return err
}

func settleResult(ref Ref, printed SettleValue, previous string, held, changed bool, schema *operatorv1alpha1.PtahSchema) SettleResult {
	result := SettleResult{
		Ref: ref, Printed: printed, Previous: previous, Held: held, Changed: changed,
		Apply: schema.Spec.Policy.Apply, RetryInterval: schema.Spec.Execution.FailureRetryInterval.Duration,
		Suspended: schema.Spec.Suspend, Phase: string(schema.Status.Phase), UID: schema.UID, Object: schema,
	}
	if ready := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReady); ready != nil {
		result.ReadyReason = ready.Reason
	}
	if pending := schema.Status.PendingObservation; pending != nil {
		result.PlanTarget, result.Plan, result.Outcome = pending.Plan.TargetIdentityDigest, pending.Plan.Name, pending.Outcome
		result.Journal = applyFinished(schema.Status.Journal, pending.Plan.Name)
	}
	if plan := schema.Status.Plan; plan != nil && plan.Approval != nil {
		result.Approval = plan.Approval.Name
	}
	return result
}

// applyFinished is the journal's newest ApplyFinished entry for plan: the Apply
// whose proof is owed, since a later Apply of the same plan waits for that proof.
func applyFinished(journal *operatorv1alpha1.LifecycleJournal, plan string) *operatorv1alpha1.JournalEntry {
	if journal == nil || plan == "" {
		return nil
	}
	for index := len(journal.Entries) - 1; index >= 0; index-- {
		entry := journal.Entries[index]
		if entry.Type == operatorv1alpha1.JournalApplyFinished && entry.Plan == plan {
			return &entry
		}
	}
	return nil
}

// TargetSettled is the end of a settlement: the operator took the statement
// Settle wrote and gave the proof up, which the journal records as a Verified
// entry for the Apply's plan with outcome Unknown. It is waited on only with
// WaitOptions.Settlement and a baseline that still owes the proof, such as
// SettleResult.Object.
const TargetSettled Target = "settled"

const (
	// StopProofEnded is a proof that ended without the statement: a read of the
	// target the plan was made for proved the Apply or found changes still to
	// make, or the proof settled under a newer generation, policy or execution
	// binding.
	StopProofEnded StopReason = "ProofEnded"
	// StopStatementWithdrawn is a settled-apply annotation that no longer holds
	// the value the wait is for while the proof is still owed: nothing settles it.
	StopStatementWithdrawn StopReason = "StatementWithdrawn"
)

// checkSettled decides a wait for the statement Settle wrote.
func (w *waiter) checkSettled(s snapshot) verdict {
	schema, ok := s.object.(*operatorv1alpha1.PtahSchema)
	if !ok {
		return waitingFor("a PtahSchema")
	}
	value := w.settlement
	pending := schema.Status.PendingObservation
	if pending == nil || pending.ApplyOperationID != value.ApplyOperationID {
		return w.proofEnded(s, schema)
	}
	if held := schema.Annotations[operatorv1alpha1.SettledApplyAnnotation]; held != value.Value {
		return w.stop(s, StopStatementWithdrawn, fmt.Sprintf("the %s annotation no longer holds %s, so no read settles the "+
			"proof of Apply %s", operatorv1alpha1.SettledApplyAnnotation, value.Value, value.ApplyOperationID))
	}
	if s.suspend {
		return waitingFor("resume: a suspended resource starts no read, and the statement waits for one that names target %s",
			value.TargetDigest)
	}
	// Which target the URL Secret holds is not in status, and a read of the
	// plan's own target, once the first URL is back, finishes the proof
	// without the statement. Every answer below says so rather than promise
	// that the next read takes it.
	first := pending.Plan.TargetIdentityDigest
	failed := meta.FindStatusCondition(s.conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if failed == nil || failed.Status != metav1.ConditionTrue || failed.Reason != string(operatorv1alpha1.ReasonTargetIdentityChanged) {
		return waitingFor("a read that names target %s and stops there: the last read did not stop on a moved target. It "+
			"failed and named no target, or it read target %s, the one the plan was made for, where the proof finishes "+
			"without the statement", value.TargetDigest, orNone(first))
	}
	if printed, err := PrintedSettleValue(failed.Message, schema.Name, schema.Namespace); err != nil || printed.Value != value.Value {
		return waitingFor("a stop on target %s again: the last read named another target, and the statement waits for a "+
			"stop that prints it", value.TargetDigest)
	}
	return waitingFor("the next read of target %s, which takes the statement (spec.execution.failureRetryInterval is %s), "+
		"unless the URL Secret holds target %s again, the one the plan was made for: a read of it finishes the proof "+
		"instead", value.TargetDigest, schema.Spec.Execution.FailureRetryInterval.Duration, orNone(first))
}

// proofEnded decides a document whose proof of the Apply is no longer owed, by
// the journal's Verified entry for its plan that the baseline does not hold.
func (w *waiter) proofEnded(s snapshot, schema *operatorv1alpha1.PtahSchema) verdict {
	plan := w.baseline.proofPlan.name
	ended := newVerified(w.baseline.journal, s.journal, plan)
	if ended == nil {
		// The status write that ends a proof records it in the journal, so a
		// proof gone with no new entry is not one this wait can call settled.
		return w.stop(s, StopProofEnded, fmt.Sprintf("the proof of Apply %s is no longer owed, and the journal records no "+
			"reading of plan %s that ended it", w.settlement.ApplyOperationID, orNone(plan)))
	}
	switch ended.Outcome {
	case operatorv1alpha1.JournalUnknown:
		return verdict{done: true}
	case operatorv1alpha1.JournalInSync:
		return w.stop(s, StopProofEnded, fmt.Sprintf("a read of the target plan %s was made for proved the Apply, and the "+
			"journal records Verified InSync: the statement was not needed", plan))
	case operatorv1alpha1.JournalDriftRemains:
		return w.stop(s, StopProofEnded, fmt.Sprintf("a read of the target plan %s was made for ended the proof, and the "+
			"journal records Verified DriftRemains: changes are still to make", plan))
	default:
		return w.stop(s, StopProofEnded, fmt.Sprintf("the proof settled under a newer generation, policy or execution "+
			"binding, and the journal records Verified %s for plan %s", orNone(string(ended.Outcome)), plan))
	}
}

// newVerified is the newest Verified entry for plan in journal that baseline
// does not hold. Entries are told apart by their time and outcome, compared for
// equality: a proof ends once, and the entry that records it is written in the
// same status write.
func newVerified(baseline, journal *operatorv1alpha1.LifecycleJournal, plan string) *operatorv1alpha1.JournalEntry {
	if journal == nil || plan == "" {
		return nil
	}
	held := func(entry operatorv1alpha1.JournalEntry) bool {
		if baseline == nil {
			return false
		}
		for _, old := range baseline.Entries {
			if old.Type == entry.Type && old.Plan == entry.Plan && old.Outcome == entry.Outcome && old.At.Equal(&entry.At) {
				return true
			}
		}
		return false
	}
	for index := len(journal.Entries) - 1; index >= 0; index-- {
		entry := journal.Entries[index]
		if entry.Type == operatorv1alpha1.JournalVerified && entry.Plan == plan {
			if held(entry) {
				return nil
			}
			return &entry
		}
	}
	return nil
}
