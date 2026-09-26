package main

import "k8s.io/client-go/rest"

const (
	// boundedSweepQueriesPerSecond is the request rate a hook's in-cluster
	// client may sustain. client-go's default of 5 requests per second with a
	// burst of 10 is sized for a long-lived controller sharing the API server;
	// a hook is a one-shot process whose verifications read every guard it owns
	// several times over, inside a Job deadline.
	boundedSweepQueriesPerSecond float32 = 100
	boundedSweepBurst                    = 200
)

// boundedSweepRESTConfig returns config with the hook's request rate applied.
func boundedSweepRESTConfig(config *rest.Config) *rest.Config {
	config.QPS = boundedSweepQueriesPerSecond
	config.Burst = boundedSweepBurst
	return config
}
