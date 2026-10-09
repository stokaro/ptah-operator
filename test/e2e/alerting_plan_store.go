package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/plancontract"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	alPlanStoreAlert  = "PtahOperatorPlanStoreLarge"
	alPlanStoreMetric = "ptah_operator_stored_plan_bytes"
	alPlanStoreLimit  = int64(128 << 20)
	alPlanStoreCount  = 17
)

type alPlanKey struct{ family, namespace, name string }
type alPlanPinSet map[alPlanKey]types.UID

// The runbook's complete pin inventory, including retired approvals and
// unresolved work. Kind and namespace are part of an immutable reference.
func alPlanPins(schemas []ptahv1.PtahSchema, migrations []ptahv1.PtahMigration, approvals []ptahv1.PtahSchemaApproval, migrationApprovals []ptahv1.PtahMigrationApproval) (alPlanPinSet, error) {
	pins := alPlanPinSet{}
	add := func(family, namespace, name string, uid types.UID) error {
		k := alPlanKey{family, namespace, name}
		if namespace == "" || name == "" || uid == "" || pins[k] != "" && pins[k] != uid {
			return errors.New("a plan pin has an incomplete or conflicting identity")
		}
		pins[k] = uid
		return nil
	}
	for _, s := range schemas {
		var refs []ptahv1.ImmutableObjectReference
		if p := s.Status.Plan; p != nil {
			refs = append(refs, ptahv1.ImmutableObjectReference{Name: p.Name, UID: p.UID})
		}
		if p := s.Status.PendingObservation; p != nil {
			refs = append(refs, ptahv1.ImmutableObjectReference{Name: p.Plan.Name, UID: p.Plan.UID})
		}
		if p := s.Status.Applied; p != nil {
			refs = append(refs, p.PlanRef)
		}
		if p := s.Status.PendingBindingRetirement; p != nil && p.Plan != nil {
			refs = append(refs, ptahv1.ImmutableObjectReference{Name: p.Plan.Name, UID: p.Plan.UID})
		}
		for _, r := range refs {
			if err := add("schema", s.Namespace, r.Name, r.UID); err != nil {
				return nil, err
			}
		}
	}
	for _, m := range migrations {
		var refs []ptahv1.ImmutableObjectReference
		if m.Status.Plan != nil {
			refs = append(refs, *m.Status.Plan)
		}
		if p := m.Status.ActiveOperation; p != nil && p.PlanRef != nil {
			refs = append(refs, *p.PlanRef)
		}
		if p := m.Status.UnresolvedRun; p != nil {
			refs = append(refs, p.PlanRef)
		}
		for _, r := range refs {
			if err := add("migration", m.Namespace, r.Name, r.UID); err != nil {
				return nil, err
			}
		}
	}
	for _, a := range approvals {
		if err := add("schema", a.Namespace, a.Spec.PlanRef.Name, a.Spec.PlanRef.UID); err != nil {
			return nil, err
		}
	}
	for _, a := range migrationApprovals {
		if err := add("migration", a.Namespace, a.Spec.PlanRef.Name, a.Spec.PlanRef.UID); err != nil {
			return nil, err
		}
	}
	if len(pins) == 0 {
		return nil, errors.New("the plan pin inventory is empty")
	}
	return pins, nil
}

func alPlanStoreQuiescent(s *ptahv1.PtahSchema) bool {
	if s == nil {
		return false
	}
	currentSuspension := false
	for _, c := range s.Status.Conditions {
		if c.Type == ptahv1.ConditionSuspended && c.Status == metav1.ConditionTrue && c.Reason == "Requested" && c.ObservedGeneration == s.Generation {
			currentSuspension = true
		}
	}
	return s != nil && s.UID != "" && s.Generation > 0 && s.DeletionTimestamp == nil && s.Spec.Suspend && s.Spec.Policy.Apply == ptahv1.ApplyPolicyNever &&
		s.Status.ObservedGeneration == s.Generation && s.Status.ActiveOperation == nil && s.Status.PendingObservation == nil && s.Status.PendingLockRelease == nil && s.Status.PendingBindingRetirement == nil &&
		s.Status.Plan != nil && s.Status.Plan.UID != "" && s.Status.Plan.Name != "" && s.Status.Plan.Approval == nil &&
		currentSuspension
}

func alPlanMayPrune(plan *ptahv1.PtahSchemaPlan, owner *ptahv1.PtahSchema, pins alPlanPinSet) error {
	if plan == nil || plan.UID == "" || plan.ResourceVersion == "" || plan.DeletionTimestamp != nil || !alPlanStoreQuiescent(owner) ||
		plan.Namespace != owner.Namespace || plan.Spec.SchemaRef.Name != owner.Name || plan.Spec.SchemaRef.UID != owner.UID ||
		!ownedExactlyOnce(plan.OwnerReferences, ptahv1.GroupVersion.String(), "PtahSchema", owner.Name, owner.UID) ||
		len(pins) == 0 || pins[alPlanKey{"schema", plan.Namespace, plan.Name}] != "" || owner.Status.Plan.UID == plan.UID || owner.Status.Plan.Name == plan.Name {
		return errors.New("pruning requires an original unpinned plan and its quiescent Never owner")
	}
	return nil
}

type alPlanExport struct {
	Plan   *ptahv1.PtahSchemaPlan       `json:"plan"`
	Chunks []ptahv1.PtahSchemaPlanChunk `json:"chunks"`
}

// An independent reconstruction checks every chunk's identity, ownership,
// index, size and hash as well as the manifest's total. No empty export passes.
func (e alPlanExport) document() ([]byte, error) {
	p := e.Plan
	if p == nil || p.UID == "" || p.Name == "" || p.Namespace == "" || p.Spec.Size < 1 || p.Spec.Size > plancontract.MaxExecutableBytes || len(p.Spec.Chunks) == 0 || len(p.Spec.Chunks) > plancontract.MaxChunks || len(e.Chunks) != len(p.Spec.Chunks) {
		return nil, errors.New("no complete retained plan payload")
	}
	ready := meta.FindStatusCondition(p.Status.Conditions, ptahv1.ConditionPlanStorageReady)
	if p.Generation < 1 || p.Status.ObservedGeneration != p.Generation || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != p.Generation || len(p.Status.PublishedChunks) != len(p.Spec.Chunks) {
		return nil, errors.New("retained plan storage has no current publication commit")
	}
	// The status list is keyed by index. Its order is immaterial, but every
	// manifest entry must bind to the concrete chunk the manager committed.
	published := make(map[int32]ptahv1.PublishedPlanChunkStatus, len(p.Status.PublishedChunks))
	for _, c := range p.Status.PublishedChunks {
		if c.Index < 0 || int(c.Index) >= len(p.Spec.Chunks) || c.Name == "" || c.UID == "" {
			return nil, errors.New("retained plan has an invalid chunk publication record")
		}
		if _, exists := published[c.Index]; exists {
			return nil, errors.New("retained plan has duplicate chunk publication records")
		}
		published[c.Index] = c
	}
	var b bytes.Buffer
	seen := map[string]bool{}
	for i, r := range p.Spec.Chunks {
		c := e.Chunks[i]
		committed := published[int32(i)]
		if r.Index != int32(i) || r.Name == "" || seen[r.Name] || r.Size <= 0 || int64(r.Size) > plancontract.ChunkBytes || c.Name != r.Name || c.Namespace != p.Namespace || c.UID == "" || committed.Name != c.Name || committed.UID != c.UID || c.DeletionTimestamp != nil ||
			!ownedExactlyOnce(c.OwnerReferences, ptahv1.GroupVersion.String(), "PtahSchemaPlan", p.Name, p.UID) || int32(len(c.Spec.Data)) != r.Size || fmt.Sprintf("sha256:%x", sha256.Sum256(c.Spec.Data)) != r.Digest {
			return nil, errors.New("a retained chunk lost its original identity or exact bytes")
		}
		seen[r.Name] = true
		b.Write(c.Spec.Data)
	}
	if int64(b.Len()) != p.Spec.Size || fmt.Sprintf("sha256:%x", sha256.Sum256(b.Bytes())) != p.Spec.ContentDigest {
		return nil, errors.New("reconstructed payload differs from its retained plan")
	}
	return b.Bytes(), nil
}

// Generated fixture payloads compress well. Keeping a reversible archive in
// the retained phase log preserves the actual export after test cleanup.
func (e alPlanExport) archive() ([]byte, error) {
	if _, err := e.document(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	if err := json.NewEncoder(z).Encode(e); err != nil {
		return nil, err
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	restored, err := alReadPlanExport(b.Bytes())
	if err != nil {
		return nil, err
	}
	if restored.Plan.UID != e.Plan.UID || restored.Plan.Spec.ContentDigest != e.Plan.Spec.ContentDigest {
		return nil, errors.New("plan export round trip changed its identity")
	}
	return b.Bytes(), nil
}

func alReadPlanExport(body []byte) (alPlanExport, error) {
	var e alPlanExport
	z, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return e, err
	}
	defer z.Close()
	// Base64 plus object metadata is bounded separately from executable bytes.
	raw, err := io.ReadAll(io.LimitReader(z, 16<<20))
	if err != nil {
		return e, err
	}
	if len(raw) == 16<<20 {
		return e, errors.New("plan export exceeds its evidence bound")
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, err
	}
	_, err = e.document()
	return e, err
}

type alPlanStoreHistory struct {
	crossedLower, crossedUpper, clearedLower, clearedUpper, through time.Time
	latest                                                          int64
}

// A scrape is timestamped when it starts but becomes queryable only after it
// commits. A query during that scrape can have an unfinished tail. Requery the
// same instant within the configured scrape timeout; never move the measurement
// window or relax the checks for missing, unhealthy, or mismatched samples.
func alQueryPlanStoreHistory(ctx context.Context, query func(context.Context, time.Time) ([]byte, error), pods []string, leader string, started, at time.Time, recovery bool) (alPlanStoreHistory, []byte, error) {
	var history alPlanStoreHistory
	var body []byte
	read := func(ctx context.Context) error {
		snapshot, err := query(ctx, at)
		if err != nil {
			return err
		}
		// A timed-out retry must retain the last response for diagnosis.
		body = snapshot
		groups, err := alSplitHistorySnapshot(body, alScrapeJob, []string{alPlanStoreMetric, "up", "scrape_duration_seconds"})
		if err != nil {
			return err
		}
		history, err = alReadPlanStoreWindow(groups[alPlanStoreMetric], groups["up"], groups["scrape_duration_seconds"], pods, leader, started, at, recovery)
		return err
	}
	if err := read(ctx); !errors.Is(err, errAlHistoryNotFresh) {
		return history, body, err
	}
	ctx, cancel := context.WithTimeout(ctx, alScrapeTimeout)
	defer cancel()
	err := harness.Wait(ctx, "a committed native plan-store scrape", alScrapeTimeout, 100*time.Millisecond, func(ctx context.Context) (bool, string, error) {
		err := read(ctx)
		if errors.Is(err, errAlHistoryNotFresh) {
			return false, err.Error(), nil
		}
		return err == nil, "", err
	})
	return history, body, err
}

func alReadPlanStoreHistory(gaugeBody, upBody, durationBody []byte, pods []string, leader string, started, queriedAt time.Time) (alPlanStoreHistory, error) {
	return alReadPlanStoreWindow(gaugeBody, upBody, durationBody, pods, leader, started, queriedAt, false)
}

// Recovery starts from a fresh above-threshold baseline immediately before
// pruning. The earlier firing window already established the crossing. Export
// and replacement work between those events cannot extend either deadline.
func alReadPlanStoreRecoveryHistory(gaugeBody, upBody, durationBody []byte, pods []string, leader string, started, queriedAt time.Time) (alPlanStoreHistory, error) {
	return alReadPlanStoreWindow(gaugeBody, upBody, durationBody, pods, leader, started, queriedAt, true)
}

func alReadPlanStoreWindow(gaugeBody, upBody, durationBody []byte, pods []string, leader string, started, queriedAt time.Time, recovery bool) (alPlanStoreHistory, error) {
	h := alPlanStoreHistory{}
	health, err := alReadScrapeHistory(upBody, durationBody, pods, leader, started, queriedAt)
	if err != nil {
		return h, err
	}
	if !health.firstFailure.IsZero() {
		return h, errors.New("the plan-store proof lost a healthy manager scrape")
	}
	h.through = health.scrapedThrough
	series, err := alAdmissionNativeMatrix(gaugeBody)
	if err != nil {
		return h, err
	}
	if len(series) != 1 || series[0].Metric["__name__"] != alPlanStoreMetric || series[0].Metric["job"] != alScrapeJob || series[0].Metric["pod"] != leader || series[0].Metric["instance"] != health.leaderInstance {
		return h, errors.New("plan bytes must come from the original leader alone")
	}
	since := started.Add(-2 * alScrapeInterval)
	// Health already proves a fresh committed scrape. Its gauge must match
	// those exact timestamps below; a missing gauge is not a pending scrape.
	values, err := alNativeSamples(series[0], since, queriedAt, true, false)
	if err != nil {
		return h, err
	}
	if len(values) < 2 || values[0].at.After(since) {
		return h, errors.New("plan store lacks a pre-fault byte reading")
	}
	for len(values) > 1 && !values[1].at.After(since) {
		values = values[1:]
	}
	if recovery && values[0].value <= float64(alPlanStoreLimit) {
		return h, errors.New("plan-store recovery lacks an above-limit baseline")
	}
	durations, err := alAdmissionNativeMatrix(durationBody)
	if err != nil {
		return h, err
	}
	var times []alAdmissionSample
	for _, s := range durations {
		if s.Metric["pod"] == leader {
			times, err = alAdmissionNativeSamples(s, since, queriedAt, false)
			if err != nil {
				return h, err
			}
		}
	}
	for len(times) > 1 && !times[1].at.After(since) {
		times = times[1:]
	}
	if len(values) != len(times) {
		return h, errors.New("plan bytes omitted a native scrape")
	}
	for i, v := range values {
		if !v.at.Equal(times[i].at) {
			return h, errors.New("plan bytes lost their scrape identity")
		}
		above := v.value > float64(alPlanStoreLimit)
		if !v.at.After(started) && above != recovery {
			return h, errors.New("plan store disagrees with its pre-fault baseline")
		}
		if above {
			if !h.clearedLower.IsZero() {
				return h, errors.New("plan bytes crossed the limit again after pruning")
			}
			if !recovery && h.crossedLower.IsZero() {
				if i == 0 {
					return h, errors.New("plan store has no below-limit baseline")
				}
				h.crossedLower, h.crossedUpper = values[i-1].at, v.at.Add(time.Duration(times[i].value*float64(time.Second)))
			}
		} else if (recovery || !h.crossedLower.IsZero()) && h.clearedLower.IsZero() {
			h.clearedLower, h.clearedUpper = values[i-1].at, v.at.Add(time.Duration(times[i].value*float64(time.Second)))
		}
		h.latest = int64(v.value)
	}
	return h, nil
}

// Retain the original firing and every later receiver notification while
// exports and replacements run. Missing gauges outside the timing windows
// must not hide an early resolution or a different incident.
func alPlanStoreIncidentHeld(deliveries []alDelivery, index int, firing alDelivery) bool {
	if firing.AlertName != alPlanStoreAlert || firing.Status != "firing" || firing.StartsAt.IsZero() || firing.Receiver == "" ||
		firing.Labels["operator_namespace"] == "" || firing.Labels["operator_metrics_service"] == "" ||
		index < 0 || index >= len(deliveries) || !reflect.DeepEqual(deliveries[index], firing) {
		return false
	}
	for _, d := range deliveries[index+1:] {
		if d.AlertName != firing.AlertName || d.Labels["operator_namespace"] != firing.Labels["operator_namespace"] ||
			d.Labels["operator_metrics_service"] != firing.Labels["operator_metrics_service"] {
			continue
		}
		if d.Status != "firing" || !d.StartsAt.Equal(firing.StartsAt) || d.Receiver != firing.Receiver ||
			!maps.Equal(d.Labels, firing.Labels) || d.Annotations["runbook_url"] != firing.Annotations["runbook_url"] {
			return false
		}
	}
	return true
}

func alPlanStoreDelivered(d alDelivery, h alPlanStoreHistory) bool {
	return !h.crossedLower.IsZero() && !d.StartsAt.Before(h.crossedLower) && !d.ReceivedAt.Before(d.StartsAt) && !d.ReceivedAt.After(h.crossedLower.Add(alDetectionSlack)) && !h.through.Before(d.ReceivedAt)
}
func alPlanStoreResolved(firing, resolved alDelivery, h alPlanStoreHistory, pruningStarted time.Time) bool {
	return !h.clearedLower.IsZero() && !pruningStarted.IsZero() && !h.clearedUpper.Before(pruningStarted) && resolved.StartsAt.Equal(firing.StartsAt) &&
		!resolved.EndsAt.Before(h.clearedLower) && !resolved.ReceivedAt.Before(resolved.EndsAt) && !resolved.ReceivedAt.After(h.clearedLower.Add(alDetectionSlack)) && !h.through.Before(resolved.ReceivedAt)
}
