package e2e

import (
	"encoding/json"
	"os"
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

// The accepted reading is the status the row matched on Kubernetes 1.35.8. Each
// refused reading is that status with the one field the mistake would change.
func TestDroppedExtensionObservedRefusesEveryOtherReading(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("../../testdata/e2e/readings/postgresql-extension-drift.json")
	if err != nil {
		t.Fatal(err)
	}
	native := &ptahv1alpha1.PtahSchema{}
	if err := json.Unmarshal(body, native); err != nil {
		t.Fatal(err)
	}
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
