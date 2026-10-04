package e2e

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPostgresBackendTerminationRequiresOriginalSession(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "original"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.3.161"}}
	reading := "42/10.244.3.161/1790832000.123456"
	query, err := alPostgresBackendTermination("ptah_alert_schema_postgresql", reading, pod)
	if err != nil {
		t.Fatal(err)
	}
	for _, predicate := range []string{"pid=42", "datname='ptah_alert_schema_postgresql'", "usename='ptah_e2e'", "host(client_addr)='10.244.3.161'", "extract(epoch FROM backend_start)=1790832000.123456"} {
		if !strings.Contains(query, predicate) {
			t.Fatalf("termination lost session binding %s: %s", predicate, query)
		}
	}
	for _, invalid := range []string{
		"42/10.244.3.161", "42/10.244.3.162/1790832000.123456",
		"42/10.244.3.161/0", "42/10.244.3.161/NaN", "42/10.244.3.161/1e9",
		"42/10.244.3.161/1790832000; SELECT 1", "42/10.244.3.161/1790832000.1234567",
		"2147483648/10.244.3.161/1790832000", "0/10.244.3.161/1790832000",
		reading + "\n" + reading,
	} {
		if q, err := alPostgresBackendTermination("isolated", invalid, pod); err == nil || q != "" {
			t.Fatalf("unsafe backend accepted: %q", invalid)
		}
	}
	for _, database := range []string{"", "unquoted database", "db'; SELECT 1"} {
		if _, err := alPostgresBackendTermination(database, reading, pod); err == nil {
			t.Fatal("unsafe database identifier accepted")
		}
	}
	if _, err := alPostgresBackendTermination("isolated", reading, nil); err == nil {
		t.Fatal("missing original Pod accepted")
	}
	pod.Status.Phase = corev1.PodSucceeded
	if _, err := alPostgresBackendTermination("isolated", reading, pod); err == nil {
		t.Fatal("backend identity was bound after its Pod stopped")
	}
}
