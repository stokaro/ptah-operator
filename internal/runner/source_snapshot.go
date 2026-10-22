package runner

import (
	"fmt"

	"github.com/stokaro/ptah-operator/internal/sourcecontent"
)

// CodeSourceSnapshotMismatch is the refusal of a source snapshot that does not
// hold the content its operation was bound to.
const CodeSourceSnapshotMismatch = "source_snapshot_mismatch"

// MaterializeSourceSnapshot checks the ConfigMap source snapshot mounted at
// snapshotDir against contentDigest and copies its files into destination. It
// runs in an init container, before the container that holds the database URL
// starts, so that container reads only files that give the bound digest. It
// opens no network connection and reads no credential.
func MaterializeSourceSnapshot(snapshotDir, destination, contentDigest string) error {
	if err := sourcecontent.Materialize(snapshotDir, destination, contentDigest); err != nil {
		return fmt.Errorf("%s: %w", CodeSourceSnapshotMismatch, err)
	}
	return nil
}
