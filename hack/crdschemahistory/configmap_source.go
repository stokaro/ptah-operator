package crdschemahistory

import "strconv"

// A resource's source becomes a union: an OCI artifact or a ConfigMap
// (#696, #698). Schema version 3 adds rules to fields that existed at version
// 2, and the verifier cannot tell on its own that no stored object fails them.
// None does, because each rule holds for every object a version 2 schema
// admitted:
//
//   - a source at version 2 required ociRef and verificationPolicyFrom and had
//     no configMap, so it has exactly one of the two sources, a policy with
//     its OCI reference, and registry settings beside it;
//   - an access binding required resolvedReference and had no snapshot;
//   - a plan's contractVersion was 1, and it required artifactDigest and both
//     verification policy fields and had no source;
//   - the rule on a source's verification policy now also admits its absence.
//
// TestConfigMapSourceBreakHoldsForEveryVersion2Object evaluates each rule
// against those shapes.
var configMapSourceBreak = declaredBreak{
	version: 3,
	reason: "a source is an OCI artifact or a ConfigMap; every rule the union adds holds for an object a " +
		"version 2 schema admitted, which required ociRef, verificationPolicyFrom, resolvedReference and a version 1 plan",
	transitions: configMapSourceTransitions(),
}

// The rules schema version 3 adds or changes, as the API types declare them.
const (
	ruleOneSource       = "has(self.ociRef) != has(self.configMap)"
	rulePolicyWithOCI   = "has(self.ociRef) == has(self.verificationPolicyFrom)"
	ruleNoRegistryAuth  = "has(self.ociRef) || !has(self.registryAuthFrom)"
	ruleNoTransport     = "has(self.ociRef) || !has(self.transport) || (!has(self.transport.caFrom) && (!has(self.transport.plainHTTP) || !self.transport.plainHTTP))"
	ruleOneBinding      = "has(self.resolvedReference) != has(self.snapshot)"
	rulePlanContract    = "self.contractVersion == 1 ? (has(self.artifactDigest) && has(self.verificationPolicyUID) && has(self.verificationPolicyDigest) && !has(self.source)) : (has(self.source) && !has(self.artifactDigest) && !has(self.verificationPolicyUID) && !has(self.verificationPolicyDigest))"
	ruleSchemaOneFile   = "!has(self.desired.configMap) || self.desired.configMap.items.size() == 1"
	ruleSchemaPolicy    = "!has(self.desired.verificationPolicyFrom) || (self.desired.verificationPolicyFrom.name.size() > 0 && self.desired.verificationPolicyFrom.key.size() > 0 && (!has(self.desired.verificationPolicyFrom.optional) || !self.desired.verificationPolicyFrom.optional))"
	ruleMigrationPolicy = "!has(self.artifact.verificationPolicyFrom) || (self.artifact.verificationPolicyFrom.name.size() > 0 && self.artifact.verificationPolicyFrom.key.size() > 0 && (!has(self.artifact.verificationPolicyFrom.optional) || !self.artifact.verificationPolicyFrom.optional))"
)

func configMapSourceTransitions() []string {
	rule := func(crd, path, expression string) string {
		return crd + ": " + path + ": x-kubernetes-validations gained or changed the rule " + strconv.Quote(expression)
	}
	const (
		schemas        = "ptahschemas.operator.ptah.run"
		migrations     = "ptahmigrations.operator.ptah.run"
		schemaPlans    = "ptahschemaplans.operator.ptah.run"
		migrationPlans = "ptahmigrationplans.operator.ptah.run"
	)
	transitions := []string{
		rule(migrationPlans, "spec", rulePlanContract),
		rule(schemaPlans, "spec", rulePlanContract),
		rule(migrations, "spec", ruleMigrationPolicy),
		rule(schemas, "spec", ruleSchemaOneFile),
		rule(schemas, "spec", ruleSchemaPolicy),
		rule(migrations, "status.activeOperation.source", ruleOneBinding),
		rule(migrations, "status.artifact", ruleOneBinding),
		rule(schemas, "status.activeOperation.source", ruleOneBinding),
		rule(schemas, "status.pendingObservation.source", ruleOneBinding),
	}
	for _, source := range []struct{ crd, path string }{{migrations, "spec.artifact"}, {schemas, "spec.desired"}} {
		for _, expression := range []string{ruleOneSource, rulePolicyWithOCI, ruleNoRegistryAuth, ruleNoTransport} {
			transitions = append(transitions, rule(source.crd, source.path, expression))
		}
	}
	return transitions
}
