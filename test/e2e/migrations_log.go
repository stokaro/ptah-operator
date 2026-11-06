package e2e

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// migrationLogRow is one row of Ptah's schema_migrations_log, the record a
// database keeps of what ran against it, which outlives the cluster.
type migrationLogRow struct {
	RunID       string
	Version     int64
	State       string
	ActorSource string
	Actor       string
}

const (
	// loggedPerson is who approves the main migration: a username with a space, a
	// colon, non-ASCII characters and a variable reference, which an authenticator
	// is free to produce and which has to reach the database's log through the
	// webhook, the claim, the Job's environment, the kubelet and the executor. The
	// executor's environment sets HOME, so a kubelet left to expand the reference
	// would change what the row reads.
	loggedPerson = "oidc:José García $(HOME)"
	// loggedPersonInTheLog is that username as the log stores it. It is written out
	// rather than taken from the code under test, because it is what a person
	// reads, and a unit test holds the two to one value.
	loggedPersonInTheLog = "oidc:Jos%C3%A9 Garc%C3%ADa %24(HOME)"
)

// actorSourceProvided is what Ptah calls a name a caller handed it. The other
// two it writes are unknown, for no name at all, and process-user, for the user
// the process runs as, and neither says who approved anything.
const actorSourceProvided = "provided"

// migrationLogQuery prints the database's log rows in the order the run wrote
// them, one per line. The name crosses as hex, so the row reads the bytes the
// table holds, and neither client's encoding nor the way it pads a value can
// change what it compares.
func (e migrationEngine) migrationLogQuery() string {
	actor := "encode(convert_to(coalesce(actor, ''), 'UTF8'), 'hex')"
	if e.name == "mysql" {
		actor = "hex(coalesce(actor, ''))"
	}
	return "SELECT concat(run_id, ':', version, ':', state, ':', actor_source, ':', " + actor +
		") FROM schema_migrations_log ORDER BY seq"
}

// parseMigrationLog reads what migrationLogQuery printed.
func parseMigrationLog(output string) ([]migrationLogRow, error) {
	var rows []migrationLogRow
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 5 {
			return nil, fmt.Errorf("the log row %q is not run:version:state:source:name", line)
		}
		version, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("the log row %q names no version: %w", line, err)
		}
		actor, err := hex.DecodeString(fields[4])
		if err != nil {
			return nil, fmt.Errorf("the log row %q carries no hex name: %w", line, err)
		}
		rows = append(rows, migrationLogRow{RunID: fields[0], Version: version, State: fields[2], ActorSource: fields[3], Actor: string(actor)})
	}
	return rows, nil
}

// runLogNames is a log that holds one successful run of the versions given, in
// order, and names actor, as the log writes it, on every row of it. Each version
// writes two rows, so the count is declared: a run that lost a row, as Ptah does
// when the database refuses one and carries on, does not pass by naming the
// person on the rest. A name outside printable ASCII is what a database in a
// narrower character set refuses, so a row that holds one is refused here.
func runLogNames(rows []migrationLogRow, actor string, versions ...int64) error {
	if actor == "" {
		return errors.New("the row has no name to look for")
	}
	if len(versions) == 0 {
		return errors.New("the row names no version to look for")
	}
	if want := 2 * len(versions); len(rows) != want {
		return fmt.Errorf("the log holds %d rows, and a run of versions %v writes %d", len(rows), versions, want)
	}
	for index, row := range rows {
		version, state := versions[index/2], "started"
		if index%2 == 1 {
			state = "applied"
		}
		switch {
		case row.RunID == "" || row.RunID != rows[0].RunID:
			return fmt.Errorf("row %d belongs to run %q, and row 0 to run %q", index, row.RunID, rows[0].RunID)
		case row.Version != version || row.State != state:
			return fmt.Errorf("row %d is version %d %s, want version %d %s", index, row.Version, row.State, version, state)
		case row.Actor == "":
			return fmt.Errorf("row %d (version %d %s) names nobody, with source %q", index, row.Version, row.State, row.ActorSource)
		case row.ActorSource != actorSourceProvided:
			return fmt.Errorf("row %d (version %d %s) has source %q, want %q", index, row.Version, row.State, row.ActorSource, actorSourceProvided)
		case !printableASCII(row.Actor):
			return fmt.Errorf("row %d (version %d %s) names %q, which is not printable ASCII", index, row.Version, row.State, row.Actor)
		case row.Actor != actor:
			return fmt.Errorf("row %d (version %d %s) names %q, want %q", index, row.Version, row.State, row.Actor, actor)
		}
	}
	return nil
}

// printableASCII reports whether every byte is what the operator leaves
// unencoded.
func printableASCII(value string) bool {
	for index := range len(value) {
		if value[index] < 0x20 || value[index] > 0x7E {
			return false
		}
	}
	return true
}

// loggedNameDecodesTo reports whether a name read from the log decodes back to
// the username admission stamped, with the decoder the documentation names: one
// that leaves a plus alone.
func loggedNameDecodesTo(logged, stamped string) error {
	decoded, err := url.PathUnescape(logged)
	if err != nil {
		return fmt.Errorf("%q does not decode: %w", logged, err)
	}
	if decoded != stamped {
		return fmt.Errorf("%q decodes to %q, and admission stamped %q", logged, decoded, stamped)
	}
	return nil
}

// alwaysPolicyActor is the name a run under apply: Always records. It is written
// out here rather than taken from the code under test, because a person reading
// the log reads this string and the documentation promises it.
func alwaysPolicyActor(namespace, name string) string {
	return "system:ptah-operator:apply-always:" + namespace + "/" + name
}
