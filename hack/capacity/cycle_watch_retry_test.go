package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

type cycleWatchTransport func(*http.Request) (*http.Response, error)

func (f cycleWatchTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCycleWatchRetriesFromItsCursor(t *testing.T) {
	for _, failure := range []string{"HTTP2 connection lost", "unavailable watch event"} {
		t.Run(failure, func(t *testing.T) {
			var lists, watches atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Query().Get("watch") != "true" {
					lists.Add(1)
					fmt.Fprintln(w, `{"apiVersion":"operator.ptah.run/v1alpha1","kind":"PtahMigrationList","metadata":{"resourceVersion":"42"},"items":[]}`)
					return
				}
				switch watches.Load() {
				case 1, 3:
					rv := 43
					if watches.Load() == 3 {
						rv = 45
					}
					fmt.Fprintf(w, `{"type":"MODIFIED","object":{"apiVersion":"operator.ptah.run/v1alpha1","kind":"PtahMigration","metadata":{"name":"slot","namespace":"work","uid":"uid","generation":1,"resourceVersion":"%d","creationTimestamp":"2026-10-01T00:00:00Z"},"status":{"observedGeneration":1}}}`+"\n", rv)
					fmt.Fprintf(w, `{"type":"BOOKMARK","object":{"apiVersion":"operator.ptah.run/v1alpha1","kind":"PtahMigration","metadata":{"resourceVersion":"%d"}}}`+"\n", rv+1)
				case 2:
					fmt.Fprintln(w, `{"type":"ERROR","object":{"apiVersion":"v1","kind":"Status","code":503,"reason":"ServiceUnavailable","message":"temporary outage"}}`)
				default:
					w.WriteHeader(http.StatusGone)
					fmt.Fprintln(w, `{"apiVersion":"v1","kind":"Status","code":410,"reason":"Expired","message":"test cursor expired"}`)
				}
			}))
			defer server.Close()
			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL, Transport: cycleWatchTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("watch") == "true" {
					n := watches.Add(1)
					want := map[int32]string{1: "42", 2: "44", 3: "44", 4: "46"}[n]
					if r.URL.Query().Get("resourceVersion") != want || r.URL.Query().Get("labelSelector") != "workload=proof" {
						t.Errorf("watch %d lost its cursor or selector: %s", n, r.URL.RawQuery)
					}
					if n == 2 && failure == "HTTP2 connection lost" {
						return nil, errors.New("http2: client connection lost")
					}
				}
				return transport.RoundTrip(r)
			})})
			if err != nil {
				t.Fatal(err)
			}
			recorder := newCycleRecorder(client.Resource(migrationResource).Namespace("work"), "migration", "work", "workload=proof")
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			go recorder.run(ctx)
			select {
			case <-recorder.done:
			case <-ctx.Done():
				t.Fatal("watch failed to terminate")
			}
			h := recorder.snapshot()
			if lists.Load() != 1 || watches.Load() != 4 || h.Cursor != "46" || len(h.Readings) != 2 || !strings.Contains(h.Error, "410") {
				t.Fatalf("lost history or accepted expiry: lists=%d watches=%d history=%+v", lists.Load(), watches.Load(), h)
			}
			if len(h.Retries) != 1 || h.Retries[0].Cursor != "44" || h.Retries[0].At.IsZero() || h.Retries[0].Error == "" {
				t.Fatalf("transport failure was not retained: %+v", h.Retries)
			}
		})
	}
}

func TestCycleWatchBoundsRetriesAndPreservesUnrecoveredFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", canceled), func(t *testing.T) {
			var lists, watches atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				lists.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintln(w, `{"apiVersion":"operator.ptah.run/v1alpha1","kind":"PtahMigrationList","metadata":{"resourceVersion":"42"},"items":[]}`)
			}))
			defer server.Close()
			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL, Transport: cycleWatchTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("watch") == "true" {
					watches.Add(1)
					if r.URL.Query().Get("resourceVersion") != "42" {
						t.Error("retry changed the cursor")
					}
					return nil, errors.New("http2: client connection lost")
				}
				return transport.RoundTrip(r)
			})})
			if err != nil {
				t.Fatal(err)
			}
			recorder := newCycleRecorder(client.Resource(migrationResource).Namespace("work"), "migration", "work", "workload=proof")
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			go recorder.run(ctx)
			if canceled {
				for len(recorder.snapshot().Retries) == 0 && ctx.Err() == nil {
					time.Sleep(time.Millisecond)
				}
				cancel()
			}
			select {
			case <-recorder.done:
			case <-time.After(5 * time.Second):
				t.Fatal("failed watch did not terminate")
			}
			h := recorder.snapshot()
			if lists.Load() != 1 || h.Cursor != "42" || !strings.Contains(h.Error, "http2: client connection lost") {
				t.Fatalf("unrecovered failure erased or relisted: %+v", h)
			}
			if !canceled && (watches.Load() != 4 || len(h.Retries) != 3) {
				t.Fatalf("retry bound changed: watches=%d retries=%d", watches.Load(), len(h.Retries))
			}
		})
	}
}

func TestCycleWatchDoesNotRetryInvalidEvidenceOrAuthority(t *testing.T) {
	for _, err := range []error{
		apierrors.NewResourceExpired("old cursor"), apierrors.NewGone("old cursor"),
		apierrors.NewUnauthorized("no credentials"), apierrors.NewForbidden(schema.GroupResource{Resource: "ptahschemas"}, "slot", errors.New("no grant")),
		errors.New("cycle watch event has no object cursor"), errors.New("unexpected cycle watch event"),
	} {
		if retryableCycleWatchError(err) {
			t.Fatalf("retried a permanent failure: %v", err)
		}
	}
}
