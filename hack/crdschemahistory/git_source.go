package crdschemahistory

import "strconv"

// A resource's source union gains Git (#699). Schema version 4 changes two
// rules that existed at version 3 so they count three members instead of two,
// and adds one to a schema's spec, and the verifier cannot tell on its own
// that no stored object fails them. None does, because a version 3 schema had
// no git field:
//
//   - a source at version 3 had exactly one of ociRef and configMap, so it has
//     exactly one of ociRef, configMap and git;
//   - an access binding at version 3 had exactly one of resolvedReference and
//     snapshot, so it has exactly one of the three;
//   - a schema's spec at version 3 had no desired.git, so the rule that a Git
//     path selects a file holds for it.
//
// TestGitSourceBreakHoldsForEveryVersion3Object evaluates each rule against
// those shapes.
var gitSourceBreak = declaredBreak{
	version: 4,
	reason: "a source may be a Git commit; the source and access-binding rules count three members, a schema's " +
		"Git path has to select a file, and every object a version 3 schema admitted had exactly one of the other " +
		"two and no git",
	transitions: gitSourceTransitions(),
}

// The rules schema version 4 changes, as the API types declare them.
const (
	ruleOneOfThreeSources  = "[has(self.ociRef), has(self.configMap), has(self.git)].filter(set, set).size() == 1"
	ruleOneOfThreeBindings = "[has(self.resolvedReference), has(self.snapshot), has(self.git)].filter(set, set).size() == 1"
	ruleSchemaGitFile      = "!has(self.desired.git) || self.desired.git.path != '.'"
)

func gitSourceTransitions() []string {
	rule := func(crd, path, expression string) string {
		return crd + ": " + path + ": x-kubernetes-validations gained or changed the rule " + strconv.Quote(expression)
	}
	const (
		schemas    = "ptahschemas.operator.ptah.run"
		migrations = "ptahmigrations.operator.ptah.run"
	)
	return []string{
		rule(migrations, "spec.artifact", ruleOneOfThreeSources),
		rule(migrations, "status.activeOperation.source", ruleOneOfThreeBindings),
		rule(migrations, "status.artifact", ruleOneOfThreeBindings),
		rule(schemas, "spec", ruleSchemaGitFile),
		rule(schemas, "spec.desired", ruleOneOfThreeSources),
		rule(schemas, "status.activeOperation.source", ruleOneOfThreeBindings),
		rule(schemas, "status.pendingObservation.source", ruleOneOfThreeBindings),
	}
}
