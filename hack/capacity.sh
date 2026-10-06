#!/usr/bin/env bash
# Measure the operator against a declared workload on the demonstration lab.
#
# The lab is the acceptance harness stopped after its bootstrap (make demo-up),
# so the operator measured here is the chart and images this commit builds.
# What this script adds is what the workload needs and the lab does not have: a
# database server in a fixture namespace, two isolated workload namespaces, one
# database and owner login per resource, and the four artifacts the workload
# moves between. Then it runs hack/capacity, which does the measuring.
#
#   make demo-up
#   CAPACITY_OUT_DIR=/tmp/capacity hack/capacity.sh
#
# The workload is support/capacity/workload.json unless CAPACITY_WORKLOAD names
# another. Owned namespaces are removed on exit after workload finalizers finish.
# A failed cleanup preserves its journal and reports the remaining fixtures.
# CAPACITY_REMOVE_LAB=1 removes the entire owned disposable lab instead. CI uses
# this after measurement so it does not wait an hour for result retention before
# deleting the same cluster. Reports must be outside the lab's directories.
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
LAB_ENVIRONMENT=${LAB_ENVIRONMENT:-$ROOT_DIR/demo/.lab/environment}
WORKLOAD=${CAPACITY_WORKLOAD:-$ROOT_DIR/support/capacity/workload.json}
VARIED_INPUTS=${CAPACITY_VARIED_INPUTS:-0}
REMOVE_LAB=${CAPACITY_REMOVE_LAB:-0}
OUT_DIR=${CAPACITY_OUT_DIR:?set CAPACITY_OUT_DIR to where the report goes}

fail() {
	printf 'capacity: %s\n' "$*" >&2
	exit 1
}

case "$VARIED_INPUTS" in
0|1) ;;
*) fail "CAPACITY_VARIED_INPUTS must be 0 or 1" ;;
esac
case "$REMOVE_LAB" in
0|1) ;;
*) fail "CAPACITY_REMOVE_LAB must be 0 or 1" ;;
esac
UNRELATED_ENABLED=$(python3 -c 'import json,sys; print(int(json.load(open(sys.argv[1])).get("unrelatedObjects",False)))' "$WORKLOAD")
SOAK_ENABLED=$(python3 -c 'import json,sys; w=json.load(open(sys.argv[1])); print(int(w.get("soak") is not None or w.get("approvalBacklog",False)))' "$WORKLOAD")
if [ "$SOAK_ENABLED" -eq 1 ] && [ "$VARIED_INPUTS" -ne 1 ]; then
 fail "soak and approval backlog workloads require CAPACITY_VARIED_INPUTS=1"
fi
[ -f "$LAB_ENVIRONMENT" ] || fail "no lab at $LAB_ENVIRONMENT; bring one up with make demo-up"
LAB_ENVIRONMENT="$(cd "$(dirname "$LAB_ENVIRONMENT")" && pwd)/$(basename "$LAB_ENVIRONMENT")"
set -a
# shellcheck disable=SC1090 # The lab's own NAME=value file.
. "$LAB_ENVIRONMENT"
set +a
export KUBECONFIG=$E2E_KUBECONFIG
BOOTSTRAP="$ROOT_DIR/support/qualification/probes/capacity_bootstrap.py"
mkdir -p "$OUT_DIR"
OUT_DIR=$(cd "$OUT_DIR" && pwd)
if [ "$REMOVE_LAB" -eq 1 ]; then
	[ -n "${E2E_WORK_DIR:-}" ] || fail "lab teardown requires its recorded work directory"
	python3 - "$OUT_DIR" "$E2E_WORK_DIR" "$ROOT_DIR/demo/.lab" <<'PY'
import sys
from pathlib import Path
report = Path(sys.argv[1]).resolve()
for directory in map(lambda value: Path(value).resolve(), sys.argv[2:]):
    if report == directory or directory in report.parents:
        sys.exit('capacity: reports must be outside directories removed by lab teardown')
PY
fi
WORKLOAD="$(cd "$(dirname "$WORKLOAD")" && pwd)/$(basename "$WORKLOAD")"
# Keep the ownership journal outside disposable scratch space. A cleanup failure
# must leave enough identity evidence for a safe retry.
STATE_FILE="$OUT_DIR/bootstrap-state.json"
[ ! -e "$STATE_FILE" ] || fail "ownership journal already exists at $STATE_FILE; use a fresh output directory"
[ ! -e "$OUT_DIR/host.json" ] || fail "host evidence already exists; use a fresh output directory"
WORK_DIR=$(mktemp -d)
umask 077
CAPACITY_COMPLETED=0
cleanup() {
	status=$?
	[ "$status" -ne 0 ] || [ "$CAPACITY_COMPLETED" -eq 1 ] || status=1
	if [ "$REMOVE_LAB" -eq 1 ]; then
		if ! LAB_ENVIRONMENT="$LAB_ENVIRONMENT" "$ROOT_DIR/demo/bin/lab" down; then
			printf 'capacity: lab teardown failed; its environment and ownership journal are retained\n' >&2
			status=1
		fi
	elif ! python3 "$BOOTSTRAP" cleanup --state "$STATE_FILE"; then
		printf 'capacity: owned fixtures remain; retry cleanup with %s\n' "$STATE_FILE" >&2
		status=1
	fi
	rm -rf "$WORK_DIR"
	exit "$status"
}
trap cleanup EXIT

# All kind nodes share this daemon. Record its capacity once, separately from
# Kubernetes allocatable totals. Keep the original reading with the report.
# The bootstrap stores its task-specific context in a separate config directory.
DOCKER_CONFIG="${E2E_DOCKER_CONFIG:-${DOCKER_CONFIG:-}}" \
	docker --context "${E2E_DOCKER_CONTEXT:?lab environment must name its Docker context}" info \
	--format '{"dockerID":{{json .ID}},"name":{{json .Name}},"cpus":{{.NCPU}},"memoryBytes":{{.MemTotal}},"architecture":{{json .Architecture}},"os":{{json .OSType}},"observedAt":{{json .SystemTime}}}' \
	> "$OUT_DIR/host.json"

# The helper emits only the namespace list, never credentials or shell code.
CAPACITY_NAMESPACES=$(python3 "$BOOTSTRAP" prepare --state "$STATE_FILE" --workload "$WORKLOAD")
CAPACITY_ENGINE=$(python3 "$BOOTSTRAP" engine --state "$STATE_FILE")
case "$CAPACITY_ENGINE" in
PostgreSQL)
	SCHEMA_DIR="$ROOT_DIR/demo/schemas"
	MIGRATION_DIR="$ROOT_DIR/demo/migrations"
	DIALECT=postgres
	;;
MySQL)
	SCHEMA_DIR="$ROOT_DIR/support/capacity/mysql/schemas"
	MIGRATION_DIR="$ROOT_DIR/support/capacity/mysql/migrations"
	DIALECT=mysql
	;;
*) fail "unsupported workload engine: $CAPACITY_ENGINE" ;;
esac

# The artifacts, published with the Ptah the lab's executor was built from.
eval "$("$ROOT_DIR/demo/bin/lab" credentials)"
export PTAH_OCI_USERNAME PTAH_OCI_PASSWORD PTAH_OCI_REGISTRY
PATH="$("$ROOT_DIR/demo/bin/lab" tools):$PATH"
push_schema() {
	ptah schema push "oci://$PTAH_OCI_REGISTRY/schemas/capacity:$1-$$" \
		--schema-file "$SCHEMA_DIR/$1.sql" --dialect "$DIALECT" --plain-http |
		sed -n 's/^Digest: //p'
}
push_migrations() {
	ptah migrations push "oci://$PTAH_OCI_REGISTRY/migrations/capacity:$1-$$" \
		--migrations-dir "$2" --dir-format ptah --version "$1-$$" --plain-http |
		sed -n 's/^Digest: //p'
}
INPUT_PROBE="$ROOT_DIR/support/qualification/probes/capacity_workload.py"
if [ "$VARIED_INPUTS" -eq 1 ]; then
	CHANGE_BATCH=$(python3 -c 'import json,sys; v=json.load(open(sys.argv[1]))["changeBatch"]; assert type(v) is int and v in (0,5), "varied inputs require changeBatch 0 or 5"; print(v)' "$WORKLOAD")
	python3 "$INPUT_PROBE" --state "$STATE_FILE" --directory "$OUT_DIR/inputs"
	CAPACITY_ARGS=(-inputs "$OUT_DIR/inputs/catalog.json" -checkpoint-probe "$INPUT_PROBE")
	FINAL_ROUND=$(python3 -c 'import json,sys; print((json.load(open(sys.argv[1])).get("soak") or {}).get("rounds",1))' "$WORKLOAD")
else
	schema_v1=$(push_schema v1)
	schema_v2=$(push_schema v2)
	cp -R "$MIGRATION_DIR" "$WORK_DIR/migrations-v2"
	printf 'ALTER TABLE shipments ADD COLUMN note TEXT;\n' >"$WORK_DIR/migrations-v2/0000000003_add_note.up.sql"
	printf 'ALTER TABLE shipments DROP COLUMN note;\n' >"$WORK_DIR/migrations-v2/0000000003_add_note.down.sql"
	migration_v1=$(push_migrations v1 "$MIGRATION_DIR")
	migration_v2=$(push_migrations v2 "$WORK_DIR/migrations-v2")
	for digest in "$schema_v1" "$schema_v2" "$migration_v1" "$migration_v2"; do
		case "$digest" in
		sha256:*) ;;
		*) fail "a push returned no digest" ;;
		esac
	done
	CAPACITY_ARGS=(
		-schema-v1 "oci://$E2E_REGISTRY_HOST/schemas/capacity@$schema_v1"
		-schema-v2 "oci://$E2E_REGISTRY_HOST/schemas/capacity@$schema_v2"
		-migration-v1 "oci://$E2E_REGISTRY_HOST/migrations/capacity@$migration_v1"
		-migration-v2 "oci://$E2E_REGISTRY_HOST/migrations/capacity@$migration_v2"
	)
fi

if [ "$UNRELATED_ENABLED" -eq 1 ]; then
	python3 "$BOOTSTRAP" verify-unrelated --state "$STATE_FILE" --checkpoint before
fi
cd "$ROOT_DIR"
go run ./hack/capacity \
	-kubeconfig "$KUBECONFIG" \
	-host-info "$OUT_DIR/host.json" \
	-checkpoint-state "$STATE_FILE" \
	-workload "$WORKLOAD" \
	-namespace "$CAPACITY_NAMESPACES" \
	-operator-namespace "$E2E_OPERATOR_NAMESPACE" \
	"${CAPACITY_ARGS[@]}" \
	-registry-ip "$E2E_REGISTRY_IP" \
	-out "$OUT_DIR"
if [ "$VARIED_INPUTS" -eq 1 ]; then
	python3 "$INPUT_PROBE" --state "$STATE_FILE" --directory "$OUT_DIR/inputs" --verify-changed "$CHANGE_BATCH" --verify-round "$FINAL_ROUND"
fi
if [ "$UNRELATED_ENABLED" -eq 1 ]; then
	python3 "$BOOTSTRAP" verify-unrelated --state "$STATE_FILE" --checkpoint after
fi
CAPACITY_COMPLETED=1
