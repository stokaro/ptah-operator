package e2e

import (
	"slices"
	"testing"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// Each refused reading is the accepted one with what the mistake would change.
// The first two are what Ptah v0.18.0 reported on PostgreSQL 17.11: the
// documented login found nothing, and the same login with pg_read_all_data
// found the table without its primary key. The accepted one is
// testdata/e2e/readings/postgresql-login-visibility-drift.json, the status the row
// printed on kind, and the README beside it says in which run.
func TestUnreadableTableObservedRefusesEveryOtherReading(t *testing.T) {
	t.Parallel()
	finding := func(category string, count int32, severity string) ptahv1alpha1.DriftFindingStatus {
		return ptahv1alpha1.DriftFindingStatus{Category: category, Count: count, Severity: severity}
	}
	withoutKind := func(status *ptahv1alpha1.PtahSchemaStatus, kind string) {
		status.Target.Inventory = slices.DeleteFunc(status.Target.Inventory, func(entry ptahv1alpha1.ObjectInventoryStatus) bool {
			return entry.Kind == kind
		})
	}
	accepted := readSchemaReading(t, "postgresql-login-visibility-drift.json")
	digest := accepted.Status.Source.Digest
	if digest == "" || !unreadableTableObserved(accepted, digest) {
		t.Fatal("the reading of the table and its primary key was refused")
	}
	for _, reading := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchemaStatus)
	}{
		{"the documented login at Ptah v0.18.0, which found nothing", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.HighestDriftSeverity, s.Target.DriftFindingCount, s.Target.DriftFindings = "", 0, nil
			s.Target.Inventory = []ptahv1alpha1.ObjectInventoryStatus{{Kind: "tables", Desired: 1, Observed: 1}}
		}},
		{"the login with pg_read_all_data, which found no constraint", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.DriftFindingCount = 1
			s.Target.DriftFindings = []ptahv1alpha1.DriftFindingStatus{finding("tables_removed", 1, "destructive")}
		}},
		{"the constraint without its table", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.DriftFindingCount = 1
			s.Target.DriftFindings = []ptahv1alpha1.DriftFindingStatus{finding("constraints_removed", 1, "destructive")}
		}},
		{"the observation before the first Apply, which found the declared table missing", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.HighestDriftSeverity, s.Target.DriftFindingCount = "safe", 1
			s.Target.DriftFindings = []ptahv1alpha1.DriftFindingStatus{finding("tables_added", 1, "safe")}
			s.Target.Inventory = []ptahv1alpha1.ObjectInventoryStatus{{Kind: "tables", Desired: 1, Observed: 0}}
		}},
		{"a declared table the login reads as missing", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.DriftFindingCount = 3
			s.Target.DriftFindings = append(s.Target.DriftFindings, finding("tables_added", 1, "safe"))
		}},
		{"a second table", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.DriftFindingCount = 3
			s.Target.DriftFindings[1].Count = 2
		}},
		{"another object beside the table", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.DriftFindingCount = 3
			s.Target.DriftFindings = append(s.Target.DriftFindings, finding("views_removed", 1, "destructive"))
		}},
		{"a total that does not add up", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.DriftFindingCount = 3
		}},
		{"a table rated below destructive", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.DriftFindings[1].Severity = "warning"
		}},
		{"a highest severity below the findings", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Target.HighestDriftSeverity = "safe"
		}},
		{"an inventory that does not count the table", func(s *ptahv1alpha1.PtahSchemaStatus) {
			withoutKind(s, "tables")
		}},
		{"an inventory that does not count the constraint", func(s *ptahv1alpha1.PtahSchemaStatus) {
			withoutKind(s, "constraints")
		}},
		{"another artifact", func(s *ptahv1alpha1.PtahSchemaStatus) {
			s.Source.Digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		}},
	} {
		t.Run(reading.name, func(t *testing.T) {
			t.Parallel()
			schema := accepted.DeepCopy()
			reading.mutate(&schema.Status)
			if unreadableTableObserved(schema, digest) {
				t.Errorf("a reading of %s was accepted", reading.name)
			}
		})
	}
}
