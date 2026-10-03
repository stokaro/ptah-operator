package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func TestAlScrapeReadingsFromPrometheus(t *testing.T) {
	t.Parallel()
	read := func(name string) []byte {
		t.Helper()
		body, err := os.ReadFile("../../testdata/e2e/readings/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	if !alScrapeFaultLoaded(read("prometheus-scrape-fault-config.json"), "leader") {
		t.Fatal("the pinned Prometheus runtime's normalized fault configuration was refused")
	}
	if !alScrapeFaultLoaded(read("prometheus-scrape-restored-config.json"), "") {
		t.Fatal("the pinned Prometheus runtime's restored configuration was refused")
	}
	loaded := time.Date(2026, 9, 29, 14, 10, 0, 0, time.UTC)
	if !alOneTargetLost(read("prometheus-one-target-lost.json"), 2, "leader", loaded) {
		t.Fatal("the pinned Prometheus runtime's failed leader scrape was refused")
	}
}

func TestAlScrapeFaultLoaded(t *testing.T) {
	t.Parallel()
	response := func(config string) []byte {
		body, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"yaml": config}})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	base := alPrometheusConfig("monitoring", "operator", "ptah-metrics")
	fault := alScrapeFaultConfig(base, "manager-a")
	if !alScrapeFaultLoaded(response(fault), "manager-a") || !alScrapeFaultLoaded(response(base), "") {
		t.Fatal("the selected fault and restored configuration were not recognized")
	}
	for name, body := range map[string][]byte{
		"old volume":           response(base),
		"wrong Pod":            response(alScrapeFaultConfig(base, "manager-b")),
		"all Pods":             response(strings.ReplaceAll(fault, `regex: "manager-a"`, `regex: ".*"`)),
		"wrong label":          response(strings.ReplaceAll(fault, "__meta_kubernetes_pod_name", "pod")),
		"wrong path":           response(strings.ReplaceAll(fault, alMissingMetricsPath, "/metrics")),
		"drop instead of fail": response(strings.ReplaceAll(fault, "action: replace", "action: drop")),
		"wrong job":            response(strings.ReplaceAll(fault, "job_name: "+alScrapeJob, "job_name: another")),
		"duplicate fault":      response(alScrapeFaultConfig(fault, "manager-a")),
		"no config":            []byte(`{"status":"success","data":{}}`),
		"API failure":          []byte(`{"status":"error","data":{}}`),
		"invalid YAML":         response("scrape_configs: ["),
	} {
		if alScrapeFaultLoaded(body, "manager-a") {
			t.Errorf("%s was accepted", name)
		}
	}
	if alScrapeFaultLoaded(response(fault), "") {
		t.Error("a remaining fault counted as restoration")
	}
}

func TestAlOneTargetLost(t *testing.T) {
	t.Parallel()
	loaded := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	lost := alTarget{
		Labels: map[string]string{"job": alScrapeJob, "pod": "leader"}, Health: "down",
		ScrapeURL: "http://10.0.0.1:8080" + alMissingMetricsPath,
		LastError: "server returned HTTP status 404 Not Found", LastScrape: loaded.Add(time.Second),
	}
	follower := alTarget{Labels: map[string]string{"job": alScrapeJob, "pod": "follower"}, Health: "up"}
	if !alOneTargetLost(alTargetsBody(t, lost, follower), 2, "leader", loaded) {
		t.Fatal("a failed leader scrape with its follower up was refused")
	}
	for name, mutate := range map[string]func(*alTarget){
		"leader healthy":  func(t *alTarget) { t.Health = "up" },
		"no actual error": func(t *alTarget) { t.LastError = "" },
		"old scrape":      func(t *alTarget) { t.LastScrape = loaded.Add(-time.Second) },
		"no scrape time":  func(t *alTarget) { t.LastScrape = time.Time{} },
		"old scrape path": func(t *alTarget) { t.ScrapeURL = "http://10.0.0.1:8080/metrics" },
		"invalid URL":     func(t *alTarget) { t.ScrapeURL = "://" },
		"another job":     func(t *alTarget) { t.Labels = map[string]string{"job": "another", "pod": "leader"} },
		"not the leader":  func(t *alTarget) { t.Labels = map[string]string{"job": alScrapeJob, "pod": "other"} },
	} {
		bad := lost
		mutate(&bad)
		if alOneTargetLost(alTargetsBody(t, bad, follower), 2, "leader", loaded) {
			t.Errorf("%s was accepted", name)
		}
	}
	brokenFollower := follower
	brokenFollower.Health = "down"
	for name, targets := range map[string][]alTarget{
		"no targets":       nil,
		"missing follower": {lost},
		"missing leader":   {follower},
		"both down":        {lost, brokenFollower},
		"duplicate leader": {lost, lost},
		"extra target":     {lost, follower, follower},
	} {
		if alOneTargetLost(alTargetsBody(t, targets...), 2, "leader", loaded) {
			t.Errorf("%s was accepted", name)
		}
	}
	if alOneTargetLost(alTargetsBody(t, lost), 1, "leader", loaded) ||
		alOneTargetLost(alTargetsBody(t, lost, follower), 2, "", loaded) ||
		alOneTargetLost(alTargetsBody(t, lost, follower), 2, "leader", time.Time{}) {
		t.Error("an unbound or single-target fault was accepted")
	}
}

func TestAlSameManagers(t *testing.T) {
	t.Parallel()
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{UID: "leader-lease"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: ptr.To("leader_unique"), LeaseTransitions: ptr.To[int32](2)},
	}
	pods := []corev1.Pod{}
	for _, name := range []string{"leader", "follower"} {
		pods = append(pods, corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid")},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "manager", Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
			},
		})
	}
	if !alSameManagers(lease, pods, lease, pods) {
		t.Fatal("unchanged ready managers were refused")
	}
	beforeElection := lease.DeepCopy()
	beforeElection.Spec.HolderIdentity = ptr.To("retired_unique")
	if alSameManagers(beforeElection, pods, beforeElection, pods) {
		t.Fatal("ready replacements with a retired Lease holder passed as elected")
	}
	if !alSameManagers(lease, []corev1.Pod{pods[1], pods[0]}, lease, pods) {
		t.Fatal("Pod list order changed identity")
	}
	for name, change := range map[string]func(*coordinationv1.Lease, []corev1.Pod){
		"new Lease":                 func(l *coordinationv1.Lease, _ []corev1.Pod) { l.UID = "replacement" },
		"leader moved":              func(l *coordinationv1.Lease, _ []corev1.Pod) { l.Spec.HolderIdentity = ptr.To("follower_unique") },
		"leader moved and returned": func(l *coordinationv1.Lease, _ []corev1.Pod) { l.Spec.LeaseTransitions = ptr.To[int32](4) },
		"leader disappeared":        func(l *coordinationv1.Lease, _ []corev1.Pod) { l.Spec.HolderIdentity = nil },
		"replaced Pod":              func(_ *coordinationv1.Lease, p []corev1.Pod) { p[0].UID = "replacement" },
		"terminating Pod":           func(_ *coordinationv1.Lease, p []corev1.Pod) { p[0].DeletionTimestamp = ptr.To(metav1.Now()) },
		"unready follower": func(_ *coordinationv1.Lease, p []corev1.Pod) {
			p[1].Status.Conditions[0].Status = corev1.ConditionFalse
		},
		"nonrunning Pod":    func(_ *coordinationv1.Lease, p []corev1.Pod) { p[0].Status.Phase = corev1.PodPending },
		"process restarted": func(_ *coordinationv1.Lease, p []corev1.Pod) { p[0].Status.ContainerStatuses[0].RestartCount++ },
		"process waiting":   func(_ *coordinationv1.Lease, p []corev1.Pod) { p[0].Status.ContainerStatuses[0].State.Running = nil },
		"process unready":   func(_ *coordinationv1.Lease, p []corev1.Pod) { p[0].Status.ContainerStatuses[0].Ready = false },
		"missing statuses":  func(_ *coordinationv1.Lease, p []corev1.Pod) { p[0].Status.ContainerStatuses = nil },
		"duplicate Pods":    func(_ *coordinationv1.Lease, p []corev1.Pod) { p[1] = p[0] },
	} {
		changedLease := lease.DeepCopy()
		changedPods := []corev1.Pod{*pods[0].DeepCopy(), *pods[1].DeepCopy()}
		change(changedLease, changedPods)
		if alSameManagers(changedLease, changedPods, lease, pods) {
			t.Errorf("%s passed as unchanged", name)
		}
	}
	if alSameManagers(lease, nil, lease, nil) || alSameManagers(lease, pods[:1], lease, pods[:1]) {
		t.Error("an empty or single-manager fixture was accepted")
	}
}
