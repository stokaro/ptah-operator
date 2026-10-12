package controller

import (
	"sync"

	"github.com/stokaro/ptah-operator/internal/telemetry"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	controlleroptions "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var controllerQueueMetrics = sync.OnceValue(func() *telemetry.QueueMetrics {
	return telemetry.NewQueueMetrics(metrics.Registry)
})

// Keep the pinned controller-runtime priority queue and its supplied rate
// limiter. Only its public metrics provider differs, so queue observations do
// not disappear when the native histogram reaches its hourly reset.
func queueOptions() controlleroptions.Options {
	return controlleroptions.Options{
		NewQueue: func(name string, limiter workqueue.TypedRateLimiter[reconcile.Request]) workqueue.TypedRateLimitingInterface[reconcile.Request] {
			provider := controllerQueueMetrics()
			return priorityqueue.New(name, func(o *priorityqueue.Opts[reconcile.Request]) {
				o.RateLimiter = limiter
				o.MetricProvider = provider
				o.Log = ctrl.Log.WithValues("controller", name)
			})
		},
	}
}
