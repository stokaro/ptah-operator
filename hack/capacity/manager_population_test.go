package main

import (
	"context"
	"slices"
	"testing"
)

func TestManagerCollectionRefusesIncompletePopulation(t *testing.T) {
	for _, scenario := range []string{"missing before collection", "missing after collection", "extra replica", "terminating replica"} {
		t.Run(scenario, func(t *testing.T) {
			lists := 0
			s := measurementFixture(t, "", completeProcessMetrics, func(path string, document map[string]any) {
				if path != "/api/v1/namespaces/operator/pods" {
					return
				}
				lists++
				items := document["items"].([]any)
				switch scenario {
				case "missing before collection":
					document["items"] = items[:1]
				case "missing after collection":
					if lists > 1 {
						document["items"] = items[:1]
					}
				case "extra replica":
					document["items"] = append(items, fixtureManager("manager-3"))
				case "terminating replica":
					items[1].(map[string]any)["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-01T00:00:01Z"
				}
			})
			s.take(context.Background())
			rows, _ := s.snapshot()
			if len(rows) != 1 || !slices.Contains(rows[0].Incomplete, sourceManagers) {
				t.Fatal("an incomplete manager population was reported as complete")
			}
			if value := jsonObject(t, rows[0])["managers"]; string(value) != "null" {
				t.Fatalf("partial process metrics remain publishable: %s", value)
			}
		})
	}
}
