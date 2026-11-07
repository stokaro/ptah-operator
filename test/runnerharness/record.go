package runnerharness

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// RunLabel marks the network a run creates first and removes last. Its value is
// the run's RunRecord, so a network that outlived its run says what is left of
// that run and when nothing can still be using it.
const RunLabel = "operator.ptah.run/runner-harness"

// RunRecord is what a later run needs to clear a run that died and whose
// janitor could not finish.
type RunRecord struct {
	// Prefix starts the name of everything the run made.
	Prefix string `json:"prefix"`
	// Expires is a time after the run's own deadline. Until then the run may
	// still be alive, so another run on the same daemon leaves it alone.
	Expires time.Time `json:"expires"`
	// Pulled names the images the host did not hold when the run started.
	Pulled []string `json:"pulled,omitempty"`
}

// Docker names start with a letter or a digit, and a prefix is used unquoted in
// an anchored name filter, so anything else could match another owner's objects.
var runPrefix = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ParseRunRecord reads the label's value. It refuses a record that does not say
// whose objects these are or when they expire, so a sweep removes nothing on the
// strength of it.
func ParseRunRecord(label string) (RunRecord, error) {
	var record RunRecord
	if err := json.Unmarshal([]byte(label), &record); err != nil {
		return RunRecord{}, fmt.Errorf("read the run's record: %w", err)
	}
	if !runPrefix.MatchString(record.Prefix) {
		return RunRecord{}, fmt.Errorf("the run's record names the prefix %q", record.Prefix)
	}
	if record.Expires.IsZero() {
		return RunRecord{}, errors.New("the run's record has no expiry")
	}
	return record, nil
}

// Label is the record as the label's value.
func (r RunRecord) Label() (string, error) {
	encoded, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("write the run's record: %w", err)
	}
	return string(encoded), nil
}

// Expired says that no live run can still be using what the record names.
func (r RunRecord) Expired(now time.Time) bool {
	return now.After(r.Expires)
}
