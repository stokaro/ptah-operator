package operate_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
)

// blockedKind is what a Blocked does to a wait.
type blockedKind int

const (
	// blockedStops is a Blocked a person's change ends: it stops every wait.
	blockedStops blockedKind = iota
	// blockedWaitsForNoPerson is a Blocked no change of this resource ends, or no
	// refusal at all: a realm's, or a plan held under spec.policy.apply Never. It
	// stops no wait.
	blockedWaitsForNoPerson
	// blockedEngine is an engine the operator does not run, under which it takes
	// no read request.
	blockedEngine
	// blockedAfterAReading is a migration run nobody accounted for, which the
	// operator's next reading settles when nothing is pending: it stops a wait
	// once a reading has followed its record
	// (TestARunNobodyAccountedForStopsAWaitOnceAReadingFollowsIt).
	blockedAfterAReading
)

// blockedReading is a resource at rest in Blocked: the conditions the reconciler
// stores for it, at generation, and what a stop on it has to say.
type blockedReading struct {
	family     operate.Family
	reason     string
	generation int64
	conditions []metav1.Condition
	applyNever bool
	kind       blockedKind
	// unresolved is the run record a migration's Blocked stands beside.
	unresolved *unresolvedDoc
	// says is the condition's words as status carries them: a schema's Ready, a
	// migration's Blocked.
	says string
	// ends is the start of what the stop says ends it.
	ends string
	// stored is a document internal/controller holds to what the reconciler
	// writes (TestTheRefusalReadingIsWhatTheReconcilerStores,
	// TestTheDirectoryRefusalReadingIsWhatTheReconcilerStores and
	// TestTheRepeatableReadingsAreWhatTheReconcilerStores there), and not
	// conditions written here after the reconciler's.
	stored bool
}

// blockedReadings is every Blocked the rows hold a wait to: five from the
// reconciler's documents, and the rest as the reconciler writes them
// (recordMigrationHistory, the end of a Plan with nothing to publish,
// setPlanPolicyStatus, schemaRealmBlocked, migrationRealmBlocked and the engine
// checks in internal/controller), with a reason no release writes yet, which the
// rule has to hold without an entry of its own.
func blockedReadings(t *testing.T) []blockedReading {
	t.Helper()
	var readings []blockedReading
	for _, file := range []struct {
		family operate.Family
		reason operatorv1alpha1.ConditionReason
		name   string
		ends   string
	}{
		{operate.FamilySchema, operatorv1alpha1.ReasonSchemaSourceUnreadable, "schema-source-unreadable.json", "A file Ptah can read ends it"},
		{operate.FamilySchema, operatorv1alpha1.ReasonSchemaInvalid, "schema-invalid.json", "A schema Ptah accepts ends it"},
		{operate.FamilySchema, operatorv1alpha1.ReasonPlanRefused, "schema-plan-refused.json", "A desired schema that states what the refusal names"},
		{operate.FamilyMigration, operatorv1alpha1.ReasonMigrationDirectoryUnreadable, "migration-directory-unreadable.json", "A directory Ptah can read ends it"},
		{operate.FamilyMigration, operatorv1alpha1.ReasonRepeatableMigrationUnfinished, "migration-repeatable-unfinished.json", "Removing the record with the repair command ends it"},
	} {
		data, err := os.ReadFile(filepath.Join("..", "controller", "testdata", file.name))
		if err != nil {
			t.Fatal(err)
		}
		reading := blockedReading{family: file.family, reason: string(file.reason), ends: file.ends, stored: true}
		var phase string
		var told, carried *metav1.Condition
		if file.family == operate.FamilySchema {
			stored := &operatorv1alpha1.PtahSchema{}
			if err := json.Unmarshal(data, stored); err != nil {
				t.Fatal(err)
			}
			phase, reading.generation, reading.conditions = string(stored.Status.Phase), stored.Generation, stored.Status.Conditions
			told = meta.FindStatusCondition(reading.conditions, operatorv1alpha1.ConditionReady)
			carried = told
		} else {
			stored := &operatorv1alpha1.PtahMigration{}
			if err := json.Unmarshal(data, stored); err != nil {
				t.Fatal(err)
			}
			phase, reading.generation, reading.conditions = string(stored.Status.Phase), stored.Generation, stored.Status.Conditions
			told = meta.FindStatusCondition(reading.conditions, operatorv1alpha1.ConditionMigrationProgressing)
			carried = meta.FindStatusCondition(reading.conditions, operatorv1alpha1.ConditionMigrationBlocked)
		}
		if phase != "Blocked" || told == nil || told.Status != metav1.ConditionFalse || told.Reason != reading.reason ||
			carried == nil || carried.Reason != reading.reason || carried.Message == "" {
			t.Fatalf("%s is not a %s refusal at rest: phase %s, %#v, %#v", file.name, reading.reason, phase, told, carried)
		}
		reading.says = carried.Message
		readings = append(readings, reading)
	}
	const otherwise = "A change to the spec, its source or the database ends it"
	return append(readings,
		migrationBlocked(operatorv1alpha1.ReasonHistoryDirty, "Revision 3 is recorded dirty; a person has to decide what the interrupted run did",
			"Repairing the dirty revision row ends it"),
		migrationBlocked(operatorv1alpha1.ReasonHistoryAhead, "The database is at version 5 and this artifact ends at version 3; "+
			"publish an artifact that carries the versions the database already applied, because nothing here rolls it back",
			"An artifact that carries the versions the database applied ends it"),
		unknownRun(migrationBlocked(operatorv1alpha1.ReasonApplyOutcomeUnknown, "1 migration is pending and the last run's "+
			"outcome was Unknown, so it may not run again", "A reading has followed the run and did not settle it. "+
			"A reading of the run's database with nothing pending ends it")),
		schemaBlocked(operatorv1alpha1.ReasonDifferenceNotPlannable, "difference_not_plannable: generated column total changed its "+
			"expression. Nothing is planned: make the change by hand or with a migration, or change the artifact",
			"The change made by hand or with a migration"),
		schemaBlocked(operatorv1alpha1.ReasonPolicyBlocked, "Plan is blocked by destructive-change policy",
			"A desired schema whose plan destroys nothing, or spec.policy.allowDestructive, ends it"),
		schemaBlocked("PlanRefusedByALaterRelease", "plan_refused: a reason this release has no words for", otherwise),
		exempt(schemaBlocked(operatorv1alpha1.ReasonApplyDisabled, "Plan is ready but apply is disabled", ""), true),
		exempt(schemaBlocked(operatorv1alpha1.ReasonPolicyBlocked, "Plan is blocked by destructive-change policy", ""), true),
		exempt(schemaBlocked(operatorv1alpha1.ReasonDestructiveChangesDisabled, "Plan contains destructive changes and policy disallows them", ""), true),
		exempt(schemaBlocked(operatorv1alpha1.ReasonRealmConflict, "This database realm is claimed by 2 resources", ""), false),
		exempt(migrationBlocked(operatorv1alpha1.ReasonRealmNotAuthorized, "PtahRealm orders does not admit this namespace", ""), false),
		engine(schemaBlocked(operatorv1alpha1.ReasonUnsupportedEngine, `Database engine "Oracle" is not supported`, "")),
		engine(migrationBlocked(operatorv1alpha1.ReasonUnsupportedEngine, `Database engine "Oracle" is not supported by this operator`, "")),
	)
}

// schemaBlocked is a schema at rest in Blocked for reason, as a harvest or a pass
// writes it: Ready carries the reason and the words, and InSync the reason.
func schemaBlocked(reason operatorv1alpha1.ConditionReason, says, ends string) blockedReading {
	return blockedReading{
		family: operate.FamilySchema, reason: string(reason), generation: 3, says: says, ends: ends,
		conditions: []metav1.Condition{
			condition(operatorv1alpha1.ConditionInSync, metav1.ConditionFalse, string(reason), "The schema has not converged", 3),
			condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, string(reason), says, 3),
		},
	}
}

// migrationBlocked is a migration at rest in Blocked for reason: Blocked True with
// the words, and Ready and Progressing False with the reason.
func migrationBlocked(reason operatorv1alpha1.ConditionReason, says, ends string) blockedReading {
	return blockedReading{
		family: operate.FamilyMigration, reason: string(reason), generation: 3, says: says, ends: ends,
		conditions: []metav1.Condition{
			condition(operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, string(reason), says, 3),
			condition(operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, string(reason), "The migration is blocked", 3),
			condition(operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, string(reason), "Nothing runs from here", 3),
		},
	}
}

func exempt(r blockedReading, applyNever bool) blockedReading {
	r.kind, r.applyNever = blockedWaitsForNoPerson, applyNever
	return r
}

func engine(r blockedReading) blockedReading {
	r.kind = blockedEngine
	return r
}

// unknownRun is r beside the record of the run it is about, as the Apply that
// ended Unknown and each reading after it leave the resource.
func unknownRun(r blockedReading) blockedReading {
	r.kind = blockedAfterAReading
	r.unresolved = &unresolvedDoc{
		operationID: "op-a", outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, job: "run-a", plan: "plan-a",
		planUID: "plan-a-uid", recordedAt: at(100),
	}
	return r
}

// readingsOf is the readings of kind.
func readingsOf(t *testing.T, kind blockedKind) []blockedReading {
	t.Helper()
	var of []blockedReading
	for _, reading := range blockedReadings(t) {
		if reading.kind == kind {
			of = append(of, reading)
		}
	}
	if len(of) == 0 {
		t.Fatalf("no reading of kind %d", kind)
	}
	return of
}

// at is the resource at rest in Blocked, stamped at stamped.
func (r blockedReading) at(stamped time.Time) doc {
	d := doc{
		generation: r.generation, phase: "Blocked", observedAt: t0, conditions: slices.Clone(r.conditions), applyNever: r.applyNever,
		unresolved: r.unresolved,
	}
	r.stamp(&d, stamped)
	return d
}

// stamp dates the Blocked d records: a schema by status.lastAttemptTime, a
// migration by the lastTransitionTime of Progressing.
func (r blockedReading) stamp(d *doc, stamped time.Time) {
	if r.family == operate.FamilySchema {
		d.attemptedAt = stamped
		return
	}
	for index := range d.conditions {
		if d.conditions[index].Type == operatorv1alpha1.ConditionMigrationProgressing {
			d.conditions[index].LastTransitionTime = metav1.NewTime(stamped)
		}
	}
}

// claimed is the claim of the read after the Blocked, at claimedAt: a schema's
// claim takes Ready to OperationInProgress and writes lastAttemptTime, a
// migration's takes Progressing to True. A migration's Blocked and Ready keep the
// reason through the read.
func (r blockedReading) claimed(claimedAt time.Time) doc {
	d := r.at(time.Time{})
	d.phase, d.operation = "Resolving", "Resolve"
	d.conditions = r.rewrittenByClaim(claimedAt)
	if r.family == operate.FamilySchema {
		d.attemptedAt = claimedAt
	}
	return d
}

// rewrittenByClaim is the Blocked's conditions with the one the claim rewrites
// rewritten.
func (r blockedReading) rewrittenByClaim(claimedAt time.Time) []metav1.Condition {
	if r.family == operate.FamilySchema {
		return rewritten(r.conditions, metav1.Condition{
			Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionFalse, ObservedGeneration: r.generation,
			Reason: string(operatorv1alpha1.ReasonOperationInProgress), Message: "Resolve operation is in progress",
			LastTransitionTime: metav1.NewTime(claimedAt),
		})
	}
	return rewritten(r.conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionMigrationProgressing, Status: metav1.ConditionTrue, ObservedGeneration: r.generation,
		Reason: string(operatorv1alpha1.ReasonOperationInProgress), Message: "Resolve operation is in progress",
		LastTransitionTime: metav1.NewTime(claimedAt),
	})
}

func (r blockedReading) name() string {
	name := string(r.family) + "/" + r.reason
	if r.applyNever {
		name += "/apply-Never"
	}
	return name
}

// assertBlockedStop holds err to a StopBlocked at read want of a wait for target,
// which names the reason, the condition's words and what ends it, and says whether
// the read a wait for a read asked for ended in it.
func assertBlockedStop(t *testing.T, err error, r blockedReading, target operate.Target, reads, want int, readEnded bool) {
	t.Helper()
	var stop *operate.StopError
	if !errors.As(err, &stop) || stop.Reason != operate.StopBlocked || stop.Phase != "Blocked" || stop.Target != target ||
		stop.Held != r.reason || stop.ReadEnded != readEnded || reads != want ||
		!strings.Contains(stop.Message, r.reason+": "+strings.TrimSuffix(r.says, ".")) ||
		(r.ends != "" && !strings.Contains(stop.Message, r.ends)) {
		t.Fatalf("err = %v (%#v) after %d reads, want a %s stop at read %d naming %s, %q and %q, with ReadEnded %t",
			err, stop, reads, operate.StopBlocked, want, r.reason, r.says, r.ends, readEnded)
	}
}

// A wait for a read stops on the Blocked the read its request asked for ended in:
// at rest in Blocked, on the condition every claim rewrites, under a stamp other
// than the one on the document the request is dated by. The stamp is compared and
// never ordered: the rows date a Blocked before the request, after it, and before
// the one it replaces, as managers whose clocks disagree do. Each row that names no
// stop or end is a document the filter has to refuse, and none of them may stop the
// wait. The rule holds every reason the reconciler rests at Blocked for, and one it
// does not write yet; a run nobody accounted for has rows of its own
// (TestARunNobodyAccountedForStopsAWaitOnceAReadingFollowsIt).
func TestAWaitForAReadStopsOnTheBlockedItsReadEndedIn(t *testing.T) {
	t.Parallel()

	// The request is at(5). A Blocked stamped earlier or later is not, for that,
	// older or newer than it.
	before, inFlight, after := at(-600), at(-60), at(120)
	for _, row := range []struct {
		name string
		// since is the baseline; nil makes the first read the baseline, as
		// kubectl ptah wait --for read does.
		since func(blockedReading) doc
		docs  func(blockedReading) []doc
		// wantStop is the read the wait stops at, and wantDone the read it is
		// done at. Zero for both expects a timeout.
		wantStop, wantDone int
	}{
		{
			name:  "the read the request started ended in it",
			since: func(r blockedReading) doc { return asked(r.at(before)) },
			docs: func(r blockedReading) []doc {
				return []doc{taken(r.at(before)), taken(r.claimed(at(10))), taken(r.at(after))}
			},
			wantStop: 3,
		},
		{
			name:     "it before the wait's first read",
			since:    func(r blockedReading) doc { return asked(r.at(before)) },
			docs:     func(r blockedReading) []doc { return []doc{taken(r.at(after))} },
			wantStop: 1,
		},
		{
			name:     "it dated before the one it replaced, by another manager's clock",
			since:    func(r blockedReading) doc { return asked(r.at(at(-600))) },
			docs:     func(r blockedReading) []doc { return []doc{taken(r.at(at(-1200)))} },
			wantStop: 1,
		},
		{
			name:  "an older one: the operator took the request and has not claimed the read",
			since: func(r blockedReading) doc { return asked(r.at(before)) },
			docs:  func(r blockedReading) []doc { return []doc{taken(r.at(before))} },
		},
		{
			name:  "an older one dated after the request by the operator's clock",
			since: func(r blockedReading) doc { return asked(r.at(at(600))) },
			docs:  func(r blockedReading) []doc { return []doc{taken(r.at(at(600)))} },
		},
		{
			name: "a wait begun after the operator took the request finds the older one standing",
			docs: func(r blockedReading) []doc { return []doc{taken(r.at(before))} },
		},
		{
			name: "and stops on the one the read after it ended in",
			docs: func(r blockedReading) []doc {
				return []doc{taken(r.at(before)), taken(r.claimed(at(10))), taken(r.at(after))}
			},
			wantStop: 3,
		},
		{
			name:  "the read is under way",
			since: func(r blockedReading) doc { return asked(r.at(before)) },
			docs:  func(r blockedReading) []doc { return []doc{taken(r.claimed(at(10)))} },
		},
		{
			name:  "a claim in flight though the phase still reads Blocked",
			since: func(r blockedReading) doc { return asked(r.at(before)) },
			docs: func(r blockedReading) []doc {
				claimed := taken(r.at(after))
				claimed.operation = "Resolve"
				return []doc{claimed}
			},
		},
		{
			name:  "the condition the claim rewrites, at rest in Blocked with the phase not yet moved",
			since: func(r blockedReading) doc { return asked(r.at(before)) },
			docs: func(r blockedReading) []doc {
				d := taken(r.at(after))
				d.conditions = r.rewrittenByClaim(after)
				return []doc{d}
			},
		},
		{
			name:  "a read under way when the request was written ends in one that does not count",
			since: func(r blockedReading) doc { return asked(r.claimed(before)) },
			docs: func(r blockedReading) []doc {
				return []doc{asked(r.at(inFlight)), taken(r.at(inFlight))}
			},
		},
		{
			name:  "the read the operator starts after it does",
			since: func(r blockedReading) doc { return asked(r.claimed(before)) },
			docs: func(r blockedReading) []doc {
				return []doc{asked(r.at(inFlight)), taken(r.at(inFlight)), taken(r.claimed(at(10))), taken(r.at(after))}
			},
			wantStop: 4,
		},
		{
			name:  "a spec edited since",
			since: func(r blockedReading) doc { return asked(r.at(before)) },
			docs: func(r blockedReading) []doc {
				edited := taken(r.at(after))
				edited.generation, edited.observedGeneration = r.generation+1, r.generation
				return []doc{edited}
			},
		},
		{
			name:  "a newer request the operator has not taken",
			since: func(r blockedReading) doc { return asked(r.at(before)) },
			docs: func(r blockedReading) []doc {
				newer := taken(r.at(after))
				newer.annotation = otherRequest
				return []doc{newer}
			},
		},
		{
			// A history read writes the reading and what it found in one status
			// write, and a schema's Plan refuses beside the reading its Observe
			// recorded, so the reading is this read's. A Blocked that came before
			// the read, a directory Ptah cannot read, reads the same when a poll
			// misses a rest: the latest read since the request ended there.
			name:  "it beside a reading recorded since the request",
			since: func(r blockedReading) doc { return asked(inSyncAt(r, before)) },
			docs: func(r blockedReading) []doc {
				return []doc{taken(r.claimed(at(10))), taken(readAt(r.at(after), at(30)))}
			},
			wantStop: 2,
		},
		{
			// The operator takes a newer request on the pass after the rest the
			// read ended in, milliseconds later, so a poll can miss that rest and
			// find the newer read's Blocked beside this read's reading.
			name:  "the read ended with a reading, and a newer request's read in it",
			since: func(r blockedReading) doc { return asked(inSyncAt(r, before)) },
			docs: func(r blockedReading) []doc {
				return []doc{taken(r.claimed(at(10))), takenNewer(readAt(r.at(after), at(30)))}
			},
			wantDone: 2,
		},
	} {
		for _, reading := range readingsOf(t, blockedStops) {
			t.Run(row.name+"/"+reading.name(), func(t *testing.T) {
				t.Parallel()

				tl := &timeline{family: reading.family, docs: row.docs(reading)}
				options := operate.WaitOptions{
					Target: operate.TargetRead, ReadRequest: request, Timeout: timeout, Interval: interval, Clock: newStepClock(),
				}
				if row.since != nil {
					options.Since = row.since(reading).object(reading.family)
				}
				_, err := operate.Wait(context.Background(), tl.reader(t), refOf(reading.family), options)

				var timedOut *operate.TimeoutError
				switch {
				case row.wantDone > 0:
					if err != nil || tl.readCount() != row.wantDone {
						t.Fatalf("err = %v after %d reads, want the read done at read %d", err, tl.readCount(), row.wantDone)
					}
				case row.wantStop > 0:
					assertBlockedStop(t, err, reading, operate.TargetRead, tl.readCount(), row.wantStop, true)
				case !errors.As(err, &timedOut):
					t.Fatalf("err = %v after %d reads, want a timeout", err, tl.readCount())
				}
			})
		}
	}
}

// A read that ends at a Blocked no person is awaited for ends as a read: a plan
// held under spec.policy.apply Never is what the policy asks for, so kubectl ptah
// refresh --wait under Never keeps exiting 0, and a realm's refusal is another
// object's to end. Where the same document under another apply policy stops the
// wait (TestAWaitForAReadStopsOnTheBlockedItsReadEndedIn), these do not.
func TestAReadThatEndsWhereNoPersonIsAwaitedStopsNoWait(t *testing.T) {
	t.Parallel()

	before, after := at(-600), at(120)
	for _, row := range []struct {
		name     string
		docs     func(blockedReading) []doc
		wantDone int
	}{
		{
			name: "the read ended there with a reading",
			docs: func(r blockedReading) []doc {
				return []doc{taken(r.claimed(at(10))), taken(readAt(r.at(after), at(30)))}
			},
			wantDone: 2,
		},
		{
			name: "the read ended there with no reading since the request",
			docs: func(r blockedReading) []doc {
				return []doc{taken(r.claimed(at(10))), taken(r.at(after))}
			},
		},
		{
			name: "it stands when the request is written",
			docs: func(r blockedReading) []doc { return []doc{asked(r.at(before))} },
		},
	} {
		for _, reading := range readingsOf(t, blockedWaitsForNoPerson) {
			t.Run(row.name+"/"+reading.name(), func(t *testing.T) {
				t.Parallel()

				tl := &timeline{family: reading.family, docs: row.docs(reading)}
				_, err := operate.Wait(context.Background(), tl.reader(t), refOf(reading.family), operate.WaitOptions{
					Target: operate.TargetRead, ReadRequest: request, Timeout: timeout, Interval: interval, Clock: newStepClock(),
					Since: asked(inSyncAt(reading, before)).object(reading.family),
				})

				var timedOut *operate.TimeoutError
				switch {
				case row.wantDone > 0:
					if err != nil || tl.readCount() != row.wantDone {
						t.Fatalf("err = %v after %d reads, want the read done at read %d", err, tl.readCount(), row.wantDone)
					}
				case !errors.As(err, &timedOut):
					t.Fatalf("err = %v after %d reads, want a timeout", err, tl.readCount())
				}
			})
		}
	}
}

// The operator checks the engine before it looks at a read request, so it takes
// none while the engine is unsupported, and a wait for a read stops on the first
// poll that finds the engine Blocked for the generation the resource has now: no
// read it could wait for begins. The headline is the plain one, since no read
// ended, also where the pass that found the engine moved the Blocked's stamp after
// the operator took the request, which tells a read's end anywhere else. A spec
// edit the operator has not observed may be the edit that ends it.
func TestAReadRequestUnderAnUnsupportedEngineStopsTheWait(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name     string
		since    func(blockedReading) doc
		docs     func(blockedReading) []doc
		wantStop int
	}{
		{
			name:     "the request the operator does not take",
			since:    func(r blockedReading) doc { return asked(r.at(at(-600))) },
			docs:     func(r blockedReading) []doc { return []doc{asked(r.at(at(-600)))} },
			wantStop: 1,
		},
		{
			// A spec edit named the engine after the operator took the request,
			// and the pass that found it wrote a Blocked under a stamp other than
			// the request's document carries.
			name:  "an engine named after the request was taken",
			since: func(r blockedReading) doc { return asked(inSyncAt(r, at(-600))) },
			docs: func(r blockedReading) []doc {
				edited := taken(r.at(at(-600)))
				edited.generation = r.generation + 1
				for index := range edited.conditions {
					edited.conditions[index].ObservedGeneration = r.generation + 1
				}
				return []doc{taken(inSyncAt(r, at(-600))), edited}
			},
			wantStop: 2,
		},
		{
			name:  "a spec edit the operator has not observed",
			since: func(r blockedReading) doc { return asked(r.at(at(-600))) },
			docs: func(r blockedReading) []doc {
				edited := asked(r.at(at(-600)))
				edited.generation, edited.observedGeneration = r.generation+1, r.generation
				return []doc{edited}
			},
		},
	} {
		for _, reading := range readingsOf(t, blockedEngine) {
			t.Run(row.name+"/"+reading.name(), func(t *testing.T) {
				t.Parallel()

				tl := &timeline{family: reading.family, docs: row.docs(reading)}
				_, err := operate.Wait(context.Background(), tl.reader(t), refOf(reading.family), operate.WaitOptions{
					Target: operate.TargetRead, ReadRequest: request, Timeout: timeout, Interval: interval, Clock: newStepClock(),
					Since: row.since(reading).object(reading.family),
				})

				var timedOut *operate.TimeoutError
				switch {
				case row.wantStop > 0:
					assertBlockedStop(t, err, reading, operate.TargetRead, tl.readCount(), row.wantStop, false)
					if !strings.Contains(err.Error(), "will not reach read: Blocked: the operator takes no read request and starts no read while the engine is unsupported") ||
						!strings.Contains(err.Error(), "A spec that names an engine this operator runs ends it") {
						t.Fatalf("err = %v, want it to say no read begins and what ends it", err)
					}
				case !errors.As(err, &timedOut):
					t.Fatalf("err = %v after %d reads, want a timeout", err, tl.readCount())
				}
			})
		}
	}
}

// Any other wait stops on a Blocked that holds: at rest in Blocked on the condition
// every claim rewrites, for the generation the resource has now, on two polls in a
// row, as a verification policy's refusal does. Each row below that names no stop
// is a document the filter has to refuse. A migration's Blocked and Ready keep the
// reason through the read after it, so neither decides anything. A Blocked no
// person is awaited for stops none of these waits, and a run nobody accounted for
// has rows of its own (TestARunNobodyAccountedForStopsAWaitOnceAReadingFollowsIt).
func TestWaitStopsOnABlockedThatHolds(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		// kinds is the kinds of Blocked the row is about, and wantStop the read
		// the wait stops at for each. A kind left out is not read; zero expects
		// a timeout.
		kinds map[blockedKind]int
		// only is the family the row is about, when one family alone can read so.
		only operate.Family
		docs func(blockedReading) []doc
	}{
		{
			name:  "one that stands when the wait begins stops it, on the second poll",
			kinds: map[blockedKind]int{blockedStops: 2, blockedEngine: 2, blockedWaitsForNoPerson: 0},
			docs:  func(r blockedReading) []doc { return []doc{r.at(at(-600))} },
		},
		{
			name:  "a request answered long ago leaves it standing",
			kinds: map[blockedKind]int{blockedStops: 2, blockedEngine: 2, blockedWaitsForNoPerson: 0},
			docs: func(r blockedReading) []doc {
				answered := r.at(at(-600))
				answered.annotation, answered.lastRead = request, request
				return []doc{answered}
			},
		},
		{
			// The operator takes no request while the engine is unsupported, so a
			// pending one says nothing; anywhere else it starts the next read.
			name:  "a read request the operator has not taken",
			kinds: map[blockedKind]int{blockedStops: 0, blockedEngine: 2},
			docs: func(r blockedReading) []doc {
				requested := r.at(at(-600))
				requested.annotation = request
				return []doc{requested}
			},
		},
		{
			name:  "one poll is not enough: the next found a claim",
			kinds: map[blockedKind]int{blockedStops: 0},
			docs:  func(r blockedReading) []doc { return []doc{r.at(at(-600)), r.claimed(at(10))} },
		},
		{
			name:  "the count starts again after a poll that found another state",
			kinds: map[blockedKind]int{blockedStops: 4},
			docs: func(r blockedReading) []doc {
				return []doc{r.at(at(-600)), r.claimed(at(10)), r.at(at(120))}
			},
		},
		{
			name:  "a spec edited since",
			kinds: map[blockedKind]int{blockedStops: 0, blockedEngine: 0},
			docs: func(r blockedReading) []doc {
				edited := r.at(at(-600))
				edited.generation, edited.observedGeneration = r.generation+1, r.generation
				return []doc{edited}
			},
		},
		{
			name:  "a claim in flight though the phase still reads Blocked",
			kinds: map[blockedKind]int{blockedStops: 0},
			docs: func(r blockedReading) []doc {
				claimed := r.at(at(-600))
				claimed.operation = "Resolve"
				return []doc{claimed}
			},
		},
		{
			name:  "a proof owed in Blocked",
			kinds: map[blockedKind]int{blockedStops: 0},
			only:  operate.FamilySchema,
			docs: func(r blockedReading) []doc {
				owed := r.at(at(-600))
				owed.proof = true
				return []doc{owed}
			},
		},
		{
			name:  "a phase that is not Blocked",
			kinds: map[blockedKind]int{blockedStops: 0},
			docs: func(r blockedReading) []doc {
				failed := r.at(at(-600))
				failed.phase = "Failed"
				return []doc{failed}
			},
		},
		{
			name:  "the reason left beside the condition the claim rewrote",
			kinds: map[blockedKind]int{blockedStops: 0},
			docs: func(r blockedReading) []doc {
				d := r.at(at(-600))
				d.conditions = r.rewrittenByClaim(at(10))
				return []doc{d}
			},
		},
	} {
		for _, reading := range blockedReadings(t) {
			wantStop, read := row.kinds[reading.kind]
			if !read || row.only != "" && row.only != reading.family {
				continue
			}
			for _, target := range []operate.Target{operate.TargetInSync, operate.TargetApproval, operate.TargetApplied} {
				t.Run(row.name+"/"+reading.name()+"/"+string(target), func(t *testing.T) {
					t.Parallel()

					tl := &timeline{family: reading.family, docs: row.docs(reading)}
					_, err := operate.Wait(context.Background(), tl.reader(t), refOf(reading.family), operate.WaitOptions{
						Target: target, Timeout: timeout, Interval: interval, Clock: newStepClock(),
					})

					var timedOut *operate.TimeoutError
					switch {
					case wantStop > 0:
						assertBlockedStop(t, err, reading, target, tl.readCount(), wantStop, false)
					case !errors.As(err, &timedOut):
						t.Fatalf("err = %v after %d reads, want a timeout", err, tl.readCount())
					}
				})
			}
		}
	}
}

// A migration run that ended Unknown or Partial rests in Blocked beside the reading
// from before it until the operator reads the database again, and that reading
// removes the record when nothing of the artifact is pending in the run's
// database. So the Blocked stops a wait only beside a reading the wait saw replace
// the one it found beside the record, and on two polls of it in a row for a wait
// that is not for a read. The times are never put in order: the record is at(100),
// and the rows date a reading that followed it before it and one that came before
// it after it, as managers whose clocks disagree do. Each row that names no stop
// or end is a document the filter has to refuse.
func TestARunNobodyAccountedForStopsAWaitOnceAReadingFollowsIt(t *testing.T) {
	t.Parallel()

	run := readingsOf(t, blockedAfterAReading)[0]
	// unread is the record beside a reading no later than the one the Apply
	// found, and read the record beside a reading recorded after it, which also
	// wrote the Blocked.
	unread := func(observedAt time.Time) doc { return readAt(run.at(at(90)), observedAt) }
	read := func(observedAt time.Time) doc { return readAt(run.at(observedAt), observedAt) }
	settled := func(observedAt time.Time) doc {
		return doc{
			generation: run.generation, phase: "InSync", observedAt: observedAt,
			conditions: inSyncConditions(operate.FamilyMigration, run.generation),
		}
	}
	// acknowledged is the write that takes a PtahMigrationRunAcknowledgment: the
	// record goes, Blocked turns False, and Progressing keeps the reason until
	// the read that follows at once.
	acknowledged := func(observedAt time.Time) doc {
		d := unread(observedAt)
		d.unresolved = nil
		d.conditions = rewritten(d.conditions, condition(operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse,
			string(operatorv1alpha1.ReasonRunAcknowledged), "jane acknowledged the Unknown run", run.generation))
		return d
	}
	// another is the record of a later run, which the Apply after a settling
	// reading wrote beside it.
	another := func(d doc) doc {
		next := *d.unresolved
		next.operationID, next.job, next.recordedAt = "op-b", "run-b", at(400)
		d.unresolved = &next
		return d
	}

	t.Run("waits", func(t *testing.T) {
		t.Parallel()
		every := []operate.Target{operate.TargetInSync, operate.TargetApproval, operate.TargetApplied}
		// A wait for applied stops on the record of a run that ended while it
		// read (StopUnresolved), before any Blocked.
		noRunEnds := []operate.Target{operate.TargetInSync, operate.TargetApproval}
		for _, row := range []struct {
			name string
			docs []doc
			// targets is the waits the row reads: every one when nil.
			targets []operate.Target
			// wantStop is the read the wait stops at, and wantDone the read it is
			// done at. Zero for both expects a timeout.
			wantStop, wantDone int
		}{
			{name: "the record beside the reading the wait found first", docs: []doc{unread(at(0))}},
			{name: "a reading recorded while it stood, on the second poll of it", docs: []doc{unread(at(0)), read(at(300))}, wantStop: 3},
			{
				name: "a reading that followed it, dated before it by another manager's clock",
				docs: []doc{unread(at(0)), read(at(50))}, wantStop: 3,
			},
			{name: "the reading from before the run, dated after it by a clock ahead", docs: []doc{unread(at(200))}},
			{name: "one poll is not enough: the next found a claim", docs: []doc{unread(at(0)), read(at(300)), run.claimed(at(310))}},
			{
				name: "the reading after it settled the run", docs: []doc{unread(at(0)), settled(at(300))},
				targets: []operate.Target{operate.TargetInSync}, wantDone: 2,
			},
			{name: "an acknowledgment took the record", docs: []doc{unread(at(0)), acknowledged(at(0))}},
			{
				name: "an acknowledgment took the record, beside a reading recorded since",
				docs: []doc{unread(at(0)), acknowledged(at(300))},
			},
			{
				name: "another run's record beside the reading before it",
				docs: []doc{unread(at(0)), another(read(at(300)))}, targets: noRunEnds,
			},
			{
				name: "and the reading after that run",
				docs: []doc{unread(at(0)), another(read(at(300))), another(read(at(600)))}, targets: noRunEnds, wantStop: 4,
			},
		} {
			targets := row.targets
			if targets == nil {
				targets = every
			}
			for _, target := range targets {
				t.Run(row.name+"/"+string(target), func(t *testing.T) {
					t.Parallel()

					tl := &timeline{family: operate.FamilyMigration, docs: row.docs}
					_, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilyMigration), operate.WaitOptions{
						Target: target, Timeout: timeout, Interval: interval, Clock: newStepClock(),
					})

					var timedOut *operate.TimeoutError
					switch {
					case row.wantDone > 0:
						if err != nil || tl.readCount() != row.wantDone {
							t.Fatalf("err = %v after %d reads, want the wait done at read %d", err, tl.readCount(), row.wantDone)
						}
					case row.wantStop > 0:
						assertBlockedStop(t, err, run, target, tl.readCount(), row.wantStop, false)
					case !errors.As(err, &timedOut):
						t.Fatalf("err = %v after %d reads, want a timeout", err, tl.readCount())
					}
				})
			}
		}
	})

	t.Run("reads", func(t *testing.T) {
		t.Parallel()
		before := asked(unread(at(0)))
		for _, row := range []struct {
			name               string
			since              doc
			docs               []doc
			wantStop, wantDone int
		}{
			{
				name: "the read the request started left it standing", since: before,
				docs: []doc{taken(run.claimed(at(10))), taken(read(at(300)))}, wantStop: 2,
			},
			{
				// The record and the reading from before the run are on the
				// document refresh read when it wrote the request.
				name: "the read the request started ended before the wait's first poll", since: before,
				docs: []doc{taken(read(at(300)))}, wantStop: 1,
			},
			{
				name: "the read the request started, dated before the record", since: before,
				docs: []doc{taken(run.claimed(at(10))), taken(read(at(50)))}, wantStop: 2,
			},
			{
				name: "the read the request started settled it", since: before,
				docs: []doc{taken(run.claimed(at(10))), taken(settled(at(300)))}, wantDone: 2,
			},
			{
				// The read ended in Planning, and the Apply under Always that
				// followed ended Unknown beside the read's reading before a poll
				// found the rest.
				name: "a run that ended after the read did", since: asked(inSyncAt(run, at(-600))),
				docs: []doc{taken(inSyncAt(run, at(10))), taken(read(at(300)))}, wantDone: 2,
			},
		} {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()

				tl := &timeline{family: operate.FamilyMigration, docs: row.docs}
				_, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilyMigration), operate.WaitOptions{
					Target: operate.TargetRead, ReadRequest: request, Timeout: timeout, Interval: interval, Clock: newStepClock(),
					Since: row.since.object(operate.FamilyMigration),
				})

				var timedOut *operate.TimeoutError
				switch {
				case row.wantDone > 0:
					if err != nil || tl.readCount() != row.wantDone {
						t.Fatalf("err = %v after %d reads, want the read done at read %d", err, tl.readCount(), row.wantDone)
					}
				case row.wantStop > 0:
					assertBlockedStop(t, err, run, operate.TargetRead, tl.readCount(), row.wantStop, true)
				case !errors.As(err, &timedOut):
					t.Fatalf("err = %v after %d reads, want a timeout", err, tl.readCount())
				}
			})
		}
	})
}

// A wait for the reference set-source wrote stops on a Blocked of that artifact
// alone: one at the new generation of another digest is not this one's.
func TestWaitForASourceStopsOnABlockedOfThatSource(t *testing.T) {
	t.Parallel()

	for _, reading := range blockedReadings(t) {
		if !reading.stored {
			continue
		}
		// The change made generation generation+1, which the operator acts on.
		changed := reading.generation + 1
		answer := reading.at(at(-600))
		answer.generation = changed
		answer.observedGeneration = reading.generation
		resolving := reading.claimed(at(10))
		resolving.generation = changed
		nowBlocked := reading.at(at(120))
		nowBlocked.generation = changed
		for index := range nowBlocked.conditions {
			nowBlocked.conditions[index].ObservedGeneration = changed
		}
		for _, row := range []struct {
			name     string
			objects  []client.Object
			wantStop int
		}{
			{
				name: "its own Blocked stops the wait",
				objects: []client.Object{
					pointedAt(resolving, reading.family, pinnedNew, digestOld, digestOld),
					pointedAt(nowBlocked, reading.family, pinnedNew, digestNew, digestNew),
				},
				wantStop: 3,
			},
			{
				name:    "the previous artifact's does not",
				objects: []client.Object{pointedAt(answer, reading.family, pinnedNew, digestOld, digestOld)},
			},
			{
				name:    "one at the new generation of another digest does not",
				objects: []client.Object{pointedAt(nowBlocked, reading.family, pinnedNew, digestOld, digestOld)},
			},
		} {
			for _, target := range []operate.Target{operate.TargetPlanned, operate.TargetApproval, operate.TargetInSync} {
				t.Run(reading.name()+"/"+row.name+"/"+string(target), func(t *testing.T) {
					t.Parallel()

					tl := &served{objects: row.objects}
					_, err := operate.Wait(context.Background(), tl.reader(t), refOf(reading.family), operate.WaitOptions{
						Target: target, Timeout: timeout, Interval: interval, Clock: newStepClock(),
						Since:      pointedAt(answer, reading.family, pinnedNew, digestOld, digestOld),
						Generation: changed, Source: pinnedNew,
					})

					var timedOut *operate.TimeoutError
					switch {
					case row.wantStop > 0:
						assertBlockedStop(t, err, reading, target, tl.reads, row.wantStop, false)
					case !errors.As(err, &timedOut):
						t.Fatalf("err = %v after %d reads, want a timeout", err, tl.reads)
					}
				})
			}
		}
	}
}

// A run that recorded its migrations applied is read back, and a read that ends
// Blocked before the history is read, a directory Ptah cannot read, holds that
// reading back as a verification policy's refusal does. The wait for the run names
// it, says it applied, and says the read after it ended Blocked, on the second
// poll. A realm's refusal is waited out, and a read back that records itself on the
// run decides by that record (TestARefusalAfterARunReadBackStopsNoWait). A run an
// operator older than 0.2.10 recorded names no operation and no plan, so only a
// wait for any run reads it, and the wait dates its read back by its finishedAt:
// the Blocked beside the reading from before the run holds it the same way.
func TestABlockedReadBackSaysTheRunApplied(t *testing.T) {
	t.Parallel()

	const job = "run-1"
	baseline := doc{phase: "AwaitingApproval", observedAt: at(0), plan: &planDoc{name: "plan-a", uid: "plan-uid-a"}}
	applied := &runDoc{
		outcome: operatorv1alpha1.MigrationRunOutcomeApplied, job: job, jobUID: "job-uid-1",
		startedAt: at(40), finishedAt: at(80), operationID: "op-1", database: databaseA,
		plan: "plan-a", planUID: "plan-uid-a", fingerprint: "sha256:a", approval: "approval-a", approvalUID: "approval-uid-a",
	}
	var directory, realm blockedReading
	for _, reading := range blockedReadings(t) {
		switch {
		case reading.reason == string(operatorv1alpha1.ReasonMigrationDirectoryUnreadable):
			directory = reading
		case reading.reason == string(operatorv1alpha1.ReasonRealmNotAuthorized):
			realm = reading
		}
	}
	blockedAfter := func(r blockedReading) doc {
		d := r.at(at(120))
		d.lastRun = applied
		return d
	}
	claimed := func(r blockedReading) doc {
		d := r.claimed(at(100))
		d.lastRun = applied
		return d
	}
	older := &runDoc{
		outcome: operatorv1alpha1.MigrationRunOutcomeApplied, job: job, jobUID: "job-uid-1", startedAt: at(40), finishedAt: at(80),
	}
	ofAnOlderOperator := func(d doc) doc {
		d.lastRun = older
		return d
	}
	for _, row := range []struct {
		name     string
		docs     []doc
		anyRun   bool
		wantStop int
	}{
		{name: "a directory Ptah cannot read", docs: []doc{baseline, blockedAfter(directory)}, wantStop: 3},
		{name: "one poll of it is not enough: the next found a claim", docs: []doc{baseline, blockedAfter(directory), claimed(directory)}},
		{name: "a realm's refusal", docs: []doc{baseline, blockedAfter(realm)}},
		{
			name: "a run an older operator recorded, before its read back",
			docs: []doc{baseline, ofAnOlderOperator(blockedAfter(directory))}, anyRun: true, wantStop: 3,
		},
		{
			name: "a run an older operator recorded, and a claim on the next poll",
			docs: []doc{baseline, ofAnOlderOperator(blockedAfter(directory)), ofAnOlderOperator(claimed(directory))}, anyRun: true,
		},
	} {
		for _, wait := range []struct {
			name    string
			options operate.WaitOptions
		}{
			{"any run", operate.WaitOptions{}},
			{"a plan", operate.WaitOptions{Plan: "plan-a"}},
			{"a decision", operate.WaitOptions{Decision: &operate.Decision{
				PlanName: "plan-a", PlanUID: "plan-uid-a", ApprovalName: "approval-a", ApprovalUID: "approval-uid-a",
			}}},
		} {
			if row.anyRun && wait.name != "any run" {
				continue
			}
			t.Run(row.name+"/"+wait.name, func(t *testing.T) {
				t.Parallel()

				tl := &timeline{family: operate.FamilyMigration, docs: row.docs}
				options := wait.options
				options.Target, options.Timeout, options.Interval, options.Clock = operate.TargetApplied, timeout, interval, newStepClock()
				_, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilyMigration), options)

				var stop *operate.StopError
				var timedOut *operate.TimeoutError
				switch {
				case row.wantStop > 0:
					if !errors.As(err, &stop) || stop.Reason != operate.StopNotProved || tl.readCount() != row.wantStop ||
						!strings.Contains(stop.Message, "run "+job+" recorded its migrations applied, and no reading of its database "+
							"has followed, since the read after it ended Blocked: "+directory.reason+": "+directory.says) {
						t.Fatalf("err = %v after %d reads, want a %s stop at read %d that names run %s and %s",
							err, tl.readCount(), operate.StopNotProved, row.wantStop, job, directory.reason)
					}
				case !errors.As(err, &timedOut):
					t.Fatalf("err = %v, want a timeout", err)
				}
			})
		}
	}
}

// asked is d with the request written and not taken.
func asked(d doc) doc {
	d.annotation = request
	return d
}

// taken is d with the request taken.
func taken(d doc) doc {
	d.annotation, d.lastRead = request, request
	return d
}

// takenNewer is d with a newer request written and taken.
func takenNewer(d doc) doc {
	d.annotation, d.lastRead = otherRequest, otherRequest
	return d
}

// readAt is d with a reading recorded at observedAt.
func readAt(d doc, observedAt time.Time) doc {
	d.observedAt = observedAt
	return d
}

// inSyncAt is the resource of r's family at rest in sync, its last attempt at
// attemptedAt.
func inSyncAt(r blockedReading, attemptedAt time.Time) doc {
	return doc{generation: r.generation, phase: "InSync", observedAt: t0, attemptedAt: attemptedAt, applyNever: r.applyNever}
}
