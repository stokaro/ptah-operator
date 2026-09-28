#!/bin/sh

set -eu

"$(dirname -- "$0")/failed-hook-evidence-selftest.sh"
"$(dirname -- "$0")/admission-schema-contract-selftest.sh"
"$(dirname -- "$0")/controller-object-schema-contract-selftest.sh"
"$(dirname -- "$0")/migration-refusal-filter-selftest.sh"
"$(dirname -- "$0")/acceptance-issue-map-selftest.sh"

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-operator-e2e-static.XXXXXX")
RENDERED_WEBHOOKS=$WORK_DIR/webhooks.yaml
ADMISSION_RENDER=$WORK_DIR/admission.yaml
ROTATOR_RENDER=$WORK_DIR/rotator.yaml
ROTATOR_RECREATE_RENDER=$WORK_DIR/rotator-recreate.yaml
OBSOLETE_RENDER=$WORK_DIR/obsolete-webhooks.yaml
OBSOLETE_ERROR=$WORK_DIR/obsolete-webhooks.err
MUTABLE_MANAGER_ERROR=$WORK_DIR/mutable-manager.err
REJECTED_MANAGER_IMAGE_ERROR=$WORK_DIR/rejected-manager-image.err
LOCAL_REGISTRY_MANAGER_RENDER=$WORK_DIR/local-registry-manager.yaml
LEADER_ELECTION_ERROR=$WORK_DIR/leader-election.err
NO_ELECTION_DEPLOYMENT_RENDER=$WORK_DIR/no-election-deployment.yaml
HA_DEPLOYMENT_RENDER=$WORK_DIR/ha-deployment.yaml
INVALID_BUILD_REVISION_ERROR=$WORK_DIR/invalid-build-revision.err
MISSING_PTAH_VERSION_ERROR=$WORK_DIR/missing-ptah-version.err
MISSING_PTAH_VERSION_TEMPLATE_ERROR=$WORK_DIR/missing-ptah-version-template.err
DEFAULT_RBAC_RENDER=$WORK_DIR/default-rbac.yaml
SHARED_RBAC_RENDER=$WORK_DIR/shared-rbac.yaml
CRD_INSTALL_RENDER=$WORK_DIR/crd-install.yaml
CRD_UPGRADE_RENDER=$WORK_DIR/crd-upgrade.yaml
CRD_FULL_RENDER=$WORK_DIR/crd-full.yaml
EXTERNAL_CERTIFICATE_RENDER=$WORK_DIR/external-certificate.yaml
CONTROLLER_WRITE_GUARD_RENDER=$WORK_DIR/controller-write-guard.yaml
CONTROLLER_OBJECT_GUARD_RENDER=$WORK_DIR/controller-object-guard.yaml
APPLY_POLICY_GUARD_RENDER=$WORK_DIR/apply-policy-guard.yaml
APPLY_POLICY_GUARD_GROUPS_RENDER=$WORK_DIR/apply-policy-guard-groups.yaml
APPLY_POLICY_GUARD_OFF_RENDER=$WORK_DIR/apply-policy-guard-off.yaml
APPLY_POLICY_GUARD_EVERYONE_ERROR=$WORK_DIR/apply-policy-guard-everyone.err
HOOK_FULLNAME_COLLISION_ERROR=$WORK_DIR/hook-fullname-collision.err
INVALID_SERVICE_ACCOUNT_ERROR=$WORK_DIR/invalid-service-account.err
CRD_GUARD_PENDING_FIXTURE=$WORK_DIR/crd-guard-pending.json
CRD_GUARD_FAILED_FIXTURE=$WORK_DIR/crd-guard-failed.json
CRD_GUARD_RUNNING_FIXTURE=$WORK_DIR/crd-guard-running.json
CRD_GUARD_TERMINATED_FIXTURE=$WORK_DIR/crd-guard-terminated.json
CRD_GUARD_STATE=$WORK_DIR/crd-guard-state.json
CANDIDATE_VALUES_FIXTURE=$WORK_DIR/candidate-values.json
PUBLISHER_JOB_FIXTURE=$WORK_DIR/publisher-job.json
NEGATIVE_FIXTURE=$WORK_DIR/negative-job.json
FEATURE_GATE_135_ACTUAL=$WORK_DIR/feature-gate-1.35.yaml
FEATURE_GATE_135_EXPECTED=$WORK_DIR/feature-gate-1.35.expected.yaml
FEATURE_GATE_136_ACTUAL=$WORK_DIR/feature-gate-1.36.yaml
FEATURE_GATE_137_ACTUAL=$WORK_DIR/feature-gate-1.37.yaml
FEATURE_GATE_137_EXPECTED=$WORK_DIR/feature-gate-1.37.expected.yaml
CRD_UPGRADE_INVOCATION_ENV=$WORK_DIR/crd-upgrade-invocation-env
CRD_UNINSTALL_INVOCATION_ENV=$WORK_DIR/crd-uninstall-invocation-env
EXIT_LATCH_PROBE_SCRIPT=$WORK_DIR/exit-latch-probe.sh
EXIT_LATCH_FUNCTIONS=$WORK_DIR/exit-latch-functions
STATIC_PTAH_VERSION=e2e-explicit-version

# A refused parameter expansion (${VAR:?...}) or an unset name under set -u
# ends the shell without setting $?, so an EXIT trap that reports $? reads the
# previous command's success and a script that never finished reports a pass.
# The latch is set where the script reaches its own end; the trap trusts it.
PHASE_COMPLETED=0
cleanup() {
	status=$?
	[ "$status" -ne 0 ] || [ "$PHASE_COMPLETED" -eq 1 ] || status=1
	trap - EXIT HUP INT TERM
	case "$WORK_DIR" in
		"${TMPDIR:-/tmp}"/ptah-operator-e2e-static.*) rm -rf -- "$WORK_DIR" ;;
		*)
			printf 'e2e static: refusing to remove unexpected work directory %s\n' "$WORK_DIR" >&2
			status=1
		;;
	esac
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

command -v helm >/dev/null 2>&1 || {
	printf '%s\n' 'e2e static: Helm is required for rendered webhook checks' >&2
	exit 1
}
command -v shellcheck >/dev/null 2>&1 || {
	printf '%s\n' 'e2e static: shellcheck is required' >&2
	exit 1
}
command -v dash >/dev/null 2>&1 || {
	printf '%s\n' 'e2e static: dash is required for POSIX shell checks' >&2
	exit 1
}

assert_crd_manager_job_container_contract() {
	contract_file=$1
	expected_jobs=$2
	awk -v expected_jobs="$expected_jobs" '
    function finish_document() {
      if (kind == "Job" && manager_command) {
        jobs++
        if (termination_path_fields != 1 || safe_termination_paths != 1 ||
            termination_policy_fields != 1 || safe_termination_policies != 1 ||
            pod_restart_policy != 1 || container_restart_fields != 0) {
          invalid = 1
        }
      }
      kind = ""
      manager_command = 0
      termination_path_fields = 0
      safe_termination_paths = 0
      termination_policy_fields = 0
      safe_termination_policies = 0
      pod_restart_policy = 0
      container_restart_fields = 0
    }
    /^---$/ {
      finish_document()
      next
    }
    $0 == "kind: Job" { kind = "Job" }
    $0 == "          command: [\"/ptah-crd-manager\"]" { manager_command = 1 }
    /^          terminationMessagePath:/ {
      termination_path_fields++
      if ($0 == "          terminationMessagePath: /dev/termination-log") {
        safe_termination_paths++
      }
    }
    /^          terminationMessagePolicy:/ {
      termination_policy_fields++
      if ($0 == "          terminationMessagePolicy: File") {
        safe_termination_policies++
      }
    }
    $0 == "      restartPolicy: Never" { pod_restart_policy++ }
    /^          restartPolicy(Rules)?:/ { container_restart_fields++ }
    END {
      finish_document()
      if (jobs != expected_jobs) invalid = 1
      exit invalid
    }
  ' "$contract_file"
}

assert_webhook_runtime_argument_owners() {
	contract_file=$1
	awk '
    function reset_document() {
      kind = ""
      name = ""
      in_metadata = 0
      in_args = 0
      service_name_args = 0
      timeout_args = 0
    }
    function finish_document(   key) {
      if (kind == "Job" || kind == "Deployment") {
        key = kind "/" name
        if (service_name_args != 0 || timeout_args != 0) {
          if (!(key in expected) || service_name_args != 1 || timeout_args != 1) {
            invalid = 1
          }
          observed[key]++
        }
      }
      reset_document()
    }
    BEGIN {
      expected["Deployment/ptah-e2e-ptah-operator"] = 1
      expected["Deployment/ptah-e2e-ptah-operator-cert-rotator"] = 1
      expected_count = 2
      reset_document()
    }
    /^---$/ {
      finish_document()
      next
    }
    $0 == "kind: Job" { kind = "Job" }
    $0 == "kind: Deployment" { kind = "Deployment" }
    $0 == "metadata:" {
      in_metadata = 1
      next
    }
    in_metadata && /^  name: / && name == "" {
      name = $0
      sub(/^  name: /, "", name)
      gsub(/^"|"$/, "", name)
      in_metadata = 0
      next
    }
    in_metadata && !/^  / { in_metadata = 0 }
    $0 == "          args:" {
      in_args = 1
      next
    }
    in_args && $0 == "            - \"--webhook-service-name=ptah-e2e-ptah-operator-webhook\"" {
      service_name_args++
      next
    }
    in_args && $0 == "            - \"--webhook-timeout-seconds=5\"" {
      timeout_args++
      next
    }
    in_args && !/^            - / { in_args = 0 }
    END {
      finish_document()
      observed_count = 0
      for (key in expected) {
        if (observed[key] != 1) invalid = 1
        observed_count += observed[key]
      }
      if (observed_count != expected_count) invalid = 1
      exit invalid
    }
  ' "$contract_file"
}

for script in "$ROOT_DIR"/hack/e2e-*.sh; do
	sh -n "$script"
	dash -n "$script"
done
sh -n "$ROOT_DIR/hack/stamp-crd-schema-version.sh"
dash -n "$ROOT_DIR/hack/stamp-crd-schema-version.sh"

# These findings are version-dependent, so an unpinned version makes a local pass
# and a CI pass two different claims. 0.11.0 reports an unreachable trap handler
# as SC2329 on the function while 0.9.x and 0.10.x report SC2317 on each command
# in its body, and 0.9.x reports SC2015 on an `A && B || fail` chain that 0.11.0
# does not. Both differences have already produced a pull request that was green
# for whoever ran it and red here.
#
# So the version is exact, the way the kind version is, and the CI workflow
# installs this one from support/tools.json rather than taking whatever the
# runner image happens to ship.
EXPECTED_SHELLCHECK_VERSION=$(jq -r '.shellcheck.version // empty' "$ROOT_DIR/support/tools.json")
[ -n "$EXPECTED_SHELLCHECK_VERSION" ] || {
	printf '%s\n' 'e2e static: support/tools.json does not declare the required shellcheck version' >&2
	exit 1
}
ACTUAL_SHELLCHECK_VERSION=v$(shellcheck --version | awk '/^version:/ { print $2 }')
[ "$ACTUAL_SHELLCHECK_VERSION" = "$EXPECTED_SHELLCHECK_VERSION" ] || {
	printf 'e2e static: shellcheck %s is required, got %s\n' \
		"$EXPECTED_SHELLCHECK_VERSION" "$ACTUAL_SHELLCHECK_VERSION" >&2
	printf 'e2e static: %s carries it\n' \
		"$(jq -r '.shellcheck.linuxAmd64Url // "the ShellCheck release page"' "$ROOT_DIR/support/tools.json")" >&2
	exit 1
}
printf 'e2e static: shellcheck %s\n' "$ACTUAL_SHELLCHECK_VERSION"
# -x so the stopwatch the driver and the phases source is checked in the
# context that sources it, rather than reported as a file nothing followed.
shellcheck -x "$ROOT_DIR"/hack/e2e-*.sh "$ROOT_DIR/hack/stamp-crd-schema-version.sh" \
	"$ROOT_DIR/hack/acceptance-issue-map.sh" "$ROOT_DIR/hack/acceptance-issue-map-selftest.sh"

# The demonstration's shell is published: a reader repeats what a scenario ran.
# The census comes from git rather than from a glob, and holds above a floor,
# because a glob that stopped matching reports nothing and reads as a pass.
DEMO_SHELL=$(git -C "$ROOT_DIR" ls-files demo/bin/lab 'demo/lib/*.sh' 'demo/acceptance/*.sh')
DEMO_SHELL_COUNT=$(printf '%s\n' "$DEMO_SHELL" | grep -c . || true)
[ "$DEMO_SHELL_COUNT" -ge 4 ] || {
	printf 'e2e static: the demonstration ships more shell than the %s files this checked\n' \
		"$DEMO_SHELL_COUNT" >&2
	exit 1
}
# shellcheck disable=SC2086 # The census is one tracked path per line, none with a space.
(cd "$ROOT_DIR" && shellcheck -x $DEMO_SHELL)
printf 'e2e static: %s demonstration scripts\n' "$DEMO_SHELL_COUNT"

# Every image the harness builds is recorded as one it created.
#
# The record is the only answer a teardown has: the images carry no owner label,
# so one the harness built and never recorded stays on the daemon and refuses
# the next run for an identity nobody is using. That is how the demonstration
# lab's operator image was left behind.
BUILT_IMAGE_VARIABLES=$(grep -oE -- '--tag "\$[A-Z_]+"' "$ROOT_DIR/hack/e2e-kind.sh" |
	sed 's/.*"\$\([A-Z_]*\)"/\1/' | sort -u)
BUILT_IMAGE_COUNT=$(printf '%s\n' "$BUILT_IMAGE_VARIABLES" | grep -c . || true)
[ "$BUILT_IMAGE_COUNT" -ge 4 ] || {
	printf 'e2e static: found %s built images, and the harness builds more than that\n' \
		"$BUILT_IMAGE_COUNT" >&2
	exit 1
}
printf '%s\n' "$BUILT_IMAGE_VARIABLES" | while IFS= read -r built_image; do
	grep -qF "add_created_image \"\$$built_image\"" "$ROOT_DIR/hack/e2e-kind.sh" || {
		printf 'e2e static: the harness builds $%s and never records it, so a teardown cannot remove it\n' \
			"$built_image" >&2
		exit 1
	}
done || exit 1
printf 'e2e static: %s built images, each recorded for the teardown\n' "$BUILT_IMAGE_COUNT"

"$ROOT_DIR/hack/e2e-timing-selftest.sh"
"$ROOT_DIR/hack/e2e-shared-images-selftest.sh"
"$ROOT_DIR/hack/e2e-suites-selftest.sh"
"$ROOT_DIR/hack/e2e-control-plane-shape-selftest.sh"
"$ROOT_DIR/hack/e2e-sql-selftest.sh"
"$ROOT_DIR/hack/e2e-ha-metrics-selftest.sh"

# Every phase the driver runs is measured, and it is measured in the one place
# that runs them. A phase invoked around run_recorded_phase would be missing
# from the ledger, and a ledger with a phase missing reads as a run that never
# spent that time.
timing_recorded_phases=$(grep -c 'run_recorded_phase [a-z-]* ' "$ROOT_DIR/hack/e2e-kind.sh" || true)
[ "$timing_recorded_phases" -ge 8 ] || {
	printf 'e2e static: the driver records %s phases, and the lifecycle has eight\n' \
		"$timing_recorded_phases" >&2
	exit 1
}
# shellcheck disable=SC2016 # Match the literal call in the driver.
grep -F 'timing_begin phase "$recorded_phase"' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null || {
	printf '%s\n' 'e2e static: the driver does not measure the phases it runs' >&2
	exit 1
}
grep -F 'timing_end fail' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null || {
	printf '%s\n' 'e2e static: a failed phase is not recorded as failed' >&2
	exit 1
}
# The longest phases carry scenario marks. Without them the report names a
# ninety-minute phase and nothing inside it, which is the measurement the
# critical-path work needs most.
for timing_phase_script in e2e-migrations e2e-reference-data; do
	# Indented too: a phase that selects its scenarios by engine marks them
	# inside the branch that runs them.
	timing_scenarios=$(grep -cE '^[[:space:]]*timing_next scenario ' \
		"$ROOT_DIR/hack/$timing_phase_script.sh" || true)
	[ "$timing_scenarios" -ge 3 ] || {
		printf 'e2e static: %s marks %s scenarios; the report needs its steps named\n' \
			"$timing_phase_script.sh" "$timing_scenarios" >&2
		exit 1
	}
	# shellcheck disable=SC2016 # Match the literal source line in each phase.
	grep -F '. "$ROOT_DIR/hack/e2e-timing.sh"' "$ROOT_DIR/hack/$timing_phase_script.sh" >/dev/null || {
		printf 'e2e static: %s marks scenarios without sourcing the stopwatch\n' \
			"$timing_phase_script.sh" >&2
		exit 1
	}
done
printf 'e2e static: %s measured lifecycle phases, each phase script naming its scenarios\n' \
	"$timing_recorded_phases"

# The images a matrix loads are checked against the run that loads them. The
# refusals live in the driver and are measured by the self-test above; these
# refuse a driver that stopped labeling what it builds, because an unlabeled
# image passes every check that reads a label.
# shellcheck disable=SC2016 # Match the literal label arguments in the driver.
for timing_image_label in \
	'--label "ptah.run/e2e-role=operator"' \
	'--label "ptah.run/e2e-role=next-operator"' \
	'--label "ptah.run/e2e-role=fixture"' \
	'--label "ptah.run/e2e-role=executor"' \
	'--label "ptah.run/e2e-operator-revision=$CONTROLLER_REVISION"' \
	'--label "ptah.run/e2e-ptah-commit=${PTAH_COMMIT}"'; do
	grep -F -- "$timing_image_label" "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null || {
		printf 'e2e static: a task image is built without its provenance: %s\n' \
			"$timing_image_label" >&2
		exit 1
	}
done
printf '%s\n' 'e2e static: the four task images carry the commit, the Ptah pin and the role they play'

# shellcheck disable=SC2016 # These checks intentionally match literal script variables.
grep -F 'git -C "$SOURCE_REPOSITORY_ROOT" archive --format=tar' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null || {
	printf '%s\n' 'e2e static: source snapshots are not materialized with git archive' >&2
	exit 1
}
# shellcheck disable=SC2016 # Match the exact context-bound Buildx invocation.
grep -F 'docker --context "$DOCKER_CONTEXT" buildx inspect "$DOCKER_CONTEXT"' \
	"$ROOT_DIR/hack/e2e-kind.sh" >/dev/null || {
	printf '%s\n' 'e2e static: the selected remote Buildx builder is not checked' >&2
	exit 1
}
# shellcheck disable=SC2016 # Match task-local plugin isolation literally.
grep -F 'ln -s "$BUILDX_PLUGIN_PATH" "$DOCKER_CLI_CONFIG/cli-plugins/docker-buildx"' \
	"$ROOT_DIR/hack/e2e-kind.sh" >/dev/null || {
	printf '%s\n' 'e2e static: isolated Docker config cannot discover the checked Buildx plugin' >&2
	exit 1
}
# shellcheck disable=SC2016 # Count the literal pre- and post-isolation checks.
[ "$(grep -Fc 'buildx inspect "$DOCKER_CONTEXT"' "$ROOT_DIR/hack/e2e-kind.sh")" -eq 2 ] || {
	printf '%s\n' 'e2e static: Buildx must be checked before and after Docker config isolation' >&2
	exit 1
}
# shellcheck disable=SC2016 # Match the exact context-bound Buildx invocation.
[ "$(grep -Fc 'docker --context "$DOCKER_CONTEXT" buildx build' \
	"$ROOT_DIR/hack/e2e-kind.sh")" -eq 4 ] || {
	printf '%s\n' 'e2e static: every task image must use explicit Buildx' >&2
	exit 1
}
# shellcheck disable=SC2016 # Match the exact remote builder binding.
[ "$(grep -Fc -- '--builder "$DOCKER_CONTEXT"' "$ROOT_DIR/hack/e2e-kind.sh")" -eq 4 ] || {
	printf '%s\n' 'e2e static: every task image must bind the selected remote builder' >&2
	exit 1
}
[ "$(grep -Fc -- '--load' "$ROOT_DIR/hack/e2e-kind.sh")" -eq 4 ] || {
	printf '%s\n' 'e2e static: every task image must load its Buildx result into the selected daemon' >&2
	exit 1
}
for read_only_job_marker in \
	'dispatch_read_only_job_fixture() {' \
	'stage_read_only_job_completion' \
	'stage_read_only_job_uid_gap' \
	'wait_for_read_only_job_cleanup' \
	'quiesce_read_only_job_schema'; do
	grep -F "$read_only_job_marker" "$ROOT_DIR/hack/e2e-crd-upgrade.sh" >/dev/null || {
		printf '%s\n' 'e2e static: read-only Job cleanup proof is incomplete' >&2
		exit 1
	}
done
[ "$(grep -Fc 'dispatch_read_only_job_fixture' "$ROOT_DIR/hack/e2e-crd-upgrade.sh")" -eq 3 ] || {
	printf '%s\n' 'e2e static: read-only Job fixture must be dispatched once per release under proof' >&2
	exit 1
}
# shellcheck disable=SC2016 # These checks intentionally match literal harness variables.
for controller_object_live_marker in \
	'E2E_KUBERNETES_VERSION=${E2E_KUBERNETES_VERSION:?E2E_KUBERNETES_VERSION is required}' \
	'verify_supported_server_version' \
	'prove_controller_object_supported_window_guard() {' \
	'expect_controller_job_api_acceptance' \
	'expect_controller_job_vap_denial' \
	'JobSpec.scheduling' \
	'PodSpec.evictionResponders' \
	'EmptyDirVolumeSource.mode' \
	'VolumeMount.bindMountOptions' \
	'PodSpec.workloadRef' \
	'Ptah controller Job write guard rejected an unsafe workload shape'; do
	grep -F "$controller_object_live_marker" "$ROOT_DIR/hack/e2e-crd-upgrade.sh" >/dev/null || {
		printf '%s\n' 'e2e static: live controller-object guarded-field proof is incomplete' >&2
		exit 1
	}
done
# shellcheck disable=SC2016 # Match the literal child-process version binding.
[ "$(grep -Fc 'E2E_KUBERNETES_VERSION=$K8S_VERSION' "$ROOT_DIR/hack/e2e-kind.sh")" -eq 2 ] || {
	printf '%s\n' 'e2e static: live Kubernetes version is not bound into both CRD lifecycle phases' >&2
	exit 1
}
api_server_feature_gate_patch_section=$(sed -n \
	'/^append_api_server_feature_gate_patch()/,/^}/p' "$ROOT_DIR/hack/e2e-kind.sh")
[ -n "$api_server_feature_gate_patch_section" ] || {
	printf '%s\n' 'e2e static: API-server feature gate patch helper is missing' >&2
	exit 1
}
eval "$api_server_feature_gate_patch_section"
: >"$FEATURE_GATE_135_ACTUAL"
: >"$FEATURE_GATE_136_ACTUAL"
: >"$FEATURE_GATE_137_ACTUAL"
EXPECTED_API_SERVER_FEATURE_GATES=
append_api_server_feature_gate_patch 1.35 "$FEATURE_GATE_135_ACTUAL"
[ "$EXPECTED_API_SERVER_FEATURE_GATES" = GenericWorkload=true ] || {
	printf '%s\n' 'e2e static: Kubernetes 1.35 API-server feature gate value is incorrect' >&2
	exit 1
}
EXPECTED_API_SERVER_FEATURE_GATES=
append_api_server_feature_gate_patch 1.36 "$FEATURE_GATE_136_ACTUAL"
[ ! -s "$FEATURE_GATE_136_ACTUAL" ] || {
	printf '%s\n' 'e2e static: Kubernetes 1.36 kind configuration must remain unpatched' >&2
	exit 1
}
[ -z "$EXPECTED_API_SERVER_FEATURE_GATES" ] || {
	printf '%s\n' 'e2e static: Kubernetes 1.36 gained an API-server feature gate expectation' >&2
	exit 1
}
EXPECTED_API_SERVER_FEATURE_GATES=
append_api_server_feature_gate_patch 1.37 "$FEATURE_GATE_137_ACTUAL"
[ "$EXPECTED_API_SERVER_FEATURE_GATES" = \
	EmptyDirVolumeMode=true,EvictionRequestAPI=true,GenericWorkload=true,VolumeBindMountOptions=true,WorkloadWithJob=true ] || {
	printf '%s\n' 'e2e static: Kubernetes 1.37 API-server feature gate value is incorrect' >&2
	exit 1
}
{
	printf '%s\n' 'kubeadmConfigPatchesJSON6902:'
	printf '%s\n' '- group: kubeadm.k8s.io'
	printf '%s\n' '  version: v1beta3'
	printf '%s\n' '  kind: ClusterConfiguration'
	printf '%s\n' '  patch: |'
	printf '%s\n' '    - op: add'
	printf '%s\n' '      path: /apiServer/extraArgs/feature-gates'
	printf '%s\n' '      value: GenericWorkload=true'
} >"$FEATURE_GATE_135_EXPECTED"
{
	printf '%s\n' 'kubeadmConfigPatchesJSON6902:'
	printf '%s\n' '- group: kubeadm.k8s.io'
	printf '%s\n' '  version: v1beta4'
	printf '%s\n' '  kind: ClusterConfiguration'
	printf '%s\n' '  patch: |'
	printf '%s\n' '    - op: add'
	printf '%s\n' '      path: /apiServer/extraArgs/-'
	printf '%s\n' '      value:'
	printf '%s\n' '        name: feature-gates'
	printf '%s\n' '        value: EmptyDirVolumeMode=true,EvictionRequestAPI=true,GenericWorkload=true,VolumeBindMountOptions=true,WorkloadWithJob=true'
} >"$FEATURE_GATE_137_EXPECTED"
cmp -s "$FEATURE_GATE_135_ACTUAL" "$FEATURE_GATE_135_EXPECTED" || {
	printf '%s\n' 'e2e static: Kubernetes 1.35 API-server feature gate patch differs from the exact kubeadm v1beta3 contract' >&2
	exit 1
}
cmp -s "$FEATURE_GATE_137_ACTUAL" "$FEATURE_GATE_137_EXPECTED" || {
	printf '%s\n' 'e2e static: Kubernetes 1.37 API-server feature gate patch differs from the exact kubeadm v1beta4 contract' >&2
	exit 1
}
if grep -Eq '^[[:space:]]*featureGates:' \
	"$FEATURE_GATE_135_ACTUAL" "$FEATURE_GATE_137_ACTUAL"; then
	printf '%s\n' 'e2e static: guarded fields escaped into global kind feature gates' >&2
	exit 1
fi
# shellcheck disable=SC2016 # These checks intentionally match literal harness variables.
for admission_schema_marker in \
	'kubectl --kubeconfig "$KUBECONFIG_FILE" get --raw' \
	'/openapi/v3/apis/admissionregistration.k8s.io/v1' \
	'hack/admission-schema-contract.jq'; do
	grep -F -- "$admission_schema_marker" "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null || {
		printf '%s\n' 'e2e static: per-minor admission OpenAPI contract is not wired into the cluster path' >&2
		exit 1
	}
done
# shellcheck disable=SC2016 # These checks intentionally match literal harness variables.
for controller_schema_marker in \
	'/openapi/v3/apis/batch/v1' \
	'/openapi/v3/api/v1' \
	'--arg minor "${server_major}.${server_minor}"' \
	'hack/controller-object-schema-contract.jq'; do
	grep -F -- "$controller_schema_marker" "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null || {
		printf '%s\n' 'e2e static: per-minor controller Job OpenAPI contract is not wired into the cluster path' >&2
		exit 1
	}
done
release_values_section=$(sed -n '/^render_release_values()/,/^}/p' "$ROOT_DIR/hack/e2e-kind.sh")
[ -n "$release_values_section" ] || {
	printf '%s\n' 'e2e static: release values helper is missing' >&2
	exit 1
}
export E2E_EXECUTOR_IMAGE=e2e.invalid/executor@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export E2E_RUNNER_IMAGE=e2e.invalid/runner@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
export E2E_PTAH_VERSION=release-values-proof
export RUNTIME_FULLNAME=rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr
eval "$release_values_section"
render_release_values "$CANDIDATE_VALUES_FIXTURE" candidate.invalid/operator new \
	sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
	candidate-registry-pull '["e2e:static-administrators", "e2e:static-operators"]'
jq -e '
  .fullnameOverride == "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr" and
  .image.repository == "candidate.invalid/operator" and
  .image.tag == "new" and
  .image.digest == "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" and
  (.image | has("allowMutableTag") | not) and
  .image.pullPolicy == "IfNotPresent" and
  .imagePullSecrets == [{name: "candidate-registry-pull"}] and
  (.image | has("testIdentityDigest") | not)
' "$CANDIDATE_VALUES_FIXTURE" >/dev/null || {
	printf '%s\n' 'e2e static: candidate values lost the production digest-pinned image contract' >&2
	exit 1
}
# The harness identity writes most rows' resources with apply: Always, and
# the chart's apply-policy guard judges it like anyone else, so the release
# values exempt exactly the groups the harness read from the API server: a
# JSON array handed in as they were read, not a name the harness assumed.
jq -e '
  .applyPolicyGuard == {exemptGroups: ["e2e:static-administrators", "e2e:static-operators"]}
' "$CANDIDATE_VALUES_FIXTURE" >/dev/null || {
	printf '%s\n' 'e2e static: candidate values do not exempt the harness groups from the apply-policy guard' >&2
	exit 1
}
if render_release_values "$WORK_DIR/candidate-values-no-groups.json" candidate.invalid/operator new \
	sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
	candidate-registry-pull 'not-a-json-array' 2>/dev/null; then
	printf '%s\n' 'e2e static: release values accepted exempt groups that are not a JSON array' >&2
	exit 1
fi

# The acceptance scripts read the runner protocol from support/ptah.json, so
# the constant, the catalog and the protocol record have to name one version:
# a bump that moved only the constant would leave every lifecycle asserting
# the old one.
runner_protocol_constant=$(sed -n 's/^[[:space:]]*ProtocolVersion = \([1-9][0-9]*\)$/\1/p' \
	"$ROOT_DIR/internal/runner/protocol.go")
runner_protocol_catalog=$(jq -er '
  [.releases[] | select(.operator == "edge") | .verified[].runnerProtocolVersion] | unique |
  if length == 1 and (.[0] | type) == "number" then .[0]
  else error("support/ptah.json must record exactly one runner protocol version for edge") end
' "$ROOT_DIR/support/ptah.json")
runner_protocol_record=$(jq -er '.protocolVersion' "$ROOT_DIR/support/runner-protocol.json")
if [ -z "$runner_protocol_constant" ] ||
	[ "$runner_protocol_constant" != "$runner_protocol_catalog" ] ||
	[ "$runner_protocol_constant" != "$runner_protocol_record" ]; then
	printf 'e2e static: runner protocol constant %s, support/ptah.json %s and support/runner-protocol.json %s disagree\n' \
		"$runner_protocol_constant" "$runner_protocol_catalog" "$runner_protocol_record" >&2
	exit 1
fi
grep -F 'TestParserRejectsSuccessfulVerifyFrameFromPreviousProtocol' \
	"$ROOT_DIR/internal/runner/protocol_test.go" >/dev/null || {
	printf '%s\n' 'e2e static: previous runner protocol rejection regression is missing' >&2
	exit 1
}
grep -F 'RegistryCASHA256SecretKey = "caSHA256"' \
	"$ROOT_DIR/api/v1alpha1/ptahschema_types.go" >/dev/null || {
	printf '%s\n' 'e2e static: fixed CA digest Secret key is missing' >&2
	exit 1
}

grep -F '__API_SERVER_PORT__' "$ROOT_DIR/testdata/e2e/kind.yaml.tmpl" >/dev/null
for kubelet_user_namespace_marker in \
	'    kubeadmConfigPatches:' \
	'        kind: KubeletConfiguration' \
	'        apiVersion: kubelet.config.k8s.io/v1beta1' \
	'        featureGates:' \
	'          KubeletInUserNamespace: true'; do
	grep -Fx "$kubelet_user_namespace_marker" "$ROOT_DIR/testdata/e2e/kind.yaml.tmpl" >/dev/null || {
		printf '%s\n' 'e2e static: kind template lacks the exact kubelet-only user-namespace compatibility patch' >&2
		exit 1
	}
done
[ "$(grep -Fc 'KubeletInUserNamespace: true' "$ROOT_DIR/testdata/e2e/kind.yaml.tmpl")" -eq 4 ] || {
	printf '%s\n' 'e2e static: kind template must enable KubeletInUserNamespace on all four HA nodes' >&2
	exit 1
}
# shellcheck disable=SC2016 # Exact source markers intentionally retain shell variables literally.
for api_server_ha_marker in \
	'API_SERVER_ENDPOINT_ADDRESS_FILE=$WORK_DIR/api-server-endpoint-addresses.txt' \
	'        elif (($pod.metadata.annotations["kubernetes.io/config.mirror"] // "") | length) == 0 then' \
	'        elif $pod.metadata.name != ($component + "-" + $pod.spec.nodeName) then' \
	'        ($pod.status.phase == "Running") and' \
	'        ($pod.status.containerStatuses[0].ready == true) and' \
	'        (($pod.status.containerStatuses[0].state.running | type) == "object");' \
	'probe_api_server_endpoints() {' \
	'kubectl --kubeconfig /etc/kubernetes/admin.conf' \
	'--server "https://${api_server_endpoint}:6443"' \
	'--tls-server-name kubernetes' \
	'--request-timeout=10s get --raw=/readyz' \
	'[ "$api_server_readyz" = ok ] || return 1'; do
	grep -F -- "$api_server_ha_marker" "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null || {
		printf 'e2e static: kind HA proof lacks exact marker: %s\n' "$api_server_ha_marker" >&2
		exit 1
	}
done
# shellcheck disable=SC2016 # Exact source markers intentionally retain jq variables literally.
for api_server_endpoint_binding_marker in \
	'  [$cluster + "-control-plane", $cluster + "-control-plane2", $cluster + "-control-plane3"] as $control_plane_names |' \
	'    | select(.metadata.labels["node-role.kubernetes.io/control-plane"] != null)' \
	'  ([$control_plane_nodes[].metadata.name] | sort) == ($control_plane_names | sort) and' \
	'    [(.status.addresses // [])[] | select(.type == "InternalIP") | .address] as $internal_ips |' \
	'  ($addresses | sort) == ($control_plane_addresses | sort)'; do
	grep -F -- "$api_server_endpoint_binding_marker" \
		"$ROOT_DIR/hack/api-server-endpoint-inventory.jq" >/dev/null || {
		printf 'e2e static: API server EndpointSlice filter lacks exact node binding: %s\n' \
			"$api_server_endpoint_binding_marker" >&2
		exit 1
	}
done
grep -F 'application/vnd.stokaro.ptah.schema.v1' \
	"$ROOT_DIR/testdata/e2e/verification-policy.yaml" >/dev/null
if grep -F 'require_digest_pin: true' "$ROOT_DIR/testdata/e2e/verification-policy.yaml" >/dev/null; then
	printf '%s\n' 'e2e static: mutable-tag policy cannot require the requested reference to be pinned' >&2
	exit 1
fi
grep -F 'require_digest_pin: true' \
	"$ROOT_DIR/testdata/e2e/verification-policy-digest-pin.yaml" >/dev/null
grep -F 'application/vnd.stokaro.ptah.schema.v1' \
	"$ROOT_DIR/testdata/e2e/verification-policy-digest-pin.yaml" >/dev/null
# shellcheck disable=SC2016 # Exact source markers intentionally retain shell variables literally.
for tls_fixture_marker in \
	'TLS_PROXY_CA_CONFIG_FILE=$TLS_PROXY_DIR/ca.conf' \
	"subjectAltName=DNS:\${TLS_PROXY_DNS_NAME}" \
	'run ./test/e2e/handcraftoci verify-certificate' \
	'E2E_TLS_PROXY_CA_FILE=$TLS_PROXY_CA_FILE' \
	'E2E_TLS_PROXY_CERT_FILE=$TLS_PROXY_CERT_FILE' \
	'E2E_TLS_PROXY_KEY_FILE=$TLS_PROXY_CERT_KEY_FILE'; do
	grep -F -- "$tls_fixture_marker" "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
done
if grep -F -- '-addext' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null; then
	printf '%s\n' 'e2e static: TLS fixture certificate generation depends on non-portable openssl -addext' >&2
	exit 1
fi
grep -Eq '^[[:space:]]*adminListenAddress[[:space:]]*=[[:space:]]*":8081"$' \
	"$ROOT_DIR/test/e2e/handcraftoci/main.go"
for tls_fixture_source_marker in \
	'registry redirects are not permitted' \
	'TestTLSProxyRoutesReadOnlyRegistryRequestsAndCounts' \
	'TestTLSProxyRejectsMutationsUnknownPathsAndRegistryRedirects' \
	'TestRequestCountAdminDoesNotExposeRegistryRoutes' \
	'TestVerifyCertificateFilesBindsChainDNSAndServerUsage'; do
	grep -F -- "$tls_fixture_source_marker" \
		"$ROOT_DIR/test/e2e/handcraftoci/main.go" \
		"$ROOT_DIR/test/e2e/handcraftoci/main_test.go" >/dev/null
done
grep -F './cmd/ptah' "$ROOT_DIR/Dockerfile.executor" >/dev/null
grep -Eq '^FROM .*@sha256:[0-9a-f]{64}' "$ROOT_DIR/Dockerfile.executor"
grep -F './cmd/manager' "$ROOT_DIR/test/e2e/Dockerfile.operator" >/dev/null
grep -F './cmd/ptah-runner' "$ROOT_DIR/test/e2e/Dockerfile.operator" >/dev/null
grep -F './cmd/ptah-cert-rotator' "$ROOT_DIR/test/e2e/Dockerfile.operator" >/dev/null
grep -F './test/e2e/handcraftoci' "$ROOT_DIR/test/e2e/Dockerfile.operator" >/dev/null
grep -F 'e2e-fixture' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
[ "$(grep -Ec '^FROM .*@sha256:[0-9a-f]{64}' "$ROOT_DIR/test/e2e/Dockerfile.operator")" -eq 3 ]
operator_stage=$(sed -n '/ AS operator$/,/ AS fixture$/p' "$ROOT_DIR/test/e2e/Dockerfile.operator")
printf '%s\n' "$operator_stage" | grep -F 'COPY --from=builder /out/manager /manager' >/dev/null
printf '%s\n' "$operator_stage" |
	grep -F 'COPY --from=builder /out/ptah-cert-rotator /ptah-cert-rotator' >/dev/null
if printf '%s\n' "$operator_stage" | grep -F 'e2e-handcraft-oci' >/dev/null; then
	printf '%s\n' 'e2e static: controller image stage contains the test-only OCI publisher' >&2
	exit 1
fi
if printf '%s\n' "$operator_stage" | grep -F 'e2e-alert-sink' >/dev/null; then
	printf '%s\n' 'e2e static: controller image stage contains the test-only alert receiver' >&2
	exit 1
fi
fixture_stage=$(sed -n '/ AS fixture$/,$p' "$ROOT_DIR/test/e2e/Dockerfile.operator")
printf '%s\n' "$fixture_stage" |
	grep -F 'COPY --from=builder /out/e2e-handcraft-oci /e2e-handcraft-oci' >/dev/null
printf '%s\n' "$fixture_stage" |
	grep -F 'COPY --from=builder /out/e2e-alert-sink /e2e-alert-sink' >/dev/null
if printf '%s\n' "$fixture_stage" | grep -Eq '/out/(manager|ptah-runner|ptah-cert-rotator)'; then
	printf '%s\n' 'e2e static: isolated fixture image stage contains an operator binary' >&2
	exit 1
fi
grep -F -- '--target operator' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
grep -F -- '--target fixture' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
grep -F 'the controller image contains the test-only OCI publisher' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
grep -F 'the isolated fixture image contains an operator binary' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
for packaged_chart_marker in \
	"go -C \"\$ROOT_DIR\" run ./hack/chartpackage" \
	"helm show chart \"\$CHART_PACKAGE\"" \
	"E2E_CHART_PACKAGE=\$CHART_PACKAGE" \
	'installing release-form chart'; do
	grep -F -- "$packaged_chart_marker" "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
done
if grep -Eq '(^|[[:space:]])scale[[:space:]]+deployment(/|[[:space:]])' \
	"$ROOT_DIR/hack/e2e-crd-upgrade.sh"; then
	printf '%s\n' 'e2e static: e2e-crd-upgrade.sh bypasses the Deployment admission contract through the scale subresource' >&2
	exit 1
fi
if grep -F '{"spec":{"replicas":0}}' "$ROOT_DIR/hack/e2e-crd-upgrade.sh" >/dev/null; then
	printf '%s\n' 'e2e static: e2e-crd-upgrade.sh mutates an immutable runtime Deployment to manufacture an outage' >&2
	exit 1
fi
# The Go phases are held to the same two rules. A census from git rather than
# a glob, held above a floor, because a glob that stopped matching reports
# nothing and reads as a pass.
GO_PHASE_SOURCES=$(git -C "$ROOT_DIR" ls-files 'test/e2e/*.go')
GO_PHASE_SOURCE_COUNT=$(printf '%s\n' "$GO_PHASE_SOURCES" | grep -c . || true)
[ "$GO_PHASE_SOURCE_COUNT" -ge 3 ] || {
	printf 'e2e static: found %s Go phase sources, and the harness carries more than that\n' \
		"$GO_PHASE_SOURCE_COUNT" >&2
	exit 1
}
printf '%s\n' "$GO_PHASE_SOURCES" | while IFS= read -r go_phase_source; do
	if grep -Eq 'SubResource\("scale"\)|GetScale|UpdateScale' "$ROOT_DIR/$go_phase_source"; then
		printf 'e2e static: %s bypasses the Deployment admission contract through the scale subresource\n' \
			"$go_phase_source" >&2
		exit 1
	fi
	if grep -F '{"spec":{"replicas":0}}' "$ROOT_DIR/$go_phase_source" >/dev/null; then
		printf 'e2e static: %s mutates an immutable runtime Deployment to manufacture an outage\n' \
			"$go_phase_source" >&2
		exit 1
	fi
done || exit 1
grep -F 'LeaderElectionNamespace: targetLockNamespace' "$ROOT_DIR/cmd/manager/main.go" >/dev/null
grep -F 'ptah-operator.operator.ptah.run' "$ROOT_DIR/cmd/manager/main.go" >/dev/null
for ha_marker in \
	"assert_can_i no \"\$FOREIGN_NAMESPACE\"" \
	'holder_is_ready_manager_pod' \
	'manager leader Lease did not move to a ready replica' \
	'leader Pod failover did not increment leaseTransitions' \
	'wait_for_admitted_operation_pod' \
	'operator.ptah.run/admission-snapshot-digest' \
	'validate_custom_operator_metrics' \
	'ptah_operator_reconciliations_total{family=\"schema\",result=\"success\"}' \
	'ptah_operator_failures_total{category=\"operation\",family=\"schema\",stage=\"resolve\"}' \
	'reconciliation_help == 1 && failure_help == 1' \
	'reconciliation_type == 1 && failure_type == 1' \
	'reconciliation_sample == 1 && failure_sample == 1' \
	"wait_for_failed_resolve_lifecycle \"\$ha_schema_uid\"" \
	"assert_custom_operator_metrics \"\$second_holder\"" \
	'--cascade=background' \
	'operation_pod_deadline' \
	'background Job deletion left orphan operation Pods' \
	'e2e HA: PASS one Lease, exact RBAC, Pod failover, admitted operation, and custom metrics'; do
	grep -F -- "$ha_marker" "$ROOT_DIR/hack/e2e-ha.sh" >/dev/null || {
		printf 'e2e static: live HA proof marker is missing: %s\n' "$ha_marker" >&2
		exit 1
	}
done
# The migration phase proves the rows a unit test cannot: a real history read
# from a real revision table, an approval that authorizes one run, and the
# credential boundary the Jobs keep. Each marker names one of them, so a proof
# that quietly stopped running is a failure here rather than a green phase that
# asserts less than it did.
# shellcheck disable=SC2016 # These markers match literal script and jq text.
for migration_marker in \
	'run_engine_migrations postgresql' \
	'run_engine_migrations mysql' \
	'create_migration_database' \
	'the migration proof must own a database the schema path never touched' \
	'"migrations", "push", $reference, "--migrations-dir", "/migrations",' \
	'"--dir-format", "ptah", "--version", $version, "--plain-http"' \
	'subPath: ., readOnly: true' \
	'the publisher said:' \
	'the migration publisher Job did not preserve the no-database-credential boundary' \
	'wait_for_migration_phase AwaitingApproval' \
	'did not reach an exact three-migration approval gate' \
	'[$spec.migrations[].version] == [1, 2, 3]' \
	'carries SQL text' \
	'approve_migration "$MIGRATION_APPROVAL"' \
	'was not stamped and bound to the exact plan' \
	'wait_for_migration_phase InSync' \
	'did not settle on a history that matches the artifact' \
	'assert_database_migrated' \
	'assert_repeated_reconciliation_runs_nothing' \
	'create_migration_realm' \
	'realmRef: {name: $realm}' \
	'assert_realm_admits_only_listed_claimants' \
	'grant_rival_author_role' \
	"the rival's author listed its own namespace in" \
	"the rival's author created a PtahRealm of its own" \
	'was never refused for naming a realm that does not list' \
	'stopped running while a namespace the realm does not list claimed it' \
	'$status.history.observedAt >= $refusedAt' \
	'kept managing a database a PtahSchema also claims' \
	'was allowed to manage a database a PtahMigration also claims' \
	'a resource that runs nothing claims nothing' \
	'run_unknown_layer_proof' \
	'./hack/unknownlayerfixture' \
	'acted on an artifact carrying a layer its executor cannot read' \
	'assert_unknown_layer_refusal_is_named' \
	'never named the step that refused the artifact within' \
	'was opened for an artifact whose layers were refused' \
	'run_uncertain_apply_proof' \
	'wait_for_uncertain_commit' \
	'did not stop on a run whose evidence it could not read' \
	'did not leave the rows its first migration inserted' \
	'after one whose evidence it could not read' \
	'were doubled, so a run was replayed over what it had already committed' \
	'run_isolated_node_proof' \
	'testdata/e2e/isolated-apply-held.jq' \
	'let go of an Apply whose node is cut off, while its Pod may still be writing' \
	'while the one it dispatched may still be writing' \
	'realm Lease left the claim while its Apply may still be writing' \
	'so a migration ran twice' \
	'testdata/e2e/isolated-run-unknown.jq' \
	'realm Lease was handed back while a Pod of the isolated Apply still exists' \
	'testdata/e2e/isolated-run-settled.jq' \
	'not migrations 1 to 3 once each' \
	'run_checkpoint_bootstrap_proof' \
	'assert_checkpoint_gate' \
	'did not hold a checkpoint bootstrap at the approval gate' \
	'[.spec.migrations[].version] == [3, 4]' \
	'assert_checkpoint_equals_the_long_way' \
	'and the replayed one is' \
	'did not run against the rows the checkpoint seeded' \
	'assert_checkpoint_bootstrap_stays_settled' \
	'asked for another approval after its bootstrap settled' \
	'assert_older_artifact_blocks_everything' \
	'called an artifact older than its database InSync' \
	'did not report the reading that disagrees with itself' \
	'for an artifact older than its database' \
	'assert_partial_run_blocks_and_recovers' \
	'did not stop on a migration that committed half of itself' \
	'stopped refusing while a partial migration stood unresolved' \
	'testdata/e2e/migration-partial-refusal.jq' \
	'testdata/e2e/migration-dirty-reading.jq' \
	'testdata/e2e/migration-refused-boundary.jq' \
	'testdata/e2e/migration-history-ahead.jq' \
	'testdata/e2e/migration-untouched-database.jq' \
	'testdata/e2e/migration-partial-run-recorded.jq' \
	'did not keep the statement the partial migration committed' \
	'assert_no_new_apply_job' \
	'after a partial one' \
	'did not recover on its own reading of a database somebody fixed' \
	'assert_modified_file_blocks_everything' \
	'$status.history.modifiedVersions == [1] and' \
	're-ran an applied migration' \
	'started new work for a history it already matched' \
	'did not apply its data-only change' \
	'seeded rows, not the three migration 1 inserted once' \
	'left its column nullable' \
	'migration Jobs did not keep registry access out of the process that runs SQL' \
	'an approval naming the consumed plan was accepted' \
	'kubectl ptah migration printed SQL' \
	'assert_kubectl_ptah_migration AwaitingApproval' \
	'assert_kubectl_ptah_migration InSync' \
	'does not publish the plan a reader has to approve' \
	'printed the order as' \
	'run_existing_schema_adoption_proof' \
	'run_stopped_apply_proof' \
	'run_lost_log_proof' \
	'migration-stopped-run-recorded.jq' \
	'migration-lost-log-run-recorded.jq' \
	'build_adopt_schema_without_the_operator' \
	'the operator recorded a migration as applied in a database it was never approved to migrate' \
	'against a database that already carries the schema' \
	'"migrations", "baseline", "--migrations-dir", "/migrations",' \
	'"--shadow-db", "$(PTAH_E2E_SHADOW_URL)"' \
	'the adoption Job did not keep registry access out of the process that runs SQL' \
	'did not settle on the history a person recorded' \
	'after a person adopted the database' \
	'(.status | has("lastRun") | not)' \
	'run_apply_policy_guard_proof' \
	'read_apply_policy_guard' \
	'-l app.kubernetes.io/component=apply-policy-guard -o json' \
	'select(.name == "exemptGroups") | .expression | contains($group | tojson)' \
	'"the author selected Always"' \
	'"the author created a migration with Always already set"' \
	'create --dry-run=server -f "$RESOURCE_FILE"' \
	'k_as "$GUARD_ADMINISTRATOR" "$GUARD_ADMINISTRATOR_GROUP" -n "$TEST_NAMESPACE" patch ptahmigration' \
	'e2e migrations: PASS %s approval gate, applied sequence, matching history, and credential isolation'; do
	grep -F -- "$migration_marker" "$ROOT_DIR/hack/e2e-migrations.sh" >/dev/null || {
		printf 'e2e static: live migration proof marker is missing: %s\n' "$migration_marker" >&2
		exit 1
	}
done
# The archived Job inventory is what makes the isolation row provable at all:
# the controller stamps a TTL on every Job it finished reading, and a lifecycle
# outlasts it.
grep -F 'record_migration_jobs' "$ROOT_DIR/hack/e2e-migrations.sh" >/dev/null || {
	printf '%s\n' 'e2e static: the migration phase does not archive Jobs before their TTL removes them' >&2
	exit 1
}
grep -F 'application/vnd.stokaro.ptah.migrations.v1' \
	"$ROOT_DIR/testdata/e2e/verification-policy-migrations.yaml" >/dev/null || {
	printf '%s\n' 'e2e static: the migration verification policy does not pin the migration artifact type' >&2
	exit 1
}
if grep -F 'application/vnd.stokaro.ptah.schema.v1' \
	"$ROOT_DIR/testdata/e2e/verification-policy-migrations.yaml" >/dev/null; then
	printf '%s\n' 'e2e static: the migration verification policy also accepts a schema artifact' >&2
	exit 1
fi
for migration_engine in postgresql mysql postgresql-modified mysql-modified; do
	migration_fixture_count=$(git -C "$ROOT_DIR" ls-files "testdata/e2e/migrations/${migration_engine}/*.sql" | grep -c . || true)
	[ "$migration_fixture_count" -eq 6 ] || {
		printf 'e2e static: the %s migration fixtures are %s files, and the proof applies three migrations in both directions\n' \
			"$migration_engine" "$migration_fixture_count" >&2
		exit 1
	}
done
# The partial fixtures are the three the lifecycle applies plus the one that
# commits half of itself, and the directive is what makes that fourth file a
# partial rather than a rollback. A fixture that lost the directive would still
# fail, the transaction would put the database back, and the row would prove a
# clean failure while claiming to prove a partial one.
# The older fixtures end at version 2 while the lifecycle's database reaches 3.
# A fourth file here, or a third, and the row would prove nothing: the artifact
# has to end before the database does.
# The checkpoint fixtures are the two the checkpoint covers, the checkpoint
# itself, and the migration after it, in both directions. The checkpoint is
# recognized by its file name and by nothing else, so the name is pinned here:
# a rename turns the row into an ordinary four-migration replay that would pass
# every assertion below while proving nothing about checkpoints.
# The uncertain fixtures are two migrations and a third that sleeps. The sleep
# is the window the row needs: without it the run finishes before its evidence
# can be taken away, and the proof becomes a race that passes by luck.
# The fixture that publishes an artifact no product command can produce must
# keep publishing one this executor refuses. A layer media type it accepts would
# turn the row into an ordinary successful pull that asserts nothing, so the
# refusal the fixture is built to trigger is pinned to the two constants it
# stands on.
# The partial row's filter is measured against a document the operator really
# produced, so the self-test can fail the way CI failed rather than the way its
# author imagined. That document is an input to the self-test and not a file the
# phase reads, so a marker in the phase's source can never stand for it.
#
# What has to hold is that the self-test still reads it. A reading the self-test
# stopped naming is a file: the self-test keeps passing, on one case fewer, and
# the case it dropped is the one that came from a real failure. A reading that
# is gone at all the self-test refuses on its own, and this says so first.
recorded_reading=partial-run-left-a-dirty-revision.json
[ -f "$ROOT_DIR/testdata/e2e/readings/$recorded_reading" ] || {
	printf 'e2e static: the recorded operator reading is gone: testdata/e2e/readings/%s\n' \
		"$recorded_reading" >&2
	exit 1
}
grep -F -- "$recorded_reading" "$ROOT_DIR/hack/migration-refusal-filter-selftest.sh" >/dev/null || {
	printf 'e2e static: the filter self-test no longer measures itself against the recorded reading: %s\n' \
		"$recorded_reading" >&2
	exit 1
}

for unknown_layer_marker in \
	'application/vnd.stokaro.ptah.migration.file.v1' \
	'application/vnd.stokaro.ptah.migrations.v1' \
	'the extra layer must not be'; do
	grep -F -- "$unknown_layer_marker" "$ROOT_DIR/hack/unknownlayerfixture/main.go" >/dev/null || {
		printf 'e2e static: the unknown-layer fixture lost the constant it stands on: %s\n' \
			"$unknown_layer_marker" >&2
		exit 1
	}
done

for migration_engine in postgresql-uncertain mysql-uncertain; do
	migration_fixture_count=$(git -C "$ROOT_DIR" ls-files "testdata/e2e/migrations/${migration_engine}/*.sql" | grep -c . || true)
	[ "$migration_fixture_count" -eq 6 ] || {
		printf 'e2e static: the %s migration fixtures are %s files, and the proof needs two committed migrations and one that is still running\n' \
			"$migration_engine" "$migration_fixture_count" >&2
		exit 1
	}
	grep -Eq 'pg_sleep|SLEEP' \
		"$ROOT_DIR/testdata/e2e/migrations/${migration_engine}/0000000003_settle_slowly.up.sql" || {
		printf 'e2e static: the %s interrupted migration does not wait, so the run would finish before its evidence could be removed\n' \
			"$migration_engine" >&2
		exit 1
	}
done

for migration_engine in postgresql-stopped mysql-stopped; do
	migration_fixture_count=$(git -C "$ROOT_DIR" ls-files "testdata/e2e/migrations/${migration_engine}/*.sql" | grep -c . || true)
	[ "$migration_fixture_count" -eq 4 ] || {
		printf 'e2e static: the %s migration fixtures are %s files, and the proof needs one committed migration and one that is still running\n' \
			"$migration_engine" "$migration_fixture_count" >&2
		exit 1
	}
	grep -Eq 'pg_sleep\(600\)|SLEEP\(600\)' \
		"$ROOT_DIR/testdata/e2e/migrations/${migration_engine}/0000000002_wait_to_be_stopped.up.sql" || {
		printf 'e2e static: the %s stopped migration does not outlast its window, so the run would finish before it could be stopped\n' \
			"$migration_engine" >&2
		exit 1
	}
done

for migration_engine in postgresql-checkpoint mysql-checkpoint; do
	migration_fixture_count=$(git -C "$ROOT_DIR" ls-files "testdata/e2e/migrations/${migration_engine}/*.sql" | grep -c . || true)
	[ "$migration_fixture_count" -eq 8 ] || {
		printf 'e2e static: the %s migration fixtures are %s files, and the proof needs two covered migrations, a checkpoint, and one after it\n' \
			"$migration_engine" "$migration_fixture_count" >&2
		exit 1
	}
	for checkpoint_direction in up down; do
		[ -f "$ROOT_DIR/testdata/e2e/migrations/${migration_engine}/0000000003_snapshot.checkpoint.${checkpoint_direction}.sql" ] || {
			printf 'e2e static: the %s checkpoint is not named as one, so Ptah would read it as an ordinary migration\n' \
				"$migration_engine" >&2
			exit 1
		}
	done
done

for migration_engine in postgresql-older mysql-older; do
	migration_fixture_count=$(git -C "$ROOT_DIR" ls-files "testdata/e2e/migrations/${migration_engine}/*.sql" | grep -c . || true)
	[ "$migration_fixture_count" -eq 4 ] || {
		printf 'e2e static: the %s migration fixtures are %s files, and the proof needs an artifact that ends at two migrations\n' \
			"$migration_engine" "$migration_fixture_count" >&2
		exit 1
	}
done

for migration_engine in postgresql-partial mysql-partial; do
	migration_fixture_count=$(git -C "$ROOT_DIR" ls-files "testdata/e2e/migrations/${migration_engine}/*.sql" | grep -c . || true)
	[ "$migration_fixture_count" -eq 8 ] || {
		printf 'e2e static: the %s migration fixtures are %s files, and the proof applies four migrations in both directions\n' \
			"$migration_engine" "$migration_fixture_count" >&2
		exit 1
	}
	grep -F -- '-- +ptah no_transaction' \
		"$ROOT_DIR/testdata/e2e/migrations/${migration_engine}/0000000004_partial_backfill.up.sql" >/dev/null || {
		printf 'e2e static: the %s partial migration runs inside a transaction, so it would roll back rather than stop halfway\n' \
			"$migration_engine" >&2
		exit 1
	}
done

# The out-of-order row needs versions with room between them: 10 and 30 applied,
# and 20 arriving afterwards. A fixture that lost the gap would make the proof
# about an ordinary pending migration.
for branch_engine in postgresql mysql; do
	branch_base_count=$(git -C "$ROOT_DIR" ls-files "testdata/e2e/migrations/${branch_engine}-branch/*.sql" | grep -c . || true)
	[ "$branch_base_count" -eq 4 ] || {
		printf 'e2e static: the %s-branch fixtures are %s files, and the applied history is two migrations in both directions\n' \
			"$branch_engine" "$branch_base_count" >&2
		exit 1
	}
	branch_late_count=$(git -C "$ROOT_DIR" ls-files "testdata/e2e/migrations/${branch_engine}-branch-late/*.sql" | grep -c . || true)
	[ "$branch_late_count" -eq 6 ] || {
		printf 'e2e static: the %s-branch-late fixtures are %s files, and the late arrival adds one migration in both directions\n' \
			"$branch_engine" "$branch_late_count" >&2
		exit 1
	}
	for branch_shared in 0000000010_create_branch_widgets 0000000030_add_branch_note; do
		for branch_direction in up down; do
			cmp "$ROOT_DIR/testdata/e2e/migrations/${branch_engine}-branch/${branch_shared}.${branch_direction}.sql" \
				"$ROOT_DIR/testdata/e2e/migrations/${branch_engine}-branch-late/${branch_shared}.${branch_direction}.sql" || {
				printf 'e2e static: %s %s differs between the branch fixtures, which would make the proof about a changed checksum\n' \
					"$branch_engine" "$branch_shared" >&2
				exit 1
			}
		done
	done
	git -C "$ROOT_DIR" ls-files "testdata/e2e/migrations/${branch_engine}-branch-late/0000000020_*.sql" | grep -q . || {
		printf 'e2e static: the %s-branch-late fixtures carry no migration between the applied versions\n' \
			"$branch_engine" >&2
		exit 1
	}
done
grep -F 'unset REGISTRY_PASSWORD' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
if grep -F 'E2E_REGISTRY_PASSWORD' "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null; then
	printf '%s\n' 'e2e static: registry password is handed off through the host environment' >&2
	exit 1
fi
for secret_script in e2e-migrations.sh e2e-reference-data.sh; do
	grep -F 'grep -F -f' "$ROOT_DIR/hack/$secret_script" >/dev/null
	grep -F -- '--rawfile' "$ROOT_DIR/hack/$secret_script" >/dev/null
done

# The reference-data phase measures a refusal -- no declared row value in
# status, an Event, or a log -- so the proofs that read for one are pinned here.
# A phase that kept the scanner and stopped calling it would still pass its own
# assertions.
for reference_marker in \
	'e2e reference data: starting the %s lifecycle on a database with no tables' \
	'scan_for_rows' \
	'collect_declared_row_values' \
	'assert_declared_rows 2 0 ""' \
	'assert_declared_rows 2 3 "Czech Republic"' \
	'assert_repeated_reconciliation_changes_nothing' \
	'assert_data_only_change_reconciles' \
	'assert_external_edit_refuses_a_stale_approval' \
	'Reference data:   0 to insert, 1 to update, 0 to delete' \
	'assert_kubectl_ptah_schema_line' \
	'kubectl ptah schema never reported' \
	'assert_removed_declaration_keeps_rows' \
	'assert_rows_never_left_the_database' \
	'a reconciliation with no declared change rewrote the managed rows' \
	'the controller log is empty, so the row scan would have measured nothing' \
	'an approval naming the replaced plan was accepted' \
	'removing the declaration changed the managed rows' \
	'carries a declared row value'; do
	grep -F -- "$reference_marker" "$ROOT_DIR/hack/e2e-reference-data.sh" >/dev/null || {
		printf 'e2e static: live reference-data proof marker is missing: %s\n' "$reference_marker" >&2
		exit 1
	}
done
# The proof is a sequence, and each step is a different statement about the same
# tables: declared, changed, no longer declared, declared empty, and declared
# again behind a fence. Each revision carries the Go source Ptah reads and the
# rows it declares -- the emptied one declares a file with no rows in it, which
# is the statement that separates ending management from emptying a table, and
# the last one is the change spec.policy.protectedTables refuses.
for reference_revision in v1 v2 v3 v4 v5; do
	reference_entities=$(git -C "$ROOT_DIR" ls-files "testdata/e2e/reference/${reference_revision}/entities.go" | grep -c . || true)
	[ "$reference_entities" -eq 1 ] || {
		printf 'e2e static: reference-data revision %s has no entities.go\n' "$reference_revision" >&2
		exit 1
	}
	reference_rows=$(git -C "$ROOT_DIR" ls-files "testdata/e2e/reference/${reference_revision}/*.yaml" | grep -c . || true)
	[ "$reference_rows" -ge 1 ] || {
		printf 'e2e static: reference-data revision %s declares no rows\n' "$reference_revision" >&2
		exit 1
	}
done
grep -F 'ptah:schema:data' "$ROOT_DIR/testdata/e2e/reference/v1/entities.go" >/dev/null || {
	printf '%s\n' 'e2e static: the reference-data fixture declares no managed rows' >&2
	exit 1
}
if grep -F 'file="countries.yaml"' "$ROOT_DIR/testdata/e2e/reference/v3/entities.go" >/dev/null; then
	printf '%s\n' 'e2e static: the ended-management fixture still declares the countries rows' >&2
	exit 1
fi
for pinned_input in E2E_REGISTRY_IMAGE E2E_POSTGRES_SOURCE_IMAGE E2E_MYSQL_SOURCE_IMAGE \
	E2E_PROMETHEUS_SOURCE_IMAGE E2E_ALERTMANAGER_SOURCE_IMAGE; do
	grep -E "^${pinned_input}=.*@sha256:[0-9a-f]{64}" "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
done
for engine in postgresql mysql; do
	for revision in v1 v2 v3 v4; do
		fixture="$ROOT_DIR/testdata/e2e/${engine}-${revision}.sql"
		[ -s "$fixture" ] || {
			printf 'e2e static: missing schema fixture %s\n' "$fixture" >&2
			exit 1
		}
	done
	grep -F 'note ' "$ROOT_DIR/testdata/e2e/${engine}-v2.sql" >/dev/null
	grep -F 'enabled ' "$ROOT_DIR/testdata/e2e/${engine}-v3.sql" >/dev/null
	if [ "$engine" = mysql ]; then
		grep -F 'note ' "$ROOT_DIR/testdata/e2e/mysql-v4.sql" >/dev/null
		grep -F 'enabled ' "$ROOT_DIR/testdata/e2e/mysql-v4.sql" >/dev/null
		if grep -Fi 'e2e_widgets_name_idx' "$ROOT_DIR/testdata/e2e/mysql-v4.sql" >/dev/null; then
			printf '%s\n' 'e2e static: MySQL v4 must remove only the plain named index' >&2
			exit 1
		fi
		for mysql_fixture in mysql-v3.sql mysql-v4.sql mysql-fault-v1.sql; do
			fixture_path="$ROOT_DIR/testdata/e2e/$mysql_fixture"
			if [ "$(grep -Fxc '  UNIQUE KEY e2e_widgets_name_unique (name)' \
				"$fixture_path")" -ne 1 ] ||
				[ "$(grep -Eic 'e2e_widgets_name_unique' "$fixture_path")" -ne 1 ]; then
				printf 'e2e static: %s must contain exactly one unique name index\n' \
					"$mysql_fixture" >&2
				exit 1
			fi
			if grep -E '(^|[[:space:]])(--|#)|/\*|\*/' "$fixture_path" >/dev/null; then
				printf 'e2e static: %s must not hide index evidence in SQL comments\n' \
					"$mysql_fixture" >&2
				exit 1
			fi
		done
		for mysql_fixture in mysql-v3.sql mysql-fault-v1.sql; do
			fixture_path="$ROOT_DIR/testdata/e2e/$mysql_fixture"
			if [ "$(grep -Ec '^CREATE INDEX e2e_widgets_name_idx ON e2e_widgets \(name\);$' \
				"$fixture_path")" -ne 1 ] ||
				[ "$(grep -Eic 'e2e_widgets_name_idx' "$fixture_path")" -ne 1 ]; then
				printf 'e2e static: %s must contain exactly one standalone plain named index\n' \
					"$mysql_fixture" >&2
				exit 1
			fi
		done
	fi
	if [ "$engine" = postgresql ] &&
		grep -E 'note |enabled ' "$ROOT_DIR/testdata/e2e/${engine}-v4.sql" >/dev/null; then
		printf '%s\n' 'e2e static: PostgreSQL v4 must exercise destructive column removals' >&2
		exit 1
	fi
	[ -s "$ROOT_DIR/testdata/e2e/${engine}-fault-v1.sql" ] || {
		printf 'e2e static: missing %s fault schema fixture\n' "$engine" >&2
		exit 1
	}
	grep -F 'fault_token ' "$ROOT_DIR/testdata/e2e/${engine}-fault-v1.sql" >/dev/null
	# The fault databases are seeded from the v3 fixture, so the fault schema has
	# to be that fixture plus the fault_token column. Anything else plans a drop,
	# the schema blocks on a destructive plan, and the fault injection waits for an
	# approval boundary it can never reach. Trailing commas move with the column,
	# so compare the fixtures without them.
	fault_seed_lines=$(sed 's/,$//' "$ROOT_DIR/testdata/e2e/${engine}-v3.sql")
	fault_desired_lines=$(grep -v 'fault_token ' \
		"$ROOT_DIR/testdata/e2e/${engine}-fault-v1.sql" | sed 's/,$//')
	[ "$fault_desired_lines" = "$fault_seed_lines" ] || {
		printf 'e2e static: %s-fault-v1.sql must be %s-v3.sql plus the fault_token column\n' \
			"$engine" "$engine" >&2
		exit 1
	}
done
static_reject_marker() {
	static_section=$1
	static_marker=$2
	static_context=$3
	if printf '%s\n' "$static_section" | grep -F -- "$static_marker" >/dev/null; then
		printf 'e2e static: %s contains forbidden marker %s\n' \
			"$static_context" "$static_marker" >&2
		exit 1
	fi
}
static_require_count() {
	static_section=$1
	static_marker=$2
	static_expected=$3
	static_context=$4
	static_actual=$(printf '%s\n' "$static_section" | grep -Fc -- "$static_marker" || true)
	[ "$static_actual" -eq "$static_expected" ] || {
		printf 'e2e static: %s has %s occurrences of %s, expected %s\n' \
			"$static_context" "$static_actual" "$static_marker" "$static_expected" >&2
		exit 1
	}
}

static_require_order() {
	static_section=$1
	static_context=$2
	shift 2
	static_after=0
	for static_marker do
		static_line=$(printf '%s\n' "$static_section" | grep -Fn -- "$static_marker" |
			awk -F: -v after="$static_after" '$1 > after { print $1; exit }')
		[ -n "$static_line" ] || {
			printf 'e2e static: %s lacks ordered marker %s after line %s\n' \
				"$static_context" "$static_marker" "$static_after" >&2
			exit 1
		}
		static_after=$static_line
	done
}

next_release_harness_source=$(cat "$ROOT_DIR/hack/e2e-kind.sh")
next_release_crd_source=$(cat "$ROOT_DIR/hack/e2e-crd-upgrade.sh")
# shellcheck disable=SC2016 # Exact synthetic-release markers retain runtime variables literally.
for next_release_harness_marker in \
	'--output="$NEXT_SOURCE_ARCHIVE" "$CONTROLLER_REVISION"' \
	'add_created_image "$NEXT_OPERATOR_IMAGE"' \
	'--file "$NEXT_BUILD_CONTEXT/test/e2e/Dockerfile.operator"' \
	'push_task_image "$NEXT_OPERATOR_IMAGE" ptah-operator-next' \
	'"$NEXT_VALUES_FILE" "$NEXT_CONTROLLER_REPOSITORY" "$IMAGE_TAG"' \
	'E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE' \
	'E2E_NEXT_VALUES_FILE=$NEXT_VALUES_FILE' \
	'E2E_NEXT_CONTROLLER_IMAGE=$NEXT_CONTROLLER_IMAGE'; do
	static_require_count "$next_release_harness_source" "$next_release_harness_marker" 1 \
		'synthetic next-release harness'
done
# The same exact current-release values and image identity must reach both the
# upgrade proof and the final fresh-install proof.
# shellcheck disable=SC2016 # Exact handoff markers retain shell variables literally.
static_require_count "$next_release_harness_source" \
	'E2E_CANDIDATE_VALUES_FILE=$CANDIDATE_VALUES_FILE' 2 \
	'candidate values handoff to upgrade and fresh-install proofs'
# shellcheck disable=SC2016 # Exact handoff marker retains shell variables literally.
static_require_count "$next_release_harness_source" \
	'E2E_CANDIDATE_IMAGE=$CANDIDATE_OPERATOR_IMAGE' 2 \
	'candidate image handoff to upgrade and fresh-install proofs'
# shellcheck disable=SC2016 # Count the literal immutable source repository reads.
static_require_count "$next_release_harness_source" \
	'git -C "$SOURCE_REPOSITORY_ROOT" archive --format=tar' 2 \
	'exact snapshot verification and next-release source archives'
# shellcheck disable=SC2016 # Ordered markers intentionally retain runtime variables literally.
static_require_order "$next_release_harness_source" \
	'synthetic next-release archive, image, registry, values, and uninstall handoff' \
	'NEXT_SOURCE_ARCHIVE=$WORK_DIR/next-source.tar' \
	'git -C "$SOURCE_REPOSITORY_ROOT" archive --format=tar' \
	'--output="$NEXT_SOURCE_ARCHIVE" "$CONTROLLER_REVISION"' \
	'tar -xf "$NEXT_SOURCE_ARCHIVE" -C "$NEXT_BUILD_CONTEXT"' \
	'go -C "$NEXT_BUILD_CONTEXT" run -mod=readonly ./hack/chartpackage' \
	'add_created_image "$NEXT_OPERATOR_IMAGE"' \
	'--file "$NEXT_BUILD_CONTEXT/test/e2e/Dockerfile.operator"' \
	'push_task_image "$NEXT_OPERATOR_IMAGE" ptah-operator-next' \
	'"$NEXT_VALUES_FILE" "$NEXT_CONTROLLER_REPOSITORY" "$IMAGE_TAG"' \
	'E2E_NEXT_CHART_PACKAGE=$NEXT_CHART_PACKAGE' \
	'E2E_NEXT_VALUES_FILE=$NEXT_VALUES_FILE' \
	'E2E_NEXT_CONTROLLER_IMAGE=$NEXT_CONTROLLER_IMAGE' \
	'E2E_PHASE=uninstall'

# The release comparison artifact must be the exact current-release package that the
# candidate lifecycle installed. It is published only after the mandatory
# next-release uninstall proof has returned successfully.
# shellcheck disable=SC2016 # Exact artifact markers retain runtime variables literally.
for release_chart_output_marker in \
	'E2E_RELEASE_CHART_OUTPUT=${E2E_RELEASE_CHART_OUTPUT:-}' \
	'export_release_chart() {' \
	'[ -n "$E2E_RELEASE_CHART_OUTPUT" ] || return 0' \
	'E2E_RELEASE_CHART_OUTPUT must be an absolute path' \
	'E2E_RELEASE_CHART_OUTPUT parent must be an existing non-symlink directory' \
	'E2E_RELEASE_CHART_OUTPUT must be outside the task work directory' \
	'refusing to replace existing E2E_RELEASE_CHART_OUTPUT target' \
	'"$RELEASE_CHART_OUTPUT_PARENT/.ptah-operator-release-chart.XXXXXX"' \
	'cp "$CHART_PACKAGE" "$RELEASE_CHART_OUTPUT_TEMP"' \
	'cmp -s "$CHART_PACKAGE" "$RELEASE_CHART_OUTPUT_TEMP"' \
	'ln "$RELEASE_CHART_OUTPUT_TEMP" "$RELEASE_CHART_OUTPUT_TARGET"' \
	'if [ -n "$RELEASE_CHART_OUTPUT_TEMP" ]; then'; do
	static_require_count "$next_release_harness_source" "$release_chart_output_marker" 1 \
		'exact post-lifecycle current-release chart export'
done
# The successful publication path removes the temporary hard link, while the
# EXIT trap removes an incomplete one after any earlier failure.
# shellcheck disable=SC2016 # Exact cleanup marker retains runtime variables literally.
static_require_count "$next_release_harness_source" \
	'if ! rm -f -- "$RELEASE_CHART_OUTPUT_TEMP"; then' 2 \
	'exact release chart temporary-file cleanup'
# shellcheck disable=SC2016 # Reject the literal synthetic chart source without expansion.
static_reject_marker "$next_release_harness_source" \
	'cp "$NEXT_CHART_PACKAGE" "$RELEASE_CHART_OUTPUT_TEMP"' \
	'current-release chart export source'
# shellcheck disable=SC2016 # Reject the literal clobbering rename without expansion.
static_reject_marker "$next_release_harness_source" \
	'mv "$RELEASE_CHART_OUTPUT_TEMP" "$RELEASE_CHART_OUTPUT_TARGET"' \
	'no-clobber release chart publication'
# shellcheck disable=SC2016 # Ordered markers intentionally retain runtime variables literally.
static_require_order "$next_release_harness_source" \
	'exact post-lifecycle current-release chart export' \
	'CHART_PACKAGE="$CHART_PACKAGE_DIR/ptah-operator-${chart_version}.tgz"' \
	'E2E_CHART_PACKAGE=$CHART_PACKAGE' \
	'E2E_PHASE=uninstall' \
	'"$ROOT_DIR/hack/e2e-crd-upgrade.sh"' \
	'export_release_chart' \
	'printf '\''e2e: PASS Kubernetes=%s cluster=%s\n'\'' "$server_version" "$CLUSTER_NAME"'

# shellcheck disable=SC2016 # Exact lifecycle markers intentionally retain runtime variables literally.
for next_release_crd_marker in \
	'production_controller_image_from_values() {' \
	'.repository + "@" + .digest' \
	'create_late_failure_blocker() {' \
	'prove_late_failure_recovery() {' \
	'retry_same_candidate() {' \
	'prove_rollback_refused_over_future_state() {' \
	'prove_rollback() {' \
	'run_next_release_upgrade_proof() {' \
	'helm_e2e install "$E2E_HELM_RELEASE" "$E2E_NEXT_CHART_PACKAGE"' \
	'helm_e2e install "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE"' \
	'e2e crd: exact exported current-release chart passed fresh install and zero-residue uninstall'; do
	static_require_count "$next_release_crd_source" "$next_release_crd_marker" 1 \
		'synthetic next-release CRD lifecycle'
done
# The synthetic next release is applied twice: once behind the late-failure
# blocker, which refuses Helm's apply of the candidate Deployments after the
# hook stopped the runtime, and once for real.
# shellcheck disable=SC2016 # Exact upgrade marker retains runtime variables literally.
static_require_count "$next_release_crd_source" \
	'helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_NEXT_CHART_PACKAGE"' 2 \
	'late-failure attempt and real next-release upgrade'
# The definition, and one check after each of the three uninstalls.
static_require_count "$next_release_crd_source" \
	'assert_release_runtime_removed' 4 \
	'uninstall residue checks'
# shellcheck disable=SC2016 # Exact identity-capture destination retains a runtime variable literally.
static_require_count "$next_release_crd_source" \
	'"$WORK_DIR/reinstalled-next-release-controller-identity.json"' 1 \
	'next-release identity after the reinstall'
static_reject_marker "$next_release_crd_source" \
	'.repository + "@" + .testIdentityDigest' \
	'production controller image identity extraction'
# shellcheck disable=SC2016 # Ordered markers intentionally retain runtime variables literally.
static_require_order "$next_release_crd_source" \
	'current to next release upgrade, late failure, retry and rollback' \
	'run_next_release_upgrade_proof() {' \
	'capture_controller_service_account_identity' \
	'"$CURRENT_RELEASE_CONTROLLER_IMAGE"' \
	'prepare_expected_hook_names "$E2E_NEXT_CHART_PACKAGE" "$E2E_NEXT_VALUES_FILE"' \
	'start_running_apply_fixture' \
	'stage_predecessor_apply_job_uid_gap_while_running' \
	'prove_late_failure_recovery "$CURRENT_RELEASE_CONTROLLER_IMAGE"' \
	'stage_read_only_job_completion' \
	'stage_read_only_job_uid_gap' \
	'assert_late_failure_candidate_unchanged' \
	'delete_late_failure_blocker' \
	'retry_same_candidate' \
	'wait_for_read_only_job_cleanup' \
	'assert_predecessor_apply_remains_exclusive_while_running' \
	'release_running_apply_barrier' \
	'"$E2E_NEXT_CONTROLLER_IMAGE"' \
	'prove_rollback_refused_over_future_state "$current_release_revision"' \
	'prove_rollback "$current_release_revision" "$CURRENT_RELEASE_CONTROLLER_IMAGE"' \
	'e2e crd: synthetic next-release upgrade kept the controller identity, and the rollback to the current release went through its hook'
# The blocker has to be in place before the candidate is applied, and the
# evidence read is the revision the blocker failed.
# shellcheck disable=SC2016 # Ordered markers intentionally retain runtime variables literally.
static_require_order "$next_release_crd_source" \
	'late failure after the runtime stop' \
	'prove_late_failure_recovery() {' \
	'create_late_failure_blocker' \
	'if helm_e2e upgrade "$E2E_HELM_RELEASE" "$E2E_NEXT_CHART_PACKAGE"' \
	'late-failure-blocker.operator.ptah.run' \
	'--revision "$late_revision" -o json' \
	'.spec.replicas == 0 and'
# shellcheck disable=SC2016 # Ordered markers intentionally retain runtime variables literally.
static_require_order "$next_release_crd_source" \
	'next chart reinstall and final zero-residue proof' \
	'run_uninstall_proof() {' \
	'run_next_release_upgrade_proof' \
	'helm_e2e uninstall "$E2E_HELM_RELEASE"' \
	'assert_release_runtime_removed' \
	'helm_e2e install "$E2E_HELM_RELEASE" "$E2E_NEXT_CHART_PACKAGE"' \
	'"$WORK_DIR/reinstalled-next-release-controller-identity.json"' \
	'helm_e2e uninstall "$E2E_HELM_RELEASE"' \
	'assert_release_runtime_removed' \
	'e2e crd: fresh-installing the exact exported current-release chart bytes' \
	'helm_e2e install "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE"' \
	'"$WORK_DIR/fresh-current-release-controller-identity.json"' \
	'helm_e2e uninstall "$E2E_HELM_RELEASE"' \
	'assert_release_runtime_removed' \
	'e2e crd: exact exported current-release chart passed fresh install and zero-residue uninstall' \
	'e2e crd: uninstall retained CRDs and live objects'

# shellcheck disable=SC2016 # Exact handoff markers intentionally retain shell variables literally.
static_require_order "$(cat "$ROOT_DIR/hack/e2e-kind.sh")" \
	'external PostgreSQL credential handoff' \
	'E2E_DOCKER_CONTEXT=$DOCKER_CONTEXT' \
	'E2E_EXTERNAL_POSTGRES_CONTAINER_ID=$EXTERNAL_PG_CONTAINER_ID' \
	'E2E_EXTERNAL_POSTGRES_IP=$EXTERNAL_PG_IP' \
	'E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE=$EXTERNAL_PG_CREDENTIALS_FILE' \
	'E2E_PHASE=upgrade' \
	'"$ROOT_DIR/hack/e2e-crd-upgrade.sh"'

# shellcheck disable=SC2016 # Match the exact generated OpenAPI regular expression.
controller_revision_pattern='pattern: ^[^[:space:][:cntrl:]]([^[:cntrl:]]*[^[:space:][:cntrl:]])?$'
# The realm kind is an administrator's grant and carries no controller state,
# and a plan chunk carries bytes and nothing about who wrote them, which its
# plan records; so neither has to carry a controllerRevision. Neither is
# skipped: a revision field added to one later is held to the exact pattern
# like the rest.
for controller_revision_crd in "$ROOT_DIR"/config/crd/bases/*.yaml; do
	controller_revision_fields=$(grep -c '^[[:space:]]*controllerRevision:' "$controller_revision_crd" || true)
	controller_revision_patterns=$(grep -Fc "$controller_revision_pattern" "$controller_revision_crd" || true)
	controller_revision_minimum=1
	case "${controller_revision_crd##*/}" in
	operator.ptah.run_ptahrealms.yaml | operator.ptah.run_ptahschemaplanchunks.yaml)
		controller_revision_minimum=0
		;;
	*approvals.yaml | *acknowledgments.yaml)
		# An approval binds nothing about the manager that published the plan
		# or dispatched the Job, and an acknowledgment of a run nobody
		# accounted for binds nothing but the run, so neither carries a
		# revision at all.
		if [ "$controller_revision_fields" -ne 0 ]; then
			printf 'e2e static: %s carries a controllerRevision a decision must not bind\n' \
				"$controller_revision_crd" >&2
			exit 1
		fi
		continue
		;;
	esac
	if [ "$controller_revision_fields" -lt "$controller_revision_minimum" ] ||
		[ "$controller_revision_patterns" -ne "$controller_revision_fields" ]; then
		printf 'e2e static: %s lacks exact revision validation on every controllerRevision field\n' \
			"$controller_revision_crd" >&2
		exit 1
	fi
done

external_pg_create_section=$(sed -n \
	'/^# external-postgresql-container-create-begin$/,/^# external-postgresql-container-create-end$/p' \
	"$ROOT_DIR/hack/e2e-kind.sh")
external_pg_host_contract_section=$(sed -n '/^assert_external_pg_container_contract() {$/,/^}$/p' \
	"$ROOT_DIR/hack/e2e-kind.sh")
external_pg_mount_contract_section=$(sed -n '/^external_pg_mounts_are_ephemeral() {$/,/^}$/p' \
	"$ROOT_DIR/hack/e2e-kind.sh")
external_pg_app_query_section=$(sed -n '/^external_pg_app_query() {$/,/^}$/p' \
	"$ROOT_DIR/hack/e2e-kind.sh")
for required_static_section in \
	"$external_pg_create_section" "$external_pg_host_contract_section" \
	"$external_pg_mount_contract_section" "$external_pg_app_query_section"; do
	[ -n "$required_static_section" ] || {
		printf '%s\n' 'e2e static: a required external PostgreSQL bootstrap function is missing' >&2
		exit 1
	}
done

eval "$external_pg_mount_contract_section"
assert_external_pg_mount_contract_accepts() {
	printf '%s\n' "$1" | external_pg_mounts_are_ephemeral || {
		printf '%s\n' 'e2e static: external PostgreSQL mount contract rejected an ephemeral representation' >&2
		exit 1
	}
}
assert_external_pg_mount_contract_rejects() {
	if printf '%s\n' "$1" | external_pg_mounts_are_ephemeral; then
		printf '%s\n' 'e2e static: external PostgreSQL mount contract accepted persistent or unsafe storage' >&2
		exit 1
	fi
}
assert_external_pg_mount_contract_accepts \
	'{"HostConfig":{"Tmpfs":{"/var/lib/postgresql/data":"rw,noexec,nosuid,nodev,size=536870912"},"Binds":null,"Mounts":null,"VolumesFrom":null},"Mounts":[]}'
assert_external_pg_mount_contract_accepts \
	'{"HostConfig":{"Tmpfs":{"/var/lib/postgresql/data":"nodev,nosuid,noexec,size=536870912,rw"},"Binds":[],"Mounts":[],"VolumesFrom":[]},"Mounts":[{"Type":"tmpfs","Destination":"/var/lib/postgresql/data"}]}'
assert_external_pg_mount_contract_rejects \
	'{"HostConfig":{"Tmpfs":{"/var/lib/postgresql/data":"rw,noexec,nosuid,nodev,size=536870912"},"Binds":null,"Mounts":null,"VolumesFrom":null},"Mounts":[{"Type":"volume","Destination":"/var/lib/postgresql/data"}]}'
assert_external_pg_mount_contract_rejects \
	'{"HostConfig":{"Tmpfs":{"/var/lib/postgresql/data":"rw,noexec,nosuid,nodev,size=536870912"},"Binds":["/host:/var/lib/postgresql/data"],"Mounts":null,"VolumesFrom":null},"Mounts":[]}'
assert_external_pg_mount_contract_rejects \
	'{"HostConfig":{"Tmpfs":{"/var/lib/postgresql/data":"rw,nosuid,nodev,size=536870912"},"Binds":null,"Mounts":null,"VolumesFrom":null},"Mounts":[]}'

# Docker Go templates are already single-quoted. Backslash-escaped quotes are
# passed literally and make `docker inspect --format` fail before Helm install.
if printf '%s\n' "$external_pg_host_contract_section" | grep -F '\"' >/dev/null; then
	printf '%s\n' 'e2e static: external PostgreSQL Docker template contains literal escaped quotes' >&2
	exit 1
fi
for external_host_contract_marker in \
	'{{index .Config.Labels "operator.ptah.run/e2e-owner"}}' \
	'{{index .Config.Labels "operator.ptah.run/e2e-component"}}'; do
	printf '%s\n' "$external_pg_host_contract_section" |
		grep -F -- "$external_host_contract_marker" >/dev/null
done
# shellcheck disable=SC2016 # Exact source markers retain shell variables literally.
for external_pg_app_query_marker in \
	'cat "$EXTERNAL_PG_PASSWORD_FILE"' \
	'IFS= read -r PGPASSWORD' \
	'exec psql -h 127.0.0.1 -U "$1" -d "$2" -Atqc "$3"'; do
	printf '%s\n' "$external_pg_app_query_section" |
		grep -F -- "$external_pg_app_query_marker" >/dev/null
done
if printf '%s\n' "$external_pg_app_query_section" | grep -F 'POSTGRES_PASSWORD' >/dev/null; then
	printf '%s\n' 'e2e static: application queries reuse the bootstrap superuser credential' >&2
	exit 1
fi

# shellcheck disable=SC2016 # Exact source markers intentionally retain shell variables literally.
for external_create_marker in \
	'docker --context "$DOCKER_CONTEXT" create --restart=no' \
	'--network kind' \
	'--env-file "$EXTERNAL_PG_ENV_FILE"' \
	'--tmpfs '\''/var/lib/postgresql/data:rw,noexec,nosuid,nodev,size=536870912'\''' \
	'--label "operator.ptah.run/e2e-owner=${CLUSTER_NAME}"' \
	"--label 'operator.ptah.run/e2e-component=external-postgresql'" \
	'"$E2E_POSTGRES_SOURCE_IMAGE"'; do
	static_require_count "$external_pg_create_section" "$external_create_marker" 1 \
		'external PostgreSQL Docker create contract'
done
if printf '%s\n' "$external_pg_create_section" |
	grep -Eq -- '(^|[[:space:]])(--publish(-all)?|-p|--volume|-v)([=[:space:]]|$)'; then
	printf '%s\n' 'e2e static: external PostgreSQL Docker create exposes a host port or volume' >&2
	exit 1
fi

# shellcheck disable=SC2016 # Exact source markers intentionally retain shell variables literally.
static_require_order "$(cat "$ROOT_DIR/hack/e2e-kind.sh")" \
	'external PostgreSQL secret and identity lifecycle' \
	'EXTERNAL_PG_ENV_FILE=$WORK_DIR/external-postgresql.env' \
	'EXTERNAL_PG_ADMIN_PASSWORD_FILE=$WORK_DIR/external-postgresql-admin.password' \
	'EXTERNAL_PG_PASSWORD_FILE=$WORK_DIR/external-postgresql.password' \
	'EXTERNAL_PG_BOOTSTRAP_SQL_FILE=$WORK_DIR/external-postgresql-bootstrap.sql' \
	'printf '\''%s'\'' "$EXTERNAL_PG_PASSWORD" >"$EXTERNAL_PG_PASSWORD_FILE"' \
	'printf '\''%s'\'' "$EXTERNAL_PG_ADMIN_PASSWORD" >"$EXTERNAL_PG_ADMIN_PASSWORD_FILE"' \
	'printf '\''POSTGRES_USER=%s\n'\'' "$EXTERNAL_PG_ADMIN_USER"' \
	'cat "$EXTERNAL_PG_ADMIN_PASSWORD_FILE"' \
	'CREATE ROLE %s WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS' \
	'CREATE DATABASE %s OWNER %s' \
	'--rawfile password "$EXTERNAL_PG_PASSWORD_FILE"' \
	'"$EXTERNAL_PG_ENV_FILE" "$EXTERNAL_PG_ADMIN_PASSWORD_FILE"' \
	'unset EXTERNAL_PG_ADMIN_PASSWORD EXTERNAL_PG_PASSWORD EXTERNAL_PG_URL' \
	'# external-postgresql-container-create-begin' \
	'EXTERNAL_PG_CONTAINER_ID=$(docker --context "$DOCKER_CONTEXT" container inspect' \
	'docker --context "$DOCKER_CONTEXT" start "$EXTERNAL_PG_CONTAINER_ID"' \
	'EXTERNAL_PG_IP=$(docker --context "$DOCKER_CONTEXT" container inspect' \
	'assert_external_pg_container_contract "$EXTERNAL_PG_CONTAINER_ID" "$EXTERNAL_PG_IP"' \
	'external PostgreSQL fixture is not major version 17' \
	'<"$EXTERNAL_PG_BOOTSTRAP_SQL_FILE" >/dev/null' \
	'rm -f "$EXTERNAL_PG_ADMIN_PASSWORD_FILE" "$EXTERNAL_PG_BOOTSTRAP_SQL_FILE"' \
	'external PostgreSQL fixture login retained administrative attributes' \
	'external PostgreSQL fixture login does not own its database' \
	'E2E_EXTERNAL_POSTGRES_CONTAINER_ID=$EXTERNAL_PG_CONTAINER_ID'
# shellcheck disable=SC2016 # Exact forbidden source markers retain shell variables literally.
for external_secret_argv_marker in \
	'--arg password "$EXTERNAL_PG_PASSWORD"' \
	'--arg password "$EXTERNAL_PG_ADMIN_PASSWORD"' \
	'--env "PGPASSWORD=$EXTERNAL_PG_PASSWORD"' \
	'--env "PGPASSWORD=$EXTERNAL_PG_ADMIN_PASSWORD"' \
	'--arg url "$EXTERNAL_PG_URL"'; do
	static_reject_marker "$(cat "$ROOT_DIR/hack/e2e-kind.sh")" \
		"$external_secret_argv_marker" 'external PostgreSQL host process arguments'
done
# shellcheck disable=SC2016 # Exact source markers intentionally retain shell variables literally.
for external_cleanup_marker in \
	'external_cleanup_id=$EXTERNAL_PG_CONTAINER_ID' \
	'external_cleanup_owner=' \
	'external_cleanup_component=' \
	'docker --context "$DOCKER_CONTEXT" container rm -fv "$external_cleanup_id"'; do
	static_require_count "$(sed -n '/^cleanup() {$/,/^}/p' "$ROOT_DIR/hack/e2e-kind.sh")" \
		"$external_cleanup_marker" 1 'exact external PostgreSQL cleanup'
done
# shellcheck disable=SC2016 # Exact source markers intentionally retain shell variables literally.
for registry_cleanup_marker in \
	'registry_cleanup_id=$REGISTRY_CONTAINER_ID' \
	'registry_cleanup_owner=' \
	'registry_cleanup_component=' \
	'docker --context "$DOCKER_CONTEXT" container rm -fv "$registry_cleanup_id"'; do
	static_require_count "$(sed -n '/^cleanup() {$/,/^}/p' "$ROOT_DIR/hack/e2e-kind.sh")" \
		"$registry_cleanup_marker" 1 'exact registry cleanup'
done

# shellcheck disable=SC2016 # The handoff marker intentionally retains the shell variable literally.
static_require_count "$(cat "$ROOT_DIR/hack/e2e-kind.sh")" \
	'E2E_REGISTRY_PORT=$E2E_REGISTRY_PORT' 1 'registry readiness port handoff'

# A loop that is an if condition runs in this shell, so an exit inside it ends
# the phase rather than the loop, and the phase dies with no message at all.
# The shape reads as "run the loop and take its answer", which is why it was
# written; refuse it, and carry the answer in a variable instead.
for phase_script in "$ROOT_DIR"/hack/e2e-*.sh; do
	if grep -nE '^[[:space:]]*if[[:space:]]+(!)?[[:space:]]*while[[:space:]]' \
		"$phase_script" >/dev/null; then
		printf 'e2e static: %s uses a loop as an if condition; an exit inside it ends the phase\n' \
			"${phase_script##*/}" >&2
		exit 1
	fi
done
# The immutable verification policy is created by the bootstrap now, because
# every suite's resources refer to it. Both halves are still required: the
# bootstrap writes it immutable, and the control-plane phase in Go refuses to
# run without it and holds it to the committed file.
grep -F "jq '.immutable = true'" "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null || {
	printf '%s\n' 'e2e static: the bootstrap does not create the verification policy as immutable' >&2
	exit 1
}
jq -n '
  def secretEnv($name; $key):
    {name: $name, valueFrom: {secretKeyRef: {name: "registry-auth", key: $key}}};
  {spec: {template: {spec: {containers: [{
    name: "publisher",
    image: "e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000",
    env: [
      {name: "HOME", value: "/work"},
      {name: "TMPDIR", value: "/work"},
      secretEnv("PTAH_OCI_USERNAME"; "username"),
      secretEnv("PTAH_OCI_PASSWORD"; "password"),
      secretEnv("PTAH_OCI_REGISTRY"; "registry")
    ]
  }]}}}}
' >"$PUBLISHER_JOB_FIXTURE"
jq -e \
	--arg image e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--arg registrySecret registry-auth \
	-f "$ROOT_DIR/testdata/e2e/publisher-job-isolation.jq" \
	"$PUBLISHER_JOB_FIXTURE" >/dev/null
jq '
  .spec.template.spec.containers[0].env += [{
    name: "PTAH_DEV_URL",
    valueFrom: {secretKeyRef: {name: "database-url", key: "url"}}
  }]
' "$PUBLISHER_JOB_FIXTURE" >"$NEGATIVE_FIXTURE"
if jq -e \
	--arg image e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--arg registrySecret registry-auth \
	-f "$ROOT_DIR/testdata/e2e/publisher-job-isolation.jq" \
	"$NEGATIVE_FIXTURE" >/dev/null; then
	printf '%s\n' 'e2e static: publisher isolation accepted a development database credential' >&2
	exit 1
fi

if helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca \
	>/dev/null 2>"$MISSING_PTAH_VERSION_ERROR"; then
	printf '%s\n' 'e2e static: chart silently assigned a version to an arbitrary executor digest' >&2
	exit 1
fi
grep -F 'ptahVersion' "$MISSING_PTAH_VERSION_ERROR" >/dev/null || {
	printf '%s\n' 'e2e static: missing executor version did not produce an actionable schema error' >&2
	exit 1
}

if helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--skip-schema-validation \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca \
	>/dev/null 2>"$MISSING_PTAH_VERSION_TEMPLATE_ERROR"; then
	printf '%s\n' 'e2e static: chart template bypass silently assigned an executor version' >&2
	exit 1
fi
grep -F 'execution.ptahVersion is required and must identify the build in execution.executorImage' \
	"$MISSING_PTAH_VERSION_TEMPLATE_ERROR" >/dev/null || {
	printf '%s\n' 'e2e static: template-level executor version failure is not actionable' >&2
	exit 1
}

if helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca \
	>/dev/null 2>"$MUTABLE_MANAGER_ERROR"; then
	printf '%s\n' 'e2e static: chart accepted an unpinned manager image' >&2
	exit 1
fi
grep -F 'image.digest must pin the manager' "$MUTABLE_MANAGER_ERROR" >/dev/null

assert_rejected_manager_image_values() {
	manager_image_error=$1
	shift
	if helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
		--namespace ptah-e2e \
		--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
		--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
		--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
		--set-string webhook.existingSecret=e2e-webhook-cert \
		--set-string webhook.caBundle=e2e-ca \
		"$@" >/dev/null 2>"$REJECTED_MANAGER_IMAGE_ERROR"; then
		printf '%s\n' 'e2e static: chart accepted an unsupported manager image configuration' >&2
		exit 1
	fi
	grep -F "$manager_image_error" "$REJECTED_MANAGER_IMAGE_ERROR" >/dev/null || {
		printf 'e2e static: manager image rejection did not identify %s\n' "$manager_image_error" >&2
		exit 1
	}
}

for manager_image_validation in schema template; do
	set --
	mutable_tag_error=allowMutableTag
	test_identity_error=testIdentityDigest
	if [ "$manager_image_validation" = template ]; then
		set -- --skip-schema-validation
		mutable_tag_error='image.allowMutableTag is not a chart value; use image.digest with a registry manifest digest'
		test_identity_error='image.testIdentityDigest is not a chart value; use image.digest with a registry manifest digest'
	fi
	assert_rejected_manager_image_values "$mutable_tag_error" "$@" \
		--set image.allowMutableTag=true \
		--set-string image.testIdentityDigest=sha256:3333333333333333333333333333333333333333333333333333333333333333
	assert_rejected_manager_image_values "$mutable_tag_error" "$@" \
		--set image.allowMutableTag=true \
		--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222
	assert_rejected_manager_image_values "$test_identity_error" "$@" \
		--set-string image.testIdentityDigest=sha256:3333333333333333333333333333333333333333333333333333333333333333
	assert_rejected_manager_image_values "$test_identity_error" "$@" \
		--set-string image.testIdentityDigest=sha256:3333333333333333333333333333333333333333333333333333333333333333 \
		--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222
	assert_rejected_manager_image_values 'digest' "$@" \
		--set-string image.digest=sha256:invalid
done

helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--show-only templates/deployment.yaml \
	--show-only templates/crd-upgrade.yaml \
	--show-only templates/certificate-rotation.yaml \
	--set-string image.repository=registry.local:5000/ptah-operator \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	>"$LOCAL_REGISTRY_MANAGER_RENDER"
local_registry_manager=registry.local:5000/ptah-operator@sha256:2222222222222222222222222222222222222222222222222222222222222222
rendered_manager_images=$(awk '/^[[:space:]]+image: / { print $2 }' \
	"$LOCAL_REGISTRY_MANAGER_RENDER" | sort -u)
[ "$rendered_manager_images" = "$local_registry_manager" ] || {
	printf '%s\n' 'e2e static: runtime and hook images do not share the registry manifest identity' >&2
	exit 1
}
for manager_image_argument in manager-image controller-image; do
	rendered_manager_identity=$(awk -v argument="$manager_image_argument" '
    $0 ~ "^[[:space:]]+- \"?--" argument "=" {
      sub("^.*--" argument "=", "")
      sub(/"$/, "")
      print
    }
  ' "$LOCAL_REGISTRY_MANAGER_RENDER" | sort -u)
	[ "$rendered_manager_identity" = "$local_registry_manager" ] || {
		printf 'e2e static: --%s does not match the runtime and hook image\n' "$manager_image_argument" >&2
		exit 1
	}
done

if helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--set replicaCount=2 \
	--set leaderElection=false \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca \
	>/dev/null 2>"$LEADER_ELECTION_ERROR"; then
	printf '%s\n' 'e2e static: chart accepted multiple replicas without leader election' >&2
	exit 1
fi
grep -F 'replicaCount' "$LEADER_ELECTION_ERROR" >/dev/null

helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--show-only templates/deployment.yaml \
	--set replicaCount=1 \
	--set leaderElection=false \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca >"$NO_ELECTION_DEPLOYMENT_RENDER"
[ "$(grep -c '^    type: Recreate$' "$NO_ELECTION_DEPLOYMENT_RENDER")" -eq 1 ] || {
	printf '%s\n' 'e2e static: manager without leader election does not use Recreate rollout' >&2
	exit 1
}
if grep -F 'rollingUpdate:' "$NO_ELECTION_DEPLOYMENT_RENDER" >/dev/null; then
	printf '%s\n' 'e2e static: manager without leader election still permits a surge rollout' >&2
	exit 1
fi
grep -F -- '--leader-elect=false' "$NO_ELECTION_DEPLOYMENT_RENDER" >/dev/null

helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--show-only templates/deployment.yaml \
	--set replicaCount=2 \
	--set leaderElection=true \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca >"$HA_DEPLOYMENT_RENDER"
[ "$(grep -c '^    type: Recreate$' "$HA_DEPLOYMENT_RENDER")" -eq 1 ] || {
	printf '%s\n' 'e2e static: elected HA manager permits mixed revisions during rollout' >&2
	exit 1
}
if grep -F 'rollingUpdate:' "$HA_DEPLOYMENT_RENDER" >/dev/null; then
	printf '%s\n' 'e2e static: elected HA manager still permits a mixed-revision surge' >&2
	exit 1
fi
grep -F -- '--leader-elect=true' "$HA_DEPLOYMENT_RENDER" >/dev/null

helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--show-only templates/rbac.yaml \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca >"$DEFAULT_RBAC_RENDER"

helm template ptah-e2e-ha "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e-ha \
	--show-only templates/rbac.yaml \
	--set-string coordination.namespace=ptah-e2e \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca >"$SHARED_RBAC_RENDER"

for rbac_render in "$DEFAULT_RBAC_RENDER" "$SHARED_RBAC_RENDER"; do
	[ "$(grep -c '^kind: Role$' "$rbac_render")" -eq 1 ] || {
		printf 'e2e static: %s does not render exactly one scoped manager Role\n' "$rbac_render" >&2
		exit 1
	}
	[ "$(grep -c '^kind: RoleBinding$' "$rbac_render")" -eq 1 ] || {
		printf 'e2e static: %s does not render exactly one scoped manager RoleBinding\n' "$rbac_render" >&2
		exit 1
	}
	if awk '
      /^kind: ClusterRole$/ {cluster_role = 1; next}
      /^---$/ {cluster_role = 0}
      cluster_role && /resources: \["leases"\]/ {found = 1}
      END {exit found ? 0 : 1}
    ' "$rbac_render"; then
		printf 'e2e static: %s grants cluster-wide manager Lease access\n' "$rbac_render" >&2
		exit 1
	fi
	lease_verbs=$(awk '
      /resources: \["leases"\]/ {
        if (getline > 0) print
        exit
      }
    ' "$rbac_render" | tr -d '[:space:]')
	[ "$lease_verbs" = 'verbs:["get","create","update"]' ] || {
		printf 'e2e static: %s manager Lease verbs are not exact\n' "$rbac_render" >&2
		exit 1
	}
	ptahschema_verbs=$(awk '
      /^kind: ClusterRole$/ {cluster_role = 1; next}
      cluster_role && /^---$/ {exit}
      cluster_role && /resources: \["ptahschemas"\]/ {
        if (getline > 0) print
        exit
      }
    ' "$rbac_render" | tr -d '[:space:]')
	[ "$ptahschema_verbs" = 'verbs:["get","list","watch","patch"]' ] || {
		printf 'e2e static: %s main PtahSchema verbs are not exact guarded patch access\n' "$rbac_render" >&2
		exit 1
	}
	for admission_read_contract in \
		'serviceaccounts verbs:["get"]' \
		'limitranges verbs:["list"]'; do
		admission_resource=${admission_read_contract%% *}
		admission_verbs=${admission_read_contract#* }
		if ! awk -v resource="$admission_resource" -v verbs="$admission_verbs" '
          /^kind: ClusterRole$/ {cluster_role = 1; next}
          cluster_role && /^---$/ {exit}
          cluster_role && index($0, "resources: [\"" resource "\"]") {
            if (getline > 0) {
              compact = $0
              gsub(/[[:space:]]/, "", compact)
              if (compact == verbs) found = 1
            }
          }
          END {exit found ? 0 : 1}
        ' "$rbac_render"; then
			printf 'e2e static: %s controller ClusterRole lacks exact %s %s admission access\n' \
				"$rbac_render" "$admission_resource" "$admission_verbs" >&2
			exit 1
		fi
	done
done

# shellcheck disable=SC2016 # Match literal impersonation variables in the live proof.
for controller_write_live_marker in \
	'prove_controller_write_guard' \
	'prove_controller_direct_write_webhook' \
	'directly read plan manifest' \
	'--as-user-extra "authentication.kubernetes.io/pod-name=$CONTROLLER_IMPERSONATION_POD_NAME"' \
	'--as-user-extra "authentication.kubernetes.io/pod-uid=$CONTROLLER_IMPERSONATION_POD_UID"' \
	'expect_controller_write_denial spec' \
	'expect_controller_write_denial labels' \
	'expect_controller_write_denial annotations' \
	'expect_controller_write_denial ownerReferences' \
	"expect_controller_write_denial 'a foreign finalizer'" \
	'operator.ptah.run/active-operation' \
	'controller desired-state and direct-write boundaries passed'; do
	grep -F -- "$controller_write_live_marker" "$ROOT_DIR/hack/e2e-crd-upgrade.sh" >/dev/null
done

default_role_namespace=$(awk '
  function finish() {
    if (kind == "Role" && leases) print namespace
    kind = ""; namespace = ""; leases = 0
  }
  /^---$/ {finish(); next}
  /^kind:/ {kind = $2; next}
  /^  namespace:/ {namespace = $2; next}
  /resources: \["leases"\]/ {leases = 1}
  END {finish()}
' "$DEFAULT_RBAC_RENDER")
[ "$default_role_namespace" = ptah-e2e ] || {
	printf 'e2e static: default manager Lease Role namespace = %s, want ptah-e2e\n' \
		"$default_role_namespace" >&2
	exit 1
}
shared_role_namespace=$(awk '
  function finish() {
    if (kind == "Role" && leases) print namespace
    kind = ""; namespace = ""; leases = 0
  }
  /^---$/ {finish(); next}
  /^kind:/ {kind = $2; next}
  /^  namespace:/ {namespace = $2; next}
  /resources: \["leases"\]/ {leases = 1}
  END {finish()}
' "$SHARED_RBAC_RENDER")
[ "$shared_role_namespace" = ptah-e2e ] || {
	printf 'e2e static: shared manager Lease Role namespace = %s, want ptah-e2e\n' \
		"$shared_role_namespace" >&2
	exit 1
}
shared_subject_namespace=$(awk '
  /^kind: RoleBinding$/ {binding = 1; next}
  binding && /^    namespace:/ {print $2; exit}
' "$SHARED_RBAC_RENDER")
[ "$shared_subject_namespace" = ptah-e2e-ha ] || {
	printf 'e2e static: cross-namespace Lease RoleBinding subject = %s, want ptah-e2e-ha\n' \
		"$shared_subject_namespace" >&2
	exit 1
}

helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
		--namespace ptah-e2e \
		--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
		--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca >"$RENDERED_WEBHOOKS"
helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--show-only templates/webhook.yaml \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=ZTJlLWNh >"$ADMISSION_RENDER"
[ "$(grep -Fc 'resources: ["ptahschemas/finalizers", "ptahschemaplans/finalizers"]' \
	"$RENDERED_WEBHOOKS")" -eq 1 ] || {
	printf '%s\n' 'e2e static: rendered controller role lacks its exact blocking-owner finalizer resources' >&2
	exit 1
}
finalizer_verbs=$(awk '
  index($0, "resources: [\"ptahschemas/finalizers\", \"ptahschemaplans/finalizers\"]") {
    if (getline > 0) print
    exit
  }
' "$RENDERED_WEBHOOKS" | tr -d '[:space:]')
[ "$finalizer_verbs" = 'verbs:["update"]' ] || {
	printf '%s\n' 'e2e static: rendered controller role lacks exact owner-finalizer update authorization' >&2
	exit 1
}
[ "$(grep -c '^kind: MutatingWebhookConfiguration$' "$ADMISSION_RENDER")" -eq 1 ]
[ "$(grep -c '^kind: ValidatingWebhookConfiguration$' "$ADMISSION_RENDER")" -eq 1 ]
# approvals.requireDistinctApprover defaults to false, and the spec-writer
# entries exist only when it is on: 3 mutating (approval, migration approval,
# run acknowledgment) plus 5 validating, none of them failurePolicy anything
# but Fail.
[ "$(grep -c '^[[:space:]]*failurePolicy: Fail$' "$ADMISSION_RENDER")" -eq 8 ]
if grep -Fq 'name: mschemawriter.operator.ptah.run' "$ADMISSION_RENDER" ||
	grep -Fq 'name: mmigrationwriter.operator.ptah.run' "$ADMISSION_RENDER"; then
	printf '%s\n' 'e2e static: the default-off release renders the spec-writer webhook entries' >&2
	exit 1
fi
grep -F 'name: mmigrationapproval.operator.ptah.run' "$ADMISSION_RENDER" >/dev/null
grep -F 'name: vmigrationapproval.operator.ptah.run' "$ADMISSION_RENDER" >/dev/null
grep -F 'resources: ["ptahmigrationapprovals"]' "$ADMISSION_RENDER" >/dev/null
grep -F 'name: mmigrationrunacknowledgment.operator.ptah.run' "$ADMISSION_RENDER" >/dev/null
grep -F 'name: vmigrationrunacknowledgment.operator.ptah.run' "$ADMISSION_RENDER" >/dev/null
grep -F 'resources: ["ptahmigrationrunacknowledgments"]' "$ADMISSION_RENDER" >/dev/null

# The two spec-writer entries render only when approvals.requireDistinctApprover
# is on, and the manager's own --require-distinct-approver flag, the CRD hook's
# runtime-verify flag of the same name (once for the manager Deployment's own
# init container, once for the rotator's), and the rotator's probed webhook
# names all move with it. webhook.existingSecret omits certificate-rotation.yaml
# entirely (built-in rotation is "automatically omitted" then), so this reads
# the built-in-rotation render instead of $RENDERED_WEBHOOKS/$ADMISSION_RENDER.
DISTINCT_APPROVER_OFF_RENDER=$WORK_DIR/full-distinct-approver-off.yaml
helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	>"$DISTINCT_APPROVER_OFF_RENDER"
DISTINCT_APPROVER_ON_RENDER=$WORK_DIR/full-distinct-approver-on.yaml
helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	--set approvals.requireDistinctApprover=true >"$DISTINCT_APPROVER_ON_RENDER"
DISTINCT_APPROVER_ON_ADMISSION_RENDER=$WORK_DIR/admission-distinct-approver-on.yaml
helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--show-only templates/webhook.yaml \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=ZTJlLWNh \
	--set approvals.requireDistinctApprover=true >"$DISTINCT_APPROVER_ON_ADMISSION_RENDER"
[ "$(grep -c '^[[:space:]]*failurePolicy: Fail$' "$DISTINCT_APPROVER_ON_ADMISSION_RENDER")" -eq 10 ]
grep -F 'name: mschemawriter.operator.ptah.run' "$DISTINCT_APPROVER_ON_ADMISSION_RENDER" >/dev/null
grep -F 'path: /mutate-operator-ptah-run-v1alpha1-ptahschema' "$DISTINCT_APPROVER_ON_ADMISSION_RENDER" >/dev/null
grep -F 'name: mmigrationwriter.operator.ptah.run' "$DISTINCT_APPROVER_ON_ADMISSION_RENDER" >/dev/null
grep -F 'path: /mutate-operator-ptah-run-v1alpha1-ptahmigration' "$DISTINCT_APPROVER_ON_ADMISSION_RENDER" >/dev/null
[ "$(grep -Fc -- '--require-distinct-approver=false' "$DISTINCT_APPROVER_OFF_RENDER")" -eq 3 ] || {
	printf '%s\n' 'e2e static: --require-distinct-approver=false does not reach the manager and both runtime-verify hooks' >&2
	exit 1
}
[ "$(grep -Fc -- '--require-distinct-approver=true' "$DISTINCT_APPROVER_ON_RENDER")" -eq 3 ] || {
	printf '%s\n' 'e2e static: --require-distinct-approver=true does not reach the manager and both runtime-verify hooks' >&2
	exit 1
}
grep -F -- '--mutating-webhook-names=mapproval.operator.ptah.run,mmigrationapproval.operator.ptah.run,mmigrationrunacknowledgment.operator.ptah.run,mschemawriter.operator.ptah.run,mmigrationwriter.operator.ptah.run' \
	"$DISTINCT_APPROVER_ON_RENDER" >/dev/null ||
	{
		printf '%s\n' 'e2e static: the rotator does not probe the spec-writer entries when the four-eyes control is on' >&2
		exit 1
	}
if grep -Fq -- '--mutating-webhook-names=mapproval.operator.ptah.run,mmigrationapproval.operator.ptah.run,mmigrationrunacknowledgment.operator.ptah.run,mschemawriter.operator.ptah.run,mmigrationwriter.operator.ptah.run' \
	"$DISTINCT_APPROVER_OFF_RENDER"; then
	printf '%s\n' 'e2e static: the rotator still probes the spec-writer entries with the four-eyes control off' >&2
	exit 1
fi
grep -F -- '--mutating-webhook-names=mapproval.operator.ptah.run,mmigrationapproval.operator.ptah.run,mmigrationrunacknowledgment.operator.ptah.run"' \
	"$DISTINCT_APPROVER_OFF_RENDER" >/dev/null ||
	{
		printf '%s\n' 'e2e static: the rotator does not probe exactly the approval and acknowledgment entries with the four-eyes control off' >&2
		exit 1
	}
controller_service_account_name=$(awk '
  $1 == "operator.ptah.run/controller-service-account-name:" {
    gsub(/"/, "", $2)
    print $2
    exit
  }
' "$ADMISSION_RENDER")
printf '%s\n' "$controller_service_account_name" |
	grep -Eq '^ptah-e2e-ptah-operator$'
[ "$(grep -Fc -- \
	"operator.ptah.run/controller-service-account-name: \"$controller_service_account_name\"" \
	"$ADMISSION_RENDER")" -eq 2 ]
grep -F 'name: vpodintent.operator.ptah.run' "$ADMISSION_RENDER" >/dev/null
grep -F 'path: /validate-v1-pod-ptah-operation-intent' "$ADMISSION_RENDER" >/dev/null
grep -F 'resources: ["pods", "pods/ephemeralcontainers", "pods/resize"]' "$ADMISSION_RENDER" >/dev/null
grep -F 'operations: ["CREATE", "UPDATE"]' "$ADMISSION_RENDER" >/dev/null
grep -F 'name: managed-or-operation-job-pod' "$ADMISSION_RENDER" >/dev/null
grep -F 'name: vcontrollerwrite.operator.ptah.run' "$ADMISSION_RENDER" >/dev/null
grep -F 'path: /validate-operator-controller-write' "$ADMISSION_RENDER" >/dev/null
grep -F 'name: controller-service-account' "$ADMISSION_RENDER" >/dev/null
grep -F 'request.userInfo.username ==' "$ADMISSION_RENDER" >/dev/null
grep -F "'system:serviceaccount:ptah-e2e:$controller_service_account_name'" \
	"$ADMISSION_RENDER" >/dev/null
grep -F 'resources: ["jobs"]' "$ADMISSION_RENDER" >/dev/null
grep -F 'resources: ["configmaps"]' "$ADMISSION_RENDER" >/dev/null
grep -F 'resources: ["ptahschemaplans", "ptahschemaplanchunks", "ptahmigrationplans"]' "$ADMISSION_RENDER" >/dev/null
if grep -Eq '^[[:space:]]*objectSelector:' "$ADMISSION_RENDER"; then
	printf '%s\n' 'e2e static: admission webhooks must not trust user-controlled object selectors' >&2
	exit 1
fi
rendered_webhook_block() {
	awk -v target="$1" '
      /^  - name: / {
        if (selected) exit
        selected = ($3 == target)
      }
      selected { print }
	' "$ADMISSION_RENDER"
}
controller_write_webhook=$(rendered_webhook_block vcontrollerwrite.operator.ptah.run)
[ "$(printf '%s\n' "$controller_write_webhook" | sed -n 's/^[[:space:]]*timeoutSeconds: //p')" = 30 ] || {
	printf '%s\n' 'e2e static: controller-write webhook does not use its dedicated 30-second fail-closed timeout' >&2
	exit 1
}
for approval_webhook_name in mapproval.operator.ptah.run vapproval.operator.ptah.run; do
	approval_webhook=$(rendered_webhook_block "$approval_webhook_name")
	[ -n "$approval_webhook" ] || {
		printf 'e2e static: rendered approval webhook %s is missing\n' "$approval_webhook_name" >&2
		exit 1
	}
	if printf '%s\n' "$approval_webhook" | grep -Eq '^[[:space:]]*objectSelector:'; then
		printf 'e2e static: approval webhook %s has an object selector\n' "$approval_webhook_name" >&2
		exit 1
	fi
done
pod_intent_webhook=$(rendered_webhook_block vpodintent.operator.ptah.run)
[ -n "$pod_intent_webhook" ] || {
	printf '%s\n' 'e2e static: rendered Pod intent webhook is missing' >&2
	exit 1
}
if printf '%s\n' "$pod_intent_webhook" | grep -Eq '^[[:space:]]*objectSelector:'; then
	printf '%s\n' 'e2e static: Pod intent webhook has a bypassable object selector' >&2
	exit 1
fi
grep -F -- '--default-tolerations-enabled=true' "$RENDERED_WEBHOOKS" >/dev/null
grep -F -- '--extended-resource-toleration-enabled=false' "$RENDERED_WEBHOOKS" >/dev/null
grep -F -- '--always-pull-images-enabled=false' "$RENDERED_WEBHOOKS" >/dev/null
grep -F -- '--require-distinct-approver=false' "$RENDERED_WEBHOOKS" >/dev/null
rendered_manager_container=$(awk '
  /^# Source: ptah-operator\/templates\/deployment[.]yaml$/ {deployment = 1; next}
  deployment && /^---$/ {exit}
  deployment && $0 == "        - name: manager" {manager = 1}
  manager && /^      volumes:$/ {exit}
  manager {print}
' "$RENDERED_WEBHOOKS")
[ "$(printf '%s\n' "$rendered_manager_container" |
	grep -Fc -- "--ptah-version=$STATIC_PTAH_VERSION")" -eq 1 ] || {
	printf '%s\n' 'e2e static: rendered manager does not bind the one explicit executor version' >&2
	exit 1
}
for admission_resource in \
	'resources: ["serviceaccounts"]' \
	'resources: ["limitranges"]' \
	'resources: ["runtimeclasses"]' \
	'resources: ["priorityclasses"]'; do
	grep -F "$admission_resource" "$RENDERED_WEBHOOKS" >/dev/null
done
pods_verbs=$(awk '
  index($0, "resources: [\"pods\"]") {
    if (getline > 0) print
    exit
  }
' "$RENDERED_WEBHOOKS" | tr -d '[:space:]')
[ "$pods_verbs" = 'verbs:["get","list","watch"]' ] || {
	printf '%s\n' 'e2e static: controller Pod RBAC exceeds read-only evidence access' >&2
	exit 1
}
if grep -Eq '^[[:space:]]*failurePolicy: (Ignore|FailOpen)$' "$ADMISSION_RENDER"; then
	printf '%s\n' 'e2e static: rendered admission webhooks are not fail-closed' >&2
	exit 1
fi

helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--show-only templates/certificate-rotation.yaml \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	>"$ROTATOR_RENDER"
[ "$(grep -c '^kind: Deployment$' "$ROTATOR_RENDER")" -eq 1 ]
if grep -Eq '^kind: (Job|CronJob)$' "$ROTATOR_RENDER"; then
	printf '%s\n' 'e2e static: certificate rotator is blocked by Job Pod admission' >&2
	exit 1
fi
for rotator_marker in \
		'--run-interval=6h' \
	'--operation-timeout=15m' \
	'--retry-initial=5s' \
	'--retry-max=5m' \
	'--health-bind-address=:8081' \
	'path: /healthz' \
	'path: /readyz' \
	'name: health' \
	'containerPort: 8081'; do
	grep -F -- "$rotator_marker" "$ROTATOR_RENDER" >/dev/null
done
rotator_crd_verbs=$(awk '
  index($0, "resources: [\"customresourcedefinitions\"]") {
    while (getline > 0) {
      if (index($0, "verbs:")) {print; exit}
    }
  }
' "$ROTATOR_RENDER" | tr -d '[:space:]')
[ "$rotator_crd_verbs" = 'verbs:["get"]' ] || {
	printf '%s\n' 'e2e static: certificate rotator CRD verifier has mutation or list access' >&2
	exit 1
}
# The rotator reads no admission policy and no scheduling or quota object: the
# verifier that needed them is gone.
for default_forbidden_marker in \
		'kind: ValidatingAdmissionPolicy' \
		'kind: ValidatingAdmissionPolicyBinding' \
		'validatingadmissionpolicies' \
		'priorityclasses' \
		'limitranges' \
		'resources: ["serviceaccounts"]' \
		'resources: ["configmaps"]' \
		'verbs: ["create"]' \
		'--recreate-missing-secret'; do
	if grep -F -- "$default_forbidden_marker" "$ROTATOR_RENDER" >/dev/null; then
		printf 'e2e static: default certificate lifecycle contains opt-in marker %s\n' \
			"$default_forbidden_marker" >&2
		exit 1
	fi
done

helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--show-only templates/certificate-rotation.yaml \
	--show-only templates/certificate-secret-guard.yaml \
	--set certificateRotation.recreateMissingSecret=true \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	>"$ROTATOR_RECREATE_RENDER"
for recreation_marker in \
		'kind: ValidatingAdmissionPolicy' \
		'kind: ValidatingAdmissionPolicyBinding' \
		'failurePolicy: Fail' \
		'validationActions: [Deny]' \
		'verbs: ["create"]' \
		'operator.ptah.run/generated-webhook-certificate' \
		'certificate rotator Secret CREATE is outside its exact recovery contract' \
		'--recreate-missing-secret=true'; do
	grep -F -- "$recreation_marker" "$ROTATOR_RECREATE_RENDER" >/dev/null
done
# Recreation grants Secret CREATE and nothing on the guard itself: the API
# server enforces the policy on the rotator's request, and the rotator never
# reads it back.
if grep -F -- 'validatingadmissionpolicies' "$ROTATOR_RECREATE_RENDER" >/dev/null; then
	printf '%s\n' 'e2e static: certificate Secret recreation grants the rotator access to admission policies' >&2
	exit 1
fi
# Off by default: with recreation not opted into there is no create grant to
# narrow, and the guard's template renders nothing, which Helm reports as a
# template it cannot find.
if helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--show-only templates/certificate-secret-guard.yaml \
	--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
	--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	>/dev/null 2>&1; then
	printf '%s\n' 'e2e static: the certificate Secret CREATE guard renders without recreateMissingSecret' >&2
	exit 1
fi
grep -F 'StartedChecker()' "$ROOT_DIR/cmd/manager/main.go" >/dev/null
[ "$(grep -Fc 'recreateMissingSecret: true' "$ROOT_DIR/hack/e2e-kind.sh")" -eq 1 ]
for per_entry_marker in \
	'"mutating"' \
	'"approvalValidating"' \
	'"podValidating"' \
	'ptah-operator.webhookEntryCABundle'; do
	grep -F -- "$per_entry_marker" "$ROOT_DIR/charts/ptah-operator/templates/webhook.yaml" >/dev/null
done
if grep -F 'longestExistingBundle' "$ROOT_DIR/charts/ptah-operator/templates/webhook.yaml" >/dev/null; then
	printf '%s\n' 'e2e static: Helm webhook trust recovery selects or cross-copies a global bundle' >&2
	exit 1
fi

if helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
		--namespace ptah-e2e \
		--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222 \
		--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
	--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
	--set-string execution.ptahVersion="$STATIC_PTAH_VERSION" \
	--set-string webhook.existingSecret=e2e-webhook-cert \
	--set-string webhook.caBundle=e2e-ca \
	--set-string webhook.failurePolicy=Ignore \
	>"$OBSOLETE_RENDER" 2>"$OBSOLETE_ERROR"; then
	[ "$(grep -c '^[[:space:]]*failurePolicy: Fail$' "$OBSOLETE_RENDER")" -eq 3 ] || {
		printf '%s\n' 'e2e static: obsolete webhook.failurePolicy changed the rendered fail-closed contract' >&2
		exit 1
	}
	if grep -Eq '^[[:space:]]*failurePolicy: (Ignore|FailOpen)$' "$OBSOLETE_RENDER"; then
		printf '%s\n' 'e2e static: obsolete webhook.failurePolicy enabled fail-open admission' >&2
		exit 1
	fi
else
	grep -F 'failurePolicy' "$OBSOLETE_ERROR" >/dev/null || {
		printf '%s\n' 'e2e static: obsolete webhook.failurePolicy render failed for an unrelated reason' >&2
		exit 1
	}
fi

# The versions the CRDs must carry are the ones the Makefile stamps them with,
# read rather than restated: a number written here again is a number that stops
# agreeing with the stamp the moment either one moves.
EXPECTED_CRD_SCHEMA_VERSION=$(sed -n 's/^CRD_SCHEMA_VERSION := //p' "$ROOT_DIR/Makefile")
EXPECTED_CONTROLLER_STATE_VERSION=$(sed -n 's/^CONTROLLER_STATE_VERSION := //p' "$ROOT_DIR/Makefile")
case "$EXPECTED_CRD_SCHEMA_VERSION$EXPECTED_CONTROLLER_STATE_VERSION" in
'' | *[!0-9]*)
	printf '%s\n' 'e2e static: the Makefile does not declare both stamped versions as numbers' >&2
	exit 1
	;;
esac

for crd_file in "$ROOT_DIR"/config/crd/bases/*.yaml; do
	crd_basename=${crd_file##*/}
	cmp "$crd_file" "$ROOT_DIR/charts/ptah-operator/crds/$crd_basename"
	cmp "$crd_file" "$ROOT_DIR/internal/crdupgrade/assets/$crd_basename"
	[ "$(grep -Fc "operator.ptah.run/controller-state-version: \"$EXPECTED_CONTROLLER_STATE_VERSION\"" "$crd_file")" -eq 1 ]
	[ "$(grep -Fc "operator.ptah.run/crd-schema-version: \"$EXPECTED_CRD_SCHEMA_VERSION\"" "$crd_file")" -eq 1 ]
	[ "$(grep -Ec 'operator[.]ptah[.]run/crd-schema-digest: "sha256:[0-9a-f]{64}"' "$crd_file")" -eq 1 ]
done
[ "$(find "$ROOT_DIR/config/crd/bases" -type f -name '*.yaml' | wc -l | tr -d '[:space:]')" = 9 ]
[ "$(find "$ROOT_DIR/internal/crdupgrade/assets" -type f -name '*.yaml' | wc -l | tr -d '[:space:]')" = 9 ]
for crd_directory in \
	"$ROOT_DIR/config/crd/bases" \
	"$ROOT_DIR/charts/ptah-operator/crds" \
	"$ROOT_DIR/internal/crdupgrade/assets"; do
	if find "$crd_directory" -type f -name '*.yaml' ! -perm 0644 -print | grep -q .; then
		printf 'e2e static: generated CRD assets in %s do not have deterministic mode 0644\n' \
			"$crd_directory" >&2
		exit 1
	fi
done
# Declared, and a number. Not a particular number: the values move when a CRD
# schema or the controller state contract moves, and the stamp above already
# holds the generated files to whatever the Makefile says.
grep -Eq '^CRD_SCHEMA_VERSION := [0-9]+$' "$ROOT_DIR/Makefile"
grep -Eq '^CONTROLLER_STATE_VERSION := [0-9]+$' "$ROOT_DIR/Makefile"

# Every e2e proof reads that declaration rather than restating it. These three
# forms are where restating it does real damage: the annotation a release
# carries, the stored state a downgrade proof manufactures, and the value a
# phase is handed. Each reads correctly and agrees with the chart on the day it
# is written, and the proofs then say the opposite of what they claim -- state
# manufactured to be newer than the manager stops being newer, so the upgrade
# a proof exists to refuse is admitted and the proof passes.
CONTROLLER_STATE_LITERAL='operator[.]ptah[.]run/controller-state-version[]" ]*[=:][^$]*[0-9]'
CONTROLLER_STATE_LITERAL=$CONTROLLER_STATE_LITERAL'|executionBinding/controllerStateVersion\\?",\\?"value\\?":[0-9]'
CONTROLLER_STATE_LITERAL=$CONTROLLER_STATE_LITERAL'|E2E_CONTROLLER_STATE_VERSION=[0-9]'
# Reading the pattern again catches a reasoning error and misses the one that
# matters, so it is run over the four mistakes that were actually made and the
# four derived forms that replaced them.
controller_state_literal_refused=$(grep -cE "$CONTROLLER_STATE_LITERAL" <<'CONTROLLER_STATE_MISTAKES' || true
	operator.ptah.run/controller-state-version=2 --overwrite
	.metadata.annotations["operator.ptah.run/controller-state-version"] = "1" |
	--type=json -p='[{"op":"replace","path":"/status/executionBinding/controllerStateVersion","value":2}]'
	E2E_CONTROLLER_STATE_VERSION=1 \
CONTROLLER_STATE_MISTAKES
)
[ "$controller_state_literal_refused" -eq 4 ] || {
	printf 'e2e static: the controller-state literal filter refused %s of 4 known mistakes\n' \
		"$controller_state_literal_refused" >&2
	exit 1
}
controller_state_literal_accepted=$(grep -cE "$CONTROLLER_STATE_LITERAL" <<'CONTROLLER_STATE_DERIVED' || true
	"operator.ptah.run/controller-state-version=$NEWER_CONTROLLER_STATE_VERSION" --overwrite
	.metadata.annotations["operator.ptah.run/controller-state-version"] == $state and
	--type=json -p="[{\"op\":\"replace\",\"path\":\"/status/executionBinding/controllerStateVersion\",\"value\":$CONTROLLER_STATE_VERSION}]"
	E2E_CONTROLLER_STATE_VERSION=$CONTROLLER_STATE_VERSION \
CONTROLLER_STATE_DERIVED
)
[ "$controller_state_literal_accepted" -eq 0 ] || {
	printf 'e2e static: the controller-state literal filter refused %s derived forms\n' \
		"$controller_state_literal_accepted" >&2
	exit 1
}
for controller_state_script in "$ROOT_DIR"/hack/e2e-*.sh; do
	# This file holds the pattern, so scanning it would match the pattern
	# itself. Its own two uses are the derived form, checked above.
	case "${controller_state_script##*/}" in
		e2e-static.sh) continue ;;
	esac
	if grep -nE "$CONTROLLER_STATE_LITERAL" "$controller_state_script"; then
		printf 'e2e static: %s writes a controller-state version as a number\n' \
			"${controller_state_script##*/}" >&2
		exit 1
	fi
done
# shellcheck disable=SC2016 # Match the literal deterministic-mode command in the generator.
grep -F 'chmod 0644 "$STAMP_TEMP"' "$ROOT_DIR/hack/stamp-crd-schema-version.sh" >/dev/null
grep -F 'ComputeSchemaDigest(crd)' "$ROOT_DIR/hack/crdschemadigest/main.go" >/dev/null
grep -F 'verify-crd-schema-history:' "$ROOT_DIR/Makefile" >/dev/null
# shellcheck disable=SC2016 # Match the literal Make recipe rather than expanding it here.
grep -F '$(GO) run ./hack/verifycrdschemahistory' "$ROOT_DIR/Makefile" >/dev/null
# shellcheck disable=SC2016 # Match literal GitHub expression bindings in the audited workflow.
for crd_history_ci_marker in \
	'fetch-depth: 0' \
	'PULL_REQUEST_BASE_SHA: ${{ github.event.pull_request.base.sha }}' \
	'EVENT_BEFORE_SHA: ${{ github.event.before }}' \
	'CRD_SCHEMA_BASELINE_REF: ${{ steps.crd-baseline.outputs.baseline }}' \
	'CRD_SCHEMA_REQUIRE_EXPLICIT_BASELINE: "true"'; do
	grep -F -- "$crd_history_ci_marker" "$ROOT_DIR/.github/workflows/ci.yml" >/dev/null
done

crd_render_args='--set-string image.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222
--set-string execution.executorImage=e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000
--set-string execution.runnerImage=e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111
--set-string execution.ptahVersion=e2e-explicit-version'
# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" --namespace ptah-e2e \
	--show-only templates/crd-upgrade.yaml $crd_render_args >"$CRD_INSTALL_RENDER"
# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" --namespace ptah-e2e --is-upgrade \
	--show-only templates/crd-upgrade.yaml $crd_render_args >"$CRD_UPGRADE_RENDER"
# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" --namespace ptah-e2e \
	$crd_render_args >"$CRD_FULL_RENDER"
# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" --namespace ptah-e2e \
	--set-string webhook.existingSecret=external-tls \
	--set-string webhook.caBundle=Y2E= \
	$crd_render_args >"$EXTERNAL_CERTIFICATE_RENDER"
# Each guard family is rendered on its own, so the markers below read exactly
# its own policies. The Deployment comes along because the guards name the
# ServiceAccount it runs as. The templates are generated from the Go
# definitions (make chart-policies), and verify-source holds them to the
# generator; what is read here is that the release values reach the policies.
# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" --namespace ptah-e2e \
	--show-only templates/controller-write-guard.yaml \
	--show-only templates/deployment.yaml \
	$crd_render_args >"$CONTROLLER_WRITE_GUARD_RENDER"
# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" --namespace ptah-e2e \
	--show-only templates/controller-object-guard.yaml \
	--show-only templates/deployment.yaml \
	$crd_render_args >"$CONTROLLER_OBJECT_GUARD_RENDER"
# The apply-policy guard, three ways: as the chart ships it, with two exempt
# groups an installer named, and turned off. What the fourth render proves is a
# refusal: system:authenticated is everyone, and a chart that rendered it into
# the exempt list would ship a guard that judges nobody.
# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" --namespace ptah-e2e \
	--show-only templates/apply-policy-guard.yaml \
	$crd_render_args >"$APPLY_POLICY_GUARD_RENDER"
# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" --namespace ptah-e2e \
	--show-only templates/apply-policy-guard.yaml \
	--set 'applyPolicyGuard.exemptGroups={platform:apply-policy,system:serviceaccounts:flux-system}' \
	$crd_render_args >"$APPLY_POLICY_GUARD_GROUPS_RENDER"
# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" --namespace ptah-e2e \
	--set applyPolicyGuard.enabled=false \
	$crd_render_args >"$APPLY_POLICY_GUARD_OFF_RENDER"
# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
if helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" --namespace ptah-e2e \
	--set 'applyPolicyGuard.exemptGroups={system:authenticated}' \
	$crd_render_args >/dev/null 2>"$APPLY_POLICY_GUARD_EVERYONE_ERROR"; then
	printf '%s\n' 'e2e static: the chart exempted every authenticated identity from the apply-policy guard' >&2
	exit 1
fi
grep -F 'applyPolicyGuard.exemptGroups names system:authenticated' "$APPLY_POLICY_GUARD_EVERYONE_ERROR" >/dev/null || {
	printf '%s\n' 'e2e static: the refusal of system:authenticated does not name the value to change' >&2
	exit 1
}

# The hook's objects are named after the release fullname, stable across
# upgrades like the controller's own ServiceAccount, so a fullname chosen to
# collide with the hook's own suffix makes the controller and the hook one
# ServiceAccount. The chart refuses it rather than render it.
crd_hook_name=$(awk '
  /^kind:/ {kind = $2}
  kind == "ServiceAccount" && /^  name:/ {
    print $2
    exit
  }
' "$CRD_UPGRADE_RENDER")
printf '%s\n' "$crd_hook_name" | grep -Eq -- '^ptah-e2e-ptah-operator-crd-manager$' || {
	printf 'e2e static: the CRD hook ServiceAccount is named %s, not stably after the release\n' \
		"$crd_hook_name" >&2
	exit 1
}
# 51 characters, so the hook's own trunc-51 base reproduces this fullname
# exactly and its "-crd-manager" suffix then collides with the fullname itself.
fixed_point_fullname="abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxy-crd-manager"
# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
if helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" --namespace ptah-e2e \
	--show-only templates/crd-upgrade.yaml \
	--set-string "fullnameOverride=$fixed_point_fullname" \
	$crd_render_args >/dev/null 2>"$HOOK_FULLNAME_COLLISION_ERROR"; then
	printf '%s\n' 'e2e static: the chart accepted a fullname that makes the controller and the CRD hook one ServiceAccount' >&2
	exit 1
fi
grep -F 'lifecycle resource identity collision:' "$HOOK_FULLNAME_COLLISION_ERROR" >/dev/null
# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
if helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" --namespace ptah-e2e \
	--set serviceAccount.create=false \
	--set-string 'serviceAccount.name=INVALID_SERVICE_ACCOUNT' \
	$crd_render_args >/dev/null 2>"$INVALID_SERVICE_ACCOUNT_ERROR"; then
	printf '%s\n' 'e2e static: chart accepted an invalid external controller ServiceAccount name' >&2
	exit 1
fi
grep -F '/serviceAccount/name' "$INVALID_SERVICE_ACCOUNT_ERROR" >/dev/null

# The upgrade runs one hook Job. It reads the stored state, stops the runtime
# when the manager image changes and updates the CRDs, and it runs on install,
# upgrade and rollback alike.
grep -F -- '- "reconcile"' "$CRD_INSTALL_RENDER" >/dev/null
[ "$(grep -Fc -- '- "reconcile"' "$CRD_UPGRADE_RENDER")" -eq 1 ]
[ "$(grep -Fxc 'kind: Job' "$CRD_UPGRADE_RENDER")" -eq 1 ]
[ "$(grep -Fc -- '- "--timeout=360s"' "$CRD_UPGRADE_RENDER")" -eq 1 ]
[ "$(grep -Fc -- 'activeDeadlineSeconds: 390' "$CRD_UPGRADE_RENDER")" -eq 1 ]
# The hook refuses a chart whose controller-state version its image does not
# compile, so the chart has to hand it the one the binary is built with, read
# from where the binary reads it.
for crd_reconcile_argument in \
	'- "--release-name=ptah-e2e"' \
	'- "--release-namespace=ptah-e2e"' \
	'- "--controller-deployment-name=ptah-e2e-ptah-operator"' \
	'- "--certificate-deployment-name=ptah-e2e-ptah-operator-cert-rotator"' \
	'- "--manager-image=ghcr.io/stokaro/ptah-operator@sha256:2222222222222222222222222222222222222222222222222222222222222222"' \
	"- \"--controller-state-version=$EXPECTED_CONTROLLER_STATE_VERSION\""; do
	[ "$(grep -Fc -- "$crd_reconcile_argument" "$CRD_UPGRADE_RENDER")" -eq 1 ] || {
		printf 'e2e static: the CRD reconcile hook lacks %s\n' "$crd_reconcile_argument" >&2
		exit 1
	}
done
assert_crd_manager_job_container_contract "$CRD_UPGRADE_RENDER" 1 || {
	printf '%s\n' 'e2e static: CRD hook containers expose an unsafe termination or restart contract' >&2
	exit 1
}
crd_hook_resource_count=$(grep -Ec '^apiVersion:' "$CRD_UPGRADE_RENDER")
[ "$crd_hook_resource_count" -eq 6 ]
[ "$(grep -Fxc '    helm.sh/hook: pre-install,pre-upgrade,pre-rollback' "$CRD_UPGRADE_RENDER")" -eq "$crd_hook_resource_count" ]
[ "$(grep -Fc 'helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded,hook-failed' "$CRD_UPGRADE_RENDER")" -eq "$crd_hook_resource_count" ]
# Helm deletes the failed Job, so its log is the only place its refusal reaches
# the person who ran the command.
[ "$(grep -Fxc '    helm.sh/hook-output-log-policy: hook-failed' "$CRD_UPGRADE_RENDER")" -eq 1 ] || {
	printf '%s\n' 'e2e static: the CRD reconcile hook does not print its log when it fails' >&2
	exit 1
}
[ "$(grep -Fc 'image: ghcr.io/stokaro/ptah-operator@sha256:2222222222222222222222222222222222222222222222222222222222222222' "$CRD_FULL_RENDER")" -ge 3 ]
crd_role_section=$(awk '
  function emit() {
    if (cluster_role && manager) printf "%s", document
    document = ""
    cluster_role = 0
    manager = 0
  }
  /^---$/ {emit(); next}
  {document = document $0 ORS}
  $0 == "kind: ClusterRole" {cluster_role = 1}
  index($0, "app.kubernetes.io/component: crd-manager") {manager = 1}
  END {emit()}
' "$CRD_UPGRADE_RENDER")
for crd_name in \
	ptahschemaapprovals.operator.ptah.run \
	ptahschemaplanchunks.operator.ptah.run \
	ptahschemaplans.operator.ptah.run \
	ptahschemas.operator.ptah.run; do
	[ "$(printf '%s\n' "$crd_role_section" | grep -Fc -- "- $crd_name")" -eq 1 ]
done
printf '%s\n' "$crd_role_section" | grep -F 'verbs: ["get", "update"]' >/dev/null
printf '%s\n' "$crd_role_section" |
	grep -F 'resources: ["ptahschemas", "ptahschemaplans", "ptahschemaapprovals", "ptahmigrations", "ptahmigrationplans", "ptahmigrationapprovals"]' >/dev/null
[ "$(printf '%s\n' "$crd_role_section" | grep -Fc 'verbs: ["list"]')" -eq 1 ]
# The controller runs as one ServiceAccount in every release, so no binding
# moves between releases and the hook reaches no RBAC object at all.
if printf '%s\n' "$crd_role_section" | grep -F 'apiGroups: ["rbac.authorization.k8s.io"]' >/dev/null; then
	printf '%s\n' 'e2e static: fresh-install CRD manager ClusterRole reaches an RBAC object' >&2
	exit 1
fi
# The hook creates nothing cluster-wide. Its one create grant was the access
# review it no longer sends, and a review grant is authority no step uses.
[ "$(printf '%s\n' "$crd_role_section" | grep -Fc 'verbs: ["create"]')" -eq 0 ] || {
	printf '%s\n' 'e2e static: fresh-install CRD manager ClusterRole grants create' >&2
	exit 1
}
if printf '%s\n' "$crd_role_section" |
	grep -Eq 'verbs:.*(bind|escalate|delete|watch)|resources:.*(\*|endpointslices|"roles"|subjectaccessreviews)'; then
	printf '%s\n' 'e2e static: fresh-install CRD manager ClusterRole contains an unsafe verb, wildcard, cluster-wide namespaced access, or an unused access review grant' >&2
	exit 1
fi
for crd_runtime_marker in \
	'command: ["/ptah-crd-manager"]' \
	'name: verify-candidate-runtime' \
	'- "runtime-verify"' \
	'- "--release-name=ptah-e2e"' \
	'- "--release-namespace=ptah-e2e"' \
	'- "--coordination-namespace=ptah-e2e"' \
	'- "--leader-election=true"' \
	'- "--leader-election-id=ptah-operator.operator.ptah.run"' \
	'- "--webhook-service-name=ptah-e2e-ptah-operator-webhook"' \
	'- "--webhook-timeout-seconds=5"'; do
	grep -F -- "$crd_runtime_marker" "$CRD_FULL_RENDER" >/dev/null
done
[ "$(grep -Fc -- '- "runtime-verify"' "$CRD_FULL_RENDER")" -eq 2 ]
assert_webhook_runtime_argument_owners "$CRD_FULL_RENDER" || {
	printf '%s\n' 'e2e static: webhook runtime arguments do not have the exact expected workload owners' >&2
	exit 1
}
[ "$(grep -Fc -- '- "--verify-controller-state=true"' "$CRD_FULL_RENDER")" -eq 1 ]
grep -F -- '--controller-image=ghcr.io/stokaro/ptah-operator@sha256:2222222222222222222222222222222222222222222222222222222222222222' \
	"$CRD_FULL_RENDER" >/dev/null
[ "$(grep -Fc 'resources: ["customresourcedefinitions"]' "$CRD_FULL_RENDER")" -eq 3 ]
[ "$(grep -Fc 'resourceNames: ["ptah-operator-admission"]' "$CRD_FULL_RENDER")" -eq 3 ]
# Every hook the chart renders is the CRD reconcile hook and its identity, or
# a CRD the release adds. The uninstall runs none: Helm deletes the release's
# objects, and the CRDs stay.
crd_bootstrap_count=$(find "$ROOT_DIR/charts/ptah-operator/crds" -type f -name '*.yaml' | wc -l | tr -d '[:space:]')
[ "$(grep -Fc 'helm.sh/hook:' "$CRD_FULL_RENDER")" -eq $((crd_hook_resource_count + crd_bootstrap_count)) ] || {
	printf '%s\n' 'e2e static: the chart renders a hook other than the CRD reconcile hook, its identity and the CRD bootstrap' >&2
	exit 1
}
[ "$(grep -Fc 'helm.sh/hook: pre-delete' "$CRD_FULL_RENDER")" -eq 0 ] || {
	printf '%s\n' 'e2e static: the chart renders a pre-delete hook' >&2
	exit 1
}
for singleton_annotation in \
	'operator.ptah.run/release-name: "ptah-e2e"' \
	'operator.ptah.run/release-namespace: "ptah-e2e"' \
	'operator.ptah.run/coordination-namespace: "ptah-e2e"' \
	'operator.ptah.run/leader-election: "true"' \
	'operator.ptah.run/leader-election-id: "ptah-operator.operator.ptah.run"' \
	'operator.ptah.run/webhook-service-name: "ptah-e2e-ptah-operator-webhook"' \
	'operator.ptah.run/controller-deployment-name: "ptah-e2e-ptah-operator"' \
	'operator.ptah.run/certificate-deployment-name: "ptah-e2e-ptah-operator-cert-rotator"' \
	"operator.ptah.run/controller-state-version: \"$EXPECTED_CONTROLLER_STATE_VERSION\"" \
	'operator.ptah.run/admission-contract-version: "2"'; do
	[ "$(grep -Fc -- "$singleton_annotation" "$ADMISSION_RENDER")" -eq 2 ]
done
hook_service_account_name=$(awk '
  $1 == "operator.ptah.run/hook-service-account-name:" {
    gsub(/"/, "", $2)
    print $2
    exit
  }
' "$ADMISSION_RENDER")
[ "$hook_service_account_name" = "ptah-e2e-ptah-operator-crd-manager" ]
[ "$(grep -Fc -- \
	"operator.ptah.run/hook-service-account-name: \"$hook_service_account_name\"" \
	"$ADMISSION_RENDER")" -eq 2 ]
# The release keeps nine admission policies -- six on the manager's own
# writes, the apply-policy guard, and the two that keep status and the copy of
# an unresolved run the manager's -- and each is an ordinary release object: no
# hook annotation, no keep policy and no parameter. Their
# names carry the release's own digest and nothing that changes between its
# upgrades, so an upgrade updates each in place and the uninstall deletes it.
controller_guard_policy_names() {
	awk '
      /^kind: ValidatingAdmissionPolicy$/ {policy = 1; next}
      /^kind:/ {policy = 0}
      policy && /^  name: / {print $2; policy = 0}
    ' "$1" | sort
}
controller_guard_names=$(controller_guard_policy_names "$CRD_FULL_RENDER")
[ "$(printf '%s\n' "$controller_guard_names" | grep -c .)" -eq 9 ] || {
	printf '%s\n' 'e2e static: the release does not render exactly nine admission policies' >&2
	exit 1
}
[ "$(grep -Fxc 'kind: ValidatingAdmissionPolicyBinding' "$CRD_FULL_RENDER")" -eq 9 ] || {
	printf '%s\n' 'e2e static: the release does not render exactly nine admission policy bindings' >&2
	exit 1
}
for controller_guard_family in \
	controller-write-guard \
	job-write-guard \
	chunk-write-guard \
	projection-write-guard \
	plan-write-guard \
	migration-plan-write-guard \
	apply-policy-guard \
	status-write-guard \
	unresolved-run-guard; do
	[ "$(printf '%s\n' "$controller_guard_names" |
		grep -Ec "^ptah-operator-${controller_guard_family}-[0-9a-f]{12}\$")" -eq 1 ] || {
		printf 'e2e static: the release does not render one %s policy\n' "$controller_guard_family" >&2
		exit 1
	}
done
[ "$(printf '%s\n' "$controller_guard_names" | sed 's/.*-//' | sort -u | grep -c .)" -eq 1 ] || {
	printf '%s\n' 'e2e static: the admission policies do not share the release digest' >&2
	exit 1
}
for controller_guard_name in $controller_guard_names; do
	[ "$(grep -Fxc -- "  name: $controller_guard_name" "$CRD_FULL_RENDER")" -eq 2 ] || {
		printf 'e2e static: controller guard %s does not have one policy and one binding\n' \
			"$controller_guard_name" >&2
		exit 1
	}
	[ "$(grep -Fxc -- "  policyName: $controller_guard_name" "$CRD_FULL_RENDER")" -eq 1 ] || {
		printf 'e2e static: no single binding targets controller guard %s\n' "$controller_guard_name" >&2
		exit 1
	}
done
# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
controller_guard_successor_names=$(helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
	--namespace ptah-e2e \
	--show-only templates/controller-write-guard.yaml \
	--show-only templates/controller-object-guard.yaml \
	--show-only templates/apply-policy-guard.yaml \
	--show-only templates/manager-state-guard.yaml \
	$crd_render_args \
	--set-string image.digest=sha256:3333333333333333333333333333333333333333333333333333333333333333 |
	controller_guard_policy_names /dev/stdin)
[ "$controller_guard_successor_names" = "$controller_guard_names" ] || {
	printf '%s\n' 'e2e static: another manager image renames the admission policies, so an upgrade would replace them' >&2
	exit 1
}
# The apply-policy guard as the chart ships it: both kinds, both writes that
# can set the field, its default exempt group as a literal, the transition
# rule rather than the value, and a refusal that says what to do instead.
[ "$(grep -Fxc 'kind: ValidatingAdmissionPolicy' "$APPLY_POLICY_GUARD_RENDER")" -eq 1 ] &&
	[ "$(grep -Fxc 'kind: ValidatingAdmissionPolicyBinding' "$APPLY_POLICY_GUARD_RENDER")" -eq 1 ] || {
	printf '%s\n' 'e2e static: the apply-policy guard is not one policy and one binding' >&2
	exit 1
}
for apply_policy_guard_marker in \
	'app.kubernetes.io/component: apply-policy-guard' \
	'failurePolicy: Fail' \
	'matchPolicy: Equivalent' \
	'operations: ["CREATE", "UPDATE"]' \
	'resources: ["ptahschemas", "ptahmigrations"]' \
	'expression: "[\"system:masters\"]"' \
	'variables.exemptGroups.exists(group, group in request.userInfo.groups)' \
	'object.spec.policy.apply == "Always"' \
	'oldObject != null && has(oldObject.spec.policy)' \
	'variables.requesterIsExempt || !variables.selectsAlways || variables.alreadyAlways' \
	'reserves that choice for the groups that own apply policy. Use OnApproval, or ask an apply-policy administrator' \
	'reason: Forbidden' \
	'validationActions: [Deny]'; do
	grep -F -- "$apply_policy_guard_marker" "$APPLY_POLICY_GUARD_RENDER" >/dev/null || {
		printf 'e2e static: the apply-policy guard lacks %s\n' "$apply_policy_guard_marker" >&2
		exit 1
	}
done
# Every group an installer names reaches the literal, in order and quoted.
grep -F -- 'expression: "[\"platform:apply-policy\", \"system:serviceaccounts:flux-system\"]"' \
	"$APPLY_POLICY_GUARD_GROUPS_RENDER" >/dev/null || {
	printf '%s\n' 'e2e static: the exempt groups an installer names do not reach the apply-policy guard' >&2
	exit 1
}
# Off, the release renders the other eight policies and no trace of this one,
# so an upgrade that turns it off removes it.
[ "$(controller_guard_policy_names "$APPLY_POLICY_GUARD_OFF_RENDER" | grep -c .)" -eq 8 ] &&
	[ "$(grep -Fxc 'kind: ValidatingAdmissionPolicyBinding' "$APPLY_POLICY_GUARD_OFF_RENDER")" -eq 8 ] || {
	printf '%s\n' 'e2e static: turning the apply-policy guard off does not leave exactly the other eight policies' >&2
	exit 1
}
if grep -F 'apply-policy-guard' "$APPLY_POLICY_GUARD_OFF_RENDER" >/dev/null; then
	printf '%s\n' 'e2e static: the apply-policy guard is rendered while turned off' >&2
	exit 1
fi
if grep -Eq '^  (paramKind|paramRef):' "$CRD_FULL_RENDER"; then
	printf '%s\n' 'e2e static: an admission policy still reads a parameter' >&2
	exit 1
fi
if grep -F 'ptah-operator-release-activation' "$CRD_FULL_RENDER" >/dev/null; then
	printf '%s\n' 'e2e static: the chart still renders the release activation' >&2
	exit 1
fi
for controller_write_marker in \
	'dyn(object).spec == dyn(oldObject).spec' \
	'dyn(object).status == dyn(oldObject).status' \
	'resources: ["ptahschemas", "ptahmigrations"]' \
	'request.resource.resource == \"ptahmigrations\" ? \"operator.ptah.run/migration-operation\" : \"operator.ptah.run/active-operation\"' \
	'Ptah controller write guard rejected a desired-state mutation'; do
	grep -F -- "$controller_write_marker" "$CONTROLLER_WRITE_GUARD_RENDER" >/dev/null
done
grep -F -- \
	"request.userInfo.username == \\\"system:serviceaccount:ptah-e2e:$controller_service_account_name\\\"" \
	"$CONTROLLER_WRITE_GUARD_RENDER" >/dev/null
# The object guards admit what this release's manager writes: its image and
# its controller-state version are literals in the policy, where a parameter
# used to carry them.
for controller_object_marker in \
	'- name: releaseControllerImage' \
	'expression: "\"ghcr.io/stokaro/ptah-operator@sha256:2222222222222222222222222222222222222222222222222222222222222222\""' \
	'- name: releaseControllerStateString' \
	"expression: \"\\\"$EXPECTED_CONTROLLER_STATE_VERSION\\\"\"" \
	'- name: releaseControllerState' \
	"expression: \"$EXPECTED_CONTROLLER_STATE_VERSION\"" \
	'== variables.releaseControllerImage' \
	'== variables.releaseControllerStateString' \
	'== variables.releaseControllerState' \
	'resources: ["jobs"]' \
	'resources: ["configmaps"]' \
	'resources: ["ptahschemaplanchunks"]' \
	'resources: ["ptahschemaplans"]' \
	'resources: ["ptahmigrationplans"]' \
	'Ptah controller migration plan write guard rejected an unsafe manifest shape' \
	'dyn(object).spec.ttlSecondsAfterFinished == 300' \
	'dyn(object).binaryData[\"chunk\"].size() <= 699052' \
	'dyn(object).spec.data.size() <= 699052' \
	'dyn(object).spec.contractVersion == 3' \
	'Ptah controller Job write guard rejected an unsafe workload shape' \
	'Ptah controller chunk write guard rejected an unsafe PtahSchemaPlanChunk shape' \
	'Ptah controller projection write guard rejected an unsafe ConfigMap shape' \
	'Ptah controller plan write guard rejected an unsafe manifest shape'; do
	grep -F -- "$controller_object_marker" "$CONTROLLER_OBJECT_GUARD_RENDER" >/dev/null || {
		printf 'e2e static: the controller object guards lack %s\n' "$controller_object_marker" >&2
		exit 1
	}
done
# Only the current Job envelope and plan contract are admitted: a Job without
# controller provenance and a plan older than contract 3 are refused.
for retired_controller_object_marker in \
	'variables.previousRelease' \
	'variables.activeRelease' \
	'params.' \
	'dyn(object).spec.contractVersion == 2'; do
	if grep -F -- "$retired_controller_object_marker" "$CONTROLLER_OBJECT_GUARD_RENDER" >/dev/null; then
		printf 'e2e static: controller object guard still reads %s\n' "$retired_controller_object_marker" >&2
		exit 1
	fi
done
(cd "$ROOT_DIR" && \
	PTAH_ADMISSION_RENDER="$ADMISSION_RENDER" \
	PTAH_PRIVILEGE_RENDER="$CRD_FULL_RENDER" \
	GOCACHE="${GOCACHE:-$WORK_DIR/gocache}" \
	go test ./internal/crdupgrade \
		-run '^(TestRenderedAdmissionSingletonMatchesRuntimeContract|TestRenderedReleaseRBACMatchesCompiledContract)$' -count=1)
(cd "$ROOT_DIR" && \
	PTAH_PRIVILEGE_RENDER="$EXTERNAL_CERTIFICATE_RENDER" \
	PTAH_RBAC_CERTIFICATE_RUNTIME_ENABLED=false \
	GOCACHE="${GOCACHE:-$WORK_DIR/gocache}" \
	go test ./internal/crdupgrade -run '^TestRenderedReleaseRBACMatchesCompiledContract$' -count=1)
# Exercise each namespace merge branch in the release grants.
for namespace_pair in default:ptah-coordination ptah-e2e:default; do
	release_namespace=${namespace_pair%:*}
	coordination_namespace=${namespace_pair#*:}
	merged_privilege_render=$WORK_DIR/privilege-${release_namespace}-${coordination_namespace}.yaml
	# shellcheck disable=SC2086 # Static argument lines intentionally become separate Helm arguments.
	helm template ptah-e2e "$ROOT_DIR/charts/ptah-operator" \
		--namespace "$release_namespace" \
		--set-string "coordination.namespace=$coordination_namespace" \
		$crd_render_args >"$merged_privilege_render"
	(cd "$ROOT_DIR" && \
		PTAH_PRIVILEGE_RENDER="$merged_privilege_render" \
		PTAH_RBAC_RELEASE_NAMESPACE="$release_namespace" \
		PTAH_RBAC_COORDINATION_NAMESPACE="$coordination_namespace" \
		GOCACHE="${GOCACHE:-$WORK_DIR/gocache}" \
		go test ./internal/crdupgrade \
			-run '^TestRenderedReleaseRBACMatchesCompiledContract$' -count=1)
done
for singleton_guard_marker in \
	'lookup "admissionregistration.k8s.io/v1" "MutatingWebhookConfiguration"' \
	'lookup "admissionregistration.k8s.io/v1" "ValidatingWebhookConfiguration"' \
	'is not owned by Helm release' \
	'has an incomplete owned annotation tuple'; do
	grep -F -- "$singleton_guard_marker" "$ROOT_DIR/charts/ptah-operator/templates/_helpers.tpl" >/dev/null
done
grep -E 'leaderElectionID[[:space:]]*=[[:space:]]*"ptah-operator.operator.ptah.run"' \
	"$ROOT_DIR/cmd/manager/main.go" >/dev/null
for image_file in "$ROOT_DIR/Dockerfile" "$ROOT_DIR/test/e2e/Dockerfile.operator"; do
	grep -F '/out/ptah-crd-manager ./cmd/ptah-crd-manager' "$image_file" >/dev/null
	grep -F 'COPY --from=builder /out/ptah-crd-manager /ptah-crd-manager' "$image_file" >/dev/null
done
for crd_live_marker in \
	'E2E_PHASE=upgrade' \
	'E2E_PHASE=uninstall' \
	'upgrade with a missing CRD' \
	'CRD hook recreated a missing CRD' \
	'proving a newer CRD schema version blocks rollback' \
	'upgrade with a newer CRD schema version' \
	'proving an incomplete schema identity and a digest collision are refused' \
	'upgrade with a missing schema digest' \
	'upgrade with a same-version schema digest collision' \
	'outdated e2e schema' \
	'UID, spec, or status changed during CRD management' \
	'upgrade against future controller state' \
	'changed despite failed CRD preflight' \
	'failed CRD preflight rewrote future controller state' \
	'a second operator release was installed' \
	'proving an active release survives losing both runtime Deployments' \
	'the upgrade that restores both deleted runtime Deployments was refused' \
	'holding one Apply open across the next-release upgrade' \
	'the successor did not adopt the running Apply under the epoch it was dispatched in' \
	'the successor replaced, completed, or cleaned the running Apply Job' \
	'the successor did not account for the adopted Apply Job through its own result' \
	'coordination namespace mutation' \
	'leader-election mutation' \
	'proving the chart refuses a release namespace that runs foreign workloads' \
	'shared release namespace upgrade' \
	'failed without the shared-namespace refusal naming it' \
	'releaseNamespace.allowSharedNamespace=true did not admit the upgrade over a foreign CronJob' \
	'runtime rejection of an incomplete singleton' \
	'incomplete admission singleton' \
	'proving the runtime refuses an admission singleton another release owns' \
	'foreign admission singleton owner' \
	'runtime rejection of drifted admission behavior' \
	'drifted admission behavior' \
	'failurePolicy","value":"Ignore' \
	'clientConfig/service/name","value":"foreign-service' \
	'proving controller downgrade preflight' \
	'controller-only downgrade preflight prevented the certificate rotator from remaining ready' \
	'blocked candidate manager rewrote future PtahSchema state' \
	'reinstalling over retained and drifted CRDs' \
	'the reinstall did not reconcile a retained CRD another manager drifted' \
	'fresh-installing the exact exported current-release chart bytes' \
	'the exact released-chart install did not reconcile a retained CRD another manager drifted' \
	'exact exported current-release chart passed fresh install and zero-residue uninstall' \
	'uninstall retained CRDs and live objects' \
	'the late failure left the runtime stopped on the predecessor template' \
	'the late failure did not come after a reconcile hook that succeeded' \
	'the same-candidate retry did not complete the upgrade' \
	'the refused rollback did not reach its pre-rollback hook' \
	'the refused rollback changed a runtime Deployment' \
	'the rollback was refused before any Pod changed' \
	'did not end deployed'; do
	grep -F -- "$crd_live_marker" "$ROOT_DIR/hack/e2e-kind.sh" \
		"$ROOT_DIR/hack/e2e-crd-upgrade.sh" >/dev/null || {
		printf 'e2e static: live CRD proof marker is missing: %s\n' "$crd_live_marker" >&2
		exit 1
	}
done

if grep -F 'expected one controller Deployment' \
	"$ROOT_DIR/hack/e2e-crd-upgrade.sh" >/dev/null; then
	printf '%s\n' 'e2e static: stopped-runtime upgrade proof still requires a live controller Deployment' >&2
	exit 1
fi
deployment_evidence_section=$(sed -n '/^deployment_evidence() {$/,/^}$/p' \
	"$ROOT_DIR/hack/e2e-crd-upgrade.sh")
for deployment_evidence_marker in \
	'get deployment -o json' \
	'labels: (.metadata.labels // {})' \
	'annotations: (.metadata.annotations // {})' \
	'ownerReferences: (.metadata.ownerReferences // [])' \
	'spec: .spec' \
	'sort_by(.name)'; do
	printf '%s\n' "$deployment_evidence_section" |
		grep -F -- "$deployment_evidence_marker" >/dev/null
done
grep -F 'mutated runtime Deployments' "$ROOT_DIR/hack/e2e-crd-upgrade.sh" >/dev/null

if grep -F '((.status.containerStatuses // []) | length) == 0' \
	"$ROOT_DIR/hack/e2e-crd-upgrade.sh" >/dev/null; then
	printf '%s\n' 'e2e static: CRD runtime guard treats transient empty main status as proof' >&2
	exit 1
fi
# shellcheck disable=SC2016 # Match literal runtime guard variables in the harness.
for crd_guard_marker in \
	'BLOCKED_STABILITY_SECONDS=10' \
	'BLOCKED_FAILURE_TIMEOUT_SECONDS=150' \
	'assert_explicit_runtime_guard "$description" all 3' \
	'assert_explicit_runtime_guard "future stored controller state" controller 2' \
	'.mainContainersNeverStarted' \
	'.explicitVerifierFailures' \
	'blocked_pod_uids=$current_pod_uids'; do
	grep -F -- "$crd_guard_marker" "$ROOT_DIR/hack/e2e-crd-upgrade.sh" >/dev/null
done
for crd_guard_filter_marker in \
	'.state.terminated.exitCode' \
	'.lastState.terminated.exitCode' \
	'.restartCount' \
	'.started' \
	'PodInitializing'; do
	grep -F -- "$crd_guard_filter_marker" "$ROOT_DIR/hack/e2e-crd-init-guard.jq" >/dev/null
done

jq -n '
  def labels($component): {
    "app.kubernetes.io/instance": "ptah-e2e",
    "app.kubernetes.io/component": $component
  };
  {items: [
    {metadata: {uid: "controller-a", labels: labels("controller")}, status: {}},
    {metadata: {uid: "controller-b", labels: labels("controller")}, status: {}},
    {metadata: {uid: "rotator", labels: labels("certificate-rotation")}, status: {}}
  ]}
' >"$CRD_GUARD_PENDING_FIXTURE"
jq '
  .items[].status = {
    containerStatuses: [{
      name: "main", ready: false, restartCount: 0, started: false,
      state: {waiting: {reason: "PodInitializing"}}, lastState: {}
    }],
    initContainerStatuses: [{
      name: "verify-candidate-runtime", ready: false, restartCount: 1,
      state: {waiting: {reason: "CrashLoopBackOff"}},
      lastState: {terminated: {exitCode: 1, reason: "Error"}}
    }]
  }
' "$CRD_GUARD_PENDING_FIXTURE" >"$CRD_GUARD_FAILED_FIXTURE"
jq '.items[0].status.containerStatuses[0] = {
      name: "main", ready: true, restartCount: 0, started: true,
      state: {running: {startedAt: "2026-01-01T00:00:00Z"}}, lastState: {}
    }' "$CRD_GUARD_FAILED_FIXTURE" >"$CRD_GUARD_RUNNING_FIXTURE"
jq '.items[0].status.containerStatuses[0] = {
      name: "main", ready: false, restartCount: 1, started: false,
      state: {waiting: {reason: "CrashLoopBackOff"}},
      lastState: {terminated: {exitCode: 1, reason: "Error"}}
    }' "$CRD_GUARD_FAILED_FIXTURE" >"$CRD_GUARD_TERMINATED_FIXTURE"

jq --arg release ptah-e2e --arg scope all --argjson expected 3 \
	-f "$ROOT_DIR/hack/e2e-crd-init-guard.jq" \
	"$CRD_GUARD_PENDING_FIXTURE" >"$CRD_GUARD_STATE"
[ "$(jq -r '.explicitVerifierFailures' "$CRD_GUARD_STATE")" = false ] || {
	printf '%s\n' 'e2e static: transient Pending Pods satisfy the CRD runtime guard' >&2
	exit 1
}
jq --arg release ptah-e2e --arg scope all --argjson expected 3 \
	-f "$ROOT_DIR/hack/e2e-crd-init-guard.jq" \
	"$CRD_GUARD_FAILED_FIXTURE" >"$CRD_GUARD_STATE"
jq -e '.podCount == 3 and .explicitVerifierFailures and .mainContainersNeverStarted' \
	"$CRD_GUARD_STATE" >/dev/null || {
	printf '%s\n' 'e2e static: explicit init failures do not satisfy the stable guard state' >&2
	exit 1
}
for started_fixture in "$CRD_GUARD_RUNNING_FIXTURE" "$CRD_GUARD_TERMINATED_FIXTURE"; do
	jq --arg release ptah-e2e --arg scope all --argjson expected 3 \
		-f "$ROOT_DIR/hack/e2e-crd-init-guard.jq" \
		"$started_fixture" >"$CRD_GUARD_STATE"
	[ "$(jq -r '.mainContainersNeverStarted' "$CRD_GUARD_STATE")" = false ] || {
		printf 'e2e static: runtime guard missed main-container start evidence in %s\n' \
			"$started_fixture" >&2
		exit 1
	}
done

# shellcheck disable=SC2016 # Match the literal runtime ROOT_DIR expression in the harness.
crd_script_invocation='"$ROOT_DIR/hack/e2e-crd-upgrade.sh"'
[ "$(grep -Fc "$crd_script_invocation" "$ROOT_DIR/hack/e2e-kind.sh")" -eq 2 ]

# The uninstall phase re-runs the upgrade proof inside itself, so it needs every
# variable the upgrade invocation passes. A name the uninstall invocation drops
# does not announce itself: the phase script refuses it through ${VAR:?...},
# which ends that shell without setting $?.
crd_invocation_environment() {
	# GNU awk drops an unescaped terminal backslash in a -v assignment. Build
	# the continuation inside the program so the exact line match is portable.
	awk -v phase="E2E_PHASE=$1" -v target='"$ROOT_DIR/hack/e2e-crd-upgrade.sh"' '
		index($0, "E2E_") == 1 {
			block = block $0 "\n"
			if ($0 == phase " \\") { matched = 1 }
			next
		}
		index($0, target) > 0 {
			if (matched) { printf "%s", block }
			block = ""
			matched = 0
			next
		}
		{ block = ""; matched = 0 }
	' "${2:-$ROOT_DIR/hack/e2e-kind.sh}" | sed 's/=.*//' | sort -u
}

# shellcheck disable=SC2016 # These are literal harness lines, not commands to execute.
crd_invocation_parser_probe=$(printf '%s\n' \
	"E2E_WRONG_PHASE=value \\" \
	"E2E_PHASE=uninstall \\" \
	'  "$ROOT_DIR/hack/e2e-crd-upgrade.sh"' \
	"E2E_WRONG_TARGET=value \\" \
	"E2E_PHASE=upgrade \\" \
	'  "$ROOT_DIR/hack/another-phase.sh"' \
	"E2E_REQUIRED=value \\" \
	"E2E_PHASE=upgrade \\" \
	'  "$ROOT_DIR/hack/e2e-crd-upgrade.sh"' |
	crd_invocation_environment upgrade -)
[ "$crd_invocation_parser_probe" = "$(printf '%s\n' E2E_PHASE E2E_REQUIRED)" ] || {
	printf '%s\n' 'e2e static: CRD environment parser mixed invocation blocks or lost an assignment' >&2
	exit 1
}
# shellcheck disable=SC2016 # A phase without a continuation is not part of the command environment.
crd_invocation_parser_probe=$(printf '%s\n' \
	"E2E_REQUIRED=value \\" \
	'E2E_PHASE=upgrade' \
	'  "$ROOT_DIR/hack/e2e-crd-upgrade.sh"' |
	crd_invocation_environment upgrade -)
[ -z "$crd_invocation_parser_probe" ] || {
	printf '%s\n' 'e2e static: CRD environment parser accepted a phase without a command continuation' >&2
	exit 1
}

crd_invocation_environment upgrade >"$CRD_UPGRADE_INVOCATION_ENV"
crd_invocation_environment uninstall >"$CRD_UNINSTALL_INVOCATION_ENV"
for crd_invocation_env_file in "$CRD_UPGRADE_INVOCATION_ENV" "$CRD_UNINSTALL_INVOCATION_ENV"; do
	[ -s "$crd_invocation_env_file" ] || {
		printf 'e2e static: could not read a CRD phase invocation environment from hack/e2e-kind.sh\n' >&2
		exit 1
	}
done
crd_invocation_env_missing=$(grep -Fxv -f "$CRD_UNINSTALL_INVOCATION_ENV" \
	"$CRD_UPGRADE_INVOCATION_ENV" || true)
[ -z "$crd_invocation_env_missing" ] || {
	printf 'e2e static: the uninstall CRD phase does not receive %s\n' \
		"$(printf '%s' "$crd_invocation_env_missing" | tr '\n' ' ')" >&2
	exit 1
}

# A shell that ends on a refused ${VAR:?...} or an unset name under set -u never
# sets $?, so an EXIT trap that reports $? reports the previous command's
# success. Every trap in the harness therefore latches its own completion.
assert_exit_traps_latched() {
	latch_script=$1
	sed -n 's/^[[:space:]]*trap \([a-z_][a-z_]*\) EXIT$/\1/p' \
		"$latch_script" >"$EXIT_LATCH_FUNCTIONS"
	while IFS= read -r latch_function; do
		awk -v fn="$latch_function" '
			{
				if (found && checked < 2) {
					checked++
					if (index($0, "[ \"$status\" -ne 0 ] || [ \"$") > 0 &&
						index($0, "_COMPLETED\" -eq 1 ] || status=1") > 0) {
						latched = 1
					}
				}
				line = $0
				sub(/^[ \t]+/, "", line)
				if (line == fn "() {") { found = 1; checked = 0 }
			}
			END { exit latched ? 0 : 1 }
		' "$latch_script" || {
			printf 'e2e static: EXIT trap %s in %s reports $? without a completion latch\n' \
				"$latch_function" "$latch_script" >&2
			exit 1
		}
	done <"$EXIT_LATCH_FUNCTIONS"
}
for latch_candidate in $(git -C "$ROOT_DIR" ls-files 'hack/*.sh'); do
	assert_exit_traps_latched "$ROOT_DIR/$latch_candidate"
done

# The latch is load-bearing only if the guarded shape really refuses, so run it.
# The shape is printed rather than written as a heredoc body: a heredoc would add
# a second literal fail-fast mode line to this file's own source contract.
# shellcheck disable=SC2016 # The probe expands when it runs, not while written.
printf '%s\n' \
	'#!/bin/sh' \
	'' \
	'set -eu' \
	'' \
	'PHASE_COMPLETED=0' \
	'cleanup() {' \
	'	status=$?' \
	'	[ "$status" -ne 0 ] || [ "$PHASE_COMPLETED" -eq 1 ] || status=1' \
	'	trap - EXIT HUP INT TERM' \
	'	exit "$status"' \
	'}' \
	'trap cleanup EXIT' \
	'true' \
	'EXIT_LATCH_PROBE=${EXIT_LATCH_PROBE:?probe value is required}' \
	'PHASE_COMPLETED=1' \
	'printf "%s\\n" "$EXIT_LATCH_PROBE"' \
	>"$EXIT_LATCH_PROBE_SCRIPT"
if sh "$EXIT_LATCH_PROBE_SCRIPT" >/dev/null 2>&1; then
	printf '%s\n' 'e2e static: the completion latch reported a refused expansion as a pass' >&2
	exit 1
fi
EXIT_LATCH_PROBE=probe-value sh "$EXIT_LATCH_PROBE_SCRIPT" >/dev/null || {
	printf '%s\n' 'e2e static: the completion latch refused a script that reached its end' >&2
	exit 1
}
# shellcheck disable=SC2016 # Match literal runtime provenance expressions in the harness.
controller_revision_assignment='CONTROLLER_REVISION=${E2E_CONTROLLER_REVISION:?E2E_CONTROLLER_REVISION is required inside the source snapshot}'
grep -F -- "$controller_revision_assignment" "$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
grep -F "operator source revision must be an exact 40-character lowercase Git commit" \
	"$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
# shellcheck disable=SC2016 # Match the literal variable passed to both Docker builds.
controller_revision_build_arg='--build-arg "REVISION=$CONTROLLER_REVISION"'
[ "$(grep -Fc -- "$controller_revision_build_arg" "$ROOT_DIR/hack/e2e-kind.sh")" -eq 3 ]
# shellcheck disable=SC2016 # Match the registry digest passed to the packaged candidate values.
grep -F -- '"$CANDIDATE_OPERATOR_DIGEST" "$MANAGER_PULL_SECRET"' \
	"$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
# shellcheck disable=SC2016 # Match the literal candidate push expression.
grep -F 'push_task_image "$OPERATOR_IMAGE" ptah-operator' \
	"$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
# shellcheck disable=SC2016 # Reject the literal test-only candidate load expression.
if grep -F 'kind load docker-image "$OPERATOR_IMAGE"' \
	"$ROOT_DIR/hack/e2e-kind.sh" >/dev/null; then
	printf '%s\n' 'e2e static: candidate manager can bypass the authenticated registry pull' >&2
	exit 1
fi
grep -F 'type: "kubernetes.io/dockerconfigjson"' \
	"$ROOT_DIR/hack/e2e-kind.sh" >/dev/null
# shellcheck disable=SC2016 # Match the literal pull-secret creation pipeline.
grep -F '| kubectl --kubeconfig "$KUBECONFIG_FILE" create -f - >/dev/null' \
	"$ROOT_DIR/hack/e2e-kind.sh" >/dev/null || {
	printf '%s\n' 'e2e static: registry pull Secret creation is missing' >&2
	exit 1
}
# Each phase that dispatches operations is handed the controller identity in
# full, spelled the same way: the data plane, the two migration paths -- one per
# engine -- and the uninstall proof. The count is exact so a phase that stopped
# receiving one of the three is a failure here rather than a Job the admission
# guards refuse in a cluster an hour later.
# shellcheck disable=SC2016 # Match literal runtime controller identity expressions.
for controller_identity_assignment in \
	'E2E_CONTROLLER_IMAGE=$CANDIDATE_OPERATOR_IMAGE' \
	'E2E_CONTROLLER_REVISION=$CONTROLLER_REVISION' \
	'E2E_CONTROLLER_STATE_VERSION=$CONTROLLER_STATE_VERSION'; do
	controller_identity_count=$(grep -Fc -- "$controller_identity_assignment" \
		"$ROOT_DIR/hack/e2e-kind.sh")
	[ "$controller_identity_count" -eq 4 ] || {
		printf 'e2e static: %s is handed to %s phases, and four dispatch operations\n' \
			"$controller_identity_assignment" "$controller_identity_count" >&2
		exit 1
	}
done

if make -s -C "$ROOT_DIR" docker-build REVISION=not-a-git-commit \
	>/dev/null 2>"$INVALID_BUILD_REVISION_ERROR"; then
	printf '%s\n' 'e2e static: docker-build accepted an invalid manager revision' >&2
	exit 1
fi
grep -F 'REVISION must be an exact 40-character lowercase Git commit' \
	"$INVALID_BUILD_REVISION_ERROR" >/dev/null
expected_build_revision=$(git -C "$ROOT_DIR" rev-parse --verify HEAD)
different_build_revision=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
[ "$different_build_revision" != "$expected_build_revision" ] || \
	different_build_revision=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
if make -s -C "$ROOT_DIR" docker-build REVISION="$different_build_revision" \
		>/dev/null 2>"$INVALID_BUILD_REVISION_ERROR"; then
	printf '%s\n' 'e2e static: docker-build accepted a valid but foreign manager revision' >&2
	exit 1
fi
grep -F "must equal current HEAD $expected_build_revision" \
	"$INVALID_BUILD_REVISION_ERROR" >/dev/null
docker_build_dry_run=$(make -s -n -C "$ROOT_DIR" docker-build)
printf '%s\n' "$docker_build_dry_run" |
	grep -F -- "--build-arg \"REVISION=$expected_build_revision\"" >/dev/null || {
	printf '%s\n' 'e2e static: docker-build does not inject the current source revision' >&2
	exit 1
}

PHASE_COMPLETED=1
printf '%s\n' 'e2e static: PASS'
