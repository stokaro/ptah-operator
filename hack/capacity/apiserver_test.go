package main

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func identifiedAPI(name string, at time.Time, h histogram) apiReading {
	id, _ := identifiedManager(name, managerReading{}).identity()
	return apiReading{apiIdentity: apiIdentity{Node: name, NodeUID: name + "-node", Pod: name + "-pod", processIdentity: id},
		ScrapeStartedAt: at, ScrapeCompletedAt: at.Add(time.Millisecond), Admission: h}
}

func TestAPIHistogramsRemainSeparate(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var samples []sample
	for i := range 3 {
		row := sample{At: base.Add(time.Duration(i) * time.Minute), APIServers: map[string]apiReading{}}
		for name, bound := range map[string]float64{"fast": 0.1, "slow": 5} {
			h := testHistogram(map[float64]float64{0.1: 0, 5: float64(20 * i)}, float64(20*i), float64(i))
			if bound == 0.1 {
				h.buckets[0.1] = float64(20 * i)
			}
			api := identifiedAPI(name, row.At, h)
			row.APIServers[name] = api
			row.APITargets = append(row.APITargets, api.apiIdentity)
		}
		samples = append(samples, row)
	}
	costs, _, problems := apiGrowth(samples)
	if len(problems) != 0 || len(costs) != 2 || costs["fast"].AdmissionSeconds.P95 != 0.1 || costs["slow"].AdmissionSeconds.P95 != 5 {
		t.Fatalf("independent histograms mixed or lost: %+v %v", costs, problems)
	}
	for _, name := range []string{"fast", "slow"} {
		if costs[name].AdmissionSeconds.Count != 40 {
			t.Fatal("population count lost")
		}
	}
	for _, change := range []func([]sample){
		func(s []sample) { delete(s[1].APIServers, "slow") },
		func(s []sample) {
			for i, rejected := range []float64{10, 2, 15} {
				v := s[i].APIServers["slow"]
				v.Rejected = rejected
				s[i].APIServers["slow"] = v
			}
		},
		func(s []sample) { v := s[1].APIServers["slow"]; v.ProcessStartedAt++; s[1].APIServers["slow"] = v },
		func(s []sample) { v := s[1].APIServers["slow"]; v.NodeUID = "replacement"; s[1].APIServers["slow"] = v },
		func(s []sample) { v := s[1].APIServers["slow"]; v.Rejected = -1; s[1].APIServers["slow"] = v },
	} {
		copyRows := append([]sample(nil), samples...)
		for i := range copyRows {
			copyRows[i].APIServers = map[string]apiReading{}
			for name, value := range samples[i].APIServers {
				copyRows[i].APIServers[name] = value
			}
		}
		change(copyRows)
		if got, _, issues := apiGrowth(copyRows); len(issues) == 0 || got != nil {
			t.Fatal("partial or mixed identity population supplied an API bound")
		}
	}
}

func TestAPIPopulationRequiresEveryDeclaredNode(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	nodes := []corev1.Node{}
	pods := []corev1.Pod{}
	for _, name := range []string{"a", "b", "c"} {
		nodes = append(nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name), Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}}})
		pods = append(pods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-" + name, Namespace: "kube-system", UID: types.UID("pod-" + name), Labels: map[string]string{"component": "kube-apiserver"}},
			Spec: corev1.PodSpec{NodeName: name}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "kube-apiserver", ContainerID: "container-" + name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(base)}}}}}})
	}
	if targets, err := apiPopulation(nodes, pods, 3); err != nil || len(targets) != 3 {
		t.Fatalf("complete population refused: %v", err)
	}
	for name, change := range map[string]func([]corev1.Node, []corev1.Pod) ([]corev1.Node, []corev1.Pod){
		"missing node":   func(n []corev1.Node, p []corev1.Pod) ([]corev1.Node, []corev1.Pod) { return n[:2], p },
		"missing API":    func(n []corev1.Node, p []corev1.Pod) ([]corev1.Node, []corev1.Pod) { return n, p[:2] },
		"duplicate node": func(n []corev1.Node, p []corev1.Pod) ([]corev1.Node, []corev1.Pod) { n[1] = n[0]; return n, p },
		"duplicate Pod":  func(n []corev1.Node, p []corev1.Pod) ([]corev1.Node, []corev1.Pod) { p[1] = p[0]; return n, p },
		"foreign node": func(n []corev1.Node, p []corev1.Pod) ([]corev1.Node, []corev1.Pod) {
			p[1].Spec.NodeName = "foreign"
			return n, p
		},
		"not running": func(n []corev1.Node, p []corev1.Pod) ([]corev1.Node, []corev1.Pod) {
			p[1].Status.Phase = corev1.PodPending
			return n, p
		},
		"deleting": func(n []corev1.Node, p []corev1.Pod) ([]corev1.Node, []corev1.Pod) {
			p[1].DeletionTimestamp = &metav1.Time{Time: base}
			return n, p
		},
	} {
		t.Run(name, func(t *testing.T) {
			n, p := change(append([]corev1.Node(nil), nodes...), append([]corev1.Pod(nil), pods...))
			if _, err := apiPopulation(n, p, 3); err == nil {
				t.Fatal("partial or ambiguous population accepted")
			}
		})
	}
}

func TestAPICollectionRefusesReplacementDuringScrape(t *testing.T) {
	s := measurementFixture(t, "", completeProcessMetrics, func(path string, object map[string]any) {
		if path == "/api/v1/namespaces/kube-system/pods/api-one" {
			object["metadata"].(map[string]any)["uid"] = "replacement"
		}
	})
	s.take(context.Background())
	samples, _ := s.snapshot()
	if len(samples) != 1 || !slices.Contains(samples[0].Incomplete, sourceAPI) || string(jsonObject(t, samples[0])["apiServers"]) != "null" {
		t.Fatal("mixed identity API scrape became measured evidence")
	}
}

func TestAPICollectionRefusesPopulationChangeAfterScrapes(t *testing.T) {
	var lists atomic.Int32
	s := measurementFixture(t, "", completeProcessMetrics, func(path string, object map[string]any) {
		if path == "/api/v1/nodes" && lists.Add(1) > 1 {
			object["items"].([]any)[0].(map[string]any)["metadata"].(map[string]any)["uid"] = "new-node"
		}
	})
	s.take(context.Background())
	samples, _ := s.snapshot()
	if len(samples) != 1 || !slices.Contains(samples[0].Incomplete, sourceAPI) || string(jsonObject(t, samples[0])["apiServers"]) != "null" {
		t.Fatal("population changed after successful scrapes without invalidating the reading")
	}
}
