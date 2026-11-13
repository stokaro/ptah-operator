package crdschemahistory

import "strconv"

// A resource's source union gains an HTTPS download (#1041). Schema version 19
// changes the rules that existed at version 18 so they count four members
// instead of three, and adds one rule to each kind's spec, and the verifier
// cannot tell on its own that no stored object fails them. None does, because
// a version 18 object had no https field:
//
//   - a source at version 18 had exactly one of ociRef, configMap and git, so
//     it has exactly one of the four;
//   - an access binding at version 18 had exactly one of resolvedReference,
//     snapshot and git, so it has exactly one of the four;
//   - a schema's spec at version 18 had no desired.https, so the rule that an
//     HTTPS path selects a file holds for it, and a migration's spec had no
//     artifact.https, so the rule that its format is an archive holds.
//
// TestHTTPSSourceBreakHoldsForEveryVersion18Object evaluates each rule against
// those shapes.
var httpsSourceBreak = declaredBreak{
	version: 19,
	reason: "a source may be an HTTPS download; the source and access-binding rules count four members, a schema's " +
		"HTTPS path has to select a file, a migration's HTTPS format has to be an archive, and every object a " +
		"version 18 schema admitted had exactly one of the other three and no https",
	transitions: httpsSourceTransitions(),
}

// The rules schema version 19 changes, as the API types declare them.
const (
	ruleOneOfFourSources      = "[has(self.ociRef), has(self.configMap), has(self.git), has(self.https)].filter(set, set).size() == 1"
	ruleOneOfFourBindings     = "[has(self.resolvedReference), has(self.snapshot), has(self.git), has(self.https)].filter(set, set).size() == 1"
	ruleSchemaHTTPSFile       = "!has(self.desired.https) || self.desired.https.path != '.'"
	ruleMigrationHTTPSArchive = "!has(self.artifact.https) || self.artifact.https.format != 'File'"
)

func httpsSourceTransitions() []string {
	rule := func(crd, path, expression string) string {
		return crd + ": " + path + ": x-kubernetes-validations gained or changed the rule " + strconv.Quote(expression)
	}
	const (
		schemas    = "ptahschemas.operator.ptah.run"
		migrations = "ptahmigrations.operator.ptah.run"
	)
	return []string{
		rule(migrations, "spec", ruleMigrationHTTPSArchive),
		rule(migrations, "spec.artifact", ruleOneOfFourSources),
		rule(migrations, "status.activeOperation.source", ruleOneOfFourBindings),
		rule(migrations, "status.artifact", ruleOneOfFourBindings),
		rule(schemas, "spec", ruleSchemaHTTPSFile),
		rule(schemas, "spec.desired", ruleOneOfFourSources),
		rule(schemas, "status.activeOperation.source", ruleOneOfFourBindings),
		rule(schemas, "status.pendingObservation.source", ruleOneOfFourBindings),
	}
}
