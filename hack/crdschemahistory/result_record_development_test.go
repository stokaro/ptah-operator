package crdschemahistory

import (
	"testing"
)

func TestResultRecordDevelopmentAdditionIsExact(t *testing.T) {
	files, err := readWorkingTreeFiles("../..", "config/crd/bases")
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
