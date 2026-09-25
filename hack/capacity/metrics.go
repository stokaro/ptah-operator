package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"k8s.io/client-go/kubernetes"
)

// scrape is one reading of a Prometheus endpoint.
type scrape map[string]*dto.MetricFamily

// scrapePod reads a Pod's metrics endpoint through the API server's Pod proxy,
// so the measurement needs no port-forward and no route to the Pod network.
func scrapePod(ctx context.Context, clientset kubernetes.Interface, namespace, pod string, port int) (scrape, error) {
	raw, err := clientset.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%d/proxy/metrics", namespace, pod, port)).
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("scrape %s/%s: %w", namespace, pod, err)
	}
	return parseScrape(raw)
}

// scrapeAPIServer reads the API server's own metrics, which is where admission
// latency and priority-and-fairness rejections are counted.
func scrapeAPIServer(ctx context.Context, clientset kubernetes.Interface) (scrape, error) {
	raw, err := clientset.CoreV1().RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("scrape the API server: %w", err)
	}
	return parseScrape(raw)
}

func parseScrape(raw []byte) (scrape, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return families, nil
}

// labelsMatch reports whether a series carries every wanted label value. A
// wanted value that ends in "*" matches as a prefix.
func labelsMatch(metric *dto.Metric, wanted map[string]string) bool {
	for name, want := range wanted {
		found := false
		for _, pair := range metric.GetLabel() {
			if pair.GetName() != name {
				continue
			}
			value := pair.GetValue()
			if prefix, ok := strings.CutSuffix(want, "*"); ok {
				found = strings.HasPrefix(value, prefix)
			} else {
				found = value == want
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// value is the sum of a gauge or counter over the series that match.
func (s scrape) value(name string, wanted map[string]string) (float64, bool) {
	family, ok := s[name]
	if !ok {
		return 0, false
	}
	total, found := 0.0, false
	for _, metric := range family.GetMetric() {
		if !labelsMatch(metric, wanted) {
			continue
		}
		switch {
		case metric.GetGauge() != nil:
			total += metric.GetGauge().GetValue()
		case metric.GetCounter() != nil:
			total += metric.GetCounter().GetValue()
		case metric.GetUntyped() != nil:
			total += metric.GetUntyped().GetValue()
		default:
			continue
		}
		found = true
	}
	return total, found
}

// maxValue is the largest gauge over the series that match, by one label.
func (s scrape) maxBy(name, label string) map[string]float64 {
	out := map[string]float64{}
	family, ok := s[name]
	if !ok {
		return out
	}
	for _, metric := range family.GetMetric() {
		key := ""
		for _, pair := range metric.GetLabel() {
			if pair.GetName() == label {
				key = pair.GetValue()
			}
		}
		out[key] = math.Max(out[key], metric.GetGauge().GetValue())
	}
	return out
}

// histogram is the cumulative buckets of every matching series summed.
type histogram struct {
	count   float64
	sum     float64
	buckets map[float64]float64
}

func (s scrape) histogram(name string, wanted map[string]string) histogram {
	h := histogram{buckets: map[float64]float64{}}
	family, ok := s[name]
	if !ok {
		return h
	}
	for _, metric := range family.GetMetric() {
		if !labelsMatch(metric, wanted) || metric.GetHistogram() == nil {
			continue
		}
		reading := metric.GetHistogram()
		h.count += float64(reading.GetSampleCount())
		h.sum += reading.GetSampleSum()
		for _, bucket := range reading.GetBucket() {
			h.buckets[bucket.GetUpperBound()] += float64(bucket.GetCumulativeCount())
		}
	}
	return h
}

// since is what a histogram gained after an earlier reading of the same
// process. A process that restarted in between starts again from zero, so a
// reading lower than the earlier one is taken whole rather than subtracted.
func (h histogram) since(earlier histogram) histogram {
	if h.count < earlier.count {
		return h
	}
	out := histogram{count: h.count - earlier.count, sum: h.sum - earlier.sum, buckets: map[float64]float64{}}
	for bound, cumulative := range h.buckets {
		out.buckets[bound] = cumulative - earlier.buckets[bound]
	}
	return out
}

func (h histogram) add(other histogram) histogram {
	out := histogram{count: h.count + other.count, sum: h.sum + other.sum, buckets: map[float64]float64{}}
	for bound, value := range h.buckets {
		out.buckets[bound] += value
	}
	for bound, value := range other.buckets {
		out.buckets[bound] += value
	}
	return out
}

// quantile is the upper bound of the bucket that holds the q-th observation:
// a histogram knows no finer than its buckets, so the report says "at most"
// rather than interpolating a figure the data does not hold.
func (h histogram) quantile(q float64) (float64, bool) {
	if h.count <= 0 {
		return 0, false
	}
	bounds := make([]float64, 0, len(h.buckets))
	for bound := range h.buckets {
		bounds = append(bounds, bound)
	}
	sort.Float64s(bounds)
	target := q * h.count
	for _, bound := range bounds {
		if h.buckets[bound] >= target {
			return bound, true
		}
	}
	return math.Inf(1), true
}

// counterDelta is how much a counter grew between two readings of one process,
// with the same rule as histogram.since for a restart in between.
func counterDelta(later, earlier float64) float64 {
	if later < earlier {
		return later
	}
	return later - earlier
}
