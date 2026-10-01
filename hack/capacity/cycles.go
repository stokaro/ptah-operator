package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type cycleOperation struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	JobName   string    `json:"jobName"`
	JobUID    string    `json:"jobUID"`
	StartedAt time.Time `json:"startedAt"`
}

type cycleReading struct {
	Event              string             `json:"event"`
	ReceivedAt         time.Time          `json:"receivedAt"`
	ResourceVersion    string             `json:"resourceVersion"`
	Namespace          string             `json:"namespace"`
	Name               string             `json:"name"`
	UID                string             `json:"uid"`
	CreatedAt          time.Time          `json:"createdAt"`
	Generation         int64              `json:"generation"`
	ObservedGeneration int64              `json:"observedGeneration"`
	Operation          *cycleOperation    `json:"operation,omitempty"`
	Conditions         []metav1.Condition `json:"conditions"`
	CompletedAt        time.Time          `json:"completedAt"`
	PendingRelease     bool               `json:"pendingRelease"`
	Unsafe             bool               `json:"unsafe"`
}

type cycleHistory struct {
	Family    string            `json:"family"`
	Namespace string            `json:"namespace"`
	Selector  string            `json:"selector"`
	StartedAt time.Time         `json:"startedAt"`
	EndedAt   time.Time         `json:"endedAt"`
	Cursor    string            `json:"cursor"`
	Error     string            `json:"error,omitempty"`
	Readings  []cycleReading    `json:"readings"`
	Retries   []cycleWatchRetry `json:"watchRetries,omitempty"`
}

// A resumed cursor preserves events, but the transport interruption remains
// evidence. It must not erase failed metric samples from the same interval.
type cycleWatchRetry struct {
	At     time.Time `json:"at"`
	Cursor string    `json:"cursor"`
	Error  string    `json:"error"`
}

type completedCycle struct {
	Family                    string           `json:"family"`
	Namespace                 string           `json:"namespace"`
	Name                      string           `json:"name"`
	UID                       string           `json:"uid"`
	Generation                int64            `json:"generation"`
	StartedAt                 time.Time        `json:"startedAt"`
	CompletedAt               time.Time        `json:"completedAt"`
	ConfirmedAt               time.Time        `json:"confirmedAt"`
	CompletionResourceVersion string           `json:"completionResourceVersion"`
	Operations                []cycleOperation `json:"operations"`
}

type cycleLifetime struct {
	Family        string     `json:"family"`
	Namespace     string     `json:"namespace"`
	Name          string     `json:"name"`
	UID           string     `json:"uid"`
	CreatedAt     time.Time  `json:"createdAt"`
	FirstSeenAt   time.Time  `json:"firstSeenAt"`
	DeletedSeenAt *time.Time `json:"deletedSeenAt,omitempty"`
}

type cycleEvidence struct {
	Histories []cycleHistory   `json:"histories"`
	Lifetimes []cycleLifetime  `json:"lifetimes"`
	Completed []completedCycle `json:"completed"`
}

func readCycle(family, event string, object *unstructured.Unstructured, received time.Time) (cycleReading, error) {
	kind := map[string]string{"schema": "PtahSchema", "migration": "PtahMigration"}[family]
	if kind == "" || object.GetKind() != kind || object.GetAPIVersion() != "operator.ptah.run/v1alpha1" {
		return cycleReading{}, fmt.Errorf("cycle watch has an unexpected resource kind")
	}
	r := cycleReading{Event: event, ReceivedAt: received, ResourceVersion: object.GetResourceVersion(), Namespace: object.GetNamespace(), Name: object.GetName(), UID: string(object.GetUID()), CreatedAt: object.GetCreationTimestamp().Time, Generation: object.GetGeneration()}
	if r.ResourceVersion == "" || r.UID == "" || r.Namespace == "" || r.Name == "" || r.CreatedAt.IsZero() || r.Generation < 1 {
		return r, fmt.Errorf("cycle reading lacks resource identity")
	}
	var status struct {
		ObservedGeneration           int64              `json:"observedGeneration"`
		ActiveOperation              *cycleOperation    `json:"activeOperation"`
		Conditions                   []metav1.Condition `json:"conditions"`
		LastSuccessfulReconciliation time.Time          `json:"lastSuccessfulReconciliation"`
		History                      *struct {
			ObservedAt time.Time `json:"observedAt"`
		} `json:"history"`
		PendingObservation json.RawMessage `json:"pendingObservation"`
		PendingLockRelease json.RawMessage `json:"pendingLockRelease"`
		UnresolvedRun      json.RawMessage `json:"unresolvedRun"`
	}
	raw, err := json.Marshal(object.Object["status"])
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return r, err
	}
	r.ObservedGeneration = status.ObservedGeneration
	r.Operation = status.ActiveOperation
	r.Conditions = status.Conditions
	for i := range r.Conditions {
		r.Conditions[i].Message = ""
	}
	if family == "schema" {
		r.CompletedAt = status.LastSuccessfulReconciliation
	} else if family == "migration" {
		if status.History != nil {
			r.CompletedAt = status.History.ObservedAt
		}
	} else {
		return r, fmt.Errorf("unknown cycle family %s", family)
	}
	for _, raw := range []json.RawMessage{status.PendingObservation, status.UnresolvedRun} {
		if len(raw) > 0 && string(raw) != "null" {
			r.Unsafe = true
		}
	}
	r.PendingRelease = len(status.PendingLockRelease) > 0 && string(status.PendingLockRelease) != "null"
	if r.Operation != nil && (r.Operation.ID == "" || r.Operation.Type == "" || r.Operation.StartedAt.IsZero()) {
		return r, fmt.Errorf("cycle operation lacks claim identity")
	}
	return r, nil
}

func cycleReady(family string, r cycleReading) bool {
	if r.Unsafe || r.PendingRelease || r.Operation != nil || r.ObservedGeneration != r.Generation || r.CompletedAt.IsZero() {
		return false
	}
	ready, converged := false, family == "migration"
	for _, condition := range r.Conditions {
		if condition.ObservedGeneration != r.Generation {
			continue
		}
		if condition.Type == "Ready" && condition.Status == metav1.ConditionTrue {
			ready = family == "schema" && condition.Reason == "InSync" || family == "migration" && condition.Reason == "HistoryMatched"
		}
		if family == "schema" && condition.Type == "InSync" && condition.Status == metav1.ConditionTrue && condition.Reason == "ScopedConverged" {
			converged = true
		}
	}
	return ready && converged
}

// Completed cycles require every read-only claim in order, with exact Job
// identities, followed by a fresh accepted convergence reading for that UID.
// A status seen at startup, a repeated condition or post-Apply proof is not a
// new refresh cycle. The retained stream can be replayed to recompute counts.
func completedCycles(h cycleHistory) ([]completedCycle, []cycleLifetime) {
	expected := []string{"Resolve", "Verify", "Observe", "Plan"}
	if h.Family == "migration" {
		expected = []string{"Resolve", "Verify", "History"}
	}
	type progress struct {
		generation int64
		operations []cycleOperation
	}
	active := map[string]*progress{}
	rejectedClaims := map[string]bool{}
	completedJobs := map[string]bool{}
	lifetimes := map[string]*cycleLifetime{}
	var completed []completedCycle
	for _, r := range h.Readings {
		if lifetimes[r.UID] == nil {
			lifetimes[r.UID] = &cycleLifetime{Family: h.Family, Namespace: r.Namespace, Name: r.Name, UID: r.UID, CreatedAt: r.CreatedAt, FirstSeenAt: r.ReceivedAt}
		}
		if r.Event == "DELETED" {
			at := r.ReceivedAt
			lifetimes[r.UID].DeletedSeenAt = &at
			delete(active, r.UID)
			continue
		}
		if r.Event == "INITIAL" || r.Unsafe || r.ObservedGeneration != r.Generation {
			delete(active, r.UID)
			continue
		}
		p := active[r.UID]
		if p != nil && p.generation != r.Generation {
			delete(active, r.UID)
			p = nil
		}
		if op := r.Operation; op != nil {
			claimKey := r.UID + "/" + op.ID
			if rejectedClaims[claimKey] {
				delete(active, r.UID)
				continue
			}
			consistent := true
			if p != nil {
				for _, before := range p.operations {
					if before.ID == op.ID && (before.Type != op.Type || !before.StartedAt.Equal(op.StartedAt) || before.JobName != op.JobName || before.JobUID != "" && before.JobUID != op.JobUID) {
						consistent = false
					}
				}
			}
			if !consistent {
				rejectedClaims[claimKey] = true
				delete(active, r.UID)
				continue
			}
			if op.Type == expected[0] {
				if p == nil || len(p.operations) != 1 || p.operations[0].ID != op.ID {
					if op.StartedAt.Before(h.StartedAt) || op.StartedAt.Before(r.CreatedAt) {
						delete(active, r.UID)
						continue
					}
					p = &progress{generation: r.Generation, operations: []cycleOperation{*op}}
					active[r.UID] = p
				} else {
					p.operations[0] = *op
				}
				continue
			}
			if p == nil {
				continue
			}
			last := len(p.operations) - 1
			if op.Type == p.operations[last].Type {
				// A retried stage must still carry a concrete latest claim/Job.
				if op.StartedAt.Before(p.operations[last].StartedAt) {
					delete(active, r.UID)
					continue
				}
				p.operations[last] = *op
				continue
			}
			if last+1 >= len(expected) || op.Type != expected[last+1] || p.operations[last].JobUID == "" || op.StartedAt.Before(p.operations[last].StartedAt) {
				delete(active, r.UID)
				continue
			}
			p.operations = append(p.operations, *op)
			continue
		}
		if p == nil || len(p.operations) != len(expected) || !cycleReady(h.Family, r) {
			continue
		}
		ids, jobs := map[string]bool{}, map[string]bool{}
		valid := !r.CompletedAt.Before(p.operations[len(p.operations)-1].StartedAt)
		for _, op := range p.operations {
			valid = valid && op.JobUID != "" && op.JobName != "" && !ids[op.ID] && !jobs[op.JobUID] && !completedJobs[r.UID+"/"+op.JobUID]
			ids[op.ID] = true
			jobs[op.JobUID] = true
		}
		if valid {
			for _, op := range p.operations {
				completedJobs[r.UID+"/"+op.JobUID] = true
			}
			completed = append(completed, completedCycle{Family: h.Family, Namespace: r.Namespace, Name: r.Name, UID: r.UID, Generation: r.Generation, StartedAt: p.operations[0].StartedAt, CompletedAt: r.CompletedAt, ConfirmedAt: r.ReceivedAt, CompletionResourceVersion: r.ResourceVersion, Operations: append([]cycleOperation(nil), p.operations...)})
		}
		delete(active, r.UID)
	}
	lives := make([]cycleLifetime, 0, len(lifetimes))
	for _, life := range lifetimes {
		lives = append(lives, *life)
	}
	sort.Slice(lives, func(i, j int) bool { return lives[i].UID < lives[j].UID })
	return completed, lives
}

type resourceCycleCount struct {
	Family    string `json:"family"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Completed int    `json:"completed"`
}

func cyclesInWindow(w window, evidence cycleEvidence) ([]resourceCycleCount, []string) {
	var problems []string
	if len(evidence.Histories) == 0 {
		problems = append(problems, "no cycle histories")
	}
	for _, history := range evidence.Histories {
		if history.Error != "" || history.StartedAt.After(w.Start) || history.EndedAt.Before(w.End) {
			problems = append(problems, fmt.Sprintf("%s/%s cycle history does not cover the scenario: %s", history.Namespace, history.Family, history.Error))
		}
	}
	counts := map[string]*resourceCycleCount{}
	for _, life := range evidence.Lifetimes {
		if life.CreatedAt.After(w.End) || life.DeletedSeenAt != nil && life.DeletedSeenAt.Before(w.Start) {
			continue
		}
		key := life.Family + "/" + life.Namespace + "/" + life.Name + "/" + life.UID
		counts[key] = &resourceCycleCount{Family: life.Family, Namespace: life.Namespace, Name: life.Name, UID: life.UID}
	}
	for _, cycle := range evidence.Completed {
		if cycle.StartedAt.Before(w.Start) || cycle.CompletedAt.After(w.End) || cycle.ConfirmedAt.IsZero() || cycle.ConfirmedAt.After(w.End) {
			continue
		}
		key := cycle.Family + "/" + cycle.Namespace + "/" + cycle.Name + "/" + cycle.UID
		if count := counts[key]; count != nil {
			count.Completed++
		} else {
			problems = append(problems, "completed cycle has no recorded resource lifetime")
		}
	}
	if len(counts) == 0 {
		problems = append(problems, "no workload resource lifetimes in scenario")
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]resourceCycleCount, 0, len(keys))
	for _, key := range keys {
		out = append(out, *counts[key])
	}
	return out, problems
}
