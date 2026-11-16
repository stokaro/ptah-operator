package dataplane

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
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
	PlanEffectActionDrop    = "drop"

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
// can use, and the statement's SQL is read for the option instead, beside hold
// where that is true. drops carries what the plan's earlier statements dropped,
// and is moved past this one.
//
// The effect is read only where it plainly describes the statement: every
// record is one the operator reads, exactly one of them is a view's with a
// security_invoker, and that one names the view the SQL names, with the
// statement's own verb, in a statement that is one view statement however its
// strings are read, and with values that agree with the action and with
// changed. Anything else is read from the SQL, as a plan from a Ptah that
// writes no effects is.
//
// A view that existed before the statement and still does after it holds when
// the option changes, in either direction: its readers either start to need
// their own rights on what it reads, or start to read with its owner's. A
// restated option holds nothing, which is what the effect adds: CREATE OR
// REPLACE VIEW has to state every option the view keeps, so its SQL cannot say
// whether a body change also changed the option.
//
// A view the statement brings into being is read against the view an earlier
// statement dropped under its name. Ptah rebuilds a view it cannot change in
// place with DROP VIEW and then CREATE VIEW, and the create's effect records
// the option as absent before, as it does for a view that never existed, so
// the value it replaces is the one the drop's effect recorded: the new view
// holds when its value differs from that one. Where the plan may have dropped
// the view without an effect that says what it held, it holds whatever it
// states: the view it replaces may have read with its readers' rights, and one
// made again with no option reads with its owner's. That is a view named by a
// DROP VIEW whose effect the operator does not read, and any view after a
// statement that may drop views it does not name (see dropsUnnamedViews). The
// same holds for CREATE VIEW and CREATE OR REPLACE VIEW of such a view with no
// effect the operator reads, which is how Ptah writes a CREATE OR REPLACE VIEW
// of a view whose state a CASCADE left uncertain. Until Ptah records what such
// a statement drops (stokaro/ptah#6023), the hold cannot tell a view it took
// from one it left or one that never existed. A view nothing dropped holds when
// it reads with its readers' rights. Every new view records the option as changed, from
// absent to its value, so changed alone would hold every new view; one with its
// owner's rights is held no more than a new view that states no option.
func securityInvokerHold(statement PlanStatement, dialect string, drops *viewDrops) (hold, described bool) {
	view, named := readViewStatement(statement.SQL, dialect)
	if dropsUnnamedViews(statement.SQL, dialect, named) {
		drops.unnamed = true
	}
	if !named {
		return false, false
	}
	invoker, read := describedInvoker(statement.Effects, view)
	switch {
	case view.action == PlanEffectActionDrop:
		// A DROP VIEW states no option, and its SQL raises nothing.
		drops.drop(view.name, invoker, read)
		return false, false
	case !read:
		taken := view.action != PlanEffectActionAlter && drops.mayHaveTaken(view.name)
		drops.forgetNamed(view.name)
		return taken, false
	}
	replaced, known, dropped := drops.recreate(invoker.object, view.name)
	switch {
	case invoker.before != nil:
		return *invoker.before != *invoker.after, true
	case known:
		return replaced != *invoker.after, true
	case dropped:
		return true, true
	default:
		return *invoker.after, true
	}
}

// viewInvoker is a view's security_invoker as one statement's effect records
// it, before the statement and after. An absent value says the view does not
// exist at that point.
type viewInvoker struct {
	object        PlanEffectObject
	before, after *bool
}

// describedInvoker returns the security_invoker the statement's effects record
// for the view it names. ok is false unless they plainly describe the
// statement, as securityInvokerHold says.
func describedInvoker(effects []PlanEffect, view viewStatement) (viewInvoker, bool) {
	var (
		effect   PlanEffect
		property *PlanEffectProperty
		records  int
	)
	for _, candidate := range effects {
		if !candidate.Read() {
			return viewInvoker{}, false
		}
		if candidate.Object.Kind != PlanEffectKindView {
			continue
		}
		var found *PlanEffectProperty
		for index := range candidate.Properties {
			if candidate.Properties[index].Name != PlanEffectSecurityInvoker {
				continue
			}
			if found != nil {
				return viewInvoker{}, false
			}
			found = &candidate.Properties[index]
		}
		if found != nil {
			effect, property = candidate, found
			records++
		}
	}
	if records != 1 || effect.Action != view.action || !viewNamed(effect.Object, view.name) {
		return viewInvoker{}, false
	}
	before, beforeOK := effectBoolean(property.Before)
	after, afterOK := effectBoolean(property.After)
	if !beforeOK || !afterOK || property.Changed == sameEffectValue(before, after) {
		return viewInvoker{}, false
	}
	// CREATE VIEW makes a view that did not exist, ALTER VIEW changes one that
	// does, and DROP VIEW removes one. CREATE OR REPLACE VIEW may do either of
	// the first two: it also makes a view a statement before it dropped.
	var agrees bool
	switch effect.Action {
	case PlanEffectActionCreate:
		agrees = before == nil && after != nil
	case PlanEffectActionReplace:
		agrees = after != nil
	case PlanEffectActionAlter:
		agrees = before != nil && after != nil
	case PlanEffectActionDrop:
		agrees = before != nil && after == nil
	}
	if !agrees {
		return viewInvoker{}, false
	}
	return viewInvoker{object: effect.Object, before: before, after: after}, true
}

// viewDrops follows the views a plan drops, statement by statement in the
// order the plan runs them, so that a statement that creates a view again is
// read against the view it replaces.
type viewDrops struct {
	// held is the security_invoker of each view a drop the operator read the
	// effect of dropped, by the view the effect names.
	held map[PlanEffectObject]bool
	// unread is the views a DROP VIEW dropped without an effect the operator
	// reads, as readQualifiedName spells the names the SQL gives them. Ptah
	// writes none for a view whose state it could not follow, such as one an
	// earlier CASCADE may already have taken, and for a DROP VIEW IF EXISTS of
	// a view the database did not hold.
	unread []string
	// unnamed is set once a statement may have dropped a view it does not
	// name.
	unnamed bool
}

func newViewDrops() *viewDrops {
	return &viewDrops{held: make(map[PlanEffectObject]bool)}
}

// drop records a DROP VIEW of the view name. With read, invoker is what its
// effect recorded the view held.
func (d *viewDrops) drop(name string, invoker viewInvoker, read bool) {
	if read {
		d.unread = slices.DeleteFunc(d.unread, func(unread string) bool { return unread == name })
		d.held[invoker.object] = *invoker.before
		return
	}
	d.forgetNamed(name)
	d.unread = append(d.unread, name)
}

// recreate returns what the plan dropped of the view object, which a statement
// the operator read the effect of now names, and forgets it: the statement
// either makes the view again or changes one that exists. known is true with
// what a drop's effect recorded the view held. dropped is true where the plan
// may have dropped it without such an effect.
func (d *viewDrops) recreate(object PlanEffectObject, name string) (replaced, known, dropped bool) {
	replaced, known = d.held[object]
	dropped = d.unnamed || slices.ContainsFunc(d.unread, func(unread string) bool { return viewNamed(object, unread) })
	delete(d.held, object)
	d.unread = slices.DeleteFunc(d.unread, func(unread string) bool { return unread == name })
	return replaced, known, dropped
}

// mayHaveTaken reports whether the plan may have dropped a view name may name
// without an effect that says what it held, for a statement that makes the
// view and whose own effect the operator does not read.
func (d *viewDrops) mayHaveTaken(name string) bool {
	return d.unnamed || slices.ContainsFunc(d.unread, func(unread string) bool { return sameViewName(unread, name) })
}

// forgetNamed forgets what a drop's effect recorded for any view name may
// name, after a statement whose effect the operator does not read made or
// changed it. The unread drops of other spellings stay, so a later statement
// may still be held for them.
func (d *viewDrops) forgetNamed(name string) {
	maps.DeleteFunc(d.held, func(object PlanEffectObject, _ bool) bool { return viewNamed(object, name) })
	d.unread = slices.DeleteFunc(d.unread, func(unread string) bool { return unread == name })
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

// viewStatement is a statement that is one view statement: the effect action
// its verb is, and the view it names, as readQualifiedName spells the name.
type viewStatement struct {
	action, name string
}

// readViewStatement reads a statement that is one view statement. ok is false
// for any other statement, and for one whose view's name is not read.
func readViewStatement(sql, dialect string) (viewStatement, bool) {
	action, ok := viewStatementAction(sql, dialect)
	if !ok {
		return viewStatement{}, false
	}
	name, ok := readViewName(sql, action)
	return viewStatement{action: action, name: name}, ok
}

// viewStatementAction names the effect action a statement's SQL is: CREATE
// VIEW is create, CREATE OR REPLACE VIEW replace, ALTER VIEW alter and DROP
// VIEW drop. ok is false unless each reading of the statement is exactly one
// such statement. The readings differ only past a string, and the words that
// name the action come before any. A materialized view is another kind of
// object, and ALTER TABLE on a view is not a statement Ptah describes.
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
	case "DROP":
		return PlanEffectActionDrop, tokenAt(tokens, 1) == "VIEW"
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

// readViewName reads the name a view statement gives its view from the
// statement's text: the words viewStatementAction read, IF EXISTS after ALTER
// VIEW and DROP VIEW, and then a name of one or two parts. ok is false where
// anything else comes between them, a comment included, where the name has a
// third part, and where a DROP VIEW names more than one view or goes on past
// CASCADE or RESTRICT.
func readViewName(sql, action string) (string, bool) {
	rest := strings.TrimSpace(sql)
	var ok bool
	switch action {
	case PlanEffectActionAlter, PlanEffectActionDrop:
		verb := "ALTER"
		if action == PlanEffectActionDrop {
			verb = "DROP"
		}
		if rest, ok = readSQLKeywords(rest, verb, "VIEW"); !ok {
			return "", false
		}
		if after, ok := readSQLKeywords(rest, "IF", "EXISTS"); ok {
			rest = after
		}
	default:
		if rest, ok = readSQLKeyword(rest, "CREATE"); !ok {
			return "", false
		}
		if action == PlanEffectActionReplace {
			if rest, ok = readSQLKeywords(rest, "OR", "REPLACE"); !ok {
				return "", false
			}
		}
		for {
			after, ok := readSQLKeyword(rest, "TEMP")
			if !ok {
				after, ok = readSQLKeyword(rest, "TEMPORARY")
			}
			if !ok {
				after, ok = readSQLKeyword(rest, "RECURSIVE")
			}
			if !ok {
				break
			}
			rest = after
		}
		if rest, ok = readSQLKeyword(rest, "VIEW"); !ok {
			return "", false
		}
	}
	name, rest, ok := readQualifiedName(rest)
	if !ok || strings.HasPrefix(rest, ".") {
		return "", false
	}
	if action == PlanEffectActionDrop {
		rest = strings.TrimSuffix(strings.TrimSpace(rest), ";")
		if after, ok := readSQLKeyword(rest, "CASCADE"); ok {
			rest = after
		} else if after, ok := readSQLKeyword(rest, "RESTRICT"); ok {
			rest = after
		}
		if strings.TrimSpace(rest) != "" {
			return "", false
		}
	}
	return name, true
}

// readSQLKeywords consumes each keyword in turn, as readSQLKeyword does, and
// consumes nothing unless every one is there.
func readSQLKeywords(statement string, keywords ...string) (string, bool) {
	rest := statement
	for _, keyword := range keywords {
		var ok bool
		if rest, ok = readSQLKeyword(rest, keyword); !ok {
			return statement, false
		}
	}
	return rest, true
}

// viewNamed reports whether name, as readQualifiedName spells it, names the
// view object: by its name alone, or by its schema and its name.
func viewNamed(object PlanEffectObject, name string) bool {
	quoted := quoteSQLIdentifier(object.Name)
	return name == quoted || object.Schema != "" && name == quoteSQLIdentifier(object.Schema)+"."+quoted
}

// sameViewName reports whether two names, as readQualifiedName spells them,
// may name the same view: the same name, and the same schema where both state
// one. An unqualified name may name a view in any schema.
func sameViewName(a, b string) bool {
	aSchema, aView := splitViewName(a)
	bSchema, bView := splitViewName(b)
	return aView == bView && (aSchema == "" || bSchema == "" || aSchema == bSchema)
}

// splitViewName splits a name readQualifiedName spelled into its schema, empty
// where it states none, and the view's own name.
func splitViewName(name string) (schema, view string) {
	first, rest, _ := readIdentifierPart(name)
	if after, qualified := strings.CutPrefix(rest, "."); qualified {
		return first, after
	}
	return "", first
}

// quoteSQLIdentifier spells an identifier the way readQualifiedName keeps a
// quoted one.
func quoteSQLIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

// dropsUnnamedViews reports whether a statement may drop a view it does not
// name, where Ptah's own description stops following the views it reached: a
// DROP with CASCADE, which also drops what depends on what it drops, DROP
// OWNED, which drops what a role owns, and a DROP VIEW whose name was not read.
// ON DELETE CASCADE and ON UPDATE CASCADE are a foreign key's actions and drop
// nothing.
func dropsUnnamedViews(sql, dialect string, named bool) bool {
	for _, tokens := range statementSegments(sqlKeywordTokens(sql, dialect)) {
		if tokenAt(tokens, 0) == "DROP" && (tokenAt(tokens, 1) == "OWNED" || tokenAt(tokens, 1) == "VIEW" && !named) {
			return true
		}
		if !hasSQLToken(tokens, "DROP") {
			continue
		}
		for index, token := range tokens {
			if previous := tokenAt(tokens, index-1); token == "CASCADE" && previous != "DELETE" && previous != "UPDATE" {
				return true
			}
		}
	}
	return false
}
