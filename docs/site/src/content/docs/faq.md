---
title: FAQ
description: Short answers to what the operator does, what it refuses, and which page to open next.
tableOfContents:
  minHeadingLevel: 2
  maxHeadingLevel: 2
faqVerdicts: true
searchAliases:
  - "UnsupportedEngine"
  - "approval rejected"
  - "nothing applies"
  - "tables still there"
  - "registry auth fails"
  - "status Unknown"
  - "digest refused"
  - "plan too large"
  - "install fails"
---

Questions about the Ptah CLI itself -- schema formats, migrations, dialects --
live in the [FAQ on the Ptah documentation site](https://docs.ptah.run/faq/).

## What it is, and what it is not {#g-scope}

### What does the operator actually do, and is it a second Ptah? {#what-the-operator-does}

No. It is a control plane that converges a database on a desired schema
published as an OCI artifact: it resolves the tag to a digest, verifies the
artifact, observes the database, publishes a plan, applies the approved plan,
and observes again. The operator does not contain Ptah; it runs one as a Job.

- [Overview](../)
- [Ptah compatibility](../support/ptah/)

### We use versioned migrations. Can I point `PtahSchema` at a migration directory so the operator runs `up`? {#ptahschema-and-migration-directory}

No. `PtahSchema` reconciles a desired schema. `PtahMigration` is the resource for
a migration directory: the operator plans the pending files, and they run under
the same exact-plan approval.

- [Run versioned migrations](../use/migrations/)

### How does the operator compare with SchemaHero? {#compare-schemahero}

Both declare a schema as Kubernetes resources. Ptah Operator binds an approval
to the state of the database and refuses to run when that state has moved.
SchemaHero 0.26.2 binds an approval to the text of the SQL and runs that text. It
also keeps that SQL in the cluster, while `PtahMigration` runs files from your
repository and the database records what ran. SchemaHero is quicker to install
and lists more engines.

- [Compared with SchemaHero](../start/compare-schemahero/)

### Does the operator create the database? {#operator-does-not-provision}

No. It needs a network route to the target and a namespace-local Secret holding
the URL selected by `spec.target.urlFrom`. The target can be a managed service, a
private endpoint, or a database outside Kubernetes. NetworkPolicy, TLS,
privileges, backups, and high availability stay with the platform owner. See
[Operations](../use/operations/#external-database-targets).

### Can I run this in production? {#is-it-production-ready}

The API is `v1alpha1` and the project calls itself an implementation preview
until its database end-to-end matrix is green and a release is published. Read
[Releases and provenance](../support/releases/) and [Ptah compatibility](../support/ptah/) before you decide, and treat
an unverified combination as untested.

### Ptah's documentation version selector does not change the operator docs. Is it broken? {#operator-version-selector}

No. The operator tracks its own releases, and its documentation is the authority
on what a given operator version supports. Ptah's selector does not select an
operator version.

## Installing and upgrading {#g-install}

### Installation fails because three values have no default. Why not ship defaults? {#three-values-with-no-default}

`image.digest`, `execution.executorImage`, and `execution.runnerImage` are
registry manifest digests, and `execution.ptahVersion` is the identity verified
from the executor image's own provenance. A default in `values.yaml` would let
the chart assign a version identity to a digest nobody measured. All four are
recorded on every plan, Job and applied status. The executor image and
`ptahVersion` are also bound into every plan fingerprint and approval; the
manager and runner digests are not, so an operator patch release keeps pending
approvals.

The chart a release publishes to its OCI registry from v0.2.10 installs with no
values: it carries the digests and the version that release was tested with, and
refuses half of an override. The error above comes from the chart in the source
tree or a release's chart asset.

- [Configuration](../use/configuration/#the-three-values-with-no-default)

### I upgraded the Ptah CLI. Do I upgrade the operator at the same time? {#upgrade-cli-and-operator-together}

Do not match version numbers: the two projects release independently, and the
CLI you use to build an artifact is a different question from the executor the
installation binds. Read the compatibility table, which separates a declared
promise, a verified measurement, and an untested combination.

- [Ptah compatibility](../support/ptah/)

### The compatibility table lists nothing for my combination. Does that mean it is incompatible? {#untested-is-not-incompatible}

No. Absent is neither supported nor refused: a combination no row mentions is
untested. A version with nothing verified carries `unverifiedReason` naming the
check that has not run.

- [Ptah compatibility](../support/ptah/#three-states-kept-apart)

### Which Kubernetes versions are supported, and how does the window move? {#kubernetes-version-window}

The supported minor window is defined in [Kubernetes support](../support/kubernetes/). A change adds the
new minor and removes the oldest one atomically, and only after the whole
real-cluster matrix succeeds.

- [Operations](../use/operations/#kubernetes-versions)

## Artifacts and the registry {#g-registry}

### Registry authentication fails even though the credentials are right. What is missing? {#registry-secret-authority}

Every registry-authentication Secret must include `registry: <host[:port]>`
matching the OCI client's effective request authority, which is not always what
you typed: a source under `oci://docker.io/...` needs `registry-1.docker.io`. A
missing, malformed, or mismatched authority stops the Job before any registry
request.

- [Operations](../use/operations/#mutable-tags-and-registry-outages)

### I moved the tag and my existing approval stopped working. Why? {#moved-tag-invalidates-approval}

Every reconciliation interval resolves the reference again, and a moved tag
clears dependent plan and applied evidence before repeating verification and
observation. An old approval cannot authorize a plan built from new bytes. An
approval the operator recorded before the interval ended is the exception: the
plan it names is applied first, and the reference is resolved again one
interval after the proof that follows that Apply. Pin a digest when you do not
want the reference to move.

- [Operations](../use/operations/#mutable-tags-and-registry-outages)

### My policy requires a pinned digest and a tag that resolved fine was still refused. Why? {#digest-pin-refuses-a-tag}

Native verification inspects the resolved digest, and `require_digest_pin` also
evaluates the reference you originally requested. A tag, or an implicit `latest`,
is refused even when it resolved; an explicit digest stays eligible. See
[Operations](../use/operations/#mutable-tags-and-registry-outages).

### The registry was unreachable and the status went `Unknown` instead of keeping the last good result. Why? {#registry-outage-fail-closed}

Registry failures are fail-closed. The last source, plan, and applied record stay
as historical evidence, but they are not a claim about the present, so
`ArtifactResolved` becomes `Unknown` with reason `RefreshFailed` and no Verify,
Observe, Plan, or Apply follows. After connectivity returns the operator resolves
and verifies again.

- [Operations](../use/operations/#mutable-tags-and-registry-outages)

## Plans and approvals {#g-approval}

An approval names one plan, and the reviewer reads the SQL rather than a hash.

### My approval was rejected. What does it have to name? {#approval-rejected}

An approval is an independent immutable resource that must select the schema name
and UID, the plan name and UID, and the plan fingerprint. Creation is rejected if
the plan is already stale, its storage is not committed, the policy ConfigMap
changed, the approval names a plan of another schema or a fingerprint the plan
does not carry, or the schema is not waiting for exactly one approval. Updates
cannot change `spec`; create a new approval for a new plan. See
[Exact-plan approvals](../use/approvals/).

### The reviewer can see the plan but not the SQL. What is missing? {#reviewer-cannot-read-sql}

Read access to the chunks. The plan resource carries immutable chunk names and
digests, and the SQL lives in the `PtahSchemaPlanChunk` objects it names. One
rule, `get` on `ptahschemaplanchunks` in the namespace, reads every plan there;
the chart's approver ClusterRole carries it, and
`examples/approver-plan-reader-role.yaml` grants it without the approval
verbs. A role that reads plans and not chunks shows the hashes and none of the
statements. Once granted, `kubectl ptah plan <schema> --current` reads it;
approving hashes without reading the SQL is not an independent review.

- [Exact-plan approvals](../use/approvals/)
- [Read a plan](../use/read-a-plan/#install)

### How do I see the SQL a plan holds? {#read-a-plan}

`kubectl ptah plan <schema> -n <namespace>` prints it, and `--applied` prints what
the last confirmed apply ran instead of what would run next. The plugin is
published with each release and installed once, and `plan` only reads: it reads every
chunk, checks each against the digest and size the plan records, joins them in
order and checks the whole document before printing a line. Decoding the chunks
by hand is not the supported way to read a plan, and it is wrong for
any plan larger than one chunk: the document is split by bytes, so a boundary can
fall inside a SQL string, a JSON escape or a multi-byte character.

- [Read a plan](../use/read-a-plan/#install)

### The operator recorded a plan and refuses to apply it. Why? {#plan-recorded-not-applied}

Destructive plans are disabled by default, and enabling them still requires an
approval bound to the exact plan bytes. A plan that changes privileges needs an
approval even under `apply: Always`. Check the condition reason: policy states
appear as `ApplyDisabled`, `DestructiveChangesDisabled`, `PrivilegeChanges`, or
`AwaitingApproval`.

- [Condition reasons](../troubleshoot/condition-reasons/)
- [Exact-plan approvals](../use/approvals/)

### My plan was rejected as too large. What is the limit? {#plan-too-large}

An executable plan is limited to 8 MiB including the trailing newline, and the
runner refuses a larger native plan before publication. Accepted bytes are stored
in immutable 512 KiB chunks that stay below the Kubernetes object-size limit
after encoding. Split the change across artifacts.

- [Operations](../use/operations/#plan-retention)

### Old `PtahSchemaPlan` objects are piling up. Do I clean them? {#old-plan-objects}

They are owned by the schema and stay until garbage collection removes them
with the owner, which happens as soon as the schema is deleted, so export any
an audit needs first. Only the exact UID and fingerprint in `status.plan` are
current, so read that rather than the newest object you find.

- [Operations](../use/operations/#plan-retention)

## When something stops {#g-failures}

Every refusal here is deliberate: the operator would rather stop than mutate on an uncertain reading.

### An apply failed and the operator will not retry it. Why not run the plan again? {#apply-outcome-unknown}

Once a mutating child may have been dispatched, a missing, timed-out, or
malformed outcome is `ApplyOutcomeUnknown`: whether the mutation happened is not
known. The controller preserves the immutable apply holder and permits only a
fresh observation, because a blind replay could perform the change twice. See
[Operations](../use/operations/#failure-recovery).

### Does the operator roll back a failed change? {#no-automatic-rollback}

No. There is no automatic rollback. Repair the desired artifact or the database
deliberately, and let the next observation produce a new plan.

- [Operations](../use/operations/#failure-recovery)

### I set `spec.suspend=true` and now another schema in the same database is blocked. Is that a bug? {#suspend-blocks-the-realm}

No. When an apply still needs convergence proof, suspension retains and renews
that operation's database-realm Lease, which blocks later mutations in the same
realm until the resource is resumed and the read-only proof completes, or until
it is deleted. Releasing the Lease early would let an intervening apply
contaminate the audit result.

- [Operations](../use/operations/#suspension-and-deletion)

### I deleted the `PtahSchema` and the tables are still there. Why no cleanup? {#deleting-never-drops-data}

No. Deleting a `PtahSchema` never runs SQL, and neither does suspending one. The
finalizer only observes an already active operation and releases coordination
safely; Kubernetes then garbage-collects plans and Jobs while database objects
stay untouched. Dropping data has to be a separate, deliberate action. See
[Operations](../use/operations/#suspension-and-deletion).

## Databases and access {#g-database}

### Ptah supports my database, and the operator reports `UnsupportedEngine`. Why? {#unsupported-engine}

The operator's support contract is narrower than the CLI's on purpose: an engine
is supported only when the whole lifecycle is green on every supported Kubernetes
minor, which today means PostgreSQL 17.x and 18.x and MySQL 8.4.x LTS. An unknown engine
value is stored without any database or registry access and sets `phase=Blocked`.
Changing the spec back to a supported engine restarts the workflow at Resolve.

- [Databases and privileges](../support/databases/)
- [Condition reasons](../troubleshoot/condition-reasons/#unsupported-engines)

### Two `PtahSchema` resources reach the same physical database through different URLs. What do I set? {#one-database-two-urls}

Set one stable `coordinationKey` for every URL alias that reaches the same
database. Coordination is keyed on that value, so distinct aliases without it
look like distinct databases and lose their mutual exclusion. The key is scoped
to the namespace: when the two resources live in different namespaces, ask an
administrator for a `PtahRealm` that lists both, and name it with
`spec.target.realmRef` in each.

- [Operations](../use/operations/#external-database-targets)

### Does the login need superuser? {#least-privilege-login}

No, and it should not have it. On PostgreSQL, make the login the owner of the
managed objects or a member of a dedicated no-login owner role, then grant only
connect, schema usage, and object creation. Widen to extensions, roles, or other
schemas only when the artifact manages those object kinds. That login still reads
what another role created, so its drift report names such a table, and it cannot
drop one unless it owns the database or the schema. List a table that should stay
in `spec.policy.exclude`.

- [Databases and privileges](../support/databases/)

### Can the controller read my database credentials? {#controller-and-credentials}

No. Database work runs in short-lived Jobs, the controller itself has no
permission to read database Secrets, and registry and database credentials are
kept apart from each other. Credentials never appear in status, Events, plan
resources, or command arguments.

- [Security model](../use/security/)

### Can I keep database credentials in Vault or a cloud secret manager? {#external-secret-store}

Yes. A tool writes the store's value into a Kubernetes Secret and
`spec.target.urlFrom` names that Secret. External Secrets Operator and the Vault
Secrets Operator were both run this way against Vault, with rotation measured.
No cloud secret manager has been run. The operator has no native integration, so
it holds no credential for the store.

- [Credentials from an external secret store](../reference/credentials-and-admission/#external-secret-stores)

## Reading status, and reporting {#g-status}

### What should my automation read to decide whether a schema is converged? {#reading-convergence-status}

Read the full condition tuple of `type`, `status`, and `reason`, and use
`observedGeneration` to tell whether it describes the current spec. Condition
messages are diagnostic text and may gain detail without an API version change;
the reasons are the stable interface.

- [Condition reasons](../troubleshoot/condition-reasons/)

### How do I wait until the operator has acted on my change? {#wait-for-the-operator}

`kubectl ptah wait schema/<name> --for in-sync -n <namespace>` reads the
resource until the operator reports it in sync, and gives up at `--timeout`;
`--for` also takes `approval`, `applied` and `suspended`. Apart from a plan
already waiting for an approval, it counts nothing that was already true when it
began. A condition that is `True` says what the
operator saw last, so a schema that was `InSync` before somebody changed its
database stays `InSync` until it reads the database again: ask for that read
with `kubectl ptah refresh schema/<name> --wait`. That read covers
[what the login can see](../support/databases/#what-the-login-cannot-see). A
change made with the plugin, an approval included, waits in the same command
with `--wait`.

- [Operations](../use/operations/#wait)

### I found a bug in the operator. Where does it go? {#reporting-an-operator-bug}

The operator is a separate repository, `github.com/stokaro/ptah-operator`, with
its own issues and releases, so a report filed against the Ptah CLI has to be
moved before anyone can act on it. Include the condition `type`, `status`, and
`reason`, the `status.observedGeneration`, and the manager and executor image
digests.
