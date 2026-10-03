package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func twoNamespaceScenarios() *scenarios {
	return &scenarios{in: inputs{namespace: "work-a", namespaces: []string{"work-a", "work-b"}, databaseSecret: "db-%d"}, load: workload{Name: "two-namespaces", Schemas: 10, Migrations: 10}}
}

func TestNamespaceInputsAndEvenFamilyPlacement(t *testing.T) {
	for _, raw := range []string{"", "a,", "a,a", "a,A", "a,has.dot", "a, b, a"} {
		if _, err := parseNamespaces(raw); err == nil {
			t.Errorf("accepted invalid namespace list %q", raw)
		}
	}
	names, err := parseNamespaces("work-a, work-b")
	if err != nil || len(names) != 2 {
		t.Fatal(names, err)
	}
	s := twoNamespaceScenarios()
	counts := map[string]int{}
	secrets := map[string]bool{}
	for i := range 10 {
		for _, object := range []*unstructured.Unstructured{s.schemaObject(i), s.migrationObject(s.migrationName(i), 10+i, "Always", true)} {
			counts[object.GetNamespace()+"/"+object.GetKind()]++
			secret, _, _ := unstructured.NestedString(object.Object, "spec", "target", "urlFrom", "name")
			key := object.GetNamespace() + "/" + secret
			if secrets[key] {
				t.Fatal("database Secret reused across workload slots")
			}
			secrets[key] = true
		}
	}
	if len(counts) != 4 {
		t.Fatalf("family/namespace populations: %v", counts)
	}
	for _, n := range counts {
		if n != 5 {
			t.Fatal("families are not evenly distributed", counts)
		}
	}
}

func TestSamplerReadsBothNamespacesAndRetainsJobIdentities(t *testing.T) {
	labels := map[string]string{"app.kubernetes.io/managed-by": "ptah-operator"}
	objects := []runtime.Object{}
	for i, ns := range []string{"work-a", "work-b"} {
		objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "same-name", Namespace: ns, UID: types.UID(ns + "-pod"), Labels: labels}, Status: corev1.PodStatus{Phase: []corev1.PodPhase{corev1.PodRunning, corev1.PodPending}[i]}})
		objects = append(objects, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "same-name", Namespace: ns, UID: types.UID(ns + "-job"), Labels: labels}})
	}
	s := &sampler{namespace: "work-a", namespaces: []string{"work-a", "work-b"}, clientset: fake.NewClientset(objects...), jobs: map[string]*jobRecord{}}
	var reading sample
	if err := s.readPods(context.Background(), &reading); err != nil {
		t.Fatal(err)
	}
	if reading.PodsRunning != 1 || reading.PodsPending != 1 {
		t.Fatalf("one namespace replaced the other: %+v", reading)
	}
	if err := s.readJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, jobs := s.snapshot()
	if len(jobs) != 2 {
		t.Fatalf("same-name jobs merged: %+v", jobs)
	}
	for _, job := range jobs {
		if job.UID != job.Namespace+"-job" {
			t.Fatal("job lost namespace or UID", job)
		}
	}
	s.clientset.(*fake.Clientset).PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "work-b" {
			return true, nil, errors.New("second namespace unavailable")
		}
		return false, nil, nil
	})
	if err := s.readPods(context.Background(), &sample{}); err == nil {
		t.Fatal("a partial namespace reading became a complete measurement")
	}
}

func TestConvergenceAndPatchesUseDeclaredNamespace(t *testing.T) {
	s := twoNamespaceScenarios()
	s.in.databaseSecret = "db-%d"
	objects := []runtime.Object{}
	for i := range 10 {
		for _, obj := range []*unstructured.Unstructured{s.schemaObject(i), s.migrationObject(s.migrationName(i), 10+i, "Always", true)} {
			obj.Object["status"] = map[string]any{"phase": "InSync"}
			objects = append(objects, obj)
		}
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{schemaResource: "PtahSchemaList", migrationResource: "PtahMigrationList"}, objects...)
	s.dynamic = client
	if ok, err := s.allConverged(context.Background(), time.Time{}, nil); err != nil || !ok {
		t.Fatal("complete fleet refused", ok, err)
	}
	if err := s.patchReference(context.Background(), schemaResource, s.schemaName(1), "desired", "oci://registry/new@sha256:test"); err != nil {
		t.Fatal(err)
	}
	obj, err := client.Resource(schemaResource).Namespace("work-b").Get(context.Background(), s.schemaName(1), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ref, _, _ := unstructured.NestedString(obj.Object, "spec", "desired", "ociRef")
	if !strings.Contains(ref, "registry/new") {
		t.Fatal("patch did not reach second namespace")
	}
	if err := client.Resource(schemaResource).Namespace("work-b").Delete(context.Background(), s.schemaName(1), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	obj.SetNamespace("work-a")
	obj.SetResourceVersion("")
	if _, err := client.Resource(schemaResource).Namespace("work-a").Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.allConverged(context.Background(), time.Time{}, nil); err == nil || ok {
		t.Fatal("right total in the wrong namespace passed convergence")
	}
}

func TestOutageCleansBothNamespacesOnSecondReadFailure(t *testing.T) {
	s := twoNamespaceScenarios()
	s.in.registryIP = "192.0.2.1"
	s.load.Outage = duration{time.Nanosecond}
	client := fake.NewClientset()
	s.clientset = client
	client.PrependReactor("create", "networkpolicies", func(a clienttesting.Action) (bool, runtime.Object, error) {
		policy := a.(clienttesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy)
		if policy.Namespace != a.GetNamespace() {
			return true, nil, fmt.Errorf("policy request crossed namespace")
		}
		policy.UID = types.UID(a.GetNamespace() + "-policy")
		return false, nil, nil
	})
	client.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "work-b" {
			return true, nil, errors.New("second namespace read failed")
		}
		return false, nil, nil
	})
	err := s.outage(context.Background())
	if err == nil || !strings.Contains(err.Error(), "second namespace read failed") {
		t.Fatal("outage did not read the second namespace", err)
	}
	deleted := map[string]bool{}
	for _, a := range client.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == "networkpolicies" {
			pre := a.(clienttesting.DeleteAction).GetDeleteOptions().Preconditions
			if pre == nil || pre.UID == nil || string(*pre.UID) != a.GetNamespace()+"-policy" {
				t.Fatal("cleanup lost namespace/UID binding")
			}
			deleted[a.GetNamespace()] = true
		}
	}
	if len(deleted) != 2 {
		t.Fatal("partial failure left a namespace disconnected", deleted)
	}
}

func TestOutageDoesNotDeleteExistingSecondNamespacePolicy(t *testing.T) {
	s := twoNamespaceScenarios()
	s.in.registryIP = "192.0.2.1"
	s.load.Outage = duration{time.Second}
	existing := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: outagePolicyName, Namespace: "work-b", UID: "existing-policy"}}
	client := fake.NewClientset(existing)
	s.clientset = client
	client.PrependReactor("create", "networkpolicies", func(a clienttesting.Action) (bool, runtime.Object, error) {
		a.(clienttesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy).UID = "created-policy"
		return false, nil, nil
	})
	if err := s.outage(context.Background()); err == nil {
		t.Fatal("existing fault policy was overwritten")
	}
	got, err := client.NetworkingV1().NetworkPolicies("work-b").Get(context.Background(), outagePolicyName, metav1.GetOptions{})
	if err != nil || got.UID != "existing-policy" {
		t.Fatal("cleanup touched an existing policy", got, err)
	}
	deleted := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "delete" {
			deleted++
			if a.GetNamespace() != "work-a" || *a.(clienttesting.DeleteAction).GetDeleteOptions().Preconditions.UID != "created-policy" {
				t.Fatal("cleanup crossed its creation boundary")
			}
		}
	}
	if deleted != 1 {
		t.Fatal("created policy was not removed", deleted)
	}
}
