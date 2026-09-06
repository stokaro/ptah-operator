package crdupgrade

import "strings"

// oldObjectExpression rewrites a create-time contract so it evaluates the
// retained object of an UPDATE or DELETE. Both spellings of the incoming object
// move: the typed field access object.spec and the dyn(object) wrapper that
// relaxes it. A DELETE carries no object, so a reference either rewrite misses
// evaluates dyn(null).spec, the validation errors, and the policy denies.
//
// The chart performs the same rewrite in ptah-operator.oldObjectExpression;
// the rendered-versus-compiled contract tests hold the two together.
func oldObjectExpression(expression string) string {
	return strings.ReplaceAll(strings.ReplaceAll(expression, "dyn(object)", "dyn(oldObject)"), "object.", "oldObject.")
}
