# Security model

## Trust boundaries

The operator separates four authorities:

1. A desired-state author may change `PtahSchema` but cannot approve a plan
   merely by editing that resource.
2. An approver may read schemas and plans and create immutable approvals. The
   chart creates an optional ClusterRole but never binds it automatically.
3. The controller may manage plans, Jobs, ConfigMaps, Leases, status, and
   Events. Its shipped ClusterRole contains no Secret permission.
4. A Job receives only the credentials needed for its fixed operation through
   same-namespace Secret selectors resolved by the kubelet.

Database-operation Pods disable service-account token mounting and service-link
environment injection. Every container runs as non-root with a read-only root
filesystem, `RuntimeDefault` seccomp, no Linux capabilities, no privilege
escalation, a deadline, bounded memory-backed work volumes, and no automatic
Job retry.

Before dispatch, the controller persists a canonical, credential-free snapshot
of the built-in Kubernetes mutations that may affect an operation Pod. It reads
ServiceAccount names and image-pull-secret references, LimitRange quantities,
RuntimeClass scheduling and overhead, and PriorityClass values, but never reads
the referenced Secret data. A fail-closed validating webhook checks the final
post-mutation Pod before scheduling. Only the exact snapshotted mutations are
accepted; executable, environment, volume, and security fields remain exact.
The controller retains read-only Pod evidence permissions and is not granted
Pod create or delete permission.

Memory-backed `emptyDir` usage is charged to the writing container by
Kubernetes. The chart defaults bound each volume, but production resource
limits must also leave headroom for the runner binary, fetched schema, plan,
and client scratch data in addition to the process heap.

## OCI integrity and identity

The operator always resolves a tag to SHA-256 content and records that digest.
It verifies policy output against the resolved digest, inspects the pinned
artifact independently, and requires the Ptah schema artifact type. These
checks provide content integrity and prevent artifact-type confusion.
Verification policy ConfigMaps must be immutable. Plans, approvals, active
operations, and post-Apply proof bind both the ConfigMap UID and exact policy
digest, so deleting and recreating the same name cannot preserve authority.

The current Ptah verification policy can require a signature artifact to be
attached, but that requirement is a presence check. It does not validate a
cryptographic signature, key, certificate identity, issuer, or transparency
log. Do not describe `ArtifactVerified=True` as publisher authenticity when a
policy relies on that field.

For production publisher identity, verify the digest cryptographically in the
artifact promotion pipeline or enforce an OCI admission/promotion policy before
the digest is referenced by `PtahSchema`. Keep the resource digest-pinned after
that decision. A later operator API may add an independently versioned verifier
contract; it must bind its verifier image, trust policy bytes, and evidence into
the plan rather than executing arbitrary user commands.

Plain HTTP registry transport is an explicit opt-in for trusted test or
air-gapped networks. Custom CA bundles and mutual-TLS client certificates are
supported through same-namespace ConfigMap and Secret references. Registry
credentials and database credentials are routed to different containers for
observe and plan operations.

## Plan and approval visibility

Plan ConfigMaps contain schema-changing SQL, not credentials. They are
intentionally inspectable by independently authorized approvers, but arbitrary
schema names, defaults, comments, and literals may still be sensitive. Restrict
ConfigMap and `PtahSchemaPlan` read access in application namespaces
accordingly. The built-in approver role can read plan metadata but deliberately
cannot read every ConfigMap cluster-wide. Grant a separate namespace Role
restricted to the current plan chunk names, as described in
[Exact-plan approvals](approvals.md).

Approval admission fails closed. It binds names to UIDs, rejects a plan whose
storage commit is incomplete, rejects changed policy bytes or target state, and
makes the stamped decision immutable. Both approval webhook configurations use
the non-configurable `Fail` policy, so an unavailable webhook cannot admit a
caller-supplied identity stamp. Ordinary Kubernetes RBAC is not treated as
field-level authorization.

## Output handling

The runner never invokes a shell. It checks command arguments against known
credential values, derives and redacts standalone and escaped credentials from
database URLs, bounds stdout and stderr, and validates a framed result containing
the operation ID, coordination digest, and protocol version. The required
`spec.target.coordinationKey` is a non-secret operator input; it is hashed with
the normalized engine, and the plaintext key is never copied into status.
Status otherwise stores only hashes, counts, classification, immutable
references, and timestamps.

The credential-free target identity includes connection-security semantics,
not merely host and database names. Rotating password or certificate bytes is
allowed when the non-secret route and certificate paths stay fixed, while a
change to TLS verification, channel binding, authentication requirements, or
plaintext fallback invalidates the plan before the mutating child dispatches.

Raw drift details are parsed in memory and excluded from the framed result.
Resolve and Verify follow the same boundary: native stdout is strictly decoded
before a small typed descriptor or requirement-name set is emitted, arbitrary
verification details and inspection metadata are discarded, and native stderr
or executor errors can produce only generic typed failures. No Resolve,
Verify, Observe, or Apply frame carries native stdout.
Planning executes twice under the target Lease, requires byte-identical native
plans, and validates the accepted bytes through a native Apply dry-run before
publication. The independent operator classifier may raise destructive
severity from the rendered SQL and never lowers executor metadata. It also
rejects credential-bearing principal DDL.

Apply native stdout and stderr are never copied into the framed result or
runner diagnostics, including failure paths. Only generic typed failures leave
the runner. Stale Apply is classified as pre-mutation only for the exact native
diagnostic bound to the reconstructed plan's source fingerprint; altered,
extra, or truncated output is treated as uncertain.

Treat access to Job Pod logs as a separate privilege. The controller needs it
to harvest the framed result, while ordinary desired-state authors usually do
not. A successful Plan frame necessarily transports exact plan bytes to the
controller before they are committed to immutable chunks, so Pod-log access is
at least as sensitive as plan-chunk access. Apply frames never contain native
SQL output.

## Remaining deployment responsibilities

- Apply namespace NetworkPolicies that allow executor Pods to reach only the
  required registry, DNS, and database endpoints. Start from the
  [egress-policy example](../examples/networkpolicy-egress.yaml).
- Grant the database user the minimum DDL and introspection privileges needed
  for the selected schemas. See [Database support and privileges](database-support.md)
  and do not use a cluster-wide administrative account.
- Pin manager, runner, and executor images by digest and verify their release
  provenance before installation.
- Protect the shared target-lock namespace from untrusted Lease writers.
- Install exactly one operator Helm release per cluster. Scale replicas within
  that release for high availability; the singleton admission configuration
  intentionally prevents ordinary independent-release ownership.
- Assign one stable coordination key to every physical database and reuse it
  across all aliases, proxies, credentials, namespaces, and Ptah resource kinds.
- Use separate database credentials for production and optional dev rehearsal
  targets.
