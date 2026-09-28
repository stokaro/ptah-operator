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
Before any collision checks or mutable work, the harness atomically claims its
derived identity with a labeled volume on the selected daemon. Docker preserves
the first creator's immutable label nonce, so concurrent runs with the same
explicit `E2E_RUN_ID` cannot both enter creation or cleanup. The claim itself is
removed only after its exact owner labels are revalidated.
Content-addressed BuildKit cache layers are managed by Docker and are never
removed with a broad cleanup operation.

The local client needs Docker, kind, kubectl, Helm, Go, Git, OpenSSL, jq, SSH,
curl, and htpasswd.
Helm must be version 4 or newer, and the driver refuses an older one: the suite
reads the field ownership Helm's server-side apply leaves behind, which Helm 3
does not produce.
The Kind version must exactly match `support/kubernetes.json`; kubectl must be
within one minor of the selected API server. The selected remote host only
needs the Docker daemon represented by the chosen context.

Required inputs:

- `K8S_VERSION`: exact Kubernetes version represented by the kind node image.
- `KIND_NODE_IMAGE`: digest-pinned `kindest/node` image matching
  `K8S_VERSION`. The harness reads the pinned image from
  `support/kubernetes.json` when the tested version is in the support window;
  callers must provide it explicitly for any other version.

The harness builds its Ptah executor from the commit the compatibility
catalog records as verified, read from
[`support/ptah.json`](../../support/ptah.json), in a sibling Ptah checkout by
default. The catalog is the only place that commit is written down, because
the claim it publishes and the build this suite exercises have to be the same
one. Set `E2E_PTAH_SOURCE_DIR` and `E2E_PTAH_REVISION` to select another
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

Failure diagnostics print the cluster inventory and the Helm release status, and
withhold pod logs. The manager and the execution Jobs hold the database and
registry credentials this operator exists to keep away from the controller, and
diagnostics run at the moment a credential boundary failed; a pull-request run
log is public.

`E2E_DEBUG_LOGS=1` prints them, and is refused under `CI=true` for that reason --
the mirror image of `E2E_DIRECT_HOST_ACCESS`, which is refused anywhere else. It
also has to start collecting before anything fails: a hook Job carries
`helm.sh/hook-delete-policy: hook-failed`, so Helm removes a failed hook and its
pod before any end-of-run diagnostic could read it, and following each pod from
the moment the cluster exists is what holds that output. The same switch
prints the stderr of an upgrade Helm refused before it wrote a revision: a
template `fail` or a values-schema rejection leaves no hook, no pod and no
revision to inspect, so that stderr is the only record of which template
refused and why.

`E2E_KEEP_ON_FAILURE=1` keeps what a failed run would otherwise remove: the kind
cluster with its kubeconfig, the registry and database containers, the work
directory, and, when the failure is inside the CRD upgrade phase, that phase's
work directory and every proof object it created. A refusal is then read from
the objects that produced it instead of reconstructed from a log, and a single
phase can be replayed against the retained cluster in minutes rather than
through a fresh run. Nothing in CI sets it; the run names what it kept, and the
caller removes those resources by name afterwards.

Replaying one phase is what `hack/e2e-rerun-phase.sh <work-dir> <phase>` does.
Each phase is a separate script or Go test driven entirely by the environment
the harness hands it, and the harness records that environment beside the work
directory, so the tool puts the phase back on the retained cluster from the
working tree:

```bash
hack/e2e-rerun-phase.sh /tmp/ptah-operator-e2e.XXXXXX uninstall
```

A Go phase is rerun through `go test`, so an edit to it is compiled into the
rerun. That turns an edit to a phase or to the chart into a loop of minutes
instead of the hour and three quarters a run spends rebuilding the state the
last phase needs. It does not rebuild the manager image, which the cluster
pulled from the commit the run snapshotted, so a change under `cmd/` or
`internal/` still needs a full run. The tool says so when it starts.

Two things are worth knowing before reaching for a container snapshot instead.
A rendering refusal reproduces in seconds with `helm upgrade --dry-run=server`
against the retained cluster; a plain `--dry-run` does not, because it disables
`lookup` and the chart then sees none of the live objects its refusals are
about. And a failed hook Job is gone once Helm returns, because the hook
carries `hook-delete-policy: before-hook-creation,hook-succeeded,hook-failed`.
Helm prints its log to stderr first, because it also carries
`hook-output-log-policy: hook-failed`, so the refusal is in the failed
command's stderr, which a phase shows under `E2E_DEBUG_LOGS=1`. To keep the Job
itself, remove `hook-failed` from the delete policy in a local copy of the
chart and upgrade with it.

Set `E2E_RUN_ID` to a CI run identifier for deterministic, collision-resistant
resource names. Local runs include the Git revision and process ID by default.
Set `K8S_VERSION` once per matrix job: the complete suite runs against that one
selected version, so the sliding support window does not hide data-plane gaps
behind a single preferred Kubernetes minor.

The control-plane checks cover manager readiness, CRD discovery, fail-closed
webhook configuration, authenticated approval stamping, approval
immutability, exact plan binding, refusal of every missing required schema,
plan, UID, or fingerprint field, refusal of a plan of another schema, a
replaced plan UID and a plan binding the API no longer carries, absence of
controller Secret-read
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
Kubernetes workload hosts that database, then, under `apply: Always`, performs
a focused Plan, a single Apply with no approval, a post-Apply `NoChanges`
proof, and an independent Docker-side catalog check.

It then publishes a second artifact that adds one new `SECURITY DEFINER`
function, which Ptah rates safe and not destructive and writes as a plain
`CREATE FUNCTION` -- the routine did not exist, so nothing is replaced -- so
only the privilege class stands between that plan and an unattended Apply.
The plan must record `SecurityDefiner`, and the resource must wait with
`ApprovalRequired` reason `PrivilegeChanges` and a condition message that names
the kind and nothing the statement says. It has to hold through its persisted
refresh deadline and the refresh after it: every Apply read on every poll finds
none, the refresh Resolve is dated at or after the deadline, the deadline sits
a full interval after the Plan Job completed, and the database has no such
function. One exact approval then applies it, and the database holds the
function with definer rights.

A third artifact adds only `GRANT SELECT ON TABLE e2e_widgets TO PUBLIC`. Ptah's
drift report has no category for a grant, so the observation has to be recorded
as drift in no category -- a `safe` highest severity, no findings, no count --
and the scoped plan that follows has to record `Grant` and nothing else. The
reading that matched is the one asserted: the resource waits with
`ApprovalRequired` reason `PrivilegeChanges`, no condition message and nothing
in `status.target` names the table, the privilege or the grantee, no Apply Job
runs, and PUBLIC still cannot read the table. One exact approval then applies
it, the resource converges, and PUBLIC can.

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
Each watch rotates through naturally expiring 30-second API segments, keeps
every event in the order the API server sent it, and resumes from the last
resource version it read. Final shutdown waits for the open segment to end on
its own, so no event in flight is lost from the evidence. Inert annotations
advance every watched kind at least once per segment; the suite never advances
a watch position through an unobserved list response, and any heartbeat
failure is fatal.
Database metadata barriers hold two real PostgreSQL Apply operations and one
MySQL Apply operation after their database-local advisory locks are acquired.
Each assertion binds the database advisory-lock owner to the exact backend
waiting for the metadata lock; observing an unrelated lock holder and DDL
waiter cannot satisfy the test.
The two PostgreSQL targets use distinct coordination keys and databases, so
the suite proves same-engine controller independence with concurrent Jobs,
Pods, Leases, and native locks. It restarts the manager and requires a new
manager Pod UID without changing any Apply operation, Job, Pod, Lease, or
database-lock identity. It then signals the runner of a blocked Apply Pod to
terminate, without deleting the Pod, and requires a terminal single-Pod Job, a
durably consumed approval, `OutcomeUnknown`, and read-only Observe with a
fresh plan and no replay.
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
The original Apply Job must expose one exact, production-parsed result in the
runner protocol the release records, whose mutating outcome, plan digest,
coordination digest, and target digest match both the persisted active
operation and pending proof snapshot.
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
planned SQL. Because the Apply Job was dispatched, even the `stale-plan`
refusal in Ptah's apply report must report `mutationStarted=true`,
`uncertain=true`, and cause a durable `OutcomeUnknown`; the exact approval must be both consumed and marked
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

The same isolated fixture image carries `/e2e-alert-sink`, the webhook receiver
the alerting phase points Alertmanager at. It writes one JSON line per delivered
alert and refuses a payload it cannot read, so the phase asserts on what was
delivered. The controller image audit refuses it exactly as it refuses the OCI
publisher. The phase's Prometheus and Alertmanager images are pinned by digest
in `hack/e2e-kind.sh` and mirrored into the run's registry only when the suite
runs the alerting phase.

PostgreSQL and MySQL use distinct stable coordination keys. The suite requires
their status and approval bindings to expose only the derived digest, never the
plaintext key, and binds every approval to the runner protocol version
`support/ptah.json` records, which `hack/verifyptahsupport` holds to
`runner.ProtocolVersion`.

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
complete integrity-bound frame of that runner protocol by the production
parser, rather than inferred from Job success alone. Every parsed frame is also bound to the
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

## Phases in Go

The phases are moving from shell scripts under `hack/` to Go tests in this
directory, one suite at a time. The certificates suite and the data-plane
suite are ported: the control-plane phase, `assert`, and the data plane
itself, `dataplane`, restart and fault injection included. So are the two
migration phases, `migrations-postgresql` and `migrations-mysql`; the reference
data and the alerting phase that share their suites are still scripts. The
driver keeps
the bootstrap: the kind cluster, the images, the registry, the
databases and the chart install. Before it creates the cluster it builds one
test binary from the snapshot:

```bash
go test -tags e2e -c -o "$WORK_DIR/ptah-e2e.test" ./test/e2e
```

and runs a Go phase by name, from this directory:

```bash
ptah-e2e.test -test.v -e2e.phase=cert-rotation -e2e.completed="$WORK_DIR/go-phase-cert-rotation.completed"
```

The `e2e` tag keeps the phases out of a plain `go test ./...`, which runs only
their unit tests. What each phase is lives in `phases/`: the test function, the
environment variables it reads as a struct, the scenarios it records and the
bound it runs under. The binary runs that phase's test and nothing else, sets
its timeout from the declaration, and fails a run in which the test did not
reach its end or a scenario did not run, since `go test` reports a filter that
matched nothing as a pass. Only a phase that passed writes its name to the
completion record, and the driver passes the phase on that record rather than
on the exit status alone.
`harness/` loads the inputs, reaches the cluster with a controller-runtime
client that reads straight from the API server, waits with failures that name
what was awaited and what was last seen, and writes each scenario into the
timing ledger the shell phases write. A phase that fails prints its reason and
the diagnostics the shell phase printed, such as `kubectl describe` of a
Deployment that did not roll out.

`TestCertRotation` runs its scenarios in order, each starting from the state
the one before it left. The manager cannot read the webhook Secret, and the
rotator's own identity, bound to its running Pod, cannot create a Secret
outside its recovery contract. Helm's live lookup keeps each webhook entry's
trust apart across an upgrade: every managed entry starts from the serving root
plus a root of its own, and the upgrade keeps exactly that. A corrupt `ca.crt`
is recovered by staging a new CA, publishing old and new in every entry while
the Secret still holds the corrupt value, and switching no earlier than
`--ca-switch-delay` after the expansion, dated by the staging record and the
Secret's field management. A deleted Secret is recreated at once rather than
after the switch delay, with the chart's exact labels, annotations and four
fields, and every entry contracts to its CA.

`TestControlPlaneContract` is the `assert` phase, the control-plane checks
described above. It sends its requests as the documents the shell phase sent,
built as JSON objects rather than typed structs: a typed PtahSchema would
serialize its zero durations as `0s`, and the API server would refuse a
request the phase never meant to make. It reads everything back typed, except
where the claim is about the stored representation itself, such as a default
the API server wrote as `10m` or an approval spec that carries exactly six
keys. It creates with strict field validation, as `kubectl create` does, so a
field the API dropped is refused instead of pruned. The plan fingerprint and
the realm digest are derived here, independently of the operator, and a unit
test holds both derivations to `internal/fingerprint`.

`TestDataPlane` is the `dataplane` phase: the fixtures the scenarios share,
both engine lifecycles, the external PostgreSQL rows, the refusals, the fault
injection, and the four-eyes and Pod-metadata rows, as described above. The
fault injection is five scenarios. `watches` starts the recorders, refuses
the credential-bearing principal artifact and stands up the fault schemas.
`job-deadline` lets Kubernetes end an Apply at its deadline, then holds three
Applies at database barriers, and `manager-restart` replaces the manager under
them. `runner-termination` ends one runner and follows its recovery, the two
PostgreSQL convergences and the shared-alias realm. `job-deletion` removes a
held read-only Job, deletes a schema awaiting approval, drifts a database by
hand, and closes the watch history. It
sends and reads on the same terms as `TestControlPlaneContract`. Its first
scenario stands up the registry endpoint, the databases, the TLS proxy and the
admission fixtures; the migration suites run the phase with
`E2E_DATAPLANE_MODE=prepare`, and it stops there through `Run.Prepared`, which
fails a run that stops anywhere but the boundary `phases/` declares.

Every wait audits the Jobs that finished since the last reading, before the
controller's TTL can delete them: their objects, their Pods and every container
log are scanned for the fixture credentials, and a completed operation Job's
Job, Pod, settled log and result are kept in memory for the proofs that read
its history later. Results are read with `resultframe`, which wraps the
production parser. The phase keeps three ledgers in memory -- the Jobs it
observed, and the Jobs it audited broadly and fully -- and the fault scenarios
add to the same ledgers, so the closing audit holds every Job the phase saw,
the fault Jobs included.
The filters that were files under `testdata/e2e` are Go predicates in
`dataplane_filters.go`, each held by a unit test to the readings it accepts and
the mistakes it refuses, and so is every other predicate the phase decides a
row by. SQL runs through `kubectl exec` into the database Deployments, as it
did from the shell.

`TestMigrationsPostgreSQL` and `TestMigrationsMySQL` are the migration phases,
one engine each, and each refuses an `E2E_ENGINE` other than its own. Both
start with `migration-policy`; MySQL then runs `mysql-transaction-mode`, since
the sequence it applies names a transaction mode. The lifecycle publishes the
migration directory with the Ptah the operator runs, takes a database nothing
has migrated through the approval gate, the run and the history it leaves, and
then runs every row that holds the path to a refusal or a fault, in the
namespace the data plane prepared:

- `migrations_lifecycle_e2e_test.go`: the main lifecycle, the approval stamp,
  and the refusal of an approval that names a consumed plan.
- `migrations_realm_e2e_test.go`: the realm, the partial run, the older
  artifact, the modified file and the branch applied out of order.
- `migrations_guard_e2e_test.go`: the apply-policy guard, adoption of an
  existing schema and the checkpoint bootstrap.
- `migrations_uncertain_e2e_test.go`: the uncertain Apply, the late dispatch
  and the restored history.
- `migrations_faults_e2e_test.go`: deletion, a stopped Apply, a lost log, the
  retry interval, suspension and the lock-release fault.
- `migrations_isolation_e2e_test.go`: the isolated node, the unknown layer,
  egress, the retarget before dispatch, the rebuild drill and the transaction
  mode.

The phase stops at its first failure, as the script did, and a cleanup
registered before it creates anything puts back the isolation rules, the
egress policies and the apply gate however it ends. The isolated-node row cuts
the isolation worker's node container off from the API server, which is why
the phase reads the Docker context and the kind cluster's name, and why only a
suite that declares the worker may run it. The Job and status filters are Go
predicates beside the rows that use them, each held by a unit test to the
readings it accepts and the mistakes it refuses.
