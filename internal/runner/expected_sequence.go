package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

// approvedSequenceDocument turns the sequence a migration Apply Job carries
// into the file `migrations up --expect-sequence` reads.
//
// The sequence is refused unless it digests to the value the plan bound, so a
// Job that carried one list and another list's digest runs nothing. The file
// names each migration by version and key only, which is what Ptah compares
// against its selection; the checksums were already bound by the digest, and
// Ptah verifies the files it runs against the revision table on its own.
func approvedSequenceDocument(encoded, expectedDigest string) ([]byte, error) {
	if encoded == "" {
		return nil, errors.New("the approved migration sequence is required")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(encoded)))
	decoder.DisallowUnknownFields()
	var entries []fingerprint.SequenceEntry
	if err := decoder.Decode(&entries); err != nil {
		return nil, fmt.Errorf("the approved migration sequence is not readable: %w", err)
	}
	if decoder.More() {
		return nil, errors.New("the approved migration sequence holds more than one document")
	}
	digest, err := fingerprint.MigrationSequenceDigest(entries)
	if err != nil {
		return nil, err
	}
	if digest != expectedDigest {
		return nil, errors.New("the approved migration sequence does not match the digest its plan bound")
	}
	type migration struct {
		Version    int64  `json:"version"`
		VersionKey string `json:"version_key,omitempty"`
	}
	document := struct {
		Migrations []migration `json:"migrations"`
	}{Migrations: make([]migration, 0, len(entries))}
	for _, entry := range entries {
		document.Migrations = append(document.Migrations, migration{Version: entry.Version, VersionKey: entry.VersionKey})
	}
	return json.Marshal(document)
}
