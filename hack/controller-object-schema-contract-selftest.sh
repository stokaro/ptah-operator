#!/bin/sh

set -eu

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
FILTER=$ROOT_DIR/hack/controller-object-schema-contract.jq
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-controller-object-schema-selftest.XXXXXX")

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	case "$WORK_DIR" in
	"${TMPDIR:-/tmp}"/ptah-controller-object-schema-selftest.*) rm -rf -- "$WORK_DIR" ;;
	*)
		printf 'controller object schema self-test: refusing to remove unexpected directory %s\n' \
			"$WORK_DIR" >&2
		status=1
		;;
	esac
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

batch_fixture=$WORK_DIR/batch.json
core_fixture=$WORK_DIR/core.json

jq -n '{components: {schemas: {
  "io.k8s.api.batch.v1.JobSpec": {properties: {}}
}}}' >"$batch_fixture"

jq -n '
  [
    "PodTemplateSpec", "PodSpec", "Container", "HTTPGetAction", "GRPCAction", "SecurityContext",
    "PodSecurityContext", "Volume", "EmptyDirVolumeSource",
    "ConfigMapVolumeSource", "SecretVolumeSource", "ProjectedVolumeSource",
    "VolumeProjection", "ServiceAccountTokenProjection", "ClusterTrustBundleProjection",
    "PodCertificateProjection", "ConfigMapProjection", "SecretProjection", "DownwardAPIProjection",
    "DownwardAPIVolumeSource", "DownwardAPIVolumeFile", "KeyToPath",
    "VolumeMount", "ResourceRequirements", "Capabilities", "SeccompProfile",
    "EnvVar", "EnvVarSource", "EnvFromSource", "SecretKeySelector",
    "ConfigMapKeySelector", "Toleration"
  ] as $names |
  {components: {schemas: (reduce $names[] as $name ({};
    .["io.k8s.api.core.v1." + $name] = {properties: {}}))}}
' >"$core_fixture"

evaluate() {
	minor=$1
	batch=$2
	core=$3
	jq -e --arg minor "$minor" --slurpfile core "$core" -f "$FILTER" "$batch" >/dev/null
}

evaluate 1.37 "$batch_fixture" "$core_fixture"

job_extra=$WORK_DIR/job-extra.json
jq '.components.schemas["io.k8s.api.batch.v1.JobSpec"].properties.futureField = {}' \
	"$batch_fixture" >"$job_extra"
if evaluate 1.37 "$job_extra" "$core_fixture" 2>/dev/null; then
	printf '%s\n' 'controller object schema self-test: accepted an added JobSpec field' >&2
	exit 1
fi

pod_extra=$WORK_DIR/pod-extra.json
jq '.components.schemas["io.k8s.api.core.v1.PodSpec"].properties.futureField = {}' \
	"$core_fixture" >"$pod_extra"
if evaluate 1.37 "$batch_fixture" "$pod_extra" 2>/dev/null; then
	printf '%s\n' 'controller object schema self-test: accepted an added PodSpec field' >&2
	exit 1
fi

volume_extra=$WORK_DIR/volume-extra.json
jq '.components.schemas["io.k8s.api.core.v1.EmptyDirVolumeSource"].properties.futureField = {}' \
	"$core_fixture" >"$volume_extra"
if evaluate 1.37 "$batch_fixture" "$volume_extra" 2>/dev/null; then
	printf '%s\n' 'controller object schema self-test: accepted an added nested volume field' >&2
	exit 1
fi

projection_extra=$WORK_DIR/projection-extra.json
jq '.components.schemas["io.k8s.api.core.v1.ServiceAccountTokenProjection"].properties.futureField = {}' \
	"$core_fixture" >"$projection_extra"
if evaluate 1.37 "$batch_fixture" "$projection_extra" 2>/dev/null; then
	printf '%s\n' 'controller object schema self-test: accepted an added projection field' >&2
	exit 1
fi

missing_schema=$WORK_DIR/missing-schema.json
jq 'del(.components.schemas["io.k8s.api.core.v1.VolumeMount"])' \
	"$core_fixture" >"$missing_schema"
if evaluate 1.37 "$batch_fixture" "$missing_schema" 2>/dev/null; then
	printf '%s\n' 'controller object schema self-test: accepted a missing reviewed schema' >&2
	exit 1
fi

if evaluate 1.38 "$batch_fixture" "$core_fixture" 2>/dev/null; then
	printf '%s\n' 'controller object schema self-test: accepted an unreviewed Kubernetes minor' >&2
	exit 1
fi

printf '%s\n' 'controller object schema self-test: PASS'
