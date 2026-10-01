package e2e

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// One range selector reads related metrics from one storage snapshot. Reusing
// an evaluation timestamp across HTTP requests does not freeze intervening
// scrape commits. Keep native samples intact; never join by interpolation or
// discard an unmatched sample to make a completeness check pass.
func alHistorySnapshotQuery(job, extraMatchers string, metrics []string) string {
	names := make([]string, len(metrics))
	for i, name := range metrics {
		names[i] = regexp.QuoteMeta(name)
	}
	return fmt.Sprintf(`{job=%q,__name__=~%q%s}[%ds]`, job, strings.Join(names, "|"), extraMatchers, int(alAdmissionHistoryWindow/time.Second))
}

func alSplitHistorySnapshot(body []byte, job string, metrics []string) (map[string][]byte, error) {
	series, err := alAdmissionNativeMatrix(body)
	if err != nil {
		return nil, err
	}
	groups := map[string][]alAdmissionSeries{}
	for _, name := range metrics {
		if _, exists := groups[name]; name == "" || exists {
			return nil, fmt.Errorf("history snapshot needs distinct metric names")
		}
		groups[name] = []alAdmissionSeries{}
	}
	for _, row := range series {
		name := row.Metric["__name__"]
		if _, exists := groups[name]; !exists || row.Metric["job"] != job {
			return nil, fmt.Errorf("history snapshot includes an unexpected metric or scrape job")
		}
		groups[name] = append(groups[name], row)
	}
	out := map[string][]byte{}
	for name, rows := range groups {
		out[name], err = json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": rows}})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
