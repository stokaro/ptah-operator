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
def has_database_ref($container):
  any($container.env[]?;
    .name == "PTAH_DB_URL" and .value == null and
    .valueFrom.secretKeyRef.name == $databaseSecret and .valueFrom.secretKeyRef.key == "url");
def has_expected_database_engine($container):
  any($container.env[]?;
    .name == "PTAH_EXPECTED_DATABASE_ENGINE" and .valueFrom == null and
    (.value == "PostgreSQL" or .value == "MySQL"));
def has_registry_refs($container):
  ([["PTAH_OCI_USERNAME", "PTAH_OCI_PASSWORD", "PTAH_OCI_TOKEN", "PTAH_OCI_REGISTRY"][] as $name |
    any($container.env[]?;
      .name == $name and .value == null and
      .valueFrom.secretKeyRef.name == $registrySecret)] | all);
def no_database($container):
  (env_names($container) | index("PTAH_DB_URL") | not) and
  (env_names($container) | index("PTAH_DEV_URL") | not) and
  (secret_names($container) | index($databaseSecret) | not);
def no_registry($container):
  (env_names($container) |
    map(select(startswith("PTAH_OCI_") or . == "DOCKER_CONFIG" or . == "PTAH_PLAIN_HTTP")) |
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
def source_job_isolated($job):
  named($job; "ptah") as $main |
  named($job; "fetch-schema") as $fetch |
  ($main | length) == 1 and ($fetch | length) == 0 and
  (no_database($main[0]) and has_registry_refs($main[0])) and
  only_neutral_except($job; ["ptah"]);
def database_source_job_isolated($job):
  named($job; "ptah") as $main |
  named($job; "fetch-schema") as $fetch |
  ($main | length) == 1 and ($fetch | length) == 1 and
  (has_database_ref($main[0]) and no_registry($main[0])) and
  (no_database($fetch[0]) and has_registry_refs($fetch[0])) and
  only_neutral_except($job; ["ptah", "fetch-schema"]);
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
