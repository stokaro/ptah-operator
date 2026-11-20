package crdschemahistory

import "testing"

// TestRepeatableMigrationsBreakHoldsForEveryVersion23Object holds the declared
// break to its reason: each rule schema version 24 adds holds for each object
// a version 23 schema admitted and for the new shapes it exists to admit, and
// refuses the objects it exists to refuse, so the evaluation is not vacuous.
func TestRepeatableMigrationsBreakHoldsForEveryVersion23Object(t *testing.T) {
	t.Parallel()
	migration := map[string]any{"version": int64(3), "checksum": "sha256:c3"}
	repeatable := map[string]any{"identity": "item_names", "checksum": "h1:" + "AAkShW1bQ18rh5+kF6nNdfoHF+XZ/7T1YU+poQYCPN4="}
	for _, test := range []struct {
		rule              string
		version23, admits []map[string]any
		refuses           []map[string]any
	}{
		{
			rule:      rulePlanCarriesWork,
			version23: []map[string]any{{"migrations": []any{migration}}, {"migrations": []any{migration, migration}}},
			admits:    []map[string]any{{"repeatables": []any{repeatable}}, {"migrations": []any{migration}, "repeatables": []any{repeatable}}},
			refuses:   []map[string]any{{}, {"currentVersion": int64(3)}},
		},
		{
			rule: ruleLossNamesOneMigration,
			version23: []map[string]any{
				{"version": int64(1), "state": "Applied"},
				{"version": int64(7), "state": "Failed", "sqlState": "22001", "errorNumber": int64(1406)},
			},
			admits:  []map[string]any{{"repeatable": "item_names", "state": "Applied"}},
			refuses: []map[string]any{{"state": "Applied"}, {"version": int64(1), "repeatable": "item_names", "state": "Applied"}},
		},
	} {
		for _, self := range append(append([]map[string]any{}, test.version23...), test.admits...) {
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
	if len(repeatableMigrationsBreak.transitions) != 4 {
		t.Fatalf("the break declares %d transitions; the plan rule and the loss rule in three places make 4",
			len(repeatableMigrationsBreak.transitions))
	}
}
