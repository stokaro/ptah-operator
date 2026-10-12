package telemetry

import (
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestQueueLatencyKeepsClassicObservationsWithoutNativeResets(t *testing.T) {
	registry := prometheus.NewRegistry()
	// The dependency registers this descriptor before the manager starts.
	legacy := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: "workqueue", Name: "queue_duration_seconds", Help: "How long in seconds an item stays in workqueue before being requested",
		Buckets:                     prometheus.ExponentialBuckets(10e-9, 10, 12),
		NativeHistogramBucketFactor: 1.1, NativeHistogramMaxBucketNumber: 100,
		NativeHistogramMinResetDuration: time.Hour,
	}, []string{"name", "controller"})
	registry.MustRegister(legacy)
	metrics := NewQueueMetrics(registry)
	for _, name := range []string{"schema", "migration"} {
		observer := metrics.NewLatencyMetric(name)
		for round := range 2 {
			// Populate more than the native bucket budget across the same
			// range that triggers its reset, without waiting for an hour.
			for i := range 4000 {
				observer.Observe(math.Pow(10, -8+11*float64(i)/3999))
			}
			metric := &dto.Metric{}
			if err := metrics.latency.WithLabelValues(name, name).(prometheus.Metric).Write(metric); err != nil {
				t.Fatal(err)
			}
			h := metric.GetHistogram()
			if h.GetSampleCount() != uint64((round+1)*4000) || len(h.Bucket) != 12 || h.Schema != nil {
				t.Fatalf("latency is not a bounded, non-resetting classic histogram: %v", h)
			}
		}
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, family := range families {
		if family.GetName() != "workqueue_queue_duration_seconds" {
			continue
		}
		found++
		if len(family.Metric) != 2 {
			t.Fatalf("expected both controller queues, got %d", len(family.Metric))
		}
		for _, metric := range family.Metric {
			if metric.GetHistogram().GetSampleCount() != 8000 || metric.GetHistogram().Schema != nil {
				t.Fatal("the registry still exports the resetting latency collector")
			}
		}
	}
	if found != 1 {
		t.Fatalf("found %d latency metric families, want one", found)
	}
}

func TestQueueProviderReusesOtherCollectors(t *testing.T) {
	registry := prometheus.NewRegistry()
	adds := prometheus.NewCounterVec(prometheus.CounterOpts{Subsystem: "workqueue", Name: "adds_total", Help: "Total number of adds handled by workqueue"}, []string{"name", "controller"})
	registry.MustRegister(adds)
	adds.WithLabelValues("schema", "schema").Add(41)
	metrics := NewQueueMetrics(registry)
	metrics.NewAddsMetric("schema").Inc()
	if metrics.adds != adds {
		t.Fatal("replaced an unrelated collector")
	}
	metric := &dto.Metric{}
	if err := adds.WithLabelValues("schema", "schema").Write(metric); err != nil || metric.GetCounter().GetValue() != 42 {
		t.Fatalf("existing counter was not preserved: %v %v", metric, err)
	}
}
