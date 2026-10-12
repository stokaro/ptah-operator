package controller

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestControllerQueuePreservesPriorityDeduplicationAndRateLimiter(t *testing.T) {
	limiter := workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](time.Millisecond, time.Second)
	q := queueOptions().NewQueue("queue-metrics-test", limiter)
	t.Cleanup(q.ShutDown)
	priority, ok := q.(priorityqueue.PriorityQueue[reconcile.Request])
	if !ok {
		t.Fatal("metrics configuration replaced the priority queue")
	}
	low := reconcile.Request{NamespacedName: types.NamespacedName{Name: "low"}}
	high := reconcile.Request{NamespacedName: types.NamespacedName{Name: "high"}}
	one, ten := 1, 10
	priority.AddWithOpts(priorityqueue.AddOpts{Priority: &one}, low)
	priority.AddWithOpts(priorityqueue.AddOpts{Priority: &ten}, high, high)
	waitQueueLength(t, q, 2)
	item, rank, stopped := priority.GetWithPriority()
	if stopped || item != high || rank != ten {
		t.Fatalf("priority was lost: item=%v priority=%d stopped=%v", item, rank, stopped)
	}
	q.Done(item)
	item, rank, stopped = priority.GetWithPriority()
	if stopped || item != low || rank != one {
		t.Fatalf("remaining item changed: item=%v priority=%d stopped=%v", item, rank, stopped)
	}
	q.Done(item)
	if q.Len() != 0 {
		t.Fatal("duplicate enqueue left another item")
	}
	q.AddRateLimited(low)
	waitQueueLength(t, q, 1)
	if limiter.NumRequeues(low) != 1 {
		t.Fatal("the supplied rate limiter was not used")
	}
	item, stopped = q.Get()
	if stopped || item != low {
		t.Fatal("rate-limited work did not return")
	}
	q.Done(item)
	q.Forget(low)
	if limiter.NumRequeues(low) != 0 {
		t.Fatal("Forget did not reach the supplied rate limiter")
	}
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, family := range families {
		if family.GetName() != "workqueue_queue_duration_seconds" {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "name" && label.GetValue() == "queue-metrics-test" {
					found = true
					if metric.GetHistogram().GetSampleCount() != 3 || metric.GetHistogram().Schema != nil {
						t.Fatal("actual queue did not use persistent classic latency observations")
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("actual queue exported no latency observations")
	}
}

func waitQueueLength(t *testing.T, q workqueue.TypedRateLimitingInterface[reconcile.Request], want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for q.Len() != want {
		if time.Now().After(deadline) {
			t.Fatalf("queue length %d did not reach %d", q.Len(), want)
		}
		time.Sleep(time.Millisecond)
	}
}
