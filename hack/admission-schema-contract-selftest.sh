#!/bin/sh

set -eu

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
FILTER=$ROOT_DIR/hack/admission-schema-contract.jq
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-admission-schema-selftest.XXXXXX")

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	case "$WORK_DIR" in
	"${TMPDIR:-/tmp}"/ptah-admission-schema-selftest.*) rm -rf -- "$WORK_DIR" ;;
	*)
		printf 'admission schema self-test: refusing to remove unexpected directory %s\n' "$WORK_DIR" >&2
		status=1
		;;
	esac
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

fixture=$WORK_DIR/openapi.json
jq -n '
  def properties($names): reduce $names[] as $name ({}; .[$name] = {});
  {components: {schemas: {
    "io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta": {
      properties: properties([
        "annotations", "creationTimestamp", "deletionGracePeriodSeconds",
        "deletionTimestamp", "finalizers", "generateName", "generation",
        "labels", "managedFields", "name", "namespace", "ownerReferences",
        "resourceVersion", "selfLink", "uid"
      ])
    },
    "io.k8s.api.admissionregistration.v1.WebhookClientConfig": {
      properties: properties(["caBundle", "service", "url"])
    },
    "io.k8s.api.admissionregistration.v1.MutatingWebhook": {
      properties: properties([
        "admissionReviewVersions", "clientConfig", "failurePolicy",
        "matchConditions", "matchPolicy", "name", "namespaceSelector",
        "objectSelector", "reinvocationPolicy", "rules", "sideEffects",
        "timeoutSeconds"
      ])
    },
    "io.k8s.api.admissionregistration.v1.ValidatingWebhook": {
      properties: properties([
        "admissionReviewVersions", "clientConfig", "failurePolicy",
        "matchConditions", "matchPolicy", "name", "namespaceSelector",
        "objectSelector", "rules", "sideEffects", "timeoutSeconds"
      ])
    },
    "io.k8s.api.admissionregistration.v1.MutatingWebhookConfiguration": {
      properties: properties(["apiVersion", "kind", "metadata", "webhooks"])
    },
    "io.k8s.api.admissionregistration.v1.ValidatingWebhookConfiguration": {
      properties: properties(["apiVersion", "kind", "metadata", "webhooks"])
    }
  }}}
' >"$fixture"

jq -e -f "$FILTER" "$fixture" >/dev/null

extra=$WORK_DIR/extra.json
jq '.components.schemas["io.k8s.api.admissionregistration.v1.ValidatingWebhook"].properties.futureField = {}' \
	"$fixture" >"$extra"
if jq -e -f "$FILTER" "$extra" >/dev/null 2>&1; then
	printf '%s\n' 'admission schema self-test: accepted an added webhook field' >&2
	exit 1
fi

missing=$WORK_DIR/missing.json
jq 'del(.components.schemas["io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta"].properties.managedFields)' \
	"$fixture" >"$missing"
if jq -e -f "$FILTER" "$missing" >/dev/null 2>&1; then
	printf '%s\n' 'admission schema self-test: accepted a missing metadata field' >&2
	exit 1
fi

top_level=$WORK_DIR/top-level.json
jq '.components.schemas["io.k8s.api.admissionregistration.v1.MutatingWebhookConfiguration"].properties.status = {}' \
	"$fixture" >"$top_level"
if jq -e -f "$FILTER" "$top_level" >/dev/null 2>&1; then
	printf '%s\n' 'admission schema self-test: accepted an added configuration field' >&2
	exit 1
fi

printf '%s\n' 'admission schema self-test: PASS'
