package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// A mutating claim is marked before its one create. With two managers acting
// at once, which a node pause causes until the old leader's lease runs out,
// one of them can read the other's mark before that create lands. It used to
// retire the claim there as an Apply nobody accounted for and spend the
// approval (#869). These rows hold both families to waiting
// mutationlifecycle.MissingJobGrace, measured on the reader's own clock from
// its first sighting, before calling the Job lost.

// waitOutMissingJobGrace runs the pass that first finds a marked Apply with no
// recorded Job and no Job, holds it to waiting the whole grace, and moves clock
// to the end of the grace, so the caller's next pass is the one that calls the
// Job lost.
func waitOutMissingJobGrace(t *testing.T, clock *safetyClock, pass func() (ctrl.Result, error)) {
	t.Helper()
	result, err := pass()
	if err != nil {
		t.Fatalf("Reconcile() on first finding the marked Apply's Job missing: %v", err)
	}
	if result.RequeueAfter != mutationlifecycle.MissingJobGrace {
		t.Fatalf("the first pass to find the marked Apply's Job missing requeued after %s, want the whole grace %s",
			result.RequeueAfter, mutationlifecycle.MissingJobGrace)
	}
	clock.now = clock.now.Add(mutationlifecycle.MissingJobGrace)
}

const missingJobMessage = "dispatched Apply Job is missing and will not be recreated"

// markedClaim is one family's marked Apply under test, as one manager sees it.
type markedClaim struct {
	t     *testing.T
	clock *safetyClock
	start time.Time
	// reconcile runs one pass of the manager the row currently stands for.
	reconcile func() (ctrl.Result, error)
	// restart replaces that manager with a new process over the same API
	// server and the same clock.
	restart func()
	// state reads what the API server holds about the claim.
	state func() markedClaimState
}

// markedClaimState is the part of a resource these rows judge.
type markedClaimState struct {
	// present: the claim is still active.
	present bool
	marked  bool
	jobUID  types.UID
	// lost: the resource records the claim's Job as missing and the outcome as
	// unknown.
	lost bool
	// jobs is every Job in the namespace, under any name.
	jobs int
}

func countNamespaceJobs(t *testing.T, api client.Client, namespace string) int {
	t.Helper()
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs, client.InNamespace(namespace)); err != nil {
		t.Fatalf("List(Jobs) error = %v", err)
	}
	return len(jobs.Items)
}

func schemaMarkedClaimState(t *testing.T, api client.Client, schema *operatorv1alpha1.PtahSchema) markedClaimState {
	t.Helper()
	stored := safetyGetSchema(t, api, schema)
	state := markedClaimState{jobs: countNamespaceJobs(t, api, schema.Namespace)}
	if operation := stored.Status.ActiveOperation; operation != nil {
		state.present, state.marked, state.jobUID = true, operation.DispatchStarted, operation.JobUID
	}
	failed := meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	state.lost = stored.Status.PendingObservation != nil &&
		stored.Status.PendingObservation.Outcome == operatorv1alpha1.PendingObservationOutcomeUnknown &&
		failed != nil && failed.Reason == string(operatorv1alpha1.ReasonApplyOutcomeUnknown) &&
		strings.Contains(failed.Message, missingJobMessage)
	return state
}

func migrationMarkedClaimState(t *testing.T, api client.Client, migration *operatorv1alpha1.PtahMigration) markedClaimState {
	t.Helper()
	stored := readMigration(t, api, migration)
	state := markedClaimState{jobs: countNamespaceJobs(t, api, migration.Namespace)}
	if operation := stored.Status.ActiveOperation; operation != nil {
		state.present, state.marked, state.jobUID = true, operation.DispatchStarted, operation.JobUID
	}
	blocked := meta.FindStatusCondition(stored.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
	state.lost = stored.Status.LastRun != nil &&
		stored.Status.LastRun.Outcome == operatorv1alpha1.MigrationRunOutcomeUnknown &&
		blocked != nil && blocked.Status == "True" && strings.Contains(blocked.Message, missingJobMessage)
	return state
}

// pass runs one pass at the instant the row names, measured from the start.
func (c *markedClaim) pass(at time.Duration) ctrl.Result {
	c.t.Helper()
	c.clock.now = c.start.Add(at)
	result, err := c.reconcile()
	if err != nil {
		c.t.Fatalf("Reconcile() %s after the first sighting: %v", at, err)
	}
	return result
}

// waits holds the pass at at to waiting want more and to leaving the claim
// marked, unretired and with no Job.
func (c *markedClaim) waits(at, want time.Duration) {
	c.t.Helper()
	result := c.pass(at)
	state := c.state()
	if !state.present || !state.marked || state.lost {
		c.t.Fatalf("the pass %s after the first sighting, inside the grace, left %+v; want the claim marked and kept", at, state)
	}
	if state.jobs != 0 {
		c.t.Fatalf("the pass %s after the first sighting created %d Jobs for a claim that may already have dispatched", at, state.jobs)
	}
	if result.RequeueAfter != want {
		c.t.Fatalf("the pass %s after the first sighting requeued after %s, want what is left of the grace, %s",
			at, result.RequeueAfter, want)
	}
}

// loses holds the pass at at to retiring the claim as one whose Job is missing,
// with no Job created.
func (c *markedClaim) loses(at time.Duration) {
	c.t.Helper()
	c.pass(at)
	state := c.state()
	if state.present || !state.lost {
		c.t.Fatalf("the pass %s after the first sighting left %+v; want the claim retired with its Job missing", at, state)
	}
	if state.jobs != 0 {
		c.t.Fatalf("the pass %s after the first sighting created %d Jobs for a claim that may already have dispatched", at, state.jobs)
	}
}

func markedSchemaApply(t *testing.T, jobUID types.UID) *markedClaim {
	t.Helper()
	schema := safetyApplySchema(t)
	schema.Status.ActiveOperation.DispatchStarted = true
	schema.Status.ActiveOperation.JobUID = jobUID
	reconciler, api := fakeReconciler(t, staticLogs{}, schema)
	reconciler.Locks = targetlock.New(api, api, nil)
	clock := &safetyClock{now: reconciler.now()}
	reconciler.Clock = clock.Now
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	manager := reconciler
	return &markedClaim{
		t: t, clock: clock, start: clock.now,
		reconcile: func() (ctrl.Result, error) { return manager.Reconcile(context.Background(), request) },
		restart: func() {
			restarted := *reconciler
			restarted.missingJobs = nil
			restarted.Locks = targetlock.New(api, api, nil)
			manager = &restarted
		},
		state: func() markedClaimState { return schemaMarkedClaimState(t, api, schema) },
	}
}

func markedMigrationApply(t *testing.T, jobUID types.UID) *markedClaim {
	t.Helper()
	migration, plan := awaitingApprovalFixture(t)
	operation := applyClaimFor(t, migration, plan)
	operation.DispatchStarted = true
	operation.JobUID = jobUID
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, verificationPolicyConfigMap())
	holdMigrationApplyLease(t, reconciler, api, migration)
	clock := &safetyClock{now: reconciler.now()}
	reconciler.Clock = clock.Now
	request := migrationRequest(migration)
	manager := reconciler
	return &markedClaim{
		t: t, clock: clock, start: clock.now,
		reconcile: func() (ctrl.Result, error) { return manager.Reconcile(context.Background(), request) },
		restart: func() {
			restarted := *reconciler
			restarted.missingJobs = nil
			restarted.Locks = targetlock.New(api, api, &fixedClock{now: clock.now})
			manager = &restarted
		},
		state: func() markedClaimState { return migrationMarkedClaimState(t, api, migration) },
	}
}

var markedClaimFamilies = []struct {
	name   string
	marked func(*testing.T, types.UID) *markedClaim
}{
	{"PtahSchema", markedSchemaApply},
	{"PtahMigration", markedMigrationApply},
}

// A marked Apply whose Job never lands is lost once the grace has passed, and
// not one pass earlier; until then each pass asks to come back when what is
// left of the grace has passed.
func TestAMarkedApplyWithNoJobIsLostOnlyAfterTheGrace(t *testing.T) {
	t.Parallel()

	grace := mutationlifecycle.MissingJobGrace
	for _, family := range markedClaimFamilies {
		t.Run(family.name, func(t *testing.T) {
			t.Parallel()
			claim := family.marked(t, "")
			claim.waits(0, grace)
			claim.waits(grace-time.Second, time.Second)
			claim.loses(grace)
		})
	}
}

// A claim that recorded its Job's UID saw its create return, so a missing Job
// is lost on the first pass, as it always was.
func TestARecordedApplyJobThatIsMissingIsLostAtOnce(t *testing.T) {
	t.Parallel()

	for _, family := range markedClaimFamilies {
		t.Run(family.name, func(t *testing.T) {
			t.Parallel()
			family.marked(t, "recorded-apply-job-uid").loses(0)
		})
	}
}

// A manager that restarts inside the grace remembers nothing and waits the
// whole grace from its own first sighting.
func TestAManagerRestartedInsideTheGraceWaitsAgain(t *testing.T) {
	t.Parallel()

	grace := mutationlifecycle.MissingJobGrace
	restartAt := 10 * time.Second
	for _, family := range markedClaimFamilies {
		t.Run(family.name, func(t *testing.T) {
			t.Parallel()
			claim := family.marked(t, "")
			claim.waits(0, grace)
			claim.restart()
			claim.waits(restartAt, grace)
			claim.waits(grace, restartAt)
			claim.loses(restartAt + grace)
		})
	}
}

// createRace is the dispatching manager's client. Its first Job create runs
// between first: another manager's pass, which reads the claim after the mark
// and before the create lands. With stopAfter it refuses every write after the
// create, as a manager that stopped there would.
type createRace struct {
	client.Client
	between   func()
	stopAfter bool
	fired     bool
	stopped   bool
}

func (c *createRace) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if c.stopped {
		return errProcessStopped
	}
	if _, isJob := object.(*batchv1.Job); !isJob || c.fired {
		return c.Client.Create(ctx, object, options...)
	}
	c.fired = true
	c.between()
	err := c.Client.Create(ctx, object, options...)
	c.stopped = c.stopAfter
	return err
}

func (c *createRace) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if c.stopped {
		return errProcessStopped
	}
	return c.Client.Update(ctx, object, options...)
}

func (c *createRace) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if c.stopped {
		return errProcessStopped
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func (c *createRace) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	if c.stopped {
		return errProcessStopped
	}
	return c.Client.Delete(ctx, object, options...)
}

func (c *createRace) Status() client.SubResourceWriter {
	return &createRaceStatus{SubResourceWriter: c.Client.Status(), race: c}
}

type createRaceStatus struct {
	client.SubResourceWriter
	race *createRace
}

func (w *createRaceStatus) Patch(
	ctx context.Context, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption,
) error {
	if w.race.stopped {
		return errProcessStopped
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}

func (w *createRaceStatus) Update(ctx context.Context, object client.Object, options ...client.SubResourceUpdateOption) error {
	if w.race.stopped {
		return errProcessStopped
	}
	return w.SubResourceWriter.Update(ctx, object, options...)
}

// racedDispatch is one family's approved Apply, a dispatching manager and a
// second manager over the same API server.
type racedDispatch struct {
	clock    *safetyClock
	dispatch func() error
	race     *createRace
	other    func() (ctrl.Result, error)
	state    func() markedClaimState
}

func racedSchemaDispatch(t *testing.T) *racedDispatch {
	t.Helper()
	reconciler, api, schema, _ := dispatchableApply(t, true)
	clock := &safetyClock{now: reconciler.now()}
	reconciler.Clock = clock.Now
	other := *reconciler
	other.missingJobs = nil
	other.Locks = targetlock.New(api, api, nil)
	race := &createRace{Client: reconciler.Client}
	reconciler.Client = race
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	return &racedDispatch{
		clock: clock, race: race,
		dispatch: func() error { _, err := reconciler.Reconcile(context.Background(), request); return err },
		other:    func() (ctrl.Result, error) { return other.Reconcile(context.Background(), request) },
		state:    func() markedClaimState { return schemaMarkedClaimState(t, api, schema) },
	}
}

func racedMigrationDispatch(t *testing.T) *racedDispatch {
	t.Helper()
	migration, plan := awaitingApprovalFixture(t)
	approval := migrationApprovalFor(migration, plan)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval, verificationPolicyConfigMap())
	request := migrationRequest(migration)
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("claim the approved Apply: %v", err)
	}
	if claim := readMigration(t, api, migration).Status.ActiveOperation; claim == nil ||
		claim.Type != operatorv1alpha1.MigrationOperationApply || claim.DispatchStarted {
		t.Fatalf("the fixture did not reach an undispatched Apply claim: %#v", claim)
	}
	clock := &safetyClock{now: reconciler.now()}
	reconciler.Clock = clock.Now
	other := *reconciler
	other.missingJobs = nil
	other.Locks = targetlock.New(api, api, &fixedClock{now: clock.now})
	race := &createRace{Client: reconciler.Client}
	reconciler.Client = race
	return &racedDispatch{
		clock: clock, race: race,
		dispatch: func() error { _, err := reconciler.Reconcile(context.Background(), request); return err },
		other:    func() (ctrl.Result, error) { return other.Reconcile(context.Background(), request) },
		state:    func() markedClaimState { return migrationMarkedClaimState(t, api, migration) },
	}
}

// The race #869 measured, with the second manager's pass placed exactly where
// it landed: after the mark and before the create. That pass waits and writes
// nothing, and the pass it asks for, inside the grace, goes on with the Job
// the first manager created -- supervising it where that manager recorded it,
// adopting it where that manager stopped first. Neither family retires the
// claim, and neither creates a second Job.
func TestASecondManagerWaitsForTheCreateItRacedAndThenUsesTheJob(t *testing.T) {
	t.Parallel()

	grace := mutationlifecycle.MissingJobGrace
	for _, family := range []struct {
		name  string
		build func(*testing.T) *racedDispatch
	}{
		{"PtahSchema", racedSchemaDispatch},
		{"PtahMigration", racedMigrationDispatch},
	} {
		for _, row := range []struct {
			name      string
			stopAfter bool
		}{
			{name: "the dispatching manager records its Job"},
			{name: "the dispatching manager stops before it records its Job", stopAfter: true},
		} {
			t.Run(family.name+"/"+row.name, func(t *testing.T) {
				t.Parallel()

				race := family.build(t)
				race.race.stopAfter = row.stopAfter
				start := race.clock.now
				var raced ctrl.Result
				race.race.between = func() {
					t.Helper()
					if seen := race.state(); !seen.present || !seen.marked || seen.jobUID != "" || seen.jobs != 0 {
						t.Fatalf("the second manager's pass came at %+v, not between the mark and the create; it measures nothing", seen)
					}
					var err error
					if raced, err = race.other(); err != nil {
						t.Fatalf("the second manager's pass between the mark and the create: %v", err)
					}
					if seen := race.state(); !seen.present || !seen.marked || seen.lost || seen.jobs != 0 {
						t.Fatalf("the second manager's pass between the mark and the create left %+v; want the claim marked and kept", seen)
					}
				}
				for attempt := 0; attempt < 6 && !race.race.fired; attempt++ {
					if err := race.dispatch(); err != nil && !row.stopAfter {
						t.Fatalf("the dispatching manager's pass %d: %v", attempt+1, err)
					}
				}
				if !race.race.fired {
					t.Fatal("the dispatching manager never created its Job, so the race was not run")
				}
				if raced.RequeueAfter != grace {
					t.Fatalf("the raced pass, the second manager's first sighting, requeued after %s, want the whole grace %s",
						raced.RequeueAfter, grace)
				}
				created := race.state()
				if created.jobs != 1 || (created.jobUID == "") != row.stopAfter {
					t.Fatalf("after the dispatching manager's create: %+v", created)
				}

				for _, at := range []time.Duration{10 * time.Second, grace + time.Second} {
					race.clock.now = start.Add(at)
					if _, err := race.other(); err != nil {
						t.Fatalf("the second manager's pass %s after the race: %v", at, err)
					}
					after := race.state()
					if !after.present || after.lost || after.jobUID == "" || after.jobs != 1 {
						t.Fatalf("the second manager's pass %s after the race left %+v; want the claim kept on its one recorded Job", at, after)
					}
				}
			})
		}
	}
}
