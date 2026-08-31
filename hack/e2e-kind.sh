#!/bin/sh

set -eu

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)

DOCKER_CONTEXT=${DOCKER_CONTEXT:-remote-dev-container}
K8S_VERSION=${K8S_VERSION:-}
KIND_NODE_IMAGE=${KIND_NODE_IMAGE:-}
E2E_EXECUTOR_IMAGE=${E2E_EXECUTOR_IMAGE:-}
E2E_RUNNER_IMAGE=${E2E_RUNNER_IMAGE:-}
E2E_PTAH_VERSION=${E2E_PTAH_VERSION:-}
E2E_PTAH_SOURCE_DIR=${E2E_PTAH_SOURCE_DIR:-}
E2E_PTAH_REVISION=${E2E_PTAH_REVISION:-044287f93d7f606a33541ca32affdaa0d20b9cb1}
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

# An imported variable retains its export attribute after reassignment in POSIX
# shells. Clear secret-bearing names before generating task credentials so no
# later host subprocess can inherit their values.
unset REGISTRY_PASSWORD

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

for command_name in docker kind kubectl helm jq ssh git go tar awk sed grep tr cut cksum mktemp date sleep curl htpasswd; do
	require_command "$command_name"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	fail "sha256sum or shasum is required"
fi

[ -n "$K8S_VERSION" ] || fail "K8S_VERSION is required (for example, 1.37.0)"
printf '%s\n' "$K8S_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||
	fail "K8S_VERSION must be an exact major.minor.patch version"
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
HA_TEST_NAMESPACE=$(dns_name ptah-test-ha "$identity")
TEST_NAMESPACE=$(dns_name ptah-test-a "$identity")
FOREIGN_NAMESPACE=$(dns_name ptah-test-b "$identity")
# Leave room for the chart name and its longest resource suffix.
HELM_RELEASE=$(dns_name ptah "$identity" 35)
IMAGE_REPOSITORY=ptah-operator-e2e.local/ptah-operator
IMAGE_TAG=$(printf '%s' "$identity" | sha256 | cut -c1-16)
OPERATOR_IMAGE="${IMAGE_REPOSITORY}:${IMAGE_TAG}"
FIXTURE_BUILD_IMAGE="ptah-operator-e2e.local/e2e-fixture:${IMAGE_TAG}"
PTAH_IMAGE="ptah-executor-e2e.local/ptah:${IMAGE_TAG}"
REGISTRY_CONTAINER=$(dns_name ptah-registry "$identity" 63)
REGISTRY_SERVICE=e2e-registry
REGISTRY_DNS_NAME="${REGISTRY_SERVICE}.${TEST_NAMESPACE}.svc.cluster.local"
REGISTRY_HOST="${REGISTRY_DNS_NAME}:5000"
REMOTE_REGISTRY=127.0.0.1
REGISTRY_USERNAME=ptah_e2e
registry_credential_suffix=$(printf '%s-registry-auth' "$identity" | sha256 | cut -c1-24)
REGISTRY_PASSWORD="e2eRegistry${registry_credential_suffix}Q7"

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

if ! existing_clusters=$(kind get clusters); then
	fail "could not list kind clusters through Docker context $DOCKER_CONTEXT"
fi
if printf '%s\n' "$existing_clusters" | grep -Fx "$CLUSTER_NAME" >/dev/null; then
	fail "refusing to reuse pre-existing kind cluster $CLUSTER_NAME; choose another E2E_RUN_ID"
fi
if docker --context "$DOCKER_CONTEXT" image inspect "$OPERATOR_IMAGE" >/dev/null 2>&1; then
	fail "refusing to overwrite pre-existing image $OPERATOR_IMAGE; choose another E2E_RUN_ID"
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
for task_image in \
	"${REMOTE_REGISTRY}/e2e-fixture:${IMAGE_TAG}" \
	"${REMOTE_REGISTRY}/ptah-executor:${IMAGE_TAG}" \
	"${REMOTE_REGISTRY}/ptah-runner:${IMAGE_TAG}" \
	"${REMOTE_REGISTRY}/postgresql:${IMAGE_TAG}" \
	"${REMOTE_REGISTRY}/mysql:${IMAGE_TAG}"; do
	if docker --context "$DOCKER_CONTEXT" image inspect "$task_image" >/dev/null 2>&1; then
		fail "refusing to overwrite pre-existing image $task_image; choose another E2E_RUN_ID"
	fi
done
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
IMAGE_AUDIT_ARCHIVE=$WORK_DIR/image-audit.tar
CHART_PACKAGE_DIR=$WORK_DIR/chart-package
CLUSTER_CREATED=0
IMAGE_CREATED=0
IMAGE_AUDIT_CONTAINER_CREATED=0
REGISTRY_CREATED=0
KIND_NODE_IMAGE_CREATED=0
KIND_NETWORK_CREATED=0
CREATED_IMAGE_REFS=
TUNNEL_PID=
IMAGE_AUDIT_CONTAINER=$(dns_name ptah-image-audit "$identity" 63)

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

add_created_image() {
	if [ -z "$CREATED_IMAGE_REFS" ]; then
		CREATED_IMAGE_REFS=$1
	else
		CREATED_IMAGE_REFS="$1
$CREATED_IMAGE_REFS"
	fi
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
	if [ "$REGISTRY_CREATED" -eq 1 ]; then
		if ! docker --context "$DOCKER_CONTEXT" container rm -fv "$REGISTRY_CONTAINER" >/dev/null 2>&1; then
			if ! docker --context "$DOCKER_CONTEXT" info >/dev/null 2>&1 ||
				docker --context "$DOCKER_CONTEXT" container inspect "$REGISTRY_CONTAINER" >/dev/null 2>&1; then
				printf 'e2e: could not remove registry container %s from Docker context %s\n' \
					"$REGISTRY_CONTAINER" "$SELECTED_DOCKER_CONTEXT" >&2
				cleanup_failed=1
			fi
		fi
	fi
	if [ "$IMAGE_AUDIT_CONTAINER_CREATED" -eq 1 ]; then
		if ! docker --context "$DOCKER_CONTEXT" container rm -f "$IMAGE_AUDIT_CONTAINER" >/dev/null 2>&1; then
			if ! docker --context "$DOCKER_CONTEXT" info >/dev/null 2>&1 ||
				docker --context "$DOCKER_CONTEXT" container inspect "$IMAGE_AUDIT_CONTAINER" >/dev/null 2>&1; then
				printf 'e2e: could not remove image-audit container %s from Docker context %s\n' \
					"$IMAGE_AUDIT_CONTAINER" "$SELECTED_DOCKER_CONTEXT" >&2
				cleanup_failed=1
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

chart_version=$(sed -n 's/^version: //p' "$ROOT_DIR/charts/ptah-operator/Chart.yaml")
[ -n "$chart_version" ] || fail "Helm chart version is missing"
chart_source_epoch=$(git -C "$ROOT_DIR" show -s --format=%ct HEAD)
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

mkdir -p "$DOCKER_CLI_CONFIG"
unset DOCKER_HOST
DOCKER_CONFIG=$DOCKER_CLI_CONFIG docker context create "$TASK_DOCKER_CONTEXT" \
	--docker "host=${DOCKER_ENDPOINT}" >/dev/null
export DOCKER_HOST="$DOCKER_ENDPOINT"
export DOCKER_CONFIG="$DOCKER_CLI_CONFIG"
DOCKER_CONTEXT=$TASK_DOCKER_CONTEXT

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
	docker --context "$DOCKER_CONTEXT" tag "$push_source" "$push_target"
	add_created_image "$push_target"
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
	docker --context "$DOCKER_CONTEXT" build \
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
docker --context "$DOCKER_CONTEXT" build \
	--file "$ROOT_DIR/test/e2e/Dockerfile.operator" \
	--target operator \
	--tag "$OPERATOR_IMAGE" "$ROOT_DIR"
add_created_image "$FIXTURE_BUILD_IMAGE"
docker --context "$DOCKER_CONTEXT" build \
	--file "$ROOT_DIR/test/e2e/Dockerfile.operator" \
	--target fixture \
	--tag "$FIXTURE_BUILD_IMAGE" "$ROOT_DIR"

if docker --context "$DOCKER_CONTEXT" container inspect "$IMAGE_AUDIT_CONTAINER" >/dev/null 2>&1; then
	fail "refusing to reuse pre-existing image-audit container $IMAGE_AUDIT_CONTAINER"
fi
IMAGE_AUDIT_CONTAINER_CREATED=1
docker --context "$DOCKER_CONTEXT" create --name "$IMAGE_AUDIT_CONTAINER" \
	"$OPERATOR_IMAGE" >/dev/null
docker --context "$DOCKER_CONTEXT" export "$IMAGE_AUDIT_CONTAINER" >"$IMAGE_AUDIT_ARCHIVE"
if tar -tf "$IMAGE_AUDIT_ARCHIVE" | grep -Eq '(^|/)e2e-handcraft-oci$'; then
	fail "the controller image contains the test-only OCI publisher"
fi
tar -tf "$IMAGE_AUDIT_ARCHIVE" | grep -Eq '(^|/)manager$' ||
	fail "the controller image does not contain /manager"
tar -tf "$IMAGE_AUDIT_ARCHIVE" | grep -Eq '(^|/)ptah-runner$' ||
	fail "the controller image does not contain /ptah-runner"
docker --context "$DOCKER_CONTEXT" container rm "$IMAGE_AUDIT_CONTAINER" >/dev/null
IMAGE_AUDIT_CONTAINER_CREATED=0

IMAGE_AUDIT_CONTAINER_CREATED=1
docker --context "$DOCKER_CONTEXT" create --name "$IMAGE_AUDIT_CONTAINER" \
	"$FIXTURE_BUILD_IMAGE" >/dev/null
docker --context "$DOCKER_CONTEXT" export "$IMAGE_AUDIT_CONTAINER" >"$IMAGE_AUDIT_ARCHIVE"
tar -tf "$IMAGE_AUDIT_ARCHIVE" | grep -Eq '(^|/)e2e-handcraft-oci$' ||
	fail "the isolated fixture image does not contain /e2e-handcraft-oci"
if tar -tf "$IMAGE_AUDIT_ARCHIVE" | grep -Eq '(^|/)(manager|ptah-runner)$'; then
	fail "the isolated fixture image contains an operator binary"
fi
docker --context "$DOCKER_CONTEXT" container rm "$IMAGE_AUDIT_CONTAINER" >/dev/null
IMAGE_AUDIT_CONTAINER_CREATED=0

printf 'e2e: creating kind cluster %s with Kubernetes %s\n' "$CLUSTER_NAME" "$K8S_VERSION"
CLUSTER_CREATED=1
kind create cluster \
	--name "$CLUSTER_NAME" \
	--image "$KIND_NODE_IMAGE" \
	--config "$KIND_CONFIG" \
	--kubeconfig "$KUBECONFIG_FILE" \
	--wait 5m

kind load docker-image "$OPERATOR_IMAGE" --name "$CLUSTER_NAME"

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
	--env REGISTRY_AUTH=htpasswd \
	--env REGISTRY_AUTH_HTPASSWD_REALM=ptah-e2e \
	--env REGISTRY_AUTH_HTPASSWD_PATH=/registry.htpasswd \
	"$E2E_REGISTRY_IMAGE" >/dev/null
docker --context "$DOCKER_CONTEXT" cp "$REGISTRY_HTPASSWD_FILE" \
	"${REGISTRY_CONTAINER}:/registry.htpasswd"
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
CONTROL_PLANE_CONTAINER="${CLUSTER_NAME}-control-plane"
registry_dns_deadline=$(($(date +%s) + 30))
registry_dns_ready=0
while [ "$(date +%s)" -lt "$registry_dns_deadline" ]; do
	if docker --context "$DOCKER_CONTEXT" exec "$CONTROL_PLANE_CONTAINER" \
		getent ahostsv4 "$REGISTRY_DNS_NAME" 2>/dev/null |
		awk -v expected="$REGISTRY_IP" '$1 == expected {found = 1} END {exit !found}'; then
		registry_dns_ready=1
		break
	fi
	sleep 1
done
[ "$registry_dns_ready" -eq 1 ] ||
	fail "registry network alias did not resolve to its task-scoped container"
docker --context "$DOCKER_CONTEXT" exec "$CONTROL_PLANE_CONTAINER" \
	mkdir -p "/etc/containerd/certs.d/${REGISTRY_HOST}"
docker --context "$DOCKER_CONTEXT" cp "$REGISTRY_HOSTS_FILE" \
	"${CONTROL_PLANE_CONTAINER}:/etc/containerd/certs.d/${REGISTRY_HOST}/hosts.toml"

printf '%s\n' 'e2e: mirroring immutable execution and database images into the isolated registry'
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

printf 'e2e: installing Helm release %s/%s\n' "$OPERATOR_NAMESPACE" "$HELM_RELEASE"
helm --kubeconfig "$KUBECONFIG_FILE" upgrade --install "$HELM_RELEASE" \
	"$CHART_PACKAGE" \
	--namespace "$OPERATOR_NAMESPACE" \
	--create-namespace \
	--wait \
	--timeout 5m \
		--set-string image.repository="$IMAGE_REPOSITORY" \
		--set-string image.tag="$IMAGE_TAG" \
		--set image.allowMutableTag=true \
		--set-string image.pullPolicy=Never \
	--set-string execution.executorImage="$E2E_EXECUTOR_IMAGE" \
	--set-string execution.runnerImage="$E2E_RUNNER_IMAGE" \
	--set-string execution.ptahVersion="$E2E_PTAH_VERSION" \
	--set-string certificateRotation.interval="168h" \
	--set certificateRotation.recreateMissingSecret=true \
	--set replicaCount=2 \
	--set podDisruptionBudget.enabled=false

E2E_KUBECONFIG=$KUBECONFIG_FILE \
E2E_OPERATOR_NAMESPACE=$OPERATOR_NAMESPACE \
E2E_HA_TEST_NAMESPACE=$HA_TEST_NAMESPACE \
E2E_FOREIGN_NAMESPACE=$FOREIGN_NAMESPACE \
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
E2E_EXECUTOR_IMAGE=$E2E_EXECUTOR_IMAGE \
E2E_RUNNER_IMAGE=$E2E_RUNNER_IMAGE \
E2E_FIXTURE_IMAGE=$E2E_FIXTURE_IMAGE \
E2E_POSTGRES_IMAGE=$E2E_POSTGRES_IMAGE \
E2E_MYSQL_IMAGE=$E2E_MYSQL_IMAGE \
E2E_REGISTRY_IP=$REGISTRY_IP \
E2E_REGISTRY_SERVICE=$REGISTRY_SERVICE \
E2E_REGISTRY_CREDENTIALS_FILE=$REGISTRY_CREDENTIALS_FILE \
	"$ROOT_DIR/hack/e2e-dataplane.sh"

printf 'e2e: PASS Kubernetes=%s cluster=%s\n' "$server_version" "$CLUSTER_NAME"
