#!/bin/sh

set -eu

# Prove that a suite runs exactly the phases it claims.
#
# The matrix is now one job per Kubernetes minor and suite, and each job runs a
# subset of the lifecycle. Two ways that goes wrong: a job that runs a phase
# belonging to another suite pays for it twice, and a job that skips a phase no
# other suite claims leaves it unproven while every check stays green.
#
# The catalog is checked in Go (hack/e2e_suites_test.go). This measures the
# shell that reads it: the selection the driver performs for each suite, and the
# guard every phase invocation passes through.
#
# Usage: hack/e2e-suites-selftest.sh

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
CATALOG=$ROOT_DIR/support/e2e-suites.json

fail() {
	printf 'e2e suites self-test: %s\n' "$*" >&2
	exit 1
}

[ -f "$CATALOG" ] || fail "the acceptance suite catalog is missing: $CATALOG"

# The guard the driver applies to every phase, extracted from the driver so this
# measures what runs rather than a copy of it.
FUNCTIONS_FILE=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-suites-selftest.XXXXXX")
trap 'rm -f -- "$FUNCTIONS_FILE"' EXIT
awk '
	/^suite_runs_phase\(\) \{$/ { capture = 1 }
	capture { print }
	capture && /^\}$/ { exit }
' "$ROOT_DIR/hack/e2e-kind.sh" >"$FUNCTIONS_FILE"
grep -q '^suite_runs_phase() {' "$FUNCTIONS_FILE" ||
	fail "the extraction from hack/e2e-kind.sh does not carry suite_runs_phase"

# The two names below are read by the guard this sources, which shellcheck
# cannot follow, and the literals matched further down are the driver's own.
# shellcheck disable=SC2034,SC2016
runs_phase() {
	(
		SUITE_PHASES=$1
		SUITE_PREPARE_PHASES=$2
		# shellcheck source=/dev/null
		. "$FUNCTIONS_FILE"
		if suite_runs_phase "$3"; then printf 'yes\n'; else printf 'no\n'; fi
	)
}

[ "$(runs_phase 'migrations-postgresql reference-data-postgresql' 'dataplane' migrations-postgresql)" = yes ] ||
	fail "a suite does not run a phase it claims"
[ "$(runs_phase 'migrations-postgresql reference-data-postgresql' 'dataplane' dataplane)" = yes ] ||
	fail "a suite does not run the phase it prepares with"
[ "$(runs_phase 'migrations-postgresql reference-data-postgresql' 'dataplane' assert)" = no ] ||
	fail "a suite runs a phase that belongs to another suite"
[ "$(runs_phase 'migrations-postgresql reference-data-postgresql' '' dataplane)" = no ] ||
	fail "a suite with no preparation runs another suite's phase"
# The engine split is two suites over the same two scripts, so the phase names
# are what keeps them apart. One engine's suite must not reach the other's.
[ "$(runs_phase 'migrations-postgresql reference-data-postgresql' 'dataplane' migrations-mysql)" = no ] ||
	fail "an engine's suite runs the other engine's migration phase"
[ "$(runs_phase 'migrations-mysql reference-data-mysql' 'dataplane' reference-data-postgresql)" = no ] ||
	fail "an engine's suite runs the other engine's reference-data phase"
# A phase whose name is a prefix or a suffix of a claimed one is not claimed:
# the membership test is on whole words, and " $list " is what makes it so.
[ "$(runs_phase 'reference-data' '' reference)" = no ] ||
	fail "a phase name that is a prefix of a claimed one was treated as claimed"
[ "$(runs_phase 'dataplane' '' plane)" = no ] ||
	fail "a phase name that is a suffix of a claimed one was treated as claimed"

# The selection the driver performs, run here as the driver runs it: the same jq
# expressions against the real catalog. Every suite selects its own phases, and
# the union of every suite's phases is what all selects.
catalog_suites=$(jq -r '.suites[].name' "$CATALOG")
printf '%s\n' "$catalog_suites" | while IFS= read -r suite_name; do
	[ -n "$suite_name" ] || continue
	selected=$(jq -r --arg suite "$suite_name" \
		'.suites[] | select(.name == $suite) | .phases | join(" ")' "$CATALOG")
	[ -n "$selected" ] || fail "suite $suite_name selects no phase"
	for selected_phase in $selected; do
		grep -Eq "^[[:space:]]*run_recorded_phase $selected_phase " "$ROOT_DIR/hack/e2e-kind.sh" ||
			fail "suite $suite_name claims phase $selected_phase, which the driver never invokes"
	done
done

all_phases=$(jq -r '[.suites[].phases[]] | join(" ")' "$CATALOG")
driver_phases=$(grep -Eo '^[[:space:]]*run_recorded_phase [a-z][a-z0-9-]*' "$ROOT_DIR/hack/e2e-kind.sh" |
	awk '{ print $2 }' | LC_ALL=C sort -u)
for driver_phase in $driver_phases; do
	case " $all_phases " in
		*" $driver_phase "*) ;;
		*) fail "the driver runs phase $driver_phase, which no suite claims" ;;
	esac
done

# The data plane is the phase another suite prepares with, and preparation is a
# mode of that phase rather than a copy of its setup.
# shellcheck disable=SC2016 # Match the literal mode bindings in the harness.
grep -Fq 'E2E_DATAPLANE_MODE=$DATAPLANE_MODE' "$ROOT_DIR/hack/e2e-kind.sh" ||
	fail "the driver does not tell the data plane which mode to run in"
# shellcheck disable=SC2016 # Match the literal boundary in the phase.
grep -Fq 'if [ "$E2E_DATAPLANE_MODE" = prepare ]; then' "$ROOT_DIR/hack/e2e-dataplane.sh" ||
	fail "the data plane has no preparation boundary"
# The boundary stops before the phase's own acceptance: the engine lifecycles
# and the fault injection are what must not run in preparation mode.
# shellcheck disable=SC2016 # Match the literal boundary in the phase.
boundary_line=$(grep -n 'if \[ "\$E2E_DATAPLANE_MODE" = prepare \]; then' \
	"$ROOT_DIR/hack/e2e-dataplane.sh" | cut -d: -f1)
for acceptance_call in 'run_engine_lifecycle postgresql' 'run_engine_lifecycle mysql' \
	'hack/e2e-faults.sh'; do
	acceptance_line=$(grep -n -F -- "$acceptance_call" "$ROOT_DIR/hack/e2e-dataplane.sh" |
		tail -1 | cut -d: -f1)
	[ -n "$acceptance_line" ] ||
		fail "the data plane no longer runs $acceptance_call"
	[ "$acceptance_line" -gt "$boundary_line" ] ||
		fail "$acceptance_call runs before the preparation boundary, so preparation would execute it"
done

# Two suites run the same two scripts, so the engine is an input rather than a
# default: a phase that picked one on its own would cover one engine and report
# the coverage of two. Each script reads the engine the driver names and refuses
# a run that names none.
for engine_phase_script in e2e-migrations.sh e2e-reference-data.sh; do
	# shellcheck disable=SC2016 # Match the literal import in each phase.
	grep -Fq 'PHASE_ENGINE=${E2E_ENGINE:-}' "$ROOT_DIR/hack/$engine_phase_script" ||
		fail "$engine_phase_script does not read the engine the driver names"
	grep -Fq 'fail "E2E_ENGINE must name postgresql or mysql, and names' \
		"$ROOT_DIR/hack/$engine_phase_script" ||
		fail "$engine_phase_script does not refuse a run with no engine named"
	# shellcheck disable=SC2016 # Match the literal selection in each phase.
	grep -Fq 'if [ "$PHASE_ENGINE" = postgresql ]; then' "$ROOT_DIR/hack/$engine_phase_script" ||
		fail "$engine_phase_script does not select its scenarios by engine"
done
# Every engine-named phase the driver runs binds the engine its name says. A
# name and a binding that disagree would run one engine twice and skip the
# other, and both jobs would pass.
for engine_phase in migrations reference-data; do
	for phase_engine in postgresql mysql; do
		engine_binding=$(grep -B1 -E "^[[:space:]]*run_recorded_phase ${engine_phase}-${phase_engine} " \
			"$ROOT_DIR/hack/e2e-kind.sh" | head -1)
		[ "$engine_binding" = "E2E_ENGINE=$phase_engine \\" ] ||
			fail "phase ${engine_phase}-${phase_engine} is invoked with [$engine_binding] rather than its own engine"
	done
done

printf '%s\n' 'e2e suites self-test: PASS every suite runs the phases it claims, and the driver runs no phase outside one'
