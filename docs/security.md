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

## OCI integrity and identity

The operator always resolves a tag to SHA-256 content and records that digest.
It verifies policy output against the resolved digest, inspects the pinned
artifact independently, and requires the Ptah schema artifact type. These
checks provide content integrity and prevent artifact-type confusion.

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
intentionally inspectable by independently authorized approvers, but schema
structure may still be sensitive. Restrict ConfigMap and `PtahSchemaPlan` read
access in application namespaces accordingly. The built-in approver role is
cluster-scoped for convenience; installations with stricter tenancy should
bind namespace-scoped Roles instead.

Approval admission fails closed. It binds names to UIDs, rejects a plan whose
storage commit is incomplete, rejects changed policy bytes or target state, and
makes the stamped decision immutable. Ordinary Kubernetes RBAC is not treated
as field-level authorization.

## Output handling

The runner never invokes a shell. It checks command arguments against known
credential values, derives and redacts standalone and escaped credentials from
database URLs, bounds stdout and stderr, and validates a framed result containing
the operation ID and protocol version. Status stores only hashes, counts,
classification, immutable references, and timestamps.

Treat access to Job Pod logs as a separate privilege. The controller needs it
to harvest the framed result, while ordinary desired-state authors usually do
not.

## Remaining deployment responsibilities

- Apply namespace NetworkPolicies that allow executor Pods to reach only the
  required registry, DNS, and database endpoints.
- Grant the database user the minimum DDL and introspection privileges needed
  for the selected schemas. Do not use a cluster-wide administrative account.
- Pin manager, runner, and executor images by digest and verify their release
  provenance before installation.
- Protect the shared target-lock namespace from untrusted Lease writers.
- Use separate database credentials for production and optional dev rehearsal
  targets.
