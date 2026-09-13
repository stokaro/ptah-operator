---
title: FAQ
description: Short answers to what the operator does, what it refuses, and which page to open next.
tableOfContents: false
---

Italicized guide names become internal links at integration time. Questions
about the Ptah CLI itself live in the FAQ on the Ptah documentation site.

## What does the operator actually do, and is it a second Ptah? {#what-the-operator-does}

No. It is a control plane that converges a database on a desired schema
published as an OCI artifact: it resolves the tag to a digest, verifies the
artifact, observes the database, publishes a plan, applies the approved plan,
and observes again. The operator does not contain Ptah; it runs one as a Job.
See [Overview](../) and [Ptah compatibility](../support/ptah/).

## We use versioned migrations. Can I point `PtahSchema` at a migration directory so the operator runs `up`? {#ptahschema-and-migration-directory}

No. `PtahSchema` reconciles a desired schema, and no resource applies a migration
directory today. Run `ptah migrations up` against a pinned artifact from your own
pipeline or Job; that is a different delivery route, not different content for
`PtahSchema`.

## I deleted the `PtahSchema` and the tables are still there. Why no cleanup? {#deleting-never-drops-data}

Deleting a `PtahSchema` never runs SQL, and neither does suspending one. The
finalizer only observes an already active operation and releases coordination
safely; Kubernetes then garbage-collects plans and Jobs while database objects
stay untouched. Dropping data has to be a separate, deliberate action. See
[Operations](../use/operations/#suspension-and-deletion).

## I set `spec.suspend=true` and now another schema in the same database is blocked. Is that a bug? {#suspend-blocks-the-realm}

No. When an apply still needs convergence proof, suspension retains and renews
that operation's database-realm Lease, which blocks later mutations in the same
realm until the resource is resumed and the read-only proof completes, or until
it is deleted. Releasing the Lease early would let an intervening apply
contaminate the audit result. See [Operations](../use/operations/#suspension-and-deletion).

## Installation fails because three values have no default. Why not ship defaults? {#three-values-with-no-default}

`image.digest`, `execution.executorImage`, and `execution.runnerImage` are
registry manifest digests, and `execution.ptahVersion` is the identity verified
from the executor image's own provenance. A default would let the chart assign a
version identity to a digest nobody measured. All four are recorded in every
plan, approval, Job, and applied status. See [Configuration](../use/configuration/#the-three-values-with-no-default).

## I upgraded the Ptah CLI. Do I upgrade the operator at the same time? {#upgrade-cli-and-operator-together}

Do not match version numbers: the two projects release independently, and the
CLI you use to build an artifact is a different question from the executor the
installation binds. Read the compatibility table, which separates a declared
promise, a verified measurement, and an untested combination. See [Ptah compatibility](../support/ptah/).

## The compatibility table lists nothing for my combination. Does that mean it is incompatible? {#untested-is-not-incompatible}

No. Absent is neither supported nor refused: a combination no row mentions is
untested. A version with nothing verified carries `unverifiedReason` naming the
check that has not run. See [Ptah compatibility](../support/ptah/#three-states-kept-apart).

## Ptah supports my database, and the operator reports `UnsupportedEngine`. Why? {#unsupported-engine}

The operator's support contract is narrower than the CLI's on purpose: an engine
is supported only when the whole lifecycle is green on every supported Kubernetes
minor, which today means PostgreSQL 17.x and MySQL 8.4.x LTS. An unknown engine
value is stored without any database or registry access and sets `phase=Blocked`.
Changing the spec back to a supported engine restarts the workflow at Resolve.
See [Databases and privileges](../support/databases/) and [Condition reasons](../troubleshoot/condition-reasons/#unsupported-engines).

## Registry authentication fails even though the credentials are right. What is missing? {#registry-secret-authority}

Every registry-authentication Secret must include `registry: <host[:port]>`
matching the OCI client's effective request authority, which is not always what
you typed: a source under `oci://docker.io/...` needs `registry-1.docker.io`. A
missing, malformed, or mismatched authority stops the Job before any registry
request. See [Operations](../use/operations/#mutable-tags-and-registry-outages).

## I moved the tag and my existing approval stopped working. Why? {#moved-tag-invalidates-approval}

Every reconciliation interval resolves the reference again, and a moved tag
clears dependent plan and applied evidence before repeating verification and
observation. An old approval cannot authorize a plan built from new bytes. Pin a
digest when you do not want the reference to move. See [Operations](../use/operations/#mutable-tags-and-registry-outages).

## My policy requires a pinned digest and a tag that resolved fine was still refused. Why? {#digest-pin-refuses-a-tag}

Native verification inspects the resolved digest, and `require_digest_pin` also
evaluates the reference you originally requested. A tag, or an implicit `latest`,
is refused even when it resolved; an explicit digest stays eligible. See
[Operations](../use/operations/#mutable-tags-and-registry-outages).

## The registry was unreachable and the status went `Unknown` instead of keeping the last good result. Why? {#registry-outage-fail-closed}

Registry failures are fail-closed. The last source, plan, and applied record stay
as historical evidence, but they are not a claim about the present, so
`ArtifactResolved` becomes `Unknown` with reason `RefreshFailed` and no Verify,
Observe, Plan, or Apply follows. After connectivity returns the operator resolves
and verifies again. See [Operations](../use/operations/#mutable-tags-and-registry-outages).

## An apply failed and the operator will not retry it. Why not run the plan again? {#apply-outcome-unknown}

Once a mutating child may have been dispatched, a missing, timed-out, or
malformed outcome is `ApplyOutcomeUnknown`: whether the mutation happened is not
known. The controller preserves the immutable apply holder and permits only a
fresh observation, because a blind replay could perform the change twice. See
[Operations](../use/operations/#failure-recovery).

## Does the operator roll back a failed change? {#no-automatic-rollback}

No. There is no automatic rollback. Repair the desired artifact or the database
deliberately, and let the next observation produce a new plan. See [Operations](../use/operations/#failure-recovery).

## My approval was rejected. What does it have to name? {#approval-rejected}

An approval is an independent immutable resource that must select the schema name
and UID, the plan name and UID, and the plan fingerprint. Creation is rejected if
the plan is already stale, its storage is not committed, the policy ConfigMap
changed, a derived field conflicts, or the schema is not waiting for exactly one
approval. Updates cannot change `spec`; create a new approval for a new plan. See
[Exact-plan approvals](../use/approvals/).

## The reviewer can see the plan but not the SQL. Is that intended? {#reviewer-cannot-read-sql}

Yes. The plan resource carries immutable chunk names and digests, and the SQL
lives in controller-owned ConfigMaps that the built-in approver ClusterRole
deliberately cannot read. A namespace administrator grants `get` on the current
chunk names through a least-privilege Role, replaced for the next plan. Once
granted, `kubectl ptah plan <schema> --current` reads it; approving hashes
without reading the SQL is not an independent review. See
[Exact-plan approvals](../use/approvals/) and
[Read a plan](../use/read-a-plan/), which carries the
[install](../use/read-a-plan/#install).

## How do I see the SQL a plan holds? {#read-a-plan}

`kubectl ptah plan <schema> -n <namespace>` prints it, and `--applied` prints
what the last confirmed apply ran instead of what would run next. The plugin is
a read-only client published with each release and
[installed once](../use/read-a-plan/#install); it reads every chunk, checks
each against the digest and size the plan records, joins them in order and
checks the whole document before printing a line.

Decoding the chunk ConfigMaps by hand is not the supported way to read a plan
and is wrong for any plan larger than one chunk: the document is split by bytes,
so a boundary can fall inside a SQL string, a JSON escape or a multi-byte
character. See [Read a plan](../use/read-a-plan/).

## The operator recorded a plan and refuses to apply it. Why? {#plan-recorded-not-applied}

Destructive plans are disabled by default, and enabling them still requires an
approval bound to the exact plan bytes. Check the condition reason: policy states
appear as `ApplyDisabled`, `DestructiveChangesDisabled`, or `AwaitingApproval`.
See [Condition reasons](../troubleshoot/condition-reasons/) and [Exact-plan approvals](../use/approvals/).

## My plan was rejected as too large. What is the limit? {#plan-too-large}

An executable plan is limited to 8 MiB including the trailing newline, and the
runner refuses a larger native plan before publication. Accepted bytes are stored
in immutable 512 KiB ConfigMap chunks that stay below the Kubernetes object-size
limit after encoding. Split the change across artifacts. See [Operations](../use/operations/#plan-retention).

## Old `PtahSchemaPlan` objects are piling up. Do I clean them? {#old-plan-objects}

They are owned by the schema and can stay as audit evidence until garbage
collection removes the owner. Only the exact UID and fingerprint in `status.plan`
are current, so read that rather than the newest object you find. See [Operations](../use/operations/#plan-retention).

## Does the operator create the database? {#operator-does-not-provision}

No. It needs a network route to the target and a namespace-local Secret holding
the URL selected by `spec.target.urlFrom`. The target can be a managed service, a
private endpoint, or a database outside Kubernetes. NetworkPolicy, TLS,
privileges, backups, and high availability stay with the platform owner. See
[Operations](../use/operations/#external-database-targets).

## Two `PtahSchema` resources reach the same physical database through different URLs. What do I set? {#one-database-two-urls}

Set one stable `coordinationKey` for every URL alias that reaches the same
database. Coordination is keyed on that value, so distinct aliases without it
look like distinct databases and lose their mutual exclusion. See [Operations](../use/operations/#external-database-targets).

## Does the login need superuser? {#least-privilege-login}

No, and it should not have it. On PostgreSQL, make the login the owner of the
managed objects or a member of a dedicated no-login owner role, then grant only
connect, schema usage, object creation, and the catalog visibility needed to
inspect them. Widen to extensions, roles, or other schemas only when the artifact
manages those object kinds. See [Databases and privileges](../support/databases/).

## Can the controller read my database credentials? {#controller-and-credentials}

No. Database work runs in short-lived Jobs, the controller itself has no
permission to read database Secrets, and registry and database credentials are
kept apart from each other. Credentials never appear in status, Events, plan
resources, or command arguments. See [Security model](../use/security/).

## Which Kubernetes versions are supported, and how does the window move? {#kubernetes-version-window}

The supported minor window is defined in [Kubernetes support](../support/kubernetes/). A change adds the
new minor and removes the oldest one atomically, and only after the whole
real-cluster matrix succeeds. See [Operations](../use/operations/#kubernetes-versions).

## What should my automation read to decide whether a schema is converged? {#reading-convergence-status}

Read the full condition tuple of `type`, `status`, and `reason`, and use
`observedGeneration` to tell whether it describes the current spec. Condition
messages are diagnostic text and may gain detail without an API version change;
the reasons are the stable interface. See [Condition reasons](../troubleshoot/condition-reasons/).

## Ptah's documentation version selector does not change the operator docs. Is it broken? {#operator-version-selector}

No. The operator tracks its own releases, and its documentation is the authority
on what a given operator version supports. Ptah's selector does not select an
operator version.

## Can I run this in production? {#is-it-production-ready}

The API is `v1alpha1` and the project calls itself an implementation preview
until its database end-to-end matrix is green and a release is published. Read
[Releases and provenance](../support/releases/) and [Ptah compatibility](../support/ptah/) before you decide, and treat
an unverified combination as untested.

## I found a bug in the operator. Where does it go? {#reporting-an-operator-bug}

The operator is a separate repository, `github.com/stokaro/ptah-operator`, with
its own issues and releases, so a report filed against the Ptah CLI has to be
moved before anyone can act on it. Include the condition `type`, `status`, and
`reason`, the `status.observedGeneration`, and the manager and executor image
digests.
