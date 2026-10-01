package e2e

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"testing"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func TestAlPlanPinsIncludesEveryDocumentedReference(t *testing.T) {
	ref := func(name string) ptahv1.ImmutableObjectReference {
		return ptahv1.ImmutableObjectReference{Name: name, UID: types.UID(name + "-uid")}
	}
	current := func(name string) *ptahv1.CurrentPlanStatus {
		return &ptahv1.CurrentPlanStatus{Name: name, UID: types.UID(name + "-uid")}
	}
	s := ptahv1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: "one"}}
	s.Status.Plan = current("schema-current")
	s.Status.PendingObservation = &ptahv1.PendingObservationStatus{Plan: *current("schema-pending")}
	s.Status.Applied = &ptahv1.AppliedStatus{PlanRef: ref("schema-applied")}
	s.Status.PendingBindingRetirement = &ptahv1.BindingRetirementStatus{Plan: &ptahv1.RetiredPlanReference{Name: "schema-retired", UID: "schema-retired-uid"}}
	m := ptahv1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Namespace: "one"}}
	p := ref("migration-current")
	m.Status.Plan = &p
	m.Status.ActiveOperation = &ptahv1.MigrationOperationStatus{PlanRef: ptr.To(ref("migration-active"))}
	m.Status.UnresolvedRun = &ptahv1.UnresolvedMigrationRunStatus{PlanRef: ref("migration-unresolved")}
	sa := ptahv1.PtahSchemaApproval{ObjectMeta: metav1.ObjectMeta{Namespace: "one"}}
	sa.Spec.PlanRef = ref("schema-approved")
	ma := ptahv1.PtahMigrationApproval{ObjectMeta: metav1.ObjectMeta{Namespace: "one"}}
	ma.Spec.PlanRef = ref("migration-approved")
	pins, err := alPlanPins([]ptahv1.PtahSchema{s}, []ptahv1.PtahMigration{m}, []ptahv1.PtahSchemaApproval{sa}, []ptahv1.PtahMigrationApproval{ma})
	if err != nil || len(pins) != 9 {
		t.Fatalf("complete pins = %v, %v", pins, err)
	}
	for _, family := range []string{"schema", "migration"} {
		for key, uid := range pins {
			if key.family == family && uid != types.UID(key.name+"-uid") {
				t.Fatal("pin lost its immutable identity")
			}
		}
	}
	// A collision across a namespace or kind is not the same plan.
	other := s.DeepCopy()
	other.Namespace = "two"
	more, err := alPlanPins([]ptahv1.PtahSchema{s, *other}, nil, nil, nil)
	if err != nil || len(more) != 8 {
		t.Fatalf("namespace pins collapsed: %v, %v", more, err)
	}
	ma.Spec.PlanRef = sa.Spec.PlanRef
	more, err = alPlanPins(nil, nil, []ptahv1.PtahSchemaApproval{sa}, []ptahv1.PtahMigrationApproval{ma})
	if err != nil || len(more) != 2 {
		t.Fatalf("family pins collapsed: %v, %v", more, err)
	}
	conflicting := sa.DeepCopy()
	conflicting.Spec.PlanRef.UID = "replacement"
	if _, err := alPlanPins(nil, nil, []ptahv1.PtahSchemaApproval{sa, *conflicting}, nil); err == nil {
		t.Fatal("conflicting UID accepted")
	}
	sa.Spec.PlanRef.UID = ""
	if _, err := alPlanPins(nil, nil, []ptahv1.PtahSchemaApproval{sa}, nil); err == nil {
		t.Fatal("incomplete pin accepted")
	}
	if _, err := alPlanPins(nil, nil, nil, nil); err == nil {
		t.Fatal("empty inventory accepted")
	}
}

func alPlanExportFixture() alPlanExport {
	hash := func(b []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(b)) }
	owner := func(kind, name string, uid types.UID) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: ptahv1.GroupVersion.String(), Kind: kind, Name: name, UID: uid, Controller: ptr.To(true)}}
	}
	p := &ptahv1.PtahSchemaPlan{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "one", UID: "old-uid", ResourceVersion: "1", OwnerReferences: owner("PtahSchema", "owner", "owner-uid")}}
	p.Spec.SchemaRef = ptahv1.ImmutableObjectReference{Name: "owner", UID: "owner-uid"}
	e := alPlanExport{Plan: p}
	for i, b := range [][]byte{[]byte("first"), []byte("second")} {
		name := fmt.Sprintf("chunk-%d", i)
		p.Spec.Chunks = append(p.Spec.Chunks, ptahv1.PlanChunkReference{Name: name, Index: int32(i), Size: int32(len(b)), Digest: hash(b)})
		e.Chunks = append(e.Chunks, ptahv1.PtahSchemaPlanChunk{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.Namespace, UID: types.UID(name + "-uid"), OwnerReferences: owner("PtahSchemaPlan", p.Name, p.UID)}, Spec: ptahv1.PtahSchemaPlanChunkSpec{Data: b}})
	}
	p.Spec.Size = 11
	p.Spec.ContentDigest = hash([]byte("firstsecond"))
	return e
}
func TestAlPlanExportRequiresEveryExactChunkAndRoundTrips(t *testing.T) {
	e := alPlanExportFixture()
	b, err := e.archive()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := alReadPlanExport(b)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := restored.document()
	if err != nil || string(doc) != "firstsecond" {
		t.Fatalf("archive changed payload: %q %v", doc, err)
	}
	for _, mutation := range []string{"missing chunk", "extra chunk", "wrong order", "wrong owner", "wrong namespace", "missing UID", "wrong chunk digest", "wrong payload digest", "wrong size", "corrupt bytes", "empty chunks"} {
		t.Run(mutation, func(t *testing.T) {
			bad := alPlanExportFixture()
			switch mutation {
			case "missing chunk":
				bad.Chunks = bad.Chunks[:1]
			case "extra chunk":
				bad.Chunks = append(bad.Chunks, bad.Chunks[0])
			case "wrong order":
				bad.Plan.Spec.Chunks[0].Index = 1
			case "wrong owner":
				bad.Chunks[0].OwnerReferences[0].UID = "replacement"
			case "wrong namespace":
				bad.Chunks[0].Namespace = "other"
			case "missing UID":
				bad.Chunks[0].UID = ""
			case "wrong chunk digest":
				bad.Plan.Spec.Chunks[0].Digest = bad.Plan.Spec.Chunks[1].Digest
			case "wrong payload digest":
				bad.Plan.Spec.ContentDigest = bad.Plan.Spec.Chunks[0].Digest
			case "wrong size":
				bad.Plan.Spec.Size++
			case "corrupt bytes":
				bad.Chunks[0].Spec.Data[0] = 'X'
			case "empty chunks":
				bad.Chunks = nil
				bad.Plan.Spec.Chunks = nil
			}
			if _, err := bad.archive(); err == nil {
				t.Fatal("corrupt export accepted")
			}
		})
	}
	b[len(b)-1] ^= 1
	if _, err := alReadPlanExport(b); err == nil {
		t.Fatal("damaged archive accepted")
	}
}
func TestAlPlanPruningRequiresAnUnpinnedQuiescentOwner(t *testing.T) {
	fixture := func() (*ptahv1.PtahSchemaPlan, *ptahv1.PtahSchema, alPlanPinSet) {
		p := alPlanExportFixture().Plan
		s := &ptahv1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "one", UID: "owner-uid", Generation: 2}}
		s.Spec.Suspend = true
		s.Spec.Policy.Apply = ptahv1.ApplyPolicyNever
		s.Status.ObservedGeneration = 2
		s.Status.Plan = &ptahv1.CurrentPlanStatus{Name: "current", UID: "current-uid"}
		s.Status.Conditions = []metav1.Condition{{Type: ptahv1.ConditionSuspended, Status: metav1.ConditionTrue, Reason: "Requested", ObservedGeneration: 2}}
		return p, s, alPlanPinSet{{"schema", "one", "current"}: "current-uid"}
	}
	p, s, pins := fixture()
	if err := alPlanMayPrune(p, s, pins); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"pinned", "missing pins", "active operation", "pending observation", "unsuspended", "unobserved generation", "stale suspension", "different owner", "Apply allowed", "approval", "deleted owner"} {
		t.Run(mutation, func(t *testing.T) {
			p, s, pins := fixture()
			switch mutation {
			case "pinned":
				pins[alPlanKey{"schema", p.Namespace, p.Name}] = p.UID
			case "missing pins":
				pins = nil
			case "active operation":
				s.Status.ActiveOperation = &ptahv1.ActiveOperationStatus{Type: ptahv1.OperationPlan}
			case "pending observation":
				s.Status.PendingObservation = &ptahv1.PendingObservationStatus{}
			case "unsuspended":
				s.Spec.Suspend = false
			case "unobserved generation":
				s.Generation++
			case "stale suspension":
				s.Status.Conditions[0].ObservedGeneration--
			case "different owner":
				p.Spec.SchemaRef.UID = "other"
			case "Apply allowed":
				s.Spec.Policy.Apply = ptahv1.ApplyPolicyAlways
			case "approval":
				s.Status.Plan.Approval = &ptahv1.ConsumedApprovalStatus{}
			case "deleted owner":
				at := metav1.Now()
				s.DeletionTimestamp = &at
			}
			if err := alPlanMayPrune(p, s, pins); err == nil {
				t.Fatal("unsafe prune accepted")
			}
		})
	}
}

func TestAlPlanStoreMeasuresNativeCrossingAndRecovery(t *testing.T) {
	start := time.Unix(1800000000, 0).UTC()
	at := start.Add(46 * time.Second)
	fixture := func() ([]alAdmissionSeries, []alAdmissionSeries, []alAdmissionSeries) {
		var gauges, up, durations []alAdmissionSeries
		for i, pod := range []string{"leader", "follower"} {
			labels := map[string]string{"job": alScrapeJob, "pod": pod, "instance": fmt.Sprintf("10.0.0.%d:8080", i)}
			series := func(metric string) alAdmissionSeries {
				m := maps.Clone(labels)
				m["__name__"] = metric
				return alAdmissionSeries{Metric: m}
			}
			u, d, g := series("up"), series("scrape_duration_seconds"), series(alPlanStoreMetric)
			for second := -15; second <= 45; second += 5 {
				when := start.Add(time.Duration(second) * time.Second)
				size := alPlanStoreLimit
				if second > 0 && second < 20 {
					size++
				}
				if second >= 20 {
					size--
				}
				u.Values = append(u.Values, alAdmissionHistorySampleForTest(when, "1"))
				d.Values = append(d.Values, alAdmissionHistorySampleForTest(when, "0.125"))
				g.Values = append(g.Values, alAdmissionHistorySampleForTest(when, fmt.Sprint(size)))
			}
			up = append(up, u)
			durations = append(durations, d)
			if pod == "leader" {
				gauges = append(gauges, g)
			}
		}
		return gauges, up, durations
	}
	read := func(g, u, d []alAdmissionSeries) (alPlanStoreHistory, error) {
		return alReadPlanStoreHistory(alAdmissionHistoryBodyForTest(t, g), alAdmissionHistoryBodyForTest(t, u), alAdmissionHistoryBodyForTest(t, d), []string{"leader", "follower"}, "leader", start, at)
	}
	g, u, d := fixture()
	h, err := read(g, u, d)
	if err != nil || !h.crossedLower.Equal(start) || !h.clearedLower.Equal(start.Add(15*time.Second)) || h.latest != alPlanStoreLimit-1 {
		t.Fatalf("native transitions = %+v, %v", h, err)
	}
	for _, mutation := range []string{"missing bytes", "follower bytes", "missing follower", "unhealthy follower", "missing scrape", "early crossing", "recrossing", "wrong duration identity"} {
		t.Run(mutation, func(t *testing.T) {
			g, u, d := fixture()
			switch mutation {
			case "missing bytes":
				g = nil
			case "follower bytes":
				g[0].Metric["pod"] = "follower"
			case "missing follower":
				u = u[:1]
			case "unhealthy follower":
				u[1].Values[5] = alAdmissionHistorySampleForTest(start.Add(10*time.Second), "0")
			case "missing scrape":
				g[0].Values = append(g[0].Values[:5], g[0].Values[6:]...)
			case "early crossing":
				g[0].Values[2] = alAdmissionHistorySampleForTest(start.Add(-5*time.Second), fmt.Sprint(alPlanStoreLimit+1))
			case "recrossing":
				g[0].Values[10] = alAdmissionHistorySampleForTest(start.Add(35*time.Second), fmt.Sprint(alPlanStoreLimit+1))
			case "wrong duration identity":
				d[0].Metric["instance"] = "other"
			}
			if _, err := read(g, u, d); err == nil {
				t.Fatal("invalid native history accepted")
			}
		})
	}
	firing := alDelivery{StartsAt: start.Add(5 * time.Second), ReceivedAt: start.Add(alDetectionSlack)}
	if !alPlanStoreDelivered(firing, h) {
		t.Fatal("on-time delivery refused")
	}
	firing.ReceivedAt = firing.ReceivedAt.Add(time.Nanosecond)
	if alPlanStoreDelivered(firing, h) {
		t.Fatal("late firing accepted")
	}
	resolved := alDelivery{StartsAt: firing.StartsAt, EndsAt: start.Add(20 * time.Second), ReceivedAt: start.Add(25 * time.Second)}
	if !alPlanStoreResolved(firing, resolved, h, start.Add(16*time.Second)) {
		t.Fatal("on-time safe recovery refused")
	}
	if alPlanStoreResolved(firing, resolved, h, start.Add(21*time.Second)) {
		t.Fatal("clearing before pruning accepted")
	}
	resolved.StartsAt = resolved.StartsAt.Add(time.Second)
	if alPlanStoreResolved(firing, resolved, h, start.Add(16*time.Second)) {
		t.Fatal("another incident accepted")
	}
	resolved.StartsAt = firing.StartsAt
	resolved.ReceivedAt = h.clearedLower.Add(alDetectionSlack + time.Nanosecond)
	h.through = resolved.ReceivedAt
	if alPlanStoreResolved(firing, resolved, h, start.Add(16*time.Second)) {
		t.Fatal("late resolution accepted")
	}
}
