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

One persisted `status.activeOperation` is the ordinary serialization point:

1. **Resolve** records the immutable digest behind the requested OCI reference.
2. **Verify** evaluates the selected policy and independently inspects the
   digest-pinned artifact type.
3. **Observe** fetches the verified schema by digest and validates a raw,
   read-only drift report. The report's schema details stay inside the runner;
   status receives only a digest, dialect, counts, and severity summary.
4. **Plan** performs two independent native scoped plans while holding the
   database-realm Lease. It accepts only byte-identical results, then makes the
   native Apply path parse and dry-run those exact bytes. A no-change result is
   the authoritative convergence proof; changed bytes are published.
5. **Approval** waits for an immutable resource when policy requires one;
   safe `Always` plans instead become `ReadyToApply` without an approval event.
6. **Apply** reconstructs and hashes the published bytes immediately before
   executing the exact plan.
7. **Observe and Plan** run again under the original Apply Lease. Only a new,
   coherent no-change plan can establish convergence.

The controller writes the operation claim before it creates a deterministic
Job. It then resolves a bounded, credential-free Pod-admission snapshot and
persists its canonical digest as a separate boundary before any dispatch. The
snapshot binds the exact ServiceAccount, LimitRange defaults, RuntimeClass
scheduling and overhead, PriorityClass values, and configured built-in
admission behavior by UID and resourceVersion. Job and Pod annotations carry
that digest. A fail-closed validating webhook compares the final post-mutation
Pod against the persisted snapshot before scheduling, while retaining exact
matching for executable, environment, volume, and security fields. A changed
admission resource therefore rejects the Pod instead of reinterpreting an
already claimed operation.

Apply has another persisted dispatch boundary immediately before its only
permitted Job create attempt. After a restart, a dispatched Apply with a
missing or replaced Job is outcome-unknown and is never recreated.

`status.pendingObservation` is a separate durable safety claim. It snapshots
the applied plan, key-free target Secret selector, coordination digest,
observation policy, Apply holder, and Lease duration until a same-target
read-only observation completes. It takes
priority over phase changes, ordinary retries, and newer desired generations.
A namespace-wide exact-owner Pod scan binds Apply attempts by Job name and UID,
not mutable labels. A late or duplicate Apply Pod blocks proof while active and
invalidates any proof already in flight. At most eight Pod UIDs plus the total
attempt count are retained as bounded evidence.
A terminal Job remains the active operation until the controller has both read
its result and successfully scheduled its bounded cleanup TTL. A transient API
or RBAC failure therefore retries the transition instead of orphaning the Job.

## Immutable bindings

`PtahSchemaPlan` is controller-created and immutable. Its fingerprint binds:

- schema name and UID;
- desired artifact digest;
- stable database coordination-realm digest;
- credential-free database route identity;
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
| Observe/Plan authority guard | authority and transport grants only | no | digest-pinned reference plus optional CA source bytes |
| Observe fetch init | yes | no | digest-pinned schema artifact plus optional read-only CA snapshot |
| Observe main | no | target only | local read-only schema file |
| Plan fetch init | yes | no | digest-pinned schema artifact plus optional read-only CA snapshot |
| Plan main | no | target and optional dev target | local schema file |
| Apply | no | target only | immutable plan chunks |

The controller sees only Secret names and keys. Kubernetes resolves those
selectors in the Job Pod. For Resolve and Verify, the runner replaces a mounted
custom CA ConfigMap projection with a private snapshot before starting Ptah.
For Observe and Plan, only the credential-free guard mounts that projection; it
validates the fixed Secret-owned digest grant and copies exact bytes to a
dedicated EmptyDir before the credentialed fetch starts.

## Concurrency

The per-resource operation claim prevents two Jobs for one `PtahSchema`.
Database-level apply coordination uses an owner-neutral Lease keyed by a
SHA-256 digest of a versioned tuple containing the normalized engine and exact
`spec.target.coordinationKey`. The required key is a non-secret, stable name
for one physical database realm. Every resource that can mutate that database
must use the same key, including resources that connect through different DNS
aliases, proxies, or credentials. The plaintext key remains in spec; status,
plans, approvals, Jobs, and Leases carry only its digest.

The URL-derived `targetIdentityDigest` is a separate redirect guard within one
plan lifecycle. It binds the effective route, database name, username, role,
SQL namespace/search path, semantic session options, and non-secret transport
and authentication policy immediately before Apply. PostgreSQL TLS mode,
channel binding, required authentication, protocol policy, and certificate
paths are bound; MySQL TLS mode, plaintext fallback switches, authentication
mechanisms, and server public-key selection are bound. Password bytes and
connection timing controls do not enter this digest, so credential rotation
and liveness tuning remain possible. Certificate or CA bytes can rotate behind
an unchanged bound path without authorizing a transport downgrade. Default
ports, PostgreSQL route overrides, and equivalent spellings of the same IP
address are normalized. DNS absolute-name markers, address families, and MySQL
database-name casing are preserved;
ambiguous repeated scope parameters and multi-endpoint targets are rejected. A
route alias need not produce the same identity because cross-resource
serialization comes from the explicit coordination realm rather than an
unverifiable DNS inference.

All schemas use one configurable coordination namespace, so resources in
different namespaces still contend for the same database target. The manager
also creates its fixed-name leader-election Lease there. Replicas in the one
supported Helm release form one ownership domain with exactly one active
reconciler, while webhook servers remain available on every ready replica.
Different future Ptah resource kinds can use the same target Lease contract.

Managers currently watch cluster-wide resources, and the fixed
`ptah-operator-admission` configurations form a singleton admission
availability domain. Exactly one Helm release is supported per cluster;
high availability comes from replicas within that release. Independent
releases are unsafe without disjoint watch and admission scopes.

The Lease complements the database advisory lock used by Ptah. Its immutable
duration covers the maximum Job deadline plus grace. The same holder is
renewed through post-Apply proof, including retry delays. If Job creation or
identity is uncertain, read-only proof waits for a complete Lease duration so
a possibly unobserved mutating Pod cannot overlap it. Apply reconstructs the
exact plan and revalidates the target after acquiring the Lease.

Raw drift is intentionally advisory: its selector language is not reused as a
planning scope, and its details never authorize Apply. The authoritative Plan
uses the exact `spec.policy.exclude` scope twice, requires byte identity, and
then passes those bytes through the native Apply parser in dry-run mode. Apply
recognizes a stale-plan refusal only when the strict native diagnostic names
the reconstructed plan's source fingerprint; that typed refusal is
pre-mutation, while every other Apply failure after dispatch remains
outcome-unknown.

## OCI schemas and future migrations

Desired schemas and versioned migration directories are distinct artifact
types with different lifecycle semantics. They should share only mechanisms:

- tag resolution and digest pinning;
- registry authentication and custom transport trust;
- artifact verification and type enforcement;
- fixed Job construction and result framing;
- database coordination realms, route identity guards, and cross-kind Leases.

A schema answers “what should the database look like?” and is reconciled by
observation and direct exact plans. A migration stream answers “which ordered
versions have run?” and needs history, version ordering, checksum, baseline,
and rollout semantics. Combining them in one CRD would make approval and status
ambiguous, so versioned migrations remain a separate future controller.
