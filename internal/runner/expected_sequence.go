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
//
// The repeatable migrations travel in a variable of their own, which a plan
// that selected none leaves unset, and the one digest binds both lists. Ptah
// compares each repeatable migration by identity and the digest of the content
// it would run, in order, so the file names both. A file without them asks Ptah
// for no repeatable migration, and Ptah refuses a run that would execute one.
func approvedSequenceDocument(encoded, encodedRepeatables, expectedDigest string) ([]byte, error) {
	if encoded == "" {
		return nil, errors.New("the approved migration sequence is required")
	}
	var entries []fingerprint.SequenceEntry
	if err := decodeApprovedList(encoded, &entries); err != nil {
		return nil, fmt.Errorf("the approved migration sequence %w", err)
	}
	var repeatables []fingerprint.RepeatableEntry
	if encodedRepeatables != "" {
		if err := decodeApprovedList(encodedRepeatables, &repeatables); err != nil {
			return nil, fmt.Errorf("the approved repeatable migrations %w", err)
		}
		// A variable that is set names at least one, because the Job leaves
		// it off for a plan without them; an empty list would digest as a
		// plan without them and still reach Ptah as a second shape.
		if len(repeatables) == 0 {
			return nil, errors.New("the approved repeatable migrations are an empty list")
		}
	}
	digest, err := fingerprint.MigrationSequenceDigest(entries, repeatables)
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
	type repeatable struct {
		Identity string `json:"identity"`
		Checksum string `json:"checksum"`
	}
	document := struct {
		Migrations  []migration  `json:"migrations"`
		Repeatables []repeatable `json:"repeatables,omitempty"`
	}{Migrations: make([]migration, 0, len(entries))}
	for _, entry := range entries {
		document.Migrations = append(document.Migrations, migration{Version: entry.Version, VersionKey: entry.VersionKey})
	}
	for _, entry := range repeatables {
		document.Repeatables = append(document.Repeatables, repeatable{Identity: entry.Identity, Checksum: entry.Checksum})
	}
	return json.Marshal(document)
}

// decodeApprovedList reads one list of the approved sequence strictly: one
// JSON array and nothing after it, with no field the digest does not bind.
func decodeApprovedList[T any](encoded string, target *[]T) error {
	decoder := json.NewDecoder(bytes.NewReader([]byte(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("is not readable: %w", err)
	}
	if decoder.More() {
		return errors.New("holds more than one document")
	}
	return nil
}
