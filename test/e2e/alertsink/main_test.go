package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var receivedAt = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func deliver(t *testing.T, body string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	server := httptest.NewServer(handler(&out, func() time.Time { return receivedAt }))
	defer server.Close()
	response, err := http.Post(server.URL+"/alerts", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, out.String()
}

func TestADeliveryIsWrittenOneLinePerAlert(t *testing.T) {
	t.Parallel()
	status, out := deliver(t, `{
	  "version": "4", "receiver": "ptah", "status": "firing",
	  "alerts": [
	    {"status": "firing", "labels": {"alertname": "PtahOperatorUnresolvedApply", "family": "migration"},
	     "annotations": {"runbook_url": "https://example.invalid/operations/#unresolved-gauges"},
	     "startsAt": "2026-09-25T11:59:00Z", "endsAt": "0001-01-01T00:00:00Z"},
	    {"status": "resolved", "labels": {"alertname": "PtahOperatorOperationStalled", "family": "migration", "operation": "Apply"},
	     "startsAt": "2026-09-25T11:50:00Z", "endsAt": "2026-09-25T11:58:00Z"}
	  ]
	}`)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want one per alert:\n%s", len(lines), out)
	}
	var first delivery
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first.AlertName != "PtahOperatorUnresolvedApply" || first.Status != "firing" ||
		first.Labels["family"] != "migration" || first.Receiver != "ptah" ||
		first.Annotations["runbook_url"] == "" || !first.ReceivedAt.Equal(receivedAt) {
		t.Errorf("first line = %+v", first)
	}
	var second delivery
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if second.Status != "resolved" || second.AlertName != "PtahOperatorOperationStalled" {
		t.Errorf("second line = %+v", second)
	}
}

// A line in the log is a delivery that was understood, so anything else is
// refused and leaves the log as it was.
func TestADeliveryThatCannotBeReadWritesNothing(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"not json":      `firing`,
		"version 3":     `{"version": "3", "alerts": [{"status": "firing", "labels": {"alertname": "A"}}]}`,
		"no alert":      `{"version": "4", "alerts": []}`,
		"no alertname":  `{"version": "4", "alerts": [{"status": "firing", "labels": {}}]}`,
		"odd status":    `{"version": "4", "alerts": [{"status": "pending", "labels": {"alertname": "A"}}]}`,
		"one bad alert": `{"version": "4", "alerts": [{"status": "firing", "labels": {"alertname": "A"}}, {"status": "firing", "labels": {}}]}`,
	} {
		status, out := deliver(t, body)
		if status != http.StatusBadRequest || out != "" {
			t.Errorf("%s: status %d, wrote %q", name, status, out)
		}
	}
}
