package crdschemahistory

import "strconv"

// A migration's policy may ask Ptah to check the target for drift before an
// Apply, against a shadow database a Secret key names (#1054). Schema version
// 21 adds two rules to a migration's spec, that the shadow names a required
// Secret key and that it is not the target's own key, and the verifier cannot
// tell on its own that no stored object fails them. None does: a version 20
// object had no policy.driftCheck, and both rules hold for every spec without
// one.
//
// TestMigrationDriftCheckBreakHoldsForEveryVersion20Object evaluates the rules
// against those shapes.
var migrationDriftCheckBreak = declaredBreak{
	version: 21,
	reason: "a migration's policy may name a shadow database for a drift check; the spec rules that the shadow " +
		"names a required Secret key, and not the target's, hold for every object a version 20 schema admitted, " +
		"none of which had policy.driftCheck",
	transitions: migrationDriftCheckTransitions(),
}

// The rules schema version 21 adds, as the API types declare them.
const (
	ruleShadowIsRequired = "!has(self.policy) || !has(self.policy.driftCheck) || " +
		"(self.policy.driftCheck.shadow.urlFrom.name.size() > 0 && self.policy.driftCheck.shadow.urlFrom.key.size() > 0 && " +
		"(!has(self.policy.driftCheck.shadow.urlFrom.optional) || !self.policy.driftCheck.shadow.urlFrom.optional))"
	ruleShadowIsNotTarget = "!has(self.policy) || !has(self.policy.driftCheck) || " +
		"self.policy.driftCheck.shadow.urlFrom.name != self.target.urlFrom.name || " +
		"self.policy.driftCheck.shadow.urlFrom.key != self.target.urlFrom.key"
)

func migrationDriftCheckTransitions() []string {
	transitions := make([]string, 0, 2)
	for _, rule := range []string{ruleShadowIsRequired, ruleShadowIsNotTarget} {
		transitions = append(transitions,
			"ptahmigrations.operator.ptah.run: spec: x-kubernetes-validations gained or changed the rule "+strconv.Quote(rule))
	}
	return transitions
}
