. as $document |

def require_exact_properties($schema_name; $expected):
  ($document.components.schemas[$schema_name] //
    error("OpenAPI schema is missing: " + $schema_name)) as $schema |
  ($schema.properties //
    error("OpenAPI schema has no properties: " + $schema_name)) as $properties |
  ($properties | keys) as $actual |
  if $actual == $expected then true
  else error("OpenAPI properties changed for " + $schema_name +
    ": actual=" + ($actual | tojson) + ", expected=" + ($expected | tojson))
  end;

require_exact_properties(
  "io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta";
  [
    "annotations",
    "creationTimestamp",
    "deletionGracePeriodSeconds",
    "deletionTimestamp",
    "finalizers",
    "generateName",
    "generation",
    "labels",
    "managedFields",
    "name",
    "namespace",
    "ownerReferences",
    "resourceVersion",
    "selfLink",
    "uid"
  ]
) and
require_exact_properties(
  "io.k8s.api.admissionregistration.v1.WebhookClientConfig";
  ["caBundle", "service", "url"]
) and
require_exact_properties(
  "io.k8s.api.admissionregistration.v1.MutatingWebhook";
  [
    "admissionReviewVersions",
    "clientConfig",
    "failurePolicy",
    "matchConditions",
    "matchPolicy",
    "name",
    "namespaceSelector",
    "objectSelector",
    "reinvocationPolicy",
    "rules",
    "sideEffects",
    "timeoutSeconds"
  ]
) and
require_exact_properties(
  "io.k8s.api.admissionregistration.v1.ValidatingWebhook";
  [
    "admissionReviewVersions",
    "clientConfig",
    "failurePolicy",
    "matchConditions",
    "matchPolicy",
    "name",
    "namespaceSelector",
    "objectSelector",
    "rules",
    "sideEffects",
    "timeoutSeconds"
  ]
) and
require_exact_properties(
  "io.k8s.api.admissionregistration.v1.MutatingWebhookConfiguration";
  ["apiVersion", "kind", "metadata", "webhooks"]
) and
require_exact_properties(
  "io.k8s.api.admissionregistration.v1.ValidatingWebhookConfiguration";
  ["apiVersion", "kind", "metadata", "webhooks"]
)
