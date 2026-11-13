package dataplane

import (
	"encoding/json"
	"errors"
)

// PlanEffectVersion is the version of Ptah's statement effect record this
// operator reads (stokaro/ptah's docs/site/src/content/docs/reference/
// statement-effects.md). Ptah raises it when a field changes meaning or leaves;
// a new kind of object, action, property or field keeps it. A record at another
// version is kept and not read, and its statement is read from its SQL.
const PlanEffectVersion = 1

// The object kinds, actions and properties of an effect the operator reads.
const (
	PlanEffectKindView = "view"

	PlanEffectActionCreate  = "create"
	PlanEffectActionReplace = "replace"
	PlanEffectActionAlter   = "alter"

	// PlanEffectSecurityInvoker is a PostgreSQL view's security_invoker:
	// whether the view reads the tables under it with its reader's rights
	// rather than its owner's.
	PlanEffectSecurityInvoker = "security_invoker"
)

// PlanEffect is what Ptah recorded one plan statement does to one object,
// computed from the database the plan read: the object, the kind of statement,
// and each property's value before the statement runs and after.
//
// Ptah adds fields without raising Version, and a reader leaves out what it
// does not know. So a record is decoded leniently inside the strict plan
// decoder: a field it does not declare is ignored, and a record whose declared
// fields do not have the types version 1 gives them is kept unread, as a record
// at another version is. Nothing in a record reaches the executor. Ptah does
// not read effects back when it applies a plan, and the plan digest and the
// approval cover them with the rest of the file.
type PlanEffect struct {
	Version    int                  `json:"version"`
	Object     PlanEffectObject     `json:"object"`
	Action     string               `json:"action"`
	Properties []PlanEffectProperty `json:"properties,omitempty"`

	// read is true for a record at PlanEffectVersion whose declared fields
	// decoded.
	read bool
}

// PlanEffectObject names the object an effect describes, as the database spells
// it.
type PlanEffectObject struct {
	Kind   string `json:"kind"`
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
}

// PlanEffectProperty is one property of the object, before the statement runs
// and after. An absent value says the object does not exist at that point. The
// values stay undecoded here: a property a later Ptah adds may hold another
// type, and each property the operator reads is decoded where it is read.
type PlanEffectProperty struct {
	Name    string          `json:"name"`
	Before  json.RawMessage `json:"before,omitempty"`
	After   json.RawMessage `json:"after,omitempty"`
	Changed bool            `json:"changed"`
}

// Read reports whether the record is one the operator reads: version 1, with
// its declared fields of the declared types.
func (e PlanEffect) Read() bool { return e.read }

// UnmarshalJSON decodes one effect record. Only a value that is not a JSON
// object is an error, which refuses the plan: the list holds records, and
// something else in it is not a record of any version.
func (e *PlanEffect) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return errors.New("a statement effect is not a JSON object")
	}
	*e = PlanEffect{}
	var version int
	if raw, present := fields["version"]; !present || json.Unmarshal(raw, &version) != nil {
		return nil
	}
	e.Version = version
	if version != PlanEffectVersion {
		return nil
	}
	// The record type has no methods, so this decode does not come back here.
	type record PlanEffect
	var decoded record
	if json.Unmarshal(data, &decoded) != nil {
		return nil
	}
	*e = PlanEffect(decoded)
	e.read = true
	return nil
}

// securityInvokerHold reads a statement's effects for the security_invoker of
// the view it changes, and reports whether the change is one the privilege
// class raises. described is false when the effects say nothing the operator
// can use, and the statement's SQL is read for the option instead.
//
// The effect is read only where it plainly describes the statement: one record
// the operator reads, for a view, whose action is the statement's own verb in
// a statement that is one view statement however its strings are read, with a
// security_invoker whose values agree with the action and with changed.
// Anything else is read from the SQL, as a plan from a Ptah that writes no
// effects is.
//
// A view that existed before the statement and still does after it holds
// when the option changes, in either direction: its readers either start to
// need their own rights on what it reads, or start to read with its owner's.
// A restated option holds nothing, which is what the effect adds: CREATE OR
// REPLACE VIEW has to state every option the view keeps, so its SQL cannot say
// whether a body change also changed the option. A view the statement brings
// into being holds when it reads with its readers' rights. Every new view
// records the option as changed, from absent to its value, so changed alone
// would hold every new view; one with its owner's rights is held no more than
// a new view that states no option. A DROP VIEW states no option, so its
// effect, action drop, adds nothing to the SQL and is not read.
func securityInvokerHold(statement PlanStatement, dialect string) (hold, described bool) {
	var effect PlanEffect
	read := 0
	for _, candidate := range statement.Effects {
		if candidate.Read() {
			effect = candidate
			read++
		}
	}
	if read != 1 || effect.Object.Kind != PlanEffectKindView {
		return false, false
	}
	if action, ok := viewStatementAction(statement.SQL, dialect); !ok || action != effect.Action {
		return false, false
	}
	var property *PlanEffectProperty
	for index := range effect.Properties {
		if effect.Properties[index].Name != PlanEffectSecurityInvoker {
			continue
		}
		if property != nil {
			return false, false
		}
		property = &effect.Properties[index]
	}
	if property == nil {
		return false, false
	}
	before, beforeOK := effectBoolean(property.Before)
	after, afterOK := effectBoolean(property.After)
	if !beforeOK || !afterOK || property.Changed == sameEffectValue(before, after) {
		return false, false
	}
	switch {
	case after == nil:
		// Each statement read here leaves a view in place.
		return false, false
	case effect.Action == PlanEffectActionCreate && before != nil,
		effect.Action == PlanEffectActionAlter && before == nil:
		// CREATE VIEW makes a view that did not exist, and ALTER VIEW
		// changes one that does. CREATE OR REPLACE VIEW may do either: it
		// also makes a view a statement before it dropped.
		return false, false
	case before == nil:
		return *after, true
	default:
		return *before != *after, true
	}
}

// effectBoolean reads one value of a boolean property. An absent value is nil
// and ok; a value that is not a boolean is not ok.
func effectBoolean(raw json.RawMessage) (*bool, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var value *bool
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, false
	}
	return value, true
}

// sameEffectValue reports whether two values are equal, an absent value equal
// only to another absent one, as Ptah computes changed.
func sameEffectValue(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// viewStatementAction names the effect action a statement's SQL is: CREATE
// VIEW is create, CREATE OR REPLACE VIEW replace and ALTER VIEW alter. ok is
// false unless each reading of the statement is exactly one such statement. The
// readings differ only past a string, and the words that name the action come
// before any. A materialized view is another kind of object, and ALTER TABLE on
// a view is not a statement Ptah describes.
func viewStatementAction(sql, dialect string) (string, bool) {
	action := ""
	for _, escapes := range []bool{false, true} {
		segments := statementSegments(sqlKeywordTokensWithEscapes(sql, dialect, escapes))
		if len(segments) != 1 {
			return "", false
		}
		read, ok := viewSegmentAction(segments[0])
		if !ok {
			return "", false
		}
		action = read
	}
	return action, true
}

func viewSegmentAction(tokens []string) (string, bool) {
	switch tokenAt(tokens, 0) {
	case "ALTER":
		return PlanEffectActionAlter, tokenAt(tokens, 1) == "VIEW"
	case "CREATE":
		action, rest := PlanEffectActionCreate, tokens[1:]
		if tokenAt(rest, 0) == "OR" && tokenAt(rest, 1) == "REPLACE" {
			action, rest = PlanEffectActionReplace, rest[2:]
		}
		for len(rest) > 0 && (rest[0] == "TEMP" || rest[0] == "TEMPORARY" || rest[0] == "RECURSIVE") {
			rest = rest[1:]
		}
		return action, tokenAt(rest, 0) == "VIEW"
	}
	return "", false
}
