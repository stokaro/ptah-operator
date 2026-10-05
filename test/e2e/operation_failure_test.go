package e2e

import "testing"

func TestRunnerDeliveryFailureDoesNotExposeLogContent(t *testing.T) {
	for name, row := range map[string]struct{ logs, want string }{
		"busy receiver":     {"private SQL\nptah-runner: durable result delivery failed: result delivery attempts exhausted: result receiver returned HTTP 503\nprivate token", "delivery: HTTP 503 (attempts exhausted)"},
		"authority refusal": {"ptah-runner: durable result delivery failed: result receiver returned HTTP 403", "delivery: HTTP 403"},
		"preflight":         {"ptah-runner: result receiver preflight failed: result receiver preflight returned HTTP 503\r\n", "preflight: HTTP 503"},
		"transport":         {"ptah-runner: durable result delivery failed: result delivery attempts exhausted: result receiver unavailable", "delivery: transport error (attempts exhausted)"},
		"deadline":          {"ptah-runner: durable result delivery failed: context deadline exceeded", "delivery: deadline exceeded"},
		"old runner":        {"ptah-runner: durable result delivery failed", "delivery failed; runner did not report the cause"},
		"private suffix":    {"ptah-runner: durable result delivery failed: result receiver returned HTTP 503 private token", "no recognized runner delivery diagnostic"},
		"private error":     {"ptah-runner: durable result delivery failed: private SQL", "no recognized runner delivery diagnostic"},
		"quoted marker":     {"SQL contains ptah-runner: durable result delivery failed: result receiver returned HTTP 403", "no recognized runner delivery diagnostic"},
		"invalid status":    {"ptah-runner: durable result delivery failed: result receiver returned HTTP 999", "no recognized runner delivery diagnostic"},
		"no message":        {"private SQL and token", "no recognized runner delivery diagnostic"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := runnerDeliveryFailure([]byte(row.logs)); got != row.want {
				t.Fatalf("diagnostic = %q, want %q", got, row.want)
			}
		})
	}
}
