package crd_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// RBAC loss does not empty an already synchronized informer. The real API
// refusal must reach the telemetry collector even while that cache still
// answers successfully. This test deliberately runs before parallel CRD rows.
func TestTelemetryReadFailureDoesNotPublishWarmCacheState(t *testing.T) {
	plane.Require(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	namespace := newNamespace(t, "telemetry")
	object := schemaBase(namespace)()
	if err := api.Create(ctx, object); err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{ptahv1.GroupVersion.Group}, Resources: []string{"ptahschemas"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{ptahv1.GroupVersion.Group}, Resources: []string{"ptahmigrations", "ptahschemaplans", "ptahmigrationplans"}, Verbs: []string{"list"}},
		},
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: namespace}},
	}
	for _, resource := range []client.Object{role, binding} {
		if err := api.Create(ctx, resource); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := api.Delete(context.Background(), resource); err != nil {
				t.Error(err)
			}
		})
	}
	scheme := runtime.NewScheme()
	if err := ptahv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	config := plane.Impersonate(namespace)
	reader, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	waitRead := func(allowed bool) {
		t.Helper()
		if err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			err := reader.List(ctx, &ptahv1.PtahSchemaList{})
			if err != nil && !apierrors.IsForbidden(err) {
				return false, err
			}
			return (err == nil) == allowed, nil
		}); err != nil {
			t.Fatalf("wait for schema-list permission=%v: %v", allowed, err)
		}
	}
	waitRead(true)
	cached, err := cache.New(config, cache.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cached.GetInformer(ctx, &ptahv1.PtahSchema{}); err != nil {
		t.Fatal(err)
	}
	cacheCtx, stopCache := context.WithCancel(ctx)
	cacheDone := make(chan error, 1)
	go func() { cacheDone <- cached.Start(cacheCtx) }()
	defer func() {
		stopCache()
		if err := <-cacheDone; err != nil {
			t.Error(err)
		}
	}()
	if !cached.WaitForCacheSync(ctx) {
		t.Fatal("schema informer did not synchronize")
	}
	registry := prometheus.NewRegistry()
	collector := telemetry.NewSnapshotCollector(registry, reader, nil)
	leaderCtx, stopLeader := context.WithCancel(ctx)
	leaderDone := make(chan error, 1)
	go func() { leaderDone <- collector.Start(leaderCtx) }()
	defer func() {
		stopLeader()
		if err := <-leaderDone; err != nil {
			t.Error(err)
		}
	}()
	read := func() map[string]*dto.MetricFamily {
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
	if err := wait.PollUntilContextTimeout(ctx, time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
		return read()["ptah_operator_unresolved_view_synced"].GetMetric()[0].GetGauge().GetValue() == 1, nil
	}); err != nil {
		t.Fatal("leader did not publish current API state", err)
	}
	baseline := read()["ptah_operator_unresolved_view_read_failures_total"].GetMetric()[0].GetCounter().GetValue()
	originalRules := role.DeepCopy().Rules
	role.Rules[0].Verbs = []string{"get", "watch"}
	if err := api.Update(ctx, role); err != nil {
		t.Fatal(err)
	}
	waitRead(false)
	stale := &ptahv1.PtahSchemaList{}
	if err := cached.List(ctx, stale, client.InNamespace(namespace)); err != nil || len(stale.Items) != 1 || stale.Items[0].UID != object.GetUID() {
		t.Fatalf("warm-cache control did not retain the original object: count=%d error=%v", len(stale.Items), err)
	}
	metrics := read()
	if len(metrics) != 2 || metrics["ptah_operator_unresolved_view_synced"].GetMetric()[0].GetGauge().GetValue() != 0 ||
		metrics["ptah_operator_unresolved_view_read_failures_total"].GetMetric()[0].GetCounter().GetValue() != baseline+1 {
		t.Fatalf("real RBAC refusal did not suppress all state and count its failed read: %v", metrics)
	}
	role.Rules = originalRules
	if err := api.Update(ctx, role); err != nil {
		t.Fatal(err)
	}
	waitRead(true)
	beforeDeletion := telemetrySchemaPopulation(read())
	if err := api.Delete(ctx, object); err != nil {
		t.Fatal(err)
	}
	metrics = read()
	if metrics["ptah_operator_unresolved_view_synced"].GetMetric()[0].GetGauge().GetValue() != 1 ||
		metrics["ptah_operator_unresolved_view_read_failures_total"].GetMetric()[0].GetCounter().GetValue() != baseline+1 {
		t.Fatal("restored API access did not recover the view while retaining failure history")
	}
	if beforeDeletion < 1 || telemetrySchemaPopulation(metrics) != beforeDeletion-1 {
		t.Fatal("recovered scrape did not account for the deleted resource")
	}
	t.Log("warm cache still held the original UID after API list refusal; telemetry omitted every state gauge, counted the failure, and recovered from a fresh API reading")
}

func telemetrySchemaPopulation(metrics map[string]*dto.MetricFamily) float64 {
	var total float64
	for _, metric := range metrics["ptah_operator_resources"].GetMetric() {
		for _, label := range metric.GetLabel() {
			if label.GetName() == "family" && label.GetValue() == "schema" {
				total += metric.GetGauge().GetValue()
			}
		}
	}
	return total
}
