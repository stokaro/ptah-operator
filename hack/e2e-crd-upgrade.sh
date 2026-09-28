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

E2E_KUBECONFIG=${E2E_KUBECONFIG:?E2E_KUBECONFIG is required}
E2E_OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:?E2E_OPERATOR_NAMESPACE is required}
E2E_PROOF_NAMESPACE=${E2E_PROOF_NAMESPACE:?E2E_PROOF_NAMESPACE is required}
E2E_HELM_RELEASE=${E2E_HELM_RELEASE:?E2E_HELM_RELEASE is required}
E2E_CHART_PACKAGE=${E2E_CHART_PACKAGE:?E2E_CHART_PACKAGE is required}
E2E_KUBERNETES_VERSION=${E2E_KUBERNETES_VERSION:?E2E_KUBERNETES_VERSION is required}
E2E_PHASE=${E2E_PHASE:-upgrade}
E2E_CANDIDATE_VALUES_FILE=${E2E_CANDIDATE_VALUES_FILE:-}
E2E_CANDIDATE_IMAGE=${E2E_CANDIDATE_IMAGE:-}
E2E_NEXT_CHART_PACKAGE=${E2E_NEXT_CHART_PACKAGE:-}
E2E_NEXT_VALUES_FILE=${E2E_NEXT_VALUES_FILE:-}
E2E_NEXT_CONTROLLER_IMAGE=${E2E_NEXT_CONTROLLER_IMAGE:-}
E2E_DOCKER_CONTEXT=${E2E_DOCKER_CONTEXT:-}
E2E_EXTERNAL_POSTGRES_CONTAINER_ID=${E2E_EXTERNAL_POSTGRES_CONTAINER_ID:-}

ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-e2e-crd.XXXXXX")
chmod 700 "$WORK_DIR"
umask 077
PROOF_NAMESPACE=$E2E_PROOF_NAMESPACE
PROOF_SCHEMA=crd-upgrade-proof
PROOF_PLAN=crd-upgrade-proof
PROOF_APPROVAL=crd-upgrade-proof
PROOF_CONTROLLER_IMAGE=
CURRENT_RELEASE_CONTROLLER_IMAGE=
UPGRADE_VALUES_FILE=
EXPECTED_CRD_UPGRADE_RENDER_FILE=$WORK_DIR/expected-crd-upgrade-render.yaml
EXPECTED_RECONCILE_HOOK_NAME=
CURRENT_READ_ONLY_JOB_SCHEMA=read-only-job-current
SUCCESSOR_READ_ONLY_JOB_SCHEMA=read-only-job-successor
READ_ONLY_JOB_SCHEMA=
READ_ONLY_JOB_NAME=
READ_ONLY_JOB_UID=
RUNNING_APPLY_SCHEMA=running-apply-across-upgrade
RUNNING_APPLY_DATABASE=running-apply-database
RUNNING_APPLY_POLICY=running-apply-verification-policy
RUNNING_APPLY_PULL_SECRET=running-apply-registry
RUNNING_APPLY_PLAN_NAME=
RUNNING_APPLY_PLAN_UID=
RUNNING_APPLY_JOB_NAME=
RUNNING_APPLY_JOB_UID=
RUNNING_APPLY_POD_NAME=
RUNNING_APPLY_POD_UID=
RUNNING_APPLY_BARRIER_PID=
RUNNING_APPLY_BARRIER_ACTIVE=0
# The advisory key the barrier holds and the Apply's one statement asks for.
# Both spellings are here so a reader sees the whole contention in one place.
RUNNING_APPLY_BARRIER_KEY=742019370001
# The database the barrier holds its advisory lock in, which has to be the one
# the Apply connects to. A PostgreSQL advisory lock is per database: measured on
# PostgreSQL 17, a holder and a waiter in the same database contend, and a
# holder in the container's administrative database with a waiter in the
# application database do not -- so a barrier held in the wrong one blocks
# nothing and the Apply returns at once. The name comes from the same
# credentials the fixture's Secret is built from.
RUNNING_APPLY_BARRIER_DATABASE=
RUNNING_APPLY_BARRIER_APPLICATION=ptah-operator-running-apply-barrier
BLOCKED_STABILITY_SECONDS=10
BLOCKED_FAILURE_TIMEOUT_SECONDS=150
CERTIFICATE_SECRET_NAME=
CERTIFICATE_STAGING_SECRET_NAME=
LATE_FAILURE_BLOCKER_WEBHOOK=
CONTROLLER_IMPERSONATION_USERNAME=
CONTROLLER_IMPERSONATION_UID=
CONTROLLER_IMPERSONATION_POD_NAME=
CONTROLLER_IMPERSONATION_POD_UID=
CONTROLLER_GUARD_OWNER=
# Set while the shared-namespace proof's foreign CronJob exists, so the exit
# trap removes it: left behind, it makes every later upgrade in the release
# namespace fail the chart's shared-namespace check.
SHARED_NAMESPACE_PROBE=
CONTROLLER_GUARD_PROBE_INDEX=0
CONTROLLER_OBJECT_GUARD_PROBE_INDEX=0
KUBERNETES_MAJOR_MINOR=
CANDIDATE_CRD_SCHEMA_VERSION=$(awk '
  $1 == "operator.ptah.run/crd-schema-version:" {
    gsub(/"/, "", $2)
    print $2
    exit
  }
' "$ROOT_DIR/config/crd/bases/operator.ptah.run_ptahschemas.yaml")

# A refused parameter expansion (${VAR:?...}) or an unset name under set -u
# ends the shell without setting $?, so an EXIT trap that reports $? reads the
# previous command's success and a script that never finished reports a pass.
# The latch is set where the script reaches its own end; the trap trusts it.
PHASE_COMPLETED=0
cleanup() {
	status=$?
	[ "$status" -ne 0 ] || [ "$PHASE_COMPLETED" -eq 1 ] || status=1
	trap - EXIT HUP INT TERM
	# The marker is unset when this handler is extracted and run on its own
	# by a test; there is nothing to report then.
	if [ -n "${PHASE_REASON_MARKER:-}" ]; then
		if [ "$status" -ne 0 ] && [ ! -f "$PHASE_REASON_MARKER" ]; then
			printf 'e2e crd: exited with status %s at a command that failed under set -e; no proof reported a reason\n' "$status" >&2
		fi
		rm -f -- "$PHASE_REASON_MARKER"
	fi
	# E2E_KEEP_ON_FAILURE=1 keeps a failed phase's work directory and every
	# cluster object it created, so the refusal that ended it can be read from
	# the objects that produced it. Background processes are still stopped.
	retain=0
	if [ "$status" -ne 0 ] && [ "${E2E_KEEP_ON_FAILURE:-0}" = 1 ]; then
		retain=1
	fi
	# A barrier left holding an advisory lock keeps the Apply it blocks alive
	# past the phase, and the next phase meets a database nobody can change.
	if [ "$RUNNING_APPLY_BARRIER_ACTIVE" -eq 1 ]; then
		if ! docker --context "$E2E_DOCKER_CONTEXT" exec "$E2E_EXTERNAL_POSTGRES_CONTAINER_ID" \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD"; export PGPASSWORD; exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$2" -Atqc "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = '"'"'$1'"'"' AND pid <> pg_backend_pid()"' \
			sh "$RUNNING_APPLY_BARRIER_APPLICATION" "$RUNNING_APPLY_BARRIER_DATABASE" >/dev/null 2>&1; then
			printf 'e2e crd: could not release the running Apply database barrier\n' >&2
		fi
		RUNNING_APPLY_BARRIER_ACTIVE=0
	fi
	if [ -n "$RUNNING_APPLY_BARRIER_PID" ]; then
		kill "$RUNNING_APPLY_BARRIER_PID" >/dev/null 2>&1 || true
		wait "$RUNNING_APPLY_BARRIER_PID" >/dev/null 2>&1 || true
		RUNNING_APPLY_BARRIER_PID=
	fi
	if [ -n "$LATE_FAILURE_BLOCKER_WEBHOOK" ]; then
		if [ "$retain" -eq 0 ] && ! kube delete validatingwebhookconfiguration "$LATE_FAILURE_BLOCKER_WEBHOOK" \
			--ignore-not-found=true >/dev/null 2>&1; then
			status=1
		fi
	fi
	if [ "$retain" -eq 0 ] && [ -n "$SHARED_NAMESPACE_PROBE" ]; then
		if ! kube -n "$E2E_OPERATOR_NAMESPACE" delete cronjob "$SHARED_NAMESPACE_PROBE" \
			--ignore-not-found=true >/dev/null 2>&1; then
			status=1
		fi
	fi
	if [ "$retain" -eq 0 ] && [ -n "$CONTROLLER_GUARD_OWNER" ]; then
		if ! kube -n "$PROOF_NAMESPACE" delete configmap "$CONTROLLER_GUARD_OWNER" \
			--ignore-not-found=true >/dev/null 2>&1; then
			status=1
		fi
	fi
	if [ "$retain" -eq 1 ]; then
		printf 'e2e crd: E2E_KEEP_ON_FAILURE=1: retaining work directory %s and the proof objects in namespaces %s and %s\n' \
			"$WORK_DIR" "$E2E_OPERATOR_NAMESPACE" "$PROOF_NAMESPACE" >&2
		[ -z "$LATE_FAILURE_BLOCKER_WEBHOOK" ] ||
			printf 'e2e crd: retaining validating webhook configuration %s\n' "$LATE_FAILURE_BLOCKER_WEBHOOK" >&2
	else
		case "$WORK_DIR" in
			"${TMPDIR:-/tmp}"/ptah-operator-e2e-crd.*) rm -rf -- "$WORK_DIR" ;;
			*)
				printf 'e2e crd: refusing to remove unexpected work directory %s\n' "$WORK_DIR" >&2
				status=1
			;;
		esac
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

# A command that fails outside a guard calling fail ends the shell with no
# reason printed, and the EXIT trap then dumps diagnostics that explain
# nothing. So fail records that it spoke, in a file rather than a variable
# so that a fail inside a subshell still counts, and the trap says so when
# nothing did.
PHASE_REASON_MARKER=${TMPDIR:-/tmp}/ptah-e2e-reason-crd-upgrade.$$

fail() {
	printf 'e2e crd: %s\n' "$*" >&2
	# Tolerant of an unset marker: this function is also extracted and run on
	# its own by a test, and a reporting helper that
	# fails is worse than one that reports nothing.
	if [ -n "${PHASE_REASON_MARKER:-}" ]; then
		: >"$PHASE_REASON_MARKER" 2>/dev/null || true
	fi
	exit 1
}

# The controller-state version the candidate chart stamps, read out of the
# Makefile that stamps it. This phase proves both sides of the release fence,
# so it needs the number the candidate writes and the number one past it; a
# literal here agrees with the chart until the contract moves and then proves
# the opposite of what it says -- a rollback marker that is no longer newer is
# admitted, and the proof that a rollback is refused passes an upgrade.
CONTROLLER_STATE_VERSION=$(sed -n 's/^CONTROLLER_STATE_VERSION := //p' "$ROOT_DIR/Makefile")
printf '%s\n' "$CONTROLLER_STATE_VERSION" | grep -Eq '^[1-9][0-9]*$' ||
	fail "the Makefile must declare CONTROLLER_STATE_VERSION as a positive integer"
NEWER_CONTROLLER_STATE_VERSION=$((CONTROLLER_STATE_VERSION + 1))

printf '%s\n' "$PROOF_NAMESPACE" | grep -Eq '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' ||
	fail "E2E_PROOF_NAMESPACE must be a DNS-1123 label"
[ "${#PROOF_NAMESPACE}" -le 63 ] ||
	fail "E2E_PROOF_NAMESPACE must not exceed 63 characters"

require_mode_0600_regular_file() {
	mode_file=$1
	mode_description=$2
	if [ ! -f "$mode_file" ] || [ -L "$mode_file" ]; then
		fail "$mode_description must name a regular non-symlink file"
	fi
	if mode_value=$(stat -c '%a' "$mode_file" 2>/dev/null); then
		:
	else
		mode_value=$(stat -f '%Lp' "$mode_file") ||
			fail "could not inspect $mode_description permissions"
	fi
	[ "$mode_value" = 600 ] || fail "$mode_description must have mode 0600"
}

file_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
		return
	fi
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
		return
	fi
	fail "sha256sum or shasum is required to fingerprint the late-failure candidate"
}

case "$CANDIDATE_CRD_SCHEMA_VERSION" in
'' | 0 | 0* | *[!0-9]*) fail "candidate CRD schema version is not a positive exact decimal" ;;
esac

kube() {
	kubectl --kubeconfig "$E2E_KUBECONFIG" "$@"
}

helm_e2e() {
	helm --kubeconfig "$E2E_KUBECONFIG" "$@"
}

controller_kube() {
	[ -n "$CONTROLLER_IMPERSONATION_USERNAME" ] || fail "controller impersonation username is missing"
	[ -n "$CONTROLLER_IMPERSONATION_UID" ] || fail "controller impersonation UID is missing"
	[ -n "$CONTROLLER_IMPERSONATION_POD_NAME" ] || fail "controller impersonation Pod name is missing"
	[ -n "$CONTROLLER_IMPERSONATION_POD_UID" ] || fail "controller impersonation Pod UID is missing"
	kubectl --kubeconfig "$E2E_KUBECONFIG" \
		--as "$CONTROLLER_IMPERSONATION_USERNAME" \
		--as-uid "$CONTROLLER_IMPERSONATION_UID" \
		--as-group system:serviceaccounts \
		--as-group "system:serviceaccounts:$E2E_OPERATOR_NAMESPACE" \
		--as-group system:authenticated \
		--as-user-extra "authentication.kubernetes.io/pod-name=$CONTROLLER_IMPERSONATION_POD_NAME" \
		--as-user-extra "authentication.kubernetes.io/pod-uid=$CONTROLLER_IMPERSONATION_POD_UID" \
		"$@"
}

verify_supported_server_version() {
	printf '%s\n' "$E2E_KUBERNETES_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||
		fail "E2E_KUBERNETES_VERSION must be an exact major.minor.patch version"
	KUBERNETES_MAJOR_MINOR=$(printf '%s\n' "$E2E_KUBERNETES_VERSION" | cut -d. -f1,2)
	"$ROOT_DIR/hack/e2e-kubernetes-support-image.sh" \
		"$ROOT_DIR/support/kubernetes.json" "$E2E_KUBERNETES_VERSION" >/dev/null ||
		fail "Kubernetes $E2E_KUBERNETES_VERSION is not an exact member of support/kubernetes.json"
	server_version=$(kube version -o json | jq -er '.serverVersion.gitVersion')
	case "$server_version" in
	v"$E2E_KUBERNETES_VERSION" | v"$E2E_KUBERNETES_VERSION"-*) ;;
	*)
		fail "cluster reports $server_version, expected v$E2E_KUBERNETES_VERSION for the guarded-field proof"
		;;
	esac
}

verify_supported_server_version

production_controller_image_from_values() {
	values_file=$1
	controller_image=$(jq -er '
      .image |
      select(
        type == "object" and
        (.repository | type == "string" and test("^[^[:space:]@]+$")) and
        (.digest | type == "string" and test("^sha256:[0-9a-f]{64}$")) and
        (has("allowMutableTag") | not) and
        (has("testIdentityDigest") | not)
      ) |
      .repository + "@" + .digest
    ' "$values_file") || fail "release values do not contain one exact production controller image identity"
	printf '%s\n' "$controller_image" |
		grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' ||
		fail "release values produced an invalid production controller image identity"
	printf '%s\n' "$controller_image"
}

object_evidence() {
	resource=$1
	name=$2
	destination=$3
	kube -n "$PROOF_NAMESPACE" get "$resource" "$name" -o json |
		jq -S '{uid: .metadata.uid, spec: .spec, status: (.status // {})}' >"$destination"
}

assert_object_unchanged() {
	resource=$1
	name=$2
	before=$3
	after=$WORK_DIR/${resource}-after.json
	object_evidence "$resource" "$name" "$after"
	cmp "$before" "$after" || fail "$resource/$name UID, spec, or status changed during CRD management"
}

crd_evidence() {
	name=$1
	destination=$2
	kube get crd "$name" -o json |
		jq -S '{uid: .metadata.uid, resourceVersion: .metadata.resourceVersion, annotations: (.metadata.annotations // {}), spec: .spec}' >"$destination"
}

assert_crd_unchanged() {
	name=$1
	before=$2
	after=$WORK_DIR/${name}-after.json
	crd_evidence "$name" "$after"
	cmp "$before" "$after" ||
		fail "$name identity, annotations, spec, or resourceVersion changed despite failed CRD preflight"
}

rendered_hook_job_name() {
	rendered_hook_component=$1
	rendered_hook_weight=$2
	awk -v expected_component="$rendered_hook_component" -v expected_weight="$rendered_hook_weight" '
      function reset_document() {
        is_job = 0
        name = ""
        component = ""
        weight = ""
      }
      function emit_match() {
        if (is_job && name != "" && component == expected_component && weight == expected_weight) {
          print name
        }
      }
      /^---$/ {
        emit_match()
        reset_document()
        next
      }
      /^kind: Job$/ {
        is_job = 1
        next
      }
      is_job && /^  name: / && name == "" {
        name = $2
        gsub(/^"|"$/, "", name)
        next
      }
      is_job && /^    helm.sh\/hook-weight: / {
        weight = $2
        gsub(/^"|"$/, "", weight)
        next
      }
      is_job && /^    app.kubernetes.io\/component: / {
        component = $2
        gsub(/^"|"$/, "", component)
        next
      }
      END { emit_match() }
    ' "$EXPECTED_CRD_UPGRADE_RENDER_FILE"
}

prepare_expected_hook_names() {
	expected_hook_chart=$1
	expected_hook_values=$2
	helm_e2e template "$E2E_HELM_RELEASE" "$expected_hook_chart" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$expected_hook_values" \
		--show-only templates/crd-upgrade.yaml >"$EXPECTED_CRD_UPGRADE_RENDER_FILE"
	reconcile_matches=$(rendered_hook_job_name crd-manager 0)
	[ "$(printf '%s\n' "$reconcile_matches" | awk 'NF { count++ } END { print count + 0 }')" -eq 1 ] ||
		fail "candidate render does not contain exactly one weight-0 reconcile hook Job"
	printf '%s\n' "$reconcile_matches" | grep -Eq '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' ||
		fail "candidate render contains an invalid reconcile hook Job name"
	[ "${#reconcile_matches}" -le 63 ] || fail "candidate render contains an overlong reconcile hook Job name"
	EXPECTED_RECONCILE_HOOK_NAME=$reconcile_matches
}

deployment_evidence() {
	kube -n "$E2E_OPERATOR_NAMESPACE" get deployment -o json |
		jq -S '[.items[] | {
          name: .metadata.name,
          uid: .metadata.uid,
          generation: .metadata.generation,
          labels: (.metadata.labels // {}),
          annotations: (.metadata.annotations // {}),
          ownerReferences: (.metadata.ownerReferences // []),
          spec: .spec
        }] | sort_by(.name)'
}

expect_upgrade_failure_without_deployment_change() {
	description=$1
	shift
	before=$WORK_DIR/deployment-before.json
	after=$WORK_DIR/deployment-after.json
	status_file=$WORK_DIR/failed-upgrade-status.json
	[ -n "$UPGRADE_VALUES_FILE" ] || fail "upgrade values file is not configured"
	[ -n "$EXPECTED_RECONCILE_HOOK_NAME" ] || fail "rendered reconcile hook name is unavailable"
	before_revision=$(helm_e2e status "$E2E_HELM_RELEASE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" -o json | jq -er '.version | select(type == "number" and . >= 1)')
	failed_revision=$((before_revision + 1))
	deployment_evidence >"$before"
	if helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$UPGRADE_VALUES_FILE" \
		--wait --timeout 2m "$@" >"$WORK_DIR/failed-upgrade.out" 2>"$WORK_DIR/failed-upgrade.err"; then
		fail "$description unexpectedly succeeded"
	fi
	if ! helm_e2e status "$E2E_HELM_RELEASE" --namespace "$E2E_OPERATOR_NAMESPACE" \
		--revision "$failed_revision" -o json >"$status_file"; then
		if [ "${E2E_DEBUG_LOGS:-0}" -eq 1 ]; then
			printf 'e2e crd: E2E_DEBUG_LOGS=1: stderr of the refused upgrade follows\n' >&2
			cat "$WORK_DIR/failed-upgrade.err" >&2 || true
		fi
		fail "$description did not retain structured Helm evidence for failed revision $failed_revision"
	fi
	if ! jq -e \
		--argjson expected_revision "$failed_revision" \
		--arg expected_name "$EXPECTED_RECONCILE_HOOK_NAME" \
		-f "$ROOT_DIR/hack/failed-hook-evidence.jq" "$status_file" >/dev/null; then
		jq -c '{version, status: .info.status, description: .info.description, hooks: [(.hooks // [])[] | {name, kind, weight, events, last_run}]}' \
			"$status_file" >&2 || true
		fail "$description lacks exact revision-bound failed reconcile evidence"
	fi
	deployment_evidence >"$after"
	cmp "$before" "$after" || fail "$description mutated runtime Deployments"
}

expect_upgrade_render_failure_without_deployment_change() {
	description=$1
	shift
	before=$WORK_DIR/deployment-before.json
	after=$WORK_DIR/deployment-after.json
	[ -n "$UPGRADE_VALUES_FILE" ] || fail "upgrade values file is not configured"
	before_revision=$(helm_e2e status "$E2E_HELM_RELEASE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" -o json | jq -er '.version | select(type == "number" and . >= 1)')
	deployment_evidence >"$before"
	if helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$UPGRADE_VALUES_FILE" \
		--wait --timeout 2m "$@" >"$WORK_DIR/failed-upgrade.out" 2>"$WORK_DIR/failed-upgrade.err"; then
		fail "$description unexpectedly succeeded"
	fi
	after_revision=$(helm_e2e status "$E2E_HELM_RELEASE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" -o json | jq -er '.version | select(type == "number" and . >= 1)')
	[ "$after_revision" -eq "$before_revision" ] ||
		fail "$description created Helm revision $after_revision before template validation, expected $before_revision"
	deployment_evidence >"$after"
	cmp "$before" "$after" || fail "$description mutated runtime Deployments"
}

# The release namespace is part of the operator's trusted computing base, so
# the chart refuses to install into one that runs somebody else's workloads.
# The foreign workload is a suspended CronJob: a workload controller that starts
# no Pod and that no admission policy of this chart matches, so the refusal can
# only be the chart's. The same upgrade with the override is then rendered as a
# server-side dry run, which reads the same cluster and runs no hook.
prove_shared_release_namespace_refusal() {
	printf '%s\n' 'e2e crd: proving the chart refuses a release namespace that runs foreign workloads'
	SHARED_NAMESPACE_PROBE=ptah-e2e-shared-namespace-probe
	kube -n "$E2E_OPERATOR_NAMESPACE" create --request-timeout=15s -f - >/dev/null <<EOF
apiVersion: batch/v1
kind: CronJob
metadata:
  name: $SHARED_NAMESPACE_PROBE
spec:
  suspend: true
  schedule: "0 0 1 1 *"
  jobTemplate:
    spec:
      backoffLimit: 0
      template:
        spec:
          restartPolicy: Never
          automountServiceAccountToken: false
          securityContext:
            runAsNonRoot: true
            runAsUser: 65532
            seccompProfile:
              type: RuntimeDefault
          containers:
            - name: probe
              image: registry.k8s.io/pause:3.10
              securityContext:
                allowPrivilegeEscalation: false
                capabilities:
                  drop: ["ALL"]
EOF
	expect_upgrade_render_failure_without_deployment_change "shared release namespace upgrade"
	grep -F "release namespace $E2E_OPERATOR_NAMESPACE runs workloads without app.kubernetes.io/instance=$E2E_HELM_RELEASE (CronJob/$SHARED_NAMESPACE_PROBE)" \
		"$WORK_DIR/failed-upgrade.err" >/dev/null ||
		fail "an upgrade into a release namespace running a foreign CronJob failed without the shared-namespace refusal naming it"
	if ! helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$UPGRADE_VALUES_FILE" \
		--set releaseNamespace.allowSharedNamespace=true \
		--dry-run=server >/dev/null 2>"$WORK_DIR/shared-namespace-override.err"; then
		cat "$WORK_DIR/shared-namespace-override.err" >&2 || true
		fail "releaseNamespace.allowSharedNamespace=true did not admit the upgrade over a foreign CronJob"
	fi
	kube -n "$E2E_OPERATOR_NAMESPACE" delete cronjob "$SHARED_NAMESPACE_PROBE" \
		--wait=true --timeout=60s --request-timeout=15s >/dev/null
	SHARED_NAMESPACE_PROBE=
}

# A failure after the hook stopped the runtime and before Helm applied the new
# Deployments is the one partial upgrade that leaves no runtime running: the
# hook scaled the predecessor to zero, and nothing replaced it. The blocker
# stands in for whatever fails there. It is a webhook with no backend that
# matches only a write of this release's two Deployments carrying the
# candidate image, so the hook's scale-down passes it and Helm's apply of the
# candidate is refused.
create_late_failure_blocker() {
	LATE_FAILURE_BLOCKER_WEBHOOK=ptah-operator-e2e-late-failure-blocker
	runtime_deployment_names
	kube apply -f - >/dev/null <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: $LATE_FAILURE_BLOCKER_WEBHOOK
webhooks:
  - name: late-failure-blocker.operator.ptah.run
    admissionReviewVersions: ["v1"]
    clientConfig:
      service:
        name: ptah-operator-e2e-missing-blocker
        namespace: $E2E_OPERATOR_NAMESPACE
        path: /deny
        port: 443
    failurePolicy: Fail
    matchPolicy: Exact
    timeoutSeconds: 2
    sideEffects: None
    matchConditions:
      - name: exact-runtime-deployment
        expression: 'request.namespace == "$E2E_OPERATOR_NAMESPACE" && (request.name == "$CONTROLLER_DEPLOYMENT" || request.name == "$ROTATOR_DEPLOYMENT")'
      - name: candidate-image
        expression: 'object != null && object.spec.template.spec.containers.exists(container, container.image == "$E2E_NEXT_CONTROLLER_IMAGE")'
    rules:
      - apiGroups: ["apps"]
        apiVersions: ["v1"]
        operations: ["CREATE", "UPDATE"]
        resources: ["deployments"]
        scope: Namespaced
EOF
}

delete_late_failure_blocker() {
	[ -n "$LATE_FAILURE_BLOCKER_WEBHOOK" ] || return
	kube delete validatingwebhookconfiguration "$LATE_FAILURE_BLOCKER_WEBHOOK" \
		--wait=true >/dev/null
	LATE_FAILURE_BLOCKER_WEBHOOK=
}

assert_late_failure_candidate_unchanged() {
	late_retry_chart_sha256=$(file_sha256 "$E2E_NEXT_CHART_PACKAGE") ||
		fail "could not checksum the late-failure candidate chart"
	late_retry_values_sha256=$(file_sha256 "$E2E_NEXT_VALUES_FILE") ||
		fail "could not checksum the late-failure candidate values"
	if [ "$late_retry_chart_sha256" != "$late_candidate_chart_sha256" ] ||
		[ "$late_retry_values_sha256" != "$late_candidate_values_sha256" ] ||
		[ "$E2E_NEXT_CONTROLLER_IMAGE" != "$late_candidate_image" ]; then
		fail "late-failure recovery changed the candidate chart, values, or image"
	fi
}

prove_late_failure_recovery() {
	late_current_image=$1
	printf '%s\n' 'e2e crd: proving the boundary a late upgrade failure leaves behind'
	late_candidate_chart_sha256=$(file_sha256 "$E2E_NEXT_CHART_PACKAGE")
	late_candidate_values_sha256=$(file_sha256 "$E2E_NEXT_VALUES_FILE")
	late_candidate_image=$E2E_NEXT_CONTROLLER_IMAGE
	[ -n "$EXPECTED_RECONCILE_HOOK_NAME" ] || fail "rendered reconcile hook name is unavailable"
	runtime_deployment_names
	before_revision=$(helm_e2e status "$E2E_HELM_RELEASE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" -o json |
		jq -er '.version | select(type == "number" and . >= 1)')
	late_revision=$((before_revision + 1))
	late_status_file=$WORK_DIR/late-failure-status.json
	create_late_failure_blocker
	# Helm 4 applies server-side, and a conflict is raised for a field whose
	# value this apply changes while another manager owns it. The hook stops
	# the runtime by scaling both Deployments to zero, so it owns .spec.replicas
	# with the value this apply has to raise again. The force is confined to
	# what the stop moved.
	if helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_NEXT_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$E2E_NEXT_VALUES_FILE" \
		--force-conflicts \
		--wait --timeout 7m >"$WORK_DIR/late-failure.out" \
		2>"$WORK_DIR/late-failure.err"; then
		fail "upgrade with a late-failure blocker unexpectedly succeeded"
	fi
	grep -F 'late-failure-blocker.operator.ptah.run' "$WORK_DIR/late-failure.err" >/dev/null || {
		if [ "${E2E_DEBUG_LOGS:-0}" -eq 1 ]; then
			cat "$WORK_DIR/late-failure.err" >&2 || true
		fi
		fail "the late upgrade failed without the blocker's refusal of the candidate Deployments"
	}
	helm_e2e status "$E2E_HELM_RELEASE" --namespace "$E2E_OPERATOR_NAMESPACE" \
		--revision "$late_revision" -o json >"$late_status_file" 2>/dev/null ||
		fail "the late failure did not retain structured Helm evidence for revision $late_revision"
	if ! jq -e --argjson expected_revision "$late_revision" \
		--arg expected_reconcile_name "$EXPECTED_RECONCILE_HOOK_NAME" '
          ((.hooks // []) | if type == "array" then . else [] end) as $hooks |
          [$hooks[] | select(.name == $expected_reconcile_name and .kind == "Job")] as $reconcile |
          .version == $expected_revision and
          .info.status == "failed" and
          ([$hooks[] | select(.last_run.phase == "Failed")] | length == 0) and
          ($reconcile | length == 1) and
          ($reconcile[0] |
            .last_run.phase == "Succeeded" and
            ((.events // []) | index("pre-upgrade") != null))
        ' "$late_status_file" >/dev/null; then
		jq -c '{version, status: .info.status, description: .info.description, hooks: [(.hooks // [])[] | {name, kind, weight, events, last_run}]}' \
			"$late_status_file" >&2 || true
		fail "the late failure did not come after a reconcile hook that succeeded"
	fi
	for deployment_name in "$CONTROLLER_DEPLOYMENT" "$ROTATOR_DEPLOYMENT"; do
		kube -n "$E2E_OPERATOR_NAMESPACE" get deployment "$deployment_name" -o json |
			jq -e --arg image "$late_current_image" '
              .spec.replicas == 0 and
              all(.spec.template.spec.containers[]; .image == $image)
            ' >/dev/null || fail "the late failure did not leave $deployment_name stopped on the predecessor's template"
	done
	kube -n "$E2E_OPERATOR_NAMESPACE" get pods -o json |
		jq -e --arg controller "$current_service_account" --arg certificate "$ROTATOR_DEPLOYMENT" '
          all(.items[]; .spec.serviceAccountName != $controller and .spec.serviceAccountName != $certificate)
        ' >/dev/null || fail "the late failure left a runtime Pod after the runtime stop"
	printf '%s\n' 'e2e crd: the late failure left the runtime stopped on the predecessor template'
}

# The retry is the identical candidate. Its hook finds the Deployments still on
# the predecessor's template, stops them again, which changes nothing, and Helm
# applies the candidate over them.
retry_same_candidate() {
	if ! helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_NEXT_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$E2E_NEXT_VALUES_FILE" \
		--force-conflicts \
		--wait --timeout 7m >"$WORK_DIR/same-candidate-retry.out" \
		2>"$WORK_DIR/same-candidate-retry.err"; then
		if [ "${E2E_DEBUG_LOGS:-0}" -eq 1 ]; then
			cat "$WORK_DIR/same-candidate-retry.err" >&2 || true
		fi
		fail "the same-candidate retry did not complete the upgrade"
	fi
}

# A rollback runs the CRD hook of the release it rolls back to. That release
# has to read what is stored, and when it cannot, its hook refuses before Helm
# replaces the running Pods with ones whose verifier would refuse to start.
prove_rollback_refused_over_future_state() {
	rollback_revision=$1
	printf '%s\n' 'e2e crd: proving a rollback the stored state has outgrown is refused before any Pod changes'
	before=$WORK_DIR/rollback-deployments-before.json
	after=$WORK_DIR/rollback-deployments-after.json
	history_before=$(helm_e2e history "$E2E_HELM_RELEASE" --namespace "$E2E_OPERATOR_NAMESPACE" \
		--max 1 -o json | jq -er '.[0].revision | select(type == "number" and . >= 1)')
	stored_version=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" \
		-o jsonpath='{.status.executionBinding.controllerStateVersion}')
	[ "$stored_version" = "$CONTROLLER_STATE_VERSION" ] ||
		fail "proof PtahSchema controller state version is $stored_version, expected $CONTROLLER_STATE_VERSION"
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" --subresource=status \
		--type=json -p="[{\"op\":\"replace\",\"path\":\"/status/executionBinding/controllerStateVersion\",\"value\":$NEWER_CONTROLLER_STATE_VERSION}]" >/dev/null
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o json |
		jq -S '.status' >"$WORK_DIR/rollback-future-state.json"
	deployment_evidence >"$before"
	if helm_e2e rollback "$E2E_HELM_RELEASE" "$rollback_revision" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --force-conflicts \
		--wait --timeout 3m >"$WORK_DIR/refused-rollback.out" 2>"$WORK_DIR/refused-rollback.err"; then
		fail "a rollback over stored state newer than the release it rolls back to was admitted"
	fi
	# Helm writes the rollback revision before it runs the pre-rollback hook, so
	# a revision past the last one is what separates a refusal in the hook from
	# one before it.
	helm_e2e history "$E2E_HELM_RELEASE" --namespace "$E2E_OPERATOR_NAMESPACE" \
		--max 1 -o json >"$WORK_DIR/refused-rollback-history.json"
	jq -e --argjson before "$history_before" '
      .[0].revision == ($before + 1) and
      .[0].status != "deployed" and .[0].status != "superseded"
    ' "$WORK_DIR/refused-rollback-history.json" >/dev/null || {
		jq -c . "$WORK_DIR/refused-rollback-history.json" >&2 || true
		if [ "${E2E_DEBUG_LOGS:-0}" -eq 1 ]; then
			cat "$WORK_DIR/refused-rollback.err" >&2 || true
		fi
		fail "the refused rollback did not reach its pre-rollback hook"
	}
	deployment_evidence >"$after"
	cmp "$before" "$after" || fail "the refused rollback changed a runtime Deployment"
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o json |
		jq -S '.status' >"$WORK_DIR/rollback-future-state-after.json"
	cmp "$WORK_DIR/rollback-future-state.json" "$WORK_DIR/rollback-future-state-after.json" ||
		fail "the refused rollback rewrote the future PtahSchema state"
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" --subresource=status \
		--type=json -p="[{\"op\":\"replace\",\"path\":\"/status/executionBinding/controllerStateVersion\",\"value\":$CONTROLLER_STATE_VERSION}]" >/dev/null
	wait_runtime_ready
	printf '%s\n' 'e2e crd: the rollback was refused before any Pod changed'
}

# A rollback to a release that reads what is stored goes through: its hook stops
# the runtime the image no longer matches, and Helm brings up the release rolled
# back to. A refused rollback leaves Helm's rollback revision pending, and this
# is also the way out of it.
prove_rollback() {
	rollback_revision=$1
	rollback_image=$2
	printf 'e2e crd: rolling back to revision %s\n' "$rollback_revision"
	if ! helm_e2e rollback "$E2E_HELM_RELEASE" "$rollback_revision" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --force-conflicts \
		--wait --timeout 5m >"$WORK_DIR/rollback.out" 2>"$WORK_DIR/rollback.err"; then
		if [ "${E2E_DEBUG_LOGS:-0}" -eq 1 ]; then
			cat "$WORK_DIR/rollback.err" >&2 || true
		fi
		fail "the rollback to revision $rollback_revision was refused"
	fi
	wait_runtime_ready
	runtime_deployment_names
	for deployment_name in "$CONTROLLER_DEPLOYMENT" "$ROTATOR_DEPLOYMENT"; do
		kube -n "$E2E_OPERATOR_NAMESPACE" get deployment "$deployment_name" -o json |
			jq -e --arg image "$rollback_image" '
              .spec.replicas >= 1 and
              all(.spec.template.spec.containers[]; .image == $image)
            ' >/dev/null || fail "the rollback did not bring $deployment_name back on $rollback_image"
	done
	helm_e2e status "$E2E_HELM_RELEASE" --namespace "$E2E_OPERATOR_NAMESPACE" -o json |
		jq -e '.info.status == "deployed"' >/dev/null ||
		fail "the rollback to revision $rollback_revision did not end deployed"
}

# The controller dispatches a read-only Job for an unsuspended schema whose
# execution nodeSelector nothing satisfies, so the Job stays pending with a
# committed UID and never reads the database URL or the desired reference.
# Its manifest is the base every controller-object probe mutates, and its
# staged terminal state is what the cleanup proofs hand across a release.
dispatch_read_only_job_fixture() {
	[ -n "$READ_ONLY_JOB_SCHEMA" ] || fail "read-only Job fixture schema name is unset"
	kube -n "$PROOF_NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: $READ_ONLY_JOB_SCHEMA
spec:
  target:
    engine: PostgreSQL
    coordinationKey: $READ_ONLY_JOB_SCHEMA
    urlFrom:
      name: unused-database-url
      key: url
  desired:
    ociRef: oci://example.invalid/schema:v1
    verificationPolicyFrom:
      name: unused-verification-policy
      key: policy.yaml
  execution:
    serviceAccountName: default
    nodeSelector:
      operator.ptah.run/read-only-job-proof: blocked
EOF
	deadline=$(($(date +%s) + 120))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		READ_ONLY_JOB_NAME=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$READ_ONLY_JOB_SCHEMA" \
			-o jsonpath='{.status.activeOperation.jobName}' 2>/dev/null || true)
		READ_ONLY_JOB_UID=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$READ_ONLY_JOB_SCHEMA" \
			-o jsonpath='{.status.activeOperation.jobUID}' 2>/dev/null || true)
		if [ -n "$READ_ONLY_JOB_NAME" ] && [ -n "$READ_ONLY_JOB_UID" ] &&
			kube -n "$PROOF_NAMESPACE" get job "$READ_ONLY_JOB_NAME" -o json \
				>"$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job.json" 2>/dev/null; then
			jq -e \
				--arg schema "$READ_ONLY_JOB_SCHEMA" \
				--arg uid "$READ_ONLY_JOB_UID" '
              .metadata.uid == $uid and
              .metadata.labels["operator.ptah.run/schema"] == $schema and
              .metadata.labels["operator.ptah.run/operation"] == "resolve" and
              (.spec | has("ttlSecondsAfterFinished") | not)
            ' "$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job.json" >/dev/null ||
				fail "read-only Job does not match the dispatched operation identity"
			return
		fi
		sleep 1
	done
	fail "controller did not dispatch a read-only Job with a committed UID for $READ_ONLY_JOB_SCHEMA"
}

quiesce_read_only_job_schema() {
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$READ_ONLY_JOB_SCHEMA" \
		--type=merge -p '{"spec":{"suspend":true}}' >/dev/null
	wait_for_suspended "$READ_ONLY_JOB_SCHEMA"
	kube -n "$PROOF_NAMESPACE" get ptahschema "$READ_ONLY_JOB_SCHEMA" -o json |
		jq -e '
          .spec.suspend == true and
          .status.phase == "Suspended" and
          .status.activeOperation == null
        ' >/dev/null || fail "read-only Job schema $READ_ONLY_JOB_SCHEMA did not quiesce exactly"
}

# The operator's Pod webhook fails closed, and the controller that serves it
# is stopped while a terminal Job is staged. The Job controller cannot retire
# the pending Pod through a webhook nobody answers, so the outage is bridged
# by an exact, verified failurePolicy transition and restored right after.
set_pod_webhook_failure_policy() {
	expected_policy=$1
	desired_policy=$2
	case "$expected_policy:$desired_policy" in
	Fail:Ignore | Ignore:Fail) ;;
	*) fail "unsupported Pod webhook failurePolicy transition $expected_policy -> $desired_policy" ;;
	esac
	pod_webhook_index=$(kube get validatingwebhookconfiguration ptah-operator-admission -o json |
		jq -er '
		  [.webhooks | to_entries[] | select(.value.name == "vpodintent.operator.ptah.run")] |
		  select(length == 1) | .[0].key
		')
	pod_webhook_policy=$(kube get validatingwebhookconfiguration ptah-operator-admission -o json |
		jq -er --argjson index "$pod_webhook_index" '.webhooks[$index].failurePolicy')
	[ "$pod_webhook_policy" = "$expected_policy" ] ||
		fail "Pod webhook failurePolicy is $pod_webhook_policy, expected $expected_policy"
	pod_webhook_patch=$(jq -nc \
		--argjson index "$pod_webhook_index" \
		--arg expected "$expected_policy" \
		--arg desired "$desired_policy" '[
		  {op: "test", path: ("/webhooks/" + ($index | tostring) + "/name"), value: "vpodintent.operator.ptah.run"},
		  {op: "test", path: ("/webhooks/" + ($index | tostring) + "/failurePolicy"), value: $expected},
		  {op: "replace", path: ("/webhooks/" + ($index | tostring) + "/failurePolicy"), value: $desired}
		]')
	kube patch validatingwebhookconfiguration ptah-operator-admission \
		--type=json -p "$pod_webhook_patch" >/dev/null
	kube get validatingwebhookconfiguration ptah-operator-admission -o json |
		jq -e \
			--argjson index "$pod_webhook_index" \
			--arg desired "$desired_policy" '
			.webhooks[$index].name == "vpodintent.operator.ptah.run" and
			.webhooks[$index].failurePolicy == $desired
			' >/dev/null || fail "Pod webhook failurePolicy transition was not persisted"
}

stage_read_only_job_completion() {
	[ -n "$READ_ONLY_JOB_NAME" ] || fail "read-only Job name is missing"
	[ -n "$READ_ONLY_JOB_UID" ] || fail "read-only Job UID is missing"
	terminal_reason=ReadOnlyJobProof
	terminal_message='terminal read-only Job retained across quiescence'
	kube -n "$PROOF_NAMESPACE" get job "$READ_ONLY_JOB_NAME" -o json |
		jq -e --arg uid "$READ_ONLY_JOB_UID" '
		  .metadata.uid == $uid and
		  ((.status.conditions // []) | all(.status != "True" or (.type != "Complete" and .type != "Failed" and .type != "FailureTarget"))) and
		  (.status | has("completionTime") | not)
		' >/dev/null || fail "read-only Job was already terminal before FailureTarget staging"
	failure_target_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
	failure_target_patch=$(jq -nc \
		--arg failure_target_at "$failure_target_at" \
		--arg reason "$terminal_reason" \
		--arg message "$terminal_message" '{
	  status: {
	    conditions: [{
	      type: "FailureTarget", status: "True",
	      reason: $reason, message: $message,
	      lastProbeTime: $failure_target_at,
	      lastTransitionTime: $failure_target_at
	    }]
	  }
	}')
	kube -n "$PROOF_NAMESPACE" patch job "$READ_ONLY_JOB_NAME" --subresource=status \
		--type=merge -p "$failure_target_patch" >/dev/null

	read_only_job_terminal=0
	deadline=$(($(date +%s) + 120))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		if kube -n "$PROOF_NAMESPACE" get job "$READ_ONLY_JOB_NAME" -o json \
			>"$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job-terminal.json" 2>/dev/null &&
			jq -e \
				--arg uid "$READ_ONLY_JOB_UID" \
				--arg reason "$terminal_reason" \
				--arg message "$terminal_message" '
				  .metadata.uid == $uid and
				  (.status.startTime != null) and
				  ((.status.active // 0) == 0) and
				  ((.status.ready // 0) == 0) and
				  ((.status.terminating // 0) == 0) and
				  (((.status.uncountedTerminatedPods.succeeded // []) | length) == 0) and
				  (((.status.uncountedTerminatedPods.failed // []) | length) == 0) and
				  (.status | has("completionTime") | not) and
				  ((.status.conditions // []) | any(
				    .type == "FailureTarget" and .status == "True" and
				    .reason == $reason and .message == $message
				  )) and
				  ((.status.conditions // []) | any(
				    .type == "Failed" and .status == "True" and
				    .reason == $reason and .message == $message
				  )) and
				  (.spec | has("ttlSecondsAfterFinished") | not)
				' "$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job-terminal.json" >/dev/null; then
			read_only_job_terminal=1
			break
		fi
		sleep 1
	done
	if [ "$read_only_job_terminal" -ne 1 ]; then
		kube -n "$PROOF_NAMESPACE" get job "$READ_ONLY_JOB_NAME" -o json \
			>"$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job-terminal.json" 2>/dev/null || true
		kube -n "$PROOF_NAMESPACE" get pods \
			-l "batch.kubernetes.io/job-name=$READ_ONLY_JOB_NAME" -o json \
			>"$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job-pods.json" 2>/dev/null || true
		jq -c '{name: .metadata.name, uid: .metadata.uid, status: .status}' \
			"$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job-terminal.json" >&2 || true
		jq -c '[.items[]? | {name: .metadata.name, uid: .metadata.uid, phase: .status.phase, deletionTimestamp: .metadata.deletionTimestamp}]' \
			"$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job-pods.json" >&2 || true
		fail "Job controller did not retire the read-only Job after FailureTarget staging"
	fi
	kube -n "$PROOF_NAMESPACE" get job "$READ_ONLY_JOB_NAME" -o json |
		jq -S '{
          uid: .metadata.uid,
          name: .metadata.name,
          namespace: .metadata.namespace,
          labels: .metadata.labels,
          annotations: .metadata.annotations,
          ownerReferences: .metadata.ownerReferences,
          finalizers: (.metadata.finalizers // []),
          spec: (.spec | del(.ttlSecondsAfterFinished))
		}' >"$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job-before-cleanup.json"
}

stage_read_only_job_uid_gap() {
	[ -n "$READ_ONLY_JOB_NAME" ] || fail "read-only Job name is missing"
	[ -n "$READ_ONLY_JOB_UID" ] || fail "read-only Job UID is missing"
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$READ_ONLY_JOB_SCHEMA" --subresource=status \
		--type=json -p='[{"op":"remove","path":"/status/activeOperation/jobUID"}]' >/dev/null
	kube -n "$PROOF_NAMESPACE" get ptahschema "$READ_ONLY_JOB_SCHEMA" -o json |
		jq -e \
			--arg job_name "$READ_ONLY_JOB_NAME" '
          .status.activeOperation.jobName == $job_name and
          .status.activeOperation.type == "Resolve" and
          (.status.activeOperation | has("jobUID") | not)
        ' >/dev/null || fail "read-only fixture did not retain the exact Job name with an empty committed UID"
	kube -n "$PROOF_NAMESPACE" get job "$READ_ONLY_JOB_NAME" -o json |
		jq -e --arg uid "$READ_ONLY_JOB_UID" '
          .metadata.uid == $uid and
          (.status.conditions | any(.type == "Failed" and .status == "True")) and
          (.spec | has("ttlSecondsAfterFinished") | not)
        ' >/dev/null || fail "late-created read-only Job identity changed while staging the UID gap"
}

wait_for_read_only_job_cleanup() {
	deadline=$(($(date +%s) + 120))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		if kube -n "$PROOF_NAMESPACE" get job "$READ_ONLY_JOB_NAME" -o json \
			>"$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job-after.json" 2>/dev/null &&
			[ "$(jq -r '.spec.ttlSecondsAfterFinished // 0' "$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job-after.json")" -eq 300 ]; then
			jq -S '{
              uid: .metadata.uid,
              name: .metadata.name,
              namespace: .metadata.namespace,
              labels: .metadata.labels,
              annotations: .metadata.annotations,
              ownerReferences: .metadata.ownerReferences,
              finalizers: (.metadata.finalizers // []),
              spec: (.spec | del(.ttlSecondsAfterFinished))
            }' "$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job-after.json" \
				>"$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job-after-cleanup.json"
			cmp "$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job-before-cleanup.json" \
				"$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job-after-cleanup.json" ||
				fail "successor cleanup changed the read-only Job outside ttlSecondsAfterFinished"
			return
		fi
		sleep 1
	done
	fail "candidate manager did not schedule cleanup for the quiesced read-only Job"
}

wait_for_suspended() {
	schema_name=${1:-$PROOF_SCHEMA}
	deadline=$(($(date +%s) + 90))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		phase=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$schema_name" \
			-o jsonpath='{.status.phase}' 2>/dev/null || true)
		[ "$phase" = Suspended ] && return
		sleep 1
	done
	fail "PtahSchema $schema_name did not become Suspended"
}

runtime_deployment_names() {
	CONTROLLER_DEPLOYMENT=$(kube -n "$E2E_OPERATOR_NAMESPACE" get deployment \
		-l 'app.kubernetes.io/component=controller' -o jsonpath='{.items[0].metadata.name}')
	ROTATOR_DEPLOYMENT=$(kube -n "$E2E_OPERATOR_NAMESPACE" get deployment \
		-l 'app.kubernetes.io/component=certificate-rotation' -o jsonpath='{.items[0].metadata.name}')
	[ -n "$CONTROLLER_DEPLOYMENT" ] || fail "controller Deployment is missing"
	[ -n "$ROTATOR_DEPLOYMENT" ] || fail "certificate-rotation Deployment is missing"
}

capture_certificate_secret_names() {
	runtime_deployment_names
	if ! CERTIFICATE_SECRET_NAME=$(kube -n "$E2E_OPERATOR_NAMESPACE" get deployment \
		"$ROTATOR_DEPLOYMENT" -o json | jq -er '
          [.spec.template.spec.containers[] |
            select(.name == "certificate-rotator") |
            (.args // [])[] |
            select(startswith("--secret-name=")) |
            ltrimstr("--secret-name=")] as $names |
          if ($names | length) == 1 and ($names[0] | length) > 0
          then $names[0]
          else error("certificate rotator must carry one nonempty serving Secret identity")
          end
        '); then
		fail "could not capture the exact generated certificate Secret identity"
	fi
	if ! CERTIFICATE_STAGING_SECRET_NAME=$(kube -n "$E2E_OPERATOR_NAMESPACE" get deployment \
		"$ROTATOR_DEPLOYMENT" -o json | jq -er '
          [.spec.template.spec.containers[] |
            select(.name == "certificate-rotator") |
            (.args // [])[] |
            select(startswith("--staging-secret-name=")) |
            ltrimstr("--staging-secret-name=")] as $names |
          if ($names | length) == 1 and ($names[0] | length) > 0
          then $names[0]
          else error("certificate rotator must carry one nonempty staging Secret identity")
          end
        '); then
		fail "could not capture the exact certificate staging Secret identity"
	fi
	[ "$CERTIFICATE_SECRET_NAME" != "$CERTIFICATE_STAGING_SECRET_NAME" ] ||
		fail "generated and staging certificate Secret identities must differ"
}

capture_controller_impersonation_identity() {
	runtime_deployment_names
	controller_pod_json=$WORK_DIR/controller-impersonation-pod.json
	kube -n "$E2E_OPERATOR_NAMESPACE" get pods \
		-l "app.kubernetes.io/instance=$E2E_HELM_RELEASE,app.kubernetes.io/component=controller" \
		-o json | jq -e '
          [.items[] | select(.status.phase == "Running")] |
          if length > 0 then sort_by(.metadata.name)[0] else error("no running controller Pod") end
        ' >"$controller_pod_json"
	controller_service_account=$(jq -er '.spec.serviceAccountName' "$controller_pod_json")
	controller_service_account_uid=$(kube -n "$E2E_OPERATOR_NAMESPACE" get serviceaccount \
		"$controller_service_account" -o jsonpath='{.metadata.uid}')
	[ -n "$controller_service_account_uid" ] || fail "controller ServiceAccount UID is empty"
	CONTROLLER_IMPERSONATION_USERNAME="system:serviceaccount:$E2E_OPERATOR_NAMESPACE:$controller_service_account"
	CONTROLLER_IMPERSONATION_UID=$controller_service_account_uid
	CONTROLLER_IMPERSONATION_POD_NAME=$(jq -er '.metadata.name' "$controller_pod_json")
	CONTROLLER_IMPERSONATION_POD_UID=$(jq -er '.metadata.uid' "$controller_pod_json")
}

clear_controller_impersonation_identity() {
	CONTROLLER_IMPERSONATION_USERNAME=
	CONTROLLER_IMPERSONATION_UID=
	CONTROLLER_IMPERSONATION_POD_NAME=
	CONTROLLER_IMPERSONATION_POD_UID=
}

runtime_deployment_evidence() {
	runtime_deployment_names
	kube -n "$E2E_OPERATOR_NAMESPACE" get deployment \
		"$CONTROLLER_DEPLOYMENT" "$ROTATOR_DEPLOYMENT" -o json |
		jq -S '[.items[] | {
          name: .metadata.name,
          uid: .metadata.uid,
          generation: .metadata.generation,
          spec: .spec
        }] | sort_by(.name)'
}

capture_controller_service_account_identity() {
	expected_manager_image=$1
	destination=$2
	deployment_list=${destination}.deployments
	deployment_identity=${destination}.deployment-identity
	service_account_object=${destination}.service-account
	kube -n "$E2E_OPERATOR_NAMESPACE" get deployment \
		-l "app.kubernetes.io/instance=$E2E_HELM_RELEASE,app.kubernetes.io/component=controller" \
		-o json >"$deployment_list"
	jq -e \
		--arg release "$E2E_HELM_RELEASE" \
		--arg image "$expected_manager_image" '
      [.items[] | select(
        .metadata.labels["app.kubernetes.io/instance"] == $release and
        .metadata.labels["app.kubernetes.io/component"] == "controller"
      )] |
      # A release runs one controller Deployment under a stable name, so the
      # labels find exactly one. Which release it belongs to is the manager
      # image below: the Deployment carries no release sequence.
      if length != 1 then error("controller Deployment cardinality differs") else .[0] end |
      select(
        (.metadata.uid | type == "string" and length > 0) and
        (.spec.template.spec.serviceAccountName | type == "string" and length > 0) and
        ([.spec.template.spec.containers[] |
          select(.name == "manager" and .image == $image)] | length) == 1
      ) |
      {
        deploymentName: .metadata.name,
        deploymentUID: .metadata.uid,
        serviceAccountName: .spec.template.spec.serviceAccountName,
        managerImage: $image
      }
    ' "$deployment_list" >"$deployment_identity" ||
		fail "controller Deployment does not have the exact runtime identity for image $expected_manager_image"
	controller_service_account=$(jq -er '.serviceAccountName' "$deployment_identity")
	kube -n "$E2E_OPERATOR_NAMESPACE" get serviceaccount "$controller_service_account" \
		-o json >"$service_account_object"
	controller_service_account_uid=$(jq -er \
		--arg name "$controller_service_account" \
		--arg namespace "$E2E_OPERATOR_NAMESPACE" \
		--arg release "$E2E_HELM_RELEASE" '
      select(
        .apiVersion == "v1" and
        .kind == "ServiceAccount" and
        .metadata.name == $name and
        .metadata.namespace == $namespace and
        .metadata.labels["app.kubernetes.io/instance"] == $release and
        (.metadata.uid | type == "string" and length > 0) and
        .metadata.deletionTimestamp == null
      ) |
      .metadata.uid
    ' "$service_account_object") ||
		fail "controller ServiceAccount does not have the exact live identity for image $expected_manager_image"
	jq --arg service_account_uid "$controller_service_account_uid" \
		'. + {serviceAccountUID: $service_account_uid}' \
		"$deployment_identity" >"$destination"
}

assert_release_runtime_removed() {
	[ -n "$CERTIFICATE_SECRET_NAME" ] ||
		fail "generated certificate Secret identity was not captured before uninstall"
	[ -n "$CERTIFICATE_STAGING_SECRET_NAME" ] ||
		fail "certificate staging Secret identity was not captured before uninstall"
	# helm uninstall --wait waits for the objects Helm deletes. The ReplicaSets
	# and Pods behind the Deployments go through garbage collection and the
	# Pods' termination grace afterwards, so give them that time before the
	# inventory below asserts that nothing labeled is left.
	runtime_removal_deadline=$(($(date +%s) + 180))
	while :; do
		remaining_runtime=$(kube -n "$E2E_OPERATOR_NAMESPACE" get replicaset,pod \
			-l "app.kubernetes.io/instance=$E2E_HELM_RELEASE" -o json |
			jq -r '.items | length')
		[ "$remaining_runtime" -eq 0 ] && break
		[ "$(date +%s)" -lt "$runtime_removal_deadline" ] ||
			fail "$remaining_runtime labeled ReplicaSet or Pod objects outlived uninstall by 180s"
		sleep 2
	done
	for singleton_resource in mutatingwebhookconfiguration validatingwebhookconfiguration; do
		remaining=$(kube get "$singleton_resource" ptah-operator-admission \
			--ignore-not-found=true -o name)
		[ -z "$remaining" ] || fail "$singleton_resource/ptah-operator-admission survived uninstall"
	done
	for cluster_resource in \
		validatingadmissionpolicy \
		validatingadmissionpolicybinding \
		mutatingwebhookconfiguration \
		validatingwebhookconfiguration \
		clusterrole \
		clusterrolebinding; do
		remaining=$(kube get "$cluster_resource" \
			-l "app.kubernetes.io/instance=$E2E_HELM_RELEASE" -o json |
			jq -r '.items | length')
		[ "$remaining" -eq 0 ] ||
			fail "$remaining labeled $cluster_resource objects survived uninstall"
	done

	for namespaced_resource in \
		deployment replicaset service secret serviceaccount role rolebinding \
		job pod configmap lease poddisruptionbudget; do
		remaining=$(kube -n "$E2E_OPERATOR_NAMESPACE" get "$namespaced_resource" \
			-l "app.kubernetes.io/instance=$E2E_HELM_RELEASE" -o json |
			jq -r '.items | length')
		[ "$remaining" -eq 0 ] ||
			fail "$remaining labeled $namespaced_resource objects survived uninstall"
	done
	remaining=$(kube -n "$E2E_OPERATOR_NAMESPACE" get \
		"secret/$CERTIFICATE_SECRET_NAME" --ignore-not-found=true -o name)
	[ -z "$remaining" ] ||
		fail "unlabeled generated certificate Secret/$CERTIFICATE_SECRET_NAME survived uninstall"
	remaining=$(kube -n "$E2E_OPERATOR_NAMESPACE" get \
		"secret/$CERTIFICATE_STAGING_SECRET_NAME" --ignore-not-found=true -o name)
	[ -z "$remaining" ] ||
		fail "unlabeled certificate staging Secret/$CERTIFICATE_STAGING_SECRET_NAME survived uninstall"
	CERTIFICATE_SECRET_NAME=
	CERTIFICATE_STAGING_SECRET_NAME=
}

# snapshot_runtime_deployment records a runtime Deployment and the field
# manager that owns it. Helm 4 applies server-side, so restoring the snapshot
# with kubectl's own manager would leave the object owned by "kubectl-create"
# and the next helm upgrade would fail with a field-manager conflict over the
# fields Helm expects to own, instead of upgrading. The restore therefore
# applies as the manager the live object had, and a Deployment with no
# server-side apply manager, or more than one, is refused rather than guessed.
snapshot_runtime_deployment() {
	deployment_name=$1
	destination=$2
	kube -n "$E2E_OPERATOR_NAMESPACE" get deployment "$deployment_name" \
		--show-managed-fields -o json >"$destination.live"
	deployment_managers=$(jq -r '
          [.metadata.managedFields[]? | select(.operation == "Apply") | .manager] | unique
        ' "$destination.live")
	[ "$(printf '%s\n' "$deployment_managers" | jq -r 'length')" -eq 1 ] ||
		fail "runtime Deployment $deployment_name has no single server-side apply field manager to restore: $(printf '%s\n' "$deployment_managers" | jq -c .)"
	printf '%s\n' "$deployment_managers" | jq -r '.[0]' >"$destination.manager"
	jq 'del(
          .metadata.creationTimestamp,
          .metadata.generation,
          .metadata.managedFields,
          .metadata.resourceVersion,
          .metadata.uid,
          .metadata.annotations."deployment.kubernetes.io/revision",
          .status
        )' "$destination.live" >"$destination"
	rm -f "$destination.live"
}

# restore_runtime_deployment recreates a snapshot as its recorded owner.
restore_runtime_deployment() {
	deployment_snapshot=$1
	[ -s "$deployment_snapshot" ] || fail "runtime Deployment snapshot $deployment_snapshot is missing"
	[ -s "$deployment_snapshot.manager" ] ||
		fail "runtime Deployment snapshot $deployment_snapshot has no recorded field manager"
	kube apply --server-side \
		--field-manager="$(cat "$deployment_snapshot.manager")" \
		-f "$deployment_snapshot" >/dev/null
}

stop_runtime_deployments() {
	runtime_deployment_names
	CONTROLLER_DEPLOYMENT_SNAPSHOT=$WORK_DIR/controller-deployment.json
	ROTATOR_DEPLOYMENT_SNAPSHOT=$WORK_DIR/certificate-deployment.json
	snapshot_runtime_deployment "$CONTROLLER_DEPLOYMENT" "$CONTROLLER_DEPLOYMENT_SNAPSHOT"
	snapshot_runtime_deployment "$ROTATOR_DEPLOYMENT" "$ROTATOR_DEPLOYMENT_SNAPSHOT"
	kube -n "$E2E_OPERATOR_NAMESPACE" delete deployment \
		"$CONTROLLER_DEPLOYMENT" "$ROTATOR_DEPLOYMENT" \
		--cascade=foreground --wait=true --timeout=2m >/dev/null
	for component in controller certificate-rotation; do
		kube -n "$E2E_OPERATOR_NAMESPACE" wait pod \
			-l "app.kubernetes.io/component=$component" \
			--for=delete --timeout=2m >/dev/null
	done
}

stop_controller_deployment() {
	runtime_deployment_names
	CONTROLLER_DEPLOYMENT_SNAPSHOT=$WORK_DIR/controller-deployment.json
	snapshot_runtime_deployment "$CONTROLLER_DEPLOYMENT" "$CONTROLLER_DEPLOYMENT_SNAPSHOT"
	kube -n "$E2E_OPERATOR_NAMESPACE" delete deployment "$CONTROLLER_DEPLOYMENT" \
		--cascade=foreground --wait=true --timeout=2m >/dev/null
	kube -n "$E2E_OPERATOR_NAMESPACE" wait pod \
		-l 'app.kubernetes.io/component=controller' \
		--for=delete --timeout=2m >/dev/null
}

start_runtime_deployments() {
	restore_runtime_deployment "$CONTROLLER_DEPLOYMENT_SNAPSHOT"
	restore_runtime_deployment "$ROTATOR_DEPLOYMENT_SNAPSHOT"
}

start_controller_deployment() {
	restore_runtime_deployment "$CONTROLLER_DEPLOYMENT_SNAPSHOT"
}

assert_explicit_runtime_guard() {
	description=$1
	scope=$2
	expected_pods=$3
	deadline=$(($(date +%s) + BLOCKED_FAILURE_TIMEOUT_SECONDS))
	blocked_since=0
	blocked_pod_uids=
	while [ "$(date +%s)" -lt "$deadline" ]; do
		kube -n "$E2E_OPERATOR_NAMESPACE" get pods -o json >"$WORK_DIR/runtime-guard-pods.json"
		jq --arg release "$E2E_HELM_RELEASE" --arg scope "$scope" \
			--argjson expected "$expected_pods" \
			-f "$ROOT_DIR/hack/e2e-crd-init-guard.jq" \
			"$WORK_DIR/runtime-guard-pods.json" >"$WORK_DIR/runtime-guard-state.json"
		if [ "$(jq -r '.mainContainersNeverStarted' "$WORK_DIR/runtime-guard-state.json")" != true ]; then
			fail "$description allowed a manager or certificate-rotator main container to start"
		fi
		if [ "$(jq -r '.explicitVerifierFailures' "$WORK_DIR/runtime-guard-state.json")" = true ]; then
			current_pod_uids=$(jq -r '.podUIDs' "$WORK_DIR/runtime-guard-state.json")
			if [ "$current_pod_uids" != "$blocked_pod_uids" ]; then
				blocked_pod_uids=$current_pod_uids
				blocked_since=$(date +%s)
			elif [ "$(($(date +%s) - blocked_since))" -ge "$BLOCKED_STABILITY_SECONDS" ]; then
				return
			fi
		else
			blocked_since=0
			blocked_pod_uids=
		fi
		sleep 1
	done
	fail "$description did not produce stable explicit init-container failures on $expected_pods Pods"
}

assert_runtime_blocked() {
	description=$1
	assert_explicit_runtime_guard "$description" all 3
}

wait_runtime_ready() {
	runtime_deployment_names
	for ready_deployment in "$CONTROLLER_DEPLOYMENT" "$ROTATOR_DEPLOYMENT"; do
		if ! kube -n "$E2E_OPERATOR_NAMESPACE" rollout status deployment "$ready_deployment" --timeout=3m >/dev/null; then
			kube -n "$E2E_OPERATOR_NAMESPACE" get pods \
				-l "app.kubernetes.io/instance=$E2E_HELM_RELEASE" -o json 2>/dev/null |
				jq -c '.items[:10][] | {pod: .metadata.name, init: [
                  .status.initContainerStatuses[:8][]? |
                  {name: .name, waitingReason: .state.waiting.reason,
                    terminatedReason: .state.terminated.reason, exitCode: .state.terminated.exitCode}
                ]}' >&2 || true
			fail "runtime Deployment $E2E_OPERATOR_NAMESPACE/$ready_deployment did not become ready within 3m"
		fi
	done
}

controller_write_evidence() {
	destination=$1
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o json |
		jq -S '{
          uid: .metadata.uid,
          labels: (.metadata.labels // {}),
          annotations: (.metadata.annotations // {}),
          ownerReferences: (.metadata.ownerReferences // []),
          finalizers: (.metadata.finalizers // []),
          spec: .spec,
          status: (.status // {})
        }' >"$destination"
}

expect_controller_write_denial() {
	description=$1
	patch_type=$2
	patch_body=$3
	CONTROLLER_GUARD_PROBE_INDEX=$((CONTROLLER_GUARD_PROBE_INDEX + 1))
	stdout=$WORK_DIR/controller-write-denial-${CONTROLLER_GUARD_PROBE_INDEX}.out
	stderr=$WORK_DIR/controller-write-denial-${CONTROLLER_GUARD_PROBE_INDEX}.err
	if controller_kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" \
		--type "$patch_type" -p "$patch_body" --dry-run=server -o json \
		>"$stdout" 2>"$stderr"; then
		fail "controller identity was allowed to mutate $description"
	fi
	if ! grep -F 'Ptah controller write guard rejected a desired-state mutation' "$stderr" >/dev/null &&
		! grep -F 'Ptah controller write guard rejected a desired-state mutation' "$stdout" >/dev/null; then
		# The refusal that did come back is the API server's answer to a patch
		# of this resource, which carries no credential, and it is the only
		# evidence of which admission step refused instead.
		fail "controller $description mutation failed without the exact write-guard denial: $(head -c 600 "$stderr" | tr '\n' ' ')"
	fi
}

expect_controller_job_api_acceptance() {
	description=$1
	manifest=$2
	retained_filter=$3
	CONTROLLER_OBJECT_GUARD_PROBE_INDEX=$((CONTROLLER_OBJECT_GUARD_PROBE_INDEX + 1))
	stdout=$WORK_DIR/controller-object-api-${CONTROLLER_OBJECT_GUARD_PROBE_INDEX}.out
	stderr=$WORK_DIR/controller-object-api-${CONTROLLER_OBJECT_GUARD_PROBE_INDEX}.err
	if ! kube create --dry-run=server -o json -f "$manifest" >"$stdout" 2>"$stderr"; then
		cat "$stderr" >&2
		fail "Kubernetes $KUBERNETES_MAJOR_MINOR did not accept the $description guard probe"
	fi
	jq -e "$retained_filter" "$stdout" >/dev/null ||
		fail "Kubernetes $KUBERNETES_MAJOR_MINOR dropped the $description guard probe before admission"
}

expect_controller_job_vap_denial() {
	description=$1
	manifest=$2
	CONTROLLER_OBJECT_GUARD_PROBE_INDEX=$((CONTROLLER_OBJECT_GUARD_PROBE_INDEX + 1))
	stdout=$WORK_DIR/controller-object-denial-${CONTROLLER_OBJECT_GUARD_PROBE_INDEX}.out
	stderr=$WORK_DIR/controller-object-denial-${CONTROLLER_OBJECT_GUARD_PROBE_INDEX}.err
	if controller_kube create --dry-run=server -o json -f "$manifest" >"$stdout" 2>"$stderr"; then
		fail "controller identity was allowed to create a Job with $description"
	fi
	if ! grep -F 'Ptah controller Job write guard rejected an unsafe workload shape' \
		"$stdout" "$stderr" >/dev/null; then
		cat "$stderr" >&2
		fail "controller $description probe failed without the exact controller-object VAP denial"
	fi
}

prove_controller_object_supported_window_guard() {
	printf 'e2e crd: proving controller Job guarded fields on Kubernetes %s\n' \
		"$KUBERNETES_MAJOR_MINOR"
	base_manifest=$WORK_DIR/controller-object-base-job.json
	base_job_source=$WORK_DIR/$CURRENT_READ_ONLY_JOB_SCHEMA-read-only-job.json
	[ -s "$base_job_source" ] ||
		fail "current-release read-only Job evidence is unavailable for the controller-object proof"
	printf '%s\n' "$PROOF_CONTROLLER_IMAGE" |
		grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' ||
		fail "controller-object proof lacks an exact candidate controller image"
	jq \
		--arg controller_image "$PROOF_CONTROLLER_IMAGE" \
		--arg controller_state_version "$CONTROLLER_STATE_VERSION" '
      del(
        .metadata.creationTimestamp,
        .metadata.generation,
        .metadata.managedFields,
        .metadata.resourceVersion,
        .metadata.uid,
        .spec.selector,
        .spec.ttlSecondsAfterFinished,
        .status,
        .spec.template.metadata.creationTimestamp,
        .spec.template.metadata.generation,
        .spec.template.metadata.managedFields,
        .spec.template.metadata.resourceVersion,
        .spec.template.metadata.uid
      ) |
      # The Job write guard requires the name to start with the operation
      # its own label names, so the probe is named after the operation the
      # captured Job carries rather than after a fixed one.
      .metadata.name = "ptah-" + .metadata.labels["operator.ptah.run/operation"] + "-vap-probe-0123456789abcdef" |
      .metadata.annotations["operator.ptah.run/controller-image"] = $controller_image |
      .metadata.annotations["operator.ptah.run/controller-revision"] = "e2e-controller-object-guard" |
      .metadata.annotations["operator.ptah.run/controller-state-version"] = $controller_state_version |
      del(
        .spec.template.metadata.labels["batch.kubernetes.io/controller-uid"],
        .spec.template.metadata.labels["batch.kubernetes.io/job-name"],
        .spec.template.metadata.labels["controller-uid"],
        .spec.template.metadata.labels["job-name"]
      ) |
      .spec.template.metadata.annotations = .metadata.annotations
    ' "$base_job_source" >"$base_manifest"

	baseline_stdout=$WORK_DIR/controller-object-baseline.out
	baseline_stderr=$WORK_DIR/controller-object-baseline.err
	# This boundary is the webhook's answer, and the manager rolled out a moment
	# ago, so its Service can still hold an endpoint that refuses the connection.
	# Retry only while the API server reports it could not reach the webhook at
	# all: a refusal that arrives is the answer under test, whatever it says.
	baseline_deadline=$(($(date +%s) + 120))
	while :; do
		if controller_kube create --dry-run=server -o json -f "$base_manifest" \
			>"$baseline_stdout" 2>"$baseline_stderr"; then
			fail "controller-object baseline bypassed the semantic Job write boundary"
		fi
		grep -Eq 'failed calling webhook|no endpoints available|connection refused|service unavailable' \
			"$baseline_stderr" || break
		[ "$(date +%s)" -lt "$baseline_deadline" ] || {
			cat "$baseline_stderr" >&2
			fail "controller write webhook stayed unreachable for the baseline boundary"
		}
		sleep 2
	done
	if grep -F 'Ptah controller Job write guard rejected an unsafe workload shape' \
		"$baseline_stdout" "$baseline_stderr" >/dev/null; then
		fail "controller-object baseline does not satisfy the structural VAP contract"
	fi
	grep -F 'Job does not match a not-yet-created active operation' \
		"$baseline_stdout" "$baseline_stderr" >/dev/null || {
		cat "$baseline_stderr" >&2
		fail "controller-object baseline did not reach the semantic Job write boundary"
	}

	# The policy carries this release's manager image, so a Job stamped with any
	# other is refused before the webhook is asked: a manager left over from
	# another release cannot create one. The baseline above carries this
	# release's image and reached the webhook, so the refusal is the image.
	manifest=$WORK_DIR/controller-object-other-release.json
	jq '.metadata.annotations["operator.ptah.run/controller-image"] = "registry.invalid/ptah-operator@sha256:" + ("0" * 64) |
      .spec.template.metadata.annotations = .metadata.annotations' \
		"$base_manifest" >"$manifest"
	expect_controller_job_vap_denial "another release's manager image" "$manifest"

	case "$KUBERNETES_MAJOR_MINOR" in
	1.35)
		manifest=$WORK_DIR/controller-object-workload-ref.json
		jq '.spec.template.spec.workloadRef = {name: "probe", podGroup: "probe"}' \
			"$base_manifest" >"$manifest"
		expect_controller_job_api_acceptance PodSpec.workloadRef "$manifest" \
			'.spec.template.spec.workloadRef == {name: "probe", podGroup: "probe"}'
		expect_controller_job_vap_denial PodSpec.workloadRef "$manifest"
		;;
	1.36)
		printf '%s\n' \
			'e2e crd: Kubernetes 1.36 has no requested version-specific guarded-field probe'
		;;
	1.37)
		manifest=$WORK_DIR/controller-object-job-scheduling.json
		jq '.spec.scheduling = {schedulingPolicy: {basic: {}}}' \
			"$base_manifest" >"$manifest"
		expect_controller_job_api_acceptance JobSpec.scheduling "$manifest" \
			'.spec.scheduling.schedulingPolicy.basic == {}'
		expect_controller_job_vap_denial JobSpec.scheduling "$manifest"

		manifest=$WORK_DIR/controller-object-eviction-responders.json
		jq '.spec.template.spec.evictionResponders = [{name: "example.com/probe", priority: 1000}]' \
			"$base_manifest" >"$manifest"
		expect_controller_job_api_acceptance PodSpec.evictionResponders "$manifest" \
			'.spec.template.spec.evictionResponders == [{name: "example.com/probe", priority: 1000}]'
		expect_controller_job_vap_denial PodSpec.evictionResponders "$manifest"

		manifest=$WORK_DIR/controller-object-empty-dir-mode.json
		jq '(.spec.template.spec.volumes[] | select(.name == "work").emptyDir.mode) = 448' \
			"$base_manifest" >"$manifest"
		expect_controller_job_api_acceptance EmptyDirVolumeSource.mode "$manifest" \
			'any(.spec.template.spec.volumes[]; .name == "work" and .emptyDir.mode == 448)'
		expect_controller_job_vap_denial EmptyDirVolumeSource.mode "$manifest"

		manifest=$WORK_DIR/controller-object-bind-mount-options.json
		jq '(.spec.template.spec.containers[0].volumeMounts[] | select(.name == "work").bindMountOptions) = ["noexec"]' \
			"$base_manifest" >"$manifest"
		expect_controller_job_api_acceptance VolumeMount.bindMountOptions "$manifest" \
			'any(.spec.template.spec.containers[0].volumeMounts[]; .name == "work" and .bindMountOptions == ["noexec"])'
		expect_controller_job_vap_denial VolumeMount.bindMountOptions "$manifest"
		;;
	esac
	printf 'e2e crd: controller Job guarded-field proof passed on Kubernetes %s\n' \
		"$KUBERNETES_MAJOR_MINOR"
}

prove_controller_direct_write_webhook() {
	manifest=$WORK_DIR/controller-direct-write-probe.yaml
	error_file=$WORK_DIR/controller-direct-write-probe.err
	cat >"$manifest" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ptah-plan-111111111111111111111111-000
  namespace: $PROOF_NAMESPACE
  labels:
    operator.ptah.run/plan: ptah-plan-111111111111111111111111
    operator.ptah.run/schema: $PROOF_SCHEMA
  ownerReferences:
    - apiVersion: operator.ptah.run/v1alpha1
      kind: PtahSchemaPlan
      name: ptah-plan-111111111111111111111111
      uid: 11111111-1111-1111-1111-111111111111
      controller: true
      blockOwnerDeletion: true
immutable: true
binaryData:
  chunk: cHJvYmU=
EOF
	# This runs immediately after the manager rollout, so the webhook may not be
	# serving yet. Every webhook here is failurePolicy: Fail, so an unready one
	# still rejects the create -- the refusal below is satisfied by the API
	# server rather than by the boundary this proves, and the message is a
	# connection error instead of the semantic one. Retry until the rejection is
	# the semantic one.
	#
	# Acceptance is not retried: with failurePolicy: Fail nothing accepts this
	# chunk while the webhook is away, so an accepted create is the defect the
	# probe exists to catch.
	deadline=$(($(date +%s) + 60))
	while :; do
		if controller_kube create --dry-run=server -f "$manifest" >/dev/null 2>"$error_file"; then
			fail "controller direct-write webhook accepted a structurally valid chunk without a persisted plan"
		fi
		if grep -F 'directly read plan manifest' "$error_file" >/dev/null; then
			return
		fi
		[ "$(date +%s)" -lt "$deadline" ] || break
		sleep 1
	done
	# The sibling Job probe prints what it got before giving up; this one used to
	# swallow it, which cost a whole run to work out what had rejected the write.
	cat "$error_file" >&2
	fail "controller direct-write probe did not reach the uncached semantic webhook boundary"
}

prove_controller_write_guard() {
	printf '%s\n' 'e2e crd: proving the controller desired-state write boundary'
	capture_controller_impersonation_identity

	controller_write_evidence "$WORK_DIR/controller-write-before.json"
	prove_controller_direct_write_webhook
	prove_controller_object_supported_window_guard
	stop_controller_deployment

	current_suspend=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o json |
		jq -r '
      (.spec.suspend // false) as $suspend |
      if ($suspend | type) == "boolean" then ($suspend | tostring)
      else error("schema spec.suspend must be a boolean") end
    ')
	suspend_patch=$(jq -cn --argjson current "$current_suspend" '{spec: {suspend: ($current | not)}}')
	expect_controller_write_denial spec merge "$suspend_patch"
	expect_controller_write_denial labels merge \
		'{"metadata":{"labels":{"operator.ptah.run/controller-write-probe":"forbidden"}}}'
	expect_controller_write_denial annotations merge \
		'{"metadata":{"annotations":{"operator.ptah.run/controller-write-probe":"forbidden"}}}'

	CONTROLLER_GUARD_OWNER=controller-write-guard-owner
	kube -n "$PROOF_NAMESPACE" create configmap "$CONTROLLER_GUARD_OWNER" >/dev/null
	owner_uid=$(kube -n "$PROOF_NAMESPACE" get configmap "$CONTROLLER_GUARD_OWNER" \
		-o jsonpath='{.metadata.uid}')
	owner_patch=$(jq -cn --arg name "$CONTROLLER_GUARD_OWNER" --arg uid "$owner_uid" '{
      metadata: {ownerReferences: [{
        apiVersion: "v1", kind: "ConfigMap", name: $name, uid: $uid
      }]}
    }')
	expect_controller_write_denial ownerReferences merge "$owner_patch"
	expect_controller_write_denial 'a foreign finalizer' merge \
		'{"metadata":{"finalizers":["operator.ptah.run/foreign-operation"]}}'

	status_before=$WORK_DIR/controller-write-status-before.json
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o json |
		jq -S '.status // {}' >"$status_before"
	if controller_kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" \
		--type merge -p '{"status":{"phase":"ControllerWriteProbe"}}' \
		--dry-run=server -o json >"$WORK_DIR/controller-write-status-dry-run.json" \
		2>"$WORK_DIR/controller-write-status-dry-run.err"; then
		jq -S '.status // {}' "$WORK_DIR/controller-write-status-dry-run.json" \
			>"$WORK_DIR/controller-write-status-response.json"
		cmp "$status_before" "$WORK_DIR/controller-write-status-response.json" ||
			fail "the main PtahSchema endpoint accepted a controller status mutation"
	else
		grep -F 'Ptah controller write guard rejected a desired-state mutation' \
			"$WORK_DIR/controller-write-status-dry-run.err" >/dev/null ||
			fail "controller main-resource status mutation failed without a safe API or write-guard refusal"
	fi

	current_finalizers=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o json |
		jq -c '(.metadata.finalizers // [])')
	printf '%s\n' "$current_finalizers" |
		jq -e 'index("operator.ptah.run/active-operation") == null' >/dev/null ||
		fail "proof PtahSchema already has the active-operation finalizer"
	add_finalizer_patch=$(printf '%s\n' "$current_finalizers" | jq -c '{
      metadata: {finalizers: (. + ["operator.ptah.run/active-operation"])}
    }')
	controller_kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" \
		--type merge -p "$add_finalizer_patch" >/dev/null
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o json |
		jq -e --argjson before "$current_finalizers" '
          (.metadata.finalizers // []) == ($before + ["operator.ptah.run/active-operation"])
        ' >/dev/null || fail "controller identity did not add exactly its active-operation finalizer"
	remove_finalizer_patch=$(printf '%s\n' "$current_finalizers" | jq -c '{metadata: {finalizers: .}}')
	controller_kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" \
		--type merge -p "$remove_finalizer_patch" >/dev/null

	kube -n "$PROOF_NAMESPACE" delete configmap "$CONTROLLER_GUARD_OWNER" --wait=true >/dev/null
	CONTROLLER_GUARD_OWNER=
	controller_write_evidence "$WORK_DIR/controller-write-after.json"
	cmp "$WORK_DIR/controller-write-before.json" "$WORK_DIR/controller-write-after.json" ||
		fail "controller write proof changed anything except the temporary active-operation finalizer"

	start_controller_deployment
	wait_runtime_ready
	clear_controller_impersonation_identity
	printf '%s\n' 'e2e crd: controller desired-state and direct-write boundaries passed'
}

# An active release whose two runtime Deployments were both deleted -- a GitOps
# prune, a namespace-wide delete that spared the release's other objects -- can
# only be restored by an upgrade, so the upgrade has to run in that state
# (stokaro/ptah-operator#10). The reconcile hook finds nothing to stop, and
# Helm creates both Deployments again.
prove_runtime_deployment_recovery() {
	printf '%s\n' 'e2e crd: proving an active release survives losing both runtime Deployments'
	stop_runtime_deployments
	for missing_deployment in "$CONTROLLER_DEPLOYMENT" "$ROTATOR_DEPLOYMENT"; do
		if kube -n "$E2E_OPERATOR_NAMESPACE" get deployment "$missing_deployment" >/dev/null 2>&1; then
			fail "$missing_deployment survived the delete this proof depends on"
		fi
	done
	if ! helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$WORK_DIR/release-values.yaml" \
		--wait --timeout 5m >"$WORK_DIR/recovery-upgrade.out" 2>"$WORK_DIR/recovery-upgrade.err"; then
		if [ "${E2E_DEBUG_LOGS:-0}" -eq 1 ]; then
			cat "$WORK_DIR/recovery-upgrade.err" >&2 || true
		fi
		fail "the upgrade that restores both deleted runtime Deployments was refused"
	fi
	wait_runtime_ready
	printf '%s\n' 'e2e crd: both runtime Deployments were restored by the upgrade'
}

# A running Apply is the one kind of work an upgrade may not interrupt: only
# the database knows what its SQL did, so the successor adopts it rather than
# replacing, completing, or cleaning it. Proving that needs an Apply that is
# genuinely running at the moment the upgrade starts, which is what the
# database barrier below provides: it holds an advisory lock, the Apply's one
# statement asks for the same lock, and the Apply blocks inside the engine
# until the barrier is released (stokaro/ptah-operator#7).
#
# resolve_running_apply_database names the database the barrier and the Apply
# must share. It is the one the fixture's Secret is built from, read from the
# same credentials, because the barrier starts before that Secret exists.
resolve_running_apply_database() {
	resolved_database=$(jq -er '.database' \
		"${E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE:?E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE is required for the running Apply barrier}") ||
		fail "external PostgreSQL credentials name no database for the running Apply barrier"
	printf '%s\n' "$resolved_database" | grep -Eq '^[A-Za-z_][A-Za-z0-9_]*$' ||
		fail "external PostgreSQL credentials name an unusable database for the running Apply barrier"
	printf '%s' "$resolved_database"
}

# Read in the database the lock lives in. pg_locks is cluster-wide, so the
# question could be asked anywhere; asking it where the lock is keeps the
# barrier, the waiter and the reading in one place.
running_apply_postgres_query() {
	docker --context "$E2E_DOCKER_CONTEXT" exec "$E2E_EXTERNAL_POSTGRES_CONTAINER_ID" \
		sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD"; export PGPASSWORD; exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$2" -Atqc "$1"' \
		sh "$1" "$RUNNING_APPLY_BARRIER_DATABASE"
}

running_apply_barrier_contention_query() {
	printf '%s' "SELECT count(*) FROM pg_locks AS waiting JOIN pg_locks AS held USING (locktype, database, classid, objid, objsubid) JOIN pg_stat_activity AS holder ON holder.pid = held.pid WHERE held.locktype = 'advisory' AND held.granted AND NOT waiting.granted AND waiting.pid <> held.pid AND holder.application_name = '$RUNNING_APPLY_BARRIER_APPLICATION'"
}

start_running_apply_barrier() {
	[ "$RUNNING_APPLY_BARRIER_ACTIVE" -eq 0 ] ||
		fail "running Apply database barrier is already active"
	RUNNING_APPLY_BARRIER_DATABASE=$(resolve_running_apply_database)
	docker --context "$E2E_DOCKER_CONTEXT" exec "$E2E_EXTERNAL_POSTGRES_CONTAINER_ID" \
		sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD"; export PGPASSWORD; PGAPPNAME="$1"; export PGAPPNAME; exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$3" -v ON_ERROR_STOP=1 -Atqc "SELECT pg_advisory_lock($2); SELECT pg_sleep(900)"' \
		sh "$RUNNING_APPLY_BARRIER_APPLICATION" "$RUNNING_APPLY_BARRIER_KEY" \
		"$RUNNING_APPLY_BARRIER_DATABASE" \
		>"$WORK_DIR/running-apply-barrier.out" \
		2>"$WORK_DIR/running-apply-barrier.err" &
	RUNNING_APPLY_BARRIER_PID=$!
	RUNNING_APPLY_BARRIER_ACTIVE=1

	barrier_deadline=$(($(date +%s) + 30))
	while [ "$(date +%s)" -lt "$barrier_deadline" ]; do
		held=$(running_apply_postgres_query \
			"SELECT count(*) FROM pg_locks AS lock JOIN pg_stat_activity AS activity USING (pid) WHERE lock.locktype = 'advisory' AND lock.granted AND activity.application_name = '$RUNNING_APPLY_BARRIER_APPLICATION'") ||
			fail "could not inspect the running Apply database barrier"
		if [ "$held" -eq 1 ]; then
			return
		fi
		if ! kill -0 "$RUNNING_APPLY_BARRIER_PID" 2>/dev/null; then
			cat "$WORK_DIR/running-apply-barrier.err" >&2
			fail "running Apply database barrier exited before acquiring its lock"
		fi
		sleep 1
	done
	fail "running Apply database barrier did not acquire its lock"
}

wait_for_running_apply_barrier_contention() {
	contention_deadline=$(($(date +%s) + 120))
	while [ "$(date +%s)" -lt "$contention_deadline" ]; do
		waiting=$(running_apply_postgres_query "$(running_apply_barrier_contention_query)") ||
			fail "could not inspect running Apply barrier contention"
		if [ "$waiting" -eq 1 ]; then
			return
		fi
		sleep 1
	done
	fail "the Apply did not block on the controlled database barrier"
}

assert_running_apply_barrier_contended() {
	waiting=$(running_apply_postgres_query "$(running_apply_barrier_contention_query)") ||
		fail "could not recheck running Apply barrier contention"
	[ "$waiting" -eq 1 ] ||
		fail "the Apply left the controlled database barrier before it was released"
}

release_running_apply_barrier() {
	[ "$RUNNING_APPLY_BARRIER_ACTIVE" -eq 1 ] ||
		fail "running Apply database barrier is not active"
	released=$(running_apply_postgres_query \
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = '$RUNNING_APPLY_BARRIER_APPLICATION' AND pid <> pg_backend_pid()") ||
		fail "could not release the running Apply database barrier"
	[ "$released" = t ] ||
		fail "running Apply database barrier release did not terminate exactly one holder"
	RUNNING_APPLY_BARRIER_ACTIVE=0
	if wait "$RUNNING_APPLY_BARRIER_PID"; then
		fail "running Apply database barrier exited successfully instead of being explicitly released"
	fi
	RUNNING_APPLY_BARRIER_PID=
}

wait_for_successful_fixture_job() {
	fixture_job_name=$1
	fixture_job_deadline=$(($(date +%s) + 300))
	while [ "$(date +%s)" -lt "$fixture_job_deadline" ]; do
		if kube -n "$PROOF_NAMESPACE" get job "$fixture_job_name" -o json \
			>"$WORK_DIR/fixture-job.json" 2>/dev/null; then
			if jq -e '(.status.conditions // []) | any(.type == "Complete" and .status == "True")' \
				"$WORK_DIR/fixture-job.json" >/dev/null; then
				return
			fi
			if jq -e '(.status.conditions // []) | any(.type == "Failed" and .status == "True")' \
				"$WORK_DIR/fixture-job.json" >/dev/null; then
				kube -n "$PROOF_NAMESPACE" logs "job/$fixture_job_name" >&2 2>/dev/null || true
				fail "fixture Job $fixture_job_name failed"
			fi
		fi
		sleep 1
	done
	fail "fixture Job $fixture_job_name did not complete"
}

# prepare_running_apply_fixture builds everything the release under test needs
# to decide, on its own, to apply one long-running statement: a database it can
# reach, an immutable verification policy, a suspended PtahSchema, and a plan
# whose every binding the manager re-derives. The plan's state fingerprints come
# from a real `ptah schema plan` against that database rather than from
# literals, because the Apply refuses a plan recorded against a state it does
# not observe.
prepare_running_apply_fixture() {
	E2E_REGISTRY_CREDENTIALS_FILE=${E2E_REGISTRY_CREDENTIALS_FILE:?E2E_REGISTRY_CREDENTIALS_FILE is required for the running Apply proof}
	E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE=${E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE:?E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE is required for the running Apply proof}
	E2E_EXTERNAL_POSTGRES_IP=${E2E_EXTERNAL_POSTGRES_IP:?E2E_EXTERNAL_POSTGRES_IP is required for the running Apply proof}
	E2E_DOCKER_CONTEXT=${E2E_DOCKER_CONTEXT:?E2E_DOCKER_CONTEXT is required for the running Apply proof}
	E2E_EXTERNAL_POSTGRES_CONTAINER_ID=${E2E_EXTERNAL_POSTGRES_CONTAINER_ID:?E2E_EXTERNAL_POSTGRES_CONTAINER_ID is required for the running Apply proof}
	require_mode_0600_regular_file "$E2E_REGISTRY_CREDENTIALS_FILE" E2E_REGISTRY_CREDENTIALS_FILE
	require_mode_0600_regular_file "$E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE" \
		E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE
	printf '%s\n' "$E2E_EXTERNAL_POSTGRES_IP" | grep -Eq '^[0-9]+(\.[0-9]+){3}$' ||
		fail "E2E_EXTERNAL_POSTGRES_IP must be an IPv4 address"
	case "$E2E_DOCKER_CONTEXT" in
	'' | default | orbstack) fail "E2E_DOCKER_CONTEXT must name an explicit allowed remote context" ;;
	esac
	printf '%s\n' "$E2E_EXTERNAL_POSTGRES_CONTAINER_ID" | grep -Eq '^[0-9a-f]{64}$' ||
		fail "E2E_EXTERNAL_POSTGRES_CONTAINER_ID must be an exact Docker container ID"
	running_apply_container_id=$(docker --context "$E2E_DOCKER_CONTEXT" container inspect \
		--format '{{.Id}}' "$E2E_EXTERNAL_POSTGRES_CONTAINER_ID") ||
		fail "could not inspect the external PostgreSQL barrier container"
	[ "$running_apply_container_id" = "$E2E_EXTERNAL_POSTGRES_CONTAINER_ID" ] ||
		fail "external PostgreSQL barrier container identity changed"
	# Inspecting a container says it exists, not that it is serving, and the two
	# failures look identical from inside the cluster: a Pod dialing the barrier
	# Service reports `connection refused` whether the backend is down or the
	# Service has no programmed endpoint yet. Separating them here costs one
	# call and names the first one before any Pod can blame the second.
	running_apply_container_running=$(docker --context "$E2E_DOCKER_CONTEXT" container inspect \
		--format '{{.State.Running}}' "$E2E_EXTERNAL_POSTGRES_CONTAINER_ID")
	[ "$running_apply_container_running" = "true" ] ||
		fail "the external PostgreSQL barrier container is not running"

	# The executor the Apply will run is the one the live release configured,
	# read from the controller it dispatched with rather than from a value file
	# the proof could get wrong.
	runtime_deployment_names
	running_apply_executor_image=$(kube -n "$E2E_OPERATOR_NAMESPACE" get deployment \
		"$CONTROLLER_DEPLOYMENT" -o json | jq -er '
          [.spec.template.spec.containers[] | select(.name == "manager") |
            (.args // [])[] | select(startswith("--executor-image=")) |
            ltrimstr("--executor-image=")] as $images |
          if ($images | length) == 1 and ($images[0] | length) > 0
          then $images[0]
          else error("the controller must carry one executor image")
          end
        ') || fail "could not read the live executor image"
	printf '%s\n' "$running_apply_executor_image" |
		grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' ||
		fail "the live executor image is not digest-pinned"
	running_apply_registry=${running_apply_executor_image%%/*}

	jq -n \
		--arg namespace "$PROOF_NAMESPACE" \
		--arg name "$RUNNING_APPLY_PULL_SECRET" \
		--arg registry "$running_apply_registry" \
		--slurpfile credentials "$E2E_REGISTRY_CREDENTIALS_FILE" '
      {
        apiVersion: "v1", kind: "Secret", immutable: true,
        metadata: {namespace: $namespace, name: $name},
        type: "kubernetes.io/dockerconfigjson",
        data: {
          ".dockerconfigjson": ({auths: {($registry): {
            username: $credentials[0].username,
            password: $credentials[0].password,
            auth: (($credentials[0].username + ":" + $credentials[0].password) | @base64)
          }}} | tojson | @base64)
        }
      }
    ' | kube create -f - >/dev/null

	jq -n \
		--arg namespace "$PROOF_NAMESPACE" \
		--arg name "$RUNNING_APPLY_DATABASE" \
		--arg authority "$RUNNING_APPLY_DATABASE:5432" \
		--slurpfile credentials "$E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE" '
      {
        apiVersion: "v1", kind: "Secret", immutable: true,
        metadata: {namespace: $namespace, name: $name},
        stringData: {
          url: ("postgres://" + $credentials[0].username + ":" + $credentials[0].password +
            "@" + $authority + "/" + $credentials[0].database + "?sslmode=disable")
        }
      }
    ' | kube create -f - >/dev/null
	jq -n \
		--arg namespace "$PROOF_NAMESPACE" \
		--arg name "$RUNNING_APPLY_DATABASE" '
      {
        apiVersion: "v1", kind: "Service",
        metadata: {namespace: $namespace, name: $name},
        spec: {ports: [{name: "postgresql", port: 5432, protocol: "TCP", targetPort: 5432}]}
      }
    ' | kube create -f - >/dev/null
	running_apply_service_uid=$(kube -n "$PROOF_NAMESPACE" get service \
		"$RUNNING_APPLY_DATABASE" -o jsonpath='{.metadata.uid}')
	jq -n \
		--arg namespace "$PROOF_NAMESPACE" \
		--arg name "${RUNNING_APPLY_DATABASE}-docker" \
		--arg service "$RUNNING_APPLY_DATABASE" \
		--arg serviceUID "$running_apply_service_uid" \
		--arg address "$E2E_EXTERNAL_POSTGRES_IP" '
      {
        apiVersion: "discovery.k8s.io/v1", kind: "EndpointSlice",
        metadata: {
          namespace: $namespace, name: $name,
          labels: {
            "kubernetes.io/service-name": $service,
            "endpointslice.kubernetes.io/managed-by": "ptah-operator-e2e"
          },
          ownerReferences: [{
            apiVersion: "v1", kind: "Service", name: $service, uid: $serviceUID,
            controller: true, blockOwnerDeletion: false
          }]
        },
        addressType: "IPv4",
        endpoints: [{addresses: [$address], conditions: {ready: true}}],
        ports: [{name: "postgresql", port: 5432, protocol: "TCP"}]
      }
    ' | kube create -f - >/dev/null

	running_apply_policy_file=$WORK_DIR/running-apply-policy.yaml
	printf '%s\n' 'version: 1' >"$running_apply_policy_file"
	kube -n "$PROOF_NAMESPACE" create configmap "$RUNNING_APPLY_POLICY" \
		--from-file="policy.yaml=$running_apply_policy_file" >/dev/null
	kube -n "$PROOF_NAMESPACE" patch configmap "$RUNNING_APPLY_POLICY" --type=merge \
		-p='{"immutable":true}' >/dev/null
	running_apply_policy_uid=$(kube -n "$PROOF_NAMESPACE" get configmap "$RUNNING_APPLY_POLICY" \
		-o jsonpath='{.metadata.uid}')

	running_apply_artifact_digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
	kube -n "$PROOF_NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: $RUNNING_APPLY_SCHEMA
spec:
  suspend: true
  interval: 24h
  target:
    engine: PostgreSQL
    coordinationKey: $RUNNING_APPLY_SCHEMA
    urlFrom: {name: $RUNNING_APPLY_DATABASE, key: url}
  desired:
    ociRef: oci://example.invalid/schema@$running_apply_artifact_digest
    verificationPolicyFrom: {name: $RUNNING_APPLY_POLICY, key: policy.yaml}
  policy:
    apply: Always
    allowDestructive: false
    driftSeverity: all
    lockTimeout: 30s
    transactionMode: file
  execution:
    activeDeadlineSeconds: 600
    failureRetryInterval: 30s
    connectTimeout: 10s
    serviceAccountName: default
    imagePullSecrets: [{name: $RUNNING_APPLY_PULL_SECRET}]
EOF
	wait_for_suspended "$RUNNING_APPLY_SCHEMA"

	running_apply_plan_source=$WORK_DIR/running-apply-schema.sql
	cp "$ROOT_DIR/testdata/e2e/postgresql-v1.sql" "$running_apply_plan_source"
	kube -n "$PROOF_NAMESPACE" create configmap running-apply-plan-source \
		--from-file="schema.sql=$running_apply_plan_source" >/dev/null
	# The Job dials the barrier Service seconds after the Service and its
	# EndpointSlice were created, and a ClusterIP whose endpoint kube-proxy has
	# not programmed yet is rejected rather than dropped, so the executor sees
	# `connection refused` on its first packet and its connect timeout never
	# applies. Measured on run 35282131046, `Kubernetes 1.35 lifecycle`: the
	# diagnostics dump has the Service at AGE 5s and the Pod already in Error
	# inside those same five seconds.
	#
	# backoffLimit gives the dial a second and third chance in a fresh Pod,
	# which is the only probe of that path the fixture has. It hides nothing: a
	# barrier that is genuinely down fails every attempt with the same message,
	# and a barrier container that is not running is refused above, by name,
	# before any Pod runs.
	jq -n \
		--arg namespace "$PROOF_NAMESPACE" \
		--arg image "$running_apply_executor_image" \
		--arg pullSecret "$RUNNING_APPLY_PULL_SECRET" \
		--arg databaseSecret "$RUNNING_APPLY_DATABASE" '
      {
        apiVersion: "batch/v1", kind: "Job",
        metadata: {namespace: $namespace, name: "running-apply-plan-source"},
        spec: {
          backoffLimit: 3, activeDeadlineSeconds: 180, ttlSecondsAfterFinished: 300,
          template: {
            metadata: {labels: {"app.kubernetes.io/component": "running-apply-plan-source"}},
            spec: {
              restartPolicy: "Never", automountServiceAccountToken: false,
              imagePullSecrets: [{name: $pullSecret}],
              securityContext: {
                runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532,
                seccompProfile: {type: "RuntimeDefault"}
              },
              containers: [{
                name: "planner", image: $image, imagePullPolicy: "IfNotPresent",
                command: ["/usr/local/bin/ptah"], args: ["schema", "plan", "--dry-run"],
                env: [
                  {name: "HOME", value: "/work"}, {name: "TMPDIR", value: "/work"},
                  {name: "PTAH_SCHEMA_FILE", value: "/schema/schema.sql"},
                  {name: "PTAH_CONNECT_TIMEOUT", value: "10s"},
                  {name: "PTAH_LOCK_TIMEOUT", value: "30s"},
                  {name: "PTAH_DB_URL", valueFrom: {secretKeyRef: {name: $databaseSecret, key: "url"}}}
                ],
                securityContext: {
                  allowPrivilegeEscalation: false, readOnlyRootFilesystem: true,
                  capabilities: {drop: ["ALL"]}
                },
                volumeMounts: [
                  {name: "schema", mountPath: "/schema", readOnly: true},
                  {name: "work", mountPath: "/work"}
                ]
              }],
              volumes: [
                {name: "schema", configMap: {name: "running-apply-plan-source"}},
                {name: "work", emptyDir: {sizeLimit: "64Mi"}}
              ]
            }
          }
        }
      }
    ' | kube create -f - >/dev/null
	wait_for_successful_fixture_job running-apply-plan-source
	# The plan comes from the Pod that succeeded, by name. `logs job/<name>`
	# picks one Pod of the Job's label set, and a retried Job has more than one:
	# a failed attempt's output would be read as the plan and fail the shape
	# check below with a reason that is about neither.
	running_apply_plan_pod=$(kube -n "$PROOF_NAMESPACE" get pods \
		-l app.kubernetes.io/component=running-apply-plan-source \
		--field-selector=status.phase=Succeeded \
		-o jsonpath='{.items[0].metadata.name}')
	[ -n "$running_apply_plan_pod" ] ||
		fail "the running Apply plan Job completed without a succeeded Pod"
	kube -n "$PROOF_NAMESPACE" logs "$running_apply_plan_pod" \
		>"$WORK_DIR/running-apply-native-plan.json"
	jq -ce \
		--arg plan_name "$RUNNING_APPLY_SCHEMA" \
		--argjson key "$RUNNING_APPLY_BARRIER_KEY" '
      if .format_version == 1 and
        (.from_fingerprint | test("^sha256:[0-9a-f]{64}$")) and
        (.to_fingerprint | test("^sha256:[0-9a-f]{64}$"))
      then
        .name = $plan_name |
        .destructive = false |
        .statements = [{
          sql: ("SELECT pg_advisory_lock(" + ($key | tostring) + ")"), severity: "safe",
          reason: "upgrade quiescence proof"
        }]
      else error("native plan lacks exact state fingerprints")
      end
    ' "$WORK_DIR/running-apply-native-plan.json" >"$WORK_DIR/running-apply-plan.json" ||
		fail "could not derive an exact long-running Apply plan"

	kube -n "$PROOF_NAMESPACE" get ptahschema "$RUNNING_APPLY_SCHEMA" -o json \
		>"$WORK_DIR/running-apply-schema.json"
	running_apply_database_url=$(kube -n "$PROOF_NAMESPACE" get secret \
		"$RUNNING_APPLY_DATABASE" -o jsonpath='{.data.url}' | base64 -d)
	printf '%s\n' "$running_apply_database_url" | grep -Eq '^postgres://' ||
		fail "running Apply database secret does not carry a postgres URL"
	# The exact URL the Apply Job resolves. The runner derives the target
	# identity from it and refuses a plan recorded against a different one, so
	# the fixture binds this value rather than a placeholder.
	# The plan records the manager that publishes it. The status binding holds
	# only what the plan binds, so the manager's identity is read from a Job it
	# dispatched: the read-only fixture this proof dispatches first.
	running_apply_manager_job=$WORK_DIR/$READ_ONLY_JOB_SCHEMA-read-only-job.json
	{ [ -n "$READ_ONLY_JOB_SCHEMA" ] && [ -s "$running_apply_manager_job" ]; } ||
		fail "the running Apply fixture needs a Job the live manager dispatched; the read-only Job fixture runs first"
	go -C "$ROOT_DIR" run ./hack/predecessorapplyfixture \
		-schema "$WORK_DIR/running-apply-schema.json" \
		-manager-job "$running_apply_manager_job" \
		-plan "$WORK_DIR/running-apply-plan.json" \
		-policy-uid "$running_apply_policy_uid" \
		-policy "$running_apply_policy_file" \
		-database-url "$running_apply_database_url" \
		>"$WORK_DIR/running-apply-bundle.json"
	jq -e '
      .plan.spec.contractVersion == 3 and
      (.plan.spec.controllerImage | test("^[^[:space:]@]+@sha256:[0-9a-f]{64}$")) and
      (.plan.spec.controllerRevision | length) > 0 and
      .plan.spec.controllerStateVersion >= 1 and
      (.plan.spec.chunks | length) == 1
    ' "$WORK_DIR/running-apply-bundle.json" >/dev/null ||
		fail "the generated Apply plan does not carry the current manager contract"
	jq '.plan' "$WORK_DIR/running-apply-bundle.json" | kube create -f - >/dev/null
	RUNNING_APPLY_PLAN_NAME=$(jq -er '.plan.metadata.name' "$WORK_DIR/running-apply-bundle.json")
	RUNNING_APPLY_PLAN_UID=$(kube -n "$PROOF_NAMESPACE" get ptahschemaplan \
		"$RUNNING_APPLY_PLAN_NAME" -o jsonpath='{.metadata.uid}')
	running_apply_plan_generation=$(kube -n "$PROOF_NAMESPACE" get ptahschemaplan \
		"$RUNNING_APPLY_PLAN_NAME" -o jsonpath='{.metadata.generation}')
	running_apply_chunk_name=$(jq -er '.plan.spec.chunks[0].name' "$WORK_DIR/running-apply-bundle.json")
	jq -n \
		--arg namespace "$PROOF_NAMESPACE" \
		--arg name "$running_apply_chunk_name" \
		--arg plan "$RUNNING_APPLY_PLAN_NAME" \
		--arg planUID "$RUNNING_APPLY_PLAN_UID" \
		--arg schema "$RUNNING_APPLY_SCHEMA" \
		--rawfile content "$WORK_DIR/running-apply-plan.json" '
      {
        apiVersion: "v1", kind: "ConfigMap", immutable: true,
        metadata: {
          namespace: $namespace, name: $name,
          labels: {"operator.ptah.run/plan": $plan, "operator.ptah.run/schema": $schema},
          ownerReferences: [{
            apiVersion: "operator.ptah.run/v1alpha1", kind: "PtahSchemaPlan",
            name: $plan, uid: $planUID, controller: true, blockOwnerDeletion: true
          }]
        },
        binaryData: {chunk: ($content | @base64)}
      }
    ' | kube create -f - >/dev/null
	running_apply_chunk_uid=$(kube -n "$PROOF_NAMESPACE" get configmap "$running_apply_chunk_name" \
		-o jsonpath='{.metadata.uid}')
	running_apply_plan_ready_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
	kube -n "$PROOF_NAMESPACE" patch ptahschemaplan "$RUNNING_APPLY_PLAN_NAME" \
		--subresource=status --type=merge \
		-p "{\"status\":{\"observedGeneration\":$running_apply_plan_generation,\"publishedChunks\":[{\"name\":\"$running_apply_chunk_name\",\"uid\":\"$running_apply_chunk_uid\",\"index\":0}],\"conditions\":[{\"type\":\"Ready\",\"status\":\"True\",\"reason\":\"Published\",\"message\":\"Verified 1 immutable plan chunks\",\"observedGeneration\":$running_apply_plan_generation,\"lastTransitionTime\":\"$running_apply_plan_ready_at\"}]}}" >/dev/null
}

# start_running_apply_fixture hands the manager the status that makes the Apply
# its own decision, and waits until the Apply Pod is actually running. The
# controller is stopped while the status is written so that nothing reconciles a
# half-written state, which is the same fence the read-only fixture uses.
start_running_apply_fixture() {
	[ -n "$RUNNING_APPLY_PLAN_NAME" ] || fail "running Apply plan name is missing"
	[ -n "$RUNNING_APPLY_PLAN_UID" ] || fail "running Apply plan UID is missing"
	stop_controller_deployment
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$RUNNING_APPLY_SCHEMA" --type=merge \
		-p='{"spec":{"suspend":false}}' >/dev/null
	kube -n "$PROOF_NAMESPACE" get ptahschema "$RUNNING_APPLY_SCHEMA" -o json \
		>"$WORK_DIR/running-apply-schema-enabled.json"
	running_apply_generation=$(jq -er '.metadata.generation' \
		"$WORK_DIR/running-apply-schema-enabled.json")
	jq \
		--slurpfile bundle "$WORK_DIR/running-apply-bundle.json" \
		--arg planUID "$RUNNING_APPLY_PLAN_UID" \
		--argjson generation "$running_apply_generation" '
      .status = $bundle[0].schemaStatus |
      .status.observedGeneration = $generation |
      .status.plan.uid = $planUID |
      (.status.conditions[].observedGeneration) = $generation
    ' "$WORK_DIR/running-apply-schema-enabled.json" \
		>"$WORK_DIR/running-apply-schema-ready.json"
	kube replace --subresource=status -f "$WORK_DIR/running-apply-schema-ready.json" >/dev/null
	start_controller_deployment
	kube -n "$E2E_OPERATOR_NAMESPACE" rollout status deployment "$CONTROLLER_DEPLOYMENT" \
		--timeout=3m >/dev/null

	running_apply_deadline=$(($(date +%s) + 300))
	while [ "$(date +%s)" -lt "$running_apply_deadline" ]; do
		kube -n "$PROOF_NAMESPACE" get ptahschema "$RUNNING_APPLY_SCHEMA" -o json \
			>"$WORK_DIR/running-apply-live-schema.json"
		if jq -e '
          .status.pendingObservation.outcome == "OutcomeUnknown" or
          ((.status.conditions // []) | any(
            .type == "ReconciliationFailed" and .status == "True"
          ))
        ' "$WORK_DIR/running-apply-live-schema.json" >/dev/null; then
			emit_running_apply_diagnostic
			fail "the Apply reached a terminal failure before its Pod was observed running"
		fi
		RUNNING_APPLY_JOB_NAME=$(jq -r \
			'.status.activeOperation | select(.type == "Apply" and .dispatchStarted == true) | .jobName // empty' \
			"$WORK_DIR/running-apply-live-schema.json")
		running_apply_committed_uid=$(jq -r '.status.activeOperation.jobUID // empty' \
			"$WORK_DIR/running-apply-live-schema.json")
		if [ -n "$RUNNING_APPLY_JOB_NAME" ] && [ -n "$running_apply_committed_uid" ] &&
			kube -n "$PROOF_NAMESPACE" get job "$RUNNING_APPLY_JOB_NAME" -o json \
				>"$WORK_DIR/running-apply-job.json" 2>/dev/null; then
			RUNNING_APPLY_JOB_UID=$(jq -r '.metadata.uid' "$WORK_DIR/running-apply-job.json")
			if [ "$running_apply_committed_uid" = "$RUNNING_APPLY_JOB_UID" ]; then
				kube -n "$PROOF_NAMESPACE" get pods -o json |
					jq -e --arg uid "$RUNNING_APPLY_JOB_UID" '
                      [.items[] | select(
                        .status.phase == "Running" and
                        any(.metadata.ownerReferences[]?;
                          .apiVersion == "batch/v1" and .kind == "Job" and .uid == $uid and
                          .controller == true
                        )
                      )] | if length == 1 then .[0] else empty end
                    ' >"$WORK_DIR/running-apply-pod.json" 2>/dev/null || true
				if [ -s "$WORK_DIR/running-apply-pod.json" ]; then
					RUNNING_APPLY_POD_NAME=$(jq -er '.metadata.name' "$WORK_DIR/running-apply-pod.json")
					RUNNING_APPLY_POD_UID=$(jq -er '.metadata.uid' "$WORK_DIR/running-apply-pod.json")
					break
				fi
			fi
		fi
		sleep 1
	done
	if [ -z "$RUNNING_APPLY_POD_UID" ]; then
		emit_running_apply_diagnostic
		fail "the Apply Job did not reach a running Pod"
	fi
	jq -e --arg schema "$RUNNING_APPLY_SCHEMA" '
      .metadata.labels["operator.ptah.run/schema"] == $schema and
      .metadata.labels["operator.ptah.run/operation"] == "apply" and
      (.metadata.annotations["operator.ptah.run/plan-fingerprint"] |
        test("^sha256:[0-9a-f]{64}$")) and
      (.metadata.annotations["operator.ptah.run/admission-snapshot-digest"] |
        test("^sha256:[0-9a-f]{64}$")) and
      .spec.template.metadata.annotations == .metadata.annotations and
      (.spec | has("ttlSecondsAfterFinished") | not)
    ' "$WORK_DIR/running-apply-job.json" >/dev/null ||
		fail "the running Apply Job does not carry its dispatched operation identity"
	wait_for_running_apply_barrier_contention
}

emit_running_apply_diagnostic() {
	running_apply_diagnostic=$WORK_DIR/running-apply-diagnostic.json
	(umask 077 && : >"$running_apply_diagnostic")
	kube -n "$PROOF_NAMESPACE" get ptahschema "$RUNNING_APPLY_SCHEMA" -o json 2>/dev/null |
		jq -c '{
          phase: .status.phase,
          activeOperation: .status.activeOperation,
          pendingObservation: .status.pendingObservation,
          conditions: [(.status.conditions // [])[] | {type, status, reason}]
        }' >>"$running_apply_diagnostic" 2>/dev/null || true
	kube -n "$PROOF_NAMESPACE" get jobs -l "operator.ptah.run/schema=$RUNNING_APPLY_SCHEMA" -o json 2>/dev/null |
		jq -c '[.items[] | {name: .metadata.name, uid: .metadata.uid, conditions: [(.status.conditions // [])[] | {type, status}]}]' \
			>>"$running_apply_diagnostic" 2>/dev/null || true
	printf 'e2e crd: running Apply diagnostic written to %s\n' "$running_apply_diagnostic" >&2
	cat "$running_apply_diagnostic" >&2 || true
}

# assert_staged_uid_gap holds one condition of the staged gap and says which
# one failed.
#
# One message for five conditions cost a whole lifecycle to diagnose: the phase
# stops at its first failure, the run is ninety minutes, and the log said only
# that the gap was not retained. Each condition names itself now, and the
# schema's own status is printed beside it.
assert_staged_uid_gap() {
	staged_gap_filter=$1
	staged_gap_reason=$2
	jq -e --arg name "$RUNNING_APPLY_JOB_NAME" "$staged_gap_filter" \
		"$WORK_DIR/running-apply-staged-gap.json" >/dev/null && return 0
	emit_running_apply_diagnostic
	fail "the Apply fixture did not retain the running late-create UID gap: $staged_gap_reason"
}

# The UID-adoption boundary: a Job the manager created but whose UID it had not
# yet recorded is still that manager's work. The successor has to adopt it by
# name and then by UID, before any Pod discovery.
stage_predecessor_apply_job_uid_gap_while_running() {
	[ -n "$RUNNING_APPLY_JOB_NAME" ] || fail "running Apply Job name is missing"
	[ -n "$RUNNING_APPLY_JOB_UID" ] || fail "running Apply Job UID is missing"
	[ -n "$RUNNING_APPLY_POD_UID" ] || fail "running Apply Pod UID is missing"
	kube -n "$PROOF_NAMESPACE" get job "$RUNNING_APPLY_JOB_NAME" -o json \
		>"$WORK_DIR/running-apply-before-upgrade.json"
	jq -e --arg uid "$RUNNING_APPLY_JOB_UID" '
      .metadata.uid == $uid and
      ((.status.conditions // []) |
        any((.type == "Complete" or .type == "Failed") and .status == "True") | not) and
      (.spec | has("ttlSecondsAfterFinished") | not)
    ' "$WORK_DIR/running-apply-before-upgrade.json" >/dev/null ||
		fail "the Apply Job is not running at the upgrade boundary"
	kube -n "$PROOF_NAMESPACE" get pod "$RUNNING_APPLY_POD_NAME" -o json |
		jq -e --arg uid "$RUNNING_APPLY_POD_UID" '
          .metadata.uid == $uid and .status.phase == "Running"
        ' >/dev/null || fail "the Apply Pod is not running at the upgrade boundary"
	jq -S '{
      uid: .metadata.uid,
      name: .metadata.name,
      namespace: .metadata.namespace,
      labels: .metadata.labels,
      annotations: .metadata.annotations,
      ownerReferences: .metadata.ownerReferences,
      finalizers: (.metadata.finalizers // []),
      spec: (.spec | del(.ttlSecondsAfterFinished))
    }' "$WORK_DIR/running-apply-before-upgrade.json" \
		>"$WORK_DIR/running-apply-job-before-cleanup.json"
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$RUNNING_APPLY_SCHEMA" --subresource=status \
		--type=json -p='[{"op":"remove","path":"/status/activeOperation/jobUID"}]' >/dev/null
	kube -n "$PROOF_NAMESPACE" get ptahschema "$RUNNING_APPLY_SCHEMA" -o json \
		>"$WORK_DIR/running-apply-staged-gap.json"
	assert_staged_uid_gap '.status.activeOperation.type == "Apply"' \
		'the claim is no longer an Apply'
	assert_staged_uid_gap '.status.activeOperation.dispatchStarted == true' \
		'the claim no longer records a started dispatch'
	# shellcheck disable=SC2016 # $name is a jq argument, not a shell variable.
	assert_staged_uid_gap '.status.activeOperation.jobName == $name' \
		'the claim names another Job'
	assert_staged_uid_gap '.status.activeOperation | has("jobUID") | not' \
		'the manager recorded the Job UID again before the upgrade'
	assert_staged_uid_gap '.status | has("pendingObservation") | not' \
		'the Apply was already retired into a pending observation'
}

# The next release differs from the one that dispatched the Apply in its
# manager image alone, which the execution binding does not hold. So the
# successor keeps the epoch and adopts the running Apply as its own: the claim
# stays, the Job UID the harness removed is recorded again, and nothing is
# retired into a pending observation while the Apply is still inside the
# engine.
assert_predecessor_apply_remains_exclusive_while_running() {
	# shellcheck disable=SC2016 # $before, $job and $uid are jq arguments, not shell variables.
	exclusive_filter='
      $before[0].status as $before |
      .status.executionBinding.epoch == $before.executionBinding.epoch and
      .status.activeOperation.type == "Apply" and
      .status.activeOperation.id == $before.activeOperation.id and
      .status.activeOperation.jobName == $job and
      .status.activeOperation.jobUID == $uid and
      .status.activeOperation.dispatchStarted == true and
      .status.activeOperation.executionBindingID == .status.executionBinding.epoch and
      (.status | has("pendingObservation") | not) and
      ((.status.conditions // []) | any(.reason == "ExecutionBindingChanged") | not)
    '
	exclusive_deadline=$(($(date +%s) + 180))
	while [ "$(date +%s)" -lt "$exclusive_deadline" ]; do
		if kube -n "$PROOF_NAMESPACE" get ptahschema "$RUNNING_APPLY_SCHEMA" -o json \
			>"$WORK_DIR/running-apply-fenced-schema.json" 2>/dev/null &&
			jq -e \
				--arg job "$RUNNING_APPLY_JOB_NAME" \
				--arg uid "$RUNNING_APPLY_JOB_UID" \
				--slurpfile before "$WORK_DIR/running-apply-staged-gap.json" \
				"$exclusive_filter" "$WORK_DIR/running-apply-fenced-schema.json" >/dev/null; then
			break
		fi
		sleep 1
	done
	jq -e \
		--arg job "$RUNNING_APPLY_JOB_NAME" \
		--arg uid "$RUNNING_APPLY_JOB_UID" \
		--slurpfile before "$WORK_DIR/running-apply-staged-gap.json" \
		"$exclusive_filter" "$WORK_DIR/running-apply-fenced-schema.json" >/dev/null || {
		emit_running_apply_diagnostic
		fail "the successor did not adopt the running Apply under the epoch it was dispatched in"
	}

	kube -n "$PROOF_NAMESPACE" get job "$RUNNING_APPLY_JOB_NAME" -o json \
		>"$WORK_DIR/running-apply-after-upgrade.json"
	jq -e --arg uid "$RUNNING_APPLY_JOB_UID" '
      .metadata.uid == $uid and
      ((.status.conditions // []) |
        any((.type == "Complete" or .type == "Failed") and .status == "True") | not) and
      (.spec | has("ttlSecondsAfterFinished") | not)
    ' "$WORK_DIR/running-apply-after-upgrade.json" >/dev/null ||
		fail "the successor replaced, completed, or cleaned the running Apply Job"
	kube -n "$PROOF_NAMESPACE" get pod "$RUNNING_APPLY_POD_NAME" -o json |
		jq -e --arg uid "$RUNNING_APPLY_POD_UID" '
          .metadata.uid == $uid and .status.phase == "Running"
        ' >/dev/null || fail "the upgrade did not retain the running Apply Pod UID"
	kube -n "$PROOF_NAMESPACE" get jobs \
		-l "operator.ptah.run/schema=$RUNNING_APPLY_SCHEMA" -o json |
		jq -e --arg name "$RUNNING_APPLY_JOB_NAME" --arg uid "$RUNNING_APPLY_JOB_UID" '
          .items | length == 1 and .[0].metadata.name == $name and .[0].metadata.uid == $uid
        ' >/dev/null || fail "the successor launched new work over the running Apply"
	# The Apply is still inside the engine, waiting on the barrier: exclusivity
	# that held because the Apply had already finished would prove nothing.
	assert_running_apply_barrier_contended
}

wait_for_predecessor_apply_job_terminal() {
	terminal_deadline=$(($(date +%s) + 300))
	while [ "$(date +%s)" -lt "$terminal_deadline" ]; do
		if kube -n "$PROOF_NAMESPACE" get job "$RUNNING_APPLY_JOB_NAME" -o json \
			>"$WORK_DIR/running-apply-terminal-job.json" 2>/dev/null &&
			jq -e '(.status.conditions // []) | any((.type == "Complete" or .type == "Failed") and .status == "True")' \
				"$WORK_DIR/running-apply-terminal-job.json" >/dev/null; then
			break
		fi
		sleep 1
	done
	jq -e --arg uid "$RUNNING_APPLY_JOB_UID" '
      .metadata.uid == $uid and
      ((.status.conditions // []) | any((.type == "Complete" or .type == "Failed") and .status == "True"))
    ' "$WORK_DIR/running-apply-terminal-job.json" >/dev/null ||
		fail "the Apply Job did not finish after the successor adopted it"
	kube -n "$PROOF_NAMESPACE" get pod "$RUNNING_APPLY_POD_NAME" -o json |
		jq -e --arg uid "$RUNNING_APPLY_POD_UID" '
          .metadata.uid == $uid and (.status.phase == "Succeeded" or .status.phase == "Failed")
        ' >/dev/null || fail "the Apply Pod is not terminal after the successor adopted it"
}

# The adopted Apply is accounted for through its own result, under the epoch
# it was dispatched in: the successor read the frame, scheduled the Job's
# cleanup and recorded what the run said -- applied, or unknown when the frame
# says so -- for the read-only observation that follows. Either account names
# this Job; a fence would have moved the epoch instead. It also names the
# manager that dispatched the Job, read from the Job's own Pod template: the
# predecessor, not the successor that harvested it, because the Job is
# collected five minutes later and the record is what is left.
wait_for_predecessor_apply_job_cleanup() {
	cleanup_deadline=$(($(date +%s) + 240))
	while [ "$(date +%s)" -lt "$cleanup_deadline" ]; do
		if kube -n "$PROOF_NAMESPACE" get job "$RUNNING_APPLY_JOB_NAME" -o json \
			>"$WORK_DIR/running-apply-job-after.json" 2>/dev/null &&
			[ "$(jq -r '.spec.ttlSecondsAfterFinished // 0' "$WORK_DIR/running-apply-job-after.json")" -eq 300 ] &&
			kube -n "$PROOF_NAMESPACE" get ptahschema "$RUNNING_APPLY_SCHEMA" -o json \
				>"$WORK_DIR/running-apply-schema-after.json"; then
			running_apply_dispatcher=$(jq -r \
				'.spec.template.metadata.annotations["operator.ptah.run/controller-image"] // ""' \
				"$WORK_DIR/running-apply-job-after.json")
			if [ -z "$running_apply_dispatcher" ] ||
				[ "$running_apply_dispatcher" = "$E2E_NEXT_CONTROLLER_IMAGE" ]; then
				fail "the adopted Apply Job does not record the predecessor that dispatched it"
			fi
			if jq -e \
				--arg job "$RUNNING_APPLY_JOB_NAME" \
				--arg uid "$RUNNING_APPLY_JOB_UID" \
				--arg planUID "$RUNNING_APPLY_PLAN_UID" \
				--arg dispatcher "$running_apply_dispatcher" \
				--slurpfile before "$WORK_DIR/running-apply-staged-gap.json" '
                  $before[0].status as $before |
                  .status.executionBinding.epoch == $before.executionBinding.epoch and
                  ((.status.conditions // []) | any(.reason == "ExecutionBindingChanged") | not) and
                  ((.status.pendingObservation.applyJobName == $job and
                    .status.pendingObservation.applyJobUID == $uid and
                    (.status.pendingObservation.outcome == "ApplySucceeded" or
                      .status.pendingObservation.outcome == "OutcomeUnknown") and
                    .status.pendingObservation.plan.executionBindingID == .status.executionBinding.epoch and
                    .status.pendingObservation.dispatchedBy.controllerImage == $dispatcher) or
                   (.status.applied.planRef.uid == $planUID and
                    .status.applied.executionBindingID == .status.executionBinding.epoch and
                    .status.applied.dispatchedBy.controllerImage == $dispatcher))
                ' "$WORK_DIR/running-apply-schema-after.json" >/dev/null; then
				jq -S '{
                  uid: .metadata.uid,
                  name: .metadata.name,
                  namespace: .metadata.namespace,
                  labels: .metadata.labels,
                  annotations: .metadata.annotations,
                  ownerReferences: .metadata.ownerReferences,
                  finalizers: (.metadata.finalizers // []),
                  spec: (.spec | del(.ttlSecondsAfterFinished))
                }' "$WORK_DIR/running-apply-job-after.json" \
					>"$WORK_DIR/running-apply-job-after-cleanup.json"
				cmp "$WORK_DIR/running-apply-job-before-cleanup.json" \
					"$WORK_DIR/running-apply-job-after-cleanup.json" ||
					fail "the successor changed the adopted Apply Job outside ttlSecondsAfterFinished"
				return
			fi
		fi
		sleep 1
	done
	emit_running_apply_diagnostic
	fail "the successor did not account for the adopted Apply Job through its own result"
}

assert_controller_downgrade_blocked() {
	assert_explicit_runtime_guard "future stored controller state" controller 2
	rotator_ready=$(kube -n "$E2E_OPERATOR_NAMESPACE" get deployment "$ROTATOR_DEPLOYMENT" \
		-o jsonpath='{.status.availableReplicas}' 2>/dev/null || true)
	[ "$rotator_ready" = 1 ] ||
		fail "controller-only downgrade preflight prevented the certificate rotator from remaining ready"
}

prove_runtime_singleton_guard() {
	printf '%s\n' 'e2e crd: proving runtime rejection of an incomplete singleton'
	kube get validatingwebhookconfiguration ptah-operator-admission -o json |
		jq 'del(.metadata.creationTimestamp, .metadata.generation, .metadata.managedFields, .metadata.resourceVersion, .metadata.uid)' \
		>"$WORK_DIR/validating-webhook.json"
	stop_runtime_deployments
	kube delete validatingwebhookconfiguration ptah-operator-admission >/dev/null
	start_runtime_deployments
	assert_runtime_blocked "incomplete admission singleton"
	kube create -f "$WORK_DIR/validating-webhook.json" >/dev/null
	kube -n "$E2E_OPERATOR_NAMESPACE" delete pod \
		-l "app.kubernetes.io/instance=$E2E_HELM_RELEASE" --wait=false >/dev/null
	wait_runtime_ready

	# The runtime refuses to serve an admission singleton another release owns.
	# Nothing stops an administrator writing that annotation, so the refusal is
	# the runtime's: its Pods do not start until the singleton names this
	# release again.
	printf '%s\n' 'e2e crd: proving the runtime refuses an admission singleton another release owns'
	stop_runtime_deployments
	kube annotate mutatingwebhookconfiguration ptah-operator-admission \
		operator.ptah.run/release-name=foreign-release --overwrite >/dev/null
	start_runtime_deployments
	assert_runtime_blocked "foreign admission singleton owner"
	kube annotate mutatingwebhookconfiguration ptah-operator-admission \
		"operator.ptah.run/release-name=$E2E_HELM_RELEASE" --overwrite >/dev/null
	kube -n "$E2E_OPERATOR_NAMESPACE" delete pod \
		-l "app.kubernetes.io/instance=$E2E_HELM_RELEASE" --wait=false >/dev/null
	wait_runtime_ready

	printf '%s\n' 'e2e crd: proving runtime rejection of drifted admission behavior'
	webhook_service=$(kube get validatingwebhookconfiguration ptah-operator-admission \
		-o jsonpath='{.webhooks[0].clientConfig.service.name}')
	[ -n "$webhook_service" ] || fail "admission webhook Service name is empty"
	first_validating_name=$(kube get validatingwebhookconfiguration ptah-operator-admission \
		-o jsonpath='{.webhooks[0].name}')
	[ "$first_validating_name" = vapproval.operator.ptah.run ] ||
		fail "approval validating webhook is not in its rendered position"
	stop_runtime_deployments
	kube patch mutatingwebhookconfiguration ptah-operator-admission --type=json \
		-p='[{"op":"replace","path":"/webhooks/0/failurePolicy","value":"Ignore"}]' >/dev/null
	kube patch validatingwebhookconfiguration ptah-operator-admission --type=json \
		-p='[{"op":"replace","path":"/webhooks/0/clientConfig/service/name","value":"foreign-service"}]' >/dev/null
	start_runtime_deployments
	assert_runtime_blocked "drifted admission behavior"
	kube patch mutatingwebhookconfiguration ptah-operator-admission --type=json \
		-p='[{"op":"replace","path":"/webhooks/0/failurePolicy","value":"Fail"}]' >/dev/null
	kube patch validatingwebhookconfiguration ptah-operator-admission --type=json \
		-p="[{\"op\":\"replace\",\"path\":\"/webhooks/0/clientConfig/service/name\",\"value\":\"$webhook_service\"}]" >/dev/null
	kube -n "$E2E_OPERATOR_NAMESPACE" delete pod \
		-l "app.kubernetes.io/instance=$E2E_HELM_RELEASE" --wait=false >/dev/null
	wait_runtime_ready
}

prove_controller_downgrade_guard() {
	printf '%s\n' 'e2e crd: proving controller downgrade preflight'
	stop_controller_deployment
	stored_version=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" \
		-o jsonpath='{.status.executionBinding.controllerStateVersion}')
	[ "$stored_version" = "$CONTROLLER_STATE_VERSION" ] ||
		fail "proof PtahSchema controller state version is $stored_version, expected $CONTROLLER_STATE_VERSION"
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" --subresource=status \
		--type=json -p="[{\"op\":\"replace\",\"path\":\"/status/executionBinding/controllerStateVersion\",\"value\":$NEWER_CONTROLLER_STATE_VERSION}]" >/dev/null
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o json |
		jq -S '.status' >"$WORK_DIR/future-controller-state.json"
	start_controller_deployment
	assert_controller_downgrade_blocked
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o json |
		jq -S '.status' >"$WORK_DIR/future-controller-state-after.json"
	cmp "$WORK_DIR/future-controller-state.json" "$WORK_DIR/future-controller-state-after.json" ||
		fail "blocked candidate manager rewrote future PtahSchema state"
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" --subresource=status \
		--type=json -p="[{\"op\":\"replace\",\"path\":\"/status/executionBinding/controllerStateVersion\",\"value\":$CONTROLLER_STATE_VERSION}]" >/dev/null
	kube -n "$E2E_OPERATOR_NAMESPACE" delete pod \
		-l 'app.kubernetes.io/component=controller' --wait=false >/dev/null
	wait_runtime_ready
}

create_proof_objects() {
	kube get namespace "$PROOF_NAMESPACE" >/dev/null
	kube -n "$PROOF_NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: $PROOF_SCHEMA
spec:
  suspend: true
  target:
    engine: PostgreSQL
    coordinationKey: crd-upgrade-proof
    urlFrom:
      name: unused-database-url
      key: url
  desired:
    ociRef: oci://example.invalid/schema:v1
    verificationPolicyFrom:
      name: unused-verification-policy
      key: policy.yaml
EOF
	wait_for_suspended

	# Only the controller reconciles PtahSchemas, and only its rollout is what
	# the drifted-CRD proof watches, so only the controller is stopped. Losing
	# both is its own recovery path, proven separately by
	# prove_runtime_deployment_recovery.
	stop_controller_deployment

	kube delete mutatingwebhookconfiguration ptah-operator-admission >/dev/null
	kube delete validatingwebhookconfiguration ptah-operator-admission >/dev/null
	schema_uid=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o jsonpath='{.metadata.uid}')

	kube -n "$PROOF_NAMESPACE" create -f - >/dev/null <<EOF
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchemaPlan
metadata:
  name: $PROOF_PLAN
spec:
  contractVersion: 3
  schemaRef: {name: $PROOF_SCHEMA, uid: $schema_uid}
  fingerprint: plan-fingerprint
  contentDigest: sha256:content
  size: 1
  artifactDigest: sha256:artifact
  coordinationDigest: sha256:coordination
  targetIdentityDigest: sha256:target
  actualStateFingerprint: actual
  desiredStateFingerprint: desired
  policyFingerprint: policy
  verificationPolicyUID: verification-policy-uid
  verificationPolicyDigest: sha256:verification-policy
  executionBindingID: v1-00000000000000000000000000000000
  controllerImage: $PROOF_CONTROLLER_IMAGE
  controllerRevision: e2e-crd-upgrade
  controllerStateVersion: 1
  ptahVersion: e2e
  executorImage: e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000
  runnerImage: e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111
  runnerProtocolVersion: 1
  dialect: postgresql
  destructive: false
  statementCount: 1
  chunks:
    - {name: proof-chunk, key: plan.sql, index: 0, digest: sha256:chunk, size: 1}
EOF
	plan_uid=$(kube -n "$PROOF_NAMESPACE" get ptahschemaplan "$PROOF_PLAN" -o jsonpath='{.metadata.uid}')
	kube -n "$PROOF_NAMESPACE" patch ptahschemaplan "$PROOF_PLAN" --subresource=status \
		--type=merge -p '{"status":{"observedGeneration":1,"conditions":[{"type":"Ready","status":"True","reason":"UpgradeProof","message":"proof status","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}' >/dev/null

	kube -n "$PROOF_NAMESPACE" create -f - >/dev/null <<EOF
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchemaApproval
metadata:
  name: $PROOF_APPROVAL
spec:
  schemaRef: {name: $PROOF_SCHEMA, uid: $schema_uid}
  planRef: {name: $PROOF_PLAN, uid: $plan_uid}
  planFingerprint: plan-fingerprint
  approver: {username: crd-upgrade-proof}
  approvedAt: "2026-01-01T00:00:00Z"
  mutationRequestUID: crd-upgrade-proof
EOF
	kube -n "$PROOF_NAMESPACE" patch ptahschemaapproval "$PROOF_APPROVAL" --subresource=status \
		--type=merge -p '{"status":{"observedGeneration":1,"conditions":[{"type":"Accepted","status":"True","reason":"UpgradeProof","message":"proof status","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}' >/dev/null
}

run_upgrade_proof() {
	E2E_CANDIDATE_VALUES_FILE=${E2E_CANDIDATE_VALUES_FILE:?E2E_CANDIDATE_VALUES_FILE is required for the upgrade proof}
	E2E_CANDIDATE_IMAGE=${E2E_CANDIDATE_IMAGE:?E2E_CANDIDATE_IMAGE is required for the upgrade proof}
	[ -f "$E2E_CANDIDATE_VALUES_FILE" ] || fail "candidate values file is missing"
	prepare_expected_hook_names "$E2E_CHART_PACKAGE" "$E2E_CANDIDATE_VALUES_FILE"
	runtime_deployment_names
	installed_deployment_image=$(kube -n "$E2E_OPERATOR_NAMESPACE" get deployment "$CONTROLLER_DEPLOYMENT" \
		-o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}')
	[ "$installed_deployment_image" = "$E2E_CANDIDATE_IMAGE" ] ||
		fail "installed current-release manager image is $installed_deployment_image, expected $E2E_CANDIDATE_IMAGE"

	printf '%s\n' 'e2e crd: proving read-only Job cleanup within the current release'
	kube create namespace "$PROOF_NAMESPACE" >/dev/null
	READ_ONLY_JOB_SCHEMA=$CURRENT_READ_ONLY_JOB_SCHEMA
	dispatch_read_only_job_fixture
	stop_runtime_deployments
	set_pod_webhook_failure_policy Fail Ignore
	stage_read_only_job_completion
	set_pod_webhook_failure_policy Ignore Fail
	start_runtime_deployments
	wait_runtime_ready
	wait_for_read_only_job_cleanup
	quiesce_read_only_job_schema

	helm_e2e get values "$E2E_HELM_RELEASE" -n "$E2E_OPERATOR_NAMESPACE" -o yaml >"$WORK_DIR/release-values.yaml"
	UPGRADE_VALUES_FILE=$WORK_DIR/release-values.yaml
	helm_e2e get values "$E2E_HELM_RELEASE" -n "$E2E_OPERATOR_NAMESPACE" \
		-o json >"$WORK_DIR/release-values.json"
	PROOF_CONTROLLER_IMAGE=$(production_controller_image_from_values \
		"$WORK_DIR/release-values.json")

	printf '%s\n' 'e2e crd: proving a missing CRD aborts Helm upgrade without recreation'
	kube delete crd ptahschemaapprovals.operator.ptah.run >/dev/null
	expect_upgrade_failure_without_deployment_change "upgrade with a missing CRD"
	if kube get crd ptahschemaapprovals.operator.ptah.run >/dev/null 2>&1; then
		fail "CRD hook recreated a missing CRD"
	fi
	kube create -f "$ROOT_DIR/config/crd/bases/operator.ptah.run_ptahschemaapprovals.yaml" >/dev/null
	kube wait --for=condition=Established crd/ptahschemaapprovals.operator.ptah.run --timeout=60s >/dev/null

	printf '%s\n' 'e2e crd: proving a newer CRD schema version blocks rollback'
	future_schema_version=$((CANDIDATE_CRD_SCHEMA_VERSION + 1))
	kube annotate crd ptahschemaplans.operator.ptah.run \
		"operator.ptah.run/crd-schema-version=$future_schema_version" --overwrite >/dev/null
	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		crd_evidence "$crd_name" "$WORK_DIR/${crd_name}-before-schema-rollback.json"
	done
	expect_upgrade_failure_without_deployment_change "upgrade with a newer CRD schema version"
	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-schema-rollback.json"
	done
	kube annotate crd ptahschemaplans.operator.ptah.run \
		"operator.ptah.run/crd-schema-version=$CANDIDATE_CRD_SCHEMA_VERSION" --overwrite >/dev/null

	printf '%s\n' 'e2e crd: proving a newer durable controller-state marker blocks rollback'
	kube annotate crd ptahschemaplans.operator.ptah.run \
		"operator.ptah.run/controller-state-version=$NEWER_CONTROLLER_STATE_VERSION" --overwrite >/dev/null
	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		crd_evidence "$crd_name" "$WORK_DIR/${crd_name}-before-state-rollback.json"
	done
	expect_upgrade_failure_without_deployment_change "upgrade with a newer durable controller-state marker"
	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-state-rollback.json"
	done
	kube annotate crd ptahschemaplans.operator.ptah.run \
		"operator.ptah.run/controller-state-version=$CONTROLLER_STATE_VERSION" --overwrite >/dev/null

	printf '%s\n' 'e2e crd: proving an incomplete schema identity and a digest collision are refused'
	digest_crd=ptahschemaplans.operator.ptah.run
	candidate_schema_digest=$(kube get crd "$digest_crd" \
		-o jsonpath='{.metadata.annotations.operator\.ptah\.run/crd-schema-digest}')
	printf '%s\n' "$candidate_schema_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
		fail "$digest_crd does not carry a valid candidate schema digest"
	# The live spec still matches the candidate exactly; only the digest is
	# missing. The operator upgrades only from a release that carries the
	# whole identity tuple, so an exact schema without it is refused too.
	kube annotate crd "$digest_crd" operator.ptah.run/crd-schema-digest- >/dev/null
	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		crd_evidence "$crd_name" "$WORK_DIR/${crd_name}-before-missing-digest.json"
	done
	# The refusal happens inside the reconcile hook's container, so Helm
	# reports only that the hook Job failed. What this proof pins is the
	# refusal itself, and the shared helper pins that the reconcile hook of the
	# expected revision is what refused while every CRD and Deployment stayed
	# unchanged.
	expect_upgrade_failure_without_deployment_change "upgrade with a missing schema digest"
	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-missing-digest.json"
	done
	kube annotate crd "$digest_crd" \
		"operator.ptah.run/crd-schema-digest=$candidate_schema_digest" --overwrite >/dev/null
	helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$WORK_DIR/release-values.yaml" \
		--wait --timeout 5m >/dev/null

	collision_digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
	if [ "$collision_digest" = "$candidate_schema_digest" ]; then
		collision_digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
	fi
	kube annotate crd "$digest_crd" \
		"operator.ptah.run/crd-schema-digest=$collision_digest" --overwrite >/dev/null
	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		crd_evidence "$crd_name" "$WORK_DIR/${crd_name}-before-digest-collision.json"
	done
	expect_upgrade_failure_without_deployment_change "upgrade with a same-version schema digest collision"
	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-digest-collision.json"
	done
	kube annotate crd "$digest_crd" \
		"operator.ptah.run/crd-schema-digest=$candidate_schema_digest" --overwrite >/dev/null

	printf '%s\n' 'e2e crd: creating live-object preservation evidence'
	create_proof_objects
	object_evidence ptahschema "$PROOF_SCHEMA" "$WORK_DIR/ptahschema-before.json"
	object_evidence ptahschemaplan "$PROOF_PLAN" "$WORK_DIR/ptahschemaplan-before.json"
	object_evidence ptahschemaapproval "$PROOF_APPROVAL" "$WORK_DIR/ptahschemaapproval-before.json"

	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		kube patch crd "$crd_name" --type=json \
			-p='[{"op":"add","path":"/spec/versions/0/schema/openAPIV3Schema/description","value":"outdated e2e schema"}]' >/dev/null
		crd_evidence "$crd_name" "$WORK_DIR/${crd_name}-before-future-state.json"
	done
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" --subresource=status \
		--type=json -p="[{\"op\":\"replace\",\"path\":\"/status/executionBinding/controllerStateVersion\",\"value\":$NEWER_CONTROLLER_STATE_VERSION}]" >/dev/null
	expect_upgrade_failure_without_deployment_change "upgrade against future controller state"
	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-future-state.json"
	done
	stored_version=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" \
		-o jsonpath='{.status.executionBinding.controllerStateVersion}')
	[ "$stored_version" = "$NEWER_CONTROLLER_STATE_VERSION" ] ||
		fail "failed CRD preflight rewrote future controller state"
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" --subresource=status \
		--type=json -p="[{\"op\":\"replace\",\"path\":\"/status/executionBinding/controllerStateVersion\",\"value\":$CONTROLLER_STATE_VERSION}]" >/dev/null

	printf '%s\n' 'e2e crd: upgrading drifted CRDs before the manager rollout'
	helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$WORK_DIR/release-values.yaml" \
		--wait --timeout 5m >/dev/null
	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		description=$(kube get crd "$crd_name" -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.description}')
		[ "$description" != "outdated e2e schema" ] || fail "$crd_name retained the outdated schema"
	done
	assert_object_unchanged ptahschema "$PROOF_SCHEMA" "$WORK_DIR/ptahschema-before.json"
	assert_object_unchanged ptahschemaplan "$PROOF_PLAN" "$WORK_DIR/ptahschemaplan-before.json"
	assert_object_unchanged ptahschemaapproval "$PROOF_APPROVAL" "$WORK_DIR/ptahschemaapproval-before.json"
	prove_controller_write_guard
	prove_runtime_deployment_recovery

	printf '%s\n' 'e2e crd: proving immutable singleton coordination'
	second_release=${E2E_HELM_RELEASE}-second
	if helm_e2e install "$second_release" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$WORK_DIR/release-values.yaml" \
		--wait --timeout 2m >"$WORK_DIR/second-release.out" 2>"$WORK_DIR/second-release.err"; then
		fail "a second operator release was installed"
	fi
	# The chart refuses a second release in a namespace another release owns,
	# and the controller Deployment's provenance is the first thing it reads,
	# so that refusal is what a second install sees. It names the release that
	# owns the object, which is what makes the refusal a coordination proof
	# rather than any rendering error.
	if ! grep -F 'is not owned by Helm release' "$WORK_DIR/second-release.err" >/dev/null ||
		! grep -F "$E2E_HELM_RELEASE" "$WORK_DIR/second-release.err" >/dev/null; then
		fail "second release failed without the ownership refusal naming the installed release"
	fi
	if helm_e2e status "$second_release" -n "$E2E_OPERATOR_NAMESPACE" >/dev/null 2>&1; then
		fail "failed second release was recorded"
	fi

		expect_upgrade_render_failure_without_deployment_change \
			"coordination namespace mutation" --set-string coordination.namespace=forbidden-coordination
	grep -F 'operator.ptah.run/coordination-namespace' "$WORK_DIR/failed-upgrade.err" >/dev/null ||
		fail "coordination mutation failed without the immutable annotation guard"
		expect_upgrade_render_failure_without_deployment_change \
		"leader-election mutation" --set replicaCount=1 --set leaderElection=false
	grep -F 'operator.ptah.run/leader-election' "$WORK_DIR/failed-upgrade.err" >/dev/null ||
		fail "leader-election mutation failed without the immutable annotation guard"
	prove_shared_release_namespace_refusal

	prove_runtime_singleton_guard
	prove_controller_downgrade_guard
	helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$WORK_DIR/release-values.yaml" \
		--wait --timeout 5m >/dev/null
	printf '%s\n' 'e2e crd: upgrade and singleton proofs passed'
}

run_next_release_upgrade_proof() {
	# Managed lifecycle coverage advances an already active release: the
	# current release is the predecessor, and the synthetic next release is
	# the candidate that replaces it.
	if [ ! -f "$E2E_NEXT_CHART_PACKAGE" ] || [ -L "$E2E_NEXT_CHART_PACKAGE" ]; then
		fail "E2E_NEXT_CHART_PACKAGE must name a regular synthetic next-release chart package"
	fi
	if [ ! -f "$E2E_NEXT_VALUES_FILE" ] || [ -L "$E2E_NEXT_VALUES_FILE" ]; then
		fail "E2E_NEXT_VALUES_FILE must name a regular synthetic next-release values file"
	fi
	printf '%s\n' "$E2E_NEXT_CONTROLLER_IMAGE" |
		grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' ||
		fail "E2E_NEXT_CONTROLLER_IMAGE must be an exact repository-and-digest identity"
	next_values_controller_image=$(production_controller_image_from_values \
		"$E2E_NEXT_VALUES_FILE")
	[ "$next_values_controller_image" = "$E2E_NEXT_CONTROLLER_IMAGE" ] ||
		fail "synthetic next-release values do not bind the supplied controller image"
	helm_e2e get values "$E2E_HELM_RELEASE" -n "$E2E_OPERATOR_NAMESPACE" \
		-o json >"$WORK_DIR/current-release-values.json"
	CURRENT_RELEASE_CONTROLLER_IMAGE=$(production_controller_image_from_values \
		"$WORK_DIR/current-release-values.json")
	[ "$CURRENT_RELEASE_CONTROLLER_IMAGE" != "$E2E_NEXT_CONTROLLER_IMAGE" ] ||
		fail "current and synthetic next-release controller images must be distinct"

	current_release_identity=$WORK_DIR/current-release-controller-identity.json
	next_release_identity=$WORK_DIR/next-release-controller-identity.json
	capture_controller_service_account_identity \
		"$CURRENT_RELEASE_CONTROLLER_IMAGE" \
		"$current_release_identity"
	current_service_account=$(jq -er '.serviceAccountName' "$current_release_identity")
	current_release_revision=$(helm_e2e status "$E2E_HELM_RELEASE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" -o json |
		jq -er 'select(.info.status == "deployed") | .version | select(type == "number" and . >= 1)')

	prepare_expected_hook_names "$E2E_NEXT_CHART_PACKAGE" "$E2E_NEXT_VALUES_FILE"
	# The late failure leaves the runtime stopped; stage the handoff while no
	# runtime can consume or clean up the Job. Do not delete or resurrect
	# Deployments across that recovery boundary.
	printf '%s\n' 'e2e crd: dispatching a read-only Job the successor must retire'
	READ_ONLY_JOB_SCHEMA=$SUCCESSOR_READ_ONLY_JOB_SCHEMA
	# A running Apply is the work an upgrade may not interrupt. The barrier holds
	# the statement inside the engine, so the Apply is genuinely running across
	# the upgrade rather than finished before it (stokaro/ptah-operator#7).
	printf '%s\n' 'e2e crd: holding one Apply open across the next-release upgrade'
	dispatch_read_only_job_fixture
	start_running_apply_barrier
	prepare_running_apply_fixture
	start_running_apply_fixture
	stage_predecessor_apply_job_uid_gap_while_running
	prove_late_failure_recovery "$CURRENT_RELEASE_CONTROLLER_IMAGE"
	set_pod_webhook_failure_policy Fail Ignore
	stage_read_only_job_completion
	set_pod_webhook_failure_policy Ignore Fail
	stage_read_only_job_uid_gap
	assert_late_failure_candidate_unchanged
	delete_late_failure_blocker
	before_retry_revision=$(helm_e2e status "$E2E_HELM_RELEASE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" -o json |
		jq -er '.version | select(type == "number" and . >= 1)')
	[ "$before_retry_revision" -eq "$late_revision" ] ||
		fail "late-failure recovery did not resume the exact failed Helm revision"
	printf '%s\n' 'e2e crd: retrying the upgrade to the same synthetic next release'
	retry_same_candidate
	wait_runtime_ready
	wait_for_read_only_job_cleanup
	quiesce_read_only_job_schema
	assert_predecessor_apply_remains_exclusive_while_running
	release_running_apply_barrier
	wait_for_predecessor_apply_job_terminal
	wait_for_predecessor_apply_job_cleanup
	after_revision=$(helm_e2e status "$E2E_HELM_RELEASE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" -o json |
		jq -er 'select(.info.status == "deployed") | .version | select(type == "number" and . >= 1)')
	[ "$after_revision" -eq $((late_revision + 1)) ] ||
		fail "same-candidate recovery did not create exactly one retry Helm revision"

	capture_controller_service_account_identity \
		"$E2E_NEXT_CONTROLLER_IMAGE" \
		"$next_release_identity"
	current_deployment=$(jq -er '.deploymentName' "$current_release_identity")
	next_deployment=$(jq -er '.deploymentName' "$next_release_identity")
	[ "$next_deployment" = "$current_deployment" ] ||
		fail "synthetic next-release upgrade changed the controller Deployment identity"
	# Every release runs the controller as the same ServiceAccount, and Helm
	# keeps that object across the upgrade: the name and the UID both hold.
	current_service_account_uid=$(jq -er '.serviceAccountUID' "$current_release_identity")
	next_service_account=$(jq -er '.serviceAccountName' "$next_release_identity")
	next_service_account_uid=$(jq -er '.serviceAccountUID' "$next_release_identity")
	[ "$next_service_account" = "$current_service_account" ] ||
		fail "synthetic next-release upgrade moved the controller from ServiceAccount $current_service_account to $next_service_account"
	[ "$next_service_account_uid" = "$current_service_account_uid" ] ||
		fail "synthetic next-release upgrade replaced controller ServiceAccount $current_service_account (UID $current_service_account_uid became $next_service_account_uid)"
	# The synthetic next release differs from this one in its manager image
	# alone: the executor, the Ptah version, the runner protocol and the
	# controller-state version are the same. None of that is in the execution
	# binding, so the schema keeps its epoch and the plan and approval under it
	# stay exactly as they were -- a patch release retires nothing.
	for resource in ptahschema ptahschemaplan ptahschemaapproval; do
		assert_object_unchanged "$resource" "$PROOF_SCHEMA" \
			"$WORK_DIR/${resource}-before.json"
	done
	printf '%s\n' 'e2e crd: same-candidate late-failure recovery passed'

	# A rollback to the release this one replaced runs that release's CRD hook
	# first. Refused, it leaves the running release alone and the rollback
	# revision pending; the rollback that follows is both the way out of that
	# and the proof that a rollback the stored state allows goes through.
	prove_rollback_refused_over_future_state "$current_release_revision"
	prove_rollback "$current_release_revision" "$CURRENT_RELEASE_CONTROLLER_IMAGE"
	for resource in ptahschema ptahschemaplan ptahschemaapproval; do
		assert_object_unchanged "$resource" "$PROOF_SCHEMA" \
			"$WORK_DIR/${resource}-before.json"
	done
	# Everything after this, including the uninstall, is held to the state the
	# rollback leaves behind.
	for resource in ptahschema ptahschemaplan ptahschemaapproval; do
		object_evidence "$resource" "$PROOF_SCHEMA" "$WORK_DIR/${resource}-before.json"
	done
	printf '%s\n' 'e2e crd: synthetic next-release upgrade kept the controller identity, and the rollback to the current release went through its hook'
}

run_uninstall_proof() {
	if [ ! -f "$E2E_CHART_PACKAGE" ] || [ -L "$E2E_CHART_PACKAGE" ]; then
		fail "E2E_CHART_PACKAGE must name the regular non-symlink current-release chart package"
	fi
	if [ ! -f "$E2E_CANDIDATE_VALUES_FILE" ] || [ -L "$E2E_CANDIDATE_VALUES_FILE" ]; then
		fail "E2E_CANDIDATE_VALUES_FILE must name the regular non-symlink current-release values file"
	fi
	printf '%s\n' "$E2E_CANDIDATE_IMAGE" |
		grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' ||
		fail "E2E_CANDIDATE_IMAGE must be an exact repository-and-digest identity"
	candidate_values_controller_image=$(production_controller_image_from_values \
		"$E2E_CANDIDATE_VALUES_FILE")
	[ "$candidate_values_controller_image" = "$E2E_CANDIDATE_IMAGE" ] ||
		fail "current-release values do not bind the supplied candidate controller image"
	for resource in ptahschema ptahschemaplan ptahschemaapproval; do
		object_evidence "$resource" "$PROOF_SCHEMA" "$WORK_DIR/${resource}-before.json"
	done
	run_next_release_upgrade_proof

	capture_certificate_secret_names
	helm_e2e uninstall "$E2E_HELM_RELEASE" -n "$E2E_OPERATOR_NAMESPACE" \
		--wait --timeout 5m >/dev/null ||
		fail "the uninstall of the rolled-back release failed; Helm's own error is above"
	assert_release_runtime_removed
	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		kube get crd "$crd_name" >/dev/null
	done
	for resource in ptahschema ptahschemaplan ptahschemaapproval; do
		assert_object_unchanged "$resource" "$PROOF_SCHEMA" "$WORK_DIR/${resource}-before.json"
	done

	printf '%s\n' 'e2e crd: reinstalling over retained and drifted CRDs'
	kube patch crd ptahschemas.operator.ptah.run --type=json \
		-p='[{"op":"add","path":"/spec/versions/0/schema/openAPIV3Schema/description","value":"retained reinstall drift"}]' >/dev/null
	# The drift above is written by kubectl, which owns the field it added.
	# Helm 4 applies the chart's CRDs server-side on install and refuses to
	# change a field another manager owns, so an install over a retained CRD
	# somebody edited needs the force. It is confined to the CRDs this chart
	# ships.
	#
	# That apply also happens before any hook runs, so what this step proves is
	# that the install converges a retained CRD, not that the hook does. The
	# hook's own CRD convergence is proved on the upgrade above, where Helm
	# leaves the CRDs alone and the same drift needs no force.
	helm_e2e install "$E2E_HELM_RELEASE" "$E2E_NEXT_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$E2E_NEXT_VALUES_FILE" \
		--force-conflicts \
		--wait --timeout 5m >/dev/null
	description=$(kube get crd ptahschemas.operator.ptah.run \
		-o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.description}')
	[ "$description" != "retained reinstall drift" ] ||
		fail "the reinstall did not reconcile a retained CRD another manager drifted"
	for resource in ptahschema ptahschemaplan ptahschemaapproval; do
		assert_object_unchanged "$resource" "$PROOF_SCHEMA" "$WORK_DIR/${resource}-before.json"
	done
	capture_controller_service_account_identity \
		"$E2E_NEXT_CONTROLLER_IMAGE" \
		"$WORK_DIR/reinstalled-next-release-controller-identity.json"
	capture_certificate_secret_names
	helm_e2e uninstall "$E2E_HELM_RELEASE" -n "$E2E_OPERATOR_NAMESPACE" \
		--wait --timeout 5m >/dev/null ||
		fail "the uninstall of the release reinstalled over retained CRDs failed; Helm's own error is above"
	assert_release_runtime_removed
	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		kube get crd "$crd_name" >/dev/null
	done
	for resource in ptahschema ptahschemaplan ptahschemaapproval; do
		assert_object_unchanged "$resource" "$PROOF_SCHEMA" "$WORK_DIR/${resource}-before.json"
	done

	printf '%s\n' 'e2e crd: fresh-installing the exact exported current-release chart bytes'
	kube patch crd ptahschemas.operator.ptah.run --type=json \
		-p='[{"op":"add","path":"/spec/versions/0/schema/openAPIV3Schema/description","value":"exact released-chart install drift"}]' >/dev/null
	# The drift above is written by kubectl, which owns the field it added, and
	# Helm 4 refuses to change a field another manager owns. This install carries
	# the force for the same reason the one before it does, and proves the same
	# thing: that the install converges a retained CRD.
	helm_e2e install "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$E2E_CANDIDATE_VALUES_FILE" \
		--force-conflicts \
		--wait --timeout 5m >/dev/null
	wait_runtime_ready
	description=$(kube get crd ptahschemas.operator.ptah.run \
		-o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.description}')
	[ "$description" != "exact released-chart install drift" ] ||
		fail "the exact released-chart install did not reconcile a retained CRD another manager drifted"
	capture_controller_service_account_identity \
		"$E2E_CANDIDATE_IMAGE" \
		"$WORK_DIR/fresh-current-release-controller-identity.json"
	# This install puts a different manager in place and changes nothing the
	# execution binding holds, so the schema, its plan and its approval stay
	# exactly as they were, as they do across the next-release upgrade.
	# Everything after this, including this release's own
	# uninstall, is held to the state it leaves behind.
	for resource in ptahschema ptahschemaplan ptahschemaapproval; do
		object_evidence "$resource" "$PROOF_SCHEMA" "$WORK_DIR/${resource}-before.json"
	done
	capture_certificate_secret_names
	helm_e2e uninstall "$E2E_HELM_RELEASE" -n "$E2E_OPERATOR_NAMESPACE" \
		--wait --timeout 5m >/dev/null ||
		fail "the uninstall of the exported current-release chart failed; Helm's own error is above"
	assert_release_runtime_removed
	for crd_name in \
		ptahschemas.operator.ptah.run \
		ptahschemaplans.operator.ptah.run \
		ptahschemaapprovals.operator.ptah.run; do
		kube get crd "$crd_name" >/dev/null
	done
	for resource in ptahschema ptahschemaplan ptahschemaapproval; do
		assert_object_unchanged "$resource" "$PROOF_SCHEMA" "$WORK_DIR/${resource}-before.json"
	done
	printf '%s\n' 'e2e crd: exact exported current-release chart passed fresh install and zero-residue uninstall'
	printf '%s\n' 'e2e crd: uninstall retained CRDs and live objects'
}

case "$E2E_PHASE" in
	upgrade) run_upgrade_proof ;;
	uninstall) run_uninstall_proof ;;
	*) fail "unsupported E2E_PHASE $E2E_PHASE" ;;
esac

PHASE_COMPLETED=1
printf 'e2e crd: PASS phase=%s\n' "$E2E_PHASE"
