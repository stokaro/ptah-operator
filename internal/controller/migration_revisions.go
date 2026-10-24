package controller

import (
	"cmp"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// revisionRows are the revision rows the history read found, newest first,
// bounded so status stays the same size however long the history grows. A
// migration the database holds no row for is not one; the row of a modified
// migration carries the checksum it recorded, not the file's.
func revisionRows(records []dataplane.MigrationRecord) []operatorv1alpha1.MigrationRevisionStatus {
	rows := make([]operatorv1alpha1.MigrationRevisionStatus, 0, len(records))
	for _, record := range records {
		if !record.HasRevisionRow() {
			continue
		}
		row := operatorv1alpha1.MigrationRevisionStatus{
			Version:     record.Version,
			Description: record.Description,
			Checksum:    record.AppliedChecksum,
			State:       operatorv1alpha1.MigrationRevisionState(record.State),
		}
		if record.AppliedAt != nil {
			appliedAt := metav1.NewTime(*record.AppliedAt)
			row.AppliedAt = &appliedAt
		}
		if record.ExecutionMs != nil {
			milliseconds := *record.ExecutionMs
			row.ExecutionMillis = &milliseconds
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil
	}
	slices.SortFunc(rows, func(a, b operatorv1alpha1.MigrationRevisionStatus) int {
		return cmp.Compare(b.Version, a.Version)
	})
	return rows[:min(len(rows), operatorv1alpha1.MaxMigrationRevisions)]
}
