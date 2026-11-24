package crdschemahistory

import "strconv"

// A database URL may come from an object a SecretProviderClass of the Secrets
// Store CSI driver writes into the volume it mounts, beside a Secret key
// (#831). Schema version 27 makes urlFrom one of two sibling fields of a
// schema's target and development database and of a migration's target and
// drift-check shadow. Each place gains a rule that exactly one is set; the
// spec rules that read urlFrom are guarded by has(); two new spec rules read
// only the new field: that a class names a ServiceAccount other than default,
// and that a migration's shadow names another class object than its target.
//
// The verifier cannot tell on its own that no stored object fails them. None
// does: a version 26 schema required urlFrom in each place and had no
// urlFromSecretProviderClass, so exactly one is set in every stored object,
// a guarded rule holds wherever its unguarded form held, and a rule about the
// new field holds for every object without it. The status copies of a
// development database carry the type's rule as well, and the manager wrote
// them from a spec that required urlFrom.
//
// TestSecretProviderClassBreakHoldsForEveryVersion26Object evaluates the
// rules against those shapes.
var secretProviderClassBreak = declaredBreak{
	version: 27,
	reason: "a database URL may come from a SecretProviderClass object beside a Secret key; the rules that exactly " +
		"one is set, the has() guards on the urlFrom rules and the rules about the new field hold for every object " +
		"a version 26 schema admitted, each of which named a Secret key and no class",
	transitions: secretProviderClassTransitions(),
}

// The rules schema version 27 adds or changes, as the API types declare them.
const (
	ruleOneURLSource = "has(self.urlFrom) != has(self.urlFromSecretProviderClass)"

	ruleSchemaTargetSecret = "!has(self.target.urlFrom) || (self.target.urlFrom.name.size() > 0 && " +
		"self.target.urlFrom.key.size() > 0 && (!has(self.target.urlFrom.optional) || !self.target.urlFrom.optional))"
	ruleSchemaDevSecret = "!has(self.dev) || !has(self.dev.urlFrom) || (self.dev.urlFrom.name.size() > 0 && " +
		"self.dev.urlFrom.key.size() > 0 && (!has(self.dev.urlFrom.optional) || !self.dev.urlFrom.optional))"
	ruleSchemaClassAccount = "(!has(self.target.urlFromSecretProviderClass) && (!has(self.dev) || " +
		"!has(self.dev.urlFromSecretProviderClass))) || (has(self.execution) && has(self.execution.serviceAccountName) && " +
		"self.execution.serviceAccountName.size() > 0 && self.execution.serviceAccountName != 'default')"

	ruleMigrationShadowSecret = "!has(self.policy) || !has(self.policy.driftCheck) || " +
		"!has(self.policy.driftCheck.shadow.urlFrom) || (self.policy.driftCheck.shadow.urlFrom.name.size() > 0 && " +
		"self.policy.driftCheck.shadow.urlFrom.key.size() > 0 && (!has(self.policy.driftCheck.shadow.urlFrom.optional) || " +
		"!self.policy.driftCheck.shadow.urlFrom.optional))"
	ruleMigrationShadowSecretIsNotTarget = "!has(self.policy) || !has(self.policy.driftCheck) || " +
		"!has(self.policy.driftCheck.shadow.urlFrom) || !has(self.target.urlFrom) || " +
		"self.policy.driftCheck.shadow.urlFrom.name != self.target.urlFrom.name || " +
		"self.policy.driftCheck.shadow.urlFrom.key != self.target.urlFrom.key"
	ruleMigrationShadowClassIsNotTarget = "!has(self.policy) || !has(self.policy.driftCheck) || " +
		"!has(self.policy.driftCheck.shadow.urlFromSecretProviderClass) || !has(self.target.urlFromSecretProviderClass) || " +
		"self.policy.driftCheck.shadow.urlFromSecretProviderClass.name != self.target.urlFromSecretProviderClass.name || " +
		"self.policy.driftCheck.shadow.urlFromSecretProviderClass.objectName != " +
		"self.target.urlFromSecretProviderClass.objectName"
	ruleMigrationTargetSecret = ruleSchemaTargetSecret
	ruleMigrationClassAccount = "(!has(self.target.urlFromSecretProviderClass) && (!has(self.policy) || " +
		"!has(self.policy.driftCheck) || !has(self.policy.driftCheck.shadow.urlFromSecretProviderClass))) || " +
		"(has(self.execution) && has(self.execution.serviceAccountName) && " +
		"self.execution.serviceAccountName.size() > 0 && self.execution.serviceAccountName != 'default')"
)

func secretProviderClassTransitions() []string {
	rule := func(crd, path, expression string) string {
		return crd + ": " + path + ": x-kubernetes-validations gained or changed the rule " + strconv.Quote(expression)
	}
	const migrations, schemas = "ptahmigrations.operator.ptah.run", "ptahschemas.operator.ptah.run"
	return []string{
		rule(migrations, "spec", ruleMigrationShadowSecretIsNotTarget),
		rule(migrations, "spec", ruleMigrationShadowSecret),
		rule(migrations, "spec", ruleMigrationShadowClassIsNotTarget),
		rule(migrations, "spec", ruleMigrationTargetSecret),
		rule(migrations, "spec", ruleMigrationClassAccount),
		rule(migrations, "spec.policy.driftCheck.shadow", ruleOneURLSource),
		rule(migrations, "spec.target", ruleOneURLSource),
		rule(schemas, "spec", ruleSchemaDevSecret),
		rule(schemas, "spec", ruleSchemaTargetSecret),
		rule(schemas, "spec", ruleSchemaClassAccount),
		rule(schemas, "spec.dev", ruleOneURLSource),
		rule(schemas, "spec.target", ruleOneURLSource),
		rule(schemas, "status.activeOperation.observationDev", ruleOneURLSource),
		rule(schemas, "status.pendingObservation.dev", ruleOneURLSource),
	}
}
