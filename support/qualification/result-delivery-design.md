# Durable runner result delivery

Implementation design for [#586](https://github.com/stokaro/ptah-operator/issues/586).
The target is stable 0.2.0. This document does not qualify the transport or
authorize a release. The current runner still writes result frames to logs.

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

## Receiver and runner integration still required

The receiver runs independently of family reconcile workers, behind its own TLS
Service. It must authenticate clients before reading a large body, bound active
uploads and body-read time, reject saturation without queuing unbounded bodies,
and apply a storage deadline. Manager HTTP logs and rejection messages must not
include payloads or credentials.

The intended sender credential is a short-lived, per-attempt client certificate,
issued only after a direct read establishes the actual Job and Pod UIDs and the
existing `jobclaim` and Pod-intent contracts. Its private key is projected from
an operation-specific Secret; it is not a Kubernetes API token. The certificate
must bind the Pod UID and operation authority, not just a ServiceAccount or Job
label. A replacement Pod must never inherit authority to submit as its
predecessor. The admission/projection design must enforce that before this
credential path is implemented or enabled.

The receiver must revalidate the execution epoch, exact live Job/Pod, and current
claim through direct reads. Certificate expiration alone is not revocation.
Before committing a new completion, it must recheck authority after the upload.
Controllers must retain their own epoch and provenance checks when consuming a
receipt: persistence is evidence, not permission to apply. Admission must bind
receiver writes to the same publication intent and protect the metadata as well
as the immutable payload. Deleting/recreating a Secret is not prevented by
`immutable: true`.

The runner must validate receiver configuration before executing SQL, retain the
one completed result in memory, and retry only transmission within a fixed
delivery deadline. It must not call its executor again after a lost response.
The summary remains bounded and digest-bound; stdout/stderr become diagnostics.
There must be no correctness fallback to `pods/log`.

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

The current tests prove publication integrity, replay behavior, and Kubernetes
Secret persistence/immutability. They do not prove authentication, TLS, RBAC,
admission, garbage collection, manager failover, Lease independence, or the
end-to-end supported plan limit. Those remain the explicit #586 acceptance rows.
Keep the 64 MiB workaround until the complete workflow passes with the default
10 MiB kubelet configuration, deliberate log removal, and manager replacement.
