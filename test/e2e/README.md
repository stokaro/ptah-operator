# End-to-end harness

`make e2e` creates one uniquely named kind cluster on an explicitly selected
remote Docker context, builds and loads the operator image, installs the Helm
chart with its CRDs and webhooks, and runs both control-plane and real
data-plane acceptance checks. Cleanup is automatic and is limited to the
cluster, authenticated registry container, external PostgreSQL container,
temporary image tags, source and kind node images first pulled by that
invocation, and an otherwise-unused kind network created by that invocation.
Images and networks that predated the run are preserved; a newly created kind
network is also preserved if another container attaches to it. Containers are
removed by their captured full Docker IDs rather than reusable names.
Content-addressed BuildKit cache layers are managed by Docker and are never
removed with a broad cleanup operation.

The local client needs Docker, kind, kubectl, Helm, Go, Git, OpenSSL, jq, SSH,
curl, and htpasswd.
The Kind version must exactly match `support/kubernetes.json`; kubectl must be
within one minor of the selected API server. The selected remote host only
needs the Docker daemon represented by the chosen context.

Required inputs:

- `K8S_VERSION`: exact Kubernetes version represented by the kind node image.
- `KIND_NODE_IMAGE`: digest-pinned `kindest/node` image matching
  `K8S_VERSION`. The harness reads the pinned image from
  `support/kubernetes.json` when the tested version is in the support window;
  callers must provide it explicitly for any other version.

The harness builds its Ptah executor from commit
`fe26eb5af616b3b48aa75bf5cdb59ac9306b7836` in a sibling Ptah checkout by
default. Set `E2E_PTAH_SOURCE_DIR` and `E2E_PTAH_REVISION` to select another
checkout and exact commit. When no sibling checkout exists, the harness clones
`E2E_PTAH_GIT_URL` into its task-owned temporary directory.
`E2E_EXECUTOR_IMAGE` may instead provide a digest-pinned external image, in
which case `E2E_PTAH_VERSION` is required. `E2E_RUNNER_IMAGE` may similarly
override the runner embedded in the freshly built operator image.

For a source build, the harness derives `E2E_PTAH_VERSION` from the selected
exact commit when the caller does not provide it. In both source and external
image modes, it passes that non-empty version explicitly to Helm and verifies
the same version-and-digest binding through the control-plane resources; no
chart or assertion fallback supplies a release label.

The registry, PostgreSQL, MySQL, and both e2e build images have digest-pinned
defaults. Each runtime image is copied into the disposable registry and
addressed by the digest produced by that registry before Kubernetes starts it.
Overrides supplied with `E2E_REGISTRY_IMAGE`, `E2E_POSTGRES_SOURCE_IMAGE`, and
`E2E_MYSQL_SOURCE_IMAGE` must also be digest-pinned.

`DOCKER_CONTEXT` defaults to `remote-dev-container`. The harness rejects the
local default and OrbStack contexts, and it derives an SSH tunnel from the
selected context so the local Kubernetes and Helm clients can reach the API
server hosted by the remote daemon.

The hosted CI matrix creates its own explicitly named loopback-SSH Docker
context on an ephemeral runner. It sets `E2E_DIRECT_HOST_ACCESS=1` because the
Docker daemon and clients share that one disposable host; the harness rejects
this tunnel-free mode outside `CI=true`.

Set `E2E_RUN_ID` to a CI run identifier for deterministic, collision-resistant
resource names. Local runs include the Git revision and process ID by default.
Set `K8S_VERSION` once per matrix job: the complete suite runs against that one
selected version, so the sliding support window does not hide data-plane gaps
behind a single preferred Kubernetes minor.

The control-plane checks cover manager readiness, CRD discovery, fail-closed
webhook configuration, authenticated approval stamping, approval
immutability, exact plan binding, refusal of every missing required schema,
plan, UID, or fingerprint field before hydration, refusal of conflicting
derived artifact or protocol bindings, absence of controller Secret-read
permissions, namespace-local references, and real API-server rejection of
nanosecond, negative, and over-deadline duration values. Verification-policy
ConfigMaps must be immutable. The API server must also reject whitespace-only,
edge-whitespace, control-character, overlong, and duplicate managed-scope
selectors without creating an operation Job. Empty keys in the target Secret,
development-target Secret, verification-policy ConfigMap, and OCI CA ConfigMap
selectors are likewise rejected by the real API server before any Job exists.

The data-plane checks use an authenticated OCI registry plus disposable
PostgreSQL and MySQL databases. They also start PostgreSQL 17 as a
digest-pinned, task-labeled Docker container on the kind bridge, with a tmpfs
data directory, no anonymous or persistent volume, no restart policy, and no
published host port. A selectorless Service and an exact owner-bound
EndpointSlice with a unique managed-by label route operation Jobs to its
captured bridge IP. The fixture role is demoted from superuser after
initialization while retaining database ownership. The suite proves that no
Kubernetes workload hosts that database, then performs a focused Plan, exact
approval, single Apply, post-Apply `NoChanges` proof, and an independent
Docker-side catalog check.

For each in-cluster engine the suite publishes a real schema
artifact to a mutable tag, verifies tag-to-digest and artifact-type evidence,
observes drift, publishes and exactly approves a plan, applies it, proves post-apply
convergence in the database, and forces a second complete Resolve, Verify,
Observe, and scoped Plan cycle whose explicit `NoChanges` result must remain a
no-op. A single lossless Job watch must prove that Resolve completed before
Verify was added and Verify completed before the first database Observe was
added.
The registry Secret independently grants its exact in-cluster authority through
the fixed `registry` key and explicitly grants the test-only cleartext transport
through `allowPlainHTTP: "true"`. Job isolation checks require the
credential-free authority guard to run before every credentialed Observe or
Plan fetch.
Before the first PostgreSQL Apply, the suite records an exact old-binding
approval, drains the manager Deployment to zero, and Helm-upgrades it with a
changed Ptah version while both image digests remain unchanged. The old
approval must become stale with no Apply. The replacement manager must execute
exactly one sequential Resolve, Verify, Observe, and Plan chain and publish a
distinct plan UID and fingerprint bound to the new version before the suite
grants a fresh approval.

After a successful periodic no-op and before moving the mutable tag, the suite
stops the exact captured registry container. It requires exactly one failed
Resolve, no later operation or Apply, byte-identical retained source, target,
plan, applied, and last-success evidence, and `Unknown` source-freshness
Conditions. The harness freezes retries while it captures that boundary,
restarts the same container ID, waits for its authenticated HTTP API through
the existing loopback tunnel, and then requires one ordered Resolve, Verify,
Observe, and `NoChanges` Plan recovery chain with zero Apply and restored
freshness Conditions.
Custom-CA coverage must instead use HTTPS and put the exact selected CA-byte
digest in the same registry Secret under the fixed `caSHA256` key. Those checks
run a digest-pinned, non-root, read-only TLS proxy with a task-scoped server
certificate whose SAN is the exact in-cluster Service DNS name. A separate
admin listener exposes only an atomic request count through the Kubernetes API
proxy for the exact captured Pod; it has no ClusterIP Service. One immutable
Secret has the right authority and a wrong CA digest;
another has the right CA digest and a wrong authority. Each must produce one
typed pre-child Resolve refusal, no other operation, and zero registry-request
delta while the exact proxy Pod UID, container ID, ready state, and zero restart
count remain unchanged. The matching Secret must complete Resolve, Verify,
Observe, and Plan over HTTPS, increase the same counter, reach approval without
Apply, and suspend. Completed Observe and Plan Pods prove that only the
credential-free guard mounts the source ConfigMap, that it creates a private CA
snapshot, that the credentialed fetch mounts only that read-only snapshot, and
that the database-bearing container receives only the fetched schema.
They then move the tag, require a new
plan and stale the unused old approval before applying the new schema. To make
that admission-versus-reconciliation race deterministic, the test briefly
removes only the controller's status-write verb, leaves webhook reads
available, verifies the changed authorization, moves the tag, and restores and
verifies the exact original verb list. A final destructive tag move must remain
blocked and must not create an Apply Job. The MySQL destructive fixture removes
a standalone plain index on `name` while retaining the separate unique
constraint on that column and all table columns.
Its native plan reports `DROP INDEX` without destructive metadata; the
published plan must conservatively elevate it to destructive, and the default
policy must refuse it while both indexes remain present. The refusal is held
across three scheduled reconciliations and is checked again after the entire
fault suite, including the exact Plan UID, Blocked conditions, zero Apply UIDs,
columns, unique constraint, and plain index.
An additional MySQL case submits a DSN containing both `multiStatements` and
an encoded server-session payload. Exact protocol results must refuse both
Observe and Plan before child dispatch, logs must not expose the payload, and
the complete column-and-index fingerprint must remain unchanged.

Fault-injection acceptance starts watches from exact Kubernetes
`resourceVersion` values for Jobs, Pods, schemas, approvals, and target Leases.
Each watch rotates through naturally expiring 30-second API segments and
streams each validated newline-delimited frame into an atomic evidence file
immediately, then resumes from the last completely framed resource version.
Final shutdown waits
for natural segment EOF, so a partially received event is never accepted or
silently discarded as evidence. Inert annotations advance every watched kind
at least once per segment; the suite never advances a watch position through
an unobserved list response, and any heartbeat failure is fatal.
Database metadata barriers hold two real PostgreSQL Apply operations and one
MySQL Apply operation after their database-local advisory locks are acquired.
Each assertion binds the database advisory-lock owner to the exact backend
waiting for the metadata lock; observing an unrelated lock holder and DDL
waiter cannot satisfy the test.
The two PostgreSQL targets use distinct coordination keys and databases, so
the suite proves same-engine controller independence with concurrent Jobs,
Pods, Leases, and native locks. It restarts the manager and requires a new
manager Pod UID without changing any Apply operation, Job, Pod, Lease, or
database-lock identity. It then gracefully deletes a blocked Apply Pod and
requires a terminal single-Pod Job, a durably consumed approval,
`OutcomeUnknown`, and read-only Observe with a fresh plan and no replay.
The complete history must retain the original uncertain Apply holder and lease
epoch through that exact Observe and Plan, preserve the immutable pending
target/source/plan snapshot, and release the Lease only after Plan completion.
The harness pauses only controller status harvesting at each proof boundary,
but first installs a harness-owned `NoSchedule` taint on every cluster node.
The exact read-only Job must remain unscheduled and nonterminal until status
write denial is confirmed; the harness then removes only that taint and
observes the exact terminal Job. It requires the original live Lease holder
and epoch plus a release-free Lease watch history before harvesting resumes.
The resulting unapproved Plan must remain bound to that snapshot, no Apply
attribution may be recorded, and canonical MySQL column-and-index fingerprints
must remain identical immediately after recovery and through the final delayed
replay window.
For successful blocked Applies, the complete schema and Lease watch histories
must show the original Apply holder and lease epoch continuously retained
through the exact post-Apply Observe and `NoChanges` Plan. The pending proof's
target, source, development target, exclusions, severity, timeouts, plan, and
coordination binding must be byte-for-byte equal to the immutable Apply
snapshot. At both terminal Job boundaries, the same controlled status-harvest
pause and scheduling barrier must prove that the original live Lease holder
and epoch remain and that the Lease watch contains no release, deletion, or
replacement event; only then may status harvesting resume and release the
Lease.
The original Apply Job must expose one exact, production-parsed protocol-v4
result whose mutating outcome, plan digest, coordination digest, and target
digest match both the persisted active operation and pending proof snapshot.
Both read-only proof Jobs must also expose one exact result:
Observe must report no managed drift with target and drift-report digests bound
to status, and Plan must report `NoChanges` with empty stdout and no content
digest. A transport-successful Job carrying an application error is rejected.

The watches reconstruct both Job and Pod lifetimes to reject overlap,
including every terminating interval. Two URL aliases for one database and
coordination key must contend on one Lease; after the holder releases that
Lease, the exact contender operation ID, Job UID, and persisted lease epoch
must match the same-UID Lease reacquisition. The contender must remain at zero
Jobs through the holder's final proof boundary and then dispatch exactly once,
preventing an absence-only contention assertion. Because the holder changes
the shared database first, the contender's old plan must return the exact
conservative stale-plan result with one exact Pod, become `OutcomeUnknown`, and
retain its reacquired Lease through a deterministically blocked read-only
Observe and `NoChanges` Plan. Its consumed approval must become stale, and the
database fingerprint must remain unchanged throughout that recovery. Deleting an unapproved schema must create no later Job
and must leave an exact database fingerprint unchanged through the remainder
of the suite. A manual database change after approval must execute none of the
planned SQL. Because the Apply Job was dispatched, even the exact native stale
diagnostic must report `mutationStarted=true`, `uncertain=true`, and cause a
durable `OutcomeUnknown`; the exact approval must be both consumed and marked
stale when a fresh plan replaces it. The controller must then dispatch exactly
one read-only Observe followed by exactly one read-only Plan, with no Apply Job
or Pod replay and an unchanged database fingerprint. The original manual-drift
Apply Lease holder and epoch must remain live through both deterministically
blocked read-only proof Jobs and may be released only afterward. Both results are parsed, bound to
their persisted evidence, and ordered by exact Job history; the plan UID and
actual-state fingerprint must change.

A separately built, test-only OCI publisher handcrafts a PostgreSQL artifact
containing credential-bearing principal DDL. The publisher image is audited to
contain no operator binaries, and the operator image is audited to contain no
publisher. The resulting Plan operation must return the exact fail-closed
`invalid_plan_output` result, publish no plan or approval, execute no SQL,
create no role, and leak none of the embedded credential. The schema is then
suspended because the CRD's maximum failure retry is shorter than the suite's
maximum duration; final watch history must still contain exactly the original
Plan Job and Pod UIDs and zero Apply UIDs.

PostgreSQL and MySQL use distinct stable coordination keys. The suite requires
their status and approval bindings to expose only the derived digest, never the
plaintext key, and binds every approval to runner protocol version 4.

Registry and database credentials are supplied only through namespaced
Secrets. Host-side generated credentials, including the external PostgreSQL
environment and URL material, are handed between scripts through mode-0600
files in a mode-0700 task directory, never through exported password variables
or command-line arguments. Docker-side readiness and catalog queries consume
the container's private environment without printing it. Every observed Job UID
must have a complete terminal log audit. Terminal Jobs are re-read by exact UID, their Pods
are selected by controller owner UID rather than a reusable name label, and
every exact terminated Pod and container is rechecked after its log scan before
the Job UID is certified. Exact operation results are extracted from one
complete integrity-bound protocol-v4 frame by the production parser, rather
than inferred from Job success alone. Every parsed frame is also bound to the
CR's persisted active-operation ID and Job UID and to the exact `ADDED` Job
annotation, so a stale or foreign frame cannot satisfy the suite. Any stdout
or stderr truncation is fatal because discarded output cannot be credential
audited. Log followers cover the manager replacement and active-Pod deletion
windows until natural EOF. The Pod-deletion proof adds and removes only its
named test finalizer by value, preserving Kubernetes and third-party
finalizers, and trap cleanup removes that same named value after an interrupted
run. The retained watch histories are scanned before cleanup. The
suite also repeatedly scans the enumerated non-Secret workload and custom
resources, manager logs, and current Pod logs for the exact password and
database URL values. Safety assertions compare
checkpointed Job UIDs, so deletion cannot hide an unexpected Apply or Plan
Job.
