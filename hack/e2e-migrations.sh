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
BRANCH_DB_URL_FILE=$WORK_DIR/branch-database.url
ADOPT_DB_URL_FILE=$WORK_DIR/adopt-database.url
ADOPT_SHADOW_DB_URL_FILE=$WORK_DIR/adopt-shadow-database.url
CHECKPOINT_DB_URL_FILE=$WORK_DIR/checkpoint-database.url
CHECKPOINT_PLAN_FILE=$WORK_DIR/checkpoint-plan.json
UNKNOWN_LAYER_DB_URL_FILE=$WORK_DIR/unknown-layer-database.url
UNCERTAIN_DB_URL_FILE=$WORK_DIR/uncertain-database.url
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
# The address the harness itself publishes through, which is the same registry
# under a different name: the cluster reaches it by Service, the host by the
# port the registry container publishes. One fixture below has to publish an
# artifact no product command can produce, and it runs here rather than in a
# Job, so it needs the second name.
REGISTRY_HOST_ADDRESS=${E2E_REGISTRY_HOST_ADDRESS:-}
REGISTRY_CREDENTIALS_FILE=${E2E_REGISTRY_CREDENTIALS_FILE:-}
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
	BRANCH_DATABASE=ptah_e2e_branch
	BRANCH_DB_SECRET="e2e-${ENGINE}-branch-db"
	BRANCH_MIGRATION="e2e-branch-${ENGINE}"
	BRANCH_APPROVAL="e2e-branch-${ENGINE}-approval"
	BRANCH_COORDINATION_KEY="e2e/branch/${ENGINE}"
	BRANCH_REFERENCE="oci://${REGISTRY_HOST}/migrations/${ENGINE}-branch:stable"
	BRANCH_FIXTURE_DIR="$ROOT_DIR/testdata/e2e/migrations/${ENGINE}-branch"
	BRANCH_LATE_FIXTURE_DIR="$ROOT_DIR/testdata/e2e/migrations/${ENGINE}-branch-late"
	ADOPT_DATABASE=ptah_e2e_adopt
	ADOPT_SHADOW_DATABASE=ptah_e2e_adopt_shadow
	ADOPT_DB_SECRET="e2e-${ENGINE}-adopt-db"
	ADOPT_MIGRATION="e2e-adopt-${ENGINE}"
	ADOPT_COORDINATION_KEY="e2e/adopt/${ENGINE}"
	ADOPT_REFERENCE="oci://${REGISTRY_HOST}/migrations/${ENGINE}-adopt:stable"
	ADOPT_CONFIGMAP="e2e-migrations-${ENGINE}-adopt"
	ADOPT_BASELINE_JOB="e2e-adopt-baseline-${ENGINE}"
	CHECKPOINT_DATABASE=ptah_e2e_checkpoint
	CHECKPOINT_DB_SECRET="e2e-${ENGINE}-checkpoint-db"
	CHECKPOINT_MIGRATION="e2e-checkpoint-${ENGINE}"
	CHECKPOINT_APPROVAL="e2e-checkpoint-${ENGINE}-approval"
	CHECKPOINT_COORDINATION_KEY="e2e/checkpoint/${ENGINE}"
	CHECKPOINT_REFERENCE="oci://${REGISTRY_HOST}/migrations/${ENGINE}-checkpoint:stable"
	CHECKPOINT_FIXTURE_DIR="$ROOT_DIR/testdata/e2e/migrations/${ENGINE}-checkpoint"
	UNCERTAIN_DATABASE=ptah_e2e_uncertain
	UNCERTAIN_DB_SECRET="e2e-${ENGINE}-uncertain-db"
	UNCERTAIN_MIGRATION="e2e-uncertain-${ENGINE}"
	UNCERTAIN_COORDINATION_KEY="e2e/uncertain/${ENGINE}"
	UNCERTAIN_REFERENCE="oci://${REGISTRY_HOST}/migrations/${ENGINE}-uncertain:stable"
	UNCERTAIN_FIXTURE_DIR="$ROOT_DIR/testdata/e2e/migrations/${ENGINE}-uncertain"
	UNKNOWN_LAYER_DATABASE=ptah_e2e_unknown_layer
	UNKNOWN_LAYER_DB_SECRET="e2e-${ENGINE}-unknown-layer-db"
	UNKNOWN_LAYER_MIGRATION="e2e-unknown-layer-${ENGINE}"
	UNKNOWN_LAYER_COORDINATION_KEY="e2e/unknown-layer/${ENGINE}"
	UNKNOWN_LAYER_REFERENCE="oci://${REGISTRY_HOST}/migrations/${ENGINE}-unknown:stable"
	UNKNOWN_LAYER_PUBLISH_REFERENCE="${REGISTRY_HOST_ADDRESS}/migrations/${ENGINE}-unknown:stable"
	# The adoption row reads the artifact this engine already publishes. A
	# second copy of the same three migrations would be a second thing to keep
	# in step with the schema the proof builds out of them by hand.
	ADOPT_FIXTURE_DIR="$MIGRATION_FIXTURE_DIR"
	[ -d "$BRANCH_FIXTURE_DIR" ] || fail "branch migration fixtures are missing: $BRANCH_FIXTURE_DIR"
	MIGRATION_EDITED_FIXTURE_DIR="$ROOT_DIR/testdata/e2e/migrations/${ENGINE}-modified"
	MIGRATION_PARTIAL_FIXTURE_DIR="$ROOT_DIR/testdata/e2e/migrations/${ENGINE}-partial"
	MIGRATION_OLDER_FIXTURE_DIR="$ROOT_DIR/testdata/e2e/migrations/${ENGINE}-older"
	MIGRATION_COORDINATION_DIGEST=$(coordination_digest "$ENGINE" "$MIGRATION_COORDINATION_KEY")
	[ -d "$MIGRATION_FIXTURE_DIR" ] || fail "migration fixtures are missing: $MIGRATION_FIXTURE_DIR"
	[ -d "$MIGRATION_PARTIAL_FIXTURE_DIR" ] ||
		fail "migration fixtures are missing: $MIGRATION_PARTIAL_FIXTURE_DIR"
	[ -d "$MIGRATION_OLDER_FIXTURE_DIR" ] ||
		fail "migration fixtures are missing: $MIGRATION_OLDER_FIXTURE_DIR"
	[ -d "$CHECKPOINT_FIXTURE_DIR" ] ||
		fail "migration fixtures are missing: $CHECKPOINT_FIXTURE_DIR"
	[ -d "$UNCERTAIN_FIXTURE_DIR" ] ||
		fail "migration fixtures are missing: $UNCERTAIN_FIXTURE_DIR"

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
	query_database=${2:-$MIGRATION_DATABASE}
	case "$ENGINE" in
	postgresql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -Atqc "$2"' \
			sh "$query_database" "$1" | tr -d '[:space:]'
		;;
	mysql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1" -Nse "$2"' \
			sh "$query_database" "$1" | tr -d '[:space:]'
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

# The Apply Jobs this migration has dispatched, by UID. A Blocked resource keeps
# reading, so its read-only Jobs go on appearing; what must not appear is
# another run.
#
# Identities rather than a count, because a finished Job carries a five-minute
# TTL and the ones from earlier in the lifecycle disappear while later rows are
# still running. A count that fell by one deletion and rose by one dispatch is
# the same count, and a count compared across a deletion accuses the wrong
# thing.
migration_apply_job_uids() {
	apply_job_resource=${1:-$MIGRATION_NAME}
	k -n "$TEST_NAMESPACE" get jobs \
		-l "operator.ptah.run/migration=${apply_job_resource},operator.ptah.run/operation=apply" \
		-o json | jq -r '.items[]?.metadata.uid' | LC_ALL=C sort
}

# assert_no_new_apply_job fails when an Apply Job appears that the recorded file
# does not name. A Job that went away in the meantime is not a finding: the TTL
# removes them, and nothing this proves depends on one staying.
assert_no_new_apply_job() {
	recorded_applies=$1
	dispatch_description=$2
	dispatch_resource=${3:-$MIGRATION_NAME}
	migration_apply_job_uids "$dispatch_resource" >"$WORK_DIR/applies-now.txt"
	if grep -vxF -f "$recorded_applies" "$WORK_DIR/applies-now.txt" | grep -q .; then
		fail "$dispatch_resource dispatched another run $dispatch_description"
	fi
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
	publish_reference=${3:-$MIGRATION_REFERENCE}
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
		--arg reference "$publish_reference" \
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
		# What this reading claims, and what it deliberately leaves out, is in
		# the filter file itself.
		if jq -e --argjson stoppedAt 3 \
			-f "$ROOT_DIR/testdata/e2e/migration-dirty-reading.jq" \
			"$STATUS_FILE" >/dev/null; then
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
	# Jobs go on appearing, so what has to stand still is the set of Apply ones,
	# recorded here with the partial run's own Job already in it.
	#
	# What is held is the refusal, not the phase; the filter file says why.
	migration_apply_job_uids >"$WORK_DIR/partial-applies.txt"
	partial_hold_deadline=$(($(date +%s) + 90))
	while [ "$(date +%s)" -lt "$partial_hold_deadline" ]; do
		record_migration_jobs
		migration_status
		jq -e -f "$ROOT_DIR/testdata/e2e/migration-partial-refusal.jq" \
			"$STATUS_FILE" >/dev/null ||
			fail "$MIGRATION_NAME stopped refusing while a partial migration stood unresolved"
		assert_no_new_apply_job "$WORK_DIR/partial-applies.txt" "after a partial one"
		sleep 10
	done

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

# The older-artifact row of the matrix: a tag moved back to an artifact that
# ends before the database does.
#
# Nothing is pending here, and that is the trap. Pending is a statement about
# the artifact's own migrations, so a revision the artifact does not carry is
# in no state at all, and a controller that only counts pending work reads this
# as success -- every sentence true, the verdict wrong. The database is asked
# what it holds and the artifact is asked what it ends at, and when the first
# is past the second the resource stops.
#
# There is no automatic recovery and there must not be one: rolling a database
# back to match an older artifact is a data-loss decision. Putting the tag back
# is a person's, and the operator converges on its own reading once it lands.
assert_older_artifact_blocks_everything() {
	printf 'e2e migrations: moving the %s tag back to an artifact that ends before the database does\n' \
		"$ENGINE_KIND" >&2
	migration_apply_job_uids >"$WORK_DIR/older-applies.txt"
	publish_migrations v5 "$MIGRATION_OLDER_FIXTURE_DIR"

	older_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$older_deadline" ]; do
		record_migration_jobs
		migration_status
		if jq -e '
          .status as $status |
          $status.phase == "Blocked" and
          (any($status.conditions[];
            .type == "Blocked" and .status == "True" and .reason == "HistoryAhead"))
        ' "$STATUS_FILE" >/dev/null; then
			older_blocked=yes
			break
		fi
		# InSync is the answer being refused here, but only once the status
		# names the artifact that is older. Until the moved tag is resolved the
		# resource is still settled on the one it was settled on, and failing
		# on that would be failing on the poll landing early.
		older_phase=$(jq -er '.status.phase' "$STATUS_FILE")
		older_digest=$(jq -r '.status.artifact.digest // ""' "$STATUS_FILE")
		if [ "$older_digest" = "$PUBLISHED_DIGEST" ] && [ "$older_phase" = InSync ]; then
			fail "$MIGRATION_NAME called an artifact older than its database InSync"
		fi
		sleep 5
	done
	[ "${older_blocked:-no}" = yes ] ||
		fail "$MIGRATION_NAME did not refuse an artifact that ends before its database within ${TIMEOUT_SECONDS}s"

	jq -e \
		--arg digest "$PUBLISHED_DIGEST" '
      .status as $status |
      $status.artifact.digest == $digest and
      $status.history.currentVersion == 3 and
      $status.history.appliedCount == 2 and
      $status.history.pendingCount == 0 and
      ($status.history.dirty // false) == false and
      ($status.history.modifiedVersions // []) == [] and
      ($status.plan // null) == null and
      ($status.activeOperation // null) == null and
      (any($status.conditions[]; .type == "Ready" and .status == "True") | not)
    ' "$STATUS_FILE" >/dev/null ||
		fail "$MIGRATION_NAME did not report the reading that disagrees with itself"
	# The refusal names both numbers, because only one of them is a field.
	jq -e '[.status.conditions[] | select(.type == "Blocked") | .message]
           | any(test("3") and test("2"))' "$STATUS_FILE" >/dev/null ||
		fail "the older-artifact refusal does not name the two versions that disagree"
	scan_for_credentials "$STATUS_FILE" "the older-artifact refusal"

	# Nothing ran, and above all nothing ran backwards.
	assert_no_new_apply_job "$WORK_DIR/older-applies.txt" \
		"for an artifact older than its database"
	assert_database_migrated

	printf 'e2e migrations: putting the %s tag back on the artifact the database was migrated with\n' \
		"$ENGINE_KIND" >&2
	publish_migrations v6 "$MIGRATION_FIXTURE_DIR"
	wait_for_migration_phase InSync
	migration_status
	jq -e '
      .status as $status |
      $status.history.currentVersion == 3 and
      $status.history.appliedCount == 3 and
      $status.history.pendingCount == 0 and
      (any($status.conditions[];
        .type == "Ready" and .status == "True" and .reason == "HistoryMatched"))
    ' "$STATUS_FILE" >/dev/null ||
		fail "$MIGRATION_NAME did not settle again once the artifact matched its database"
	printf 'e2e migrations: PASS %s refused an artifact older than its database, and settled when it was restored\n' \
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

# The incompatible-history row of the matrix: a migration that arrives below the
# version the database has already applied.
#
# It happens when two branches number migrations independently and the lower
# number lands second. Ptah executes in linear order and refuses the whole run
# while such a file is pending, so the operator refuses before it publishes a
# plan: a plan for it would ask a person to approve a sequence the executor
# cannot run, and the refusal would arrive as a failed Job instead of as the
# answer it is.
#
# The proof needs versions with room between them, which the main fixture does
# not have -- 1, 2 and 3 are all applied, and no integer sits between them. So
# it runs on a database and an artifact of its own, numbered 10 and 30, and the
# late arrival is 20.
branch_database_exists() {
	case "$ENGINE" in
	postgresql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atqc "$1"' \
			sh "SELECT count(*) FROM pg_database WHERE datname='${BRANCH_DATABASE}'" |
			tr -d '[:space:]'
		;;
	mysql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -Nse "$1"' \
			sh "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='${BRANCH_DATABASE}'" |
			tr -d '[:space:]'
		;;
	esac
}

create_branch_database() {
	[ "$(branch_database_exists)" = 0 ] ||
		fail "database $BRANCH_DATABASE already exists on $ENGINE; the out-of-order proof needs a history that starts from nothing"
	case "$ENGINE" in
	postgresql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -qc "$1"' \
			sh "CREATE DATABASE ${BRANCH_DATABASE}" >/dev/null ||
			fail "database $BRANCH_DATABASE could not be created"
		;;
	mysql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -e "$1"' \
			sh "CREATE DATABASE ${BRANCH_DATABASE}; GRANT ALL PRIVILEGES ON ${BRANCH_DATABASE}.* TO '${DATABASE_USER}'@'%'; FLUSH PRIVILEGES" >/dev/null ||
			fail "database $BRANCH_DATABASE could not be created"
		;;
	esac

	branch_password=$(cat "$MIGRATION_DB_PASSWORD_FILE")
	branch_authority="${DATABASE_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local"
	case "$ENGINE" in
	postgresql)
		branch_url="postgres://${DATABASE_USER}:${branch_password}@${branch_authority}:5432/${BRANCH_DATABASE}?sslmode=disable"
		;;
	mysql)
		branch_url="mysql://${DATABASE_USER}:${branch_password}@tcp(${branch_authority}:3306)/${BRANCH_DATABASE}"
		;;
	esac
	printf '%s' "$branch_url" >"$BRANCH_DB_URL_FILE"
	chmod 600 "$BRANCH_DB_URL_FILE"
	printf '%s\n' "$branch_url" >>"$CREDENTIAL_PATTERNS_FILE"
	branch_password=
	branch_url=
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$BRANCH_DB_SECRET" \
		--arg username "$DATABASE_USER" \
		--rawfile password "$MIGRATION_DB_PASSWORD_FILE" \
		--arg database "$BRANCH_DATABASE" \
		--rawfile url "$BRANCH_DB_URL_FILE" '
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

create_branch_migration_resource() {
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$BRANCH_MIGRATION" \
		--arg secret "$BRANCH_DB_SECRET" \
		--arg reference "$BRANCH_REFERENCE" \
		--arg coordinationKey "$BRANCH_COORDINATION_KEY" \
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
            name: $registryAuthSecret, mode: "Environment",
            usernameKey: "username", passwordKey: "password", registryKey: "registry"
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
}

wait_for_branch_phase() {
	branch_phase=$1
	branch_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$branch_deadline" ]; do
		branch_observed=$(k -n "$TEST_NAMESPACE" get ptahmigration "$BRANCH_MIGRATION" \
			-o jsonpath='{.status.phase}' 2>/dev/null || true)
		[ "$branch_observed" != "$branch_phase" ] || return 0
		sleep 5
	done
	fail "$BRANCH_MIGRATION did not reach $branch_phase within ${TIMEOUT_SECONDS}s; it is in ${branch_observed:-<none>}"
}

approve_branch_plan() {
	branch_plan=$(k -n "$TEST_NAMESPACE" get ptahmigration "$BRANCH_MIGRATION" \
		-o jsonpath='{.status.plan.name}')
	[ -n "$branch_plan" ] || fail "$BRANCH_MIGRATION published no plan to approve"
	branch_plan_uid=$(k -n "$TEST_NAMESPACE" get ptahmigrationplan "$branch_plan" \
		-o jsonpath='{.metadata.uid}')
	branch_plan_fingerprint=$(k -n "$TEST_NAMESPACE" get ptahmigrationplan "$branch_plan" \
		-o jsonpath='{.spec.fingerprint}')
	branch_migration_uid=$(k -n "$TEST_NAMESPACE" get ptahmigration "$BRANCH_MIGRATION" \
		-o jsonpath='{.metadata.uid}')
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$BRANCH_APPROVAL" \
		--arg migration "$BRANCH_MIGRATION" \
		--arg migrationUID "$branch_migration_uid" \
		--arg plan "$branch_plan" \
		--arg planUID "$branch_plan_uid" \
		--arg fingerprint "$branch_plan_fingerprint" '
    {
      apiVersion: "operator.ptah.run/v1alpha1", kind: "PtahMigrationApproval",
      metadata: {namespace: $namespace, name: $name},
      spec: {
        migrationRef: {name: $migration, uid: $migrationUID},
        planRef: {name: $plan, uid: $planUID},
        planFingerprint: $fingerprint
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
}

# assert_late_branch_migration_blocks is the row itself: the artifact gains a
# migration numbered below what the database has applied, and the operator
# refuses before planning rather than after a Job fails.
assert_late_branch_migration_blocks() {
	printf 'e2e migrations: publishing a %s migration numbered below the applied version\n' \
		"$ENGINE_KIND" >&2
	migration_apply_job_uids "$BRANCH_MIGRATION" >"$WORK_DIR/branch-applies.txt"
	publish_migrations "branch-late" "$BRANCH_LATE_FIXTURE_DIR" "$BRANCH_REFERENCE"

	branch_blocked_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$branch_blocked_deadline" ]; do
		k -n "$TEST_NAMESPACE" get ptahmigration "$BRANCH_MIGRATION" -o json >"$STATUS_FILE"
		scan_for_credentials "$STATUS_FILE" "$BRANCH_MIGRATION status"
		if jq -e '
          .status.phase == "Blocked" and
          (.status.conditions | any(
            .type == "Blocked" and .status == "True" and .reason == "HistoryOutOfOrder"))
        ' "$STATUS_FILE" >/dev/null; then
			break
		fi
		sleep 5
	done
	jq -e '
      .status.phase == "Blocked" and
      .status.activeOperation == null and
      (.status.history.outOfOrderVersions // []) == [20] and
      .status.history.currentVersion == 30 and
      (.status.conditions | any(
        .type == "Blocked" and .status == "True" and .reason == "HistoryOutOfOrder")) and
      (.status.conditions | any(
        .type == "Ready" and .status == "False" and .reason == "HistoryOutOfOrder")) and
      (.status | has("plan") | not)
    ' "$STATUS_FILE" >/dev/null ||
		fail "$BRANCH_MIGRATION did not refuse the out-of-order migration before planning"
	# No plan means no approval to give and no Job to run: the refusal has to
	# stop the work rather than describe it.
	[ "$(k -n "$TEST_NAMESPACE" get ptahmigrationplan \
		-l "operator.ptah.run/migration=${BRANCH_MIGRATION}" -o json | jq '.items | length')" -eq 1 ] ||
		fail "$BRANCH_MIGRATION published a plan for a sequence the executor refuses"
	assert_no_new_apply_job "$WORK_DIR/branch-applies.txt" \
		"for a migration it refuses to plan" "$BRANCH_MIGRATION"
	[ "$(migration_query "SELECT count(*) FROM information_schema.columns WHERE table_name = 'e2e_branch_widgets' AND column_name = 'label'" "$BRANCH_DATABASE")" = 0 ] ||
		fail "the out-of-order migration reached the database"
	printf 'e2e migrations: PASS %s refuses a migration numbered below the applied version\n' \
		"$ENGINE_KIND" >&2
}

# run_branch_out_of_order_proof applies a history with room between its versions
# and then hands the artifact a migration that lands in that room.
run_branch_out_of_order_proof() {
	create_branch_database
	publish_migrations "branch" "$BRANCH_FIXTURE_DIR" "$BRANCH_REFERENCE"
	create_branch_migration_resource
	wait_for_branch_phase AwaitingApproval
	approve_branch_plan
	wait_for_branch_phase InSync
	[ "$(k -n "$TEST_NAMESPACE" get ptahmigration "$BRANCH_MIGRATION" \
		-o jsonpath='{.status.history.currentVersion}')" = 30 ] ||
		fail "$BRANCH_MIGRATION did not apply its spaced history"
	assert_late_branch_migration_blocks
}

# The matrix row an existing schema asks for is a refusal. Ptah reports every
# migration pending on a database with an empty revision table whether or not
# that database already carries the schema, so the operator cannot tell the two
# apart and must not guess: it neither records the migrations as applied nor
# calls the database up to date. The adoption is a person's, and this proof runs
# it the way a person would, with Ptah's own baseline against a disposable
# shadow database.

database_exists() {
	exists_database=$1
	case "$ENGINE" in
	postgresql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atqc "$1"' \
			sh "SELECT count(*) FROM pg_database WHERE datname='${exists_database}'" |
			tr -d '[:space:]'
		;;
	mysql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -Nse "$1"' \
			sh "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='${exists_database}'" |
			tr -d '[:space:]'
		;;
	esac
}

create_database() {
	create_name=$1
	[ "$(database_exists "$create_name")" = 0 ] ||
		fail "database $create_name already exists on $ENGINE; the adoption proof builds the schema itself, so drop it before running this phase again"
	case "$ENGINE" in
	postgresql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -qc "$1"' \
			sh "CREATE DATABASE ${create_name}" >/dev/null ||
			fail "database $create_name could not be created"
		;;
	mysql)
		# The unprivileged user the Jobs connect as owns nothing by default, and
		# MySQL grants are per schema, so the grant belongs to creating it.
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -e "$1"' \
			sh "CREATE DATABASE ${create_name}; GRANT ALL PRIVILEGES ON ${create_name}.* TO '${DATABASE_USER}'@'%'; FLUSH PRIVILEGES" >/dev/null ||
			fail "database $create_name could not be created"
		;;
	esac
}

database_url() {
	url_database=$1
	url_password=$(cat "$MIGRATION_DB_PASSWORD_FILE")
	url_authority="${DATABASE_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local"
	case "$ENGINE" in
	postgresql)
		printf 'postgres://%s:%s@%s:5432/%s?sslmode=disable' \
			"$DATABASE_USER" "$url_password" "$url_authority" "$url_database"
		;;
	mysql)
		printf 'mysql://%s:%s@tcp(%s:3306)/%s' \
			"$DATABASE_USER" "$url_password" "$url_authority" "$url_database"
		;;
	esac
	url_password=
}

create_adopt_databases() {
	create_database "$ADOPT_DATABASE"
	# baseline verifies its claim before recording it: the shadow database is
	# where the migrations are replayed so the schema they produce can be
	# compared with the schema the target already has. Without it Ptah falls
	# back to reading Go entities from the working copy, which an executor image
	# does not carry, and refuses.
	create_database "$ADOPT_SHADOW_DATABASE"
	database_url "$ADOPT_DATABASE" >"$ADOPT_DB_URL_FILE"
	chmod 600 "$ADOPT_DB_URL_FILE"
	database_url "$ADOPT_SHADOW_DATABASE" >"$ADOPT_SHADOW_DB_URL_FILE"
	chmod 600 "$ADOPT_SHADOW_DB_URL_FILE"
	{
		cat "$ADOPT_DB_URL_FILE"
		printf '\n'
		cat "$ADOPT_SHADOW_DB_URL_FILE"
		printf '\n'
	} >>"$CREDENTIAL_PATTERNS_FILE"
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$ADOPT_DB_SECRET" \
		--arg username "$DATABASE_USER" \
		--rawfile password "$MIGRATION_DB_PASSWORD_FILE" \
		--arg database "$ADOPT_DATABASE" \
		--rawfile url "$ADOPT_DB_URL_FILE" \
		--rawfile shadowURL "$ADOPT_SHADOW_DB_URL_FILE" '
    {
      apiVersion: "v1", kind: "Secret",
      metadata: {namespace: $namespace, name: $name},
      immutable: true,
      type: "Opaque",
      stringData: {
        username: $username, password: $password,
        database: $database, url: $url, shadowUrl: $shadowURL
      }
    }' >"$SECRET_FILE"
	chmod 600 "$SECRET_FILE"
	k apply -f "$SECRET_FILE" >/dev/null
	rm -f "$SECRET_FILE"
}

# build_adopt_schema_without_the_operator replays the artifact's own migration
# SQL into a database the operator has never seen. The SQL comes from the
# fixtures rather than from a copy written here: a second copy would part
# company with the artifact the operator is about to read, and the row is about
# a database whose schema already matches that artifact exactly.
build_adopt_schema_without_the_operator() {
	adopt_replayed=0
	for adopt_file in "$ADOPT_FIXTURE_DIR"/*.up.sql; do
		[ -f "$adopt_file" ] || fail "migration fixtures are missing: $ADOPT_FIXTURE_DIR"
		adopt_sql=$(cat "$adopt_file")
		case "$ENGINE" in
		postgresql)
			# shellcheck disable=SC2016 # Variables expand inside the database container.
			k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
				sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -v ON_ERROR_STOP=1 -qc "$2"' \
				sh "$ADOPT_DATABASE" "$adopt_sql" >/dev/null ||
				fail "$(basename "$adopt_file") could not be replayed into $ADOPT_DATABASE"
			;;
		mysql)
			# shellcheck disable=SC2016 # Variables expand inside the database container.
			k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
				sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1" -e "$2"' \
				sh "$ADOPT_DATABASE" "$adopt_sql" >/dev/null ||
				fail "$(basename "$adopt_file") could not be replayed into $ADOPT_DATABASE"
			;;
		esac
		adopt_replayed=$((adopt_replayed + 1))
	done
	[ "$adopt_replayed" -eq 3 ] ||
		fail "the adoption proof replayed $adopt_replayed migrations, and the artifact carries three"
	[ "$(migration_query "SELECT count(*) FROM e2e_migration_widgets" "$ADOPT_DATABASE")" = 3 ] ||
		fail "the hand-built schema in $ADOPT_DATABASE does not carry the rows its migrations insert"
	[ "$(adopt_revision_tables)" = 0 ] ||
		fail "the hand-built schema in $ADOPT_DATABASE already carries a revision table"
}

# adopt_revision_tables counts the revision table Ptah records history in. It is
# the one object that separates a database nothing has migrated from a database
# that carries the schema and no account of it.
adopt_revision_tables() {
	case "$ENGINE" in
	postgresql)
		migration_query \
			"SELECT count(*) FROM information_schema.tables WHERE table_name = 'schema_migrations'" \
			"$ADOPT_DATABASE"
		;;
	mysql)
		migration_query \
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = '${ADOPT_DATABASE}' AND table_name = 'schema_migrations'" \
			"$ADOPT_DATABASE"
		;;
	esac
}

create_adopt_migration_resource() {
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$ADOPT_MIGRATION" \
		--arg secret "$ADOPT_DB_SECRET" \
		--arg reference "$ADOPT_REFERENCE" \
		--arg coordinationKey "$ADOPT_COORDINATION_KEY" \
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
            name: $registryAuthSecret, mode: "Environment",
            usernameKey: "username", passwordKey: "password", registryKey: "registry"
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
}

wait_for_adopt_phase() {
	adopt_phase=$1
	adopt_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$adopt_deadline" ]; do
		adopt_observed=$(k -n "$TEST_NAMESPACE" get ptahmigration "$ADOPT_MIGRATION" \
			-o jsonpath='{.status.phase}' 2>/dev/null || true)
		[ "$adopt_observed" != "$adopt_phase" ] || return 0
		sleep 5
	done
	fail "$ADOPT_MIGRATION did not reach $adopt_phase within ${TIMEOUT_SECONDS}s; it is in ${adopt_observed:-<none>}"
}

# assert_existing_schema_is_not_adopted is the row itself: the operator reads an
# empty history from a database that is not empty, and publishes the whole
# sequence for a person to decide about instead of recording any part of it as
# already applied. Nothing reaches the database.
assert_existing_schema_is_not_adopted() {
	k -n "$TEST_NAMESPACE" get ptahmigration "$ADOPT_MIGRATION" -o json >"$STATUS_FILE"
	scan_for_credentials "$STATUS_FILE" "$ADOPT_MIGRATION status"
	jq -e '
      .status.phase == "AwaitingApproval" and
      (.status.history.currentVersion // 0) == 0 and
      (.status.history.appliedCount // 0) == 0 and
      (.status.history.pendingCount // 0) == 3 and
      (.status.history.dirty // false) == false and
      (.status.conditions | any(
        .type == "ApprovalRequired" and .status == "True" and .reason == "AwaitingApproval")) and
      (.status.conditions | any(.type == "Ready" and .status == "False")) and
      (.status | has("lastRun") | not)
    ' "$STATUS_FILE" >/dev/null ||
		fail "$ADOPT_MIGRATION did not hold the whole sequence at the approval gate"
	[ "$(adopt_revision_tables)" = 0 ] ||
		fail "the operator wrote a revision table into a database it was never approved to migrate"
	assert_no_new_apply_job "$WORK_DIR/adopt-applies.txt" \
		"against a database that already carries the schema" "$ADOPT_MIGRATION"
	printf 'e2e migrations: %s held an existing schema at the approval gate and recorded nothing\n' \
		"$ENGINE_KIND" >&2
}

# run_adoption_baseline is the path the refusal leaves open, run the way a
# person runs it: Ptah's own baseline, verified against a shadow database before
# a single row is recorded. Nothing in the operator takes part, and the Job
# holds a database credential and no registry credential.
run_adoption_baseline() {
	adopt_mounts=$(find "$ADOPT_FIXTURE_DIR" -maxdepth 1 -type f -name '*.sql' \
		-exec basename {} \; | LC_ALL=C sort | jq -R . | jq -s .)
	[ "$(printf '%s' "$adopt_mounts" | jq 'length')" -gt 0 ] ||
		fail "no migration files to baseline from $ADOPT_FIXTURE_DIR"
	jq -n \
		--argjson mounts "$adopt_mounts" \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$ADOPT_BASELINE_JOB" \
		--arg image "$EXECUTOR_IMAGE" \
		--arg configMap "$ADOPT_CONFIGMAP" \
		--arg secret "$ADOPT_DB_SECRET" \
		--arg registryPullSecret "$REGISTRY_PULL_SECRET" '
    {
      apiVersion: "batch/v1", kind: "Job",
      metadata: {
        namespace: $namespace, name: $name,
        labels: {"app.kubernetes.io/component": "e2e-migration-adopter"}
      },
      spec: {
        backoffLimit: 0, activeDeadlineSeconds: 300,
        template: {
          metadata: {labels: {"app.kubernetes.io/component": "e2e-migration-adopter"}},
          spec: {
            restartPolicy: "Never", automountServiceAccountToken: false,
            imagePullSecrets: [{name: $registryPullSecret}],
            securityContext: {
              runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532,
              seccompProfile: {type: "RuntimeDefault"}
            },
            containers: [{
              name: "adopter", image: $image, imagePullPolicy: "IfNotPresent",
              command: ["/usr/local/bin/ptah"],
              args: [
                "migrations", "baseline", "--migrations-dir", "/migrations",
                "--dir-format", "ptah",
                "--db-url", "$(PTAH_E2E_TARGET_URL)",
                "--shadow-db", "$(PTAH_E2E_SHADOW_URL)"
              ],
              env: [
                {name: "HOME", value: "/work"},
                {name: "TMPDIR", value: "/work"},
                {name: "PTAH_E2E_TARGET_URL",
                 valueFrom: {secretKeyRef: {name: $secret, key: "url"}}},
                {name: "PTAH_E2E_SHADOW_URL",
                 valueFrom: {secretKeyRef: {name: $secret, key: "shadowUrl"}}}
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
	# The adopter reaches the database and must reach no registry: it is the
	# mirror of the boundary the publisher keeps, read off the object that was
	# actually created rather than off the text that asked for it.
	k create -f "$RESOURCE_FILE" >/dev/null
	k -n "$TEST_NAMESPACE" get job "$ADOPT_BASELINE_JOB" -o json |
		jq -e --arg secret "$ADOPT_DB_SECRET" '
      (.spec.template.spec.containers | length) == 1 and
      (.spec.template.spec.containers[0].env
       | map(select(.valueFrom.secretKeyRef.name != null) | .valueFrom.secretKeyRef.name)
       | unique) == [$secret]
    ' >/dev/null ||
		fail "the adoption Job did not keep registry access out of the process that runs SQL"
	adopt_baseline_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$adopt_baseline_deadline" ]; do
		adopt_baseline_state=$(k -n "$TEST_NAMESPACE" get job "$ADOPT_BASELINE_JOB" -o json |
			jq -r 'if (.status.succeeded // 0) > 0 then "succeeded"
                   elif (.status.failed // 0) > 0 then "failed" else "running" end')
		case "$adopt_baseline_state" in
		succeeded) break ;;
		failed)
			# The adopter holds a database URL, so its words are printed only
			# through the scanner that refuses one.
			k -n "$TEST_NAMESPACE" logs job/"$ADOPT_BASELINE_JOB" >"$LOG_FILE" 2>&1 || true
			scan_for_credentials "$LOG_FILE" "the migration adopter log"
			printf 'e2e migrations: the adopter said:\n' >&2
			sed 's/^/e2e migrations:   /' "$LOG_FILE" >&2
			fail "the baseline a person runs did not record the existing schema"
			;;
		esac
		sleep 3
	done
	[ "${adopt_baseline_state:-}" = succeeded ] ||
		fail "the adoption baseline Job did not finish within ${TIMEOUT_SECONDS}s"
}

# assert_adopted_history_matches closes the row: once a person has recorded the
# history, the operator settles on it without running anything, and the rows the
# database already held are still the rows it holds.
assert_adopted_history_matches() {
	[ "$(adopt_revision_tables)" = 1 ] ||
		fail "the baseline recorded no revision table in $ADOPT_DATABASE"
	[ "$(migration_query "SELECT count(*) FROM schema_migrations" "$ADOPT_DATABASE")" = 3 ] ||
		fail "the baseline did not record every migration the artifact carries"
	wait_for_adopt_phase InSync
	k -n "$TEST_NAMESPACE" get ptahmigration "$ADOPT_MIGRATION" -o json >"$STATUS_FILE"
	scan_for_credentials "$STATUS_FILE" "$ADOPT_MIGRATION status"
	jq -e '
      .status.phase == "InSync" and
      .status.history.currentVersion == 3 and
      (.status.history.appliedCount // 0) == 3 and
      (.status.history.pendingCount // 0) == 0 and
      (.status.conditions | any(
        .type == "Ready" and .status == "True" and .reason == "HistoryMatched")) and
      (.status.conditions | any(
        .type == "Blocked" and .status == "False" and .reason == "HistoryMatched")) and
      (.status | has("lastRun") | not)
    ' "$STATUS_FILE" >/dev/null ||
		fail "$ADOPT_MIGRATION did not settle on the history a person recorded"
	assert_no_new_apply_job "$WORK_DIR/adopt-applies.txt" \
		"after a person adopted the database" "$ADOPT_MIGRATION"
	[ "$(migration_query "SELECT color FROM e2e_migration_widgets WHERE id = 1" "$ADOPT_DATABASE")" = blue ] ||
		fail "the adopted database lost the rows its hand-built schema carried"
}

# run_existing_schema_adoption_proof is the matrix row "existing schema with an
# empty revision table": no implicit bootstrap or baseline, and an adoption path
# a person can take.
run_existing_schema_adoption_proof() {
	# Recorded before the resource exists, so the set it is compared against is
	# the empty one. `status.lastRun` is the durable half of the same statement:
	# a Job carries a five-minute TTL and an Apply that ran and was collected
	# would leave no Job to find, where the run it recorded never expires.
	: >"$WORK_DIR/adopt-applies.txt"
	create_adopt_databases
	build_adopt_schema_without_the_operator
	publish_migrations "adopt" "$ADOPT_FIXTURE_DIR" "$ADOPT_REFERENCE"
	create_adopt_migration_resource
	wait_for_adopt_phase AwaitingApproval
	assert_existing_schema_is_not_adopted
	run_adoption_baseline
	assert_adopted_history_matches
	printf 'e2e migrations: PASS %s refuses to adopt an existing schema, and settles once a person does\n' \
		"$ENGINE_KIND" >&2
}

# run_engine_migrations drives one engine from an empty database to a history
# that matches the artifact, and proves each step on the way.
# The checkpoint rows of the matrix. A checkpoint carries the schema and the
# rows its predecessors produce, so a database that has run nothing starts from
# it instead of replaying them -- and has to end up indistinguishable from one
# that did replay them. That equivalence is the claim, and the only honest way
# to check it is against a database that took the long way, which this phase
# already has.
#
# The comparison is made on this engine's own catalog rather than on a schema
# dump, because a dump is a third opinion about what the two databases hold.
checkpoint_column_shape() {
	shape_database=$1
	case "$ENGINE" in
	postgresql)
		migration_query "SELECT string_agg(column_name || '/' || data_type || '/' || is_nullable, ','
                       ORDER BY column_name)
                     FROM information_schema.columns
                     WHERE table_schema = current_schema()
                       AND table_name = 'e2e_migration_widgets'" \
			"$shape_database"
		;;
	mysql)
		migration_query "SELECT GROUP_CONCAT(CONCAT(column_name, '/', data_type, '/', is_nullable)
                       ORDER BY column_name SEPARATOR ',')
                     FROM information_schema.columns
                     WHERE table_schema = database()
                       AND table_name = 'e2e_migration_widgets'" \
			"$shape_database"
		;;
	esac
}

checkpoint_row_shape() {
	shape_database=$1
	case "$ENGINE" in
	postgresql)
		migration_query "SELECT string_agg(id || '/' || name || '/' || color, ',' ORDER BY id)
                     FROM e2e_migration_widgets" \
			"$shape_database"
		;;
	mysql)
		migration_query "SELECT GROUP_CONCAT(CONCAT(id, '/', name, '/', color)
                       ORDER BY id SEPARATOR ',')
                     FROM e2e_migration_widgets" \
			"$shape_database"
		;;
	esac
}

create_checkpoint_database() {
	create_database "$CHECKPOINT_DATABASE"
	database_url "$CHECKPOINT_DATABASE" >"$CHECKPOINT_DB_URL_FILE"
	chmod 600 "$CHECKPOINT_DB_URL_FILE"
	{
		cat "$CHECKPOINT_DB_URL_FILE"
		printf '\n'
	} >>"$CREDENTIAL_PATTERNS_FILE"
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$CHECKPOINT_DB_SECRET" \
		--arg username "$DATABASE_USER" \
		--rawfile password "$MIGRATION_DB_PASSWORD_FILE" \
		--arg database "$CHECKPOINT_DATABASE" \
		--rawfile url "$CHECKPOINT_DB_URL_FILE" '
    {
      apiVersion: "v1", kind: "Secret",
      metadata: {namespace: $namespace, name: $name},
      immutable: true,
      type: "Opaque",
      stringData: {username: $username, password: $password, database: $database, url: $url}
    }' >"$SECRET_FILE"
	chmod 600 "$SECRET_FILE"
	k apply -f "$SECRET_FILE" >/dev/null
	rm -f "$SECRET_FILE"
}

create_checkpoint_migration_resource() {
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$CHECKPOINT_MIGRATION" \
		--arg secret "$CHECKPOINT_DB_SECRET" \
		--arg reference "$CHECKPOINT_REFERENCE" \
		--arg coordinationKey "$CHECKPOINT_COORDINATION_KEY" \
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
            name: $registryAuthSecret, mode: "Environment",
            usernameKey: "username", passwordKey: "password", registryKey: "registry"
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
}

wait_for_checkpoint_phase() {
	checkpoint_phase=$1
	checkpoint_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$checkpoint_deadline" ]; do
		checkpoint_observed=$(k -n "$TEST_NAMESPACE" get ptahmigration "$CHECKPOINT_MIGRATION" \
			-o jsonpath='{.status.phase}' 2>/dev/null || true)
		[ "$checkpoint_observed" != "$checkpoint_phase" ] || return 0
		sleep 5
	done
	fail "$CHECKPOINT_MIGRATION did not reach $checkpoint_phase within ${TIMEOUT_SECONDS}s; it is in ${checkpoint_observed:-<none>}"
}

checkpoint_status() {
	k -n "$TEST_NAMESPACE" get ptahmigration "$CHECKPOINT_MIGRATION" -o json >"$STATUS_FILE" ||
		fail "$CHECKPOINT_MIGRATION could not be read"
	scan_for_credentials "$STATUS_FILE" "$CHECKPOINT_MIGRATION status"
}

# The gate says what the checkpoint changed: the two migrations it carries are
# reported as accounted for rather than pending, and what a person is asked to
# approve is the checkpoint and the migration after it.
assert_checkpoint_gate() {
	checkpoint_status
	jq -e '
      .status as $status |
      $status.phase == "AwaitingApproval" and
      ($status.history.currentVersion // 0) == 0 and
      $status.history.checkpointVersion == 3 and
      $status.history.appliedCount == 2 and
      $status.history.pendingCount == 2 and
      ($status.history.dirty // false) == false and
      ($status.history.modifiedVersions // []) == [] and
      ($status.history.outOfOrderVersions // []) == [] and
      (any($status.conditions[];
        .type == "ApprovalRequired" and .status == "True" and .reason == "AwaitingApproval"))
    ' "$STATUS_FILE" >/dev/null ||
		fail "$CHECKPOINT_MIGRATION did not hold a checkpoint bootstrap at the approval gate"
	checkpoint_plan=$(jq -er '.status.plan.name' "$STATUS_FILE")
	k -n "$TEST_NAMESPACE" get ptahmigrationplan "$checkpoint_plan" -o json >"$CHECKPOINT_PLAN_FILE"
	jq -e '[.spec.migrations[].version] == [3, 4]' "$CHECKPOINT_PLAN_FILE" >/dev/null ||
		fail "the $ENGINE checkpoint plan is not the checkpoint and the migration after it"
	printf 'e2e migrations: %s starts a fresh database at checkpoint 3, with 1 and 2 accounted for\n' \
		"$ENGINE_KIND" >&2
}

approve_checkpoint_plan() {
	checkpoint_plan_uid=$(k -n "$TEST_NAMESPACE" get ptahmigrationplan "$checkpoint_plan" \
		-o jsonpath='{.metadata.uid}')
	checkpoint_plan_fingerprint=$(k -n "$TEST_NAMESPACE" get ptahmigrationplan "$checkpoint_plan" \
		-o jsonpath='{.spec.fingerprint}')
	checkpoint_migration_uid=$(k -n "$TEST_NAMESPACE" get ptahmigration "$CHECKPOINT_MIGRATION" \
		-o jsonpath='{.metadata.uid}')
	[ -n "$checkpoint_plan_uid" ] && [ -n "$checkpoint_plan_fingerprint" ] ||
		fail "checkpoint plan $checkpoint_plan has no UID or fingerprint"
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$CHECKPOINT_APPROVAL" \
		--arg migration "$CHECKPOINT_MIGRATION" \
		--arg migrationUID "$checkpoint_migration_uid" \
		--arg plan "$checkpoint_plan" \
		--arg planUID "$checkpoint_plan_uid" \
		--arg fingerprint "$checkpoint_plan_fingerprint" '
    {
      apiVersion: "operator.ptah.run/v1alpha1", kind: "PtahMigrationApproval",
      metadata: {namespace: $namespace, name: $name},
      spec: {
        migrationRef: {name: $migration, uid: $migrationUID},
        planRef: {name: $plan, uid: $planUID},
        planFingerprint: $fingerprint
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
}

# The row itself: what the bootstrapped database holds is what the replayed one
# holds. The checkpoint version is gone from the reading afterwards, because it
# describes a bootstrap that has happened rather than one that is about to, and
# the resource settles instead of publishing the covered migrations again.
assert_checkpoint_equals_the_long_way() {
	checkpoint_status
	jq -e '
      .status as $status |
      $status.phase == "InSync" and
      $status.history.currentVersion == 4 and
      $status.history.pendingCount == 0 and
      ($status.history.dirty // false) == false and
      ($status.plan // null) == null and
      ($status.history.checkpointVersion // 0) == 0 and
      $status.lastRun.outcome == "Applied" and
      ($status.lastRun.appliedVersions // []) == [3, 4] and
      (any($status.conditions[];
        .type == "Ready" and .status == "True" and .reason == "HistoryMatched"))
    ' "$STATUS_FILE" >/dev/null ||
		fail "$CHECKPOINT_MIGRATION did not settle on the history its bootstrap produced"

	checkpoint_columns=$(checkpoint_column_shape "$CHECKPOINT_DATABASE")
	replayed_columns=$(checkpoint_column_shape "$MIGRATION_DATABASE")
	[ -n "$checkpoint_columns" ] ||
		fail "the bootstrapped $ENGINE database has no table to compare"
	[ "$checkpoint_columns" = "$replayed_columns" ] ||
		fail "the bootstrapped $ENGINE schema is [$checkpoint_columns], and the replayed one is [$replayed_columns]"

	checkpoint_rows=$(checkpoint_row_shape "$CHECKPOINT_DATABASE")
	replayed_rows=$(checkpoint_row_shape "$MIGRATION_DATABASE")
	[ -n "$checkpoint_rows" ] ||
		fail "the bootstrapped $ENGINE database carries none of the rows its checkpoint seeds"
	[ "$checkpoint_rows" = "$replayed_rows" ] ||
		fail "the bootstrapped $ENGINE rows are [$checkpoint_rows], and the replayed ones are [$replayed_rows]"

	# The migration after the checkpoint depends on the data the checkpoint
	# seeded, so a bootstrap that skipped the seeding would leave this row
	# unrecolored rather than fail outright.
	[ "$(migration_query "SELECT color FROM e2e_migration_widgets WHERE id = 1" "$CHECKPOINT_DATABASE")" = blue ] ||
		fail "the $ENGINE migration after the checkpoint did not run against the rows the checkpoint seeded"
}

# A second reading changes nothing. The covered migrations report themselves
# pending once the bootstrap is behind the database, and a resource that
# recounted them would publish a plan for migrations the checkpoint replaced and
# ask for an approval to run them, on every pass, forever.
#
# What is watched is the plan and the approval, not the phase. A settled
# resource still resolves, verifies and reads at its interval, so it is
# legitimately out of InSync for part of every cycle; requiring InSync on each
# poll would fail on the poll that landed mid-cycle. The proof is that the
# history was read again -- a new observedAt -- with no plan published on the
# way, and that it is InSync at the end.
assert_checkpoint_bootstrap_stays_settled() {
	checkpoint_status
	checkpoint_observed_before=$(jq -er '.status.history.observedAt' "$STATUS_FILE")
	checkpoint_settled_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$checkpoint_settled_deadline" ]; do
		checkpoint_status
		jq -e '
          .status as $status |
          ($status.plan // null) == null and
          $status.phase != "AwaitingApproval" and
          $status.phase != "Blocked" and
          (any($status.conditions[];
            .type == "ApprovalRequired" and .status == "True") | not)
        ' "$STATUS_FILE" >/dev/null ||
			fail "$CHECKPOINT_MIGRATION asked for another approval after its bootstrap settled"
		checkpoint_observed_now=$(jq -er '.status.history.observedAt' "$STATUS_FILE")
		if [ "$checkpoint_observed_now" != "$checkpoint_observed_before" ] &&
			[ "$(jq -er '.status.phase' "$STATUS_FILE")" = InSync ]; then
			checkpoint_reread=yes
			break
		fi
		sleep 5
	done
	[ "${checkpoint_reread:-no}" = yes ] ||
		fail "$CHECKPOINT_MIGRATION did not read its history again within ${TIMEOUT_SECONDS}s"
	jq -e '
      .status as $status |
      $status.phase == "InSync" and
      $status.history.pendingCount == 0 and
      $status.history.currentVersion == 4 and
      ($status.plan // null) == null
    ' "$STATUS_FILE" >/dev/null ||
		fail "$CHECKPOINT_MIGRATION did not settle again on the history its bootstrap produced"
}

run_checkpoint_bootstrap_proof() {
	create_checkpoint_database
	publish_migrations "checkpoint" "$CHECKPOINT_FIXTURE_DIR" "$CHECKPOINT_REFERENCE"
	create_checkpoint_migration_resource
	wait_for_checkpoint_phase AwaitingApproval
	assert_checkpoint_gate
	approve_checkpoint_plan
	wait_for_checkpoint_phase InSync
	assert_checkpoint_equals_the_long_way
	k -n "$TEST_NAMESPACE" patch ptahmigration "$CHECKPOINT_MIGRATION" --type=merge \
		--patch '{"spec":{"interval":"30s"}}' >/dev/null
	assert_checkpoint_bootstrap_stays_settled
	printf 'e2e migrations: PASS %s bootstrapped from a checkpoint and matches the database that replayed everything\n' \
		"$ENGINE_KIND" >&2
}

# The row the matrix calls "failure after the SQL and before the status update".
#
# A run that committed and a controller that never got to say so. The evidence
# is removed while the run is still going, because a sequence that finishes in a
# second leaves no window: the controller would read the result before anything
# could take it away, and the proof would be a race. The third migration of this
# artifact sleeps for that reason, and the first two are committed by the time
# it starts, which is what makes the interruption a failure after the SQL.
#
# What the operator owes here is not a retry. Re-running the file would run its
# statements twice, and the first migration inserts rows, so a blind replay is
# visible as duplicate-key failure or as doubled data. The answer is to stop and
# say the outcome is unknown.
create_uncertain_database() {
	create_database "$UNCERTAIN_DATABASE"
	database_url "$UNCERTAIN_DATABASE" >"$UNCERTAIN_DB_URL_FILE"
	chmod 600 "$UNCERTAIN_DB_URL_FILE"
	{
		cat "$UNCERTAIN_DB_URL_FILE"
		printf '\n'
	} >>"$CREDENTIAL_PATTERNS_FILE"
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$UNCERTAIN_DB_SECRET" \
		--arg username "$DATABASE_USER" \
		--rawfile password "$MIGRATION_DB_PASSWORD_FILE" \
		--arg database "$UNCERTAIN_DATABASE" \
		--rawfile url "$UNCERTAIN_DB_URL_FILE" '
    {
      apiVersion: "v1", kind: "Secret",
      metadata: {namespace: $namespace, name: $name},
      immutable: true,
      type: "Opaque",
      stringData: {username: $username, password: $password, database: $database, url: $url}
    }' >"$SECRET_FILE"
	chmod 600 "$SECRET_FILE"
	k apply -f "$SECRET_FILE" >/dev/null
	rm -f "$SECRET_FILE"
}

# Always, because the row is about a run that started and not about the gate
# that authorizes one. An approval here would only add a step between the
# publish and the interruption.
create_uncertain_migration_resource() {
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$UNCERTAIN_MIGRATION" \
		--arg secret "$UNCERTAIN_DB_SECRET" \
		--arg reference "$UNCERTAIN_REFERENCE" \
		--arg coordinationKey "$UNCERTAIN_COORDINATION_KEY" \
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
            name: $registryAuthSecret, mode: "Environment",
            usernameKey: "username", passwordKey: "password", registryKey: "registry"
          },
          verificationPolicyFrom: {name: $policy, key: "policy.yaml"},
          transport: {plainHTTP: true}
        },
        policy: {apply: "Always", lockTimeout: "30s"},
        interval: $interval,
        execution: {
          activeDeadlineSeconds: 300, failureRetryInterval: "10s", connectTimeout: "30s"
        }
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
}

uncertain_status() {
	k -n "$TEST_NAMESPACE" get ptahmigration "$UNCERTAIN_MIGRATION" -o json >"$STATUS_FILE" ||
		fail "$UNCERTAIN_MIGRATION could not be read"
	scan_for_credentials "$STATUS_FILE" "$UNCERTAIN_MIGRATION status"
}

# The Job to remove is the one the resource says it dispatched, by name and by
# UID. A Job found by label could be a later one, and removing that would prove
# something about a run nobody was waiting on.
wait_for_uncertain_apply_dispatch() {
	dispatch_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$dispatch_deadline" ]; do
		uncertain_status
		if jq -e '
          .status.activeOperation.type == "Apply" and
          ((.status.activeOperation.jobName // "") | length) > 0 and
          ((.status.activeOperation.jobUID // "") | length) > 0
        ' "$STATUS_FILE" >/dev/null; then
			UNCERTAIN_APPLY_JOB=$(jq -er '.status.activeOperation.jobName' "$STATUS_FILE")
			UNCERTAIN_APPLY_JOB_UID=$(jq -er '.status.activeOperation.jobUID' "$STATUS_FILE")
			return 0
		fi
		sleep 2
	done
	fail "$UNCERTAIN_MIGRATION did not dispatch an Apply bound to its own Job within ${TIMEOUT_SECONDS}s"
}

# The database is what says the SQL committed, not the Job and not the status.
# Waiting for the second migration to be recorded is waiting for the part of the
# run that must survive the interruption.
wait_for_uncertain_commit() {
	commit_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$commit_deadline" ]; do
		if [ "$(migration_query "SELECT count(*) FROM schema_migrations WHERE version <= 2 AND state = 'applied'" \
			"$UNCERTAIN_DATABASE")" = 2 ]; then
			return 0
		fi
		sleep 2
	done
	fail "the $ENGINE uncertain run did not commit its first two migrations within ${TIMEOUT_SECONDS}s"
}

assert_uncertain_apply_blocks_without_replaying() {
	wait_for_uncertain_phase Blocked
	uncertain_status
	jq -e '
      .status as $status |
      $status.phase == "Blocked" and
      $status.lastRun.outcome == "Unknown" and
      ($status.activeOperation // null) == null and
      ($status.plan // null) == null and
      (any($status.conditions[];
        .type == "Blocked" and .status == "True" and .reason == "ApplyOutcomeUnknown")) and
      (any($status.conditions[]; .type == "Ready" and .status == "True") | not)
    ' "$STATUS_FILE" >/dev/null ||
		fail "$UNCERTAIN_MIGRATION did not stop on a run whose evidence it could not read"
	scan_for_credentials "$STATUS_FILE" "the uncertain-run refusal"

	# The run is over and the database keeps what it committed. Both halves
	# matter: without the first the refusal is about nothing, and without the
	# second there would be nothing a replay could double.
	[ "$(migration_query "SELECT count(*) FROM e2e_migration_widgets" "$UNCERTAIN_DATABASE")" = 3 ] ||
		fail "the $ENGINE uncertain run did not leave the rows its first migration inserted"
	# Not the absence of a row: a run that was cut off may leave a revision
	# behind, and whether it does is Ptah's business. What may not have happened
	# is the migration recording itself finished.
	[ "$(migration_query "SELECT count(*) FROM schema_migrations WHERE version = 3 AND state = 'applied'" \
		"$UNCERTAIN_DATABASE")" = 0 ] ||
		fail "the $ENGINE migration that was interrupted recorded itself applied"

	# Nothing dispatches again. A replay would re-run the first migration, whose
	# insert is not idempotent, so this is the assertion the row exists for.
	migration_apply_job_uids "$UNCERTAIN_MIGRATION" >"$WORK_DIR/uncertain-applies.txt"
	# The same refusal the partial row holds, and the same filter: a run that
	# stopped and a run nobody could read owe the reader the same thing, so
	# they are not two claims with two chances to drift.
	uncertain_hold_deadline=$(($(date +%s) + 90))
	while [ "$(date +%s)" -lt "$uncertain_hold_deadline" ]; do
		uncertain_status
		jq -e -f "$ROOT_DIR/testdata/e2e/migration-partial-refusal.jq" \
			"$STATUS_FILE" >/dev/null ||
			fail "$UNCERTAIN_MIGRATION stopped refusing while its run stood unaccounted for"
		assert_no_new_apply_job "$WORK_DIR/uncertain-applies.txt" \
			"after one whose evidence it could not read" "$UNCERTAIN_MIGRATION"
		sleep 10
	done
	[ "$(migration_query "SELECT count(*) FROM e2e_migration_widgets" "$UNCERTAIN_DATABASE")" = 3 ] ||
		fail "the $ENGINE rows were doubled, so a run was replayed over what it had already committed"
}

wait_for_uncertain_phase() {
	uncertain_phase=$1
	uncertain_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$uncertain_deadline" ]; do
		uncertain_observed=$(k -n "$TEST_NAMESPACE" get ptahmigration "$UNCERTAIN_MIGRATION" \
			-o jsonpath='{.status.phase}' 2>/dev/null || true)
		[ "$uncertain_observed" != "$uncertain_phase" ] || return 0
		sleep 5
	done
	fail "$UNCERTAIN_MIGRATION did not reach $uncertain_phase within ${TIMEOUT_SECONDS}s; it is in ${uncertain_observed:-<none>}"
}

run_uncertain_apply_proof() {
	create_uncertain_database
	publish_migrations "uncertain" "$UNCERTAIN_FIXTURE_DIR" "$UNCERTAIN_REFERENCE"
	create_uncertain_migration_resource
	wait_for_uncertain_apply_dispatch
	wait_for_uncertain_commit
	printf 'e2e migrations: removing the %s Apply Job while its run is still going\n' \
		"$ENGINE_KIND" >&2
	# The name is reused across attempts, so the UID is what says this is the
	# Job the resource is waiting on rather than a later one under the same name.
	uncertain_live_uid=$(k -n "$TEST_NAMESPACE" get job "$UNCERTAIN_APPLY_JOB" \
		-o jsonpath='{.metadata.uid}' 2>/dev/null || true)
	[ "$uncertain_live_uid" = "$UNCERTAIN_APPLY_JOB_UID" ] ||
		fail "the $ENGINE Apply Job under that name is not the one the resource dispatched"
	k -n "$TEST_NAMESPACE" delete job "$UNCERTAIN_APPLY_JOB" --wait=true >/dev/null ||
		fail "the $ENGINE Apply Job could not be removed"
	assert_uncertain_apply_blocks_without_replaying
	printf 'e2e migrations: PASS %s stopped on a run it could not read, and replayed nothing\n' \
		"$ENGINE_KIND" >&2
}

# The capability row of the matrix: an executor meets an artifact built by
# something newer than itself.
#
# A reader that finds a layer outside the set it accepts refuses the whole
# artifact on the layer descriptor, before the bytes are fetched. That is how it
# fails closed instead of reading around what it cannot understand, and it is
# the refusal ADR 0019 assigns: the media type suffix is the version, and the
# refusal lands before a database connection exists.
#
# No product command builds this artifact, and that is the point of the fixture:
# `ptah migrations push` writes the layers it knows. What is imitated is not
# corruption but the next version of the format.
publish_unknown_layer_artifact() {
	[ -n "$REGISTRY_HOST_ADDRESS" ] ||
		fail "the harness published no registry address, so the unknown-layer artifact cannot be built"
	[ -s "$REGISTRY_CREDENTIALS_FILE" ] ||
		fail "the harness published no registry credentials file"
	printf 'e2e migrations: publishing a %s artifact carrying a layer this executor cannot accept\n' \
		"$ENGINE_KIND" >&2
	unknown_username=$(jq -er '.username' "$REGISTRY_CREDENTIALS_FILE")
	unknown_password=$(jq -er '.password' "$REGISTRY_CREDENTIALS_FILE")
	go -C "$ROOT_DIR" run ./hack/unknownlayerfixture \
		--reference "$UNKNOWN_LAYER_PUBLISH_REFERENCE" \
		--dir "$MIGRATION_FIXTURE_DIR" \
		--unknown-media-type "application/vnd.stokaro.ptah.migration.capability.v1" \
		--username "$unknown_username" \
		--password "$unknown_password" \
		--plain-http >"$LOG_FILE" 2>&1 ||
		{
			scan_for_credentials "$LOG_FILE" "the unknown-layer publisher"
			sed 's/^/e2e migrations:   /' "$LOG_FILE" >&2
			fail "the unknown-layer artifact could not be published"
		}
	unknown_username=
	unknown_password=
	scan_for_credentials "$LOG_FILE" "the unknown-layer publisher"
}

create_unknown_layer_database() {
	create_database "$UNKNOWN_LAYER_DATABASE"
	database_url "$UNKNOWN_LAYER_DATABASE" >"$UNKNOWN_LAYER_DB_URL_FILE"
	chmod 600 "$UNKNOWN_LAYER_DB_URL_FILE"
	{
		cat "$UNKNOWN_LAYER_DB_URL_FILE"
		printf '\n'
	} >>"$CREDENTIAL_PATTERNS_FILE"
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$UNKNOWN_LAYER_DB_SECRET" \
		--arg username "$DATABASE_USER" \
		--rawfile password "$MIGRATION_DB_PASSWORD_FILE" \
		--arg database "$UNKNOWN_LAYER_DATABASE" \
		--rawfile url "$UNKNOWN_LAYER_DB_URL_FILE" '
    {
      apiVersion: "v1", kind: "Secret",
      metadata: {namespace: $namespace, name: $name},
      immutable: true,
      type: "Opaque",
      stringData: {username: $username, password: $password, database: $database, url: $url}
    }' >"$SECRET_FILE"
	chmod 600 "$SECRET_FILE"
	k apply -f "$SECRET_FILE" >/dev/null
	rm -f "$SECRET_FILE"
}

create_unknown_layer_migration_resource() {
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$UNKNOWN_LAYER_MIGRATION" \
		--arg secret "$UNKNOWN_LAYER_DB_SECRET" \
		--arg reference "$UNKNOWN_LAYER_REFERENCE" \
		--arg coordinationKey "$UNKNOWN_LAYER_COORDINATION_KEY" \
		--arg policy "$MIGRATION_POLICY" \
		--arg registryAuthSecret "$REGISTRY_AUTH_SECRET" \
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
            name: $registryAuthSecret, mode: "Environment",
            usernameKey: "username", passwordKey: "password", registryKey: "registry"
          },
          verificationPolicyFrom: {name: $policy, key: "policy.yaml"},
          transport: {plainHTTP: true}
        },
        policy: {apply: "Always", lockTimeout: "30s"},
        interval: "30s",
        execution: {
          activeDeadlineSeconds: 300, failureRetryInterval: "10s", connectTimeout: "30s"
        }
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
}

unknown_layer_revision_tables() {
	case "$ENGINE" in
	postgresql)
		migration_query \
			"SELECT count(*) FROM information_schema.tables WHERE table_name = 'schema_migrations'" \
			"$UNKNOWN_LAYER_DATABASE"
		;;
	mysql)
		migration_query \
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = '${UNKNOWN_LAYER_DATABASE}' AND table_name = 'schema_migrations'" \
			"$UNKNOWN_LAYER_DATABASE"
		;;
	esac
}

# Two statements, and the order between them is the point. First the refusal has
# to arrive, which takes as long as resolving and verifying take -- only the
# history read fetches artifact bytes, so that is the operation whose fetch step
# fails, and it is also the one that would have opened the database. Then the
# window is held open to say that nothing follows it.
#
# Holding a window without waiting for the refusal first would fail on the poll
# that landed before the operator had got that far, which is a statement about
# the harness rather than about the operator.
assert_unknown_layer_refusal_is_named() {
	named_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$named_deadline" ]; do
		k -n "$TEST_NAMESPACE" get ptahmigration "$UNKNOWN_LAYER_MIGRATION" -o json >"$STATUS_FILE" ||
			fail "$UNKNOWN_LAYER_MIGRATION could not be read"
		scan_for_credentials "$STATUS_FILE" "$UNKNOWN_LAYER_MIGRATION status"
		if jq -e --arg step "fetch-migrations" \
			-f "$ROOT_DIR/testdata/e2e/migration-refused-boundary.jq" \
			"$STATUS_FILE" >/dev/null; then
			return 0
		fi
		sleep 5
	done
	fail "$UNKNOWN_LAYER_MIGRATION never named the step that refused the artifact within ${TIMEOUT_SECONDS}s"
}

assert_unknown_layer_never_reaches_the_database() {
	assert_unknown_layer_refusal_is_named
	unknown_deadline=$(($(date +%s) + 90))
	while [ "$(date +%s)" -lt "$unknown_deadline" ]; do
		k -n "$TEST_NAMESPACE" get ptahmigration "$UNKNOWN_LAYER_MIGRATION" -o json >"$STATUS_FILE" ||
			fail "$UNKNOWN_LAYER_MIGRATION could not be read"
		scan_for_credentials "$STATUS_FILE" "$UNKNOWN_LAYER_MIGRATION status"
		jq -e '
          .status as $status |
          ($status.plan // null) == null and
          ($status.lastRun // null) == null and
          ($status.phase != "AwaitingApproval") and
          ($status.phase != "InSync") and
          (($status.history.appliedCount // 0) == 0)
        ' "$STATUS_FILE" >/dev/null ||
			fail "$UNKNOWN_LAYER_MIGRATION acted on an artifact carrying a layer its executor cannot read"
		[ "$(k -n "$TEST_NAMESPACE" get jobs \
			-l "operator.ptah.run/migration=${UNKNOWN_LAYER_MIGRATION},operator.ptah.run/operation=apply" \
			-o json | jq '.items | length')" -eq 0 ] ||
			fail "$UNKNOWN_LAYER_MIGRATION dispatched a run for an artifact its executor refused"
		sleep 10
	done

	# Nothing connected. Ptah creates its revision table on the first history
	# read, so a database that has none was never opened, which is the half of
	# this row that the refusal's own message cannot prove.
	[ "$(unknown_layer_revision_tables)" = 0 ] ||
		fail "the $ENGINE database was opened for an artifact whose layers were refused"
}

run_unknown_layer_proof() {
	create_unknown_layer_database
	publish_unknown_layer_artifact
	create_unknown_layer_migration_resource
	assert_unknown_layer_never_reaches_the_database
	printf 'e2e migrations: PASS %s refused an artifact built newer than its executor, before opening the database\n' \
		"$ENGINE_KIND" >&2
}

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
	assert_older_artifact_blocks_everything
	assert_modified_file_blocks_everything
	run_branch_out_of_order_proof
	run_existing_schema_adoption_proof
	run_checkpoint_bootstrap_proof
	run_uncertain_apply_proof
	run_unknown_layer_proof
	printf 'e2e migrations: PASS %s approval gate, applied sequence, and matching history\n' \
		"$ENGINE_KIND" >&2
}

create_migration_policy
run_engine_migrations postgresql
run_engine_migrations mysql

PHASE_COMPLETED=1
printf '%s\n' 'e2e migrations: PASS approval gate, applied sequence, matching history, and credential isolation'
