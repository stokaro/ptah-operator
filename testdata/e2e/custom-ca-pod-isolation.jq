def containers($pod):
  (($pod.spec.containers // []) + ($pod.spec.initContainers // []));
def named($pod; $name):
  [containers($pod)[] | select(.name == $name)];
def env_names($container):
  [$container.env[]?.name];
def secret_names($container):
  [$container.env[]? | .valueFrom.secretKeyRef.name? |
    select(type == "string")];
def has_literal_env($container; $name; $value):
  any($container.env[]?;
    .name == $name and .value == $value and .valueFrom == null);
def has_required_secret_env($container; $name; $secret; $key):
  any($container.env[]?;
    .name == $name and .value == null and
    .valueFrom.secretKeyRef.name == $secret and
    .valueFrom.secretKeyRef.key == $key and
    (.valueFrom.secretKeyRef.optional // false) == false);
def has_optional_secret_env($container; $name; $secret; $key):
  any($container.env[]?;
    .name == $name and .value == null and
    .valueFrom.secretKeyRef.name == $secret and
    .valueFrom.secretKeyRef.key == $key and
    .valueFrom.secretKeyRef.optional == true);
def exact_mounts($container; $expected):
  (($container.volumeMounts // []) |
    map({name: .name, mountPath: .mountPath, readOnly: (.readOnly // false)}) |
    sort_by(.name)) == ($expected | sort_by(.name)) and
  all($container.volumeMounts[]?;
    (.subPath // "") == "" and (.subPathExpr // "") == "" and
    .mountPropagation == null and (.recursiveReadOnly // "") == "");
def exact_memory_volume($pod; $name; $size):
  [.spec.volumes[]? | select(
    .name == $name and (keys | sort) == ["emptyDir", "name"] and
    .emptyDir.medium == "Memory" and .emptyDir.sizeLimit == $size)] |
  length == 1;
def hardened_container($container):
  $container.securityContext.allowPrivilegeEscalation == false and
  $container.securityContext.readOnlyRootFilesystem == true and
  $container.securityContext.runAsNonRoot == true and
  $container.securityContext.runAsUser == 65532 and
  $container.securityContext.runAsGroup == 65532 and
  ($container.securityContext.privileged // false) == false and
  ($container.securityContext.capabilities.add // []) == [] and
  $container.securityContext.capabilities.drop == ["ALL"] and
  $container.securityContext.seccompProfile == {type: "RuntimeDefault"};
def hardened_pod($pod):
  $pod.spec.automountServiceAccountToken == false and
  $pod.spec.enableServiceLinks == false and
  $pod.spec.securityContext.runAsNonRoot == true and
  $pod.spec.securityContext.runAsUser == 65532 and
  $pod.spec.securityContext.runAsGroup == 65532 and
  $pod.spec.securityContext.fsGroup == 65532 and
  $pod.spec.securityContext.fsGroupChangePolicy == "OnRootMismatch" and
  $pod.spec.securityContext.seccompProfile == {type: "RuntimeDefault"} and
  ($pod.spec.securityContext.supplementalGroups // []) == [] and
  ($pod.spec.securityContext.sysctls // []) == [] and
  ($pod.spec.ephemeralContainers // []) == [] and
  all(containers($pod)[];
    hardened_container(.) and (.envFrom // []) == []);
def has_no_registry_env($container):
  (env_names($container) |
    map(select(startswith("PTAH_OCI_") or startswith("PTAH_OPERATOR_OCI_") or
      . == "PTAH_PLAIN_HTTP" or . == "DOCKER_CONFIG")) |
    length) == 0;
def status_succeeded($pod; $name; $init):
  (if $init then
    [$pod.status.initContainerStatuses[]? | select(.name == $name)]
  else
    [$pod.status.containerStatuses[]? | select(.name == $name)]
  end) as $statuses |
  ($statuses | length) == 1 and
  $statuses[0].restartCount == 0 and
  $statuses[0].state.terminated.exitCode == 0;
def isolated($pod):
  named($pod; "ptah") as $main |
  named($pod; "install-runner") as $install |
  named($pod; "validate-source-authority") as $guard |
  named($pod; "fetch-schema") as $fetch |
  ($main | length) == 1 and ($install | length) == 1 and
  ($guard | length) == 1 and ($fetch | length) == 1 and
  .status.phase == "Succeeded" and
  ([.metadata.ownerReferences[]? |
    select(.apiVersion == "batch/v1" and .kind == "Job" and .controller == true)] |
    length) == 1 and
  [.spec.initContainers[]?.name] ==
    ["install-runner", "validate-source-authority", "fetch-schema"] and
  [.spec.containers[]?.name] == ["ptah"] and
  hardened_pod(.) and
  status_succeeded(.; "install-runner"; true) and
  status_succeeded(.; "validate-source-authority"; true) and
  status_succeeded(.; "fetch-schema"; true) and
  status_succeeded(.; "ptah"; false) and
  ([.spec.volumes[]?.name] | sort) ==
    ["fetch-work", "registry-ca", "registry-ca-snapshot", "runner", "schema-source", "work"] and
  exact_memory_volume(.; "runner"; "64Mi") and
  exact_memory_volume(.; "work"; "128Mi") and
  exact_memory_volume(.; "fetch-work"; "64Mi") and
  exact_memory_volume(.; "registry-ca-snapshot"; "2Mi") and
  exact_memory_volume(.; "schema-source"; "64Mi") and
  ([.spec.volumes[]? | select(
    .name == "registry-ca" and (keys | sort) == ["configMap", "name"] and
    .configMap.name == $caConfigMap and
    (.configMap.optional // false) == false and
    (.configMap.defaultMode // 420) == 420 and
	.configMap.items == [{key: "ca.pem", path: "ca.pem", mode: 288}])] |
    length) == 1 and
  ($install[0].command == ["/ptah-runner"] and
    $install[0].args == ["--install-to", "/runner/ptah-runner"] and
    ($install[0].env // []) == [] and
    (secret_names($install[0]) | length) == 0 and
    exact_mounts($install[0]; [
      {name: "runner", mountPath: "/runner", readOnly: false}
    ])) and
  ($guard[0].command == ["/runner/ptah-runner"] and
    $guard[0].args == [
      "--validate-oci-source", $resolvedReference,
      "--snapshot-oci-ca-to", "/credentials/ca-snapshot/ca.pem"
    ] and
    has_literal_env($guard[0]; "PTAH_OPERATOR_OCI_AUTH_MODE"; "Environment") and
    has_required_secret_env($guard[0]; "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT"; $registrySecret; "registry") and
    has_required_secret_env($guard[0]; "PTAH_OPERATOR_OCI_CA_SHA256_GRANT"; $registrySecret; "caSHA256") and
    has_literal_env($guard[0]; "PTAH_OPERATOR_OCI_HAS_CA"; "true") and
    has_literal_env($guard[0]; "PTAH_OPERATOR_OCI_CA_SOURCE_FILE"; "/credentials/ca-source/ca.pem") and
    has_literal_env($guard[0]; "PTAH_OCI_REGISTRY"; $registryAuthority) and
    has_literal_env($guard[0]; "PTAH_PLAIN_HTTP"; "false") and
    (env_names($guard[0]) | index("PTAH_OCI_USERNAME") | not) and
    (env_names($guard[0]) | index("PTAH_OCI_PASSWORD") | not) and
    (env_names($guard[0]) | index("PTAH_OCI_TOKEN") | not) and
    (secret_names($guard[0]) | sort) == [$registrySecret, $registrySecret] and
    exact_mounts($guard[0]; [
      {name: "runner", mountPath: "/runner", readOnly: true},
      {name: "registry-ca", mountPath: "/credentials/ca-source", readOnly: true},
      {name: "registry-ca-snapshot", mountPath: "/credentials/ca-snapshot", readOnly: false}
    ])) and
  ($fetch[0].command == ["/usr/local/bin/ptah"] and
    $fetch[0].args == ["schema", "pull", $resolvedReference, "--out", "/source/schema.hcl"] and
    has_optional_secret_env($fetch[0]; "PTAH_OCI_USERNAME"; $registrySecret; "username") and
    has_optional_secret_env($fetch[0]; "PTAH_OCI_PASSWORD"; $registrySecret; "password") and
    has_optional_secret_env($fetch[0]; "PTAH_OCI_TOKEN"; $registrySecret; "token") and
    has_literal_env($fetch[0]; "PTAH_OCI_REGISTRY"; $registryAuthority) and
    has_literal_env($fetch[0]; "PTAH_OCI_CA_FILE"; "/credentials/ca-snapshot/ca.pem") and
    has_literal_env($fetch[0]; "PTAH_PLAIN_HTTP"; "false") and
    (env_names($fetch[0]) | map(select(startswith("PTAH_OPERATOR_OCI_"))) | length) == 0 and
    (secret_names($fetch[0]) | sort) == [$registrySecret, $registrySecret, $registrySecret] and
    exact_mounts($fetch[0]; [
      {name: "fetch-work", mountPath: "/fetch-work", readOnly: false},
      {name: "registry-ca-snapshot", mountPath: "/credentials/ca-snapshot", readOnly: true},
      {name: "schema-source", mountPath: "/source", readOnly: false}
    ])) and
  (has_required_secret_env($main[0]; "PTAH_DB_URL"; $databaseSecret; "url") and
    has_literal_env($main[0]; "PTAH_EXPECTED_DATABASE_ENGINE"; "PostgreSQL") and
    has_no_registry_env($main[0]) and
    (secret_names($main[0]) | sort) == [$databaseSecret] and
    exact_mounts($main[0]; [
      {name: "runner", mountPath: "/runner", readOnly: true},
      {name: "schema-source", mountPath: "/source", readOnly: true},
      {name: "work", mountPath: "/work", readOnly: false}
    ]));

[.items[] |
  select(.metadata.labels["operator.ptah.dev/operation"] == "observe" or
    .metadata.labels["operator.ptah.dev/operation"] == "plan")] as $pods |
($pods | length) == 2 and all($pods[]; isolated(.))
