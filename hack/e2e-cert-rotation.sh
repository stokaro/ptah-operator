#!/bin/sh

set -eu

KUBECONFIG_FILE=${E2E_KUBECONFIG:?E2E_KUBECONFIG is required}
OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:?E2E_OPERATOR_NAMESPACE is required}
TEST_NAMESPACE=${E2E_TEST_NAMESPACE:?E2E_TEST_NAMESPACE is required}
HELM_RELEASE=${E2E_HELM_RELEASE:?E2E_HELM_RELEASE is required}
CHART_PACKAGE=${E2E_CHART_PACKAGE:?E2E_CHART_PACKAGE is required}

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

webhook_bundle() {
	kind=$1
	configuration=$2
	webhook_name=$3
	kubectl --kubeconfig "$KUBECONFIG_FILE" get "$kind" "$configuration" -o json |
		jq -r --arg name "$webhook_name" '
          [.webhooks[] | select(.name == $name) | .clientConfig.caBundle] |
          if length == 1 then .[0] else empty end
		'
}

uniform_service_bundle() {
	kind=$1
	configuration=$2
	kubectl --kubeconfig "$KUBECONFIG_FILE" get "$kind" "$configuration" -o json |
		jq -r --arg service "$SERVICE" '
          .webhooks as $webhooks |
          [$webhooks[] | select(.clientConfig.service.name == $service) | .clientConfig.caBundle] as $bundles |
          if ($webhooks | length) > 0 and
             ($bundles | length) == ($webhooks | length) and
             ($bundles | all(type == "string" and length > 0)) and
             ($bundles | unique | length) == 1
          then $bundles[0]
          else empty
          end
        '
}

generate_upgrade_ca() {
	name=$1
	key_file=$UPGRADE_WORK_DIR/${name}.key
	certificate_file=$UPGRADE_WORK_DIR/${name}.pem
	if ! openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
		-subj "/CN=ptah-e2e-${name}" \
		-addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
		-addext 'keyUsage=critical,keyCertSign,cRLSign' \
		-keyout "$key_file" -out "$certificate_file" >/dev/null 2>&1; then
		fail "could not generate the ${name} Helm-upgrade CA fixture"
	fi
	rm -f -- "$key_file"
	if ! openssl verify -CAfile "$certificate_file" "$certificate_file" >/dev/null 2>&1; then
		fail "the ${name} Helm-upgrade CA fixture is not a valid self-signed root"
	fi
	printf '%s\n' "$certificate_file"
}

build_overlap_bundle() {
	name=$1
	certificate_file=$2
	bundle_file=$UPGRADE_WORK_DIR/${name}-overlap.pem
	if ! {
		printf '%s' "$OLD_CA" | openssl base64 -d -A
		printf '\n'
		cat "$certificate_file"
	} >"$bundle_file"; then
		fail "could not build the ${name} Helm-upgrade CA overlap"
	fi
	if [ "$(grep -c '^-----BEGIN CERTIFICATE-----$' "$bundle_file")" -ne 2 ] ||
		! openssl crl2pkcs7 -nocrl -certfile "$bundle_file" >/dev/null 2>&1; then
		fail "the ${name} Helm-upgrade CA overlap is not exactly two valid certificates"
	fi
	openssl base64 -A -in "$bundle_file"
}

assert_entry_bundle() {
	kind=$1
	configuration=$2
	webhook_name=$3
	entry_certificate=$4
	shift 4
	observed_bundle=$(webhook_bundle "$kind" "$configuration" "$webhook_name")
	[ -n "$observed_bundle" ] || fail "could not read caBundle for ${webhook_name}"
	observed_file=$UPGRADE_WORK_DIR/observed-${webhook_name}.pem
	if ! printf '%s' "$observed_bundle" | openssl base64 -d -A >"$observed_file"; then
		fail "caBundle for ${webhook_name} is not valid base64"
	fi
	if [ "$(grep -c '^-----BEGIN CERTIFICATE-----$' "$observed_file")" -ne 2 ] ||
		! openssl crl2pkcs7 -nocrl -certfile "$observed_file" >/dev/null 2>&1; then
		fail "caBundle for ${webhook_name} is not exactly two valid certificates"
	fi
	if ! openssl verify -CAfile "$observed_file" "$CURRENT_CA_FILE" >/dev/null 2>&1; then
		fail "caBundle for ${webhook_name} dropped the current serving root"
	fi
	if ! openssl verify -CAfile "$observed_file" "$entry_certificate" >/dev/null 2>&1; then
		fail "caBundle for ${webhook_name} dropped its entry-local root"
	fi
	for foreign_certificate in "$@"; do
		if openssl verify -CAfile "$observed_file" "$foreign_certificate" >/dev/null 2>&1; then
			fail "caBundle for ${webhook_name} gained another entry's root"
		fi
	done
}

assert_approval_admission_callable() {
	stage=$1
	if ! kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$TEST_NAMESPACE" \
		patch ptahschemaapproval e2e-approval --type=merge \
		-p '{"metadata":{"annotations":{"operator.ptah.dev/certificate-upgrade-probe":"true"}}}' \
		--dry-run=server -o name >/dev/null; then
		fail "approval admission was not callable ${stage}"
	fi
}

[ -f "$KUBECONFIG_FILE" ] || fail "E2E_KUBECONFIG does not name a file"
[ -f "$CHART_PACKAGE" ] || fail "E2E_CHART_PACKAGE does not name the packaged chart"
command -v openssl >/dev/null 2>&1 || fail "OpenSSL is required"

UPGRADE_WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-cert-upgrade.XXXXXX")
chmod 700 "$UPGRADE_WORK_DIR"
cleanup_upgrade_files() {
	status=$?
	trap - EXIT HUP INT TERM
	case "$UPGRADE_WORK_DIR" in
	"${TMPDIR:-/tmp}"/ptah-operator-cert-upgrade.*) rm -rf -- "$UPGRADE_WORK_DIR" ;;
	*)
		printf 'e2e certificate rotation: refusing to remove unexpected work directory %s\n' \
			"$UPGRADE_WORK_DIR" >&2
		status=1
		;;
	esac
	exit "$status"
}
trap cleanup_upgrade_files EXIT
trap 'exit 130' HUP INT TERM

DEPLOYMENT=$(resource_name deployment controller)
ROTATOR_DEPLOYMENT=$(resource_name deployment certificate-rotation)
ROTATOR_DEPLOYMENT_JSON=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get deployment "$ROTATOR_DEPLOYMENT" -o json)
ROTATOR_SERVICE_ACCOUNT=$(printf '%s' "$ROTATOR_DEPLOYMENT_JSON" | jq -r '.spec.template.spec.serviceAccountName')
ROTATOR_POD=$(resource_name pod certificate-rotation)
ROTATOR_POD_JSON=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get pod "$ROTATOR_POD" -o json)
ROTATOR_SERVICE_ACCOUNT_UID=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get serviceaccount "$ROTATOR_SERVICE_ACCOUNT" -o jsonpath='{.metadata.uid}')
ROTATOR_POD_UID=$(printf '%s' "$ROTATOR_POD_JSON" | jq -r '.metadata.uid')
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
if [ -z "$ROTATOR_SERVICE_ACCOUNT_UID" ] || [ -z "$ROTATOR_POD" ] || [ -z "$ROTATOR_POD_UID" ] ||
	[ "$ROTATOR_POD_UID" = null ]; then
	fail "certificate rotator workload-bound identity was not found"
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

rotator_kube() {
	kubectl --kubeconfig "$KUBECONFIG_FILE" \
		--as="system:serviceaccount:${OPERATOR_NAMESPACE}:${ROTATOR_SERVICE_ACCOUNT}" \
		--as-uid="$ROTATOR_SERVICE_ACCOUNT_UID" \
		--as-group=system:serviceaccounts \
		--as-group="system:serviceaccounts:${OPERATOR_NAMESPACE}" \
		--as-group=system:authenticated \
		--as-user-extra="authentication.kubernetes.io/pod-name=${ROTATOR_POD}" \
		--as-user-extra="authentication.kubernetes.io/pod-uid=${ROTATOR_POD_UID}" \
		"$@"
}

CERTIFICATE_WRITE_PROBE_INDEX=0
expect_certificate_write_denial() {
	resource=$1
	description=$2
	filter=$3
	denial=$4
	CERTIFICATE_WRITE_PROBE_INDEX=$((CERTIFICATE_WRITE_PROBE_INDEX + 1))
	source=$UPGRADE_WORK_DIR/certificate-write-${CERTIFICATE_WRITE_PROBE_INDEX}-source.json
	candidate=$UPGRADE_WORK_DIR/certificate-write-${CERTIFICATE_WRITE_PROBE_INDEX}-candidate.json
	error_file=$UPGRADE_WORK_DIR/certificate-write-${CERTIFICATE_WRITE_PROBE_INDEX}.err
	kubectl --kubeconfig "$KUBECONFIG_FILE" get "$resource" ptah-operator-admission -o json >"$source"
	jq "$filter" "$source" >"$candidate"
	if rotator_kube replace --field-manager='' --dry-run=server -f "$candidate" \
		>/dev/null 2>"$error_file"; then
		fail "certificate write guard accepted ${description}"
	fi
	grep -F "$denial" "$error_file" >/dev/null ||
		fail "${description} was not rejected by the typed certificate write guard"
}

prove_certificate_write_guards() {
	proof_bundle=$(printf '%s' 'certificate-write-boundary-proof' | base64 | tr -d '\n')
	for resource in mutatingwebhookconfiguration validatingwebhookconfiguration; do
		source=$UPGRADE_WORK_DIR/${resource}-ca-source.json
		candidate=$UPGRADE_WORK_DIR/${resource}-ca-candidate.json
		kubectl --kubeconfig "$KUBECONFIG_FILE" get "$resource" ptah-operator-admission -o json >"$source"
		jq --arg bundle "$proof_bundle" '(.webhooks[].clientConfig.caBundle) = $bundle' \
			"$source" >"$candidate"
		rotator_kube replace --field-manager='' --dry-run=server -f "$candidate" >/dev/null ||
			fail "certificate write guard rejected a bounded CA-only ${resource} update"
	done
	expect_certificate_write_denial mutatingwebhookconfiguration \
		'a mutating reinvocationPolicy change' \
		'.webhooks[0].reinvocationPolicy = "IfNeeded"' \
		'Ptah certificate mutating write guard rejected an unsafe mutation'
	expect_certificate_write_denial validatingwebhookconfiguration \
		'a validating failurePolicy change' \
		'.webhooks[0].failurePolicy = "Ignore"' \
		'Ptah certificate validating write guard rejected an unsafe mutation'
	expect_certificate_write_denial validatingwebhookconfiguration \
		'a validating metadata annotation change' \
		'.metadata.annotations["operator.ptah.dev/certificate-write-e2e"] = "changed"' \
		'Ptah certificate validating write guard rejected an unsafe mutation'
	expect_certificate_write_denial validatingwebhookconfiguration \
		'an empty validating caBundle' \
		'.webhooks[0].clientConfig.caBundle = ""' \
		'Ptah certificate validating write guard rejected an unsafe mutation'
	# shellcheck disable=SC2016 # jq binds $first; the shell must not expand it.
	expect_certificate_write_denial validatingwebhookconfiguration \
		'a validating webhook reorder' \
		'.webhooks[0] as $first | .webhooks[0] = .webhooks[1] | .webhooks[1] = $first' \
		'Ptah certificate validating write guard rejected an unsafe mutation'
	printf '%s\n' 'e2e certificate rotation: typed certificate write boundary proof passed'
}

prove_certificate_write_guards

GUARD_ERROR=$UPGRADE_WORK_DIR/secret-guard.err
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

kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" patch \
	deployment "$DEPLOYMENT" --type=merge -p '{"spec":{"replicas":2}}' >/dev/null
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

# Exercise Helm's live lookup path while the generated Secret exists. Every
# managed entry begins with the serving root plus a distinct valid local root;
# the upgrade must preserve that exact trust partition without cross-copying.
CURRENT_CA_FILE=$UPGRADE_WORK_DIR/current-ca.pem
if ! printf '%s' "$OLD_CA" | openssl base64 -d -A >"$CURRENT_CA_FILE" ||
	! openssl verify -CAfile "$CURRENT_CA_FILE" "$CURRENT_CA_FILE" >/dev/null 2>&1; then
	fail "generated webhook Secret ca.crt is not a valid self-signed root"
fi
MUTATING_UPGRADE_CA=$(generate_upgrade_ca mutating)
APPROVAL_UPGRADE_CA=$(generate_upgrade_ca approval-validating)
POD_UPGRADE_CA=$(generate_upgrade_ca pod-validating)
CONTROLLER_WRITE_UPGRADE_CA=$(generate_upgrade_ca controller-write-validating)
MUTATING_OVERLAP=$(build_overlap_bundle mutating "$MUTATING_UPGRADE_CA")
APPROVAL_OVERLAP=$(build_overlap_bundle approval-validating "$APPROVAL_UPGRADE_CA")
POD_OVERLAP=$(build_overlap_bundle pod-validating "$POD_UPGRADE_CA")
CONTROLLER_WRITE_OVERLAP=$(build_overlap_bundle controller-write-validating "$CONTROLLER_WRITE_UPGRADE_CA")

kubectl --kubeconfig "$KUBECONFIG_FILE" get \
	mutatingwebhookconfiguration "$MUTATING_CONFIGURATION" -o json |
	jq --arg bundle "$MUTATING_OVERLAP" '
      (.webhooks[] | select(.name == "mapproval.operator.ptah.dev") | .clientConfig.caBundle) = $bundle
    ' |
	kubectl --kubeconfig "$KUBECONFIG_FILE" replace -f - >/dev/null
kubectl --kubeconfig "$KUBECONFIG_FILE" get \
	validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" -o json |
	jq --arg approval "$APPROVAL_OVERLAP" --arg pod "$POD_OVERLAP" --arg controller "$CONTROLLER_WRITE_OVERLAP" '
	      (.webhooks[] | select(.name == "vapproval.operator.ptah.dev") | .clientConfig.caBundle) = $approval |
	      (.webhooks[] | select(.name == "vpodintent.operator.ptah.dev") | .clientConfig.caBundle) = $pod |
	      (.webhooks[] | select(.name == "vcontrollerwrite.operator.ptah.dev") | .clientConfig.caBundle) = $controller
    ' |
	kubectl --kubeconfig "$KUBECONFIG_FILE" replace -f - >/dev/null

assert_entry_bundle mutatingwebhookconfiguration "$MUTATING_CONFIGURATION" \
	mapproval.operator.ptah.dev "$MUTATING_UPGRADE_CA" "$APPROVAL_UPGRADE_CA" "$POD_UPGRADE_CA" "$CONTROLLER_WRITE_UPGRADE_CA"
assert_entry_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" \
	vapproval.operator.ptah.dev "$APPROVAL_UPGRADE_CA" "$MUTATING_UPGRADE_CA" "$POD_UPGRADE_CA" "$CONTROLLER_WRITE_UPGRADE_CA"
assert_entry_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" \
	vpodintent.operator.ptah.dev "$POD_UPGRADE_CA" "$MUTATING_UPGRADE_CA" "$APPROVAL_UPGRADE_CA" "$CONTROLLER_WRITE_UPGRADE_CA"
assert_entry_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" \
	vcontrollerwrite.operator.ptah.dev "$CONTROLLER_WRITE_UPGRADE_CA" "$MUTATING_UPGRADE_CA" "$APPROVAL_UPGRADE_CA" "$POD_UPGRADE_CA"
assert_approval_admission_callable "before the Helm upgrade"

if ! helm --kubeconfig "$KUBECONFIG_FILE" upgrade "$HELM_RELEASE" "$CHART_PACKAGE" \
	--namespace "$OPERATOR_NAMESPACE" --reuse-values --wait --timeout 5m >/dev/null; then
	fail "packaged-chart upgrade failed during live certificate lookup"
fi

assert_entry_bundle mutatingwebhookconfiguration "$MUTATING_CONFIGURATION" \
	mapproval.operator.ptah.dev "$MUTATING_UPGRADE_CA" "$APPROVAL_UPGRADE_CA" "$POD_UPGRADE_CA" "$CONTROLLER_WRITE_UPGRADE_CA"
assert_entry_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" \
	vapproval.operator.ptah.dev "$APPROVAL_UPGRADE_CA" "$MUTATING_UPGRADE_CA" "$POD_UPGRADE_CA" "$CONTROLLER_WRITE_UPGRADE_CA"
assert_entry_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" \
	vpodintent.operator.ptah.dev "$POD_UPGRADE_CA" "$MUTATING_UPGRADE_CA" "$APPROVAL_UPGRADE_CA" "$CONTROLLER_WRITE_UPGRADE_CA"
assert_entry_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" \
	vcontrollerwrite.operator.ptah.dev "$CONTROLLER_WRITE_UPGRADE_CA" "$MUTATING_UPGRADE_CA" "$APPROVAL_UPGRADE_CA" "$POD_UPGRADE_CA"
assert_approval_admission_callable "after the Helm upgrade"

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

MUTATING_CA=$(uniform_service_bundle mutatingwebhookconfiguration "$MUTATING_CONFIGURATION")
VALIDATING_CA=$(uniform_service_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION")
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
MUTATING_CA=$(uniform_service_bundle mutatingwebhookconfiguration "$MUTATING_CONFIGURATION")
VALIDATING_CA=$(uniform_service_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION")
[ "$MUTATING_CA" = "$RECREATED_CA" ] || fail "mutating webhook trust did not contract after Secret recreation"
[ "$VALIDATING_CA" = "$RECREATED_CA" ] || fail "validating webhook trust did not contract after Secret recreation"

if ! RECREATE_LOGS=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	logs "$ROTATOR_POD_AFTER_RECREATE" 2>/dev/null); then
	fail "could not inspect missing-Secret recovery logs"
fi
if printf '%s' "$RECREATE_LOGS" | grep -Eiq -- 'PRIVATE[ _-]?KEY|-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----'; then
	fail "missing-Secret recovery logs contain private key material"
fi

printf '%s\n' 'e2e certificate rotation: PASS live Helm lookup, corrupt-CA recovery, and exact guarded recreation'
