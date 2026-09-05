#!/bin/sh

set -eu

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
PREDECESSOR_IDENTITY_FILE=$ROOT_DIR/internal/crdupgrade/assets/predecessor.json

DOCKER_CONTEXT=${DOCKER_CONTEXT:-remote-dev-container}
K8S_VERSION=${K8S_VERSION:-}
KIND_NODE_IMAGE=${KIND_NODE_IMAGE:-}
E2E_EXECUTOR_IMAGE=${E2E_EXECUTOR_IMAGE:-}
E2E_RUNNER_IMAGE=${E2E_RUNNER_IMAGE:-}
E2E_PTAH_VERSION=${E2E_PTAH_VERSION:-}
E2E_PTAH_SOURCE_DIR=${E2E_PTAH_SOURCE_DIR:-}
E2E_PTAH_REVISION=${E2E_PTAH_REVISION:-00fc362c943bfb9d0363d5890bf449a2a9b5e7cf}
E2E_PTAH_GIT_URL=${E2E_PTAH_GIT_URL:-https://github.com/stokaro/ptah.git}
E2E_REGISTRY_IMAGE=${E2E_REGISTRY_IMAGE:-registry:3@sha256:1be55279f18a2fe1a74edf2664cac61c1bea305b7b4642dab412e7affdcb3e33}
E2E_POSTGRES_SOURCE_IMAGE=${E2E_POSTGRES_SOURCE_IMAGE:-postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73}
E2E_MYSQL_SOURCE_IMAGE=${E2E_MYSQL_SOURCE_IMAGE:-mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb}
E2E_RUN_ID=${E2E_RUN_ID:-}
E2E_API_SERVER_PORT=${E2E_API_SERVER_PORT:-}
E2E_REGISTRY_PORT=${E2E_REGISTRY_PORT:-}
E2E_SSH_TARGET=${E2E_SSH_TARGET:-}
E2E_SSH_PORT=${E2E_SSH_PORT:-}
E2E_DIRECT_HOST_ACCESS=${E2E_DIRECT_HOST_ACCESS:-0}
E2E_RELEASE_CHART_OUTPUT=${E2E_RELEASE_CHART_OUTPUT:-}

# An imported variable retains its export attribute after reassignment in POSIX
# shells. Clear secret-bearing names before generating task credentials so no
# later host subprocess can inherit their values.
unset REGISTRY_PASSWORD EXTERNAL_PG_ADMIN_PASSWORD EXTERNAL_PG_PASSWORD EXTERNAL_PG_URL

fail() {
	printf 'e2e: %s\n' "$*" >&2
	exit 1
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || fail "required command is not installed: $1"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum | awk '{print $1}'
		return
	fi
	shasum -a 256 | awk '{print $1}'
}

dns_name() {
	prefix=$1
	identity=$2
	max_length=${3:-63}
	stem_length=$((max_length - 11))
	slug=$(printf '%s' "$identity" |
		tr '[:upper:]' '[:lower:]' |
		sed -e 's/[^a-z0-9-]/-/g' -e 's/--*/-/g' -e 's/^-//' -e 's/-$//')
	[ -n "$slug" ] || slug=run
	suffix=$(printf '%s' "$identity" | sha256 | cut -c1-10)
	stem=$(printf '%s-%s' "$prefix" "$slug" | cut -c1-"$stem_length" | sed 's/-$//')
	printf '%s-%s\n' "$stem" "$suffix"
}

is_pinned_image() {
	printf '%s\n' "$1" | grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$'
}

for command_name in docker kind kubectl helm jq ssh git go tar awk sed grep tr cut cksum cmp cp ln mktemp date sleep curl htpasswd openssl mv; do
	require_command "$command_name"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	fail "sha256sum or shasum is required"
fi

[ -f "$PREDECESSOR_IDENTITY_FILE" ] ||
	fail "predecessor identity fixture is missing: $PREDECESSOR_IDENTITY_FILE"
PREDECESSOR_REVISION=$(jq -er '.revision' "$PREDECESSOR_IDENTITY_FILE")
PREDECESSOR_DOCKERFILE=$(jq -er '.dockerfile' "$PREDECESSOR_IDENTITY_FILE")
PREDECESSOR_CHART=$(jq -er '.chart' "$PREDECESSOR_IDENTITY_FILE")
printf '%s\n' "$PREDECESSOR_REVISION" | grep -Eq '^[0-9a-f]{40}$' ||
	fail "predecessor revision must be an exact 40-character lowercase Git commit"
case "$PREDECESSOR_DOCKERFILE" in
	/* | ../* | */../* | */..) fail "predecessor Dockerfile path must stay inside its archive" ;;
esac
case "$PREDECESSOR_CHART" in
	/* | ../* | */../* | */..) fail "predecessor chart path must stay inside its archive" ;;
esac
resolved_predecessor=$(git -C "$ROOT_DIR" rev-parse --verify "${PREDECESSOR_REVISION}^{commit}" 2>/dev/null) ||
	fail "exact predecessor commit $PREDECESSOR_REVISION is unavailable; fetch repository history"
[ "$resolved_predecessor" = "$PREDECESSOR_REVISION" ] ||
	fail "predecessor revision resolved to $resolved_predecessor, expected $PREDECESSOR_REVISION"

[ -n "$K8S_VERSION" ] || fail "K8S_VERSION is required (for example, 1.37.0)"
printf '%s\n' "$K8S_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||
	fail "K8S_VERSION must be an exact major.minor.patch version"
K8S_MAJOR_MINOR=$(printf '%s\n' "$K8S_VERSION" | cut -d. -f1,2)
case "$K8S_MAJOR_MINOR" in
1.35 | 1.36 | 1.37) ;;
*) fail "Kubernetes $K8S_MAJOR_MINOR is outside the supported 1.35-1.37 window" ;;
esac
printf '%s\n' "$E2E_PTAH_REVISION" | grep -Eq '^[0-9a-f]{40}$' ||
	fail "E2E_PTAH_REVISION must be an exact 40-character lowercase Git commit"
if [ -z "$KIND_NODE_IMAGE" ]; then
	KIND_NODE_IMAGE=$(jq -r --arg version "v${K8S_VERSION}" '
    [.releases[].nodeImage | select(startswith("kindest/node:" + $version + "@sha256:"))][0] // empty
  ' "$ROOT_DIR/support/kubernetes.json")
fi
[ -n "$KIND_NODE_IMAGE" ] ||
	fail "KIND_NODE_IMAGE is required when K8S_VERSION is outside the support manifest"
is_pinned_image "$KIND_NODE_IMAGE" ||
	fail "KIND_NODE_IMAGE must be pinned with @sha256:<64 lowercase hex>"
case "$KIND_NODE_IMAGE" in
	kindest/node:v"$K8S_VERSION"@sha256:*) ;;
	*) fail "KIND_NODE_IMAGE version does not match K8S_VERSION $K8S_VERSION" ;;
esac

EXPECTED_KIND_VERSION=$(jq -r '.kindVersion // empty' "$ROOT_DIR/support/kubernetes.json")
[ -n "$EXPECTED_KIND_VERSION" ] || fail "support manifest does not declare kindVersion"
ACTUAL_KIND_VERSION=$(kind version | awk '{print $2}')
[ "$ACTUAL_KIND_VERSION" = "$EXPECTED_KIND_VERSION" ] ||
	fail "kind $EXPECTED_KIND_VERSION is required, got $ACTUAL_KIND_VERSION"

if [ -n "$E2E_EXECUTOR_IMAGE" ]; then
	is_pinned_image "$E2E_EXECUTOR_IMAGE" ||
		fail "E2E_EXECUTOR_IMAGE must be pinned with @sha256:<64 lowercase hex> when provided"
fi
if [ -n "$E2E_RUNNER_IMAGE" ]; then
	is_pinned_image "$E2E_RUNNER_IMAGE" ||
		fail "E2E_RUNNER_IMAGE must be pinned with @sha256:<64 lowercase hex> when provided"
fi
CONTROLLER_REVISION=$(git -C "$ROOT_DIR" rev-parse HEAD)
printf '%s\n' "$CONTROLLER_REVISION" | grep -Eq '^[0-9a-f]{40}$' ||
	fail "operator source revision must be an exact 40-character lowercase Git commit"
for source_image in "$E2E_REGISTRY_IMAGE" "$E2E_POSTGRES_SOURCE_IMAGE" "$E2E_MYSQL_SOURCE_IMAGE"; do
	is_pinned_image "$source_image" ||
		fail "registry and database source images must be pinned by digest: $source_image"
done

case "$DOCKER_CONTEXT" in
	default | orbstack)
		fail "Docker context $DOCKER_CONTEXT is not allowed; use an explicit remote context"
	;;
	'')
		fail "DOCKER_CONTEXT must name an explicit remote context"
	;;
esac

docker --context "$DOCKER_CONTEXT" context inspect "$DOCKER_CONTEXT" >/dev/null
DOCKER_ENDPOINT=$(docker --context "$DOCKER_CONTEXT" context inspect \
	--format '{{ (index .Endpoints "docker").Host }}' "$DOCKER_CONTEXT")
case "$DOCKER_ENDPOINT" in
	ssh://*) ;;
	*) fail "Docker context $DOCKER_CONTEXT must use an SSH endpoint, got $DOCKER_ENDPOINT" ;;
esac
docker --context "$DOCKER_CONTEXT" info >/dev/null
BUILDX_PLUGIN_PATH=$(docker --context "$DOCKER_CONTEXT" info \
	--format '{{range .ClientInfo.Plugins}}{{if eq .Name "buildx"}}{{.Path}}{{end}}{{end}}')
printf '%s\n' "$BUILDX_PLUGIN_PATH" | grep -Eq '^/[^[:cntrl:]]+$' ||
	fail "Docker Buildx plugin path must be absolute"
[ -x "$BUILDX_PLUGIN_PATH" ] ||
	fail "Docker Buildx plugin is not executable: $BUILDX_PLUGIN_PATH"
docker --context "$DOCKER_CONTEXT" buildx inspect "$DOCKER_CONTEXT" >/dev/null 2>&1 ||
	fail "Docker context $DOCKER_CONTEXT must expose a working Buildx builder"

ssh_authority=${DOCKER_ENDPOINT#ssh://}
ssh_authority=${ssh_authority%%/*}
if [ -z "$E2E_SSH_TARGET" ]; then
	case "$ssh_authority" in
		*:*:*)
			fail "set E2E_SSH_TARGET and E2E_SSH_PORT for an IPv6 or nonstandard SSH Docker endpoint"
		;;
		*:*)
			E2E_SSH_PORT=${E2E_SSH_PORT:-${ssh_authority##*:}}
			E2E_SSH_TARGET=${ssh_authority%:*}
		;;
		*) E2E_SSH_TARGET=$ssh_authority ;;
	esac
fi
[ -n "$E2E_SSH_TARGET" ] || fail "could not derive the SSH target from Docker context $DOCKER_CONTEXT"
case "$E2E_SSH_TARGET" in
	-* | *[![:graph:]]*) fail "E2E_SSH_TARGET contains unsupported characters" ;;
esac
if [ -n "$E2E_SSH_PORT" ]; then
	printf '%s\n' "$E2E_SSH_PORT" | grep -Eq '^[0-9]+$' || fail "E2E_SSH_PORT must be numeric"
	if [ "$E2E_SSH_PORT" -lt 1 ] || [ "$E2E_SSH_PORT" -gt 65535 ]; then
		fail "E2E_SSH_PORT must be between 1 and 65535"
	fi
fi
case "$E2E_DIRECT_HOST_ACCESS" in
0 | 1) ;;
*) fail "E2E_DIRECT_HOST_ACCESS must be 0 or 1" ;;
esac
if [ "$E2E_DIRECT_HOST_ACCESS" -eq 1 ] && [ "${CI:-}" != true ]; then
	fail "E2E_DIRECT_HOST_ACCESS is reserved for an ephemeral CI host"
fi

if [ -z "$E2E_RUN_ID" ]; then
	git_revision=$(git -C "$ROOT_DIR" rev-parse --short=10 HEAD 2>/dev/null || printf 'worktree')
	E2E_RUN_ID="local-${git_revision}-$$"
fi
identity="${K8S_VERSION}-${E2E_RUN_ID}"
CLUSTER_NAME=$(dns_name ptah-e2e "$identity" 48)
OPERATOR_NAMESPACE=$(dns_name ptah-system "$identity")
CRD_PROOF_NAMESPACE=$(dns_name ptah-crd-proof "$identity")
# Force the runtime ReplicaSet-to-Pod generated-name truncation boundary in
# every supported-minor run, independently of the caller's run ID length.
RUNTIME_FULLNAME=$(dns_name ptah-runtime-generated-name-prefix-boundary-proof "$identity" 60)
[ "${#RUNTIME_FULLNAME}" -eq 60 ] || fail "runtime fullname boundary fixture must be exactly 60 characters"
MANAGER_PULL_SECRET=$(dns_name ptah-manager-pull "$identity")
HA_TEST_NAMESPACE=$(dns_name ptah-test-ha "$identity")
TEST_NAMESPACE=$(dns_name ptah-test-a "$identity")
FOREIGN_NAMESPACE=$(dns_name ptah-test-b "$identity")
# Leave room for the chart name and its longest resource suffix.
HELM_RELEASE=$(dns_name ptah "$identity" 35)
IMAGE_REPOSITORY=ptah-operator-e2e.local/ptah-operator
IMAGE_TAG=$(printf '%s' "$identity" | sha256 | cut -c1-16)
OPERATOR_IMAGE="${IMAGE_REPOSITORY}:${IMAGE_TAG}"
NEXT_IMAGE_REPOSITORY=ptah-operator-e2e.local/ptah-operator-next
NEXT_OPERATOR_IMAGE="${NEXT_IMAGE_REPOSITORY}:${IMAGE_TAG}"
PREDECESSOR_IMAGE_REPOSITORY=ptah-operator-e2e.local/ptah-operator-predecessor
PREDECESSOR_OPERATOR_IMAGE="${PREDECESSOR_IMAGE_REPOSITORY}:${IMAGE_TAG}"
FIXTURE_BUILD_IMAGE="ptah-operator-e2e.local/e2e-fixture:${IMAGE_TAG}"
PTAH_IMAGE="ptah-executor-e2e.local/ptah:${IMAGE_TAG}"
REGISTRY_CONTAINER=$(dns_name ptah-registry "$identity" 63)
REGISTRY_SERVICE=e2e-registry
REGISTRY_DNS_NAME="${REGISTRY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local"
REGISTRY_HOST="${REGISTRY_DNS_NAME}:5000"
EXTERNAL_PG_CONTAINER=$(dns_name ptah-postgresql-external "$identity" 63)
EXTERNAL_PG_SERVICE=e2e-postgresql-external
EXTERNAL_PG_DNS_NAME="${EXTERNAL_PG_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local"
EXTERNAL_PG_ADMIN_USER=postgres
EXTERNAL_PG_ADMIN_DATABASE=postgres
EXTERNAL_PG_USER=ptah_external
EXTERNAL_PG_DATABASE=ptah_external
TLS_PROXY_SERVICE=e2e-registry-tls
TLS_PROXY_DNS_NAME="${TLS_PROXY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local"
REMOTE_REGISTRY=127.0.0.1
REGISTRY_USERNAME=ptah_e2e
registry_credential_suffix=$(printf '%s-registry-auth' "$identity" | sha256 | cut -c1-24)
REGISTRY_PASSWORD="e2eRegistry${registry_credential_suffix}Q7"
external_pg_credential_suffix=$(printf '%s-postgresql-external' "$identity" | sha256 | cut -c1-24)
EXTERNAL_PG_PASSWORD="e2eExternalPg${external_pg_credential_suffix}Q7"
external_pg_admin_credential_suffix=$(printf '%s-postgresql-admin' "$identity" | sha256 | cut -c1-24)
EXTERNAL_PG_ADMIN_PASSWORD="e2eExternalPgAdmin${external_pg_admin_credential_suffix}Q7"
[ "$EXTERNAL_PG_ADMIN_PASSWORD" != "$EXTERNAL_PG_PASSWORD" ] ||
	fail "external PostgreSQL admin and application credentials must differ"

if [ -z "$E2E_API_SERVER_PORT" ]; then
	port_seed=$(printf '%s' "$CLUSTER_NAME" | cksum | awk '{print $1}')
	E2E_API_SERVER_PORT=$((30000 + port_seed % 20000))
fi
printf '%s\n' "$E2E_API_SERVER_PORT" | grep -Eq '^[0-9]+$' || fail "E2E_API_SERVER_PORT must be numeric"
if [ "$E2E_API_SERVER_PORT" -lt 1024 ] || [ "$E2E_API_SERVER_PORT" -gt 65535 ]; then
	fail "E2E_API_SERVER_PORT must be between 1024 and 65535"
fi
if [ -z "$E2E_REGISTRY_PORT" ]; then
	registry_seed=$(printf '%s-registry' "$CLUSTER_NAME" | cksum | awk '{print $1}')
	E2E_REGISTRY_PORT=$((20000 + registry_seed % 9000))
fi
printf '%s\n' "$E2E_REGISTRY_PORT" | grep -Eq '^[0-9]+$' || fail "E2E_REGISTRY_PORT must be numeric"
if [ "$E2E_REGISTRY_PORT" -lt 1024 ] || [ "$E2E_REGISTRY_PORT" -gt 65535 ]; then
	fail "E2E_REGISTRY_PORT must be between 1024 and 65535"
fi
REMOTE_REGISTRY="127.0.0.1:${E2E_REGISTRY_PORT}"

export DOCKER_HOST="$DOCKER_ENDPOINT"
export KIND_EXPERIMENTAL_PROVIDER=docker
unset DOCKER_CERT_PATH DOCKER_TLS_VERIFY

if [ -n "$E2E_EXECUTOR_IMAGE" ] && [ -z "$E2E_PTAH_VERSION" ]; then
	fail "E2E_PTAH_VERSION is required when E2E_EXECUTOR_IMAGE supplies an external Ptah build"
fi

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-e2e.XXXXXX")
chmod 700 "$WORK_DIR"
KUBECONFIG_FILE=$WORK_DIR/kubeconfig
KIND_CONFIG=$WORK_DIR/kind.yaml
TUNNEL_LOG=$WORK_DIR/ssh-tunnel.log
REGISTRY_HOSTS_FILE=$WORK_DIR/registry-hosts.toml
REGISTRY_HTPASSWD_FILE=$WORK_DIR/registry.htpasswd
REGISTRY_NETRC_FILE=$WORK_DIR/registry.netrc
REGISTRY_PASSWORD_FILE=$WORK_DIR/registry.password
REGISTRY_CREDENTIALS_FILE=$WORK_DIR/registry-credentials.json
EXTERNAL_PG_ENV_FILE=$WORK_DIR/external-postgresql.env
EXTERNAL_PG_ADMIN_PASSWORD_FILE=$WORK_DIR/external-postgresql-admin.password
EXTERNAL_PG_PASSWORD_FILE=$WORK_DIR/external-postgresql.password
EXTERNAL_PG_CREDENTIALS_FILE=$WORK_DIR/external-postgresql-credentials.json
EXTERNAL_PG_BOOTSTRAP_SQL_FILE=$WORK_DIR/external-postgresql-bootstrap.sql
NODE_READINESS_FILE=$WORK_DIR/node-readiness.json
KIND_NODE_INVENTORY_FILE=$WORK_DIR/kind-node-inventory.txt
API_SERVER_ENDPOINT_INVENTORY_FILE=$WORK_DIR/api-server-endpoints.json
API_SERVER_ENDPOINT_ADDRESS_FILE=$WORK_DIR/api-server-endpoint-addresses.txt
TLS_PROXY_DIR=$WORK_DIR/tls-proxy
TLS_PROXY_CA_KEY_FILE=$TLS_PROXY_DIR/ca.key
TLS_PROXY_CA_FILE=$TLS_PROXY_DIR/ca.crt
TLS_PROXY_CA_CONFIG_FILE=$TLS_PROXY_DIR/ca.conf
TLS_PROXY_CERT_KEY_FILE=$TLS_PROXY_DIR/tls.key
TLS_PROXY_CERT_REQUEST_FILE=$TLS_PROXY_DIR/tls.csr
TLS_PROXY_CERT_FILE=$TLS_PROXY_DIR/tls.crt
TLS_PROXY_CERT_EXT_FILE=$TLS_PROXY_DIR/server.ext
IMAGE_AUDIT_ARCHIVE=$WORK_DIR/image-audit.tar
CHART_PACKAGE_DIR=$WORK_DIR/chart-package
NEXT_CHART_PACKAGE_DIR=$WORK_DIR/next-chart-package
PREDECESSOR_SOURCE_ARCHIVE=$WORK_DIR/predecessor-source.tar
PREDECESSOR_BUILD_CONTEXT=$WORK_DIR/predecessor-source
NEXT_SOURCE_ARCHIVE=$WORK_DIR/next-source.tar
NEXT_BUILD_CONTEXT=$WORK_DIR/next-source
PREDECESSOR_VALUES_FILE=$WORK_DIR/predecessor-values.json
CANDIDATE_VALUES_FILE=$WORK_DIR/candidate-values.json
NEXT_VALUES_FILE=$WORK_DIR/next-values.json
CLUSTER_CREATED=0
IMAGE_CREATED=0
IMAGE_AUDIT_CONTAINER_CREATED=0
IMAGE_AUDIT_CONTAINER_ID=
REGISTRY_CREATED=0
REGISTRY_CONTAINER_ID=
EXTERNAL_PG_CREATED=0
EXTERNAL_PG_CONTAINER_ID=
EXTERNAL_PG_IP=
KIND_NODE_IMAGE_CREATED=0
KIND_NETWORK_CREATED=0
CREATED_IMAGE_REFS=
CURRENT_RELEASE_SEQUENCE=
NEXT_RELEASE_SEQUENCE=
RELEASE_CHART_OUTPUT_PARENT=
RELEASE_CHART_OUTPUT_TEMP=
TUNNEL_PID=
IMAGE_AUDIT_CONTAINER=$(dns_name ptah-image-audit "$identity" 63)
TASK_CLAIM_VOLUME=$(dns_name ptah-e2e-claim "$identity" 63)
TASK_CLAIM_TOKEN=$(openssl rand -hex 16)
TASK_CLAIM_CREATE_STARTED=0
TASK_CLAIM_ACQUIRED=0

SELECTED_DOCKER_CONTEXT=$DOCKER_CONTEXT
TASK_DOCKER_CONTEXT=$(dns_name ptah-e2e-docker "$identity" 63)
DOCKER_CLI_CONFIG=$WORK_DIR/docker-cli

umask 077
printf '%s' "$REGISTRY_PASSWORD" >"$REGISTRY_PASSWORD_FILE"
jq -n \
	--arg username "$REGISTRY_USERNAME" \
	--rawfile password "$REGISTRY_PASSWORD_FILE" \
	'{username: $username, password: $password}' >"$REGISTRY_CREDENTIALS_FILE"
chmod 600 "$REGISTRY_PASSWORD_FILE" "$REGISTRY_CREDENTIALS_FILE"
printf '%s' "$EXTERNAL_PG_PASSWORD" >"$EXTERNAL_PG_PASSWORD_FILE"
printf '%s' "$EXTERNAL_PG_ADMIN_PASSWORD" >"$EXTERNAL_PG_ADMIN_PASSWORD_FILE"
printf '%s\n' "$EXTERNAL_PG_PASSWORD" | grep -Eq '^[A-Za-z0-9]+$' ||
	fail "external PostgreSQL application password is not SQL-literal safe"
{
	printf 'POSTGRES_USER=%s\n' "$EXTERNAL_PG_ADMIN_USER"
	printf 'POSTGRES_PASSWORD='
	cat "$EXTERNAL_PG_ADMIN_PASSWORD_FILE"
	printf '\nPOSTGRES_DB=%s\n' "$EXTERNAL_PG_ADMIN_DATABASE"
	printf '%s\n' 'PGDATA=/var/lib/postgresql/data/pgdata'
} >"$EXTERNAL_PG_ENV_FILE"
{
	printf "CREATE ROLE %s WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '%s';\n" \
		"$EXTERNAL_PG_USER" "$EXTERNAL_PG_PASSWORD"
	printf 'CREATE DATABASE %s OWNER %s;\n' "$EXTERNAL_PG_DATABASE" "$EXTERNAL_PG_USER"
} >"$EXTERNAL_PG_BOOTSTRAP_SQL_FILE"
jq -n \
	--arg username "$EXTERNAL_PG_USER" \
	--rawfile password "$EXTERNAL_PG_PASSWORD_FILE" \
	--arg database "$EXTERNAL_PG_DATABASE" \
	--arg authority "${EXTERNAL_PG_DNS_NAME}:5432" '
      {
        username: $username,
        password: $password,
        database: $database,
        url: ("postgres://" + $username + ":" + $password + "@" + $authority + "/" +
          $database + "?sslmode=disable")
      }
    ' \
	>"$EXTERNAL_PG_CREDENTIALS_FILE"
chmod 600 \
	"$EXTERNAL_PG_ENV_FILE" "$EXTERNAL_PG_ADMIN_PASSWORD_FILE" \
	"$EXTERNAL_PG_PASSWORD_FILE" "$EXTERNAL_PG_CREDENTIALS_FILE" \
	"$EXTERNAL_PG_BOOTSTRAP_SQL_FILE"
unset EXTERNAL_PG_ADMIN_PASSWORD EXTERNAL_PG_PASSWORD EXTERNAL_PG_URL

add_created_image() {
	if [ -z "$CREATED_IMAGE_REFS" ]; then
		CREATED_IMAGE_REFS=$1
	else
		CREATED_IMAGE_REFS="$1
$CREATED_IMAGE_REFS"
	fi
}

replace_exact_line_once() {
	transformation_file=$1
	expected_line=$2
	replacement_line=$3
	transformation_description=$4
	temporary_file=${transformation_file}.e2e-next
	[ -f "$transformation_file" ] && [ ! -L "$transformation_file" ] ||
		fail "$transformation_description target must be a regular non-symlink file"
	match_count=$(awk -v expected="$expected_line" '
      $0 == expected { count++ }
      END { print count + 0 }
    ' "$transformation_file")
	[ "$match_count" -eq 1 ] ||
		fail "$transformation_description source line count is $match_count, expected exactly one"
	awk -v expected="$expected_line" -v replacement="$replacement_line" '
      $0 == expected { print replacement; next }
      { print }
    ' "$transformation_file" >"$temporary_file"
	old_count=$(awk -v expected="$expected_line" '
      $0 == expected { count++ }
      END { print count + 0 }
    ' "$temporary_file")
	new_count=$(awk -v replacement="$replacement_line" '
      $0 == replacement { count++ }
      END { print count + 0 }
    ' "$temporary_file")
	if [ "$old_count" -ne 0 ] || [ "$new_count" -ne 1 ]; then
		fail "$transformation_description did not produce exactly one replacement"
	fi
	mv "$temporary_file" "$transformation_file"
}

go_release_sequence_from_source() {
	sequence_file=$1
	sequence_prefix=$(printf '\tCurrentReleaseSequence int32 = ')
	awk -v prefix="$sequence_prefix" '
      index($0, prefix) == 1 {
        candidate_lines++
        value = substr($0, length(prefix) + 1)
        if (value ~ /^[1-9][0-9]*$/ && $0 == prefix value) {
          declarations++
          sequence = value
        }
      }
      END {
        if (candidate_lines != 1 || declarations != 1) {
          exit 1
        }
        print sequence
      }
    ' "$sequence_file"
}

helm_release_sequence_from_source() {
	sequence_file=$1
	helpers_prefix='{{- define "ptah-operator.releaseSequence" -}}'
	helpers_suffix='{{- end -}}'
	awk -v prefix="$helpers_prefix" -v suffix="$helpers_suffix" '
      index($0, prefix) == 1 {
        candidate_lines++
        if (substr($0, length($0) - length(suffix) + 1) == suffix) {
          value = substr($0, length(prefix) + 1,
            length($0) - length(prefix) - length(suffix))
          if (value ~ /^[1-9][0-9]*$/) {
            declarations++
            sequence = value
          }
        }
      }
      END {
        if (candidate_lines != 1 || declarations != 1) {
          exit 1
        }
        print sequence
      }
    ' "$sequence_file"
}

export_release_chart() {
	[ -n "$E2E_RELEASE_CHART_OUTPUT" ] || return 0
	case "$E2E_RELEASE_CHART_OUTPUT" in
		/*) ;;
		*) fail "E2E_RELEASE_CHART_OUTPUT must be an absolute path" ;;
	esac
	release_chart_output_name=${E2E_RELEASE_CHART_OUTPUT##*/}
	case "$release_chart_output_name" in
		'' | . | ..) fail "E2E_RELEASE_CHART_OUTPUT must name a file" ;;
	esac
	release_chart_output_parent=${E2E_RELEASE_CHART_OUTPUT%/*}
	[ -n "$release_chart_output_parent" ] || release_chart_output_parent=/
	[ -d "$release_chart_output_parent" ] && [ ! -L "$release_chart_output_parent" ] ||
		fail "E2E_RELEASE_CHART_OUTPUT parent must be an existing non-symlink directory"
	RELEASE_CHART_OUTPUT_PARENT=$(cd -P "$release_chart_output_parent" && pwd) ||
		fail "could not resolve E2E_RELEASE_CHART_OUTPUT parent"
	case "$RELEASE_CHART_OUTPUT_PARENT" in
		/) RELEASE_CHART_OUTPUT_TARGET=/$release_chart_output_name ;;
		*) RELEASE_CHART_OUTPUT_TARGET=$RELEASE_CHART_OUTPUT_PARENT/$release_chart_output_name ;;
	esac
	physical_work_dir=$(cd -P "$WORK_DIR" && pwd) ||
		fail "could not resolve the task work directory"
	case "$RELEASE_CHART_OUTPUT_TARGET" in
		"$physical_work_dir" | "$physical_work_dir"/*)
			fail "E2E_RELEASE_CHART_OUTPUT must be outside the task work directory"
		;;
	esac
	[ ! -e "$RELEASE_CHART_OUTPUT_TARGET" ] && [ ! -L "$RELEASE_CHART_OUTPUT_TARGET" ] ||
		fail "refusing to replace existing E2E_RELEASE_CHART_OUTPUT target"
	[ -f "$CHART_PACKAGE" ] && [ ! -L "$CHART_PACKAGE" ] ||
		fail "release-form sequence-$CURRENT_RELEASE_SEQUENCE chart package is not a regular non-symlink file"
	RELEASE_CHART_OUTPUT_TEMP=$(mktemp \
		"$RELEASE_CHART_OUTPUT_PARENT/.ptah-operator-release-chart.XXXXXX") ||
		fail "could not create the atomic release chart output"
	if ! cp "$CHART_PACKAGE" "$RELEASE_CHART_OUTPUT_TEMP"; then
		rm -f -- "$RELEASE_CHART_OUTPUT_TEMP"
		RELEASE_CHART_OUTPUT_TEMP=
		fail "could not copy the release-form sequence-$CURRENT_RELEASE_SEQUENCE chart package"
	fi
	chmod 600 "$RELEASE_CHART_OUTPUT_TEMP"
	if ! cmp -s "$CHART_PACKAGE" "$RELEASE_CHART_OUTPUT_TEMP"; then
		rm -f -- "$RELEASE_CHART_OUTPUT_TEMP"
		RELEASE_CHART_OUTPUT_TEMP=
		fail "atomic release chart output differs from the sequence-$CURRENT_RELEASE_SEQUENCE chart package"
	fi
	[ ! -e "$RELEASE_CHART_OUTPUT_TARGET" ] && [ ! -L "$RELEASE_CHART_OUTPUT_TARGET" ] || {
		rm -f -- "$RELEASE_CHART_OUTPUT_TEMP"
		RELEASE_CHART_OUTPUT_TEMP=
		fail "E2E_RELEASE_CHART_OUTPUT target appeared during export"
	}
	# The hard-link creation is the atomic publication step and, unlike a plain
	# rename, cannot replace a target that appears after the checks above.
	if ! ln "$RELEASE_CHART_OUTPUT_TEMP" "$RELEASE_CHART_OUTPUT_TARGET"; then
		rm -f -- "$RELEASE_CHART_OUTPUT_TEMP"
		RELEASE_CHART_OUTPUT_TEMP=
		fail "could not publish the no-clobber atomic release chart output"
	fi
	if ! rm -f -- "$RELEASE_CHART_OUTPUT_TEMP"; then
		fail "could not remove the temporary release chart link after publication"
	fi
	RELEASE_CHART_OUTPUT_TEMP=
	printf 'e2e: exported sequence-%s release chart %s (%s)\n' \
		"$CURRENT_RELEASE_SEQUENCE" "$RELEASE_CHART_OUTPUT_TARGET" "$CHART_PACKAGE_DIGEST"
}

# Docker preserves a named volume's original labels when a later create uses
# the same name. A random label therefore turns create-plus-inspect into a
# daemon-side compare-and-set without requiring a helper image.
task_claim_matches_owner() {
	docker --context "$DOCKER_CONTEXT" volume inspect "$TASK_CLAIM_VOLUME" \
		--format '{{json .Labels}}' 2>/dev/null |
		jq -e \
			--arg owner "$CLUSTER_NAME" \
			--arg token "$TASK_CLAIM_TOKEN" '
        .["operator.ptah.dev/e2e-owner"] == $owner and
        .["operator.ptah.dev/e2e-component"] == "task-claim" and
        .["operator.ptah.dev/e2e-claim-token"] == $token
      ' >/dev/null
}

acquire_task_claim() {
	[ "$TASK_CLAIM_CREATE_STARTED" -eq 0 ] || fail "task identity claim acquisition was attempted more than once"
	TASK_CLAIM_CREATE_STARTED=1
	if ! created_claim=$(docker --context "$DOCKER_CONTEXT" volume create \
		--label "operator.ptah.dev/e2e-owner=${CLUSTER_NAME}" \
		--label 'operator.ptah.dev/e2e-component=task-claim' \
		--label "operator.ptah.dev/e2e-claim-token=${TASK_CLAIM_TOKEN}" \
		"$TASK_CLAIM_VOLUME"); then
		fail "could not acquire the task identity claim on Docker context $SELECTED_DOCKER_CONTEXT"
	fi
	[ "$created_claim" = "$TASK_CLAIM_VOLUME" ] ||
		fail "Docker returned an unexpected task identity claim name"
	if ! task_claim_matches_owner; then
		fail "E2E identity $identity is already claimed on Docker context $SELECTED_DOCKER_CONTEXT; choose another E2E_RUN_ID"
	fi
	TASK_CLAIM_ACQUIRED=1
}

image_audit_container_matches_task() {
	image_audit_id=$1
	docker --context "$DOCKER_CONTEXT" container inspect "$image_audit_id" 2>/dev/null |
		jq -e \
			--arg id "$image_audit_id" \
			--arg name "/${IMAGE_AUDIT_CONTAINER}" \
			--arg owner "$CLUSTER_NAME" \
			--arg token "$TASK_CLAIM_TOKEN" '
        length == 1 and
        .[0].Id == $id and
        .[0].Name == $name and
        .[0].Config.Labels["operator.ptah.dev/e2e-owner"] == $owner and
        .[0].Config.Labels["operator.ptah.dev/e2e-component"] == "image-audit" and
        .[0].Config.Labels["operator.ptah.dev/e2e-claim-token"] == $token
      ' >/dev/null
}

create_image_audit_container() {
	image_audit_source=$1
	if docker --context "$DOCKER_CONTEXT" container inspect "$IMAGE_AUDIT_CONTAINER" >/dev/null 2>&1; then
		fail "refusing to reuse pre-existing image-audit container $IMAGE_AUDIT_CONTAINER"
	fi
	IMAGE_AUDIT_CONTAINER_CREATED=1
	if ! image_audit_id=$(docker --context "$DOCKER_CONTEXT" create \
		--name "$IMAGE_AUDIT_CONTAINER" \
		--label "operator.ptah.dev/e2e-owner=${CLUSTER_NAME}" \
		--label 'operator.ptah.dev/e2e-component=image-audit' \
		--label "operator.ptah.dev/e2e-claim-token=${TASK_CLAIM_TOKEN}" \
		"$image_audit_source"); then
		fail "could not create the task-owned image-audit container"
	fi
	printf '%s\n' "$image_audit_id" | grep -Eq '^[0-9a-f]{64}$' ||
		fail "image-audit container does not have an exact Docker ID"
	IMAGE_AUDIT_CONTAINER_ID=$image_audit_id
	image_audit_container_matches_task "$IMAGE_AUDIT_CONTAINER_ID" ||
		fail "image-audit container does not have the exact task identity"
}

remove_image_audit_container() {
	[ "$IMAGE_AUDIT_CONTAINER_CREATED" -eq 1 ] ||
		fail "image-audit container removal was requested without task ownership"
	[ -n "$IMAGE_AUDIT_CONTAINER_ID" ] || fail "image-audit container ID is empty"
	image_audit_container_matches_task "$IMAGE_AUDIT_CONTAINER_ID" ||
		fail "refusing to remove an image-audit container without the exact task identity"
	docker --context "$DOCKER_CONTEXT" container rm "$IMAGE_AUDIT_CONTAINER_ID" >/dev/null ||
		fail "could not remove the task-owned image-audit container"
	if docker --context "$DOCKER_CONTEXT" container inspect "$IMAGE_AUDIT_CONTAINER_ID" >/dev/null 2>&1; then
		fail "image-audit container still exists after removal"
	fi
	IMAGE_AUDIT_CONTAINER_CREATED=0
	IMAGE_AUDIT_CONTAINER_ID=
}

external_pg_mounts_are_ephemeral() {
	jq -e '
      (.HostConfig.Tmpfs | keys) == ["/var/lib/postgresql/data"] and
      ((.HostConfig.Tmpfs["/var/lib/postgresql/data"] | split(",") | sort) ==
        (["rw", "noexec", "nosuid", "nodev", "size=536870912"] | sort)) and
      ((.HostConfig.Binds // []) | length) == 0 and
      ((.HostConfig.Mounts // []) | length) == 0 and
      ((.HostConfig.VolumesFrom // []) | length) == 0 and
      ((.Mounts // []) | all(
        .Type == "tmpfs" and .Destination == "/var/lib/postgresql/data"))
	' >/dev/null
}

external_pg_app_query() {
	external_pg_query=$1
	{
		cat "$EXTERNAL_PG_PASSWORD_FILE"
		printf '\n'
	} | docker --context "$DOCKER_CONTEXT" exec -i "$EXTERNAL_PG_CONTAINER_ID" \
		sh -ec '
      IFS= read -r PGPASSWORD
      export PGPASSWORD
      exec psql -h 127.0.0.1 -U "$1" -d "$2" -Atqc "$3"
    ' sh "$EXTERNAL_PG_USER" "$EXTERNAL_PG_DATABASE" "$external_pg_query"
}

assert_external_pg_container_contract() {
	external_contract_id=$1
	external_contract_ip=$2
	[ -n "$external_contract_id" ] || fail "external PostgreSQL container ID is empty"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{.Id}}' "$external_contract_id")" = "$external_contract_id" ] ||
		fail "external PostgreSQL container identity changed"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{.Name}}' "$external_contract_id")" = "/${EXTERNAL_PG_CONTAINER}" ] ||
		fail "external PostgreSQL container name changed"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{.State.Running}}' "$external_contract_id")" = true ] ||
		fail "external PostgreSQL container is not running"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{.HostConfig.RestartPolicy.Name}}' "$external_contract_id")" = no ] ||
		fail "external PostgreSQL container gained a restart policy"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{.HostConfig.PublishAllPorts}}' "$external_contract_id")" = false ] ||
		fail "external PostgreSQL container publishes all ports"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{index .Config.Labels "operator.ptah.dev/e2e-owner"}}' \
		"$external_contract_id")" = "$CLUSTER_NAME" ] ||
		fail "external PostgreSQL container lost its task owner label"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{index .Config.Labels "operator.ptah.dev/e2e-component"}}' \
		"$external_contract_id")" = external-postgresql ] ||
		fail "external PostgreSQL container lost its component label"
	[ "$(docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{.Config.Image}}' "$external_contract_id")" = "$E2E_POSTGRES_SOURCE_IMAGE" ] ||
		fail "external PostgreSQL container lost its digest-pinned source image"
	docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{json .HostConfig.PortBindings}}' "$external_contract_id" |
		jq -e '. == null or . == {}' >/dev/null ||
		fail "external PostgreSQL container has host port bindings"
	docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{json .NetworkSettings.Ports}}' "$external_contract_id" |
		jq -e 'to_entries | all(.value == null)' >/dev/null ||
		fail "external PostgreSQL container exposes a host port"
	docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{"HostConfig":{"Tmpfs":{{json .HostConfig.Tmpfs}},"Binds":{{json .HostConfig.Binds}},"Mounts":{{json .HostConfig.Mounts}},"VolumesFrom":{{json .HostConfig.VolumesFrom}}},"Mounts":{{json .Mounts}}}' \
		"$external_contract_id" | external_pg_mounts_are_ephemeral ||
		fail "external PostgreSQL container has a persistent or unexpected mount"
	docker --context "$DOCKER_CONTEXT" container inspect \
		--format '{{json .NetworkSettings.Networks}}' "$external_contract_id" |
		jq -e --arg address "$external_contract_ip" '
      keys == ["kind"] and .kind.IPAddress == $address
    ' >/dev/null || fail "external PostgreSQL container left its exact kind-network address"
}

collect_node_readiness_diagnostics() {
	node_diagnostics_context=$1
	printf 'e2e: Kubernetes node readiness diagnostics (%s)\n' \
		"$node_diagnostics_context" >&2
	printf '%s\n' 'e2e: node conditions: name type status reason last-transition' >&2
	kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s get nodes -o json |
		jq -r '
      .items[] as $node
      | ($node.status.conditions // [])[]
      | [
          $node.metadata.name,
          .type,
          .status,
          (.reason // "-"),
          (.lastTransitionTime // "-")
        ]
      | @tsv
    ' >&2 || true
	printf '%s\n' 'e2e: recent node warnings: namespace node reason count time' >&2
	kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s get events -A \
		--field-selector type=Warning -o json |
		jq -r '
      [.items[] | select(.involvedObject.kind == "Node")]
      | sort_by(.eventTime // .lastTimestamp // .metadata.creationTimestamp // "")
      | .[-20:][]
      | [
          (.metadata.namespace // "-"),
          .involvedObject.name,
          (.reason // "-"),
          ((.count // 1) | tostring),
          (.eventTime // .lastTimestamp // .metadata.creationTimestamp // "-")
        ]
      | @tsv
    ' >&2 || true
}

wait_for_ready_nodes() {
	node_readiness_context=$1
	if ! kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
		get nodes -o json >"$NODE_READINESS_FILE"; then
		collect_node_readiness_diagnostics "$node_readiness_context"
		return 1
	fi
	if ! jq -e '.items | length == 4' "$NODE_READINESS_FILE" >/dev/null; then
		collect_node_readiness_diagnostics "$node_readiness_context"
		return 1
	fi
	if ! kubectl --kubeconfig "$KUBECONFIG_FILE" wait \
		--for=condition=Ready nodes --all --timeout=2m; then
		collect_node_readiness_diagnostics "$node_readiness_context"
		return 1
	fi
}

nodes_ready_now() {
	kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
		get nodes -o json >"$NODE_READINESS_FILE" &&
		jq -e '
	  ((.items | length) == 4) and
	  all(.items[];
        any((.status.conditions // [])[];
          .type == "Ready" and .status == "True"
        )
      )
    ' "$NODE_READINESS_FILE" >/dev/null
}

assert_kind_ha_topology() {
	kind get nodes --name "$CLUSTER_NAME" | LC_ALL=C sort >"$KIND_NODE_INVENTORY_FILE"
	if ! {
		printf '%s\n' \
			"${CLUSTER_NAME}-control-plane" \
			"${CLUSTER_NAME}-control-plane2" \
			"${CLUSTER_NAME}-control-plane3" \
			"${CLUSTER_NAME}-worker"
	} | LC_ALL=C sort | cmp -s - "$KIND_NODE_INVENTORY_FILE"; then
		fail "kind cluster does not have the exact three-control-plane, one-worker topology"
	fi
	kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
		get nodes -o json >"$NODE_READINESS_FILE"
	jq -e --arg cluster "$CLUSTER_NAME" '
      ([.items[].metadata.name] | sort) == ([$cluster + "-control-plane", $cluster + "-control-plane2", $cluster + "-control-plane3", $cluster + "-worker"] | sort) and
      ([.items[] | select(.metadata.labels["node-role.kubernetes.io/control-plane"] != null)] | length) == 3 and
      ([.items[] | select(.metadata.labels["node-role.kubernetes.io/control-plane"] == null)] | length) == 1 and
      all(.items[];
        any((.status.conditions // [])[];
          .type == "Ready" and .status == "True"
        )
      )
    ' "$NODE_READINESS_FILE" >/dev/null ||
		fail "Kubernetes node inventory does not match the ready HA kind topology"
}

assert_api_server_endpoint_inventory() {
	api_endpoint_deadline=$(($(date +%s) + 60))
	while [ "$(date +%s)" -lt "$api_endpoint_deadline" ]; do
		if kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
			get nodes -o json >"$NODE_READINESS_FILE" &&
			kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
			-n default get endpointslices \
			-l kubernetes.io/service-name=kubernetes -o json >"$API_SERVER_ENDPOINT_INVENTORY_FILE" &&
			jq -e --arg cluster "$CLUSTER_NAME" --slurpfile nodes "$NODE_READINESS_FILE" \
				-f "$ROOT_DIR/hack/api-server-endpoint-inventory.jq" \
				"$API_SERVER_ENDPOINT_INVENTORY_FILE" >/dev/null &&
			probe_api_server_endpoints; then
			return 0
		fi
		sleep 1
	done
	fail "default Kubernetes Service did not advertise and serve exactly the three control-plane API server endpoints"
}

probe_api_server_endpoints() {
	jq -er '
      [.items[]
        | select(.metadata.labels["kubernetes.io/service-name"] == "kubernetes")
        | .endpoints[].addresses[]]
      | sort[]
    ' "$API_SERVER_ENDPOINT_INVENTORY_FILE" >"$API_SERVER_ENDPOINT_ADDRESS_FILE" || return 1
	api_server_endpoint_probe_count=0
	while IFS= read -r api_server_endpoint; do
		api_server_endpoint_probe_count=$((api_server_endpoint_probe_count + 1))
		if ! api_server_readyz=$(docker --context "$DOCKER_CONTEXT" exec \
			"${CLUSTER_NAME}-control-plane" \
			kubectl --kubeconfig /etc/kubernetes/admin.conf \
			--server "https://${api_server_endpoint}:6443" \
			--tls-server-name kubernetes \
			--request-timeout=10s get --raw=/readyz 2>/dev/null); then
			return 1
		fi
		[ "$api_server_readyz" = ok ] || return 1
	done <"$API_SERVER_ENDPOINT_ADDRESS_FILE"
	[ "$api_server_endpoint_probe_count" -eq 3 ]
}

configure_registry_hosts_on_kind_nodes() {
	for kind_node_container in \
		"${CLUSTER_NAME}-control-plane" \
		"${CLUSTER_NAME}-control-plane2" \
		"${CLUSTER_NAME}-control-plane3" \
		"${CLUSTER_NAME}-worker"; do
		registry_dns_deadline=$(($(date +%s) + 30))
		registry_dns_ready=0
		while [ "$(date +%s)" -lt "$registry_dns_deadline" ]; do
			if docker --context "$DOCKER_CONTEXT" exec "$kind_node_container" \
				getent ahostsv4 "$REGISTRY_DNS_NAME" 2>/dev/null |
				awk -v expected="$REGISTRY_IP" '$1 == expected {found = 1} END {exit !found}'; then
				registry_dns_ready=1
				break
			fi
			sleep 1
		done
		[ "$registry_dns_ready" -eq 1 ] ||
			fail "registry network alias did not resolve on kind node $kind_node_container"
		docker --context "$DOCKER_CONTEXT" exec "$kind_node_container" \
			mkdir -p "/etc/containerd/certs.d/${REGISTRY_HOST}"
		docker --context "$DOCKER_CONTEXT" cp "$REGISTRY_HOSTS_FILE" \
			"${kind_node_container}:/etc/containerd/certs.d/${REGISTRY_HOST}/hosts.toml"
		if ! docker --context "$DOCKER_CONTEXT" exec "$kind_node_container" \
			cat "/etc/containerd/certs.d/${REGISTRY_HOST}/hosts.toml" |
			cmp -s "$REGISTRY_HOSTS_FILE" -; then
			fail "registry hosts configuration differs on kind node $kind_node_container"
		fi
	done
}

require_ready_nodes() {
	required_readiness_context=$1
	if ! wait_for_ready_nodes "$required_readiness_context"; then
		fail "infrastructure readiness check failed: $required_readiness_context"
	fi
}

assert_api_server_feature_gate_scope() {
	expected_api_server_feature_gates=$1
	control_plane_pods_file=$WORK_DIR/control-plane-pods.json
	component_configs_file=$WORK_DIR/component-configs.json
	kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
		-n kube-system get pods -o json >"$control_plane_pods_file"
	jq -e --arg expected "$expected_api_server_feature_gates" --arg cluster "$CLUSTER_NAME" '
      def component_pods($component):
        [.items[] | select(.metadata.labels.component == $component)];
      def command_options($pod; $prefix):
        [$pod.spec.containers[].command[] | select(startswith($prefix))];
      def exact_control_plane_nodes($pods):
        ([$pods[].spec.nodeName] | sort) ==
          ([$cluster + "-control-plane", $cluster + "-control-plane2", $cluster + "-control-plane3"] | sort);
      def component_is_ready($pod; $container_name):
        ($pod.metadata.deletionTimestamp == null) and
        (($pod.metadata.annotations["kubernetes.io/config.mirror"] // "") | length) > 0 and
        ($pod.metadata.name == ($container_name + "-" + $pod.spec.nodeName)) and
        ($pod.status.phase == "Running") and
        ([($pod.status.conditions // [])[] | select(.type == "Ready" and .status == "True")] | length) == 1 and
        (($pod.spec.containers // []) | length) == 1 and
        ($pod.spec.containers[0].name == $container_name) and
        (($pod.status.containerStatuses // []) | length) == 1 and
        ($pod.status.containerStatuses[0].name == $container_name) and
        ($pod.status.containerStatuses[0].ready == true) and
        (($pod.status.containerStatuses[0].state.running | type) == "object");

      (component_pods("kube-apiserver")) as $api_servers |
      (component_pods("kube-controller-manager")) as $controller_managers |
      (component_pods("kube-scheduler")) as $schedulers |
      ($api_servers | length) == 3 and
      ($controller_managers | length) == 3 and
      ($schedulers | length) == 3 and
      exact_control_plane_nodes($api_servers) and
      exact_control_plane_nodes($controller_managers) and
      exact_control_plane_nodes($schedulers) and
      all($api_servers[];
        component_is_ready(.; "kube-apiserver") and
        command_options(.; "--feature-gates=") ==
          (if $expected == "" then [] else ["--feature-gates=" + $expected] end) and
        (command_options(.; "--runtime-config=") | length) == 1
      ) and
      all($controller_managers[];
        component_is_ready(.; "kube-controller-manager") and
        command_options(.; "--feature-gates=") == []
      ) and
      all($schedulers[];
        component_is_ready(.; "kube-scheduler") and
        command_options(.; "--feature-gates=") == []
      )
	' "$control_plane_pods_file" >/dev/null ||
		fail "control-plane feature gates are not confined to the API server or kind runtime-config was replaced"
	kubectl --kubeconfig "$KUBECONFIG_FILE" --request-timeout=15s \
		-n kube-system get configmaps kubelet-config kube-proxy -o json >"$component_configs_file"
	jq -e '
      (.items | map(select(.metadata.name == "kubelet-config"))) as $kubelet_configs |
      ([.items[].data | to_entries[].value] | join("\n")) as $configs |
      (.items | length) == 2 and
      ($kubelet_configs | length) == 1 and
      (($kubelet_configs[0].data.kubelet // "") | contains("KubeletInUserNamespace: true")) and
      ([
        "EmptyDirVolumeMode",
        "EvictionRequestAPI",
        "GenericWorkload",
        "VolumeBindMountOptions",
        "WorkloadWithJob"
      ] |
      all(.[]; . as $gate | ($configs | contains($gate) | not)))
    ' "$component_configs_file" >/dev/null ||
		fail "API-server-only feature gates leaked into kubelet or kube-proxy configuration"
}

collect_diagnostics() {
	[ "$CLUSTER_CREATED" -eq 1 ] || return 0
	[ -s "$KUBECONFIG_FILE" ] || return 0
	printf '%s\n' 'e2e: collecting failure diagnostics' >&2
	kubectl --kubeconfig "$KUBECONFIG_FILE" get nodes -o wide >&2 || true
	kubectl --kubeconfig "$KUBECONFIG_FILE" get all -A >&2 || true
	printf '%s\n' 'e2e: raw manager and Job logs are suppressed to protect credential-isolation failures' >&2
	helm --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" status "$HELM_RELEASE" >&2 || true
}

cleanup() {
	status=$?
	cleanup_failed=0
	trap - EXIT HUP INT TERM
	set +e
	if [ "$status" -ne 0 ]; then
		collect_diagnostics
	fi
	if [ -n "$RELEASE_CHART_OUTPUT_TEMP" ]; then
		case "$RELEASE_CHART_OUTPUT_TEMP" in
			"$RELEASE_CHART_OUTPUT_PARENT"/.ptah-operator-release-chart.*)
				if ! rm -f -- "$RELEASE_CHART_OUTPUT_TEMP"; then
					printf 'e2e: could not remove incomplete release chart output %s\n' \
						"$RELEASE_CHART_OUTPUT_TEMP" >&2
					cleanup_failed=1
				fi
			;;
			*)
				printf 'e2e: refusing to remove unexpected release chart temporary path %s\n' \
					"$RELEASE_CHART_OUTPUT_TEMP" >&2
				cleanup_failed=1
			;;
		esac
	fi
	if [ "$EXTERNAL_PG_CREATED" -eq 1 ]; then
		external_cleanup_id=$EXTERNAL_PG_CONTAINER_ID
		if [ -z "$external_cleanup_id" ] &&
			docker --context "$DOCKER_CONTEXT" container inspect "$EXTERNAL_PG_CONTAINER" >/dev/null 2>&1; then
			external_cleanup_owner=$(docker --context "$DOCKER_CONTEXT" container inspect \
				--format '{{index .Config.Labels "operator.ptah.dev/e2e-owner"}}' \
				"$EXTERNAL_PG_CONTAINER" 2>/dev/null)
			external_cleanup_component=$(docker --context "$DOCKER_CONTEXT" container inspect \
				--format '{{index .Config.Labels "operator.ptah.dev/e2e-component"}}' \
				"$EXTERNAL_PG_CONTAINER" 2>/dev/null)
			if [ "$external_cleanup_owner" = "$CLUSTER_NAME" ] &&
				[ "$external_cleanup_component" = external-postgresql ]; then
				external_cleanup_id=$(docker --context "$DOCKER_CONTEXT" container inspect \
					--format '{{.Id}}' "$EXTERNAL_PG_CONTAINER" 2>/dev/null)
			else
				printf 'e2e: refusing to remove external PostgreSQL container %s without the exact task owner\n' \
					"$EXTERNAL_PG_CONTAINER" >&2
				cleanup_failed=1
			fi
		fi
		if [ -n "$external_cleanup_id" ]; then
			if ! docker --context "$DOCKER_CONTEXT" container rm -fv "$external_cleanup_id" >/dev/null 2>&1; then
				if ! docker --context "$DOCKER_CONTEXT" info >/dev/null 2>&1 ||
					docker --context "$DOCKER_CONTEXT" container inspect "$external_cleanup_id" >/dev/null 2>&1; then
					printf 'e2e: could not remove external PostgreSQL container ID %s from Docker context %s\n' \
						"$external_cleanup_id" "$SELECTED_DOCKER_CONTEXT" >&2
					cleanup_failed=1
				fi
			fi
		fi
	fi
	if [ "$REGISTRY_CREATED" -eq 1 ]; then
		registry_cleanup_id=$REGISTRY_CONTAINER_ID
		if [ -z "$registry_cleanup_id" ] &&
			docker --context "$DOCKER_CONTEXT" container inspect "$REGISTRY_CONTAINER" >/dev/null 2>&1; then
			registry_cleanup_owner=$(docker --context "$DOCKER_CONTEXT" container inspect \
				--format '{{index .Config.Labels "operator.ptah.dev/e2e-owner"}}' \
				"$REGISTRY_CONTAINER" 2>/dev/null)
			registry_cleanup_component=$(docker --context "$DOCKER_CONTEXT" container inspect \
				--format '{{index .Config.Labels "operator.ptah.dev/e2e-component"}}' \
				"$REGISTRY_CONTAINER" 2>/dev/null)
			if [ "$registry_cleanup_owner" = "$CLUSTER_NAME" ] &&
				[ "$registry_cleanup_component" = registry ]; then
				registry_cleanup_id=$(docker --context "$DOCKER_CONTEXT" container inspect \
					--format '{{.Id}}' "$REGISTRY_CONTAINER" 2>/dev/null)
			else
				printf 'e2e: refusing to remove registry container %s without the exact task labels\n' \
					"$REGISTRY_CONTAINER" >&2
				cleanup_failed=1
			fi
		fi
		if [ -n "$registry_cleanup_id" ]; then
			if ! docker --context "$DOCKER_CONTEXT" container rm -fv "$registry_cleanup_id" >/dev/null 2>&1; then
				if ! docker --context "$DOCKER_CONTEXT" info >/dev/null 2>&1 ||
					docker --context "$DOCKER_CONTEXT" container inspect "$registry_cleanup_id" >/dev/null 2>&1; then
					printf 'e2e: could not remove registry container ID %s from Docker context %s\n' \
						"$registry_cleanup_id" "$SELECTED_DOCKER_CONTEXT" >&2
					cleanup_failed=1
				fi
			fi
		fi
	fi
	if [ "$IMAGE_AUDIT_CONTAINER_CREATED" -eq 1 ]; then
		image_audit_cleanup_id=$IMAGE_AUDIT_CONTAINER_ID
		if [ -z "$image_audit_cleanup_id" ] &&
			docker --context "$DOCKER_CONTEXT" container inspect "$IMAGE_AUDIT_CONTAINER" >/dev/null 2>&1; then
			image_audit_cleanup_id=$(docker --context "$DOCKER_CONTEXT" container inspect \
				--format '{{.Id}}' "$IMAGE_AUDIT_CONTAINER" 2>/dev/null)
		fi
		if [ -n "$image_audit_cleanup_id" ]; then
			if ! image_audit_container_matches_task "$image_audit_cleanup_id"; then
				printf 'e2e: refusing to remove image-audit container %s without the exact task identity\n' \
					"$image_audit_cleanup_id" >&2
				cleanup_failed=1
			elif ! docker --context "$DOCKER_CONTEXT" container rm -f "$image_audit_cleanup_id" >/dev/null 2>&1; then
				if ! docker --context "$DOCKER_CONTEXT" info >/dev/null 2>&1 ||
					docker --context "$DOCKER_CONTEXT" container inspect "$image_audit_cleanup_id" >/dev/null 2>&1; then
					printf 'e2e: could not remove image-audit container ID %s from Docker context %s\n' \
						"$image_audit_cleanup_id" "$SELECTED_DOCKER_CONTEXT" >&2
					cleanup_failed=1
				fi
			fi
		fi
	fi
	if [ "$CLUSTER_CREATED" -eq 1 ]; then
		if ! kind delete cluster --name "$CLUSTER_NAME" >/dev/null; then
			printf 'e2e: could not delete kind cluster %s\n' "$CLUSTER_NAME" >&2
			cleanup_failed=1
		fi
	fi
	if [ "$KIND_NODE_IMAGE_CREATED" -eq 1 ]; then
		if ! docker --context "$DOCKER_CONTEXT" image rm "$KIND_NODE_IMAGE" >/dev/null 2>&1; then
			if ! docker --context "$DOCKER_CONTEXT" info >/dev/null 2>&1 ||
				docker --context "$DOCKER_CONTEXT" image inspect "$KIND_NODE_IMAGE" >/dev/null 2>&1; then
				printf 'e2e: could not remove kind node image %s from Docker context %s\n' \
					"$KIND_NODE_IMAGE" "$SELECTED_DOCKER_CONTEXT" >&2
				cleanup_failed=1
			fi
		fi
	fi
	if [ "$KIND_NETWORK_CREATED" -eq 1 ] &&
		docker --context "$DOCKER_CONTEXT" network inspect kind >/dev/null 2>&1; then
		kind_network_users=$(docker --context "$DOCKER_CONTEXT" network inspect kind \
			--format '{{len .Containers}}' 2>/dev/null || printf 'unknown')
		if [ "$kind_network_users" = 0 ]; then
			if ! docker --context "$DOCKER_CONTEXT" network rm kind >/dev/null 2>&1; then
				printf 'e2e: could not remove the task-created empty kind network from Docker context %s\n' \
					"$SELECTED_DOCKER_CONTEXT" >&2
				cleanup_failed=1
			fi
		else
			printf 'e2e: preserving kind network because it now has %s attached containers\n' \
				"$kind_network_users" >&2
		fi
	fi
	if [ -n "$TUNNEL_PID" ]; then
		kill "$TUNNEL_PID" >/dev/null 2>&1
		wait "$TUNNEL_PID" >/dev/null 2>&1
	fi
	if [ "$IMAGE_CREATED" -eq 1 ]; then
		if ! docker --context "$DOCKER_CONTEXT" image rm "$OPERATOR_IMAGE" >/dev/null 2>&1; then
			if ! docker --context "$DOCKER_CONTEXT" info >/dev/null 2>&1 ||
				docker --context "$DOCKER_CONTEXT" image inspect "$OPERATOR_IMAGE" >/dev/null 2>&1; then
				printf 'e2e: could not remove or verify removal of image %s from Docker context %s\n' \
					"$OPERATOR_IMAGE" "$SELECTED_DOCKER_CONTEXT" >&2
				cleanup_failed=1
			fi
		fi
	fi
	for cleanup_image in $CREATED_IMAGE_REFS; do
		if ! docker --context "$DOCKER_CONTEXT" image rm "$cleanup_image" >/dev/null 2>&1; then
			if ! docker --context "$DOCKER_CONTEXT" info >/dev/null 2>&1 ||
				docker --context "$DOCKER_CONTEXT" image inspect "$cleanup_image" >/dev/null 2>&1; then
				printf 'e2e: could not remove image %s from Docker context %s\n' \
					"$cleanup_image" "$SELECTED_DOCKER_CONTEXT" >&2
				cleanup_failed=1
			fi
		fi
	done
	if [ "$TASK_CLAIM_CREATE_STARTED" -eq 1 ] &&
		docker --context "$DOCKER_CONTEXT" volume inspect "$TASK_CLAIM_VOLUME" >/dev/null 2>&1; then
		if task_claim_matches_owner; then
			if ! docker --context "$DOCKER_CONTEXT" volume rm "$TASK_CLAIM_VOLUME" >/dev/null 2>&1; then
				if ! docker --context "$DOCKER_CONTEXT" info >/dev/null 2>&1 ||
					docker --context "$DOCKER_CONTEXT" volume inspect "$TASK_CLAIM_VOLUME" >/dev/null 2>&1; then
					printf 'e2e: could not remove task identity claim %s from Docker context %s\n' \
						"$TASK_CLAIM_VOLUME" "$SELECTED_DOCKER_CONTEXT" >&2
					cleanup_failed=1
				fi
			fi
		elif [ "$TASK_CLAIM_ACQUIRED" -eq 1 ]; then
			printf 'e2e: refusing to remove task identity claim %s after its owner labels changed\n' \
				"$TASK_CLAIM_VOLUME" >&2
			cleanup_failed=1
		else
			printf 'e2e: preserving task identity claim %s owned by another run\n' \
				"$TASK_CLAIM_VOLUME" >&2
			fi
	fi
	case "$WORK_DIR" in
		"${TMPDIR:-/tmp}"/ptah-operator-e2e.*)
			rm -rf -- "$WORK_DIR"
			if [ -e "$WORK_DIR" ]; then
				printf 'e2e: could not remove task work directory %s\n' "$WORK_DIR" >&2
				cleanup_failed=1
			fi
		;;
		*)
			printf 'e2e: refusing to remove unexpected work directory %s\n' "$WORK_DIR" >&2
			cleanup_failed=1
		;;
	esac
	if [ "$cleanup_failed" -ne 0 ]; then
		printf '%s\n' 'e2e: cleanup is incomplete; remove only the named resources reported above' >&2
		[ "$status" -ne 0 ] || status=1
	elif [ "$status" -ne 0 ]; then
		printf 'e2e: failed; task-created resources were cleaned up\n' >&2
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

acquire_task_claim
if ! existing_clusters=$(kind get clusters); then
	fail "could not list kind clusters through Docker context $DOCKER_CONTEXT"
fi
if printf '%s\n' "$existing_clusters" | grep -Fx "$CLUSTER_NAME" >/dev/null; then
	fail "refusing to reuse pre-existing kind cluster $CLUSTER_NAME; choose another E2E_RUN_ID"
fi
if docker --context "$DOCKER_CONTEXT" image inspect "$OPERATOR_IMAGE" >/dev/null 2>&1; then
	fail "refusing to overwrite pre-existing image $OPERATOR_IMAGE; choose another E2E_RUN_ID"
fi
if docker --context "$DOCKER_CONTEXT" image inspect "$NEXT_OPERATOR_IMAGE" >/dev/null 2>&1; then
	fail "refusing to overwrite pre-existing image $NEXT_OPERATOR_IMAGE; choose another E2E_RUN_ID"
fi
if docker --context "$DOCKER_CONTEXT" image inspect "$PREDECESSOR_OPERATOR_IMAGE" >/dev/null 2>&1; then
	fail "refusing to overwrite pre-existing image $PREDECESSOR_OPERATOR_IMAGE; choose another E2E_RUN_ID"
fi
if docker --context "$DOCKER_CONTEXT" image inspect "$FIXTURE_BUILD_IMAGE" >/dev/null 2>&1; then
	fail "refusing to overwrite pre-existing image $FIXTURE_BUILD_IMAGE; choose another E2E_RUN_ID"
fi
if [ -z "$E2E_EXECUTOR_IMAGE" ] &&
	docker --context "$DOCKER_CONTEXT" image inspect "$PTAH_IMAGE" >/dev/null 2>&1; then
	fail "refusing to overwrite pre-existing image $PTAH_IMAGE; choose another E2E_RUN_ID"
fi
if docker --context "$DOCKER_CONTEXT" container inspect "$REGISTRY_CONTAINER" >/dev/null 2>&1; then
	fail "refusing to reuse pre-existing registry container $REGISTRY_CONTAINER; choose another E2E_RUN_ID"
fi
if docker --context "$DOCKER_CONTEXT" container inspect "$EXTERNAL_PG_CONTAINER" >/dev/null 2>&1; then
	fail "refusing to reuse pre-existing external PostgreSQL container $EXTERNAL_PG_CONTAINER; choose another E2E_RUN_ID"
fi
for task_image in \
	"${REMOTE_REGISTRY}/ptah-operator:${IMAGE_TAG}" \
	"${REMOTE_REGISTRY}/ptah-operator-next:${IMAGE_TAG}" \
	"${REMOTE_REGISTRY}/e2e-fixture:${IMAGE_TAG}" \
	"${REMOTE_REGISTRY}/ptah-executor:${IMAGE_TAG}" \
	"${REMOTE_REGISTRY}/ptah-runner:${IMAGE_TAG}" \
	"${REMOTE_REGISTRY}/postgresql:${IMAGE_TAG}" \
	"${REMOTE_REGISTRY}/mysql:${IMAGE_TAG}"; do
	if docker --context "$DOCKER_CONTEXT" image inspect "$task_image" >/dev/null 2>&1; then
		fail "refusing to overwrite pre-existing image $task_image; choose another E2E_RUN_ID"
	fi
done

mkdir -p "$TLS_PROXY_DIR"
chmod 700 "$TLS_PROXY_DIR"
printf '%s\n' \
	'[req]' \
	'distinguished_name=dn' \
	'x509_extensions=v3_ca' \
	'prompt=no' \
	'[dn]' \
	"CN=${CLUSTER_NAME}-registry-ca" \
	'[v3_ca]' \
	'basicConstraints=critical,CA:TRUE,pathlen:0' \
	'keyUsage=critical,keyCertSign,cRLSign' \
	>"$TLS_PROXY_CA_CONFIG_FILE"
if ! openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
	-out "$TLS_PROXY_CA_KEY_FILE" >/dev/null 2>&1 ||
	! openssl req -x509 -new -sha256 -days 2 \
	-key "$TLS_PROXY_CA_KEY_FILE" \
	-config "$TLS_PROXY_CA_CONFIG_FILE" \
	-extensions v3_ca \
	-out "$TLS_PROXY_CA_FILE" >/dev/null 2>&1 ||
	! openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
	-out "$TLS_PROXY_CERT_KEY_FILE" >/dev/null 2>&1 ||
	! openssl req -new -sha256 \
	-key "$TLS_PROXY_CERT_KEY_FILE" \
	-subj '/CN=ptah-e2e-registry' \
	-out "$TLS_PROXY_CERT_REQUEST_FILE" >/dev/null 2>&1; then
	fail "could not generate task-scoped TLS proxy key material"
fi
printf '%s\n' \
	'[server]' \
	'basicConstraints=critical,CA:FALSE' \
	'keyUsage=critical,digitalSignature' \
	'extendedKeyUsage=serverAuth' \
	"subjectAltName=DNS:${TLS_PROXY_DNS_NAME}" \
	>"$TLS_PROXY_CERT_EXT_FILE"
if ! openssl x509 -req -sha256 -days 2 \
	-in "$TLS_PROXY_CERT_REQUEST_FILE" \
	-CA "$TLS_PROXY_CA_FILE" \
	-CAkey "$TLS_PROXY_CA_KEY_FILE" \
	-set_serial 1 \
	-extfile "$TLS_PROXY_CERT_EXT_FILE" \
	-extensions server \
	-out "$TLS_PROXY_CERT_FILE" >/dev/null 2>&1; then
	fail "could not sign the task-scoped TLS proxy certificate"
fi
chmod 600 \
	"$TLS_PROXY_CA_KEY_FILE" "$TLS_PROXY_CA_FILE" \
	"$TLS_PROXY_CA_CONFIG_FILE" \
	"$TLS_PROXY_CERT_KEY_FILE" "$TLS_PROXY_CERT_REQUEST_FILE" \
	"$TLS_PROXY_CERT_FILE" "$TLS_PROXY_CERT_EXT_FILE"
if ! go -C "$ROOT_DIR" run ./test/e2e/handcraftoci verify-certificate \
	"$TLS_PROXY_CA_FILE" "$TLS_PROXY_CERT_FILE" "$TLS_PROXY_DNS_NAME" \
	>/dev/null 2>&1; then
	fail "task-scoped TLS proxy certificate does not bind its exact Service DNS name"
fi

mkdir -p "$PREDECESSOR_BUILD_CONTEXT"
git -C "$ROOT_DIR" archive --format=tar \
	--output="$PREDECESSOR_SOURCE_ARCHIVE" "$PREDECESSOR_REVISION"
tar -xf "$PREDECESSOR_SOURCE_ARCHIVE" -C "$PREDECESSOR_BUILD_CONTEXT"
[ -f "$PREDECESSOR_BUILD_CONTEXT/$PREDECESSOR_DOCKERFILE" ] ||
	fail "predecessor archive is missing $PREDECESSOR_DOCKERFILE"
[ -f "$PREDECESSOR_BUILD_CONTEXT/$PREDECESSOR_CHART/Chart.yaml" ] ||
	fail "predecessor archive is missing $PREDECESSOR_CHART/Chart.yaml"
predecessor_crd_count=$(jq -er '.crds | length' "$PREDECESSOR_IDENTITY_FILE")
[ "$predecessor_crd_count" -eq 3 ] ||
	fail "predecessor identity must contain exactly three CRDs"
predecessor_crd_index=0
while [ "$predecessor_crd_index" -lt "$predecessor_crd_count" ]; do
	predecessor_crd_path=$(jq -er ".crds[$predecessor_crd_index].path" "$PREDECESSOR_IDENTITY_FILE")
	predecessor_crd_digest=$(jq -er ".crds[$predecessor_crd_index].normalizedSpecDigest" "$PREDECESSOR_IDENTITY_FILE")
	case "$predecessor_crd_path" in
		/* | ../* | */../* | */..) fail "predecessor CRD path must stay inside its archive" ;;
	esac
	[ -f "$PREDECESSOR_BUILD_CONTEXT/$predecessor_crd_path" ] ||
		fail "predecessor archive is missing $predecessor_crd_path"
	actual_predecessor_digest=$(go -C "$ROOT_DIR" run ./hack/crdschemadigest \
		"$PREDECESSOR_BUILD_CONTEXT/$predecessor_crd_path")
	[ "$actual_predecessor_digest" = "$predecessor_crd_digest" ] ||
		fail "predecessor CRD $predecessor_crd_path digest is $actual_predecessor_digest, expected $predecessor_crd_digest"
	predecessor_crd_index=$((predecessor_crd_index + 1))
done

mkdir -p "$NEXT_BUILD_CONTEXT"
git -C "$ROOT_DIR" archive --format=tar \
	--output="$NEXT_SOURCE_ARCHIVE" "$CONTROLLER_REVISION"
tar -xf "$NEXT_SOURCE_ARCHIVE" -C "$NEXT_BUILD_CONTEXT"
NEXT_GO_SEQUENCE_FILE=$NEXT_BUILD_CONTEXT/internal/crdupgrade/rollout.go
NEXT_HELM_SEQUENCE_FILE=$NEXT_BUILD_CONTEXT/charts/ptah-operator/templates/_helpers.tpl
for next_sequence_source in "$NEXT_GO_SEQUENCE_FILE" "$NEXT_HELM_SEQUENCE_FILE"; do
	[ -f "$next_sequence_source" ] && [ ! -L "$next_sequence_source" ] ||
		fail "synthetic next-release sequence source must be a regular non-symlink file: $next_sequence_source"
done
CURRENT_GO_RELEASE_SEQUENCE=$(go_release_sequence_from_source "$NEXT_GO_SEQUENCE_FILE") ||
	fail "archived Go source must contain exactly one positive CurrentReleaseSequence declaration"
CURRENT_HELM_RELEASE_SEQUENCE=$(helm_release_sequence_from_source "$NEXT_HELM_SEQUENCE_FILE") ||
	fail "archived Helm source must contain exactly one positive releaseSequence helper"
[ "$CURRENT_GO_RELEASE_SEQUENCE" = "$CURRENT_HELM_RELEASE_SEQUENCE" ] ||
	fail "archived Go release sequence $CURRENT_GO_RELEASE_SEQUENCE differs from Helm sequence $CURRENT_HELM_RELEASE_SEQUENCE"
CURRENT_RELEASE_SEQUENCE=$CURRENT_GO_RELEASE_SEQUENCE
printf '%s\n' "$CURRENT_RELEASE_SEQUENCE" | grep -Eq '^[1-9][0-9]{0,9}$' ||
	fail "current release sequence must be a positive base-10 int32"
[ "$CURRENT_RELEASE_SEQUENCE" -le 2147483646 ] ||
	fail "current release sequence cannot be advanced within positive int32 bounds"
NEXT_RELEASE_SEQUENCE=$((CURRENT_RELEASE_SEQUENCE + 1))
current_go_sequence_line=$(printf '\tCurrentReleaseSequence int32 = %s' \
	"$CURRENT_RELEASE_SEQUENCE")
next_go_sequence_line=$(printf '\tCurrentReleaseSequence int32 = %s' \
	"$NEXT_RELEASE_SEQUENCE")
current_helm_sequence_line=$(printf \
	'{{- define "ptah-operator.releaseSequence" -}}%s{{- end -}}' \
	"$CURRENT_RELEASE_SEQUENCE")
next_helm_sequence_line=$(printf \
	'{{- define "ptah-operator.releaseSequence" -}}%s{{- end -}}' \
	"$NEXT_RELEASE_SEQUENCE")
replace_exact_line_once \
	"$NEXT_GO_SEQUENCE_FILE" \
	"$current_go_sequence_line" \
	"$next_go_sequence_line" \
	"synthetic next-release Go sequence $CURRENT_RELEASE_SEQUENCE to $NEXT_RELEASE_SEQUENCE"
replace_exact_line_once \
	"$NEXT_HELM_SEQUENCE_FILE" \
	"$current_helm_sequence_line" \
	"$next_helm_sequence_line" \
	"synthetic next-release Helm sequence $CURRENT_RELEASE_SEQUENCE to $NEXT_RELEASE_SEQUENCE"

chart_version=$(sed -n 's/^version: //p' "$ROOT_DIR/charts/ptah-operator/Chart.yaml")
[ -n "$chart_version" ] || fail "Helm chart version is missing"
chart_source_epoch=$(git -C "$ROOT_DIR" show -s --format=%ct "$CONTROLLER_REVISION")
printf '%s\n' "$chart_source_epoch" | grep -Eq '^[0-9]+$' ||
	fail "source commit does not have a valid release epoch"
printf 'e2e: reproducibly packaging Helm chart %s\n' "$chart_version"
go -C "$ROOT_DIR" run ./hack/chartpackage \
	-epoch "$chart_source_epoch" -destination "$CHART_PACKAGE_DIR"
CHART_PACKAGE="$CHART_PACKAGE_DIR/ptah-operator-${chart_version}.tgz"
[ -f "$CHART_PACKAGE" ] || fail "release chart package was not created: $CHART_PACKAGE"
packaged_chart_metadata=$(helm show chart "$CHART_PACKAGE")
packaged_chart_name=$(printf '%s\n' "$packaged_chart_metadata" |
	awk '$1 == "name:" {print $2}')
packaged_chart_version=$(printf '%s\n' "$packaged_chart_metadata" |
	awk '$1 == "version:" {print $2}')
[ "$packaged_chart_name" = ptah-operator ] ||
	fail "packaged chart name is $packaged_chart_name, want ptah-operator"
[ "$packaged_chart_version" = "$chart_version" ] ||
	fail "packaged chart version is $packaged_chart_version, want $chart_version"
CHART_PACKAGE_DIGEST=$(sha256 <"$CHART_PACKAGE")
chart_asset=${CHART_PACKAGE##*/}
printf 'e2e: installing release-form chart %s (%s)\n' \
	"$chart_asset" "$CHART_PACKAGE_DIGEST"

printf 'e2e: reproducibly packaging synthetic sequence-%s Helm chart from commit %s\n' \
	"$NEXT_RELEASE_SEQUENCE" "$CONTROLLER_REVISION"
go -C "$NEXT_BUILD_CONTEXT" run -mod=readonly ./hack/chartpackage \
	-chart charts/ptah-operator \
	-epoch "$chart_source_epoch" \
	-destination "$NEXT_CHART_PACKAGE_DIR"
NEXT_CHART_PACKAGE="$NEXT_CHART_PACKAGE_DIR/ptah-operator-${chart_version}.tgz"
[ -f "$NEXT_CHART_PACKAGE" ] ||
	fail "synthetic next-release chart package was not created: $NEXT_CHART_PACKAGE"
next_packaged_chart_metadata=$(helm show chart "$NEXT_CHART_PACKAGE")
next_packaged_chart_name=$(printf '%s\n' "$next_packaged_chart_metadata" |
	awk '$1 == "name:" {print $2}')
next_packaged_chart_version=$(printf '%s\n' "$next_packaged_chart_metadata" |
	awk '$1 == "version:" {print $2}')
[ "$next_packaged_chart_name" = ptah-operator ] ||
	fail "synthetic packaged chart name is $next_packaged_chart_name, want ptah-operator"
[ "$next_packaged_chart_version" = "$chart_version" ] ||
	fail "synthetic packaged chart version is $next_packaged_chart_version, want $chart_version"
NEXT_CHART_PACKAGE_DIGEST=$(sha256 <"$NEXT_CHART_PACKAGE")
printf 'e2e: synthetic sequence-%s chart %s has digest %s\n' \
	"$NEXT_RELEASE_SEQUENCE" "${NEXT_CHART_PACKAGE##*/}" "$NEXT_CHART_PACKAGE_DIGEST"

mkdir -p "$DOCKER_CLI_CONFIG/cli-plugins"
ln -s "$BUILDX_PLUGIN_PATH" "$DOCKER_CLI_CONFIG/cli-plugins/docker-buildx"
unset DOCKER_HOST
DOCKER_CONFIG=$DOCKER_CLI_CONFIG docker context create "$TASK_DOCKER_CONTEXT" \
	--docker "host=${DOCKER_ENDPOINT}" >/dev/null
export DOCKER_HOST="$DOCKER_ENDPOINT"
export DOCKER_CONFIG="$DOCKER_CLI_CONFIG"
DOCKER_CONTEXT=$TASK_DOCKER_CONTEXT
docker --context "$DOCKER_CONTEXT" buildx inspect "$DOCKER_CONTEXT" >/dev/null 2>&1 ||
	fail "isolated Docker config cannot use its task-scoped Buildx builder"

if ! docker --context "$DOCKER_CONTEXT" image inspect "$KIND_NODE_IMAGE" >/dev/null 2>&1; then
	KIND_NODE_IMAGE_CREATED=1
fi
if ! docker --context "$DOCKER_CONTEXT" network inspect kind >/dev/null 2>&1; then
	KIND_NETWORK_CREATED=1
fi

ensure_source_image() {
	source_image=$1
	if docker --context "$DOCKER_CONTEXT" image inspect "$source_image" >/dev/null 2>&1; then
		printf 'e2e: preserving and using cached source image %s\n' "$source_image"
		return
	fi
	printf 'e2e: pulling source image %s through Docker context %s\n' \
		"$source_image" "$SELECTED_DOCKER_CONTEXT"
	docker --context "$DOCKER_CONTEXT" pull "$source_image"
	add_created_image "$source_image"
}

PUSHED_IMAGE_REF=
push_task_image() {
	push_source=$1
	push_repository=$2
	push_target="${REMOTE_REGISTRY}/${push_repository}:${IMAGE_TAG}"
	add_created_image "$push_target"
	docker --context "$DOCKER_CONTEXT" tag "$push_source" "$push_target"
	push_output=$(docker --context "$DOCKER_CONTEXT" push "$push_target")
	printf '%s\n' "$push_output"
	push_digest=$(printf '%s\n' "$push_output" |
		sed -n 's/^.*digest: \(sha256:[0-9a-f]\{64\}\).*$/\1/p' | tail -n 1)
	printf '%s\n' "$push_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
		fail "could not determine the registry digest for $push_target"
	PUSHED_IMAGE_REF="${REGISTRY_HOST}/${push_repository}@${push_digest}"
}

mirror_task_image() {
	mirror_source=$1
	mirror_repository=$2
	ensure_source_image "$mirror_source"
	push_task_image "$mirror_source" "$mirror_repository"
}

sed "s/__API_SERVER_PORT__/${E2E_API_SERVER_PORT}/g" \
	"$ROOT_DIR/testdata/e2e/kind.yaml.tmpl" >"$KIND_CONFIG"
EXPECTED_API_SERVER_FEATURE_GATES=
append_api_server_feature_gate_patch() {
	feature_gate_minor=$1
	feature_gate_config=$2
	case "$feature_gate_minor" in
	1.35)
		EXPECTED_API_SERVER_FEATURE_GATES=GenericWorkload=true
		{
			printf '%s\n' 'kubeadmConfigPatchesJSON6902:'
			printf '%s\n' '- group: kubeadm.k8s.io'
			printf '%s\n' '  version: v1beta3'
			printf '%s\n' '  kind: ClusterConfiguration'
			printf '%s\n' '  patch: |'
			printf '%s\n' '    - op: add'
			printf '%s\n' '      path: /apiServer/extraArgs/feature-gates'
			printf '%s\n' '      value: GenericWorkload=true'
		} >>"$feature_gate_config"
		;;
	1.36) ;;
	1.37)
		EXPECTED_API_SERVER_FEATURE_GATES=EmptyDirVolumeMode=true,EvictionRequestAPI=true,GenericWorkload=true,VolumeBindMountOptions=true,WorkloadWithJob=true
		{
			printf '%s\n' 'kubeadmConfigPatchesJSON6902:'
			printf '%s\n' '- group: kubeadm.k8s.io'
			printf '%s\n' '  version: v1beta4'
			printf '%s\n' '  kind: ClusterConfiguration'
			printf '%s\n' '  patch: |'
			printf '%s\n' '    - op: add'
			printf '%s\n' '      path: /apiServer/extraArgs/-'
			printf '%s\n' '      value:'
			printf '%s\n' '        name: feature-gates'
			printf '%s\n' '        value: EmptyDirVolumeMode=true,EvictionRequestAPI=true,GenericWorkload=true,VolumeBindMountOptions=true,WorkloadWithJob=true'
		} >>"$feature_gate_config"
		;;
	esac
}
append_api_server_feature_gate_patch "$K8S_MAJOR_MINOR" "$KIND_CONFIG"

if [ "$E2E_DIRECT_HOST_ACCESS" -eq 0 ]; then
	ssh_args="-N -o BatchMode=yes -o ExitOnForwardFailure=yes"
	if [ -n "$E2E_SSH_PORT" ]; then
		ssh_args="$ssh_args -p $E2E_SSH_PORT"
	fi
	# shellcheck disable=SC2086 # ssh_args intentionally expands into separate options.
	ssh $ssh_args \
		-L "127.0.0.1:${E2E_API_SERVER_PORT}:127.0.0.1:${E2E_API_SERVER_PORT}" \
		-L "127.0.0.1:${E2E_REGISTRY_PORT}:127.0.0.1:${E2E_REGISTRY_PORT}" \
		"$E2E_SSH_TARGET" >"$TUNNEL_LOG" 2>&1 &
	TUNNEL_PID=$!
	sleep 1
	if ! kill -0 "$TUNNEL_PID" >/dev/null 2>&1; then
		cat "$TUNNEL_LOG" >&2
		fail "could not establish the Kubernetes API SSH tunnel"
	fi
fi

if [ -z "$E2E_EXECUTOR_IMAGE" ]; then
	if [ -z "$E2E_PTAH_SOURCE_DIR" ]; then
		if git -C "$ROOT_DIR/../ptah" rev-parse --git-dir >/dev/null 2>&1; then
			E2E_PTAH_SOURCE_DIR=$ROOT_DIR/../ptah
		else
			E2E_PTAH_SOURCE_DIR=$WORK_DIR/ptah-repository
			printf 'e2e: cloning Ptah source from %s\n' "$E2E_PTAH_GIT_URL"
			git clone --filter=blob:none --no-checkout "$E2E_PTAH_GIT_URL" "$E2E_PTAH_SOURCE_DIR"
		fi
	fi
	git -C "$E2E_PTAH_SOURCE_DIR" rev-parse --git-dir >/dev/null 2>&1 ||
		fail "E2E_PTAH_SOURCE_DIR must name a Ptah Git checkout"
	PTAH_COMMIT=$(git -C "$E2E_PTAH_SOURCE_DIR" rev-parse "${E2E_PTAH_REVISION}^{commit}")
	[ "$PTAH_COMMIT" = "$E2E_PTAH_REVISION" ] ||
		fail "Ptah revision resolved to $PTAH_COMMIT, expected exact commit $E2E_PTAH_REVISION"
	PTAH_SHORT_COMMIT=$(printf '%s' "$PTAH_COMMIT" | cut -c1-12)
	if [ -z "$E2E_PTAH_VERSION" ]; then
		E2E_PTAH_VERSION=$(git -C "$E2E_PTAH_SOURCE_DIR" describe --tags --always "$PTAH_COMMIT")
	fi
	PTAH_BUILD_DATE=$(git -C "$E2E_PTAH_SOURCE_DIR" show -s --format=%cI "$PTAH_COMMIT")
	PTAH_SOURCE_ARCHIVE=$WORK_DIR/ptah-source.tar
	PTAH_BUILD_CONTEXT=$WORK_DIR/ptah-source
	mkdir -p "$PTAH_BUILD_CONTEXT"
	git -C "$E2E_PTAH_SOURCE_DIR" archive --format=tar \
		--output="$PTAH_SOURCE_ARCHIVE" "$PTAH_COMMIT"
	tar -xf "$PTAH_SOURCE_ARCHIVE" -C "$PTAH_BUILD_CONTEXT"
	cp "$ROOT_DIR/test/e2e/Dockerfile.ptah" "$PTAH_BUILD_CONTEXT/Dockerfile.e2e"
	printf 'e2e: building Ptah executor %s from commit %s\n' "$PTAH_IMAGE" "$PTAH_COMMIT"
	add_created_image "$PTAH_IMAGE"
	docker --context "$DOCKER_CONTEXT" buildx build \
		--builder "$DOCKER_CONTEXT" \
		--load \
		--file "$PTAH_BUILD_CONTEXT/Dockerfile.e2e" \
		--build-arg "PTAH_BUILD_VERSION=${E2E_PTAH_VERSION}" \
		--build-arg "PTAH_BUILD_COMMIT=${PTAH_SHORT_COMMIT}" \
		--build-arg "PTAH_BUILD_DATE=${PTAH_BUILD_DATE}" \
		--tag "$PTAH_IMAGE" "$PTAH_BUILD_CONTEXT"
	EXECUTOR_SOURCE_IMAGE=$PTAH_IMAGE
else
	EXECUTOR_SOURCE_IMAGE=$E2E_EXECUTOR_IMAGE
fi
if [ -n "$E2E_RUNNER_IMAGE" ]; then
	RUNNER_SOURCE_IMAGE=$E2E_RUNNER_IMAGE
else
	RUNNER_SOURCE_IMAGE=$OPERATOR_IMAGE
fi

printf 'e2e: building %s with Docker context %s\n' "$OPERATOR_IMAGE" "$SELECTED_DOCKER_CONTEXT"
IMAGE_CREATED=1
docker --context "$DOCKER_CONTEXT" buildx build \
	--builder "$DOCKER_CONTEXT" \
	--load \
	--file "$ROOT_DIR/test/e2e/Dockerfile.operator" \
	--build-arg "REVISION=$CONTROLLER_REVISION" \
	--target operator \
	--tag "$OPERATOR_IMAGE" "$ROOT_DIR"
printf 'e2e: building synthetic sequence-%s image %s from exact commit archive %s\n' \
	"$NEXT_RELEASE_SEQUENCE" "$NEXT_OPERATOR_IMAGE" "$CONTROLLER_REVISION"
add_created_image "$NEXT_OPERATOR_IMAGE"
docker --context "$DOCKER_CONTEXT" buildx build \
	--builder "$DOCKER_CONTEXT" \
	--load \
	--file "$NEXT_BUILD_CONTEXT/test/e2e/Dockerfile.operator" \
	--build-arg "REVISION=$CONTROLLER_REVISION" \
	--target operator \
	--tag "$NEXT_OPERATOR_IMAGE" "$NEXT_BUILD_CONTEXT"
printf 'e2e: building predecessor image %s from exact commit %s\n' \
	"$PREDECESSOR_OPERATOR_IMAGE" "$PREDECESSOR_REVISION"
add_created_image "$PREDECESSOR_OPERATOR_IMAGE"
docker --context "$DOCKER_CONTEXT" buildx build \
	--builder "$DOCKER_CONTEXT" \
	--load \
	--file "$PREDECESSOR_BUILD_CONTEXT/$PREDECESSOR_DOCKERFILE" \
	--build-arg "REVISION=$PREDECESSOR_REVISION" \
	--tag "$PREDECESSOR_OPERATOR_IMAGE" "$PREDECESSOR_BUILD_CONTEXT"
predecessor_image_revision=$(docker --context "$DOCKER_CONTEXT" image inspect \
	--format '{{index .Config.Labels "org.opencontainers.image.revision"}}' \
	"$PREDECESSOR_OPERATOR_IMAGE")
[ "$predecessor_image_revision" = "$PREDECESSOR_REVISION" ] ||
	fail "predecessor image revision is $predecessor_image_revision, expected $PREDECESSOR_REVISION"
add_created_image "$FIXTURE_BUILD_IMAGE"
docker --context "$DOCKER_CONTEXT" buildx build \
	--builder "$DOCKER_CONTEXT" \
	--load \
	--file "$ROOT_DIR/test/e2e/Dockerfile.operator" \
	--build-arg "REVISION=$CONTROLLER_REVISION" \
	--target fixture \
	--tag "$FIXTURE_BUILD_IMAGE" "$ROOT_DIR"

create_image_audit_container "$OPERATOR_IMAGE"
docker --context "$DOCKER_CONTEXT" export "$IMAGE_AUDIT_CONTAINER_ID" >"$IMAGE_AUDIT_ARCHIVE"
if tar -tf "$IMAGE_AUDIT_ARCHIVE" | grep -Eq '(^|/)e2e-handcraft-oci$'; then
	fail "the controller image contains the test-only OCI publisher"
fi
tar -tf "$IMAGE_AUDIT_ARCHIVE" | grep -Eq '(^|/)manager$' ||
	fail "the controller image does not contain /manager"
tar -tf "$IMAGE_AUDIT_ARCHIVE" | grep -Eq '(^|/)ptah-runner$' ||
	fail "the controller image does not contain /ptah-runner"
tar -tf "$IMAGE_AUDIT_ARCHIVE" | grep -Eq '(^|/)ptah-crd-manager$' ||
	fail "the controller image does not contain /ptah-crd-manager"
remove_image_audit_container

create_image_audit_container "$FIXTURE_BUILD_IMAGE"
docker --context "$DOCKER_CONTEXT" export "$IMAGE_AUDIT_CONTAINER_ID" >"$IMAGE_AUDIT_ARCHIVE"
tar -tf "$IMAGE_AUDIT_ARCHIVE" | grep -Eq '(^|/)e2e-handcraft-oci$' ||
	fail "the isolated fixture image does not contain /e2e-handcraft-oci"
if tar -tf "$IMAGE_AUDIT_ARCHIVE" | grep -Eq '(^|/)(manager|ptah-runner)$'; then
	fail "the isolated fixture image contains an operator binary"
fi
remove_image_audit_container

printf 'e2e: creating kind cluster %s with Kubernetes %s\n' "$CLUSTER_NAME" "$K8S_VERSION"
CLUSTER_CREATED=1
kind create cluster \
	--name "$CLUSTER_NAME" \
	--image "$KIND_NODE_IMAGE" \
	--config "$KIND_CONFIG" \
	--kubeconfig "$KUBECONFIG_FILE" \
	--wait 5m
require_ready_nodes "after kind cluster creation"
assert_kind_ha_topology
assert_api_server_endpoint_inventory
assert_api_server_feature_gate_scope "$EXPECTED_API_SERVER_FEATURE_GATES"

kind load docker-image "$PREDECESSOR_OPERATOR_IMAGE" --name "$CLUSTER_NAME"

server_version=$(kubectl --kubeconfig "$KUBECONFIG_FILE" version -o json |
	jq -r '.serverVersion.gitVersion')
case "$server_version" in
	v"$K8S_VERSION"*) ;;
	*) fail "cluster reports $server_version, expected v$K8S_VERSION" ;;
esac
client_version=$(kubectl version --client -o json | jq -r '.clientVersion.gitVersion')
client_major=$(printf '%s\n' "$client_version" | sed -n 's/^v\([0-9][0-9]*\)\..*$/\1/p')
client_minor=$(printf '%s\n' "$client_version" | sed -n 's/^v[0-9][0-9]*\.\([0-9][0-9]*\)\..*$/\1/p')
server_major=$(printf '%s\n' "$K8S_VERSION" | cut -d. -f1)
server_minor=$(printf '%s\n' "$K8S_VERSION" | cut -d. -f2)
if [ -z "$client_major" ] || [ -z "$client_minor" ]; then
	fail "could not parse kubectl client version $client_version"
fi
[ "$client_major" -eq "$server_major" ] ||
	fail "kubectl $client_version has a different major version than Kubernetes $K8S_VERSION"
minor_skew=$((client_minor - server_minor))
if [ "$minor_skew" -lt -1 ] || [ "$minor_skew" -gt 1 ]; then
	fail "kubectl $client_version is outside the supported one-minor skew for Kubernetes $K8S_VERSION"
fi

ADMISSION_OPENAPI_FILE=$WORK_DIR/admissionregistration-openapi-v3.json
kubectl --kubeconfig "$KUBECONFIG_FILE" get --raw \
	/openapi/v3/apis/admissionregistration.k8s.io/v1 >"$ADMISSION_OPENAPI_FILE"
jq -e -f "$ROOT_DIR/hack/admission-schema-contract.jq" \
	"$ADMISSION_OPENAPI_FILE" >/dev/null ||
	fail "Kubernetes $K8S_VERSION admission schema exceeds the frozen certificate write boundary"

CONTROLLER_BATCH_OPENAPI_FILE=$WORK_DIR/controller-batch-openapi-v3.json
CONTROLLER_CORE_OPENAPI_FILE=$WORK_DIR/controller-core-openapi-v3.json
kubectl --kubeconfig "$KUBECONFIG_FILE" get --raw \
	/openapi/v3/apis/batch/v1 >"$CONTROLLER_BATCH_OPENAPI_FILE"
kubectl --kubeconfig "$KUBECONFIG_FILE" get --raw \
	/openapi/v3/api/v1 >"$CONTROLLER_CORE_OPENAPI_FILE"
jq -e \
	--arg minor "${server_major}.${server_minor}" \
	--slurpfile core "$CONTROLLER_CORE_OPENAPI_FILE" \
	-f "$ROOT_DIR/hack/controller-object-schema-contract.jq" \
	"$CONTROLLER_BATCH_OPENAPI_FILE" >/dev/null ||
	fail "Kubernetes $K8S_VERSION Job/Pod API exceeds the reviewed controller write boundary"

ensure_source_image "$E2E_REGISTRY_IMAGE"
printf 'e2e: starting isolated registry %s on Docker context %s\n' \
	"$REGISTRY_CONTAINER" "$SELECTED_DOCKER_CONTEXT"
REGISTRY_CREATED=1
umask 077
if ! printf '%s\n' "$REGISTRY_PASSWORD" | \
	htpasswd -Bni "$REGISTRY_USERNAME" >"$REGISTRY_HTPASSWD_FILE"; then
	fail "could not create task-scoped registry authentication data"
fi
docker --context "$DOCKER_CONTEXT" create --restart=no \
	--name "$REGISTRY_CONTAINER" \
	--network kind \
	--network-alias "$REGISTRY_DNS_NAME" \
	--publish "127.0.0.1:${E2E_REGISTRY_PORT}:5000" \
	--label "operator.ptah.dev/e2e-owner=${CLUSTER_NAME}" \
	--label 'operator.ptah.dev/e2e-component=registry' \
	--env REGISTRY_AUTH=htpasswd \
	--env REGISTRY_AUTH_HTPASSWD_REALM=ptah-e2e \
	--env REGISTRY_AUTH_HTPASSWD_PATH=/registry.htpasswd \
	"$E2E_REGISTRY_IMAGE" >/dev/null
docker --context "$DOCKER_CONTEXT" cp "$REGISTRY_HTPASSWD_FILE" \
	"${REGISTRY_CONTAINER}:/registry.htpasswd"
REGISTRY_CONTAINER_ID=$(docker --context "$DOCKER_CONTEXT" container inspect \
	--format '{{.Id}}' "$REGISTRY_CONTAINER")
printf '%s\n' "$REGISTRY_CONTAINER_ID" | grep -Eq '^[0-9a-f]{64}$' ||
	fail "registry container does not have an exact Docker ID"
docker --context "$DOCKER_CONTEXT" start "$REGISTRY_CONTAINER" >/dev/null

registry_deadline=$(($(date +%s) + 60))
registry_ready=0
while [ "$(date +%s)" -lt "$registry_deadline" ]; do
	registry_status=$(curl --silent --output /dev/null --write-out '%{http_code}' \
		"http://${REMOTE_REGISTRY}/v2/" 2>/dev/null || true)
	if [ "$registry_status" = 401 ]; then
		registry_ready=1
		break
	fi
	sleep 1
done
[ "$registry_ready" -eq 1 ] || fail "authenticated registry did not become ready"
printf 'machine 127.0.0.1 login %s password %s\n' \
	"$REGISTRY_USERNAME" "$REGISTRY_PASSWORD" >"$REGISTRY_NETRC_FILE"
authenticated_status=$(curl --silent --output /dev/null --write-out '%{http_code}' \
	--netrc-file "$REGISTRY_NETRC_FILE" "http://${REMOTE_REGISTRY}/v2/")
[ "$authenticated_status" = 200 ] ||
	fail "registry refused its task-scoped credentials"
printf '%s' "$REGISTRY_PASSWORD" | docker --context "$DOCKER_CONTEXT" login \
	"$REMOTE_REGISTRY" --username "$REGISTRY_USERNAME" --password-stdin >/dev/null
REGISTRY_IP=$(docker --context "$DOCKER_CONTEXT" container inspect \
	--format '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' \
	"$REGISTRY_CONTAINER")
printf '%s\n' "$REGISTRY_IP" | grep -Eq '^[0-9]+(\.[0-9]+){3}$' ||
	fail "registry container has no IPv4 address on the kind network"

printf 'server = "http://%s"\n\n[host."http://%s"]\n  capabilities = ["pull", "resolve"]\n' \
	"$REGISTRY_HOST" "$REGISTRY_HOST" >"$REGISTRY_HOSTS_FILE"
configure_registry_hosts_on_kind_nodes

printf '%s\n' 'e2e: mirroring immutable execution and database images into the isolated registry'
push_task_image "$OPERATOR_IMAGE" ptah-operator
CANDIDATE_OPERATOR_IMAGE=$PUSHED_IMAGE_REF
CANDIDATE_OPERATOR_REPOSITORY=${CANDIDATE_OPERATOR_IMAGE%@*}
CANDIDATE_OPERATOR_DIGEST=${CANDIDATE_OPERATOR_IMAGE#*@}
[ "$CANDIDATE_OPERATOR_REPOSITORY" != "$CANDIDATE_OPERATOR_IMAGE" ] ||
	fail "candidate operator image is not repository-and-digest pinned"
printf '%s\n' "$CANDIDATE_OPERATOR_DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
	fail "candidate operator image digest is invalid"
push_task_image "$NEXT_OPERATOR_IMAGE" ptah-operator-next
NEXT_CONTROLLER_IMAGE=$PUSHED_IMAGE_REF
NEXT_CONTROLLER_REPOSITORY=${NEXT_CONTROLLER_IMAGE%@*}
NEXT_CONTROLLER_DIGEST=${NEXT_CONTROLLER_IMAGE#*@}
[ "$NEXT_CONTROLLER_REPOSITORY" != "$NEXT_CONTROLLER_IMAGE" ] ||
	fail "synthetic next-release operator image is not repository-and-digest pinned"
printf '%s\n' "$NEXT_CONTROLLER_DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
	fail "synthetic next-release operator image digest is invalid"
mirror_task_image "$FIXTURE_BUILD_IMAGE" e2e-fixture
E2E_FIXTURE_IMAGE=$PUSHED_IMAGE_REF
mirror_task_image "$EXECUTOR_SOURCE_IMAGE" ptah-executor
E2E_EXECUTOR_IMAGE=$PUSHED_IMAGE_REF
mirror_task_image "$RUNNER_SOURCE_IMAGE" ptah-runner
E2E_RUNNER_IMAGE=$PUSHED_IMAGE_REF
mirror_task_image "$E2E_POSTGRES_SOURCE_IMAGE" postgresql
E2E_POSTGRES_IMAGE=$PUSHED_IMAGE_REF
mirror_task_image "$E2E_MYSQL_SOURCE_IMAGE" mysql
E2E_MYSQL_IMAGE=$PUSHED_IMAGE_REF

# external-postgresql-container-create-begin
printf 'e2e: starting external PostgreSQL container %s without host ports\n' \
	"$EXTERNAL_PG_CONTAINER"
EXTERNAL_PG_CREATED=1
docker --context "$DOCKER_CONTEXT" create --restart=no \
	--name "$EXTERNAL_PG_CONTAINER" \
	--network kind \
	--env-file "$EXTERNAL_PG_ENV_FILE" \
	--tmpfs '/var/lib/postgresql/data:rw,noexec,nosuid,nodev,size=536870912' \
	--label "operator.ptah.dev/e2e-owner=${CLUSTER_NAME}" \
	--label 'operator.ptah.dev/e2e-component=external-postgresql' \
	"$E2E_POSTGRES_SOURCE_IMAGE" >/dev/null
# external-postgresql-container-create-end
EXTERNAL_PG_CONTAINER_ID=$(docker --context "$DOCKER_CONTEXT" container inspect \
	--format '{{.Id}}' "$EXTERNAL_PG_CONTAINER")
printf '%s\n' "$EXTERNAL_PG_CONTAINER_ID" | grep -Eq '^[0-9a-f]{64}$' ||
	fail "external PostgreSQL container does not have an exact Docker ID"
docker --context "$DOCKER_CONTEXT" start "$EXTERNAL_PG_CONTAINER_ID" >/dev/null
EXTERNAL_PG_IP=$(docker --context "$DOCKER_CONTEXT" container inspect \
	--format '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' \
	"$EXTERNAL_PG_CONTAINER_ID")
printf '%s\n' "$EXTERNAL_PG_IP" | grep -Eq '^[0-9]+(\.[0-9]+){3}$' ||
	fail "external PostgreSQL container has no IPv4 address on the kind network"
assert_external_pg_container_contract "$EXTERNAL_PG_CONTAINER_ID" "$EXTERNAL_PG_IP"
external_pg_deadline=$(($(date +%s) + 90))
external_pg_ready=0
while [ "$(date +%s)" -lt "$external_pg_deadline" ]; do
	if docker --context "$DOCKER_CONTEXT" exec "$EXTERNAL_PG_CONTAINER_ID" \
		sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" pg_isready -q -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
		>/dev/null 2>&1; then
		external_pg_ready=1
		break
	fi
	sleep 2
done
[ "$external_pg_ready" -eq 1 ] || fail "external PostgreSQL container did not become queryable"
external_pg_server_version_num=$(docker --context "$DOCKER_CONTEXT" exec "$EXTERNAL_PG_CONTAINER_ID" \
	sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atqc "SHOW server_version_num"')
external_pg_server_version_num=$(printf '%s' "$external_pg_server_version_num" | tr -d '[:space:]')
printf '%s\n' "$external_pg_server_version_num" | grep -Eq '^17[0-9]{4}$' ||
	fail "external PostgreSQL fixture is not major version 17"
docker --context "$DOCKER_CONTEXT" exec -i "$EXTERNAL_PG_CONTAINER_ID" \
	sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1' \
	<"$EXTERNAL_PG_BOOTSTRAP_SQL_FILE" >/dev/null
rm -f "$EXTERNAL_PG_ADMIN_PASSWORD_FILE" "$EXTERNAL_PG_BOOTSTRAP_SQL_FILE" \
	"$EXTERNAL_PG_ENV_FILE"
external_pg_least_privileged=$(external_pg_app_query '
  SELECT NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND
         NOT rolreplication AND NOT rolbypassrls
  FROM pg_roles WHERE rolname = current_user
')
external_pg_least_privileged=$(printf '%s' "$external_pg_least_privileged" | tr -d '[:space:]')
[ "$external_pg_least_privileged" = t ] ||
	fail "external PostgreSQL fixture login retained administrative attributes"
external_pg_database_owner=$(external_pg_app_query \
	'SELECT pg_get_userbyid(datdba) = current_user FROM pg_database WHERE datname = current_database()')
external_pg_database_owner=$(printf '%s' "$external_pg_database_owner" | tr -d '[:space:]')
[ "$external_pg_database_owner" = t ] ||
	fail "external PostgreSQL fixture login does not own its database"
assert_external_pg_container_contract "$EXTERNAL_PG_CONTAINER_ID" "$EXTERNAL_PG_IP"

render_release_values() {
	values_destination=$1
	values_image_repository=$2
	values_image_tag=$3
	values_image_digest=$4
	values_pull_secret=${5:-}
	jq -n \
		--arg fullnameOverride "$RUNTIME_FULLNAME" \
		--arg repository "$values_image_repository" \
		--arg tag "$values_image_tag" \
		--arg digest "$values_image_digest" \
		--arg pullSecret "$values_pull_secret" \
		--arg executorImage "$E2E_EXECUTOR_IMAGE" \
		--arg runnerImage "$E2E_RUNNER_IMAGE" \
		--arg ptahVersion "$E2E_PTAH_VERSION" '
      {
        fullnameOverride: $fullnameOverride,
        image: {
          repository: $repository,
          tag: $tag,
          allowMutableTag: true,
          pullPolicy: "Never"
        },
        execution: {
          executorImage: $executorImage,
          runnerImage: $runnerImage,
          ptahVersion: $ptahVersion
        },
        certificateRotation: {
          interval: "168h",
          recreateMissingSecret: true
        },
        replicaCount: 2,
        podDisruptionBudget: {enabled: false}
      }
      | if $digest == "" then . else
          .image.digest = $digest
          | .image.allowMutableTag = false
          | .image.pullPolicy = "IfNotPresent"
          | .imagePullSecrets = [{name: $pullSecret}]
        end
    ' >"$values_destination"
}

render_release_values \
	"$PREDECESSOR_VALUES_FILE" "$PREDECESSOR_IMAGE_REPOSITORY" "$IMAGE_TAG" ""
render_release_values \
	"$CANDIDATE_VALUES_FILE" "$CANDIDATE_OPERATOR_REPOSITORY" "$IMAGE_TAG" \
	"$CANDIDATE_OPERATOR_DIGEST" "$MANAGER_PULL_SECRET"
render_release_values \
	"$NEXT_VALUES_FILE" "$NEXT_CONTROLLER_REPOSITORY" "$IMAGE_TAG" \
	"$NEXT_CONTROLLER_DIGEST" "$MANAGER_PULL_SECRET"

printf 'e2e: installing exact predecessor release %s/%s from %s\n' \
	"$OPERATOR_NAMESPACE" "$HELM_RELEASE" "$PREDECESSOR_REVISION"
require_ready_nodes "immediately before predecessor Helm install"
if command helm --kubeconfig "$KUBECONFIG_FILE" install "$HELM_RELEASE" \
	"$PREDECESSOR_BUILD_CONTEXT/$PREDECESSOR_CHART" \
	--namespace "$OPERATOR_NAMESPACE" \
	--create-namespace \
	--wait \
	--timeout 5m \
	--values "$PREDECESSOR_VALUES_FILE"; then
	:
else
	predecessor_install_status=$?
	if nodes_ready_now; then
		fail "predecessor release installation failed while Kubernetes nodes were Ready at the immediate post-failure check (Helm exit $predecessor_install_status)"
	fi
	collect_node_readiness_diagnostics "immediately after predecessor Helm install failed"
	fail "infrastructure readiness loss: predecessor release installation failed and node readiness was absent or unqueryable immediately afterward (Helm exit $predecessor_install_status)"
fi

jq -n \
	--arg name "$MANAGER_PULL_SECRET" \
	--arg namespace "$OPERATOR_NAMESPACE" \
	--arg registry "$REGISTRY_HOST" \
	--arg username "$REGISTRY_USERNAME" \
	--arg password "$REGISTRY_PASSWORD" '
  {
    apiVersion: "v1",
    kind: "Secret",
    metadata: {name: $name, namespace: $namespace},
    immutable: true,
    type: "kubernetes.io/dockerconfigjson",
    data: {
      ".dockerconfigjson": ({
        auths: {($registry): {
          username: $username,
          password: $password,
          auth: (($username + ":" + $password) | @base64)
        }}
      } | tojson | @base64)
    }
  }
' | kubectl --kubeconfig "$KUBECONFIG_FILE" apply -f - >/dev/null

E2E_KUBECONFIG=$KUBECONFIG_FILE \
E2E_OPERATOR_NAMESPACE=$OPERATOR_NAMESPACE \
E2E_PROOF_NAMESPACE=$CRD_PROOF_NAMESPACE \
E2E_HELM_RELEASE=$HELM_RELEASE \
E2E_CHART_PACKAGE=$CHART_PACKAGE \
E2E_CANDIDATE_VALUES_FILE=$CANDIDATE_VALUES_FILE \
E2E_PREDECESSOR_IDENTITY_FILE=$PREDECESSOR_IDENTITY_FILE \
E2E_PREDECESSOR_SOURCE_DIR=$PREDECESSOR_BUILD_CONTEXT \
E2E_PREDECESSOR_IMAGE=$PREDECESSOR_OPERATOR_IMAGE \
E2E_CANDIDATE_IMAGE=$CANDIDATE_OPERATOR_IMAGE \
E2E_KUBERNETES_VERSION=$K8S_VERSION \
E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \
E2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \
E2E_EXTERNAL_POSTGRES_CONTAINER_ID=$EXTERNAL_PG_CONTAINER_ID \
E2E_EXTERNAL_POSTGRES_IP=$EXTERNAL_PG_IP \
E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE=$EXTERNAL_PG_CREDENTIALS_FILE \
E2E_PHASE=upgrade \
	"$ROOT_DIR/hack/e2e-crd-upgrade.sh"

E2E_KUBECONFIG=$KUBECONFIG_FILE \
E2E_OPERATOR_NAMESPACE=$OPERATOR_NAMESPACE \
E2E_HA_TEST_NAMESPACE=$HA_TEST_NAMESPACE \
E2E_FOREIGN_NAMESPACE=$FOREIGN_NAMESPACE \
E2E_PROOF_NAMESPACE=$CRD_PROOF_NAMESPACE \
E2E_HELM_RELEASE=$HELM_RELEASE \
	"$ROOT_DIR/hack/e2e-ha.sh"

E2E_KUBECONFIG=$KUBECONFIG_FILE \
E2E_OPERATOR_NAMESPACE=$OPERATOR_NAMESPACE \
E2E_TEST_NAMESPACE=$TEST_NAMESPACE \
E2E_FOREIGN_NAMESPACE=$FOREIGN_NAMESPACE \
E2E_HELM_RELEASE=$HELM_RELEASE \
E2E_EXECUTOR_IMAGE=$E2E_EXECUTOR_IMAGE \
E2E_RUNNER_IMAGE=$E2E_RUNNER_IMAGE \
E2E_PTAH_VERSION=$E2E_PTAH_VERSION \
E2E_CONTROLLER_IMAGE=$CANDIDATE_OPERATOR_IMAGE \
E2E_CONTROLLER_REVISION=$CONTROLLER_REVISION \
E2E_CONTROLLER_STATE_VERSION=1 \
	"$ROOT_DIR/hack/e2e-assert.sh"

E2E_KUBECONFIG=$KUBECONFIG_FILE \
E2E_OPERATOR_NAMESPACE=$OPERATOR_NAMESPACE \
E2E_TEST_NAMESPACE=$TEST_NAMESPACE \
E2E_HELM_RELEASE=$HELM_RELEASE \
E2E_CHART_PACKAGE=$CHART_PACKAGE \
	"$ROOT_DIR/hack/e2e-cert-rotation.sh"

E2E_KUBECONFIG=$KUBECONFIG_FILE \
E2E_OPERATOR_NAMESPACE=$OPERATOR_NAMESPACE \
E2E_TEST_NAMESPACE=$TEST_NAMESPACE \
E2E_HELM_RELEASE=$HELM_RELEASE \
E2E_CHART_PACKAGE=$CHART_PACKAGE \
E2E_PTAH_VERSION=$E2E_PTAH_VERSION \
E2E_EXECUTOR_IMAGE=$E2E_EXECUTOR_IMAGE \
E2E_RUNNER_IMAGE=$E2E_RUNNER_IMAGE \
E2E_FIXTURE_IMAGE=$E2E_FIXTURE_IMAGE \
E2E_CONTROLLER_IMAGE=$CANDIDATE_OPERATOR_IMAGE \
E2E_CONTROLLER_REVISION=$CONTROLLER_REVISION \
E2E_CONTROLLER_STATE_VERSION=1 \
E2E_POSTGRES_IMAGE=$E2E_POSTGRES_IMAGE \
E2E_MYSQL_IMAGE=$E2E_MYSQL_IMAGE \
E2E_REGISTRY_IP=$REGISTRY_IP \
E2E_REGISTRY_SERVICE=$REGISTRY_SERVICE \
E2E_REGISTRY_PORT=$E2E_REGISTRY_PORT \
E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \
E2E_DOCKER_CONTEXT=$DOCKER_CONTEXT \
E2E_REGISTRY_CONTAINER_ID=$REGISTRY_CONTAINER_ID \
E2E_EXTERNAL_POSTGRES_CONTAINER_ID=$EXTERNAL_PG_CONTAINER_ID \
E2E_EXTERNAL_POSTGRES_IP=$EXTERNAL_PG_IP \
E2E_EXTERNAL_POSTGRES_SERVICE=$EXTERNAL_PG_SERVICE \
E2E_EXTERNAL_POSTGRES_IMAGE=$E2E_POSTGRES_SOURCE_IMAGE \
E2E_EXTERNAL_POSTGRES_OWNER=$CLUSTER_NAME \
E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE=$EXTERNAL_PG_CREDENTIALS_FILE \
E2E_TLS_PROXY_SERVICE=$TLS_PROXY_SERVICE \
E2E_TLS_PROXY_CA_FILE=$TLS_PROXY_CA_FILE \
E2E_TLS_PROXY_CERT_FILE=$TLS_PROXY_CERT_FILE \
E2E_TLS_PROXY_KEY_FILE=$TLS_PROXY_CERT_KEY_FILE \
	"$ROOT_DIR/hack/e2e-dataplane.sh"

E2E_KUBECONFIG=$KUBECONFIG_FILE \
E2E_OPERATOR_NAMESPACE=$OPERATOR_NAMESPACE \
E2E_PROOF_NAMESPACE=$CRD_PROOF_NAMESPACE \
E2E_HELM_RELEASE=$HELM_RELEASE \
E2E_CHART_PACKAGE=$CHART_PACKAGE \
E2E_CANDIDATE_VALUES_FILE=$CANDIDATE_VALUES_FILE \
E2E_CANDIDATE_IMAGE=$CANDIDATE_OPERATOR_IMAGE \
E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE \
E2E_NEXT_VALUES_FILE=$NEXT_VALUES_FILE \
E2E_NEXT_CONTROLLER_IMAGE=$NEXT_CONTROLLER_IMAGE \
E2E_CURRENT_RELEASE_SEQUENCE=$CURRENT_RELEASE_SEQUENCE \
E2E_NEXT_RELEASE_SEQUENCE=$NEXT_RELEASE_SEQUENCE \
E2E_KUBERNETES_VERSION=$K8S_VERSION \
E2E_PHASE=uninstall \
	"$ROOT_DIR/hack/e2e-crd-upgrade.sh"

export_release_chart
printf 'e2e: PASS Kubernetes=%s cluster=%s\n' "$server_version" "$CLUSTER_NAME"
