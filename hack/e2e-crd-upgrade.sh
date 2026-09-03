#!/bin/sh

set -eu

E2E_KUBECONFIG=${E2E_KUBECONFIG:?E2E_KUBECONFIG is required}
E2E_OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:?E2E_OPERATOR_NAMESPACE is required}
E2E_HELM_RELEASE=${E2E_HELM_RELEASE:?E2E_HELM_RELEASE is required}
E2E_CHART_PACKAGE=${E2E_CHART_PACKAGE:?E2E_CHART_PACKAGE is required}
E2E_KUBERNETES_VERSION=${E2E_KUBERNETES_VERSION:?E2E_KUBERNETES_VERSION is required}
E2E_PHASE=${E2E_PHASE:-upgrade}
E2E_DOCKER_CONTEXT=${E2E_DOCKER_CONTEXT:-}
E2E_EXTERNAL_POSTGRES_CONTAINER_ID=${E2E_EXTERNAL_POSTGRES_CONTAINER_ID:-}

ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-e2e-crd.XXXXXX")
PROOF_NAMESPACE=${E2E_OPERATOR_NAMESPACE}-crd-proof
PROOF_SCHEMA=crd-upgrade-proof
PROOF_PLAN=crd-upgrade-proof
PROOF_APPROVAL=crd-upgrade-proof
PROOF_CONTROLLER_IMAGE=
UPGRADE_VALUES_FILE=
EXPECTED_SINGLETON_ANNOTATIONS_FILE=$WORK_DIR/expected-singleton-annotations.json
EXPECTED_SINGLETON_RENDER_FILE=$WORK_DIR/expected-singleton-render.yaml
PREDECESSOR_SCHEMA=predecessor-live
PREDECESSOR_PLAN=predecessor-live
PREDECESSOR_APPROVAL=predecessor-live
PREDECESSOR_DELETING_SCHEMA=predecessor-deleting
PREDECESSOR_JOB_SCHEMA=predecessor-read-only-job
PREDECESSOR_JOB_NAME=
PREDECESSOR_JOB_UID=
PREDECESSOR_APPLY_SCHEMA=predecessor-running-apply
PREDECESSOR_APPLY_POLICY=predecessor-apply-policy
PREDECESSOR_APPLY_DATABASE=predecessor-apply-database
PREDECESSOR_APPLY_PULL_SECRET=predecessor-apply-pull
PREDECESSOR_APPLY_PLAN_NAME=
PREDECESSOR_APPLY_PLAN_UID=
PREDECESSOR_APPLY_JOB_NAME=
PREDECESSOR_APPLY_JOB_UID=
PREDECESSOR_APPLY_POD_NAME=
PREDECESSOR_APPLY_POD_UID=
PREDECESSOR_APPLY_BARRIER_PID=
PREDECESSOR_APPLY_BARRIER_ACTIVE=0
BLOCKED_STABILITY_SECONDS=10
BLOCKED_FAILURE_TIMEOUT_SECONDS=150
FOREIGN_TEARDOWN_BINDING=
CONTROLLER_IMPERSONATION_USERNAME=
CONTROLLER_IMPERSONATION_UID=
CONTROLLER_IMPERSONATION_POD_NAME=
CONTROLLER_IMPERSONATION_POD_UID=
CONTROLLER_GUARD_OWNER=
CONTROLLER_GUARD_PROBE_INDEX=0
CONTROLLER_OBJECT_GUARD_PROBE_INDEX=0
KUBERNETES_MAJOR_MINOR=
CANDIDATE_CRD_SCHEMA_VERSION=$(awk '
  $1 == "operator.ptah.dev/crd-schema-version:" {
    gsub(/"/, "", $2)
    print $2
    exit
  }
' "$ROOT_DIR/config/crd/bases/operator.ptah.dev_ptahschemas.yaml")

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	if [ "$PREDECESSOR_APPLY_BARRIER_ACTIVE" -eq 1 ]; then
		if ! docker --context "$E2E_DOCKER_CONTEXT" exec "$E2E_EXTERNAL_POSTGRES_CONTAINER_ID" \
			sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD"; export PGPASSWORD; exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atqc "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = '\''ptah-operator-predecessor-apply-barrier'\'' AND pid <> pg_backend_pid()"' \
			>/dev/null 2>&1; then
			status=1
		fi
		PREDECESSOR_APPLY_BARRIER_ACTIVE=0
	fi
	if [ -n "$PREDECESSOR_APPLY_BARRIER_PID" ]; then
		kill "$PREDECESSOR_APPLY_BARRIER_PID" >/dev/null 2>&1 || true
		wait "$PREDECESSOR_APPLY_BARRIER_PID" >/dev/null 2>&1 || true
		PREDECESSOR_APPLY_BARRIER_PID=
	fi
	if [ -n "$FOREIGN_TEARDOWN_BINDING" ]; then
		if ! kube delete clusterrolebinding "$FOREIGN_TEARDOWN_BINDING" \
			--ignore-not-found=true >/dev/null 2>&1; then
			status=1
		fi
	fi
	if [ -n "$CONTROLLER_GUARD_OWNER" ]; then
		if ! kube -n "$PROOF_NAMESPACE" delete configmap "$CONTROLLER_GUARD_OWNER" \
			--ignore-not-found=true >/dev/null 2>&1; then
			status=1
		fi
	fi
	case "$WORK_DIR" in
		"${TMPDIR:-/tmp}"/ptah-operator-e2e-crd.*) rm -rf -- "$WORK_DIR" ;;
		*)
			printf 'e2e crd: refusing to remove unexpected work directory %s\n' "$WORK_DIR" >&2
			status=1
		;;
	esac
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

fail() {
	printf 'e2e crd: %s\n' "$*" >&2
	exit 1
}

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
	case "$KUBERNETES_MAJOR_MINOR" in
	1.35 | 1.36 | 1.37) ;;
	*) fail "Kubernetes $KUBERNETES_MAJOR_MINOR is outside the supported 1.35-1.37 window" ;;
	esac
	server_version=$(kube version -o json | jq -er '.serverVersion.gitVersion')
	case "$server_version" in
	v"$E2E_KUBERNETES_VERSION" | v"$E2E_KUBERNETES_VERSION"-*) ;;
	*)
		fail "cluster reports $server_version, expected v$E2E_KUBERNETES_VERSION for the guarded-field proof"
		;;
	esac
}

verify_supported_server_version

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

schema_identity_evidence() {
	name=$1
	destination=$2
	kube -n "$PROOF_NAMESPACE" get ptahschema "$name" -o json |
		jq -S '{uid: .metadata.uid, spec: .spec}' >"$destination"
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

crd_normalized_digest() {
	name=$1
	destination=$WORK_DIR/${name}-digest-input.json
	kube get crd "$name" -o json >"$destination"
	go -C "$ROOT_DIR" run ./hack/crdschemadigest "$destination"
}

restore_predecessor_crd() {
	name=$1
	path=$(jq -er --arg name "$name" '.crds[] | select(.name == $name) | .path' \
		"$E2E_PREDECESSOR_IDENTITY_FILE")
	desired=$WORK_DIR/${name}-predecessor.json
	live=$WORK_DIR/${name}-live.json
	kube create --dry-run=client -f "$E2E_PREDECESSOR_SOURCE_DIR/$path" -o json >"$desired"
	kube get crd "$name" -o json >"$live"
	jq --slurpfile desired "$desired" '.spec = $desired[0].spec | del(.status, .metadata.managedFields)' \
		"$live" | kube replace -f - >/dev/null
	want=$(jq -er --arg name "$name" '.crds[] | select(.name == $name) | .normalizedSpecDigest' \
		"$E2E_PREDECESSOR_IDENTITY_FILE")
	got=$(crd_normalized_digest "$name")
	[ "$got" = "$want" ] ||
		fail "restored predecessor CRD $name digest is $got, expected $want"
}

singleton_contract_evidence() {
	resource=$1
	destination=$2
	kube get "$resource" ptah-operator-admission -o json |
		jq -S '{uid: .metadata.uid, labels: (.metadata.labels // {}), annotations: (.metadata.annotations // {}), webhooks: .webhooks}' \
		>"$destination"
}

owned_singleton_annotation_count() {
	resource=$1
	[ -s "$EXPECTED_SINGLETON_ANNOTATIONS_FILE" ] ||
		fail "expected admission singleton annotations are missing"
	kube get "$resource" ptah-operator-admission -o json | jq \
		--slurpfile expected "$EXPECTED_SINGLETON_ANNOTATIONS_FILE" '[
      .metadata.annotations // {} | keys[] as $key |
      select($expected[0] | has($key))
    ] | length'
}

prepare_expected_singleton_annotations() {
	helm_e2e template "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$E2E_CANDIDATE_VALUES_FILE" \
		--show-only templates/webhook.yaml >"$EXPECTED_SINGLETON_RENDER_FILE"
	awk '
      $1 == "kind:" && $2 == "MutatingWebhookConfiguration" { mutating = 1; next }
      mutating && $1 == "annotations:" { annotations = 1; next }
      annotations && $1 == "labels:" { exit }
      annotations { sub(/^    /, ""); print }
    ' "$EXPECTED_SINGLETON_RENDER_FILE" | jq -Rn '
	  [inputs | capture("^(?<key>[^:]+): \"(?<value>[^\"]*)\"$")] |
      from_entries
    ' >"$EXPECTED_SINGLETON_ANNOTATIONS_FILE"
	jq -e '
      length == 13 and
      (keys == [
        "operator.ptah.dev/admission-contract-version",
        "operator.ptah.dev/certificate-deployment-name",
        "operator.ptah.dev/controller-deployment-name",
        "operator.ptah.dev/controller-service-account-name",
        "operator.ptah.dev/controller-state-version",
        "operator.ptah.dev/coordination-namespace",
        "operator.ptah.dev/hook-service-account-name",
        "operator.ptah.dev/leader-election",
        "operator.ptah.dev/leader-election-id",
        "operator.ptah.dev/release-name",
        "operator.ptah.dev/release-namespace",
        "operator.ptah.dev/release-sequence",
        "operator.ptah.dev/webhook-service-name"
      ])
    ' "$EXPECTED_SINGLETON_ANNOTATIONS_FILE" >/dev/null ||
		fail "candidate render does not contain the complete 13-field admission singleton tuple"
}

assert_singleton_annotation_free() {
	for singleton_resource in mutatingwebhookconfiguration validatingwebhookconfiguration; do
		count=$(owned_singleton_annotation_count "$singleton_resource")
		[ "$count" -eq 0 ] ||
			fail "$singleton_resource/ptah-operator-admission has $count candidate ownership annotations before adoption"
	done
}

assert_adopted_singleton_annotations() {
	for singleton_resource in mutatingwebhookconfiguration validatingwebhookconfiguration; do
		kube get "$singleton_resource" ptah-operator-admission -o json | jq -e \
			--slurpfile expected "$EXPECTED_SINGLETON_ANNOTATIONS_FILE" '
          .metadata.annotations as $actual |
          ($expected[0] | to_entries | all(. as $entry; $actual[$entry.key] == $entry.value))
        ' >/dev/null ||
			fail "$singleton_resource/ptah-operator-admission did not acquire the complete exact candidate annotation tuple"
	done
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
	before_revision=$(helm_e2e status "$E2E_HELM_RELEASE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" -o json | jq -er '.version | select(type == "number" and . >= 1)')
	failed_revision=$((before_revision + 1))
	hook_service_account=$(jq -er '."operator.ptah.dev/hook-service-account-name"' \
		"$EXPECTED_SINGLETON_ANNOTATIONS_FILE")
	expected_hook_name=$(printf '%s' "$hook_service_account" | cut -c1-53 | sed 's/-$//')-preflight
	deployment_evidence >"$before"
	if helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$UPGRADE_VALUES_FILE" \
		--wait --timeout 2m "$@" >"$WORK_DIR/failed-upgrade.out" 2>"$WORK_DIR/failed-upgrade.err"; then
		fail "$description unexpectedly succeeded"
	fi
	if ! helm_e2e status "$E2E_HELM_RELEASE" --namespace "$E2E_OPERATOR_NAMESPACE" \
		--revision "$failed_revision" -o json >"$status_file"; then
		fail "$description did not retain structured Helm evidence for failed revision $failed_revision"
	fi
	if ! jq -e \
		--argjson expected_revision "$failed_revision" \
		--arg expected_name "$expected_hook_name" \
		--argjson expected_weight -60 \
		-f "$ROOT_DIR/hack/failed-hook-evidence.jq" "$status_file" >/dev/null; then
		jq -c '{version, status: .info.status, description: .info.description, hooks: [(.hooks // [])[] | {name, kind, weight, events, last_run}]}' \
			"$status_file" >&2 || true
		fail "$description lacks exact revision-bound failed preflight evidence"
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

wait_for_schema_deleted() {
	schema_name=$1
	deadline=$(($(date +%s) + 120))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		if kube -n "$PROOF_NAMESPACE" get ptahschema "$schema_name" \
			>"$WORK_DIR/deleting-schema.out" 2>"$WORK_DIR/deleting-schema.err"; then
			sleep 1
			continue
		fi
		if grep -F '(NotFound)' "$WORK_DIR/deleting-schema.err" >/dev/null; then
			return
		fi
		fail "could not verify deletion of PtahSchema $schema_name"
	done
	fail "PtahSchema $schema_name was not deleted"
}

wait_for_predecessor_read_only_job() {
	deadline=$(($(date +%s) + 120))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		PREDECESSOR_JOB_NAME=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PREDECESSOR_JOB_SCHEMA" \
			-o jsonpath='{.status.activeOperation.jobName}' 2>/dev/null || true)
		PREDECESSOR_JOB_UID=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PREDECESSOR_JOB_SCHEMA" \
			-o jsonpath='{.status.activeOperation.jobUID}' 2>/dev/null || true)
		if [ -n "$PREDECESSOR_JOB_NAME" ] && [ -n "$PREDECESSOR_JOB_UID" ] &&
			kube -n "$PROOF_NAMESPACE" get job "$PREDECESSOR_JOB_NAME" -o json \
				>"$WORK_DIR/predecessor-read-only-job.json" 2>/dev/null; then
			jq -e \
				--arg schema "$PREDECESSOR_JOB_SCHEMA" \
				--arg uid "$PREDECESSOR_JOB_UID" '
              .metadata.uid == $uid and
              .metadata.labels["operator.ptah.dev/schema"] == $schema and
              .metadata.labels["operator.ptah.dev/operation"] == "resolve" and
              (.metadata.annotations | keys | sort) == [
                "operator.ptah.dev/admission-snapshot-digest",
                "operator.ptah.dev/execution-binding-id",
                "operator.ptah.dev/input-fingerprint",
                "operator.ptah.dev/operation-id",
                "operator.ptah.dev/ptah-version"
              ] and
              (.spec | has("ttlSecondsAfterFinished") | not)
            ' "$WORK_DIR/predecessor-read-only-job.json" >/dev/null ||
				fail "predecessor read-only Job does not match its exact five-annotation contract"
			return
		fi
		sleep 1
	done
	fail "predecessor controller did not dispatch a read-only Job with a committed UID"
}

stage_predecessor_read_only_job_completion() {
	[ -n "$PREDECESSOR_JOB_NAME" ] || fail "predecessor read-only Job name is missing"
	[ -n "$PREDECESSOR_JOB_UID" ] || fail "predecessor read-only Job UID is missing"
	completed_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
	kube -n "$PROOF_NAMESPACE" patch job "$PREDECESSOR_JOB_NAME" --subresource=status \
		--type=merge -p "{\"status\":{\"completionTime\":\"$completed_at\",\"conditions\":[{\"type\":\"Failed\",\"status\":\"True\",\"reason\":\"PredecessorUpgradeProof\",\"message\":\"terminal read-only Job retained across quiescence\",\"lastProbeTime\":\"$completed_at\",\"lastTransitionTime\":\"$completed_at\"}]}}" >/dev/null
	kube -n "$PROOF_NAMESPACE" get job "$PREDECESSOR_JOB_NAME" -o json |
		jq -e --arg uid "$PREDECESSOR_JOB_UID" '
          .metadata.uid == $uid and
          (.status.conditions | any(.type == "Failed" and .status == "True")) and
          (.spec | has("ttlSecondsAfterFinished") | not)
        ' >/dev/null || fail "predecessor read-only Job did not survive quiescence at a terminal cleanup boundary"
	kube -n "$PROOF_NAMESPACE" get job "$PREDECESSOR_JOB_NAME" -o json |
		jq -S '{
          uid: .metadata.uid,
          name: .metadata.name,
          namespace: .metadata.namespace,
          labels: .metadata.labels,
          annotations: .metadata.annotations,
          ownerReferences: .metadata.ownerReferences,
          finalizers: (.metadata.finalizers // []),
          spec: (.spec | del(.ttlSecondsAfterFinished))
		}' >"$WORK_DIR/predecessor-read-only-job-before-cleanup.json"
}

stage_predecessor_read_only_job_uid_gap() {
	[ -n "$PREDECESSOR_JOB_NAME" ] || fail "predecessor read-only Job name is missing"
	[ -n "$PREDECESSOR_JOB_UID" ] || fail "predecessor read-only Job UID is missing"
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PREDECESSOR_JOB_SCHEMA" --subresource=status \
		--type=json -p='[{"op":"remove","path":"/status/activeOperation/jobUID"}]' >/dev/null
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PREDECESSOR_JOB_SCHEMA" -o json |
		jq -e \
			--arg job_name "$PREDECESSOR_JOB_NAME" '
          .status.activeOperation.jobName == $job_name and
          .status.activeOperation.type == "Resolve" and
          (.status.activeOperation | has("jobUID") | not)
        ' >/dev/null || fail "predecessor read-only fixture did not retain the exact Job name with an empty committed UID"
	kube -n "$PROOF_NAMESPACE" get job "$PREDECESSOR_JOB_NAME" -o json |
		jq -e --arg uid "$PREDECESSOR_JOB_UID" '
          .metadata.uid == $uid and
          (.status.conditions | any(.type == "Failed" and .status == "True")) and
          (.spec | has("ttlSecondsAfterFinished") | not)
        ' >/dev/null || fail "late-created predecessor read-only Job identity changed while staging the UID gap"
}

wait_for_predecessor_read_only_job_cleanup() {
	deadline=$(($(date +%s) + 120))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		if kube -n "$PROOF_NAMESPACE" get job "$PREDECESSOR_JOB_NAME" -o json \
			>"$WORK_DIR/predecessor-read-only-job-after.json" 2>/dev/null &&
			[ "$(jq -r '.spec.ttlSecondsAfterFinished // 0' "$WORK_DIR/predecessor-read-only-job-after.json")" -eq 300 ]; then
			jq -S '{
              uid: .metadata.uid,
              name: .metadata.name,
              namespace: .metadata.namespace,
              labels: .metadata.labels,
              annotations: .metadata.annotations,
              ownerReferences: .metadata.ownerReferences,
              finalizers: (.metadata.finalizers // []),
              spec: (.spec | del(.ttlSecondsAfterFinished))
            }' "$WORK_DIR/predecessor-read-only-job-after.json" \
				>"$WORK_DIR/predecessor-read-only-job-after-cleanup.json"
			cmp "$WORK_DIR/predecessor-read-only-job-before-cleanup.json" \
				"$WORK_DIR/predecessor-read-only-job-after-cleanup.json" ||
				fail "candidate cleanup changed the predecessor Job outside ttlSecondsAfterFinished"
			return
		fi
		sleep 1
	done
	fail "candidate manager did not schedule cleanup for the quiesced predecessor read-only Job"
}

wait_for_successful_fixture_job() {
	job_name=$1
	deadline=$(($(date +%s) + 180))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		if kube -n "$PROOF_NAMESPACE" get job "$job_name" -o json \
			>"$WORK_DIR/fixture-job.json" 2>/dev/null; then
			if jq -e '.status.conditions | any(.type == "Complete" and .status == "True")' \
				"$WORK_DIR/fixture-job.json" >/dev/null; then
				return
			fi
			if jq -e '.status.conditions | any(.type == "Failed" and .status == "True")' \
				"$WORK_DIR/fixture-job.json" >/dev/null; then
				fail "fixture Job $job_name failed"
			fi
		fi
		sleep 1
	done
	fail "fixture Job $job_name did not complete"
}

external_predecessor_postgres_query() {
	query=$1
	docker --context "$E2E_DOCKER_CONTEXT" exec "$E2E_EXTERNAL_POSTGRES_CONTAINER_ID" \
		sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD"; export PGPASSWORD; exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atqc "$1"' \
		sh "$query"
}

start_predecessor_apply_barrier() {
	[ "$PREDECESSOR_APPLY_BARRIER_ACTIVE" -eq 0 ] ||
		fail "predecessor Apply database barrier is already active"
	docker --context "$E2E_DOCKER_CONTEXT" exec "$E2E_EXTERNAL_POSTGRES_CONTAINER_ID" \
		sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD"; export PGPASSWORD; PGAPPNAME=ptah-operator-predecessor-apply-barrier; export PGAPPNAME; exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -Atqc "SELECT pg_advisory_lock(742019370001); SELECT pg_sleep(900)"' \
		>"$WORK_DIR/predecessor-apply-barrier.out" \
		2>"$WORK_DIR/predecessor-apply-barrier.err" &
	PREDECESSOR_APPLY_BARRIER_PID=$!
	PREDECESSOR_APPLY_BARRIER_ACTIVE=1

	deadline=$(($(date +%s) + 30))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		held=$(external_predecessor_postgres_query \
			"SELECT count(*) FROM pg_locks AS lock JOIN pg_stat_activity AS activity USING (pid) WHERE lock.locktype = 'advisory' AND lock.granted AND activity.application_name = 'ptah-operator-predecessor-apply-barrier'") ||
			fail "could not inspect the predecessor Apply database barrier"
		if [ "$held" -eq 1 ]; then
			return
		fi
		if ! kill -0 "$PREDECESSOR_APPLY_BARRIER_PID" 2>/dev/null; then
			cat "$WORK_DIR/predecessor-apply-barrier.err" >&2
			fail "predecessor Apply database barrier exited before acquiring its lock"
		fi
		sleep 1
	done
	fail "predecessor Apply database barrier did not acquire its lock"
}

wait_for_predecessor_apply_barrier_contention() {
	deadline=$(($(date +%s) + 60))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		waiting=$(external_predecessor_postgres_query \
			"SELECT count(*) FROM pg_locks AS waiting JOIN pg_locks AS held USING (locktype, database, classid, objid, objsubid) JOIN pg_stat_activity AS holder ON holder.pid = held.pid WHERE held.locktype = 'advisory' AND held.granted AND NOT waiting.granted AND waiting.pid <> held.pid AND holder.application_name = 'ptah-operator-predecessor-apply-barrier'") ||
			fail "could not inspect predecessor Apply barrier contention"
		if [ "$waiting" -eq 1 ]; then
			return
		fi
		sleep 1
	done
	fail "predecessor Apply did not block on the controlled database barrier"
}

assert_predecessor_apply_barrier_contended() {
	waiting=$(external_predecessor_postgres_query \
		"SELECT count(*) FROM pg_locks AS waiting JOIN pg_locks AS held USING (locktype, database, classid, objid, objsubid) JOIN pg_stat_activity AS holder ON holder.pid = held.pid WHERE held.locktype = 'advisory' AND held.granted AND NOT waiting.granted AND waiting.pid <> held.pid AND holder.application_name = 'ptah-operator-predecessor-apply-barrier'") ||
		fail "could not recheck predecessor Apply barrier contention"
	[ "$waiting" -eq 1 ] || fail "predecessor Apply left the controlled database barrier before release"
}

release_predecessor_apply_barrier() {
	[ "$PREDECESSOR_APPLY_BARRIER_ACTIVE" -eq 1 ] ||
		fail "predecessor Apply database barrier is not active"
	released=$(external_predecessor_postgres_query \
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = 'ptah-operator-predecessor-apply-barrier' AND pid <> pg_backend_pid()") ||
		fail "could not release predecessor Apply database barrier"
	[ "$released" = t ] || fail "predecessor Apply database barrier release did not terminate exactly one holder"
	PREDECESSOR_APPLY_BARRIER_ACTIVE=0
	if wait "$PREDECESSOR_APPLY_BARRIER_PID"; then
		fail "predecessor Apply database barrier exited successfully instead of being explicitly released"
	fi
	PREDECESSOR_APPLY_BARRIER_PID=
}

prepare_predecessor_apply_fixture() {
	E2E_REGISTRY_CREDENTIALS_FILE=${E2E_REGISTRY_CREDENTIALS_FILE:?E2E_REGISTRY_CREDENTIALS_FILE is required for predecessor Apply proof}
	E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE=${E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE:?E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE is required for predecessor Apply proof}
	E2E_EXTERNAL_POSTGRES_IP=${E2E_EXTERNAL_POSTGRES_IP:?E2E_EXTERNAL_POSTGRES_IP is required for predecessor Apply proof}
	E2E_DOCKER_CONTEXT=${E2E_DOCKER_CONTEXT:?E2E_DOCKER_CONTEXT is required for predecessor Apply proof}
	E2E_EXTERNAL_POSTGRES_CONTAINER_ID=${E2E_EXTERNAL_POSTGRES_CONTAINER_ID:?E2E_EXTERNAL_POSTGRES_CONTAINER_ID is required for predecessor Apply proof}
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
	actual_external_postgres_id=$(docker --context "$E2E_DOCKER_CONTEXT" container inspect \
		--format '{{.Id}}' "$E2E_EXTERNAL_POSTGRES_CONTAINER_ID") ||
		fail "could not inspect the external PostgreSQL barrier container"
	[ "$actual_external_postgres_id" = "$E2E_EXTERNAL_POSTGRES_CONTAINER_ID" ] ||
		fail "external PostgreSQL barrier container identity changed"

	predecessor_executor_image=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PREDECESSOR_SCHEMA" \
		-o jsonpath='{.status.executionBinding.executorImage}')
	printf '%s\n' "$predecessor_executor_image" |
		grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' ||
		fail "predecessor execution binding does not contain a digest-pinned executor image"
	predecessor_registry=${predecessor_executor_image%%/*}
	jq -n \
		--arg namespace "$PROOF_NAMESPACE" \
		--arg name "$PREDECESSOR_APPLY_PULL_SECRET" \
		--arg registry "$predecessor_registry" \
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
		--arg name "$PREDECESSOR_APPLY_DATABASE" \
		--arg authority "${PREDECESSOR_APPLY_DATABASE}.${PROOF_NAMESPACE}.svc.cluster.local:5432" \
		--slurpfile credentials "$E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE" '
      {
        apiVersion: "v1", kind: "Secret", immutable: true,
        metadata: {namespace: $namespace, name: $name}, type: "Opaque",
        stringData: {
          url: ("postgres://" + $credentials[0].username + ":" + $credentials[0].password +
            "@" + $authority + "/" + $credentials[0].database + "?sslmode=disable")
        }
      }
    ' | kube create -f - >/dev/null
	jq -n \
		--arg namespace "$PROOF_NAMESPACE" \
		--arg name "$PREDECESSOR_APPLY_DATABASE" '
      {
        apiVersion: "v1", kind: "Service",
        metadata: {namespace: $namespace, name: $name},
        spec: {ports: [{name: "postgresql", port: 5432, protocol: "TCP", targetPort: 5432}]}
      }
    ' | kube create -f - >/dev/null
	predecessor_database_service_uid=$(kube -n "$PROOF_NAMESPACE" get service \
		"$PREDECESSOR_APPLY_DATABASE" -o jsonpath='{.metadata.uid}')
	jq -n \
		--arg namespace "$PROOF_NAMESPACE" \
		--arg name "${PREDECESSOR_APPLY_DATABASE}-docker" \
		--arg service "$PREDECESSOR_APPLY_DATABASE" \
		--arg serviceUID "$predecessor_database_service_uid" \
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

	predecessor_policy_file=$WORK_DIR/predecessor-apply-policy.yaml
	printf '%s\n' 'version: 1' >"$predecessor_policy_file"
	kube -n "$PROOF_NAMESPACE" create configmap "$PREDECESSOR_APPLY_POLICY" \
		--from-file="policy.yaml=$predecessor_policy_file" >/dev/null
	kube -n "$PROOF_NAMESPACE" patch configmap "$PREDECESSOR_APPLY_POLICY" --type=merge \
		-p='{"immutable":true}' >/dev/null
	predecessor_policy_uid=$(kube -n "$PROOF_NAMESPACE" get configmap "$PREDECESSOR_APPLY_POLICY" \
		-o jsonpath='{.metadata.uid}')

	predecessor_artifact_digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
	kube -n "$PROOF_NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: operator.ptah.dev/v1alpha1
kind: PtahSchema
metadata:
  name: $PREDECESSOR_APPLY_SCHEMA
spec:
  suspend: true
  interval: 24h
  target:
    engine: PostgreSQL
    coordinationKey: $PREDECESSOR_APPLY_SCHEMA
    urlFrom: {name: $PREDECESSOR_APPLY_DATABASE, key: url}
  desired:
    ociRef: oci://example.invalid/schema@$predecessor_artifact_digest
    verificationPolicyFrom: {name: $PREDECESSOR_APPLY_POLICY, key: policy.yaml}
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
    imagePullSecrets: [{name: $PREDECESSOR_APPLY_PULL_SECRET}]
EOF
	wait_for_suspended "$PREDECESSOR_APPLY_SCHEMA"

	predecessor_plan_source=$WORK_DIR/predecessor-apply-schema.sql
	cp "$ROOT_DIR/testdata/e2e/postgres-v1.sql" "$predecessor_plan_source"
	kube -n "$PROOF_NAMESPACE" create configmap predecessor-apply-plan-source \
		--from-file="schema.sql=$predecessor_plan_source" >/dev/null
	jq -n \
		--arg namespace "$PROOF_NAMESPACE" \
		--arg image "$predecessor_executor_image" \
		--arg pullSecret "$PREDECESSOR_APPLY_PULL_SECRET" \
		--arg databaseSecret "$PREDECESSOR_APPLY_DATABASE" '
      {
        apiVersion: "batch/v1", kind: "Job",
        metadata: {namespace: $namespace, name: "predecessor-apply-plan-source"},
        spec: {
          backoffLimit: 0, activeDeadlineSeconds: 180, ttlSecondsAfterFinished: 300,
          template: {
            metadata: {labels: {"app.kubernetes.io/component": "predecessor-apply-plan-source"}},
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
                {name: "schema", configMap: {name: "predecessor-apply-plan-source"}},
                {name: "work", emptyDir: {sizeLimit: "64Mi"}}
              ]
            }
          }
        }
      }
    ' | kube create -f - >/dev/null
	wait_for_successful_fixture_job predecessor-apply-plan-source
	kube -n "$PROOF_NAMESPACE" logs job/predecessor-apply-plan-source \
		>"$WORK_DIR/predecessor-native-plan.json"
	jq -ce --arg plan_name "$PREDECESSOR_APPLY_SCHEMA" '
      if .format_version == 1 and
        (.from_fingerprint | test("^sha256:[0-9a-f]{64}$")) and
        (.to_fingerprint | test("^sha256:[0-9a-f]{64}$"))
      then
        .name = $plan_name |
        .destructive = false |
        .statements = [{
          sql: "SELECT pg_advisory_lock(742019370001)", severity: "safe",
          reason: "upgrade quiescence proof"
        }]
      else error("native plan lacks exact state fingerprints")
      end
    ' "$WORK_DIR/predecessor-native-plan.json" >"$WORK_DIR/predecessor-apply-plan.json" ||
		fail "could not derive an exact long-running predecessor Apply plan"

	kube -n "$PROOF_NAMESPACE" get ptahschema "$PREDECESSOR_APPLY_SCHEMA" -o json \
		>"$WORK_DIR/predecessor-apply-schema.json"
	go -C "$ROOT_DIR" run ./hack/predecessorapplyfixture \
		-schema "$WORK_DIR/predecessor-apply-schema.json" \
		-plan "$WORK_DIR/predecessor-apply-plan.json" \
		-policy-uid "$predecessor_policy_uid" \
		-policy "$predecessor_policy_file" \
		>"$WORK_DIR/predecessor-apply-bundle.json"
	jq -e '
      .plan.spec.contractVersion == 2 and
      (.plan.spec | has("controllerImage") | not) and
      (.plan.spec | has("controllerRevision") | not) and
      (.plan.spec | has("controllerStateVersion") | not) and
      (.plan.spec.chunks | length) == 1
    ' "$WORK_DIR/predecessor-apply-bundle.json" >/dev/null ||
		fail "generated predecessor Apply plan does not have the exact contract-v2 shape"
	jq '.plan' "$WORK_DIR/predecessor-apply-bundle.json" | kube create -f - >/dev/null
	PREDECESSOR_APPLY_PLAN_NAME=$(jq -er '.plan.metadata.name' "$WORK_DIR/predecessor-apply-bundle.json")
	PREDECESSOR_APPLY_PLAN_UID=$(kube -n "$PROOF_NAMESPACE" get ptahschemaplan \
		"$PREDECESSOR_APPLY_PLAN_NAME" -o jsonpath='{.metadata.uid}')
	predecessor_plan_generation=$(kube -n "$PROOF_NAMESPACE" get ptahschemaplan \
		"$PREDECESSOR_APPLY_PLAN_NAME" -o jsonpath='{.metadata.generation}')
	predecessor_chunk_name=$(jq -er '.plan.spec.chunks[0].name' "$WORK_DIR/predecessor-apply-bundle.json")
	jq -n \
		--arg namespace "$PROOF_NAMESPACE" \
		--arg name "$predecessor_chunk_name" \
		--arg plan "$PREDECESSOR_APPLY_PLAN_NAME" \
		--arg planUID "$PREDECESSOR_APPLY_PLAN_UID" \
		--arg schema "$PREDECESSOR_APPLY_SCHEMA" \
		--rawfile content "$WORK_DIR/predecessor-apply-plan.json" '
      {
        apiVersion: "v1", kind: "ConfigMap", immutable: true,
        metadata: {
          namespace: $namespace, name: $name,
          labels: {"operator.ptah.dev/plan": $plan, "operator.ptah.dev/schema": $schema},
          ownerReferences: [{
            apiVersion: "operator.ptah.dev/v1alpha1", kind: "PtahSchemaPlan",
            name: $plan, uid: $planUID, controller: true, blockOwnerDeletion: true
          }]
        },
        binaryData: {chunk: ($content | @base64)}
      }
    ' | kube create -f - >/dev/null
	predecessor_chunk_uid=$(kube -n "$PROOF_NAMESPACE" get configmap "$predecessor_chunk_name" \
		-o jsonpath='{.metadata.uid}')
	plan_ready_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
	kube -n "$PROOF_NAMESPACE" patch ptahschemaplan "$PREDECESSOR_APPLY_PLAN_NAME" \
		--subresource=status --type=merge -p "{\"status\":{\"observedGeneration\":$predecessor_plan_generation,\"publishedChunks\":[{\"name\":\"$predecessor_chunk_name\",\"uid\":\"$predecessor_chunk_uid\",\"index\":0}],\"conditions\":[{\"type\":\"Ready\",\"status\":\"True\",\"reason\":\"Published\",\"message\":\"Verified 1 immutable plan chunks\",\"observedGeneration\":$predecessor_plan_generation,\"lastTransitionTime\":\"$plan_ready_at\"}]}}" >/dev/null
}

start_predecessor_apply_fixture() {
	[ -n "$PREDECESSOR_APPLY_PLAN_NAME" ] || fail "predecessor Apply plan name is missing"
	[ -n "$PREDECESSOR_APPLY_PLAN_UID" ] || fail "predecessor Apply plan UID is missing"
	stop_controller_deployment
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PREDECESSOR_APPLY_SCHEMA" --type=merge \
		-p='{"spec":{"suspend":false}}' >/dev/null
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PREDECESSOR_APPLY_SCHEMA" -o json \
		>"$WORK_DIR/predecessor-apply-schema-enabled.json"
	predecessor_apply_generation=$(jq -er '.metadata.generation' \
		"$WORK_DIR/predecessor-apply-schema-enabled.json")
	jq \
		--slurpfile bundle "$WORK_DIR/predecessor-apply-bundle.json" \
		--arg planUID "$PREDECESSOR_APPLY_PLAN_UID" \
		--argjson generation "$predecessor_apply_generation" '
      .status = $bundle[0].schemaStatus |
      .status.observedGeneration = $generation |
      .status.plan.uid = $planUID |
      (.status.conditions[].observedGeneration) = $generation
    ' "$WORK_DIR/predecessor-apply-schema-enabled.json" \
		>"$WORK_DIR/predecessor-apply-schema-ready.json"
	kube replace --subresource=status -f "$WORK_DIR/predecessor-apply-schema-ready.json" >/dev/null
	start_controller_deployment
	kube -n "$E2E_OPERATOR_NAMESPACE" rollout status deployment "$CONTROLLER_DEPLOYMENT" \
		--timeout=3m >/dev/null

	deadline=$(($(date +%s) + 180))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		kube -n "$PROOF_NAMESPACE" get ptahschema "$PREDECESSOR_APPLY_SCHEMA" -o json \
			>"$WORK_DIR/predecessor-apply-running-schema.json"
		PREDECESSOR_APPLY_JOB_NAME=$(jq -r \
			'.status.activeOperation | select(.type == "Apply" and .dispatchStarted == true) | .jobName // empty' \
			"$WORK_DIR/predecessor-apply-running-schema.json")
		committed_job_uid=$(jq -r '.status.activeOperation.jobUID // empty' \
			"$WORK_DIR/predecessor-apply-running-schema.json")
		if [ -n "$PREDECESSOR_APPLY_JOB_NAME" ] && [ -n "$committed_job_uid" ] &&
			kube -n "$PROOF_NAMESPACE" get job "$PREDECESSOR_APPLY_JOB_NAME" -o json \
				>"$WORK_DIR/predecessor-apply-running-job.json" 2>/dev/null; then
			PREDECESSOR_APPLY_JOB_UID=$(jq -r '.metadata.uid' \
				"$WORK_DIR/predecessor-apply-running-job.json")
			if [ "$committed_job_uid" = "$PREDECESSOR_APPLY_JOB_UID" ]; then
				kube -n "$PROOF_NAMESPACE" get pods -o json |
					jq -e --arg uid "$PREDECESSOR_APPLY_JOB_UID" '
                  [.items[] | select(
                    .status.phase == "Running" and
                    any(.metadata.ownerReferences[]?;
                      .apiVersion == "batch/v1" and .kind == "Job" and .uid == $uid and
                      .controller == true
                    )
                  )] | if length == 1 then .[0] else empty end
                ' >"$WORK_DIR/predecessor-apply-running-pod.json" 2>/dev/null || true
				if [ -s "$WORK_DIR/predecessor-apply-running-pod.json" ]; then
					PREDECESSOR_APPLY_POD_NAME=$(jq -er '.metadata.name' \
						"$WORK_DIR/predecessor-apply-running-pod.json")
					PREDECESSOR_APPLY_POD_UID=$(jq -er '.metadata.uid' \
						"$WORK_DIR/predecessor-apply-running-pod.json")
					break
				fi
			fi
		fi
		sleep 1
	done
	[ -n "$PREDECESSOR_APPLY_POD_UID" ] || fail "predecessor Apply Job did not reach a running Pod"
	jq -e --arg schema "$PREDECESSOR_APPLY_SCHEMA" '
      .metadata.labels as $labels |
      $labels == {
        "app.kubernetes.io/component": "schema-operation",
        "app.kubernetes.io/managed-by": "ptah-operator",
        "operator.ptah.dev/operation": "apply",
        "operator.ptah.dev/operation-id": $labels["operator.ptah.dev/operation-id"],
        "operator.ptah.dev/schema": $schema
      } and
      (.metadata.annotations | keys | sort) == [
        "operator.ptah.dev/admission-snapshot-digest",
        "operator.ptah.dev/execution-binding-id",
        "operator.ptah.dev/input-fingerprint",
        "operator.ptah.dev/operation-id",
        "operator.ptah.dev/plan-content-digest",
        "operator.ptah.dev/plan-fingerprint",
        "operator.ptah.dev/ptah-version"
      ] and
      .spec.template.metadata.annotations == .metadata.annotations and
      (.spec | has("ttlSecondsAfterFinished") | not)
    ' "$WORK_DIR/predecessor-apply-running-job.json" >/dev/null ||
		fail "running predecessor Apply Job does not match its exact seven-annotation contract"
}

stage_predecessor_apply_job_uid_gap_while_running() {
	[ -n "$PREDECESSOR_APPLY_JOB_NAME" ] || fail "predecessor Apply Job name is missing"
	[ -n "$PREDECESSOR_APPLY_JOB_UID" ] || fail "predecessor Apply Job UID is missing"
	[ -n "$PREDECESSOR_APPLY_POD_UID" ] || fail "predecessor Apply Pod UID is missing"
	kube -n "$PROOF_NAMESPACE" get job "$PREDECESSOR_APPLY_JOB_NAME" -o json \
		>"$WORK_DIR/predecessor-apply-running-before-upgrade.json"
	jq -e --arg uid "$PREDECESSOR_APPLY_JOB_UID" '
      .metadata.uid == $uid and
      ((.status.conditions // []) |
        any((.type == "Complete" or .type == "Failed") and .status == "True") | not) and
      (.spec | has("ttlSecondsAfterFinished") | not)
    ' "$WORK_DIR/predecessor-apply-running-before-upgrade.json" >/dev/null ||
		fail "predecessor Apply Job is not running at the candidate upgrade boundary"
	kube -n "$PROOF_NAMESPACE" get pod "$PREDECESSOR_APPLY_POD_NAME" -o json |
		jq -e --arg uid "$PREDECESSOR_APPLY_POD_UID" '
		  .metadata.uid == $uid and .status.phase == "Running"
		' >/dev/null || fail "predecessor Apply Pod is not running at the candidate upgrade boundary"
	jq -S '{
      uid: .metadata.uid,
      name: .metadata.name,
      namespace: .metadata.namespace,
      labels: .metadata.labels,
      annotations: .metadata.annotations,
      ownerReferences: .metadata.ownerReferences,
      finalizers: (.metadata.finalizers // []),
      spec: (.spec | del(.ttlSecondsAfterFinished))
    }' "$WORK_DIR/predecessor-apply-running-before-upgrade.json" \
		>"$WORK_DIR/predecessor-apply-job-before-cleanup.json"
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PREDECESSOR_APPLY_SCHEMA" --subresource=status \
		--type=json -p='[{"op":"remove","path":"/status/activeOperation/jobUID"}]' >/dev/null
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PREDECESSOR_APPLY_SCHEMA" -o json |
		jq -e --arg name "$PREDECESSOR_APPLY_JOB_NAME" '
          .status.activeOperation.type == "Apply" and
          .status.activeOperation.dispatchStarted == true and
          .status.activeOperation.jobName == $name and
          (.status.activeOperation | has("jobUID") | not) and
          (.status | has("pendingObservation") | not)
		' >/dev/null || fail "predecessor Apply fixture did not retain the running late-create UID gap"
}

assert_predecessor_apply_remains_exclusive_while_running() {
	deadline=$(($(date +%s) + 90))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		if kube -n "$PROOF_NAMESPACE" get ptahschema "$PREDECESSOR_APPLY_SCHEMA" -o json \
			>"$WORK_DIR/predecessor-apply-fenced-schema.json" 2>/dev/null &&
			jq -e \
				--arg job "$PREDECESSOR_APPLY_JOB_NAME" \
				--arg uid "$PREDECESSOR_APPLY_JOB_UID" \
				--arg pod_uid "$PREDECESSOR_APPLY_POD_UID" '
              .status.phase == "Pending" and
              (.status | has("activeOperation") | not) and
              .status.pendingObservation.outcome == "OutcomeUnknown" and
              .status.pendingObservation.applyJobName == $job and
              .status.pendingObservation.applyJobUID == $uid and
              (.status.pendingObservation.applyPodUIDs | index($pod_uid) != null) and
              .status.pendingObservation.applyPodCount == 1 and
              .status.pendingObservation.planRequired != true and
              .status.pendingObservation.plan.executionBindingID != .status.executionBinding.epoch
			' "$WORK_DIR/predecessor-apply-fenced-schema.json" >/dev/null; then
			break
		fi
		sleep 1
	done
	jq -e \
		--arg job "$PREDECESSOR_APPLY_JOB_NAME" \
		--arg uid "$PREDECESSOR_APPLY_JOB_UID" \
		--arg pod_uid "$PREDECESSOR_APPLY_POD_UID" '
      .status.phase == "Pending" and
      (.status | has("activeOperation") | not) and
      .status.pendingObservation.outcome == "OutcomeUnknown" and
      .status.pendingObservation.applyJobName == $job and
      .status.pendingObservation.applyJobUID == $uid and
      (.status.pendingObservation.applyPodUIDs | index($pod_uid) != null) and
      .status.pendingObservation.applyPodCount == 1 and
      .status.pendingObservation.planRequired != true and
      .status.pendingObservation.plan.executionBindingID != .status.executionBinding.epoch
	' "$WORK_DIR/predecessor-apply-fenced-schema.json" >/dev/null ||
		fail "candidate manager did not durably fence and adopt the running predecessor Apply"

	kube -n "$PROOF_NAMESPACE" get job "$PREDECESSOR_APPLY_JOB_NAME" -o json \
		>"$WORK_DIR/predecessor-apply-running-after-upgrade.json"
	jq -e --arg uid "$PREDECESSOR_APPLY_JOB_UID" '
      .metadata.uid == $uid and
      ((.status.conditions // []) |
        any((.type == "Complete" or .type == "Failed") and .status == "True") | not) and
      (.spec | has("ttlSecondsAfterFinished") | not)
	' "$WORK_DIR/predecessor-apply-running-after-upgrade.json" >/dev/null ||
		fail "candidate manager replaced, completed, or cleaned the running predecessor Apply Job"
	kube -n "$PROOF_NAMESPACE" get pod "$PREDECESSOR_APPLY_POD_NAME" -o json |
		jq -e --arg uid "$PREDECESSOR_APPLY_POD_UID" '
          .metadata.uid == $uid and .status.phase == "Running"
		' >/dev/null || fail "candidate upgrade did not retain the running predecessor Apply Pod UID"
	kube -n "$PROOF_NAMESPACE" get jobs \
		-l "operator.ptah.dev/schema=$PREDECESSOR_APPLY_SCHEMA" -o json |
		jq -e --arg name "$PREDECESSOR_APPLY_JOB_NAME" --arg uid "$PREDECESSOR_APPLY_JOB_UID" '
          .items | length == 1 and .[0].metadata.name == $name and .[0].metadata.uid == $uid
		' >/dev/null || fail "candidate manager launched new work over the running predecessor Apply"
}

wait_for_predecessor_apply_job_terminal() {
	deadline=$(($(date +%s) + 300))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		if kube -n "$PROOF_NAMESPACE" get job "$PREDECESSOR_APPLY_JOB_NAME" -o json \
			>"$WORK_DIR/predecessor-apply-terminal-job.json" 2>/dev/null &&
			jq -e '.status.conditions | any((.type == "Complete" or .type == "Failed") and .status == "True")' \
				"$WORK_DIR/predecessor-apply-terminal-job.json" >/dev/null; then
			break
		fi
		sleep 1
	done
	jq -e --arg uid "$PREDECESSOR_APPLY_JOB_UID" '
      .metadata.uid == $uid and
      (.status.conditions | any((.type == "Complete" or .type == "Failed") and .status == "True"))
	' "$WORK_DIR/predecessor-apply-terminal-job.json" >/dev/null ||
		fail "predecessor Apply Job did not finish after the candidate fenced it"
	kube -n "$PROOF_NAMESPACE" get pod "$PREDECESSOR_APPLY_POD_NAME" -o json |
		jq -e --arg uid "$PREDECESSOR_APPLY_POD_UID" '
          .metadata.uid == $uid and (.status.phase == "Succeeded" or .status.phase == "Failed")
		' >/dev/null || fail "predecessor Apply Pod is not terminal after the candidate fence"
}

wait_for_predecessor_apply_job_cleanup() {
	deadline=$(($(date +%s) + 180))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		if kube -n "$PROOF_NAMESPACE" get job "$PREDECESSOR_APPLY_JOB_NAME" -o json \
			>"$WORK_DIR/predecessor-apply-job-after.json" 2>/dev/null &&
			[ "$(jq -r '.spec.ttlSecondsAfterFinished // 0' "$WORK_DIR/predecessor-apply-job-after.json")" -eq 300 ] &&
			kube -n "$PROOF_NAMESPACE" get ptahschema "$PREDECESSOR_APPLY_SCHEMA" -o json \
				>"$WORK_DIR/predecessor-apply-schema-after.json"; then
			if jq -e \
				--arg job "$PREDECESSOR_APPLY_JOB_NAME" \
				--arg uid "$PREDECESSOR_APPLY_JOB_UID" '
              .status.phase == "Pending" and
              (.status | has("activeOperation") | not) and
              (.status | has("applied") | not) and
              .status.pendingObservation.outcome == "OutcomeUnknown" and
              .status.pendingObservation.applyJobName == $job and
              .status.pendingObservation.applyJobUID == $uid and
              .status.pendingObservation.planRequired != true and
              .status.pendingObservation.plan.executionBindingID != .status.executionBinding.epoch and
              any(.status.conditions[];
                .type == "PlanReady" and .status == "False" and .reason == "ExecutionBindingChanged") and
              any(.status.conditions[];
                .type == "ApprovalRequired" and .status == "False" and .reason == "ExecutionBindingChanged")
            ' "$WORK_DIR/predecessor-apply-schema-after.json" >/dev/null; then
				jq -S '{
                  uid: .metadata.uid,
                  name: .metadata.name,
                  namespace: .metadata.namespace,
                  labels: .metadata.labels,
                  annotations: .metadata.annotations,
                  ownerReferences: .metadata.ownerReferences,
                  finalizers: (.metadata.finalizers // []),
                  spec: (.spec | del(.ttlSecondsAfterFinished))
                }' "$WORK_DIR/predecessor-apply-job-after.json" \
					>"$WORK_DIR/predecessor-apply-job-after-cleanup.json"
				cmp "$WORK_DIR/predecessor-apply-job-before-cleanup.json" \
					"$WORK_DIR/predecessor-apply-job-after-cleanup.json" ||
					fail "candidate cleanup changed the predecessor Apply Job outside ttlSecondsAfterFinished"
				return
			fi
		fi
		sleep 1
	done
	fail "candidate manager did not adopt and clean the quiesced predecessor Apply Job"
}

runtime_deployment_names() {
	CONTROLLER_DEPLOYMENT=$(kube -n "$E2E_OPERATOR_NAMESPACE" get deployment \
		-l 'app.kubernetes.io/component=controller' -o jsonpath='{.items[0].metadata.name}')
	ROTATOR_DEPLOYMENT=$(kube -n "$E2E_OPERATOR_NAMESPACE" get deployment \
		-l 'app.kubernetes.io/component=certificate-rotation' -o jsonpath='{.items[0].metadata.name}')
	[ -n "$CONTROLLER_DEPLOYMENT" ] || fail "controller Deployment is missing"
	[ -n "$ROTATOR_DEPLOYMENT" ] || fail "certificate-rotation Deployment is missing"
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

assert_predecessor_certificate_update_allowed() {
	command -v openssl >/dev/null 2>&1 || fail "OpenSSL is required for predecessor certificate proof"
	runtime_deployment_names
	rotator_service_account=$(kube -n "$E2E_OPERATOR_NAMESPACE" get deployment "$ROTATOR_DEPLOYMENT" \
		-o jsonpath='{.spec.template.spec.serviceAccountName}')
	[ -n "$rotator_service_account" ] || fail "predecessor certificate ServiceAccount is missing"
	kube -n "$E2E_OPERATOR_NAMESPACE" rollout status deployment "$ROTATOR_DEPLOYMENT" \
		--timeout=60s >/dev/null || fail "predecessor certificate rotator is not ready after failed preflight"
	rotator_pod=$(kube -n "$E2E_OPERATOR_NAMESPACE" get pod \
		-l "app.kubernetes.io/instance=${E2E_HELM_RELEASE},app.kubernetes.io/component=certificate-rotation" \
		-o jsonpath='{.items[0].metadata.name}')
	rotator_pod_uid=$(kube -n "$E2E_OPERATOR_NAMESPACE" get pod "$rotator_pod" -o jsonpath='{.metadata.uid}')
	rotator_service_account_uid=$(kube -n "$E2E_OPERATOR_NAMESPACE" get serviceaccount \
		"$rotator_service_account" -o jsonpath='{.metadata.uid}')
	if [ -z "$rotator_pod" ] || [ -z "$rotator_pod_uid" ] || [ -z "$rotator_service_account_uid" ]; then
		fail "predecessor certificate workload-bound identity is incomplete"
	fi

	kube get validatingwebhookconfiguration ptah-operator-admission -o json \
		>"$WORK_DIR/predecessor-certificate-source.json"
	current_bundle=$(jq -er '.webhooks[0].clientConfig.caBundle | select(length > 0)' \
		"$WORK_DIR/predecessor-certificate-source.json")
	if ! printf '%s' "$current_bundle" | openssl base64 -d -A \
		>"$WORK_DIR/predecessor-certificate.pem"; then
		fail "predecessor validating webhook CA is not valid base64"
	fi
	overlap_bundle=$(awk '1' "$WORK_DIR/predecessor-certificate.pem" \
		"$WORK_DIR/predecessor-certificate.pem" | openssl base64 -A)
	[ -n "$overlap_bundle" ] || fail "could not build predecessor CA-only update fixture"

	for singleton_resource in mutatingwebhookconfiguration validatingwebhookconfiguration; do
		candidate=$WORK_DIR/${singleton_resource}-certificate-update.json
		result=$WORK_DIR/${singleton_resource}-certificate-update-result.json
		kube get "$singleton_resource" ptah-operator-admission -o json |
			jq --arg bundle "$overlap_bundle" \
				'(.webhooks[].clientConfig.caBundle) = $bundle' >"$candidate"
		if ! kube \
			--as "system:serviceaccount:${E2E_OPERATOR_NAMESPACE}:${rotator_service_account}" \
			--as-uid "$rotator_service_account_uid" \
			--as-group system:serviceaccounts \
			--as-group "system:serviceaccounts:${E2E_OPERATOR_NAMESPACE}" \
			--as-group system:authenticated \
			--as-user-extra "authentication.kubernetes.io/pod-name=$rotator_pod" \
			--as-user-extra "authentication.kubernetes.io/pod-uid=$rotator_pod_uid" \
			replace --field-manager='' --dry-run=server -f "$candidate" -o json >"$result"; then
			fail "retained certificate guard blocked a CA-only predecessor $singleton_resource update"
		fi
		old_generation=$(jq -er '.metadata.generation | select(type == "number")' "$candidate")
		new_generation=$(jq -er '.metadata.generation | select(type == "number")' "$result")
		[ "$new_generation" -eq $((old_generation + 1)) ] ||
			fail "CA-only predecessor $singleton_resource update did not exercise the server generation transition"
	done
}

assert_release_runtime_removed() {
	for singleton_resource in mutatingwebhookconfiguration validatingwebhookconfiguration; do
		remaining=$(kube get "$singleton_resource" ptah-operator-admission \
			--ignore-not-found=true -o name)
		[ -z "$remaining" ] || fail "$singleton_resource/ptah-operator-admission survived uninstall"
	done
	remaining=$(kube -n "$E2E_OPERATOR_NAMESPACE" get configmap \
		ptah-operator-release-activation --ignore-not-found=true -o name)
	[ -z "$remaining" ] || fail "release activation ConfigMap survived uninstall"

	for cluster_resource in \
		validatingadmissionpolicy \
		validatingadmissionpolicybinding \
		clusterrole \
		clusterrolebinding; do
		remaining=$(kube get "$cluster_resource" \
			-l "app.kubernetes.io/instance=$E2E_HELM_RELEASE" -o json |
			jq -r '.items | length')
		[ "$remaining" -eq 0 ] ||
			fail "$remaining labeled $cluster_resource objects survived uninstall"
	done
	for namespaced_resource in deployment role rolebinding serviceaccount job pod configmap; do
		remaining=$(kube -n "$E2E_OPERATOR_NAMESPACE" get "$namespaced_resource" \
			-l "app.kubernetes.io/instance=$E2E_HELM_RELEASE" -o json |
			jq -r '.items | length')
		[ "$remaining" -eq 0 ] ||
			fail "$remaining labeled $namespaced_resource objects survived uninstall"
	done
}

snapshot_runtime_deployment() {
	deployment_name=$1
	destination=$2
	kube -n "$E2E_OPERATOR_NAMESPACE" get deployment "$deployment_name" -o json |
		jq 'del(
          .metadata.creationTimestamp,
          .metadata.generation,
          .metadata.managedFields,
          .metadata.resourceVersion,
          .metadata.uid,
          .metadata.annotations."deployment.kubernetes.io/revision",
          .status
        )' >"$destination"
}

stop_runtime_deployments() {
	runtime_deployment_names
	CONTROLLER_DEPLOYMENT_SNAPSHOT=$WORK_DIR/controller-deployment.json
	ROTATOR_DEPLOYMENT_SNAPSHOT=$WORK_DIR/certificate-deployment.json
	snapshot_runtime_deployment "$CONTROLLER_DEPLOYMENT" "$CONTROLLER_DEPLOYMENT_SNAPSHOT"
	snapshot_runtime_deployment "$ROTATOR_DEPLOYMENT" "$ROTATOR_DEPLOYMENT_SNAPSHOT"
	kube -n "$E2E_OPERATOR_NAMESPACE" delete deployment \
		"$CONTROLLER_DEPLOYMENT" "$ROTATOR_DEPLOYMENT" \
		--cascade=foreground --wait=true >/dev/null
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
		--cascade=foreground --wait=true >/dev/null
	kube -n "$E2E_OPERATOR_NAMESPACE" wait pod \
		-l 'app.kubernetes.io/component=controller' \
		--for=delete --timeout=2m >/dev/null
}

start_runtime_deployments() {
	[ -s "$CONTROLLER_DEPLOYMENT_SNAPSHOT" ] || fail "controller Deployment snapshot is missing"
	[ -s "$ROTATOR_DEPLOYMENT_SNAPSHOT" ] || fail "certificate Deployment snapshot is missing"
	kube create -f "$CONTROLLER_DEPLOYMENT_SNAPSHOT" >/dev/null
	kube create -f "$ROTATOR_DEPLOYMENT_SNAPSHOT" >/dev/null
}

start_controller_deployment() {
	[ -s "$CONTROLLER_DEPLOYMENT_SNAPSHOT" ] || fail "controller Deployment snapshot is missing"
	kube create -f "$CONTROLLER_DEPLOYMENT_SNAPSHOT" >/dev/null
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
	kube -n "$E2E_OPERATOR_NAMESPACE" rollout status deployment "$CONTROLLER_DEPLOYMENT" --timeout=3m >/dev/null
	kube -n "$E2E_OPERATOR_NAMESPACE" rollout status deployment "$ROTATOR_DEPLOYMENT" --timeout=3m >/dev/null
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
		fail "controller $description mutation failed without the exact write-guard denial"
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
	[ -s "$WORK_DIR/predecessor-apply-job-after.json" ] ||
		fail "predecessor Apply cleanup evidence is unavailable for the controller-object proof"
	printf '%s\n' "$PROOF_CONTROLLER_IMAGE" |
		grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' ||
		fail "controller-object proof lacks an exact candidate controller image"
	jq \
		--arg controller_image "$PROOF_CONTROLLER_IMAGE" '
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
      .metadata.name = "ptah-apply-vap-probe-0123456789abcdef" |
      .metadata.annotations["operator.ptah.dev/controller-image"] = $controller_image |
      .metadata.annotations["operator.ptah.dev/controller-revision"] = "e2e-controller-object-guard" |
      .metadata.annotations["operator.ptah.dev/controller-state-version"] = "1" |
      del(
        .spec.template.metadata.labels["batch.kubernetes.io/controller-uid"],
        .spec.template.metadata.labels["batch.kubernetes.io/job-name"],
        .spec.template.metadata.labels["controller-uid"],
        .spec.template.metadata.labels["job-name"]
      ) |
      .spec.template.metadata.annotations = .metadata.annotations
    ' "$WORK_DIR/predecessor-apply-job-after.json" >"$base_manifest"

	baseline_stdout=$WORK_DIR/controller-object-baseline.out
	baseline_stderr=$WORK_DIR/controller-object-baseline.err
	if controller_kube create --dry-run=server -o json -f "$base_manifest" \
		>"$baseline_stdout" 2>"$baseline_stderr"; then
		fail "controller-object baseline bypassed the semantic Job write boundary"
	fi
	if grep -F 'Ptah controller Job write guard rejected an unsafe workload shape' \
		"$baseline_stdout" "$baseline_stderr" >/dev/null; then
		fail "controller-object baseline does not satisfy the structural VAP contract"
	fi
	grep -F 'Job does not match a not-yet-created active operation' \
		"$baseline_stdout" "$baseline_stderr" >/dev/null || {
		cat "$baseline_stderr" >&2
		fail "controller-object baseline did not reach the semantic Job write boundary"
	}

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
    operator.ptah.dev/plan: ptah-plan-111111111111111111111111
    operator.ptah.dev/schema: $PROOF_SCHEMA
  ownerReferences:
    - apiVersion: operator.ptah.dev/v1alpha1
      kind: PtahSchemaPlan
      name: ptah-plan-111111111111111111111111
      uid: 11111111-1111-1111-1111-111111111111
      controller: true
      blockOwnerDeletion: true
immutable: true
binaryData:
  chunk: cHJvYmU=
EOF
	if controller_kube create --dry-run=server -f "$manifest" >/dev/null 2>"$error_file"; then
		fail "controller direct-write webhook accepted a structurally valid chunk without a persisted plan"
	fi
	grep -F 'directly read plan manifest' "$error_file" >/dev/null ||
		fail "controller direct-write probe did not reach the uncached semantic webhook boundary"
}

prove_controller_write_guard() {
	printf '%s\n' 'e2e crd: proving the controller desired-state write boundary'
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

	controller_write_evidence "$WORK_DIR/controller-write-before.json"
	prove_controller_direct_write_webhook
	prove_controller_object_supported_window_guard
	stop_controller_deployment

	current_suspend=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o json |
		jq -er '.spec.suspend // false')
	suspend_patch=$(jq -cn --argjson current "$current_suspend" '{spec: {suspend: ($current | not)}}')
	expect_controller_write_denial spec merge "$suspend_patch"
	expect_controller_write_denial labels merge \
		'{"metadata":{"labels":{"operator.ptah.dev/controller-write-probe":"forbidden"}}}'
	expect_controller_write_denial annotations merge \
		'{"metadata":{"annotations":{"operator.ptah.dev/controller-write-probe":"forbidden"}}}'

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
		'{"metadata":{"finalizers":["operator.ptah.dev/foreign-operation"]}}'

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
		jq -e 'index("operator.ptah.dev/active-operation") == null' >/dev/null ||
		fail "proof PtahSchema already has the active-operation finalizer"
	add_finalizer_patch=$(printf '%s\n' "$current_finalizers" | jq -c '{
      metadata: {finalizers: (. + ["operator.ptah.dev/active-operation"])}
    }')
	controller_kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" \
		--type merge -p "$add_finalizer_patch" >/dev/null
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o json |
		jq -e --argjson before "$current_finalizers" '
          (.metadata.finalizers // []) == ($before + ["operator.ptah.dev/active-operation"])
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
	CONTROLLER_IMPERSONATION_USERNAME=
	CONTROLLER_IMPERSONATION_UID=
	CONTROLLER_IMPERSONATION_POD_NAME=
	CONTROLLER_IMPERSONATION_POD_UID=
	printf '%s\n' 'e2e crd: controller desired-state and direct-write boundaries passed'
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

	printf '%s\n' 'e2e crd: proving runtime rejection of mismatched ownership'
	stop_runtime_deployments
	kube annotate mutatingwebhookconfiguration ptah-operator-admission \
		operator.ptah.dev/release-name=foreign-release --overwrite >/dev/null
	start_runtime_deployments
	assert_runtime_blocked "mismatched admission singleton"
	kube annotate mutatingwebhookconfiguration ptah-operator-admission \
		"operator.ptah.dev/release-name=$E2E_HELM_RELEASE" --overwrite >/dev/null
	kube -n "$E2E_OPERATOR_NAMESPACE" delete pod \
		-l "app.kubernetes.io/instance=$E2E_HELM_RELEASE" --wait=false >/dev/null
	wait_runtime_ready

	printf '%s\n' 'e2e crd: proving runtime rejection of drifted admission behavior'
	webhook_service=$(kube get validatingwebhookconfiguration ptah-operator-admission \
		-o jsonpath='{.webhooks[0].clientConfig.service.name}')
	[ -n "$webhook_service" ] || fail "admission webhook Service name is empty"
	first_validating_name=$(kube get validatingwebhookconfiguration ptah-operator-admission \
		-o jsonpath='{.webhooks[0].name}')
	[ "$first_validating_name" = vapproval.operator.ptah.dev ] ||
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
	[ "$stored_version" = 1 ] ||
		fail "proof PtahSchema controller state version is $stored_version, expected 1"
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" --subresource=status \
		--type=json -p='[{"op":"replace","path":"/status/executionBinding/controllerStateVersion","value":2}]' >/dev/null
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o json |
		jq -S '.status' >"$WORK_DIR/future-controller-state.json"
	start_controller_deployment
	assert_controller_downgrade_blocked
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o json |
		jq -S '.status' >"$WORK_DIR/future-controller-state-after.json"
	cmp "$WORK_DIR/future-controller-state.json" "$WORK_DIR/future-controller-state-after.json" ||
		fail "blocked candidate manager rewrote future PtahSchema state"
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" --subresource=status \
		--type=json -p='[{"op":"replace","path":"/status/executionBinding/controllerStateVersion","value":1}]' >/dev/null
	kube -n "$E2E_OPERATOR_NAMESPACE" delete pod \
		-l 'app.kubernetes.io/component=controller' --wait=false >/dev/null
	wait_runtime_ready
}

create_predecessor_live_objects() {
	kube create namespace "$PROOF_NAMESPACE" >/dev/null
	for schema_name in "$PREDECESSOR_SCHEMA" "$PREDECESSOR_DELETING_SCHEMA"; do
		kube -n "$PROOF_NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: operator.ptah.dev/v1alpha1
kind: PtahSchema
metadata:
  name: $schema_name
spec:
  suspend: true
  target:
    engine: PostgreSQL
    coordinationKey: $schema_name
    urlFrom:
      name: unused-database-url
      key: url
  desired:
    ociRef: oci://example.invalid/schema:v1
    verificationPolicyFrom:
      name: unused-verification-policy
      key: policy.yaml
EOF
		wait_for_suspended "$schema_name"
	done

	kube -n "$PROOF_NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: operator.ptah.dev/v1alpha1
kind: PtahSchema
metadata:
  name: $PREDECESSOR_JOB_SCHEMA
spec:
  target:
    engine: PostgreSQL
    coordinationKey: $PREDECESSOR_JOB_SCHEMA
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
      operator.ptah.dev/predecessor-job-proof: blocked
EOF
	wait_for_predecessor_read_only_job

	predecessor_schema_uid=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PREDECESSOR_SCHEMA" \
		-o jsonpath='{.metadata.uid}')
	kube -n "$PROOF_NAMESPACE" create -f - >/dev/null <<EOF
apiVersion: operator.ptah.dev/v1alpha1
kind: PtahSchemaPlan
metadata:
  name: $PREDECESSOR_PLAN
spec:
  contractVersion: 2
  schemaRef: {name: $PREDECESSOR_SCHEMA, uid: $predecessor_schema_uid}
  fingerprint: predecessor-plan-fingerprint
  contentDigest: sha256:2222222222222222222222222222222222222222222222222222222222222222
  size: 1
  artifactDigest: sha256:3333333333333333333333333333333333333333333333333333333333333333
  coordinationDigest: sha256:4444444444444444444444444444444444444444444444444444444444444444
  targetIdentityDigest: sha256:5555555555555555555555555555555555555555555555555555555555555555
  actualStateFingerprint: predecessor-actual
  desiredStateFingerprint: predecessor-desired
  policyFingerprint: predecessor-policy
  verificationPolicyUID: predecessor-policy-uid
  verificationPolicyDigest: sha256:6666666666666666666666666666666666666666666666666666666666666666
  executionBindingID: v1-11111111111111111111111111111111
  ptahVersion: predecessor-e2e
  executorImage: e2e.invalid/executor@sha256:7777777777777777777777777777777777777777777777777777777777777777
  runnerImage: e2e.invalid/runner@sha256:8888888888888888888888888888888888888888888888888888888888888888
  runnerProtocolVersion: 4
  dialect: postgresql
  destructive: false
  statementCount: 1
  chunks:
    - name: predecessor-plan-chunk
      key: plan.sql
      index: 0
      digest: sha256:9999999999999999999999999999999999999999999999999999999999999999
      size: 1
EOF
	kube -n "$PROOF_NAMESPACE" patch ptahschemaplan "$PREDECESSOR_PLAN" --subresource=status \
		--type=merge -p '{"status":{"observedGeneration":1,"conditions":[{"type":"Ready","status":"True","reason":"PredecessorProof","message":"predecessor evidence","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}' >/dev/null
	prepare_predecessor_apply_fixture
}

create_predecessor_approval_while_webhooks_stopped() {
	predecessor_schema_uid=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PREDECESSOR_SCHEMA" \
		-o jsonpath='{.metadata.uid}')
	predecessor_plan_uid=$(kube -n "$PROOF_NAMESPACE" get ptahschemaplan "$PREDECESSOR_PLAN" \
		-o jsonpath='{.metadata.uid}')
	kube -n "$PROOF_NAMESPACE" create -f - >/dev/null <<EOF
apiVersion: operator.ptah.dev/v1alpha1
kind: PtahSchemaApproval
metadata:
  name: $PREDECESSOR_APPROVAL
spec:
  schemaRef: {name: $PREDECESSOR_SCHEMA, uid: $predecessor_schema_uid}
  planRef: {name: $PREDECESSOR_PLAN, uid: $predecessor_plan_uid}
  planFingerprint: predecessor-plan-fingerprint
  artifactDigest: sha256:3333333333333333333333333333333333333333333333333333333333333333
  coordinationDigest: sha256:4444444444444444444444444444444444444444444444444444444444444444
  targetIdentityDigest: sha256:5555555555555555555555555555555555555555555555555555555555555555
  actualStateFingerprint: predecessor-actual
  desiredStateFingerprint: predecessor-desired
  policyFingerprint: predecessor-policy
  verificationPolicyUID: predecessor-policy-uid
  verificationPolicyDigest: sha256:6666666666666666666666666666666666666666666666666666666666666666
  executionBindingID: v1-11111111111111111111111111111111
  ptahVersion: predecessor-e2e
  executorImage: e2e.invalid/executor@sha256:7777777777777777777777777777777777777777777777777777777777777777
  runnerImage: e2e.invalid/runner@sha256:8888888888888888888888888888888888888888888888888888888888888888
  runnerProtocolVersion: 4
  approver: {username: predecessor-proof}
  approvedAt: "2026-01-01T00:00:00Z"
  mutationRequestUID: predecessor-proof
EOF
	kube -n "$PROOF_NAMESPACE" patch ptahschemaapproval "$PREDECESSOR_APPROVAL" --subresource=status \
		--type=merge -p '{"status":{"observedGeneration":1,"conditions":[{"type":"Accepted","status":"True","reason":"PredecessorProof","message":"predecessor evidence","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}' >/dev/null
}

stage_predecessor_deletion() {
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PREDECESSOR_DELETING_SCHEMA" --type=merge \
		-p '{"metadata":{"finalizers":["operator.ptah.dev/active-operation"]}}' >/dev/null
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PREDECESSOR_DELETING_SCHEMA" --subresource=status \
		--type=merge -p '{"status":{"pendingLockRelease":{"coordinationDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operationID":"predecessor-lock-release","leaseDurationSeconds":30,"leaseEpoch":"v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}' >/dev/null
	kube -n "$PROOF_NAMESPACE" delete ptahschema "$PREDECESSOR_DELETING_SCHEMA" --wait=false >/dev/null
	kube -n "$PROOF_NAMESPACE" get ptahschema "$PREDECESSOR_DELETING_SCHEMA" -o json | jq -e '
      (.metadata.deletionTimestamp != null) and
      (.metadata.finalizers | index("operator.ptah.dev/active-operation") != null) and
      (.status.pendingLockRelease.operationID == "predecessor-lock-release")
    ' >/dev/null || fail "predecessor deleting PtahSchema did not retain its durable lock-release work"
}

assert_predecessor_crds() {
	predecessor_crd_count=$(jq -er '.crds | length' "$E2E_PREDECESSOR_IDENTITY_FILE")
	[ "$predecessor_crd_count" -eq 3 ] || fail "predecessor identity must contain exactly three CRDs"
	predecessor_crd_index=0
	while [ "$predecessor_crd_index" -lt "$predecessor_crd_count" ]; do
		crd_name=$(jq -er ".crds[$predecessor_crd_index].name" "$E2E_PREDECESSOR_IDENTITY_FILE")
		want_digest=$(jq -er ".crds[$predecessor_crd_index].normalizedSpecDigest" \
			"$E2E_PREDECESSOR_IDENTITY_FILE")
		kube get crd "$crd_name" -o json | jq -e '
          ((.metadata.annotations // {}) | has("operator.ptah.dev/crd-schema-version") | not) and
          ((.metadata.annotations // {}) | has("operator.ptah.dev/crd-schema-digest") | not) and
          ((.metadata.annotations // {}) | has("operator.ptah.dev/controller-state-version") | not)
        ' >/dev/null || fail "predecessor CRD $crd_name is not annotation-free"
		got_digest=$(crd_normalized_digest "$crd_name")
		[ "$got_digest" = "$want_digest" ] ||
			fail "predecessor CRD $crd_name digest is $got_digest, expected $want_digest"
		kube get crd "$crd_name" -o jsonpath='{.metadata.uid}' \
			>"$WORK_DIR/${crd_name}-predecessor-uid"
		predecessor_crd_index=$((predecessor_crd_index + 1))
	done
}

assert_candidate_crds_adopted() {
	predecessor_crd_count=$(jq -er '.crds | length' "$E2E_PREDECESSOR_IDENTITY_FILE")
	predecessor_crd_index=0
	while [ "$predecessor_crd_index" -lt "$predecessor_crd_count" ]; do
		crd_name=$(jq -er ".crds[$predecessor_crd_index].name" "$E2E_PREDECESSOR_IDENTITY_FILE")
		crd_path=$(jq -er ".crds[$predecessor_crd_index].path" "$E2E_PREDECESSOR_IDENTITY_FILE")
		before_uid=$(cat "$WORK_DIR/${crd_name}-predecessor-uid")
		after_uid=$(kube get crd "$crd_name" -o jsonpath='{.metadata.uid}')
		[ "$after_uid" = "$before_uid" ] || fail "candidate upgrade recreated CRD $crd_name"
		candidate_digest=$(go -C "$ROOT_DIR" run ./hack/crdschemadigest "$ROOT_DIR/$crd_path")
		live_digest=$(crd_normalized_digest "$crd_name")
		[ "$live_digest" = "$candidate_digest" ] ||
			fail "adopted CRD $crd_name digest is $live_digest, expected candidate $candidate_digest"
		kube get crd "$crd_name" -o json | jq -e \
			--arg digest "$candidate_digest" \
			--arg schema_version "$CANDIDATE_CRD_SCHEMA_VERSION" '
		  .metadata.annotations["operator.ptah.dev/crd-schema-version"] == $schema_version and
          .metadata.annotations["operator.ptah.dev/crd-schema-digest"] == $digest and
          .metadata.annotations["operator.ptah.dev/controller-state-version"] == "1"
        ' >/dev/null || fail "candidate CRD $crd_name did not acquire the exact schema and controller-state identity"
		predecessor_crd_index=$((predecessor_crd_index + 1))
	done
}

run_predecessor_upgrade_proof() {
	E2E_CANDIDATE_VALUES_FILE=${E2E_CANDIDATE_VALUES_FILE:?E2E_CANDIDATE_VALUES_FILE is required for predecessor upgrade proof}
	E2E_PREDECESSOR_IDENTITY_FILE=${E2E_PREDECESSOR_IDENTITY_FILE:?E2E_PREDECESSOR_IDENTITY_FILE is required for predecessor upgrade proof}
	E2E_PREDECESSOR_SOURCE_DIR=${E2E_PREDECESSOR_SOURCE_DIR:?E2E_PREDECESSOR_SOURCE_DIR is required for predecessor upgrade proof}
	E2E_PREDECESSOR_IMAGE=${E2E_PREDECESSOR_IMAGE:?E2E_PREDECESSOR_IMAGE is required for predecessor upgrade proof}
	E2E_CANDIDATE_IMAGE=${E2E_CANDIDATE_IMAGE:?E2E_CANDIDATE_IMAGE is required for predecessor upgrade proof}
	[ -f "$E2E_CANDIDATE_VALUES_FILE" ] || fail "candidate values file is missing"
	[ -f "$E2E_PREDECESSOR_IDENTITY_FILE" ] || fail "predecessor identity file is missing"
	[ -d "$E2E_PREDECESSOR_SOURCE_DIR" ] || fail "predecessor source archive is missing"
	UPGRADE_VALUES_FILE=$E2E_CANDIDATE_VALUES_FILE
	prepare_expected_singleton_annotations

	printf '%s\n' 'e2e crd: proving exact predecessor-to-candidate upgrade'
	runtime_deployment_names
	old_deployment_image=$(kube -n "$E2E_OPERATOR_NAMESPACE" get deployment "$CONTROLLER_DEPLOYMENT" \
		-o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}')
	[ "$old_deployment_image" = "$E2E_PREDECESSOR_IMAGE" ] ||
		fail "installed predecessor manager image is $old_deployment_image, expected $E2E_PREDECESSOR_IMAGE"
	assert_predecessor_crds
	assert_singleton_annotation_free
	create_predecessor_live_objects
	singleton_contract_evidence mutatingwebhookconfiguration "$WORK_DIR/predecessor-mutating.json"
	singleton_contract_evidence validatingwebhookconfiguration "$WORK_DIR/predecessor-validating.json"

	printf '%s\n' 'e2e crd: proving unknown annotation-free admission behavior is not adopted'
	kube patch validatingwebhookconfiguration ptah-operator-admission --type=json \
		-p='[{"op":"replace","path":"/webhooks/0/failurePolicy","value":"Ignore"}]' >/dev/null
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		crd_evidence "$crd_name" "$WORK_DIR/${crd_name}-before-unknown-singleton.json"
	done
	expect_upgrade_failure_without_deployment_change \
		"upgrade with unknown annotation-free admission behavior"
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-unknown-singleton.json"
	done
	assert_singleton_annotation_free
	assert_predecessor_certificate_update_allowed

	stop_runtime_deployments
	stage_predecessor_read_only_job_completion
	stage_predecessor_read_only_job_uid_gap
	stage_predecessor_deletion

	kube patch mutatingwebhookconfiguration ptah-operator-admission --type=json \
		-p='[{"op":"replace","path":"/webhooks/0/failurePolicy","value":"Ignore"}]' >/dev/null
	create_predecessor_approval_while_webhooks_stopped
	kube patch mutatingwebhookconfiguration ptah-operator-admission --type=json \
		-p='[{"op":"replace","path":"/webhooks/0/failurePolicy","value":"Fail"}]' >/dev/null
	kube patch validatingwebhookconfiguration ptah-operator-admission --type=json \
		-p='[{"op":"replace","path":"/webhooks/0/failurePolicy","value":"Fail"}]' >/dev/null
	singleton_contract_evidence mutatingwebhookconfiguration "$WORK_DIR/restored-mutating.json"
	singleton_contract_evidence validatingwebhookconfiguration "$WORK_DIR/restored-validating.json"
	cmp "$WORK_DIR/predecessor-mutating.json" "$WORK_DIR/restored-mutating.json" ||
		fail "predecessor MutatingWebhookConfiguration was not restored exactly"
	cmp "$WORK_DIR/predecessor-validating.json" "$WORK_DIR/restored-validating.json" ||
		fail "predecessor ValidatingWebhookConfiguration was not restored exactly"

	schema_identity_evidence "$PREDECESSOR_SCHEMA" "$WORK_DIR/predecessor-schema-before.json"
	object_evidence ptahschemaplan "$PREDECESSOR_PLAN" "$WORK_DIR/predecessor-plan-before.json"
	object_evidence ptahschemaapproval "$PREDECESSOR_APPROVAL" "$WORK_DIR/predecessor-approval-before.json"

	printf '%s\n' 'e2e crd: proving unknown annotation-free CRD drift is refused before controller rollout'
	drift_crd=ptahschemaplans.operator.ptah.dev
	kube patch crd "$drift_crd" --type=json \
		-p='[{"op":"add","path":"/spec/versions/0/schema/openAPIV3Schema/description","value":"unknown predecessor drift"}]' >/dev/null
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		crd_evidence "$crd_name" "$WORK_DIR/${crd_name}-before-unknown-predecessor.json"
	done
	expect_upgrade_failure_without_deployment_change \
		"upgrade with unknown annotation-free CRD drift"
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-unknown-predecessor.json"
	done
	assert_singleton_annotation_free
	restore_predecessor_crd "$drift_crd"

	printf '%s\n' 'e2e crd: starting a predecessor Apply across the candidate upgrade'
	start_runtime_deployments
	wait_runtime_ready
	start_predecessor_apply_barrier
	start_predecessor_apply_fixture
	wait_for_predecessor_apply_barrier_contention
	stop_runtime_deployments
	stage_predecessor_apply_job_uid_gap_while_running

	printf '%s\n' 'e2e crd: upgrading while the exact predecessor Apply is still running'
	helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$E2E_CANDIDATE_VALUES_FILE" \
		--wait --timeout 5m >/dev/null
	wait_runtime_ready
	assert_predecessor_apply_remains_exclusive_while_running
	assert_predecessor_apply_barrier_contended
	release_predecessor_apply_barrier
	wait_for_predecessor_apply_job_terminal
	wait_for_predecessor_read_only_job_cleanup
	wait_for_predecessor_apply_job_cleanup
	new_deployment_image=$(kube -n "$E2E_OPERATOR_NAMESPACE" get deployment "$CONTROLLER_DEPLOYMENT" \
		-o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}')
	[ "$new_deployment_image" = "$E2E_CANDIDATE_IMAGE" ] ||
		fail "upgraded manager image is $new_deployment_image, expected $E2E_CANDIDATE_IMAGE"
	assert_candidate_crds_adopted
	assert_adopted_singleton_annotations
	schema_identity_evidence "$PREDECESSOR_SCHEMA" "$WORK_DIR/predecessor-schema-after.json"
	cmp "$WORK_DIR/predecessor-schema-before.json" "$WORK_DIR/predecessor-schema-after.json" ||
		fail "predecessor PtahSchema UID or spec changed during candidate upgrade"
	assert_object_unchanged ptahschemaplan "$PREDECESSOR_PLAN" "$WORK_DIR/predecessor-plan-before.json"
	assert_object_unchanged ptahschemaapproval "$PREDECESSOR_APPROVAL" "$WORK_DIR/predecessor-approval-before.json"
	wait_for_schema_deleted "$PREDECESSOR_DELETING_SCHEMA"
	printf '%s\n' 'e2e crd: exact predecessor upgrade proof passed'
}

create_proof_objects() {
	kube get namespace "$PROOF_NAMESPACE" >/dev/null
	kube -n "$PROOF_NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: operator.ptah.dev/v1alpha1
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

	stop_runtime_deployments

	kube delete mutatingwebhookconfiguration ptah-operator-admission >/dev/null
	kube delete validatingwebhookconfiguration ptah-operator-admission >/dev/null
	schema_uid=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" -o jsonpath='{.metadata.uid}')

	kube -n "$PROOF_NAMESPACE" create -f - >/dev/null <<EOF
apiVersion: operator.ptah.dev/v1alpha1
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
apiVersion: operator.ptah.dev/v1alpha1
kind: PtahSchemaApproval
metadata:
  name: $PROOF_APPROVAL
spec:
  schemaRef: {name: $PROOF_SCHEMA, uid: $schema_uid}
  planRef: {name: $PROOF_PLAN, uid: $plan_uid}
  planFingerprint: plan-fingerprint
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
  approver: {username: crd-upgrade-proof}
  approvedAt: "2026-01-01T00:00:00Z"
  mutationRequestUID: crd-upgrade-proof
EOF
	kube -n "$PROOF_NAMESPACE" patch ptahschemaapproval "$PROOF_APPROVAL" --subresource=status \
		--type=merge -p '{"status":{"observedGeneration":1,"conditions":[{"type":"Accepted","status":"True","reason":"UpgradeProof","message":"proof status","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}' >/dev/null
}

run_upgrade_proof() {
	run_predecessor_upgrade_proof
	helm_e2e get values "$E2E_HELM_RELEASE" -n "$E2E_OPERATOR_NAMESPACE" -o yaml >"$WORK_DIR/release-values.yaml"
	UPGRADE_VALUES_FILE=$WORK_DIR/release-values.yaml
	PROOF_CONTROLLER_IMAGE=$(helm_e2e get values "$E2E_HELM_RELEASE" \
		-n "$E2E_OPERATOR_NAMESPACE" -o json | jq -er '
          .image |
          select(.repository != null and .testIdentityDigest != null) |
          .repository + "@" + .testIdentityDigest
        ')
	printf '%s\n' "$PROOF_CONTROLLER_IMAGE" |
		grep -Eq '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' ||
		fail "release values do not identify the exact controller image content"

	printf '%s\n' 'e2e crd: proving a missing CRD aborts Helm upgrade without recreation'
	kube delete crd ptahschemaapprovals.operator.ptah.dev >/dev/null
	expect_upgrade_failure_without_deployment_change "upgrade with a missing CRD"
	if kube get crd ptahschemaapprovals.operator.ptah.dev >/dev/null 2>&1; then
		fail "CRD hook recreated a missing CRD"
	fi
	kube create -f "$ROOT_DIR/config/crd/bases/operator.ptah.dev_ptahschemaapprovals.yaml" >/dev/null
	kube wait --for=condition=Established crd/ptahschemaapprovals.operator.ptah.dev --timeout=60s >/dev/null

	printf '%s\n' 'e2e crd: proving a newer CRD schema version blocks rollback'
	future_schema_version=$((CANDIDATE_CRD_SCHEMA_VERSION + 1))
	kube annotate crd ptahschemaplans.operator.ptah.dev \
		"operator.ptah.dev/crd-schema-version=$future_schema_version" --overwrite >/dev/null
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		crd_evidence "$crd_name" "$WORK_DIR/${crd_name}-before-schema-rollback.json"
	done
	expect_upgrade_failure_without_deployment_change "upgrade with a newer CRD schema version"
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-schema-rollback.json"
	done
	kube annotate crd ptahschemaplans.operator.ptah.dev \
		"operator.ptah.dev/crd-schema-version=$CANDIDATE_CRD_SCHEMA_VERSION" --overwrite >/dev/null

	printf '%s\n' 'e2e crd: proving a newer durable controller-state marker blocks rollback'
	kube annotate crd ptahschemaplans.operator.ptah.dev \
		operator.ptah.dev/controller-state-version=2 --overwrite >/dev/null
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		crd_evidence "$crd_name" "$WORK_DIR/${crd_name}-before-state-rollback.json"
	done
	expect_upgrade_failure_without_deployment_change "upgrade with a newer durable controller-state marker"
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-state-rollback.json"
	done
	kube annotate crd ptahschemaplans.operator.ptah.dev \
		operator.ptah.dev/controller-state-version=1 --overwrite >/dev/null

	printf '%s\n' 'e2e crd: proving schema digest adoption and collision rejection'
	digest_crd=ptahschemaplans.operator.ptah.dev
	candidate_schema_digest=$(kube get crd "$digest_crd" \
		-o jsonpath='{.metadata.annotations.operator\.ptah\.dev/crd-schema-digest}')
	printf '%s\n' "$candidate_schema_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' ||
		fail "$digest_crd does not carry a valid candidate schema digest"
	kube annotate crd "$digest_crd" operator.ptah.dev/crd-schema-digest- >/dev/null
	helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$WORK_DIR/release-values.yaml" \
		--wait --timeout 5m >/dev/null
	adopted_schema_digest=$(kube get crd "$digest_crd" \
		-o jsonpath='{.metadata.annotations.operator\.ptah\.dev/crd-schema-digest}')
	[ "$adopted_schema_digest" = "$candidate_schema_digest" ] ||
		fail "exact legacy schema did not adopt the candidate digest"

	kube annotate crd "$digest_crd" operator.ptah.dev/crd-schema-digest- >/dev/null
	kube patch crd "$digest_crd" --type=json \
		-p='[{"op":"add","path":"/spec/versions/0/schema/openAPIV3Schema/description","value":"unidentified digestless schema"}]' >/dev/null
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		crd_evidence "$crd_name" "$WORK_DIR/${crd_name}-before-missing-digest.json"
	done
	expect_upgrade_failure_without_deployment_change "upgrade with digestless schema drift"
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-missing-digest.json"
	done
	kube annotate crd "$digest_crd" \
		"operator.ptah.dev/crd-schema-digest=$candidate_schema_digest" --overwrite >/dev/null
	helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$WORK_DIR/release-values.yaml" \
		--wait --timeout 5m >/dev/null

	collision_digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
	if [ "$collision_digest" = "$candidate_schema_digest" ]; then
		collision_digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
	fi
	kube annotate crd "$digest_crd" \
		"operator.ptah.dev/crd-schema-digest=$collision_digest" --overwrite >/dev/null
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		crd_evidence "$crd_name" "$WORK_DIR/${crd_name}-before-digest-collision.json"
	done
	expect_upgrade_failure_without_deployment_change "upgrade with a same-version schema digest collision"
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-digest-collision.json"
	done
	kube annotate crd "$digest_crd" \
		"operator.ptah.dev/crd-schema-digest=$candidate_schema_digest" --overwrite >/dev/null

	printf '%s\n' 'e2e crd: creating live-object preservation evidence'
	create_proof_objects
	object_evidence ptahschema "$PROOF_SCHEMA" "$WORK_DIR/ptahschema-before.json"
	object_evidence ptahschemaplan "$PROOF_PLAN" "$WORK_DIR/ptahschemaplan-before.json"
	object_evidence ptahschemaapproval "$PROOF_APPROVAL" "$WORK_DIR/ptahschemaapproval-before.json"

	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		kube patch crd "$crd_name" --type=json \
			-p='[{"op":"add","path":"/spec/versions/0/schema/openAPIV3Schema/description","value":"outdated e2e schema"}]' >/dev/null
		crd_evidence "$crd_name" "$WORK_DIR/${crd_name}-before-future-state.json"
	done
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" --subresource=status \
		--type=json -p='[{"op":"replace","path":"/status/executionBinding/controllerStateVersion","value":2}]' >/dev/null
	expect_upgrade_failure_without_deployment_change "upgrade against future controller state"
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-future-state.json"
	done
	stored_version=$(kube -n "$PROOF_NAMESPACE" get ptahschema "$PROOF_SCHEMA" \
		-o jsonpath='{.status.executionBinding.controllerStateVersion}')
	[ "$stored_version" = 2 ] || fail "failed CRD preflight rewrote future controller state"
	kube -n "$PROOF_NAMESPACE" patch ptahschema "$PROOF_SCHEMA" --subresource=status \
		--type=json -p='[{"op":"replace","path":"/status/executionBinding/controllerStateVersion","value":1}]' >/dev/null

	printf '%s\n' 'e2e crd: upgrading drifted CRDs before the manager rollout'
	helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$WORK_DIR/release-values.yaml" \
		--wait --timeout 5m >/dev/null
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		description=$(kube get crd "$crd_name" -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.description}')
		[ "$description" != "outdated e2e schema" ] || fail "$crd_name retained the outdated schema"
	done
	assert_object_unchanged ptahschema "$PROOF_SCHEMA" "$WORK_DIR/ptahschema-before.json"
	assert_object_unchanged ptahschemaplan "$PROOF_PLAN" "$WORK_DIR/ptahschemaplan-before.json"
	assert_object_unchanged ptahschemaapproval "$PROOF_APPROVAL" "$WORK_DIR/ptahschemaapproval-before.json"
	prove_controller_write_guard

	printf '%s\n' 'e2e crd: proving immutable singleton coordination'
	second_release=${E2E_HELM_RELEASE}-second
	if helm_e2e install "$second_release" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$WORK_DIR/release-values.yaml" \
		--wait --timeout 2m >"$WORK_DIR/second-release.out" 2>"$WORK_DIR/second-release.err"; then
		fail "a second operator release was installed"
	fi
	grep -F 'fixed admission singleton' "$WORK_DIR/second-release.err" >/dev/null ||
		fail "second release failed without the singleton ownership guard"
	if helm_e2e status "$second_release" -n "$E2E_OPERATOR_NAMESPACE" >/dev/null 2>&1; then
		fail "failed second release was recorded"
	fi

		expect_upgrade_render_failure_without_deployment_change \
			"coordination namespace mutation" --set-string coordination.namespace=forbidden-coordination
	grep -F 'operator.ptah.dev/coordination-namespace' "$WORK_DIR/failed-upgrade.err" >/dev/null ||
		fail "coordination mutation failed without the immutable annotation guard"
		expect_upgrade_render_failure_without_deployment_change \
		"leader-election mutation" --set replicaCount=1 --set leaderElection=false
	grep -F 'operator.ptah.dev/leader-election' "$WORK_DIR/failed-upgrade.err" >/dev/null ||
		fail "leader-election mutation failed without the immutable annotation guard"

	prove_runtime_singleton_guard
	prove_controller_downgrade_guard
	helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$WORK_DIR/release-values.yaml" \
		--wait --timeout 5m >/dev/null
	printf '%s\n' 'e2e crd: upgrade and singleton proofs passed'
}

run_uninstall_proof() {
	helm_e2e get values "$E2E_HELM_RELEASE" -n "$E2E_OPERATOR_NAMESPACE" -o yaml >"$WORK_DIR/release-values.yaml"
	for resource in ptahschema ptahschemaplan ptahschemaapproval; do
		object_evidence "$resource" "$PROOF_SCHEMA" "$WORK_DIR/${resource}-before.json"
	done

	printf '%s\n' 'e2e crd: proving foreign controller RBAC blocks uninstall before quiescence'
	runtime_deployment_evidence >"$WORK_DIR/runtime-before-blocked-uninstall.json"
	controller_service_account=$(kube -n "$E2E_OPERATOR_NAMESPACE" get deployment "$CONTROLLER_DEPLOYMENT" \
		-o jsonpath='{.spec.template.spec.serviceAccountName}')
	[ -n "$controller_service_account" ] || fail "controller ServiceAccount is missing from its Deployment"
	FOREIGN_TEARDOWN_BINDING=ptah-e2e-foreign-controller-binding
	kube create clusterrolebinding "$FOREIGN_TEARDOWN_BINDING" \
		--clusterrole=view \
		--serviceaccount="$E2E_OPERATOR_NAMESPACE:$controller_service_account" >/dev/null
	if helm_e2e uninstall "$E2E_HELM_RELEASE" -n "$E2E_OPERATOR_NAMESPACE" \
		--wait --timeout 2m >"$WORK_DIR/blocked-uninstall.out" 2>"$WORK_DIR/blocked-uninstall.err"; then
		fail "uninstall with a foreign controller binding unexpectedly succeeded"
	fi
	if ! grep -F "foreign ClusterRoleBinding/$FOREIGN_TEARDOWN_BINDING" \
		"$WORK_DIR/blocked-uninstall.err" >/dev/null &&
		! grep -F "foreign ClusterRoleBinding/$FOREIGN_TEARDOWN_BINDING" \
			"$WORK_DIR/blocked-uninstall.out" >/dev/null; then
		fail "blocked uninstall did not report the foreign controller binding"
	fi
	runtime_deployment_evidence >"$WORK_DIR/runtime-after-blocked-uninstall.json"
	cmp "$WORK_DIR/runtime-before-blocked-uninstall.json" \
		"$WORK_DIR/runtime-after-blocked-uninstall.json" ||
		fail "blocked uninstall mutated a runtime Deployment before privilege preflight"
	kube delete clusterrolebinding "$FOREIGN_TEARDOWN_BINDING" --wait=true >/dev/null
	FOREIGN_TEARDOWN_BINDING=

	helm_e2e uninstall "$E2E_HELM_RELEASE" -n "$E2E_OPERATOR_NAMESPACE" \
		--wait --timeout 5m >/dev/null
	assert_release_runtime_removed
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		kube get crd "$crd_name" >/dev/null
	done
	for resource in ptahschema ptahschemaplan ptahschemaapproval; do
		assert_object_unchanged "$resource" "$PROOF_SCHEMA" "$WORK_DIR/${resource}-before.json"
	done

	printf '%s\n' 'e2e crd: reinstalling over retained and drifted CRDs'
	kube patch crd ptahschemas.operator.ptah.dev --type=json \
		-p='[{"op":"add","path":"/spec/versions/0/schema/openAPIV3Schema/description","value":"retained reinstall drift"}]' >/dev/null
	helm_e2e install "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$WORK_DIR/release-values.yaml" \
		--wait --timeout 5m >/dev/null
	description=$(kube get crd ptahschemas.operator.ptah.dev \
		-o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.description}')
	[ "$description" != "retained reinstall drift" ] ||
		fail "pre-install hook did not reconcile a retained CRD"
	for resource in ptahschema ptahschemaplan ptahschemaapproval; do
		assert_object_unchanged "$resource" "$PROOF_SCHEMA" "$WORK_DIR/${resource}-before.json"
	done
	helm_e2e uninstall "$E2E_HELM_RELEASE" -n "$E2E_OPERATOR_NAMESPACE" \
		--wait --timeout 5m >/dev/null
	assert_release_runtime_removed
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		kube get crd "$crd_name" >/dev/null
	done
	for resource in ptahschema ptahschemaplan ptahschemaapproval; do
		assert_object_unchanged "$resource" "$PROOF_SCHEMA" "$WORK_DIR/${resource}-before.json"
	done
	printf '%s\n' 'e2e crd: uninstall retained CRDs and live objects'
}

case "$E2E_PHASE" in
	upgrade) run_upgrade_proof ;;
	uninstall) run_uninstall_proof ;;
	*) fail "unsupported E2E_PHASE $E2E_PHASE" ;;
esac

printf 'e2e crd: PASS phase=%s\n' "$E2E_PHASE"
