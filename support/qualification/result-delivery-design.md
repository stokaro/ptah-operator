# Durable runner result delivery

Implementation design for [#586](https://github.com/stokaro/ptah-operator/issues/586).
The target is stable 0.2.0. This document does not qualify the transport or
authorize a release. Installed Jobs still use log frames. The storage, TLS
delivery, live authorization, credential issuer, runner command, and optional
workload projection are implemented. The manager does not select or issue these
credentials yet. Its listener, chart, admission, and result consumption remain
unconnected.

## Storage boundary

`internal/resultstore` implements the storage portion. A receiver must validate
the protocol and authenticate the operation before calling it. The package
cannot establish those properties by hashing a request or checking its fields.

Use immutable Secrets in the resource's namespace for the upload intent,
payload chunks, and completion record. ConfigMaps would expose plan content to
identities that only need ordinary configuration. Secrets are access controlled,
not inherently encrypted at rest: the cluster's existing encryption and backup
policy still applies. A principal with Secret read access in the namespace can
read these bytes. This access expansion must be covered by the security review
before the receiver is enabled; no chart permissions change in this step.

The record binds namespace, resource kind/name/UID/generation, execution binding,
input fingerprint, operation type/ID, attempt Job name/UID, and Pod name/UID.
Its deterministic name fixes namespace, resource UID, operation ID, and reserved
Job name. A replacement Job or Pod cannot start a second publication under that
attempt name. The receiver, not the submitted body, supplies this binding from
authenticated authority and direct API reads.

The publication order is:

1. Create an immutable intent containing the binding, complete byte length and
   SHA-256, and each chunk's length and SHA-256. Existing content must match.
   This reserves the digest even if the first request stops before any chunks.
2. Create immutable chunks, each at most 512 KiB, and read them directly from
   the API server. Each chunk is owned by the exact intent UID.
3. Create an immutable completion record containing the intent UID, the hash of
   its exact manifest bytes, and the UIDs of all chunks.
4. Read back the intent, completion, and every chunk. Verify all identities,
   lengths, individual hashes, and the complete hash before returning a receipt.

An upload may occupy up to the existing result payload limit, including JSON
escaping overhead; the 8 MiB executable-plan limit is unchanged. Reads refuse
empty, oversized, missing, terminating, mutable, replaced, or corrupted records.
No partial bytes escape a failed read. The store never updates an object and
never repairs a missing chunk under an existing completion record.

A lost API write response returns an error. The next identical publication reads
what was actually persisted and continues. A lost HTTP acknowledgment will use
the same path and return the same completion UID and digest. Concurrent identical
uploads converge; concurrent different uploads cannot combine chunks or replace
the reserved digest. The store has no process key or in-memory receipt index.

The intent belongs to the schema or migration, not its Job or Pod. Chunks and
completion belong to the intent. Deleting a Job must not garbage-collect the
result. Resource finalization must preserve unresolved evidence until the
existing recovery rules allow deletion. A storage test without Kubernetes
controllers does not prove that lifecycle.

## TLS delivery component

`internal/resultdelivery` implements a dedicated HTTP receiver and a sender
that accepts already executed result bytes. The receiver requires TLS 1.3 and
a verified client certificate. The certificate's single URI SAN encodes the
complete publication binding and target engine. The route must name that
binding's deterministic publication name; the result must name its operation
and operation ID. Certificates are checked again on every request and before
publication, including their validity on reused TLS connections.

The receiver requires an explicit live-authorizer callback. No callback that
permits arbitrary requests is supplied by production code. The Kubernetes
implementation is `internal/resultauthority.Authorizer.Check`, which requires
an uncached API reader. The TLS tests use controlled authorization callbacks;
the separate authorizer tests check the Kubernetes predicates with a fake client.
A definitive authority refusal is HTTP 403. A temporary API failure is HTTP 503,
so a runner may retry delivery without mistaking an outage for revoked authority.

The receiver checks authority before reading the body, after validating it, and
through `resultstore.PublishAuthorized` after chunk writes, just before the
completion write or return of a previously committed receipt. A concurrent
epoch change still requires admission and consumer-side validation; these API
operations are not a cross-object transaction.

Requests have a required length and SHA-256 header. Unknown-length, oversized,
noncanonical JSON, mismatched operation, foreign-engine plan, invalid protocol,
or unreadable process-sealed plan payloads are refused before publication.
The wire media type is `application/vnd.ptah.result.v1+json`; this endpoint never
parses mixed diagnostic logs. The existing result validator is shared with the
log protocol rather than copied. Plaintext plans are decoded and hash-checked
under the unchanged 8 MiB limit. Controller policy checks remain necessary.

The configured upload limit is enforced before reading bodies or performing
live API checks. Saturation returns 503 immediately. A request context bounds
API operations; a socket/stream read deadline also terminates a stalled body.
Rejected bodies retain a read deadline and close HTTP/1 connections, so the
HTTP server cannot indefinitely drain them after releasing an upload slot.
Installation sizing must account for concurrent decoded and encoded payload
buffers; the package's configurable bound is not a resource-budget qualification.

The sender requires server certificate verification and a client certificate
whose identity matches the supplied binding. It refuses redirects and validates
the returned receipt name, UID, length, and digest. Network failures and explicit
transient statuses retry the same copied bytes within bounded attempts and an
overall deadline. Definitive refusals stop delivery. The sender has no SQL or
executor callback. The runner invokes it only after its one execution returns.

The local TLS tests cover a lost acknowledgment after persistence, a new receiver
reading the existing store, conflicting retransmission, invalid client/server
trust, authority retirement at each check, a stalled body, saturation, redirects,
and an exact 8 MiB escaping-heavy plan. They use the real Secret store over a fake
API client; the separate envtest suite covers the real API's storage behavior.
Neither is a complete Job-to-controller acceptance run.

## Live result authority

`internal/resultauthority` holds a certificate identity to the current schema or
migration UID, generation, active operation, input fingerprint, and execution
epoch. It verifies the exact recorded Job UID through `jobclaim.Match`, then
holds the Pod UID, owner, generated name, metadata, and workload to the persisted
admission snapshot. Controllers and delivery share `podintent.ValidateStoredPod`.
The Job must still have the one-shot admission envelope and no cleanup TTL.
A bounded Pod list must identify only the original Pod; a second Pod, incomplete
list, or empty list refuses publication. The subject is read again after these
checks, and a changed claim refuses publication.

A missing recorded Job UID is temporary: the runner may finish before the
controller persists adoption. API outages are also retryable; missing objects,
changed authority, and lost Lease continuity are definitive refusals. Mutating
claims must have persisted `DispatchStarted`; read-only claims do not set it.
An expired execution deadline or a terminating original Pod does not itself
refuse an outcome that the runner has already produced. This grants permission
to deliver evidence, never permission to execute SQL.

The authorizer does not issue credentials, install admission, or establish a
transaction across its reads and the completion write. The issuer reads
the original generation from the Job template, covered by its admission
snapshot. The authorizer refuses a certificate whose generation differs from
that value even if the live resource now has that generation. Consumers still decide whether evidence is
current and whether the SQL process has stopped under the existing Lease and
unknown-outcome rules. Tests cover all nine operation types using the golden
Jobs that workload tests hold to the production builders, plus authority changes,
API outages, retirement during reads, and Pod replacement. These are local
predicate tests, not installed certificate or cluster acceptance evidence.

## Runner delivery path

The runner accepts `--result-endpoint` and `--result-credentials` together. The
credentials directory contains `tls.crt`, `tls.key`, and `ca.crt`; each file is
bounded to 64 KiB. The command verifies the key pair, client certificate lifetime
and usage, receiver configuration, and the certificate's operation, operation
ID, target engine, and actual Pod namespace/name/UID before starting a child.
The Pod identity must come from downward API values `PTAH_RESULT_POD_NAMESPACE`,
`PTAH_RESULT_POD_NAME`, and `PTAH_RESULT_POD_UID`. The original resource
generation is the literal `PTAH_RESULT_GENERATION` in the Job template. Duplicate binding variables
are refused. These are delivery credentials, not Kubernetes API credentials.
The receiver remains responsible for authenticating the issuer and current
claim. Config validation does not promise the receiver is reachable.

After execution, the runner encodes one immutable result, writes a bounded
termination summary, and sends the payload. The summary's existing
`frameDigest` field names the canonical payload SHA-256, which is also the
receipt digest; a summary never claims durable acceptance. Delivery permits
four attempts, each bounded to 30 seconds, within a two-minute total deadline.
It has its own context so execution cancellation can still be reported. The
kubelet's termination grace can kill the process before delivery completes;
unknown-outcome recovery remains necessary.

Durable Plan execution preserves the validated raw plan bytes and their digest
without requiring an ephemeral manager key. The limit is still exactly 8 MiB.
The new command path never writes the payload to stdout and never falls back to
log framing on failure. Jobs without delivery flags still use the old sealed
log protocol until the issuer, workload, and consumer integration is ready.
Removing that installation dependency remains required for #586.

Local command tests run a real child process that returns a native migration
Apply report, persist its result through TLS and the Secret store, then lose the
first acknowledgment. The runner redelivers identical bytes and starts the child
only once, including when the execution context has been canceled after the
first persistence. The API client in this test is fake; it does not prove a real
database commit, installed admission, or a Job lifecycle. Plan tests exercise
exact-limit and maximum-plus-one output without a process key.

## Credential issuance and Job projection

`workload.Builder.ResultEndpoint` selects the durable runner arguments and
credential projection for every schema and migration operation. It remains
empty in the manager until installation wiring is ready. The common
`jobconfig` package fixes the Secret name from resource UID, operation ID, and
Job name, the original resource generation, the actual-Pod downward API fields,
and a read-only `0440` Secret mount in the main runner container. Admission
snapshots cover this complete template. Readback refuses credential aliases,
mounts in init or ephemeral containers, credential environment references,
image-pull use, and automatic Kubernetes API token mounting.

`internal/resultcredentials` uses a dedicated client CA and an uncached reader.
It validates live authority before generating a key, again before creation, and
after direct Secret readback. It only creates immutable TLS Secrets and never
updates them. Concurrent issuers converge on the persisted winner; a lost API
response is retried by reading that winner. The certificate carries the exact
identity in one URI SAN and permits client authentication only. Its lifetime
covers the supported Job horizon plus ten minutes for grace and reporting, and
must fit within the signer's remaining lifetime.

The credential Secret belongs to the schema or migration, not the Job or Pod.
It fixes the original Pod identity even after that Pod disappears: a replacement
Pod cannot overwrite or reuse the same attempt's credential. A Job from an older
generation cannot be reissued under the new generation. The configured client
trust pool may include the previous signer; an existing credential is preserved
during that overlap. This is not proof of installed CA rotation. Server trust
is immutable in the credential: installation rotation must preserve old runners'
trust until their bounded attempts finish, rather than rewriting their Secrets.

Before enabling issuance, admission must protect credential creation, metadata,
and deletion, and prevent unrelated Pods from projecting these Secrets. Deleting
an active credential and allowing reminting would erase its original-Pod pin;
resource ownership and `immutable: true` alone do not prevent that. Restore and
retention must preserve active pins. Issuance and authority reads are not a
cross-object transaction, so consumer and receiver checks remain required.
Component tests use production Job fixtures and a fake API. They do not prove
kubelet projection, garbage collection, installed authorization, or backup.

## Installation and runner integration still required

The receiver runs independently of family reconcile workers, behind its own TLS
Service. It must authenticate clients before reading a large body, bound active
uploads and body-read time, reject saturation without queuing unbounded bodies,
and apply a storage deadline. Manager HTTP logs and rejection messages must not
include payloads or credentials.

The manager must invoke the issuer after exact Job adoption and Pod admission,
and installation admission must enforce the projection and active credential
pin before enabling that path. A replacement Pod must never inherit authority
to submit as its predecessor.

The receiver must revalidate the execution epoch, exact live Job/Pod, and current
claim through direct reads. Certificate expiration alone is not revocation.
Before committing a new completion, it must recheck authority after the upload.
Controllers must retain their own epoch and provenance checks when consuming a
receipt: persistence is evidence, not permission to apply. Admission must bind
receiver writes to the same publication intent and protect the metadata as well
as the immutable payload. Deleting/recreating a Secret is not prevented by
`immutable: true`.

Workload builders must supply the receiver configuration, projected credentials,
and downward API identity to the implemented runner path. Controllers must use
durable results without a correctness fallback to `pods/log`.

The new TLS payload must carry usable plan bytes, not an old per-process sealed
Plan. Storing those bytes in access-controlled Secrets removes the dependency on
the receiving process's private key. The old log seal may be removed only as the
new protocol stops emitting plan content to logs. Receiver CA/server/client key
rotation, overlap, restore, and HA behavior remain separate requirements; none
may make an acknowledged payload unreadable.

Controllers must consume durable receipts without requiring the producing Pod
to remain present. If no receipt exists after a mutating runner disappears,
existing unknown-outcome recovery, database inspection, and fresh authorization
still apply. SQL and publication are not an atomic transaction.

## Retention and acceptance

Before enabling this path, implement guarded Secret creation and deletion,
receiver certificates and NetworkPolicy, bounded request metrics, and receipt
consumption and retention. Do not attach a time-only TTL to unconsumed results or
unresolved operations. Abandoned partial publications become eligible only after
the exact attempt is retired and cannot still deliver. Cleanup must use UID/RV
preconditions and respect the backup/recovery window and pinned plan evidence.
Backups must include intents, chunks, completions, consumption state, and the
certificate authority needed for outstanding delivery credentials. Restore must
not accept an incomplete publication or reactivate retired delivery authority.

The current tests prove component publication integrity, TLS identity checking,
bounded redelivery, and Kubernetes Secret persistence/immutability. They do not
prove live claim authorization, credential issuance, installed RBAC/admission,
garbage collection, manager failover, Lease independence, or the complete
Job-to-controller workflow at the supported plan limit. Those remain the
explicit #586 acceptance rows.
Keep the 64 MiB workaround until the complete workflow passes with the default
10 MiB kubelet configuration, deliberate log removal, and manager replacement.
