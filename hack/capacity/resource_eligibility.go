package main

import (
	"encoding/json"
	"fmt"
	"time"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Keep the identity and inputs of each freshness measurement. Suspension is
// an explicit exclusion; an approval gate is not, since it still refreshes.
type resourceFreshness struct {
	ActiveOperation        *cycleOperation `json:"activeOperation"`
	Family                 string          `json:"family"`
	Namespace              string          `json:"namespace"`
	Name                   string          `json:"name"`
	UID                    string          `json:"uid"`
	ResourceVersion        string          `json:"resourceVersion"`
	Generation             int64           `json:"generation"`
	ReadAt                 time.Time       `json:"readAt"`
	Suspended              bool            `json:"suspended"`
	Deleting               bool            `json:"deleting"`
	ObservedAt             *time.Time      `json:"observedAt"`
	NextReconciliationTime *time.Time      `json:"nextReconciliationTime"`
}

func readResourceFreshness(family string, object *unstructured.Unstructured, at time.Time, observedPath []string) (resourceFreshness, error) {
	r := resourceFreshness{Family: family, Namespace: object.GetNamespace(), Name: object.GetName(), UID: string(object.GetUID()), ResourceVersion: object.GetResourceVersion(), Generation: object.GetGeneration(), ReadAt: at, Deleting: object.GetDeletionTimestamp() != nil}
	if r.UID == "" || r.ResourceVersion == "" || r.Namespace == "" || r.Name == "" || r.Generation < 1 {
		return r, fmt.Errorf("freshness reading lacks resource identity: %s/%s", r.Namespace, r.Name)
	}
	var err error
	r.Suspended, _, err = unstructured.NestedBool(object.Object, "spec", "suspend")
	if err != nil {
		return r, err
	}
	for _, field := range []struct {
		path   []string
		target **time.Time
	}{{observedPath, &r.ObservedAt}, {[]string{"status", "nextReconciliationTime"}, &r.NextReconciliationTime}} {
		raw, found, err := unstructured.NestedString(object.Object, field.path...)
		if err != nil {
			return r, err
		}
		if !found || raw == "" {
			continue
		}
		value, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return r, fmt.Errorf("invalid freshness timestamp for %s/%s: %w", r.Namespace, r.Name, err)
		}
		*field.target = &value
	}
	claim, found, err := unstructured.NestedMap(object.Object, "status", "activeOperation")
	if err != nil {
		return r, err
	}
	if found {
		raw, err := json.Marshal(claim)
		if err != nil {
			return r, err
		}
		if err := json.Unmarshal(raw, &r.ActiveOperation); err != nil {
			return r, err
		}
		if !validFreshnessClaim(r) {
			return r, fmt.Errorf("invalid active claim in freshness reading for %s/%s", r.Namespace, r.Name)
		}
	}
	return r, nil
}

// A phase label cannot establish that work is in flight. Keep the persisted
// claim and validate it against the operation catalog the controllers use.
// JobUID is optional: a claim is persisted before its Job is created.
func validFreshnessClaim(r resourceFreshness) bool {
	op := r.ActiveOperation
	if op == nil {
		return true
	}
	if op.ID == "" || op.JobName == "" || op.StartedAt.IsZero() || op.StartedAt.After(r.ReadAt) {
		return false
	}
	switch r.Family {
	case "schema":
		return mutationlifecycle.SchemaOperation(operatorv1alpha1.OperationType(op.Type)).Known
	case "migration":
		return mutationlifecycle.MigrationOperation(operatorv1alpha1.MigrationOperationType(op.Type)).Known
	default:
		return false
	}
}

type freshnessCost struct {
	OutsideWindowReadings        int      `json:"outsideWindowReadings"`
	ScheduledReadings            int      `json:"scheduledReadings"`
	InFlightReadings             int      `json:"inFlightReadings"`
	ActiveOperationAgeMaxSeconds *float64 `json:"activeOperationAgeMaxSeconds"`
	EligibleReadings             int      `json:"eligibleReadings"`
	SuspendedReadings            int      `json:"suspendedReadings"`
	DeletingReadings             int      `json:"deletingReadings"`
	MissingObservations          int      `json:"missingObservations"`
	MissingDeadlines             int      `json:"missingDeadlines"`
	ObservationAgeMaxSeconds     *float64 `json:"observationAgeMaxSeconds"`
	OverdueMaxSeconds            *float64 `json:"overdueMaxSeconds"`
}

// No retained population (including older reports), a partial population, or
// no eligible member cannot become a passing zero. Missing timestamps affect
// only their own measurement and remain counted in the report.
func eligibleFreshness(samples []sample) *freshnessCost {
	if len(samples) == 0 {
		return nil
	}
	out := &freshnessCost{}
	age, overdue, operationAge := 0.0, 0.0, 0.0
	for _, s := range samples {
		if s.ResourceFreshness == nil || len(s.ResourceFreshness) != s.Resources {
			return nil
		}
		for _, source := range s.Incomplete {
			if source == sourceResources {
				return nil
			}
		}
		seen := map[string]bool{}
		for _, r := range s.ResourceFreshness {
			key := r.Family + "/" + r.Namespace + "/" + r.Name
			if seen[key] || r.UID == "" || r.ResourceVersion == "" || r.Generation < 1 || r.ReadAt.IsZero() || !validFreshnessClaim(r) {
				return nil
			}
			seen[key] = true
			if r.Deleting {
				out.DeletingReadings++
				continue
			}
			if r.Suspended {
				out.SuspendedReadings++
				continue
			}
			out.EligibleReadings++
			if r.ActiveOperation != nil {
				out.InFlightReadings++
				operationAge = max(operationAge, r.ReadAt.Sub(r.ActiveOperation.StartedAt).Seconds())
			}
			if r.ObservedAt == nil || r.ObservedAt.After(r.ReadAt) {
				out.MissingObservations++
			} else {
				age = max(age, r.ReadAt.Sub(*r.ObservedAt).Seconds())
			}
			if r.NextReconciliationTime == nil {
				if r.ActiveOperation == nil {
					out.MissingDeadlines++
				}
			} else {
				out.ScheduledReadings++
				overdue = max(overdue, r.ReadAt.Sub(*r.NextReconciliationTime).Seconds())
			}
		}
	}
	if out.EligibleReadings > 0 {
		if out.MissingObservations == 0 {
			out.ObservationAgeMaxSeconds = &age
		}
		if out.ScheduledReadings > 0 && out.MissingDeadlines == 0 {
			out.OverdueMaxSeconds = &overdue
		}
	}
	if out.InFlightReadings > 0 {
		out.ActiveOperationAgeMaxSeconds = &operationAge
	}
	return out
}

// Collection is sequential: a resource LIST can finish after the scenario in
// which its sample began. Attribute freshness by its actual read timestamp.
// Keep failed or partial populations unavailable rather than filtering away
// the evidence of a missing LIST.
func eligibleFreshnessInWindow(w window, samples []sample) *freshnessCost {
	var selected []sample
	outside := 0
	for _, s := range samples {
		start, end := s.ResourceReadStartedAt, s.ResourceReadFinishedAt
		if start.IsZero() || end.IsZero() || end.Before(start) {
			// Older reports cannot locate failed or delayed reads within a
			// scenario, so they cannot establish this scoped bound.
			return nil
		}
		if end.Before(w.Start) || start.After(w.End) {
			continue
		}
		if eligibleFreshness([]sample{s}) == nil {
			return nil
		}
		rows := make([]resourceFreshness, 0, len(s.ResourceFreshness))
		for _, r := range s.ResourceFreshness {
			if r.ReadAt.Before(start) || r.ReadAt.After(end) {
				return nil
			}
			if !r.ReadAt.Before(w.Start) && !r.ReadAt.After(w.End) {
				rows = append(rows, r)
			}
		}
		outside += len(s.ResourceFreshness) - len(rows)
		s.ResourceFreshness, s.Resources = rows, len(rows)
		selected = append(selected, s)
	}
	out := eligibleFreshness(selected)
	if out != nil {
		out.OutsideWindowReadings = outside
	}
	return out
}
