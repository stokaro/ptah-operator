package main

import "k8s.io/client-go/rest"

const (
	// boundedSweepQueriesPerSecond is the request rate a hook's in-cluster
	// client may sustain. client-go's default of 5 requests per second with a
	// burst of 10 is sized for a long-lived controller sharing the API server;
	// a hook is a one-shot process whose verifications are bounded sweeps, a
	// few dozen reads of the guards it owns, repeated under a per-attempt
	// budget of seconds. At the default rate one sweep alone outlasted that
	// budget, and the barrier reported the rate limiter's wait as contract
	// drift. The direct per-API-server clients already run at this rate.
	boundedSweepQueriesPerSecond float32 = 100
	boundedSweepBurst                    = 200
)

// boundedSweepRESTConfig returns config with the hook's request rate applied.
func boundedSweepRESTConfig(config *rest.Config) *rest.Config {
	config.QPS = boundedSweepQueriesPerSecond
	config.Burst = boundedSweepBurst
	return config
}
