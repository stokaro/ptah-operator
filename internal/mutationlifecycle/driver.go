package mutationlifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A claim reaches its one permitted create through a fixed sequence of durable
// writes, and the order of those writes is what makes a process that stops
// anywhere in it safe to start again. Both families ran the sequence, in two
// copies that disagreed: one consumed the approval and then marked the dispatch
// boundary, the other marked the boundary and consumed the approval after it.
// The order is written down once, here, and each family supplies only what
// differs: how it authorizes a claim, what its Job is, and where it keeps its
// status.

// Step is one boundary of the sequence.
type Step int

const (
	// StepAuthorize re-reads everything the claim was decided from, and
	// retires or fails the claim when any of it moved. It writes nothing
	// otherwise.
	StepAuthorize Step = iota + 1
	// StepLease takes or renews the database realm Lease under the claim's own
	// epoch, for a claim that holds the realm.
	StepLease
	// StepStage writes what the Job reads that is not the Job itself: a schema
	// Apply's plan projection. A family with nothing to stage proceeds.
	StepStage
	// StepSnapshot makes the Pod admission snapshot durable. The pass that
	// writes it ends there, so no Job carries a digest the claim does not hold.
	StepSnapshot
	// StepConsume spends the approval that authorized a mutating claim.
	StepConsume
	// StepMark records that a Job may exist, for a claim that holds the realm.
	StepMark
	// StepCreate is the one create the claim is permitted.
	StepCreate
	// StepConfirm reads the created Job back and holds it to the claim.
	StepConfirm
	// StepRecord persists the confirmed Job's UID.
	StepRecord
)

var stepNames = map[Step]string{
	StepAuthorize: "authorize",
	StepLease:     "lease",
	StepStage:     "stage",
	StepSnapshot:  "snapshot",
	StepConsume:   "consume",
	StepMark:      "mark",
	StepCreate:    "create",
	StepConfirm:   "confirm",
	StepRecord:    "record",
}

func (s Step) String() string {
	if name, ok := stepNames[s]; ok {
		return name
	}
	return fmt.Sprintf("step(%d)", int(s))
}

// order is the sequence, and each position has a reason:
//
//   - Authorize comes first, so a claim whose authorization moved is retired
//     without waiting for a Lease another claimant holds, and without taking
//     the realm for work that will not run.
//   - Lease, stage, snapshot and consume come before mark. The mark is the
//     statement that a Job may exist, so everything a Job depends on -- the
//     realm held under the claim's epoch, the plan it mounts, the admission
//     envelope it is judged by, the approval it spends -- is durable before
//     it, and every state in which a Job may exist carries the whole account
//     of what authorized it.
//   - Consume before mark, specifically. The mark is what turns a missing Job
//     into an outcome nobody established, so nothing that can fail may sit
//     between it and the create, and spending the approval is a write that
//     can. A process that stops between the two leaves a spent approval and no
//     mark; the same claim dispatches on the next pass under the approval it
//     already spent, and a claim retired instead leaves the approval spent,
//     which asks a person to approve again and authorizes nothing.
//   - Mark before create, so the one create is always covered by the mark.
//   - Confirm before record, so a Job's UID is recorded only once the Job was
//     read back and held to the claim.
var order = []Step{
	StepAuthorize,
	StepLease,
	StepStage,
	StepSnapshot,
	StepConsume,
	StepMark,
	StepCreate,
	StepConfirm,
	StepRecord,
}

// Order returns the sequence a zero Driver runs.
func Order() []Step {
	return slices.Clone(order)
}

// Outcome is what a step says about the rest of the pass.
type Outcome struct {
	stop   bool
	result reconcile.Result
	err    error
}

// Proceed lets the pass go on to the next step.
func Proceed() Outcome { return Outcome{} }

// Stop ends the pass with this result.
func Stop(result reconcile.Result, err error) Outcome {
	return Outcome{stop: true, result: result, err: err}
}

// Claim is what the driver reads about the claim at each step. A step that
// writes status changes what the family reports next.
type Claim struct {
	// Type names the operation, for the messages the driver writes.
	Type string
	// Mutating says the claim may change the database.
	Mutating bool
	// HoldsLock says the claim holds the database realm while it runs.
	HoldsLock bool
	// Dispatch is what the claim records about whether its Job may exist.
	Dispatch DispatchState
	// Snapshot is the claim's admission snapshot, and SnapshotRefreshed says
	// it was already resolved again once for a template that moved.
	Snapshot          *operatorv1alpha1.PodAdmissionSnapshot
	SnapshotRefreshed bool
}

// Refusal is why a claim's Job cannot be judged against its admission
// envelope.
type Refusal int

const (
	// RefusalSnapshotInvalid: the persisted snapshot does not validate.
	RefusalSnapshotInvalid Refusal = iota + 1
	// RefusalTemplateUnreadable: the rebuilt Pod template cannot be digested.
	RefusalTemplateUnreadable
	// RefusalTemplateMoved: the rebuilt template differs from a snapshot that
	// was already resolved again for it once.
	RefusalTemplateMoved
	// RefusalSnapshotUnresolvable: the envelope cannot be resolved.
	RefusalSnapshotUnresolvable
)

// Admission resolves and checks a Pod template's admission envelope. It is
// podintent seen through the reader and options a reconciler holds; this
// package does not import podintent, because the Job builder that package
// judges depends on this one.
type Admission interface {
	Resolve(ctx context.Context, template *corev1.PodTemplateSpec) (*operatorv1alpha1.PodAdmissionSnapshot, error)
	Validate(snapshot *operatorv1alpha1.PodAdmissionSnapshot) error
	Digest(template *corev1.PodTemplateSpec) (string, error)
}

// Family is one resource family's side of the sequence.
type Family interface {
	// Claim describes the claim as it stands now.
	Claim() Claim
	// Authorize re-reads the claim's authorizations, and refuses it -- by
	// retiring or failing it -- when any of them moved.
	Authorize(ctx context.Context) Outcome
	// AcquireLease takes or renews the realm Lease under the claim's epoch.
	AcquireLease(ctx context.Context) Outcome
	// Stage writes what the Job reads besides the Job.
	Stage(ctx context.Context) Outcome
	// Build returns the Job the claim names, or refuses the claim.
	Build(ctx context.Context) (*batchv1.Job, Outcome)
	// Admission resolves and checks the envelope of the claim's Pod.
	Admission() Admission
	// SaveSnapshot persists the admission snapshot resolved for the claim.
	SaveSnapshot(ctx context.Context, snapshot *operatorv1alpha1.PodAdmissionSnapshot) (reconcile.Result, error)
	// RefreshSnapshot drops a snapshot whose template moved and records that
	// it was resolved again once.
	RefreshSnapshot(ctx context.Context) (reconcile.Result, error)
	// Refuse ends a claim whose Job cannot be judged against its envelope.
	Refuse(ctx context.Context, refusal Refusal, failure error) (reconcile.Result, error)
	// Consume spends the approval the claim was authorized by, and refuses
	// the claim when that approval no longer holds.
	Consume(ctx context.Context) Outcome
	// Mark persists the dispatch marker, with whatever the family records at
	// the same boundary.
	Mark(ctx context.Context) error
	// Create creates the Job.
	Create(ctx context.Context, job *batchv1.Job) error
	// Confirm reads the created Job back from the API server.
	Confirm(ctx context.Context, job *batchv1.Job) (*batchv1.Job, error)
	// Intent holds a Job read back to the Job the claim builds.
	Intent(actual, expected *batchv1.Job) error
	// Record persists the confirmed Job's UID and reports the start.
	Record(ctx context.Context, job *batchv1.Job) (reconcile.Result, error)
	// Unaccounted retires a mutating claim whose Job may exist as an outcome
	// nobody established; job is the Job in hand, or nil.
	Unaccounted(ctx context.Context, job *batchv1.Job, failure error) (reconcile.Result, error)
	// Retry moves a read-only claim to a fresh attempt.
	Retry(ctx context.Context, failure error) (reconcile.Result, error)
}

// Driver runs the sequence. Its zero value runs Order.
type Driver struct {
	steps []Step
}

// Swapped returns a driver that runs a and b in each other's place. It exists
// for the tests that hold each boundary to its reason: a test that passes
// against the order and against the order with its boundary moved measures
// nothing about the boundary. Production code runs the zero Driver.
func (d Driver) Swapped(a, b Step) Driver {
	steps := d.sequence()
	i, j := slices.Index(steps, a), slices.Index(steps, b)
	if i >= 0 && j >= 0 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	return Driver{steps: steps}
}

func (d Driver) sequence() []Step {
	if len(d.steps) == 0 {
		return Order()
	}
	return slices.Clone(d.steps)
}

// ErrMayHaveDispatched refuses to dispatch a mutating claim whose Job may
// already exist. Such a claim is an outcome nobody established, and a second
// create would start a second executor beside one that may be running SQL.
var ErrMayHaveDispatched = errors.New("a mutating claim that may already have dispatched is never dispatched again")

// Dispatch runs one pass of a claim whose Job does not exist toward its one
// permitted create.
func (d Driver) Dispatch(ctx context.Context, family Family) (reconcile.Result, error) {
	if claim := family.Claim(); claim.Mutating && MayHaveDispatched(claim.Dispatch) {
		return reconcile.Result{}, ErrMayHaveDispatched
	}
	pass := &dispatchPass{family: family}
	for _, step := range d.sequence() {
		if outcome := pass.run(ctx, step); outcome.stop {
			return outcome.result, outcome.err
		}
	}
	return pass.result, pass.err
}

type dispatchPass struct {
	family   Family
	expected *batchv1.Job
	created  *batchv1.Job
	result   reconcile.Result
	err      error
}

func (p *dispatchPass) run(ctx context.Context, step Step) Outcome {
	claim := p.family.Claim()
	switch step {
	case StepAuthorize:
		return p.family.Authorize(ctx)
	case StepLease:
		if !claim.HoldsLock {
			return Proceed()
		}
		return p.family.AcquireLease(ctx)
	case StepStage:
		return p.family.Stage(ctx)
	case StepSnapshot:
		return p.snapshot(ctx, claim)
	case StepConsume:
		// Dispatch refuses a mutating claim that may have dispatched, so a
		// claim spends its approval here before anything marks it.
		if !claim.Mutating {
			return Proceed()
		}
		return p.family.Consume(ctx)
	case StepMark:
		if !claim.HoldsLock {
			return Proceed()
		}
		if err := p.family.Mark(ctx); err != nil {
			return Stop(reconcile.Result{}, err)
		}
		return Proceed()
	case StepCreate:
		return p.create(ctx, claim)
	case StepConfirm:
		return p.confirm(ctx, claim)
	case StepRecord:
		return p.record(ctx)
	}
	return Stop(reconcile.Result{}, fmt.Errorf("dispatch has no %s step", step))
}

// build returns a copy of the Job the claim names, building it once per pass.
func (p *dispatchPass) build(ctx context.Context) (*batchv1.Job, Outcome) {
	if p.expected == nil {
		job, outcome := p.family.Build(ctx)
		if outcome.stop {
			return nil, outcome
		}
		p.expected = job
	}
	return p.expected.DeepCopy(), Proceed()
}

// snapshot is the admission boundary: a claim without a snapshot resolves one
// and ends the pass on the write, and a claim with one holds the rebuilt
// template to it, resolving it again once when the template moved.
func (p *dispatchPass) snapshot(ctx context.Context, claim Claim) Outcome {
	admission := p.family.Admission()
	// A snapshot the claim holds is checked before anything is built from the
	// claim it belongs to.
	if claim.Snapshot != nil {
		if err := admission.Validate(claim.Snapshot); err != nil {
			return Stop(p.family.Refuse(ctx, RefusalSnapshotInvalid,
				fmt.Errorf("validate persisted Pod admission snapshot: %w", err)))
		}
	}
	job, outcome := p.build(ctx)
	if outcome.stop {
		return outcome
	}
	if claim.Snapshot == nil {
		snapshot, err := admission.Resolve(ctx, &job.Spec.Template)
		if err != nil {
			return Stop(p.family.Refuse(ctx, RefusalSnapshotUnresolvable,
				fmt.Errorf("resolve Pod admission snapshot: %w", err)))
		}
		return Stop(p.family.SaveSnapshot(ctx, snapshot))
	}
	digest, err := admission.Digest(&job.Spec.Template)
	if err != nil {
		return Stop(p.family.Refuse(ctx, RefusalTemplateUnreadable,
			fmt.Errorf("digest the rebuilt Job Pod template: %w", err)))
	}
	if digest == claim.Snapshot.TemplateDigest {
		return Proceed()
	}
	// Nothing was dispatched and the claim's inputs still hold, so what
	// differs is the manager that built the template. The snapshot is
	// resolved again from this manager's template -- once: a template that
	// moves again comes from a builder that does not build the same Job twice,
	// and refreshing it would never end.
	if claim.SnapshotRefreshed {
		return Stop(p.family.Refuse(ctx, RefusalTemplateMoved, errors.New(
			"the rebuilt Job Pod template differs from the admission snapshot it was already resolved again for")))
	}
	return Stop(p.family.RefreshSnapshot(ctx))
}

func (p *dispatchPass) create(ctx context.Context, claim Claim) Outcome {
	job, outcome := p.build(ctx)
	if outcome.stop {
		return outcome
	}
	if err := p.family.Create(ctx, job); err != nil {
		switch DispatchFailure(StageCreate, claim.Mutating, apierrors.IsAlreadyExists(err)) {
		case DispositionUnaccounted:
			// Including AlreadyExists. A Job standing under the name the claim
			// reserved may be one this claim created on a pass whose answer
			// was lost, and retrying would dispatch beside it.
			return Stop(p.family.Unaccounted(ctx, nil,
				fmt.Errorf("the %s Job create result is uncertain: %w", claim.Type, err)))
		case DispositionRetry:
			return Stop(p.family.Retry(ctx, errors.New("the claimed Job name was occupied during dispatch")))
		}
		return Stop(reconcile.Result{}, fmt.Errorf("create %s Job: %w", claim.Type, err))
	}
	p.created = job
	return Proceed()
}

func (p *dispatchPass) confirm(ctx context.Context, claim Claim) Outcome {
	if p.created == nil {
		return Stop(reconcile.Result{}, fmt.Errorf("no created %s Job to confirm", claim.Type))
	}
	read, err := p.family.Confirm(ctx, p.created)
	if err != nil {
		if DispatchFailure(StageConfirm, claim.Mutating, false) == DispositionUnaccounted {
			return Stop(p.family.Unaccounted(ctx, nil,
				fmt.Errorf("cannot confirm the dispatched %s Job: %w", claim.Type, err)))
		}
		// The boundary is already durable, so the next pass re-enters through
		// the claim's own verdict and can say more than this one.
		return Stop(reconcile.Result{}, fmt.Errorf("read created %s Job: %w", claim.Type, err))
	}
	if err := p.family.Intent(read, p.expected); err != nil {
		failure := fmt.Errorf("the created %s Job failed immutable intent validation: %w", claim.Type, err)
		if DispatchFailure(StageIntent, claim.Mutating, false) == DispositionUnaccounted {
			// The Job exists by now and its executor may already be opening the
			// database, so the claim is not free to walk away from it.
			return Stop(p.family.Unaccounted(ctx, read, failure))
		}
		return Stop(p.family.Retry(ctx, failure))
	}
	p.created = read
	return Proceed()
}

func (p *dispatchPass) record(ctx context.Context) Outcome {
	if p.created == nil {
		return Stop(reconcile.Result{}, errors.New("no created Job to record"))
	}
	p.result, p.err = p.family.Record(ctx, p.created)
	if p.err != nil {
		return Stop(reconcile.Result{}, p.err)
	}
	return Proceed()
}
