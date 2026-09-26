package dataplane

import "slices"

// The kinds of authority a plan statement can change. Each names a kind of
// change and never its object, its grantee or its text: the kinds are copied
// into the plan and into status, where a role name or a function body does not
// belong.
//
// Extending this list is an API change: the CRD enumerates the same values, and
// a stored plan that names a kind the schema does not list stops validating.
const (
	// PrivilegeGrant is a privilege granted on an object, default privileges
	// included.
	PrivilegeGrant = "Grant"
	// PrivilegeRevoke is a privilege revoked from an object.
	PrivilegeRevoke = "Revoke"
	// PrivilegeRoleMembership is a role granted to, or revoked from, another
	// role or user.
	PrivilegeRoleMembership = "RoleMembership"
	// PrivilegeRole is a role, user or group created, altered or dropped -- its
	// attributes, its settings, its existence -- or assumed for the rest of the
	// session with SET ROLE or SET SESSION AUTHORIZATION.
	PrivilegeRole = "Role"
	// PrivilegeOwnership is an object given to another owner.
	PrivilegeOwnership = "Ownership"
	// PrivilegeRowSecurityPolicy is a row-security policy created, altered or
	// dropped, or row security switched off or unforced on a table.
	PrivilegeRowSecurityPolicy = "RowSecurityPolicy"
	// PrivilegeSecurityDefiner is code set to run with its owner's rights
	// rather than its caller's. A MySQL or MariaDB routine does that unless it
	// says SQL SECURITY INVOKER, so a routine that names no mode counts.
	PrivilegeSecurityDefiner = "SecurityDefiner"
	// PrivilegeDefiner is a MySQL DEFINER clause, which names the account a
	// view, routine, trigger or event runs as.
	PrivilegeDefiner = "Definer"
	// PrivilegeFunctionReplacement is CREATE OR REPLACE of a function or a
	// procedure, which can rewrite code that policies, triggers and other
	// roles already call.
	PrivilegeFunctionReplacement = "FunctionReplacement"
)

var privilegeChangeKinds = []string{
	PrivilegeGrant,
	PrivilegeRevoke,
	PrivilegeRoleMembership,
	PrivilegeRole,
	PrivilegeOwnership,
	PrivilegeRowSecurityPolicy,
	PrivilegeSecurityDefiner,
	PrivilegeDefiner,
	PrivilegeFunctionReplacement,
}

// PrivilegeChangeKinds returns a copy of the closed vocabulary, in the order a
// plan lists its kinds.
func PrivilegeChangeKinds() []string {
	return slices.Clone(privilegeChangeKinds)
}

// IsKnownPrivilegeChange reports whether kind belongs to the vocabulary.
func IsKnownPrivilegeChange(kind string) bool {
	return slices.Contains(privilegeChangeKinds, kind)
}

// privilegeChanges returns the kinds of authority a statement changes, in
// vocabulary order.
//
// The reading is conservative in the same way as the destructive one: it looks
// at keywords, never at what a clause evaluates to, so it raises every policy
// rather than guessing which USING clause means true, and every CREATE OR
// REPLACE FUNCTION because the plan does not say whether the function existed.
// A body is read too, which raises a GRANT that only runs when the function is
// called. What it cannot see is SQL built at run time, such as a DO block that
// EXECUTEs a string.
func privilegeChanges(statement, dialect string) []string {
	found := make(map[string]bool)
	for _, tokens := range statementSegments(sqlKeywordTokens(statement, dialect)) {
		classifyPrivilegeSegment(tokens, found)
		if mysqlDialect(dialect) && definerRightsRoutine(tokens) {
			found[PrivilegeSecurityDefiner] = true
		}
	}
	var kinds []string
	for _, kind := range privilegeChangeKinds {
		if found[kind] {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// statementSegments splits a token stream at every boundary, so a rule that
// reads a statement from its first word reads one statement.
func statementSegments(tokens []string) [][]string {
	var segments [][]string
	start := 0
	for index := 0; index <= len(tokens); index++ {
		if index < len(tokens) && tokens[index] != statementBoundary {
			continue
		}
		if index > start {
			segments = append(segments, tokens[start:index])
		}
		start = index + 1
	}
	return segments
}

func classifyPrivilegeSegment(tokens []string, found map[string]bool) {
	for index, token := range tokens {
		previous, next := tokenAt(tokens, index-1), tokenAt(tokens, index+1)
		switch token {
		case "GRANT":
			// WITH GRANT OPTION and REVOKE GRANT OPTION FOR name the option,
			// not a second statement.
			if next != "OPTION" {
				found[grantKind(tokens, index, "TO", PrivilegeGrant)] = true
			}
		case "REVOKE":
			found[grantKind(tokens, index, "FROM", PrivilegeRevoke)] = true
		case "ROLE", "USER", "GROUP":
			switch {
			case previous != "CREATE" && previous != "ALTER" && previous != "DROP":
			case insideAlter(tokens, index-1, "TABLE"):
				// ALTER TABLE t ALTER role, where COLUMN is optional, names a
				// column.
			case insideAlter(tokens, index-1, "GROUP"),
				token == "GROUP" && previous == "ALTER" &&
					(hasSQLTokenSequence(tokens[index:], "ADD", "USER") || hasSQLTokenSequence(tokens[index:], "DROP", "USER")):
				found[PrivilegeRoleMembership] = true
			default:
				found[PrivilegeRole] = true
			}
		case "POLICY":
			if (previous == "CREATE" || previous == "ALTER" || previous == "DROP") && !insideAlter(tokens, index-1, "TABLE") {
				found[PrivilegeRowSecurityPolicy] = true
			}
		case "OWNER":
			if next == "TO" && !renamesTo(tokens, index) {
				found[PrivilegeOwnership] = true
			}
		case "OWNED":
			switch previous {
			case "REASSIGN":
				found[PrivilegeOwnership] = true
			case "DROP":
				// DROP OWNED BY revokes every privilege the role holds.
				found[PrivilegeRevoke] = true
			}
		case "AUTHORIZATION":
			if hasSQLTokenSequence(tokens[:index], "CREATE", "SCHEMA") {
				found[PrivilegeOwnership] = true
			}
		case "SECURITY":
			if next == "DEFINER" {
				found[PrivilegeSecurityDefiner] = true
			}
		case "SET":
			if startsStatement(tokens, index) && assumesIdentity(tokens[index+1:]) {
				found[PrivilegeRole] = true
			}
		case "SECURITY_INVOKER":
			// A PostgreSQL view reverting to its owner's rights.
			if next == "FALSE" || next == "OFF" || previous == "RESET" {
				found[PrivilegeSecurityDefiner] = true
			}
		case "DEFINER":
			if previous != "SECURITY" && definerClause(tokens, index) {
				found[PrivilegeDefiner] = true
			}
		case "REPLACE":
			if previous == "OR" && tokenAt(tokens, index-2) == "CREATE" && namesRoutine(tokens[index+1:]) {
				found[PrivilegeFunctionReplacement] = true
			}
		}
	}
	if hasSQLTokenSequence(tokens, "DISABLE", "ROW", "LEVEL", "SECURITY") ||
		hasSQLTokenSequence(tokens, "NO", "FORCE", "ROW", "LEVEL", "SECURITY") {
		found[PrivilegeRowSecurityPolicy] = true
	}
	if tokenAt(tokens, 0) == "SET" && tokenAt(tokens, 1) == "DEFAULT" && tokenAt(tokens, 2) == "ROLE" {
		// MySQL's SET DEFAULT ROLE decides which granted roles a user holds.
		found[PrivilegeRoleMembership] = true
	}
}

func tokenAt(tokens []string, index int) string {
	if index < 0 || index >= len(tokens) {
		return ""
	}
	return tokens[index]
}

// grantKind decides whether a GRANT or REVOKE is about an object or about a
// role. An object privilege names its object with ON before the grantee; a role
// grant reaches the grantee without one. A statement too short to say is
// counted as the object privilege, which is still privileged.
func grantKind(tokens []string, index int, grantee, objectKind string) string {
	for _, token := range tokens[index+1:] {
		switch token {
		case "ON", "PRIVILEGES":
			return objectKind
		case grantee:
			return PrivilegeRoleMembership
		}
	}
	return objectKind
}

// insideAlter reports whether the word at index is an action inside ALTER of
// the named object rather than the start of a statement of its own: in ALTER
// TABLE, ALTER role and DROP policy name a column, and in ALTER GROUP, DROP USER
// takes a member out.
func insideAlter(tokens []string, index int, object string) bool {
	return index > 0 && hasSQLTokenSequence(tokens[:index], "ALTER", object)
}

// renamesTo reports whether OWNER TO at index is a rename of something called
// owner -- RENAME [COLUMN | CONSTRAINT | ATTRIBUTE] owner TO -- rather than a
// change of owner. A RENAME that follows an object keyword is that object's
// name, as in ALTER TABLE rename OWNER TO, and changes the owner.
func renamesTo(tokens []string, index int) bool {
	switch tokenAt(tokens, index-1) {
	case "COLUMN", "CONSTRAINT", "ATTRIBUTE":
		return true
	case "RENAME":
		switch tokenAt(tokens, index-2) {
		case "TABLE", "VIEW", "SEQUENCE", "INDEX", "SCHEMA", "TYPE", "DOMAIN", "FUNCTION", "PROCEDURE",
			"DATABASE", "EXISTS", "ONLY":
			return false
		}
		return true
	}
	return false
}

// definerClause reports whether DEFINER at index is the clause of a CREATE or
// ALTER of a view, routine, trigger or event, which puts it ahead of the object
// keyword, rather than a column of that name.
func definerClause(tokens []string, index int) bool {
	if first := tokenAt(tokens, 0); first != "CREATE" && first != "ALTER" {
		return false
	}
	if slices.Contains(tokens[:index], "TABLE") {
		return false
	}
	for _, token := range tokens[index+1:] {
		switch token {
		case "VIEW", "PROCEDURE", "FUNCTION", "TRIGGER", "EVENT":
			return true
		}
	}
	return false
}

// startsStatement reports whether the word at index begins a statement: the
// first word of a segment, or the first word after a procedural block opens a
// branch, as in a DO block or a routine body. UPDATE t SET role = ... is not
// one.
func startsStatement(tokens []string, index int) bool {
	switch tokenAt(tokens, index-1) {
	case "", "BEGIN", "THEN", "ELSE", "LOOP":
		return true
	}
	return false
}

// assumesIdentity reports whether the words after SET change the role the
// session acts as, and so everything the rest of the Apply runs as:
// SET [SESSION | LOCAL] ROLE and SET [SESSION | LOCAL] SESSION AUTHORIZATION.
// MySQL's SET DEFAULT ROLE is membership and is read elsewhere.
func assumesIdentity(tokens []string) bool {
	sessionAuthorization := func(tokens []string) bool {
		return tokenAt(tokens, 0) == "SESSION" && tokenAt(tokens, 1) == "AUTHORIZATION"
	}
	// SESSION is both a scope and the first word of SESSION AUTHORIZATION, so
	// the statement is read as written before a scope is set aside.
	if sessionAuthorization(tokens) {
		return true
	}
	if first := tokenAt(tokens, 0); first == "SESSION" || first == "LOCAL" {
		tokens = tokens[1:]
	}
	return tokenAt(tokens, 0) == "ROLE" || sessionAuthorization(tokens)
}

// definerRightsRoutine reports whether a MySQL or MariaDB statement creates a
// stored routine that runs with its definer's rights. The engines default a
// routine to SQL SECURITY DEFINER, so the only routine that does not is one
// that says SQL SECURITY INVOKER; one that names no mode, or DEFINER, does.
//
// The characteristic is looked for anywhere in the statement rather than
// between the name and the body. It cannot appear in a body by accident: SQL
// is a reserved word, so the three words together are only ever the
// characteristic. And each reading of the statement is judged on its own, so a
// quote that hides the characteristic from one reading raises the routine.
func definerRightsRoutine(tokens []string) bool {
	return tokenAt(tokens, 0) == "CREATE" && namesRoutine(tokens[1:]) &&
		!hasSQLTokenSequence(tokens, "SQL", "SECURITY", "INVOKER")
}

// namesRoutine reports whether the words after CREATE, or after CREATE OR
// REPLACE, reach FUNCTION or PROCEDURE before any other object keyword. MySQL
// and MariaDB put a DEFINER clause, and MariaDB AGGREGATE, in between.
func namesRoutine(tokens []string) bool {
	for _, token := range tokens {
		switch token {
		case "FUNCTION", "PROCEDURE":
			return true
		case "VIEW", "TRIGGER", "RULE", "TABLE", "INDEX", "SEQUENCE", "TYPE", "SCHEMA", "DATABASE", "EVENT",
			"LANGUAGE", "TRANSFORM", "PACKAGE", "SERVER", "USER", "ROLE", "RECURSIVE", "MATERIALIZED", "TEMPORARY", "TEMP":
			return false
		}
	}
	return false
}
