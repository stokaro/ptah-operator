#!/bin/sh

set -eu

E2E_KUBECONFIG=${E2E_KUBECONFIG:?E2E_KUBECONFIG is required}
E2E_OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:?E2E_OPERATOR_NAMESPACE is required}
E2E_HELM_RELEASE=${E2E_HELM_RELEASE:?E2E_HELM_RELEASE is required}
E2E_CHART_PACKAGE=${E2E_CHART_PACKAGE:?E2E_CHART_PACKAGE is required}
E2E_PHASE=${E2E_PHASE:-upgrade}

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
BLOCKED_STABILITY_SECONDS=10
BLOCKED_FAILURE_TIMEOUT_SECONDS=150
FOREIGN_TEARDOWN_BINDING=
CONTROLLER_IMPERSONATION_USERNAME=
CONTROLLER_IMPERSONATION_UID=
CONTROLLER_IMPERSONATION_POD_NAME=
CONTROLLER_IMPERSONATION_POD_UID=
CONTROLLER_GUARD_OWNER=
CONTROLLER_GUARD_PROBE_INDEX=0

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
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
	expected_hook_suffix=$2
	shift 2
	before=$WORK_DIR/deployment-before.json
	after=$WORK_DIR/deployment-after.json
	[ -n "$UPGRADE_VALUES_FILE" ] || fail "upgrade values file is not configured"
	deployment_evidence >"$before"
	if helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$UPGRADE_VALUES_FILE" \
		--wait --timeout 2m "$@" >"$WORK_DIR/failed-upgrade.out" 2>"$WORK_DIR/failed-upgrade.err"; then
		fail "$description unexpectedly succeeded"
	fi
	if ! grep -F -- "-${expected_hook_suffix} not ready" \
		"$WORK_DIR/failed-upgrade.out" "$WORK_DIR/failed-upgrade.err" >/dev/null; then
		failed_hook=$(sed -n 's/.*resource Job\/[^/]*\/\([^ ]*\) not ready.*/\1/p' \
			"$WORK_DIR/failed-upgrade.out" "$WORK_DIR/failed-upgrade.err" | sed -n '1p')
		fail "$description failed in unexpected hook ${failed_hook:-unknown}; expected *-${expected_hook_suffix}"
	fi
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
	printf '%s\n' 'e2e crd: controller desired-state write boundary proof passed'
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
		kube get crd "$crd_name" -o json | jq -e --arg digest "$candidate_digest" '
          .metadata.annotations["operator.ptah.dev/crd-schema-version"] == "1" and
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

	stop_runtime_deployments
	stage_predecessor_deletion

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
		"upgrade with unknown annotation-free admission behavior" preflight
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-unknown-singleton.json"
	done
	assert_singleton_annotation_free

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
		"upgrade with unknown annotation-free CRD drift" preflight
	for crd_name in \
		ptahschemas.operator.ptah.dev \
		ptahschemaplans.operator.ptah.dev \
		ptahschemaapprovals.operator.ptah.dev; do
		assert_crd_unchanged "$crd_name" "$WORK_DIR/${crd_name}-before-unknown-predecessor.json"
	done
	assert_singleton_annotation_free
	restore_predecessor_crd "$drift_crd"

	printf '%s\n' 'e2e crd: upgrading the exact predecessor CRDs and live objects'
	helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" \
		--namespace "$E2E_OPERATOR_NAMESPACE" --values "$E2E_CANDIDATE_VALUES_FILE" \
		--wait --timeout 5m >/dev/null
	wait_runtime_ready
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
	kube annotate crd ptahschemaplans.operator.ptah.dev \
		operator.ptah.dev/crd-schema-version=2 --overwrite >/dev/null
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
		operator.ptah.dev/crd-schema-version=1 --overwrite >/dev/null

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

	expect_upgrade_failure_without_deployment_change \
		"coordination namespace mutation" --set-string coordination.namespace=forbidden-coordination
	grep -F 'operator.ptah.dev/coordination-namespace' "$WORK_DIR/failed-upgrade.err" >/dev/null ||
		fail "coordination mutation failed without the immutable annotation guard"
	expect_upgrade_failure_without_deployment_change \
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
