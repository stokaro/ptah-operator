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
		"a record at a later version": func(statement, effect map[string]any) {
			statement["effects"] = []any{effect, map[string]any{"version": 2, "object": "anything"}}
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
