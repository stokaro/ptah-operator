package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func TestManagerExitSignals(t *testing.T) {
	t.Parallel()
	var actual bytes.Buffer
	logger := zap.New(zap.UseDevMode(false), zap.WriteTo(&actual)).WithName("setup")
	logger.Error(errors.New("leader election lost"), "manager stopped")
	if got := managerExitSignals(actual.Bytes()); !reflect.DeepEqual(got, []string{"leader_election_lost", "manager_stopped"}) {
		t.Fatalf("manager logger output classified as %v", got)
	}
	for name, row := range map[string]struct {
		body string
		want []string
	}{
		"klog lease renewal": {
			`E0929 13:51:16.000000 1 leaderelection.go:509] "Failed to update lease" err="Put https://user:secret@api.invalid: context deadline exceeded" lock="operator/ptah-operator.operator.ptah.run"` + "\n" +
				`I0929 13:51:17.000000 1 leaderelection.go:306] "Failed to renew lease" lock="operator/ptah-operator.operator.ptah.run" err="context deadline exceeded"`,
			[]string{"lease_renew_failed", "lease_update_failed", "request_deadline"},
		},
		"JSON lease read":           {`{"msg":"Error retrieving lease lock","error":"leases is forbidden: secret-value"}`, []string{"api_forbidden", "lease_read_failed"}},
		"cache start":               {`{"logger":"setup","msg":"manager stopped","error":"failed to wait for ptah caches to sync kind source: timed out waiting for cache to be synced"}`, []string{"cache_sync_failed", "manager_stopped"}},
		"other manager error":       {`{"logger":"setup","msg":"manager stopped","error":"password=secret-value"}`, []string{"manager_stopped"}},
		"another logger":            {`{"logger":"untrusted","msg":"manager stopped","error":"leader election lost"}`, []string{}},
		"unrelated reconcile error": {`{"msg":"Reconciler error","error":"leader election lost; context deadline exceeded; password=secret-value"}`, []string{}},
		"not a log":                 {`password=secret-value manager stopped leader election lost`, []string{}},
		"wrong klog source":         {`E0929 13:51:16.000000 1 elsewhere.go:509] "Failed to update lease" err="context deadline exceeded"`, []string{}},
		"malformed JSON":            {`{"logger":"setup","msg":"manager stopped",`, []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			got := managerExitSignals([]byte(row.body))
			if !reflect.DeepEqual(got, row.want) {
				t.Fatalf("signals = %v, want %v", got, row.want)
			}
		})
	}
}

func managerExitPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "manager", Namespace: "operator", UID: "original"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "manager", RestartCount: 1,
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ContainerID: "containerd://previous", ExitCode: 1,
				FinishedAt: metav1.NewTime(time.Date(2026, 9, 29, 13, 51, 17, 0, time.UTC)),
			}},
		}}},
	}
}

func TestReadManagerExit(t *testing.T) {
	t.Parallel()
	const stopped = `{"logger":"setup","msg":"manager stopped","error":"leader election lost","private":"secret-value"}`
	for name, row := range map[string]struct {
		change      func(*corev1.Pod, int32)
		body        string
		logStatus   int
		want        managerExitDiagnostic
		logRequests int32
	}{
		"exact previous run":   {body: stopped, want: managerExitDiagnostic{Read: "read", Signals: []string{"leader_election_lost", "manager_stopped"}}, logRequests: 1},
		"replaced before read": {change: func(p *corev1.Pod, _ int32) { p.UID = "replacement" }, want: managerExitDiagnostic{Read: "identity_changed"}},
		"restarted during read": {change: func(p *corev1.Pod, read int32) {
			if read == 2 {
				p.Status.ContainerStatuses[0].RestartCount++
			}
		}, body: stopped, want: managerExitDiagnostic{Read: "identity_changed"}, logRequests: 1},
		"termination changed": {change: func(p *corev1.Pod, read int32) {
			if read == 2 {
				p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ContainerID = "new"
			}
		}, body: stopped, want: managerExitDiagnostic{Read: "identity_changed"}, logRequests: 1},
		"secret in API error":       {body: "error contains secret-value", logStatus: http.StatusForbidden, want: managerExitDiagnostic{Read: "unavailable"}, logRequests: 1},
		"server ignores byte limit": {body: stopped + "\n" + strings.Repeat("secret-value", managerExitLogLimit), want: managerExitDiagnostic{Read: "read", Limited: true, Signals: []string{"leader_election_lost", "manager_stopped"}}, logRequests: 1},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := managerExitPod()
			var reads, logs atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/namespaces/operator/pods/manager":
					pod := snapshot.DeepCopy()
					n := reads.Add(1)
					if row.change != nil {
						row.change(pod, n)
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(pod)
				case "/api/v1/namespaces/operator/pods/manager/log":
					logs.Add(1)
					q := r.URL.Query()
					if q.Get("previous") != "true" || q.Get("container") != "manager" || q.Get("tailLines") != "512" || q.Get("limitBytes") != fmt.Sprint(managerExitLogLimit) {
						t.Errorf("log request was not bound to the previous container and limits: %v", q)
					}
					if row.logStatus != 0 {
						w.WriteHeader(row.logStatus)
					}
					_, _ = w.Write([]byte(row.body))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			got := readManagerExit(t.Context(), clientset, snapshot, snapshot.Status.ContainerStatuses[0])
			if !reflect.DeepEqual(got, row.want) {
				t.Errorf("diagnosis = %+v, want %+v", got, row.want)
			}
			if logs.Load() != row.logRequests {
				t.Errorf("log requests = %d, want %d", logs.Load(), row.logRequests)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(encoded, []byte("secret-value")) {
				t.Error("the diagnosis leaked log or API error text")
			}
		})
	}
}

func TestReadManagerExitHonorsCancellation(t *testing.T) {
	t.Parallel()
	snapshot := managerExitPod()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			cancel()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot)
	}))
	defer server.Close()
	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if got := readManagerExit(ctx, clientset, snapshot, snapshot.Status.ContainerStatuses[0]); got.Read != "unavailable" || len(got.Signals) != 0 {
		t.Fatalf("canceled read produced a diagnosis: %+v", got)
	}
}
