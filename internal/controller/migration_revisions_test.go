package controller

import (
	"fmt"
	"testing"
	"time"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// The revision rows status keeps are the newest the database holds, newest
// first, and no more than the bound: a modified row says what the row
// recorded rather than what its file now hashes to, the dirty row counts as
// the newest it is, and a migration with no row is not one.
func TestRevisionRowsAreTheNewestRowsTheDatabaseHolds(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 10, 24, 7, 0, 0, 0, time.UTC)
	ranAt := func(version int64) *time.Time {
		at := first.Add(time.Duration(version) * time.Minute)
		return &at
	}
	took := func(version int64) *int64 {
		milliseconds := version * 10
		return &milliseconds
	}
	records := make([]dataplane.MigrationRecord, 0, 72)
	for version := int64(1); version <= 70; version++ {
		records = append(records, dataplane.MigrationRecord{
			Version: version, Description: fmt.Sprintf("step %d", version),
			Checksum: "h1:file", AppliedChecksum: fmt.Sprintf("h1:row-%d", version),
			AppliedAt: ranAt(version), ExecutionMs: took(version), State: dataplane.MigrationStateApplied,
		})
	}
	records[69].State = dataplane.MigrationStateModified
	records = append(records,
		dataplane.MigrationRecord{Version: 71, Checksum: "h1:file", AppliedAt: ranAt(71), State: dataplane.MigrationStateDirty},
		dataplane.MigrationRecord{Version: 72, Checksum: "h1:file", State: dataplane.MigrationStatePending},
	)

	rows := revisionRows(records)

	if len(rows) != operatorv1alpha1.MaxMigrationRevisions {
		t.Fatalf("kept %d rows, want %d", len(rows), operatorv1alpha1.MaxMigrationRevisions)
	}
	for index, row := range rows {
		version := int64(71 - index)
		if row.Version != version {
			t.Fatalf("row %d is version %d, want %d: newest first", index, row.Version, version)
		}
		if row.AppliedAt == nil || !row.AppliedAt.Time.Equal(*ranAt(version)) {
			t.Fatalf("version %d was written at %v, want %v", version, row.AppliedAt, ranAt(version))
		}
	}
	if dirty := rows[0]; dirty.State != operatorv1alpha1.MigrationRevisionDirty || dirty.Checksum != "" || dirty.ExecutionMillis != nil {
		t.Fatalf("the dirty row reads %+v, want state dirty with no checksum and no duration", dirty)
	}
	if modified := rows[1]; modified.State != operatorv1alpha1.MigrationRevisionModified || modified.Checksum != "h1:row-70" {
		t.Fatalf("the modified row reads %+v, want the checksum the row recorded", modified)
	}
	if applied := rows[2]; applied.State != operatorv1alpha1.MigrationRevisionApplied ||
		applied.Description != "step 69" || applied.ExecutionMillis == nil || *applied.ExecutionMillis != 690 {
		t.Fatalf("version 69 reads %+v, want its description and 690 ms", applied)
	}
	if last := rows[len(rows)-1]; last.Version != 8 {
		t.Fatalf("the oldest row kept is version %d, want 8", last.Version)
	}
}

// A history whose migrations have no rows publishes no revisions at all, not
// an empty list.
func TestRevisionRowsOfAHistoryWithNoRowsAreAbsent(t *testing.T) {
	t.Parallel()
	rows := revisionRows([]dataplane.MigrationRecord{
		{Version: 1, Checksum: "h1:one", State: dataplane.MigrationStateCheckpointCovered},
		{Version: 2, Checksum: "h1:two", State: dataplane.MigrationStatePending},
		{Version: 3, Checksum: "h1:three", State: dataplane.MigrationStateOutOfOrder},
	})

	if rows != nil {
		t.Fatalf("rows = %+v, want none", rows)
	}
}
