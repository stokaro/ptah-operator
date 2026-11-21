package e2e

import (
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// planRefusedReading is the schema the reconciler stores when a Plan met
// Ptah's refusal of a view whose security_invoker the schema leaves unstated,
// from the plan document the pinned image printed for the runner harness. Its
// view is differs_notes_view, at generation 1.
// TestTheRefusalReadingIsWhatTheReconcilerStores in internal/controller holds
// the file to what the reconciler writes.
func planRefusedReading(t *testing.T) *ptahv1alpha1.PtahSchema {
	t.Helper()
	return storedReading(t, "schema-plan-refused.json")
}

const planRefusedReadingView = "differs_notes_view"

// viewSecurityPlanRefused takes the schema the reconciler stores for the
// refused Plan, and refuses the retry it replaced, the other stops on a
// desired schema, a refusal of the spec before the one the row asked for, and
// a message that names another view, another option or another code, or that
// carries Ptah's sentence.
func TestAViewSecurityPlanRefusalIsBlockedWithNoClaim(t *testing.T) {
	t.Parallel()
	message, err := viewSecurityPlanRefused(planRefusedReading(t), planRefusedReadingView, 1)
	if err != nil {
		t.Fatalf("viewSecurityPlanRefused() refused the reconciler's reading: %v", err)
	}
	if message != viewSecurityRefusal(planRefusedReadingView) {
		t.Fatalf("the reading says %q", message)
	}
	for name, reading := range map[string]*ptahv1alpha1.PtahSchema{
		"a file Ptah could not read": refusalReading(t),
		"a schema Ptah refuses":      invalidReading(t),
	} {
		if said, err := viewSecurityPlanRefused(reading, planRefusedReadingView, 1); err == nil {
			t.Errorf("accepted the reading of %s: %q", name, said)
		}
	}
	if said, err := schemaInvalidRefused(planRefusedReading(t)); err == nil {
		t.Errorf("schemaInvalidRefused() accepted the reading of a refused plan: %q", said)
	}

	ready := func(s *ptahv1alpha1.PtahSchema) *metav1.Condition {
		return meta.FindStatusCondition(s.Status.Conditions, ptahv1alpha1.ConditionReady)
	}
	for _, row := range []struct {
		name       string
		view       string
		generation int64
		edit       func(*ptahv1alpha1.PtahSchema)
	}{
		{name: "the retry it replaced", edit: func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Phase = ptahv1alpha1.PhaseFailed
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationPlan, ID: "plan", Attempt: 2}
			failure := ready(s).Message
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{Type: ptahv1alpha1.ConditionReconciliationFailed,
				Status: metav1.ConditionTrue, Reason: string(ptahv1alpha1.ReasonOperationFailed), Message: failure})
			ready(s).Reason, ready(s).Message = string(ptahv1alpha1.ReasonOperationFailed), "Reconciliation did not complete"
		}},
		{name: "the spec moved after the refusal", generation: 2},
		{name: "another view", view: externalView},
		{name: "another option", edit: func(s *ptahv1alpha1.PtahSchema) {
			ready(s).Message = "plan_refused: ptah refused to plan the change (view-security-option: public." +
				planRefusedReadingView + " security_barrier)"
		}},
		{name: "another code", edit: func(s *ptahv1alpha1.PtahSchema) {
			ready(s).Message = "plan_refused: ptah refused to plan the change (view-definer: public." +
				planRefusedReadingView + " security_invoker)"
		}},
		{name: "the code with no view", edit: func(s *ptahv1alpha1.PtahSchema) {
			ready(s).Message = "plan_refused: ptah refused to plan the change (view-security-option)"
		}},
		{name: "Ptah's sentence carried", edit: func(s *ptahv1alpha1.PtahSchema) {
			ready(s).Message += ": view public." + planRefusedReadingView + " holds security_invoker=true and the schema does not state it"
		}},
		{name: "the output the retry wrote", edit: func(s *ptahv1alpha1.PtahSchema) {
			ready(s).Message = "invalid_plan_output: plan command did not complete successfully"
		}},
		{name: "a database it did not reach", edit: func(s *ptahv1alpha1.PtahSchema) {
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{Type: ptahv1alpha1.ConditionDatabaseReachable,
				Status: metav1.ConditionFalse, Reason: string(ptahv1alpha1.ReasonDatabaseUnreachable), Message: "connection refused"})
		}},
		{name: "a claim beside", edit: func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationResolve, ID: "next"}
		}},
		{name: "a plan beside", edit: func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{Name: "plan"}
		}},
		{name: "another phase", edit: func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseObserving }},
		{name: "Ready true", edit: func(s *ptahv1alpha1.PtahSchema) { ready(s).Status = metav1.ConditionTrue }},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema := planRefusedReading(t)
			if row.edit != nil {
				row.edit(schema)
			}
			view, generation := row.view, row.generation
			if view == "" {
				view = planRefusedReadingView
			}
			if generation == 0 {
				generation = 1
			}
			if said, err := viewSecurityPlanRefused(schema, view, generation); err == nil {
				t.Fatalf("accepted it: %q", said)
			}
		})
	}
}

// liftedReading is the schema the reconciler stores when the read after the
// spec edit that removed a refused plan's cause finds nothing to plan, at
// generation 2. TestTheLiftedRefusalReadingIsWhatTheReconcilerStores in
// internal/controller holds the file to what the reconciler writes: InSync on
// liftedReadingDigest and no Applied record, since the Resolve of the changed
// source cleared it. refusedAt is the attempt of the refused reading, which
// the controller's fixed clock stamps at the instant it stamped the refusal
// the lifted reading started from, ten minutes before its own attempt.
func liftedReading(t *testing.T) (*ptahv1alpha1.PtahSchema, *metav1.Time) {
	t.Helper()
	return storedReading(t, "schema-plan-refusal-lifted.json"), planRefusedReading(t).Status.LastAttemptTime.DeepCopy()
}

const liftedReadingDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// planRefusalLifted takes the schema the reconciler stores once an edit lifted
// the refusal, and refuses the refusal itself, a reading of another digest or
// of the refused spec, one whose last attempt is the refused one, one whose
// Ready still names the refusal, one that is not converged, one with work in
// flight, and one with an Apply recorded, which the lifted read never writes.
func TestAPlanRefusalLiftedIsInSyncAfterTheRefusal(t *testing.T) {
	t.Parallel()
	lifted, refusedAt := liftedReading(t)
	if err := planRefusalLifted(lifted, liftedReadingDigest, 2, refusedAt); err != nil {
		t.Fatalf("planRefusalLifted() refused the reconciler's reading: %v", err)
	}
	refused := planRefusedReading(t)
	refused.Status.Source.Digest = liftedReadingDigest
	if err := planRefusalLifted(refused, liftedReadingDigest, 1, refusedAt); err == nil {
		t.Fatal("accepted the refusal itself")
	}

	for _, row := range []struct {
		name       string
		digest     string
		generation int64
		refusedAt  func(*metav1.Time) *metav1.Time
		edit       func(*ptahv1alpha1.PtahSchema)
	}{
		{name: "another digest", digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
		{name: "the refused spec's generation", generation: 1},
		{name: "another phase", edit: func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseBlocked }},
		{name: "status of the refused spec", edit: func(s *ptahv1alpha1.PtahSchema) { s.Status.ObservedGeneration = 1 }},
		{name: "Ready of the refused spec", edit: func(s *ptahv1alpha1.PtahSchema) {
			meta.FindStatusCondition(s.Status.Conditions, ptahv1alpha1.ConditionReady).ObservedGeneration = 1
		}},
		{name: "Ready still names the refusal", edit: func(s *ptahv1alpha1.PtahSchema) {
			ready := meta.FindStatusCondition(s.Status.Conditions, ptahv1alpha1.ConditionReady)
			ready.Status, ready.Reason = metav1.ConditionFalse, string(ptahv1alpha1.ReasonPlanRefused)
		}},
		{name: "no Ready", edit: func(s *ptahv1alpha1.PtahSchema) {
			meta.RemoveStatusCondition(&s.Status.Conditions, ptahv1alpha1.ConditionReady)
		}},
		{name: "InSync after an outcome nobody established", edit: func(s *ptahv1alpha1.PtahSchema) {
			meta.FindStatusCondition(s.Status.Conditions, ptahv1alpha1.ConditionInSync).Reason =
				string(ptahv1alpha1.ReasonConvergedAfterUnknownOutcome)
		}},
		{name: "InSync unknown", edit: func(s *ptahv1alpha1.PtahSchema) {
			meta.FindStatusCondition(s.Status.Conditions, ptahv1alpha1.ConditionInSync).Status = metav1.ConditionUnknown
		}},
		{name: "the refused attempt", edit: func(s *ptahv1alpha1.PtahSchema) {
			s.Status.LastAttemptTime = refused.Status.LastAttemptTime.DeepCopy()
		}},
		{name: "no refusal to date from", refusedAt: func(*metav1.Time) *metav1.Time { return nil }},
		{name: "a read in flight", edit: func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationObserve, ID: "read"}
		}},
		{name: "a proof owed", edit: func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{ApplyGeneration: 2}
		}},
		{name: "a lock release owed", edit: func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
		}},
		{name: "an Apply recorded", edit: func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Applied = &ptahv1alpha1.AppliedStatus{ArtifactDigest: liftedReadingDigest}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema, at := liftedReading(t)
			if row.edit != nil {
				row.edit(schema)
			}
			if row.refusedAt != nil {
				at = row.refusedAt(at)
			}
			want, generation := row.digest, row.generation
			if want == "" {
				want = liftedReadingDigest
			}
			if generation == 0 {
				generation = 2
			}
			if err := planRefusalLifted(schema, want, generation, at); err == nil {
				t.Fatal("accepted it")
			}
		})
	}
}

// holdSchemaRefusal holds the reconciler's refused reading until two failure
// retries after its attempt, from the interval the refused document holds,
// and makes its last reading at or after that end. It refuses a retry within
// the hold, which stamps another attempt, a reading that no longer says the
// refusal, a failed read and a phase that ended, and holds nothing for a
// document with no attempt or no retry interval to hold past.
func TestARefusalHoldLastsTwoFailureRetriesFromTheAttempt(t *testing.T) {
	t.Parallel()
	const failureRetry = 45 * time.Second
	refusedAs := func(s *ptahv1alpha1.PtahSchema) (string, error) {
		return viewSecurityPlanRefused(s, planRefusedReadingView, 1)
	}
	refused := func(t *testing.T) *ptahv1alpha1.PtahSchema {
		t.Helper()
		schema := planRefusedReading(t)
		schema.Spec.Execution.FailureRetryInterval = metav1.Duration{Duration: failureRetry}
		return schema
	}
	// hold runs a hold that the wait noticed five seconds after the attempt,
	// on a clock each pause moves on, and answers each read with answer at
	// the time it reads.
	type held struct {
		readings int
		err      error
		readAt   []time.Time
		paused   []time.Duration
	}
	hold := func(t *testing.T, schema *ptahv1alpha1.PtahSchema, answer func(time.Time) (*ptahv1alpha1.PtahSchema, error),
		pause func(time.Duration) error) held {
		t.Helper()
		var outcome held
		now := time.Date(2026, 8, 30, 12, 0, 5, 0, time.UTC)
		if attempt := schema.Status.LastAttemptTime; attempt != nil {
			now = attempt.Add(5 * time.Second)
		}
		outcome.readings, outcome.err = holdSchemaRefusal(schema, refusedAs, func() (*ptahv1alpha1.PtahSchema, error) {
			outcome.readAt = append(outcome.readAt, now)
			return answer(now)
		}, func() time.Time { return now }, func(d time.Duration) error {
			outcome.paused = append(outcome.paused, d)
			if pause != nil {
				if err := pause(d); err != nil {
					return err
				}
			}
			now = now.Add(d)
			return nil
		})
		return outcome
	}
	unchanged := func(t *testing.T) func(time.Time) (*ptahv1alpha1.PtahSchema, error) {
		return func(time.Time) (*ptahv1alpha1.PtahSchema, error) { return refused(t), nil }
	}

	t.Run("the refusal stands", func(t *testing.T) {
		t.Parallel()
		schema := refused(t)
		outcome := hold(t, schema, unchanged(t), nil)
		if outcome.err != nil {
			t.Fatalf("holdSchemaRefusal() refused a refusal that stood: %v", outcome.err)
		}
		end := schema.Status.LastAttemptTime.Add(2 * failureRetry)
		last := outcome.readAt[len(outcome.readAt)-1]
		if last.Before(end) || !outcome.readAt[len(outcome.readAt)-2].Before(end) {
			t.Fatalf("the hold read at %v, want its last reading the first at or after %v", outcome.readAt, end)
		}
		if outcome.readings != len(outcome.readAt) {
			t.Fatalf("the hold reports %d readings and made %d", outcome.readings, len(outcome.readAt))
		}
		for _, pause := range outcome.paused {
			if pause != refusalHoldPause {
				t.Fatalf("the hold paused %v, want %v between readings", pause, refusalHoldPause)
			}
		}
	})

	for _, row := range []struct {
		name   string
		answer func(*testing.T, *ptahv1alpha1.PtahSchema) func(time.Time) (*ptahv1alpha1.PtahSchema, error)
		pause  func(time.Duration) error
	}{
		{name: "a retry at the first failure retry", answer: func(t *testing.T, s *ptahv1alpha1.PtahSchema) func(time.Time) (*ptahv1alpha1.PtahSchema, error) {
			retry := s.Status.LastAttemptTime.Add(failureRetry)
			return func(at time.Time) (*ptahv1alpha1.PtahSchema, error) {
				reading := refused(t)
				if !at.Before(retry) {
					stamped := metav1.NewTime(retry)
					reading.Status.LastAttemptTime = &stamped
				}
				return reading, nil
			}
		}},
		{name: "a retry just before the hold ends", answer: func(t *testing.T, s *ptahv1alpha1.PtahSchema) func(time.Time) (*ptahv1alpha1.PtahSchema, error) {
			retry := s.Status.LastAttemptTime.Add(2*failureRetry - time.Second)
			return func(at time.Time) (*ptahv1alpha1.PtahSchema, error) {
				reading := refused(t)
				if !at.Before(retry) {
					stamped := metav1.NewTime(retry)
					reading.Status.LastAttemptTime = &stamped
				}
				return reading, nil
			}
		}},
		{name: "the retry it replaced", answer: func(t *testing.T, s *ptahv1alpha1.PtahSchema) func(time.Time) (*ptahv1alpha1.PtahSchema, error) {
			return func(time.Time) (*ptahv1alpha1.PtahSchema, error) {
				reading := refused(t)
				reading.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationPlan, ID: "plan", Attempt: 2}
				return reading, nil
			}
		}},
		{name: "a read that failed", answer: func(*testing.T, *ptahv1alpha1.PtahSchema) func(time.Time) (*ptahv1alpha1.PtahSchema, error) {
			return func(time.Time) (*ptahv1alpha1.PtahSchema, error) { return nil, errors.New("connection refused") }
		}},
		{name: "a phase that ended", answer: func(t *testing.T, _ *ptahv1alpha1.PtahSchema) func(time.Time) (*ptahv1alpha1.PtahSchema, error) {
			return unchanged(t)
		}, pause: func(time.Duration) error { return errors.New("the phase ended") }},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema := refused(t)
			if outcome := hold(t, schema, row.answer(t, schema), row.pause); outcome.err == nil {
				t.Fatalf("holdSchemaRefusal() accepted it after %d readings", outcome.readings)
			}
		})
	}

	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchema){
		"no attempt":       func(s *ptahv1alpha1.PtahSchema) { s.Status.LastAttemptTime = nil },
		"no failure retry": func(s *ptahv1alpha1.PtahSchema) { s.Spec.Execution.FailureRetryInterval = metav1.Duration{} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := refused(t)
			edit(schema)
			if outcome := hold(t, schema, unchanged(t), nil); outcome.err == nil || len(outcome.readAt) != 0 {
				t.Fatalf("holdSchemaRefusal() held %d readings with error %v, want a refusal before any reading",
					len(outcome.readAt), outcome.err)
			}
		})
	}
}
