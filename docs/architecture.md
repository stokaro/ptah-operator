# Architecture

## Control plane and data plane

The manager watches namespaced `PtahSchema`, `PtahSchemaPlan`, and
`PtahSchemaApproval` resources. It makes reconciliation decisions and creates
Kubernetes Jobs, but it never connects to a database and has no Secret read
permission.

Each operation runs a fixed command in a digest-pinned Ptah executor image. A
digest-pinned operator image installs `ptah-runner` into a shared `emptyDir`;
the executor invokes that runner instead of accepting user-provided commands.
The runner validates inputs and machine-readable output, bounds captured data,
redacts credential representations, and emits an integrity-framed result for
the controller.

## State machine

One persisted `status.activeOperation` is the serialization point:

1. **Resolve** records the immutable digest behind the requested OCI reference.
2. **Verify** evaluates the selected policy and independently inspects the
   digest-pinned artifact type.
3. **Observe** fetches the verified schema by digest and compares it with the
   live database.
4. **Plan** publishes exact plan bytes when drift exists.
5. **Approval** waits for an immutable resource when policy requires one;
   safe `Always` plans instead become `ReadyToApply` without an approval event.
6. **Apply** reconstructs and hashes the published bytes immediately before
   executing the exact plan.
7. **Observe** runs again and is the only transition that can establish
   convergence.

The controller writes the operation claim before it creates a deterministic
Job. After a restart it reads the claim and Job rather than planning a second
operation. A terminal Job is retained until its result is harvested; only then
does the controller add a bounded cleanup TTL.

## Immutable bindings

`PtahSchemaPlan` is controller-created and immutable. Its fingerprint binds:

- schema name and UID;
- desired artifact digest;
- credential-free database target identity;
- observed and desired state fingerprints;
- reconciliation policy and verification-policy bytes;
- Ptah version and digest-pinned executor image;
- runner image and protocol version;
- exact plan content digest.

Plan bytes are split into bounded immutable ConfigMaps. The plan status commits
the concrete ConfigMap UIDs only after every chunk has been read back and
verified. Apply checks names, UIDs, ordering, sizes, per-chunk hashes, and the
complete hash before dispatching the database process.

An approval repeats the important bindings and is stamped by mutating
admission with the authenticated username, UID, groups, request UID, and UTC
time. Validating admission reads the current schema, plan, and verification
policy directly from the API server. The controller performs the same current
state checks again before apply.

## Credential routing

| Operation | Registry credential | Database credential | Desired input |
| --- | --- | --- | --- |
| Resolve | yes | no | requested OCI reference |
| Verify | yes | no | requested reference plus resolved digest evidence |
| Observe fetch init | yes | no | digest-pinned schema artifact |
| Observe main | no | target only | local read-only schema file |
| Plan fetch init | yes | no | digest-pinned schema artifact |
| Plan main | no | target and optional dev target | local schema file |
| Apply | no | target only | immutable plan chunks |

The controller sees only Secret names and keys. Kubernetes resolves those
selectors in the Job Pod.

## Concurrency

The per-resource operation claim prevents two Jobs for one `PtahSchema`.
Database-level apply coordination uses an owner-neutral Lease keyed by the full
SHA-256 target identity. All schemas use one configurable coordination
namespace, so resources in different namespaces still contend for the same
database target. Different future Ptah resource kinds can use the same Lease
contract.

The Lease complements the database advisory lock used by Ptah. Its lifetime is
long enough to cover the maximum Job deadline and a manager outage. Apply
revalidates the plan after acquiring the Lease.

## OCI schemas and future migrations

Desired schemas and versioned migration directories are distinct artifact
types with different lifecycle semantics. They should share only mechanisms:

- tag resolution and digest pinning;
- registry authentication and custom transport trust;
- artifact verification and type enforcement;
- fixed Job construction and result framing;
- database target identity and cross-kind Lease coordination.

A schema answers “what should the database look like?” and is reconciled by
observation and direct exact plans. A migration stream answers “which ordered
versions have run?” and needs history, version ordering, checksum, baseline,
and rollout semantics. Combining them in one CRD would make approval and status
ambiguous, so versioned migrations remain a separate future controller.
