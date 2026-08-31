#!/bin/sh

set -eu

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)

KUBECONFIG_FILE=${E2E_KUBECONFIG:-}
OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:-}
HA_TEST_NAMESPACE=${E2E_HA_TEST_NAMESPACE:-}
FOREIGN_NAMESPACE=${E2E_FOREIGN_NAMESPACE:-}
HELM_RELEASE=${E2E_HELM_RELEASE:-}

fail() {
	printf 'e2e HA: %s\n' "$*" >&2
	exit 1
}

for value_name in \
	KUBECONFIG_FILE OPERATOR_NAMESPACE HA_TEST_NAMESPACE FOREIGN_NAMESPACE \
	HELM_RELEASE; do
	eval "value=\${$value_name}"
	[ -n "$value" ] || fail "$value_name is required"
done
[ -f "$KUBECONFIG_FILE" ] || fail "E2E_KUBECONFIG does not name a file"
[ "$HA_TEST_NAMESPACE" != "$OPERATOR_NAMESPACE" ] ||
	fail "HA workload namespace must differ from the operator namespace"
[ "$FOREIGN_NAMESPACE" != "$OPERATOR_NAMESPACE" ] ||
	fail "foreign namespace must differ from the coordination namespace"

k() {
	kubectl --kubeconfig "$KUBECONFIG_FILE" "$@"
}

MANAGER="${HELM_RELEASE}-ptah-operator"
LEADER_LEASE=ptah-operator.operator.ptah.dev
LEADER_TIMEOUT_SECONDS=120
WORKLOAD_TIMEOUT_SECONDS=120
HA_SCHEMA=leader-failover

cleanup() {
	k delete namespace "$HA_TEST_NAMESPACE" --ignore-not-found --wait=false \
		>/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

assert_can_i() {
	can_i_expected=$1
	can_i_namespace=$2
	can_i_verb=$3
	if can_i_result=$(k auth can-i "$can_i_verb" leases.coordination.k8s.io \
		--namespace "$can_i_namespace" \
		--as="system:serviceaccount:${OPERATOR_NAMESPACE}:${MANAGER}"); then
		can_i_status=0
	else
		can_i_status=$?
	fi
	if [ "$can_i_result" = yes ] && [ "$can_i_status" -ne 0 ]; then
		fail "kubectl reported yes with exit status $can_i_status"
	fi
	if [ "$can_i_result" = no ] && [ "$can_i_status" -eq 0 ]; then
		fail "kubectl reported no with a successful exit status"
	fi
	[ "$can_i_result" = "$can_i_expected" ] ||
		fail "$OPERATOR_NAMESPACE/$MANAGER can-i $can_i_verb Lease in $can_i_namespace = $can_i_result, want $can_i_expected"
}

leader_holder() {
	k -n "$OPERATOR_NAMESPACE" get lease "$LEADER_LEASE" \
		-o jsonpath='{.spec.holderIdentity}' 2>/dev/null
}

leader_pod_name() {
	printf '%s\n' "$1" | sed 's/_[^_]*$//'
}

holder_is_ready_manager_pod() {
	holder_identity=$1
	holder_pod=$(leader_pod_name "$holder_identity")
	[ -n "$holder_pod" ] || return 1
	k -n "$OPERATOR_NAMESPACE" get pod "$holder_pod" -o json 2>/dev/null |
		jq -e --arg release "$HELM_RELEASE" '
      .metadata.deletionTimestamp == null and
      .metadata.labels["app.kubernetes.io/instance"] == $release and
      .metadata.labels["app.kubernetes.io/component"] == "controller" and
      any(.status.conditions[]?; .type == "Ready" and .status == "True")
    ' >/dev/null
}

wait_for_leader() {
	wait_previous_holder=$1
	wait_deadline=$(($(date +%s) + LEADER_TIMEOUT_SECONDS))
	while [ "$(date +%s)" -lt "$wait_deadline" ]; do
		wait_holder=$(leader_holder || true)
		if [ -n "$wait_holder" ] && [ "$wait_holder" != "$wait_previous_holder" ] &&
			holder_is_ready_manager_pod "$wait_holder"; then
			printf '%s\n' "$wait_holder"
			return 0
		fi
		sleep 1
	done
	k -n "$OPERATOR_NAMESPACE" get lease "$LEADER_LEASE" -o yaml >&2 || true
	k -n "$OPERATOR_NAMESPACE" get pods \
		-l app.kubernetes.io/component=controller -o wide >&2 || true
	fail "manager leader Lease did not move to a ready replica"
}

assert_active_leader_metric() {
	metric_holder=$1
	metric_pod=$(leader_pod_name "$metric_holder")
	metric_body=$(k get --raw \
		"/api/v1/namespaces/${OPERATOR_NAMESPACE}/pods/${metric_pod}:8080/proxy/metrics")
	printf '%s\n' "$metric_body" |
		grep -Eq '^leader_election_master_status(\{[^}]*\})?[[:space:]]+1(\.0+)?$' ||
		fail "$OPERATOR_NAMESPACE/$metric_pod does not report active leader status"
}

assert_lease_identity() {
	identity_expected_uid=$1
	identity_actual_uid=$(k -n "$OPERATOR_NAMESPACE" get lease "$LEADER_LEASE" \
		-o jsonpath='{.metadata.uid}')
	[ "$identity_actual_uid" = "$identity_expected_uid" ] ||
		fail "leader Lease was recreated during failover: $identity_actual_uid != $identity_expected_uid"
	identity_count=$(k get leases.coordination.k8s.io -A -o json |
		jq --arg name "$LEADER_LEASE" '[.items[] | select(.metadata.name == $name)] | length')
	[ "$identity_count" -eq 1 ] ||
		fail "cluster contains $identity_count manager leader Leases named $LEADER_LEASE, want one"
}

wait_for_default_service_account() {
	service_account_deadline=$(($(date +%s) + 60))
	while [ "$(date +%s)" -lt "$service_account_deadline" ]; do
		if k -n "$HA_TEST_NAMESPACE" get serviceaccount default >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	fail "$HA_TEST_NAMESPACE/default ServiceAccount was not created"
}

wait_for_admitted_operation_pod() {
	schema_uid=$1
	workload_deadline=$(($(date +%s) + WORKLOAD_TIMEOUT_SECONDS))
	while [ "$(date +%s)" -lt "$workload_deadline" ]; do
		jobs=$(k -n "$HA_TEST_NAMESPACE" get jobs \
			-l "operator.ptah.dev/schema=${HA_SCHEMA}" -o json)
		job_count=$(printf '%s\n' "$jobs" | jq --arg uid "$schema_uid" '
      [.items[] | select(any(.metadata.ownerReferences[]?;
        .controller == true and
        .apiVersion == "operator.ptah.dev/v1alpha1" and
        .kind == "PtahSchema" and
        .name == "leader-failover" and
        .uid == $uid))] | length
    ')
		[ "$job_count" -le 1 ] || fail "failover reconciliation created $job_count operation Jobs"
		if [ "$job_count" -eq 1 ]; then
			job_name=$(printf '%s\n' "$jobs" | jq -r --arg uid "$schema_uid" '
        [.items[] | select(any(.metadata.ownerReferences[]?;
          .controller == true and .uid == $uid))][0].metadata.name
      ')
			job_uid=$(printf '%s\n' "$jobs" | jq -r --arg name "$job_name" '
        .items[] | select(.metadata.name == $name) | .metadata.uid
      ')
			job_digest=$(printf '%s\n' "$jobs" | jq -r --arg name "$job_name" '
        .items[] | select(.metadata.name == $name) |
        .metadata.annotations["operator.ptah.dev/admission-snapshot-digest"] // ""
      ')
			pods=$(k -n "$HA_TEST_NAMESPACE" get pods \
				-l "batch.kubernetes.io/job-name=${job_name}" -o json)
			if printf '%s\n' "$pods" | jq -e \
				--arg job_uid "$job_uid" --arg digest "$job_digest" '
          ($digest | test("^sha256:[0-9a-f]{64}$")) and
          ([.items[] | select(
            .metadata.deletionTimestamp == null and
            .metadata.annotations["operator.ptah.dev/admission-snapshot-digest"] == $digest and
            any(.metadata.ownerReferences[]?;
              .controller == true and
              .apiVersion == "batch/v1" and
              .kind == "Job" and
              .uid == $job_uid))] | length) == 1
        ' >/dev/null; then
			printf '%s\n' "$job_name"
			return 0
		fi
		fi
		sleep 1
	done
	k -n "$HA_TEST_NAMESPACE" get ptahschema "$HA_SCHEMA" -o yaml >&2 || true
	k -n "$HA_TEST_NAMESPACE" get jobs,pods -o wide >&2 || true
	k -n "$HA_TEST_NAMESPACE" get events --sort-by=.metadata.creationTimestamp >&2 || true
	fail "new leader did not reconcile an operation into an admitted Job Pod"
}

printf '%s\n' 'e2e HA: verifying namespace-scoped Lease authorization'
for manager_verb in get create update; do
	assert_can_i yes "$OPERATOR_NAMESPACE" "$manager_verb"
	assert_can_i no "$FOREIGN_NAMESPACE" "$manager_verb"
done
for denied_verb in list watch patch delete deletecollection; do
	assert_can_i no "$OPERATOR_NAMESPACE" "$denied_verb"
	assert_can_i no "$FOREIGN_NAMESPACE" "$denied_verb"
done

k -n "$OPERATOR_NAMESPACE" get role "$MANAGER" -o json |
	jq -e '
    .rules == [{
      apiGroups: ["coordination.k8s.io"],
      resources: ["leases"],
      verbs: ["get", "create", "update"]
    }]
  ' >/dev/null || fail "manager Lease Role is not exact"
k get clusterrole "$MANAGER" -o json |
	jq -e '[.rules[]? | select(
      (.apiGroups | index("coordination.k8s.io")) != null and
      (.resources | index("leases")) != null)] | length == 0
  ' >/dev/null || fail "manager ClusterRole grants cluster-wide Lease access"

k -n "$OPERATOR_NAMESPACE" rollout status deployment/"$MANAGER" --timeout=180s
ready_replicas=$(k -n "$OPERATOR_NAMESPACE" get deployment "$MANAGER" \
	-o jsonpath='{.status.readyReplicas}')
[ "$ready_replicas" -eq 2 ] || fail "manager Deployment has $ready_replicas ready replicas, want two"

initial_holder=$(wait_for_leader "")
initial_pod=$(leader_pod_name "$initial_holder")
lease_uid=$(k -n "$OPERATOR_NAMESPACE" get lease "$LEADER_LEASE" -o jsonpath='{.metadata.uid}')
initial_transitions=$(k -n "$OPERATOR_NAMESPACE" get lease "$LEADER_LEASE" \
	-o json | jq '.spec.leaseTransitions // 0')
assert_lease_identity "$lease_uid"
assert_active_leader_metric "$initial_holder"

printf 'e2e HA: deleting leader Pod %s/%s\n' "$OPERATOR_NAMESPACE" "$initial_pod"
k -n "$OPERATOR_NAMESPACE" delete pod "$initial_pod" --wait=true --timeout=90s >/dev/null
second_holder=$(wait_for_leader "$initial_holder")
assert_lease_identity "$lease_uid"
second_transitions=$(k -n "$OPERATOR_NAMESPACE" get lease "$LEADER_LEASE" \
	-o json | jq '.spec.leaseTransitions // 0')
[ "$second_transitions" -gt "$initial_transitions" ] ||
	fail "leader Pod failover did not increment leaseTransitions"
assert_active_leader_metric "$second_holder"
k -n "$OPERATOR_NAMESPACE" rollout status deployment/"$MANAGER" --timeout=180s

printf '%s\n' 'e2e HA: creating a real operation after leader failover'
k wait --for=condition=Established crd/ptahschemas.operator.ptah.dev --timeout=60s
k create namespace "$HA_TEST_NAMESPACE" >/dev/null
wait_for_default_service_account
k -n "$HA_TEST_NAMESPACE" create configmap e2e-ha-verification-policy \
	--from-file="policy.yaml=${ROOT_DIR}/testdata/e2e/verification-policy.yaml" >/dev/null
k -n "$HA_TEST_NAMESPACE" create secret generic e2e-ha-database-url \
	--from-literal=url='postgres://e2e:unused@database.invalid/e2e' >/dev/null
jq -n --arg namespace "$HA_TEST_NAMESPACE" --arg name "$HA_SCHEMA" '
  {
    apiVersion: "operator.ptah.dev/v1alpha1",
    kind: "PtahSchema",
    metadata: {namespace: $namespace, name: $name},
    spec: {
      target: {
        engine: "PostgreSQL",
        coordinationKey: "e2e/ha/leader-failover",
        urlFrom: {name: "e2e-ha-database-url", key: "url"}
      },
      desired: {
        ociRef: "oci://127.0.0.1:1/e2e/leader-failover:unreachable",
        verificationPolicyFrom: {name: "e2e-ha-verification-policy", key: "policy.yaml"},
        transport: {plainHTTP: true}
      },
      interval: "24h",
      execution: {activeDeadlineSeconds: 120, failureRetryInterval: "1h"}
    }
  }
' | k create -f - >/dev/null
ha_schema_uid=$(k -n "$HA_TEST_NAMESPACE" get ptahschema "$HA_SCHEMA" \
	-o jsonpath='{.metadata.uid}')
operation_job=$(wait_for_admitted_operation_pod "$ha_schema_uid")
[ -n "$operation_job" ] || fail "failover operation Job name is empty"
assert_active_leader_metric "$second_holder"
assert_lease_identity "$lease_uid"

k -n "$HA_TEST_NAMESPACE" delete ptahschema "$HA_SCHEMA" --wait=false >/dev/null
k -n "$HA_TEST_NAMESPACE" delete job "$operation_job" \
	--cascade=foreground --wait=true --timeout=90s >/dev/null
remaining_operation_pods=$(k -n "$HA_TEST_NAMESPACE" get pods \
	-l "batch.kubernetes.io/job-name=${operation_job}" -o name)
[ -z "$remaining_operation_pods" ] ||
	fail "foreground Job deletion left operation Pods: $remaining_operation_pods"
k -n "$HA_TEST_NAMESPACE" wait --for=delete ptahschema/"$HA_SCHEMA" --timeout=60s
k delete namespace "$HA_TEST_NAMESPACE" --wait=true --timeout=120s >/dev/null

printf '%s\n' 'e2e HA: PASS one Lease, exact RBAC, Pod failover, and admitted post-failover operation'
