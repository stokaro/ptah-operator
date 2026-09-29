package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func measurementFixture(t *testing.T, failPath, managerMetrics string) *sampler {
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
		if r.URL.Path == "/metrics" {
			_, _ = w.Write([]byte("# TYPE apiserver_flowcontrol_rejected_requests_total counter\napiserver_flowcontrol_rejected_requests_total 0\n"))
			return
		}
		list := map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{}}
		switch {
		case r.URL.Path == "/api/v1/namespaces/operator/pods":
			list["kind"] = "PodList"
			list["items"] = []any{
				map[string]any{"metadata": map[string]any{"name": "manager"}, "status": map[string]any{"phase": "Running"}},
				map[string]any{"metadata": map[string]any{"name": "manager-2"}, "status": map[string]any{"phase": "Running"}},
			}
		case r.URL.Path == "/api/v1/namespaces/work/pods":
			list["kind"] = "PodList"
		case r.URL.Path == "/apis/batch/v1/namespaces/work/jobs":
			list["apiVersion"], list["kind"] = "batch/v1", "JobList"
		case strings.HasPrefix(r.URL.Path, "/apis/operator.ptah.run/v1alpha1/namespaces/work/"):
			list["apiVersion"] = "operator.ptah.run/v1alpha1"
			if strings.HasSuffix(r.URL.Path, "/ptahschemas") || strings.HasSuffix(r.URL.Path, "/ptahschemaplans") {
				list["items"] = []any{map[string]any{"metadata": map[string]any{"name": "already-read"}}}
			}
		default:
			t.Errorf("unexpected API request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
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
	return &sampler{clientset: clientset, dynamic: dynamicClient, namespace: "work", operatorNamespace: "operator",
		metricsPort: 8080, jobs: map[string]*jobRecord{}}
}

const completeProcessMetrics = "# TYPE process_resident_memory_bytes gauge\nprocess_resident_memory_bytes 104857600\n# TYPE process_cpu_seconds_total counter\nprocess_cpu_seconds_total 2\n"

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
		sourceAPI:       "/metrics",
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
				rows++
				parts := strings.Split(line, "|")
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
		strings.ReplaceAll(completeProcessMetrics, "104857600", "0")} {
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

func TestCanceledFinalCollectionIsNotPublished(t *testing.T) {
	t.Parallel()
	s := measurementFixture(t, "", completeProcessMetrics)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.take(ctx)
	samples, _ := s.snapshot()
	if len(samples) != 0 {
		t.Fatal("the canceled final collection became a measurement")
	}
}
