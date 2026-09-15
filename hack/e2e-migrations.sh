#!/bin/sh

set -eu

# A rerun with debug logging traces this phase without a source change. GitHub
# sets RUNNER_DEBUG=1 for "Re-run with debug logging", and E2E_TRACE=1 does the
# same locally. PS4 is single-quoted so each prefix is expanded at the traced
# command, not here.
#
# dash, which is /bin/sh on the runner, has no LINENO: the reference would stay
# literal in every prefix and, under set -u, print "LINENO: parameter not set"
# before each traced command. So ask the shell, and name the script alone when
# it cannot number the line.
if [ "${RUNNER_DEBUG:-0}" = 1 ] || [ "${E2E_TRACE:-0}" = 1 ]; then
	# shellcheck disable=SC3028 # Read only where the shell sets it; the else branch is the shell that does not.
	if [ -n "${LINENO:-}" ]; then
		PS4='+ ${0##*/}:${LINENO}: '
	else
		PS4='+ ${0##*/}: '
	fi
	set -x
fi

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)

KUBECONFIG_FILE=${E2E_KUBECONFIG:-}
TEST_NAMESPACE=${E2E_TEST_NAMESPACE:-}
EXECUTOR_IMAGE=${E2E_EXECUTOR_IMAGE:-}
RUNNER_IMAGE=${E2E_RUNNER_IMAGE:-}
CONTROLLER_IMAGE=${E2E_CONTROLLER_IMAGE:-}
CONTROLLER_REVISION=${E2E_CONTROLLER_REVISION:-}
CONTROLLER_STATE_VERSION=${E2E_CONTROLLER_STATE_VERSION:-}
REGISTRY_SERVICE=${E2E_REGISTRY_SERVICE:-registry}
INTERVAL=${E2E_MIGRATION_INTERVAL:-5m}
TIMEOUT_SECONDS=${E2E_TIMEOUT_SECONDS:-600}

# Imported variables retain their export attribute across reassignment in POSIX
# shells. Clear every secret-bearing name before loading task values.
unset PG_PASSWORD MIGRATION_DB_URL

# A command that fails outside a guard calling fail ends the shell with no
# reason printed, and the EXIT trap then dumps diagnostics that explain
# nothing. So fail records that it spoke, in a file rather than a variable
# so that a fail inside a subshell still counts, and the trap says so when
# nothing did.
PHASE_REASON_MARKER=${TMPDIR:-/tmp}/ptah-e2e-reason-migrations.$$

fail() {
	printf 'e2e migrations: %s\n' "$*" >&2
	if [ -n "${PHASE_REASON_MARKER:-}" ]; then
		: >"$PHASE_REASON_MARKER" 2>/dev/null || true
	fi
	exit 1
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || fail "required command is not installed: $1"
}

is_pinned_image() {
	printf '%s\n' "$1" | grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$'
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum | awk '{print $1}'
		return
	fi
	shasum -a 256 | awk '{print $1}'
}

for command_name in kubectl jq awk sed grep tr mktemp date go env base64; do
	require_command "$command_name"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	fail "sha256sum or shasum is required"
fi
for value_name in \
	KUBECONFIG_FILE TEST_NAMESPACE EXECUTOR_IMAGE RUNNER_IMAGE \
	CONTROLLER_IMAGE CONTROLLER_REVISION CONTROLLER_STATE_VERSION; do
	eval "value=\${$value_name}"
	[ -n "$value" ] || fail "$value_name is required"
done
[ -f "$KUBECONFIG_FILE" ] || fail "E2E_KUBECONFIG does not name a file"
for image in "$EXECUTOR_IMAGE" "$RUNNER_IMAGE" "$CONTROLLER_IMAGE"; do
	is_pinned_image "$image" ||
		fail "migration phase images must be pinned by a lowercase SHA-256 digest: $image"
done
printf '%s\n' "$CONTROLLER_STATE_VERSION" | grep -Eq '^[1-9][0-9]*$' ||
	fail "E2E_CONTROLLER_STATE_VERSION must be a positive integer"
printf '%s\n' "$TIMEOUT_SECONDS" | grep -Eq '^[1-9][0-9]*$' ||
	fail "E2E_TIMEOUT_SECONDS must be a positive integer"

k() {
	kubectl --kubeconfig "$KUBECONFIG_FILE" "$@"
}

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-migrations-e2e.XXXXXX")
chmod 700 "$WORK_DIR"
umask 077
RESOURCE_FILE=$WORK_DIR/resource.json
SECRET_FILE=$WORK_DIR/migration-secret.json
LOG_FILE=$WORK_DIR/logs.txt
JOB_RECORDS_FILE=$WORK_DIR/migration-jobs.jsonl
JOB_INVENTORY_FILE=$WORK_DIR/migration-job-inventory.json
CREDENTIAL_PATTERNS_FILE=$WORK_DIR/credential-patterns.txt
MIGRATION_DB_PASSWORD_FILE=$WORK_DIR/migration-database.password
MIGRATION_DB_URL_FILE=$WORK_DIR/migration-database.url
ADMISSION_ERROR_FILE=$WORK_DIR/admission-error.txt
STATUS_FILE=$WORK_DIR/migration-status.json
: >"$JOB_RECORDS_FILE"

PHASE_COMPLETED=0
cleanup() {
	status=$?
	[ "$status" -ne 0 ] || [ "$PHASE_COMPLETED" -eq 1 ] || status=1
	if [ -n "${PHASE_REASON_MARKER:-}" ]; then
		if [ "$status" -ne 0 ] && [ ! -f "$PHASE_REASON_MARKER" ]; then
			printf 'e2e migrations: exited with status %s at a command that failed under set -e; no proof reported a reason\n' \
				"$status" >&2
		fi
		rm -f -- "$PHASE_REASON_MARKER"
	fi
	if [ "$status" -ne 0 ]; then
		printf 'e2e migrations: collecting failure diagnostics\n' >&2
		# The status of a migration carries no rows and no SQL by contract, so
		# it is safe to print, and it is the one artifact that explains what the
		# controller decided. Job logs are not printed: a credential-isolation
		# failure would put a database URL in them.
		k -n "$TEST_NAMESPACE" get ptahmigrations -o json 2>/dev/null |
			jq '.items[] | {name: .metadata.name, status: .status}' >&2 || true
		k -n "$TEST_NAMESPACE" get jobs \
			-l app.kubernetes.io/component=migration-operation \
			-o json 2>/dev/null |
			jq '[.items[] | {name: .metadata.name, labels: .metadata.labels, status: .status}]' >&2 || true
		printf 'e2e migrations: raw Job logs are suppressed to protect credential-isolation failures\n' >&2
	fi
	rm -rf -- "$WORK_DIR"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

deadline_from_now() {
	printf '%s\n' "$(($(date +%s) + TIMEOUT_SECONDS))"
}

# The fixtures the data plane already stood up in this namespace are reused
# rather than rebuilt: a second PostgreSQL, a second MySQL and a second registry
# credential would be a second answer to questions the earlier phase already
# answered, and the first one to drift would do so silently. What this phase
# adds per engine is a database of its own, so a migration history starts from
# nothing.
REGISTRY_AUTH_SECRET=e2e-registry-auth
REGISTRY_PULL_SECRET=e2e-registry-pull
REGISTRY_HOST="${REGISTRY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5000"
MIGRATION_DATABASE=ptah_e2e_migrations
MIGRATION_POLICY=e2e-migrations-verification-policy
MIGRATION_POLICY_KEY=policy.yaml
MIGRATION_POLICY_FILE="$ROOT_DIR/testdata/e2e/verification-policy-migrations.yaml"
DATABASE_USER=ptah_e2e
[ -f "$MIGRATION_POLICY_FILE" ] || fail "migration verification policy fixture is missing"
[ "$MIGRATION_DATABASE" != ptah_e2e ] ||
	fail "the migration proof must own a database the schema path never touched"
: >"$CREDENTIAL_PATTERNS_FILE"
chmod 600 "$CREDENTIAL_PATTERNS_FILE"

coordination_digest() {
	coordination_canonical=$(jq -cn \
		--arg engine "$1" \
		--arg key "$2" '
      {contract_version: 1, engine: $engine, coordination_key: $key}
    ')
	printf 'sha256:%s\n' "$(printf '%s' "$coordination_canonical" | sha256)"
}

# select_engine names everything one engine's lifecycle needs. The two run the
# same proof against different servers, and naming the differences in one place
# is what keeps the second engine from becoming a second proof.
select_engine() {
	ENGINE=$1
	case "$ENGINE" in
	postgresql)
		ENGINE_KIND=PostgreSQL
		DATABASE_SERVICE=e2e-postgresql
		DATABASE_SOURCE_SECRET=e2e-postgresql-db
		;;
	mysql)
		ENGINE_KIND=MySQL
		DATABASE_SERVICE=e2e-mysql
		DATABASE_SOURCE_SECRET=e2e-mysql-db
		;;
	*) fail "unsupported migration engine $ENGINE" ;;
	esac
	MIGRATION_DB_SECRET="e2e-${ENGINE}-migrations-db"
	MIGRATION_NAME="e2e-migrations-${ENGINE}"
	MIGRATION_APPROVAL="e2e-migrations-${ENGINE}-approval"
	MIGRATION_STALE_APPROVAL="e2e-migrations-${ENGINE}-stale-approval"
	MIGRATION_RIVAL_SCHEMA="e2e-migrations-${ENGINE}-rival"
	MIGRATION_PARTIAL_APPROVAL="e2e-migrations-${ENGINE}-partial-approval"
	MIGRATION_COORDINATION_KEY="e2e/migrations/${ENGINE}"
	MIGRATION_REFERENCE="oci://${REGISTRY_HOST}/migrations/${ENGINE}:stable"
	MIGRATION_FIXTURE_DIR="$ROOT_DIR/testdata/e2e/migrations/${ENGINE}"
	MIGRATION_EDITED_FIXTURE_DIR="$ROOT_DIR/testdata/e2e/migrations/${ENGINE}-modified"
	MIGRATION_PARTIAL_FIXTURE_DIR="$ROOT_DIR/testdata/e2e/migrations/${ENGINE}-partial"
	MIGRATION_COORDINATION_DIGEST=$(coordination_digest "$ENGINE" "$MIGRATION_COORDINATION_KEY")
	[ -d "$MIGRATION_FIXTURE_DIR" ] || fail "migration fixtures are missing: $MIGRATION_FIXTURE_DIR"
	[ -d "$MIGRATION_PARTIAL_FIXTURE_DIR" ] ||
		fail "migration fixtures are missing: $MIGRATION_PARTIAL_FIXTURE_DIR"

	# The password is read back from the Secret the data plane created rather
	# than derived a second time here. A second derivation is a second
	# definition, and the two would part company the moment either moved.
	k -n "$TEST_NAMESPACE" get secret "$DATABASE_SOURCE_SECRET" \
		-o jsonpath='{.data.password}' |
		tr -d '\n' | base64 -d >"$MIGRATION_DB_PASSWORD_FILE" ||
		fail "the data plane $ENGINE Secret $DATABASE_SOURCE_SECRET could not be read"
	chmod 600 "$MIGRATION_DB_PASSWORD_FILE"
	[ -s "$MIGRATION_DB_PASSWORD_FILE" ] ||
		fail "the data plane $ENGINE Secret carries no password"
	engine_password=$(cat "$MIGRATION_DB_PASSWORD_FILE")
	engine_authority="${DATABASE_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local"
	case "$ENGINE" in
	postgresql)
		engine_url="postgres://${DATABASE_USER}:${engine_password}@${engine_authority}:5432/${MIGRATION_DATABASE}?sslmode=disable"
		;;
	mysql)
		engine_url="mysql://${DATABASE_USER}:${engine_password}@tcp(${engine_authority}:3306)/${MIGRATION_DATABASE}"
		;;
	esac
	printf '%s' "$engine_url" >"$MIGRATION_DB_URL_FILE"
	chmod 600 "$MIGRATION_DB_URL_FILE"
	printf '%s\n' "$engine_password" "$engine_url" >>"$CREDENTIAL_PATTERNS_FILE"
	engine_password=
	engine_url=
}

# The plan-inspection surface is proved from the same build a user installs,
# against the live resource, rather than from a golden file.
mkdir -p "$WORK_DIR/go-cache"
KUBECTL_PTAH_BINARY=$WORK_DIR/kubectl-ptah
env GOCACHE="$WORK_DIR/go-cache" go build -trimpath \
	-o "$KUBECTL_PTAH_BINARY" ./cmd/kubectl-ptah

# scan_for_credentials refuses any evidence file that carries a value only the
# Pod should ever hold. Every assertion below reads through it.
scan_for_credentials() {
	scan_file=$1
	scan_description=$2
	[ -s "$CREDENTIAL_PATTERNS_FILE" ] ||
		fail "credential scanner has no non-empty protected patterns"
	# An empty pattern line matches every line, which would turn this scanner
	# into one that always fires, and a scanner that always fires is one
	# somebody removes.
	grep -c '^$' "$CREDENTIAL_PATTERNS_FILE" | grep -qx 0 ||
		fail "credential scanner has an empty protected pattern"
	[ -s "$scan_file" ] || return 0
	if grep -F -f "$CREDENTIAL_PATTERNS_FILE" "$scan_file" >/dev/null; then
		fail "$scan_description carries a database credential"
	else
		scan_status=$?
		[ "$scan_status" -eq 1 ] ||
			fail "credential scanner failed closed while checking $scan_description"
	fi
}

# record_migration_jobs archives every Job the controller has created for this
# migration so far.
#
# The controller stamps a five-minute TTL on a Job it finished reading, and a
# whole lifecycle outlasts that, so the Jobs a final assertion would find are
# only the last ones. The record is taken while each Job still exists, keyed by
# UID so repeated calls add rather than duplicate.
record_migration_jobs() {
	k -n "$TEST_NAMESPACE" get jobs \
		-l "operator.ptah.run/migration=${MIGRATION_NAME}" -o json 2>/dev/null |
		jq -c '.items[]?' >>"$JOB_RECORDS_FILE" || true
}

migration_job_inventory() {
	jq -s '{items: (. | unique_by(.metadata.uid))}' "$JOB_RECORDS_FILE" \
		>"$JOB_INVENTORY_FILE" ||
		fail "the archived migration Job records could not be read"
}

migration_status() {
	k -n "$TEST_NAMESPACE" get ptahmigration "$MIGRATION_NAME" -o json >"$STATUS_FILE" ||
		fail "$MIGRATION_NAME could not be read"
	scan_for_credentials "$STATUS_FILE" "$MIGRATION_NAME status"
}

migration_phase() {
	k -n "$TEST_NAMESPACE" get ptahmigration "$MIGRATION_NAME" \
		-o jsonpath='{.status.phase}' 2>/dev/null || true
}

wait_for_migration_phase() {
	wait_phase=$1
	wait_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$wait_deadline" ]; do
		record_migration_jobs
		observed_phase=$(migration_phase)
		case "$observed_phase" in
		"$wait_phase")
			record_migration_jobs
			return 0
			;;
		Failed)
			[ "$wait_phase" = Failed ] || fail "$MIGRATION_NAME failed while waiting for $wait_phase"
			;;
		esac
		sleep 5
	done
	fail "$MIGRATION_NAME did not reach $wait_phase within ${TIMEOUT_SECONDS}s; it is in ${observed_phase:-<none>}"
}

# The database this proof migrates is created on the server the data plane
# already runs, and it is one this suite has not touched before: a migration
# history has to start from nothing for "current version 0, two pending" to
# mean anything.
create_migration_database() {
	case "$ENGINE" in
	postgresql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		existing=$(k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atqc "$1"' \
			sh "SELECT count(*) FROM pg_database WHERE datname='${MIGRATION_DATABASE}'" |
			tr -d '[:space:]')
		;;
	mysql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		existing=$(k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -Nse "$1"' \
			sh "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='${MIGRATION_DATABASE}'" |
			tr -d '[:space:]')
		;;
	esac
	[ "$existing" = 0 ] ||
		fail "database $MIGRATION_DATABASE already exists on $ENGINE; the migration proof needs a database nothing has migrated, so drop it before running this phase again"
	case "$ENGINE" in
	postgresql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -qc "$1"' \
			sh "CREATE DATABASE ${MIGRATION_DATABASE}" >/dev/null ||
			fail "database $MIGRATION_DATABASE could not be created"
		;;
	mysql)
		# The unprivileged user the operation Pod connects as owns nothing by
		# default, and MySQL grants are per schema: the grant is part of
		# creating the database rather than a separate setup step.
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -e "$1"' \
			sh "CREATE DATABASE ${MIGRATION_DATABASE}; GRANT ALL PRIVILEGES ON ${MIGRATION_DATABASE}.* TO '${DATABASE_USER}'@'%'; FLUSH PRIVILEGES" >/dev/null ||
			fail "database $MIGRATION_DATABASE could not be created"
		;;
	esac

	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$MIGRATION_DB_SECRET" \
		--arg username "$DATABASE_USER" \
		--rawfile password "$MIGRATION_DB_PASSWORD_FILE" \
		--arg database "$MIGRATION_DATABASE" \
		--rawfile url "$MIGRATION_DB_URL_FILE" '
    {
      apiVersion: "v1", kind: "Secret",
      metadata: {namespace: $namespace, name: $name},
      immutable: true,
      type: "Opaque",
      stringData: {
        username: $username, password: $password,
        database: $database, url: $url
      }
    }' >"$SECRET_FILE"
	chmod 600 "$SECRET_FILE"
	k apply -f "$SECRET_FILE" >/dev/null
	rm -f "$SECRET_FILE"
}

migration_query() {
	case "$ENGINE" in
	postgresql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -Atqc "$2"' \
			sh "$MIGRATION_DATABASE" "$1" | tr -d '[:space:]'
		;;
	mysql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1" -Nse "$2"' \
			sh "$MIGRATION_DATABASE" "$1" | tr -d '[:space:]'
		;;
	esac
}

# Whether the table a migration would change carries a column, asked of the
# catalog rather than of the migration that was supposed to add it. The two
# engines spell the current database differently in information_schema, and
# MySQL's spans the whole server, so an unfiltered count there would answer for
# another phase's database.
migration_widget_column_count() {
	case "$ENGINE" in
	postgresql)
		migration_query "SELECT count(*) FROM information_schema.columns
                     WHERE table_schema = current_schema()
                       AND table_name = 'e2e_migration_widgets'
                       AND column_name = '$1'"
		;;
	mysql)
		migration_query "SELECT count(*) FROM information_schema.columns
                     WHERE table_schema = database()
                       AND table_name = 'e2e_migration_widgets'
                       AND column_name = '$1'"
		;;
	esac
}

# The Apply Jobs this migration has dispatched. A Blocked resource keeps
# reading, so its read-only Jobs go on appearing; what must not appear is
# another run.
migration_apply_job_count() {
	k -n "$TEST_NAMESPACE" get jobs \
		-l "operator.ptah.run/migration=${MIGRATION_NAME},operator.ptah.run/operation=apply" \
		-o json | jq '.items | length'
}

# The verification policy this phase applies names the migration artifact type.
# The schema path's policy names the schema type, and a policy that accepted
# both would let a schema artifact stand in for a migration directory.
create_migration_policy() {
	# Applied rather than created so a rerun of this phase against a retained
	# cluster reaches its own proof instead of failing on the fixture. The
	# object is immutable, so an apply either writes it once or changes nothing.
	k -n "$TEST_NAMESPACE" create configmap "$MIGRATION_POLICY" \
		--from-file="${MIGRATION_POLICY_KEY}=${MIGRATION_POLICY_FILE}" \
		--dry-run=client -o json | jq '.immutable = true' | k apply -f - >/dev/null
	grep -F 'application/vnd.stokaro.ptah.migrations.v1' "$MIGRATION_POLICY_FILE" >/dev/null ||
		fail "the migration verification policy does not pin the migration artifact type"
}

# The artifact is published by the same Ptah the operator runs, with the command
# a person would use. The harness owns no migration-directory format of its own:
# a second implementation of the layout is a second thing to keep in step.
publish_migrations() {
	publish_version=${1:-v1}
	publish_directory=${2:-$MIGRATION_FIXTURE_DIR}
	publish_configmap="e2e-migrations-${ENGINE}-${publish_version}"
	publish_job="e2e-push-migrations-${ENGINE}-${publish_version}"
	[ -d "$publish_directory" ] || fail "migration fixtures are missing: $publish_directory"
	printf 'e2e migrations: publishing the %s migration directory as %s\n' \
		"$ENGINE_KIND" "$publish_version" >&2
	k -n "$TEST_NAMESPACE" create configmap "$publish_configmap" \
		--from-file="$publish_directory" >/dev/null
	# A ConfigMap volume is not a directory of files. The kubelet writes the
	# keys into a timestamped directory, points `..data` at it, and leaves one
	# symlink per key beside it, so a walker that descends into directories
	# finds every migration twice -- once as the top-level symlink and once
	# inside the timestamped directory. Ptah's Discover does exactly that.
	#
	# A subPath mount per file is the documented way to get a plain directory:
	# the kubelet bind-mounts each file at its own path, and `/migrations` then
	# holds the six files and nothing else. The list comes from the fixtures so
	# it cannot fall behind them.
	publish_mounts=$(find "$publish_directory" -maxdepth 1 -type f -name '*.sql' \
		-exec basename {} \; | LC_ALL=C sort | jq -R . | jq -s .)
	[ "$(printf '%s' "$publish_mounts" | jq 'length')" -gt 0 ] ||
		fail "no migration files to publish from $publish_directory"
	jq -n \
		--argjson mounts "$publish_mounts" \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$publish_job" \
		--arg image "$EXECUTOR_IMAGE" \
		--arg configMap "$publish_configmap" \
		--arg reference "$MIGRATION_REFERENCE" \
		--arg version "$publish_version" \
		--arg registryAuthSecret "$REGISTRY_AUTH_SECRET" \
		--arg registryPullSecret "$REGISTRY_PULL_SECRET" '
    def registrySecretEnv($name; $key):
      {name: $name, valueFrom: {secretKeyRef: {name: $registryAuthSecret, key: $key}}};
    {
      apiVersion: "batch/v1", kind: "Job",
      metadata: {
        namespace: $namespace, name: $name,
        labels: {"app.kubernetes.io/component": "e2e-migration-publisher"}
      },
      spec: {
        backoffLimit: 0, activeDeadlineSeconds: 300,
        template: {
          metadata: {labels: {"app.kubernetes.io/component": "e2e-migration-publisher"}},
          spec: {
            restartPolicy: "Never", automountServiceAccountToken: false,
            imagePullSecrets: [{name: $registryPullSecret}],
            securityContext: {
              runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532,
              seccompProfile: {type: "RuntimeDefault"}
            },
            containers: [{
              name: "publisher", image: $image, imagePullPolicy: "IfNotPresent",
              command: ["/usr/local/bin/ptah"],
              args: [
                "migrations", "push", $reference, "--migrations-dir", "/migrations",
                "--dir-format", "ptah", "--version", $version, "--plain-http"
              ],
              env: [
                {name: "HOME", value: "/work"},
                {name: "TMPDIR", value: "/work"},
                registrySecretEnv("PTAH_OCI_USERNAME"; "username"),
                registrySecretEnv("PTAH_OCI_PASSWORD"; "password"),
                registrySecretEnv("PTAH_OCI_REGISTRY"; "registry")
              ],
              securityContext: {
                allowPrivilegeEscalation: false, readOnlyRootFilesystem: true,
                capabilities: {drop: ["ALL"]}
              },
              volumeMounts: ([$mounts[] | {
                name: "migrations", mountPath: ("/migrations/" + .),
                subPath: ., readOnly: true
              }] + [{name: "work", mountPath: "/work"}])
            }],
            volumes: [
              {name: "migrations", configMap: {name: $configMap}},
              {name: "work", emptyDir: {sizeLimit: "64Mi"}}
            ]
          }
        }
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
	publish_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$publish_deadline" ]; do
		publish_state=$(k -n "$TEST_NAMESPACE" get job "$publish_job" -o json |
			jq -r 'if (.status.succeeded // 0) > 0 then "succeeded"
                   elif (.status.failed // 0) > 0 then "failed" else "running" end')
		case "$publish_state" in
		succeeded) break ;;
		failed)
			# The publisher reaches the registry and never the database, so its
			# own words are safe to print once scanned. A publisher that fails
			# silently costs a whole lifecycle to ask again.
			k -n "$TEST_NAMESPACE" logs job/"$publish_job" >"$LOG_FILE" 2>&1 || true
			scan_for_credentials "$LOG_FILE" "the migration publisher log"
			printf 'e2e migrations: the publisher said:\n' >&2
			sed 's/^/e2e migrations:   /' "$LOG_FILE" >&2
			fail "the migration publisher Job failed"
			;;
		esac
		sleep 3
	done
	[ "${publish_state:-}" = succeeded ] ||
		fail "the migration publisher Job did not finish within ${TIMEOUT_SECONDS}s"
	# The publisher holds registry credentials and must hold no database
	# credential: it is the same boundary the schema publisher keeps.
	k -n "$TEST_NAMESPACE" get job "$publish_job" -o json |
		jq -e \
			--arg image "$EXECUTOR_IMAGE" \
			--arg registrySecret "$REGISTRY_AUTH_SECRET" \
			-f "$ROOT_DIR/testdata/e2e/publisher-job-isolation.jq" >/dev/null ||
		fail "the migration publisher Job did not preserve the no-database-credential boundary"
	k -n "$TEST_NAMESPACE" logs job/"$publish_job" >"$LOG_FILE"
	PUBLISHED_DIGEST=$(sed -n 's/^Digest: \(sha256:[0-9a-f]\{64\}\)$/\1/p' "$LOG_FILE" | tail -n 1)
	printf '%s\n' "$PUBLISHED_DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
		fail "could not read the published migration digest from Job $publish_job"
}

create_migration_resource() {
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$MIGRATION_NAME" \
		--arg secret "$MIGRATION_DB_SECRET" \
		--arg reference "$MIGRATION_REFERENCE" \
		--arg coordinationKey "$MIGRATION_COORDINATION_KEY" \
		--arg policy "$MIGRATION_POLICY" \
		--arg registryAuthSecret "$REGISTRY_AUTH_SECRET" \
		--arg interval "$INTERVAL" \
		--arg engine "$ENGINE_KIND" '
    {
      apiVersion: "operator.ptah.run/v1alpha1", kind: "PtahMigration",
      metadata: {namespace: $namespace, name: $name},
      spec: {
        target: {
          engine: $engine,
          coordinationKey: $coordinationKey,
          urlFrom: {name: $secret, key: "url"}
        },
        artifact: {
          ociRef: $reference,
          registryAuthFrom: {
            name: $registryAuthSecret,
            mode: "Environment",
            usernameKey: "username",
            passwordKey: "password",
            registryKey: "registry"
          },
          verificationPolicyFrom: {name: $policy, key: "policy.yaml"},
          transport: {plainHTTP: true}
        },
        policy: {lockTimeout: "30s"},
        interval: $interval,
        execution: {
          activeDeadlineSeconds: 300, failureRetryInterval: "10s", connectTimeout: "30s"
        }
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
	# An artifact of arbitrary SQL gets the conservative default whether or not
	# the author wrote one down.
	k -n "$TEST_NAMESPACE" get ptahmigration "$MIGRATION_NAME" -o json |
		jq -e '.spec.policy.apply == "OnApproval" and .spec.suspend == false' >/dev/null ||
		fail "$MIGRATION_NAME did not persist the safe apply-policy default"
}

assert_awaiting_approval() {
	migration_status
	jq -e \
		--arg digest "$PUBLISHED_DIGEST" \
		--arg coordinationKey "$MIGRATION_COORDINATION_KEY" \
		--arg controllerImage "$CONTROLLER_IMAGE" \
		--arg controllerRevision "$CONTROLLER_REVISION" \
		--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" '
      .status as $status |
      $status.phase == "AwaitingApproval" and
      $status.artifact.digest == $digest and
      $status.history.contractVersion == 1 and
      $status.history.currentVersion == 0 and
      $status.history.appliedCount == 0 and
      $status.history.pendingCount == 3 and
      ($status.history.dirty // false) == false and
      ($status.history.modifiedVersions // []) == [] and
      ($status.history.fingerprint | test("^sha256:[0-9a-f]{64}$")) and
      ($status.history.targetIdentityDigest | test("^sha256:[0-9a-f]{64}$")) and
      $status.executionBinding.controllerImage == $controllerImage and
      $status.executionBinding.controllerRevision == $controllerRevision and
      $status.executionBinding.controllerStateVersion == $controllerStateVersion and
      ($status.plan.name | startswith("ptah-mplan-")) and
      ($status.lastRun // null) == null and
      (any($status.conditions[];
        .type == "ApprovalRequired" and .status == "True" and
        .reason == "AwaitingApproval")) and
      (any($status.conditions[];
        .type == "ArtifactVerified" and .status == "True")) and
      (any($status.conditions[]; .type == "Ready" and .status == "True") | not) and
      ([$status | .. | scalars | select(. == $coordinationKey)] | length) == 0
    ' "$STATUS_FILE" >/dev/null ||
		fail "$MIGRATION_NAME did not reach an exact three-migration approval gate"
	MIGRATION_PLAN=$(jq -er '.status.plan.name' "$STATUS_FILE")
	printf 'e2e migrations: plan %s awaits approval\n' "$MIGRATION_PLAN" >&2
}

assert_plan_sequence() {
	plan_file=$WORK_DIR/plan.json
	k -n "$TEST_NAMESPACE" get ptahmigrationplan "$MIGRATION_PLAN" -o json >"$plan_file" ||
		fail "migration plan $MIGRATION_PLAN could not be read"
	scan_for_credentials "$plan_file" "migration plan $MIGRATION_PLAN"
	jq -e \
		--arg migration "$MIGRATION_NAME" \
		--arg digest "$PUBLISHED_DIGEST" \
		--arg coordinationDigest "$MIGRATION_COORDINATION_DIGEST" \
		--arg controllerImage "$CONTROLLER_IMAGE" \
		--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" '
      .spec as $spec |
      $spec.contractVersion == 1 and
      $spec.migrationRef.name == $migration and
      $spec.artifactDigest == $digest and
      $spec.coordinationDigest == $coordinationDigest and
      $spec.currentVersion == 0 and
      ($spec.fingerprint | test("^sha256:[0-9a-f]{64}$")) and
      ($spec.historyFingerprint | test("^sha256:[0-9a-f]{64}$")) and
      [$spec.migrations[].version] == [1, 2, 3] and
      all($spec.migrations[]; .checksum != "" and (.checkpoint // false) == false) and
      $spec.controllerImage == $controllerImage and
      $spec.controllerStateVersion == $controllerStateVersion
    ' "$plan_file" >/dev/null ||
		fail "migration plan $MIGRATION_PLAN is not the exact pending sequence"
	# A plan says which migrations run and in what order. It never carries the
	# SQL, so a reader who may see the order still may not read the statements.
	jq -e '
      [.. | strings | select(test("(?i)(create[[:space:]]+table|alter[[:space:]]+table|insert[[:space:]]+into)"))]
        | length == 0
    ' "$plan_file" >/dev/null ||
		fail "migration plan $MIGRATION_PLAN carries SQL text"
}

approve_migration() {
	approve_name=$1
	approve_plan=$2
	approve_plan_uid=$3
	approve_fingerprint=$4
	migration_uid=$(k -n "$TEST_NAMESPACE" get ptahmigration "$MIGRATION_NAME" \
		-o jsonpath='{.metadata.uid}')
	[ -n "$migration_uid" ] || fail "$MIGRATION_NAME has no UID"
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$approve_name" \
		--arg migration "$MIGRATION_NAME" \
		--arg migrationUID "$migration_uid" \
		--arg plan "$approve_plan" \
		--arg planUID "$approve_plan_uid" \
		--arg fingerprint "$approve_fingerprint" '
    {
      apiVersion: "operator.ptah.run/v1alpha1", kind: "PtahMigrationApproval",
      metadata: {namespace: $namespace, name: $name},
      spec: {
        migrationRef: {name: $migration, uid: $migrationUID},
        planRef: {name: $plan, uid: $planUID},
        planFingerprint: $fingerprint
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE"
}

assert_approval_hydrated() {
	k -n "$TEST_NAMESPACE" get ptahmigrationapproval "$MIGRATION_APPROVAL" -o json \
		>"$WORK_DIR/approval.json"
	scan_for_credentials "$WORK_DIR/approval.json" "approval $MIGRATION_APPROVAL"
	jq -e \
		--arg migration "$MIGRATION_NAME" \
		--arg plan "$MIGRATION_PLAN" \
		--arg digest "$PUBLISHED_DIGEST" \
		--arg coordinationDigest "$MIGRATION_COORDINATION_DIGEST" \
		--arg controllerImage "$CONTROLLER_IMAGE" \
		--arg controllerRevision "$CONTROLLER_REVISION" \
		--argjson controllerStateVersion "$CONTROLLER_STATE_VERSION" '
      .spec as $spec |
      $spec.migrationRef.name == $migration and $spec.planRef.name == $plan and
      $spec.artifactDigest == $digest and
      $spec.coordinationDigest == $coordinationDigest and
      ($spec.historyFingerprint | test("^sha256:[0-9a-f]{64}$")) and
      ($spec.targetIdentityDigest | test("^sha256:[0-9a-f]{64}$")) and
      $spec.policyFingerprint != "" and $spec.verificationPolicyDigest != "" and
      $spec.ptahVersion != "" and
      ($spec.executionBindingID | test("^v1-[0-9a-f]{32}$")) and
      $spec.controllerImage == $controllerImage and
      $spec.controllerRevision == $controllerRevision and
      $spec.controllerStateVersion == $controllerStateVersion and
      ($spec.executorImage | test("@sha256:[0-9a-f]{64}$")) and
      ($spec.runnerImage | test("@sha256:[0-9a-f]{64}$")) and
      $spec.approver.username != "" and $spec.approvedAt != null and
      $spec.mutationRequestUID != ""
    ' "$WORK_DIR/approval.json" >/dev/null ||
		fail "approval $MIGRATION_APPROVAL was not hydrated and bound to the exact plan"
}

assert_in_sync() {
	migration_status
	jq -e \
		--arg digest "$PUBLISHED_DIGEST" \
		--arg coordinationKey "$MIGRATION_COORDINATION_KEY" '
      .status as $status |
      $status.phase == "InSync" and
      $status.artifact.digest == $digest and
      $status.history.currentVersion == 3 and
      $status.history.appliedCount == 3 and
      $status.history.pendingCount == 0 and
      ($status.history.dirty // false) == false and
      ($status.history.modifiedVersions // []) == [] and
      $status.lastRun.outcome == "Applied" and
      ($status.lastRun.appliedVersions | sort) == [1, 2, 3] and
      $status.lastRun.jobName != "" and $status.lastRun.jobUID != "" and
      $status.lastRun.finishedAt != null and
      ($status.plan // null) == null and
      ($status.activeOperation // null) == null and
      (any($status.conditions[];
        .type == "Ready" and .status == "True" and .reason == "HistoryMatched")) and
      (any($status.conditions[];
        .type == "ApprovalRequired" and .status == "True") | not) and
      (any($status.conditions[]; .type == "Blocked" and .status == "True") | not) and
      ([$status | .. | scalars | select(. == $coordinationKey)] | length) == 0
    ' "$STATUS_FILE" >/dev/null ||
		fail "$MIGRATION_NAME did not settle on a history that matches the artifact"
	# A run's evidence explains what happened without reproducing what ran.
	jq -e '
      [.status | .. | strings |
        select(test("(?i)(create[[:space:]]+table|alter[[:space:]]+table|insert[[:space:]]+into)"))]
        | length == 0
    ' "$STATUS_FILE" >/dev/null ||
		fail "$MIGRATION_NAME status carries SQL text"
}

# What the migrations claim to have done, checked in the database rather than in
# the status that reports it.
assert_database_migrated() {
	case "$ENGINE" in
	postgresql) migrated_schema="table_schema='public'" ;;
	mysql) migrated_schema="table_schema=DATABASE()" ;;
	esac
	table_count=$(migration_query "SELECT count(*) FROM information_schema.tables WHERE ${migrated_schema} AND table_name='e2e_migration_widgets'")
	[ "$table_count" = 1 ] ||
		fail "$ENGINE migration 1 did not create its table; information_schema reports $table_count"
	# Migration 1 seeds three rows. A run the history already recorded must not
	# execute again, and a repeated INSERT is the one kind of DML that says so
	# without an observer: the count is what proves it did not.
	row_count=$(migration_query "SELECT count(*) FROM e2e_migration_widgets")
	[ "$row_count" = 3 ] ||
		fail "$ENGINE has $row_count seeded rows, not the three migration 1 inserted once"
	column_count=$(migration_query "SELECT count(*) FROM information_schema.columns WHERE ${migrated_schema} AND table_name='e2e_migration_widgets' AND column_name='color'")
	[ "$column_count" = 1 ] ||
		fail "$ENGINE migration 2 did not add its column; information_schema reports $column_count"
	# Migration 2 adds the column, fills it, and only then constrains it. A
	# reordered run leaves the constraint refused or the column nullable.
	nullable=$(migration_query "SELECT is_nullable FROM information_schema.columns WHERE ${migrated_schema} AND table_name='e2e_migration_widgets' AND column_name='color'")
	[ "$nullable" = NO ] ||
		fail "$ENGINE migration 2 left its column nullable: is_nullable=$nullable"
	# Migration 3 is DML with an empty schema diff. It has to run and be
	# recorded like any other version.
	recolored=$(migration_query "SELECT color FROM e2e_migration_widgets WHERE id = 1")
	[ "$recolored" = blue ] ||
		fail "$ENGINE migration 3 did not apply its data-only change: color=$recolored"
	untouched=$(migration_query "SELECT count(*) FROM e2e_migration_widgets WHERE color = 'unset'")
	[ "$untouched" = 2 ] ||
		fail "$ENGINE migration 3 changed $((3 - untouched)) rows instead of the one it names"
}

# The isolation row of the matrix, read from the Jobs the controller created
# rather than from the builder that wrote them.
assert_migration_job_isolation() {
	migration_job_inventory
	jq -e \
		--arg databaseSecret "$MIGRATION_DB_SECRET" \
		--arg registrySecret "$REGISTRY_AUTH_SECRET" \
		--arg executorImage "$EXECUTOR_IMAGE" \
		--arg runnerImage "$RUNNER_IMAGE" \
		--arg serviceAccountName "default" \
		-f "$ROOT_DIR/testdata/e2e/migration-job-isolation.jq" \
		"$JOB_INVENTORY_FILE" >/dev/null ||
		fail "migration Jobs did not keep registry access out of the process that runs SQL"
	observed_operations=$(jq -r \
		'[.items[].metadata.labels["operator.ptah.run/operation"]] | unique | sort | join(",")' \
		"$JOB_INVENTORY_FILE")
	[ "$observed_operations" = "apply,history,resolve,verify" ] ||
		fail "the archived migration Jobs cover $observed_operations, not the whole lifecycle"
}

# The repeated-reconciliation row of the matrix: a history the database already
# holds produces no second run, and the DML a migration carried is not executed
# again.
#
# The interval is shortened rather than waited out, which also makes this a new
# generation: the whole read-only chain runs again from resolution, which is the
# strongest form of "reconciled again" the resource has.
assert_repeated_reconciliation_runs_nothing() {
	settled_run_uid=$(jq -er '.status.lastRun.jobUID' "$STATUS_FILE")
	settled_observed=$(jq -er '.status.history.observedAt' "$STATUS_FILE")
	[ -n "$settled_run_uid" ] && [ -n "$settled_observed" ] ||
		fail "$MIGRATION_NAME settled without run and history evidence to compare against"
	k -n "$TEST_NAMESPACE" patch ptahmigration "$MIGRATION_NAME" --type=merge \
		--patch '{"spec":{"interval":"30s"}}' >/dev/null

	repeat_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$repeat_deadline" ]; do
		record_migration_jobs
		migration_status
		repeat_observed=$(jq -er '.status.history.observedAt' "$STATUS_FILE")
		repeat_phase=$(jq -er '.status.phase' "$STATUS_FILE")
		if [ "$repeat_phase" = InSync ] && [ "$repeat_observed" != "$settled_observed" ]; then
			break
		fi
		[ "$repeat_phase" != Failed ] && [ "$repeat_phase" != Blocked ] ||
			fail "$MIGRATION_NAME left InSync on a repeated reconciliation: $repeat_phase"
		sleep 5
	done
	[ "${repeat_observed:-}" != "$settled_observed" ] ||
		fail "$MIGRATION_NAME did not read its history again within ${TIMEOUT_SECONDS}s"

	jq -e \
		--arg runUID "$settled_run_uid" '
      .status as $status |
      $status.phase == "InSync" and
      $status.history.currentVersion == 3 and
      $status.history.pendingCount == 0 and
      $status.lastRun.jobUID == $runUID and
      ($status.plan // null) == null
    ' "$STATUS_FILE" >/dev/null ||
		fail "$MIGRATION_NAME started new work for a history it already matched"
	assert_database_migrated
}

# A decision authorizes one run. The plan it named is gone once that run
# finished, and admission refuses a second approval that still names it.
assert_replaced_plan_approval_refused() {
	if approve_migration "$MIGRATION_STALE_APPROVAL" "$MIGRATION_PLAN" \
		"$MIGRATION_PLAN_UID" "$MIGRATION_PLAN_FINGERPRINT" \
		>"$ADMISSION_ERROR_FILE" 2>&1; then
		fail "an approval naming the consumed plan was accepted"
	fi
	# Which refusal fires depends on where the resource is in its cycle: the
	# plan is no longer current, the migration is not awaiting approval, or it
	# has an operation in flight. Every one of them names what it read.
	grep -Ei 'plan|approval|migration' "$ADMISSION_ERROR_FILE" >/dev/null ||
		fail "the refusal of a consumed-plan approval did not say what it refused"
	scan_for_credentials "$ADMISSION_ERROR_FILE" "the consumed-plan approval refusal"
}

# The plan-inspection row of the matrix: a reader reviews the migration order
# through the plugin rather than by extracting a ConfigMap by hand, and never
# sees a statement while doing it.
# The ownership row of the matrix, and the combination #45 names outright: a
# PtahSchema and a PtahMigration claiming one database.
#
# Serialization is not ownership. These two would take turns through the Lease
# and the database's own lock, and undo each other while doing it, so the
# operator refuses both until each declares the realm shared. Here neither
# does, and what the proof wants is the refusal and the recovery: removing the
# second claimant ends it without anybody editing the first.
assert_second_claimant_blocks_the_realm() {
	printf 'e2e migrations: claiming the %s migration database with a PtahSchema as well\n' \
		"$ENGINE_KIND" >&2
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$MIGRATION_RIVAL_SCHEMA" \
		--arg engine "$ENGINE_KIND" \
		--arg secret "$MIGRATION_DB_SECRET" \
		--arg coordinationKey "$MIGRATION_COORDINATION_KEY" \
		--arg policy "$MIGRATION_POLICY" \
		--arg registryAuthSecret "$REGISTRY_AUTH_SECRET" '
    {
      apiVersion: "operator.ptah.run/v1alpha1", kind: "PtahSchema",
      metadata: {namespace: $namespace, name: $name},
      spec: {
        target: {
          engine: $engine,
          coordinationKey: $coordinationKey,
          urlFrom: {name: $secret, key: "url"}
        },
        desired: {
          ociRef: "oci://example.invalid/schema:v1",
          registryAuthFrom: {
            name: $registryAuthSecret, mode: "Environment",
            usernameKey: "username", passwordKey: "password", registryKey: "registry"
          },
          verificationPolicyFrom: {name: $policy, key: "policy.yaml"},
          transport: {plainHTTP: true}
        },
        interval: "1h",
        execution: {activeDeadlineSeconds: 300}
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null

	# Both claimants, not only the newcomer: a refusal that blocked one side
	# would leave the other free to keep changing the database.
	rival_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$rival_deadline" ]; do
		migration_status
		rival_phase=$(jq -er '.status.phase' "$STATUS_FILE")
		schema_refused=$(k -n "$TEST_NAMESPACE" get ptahschema "$MIGRATION_RIVAL_SCHEMA" -o json |
			jq -r 'if (.status.conditions // []) | any(.type == "Ready" and .reason == "RealmConflict")
                   then "yes" else "no" end')
		if [ "$rival_phase" = Blocked ] && [ "$schema_refused" = yes ]; then
			break
		fi
		sleep 5
	done
	jq -e '
      .status as $status |
      $status.phase == "Blocked" and
      ($status.activeOperation // null) == null and
      (any($status.conditions[];
        .type == "Blocked" and .status == "True" and .reason == "RealmConflict")) and
      (any($status.conditions[]; .type == "Ready" and .status == "True") | not)
    ' "$STATUS_FILE" >/dev/null ||
		fail "$MIGRATION_NAME kept managing a database a PtahSchema also claims"
	[ "${schema_refused:-no}" = yes ] ||
		fail "$MIGRATION_RIVAL_SCHEMA was allowed to manage a database a PtahMigration also claims"
	# The refusal names counts and kinds and no other namespace's objects.
	scan_for_credentials "$STATUS_FILE" "the realm refusal"
	jq -e --arg key "$MIGRATION_COORDINATION_KEY" '
      [.status | .. | scalars | select(. == $key)] | length == 0
    ' "$STATUS_FILE" >/dev/null ||
		fail "the realm refusal published the coordination key"

	# The refusal precedes the first claim, so the newcomer never resolved its
	# reference and never created a Job.
	[ "$(k -n "$TEST_NAMESPACE" get jobs \
		-l "operator.ptah.run/schema=${MIGRATION_RIVAL_SCHEMA}" -o json |
		jq '.items | length')" -eq 0 ] ||
		fail "$MIGRATION_RIVAL_SCHEMA dispatched a Job for a database it may not manage"

	# Suspending a claimant ends the conflict, and the survivor is not edited to
	# make that happen: a resource that runs nothing claims nothing, which is
	# how one database is handed to one manager without declaring anything
	# shared.
	k -n "$TEST_NAMESPACE" patch ptahschema "$MIGRATION_RIVAL_SCHEMA" --type=merge \
		--patch '{"spec":{"suspend":true}}' >/dev/null
	wait_for_migration_phase InSync
	k -n "$TEST_NAMESPACE" delete ptahschema "$MIGRATION_RIVAL_SCHEMA" \
		--wait=true >/dev/null
	printf 'e2e migrations: PASS %s realm refusal and recovery\n' "$ENGINE_KIND" >&2
}

# The modified-file row of the matrix: an applied migration whose file changed
# afterwards is the refusal a versioned workflow exists to make.
#
# The tag moves to an artifact whose first migration is edited. Nothing about
# the database changed, so the refusal has to come from comparing the artifact
# against what the revision table recorded, and it has to leave the database
# exactly as the run left it.
# The partial-migration row of the matrix: a migration that committed some of
# its statements and not the rest.
#
# The fourth file opts out of the per-migration transaction, which is what
# makes the case real rather than arranged. A file that rolls back leaves the
# database as it was and needs no recovery at all; this one adds a column, then
# fails, and the column stays. Ptah records the revision as not applied with
# the statement count it reached, and the operator stops there: re-running a
# file that committed half of itself would run that half twice, and nothing
# reading the revision row can know which half.
#
# The recovery is a person's, because only a person can say what the half did.
# Here the decision is to undo it and take the migration out of the sequence.
# Nothing about the resource is edited to make that land -- the operator is
# reading the database at its interval, and a database somebody fixed is the
# whole recovery.
assert_partial_run_blocks_and_recovers() {
	printf 'e2e migrations: moving the %s tag to an artifact whose fourth migration commits half of itself\n' \
		"$ENGINE_KIND" >&2
	partial_applies_before=$(migration_apply_job_count)
	publish_migrations v3 "$MIGRATION_PARTIAL_FIXTURE_DIR"
	wait_for_migration_phase AwaitingApproval
	migration_status
	partial_plan=$(jq -er '.status.plan.name' "$STATUS_FILE") ||
		fail "$MIGRATION_NAME published no plan for the partial migration"
	jq -e '.status.history.pendingCount == 1 and .status.history.currentVersion == 3' \
		"$STATUS_FILE" >/dev/null ||
		fail "$MIGRATION_NAME did not plan the fourth migration alone"
	partial_plan_uid=$(k -n "$TEST_NAMESPACE" get ptahmigrationplan "$partial_plan" \
		-o jsonpath='{.metadata.uid}')
	partial_fingerprint=$(k -n "$TEST_NAMESPACE" get ptahmigrationplan "$partial_plan" \
		-o jsonpath='{.spec.fingerprint}')
	[ -n "$partial_plan_uid" ] && [ -n "$partial_fingerprint" ] ||
		fail "migration plan $partial_plan has no UID or fingerprint"
	approve_migration "$MIGRATION_PARTIAL_APPROVAL" "$partial_plan" \
		"$partial_plan_uid" "$partial_fingerprint" >/dev/null

	wait_for_migration_phase Blocked
	migration_status
	# The run's own account stops the resource, and the reason it carries
	# depends on whether the next history read has landed yet: the run says the
	# outcome is unattributable, and the database then says a revision is
	# dirty. Both are the same refusal, so neither is worth racing.
	jq -e '
      .status as $status |
      $status.phase == "Blocked" and
      $status.lastRun.outcome == "Partial" and
      ($status.lastRun.appliedVersions // []) == [] and
      ($status.plan // null) == null and
      ($status.activeOperation // null) == null and
      (any($status.conditions[];
        .type == "Blocked" and .status == "True" and
        (.reason == "ApplyOutcomeUnknown" or .reason == "HistoryDirty"))) and
      (any($status.conditions[]; .type == "Ready" and .status == "True") | not)
    ' "$STATUS_FILE" >/dev/null ||
		fail "$MIGRATION_NAME did not stop on a migration that committed half of itself"
	scan_for_credentials "$STATUS_FILE" "the partial-run refusal"

	# Partial is a fact about the database, not a label the run chose: the
	# first statement is committed and the revision says the file never
	# finished.
	[ "$(migration_widget_column_count weight)" = 1 ] ||
		fail "$ENGINE did not keep the statement the partial migration committed"
	[ "$(migration_query "SELECT count(*) FROM schema_migrations WHERE state <> 'applied'")" = 1 ] ||
		fail "$ENGINE recorded no unfinished revision for the migration that stopped halfway"

	# The reading that settles it is the database's. Once the history has been
	# read again the refusal names the dirty revision, and the pending count is
	# gone: nothing is pending behind a revision nobody has accounted for.
	dirty_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$dirty_deadline" ]; do
		record_migration_jobs
		migration_status
		if jq -e '
          .status as $status |
          $status.phase == "Blocked" and
          ($status.history.dirty // false) == true and
          $status.history.currentVersion == 3 and
          $status.history.pendingCount == 0 and
          (any($status.conditions[];
            .type == "Blocked" and .status == "True" and .reason == "HistoryDirty"))
        ' "$STATUS_FILE" >/dev/null; then
			dirty_settled=yes
			break
		fi
		sleep 5
	done
	[ "${dirty_settled:-no}" = yes ] ||
		fail "$MIGRATION_NAME never reported the dirty revision the partial run left"
	jq -e '[.status.conditions[] | select(.type == "Blocked") | .message]
           | any(test("[0-9]"))' "$STATUS_FILE" >/dev/null ||
		fail "the dirty refusal does not name the revision a person has to decide about"

	# A resource that stopped keeps reading and never runs again. The read-only
	# Jobs go on appearing, so the count that has to stand still is the Apply
	# one.
	partial_hold_deadline=$(($(date +%s) + 90))
	while [ "$(date +%s)" -lt "$partial_hold_deadline" ]; do
		record_migration_jobs
		[ "$(migration_phase)" = Blocked ] ||
			fail "$MIGRATION_NAME left Blocked while a partial migration stood unresolved"
		sleep 10
	done
	partial_applies_after=$(migration_apply_job_count)
	[ "$partial_applies_after" -eq "$((partial_applies_before + 1))" ] ||
		fail "$MIGRATION_NAME dispatched another run after a partial one: ${partial_applies_before} became ${partial_applies_after}"

	# The person's decision: the half is undone, the revision row goes with it,
	# and the sequence loses the migration that should not have run.
	printf 'e2e migrations: undoing the %s partial migration by hand and putting the sequence back\n' \
		"$ENGINE_KIND" >&2
	migration_query "ALTER TABLE e2e_migration_widgets DROP COLUMN weight" >/dev/null ||
		fail "could not undo the column the $ENGINE partial migration committed"
	migration_query "DELETE FROM schema_migrations WHERE state <> 'applied'" >/dev/null ||
		fail "could not take the unfinished $ENGINE revision out of the history"
	publish_migrations v4 "$MIGRATION_FIXTURE_DIR"

	wait_for_migration_phase InSync
	migration_status
	jq -e '
      .status as $status |
      $status.phase == "InSync" and
      $status.history.currentVersion == 3 and
      $status.history.appliedCount == 3 and
      $status.history.pendingCount == 0 and
      ($status.history.dirty // false) == false and
      ($status.plan // null) == null and
      $status.lastRun.outcome == "Partial" and
      (any($status.conditions[];
        .type == "Ready" and .status == "True" and .reason == "HistoryMatched")) and
      (any($status.conditions[]; .type == "Blocked" and .status == "True") | not)
    ' "$STATUS_FILE" >/dev/null ||
		fail "$MIGRATION_NAME did not recover on its own reading of a database somebody fixed"
	assert_database_migrated
	[ "$(migration_widget_column_count weight)" = 0 ] ||
		fail "$ENGINE kept the column the partial migration added after it was dropped"
	printf 'e2e migrations: PASS %s partial run blocked, and recovered without a spec edit\n' \
		"$ENGINE_KIND" >&2
}

assert_modified_file_blocks_everything() {
	printf 'e2e migrations: moving the %s tag to an artifact whose applied file changed\n' \
		"$ENGINE_KIND" >&2
	blocked_before_digest=$PUBLISHED_DIGEST
	publish_migrations v2 "$MIGRATION_EDITED_FIXTURE_DIR"
	[ "$PUBLISHED_DIGEST" != "$blocked_before_digest" ] ||
		fail "the edited $ENGINE artifact resolved to the digest the unedited one had"

	blocked_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$blocked_deadline" ]; do
		record_migration_jobs
		migration_status
		blocked_phase=$(jq -er '.status.phase' "$STATUS_FILE")
		[ "$blocked_phase" != Blocked ] || break
		[ "$blocked_phase" != Failed ] ||
			fail "$MIGRATION_NAME failed instead of refusing an edited applied migration"
		sleep 5
	done
	[ "${blocked_phase:-}" = Blocked ] ||
		fail "$MIGRATION_NAME did not refuse an edited applied migration within ${TIMEOUT_SECONDS}s; it is in ${blocked_phase:-<none>}"

	jq -e \
		--arg digest "$PUBLISHED_DIGEST" '
      .status as $status |
      $status.artifact.digest == $digest and
      $status.history.modifiedVersions == [1] and
      $status.history.currentVersion == 3 and
      ($status.plan // null) == null and
      ($status.activeOperation // null) == null and
      (any($status.conditions[];
        .type == "Blocked" and .status == "True" and .reason == "HistoryModified")) and
      (any($status.conditions[];
        .type == "Ready" and .status == "True") | not)
    ' "$STATUS_FILE" >/dev/null ||
		fail "$MIGRATION_NAME did not report the exact modified version and stay out of Ready"
	# Nothing ran, so nothing moved.
	assert_database_migrated
	first_name=$(migration_query "SELECT name FROM e2e_migration_widgets WHERE id = 1")
	[ "$first_name" = first ] ||
		fail "$ENGINE re-ran an applied migration: row 1 now reads $first_name"
}

assert_kubectl_ptah_migration() {
	view_phase=$1
	view_file=$WORK_DIR/kubectl-ptah-migration-${ENGINE}-${view_phase}.txt
	"$KUBECTL_PTAH_BINARY" migration "$MIGRATION_NAME" \
		--kubeconfig "$KUBECONFIG_FILE" -n "$TEST_NAMESPACE" >"$view_file" ||
		fail "kubectl ptah migration could not read $MIGRATION_NAME"
	scan_for_credentials "$view_file" "the kubectl ptah migration view"
	grep -Fx "Phase:          ${view_phase}" "$view_file" >/dev/null ||
		fail "kubectl ptah migration does not report phase $view_phase"
	grep -F "Migration:      ${TEST_NAMESPACE}/${MIGRATION_NAME}" "$view_file" >/dev/null ||
		fail "kubectl ptah migration does not name the resource it read"
	if grep -Ei 'create[[:space:]]+table|alter[[:space:]]+table|insert[[:space:]]+into|update[[:space:]]+e2e' \
		"$view_file" >/dev/null; then
		fail "kubectl ptah migration printed SQL"
	fi
	if [ "$view_phase" = AwaitingApproval ]; then
		grep -F "Plan ${MIGRATION_PLAN}, 3 migrations from version 0:" "$view_file" >/dev/null ||
			fail "kubectl ptah migration does not publish the plan a reader has to approve"
		view_order=$(sed -n 's/^  \([0-9][0-9]*\) .*/\1/p' "$view_file" | tr '\n' ' ')
		[ "$view_order" = "1 2 3 " ] ||
			fail "kubectl ptah migration printed the order as [$view_order], not the planned sequence"
		grep -Fx "Pending:        3" "$view_file" >/dev/null ||
			fail "kubectl ptah migration does not report the pending count"
	else
		grep -Fx "No plan is published." "$view_file" >/dev/null ||
			fail "kubectl ptah migration still shows a plan after the run that consumed it"
		view_applied=$(sed -n 's/^Run applied: *//p' "$view_file" |
			tr ',' '\n' | tr -d ' ' | sort -n | tr '\n' ' ')
		[ "$view_applied" = "1 2 3 " ] ||
			fail "kubectl ptah migration reports [$view_applied] applied, not the three versions the run recorded"
	fi
}

# run_engine_migrations drives one engine from an empty database to a history
# that matches the artifact, and proves each step on the way.
run_engine_migrations() {
	select_engine "$1"
	printf 'e2e migrations: starting the %s lifecycle on a database nothing has migrated\n' \
		"$ENGINE_KIND" >&2
	: >"$JOB_RECORDS_FILE"
	create_migration_database
	publish_migrations

	create_migration_resource
	wait_for_migration_phase AwaitingApproval
	assert_awaiting_approval
	assert_plan_sequence
	assert_kubectl_ptah_migration AwaitingApproval
	MIGRATION_PLAN_UID=$(k -n "$TEST_NAMESPACE" get ptahmigrationplan "$MIGRATION_PLAN" \
		-o jsonpath='{.metadata.uid}')
	MIGRATION_PLAN_FINGERPRINT=$(k -n "$TEST_NAMESPACE" get ptahmigrationplan "$MIGRATION_PLAN" \
		-o jsonpath='{.spec.fingerprint}')
	[ -n "$MIGRATION_PLAN_UID" ] && [ -n "$MIGRATION_PLAN_FINGERPRINT" ] ||
		fail "migration plan $MIGRATION_PLAN has no UID or fingerprint"

	approve_migration "$MIGRATION_APPROVAL" "$MIGRATION_PLAN" \
		"$MIGRATION_PLAN_UID" "$MIGRATION_PLAN_FINGERPRINT" >/dev/null
	assert_approval_hydrated
	wait_for_migration_phase InSync
	assert_in_sync
	assert_database_migrated
	assert_repeated_reconciliation_runs_nothing
	assert_migration_job_isolation
	assert_replaced_plan_approval_refused
	# The shortened interval means the resource may be mid-cycle by now; the
	# settled view is a statement about the settled state.
	wait_for_migration_phase InSync
	assert_kubectl_ptah_migration InSync
	assert_second_claimant_blocks_the_realm
	assert_partial_run_blocks_and_recovers
	assert_modified_file_blocks_everything
	printf 'e2e migrations: PASS %s approval gate, applied sequence, and matching history\n' \
		"$ENGINE_KIND" >&2
}

create_migration_policy
run_engine_migrations postgresql
run_engine_migrations mysql

PHASE_COMPLETED=1
printf '%s\n' 'e2e migrations: PASS approval gate, applied sequence, matching history, and credential isolation'
