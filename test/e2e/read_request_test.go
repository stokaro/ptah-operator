package e2e

import (
	"encoding/json"
	"os"
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

// TestRecoveryHistoryOnRequestRefusesAReadingTheRequestDidNotCause holds the
// runner protocol row's wait to the History reads the uncertain row captured
// beside an Unknown record it had not settled. Those were read on the
// interval, so as stored they answer no request and are refused; a copy that
// answered a request 40 seconds before the read is accepted, and each copy
// that changes what the row asserts is refused.
func TestRecoveryHistoryOnRequestRefusesAReadingTheRequestDidNotCause(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../testdata/e2e/readings/uncertain-migration-history-dirty.json")
	if err != nil {
		t.Fatal(err)
	}
	var readings map[string]*ptahv1alpha1.PtahMigration
	if err := json.Unmarshal(content, &readings); err != nil {
		t.Fatal(err)
	}
	if len(readings) != 2 || readings["postgresql"] == nil || readings["mysql"] == nil {
		t.Fatal("both native engine readings are required")
	}
	for engine, captured := range readings {
		t.Run(engine, func(t *testing.T) {
			t.Parallel()
			history, unresolved := captured.Status.History, captured.Status.UnresolvedRun
			if history == nil || history.ObservedAt.IsZero() || unresolved == nil || captured.Status.ActiveOperation != nil {
				t.Fatal("the capture no longer holds a History read beside an unresolved run")
			}
			requested := history.ObservedAt.Add(-40 * time.Second)
			value := requested.Format(time.RFC3339)
			intervalRead := requested.Add(55 * time.Minute)
			fingerprint, operation := history.Fingerprint, unresolved.OperationID
			if recoveryHistoryOnRequest(captured, value, requested, intervalRead, fingerprint, operation) {
				t.Fatal("the captured interval read was taken for a read on request")
			}
			answered := func(change func(*ptahv1alpha1.PtahMigration)) *ptahv1alpha1.PtahMigration {
				migration := captured.DeepCopy()
				migration.Status.LastReadRequest = value
				change(migration)
				return migration
			}
			if !recoveryHistoryOnRequest(answered(func(*ptahv1alpha1.PtahMigration) {}), value, requested, intervalRead, fingerprint, operation) {
				t.Fatal("a History read 40 seconds after the request it answered was refused")
			}
			// Inside the bound, so only the scheduled read can refuse it.
			if recoveryHistoryOnRequest(answered(func(*ptahv1alpha1.PtahMigration) {}), value, requested, requested.Add(30*time.Second), fingerprint, operation) {
				t.Fatal("a History read after the one the refused run scheduled was taken for one the request caused")
			}
			for name, change := range map[string]func(*ptahv1alpha1.PtahMigration){
				"another request answered": func(m *ptahv1alpha1.PtahMigration) {
					m.Status.LastReadRequest = requested.Add(-time.Hour).Format(time.RFC3339)
				},
				"no history": func(m *ptahv1alpha1.PtahMigration) { m.Status.History = nil },
				"a history read before the request": func(m *ptahv1alpha1.PtahMigration) {
					m.Status.History.ObservedAt = metav1.NewTime(requested.Add(-time.Second))
				},
				"a history read in the request's own second": func(m *ptahv1alpha1.PtahMigration) {
					m.Status.History.ObservedAt = metav1.NewTime(requested)
				},
				"a history read past the bound": func(m *ptahv1alpha1.PtahMigration) {
					m.Status.History.ObservedAt = metav1.NewTime(requested.Add(readRequestBound + time.Second))
				},
				"an operation running": func(m *ptahv1alpha1.PtahMigration) {
					m.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{Type: ptahv1alpha1.MigrationOperationHistory}
				},
				"another reading of the revision table": func(m *ptahv1alpha1.PtahMigration) {
					m.Status.History.Fingerprint = "sha256:" + strings.Repeat("0", 64)
				},
				"the record settled": func(m *ptahv1alpha1.PtahMigration) { m.Status.UnresolvedRun = nil },
				"another run unresolved": func(m *ptahv1alpha1.PtahMigration) {
					m.Status.UnresolvedRun.OperationID = "sha256:" + strings.Repeat("1", 64)
				},
			} {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					if recoveryHistoryOnRequest(answered(change), value, requested, intervalRead, fingerprint, operation) {
						t.Fatal("recoveryHistoryOnRequest accepted it")
					}
				})
			}
		})
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
	if schemaObservedOnRequest(reading(func(*ptahv1alpha1.PtahSchema) {}), value, requested, requested.Add(30*time.Second)) {
		t.Fatal("an observation after the interval's own read was taken for one the request caused")
	}
}
