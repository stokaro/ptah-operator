package crdschemahistory

// The executor moved to Ptah v0.13.0, whose drift reports name 31 categories
// the earlier pin did not: PtahSchema's status enum takes them, and its summary
// bound rises to 128 so the whole vocabulary fits. No other CRD changes, and no
// published release stored either schema, so the contract stays at 1. After the
// first tag, schema changes still require a version advance.
const (
	driftVocabularyBeforeDigest = "sha256:b748c135a91ab9f47252aba72d8b556d301e2cec6d505c3caa4ed6f2eb8736fe"
	driftVocabularyAfterDigest  = "sha256:ce3071b2d2ed9f1f5dc9587a5e9dcc45ab5bfab2f415b2c7aa29544ca8e910c6"
)

// driftVocabularyDevelopmentExtension admits exactly that transition: every CRD
// at version 1 on both sides, PtahSchema from the one digest to the other, and
// every other spec unchanged.
func driftVocabularyDevelopmentExtension(baseline, candidate documentSet) bool {
	if len(baseline.byName) != len(candidate.byName) {
		return false
	}
	for name, before := range baseline.byName {
		after, found := candidate.byName[name]
		if !found || before.crd.Annotations[schemaVersionAnnotation] != "1" || after.crd.Annotations[schemaVersionAnnotation] != "1" {
			return false
		}
		beforeDigest, afterDigest := digestSpec(before.normalizedSpec), digestSpec(after.normalizedSpec)
		if name == "ptahschemas.operator.ptah.run" {
			if beforeDigest != driftVocabularyBeforeDigest || afterDigest != driftVocabularyAfterDigest {
				return false
			}
		} else if beforeDigest != afterDigest {
			return false
		}
	}
	return true
}
