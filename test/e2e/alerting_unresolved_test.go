package e2e

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"testing"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestAlUnresolvedNativeHistoryRequiresOneIncidentAndHealthyReplicas(t *testing.T) {
	start := time.Unix(1800000000, 0).UTC()
	at := start.Add(46 * time.Second)
	fixture := func() ([]alAdmissionSeries, []alAdmissionSeries, []alAdmissionSeries) {
		var gauge, up, duration []alAdmissionSeries
		for i, pod := range []string{"leader", "follower"} {
			labels := map[string]string{"job": alScrapeJob, "pod": pod, "instance": fmt.Sprintf("10.0.0.%d:8080", i)}
			series := func(metric string) alAdmissionSeries {
				m := maps.Clone(labels)
				m["__name__"] = metric
				return alAdmissionSeries{Metric: m}
			}
			g, u, d := series(alUnresolvedMetric), series("up"), series("scrape_duration_seconds")
			g.Metric["family"] = "migration"
			for second := -15; second <= 45; second += 5 {
				when := start.Add(time.Duration(second) * time.Second)
				value := "0"
				if second > 0 && second < 20 {
					value = "1"
				}
				g.Values = append(g.Values, alAdmissionHistorySampleForTest(when, value))
				u.Values = append(u.Values, alAdmissionHistorySampleForTest(when, "1"))
				d.Values = append(d.Values, alAdmissionHistorySampleForTest(when, "0.125"))
			}
			if pod == "leader" {
				gauge = append(gauge, g)
			}
			up = append(up, u)
			duration = append(duration, d)
		}
		return gauge, up, duration
	}
	read := func(g, u, d []alAdmissionSeries) (alUnresolvedHistory, error) {
		return alReadUnresolvedHistory(alAdmissionHistoryBodyForTest(t, g), alAdmissionHistoryBodyForTest(t, u), alAdmissionHistoryBodyForTest(t, d), []string{"leader", "follower"}, "leader", "migration", start, at)
	}
	g, u, d := fixture()
	h, err := read(g, u, d)
	if err != nil || h.latest != 0 || !h.appeared.Equal(start.Add(5*time.Second+125*time.Millisecond)) || !h.cleared.Equal(start.Add(20*time.Second+125*time.Millisecond)) {
		t.Fatalf("native history=%+v, %v", h, err)
	}
	for _, name := range []string{"missing gauge", "wrong family", "follower gauge", "wrong instance", "missing follower", "unhealthy leader", "unhealthy follower", "missing sample", "different sample time", "missing baseline", "existing incident", "second incident", "recurred incident"} {
		t.Run(name, func(t *testing.T) {
			g, u, d := fixture()
			switch name {
			case "missing gauge":
				g = nil
			case "wrong family":
				g[0].Metric["family"] = "schema"
			case "follower gauge":
				g[0].Metric["pod"] = "follower"
			case "wrong instance":
				g[0].Metric["instance"] = "other"
			case "missing follower":
				u = u[:1]
			case "unhealthy leader":
				u[0].Values[4] = alAdmissionHistorySampleForTest(start.Add(5*time.Second), "0")
			case "unhealthy follower":
				u[1].Values[4] = alAdmissionHistorySampleForTest(start.Add(5*time.Second), "0")
			case "missing sample":
				g[0].Values = append(g[0].Values[:4], g[0].Values[5:]...)
			case "different sample time":
				g[0].Values[4] = alAdmissionHistorySampleForTest(start.Add(5500*time.Millisecond), "1")
			case "missing baseline":
				g[0].Values = g[0].Values[4:]
			case "existing incident":
				g[0].Values[1] = alAdmissionHistorySampleForTest(start.Add(-10*time.Second), "1")
			case "second incident":
				g[0].Values[4] = alAdmissionHistorySampleForTest(start.Add(5*time.Second), "2")
			case "recurred incident":
				g[0].Values[9] = alAdmissionHistorySampleForTest(start.Add(30*time.Second), "1")
			}
			if _, err := read(g, u, d); err == nil {
				t.Fatal("incomplete or contaminated history passed")
			}
		})
	}
}

func TestAlUnresolvedTimingUsesPersistedRecords(t *testing.T) {
	recorded := time.Unix(1800000000, 0).UTC()
	accounted := recorded.Add(90 * time.Second)
	h := alUnresolvedHistory{appeared: recorded.Add(5 * time.Second), latest: 1}
	firing := alDelivery{StartsAt: recorded.Add(10 * time.Second), ReceivedAt: recorded.Add(alDetectionSlack)}
	if !alUnresolvedDelivered(firing, h, recorded) {
		t.Fatal("on-time firing refused")
	}
	for _, name := range []string{"late firing", "pre-existing firing", "missing gauge", "already cleared", "no record"} {
		d, b, at := firing, h, recorded
		switch name {
		case "late firing":
			d.ReceivedAt = d.ReceivedAt.Add(time.Nanosecond)
		case "pre-existing firing":
			d.StartsAt = at.Add(-time.Second)
		case "missing gauge":
			b.appeared = time.Time{}
		case "already cleared":
			b.cleared = at.Add(20 * time.Second)
		case "no record":
			at = time.Time{}
		}
		if alUnresolvedDelivered(d, b, at) {
			t.Errorf("%s accepted", name)
		}
	}
	h.cleared, h.latest, h.through = accounted.Add(5*time.Second), 0, accounted.Add(50*time.Second)
	resolved := alDelivery{StartsAt: firing.StartsAt, EndsAt: accounted.Add(5 * time.Second), ReceivedAt: accounted.Add(alDetectionSlack)}
	if !alUnresolvedCleared(firing, resolved, h, recorded, accounted) {
		t.Fatal("on-time recovery refused")
	}
	for _, name := range []string{"late recovery", "another incident", "before accounting", "gauge disappeared", "ended before repair", "missing final scrape", "record never resolved"} {
		d, b, at := resolved, h, accounted
		switch name {
		case "late recovery":
			d.ReceivedAt = d.ReceivedAt.Add(time.Nanosecond)
		case "another incident":
			d.StartsAt = d.StartsAt.Add(time.Second)
		case "before accounting":
			at = b.cleared.Add(time.Second)
		case "gauge disappeared":
			b.cleared = time.Time{}
		case "ended before repair":
			d.EndsAt = at.Add(-time.Second)
		case "missing final scrape":
			b.through = d.ReceivedAt.Add(-time.Nanosecond)
		case "record never resolved":
			at = recorded
		}
		if alUnresolvedCleared(firing, d, b, recorded, at) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAlUnresolvedRecordRetainsTheNativeRunThroughRepair(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/e2e/readings/uncertain-migration-retained-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	var readings map[string]*ptahv1.PtahMigration
	if err = json.Unmarshal(raw, &readings); err != nil {
		t.Fatal(err)
	}
	if len(readings) != 2 {
		t.Fatal("both native engine readings required")
	}
	for engine, native := range readings {
		t.Run(engine, func(t *testing.T) {
			v := native.DeepCopy()
			v.UID = "test-resource-uid"
			// The retained fixture projects status. Reconstruct the metadata copy from
			// that native record so this test can also hold its guard to corruption.
			copy, err := json.Marshal(v.Status.UnresolvedRun)
			if err != nil {
				t.Fatal(err)
			}
			v.Annotations = map[string]string{ptahv1.UnresolvedRunAnnotation: string(copy)}
			original := v.Status.ActiveOperation.DeepCopy()
			before := v.DeepCopy()
			before.Status.UnresolvedRun = nil
			before.Annotations = nil
			events := []watchEvent[*ptahv1.PtahMigration]{{Type: watch.Added, Object: before}, {Type: watch.Modified, Object: v}, {Type: watch.Modified, Object: v.DeepCopy()}}
			got, err := alUnresolvedMigrationRecord(events, v.Name, v.UID, original, v.Status.UnresolvedRun.TargetIdentityDigest)
			if err != nil || got == nil || !got.RecordedAt.Equal(&v.Status.UnresolvedRun.RecordedAt) {
				t.Fatalf("native record refused: %v", err)
			}
			for _, name := range []string{"no record", "replaced resource", "deleted resource", "lost record", "changed operation", "changed job", "changed plan", "changed time", "lost copy", "changed copy", "copy lost job", "different target with matching copy"} {
				t.Run(name, func(t *testing.T) {
					last := v.DeepCopy()
					rows := append([]watchEvent[*ptahv1.PtahMigration](nil), events...)
					rows[2].Object = last
					switch name {
					case "no record":
						rows = rows[:1]
					case "replaced resource":
						last.UID = "replacement"
					case "deleted resource":
						rows[2].Type = watch.Deleted
					case "lost record":
						last.Status.UnresolvedRun = nil
					case "changed operation":
						last.Status.UnresolvedRun.OperationID = muDigest
					case "changed job":
						last.Status.UnresolvedRun.JobUID = "another"
					case "changed plan":
						last.Status.UnresolvedRun.PlanRef.UID = "another"
					case "changed time":
						last.Status.UnresolvedRun.RecordedAt.Time = last.Status.UnresolvedRun.RecordedAt.Add(time.Second)
					case "lost copy":
						last.Annotations = nil
					case "different target with matching copy":
						last.Status.UnresolvedRun.TargetIdentityDigest = muDigest
						b, err := json.Marshal(last.Status.UnresolvedRun)
						if err != nil {
							t.Fatal(err)
						}
						last.Annotations[ptahv1.UnresolvedRunAnnotation] = string(b)
					case "copy lost job":
						copy := last.Status.UnresolvedRun.DeepCopy()
						copy.JobUID = ""
						b, err := json.Marshal(copy)
						if err != nil {
							t.Fatal(err)
						}
						last.Annotations[ptahv1.UnresolvedRunAnnotation] = string(b)
					case "changed copy":
						last.Annotations[ptahv1.UnresolvedRunAnnotation] = "{}"
					}
					if _, err := alUnresolvedMigrationRecord(rows, v.Name, v.UID, original, v.Status.UnresolvedRun.TargetIdentityDigest); err == nil {
						t.Fatal("lost or replaced unresolved evidence passed")
					}
				})
			}
		})
	}
}

func TestAlUnresolvedRecoveryAllowsHistoryButRequiresFreshAuthorization(t *testing.T) {
	original := &ptahv1.MigrationOperationStatus{ID: muOperation, PlanRef: &ptahv1.ImmutableObjectReference{Name: "old", UID: "old-uid"}}
	fixture := func() *ptahv1.PtahMigration {
		v := &ptahv1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Name: "case", UID: "case-uid", Generation: 1}}
		v.Spec.Policy.Apply = ptahv1.ApplyPolicyOnApproval
		v.Status.ObservedGeneration = 1
		v.Status.Plan = &ptahv1.ImmutableObjectReference{Name: "fresh", UID: "fresh-uid"}
		v.Status.LastRun = &ptahv1.MigrationRunStatus{Outcome: ptahv1.MigrationRunOutcomeUnknown}
		v.Status.ResolvedRun = &ptahv1.ResolvedMigrationRunStatus{OperationID: original.ID, Resolution: ptahv1.MigrationRunResolvedByAcknowledgment, AcknowledgmentRef: &ptahv1.ImmutableObjectReference{Name: "ack", UID: "ack-uid"}, AcknowledgedBy: &ptahv1.ApprovalIdentity{Username: "person"}, ResolvedAt: muInstant}
		v.Status.History = &ptahv1.MigrationHistoryStatus{ObservedAt: metav1.NewTime(muInstant.Add(time.Second))}
		v.Status.Conditions = []metav1.Condition{{Type: ptahv1.ConditionApprovalRequired, Status: metav1.ConditionTrue, ObservedGeneration: 1}}
		return v
	}
	if !alUnresolvedMigrationReady(fixture(), original) {
		t.Fatal("a retained historical run blocked fresh authorization")
	}
	for _, name := range []string{"old plan", "active claim", "unresolved run", "no acknowledgment", "different run", "old reading", "stale condition", "already approved", "automatic policy", "suspended", "lost resolution time"} {
		v := fixture()
		switch name {
		case "old plan":
			v.Status.Plan = original.PlanRef
		case "active claim":
			v.Status.ActiveOperation = original
		case "unresolved run":
			v.Status.UnresolvedRun = &ptahv1.UnresolvedMigrationRunStatus{}
		case "no acknowledgment":
			v.Status.ResolvedRun.AcknowledgmentRef = nil
		case "different run":
			v.Status.ResolvedRun.OperationID = "other"
		case "old reading":
			v.Status.History.ObservedAt = muInstant
		case "stale condition":
			v.Status.Conditions[0].ObservedGeneration = 0
		case "already approved":
			v.Status.Conditions[0].Status = metav1.ConditionFalse
		case "automatic policy":
			v.Spec.Policy.Apply = ptahv1.ApplyPolicyAlways
		case "suspended":
			v.Spec.Suspend = true
		case "lost resolution time":
			v.Status.ResolvedRun.ResolvedAt = metav1.Time{}
		}
		if alUnresolvedMigrationReady(v, original) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAlUnresolvedRepairRequiresTheExactEffectFreeFixture(t *testing.T) {
	for _, engine := range []string{"postgresql", "mysql"} {
		body, err := os.ReadFile("../../testdata/e2e/migrations/" + engine + "-uncertain/0000000003_settle_slowly.up.sql")
		if err != nil {
			t.Fatal(err)
		}
		if !alUnresolvedSleepFixture(engine, body) {
			t.Fatal("native sleep fixture refused")
		}
		for _, mutation := range [][]byte{append(append([]byte(nil), body...), []byte("\nALTER TABLE e2e_migration_widgets ADD COLUMN hidden int;")...), []byte("SELECT SLEEP(1);"), nil} {
			if alUnresolvedSleepFixture(engine, mutation) {
				t.Fatal("a migration with unaccounted effects or no interruption window passed")
			}
		}
	}
	if alUnresolvedSleepFixture("other", []byte("SELECT pg_sleep(45);")) {
		t.Fatal("unknown engine accepted")
	}
}
