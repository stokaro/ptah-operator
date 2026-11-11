package operate_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
)

const refusalMessage = "Verification policy refused the artifact with 1 finding(s): require_signature"

// refused is a resource's conditions once the policy refused its artifact
// (blockVerification and refuseMigrationArtifact in internal/controller).
func refused(family operate.Family, generation int64) []metav1.Condition {
	verified := condition(operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, "PolicyRefused", refusalMessage, generation)
	if family == operate.FamilyMigration {
		return []metav1.Condition{
			verified,
			condition(operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, "PolicyRefused", refusalMessage, generation),
			condition(operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, "PolicyRefused",
				"Artifact verification policy must be satisfied before database access", generation),
			condition(operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionFalse, "PolicyRefused",
				"Nothing runs while the verification policy refuses the artifact", generation),
			condition(operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, "PolicyRefused",
				"No plan may be used for an artifact refused by policy", generation),
		}
	}
	return []metav1.Condition{
		verified,
		condition(operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, "ArtifactUnverified", "No plan may be used for an artifact refused by policy", generation),
		condition(operatorv1alpha1.ConditionInSync, metav1.ConditionFalse, "PolicyRefused",
			"In-sync status requires an artifact accepted by the current verification policy", generation),
		condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, "PolicyRefused",
			"Artifact verification policy must be satisfied before database access", generation),
	}
}

// resolvedAgain is what the harvest of a Resolve writes over a refusal: a schema's
// ArtifactVerified False with Pending, a migration's Unknown with DigestPinned.
func resolvedAgain(family operate.Family) []metav1.Condition {
	if family == operate.FamilyMigration {
		return rewritten(refused(family, 3),
			condition(operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionUnknown, "DigestPinned", "The artifact resolved to a digest and has not been verified yet", 3))
	}
	return rewritten(refused(family, 3),
		condition(operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, "Pending", "Resolved content has not been verified", 3))
}

// A refusal stops a wait when it is the last word on the source the resource
// names now: ArtifactVerified False with PolicyRefused, for the generation it has
// now, at rest in Blocked, on two reads in a row. Each row below that names no stop
// is a document the filter has to refuse. The phase, Ready and every other condition
// that can carry the reason decide nothing.
func TestWaitStopsOnARefusalThatHolds(t *testing.T) {
	t.Parallel()

	resting := func(family operate.Family) doc {
		return doc{phase: "Blocked", observedAt: at(0), conditions: refused(family, 3)}
	}
	for _, row := range []struct {
		name string
		// only is the family the row is about, when one family alone can read so.
		only operate.Family
		docs func(operate.Family) []doc
		// wantStop is the read the wait stops at. Zero expects a timeout.
		wantStop int
	}{
		{
			name:     "a refusal that stands when the wait begins stops it, on the second read",
			docs:     func(family operate.Family) []doc { return []doc{resting(family)} },
			wantStop: 2,
		},
		{
			name: "a request answered long ago leaves the refusal standing",
			docs: func(family operate.Family) []doc {
				answered := resting(family)
				answered.annotation, answered.lastRead = request, request
				return []doc{answered}
			},
			wantStop: 2,
		},
		{
			name: "a value that starts no read holds it up only until the operator answers it",
			docs: func(family operate.Family) []doc {
				pending := resting(family)
				pending.annotation = "now please"
				answered := resting(family)
				answered.annotation, answered.lastRead = "now please", "now please"
				return []doc{pending, answered}
			},
			wantStop: 3,
		},
		{
			name: "one read of a refusal is not enough: the next read found a claim",
			docs: func(family operate.Family) []doc {
				claimed := resting(family)
				claimed.phase, claimed.operation = "Resolving", "Resolve"
				return []doc{resting(family), claimed}
			},
		},
		{
			name: "the count starts again after a read that found another state",
			docs: func(family operate.Family) []doc {
				claimed := resting(family)
				claimed.phase, claimed.operation = "Resolving", "Resolve"
				return []doc{resting(family), claimed, resting(family)}
			},
			wantStop: 4,
		},
		{
			name: "a spec edited since the refusal",
			docs: func(family operate.Family) []doc {
				edited := resting(family)
				edited.generation, edited.observedGeneration = 4, 3
				return []doc{edited}
			},
		},
		{
			name: "a read request the operator has not taken",
			docs: func(family operate.Family) []doc {
				requested := resting(family)
				requested.annotation = request
				return []doc{requested}
			},
		},
		{
			name: "a request replaced by another the operator has not taken",
			docs: func(family operate.Family) []doc {
				requested := resting(family)
				requested.annotation, requested.lastRead = otherRequest, request
				return []doc{requested}
			},
		},
		{
			name: "the claim of the read that asks again",
			docs: func(family operate.Family) []doc {
				claimed := resting(family)
				claimed.phase, claimed.operation = "Resolving", "Resolve"
				return []doc{claimed}
			},
		},
		{
			name: "a claim in flight though the phase still reads Blocked",
			docs: func(family operate.Family) []doc {
				claimed := resting(family)
				claimed.operation = "Resolve"
				return []doc{claimed}
			},
		},
		{
			name: "a proof owed in Blocked",
			only: operate.FamilySchema,
			docs: func(family operate.Family) []doc {
				owed := resting(family)
				owed.proof = true
				return []doc{owed}
			},
		},
		{
			name: "a phase that is not Blocked",
			docs: func(family operate.Family) []doc {
				failed := resting(family)
				failed.phase = "Failed"
				return []doc{failed}
			},
		},
		{
			name: "the same artifact resolved again, not verified yet",
			docs: func(family operate.Family) []doc {
				return []doc{{phase: "Verifying", observedAt: at(0), conditions: resolvedAgain(family)}}
			},
		},
		{
			name: "the same conditions at rest in Blocked with the verdict gone",
			docs: func(family operate.Family) []doc {
				return []doc{{phase: "Blocked", observedAt: at(0), conditions: resolvedAgain(family)}}
			},
		},
		{
			// What #866 left behind and #904 cleared: a stale reason beside an
			// artifact that passed. A resource written by an older manager can
			// still read this way.
			name: "the reason left on other conditions, beside an artifact that passed",
			docs: func(family operate.Family) []doc {
				return []doc{{phase: "Blocked", observedAt: at(0), conditions: rewritten(refused(family, 3),
					condition(operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionTrue, "PolicySatisfied", "The resolved artifact satisfied its verification policy", 3))}}
			},
		},
		{
			name: "another reason on ArtifactVerified False",
			docs: func(family operate.Family) []doc {
				return []doc{{phase: "Blocked", observedAt: at(0), conditions: rewritten(refused(family, 3),
					condition(operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionFalse, "PolicyChanged", "The verification policy changed", 3))}}
			},
		},
		{
			name: "the reason on ArtifactVerified Unknown",
			docs: func(family operate.Family) []doc {
				return []doc{{phase: "Blocked", observedAt: at(0), conditions: rewritten(refused(family, 3),
					condition(operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionUnknown, "PolicyRefused", refusalMessage, 3))}}
			},
		},
		{
			name: "a source with no ArtifactVerified: a Git or ConfigMap source removes it",
			docs: func(family operate.Family) []doc {
				return []doc{{phase: "Blocked", observedAt: at(0), conditions: refused(family, 3)[1:]}}
			},
		},
	} {
		for _, family := range []operate.Family{operate.FamilySchema, operate.FamilyMigration} {
			if row.only != "" && row.only != family {
				continue
			}
			for _, target := range []operate.Target{operate.TargetInSync, operate.TargetApproval, operate.TargetApplied} {
				t.Run(row.name+"/"+string(family)+"/"+string(target), func(t *testing.T) {
					t.Parallel()

					tl := &timeline{family: family, docs: row.docs(family)}
					_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
						Target: target, Timeout: timeout, Interval: interval, Clock: newStepClock(),
					})

					var stop *operate.StopError
					var timedOut *operate.TimeoutError
					switch {
					case row.wantStop > 0:
						if !errors.As(err, &stop) || stop.Reason != operate.StopPolicyRefused || stop.Phase != "Blocked" ||
							tl.readCount() != row.wantStop || !strings.Contains(stop.Message, refusalMessage) {
							t.Fatalf("err = %v after %d reads, want a %s stop at read %d naming %q",
								err, tl.readCount(), operate.StopPolicyRefused, row.wantStop, refusalMessage)
						}
					case !errors.As(err, &timedOut):
						t.Fatalf("err = %v, want a timeout", err)
					}
				})
			}
		}
	}
}

// A wait for a suspension asks something a refusal does not answer. A wait for a
// read is answered only by a refusal its own read ended in, and the one it finds
// when it begins is older (TestAWaitForAReadStopsOnTheRefusalItsReadEndedIn).
func TestARefusalDoesNotStopAWaitForAReadOrASuspension(t *testing.T) {
	t.Parallel()
	for _, family := range []operate.Family{operate.FamilySchema, operate.FamilyMigration} {
		t.Run(string(family)+"/read", func(t *testing.T) {
			t.Parallel()
			at0 := doc{phase: "Blocked", observedAt: at(0), annotation: request, lastRead: request, conditions: refused(family, 3)}
			tl := &timeline{family: family, docs: []doc{at0}}
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
				Target: operate.TargetRead, Timeout: timeout, Interval: interval, Clock: newStepClock(), ReadRequest: request,
			})
			var timedOut *operate.TimeoutError
			if !errors.As(err, &timedOut) || !strings.Contains(timedOut.Error(), "PolicyRefused") {
				t.Fatalf("err = %v, want a timeout that names the refusal", err)
			}
		})
		t.Run(string(family)+"/suspended", func(t *testing.T) {
			t.Parallel()
			suspended := doc{suspend: true, phase: "Suspended", observedAt: at(0), conditions: append(refused(family, 3),
				suspendedConditions(family, 3)...)}
			tl := &timeline{family: family, docs: []doc{suspended}}
			if _, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
				Target: operate.TargetSuspended, Timeout: timeout, Interval: interval, Clock: newStepClock(),
			}); err != nil {
				t.Fatalf("wait for suspension = %v, want it reached", err)
			}
		})
	}
}

// refusedAt is a resource at rest in Blocked on a refusal stamped at: a schema by
// its status.source.resolvedAt, which the Resolve before the refusal wrote, and a
// migration by the lastTransitionTime of ArtifactVerified, which the refusal wrote.
// A schema's ArtifactVerified keeps the time it went False, as the controller
// leaves it from Pending to PolicyRefused.
func refusedAt(family operate.Family, at time.Time) doc {
	d := doc{phase: "Blocked", observedAt: t0, conditions: refused(family, 3)}
	stamp(&d, family, at)
	return d
}

// stamp dates the refusal d records, or the Resolve a schema's next refusal will
// be stamped by, at at.
func stamp(d *doc, family operate.Family, at time.Time) {
	if family == operate.FamilySchema {
		d.resolvedAt = at
		return
	}
	for index := range d.conditions {
		if d.conditions[index].Type == operatorv1alpha1.ConditionArtifactVerified {
			d.conditions[index].LastTransitionTime = metav1.NewTime(at)
		}
	}
}

// A wait for a read stops on a refusal the read its request asked for ended in:
// ArtifactVerified False with PolicyRefused under a stamp other than the one on the
// document the request is dated by. That document is the baseline when it rests,
// and otherwise the one in which the operator takes the request. The stamp is
// compared and never ordered, here or against the request's own instant, which the
// client's clock chose: the rows date refusals before the request, after it, and
// before the refusal they replace, as managers whose clocks disagree do.
func TestAWaitForAReadStopsOnTheRefusalItsReadEndedIn(t *testing.T) {
	t.Parallel()

	// The request is at(5). A refusal stamped earlier or later is not, for that,
	// older or newer than it.
	before, inFlight, after := at(-600), at(-60), at(120)
	asked := func(d doc) doc {
		d.annotation = request
		return d
	}
	taken := func(d doc) doc {
		d.annotation, d.lastRead = request, request
		return d
	}
	// resolving is the claim of a read, with the refusal it found still recorded.
	resolving := func(family operate.Family, refusal time.Time) doc {
		d := refusedAt(family, refusal)
		d.phase, d.operation = "Resolving", "Resolve"
		return d
	}
	// verifying is the harvest of a Resolve at resolvedAt: a schema's
	// ArtifactVerified False with Pending, a migration's Unknown.
	verifying := func(family operate.Family, resolvedAt time.Time) doc {
		d := doc{phase: "Verifying", observedAt: t0, conditions: resolvedAgain(family)}
		stamp(&d, family, resolvedAt)
		return d
	}
	// accepted is a resource at rest in sync on an artifact the policy accepted.
	accepted := func(family operate.Family) doc {
		d := doc{phase: "InSync", observedAt: t0, conditions: []metav1.Condition{
			condition(operatorv1alpha1.ConditionArtifactVerified, metav1.ConditionTrue, "PolicySatisfied",
				"The resolved artifact satisfied its verification policy", 3),
		}}
		stamp(&d, family, before)
		return d
	}
	// takenNewer is d after the operator took a newer request, otherRequest.
	takenNewer := func(d doc) doc {
		d.annotation, d.lastRead = otherRequest, otherRequest
		return d
	}
	for _, row := range []struct {
		name string
		// since is the baseline; nil makes the first read the baseline, as
		// kubectl ptah wait --for read does.
		since func(operate.Family) client.Object
		docs  func(operate.Family) []doc
		// wantStop is the read the wait stops at, and wantDone the read it is
		// done at. Zero for both expects a timeout.
		wantStop, wantDone int
	}{
		{
			name:  "the read the request started ended in a refusal",
			since: func(family operate.Family) client.Object { return asked(refusedAt(family, before)).object(family) },
			docs: func(family operate.Family) []doc {
				return []doc{
					taken(refusedAt(family, before)),
					taken(resolving(family, before)),
					taken(verifying(family, after)),
					taken(refusedAt(family, after)),
				}
			},
			wantStop: 4,
		},
		{
			name:     "that refusal before the wait's first read",
			since:    func(family operate.Family) client.Object { return asked(refusedAt(family, before)).object(family) },
			docs:     func(family operate.Family) []doc { return []doc{taken(refusedAt(family, after))} },
			wantStop: 1,
		},
		{
			name:     "that refusal dated before the request by the operator's clock",
			since:    func(family operate.Family) client.Object { return asked(refusedAt(family, at(-600))).object(family) },
			docs:     func(family operate.Family) []doc { return []doc{taken(refusedAt(family, at(-300)))} },
			wantStop: 1,
		},
		{
			name:     "that refusal dated before the one it replaced, by another manager's clock",
			since:    func(family operate.Family) client.Object { return asked(refusedAt(family, at(-600))).object(family) },
			docs:     func(family operate.Family) []doc { return []doc{taken(refusedAt(family, at(-1200)))} },
			wantStop: 1,
		},
		{
			name:  "a refusal from before the request: the operator took it and has not claimed the read",
			since: func(family operate.Family) client.Object { return asked(refusedAt(family, before)).object(family) },
			docs:  func(family operate.Family) []doc { return []doc{taken(refusedAt(family, before))} },
		},
		{
			name:  "a refusal from before the request, dated after it by the operator's clock",
			since: func(family operate.Family) client.Object { return asked(refusedAt(family, at(600))).object(family) },
			docs:  func(family operate.Family) []doc { return []doc{taken(refusedAt(family, at(600)))} },
		},
		{
			// kubectl ptah wait --for read begun while that read verifies: a
			// schema's ArtifactVerified is already False, with Pending, under
			// the stamp the refusal will carry.
			name: "a wait begun while the read the request started verifies",
			docs: func(family operate.Family) []doc {
				return []doc{taken(verifying(family, after)), taken(refusedAt(family, after))}
			},
			wantStop: 2,
		},
		{
			name: "a wait begun after the operator took the request finds the refusal standing",
			docs: func(family operate.Family) []doc { return []doc{taken(refusedAt(family, before))} },
		},
		{
			name: "and stops on the refusal of a later read",
			docs: func(family operate.Family) []doc {
				return []doc{taken(refusedAt(family, before)), taken(resolving(family, before)), taken(refusedAt(family, after))}
			},
			wantStop: 3,
		},
		{
			name:  "a read under way when the request was written ends in a refusal that does not count",
			since: func(family operate.Family) client.Object { return asked(resolving(family, before)).object(family) },
			docs: func(family operate.Family) []doc {
				return []doc{
					asked(verifying(family, inFlight)),
					asked(refusedAt(family, inFlight)),
					taken(refusedAt(family, inFlight)),
				}
			},
		},
		{
			name:  "the read the operator starts after it does",
			since: func(family operate.Family) client.Object { return asked(resolving(family, before)).object(family) },
			docs: func(family operate.Family) []doc {
				return []doc{
					asked(verifying(family, inFlight)),
					asked(refusedAt(family, inFlight)),
					taken(refusedAt(family, inFlight)),
					taken(resolving(family, inFlight)),
					taken(verifying(family, after)),
					taken(refusedAt(family, after)),
				}
			},
			wantStop: 6,
		},
		{
			// It cannot tell the read under way from the one the request
			// started, so it waits for one after both.
			name:  "the first answered document already shows a read under way at the request ended",
			since: func(family operate.Family) client.Object { return asked(resolving(family, before)).object(family) },
			docs:  func(family operate.Family) []doc { return []doc{taken(refusedAt(family, inFlight))} },
		},
		{
			// The operator takes a newer request on the pass after the rest the
			// read ended in, milliseconds later, so a poll can miss that rest
			// and find the newer read's refusal beside this read's reading.
			name:  "the read the request started ended with a reading, and a newer request's read in a refusal",
			since: func(family operate.Family) client.Object { return asked(accepted(family)).object(family) },
			docs: func(family operate.Family) []doc {
				// claimed is the claim of a Resolve on the accepted artifact, beside
				// the reading at observedAt and a schema's Resolve at resolvedAt.
				claimed := func(observedAt, resolvedAt time.Time) doc {
					d := accepted(family)
					d.phase, d.operation, d.observedAt = "Resolving", "Resolve", observedAt
					stamp(&d, family, resolvedAt)
					return d
				}
				newerRefused := refusedAt(family, at(150))
				newerRefused.observedAt = at(30)
				return []doc{taken(claimed(t0, before)), takenNewer(claimed(at(30), after)), takenNewer(newerRefused)}
			},
			wantDone: 3,
		},
		{
			name:  "a refusal while a newer request waits for the operator",
			since: func(family operate.Family) client.Object { return asked(refusedAt(family, before)).object(family) },
			docs: func(family operate.Family) []doc {
				newer := taken(refusedAt(family, after))
				newer.annotation = otherRequest
				return []doc{newer}
			},
		},
		{
			name:  "a spec edited since the refusal",
			since: func(family operate.Family) client.Object { return asked(refusedAt(family, before)).object(family) },
			docs: func(family operate.Family) []doc {
				edited := taken(refusedAt(family, after))
				edited.generation, edited.observedGeneration = 4, 3
				return []doc{edited}
			},
		},
	} {
		for _, family := range []operate.Family{operate.FamilySchema, operate.FamilyMigration} {
			t.Run(row.name+"/"+string(family), func(t *testing.T) {
				t.Parallel()

				tl := &timeline{family: family, docs: row.docs(family)}
				options := operate.WaitOptions{
					Target: operate.TargetRead, ReadRequest: request, Timeout: timeout, Interval: interval, Clock: newStepClock(),
				}
				if row.since != nil {
					options.Since = row.since(family)
				}
				_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), options)

				var stop *operate.StopError
				var timedOut *operate.TimeoutError
				switch {
				case row.wantDone > 0:
					if err != nil || tl.readCount() != row.wantDone {
						t.Fatalf("err = %v after %d reads, want the read done at read %d", err, tl.readCount(), row.wantDone)
					}
				case row.wantStop > 0:
					if !errors.As(err, &stop) || stop.Reason != operate.StopPolicyRefused || stop.Phase != "Blocked" ||
						stop.Target != operate.TargetRead || tl.readCount() != row.wantStop || !strings.Contains(stop.Message, refusalMessage) {
						t.Fatalf("err = %v after %d reads, want a %s stop at read %d naming %q",
							err, tl.readCount(), operate.StopPolicyRefused, row.wantStop, refusalMessage)
					}
				case !errors.As(err, &timedOut):
					t.Fatalf("err = %v after %d reads, want a timeout", err, tl.readCount())
				}
			})
		}
	}
}

// A wait for the reference set-source wrote stops on a refusal of that artifact
// alone: the refusal the answer to the change still carries is the previous
// artifact's, and a refusal at the new generation of another digest is not this one's.
func TestWaitForASourceStopsOnARefusalOfThatSource(t *testing.T) {
	t.Parallel()

	for _, family := range []operate.Family{operate.FamilySchema, operate.FamilyMigration} {
		answer := doc{generation: 4, observedGeneration: 3, phase: "Blocked", observedAt: at(0), conditions: refused(family, 3)}
		resolving := doc{generation: 4, phase: "Resolving", operation: "Resolve", observedAt: at(0), conditions: refused(family, 3)}
		nowRefused := doc{generation: 4, phase: "Blocked", observedAt: at(0), conditions: refused(family, 4)}
		for _, row := range []struct {
			name     string
			objects  []client.Object
			wantStop int
		}{
			{
				name: "its own refusal stops the wait",
				objects: []client.Object{
					pointedAt(answer, family, pinnedNew, digestOld, digestOld),
					pointedAt(resolving, family, pinnedNew, digestOld, digestOld),
					pointedAt(nowRefused, family, pinnedNew, digestNew, digestNew),
				},
				wantStop: 4,
			},
			{
				name:    "the previous artifact's refusal does not",
				objects: []client.Object{pointedAt(answer, family, pinnedNew, digestOld, digestOld)},
			},
			{
				name:    "a refusal at the new generation of another digest does not",
				objects: []client.Object{pointedAt(nowRefused, family, pinnedNew, digestOld, digestOld)},
			},
		} {
			for _, target := range []operate.Target{operate.TargetPlanned, operate.TargetApproval, operate.TargetInSync} {
				t.Run(string(family)+"/"+row.name+"/"+string(target), func(t *testing.T) {
					t.Parallel()

					tl := &served{objects: row.objects}
					_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
						Target: target, Timeout: timeout, Interval: interval, Clock: newStepClock(),
						Since: pointedAt(answer, family, pinnedNew, digestOld, digestOld), Generation: 4, Source: pinnedNew,
					})

					var stop *operate.StopError
					var timedOut *operate.TimeoutError
					switch {
					case row.wantStop > 0:
						if !errors.As(err, &stop) || stop.Reason != operate.StopPolicyRefused || tl.reads != row.wantStop {
							t.Fatalf("err = %v after %d reads, want a %s stop at read %d", err, tl.reads, operate.StopPolicyRefused, row.wantStop)
						}
					case !errors.As(err, &timedOut):
						t.Fatalf("err = %v, want a timeout", err)
					}
				})
			}
		}
	}
}

// An approved plan's wait hears the refusal too: the refusal took the approval with it.
func TestWaitForAPlanStopsOnARefusal(t *testing.T) {
	t.Parallel()
	for _, family := range []operate.Family{operate.FamilySchema, operate.FamilyMigration} {
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()
			tl := &timeline{family: family, docs: []doc{{phase: "Blocked", observedAt: at(0), conditions: refused(family, 3)}}}
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
				Target: operate.TargetApplied, Timeout: timeout, Interval: interval, Clock: newStepClock(), Plan: "plan-a",
			})
			var stop *operate.StopError
			if !errors.As(err, &stop) || stop.Reason != operate.StopPolicyRefused {
				t.Fatalf("err = %v, want a %s stop", err, operate.StopPolicyRefused)
			}
		})
	}
}

// A refusal that comes after the run a wait for applied is about has ended does not
// say the target will not come: the run changed the database. It holds back the
// reading after the run, so the wait names the run, says it applied, and says no
// reading of its database has followed. The reading the resource shows is the one
// from before the run, dated before it, or in the same second, or after it by
// another manager's clock; none of those dates may read as a history read back.
func TestARefusalAfterAnAppliedRunSaysTheRunApplied(t *testing.T) {
	t.Parallel()

	const job = "run-1"
	baseline := doc{phase: "AwaitingApproval", observedAt: at(0), plan: &planDoc{name: "plan-a", uid: "plan-uid-a"}}
	applied := func(finishedAt int) *runDoc {
		return &runDoc{
			outcome: operatorv1alpha1.MigrationRunOutcomeApplied, job: job, jobUID: "job-uid-1",
			startedAt: at(40), finishedAt: at(finishedAt), operationID: "op-1",
			plan: "plan-a", planUID: "plan-uid-a", fingerprint: "sha256:a", approval: "approval-a", approvalUID: "approval-uid-a",
		}
	}
	refusedAfter := func(run *runDoc, observedAt int) doc {
		return doc{phase: "Blocked", observedAt: at(observedAt), lastRun: run, conditions: refused(operate.FamilyMigration, 3)}
	}
	claimed := func(run *runDoc) doc {
		return doc{phase: "Resolving", operation: "Resolve", observedAt: at(0), lastRun: run, conditions: refused(operate.FamilyMigration, 3)}
	}
	waits := []struct {
		name    string
		options operate.WaitOptions
	}{
		{"any run", operate.WaitOptions{}},
		{"a plan", operate.WaitOptions{Plan: "plan-a"}},
		{"a decision", operate.WaitOptions{Decision: &operate.Decision{
			PlanName: "plan-a", PlanUID: "plan-uid-a", ApprovalName: "approval-a", ApprovalUID: "approval-uid-a",
		}}},
	}
	for _, row := range []struct {
		name string
		docs []doc
		// wantStop is the read the wait stops at. Zero expects a timeout.
		wantStop int
	}{
		{
			name:     "the reading from before the run",
			docs:     []doc{baseline, refusedAfter(applied(80), 0)},
			wantStop: 3,
		},
		{
			name:     "the reading from before the run, in the run's second",
			docs:     []doc{baseline, refusedAfter(applied(0), 0)},
			wantStop: 3,
		},
		{
			name:     "the reading from before the run, dated after it",
			docs:     []doc{baseline, refusedAfter(applied(80), 90)},
			wantStop: 3,
		},
		{
			name: "one read of the refusal is not enough: the next read found a claim",
			docs: []doc{baseline, refusedAfter(applied(80), 0), claimed(applied(80))},
		},
	} {
		for _, wait := range waits {
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
						!strings.Contains(stop.Message, "run "+job+" recorded its migrations applied, and no reading of its database has followed") ||
						!strings.Contains(stop.Message, refusalMessage) {
						t.Fatalf("err = %v after %d reads, want a %s stop at read %d that names run %s and %q",
							err, tl.readCount(), operate.StopNotProved, row.wantStop, job, refusalMessage)
					}
				case !errors.As(err, &timedOut):
					t.Fatalf("err = %v, want a timeout", err)
				}
			})
		}
	}
}

// A run whose reading afterwards is recorded on it was read back, so a refusal that
// comes later stops no wait for it, whatever date that reading carries. Here another
// manager's clock dated it before the run ended: a wait for a plan or a decision
// goes by the record and is done, and a wait for any run, which dates the reading,
// keeps waiting for one after the run. Neither may say the target will not come, or
// that no reading followed the run.
func TestARefusalAfterARunReadBackStopsNoWait(t *testing.T) {
	t.Parallel()

	run := &runDoc{
		outcome: operatorv1alpha1.MigrationRunOutcomeApplied, job: "run-1", jobUID: "job-uid-1",
		startedAt: at(40), finishedAt: at(80), operationID: "op-1",
		plan: "plan-a", planUID: "plan-uid-a", fingerprint: "sha256:a", approval: "approval-a", approvalUID: "approval-uid-a",
		verification: operatorv1alpha1.MigrationRunVerifiedInSync, verifiedAt: at(70),
	}
	docs := []doc{
		{phase: "AwaitingApproval", observedAt: at(0), plan: &planDoc{name: "plan-a", uid: "plan-uid-a"}},
		{phase: "Blocked", observedAt: at(70), lastRun: run, conditions: refused(operate.FamilyMigration, 3)},
	}
	for _, wait := range []struct {
		name    string
		options operate.WaitOptions
		// done is a wait the record ends; any other keeps waiting to its timeout.
		done bool
	}{
		{name: "any run"},
		{name: "a plan", options: operate.WaitOptions{Plan: "plan-a"}, done: true},
		{name: "a decision", options: operate.WaitOptions{Decision: &operate.Decision{
			PlanName: "plan-a", PlanUID: "plan-uid-a", ApprovalName: "approval-a", ApprovalUID: "approval-uid-a",
		}}, done: true},
	} {
		t.Run(wait.name, func(t *testing.T) {
			t.Parallel()

			tl := &timeline{family: operate.FamilyMigration, docs: docs}
			options := wait.options
			options.Target, options.Timeout, options.Interval, options.Clock = operate.TargetApplied, timeout, interval, newStepClock()
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilyMigration), options)

			var timedOut *operate.TimeoutError
			switch {
			case wait.done && err != nil:
				t.Fatalf("err = %v, want the run recorded as read back to end the wait", err)
			case !wait.done && (!errors.As(err, &timedOut) || !strings.Contains(timedOut.Waiting, "the history read back after run run-1")):
				t.Fatalf("err = %v, want a timeout still waiting for the history read back after run run-1", err)
			}
		})
	}
}
