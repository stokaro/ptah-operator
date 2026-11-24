package crdschemahistory

import "testing"

// TestSecretProviderClassBreakHoldsForEveryVersion26Object holds the declared
// break to its reason: each rule schema version 27 adds or changes holds for
// each shape a version 26 schema admitted, where urlFrom was required and no
// class existed, and refuses the object it exists to refuse, so the
// evaluation is not vacuous.
func TestSecretProviderClassBreakHoldsForEveryVersion26Object(t *testing.T) {
	t.Parallel()

	secret := func(name, key string) map[string]any { return map[string]any{"name": name, "key": key} }
	class := func(name, object string) map[string]any { return map[string]any{"name": name, "objectName": object} }
	bySecret := map[string]any{"urlFrom": secret("orders-db", "url")}
	byClass := map[string]any{"urlFromSecretProviderClass": class("orders-store", "url")}
	account := map[string]any{"serviceAccountName": "orders-migrator"}

	// What a version 26 schema admitted, in each place a rule reads.
	references := []map[string]any{
		bySecret,
		{"urlFrom": secret("orders-db", "url"), "engine": "postgresql"},
		{"urlFrom": map[string]any{"name": "orders-db", "key": "url", "optional": false}},
	}
	schemas := []map[string]any{
		{"target": bySecret},
		{"target": bySecret, "dev": map[string]any{"urlFrom": secret("orders-dev", "url")}},
		{"target": bySecret, "execution": map[string]any{}},
		{"target": bySecret, "execution": map[string]any{"serviceAccountName": "default"}},
	}
	migrations := []map[string]any{
		{"target": bySecret},
		{"target": bySecret, "policy": map[string]any{}},
		{"target": bySecret, "policy": map[string]any{"driftCheck": map[string]any{"shadow": map[string]any{"urlFrom": secret("orders-shadow", "url")}}}},
		{"target": bySecret, "execution": map[string]any{"serviceAccountName": "default"}},
	}
	shadow := func(source map[string]any) map[string]any {
		return map[string]any{"driftCheck": map[string]any{"shadow": source}}
	}

	for _, test := range []struct {
		name    string
		rule    string
		holds   []map[string]any
		refuses []map[string]any
	}{
		{name: "one URL source", rule: ruleOneURLSource, holds: append(append([]map[string]any{}, references...), byClass),
			refuses: []map[string]any{{}, {"urlFrom": secret("orders-db", "url"), "urlFromSecretProviderClass": class("orders-store", "url")}}},
		{name: "schema target Secret", rule: ruleSchemaTargetSecret, holds: append(append([]map[string]any{}, schemas...), map[string]any{"target": byClass}),
			refuses: []map[string]any{{"target": map[string]any{"urlFrom": secret("", "url")}}, {"target": map[string]any{"urlFrom": map[string]any{"name": "orders-db", "key": "url", "optional": true}}}}},
		{name: "schema dev Secret", rule: ruleSchemaDevSecret, holds: append(append([]map[string]any{}, schemas...), map[string]any{"target": bySecret, "dev": byClass}),
			refuses: []map[string]any{{"target": bySecret, "dev": map[string]any{"urlFrom": secret("orders-dev", "")}}}},
		{name: "schema class account", rule: ruleSchemaClassAccount,
			holds: append(append([]map[string]any{}, schemas...), map[string]any{"target": byClass, "execution": account}, map[string]any{"target": bySecret, "dev": byClass, "execution": account}),
			refuses: []map[string]any{
				{"target": byClass},
				{"target": byClass, "execution": map[string]any{}},
				{"target": byClass, "execution": map[string]any{"serviceAccountName": "default"}},
				{"target": byClass, "execution": map[string]any{"serviceAccountName": ""}},
				{"target": bySecret, "dev": byClass},
			}},
		{name: "migration target Secret", rule: ruleMigrationTargetSecret, holds: append(append([]map[string]any{}, migrations...), map[string]any{"target": byClass}),
			refuses: []map[string]any{{"target": map[string]any{"urlFrom": secret("orders-db", "")}}}},
		{name: "migration shadow Secret", rule: ruleMigrationShadowSecret, holds: append(append([]map[string]any{}, migrations...), map[string]any{"target": bySecret, "policy": shadow(byClass)}),
			refuses: []map[string]any{{"target": bySecret, "policy": shadow(map[string]any{"urlFrom": secret("", "url")})}}},
		{name: "migration shadow Secret is not the target", rule: ruleMigrationShadowSecretIsNotTarget,
			holds:   append(append([]map[string]any{}, migrations...), map[string]any{"target": byClass, "policy": shadow(bySecret)}, map[string]any{"target": bySecret, "policy": shadow(byClass)}),
			refuses: []map[string]any{{"target": bySecret, "policy": shadow(bySecret)}}},
		{name: "migration shadow class is not the target", rule: ruleMigrationShadowClassIsNotTarget,
			holds: append(append([]map[string]any{}, migrations...),
				map[string]any{"target": byClass, "policy": shadow(map[string]any{"urlFromSecretProviderClass": class("orders-store", "shadow-url")})},
				map[string]any{"target": byClass, "policy": shadow(map[string]any{"urlFromSecretProviderClass": class("orders-shadow", "url")})}),
			refuses: []map[string]any{{"target": byClass, "policy": shadow(byClass)}}},
		{name: "migration class account", rule: ruleMigrationClassAccount,
			holds: append(append([]map[string]any{}, migrations...), map[string]any{"target": byClass, "execution": account}, map[string]any{"target": bySecret, "policy": shadow(byClass), "execution": account}),
			refuses: []map[string]any{
				{"target": byClass},
				{"target": byClass, "execution": map[string]any{"serviceAccountName": "default"}},
				{"target": bySecret, "policy": shadow(byClass)},
				{"target": bySecret, "policy": shadow(byClass), "execution": map[string]any{"serviceAccountName": ""}},
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if len(test.holds) == 0 || len(test.refuses) == 0 {
				t.Fatalf("%q has %d admitted and %d refused shapes, want both", test.rule, len(test.holds), len(test.refuses))
			}
			for _, self := range test.holds {
				if !evaluateRule(t, test.rule, self) {
					t.Errorf("%q refuses %v", test.rule, self)
				}
			}
			for _, self := range test.refuses {
				if evaluateRule(t, test.rule, self) {
					t.Errorf("%q admits %v", test.rule, self)
				}
			}
		})
	}
	if got := len(secretProviderClassBreak.transitions); got != 14 {
		t.Fatalf("the break declares %d transitions, the generated CRDs make 14", got)
	}
}
