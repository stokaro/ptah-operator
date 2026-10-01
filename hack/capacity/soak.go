package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type soakWorkload struct {
	Rounds         int      `json:"rounds"`
	Cadence        duration `json:"cadence"`
	ChurnPerFamily int      `json:"churnPerFamily"`
	MinimumCycles  int      `json:"minimumCycles"`
}

func (p soakWorkload) validate(w workload) error {
	if p.Rounds < 1 || p.Rounds > 9 || p.Cadence.Duration <= 0 || p.Cadence.Duration > 24*time.Hour ||
		p.ChurnPerFamily < 1 || p.ChurnPerFamily > 5 || p.MinimumCycles < 1 || p.MinimumCycles > 10000 ||
		w.SteadyState.Duration != time.Duration(p.Rounds)*p.Cadence.Duration ||
		w.Schemas != 10 || w.Migrations != 10 || w.ChangeBatch != 5 {
		return fmt.Errorf("soak requires 1-9 rounds, a positive cadence up to 24h, 1-5 replacements per family, positive cycle coverage and the populated 10+10 workload")
	}
	return nil
}

type batchTarget struct {
	family          string
	resource        schema.GroupVersionResource
	namespace, name string
	uid             types.UID
	generation      int64
	reference       string
	applied         bool
}

func targetConverged(object *unstructured.Unstructured, t batchTarget, after time.Time) (bool, error) {
	if object.GetUID() != t.uid || object.GetGeneration() != t.generation || object.GetNamespace() != t.namespace || object.GetName() != t.name {
		return false, fmt.Errorf("batch resource %s/%s changed identity or generation", t.namespace, t.name)
	}
	reading, err := readCycle(t.family, "GET", object, time.Now().UTC())
	if err != nil {
		return false, err
	}
	if !cycleReady(t.family, reading) || reading.CompletedAt.After(reading.ReceivedAt) || !reading.CompletedAt.After(after.Truncate(time.Second)) {
		return false, nil
	}
	field := "artifact"
	if t.family == "schema" {
		field = "source"
	}
	got, _, err := unstructured.NestedString(object.Object, "status", field, "digest")
	if err != nil || got != digestOf(t.reference) {
		return false, err
	}
	if t.applied {
		got, _, err = unstructured.NestedString(object.Object, "status", "applied", "artifactDigest")
		if err != nil || got != digestOf(t.reference) {
			return false, err
		}
	}
	return true, nil
}

// Remember each resource's accepted reading. Normal refreshes can overlap
// without leaving every resource idle at the same poll.
func (s *scenarios) waitBatch(ctx context.Context, targets []batchTarget, start time.Time) error {
	if len(targets) == 0 {
		return fmt.Errorf("batch has no resources")
	}
	ctx, cancel := context.WithDeadline(ctx, start.Add(s.load.Settle.Duration))
	defer cancel()
	pending := append([]batchTarget(nil), targets...)
	for len(pending) > 0 {
		for i := 0; i < len(pending); {
			target := pending[i]
			object, err := s.dynamic.Resource(target.resource).Namespace(target.namespace).Get(ctx, target.name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("read batch %s/%s: %w", target.namespace, target.name, err)
			}
			ready, err := targetConverged(object, target, start)
			if err != nil {
				return err
			}
			if ready {
				pending = append(pending[:i], pending[i+1:]...)
			} else {
				i++
			}
		}
		if len(pending) > 0 {
			if err := waitCapacityPoll(ctx); err != nil {
				return fmt.Errorf("batch still awaits %s/%s: %w", pending[0].namespace, pending[0].name, err)
			}
		}
	}
	return nil
}

func (s *scenarios) changeRound(ctx context.Context, round int) (err error) {
	if s.in.catalog == nil || round < 1 || round > 9 {
		return fmt.Errorf("change round requires a populated catalog and round 1-9")
	}
	var targets []batchTarget
	var start time.Time
	defer func() {
		if start.IsZero() {
			return
		}
		outcome := map[string]string{"round": fmt.Sprint(round), "moved": fmt.Sprint(len(targets))}
		if err == nil {
			outcome["converged"] = time.Since(start).String()
		} else {
			outcome["error"] = err.Error()
		}
		s.mark(fmt.Sprintf("soak update %02d", round), start, outcome)
	}()
	for _, family := range []string{"schema", "migration"} {
		for index := range s.load.ChangeBatch {
			resource, name, field, reference := schemaResource, s.schemaName(index), "desired", s.schemaReference(index, round)
			if family == "migration" {
				resource, name, field, reference = migrationResource, s.migrationName(index), "artifact", s.migrationReference(index, round)
			}
			client := s.dynamic.Resource(resource).Namespace(s.in.namespaceFor(index))
			old, err := client.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			previous, _, err := unstructured.NestedString(old.Object, "spec", field, "ociRef")
			if err != nil {
				return err
			}
			wantPrevious := s.schemaReference(index, round-1)
			if family == "migration" {
				wantPrevious = s.migrationReference(index, round-1)
			}
			if old.GetUID() == "" || old.GetResourceVersion() == "" || old.GetDeletionTimestamp() != nil || previous != wantPrevious {
				return fmt.Errorf("%s does not hold the previous round's original input", name)
			}
			// UID and resourceVersion preconditions prevent a read/write race from
			// changing a replacement or overwriting an intervening spec edit.
			raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": string(old.GetUID()), "resourceVersion": old.GetResourceVersion()}, "spec": map[string]any{field: map[string]any{"ociRef": reference}}})
			if err != nil {
				return err
			}
			if start.IsZero() {
				start = time.Now().UTC()
			}
			object, err := client.Patch(ctx, name, types.MergePatchType, raw, metav1.PatchOptions{})
			if err != nil {
				return err
			}
			if object.GetUID() != old.GetUID() || object.GetGeneration() <= old.GetGeneration() {
				return fmt.Errorf("%s update did not preserve UID and advance generation", name)
			}
			targets = append(targets, batchTarget{family: family, resource: resource, namespace: object.GetNamespace(), name: name, uid: object.GetUID(), generation: object.GetGeneration(), reference: reference, applied: family == "schema"})
		}
	}
	if err = s.waitBatch(ctx, targets, start); err != nil {
		return err
	}
	return s.verifyInputPlans(ctx, round)
}

func (s *scenarios) checkpointDatabase(ctx context.Context, round int, name string) error {
	if s.checkpoint == nil {
		return fmt.Errorf("database checkpoint verifier is missing")
	}
	ctx, cancel := context.WithTimeout(ctx, s.load.Settle.Duration)
	defer cancel()
	proof, err := s.checkpoint(ctx, round, name)
	s.databaseCheckpoints = append(s.databaseCheckpoints, proof)
	return err
}

func waitUntil(ctx context.Context, deadline time.Time) error {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *scenarios) soak(ctx context.Context) (err error) {
	config := s.load.Soak
	if config == nil || s.checkpoint == nil || s.in.catalog == nil {
		return fmt.Errorf("soak inputs are incomplete")
	}
	start := time.Now().UTC()
	end := start.Add(time.Duration(config.Rounds) * config.Cadence.Duration)
	completed := 0
	defer func() {
		w := window{Name: "soak", Start: start, End: time.Now().UTC(), Outcome: map[string]string{"completedRounds": fmt.Sprint(completed)}}
		if err == nil {
			w.End = end
		} else {
			w.Outcome["error"] = err.Error()
		}
		s.soakWindow = &w
		s.windows = append(s.windows, w)
	}()
	for round := 1; round <= config.Rounds; round++ {
		if err := waitUntil(ctx, start.Add(time.Duration(round-1)*config.Cadence.Duration)); err != nil {
			return err
		}
		// Each round belongs to its original time bucket. Slow work cannot shift
		// later deadlines and quietly turn a declared 90-minute run into a longer one.
		deadline := start.Add(time.Duration(round) * config.Cadence.Duration)
		if err := s.soakRound(ctx, round, deadline); err != nil {
			return fmt.Errorf("soak round %d: %w", round, err)
		}
		completed++
	}
	return waitUntil(ctx, end)
}

func (s *scenarios) soakRound(ctx context.Context, round int, deadline time.Time) error {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.checkpointDatabase(ctx, round-1, fmt.Sprintf("round-%02d-before-update", round)); err != nil {
		return err
	}
	if err := s.changeRound(ctx, round); err != nil {
		return err
	}
	if err := s.checkpointDatabase(ctx, round, fmt.Sprintf("round-%02d-after-update", round)); err != nil {
		return err
	}
	if err := s.churn(ctx, round); err != nil {
		return err
	}
	if err := s.checkpointDatabase(ctx, round, fmt.Sprintf("round-%02d-after-churn", round)); err != nil {
		return err
	}
	if err := s.maintenance(ctx, round); err != nil {
		return err
	}
	return s.checkpointDatabase(ctx, round, fmt.Sprintf("round-%02d-after-maintenance", round))
}

func (s *scenarios) validateSoakCycles(evidence cycleEvidence) error {
	w := s.soakWindow
	if w == nil || w.Outcome["completedRounds"] != fmt.Sprint(s.load.Soak.Rounds) || w.Outcome["error"] != "" {
		return fmt.Errorf("soak did not complete every declared round")
	}
	counts, problems := cyclesInWindow(*w, evidence)
	if err := s.validateChurnEvidence(evidence); err != nil {
		problems = append(problems, err.Error())
	}
	totals := map[string]int{}
	byUID := map[string]int{}
	for _, c := range counts {
		totals[c.Family+"/"+c.Namespace+"/"+c.Name] += c.Completed
		byUID[c.UID] = c.Completed
	}
	for _, family := range []string{"schema", "migration"} {
		for i := range 10 {
			name := s.schemaName(i)
			if family == "migration" {
				name = s.migrationName(i)
			}
			key := family + "/" + s.in.namespaceFor(i) + "/" + name
			if totals[key] < s.load.Soak.MinimumCycles {
				problems = append(problems, fmt.Sprintf("%s completed %d read-only cycles, need %d", key, totals[key], s.load.Soak.MinimumCycles))
			}
			delete(totals, key)
		}
	}
	if len(totals) > 0 {
		problems = append(problems, "soak counted an undeclared workload slot")
	}
	for _, life := range evidence.Lifetimes {
		begin, end := life.CreatedAt, w.End
		if begin.Before(w.Start) {
			begin = w.Start
		}
		if life.DeletedSeenAt != nil && life.DeletedSeenAt.Before(end) {
			end = *life.DeletedSeenAt
		}
		if end.Sub(begin) >= 2*s.load.Interval.Duration && byUID[life.UID] == 0 {
			problems = append(problems, fmt.Sprintf("incarnation %s completed no read-only cycle", life.UID))
		}
	}
	var failures []error
	for _, p := range problems {
		failures = append(failures, errors.New(p))
	}
	return errors.Join(failures...)
}
