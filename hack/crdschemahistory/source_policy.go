package crdschemahistory

import "strconv"

// A ConfigMap or Git source gains sourcePolicyFrom (#700). Schema version 5
// adds two rules to the source of both kinds, and the verifier cannot tell on
// its own that no stored object fails them. None does, because a version 4
// schema had no sourcePolicyFrom field, and both rules hold for a source
// without one.
//
// TestSourcePolicyBreakHoldsForEveryVersion4Object evaluates each rule against
// those shapes.
var sourcePolicyBreak = declaredBreak{
	version: 5,
	reason: "a ConfigMap or Git source may name a source policy; its two rules hold for every source a version 4 " +
		"schema admitted, which had no sourcePolicyFrom",
	transitions: sourcePolicyTransitions(),
}

// The rules schema version 5 adds, as the API types declare them.
const (
	ruleSourcePolicyNotOCI      = "!has(self.sourcePolicyFrom) || !has(self.ociRef)"
	ruleSourcePolicyRequiredKey = "!has(self.sourcePolicyFrom) || (self.sourcePolicyFrom.name.size() > 0 && " +
		"self.sourcePolicyFrom.key.size() > 0 && (!has(self.sourcePolicyFrom.optional) || !self.sourcePolicyFrom.optional))"
)

func sourcePolicyTransitions() []string {
	rule := func(crd, path, expression string) string {
		return crd + ": " + path + ": x-kubernetes-validations gained or changed the rule " + strconv.Quote(expression)
	}
	const (
		schemas    = "ptahschemas.operator.ptah.run"
		migrations = "ptahmigrations.operator.ptah.run"
	)
	return []string{
		rule(migrations, "spec.artifact", ruleSourcePolicyNotOCI),
		rule(migrations, "spec.artifact", ruleSourcePolicyRequiredKey),
		rule(schemas, "spec.desired", ruleSourcePolicyNotOCI),
		rule(schemas, "spec.desired", ruleSourcePolicyRequiredKey),
	}
}
