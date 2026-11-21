package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// readRefusal is a Blocked a read of a resource's own source or history ends in,
// with the status the reconciler stores for it, from the documents
// internal/controller holds to what the reconciler writes, at generation 4.
type readRefusal struct {
	family, reason string
	conditions     []metav1.Condition
	// history is the reading a history read records in the status write that
	// stops it, and nil for a refusal that comes before the history is read.
	history *operatorv1alpha1.MigrationHistoryStatus
	// says is the start of the words the refusal's message carries.
	says string
}

func readRefusals(t *testing.T) []readRefusal {
	t.Helper()
	var refusals []readRefusal
	for _, file := range []struct{ family, reason, name string }{
		{"schema", "SchemaSourceUnreadable", "schema-source-unreadable.json"},
		{"schema", "SchemaInvalid", "schema-invalid.json"},
		{"migration", "MigrationDirectoryUnreadable", "migration-directory-unreadable.json"},
		{"migration", "RepeatableMigrationUnfinished", "migration-repeatable-unfinished.json"},
	} {
		data, err := os.ReadFile(filepath.Join("..", "..", "internal", "controller", "testdata", file.name))
		if err != nil {
			t.Fatal(err)
		}
		var stored struct {
			Status struct {
				Conditions []metav1.Condition                       `json:"conditions"`
				History    *operatorv1alpha1.MigrationHistoryStatus `json:"history"`
			} `json:"status"`
		}
		if err := json.Unmarshal(data, &stored); err != nil {
			t.Fatal(err)
		}
		refusal := readRefusal{family: file.family, reason: file.reason, conditions: stored.Status.Conditions, history: stored.Status.History}
		if (file.reason == "RepeatableMigrationUnfinished") != (refusal.history != nil) {
			t.Fatalf("%s records history %#v: a history read records one beside its refusal, and only that read", file.name, refusal.history)
		}
		carrier := operatorv1alpha1.ConditionReady
		if file.family == "migration" {
			carrier = operatorv1alpha1.ConditionMigrationBlocked
		}
		for index := range refusal.conditions {
			refusal.conditions[index].ObservedGeneration = 4
		}
		said := meta.FindStatusCondition(refusal.conditions, carrier)
		if said == nil || said.Reason != file.reason || len(said.Message) < 40 {
			t.Fatalf("%s carries no %s refusal on %s: %#v", file.name, file.reason, carrier, said)
		}
		refusal.says = said.Message[:40]
		refusals = append(refusals, refusal)
	}
	return refusals
}

// told is the condition every claim of a read rewrites, which tells the refusal:
// a schema's Ready, a migration's Progressing.
func (r readRefusal) told() string {
	if r.family == "migration" {
		return operatorv1alpha1.ConditionMigrationProgressing
	}
	return operatorv1alpha1.ConditionReady
}

// blockedOn is the resource at rest in Blocked on the refusal, stamped at: a
// schema by status.lastAttemptTime, a migration by Progressing's
// lastTransitionTime.
func (r readRefusal) blockedOn(at metav1.Time) client.Object {
	object := resting(r.family)
	r.stopsAt(object, at)
	return object
}

// stopsAt is the status write that ends a read in the refusal, at at. A history
// read records its reading in that write, at the same instant.
func (r readRefusal) stopsAt(object client.Object, at metav1.Time) {
	conditions := slices.Clone(r.conditions)
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		typed.Status.Phase, typed.Status.ActiveOperation, typed.Status.Conditions = operatorv1alpha1.PhaseBlocked, nil, conditions
		typed.Status.LastAttemptTime = &at
	case *operatorv1alpha1.PtahMigration:
		typed.Status.Phase, typed.Status.ActiveOperation, typed.Status.Conditions = operatorv1alpha1.MigrationPhaseBlocked, nil, conditions
		meta.FindStatusCondition(typed.Status.Conditions, r.told()).LastTransitionTime = at
		if r.history != nil {
			typed.Status.History = r.history.DeepCopy()
			typed.Status.History.ObservedAt = at
		}
	}
}

// takes is the operator taking the request.
func (r readRefusal) takes(object client.Object) {
	token := operatorv1alpha1.ReadRequestToken(object.GetAnnotations()[operatorv1alpha1.ReadRequestAnnotation])
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		typed.Status.LastReadRequest = token
	case *operatorv1alpha1.PtahMigration:
		typed.Status.LastReadRequest = token
	}
}

// claimsTheRead is the claim of the read the request starts, which rewrites the
// condition that tells the refusal.
func (r readRefusal) claimsTheRead(object client.Object) {
	claimed := metav1.Condition{Type: r.told(), Status: metav1.ConditionFalse, Reason: "OperationInProgress",
		Message: "Resolve operation is in progress", ObservedGeneration: 4, LastTransitionTime: metav1.NewTime(observedAt)}
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		typed.Status.Phase = operatorv1alpha1.PhaseResolving
		typed.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{Type: "Resolve", ID: "read", JobName: "job"}
		meta.SetStatusCondition(&typed.Status.Conditions, claimed)
	case *operatorv1alpha1.PtahMigration:
		typed.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
		typed.Status.ActiveOperation = &operatorv1alpha1.MigrationOperationStatus{Type: "Resolve", ID: "read", JobName: "job", Attempt: 1}
		claimed.Status = metav1.ConditionTrue
		meta.SetStatusCondition(&typed.Status.Conditions, claimed)
	}
}

// refresh --wait on a resource stopped on its own source or history. The read it
// asked for ends there again, under a stamp the refusal it began on did not carry,
// and the wait stops with exit status 8, saying the read ended Blocked and naming
// the reason, the condition's words and the read request that reads again. A
// request the operator took whose read has not ended leaves the wait to its
// timeout, which names the refusal it began on. kubectl ptah wait on the resource
// already stopped stops on the second poll that finds it.
func TestRefreshWaitStopsOnTheReadRefusalItsReadEndedIn(t *testing.T) {
	earlier, later := metav1.NewTime(observedAt.Add(-time.Hour)), metav1.NewTime(readAgainAt)
	for _, refusal := range readRefusals(t) {
		t.Run(refusal.family+"/"+refusal.reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := newCluster(t, refusal.blockedOn(earlier))
				api.then(func(object client.Object) { refusal.takes(object); refusal.claimsTheRead(object) },
					func(object client.Object) { refusal.stopsAt(object, later) })

				code, stdout, stderr := runCommand(t, api, "refresh", refusal.family+"/storefront", "--wait")

				if code != exitWillNotReach {
					t.Fatalf("refresh --wait on a read that ended in %s = %d, stdout %q, stderr %q", refusal.reason, code, stdout, stderr)
				}
				requireSays(t, "the stop", stderr, "storefront in namespace demo: the read ended Blocked: "+refusal.reason+".\n",
					"Phase:       Blocked", "Why:         "+refusal.reason+": "+refusal.says, "(kubectl ptah refresh)", "shows where it stands.")
			})
			synctest.Test(t, func(t *testing.T) {
				api := newCluster(t, refusal.blockedOn(earlier))
				api.then(refusal.takes)

				code, stdout, stderr := runCommand(t, api, "refresh", refusal.family+"/storefront", "--wait", "--wait-timeout", "1m")

				if code != exitTimedOut || strings.Contains(stderr, "ended Blocked") {
					t.Fatalf("refresh --wait on the refusal it began on = %d, stdout %q, stderr %q", code, stdout, stderr)
				}
				requireSays(t, "the timeout", stderr, "did not reach read within 1m0s.", refusal.reason)
			})
			synctest.Test(t, func(t *testing.T) {
				api := newCluster(t, refusal.blockedOn(earlier))

				code, stdout, stderr := runCommand(t, api, "wait", refusal.family+"/storefront", "--for", "in-sync", "--timeout", "1m")

				if code != exitWillNotReach || stdout != "" {
					t.Fatalf("wait on a resource stopped on %s = %d, stdout %q, stderr %q", refusal.reason, code, stdout, stderr)
				}
				requireSays(t, "the stop", stderr, "will not reach in-sync: Blocked.\n", "Phase:       Blocked",
					"Why:         "+refusal.reason+": "+refusal.says)
			})
		})
	}
}

// readBlockedGoldenDirectory holds what the plugin prints when a wait stops on a
// Blocked. The predicate the e2e migration phases hold the plugin's stop to
// (waitStoppedOnUnfinishedRepeatable in test/e2e) is tested against these files,
// so it cannot pass against text the plugin never prints.
const readBlockedGoldenDirectory = "testdata/read-blocked"

// The stops refresh --wait and wait --for in-sync print on a migration the
// record of an unfinished repeatable migration stops, kept in
// testdata/read-blocked.
func TestTheReadBlockedStopIsWhatThePluginPrints(t *testing.T) {
	var unfinished readRefusal
	for _, refusal := range readRefusals(t) {
		if refusal.reason == "RepeatableMigrationUnfinished" {
			unfinished = refusal
		}
	}
	if unfinished.reason == "" {
		t.Fatal("no reading of an unfinished repeatable migration")
	}
	if *updateGolden {
		if err := os.MkdirAll(readBlockedGoldenDirectory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	earlier, later := metav1.NewTime(observedAt.Add(-time.Hour)), metav1.NewTime(readAgainAt)
	for _, test := range []struct {
		name      string
		arguments []string
		steps     []func(client.Object)
	}{
		{
			name:      "refresh-repeatable-unfinished",
			arguments: []string{"refresh", "migration/storefront", "--wait"},
			steps: []func(client.Object){
				func(object client.Object) { unfinished.takes(object); unfinished.claimsTheRead(object) },
				func(object client.Object) { unfinished.stopsAt(object, later) },
			},
		},
		{
			name:      "wait-repeatable-unfinished",
			arguments: []string{"wait", "migration/storefront", "--for", "in-sync"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr string
			synctest.Test(t, func(t *testing.T) {
				api := newCluster(t, unfinished.blockedOn(earlier))
				api.then(test.steps...)
				var code int
				code, _, stderr = runCommand(t, api, test.arguments...)
				if code != exitWillNotReach {
					t.Fatalf("%s = %d, stderr %q", strings.Join(test.arguments, " "), code, stderr)
				}
			})
			path := filepath.Join(readBlockedGoldenDirectory, test.name+".txt")
			if *updateGolden {
				if err := os.WriteFile(path, []byte(stderr), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			golden, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read the golden stop (run go test -update to write it): %v", err)
			}
			if stderr != string(golden) {
				t.Fatalf("%s printed what %s does not hold:\n%s", strings.Join(test.arguments, " "), path, stderr)
			}
		})
	}
}

// readEnding is a Blocked a read ends in beside the reading it recorded, as the
// reconciler writes it (recordMigrationHistory, the end of a Plan with nothing to
// publish, setPlanPolicyStatus and schemaRealmBlocked in internal/controller).
type readEnding struct {
	name, family, reason, says string
	// never is spec.policy.apply Never.
	never bool
	// stops is a read whose end exits 8, and ends the start of what the stop
	// says ends it.
	stops bool
	ends  string
}

func (r readEnding) conditions() []metav1.Condition {
	if r.family == "migration" {
		return []metav1.Condition{
			{Type: operatorv1alpha1.ConditionMigrationBlocked, Status: metav1.ConditionTrue, Reason: r.reason, Message: r.says, ObservedGeneration: 4},
			{Type: operatorv1alpha1.ConditionMigrationReady, Status: metav1.ConditionFalse, Reason: r.reason, Message: "The migration is blocked", ObservedGeneration: 4},
			{Type: operatorv1alpha1.ConditionMigrationProgressing, Status: metav1.ConditionFalse, Reason: r.reason, Message: "Nothing runs from here",
				ObservedGeneration: 4, LastTransitionTime: metav1.NewTime(readAgainAt)},
		}
	}
	return []metav1.Condition{
		{Type: operatorv1alpha1.ConditionInSync, Status: metav1.ConditionFalse, Reason: "ScopedChanges", Message: "The managed scope differs", ObservedGeneration: 4},
		{Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: r.reason, Message: r.says, ObservedGeneration: 4},
	}
}

// endsWithAReading is the status write that ends the read: the Blocked, and the
// reading the read recorded beside it, at readAgainAt.
func (r readEnding) endsWithAReading(object client.Object) {
	read := metav1.NewTime(readAgainAt)
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		typed.Status.Phase, typed.Status.ActiveOperation, typed.Status.Conditions = operatorv1alpha1.PhaseBlocked, nil, r.conditions()
		typed.Status.LastAttemptTime, typed.Status.Target.LastObservedAt = &read, &read
	case *operatorv1alpha1.PtahMigration:
		typed.Status.Phase, typed.Status.ActiveOperation, typed.Status.Conditions = operatorv1alpha1.MigrationPhaseBlocked, nil, r.conditions()
		typed.Status.History.ObservedAt = read
	}
}

// refresh --wait exits 8 on every Blocked its read ends in that waits for a
// person, whatever the reason, a history verdict and a reason this release has no
// words for included, and 0 on one that waits for no person: a plan recorded
// under spec.policy.apply Never, which is what that policy asks for, and a realm's
// refusal, which another object ends. Each read ends beside the reading it
// recorded, as a history read or a schema's Plan does.
func TestRefreshWaitExitsByTheBlockedItsReadEndedIn(t *testing.T) {
	const otherwise = "A change to the spec, its source or the database ends it"
	for _, ending := range []readEnding{
		{name: "a dirty revision", family: "migration", reason: "HistoryDirty", stops: true,
			says: "Revision 3 is recorded dirty; a person has to decide what the interrupted run did", ends: "Repairing the dirty revision row ends it"},
		{name: "a database ahead of the artifact", family: "migration", reason: "HistoryAhead", stops: true,
			says: "The database is at version 5 and this artifact ends at version 3", ends: "An artifact that carries the versions the database applied ends it"},
		{name: "a destructive plan the policy holds", family: "schema", reason: "PolicyBlocked", stops: true,
			says: "Plan is blocked by destructive-change policy", ends: "A desired schema whose plan destroys nothing"},
		{name: "a reason this release has no words for", family: "schema", reason: "PlanRefusedByALaterRelease", stops: true,
			says: "plan_refused: Ptah refused to plan the change", ends: otherwise},
		{name: "a plan recorded under apply Never", family: "schema", reason: "ApplyDisabled", never: true,
			says: "Plan is ready but apply is disabled"},
		{name: "a destructive plan under apply Never", family: "schema", reason: "PolicyBlocked", never: true,
			says: "Plan is blocked by destructive-change policy"},
		{name: "a realm's refusal", family: "schema", reason: "RealmConflict",
			says: "This database realm is claimed by 2 resources"},
	} {
		t.Run(ending.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := resting(ending.family)
				if ending.never {
					start.(*operatorv1alpha1.PtahSchema).Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyNever
				}
				claims := readRefusal{family: ending.family}
				api := newCluster(t, start)
				api.then(func(object client.Object) { claims.takes(object); claims.claimsTheRead(object) }, ending.endsWithAReading)

				code, stdout, stderr := runCommand(t, api, "refresh", ending.family+"/storefront", "--wait", "--wait-timeout", "1m")

				if !ending.stops {
					if code != 0 || strings.Contains(stderr, "Blocked") {
						t.Fatalf("refresh --wait on a read that ended in %s = %d, stdout %q, stderr %q, want 0", ending.reason, code, stdout, stderr)
					}
					return
				}
				if code != exitWillNotReach {
					t.Fatalf("refresh --wait on a read that ended in %s = %d, stdout %q, stderr %q, want %d",
						ending.reason, code, stdout, stderr, exitWillNotReach)
				}
				requireSays(t, "the stop", stderr, "storefront in namespace demo: the read ended Blocked: "+ending.reason+".\n",
					"Why:         "+ending.reason+": "+ending.says, ending.ends)
			})
		})
	}
}

// unknownRunAt is a migration whose Apply ended Unknown, as the Apply's write
// leaves it: at rest in Blocked with ApplyOutcomeUnknown beside the reading from
// before the run, at observedAt, and the run's record, dated after.
func unknownRunAt() *operatorv1alpha1.PtahMigration {
	migration := resting("migration").(*operatorv1alpha1.PtahMigration)
	ended := metav1.NewTime(observedAt.Add(2 * time.Minute))
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
	migration.Status.Conditions = []metav1.Condition{
		{Type: operatorv1alpha1.ConditionMigrationBlocked, Status: metav1.ConditionTrue, Reason: "ApplyOutcomeUnknown",
			Message: "no result frame was read", ObservedGeneration: 4, LastTransitionTime: ended},
		{Type: operatorv1alpha1.ConditionMigrationReady, Status: metav1.ConditionFalse, Reason: "ApplyOutcomeUnknown",
			Message: "The database has to be read by a person before anything else runs", ObservedGeneration: 4, LastTransitionTime: ended},
		{Type: operatorv1alpha1.ConditionMigrationProgressing, Status: metav1.ConditionFalse, Reason: "ApplyOutcomeUnknown",
			Message: "The run stopped and may not be retried", ObservedGeneration: 4, LastTransitionTime: ended},
	}
	migration.Status.UnresolvedRun = &operatorv1alpha1.UnresolvedMigrationRunStatus{
		Outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, OperationID: "op-a", JobName: "run-a",
		PlanRef: operatorv1alpha1.ImmutableObjectReference{Name: "plan-a", UID: "plan-a-uid"}, RecordedAt: ended,
	}
	return migration
}

// settlesTheRun is the history read that finds nothing pending in the run's
// database: it removes the record and the resource is in sync.
func settlesTheRun(object client.Object) {
	migration := object.(*operatorv1alpha1.PtahMigration)
	read := metav1.NewTime(readAgainAt)
	migration.Status.Phase, migration.Status.ActiveOperation = operatorv1alpha1.MigrationPhaseInSync, nil
	migration.Status.History.ObservedAt = read
	migration.Status.ResolvedRun = &operatorv1alpha1.ResolvedMigrationRunStatus{
		OperationID: "op-a", Outcome: operatorv1alpha1.MigrationRunOutcomeUnknown,
		Resolution: operatorv1alpha1.MigrationRunResolvedByHistoryRead, ResolvedAt: read,
	}
	migration.Status.UnresolvedRun = nil
	migration.Status.Conditions = []metav1.Condition{
		{Type: operatorv1alpha1.ConditionMigrationBlocked, Status: metav1.ConditionFalse, Reason: "HistoryMatched",
			Message: "The history continues this artifact", ObservedGeneration: 4, LastTransitionTime: read},
		{Type: operatorv1alpha1.ConditionMigrationReady, Status: metav1.ConditionTrue, Reason: "HistoryMatched",
			Message: "The database has every migration this artifact carries", ObservedGeneration: 4, LastTransitionTime: read},
		{Type: operatorv1alpha1.ConditionMigrationProgressing, Status: metav1.ConditionFalse, Reason: "HistoryMatched",
			Message: "Nothing is pending", ObservedGeneration: 4, LastTransitionTime: read},
	}
}

// leavesTheRunStanding is the history read that finds work pending: the record
// stands, and so does ApplyOutcomeUnknown, beside the reading.
func leavesTheRunStanding(object client.Object) {
	migration := object.(*operatorv1alpha1.PtahMigration)
	read := metav1.NewTime(readAgainAt)
	migration.Status.Phase, migration.Status.ActiveOperation = operatorv1alpha1.MigrationPhaseBlocked, nil
	migration.Status.History.ObservedAt = read
	migration.Status.Conditions = []metav1.Condition{
		{Type: operatorv1alpha1.ConditionMigrationBlocked, Status: metav1.ConditionTrue, Reason: "ApplyOutcomeUnknown",
			Message: unknownRunPending, ObservedGeneration: 4, LastTransitionTime: read},
		{Type: operatorv1alpha1.ConditionMigrationReady, Status: metav1.ConditionFalse, Reason: "ApplyOutcomeUnknown",
			Message: "A person has to account for the run before anything else runs", ObservedGeneration: 4, LastTransitionTime: read},
		{Type: operatorv1alpha1.ConditionMigrationProgressing, Status: metav1.ConditionFalse, Reason: "ApplyOutcomeUnknown",
			Message: "The dispatched run is over and may not be retried", ObservedGeneration: 4, LastTransitionTime: read},
	}
}

const unknownRunPending = "1 migration is pending and the last run's outcome was Unknown, so it may not run again"

// A migration whose Apply ended Unknown rests in Blocked until the operator reads
// its database again, and that reading settles the run when nothing is pending
// there. So refresh --wait exits 0 when its read settles the run and 8 when it
// leaves the run standing, and a plain wait for in-sync waits through the Blocked
// from before the reading, however many polls find it, and exits as the reading
// decides.
func TestAWaitOnAnUnknownRunExitsAsTheReadingAfterItDecides(t *testing.T) {
	noChange := func(client.Object) {}
	for _, row := range []struct {
		name  string
		reads func(client.Object)
		stops bool
	}{
		{name: "nothing pending", reads: settlesTheRun},
		{name: "work pending", reads: leavesTheRunStanding, stops: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				claims := readRefusal{family: "migration"}
				api := newCluster(t, unknownRunAt())
				api.then(func(object client.Object) { claims.takes(object); claims.claimsTheRead(object) }, row.reads)

				code, stdout, stderr := runCommand(t, api, "refresh", "migration/storefront", "--wait", "--wait-timeout", "1m")

				if !row.stops {
					if code != 0 || strings.Contains(stderr, "Blocked") {
						t.Fatalf("refresh --wait on a read that settled the run = %d, stdout %q, stderr %q, want 0", code, stdout, stderr)
					}
					return
				}
				if code != exitWillNotReach {
					t.Fatalf("refresh --wait on a read that left the run standing = %d, stdout %q, stderr %q, want %d",
						code, stdout, stderr, exitWillNotReach)
				}
				requireSays(t, "the stop", stderr, "storefront in namespace demo: the read ended Blocked: ApplyOutcomeUnknown.\n",
					"Why:         ApplyOutcomeUnknown: "+unknownRunPending, "A reading has followed the run and did not settle it.")
			})
			synctest.Test(t, func(t *testing.T) {
				api := newCluster(t, unknownRunAt())
				api.armed = true
				api.then(noChange, noChange, noChange, row.reads)

				code, stdout, stderr := runCommand(t, api, "wait", "migration/storefront", "--for", "in-sync", "--timeout", "1m")

				if !row.stops {
					if code != 0 || strings.Contains(stderr, "Blocked") {
						t.Fatalf("wait --for in-sync on a run the reading settled = %d, stdout %q, stderr %q, want 0", code, stdout, stderr)
					}
					return
				}
				if code != exitWillNotReach {
					t.Fatalf("wait --for in-sync on a run the reading left standing = %d, stdout %q, stderr %q, want %d",
						code, stdout, stderr, exitWillNotReach)
				}
				requireSays(t, "the stop", stderr, "will not reach in-sync: Blocked.\n", "Why:         ApplyOutcomeUnknown: "+unknownRunPending)
			})
		})
	}
}
