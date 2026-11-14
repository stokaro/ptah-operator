package dataplane_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// The plans below are the ones Ptah v0.26.0 saved for a PostgreSQL view, as the
// runner harness captured them from the pinned release image: each holds one
// statement and the effect Ptah recorded for it.
var viewPlans = []struct {
	file string
	// action is the effect's, and before and after its security_invoker.
	action, before, after string
	// withEffect is the manager's reading of the plan as Ptah saved it, and
	// fromSQL its reading of the same plan with the effects taken out, which
	// is how a plan from an earlier Ptah reads.
	withEffect, fromSQL []string
}{
	{
		// #1060: a body change that keeps the option restates it, and the SQL
		// alone cannot tell that from a change.
		file: "differing/plan-view-invoker-kept.json", action: "replace", before: "true", after: "true",
		withEffect: nil, fromSQL: []string{"SecurityDefiner"},
	},
	{
		file: "differing/plan-view-invoker-on-with-body.json", action: "replace", before: "false", after: "true",
		withEffect: []string{"SecurityDefiner"}, fromSQL: []string{"SecurityDefiner"},
	},
	{
		file: "differing/plan-view-invoker-on.json", action: "alter", before: "false", after: "true",
		withEffect: []string{"SecurityDefiner"}, fromSQL: []string{"SecurityDefiner"},
	},
	{
		// Back to the owner's rights, which #969 raises as it raises the way
		// in, and the effect keeps raising.
		file: "differing/plan-view-invoker-off.json", action: "alter", before: "true", after: "false",
		withEffect: []string{"SecurityDefiner"}, fromSQL: []string{"SecurityDefiner"},
	},
	{
		// A new view with invoker rights: the SQL reading lets CREATE VIEW
		// through, and the effect, absent before and true after, raises it.
		file: "differing/plan-view-invoker-new.json", action: "create", before: "", after: "true",
		withEffect: []string{"SecurityDefiner"}, fromSQL: nil,
	},
}

func TestDecodePlanReadsAViewsSecurityInvokerFromItsEffect(t *testing.T) {
	t.Parallel()

	for _, row := range viewPlans {
		t.Run(row.file, func(t *testing.T) {
			t.Parallel()
			data := loadCapture(t, row.file)
			decoded, err := dataplane.DecodePlan(data, "PostgreSQL")
			if err != nil {
				t.Fatalf("DecodePlan() error = %v", err)
			}
			if len(decoded.Statements) != 1 || len(decoded.Statements[0].Effects) != 1 {
				t.Fatalf("the capture holds %d statements, the first with %d effects, want one with one",
					len(decoded.Statements), len(decoded.Statements[0].Effects))
			}
			effect := decoded.Statements[0].Effects[0]
			invoker := propertyOf(effect, "security_invoker")
			if !effect.Read() || effect.Object.Kind != "view" || effect.Action != row.action || invoker == nil ||
				string(invoker.Before) != row.before || string(invoker.After) != row.after {
				t.Fatalf("the capture records %+v, want %s of a view with security_invoker %q to %q",
					effect, row.action, row.before, row.after)
			}
			if decoded.Destructive {
				t.Fatalf("the plan reads as destructive, so apply: Always would hold it whatever its privileges")
			}
			if !slices.Equal(decoded.PrivilegeChanges, row.withEffect) {
				t.Errorf("PrivilegeChanges = %q, want %q", decoded.PrivilegeChanges, row.withEffect)
			}

			bare, err := dataplane.DecodePlan(editStatement(t, data, func(statement map[string]any) {
				delete(statement, "effects")
			}), "PostgreSQL")
			if err != nil {
				t.Fatalf("DecodePlan() of the plan without effects: %v", err)
			}
			if !slices.Equal(bare.PrivilegeChanges, row.fromSQL) {
				t.Errorf("without effects PrivilegeChanges = %q, want %q", bare.PrivilegeChanges, row.fromSQL)
			}
		})
	}
}

// Each edit below leaves an effect that does not plainly describe its
// statement, and the statement is read as the same plan without effects reads.
// The edits are made to the plans whose effect and SQL disagree: the kept
// option, where the SQL raises and the effect does not, the new view, where the
// effect raises and the SQL does not, and an ALTER VIEW that restates the
// option, where the SQL raises and the effect does not. An edit a guard missed
// would read the effect, or fail on a value it cannot hold.
func TestDecodePlanReadsTheSQLWhereTheEffectDoesNotDescribeTheStatement(t *testing.T) {
	t.Parallel()

	const (
		kept    = "differing/plan-view-invoker-kept.json"
		created = "differing/plan-view-invoker-new.json"
		altered = "differing/plan-view-invoker-on.json"
	)
	invoker := func(effect map[string]any, edit func(map[string]any)) {
		for _, property := range effect["properties"].([]any) {
			if property := property.(map[string]any); property["name"] == "security_invoker" {
				edit(property)
			}
		}
	}
	// restated turns the ALTER VIEW capture into one whose option was already
	// on, which Ptah does not plan; the effect then says nothing changed.
	restated := func(effect map[string]any) {
		invoker(effect, func(property map[string]any) { property["before"], property["changed"] = true, false })
	}
	rows := []struct {
		name, file string
		edit       func(statement, effect map[string]any)
	}{
		{"another version", kept, func(_, effect map[string]any) { effect["version"] = 2 }},
		{"a version as a string", kept, func(_, effect map[string]any) { effect["version"] = "1" }},
		{"no version", kept, func(_, effect map[string]any) { delete(effect, "version") }},
		{"a materialized view", kept, func(_, effect map[string]any) {
			effect["object"].(map[string]any)["kind"] = "materialized_view"
		}},
		{"an object of another type", kept, func(_, effect map[string]any) { effect["object"] = "public.differs_notes_view" }},
		{"alter for a replace", kept, func(_, effect map[string]any) { effect["action"] = "alter" }},
		{"create for a replace", kept, func(_, effect map[string]any) { effect["action"] = "create" }},
		{"drop for a replace", kept, func(_, effect map[string]any) { effect["action"] = "drop" }},
		{"no security_invoker", kept, func(_, effect map[string]any) {
			effect["properties"] = slices.DeleteFunc(effect["properties"].([]any), func(property any) bool {
				return property.(map[string]any)["name"] == "security_invoker"
			})
		}},
		{"security_invoker twice", kept, func(_, effect map[string]any) {
			properties := effect["properties"].([]any)
			effect["properties"] = append(properties, properties[len(properties)-1])
		}},
		{"a value as a string", kept, func(_, effect map[string]any) {
			invoker(effect, func(property map[string]any) { property["before"] = "true" })
		}},
		{"a null value", kept, func(_, effect map[string]any) {
			invoker(effect, func(property map[string]any) { property["after"] = nil })
		}},
		{"a null value before a create", created, func(_, effect map[string]any) {
			invoker(effect, func(property map[string]any) { property["before"] = nil })
		}},
		{"changed beside equal values", kept, func(_, effect map[string]any) {
			invoker(effect, func(property map[string]any) { property["changed"] = true })
		}},
		{"no value after a replace", kept, func(_, effect map[string]any) {
			invoker(effect, func(property map[string]any) { delete(property, "after"); property["changed"] = true })
		}},
		{"a second record", kept, func(statement, effect map[string]any) { statement["effects"] = []any{effect, effect} }},
		// Every record has to be one the operator reads: one it does not may
		// say something about the view that the other does not.
		{"a record at a later version beside it", kept, func(statement, effect map[string]any) {
			statement["effects"] = []any{effect, map[string]any{"version": 2, "object": "anything"}}
		}},
		{"an effect for another view", kept, func(_, effect map[string]any) {
			effect["object"].(map[string]any)["name"] = "unrelated_view"
		}},
		{"an effect for the view's name in another schema", altered, func(statement, effect map[string]any) {
			restated(effect)
			statement["sql"] = strings.Replace(statement["sql"].(string), `"differs_notes_view"`, "app.differs_notes_view", 1)
		}},
		{"a view name with a third part", altered, func(statement, effect map[string]any) {
			restated(effect)
			statement["sql"] = strings.Replace(statement["sql"].(string), `"differs_notes_view"`, "harness.public.differs_notes_view", 1)
		}},
		{"a comment before the view's name", altered, func(statement, effect map[string]any) {
			restated(effect)
			statement["sql"] = strings.Replace(statement["sql"].(string), `"differs_notes_view"`, `/* x */ "differs_notes_view"`, 1)
		}},
		{"a second statement in the SQL", kept, func(statement, _ map[string]any) {
			statement["sql"] = statement["sql"].(string) + `; ALTER VIEW "other_view" SET (security_invoker=true)`
		}},
		{"a value before a create", created, func(_, effect map[string]any) {
			invoker(effect, func(property map[string]any) { property["before"] = false })
		}},
		{"no value after a create", created, func(_, effect map[string]any) {
			invoker(effect, func(property map[string]any) { delete(property, "after") })
		}},
		{"replace for a create", created, func(_, effect map[string]any) { effect["action"] = "replace" }},
		{"a materialized view's statement", created, func(statement, _ map[string]any) {
			statement["sql"] = strings.Replace(statement["sql"].(string), "CREATE VIEW", "CREATE MATERIALIZED VIEW", 1)
		}},
		{"no value before an alter", altered, func(_, effect map[string]any) {
			invoker(effect, func(property map[string]any) {
				delete(property, "before")
				property["after"], property["changed"] = false, true
			})
		}},
		{"no value after an alter", altered, func(_, effect map[string]any) {
			restated(effect)
			invoker(effect, func(property map[string]any) { delete(property, "after"); property["changed"] = true })
		}},
		{"an ALTER TABLE on the view", altered, func(statement, effect map[string]any) {
			restated(effect)
			statement["sql"] = strings.Replace(statement["sql"].(string), "ALTER VIEW", "ALTER TABLE", 1)
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			edited := editStatement(t, loadCapture(t, row.file), func(statement map[string]any) {
				row.edit(statement, statement["effects"].([]any)[0].(map[string]any))
			})
			decoded, err := dataplane.DecodePlan(edited, "PostgreSQL")
			if err != nil {
				t.Fatalf("DecodePlan() error = %v", err)
			}
			if want := fromSQL(t, edited); !slices.Equal(decoded.PrivilegeChanges, want) {
				t.Fatalf("PrivilegeChanges = %q, want the SQL's %q", decoded.PrivilegeChanges, want)
			}
		})
	}

	// The controls: each plan the rows edit reads otherwise from its effect
	// than from its SQL, so a row that read the effect would be seen.
	for file, edit := range map[string]func(effect map[string]any){
		kept: func(map[string]any) {}, created: func(map[string]any) {}, altered: restated,
	} {
		plan := editStatement(t, loadCapture(t, file), func(statement map[string]any) {
			edit(statement["effects"].([]any)[0].(map[string]any))
		})
		decoded, err := dataplane.DecodePlan(plan, "PostgreSQL")
		if err != nil {
			t.Fatal(err)
		}
		if slices.Equal(decoded.PrivilegeChanges, fromSQL(t, plan)) {
			t.Fatalf("%s reads %q from its effect and from its SQL alike", file, decoded.PrivilegeChanges)
		}
	}
}

// The effect is read where the SQL names its view however the name is spelled:
// with the schema the effect gives it, unquoted in any case, which PostgreSQL
// folds, and after IF EXISTS. Each row is the ALTER VIEW capture with the option
// restated, which its SQL raises and its effect does not.
func TestDecodePlanReadsTheEffectOfTheViewTheSQLNames(t *testing.T) {
	t.Parallel()

	for name, spelling := range map[string]string{
		"as captured":            `ALTER VIEW "differs_notes_view"`,
		"with its schema":        `ALTER VIEW "public"."differs_notes_view"`,
		"unquoted in upper case": `alter view PUBLIC.Differs_Notes_View`,
		"after IF EXISTS":        `ALTER VIEW IF EXISTS public."differs_notes_view"`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan := editStatement(t, loadCapture(t, "differing/plan-view-invoker-on.json"), func(statement map[string]any) {
				statement["sql"] = strings.Replace(statement["sql"].(string), `ALTER VIEW "differs_notes_view"`, spelling, 1)
				for _, property := range statement["effects"].([]any)[0].(map[string]any)["properties"].([]any) {
					if property := property.(map[string]any); property["name"] == "security_invoker" {
						property["before"], property["changed"] = true, false
					}
				}
			})
			decoded, err := dataplane.DecodePlan(plan, "PostgreSQL")
			if err != nil {
				t.Fatalf("DecodePlan() error = %v", err)
			}
			if len(decoded.PrivilegeChanges) != 0 {
				t.Fatalf("PrivilegeChanges = %q, want none from the effect", decoded.PrivilegeChanges)
			}
			if sql := fromSQL(t, plan); !slices.Equal(sql, []string{"SecurityDefiner"}) {
				t.Fatalf("the SQL reads %q, want SecurityDefiner, so a row that read the SQL would be seen", sql)
			}
		})
	}
}

// Ptah rebuilds a view it cannot change in place with DROP VIEW and then CREATE
// VIEW, each with its effect. The v0.26.0 release image, planning against
// PostgreSQL 18 a view that loses a column, and a column type change under a
// view, wrote DROP VIEW "differs_notes_view" at severity warning with a drop
// effect that holds the options before and nothing after; stokaro/ptah's
// internal/planeffect/pipeline_test.go holds DROP VIEW IF EXISTS ... CASCADE at
// v0.26.0 for another rebuild. The create records the option as absent before,
// as it does for a view that never existed, and the value it replaces is the
// one the drop recorded. Each plan here is the new-view capture, made again
// with the option the row gives it, after the statements the row puts before
// it.
func TestDecodePlanReadsARebuiltViewAgainstTheViewItDropped(t *testing.T) {
	t.Parallel()

	const (
		dropView    = `DROP VIEW "differs_notes_view"`
		dropCascade = `DROP VIEW IF EXISTS "differs_notes_view" CASCADE`
		withOn      = ` WITH (security_invoker=true)`
		withOff     = ` WITH (security_invoker=false)`
	)
	rows := []struct {
		name   string
		before []map[string]any
		// replace makes the view with CREATE OR REPLACE VIEW, whose effect
		// is replace, rather than CREATE VIEW.
		replace bool
		option  string
		after   bool
		want    []string
	}{
		{
			name:   "a rebuild that turns invoker rights off as it states the option",
			before: []map[string]any{dropStatement(dropView, "differs_notes_view", true)},
			option: withOff, after: false, want: []string{"SecurityDefiner"},
		},
		{
			// The SQL alone raises nothing here: a new view that states no
			// option reads with its owner's rights, as one with invoker
			// rights before the drop no longer does.
			name:   "a rebuild that turns invoker rights off by leaving the option out",
			before: []map[string]any{dropStatement(dropView, "differs_notes_view", true)},
			after:  false, want: []string{"SecurityDefiner"},
		},
		{
			name:   "a rebuild that keeps invoker rights",
			before: []map[string]any{dropStatement(dropView, "differs_notes_view", true)},
			option: withOn, after: true, want: nil,
		},
		{
			name:   "a rebuild that turns invoker rights on",
			before: []map[string]any{dropStatement(dropView, "differs_notes_view", false)},
			option: withOn, after: true, want: []string{"SecurityDefiner"},
		},
		{
			// A CASCADE may take other views, which this one is not: the drop
			// says what it held.
			name:   "a rebuild with CASCADE that keeps invoker rights",
			before: []map[string]any{dropStatement(dropCascade, "differs_notes_view", true)},
			option: withOn, after: true, want: nil,
		},
		{
			name:   "a rebuild with CASCADE that turns invoker rights off",
			before: []map[string]any{dropStatement(dropCascade, "differs_notes_view", true)},
			after:  false, want: []string{"SecurityDefiner"},
		},
		{
			// Ptah rebuilds a view around a change of a column it reads.
			name: "a rebuild around a column type change",
			before: []map[string]any{
				dropStatement(dropView, "differs_notes_view", true),
				{"sql": `ALTER TABLE "differs_notes" ALTER COLUMN "owner_name" TYPE varchar(64)`, "severity": "destructive"},
			},
			option: withOn, after: true, want: nil,
		},
		{
			name:   "a rebuild that keeps the owner's rights",
			before: []map[string]any{dropStatement(dropView, "differs_notes_view", false)},
			option: withOff, after: false, want: nil,
		},
		{
			// After the drop the view is known to be absent, so Ptah records a
			// replace with no value before. The SQL raises any option CREATE
			// OR REPLACE VIEW states, so this one states none.
			name:    "a rebuild through CREATE OR REPLACE VIEW",
			before:  []map[string]any{dropStatement(dropView, "differs_notes_view", true)},
			replace: true, after: false, want: []string{"SecurityDefiner"},
		},
		{
			name:    "a rebuild through CREATE OR REPLACE VIEW that keeps invoker rights",
			before:  []map[string]any{dropStatement(dropView, "differs_notes_view", true)},
			replace: true, option: withOn, after: true, want: nil,
		},
		{
			// The drop's effect says the view held invoker rights and does not
			// exist after the drop; one that says it still does is not read,
			// and the view is read as one the plan may have dropped.
			name: "a drop whose effect leaves the view in place",
			before: []map[string]any{func() map[string]any {
				drop := dropStatement(dropView, "differs_notes_view", true)
				invoker := drop["effects"].([]any)[0].(map[string]any)["properties"].([]any)[1].(map[string]any)
				invoker["after"], invoker["changed"] = true, false
				return drop
			}()},
			option: withOn, after: true, want: []string{"SecurityDefiner"},
		},
		{
			// Ptah writes no effect for the drop of a view it could not follow,
			// and the view made again is held whatever it states.
			name:   "a drop with no effect",
			before: []map[string]any{{"sql": `DROP VIEW IF EXISTS "differs_notes_view"`, "severity": "safe"}},
			option: withOff, after: false, want: []string{"SecurityDefiner"},
		},
		{
			name:   "a drop with no effect, spelled with the view's schema",
			before: []map[string]any{{"sql": `DROP VIEW public.differs_notes_view`, "severity": "safe"}},
			option: withOff, after: false, want: []string{"SecurityDefiner"},
		},
		{
			name:   "a drop with no effect of two views",
			before: []map[string]any{{"sql": `DROP VIEW "other_view", "differs_notes_view"`, "severity": "safe"}},
			option: withOff, after: false, want: []string{"SecurityDefiner"},
		},
		{
			// A CASCADE takes what depends on what it drops, a view the plan
			// names nowhere included.
			name:   "a CASCADE that may have taken the view",
			before: []map[string]any{{"sql": `DROP TABLE "differs_notes_archive" CASCADE`, "severity": "destructive"}},
			option: withOff, after: false, want: []string{"SecurityDefiner"},
		},
		{
			name:   "DROP OWNED, which may have taken the view",
			before: []map[string]any{{"sql": `DROP OWNED BY "differs_app"`, "severity": "destructive"}},
			option: withOff, after: false, want: []string{"Revoke", "SecurityDefiner"},
		},
		// The controls: the same new view, after statements that drop no view
		// of its name, is read from its effect as a new view, which the SQL
		// would raise.
		{
			name:   "a foreign key's ON DELETE CASCADE",
			before: []map[string]any{{"sql": `ALTER TABLE "differs_notes" DROP CONSTRAINT "differs_notes_owner_fkey", ADD CONSTRAINT "differs_notes_owner_fkey" FOREIGN KEY (owner_id) REFERENCES "differs_owners" (id) ON DELETE CASCADE`, "severity": "safe"}},
			option: withOff, after: false, want: nil,
		},
		{
			name:   "the drop of another view",
			before: []map[string]any{dropStatement(`DROP VIEW IF EXISTS "other_view"`, "other_view", true)},
			option: withOff, after: false, want: nil,
		},
		{
			name:   "no statement before it",
			option: withOff, after: false, want: nil,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			plan := withStatementsBefore(t, madeAgain(t, row.replace, row.option, row.after), row.before...)
			decoded, err := dataplane.DecodePlan(plan, "PostgreSQL")
			if err != nil {
				t.Fatalf("DecodePlan() error = %v", err)
			}
			if !slices.Equal(decoded.PrivilegeChanges, row.want) {
				t.Fatalf("PrivilegeChanges = %q, want %q", decoded.PrivilegeChanges, row.want)
			}
		})
	}
}

// After a statement that may have dropped a view without an effect that says
// what the view held, a view the plan makes again is held whatever it states:
// the view it replaces may have read with its readers' rights, and one made
// again with no option reads with its owner's (#1080). Until Ptah records what
// such a statement drops (stokaro/ptah#6023), the hold covers every view the
// statement may have taken. A CASCADE, DROP OWNED and a DROP VIEW whose names
// are not read may take any view; a DROP VIEW with no effect takes the view it
// names. Each plan is the new-view capture made again as the row says, after
// the statements the row puts before it; bare takes the statement's effect
// out, as Ptah writes a CREATE OR REPLACE VIEW of a view a CASCADE left
// uncertain.
func TestDecodePlanHoldsAViewMadeAgainAfterAnUnreadDrop(t *testing.T) {
	t.Parallel()

	const withOn = ` WITH (security_invoker=true)`
	var (
		cascade     = map[string]any{"sql": `DROP TABLE "differs_notes_archive" CASCADE`, "severity": "destructive"}
		dropOwned   = map[string]any{"sql": `DROP OWNED BY "differs_app"`, "severity": "destructive"}
		unreadDrop  = map[string]any{"sql": `DROP VIEW IF EXISTS "differs_notes_view"`, "severity": "safe"}
		twoViews    = map[string]any{"sql": `DROP VIEW "other_view", "differs_notes_view"`, "severity": "safe"}
		onDelete    = map[string]any{"sql": `ALTER TABLE "differs_notes" DROP CONSTRAINT "differs_notes_owner_fkey", ADD CONSTRAINT "differs_notes_owner_fkey" FOREIGN KEY (owner_id) REFERENCES "differs_owners" (id) ON DELETE CASCADE`, "severity": "safe"}
		truncate    = map[string]any{"sql": `TRUNCATE "differs_notes_archive" CASCADE`, "severity": "destructive"}
		otherUnread = map[string]any{"sql": `DROP VIEW IF EXISTS "other_view"`, "severity": "safe"}
		otherSchema = map[string]any{"sql": `DROP VIEW IF EXISTS app.differs_notes_view`, "severity": "safe"}
	)
	rows := []struct {
		name    string
		before  []map[string]any
		replace bool
		bare    bool
		option  string
		after   bool
		want    []string
	}{
		{name: "a CASCADE, then the view with no option", before: []map[string]any{cascade}, want: []string{"SecurityDefiner"}},
		{name: "a CASCADE, then the view with invoker rights", before: []map[string]any{cascade}, option: withOn, after: true, want: []string{"SecurityDefiner"}},
		{name: "DROP OWNED, then the view with no option", before: []map[string]any{dropOwned}, want: []string{"Revoke", "SecurityDefiner"}},
		{name: "a DROP VIEW with no effect, then the view with no option", before: []map[string]any{unreadDrop}, want: []string{"SecurityDefiner"}},
		{name: "a DROP VIEW of two views, then the view with no option", before: []map[string]any{twoViews}, want: []string{"SecurityDefiner"}},
		{
			name:   "a CASCADE, then CREATE OR REPLACE VIEW with nothing before and no option",
			before: []map[string]any{cascade}, replace: true, want: []string{"SecurityDefiner"},
		},
		{
			name:   "a CASCADE, then CREATE OR REPLACE VIEW with no effect and no option",
			before: []map[string]any{cascade}, replace: true, bare: true, want: []string{"SecurityDefiner"},
		},
		{
			name:   "a CASCADE, then CREATE VIEW with no effect and no option",
			before: []map[string]any{cascade}, bare: true, want: []string{"SecurityDefiner"},
		},
		{
			name:   "a DROP VIEW with no effect of the view in another schema, then CREATE VIEW with no effect",
			before: []map[string]any{otherSchema}, bare: true, want: []string{"SecurityDefiner"},
		},
		// The controls: what the change does not hold.
		{name: "no statement before it", want: nil},
		{name: "no statement before it, with no effect", bare: true, want: nil},
		{name: "a foreign key's ON DELETE CASCADE", before: []map[string]any{onDelete}, want: nil},
		// TRUNCATE ... CASCADE empties the tables that refer to the one it
		// empties, and drops nothing.
		{name: "a TRUNCATE with CASCADE", before: []map[string]any{truncate}, want: nil},
		{name: "a DROP VIEW with no effect of another view", before: []map[string]any{otherUnread}, want: nil},
		{
			// The effect names public.differs_notes_view, which a drop of
			// app.differs_notes_view did not take.
			name: "a DROP VIEW with no effect of the view in another schema", before: []map[string]any{otherSchema}, want: nil,
		},
		{
			// A drop after the CASCADE that Ptah records says what the view
			// held, so the rebuild reads against it.
			name:   "a CASCADE, then a rebuild whose drop says what the view held",
			before: []map[string]any{cascade, dropStatement(`DROP VIEW "differs_notes_view"`, "differs_notes_view", true)},
			option: withOn, after: true, want: nil,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			plan := madeAgain(t, row.replace, row.option, row.after)
			if row.bare {
				plan = editStatement(t, plan, func(statement map[string]any) { delete(statement, "effects") })
			}
			decoded, err := dataplane.DecodePlan(withStatementsBefore(t, plan, row.before...), "PostgreSQL")
			if err != nil {
				t.Fatalf("DecodePlan() error = %v", err)
			}
			if !slices.Equal(decoded.PrivilegeChanges, row.want) {
				t.Fatalf("PrivilegeChanges = %q, want %q", decoded.PrivilegeChanges, row.want)
			}
		})
	}

	// An ALTER VIEW changes a view that exists, so it is not held for a view
	// a CASCADE may have taken: the ALTER would fail on one that is gone.
	altered := editStatement(t, loadCapture(t, "differing/plan-view-invoker-on.json"), func(statement map[string]any) {
		statement["sql"] = `ALTER VIEW "differs_notes_view" SET (security_barrier=true)`
		delete(statement, "effects")
	})
	decoded, err := dataplane.DecodePlan(withStatementsBefore(t, altered, cascade), "PostgreSQL")
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.PrivilegeChanges) != 0 {
		t.Fatalf("an ALTER VIEW after a CASCADE reads %q, want none", decoded.PrivilegeChanges)
	}
}

// madeAgain is the new-view capture with its statement made as CREATE VIEW, or
// CREATE OR REPLACE VIEW with a replace effect, stating option, and its
// effect's security_invoker after the statement set to after.
func madeAgain(t *testing.T, replace bool, option string, after bool) []byte {
	t.Helper()
	return editStatement(t, loadCapture(t, "differing/plan-view-invoker-new.json"), func(statement map[string]any) {
		verb := "CREATE VIEW"
		if replace {
			verb = "CREATE OR REPLACE VIEW"
			statement["effects"].([]any)[0].(map[string]any)["action"] = "replace"
		}
		statement["sql"] = verb + ` "differs_notes_view"` + option + " AS\nSELECT id, owner_name FROM differs_notes"
		for _, property := range statement["effects"].([]any)[0].(map[string]any)["properties"].([]any) {
			if property := property.(map[string]any); property["name"] == "security_invoker" {
				property["after"] = after
			}
		}
	})
}

// dropStatement is a DROP VIEW of the view name, with the effect Ptah records
// for one that existed: the options it held, and nothing after.
func dropStatement(sql, name string, invoker bool) map[string]any {
	return map[string]any{
		"sql": sql, "severity": "warning",
		"effects": []any{map[string]any{
			"version": 1, "action": "drop",
			"object": map[string]any{"kind": "view", "schema": "public", "name": name},
			"properties": []any{
				map[string]any{"name": "security_barrier", "before": false, "changed": true},
				map[string]any{"name": "security_invoker", "before": invoker, "changed": true},
			},
		}},
	}
}

// withStatementsBefore returns the plan with statements put before its own.
func withStatementsBefore(t *testing.T, data []byte, statements ...map[string]any) []byte {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var plan map[string]any
	if err := decoder.Decode(&plan); err != nil {
		t.Fatal(err)
	}
	var all []any
	for _, statement := range statements {
		all = append(all, statement)
	}
	plan["statements"] = append(all, plan["statements"].([]any)...)
	edited, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return edited
}

// fromSQL is the privilege reading of a plan with its effects taken out.
func fromSQL(t *testing.T, plan []byte) []string {
	t.Helper()
	decoded, err := dataplane.DecodePlan(editStatement(t, plan, func(statement map[string]any) {
		delete(statement, "effects")
	}), "PostgreSQL")
	if err != nil {
		t.Fatal(err)
	}
	return decoded.PrivilegeChanges
}

// Ptah adds fields, properties and records to an effect without raising its
// version, and a reader leaves out what it does not know. None of these edits
// may refuse the plan or change what the effect says about security_invoker.
func TestDecodePlanReadsAnEffectPastWhatALaterPtahAdds(t *testing.T) {
	t.Parallel()

	data := loadCapture(t, "differing/plan-view-invoker-kept.json")
	edits := map[string]func(statement, effect map[string]any){
		"a field of the record": func(_, effect map[string]any) { effect["reason"] = "body changed" },
		"a field of the object": func(_, effect map[string]any) { effect["object"].(map[string]any)["owner"] = "app" },
		"a field of a property": func(_, effect map[string]any) {
			effect["properties"].([]any)[0].(map[string]any)["source"] = "catalog"
		},
		"a property of another type": func(_, effect map[string]any) {
			effect["properties"] = append(effect["properties"].([]any),
				map[string]any{"name": "check_option", "before": "local", "after": "cascaded", "changed": true})
		},
		"a record of another kind": func(statement, effect map[string]any) {
			statement["effects"] = []any{effect, map[string]any{
				"version": 1, "action": "alter", "properties": []any{},
				"object": map[string]any{"kind": "table", "schema": "public", "name": "differs_notes"},
			}}
		},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			edited := editStatement(t, data, func(statement map[string]any) {
				edit(statement, statement["effects"].([]any)[0].(map[string]any))
			})
			decoded, err := dataplane.DecodePlan(edited, "PostgreSQL")
			if err != nil {
				t.Fatalf("DecodePlan() error = %v", err)
			}
			if len(decoded.PrivilegeChanges) != 0 {
				t.Fatalf("PrivilegeChanges = %q, want none from the effect", decoded.PrivilegeChanges)
			}
		})
	}
}

// The list holds records. Something that is not one, or a list that is not a
// list, refuses the plan, as any field of the wrong type does.
func TestDecodePlanRefusesEffectsThatAreNotRecords(t *testing.T) {
	t.Parallel()

	data := loadCapture(t, "differing/plan-view-invoker-kept.json")
	for name, value := range map[string]any{
		"a number in the list":  []any{1},
		"a string in the list":  []any{"replace"},
		"a null in the list":    []any{nil},
		"an object, not a list": map[string]any{"version": 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			edited := editStatement(t, data, func(statement map[string]any) { statement["effects"] = value })
			if _, err := dataplane.DecodePlan(edited, "PostgreSQL"); err == nil {
				t.Fatal("DecodePlan() accepted effects that hold no record")
			}
		})
	}
}

func propertyOf(effect dataplane.PlanEffect, name string) *dataplane.PlanEffectProperty {
	for index := range effect.Properties {
		if effect.Properties[index].Name == name {
			return &effect.Properties[index]
		}
	}
	return nil
}

// editStatement returns the plan with its first statement edited. The plan is
// decoded keeping its numbers as they were written, so nothing else in it
// moves.
func editStatement(t *testing.T, data []byte, edit func(statement map[string]any)) []byte {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var plan map[string]any
	if err := decoder.Decode(&plan); err != nil {
		t.Fatal(err)
	}
	edit(plan["statements"].([]any)[0].(map[string]any))
	edited, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return edited
}
