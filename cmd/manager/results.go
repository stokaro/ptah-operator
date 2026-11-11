package main

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmanager "sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/stokaro/ptah-operator/internal/controllerwrite"
	"github.com/stokaro/ptah-operator/internal/resultcleanup"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultservice"
)

// retentionHost is the part of the manager that result retention registers with.
type retentionHost interface {
	GetClient() client.Client
	GetAPIReader() client.Reader
	// GetEventRecorderFor writes core/v1 Events, which the chart grants the
	// manager. The events.k8s.io recorder would need a grant it does not hold.
	GetEventRecorderFor(name string) record.EventRecorder
	Add(crmanager.Runnable) error
}

// resultCollectorEventSource names the collector as the source of its Events.
const resultCollectorEventSource = "ptah-result-collector"

// newWriteValidator builds the validator for the manager's own writes and adds
// to host the result collector whose deletions that validator judges.
//
// Both run whatever delivery says, because a record outlives the setting that
// wrote it. The collector scans metadata and the policy reads the API, so
// neither needs the receiver. Only the credentials a create is judged by come
// with delivery, from results, which is nil while delivery is off. main cannot
// register the webhook without the validator, so it cannot skip the collector
// in one mode without also leaving every controller write unjudged.
func newWriteValidator(host retentionHost, registry prometheus.Registerer, jobs controllerwrite.JobBuilder, managerUsername string, results *resultservice.Service) (*controllerwrite.Validator, error) {
	policy := resultcleanup.Policy{Reader: host.GetAPIReader(), Window: resultretention.MinimumWindow}
	collector, err := resultcleanup.New(host.GetClient(), policy, registry, host.GetEventRecorderFor(resultCollectorEventSource))
	if err != nil {
		return nil, fmt.Errorf("configure result cleanup: %w", err)
	}
	if err := host.Add(collector); err != nil {
		return nil, fmt.Errorf("register result cleanup: %w", err)
	}
	validator := &controllerwrite.Validator{Reader: host.GetAPIReader(), Jobs: jobs, ManagerUsername: managerUsername, ResultCleanup: &policy}
	if results != nil {
		// A nil *Service stored in the interface would read as configured.
		validator.ResultCredentials = results
	}
	return validator, nil
}
