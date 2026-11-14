package e2e

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// schemaApplyLogRow is one entry of Ptah's ptah_schema_apply_log, the record a
// database keeps of each schema apply that sent it a statement: a started entry
// before the first statement, and the outcome once the apply returns.
type schemaApplyLogRow struct {
	RunID        string
	Seq          int64
	State        string
	ActorSource  string
	Actor        string
	ActorOrigin  string
	RunReference string
	PlanDigest   string
}

// schemaApplyLogQuery prints every entry of the log on the engine named, one
// per line, run by run. As in migrationLogQuery, every text a caller or a plan
// gave crosses as hex.
func schemaApplyLogQuery(engine string) string {
	return "SELECT concat(run_id, ':', seq, ':', state, ':', actor_source, ':', " +
		hexText(engine, "actor") + ", ':', " + hexText(engine, "actor_origin") + ", ':', " +
		hexText(engine, "run_reference") + ", ':', " + hexText(engine, "plan_digest") +
		") FROM ptah_schema_apply_log ORDER BY logged_at, run_id, seq"
}

// parseSchemaApplyLog reads what schemaApplyLogQuery printed.
func parseSchemaApplyLog(output string) ([]schemaApplyLogRow, error) {
	var rows []schemaApplyLogRow
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 8 {
			return nil, fmt.Errorf("the log row %q is not run:seq:state:source:name:origin:reference:plan", line)
		}
		seq, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("the log row %q names no sequence number: %w", line, err)
		}
		texts, err := hexFields(fields[4:])
		if err != nil {
			return nil, fmt.Errorf("the log row %q carries a field that is not hex: %w", line, err)
		}
		rows = append(rows, schemaApplyLogRow{
			RunID: fields[0], Seq: seq, State: fields[2], ActorSource: fields[3],
			Actor: texts[0], ActorOrigin: texts[1], RunReference: texts[2], PlanDigest: texts[3],
		})
	}
	return rows, nil
}

// applyLoggedAs is nil when the log holds exactly one apply under the run's
// reference, and it applied the plan with the digest given: a started entry and
// an applied one, in that order, each naming the run's actor with source
// provided and its origin. Entries of other applies are passed over, because a
// database keeps the log of every apply it was sent, but at least one entry has
// to be read, and the count of entries under the reference is declared.
func applyLoggedAs(rows []schemaApplyLogRow, run loggedRun, planDigest string) error {
	if err := run.check(); err != nil {
		return err
	}
	if planDigest == "" {
		return fmt.Errorf("the row has no plan digest to look for")
	}
	if len(rows) == 0 {
		return fmt.Errorf("the log holds no entry")
	}
	var applied []schemaApplyLogRow
	for _, row := range rows {
		if row.RunReference == run.reference {
			applied = append(applied, row)
		}
	}
	// The run's own sequence numbers order its entries, whatever the clock
	// stamped them with.
	slices.SortStableFunc(applied, func(a, b schemaApplyLogRow) int { return cmp.Compare(a.Seq, b.Seq) })
	if len(applied) != 2 {
		return fmt.Errorf("the log holds %d entries under run reference %q among %d, and an apply that applied writes 2",
			len(applied), run.reference, len(rows))
	}
	for index, row := range applied {
		seq, state := int64(index+1), "started"
		if index == 1 {
			state = "applied"
		}
		switch {
		case row.RunID == "" || row.RunID != applied[0].RunID:
			return fmt.Errorf("entry %d belongs to run %q, and entry 0 to run %q", index, row.RunID, applied[0].RunID)
		case row.Seq != seq || row.State != state:
			return fmt.Errorf("entry %d is %d %s, want %d %s", index, row.Seq, row.State, seq, state)
		case row.ActorSource != actorSourceProvided:
			return fmt.Errorf("entry %d (%s) has source %q, want %q", index, row.State, row.ActorSource, actorSourceProvided)
		case !printableASCII(row.Actor):
			return fmt.Errorf("entry %d (%s) names %q, which is not printable ASCII", index, row.State, row.Actor)
		case row.Actor != run.actor:
			return fmt.Errorf("entry %d (%s) names %q, want %q", index, row.State, row.Actor, run.actor)
		case row.ActorOrigin != run.origin:
			return fmt.Errorf("entry %d (%s) records origin %q, want %q", index, row.State, row.ActorOrigin, run.origin)
		case row.PlanDigest != planDigest:
			return fmt.Errorf("entry %d (%s) names plan digest %q, want %q", index, row.State, row.PlanDigest, planDigest)
		}
	}
	return nil
}

// loggedActorBytes is the width of the actor column of both of Ptah's logs.
const loggedActorBytes = 256

// unencodedInTheLog reports whether the operator writes a username to the log
// as it is: printable ASCII with no % and no $, the bytes its encoding leaves
// alone, with no space at either end for it to trim, and short enough that it
// is not cut to fit the column.
func unencodedInTheLog(username string) bool {
	return username != "" && username == strings.TrimSpace(username) && len(username) <= loggedActorBytes &&
		printableASCII(username) && !strings.ContainsAny(username, "%$")
}
