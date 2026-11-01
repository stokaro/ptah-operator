package operate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// Target is the state a wait is for.
type Target string

const (
	// TargetRead is the end of the read a read request asked for: the request
	// answered, a read of the database that began after it, and nothing in
	// flight.
	TargetRead Target = "read"
	// TargetInSync is a resource that rests in sync at the required
	// generation, by a read of the database that began after the baseline.
	TargetInSync Target = "in-sync"
	// TargetApproval is a resource waiting at the approval gate in the state
	// approval admission requires of its status, with a plan from a read of
	// the database that began after the baseline. Admission also checks the
	// plan object and its bindings, which a wait does not read.
	TargetApproval Target = "approval"
	// TargetApplied is an Apply or a migration run that ended after the
	// baseline, recorded applied and settled by the operator's own reading of
	// the database. Any other ending stops the wait with a StopError.
	TargetApplied Target = "applied"
	// TargetSuspended is a resource the controller reports Suspended at the
	// required generation, with nothing in flight: for a schema, its
	// Suspended condition at that generation. A schema whose post-Apply proof
	// is still owed reaches it with the proof owed: the proof waits for
	// resume, and no Job runs meanwhile.
	TargetSuspended Target = "suspended"
)

// ParseTarget reads a target as a command line names it.
func ParseTarget(value string) (Target, error) {
	switch target := Target(strings.ToLower(value)); target {
	case TargetRead, TargetInSync, TargetApproval, TargetApplied, TargetSuspended:
		return target, nil
	default:
		return "", fmt.Errorf("unknown wait target %q: use read, in-sync, approval, applied or suspended", value)
	}
}

// DefaultInterval is how often a wait reads the resource when the options
// name no interval.
const DefaultInterval = 2 * time.Second

// Clock is the time a wait measures its timeout by and sleeps on. It decides
// nothing else: every comparison between two events uses the timestamps the
// operator wrote.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// WaitOptions says what a wait is for and how long it may take.
type WaitOptions struct {
	Target Target
	// Timeout bounds the wait. It is required.
	Timeout time.Duration
	// Interval is the time between reads; DefaultInterval when zero.
	Interval time.Duration
	// Clock is the real clock when nil. With the real clock each read also
	// carries the wait's deadline, so a read that hangs cannot outlive the
	// timeout.
	Clock Clock
	// Since is the baseline: the resource as it was before the change the
	// wait is about, such as SuspendResult.Object or RefreshResult.Object.
	// Nothing it already shows counts. When nil the wait's first read is the
	// baseline, which can miss a change that completes before it.
	//
	// For TargetRead it has to have been read after the request was written,
	// and a document whose annotation does not hold ReadRequest is refused:
	// one read before lets a read that started in between count as the answer.
	Since client.Object
	// Generation is the least status.observedGeneration the target needs,
	// such as SuspendResult.Generation. When zero, TargetInSync,
	// TargetApproval and TargetSuspended need the baseline's generation, and
	// TargetRead and TargetApplied need none: a read request and an approval
	// leave the generation where it was.
	Generation int64
	// ReadRequest is the annotation value TargetRead waits on, as
	// RefreshResult.Request returned it. It is required for TargetRead and
	// refused for any other target.
	ReadRequest string
	// Plan narrows TargetApplied, for a PtahSchema only, to the plan of that
	// name or fingerprint. Another plan applied first stops the wait.
	Plan string
}

// Result is the document that met the target.
type Result struct {
	Ref                Ref
	Target             Target
	Phase              string
	Generation         int64
	ObservedGeneration int64
	// ObservedAt is when the database was last read, as the document says.
	ObservedAt time.Time
	// Reads is how many times the resource was read.
	Reads  int
	Object client.Object
}

// StopReason is why a wait ended without reaching its target.
type StopReason string

const (
	// StopReadReplaced is a read request another value replaced before the
	// operator took it.
	StopReadReplaced StopReason = "ReadRequestReplaced"
	// StopReadFailed is a read that failed after the operator took the
	// request, with no claim left to retry it: the operator starts another
	// read after its retry delay. An attempt the operator retries under the
	// same claim does not stop the wait; a timeout names its failure.
	StopReadFailed StopReason = "ReadFailed"
	// StopApplyFailed is an Apply or a run that failed, or an Apply whose
	// claim the operator gave up before it ran.
	StopApplyFailed StopReason = "ApplyFailed"
	// StopUnresolved is an Apply or a run whose effect on the database
	// nobody established.
	StopUnresolved StopReason = "OutcomeUnknown"
	// StopNotConverged is an Apply or a run the operator's reading of the
	// database afterwards did not confirm: a schema whose proof still found
	// changes or settled under newer inputs, a migration whose history read
	// back blocked.
	StopNotConverged StopReason = "NotConverged"
	// StopOtherPlanApplied is a schema that applied another plan than the
	// one the wait named.
	StopOtherPlanApplied StopReason = "OtherPlanApplied"
	// StopNothingApplied is a migration run that found nothing to apply.
	StopNothingApplied StopReason = "NothingApplied"
	// StopNotSuspended is a wait for suspension on a resource whose spec
	// does not ask for it.
	StopNotSuspended StopReason = "NotSuspended"
	// StopDeleting is a resource that is being deleted.
	StopDeleting StopReason = "Deleting"
)

// StopError is a wait that ended because the resource recorded an outcome
// that is not the target and will not become it.
type StopError struct {
	Ref     Ref
	Target  Target
	Reason  StopReason
	Phase   string
	Message string
}

func (e *StopError) Error() string {
	return fmt.Sprintf("%s will not reach %s: %s: %s", e.Ref, e.Target, e.Reason, e.Message)
}

// TimeoutError is a wait whose timeout passed first. It names the phase the
// last read found, the condition that best says what holds the resource,
// and what the target still needed.
type TimeoutError struct {
	Ref    Ref
	Target Target
	Waited time.Duration
	// Phase is empty when the resource was never read.
	Phase string
	// Blocker is a copy of the condition that says what holds the resource:
	// a failure or refusal the operator recorded, or Ready when it is not
	// True.
	Blocker *metav1.Condition
	// Waiting is what the target still needed in the last document read.
	Waiting string
	// LastError is the error of the last read, when it failed and was
	// retried.
	LastError error
}

func (e *TimeoutError) Error() string {
	var message strings.Builder
	fmt.Fprintf(&message, "timed out after %s waiting for %s to reach %s", e.Waited, e.Ref, e.Target)
	if e.Phase != "" {
		fmt.Fprintf(&message, ": phase %s", e.Phase)
	}
	if e.Blocker != nil {
		fmt.Fprintf(&message, "; %s=%s (%s): %s", e.Blocker.Type, e.Blocker.Status, e.Blocker.Reason, e.Blocker.Message)
	}
	if e.Waiting != "" {
		fmt.Fprintf(&message, "; still waiting for %s", e.Waiting)
	}
	if e.LastError != nil {
		fmt.Fprintf(&message, "; last read failed: %v", e.LastError)
	}
	return message.String()
}

// Wait reads the resource until it reaches options.Target, the timeout
// passes, or the resource records an outcome the target cannot follow.
//
// It returns a *StopError for such an outcome or a resource being deleted, a
// *TimeoutError on timeout, ErrReplaced when the name comes to hold another
// resource, and the API server's error for a resource that is gone, a kind
// the server does not serve, or a read it refuses. A failed read the server
// may answer next time is retried until the timeout.
//
// Status timestamps carry whole seconds, so a reading made in the same second
// as the baseline's is not newer than it. That can only make a wait last
// longer, never end it early, and a read cycle runs several Jobs, so two
// readings within one second do not occur in practice.
func Wait(ctx context.Context, reader client.Reader, ref Ref, options WaitOptions) (Result, error) {
	w, err := newWaiter(ref, options)
	if err != nil {
		return Result{}, err
	}
	options = w.options
	clock := options.Clock
	if clock == nil {
		clock = realClock{}
	}
	interval := options.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	start := clock.Now()
	deadline := start.Add(options.Timeout)
	var last *snapshot
	var waiting string
	var lastErr error
	reads := 0
	for {
		object, err := ref.newObject()
		if err != nil {
			return Result{}, err
		}
		err = get(ctx, reader, ref, object, deadline, options.Clock == nil)
		reads++
		switch {
		case err == nil:
			current, err := snapshotOf(object)
			if err != nil {
				return Result{}, err
			}
			if w.baseline == nil {
				w.begin(current)
			}
			if current.uid != w.baseline.uid {
				return Result{}, ref.failed("wait for", ErrReplaced)
			}
			last, lastErr = &current, nil
			if current.deleting {
				return Result{}, w.stop(current, StopDeleting, "metadata.deletionTimestamp is set").stop
			}
			verdict := w.check(current)
			if verdict.stop != nil {
				return Result{}, verdict.stop
			}
			if verdict.done {
				return Result{
					Ref: ref, Target: options.Target, Phase: current.phase,
					Generation: current.generation, ObservedGeneration: current.observedGeneration,
					ObservedAt: current.observedAt, Reads: reads, Object: current.object,
				}, nil
			}
			waiting = verdict.waiting
		case ctx.Err() != nil:
			return Result{}, fmt.Errorf("wait for %s: %w", ref, ctx.Err())
		case errors.Is(err, context.DeadlineExceeded):
			// The read carried the wait's own deadline: the timeout below
			// reports it.
			lastErr = err
		case !retryable(err):
			return Result{}, ref.failed("wait for", err)
		default:
			lastErr = err
		}
		now := clock.Now()
		if !now.Before(deadline) {
			timeout := &TimeoutError{
				Ref: ref, Target: options.Target, Waited: now.Sub(start), Waiting: waiting, LastError: lastErr,
			}
			if last != nil {
				timeout.Phase = last.phase
				timeout.Blocker = last.blocker()
			}
			return Result{}, timeout
		}
		select {
		case <-ctx.Done():
			return Result{}, fmt.Errorf("wait for %s: %w", ref, ctx.Err())
		case <-clock.After(min(interval, deadline.Sub(now))):
		}
	}
}

// get reads the resource. With the real clock the read carries the wait's
// deadline; an injected clock's instants are not the context's.
func get(ctx context.Context, reader client.Reader, ref Ref, object client.Object, deadline time.Time, real bool) error {
	if !real {
		return reader.Get(ctx, ref.key(), object)
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return reader.Get(bounded, ref.key(), object)
}

// retryable is a read the API server may answer next time. A kind the server
// does not serve, or one this client cannot encode, is not: the CRD is not
// installed, and waiting does not install it.
func retryable(err error) bool {
	if meta.IsNoMatchError(err) || inChain(err, runtime.IsNotRegisteredError) {
		return false
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		// A transport failure: the next read may reach the server.
		return true
	}
	return apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) || apierrors.IsUnexpectedServerError(err)
}

// inChain applies is to err and to every error it wraps. Some apimachinery
// predicates, IsNotRegisteredError among them, look at the error alone.
func inChain(err error, is func(error) bool) bool {
	for ; err != nil; err = errors.Unwrap(err) {
		if is(err) {
			return true
		}
	}
	return false
}

type verdict struct {
	done    bool
	stop    *StopError
	waiting string
}

// freshness decides which reading of the database a target may count: one
// newer than floor that is not the harvest of a read already running at the
// baseline.
//
// The operator stamps a reading when it collects the Job's result, not when
// the Job read the database. A schema's Observe or a migration's History
// claim in flight at the baseline may have read the database before the
// change the wait is about and still be stamped after the baseline. The
// first newer reading is that claim's, so it moves the floor instead of
// counting. A claim for a generation older than the wait requires is not
// skipped: its inputs no longer hold, and the operator discards it rather
// than recording its reading.
type freshness struct {
	floor   time.Time
	skipOne bool
}

func newFreshness(baseline snapshot, minGeneration int64) freshness {
	return freshness{
		floor:   baseline.observedAt,
		skipOne: baseline.readingDatabase() && minGeneration <= baseline.observedGeneration,
	}
}

// see takes the documents in the order they were read and reports whether
// this one's reading counts.
func (f *freshness) see(s snapshot) bool {
	if !s.observedAt.After(f.floor) {
		return false
	}
	if f.skipOne {
		f.skipOne = false
		f.floor = s.observedAt
		return false
	}
	return true
}

// raise moves the floor to at when at is later.
func (f *freshness) raise(at time.Time) {
	if at.After(f.floor) {
		f.floor = at
	}
	f.skipOne = false
}

type waiter struct {
	ref           Ref
	options       WaitOptions
	token         string
	minGeneration int64
	baseline      *snapshot
	fresh         freshness

	// The read target's state. When the baseline does not rest, the floor
	// and failureFloor are raised to the document in which the operator
	// takes the request, which it does only once the cycle under way is over:
	// that cycle's reading and its failures belong to no read the request
	// asked for.
	answered      bool
	floorAtAnswer bool
	failureFloor  failureMark
}

func newWaiter(ref Ref, options WaitOptions) (*waiter, error) {
	target, err := ParseTarget(string(options.Target))
	if err != nil {
		return nil, err
	}
	options.Target = target
	if _, err := ref.newObject(); err != nil {
		return nil, err
	}
	if options.Timeout <= 0 {
		return nil, errors.New("a wait needs a timeout")
	}
	w := &waiter{ref: ref, options: options}
	switch {
	case options.Target == TargetRead:
		if options.ReadRequest == "" {
			return nil, errors.New("a wait for a read needs the read request value")
		}
		if _, err := time.Parse(time.RFC3339, options.ReadRequest); err != nil {
			return nil, fmt.Errorf("read request %q is not an RFC 3339 instant, so it starts no read", options.ReadRequest)
		}
		w.token = operatorv1alpha1.ReadRequestToken(options.ReadRequest)
	case options.ReadRequest != "":
		return nil, fmt.Errorf("a read request is only waited on with target %s", TargetRead)
	}
	if options.Plan != "" && (options.Target != TargetApplied || ref.Family != FamilySchema) {
		return nil, errors.New("a plan narrows only a wait for a PtahSchema to be applied: a migration run records no plan")
	}
	if options.Since != nil {
		since, err := snapshotOf(options.Since)
		if err != nil {
			return nil, err
		}
		if since.family != ref.Family {
			return nil, fmt.Errorf("the baseline is a %s, not a %s", since.family, ref.Family)
		}
		if options.Target == TargetRead && since.readRequest != options.ReadRequest {
			return nil, fmt.Errorf("the baseline was read before read request %q was written; read it after", options.ReadRequest)
		}
		w.begin(since)
	}
	return w, nil
}

// begin records the baseline.
func (w *waiter) begin(baseline snapshot) {
	w.baseline = &baseline
	w.minGeneration = w.options.Generation
	if w.minGeneration == 0 {
		switch w.options.Target {
		case TargetInSync, TargetApproval, TargetSuspended:
			w.minGeneration = baseline.generation
		}
	}
	w.fresh = newFreshness(baseline, w.minGeneration)
	w.failureFloor = baseline.failureMark()
	if w.options.Target != TargetRead {
		return
	}
	switch {
	case baseline.lastReadRequest == w.token:
		// Taken before the baseline, so a claim in flight now began after
		// the operator took it.
		w.answered = true
		w.fresh.skipOne = false
	case !baseline.resting():
		w.floorAtAnswer = true
	}
}

func (w *waiter) check(s snapshot) verdict {
	switch w.options.Target {
	case TargetRead:
		return w.checkRead(s)
	case TargetInSync:
		return w.checkInSync(s)
	case TargetApproval:
		return w.checkApproval(s)
	case TargetApplied:
		return w.checkApplied(s)
	default:
		return w.checkSuspended(s)
	}
}

func (w *waiter) stop(s snapshot, reason StopReason, message string) verdict {
	return verdict{stop: &StopError{Ref: w.ref, Target: w.options.Target, Reason: reason, Phase: s.phase, Message: message}}
}

func waitingFor(format string, arguments ...any) verdict {
	return verdict{waiting: fmt.Sprintf(format, arguments...)}
}

func (w *waiter) generationReached(s snapshot) (verdict, bool) {
	if s.observedGeneration < w.minGeneration {
		return waitingFor("the operator to observe generation %d (status.observedGeneration is %d)",
			w.minGeneration, s.observedGeneration), false
	}
	return verdict{}, true
}

func (w *waiter) staleReading(s snapshot) verdict {
	return waitingFor("a read of the database that began after the wait did (the last one is from %s)", instant(s.observedAt))
}

func (w *waiter) checkRead(s snapshot) verdict {
	fresh := w.fresh.see(s)
	if !w.answered {
		switch {
		case s.lastReadRequest == w.token:
			w.answered = true
			if w.floorAtAnswer {
				w.fresh.raise(s.observedAt)
				w.failureFloor = s.failureMark()
				fresh = false
			}
		case s.readRequest != w.options.ReadRequest:
			return w.stop(s, StopReadReplaced, fmt.Sprintf("%s holds %q, so the operator will not take request %q",
				operatorv1alpha1.ReadRequestAnnotation, s.readRequest, w.options.ReadRequest))
		default:
			return waitingFor("the operator to take read request %s; it does once no operation is running, "+
				"the resource is not suspended and no retry delay is pending", w.options.ReadRequest)
		}
	}
	// A failed attempt the operator retries under the same claim is not the
	// end of the read; only a failure that leaves no claim is.
	if mark := s.failureMark(); s.phase == phaseFailed && s.operation == "" && mark.present && !mark.equal(w.failureFloor) {
		message := "the read failed"
		if blocker := s.blocker(); blocker != nil {
			message = fmt.Sprintf("the read failed (%s): %s", blocker.Reason, blocker.Message)
		}
		return w.stop(s, StopReadFailed, message+"; the operator starts another read after its retry delay")
	}
	if !fresh {
		return waitingFor("a read of the database that began after request %s (the last one is from %s)",
			w.options.ReadRequest, instant(s.observedAt))
	}
	if !s.resting() {
		return waitingFor("the read to end: %s", inFlight(s))
	}
	if v, ok := w.generationReached(s); !ok {
		return v
	}
	return verdict{done: true}
}

func (w *waiter) checkInSync(s snapshot) verdict {
	fresh := w.fresh.see(s)
	if v, ok := w.generationReached(s); !ok {
		return v
	}
	if !fresh {
		return w.staleReading(s)
	}
	conditionType := s.inSyncCondition()
	condition := s.condition(conditionType)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration < w.minGeneration ||
		s.phase != phaseInSync {
		return waitingFor("phase InSync with %s True at generation %d", conditionType, w.minGeneration)
	}
	if !s.resting() || s.unresolved != nil {
		return waitingFor("nothing in flight: %s", inFlight(s))
	}
	return verdict{done: true}
}

func (w *waiter) checkApproval(s snapshot) verdict {
	fresh := w.fresh.see(s)
	if v, ok := w.generationReached(s); !ok {
		return v
	}
	if s.operation != "" || s.proof {
		return waitingFor("%s to end", inFlight(s))
	}
	if s.phase != phaseAwaitingApproval || !s.conditionTrue(operatorv1alpha1.ConditionApprovalRequired) {
		return waitingFor("phase AwaitingApproval with ApprovalRequired True")
	}
	if s.plan == nil {
		return waitingFor("a published plan")
	}
	if s.plan.approved {
		return waitingFor("a plan with no approval recorded: plan %s already has one", s.plan.name)
	}
	if s.family == FamilyMigration && !s.history {
		return waitingFor("a reading of the migration history")
	}
	// A plan is computed from a reading. One republished from a reading older
	// than the wait, under the same name and UID or a new one, is not news.
	if !fresh {
		return w.staleReading(s)
	}
	return verdict{done: true}
}

func (w *waiter) checkApplied(s snapshot) verdict {
	if s.family == FamilyMigration {
		return w.checkRun(s)
	}
	b := w.baseline
	// status.applied is written only by the proof that read the database
	// after the Apply and found it converged.
	if applied := s.applied; applied != nil && (b.applied == nil || applied.planUID != b.applied.planUID ||
		!applied.completedAt.Equal(b.applied.completedAt)) {
		if plan := w.options.Plan; plan != "" && applied.plan != plan && applied.fingerprint != plan {
			return w.stop(s, StopOtherPlanApplied, fmt.Sprintf("plan %s was applied, not %s", applied.plan, plan))
		}
		if v, ok := w.generationReached(s); !ok {
			return v
		}
		return verdict{done: true}
	}
	for _, entry := range journalSince(b.journal, s.journal) {
		switch {
		case entry.Type == operatorv1alpha1.JournalApplyFinished && entry.Outcome == operatorv1alpha1.JournalUnknown:
			return w.stop(s, StopUnresolved, fmt.Sprintf("the Apply of plan %s ended with an outcome nobody established; "+
				"the operator proves convergence before anything else runs, and records no Apply for it", entry.Plan))
		case entry.Type == operatorv1alpha1.JournalApplyFinished && entry.Outcome != operatorv1alpha1.JournalSucceeded:
			return w.stop(s, StopApplyFailed, fmt.Sprintf("the Apply of plan %s ended %s", entry.Plan, entry.Outcome))
		case entry.Type == operatorv1alpha1.JournalVerified && entry.Outcome == operatorv1alpha1.JournalDriftRemains:
			return w.stop(s, StopNotConverged, fmt.Sprintf("after the Apply of plan %s the database still differs "+
				"from the desired schema", entry.Plan))
		case entry.Type == operatorv1alpha1.JournalVerified && entry.Outcome == operatorv1alpha1.JournalSuperseded:
			return w.stop(s, StopNotConverged, fmt.Sprintf("the proof after the Apply of plan %s settled under a "+
				"newer generation, policy or execution binding", entry.Plan))
		}
	}
	// An Apply whose dispatch the operator refused keeps its claim and is
	// tried again, so that failure leaves the wait running and its timeout
	// names it. A schema gives an Apply claim up only when its outcome is
	// unknown, which ApplyFinished records above.
	return waitingFor("an Apply that ends after the wait began; now %s", inFlight(s))
}

func (w *waiter) checkRun(s snapshot) verdict {
	b := w.baseline
	if unresolved := s.unresolved; unresolved != nil &&
		(b.unresolved == nil || b.unresolved.operationID != unresolved.operationID) {
		return w.stop(s, StopUnresolved, fmt.Sprintf("the run of plan %s (Job %s) ended %s: its effect on the "+
			"database is not established, and nothing runs until a reading or a person accounts for it",
			unresolved.plan, unresolved.jobName, unresolved.outcome))
	}
	if run := s.lastRun; run != nil && run.finished && newRun(b.lastRun, run) {
		switch run.outcome {
		case operatorv1alpha1.MigrationRunOutcomeApplied:
			// The run's own account is written with VerifyingHistory, which
			// does not rest, and the operator reads the history back before
			// the resource settles: only that reading says what the revision
			// table holds.
			if !s.resting() || s.observedAt.Before(run.finishedAt) {
				return waitingFor("the history read back after run %s; now %s", run.jobName, inFlight(s))
			}
			if s.phase == string(operatorv1alpha1.MigrationPhaseBlocked) {
				message := "the history read back afterwards is blocked"
				if blocker := s.blocker(); blocker != nil {
					message = fmt.Sprintf("the history read back afterwards is blocked (%s): %s", blocker.Reason, blocker.Message)
				}
				return w.stop(s, StopNotConverged, fmt.Sprintf("run %s recorded its migrations applied, and %s",
					run.jobName, message))
			}
			if v, ok := w.generationReached(s); !ok {
				return v
			}
			return verdict{done: true}
		case operatorv1alpha1.MigrationRunOutcomeUpToDate:
			return w.stop(s, StopNothingApplied, fmt.Sprintf("the run (Job %s) found nothing to apply", run.jobName))
		case operatorv1alpha1.MigrationRunOutcomePartial, operatorv1alpha1.MigrationRunOutcomeUnknown:
			return w.stop(s, StopUnresolved, fmt.Sprintf("the run (Job %s) ended %s: %s", run.jobName, run.outcome, run.message))
		default:
			return w.stop(s, StopApplyFailed, fmt.Sprintf("the run (Job %s) ended %s: %s", run.jobName, run.outcome, run.message))
		}
	}
	// An Apply claim the operator could not dispatch is given up, recorded as
	// a failed Apply, and leaves no claim. A retried read keeps its claim.
	for _, entry := range journalSince(b.journal, s.journal) {
		if entry.Type == operatorv1alpha1.JournalOperationFailed &&
			entry.Operation == string(operatorv1alpha1.MigrationOperationApply) && s.operation == "" {
			return w.stop(s, StopApplyFailed, w.failureMessage(s, entry))
		}
	}
	return waitingFor("a run that finishes after the wait began; now %s", inFlight(s))
}

// newRun reports a run other than the one the baseline recorded. The
// operator writes status.lastRun once, finished, when a run ends; the claim's
// start and the Job tell two runs apart even when a run was settled without
// its Job.
func newRun(baseline, run *runRecord) bool {
	return baseline == nil || baseline.jobUID != run.jobUID || baseline.jobName != run.jobName ||
		!baseline.startedAt.Equal(run.startedAt)
}

func (w *waiter) failureMessage(s snapshot, entry operatorv1alpha1.JournalEntry) string {
	message := fmt.Sprintf("the Apply could not run (%s)", entry.Reason)
	if blocker := s.blocker(); blocker != nil && blocker.Reason == entry.Reason {
		message += ": " + blocker.Message
	}
	return message
}

func (w *waiter) checkSuspended(s snapshot) verdict {
	if !s.suspend {
		return w.stop(s, StopNotSuspended, "spec.suspend is false")
	}
	// The operator drops a claim whose Job it has not created and every read
	// when it sees the suspension, and observes an Apply, or a proof Job
	// already running after one, to its end. A proof still owed starts no Job
	// until resume, so it does not hold the target.
	if s.operation != "" {
		return waitingFor("%s to end: suspension starts no new Job, observes an Apply or a running proof to "+
			"its end, and drops any other claim", inFlight(s))
	}
	if s.family == FamilySchema {
		return w.checkSchemaSuspended(s)
	}
	if v, ok := w.generationReached(s); !ok {
		return v
	}
	if s.phase != phaseSuspended {
		return waitingFor("phase Suspended")
	}
	return verdict{done: true}
}

// checkSchemaSuspended decides a schema by its Suspended condition rather
// than by status.observedGeneration. A schema that owes post-Apply proof
// keeps observedGeneration at the Apply's generation until the proof ends,
// and the proof does not run while it is suspended; every write of phase
// Suspended stamps the condition with the generation the operator acted on
// (internal/controller/schema_controller.go, setCondition).
func (w *waiter) checkSchemaSuspended(s snapshot) verdict {
	suspended := s.condition(operatorv1alpha1.ConditionSuspended)
	switch {
	case s.phase != phaseSuspended:
		return waitingFor("phase Suspended")
	case suspended == nil || suspended.Status != metav1.ConditionTrue:
		return waitingFor("the Suspended condition True")
	case suspended.ObservedGeneration < w.minGeneration:
		return waitingFor("the operator to report Suspended for generation %d (the Suspended condition is for "+
			"generation %d)", w.minGeneration, suspended.ObservedGeneration)
	}
	return verdict{done: true}
}

// The phases this file compares by name. Both families spell them alike.
const (
	phaseInSync           = string(operatorv1alpha1.PhaseInSync)
	phaseAwaitingApproval = string(operatorv1alpha1.PhaseAwaitingApproval)
	phaseFailed           = string(operatorv1alpha1.PhaseFailed)
	phaseSuspended        = string(operatorv1alpha1.PhaseSuspended)
)

// inFlight names what the resource is doing: the operation in flight, the
// proof an Apply still owes, or only its phase.
func inFlight(s snapshot) string {
	switch {
	case s.operation != "":
		return fmt.Sprintf("the %s operation (phase %s)", s.operation, s.phase)
	case s.proof:
		return fmt.Sprintf("the post-Apply proof (phase %s)", s.phase)
	default:
		return "phase " + s.phase
	}
}

func instant(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return at.UTC().Format(time.RFC3339)
}
