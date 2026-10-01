package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func measurementFixture(t *testing.T, failPath, managerMetrics string, mutate ...func(string, map[string]any)) *sampler {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == failPath {
			http.Error(w, "injected read failure", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/proxy/metrics") {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(managerMetrics))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/metrics/") {
			_, _ = w.Write([]byte("# TYPE apiserver_flowcontrol_rejected_requests_total counter\napiserver_flowcontrol_rejected_requests_total 0\n# TYPE process_start_time_seconds gauge\nprocess_start_time_seconds 1790812800\n"))
			return
		}
		list := map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{}}
		switch {
		case r.URL.Path == "/api/v1/nodes":
			list["kind"] = "NodeList"
			list["items"] = []any{map[string]any{"metadata": map[string]any{"name": "control-one", "uid": "node-uid", "labels": map[string]any{"node-role.kubernetes.io/control-plane": ""}}}}
		case r.URL.Path == "/api/v1/namespaces/kube-system/pods":
			list["kind"] = "PodList"
			list["items"] = []any{fixtureAPIPod()}
		case r.URL.Path == "/api/v1/namespaces/kube-system/pods/api-one":
			list = fixtureAPIPod()
		case r.URL.Path == "/api/v1/namespaces/operator/pods":
			list["kind"] = "PodList"
			list["items"] = []any{
				fixtureManager("manager"), fixtureManager("manager-2"),
			}
		case r.URL.Path == "/api/v1/namespaces/operator/pods/manager":
			list = fixtureManager("manager")
		case r.URL.Path == "/api/v1/namespaces/operator/pods/manager-2":
			list = fixtureManager("manager-2")
		case r.URL.Path == "/api/v1/namespaces/work/pods":
			list["kind"] = "PodList"
		case r.URL.Path == "/apis/batch/v1/namespaces/work/jobs":
			list["apiVersion"], list["kind"] = "batch/v1", "JobList"
		case strings.HasPrefix(r.URL.Path, "/apis/operator.ptah.run/v1alpha1/namespaces/work/"):
			list["apiVersion"] = "operator.ptah.run/v1alpha1"
			if strings.HasSuffix(r.URL.Path, "/ptahschemas") || strings.HasSuffix(r.URL.Path, "/ptahschemaplans") {
				list["items"] = []any{map[string]any{"metadata": map[string]any{"name": "already-read", "namespace": "work", "uid": "already-read-uid", "resourceVersion": "1", "generation": int64(1)}}}
			}
		default:
			t.Errorf("unexpected API request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		for _, change := range mutate {
			change(r.URL.Path, list)
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(server.Close)
	config := &rest.Config{Host: server.URL, QPS: 100, Burst: 100}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return &sampler{expectedManagers: 2, expectedAPIServers: 1, scrapeAPI: func(ctx context.Context, pod corev1.Pod) (scrape, error) {
		body, err := clientset.CoreV1().RESTClient().Get().AbsPath("/metrics/" + pod.Name).DoRaw(ctx)
		if err != nil {
			return nil, err
		}
		return parseScrape(body)
	}, clientset: clientset, dynamic: dynamicClient, namespace: "work", operatorNamespace: "operator",
		metricsPort: 8080, jobs: map[string]*jobRecord{}}
}

const completeProcessMetrics = "# TYPE process_resident_memory_bytes gauge\nprocess_resident_memory_bytes 104857600\n# TYPE process_cpu_seconds_total counter\nprocess_cpu_seconds_total 2\n# TYPE process_start_time_seconds gauge\nprocess_start_time_seconds 1790812800\n"

func fixtureManager(name string) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "uid": name + "-uid"},
		"status": map[string]any{"phase": "Running", "containerStatuses": []any{
			map[string]any{"name": "manager", "containerID": "containerd://" + name, "restartCount": 0,
				"state": map[string]any{"running": map[string]any{"startedAt": "2026-10-01T00:00:00Z"}}},
		}},
	}
}

func fixtureAPIPod() map[string]any {
	pod := fixtureManager("api-one")
	meta := pod["metadata"].(map[string]any)
	meta["namespace"] = "kube-system"
	meta["labels"] = map[string]any{"component": "kube-apiserver"}
	pod["spec"] = map[string]any{"nodeName": "control-one"}
	pod["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)["name"] = "kube-apiserver"
	return pod
}

func jsonObject(t *testing.T, value any) map[string]json.RawMessage {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func TestFailedReadsAreMissingEvidence(t *testing.T) {
	t.Parallel()
	for source, path := range map[string]string{
		sourcePods:      "/api/v1/namespaces/work/pods",
		sourceResources: "/apis/operator.ptah.run/v1alpha1/namespaces/work/ptahmigrations",
		sourceRetained:  "/apis/operator.ptah.run/v1alpha1/namespaces/work/ptahschemaplanchunks",
		sourceManagers:  "/api/v1/namespaces/operator/pods/manager:8080/proxy/metrics",
		sourceAPI:       "/metrics/api-one",
		sourceJobs:      "/apis/batch/v1/namespaces/work/jobs",
	} {
		t.Run(source, func(t *testing.T) {
			s := measurementFixture(t, path, completeProcessMetrics)
			s.take(context.Background())
			samples, jobs := s.snapshot()
			if len(samples) != 1 || len(samples[0].Incomplete) != 1 || samples[0].Incomplete[0] != source {
				t.Fatalf("missing source was lost: %+v", samples)
			}
			object := jsonObject(t, samples[0])
			for _, field := range sampleFields[source] {
				if string(object[field]) != "null" {
					t.Errorf("sample %s = %s, want null", field, object[field])
				}
			}
			at := samples[0].At
			cost := cost(window{Name: "probe", Start: at.Add(-time.Second), End: at.Add(time.Second)}, samples, jobs)
			if cost.Incomplete[source] != 1 {
				t.Fatal("the scenario lost the incomplete reading")
			}
			object = jsonObject(t, cost)
			for _, field := range scenarioFields[source] {
				if string(object[field]) != "null" {
					t.Errorf("scenario %s = %s, want null", field, object[field])
				}
			}
			var summary strings.Builder
			if err := writeSummary(&summary, report{Scenarios: []scenarioCost{cost}}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(summary.String(), "missing "+source+"=1") {
				t.Fatal("the summary hid a failed reading")
			}
			columns := map[string][]int{
				sourcePods: {5}, sourceResources: {6, 7}, sourceManagers: {8, 9, 10, 11},
				sourceAPI: {12}, sourceJobs: {3, 4},
			}
			rows := 0
			for _, line := range strings.Split(summary.String(), "\n") {
				if !strings.HasPrefix(line, "| probe |") {
					continue
				}
				parts := strings.Split(line, "|")
				// The separate freshness table has eight columns.
				if len(parts) != 15 {
					continue
				}
				rows++
				for _, column := range columns[source] {
					if strings.TrimSpace(parts[column]) != "n/a" {
						t.Errorf("summary column %d still reports %q after a failed read", column, parts[column])
					}
				}
			}
			if rows != 1 {
				t.Fatalf("examined %d summary rows, want exactly one", rows)
			}
		})
	}
}

func TestSuccessfulZeroIsDistinctFromFailedRead(t *testing.T) {
	t.Parallel()
	s := measurementFixture(t, "", completeProcessMetrics)
	s.take(context.Background())
	samples, jobs := s.snapshot()
	if len(samples) != 1 || len(samples[0].Incomplete) != 0 {
		t.Fatalf("complete collection was refused: %+v", samples)
	}
	object := jsonObject(t, samples[0])
	if string(object["podsPending"]) != "0" || string(object["plans"]) != "1" || string(object["managers"]) == "null" {
		t.Fatal("measured values were replaced with missing evidence")
	}
	at := samples[0].At
	result := cost(window{Start: at.Add(-time.Second), End: at.Add(time.Second)}, samples, jobs)
	object = jsonObject(t, result)
	if string(object["podsPendingMax"]) != "0" || string(object["managerRSSMaxBytes"]) != "104857600" {
		t.Fatal("a measured zero or positive control was lost")
	}
}

func TestMissingProcessMetricsInvalidateTheManagerReading(t *testing.T) {
	t.Parallel()
	for _, metrics := range []string{"", strings.ReplaceAll(completeProcessMetrics, "process_cpu_seconds_total 2", "process_cpu_seconds_total NaN"),
		strings.ReplaceAll(completeProcessMetrics, "104857600", "0"),
		strings.ReplaceAll(completeProcessMetrics, "1790812800", "NaN"),
		strings.ReplaceAll(completeProcessMetrics, "1790812800", "0"),
		completeProcessMetrics + "# TYPE rest_client_rate_limiter_duration_seconds_sum counter\nrest_client_rate_limiter_duration_seconds_sum NaN\n",
		completeProcessMetrics + "# TYPE rest_client_requests_total counter\nrest_client_requests_total{code=\"429\"} +Inf\n",
		completeProcessMetrics + "# TYPE workqueue_depth gauge\nworkqueue_depth{name=\"schema\"} NaN\n"} {
		s := measurementFixture(t, "", metrics)
		s.take(context.Background())
		samples, _ := s.snapshot()
		if len(samples) != 1 || len(samples[0].Incomplete) != 1 || samples[0].Incomplete[0] != sourceManagers ||
			string(jsonObject(t, samples[0])["managers"]) != "null" {
			t.Fatal("missing or invalid process metrics became a zero")
		}
	}
}

func TestEmptyWindowHasNoMeasuredFigures(t *testing.T) {
	t.Parallel()
	result := cost(window{}, nil, nil)
	object := jsonObject(t, result)
	for _, fields := range scenarioFields {
		for _, field := range fields {
			if string(object[field]) != "null" {
				t.Errorf("empty window %s = %s, want null", field, object[field])
			}
		}
	}
}

func TestCanceledCollectionPreservesMissingEvidence(t *testing.T) {
	t.Parallel()
	s := measurementFixture(t, "", completeProcessMetrics)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.take(ctx); err == nil {
		t.Fatal("canceled collection succeeded")
	}
	samples, _ := s.snapshot()
	if len(samples) != 1 || len(samples[0].Incomplete) != 6 {
		t.Fatalf("canceled collection lost its missing sources: %+v", samples)
	}
	object := jsonObject(t, samples[0])
	for _, fields := range sampleFields {
		for _, field := range fields {
			if string(object[field]) != "null" {
				t.Errorf("canceled collection %s = %s, want null", field, object[field])
			}
		}
	}
}

func TestManagerScrapeRetainsAndConfirmsProcessIdentity(t *testing.T) {
	s := measurementFixture(t, "", completeProcessMetrics)
	s.take(context.Background())
	samples, _ := s.snapshot()
	if len(samples) != 1 || len(samples[0].Incomplete) != 0 || len(samples[0].Managers) != 2 {
		t.Fatalf("complete manager collection refused: %+v", samples)
	}
	for name, reading := range samples[0].Managers {
		if reading.PodUID != name+"-uid" || reading.ContainerID != "containerd://"+name || reading.ContainerStartedAt.IsZero() || reading.ProcessStartedAt != 1790812800 {
			t.Errorf("lost process identity: %+v", reading)
		}
	}
	for _, mode := range []string{"Pod replaced during scrape", "container restarted during scrape", "listed replica stopped"} {
		t.Run(mode, func(t *testing.T) {
			s := measurementFixture(t, "", completeProcessMetrics, func(path string, object map[string]any) {
				switch mode {
				case "Pod replaced during scrape":
					if path == "/api/v1/namespaces/operator/pods/manager" {
						object["metadata"].(map[string]any)["uid"] = "replacement"
					}
				case "container restarted during scrape":
					if path == "/api/v1/namespaces/operator/pods/manager" {
						object["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)["containerID"] = "replacement"
					}
				case "listed replica stopped":
					if path == "/api/v1/namespaces/operator/pods" {
						object["items"].([]any)[0].(map[string]any)["status"].(map[string]any)["phase"] = "Pending"
					}
				}
			})
			s.take(context.Background())
			samples, _ := s.snapshot()
			if len(samples) != 1 || len(samples[0].Incomplete) != 1 || samples[0].Incomplete[0] != sourceManagers || string(jsonObject(t, samples[0])["managers"]) != "null" {
				t.Fatal("partial or mixed-identity scrape was published as complete")
			}
		})
	}
}
