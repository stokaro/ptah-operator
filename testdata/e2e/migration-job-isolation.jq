# The credential boundary a migration Job has to keep, read from the Jobs the
# controller actually created.
#
# Resolve and Verify reach the registry from the container that runs Ptah, and
# that container is given no database URL. History and Apply hold the database
# URL, and the artifact reaches them as files a separate init container pair
# wrote: the process that runs SQL never holds a registry credential, never
# holds the registry CA, and is pointed at a local directory rather than at a
# reference it could fetch itself.
#
# Inputs: $databaseSecret, $registrySecret, $executorImage, $runnerImage,
# $serviceAccountName.

def containers($job):
  (($job.spec.template.spec.containers // []) +
    ($job.spec.template.spec.initContainers // []) +
    ($job.spec.template.spec.ephemeralContainers // []));
def env_names($container):
  [$container.env[]?.name];
def secret_names($container):
  [$container.env[]?.valueFrom.secretKeyRef.name? // empty];
def exact_literal_env($container; $name; $value):
  [$container.env[]? | select(.name == $name)] as $matches |
  ($matches | length) == 1 and
  $matches[0].value == $value and $matches[0].valueFrom == null;
def no_database($container):
  ([env_names($container)[] |
    select(. == "PTAH_DB_URL" or . == "PTAH_DEV_URL")] | length) == 0 and
  ([secret_names($container)[] | select(. == $databaseSecret)] | length) == 0;
def no_registry($container):
  ([env_names($container)[] | select(
    startswith("PTAH_OCI_") or startswith("PTAH_OPERATOR_OCI_") or
    . == "PTAH_PLAIN_HTTP" or . == "DOCKER_CONFIG")] | length) == 0 and
  ([secret_names($container)[] | select(. == $registrySecret)] | length) == 0 and
  ([$container.volumeMounts[]? | select(
    .name == "registry-docker-config" or .name == "registry-ca" or
    .name == "registry-ca-snapshot")] | length) == 0;
def reaches_registry($container):
  ([secret_names($container)[] | select(. == $registrySecret)] | length) > 0 and
  exact_literal_env($container; "PTAH_PLAIN_HTTP"; "true");
def holds_database($container):
  [$container.env[]? | select(.name == "PTAH_DB_URL")] as $matches |
  ($matches | length) == 1 and
  $matches[0].value == null and
  ($matches[0].valueFrom | keys) == ["secretKeyRef"] and
  $matches[0].valueFrom.secretKeyRef.name == $databaseSecret and
  $matches[0].valueFrom.secretKeyRef.key == "url";
# The directory is a path the fetch container wrote, never a reference: Ptah
# would fetch a reference itself, from the process holding the database URL.
def local_migrations_dir($container):
  [$container.env[]? | select(.name == "PTAH_MIGRATIONS_DIR")] as $matches |
  ($matches | length) == 1 and
  $matches[0].valueFrom == null and
  ($matches[0].value | startswith("/")) and
  ($matches[0].value | contains("://") | not);
def no_env_from($container):
  ($container.envFrom // []) == [];
def safe_job_contract($job):
  $job.spec.backoffLimit == 0 and
  $job.spec.podReplacementPolicy == "Failed" and
  $job.spec.template.spec.restartPolicy == "Never" and
  $job.spec.template.spec.automountServiceAccountToken == false and
  $job.spec.template.spec.enableServiceLinks == false and
  ($job.spec.template.spec.hostNetwork // false) == false and
  ($job.spec.template.spec.hostPID // false) == false and
  ($job.spec.template.spec.hostIPC // false) == false and
  ($job.spec.template.spec.shareProcessNamespace // false) == false and
  ($job.spec.template.spec.serviceAccountName // "") == $serviceAccountName and
  $job.spec.template.spec.securityContext == {
    runAsUser: 65532,
    runAsGroup: 65532,
    runAsNonRoot: true,
    fsGroup: 65532,
    fsGroupChangePolicy: "OnRootMismatch",
    seccompProfile: {type: "RuntimeDefault"}
  } and
  ($job.spec.template.spec.ephemeralContainers // []) == [];
def source_operation($job):
  ($job.spec.template.spec.containers // []) as $main |
  ($job.spec.template.spec.initContainers // []) as $init |
  [$main[].name] == ["ptah"] and
  [$init[].name] == ["install-runner"] and
  $main[0].image == $executorImage and
  $init[0].image == $runnerImage and
  reaches_registry($main[0]) and
  all(containers($job)[]; no_database(.));
def database_operation($job):
  ($job.spec.template.spec.containers // []) as $main |
  ($job.spec.template.spec.initContainers // []) as $init |
  [$main[].name] ==  ["ptah"] and
  [$init[].name] ==
    ["install-runner", "validate-source-authority", "fetch-migrations"] and
  $main[0].image == $executorImage and
  $init[0].image == $runnerImage and
  $init[2].image == $executorImage and
  $init[2].command == ["/usr/local/bin/ptah"] and
  ($init[2].args[0:2]) == ["migrations", "pull"] and
  holds_database($main[0]) and
  local_migrations_dir($main[0]) and
  no_registry($main[0]) and
  reaches_registry($init[2]) and
  no_database($init[1]) and
  no_database($init[2]) and
  # The artifact reaches the SQL-running container read-only, from the volume
  # the fetch container filled.
  ([$main[0].volumeMounts[]? |
    select(.name == "schema-source")] | length) == 1 and
  ([$main[0].volumeMounts[]? |
    select(.name == "schema-source")][0].readOnly) == true;

.items as $jobs |
($jobs | length) > 0 and
([$jobs[].metadata.labels["operator.ptah.run/operation"]] | unique | sort) ==
  ["apply", "history", "resolve", "verify"] and
all($jobs[];
  safe_job_contract(.) and
  all(containers(.)[]; no_env_from(.)) and
  (if (.metadata.labels["operator.ptah.run/operation"] == "resolve" or
       .metadata.labels["operator.ptah.run/operation"] == "verify")
   then source_operation(.)
   else database_operation(.)
   end))
