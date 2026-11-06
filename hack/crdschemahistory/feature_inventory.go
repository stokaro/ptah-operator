package crdschemahistory

import "strconv"

// Since Ptah v0.18.0 a drift report's inventory also counts what a feature
// owner compares, under the kind its qualified categories carry (#795).
// Schema version 10 keeps those kinds in status: the pattern admits a stem or a
// feature kind, and the bound rises from 64 to 128 bytes.
//
// No stored value stops validating: every stem the old pattern admitted is in
// the new one, which TestFeatureInventoryBreakWidensTheKind holds. The check
// cannot prove that for two arbitrary patterns, so the transition is declared
// here.
var featureInventoryBreak = declaredBreak{
	version: 10,
	reason:  "an inventory entry may name a feature kind; the pattern still admits every stem it did",
	transitions: []string{
		"ptahschemas.operator.ptah.run: status.target.inventory.[].kind: the pattern changed from " +
			strconv.Quote(inventoryKindPatternStems) + " to " + strconv.Quote(inventoryKindPatternWithFeatures),
	},
}

const (
	// inventoryKindPatternStems is the status pattern at schema version 9.
	inventoryKindPatternStems = `^[a-z]([a-z_]*[a-z])?$`
	// inventoryKindPatternWithFeatures is the status pattern at schema version 10.
	inventoryKindPatternWithFeatures = `^([a-z]([a-z_]*[a-z])?|[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(/[A-Za-z0-9_-][A-Za-z0-9._-]*)+)$`
)
