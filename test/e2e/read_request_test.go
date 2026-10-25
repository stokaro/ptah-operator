package e2e

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestHistoryReadOnRequestRefusesAReadingTheRequestDidNotCause(t *testing.T) {
	t.Parallel()
	requested := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	interval := requested.Add(55 * time.Minute)
	const value = "2026-10-24T12:00:00Z"
	reading := func(change func(*ptahv1alpha1.PtahMigration)) *ptahv1alpha1.PtahMigration {
		migration := &ptahv1alpha1.PtahMigration{Status: ptahv1alpha1.PtahMigrationStatus{
			LastReadRequest: value,
			History: &ptahv1alpha1.MigrationHistoryStatus{
				ObservedAt: metav1.NewTime(requested.Add(40 * time.Second)), CurrentVersion: 2,
			},
		}}
		change(migration)
		return migration
	}
	if !historyReadOnRequest(reading(func(*ptahv1alpha1.PtahMigration) {}), value, requested, interval, 2) {
		t.Fatal("a history read 40 seconds after the request it answered was refused")
	}
	for _, test := range []struct {
		name   string
		change func(*ptahv1alpha1.PtahMigration)
	}{
		{"no answer recorded", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastReadRequest = "" }},
		{"another request answered", func(m *ptahv1alpha1.PtahMigration) { m.Status.LastReadRequest = "2026-10-24T11:00:00Z" }},
		{"no history", func(m *ptahv1alpha1.PtahMigration) { m.Status.History = nil }},
		{"a history read before the request", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.History.ObservedAt = metav1.NewTime(requested.Add(-time.Second))
		}},
		{"a history read in the request's own second", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.History.ObservedAt = metav1.NewTime(requested)
		}},
		{"a history read past the bound", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.History.ObservedAt = metav1.NewTime(requested.Add(readRequestBound + time.Second))
		}},
		{"the read the interval set", func(m *ptahv1alpha1.PtahMigration) {
			m.Status.History.ObservedAt = metav1.NewTime(interval)
		}},
		{"another version", func(m *ptahv1alpha1.PtahMigration) { m.Status.History.CurrentVersion = 3 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if historyReadOnRequest(reading(test.change), value, requested, interval, 2) {
				t.Fatal("historyReadOnRequest accepted it")
			}
		})
	}
	if historyReadOnRequest(reading(func(*ptahv1alpha1.PtahMigration) {}), value, requested, requested.Add(30*time.Second), 2) {
		t.Fatal("a read after the interval's own read was taken for one the request caused")
	}
}

func TestObservedOnRequestDatesTheObservationByItsJob(t *testing.T) {
	t.Parallel()
	requested := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	interval := requested.Add(30 * time.Minute)
	job := func(created string) observedJob {
		return observedJob{UID: "uid-" + created, Name: "ptah-observe-" + created, Created: created, Operation: "observe"}
	}
	if err := observedOnRequest([]observedJob{job("2026-10-24T12:00:20Z")}, requested, interval); err != nil {
		t.Fatalf("an Observe Job 20 seconds after the request was refused: %v", err)
	}
	if err := observedOnRequest([]observedJob{job("2026-10-24T12:00:00Z")}, requested, interval); err != nil {
		t.Fatalf("an Observe Job in the request's own second was refused: %v", err)
	}
	for _, test := range []struct {
		name string
		jobs []observedJob
		want string
	}{
		{"none", nil, "0 Observe Jobs"},
		{"two", []observedJob{job("2026-10-24T12:00:20Z"), job("2026-10-24T12:00:40Z")}, "2 Observe Jobs"},
		{"one before the request", []observedJob{job("2026-10-24T11:59:59Z")}, "before the request"},
		{"one past the bound", []observedJob{job("2026-10-24T12:04:00Z")}, "more than"},
		{"one at the interval", []observedJob{job("2026-10-24T12:30:00Z")}, ""},
		{"one with no time", []observedJob{job("yesterday")}, "no creation time"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := observedOnRequest(test.jobs, requested, interval)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("observedOnRequest() = %v, want an error naming %q", err, test.want)
			}
		})
	}
	if err := observedOnRequest([]observedJob{job("2026-10-24T12:01:00Z")}, requested, requested.Add(time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "interval would have read") {
		t.Fatalf("an Observe Job at the interval's own read was taken for one the request caused: %v", err)
	}
}

func TestSchemaObservedOnRequestWaitsForTheReadToEnd(t *testing.T) {
	t.Parallel()
	requested := time.Date(2026, 10, 25, 11, 58, 41, 0, time.UTC)
	interval := requested.Add(55 * time.Minute)
	const value = "2026-10-25T11:58:41Z"
	reading := func(change func(*ptahv1alpha1.PtahSchema)) *ptahv1alpha1.PtahSchema {
		observed := metav1.NewTime(requested.Add(35 * time.Second))
		schema := &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{
			Phase:           ptahv1alpha1.PhaseAwaitingApproval,
			LastReadRequest: value,
			Plan:            &ptahv1alpha1.CurrentPlanStatus{Name: "ptah-plan-d2b03b805b4f69da49ca2eb6"},
			Target:          ptahv1alpha1.TargetStatus{LastObservedAt: &observed},
		}}
		change(schema)
		return schema
	}
	if !schemaObservedOnRequest(reading(func(*ptahv1alpha1.PtahSchema) {}), value, requested, interval) {
		t.Fatal("a schema back at its gate 35 seconds after the request it answered was refused")
	}
	for _, test := range []struct {
		name   string
		change func(*ptahv1alpha1.PtahSchema)
	}{
		// The document the 0.2.8 master run approved against: the request
		// answered and its Observe Job created, not yet run, so the last
		// observation is the one before the request.
		{"the read under way, its Observe Job not yet run", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Phase = ptahv1alpha1.PhaseObserving
			s.Status.Target.LastObservedAt = new(metav1.NewTime(requested.Add(-25 * time.Second)))
		}},
		{"the gate the read started from", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Target.LastObservedAt = new(metav1.NewTime(requested.Add(-25 * time.Second)))
		}},
		{"observed, and planning again", func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhasePlanning }},
		{"an operation running", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationPlan}
		}},
		{"no plan", func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = nil }},
		{"no answer recorded", func(s *ptahv1alpha1.PtahSchema) { s.Status.LastReadRequest = "" }},
		{"another request answered", func(s *ptahv1alpha1.PtahSchema) { s.Status.LastReadRequest = "2026-10-25T11:00:00Z" }},
		{"never observed", func(s *ptahv1alpha1.PtahSchema) { s.Status.Target.LastObservedAt = nil }},
		{"observed in the request's own second", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Target.LastObservedAt = new(metav1.NewTime(requested))
		}},
		{"observed past the bound", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Target.LastObservedAt = new(metav1.NewTime(requested.Add(readRequestBound + time.Second)))
		}},
		{"observed by the interval's read", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Target.LastObservedAt = new(metav1.NewTime(interval))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if schemaObservedOnRequest(reading(test.change), value, requested, interval) {
				t.Fatal("schemaObservedOnRequest accepted it")
			}
		})
	}
}
