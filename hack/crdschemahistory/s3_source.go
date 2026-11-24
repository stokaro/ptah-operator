package crdschemahistory

import "strconv"

// A resource's source union gains an S3 object (#679). Schema version 27
// changes the rules that existed at version 26 so they count five members
// instead of four, and adds one rule to each kind's spec, and the verifier
// cannot tell on its own that no stored object fails them. None does, because
// a version 26 object had no s3 field:
//
//   - a source at version 26 had exactly one of ociRef, configMap, git and
//     https, so it has exactly one of the five;
//   - an access binding at version 26 had exactly one of resolvedReference,
//     snapshot, git and https, so it has exactly one of the five;
//   - a schema's spec at version 26 had no desired.s3, so the rule that an S3
//     path selects a file holds for it, and a migration's spec had no
//     artifact.s3, so the rule that its format is an archive holds.
//
// TestS3SourceBreakHoldsForEveryVersion26Object evaluates each rule against
// those shapes.
var s3SourceBreak = declaredBreak{
	version: 27,
	reason: "a source may be an S3 object; the source and access-binding rules count five members, a schema's " +
		"S3 path has to select a file, a migration's S3 format has to be an archive, and every object a " +
		"version 26 schema admitted had exactly one of the other four and no s3",
	transitions: s3SourceTransitions(),
}

// The rules schema version 27 changes, as the API types declare them.
const (
	ruleOneOfFiveSources   = "[has(self.ociRef), has(self.configMap), has(self.git), has(self.https), has(self.s3)].filter(set, set).size() == 1"
	ruleOneOfFiveBindings  = "[has(self.resolvedReference), has(self.snapshot), has(self.git), has(self.https), has(self.s3)].filter(set, set).size() == 1"
	ruleSchemaS3File       = "!has(self.desired.s3) || self.desired.s3.path != '.'"
	ruleMigrationS3Archive = "!has(self.artifact.s3) || self.artifact.s3.format != 'File'"
)

func s3SourceTransitions() []string {
	rule := func(crd, path, expression string) string {
		return crd + ": " + path + ": x-kubernetes-validations gained or changed the rule " + strconv.Quote(expression)
	}
	const (
		schemas    = "ptahschemas.operator.ptah.run"
		migrations = "ptahmigrations.operator.ptah.run"
	)
	return []string{
		rule(migrations, "spec", ruleMigrationS3Archive),
		rule(migrations, "spec.artifact", ruleOneOfFiveSources),
		rule(migrations, "status.activeOperation.source", ruleOneOfFiveBindings),
		rule(migrations, "status.artifact", ruleOneOfFiveBindings),
		rule(schemas, "spec", ruleSchemaS3File),
		rule(schemas, "spec.desired", ruleOneOfFiveSources),
		rule(schemas, "status.activeOperation.source", ruleOneOfFiveBindings),
		rule(schemas, "status.pendingObservation.source", ruleOneOfFiveBindings),
	}
}
