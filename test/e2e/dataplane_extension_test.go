package e2e

import (
	"encoding/json"
	"os"
	"testing"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const extensionDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func extensionSchema(t *testing.T, document string) *ptahv1alpha1.PtahSchema {
	t.Helper()
	schema := &ptahv1alpha1.PtahSchema{}
	if err := json.Unmarshal([]byte(document), schema); err != nil {
		t.Fatalf("decode %s: %v", document, err)
	}
	return schema
}

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

func TestDroppedExtensionObservedRefusesEveryOtherReading(t *testing.T) {
	t.Parallel()
	const dropped = `{"status":{"source":{"digest":"` + extensionDigest + `"},"target":{"highestDriftSeverity":"safe",
  "driftFindingCount":1,"driftFindings":[{"category":"feature_changes:ptah.run/pgextension/extension-change","count":1,"severity":"safe"}]}}}`
	if !droppedExtensionObserved(extensionSchema(t, dropped), extensionDigest) {
		t.Fatal("the reading of the dropped extension was refused")
	}
	for _, reading := range []struct{ name, document string }{
		{"the converged schema, which found nothing", `{"status":{"source":{"digest":"` + extensionDigest + `"},"target":{}}}`},
		{"the observation before the first Apply, which also lacked the table", `{"status":{"source":{"digest":"` + extensionDigest + `"},
  "target":{"highestDriftSeverity":"safe","driftFindingCount":2,"driftFindings":[
  {"category":"feature_changes:ptah.run/pgextension/extension-change","count":1,"severity":"safe"},
  {"category":"tables_added","count":1,"severity":"safe"}]}}}`},
		{"the category Ptah v0.13.0 wrote", `{"status":{"source":{"digest":"` + extensionDigest + `"},"target":{"highestDriftSeverity":"safe",
  "driftFindingCount":1,"driftFindings":[{"category":"extensions_added","count":1,"severity":"safe"}]}}}`},
		{"the category without its kind", `{"status":{"source":{"digest":"` + extensionDigest + `"},"target":{"highestDriftSeverity":"safe",
  "driftFindingCount":1,"driftFindings":[{"category":"feature_changes","count":1,"severity":"safe"}]}}}`},
		{"another kind of the same owner", `{"status":{"source":{"digest":"` + extensionDigest + `"},"target":{"highestDriftSeverity":"safe",
  "driftFindingCount":1,"driftFindings":[{"category":"feature_changes:ptah.run/pgextension/extension","count":1,"severity":"safe"}]}}}`},
		{"two extensions", `{"status":{"source":{"digest":"` + extensionDigest + `"},"target":{"highestDriftSeverity":"safe",
  "driftFindingCount":2,"driftFindings":[{"category":"feature_changes:ptah.run/pgextension/extension-change","count":2,"severity":"safe"}]}}}`},
		{"a finding counted twice in the total", `{"status":{"source":{"digest":"` + extensionDigest + `"},"target":{"highestDriftSeverity":"safe",
  "driftFindingCount":2,"driftFindings":[{"category":"feature_changes:ptah.run/pgextension/extension-change","count":1,"severity":"safe"}]}}}`},
		{"another severity", `{"status":{"source":{"digest":"` + extensionDigest + `"},"target":{"highestDriftSeverity":"warning",
  "driftFindingCount":1,"driftFindings":[{"category":"feature_changes:ptah.run/pgextension/extension-change","count":1,"severity":"warning"}]}}}`},
		{"another artifact", `{"status":{"source":{"digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222"},
  "target":{"highestDriftSeverity":"safe","driftFindingCount":1,
  "driftFindings":[{"category":"feature_changes:ptah.run/pgextension/extension-change","count":1,"severity":"safe"}]}}}`},
	} {
		t.Run(reading.name, func(t *testing.T) {
			t.Parallel()
			if droppedExtensionObserved(extensionSchema(t, reading.document), extensionDigest) {
				t.Errorf("a reading of %s was accepted", reading.name)
			}
		})
	}
}
