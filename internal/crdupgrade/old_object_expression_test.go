package crdupgrade

// White-box testing required: oldObjectExpression is the one rewrite behind
// every derived UPDATE and DELETE contract, and its callers are exercised only
// through whole compiled policies.

import "testing"

func TestOldObjectExpressionMovesBothSpellings(t *testing.T) {
	t.Parallel()
	got := oldObjectExpression(`has(object.spec.x) && dyn(object).spec.y == 1 && dyn(dyn(object).spec.z).w && oldObject.spec.q`)
	want := `has(oldObject.spec.x) && dyn(oldObject).spec.y == 1 && dyn(dyn(oldObject).spec.z).w && oldObject.spec.q`
	if got != want {
		t.Fatalf("oldObjectExpression rewrote to\n%s\nwant\n%s", got, want)
	}
}
