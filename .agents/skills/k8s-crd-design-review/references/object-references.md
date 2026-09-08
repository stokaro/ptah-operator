# Object references & relationships (reference)

This reference summarizes Kubernetes API conventions for fields that point at other objects.

## Scope: Configuration References vs. Lifecycle References

This document focuses on **configuration references** — fields where users express relationships for operator configuration (e.g., `secretRef`, `configMapRef`, custom cross-resource dependencies).

**Key distinction**:

- **Configuration References** (primary focus): User-authored references in `spec` that express what other objects an operator should use or coordinate with. These are part of the CRD spec and allow users to define relationships for operator behavior.
  - Examples: `connectionSecretRef`, `serviceAccountRef`, `targetRef`, custom dependency coordination
  - These are typically **same-namespace** and express "what should I use?" or "what should I coordinate with?"

- **Lifecycle References**: System-managed relationships set by controllers, typically not authored directly by users in spec.
  - `ownerReferences`: Parent-child relationships where the operator creates and manages child resources
  - These express "I created this and am responsible for it"
  - See the section [Lifecycle References](#lifecycle-references) for details

## Source

Derived from the upstream Kubernetes API conventions (see "Object references" section):
https://github.com/kubernetes/community/blob/main/contributors/devel/sig-architecture/api-conventions.md

## Configuration References: Key conventions (review checklist)

This section covers the conventions for fields where users reference other objects in their CRD configuration.

### Naming

- If a field refers to another object via a structured reference, use `fooRef` (one-to-one relationship)
- For lists of references, use `fooRefs` (one-to-many relationship)

The suffix `Ref`/`Refs` is intentionally consistent; avoid ad-hoc names like `fooReference`, `fooObject`, `fooTarget`, etc., unless the purpose demands it (e.g. `targetRef`).

Use the field name to communicate the relationship's role:

- `targetRef`: the referenced object is the primary subject being applied to, acted on, or reconciled against.
- `{kind}Ref`: the referenced object is a helper or dependency of a known kind, such as `secretRef`, `configMapRef`, or `serviceAccountRef`.
- `sourceRef` / `sinkRef`: the API models directional data flow.
- `selector`: the API refers to a set of objects by labels rather than one object by identity.

### Reference shape: separate syntax from resolution

`fooRef` means that the field is a reference object. It does **not** mean that every instance must
repeat a Group/Kind/Version. A fixed-kind reference with only a `name` is still structured:

```yaml
gitProviderRef:
  name: platform
```

The field name, description, and controller contract say that `gitProviderRef` names a
`GitProvider`; the object supplies the identity that varies per instance. This is the upstream
single-resource pattern.

A bare string such as `fooName: platform` is also a valid API choice when the contract genuinely
is and will remain only a name. Do not flag it merely for being a string. Prefer `fooRef: {name}`
for a new API when treating the value as a named, self-contained relationship improves consistency
or the relationship is likely to gain a field such as `namespace`.

Changing a bare string to an object is usually breaking. Starting with a `{name}` object avoids
that shape change, but it does not justify storing fields that the controller never reads.

### Namespace scope

- For **namespaced** resources, references should normally be **same-namespace**.
- Cross-namespace references are discouraged because namespaces are security boundaries.
  - If you allow them anyway, explicitly document semantics (creation ordering, deletion behavior, permissions), and consider admission-time permission checks or double opt-in.
  - Treat references to Secrets, credentials, ServiceAccounts, Routes/Gateways, and policy targets as especially sensitive. A controller that reads from or acts on a target in another namespace can become a confused deputy: the referrer asks a privileged controller to use access that the referrer would not otherwise have.
  - Prefer an explicit producer-side grant, similar to Gateway API `ReferenceGrant`: the namespace that owns the target object opts in to allowing references from the consumer namespace. Missing grants should result in a clear Condition such as `Ready=False` / `Stalled=True` with a permission-oriented reason.

### Choose the minimum complete reference

Classify the relationship before recommending a shape. The fields in `spec` must represent a
choice the API supports today, not metadata a future tool might want.

| Relationship | Instance shape | Controller responsibility |
|---|---|---|
| Fixed kind | `{name}`; add `namespace` only when the relationship intentionally crosses namespaces | Knows the target Group/Resource and chooses a served version. |
| Bounded set of kinds | `{kind, name}` and `namespace` when needed; add `group` only if it disambiguates a real choice | Validates the supported combinations and maps each to a resource. |
| Arbitrary object | `{group, resource, name}` and optional namespace | Uses discovery and reads only universal object data. |
| Field reference | Object identity plus `fieldPath` and the version required to interpret it | Validates and reads the declared field. |

Use `resource` rather than `kind` when a reference is genuinely generic or a dynamic Kind-to-resource
mapping could be ambiguous. `kind` is appropriate for a bounded, controller-defined mapping.

### Defaults, requiredness, and validation

- Do not default the identity (`name`). Require it and reject `""` unless an empty value has a
  documented meaning; `required: [name]` alone does not make an empty string unusable. Cluster API
  is the documented exception: `Machine.spec.bootstrap.dataSecretName` accepts `""`
  (`MinLength=0`) so managed node pools can declare that no bootstrap data is needed, while an
  unset value keeps the Machine `Pending`
  ([machine_types.go](https://github.com/kubernetes-sigs/cluster-api/blob/main/api/core/v1beta1/machine_types.go)).
- Do not put `apiVersion` in an object reference; the controller chooses a served version. Cluster
  API shows the cost of the alternative: its v1beta1 `Cluster.spec.infrastructureRef` is a core
  `ObjectReference`, so every manifest pins a provider version that must be edited on each provider
  upgrade (see the `v1beta2` pins inside a `v1beta1` Cluster in
  [knr-ops](https://github.com/polarsquad/knr-ops/blob/6fea00d0e5dfbf5b54104e045698b4d379b3b64f/mgmt/aws/clusters/eu-west-1/staging/cluster.yaml)).
  v1beta2 replaced it with `ContractVersionedObjectReference` (`apiGroup` + `kind` + `name`) and
  resolves the version from CRD contract labels
  ([cluster_types.go](https://github.com/kubernetes-sigs/cluster-api/blob/main/api/core/v1beta2/cluster_types.go)).
- Omit `namespace` for a same-namespace relationship. If it is present, make its defaulting and
  authorization semantics explicit rather than silently treating empty as another namespace.
- Do not add defaulted, enum-constrained `group` or `kind` to a fixed-kind reference. They are not
  user intent, create another compatibility surface, and make a one-kind API look polymorphic.
- A type selector may be required or may have a default only when omitting it has a real, documented
  meaning in a relationship that actually supports more than one target type.
- Treat every default as part of the public API contract.

## Configuration References: Recommended schemas

This section shows schema patterns for common reference use cases.

### Single-kind reference (controller knows the target)

The target type and scope are fixed by the API. The instance names it; it does not restate facts
the controller already knows. Put the fixed target and scope in the field description so a human
and a schema-aware tool have the same contract.

```yaml
connectionSecretRef:
  description: Names a Secret in this object's namespace that holds connection credentials.
  type: object
  required: [name]
  properties:
    name:
      type: string
      minLength: 1
```

Example usage:

```yaml
spec:
  connectionSecretRef:
    name: my-secret
```

### Bounded multi-kind reference

Use this when the user chooses between a known, finite set of types. `sourceRef` is a good role
name when the kinds are different ways to supply the same input. This resembles Flux's source
references: `kind` is present because it changes the target, not because it is metadata.

```yaml
spec:
  sourceRef:
    kind: GitRepository
    name: platform
```

Schema outline:

```yaml
sourceRef:
  description: Names a GitRepository or OCIRepository in source.example.com.
  type: object
  required: [kind, name]
  properties:
    kind:
      type: string
      enum: [GitRepository, OCIRepository]
    name:
      type: string
      minLength: 1
```

Add `group` when the supported kinds span groups and the user must select the group. Validate the
allowed group/kind pairs together.

### Tooling and navigation

Reference syntax alone is not a portable navigation protocol. A structural CRD schema can show
that `gitProviderRef` has a `name`, but it does not declare that the field resolves to
`configbutler.ai/GitProvider` in the referrer's namespace. Repeating a fixed Group/Kind in every
manifest only duplicates that hidden relationship; it does not make the relationship universally
discoverable or safe to trust. Kustomize reads `x-kubernetes-object-ref-*` hints for its own name
transformer, but those hints are not a Kubernetes-wide relationship contract.

Tool authors should publish or consume a versioned relationship map alongside the CRD contract.
The map must identify the referrer Group/Version/Kind, field path, identity paths, target
Group/Resource or allowed target set, and namespace semantics. An object-reference target
deliberately has no version: the controller and tool choose a served version. A field reference is
the exception because its `fieldPath` can have different meaning in different target versions. For
example:

```yaml
references:
  - referrer:
      group: configbutler.ai
      version: v1alpha3
      kind: GitTarget
      path: .spec.gitProviderRef
    identity:
      namePath: .name
    targets:
      - group: configbutler.ai
        resource: gitproviders
    namespace:
      mode: SameNamespace
```

A bounded-kind reference also declares `identity.selectorPath` and a `selectorValue` on each
target; the full proposal shows that shape.

The map may be generated from API-owned reference types, maintained as a plugin registry, or
published through a tool-specific CRD extension that is known to survive the generator and API
server. It is not a Kubernetes-standard schema feature. Keep the map and the controller's target
mapping under the same compatibility discipline as the CRD itself.

For an intentionally small, tool-facing proposal and its upstream precedents, see
[CRD relationship metadata](./relationship-metadata.md).

## Configuration References: Controller behavior guidance

- Assume the referenced object might not exist; surface a clear error via Conditions/Events.
- Watch referenced objects when their changes affect reconciliation. For example, if `spec.secretRef` affects generated configuration, Secret updates should enqueue the referring CR.
- Validate reference fields before using them as API path segments.
- Do **not** modify the referenced object (avoid privilege escalation vectors).
- Minimize copying values from the referenced object into the referrer (including `status` and Events/logs), to avoid leaking information a user may not have permission to read.
- If cross-namespace references are enabled, check both the reference and the authorization/grant decision before reading or using target data.

## Configuration References: Common review flags

- Reference fields not suffixed with `Ref`/`Refs`.
- Cross-namespace references without explicit semantics and guardrails.
- Cross-namespace references that rely only on the referrer's spec field, without a producer-side grant or equivalent authorization check.
- A scalar reference where the resolver needs additional identity, scope, or type-selection data.
- Generic reference objects in `spec` with fields the controller does not honor, such as `uid` or `resourceVersion`.
- Status/spec fields that echo data read from the referenced object without a clear, safe rationale.

## Lifecycle References

This section covers relationship types managed by controllers, not users. These are set by operator code, not in user-authored spec.

### ownerReferences: Parent-child lifecycle

`ownerReferences` is a Kubernetes-native mechanism for expressing parent-child lifecycle relationships. It is **system-managed** — set by controllers when they create child resources.

**Use case**: When your operator **creates and manages child resources** (e.g., an Operator creates Deployments, ConfigMaps, Services to implement a larger object).

**Key semantics**:

- **Lifecycle ownership**: Defines that the parent is responsible for the child.
- **Garbage collection**: When the parent is deleted, Kubernetes automatically deletes children (unless `blockOwnerDeletion: true`).
- **Finalizers**: Controllers can use finalizers to clean up before deletion.
- **Namespace boundary**: Do not use owner references to cross namespace boundaries. A namespaced child should point only at an owner in the same namespace or at a cluster-scoped owner; a cluster-scoped child should not point at a namespaced owner.
- **Not user-configured**: Users do not write `ownerReferences` in spec; controllers set them automatically.

**Example**: A CRD MyDatabase might create a Deployment, a Service, and a ConfigMap as children. The MyDatabase controller sets `ownerReferences` on these objects pointing back to the MyDatabase instance.

**Key distinction from configuration references**: `ownerReferences` expresses "I created this and manage its lifecycle," whereas configuration references express "I need to use or coordinate with this existing object."

## Community patterns

This section is intended to grow over time as additional patterns emerge.

### 1. Dependency graph with `dependsOn`

#### Reviewer guidance (generic CRDs)

`dependsOn` is a **tool-specific orchestration pattern**, not a general reference type. Accept it only if the controller/operator implements **all** of the following:

1. **Defines what "dependency satisfied" means** — typically the referenced object has `status.conditions[].type=Ready` with `status=True`.
2. **Specifies scope rules** — whether references are namespace-scoped, cross-namespace, or cluster-scoped; and if cross-namespace, what permissions/admission controls validate the dependency.
3. **Defines cycle behavior and circular dependency detection** — build and validate a DAG, and report cycles before reconciliation proceeds.
4. **Documents it clearly** — in API docs and operator guide.

#### Dependency graph validation pattern

If implementing `dependsOn`, validate a directed acyclic graph (DAG) at admission time when possible, and surface conflicts via conditions when detected at reconciliation time.

#### Recommended alternative approach

If your CRD does not implement active dependency orchestration:

- **Clear status conditions** — define explicit `Ready`, `Reconciling`, `Stalled`, and failure conditions that reflect your reconciliation state.
- **Explicit readiness checks** — implement controller logic that checks readiness of dependent resources before proceeding; do not rely on implicit field semantics.
- **Distinguish reference types** — configuration references (user-authored in spec), `dependsOn` (orchestration ordering), and `ownerReferences` (parent-child lifecycle). Use configuration references for object pointers, use `dependsOn` only with full DAG behavior, use `ownerReferences` for controller-managed children.

#### Note on cross-kind dependencies and reference type distinction

Flux's `dependsOn` is typically **same-kind** (Kustomization → Kustomization, HelmRelease → HelmRelease). Treat this as a **community pattern**, not a general reference rule, and avoid expanding it without explicit controller behavior and documentation.

**Summary of reference types**:

- **Configuration references** (user-authored in spec): "I need to use or coordinate with this object" (e.g., `secretRef`, `serviceAccountRef`)
- **`dependsOn`** (tool-specific orchestration): "Don't reconcile me until this other resource reaches Ready" (requires explicit controller implementation)
- **`ownerReferences`** (Kubernetes native, controller-managed): "I created this; manage its lifecycle and garbage collection" (set by controllers when they create child resources)

If your use case involves users specifying configuration dependencies, use configuration references. If it involves orchestrating **existing resources** in a specific order, use `dependsOn` with full DAG validation. If it involves an operator that **creates child resources**, use `ownerReferences`.

### References

- **Flux Kustomize/Helm CRD API** (reference for `dependsOn` pattern):
  https://fluxcd.io/flux/components/kustomize/api/v1/

- **Kubernetes ownerReferences** (lifecycle/garbage collection, not ordering):
  https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/

- **Upstream Kubernetes API conventions** (object references):
  https://github.com/kubernetes/community/blob/main/contributors/devel/sig-architecture/api-conventions.md
