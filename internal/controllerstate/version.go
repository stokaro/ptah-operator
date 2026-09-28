// Package controllerstate defines the manager-side durable-state contract.
package controllerstate

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRevisionBytes is the maximum encoded size of manager build provenance.
const MaxRevisionBytes = 128

// CurrentVersion is the newest PtahSchema controller-state version this
// manager can safely interpret and write.
const CurrentVersion int32 = 1

// ValidateRevision checks the manager build provenance stored in durable
// execution identity. Interior whitespace is allowed for custom build labels,
// but edge whitespace, control characters, invalid UTF-8, and oversized values
// are refused so annotations, logs, and API evidence cannot be ambiguous.
func ValidateRevision(revision string) error {
	if revision == "" {
		return errors.New("controller revision is required")
	}
	if len(revision) > MaxRevisionBytes {
		return fmt.Errorf("controller revision must be at most %d bytes", MaxRevisionBytes)
	}
	if !utf8.ValidString(revision) {
		return errors.New("controller revision must be valid UTF-8")
	}
	if strings.TrimSpace(revision) != revision {
		return errors.New("controller revision must not have edge whitespace")
	}
	for _, character := range revision {
		if unicode.IsControl(character) {
			return errors.New("controller revision must not contain control characters")
		}
	}
	return nil
}
