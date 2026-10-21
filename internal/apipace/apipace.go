// Package apipace bounds how fast one manager process sends requests to the
// Kubernetes API server.
//
// The 0.2.0 profile allows the managers 7,000 API requests in any 60 seconds,
// summed over both replicas. Without a client-side limit -- controller-runtime
// leaves client-go's off and relies on API Priority and Fairness -- work that a
// stalled Kubernetes component held back arrives at once. After a
// controller-manager leader change the Job controller records no completion
// for minutes and then all of them together, and the managers did those
// minutes of work in one (#657). With the limit that work takes longer instead.
//
// The leader sends most requests. The follower serves the admission reads and
// result deliveries the leader's work causes, which an audited soak measured
// at about a third of the leader's. At the default 80 requests a second and a
// burst of 40, the leader sends at most 4,840 in a minute, about 6,500 with
// the follower. An installation larger than the profile raises both through
// the chart's apiRequests values, and its catch-up is then bounded by what API
// Priority and Fairness admits.
//
// The limit costs no request a long wait: a manager runs one reconcile worker
// per controller and a few background readers, so the bucket never has more
// than a handful of waiters, each waiting for a token or two.
package apipace

import (
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

const (
	// DefaultRequestsPerSecond is the rate one manager process sustains
	// unless the installation sets another.
	DefaultRequestsPerSecond = 80
	// DefaultBurst is how many requests it may send at once after a quiet
	// period.
	DefaultBurst = 40
)

// Config installs one token bucket on config and returns it. Every client
// built from config, or from a copy of it, takes its tokens from that bucket:
// controller-runtime's per-type clients, its cache, leader election, the
// event recorder and a clientset alike. A bucket per client would let their
// rates add up. A rate or burst below one is refused: a bucket that never
// fills stops the manager, and turning the limit off is not offered.
func Config(config *rest.Config, perSecond, burst int) (*rest.Config, error) {
	if perSecond < 1 || burst < 1 {
		return nil, fmt.Errorf("API request rate %d a second with burst %d: both must be at least 1", perSecond, burst)
	}
	config.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(float32(perSecond), burst)
	return config, nil
}
