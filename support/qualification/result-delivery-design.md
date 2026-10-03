# Durable runner result delivery

Implementation design for [#586](https://github.com/stokaro/ptah-operator/issues/586).
The target is stable 0.2.0. This document does not authorize a release.
The chart enables durable delivery by default: listener, Pod binding, admission
validator, background consumer, and independent certificate rotation. It
provisions trust objects and a Service, grants scoped permissions, and can
render a receiver NetworkPolicy with explicit infrastructure peers. The linked
qualification evidence records completed transport and lifecycle cases; final
default-installation integration and the #242 acceptance decision remain open.

## Pod-token authentication

The capacity failure in Actions run `37137154401` exposed a mismatch between
short-lived delivery credentials and the retained operation evidence. The certificate
implementation created a credential Secret for each operation, and admission retained that projection
as long as its canonical record exists. Secret usage therefore followed operation
rate times retention, even when few Jobs run concurrently. Raising the Secret
quota does not correct that lifecycle. The manager now selects receiver-audience Pod tokens. Installed workflow and
capacity evidence for this replacement are still required.

Each operation uses an explicit projected ServiceAccount token. Kubelet obtains and rotates a token bound to the Pod, with
`operator.ptah.run/results` as its sole audience. Keep default API-token
mounting disabled and grant no resource permissions to the runner. The receiver
alone gains permission to create TokenReviews. The receiver audience must not be
an audience accepted for Kubernetes API authentication.

Authenticate each request through TokenReview with that explicit audience. Check
the authenticated ServiceAccount against the actual Pod and require the exact
Pod name and UID from the review. A caller-provided operation identity is only a
claim: retain the existing direct-read Job, resource, generation, epoch, and
operation checks, plus the immutable first-Pod binding. Recheck authentication
and authorization before durable completion, including on reused connections.
TokenReview errors are temporary refusals, never permission to publish.

Retain a non-secret immutable operation binding independently of the token.
New binding records contain only the canonical public identity; neither token
bytes nor client private keys are written. The existing credential record role
and retention guard also read old development certificate records for cleanup. Keep the
existing result retention, publication, and unknown-outcome recovery rules.
Server TLS remains mandatory. Its public trust bundle and the operation's public
identity inputs must be fixed in the admitted Job template; only the main runner
may mount the token. The runner rereads the projected token for each bounded
preflight or delivery attempt so kubelet rotation does not require another SQL
execution. No legacy-credential fallback is selected by an incoming request.

The correction is complete only after the installed workflow proves unchanged
Secret usage across successive operations at the original quota, exact identity
refusals, lost-acknowledgment redelivery without repeated SQL, and result survival
after Pod deletion and manager replacement. Local transport tests are component
evidence only. Existing certificate-authentication evidence cannot qualify the
new authentication mechanism. No new supported platform or failure matrix is
introduced by this change.

References: [projected tokens](https://kubernetes.io/docs/concepts/storage/projected-volumes/#serviceaccounttoken-projected-volumes)
and [TokenReview authentication](https://kubernetes.io/docs/concepts/security/service-accounts/#authenticating-service-account-credentials-in-your-own-code).

## Storage boundary

`internal/resultstore` implements the storage portion. A receiver must validate
the protocol and authenticate the operation before calling it. The package
cannot establish those properties by hashing a request or checking its fields.

Use immutable `PtahResultRecord` objects in the resource's namespace for the
upload intent, payload chunks, and completion record. This dedicated RBAC
resource lets the receiver read results without permission to read database
Secrets. Ordinary ConfigMap readers must not gain access to plans. Record read
access exposes confidential operation data. Old development credential records
can contain private delivery keys; new Pod bindings contain only public identity.
Encryption at rest and backups must explicitly cover this CRD.

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
and completion, admission reads the canonical original-Pod binding and checks current Job/Pod
and operation authority. A missing binding refuses publication. TokenReview
authenticates the runner request at the receiver; it grants no direct API write.

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
an authenticated Pod-bound token with the receiver audience. A canonical public
identity header names the complete binding and target engine; the direct API
checks and immutable first-Pod record establish its authority. The route must
name that binding's deterministic publication name; the result must name its
operation and operation ID. TokenReview is repeated before publication, including
on reused TLS connections.

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

The sender requires server certificate verification and rereads the projected
Pod token for each request. It refuses redirects and validates
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

`internal/resultauthority` holds an authenticated identity to the current schema or
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
snapshot. The authorizer refuses an identity whose generation differs from
that value even if the live resource now has that generation. Consumers still decide whether evidence is
current and whether the SQL process has stopped under the existing Lease and
unknown-outcome rules. Tests cover all nine operation types using the golden
Jobs that workload tests hold to the production builders, plus authority changes,
API outages, retirement during reads, and Pod replacement. These are local
predicate tests, not installed certificate or cluster acceptance evidence.

## Runner delivery path

The installed runner accepts `--result-endpoint` and `--result-token` together.
The token is read from a bounded 8 KiB regular file on every preflight and
publication attempt. The main container alone mounts the `0440` projection;
its sole audience is `operator.ptah.run/results` and requested lifetime is one
hour. Automatic API-token mounting stays disabled.

The admitted template fixes the public server CA bundle and operation identity.
Downward API fields supply the actual Pod namespace/name/UID and owning Job UID;
the template cannot substitute literal values. The runner verifies these inputs,
the operation, engine, original generation, and server trust before starting a
child. Duplicate binding variables are refused. A bounded HEAD preflight must
succeed before execution. The receiver still checks the live operation and its
immutable first-Pod binding, and execution guards still apply after preflight.

The certificate command path remains available for earlier development fixtures.
Combining token and certificate arguments is refused. The installed manager
selects only token authentication; no failed request can choose another mode.

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

## Pod binding and Job projection

The manager sets `workload.Builder.ResultServerTrust` to the service's current
public bundle. The builder captures it with the operation identity in the
admission snapshot and adds the exact receiver-audience projection. Later trust
rotation does not rewrite the Job or its snapshot. Job matching accepts the
original public bundle only while the full template matches the persisted digest.
The existing server CA overlap must cover the maximum Job and delivery horizon.

`resultcredentials.PodBindings` reads live authority, creates an immutable public
`PtahResultRecord`, reads back the persisted winner, and rechecks live authority.
Its deterministic name fixes the resource UID, operation ID, and Job name. The
record fixes the original Job/Pod UIDs, generation, epoch, input fingerprint,
operation, and engine. Repeated enrollment returns the same record. A replacement
Pod cannot change the pin even after the original Pod disappears.

No operation creates a Secret. New bindings preserve the existing retention
window and all active-operation, pending-observation, Lease-release, unresolved
migration, and restore pins. Retired records are collected only after the exact
Job is absent and the persisted window has elapsed. The collector still reads
older development certificate records and their owned Secret projections.

The Pod webhook checks the pin for token Jobs even though they reference no
credential Secret. It admits the first Pod before enrollment, refuses another
CREATE after enrollment, and permits updates only for the recorded Pod UID.
The runner waits at receiver preflight until the controller has pinned the Pod;
this wait grants no SQL authority. Concurrent admission and enrollment are not
a cross-object transaction, so receiver and consumer checks remain mandatory.

Local tests cover all nine operation projections, original-Pod replacement,
public-record retention, a receiver restart without Secret access, and runner
token rotation after a lost acknowledgment without repeated execution. Real API
admission tests accept both resource families and refuse API audience, enlarged
token lifetime, additional projection, and init-container access. These checks
do not replace installed workflow or capacity qualification.

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

Reconciliation and admission retain a stable service reference. Installed
receivers authenticate through TokenReview and the public Pod binding. New
handshakes use the current serving certificate. The existing certificate
provisioning and enrollment policy fence remain shared by the replicas; the
client signer files support the earlier development certificate path only.

Existing Jobs retain their original public server bundle. Expanding the bundle
for new Jobs does not rewrite or invalidate existing snapshots. Already
acknowledged result records remain readable independently of token or
certificate validity.

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

Namespace termination forbids creating a retirement record. If that is the
API's refusal, the collector starts foreground deletion of the existing intent
and credential instead. This requires absent execution/recovery pins and an
absent original Job. The API-assigned deletion timestamp starts the same full
retention window; namespace age and publication age cannot shorten it.
Admission protects the foreground finalizer as well as every DELETE. After the
window, completion and chunks are collected in order, and Kubernetes may finish
the roots' foreground deletion. Late children still receive their own full
window. This path uses the existing DELETE grant and creates no new resource.
The terminating namespace's bytes remain available for backup during retention;
foreground retirement does not authorize delivery or execution to resume.

A real-API regression reproduces the NamespaceLifecycle refusal before this
fallback and checks refused early deletion/finalizer removal, absent Job and
active-claim gates, and permitted completion after the window. Envtest has no
garbage collector: its finalizer-removal requests model that actor. The
[installed namespace cleanup evidence](evidence/result-namespace-cleanup-2026-10-02/summary.json)
now supplies native collection after the real one-hour interval.

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
including backup/restore and enforced NetworkPolicy. The installed cleanup,
rotation, request bounds and receipt-consumption cases are mapped below. Do not attach a time-only TTL to unconsumed results or
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

[Receiver replacement evidence](evidence/result-receiver-restart-2026-10-02/summary.json)
adds both native migration engines on the packaged `b2576f9b` runtime. The proxy
held the retry before forwarding it. After reconstructing the first persisted
publication and observing one SQL effect, the probe removed both manager Pods
and waited for two new ready UIDs and their Service endpoints. Only then could
the retry reach the replacement receivers. Both runs returned the original
receipt, completed the original Job and Pod without restarts, and reached
current-generation `HistoryMatched` with one SQL effect. The evidence retains
the old and new manager UID sets, ordering observations, immutable publication,
calibrated database counters, and exact runtime identities.

[Unpublished execution-loss evidence](evidence/result-runner-loss-2026-10-02/summary.json)
uses the same packaged runtime and both native engines. A real result-record
quota was filled before releasing the original Apply credential. After one SQL
effect committed, with no original Apply intent and the Pod still reporting
`Running`, the probe removed that Job and Pod. The controller recorded the exact
run as `Unknown` and preserved its metadata copy. Removing the quota allowed a
new native history reading to settle the run through `HistoryRead`. Both runs
reached current-generation `HistoryMatched`, with no replacement Apply, no
original Apply publication, and one SQL effect. Retained history publications
independently report version 1 applied with nothing pending. This covers a fully
applied migration after loss of both execution objects; partial work and human
acknowledgment with fresh approval remain separate acceptance cases.

[Concurrent redelivery evidence](evidence/result-concurrent-2026-10-02/summary.json)
uses manager revision `b0a51ff7`, the packaged chart, and the existing
`b2576f9b` runner and fixture on the same Kubernetes 1.37 installation. For each
native engine, the first result was persisted and its acknowledgment lost. The
harness held the runner retry and sent six requests in overlapping pairs through
both receiver Pods. Identical pairs returned the original receipt, changed
canonical documents returned 409, and mixed pairs preserved both outcomes. Every
immutable publication member remained unchanged. The original runner then
received its receipt and completed the same Job and Pod with one calibrated SQL
effect and current-generation `HistoryMatched`.

This row found a real error-classification defect: admission refused changed
bytes before storage could answer `AlreadyExists`, so the receiver returned a
temporary 503. A direct read now identifies the immutable conflict without
converting other write failures into a receipt. The retained pre-fix response
fails the same verifier that accepts both corrected native runs. This exercises
concurrent redelivery of an existing publication; it does not force a race
between first publishers of an absent intent or saturation of one receiver.
The privileged harness uses per-Pod port forwards, so it proves no NetworkPolicy
behavior.

[First-publication race evidence](evidence/result-first-publication-2026-10-02/summary.json)
adds both native engines using manager `b0a51ff7`, runner `b2576f9b`, and fixture
`5cf47289`. The proxy held the original authenticated result before its first
upstream request. Independent API reads confirmed that its intent and completion
did not exist. Two receiving managers then reached CREATE for the same intent;
a namespace-only admission barrier held both until distinct manager Pod and
admission request UIDs were present. Neither write was released before both
arrived. Both receivers returned one receipt, and the subsequent conflicting
and mixed pairs preserved it. Each original runner then completed its lost-ACK
row with one calibrated SQL effect and no replacement execution. The evidence
retains the absent-record census, overlapping admission timestamps, publication,
and restored Service ports and webhook cleanup. This forces identical first
writes; competing different first payloads are a separate case.

The installed slow-upload evidence in
`evidence/result-upload-budget-2026-10-02/` covers native PostgreSQL and MySQL
migrations on Kubernetes 1.37.0 / Linux amd64. An incomplete authenticated body
occupies the leader receiver while another migration converges through the free
replica in 55.41 and 53.79 seconds, respectively. Both receivers are then occupied;
extra requests receive 503 with retry advice within 0.20 seconds. Each stalled
body receives 408 after about 120.03 seconds and its upload slot recovers. The
held original Apply Lease retains its identity and renews with a maximum gap of
5.23 seconds. No incomplete upload creates an intent. Restoring the route and
releasing the original Pod yields one calibrated SQL effect in each database,
with no replacement Apply. The evidence retains actual Lease samples, request
intervals, both native publications per engine, and fault cleanup.

`evidence/result-schema-upload-budget-2026-10-02/` completes the same slow-client
and saturation cases for PtahSchema on both engines. Independent schemas reach
`InSync` in 73.12 and 72.43 seconds; direct database reads verify the declared
column types, nullability and primary key. The maximum Lease renewal gap is
5.61 seconds, and each original Apply Job/Pod completes without replacement or
restart. This schema evidence claims database convergence, not the allocated SQL
counters used by the migration probes. Both engines' retained operation inventory
independently reconstructs successful publications for all nine supported
family/operation pairs. SubjectAccessReview records include an explicit `log`
subresource and show result-record GET allowed and Pod-log GET denied for the
installed manager. The existing background-consumer evidence remains applicable:
its reader and controller source hashes are unchanged. These results do not
establish progress while all receivers or persistent storage are unavailable.

### Controller-independence evidence mapping

The #586 controller-independence row is supported by the following complementary
checks. They do not claim successful new delivery during a total receiver or
storage outage. Such a claim is not the existing DoD: unavailable delivery must
remain bounded without occupying reconciliation or replaying SQL.

| Required behavior | Retained evidence |
| --- | --- |
| Both families and every supported operation | `result-schema-upload-budget-2026-10-02/operation-inventory.json` retains complete publications for all nine operation pairs on both engines. `TestBothControllersReadDurableResultsAfterPodDeletion` and `TestStoreLoaderSurvivesPodAndJobLossForEveryOperation` exercise consumption. |
| No Pod-log dependency | The inventory's explicit Pod `log` subresource SubjectAccessReviews deny access. `TestDurableResultNeverUsesLogsOrTerminationSummary` refuses any fallback. |
| Slow upload, saturation, concurrent progress and Lease renewal | Both installed upload-budget directories retain four family/engine cases. Independent resources converge before the first upload times out; both occupied receivers refuse excess requests; held Apply Leases renew within the predeclared 15-second bound. |
| Receiver unavailability | `TestRunnerAuthenticatesProjectionBeforeStartingSQL/receiver_unavailable` uses a real runner process and TLS endpoint returning 503 and proves the SQL child never starts. `result-receiver-restart-2026-10-02/` removes both installed receivers after acknowledgment; the same original runner later receives its original receipt, with one calibrated SQL effect on each engine. |
| Storage failures | `result-retention-2026-10-02/quota-recovery.json` and its API audit retain an actual refused chunk write followed by completion of the same publication. `result-runner-loss-2026-10-02/` covers quota refusal after native SQL, loss of the original Job/Pod, and recovery by fresh history without replay on both engines. |
| Bounded controller reads under failures | `TestLoadTimeoutCancellationAndRetry` bounds failed storage reads; `TestReaderRequiresLifecycleAndDropsFailedValues` discards partial failed values; `TestPollDoesNotWaitAndDeduplicates` proves polling does not wait on the background loader. These are component checks, not installed database-outage timings. |

The five consumer/controller files listed in
`evidence/result-consumption-2026-10-02.json` that implement and test durable
harvest and background reads still match their recorded SHA-256 values at
`63783c6c`. The runner main, delivery test and sender still match
`evidence/result-credential-records-2026-10-02.json`. The later full
`make verify-source` passed at `54ebef04`; production directories and dependency
files are unchanged between that tree and `63783c6c`. Reusing those executions
avoids repeating unchanged checks. Installed evidence retains each actual
manager, runner, fixture and chart identity, rather than claiming all images
were built from the documentation commit.

This mapping completes the case inventory for controller independence in the
recorded environment. The final supported matrix and acceptance decision remain
open; the component rows do not stand in for that matrix.

[Installed interrupted leaf-renewal evidence](evidence/result-leaf-rotation-2026-10-02/summary.json)
adds PostgreSQL / Kubernetes 1.37 / Linux amd64 proof before first harvest. The
rotator saved a new serving certificate and key while an exact-Secret admission
hold prevented projection. After its Pod was removed, its replacement resumed
the same candidate. Both unchanged manager Pods loaded the new projection; the
rotator verified their endpoints before returning to stable and ready. CA digests,
enrollment policy and trust-object UIDs stayed unchanged. After the producing
Pod/logs and both receiving managers were removed, the controller consumed the
original receipt and reconstructed the same 8 MiB plan. Ordinary approval led to
one Apply and verified PostgreSQL convergence. Independent live readback then
rebuilt both the receipt payload and planstore bytes. Temporary gates, rotator
arguments and leadership were restored. This closes the installed serving-key
renewal/interruption case; it does not exercise CA replacement or retirement waits.

[Installed expired CA recovery evidence](evidence/result-ca-rotation-2026-10-02/summary.json)
covers the same PostgreSQL first-harvest boundary through replacement of both
server and client authorities. An injected, correctly signed pending journal
held expired current and candidate CAs; the rotator was stopped during injection.
Both manager processes were removed. The installed rotator discarded the expired
candidate, persisted fresh CA keys and an enrollment fence, and was replaced
while a projection hold prevented completion. Its replacement retained the same
candidate and fence. Removing the hold allowed the production expiry-plus-skew
path to switch and retire trust; both receivers and the rotator became ready.
After a further manager replacement, first harvest reconstructed the original
8 MiB plan and ordinary approval led to one verified native Apply. Live readback
independently rebuilt both the publication and planstore bytes. This is installed
recovery from deliberately expired state. It does not represent elapsed normal
credential-retirement waits; the existing local state-machine tests retain that
separate timing claim. Both installed rotation cases use unchanged production
code, default log sizes, and the recorded runtime identities.

## Publication, authority and retry evidence mapping

The following rows map the existing #586 requirements to executed checks.
`evidence/result-dod-audit-2026-10-02/reuse.json` records unchanged Git objects
for production packages, installation contracts, dependencies and real API tests
since the successful `make verify-source` at `54ebef04`. Its original log is
retained beside the mapping. No component execution was repeated for this audit.
Installed bundles retain the actual runtime image identities; none is relabeled
as a final release build.

| Durable acknowledgment boundary | Executed evidence |
| --- | --- |
| Complete length/digest, protocol and exact binding before receipt | `TestSizeDigestAndInvalidBindingRefusedBeforeWrites`, `TestInvalidBodyAndRouteNeverReachStorage`, `TestPublicationBindsEveryIdentity`, and `TestPublicationAdmissionRefusesDamagedCompletion`. Installed first-harvest, concurrent delivery and quota-recovery bundles independently reconstruct manifests, all chunks and completion records. |
| Partial/canceled writes, missing or corrupt members and failed final readback | `TestStorageDamageCannotProduceReceipt`, `TestCanceledPublicationReturnsNoReceipt`, `TestFinalReadbackFailurePreventsAcknowledgment`, and `TestCompletedPublicationCannotBeHealed`. The real API publication suite rejects invalid protocol and record writes. Installed quota recovery retains an actual refused chunk CREATE. |
| Restart/resume at every publication write | `TestResumeEveryPublicationBoundary` covers failure before and after intent, each chunk and completion, then resumes with a fresh store. `TestResultPublicationResumesLostAPIWriteResponse` does the role-level lost-response cases through real API admission. Installed receiver replacement preserves the same receipt; these are separate component/API/process claims. |

| Exact authority/confidentiality boundary | Executed evidence |
| --- | --- |
| Namespace, resource UID/generation, epoch, operation, attempt and exact Job/Pod | `TestPublicationBindsEveryIdentity`, `TestRefusals` in `internal/resultauthority`, `TestMigrationClaimRefusals`, `TestAuthorityReadRacesAndFailures`, `TestSecondPodRefused`, and `TestReplacementPodCannotRemintAttemptCredential`. All-operation credential and workload tests bind these predicates to production builders. |
| Authenticated sender/receiver and expiration | `TestTLSRefusesUntrustedOrMissingClientAndServerCertificates`, `TestExpiredIdentityOnExistingConnectionAndMalformedSAN`, `TestAuthorityRecheckedAtPublicationBoundary`, `TestCurrentClientTrustRecheckedAtPublicationBoundary`, and `TestIssuedCredentialDeliversThroughLiveAuthority`. Actual runner preflight refuses invalid projection/authority before SQL. Installed recovery and rotation bundles use the chart's mTLS endpoint. |
| Enrollment epoch, retired or changed credentials | `TestEnrollmentRefusesStaleSignerWithoutChangingIssuedCredentials`, `TestEnrollmentRechecksPolicyBeforeCanonicalWrite`, `TestOldAttemptCannotAcquireNewGeneration`, `TestIssuerRejectsCorruptedOrForeignCredential`, and real API `TestResultEnrollmentPolicyFencesStaleReplicas` / `TestResultPublicationRefusesUnissuedOrRetiredAuthority`. |
| No API credential/object-write grant to the runner | `TestBuildHardensEveryContainerAndPod` and `TestBuildMigrationHardensEveryContainerAndPod` require disabled token automount. Workload and admission tests reject broadened Pod projections. `TestResultCredentialAdmission` and `TestUnrelatedPodCredentialReferences` enforce exact, read-only original-Pod credentials; `TestResultChartRBACAndTrustBootstrap` exercises rendered permissions. The installed Jobs use these same templates and guards. |
| No plan/key/credential data in diagnostic output or ordinary status | `TestRunnerRedeliversAfterLostAcknowledgmentWithoutReexecuting` requires empty stdout; `TestRunnerDoesNotFallBackToLogsAfterDeliveryRefusal` requires a fixed stderr and no frame. Misconfiguration and preflight refusal tests likewise assert fixed diagnostics. `TestDurablePlanPreservesBytesWithoutProcessKey` refuses SQL in diagnostics; `TestTheSummaryCarriesNoCredentialAndNoChildText`, `TestRunRedactsCredentialsAndURLPasswords`, and the migration error-redaction tests retain the existing bounded output contract. Controller harvesting continues to project typed metadata into ordinary status, with plan bytes in the protected store. Sensitive data stays in the scoped result records and credential projection. |

| Idempotent delivery boundary | Executed evidence |
| --- | --- |
| Identical bytes retain the receipt; changed bytes cannot replace it | `TestConcurrentIdenticalAndConflictingDelivery`, `TestConflictingBytesCannotReplacePartialPublication`, and the installed concurrent-redelivery and first-publication-race bundles. The retained pre-fix 503 and post-fix 409 show the admission conflict correction. |
| Lost response, receiving process/leader replacement, no SQL rerun | Installed `result-lost-ack-engines-2026-10-02/` and `result-receiver-restart-2026-10-02/` retain the original Job/Pod, same durable receipt and calibrated native execution counter on both engines. `TestRunnerRedeliversAfterLostAcknowledgmentWithoutReexecuting` separately counts child invocations. |
| Retry and upload bounds | `TestDeliveryAttemptAndDeadlineBounds`, `TestSenderRefusesRedirectsAndMismatchedReceipts`, and `TestSlowUploadReleasesSlotAndSaturationDoesNotQueue`. Installed slow-upload bundles for both families/engines retain the 120-second body deadline, immediate excess-request refusal, Lease renewal and independent progress. |

These mappings complete the durable-acknowledgment, authority and idempotency
case inventories. Component refusal tests, real API admission tests and installed
fault cases are explicitly distinct. They do not close the remaining supported
logging matrix, retention/restore lifecycle, default enablement or #584 acceptance.

## Acknowledged-result survival evidence mapping

| Required boundary | Retained result |
| --- | --- |
| Acknowledged result before first consumption, producing Pod/log loss and receiving process/key loss | `result-first-harvest-2026-10-02.json` reconstructs the same native 8 MiB plan and digest after replacing both managers; ordinary approval and one Apply reach database-verified convergence. |
| HA and key rotation | `result-leaf-rotation-2026-10-02/summary.json` and `result-ca-rotation-2026-10-02/summary.json` preserve that first-harvest result through interrupted serving-key renewal and expired pending CA recovery. The bounds of each case are described above. |
| Loss before acknowledgment, fully committed SQL | `result-runner-loss-2026-10-02/` retains Unknown followed by fresh history resolution with no second Apply on PostgreSQL and MySQL. |
| Loss before acknowledgment, partially committed SQL requiring a person | `result-partial-loss-2026-10-02/summary.json` retains Unknown, dirty history, manual repair without resetting the execution counter, authenticated acknowledgment, and a distinct fresh approval on both engines. Each of the two 90-second holds refuses automatic replay. One separately approved recovery Apply then converges. |

The partial-loss archive includes the exact original operation and authenticated
resolution, both approval bindings, independent complete history and Apply
publications, native allocation counters and default kubelet configuration.
PostgreSQL resumed the same unresolved operation after a recovery-publisher test
error; both procedure identities and its unchanged checkpoint are retained.
MySQL completed the corrected procedure without interruption. These are the
recorded Kubernetes 1.37 Linux amd64 results, not a final matrix claim.

This completes the case inventory for acknowledged-result survival. It does not
make database execution and result publication atomic. The existing recovery
and fresh-authorization requirements remain necessary.

Other minor combinations, restore, enforced NetworkPolicy, and the complete
Kubernetes and database matrix remain
explicit #586 acceptance work. The option remains disabled by default.

## Abandoned publication collection evidence

[Installed cleanup evidence](evidence/result-abandoned-2026-10-02/summary.json)
closes the remaining partial-publication collection case on Kubernetes 1.37
Linux amd64. A real quota refusal stopped the first Resolve chunk after its
intent and canonical credential were persisted. Suspending the read-only
resource retired that exact operation; the original Job and Pod were removed.
No complete receipt existed. The collector persisted its own retirement record,
which fixed the unchanged one-hour deadline at 18:21:27 UTC on October 2, 2026.

The three-record cohort was collected between 18:21:27.899 and 18:21:27.971 UTC.
Successful API DELETE records carry the exact original UIDs as preconditions.
Kubernetes garbage collection removed the credential's Secret projection.
The namespace contained no remaining result records after collection. Throughout
the wait, a suspended native MySQL migration's latest-run pin retained its four
records and credential projection unchanged; two published schema plan objects
also retained their identities and spec hashes. No clock, object timestamp or
retention setting was shortened.

The original interruption, quota-refusal audit, frozen cohort, observations,
deletions and independently replayed verdict are retained. The probe's refusal
tests reject a completed or replaced publication, a different cause for the 403,
early deletion, missing members and changed pinned evidence. The temporary
API audit captures no result or credential write bodies. Its original manifests
are restored after the proof. This completes this cleanup case; backup/restore,
enforced NetworkPolicy and final profile acceptance remain separate requirements.


## Installed receiver network evidence

[Retained network evidence](evidence/result-network-2026-10-02/summary.json)
proves the packaged chart's receiver ingress and the example's result egress on
Kubernetes 1.37 Linux amd64 with enforcing kindnet, two ready receivers and
unchanged `10Mi` kubelet logs. A client without operation labels reaches both
receiver Pod addresses and the Service before the policy, reaches none while
it is installed, and reaches all three after removal.

With ingress enforced and all eight example egress policies installed, native
PtahSchema and PtahMigration workflows converge on PostgreSQL 17.11 and MySQL
8.4.11. Each workflow has one Apply Job and the expected database state. Every
supported operation has a complete independently reconstructed publication:
five schema and four migration operations on each engine. Recorded producer
Pod and Job UIDs bind the publications; actual Pod labels match both policies.
Only destination namespaces, labels, registry/database addresses and database
ports are adapted to the installed lab, as the example instructs.

The archive retains policy API objects, actual producer and receiver Pods,
traffic readings, resources, Jobs, receipts, database metadata, runtime image
identities and the executed procedure. Offline replay refuses changed selectors,
Pod/Job bindings, missing operations, extra Apply Jobs, changed chunks and absent
traffic controls. Temporary policies, probe Pods and four databases are removed after capture.
Both schema namespaces were removed. The two migration namespaces exposed a
retirement-marker CREATE refusal during namespace termination. The foreground
retirement fix and installed proof below complete their removal without bypassing
admission or shortening retention. The shared qualification lab remains for
remaining work.

This completes the receiver network case on the recorded environment. It does
not replace the other denied-destination egress rows in #578, the supported-minor
payload matrix, restore coverage or final acceptance. The lifecycle checkbox
and default transport setting remain unchanged.


## Durable storage backup readback

[Encrypted snapshot restoration](evidence/result-backup-restore-2026-10-02/summary.json)
now preserves the installed result/key representation on Kubernetes 1.37 Linux
amd64. A separately identified etcd member, restored from the encrypted archive,
returns the exact bytes and UIDs for 74 selected objects. Eleven publications
reconstruct from the restored records across both resource families and native
engines. Trust, the private rotation journal, enrollment policy and operation
credential projections are included; no stored binding is rewritten.

The revised recovery inventory includes `PtahResultRecord` and requires the
journal's original projection/policy identities. The proof retains only safe
identities and checksums in Git. Its encrypted archive and key remain private.
The isolated member and plaintext files are removed, and source schemas return
to suspension with their original policies. This proves storage restoration. The preserved-identity startup case below
adds restored manager/rotator execution; service RPO/RTO, loss during Apply and
lagged operator backups remain requirements of #579. The lifecycle checkbox
stays open.


## Installed terminating-namespace cleanup

[Native cleanup evidence](evidence/result-namespace-cleanup-2026-10-02/summary.json)
records both previously stuck migration namespaces on manager `7b21f895`,
Kubernetes 1.37 Linux amd64 and unchanged `10Mi` kubelet logs. The API starts
foreground retirement at 20:49:38 UTC for intents and 20:49:43 for credentials.
All eight original records have successful API GETs after their full one-hour
deadlines. No successful CREATE can substitute a replacement UID. Completion
and chunks are deleted, garbage collection finishes the intents, and the
namespace controller collects the remaining credentials at 21:50:44 UTC.
Both namespaces disappear; four recovery-pinned control records retain their
UIDs and data hashes.

The original probe completed its retention hold and namespace checks, then
failed audit verification because it required named mutations and omitted
`DeleteCollection`. That failed report is preserved. Corrected offline replay
accepts the actual namespace-controller path and refuses missing post-deadline
reads, early or failed collection, a filtered request, a foreign caller, and
replacement creation. It does not rerun or shorten the hour. Temporary audit
configuration is removed and all three API servers and both managers are ready.

This closes the terminating-namespace cleanup case. The remaining lifecycle,
restore, payload matrix and final acceptance requirements stay open.


## Restored storage used by new processes

[Preserved-identity startup](evidence/result-control-plane-restore-2026-10-03/summary.json)
now extends storage readback through actual manager/rotator execution. All three
original etcd stores are replaced from a verified encrypted snapshot. The new
logical cluster preserves the original 41 result/key objects and their binding
hashes. Both receivers and the rotator restart on their original Pod UIDs and
unchanged images, with a verified informer revision bump and compaction.

After restoring leader access, the controller first consumes the saved receipt,
publishes the identical 8 MiB plan, and converges through one newly approved
PostgreSQL Apply. No replacement Plan Job runs. Temporary plaintext and restore
containers are removed; runtime, admission and leadership readback pass. The
case's namespace waits for ordinary retention after its database is removed.
This completes the preserved-identity startup case on Kubernetes 1.37 Linux
amd64. Other recovery cells and final acceptance remain open.


### Native MySQL maximum with default logs

[Installed MySQL boundary evidence](evidence/result-mysql-maximum-2026-10-03/summary.json)
proves the exact 8 MiB plan on Kubernetes 1.37 Linux amd64 with all four
kubelets reporting `10Mi`. The Plan receipt is committed while controller
leadership is disabled. Its producer Pod and logs are removed, both managers
are replaced, and only then is leadership restored. First harvest reconstructs
the same digest from 16 plan chunks. One fresh approval permits one Apply;
MySQL 8.4.11 reports all 100 tables and the expected 1,393,516 default characters,
including 1,393,515 `<` characters. Independent readback checks every generated
statement and its escaping, current-generation convergence, the completed Apply
Job UID, and restored admission/leadership controls.

The recorded runtime images are development identities. The database is
removed and namespace deletion follows ordinary retention. This closes this
engine/minor maximum row; maximum-plus-one and the other supported minors remain
open. PostgreSQL's unchanged maximum proof is reused, not rerun.


### Native maximum-plus-one refusal without logs

[Installed boundary refusal evidence](evidence/result-oversized-2026-10-03/summary.json)
records PostgreSQL 17.11 and MySQL 8.4.11 on Kubernetes 1.37 Linux amd64.
Each native executor saves exactly 8,388,609 plan bytes and exits zero; the
runner returns `invalid_plan_output` with the actual size and unchanged
8,388,608-byte limit. The original receipt remains readable after its producing
Pod/logs are removed and both managers are replaced before consumption.
Independent API and database readback confirms no plan, plan chunks, Apply
Jobs or native tables, with standard `10Mi` logging on every node.

The initial PostgreSQL probe failed because it required an empty active claim.
The controller retains an undispatched read-only retry claim while recording
its failure. The archive preserves that failure and the actual reading used by
the corrected verifier's regression. Verification reuses the same native
receipt; PostgreSQL Plan was not rerun. Delayed readback also accounts for
normal completed-Job cleanup by positively identifying the original retained
Plan intent and credential, with no Apply authority. MySQL's corrected native
probe exits zero. Both test databases are removed; namespace deletion follows
ordinary retention. The remaining default-log boundary rows are Kubernetes
1.35 and 1.36. These cases do not complete lifecycle or final acceptance.


### Kubernetes 1.35 default-log boundary rows

[Installed Kubernetes 1.35.8 evidence](evidence/result-kubernetes-135-2026-10-03/summary.json)
passes the exact maximum and maximum-plus-one on PostgreSQL and MySQL using
runtime images built from `942f5704`. All four native probes exit zero. Each
receipt is persisted before its producing Pod/logs are removed and both
managers replaced. The maximum plans reconstruct from 16 full chunks and reach
one approved Apply with exact native database defaults. Oversized plans report
8,388,609 saved bytes and leave no plan, chunks, Apply or database tables.
Independent readback confirms the original receipts, plan contents, every
escaping-heavy statement, current-generation outcomes and restored controls.
All four kubelets report `10Mi` and have no explicit log-size setting.

The completed 1.35 cluster, registry and databases are removed after evidence
capture. The older completed 1.37 lab was also removed to avoid shared-host
contention during preparation. Cluster deletion is not Helm uninstall
qualification. The remaining Kubernetes 1.36 rows are recorded below; full lifecycle and
final-profile acceptance remain separate requirements.


### Kubernetes 1.36 and completed default-log boundary inventory

[Installed Kubernetes 1.36.4 evidence](evidence/result-kubernetes-136-2026-10-03/summary.json)
passes all four PostgreSQL/MySQL exact-maximum and maximum-plus-one cases.
The native probes and independent API/database readback exit zero. Every
original receipt remains readable after its producing Pod/logs are removed
and both managers replaced before first consumption. Each 8 MiB plan has
16 full chunks, exact escaping-heavy SQL, one approved Apply and verified
native database defaults. Each 8 MiB plus one result reports the unchanged
limit and leaves no plan, chunks, Apply or database tables. Every kubelet has
no explicit log-size setting and reports the standard `10Mi` configuration.
The completed cluster and its owned registry/databases are removed.

Together with the retained Kubernetes 1.35 rows, the Kubernetes 1.37
[PostgreSQL maximum](evidence/result-control-plane-restore-2026-10-03/summary.json),
[MySQL maximum](evidence/result-mysql-maximum-2026-10-03/summary.json), and
[both oversized refusals](evidence/result-oversized-2026-10-03/summary.json),
this completes the default-logging DoD's supported-minor boundary inventory.
The 1.35 and 1.36 images are built from `942f5704`; their protocol, controller,
runner and chart sources have no changes from manager revision `7b21f895`
used in those 1.37 rows. The archived runtime identities remain distinct;
these are functional Linux amd64 proofs, not a final artifact or architecture
matrix verdict. Lifecycle qualification and enabling the transport by default
remain open. The shipped `64Mi` workaround is unchanged until that integration
is complete.


### Enabled receiver lifecycle

[Installed lifecycle evidence](evidence/result-ha-lifecycle-2026-10-03/summary.json)
passes the existing six upgrade, three HA, and five uninstall scenarios with
`resultDelivery.enabled=true` in both current and synthetic-next release values.
The native running Apply remains exclusive across late upgrade failure and retry;
rollback and uninstall add no SQL. Reinstallation preserves live CRD objects,
and the exported chart recovers from quota refusal under restricted Pod Security.
Final API readback finds no receiver Service, trust/journal Secret, enrollment
ConfigMap, or rotation Lease, while seven proof objects remain. The owned lab
and its resources are removed.

HA acceptance now recognizes the declared cleanup counter without allowing it
to replace the required operation failure delta. Its schema, Job and Pods must
still be removed. Namespace deletion respects the durable result window; the
already completed API-timed one-hour namespace cleanup supplies that proof.
Original failures, native metric lines, regression results and corrected native
execution are retained in the report.

The report joins this installation evidence to the existing certificate,
NetworkPolicy, pinned retention, quota, partial cleanup, and result/key restore
proofs. This completes #586's lifecycle and bounded-storage DoD. The remaining
#579 loss/timing matrix is a separate #242 requirement. Default activation,
removal of the shipped log-size workaround, documentation integration, and final
acceptance remain open.
