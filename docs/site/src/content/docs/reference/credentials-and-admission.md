---
title: Credentials and admission
description: Which process holds which credential, and what the admission contract refuses.
---

The rule the rest of the operator is built around is that the controller
never touches a database and the process that touches a database never holds
a Kubernetes credential. This page is that rule in detail, and the admission
contract that keeps it true. The operational view is
[Security model](../../use/security/).

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
| Apply | no | target only | the plan, projected from its chunks into immutable ConfigMaps |
| Migration Resolve | yes | no | requested OCI reference |
| Migration Verify | yes | no | requested reference plus resolved digest evidence |
| Migration History/Apply authority guard | authority and transport grants only | no | digest-pinned reference plus optional CA source bytes |
| Migration History/Apply fetch init | yes | no | digest-pinned migration directory |
| Migration History main | no | target only | local migration directory |
| Migration Apply main | no | target only | local migration directory |

The controller sees only Secret names and keys; Kubernetes resolves those
selectors in the Job Pod. A migration reads a local directory rather than an
OCI reference for the same reason the table splits every row: passing
`oci://…` to the process that runs SQL would put registry and database
credentials in one process.

For Resolve and Verify, the runner replaces a mounted custom CA ConfigMap
projection with a private snapshot before starting Ptah. For Observe and Plan,
only the credential-free guard mounts that projection; it validates the fixed
Secret-owned digest grant and copies exact bytes to a dedicated EmptyDir before
the credentialed fetch starts.

The dev database is a scratch database Ptah may use for a comparison. It
reaches `Plan` and nothing else — never Observe, never Apply, never any
migration operation — and the runner redacts it alongside the target.

## Admission

The manager's own writes pass through two independent fail-closed layers.

**Typed policies**, one per kind, reject objects outside their narrow
structural form: Jobs, plan chunks, the ConfigMaps an Apply mounts a plan
through, schema plans and migration plans, plus a sixth policy over
`PtahSchema` and `PtahMigration` updates.
One policy per kind avoids cross-type CEL assumptions. The Job policy admits a
Job that satisfies the schema shape or the migration shape and nothing else.
The policies are ordinary release objects, and an upgrade updates them in
place. The Job, schema plan and migration plan policies carry the release's
manager image and controller-state version as literals, so a Job or a plan
stamped by another manager release is refused. The chunk and projection
policies carry neither: each is bound to its plan by name and owner, and the
plan names the manager. Nor does the policy over `PtahSchema` and `PtahMigration`
updates, which judges resources a person owns and no manager stamps.

**Webhooks** perform the stronger semantic check by reading the owning resource
and plan and reconstructing the exact expected object. Six ship:
`mapproval` and `mmigrationapproval` on the mutating side; `vapproval`,
`vmigrationapproval`, `vpodintent` and `vcontrollerwrite` on the validating
side.

A Job carries an annotation envelope that admission checks as a set: eight
annotations, including the manager image, revision and controller-state
version, and two more on a schema Apply for the plan fingerprint and content
digest. A Job without the controller identity is refused on create and on
update.

The manager annotations record which manager built a Job, and bind nothing. A
manager release that keeps the execution binding adopts a `PtahSchema` Job its
predecessor dispatched when it builds the same Job apart from that identity:
it takes the identity from the live Pod template and holds the template to the
admission snapshot the claim persisted before dispatch, so what it takes from
the Job is pinned by the claim rather than by the Job being checked. A release
that changed anything else in the Job cannot confirm it, and settles a
dispatched Apply as outcome unknown.

Upgrade compatibility is deliberately narrower than ordinary reconstruction.
After an upgrade durably retires an execution epoch, the replacement manager
may add only the cleanup TTL to an exact terminal Job retained from the
supported predecessor, and only when the operation name, UID, owner, envelope,
admission snapshot and retirement conditions all match. A dispatched
predecessor Apply is handled only from persisted outcome-unknown evidence: the
replacement waits for every exact-owner Pod and the Job's terminal condition,
then adds only the TTL. It never reads, replays or certifies the retired
result; fresh read-only observation stays mandatory.
