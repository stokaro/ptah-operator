package e2e

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The category the row waits for is the one Ptah v0.14.0 printed for an
// extension the database lacked, which the runner's own tests frame.
func TestPgExtensionChangeIsTheCategoryPtahPrinted(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("../../internal/dataplane/testdata/drift-feature-extension.json")
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Findings []struct {
			Category string `json:"category"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 || report.Findings[0].Category != pgExtensionChange {
		t.Fatalf("Ptah reported %+v, and the row waits for %s", report.Findings, pgExtensionChange)
	}
}

func readExtensionReading(t *testing.T, name string) *ptahv1alpha1.PtahSchema {
	t.Helper()
	body, err := os.ReadFile("../../testdata/e2e/readings/" + name)
	if err != nil {
		t.Fatal(err)
	}
	native := &ptahv1alpha1.PtahSchema{}
	if err := json.Unmarshal(body, native); err != nil {
		t.Fatal(err)
	}
	return native
}

// The derived reading counts what Ptah's report counts, entry by entry and in
// the order status keeps them, so it does not drift from the capture it was
// written from.
func TestExtensionReadingCountsWhatPtahPrinted(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("../../internal/dataplane/testdata/drift-feature-extension.json")
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Inventory []ptahv1alpha1.ObjectInventoryStatus `json:"inventory"`
	}
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Inventory) == 0 {
		t.Fatal("the captured report counts nothing, so this test would hold the reading to nothing")
	}
	reading := readExtensionReading(t, "postgresql-extension-drift-inventory.json")
	if !slices.Equal(reading.Status.Target.Inventory, report.Inventory) {
		t.Fatalf("the reading counts %+v, and Ptah reported %+v", reading.Status.Target.Inventory, report.Inventory)
	}
}

// The status the row matched on Kubernetes 1.35.8 with Ptah v0.14.0 has every
// other field the predicate asks for, and no inventory. It is what a run looks
// like when the entry does not reach status, so the predicate has to refuse it.
func TestDroppedExtensionObservedRefusesACapturedStatusWithoutTheEntry(t *testing.T) {
	t.Parallel()
	captured := readExtensionReading(t, "postgresql-extension-drift.json")
	digest := captured.Status.Source.Digest
	if digest == "" || captured.Status.Target.DriftFindingCount != 1 || len(captured.Status.Target.Inventory) != 0 {
		t.Fatalf("the captured reading is not the one this row is about: %+v", captured.Status.Target)
	}
	if droppedExtensionObserved(captured, digest) {
		t.Fatal("a status with no inventory entry for the extension was accepted")
	}
	withEntry := captured.DeepCopy()
	withEntry.Status.Target.Inventory = []ptahv1alpha1.ObjectInventoryStatus{{Kind: pgExtensionKind, Desired: 1}}
	if !droppedExtensionObserved(withEntry, digest) {
		t.Fatal("the entry was added and the status is still refused, so the refusal above is about something else")
	}
}

// The accepted reading is derived rather than read from a cluster:
// testdata/e2e/readings/README.md says from what. Each refused reading is that
// status with the one field the mistake would change.
func TestDroppedExtensionObservedRefusesEveryOtherReading(t *testing.T) {
	t.Parallel()
	native := readExtensionReading(t, "postgresql-extension-drift-inventory.json")
	digest := native.Status.Source.Digest
	if digest == "" || native.Status.Applied == nil || native.Status.Applied.ArtifactDigest != digest {
		t.Fatalf("the reading was not taken after the artifact %q was applied", digest)
	}
	if !droppedExtensionObserved(native, digest) {
		t.Fatal("the reading of the dropped extension was refused")
	}
	finding := func(category string, count int32, severity string) ptahv1alpha1.DriftFindingStatus {
		return ptahv1alpha1.DriftFindingStatus{Category: category, Count: count, Severity: severity}
	}
	for _, reading := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchemaStatus)
	}{
		{"the converged schema, which found nothing", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.HighestDriftSeverity, s.Target.DriftFindingCount, s.Target.DriftFindings = "", 0, nil
		}},
		{"the observation before the first Apply, which also lacked the table", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.DriftFindingCount = 2
			s.Target.DriftFindings = append(s.Target.DriftFindings, finding("tables_added", 1, "safe"))
		}},
		{"the category Ptah v0.13.0 wrote", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.DriftFindings[0].Category = "extensions_added"
		}},
		{"the category without its kind", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.DriftFindings[0].Category = "feature_changes"
		}},
		{"another kind of the same owner", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.DriftFindings[0].Category = "feature_changes:ptah.run/pgextension/extension"
		}},
		{"two extensions", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.DriftFindingCount, s.Target.DriftFindings[0].Count = 2, 2
		}},
		{"a finding counted twice in the total", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.DriftFindingCount = 2
		}},
		{"another severity", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.HighestDriftSeverity, s.Target.DriftFindings[0].Severity = "warning", "warning"
		}},
		{"another artifact", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Source.Digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		}},
		{"no inventory, from a Ptah that does not count", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.Inventory = nil
		}},
		{"an inventory without the entry, as the operator kept it before #795", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.Inventory = slices.DeleteFunc(s.Target.Inventory, func(entry ptahv1alpha1.ObjectInventoryStatus) bool {
				return entry.Kind == pgExtensionKind
			})
		}},
		{"the extension still in the database", func(s *ptahv1alpha1.PtahSchemaStatus) {
			setExtensionCounts(s, 1, 1)
		}},
		{"a desired schema that declares none", func(s *ptahv1alpha1.PtahSchemaStatus) {
			setExtensionCounts(s, 0, 0)
		}},
		{"two extensions declared", func(s *ptahv1alpha1.PtahSchemaStatus) {
			setExtensionCounts(s, 2, 0)
		}},
		{"an entry under another kind of the same owner", func(s *ptahv1alpha1.PtahSchemaStatus) {
			for index := range s.Target.Inventory {
				if s.Target.Inventory[index].Kind == pgExtensionKind {
					s.Target.Inventory[index].Kind = "ptah.run/pgextension/extension"
				}
			}
		}},
	} {
		t.Run(reading.name, func(t *testing.T) {
			t.Parallel()
			schema := native.DeepCopy()
			reading.mutate(&schema.Status)
			if droppedExtensionObserved(schema, digest) {
				t.Errorf("a reading of %s was accepted", reading.name)
			}
		})
	}
}

func setExtensionCounts(status *ptahv1alpha1.PtahSchemaStatus, desired, observed int32) {
	for index := range status.Target.Inventory {
		if status.Target.Inventory[index].Kind == pgExtensionKind {
			status.Target.Inventory[index].Desired, status.Target.Inventory[index].Observed = desired, observed
		}
	}
}
