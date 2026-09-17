#!/bin/sh

set -eu

# Sample the machine while a lifecycle runs.
#
# A stage that took forty minutes says nothing about why. These samples are
# what separates a run that was short of CPU, memory or disk from one that
# spent its time waiting on a condition or on a timer of its own: they are
# taken from the kernel's own counters, at a fixed interval, into a CSV whose
# header names its columns so hack/e2etiming reads them by name.
#
# It samples the host it runs on. That is the machine that carries the kind
# cluster, the registry and the databases in CI, and the one whose limits a
# conclusion about an underpowered runner would be about.
#
# Usage: hack/e2e-resource-samples.sh <output.csv> [interval-seconds]
#
# It runs until it is killed, and the caller keeps its pid. Nothing here can
# fail a run: an unreadable counter is written as an empty field.

unset CDPATH

fail() {
	printf 'e2e samples: %s\n' "$*" >&2
	exit 1
}

[ "$#" -ge 1 ] || fail "usage: hack/e2e-resource-samples.sh <output.csv> [interval-seconds]"
SAMPLE_FILE=$1
SAMPLE_INTERVAL=${2:-15}
printf '%s\n' "$SAMPLE_INTERVAL" | grep -Eq '^[1-9][0-9]*$' ||
	fail "the interval must be a positive number of seconds, got $SAMPLE_INTERVAL"
[ -r /proc/loadavg ] && [ -r /proc/meminfo ] ||
	fail "this sampler reads /proc; run it on the Linux host that carries the cluster"

printf 'timestamp,load1,mem_available_mib,disk_used_percent\n' >"$SAMPLE_FILE"

while :; do
	sample_load=$(awk '{ print $1 }' /proc/loadavg 2>/dev/null || true)
	# MemAvailable is the kernel's own estimate of what a workload can still
	# claim, which is the number a run that was killed for memory ran out of.
	sample_memory=$(awk '/^MemAvailable:/ { printf "%.0f", $2 / 1024 }' /proc/meminfo 2>/dev/null || true)
	# The filesystem the container runtime writes into: images, layers, and
	# every volume a phase creates land there, and a full disk reads as an
	# unrelated failure in whatever phase was running.
	sample_disk=$(df -P /var/lib/docker 2>/dev/null | awk 'NR == 2 { print $5 }' | tr -d '%' || true)
	if [ -z "$sample_disk" ]; then
		sample_disk=$(df -P / 2>/dev/null | awk 'NR == 2 { print $5 }' | tr -d '%' || true)
	fi
	printf '%s,%s,%s,%s\n' \
		"$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$sample_load" "$sample_memory" "$sample_disk" \
		>>"$SAMPLE_FILE" 2>/dev/null || exit 0
	sleep "$SAMPLE_INTERVAL"
done
