# CRD relationship metadata (exploratory proposal)

> **Status:** An intentionally small proposal for tool authors. It is not a
> Kubernetes, Flux, or controller-runtime standard.

## Problem

A CRD schema can describe the shape of `spec.gitProviderRef.name`, but it does
not machine-readably say that the value resolves to a namespaced
`configbutler.ai` `gitproviders` resource. Editors, graph tools, and
GitOps-aware refactorers therefore need project-specific knowledge to provide
"go to definition", find usages, safe rename, and dependency-graph features.

Putting a defaulted `group` and `kind` into every fixed-kind reference is not a
solution. Those values are not a choice made by the manifest author, enlarge
the public API, and falsely imply that the reference is polymorphic.

## Design principles

- **Describe; do not control.** Metadata helps tools discover relationships. It
  does not grant access, validate target existence, decide reconcile order, or
  implement cross-namespace authorization.
- **Keep author intent in the instance.** A fixed-kind reference can remain
  `{name}`. A bounded multi-kind reference keeps `kind` because selecting the
  kind is real intent.
- **Use Group/Resource for targets.** A resource is the canonical API endpoint;
  Kind is safe only where a controller owns a predefined mapping.
- **Do not assume a GVK/GVR bijection.** Use declared Group/Resource or trusted
  discovery. If a mapping is absent or ambiguous, report the relationship as
  unresolved rather than guessing.
- **Declare namespace semantics.** Same-namespace, explicit-namespace, and
  cluster-scoped targets must be distinguishable. Cross-namespace authorization
  remains a separate concern, for example a `ReferenceGrant`-style opt-in.
- **Version the metadata separately.** The metadata must evolve and survive CRD
  version conversion without forcing a change to stored custom resources.

## Minimal relationship model

The conceptual model contains a referrer, a field path, an identity shape, one
or more possible targets, and namespace semantics.

```yaml
version: v1alpha1
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

The referrer has a version because its schema path can change between CRD
versions. The target deliberately has no version: an object reference identifies
a Group/Resource, while the controller and tooling resolve a served version. A
tool may obtain the display Kind through discovery or the target CRD; it must not
infer the resource from an arbitrary Kind. A field reference is the exception:
it needs a target version because a `fieldPath` can have different meaning in
different versions.

### Bounded multi-kind example

For a Flux-style `sourceRef`, the selector is part of the user-authored
reference and maps to a bounded, implementation-owned target set:

```yaml
version: v1alpha1
references:
  - referrer:
      group: example.io
      version: v1
      kind: Delivery
      path: .spec.sourceRef
    identity:
      namePath: .name
      selectorPath: .kind
    targets:
      - selectorValue: GitRepository
        group: source.toolkit.fluxcd.io
        resource: gitrepositories
      - selectorValue: OCIRepository
        group: source.toolkit.fluxcd.io
        resource: ocirepositories
    namespace:
      mode: SameNamespaceOrField
      path: .namespace
```

The exact names are illustrative. Candidate namespace modes are
`SameNamespace`, `FromField`, `SameNamespaceOrField`, and `ClusterScoped`.
`SameNamespaceOrField` uses the named field when present and otherwise the
referrer's namespace. A standard must precisely define these modes and the
field-path syntax before declaring this wire format stable.

## Delivery path

### 1. Prototype outside the CRD schema

Start with the versioned model in a CRD metadata annotation or a plugin registry
owned by the API provider. For example:

```yaml
metadata:
  annotations:
    relationships.configbutler.io/v1alpha1: |-
      references:
        - referrer:
            group: configbutler.ai
            version: v1alpha3
            kind: GitTarget
            path: .spec.gitProviderRef
          identity: {namePath: .name}
          targets:
            - group: configbutler.ai
              resource: gitproviders
          namespace: {mode: SameNamespace}
```

This is an experimental transport, not a claim on a Kubernetes-owned annotation
namespace. It lets an editor prototype prove navigation and rename behavior
without changing resource instances or requiring API-server support.

### 2. Generate the same model from API-owned declarations

Avoid a hand-maintained map. A controller-gen extension or a small companion
generator should emit the metadata from field-level declarations in the API
types, then test that the metadata and generated CRD stay synchronized.

### 3. Standardize beyond Kustomize only after adoption

Kustomize already reads the property-level
`x-kubernetes-object-ref-api-version`, `x-kubernetes-object-ref-kind`, and
`x-kubernetes-object-ref-name-key` hints when it loads CRD schemas into its
name-reference transformer configuration. This is live, useful prior art—not a
historical or Kubernetes-wide standard. Its model pins an API version and Kind,
does not encode Group/Resource or namespace semantics, and has not become a
shared SIG-tooling contract.

After an annotation or registry prototype demonstrates cross-tool value, the
eventual Kubernetes-defined form could be a property-level
`x-kubernetes-reference` extension:

```yaml
gitProviderRef:
  type: object
  required: [name]
  properties:
    name:
      type: string
      minLength: 1
  x-kubernetes-reference:
    targets:
      - group: configbutler.ai
        resource: gitproviders
    namespace:
      mode: SameNamespace
```

`x-kubernetes-reference` does not exist as a Kubernetes-defined CRD extension
today. It would need an API-machinery proposal that specifies validation,
preservation through conversion, and publication in OpenAPI. Consumers that do
not understand it must be able to ignore it safely.

## Non-goals

- Replacing the upstream object-reference shapes.
- Adding Group/Kind/Version values to fixed-kind resource instances.
- Verifying that a target exists or is ready.
- Authorizing cross-namespace references; use a target-owner opt-in such as
  Gateway API `ReferenceGrant` where that is required.
- Expressing controller ownership (`ownerReferences`) or orchestration
  (`dependsOn`).

## Why Flux is a useful design partner

Flux already distinguishes the two key cases. Its single-purpose references,
such as a Secret reference, use only a name. Its `Kustomization.spec.sourceRef`
uses `kind`, `name`, and an optional namespace because the caller selects from
a documented set of source types. Relationship metadata could tell tools about
that mapping without changing Flux manifests or controller behavior.

The proposal should therefore ask Flux to review a low-cost, generated metadata
contract—not to adopt a new reference object shape or a ConfigButler-specific
registry. A useful first interoperability test is whether one plugin can follow
a ConfigButler fixed-kind reference and a Flux bounded source reference from
their installed CRDs alone.

## Ecosystem observations

This is a small survey of published contracts, not a claim that one reference
shape fits every relationship. It distinguishes input references from selector
and output relationships, which navigation tooling must model differently.

| Ecosystem | Reference form | What it means |
| --- | --- | --- |
| [Flux][flux] | Fixed: `name`; sources: `kind` + `name` | Bounded choice. |
| [ESO][eso] | store: `name` + `kind`; `remoteRef.key` is external | Scope choice. |
| [cert-manager][cm] | issuer: `name`; `kind`, `group` defaulted | Open issuer set. |
| [Cluster API][capi] | `apiGroup` + `kind` + `name` | Provider target. |
| [Crossplane][xp] | `...Ref`: name or selector | Codegen knows target. |
| [Kustomize][k] | `x-kubernetes-object-ref-*` | Transformer hint. |
| [Sealed Secrets][ss] | Template produces a Secret | Output relation. |
| [Argo CD][argo] | Config names; graph carries GVK + UID | Different models. |

`kind` appears when it selects a target with meaningfully different behavior:
for example a source type, issuer, or namespaced versus cluster-scoped store.
It is not merely descriptive metadata for a fixed target. cert-manager's
`issuerRef` defaults `kind` to `Issuer` and `group` to `cert-manager.io`
([IssuerReference][cm-type]); that is permitted by the defaults rule because
external issuers make the target set open-ended, so omitting the selector has a
real meaning. Conversely, a SealedSecret is a recipe for an output Secret, not a
reference to an existing Secret; an editor should present a separate `produces`
edge, not treat its template as an object reference.

A production layout confirms the survey. [knr-ops][knr] (Flux + Cluster API +
ACK) writes bounded `{kind, name}` references for every Flux source, GVK-pinned
`{apiVersion, kind, name}` references for Cluster API v1beta1 objects, a
wrapped `roleRef: {from: {name, namespace}}` for ACK, and plain scalar names
(`clusterName`, `dataSecretName`, `secretName`) for fixed kinds. Several of its
relationships have no reference field at all: label selectors on
`ClusterResourceSet`, the `<cluster>-kubeconfig` naming convention, and ACK
references that resolve to AWS ARNs rather than objects. A navigation tool that
follows only `*Ref` fields misses most of that graph; the ACK wrapper and the
selector, produces, convention, and external-target edges are follow-up work
for this proposal.

Two examples are especially relevant. Cluster API's
`ContractVersionedObjectReference` contains Group, Kind, and name, then resolves
the version from CRD contract labels. Crossplane keeps a managed-resource
`...Ref` as a name or selector, while its generator receives the target Go type
through an API-owned declaration. Both support this proposal's separation of
author intent from target-mapping metadata. Crossplane's declaration is not
published in the installed CRD, however, so another editor cannot use it alone.

No surveyed project publishes a portable CRD relationship map. Kustomize's
property hints are the closest deployed schema precedent, but are intentionally
limited to its name transformer. A small generated map can build on these
practices without requiring every API to adopt the same instance shape.

## Related work and source links

- [Kubernetes API conventions: Object references](https://github.com/kubernetes/community/blob/main/contributors/devel/sig-architecture/api-conventions.md)
  define the single-, multi-, generic-, and field-reference models this
  proposal preserves.
- [Flux Kustomization source references](https://fluxcd.io/flux/components/kustomize/kustomizations/#source-reference)
  document the bounded `kind` choice and optional cross-namespace field.
- [Flux Kustomize API reference](https://fluxcd.io/flux/components/kustomize/api/v1/)
  exposes `CrossNamespaceSourceReference` as a typed API contract.
- [Kustomize CRD-schema loader](https://github.com/kubernetes-sigs/kustomize/blob/master/api/internal/accumulator/loadconfigfromcrds.go#L116-L125)
  shows the live `x-kubernetes-object-ref-*` hints. They are Kustomize-specific
  name-reference metadata, not a portable relationship contract.
- [Kustomize issue #4095](https://github.com/kubernetes-sigs/kustomize/issues/4095)
  records the practical difficulty of sharing those transformer configurations.
- [Kubernetes CRD API reference](https://kubernetes.io/docs/reference/kubernetes-api/apiextensions/custom-resource-definition-v1/)
  lists the supported schema extensions available today.
- [Kubernetes API union extension KEP](https://github.com/kubernetes/enhancements/blob/master/keps/sig-api-machinery/1027-api-unions/README.md)
  is a useful precedent for defining a new `x-kubernetes-*` extension and
  preserving it through OpenAPI conversion.
- [Gateway API ReferenceGrant](https://gateway-api.sigs.k8s.io/reference/api-types/referencegrant/)
  is a precedent for making cross-namespace reference authorization explicit.

[argo]: https://github.com/argoproj/argo-cd/blob/master/pkg/apis/application/v1alpha1/types.go#L2193-L2209
[capi]: https://github.com/kubernetes-sigs/cluster-api/blob/main/api/core/v1beta2/common_types.go#L390-L417
[cm]: https://cert-manager.io/docs/usage/certificate/
[cm-type]: https://github.com/cert-manager/cert-manager/blob/master/pkg/apis/meta/v1/types.go
[eso]: https://github.com/external-secrets/external-secrets/blob/main/apis/externalsecrets/v1/externalsecret_types.go
[flux]: https://fluxcd.io/flux/components/kustomize/api/v1/
[k]: https://github.com/kubernetes-sigs/kustomize/blob/master/api/internal/accumulator/loadconfigfromcrds.go#L116-L125
[knr]: https://github.com/polarsquad/knr-ops/tree/6fea00d0e5dfbf5b54104e045698b4d379b3b64f
[ss]: https://github.com/bitnami/sealed-secrets#sealedsecrets-as-templates-for-secrets
[xp]: https://github.com/crossplane/crossplane-tools#reference-resolvers
