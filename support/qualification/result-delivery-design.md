# Durable runner result delivery

Implementation design for [#586](https://github.com/stokaro/ptah-operator/issues/586).
The target is stable 0.2.0. This document does not qualify the transport or
authorize a release. Default installations still use log frames. The storage,
TLS delivery, live authorization, credential issuer, runner command, and workload
projection are implemented. The chart can enable the listener, issuer, admission
validator, background consumer, and independent certificate rotation through
`resultDelivery.enabled`. That development path is disabled by default until
retention, restore, and installed acceptance are complete. It provisions trust
objects and a Service, grants scoped permissions, and can render a receiver
NetworkPolicy with explicit infrastructure peers.

## Storage boundary

`internal/resultstore` implements the storage portion. A receiver must validate
the protocol and authenticate the operation before calling it. The package
cannot establish those properties by hashing a request or checking its fields.

Use immutable `PtahResultRecord` objects in the resource's namespace for the
upload intent, payload chunks, and completion record. This dedicated RBAC
resource lets the receiver read results without permission to read database
Secrets. Ordinary ConfigMap readers must not gain access to plans. Record read
access exposes confidential operation data and, for the credential role, private
delivery keys: encryption at rest and backups must explicitly cover this CRD.

The schema requires the role and bounded payload, freezes the whole spec, and
has no default or status. The reader independently checks decoded byte limits,
metadata, owner UIDs, and publication digests. Local API-server tests publish and
read a multi-chunk result using only `get` and `create` on this resource while
Secret GET remains forbidden. The chart routes all record writes through the
controller-write webhook. With delivery trust configured, that guard admits
credential and publication records under the rules below. Installed manager RBAC
uses GET/CREATE for delivery and LIST/DELETE for cleanup when durable delivery
is enabled. No record watch or update grant is added.

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

## Publication admission

Only the configured manager may create result records. For every intent, chunk,
and completion, admission reads the canonical credential, verifies its trusted
certificate and exact publication binding, and checks current Job/Pod and
operation authority. A missing issuer or credential refuses publication.

Intent bytes must encode a canonical versioned manifest with the complete size,
digest, bounded chunk geometry, exact name, and resource owner. A chunk must
belong to the persisted intent UID and match its declared index, size, and hash.
Before admitting completion, the webhook reads all chunks directly, validates
their UIDs and full-payload digest, and validates the runner protocol against the
credential's operation and engine. The store's ordinary reader uses the same
canonical manifest and metadata rules, including after restore.

Admission freezes metadata as well as bytes. Identical CREATE retries remain
admissible so the API can return AlreadyExists, including concurrent completion
writes. A missing member of an already completed publication cannot be recreated.
These checks do not make the live authority reads and API writes transactional;
receiver and consumer checks remain necessary.

Publication DELETE is admitted only after the persisted retirement window and
the live pin, source identity, and Job-absence checks described below.
DeleteCollection remains refused. Retiring SQL authority alone cannot establish
that its result was consumed or backed up.

Local API-server tests publish through the chart's actual admission handlers,
read the same result back, refuse unissued and retired authority and invalid
protocol bytes, and resume a lost API response after intent, chunk, or completion.
They also verify metadata protection, deletion refusal after retirement, and the
removal/restoration of the deletion guard. These use a Resolve refusal payload,
not an installed database workflow or maximum-size plan. Admission readers in
this fixture still use administrator access. Unit tests separately hold chunk
geometry, corruption, changed UIDs, forbidden repair, and concurrent identical
publication to both accepted and refused inputs.

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

Payload PUT requests have a required length and SHA-256 header. Unknown-length, oversized,
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
executor callback. The runner invokes payload delivery only after its one execution returns.
Before execution, its read-only HEAD preflight uses the same TLS identity, route,
live-authority check, concurrency bound, and request deadline. A successful
preflight returns 204 without writing any record; it is not a durable receipt
or permission to execute SQL.

The local TLS tests cover a lost acknowledgment after persistence, a new receiver
reading the existing store, conflicting retransmission, invalid client/server
trust, authority retirement at each check, a stalled body, saturation, redirects,
and an exact 8 MiB escaping-heavy plan. They use the result-record store over a fake
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
claim. After local validation, the runner makes one authenticated HEAD request,
bounded to 30 seconds, before starting its child. Invalid trust, revoked
authority, or an unavailable receiver stops the command without SQL or a log
fallback. Existing execution guards still apply after this preflight. A receiver
can fail after the check, so result delivery and unknown-outcome recovery remain
necessary.

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
Apply report, persist its result through TLS and the result-record store, then lose the
first acknowledgment. The runner redelivers identical bytes and starts the child
only once, including when the execution context has been canceled after the
first persistence. The API client in this test is fake; it does not prove a real
database commit, installed admission, or a Job lifecycle. Plan tests exercise
exact-limit and maximum-plus-one output without a process key.

## Credential issuance and Job projection

`workload.Builder.ResultEndpoint` selects the durable runner arguments and
credential projection for every schema and migration operation. It remains
empty unless the manager receives `--result-endpoint`. The common
`jobconfig` package fixes the Secret name from resource UID, operation ID, and
Job name, the original resource generation, the actual-Pod downward API fields,
and a read-only `0440` Secret mount in the main runner container. Admission
snapshots cover this complete template. Readback refuses credential aliases,
mounts in init or ephemeral containers, credential environment references,
image-pull use, and automatic Kubernetes API token mounting.

`internal/resultcredentials` uses a dedicated client CA and an uncached reader.
It validates live authority before generating a key, before creating a canonical
credential-role `PtahResultRecord`, and after direct record readback. The record
holds the private key, certificate, trust bundle, and original-Pod binding.
Concurrent issuers converge on the persisted winner; a lost record-write response
is retried by reading that winner. The certificate carries the exact identity in
one URI SAN and permits client authentication only. Its lifetime covers the
supported Job horizon plus ten minutes for grace and reporting, and must fit
within the signer's remaining lifetime.

The issuer then creates an immutable TLS Secret from the exact recorded bytes.
It never reads, patches, or updates Secrets. Its returned UID identifies the
canonical record, not the Secret. A lost projection-write response preserves the
same canonical key on retry. `AlreadyExists` does not prove the projection's
contents: the mandatory runner preflight authenticates the mounted credential
before SQL starts. A preexisting unusable projection can prevent progress, but
must not let the child start with unverified delivery credentials.

The credential record belongs to the schema or migration. A new Secret
projection belongs to that exact record by UID, with `blockOwnerDeletion: false`.
Neither is owned by the Job or Pod. Removing an eligible record lets Kubernetes
collect its projection without giving the manager Secret GET or DELETE.
Existing development projections owned directly by the resource remain readable;
new CREATEs must use the canonical record owner. The canonical record fixes the original Pod identity even after that Pod disappears: a replacement
Pod cannot overwrite or reuse the same attempt's credential. A Job from an older
generation cannot be reissued under the new generation. The configured client
trust pool may include the previous signer; an existing credential is preserved
during that overlap. This is not proof of installed CA rotation. Server trust
is immutable in the credential: installation rotation must preserve old runners'
trust until their bounded attempts finish, rather than rewriting their Secrets.

The chart routes reserved credential Secret names to the controller-write
webhook for CREATE, UPDATE, and DELETE, regardless of the writer's identity.
CREATE requires the configured manager, trusted certificate bytes, exact public
binding metadata, and current operation authority. Secret creation also requires
its bytes and binding to match the canonical record. Credential-record creation
authenticates the same certificate and live binding; record UPDATE freezes its
spec and metadata, and record DELETE preserves execution and recovery pins. Without an issuer configured
in the manager, creation is refused. UPDATE preserves data, ownership, labels,
annotations, and finalizers, except removal of the API's sole `foregroundDeletion`
finalizer from an already-deleting object. Orphan deletion is refused because it
would require rewriting immutable owner references. DELETE reads the owner directly and refuses while
that exact operation ID remains active, including after Pod loss, generation
changes, Lease loss, or the start of resource deletion. Pending schema observation,
pending Lease release, migration unresolved/resolved-run evidence, and the most
recent migration run also retain their attempt. An unresolved-run annotation
keeps that protection when restore omits status; an unreadable annotation refuses
cleanup. The enabled collector also requires the persisted retirement window
and original Job absence before credential-record deletion. A projection whose exact
canonical record still exists cannot be deleted, even after retirement. After
record removal or replacement, projection DELETE decodes the immutable
certificate's recorded binding and rechecks the original resource directly.
Expiry and CA retirement do not prevent this cleanup, but never restore delivery
authority. API failures refuse deletion. Legacy resource-owned projections keep
the original active-operation deletion guard. Resource ownership and
`immutable: true` alone do not preserve this first-Pod pin.

The collector applies this deletion boundary after the persisted retention
window. Its installed garbage-collection and recovery proofs remain required
before default activation.

The Pod webhook also receives direct references to reserved credential names,
including unlabeled Pods, environment sources, image-pull credentials, projected
volumes, inline CSI, and legacy storage sources. It refuses them outside the
exact admitted operation workload. The first Pod can precede its credential record;
later CREATEs cannot reuse an existing credential. An UPDATE verifies the
original Pod identity through a metadata-only GET on `PtahResultRecord`, never
on Secrets. The runner independently checks its downward-API UID against the
certificate and authenticates delivery before executing anything, including when
a second Pod races the first credential publication.

Local API-server tests use the actual issuer, chart routing, and admission
handlers. An impersonated issuer identity has record GET/CREATE and Secret
CREATE, while GET on both the credential projection and a database Secret is
forbidden. Issuance and repeated issuance succeed under those permissions. The
tests also prove canonical-record and projection metadata protection, active
DELETE and DeleteCollection refusal, original-Pod updates, replacement refusal,
and retirement cleanup. Removing the webhook admits its otherwise-refused
record deletion; restoring it restores the refusal.

These tests run no kubelet or garbage collector. The admission handlers still
use an administrator as their API reader, so these tests do not prove installed
webhook RBAC, kubelet projection, garbage collection, or backup. Restore and
retention must preserve active pins. Issuance and authority reads are not a
cross-object transaction, so consumer and receiver checks remain required.

## Controller consumption

Both controllers select durable consumption from the Job's explicit delivery
arguments. A malformed projection is refused; a Job requesting durable delivery
never falls back to logs or a termination summary. Jobs without those arguments
retain their legacy path until installation activation.

The consumer locates the intent by namespace, resource UID, operation ID, and
Job name, verifies the complete publication, and holds its full binding to the
persisted claim: resource kind/name/UID/generation, execution epoch, input
fingerprint, operation, and exact Job UID. It decodes the protocol for the
claim's engine. Reading requires only result-record access, not live credentials,
Pods, logs, or the receiving manager's process key.

`internal/resultconsumer.Reader` performs API reads and payload validation in
bounded background workers. Reconcile polls memory and requeues while a load is
pending or the reader is saturated. Worker count, retained entries, read deadline,
and memory retention require explicit configuration; expiration removes memory
only. A failed status write can reload the same durable evidence. Shutdown
cancels loads. The loader must honor its context. The manager starts this reader
with the receiver when durable delivery is explicitly configured. Installed
resource sizing for these limits remains unqualified.

The existing Job-intent, current-input, execution-binding, Lease, and terminal-Job
checks still precede consumption. A live replacement or additional Pod remains a
refusal. When the original Pod is absent, the publication supplies its historical
UID, which is not treated as live termination evidence or proof of SQL quiescence.
A missing or invalid receipt follows existing retry and unknown-outcome recovery.
Controllers still require the Job; a missing mutating Job retains its existing
unknown-outcome handling. The storage loader's ability to read without a Job does
not change that controller decision.

Durable Plan bytes enter the existing digest, engine, target, policy, exclusion,
and planstore validation directly. Legacy Plan frames still require their sealed
envelope. Unit tests cover both controllers' durable harvest paths for all nine
operations after Pod deletion, no log/summary fallback, replacement-Pod refusal,
exact-limit Plan loading, bounded background work, and plan publication after
manager key loss. The API-server storage test also locates a maximum-size stored
payload without knowing the Pod identity in advance. These are component and
controller-path tests; they do not run an installed manager, kubelet, or database
workflow and do not complete the default-logging acceptance matrix.

## Manager runtime

`--result-endpoint`, `--result-cert-dir`, and `--result-enrollment-policy` must
be supplied together. The first
also selects durable arguments and credential projection in every Job builder.
`--result-bind-address` defaults to `:9444`. Without the endpoint, existing
installations continue to use logs.

`internal/resultservice` loads six bounded files from the mounted directory:
`tls.crt` and `tls.key` for the server, `ca.crt` for the server trust given to
runners, `client-ca.crt` and `client-ca.key` for the dedicated client signer,
and `client-trust.crt` for accepted client signers. Startup validates the server
chain and endpoint hostname, the signer, and its trust. It generates no process
CA and reads no Kubernetes Secret. All manager replicas must mount compatible
trust. The service polls the mount every five seconds. For a Kubernetes Secret
projection it resolves `..data` once and reads only that generation. Plain
externally provisioned directories must be replaced atomically, never edited in
place. Each bounded candidate is fully validated before TLS and issuer switch
through one immutable snapshot. Invalid material preserves the last validated
snapshot, fails readiness, and pauses issuance until a valid projection returns.
The background consumer remains independent of certificate state.

Reconciliation and admission retain a stable service reference and select its
current issuer per call. New handshakes use the current serving certificate and
client roots. Every receiver authority check also verifies the peer against
current client roots, including after upload and at the publication boundary;
a keep-alive connection cannot preserve trust in a removed CA.

Existing canonical credentials retain their original server bundle. Expanding
the bundle for new Jobs does not rewrite or invalidate those records. Admission
requires the current bundle for a new credential record and exact canonical bytes
for every Secret projection. Client CA overlap permits existing credentials;
removing their signer refuses further delivery but does not affect reading
already acknowledged records.

Automatic provisioning and coordinated rotation are implemented in the rotator
loop described below and wired by the development chart option. The
rotator must establish trust on every serving replica before selecting a new
signer. Server CA rotation must first give new runners the expanded bundle and
keep serving a certificate old runners trust until their bounded attempts retire.
The runtime reload mechanism does not establish that overlap or authorize early
root removal.

The service runs independently of leader election and family reconcile workers.
It starts the receiver and background reader together, cancels their API work
and closes TLS connections on shutdown, and reports not ready before startup,
after shutdown, on an invalid trust update, when its enrollment snapshot is stale
or the public policy is unavailable, or when its serving chain or client signer
expires.
Listener or reader termination stops the service. HTTP diagnostics cannot emit
client identities or payloads. Manager configuration currently bounds uploads
to one with a two-minute deadline, and background reads to one worker with four
retained entries, a 30-second deadline, and one-minute memory retention. These
bounds still need installed resource and contention qualification.

Both controllers invoke the issuer after exact Job adoption and Lease renewal.
They list at most two matching Pods, require the single admitted original Pod,
and bind the credential to the operation, original generation, and actual UID.
Issuance has a five-second deadline. A pending Pod or issuance error returns a
short retry, preserving the existing Lease path; Events contain only a generic
failure. A terminal Job goes directly to result consumption without requiring
credential reissuance. The same service selects the current issuer for credential and publication
writes in admission. Configured durable Jobs cannot fall back to logs.

Local tests exercise the assembled service over real mTLS with a fake API client:
issue a credential, authenticate preflight, persist a result, stop the service,
start another from the same trust, and read the same receipt after Job and Pod
removal. The reader refuses Secret GET. Rotation tests expand server trust,
change the client signer with overlap, change the serving certificate, and remove
old client trust. New handshakes succeed with the expanded bundle; a preexisting
keep-alive connection under the removed client CA receives HTTP 403. Admission
and issuance also refuse that retired credential, while its acknowledged result
remains readable. These tests deliberately trigger root removal to measure the
refusal; they do not prove a safe production overlap schedule. Invalid projected
material fails readiness and recovers automatically after replacement. Separate
controller-helper tests hold
all nine operation bindings, missing or ambiguous Pods, changed workload and
generation, invalid credential receipts, and cancellation to their expected
outcomes. They do not prove installed reconciliation, kubelet Secret projection,
API admission, leader election, or a database workflow. The controllers still
retain their existing missing-Job and unknown-outcome behavior.

## Enrollment fence for coordinated rotation

The manager's `--result-enrollment-policy` names a public ConfigMap in its
ServiceAccount namespace. The ConfigMap has exactly `version: "1"`, `clientCA`
(the SHA-256 of the selected client CA's DER certificate), and `serverTrust`
(the SHA-256 of the exact PEM bundle issued to new runners). It contains no keys.
The installation rotator owns writes; the manager needs only GET on this exact
ConfigMap. A missing, terminating, malformed, foreign, or unreadable policy
refuses new issuance. The enabled chart precreates this object and grants the
rotator GET/UPDATE on its exact name. The manager retains its existing ConfigMap
read grant and cannot update the policy.

An issuer reads the policy directly before generating a credential and again
before its canonical-record CREATE. Admission separately requires the current
local signer, rather than any signer in overlap trust, and reads the policy
immediately before accepting the record. A replica still holding old mounted
material cannot keep enrolling under it after the policy changes. The service
also checks the policy in its background refresh, with a five-second deadline,
and fails readiness while the mounted snapshot differs. Readiness is advisory;
each enrollment's direct read is the authority check.

This fence applies to new canonical records. Recreating an existing canonical
credential's Secret projection, validating its publication, and reading a saved
result do not require the current enrollment policy to match that credential.
They retain the original binding, overlap-trust checks, and live operation
authority. Changing the enrollment policy neither revokes a still-trusted
credential nor extends its lifetime.

The maximum client certificate lifetime is declared once as
`resultcredentials.MaxCredentialLifetime` (24 hours and 12 minutes). A
rotation transition first persists and reads back the new enrollment policy.
Only then may it persist the start of the retirement wait; a timestamp taken
before a delayed policy write would shorten the protection window. Waiting at
least that lifetime plus the declared clock-skew allowance bounds every old
credential that could have passed admission before the fence changed. Lost
acknowledgments may extend this wait, never shorten it. The rotator must still
coordinate trust distribution and serving-certificate changes across replicas;
the enrollment fence alone does not implement that state machine.

The result rotation state machine uses that fence in this order:

1. Persist replacement server/client authorities in the rotator's private journal.
   Publish an enrollment policy for the current signer and expanded server trust,
   then project expanded server/client trust while retaining the current serving
   certificate and signer. Start the retirement wait only after policy readback.
   This bounds credentials that still trust only the old server CA.
2. After that wait, advance the policy to the replacement client signer while
   retaining expanded server trust. Project the replacement serving certificate
   and signer with both client roots. Start a new persisted wait after readback;
   it bounds the remaining credentials signed by the old client CA.
3. After the second wait and endpoint verification, publish the replacement-only
   enrollment policy and projection. Retire the old private material only after
   exact readback and endpoint verification. The journal must resume uncertain
   writes at every step without shortening either wait or regenerating usable
   candidate authorities.

Each policy write precedes the corresponding Secret update. Stale replicas may
be temporarily unready; the rotator's own policy and Secret writes must not
require the manager webhook to be available. The loop updates only precreated
objects: the six-file projection Secret, a private journal Secret, a public
policy ConfigMap, and a dedicated Lease. It checks exact Helm ownership metadata
and pins the projection and policy UIDs in the journal. Foreign state, missing
objects, replacement UIDs, or a policy rollback after a wait started are refused.
A lost UPDATE response is resolved by direct readback; a failed readback leaves
recovery to the persisted journal. No Secret CREATE permission is required.

`ptah-cert-rotator` accepts `--result-secret-name`,
`--result-journal-secret-name`, `--result-enrollment-policy`,
`--result-service-name`, and `--result-lease-name` together. The result loop and
webhook loop have separate Leases, deadlines, retry schedules, and readiness.
They run concurrently: either can publish bootstrap material while the other
waits for the manager. Process readiness requires both loops to have a successful
endpoint verdict; advancing a journal alone does not claim readiness.

Each wait is the maximum credential lifetime plus five minutes of clock skew,
bounded by the old authority's expiration plus that skew. A serving leaf that
cannot survive the first wait is renewed under the old CA before starting the
transition. Direct Pod probes check a stable, nonempty EndpointSlice snapshot,
the exact served certificate, and mTLS under every still-valid client CA in the
current overlap. The probe sends HEAD without an operation identity and requires
HTTP 401: this proves TLS client authentication succeeded without invoking
publication. TLS handshake completion alone is insufficient.

An interrupted transition can outlive its serving certificates. The rotator
renews an expired pending leaf under the same authority and journals both leaf
versions before changing the projection. Exact readback retires the previous
leaf; neither enrollment nor an existing fence changes. This also resumes a
repair whose write response was lost or whose replacement leaf expired during
another outage.

Once both candidate authorities have expired, including the clock-skew margin,
the rotator journals their removal before returning to the current authorities.
Credentials issued under the current signer retain their server trust; no
candidate credential remains usable. The current authorities may themselves
have expired, so this checkpoint stays pending and the normal renewal path must
produce a successful endpoint verdict before claiming readiness. Subsequent
rotation starts new enrollment fences. An authority that has not expired prevents
this recovery path, even if the other authority has expired. Foreign projection
or enrollment data is still refused before either recovery starts or resumes.

Local fake-API tests cover both persisted waits, delayed policy writes, lost
write responses and failed readbacks, restart, rollback refusal, replaced object
UIDs, leaf renewal, expired stable authorities, and expired pending leaves and
authorities in every transition phase. The recovery fixture verifies certificate
chains and expiration against its clock; it does not simulate endpoint reload.
Real TLS tests cover both
client roots, a missing root, the wrong serving leaf, unavailable endpoints, and
endpoint identity changes. Command tests hold independent startup, aggregate
readiness, and joint shutdown. These do not prove kubelet projection, installed
RBAC, multi-replica rotation, or installed recovery after a long outage; those
remain installation acceptance work.

Local tests cover stale local signers, changed policy between generation and
CREATE, missing/malformed policy, API failure, cancellation, and readiness
recovery. A real API-server test changes only the ConfigMap while issuer and
webhook retain their old CA, verifies both refuse new enrollment, and verifies
an already-issued credential still publishes a result. The manager identity
can read the exact policy and cannot update it in this fixture. Restoring the
policy admits the same previously refused request. These tests establish the
fence, not installed rotation or cluster-wide least-privilege permissions.

## Development installation

`resultDelivery.enabled=true` selects durable delivery for new Jobs and requires
built-in certificate rotation with generated webhook trust. The manager mounts
only the six-file projection, read-only with mode 0440 and its existing fsGroup;
it does not mount the private journal or receive Secret-read permission. The
rotator can GET/UPDATE only the named projection, journal, policy, and Leases.
The manager receives result-record GET/LIST/CREATE/DELETE and Secret CREATE. Since RBAC cannot
scope CREATE by name, every manager Secret request reaches the fail-closed
controller-write webhook, which accepts only an exact canonical delivery
credential. Other writers still cannot create reserved delivery credentials.
The enabled manager ClusterRole has no `pods/log` permission.

The ClusterIP Service uses port 443 and targets the manager's result listener on
9444. The chart can restrict that listener with
`resultDelivery.networkPolicy.enabled=true`. This requires explicit
`infrastructurePeers` for webhook, health, and metrics traffic; the chart cannot
infer the API server's source addresses. Result ingress is limited to operation
Pods of both families and this release's certificate rotator. Existing policies
union with this policy, so an existing broad ingress grant can still open the
port. A CNI that enforces NetworkPolicy is required to enforce these rules.
`examples/networkpolicy-egress.yaml` adds the receiver destination for all nine
operations; installations must substitute their release identity and namespace.
No runner receives Kubernetes API credentials.

Local API-server verification installs the enabled chart's actual RBAC, Service,
NetworkPolicy, and precreated objects. It checks allowed and forbidden manager/rotator operations,
persists bootstrap material through the rotator identity, restarts the rotator
from its journal, and loads the persisted projection through `resultservice.New`.
A manager Secret GET remains Forbidden. No Deployment, kubelet, or CNI runs in
that test: copying the six files to disk is explicit, and an absent endpoint
refuses rotation readiness. Separate admission tests prove that the real API
server refuses arbitrary manager Secret creation while allowing canonical
credentials and ordinary administrator Secrets.

This option is not yet a qualified installation mode. Installed retention and
cleanup, backup/restore, resource bounds, HA, complete key rotation, and the full
Job-to-controller acceptance matrix remain required before default activation. HA,
key overlap, restore, and rollout must preserve acknowledged results and
outstanding deliveries.

The implemented receiver rechecks live authority after upload; consumers retain
their own epoch and provenance checks. Persistence is evidence, not permission
to apply. If no receipt exists after a mutating runner disappears, existing
unknown-outcome recovery, database inspection, and fresh authorization still
apply. SQL and publication are not an atomic transaction.

## Retention and acceptance

An immutable `retired` record fixes the attempt's full binding, the name and UID
of the credential or intent that proved it, and a minimum retention duration.
Only the manager can create it. Admission reads that source directly, checks the
complete binding, and refuses every execution or recovery pin described above.
Source expiry does not invalidate historical identity. Source replacement does.

The API server assigns the retirement record's creation time. Eligibility starts
there, not at the creation of a potentially long-running Job or publication.
The minimum window is one hour, exceeding the profile's five-minute backup lag
plus thirty-minute combined recovery objective. A longer saved or current policy
wins, and every eligibility check re-reads the live pins. This timing rule is not
proof that a backup was made or that restore meets those objectives.

Both authority checks around a delivery read the retirement fence. A restored
active claim cannot issue credentials, resume a partial publication, or obtain a
new delivery acknowledgment for an attempt with a retirement record. Existing
complete receipts remain readable: fencing delivery does not discard evidence.
Unreadable fences and API failures refuse new authority. Real API tests exercise
both credential and intent sources, an attempted backdated creation time,
immutable marker retries, restored claims, and intact receipt bytes.

The enabled manager runs automatic cleanup as a separate leader-only worker.
It scans metadata in pages of 64 with a 30-second deadline per step, retains no
payload cache, and retries failures without making the reconcile workers wait.
New publications carry an immutable attempt index; existing unindexed records
remain readable and retryable. Child discovery is bounded to 128 indexed
members and 16 pages of 128 unindexed legacy children per namespace. An
incomplete scan retains the attempt. Large legacy sets may require repeated
passes as earlier attempts are collected; this is not an unbounded LIST.

Every DELETE rechecks the retirement window, live recovery pins, original Job
absence, source identity, and child ownership through direct API reads. The
webhook independently applies the same policy to every caller. Each late record
also receives a full window from its own API creation time. A longer saved or
current window wins. Deletion uses UID and resource-version preconditions and
removes completion, chunks, intent, credential, and finally the retirement
fence. The original Job UID must be absent before any deletion, so restoring an
old claim cannot reactivate its original Pod after collection. Orphan deletion
is refused. The Kubernetes garbage collector removes the credential's Secret
projection; the manager receives neither Secret GET nor Secret DELETE.

`ptah_operator_result_cleanup_operations_total` reports scan, retirement, and
deletion outcomes with only bounded action/outcome labels. Local tests restart
collection after failures before and after every publication deletion, refuse
partial scans and API errors, retain late writes and restored claims, and
collect legacy children whose intent was lost. A real API-server test runs the
collector with manager permissions and admission, advancing only its test clock
to exercise the one-hour window. Envtest has no garbage collector; it cannot
establish installed Secret collection or quota recovery.

[Installed retention evidence](evidence/result-retention-2026-10-02/summary.json)
records the real one-hour window on Kubernetes 1.37 with PostgreSQL and both
resource families. A frozen cohort covered all nine operation kinds. After
manager replacement, 75 eligible records and 15 Secret projections were removed;
the latest migration run retained its four records and projection, and four
published plan objects retained their UIDs and spec hashes. Successful API
DELETE events carry each original UID as a precondition and start no earlier
than that member's independently recomputed deadline. The Secret deletions
belong to Kubernetes' garbage-collector ServiceAccount. Actual authorization
reviews still deny the manager Secret GET/DELETE and `pods/log` GET.

The same installation exhausted result-record quota after a Resolve intent was
persisted. Audit records the first chunk's quota refusal. Restoring capacity
allowed the original Job to complete the same intent UID and payload digest.
This is read-only publication recovery, not proof of SQL replay prevention
after a lost acknowledgment. The retained audit and cohort can be checked with
`support/qualification/probes/result_retention_evidence.py`; refusal tests cover
early deletions, replaced identities, changed pins, and incorrect GC actors.

Before enabling this path by default, finish the installed lifecycle matrix,
including abandoned partial-publication cleanup, backup/restore, receiver trust
rotation and NetworkPolicy, bounded request metrics, and the remaining receipt
consumption failures. Do not attach a time-only TTL to unconsumed results or
unresolved operations. Abandoned partial publications become eligible only after
the exact attempt is retired and cannot still deliver. Cleanup must use UID/RV
preconditions and respect the backup/recovery window and pinned plan evidence.
Backups must include intents, chunks, completions, consumption state, and the
certificate authority needed for outstanding delivery credentials. Restore must
not accept an incomplete publication or reactivate retired delivery authority.

The component checks cover publication integrity, TLS identity, bounded
redelivery, credential issuance without Secret reads, authenticated preflight,
and admission through a real API server.

[Installed PostgreSQL evidence](evidence/result-installed-2026-10-02.json)
records a four-node Kubernetes 1.37 run with every kubelet reporting `10Mi`
container logs. A native plan of exactly 8 MiB reached approval, one Apply,
and `InSync`; database inspection confirmed its generated default. The run used
operator images from `8ea0a44f` and generated admission policies from `d924b70e`.
It exposed and fixed the Job guard's missing durable credential volume.

After consumption and convergence, the run removed all seven operation Jobs
and their Pods, restarted both managers, and independently verified unchanged
receipt UIDs, chunk sizes, and digests. The plan still reconstructed from its
16 chunks. That run did not remove logs before first consumption.

[First-harvest evidence](evidence/result-first-harvest-2026-10-02.json) uses a
fresh installation built entirely from `7ec19b64`. With every kubelet still
reporting `10Mi`, an exact 8 MiB native PostgreSQL Plan was acknowledged while
both fresh managers lacked leadership permission. The active claim remained
unchanged and no plan had been published. The probe then removed the producing
Pod and its logs, replaced both manager processes again, and restored leadership.
The first harvest reconstructed the same bytes into 16 planstore chunks. An
ordinary approval led to one Apply and database-verified convergence. The
9,787,143-byte result occupied 19 result chunks. The completed Job remained.

The [repeatable probe](probes/result-delivery.md) records the distinct manager
UID sets and checks the full publication before removal. Its verifier refuses
missing, replaced, foreign, and corrupted members. This establishes first
consumption after Pod/log loss and manager restart for the recorded PostgreSQL
and Kubernetes versions. It does not establish first consumption after Job
deletion or redelivery after a lost HTTP acknowledgment.

[Lost-acknowledgment evidence](evidence/result-lost-ack-2026-10-02/summary.json)
uses the packaged installation built from `ded02b25`, PostgreSQL migrations,
Kubernetes 1.37, two manager replicas, and default `10Mi` kubelet logs. An
isolated mTLS proxy forwarded the original Apply Pod's credential to the real
receiver, read its successful durable receipt, and closed the runner connection
without sending an HTTP response. The retry returned identical receipt bytes
and the same UID. While that response was held, the original runner was still
running and independent API reads reconstructed the complete publication.

The migration inserted one row using a PostgreSQL sequence. Both the committed
row count and the sequence remained one before and after releasing the retry;
the sequence would advance even if another execution rolled back. The same Job
and Pod completed without replacements or restarts, with one preflight, and the
current generation reached `HistoryMatched`. The probe restored the ordinary
Service route before releasing the retry and removed its proxy and copied
credential afterward. Retained evidence includes the exact immutable publication,
receipt observations, runtime identities, and original Job/Pod completion.

[Calibrated engine evidence](evidence/result-lost-ack-engines-2026-10-02/summary.json)
repeats the fault for PostgreSQL 17.11 and MySQL 8.4.11 on the same packaged
`ded02b25` installation. MySQL uses InnoDB and the ordinary file transaction mode.
Before either migration, an actual rolled-back insertion proves that its counter
advances without a committed row; the witness is then reset. MySQL reads the
allocated auto-increment value with cached statistics disabled. Both original
Apply Pods returned the same durable receipt on retry and completed once, with
one committed row and one allocated counter value. The retained publications
were reconstructed and their digests checked independently. Both probes restored
the Service route and removed their proxies and copied credentials.

This proves one lost acknowledgment for each recorded native migration row.
Concurrent duplicate requests, receiver failure during retry, and other minor
combinations still require installed proof. Remaining failure cases,
abandoned partial-publication cleanup,
restore, CA rotation, enforced NetworkPolicy, Lease contention, and the complete
Kubernetes and database matrix remain explicit #586 acceptance work. The option
remains disabled by default.
