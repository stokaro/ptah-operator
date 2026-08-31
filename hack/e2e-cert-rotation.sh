#!/bin/sh

set -eu

KUBECONFIG_FILE=${E2E_KUBECONFIG:?E2E_KUBECONFIG is required}
OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:?E2E_OPERATOR_NAMESPACE is required}
HELM_RELEASE=${E2E_HELM_RELEASE:?E2E_HELM_RELEASE is required}

fail() {
	printf 'e2e certificate rotation: %s\n' "$*" >&2
	exit 1
}

resource_name() {
	kind=$1
	component=$2
	selector="app.kubernetes.io/instance=${HELM_RELEASE}"
	if [ -n "$component" ]; then
		selector="${selector},app.kubernetes.io/component=${component}"
	fi
	names=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
		get "$kind" \
		-l "$selector" \
		-o name)
	[ "$(printf '%s\n' "$names" | sed '/^$/d' | wc -l | tr -d ' ')" -eq 1 ] ||
		fail "expected exactly one ${kind} for component ${component}"
	printf '%s\n' "${names#*/}"
}

DEPLOYMENT=$(resource_name deployment controller)
ROTATOR_DEPLOYMENT=$(resource_name deployment certificate-rotation)
MUTATING_CONFIGURATION=$(resource_name mutatingwebhookconfiguration "")
VALIDATING_CONFIGURATION=$(resource_name validatingwebhookconfiguration "")
SERVICE=$(kubectl --kubeconfig "$KUBECONFIG_FILE" get \
	mutatingwebhookconfiguration "$MUTATING_CONFIGURATION" -o json |
	jq -r '[.webhooks[] | select(.name == "mapproval.operator.ptah.dev") | .clientConfig.service.name] | unique | if length == 1 then .[0] else empty end')
[ -n "$SERVICE" ] || fail "could not resolve the exact webhook Service"

DEPLOYMENT_JSON=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" get deployment "$DEPLOYMENT" -o json)
SECRET_NAME=$(printf '%s' "$DEPLOYMENT_JSON" |
	jq -r '.spec.template.spec.volumes[] | select(.name == "webhook-cert") | .secret.secretName')
MANAGER_SERVICE_ACCOUNT=$(printf '%s' "$DEPLOYMENT_JSON" | jq -r '.spec.template.spec.serviceAccountName')
if [ -z "$SECRET_NAME" ] || [ "$SECRET_NAME" = null ]; then
	fail "manager webhook Secret was not found"
fi
if [ -z "$MANAGER_SERVICE_ACCOUNT" ] || [ "$MANAGER_SERVICE_ACCOUNT" = null ]; then
	fail "manager ServiceAccount was not found"
fi

if [ "$(kubectl --kubeconfig "$KUBECONFIG_FILE" auth can-i \
	--as="system:serviceaccount:${OPERATOR_NAMESPACE}:${MANAGER_SERVICE_ACCOUNT}" \
	get "secret/${SECRET_NAME}" -n "$OPERATOR_NAMESPACE")" != no ]; then
	fail "manager ServiceAccount can read the webhook Secret"
fi

kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" scale \
	deployment "$DEPLOYMENT" --replicas=2 >/dev/null
kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" rollout status \
	deployment "$DEPLOYMENT" --timeout=5m >/dev/null

endpoint_deadline=$(($(date +%s) + 120))
ready_endpoints=0
while [ "$(date +%s)" -lt "$endpoint_deadline" ]; do
	ready_endpoints=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
		get endpointslices.discovery.k8s.io \
		-l "kubernetes.io/service-name=${SERVICE}" -o json |
		jq '[.items[].endpoints[] | select(.conditions.ready == true) | .addresses[]] | unique | length')
	[ "$ready_endpoints" -eq 2 ] && break
	sleep 1
done
[ "$ready_endpoints" -eq 2 ] || fail "webhook Service did not converge to two ready endpoint addresses"

kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" get secret "$SECRET_NAME" -o json |
	jq -e '.data["ca.key"] | type == "string" and length > 0' >/dev/null ||
	fail "generated webhook Secret has no CA private key"
OLD_CA=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get secret "$SECRET_NAME" -o jsonpath='{.data.ca\.crt}')
OLD_CERT=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get secret "$SECRET_NAME" -o jsonpath='{.data.tls\.crt}')
if [ -z "$OLD_CA" ] || [ "$OLD_CA" = null ]; then
	fail "generated webhook Secret has no CA certificate"
fi
if [ -z "$OLD_CERT" ] || [ "$OLD_CERT" = null ]; then
	fail "generated webhook Secret has no serving certificate"
fi

# A missing CA private key models a chart-generated Secret from an older
# release. Removing only that field leaves the currently served certificate
# valid while forcing the full two-phase CA path.
kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" patch secret "$SECRET_NAME" \
	--type=json -p='[{"op":"remove","path":"/data/ca.key"}]' >/dev/null

# Break every managed trust entry before replacing the rotator Pod. The
# all-Job operation-intent webhook remains fail-closed, but the rotator is
# ReplicaSet-owned and can therefore restart without crossing that rule.
BROKEN_CA_BUNDLE=$(printf '%s' 'not a certificate' | base64 | tr -d '\n')
kubectl --kubeconfig "$KUBECONFIG_FILE" get mutatingwebhookconfiguration "$MUTATING_CONFIGURATION" -o json |
	jq --arg bundle "$BROKEN_CA_BUNDLE" '
      (.webhooks[] | select(.name == "mapproval.operator.ptah.dev") | .clientConfig.caBundle) = $bundle
    ' |
	kubectl --kubeconfig "$KUBECONFIG_FILE" replace -f - >/dev/null
kubectl --kubeconfig "$KUBECONFIG_FILE" get validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" -o json |
	jq --arg bundle "$BROKEN_CA_BUNDLE" '
      (.webhooks[] | select(.name == "vapproval.operator.ptah.dev" or .name == "vpodintent.operator.ptah.dev") | .clientConfig.caBundle) = $bundle
    ' |
	kubectl --kubeconfig "$KUBECONFIG_FILE" replace -f - >/dev/null

OLD_ROTATOR_POD=$(resource_name pod certificate-rotation)
OLD_ROTATOR_UID=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get pod "$OLD_ROTATOR_POD" -o jsonpath='{.metadata.uid}')
kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" rollout restart \
	deployment "$ROTATOR_DEPLOYMENT" >/dev/null
if ! kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" rollout status \
	deployment "$ROTATOR_DEPLOYMENT" --timeout=5m >/dev/null; then
	kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" describe \
		deployment "$ROTATOR_DEPLOYMENT" >&2 || true
	fail "certificate rotator Deployment could not restart with broken webhook trust"
fi
NEW_ROTATOR_POD=$(resource_name pod certificate-rotation)
NEW_ROTATOR_UID=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get pod "$NEW_ROTATOR_POD" -o jsonpath='{.metadata.uid}')
[ "$NEW_ROTATOR_UID" != "$OLD_ROTATOR_UID" ] || fail "certificate rotator Pod was not replaced"

rotation_deadline=$(($(date +%s) + 660))
while [ "$(date +%s)" -lt "$rotation_deadline" ]; do
	NEW_CA=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
		get secret "$SECRET_NAME" -o jsonpath='{.data.ca\.crt}')
	if [ -n "$NEW_CA" ] && [ "$NEW_CA" != "$OLD_CA" ] && \
		kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" get secret "$SECRET_NAME" -o json |
		jq -e '.data["ca.key"] | type == "string" and length > 0' >/dev/null; then
		break
	fi
	sleep 2
done
if ! ROTATION_LOGS=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	logs "$NEW_ROTATOR_POD" 2>/dev/null); then
	kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" describe \
		deployment "$ROTATOR_DEPLOYMENT" >&2 || true
	fail "could not inspect certificate rotation logs"
fi
if printf '%s' "$ROTATION_LOGS" | grep -Eiq -- 'PRIVATE[ _-]?KEY|-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----'; then
	fail "certificate rotation logs contain private key material"
fi

kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" get secret "$SECRET_NAME" -o json |
	jq -e '.data["ca.key"] | type == "string" and length > 0' >/dev/null ||
	fail "certificate rotation did not restore the CA private key"
NEW_CA=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get secret "$SECRET_NAME" -o jsonpath='{.data.ca\.crt}')
NEW_CERT=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get secret "$SECRET_NAME" -o jsonpath='{.data.tls\.crt}')
[ "$NEW_CA" != "$OLD_CA" ] || fail "recovery rotation did not replace the CA"
[ "$NEW_CERT" != "$OLD_CERT" ] || fail "recovery rotation did not replace the serving certificate"

MUTATING_CA=$(kubectl --kubeconfig "$KUBECONFIG_FILE" get mutatingwebhookconfiguration \
	-l "app.kubernetes.io/instance=${HELM_RELEASE}" -o json |
	jq -r --arg service "$SERVICE" \
		'[.items[].webhooks[] | select(.name == "mapproval.operator.ptah.dev" and .clientConfig.service.name == $service) | .clientConfig.caBundle] | if length == 1 then .[0] else empty end')
VALIDATING_CA=$(kubectl --kubeconfig "$KUBECONFIG_FILE" get validatingwebhookconfiguration \
	-l "app.kubernetes.io/instance=${HELM_RELEASE}" -o json |
	jq -r --arg service "$SERVICE" \
		'[.items[].webhooks[] | select((.name == "vapproval.operator.ptah.dev" or .name == "vpodintent.operator.ptah.dev") and .clientConfig.service.name == $service) | .clientConfig.caBundle] | if length == 2 and (unique | length) == 1 then .[0] else empty end')
[ "$MUTATING_CA" = "$NEW_CA" ] || fail "mutating webhook trust did not contract to the replacement CA"
[ "$VALIDATING_CA" = "$NEW_CA" ] || fail "validating webhook trust did not contract to the replacement CA"

printf '%s\n' 'e2e certificate rotation: PASS broken-trust recovery across all managed webhook entries'
