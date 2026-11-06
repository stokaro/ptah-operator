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

	// unreadableFetchStep is where a schema artifact the executor cannot parse
	// stops, and unreadableFetchLogReason how Ptah words that in the step's log.
	unreadableFetchStep      = "fetch-schema"
	unreadableFetchLogReason = "parse schema artifact: parse HCL schema: schema.hcl:"
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

// fetchStepSays holds a condition's or an Event's message to what a failed fetch
// says: the step by name, and nothing the step wrote, since its container holds
// the registry credentials. A message carrying the reason would read better and
// break that boundary, so the row refuses one.
func fetchStepSays(message, step string) error {
	switch {
	case !strings.Contains(message, "the "+step+" step exited "):
		return fmt.Errorf("the message does not name step %s: %q", step, message)
	case !strings.Contains(message, "so the run never started"):
		return fmt.Errorf("the message does not say the run never started: %q", message)
	case strings.Contains(message, "parse schema artifact"), strings.Contains(message, "schema.hcl"):
		return fmt.Errorf("the message carries what the step wrote: %q", message)
	}
	return nil
}

// fetchStepLogSays holds the step's log to the reason status leaves out, which is
// what a person with pods/log comes to read.
func fetchStepLogSays(log []byte) error {
	if !strings.Contains(string(log), unreadableFetchLogReason) {
		return fmt.Errorf("the log does not say %q: %q", unreadableFetchLogReason, log)
	}
	return nil
}

// unreadableSourceEvent finds the Warning Event a failure wrote on one resource
// with the reason in it.
func unreadableSourceEvent(events []corev1.Event, owner types.UID, reason, file, want string) error {
	return warningEventSays(events, owner, reason, func(message string) error {
		return unreadableSourceSays(message, file, want)
	})
}

// fetchStepEvent finds the Warning Event that names the step and nothing more.
func fetchStepEvent(events []corev1.Event, owner types.UID, reason, step string) error {
	return warningEventSays(events, owner, reason, func(message string) error {
		return fetchStepSays(message, step)
	})
}

// warningEventSays reports what it examined, so a list with no Event of that
// resource and that reason reads differently from one that says something else.
func warningEventSays(events []corev1.Event, owner types.UID, reason string, says func(string) error) error {
	examined := 0
	var last error
	for index := range events {
		event := &events[index]
		if event.InvolvedObject.UID != owner || event.Reason != reason || event.Type != corev1.EventTypeWarning {
			continue
		}
		examined++
		if last = says(event.Message); last == nil {
			return nil
		}
	}
	if examined == 0 {
		return errors.New("no Warning " + reason + " Event of the resource yet")
	}
	return fmt.Errorf("%d Warning %s Events, none says why: %w", examined, reason, last)
}
