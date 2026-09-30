package e2e

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLifecycleSQLBackendRequiresTheExactSessionBehindNAT(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "original-pod"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.9", HostIP: "172.18.0.3"}}
	backend := lifecycleSQLBackend{PID: 123, Client: pod.Status.HostIP, Database: "fixture", SessionStart: "2026-09-30 16:00:00 UTC"}
	raw, err := json.Marshal(backend)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := lifecycleSQLBackendForPod(raw, pod, "fixture"); err != nil || got != backend {
		t.Fatalf("NAT backend = %#v, %v", got, err)
	}
	for name, broken := range map[string]string{
		"multiple waiters": string(raw) + "\n" + string(raw),
		"unknown node":     strings.ReplaceAll(string(raw), backend.Client, "172.18.0.99"),
		"wrong database":   strings.ReplaceAll(string(raw), "fixture", "other"),
		"no process":       strings.ReplaceAll(string(raw), `"pid":123`, `"pid":0`),
		"no session date":  strings.ReplaceAll(string(raw), backend.SessionStart, ""),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := lifecycleSQLBackendForPod([]byte(broken), pod, "fixture"); err == nil {
				t.Fatal("unrelated or ambiguous backend passed")
			}
		})
	}
	row := map[string]any{"pid": backend.PID, "remote_host": backend.Client, "dbname": backend.Database,
		"session_start": backend.SessionStart, "message": "execute <unnamed>: " + lifecycleSQLControlStatement()}
	journal, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycleSQLBackendControl(journal, backend); err != nil {
		t.Fatal(err)
	}
	for field, value := range map[string]any{"pid": 124, "remote_host": pod.Status.PodIP, "dbname": "other",
		"session_start": "2026-09-30 15:59:59 UTC", "message": "statement: SELECT 1"} {
		t.Run(field, func(t *testing.T) {
			changed := maps.Clone(row)
			changed[field] = value
			journal, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if err := lifecycleSQLBackendControl(journal, backend); err == nil {
				t.Fatal("another session or statement substituted for the received control")
			}
		})
	}
	if err := lifecycleSQLBackendControl(append(journal, '{'), backend); err == nil {
		t.Fatal("a positive row hid a truncated journal")
	}
	if err := lifecycleSQLBackendControl(nil, backend); err == nil {
		t.Fatal("an empty journal supplied the control")
	}
}

func TestLifecycleSQLQuiescenceRequiresTheOriginalControlAndEveryRemoteClient(t *testing.T) {
	t.Parallel()
	const apply = "10.244.0.9"
	before := sqlAuditCounts{records: 8, clients: map[string]int64{apply: 3, "10.244.0.8": 2, "127.0.0.1": 3}}
	after := sqlAuditCounts{records: 10, clients: map[string]int64{apply: 3, "10.244.0.8": 2, "127.0.0.1": 5}}
	if err := lifecycleSQLQuiescent(before, after, apply); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*sqlAuditCounts, *sqlAuditCounts, *string){
		"empty baseline":           func(b, _ *sqlAuditCounts, _ *string) { *b = sqlAuditCounts{} },
		"no original control":      func(b, _ *sqlAuditCounts, _ *string) { delete(b.clients, apply) },
		"replayed by original Pod": func(_, a *sqlAuditCounts, _ *string) { a.clients[apply]++ },
		"new Pod address":          func(_, a *sqlAuditCounts, _ *string) { a.clients["10.244.0.10"] = 1 },
		"another known client":     func(_, a *sqlAuditCounts, _ *string) { a.clients["10.244.0.8"]++ },
		"lost original records":    func(_, a *sqlAuditCounts, _ *string) { a.clients[apply]-- },
		"lost unrelated client":    func(_, a *sqlAuditCounts, _ *string) { delete(a.clients, "10.244.0.8") },
		"decreased total":          func(_, a *sqlAuditCounts, _ *string) { a.records = 1 },
		"invalid client":           func(_, a *sqlAuditCounts, _ *string) { a.clients["unknown"] = 1 },
		"another loopback address": func(_, a *sqlAuditCounts, _ *string) { a.clients["127.0.0.2"] = 1 },
		"loopback control":         func(_, _ *sqlAuditCounts, host *string) { *host = "127.0.0.1" },
	} {
		t.Run(name, func(t *testing.T) {
			b, a, host := before, after, apply
			b.clients, a.clients = maps.Clone(before.clients), maps.Clone(after.clients)
			mutate(&b, &a, &host)
			if err := lifecycleSQLQuiescent(b, a, host); err == nil {
				t.Fatal("invalid lifecycle SQL window passed")
			}
		})
	}
}
