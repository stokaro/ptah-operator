The JSON beside this file contains the real Kubernetes 1.37.0 envtest
responses to a JSON Patch dry run on the installed chart's status policy.
The ordinary user had status PATCH authorization. The manager's identity
submitted the identical request and was admitted. Both PtahSchema and
PtahMigration responses retain the resource name, policy, binding and cause.

The policy leaves `reason` unset, so the API server returns `Invalid` and
HTTP 422. Its message still says `is forbidden`. Requiring `IsForbidden`
rejected these actual policy denials and stopped the unsupported-state
acceptance scenario before its injection.

`TestStatusVersionInjectionRequiresManager` repeats the native request with
the pinned envtest assets. `TestStoredStateStatusRefusalUsesActualPolicyResponses`
reads these responses and refuses unrelated authorization, schema, policy,
binding and resource errors. This evidence measures admission only. It does
not qualify the runtime state guard or a future controller-state contract.
