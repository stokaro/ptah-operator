#!/usr/bin/env bash
# Measure the operator against a declared workload on the demonstration lab.
#
# The lab is the acceptance harness stopped after its bootstrap (make demo-up),
# so the operator measured here is the chart and images this commit builds.
# What this script adds is what the workload needs and the lab does not have: a
# PostgreSQL of its own, with one database per resource so no two resources
# share a realm or an advisory lock, and the four artifacts the workload moves
# between. Then it runs hack/capacity, which does the measuring.
#
#   make demo-up
#   CAPACITY_OUT_DIR=/tmp/capacity hack/capacity.sh
#
# The workload is support/capacity/workload.json unless CAPACITY_WORKLOAD names
# another. Everything this script creates it removes on exit.
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
NAMESPACE=$E2E_TEST_NAMESPACE

k() {
	kubectl "$@"
}

schemas=$(jq -er '.schemas' "$WORKLOAD")
migrations=$(jq -er '.migrations' "$WORKLOAD")
# One database per resource, and one more for the resource the restart burst
# approves.
databases=$((schemas + migrations + 1))

WORK_DIR=$(mktemp -d)
# A refused ${VAR:?...} or an unset name under set -u ends the shell without
# setting $?, so the handler trusts only a run that reached its end.
CAPACITY_COMPLETED=0
cleanup() {
	status=$?
	[ "$status" -ne 0 ] || [ "$CAPACITY_COMPLETED" -eq 1 ] || status=1
	k -n "$NAMESPACE" delete ptahmigrationapproval,ptahmigration,ptahschema \
		-l operator.ptah.run/capacity --ignore-not-found --wait=false >/dev/null 2>&1 || true
	k -n "$NAMESPACE" delete ptahmigrationapproval,ptahmigration capacity-approval \
		--ignore-not-found --wait=false >/dev/null 2>&1 || true
	k -n "$NAMESPACE" delete networkpolicy capacity-registry-outage --ignore-not-found >/dev/null 2>&1 || true
	k -n "$NAMESPACE" delete deployment,service capacity-postgres --ignore-not-found >/dev/null 2>&1 || true
	k -n "$NAMESPACE" delete secret -l operator.ptah.run/capacity-database --ignore-not-found >/dev/null 2>&1 || true
	rm -rf "$WORK_DIR"
	exit "$status"
}
trap cleanup EXIT

# The PostgreSQL the workload runs against, from the image the acceptance suite
# mirrors, so it is the version this repository supports.
password=$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')
jq -n \
	--arg namespace "$NAMESPACE" \
	--arg image "$E2E_POSTGRES_IMAGE" \
	--arg password "$password" '
  def labels: {"app.kubernetes.io/name": "capacity-postgres"};
  {
    apiVersion: "v1", kind: "List",
    items: [
      {
        apiVersion: "apps/v1", kind: "Deployment",
        metadata: {namespace: $namespace, name: "capacity-postgres"},
        spec: {
          replicas: 1,
          selector: {matchLabels: labels},
          template: {
            metadata: {labels: labels},
            spec: {
              automountServiceAccountToken: false,
              containers: [{
                name: "postgres", image: $image, imagePullPolicy: "IfNotPresent",
                args: ["-c", "max_connections=500"],
                env: [{name: "POSTGRES_PASSWORD", value: $password}],
                ports: [{name: "postgresql", containerPort: 5432}],
                readinessProbe: {exec: {command: ["pg_isready", "-U", "postgres"]}, periodSeconds: 3}
              }]
            }
          }
        }
      },
      {
        apiVersion: "v1", kind: "Service",
        metadata: {namespace: $namespace, name: "capacity-postgres"},
        spec: {selector: labels, ports: [{name: "postgresql", port: 5432, targetPort: "postgresql"}]}
      }
    ]
  }' >"$WORK_DIR/postgres.json"
k apply -f "$WORK_DIR/postgres.json" >/dev/null
k -n "$NAMESPACE" rollout status deployment/capacity-postgres --timeout=300s >/dev/null

host="capacity-postgres.${NAMESPACE}.svc.cluster.local"
index=0
while [ "$index" -lt "$databases" ]; do
	database=$(printf 'capacity_%03d' "$index")
	k -n "$NAMESPACE" exec deploy/capacity-postgres -- \
		psql -U postgres -qc "CREATE DATABASE $database" >/dev/null
	k -n "$NAMESPACE" create secret generic "capacity-db-$index" \
		--from-literal=url="postgres://postgres:${password}@${host}:5432/${database}?sslmode=disable" \
		--dry-run=client -o json |
		jq '.metadata.labels = {"operator.ptah.run/capacity-database": "true"}' |
		k apply -f - >/dev/null
	index=$((index + 1))
done

# The artifacts, published with the Ptah the lab's executor was built from.
eval "$("$ROOT_DIR/demo/bin/lab" credentials)"
export PTAH_OCI_USERNAME PTAH_OCI_PASSWORD PTAH_OCI_REGISTRY
PATH="$("$ROOT_DIR/demo/bin/lab" tools):$PATH"
push_schema() {
	ptah schema push "oci://$PTAH_OCI_REGISTRY/schemas/capacity:$1-$$" \
		--schema-file "$ROOT_DIR/demo/schemas/$1.sql" --dialect postgres --plain-http |
		sed -n 's/^Digest: //p'
}
push_migrations() {
	ptah migrations push "oci://$PTAH_OCI_REGISTRY/migrations/capacity:$1-$$" \
		--migrations-dir "$2" --dir-format ptah --version "$1-$$" --plain-http |
		sed -n 's/^Digest: //p'
}
schema_v1=$(push_schema v1)
schema_v2=$(push_schema v2)
cp -R "$ROOT_DIR/demo/migrations" "$WORK_DIR/migrations-v2"
printf 'ALTER TABLE shipments ADD COLUMN note TEXT;\n' >"$WORK_DIR/migrations-v2/0000000003_add_note.up.sql"
printf 'ALTER TABLE shipments DROP COLUMN note;\n' >"$WORK_DIR/migrations-v2/0000000003_add_note.down.sql"
migration_v1=$(push_migrations v1 "$ROOT_DIR/demo/migrations")
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
	-namespace "$NAMESPACE" \
	-operator-namespace "$E2E_OPERATOR_NAMESPACE" \
	-schema-v1 "oci://$E2E_REGISTRY_HOST/schemas/capacity@$schema_v1" \
	-schema-v2 "oci://$E2E_REGISTRY_HOST/schemas/capacity@$schema_v2" \
	-migration-v1 "oci://$E2E_REGISTRY_HOST/migrations/capacity@$migration_v1" \
	-migration-v2 "oci://$E2E_REGISTRY_HOST/migrations/capacity@$migration_v2" \
	-registry-ip "$E2E_REGISTRY_IP" \
	-out "$OUT_DIR"
CAPACITY_COMPLETED=1
