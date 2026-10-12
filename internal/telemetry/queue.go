package telemetry

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/util/workqueue"
)

// QueueMetrics keeps queue latency for the lifetime of the manager. The
// controller-runtime provider resets its native histogram after one hour;
// that also resets its classic buckets and loses observations between scrapes.
// Ordinary bounded buckets need no reset. Other collectors reuse the
// controller-runtime registrations; depth uses the public provider's aggregate.
//
// Construct this provider once, before starting any controller queue. Both
// controllers must use it: the old latency collector is replaced in the registry.
type QueueMetrics struct {
	depth, unfinished, longest *prometheus.GaugeVec
	adds, retries              *prometheus.CounterVec
	latency, duration          *prometheus.HistogramVec
}

func NewQueueMetrics(registerer prometheus.Registerer) *QueueMetrics {
	labels := []string{"name", "controller"}
	gauge := func(name, help string, labels []string) *prometheus.GaugeVec {
		return queueCollector(registerer, prometheus.NewGaugeVec(prometheus.GaugeOpts{Subsystem: "workqueue", Name: name, Help: help}, labels))
	}
	counter := func(name, help string) *prometheus.CounterVec {
		return queueCollector(registerer, prometheus.NewCounterVec(prometheus.CounterOpts{Subsystem: "workqueue", Name: name, Help: help}, labels))
	}
	m := &QueueMetrics{
		depth:      gauge("depth", "Current depth of workqueue by workqueue and priority", []string{"name", "controller", "priority"}),
		adds:       counter("adds_total", "Total number of adds handled by workqueue"),
		unfinished: gauge("unfinished_work_seconds", "How many seconds of work has been done that is in progress and hasn't been observed by work_duration. Large values indicate stuck threads. One can deduce the number of stuck threads by observing the rate at which this increases.", labels),
		longest:    gauge("longest_running_processor_seconds", "How many seconds has the longest running processor for workqueue been running.", labels),
		retries:    counter("retries_total", "Total number of items added to the workqueue with a non-zero delay (rate-limited requeues, explicit RequeueAfter or AddAfter calls)"),
		duration: queueCollector(registerer, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Subsystem: "workqueue", Name: "work_duration_seconds", Help: "How long in seconds processing an item from workqueue takes.",
			Buckets: prometheus.ExponentialBuckets(10e-9, 10, 12),
		}, labels)),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Subsystem: "workqueue", Name: "queue_duration_seconds", Help: "How long in seconds an item stays in workqueue before being requested",
			Buckets: prometheus.ExponentialBuckets(10e-9, 10, 12),
		}, labels),
	}
	// Unregister matches descriptors, so this replaces only the library's
	// queue-latency collector. No observations exist before queues start.
	registerer.Unregister(m.latency)
	registerer.MustRegister(m.latency)
	return m
}

func queueCollector[T prometheus.Collector](registerer prometheus.Registerer, collector T) T {
	if err := registerer.Register(collector); err != nil {
		var existing prometheus.AlreadyRegisteredError
		if !errors.As(err, &existing) {
			panic(err)
		}
		return existing.ExistingCollector.(T)
	}
	return collector
}

func (m *QueueMetrics) NewDepthMetric(name string) workqueue.GaugeMetric {
	// The public MetricsProvider interface reports total depth. An empty
	// priority is the library's label for this aggregate.
	return m.depth.WithLabelValues(name, name, "")
}
func (m *QueueMetrics) NewAddsMetric(name string) workqueue.CounterMetric {
	return m.adds.WithLabelValues(name, name)
}
func (m *QueueMetrics) NewLatencyMetric(name string) workqueue.HistogramMetric {
	return m.latency.WithLabelValues(name, name)
}
func (m *QueueMetrics) NewWorkDurationMetric(name string) workqueue.HistogramMetric {
	return m.duration.WithLabelValues(name, name)
}
func (m *QueueMetrics) NewUnfinishedWorkSecondsMetric(name string) workqueue.SettableGaugeMetric {
	return m.unfinished.WithLabelValues(name, name)
}
func (m *QueueMetrics) NewLongestRunningProcessorSecondsMetric(name string) workqueue.SettableGaugeMetric {
	return m.longest.WithLabelValues(name, name)
}
func (m *QueueMetrics) NewRetriesMetric(name string) workqueue.CounterMetric {
	return m.retries.WithLabelValues(name, name)
}
