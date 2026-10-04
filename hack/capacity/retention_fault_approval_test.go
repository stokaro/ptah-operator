package main

import (
	"encoding/json"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestRetentionFaultRenewsOnlyARecordedNativeStaleApproval(t *testing.T) {
	raw, err := os.ReadFile("testdata/retention-stale-approval.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"native stale", "pending", "consumed", "wrong reason", "replaced schema", "not waiting", "active claim", "changed generation", "changed spec", "reserved approval"} {
		t.Run(mode, func(t *testing.T) {
			var v map[string]*unstructured.Unstructured
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			a, o := v["approval"], v["schema"]
			expected := o.DeepCopy()
			want := "renew"
			wantError := false
			switch mode {
			case "pending":
				unstructured.RemoveNestedField(a.Object, "status")
				want = "wait"
			case "consumed":
				_ = unstructured.SetNestedSlice(a.Object, []any{map[string]any{"type": "Consumed", "status": "True"}}, "status", "conditions")
				want = "consumed"
			case "wrong reason":
				_ = unstructured.SetNestedSlice(a.Object, []any{map[string]any{"type": "Stale", "status": "True", "reason": "Other"}}, "status", "conditions")
				wantError = true
			case "replaced schema":
				o.SetUID("replacement")
				wantError = true
			case "not waiting":
				_ = unstructured.SetNestedField(o.Object, "Observing", "status", "phase")
				want = "wait"
			case "changed generation":
				o.SetGeneration(o.GetGeneration() + 1)
				wantError = true
			case "changed spec":
				_ = unstructured.SetNestedField(o.Object, "another", "spec", "target", "urlFrom", "name")
				wantError = true
			case "reserved approval":
				_ = unstructured.SetNestedMap(o.Object, map[string]any{"name": "other", "uid": "other"}, "status", "plan", "approval")
				want = "wait"
			case "active claim":
				_ = unstructured.SetNestedMap(o.Object, map[string]any{"type": "Observe"}, "status", "activeOperation")
				want = "wait"
			}
			got, err := faultApprovalRecoveryAction(a, o, expected)
			if (err != nil) != wantError || !wantError && got != want {
				t.Fatal(got, err)
			}
		})
	}
}
