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
OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:-}
REGISTRY_SERVICE=${E2E_REGISTRY_SERVICE:-registry}
INTERVAL=${E2E_REFERENCE_DATA_INTERVAL:-45s}
TIMEOUT_SECONDS=${E2E_TIMEOUT_SECONDS:-600}

# Imported variables retain their export attribute across reassignment in POSIX
# shells. Clear every secret-bearing name before loading task values.
unset PG_PASSWORD REFERENCE_DB_URL

# A command that fails outside a guard calling fail ends the shell with no
# reason printed, and the EXIT trap then dumps diagnostics that explain
# nothing. So fail records that it spoke, in a file rather than a variable
# so that a fail inside a subshell still counts, and the trap says so when
# nothing did.
PHASE_REASON_MARKER=${TMPDIR:-/tmp}/ptah-e2e-reason-reference-data.$$

fail() {
	printf 'e2e reference data: %s\n' "$*" >&2
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
	KUBECONFIG_FILE TEST_NAMESPACE OPERATOR_NAMESPACE EXECUTOR_IMAGE RUNNER_IMAGE; do
	eval "value=\${$value_name}"
	[ -n "$value" ] || fail "$value_name is required"
done
[ -f "$KUBECONFIG_FILE" ] || fail "E2E_KUBECONFIG does not name a file"
for image in "$EXECUTOR_IMAGE" "$RUNNER_IMAGE"; do
	is_pinned_image "$image" ||
		fail "reference-data phase images must be pinned by a lowercase SHA-256 digest: $image"
done
printf '%s\n' "$TIMEOUT_SECONDS" | grep -Eq '^[1-9][0-9]*$' ||
	fail "E2E_TIMEOUT_SECONDS must be a positive integer"

k() {
	kubectl --kubeconfig "$KUBECONFIG_FILE" "$@"
}

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-reference-data-e2e.XXXXXX")
chmod 700 "$WORK_DIR"
umask 077
RESOURCE_FILE=$WORK_DIR/resource.json
SECRET_FILE=$WORK_DIR/reference-secret.json
LOG_FILE=$WORK_DIR/logs.txt
CREDENTIAL_PATTERNS_FILE=$WORK_DIR/credential-patterns.txt
REFERENCE_DB_PASSWORD_FILE=$WORK_DIR/reference-database.password
REFERENCE_DB_URL_FILE=$WORK_DIR/reference-database.url
ADMISSION_ERROR_FILE=$WORK_DIR/admission-error.txt
STATUS_FILE=$WORK_DIR/reference-status.json
PLAN_FILE=$WORK_DIR/reference-plan.json
APPROVAL_FILE=$WORK_DIR/reference-approval.json
EVENTS_FILE=$WORK_DIR/reference-events.json
ROW_PATTERNS_FILE=$WORK_DIR/declared-rows.txt
REFERENCE_PLAN=

PHASE_COMPLETED=0
cleanup() {
	status=$?
	[ "$status" -ne 0 ] || [ "$PHASE_COMPLETED" -eq 1 ] || status=1
	if [ -n "${PHASE_REASON_MARKER:-}" ]; then
		if [ "$status" -ne 0 ] && [ ! -f "$PHASE_REASON_MARKER" ]; then
			printf 'e2e reference data: exited with status %s at a command that failed under set -e; no proof reported a reason\n' \
				"$status" >&2
		fi
		rm -f -- "$PHASE_REASON_MARKER"
	fi
	if [ "$status" -ne 0 ]; then
		printf 'e2e reference data: collecting failure diagnostics\n' >&2
		# The status of a schema carries no rows and no SQL by contract, which
		# is the refusal this phase exists to measure, so it is safe to print and
		# it is the one artifact that explains what the controller decided. Job
		# logs are not printed: a credential-isolation failure would put a
		# database URL in them, and a data statement would put a row in them.
		k -n "$TEST_NAMESPACE" get ptahschemas -o json 2>/dev/null |
			jq '.items[] | {name: .metadata.name, status: .status}' >&2 || true
		k -n "$TEST_NAMESPACE" get jobs \
			-l app.kubernetes.io/component=schema-operation \
			-o json 2>/dev/null |
			jq '[.items[] | {name: .metadata.name, labels: .metadata.labels, status: .status}]' >&2 || true
		printf 'e2e reference data: raw Job logs are suppressed to protect credential-isolation failures\n' >&2
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
# adds per engine is a database of its own, so a declared row set meets tables
# that do not exist yet.
REGISTRY_AUTH_SECRET=e2e-registry-auth
REGISTRY_PULL_SECRET=e2e-registry-pull
REGISTRY_HOST="${REGISTRY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local:5000"
REFERENCE_DATABASE=ptah_e2e_reference
REFERENCE_POLICY=e2e-reference-verification-policy
REFERENCE_POLICY_KEY=policy.yaml
REFERENCE_POLICY_FILE="$ROOT_DIR/testdata/e2e/verification-policy.yaml"
DATABASE_USER=ptah_e2e
[ -f "$REFERENCE_POLICY_FILE" ] || fail "verification policy fixture is missing"
[ "$REFERENCE_DATABASE" != ptah_e2e ] ||
	fail "the reference-data proof must own a database no other phase touched"
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

# The reference-data view is proved from the same build a user installs, against
# the live resource, rather than from a golden file.
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

# The database this proof reconciles is created on the server the data plane
# already runs, and it is one this suite has not touched before: "works on first
# creation of a database, when the target tables do not exist yet" is a scope
# line rather than a hope only if the tables really do not exist.
create_reference_database() {
	case "$ENGINE" in
	postgresql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		existing=$(k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atqc "$1"' \
			sh "SELECT count(*) FROM pg_database WHERE datname='${REFERENCE_DATABASE}'" |
			tr -d '[:space:]')
		;;
	mysql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		existing=$(k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -Nse "$1"' \
			sh "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='${REFERENCE_DATABASE}'" |
			tr -d '[:space:]')
		;;
	esac
	[ "$existing" = 0 ] ||
		fail "database $REFERENCE_DATABASE already exists on $ENGINE; the reference-data proof needs a database with no tables, so drop it before running this phase again"
	case "$ENGINE" in
	postgresql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -qc "$1"' \
			sh "CREATE DATABASE ${REFERENCE_DATABASE}" >/dev/null ||
			fail "database $REFERENCE_DATABASE could not be created"
		;;
	mysql)
		# The unprivileged user the operation Pod connects as owns nothing by
		# default, and MySQL grants are per schema: the grant is part of
		# creating the database rather than a separate setup step.
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot -e "$1"' \
			sh "CREATE DATABASE ${REFERENCE_DATABASE}; GRANT ALL PRIVILEGES ON ${REFERENCE_DATABASE}.* TO '${DATABASE_USER}'@'%'; FLUSH PRIVILEGES" >/dev/null ||
			fail "database $REFERENCE_DATABASE could not be created"
		;;
	esac

	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$REFERENCE_DB_SECRET" \
		--arg username "$DATABASE_USER" \
		--rawfile password "$REFERENCE_DB_PASSWORD_FILE" \
		--arg database "$REFERENCE_DATABASE" \
		--rawfile url "$REFERENCE_DB_URL_FILE" '
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

reference_query() {
	case "$ENGINE" in
	postgresql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -Atqc "$2"' \
			sh "$REFERENCE_DATABASE" "$1" | tr -d '[:space:]'
		;;
	mysql)
		# shellcheck disable=SC2016 # Variables expand inside the database container.
		k -n "$TEST_NAMESPACE" exec deployment/"$DATABASE_SERVICE" -- \
			sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot "$1" -Nse "$2"' \
			sh "$REFERENCE_DATABASE" "$1" | tr -d '[:space:]'
		;;
	esac
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
	*) fail "unsupported reference-data engine $ENGINE" ;;
	esac
	REFERENCE_DB_SECRET="e2e-${ENGINE}-reference-db"
	REFERENCE_SCHEMA="e2e-reference-${ENGINE}"
	REFERENCE_APPROVAL="e2e-reference-${ENGINE}-approval"
	REFERENCE_COORDINATION_KEY="e2e/reference/${ENGINE}"
	REFERENCE_ARTIFACT="oci://${REGISTRY_HOST}/schemas/reference-${ENGINE}:stable"

	# The password is read back from the Secret the data plane created rather
	# than derived a second time here. A second derivation is a second
	# definition, and the two would part company the moment either moved.
	k -n "$TEST_NAMESPACE" get secret "$DATABASE_SOURCE_SECRET" \
		-o jsonpath='{.data.password}' |
		tr -d '\n' | base64 -d >"$REFERENCE_DB_PASSWORD_FILE" ||
		fail "the data plane $ENGINE Secret $DATABASE_SOURCE_SECRET could not be read"
	chmod 600 "$REFERENCE_DB_PASSWORD_FILE"
	[ -s "$REFERENCE_DB_PASSWORD_FILE" ] ||
		fail "the data plane $ENGINE Secret carries no password"
	engine_password=$(cat "$REFERENCE_DB_PASSWORD_FILE")
	engine_authority="${DATABASE_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local"
	case "$ENGINE" in
	postgresql)
		engine_url="postgres://${DATABASE_USER}:${engine_password}@${engine_authority}:5432/${REFERENCE_DATABASE}?sslmode=disable"
		;;
	mysql)
		engine_url="mysql://${DATABASE_USER}:${engine_password}@tcp(${engine_authority}:3306)/${REFERENCE_DATABASE}"
		;;
	esac
	printf '%s' "$engine_url" >"$REFERENCE_DB_URL_FILE"
	chmod 600 "$REFERENCE_DB_URL_FILE"
	printf '%s\n' "$engine_password" "$engine_url" >>"$CREDENTIAL_PATTERNS_FILE"
	engine_password=
	engine_url=
}

reference_status() {
	k -n "$TEST_NAMESPACE" get ptahschema "$REFERENCE_SCHEMA" -o json >"$STATUS_FILE" ||
		fail "$REFERENCE_SCHEMA could not be read"
	scan_for_credentials "$STATUS_FILE" "$REFERENCE_SCHEMA status"
	scan_for_rows "$STATUS_FILE" "$REFERENCE_SCHEMA status"
}

reference_phase() {
	k -n "$TEST_NAMESPACE" get ptahschema "$REFERENCE_SCHEMA" \
		-o jsonpath='{.status.phase}' 2>/dev/null || true
}

wait_for_reference_phase() {
	wait_phase=$1
	wait_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$wait_deadline" ]; do
		observed_phase=$(reference_phase)
		case "$observed_phase" in
		"$wait_phase") return 0 ;;
		Failed)
			[ "$wait_phase" = Failed ] ||
				fail "$REFERENCE_SCHEMA failed while waiting for $wait_phase"
			;;
		esac
		sleep 5
	done
	fail "$REFERENCE_SCHEMA did not reach $wait_phase within ${TIMEOUT_SECONDS}s; it is in ${observed_phase:-<none>}"
}

# scan_for_rows is the refusal this phase exists to measure: "table rows never
# reach status, Events, or ordinary logs". The values are the declared ones, so
# a leak is a literal match rather than a shape to recognize.
scan_for_rows() {
	row_scan_file=$1
	row_scan_description=$2
	[ -s "$ROW_PATTERNS_FILE" ] || fail "row scanner has no declared values to look for"
	grep -c '^$' "$ROW_PATTERNS_FILE" | grep -qx 0 ||
		fail "row scanner has an empty declared value"
	[ -s "$row_scan_file" ] || return 0
	if grep -F -f "$ROW_PATTERNS_FILE" "$row_scan_file" >/dev/null; then
		grep -F -f "$ROW_PATTERNS_FILE" "$row_scan_file" | head -3 >&2 || true
		fail "$row_scan_description carries a declared row value"
	else
		row_scan_status=$?
		[ "$row_scan_status" -eq 1 ] ||
			fail "row scanner failed closed while checking $row_scan_description"
	fi
}

# The values the row scanner looks for come from the fixtures rather than from a
# list here, so a fixture that gains a row cannot leave the scanner behind. Only
# the names are used: a two-letter code would match base64 and turn the scanner
# into one that fires on everything.
collect_declared_row_values() {
	: >"$ROW_PATTERNS_FILE"
	chmod 600 "$ROW_PATTERNS_FILE"
	for row_file in "$ROOT_DIR"/testdata/e2e/reference/*/[a-z]*.yaml; do
		[ -f "$row_file" ] || continue
		sed -n 's/^[[:space:]]*name:[[:space:]]*//p' "$row_file"
	done | sed 's/[[:space:]]*$//' | sort -u | grep -v '^$' >>"$ROW_PATTERNS_FILE"
	[ "$(grep -c '' "$ROW_PATTERNS_FILE")" -ge 5 ] ||
		fail "the declared row fixtures yielded too few values for the row scanner to mean anything"
}

create_reference_policy() {
	k -n "$TEST_NAMESPACE" create configmap "$REFERENCE_POLICY" \
		--from-file="${REFERENCE_POLICY_KEY}=${REFERENCE_POLICY_FILE}" \
		--dry-run=client -o json | jq '.immutable = true' | k apply -f - >/dev/null
	grep -F 'application/vnd.stokaro.ptah.schema.v1' "$REFERENCE_POLICY_FILE" >/dev/null ||
		fail "the verification policy does not pin the schema artifact type"
}

# The artifact is published by the same Ptah the operator runs, from Go source
# and a YAML row file, because that is how a person declares reference data.
# The harness owns no artifact format of its own.
publish_reference_schema() {
	publish_revision=$1
	publish_directory="$ROOT_DIR/testdata/e2e/reference/$publish_revision"
	publish_configmap="e2e-reference-${ENGINE}-${publish_revision}"
	publish_job="e2e-push-reference-${ENGINE}-${publish_revision}"
	[ -d "$publish_directory" ] || fail "reference-data fixtures are missing: $publish_directory"
	printf 'e2e reference data: publishing the %s declared schema and rows as %s\n' \
		"$ENGINE_KIND" "$publish_revision" >&2
	k -n "$TEST_NAMESPACE" create configmap "$publish_configmap" \
		--from-file="$publish_directory" >/dev/null
	# A ConfigMap volume is not a directory of files: the kubelet writes the keys
	# into a timestamped directory and leaves one symlink per key beside it, so a
	# walker that descends finds every file twice. A subPath mount per file gives
	# the plain directory Ptah's Go parser expects, and the list comes from the
	# fixtures so it cannot fall behind them.
	publish_mounts=$(find "$publish_directory" -maxdepth 1 -type f \
		\( -name '*.go' -o -name '*.yaml' \) -exec basename {} \; | LC_ALL=C sort | jq -R . | jq -s .)
	[ "$(printf '%s' "$publish_mounts" | jq 'length')" -gt 0 ] ||
		fail "no reference-data files to publish from $publish_directory"
	jq -n \
		--argjson mounts "$publish_mounts" \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$publish_job" \
		--arg image "$EXECUTOR_IMAGE" \
		--arg configMap "$publish_configmap" \
		--arg reference "$REFERENCE_ARTIFACT" \
		--arg version "$publish_revision" \
		--arg registryAuthSecret "$REGISTRY_AUTH_SECRET" \
		--arg registryPullSecret "$REGISTRY_PULL_SECRET" '
      def registrySecretEnv($name; $key):
        {name: $name, valueFrom: {secretKeyRef: {name: $registryAuthSecret, key: $key}}};
    {
      apiVersion: "batch/v1", kind: "Job",
      metadata: {
        namespace: $namespace, name: $name,
        labels: {"app.kubernetes.io/component": "e2e-reference-publisher"}
      },
      spec: {
        backoffLimit: 0, activeDeadlineSeconds: 300, ttlSecondsAfterFinished: 600,
        template: {
          metadata: {labels: {"app.kubernetes.io/component": "e2e-reference-publisher"}},
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
                "schema", "push", $reference, "--root-dir", "/schema",
                "--version", $version, "--plain-http"
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
              volumeMounts: ([{name: "work", mountPath: "/work"}] + [
                $mounts[] | {
                  name: "schema", mountPath: ("/schema/" + .), subPath: ., readOnly: true
                }
              ])
            }],
            volumes: [
              {name: "schema", configMap: {name: $configMap}},
              {name: "work", emptyDir: {sizeLimit: "64Mi"}}
            ]
          }
        }
      }
    }' | k create -f - >/dev/null
	publish_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$publish_deadline" ]; do
		publish_state=$(k -n "$TEST_NAMESPACE" get job "$publish_job" -o json 2>/dev/null |
			jq -r '
              if ((.status.conditions // []) | any(.type == "Complete" and .status == "True"))
              then "complete"
              elif ((.status.conditions // []) | any(.type == "Failed" and .status == "True"))
              then "failed"
              else "running" end
            ')
		case "$publish_state" in
		complete) return 0 ;;
		failed) fail "publishing the $ENGINE_KIND reference-data artifact $publish_revision failed" ;;
		esac
		sleep 5
	done
	fail "the $ENGINE_KIND reference-data artifact $publish_revision was not published within ${TIMEOUT_SECONDS}s"
}

create_reference_resource() {
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$REFERENCE_SCHEMA" \
		--arg engine "$ENGINE_KIND" \
		--arg secret "$REFERENCE_DB_SECRET" \
		--arg coordinationKey "$REFERENCE_COORDINATION_KEY" \
		--arg reference "$REFERENCE_ARTIFACT" \
		--arg policy "$REFERENCE_POLICY" \
		--arg policyKey "$REFERENCE_POLICY_KEY" \
		--arg interval "$INTERVAL" \
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
          ociRef: $reference,
          registryAuthFrom: {
            name: $registryAuthSecret, mode: "Environment",
            usernameKey: "username", passwordKey: "password", registryKey: "registry"
          },
          verificationPolicyFrom: {name: $policy, key: $policyKey},
          transport: {plainHTTP: true}
        },
        interval: $interval,
        policy: {apply: "OnApproval", allowDestructive: true},
        execution: {activeDeadlineSeconds: 600}
      }
    }' >"$RESOURCE_FILE"
	k create -f "$RESOURCE_FILE" >/dev/null
}

# approve_reference_plan writes the approval the current plan needs, with every
# binding taken from the plan itself. The webhook hydrates and re-derives them,
# so an approval that named its own values would be refused.
approve_reference_plan() {
	approval_name=$1
	approval_plan=$2
	k -n "$TEST_NAMESPACE" get ptahschemaplan "$approval_plan" -o json >"$PLAN_FILE" ||
		fail "plan $approval_plan could not be read"
	jq -n \
		--arg namespace "$TEST_NAMESPACE" \
		--arg name "$approval_name" \
		--arg schema "$REFERENCE_SCHEMA" \
		--slurpfile plan "$PLAN_FILE" '
    {
      apiVersion: "operator.ptah.run/v1alpha1", kind: "PtahSchemaApproval",
      metadata: {namespace: $namespace, name: $name},
      spec: {
        schemaRef: {name: $schema, uid: $plan[0].spec.schemaRef.uid},
        planRef: {name: $plan[0].metadata.name, uid: $plan[0].metadata.uid},
        planFingerprint: $plan[0].spec.fingerprint
      }
    }' >"$APPROVAL_FILE"
	k create -f "$APPROVAL_FILE" >/dev/null 2>"$ADMISSION_ERROR_FILE" || return 1
	return 0
}

current_reference_plan() {
	k -n "$TEST_NAMESPACE" get ptahschema "$REFERENCE_SCHEMA" \
		-o jsonpath='{.status.plan.name}' 2>/dev/null || true
}

wait_for_reference_plan() {
	plan_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$plan_deadline" ]; do
		REFERENCE_PLAN=$(current_reference_plan)
		if [ -n "$REFERENCE_PLAN" ]; then
			return 0
		fi
		sleep 5
	done
	fail "$REFERENCE_SCHEMA published no plan within ${TIMEOUT_SECONDS}s"
}

# assert_declared_rows reads the managed tables back through the database rather
# than through the operator, which is the only reading that can say the rows were
# applied rather than reported. An empty expected name means the child table has
# no declared rows yet.
assert_declared_rows() {
	expected_regions=$1
	expected_countries=$2
	# reference_query strips whitespace, because the two clients print a value
	# with different padding. The expectation is folded the same way rather than
	# written pre-folded: a caller should be able to write the name it declared.
	expected_czechia=$(printf '%s' "$3" | tr -d '[:space:]')
	observed_regions=$(reference_query "SELECT count(*) FROM regions")
	observed_countries=$(reference_query "SELECT count(*) FROM countries")
	observed_czechia=$(reference_query "SELECT name FROM countries WHERE code = 'CZ'")
	[ "$observed_regions" = "$expected_regions" ] ||
		fail "regions holds $observed_regions rows, want $expected_regions"
	[ "$observed_countries" = "$expected_countries" ] ||
		fail "countries holds $observed_countries rows, want $expected_countries"
	[ "$observed_czechia" = "$expected_czechia" ] ||
		fail "countries.CZ is $observed_czechia, want $expected_czechia"
	# The foreign key is part of the declaration, so a row set that satisfied
	# the counts while pointing at nothing would still be wrong.
	orphans=$(reference_query "SELECT count(*) FROM countries c LEFT JOIN regions r ON c.region_code = r.code WHERE r.code IS NULL")
	[ "$orphans" = 0 ] ||
		fail "$orphans countries reference a region that does not exist"
}

# A repeated reconciliation with no changes issues no DML. The evidence is the
# resource settling back into InSync with no plan, plus the rows being untouched.
#
# Pending belongs in the allowed set because this proof puts it there. The spec
# patch below bumps the generation, the generation is part of the operation
# input fingerprint, and an operation already in flight when the patch lands is
# discarded into Pending. Demanding its absence would fail on the poll that
# caught the harness changing the spec, and report it as the operator planning
# something.
#
# The phases that would mean a plan appeared stay out: ReadyToApply,
# AwaitingApproval and Applying each say the operator decided there was work.
assert_repeated_reconciliation_changes_nothing() {
	before_rows=$(reference_query "SELECT count(*) FROM countries")
	k -n "$TEST_NAMESPACE" patch ptahschema "$REFERENCE_SCHEMA" --type=merge \
		--patch '{"spec":{"interval":"20s"}}' >/dev/null
	repeat_deadline=$(($(date +%s) + 90))
	while [ "$(date +%s)" -lt "$repeat_deadline" ]; do
		reference_status
		jq -e '.status.phase == "InSync" or .status.phase == "Observing" or .status.phase == "Planning" or .status.phase == "VerifyingConvergence" or .status.phase == "Resolving" or .status.phase == "Verifying" or .status.phase == "Pending"' \
			"$STATUS_FILE" >/dev/null ||
			fail "$REFERENCE_SCHEMA left the converged cycle while nothing had changed"
		sleep 10
	done
	wait_for_reference_phase InSync
	after_rows=$(reference_query "SELECT count(*) FROM countries")
	[ "$before_rows" = "$after_rows" ] ||
		fail "a reconciliation with no declared change rewrote the managed rows"
	k -n "$TEST_NAMESPACE" patch ptahschema "$REFERENCE_SCHEMA" --type=merge \
		--patch "{\"spec\":{\"interval\":\"$INTERVAL\"}}" >/dev/null
}

# A change to reference rows alone triggers planning: no DDL change is not a
# reason to skip data reconciliation.
#
# The second revision declares the child table's rows, which the first revision
# deliberately left undeclared: Ptah emits declared rows in an order that can
# put a child row before the parent it references (stokaro/ptah#3252), so the
# parent rows arrive first and the child rows meet a foreign key they satisfy.
# The tables themselves are unchanged, so this is still a plan with no DDL in
# it, which is the row of the matrix this proves.
assert_data_only_change_reconciles() {
	publish_reference_schema v2
	wait_for_reference_phase AwaitingApproval
	wait_for_reference_plan
	reference_status
	jq -e '.status.plan.statementCount >= 1' "$STATUS_FILE" >/dev/null ||
		fail "a data-only change produced a plan with no statements"
	approve_reference_plan "${REFERENCE_APPROVAL}-v2" "$REFERENCE_PLAN" ||
		fail "the data-only plan could not be approved: $(cat "$ADMISSION_ERROR_FILE")"
	wait_for_reference_phase InSync
	assert_declared_rows 2 3 "Czech Republic"
}

# assert_kubectl_ptah_schema_line holds the reference-data view to the drift
# this proof created by hand, and to the rule that a count is all it may say.
#
# The whole line is the argument rather than three numbers a format string here
# would arrange, because the renderer arranges it and this script cannot ask it
# how. Passing the finished line puts one literal in one place, which
# TestReferenceDataPhasePinsTheLineTheViewWrites compares with what the view
# actually writes, offline, on every pull request.
#
# Two claims, and the scanners carry the second. scan_for_rows looks for the
# values the fixtures declare, read out of the fixtures rather than listed here,
# so a view that started printing a row fails on the value it printed instead of
# passing a check written from memory.
#
# It waits rather than reads once. The phase says the operator has observed and
# planned; the view is a second read, and a poll that lands between them would
# report the harness. The wait is bounded, and its failure says what it wanted.
assert_kubectl_ptah_schema_line() {
	schema_view_want=$1
	schema_view_label=$2
	schema_view_file=$WORK_DIR/kubectl-ptah-schema-${ENGINE}-${schema_view_label}.txt
	schema_view_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$schema_view_deadline" ]; do
		"$KUBECTL_PTAH_BINARY" schema "$REFERENCE_SCHEMA" \
			--kubeconfig "$KUBECONFIG_FILE" -n "$TEST_NAMESPACE" >"$schema_view_file" ||
			fail "kubectl ptah schema could not read $REFERENCE_SCHEMA"
		scan_for_credentials "$schema_view_file" "the kubectl ptah schema view"
		scan_for_rows "$schema_view_file" "the kubectl ptah schema view"
		grep -F "Schema:           ${TEST_NAMESPACE}/${REFERENCE_SCHEMA}" "$schema_view_file" >/dev/null ||
			fail "kubectl ptah schema does not name the resource it read"
		if grep -Fx "$schema_view_want" "$schema_view_file" >/dev/null; then
			return 0
		fi
		sleep 5
	done
	printf 'e2e reference data: the view said:\n' >&2
	sed 's/^/e2e reference data:   /' "$schema_view_file" >&2
	fail "kubectl ptah schema never reported [$schema_view_want] within ${TIMEOUT_SECONDS}s"
}

# A change made after approval is never silently overwritten by a stale plan.
# The row is edited in the database between the plan and the approval, so the
# approval names a plan whose observed state no longer holds.
assert_external_edit_refuses_a_stale_approval() {
	reference_query "UPDATE countries SET name = 'Edited outside the operator' WHERE code = 'US'" >/dev/null ||
		fail "the external edit could not be made"
	wait_for_reference_phase AwaitingApproval
	wait_for_reference_plan
	# One managed row differs, and this proof is what made it differ, so the
	# counts are known rather than read back from the thing under test.
	assert_kubectl_ptah_schema_line \
		"Reference data:   0 to insert, 1 to update, 0 to delete" external-edit
	stale_plan=$REFERENCE_PLAN
	reference_query "UPDATE countries SET name = 'Edited again outside the operator' WHERE code = 'US'" >/dev/null ||
		fail "the second external edit could not be made"
	# The operator observes the second edit and publishes a plan for it. The
	# approval below still names the first one, which is the stale decision.
	stale_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$stale_deadline" ]; do
		wait_for_reference_plan
		[ "$REFERENCE_PLAN" = "$stale_plan" ] || break
		sleep 5
	done
	[ "$REFERENCE_PLAN" != "$stale_plan" ] ||
		fail "the second external edit produced no new plan"
	if approve_reference_plan "${REFERENCE_APPROVAL}-stale" "$stale_plan"; then
		fail "an approval naming the replaced plan was accepted"
	fi
	# Which refusal fires depends on where the resource sits in its cycle when
	# the approval lands: the plan is no longer current, the schema is not
	# awaiting approval, or it has an operation in flight. All three are the
	# refusal this row is about, and only the first says "plan", so pinning that
	# spelling would make the proof about the timing instead.
	grep -Ei 'plan|approval|schema' "$ADMISSION_ERROR_FILE" >/dev/null ||
		fail "the refusal of a replaced-plan approval did not say what it refused"
	scan_for_credentials "$ADMISSION_ERROR_FILE" "the stale approval refusal"
	scan_for_rows "$ADMISSION_ERROR_FILE" "the stale approval refusal"
	# The current plan is approvable, and applying it puts the declared value
	# back: an external edit is drift, not a new declaration.
	approve_reference_plan "${REFERENCE_APPROVAL}-recovered" "$REFERENCE_PLAN" ||
		fail "the current plan could not be approved: $(cat "$ADMISSION_ERROR_FILE")"
	wait_for_reference_phase InSync
	restored=$(reference_query "SELECT name FROM countries WHERE code = 'US'")
	expected_restored=$(printf '%s' "United States" | tr -d '[:space:]')
	[ "$restored" = "$expected_restored" ] ||
		fail "the declared value was not restored after the external edit: $restored"
}

# A removed declaration ends management; it does not delete rows.
assert_removed_declaration_keeps_rows() {
	publish_reference_schema v3
	removal_deadline=$(deadline_from_now)
	while [ "$(date +%s)" -lt "$removal_deadline" ]; do
		observed_phase=$(reference_phase)
		case "$observed_phase" in
		InSync | AwaitingApproval) break ;;
		esac
		sleep 5
	done
	if [ "$(reference_phase)" = AwaitingApproval ]; then
		wait_for_reference_plan
		approve_reference_plan "${REFERENCE_APPROVAL}-v3" "$REFERENCE_PLAN" ||
			fail "the plan after the removed declaration could not be approved: $(cat "$ADMISSION_ERROR_FILE")"
	fi
	wait_for_reference_phase InSync
	remaining=$(reference_query "SELECT count(*) FROM countries")
	[ "$remaining" = 3 ] ||
		fail "removing the declaration changed the managed rows: countries holds $remaining, want 3"
}

# The refusal that has to hold through every step above: no declared row value
# reaches status, an Event, or the controller's own log.
assert_rows_never_left_the_database() {
	reference_status
	k -n "$TEST_NAMESPACE" get events -o json >"$EVENTS_FILE" ||
		fail "namespace Events could not be read"
	scan_for_credentials "$EVENTS_FILE" "namespace Events"
	scan_for_rows "$EVENTS_FILE" "namespace Events"
	k -n "$OPERATOR_NAMESPACE" logs -l app.kubernetes.io/component=controller \
		--tail=2000 >"$LOG_FILE" 2>/dev/null || true
	scan_for_credentials "$LOG_FILE" "the controller log"
	scan_for_rows "$LOG_FILE" "the controller log"
}

# run_engine_reference_data drives one engine from a database with no tables to
# a declared schema and declared rows the database agrees with, and proves each
# step on the way.
run_engine_reference_data() {
	select_engine "$1"
	printf 'e2e reference data: starting the %s lifecycle on a database with no tables\n' \
		"$ENGINE_KIND" >&2
	create_reference_database
	publish_reference_schema v1
	create_reference_resource

	wait_for_reference_phase AwaitingApproval
	wait_for_reference_plan
	approve_reference_plan "$REFERENCE_APPROVAL" "$REFERENCE_PLAN" ||
		fail "the first plan could not be approved: $(cat "$ADMISSION_ERROR_FILE")"
	wait_for_reference_phase InSync
	assert_declared_rows 2 0 ""
	assert_repeated_reconciliation_changes_nothing
	assert_data_only_change_reconciles
	assert_external_edit_refuses_a_stale_approval
	assert_removed_declaration_keeps_rows
	assert_rows_never_left_the_database
	printf 'e2e reference data: PASS %s declared rows, data-only change, stale approval, and ended management\n' \
		"$ENGINE_KIND" >&2
}

collect_declared_row_values
create_reference_policy
run_engine_reference_data postgresql
run_engine_reference_data mysql

PHASE_COMPLETED=1
printf '%s\n' 'e2e reference data: PASS declared rows on both engines, with no row value in status, Events, or logs'
