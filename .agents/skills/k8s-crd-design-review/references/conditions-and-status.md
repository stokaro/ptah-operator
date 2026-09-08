# Conditions and status (reference)

Use Kubernetes conventions for controller-observed state.

For GitOps/status-tool compatibility, also read [`./kstatus-readiness.md`](./kstatus-readiness.md) when reviewing long-running reconciled resources or any CRD expected to work well with Flux, Argo CD health checks, kpt, `kubectl wait`, or similar readiness consumers.

## Key recommendations

- Prefer `status.conditions` for readiness/health signals.
- Add `status.observedGeneration` and set it when reconciling.
- Prefer `observedGeneration` on each condition when using the `metav1.Condition` shape.
- If a controller writes `status`, enable `subresources.status`.

Additional recommendations:

- Prefer a single high-signal **summary condition**:
  - `Ready` for long-running resources.
  - `Succeeded` for bounded execution resources.
- Keep **polarity consistent** across Conditions, and choose names that avoid double-negatives (e.g., `Ready`/`Succeeded` are often easier to consume than `Failed`).
- Name Condition `type`s as **states**, not transition phases (e.g., prefer `Active`/`Available`/`Degraded` over `Progressing`/`Scaling`).
- Transition-style names (e.g., `Progressing`) can be acceptable for long-running transitions if treated as an observed state with consistent `True`/`False`/`Unknown` semantics.
- Treat `status.conditions` as a **map keyed by `type`**:
  - Do not append duplicates of the same `type`; update the existing entry.
  - Ordering should not matter.
- Ensure humans and automation can rely on them (e.g., `kubectl wait --for=condition=Ready=true`).
- Treat status as stale when `metadata.generation != status.observedGeneration`; do not read an old `Ready=True` as applying to the latest spec.

Also prefer Conditions over state-machine style `status.phase` or `status.state` for new APIs.

## State / phase fields

It is too strong to say "`state` is forbidden". The Kubernetes guidance is narrower:

- The old pattern of a single `status.phase` plus related message/reason fields is deprecated for newer API types.
- Conditions are observations, not a comprehensive state machine.
- New APIs should expose the important observable facts directly as conditions and status fields.

Use a `status.state`, `status.phase`, or enum summary only when it is secondary UX sugar or a domain fact that cannot be decomposed cleanly into conditions. Do not make it the only contract automation has to read.

### State/phase review checklist

Flag a `state` or `phase` field when:

- It appears under `spec` but the controller owns its transitions.
- It is the only way to tell ready/progressing/failed.
- Its enum values imply hidden booleans such as readiness, progress, failure, or deletion.
- Adding a new enum value would break existing clients, CEL rules, dashboards, or GitOps checks.
- It duplicates `Ready`, `Reconciling`, `Stalled`, or `Succeeded` without a clear UX reason.
- It has `message`, `reason`, or transition timestamps beside it instead of using conditions.

Accept a `state`-like field when:

- Conditions remain the primary machine-readable contract.
- The field is documented as a lossy summary for humans, printer columns, or dashboards.
- The value is a stable domain fact, not a controller-owned workflow phase.
- Compatibility impact of future values is explicitly considered.

### Naming conventions (quick)

- Condition `type` values are typically **CamelCase/PascalCase identifiers** (e.g., `Ready`, `Available`, `Degraded`).
- `reason` should also be CamelCase and machine-readable; `message` is human-readable.

### Validating allowed condition `type`s

- Usually **do not** strictly validate the allowed `type` set in the CRD schema; instead **document** the stable condition types your controller uses.
- Hard-enforcing `type` via enum/CEL makes adding a new condition type later a **breaking change** (the API server may reject new values).

## CRD fragment: enable status subresource

```yaml
spec:
  subresources:
    status: {}
```

## Status schema fragment (example)

This is a schema-level example. Tailor field names and required/nullable choices to your API.

```yaml
openAPIV3Schema:
  type: object
  properties:
    status:
      type: object
      description: Observed state of the resource.
      properties:
        observedGeneration:
          type: integer
          format: int64
          description: The generation observed by the controller.
        conditions:
          type: array
          description: Standard status conditions.
          # Conditions are treated as a logical map keyed by `type`.
          # Make that explicit for server-side apply / GitOps patchability.
          x-kubernetes-list-type: map
          x-kubernetes-list-map-keys:
            - type
          items:
            type: object
            required: [type, status, lastTransitionTime, reason, message]
            properties:
              type:
                type: string
                description: Condition type (CamelCase identifier, e.g., Ready).
              status:
                type: string
                enum: ["True", "False", "Unknown"]
              observedGeneration:
                type: integer
                format: int64
                description: The resource generation this condition was set from.
              lastTransitionTime:
                type: string
                format: date-time
              reason:
                type: string
                description: CamelCase machine-readable reason.
              message:
                type: string
                description: Human-readable details.

```

## Condition types

- Include at least one high-signal condition such as `Ready`.
- For long-running reconciled resources, prefer a kstatus-friendly trio:
  - `Ready`: aggregate answer for the latest observed generation.
  - `Reconciling`: active work or lag.
  - `Stalled`: blocked or insufficient progress.
- Add domain-specific conditions only when they provide meaningful operator value.

## Common review flags

- Readiness/lifecycle fields placed in `spec` but transitioned by the controller.
- Missing `observedGeneration` (tooling cannot tell whether status is current).
- Missing condition-level `observedGeneration` when using the standard `metav1.Condition` shape.
- Missing `subresources.status` (RBAC/patch semantics and concurrency issues).
- `status.phase` or `status.state` used as the primary lifecycle contract for a new CRD.
- `Ready` condition absent until success; generic status tools can treat the unknown resource as current.
- `Ready=False` without `Reconciling` or `Stalled`, leaving tools unable to distinguish progress from blocked failure.

## Links

- Kubernetes API conventions, Spec and Status: https://github.com/kubernetes/community/blob/main/contributors/devel/sig-architecture/api-conventions.md#spec-and-status
- Kubernetes API conventions, Typical status properties: https://github.com/kubernetes/community/blob/main/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties
- Helm HIP-0022, "Wait With kstatus": https://helm.sh/community/hips/hip-0022/
- Luis Ramirez, SuperOrbital, "Status and Conditions: Explained!": https://superorbital.io/blog/status-and-conditions/
