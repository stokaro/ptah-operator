package main

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	crmanager "sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/stokaro/ptah-operator/internal/resultcleanup"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultservice"
)

const testManagerUsername = "system:serviceaccount:ptah-system:ptah-operator"

type recordingHost struct {
	client    client.Client
	apiReader client.Reader
	refuse    error
	added     []crmanager.Runnable
}

func newRecordingHost() *recordingHost {
	// Two clients, so a policy built on the cached client reads as the wrong one.
	return &recordingHost{client: fake.NewClientBuilder().Build(), apiReader: fake.NewClientBuilder().Build()}
}

func (h *recordingHost) GetClient() client.Client    { return h.client }
func (h *recordingHost) GetAPIReader() client.Reader { return h.apiReader }

func (h *recordingHost) Add(runnable crmanager.Runnable) error {
	if h.refuse != nil {
		return h.refuse
	}
	h.added = append(h.added, runnable)
	return nil
}

// A record outlives the delivery setting that wrote it. Before #826 a manager
// with delivery off ran no collector and handed admission no policy, so records
// stayed for good and a namespace holding one stayed Terminating.
func TestTheManagerRunsRetentionWhateverDeliverySays(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		results *resultservice.Service
	}{
		{name: "delivery off"},
		{name: "delivery on", results: &resultservice.Service{}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			host := newRecordingHost()
			validator, err := newWriteValidator(host, prometheus.NewRegistry(), nil, testManagerUsername, row.results)
			if err != nil {
				t.Fatal(err)
			}
			if len(host.added) != 1 {
				t.Fatalf("added %d runnables to the manager, want the result collector alone", len(host.added))
			}
			if _, ok := host.added[0].(*resultcleanup.Collector); !ok {
				t.Fatalf("added a %T to the manager, want a *resultcleanup.Collector", host.added[0])
			}
			policy := validator.ResultCleanup
			if policy == nil {
				t.Fatal("admission has no retention policy, so it refuses every deletion of a record")
			}
			if policy.Window != resultretention.MinimumWindow {
				t.Fatalf("admission judges deletions by a %v window, want %v", policy.Window, resultretention.MinimumWindow)
			}
			if policy.Reader != host.apiReader || policy.Now != nil {
				t.Fatal("admission's retention policy does not read the API directly on the wall clock")
			}
			if validator.Reader != host.apiReader || validator.ManagerUsername != testManagerUsername {
				t.Fatalf("validator reads through %T as %q, want the API reader as %q", validator.Reader, validator.ManagerUsername, testManagerUsername)
			}
			switch {
			case row.results == nil && validator.ResultCredentials != nil:
				t.Fatalf("delivery off hands admission credentials %T", validator.ResultCredentials)
			case row.results != nil && validator.ResultCredentials != row.results:
				t.Fatalf("delivery on hands admission credentials %v, want the result service", validator.ResultCredentials)
			}
		})
	}
}

// main exits on these errors. A manager that went on without the collector
// would be the defect above under another cause.
func TestRetentionSetupFailsWithoutItsCollector(t *testing.T) {
	t.Parallel()
	refused := errors.New("manager already started")
	for _, row := range []struct {
		name  string
		setup func(*recordingHost)
		want  error
	}{
		{name: "the manager refuses the collector", setup: func(h *recordingHost) { h.refuse = refused }, want: refused},
		{name: "no API reader", setup: func(h *recordingHost) { h.apiReader = nil }},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			host := newRecordingHost()
			row.setup(host)
			validator, err := newWriteValidator(host, prometheus.NewRegistry(), nil, testManagerUsername, nil)
			if err == nil || validator != nil {
				t.Fatalf("got validator %v and error %v, want an error alone", validator, err)
			}
			if row.want != nil && !errors.Is(err, row.want) {
				t.Fatalf("error %v does not wrap %v", err, row.want)
			}
			if len(host.added) != 0 {
				t.Fatalf("added %d runnables after a failed setup", len(host.added))
			}
		})
	}
}
