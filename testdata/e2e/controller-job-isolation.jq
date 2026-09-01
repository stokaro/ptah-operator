def operation($name):
  [.items[] | select(.metadata.labels["operator.ptah.dev/operation"] == $name)];
def containers($job):
  (($job.spec.template.spec.containers // []) +
    ($job.spec.template.spec.initContainers // []));
def named($job; $name):
  [containers($job)[] | select(.name == $name)];
def env_names($container):
  [$container.env[]?.name];
def secret_names($container):
  [$container.env[]?.valueFrom.secretKeyRef.name?];
def has_secret_env($container; $name; $key):
  any($container.env[]?;
    .name == $name and .value == null and
    .valueFrom.secretKeyRef.name == $registrySecret and
    .valueFrom.secretKeyRef.key == $key and
    (.valueFrom.secretKeyRef.optional // false) == false);
def has_database_ref($container):
  any($container.env[]?;
    .name == "PTAH_DB_URL" and .value == null and
    .valueFrom.secretKeyRef.name == $databaseSecret and .valueFrom.secretKeyRef.key == "url");
def has_expected_database_engine($container):
  any($container.env[]?;
    .name == "PTAH_EXPECTED_DATABASE_ENGINE" and .valueFrom == null and
    (.value == "PostgreSQL" or .value == "MySQL"));
def has_registry_credentials($container):
  ([["PTAH_OCI_USERNAME", "PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN"][] as $name |
    any($container.env[]?;
      .name == $name and .value == null and
      .valueFrom.secretKeyRef.name == $registrySecret)] | all) and
  any($container.env[]?;
    .name == "PTAH_OCI_REGISTRY" and .value != null and .valueFrom == null);
def has_literal_env($container; $name; $value):
  any($container.env[]?;
    .name == $name and .value == $value and .valueFrom == null);
def has_mount($container; $name; $readOnly):
  any($container.volumeMounts[]?;
    .name == $name and (.readOnly // false) == $readOnly);
def has_custom_ca_guard_inputs($container):
  has_secret_env($container; "PTAH_OPERATOR_OCI_CA_SHA256_GRANT"; "caSHA256") and
  has_literal_env($container; "PTAH_OPERATOR_OCI_HAS_CA"; "true") and
  has_literal_env($container; "PTAH_OPERATOR_OCI_CA_SOURCE_FILE"; "/credentials/ca-source/ca.pem");
def has_authority_guard_inputs($container):
  any($container.env[]?;
    .name == "PTAH_OPERATOR_OCI_AUTH_MODE" and .value == "Environment" and .valueFrom == null) and
  has_secret_env($container; "PTAH_OPERATOR_OCI_AUTH_REGISTRY_GRANT"; "registry") and
  any($container.env[]?;
    .name == "PTAH_OCI_REGISTRY" and .value != null and .valueFrom == null) and
  ((has_secret_env($container; "PTAH_OPERATOR_OCI_ALLOW_PLAIN_HTTP"; "allowPlainHTTP") and
    has_literal_env($container; "PTAH_PLAIN_HTTP"; "true")) or
   (has_custom_ca_guard_inputs($container) and
    has_literal_env($container; "PTAH_PLAIN_HTTP"; "false")));
def no_authority_guard_inputs($container):
  (env_names($container) | map(select(startswith("PTAH_OPERATOR_OCI_"))) | length) == 0;
def no_database($container):
  (env_names($container) | index("PTAH_DB_URL") | not) and
  (env_names($container) | index("PTAH_DEV_URL") | not) and
  (secret_names($container) | index($databaseSecret) | not);
def no_registry($container):
  (env_names($container) |
    map(select(startswith("PTAH_OCI_") or startswith("PTAH_OPERATOR_OCI_") or
      . == "DOCKER_CONFIG" or . == "PTAH_PLAIN_HTTP")) |
    length) == 0 and
  (secret_names($container) | index($registrySecret) | not);
def neutral($container):
  no_database($container) and no_registry($container);
def safe_job_contract($job):
  $job.spec.backoffLimit == 0 and
  $job.spec.podReplacementPolicy == "Failed" and
  $job.spec.template.spec.restartPolicy == "Never";
def only_neutral_except($job; $allowed):
  all(containers($job)[];
    (.name as $name | ($allowed | index($name)) != null) or neutral(.));
def source_ca_snapshot_boundary($main):
  if has_custom_ca_guard_inputs($main) then
    ((env_names($main) | index("PTAH_OCI_CA_FILE") | not) and
     has_mount($main; "registry-ca"; true) and
     (has_mount($main; "registry-ca-snapshot"; true) | not) and
     (has_mount($main; "registry-ca-snapshot"; false) | not))
  else true
  end;
def fetch_ca_snapshot_boundary($guard; $fetch):
  if has_custom_ca_guard_inputs($guard) then
    (has_mount($guard; "registry-ca"; true) and
     has_mount($guard; "registry-ca-snapshot"; false) and
     (has_mount($guard; "registry-ca-snapshot"; true) | not) and
     has_literal_env($fetch; "PTAH_OCI_CA_FILE"; "/credentials/ca-snapshot/ca.pem") and
     has_mount($fetch; "registry-ca-snapshot"; true) and
     (has_mount($fetch; "registry-ca"; true) | not) and
     (has_mount($fetch; "registry-ca"; false) | not))
  else
    ((($guard.volumeMounts // []) | length) == 1 and
     $guard.volumeMounts[0].name == "runner" and
     (env_names($fetch) | index("PTAH_OCI_CA_FILE") | not))
  end;
def source_job_isolated($job):
  named($job; "ptah") as $main |
  named($job; "fetch-schema") as $fetch |
  named($job; "validate-source-authority") as $guard |
  ($main | length) == 1 and ($fetch | length) == 0 and ($guard | length) == 0 and
  (no_database($main[0]) and has_registry_credentials($main[0]) and
   has_authority_guard_inputs($main[0]) and source_ca_snapshot_boundary($main[0])) and
  only_neutral_except($job; ["ptah"]);
def database_source_job_isolated($job):
  named($job; "ptah") as $main |
  named($job; "fetch-schema") as $fetch |
  named($job; "validate-source-authority") as $guard |
  ($main | length) == 1 and ($fetch | length) == 1 and ($guard | length) == 1 and
  [$job.spec.template.spec.initContainers[]?.name] ==
    ["install-runner", "validate-source-authority", "fetch-schema"] and
  (has_database_ref($main[0]) and no_registry($main[0])) and
  (no_database($fetch[0]) and has_registry_credentials($fetch[0]) and no_authority_guard_inputs($fetch[0])) and
  (no_database($guard[0]) and has_authority_guard_inputs($guard[0]) and
    ((env_names($guard[0]) | index("PTAH_OCI_USERNAME") | not)) and
    ((env_names($guard[0]) | index("PTAH_OCI_PASSWORD") | not)) and
    ((env_names($guard[0]) | index("PTAH_OCI_TOKEN") | not)) and
    fetch_ca_snapshot_boundary($guard[0]; $fetch[0])) and
  only_neutral_except($job; ["ptah", "validate-source-authority", "fetch-schema"]);
def apply_job_isolated($job):
  named($job; "ptah") as $main |
  named($job; "fetch-schema") as $fetch |
  ($main | length) == 1 and ($fetch | length) == 0 and
  (has_database_ref($main[0]) and no_registry($main[0])) and
  only_neutral_except($job; ["ptah"]);

operation("resolve") as $resolve |
operation("verify") as $verify |
operation("observe") as $observe |
operation("plan") as $plan |
operation("apply") as $apply |
($resolve | length) > 0 and all($resolve[]; safe_job_contract(.) and source_job_isolated(.)) and
($verify | length) > 0 and all($verify[]; safe_job_contract(.) and source_job_isolated(.)) and
($observe | length) > 0 and all($observe[];
  safe_job_contract(.) and database_source_job_isolated(.) and
  (named(.; "ptah") | length == 1 and has_expected_database_engine(.[0]))) and
($plan | length) > 0 and all($plan[];
  safe_job_contract(.) and database_source_job_isolated(.) and
  (named(.; "ptah") | length == 1 and has_expected_database_engine(.[0]))) and
((($requireApply | not)) or
  (($apply | length) > 0 and all($apply[]; safe_job_contract(.) and apply_job_isolated(.))))
