package main

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Keep the identity and inputs of each freshness measurement. Suspension is
// an explicit exclusion; an approval gate is not, since it still refreshes.
type resourceFreshness struct {
	Family                 string     `json:"family"`
	Namespace              string     `json:"namespace"`
	Name                   string     `json:"name"`
	UID                    string     `json:"uid"`
	ResourceVersion        string     `json:"resourceVersion"`
	Generation             int64      `json:"generation"`
	ReadAt                 time.Time  `json:"readAt"`
	Suspended              bool       `json:"suspended"`
	Deleting               bool       `json:"deleting"`
	ObservedAt             *time.Time `json:"observedAt"`
	NextReconciliationTime *time.Time `json:"nextReconciliationTime"`
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
	return r, nil
}

type freshnessCost struct {
	EligibleReadings         int      `json:"eligibleReadings"`
	SuspendedReadings        int      `json:"suspendedReadings"`
	DeletingReadings         int      `json:"deletingReadings"`
	MissingObservations      int      `json:"missingObservations"`
	MissingDeadlines         int      `json:"missingDeadlines"`
	ObservationAgeMaxSeconds *float64 `json:"observationAgeMaxSeconds"`
	OverdueMaxSeconds        *float64 `json:"overdueMaxSeconds"`
}

// No retained population (including older reports), a partial population, or
// no eligible member cannot become a passing zero. Missing timestamps affect
// only their own measurement and remain counted in the report.
func eligibleFreshness(samples []sample) *freshnessCost {
	if len(samples) == 0 {
		return nil
	}
	out := &freshnessCost{}
	age, overdue := 0.0, 0.0
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
			if seen[key] || r.UID == "" || r.ResourceVersion == "" || r.Generation < 1 || r.ReadAt.IsZero() {
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
			if r.ObservedAt == nil || r.ObservedAt.After(r.ReadAt) {
				out.MissingObservations++
			} else {
				age = max(age, r.ReadAt.Sub(*r.ObservedAt).Seconds())
			}
			if r.NextReconciliationTime == nil {
				out.MissingDeadlines++
			} else {
				overdue = max(overdue, r.ReadAt.Sub(*r.NextReconciliationTime).Seconds())
			}
		}
	}
	if out.EligibleReadings > 0 {
		if out.MissingObservations == 0 {
			out.ObservationAgeMaxSeconds = &age
		}
		if out.MissingDeadlines == 0 {
			out.OverdueMaxSeconds = &overdue
		}
	}
	return out
}
