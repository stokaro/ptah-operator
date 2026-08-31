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
ROTATOR_DEPLOYMENT_JSON=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get deployment "$ROTATOR_DEPLOYMENT" -o json)
ROTATOR_SERVICE_ACCOUNT=$(printf '%s' "$ROTATOR_DEPLOYMENT_JSON" | jq -r '.spec.template.spec.serviceAccountName')
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
if [ -z "$ROTATOR_SERVICE_ACCOUNT" ] || [ "$ROTATOR_SERVICE_ACCOUNT" = null ]; then
	fail "certificate rotator ServiceAccount was not found"
fi
printf '%s' "$ROTATOR_DEPLOYMENT_JSON" |
	jq -e '.spec.template.spec.containers[] | select(.name == "certificate-rotator") | .args | index("--recreate-missing-secret=true") != null' \
		>/dev/null ||
	fail "certificate rotation E2E requires explicit missing-Secret recreation opt-in"

if [ "$(kubectl --kubeconfig "$KUBECONFIG_FILE" auth can-i \
	--as="system:serviceaccount:${OPERATOR_NAMESPACE}:${MANAGER_SERVICE_ACCOUNT}" \
	get "secret/${SECRET_NAME}" -n "$OPERATOR_NAMESPACE")" != no ]; then
	fail "manager ServiceAccount can read the webhook Secret"
fi

GUARD_ERROR=$(mktemp "${TMPDIR:-/tmp}/ptah-operator-secret-guard.XXXXXX")
cleanup_guard_error() {
	rm -f -- "$GUARD_ERROR"
}
trap cleanup_guard_error EXIT
trap 'exit 130' HUP INT TERM
if kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	--as="system:serviceaccount:${OPERATOR_NAMESPACE}:${ROTATOR_SERVICE_ACCOUNT}" \
	create secret generic ptah-rotator-unauthorized \
	--from-literal=uncontrolled=value --dry-run=server -o name \
	>/dev/null 2>"$GUARD_ERROR"; then
	fail "certificate rotator ServiceAccount created an unrelated Secret"
fi
grep -F 'certificate rotator Secret CREATE is outside its exact recovery contract' \
	"$GUARD_ERROR" >/dev/null ||
	fail "unrelated Secret CREATE was not rejected by the exact recovery guard"

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

# Corrupt ca.crt while leaving the serving leaf and key intact. One malformed
# webhook entry proves that recovery filters candidates independently and can
# authenticate the live leaf from the remaining exact entries.
BROKEN_CA_BUNDLE=$(printf '%s' 'not a certificate' | base64 | tr -d '\n')
kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" patch secret "$SECRET_NAME" \
	--type=json -p="[{\"op\":\"replace\",\"path\":\"/data/ca.crt\",\"value\":\"${BROKEN_CA_BUNDLE}\"}]" >/dev/null
kubectl --kubeconfig "$KUBECONFIG_FILE" get validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" -o json |
	jq --arg bundle "$BROKEN_CA_BUNDLE" '
      (.webhooks[] | select(.name == "vapproval.operator.ptah.dev") | .clientConfig.caBundle) = $bundle
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
	fail "certificate rotator Deployment could not restart with corrupted CA state"
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
	fail "certificate rotation did not retain a valid CA private key"
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

# Delete the generated Secret entirely. This E2E release explicitly opts in to
# the namespace-wide RBAC CREATE verb, whose use by the rotator is constrained
# by the established exact-object policy tested above.
RECOVERED_SECRET_UID=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get secret "$SECRET_NAME" -o jsonpath='{.metadata.uid}')
kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	delete secret "$SECRET_NAME" --wait=true --timeout=60s >/dev/null
ROTATOR_POD_BEFORE_RECREATE=$(resource_name pod certificate-rotation)
ROTATOR_UID_BEFORE_RECREATE=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get pod "$ROTATOR_POD_BEFORE_RECREATE" -o jsonpath='{.metadata.uid}')
kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" rollout restart \
	deployment "$ROTATOR_DEPLOYMENT" >/dev/null
if ! kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" rollout status \
	deployment "$ROTATOR_DEPLOYMENT" --timeout=5m >/dev/null; then
	kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" describe \
		deployment "$ROTATOR_DEPLOYMENT" >&2 || true
	fail "certificate rotator Deployment could not restart with a missing TLS Secret"
fi
ROTATOR_POD_AFTER_RECREATE=$(resource_name pod certificate-rotation)
ROTATOR_UID_AFTER_RECREATE=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get pod "$ROTATOR_POD_AFTER_RECREATE" -o jsonpath='{.metadata.uid}')
[ "$ROTATOR_UID_AFTER_RECREATE" != "$ROTATOR_UID_BEFORE_RECREATE" ] ||
	fail "certificate rotator Pod was not replaced for missing-Secret recovery"

recreate_deadline=$(($(date +%s) + 660))
RECREATED_SECRET_JSON=
recreate_ready=0
while [ "$(date +%s)" -lt "$recreate_deadline" ]; do
	if RECREATED_SECRET_JSON=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
		get secret "$SECRET_NAME" -o json 2>/dev/null); then
		RECREATED_SECRET_UID=$(printf '%s' "$RECREATED_SECRET_JSON" | jq -r '.metadata.uid')
		RECREATED_CA=$(printf '%s' "$RECREATED_SECRET_JSON" | jq -r '.data["ca.crt"] // empty')
		RECREATED_CERT=$(printf '%s' "$RECREATED_SECRET_JSON" | jq -r '.data["tls.crt"] // empty')
		if [ -n "$RECREATED_SECRET_UID" ] && [ "$RECREATED_SECRET_UID" != "$RECOVERED_SECRET_UID" ] && \
			[ -n "$RECREATED_CA" ] && [ "$RECREATED_CA" != "$NEW_CA" ] && \
			[ -n "$RECREATED_CERT" ] && [ "$RECREATED_CERT" != "$NEW_CERT" ] && \
			printf '%s' "$RECREATED_SECRET_JSON" |
			jq -e --arg label 'operator.ptah.dev/generated-webhook-certificate' '
				.type == "kubernetes.io/tls" and
				.metadata.labels == {($label): "true"} and
				(.data | keys | sort) == ["ca.crt", "ca.key", "tls.crt", "tls.key"]
			' >/dev/null; then
			recreate_ready=1
			break
		fi
	fi
	sleep 2
done
[ "$recreate_ready" -eq 1 ] ||
	fail "certificate rotator did not recreate the deleted Secret with the exact recovery contract"

kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" rollout status \
	deployment "$DEPLOYMENT" --timeout=5m >/dev/null ||
	fail "manager Deployment did not remain ready after Secret recreation"
MUTATING_CA=$(kubectl --kubeconfig "$KUBECONFIG_FILE" get mutatingwebhookconfiguration \
	"$MUTATING_CONFIGURATION" -o json |
	jq -r --arg service "$SERVICE" \
		'[.webhooks[] | select(.name == "mapproval.operator.ptah.dev" and .clientConfig.service.name == $service) | .clientConfig.caBundle] | if length == 1 then .[0] else empty end')
VALIDATING_CA=$(kubectl --kubeconfig "$KUBECONFIG_FILE" get validatingwebhookconfiguration \
	"$VALIDATING_CONFIGURATION" -o json |
	jq -r --arg service "$SERVICE" \
		'[.webhooks[] | select((.name == "vapproval.operator.ptah.dev" or .name == "vpodintent.operator.ptah.dev") and .clientConfig.service.name == $service) | .clientConfig.caBundle] | if length == 2 and (unique | length) == 1 then .[0] else empty end')
[ "$MUTATING_CA" = "$RECREATED_CA" ] || fail "mutating webhook trust did not contract after Secret recreation"
[ "$VALIDATING_CA" = "$RECREATED_CA" ] || fail "validating webhook trust did not contract after Secret recreation"

if ! RECREATE_LOGS=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	logs "$ROTATOR_POD_AFTER_RECREATE" 2>/dev/null); then
	fail "could not inspect missing-Secret recovery logs"
fi
if printf '%s' "$RECREATE_LOGS" | grep -Eiq -- 'PRIVATE[ _-]?KEY|-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----'; then
	fail "missing-Secret recovery logs contain private key material"
fi

printf '%s\n' 'e2e certificate rotation: PASS corrupt-CA and deleted-Secret recovery with exact guarded creation'
