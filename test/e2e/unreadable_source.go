package e2e

import (
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// What the rows deliver that Ptah cannot read, and how Ptah words each reason.
const (
	// unreadablePartitionSchema is the file stokaro/ptah-operator#788 reports:
	// the parser reads no PARTITION OF.
	unreadablePartitionSchema = `CREATE TABLE events (
    id bigint NOT NULL,
    happened_at timestamptz NOT NULL,
    PRIMARY KEY (id, happened_at)
) PARTITION BY RANGE (happened_at);

CREATE TABLE events_2026 PARTITION OF events
    FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
`
	unreadablePartitionFile   = "schema.sql"
	unreadablePartitionReason = "expected '(' after table name"

	unreadableMigrationFile   = "0000000001_create_events.up.sql"
	unreadableMigrationReason = `invalid +ptah no_transaction value "sometimes"`
)

// unreadableMigrationFiles is a migration directory the loader refuses: the
// directive's value is neither true nor false.
var unreadableMigrationFiles = []struct{ key, path, body string }{
	{"0001-up", unreadableMigrationFile, "-- +ptah no_transaction=sometimes\nCREATE TABLE e2e_unreadable_events (id bigint);\n"},
	{"0001-down", "0000000001_create_events.down.sql", "DROP TABLE e2e_unreadable_events;\n"},
}

// unreadableSourceSays holds a condition's or an Event's message to the claim:
// the result code that means the source could not be read, the file by name,
// and Ptah's reason. Reading the code alone would pass on a message with no
// reason in it, and the exit status it replaced names neither.
func unreadableSourceSays(message, file, reason string) error {
	switch {
	case !strings.Contains(message, runner.CodeDesiredSourceUnreadable+": "):
		return fmt.Errorf("the message does not carry %s: %q", runner.CodeDesiredSourceUnreadable, message)
	case !strings.Contains(message, file):
		return fmt.Errorf("the message does not name file %s: %q", file, message)
	case !strings.Contains(message, reason):
		return fmt.Errorf("the message does not say %q: %q", reason, message)
	}
	return nil
}

// unreadableSourceEvent finds the Warning Event a failure wrote on one resource
// with the reason in it. It reports what it examined, so a list with no Event of
// that resource and that reason reads differently from one that says something
// else.
func unreadableSourceEvent(events []corev1.Event, owner types.UID, reason, file, want string) error {
	examined := 0
	var last error
	for index := range events {
		event := &events[index]
		if event.InvolvedObject.UID != owner || event.Reason != reason || event.Type != corev1.EventTypeWarning {
			continue
		}
		examined++
		if last = unreadableSourceSays(event.Message, file, want); last == nil {
			return nil
		}
	}
	if examined == 0 {
		return errors.New("no Warning " + reason + " Event of the resource yet")
	}
	return fmt.Errorf("%d Warning %s Events, none says why: %w", examined, reason, last)
}
