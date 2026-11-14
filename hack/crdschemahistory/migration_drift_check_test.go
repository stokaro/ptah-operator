package crdschemahistory

import "testing"

// TestMigrationDriftCheckBreakHoldsForEveryVersion20Object holds the declared
// break to its reason: each rule schema version 21 adds holds for each spec a
// version 20 schema admitted, and refuses the object it exists to refuse, so
// the evaluation is not vacuous.
func TestMigrationDriftCheckBreakHoldsForEveryVersion20Object(t *testing.T) {
	t.Parallel()
	target := map[string]any{"urlFrom": map[string]any{"name": "orders-db", "key": "url"}}
	shadow := func(urlFrom map[string]any) map[string]any {
		return map[string]any{"driftCheck": map[string]any{"shadow": map[string]any{"urlFrom": urlFrom}}}
	}
	version20 := []map[string]any{
		{"target": target},
		{"target": target, "policy": map[string]any{}},
		{"target": target, "policy": map[string]any{"apply": "OnApproval", "lockTimeout": "5m", "transactionMode": "file"}},
	}
	admitted := []map[string]any{
		{"target": target, "policy": shadow(map[string]any{"name": "orders-shadow", "key": "url"})},
		{"target": target, "policy": shadow(map[string]any{"name": "orders-db", "key": "shadow-url"})},
		{"target": target, "policy": shadow(map[string]any{"name": "orders-shadow", "key": "url", "optional": false})},
	}
	for _, test := range []struct {
		rule    string
		refuses []map[string]any
	}{
		{rule: ruleShadowIsRequired, refuses: []map[string]any{
			{"target": target, "policy": shadow(map[string]any{"name": "", "key": "url"})},
			{"target": target, "policy": shadow(map[string]any{"name": "orders-shadow", "key": ""})},
			{"target": target, "policy": shadow(map[string]any{"name": "orders-shadow", "key": "url", "optional": true})},
		}},
		{rule: ruleShadowIsNotTarget, refuses: []map[string]any{
			{"target": target, "policy": shadow(map[string]any{"name": "orders-db", "key": "url"})},
		}},
	} {
		for _, self := range append(append([]map[string]any{}, version20...), admitted...) {
			if !evaluateRule(t, test.rule, self) {
				t.Errorf("%q refuses %v", test.rule, self)
			}
		}
		for _, self := range test.refuses {
			if evaluateRule(t, test.rule, self) {
				t.Errorf("%q admits %v", test.rule, self)
			}
		}
	}
	if len(migrationDriftCheckBreak.transitions) != 2 {
		t.Fatalf("the break declares %d transitions, the two spec rules make 2", len(migrationDriftCheckBreak.transitions))
	}
}
