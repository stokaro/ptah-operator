package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the parser, window reduction, disk writer and readback together:
// an in-memory percentile cannot prove that its source observations survived.
func TestReportRetainsHistogramEvidence(t *testing.T) {
	for _, beyond := range []bool{false, true} {
		name := "finite"
		if beyond {
			name = "beyond-largest-bucket"
		}
		t.Run(name, func(t *testing.T) {
			raw := `# TYPE workqueue_queue_duration_seconds histogram
workqueue_queue_duration_seconds_bucket{le="0.1"} 10
workqueue_queue_duration_seconds_bucket{le="1"} 20
workqueue_queue_duration_seconds_bucket{le="+Inf"} 20
workqueue_queue_duration_seconds_sum 8
workqueue_queue_duration_seconds_count 20
# TYPE apiserver_admission_webhook_admission_duration_seconds histogram
apiserver_admission_webhook_admission_duration_seconds_bucket{name="ptah.operator.ptah.run",le="0.1"} 10
apiserver_admission_webhook_admission_duration_seconds_bucket{name="ptah.operator.ptah.run",le="1"} 20
apiserver_admission_webhook_admission_duration_seconds_bucket{name="ptah.operator.ptah.run",le="+Inf"} 20
apiserver_admission_webhook_admission_duration_seconds_sum{name="ptah.operator.ptah.run"} 8
apiserver_admission_webhook_admission_duration_seconds_count{name="ptah.operator.ptah.run"} 20
`
			if beyond {
				raw = strings.ReplaceAll(raw, "le=\"1\"} 20", "le=\"1\"} 10")
			}
			metrics, err := parseScrape([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			queue := metrics.histogram("workqueue_queue_duration_seconds", nil)
			admission := (histogram{}).onlyPtah(metrics)
			base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			samples := []sample{
				{At: base, Managers: map[string]managerReading{"manager": identifiedManager("manager", managerReading{})}, APIServer: &apiReading{}},
				{At: base.Add(time.Minute), Managers: map[string]managerReading{"manager": identifiedManager("manager", managerReading{QueueWait: queue})}, APIServer: &apiReading{Admission: admission}},
			}
			w := window{Name: name, Start: base, End: base.Add(time.Minute)}
			original := cost(w, samples, nil)
			dir := t.TempDir()
			if err := writeReport(dir, report{FormatVersion: 3, Samples: samples, Scenarios: []scenarioCost{original}}); err != nil {
				t.Fatalf("write measured report: %v", err)
			}
			body, err := os.ReadFile(filepath.Join(dir, "report.json"))
			if err != nil {
				t.Fatal(err)
			}
			var retained report
			if err := json.Unmarshal(body, &retained); err != nil {
				t.Fatal(err)
			}
			if len(retained.Samples) != 2 || len(retained.Scenarios) != 1 {
				t.Fatal("report lost rows")
			}
			restored := cost(w, retained.Samples, nil)
			want := 1.0
			if beyond {
				want = math.Inf(1)
			}
			for _, q := range []quantiles{original.QueueWaitSeconds, original.AdmissionSeconds, restored.QueueWaitSeconds, restored.AdmissionSeconds, retained.Scenarios[0].QueueWaitSeconds, retained.Scenarios[0].AdmissionSeconds} {
				if q.Count != 20 || q.P50 != 0.1 || q.P95 != want {
					t.Errorf("retained or recomputed percentile = %+v, want 20 observations, p50 0.1, p95 %v", q, want)
				}
			}
			for _, h := range []histogram{retained.Samples[1].Managers["manager"].QueueWait, retained.Samples[1].APIServer.Admission} {
				if h.count != 20 || h.sum != 8 || len(h.buckets) != 3 || h.buckets[math.Inf(1)] != 20 {
					t.Errorf("raw histogram lost: %+v", h)
				}
			}
			summary, err := os.ReadFile(filepath.Join(dir, "summary.md"))
			if err != nil {
				t.Fatal(err)
			}
			if beyond && !strings.Contains(string(summary), "> largest bucket") {
				t.Fatal("unbounded latency was hidden")
			}
		})
	}
}

func TestLatencyBoundsPreserveOverflowAndRefuseInvalidNumbers(t *testing.T) {
	for _, value := range []float64{0, 0.1, 1, math.Inf(1)} {
		encoded, err := json.Marshal(latencyBound(value))
		if err != nil {
			t.Fatal(err)
		}
		var decoded latencyBound
		if err := json.Unmarshal(encoded, &decoded); err != nil || float64(decoded) != value {
			t.Fatalf("bound %v became %s: %v", value, encoded, err)
		}
	}
	for _, value := range []float64{math.NaN(), math.Inf(-1)} {
		if _, err := json.Marshal(latencyBound(value)); err == nil {
			t.Errorf("invalid bound %v was serialized", value)
		}
	}
	for _, raw := range []string{`null`, `"NaN"`, `"-Inf"`, `"0.1"`, `1e999`} {
		var value latencyBound
		if err := json.Unmarshal([]byte(raw), &value); err == nil {
			t.Errorf("invalid bound %s was accepted", raw)
		}
	}
	var h histogram
	if err := json.Unmarshal([]byte(`{"count":2,"sum":1,"buckets":[{"upperBound":1,"cumulativeCount":1},{"upperBound":1,"cumulativeCount":2}]}`), &h); err == nil {
		t.Fatal("duplicate bounds silently replaced retained evidence")
	}
}
