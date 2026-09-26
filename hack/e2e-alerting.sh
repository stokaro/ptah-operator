#!/bin/sh

set -eu

# The alerting path from a manager's metrics to a person, measured on the
# cluster the migrations suite leaves behind.
#
# hack/prometheus_rule_test.go runs the chart's rules through promtool against
# synthetic series. That proves the expressions, and nothing about the path a
# real alert takes: a Prometheus that has to find every manager Pod, rules
# rendered exactly as the chart renders them, an Alertmanager that has to route
# them, and a receiver that has to be told. This phase stands up that path with
# plain Deployments -- no Prometheus Operator, so the rules are taken from the
# chart's PrometheusRule and loaded as a file -- and asserts at the receiver:
#
#   - an Apply nobody accounted for, which the migrations phase leaves behind,
#     reaches the receiver naming its family, with a count the runbook's own
#     drill-down reproduces and a runbook link that resolves to a heading;
#   - an operation held off every node fires as stalled once its threshold has
#     passed and not before, and resolves once the operation leaves flight;
#   - every manager gone fires as a view nobody can read, and resolves once
#     they are back.
#
# The receiver is test/e2e/alertsink, from the isolated fixture image. What it
# logs is what Alertmanager delivered, which is the claim; Alertmanager's own
# view of what it meant to send is not.

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
OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:-}
HELM_RELEASE=${E2E_HELM_RELEASE:-}
CHART_PACKAGE=${E2E_CHART_PACKAGE:-}
FIXTURE_IMAGE=${E2E_FIXTURE_IMAGE:-}
PROMETHEUS_IMAGE=${E2E_PROMETHEUS_IMAGE:-}
ALERTMANAGER_IMAGE=${E2E_ALERTMANAGER_IMAGE:-}
REGISTRY_CREDENTIALS_FILE=${E2E_REGISTRY_CREDENTIALS_FILE:-}

PHASE_REASON_MARKER=${TMPDIR:-/tmp}/ptah-e2e-reason-alerting.$$

fail() {
	printf 'e2e alerting: %s\n' "$*" >&2
	if [ -n "${PHASE_REASON_MARKER:-}" ]; then
		: >"$PHASE_REASON_MARKER" 2>/dev/null || true
	fi
	exit 1
}

for value_name in \
	KUBECONFIG_FILE OPERATOR_NAMESPACE HELM_RELEASE CHART_PACKAGE FIXTURE_IMAGE \
	PROMETHEUS_IMAGE ALERTMANAGER_IMAGE REGISTRY_CREDENTIALS_FILE; do
	eval "value=\${$value_name}"
	[ -n "$value" ] || fail "$value_name is required"
done
[ -f "$KUBECONFIG_FILE" ] || fail "E2E_KUBECONFIG does not name a file"
[ -f "$CHART_PACKAGE" ] || fail "E2E_CHART_PACKAGE does not name a file"
for pinned in "$FIXTURE_IMAGE" "$PROMETHEUS_IMAGE" "$ALERTMANAGER_IMAGE"; do
	printf '%s\n' "$pinned" | grep -Eq '@sha256:[0-9a-f]{64}$' ||
		fail "image $pinned is not pinned by digest"
done

k() {
	kubectl --kubeconfig "$KUBECONFIG_FILE" "$@"
}

MONITORING_NAMESPACE=ptah-e2e-monitoring
STALLED_NAMESPACE=ptah-e2e-alerting-stalled
STALLED_SCHEMA="held-resolve"
GATE_LABEL=operator.ptah.run/e2e-alerting-gate
PULL_SECRET=e2e-alerting-registry-pull
DISCOVERY_ROLE=e2e-alerting-discovery

# The numbers the rules are rendered with. They are this phase's, chosen to
# keep it short, and they are not advice: the chart has no default for either
# because the right value depends on the cluster.
VIEW_UNSYNCED_FOR_SECONDS=60
STALLED_AFTER_SECONDS=60

# How often Prometheus scrapes and evaluates, and how long Alertmanager waits
# before it sends a new group. An alert whose condition holds reaches the
# receiver after its threshold plus, at most, one scrape, one evaluation, the
# group wait and one delivery. DETECTION_SLACK_SECONDS is that sum with room
# for a slow API server, and it is the detection target this phase declares:
# a delivery later than threshold plus slack fails the phase.
SCRAPE_SECONDS=5
GROUP_WAIT_SECONDS=5
DETECTION_SLACK_SECONDS=45
TIMEOUT_SECONDS=300

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-e2e-alerting.XXXXXX")
CORDONED_NODES=
GATE_OPENED=0

PHASE_COMPLETED=0
cleanup() {
	status=$?
	[ "$status" -ne 0 ] || [ "$PHASE_COMPLETED" -eq 1 ] || status=1
	trap - EXIT HUP INT TERM
	if [ "$status" -ne 0 ]; then
		if [ -n "${PHASE_REASON_MARKER:-}" ] && [ ! -f "$PHASE_REASON_MARKER" ]; then
			printf 'e2e alerting: exited with status %s at a command that failed under set -e; no proof reported a reason\n' "$status" >&2
		fi
		report_state || true
	fi
	# Nodes cordoned by the manager-loss row are released whatever happened,
	# because every later step of any run on this cluster schedules Pods.
	for node in $CORDONED_NODES; do
		k uncordon "$node" >/dev/null 2>&1 || true
	done
	if [ "$GATE_OPENED" -eq 1 ]; then
		k label nodes --all "${GATE_LABEL}-" >/dev/null 2>&1 || true
	fi
	k delete namespace "$STALLED_NAMESPACE" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
	k delete namespace "$MONITORING_NAMESPACE" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
	k -n "$OPERATOR_NAMESPACE" delete rolebinding "$DISCOVERY_ROLE" --ignore-not-found=true >/dev/null 2>&1 || true
	k -n "$OPERATOR_NAMESPACE" delete role "$DISCOVERY_ROLE" --ignore-not-found=true >/dev/null 2>&1 || true
	rm -rf "$WORK_DIR"
	rm -f "${PHASE_REASON_MARKER:-}"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

deadline_from_now() {
	printf '%s\n' "$(($(date +%s) + ${1:-$TIMEOUT_SECONDS}))"
}

# What the monitoring path held when the phase failed: the receiver's log, the
# alerts Prometheus had, and its targets. A failure here otherwise reads as a
# timeout with nothing behind it.
report_state() {
	printf 'e2e alerting: receiver log:\n' >&2
	k -n "$MONITORING_NAMESPACE" logs deployment/alert-sink --tail=60 2>/dev/null | sed 's/^/  /' >&2 || true
	printf 'e2e alerting: Prometheus alerts:\n' >&2
	prometheus_api "/api/v1/alerts" 2>/dev/null |
		jq -r '.data.alerts[]? | "  \(.labels.alertname) \(.state) \(.labels | del(.alertname) | tostring)"' >&2 || true
	printf 'e2e alerting: Prometheus targets:\n' >&2
	prometheus_api "/api/v1/targets" 2>/dev/null |
		jq -r '.data.activeTargets[]? | "  \(.labels.pod // .scrapeUrl) \(.health) \(.lastError)"' >&2 || true
	printf 'e2e alerting: Alertmanager log:\n' >&2
	k -n "$MONITORING_NAMESPACE" logs deployment/alertmanager --tail=30 2>/dev/null | sed 's/^/  /' >&2 || true
}

# A read of Prometheus's HTTP API through the API server's service proxy, so the
# phase needs no port of its own on the cluster.
prometheus_api() {
	k get --raw "/api/v1/namespaces/${MONITORING_NAMESPACE}/services/http:prometheus:9090/proxy$1"
}

prometheus_query() {
	query=$(printf '%s' "$1" | jq -sRr @uri)
	prometheus_api "/api/v1/query?query=${query}"
}

# Every delivery the receiver logged, one JSON object per line. The receiver
# writes its own diagnostics to standard error as plain text, so only the
# lines that are objects are deliveries.
deliveries() {
	k -n "$MONITORING_NAMESPACE" logs deployment/alert-sink 2>/dev/null |
		grep '^{' || true
}

# Waits for a delivery the jq filter selects and prints the first one as JSON.
wait_for_delivery() {
	delivery_filter=$1
	delivery_description=$2
	delivery_deadline=$(deadline_from_now "${3:-$TIMEOUT_SECONDS}")
	while [ "$(date +%s)" -lt "$delivery_deadline" ]; do
		if matched=$(deliveries | jq -c "select(${delivery_filter})" | head -n 1) && [ -n "$matched" ]; then
			printf '%s\n' "$matched"
			return 0
		fi
		sleep 3
	done
	fail "the receiver never got ${delivery_description}"
}

# Seconds between two RFC 3339 instants, the second minus the first. GNU date
# on the runner reads both; the fractional part Alertmanager writes is dropped.
seconds_between() {
	first=$(date -u -d "$(printf '%s' "$1" | sed 's/\.[0-9]*//')" +%s) ||
		fail "could not read the instant $1"
	second=$(date -u -d "$(printf '%s' "$2" | sed 's/\.[0-9]*//')" +%s) ||
		fail "could not read the instant $2"
	printf '%s\n' "$((second - first))"
}

wait_for_rollout() {
	k -n "$MONITORING_NAMESPACE" rollout status "deployment/$1" --timeout="${TIMEOUT_SECONDS}s" >/dev/null ||
		fail "$1 did not become ready"
}

MANAGER=$(k -n "$OPERATOR_NAMESPACE" get deployment \
	-l app.kubernetes.io/component=controller \
	-o jsonpath='{.items[0].metadata.name}')
[ -n "$MANAGER" ] || fail "installed controller Deployment is missing"
MANAGER_REPLICAS=$(k -n "$OPERATOR_NAMESPACE" get deployment "$MANAGER" -o jsonpath='{.spec.replicas}')
[ "${MANAGER_REPLICAS:-0}" -ge 1 ] || fail "the controller Deployment asks for no replica"
MANAGER_SELECTOR=$(k -n "$OPERATOR_NAMESPACE" get deployment "$MANAGER" -o json |
	jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')
[ -n "$MANAGER_SELECTOR" ] || fail "the controller Deployment has no label selector"
METRICS_SERVICE=$(k -n "$OPERATOR_NAMESPACE" get services -o json |
	jq -r '[.items[] | select(any(.spec.ports[]?; .name == "metrics"))] |
      if length == 1 then .[0].metadata.name else empty end')
[ -n "$METRICS_SERVICE" ] ||
	fail "the release has no single Service with a port named metrics for Prometheus to discover"
MANAGER_IMAGE=$(k -n "$OPERATOR_NAMESPACE" get deployment "$MANAGER" \
	-o jsonpath='{.spec.template.spec.containers[0].image}')
REGISTRY_HOST=${MANAGER_IMAGE%%/*}
REGISTRY_USERNAME=$(jq -er '.username' "$REGISTRY_CREDENTIALS_FILE") ||
	fail "E2E_REGISTRY_CREDENTIALS_FILE has no username"
REGISTRY_PASSWORD=$(jq -er '.password' "$REGISTRY_CREDENTIALS_FILE") ||
	fail "E2E_REGISTRY_CREDENTIALS_FILE has no password"

k get namespace "$MONITORING_NAMESPACE" >/dev/null 2>&1 &&
	fail "namespace $MONITORING_NAMESPACE already exists; this phase stands it up and removes it"
k get namespace "$STALLED_NAMESPACE" >/dev/null 2>&1 &&
	fail "namespace $STALLED_NAMESPACE already exists; this phase stands it up and removes it"
held=$(k get nodes -l "$GATE_LABEL" -o name)
[ -z "$held" ] || fail "nodes already carry $GATE_LABEL: $held"

# --- The rules, as the chart renders them ------------------------------------

# The release's own values, so the rules are the ones this installation would
# render, with the monitoring switches this phase needs laid over them.
k -n "$OPERATOR_NAMESPACE" get secret -l "owner=helm,name=${HELM_RELEASE},status=deployed" \
	-o name | grep -q . || fail "release $HELM_RELEASE has no deployed revision to read values from"
helm --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" get values "$HELM_RELEASE" -o yaml \
	>"$WORK_DIR/release-values.yaml" ||
	fail "the values of release $HELM_RELEASE could not be read"
helm template "$HELM_RELEASE" "$CHART_PACKAGE" --namespace "$OPERATOR_NAMESPACE" \
	-f "$WORK_DIR/release-values.yaml" \
	--set monitoring.prometheusRule.enabled=true \
	--set "monitoring.prometheusRule.viewUnsyncedFor=${VIEW_UNSYNCED_FOR_SECONDS}s" \
	--set "monitoring.prometheusRule.operationStalledAfterSeconds=${STALLED_AFTER_SECONDS}" \
	--show-only templates/prometheusrule.yaml >"$WORK_DIR/prometheusrule.yaml" ||
	fail "the chart did not render its PrometheusRule"
RUNBOOK_BASE=$(helm show values "$CHART_PACKAGE" |
	sed -n 's/^ *runbookBaseURL: *"\(.*\)"$/\1/p')
[ -n "$RUNBOOK_BASE" ] || fail "the chart names no runbookBaseURL"

# A Prometheus without the Operator reads a rule file, which is the spec of the
# PrometheusRule one level up. The template is this repository's, so its shape
# is known: everything under `spec:` is the groups.
awk 'found { sub(/^  /, ""); print } /^spec:$/ { found = 1 }' "$WORK_DIR/prometheusrule.yaml" \
	>"$WORK_DIR/rules.yaml"
head -n 1 "$WORK_DIR/rules.yaml" | grep -qx 'groups:' ||
	fail "the rendered PrometheusRule has no spec.groups to load"
for alert in PtahOperatorUnresolvedApply PtahOperatorUnresolvedViewNotSynced PtahOperatorOperationStalled; do
	grep -q "alert: ${alert}\$" "$WORK_DIR/rules.yaml" ||
		fail "the rendered rules have no ${alert}"
done

# --- The path: Prometheus, Alertmanager, a receiver ---------------------------

printf 'e2e alerting: standing up Prometheus, Alertmanager and a receiver in %s\n' \
	"$MONITORING_NAMESPACE" >&2
k create namespace "$MONITORING_NAMESPACE" >/dev/null
jq -n --arg namespace "$MONITORING_NAMESPACE" --arg name "$PULL_SECRET" \
	--arg registry "$REGISTRY_HOST" --arg username "$REGISTRY_USERNAME" \
	--arg password "$REGISTRY_PASSWORD" '
  {
    apiVersion: "v1", kind: "Secret",
    metadata: {namespace: $namespace, name: $name},
    type: "kubernetes.io/dockerconfigjson",
    stringData: {
      ".dockerconfigjson": ({auths: {
        ($registry): {username: $username, password: $password,
                      auth: (($username + ":" + $password) | @base64)}
      }} | tojson)
    }
  }' | k create -f - >/dev/null

# Prometheus discovers the manager Pods behind the metrics Service one by one,
# as the chart's ServiceMonitor would: a single scrape of the Service address
# would land on whichever replica answered.
k -n "$MONITORING_NAMESPACE" create serviceaccount prometheus >/dev/null
jq -n --arg namespace "$OPERATOR_NAMESPACE" --arg name "$DISCOVERY_ROLE" \
	--arg subjectNamespace "$MONITORING_NAMESPACE" '
  {
    apiVersion: "v1", kind: "List",
    items: [
      {
        apiVersion: "rbac.authorization.k8s.io/v1", kind: "Role",
        metadata: {namespace: $namespace, name: $name},
        rules: [
          {apiGroups: [""], resources: ["pods", "services"], verbs: ["get", "list", "watch"]},
          {apiGroups: ["discovery.k8s.io"], resources: ["endpointslices"], verbs: ["get", "list", "watch"]}
        ]
      },
      {
        apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding",
        metadata: {namespace: $namespace, name: $name},
        roleRef: {apiGroup: "rbac.authorization.k8s.io", kind: "Role", name: $name},
        subjects: [{kind: "ServiceAccount", namespace: $subjectNamespace, name: "prometheus"}]
      }
    ]
  }' | k create -f - >/dev/null

cat >"$WORK_DIR/prometheus.yml" <<EOF
global:
  scrape_interval: ${SCRAPE_SECONDS}s
  evaluation_interval: ${SCRAPE_SECONDS}s
rule_files:
  - /etc/prometheus/rules.yaml
alerting:
  alertmanagers:
    - static_configs:
        - targets: ["alertmanager.${MONITORING_NAMESPACE}.svc:9093"]
scrape_configs:
  - job_name: ptah-operator
    kubernetes_sd_configs:
      - role: endpointslice
        namespaces:
          names: ["${OPERATOR_NAMESPACE}"]
    relabel_configs:
      - source_labels: [__meta_kubernetes_service_name]
        regex: ${METRICS_SERVICE}
        action: keep
      - source_labels: [__meta_kubernetes_endpointslice_port_name]
        regex: metrics
        action: keep
      - source_labels: [__meta_kubernetes_pod_name]
        target_label: pod
EOF
k -n "$MONITORING_NAMESPACE" create configmap prometheus \
	--from-file=prometheus.yml="$WORK_DIR/prometheus.yml" \
	--from-file=rules.yaml="$WORK_DIR/rules.yaml" >/dev/null

cat >"$WORK_DIR/alertmanager.yml" <<EOF
route:
  receiver: sink
  group_by: [alertname, family, operation]
  group_wait: ${GROUP_WAIT_SECONDS}s
  group_interval: 10s
  repeat_interval: 1h
receivers:
  - name: sink
    webhook_configs:
      - url: http://alert-sink.${MONITORING_NAMESPACE}.svc:8080/alerts
        send_resolved: true
EOF
k -n "$MONITORING_NAMESPACE" create configmap alertmanager \
	--from-file=alertmanager.yml="$WORK_DIR/alertmanager.yml" >/dev/null

# One Deployment and Service each. The Pods run as the image's non-root user
# with nothing but an emptyDir to write to.
monitoring_workload() {
	jq -n \
		--arg namespace "$MONITORING_NAMESPACE" \
		--arg name "$1" \
		--arg image "$2" \
		--argjson port "$3" \
		--argjson args "$4" \
		--arg serviceAccount "$5" \
		--arg configMap "$6" \
		--argjson command "$7" \
		--arg pullSecret "$PULL_SECRET" '
    {
      apiVersion: "v1", kind: "List",
      items: [
        {
          apiVersion: "apps/v1", kind: "Deployment",
          metadata: {namespace: $namespace, name: $name},
          spec: {
            replicas: 1,
            selector: {matchLabels: {app: $name}},
            template: {
              metadata: {labels: {app: $name}},
              spec: ({
                serviceAccountName: $serviceAccount,
                automountServiceAccountToken: ($serviceAccount != "default"),
                imagePullSecrets: [{name: $pullSecret}],
                securityContext: {
                  runAsNonRoot: true, runAsUser: 65534, runAsGroup: 65534, fsGroup: 65534,
                  seccompProfile: {type: "RuntimeDefault"}
                },
                containers: [({
                  name: $name, image: $image, args: $args,
                  ports: [{name: "http", containerPort: $port}],
                  readinessProbe: {tcpSocket: {port: $port}, periodSeconds: 2},
                  securityContext: {
                    allowPrivilegeEscalation: false, readOnlyRootFilesystem: true,
                    capabilities: {drop: ["ALL"]}
                  },
                  volumeMounts: ([{name: "data", mountPath: "/data"}] +
                    (if $configMap == "" then [] else [{name: "config", mountPath: "/etc/\($configMap)"}] end))
                } + (if $command == null then {} else {command: $command} end))],
                volumes: ([{name: "data", emptyDir: {}}] +
                  (if $configMap == "" then [] else [{name: "config", configMap: {name: $configMap}}] end))
              })
            }
          }
        },
        {
          apiVersion: "v1", kind: "Service",
          metadata: {namespace: $namespace, name: $name},
          spec: {selector: {app: $name}, ports: [{name: "http", port: $port, targetPort: $port}]}
        }
      ]
    }' | k create -f - >/dev/null
}

# The receiver comes from the isolated fixture image, whose entrypoint is the
# OCI publisher, so it names its own command.
monitoring_workload alert-sink "$FIXTURE_IMAGE" 8080 \
	'["-listen", ":8080"]' default "" '["/e2e-alert-sink"]'
monitoring_workload alertmanager "$ALERTMANAGER_IMAGE" 9093 \
	'["--config.file=/etc/alertmanager/alertmanager.yml", "--storage.path=/data", "--cluster.listen-address="]' \
	default alertmanager null
monitoring_workload prometheus "$PROMETHEUS_IMAGE" 9090 \
	'["--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/data"]' \
	prometheus prometheus null
wait_for_rollout alert-sink
wait_for_rollout alertmanager
wait_for_rollout prometheus

# Every manager replica is a target, and every target is up. A Prometheus that
# found one replica of two would pass everything below on the leader's
# numbers and say nothing about the other.
targets_deadline=$(deadline_from_now)
while :; do
	if prometheus_api /api/v1/targets >"$WORK_DIR/targets.json" 2>/dev/null &&
		jq -e --argjson replicas "$MANAGER_REPLICAS" '
        [.data.activeTargets[] | select(.labels.job == "ptah-operator")] |
        length == $replicas and all(.health == "up")
      ' "$WORK_DIR/targets.json" >/dev/null; then
		break
	fi
	[ "$(date +%s)" -lt "$targets_deadline" ] || {
		report_state
		fail "Prometheus did not reach all ${MANAGER_REPLICAS} manager replicas"
	}
	sleep 3
done
prometheus_api /api/v1/rules >"$WORK_DIR/rules.json" ||
	fail "Prometheus did not answer for its rules"
jq -e '[.data.groups[].rules[] | select(.type == "alerting") | .name] |
    index("PtahOperatorUnresolvedApply") != null and
    index("PtahOperatorOperationStalled") != null' "$WORK_DIR/rules.json" >/dev/null ||
	fail "Prometheus did not load the chart's rules"
printf 'e2e alerting: Prometheus scrapes all %s manager replicas and loaded the chart rules\n' \
	"$MANAGER_REPLICAS" >&2

# --- An Apply nobody accounted for ----------------------------------------

# The migrations phase leaves at least one: the row that removed an Apply Job
# while its run was going. The count the runbook's drill-down finds is the
# count the alert has to carry.
unresolved_listed=$(k get ptahmigrations -A -o json |
	jq '[.items[] | select(.status.unresolvedRun != null)] | length')
[ "$unresolved_listed" -ge 1 ] ||
	fail "no PtahMigration carries an unresolved run, so this row has nothing to alert on; the migrations phase leaves one"
unresolved=$(wait_for_delivery \
	'.status == "firing" and .alertname == "PtahOperatorUnresolvedApply" and .labels.family == "migration"' \
	"PtahOperatorUnresolvedApply for the migration family")
printf '%s' "$unresolved" | jq -e --arg runbook "${RUNBOOK_BASE}#unresolved-gauges" \
	'.annotations.runbook_url == $runbook and .labels.severity == "critical"' >/dev/null ||
	fail "the unresolved alert arrived without the runbook link or severity the chart gives it: $unresolved"
grep -q '{#unresolved-gauges}' "$ROOT_DIR/docs/site/src/content/docs/use/operations.md" ||
	fail "the unresolved alert links to #unresolved-gauges, and the operations page has no such heading"
# The summary carries the count, and the drill-down on the page has to name as
# many resources as the alert counted, or the page does not lead to the scope.
alerted_count=$(printf '%s' "$unresolved" | jq -r '.annotations.summary' | sed -n 's/^\([0-9][0-9]*\) .*/\1/p')
[ "$alerted_count" = "$unresolved_listed" ] ||
	fail "the alert counts ${alerted_count:-nothing} unresolved migrations and the runbook's drill-down lists $unresolved_listed"
printf 'e2e alerting: PASS the receiver got PtahOperatorUnresolvedApply for %s migration(s), and the runbook lists them\n' \
	"$unresolved_listed" >&2

# --- An operation that stops moving -------------------------------------------

# A schema whose operation Pods may run only on a node carrying the gate label,
# while no node does: its first Resolve is claimed, its Pod never schedules,
# and the operation stays in flight. Nothing else in the cluster runs a schema
# Resolve for this long, which the row checks rather than assumes.
prometheus_query 'ALERTS{alertname="PtahOperatorOperationStalled",family="schema",operation="Resolve"}' \
	>"$WORK_DIR/stalled-before.json" || fail "Prometheus did not answer for its alerts"
jq -e '.data.result | length == 0' "$WORK_DIR/stalled-before.json" >/dev/null ||
	fail "a schema Resolve was already stalled before this row held one, so the row could not tell them apart"
k create namespace "$STALLED_NAMESPACE" >/dev/null
k -n "$STALLED_NAMESPACE" create configmap e2e-alerting-verification-policy \
	--from-file="policy.yaml=${ROOT_DIR}/testdata/e2e/verification-policy.yaml" >/dev/null
k -n "$STALLED_NAMESPACE" create secret generic e2e-alerting-database-url \
	--from-literal=url='postgres://e2e:unused@database.invalid/e2e' >/dev/null
k -n "$MONITORING_NAMESPACE" get secret "$PULL_SECRET" -o json |
	jq --arg namespace "$STALLED_NAMESPACE" \
		'{apiVersion, kind, type, data, metadata: {name: .metadata.name, namespace: $namespace}}' |
	k create -f - >/dev/null
jq -n --arg namespace "$STALLED_NAMESPACE" --arg name "$STALLED_SCHEMA" \
	--arg pullSecret "$PULL_SECRET" --arg gate "$GATE_LABEL" '
  {
    apiVersion: "operator.ptah.run/v1alpha1", kind: "PtahSchema",
    metadata: {namespace: $namespace, name: $name},
    spec: {
      target: {
        engine: "PostgreSQL",
        coordinationKey: "e2e/alerting/held-resolve",
        urlFrom: {name: "e2e-alerting-database-url", key: "url"}
      },
      desired: {
        ociRef: "oci://127.0.0.1:1/e2e/held-resolve:unreachable",
        verificationPolicyFrom: {name: "e2e-alerting-verification-policy", key: "policy.yaml"},
        transport: {plainHTTP: true}
      },
      interval: "24h",
      execution: {
        activeDeadlineSeconds: 600, failureRetryInterval: "1h",
        serviceAccountName: "default", imagePullSecrets: [{name: $pullSecret}],
        nodeSelector: ($gate | {(.): "open"})
      }
    }
  }' | k create -f - >/dev/null

stalled_claim_deadline=$(deadline_from_now)
stalled_started=
while [ "$(date +%s)" -lt "$stalled_claim_deadline" ]; do
	stalled_started=$(k -n "$STALLED_NAMESPACE" get ptahschema "$STALLED_SCHEMA" -o json |
		jq -r 'select(.status.activeOperation.type == "Resolve") | .status.activeOperation.startedAt // empty')
	[ -n "$stalled_started" ] && break
	sleep 2
done
[ -n "$stalled_started" ] || fail "$STALLED_SCHEMA never claimed its Resolve"
printf 'e2e alerting: %s claimed a Resolve at %s that no node will run\n' "$STALLED_SCHEMA" "$stalled_started" >&2

stalled=$(wait_for_delivery \
	'.status == "firing" and .alertname == "PtahOperatorOperationStalled" and .labels.family == "schema" and .labels.operation == "Resolve"' \
	"PtahOperatorOperationStalled for the held schema Resolve" \
	$((STALLED_AFTER_SECONDS + TIMEOUT_SECONDS)))
stalled_after=$(seconds_between "$stalled_started" "$(printf '%s' "$stalled" | jq -r '.receivedAt')")
[ "$stalled_after" -ge "$STALLED_AFTER_SECONDS" ] ||
	fail "the stalled alert arrived ${stalled_after}s after the operation started, before its ${STALLED_AFTER_SECONDS}s threshold"
[ "$stalled_after" -le $((STALLED_AFTER_SECONDS + DETECTION_SLACK_SECONDS)) ] ||
	fail "the stalled alert arrived ${stalled_after}s after the operation started; the declared target is ${STALLED_AFTER_SECONDS}s plus ${DETECTION_SLACK_SECONDS}s"
printf '%s' "$stalled" | jq -e --arg runbook "${RUNBOOK_BASE}#resource-state" \
	'.annotations.runbook_url == $runbook' >/dev/null ||
	fail "the stalled alert arrived without the runbook link the chart gives it: $stalled"
grep -q '{#resource-state}' "$ROOT_DIR/docs/site/src/content/docs/use/operations.md" ||
	fail "the stalled alert links to #resource-state, and the operations page has no such heading"
printf 'e2e alerting: PASS the receiver got PtahOperatorOperationStalled %ss after the Resolve started\n' \
	"$stalled_after" >&2

# Released, the Resolve runs, fails against the unreachable registry and leaves
# flight, and the alert has to clear on its own. A schema keeps a failed
# attempt's claim in status.activeOperation until its retry, with the resource
# in Failed, and the gauges count that as ended; this reads it the same way.
k label nodes --all "${GATE_LABEL}=open" --overwrite >/dev/null || fail "the gate could not be opened"
GATE_OPENED=1
LEFT_FLIGHT='(.status.activeOperation.type // "") != "Resolve" or .status.phase == "Failed"'
left_deadline=$(deadline_from_now)
while [ "$(date +%s)" -lt "$left_deadline" ]; do
	if k -n "$STALLED_NAMESPACE" get ptahschema "$STALLED_SCHEMA" -o json |
		jq -e "$LEFT_FLIGHT" >/dev/null; then
		break
	fi
	sleep 2
done
k -n "$STALLED_NAMESPACE" get ptahschema "$STALLED_SCHEMA" -o json |
	jq -e "$LEFT_FLIGHT" >/dev/null ||
	fail "the released Resolve never left flight"
left_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
wait_for_delivery \
	'.status == "resolved" and .alertname == "PtahOperatorOperationStalled" and .labels.family == "schema" and .labels.operation == "Resolve"' \
	"the resolution of PtahOperatorOperationStalled once the Resolve left flight" \
	$((DETECTION_SLACK_SECONDS + 60)) >"$WORK_DIR/stalled-resolved.json"
cleared_after=$(seconds_between "$left_at" "$(jq -r '.receivedAt' "$WORK_DIR/stalled-resolved.json")")
[ "$cleared_after" -le "$DETECTION_SLACK_SECONDS" ] ||
	fail "the stalled alert cleared ${cleared_after}s after the operation left flight; the declared target is ${DETECTION_SLACK_SECONDS}s"
# Suspended before the gate closes again, so its next Resolve is not held and
# does not fire the alert a second time.
k -n "$STALLED_NAMESPACE" patch ptahschema "$STALLED_SCHEMA" --type merge -p '{"spec":{"suspend":true}}' >/dev/null
k label nodes --all "${GATE_LABEL}-" >/dev/null
GATE_OPENED=0
printf 'e2e alerting: PASS the stalled alert cleared %ss after the Resolve left flight\n' "$cleared_after" >&2

# --- Every manager gone -------------------------------------------------------

# Nothing scrapes a manager that is not running, so the counts disappear rather
# than fall to zero, and an absent count is not evidence that nothing is
# unresolved. The rule that covers that silence has to fire. Cordoning every
# node keeps the replacement Pods Pending, which is a loss that lasts, where
# deleting them alone is a restart the Deployment repairs at once.
prometheus_query 'ALERTS{alertname="PtahOperatorUnresolvedViewNotSynced"}' >"$WORK_DIR/view-before.json" ||
	fail "Prometheus did not answer for its alerts"
jq -e '.data.result | length == 0' "$WORK_DIR/view-before.json" >/dev/null ||
	fail "PtahOperatorUnresolvedViewNotSynced was already active with every manager running"
for node in $(k get nodes -o jsonpath='{.items[*].metadata.name}'); do
	k cordon "$node" >/dev/null || fail "node $node could not be cordoned"
	CORDONED_NODES="$CORDONED_NODES $node"
done
printf 'e2e alerting: removing every manager replica with every node cordoned\n' >&2
k -n "$OPERATOR_NAMESPACE" delete pods -l "$MANAGER_SELECTOR" --wait=true --timeout="${TIMEOUT_SECONDS}s" >/dev/null ||
	fail "the manager Pods could not be removed"
lost_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
running=$(k -n "$OPERATOR_NAMESPACE" get pods -l "$MANAGER_SELECTOR" --field-selector=status.phase=Running -o name)
[ -z "$running" ] || fail "manager Pods are running with every node cordoned: $running"
lost=$(wait_for_delivery \
	'.status == "firing" and .alertname == "PtahOperatorUnresolvedViewNotSynced"' \
	"PtahOperatorUnresolvedViewNotSynced with every manager gone" \
	$((VIEW_UNSYNCED_FOR_SECONDS + TIMEOUT_SECONDS)))
lost_after=$(seconds_between "$lost_at" "$(printf '%s' "$lost" | jq -r '.receivedAt')")
[ "$lost_after" -le $((VIEW_UNSYNCED_FOR_SECONDS + DETECTION_SLACK_SECONDS)) ] ||
	fail "the lost-view alert arrived ${lost_after}s after the managers went; the declared target is ${VIEW_UNSYNCED_FOR_SECONDS}s plus ${DETECTION_SLACK_SECONDS}s"
printf 'e2e alerting: PASS the receiver got PtahOperatorUnresolvedViewNotSynced %ss after every manager went\n' \
	"$lost_after" >&2

for node in $CORDONED_NODES; do
	k uncordon "$node" >/dev/null || fail "node $node could not be uncordoned"
done
CORDONED_NODES=
k -n "$OPERATOR_NAMESPACE" rollout status "deployment/$MANAGER" --timeout="${TIMEOUT_SECONDS}s" >/dev/null ||
	fail "the managers did not come back"
wait_for_delivery \
	'.status == "resolved" and .alertname == "PtahOperatorUnresolvedViewNotSynced"' \
	"the resolution of PtahOperatorUnresolvedViewNotSynced once the managers were back" >/dev/null
printf 'e2e alerting: PASS the lost-view alert cleared once the managers were back\n' >&2

PHASE_COMPLETED=1
printf '%s\n' 'e2e alerting: PASS an unresolved Apply, a stalled operation and a lost view each reached the receiver, and the two that can clear did'
