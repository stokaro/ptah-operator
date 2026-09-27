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

KUBECONFIG_FILE=${E2E_KUBECONFIG:?E2E_KUBECONFIG is required}
OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:?E2E_OPERATOR_NAMESPACE is required}
TEST_NAMESPACE=${E2E_TEST_NAMESPACE:?E2E_TEST_NAMESPACE is required}
HELM_RELEASE=${E2E_HELM_RELEASE:?E2E_HELM_RELEASE is required}
CHART_PACKAGE=${E2E_CHART_PACKAGE:?E2E_CHART_PACKAGE is required}

# A command that fails outside a guard calling fail ends the shell with no
# reason printed, and the EXIT trap then dumps diagnostics that explain
# nothing. So fail records that it spoke, in a file rather than a variable
# so that a fail inside a subshell still counts, and the trap says so when
# nothing did.
PHASE_REASON_MARKER=${TMPDIR:-/tmp}/ptah-e2e-reason-cert-rotation.$$

fail() {
	printf 'e2e certificate rotation: %s\n' "$*" >&2
	# Tolerant of an unset marker: this function is also extracted and run on
	# its own by hack/e2e_cert_rotation_test.go, and a reporting helper that
	# fails is worse than one that reports nothing.
	if [ -n "${PHASE_REASON_MARKER:-}" ]; then
		: >"$PHASE_REASON_MARKER" 2>/dev/null || true
	fi
	exit 1
}

# A Pod deletion leaves the replaced Pod terminating while its replacement is
# already available, so a rollout that returns still has two Pods carrying the
# component label for a moment. Ask for the live one and wait for the other to
# go, rather than reading a count that is briefly two.
live_pod_name() {
	live_component=$1
	live_deadline=$(($(date +%s) + 180))
	while :; do
		live_name=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
			get pods \
			-l "app.kubernetes.io/instance=${HELM_RELEASE},app.kubernetes.io/component=${live_component}" \
			-o json |
			jq -r '
              [.items[] |
                select(.metadata.deletionTimestamp == null) |
                select(.status.conditions // [] |
                  any(.type == "Ready" and .status == "True")) |
                .metadata.name] |
              if length == 1 then .[0] else "" end
            ')
		if [ -n "$live_name" ]; then
			printf '%s\n' "$live_name"
			return 0
		fi
		[ "$(date +%s)" -lt "$live_deadline" ] ||
			fail "expected exactly one live ready pod for component ${live_component}"
		sleep 2
	done
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

# uniform_service_bundle prints the one caBundle every entry targeting the
# webhook Service carries, and nothing unless those entries are exactly the
# rotator's managed inventory. The two dormant certificate-rotation canary
# entries target another Service; the rotator no longer maintains them, so
# they are outside this check.
uniform_service_bundle() {
	kind=$1
	configuration=$2
	kubectl --kubeconfig "$KUBECONFIG_FILE" get "$kind" "$configuration" -o json |
		jq -r --arg kind "$kind" --arg service "$SERVICE" --arg namespace "$OPERATOR_NAMESPACE" '
          (if $kind == "mutatingwebhookconfiguration" then {
            "mapproval.operator.ptah.run": [$service, "/mutate-operator-ptah-run-v1alpha1-ptahschemaapproval"],
            "mmigrationapproval.operator.ptah.run": [$service, "/mutate-operator-ptah-run-v1alpha1-ptahmigrationapproval"]
          } else {
            "vapproval.operator.ptah.run": [$service, "/validate-operator-ptah-run-v1alpha1-ptahschemaapproval"],
            "vmigrationapproval.operator.ptah.run": [$service, "/validate-operator-ptah-run-v1alpha1-ptahmigrationapproval"],
            "vpodintent.operator.ptah.run": [$service, "/validate-v1-pod-ptah-operation-intent"],
            "vcontrollerwrite.operator.ptah.run": [$service, "/validate-operator-controller-write"]
          } end) as $expected |
          [.webhooks[] | select(.clientConfig.service.name == $service)] as $webhooks |
          [$webhooks[].clientConfig.caBundle] as $bundles |
          if ([$webhooks[].name] | sort) == ($expected | keys) and
             ($webhooks | all(.[];
               .clientConfig.url == null and
               .clientConfig.service.namespace == $namespace and
               .clientConfig.service.name == $expected[.name][0] and
               .clientConfig.service.path == $expected[.name][1] and
               .clientConfig.service.port == 443)) and
             ($bundles | all(type == "string" and length > 0)) and
             ($bundles | unique | length) == 1
          then $bundles[0]
          else empty
          end
        '
}

rotation_transition_complete() {
	expected_ca=$1
	[ "$(uniform_service_bundle mutatingwebhookconfiguration "$MUTATING_CONFIGURATION")" = "$expected_ca" ] &&
		[ "$(uniform_service_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION")" = "$expected_ca" ] &&
		kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
			get secret "$STAGING_SECRET_NAME" -o json |
		jq -e --arg name "$STAGING_SECRET_NAME" --arg namespace "$OPERATOR_NAMESPACE" \
			--arg release "$HELM_RELEASE" '
			.type == "Opaque" and .metadata.name == $name and
			.metadata.namespace == $namespace and
			.metadata.labels == {
				"app.kubernetes.io/managed-by": "Helm",
				"operator.ptah.run/certificate-rotation-staging": "true"
			} and
			.metadata.annotations == {
				"meta.helm.sh/release-name": $release,
				"meta.helm.sh/release-namespace": $namespace
			} and
			(.data // {}) == {}
		' >/dev/null
}

# ca_switch_delay_seconds prints the rotator's --ca-switch-delay in seconds.
# The ordering proofs measure the switch against it, so a value this harness
# cannot read as whole seconds fails rather than being guessed at.
ca_switch_delay_seconds() {
	switch_delay=$(printf '%s' "$ROTATOR_DEPLOYMENT_JSON" |
		jq -r '[.spec.template.spec.containers[] | select(.name == "certificate-rotator") |
			.args[] | select(startswith("--ca-switch-delay=")) | ltrimstr("--ca-switch-delay=")] |
			if length == 1 then .[0] else empty end')
	printf '%s\n' "$switch_delay" | grep -Eq '^[1-9][0-9]*s$' ||
		fail "the rotator's --ca-switch-delay is not one whole number of seconds: ${switch_delay:-absent}"
	printf '%s\n' "${switch_delay%s}"
}

# expanded_transition_time prints the expansion time from a staging Secret
# whose record is in its expanded phase, and fails for any other staging
# state. It reads the record's public fields only.
expanded_transition_time() {
	jq -e -r --arg name "$STAGING_SECRET_NAME" --arg namespace "$OPERATOR_NAMESPACE" '
		select(.kind == "Secret" and .metadata.name == $name and .metadata.namespace == $namespace) |
		(.data // {}) as $data |
		select((($data.format // "") | @base64d) == "v3" and (($data.phase // "") | @base64d) == "expanded") |
		(($data["expanded-at"] // "") | @base64d) |
		select(test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))
	' "$1"
}

# rotator_certificate_write_time prints when the rotator last changed the
# generated Secret's serving certificate. The time comes from the Secret's own
# field management, not from when this harness looked.
rotator_certificate_write_time() {
	jq -e -r '
		[.metadata.managedFields[]? |
			select(.manager == "ptah-cert-rotator" and .operation == "Update" and
				(.subresource // "") == "" and
				(((.fieldsV1 // {})["f:data"] // {}) | has("f:tls.crt")))] |
		if length == 1 then .[0].time else empty end
	' "$1"
}

# secret_field_bytes writes one data field of a Secret document to stdout
# exactly as the Secret stores it. `jq -r` would append a newline, so a PEM
# field decoded that way never compares equal to the same field decoded from
# another Secret with `openssl base64 -d`.
secret_field_bytes() {
	jq -j --arg key "$2" '.data[$key] // "" | @base64d' "$1"
}

# switched_after_delay succeeds when the switch instant lies at least the
# delay, in seconds, after the expansion instant.
switched_after_delay() {
	jq -n -e --arg switched "$1" --arg expanded "$2" --argjson delay "$3" '
		($switched | fromdateiso8601) >= ($expanded | fromdateiso8601) + $delay
	' >/dev/null
}

# rotator_container_started_at prints when a Pod's certificate-rotator
# container last started, from the Pod's own status.
rotator_container_started_at() {
	jq -e -r '
		[.status.containerStatuses[]? | select(.name == "certificate-rotator") |
			.state.running.startedAt // empty] |
		if length == 1 then .[0] else empty end
	' "$1"
}

# primary_secret_state prints the generated Secret's ca.crt and
# resourceVersion, or "absent" when the Secret does not exist.
primary_secret_state() {
	if kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
		get secret "$SECRET_NAME" -o json >"$PRIMARY_OBSERVATION" 2>"$PRIMARY_OBSERVATION_ERROR"; then
		jq -r '(.data["ca.crt"] // "") + " " + .metadata.resourceVersion' "$PRIMARY_OBSERVATION"
		return 0
	fi
	if grep -Fq '(NotFound)' "$PRIMARY_OBSERVATION_ERROR"; then
		printf '%s\n' absent
		return 0
	fi
	fail "could not read the generated Secret: $(cat "$PRIMARY_OBSERVATION_ERROR")"
}

# bundle_contains_certificate succeeds when the PEM bundle holds a certificate
# byte-identical to the given one. It compares fingerprints rather than asking
# `openssl verify`: every CA the rotator issues for a Service has the same
# subject, and verify picks an issuer by subject, so with the old and the new
# CA in one bundle it tries only the first and rejects the second.
bundle_contains_certificate() {
	contains_bundle=$1
	contains_wanted=$(openssl x509 -in "$2" -noout -fingerprint -sha256 2>/dev/null) || return 1
	contains_directory=$(mktemp -d "$UPGRADE_WORK_DIR/bundle-members.XXXXXX") || return 1
	awk -v directory="$contains_directory" '
		/^-----BEGIN CERTIFICATE-----$/ { count++; member = sprintf("%s/%d.pem", directory, count) }
		member != "" { print > member }
		/^-----END CERTIFICATE-----$/ { close(member); member = "" }
	' "$contains_bundle" || return 1
	contains_found=1
	for contains_member in "$contains_directory"/*.pem; do
		[ -f "$contains_member" ] || continue
		if [ "$(openssl x509 -in "$contains_member" -noout -fingerprint -sha256 2>/dev/null)" = "$contains_wanted" ]; then
			contains_found=0
		fi
	done
	rm -rf -- "$contains_directory"
	return "$contains_found"
}

# assert_entry_trusts fails unless the entry's caBundle holds every given
# certificate file.
assert_entry_trusts() {
	trust_kind=$1
	trust_configuration=$2
	trust_webhook=$3
	shift 3
	trust_bundle=$(webhook_bundle "$trust_kind" "$trust_configuration" "$trust_webhook")
	[ -n "$trust_bundle" ] || fail "could not read caBundle for ${trust_webhook}"
	trust_file=$UPGRADE_WORK_DIR/trust-${trust_webhook}.pem
	printf '%s' "$trust_bundle" | openssl base64 -d -A >"$trust_file" ||
		fail "caBundle for ${trust_webhook} is not valid base64"
	for trusted_certificate in "$@"; do
		bundle_contains_certificate "$trust_file" "$trusted_certificate" ||
			fail "caBundle for ${trust_webhook} does not trust ${trusted_certificate##*/}"
	done
}

# observe_expanded_trust waits for the rotator to record its expansion, then
# proves that every managed entry trusted both the CA behind the certificate
# being served and the staged CA while the generated Secret still held its
# pre-switch state. Reading the Secret before and after the entries brackets
# that read: the old CA leaves an entry only after the Secret switches. It
# sets EXPANDED_AT and writes the staged CA and serving certificate to
# STAGED_CA_FILE and STAGED_CERT_FILE.
observe_expanded_trust() {
	observed_stage=$1
	serving_ca_file=$2
	pre_switch_ca=$3
	observe_deadline=$(($(date +%s) + 120))
	EXPANDED_AT=
	while [ "$(date +%s)" -lt "$observe_deadline" ]; do
		kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
			get secret "$STAGING_SECRET_NAME" -o json >"$STAGING_OBSERVATION" ||
			fail "${observed_stage}: could not read the staging Secret"
		if EXPANDED_AT=$(expanded_transition_time "$STAGING_OBSERVATION"); then
			break
		fi
		EXPANDED_AT=
		sleep 1
	done
	[ -n "$EXPANDED_AT" ] ||
		fail "${observed_stage}: the staging Secret held no expanded CA transition within 120 seconds"
	secret_field_bytes "$STAGING_OBSERVATION" candidate.ca.crt >"$STAGED_CA_FILE" ||
		fail "${observed_stage}: could not read the staged CA"
	openssl verify -CAfile "$STAGED_CA_FILE" "$STAGED_CA_FILE" >/dev/null 2>&1 ||
		fail "${observed_stage}: the staged CA is not a valid self-signed root"
	secret_field_bytes "$STAGING_OBSERVATION" candidate.tls.crt >"$STAGED_CERT_FILE" ||
		fail "${observed_stage}: could not read the staged serving certificate"
	openssl verify -CAfile "$STAGED_CA_FILE" "$STAGED_CERT_FILE" >/dev/null 2>&1 ||
		fail "${observed_stage}: the staged serving certificate is not issued by the staged CA"

	before_state=$(primary_secret_state)
	[ "${before_state%% *}" = "$pre_switch_ca" ] ||
		fail "${observed_stage}: the generated Secret switched before its expansion could be read"
	while read -r entry_kind entry_configuration entry_name; do
		assert_entry_trusts "$entry_kind" "$entry_configuration" "$entry_name" \
			"$serving_ca_file" "$STAGED_CA_FILE"
	done <<EOF
mutatingwebhookconfiguration $MUTATING_CONFIGURATION mapproval.operator.ptah.run
mutatingwebhookconfiguration $MUTATING_CONFIGURATION mmigrationapproval.operator.ptah.run
validatingwebhookconfiguration $VALIDATING_CONFIGURATION vapproval.operator.ptah.run
validatingwebhookconfiguration $VALIDATING_CONFIGURATION vmigrationapproval.operator.ptah.run
validatingwebhookconfiguration $VALIDATING_CONFIGURATION vpodintent.operator.ptah.run
validatingwebhookconfiguration $VALIDATING_CONFIGURATION vcontrollerwrite.operator.ptah.run
EOF
	after_state=$(primary_secret_state)
	[ "$after_state" = "$before_state" ] ||
		fail "${observed_stage}: the generated Secret switched while the expanded trust was read, so the read proves nothing"
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
		-p '{"metadata":{"annotations":{"operator.ptah.run/certificate-upgrade-probe":"true"}}}' \
		--dry-run=server -o name >/dev/null; then
		fail "approval admission was not callable ${stage}"
	fi
}

[ -f "$KUBECONFIG_FILE" ] || fail "E2E_KUBECONFIG does not name a file"
[ -f "$CHART_PACKAGE" ] || fail "E2E_CHART_PACKAGE does not name the packaged chart"
command -v openssl >/dev/null 2>&1 || fail "OpenSSL is required"

umask 077
UPGRADE_WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-cert-upgrade.XXXXXX")
chmod 700 "$UPGRADE_WORK_DIR"
SECRET_BEFORE=$UPGRADE_WORK_DIR/secret-before.json
SECRET_ERROR=$UPGRADE_WORK_DIR/secret-error.log

validate_generated_secret() {
	jq -e --arg name "$SECRET_NAME" --arg namespace "$OPERATOR_NAMESPACE" \
		--arg release "$HELM_RELEASE" '
		.apiVersion == "v1" and .kind == "Secret" and
		.type == "kubernetes.io/tls" and
		.metadata.name == $name and .metadata.namespace == $namespace and
		(.metadata.generateName // "") == "" and
		(.metadata.uid | type == "string" and length > 0) and
		(.metadata.resourceVersion | type == "string" and length > 0) and
		.metadata.labels == {
			"app.kubernetes.io/managed-by": "Helm",
			"operator.ptah.run/generated-webhook-certificate": "true"
		} and
		.metadata.annotations == {
			"meta.helm.sh/release-name": $release,
			"meta.helm.sh/release-namespace": $namespace
		} and
		(.metadata.ownerReferences // []) == [] and
		(.metadata.finalizers // []) == [] and
		.metadata.deletionTimestamp == null and
		.immutable == null and (.stringData // {}) == {} and
		(.data | type == "object") and
		(.data | keys) == ["ca.crt", "ca.key", "tls.crt", "tls.key"] and
		(.data | all(.[]; type == "string" and length > 0))
	' "$1" >/dev/null 2>"$SECRET_ERROR"
}

# A refused parameter expansion (${VAR:?...}) or an unset name under set -u
# ends the shell without setting $?, so an EXIT trap that reports $? reads the
# previous command's success and a script that never finished reports a pass.
# The latch is set where the script reaches its own end; the trap trusts it.
PHASE_COMPLETED=0
cleanup_upgrade_files() {
	status=$?
	[ "$status" -ne 0 ] || [ "$PHASE_COMPLETED" -eq 1 ] || status=1
	trap - EXIT HUP INT TERM
	# The marker is unset when this handler is extracted and run on its own
	# by hack/e2e_cert_rotation_test.go; there is nothing to report then.
	if [ -n "${PHASE_REASON_MARKER:-}" ]; then
		if [ "$status" -ne 0 ] && [ ! -f "$PHASE_REASON_MARKER" ]; then
			printf 'e2e certificate rotation: exited with status %s at a command that failed under set -e; no proof reported a reason\n' "$status" >&2
		fi
		rm -f -- "$PHASE_REASON_MARKER"
	fi
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
STAGING_SECRET_NAME=$(printf '%s' "$ROTATOR_DEPLOYMENT_JSON" |
	jq -r '[.spec.template.spec.containers[] | select(.name == "certificate-rotator") |
		.args[] | select(startswith("--staging-secret-name=")) | ltrimstr("--staging-secret-name=")] |
		if length == 1 then .[0] else empty end')
[ -n "$STAGING_SECRET_NAME" ] || fail "could not resolve the exact certificate staging Secret"
ROTATOR_SERVICE_ACCOUNT=$(printf '%s' "$ROTATOR_DEPLOYMENT_JSON" | jq -r '.spec.template.spec.serviceAccountName')
ROTATOR_POD=$(live_pod_name certificate-rotation)
ROTATOR_POD_JSON=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get pod "$ROTATOR_POD" -o json)
ROTATOR_SERVICE_ACCOUNT_UID=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get serviceaccount "$ROTATOR_SERVICE_ACCOUNT" -o jsonpath='{.metadata.uid}')
ROTATOR_POD_UID=$(printf '%s' "$ROTATOR_POD_JSON" | jq -r '.metadata.uid')
MUTATING_CONFIGURATION=$(resource_name mutatingwebhookconfiguration "")
VALIDATING_CONFIGURATION=$(resource_name validatingwebhookconfiguration "")
SERVICE=$(kubectl --kubeconfig "$KUBECONFIG_FILE" get \
	mutatingwebhookconfiguration "$MUTATING_CONFIGURATION" -o json |
	jq -r '[.webhooks[] | select(.name == "mapproval.operator.ptah.run") | .clientConfig.service.name] | unique | if length == 1 then .[0] else empty end')
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
		'.metadata.annotations["operator.ptah.run/certificate-write-e2e"] = "changed"' \
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
# This proof is about the recovery contract, so it asks with the identity the
# rotator actually has: its ServiceAccount, bound to its running Pod.
if rotator_kube -n "$OPERATOR_NAMESPACE" \
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

if ! kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get secret "$SECRET_NAME" -o json \
	>"$SECRET_BEFORE" 2>"$SECRET_ERROR"; then
	fail "could not capture the generated webhook Secret before the Helm lookup proof"
fi
chmod 600 "$SECRET_BEFORE"
if ! validate_generated_secret "$SECRET_BEFORE"; then
	fail "generated webhook Secret lacks exact ownership, type, or required certificate material"
fi
OLD_CA=$(jq -r '.data["ca.crt"]' "$SECRET_BEFORE")
OLD_CERT=$(jq -r '.data["tls.crt"]' "$SECRET_BEFORE")

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
      (.webhooks[] | select(.name == "mapproval.operator.ptah.run") | .clientConfig.caBundle) = $bundle
    ' |
	kubectl --kubeconfig "$KUBECONFIG_FILE" replace -f - >/dev/null
kubectl --kubeconfig "$KUBECONFIG_FILE" get \
	validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" -o json |
	jq --arg approval "$APPROVAL_OVERLAP" --arg pod "$POD_OVERLAP" --arg controller "$CONTROLLER_WRITE_OVERLAP" '
	      (.webhooks[] | select(.name == "vapproval.operator.ptah.run") | .clientConfig.caBundle) = $approval |
	      (.webhooks[] | select(.name == "vpodintent.operator.ptah.run") | .clientConfig.caBundle) = $pod |
	      (.webhooks[] | select(.name == "vcontrollerwrite.operator.ptah.run") | .clientConfig.caBundle) = $controller
    ' |
	kubectl --kubeconfig "$KUBECONFIG_FILE" replace -f - >/dev/null

assert_entry_bundle mutatingwebhookconfiguration "$MUTATING_CONFIGURATION" \
	mapproval.operator.ptah.run "$MUTATING_UPGRADE_CA" "$APPROVAL_UPGRADE_CA" "$POD_UPGRADE_CA" "$CONTROLLER_WRITE_UPGRADE_CA"
assert_entry_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" \
	vapproval.operator.ptah.run "$APPROVAL_UPGRADE_CA" "$MUTATING_UPGRADE_CA" "$POD_UPGRADE_CA" "$CONTROLLER_WRITE_UPGRADE_CA"
assert_entry_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" \
	vpodintent.operator.ptah.run "$POD_UPGRADE_CA" "$MUTATING_UPGRADE_CA" "$APPROVAL_UPGRADE_CA" "$CONTROLLER_WRITE_UPGRADE_CA"
assert_entry_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" \
	vcontrollerwrite.operator.ptah.run "$CONTROLLER_WRITE_UPGRADE_CA" "$MUTATING_UPGRADE_CA" "$APPROVAL_UPGRADE_CA" "$POD_UPGRADE_CA"
assert_approval_admission_callable "before the Helm upgrade"

if ! helm --kubeconfig "$KUBECONFIG_FILE" upgrade "$HELM_RELEASE" "$CHART_PACKAGE" \
	--namespace "$OPERATOR_NAMESPACE" --reuse-values --wait --timeout 5m >/dev/null; then
	fail "packaged-chart upgrade failed during live certificate lookup"
fi

assert_entry_bundle mutatingwebhookconfiguration "$MUTATING_CONFIGURATION" \
	mapproval.operator.ptah.run "$MUTATING_UPGRADE_CA" "$APPROVAL_UPGRADE_CA" "$POD_UPGRADE_CA" "$CONTROLLER_WRITE_UPGRADE_CA"
assert_entry_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" \
	vapproval.operator.ptah.run "$APPROVAL_UPGRADE_CA" "$MUTATING_UPGRADE_CA" "$POD_UPGRADE_CA" "$CONTROLLER_WRITE_UPGRADE_CA"
assert_entry_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" \
	vpodintent.operator.ptah.run "$POD_UPGRADE_CA" "$MUTATING_UPGRADE_CA" "$APPROVAL_UPGRADE_CA" "$CONTROLLER_WRITE_UPGRADE_CA"
assert_entry_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" \
	vcontrollerwrite.operator.ptah.run "$CONTROLLER_WRITE_UPGRADE_CA" "$MUTATING_UPGRADE_CA" "$APPROVAL_UPGRADE_CA" "$POD_UPGRADE_CA"
assert_approval_admission_callable "after the Helm upgrade"

# A CA transition publishes the old and the new CA in every managed entry,
# records when, and switches the generated Secret no earlier than the
# configured delay after that. The corrupt-CA row proves that order from the
# objects' own timestamps; the missing-Secret row proves a deleted Secret
# does not wait.
CA_SWITCH_DELAY_SECONDS=$(ca_switch_delay_seconds)
STAGING_OBSERVATION=$UPGRADE_WORK_DIR/staging-observation.json
PRIMARY_OBSERVATION=$UPGRADE_WORK_DIR/primary-observation.json
PRIMARY_OBSERVATION_ERROR=$UPGRADE_WORK_DIR/primary-observation.err
STAGED_CA_FILE=$UPGRADE_WORK_DIR/staged-ca.pem
STAGED_CERT_FILE=$UPGRADE_WORK_DIR/staged-cert.pem

# Corrupt ca.crt while leaving the serving leaf and key intact. One malformed
# webhook entry proves that recovery filters candidates independently and can
# authenticate the live leaf from the remaining exact entries.
BROKEN_CA_BUNDLE=$(printf '%s' 'not a certificate' | base64 | tr -d '\n')
kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" patch secret "$SECRET_NAME" \
	--type=json -p="[{\"op\":\"replace\",\"path\":\"/data/ca.crt\",\"value\":\"${BROKEN_CA_BUNDLE}\"}]" >/dev/null
kubectl --kubeconfig "$KUBECONFIG_FILE" get validatingwebhookconfiguration "$VALIDATING_CONFIGURATION" -o json |
	jq --arg bundle "$BROKEN_CA_BUNDLE" '
      (.webhooks[] | select(.name == "vapproval.operator.ptah.run") | .clientConfig.caBundle) = $bundle
    ' |
	kubectl --kubeconfig "$KUBECONFIG_FILE" replace -f - >/dev/null

OLD_ROTATOR_POD=$(live_pod_name certificate-rotation)
OLD_ROTATOR_UID=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get pod "$OLD_ROTATOR_POD" -o jsonpath='{.metadata.uid}')
# The retained runtime guard pins the release's Deployments, so a rollout
# restart, which writes a Pod-template annotation, is refused. Replacing the
# Pod is what this proof needs and what the guard leaves to the ReplicaSet.
kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	delete pod "$OLD_ROTATOR_POD" --wait=false >/dev/null
if ! kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" rollout status \
	deployment "$ROTATOR_DEPLOYMENT" --timeout=5m >/dev/null; then
	kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" describe \
		deployment "$ROTATOR_DEPLOYMENT" >&2 || true
	fail "certificate rotator Deployment could not restart with corrupted CA state"
fi
NEW_ROTATOR_POD=$(live_pod_name certificate-rotation)
NEW_ROTATOR_UID=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get pod "$NEW_ROTATOR_POD" -o jsonpath='{.metadata.uid}')
[ "$NEW_ROTATOR_UID" != "$OLD_ROTATOR_UID" ] || fail "certificate rotator Pod was not replaced"
observe_expanded_trust "corrupt-CA recovery" "$CURRENT_CA_FILE" "$BROKEN_CA_BUNDLE"
assert_approval_admission_callable "while the CA switch waits"

rotation_deadline=$(($(date +%s) + 660))
rotation_ready=0
while [ "$(date +%s)" -lt "$rotation_deadline" ]; do
	NEW_CA=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
		get secret "$SECRET_NAME" -o jsonpath='{.data.ca\.crt}')
	if [ -n "$NEW_CA" ] && [ "$NEW_CA" != "$OLD_CA" ] && \
		kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" get secret "$SECRET_NAME" -o json |
		jq -e '.data["ca.key"] | type == "string" and length > 0' >/dev/null &&
		rotation_transition_complete "$NEW_CA"; then
		rotation_ready=1
		break
	fi
	sleep 2
done
[ "$rotation_ready" -eq 1 ] || fail "certificate rotation did not switch to the new CA and retire the old one"
kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get secret "$SECRET_NAME" --show-managed-fields -o json >"$PRIMARY_OBSERVATION" ||
	fail "could not read the switched generated Secret"
printf '%s' "$NEW_CA" | openssl base64 -d -A | cmp -s - "$STAGED_CA_FILE" ||
	fail "the switch installed a CA other than the one staged at ${EXPANDED_AT}"
# The serving certificate must still be the staged one, so the rotator's last
# write of tls.crt is the switch itself and not a later renewal that would
# hide an early switch.
secret_field_bytes "$PRIMARY_OBSERVATION" tls.crt | cmp -s - "$STAGED_CERT_FILE" ||
	fail "the generated Secret serves a certificate other than the one staged at ${EXPANDED_AT}"
SWITCHED_AT=$(rotator_certificate_write_time "$PRIMARY_OBSERVATION") ||
	fail "the generated Secret does not record exactly one rotator write of its serving certificate"
switched_after_delay "$SWITCHED_AT" "$EXPANDED_AT" "$CA_SWITCH_DELAY_SECONDS" ||
	fail "the rotator switched the generated Secret at ${SWITCHED_AT}, less than ${CA_SWITCH_DELAY_SECONDS}s after the expansion it recorded at ${EXPANDED_AT}"
assert_approval_admission_callable "after the corrupt-CA recovery"
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
ROTATOR_POD_BEFORE_RECREATE=$(live_pod_name certificate-rotation)
ROTATOR_UID_BEFORE_RECREATE=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get pod "$ROTATOR_POD_BEFORE_RECREATE" -o jsonpath='{.metadata.uid}')
kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	delete pod "$ROTATOR_POD_BEFORE_RECREATE" --wait=false >/dev/null

# A missing Secret is already broken: a manager Pod that restarts cannot mount
# its certificate. The rotator publishes the new CA in every managed entry and
# recreates the Secret in the same pass, without waiting out the switch delay.
# Watch for the Secret from the moment the rotator is replaced, so the entries
# are read as close to its return as this harness can.
first_seen_deadline=$(($(date +%s) + 660))
first_seen=0
while [ "$(date +%s)" -lt "$first_seen_deadline" ]; do
	if kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
		get secret "$SECRET_NAME" -o json >"$PRIMARY_OBSERVATION" 2>/dev/null; then
		first_seen=1
		break
	fi
	sleep 1
done
[ "$first_seen" -eq 1 ] || fail "certificate rotator did not recreate the deleted Secret within 660 seconds"
FIRST_SEEN_CA_FILE=$UPGRADE_WORK_DIR/first-seen-ca.pem
secret_field_bytes "$PRIMARY_OBSERVATION" ca.crt >"$FIRST_SEEN_CA_FILE" ||
	fail "could not read the recreated Secret's CA"
openssl verify -CAfile "$FIRST_SEEN_CA_FILE" "$FIRST_SEEN_CA_FILE" >/dev/null 2>&1 ||
	fail "the recreated Secret's CA is not a valid self-signed root"
RECREATED_AT=$(jq -r '.metadata.creationTimestamp' "$PRIMARY_OBSERVATION")
while read -r entry_kind entry_configuration entry_name; do
	assert_entry_trusts "$entry_kind" "$entry_configuration" "$entry_name" "$FIRST_SEEN_CA_FILE"
done <<EOF
mutatingwebhookconfiguration $MUTATING_CONFIGURATION mapproval.operator.ptah.run
mutatingwebhookconfiguration $MUTATING_CONFIGURATION mmigrationapproval.operator.ptah.run
validatingwebhookconfiguration $VALIDATING_CONFIGURATION vapproval.operator.ptah.run
validatingwebhookconfiguration $VALIDATING_CONFIGURATION vmigrationapproval.operator.ptah.run
validatingwebhookconfiguration $VALIDATING_CONFIGURATION vpodintent.operator.ptah.run
validatingwebhookconfiguration $VALIDATING_CONFIGURATION vcontrollerwrite.operator.ptah.run
EOF

if ! kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" rollout status \
	deployment "$ROTATOR_DEPLOYMENT" --timeout=5m >/dev/null; then
	kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" describe \
		deployment "$ROTATOR_DEPLOYMENT" >&2 || true
	fail "certificate rotator Deployment could not restart with a missing TLS Secret"
fi
ROTATOR_POD_AFTER_RECREATE=$(live_pod_name certificate-rotation)
ROTATOR_POD_OBSERVATION=$UPGRADE_WORK_DIR/rotator-pod-after-recreate.json
kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	get pod "$ROTATOR_POD_AFTER_RECREATE" -o json >"$ROTATOR_POD_OBSERVATION" ||
	fail "could not read the replacement certificate rotator Pod"
ROTATOR_UID_AFTER_RECREATE=$(jq -r '.metadata.uid' "$ROTATOR_POD_OBSERVATION")
[ "$ROTATOR_UID_AFTER_RECREATE" != "$ROTATOR_UID_BEFORE_RECREATE" ] ||
	fail "certificate rotator Pod was not replaced for missing-Secret recovery"
ROTATOR_STARTED_AT=$(rotator_container_started_at "$ROTATOR_POD_OBSERVATION") ||
	fail "the replacement certificate rotator Pod does not record when its rotator container started"
# Both instants come from the objects: the rotator container's start and the
# Secret's creation. A recreation that waited out the delay lands after both.
if switched_after_delay "$RECREATED_AT" "$ROTATOR_STARTED_AT" "$CA_SWITCH_DELAY_SECONDS"; then
	fail "the rotator recreated the generated Secret at ${RECREATED_AT}, ${CA_SWITCH_DELAY_SECONDS}s or more after its container started at ${ROTATOR_STARTED_AT}; a missing Secret must not wait out the switch delay"
fi

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
			jq -e \
				--arg label 'operator.ptah.run/generated-webhook-certificate' \
				--arg release "$HELM_RELEASE" \
				--arg namespace "$OPERATOR_NAMESPACE" '
				.type == "kubernetes.io/tls" and
				.metadata.labels == {
					($label): "true",
					"app.kubernetes.io/managed-by": "Helm"
				} and
				.metadata.annotations == {
					"meta.helm.sh/release-name": $release,
					"meta.helm.sh/release-namespace": $namespace
				} and
				(.data | keys | sort) == ["ca.crt", "ca.key", "tls.crt", "tls.key"]
			' >/dev/null && rotation_transition_complete "$RECREATED_CA"; then
			recreate_ready=1
			break
		fi
	fi
	sleep 2
done
[ "$recreate_ready" -eq 1 ] ||
	fail "certificate rotator did not recreate the deleted Secret with the exact recovery contract"
printf '%s' "$RECREATED_CA" | openssl base64 -d -A | cmp -s - "$FIRST_SEEN_CA_FILE" ||
	fail "the recreated Secret's CA changed after it was first seen"

kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" rollout status \
	deployment "$DEPLOYMENT" --timeout=5m >/dev/null ||
	fail "manager Deployment did not remain ready after Secret recreation"
MUTATING_CA=$(uniform_service_bundle mutatingwebhookconfiguration "$MUTATING_CONFIGURATION")
VALIDATING_CA=$(uniform_service_bundle validatingwebhookconfiguration "$VALIDATING_CONFIGURATION")
[ "$MUTATING_CA" = "$RECREATED_CA" ] || fail "mutating webhook trust did not contract after Secret recreation"
[ "$VALIDATING_CA" = "$RECREATED_CA" ] || fail "validating webhook trust did not contract after Secret recreation"
assert_approval_admission_callable "after the Secret recreation"

if ! RECREATE_LOGS=$(kubectl --kubeconfig "$KUBECONFIG_FILE" -n "$OPERATOR_NAMESPACE" \
	logs "$ROTATOR_POD_AFTER_RECREATE" 2>/dev/null); then
	fail "could not inspect missing-Secret recovery logs"
fi
if printf '%s' "$RECREATE_LOGS" | grep -Eiq -- 'PRIVATE[ _-]?KEY|-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----'; then
	fail "missing-Secret recovery logs contain private key material"
fi

PHASE_COMPLETED=1
printf '%s\n' 'e2e certificate rotation: PASS live Helm lookup, corrupt-CA recovery, and exact guarded recreation'
