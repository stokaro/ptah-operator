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
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
)

type soakWorkload struct {
	RetentionFault bool     `json:"retentionFault,omitempty"`
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
			wantPrevious := s.schemaReference(index, round-1)
			if family == "migration" {
				wantPrevious = s.migrationReference(index, round-1)
			}
			old, object, err := moveRoundInput(ctx, client, name, field, wantPrevious, reference, &start)
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

// moveRoundInput moves one resource's input from the previous round's
// reference to this round's, and returns the resource before and after. The
// operator's status writes move resourceVersion too, so a conflict is usually
// one of them. Each attempt reads the resource again and checks it still holds
// the previous round's input, so a retry never overwrites another spec edit or
// a replacement. start is set at the first write attempt.
func moveRoundInput(ctx context.Context, client dynamic.ResourceInterface, name, field, wantPrevious, reference string, start *time.Time) (old, object *unstructured.Unstructured, err error) {
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var err error
		old, err = client.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		previous, _, err := unstructured.NestedString(old.Object, "spec", field, "ociRef")
		if err != nil {
			return err
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
			*start = time.Now().UTC()
		}
		object, err = client.Patch(ctx, name, types.MergePatchType, raw, metav1.PatchOptions{})
		return err
	})
	return old, object, err
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
	problems = append(problems, s.cycleTargetProblems(*w, counts, evidence)...)
	byUID := map[string]int{}
	for _, c := range counts {
		byUID[c.UID] = c.Completed
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

// cycleTargetProblems holds each declared slot to the soak's cycle target, as
// profile revision 48 states it. A slot the rounds never change must complete
// MinimumCycles read-only cycles. A slot the rounds move to a new artifact
// spends two to three intervals of every round on its plan, approval, Apply
// and the Observe that proves it, so nine rounds leave it short of that count
// by construction. It must instead come back to read-only refresh after every
// change: one read-only cycle that starts after the round converged and ends
// before the next round, or the soak, does.
func (s *scenarios) cycleTargetProblems(w window, counts []resourceCycleCount, evidence cycleEvidence) []string {
	totals := map[string]int{}
	for _, c := range counts {
		totals[c.Family+"/"+c.Namespace+"/"+c.Name] += c.Completed
	}
	updates, problems := soakUpdates(s.windows, s.load.Soak.Rounds)
	for _, family := range []string{"schema", "migration"} {
		for i := range 10 {
			name := s.schemaName(i)
			if family == "migration" {
				name = s.migrationName(i)
			}
			key := family + "/" + s.in.namespaceFor(i) + "/" + name
			switch {
			case i < s.load.ChangeBatch:
				if len(updates) == s.load.Soak.Rounds {
					problems = append(problems, movedSlotProblems(key, w, updates, evidence.Completed)...)
				}
			case totals[key] < s.load.Soak.MinimumCycles:
				problems = append(problems, fmt.Sprintf("%s completed %d read-only cycles, need %d", key, totals[key], s.load.Soak.MinimumCycles))
			}
			delete(totals, key)
		}
	}
	if len(totals) > 0 {
		problems = append(problems, "soak counted an undeclared workload slot")
	}
	return problems
}

// soakUpdates returns the soak's update rounds in order. Each must have run
// once and converged before the next began; a moved slot is measured in the
// gaps between them.
func soakUpdates(windows []window, rounds int) ([]window, []string) {
	var out []window
	var problems []string
	for round := 1; round <= rounds; round++ {
		name := fmt.Sprintf("soak update %02d", round)
		var found []window
		for _, w := range windows {
			if w.Name == name {
				found = append(found, w)
			}
		}
		if len(found) != 1 || found[0].Outcome["converged"] == "" || found[0].Outcome["error"] != "" || found[0].End.Before(found[0].Start) {
			problems = append(problems, fmt.Sprintf("%s did not run once and converge", name))
			continue
		}
		if len(out) > 0 && found[0].Start.Before(out[len(out)-1].End) {
			problems = append(problems, fmt.Sprintf("%s started before the previous round converged", name))
		}
		out = append(out, found[0])
	}
	return out, problems
}

// movedSlotProblems reports each gap after an update round in which the slot
// completed no read-only cycle.
func movedSlotProblems(key string, soak window, updates []window, completed []completedCycle) []string {
	var problems []string
	for k, round := range updates {
		from, to, until := round.End, soak.End, "the end of the soak"
		if k+1 < len(updates) {
			to, until = updates[k+1].Start, fmt.Sprintf("update round %d", k+2)
		}
		found := false
		for _, c := range completed {
			if c.Family+"/"+c.Namespace+"/"+c.Name == key && c.within(soak) && !c.StartedAt.Before(from) && !c.CompletedAt.After(to) {
				found = true
				break
			}
		}
		if !found {
			problems = append(problems, fmt.Sprintf("%s completed no read-only cycle between update round %d converging and %s", key, k+1, until))
		}
	}
	return problems
}
