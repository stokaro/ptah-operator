package crdschemahistory

import (
	"context"
	"testing"
)

// developmentTreeCommit is master before the drift vocabulary grew: the last
// tree whose CRDs are exactly the ones the result-record transitions name.
// Reading it from history keeps those rows about their own transition when a
// later development change edits a CRD they do not cover.
const developmentTreeCommit = "81208488a8c549b6a635ba2342adf1900ce95706"

func TestResultRecordDevelopmentAdditionIsExact(t *testing.T) {
	files, err := readGitFiles(context.Background(), "../..", developmentTreeCommit, "config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"exact addition", "changed existing schema", "changed existing baseline and candidate", "changed record schema", "another version"} {
		t.Run(change, func(t *testing.T) {
			candidate, err := decodeSet("candidate", files)
			if err != nil {
				t.Fatal(err)
			}
			baseline, err := decodeSet("baseline", files)
			if err != nil {
				t.Fatal(err)
			}
			delete(baseline.byName, "ptahresultrecords.operator.ptah.run")
			mutate := func(set documentSet, name string) {
				d := set.byName[name]
				d.crd.Spec.Names.ShortNames = append(d.crd.Spec.Names.ShortNames, "changed")
				d.normalizedSpec, err = normalizeSpec(d.crd)
				if err != nil {
					t.Fatal(err)
				}
				d.crd.Annotations[schemaDigestAnnotation] = digestSpec(d.normalizedSpec)
				set.byName[name] = d
			}
			switch change {
			case "changed existing schema":
				mutate(candidate, "ptahschemas.operator.ptah.run")
			case "changed existing baseline and candidate":
				mutate(baseline, "ptahschemas.operator.ptah.run")
				mutate(candidate, "ptahschemas.operator.ptah.run")
			case "changed record schema":
				mutate(candidate, "ptahresultrecords.operator.ptah.run")
			case "another version":
				for _, set := range []documentSet{baseline, candidate} {
					for _, d := range set.byName {
						d.crd.Annotations[schemaVersionAnnotation] = "2"
					}
				}
			}
			result, err := evaluateTransition(baseline, candidate)
			if change == "exact addition" {
				if err != nil || !result.SchemaChanged || result.CandidateVersion != 1 {
					t.Fatalf("exact development addition refused: %#v %v", result, err)
				}
			} else if err == nil {
				t.Fatal("development exception admitted another transition")
			}
		})
	}
}

func TestResultRecordRetirementTransitionIsExact(t *testing.T) {
	files, err := readGitFiles(context.Background(), "../..", developmentTreeCommit, "config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"exact retirement role", "another baseline role", "another candidate role", "changed existing CRD"} {
		t.Run(variant, func(t *testing.T) {
			baseline, err := decodeSet("baseline", files)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := decodeSet("candidate", files)
			if err != nil {
				t.Fatal(err)
			}
			mutate := func(set documentSet, name string, change func(*document)) {
				d := set.byName[name]
				change(&d)
				d.normalizedSpec, err = normalizeSpec(d.crd)
				if err != nil {
					t.Fatal(err)
				}
				d.crd.Annotations[schemaDigestAnnotation] = digestSpec(d.normalizedSpec)
				set.byName[name] = d
			}
			const name = "ptahresultrecords.operator.ptah.run"
			removeRetired := func(d *document) {
				schema := d.crd.Spec.Versions[0].Schema.OpenAPIV3Schema
				spec := schema.Properties["spec"]
				role := spec.Properties["type"]
				if len(role.Enum) != 5 || string(role.Enum[4].Raw) != `"retired"` {
					t.Fatal("fixture is not the reviewed retirement-role addition")
				}
				role.Enum = role.Enum[:4]
				spec.Properties["type"] = role
				schema.Properties["spec"] = spec
			}
			mutate(baseline, name, removeRetired)
			if digestSpec(baseline.byName[name].normalizedSpec) != resultRecordBeforeRetirementDigest {
				t.Fatal("baseline does not reproduce the preceding result-record schema")
			}
			changeName := func(d *document) { d.crd.Spec.Names.ShortNames = append(d.crd.Spec.Names.ShortNames, "other") }
			switch variant {
			case "another baseline role":
				mutate(baseline, name, changeName)
			case "another candidate role":
				mutate(candidate, name, changeName)
			case "changed existing CRD":
				mutate(candidate, "ptahschemas.operator.ptah.run", changeName)
			}
			_, err = evaluateTransition(baseline, candidate)
			if (err == nil) != (variant == "exact retirement role") {
				t.Fatalf("variant %s: %v", variant, err)
			}
		})
	}
}
