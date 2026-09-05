def containers($job):
  (($job.spec.template.spec.containers // []) +
    ($job.spec.template.spec.initContainers // []) +
    ($job.spec.template.spec.ephemeralContainers // []));
def env_names($container):
  [$container.env[]?.name];
def secret_env_names($container):
  [$container.env[]? |
    select(.valueFrom.secretKeyRef.name? != null) |
    .name];
def exact_literal_env($container; $name; $value):
  [$container.env[]? | select(.name == $name)] as $matches |
  ($matches | length) == 1 and
  $matches[0].value == $value and $matches[0].valueFrom == null;
def exact_secret_env($container; $name; $key; $optional):
  [$container.env[]? | select(.name == $name)] as $matches |
  ($matches | length) == 1 and
  $matches[0].value == null and
  ($matches[0].valueFrom | keys) == ["secretKeyRef"] and
  $matches[0].valueFrom.secretKeyRef.name == $registrySecret and
  $matches[0].valueFrom.secretKeyRef.key == $key and
  (if $optional then
    ($matches[0].valueFrom.secretKeyRef | keys | sort) ==
      ["key", "name", "optional"] and
    $matches[0].valueFrom.secretKeyRef.optional == true
  else
    ($matches[0].valueFrom.secretKeyRef | keys | sort) ==
      ["key", "name"] and
    $matches[0].valueFrom.secretKeyRef.optional == null
  end);
def exact_source_env_names($operation):
  ([
    "HOME",
    "PTAH_OCI_REGISTRY",
    "PTAH_OPERATION_ID",
    "PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP",
    "PTAH_OPERATOR_OCI_AUTH_MODE",
    "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT",
    "PTAH_PLAIN_HTTP",
    "PTAH_REQUESTED_REFERENCE",
    "TMPDIR"
  ] +
  (if $authMode == "Environment" then
    ["PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN", "PTAH_OCI_USERNAME"]
  elif $authMode == "DockerConfigJSON" then
    ["DOCKER_CONFIG"]
  else
    []
  end) +
  (if $operation == "verify" then
    [
      "PTAH_EXPECTED_ARTIFACT_TYPE",
      "PTAH_RESOLVED_REFERENCE",
      "PTAH_VERIFICATION_POLICY"
    ]
  else
    []
  end)) | sort;
def no_database($container):
  ([env_names($container)[] |
    select(. == "PTAH_DB_URL" or . == "PTAH_DEV_URL")] | length) == 0 and
  ([$container.env[]?.valueFrom.secretKeyRef.name? |
    select(. == $databaseSecret)] | length) == 0;
def no_env_from($container):
  ($container.envFrom // []) == [];
def no_source_access($container):
  ([env_names($container)[] | select(
    startswith("PTAH_OCI_") or startswith("PTAH_OPERATOR_OCI_") or
    . == "PTAH_PLAIN_HTTP" or . == "DOCKER_CONFIG")] | length) == 0 and
  ([$container.volumeMounts[]? |
    select(.name == "registry-docker-config")] | length) == 0 and
  ([$container.env[]?.valueFrom.secretKeyRef.name? |
    select(. == $registrySecret)] | length) == 0;
def fixed_grants($container):
  exact_literal_env($container; "PTAH_OPERATOR_OCI_AUTH_MODE"; $authMode) and
  exact_secret_env($container; "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT"; "registry"; false) and
  exact_secret_env($container; "PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP"; "allowPlainHTTP"; false) and
  exact_literal_env($container; "PTAH_OCI_REGISTRY"; $registryAuthority) and
  exact_literal_env($container; "PTAH_PLAIN_HTTP"; "true") and
  ([env_names($container)[] |
    select(startswith("PTAH_OPERATOR_OCI_"))] | sort) == [
      "PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP",
      "PTAH_OPERATOR_OCI_AUTH_MODE",
      "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT"
    ];
def no_secret_volumes($job):
  all(($job.spec.template.spec.volumes // [])[];
    .secret == null and
    all((.projected.sources // [])[]?; .secret == null));
def environment_credentials($job; $container):
  exact_secret_env($container; "PTAH_OCI_USERNAME"; "username"; true) and
  exact_secret_env($container; "PTAH_OCI_PASSWORD"; "password"; true) and
  exact_secret_env($container; "PTAH_OCI_TOKEN"; "token"; true) and
  (secret_env_names($container) | sort) == [
    "PTAH_OCI_PASSWORD",
    "PTAH_OCI_TOKEN",
    "PTAH_OCI_USERNAME",
    "PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP",
    "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT"
  ] and
  (env_names($container) | index("DOCKER_CONFIG") | not) and
  ([$job.spec.template.spec.volumes[]? |
    select(.name == "registry-docker-config")] | length) == 0 and
  ([$container.volumeMounts[]? |
    select(.name == "registry-docker-config")] | length) == 0 and
  no_secret_volumes($job);
def docker_config_credentials($job; $container):
  ([env_names($container)[] | select(
    . == "PTAH_OCI_USERNAME" or . == "PTAH_OCI_PASSWORD" or
    . == "PTAH_OCI_TOKEN")] | length) == 0 and
  (secret_env_names($container) | sort) == [
    "PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP",
    "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT"
  ] and
  exact_literal_env($container; "DOCKER_CONFIG"; "/credentials/docker") and
  ([$container.volumeMounts[]? |
    select(.name == "registry-docker-config")] | length) == 1 and
  ([$container.volumeMounts[]? |
    select(.name == "registry-docker-config")][0] | . == {
      name: "registry-docker-config",
      mountPath: "/credentials/docker",
      readOnly: true
    }) and
  ([($job.spec.template.spec.volumes // [])[] |
    select(.name == "registry-docker-config")] | length) == 1 and
  ([($job.spec.template.spec.volumes // [])[] |
    select(.name == "registry-docker-config")][0] | . == {
      name: "registry-docker-config",
      secret: {
        secretName: $registrySecret,
        items: [{key: ".dockerconfigjson", path: "config.json", mode: 288}],
        defaultMode: 420
      }
    }) and
  all(($job.spec.template.spec.volumes // [])[];
    (.secret == null or .name == "registry-docker-config") and
    all((.projected.sources // [])[]?; .secret == null));
def expected_source_volumes($operation):
  ([
    {
      name: "runner",
      emptyDir: {medium: "Memory", sizeLimit: "64Mi"}
    },
    {
      name: "work",
      emptyDir: {medium: "Memory", sizeLimit: "128Mi"}
    }
  ] +
  (if $authMode == "DockerConfigJSON" then [{
    name: "registry-docker-config",
    secret: {
      secretName: $registrySecret,
      items: [{key: ".dockerconfigjson", path: "config.json", mode: 288}],
      defaultMode: 420
    }
  }] else [] end) +
  (if $operation == "verify" then [{
    name: "verification-policy",
    configMap: {
      name: $verificationPolicy,
      items: [{key: "policy.yaml", path: "policy.yaml", mode: 288}],
      defaultMode: 420
    }
  }] else [] end));
def expected_main_mounts($operation):
  ([
    {name: "runner", mountPath: "/runner", readOnly: true},
    {name: "work", mountPath: "/work"}
  ] +
  (if $authMode == "DockerConfigJSON" then [{
    name: "registry-docker-config",
    mountPath: "/credentials/docker",
    readOnly: true
  }] else [] end) +
  (if $operation == "verify" then [{
    name: "verification-policy",
    mountPath: "/verification",
    readOnly: true
  }] else [] end));
def hardened_container($container):
  $container.securityContext == {
    capabilities: {drop: ["ALL"]},
    runAsUser: 65532,
    runAsGroup: 65532,
    runAsNonRoot: true,
    readOnlyRootFilesystem: true,
    allowPrivilegeEscalation: false,
    seccompProfile: {type: "RuntimeDefault"}
  };
def hardened_pod($pod):
  $pod.securityContext == {
    runAsUser: 65532,
    runAsGroup: 65532,
    runAsNonRoot: true,
    fsGroup: 65532,
    fsGroupChangePolicy: "OnRootMismatch",
    seccompProfile: {type: "RuntimeDefault"}
  };
def bounded_container($container):
  $container.terminationMessagePath == "/dev/termination-log" and
  $container.terminationMessagePolicy == "File" and
  $container.lifecycle == null and
  $container.livenessProbe == null and
  $container.readinessProbe == null and
  $container.startupProbe == null and
  ($container.ports // []) == [] and
  ($container.volumeDevices // []) == [] and
  ($container.stdin // false) == false and
  ($container.stdinOnce // false) == false and
  ($container.tty // false) == false and
  $container.restartPolicy == null;
def exact_source_literals($job; $container; $operation):
  ($job.metadata.annotations["operator.ptah.dev/operation-id"] | type) ==
    "string" and
  ($job.metadata.annotations["operator.ptah.dev/operation-id"] |
    test("^sha256:[0-9a-f]{64}$")) and
  exact_literal_env($container; "HOME"; "/work") and
  exact_literal_env($container; "TMPDIR"; "/work") and
  exact_literal_env($container; "PTAH_OPERATION_ID";
    $job.metadata.annotations["operator.ptah.dev/operation-id"]) and
  exact_literal_env($container; "PTAH_REQUESTED_REFERENCE";
    $requestedReference) and
  (if $operation == "verify" then
    exact_literal_env($container; "PTAH_RESOLVED_REFERENCE";
      $resolvedReference) and
    exact_literal_env($container; "PTAH_VERIFICATION_POLICY";
      "/verification/policy.yaml") and
    exact_literal_env($container; "PTAH_EXPECTED_ARTIFACT_TYPE";
      "application/vnd.stokaro.ptah.schema.v1")
  else
    true
  end);
def safe_job_contract($job):
  $job.spec.backoffLimit == 0 and
  $job.spec.podReplacementPolicy == "Failed" and
  $job.spec.template.spec.restartPolicy == "Never" and
  $job.spec.template.spec.automountServiceAccountToken == false and
  $job.spec.template.spec.enableServiceLinks == false and
  $job.spec.template.spec.dnsPolicy == "ClusterFirst" and
  $job.spec.template.spec.dnsConfig == null and
  ($job.spec.template.spec.hostAliases // []) == [] and
  ($job.spec.template.spec.hostNetwork // false) == false and
  ($job.spec.template.spec.hostPID // false) == false and
  ($job.spec.template.spec.hostIPC // false) == false and
  ($job.spec.template.spec.shareProcessNamespace // false) == false and
  ($job.spec.template.spec.serviceAccountName // "") == $serviceAccountName and
  ($job.spec.template.spec.imagePullSecrets // []) == $imagePullSecrets and
  hardened_pod($job.spec.template.spec);
def source_job_isolated($job):
  $job.metadata.labels["operator.ptah.dev/operation"] as $operation |
  ($job.spec.template.spec.containers // []) as $main |
  ($job.spec.template.spec.initContainers // []) as $init |
  ($job.spec.template.spec.ephemeralContainers // []) as $ephemeral |
  [$main[].name] == ["ptah"] and
  [$init[].name] == ["install-runner"] and
  $ephemeral == [] and
  (env_names($main[0]) | sort) ==
    exact_source_env_names($operation) and
  ($init[0].env // []) == [] and
  $init[0].image == $runnerImage and
  $init[0].imagePullPolicy == "IfNotPresent" and
  $init[0].command == ["/ptah-runner"] and
  $init[0].args == ["--install-to", "/runner/ptah-runner"] and
  ($init[0].workingDir // "") == "" and
  bounded_container($init[0]) and
  hardened_container($init[0]) and
  ($init[0].volumeMounts // []) == [
    {name: "runner", mountPath: "/runner"}
  ] and
  $main[0].image == $executorImage and
  $main[0].imagePullPolicy == "IfNotPresent" and
  $main[0].command == ["/runner/ptah-runner"] and
  $main[0].args == [
    "--ptah-binary", "/usr/local/bin/ptah",
    "--max-result-bytes", "8388608",
    "--max-plan-bytes", "8388608",
    "--operation", $operation
  ] and
  $main[0].workingDir == "/work" and
  bounded_container($main[0]) and
  hardened_container($main[0]) and
  ($main[0].volumeMounts // []) == expected_main_mounts($operation) and
  ($job.spec.template.spec.volumes // []) == expected_source_volumes($operation) and
  exact_source_literals($job; $main[0]; $operation) and
  fixed_grants($main[0]) and
  (if $authMode == "Environment" then
    environment_credentials($job; $main[0])
  elif $authMode == "DockerConfigJSON" then
    docker_config_credentials($job; $main[0])
  else
    false
  end) and
  all(containers($job)[]; no_database(.) and no_env_from(.)) and
  all($init[]; no_source_access(.));

.items as $jobs |
([$jobs[].metadata.labels["operator.ptah.dev/operation"]] | sort) ==
  ["resolve", "verify"] and
all($jobs[]; safe_job_contract(.) and source_job_isolated(.))
