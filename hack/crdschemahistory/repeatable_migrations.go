package crdschemahistory

import "strconv"

// A migration plan may carry repeatable migrations, the R__ files Ptah runs
// after the pending versions whenever their content is new or changed (#1138).
// Schema version 24 makes a plan's versioned list optional, so a plan may
// carry repeatable migrations alone, and adds the rule that it carries one of
// the two. It also lets a lost migration log entry name a repeatable migration
// instead of a version, under the rule that it names exactly one of them. The
// verifier cannot tell on its own that no stored object fails either rule.
// None does: a version 23 plan required a non-empty migrations list, and a
// version 23 lost entry required a version and had no repeatable field to set.
//
// TestRepeatableMigrationsBreakHoldsForEveryVersion23Object evaluates the
// rules against those shapes.
var repeatableMigrationsBreak = declaredBreak{
	version: 24,
	reason: "a migration plan may carry repeatable migrations, alone or after its versions, and a lost migration " +
		"log entry may name one; the rules that a plan carries one of the two lists and that a lost entry names " +
		"a version or a repeatable migration hold for every object a version 23 schema admitted, each of which " +
		"carried a non-empty migrations list or a version",
	transitions: repeatableMigrationsTransitions(),
}

// The rules schema version 24 adds, as the API types declare them.
const (
	rulePlanCarriesWork       = "has(self.migrations) || has(self.repeatables)"
	ruleLossNamesOneMigration = "has(self.version) != has(self.repeatable)"
)

func repeatableMigrationsTransitions() []string {
	const changed = ": x-kubernetes-validations gained or changed the rule "
	return []string{
		"ptahmigrationplans.operator.ptah.run: spec" + changed + strconv.Quote(rulePlanCarriesWork),
		"ptahmigrations.operator.ptah.run: status.journal.entries.[].migrationLog.losses.[]" + changed +
			strconv.Quote(ruleLossNamesOneMigration),
		"ptahmigrations.operator.ptah.run: status.lastRun.migrationLog.losses.[]" + changed +
			strconv.Quote(ruleLossNamesOneMigration),
		"ptahschemas.operator.ptah.run: status.journal.entries.[].migrationLog.losses.[]" + changed +
			strconv.Quote(ruleLossNamesOneMigration),
	}
}
