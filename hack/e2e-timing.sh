#!/bin/sh

# The lifecycle's own stopwatch, sourced by the driver and by every phase it
# runs.
#
# Each stage a run spends time in appends one line to the ledger named by
# E2E_TIMING_LEDGER: what the stage was, when it started, how long it took and
# how it ended. The phases are separate processes appending to the same file, so
# a row is one short line written with a single append rather than a document
# rewritten in place.
#
# Nothing here can decide a run. A stage that failed still fails: the outcome is
# passed in by the caller that already knows it, every write is guarded, and a
# ledger that cannot be written is reported once and skipped. The one call that
# sits after a measured command preserves that command's exit status, so adding
# a measurement to a sequence cannot change what the sequence reports.
#
# Rows carry no identity, on purpose: the commit, the pinned Ptah, the
# Kubernetes minor and the suite belong to the run rather than to each of its
# forty stages. The driver writes them once into E2E_TIMING_CONTEXT, and
# hack/e2etiming joins the two when it publishes them.

TIMING_OPEN_KIND=
TIMING_OPEN_NAME=
TIMING_OPEN_START=
TIMING_OPEN_STAMP=
TIMING_REPORTED_FAILURE=0

# timing_enabled reports whether a ledger was named. A phase script run by hand
# names none, and then every call here does nothing.
timing_enabled() {
	[ -n "${E2E_TIMING_LEDGER:-}" ]
}

timing_epoch() {
	date -u +%s
}

timing_instant() {
	date -u +%Y-%m-%dT%H:%M:%SZ
}

# timing_label keeps a row parseable without a JSON encoder. Every name in the
# harness is a literal written here, so this replaces what would have to be
# escaped rather than escaping it, and says so in the ledger.
timing_label() {
	printf '%s' "$1" | tr -c 'A-Za-z0-9 ._:/=+-' '-'
}

timing_row() {
	timing_enabled || return 0
	# The silencing redirect comes first on purpose: a ledger in a directory
	# that is gone fails at the append, and the shell reports that itself.
	if ! printf '{"kind":"%s","name":"%s","outcome":"%s","start":"%s","end":"%s","seconds":%s}\n' \
		"$(timing_label "$1")" "$(timing_label "$2")" "$(timing_label "$3")" \
		"$4" "$5" "$6" 2>/dev/null >>"$E2E_TIMING_LEDGER"; then
		if [ "$TIMING_REPORTED_FAILURE" -eq 0 ]; then
			TIMING_REPORTED_FAILURE=1
			printf 'e2e timing: %s cannot be appended to; stage durations are lost, the run is not\n' \
				"$E2E_TIMING_LEDGER" >&2
		fi
	fi
}

# timing_begin opens a stage. An already-open stage is closed as interrupted
# rather than dropped: a stage with no end is the shape a crash leaves, and it
# should read as one.
timing_begin() {
	timing_enabled || return 0
	if [ -n "$TIMING_OPEN_NAME" ]; then
		timing_row "$TIMING_OPEN_KIND" "$TIMING_OPEN_NAME" interrupted \
			"$TIMING_OPEN_STAMP" "$(timing_instant)" "$(($(timing_epoch) - TIMING_OPEN_START))"
	fi
	TIMING_OPEN_KIND=$1
	TIMING_OPEN_NAME=$2
	TIMING_OPEN_START=$(timing_epoch)
	TIMING_OPEN_STAMP=$(timing_instant)
}

# timing_end closes the open stage with the outcome the caller names, and
# returns the status it was called with. That is what lets
#
#	some_measured_command
#	timing_end pass
#
# leave the exit status of the command alone.
timing_end() {
	timing_status=$?
	timing_enabled || return "$timing_status"
	[ -n "$TIMING_OPEN_NAME" ] || return "$timing_status"
	timing_row "$TIMING_OPEN_KIND" "$TIMING_OPEN_NAME" "$1" \
		"$TIMING_OPEN_STAMP" "$(timing_instant)" "$(($(timing_epoch) - TIMING_OPEN_START))"
	TIMING_OPEN_KIND=
	TIMING_OPEN_NAME=
	TIMING_OPEN_START=
	TIMING_OPEN_STAMP=
	return "$timing_status"
}

# timing_next closes the open stage as passed and opens the next one. The
# bootstrap is a sequence rather than a set of calls, so its boundaries are
# where one stage ends and another begins.
timing_next() {
	timing_status=$?
	timing_enabled || return "$timing_status"
	if [ -n "$TIMING_OPEN_NAME" ]; then
		timing_row "$TIMING_OPEN_KIND" "$TIMING_OPEN_NAME" pass \
			"$TIMING_OPEN_STAMP" "$(timing_instant)" "$(($(timing_epoch) - TIMING_OPEN_START))"
	fi
	TIMING_OPEN_KIND=$1
	TIMING_OPEN_NAME=$2
	TIMING_OPEN_START=$(timing_epoch)
	TIMING_OPEN_STAMP=$(timing_instant)
	return "$timing_status"
}

# timing_abandon closes whatever was open with the outcome a failing run left
# behind. The driver's exit handler calls it, so a run that died in a stage
# still reports where its time went.
timing_abandon() {
	timing_status=$?
	timing_enabled || return "$timing_status"
	[ -n "$TIMING_OPEN_NAME" ] || return "$timing_status"
	timing_row "$TIMING_OPEN_KIND" "$TIMING_OPEN_NAME" "${1:-fail}" \
		"$TIMING_OPEN_STAMP" "$(timing_instant)" "$(($(timing_epoch) - TIMING_OPEN_START))"
	TIMING_OPEN_KIND=
	TIMING_OPEN_NAME=
	TIMING_OPEN_START=
	TIMING_OPEN_STAMP=
	return "$timing_status"
}
