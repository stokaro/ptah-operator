//go:build e2e

package e2e

import (
	"context"
	"time"
)

func (a *alertingRun) historySnapshot(ctx context.Context, at time.Time, job, matchers string, metrics ...string) (map[string][]byte, []byte) {
	body, err := a.prometheus(ctx, "/api/v1/query", map[string]string{
		"query": alHistorySnapshotQuery(job, matchers, metrics), "time": at.Format(time.RFC3339Nano),
	})
	if err != nil {
		a.logf("failed native history snapshot: queriedAt=%s body=%s", at.Format(time.RFC3339Nano), body)
	}
	a.check(err, "read the native history snapshot")
	groups, err := alSplitHistorySnapshot(body, job, metrics)
	if err != nil {
		a.logf("invalid native history snapshot: queriedAt=%s body=%s", at.Format(time.RFC3339Nano), body)
	}
	a.check(err, "split the complete native history snapshot")
	return groups, body
}
