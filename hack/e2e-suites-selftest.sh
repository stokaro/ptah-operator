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

[ "$(runs_phase 'migrations-postgresql' 'dataplane' migrations-postgresql)" = yes ] ||
	fail "a suite does not run a phase it claims"
[ "$(runs_phase 'migrations-postgresql' 'dataplane' dataplane)" = yes ] ||
	fail "a suite does not run the phase it prepares with"
[ "$(runs_phase 'migrations-postgresql' 'dataplane' assert)" = no ] ||
	fail "a suite runs a phase that belongs to another suite"
[ "$(runs_phase 'migrations-postgresql' '' dataplane)" = no ] ||
	fail "a suite with no preparation runs another suite's phase"
# Migration engines run in separate suites. Their phases must not reach the
# other engine or reference-data phases owned by other suites.
[ "$(runs_phase 'migrations-postgresql' 'dataplane' migrations-mysql)" = no ] ||
	fail "an engine's suite runs the other engine's migration phase"
[ "$(runs_phase 'migrations-mysql' 'dataplane' reference-data-postgresql)" = no ] ||
	fail "a migration suite runs a reference-data phase it does not own"
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

# Whether a run's cluster carries the isolation worker, decided by the driver's
# own function against the real catalog: exactly the suites that declare it,
# every phase in one cluster whenever any suite does, and never a bootstrap that
# stops before its phases. A catalog where no suite declares it is the refusal
# the "all" branch has to be able to give.
ISOLATION_FUNCTIONS_FILE=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-suites-isolation.XXXXXX")
NO_ISOLATION_CATALOG=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-suites-catalog.XXXXXX")
SUITE_NAMES_FILE=$(mktemp "${TMPDIR:-/tmp}/ptah-e2e-suites-names.XXXXXX")
trap 'rm -f -- "$FUNCTIONS_FILE" "$ISOLATION_FUNCTIONS_FILE" "$NO_ISOLATION_CATALOG" "$SUITE_NAMES_FILE"' EXIT
awk '
	/^suite_isolation_worker\(\) \{$/ { capture = 1 }
	capture { print }
	capture && /^\}$/ { exit }
' "$ROOT_DIR/hack/e2e-kind.sh" >"$ISOLATION_FUNCTIONS_FILE"
grep -q '^suite_isolation_worker() {' "$ISOLATION_FUNCTIONS_FILE" ||
	fail "the extraction from hack/e2e-kind.sh does not carry suite_isolation_worker"
jq '.suites |= map(del(.isolationWorker))' "$CATALOG" >"$NO_ISOLATION_CATALOG"

# shellcheck disable=SC2034 # Read by the function this sources.
isolation_worker() {
	(
		E2E_SUITE=$1
		E2E_STOP_AFTER=$2
		SUITE_CATALOG=$3
		# shellcheck source=/dev/null
		. "$ISOLATION_FUNCTIONS_FILE"
		suite_isolation_worker
	)
}

# Redirected rather than piped, so a fail inside the loop ends this shell.
isolation_declarations=0
printf '%s\n' "$catalog_suites" >"$SUITE_NAMES_FILE"
while IFS= read -r suite_name; do
	[ -n "$suite_name" ] || continue
	declared=$(jq -r --arg suite "$suite_name" \
		'.suites[] | select(.name == $suite) | .isolationWorker == true' "$CATALOG")
	[ "$declared" = false ] || isolation_declarations=$((isolation_declarations + 1))
	[ "$(isolation_worker "$suite_name" '' "$CATALOG")" = "$declared" ] ||
		fail "suite $suite_name declares isolationWorker $declared, and the driver does not give its cluster that"
	[ "$(isolation_worker "$suite_name" bootstrap "$CATALOG")" = false ] ||
		fail "a bootstrap of suite $suite_name that runs no phase gets the isolation worker"
done <"$SUITE_NAMES_FILE"
# Held to a declaration the catalog actually makes, or every comparison above
# compared false with false.
[ "$isolation_declarations" -gt 0 ] ||
	fail "no suite in the catalog declares the isolation worker, so nothing above measured a cluster that has one"
[ "$(isolation_worker all '' "$CATALOG")" = true ] ||
	fail "running every phase in one cluster does not get the isolation worker a suite declares"
[ "$(isolation_worker all bootstrap "$CATALOG")" = false ] ||
	fail "a bootstrap of every phase that runs none of them gets the isolation worker"
[ "$(isolation_worker all '' "$NO_ISOLATION_CATALOG")" = false ] ||
	fail "running every phase in one cluster gets the isolation worker where no suite declares it"
[ "$(isolation_worker migrations-mysql '' "$NO_ISOLATION_CATALOG")" = false ] ||
	fail "a suite gets the isolation worker the catalog no longer declares for it"

# The data plane is the phase another suite prepares with, and preparation is a
# mode of that phase rather than a copy of its setup. Where the boundary falls
# is declared in test/e2e/phases, where TestDataPlanePreparesBeforeItsOwnAcceptance
# holds it before the engine lifecycles and the fault injection, and the
# harness refuses a run that stops anywhere else.
# shellcheck disable=SC2016 # Match the literal mode bindings in the harness.
grep -Fq 'E2E_DATAPLANE_MODE=$DATAPLANE_MODE' "$ROOT_DIR/hack/e2e-kind.sh" ||
	fail "the driver does not tell the data plane which mode to run in"

# Two suites run each engine-named phase, so the engine is an input rather
# than a default: a phase that picked one on its own would cover one engine
# and report the coverage of two. The migration and reference-data phases are
# Go tests that refuse an engine other than their own, and
# hack/verify-kubernetes-support.go holds each call to it.
# Every engine-named phase the driver runs binds the engine its name says. A
# name and a binding that disagree would run one engine twice and skip the
# other, and both jobs would pass.
for engine_phase in migrations migration-runtime reference-data; do
	for phase_engine in postgresql mysql; do
		engine_binding=$(grep -B1 -E "^[[:space:]]*run_recorded_phase ${engine_phase}-${phase_engine} " \
			"$ROOT_DIR/hack/e2e-kind.sh" | head -1)
		[ "$engine_binding" = "E2E_ENGINE=$phase_engine \\" ] ||
			fail "phase ${engine_phase}-${phase_engine} is invoked with [$engine_binding] rather than its own engine"
	done
done

printf '%s\n' 'e2e suites self-test: PASS every suite runs the phases it claims, and the driver runs no phase outside one'
