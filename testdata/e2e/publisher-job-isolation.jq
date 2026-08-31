.spec.template.spec.containers[0] as $container |
$container.name == "publisher" and $container.image == $image and
(.spec.template.spec.containers | length) == 1 and
((.spec.template.spec.initContainers // []) | length) == 0 and
([$container.env[]?.name] | index("PTAH_DB_URL") | not) and
([$container.env[]?.name] | index("PTAH_DEV_URL") | not) and
([$container.env[]? | .valueFrom.secretKeyRef.name? // empty] |
  all(. == $registrySecret)) and
([["PTAH_OCI_USERNAME", "PTAH_OCI_PASSWORD", "PTAH_OCI_REGISTRY"][] as $name |
  any($container.env[]?;
    .name == $name and .value == null and
    .valueFrom.secretKeyRef.name == $registrySecret)] | all)
