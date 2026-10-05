package e2e

import (
	"strconv"
	"strings"
)

// runnerDeliveryFailure reports only recognized transport causes. Child output
// and receiver bodies may contain SQL or credentials, so never quote a log line.
func runnerDeliveryFailure(logs []byte) string {
	for _, line := range strings.Split(string(logs), "\n") {
		line = strings.TrimSuffix(line, "\r")
		for _, stage := range []struct{ marker, name string }{
			{"ptah-runner: durable result delivery failed", "delivery"},
			{"ptah-runner: result receiver preflight failed", "preflight"},
		} {
			if line == stage.marker {
				return stage.name + " failed; runner did not report the cause"
			}
			cause, found := strings.CutPrefix(line, stage.marker+": ")
			if !found {
				continue
			}
			cause, exhausted := strings.CutPrefix(cause, "result delivery attempts exhausted: ")
			known := ""
			switch cause {
			case "result receiver authentication failed", "result receiver unavailable":
				known = "transport error"
			case "result delivery credential is unavailable":
				known = "credential unavailable"
			case "context deadline exceeded":
				known = "deadline exceeded"
			case "context canceled":
				known = "canceled"
			default:
				for _, prefix := range []string{"result receiver returned HTTP ", "result receiver preflight returned HTTP "} {
					code, ok := strings.CutPrefix(cause, prefix)
					n, err := strconv.Atoi(code)
					if ok && err == nil && len(code) == 3 && n >= 100 && n <= 599 {
						known = "HTTP " + strconv.Itoa(n)
					}
				}
			}
			if known != "" {
				if exhausted {
					known += " (attempts exhausted)"
				}
				return stage.name + ": " + known
			}
		}
	}
	return "no recognized runner delivery diagnostic"
}
