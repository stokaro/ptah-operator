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
	"k8s.io/apimachinery/pkg/types"
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
	// approval admission requires of its status. With a baseline, its plan has
	// to come from a read of the database that began after that baseline. With
	// none, the gate as the wait finds it counts. Admission also checks the plan
	// object and its bindings, which a wait does not read.
	TargetApproval Target = "approval"
	// TargetApplied is an Apply or a migration run that ended after the
	// baseline, recorded applied and settled by the operator's own reading of
	// the database. One that ended before it does not count even when its
	// proof or read-back ends after it. Any other ending of an Apply that
	// ended after the baseline stops the wait with a StopError. WaitOptions.Plan
	// and WaitOptions.Decision narrow it to one plan and one approval.
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
	// baseline, which can miss a change that completes before it, except for
	// TargetApproval: a person who starts that wait asks whether a plan waits
	// for them, and a first read that shows one answers it.
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
	// Plan narrows TargetApplied to the plan of that name or fingerprint.
	// Another plan applied first stops the wait.
	Plan string
	// Decision narrows TargetApplied, for a PtahMigration, to the run that carried
	// out one approval of one plan. The run has to name both, and a run that
	// names either differently, a plan that replaces this one, or a run that
	// names neither stops the wait: nothing else can say this plan completed.
	Decision *Decision
	// Source narrows TargetInSync and TargetApproval to the OCI reference
	// SetSource wrote, pinned by digest. A document counts only while the
	// spec still asks for it, and only once the operator worked from it: a
	// schema waiting for approval with a plan computed from its digest, and
	// otherwise the artifact the operator resolved. A spec that names another
	// source stops the wait.
	Source string
}

// Decision is one approval of one plan, as a person reviewed it and admission
// stored it.
type Decision struct {
	PlanName     string
	PlanUID      types.UID
	ApprovalName string
	ApprovalUID  types.UID
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
	// Run is the run a wait narrowed to a plan ended on, which a later run may
	// have replaced in Object's status.lastRun. It is nil for any other wait.
	Run *RunReached
}

// RunReached is a migration run that met a wait for its plan.
type RunReached struct {
	Outcome operatorv1alpha1.MigrationRunOutcome
	JobName string
	// Replaced reports that a later run has taken its place in status.lastRun,
	// and that only the journal still names it.
	Replaced bool
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
	// StopOtherPlanApplied is a resource that applied another plan than the
	// one the wait named.
	StopOtherPlanApplied StopReason = "OtherPlanApplied"
	// StopOtherApproval is a migration that ran the plan the wait named under
	// another approval than the one the wait named.
	StopOtherApproval StopReason = "OtherApprovalConsumed"
	// StopPlanSuperseded is a migration whose named plan will not run: another
	// plan took its place, or a reading found nothing left to apply.
	StopPlanSuperseded StopReason = "PlanSuperseded"
	// StopNotProved is a run that may be the one the wait named, or that was and
	// has no reading to show for it, with no record that says more: one that names
	// no plan, one that names this plan and not what allowed it or not the
	// fingerprint the wait named, one whose reading afterwards is no longer
	// recorded, or one whose database moved before the next reading.
	StopNotProved StopReason = "CompletionNotProved"
	// StopNothingApplied is a migration run that found nothing to apply.
	StopNothingApplied StopReason = "NothingApplied"
	// StopNotSuspended is a wait for suspension on a resource whose spec
	// does not ask for it.
	StopNotSuspended StopReason = "NotSuspended"
	// StopPolicyBlocked is a resource whose current plan its own policy
	// holds: a schema plan that is destructive while
	// spec.policy.allowDestructive is false, or any plan of either family
	// under spec.policy.apply Never. No approval is asked for and no Apply
	// runs, and every later reading of the same database plans the same,
	// until the spec changes.
	StopPolicyBlocked StopReason = "PolicyBlocked"
	// StopDeleting is a resource that is being deleted.
	StopDeleting StopReason = "Deleting"
	// StopDeleted is a resource that was deleted while the wait read it.
	StopDeleted StopReason = "Deleted"
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
	// retried. A read the wait's own deadline cut short is not counted: the
	// deadline is the timeout this error reports, not news about the
	// resource. When no read answered at all, LastError says so.
	LastError error
	// ReadUnderWay reports that the last document read showed a read of the
	// database in flight. A wait for TargetInSync begun after it skips that
	// read's harvest, because it may have read the database before the wait
	// began; a read request is what asks for one that begins later.
	ReadUnderWay bool
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
	if e.ReadUnderWay {
		message.WriteString("; a read of the database was under way")
	}
	return message.String()
}

// Wait reads the resource until it reaches options.Target, the timeout
// passes, or the resource records an outcome the target cannot follow.
//
// It returns a *StopError for such an outcome or a resource being deleted or
// deleted since the baseline, a *TimeoutError on timeout, ErrReplaced when the
// name comes to hold another resource, and the API server's error for a
// resource that was not there to begin with, a kind the server does not
// serve, or a read it refuses. A failed read the server may answer next time
// is retried until the timeout.
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
					Ref: ref, Target: verdict.target(options.Target), Phase: current.phase,
					Generation: current.generation, ObservedGeneration: current.observedGeneration,
					ObservedAt: current.observedAt, Reads: reads, Object: current.object, Run: verdict.run,
				}, nil
			}
			waiting = verdict.waiting
		case ctx.Err() != nil:
			return Result{}, fmt.Errorf("wait for %s: %w", ref, ctx.Err())
		case errors.Is(err, context.DeadlineExceeded) && !clock.Now().Before(deadline):
			// The read carried the wait's own deadline and ran into it, which
			// the read at the deadline always does. The timeout below reports
			// what the last read that answered found, or why the last one
			// before this failed.
			if last == nil && lastErr == nil {
				lastErr = errNoAnswer
			}
		case apierrors.IsNotFound(err) && w.baseline != nil:
			// The resource the wait began on is gone; a resource with no
			// finalizer goes without a deleting document between.
			phase := w.baseline.phase
			if last != nil {
				phase = last.phase
			}
			return Result{}, &StopError{Ref: ref, Target: options.Target, Reason: StopDeleted, Phase: phase,
				Message: "the resource was deleted"}
		case !Retryable(err):
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
				timeout.ReadUnderWay = last.readingDatabase()
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

// errNoAnswer is a timeout before which no read was answered: each read ran
// into the wait's deadline, which says nothing about the resource.
var errNoAnswer = errors.New("the API server answered no read before the timeout")

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

// Retryable reports a failed read the API server may answer next time: a
// transport failure, a timeout, throttling or a server error. A kind the
// server does not serve, or one this client cannot encode, is not: the CRD is
// not installed, and waiting does not install it. Nor is a refusal, such as
// RBAC's. Wait retries what it reports until its timeout, and so does
// kubectl ptah approve while it waits for the approval gate.
func Retryable(err error) bool {
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
	// reached is the target a done verdict met, where the wait's own target
	// names more than one (TargetPlanned).
	reached Target
	// run is the run a done verdict of a wait narrowed to a plan ended on.
	run *RunReached
}

func (v verdict) target(asked Target) Target {
	if v.reached != "" {
		return v.reached
	}
	return asked
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
	sourceDigest  string
	minGeneration int64
	baseline      *snapshot
	fresh         freshness
	// gateAsFound ends a wait for an approval on the gate as it stands, whichever
	// reading its plan came from. Such a wait has no baseline to date that reading
	// by, and an interval longer than its timeout would otherwise leave it waiting
	// beside a plan that already waits. A plan replaced meanwhile is answered by
	// kubectl ptah approve, which holds a decision to the plan it showed.
	gateAsFound bool

	// The read target's state. When the baseline does not rest, the floor
	// and failureFloor are raised to the document in which the operator
	// takes the request, which it does only once the cycle under way is over:
	// that cycle's reading and its failures belong to no read the request
	// asked for.
	answered      bool
	floorAtAnswer bool
	failureFloor  failureMark

	// earlier is the plan whose post-Apply proof the baseline owed, until
	// that proof ends: its Apply ended before the wait began. The
	// status.applied that proof writes, and the journal entries for the plan
	// up to the proof's Verified entry, neither count nor stop the wait.
	// The plan store reuses a byte-identical plan, so the same plan applied
	// again later in the wait is another Apply, and counts.
	earlier        *planRecord
	earlierApplied bool
}

func newWaiter(ref Ref, options WaitOptions) (*waiter, error) {
	target, err := ParseTarget(string(options.Target))
	if options.Target == TargetPlanned && options.Source != "" {
		// Not a target a command line names: the end of a change of source.
		target, err = TargetPlanned, nil
	}
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
	w := &waiter{ref: ref, options: options, gateAsFound: options.Target == TargetApproval && options.Since == nil}
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
	if options.Plan != "" && options.Target != TargetApplied {
		return nil, fmt.Errorf("a plan narrows only a wait for target %s", TargetApplied)
	}
	if decision := options.Decision; decision != nil {
		switch {
		case options.Target != TargetApplied || ref.Family != FamilyMigration:
			return nil, fmt.Errorf("a decision narrows only a wait for a PtahMigration to reach target %s", TargetApplied)
		case options.Plan != "":
			return nil, errors.New("a decision already names its plan")
		case decision.PlanUID == "":
			return nil, errors.New("a decision has to name its plan by UID")
		}
	}
	if options.Source != "" {
		if options.Target != TargetInSync && options.Target != TargetApproval && options.Target != TargetPlanned {
			return nil, fmt.Errorf("a source narrows only a wait for %s or %s", TargetInSync, TargetApproval)
		}
		pinned, err := ParsePinnedReference(options.Source)
		if err != nil {
			return nil, err
		}
		w.sourceDigest = pinned.Digest
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
		case TargetInSync, TargetApproval, TargetSuspended, TargetPlanned:
			w.minGeneration = baseline.generation
		}
	}
	w.fresh = newFreshness(baseline, w.minGeneration)
	w.failureFloor = baseline.failureMark()
	if baseline.proof && baseline.proofPlan.name != "" {
		earlier := baseline.proofPlan
		w.earlier = &earlier
	}
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
	if w.options.Source == "" {
		return w.checkTarget(s)
	}
	if stop, moved := w.sourceMoved(s); moved {
		return stop
	}
	return w.checkSource(s)
}

func (w *waiter) checkTarget(s snapshot) verdict {
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
				"the resource is not suspended, no retry delay is pending, and neither its engine, its "+
				"database realm nor a proof whose target moved blocks it", w.options.ReadRequest)
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
	if held, ok := w.policyHeld(s, fresh); ok {
		return held
	}
	return w.inSync(s, fresh)
}

// policyHeld stops a wait on a resource whose current plan its own policy
// holds (StopPolicyBlocked), at the generation the resource has now and
// beside a plan from a reading the target may count. None of in-sync,
// approval and applied follows such a plan.
//
// For a schema the refusal is ApprovalRequired False for
// DestructiveChangesDisabled or ApplyDisabled. The operator writes it with the
// plan (setPlanPolicyStatus in internal/controller) and leaves it alone while
// it resolves, verifies and reads the database again at its interval, while
// the phase leaves Blocked and Ready reads OperationInProgress. The plan beside
// it dates it: the Observe harvest clears status.plan in the same write that
// records the reading, and only the plan computed after it puts one back, so a
// refusal beside the reading of a cycle that has not planned yet has no plan
// beside it.
//
// For a migration it is Ready False for ApplyDisabled, which publishing a plan
// under apply policy Never writes (applyMigrationPolicyPhase). A claim writes
// the phase and Progressing and leaves Ready alone, and the harvest of the
// history writes Ready in the same write that records the reading, on every
// branch. So ApplyDisabled beside a reading was written by the publication
// that followed that reading. A migration keeps the previous plan through a
// read, so the plan dates nothing there.
//
// A database realm that blocks the resource does not stop the wait: another
// resource's change or the realm's grant ends that refusal, not this
// resource's spec, and the timeout names it.
func (w *waiter) policyHeld(s snapshot, fresh bool) (verdict, bool) {
	if s.plan == nil || !fresh {
		return verdict{}, false
	}
	held := s.policyHold()
	// The generation is the resource's own, which is never below the one the
	// wait requires: a spec edited since the refusal may be the edit that
	// allows the plan. It also holds a wait for a source to a plan from that
	// source, since sourceMoved has stopped one whose spec names another.
	if held == nil || held.ObservedGeneration < s.generation {
		return verdict{}, false
	}
	return w.stop(s, StopPolicyBlocked, fmt.Sprintf("spec.policy holds plan %s (%s): %s",
		s.plan.name, held.Reason, held.Message)), true
}

// policyHold is the condition that says the resource's own policy holds its
// current plan, or nil.
func (s snapshot) policyHold() *metav1.Condition {
	switch s.family {
	case FamilySchema:
		held := s.condition(operatorv1alpha1.ConditionApprovalRequired)
		if held != nil && held.Status == metav1.ConditionFalse {
			switch operatorv1alpha1.ConditionReason(held.Reason) {
			case operatorv1alpha1.ReasonDestructiveChangesDisabled, operatorv1alpha1.ReasonApplyDisabled:
				return held
			}
		}
	case FamilyMigration:
		held := s.condition(operatorv1alpha1.ConditionMigrationReady)
		if held != nil && held.Status == metav1.ConditionFalse &&
			held.Reason == string(operatorv1alpha1.ReasonApplyDisabled) {
			return held
		}
	}
	return nil
}

// inSync decides TargetInSync on a document whose reading fresh says counts
// or not.
func (w *waiter) inSync(s snapshot, fresh bool) verdict {
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
	if held, ok := w.policyHeld(s, fresh); ok {
		return held
	}
	return w.approval(s, fresh)
}

// approval decides TargetApproval on a document whose reading fresh says
// counts or not.
func (w *waiter) approval(s snapshot, fresh bool) verdict {
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
	// than the baseline, under the same name and UID or a new one, is not news.
	if !fresh && !w.gateAsFound {
		return w.staleReading(s)
	}
	return verdict{done: true}
}

func (w *waiter) checkApplied(s snapshot) verdict {
	// Every document passes through the freshness bookkeeping in order, as
	// it does for the other targets, though only a policy refusal below
	// asks it.
	fresh := w.fresh.see(s)
	if s.family == FamilyMigration {
		run := w.checkRun(s)
		if run.done || run.stop != nil {
			return run
		}
		if held, ok := w.policyHeld(s, fresh); ok {
			return held
		}
		return run
	}
	b := w.baseline
	// status.applied is written only by the proof that read the database
	// after the Apply and found it converged.
	if applied := s.applied; applied != nil && (b.applied == nil || applied.planUID != b.applied.planUID ||
		!applied.completedAt.Equal(b.applied.completedAt)) {
		if w.earlier != nil && !w.earlierApplied && applied.planUID == w.earlier.uid {
			// The proof the baseline owed credited its Apply, which ended
			// before the wait began, as a migration run the baseline already
			// recorded did. This record is the baseline from here on.
			credited := *applied
			b.applied, w.earlierApplied = &credited, true
			return waitingFor("an Apply that ends after the wait began: plan %s was applied before it, and its "+
				"proof ended since", applied.plan)
		}
		if plan := w.options.Plan; plan != "" && applied.plan != plan && applied.fingerprint != plan {
			return w.stop(s, StopOtherPlanApplied, fmt.Sprintf("plan %s was applied, not %s", applied.plan, plan))
		}
		if v, ok := w.generationReached(s); !ok {
			return v
		}
		return verdict{done: true}
	}
	// A schema journals an Apply's end as Succeeded or Unknown and nothing
	// else (internal/lifecyclejournal); a failure it can see is a proof that
	// did not confirm the Apply.
	for _, entry := range journalSince(b.journal, s.journal) {
		if w.earlier != nil && entry.Plan == w.earlier.name {
			if entry.Type == operatorv1alpha1.JournalVerified {
				// The proof the baseline owed ended here. What follows, the
				// same plan applied again included, is the wait's.
				b.journal, w.earlier = journalThrough(s.journal, entry), nil
			}
			continue
		}
		switch {
		case entry.Type == operatorv1alpha1.JournalApplyFinished && entry.Outcome == operatorv1alpha1.JournalUnknown:
			return w.stop(s, StopUnresolved, fmt.Sprintf("the Apply of plan %s ended with an outcome nobody established; "+
				"the operator proves convergence before anything else runs, and records no Apply for it", entry.Plan))
		case entry.Type == operatorv1alpha1.JournalVerified && entry.Outcome == operatorv1alpha1.JournalDriftRemains:
			return w.stop(s, StopNotConverged, fmt.Sprintf("after the Apply of plan %s the database still differs "+
				"from the desired schema", entry.Plan))
		case entry.Type == operatorv1alpha1.JournalVerified && entry.Outcome == operatorv1alpha1.JournalSuperseded:
			return w.stop(s, StopNotConverged, fmt.Sprintf("the proof after the Apply of plan %s settled under a "+
				"newer generation, policy or execution binding", entry.Plan))
		case entry.Type == operatorv1alpha1.JournalVerified && entry.Outcome == operatorv1alpha1.JournalUnknown:
			return w.stop(s, StopUnresolved, fmt.Sprintf("a person gave up the proof after the Apply of plan %s, "+
				"and no Apply is recorded for it", entry.Plan))
		}
	}
	if held, ok := w.policyHeld(s, fresh); ok {
		return held
	}
	// An Apply whose dispatch the operator refused keeps its claim and is
	// tried again, so that failure leaves the wait running and its timeout
	// names it. A schema gives an Apply claim up only when its outcome is
	// unknown, which ApplyFinished records above.
	return waitingFor("an Apply that ends after the wait began; now %s", inFlight(s))
}

func (w *waiter) checkRun(s snapshot) verdict {
	if w.options.Decision != nil || w.options.Plan != "" {
		return w.checkBoundRun(s)
	}
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
		default:
			return w.runDidNotApply(s, run)
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

// runDidNotApply stops a wait on a run that ended any way but Applied.
func (w *waiter) runDidNotApply(s snapshot, run *runRecord) verdict {
	switch run.outcome {
	case operatorv1alpha1.MigrationRunOutcomeUpToDate:
		return w.stop(s, StopNothingApplied, fmt.Sprintf("the run (Job %s) found nothing to apply", run.jobName))
	case operatorv1alpha1.MigrationRunOutcomePartial, operatorv1alpha1.MigrationRunOutcomeUnknown:
		return w.stop(s, StopUnresolved, fmt.Sprintf("the run (Job %s) ended %s: %s", run.jobName, run.outcome, run.message))
	default:
		return w.stop(s, StopApplyFailed, fmt.Sprintf("the run (Job %s) ended %s: %s", run.jobName, run.outcome, run.message))
	}
}

// runBinding is how a run's record stands to the plan and approval a wait names.
type runBinding int

const (
	// runIsBound is the run the wait names.
	runIsBound runBinding = iota
	// runOfAnotherPlan carried out another plan.
	runOfAnotherPlan
	// runOfAnotherApproval carried out the plan under another approval, or under
	// the resource's policy.
	runOfAnotherApproval
	// runUnidentified does not hold what the wait would have to compare, so nothing
	// can say which it was.
	runUnidentified
)

// bindingOf compares what a record says with what the wait names. A byte-identical
// plan is reused, so the plan alone does not tell two executions apart, and the
// baseline and the approval's UID do.
func (w *waiter) bindingOf(identity runIdentity) runBinding {
	if identity.planUID == "" && identity.plan == "" {
		return runUnidentified
	}
	decision := w.options.Decision
	if decision == nil {
		plan := w.options.Plan
		if identity.plan == plan || identity.fingerprint == plan {
			return runIsBound
		}
		// A plan's name holds no colon, so this is a fingerprint, which a claim from
		// before the upgrade did not copy. Its name says nothing about it.
		if identity.fingerprint == "" && strings.HasPrefix(plan, "sha256:") {
			return runUnidentified
		}
		return runOfAnotherPlan
	}
	if identity.planUID == "" {
		// An entry from before the plan's UID was recorded, which names it only.
		if identity.plan == decision.PlanName {
			return runUnidentified
		}
		return runOfAnotherPlan
	}
	if identity.planUID != decision.PlanUID {
		return runOfAnotherPlan
	}
	if decision.ApprovalUID != "" && identity.approvalUID != decision.ApprovalUID {
		// Only a record that says Policy says no approval allowed the run. One that
		// says nothing, as the entry of a hold a restore brought back does, may be
		// this decision.
		if identity.approvalUID == "" && identity.authorization != operatorv1alpha1.MigrationRunAuthorizedByPolicy {
			return runUnidentified
		}
		return runOfAnotherApproval
	}
	return runIsBound
}

// boundPlan names the plan the wait is bound to, for a message.
func (w *waiter) boundPlan() string {
	if decision := w.options.Decision; decision != nil {
		return decision.PlanName
	}
	return w.options.Plan
}

// checkBoundRun decides TargetApplied for a migration whose wait names a plan,
// and perhaps an approval.
//
// The run is found by what its record says and by nothing else, not by the order
// of events and not by when one happened: first status.lastRun, then, since a
// later run may have replaced that between two reads, the journal. Only once
// neither holds this run does anything else that happened say it will not come.
func (w *waiter) checkBoundRun(s snapshot) verdict {
	b := w.baseline
	since := journalSince(b.journal, s.journal)
	run := s.lastRun
	finished := run != nil && run.finished && newRun(b.lastRun, run)
	if finished && w.bindingOf(run.identity) == runIsBound {
		return w.boundRunEnded(s, run)
	}
	var ended []operatorv1alpha1.JournalEntry
	for _, entry := range since {
		if entry.Type != operatorv1alpha1.JournalRunFinished {
			continue
		}
		if w.bindingOf(identityOfEntry(entry)) == runIsBound {
			return w.boundRunJournaled(s, entry)
		}
		ended = append(ended, entry)
	}
	if finished {
		return w.anotherRunEnded(s, run.identity, fmt.Sprintf("the run (Job %s) ended %s", run.jobName, run.outcome))
	}
	// Before the journal's other ends: the write that brings a hold back with no
	// run beside it also writes the hold's entry, and the hold says more.
	if unresolved := s.unresolved; unresolved != nil &&
		(b.unresolved == nil || b.unresolved.operationID != unresolved.operationID) {
		identity := runIdentity{plan: unresolved.plan, planUID: unresolved.planUID}
		if w.bindingOf(identity) == runOfAnotherPlan {
			return w.stop(s, StopPlanSuperseded, fmt.Sprintf("plan %s ran and ended %s, and plan %s did not run",
				unresolved.plan, unresolved.outcome, w.boundPlan()))
		}
		return w.stop(s, StopUnresolved, fmt.Sprintf("the run of plan %s (Job %s) ended %s: its effect on the "+
			"database is not established, and nothing runs until a reading or a person accounts for it",
			unresolved.plan, unresolved.jobName, unresolved.outcome))
	}
	if len(ended) > 0 {
		entry := ended[0]
		return w.anotherRunEnded(s, identityOfEntry(entry), fmt.Sprintf("a run ended %s", entry.Outcome))
	}
	if stop, superseded := w.planSuperseded(s); superseded {
		return stop
	}
	for _, entry := range since {
		if entry.Type == operatorv1alpha1.JournalOperationFailed &&
			entry.Operation == string(operatorv1alpha1.MigrationOperationApply) && s.operation == "" {
			return w.stop(s, StopApplyFailed, w.failureMessage(s, entry))
		}
	}
	return waitingFor("the run of plan %s; now %s", w.boundPlan(), inFlight(s))
}

// boundRunEnded judges the run the wait names, from status.lastRun.
func (w *waiter) boundRunEnded(s snapshot, run *runRecord) verdict {
	if run.outcome != operatorv1alpha1.MigrationRunOutcomeApplied {
		return w.runDidNotApply(s, run)
	}
	if !run.verified {
		// A reading records itself on the run in the status write that records the
		// reading, so a later one with nothing recorded was of another database, and
		// only that database's next reading is left to wait for, if it ever comes.
		if s.observedAt.After(run.finishedAt) {
			return w.stop(s, StopNotProved, fmt.Sprintf("run %s recorded its migrations applied, and a later reading "+
				"did not verify it: it read another database, so no record says what the run's database holds", run.jobName))
		}
		return waitingFor("the history read back after run %s; now %s", run.jobName, inFlight(s))
	}
	if run.verification == operatorv1alpha1.MigrationRunVerifiedBlocked {
		message := "the history read back afterwards was blocked"
		if blocker := s.blocker(); blocker != nil {
			message = fmt.Sprintf("the history read back afterwards was blocked; the resource now reads %s: %s",
				blocker.Reason, blocker.Message)
		}
		return w.stop(s, StopNotConverged, fmt.Sprintf("run %s recorded its migrations applied, and %s", run.jobName, message))
	}
	if v, ok := w.generationReached(s); !ok {
		return v
	}
	return verdict{done: true, run: &RunReached{Outcome: run.outcome, JobName: run.jobName}}
}

// boundRunJournaled judges the run the wait names from its journal entry, once a
// later run has replaced it in status.lastRun. The reading after it is the entry
// with its operation ID.
func (w *waiter) boundRunJournaled(s snapshot, ended operatorv1alpha1.JournalEntry) verdict {
	switch ended.Outcome {
	case operatorv1alpha1.JournalSucceeded:
	case operatorv1alpha1.JournalUpToDate:
		return w.stop(s, StopNothingApplied, fmt.Sprintf("the run of plan %s found nothing to apply", ended.Plan))
	case operatorv1alpha1.JournalPartial, operatorv1alpha1.JournalUnknown:
		return w.stop(s, StopUnresolved, fmt.Sprintf("the run of plan %s ended %s", ended.Plan, ended.Outcome))
	default:
		return w.stop(s, StopApplyFailed, fmt.Sprintf("the run of plan %s ended %s", ended.Plan, ended.Outcome))
	}
	if s.journal != nil && ended.OperationID != "" {
		for _, entry := range s.journal.Entries {
			if entry.Type != operatorv1alpha1.JournalVerified || entry.OperationID != ended.OperationID {
				continue
			}
			if entry.Outcome == operatorv1alpha1.JournalBlocked {
				return w.stop(s, StopNotConverged, fmt.Sprintf("the run of plan %s recorded its migrations applied, and "+
					"the history read back afterwards was blocked", ended.Plan))
			}
			if v, ok := w.generationReached(s); !ok {
				return v
			}
			// Only an entry that says Succeeded comes this far.
			return verdict{done: true, run: &RunReached{
				Outcome: operatorv1alpha1.MigrationRunOutcomeApplied, JobName: ended.JobName, Replaced: true,
			}}
		}
	}
	return w.stop(s, StopNotProved, fmt.Sprintf("the run of plan %s recorded its migrations applied, a later run has "+
		"replaced it in status.lastRun, and the journal holds no reading of the history after it", ended.Plan))
}

// anotherRunEnded stops a wait on a run that is not the one it names.
func (w *waiter) anotherRunEnded(s snapshot, identity runIdentity, ended string) verdict {
	plan := w.boundPlan()
	switch w.bindingOf(identity) {
	case runUnidentified:
		if identity.plan == "" && identity.planUID == "" {
			return w.stop(s, StopNotProved, fmt.Sprintf("%s and its record names no plan, so nothing says whether it was "+
				"plan %s; an operator that predates run provenance writes such a record", ended, plan))
		}
		if decision := w.options.Decision; decision != nil {
			return w.stop(s, StopNotProved, fmt.Sprintf("%s and its record names plan %s and no more than that, so nothing "+
				"says whether approval %s allowed it", ended, identity.plan, decision.ApprovalName))
		}
		return w.stop(s, StopNotProved, fmt.Sprintf("%s and its record names plan %s and no fingerprint, so nothing says "+
			"whether it was %s; a claim from before the upgrade did not copy one", ended, identity.plan, plan))
	case runOfAnotherApproval:
		decision := w.options.Decision
		under := "under no approval"
		if identity.approval != "" {
			under = "under approval " + identity.approval
		}
		return w.stop(s, StopOtherApproval, fmt.Sprintf("plan %s ran %s, not under approval %s: %s", plan, under, decision.ApprovalName, ended))
	default:
		name := identity.plan
		if name == "" {
			name = string(identity.planUID)
		}
		return w.stop(s, StopOtherPlanApplied, fmt.Sprintf("plan %s ran, not %s: %s", name, plan, ended))
	}
}

// planSuperseded reports that the plan a decision names will not run, by what the
// status says now: another plan claimed or published in its place, or a reading
// that left nothing to apply. A resource that is only blocked, or that has cleared
// its plan while it refuses something else, may come back to the same plan, so it
// stops nothing and the timeout names it.
func (w *waiter) planSuperseded(s snapshot) (verdict, bool) {
	decision := w.options.Decision
	if decision == nil {
		return verdict{}, false
	}
	if s.operation == string(operatorv1alpha1.MigrationOperationApply) && s.operationPlan.uid != "" {
		if s.operationPlan.uid == decision.PlanUID {
			return verdict{}, false
		}
		return w.stop(s, StopPlanSuperseded, fmt.Sprintf("plan %s is running in place of plan %s, so approval %s "+
			"authorizes nothing", s.operationPlan.name, decision.PlanName, decision.ApprovalName)), true
	}
	if s.plan != nil && s.plan.uid != decision.PlanUID {
		return w.stop(s, StopPlanSuperseded, fmt.Sprintf("plan %s replaced plan %s before it ran, so approval %s "+
			"authorizes nothing", s.plan.name, decision.PlanName, decision.ApprovalName)), true
	}
	if s.plan == nil && s.operation == "" && s.phase == phaseInSync {
		return w.stop(s, StopPlanSuperseded, fmt.Sprintf("the database has nothing left to apply and plan %s did not "+
			"run, so approval %s authorizes nothing", decision.PlanName, decision.ApprovalName)), true
	}
	return verdict{}, false
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
