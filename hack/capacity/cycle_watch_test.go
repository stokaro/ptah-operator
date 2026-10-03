package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func TestCycleWatchResumesCursorAndRefusesExpiredHistory(t *testing.T) {
	for _, expired := range []string{"watch event", "HTTP response"} {
		t.Run(expired, func(t *testing.T) {
			var lists, watches atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Query().Get("watch") != "true" {
					lists.Add(1)
					fmt.Fprintln(w, `{"apiVersion":"operator.ptah.run/v1alpha1","kind":"PtahMigrationList","metadata":{"resourceVersion":"42"},"items":[]}`)
					return
				}
				if r.URL.Query().Get("labelSelector") != "workload=proof" {
					t.Error("watch lost workload selector")
				}
				n := watches.Add(1)
				want := "42"
				if n > 1 {
					want = "44"
				}
				if r.URL.Query().Get("resourceVersion") != want {
					t.Errorf("watch resumed %q, want %s", r.URL.Query().Get("resourceVersion"), want)
				}
				if n == 1 {
					fmt.Fprintln(w, `{"type":"MODIFIED","object":{"apiVersion":"operator.ptah.run/v1alpha1","kind":"PtahMigration","metadata":{"name":"slot","namespace":"work","uid":"uid","generation":1,"resourceVersion":"43","creationTimestamp":"2026-10-01T00:00:00Z"},"status":{"observedGeneration":1}}}`)
					fmt.Fprintln(w, `{"type":"BOOKMARK","object":{"apiVersion":"operator.ptah.run/v1alpha1","kind":"PtahMigration","metadata":{"resourceVersion":"44"}}}`)
					return
				}
				status := map[string]any{"apiVersion": "v1", "kind": "Status", "code": 410, "reason": "Expired", "message": "test cursor expired"}
				if expired == "HTTP response" {
					w.WriteHeader(http.StatusGone)
					_ = json.NewEncoder(w).Encode(status)
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{"type": "ERROR", "object": status})
				}
			}))
			defer server.Close()
			client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			recorder := newCycleRecorder(client.Resource(migrationResource).Namespace("work"), "migration", "work", "workload=proof")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go recorder.run(ctx)
			if err := <-recorder.ready; err != nil {
				t.Fatal(err)
			}
			select {
			case <-recorder.done:
			case <-ctx.Done():
				t.Fatal("expired watch did not terminate")
			}
			history := recorder.snapshot()
			if lists.Load() != 1 || watches.Load() != 2 || history.Cursor != "44" || len(history.Readings) != 1 || !strings.Contains(history.Error, "continuous history") {
				t.Fatalf("lost cursor or relisted over gap: lists=%d watches=%d history=%+v", lists.Load(), watches.Load(), history)
			}
		})
	}
}
