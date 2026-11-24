package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestAlBlockedFixtureIsAnIsolatedRefusedCopy(t *testing.T) {
	t.Parallel()
	template := &ptahv1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "producer", UID: "producer-uid", Generation: 3},
		Spec: ptahv1.PtahSchemaSpec{
			Suspend:  true,
			Interval: metav1.Duration{Duration: 2 * time.Minute},
			Dev:      &ptahv1.DatabaseTargetRef{},
			Target: ptahv1.DatabaseTargetSpec{
				RealmRef:        &ptahv1.PtahRealmReference{Name: "shared"},
				CoordinationKey: "e2e/producer",
				URLFrom:         &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "producer-db"}, Key: "url"},
			},
			Desired: ptahv1.SourceSpec{VerificationPolicyFrom: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "producer-policy"}, Key: "policy.yaml",
			}},
			Policy: ptahv1.ReconciliationPolicy{Apply: ptahv1.ApplyPolicyAlways, LockTimeout: metav1.Duration{Duration: time.Minute}},
		},
		Status: ptahv1.PtahSchemaStatus{Phase: ptahv1.PhaseAwaitingApproval},
	}
	fixture, err := alBlockedFixture(template, "blocked", "blocked-db", "blocked-policy")
	if err != nil {
		t.Fatal(err)
	}
	spec := fixture.Spec
	switch {
	case fixture.UID != "" || fixture.Generation != 0 || fixture.Status.Phase != "" || fixture.Namespace != "ns" || fixture.Name != "blocked":
		t.Fatal("the fixture inherited the producer's identity or status")
	case spec.Suspend || spec.Interval.Duration != alBlockedInterval || spec.Dev != nil:
		t.Fatal("the fixture does not refresh at the row's interval")
	case spec.Target.RealmRef != nil || spec.Target.CoordinationKey == template.Spec.Target.CoordinationKey || spec.Target.URLFrom.Name != "blocked-db":
		t.Fatal("the fixture shares the producer's database or lock")
	case spec.Desired.VerificationPolicyFrom.Name != "blocked-policy" || spec.Desired.VerificationPolicyFrom.Key != "policy.yaml":
		t.Fatal("the fixture does not read the refusing policy")
	case spec.Policy.Apply != ptahv1.ApplyPolicyOnApproval || spec.Policy.LockTimeout.Duration != time.Minute:
		t.Fatal("the fixture could apply without an approval, or lost the producer's lock timeout")
	}
	if template.Spec.Desired.VerificationPolicyFrom.Name != "producer-policy" || template.Spec.Target.URLFrom.Name != "producer-db" || !template.Spec.Suspend {
		t.Fatal("the producer was modified")
	}
	if alBlockedInterval >= alBlockedAfter {
		t.Fatal("the interval no longer refreshes the schema before the threshold, so no rule over the phase is refused")
	}
	for name, build := range map[string]func() (*ptahv1.PtahSchema, error){
		"no template": func() (*ptahv1.PtahSchema, error) {
			return alBlockedFixture(nil, "blocked", "blocked-db", "blocked-policy")
		},
		"no policy on the producer": func() (*ptahv1.PtahSchema, error) {
			copy := template.DeepCopy()
			copy.Spec.Desired.VerificationPolicyFrom = nil
			return alBlockedFixture(copy, "blocked", "blocked-db", "blocked-policy")
		},
		"no refusing policy": func() (*ptahv1.PtahSchema, error) { return alBlockedFixture(template, "blocked", "blocked-db", "") },
	} {
		if _, err := build(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestAlBlockedLeftoversNameOnlyDatedResourcesThatRun(t *testing.T) {
	t.Parallel()
	since := metav1.NewTime(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC))
	schema := func(name string, suspend bool, since *metav1.Time) ptahv1.PtahSchema {
		return ptahv1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name}, Spec: ptahv1.PtahSchemaSpec{Suspend: suspend},
			Status: ptahv1.PtahSchemaStatus{BlockedSince: since}}
	}
	migration := func(name string, suspend bool, since *metav1.Time) ptahv1.PtahMigration {
		return ptahv1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name}, Spec: ptahv1.PtahMigrationSpec{Suspend: suspend},
			Status: ptahv1.PtahMigrationStatus{BlockedSince: since}}
	}
	got := alBlockedLeftovers(
		[]ptahv1.PtahSchema{schema("undated", false, nil), schema("suspended", true, &since), schema("dated", false, &since)},
		[]ptahv1.PtahMigration{migration("undated", false, nil), migration("suspended", true, &since), migration("dated", false, &since)},
	)
	want := []string{"PtahMigration ns/dated since 2026-11-20T10:00:00Z", "PtahSchema ns/dated since 2026-11-20T10:00:00Z"}
	if !slices.Equal(got, want) {
		t.Fatalf("leftovers %q, want %q", got, want)
	}
	if got := alBlockedLeftovers([]ptahv1.PtahSchema{schema("undated", false, nil)}, nil); len(got) != 0 {
		t.Fatalf("an undated schema was named: %q", got)
	}
}

func TestAlBlockedUnresolvedPairsEachFiringWithItsResolution(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	delivery := func(alert, status, family string, starts time.Time) alDelivery {
		return alDelivery{AlertName: alert, Status: status, Labels: map[string]string{"family": family}, StartsAt: starts}
	}
	for name, row := range map[string]struct {
		deliveries []alDelivery
		want       []string
	}{
		"resolved": {[]alDelivery{
			delivery(alBlockedAlert, "firing", "migration", start), delivery(alBlockedAlert, "firing", "migration", start),
			delivery(alBlockedAlert, "resolved", "migration", start),
		}, nil},
		"still firing": {[]alDelivery{delivery(alBlockedAlert, "firing", "migration", start)}, []string{"migration since 2026-11-20T10:00:00Z"}},
		"resolved another incident": {[]alDelivery{
			delivery(alBlockedAlert, "firing", "migration", start), delivery(alBlockedAlert, "resolved", "migration", start.Add(-time.Minute)),
		}, []string{"migration since 2026-11-20T10:00:00Z"}},
		"resolved another family": {[]alDelivery{
			delivery(alBlockedAlert, "firing", "migration", start), delivery(alBlockedAlert, "resolved", "schema", start),
		}, []string{"migration since 2026-11-20T10:00:00Z"}},
		"fired again after it resolved": {[]alDelivery{
			delivery(alBlockedAlert, "firing", "schema", start), delivery(alBlockedAlert, "resolved", "schema", start),
			delivery(alBlockedAlert, "firing", "schema", start.Add(time.Minute)),
		}, []string{"schema since 2026-11-20T10:01:00Z"}},
		"another alert": {[]alDelivery{delivery(alOverdueAlert, "firing", "schema", start)}, nil},
	} {
		if got := alBlockedUnresolved(row.deliveries); !slices.Equal(got, row.want) {
			t.Errorf("%s: open %q, want %q", name, got, row.want)
		}
	}
}

// abHistory is the stored states a blocked schema writes, from the refusal
// the operator recorded: the first pass, a refresh pass with a handoff
// between its steps, the spec edit, and the pass that ends at
// AwaitingApproval.
type abHistory struct {
	states                  []*ptahv1.PtahSchema
	since, refresh, settled time.Time
}

func abStates(t *testing.T) func() abHistory {
	t.Helper()
	read := nrRead[ptahv1.PtahSchema](t, "native-refusal-schema.json")
	return func() abHistory {
		refused, _ := read()
		base := refused.Status.LastAttemptTime.Time
		h := abHistory{since: base.Add(time.Second), refresh: base.Add(90 * time.Second), settled: base.Add(4 * time.Minute)}
		version := 0
		next := func(mutate func(*ptahv1.PtahSchema)) {
			state := refused.DeepCopy()
			version++
			state.ResourceVersion = strconv.Itoa(version)
			mutate(state)
			h.states = append(h.states, state)
		}
		dated := metav1.NewTime(h.since)
		claim := func(s *ptahv1.PtahSchema, phase ptahv1.ReconciliationPhase, operation ptahv1.OperationType, id string, at time.Time) {
			s.Status.Phase = phase
			s.Status.ActiveOperation = &ptahv1.ActiveOperationStatus{Type: operation, ID: id, StartedAt: metav1.NewTime(at)}
		}
		next(func(s *ptahv1.PtahSchema) {
			claim(s, ptahv1.PhaseResolving, ptahv1.OperationResolve, "first", base.Add(-20*time.Second))
		})
		next(func(s *ptahv1.PtahSchema) {
			claim(s, ptahv1.PhaseVerifying, ptahv1.OperationVerify, "first-verify", base.Add(-10*time.Second))
		})
		next(func(s *ptahv1.PtahSchema) { s.Status.BlockedSince = &dated })
		next(func(s *ptahv1.PtahSchema) {
			s.Status.BlockedSince = &dated
			claim(s, ptahv1.PhaseResolving, ptahv1.OperationResolve, "refresh", h.refresh)
		})
		next(func(s *ptahv1.PtahSchema) {
			s.Status.BlockedSince = &dated
			s.Status.Phase = ptahv1.PhaseVerifying
		})
		next(func(s *ptahv1.PtahSchema) {
			s.Status.BlockedSince = &dated
			claim(s, ptahv1.PhaseVerifying, ptahv1.OperationVerify, "refresh-verify", h.refresh.Add(10*time.Second))
		})
		next(func(s *ptahv1.PtahSchema) { s.Status.BlockedSince = &dated })
		next(func(s *ptahv1.PtahSchema) {
			s.Generation = 2
			s.Status.BlockedSince = &dated
			s.Status.Phase = ptahv1.PhasePending
		})
		next(func(s *ptahv1.PtahSchema) {
			s.Generation, s.Status.ObservedGeneration = 2, 2
			s.Status.BlockedSince = &dated
			claim(s, ptahv1.PhasePlanning, ptahv1.OperationPlan, "edited-plan", h.settled.Add(-20*time.Second))
		})
		next(func(s *ptahv1.PtahSchema) {
			s.Generation, s.Status.ObservedGeneration = 2, 2
			s.Status.Phase = ptahv1.PhaseAwaitingApproval
			s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{
				Type: ptahv1.ConditionApprovalRequired, Status: metav1.ConditionTrue, ObservedGeneration: 2,
				Reason: string(ptahv1.ReasonPlanReady), LastTransitionTime: metav1.NewTime(h.settled),
			})
		})
		return h
	}
}

func TestAlBlockedHistoryKeepsOneDateUntilTheSettledPass(t *testing.T) {
	t.Parallel()
	read := abStates(t)
	h := read()
	uid := h.states[0].UID
	walk, err := alBlockedHistory(h.states, uid, h.refresh.Add(-time.Second))
	if err != nil {
		t.Fatalf("the recorded history was refused: %v", err)
	}
	if !walk.since.Equal(h.since) || walk.generation != 1 || walk.refresh.id != "refresh" || !walk.refresh.started.Equal(h.refresh) ||
		walk.refresh.blockedAgain != "7" || !walk.settled.Equal(h.settled) || walk.settledGeneration != 2 {
		t.Fatalf("the walk read %+v", walk)
	}

	// Readings that are not complete yet: the row waits on them.
	for name, row := range map[string]struct {
		states  func(abHistory) []*ptahv1.PtahSchema
		after   time.Time
		refresh bool
		settled bool
	}{
		"before the refresh ends":                  {func(h abHistory) []*ptahv1.PtahSchema { return h.states[:6] }, h.refresh, false, false},
		"before the edit settles":                  {func(h abHistory) []*ptahv1.PtahSchema { return h.states[:9] }, h.refresh, true, false},
		"no refresh claimed after the alert fired": {func(h abHistory) []*ptahv1.PtahSchema { return h.states }, h.refresh.Add(time.Second), false, true},
		"the Resolving state never delivered": {func(h abHistory) []*ptahv1.PtahSchema {
			return slices.Delete(slices.Clone(h.states), 3, 4)
		}, h.refresh, false, true},
		"a refresh only after the edit": {func(h abHistory) []*ptahv1.PtahSchema {
			states := slices.Clone(h.states)
			for _, s := range states[3:7] {
				s.Generation = 2
			}
			return states
		}, h.refresh, false, true},
	} {
		h := read()
		walk, err := alBlockedHistory(row.states(h), uid, row.after)
		if err != nil {
			t.Errorf("%s: refused: %v", name, err)
			continue
		}
		if (walk.refresh.id != "") != row.refresh || walk.settled.IsZero() == row.settled {
			t.Errorf("%s: read %+v", name, walk)
		}
	}

	// Readings the operator must never write.
	moved := metav1.NewTime(h.since.Add(time.Second))
	for name, mutate := range map[string]func(abHistory) []*ptahv1.PtahSchema{
		"the refresh claim moves the date": func(h abHistory) []*ptahv1.PtahSchema { h.states[3].Status.BlockedSince = &moved; return h.states },
		"the refresh claim clears the date": func(h abHistory) []*ptahv1.PtahSchema {
			h.states[3].Status.BlockedSince = nil
			return h.states
		},
		"the handoff clears the date": func(h abHistory) []*ptahv1.PtahSchema { h.states[4].Status.BlockedSince = nil; return h.states },
		"the refresh re-dates the block": func(h abHistory) []*ptahv1.PtahSchema {
			for _, s := range h.states[3:9] {
				s.Status.BlockedSince = &moved
			}
			return h.states
		},
		"the spec edit clears the date": func(h abHistory) []*ptahv1.PtahSchema {
			h.states[7].Status.BlockedSince = nil
			return h.states
		},
		"a claim dates the block": func(h abHistory) []*ptahv1.PtahSchema {
			dated := metav1.NewTime(h.since)
			h.states[1].Status.BlockedSince = &dated
			return h.states
		},
		"the settled pass keeps the date": func(h abHistory) []*ptahv1.PtahSchema {
			dated := metav1.NewTime(h.since)
			h.states[9].Status.BlockedSince = &dated
			return append(h.states, h.states[9])
		},
		"a date after the settled pass": func(h abHistory) []*ptahv1.PtahSchema {
			again := h.states[9].DeepCopy()
			again.Status.BlockedSince = &moved
			return append(h.states, again)
		},
		"cleared at the stale generation": func(h abHistory) []*ptahv1.PtahSchema {
			h.states[9].Status.Conditions[len(h.states[9].Status.Conditions)-1].ObservedGeneration = 1
			return h.states
		},
		"cleared with nothing waiting for approval": func(h abHistory) []*ptahv1.PtahSchema {
			h.states[9].Status.Phase = ptahv1.PhaseInSync
			return h.states
		},
		"another schema only": func(h abHistory) []*ptahv1.PtahSchema {
			for _, s := range h.states {
				s.UID = "another"
			}
			return h.states
		},
	} {
		h := read()
		if _, err := alBlockedHistory(mutate(h), uid, h.refresh); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := alBlockedHistory(h.states, "", h.refresh); err == nil {
		t.Error("a walk without the schema's UID was accepted")
	}
}

func TestAlBlockedDeliveredAtTheThresholdAfterTheDate(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	onTime := alDelivery{StartsAt: since.Add(alBlockedAfter + 5*time.Second), ReceivedAt: since.Add(alBlockedAfter + 12*time.Second)}
	if !alBlockedDelivered(onTime, since) {
		t.Fatal("an on-time notification was refused")
	}
	for name, row := range map[string]struct {
		firing alDelivery
		since  time.Time
	}{
		// A rule over the phase, or over a date a refresh pass moved
		// forward, fires before the persisted date allows.
		"fired before the threshold": {alDelivery{StartsAt: since.Add(alBlockedAfter - refusalClockSkew - time.Second), ReceivedAt: since.Add(alBlockedAfter)}, since},
		"received late":              {alDelivery{StartsAt: onTime.StartsAt, ReceivedAt: since.Add(alBlockedAfter + alDetectionSlack + time.Second)}, since},
		"received before it started": {alDelivery{StartsAt: onTime.StartsAt, ReceivedAt: onTime.StartsAt.Add(-time.Second)}, since},
		"no date":                    {onTime, time.Time{}},
	} {
		if alBlockedDelivered(row.firing, row.since) {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestAlBlockedClearedSpansTheRefreshPass(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 20, 10, 1, 5, 0, time.UTC)
	refreshed := start.Add(20 * time.Second)
	settled := start.Add(3 * time.Minute)
	firing := alDelivery{StartsAt: start}
	resolved := alDelivery{StartsAt: start, EndsAt: settled.Add(alStateReadGapHold + 5*time.Second), ReceivedAt: settled.Add(alStateReadGapHold + 15*time.Second)}
	if !alBlockedCleared(firing, resolved, refreshed, settled) {
		t.Fatal("a resolution after the date cleared was refused")
	}
	for name, mutate := range map[string]func(firing, resolved *alDelivery, refreshed, settled *time.Time){
		// The incident a refresh pass ended: its resolution names an end
		// before the date cleared.
		"resolved during the block": func(_, r *alDelivery, refreshed, _ *time.Time) { r.EndsAt = refreshed.Add(10 * time.Second) },
		"another incident":          func(_, r *alDelivery, _, _ *time.Time) { r.StartsAt = r.StartsAt.Add(time.Minute) },
		"fired after the refresh": func(f, r *alDelivery, refreshed, _ *time.Time) {
			f.StartsAt = refreshed.Add(time.Second)
			r.StartsAt = f.StartsAt
		},
		"received late": func(_, r *alDelivery, _, settled *time.Time) {
			r.ReceivedAt = settled.Add(alStateReadGapHold + alDetectionSlack + time.Second)
		},
		"received before it ended": func(_, r *alDelivery, _, _ *time.Time) { r.ReceivedAt = r.EndsAt.Add(-time.Second) },
		"never settled":            func(_, _ *alDelivery, _, settled *time.Time) { *settled = time.Time{} },
		"no refresh pass":          func(_, _ *alDelivery, refreshed, _ *time.Time) { *refreshed = time.Time{} },
	} {
		f, r, rf, s := firing, resolved, refreshed, settled
		mutate(&f, &r, &rf, &s)
		if alBlockedCleared(f, r, rf, s) {
			t.Errorf("%s was accepted", name)
		}
	}
}

// abAgeSeries is one manager's native samples of the blocked age, every
// scrape interval from first to last, each read by age.
func abAgeSeries(first, last time.Time, age func(time.Time) float64, labels map[string]string) alAdmissionSeries {
	metric := map[string]string{"__name__": alBlockedMetric, "family": "schema", "job": alScrapeJob, "instance": "10.0.0.1:8443"}
	for key, value := range labels {
		metric[key] = value
	}
	series := alAdmissionSeries{Metric: metric}
	for at := first; !at.After(last); at = at.Add(alScrapeInterval) {
		series.Values = append(series.Values, []json.RawMessage{
			json.RawMessage(fmt.Sprintf("%.3f", float64(at.UnixMilli())/1000)),
			json.RawMessage(strconv.Quote(strconv.FormatFloat(age(at), 'f', 3, 64))),
		})
	}
	return series
}

func TestAlBlockedAgesHoldTheGaugeToThePersistedDate(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	from, to := since.Add(90*time.Second), since.Add(2*time.Minute)
	queriedAt := to.Add(10 * time.Second)
	first, last := since.Add(500*time.Millisecond), queriedAt.Add(-time.Second)
	dated := func(at time.Time) float64 { return at.Sub(since).Seconds() + 0.4 }
	answer := func(rows ...alAdmissionSeries) []byte {
		body, err := alMatrixAnswer(rows)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	count, err := alBlockedAges(answer(abAgeSeries(first, last, dated, nil), abAgeSeries(first, last, dated, map[string]string{"instance": "10.0.0.2:8443"})), since, from, to, queriedAt)
	if err != nil || count != 12 {
		t.Fatalf("two replicas dated by the persisted date read %d samples: %v", count, err)
	}
	for name, body := range map[string][]byte{
		// The phase gauge's reading: the age restarts when the refresh
		// claims its Resolve.
		"an age the refresh reset": answer(abAgeSeries(first, last, func(at time.Time) float64 {
			if at.Before(from) {
				return dated(at)
			}
			return at.Sub(from).Seconds()
		}, nil)),
		"an age from another date":              answer(abAgeSeries(first, last, func(at time.Time) float64 { return at.Sub(since.Add(30 * time.Second)).Seconds() }, nil)),
		"one replica off the date":              answer(abAgeSeries(first, last, dated, nil), abAgeSeries(first, last, func(at time.Time) float64 { return dated(at) + 10 }, map[string]string{"instance": "10.0.0.2:8443"})),
		"no series":                             answer(),
		"a series that ended before the window": answer(abAgeSeries(first, from.Add(-time.Second), dated, nil)),
		"a series that ended inside the window": answer(abAgeSeries(first, to.Add(-2*alScrapeInterval), dated, nil)),
		"a missed scrape inside the window": answer(func() alAdmissionSeries {
			s := abAgeSeries(first, last, dated, nil)
			s.Values = slices.Delete(s.Values, 20, 21)
			return s
		}()),
		"another family": answer(abAgeSeries(first, last, dated, map[string]string{"family": "migration"})),
		"another metric": answer(abAgeSeries(first, last, dated, map[string]string{"__name__": "ptah_operator_overdue_seconds"})),
		"not an answer":  []byte(`{"status":"error"}`),
	} {
		if _, err := alBlockedAges(body, since, from, to, queriedAt); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := alBlockedAges(answer(abAgeSeries(first, last, dated, nil)), since, to, from, queriedAt); err == nil {
		t.Error("an empty window was accepted")
	}
	if _, err := alBlockedAges(answer(abAgeSeries(first, last, dated, nil)), since, from, queriedAt.Add(time.Second), queriedAt); err == nil {
		t.Error("a window past the read was accepted")
	}
	if !strings.HasPrefix(alHistorySnapshotQuery(alScrapeJob, `,family="schema"`, []string{alBlockedMetric}), `{job="ptah-operator",__name__=~"ptah_operator_blocked_seconds",family="schema"}[`) {
		t.Fatal("the row's history query does not select the schema family's blocked age")
	}
}

func TestAlBlockedHistoryNeedsTheSchemasUID(t *testing.T) {
	t.Parallel()
	if _, err := alBlockedHistory(nil, types.UID("uid"), time.Time{}); !errors.Is(err, errAlBlockedUnseen) {
		t.Fatalf("an empty history = %v, want %v", err, errAlBlockedUnseen)
	}
	// Another schema's states are not this one's: the walk still has seen
	// nothing, which the row waits through rather than fails on.
	h := abStates(t)()
	if _, err := alBlockedHistory(h.states, types.UID("another"), time.Time{}); !errors.Is(err, errAlBlockedUnseen) {
		t.Fatalf("a history of another schema = %v, want %v", err, errAlBlockedUnseen)
	}
	// A refused history is not an unseen one, so the row's wait cannot
	// wait through it: the refresh claim moves the date.
	moved := metav1.NewTime(h.since.Add(time.Minute))
	h.states[3].Status.BlockedSince = &moved
	if _, err := alBlockedHistory(h.states[:4], h.states[0].UID, time.Time{}); err == nil || errors.Is(err, errAlBlockedUnseen) {
		t.Fatalf("a history that moved the date = %v, want a refusal other than %v", err, errAlBlockedUnseen)
	}
}
