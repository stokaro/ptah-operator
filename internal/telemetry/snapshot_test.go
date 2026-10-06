package telemetry_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

type snapshotReader struct {
	client.Reader
	list func(context.Context, client.ObjectList) error
}

func (r snapshotReader) List(ctx context.Context, list client.ObjectList, _ ...client.ListOption) error {
	return r.list(ctx, list)
}

func snapshotMetrics(t *testing.T, registry *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]*dto.MetricFamily{}
	for _, family := range families {
		result[family.GetName()] = family
	}
	return result
}

func startSnapshot(t *testing.T, collector *telemetry.SnapshotCollector, registry *prometheus.Registry) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- collector.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("telemetry leadership did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if snapshotMetrics(t, registry)["ptah_operator_unresolved_view_synced"].GetMetric()[0].GetGauge().GetValue() == 1 {
			return cancel
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("telemetry leader did not publish its API reading")
	return cancel
}

func assertSnapshotMissing(t *testing.T, metrics map[string]*dto.MetricFamily, failures float64) {
	t.Helper()
	if len(metrics) != 2 || metrics["ptah_operator_unresolved_view_synced"].GetMetric()[0].GetGauge().GetValue() != 0 ||
		metrics["ptah_operator_unresolved_view_read_failures_total"].GetMetric()[0].GetCounter().GetValue() != failures {
		t.Fatalf("failed or inactive snapshot exposed state or the wrong failure count: %v", metrics)
	}
}

func TestSnapshotRejectsPartialReadsAndRecoversWithoutOldState(t *testing.T) {
	t.Parallel()
	for _, failedKind := range []string{"*v1alpha1.PtahSchemaList", "*v1alpha1.PtahMigrationList", "*v1alpha1.PtahSchemaPlanList", "*v1alpha1.PtahMigrationPlanList"} {
		t.Run(failedKind, func(t *testing.T) {
			fail, populated := false, true
			calls := map[string]int{}
			var callsMu sync.Mutex
			reader := snapshotReader{list: func(ctx context.Context, list client.ObjectList) error {
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 3*time.Second {
					return errors.New("API reading has no shared three-second bound")
				}
				kind := fmt.Sprintf("%T", list)
				callsMu.Lock()
				calls[kind]++
				callsMu.Unlock()
				if fail && kind == failedKind {
					return errors.New("API read refused")
				}
				if populated {
					switch value := list.(type) {
					case *operatorv1alpha1.PtahSchemaList:
						value.Items = []operatorv1alpha1.PtahSchema{{Status: operatorv1alpha1.PtahSchemaStatus{Phase: "Blocked"}}}
					case *operatorv1alpha1.PtahSchemaPlanList:
						value.Items = []operatorv1alpha1.PtahSchemaPlan{{Spec: operatorv1alpha1.PtahSchemaPlanSpec{Size: 4096}}}
					}
				}
				return nil
			}}
			registry := prometheus.NewRegistry()
			collector := telemetry.NewSnapshotCollector(registry, reader, nil)
			startSnapshot(t, collector, registry)
			clear(calls)
			metrics := snapshotMetrics(t, registry)
			if len(calls) != 4 || metrics["ptah_operator_stored_plan_bytes"].GetMetric()[0].GetGauge().GetValue() != 4096 {
				t.Fatalf("incomplete initial snapshot: calls=%v metrics=%v", calls, metrics)
			}
			for _, count := range calls {
				if count != 1 {
					t.Fatalf("a kind was read more than once per scrape: %v", calls)
				}
			}
			fail = true
			assertSnapshotMissing(t, snapshotMetrics(t, registry), 1)
			assertSnapshotMissing(t, snapshotMetrics(t, registry), 2)
			fail, populated = false, false
			metrics = snapshotMetrics(t, registry)
			if metrics["ptah_operator_unresolved_view_synced"].GetMetric()[0].GetGauge().GetValue() != 1 ||
				metrics["ptah_operator_stored_plan_bytes"].GetMetric()[0].GetGauge().GetValue() != 0 ||
				metrics["ptah_operator_unresolved_view_read_failures_total"].GetMetric()[0].GetCounter().GetValue() != 2 {
				t.Fatalf("recovery retained old data or lost the failure counter: %v", metrics)
			}
		})
	}
}

func TestSnapshotFollowerAndCanceledLeaderPublishNoState(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	reader := snapshotReader{list: func(context.Context, client.ObjectList) error { calls.Add(1); return nil }}
	registry := prometheus.NewRegistry()
	collector := telemetry.NewSnapshotCollector(registry, reader, nil)
	if !collector.NeedLeaderElection() {
		t.Fatal("a follower would hide a lost leader scrape")
	}
	assertSnapshotMissing(t, snapshotMetrics(t, registry), 0)
	if calls.Load() != 0 {
		t.Fatal("the follower queried the API")
	}
	cancel := startSnapshot(t, collector, registry)
	cancel()
	before := calls.Load()
	assertSnapshotMissing(t, snapshotMetrics(t, registry), 0)
	if calls.Load() != before {
		t.Fatal("the canceled leader queried the API")
	}
}

func TestSnapshotDoesNotAddIndependentAPIReadLatencies(t *testing.T) {
	t.Parallel()
	var slow atomic.Bool
	reader := snapshotReader{list: func(ctx context.Context, list client.ObjectList) error {
		if slow.Load() {
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
		if plans, ok := list.(*operatorv1alpha1.PtahSchemaPlanList); ok {
			plans.Items = []operatorv1alpha1.PtahSchemaPlan{{Spec: operatorv1alpha1.PtahSchemaPlanSpec{Size: 4096}}}
		}
		return nil
	}}
	registry := prometheus.NewRegistry()
	collector := telemetry.NewSnapshotCollector(registry, reader, nil)
	startSnapshot(t, collector, registry)
	slow.Store(true)
	metrics := snapshotMetrics(t, registry)
	bytes := metrics["ptah_operator_stored_plan_bytes"]
	if bytes == nil || len(bytes.GetMetric()) != 1 || bytes.GetMetric()[0].GetGauge().GetValue() != 4096 {
		t.Fatal("independent one-second API reads lost the plan-store reading within the shared three-second budget")
	}
	if metrics["ptah_operator_unresolved_view_synced"].GetMetric()[0].GetGauge().GetValue() != 1 ||
		metrics["ptah_operator_unresolved_view_read_failures_total"].GetMetric()[0].GetCounter().GetValue() != 0 {
		t.Fatal("bounded successful API reads were reported as a failed snapshot")
	}
}

func TestSnapshotHungReadExpiresAndLeadershipLossCancelsIt(t *testing.T) {
	t.Parallel()
	for _, loseLeadership := range []bool{false, true} {
		t.Run(fmt.Sprint(loseLeadership), func(t *testing.T) {
			var block atomic.Bool
			entered := make(chan struct{}, 1)
			reader := snapshotReader{list: func(ctx context.Context, _ client.ObjectList) error {
				if block.Load() {
					select {
					case entered <- struct{}{}:
					default:
					}
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}}
			registry := prometheus.NewRegistry()
			collector := telemetry.NewSnapshotCollector(registry, reader, nil)
			cancel := startSnapshot(t, collector, registry)
			block.Store(true)
			done := make(chan map[string]*dto.MetricFamily, 1)
			go func() { done <- snapshotMetrics(t, registry) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("API read never started")
			}
			bound, failures := 5*time.Second, 1.0
			if loseLeadership {
				cancel()
				bound, failures = time.Second, 0
			}
			select {
			case metrics := <-done:
				assertSnapshotMissing(t, metrics, failures)
			case <-time.After(bound):
				t.Fatal("API read ignored its timeout or leadership cancellation")
			}
		})
	}
}
