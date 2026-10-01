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
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
LAB_ENVIRONMENT=${LAB_ENVIRONMENT:-$ROOT_DIR/demo/.lab/environment}
WORKLOAD=${CAPACITY_WORKLOAD:-$ROOT_DIR/support/capacity/workload.json}
OUT_DIR=${CAPACITY_OUT_DIR:?set CAPACITY_OUT_DIR to where the report goes}

fail() {
	printf 'capacity: %s\n' "$*" >&2
	exit 1
}

[ -f "$LAB_ENVIRONMENT" ] || fail "no lab at $LAB_ENVIRONMENT; bring one up with make demo-up"
set -a
# shellcheck disable=SC1090 # The lab's own NAME=value file.
. "$LAB_ENVIRONMENT"
set +a
export KUBECONFIG=$E2E_KUBECONFIG
BOOTSTRAP="$ROOT_DIR/support/qualification/probes/capacity_bootstrap.py"
mkdir -p "$OUT_DIR"
OUT_DIR=$(cd "$OUT_DIR" && pwd)
WORKLOAD="$(cd "$(dirname "$WORKLOAD")" && pwd)/$(basename "$WORKLOAD")"
# Keep the ownership journal outside disposable scratch space. A cleanup failure
# must leave enough identity evidence for a safe retry.
STATE_FILE="$OUT_DIR/bootstrap-state.json"
[ ! -e "$STATE_FILE" ] || fail "ownership journal already exists at $STATE_FILE; use a fresh output directory"
WORK_DIR=$(mktemp -d)
umask 077
CAPACITY_COMPLETED=0
cleanup() {
	status=$?
	[ "$status" -ne 0 ] || [ "$CAPACITY_COMPLETED" -eq 1 ] || status=1
	if ! python3 "$BOOTSTRAP" cleanup --state "$STATE_FILE"; then
		printf 'capacity: owned fixtures remain; retry cleanup with %s\n' "$STATE_FILE" >&2
		status=1
	fi
	rm -rf "$WORK_DIR"
	exit "$status"
}
trap cleanup EXIT

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

cd "$ROOT_DIR"
go run ./hack/capacity \
	-kubeconfig "$KUBECONFIG" \
	-workload "$WORKLOAD" \
	-namespace "$CAPACITY_NAMESPACES" \
	-operator-namespace "$E2E_OPERATOR_NAMESPACE" \
	-schema-v1 "oci://$E2E_REGISTRY_HOST/schemas/capacity@$schema_v1" \
	-schema-v2 "oci://$E2E_REGISTRY_HOST/schemas/capacity@$schema_v2" \
	-migration-v1 "oci://$E2E_REGISTRY_HOST/migrations/capacity@$migration_v1" \
	-migration-v2 "oci://$E2E_REGISTRY_HOST/migrations/capacity@$migration_v2" \
	-registry-ip "$E2E_REGISTRY_IP" \
	-out "$OUT_DIR"
CAPACITY_COMPLETED=1
