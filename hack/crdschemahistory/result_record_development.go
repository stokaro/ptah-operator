package crdschemahistory

// resultRecordDevelopmentBase pins the complete unreleased schema-1 tree before
// durable result records were added for 0.2.0. No existing CRD changes in this
// transition, and no published release stored either tree. The second exact
// transition adds the retired role to the initial result-record schema. Keep the initial
// contract at 1 without exempting any other schema change from the history gate.
// After the first tag, ordinary schema changes still require a version advance.
var resultRecordDevelopmentBase = map[string]string{
	"ptahmigrationapprovals.operator.ptah.run":          "sha256:9391303b4f722d892389f53e9f8f4f7853e77547d9bdf4c02202c2801382c897",
	"ptahmigrationplans.operator.ptah.run":              "sha256:bd3dd4a8c79182fec93e420242d2246aabf1d5e5fb78898c369a944ad489a245",
	"ptahmigrationrunacknowledgments.operator.ptah.run": "sha256:158a67e8ea4e64b1cbe7678abb49bafd0eb807f4672a0ec4e540c99332e6c49f",
	"ptahmigrations.operator.ptah.run":                  "sha256:681554fea2f34a5565c9801608b4148614f110083d5b5ca02aeafe3bfe2596a0",
	"ptahrealms.operator.ptah.run":                      "sha256:8c001146bf02b378911dae351760f511fc465d7dafcda94e849b14d5b329d2fa",
	"ptahschemaapprovals.operator.ptah.run":             "sha256:269e21ffd731b1eb2d6f0964122f7eead2f1d29ecc6e550df5740e65ae1ef663",
	"ptahschemaplanchunks.operator.ptah.run":            "sha256:f762f55f2bdd3d7766ce5962a1a35f074ada6992016f454e26e4b06dbb746d0f",
	"ptahschemaplans.operator.ptah.run":                 "sha256:f99b633652aa4d3cf2007db93cc043ed9889fad39e6048588eb984131e86c600",
	"ptahschemas.operator.ptah.run":                     "sha256:b748c135a91ab9f47252aba72d8b556d301e2cec6d505c3caa4ed6f2eb8736fe",
}

const resultRecordDevelopmentDigest = "sha256:edd0a51e21a0d2d59ad6e762079888fb696ae180ed940eeea10cac675f79c61e"
const resultRecordBeforeRetirementDigest = "sha256:e12cd230da4d27c306ed813eabed59995f464d909474a7412fc537e8063a8f6b"

func resultRecordDevelopmentAddition(baseline, candidate documentSet) bool {
	if len(candidate.byName) != len(resultRecordDevelopmentBase)+1 {
		return false
	}
	switch len(baseline.byName) {
	case len(resultRecordDevelopmentBase):
		// The initial, unreleased addition of durable result records.
	case len(resultRecordDevelopmentBase) + 1:
		before, found := baseline.byName["ptahresultrecords.operator.ptah.run"]
		if !found || before.crd.Annotations[schemaVersionAnnotation] != "1" || digestSpec(before.normalizedSpec) != resultRecordBeforeRetirementDigest {
			return false
		}
	default:
		return false
	}
	for name, expected := range resultRecordDevelopmentBase {
		before, found := baseline.byName[name]
		if !found {
			return false
		}
		after, found := candidate.byName[name]
		if !found {
			return false
		}
		if before.crd.Annotations[schemaVersionAnnotation] != "1" || after.crd.Annotations[schemaVersionAnnotation] != "1" || digestSpec(before.normalizedSpec) != expected || digestSpec(after.normalizedSpec) != expected {
			return false
		}
	}
	added, found := candidate.byName["ptahresultrecords.operator.ptah.run"]
	return found && added.crd.Annotations[schemaVersionAnnotation] == "1" && digestSpec(added.normalizedSpec) == resultRecordDevelopmentDigest
}
