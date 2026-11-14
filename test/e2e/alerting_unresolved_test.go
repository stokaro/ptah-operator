package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

type alUnresolvedHistoryFixture struct {
	gauge, synced, failures, up, duration []alAdmissionSeries
}

func TestAlUnresolvedNativeHistoryRequiresOneIncidentAndHealthyReplicas(t *testing.T) {
	start := time.Unix(1800000000, 0).UTC()
	at := start.Add(46 * time.Second)
	fixture := func() alUnresolvedHistoryFixture {
		var f alUnresolvedHistoryFixture
		for i, pod := range []string{"leader", "follower"} {
			labels := map[string]string{"job": alScrapeJob, "pod": pod, "instance": fmt.Sprintf("10.0.0.%d:8080", i)}
			series := func(metric string) alAdmissionSeries {
				m := maps.Clone(labels)
				m["__name__"] = metric
				return alAdmissionSeries{Metric: m}
			}
			g, v, c, u, d := series(alUnresolvedMetric), series(alViewSyncedMetric), series(alViewReadFailuresMetric), series("up"), series("scrape_duration_seconds")
			g.Metric["family"] = "migration"
			synced := "0"
			if pod == "leader" {
				synced = "1"
			}
			for second := -15; second <= 45; second += 5 {
				when := start.Add(time.Duration(second) * time.Second)
				value := "0"
				if second > 0 && second < 20 {
					value = "1"
				}
				g.Values = append(g.Values, alAdmissionHistorySampleForTest(when, value))
				v.Values = append(v.Values, alAdmissionHistorySampleForTest(when, synced))
				c.Values = append(c.Values, alAdmissionHistorySampleForTest(when, "0"))
				u.Values = append(u.Values, alAdmissionHistorySampleForTest(when, "1"))
				d.Values = append(d.Values, alAdmissionHistorySampleForTest(when, "0.125"))
			}
			if pod == "leader" {
				f.gauge = append(f.gauge, g)
			}
			f.synced = append(f.synced, v)
			f.failures = append(f.failures, c)
			f.up = append(f.up, u)
			f.duration = append(f.duration, d)
		}
		return f
	}
	read := func(f alUnresolvedHistoryFixture) (alUnresolvedHistory, error) {
		body := func(series []alAdmissionSeries) []byte { return alAdmissionHistoryBodyForTest(t, series) }
		return alReadUnresolvedHistory(body(f.gauge), body(f.synced), body(f.failures), body(f.up), body(f.duration), []string{"leader", "follower"}, "leader", "migration", start, at)
	}
	h, err := read(fixture())
	if err != nil || h.latest != 0 || h.declared != 0 || !h.appeared.Equal(start.Add(5*time.Second+125*time.Millisecond)) || !h.appearedScrape.Equal(start.Add(5*time.Second)) ||
		!h.withheldSince.IsZero() || !h.cleared.Equal(start.Add(20*time.Second+125*time.Millisecond)) || !h.heldFrom.Equal(start.Add(20*time.Second)) || !h.through.Equal(start.Add(45*time.Second)) {
		t.Fatalf("native history=%+v, %v", h, err)
	}
	// The leader declares the failed reads at 0s and 5s: the gauge is
	// absent, the view reads 0 and the counter rises at each of them.
	declare := func(f *alUnresolvedHistoryFixture, seconds ...int) {
		count := 0
		for _, second := range seconds {
			when := start.Add(time.Duration(second) * time.Second)
			index := (second + 15) / 5
			for i, sample := range f.gauge[0].Values {
				if string(sample[0]) == string(alAdmissionHistorySampleForTest(when, "")[0]) {
					f.gauge[0].Values = append(f.gauge[0].Values[:i:i], f.gauge[0].Values[i+1:]...)
					break
				}
			}
			f.synced[0].Values[index] = alAdmissionHistorySampleForTest(when, "0")
			count++
			for i := index; i < len(f.failures[0].Values); i++ {
				f.failures[0].Values[i] = alAdmissionHistorySampleForTest(start.Add(time.Duration(i*5-15)*time.Second), fmt.Sprint(count))
			}
		}
	}
	declared := fixture()
	declare(&declared, 0, 5)
	h, err = read(declared)
	// The incident is first read at 10s, after the two reads that withheld
	// it, and the history still reaches the last scrape.
	if err != nil || h.declared != 2 || !h.withheldSince.Equal(start) || !h.appearedScrape.Equal(start.Add(10*time.Second)) || !h.heldFrom.Equal(start.Add(20*time.Second)) || !h.through.Equal(start.Add(45*time.Second)) {
		t.Fatalf("declared failed reads before the incident = %+v, %v", h, err)
	}
	inside := fixture()
	declare(&inside, 20, 25)
	h, err = read(inside)
	// The incident was last read at 15s, so the rule's hold can have started
	// at 20s, the first failed read.
	if err != nil || h.declared != 2 || !h.heldFrom.Equal(start.Add(20*time.Second)) || !h.cleared.Equal(start.Add(30*time.Second+125*time.Millisecond)) {
		t.Fatalf("declared failed reads at the end of the incident = %+v, %v", h, err)
	}
	for _, name := range []string{"missing gauge", "wrong family", "follower gauge", "wrong instance", "missing follower", "unhealthy leader", "unhealthy follower", "missing sample", "different sample time", "missing baseline", "existing incident", "second incident", "recurred incident",
		"no view", "view unsynchronized at a read", "view from the follower", "counter did not rise", "counter rose on the follower", "view synchronized at a failed read", "gap outlasted the hold", "trailing gap outlasted the hold"} {
		t.Run(name, func(t *testing.T) {
			f := fixture()
			switch name {
			case "missing gauge":
				f.gauge = nil
			case "wrong family":
				f.gauge[0].Metric["family"] = "schema"
			case "follower gauge":
				f.gauge[0].Metric["pod"] = "follower"
			case "wrong instance":
				f.gauge[0].Metric["instance"] = "other"
			case "missing follower":
				f.up = f.up[:1]
			case "unhealthy leader":
				f.up[0].Values[4] = alAdmissionHistorySampleForTest(start.Add(5*time.Second), "0")
			case "unhealthy follower":
				f.up[1].Values[4] = alAdmissionHistorySampleForTest(start.Add(5*time.Second), "0")
			case "missing sample":
				f.gauge[0].Values = append(f.gauge[0].Values[:4], f.gauge[0].Values[5:]...)
			case "different sample time":
				f.gauge[0].Values[4] = alAdmissionHistorySampleForTest(start.Add(5500*time.Millisecond), "1")
			case "missing baseline":
				f.gauge[0].Values = f.gauge[0].Values[4:]
			case "existing incident":
				f.gauge[0].Values[1] = alAdmissionHistorySampleForTest(start.Add(-10*time.Second), "1")
			case "second incident":
				f.gauge[0].Values[4] = alAdmissionHistorySampleForTest(start.Add(5*time.Second), "2")
			case "recurred incident":
				f.gauge[0].Values[9] = alAdmissionHistorySampleForTest(start.Add(30*time.Second), "1")
			case "no view":
				f.synced = nil
			case "view unsynchronized at a read":
				f.synced[0].Values[6] = alAdmissionHistorySampleForTest(start.Add(15*time.Second), "0")
			case "view from the follower":
				f.synced = f.synced[1:]
			case "counter did not rise":
				declare(&f, 5)
				for i := range f.failures[0].Values {
					f.failures[0].Values[i] = alAdmissionHistorySampleForTest(start.Add(time.Duration(i*5-15)*time.Second), "0")
				}
			case "counter rose on the follower":
				declare(&f, 5)
				f.failures[0], f.failures[1] = f.failures[1], f.failures[0]
				f.failures[0].Metric, f.failures[1].Metric = f.failures[1].Metric, f.failures[0].Metric
			case "view synchronized at a failed read":
				declare(&f, 5)
				f.synced[0].Values[4] = alAdmissionHistorySampleForTest(start.Add(5*time.Second), "1")
			case "gap outlasted the hold":
				// From the failed read at 5s to the completion of the read
				// at 35s: thirty seconds and 125 milliseconds.
				declare(&f, 5, 10, 15, 20, 25, 30)
			case "trailing gap outlasted the hold":
				declare(&f, 15, 20, 25, 30, 35, 40, 45)
			}
			if _, err := read(f); err == nil {
				t.Fatal("incomplete or contaminated history passed")
			}
		})
	}
	// One failed read fewer is held.
	held := fixture()
	declare(&held, 5, 10, 15, 20, 25)
	if h, err := read(held); err != nil || h.declared != 5 {
		t.Fatalf("a gap the hold covers was refused: %+v, %v", h, err)
	}
}

// The scrapes that failed CI run 43593974962: the leader's reads at 12:46:42
// and 12:46:47 took 3.005 s, the snapshot read's own timeout, and carried no
// gauge. The row did not query the view or the read-failure counter, so the
// test rebuilds them from the scrapes the reading holds: one sample per scrape
// and Pod, the leader's view 0 and its counter rising at each failed read
// unless a row moves them, the follower's view 0 and counter 0 throughout.
func TestAlUnresolvedHistoryAcceptsTheCIReadFailureOnlyWhenDeclared(t *testing.T) {
	t.Parallel()
	const (
		leader   = "ptah-runtime-generated-name-prefix-boundary-proof-079e1661gph8q"
		follower = "ptah-runtime-generated-name-prefix-boundary-proof-079e1661ks7sc"
		failed1  = "1794660402.241"
		failed2  = "1794660407.241"
	)
	instant := func(value string) time.Time {
		t.Helper()
		at, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	type reconstruction struct {
		// omit removes more leader gauge samples; failed are the leader
		// scrapes whose view reads 0; rises are the scrapes, by Pod, at
		// which the counter rises.
		omit, failed []string
		rises        map[string][]string
		noCounter    bool
	}
	declared := func() reconstruction {
		return reconstruction{failed: []string{failed1, failed2}, rises: map[string][]string{leader: {failed1, failed2}}}
	}
	for _, family := range []struct {
		name                         string
		started, queriedAt           string
		appeared, cleared, gapClosed string
	}{
		// Each lane began at the instant the log shows it continuing, a
		// lower bound for the row's own start.
		{"schema", "2026-11-14T12:42:40.3279289Z", "2026-11-14T12:50:39.019988963Z", "1794660227.241", "1794660587.241", "1794660412.241"},
		{"migration", "2026-11-14T12:42:40.3278431Z", "2026-11-14T12:50:59.17780816Z", "1794660207.241", "1794660622.241", "1794660412.241"},
	} {
		t.Run(family.name, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile("../../testdata/e2e/readings/prometheus-unresolved-read-gap-" + family.name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			started, at := instant(family.started), instant(family.queriedAt)
			// A scrape timestamp as the native parser reads it.
			scrape := func(value string) time.Time {
				t.Helper()
				samples, err := alNativeSamples(alAdmissionSeries{Values: [][]json.RawMessage{{json.RawMessage(value), json.RawMessage(`"0"`)}}}, at, at, false, false)
				if err != nil {
					t.Fatal(err)
				}
				return samples[0].at
			}
			duration := map[string]time.Duration{}
			read := func(r reconstruction) (alUnresolvedHistory, error) {
				t.Helper()
				series, err := alAdmissionNativeMatrix(raw)
				if err != nil {
					t.Fatal(err)
				}
				omitted := map[string]bool{}
				for _, value := range r.omit {
					omitted[value] = true
				}
				var rebuilt []alAdmissionSeries
				for i, s := range series {
					switch s.Metric["__name__"] {
					case alUnresolvedMetric:
						g := alAdmissionSeries{Metric: s.Metric}
						for _, sample := range s.Values {
							if !omitted[string(sample[0])] {
								g.Values = append(g.Values, sample)
							}
						}
						series[i] = g
					case "scrape_duration_seconds":
						view := alAdmissionSeries{Metric: maps.Clone(s.Metric)}
						view.Metric["__name__"] = alViewSyncedMetric
						counter := alAdmissionSeries{Metric: maps.Clone(s.Metric)}
						counter.Metric["__name__"] = alViewReadFailuresMetric
						count := 0
						for _, sample := range s.Values {
							if s.Metric["pod"] == leader {
								var text string
								if err := json.Unmarshal(sample[1], &text); err != nil {
									t.Fatal(err)
								}
								seconds, err := strconv.ParseFloat(text, 64)
								if err != nil {
									t.Fatal(err)
								}
								duration[string(sample[0])] = time.Duration(seconds * float64(time.Second))
							}
							synced := "0"
							if s.Metric["pod"] == leader && !slices.Contains(r.failed, string(sample[0])) {
								synced = "1"
							}
							if slices.Contains(r.rises[s.Metric["pod"]], string(sample[0])) {
								count++
							}
							view.Values = append(view.Values, []json.RawMessage{sample[0], json.RawMessage(fmt.Sprintf("%q", synced))})
							counter.Values = append(counter.Values, []json.RawMessage{sample[0], json.RawMessage(fmt.Sprintf("%q", fmt.Sprint(count)))})
						}
						rebuilt = append(rebuilt, view)
						if !r.noCounter {
							rebuilt = append(rebuilt, counter)
						}
					}
				}
				groups, err := alSplitHistorySnapshot(alAdmissionHistoryBodyForTest(t, append(series, rebuilt...)), alScrapeJob, alUnresolvedHistoryMetrics)
				if err != nil {
					t.Fatal(err)
				}
				return alReadUnresolvedHistory(groups[alUnresolvedMetric], groups[alViewSyncedMetric], groups[alViewReadFailuresMetric], groups["up"], groups["scrape_duration_seconds"], []string{leader, follower}, leader, family.name, started, at)
			}
			undeclared := declared()
			undeclared.noCounter = true
			if _, err := read(undeclared); err == nil || err.Error() != "unresolved gauge omitted a native scrape" {
				t.Fatalf("the CI reading without its counter no longer reproduces the omission: %v", err)
			}
			h, err := read(declared())
			appeared, cleared := scrape(family.appeared), scrape(family.cleared)
			// The incident spans the failed reads, so the history holds it from
			// its first reading to the reading that cleared it.
			if err != nil || h.declared != 2 || h.latest != 0 || !h.appearedScrape.Equal(appeared) || !h.appeared.Equal(appeared.Add(duration[family.appeared])) || !h.withheldSince.IsZero() ||
				!h.heldFrom.Equal(cleared) || !h.cleared.Equal(cleared.Add(duration[family.cleared])) || h.through.Before(at.Add(-alScrapeInterval)) {
				t.Fatalf("history across the declared failed reads = %+v, %v", h, err)
			}
			if gap := scrape(family.gapClosed).Add(duration[family.gapClosed]).Sub(scrape(failed1)); gap >= alStateReadGapHold/2 {
				t.Fatalf("the CI gap lasted %s, so this reading no longer measures a short one", gap)
			}
			for _, row := range []struct {
				name   string
				mutate func(*reconstruction)
			}{
				{"no failure counted", func(r *reconstruction) { r.rises = nil }},
				{"counted one scrape later", func(r *reconstruction) {
					r.rises = map[string][]string{leader: {failed2, family.gapClosed}}
				}},
				{"counted by the follower", func(r *reconstruction) {
					r.rises = map[string][]string{follower: {"1794660400.701", "1794660405.701"}}
				}},
				{"view synchronized at a failed read", func(r *reconstruction) { r.failed = []string{failed2} }},
			} {
				t.Run(row.name, func(t *testing.T) {
					r := declared()
					row.mutate(&r)
					if _, err := read(r); err == nil || err.Error() != "unresolved gauge omitted a native scrape" {
						t.Fatalf("an omission the leader did not declare was accepted: %v", err)
					}
				})
			}
			t.Run("view unsynchronized at a read", func(t *testing.T) {
				r := declared()
				r.failed = append(r.failed, family.gapClosed)
				if _, err := read(r); err == nil || err.Error() != "unresolved gauge was read without a synchronized view" {
					t.Fatalf("a gauge read beside an unsynchronized view was accepted: %v", err)
				}
			})
			// Extend the CI gap by failed reads the leader declares. Five in
			// a row end inside the hold; six end at 12:47:12 plus the read,
			// more than thirty seconds after the first.
			extend := func(extra int) reconstruction {
				r := declared()
				for i := 1; i <= extra; i++ {
					value := fmt.Sprintf("%.3f", 1794660407.241+float64(5*i))
					r.omit = append(r.omit, value)
					r.failed = append(r.failed, value)
					r.rises[leader] = append(r.rises[leader], value)
				}
				return r
			}
			if h, err := read(extend(3)); err != nil || h.declared != 5 {
				t.Fatalf("five declared failed reads were refused: %+v, %v", h, err)
			}
			if _, err := read(extend(4)); !errors.Is(err, errAlReadGapOutlastedHold) {
				t.Fatalf("six declared failed reads were accepted: %v", err)
			}
		})
	}
}

func TestAlUnresolvedTimingUsesPersistedRecords(t *testing.T) {
	recorded := time.Unix(1800000000, 0).UTC()
	accounted := recorded.Add(90 * time.Second)
	h := alUnresolvedHistory{appeared: recorded.Add(5 * time.Second), appearedScrape: recorded.Add(4 * time.Second), latest: 1}
	firing := alDelivery{StartsAt: recorded.Add(10 * time.Second), ReceivedAt: recorded.Add(alDetectionSlack)}
	if !alUnresolvedDelivered(firing, h, recorded) {
		t.Fatal("on-time firing refused")
	}
	// Failed reads from 2s to 17s withheld the incident for fifteen seconds,
	// and failed reads before the record withheld it only from the record on.
	withheld := h
	withheld.withheldSince, withheld.appearedScrape, withheld.appeared = recorded.Add(2*time.Second), recorded.Add(17*time.Second), recorded.Add(18*time.Second)
	early := withheld
	early.withheldSince = recorded.Add(-20 * time.Second)
	late := firing
	late.ReceivedAt = recorded.Add(alDetectionSlack + 15*time.Second)
	if !alUnresolvedDelivered(late, withheld, recorded) {
		t.Fatal("a firing the leader's failed reads withheld was refused")
	}
	late.ReceivedAt = recorded.Add(alDetectionSlack + 17*time.Second)
	if !alUnresolvedDelivered(late, early, recorded) {
		t.Fatal("a firing failed reads withheld from its record on was refused")
	}
	for _, name := range []string{"late firing", "pre-existing firing", "missing gauge", "already cleared", "no record", "later than the withheld time", "withheld before the record"} {
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
		case "later than the withheld time":
			b, d.ReceivedAt = withheld, recorded.Add(alDetectionSlack+15*time.Second+time.Nanosecond)
		case "withheld before the record":
			b, d.ReceivedAt = early, recorded.Add(alDetectionSlack+17*time.Second+time.Nanosecond)
		}
		if alUnresolvedDelivered(d, b, at) {
			t.Errorf("%s accepted", name)
		}
	}
	// The clearing reading's scrape is at accounted+5s, so the rule's hold
	// ends 30s later. The receiver hears of it within the hold and the
	// delivery target after the record was accounted for.
	h.cleared, h.heldFrom, h.latest, h.through = accounted.Add(5*time.Second+125*time.Millisecond), accounted.Add(5*time.Second), 0, accounted.Add(90*time.Second)
	resolved := alDelivery{StartsAt: firing.StartsAt, EndsAt: h.heldFrom.Add(alStateReadGapHold), ReceivedAt: accounted.Add(alStateReadGapHold + alDetectionSlack)}
	if !alUnresolvedCleared(firing, resolved, h, recorded, accounted) {
		t.Fatal("on-time recovery refused")
	}
	for _, name := range []string{"late recovery", "another incident", "before accounting", "gauge disappeared", "ended before repair", "missing final scrape", "record never resolved", "resolved inside the hold", "no hold"} {
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
			b.heldFrom, d.EndsAt = at.Add(-alStateReadGapHold-2*time.Second), at.Add(-time.Second)
		case "missing final scrape":
			b.through = d.ReceivedAt.Add(-time.Nanosecond)
		case "record never resolved":
			at = recorded
		case "resolved inside the hold":
			d.EndsAt = b.heldFrom.Add(alStateReadGapHold - time.Nanosecond)
		case "no hold":
			b.heldFrom = time.Time{}
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
