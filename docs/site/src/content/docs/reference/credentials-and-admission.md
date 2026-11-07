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
| Snapshot copy init, ConfigMap source | no | no | the source snapshot ConfigMap, checked against the bound content digest |
| Resolve main, Git source | Git credential and its grants | no | the repository, ref and path, and the optional CA or known hosts |
| Git fetch init, Git source | Git credential and its grants | no | the bound commit, checked against the bound content digest |

The controller sees only Secret names and keys; Kubernetes resolves those
selectors in the Job Pod. A migration reads a local directory rather than an
OCI reference for the same reason the table splits every row: passing
`oci://…` to the process that runs SQL would put registry and database
credentials in one process.

A resource whose source is a ConfigMap has no Resolve or Verify Job and no
registry credential anywhere: the manager reads the ConfigMap and writes the
snapshot, and the Observe, Plan, History and migration Apply Pods replace the
authority guard and the fetch with one init container that checks the
snapshot and copies it into the same local file or directory.

A resource whose source is Git has a Resolve Job and no Verify Job. Its
credential reaches two places and no other: the Resolve Job's operation
container, which holds no database credential, and the Git fetch init
container that replaces the authority guard and the fetch in every later
operation. The runner in either one refuses a credential unless the Secret's
`host` key names the repository's authority, and a CA bundle or known-hosts
file unless the Secret holds its digest, before Ptah starts; Ptah reads private
copies of the trust files and never sees the grants. The container that holds
the database URL mounts only the fetched files, read-only.

For Resolve and Verify, the runner replaces a mounted custom CA ConfigMap
projection with a private snapshot before starting Ptah. For Observe and Plan,
only the credential-free guard mounts that projection; it validates the fixed
Secret-owned digest grant and copies exact bytes to a dedicated EmptyDir before
the credentialed fetch starts.

The dev database is a scratch database Ptah may use for a comparison. It
reaches `Plan` and nothing else — never Observe, never Apply, never any
migration operation — and the runner redacts it alongside the target.

## Credentials from an external secret store {#external-secret-stores}

The operator reads a database URL from one place: the key of a Kubernetes Secret
that `spec.target.urlFrom` names, in the resource's namespace. A tool that writes
that Secret needs nothing from the operator, and the operator cannot tell which
tool wrote it. The routing above does not change: the manager still cannot read
the Secret, and the kubelet resolves the selector into the `PTAH_DB_URL` variable
of the container that runs Ptah. The operator has no native integration with Vault
or a cloud secret manager.

The behavior in this section comes from one run against these versions, not from
reading the code. In that run no password appeared in status, Events, plans,
result records, Jobs, Pods, the manager's logs or the logs of the operation Pods
still there when the run looked.

| Component | Version |
| --- | --- |
| Operator | `v0.2.9`, with Ptah `v0.17.0` |
| Kubernetes | `v1.37.0`, on kind `v0.33.0` |
| Target | PostgreSQL `17.11`, from the image [Databases and privileges](../../support/databases/) pins |
| Store | Vault `2.0.4` in dev mode, chart `0.34.1`, KV version 2 |
| External Secrets Operator | `v2.14.0`, chart `2.14.0` |
| Vault Secrets Operator | `1.6.2`, chart `1.6.2` |

Nothing else was run: no later operator version, no cloud secret manager, no store
other than Vault, no Vault outside dev mode, no database secrets engine or other
source of generated users, and no MySQL target. The messages quoted below are
v0.2.9's. Where v0.2.9
says `child_exit: ptah exited with code 2`, later versions name the failure, as
[A database the operation could not open](../../troubleshoot/condition-reasons/#a-database-the-operation-could-not-open)
lists.

### A tested example {#external-secret-stores-example}

External Secrets Operator took the fewest objects: a `SecretStore` and an
`ExternalSecret`, against three for the Vault Secrets Operator. The commands and
manifests below are the ones the run applied, with the operator and External
Secrets Operator installed and Vault at `vault.vault.svc`.

Put the URL in the store as one field. It has to be a complete URL, with any
reserved character in the password percent-encoded and no trailing newline.
Reading it from a file keeps it out of the command line and the shell history:

```sh
vault kv put -mount=secret ptah/application url=- < "$DATABASE_URL_FILE"
```

Let one ServiceAccount in the namespace read that one path:

```sh
vault auth enable kubernetes
vault write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc
vault policy write ptah-application - <<'POLICY'
path "secret/data/ptah/application" {
  capabilities = ["read"]
}
POLICY
vault write auth/kubernetes/role/ptah-application \
  bound_service_account_names=vault-reader \
  bound_service_account_namespaces=application \
  policies=ptah-application audience=vault ttl=20m
kubectl create namespace application
kubectl -n application create serviceaccount vault-reader
```

`vault-reader` belongs to the tool. Do not name it in
`spec.execution.serviceAccountName`: an operation Pod needs no credential beyond
the URL.

Apply the store and the Secret to write:

```yaml
apiVersion: external-secrets.io/v1
kind: SecretStore
metadata:
  name: vault
  namespace: application
spec:
  provider:
    vault:
      server: http://vault.vault.svc:8200
      path: secret
      version: v2
      auth:
        kubernetes:
          mountPath: kubernetes
          role: ptah-application
          serviceAccountRef:
            name: vault-reader
            audiences: [vault]
```

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: application-database
  namespace: application
spec:
  refreshInterval: 1m
  secretStoreRef:
    name: vault
    kind: SecretStore
  target:
    name: application-database
    creationPolicy: Owner
  data:
    - secretKey: url
      remoteRef:
        key: ptah/application
        property: url
```

Wait for the Secret, then apply a resource that reads it. This one takes its
schema from a ConfigMap, so it needs no registry:

```sh
kubectl -n application wait --for=condition=Ready \
  externalsecret/application-database --timeout=2m
kubectl -n application create configmap application-schema \
  --from-file=schema.sql=examples/configmap-kustomize/db/schema.sql
```

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: application
  namespace: application
spec:
  target:
    engine: PostgreSQL
    coordinationKey: production/application-primary
    urlFrom:
      name: application-database
      key: url
  desired:
    configMap:
      name: application-schema
      items:
        - key: schema.sql
          path: schema.sql
  policy:
    apply: OnApproval
```

From there it is [First schema](../../start/first-schema/#it-stops-and-waits-for-you):
the resource reaches `AwaitingApproval`, `kubectl ptah approve` records the
decision and the plan runs. A `PtahMigration` names the Secret the same way, in
its own `spec.target.urlFrom`. In the run one read this same Secret, on the same
database, with `sharedRealm: true` on both resources and the migration's tables in
the schema's `spec.policy.exclude`.

### What each tool writes {#external-secret-stores-tools}

The operator read the Secret from either tool and behaved the same. The tools
differ in what they leave around it:

| | External Secrets Operator | Vault Secrets Operator |
| --- | --- | --- |
| Objects beside the Secret | `SecretStore`, `ExternalSecret` | `VaultConnection`, `VaultAuth`, `VaultStaticSecret` |
| Keys in the Secret | The ones you map: `url` | Every field at the path, plus `_raw`, which holds the whole response and so a second copy of the URL |
| Owner reference | The `ExternalSecret`, as controller | The `VaultStaticSecret`, not as controller |
| Added metadata | A `managed` and a `created-by` label, and a `data-hash` annotation | Four labels, no annotation |
| A changed value | Written in place: same Secret UID | Written in place: same Secret UID |
| The Secret deleted by hand | Written again within a second, under a new UID | Written again within a second, under a new UID |
| The source object deleted | The Secret goes with it | The Secret goes with it |

A new Secret UID changes nothing for the operator, which does not read the
Secret: the plan that was waiting stayed current and ran. Neither tool changes the
password in the database. Rotating it there is your step, or the store's.

If the store holds a username and a password rather than a URL,
`target.template` can build the URL. `urlquery` escapes what a password can hold;
a password holding `@`, `/`, `:`, `%` and `+` connected through this template:

```yaml
target:
  name: application-database
  creationPolicy: Owner
  template:
    engineVersion: v2
    data:
      url: 'postgres://{{ .username | urlquery }}:{{ .password | urlquery }}@db.example:5432/application'
```

A value the operator cannot use is reported when the operation that reads it runs:

| What the Secret holds | What the run reported |
| --- | --- |
| No key `url` | The Pod waits, as below, with `couldn't find key url in Secret` |
| A `/` in the password, not escaped | `Failed`, `OperationFailed`: `invalid_target: database URL is invalid` |
| A newline after the URL | The same |

### Before the Secret exists {#external-secret-stores-not-synced}

Created before the tool has written the Secret, a `PtahSchema` stands in `Observing`
with `Ready=False`, reason `OperationInProgress`, and `ReconciliationFailed=False`.
A `PtahMigration` stands in `Reading` with `Progressing=True`, reason
`OperationInProgress`. No condition and no Event on the resource names the Secret.
The operation Pod does:

```sh
kubectl -n application get pods
kubectl -n application describe pod <the Pod in CreateContainerConfigError>
```

Its container waits in `CreateContainerConfigError` with `secret
"application-database" not found`, and the Pod carries a `Failed` Warning Event
with the same words. When the Secret appears, the kubelet starts the container and
the operation goes on: in the run the Observe finished within 30 seconds of the
Secret being written, and nobody had to touch anything.

The Job still has its `spec.execution.activeDeadlineSeconds`. Past that, a
`PtahSchema` read reports `Failed`, `OperationFailed`: `read Observe result: ptah
runner result frame not found`, which does not name the Secret either, and tries
again after `spec.execution.failureRetryInterval`. An Apply in the same position,
approved while the Secret was gone, reports `ApplyOutcomeUnknown` with `apply result
is uncertain: ptah runner result frame not found`, though its container never
started. It observes the database, publishes the same plan again and asks for a new
approval. An Apply whose Secret returned before the deadline went on under the
approval it had.

### When the credential rotates {#external-secret-stores-rotation}

`PTAH_DB_URL` is a `secretKeyRef`, resolved once when the container starts. A
rotated Secret reaches the next operation and never a running one. A running
operation keeps the old URL for as long as it lives, and PostgreSQL checks a
password only when a connection opens, so what a rotation does to it depends on
whether it opens another one.

In the rows below, "rotated" means the password changed in the store and in the
database, on the same role, while `ALTER ROLE` left open sessions running. PostgreSQL
keeps one password per role, so the old one stops working for new connections at
that moment.

While a plan waits for approval, on a `PtahSchema` unless the row says otherwise:

| What changed | What the resource did |
| --- | --- |
| Rotated | Nothing. The plan kept its name, UID and fingerprint, also across a scheduled re-read. An approval created afterwards bound it, the Apply used the new password, and the schema reached `InSync`. A `PtahMigration`'s plan also stayed the same across a requested read, and ran to `InSync` |
| The Secret and the database disagree, because one moved first | Nothing until a read. The read fails: `Failed`, `OperationFailed`, `child_exit: ptah exited with code 2`, tried again after `failureRetryInterval`. `status.plan` and `ApprovalRequired=True` stay, while admission refuses an approval with `referenced schema is not awaiting approval`, and `kubectl ptah approve` waits five minutes and approves nothing. Once the two agree, the same plan under the same fingerprint is waiting again, and an approval binds it |
| The same, approved before a read noticed | The Apply reported `ApplyOutcomeUnknown` and ran no statement, and the approval was consumed. Reads failed until the two agreed. Then the same plan under the same fingerprint was waiting, and a new approval ran it to `InSync` |
| A new username | The username is part of the plan's target identity. A scheduled re-read publishes a plan with a new fingerprint, a `PlanStale` Warning names both plans, and an approval of the new plan ran it to `InSync`. An Apply approved before that read is refused by the runner, `database target identity changed after planning` in the Pod's log, and runs no statement. The resource reads `ApplyOutcomeUnknown`, then stays `Failed` as the row for a new username below describes. With the first username back in the store, the read passed and the same plan under the same fingerprint was waiting for a new approval. A `PtahMigration` in the same position records `Unknown` and `Blocked`, naming `target_binding_mismatch: database target identity changed after planning`, and ran no migration. A `PtahMigrationRunAcknowledgment` settled it, and a plan under the new username, with its own approval, reached `InSync` |
| `GRANT role TO role` on any role of the server | See the paragraph after the next table |

While an Apply runs:

| What happened | `PtahSchema`, one `CREATE INDEX` on 15 million rows | `PtahMigration`, a 75-second `pg_sleep` in the second of three files |
| --- | --- | --- |
| Rotated | The statement finished, the post-Apply read used the new password, `InSync` | `Failed` at the next file, on the password the Pod still held: `Migration 3 (Add C) failed and committed nothing; 2 migrations before it are recorded applied`. The history read found one pending and a new plan waited for a new approval, which ran it to `InSync`. With the sleep in the only file, the run ended `Applied` and `InSync` |
| Rotated, and the sessions killed with `pg_terminate_backend` | `ApplyOutcomeUnknown`, no index. The read-only observation republished the same plan under the same fingerprint, for a new approval | `Unknown`, SQLSTATE `57P01`, `Blocked`: Ptah could not reconnect to record what happened. The revision stayed dirty. It took a `PtahMigrationRunAcknowledgment`, then `ptah migrations repair`, then a new approval to reach `InSync` |
| The store moved, the database kept the old password | The statement finished. The post-Apply read failed and retried, with `Failed`, `OperationFailed`, until the database took the password, then `InSync` | `Applied`, every file, on the password the Pod held. The history read failed and retried until the database took the password, then `InSync` |
| A new username | The statement finished, the index exists, and the journal says `ApplyFinished`, `Succeeded`. The post-Apply read is bound to the plan's identity and refuses the new one: `Failed`, `OperationFailed`, `post-apply observation target does not match the applied plan`, retried for as long as the run left it, up to five minutes, with no sign of ending by itself. Deleted and created again under the new username, the resource read the database afresh and reached `InSync` with nothing to apply | `Applied`, every file, then `InSync`: a history read is not bound to the plan the run came from |

A plan also records a fingerprint of the database as Ptah found it, and on
PostgreSQL that fingerprint covers the role memberships of the whole server:
`pg_auth_members` is not per database. In the run, `CREATE ROLE`, `CREATE DATABASE`,
`GRANT ALL ON DATABASE`, `ALTER ROLE ... PASSWORD`, `... VALID UNTIL` and `... SET`
left a waiting plan alone. `GRANT unrelated_group TO unrelated`, on two roles with
no tie to the plan's database, replaced it: the next read published a plan with a
new fingerprint, and `REVOKE` brought the first one back.
An Apply approved after the `GRANT` and before that read reported
`ApplyOutcomeUnknown` and ran no statement, and the resource went back to
`AwaitingApproval` with the new plan. The reason is only in the Pod's log, `database
schema no longer matches the approved plan`, and the Job is deleted 24 hours
later. A rotation that creates a user and grants it a group role moves that
fingerprint for every plan waiting on the server, and so would a store that generates
users that way. No such store was run.

So, for a rotation on this path:

- Keep the username, and change the password.
- Rotate with no Apply running. `kubectl get ptahschema,ptahmigration` prints the
  phase, and `status.activeOperation.type` says which operation is out.
- Keep the store's refresh interval short. The disagreement between the Secret and
  the database lasts that long: a read recovers from it by itself, and an Apply
  caught in it costs another approval.
- Do not kill the sessions of a running migration, and do not move a resource to a
  new username while it has an approved plan. Those are the rows a person has to
  clean up after.

## Admission

## Admission

The manager's own writes pass through two independent fail-closed layers.

**Typed policies**, one per kind and verb, reject objects outside their narrow
structural form: Jobs, plan chunks, the ConfigMaps the manager creates -- a
plan projection an Apply mounts or a source snapshot -- schema plans and
migration plans, the deletion of a ConfigMap that is not a source snapshot,
plus a seventh policy over `PtahSchema` and `PtahMigration` updates.
One policy per kind avoids cross-type CEL assumptions. The Job policy admits a
Job that satisfies the schema shape or the migration shape and nothing else.
The policies are ordinary release objects, and an upgrade updates them in
place. The Job, schema plan and migration plan policies carry the release's
manager image and controller-state version as literals, so a Job or a plan
stamped by another manager release is refused. The chunk, projection and
snapshot policies carry neither: a chunk and its projection are bound to their
plan by name and owner, and the plan names the manager; a snapshot is bound to
its resource, and the controller-write webhook rebuilds its bytes from the
source. Nor does the policy over `PtahSchema` and `PtahMigration`
updates, which judges resources a person owns and no manager stamps.

**Webhooks** perform the stronger semantic check by reading the owning resource
and plan and reconstructing the exact expected object. Eight ship:
`mapproval`, `mmigrationapproval` and `mmigrationrunacknowledgment` on the
mutating side; `vapproval`, `vmigrationapproval`,
`vmigrationrunacknowledgment`, `vpodintent` and `vcontrollerwrite` on the
validating side. The two acknowledgment entries stamp and check the person who
settles a run a migration recorded as unresolved, the way the approval entries
stamp an approver.

**The manager's state** is guarded against everyone else by two more policies.
The first refuses a write to the `status` subresource of any kind in the
`operator.ptah.run` group from anyone but the manager's ServiceAccount, a
cluster administrator included. It names the subresource rather than the
kinds, so a kind added later is covered by the rule already there. Status is
what the controller and the Pod-intent and controller-write webhooks decide
from: the claims, the admission snapshots, the record of a run nobody
accounted for. The second refuses anyone but the manager a change to the
`operator.ptah.run/unresolved-run` annotation of an existing `PtahMigration`,
the copy of that record a restore that drops status keeps. The policy over
`PtahSchema` and `PtahMigration` updates lets the manager move that one
annotation on a migration, and nothing else it did not move before.

A Job carries an annotation envelope that admission checks as a set: eight
annotations, including the manager image, revision and controller-state
version, and two more on a schema Apply for the plan fingerprint and content
digest. A Job without the controller identity is refused on create and on
update.

The manager annotations record which manager built a Job, and bind nothing. A
manager release that keeps the execution binding adopts a Job its predecessor
dispatched, in either family, when it builds the same Job apart from that
identity: it takes the identity from the live Pod template and holds the
template to the admission snapshot the claim persisted before dispatch, so
what it takes from the Job is pinned by the claim rather than by the Job being
checked. A release that changed anything else in the Job cannot confirm it,
and settles a dispatched Apply as outcome unknown.

Upgrade compatibility is deliberately narrower than ordinary reconstruction.
After an upgrade durably retires an execution epoch, the replacement manager
may add only the cleanup TTL to an exact terminal Job retained from the
supported predecessor, and only when the operation name, UID, owner, envelope,
admission snapshot and retirement conditions all match. A dispatched
predecessor Apply is handled only from persisted outcome-unknown evidence: the
replacement waits for every exact-owner Pod and the Job's terminal condition,
then adds only the TTL. It never reads, replays or certifies the retired
result; fresh read-only observation stays mandatory.
