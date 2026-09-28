package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// The dispatch sequence is a list of durable writes, and its safety is an
// order: whatever a Job depends on is durable before the mark that says a Job
// may exist, the mark before the one create, and the create read back before
// its UID is recorded. These tests drive each family's claim through the
// sequence with a fault at one step, read what the API server holds when the
// fault fires -- which is what a manager that stopped there leaves the next
// one -- and then run the passes that recover from it.
//
// Every step's row names two steps whose exchange must make it fail, and
// TestEachBoundaryFailsWithItsStepsSwapped runs it against that driver. A row
// that passes against the order and against the order with its boundary moved
// would be measuring something other than the boundary.

var (
	// errInjectedFault is a write or a read the harness refused.
	errInjectedFault = errors.New("injected fault")
	// errProcessStopped is every write after the one a stopped process made
	// last: nothing after it reaches the API server.
	errProcessStopped = errors.New("the process stopped after its previous write")
)

// dispatchFault is the fault one scenario injects.
type dispatchFault struct {
	// step is the step whose API call the fault is attached to.
	step mutationlifecycle.Step
	// stop lets the write commit and then stops the process, so no later
	// write reaches the API server. Otherwise the call itself fails.
	stop  bool
	fired bool
}

// dispatchCall is one call the harness saw, in order.
type dispatchCall struct {
	step mutationlifecycle.Step
	kind string
	name string
	read bool
	err  error
}

// dispatchAPI stands between a reconciler and the fake API server. It records
// every call it passes on, classifies each write by the step that makes it,
// and injects the scenario's fault.
type dispatchAPI struct {
	client.Client
	claimJob string

	mu      sync.Mutex
	calls   []dispatchCall
	fault   *dispatchFault
	stopped bool
	created map[string]bool
	creates int
}

func newDispatchAPI(base client.Client) *dispatchAPI {
	return &dispatchAPI{Client: base, created: map[string]bool{}}
}

func (a *dispatchAPI) arm(fault *dispatchFault) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fault = fault
}

// restart is a new process over the same API server: no fault, nothing
// refused.
func (a *dispatchAPI) restart() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fault = nil
	a.stopped = false
}

func (a *dispatchAPI) fired() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fault != nil && a.fault.fired
}

func (a *dispatchAPI) log() []dispatchCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]dispatchCall(nil), a.calls...)
}

func (a *dispatchAPI) jobCreates() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.creates
}

// write passes one write on, or refuses it the way the scenario says.
func (a *dispatchAPI) write(step mutationlifecycle.Step, object client.Object, apply func() error) error {
	a.mu.Lock()
	call := dispatchCall{step: step, kind: fmt.Sprintf("%T", object), name: object.GetName()}
	if a.stopped {
		call.err = errProcessStopped
		a.calls = append(a.calls, call)
		a.mu.Unlock()
		return errProcessStopped
	}
	fault := a.fault
	injected := step != 0 && fault != nil && !fault.fired && fault.step == step
	if injected {
		fault.fired = true
		if !fault.stop {
			call.err = errInjectedFault
			a.calls = append(a.calls, call)
			a.mu.Unlock()
			return errInjectedFault
		}
	}
	a.mu.Unlock()

	err := apply()

	a.mu.Lock()
	defer a.mu.Unlock()
	call.err = err
	a.calls = append(a.calls, call)
	if _, isJob := object.(*batchv1.Job); isJob && step == mutationlifecycle.StepCreate && err == nil {
		a.created[object.GetName()] = true
		a.creates++
	}
	if injected && fault.stop {
		a.stopped = true
	}
	return err
}

func (a *dispatchAPI) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	step := mutationlifecycle.Step(0)
	switch object.(type) {
	case *batchv1.Job:
		step = mutationlifecycle.StepCreate
	case *coordinationv1.Lease:
		step = mutationlifecycle.StepLease
	case *corev1.ConfigMap:
		step = mutationlifecycle.StepStage
	}
	return a.write(step, object, func() error { return a.Client.Create(ctx, object, options...) })
}

func (a *dispatchAPI) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	step := mutationlifecycle.Step(0)
	if _, ok := object.(*coordinationv1.Lease); ok {
		step = mutationlifecycle.StepLease
	}
	return a.write(step, object, func() error { return a.Client.Update(ctx, object, options...) })
}

func (a *dispatchAPI) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	return a.write(0, object, func() error { return a.Client.Patch(ctx, object, patch, options...) })
}

func (a *dispatchAPI) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	return a.write(0, object, func() error { return a.Client.Delete(ctx, object, options...) })
}

// Get fails the read that confirms a created Job when the scenario says so,
// once, and records every read of the claim's Job.
func (a *dispatchAPI) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, isJob := object.(*batchv1.Job); isJob && key.Name == a.claimJob {
		a.mu.Lock()
		fault := a.fault
		if a.created[key.Name] && fault != nil && !fault.fired && fault.step == mutationlifecycle.StepConfirm {
			fault.fired = true
			a.calls = append(a.calls, dispatchCall{
				step: mutationlifecycle.StepConfirm, kind: "*v1.Job", name: key.Name, read: true, err: errInjectedFault,
			})
			a.mu.Unlock()
			return errInjectedFault
		}
		a.mu.Unlock()
		err := a.Client.Get(ctx, key, object, options...)
		a.mu.Lock()
		a.calls = append(a.calls, dispatchCall{
			step: mutationlifecycle.StepConfirm, kind: "*v1.Job", name: key.Name, read: true, err: err,
		})
		a.mu.Unlock()
		return err
	}
	return a.Client.Get(ctx, key, object, options...)
}

func (a *dispatchAPI) Status() client.SubResourceWriter {
	return &dispatchStatusWriter{api: a, SubResourceWriter: a.Client.Status()}
}

type dispatchStatusWriter struct {
	client.SubResourceWriter
	api *dispatchAPI
}

func (w *dispatchStatusWriter) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.SubResourcePatchOption,
) error {
	step := w.api.statusStep(ctx, object)
	return w.api.write(step, object, func() error { return w.SubResourceWriter.Patch(ctx, object, patch, options...) })
}

func (w *dispatchStatusWriter) Update(
	ctx context.Context,
	object client.Object,
	options ...client.SubResourceUpdateOption,
) error {
	step := w.api.statusStep(ctx, object)
	return w.api.write(step, object, func() error { return w.SubResourceWriter.Update(ctx, object, options...) })
}

// claimRecord is the part of a claim the sequence writes, in terms both
// families share.
type claimRecord struct {
	present         bool
	dispatchStarted bool
	jobUID          types.UID
	snapshot        bool
	leaseEpoch      string
}

func recordOf(object client.Object) claimRecord {
	switch resource := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		if operation := resource.Status.ActiveOperation; operation != nil {
			return claimRecord{
				present: true, dispatchStarted: operation.DispatchStarted, jobUID: operation.JobUID,
				snapshot: operation.AdmissionSnapshot != nil, leaseEpoch: operation.LeaseEpoch,
			}
		}
	case *operatorv1alpha1.PtahMigration:
		if operation := resource.Status.ActiveOperation; operation != nil {
			return claimRecord{
				present: true, dispatchStarted: operation.DispatchStarted, jobUID: operation.JobUID,
				snapshot: operation.AdmissionSnapshot != nil, leaseEpoch: operation.LeaseEpoch,
			}
		}
	}
	return claimRecord{}
}

// statusStep names the step a status write belongs to by what it changes on
// the claim, or the approval it spends.
func (a *dispatchAPI) statusStep(ctx context.Context, object client.Object) mutationlifecycle.Step {
	switch object.(type) {
	case *operatorv1alpha1.PtahSchemaApproval, *operatorv1alpha1.PtahMigrationApproval:
		return mutationlifecycle.StepConsume
	case *operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahMigration:
	default:
		return 0
	}
	stored, ok := object.DeepCopyObject().(client.Object)
	if !ok || a.Client.Get(ctx, client.ObjectKeyFromObject(object), stored) != nil {
		return 0
	}
	before, after := recordOf(stored), recordOf(object)
	switch {
	case !before.present || !after.present:
		return 0
	case !before.snapshot && after.snapshot:
		return mutationlifecycle.StepSnapshot
	case !before.dispatchStarted && after.dispatchStarted:
		return mutationlifecycle.StepMark
	case before.jobUID == "" && after.jobUID != "":
		return mutationlifecycle.StepRecord
	case before.leaseEpoch != after.leaseEpoch:
		return mutationlifecycle.StepLease
	}
	return 0
}

// dispatchView is what the API server holds about one claim, in terms both
// families share.
type dispatchView struct {
	claim claimRecord
	// uncertain says the claim was retired into the family's record of a run
	// nobody accounted for.
	uncertain bool
	// approvalSpent says the approval that authorized the claim is consumed.
	approvalSpent bool
	// projected says the plan the Job mounts is staged; true for a family
	// with nothing to stage.
	projected bool
	// jobs counts the Jobs under the name the claim reserved.
	jobs int
	// leaseHolder is the holder the realm Lease names.
	leaseHolder string
}

// dispatchHarness is one family's claim under the harness.
type dispatchHarness struct {
	t      *testing.T
	api    *dispatchAPI
	pass   func() error
	driver func(mutationlifecycle.Driver)
	view   func() dispatchView
	// refuse moves something the claim was authorized by.
	refuse func()
	// contend takes the realm for another holder and returns that holder,
	// and a function that hands it back.
	contend func() (string, func())
}

// dispatchScenario is one step's fault.
type dispatchScenario struct {
	step mutationlifecycle.Step
	// fault is what goes wrong at the step: a refusal, contention, a write
	// that fails, a write after which the process stops, or a read that fails.
	fault string
	// swap is two steps whose exchange must make the scenario fail, and want
	// the problem it has to fail with: the promise the boundary keeps.
	swap [2]mutationlifecycle.Step
	want string
}

const (
	faultRefused   = "the claim's authorization moved while another holder had the realm"
	faultContended = "another holder has the realm"
	faultFails     = "the step's write fails"
	faultStops     = "the process stops right after the step's write"
	faultReadFails = "reading the created Job back fails"
)

func dispatchScenarios() []dispatchScenario {
	type step = mutationlifecycle.Step
	return []dispatchScenario{
		{step: mutationlifecycle.StepAuthorize, fault: faultRefused,
			swap: [2]step{mutationlifecycle.StepAuthorize, mutationlifecycle.StepLease},
			want: "was not retired while another holder had the realm"},
		{step: mutationlifecycle.StepLease, fault: faultContended,
			swap: [2]step{mutationlifecycle.StepLease, mutationlifecycle.StepMark},
			want: "marked while another holder has the realm"},
		{step: mutationlifecycle.StepStage, fault: faultFails,
			swap: [2]step{mutationlifecycle.StepStage, mutationlifecycle.StepMark},
			want: "marked with nothing staged"},
		{step: mutationlifecycle.StepSnapshot, fault: faultFails,
			swap: [2]step{mutationlifecycle.StepSnapshot, mutationlifecycle.StepMark},
			want: "marked with no admission snapshot"},
		{step: mutationlifecycle.StepConsume, fault: faultFails,
			swap: [2]step{mutationlifecycle.StepConsume, mutationlifecycle.StepMark},
			want: "the approval it spends is not"},
		{step: mutationlifecycle.StepMark, fault: faultFails,
			swap: [2]step{mutationlifecycle.StepMark, mutationlifecycle.StepCreate},
			want: "the claim does not say one may"},
		{step: mutationlifecycle.StepCreate, fault: faultStops,
			swap: [2]step{mutationlifecycle.StepCreate, mutationlifecycle.StepMark},
			want: "the claim does not say one may"},
		{step: mutationlifecycle.StepConfirm, fault: faultReadFails,
			swap: [2]step{mutationlifecycle.StepConfirm, mutationlifecycle.StepRecord},
			want: "without reading the created Job back"},
		{step: mutationlifecycle.StepRecord, fault: faultStops,
			swap: [2]step{mutationlifecycle.StepRecord, mutationlifecycle.StepConfirm},
			want: "without reading the created Job back"},
	}
}

// dispatchFamily is one family under the harness. stages says whether it
// writes anything besides the Job before the mark; the stage row is not run
// for a family that does not, rather than run and skipped.
type dispatchFamily struct {
	name    string
	stages  bool
	harness func(*testing.T) *dispatchHarness
}

func dispatchFamilies() []dispatchFamily {
	return []dispatchFamily{
		{name: "PtahSchema", stages: true, harness: schemaDispatchHarness},
		{name: "PtahMigration", harness: migrationDispatchHarness},
	}
}

// dispatchRows is every family's rows.
func dispatchRows() []struct {
	family   dispatchFamily
	scenario dispatchScenario
} {
	var rows []struct {
		family   dispatchFamily
		scenario dispatchScenario
	}
	for _, family := range dispatchFamilies() {
		for _, scenario := range dispatchScenarios() {
			if scenario.step == mutationlifecycle.StepStage && !family.stages {
				continue
			}
			rows = append(rows, struct {
				family   dispatchFamily
				scenario dispatchScenario
			}{family, scenario})
		}
	}
	return rows
}

// run drives one scenario and returns every way the durable state or the
// recovery broke the order's promises. An empty result is the pass.
//
// The invariants are read after every pass, the one the fault ends and each
// one before it, because each of those states is one a manager can stop in and
// hand to the next.
func (h *dispatchHarness) run(scenario dispatchScenario) []string {
	h.t.Helper()

	var problems []string
	check := func(contender string) dispatchView {
		view := h.view()
		for _, problem := range h.invariants(view, contender) {
			if !slices.Contains(problems, problem) {
				problems = append(problems, problem)
			}
		}
		return view
	}
	contender := ""
	release := func() {}
	switch scenario.fault {
	case faultRefused:
		contender, release = h.contend()
		h.refuse()
	case faultContended:
		contender, release = h.contend()
	case faultFails, faultReadFails:
		h.api.arm(&dispatchFault{step: scenario.step})
	case faultStops:
		h.api.arm(&dispatchFault{step: scenario.step, stop: true})
	}

	// The passes up to and including the one the fault ends.
	var stoppedAt dispatchView
	switch scenario.fault {
	case faultRefused, faultContended:
		_ = h.pass()
		stoppedAt = check(contender)
	default:
		for attempt := 0; attempt < 4 && !h.api.fired(); attempt++ {
			_ = h.pass()
			stoppedAt = check(contender)
		}
		if !h.api.fired() {
			problems = append(problems, "the fault never fired, so the step was not measured")
		}
	}
	if scenario.fault == faultRefused && stoppedAt.claim.present {
		problems = append(problems, "a claim whose authorization moved was not retired while another holder had the realm")
	}
	if scenario.fault == faultRefused && stoppedAt.leaseHolder != contender {
		problems = append(problems, "a claim whose authorization moved touched the realm Lease")
	}
	markedWithoutJob := stoppedAt.claim.dispatchStarted && stoppedAt.jobs == 0
	createsAtStop := h.api.jobCreates()

	// A new process over the same API server, with the realm free.
	release()
	h.api.restart()
	settled := h.view()
	for attempt := 0; attempt < 12 && settled.claim.present && settled.claim.jobUID == ""; attempt++ {
		_ = h.pass()
		settled = check("")
	}
	switch {
	case h.api.jobCreates() > 1:
		problems = append(problems, fmt.Sprintf("the claim created %d Jobs", h.api.jobCreates()))
	case markedWithoutJob && h.api.jobCreates() > createsAtStop:
		problems = append(problems, "a claim marked with no Job behind it created one after the restart")
	case scenario.fault == faultRefused:
		if settled.claim.present || settled.jobs != 0 {
			problems = append(problems, "the refused claim came back and dispatched")
		}
	case settled.claim.present && settled.claim.jobUID == "":
		problems = append(problems, "the claim never reached its Job after the restart")
	case !settled.claim.present && !settled.uncertain:
		problems = append(problems, "the claim was retired without dispatching and without a record")
	case !settled.claim.present && !markedWithoutJob:
		problems = append(problems, "the claim was retired as unaccounted although nothing stood in its way")
	}
	problems = append(problems, h.recordedOnlyConfirmed()...)
	return problems
}

// invariants are what every state the sequence can stop in holds. contender
// is another holder of the realm, when the scenario put one there.
func (h *dispatchHarness) invariants(view dispatchView, contender string) []string {
	var problems []string
	claim := view.claim
	if view.jobs > 0 && claim.present && !claim.dispatchStarted {
		problems = append(problems, "a Job exists under the reserved name and the claim does not say one may")
	}
	if claim.dispatchStarted {
		if !view.approvalSpent {
			problems = append(problems, "the claim is marked and the approval it spends is not")
		}
		if !claim.snapshot {
			problems = append(problems, "the claim is marked with no admission snapshot")
		}
		if !view.projected {
			problems = append(problems, "the claim is marked with nothing staged for its Job to read")
		}
		if contender != "" && view.leaseHolder == contender {
			problems = append(problems, "the claim is marked while another holder has the realm")
		}
	}
	return problems
}

// recordedOnlyConfirmed holds every UID the claim recorded to a read of the
// created Job that came between the create and the record.
func (h *dispatchHarness) recordedOnlyConfirmed() []string {
	calls := h.api.log()
	created := -1
	for index, call := range calls {
		switch {
		case call.step == mutationlifecycle.StepCreate && !call.read && call.err == nil:
			created = index
		case call.step == mutationlifecycle.StepRecord && !call.read && call.err == nil:
			if created < 0 {
				return []string{"a UID was recorded with no Job created"}
			}
			confirmed := false
			for _, between := range calls[created+1 : index] {
				if between.read && between.step == mutationlifecycle.StepConfirm && between.err == nil {
					confirmed = true
				}
			}
			if !confirmed {
				return []string{"a UID was recorded without reading the created Job back"}
			}
		}
	}
	return nil
}

// TestEachDispatchStepLeavesAStateTheNextPassRecoversFrom drives both
// families through every step boundary with a fault at that step.
func TestEachDispatchStepLeavesAStateTheNextPassRecoversFrom(t *testing.T) {
	t.Parallel()

	for _, row := range dispatchRows() {
		t.Run(row.family.name+"/"+row.scenario.step.String(), func(t *testing.T) {
			t.Parallel()

			for _, problem := range row.family.harness(t).run(row.scenario) {
				t.Errorf("%s: %s", row.scenario.fault, problem)
			}
		})
	}
}

// TestEachBoundaryFailsWithItsStepsSwapped is the other half: each scenario
// above, against a driver with the two steps its boundary is about
// exchanged, has to find something wrong.
func TestEachBoundaryFailsWithItsStepsSwapped(t *testing.T) {
	t.Parallel()

	for _, row := range dispatchRows() {
		scenario := row.scenario
		t.Run(fmt.Sprintf("%s/%s/%s-%s", row.family.name, scenario.step, scenario.swap[0], scenario.swap[1]), func(t *testing.T) {
			t.Parallel()

			h := row.family.harness(t)
			h.driver(mutationlifecycle.Driver{}.Swapped(scenario.swap[0], scenario.swap[1]))
			problems := h.run(scenario)
			if !slices.ContainsFunc(problems, func(problem string) bool {
				return strings.Contains(problem, scenario.want)
			}) {
				t.Fatalf("with %s and %s exchanged, %q did not find %q; it found %q",
					scenario.swap[0], scenario.swap[1], scenario.fault, scenario.want, problems)
			}
		})
	}
}

func schemaDispatchHarness(t *testing.T) *dispatchHarness {
	t.Helper()

	reconciler, base, schema, jobName := dispatchableApply(t, true)
	api := newDispatchAPI(base)
	api.claimJob = jobName
	reconciler.Client = assignCreatedJobUIDClient{Client: api, uid: "apply-job-uid"}
	reconciler.APIReader = api
	reconciler.Plans = planstore.Store{Client: &planUIDAssigningClient{Client: api}, Reader: api}
	reconciler.Locks = targetlock.New(api, api, nil)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	ctx := context.Background()
	stored := func() *operatorv1alpha1.PtahSchema {
		current := &operatorv1alpha1.PtahSchema{}
		if err := base.Get(ctx, request.NamespacedName, current); err != nil {
			t.Fatal(err)
		}
		return current
	}
	claim := stored().Status.ActiveOperation.DeepCopy()

	return &dispatchHarness{
		t:   t,
		api: api,
		pass: func() error {
			_, err := reconciler.Reconcile(ctx, request)
			return err
		},
		driver: func(driver mutationlifecycle.Driver) { reconciler.dispatch = driver },
		view: func() dispatchView {
			current := stored()
			view := dispatchView{
				claim:       recordOf(current),
				uncertain:   current.Status.PendingObservation != nil,
				jobs:        countJobs(t, base, schema.Namespace, jobName),
				leaseHolder: realmHolder(t, base, reconciler.LockNamespace, claim.CoordinationDigest),
			}
			approval := &operatorv1alpha1.PtahSchemaApproval{}
			if err := base.Get(ctx, client.ObjectKey{Namespace: schema.Namespace, Name: "dispatch-approval"}, approval); err != nil {
				t.Fatal(err)
			}
			view.approvalSpent = meta.IsStatusConditionTrue(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed)
			view.projected = countProjections(t, base, schema.Namespace) > 0
			return view
		},
		refuse: func() {
			current := stored()
			current.Generation++
			if err := base.Update(ctx, current); err != nil {
				t.Fatal(err)
			}
		},
		contend: func() (string, func()) {
			return contendRealm(t, reconciler.Locks, base, reconciler.LockNamespace, claim.CoordinationDigest)
		},
	}
}

func migrationDispatchHarness(t *testing.T) *dispatchHarness {
	t.Helper()

	migration, plan := awaitingApprovalFixture(t)
	approval := migrationApprovalFor(migration, plan)
	reconciler, base := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval, verificationPolicyConfigMap())
	ctx := context.Background()
	request := migrationRequest(migration)
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("claim the approved Apply: %v", err)
	}
	stored := func() *operatorv1alpha1.PtahMigration { return readMigration(t, base, migration) }
	claim := stored().Status.ActiveOperation.DeepCopy()
	if claim == nil || claim.Type != operatorv1alpha1.MigrationOperationApply || claim.DispatchStarted {
		t.Fatalf("the fixture did not reach an undispatched Apply claim: %#v", claim)
	}
	api := newDispatchAPI(base)
	api.claimJob = claim.JobName
	reconciler.Client = api
	reconciler.APIReader = api
	reconciler.Locks = targetlock.New(api, api, &fixedClock{now: reconciler.now()})

	return &dispatchHarness{
		t:   t,
		api: api,
		pass: func() error {
			_, err := reconciler.Reconcile(ctx, request)
			return err
		},
		driver: func(driver mutationlifecycle.Driver) { reconciler.dispatch = driver },
		view: func() dispatchView {
			current := stored()
			view := dispatchView{
				claim:       recordOf(current),
				uncertain:   current.Status.UnresolvedRun != nil,
				projected:   true,
				jobs:        countJobs(t, base, migration.Namespace, claim.JobName),
				leaseHolder: realmHolder(t, base, reconciler.LockNamespace, claim.CoordinationDigest),
			}
			spent := &operatorv1alpha1.PtahMigrationApproval{}
			if err := base.Get(ctx, client.ObjectKeyFromObject(approval), spent); err != nil {
				t.Fatal(err)
			}
			view.approvalSpent = meta.IsStatusConditionTrue(spent.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed)
			return view
		},
		refuse: func() {
			current := stored()
			current.Generation++
			if err := base.Update(ctx, current); err != nil {
				t.Fatal(err)
			}
		},
		contend: func() (string, func()) {
			return contendRealm(t, reconciler.Locks, base, reconciler.LockNamespace, claim.CoordinationDigest)
		},
	}
}

func countJobs(t *testing.T, api client.Client, namespace, name string) int {
	t.Helper()
	job := &batchv1.Job{}
	switch err := api.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, job); {
	case apierrors.IsNotFound(err):
		return 0
	case err != nil:
		t.Fatal(err)
	}
	return 1
}

// countProjections counts the ConfigMaps a plan owns: the projection its Apply
// Pod mounts.
func countProjections(t *testing.T, api client.Client, namespace string) int {
	t.Helper()
	list := &corev1.ConfigMapList{}
	if err := api.List(context.Background(), list, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, configMap := range list.Items {
		for _, owner := range configMap.OwnerReferences {
			if owner.Kind == "PtahSchemaPlan" {
				count++
			}
		}
	}
	return count
}

// realmHolder is the holder the realm Lease names, or empty.
func realmHolder(t *testing.T, api client.Client, namespace, coordinationDigest string) string {
	t.Helper()
	name, err := targetlock.LeaseName(coordinationDigest)
	if err != nil {
		t.Fatal(err)
	}
	lease := &coordinationv1.Lease{}
	switch err := api.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, lease); {
	case apierrors.IsNotFound(err):
		return ""
	case err != nil:
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

// contendRealm takes the realm for another resource's claim, and returns the
// holder it now names and a function that hands it back.
func contendRealm(
	t *testing.T,
	locks *targetlock.Locker,
	api client.Client,
	namespace, coordinationDigest string,
) (string, func()) {
	t.Helper()
	request := targetlock.Request{
		CoordinationNamespace: namespace,
		CoordinationDigest:    coordinationDigest,
		Holder:                targetlock.Holder{SchemaUID: "another-resource", OperationID: "another-apply"},
		Duration:              16 * time.Minute,
	}
	result, err := locks.Acquire(context.Background(), request)
	if err != nil || !result.Acquired {
		t.Fatalf("take the realm for another holder: acquired=%t err=%v", result.Acquired, err)
	}
	holder := realmHolder(t, api, namespace, coordinationDigest)
	if holder == "" {
		t.Fatal("the contender holds the realm under no name")
	}
	request.ExpectedEpoch = result.Epoch
	return holder, func() {
		if err := locks.Release(context.Background(), request); err != nil {
			t.Fatalf("hand the contender's realm back: %v", err)
		}
	}
}
