package crdschemahistory

import (
	"context"
	"testing"
)

func TestDriftVocabularyDevelopmentExtensionIsExact(t *testing.T) {
	before, err := readGitFiles(context.Background(), "../..", developmentTreeCommit, "config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	after, err := readWorkingTreeFiles("../..", "config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"exact extension", "changed neighbour", "changed PtahSchema", "another version"} {
		t.Run(change, func(t *testing.T) {
			baseline, err := decodeSet("baseline", before)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := decodeSet("candidate", after)
			if err != nil {
				t.Fatal(err)
			}
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
			case "changed neighbour":
				mutate(candidate, "ptahschemaplans.operator.ptah.run")
			case "changed PtahSchema":
				mutate(candidate, "ptahschemas.operator.ptah.run")
			case "another version":
				for _, set := range []documentSet{baseline, candidate} {
					for _, d := range set.byName {
						d.crd.Annotations[schemaVersionAnnotation] = "2"
					}
				}
			}
			result, err := evaluateTransition(baseline, candidate)
			if change == "exact extension" {
				if err != nil || !result.SchemaChanged || result.CandidateVersion != 1 {
					t.Fatalf("exact drift vocabulary extension refused: %#v %v", result, err)
				}
			} else if err == nil {
				t.Fatal("development exception admitted another transition")
			}
		})
	}
}
