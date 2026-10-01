package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// latencyBound preserves an unbounded histogram bucket as "+Inf" in JSON.
// A null or zero would turn a measured overflow into missing or fast latency.
// Other non-finite values remain errors, as they are not latency bounds.
type latencyBound float64

func (b latencyBound) MarshalJSON() ([]byte, error) {
	if math.IsInf(float64(b), 1) {
		return []byte(`"+Inf"`), nil
	}
	return json.Marshal(float64(b))
}

func (b *latencyBound) UnmarshalJSON(data []byte) error {
	if string(data) == `"+Inf"` {
		*b = latencyBound(math.Inf(1))
		return nil
	}
	var value *float64
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value == nil {
		return fmt.Errorf("latency bound cannot be null")
	}
	*b = latencyBound(*value)
	return nil
}

type retainedBucket struct {
	UpperBound      latencyBound `json:"upperBound"`
	CumulativeCount float64      `json:"cumulativeCount"`
}

type retainedHistogram struct {
	Count   float64          `json:"count"`
	Sum     float64          `json:"sum"`
	Buckets []retainedBucket `json:"buckets"`
}

func (h histogram) MarshalJSON() ([]byte, error) {
	bounds := make([]float64, 0, len(h.buckets))
	for bound := range h.buckets {
		bounds = append(bounds, bound)
	}
	sort.Float64s(bounds)
	retained := retainedHistogram{Count: h.count, Sum: h.sum, Buckets: make([]retainedBucket, 0, len(bounds))}
	for _, bound := range bounds {
		retained.Buckets = append(retained.Buckets, retainedBucket{UpperBound: latencyBound(bound), CumulativeCount: h.buckets[bound]})
	}
	return json.Marshal(retained)
}

func (h *histogram) UnmarshalJSON(data []byte) error {
	var retained retainedHistogram
	if err := json.Unmarshal(data, &retained); err != nil {
		return err
	}
	restored := histogram{count: retained.Count, sum: retained.Sum, buckets: map[float64]float64{}}
	for _, bucket := range retained.Buckets {
		bound := float64(bucket.UpperBound)
		if _, exists := restored.buckets[bound]; exists {
			return fmt.Errorf("duplicate histogram bound %g", bound)
		}
		restored.buckets[bound] = bucket.CumulativeCount
	}
	*h = restored
	return nil
}

type retainedQuantiles struct {
	Count int          `json:"count"`
	P50   latencyBound `json:"p50"`
	P95   latencyBound `json:"p95"`
	Max   float64      `json:"max,omitempty"`
}

func (q quantiles) MarshalJSON() ([]byte, error) {
	return json.Marshal(retainedQuantiles{Count: q.Count, P50: latencyBound(q.P50), P95: latencyBound(q.P95), Max: q.Max})
}

func (q *quantiles) UnmarshalJSON(data []byte) error {
	var retained retainedQuantiles
	if err := json.Unmarshal(data, &retained); err != nil {
		return err
	}
	*q = quantiles{Count: retained.Count, P50: float64(retained.P50), P95: float64(retained.P95), Max: retained.Max}
	return nil
}
