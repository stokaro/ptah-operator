package e2e

import (
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type alFailureHistoryFixture struct {
	started, queried        time.Time
	up, durations, counters []alAdmissionSeries
}

func alFailureHistoryFixtureForTest() alFailureHistoryFixture {
	f := alFailureHistoryFixture{started: time.Unix(1800000000, 0).UTC()}
	f.queried = f.started.Add(421 * time.Second)
	for i, pod := range []string{"leader", "follower"} {
		makeSeries := func(metric string) alAdmissionSeries {
			return alAdmissionSeries{Metric: map[string]string{"__name__": metric, "job": alScrapeJob, "pod": pod, "instance": fmt.Sprintf("10.0.0.%d:8080", i+1)}}
		}
		up, duration, counter := makeSeries("up"), makeSeries("scrape_duration_seconds"), makeSeries(alFailuresMetric)
		counter.Metric["family"], counter.Metric["stage"], counter.Metric["category"] = "schema", "resolve", "operation"
		for second := -305; second <= 420; second += 5 {
			at := f.started.Add(time.Duration(second) * time.Second)
			up.Values = append(up.Values, alAdmissionHistorySampleForTest(at, "1"))
			duration.Values = append(duration.Values, alAdmissionHistorySampleForTest(at, "0.125"))
			value := 7
			for _, failure := range []int{5, 20, 35, 50, 70} {
				if second >= failure && pod == "leader" {
					value++
				}
			}
			counter.Values = append(counter.Values, alAdmissionHistorySampleForTest(at, fmt.Sprint(value)))
		}
		f.up = append(f.up, up)
		f.durations = append(f.durations, duration)
		// A follower that never failed has no CounterVec child. Its complete
		// healthy scrape history remains mandatory even without that series.
		if pod == "leader" {
			f.counters = append(f.counters, counter)
		}
	}
	return f
}

func (f alFailureHistoryFixture) read(t *testing.T) (alFailuresHistory, error) {
	t.Helper()
	return alReadFailuresHistory(f.counters, f.up, f.durations, []string{"leader", "follower"}, "leader", "schema", f.started, f.queried)
}

func TestAlFailuresHistoryRetainsQuietWindowAndNativeIncrements(t *testing.T) {
	t.Parallel()
	f := alFailureHistoryFixtureForTest()
	h, err := f.read(t)
	if err != nil || h.increments != 5 || !h.thresholdLower.Equal(f.started.Add(30*time.Second)) || !h.lastLower.Equal(f.started.Add(65*time.Second)) || !h.lastUpper.Equal(f.started.Add(70*time.Second+125*time.Millisecond)) || !h.scrapedThrough.Equal(f.started.Add(420*time.Second)) {
		t.Fatalf("native failure history = %+v, %v", h, err)
	}
	for name, mutate := range map[string]func(*alFailureHistoryFixture){
		"follower also increments": func(f *alFailureHistoryFixture) {
			other := f.counters[0]
			other.Metric = maps.Clone(other.Metric)
			other.Metric["pod"], other.Metric["instance"] = "follower", f.up[1].Metric["instance"]
			f.counters = append(f.counters, other)
		},
		"missing leader counter":  func(f *alFailureHistoryFixture) { f.counters = nil },
		"missing follower scrape": func(f *alFailureHistoryFixture) { f.up = f.up[:1] },
		"duplicate counter":       func(f *alFailureHistoryFixture) { f.counters = append(f.counters, f.counters[0]) },
		"different family":        func(f *alFailureHistoryFixture) { f.counters[0].Metric["family"] = "migration" },
		"different stage":         func(f *alFailureHistoryFixture) { f.counters[0].Metric["stage"] = "apply" },
		"wrong instance":          func(f *alFailureHistoryFixture) { f.counters[0].Metric["instance"] = "other" },
		"unrelated category":      func(f *alFailureHistoryFixture) { f.counters[0].Metric["category"] = "stale_input" },
		"missing baseline":        func(f *alFailureHistoryFixture) { f.counters[0].Values = f.counters[0].Values[5:] },
		"scrape gap": func(f *alFailureHistoryFixture) {
			v := f.counters[0].Values
			f.counters[0].Values = append(v[:70], v[71:]...)
		},
		"missing latest counter sample": func(f *alFailureHistoryFixture) {
			f.counters[0].Values = f.counters[0].Values[:len(f.counters[0].Values)-1]
		},
		"counter reset": func(f *alFailureHistoryFixture) {
			f.counters[0].Values[80] = alAdmissionHistorySampleForTest(f.started.Add(95*time.Second), "0")
		},
		"failure during baseline": func(f *alFailureHistoryFixture) {
			f.counters[0].Values[20] = alAdmissionHistorySampleForTest(f.started.Add(-205*time.Second), "8")
		},
		"failed follower scrape": func(f *alFailureHistoryFixture) {
			f.up[1].Values[80] = alAdmissionHistorySampleForTest(f.started.Add(95*time.Second), "0")
		},
		"late scrape": func(f *alFailureHistoryFixture) {
			f.durations[0].Values[80] = alAdmissionHistorySampleForTest(f.started.Add(95*time.Second), "5")
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := alFailureHistoryFixtureForTest()
			mutate(&bad)
			if _, err := bad.read(t); err == nil {
				t.Fatal("incomplete or contaminated proof accepted")
			}
		})
	}
}

func TestAlFailuresDeliveryDoesNotDateThresholdByFourthFailure(t *testing.T) {
	t.Parallel()
	f := alFailureHistoryFixtureForTest()
	h, err := f.read(t)
	if err != nil {
		t.Fatal(err)
	}
	d := alDelivery{StartsAt: f.started.Add(35 * time.Second), ReceivedAt: h.thresholdLower.Add(alDetectionSlack)}
	if !alFailuresDelivered(d, h, f.started) {
		t.Fatal("on-time delivery refused")
	}
	d.ReceivedAt = d.ReceivedAt.Add(time.Nanosecond)
	if alFailuresDelivered(d, h, f.started) {
		t.Fatal("late delivery accepted")
	}
	d.ReceivedAt = h.thresholdLower.Add(alDetectionSlack)
	d.StartsAt = f.started.Add(time.Second)
	if alFailuresDelivered(d, h, f.started) {
		t.Fatal("alert before threshold accepted")
	}
	d.StartsAt = f.started.Add(35 * time.Second)
	h.increments = 3
	if alFailuresDelivered(d, h, f.started) {
		t.Fatal("insufficient real failure count accepted")
	}
}

func TestAlFailuresResolutionAllowsCountToFallBelowThreshold(t *testing.T) {
	t.Parallel()
	f := alFailureHistoryFixtureForTest()
	h, err := f.read(t)
	if err != nil {
		t.Fatal(err)
	}
	firing := alDelivery{StartsAt: f.started.Add(35 * time.Second)}
	resolved := alDelivery{StartsAt: firing.StartsAt, EndsAt: f.started.Add(325 * time.Second), ReceivedAt: f.started.Add(330 * time.Second)}
	if !alFailuresCleared(firing, resolved, h) {
		t.Fatal("resolution after oldest failures age out refused")
	}
	resolved.ReceivedAt = h.lastLower.Add(alFailuresWindow + alDetectionSlack + time.Nanosecond)
	if alFailuresCleared(firing, resolved, h) {
		t.Fatal("late resolution accepted")
	}
	resolved.ReceivedAt = f.started.Add(330 * time.Second)
	resolved.StartsAt = resolved.StartsAt.Add(time.Second)
	if alFailuresCleared(firing, resolved, h) {
		t.Fatal("another incident accepted")
	}
}

func TestAlFailuresBaselineWaitDoesNotHideBrokenEvidence(t *testing.T) {
	t.Parallel()
	f := alFailureHistoryFixtureForTest()
	// The five increments are historical when preparing a new fault. They
	// need to age out, and cannot be counted toward that later incident.
	f.started = f.started.Add(100 * time.Second)
	if _, err := f.read(t); !errors.Is(err, errAlFailuresBaseline) {
		t.Fatalf("historical failures were not distinguished from evidence failure: %v", err)
	}
	for _, fault := range []string{"missing counter", "missing follower", "reset", "failed scrape"} {
		f := alFailureHistoryFixtureForTest()
		switch fault {
		case "missing counter":
			f.counters = nil
		case "missing follower":
			f.up = f.up[:1]
		case "reset":
			f.counters[0].Values[80] = alAdmissionHistorySampleForTest(f.started.Add(95*time.Second), "0")
		case "failed scrape":
			f.up[0].Values[80] = alAdmissionHistorySampleForTest(f.started.Add(95*time.Second), "0")
		}
		if _, err := f.read(t); err == nil || errors.Is(err, errAlFailuresBaseline) {
			t.Fatalf("%s was accepted or treated as a retryable baseline: %v", fault, err)
		}
	}
}

func TestAlFailuresSupportsBothFamiliesWithHealthyFollowers(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"schema", "migration"} {
		f := alFailureHistoryFixtureForTest()
		f.counters[0].Metric["family"] = family
		follower := f.counters[0]
		follower.Metric = maps.Clone(follower.Metric)
		follower.Metric["pod"], follower.Metric["instance"] = "follower", f.up[1].Metric["instance"]
		follower.Values = append(follower.Values[:0:0], f.up[1].Values...)
		// A follower may have a previously instantiated, unchanged counter.
		f.counters = append(f.counters, follower)
		h, err := alReadFailuresHistory(f.counters, f.up, f.durations, []string{"leader", "follower"}, "leader", family, f.started, f.queried)
		if err != nil || h.increments != 5 {
			t.Fatalf("%s healthy follower proof refused: %+v, %v", family, h, err)
		}
	}
}

func TestAlFailuresRequireFourActualResultsInOneWindow(t *testing.T) {
	t.Parallel()
	start := time.Unix(1800000000, 0).UTC()
	for _, row := range []struct {
		name    string
		seconds []int
		want    bool
	}{
		{"three cannot pass by extrapolation", []int{1, 20, 35}, false},
		{"four in any input order", []int{50, 1, 35, 20}, true},
		{"left boundary is outside the window", []int{0, 100, 200, 300}, false},
		{"inside the boundary", []int{1, 100, 200, 300}, true},
		{"older failure cannot fill the count", []int{0, 310, 320, 330}, false},
		{"a later full window is sufficient", []int{0, 310, 320, 330, 340}, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			var times []time.Time
			for _, second := range row.seconds {
				times = append(times, start.Add(time.Duration(second)*time.Second))
			}
			if got := alFailuresInWindow(times); got != row.want {
				t.Fatalf("rolling-window count = %t, want %t", got, row.want)
			}
		})
	}
	if alFailuresInWindow([]time.Time{{}, start, start, start}) {
		t.Fatal("undated result accepted")
	}
}

func TestAlFailuresRejectsChangedConsumersBetweenPolls(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"schema", "migration"} {
		initial := negativeFixtureState(t, family, ptahv1.ApplyPolicyNever)
		if err := alFailuresUnchanged(initial, initial.DeepCopyObject().(client.Object)); err != nil {
			t.Fatal(err)
		}
		for _, mutation := range []string{"replacement", "generation", "name", "deletion", "spec", "Apply claim"} {
			current := initial.DeepCopyObject().(client.Object)
			switch mutation {
			case "replacement":
				current.SetUID("replacement")
			case "generation":
				current.SetGeneration(current.GetGeneration() + 1)
			case "name":
				current.SetName("another")
			case "deletion":
				at := metav1.NewTime(time.Unix(1800000001, 0))
				current.SetDeletionTimestamp(&at)
			case "spec":
				switch v := current.(type) {
				case *ptahv1.PtahSchema:
					v.Spec.Suspend = true
				case *ptahv1.PtahMigration:
					v.Spec.Suspend = true
				}
			case "Apply claim":
				switch v := current.(type) {
				case *ptahv1.PtahSchema:
					v.Status.ActiveOperation = &ptahv1.ActiveOperationStatus{Type: ptahv1.OperationApply}
				case *ptahv1.PtahMigration:
					v.Status.ActiveOperation = &ptahv1.MigrationOperationStatus{Type: ptahv1.MigrationOperationApply}
				}
			}
			if err := alFailuresUnchanged(initial, current); err == nil {
				t.Fatalf("%s %s accepted", family, mutation)
			}
		}
	}
}

func TestAlFailuresClosedWorkloadsRequireEveryOriginalResult(t *testing.T) {
	t.Parallel()
	fixture := func() (map[types.UID]alFailureResult, []watchEvent[*batchv1.Job], []watchEvent[*corev1.Pod]) {
		results := map[types.UID]alFailureResult{}
		var jobs []watchEvent[*batchv1.Job]
		var pods []watchEvent[*corev1.Pod]
		for i := 0; i < 4; i++ {
			jobUID, podUID := types.UID(fmt.Sprintf("job-%d", i)), types.UID(fmt.Sprintf("pod-%d", i))
			results[jobUID] = alFailureResult{pod: podUID, failed: true}
			jobs = append(jobs, watchEvent[*batchv1.Job]{Object: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{UID: jobUID, Labels: map[string]string{labelOperation: "resolve"}, OwnerReferences: []metav1.OwnerReference{{UID: "consumer", Controller: ptr.To(true)}}}}})
			pods = append(pods, watchEvent[*corev1.Pod]{Object: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: podUID, OwnerReferences: []metav1.OwnerReference{{UID: jobUID, Controller: ptr.To(true)}}}}})
		}
		return results, jobs, pods
	}
	r, j, p := fixture()
	if err := alFailuresWorkloads("consumer", r, j, p); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"empty results", "missing Job history", "missing Pod history", "unretained Resolve", "Apply", "changed owner", "changed operation", "replaced Pod", "extra Pod"} {
		r, j, p := fixture()
		switch mutation {
		case "empty results":
			r = nil
		case "missing Job history":
			j = j[1:]
		case "missing Pod history":
			p = p[1:]
		case "unretained Resolve":
			extra := j[0].Object.DeepCopy()
			extra.UID = "unretained"
			j = append(j, watchEvent[*batchv1.Job]{Object: extra})
		case "Apply":
			extra := j[0].Object.DeepCopy()
			extra.UID = "apply"
			extra.Labels[labelOperation] = "apply"
			j = append(j, watchEvent[*batchv1.Job]{Object: extra})
		case "changed owner":
			j[0].Object.OwnerReferences[0].UID = "other"
		case "changed operation":
			j[0].Object.Labels[labelOperation] = "observe"
		case "replaced Pod":
			p[0].Object.UID = "replacement"
		case "extra Pod":
			extra := p[0].Object.DeepCopy()
			extra.UID = "extra"
			p = append(p, watchEvent[*corev1.Pod]{Object: extra})
		}
		if err := alFailuresWorkloads("consumer", r, j, p); err == nil {
			t.Fatalf("%s accepted", mutation)
		}
	}
}

func TestAlFailuresBaselineWaitValidatesTheWholeHistory(t *testing.T) {
	t.Parallel()
	for _, partial := range []bool{false, true} {
		f := alFailureHistoryFixtureForTest()
		f.started = f.started.Add(100 * time.Second)
		if partial {
			f.counters[0].Values = f.counters[0].Values[70:]
		}
		if _, err := f.read(t); !errors.Is(err, errAlFailuresBaseline) {
			t.Fatalf("healthy unready baseline (partial=%t) must remain retryable: %v", partial, err)
		}
	}
	for _, fault := range []string{"later reset", "later duplicate", "missing leader after another category", "incomplete series before reset"} {
		t.Run(fault, func(t *testing.T) {
			f := alFailureHistoryFixtureForTest()
			// A prior deliberate failure makes the baseline unready. It must
			// not mask corrupt evidence later in this or another series.
			f.started = f.started.Add(100 * time.Second)
			switch fault {
			case "later reset":
				f.counters[0].Values[80] = alAdmissionHistorySampleForTest(f.started.Add(-5*time.Second), "0")
			case "later duplicate":
				f.counters = append(f.counters, f.counters[0])
			case "missing leader after another category":
				f.counters[0].Metric["category"] = "stale_input"
			case "incomplete series before reset":
				f.counters[0].Values[80] = alAdmissionHistorySampleForTest(f.started.Add(-5*time.Second), "0")
				f.counters[0].Values = f.counters[0].Values[70:]
			}
			if _, err := f.read(t); err == nil || errors.Is(err, errAlFailuresBaseline) {
				t.Fatalf("%s was accepted or hidden by a retryable baseline: %v", fault, err)
			}
		})
	}
}

func TestAlFailuresAcceptsItsDeclaredThirtyMinuteHistory(t *testing.T) {
	t.Parallel()
	f := alFailureHistoryFixtureForTest()
	prepend := func(series *alAdmissionSeries, value string) {
		prefix := alAdmissionSeries{}
		for second := -1300; second < -305; second += 5 {
			prefix.Values = append(prefix.Values, alAdmissionHistorySampleForTest(f.started.Add(time.Duration(second)*time.Second), value))
		}
		series.Values = append(prefix.Values, series.Values...)
	}
	for i := range f.up {
		prepend(&f.up[i], "1")
		prepend(&f.durations[i], "0.125")
	}
	prepend(&f.counters[0], "7")
	h, err := f.read(t)
	if err != nil || h.increments != 5 {
		t.Fatalf("valid thirty-minute query refused: %+v, %v", h, err)
	}
	// The wider query must retain an exact lower bound of its own.
	f.counters[0].Values[0] = alAdmissionHistorySampleForTest(f.queried.Add(-alFailuresHistoryWindow), "7")
	if _, err := f.read(t); err == nil {
		t.Fatal("sample outside the declared query window accepted")
	}
}
