#!/bin/sh

set -eu

# Prove the HA phase's custom-metric validator before trusting what it refuses.
#
# That validator decides whether the leader's metrics endpoint carries the
# exact evidence the failover proof expects, and it is a wall of awk inside a
# phase that costs an hour to reach. It has one job nothing else can do --
# refuse a family nobody decided to publish -- and two ways to fail silently:
# accept anything by loosening a pattern, or refuse everything so the loop
# times out with a message about the counter rather than the family.
#
# It is also the check that caught four gauges shipping undocumented, which is
# what it is for. So it is exercised here against readings it must accept and
# readings it must refuse, and those are the mistakes that were actually made.

ROOT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)

# validate runs the phase's own function, read out of the phase script rather
# than copied, so a change there is measured here.
eval "$(sed -n '/^validate_custom_operator_metrics() {$/,/^}$/p' "$ROOT_DIR/hack/e2e-ha.sh")"

failures=0

expect() {
	label=$1
	want=$2
	shift 2
	got=0
	printf '%s\n' "$*" | validate_custom_operator_metrics || got=$?
	if [ "$got" -ne "$want" ]; then
		printf 'e2e ha metrics self-test: %s expected exit %s and got %s\n' "$label" "$want" "$got" >&2
		failures=$((failures + 1))
	fi
}

# The reading the proof is waiting for: both counters present once, and the
# always-present gauges beside them.
complete='# HELP ptah_operator_reconciliations_total Total reconciliations.
# TYPE ptah_operator_reconciliations_total counter
ptah_operator_reconciliations_total{family="schema",result="success"} 4
# HELP ptah_operator_failures_total Total failures.
# TYPE ptah_operator_failures_total counter
ptah_operator_failures_total{category="operation",family="schema",stage="resolve"} 2
# HELP ptah_operator_unresolved_attempts Unaccounted mutations.
# TYPE ptah_operator_unresolved_attempts gauge
ptah_operator_unresolved_attempts{family="schema"} 0
ptah_operator_unresolved_attempts{family="migration"} 1
# HELP ptah_operator_unresolved_owed_seconds Owed for.
# TYPE ptah_operator_unresolved_owed_seconds gauge
ptah_operator_unresolved_owed_seconds{family="migration"} 61
# HELP ptah_operator_unresolved_view_synced View synchronized.
# TYPE ptah_operator_unresolved_view_synced gauge
ptah_operator_unresolved_view_synced 1
# HELP ptah_operator_unresolved_view_read_failures_total Failed reads.
# TYPE ptah_operator_unresolved_view_read_failures_total counter
ptah_operator_unresolved_view_read_failures_total 3'

expect 'the complete post-failover reading' 0 "$complete"

# A gauge at zero is evidence, and the whole point of publishing it: nothing is
# carrying an unaccounted mutation. Refusing it would make the proof depend on
# the cluster being in trouble.
expect 'a view that has not synchronized' 0 '# HELP ptah_operator_reconciliations_total Total reconciliations.
# TYPE ptah_operator_reconciliations_total counter
ptah_operator_reconciliations_total{family="schema",result="success"} 4
# HELP ptah_operator_failures_total Total failures.
# TYPE ptah_operator_failures_total counter
ptah_operator_failures_total{category="operation",family="schema",stage="resolve"} 2
# HELP ptah_operator_unresolved_view_synced View synchronized.
# TYPE ptah_operator_unresolved_view_synced gauge
ptah_operator_unresolved_view_synced 0'

# Still waiting rather than broken: exit 1 is what makes the caller poll.
expect 'a reading before the failure counter appeared' 1 '# HELP ptah_operator_reconciliations_total Total reconciliations.
# TYPE ptah_operator_reconciliations_total counter
ptah_operator_reconciliations_total{family="schema",result="success"} 4'

# A family nobody decided to publish. This is the refusal the check exists for.
expect 'an undeclared custom family' 2 "$complete
# HELP ptah_operator_mystery_total Something nobody decided to ship.
# TYPE ptah_operator_mystery_total counter
ptah_operator_mystery_total 1"

# The same family announced only by its HELP line. Prometheus emits both, but
# each branch has to refuse on its own: a reading that passes because the next
# branch happened to catch it leaves this one free to be loosened silently,
# which is what an earlier version of this self-test missed.
expect 'an undeclared family announced by HELP alone' 2 "$complete
# HELP ptah_operator_mystery_total Something nobody decided to ship."

expect 'an undeclared family announced by TYPE alone' 2 "$complete
# TYPE ptah_operator_mystery_total counter"

expect 'an undeclared family present only as a sample' 2 "$complete
ptah_operator_mystery_total 1"

# A counter at zero is not the evidence this phase measures going up.
expect 'a counter at zero' 2 '# HELP ptah_operator_reconciliations_total Total reconciliations.
# TYPE ptah_operator_reconciliations_total counter
ptah_operator_reconciliations_total{family="schema",result="success"} 0'

# A gauge that arrived declared as a counter is a contract change, not a value.
expect 'an always-present family with the wrong type' 2 "$complete
# TYPE ptah_operator_unresolved_attempts counter"

# A negative gauge is not a population or an age.
expect 'a negative gauge' 2 '# HELP ptah_operator_unresolved_attempts Unaccounted mutations.
# TYPE ptah_operator_unresolved_attempts gauge
ptah_operator_unresolved_attempts{family="schema"} -1'

# A duplicated sample means two collectors answered, which the proof must not
# average over.
expect 'a duplicated counter sample' 2 '# HELP ptah_operator_reconciliations_total Total reconciliations.
# TYPE ptah_operator_reconciliations_total counter
ptah_operator_reconciliations_total{family="schema",result="success"} 4
ptah_operator_reconciliations_total{family="schema",result="success"} 5
# HELP ptah_operator_failures_total Total failures.
# TYPE ptah_operator_failures_total counter
ptah_operator_failures_total{category="operation",family="schema",stage="resolve"} 2'

if [ "$failures" -ne 0 ]; then
	printf 'e2e ha metrics self-test: FAIL (%s)\n' "$failures" >&2
	exit 1
fi

printf 'e2e ha metrics self-test: PASS the leader reading is accepted, and an undeclared family, a zero counter, a wrong type and a negative gauge are refused\n'
