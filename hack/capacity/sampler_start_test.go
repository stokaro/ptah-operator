package main

import (
	"testing"
	"time"
)

func TestSamplerStartsWithACompleteAPIBaseline(t *testing.T) {
	for _, mode := range []string{"complete", "missing counters", "failed collection"} {
		t.Run(mode, func(t *testing.T) {
			metrics := completeProcessMetrics
			if mode != "missing counters" {
				metrics += "# TYPE rest_client_requests_total counter\nrest_client_requests_total{code=\"200\"} 40\n"
			}
			failPath := ""
			if mode == "failed collection" {
				failPath = "/api/v1/namespaces/work/pods"
			}
			s := measurementFixture(t, failPath, metrics)
			s.every = time.Hour
			finish := make(chan struct{})
			done, initialErr := s.start(t.Context(), finish)
			workloadStart := time.Now().UTC()
			rows, _ := s.snapshot()
			close(finish)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("sampler did not finish")
			}
			if len(rows) == 0 {
				t.Fatal("caller resumed before an initial sample was retained")
			}
			if mode == "complete" {
				_, finished, valid := requestReadBounds(rows[0])
				if initialErr != nil || !valid || finished.After(workloadStart) || len(rows[0].Managers) != 2 {
					t.Fatalf("workload start is not preceded by both complete API readings: %v", initialErr)
				}
			} else if initialErr == nil {
				t.Fatalf("%s admitted workload creation without its baseline", mode)
			}
		})
	}
}
