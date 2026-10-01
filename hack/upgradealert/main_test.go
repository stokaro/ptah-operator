package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Exercise the real CLI entry path and client-go watch transport. The API
// server here is a transport fixture, not live upgrade qualification.
func TestPrepareServeAndRestartRetainTheFailedHook(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	boundary := make(chan string, 2)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/batch/v1/namespaces/operator/jobs" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("fieldSelector") != "metadata.name=ptah-crd-manager" {
			http.Error(w, "unbounded watch", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") != "true" {
			_ = json.NewEncoder(w).Encode(&batchv1.JobList{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "JobList"}, ListMeta: metav1.ListMeta{ResourceVersion: "100"}})
			return
		}
		boundary <- r.URL.Query().Get("resourceVersion")
		s, err := loadState(statePath)
		if err != nil {
			http.Error(w, "missing state", 500)
			return
		}
		at := time.Now().UTC()
		job := fixtureJob(s, "current-upgrade", 0)
		job.CreationTimestamp = metav1.NewTime(s.StartedAt.Truncate(time.Second))
		job.TypeMeta = metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}
		job.ResourceVersion = "101"
		terminateJob(job, batchv1.JobFailed, at)
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "MODIFIED", "object": job})
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer api.Close()
	config := clientcmdapi.NewConfig()
	config.Clusters["test"] = &clientcmdapi.Cluster{Server: api.URL}
	config.AuthInfos["test"] = &clientcmdapi.AuthInfo{}
	config.Contexts["test"] = &clientcmdapi.Context{Cluster: "test", AuthInfo: "test"}
	config.CurrentContext = "test"
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := clientcmd.WriteToFile(*config, kubeconfig); err != nil {
		t.Fatal(err)
	}
	chart, values, intentPath := filepath.Join(dir, "candidate.tgz"), filepath.Join(dir, "values.yaml"), filepath.Join(dir, "intent.json")
	for _, p := range []string{chart, values} {
		if err := os.WriteFile(p, []byte("retained input bytes\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	intent := fixtureState().Intent
	var err error
	intent.ChartDigest, err = fileDigest(chart)
	if err != nil {
		t.Fatal(err)
	}
	intent.ValuesDigest, err = fileDigest(values)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(intentPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	prepare := []string{"prepare", "--state", statePath, "--kubeconfig", kubeconfig, "--intent", intentPath, "--chart", chart, "--values", values}
	if err := run(context.Background(), prepare); err != nil {
		t.Fatal(err)
	}
	original, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), prepare); err == nil {
		t.Fatal("prepare replaced an existing transaction")
	}
	// Hand the CLI an already bound socket; no free-port discovery race.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runWithListener(ctx, []string{"serve", "--state", statePath, "--kubeconfig", kubeconfig, "--listen", address}, func(string, string) (net.Listener, error) { return listener, nil })
	}()
	select {
	case rv := <-boundary:
		if rv != "100" {
			t.Fatal("watch lost prepared boundary", rv)
		}
	case err := <-done:
		t.Fatalf("serve stopped: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not establish watch")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	found := false
	for !found {
		select {
		case <-deadline.C:
			t.Fatal("serve did not publish retained failure")
		case <-tick.C:
			response, err := http.Get("http://" + address + "/metrics")
			if err != nil {
				continue
			}
			var buf strings.Builder
			_, err = io.Copy(&buf, response.Body)
			response.Body.Close()
			found = err == nil && strings.Contains(buf.String(), `ptah_operator_upgrade_failed{operator_namespace="operator",release="ptah"} 1`)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not release its watch")
	}
	restored, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if restored.FailedAt == nil || restored.ResourceVersion != "101" || !restored.StartedAt.Equal(original.StartedAt) || !restored.Deadline.Equal(original.Deadline) {
		t.Fatal("restart lost its event or original deadline")
	}
	lock, err := lockState(statePath)
	if err != nil {
		t.Fatal("stopped observer kept its writer lock", err)
	}
	lock.Close()
}
