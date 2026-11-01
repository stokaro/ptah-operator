package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// realmClaimant drives one resource of either family through the realm
// census, with a clock the test moves.
type realmClaimant struct {
	t         *testing.T
	api       client.Client
	object    client.Object
	clock     *fixedClock
	reconcile func() (ctrl.Result, error)
}

// claimantReading is what one pass left on the claimant.
type claimantReading struct {
	phase      string
	operation  string
	next       *metav1.Time
	generation int64
	observed   int64
	// refusal is the reason on the condition a realm refusal writes its
	// message to: Ready for a schema, Blocked for a migration.
	refusal string
}

func newRealmClaimant(t *testing.T, family string, interval time.Duration) *realmClaimant {
	t.Helper()

	claimant := &realmClaimant{t: t}
	switch family {
	case "schema":
		schema := schemaFixture()
		schema.Spec.Interval.Duration = interval
		reconciler, api := fakeReconciler(t, nil, schema)
		claimant.clock = &fixedClock{now: reconciler.now()}
		reconciler.Clock = claimant.clock.Now
		reconciler.Locks = targetlock.New(api, api, claimant.clock)
		claimant.api, claimant.object = api, schema
		claimant.reconcile = func() (ctrl.Result, error) {
			return reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)})
		}
	case "migration":
		migration := migrationFixture()
		migration.Spec.Interval.Duration = interval
		migration.Status.ExecutionBinding = migrationExecutionBinding()
		reconciler, api := fakeMigrationReconciler(t, nil, migration, verificationPolicyConfigMap())
		claimant.clock = movableMigrationClock(reconciler, api)
		claimant.api, claimant.object = api, migration
		claimant.reconcile = func() (ctrl.Result, error) {
			return reconciler.Reconcile(context.Background(), migrationRequest(migration))
		}
	default:
		t.Fatalf("unknown family %q", family)
	}
	return claimant
}

func (c *realmClaimant) target() operatorv1alpha1.DatabaseTargetSpec {
	switch object := c.object.(type) {
	case *operatorv1alpha1.PtahSchema:
		return object.Spec.Target
	case *operatorv1alpha1.PtahMigration:
		return object.Spec.Target
	}
	c.t.Fatalf("unknown claimant %T", c.object)
	return operatorv1alpha1.DatabaseTargetSpec{}
}

func (c *realmClaimant) read() claimantReading {
	c.t.Helper()

	key := client.ObjectKeyFromObject(c.object)
	switch c.object.(type) {
	case *operatorv1alpha1.PtahSchema:
		schema := &operatorv1alpha1.PtahSchema{}
		if err := c.api.Get(context.Background(), key, schema); err != nil {
			c.t.Fatal(err)
		}
		reading := claimantReading{
			phase: string(schema.Status.Phase), next: schema.Status.NextReconciliationTime,
			generation: schema.Generation, observed: schema.Status.ObservedGeneration,
		}
		if operation := schema.Status.ActiveOperation; operation != nil {
			reading.operation = string(operation.Type)
		}
		if ready := findCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReady); ready != nil {
			reading.refusal = ready.Reason
		}
		return reading
	case *operatorv1alpha1.PtahMigration:
		migration := &operatorv1alpha1.PtahMigration{}
		if err := c.api.Get(context.Background(), key, migration); err != nil {
			c.t.Fatal(err)
		}
		reading := claimantReading{
			phase: string(migration.Status.Phase), next: migration.Status.NextReconciliationTime,
			generation: migration.Generation, observed: migration.Status.ObservedGeneration,
		}
		if operation := migration.Status.ActiveOperation; operation != nil {
			reading.operation = string(operation.Type)
		}
		if blocked := findCondition(migration.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked); blocked != nil {
			reading.refusal = blocked.Reason
		}
		return reading
	}
	c.t.Fatalf("unknown claimant %T", c.object)
	return claimantReading{}
}

// settle runs passes at the current instant until reached holds, and returns
// the reading that matched and the result of the pass that produced it.
func (c *realmClaimant) settle(what string, reached func(claimantReading) bool) (claimantReading, ctrl.Result) {
	c.t.Helper()

	var last claimantReading
	for range 6 {
		result, err := c.reconcile()
		if err != nil {
			c.t.Fatal(err)
		}
		last = c.read()
		if reached(last) {
			return last, result
		}
	}
	c.t.Fatalf("at %s the claimant never reached %s: phase %q, operation %q, next %s, generation %d observed %d, refusal %q",
		c.clock.now.Format(time.RFC3339), what, last.phase, last.operation, formatDeadline(last.next),
		last.generation, last.observed, last.refusal)
	return last, ctrl.Result{}
}

// finishResolve leaves the claimant where a harvested Resolve leaves it: the
// cycle handed on to Verify, no operation, and the deadline the Resolve claim
// set kept for the next refresh (handoffDeadline keeps a later deadline).
func (c *realmClaimant) finishResolve() {
	c.t.Helper()

	key := client.ObjectKeyFromObject(c.object)
	switch c.object.(type) {
	case *operatorv1alpha1.PtahSchema:
		schema := &operatorv1alpha1.PtahSchema{}
		if err := c.api.Get(context.Background(), key, schema); err != nil {
			c.t.Fatal(err)
		}
		schema.Status.ActiveOperation = nil
		schema.Status.Phase = operatorv1alpha1.PhaseVerifying
		if err := c.api.Status().Update(context.Background(), schema); err != nil {
			c.t.Fatal(err)
		}
	case *operatorv1alpha1.PtahMigration:
		migration := &operatorv1alpha1.PtahMigration{}
		if err := c.api.Get(context.Background(), key, migration); err != nil {
			c.t.Fatal(err)
		}
		migration.Status.ActiveOperation = nil
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseVerifying
		if err := c.api.Status().Update(context.Background(), migration); err != nil {
			c.t.Fatal(err)
		}
	}
}

// declareShared sets spec.target.sharedRealm on object, with the generation
// bump an API server makes for a spec write.
func declareShared(t *testing.T, api client.Client, object client.Object) {
	t.Helper()

	key := client.ObjectKeyFromObject(object)
	switch object.(type) {
	case *operatorv1alpha1.PtahSchema:
		schema := &operatorv1alpha1.PtahSchema{}
		if err := api.Get(context.Background(), key, schema); err != nil {
			t.Fatal(err)
		}
		schema.Spec.Target.SharedRealm = true
		schema.Generation++
		if err := api.Update(context.Background(), schema); err != nil {
			t.Fatal(err)
		}
	case *operatorv1alpha1.PtahMigration:
		migration := &operatorv1alpha1.PtahMigration{}
		if err := api.Get(context.Background(), key, migration); err != nil {
			t.Fatal(err)
		}
		migration.Spec.Target.SharedRealm = true
		migration.Generation++
		if err := api.Update(context.Background(), migration); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown claimant %T", object)
	}
}

// A refusal can start in the middle of a cycle. A resource that was briefly
// the only claimant of its database claims a Resolve, and that claim sets the
// next refresh an interval away; the census refuses the step after it, once
// the second claimant is in the cache. The refusal still has to look again
// within a minute. The Blocked branch waits for the persisted deadline, and
// the declaration that ends the conflict is another resource's edit, which
// wakes nothing here.
//
// The refusal used to keep any deadline that had not passed. The first
// claimant to declare then stayed Blocked for the rest of its hourly interval
// after the last one declared. That is how the shared-alias acceptance row
// timed out on one Kubernetes minor: whether the first resource claims a
// Resolve alone is a race with the creation of the second.
func TestARefusalThatStartsMidCycleLooksAgainWithinAMinute(t *testing.T) {
	t.Parallel()

	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			t.Parallel()

			first := newRealmClaimant(t, family, time.Hour)

			// The only claimant starts its cycle, and the claim sets the next
			// refresh an interval away.
			claimedAt := first.clock.now
			claimed, _ := first.settle("a Resolve claim", func(reading claimantReading) bool {
				return reading.operation == "Resolve"
			})
			if claimed.next == nil || claimed.next.Time.Before(claimedAt.Add(time.Hour)) {
				t.Fatalf("the Resolve claim set the next refresh at %s, want an interval after %s",
					formatDeadline(claimed.next), claimedAt.Format(time.RFC3339))
			}
			first.finishResolve()

			// The second claimant arrives undeclared, and the next step of the
			// cycle is refused.
			first.clock.now = claimedAt.Add(2 * time.Minute)
			second := realmSchemaFixture(first.object.GetNamespace(), "second", first.target().CoordinationKey, false)
			if err := first.api.Create(context.Background(), second); err != nil {
				t.Fatal(err)
			}
			refused, result := first.settle("a realm refusal", func(reading claimantReading) bool {
				return reading.phase == "Blocked"
			})
			assertRealmRecheckBounded(t, "the refusal", first.clock.now, refused, result)

			// One declaration is not enough: the first claimant declares and
			// is refused again on its own generation.
			first.clock.now = first.clock.now.Add(10 * time.Second)
			declareShared(t, first.api, first.object)
			refusedAgain, result := first.settle("a refusal of its own declaration", func(reading claimantReading) bool {
				return reading.phase == "Blocked" && reading.observed == reading.generation && reading.generation > 1
			})
			assertRealmRecheckBounded(t, "the refusal after its own declaration", first.clock.now, refusedAgain, result)

			// The second claimant declares, and nothing wakes the first. It
			// leaves Blocked within a minute of that declaration.
			declaredAt := first.clock.now
			declareShared(t, first.api, second)
			first.clock.now = declaredAt.Add(realmRecheckCeiling)
			if _, err := first.reconcile(); err != nil {
				t.Fatal(err)
			}
			lifted := first.read()
			if lifted.operation != "Resolve" {
				t.Fatalf("a minute after the last claimant declared the realm shared, the first is still %q "+
					"(operation %q, next %s, refusal %q)",
					lifted.phase, lifted.operation, formatDeadline(lifted.next), lifted.refusal)
			}
		})
	}
}

// assertRealmRecheckBounded holds one refusal to the documented re-check: the
// persisted deadline and the requeue both within realmRecheckCeiling of now.
func assertRealmRecheckBounded(t *testing.T, what string, now time.Time, reading claimantReading, result ctrl.Result) {
	t.Helper()

	if reading.refusal != string(operatorv1alpha1.ReasonRealmConflict) {
		t.Fatalf("%s: reason %q, want %s", what, reading.refusal, operatorv1alpha1.ReasonRealmConflict)
	}
	if reading.next == nil {
		t.Fatalf("%s at %s left no time to look again", what, now.Format(time.RFC3339))
	}
	if reading.next.Time.After(now.Add(realmRecheckCeiling)) {
		t.Fatalf("%s at %s looks again at %s, later than %s after it",
			what, now.Format(time.RFC3339), formatDeadline(reading.next), realmRecheckCeiling)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > realmRecheckCeiling {
		t.Fatalf("%s requeued after %s, want within %s", what, result.RequeueAfter, realmRecheckCeiling)
	}
}

func formatDeadline(next *metav1.Time) string {
	if next == nil {
		return "none"
	}
	return next.UTC().Format(time.RFC3339)
}

// The deadline a refusal persists is never further away than the shorter of
// the interval and realmRecheckCeiling, and a deadline within that bound is
// kept, so a standing refusal writes the same status on every pass.
func TestRealmBlockDeadline(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 1, 22, 10, 0, 0, time.UTC)
	at := func(offset time.Duration) *metav1.Time {
		value := metav1.NewTime(now.Add(offset))
		return &value
	}
	for _, row := range []struct {
		name     string
		current  *metav1.Time
		interval time.Duration
		want     time.Time
	}{
		{name: "no deadline yet", interval: time.Hour, want: now.Add(realmRecheckCeiling)},
		{name: "a deadline that has passed", current: at(-time.Second), interval: time.Hour, want: now.Add(realmRecheckCeiling)},
		{name: "a deadline due now", current: at(0), interval: time.Hour, want: now.Add(realmRecheckCeiling)},
		{name: "the refusal's own deadline", current: at(30 * time.Second), interval: time.Hour, want: now.Add(30 * time.Second)},
		{name: "a deadline exactly at the ceiling", current: at(realmRecheckCeiling), interval: time.Hour, want: now.Add(realmRecheckCeiling)},
		{name: "a cycle's next refresh an interval away", current: at(time.Hour), interval: time.Hour, want: now.Add(realmRecheckCeiling)},
		{name: "a slotted refresh half an interval later", current: at(90 * time.Minute), interval: time.Hour, want: now.Add(realmRecheckCeiling)},
		{name: "a deadline just past the ceiling", current: at(realmRecheckCeiling + time.Second), interval: time.Hour, want: now.Add(realmRecheckCeiling)},
		{name: "an interval shorter than the ceiling", current: at(45 * time.Second), interval: 20 * time.Second, want: now.Add(20 * time.Second)},
		{name: "a deadline within a short interval", current: at(10 * time.Second), interval: 20 * time.Second, want: now.Add(10 * time.Second)},
		{name: "no interval", interval: 0, want: now.Add(realmRecheckCeiling)},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			got := realmBlockDeadline(row.current, now, row.interval)
			if !got.Time.Equal(row.want) {
				t.Fatalf("realmBlockDeadline(%v, now, %s) = %s, want %s",
					row.current, row.interval, got.Time.Format(time.RFC3339), row.want.Format(time.RFC3339))
			}
		})
	}
}
