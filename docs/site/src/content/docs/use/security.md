---
title: Security model
description: The trusted release namespace, the boundaries the operator holds, artifact integrity, and what a deployment still owes.
---

## The release namespace is part of the control plane {#release-namespace}

The namespace the chart is installed into is a privileged administrative
boundary and part of the operator's trusted computing base. So is
`coordination.namespace` when it names a different namespace. Everything else
on this page assumes that only Ptah administrators can act in either one, and
nothing the operator does replaces that assumption.

This follows from a Kubernetes rule, not from a choice this operator made.
Whoever can create a Pod in a namespace can run it as any ServiceAccount in
that namespace and mount any Secret there, so the right to create workloads in
a namespace carries every permission its ServiceAccounts hold. Kubernetes says
so in its
[RBAC good practices](https://kubernetes.io/docs/concepts/security/rbac-good-practices/#workload-creation),
and adds that boundaries inside a namespace should be considered weak. The
release namespace holds the ServiceAccounts of the manager, the certificate
rotator and the install hooks, and the Secret with the webhook's TLS key.
A principal that can create or modify Pods, Deployments, Jobs or any other
workload there, exec into a Pod, or request a ServiceAccount token, can act as
the controller. That principal is a Ptah administrator, whatever its Role is
called. The coordination namespace holds the Lease that elects the active
manager and the Leases that serialize every operation on a database, so a
principal that can write Leases there can stop reconciliation or take a
database's turn.

What that means for a deployment:

- Do not deploy application workloads into the release namespace or the
  coordination namespace.
- Do not grant namespace-admin, `edit`, or any right to create or modify
  workloads there to a principal you would not trust to administer Ptah. The
  same applies to `pods/exec`, `serviceaccounts/token` and Lease writes.
- Do not rely on a narrower Role inside either namespace as a security
  boundary. Kubernetes does not treat one as such, and neither does this
  operator.

A finding that starts from write access to either namespace describes an
administrator doing administration, and the
[security policy](https://github.com/stokaro/ptah-operator/blob/master/SECURITY.md)
lists it as out of scope.

### What the chart checks, and what it cannot see {#release-namespace-check}

When Helm can read the cluster, which is `helm install`, `helm upgrade` and a
dry run with `--dry-run=server`, the chart reads the release namespace before
it renders anything. It refuses the install or upgrade when:

- the namespace is `default` or starts with `kube-`, which the whole cluster
  shares;
- the namespace runs a Pod, ReplicationController, Deployment, StatefulSet,
  DaemonSet, ReplicaSet, Job or CronJob that does not carry this release's
  `app.kubernetes.io/instance` label. Every workload the chart creates, and
  every Pod those workloads start, carries it.

The refusal names the namespace and up to five of the workloads it found.
`releaseNamespace.allowSharedNamespace: true` turns it off. Set it only when
everyone who can create workloads in that namespace is trusted to administer
Ptah, because installing there makes them administrators.

The notes Helm prints after an install or upgrade warn, and refuse nothing,
about RoleBindings in the release namespace, and in a separate coordination
namespace, that give a subject other than this release's ServiceAccounts
`create` on `pods`, `pods/exec`, `serviceaccounts/token`, a workload kind or
`leases`, or that bind the `admin`, `edit` or `cluster-admin` ClusterRole.
This release's ServiceAccounts are the ones its Pods run as: the manager, the
certificate rotator, and the CRD hook. The chart knows them by the names it
gives them, so a binding the release made for a ServiceAccount it has not
created yet is still its own. A ClusterRole is judged by the rules it carries. For an
aggregated ClusterRole those are the rules the aggregation controller wrote
into it, so a grant that arrives through aggregation is warned about too.

The check catches plain mistakes. It is not the boundary, and the contract
above holds whether the check runs or not. It cannot see:

- ClusterRoleBindings, which grant the same rights in every namespace;
- an external authorizer, such as a webhook authorizer or a cloud IAM mapping;
- a grant made, or a workload deployed, after the install or upgrade it ran
  in;
- anything at all in a render that cannot read the cluster. `helm template`,
  `helm lint`, a dry run without `--dry-run=server`, and a GitOps tool that
  renders the chart with `helm template`, as Argo CD does, all get empty
  lookups, and the check lets them through without a word. An installation
  made that way has to hold to the contract with no check at all.

### Where the product boundary is {#product-boundary}

The boundary the operator is built to hold lies between it and the application
namespaces: the people who write `PtahSchema`, `PtahMigration` and approval
resources there, the database credentials those namespaces hold, and the
operation Pods that run there with those credentials. The authorities in
[Trust boundaries](#trust-boundaries) are that boundary, and the rest of this
page says how each is held. Realm membership is one of them: a claim on a
database reaches past its namespace only when a `PtahRealm` grants it, as
[Who may claim a database](#who-may-claim-a-database) describes. The open work
that hardens the rest:

- [#447](https://github.com/stokaro/ptah-operator/issues/447): let operation
  Pods carry bounded metadata for third-party mutating admission.
- [#449](https://github.com/stokaro/ptah-operator/issues/449): keep plan bytes
  out of Pod logs, which [Pod logs carry plans](#pod-logs-carry-plans)
  explains.
- [#450](https://github.com/stokaro/ptah-operator/issues/450): close the gaps
  at the human authority boundary.

### What the operator still checks about its own releases {#stale-predecessor}

Trusting the release namespace means trusting the people who administer it,
not every binary that has run there. A previous release that is stale or buggy
is an ordinary failure, and these checks exist for it:

- CRD schema identity. The CRD hook refuses a live schema version newer than
  the candidate's, and two digests under one version, so an older image cannot
  narrow a newer schema. See
  [Schema identity on a CRD](../../reference/release-lifecycle/#schema-identity-on-a-crd).
- The downgrade preflight. A manager does not start over stored state that a
  newer controller wrote. See
  [The downgrade preflight](../../reference/release-lifecycle/#the-downgrade-preflight).
- The admission singleton. One release per cluster, checked when the chart
  renders and again by the runtime verifier before a manager starts. See
  [What the runtime verifier requires](../../reference/release-lifecycle/#what-the-runtime-verifier-requires).
- `Recreate` and leader election. An old and a new manager never serve
  admission side by side, and only one of them reconciles. See
  [Install the operator](../operations/#install-before).
- The controller-write guards. Typed admission policies and a webhook that
  rebuilds the expected object bound what the manager itself may write, so a
  bug cannot create a Job, a plan, a chunk or a plan's projection outside its
  shape. See
  [Admission](../../reference/credentials-and-admission/#admission).

None of these is a defense against an administrator acting in bad faith, and
the release machinery does not try to be one: the contract above is what
answers that.

## Trust boundaries

The operator separates five authorities:

1. A desired-state author may change `PtahSchema` but cannot approve a plan
   merely by editing that resource. They can, however, make approvals
   unnecessary: `spec.policy.apply` is a field of the resource they own, and
   selecting `Always` applies a schema plan that destroys nothing and changes
   no privilege with no approval at all. RBAC cannot close that, because the
   bypass is not an approval. See
   [Who may turn the approval requirement off](#who-may-turn-the-approval-requirement-off).
2. An approver may read schemas, migrations, their plans and the chunks a
   schema plan's SQL is stored in, and create immutable approvals for either
   family, and the acknowledgment that settles a migration run nobody
   accounted for. The chart creates an optional
   ClusterRole but never binds it automatically. RBAC decides who may
   approve; it does not by itself decide that the approver is a second
   person rather than the author. See
   [Refusing a self-approval](#refusing-a-self-approval).
3. The controller may manage plans and their chunks, Jobs, Leases, status, and
   Events, and create the ConfigMaps an Apply mounts its plan through. Its
   default ClusterRole grants Secret CREATE only for operation-bound delivery
   credentials, guarded by admission; it grants no Secret reads. Disabling
   `resultDelivery.enabled` removes that CREATE grant. Typed admission policies
   that ship with the release constrain its main-resource writes to structural
   Job, immutable plan, immutable chunk and immutable projection shapes; a
   fail-closed webhook then reconstructs and compares the complete write
   intent through direct API reads. Nobody else writes status: a policy the
   chart installs refuses a write to the status subresource of every operator
   kind from any identity but the manager's ServiceAccount, a cluster
   administrator included, and another refuses anyone else a change to the copy
   of a migration's unresolved-run record on its metadata. A person settles such
   a run with a `PtahMigrationRunAcknowledgment`, which admission stamps with
   who made it and the migration records.
4. A Job receives only the credentials needed for its fixed operation through
   same-namespace Secret selectors resolved by the kubelet.
5. A realm administrator decides which namespaces may manage a database more
   than one namespace reaches, by writing a cluster-scoped `PtahRealm`. An
   author names a realm and cannot grant it. See
   [Who may claim a database](#who-may-claim-a-database).

### Who may turn the approval requirement off {#who-may-turn-the-approval-requirement-off}

The separation above is about who may write an approval. It says nothing about
who may decide one is not needed, and those are different questions with
different answers.

`spec.policy.apply` lives on the desired-state resource. An author with the
rights the example Role grants may set it to `Always`, after which schema plans
that destroy nothing and change no privilege apply without an approval;
`PtahMigration` exposes the same field, and there every plan applies. Separating
the approver Role from the author Role does not prevent this, and no amount of
RBAC on approval objects will, because nothing is approving anything.

What `Always` does not waive is a change of authority. A schema plan that
grants or revokes a privilege, changes a role or its members, hands an object
to another owner, creates, alters or drops a row-security policy, makes code
run with its owner's rights, or changes the rights an existing PostgreSQL view
reads with waits for an approval under `Always` as under
`OnApproval`, and a destructive plan still needs `allowDestructive` as well.
Whoever can push the artifact's tag can still change the schema unattended; the
changes of authority the operator can read in it wait for a person. The
operator decides this from the SQL rather than from Ptah's severity, and
[Plans that change privileges](../../reference/plans-and-approvals/#privilege-changes)
lists what it reads and what it cannot see: rights an engine grants by default,
such as a view reading with its owner's rights, SQL built at run time, and every
`PtahMigration`.

That reading is of keywords, and it is a filter rather than a boundary. The
boundary is the database login an Apply runs as: an Apply can do what that
login can do and nothing more, so a login that cannot grant, create roles or
own other roles' objects keeps those out of every plan, read or not. Keep it to
what the artifact manages, as
[Databases and privileges](../../support/databases/#postgresql-authority) and
[Does the login need superuser?](../../faq/#least-privilege-login) describe.

`Always` is not wrong. It is the deliberate unattended mode, and an
installation that wants it should have it. What matters is that choosing it is
a decision someone made on purpose, rather than a default an author can reach
without anyone else noticing.

The chart ships the guard that makes it one, on by default. `applyPolicyGuard`
renders a `ValidatingAdmissionPolicy` and its binding, cluster-scoped and owned
by the release, so an author with complete rights over resources in their own
namespace cannot edit, rebind or delete it. It covers both kinds and both
`CREATE` and `UPDATE`: a guard that watched only updates is bypassed by
creating the resource with `Always` already set. The refusal names the field
and says what to do instead: use `OnApproval`, or ask an apply-policy
administrator to make the change.

Who counts as one is `applyPolicyGuard.exemptGroups`, the groups whose members
may select `Always`. The default is `system:masters` alone. It is the one group
Kubernetes itself puts beyond authorization, and its members could delete the
policy anyway, so exempting them opens nothing and keeps a way to choose
`Always` that needs no Helm upgrade. No other group is guessed: `cluster-admin`
is a ClusterRole bound to whichever groups a distribution chose, and a chart
that named one of them would exempt a different set of people on every
distribution. Add the group that owns apply policy in your cluster, and the
group of a GitOps controller that creates resources with `Always` (for a
ServiceAccount, its `system:serviceaccounts:<namespace>` group). The chart
refuses `system:authenticated` there, because exempting every authenticated
identity is turning the guard off, and that is `applyPolicyGuard.enabled:
false`, said openly. An installation that wants unattended apply for everyone
sets it, and this section then does not apply to it.

What the guard refuses is the transition into `Always`, not the value itself.
That distinction is load-bearing. The operator patches these resources to add
and remove its operation finalizer, and its service account is not exempt, so a
guard that refused every write leaving `Always` in place would stop operations
from starting and stop a finished one from releasing its finalizer -- an
administrator who chose `Always` would have wedged every resource they chose it
for. Leaving the field where an administrator put it is permitted, so the
resource's owner keeps editing everything else in it, and so is moving back to
`OnApproval`; arriving at `Always` from anywhere else is not. Upgrading an
installation whose resources already apply with `Always` changes nothing for
them: they keep applying and their owners keep editing them. What changes is
who may create the next one.

The binding covers every namespace, and its `validationActions` is `Deny`;
`Warn` and `Audit` would record the bypass rather than refuse it. The envtest
suite under `test/envtest/admissionpolicy` holds the installed policy to each
request above, and the acceptance suite holds it to an impersonated author and
administrator on a cluster.

Start namespace-scoped bindings from the
desired-state author (`examples/desired-state-author-role.yaml`) and
diagnostic reader (`examples/diagnostic-reader-role.yaml`) examples. The
chart's optional approver ClusterRole stays unbound unless
`approverClusterRole.subjects` names someone, so these three human
permission sets can be assigned to different identities. Diagnostic access
deliberately excludes Secrets, plan chunks and ConfigMaps: the chunks are a
plan's SQL, and a ConfigMap may be the copy of it an Apply mounted. It reads
the plan manifests, which carry the digests, the statement count and the
privilege kinds, and operation Pod logs, since
[Plan results stay out of logs](#pod-logs-carry-plans). Those logs can carry row
values when an Apply fails, as [Output handling](#output-handling) says, so grant
`pods/log` as you would read access to the rows. Grant the SQL to
reviewers, with the approver role or `examples/approver-plan-reader-role.yaml`.

These write boundaries reduce the effect of controller bugs and prevent its
RBAC from becoming arbitrary workload or ConfigMap creation authority. The one
ConfigMap the manager may create is an immutable copy of a chunk, owned by its
plan, written while an Apply of that plan is being dispatched and holding
exactly the bytes the plan records. The manager binary and its status-write
authority remain trusted: the admission layers do not claim to contain a
malicious replacement image that can forge the status records used to
reconstruct intent.

Database-operation Pods disable service-account token mounting and service-link
environment injection. Every container runs as non-root with a read-only root
filesystem, `RuntimeDefault` seccomp, no Linux capabilities, no privilege
escalation, a deadline, bounded memory-backed work volumes, and no automatic
Job retry.

Before dispatch, the controller persists a canonical, credential-free snapshot
of the built-in Kubernetes mutations that may affect an operation Pod. It reads
ServiceAccount names and image-pull-secret references, LimitRange quantities,
RuntimeClass scheduling and overhead, and PriorityClass values, but never reads
the referenced Secret data. A fail-closed validating webhook checks the final
post-mutation Pod before scheduling. Only the exact snapshotted mutations are
accepted; executable, environment, volume, and security fields remain exact.
The controller retains read-only Pod evidence permissions and is not granted
Pod create or delete permission.

Operation Pod creation is additionally bound to the built-in Kubernetes Job
controller and to the API server's generated-name chain. The submitted
`generateName` must equal the exact Job name plus `-`; the concrete Pod name
must contain the API server's at-most-58-character effective prefix and one
five-character lowercase alphanumeric suffix. The reconciler repeats this
check before trusting terminal Pod evidence. Exact Job tracking-finalizer
removal is restricted to the same controller identity and cannot carry any
other Pod mutation.

Because the manager watches `PtahSchema` across namespaces and each operation
runs in its resource's namespace, the controller has cluster-wide `get` on ServiceAccounts
and `list` on LimitRanges. It has no ServiceAccount `list` or `watch`, no
LimitRange `get` or `watch`, and no write verb for either resource. This is the
minimum Kubernetes RBAC shape that permits resolving an arbitrary named
ServiceAccount and the namespace-wide LimitRange admission set without reading
Secret data.

Memory-backed `emptyDir` usage is charged to the writing container by
Kubernetes. The chart defaults bound each volume, but production resource
limits must also leave headroom for the runner binary, fetched schema, plan,
and client scratch data in addition to the process heap.

### Refusing a self-approval {#refusing-a-self-approval}

An approval proves an authenticated approver. On its own it proves nothing
about that approver being a second person, so an author with a Role broad
enough to also approve -- or a deployment that never separated the two Roles
at all -- could approve their own plan. `approvals.requireDistinctApprover`
closes that gap. It is a chart value, owned by whoever installs the operator,
and read once into the manager's own flag; no field on `PtahSchema` or
`PtahMigration` can reach it. It defaults to false, so an existing installation
that never sets it keeps admitting the approvals it always did.

The switch was a per-resource field until this control's first release exposed
what that placement could not bind: `spec.policy.requireDistinctApprover` lived
in the same spec the check judged, so an author who could also approve could
turn it off, let that edit retire the current plan and its binding, wait for
the operator to replan against the new spec, and approve the identical plan as
themselves, with the resource carrying no record that the control was ever
off. A guard that the party it constrains can also switch off is not a guard.
Moving it into the chart is the same move #450 already made for the other
approval control, [the apply-mode
guard](#who-may-turn-the-approval-requirement-off): put the switch where the
author's own RBAC over their namespace cannot reach it.

A mutating webhook on `PtahSchema` and `PtahMigration` records the
authenticated identity behind every `CREATE` and every `UPDATE` that changes
`spec` other than `spec.suspend`, in two annotations only that webhook ever writes: it overwrites
whatever a request carried for them, the same way the approval webhook
overwrites a forged `spec.approver`, so an author cannot name someone else as
the resource's writer any more than they can name someone else as the
approver. An update that leaves `spec` alone -- a label, the finalizer the
controller itself adds and removes, a status write, which is a separate
subresource the annotations cannot reach at all -- leaves the recorded
identity untouched. When the installation's flag is true, the approval webhook
refuses an approval whose approver is exactly that identity, or whose resource
carries no recorded identity at all.

The `operator.ptah.run/settled-apply` annotation is such an update, so nothing
records who wrote it except the API server's audit log, if its policy records writes
to `PtahSchema`. `patch` on `ptahschemas` alone can use it to [end a proof whose target
moved](../recovery/#a-schema-whose-target-moved) and release the database realm. It
is not the same power as deleting the resource, which takes `delete`, and patching the
target does not end a proof. Under `apply: Always` the next plan then applies with no
approval unless it is destructive or changes privileges.

The chart installs that mutating webhook's two entries under the same flag,
rather than leaving them bound whatever the flag says. An entry with
`failurePolicy: Fail` refuses every `PtahSchema` and `PtahMigration` write
while the manager is unreachable, including across an upgrade under a
`Recreate` strategy; with the switch off, which is the default, nothing reads
the recorded identity, so paying that coupling on every write buys nothing.
One consequence follows directly: a resource written while the switch was off
carries no recorded writer, and turning the switch on does not retroactively
supply one. Its approvals are refused, by the same "no spec writer is
recorded" reason a missing annotation always produces, until its spec is next
changed and the now-installed webhook has a write to stamp. Suspending or
resuming it is not such a change: the webhook does not count `spec.suspend`.
An installer turning this on should expect that gap, not read it as the
control misbehaving.

The record is metadata rather than `spec` or `status` on purpose. `status` is
a subresource on both kinds, and the API server resets it to its previous
value on every write through the main resource and drops it outright on
create, so a mutating webhook on the main resource can never persist anything
there. `spec` is the author's own field to write; recording the mutation
there would tie the two together in the one place meant to hold only their
own intent, which is exactly the placement this control moved away from.

Group membership plays no part in the comparison, and a ServiceAccount is one
identity like any other: the same ServiceAccount that wrote the spec and later
submits the approval is refused exactly as a person doing both would be. A
GitOps controller that writes every schema and migration spec makes every
human approver distinct by construction, because no human ever appears as the
recorded writer; this control is aimed at a human editing spec directly, and
adds nothing where one already runs. What the check compares is exactly what
the cluster's authenticator put on the request -- a username, and a UID where
the authenticator supplies one -- and that is also its limit. Impersonation,
or a credential more than one person holds, is indistinguishable at that layer
from two different people, and this control cannot see past it: identity here
is only as good as the cluster's own authentication.

And because the switch lives in the chart rather than in any namespace's
resources, it does not stop an author who can also edit or upgrade this Helm
release: that is authority over the installation itself, wider than authority
over one namespace's schemas, and no admission control this operator ships
reaches it. Closing that gap is RBAC over Helm releases and over the
operator's own namespace, not anything the operator can enforce from inside a
cluster it does not administer. Where any of this matters, pair the control
with the RBAC separation in
[Who may turn the approval requirement off](#who-may-turn-the-approval-requirement-off)
rather than relying on either alone.

### Who may claim a database {#who-may-claim-a-database}

Which resources count as managing one database decides who can refuse whom:
the operator refuses every claimant of a realm more than one resource claims
without agreeing to share it. A claim anybody could make by writing a string
would let a principal who can create a `PtahSchema` in one namespace, with no
Secret and no database access, stop every resource of another namespace and
learn that the key was in use.

So a claim reaches only as far as something grants it. A
`spec.target.coordinationKey` is scoped to its resource's namespace: the realm
is the engine, the namespace and the key, and the same key elsewhere is another
realm and another Lease. A claim across namespaces is made with
`spec.target.realmRef`, and the `PtahRealm` it names lists the namespaces it
admits. A resource elsewhere is refused with reason `RealmNotAuthorized`, runs
nothing, and is left out of the census of the admitted resources, so the
refusal falls on the resource that made the claim and on nobody else.

The boundary holds only while authors cannot write realms. Realms are
cluster-scoped, so no namespaced Role grants them; grant them only through a
ClusterRole such as
[`examples/realm-administrator-role.yaml`](https://github.com/stokaro/ptah-operator/blob/master/examples/realm-administrator-role.yaml),
and do not aggregate `ptahrealms` into the built-in `edit` or `admin` roles.
The manager reads realms and holds no write verb on them. A realm lists
namespaces by name rather than by selector, because a namespace's labels are
often writable by whoever administers that namespace.

Visibility follows the same line. A realm names the namespaces it admits, so
it is for administrators, and the operator writes no status into it. A
resource's own status names the realm it asked for, its own namespace and
counts of claimants, never another namespace or resource. A refused resource
reads the same message whether the realm is missing, does not list its
namespace or names another engine, so a tenant cannot probe for realms it may
not use.

Listing a namespace in a realm is trusting it with the database. A listed
namespace can still contest the realm by creating a second claimant, as any
claimant can; what a realm withholds is that power from the namespaces it
does not list.

The grant is to a namespace name, not to a namespace object, so it passes to
whoever holds that name. A name listed before its namespace exists, or left
listed after the namespace is deleted, goes to whoever creates the namespace
next. Where tenants choose their own namespace names -- OpenShift project
self-provisioning, Capsule, HNC subnamespaces -- that is any tenant, and the
denial of service a realm exists to prevent comes back through the grant
itself. Remove a namespace from every realm before deleting it, and do not
list a name that does not exist yet on a cluster where tenants can create
namespaces.

## OCI integrity and identity

The operator always resolves a tag to SHA-256 content and records that digest.
It verifies policy output against the resolved digest, inspects the pinned
artifact independently, and requires the Ptah schema artifact type. These
checks provide content integrity and prevent artifact-type confusion.
Verification policy ConfigMaps must be immutable. Plans, approvals, active
operations, and post-Apply proof bind both the ConfigMap UID and exact policy
digest, so deleting and recreating the same name cannot preserve authority.

The current Ptah verification policy can require a signature artifact to be
attached, but that requirement is a presence check. It does not validate a
cryptographic signature, key, certificate identity, issuer, or transparency
log. Do not describe `ArtifactVerified=True` as publisher authenticity when a
policy relies on that field.

For production publisher identity, verify the digest cryptographically in the
artifact promotion pipeline or enforce an OCI admission/promotion policy before
the digest is referenced by `PtahSchema`. Keep the resource digest-pinned after
that decision. A later operator API may add an independently versioned verifier
contract; it must bind its verifier image, trust policy bytes, and evidence into
the plan rather than executing arbitrary user commands.

Plain HTTP registry transport is an explicit opt-in for trusted test or
air-gapped networks. Sending a registry Secret over that transport additionally
requires the Secret owner to set the fixed `allowPlainHTTP` key to exactly
`true`; a schema author cannot authorize that downgrade alone. Plain HTTP cannot
be combined with a custom CA.

Every registry credential Secret must contain a non-optional fixed `registry`
key, whether it uses environment keys or Docker config JSON. Its authority-only
`host[:port]` value must exactly match the OCI client's effective request
authority before any Ptah process or network request starts. This includes the
request-host mapping applied by the OCI client; host case is normalized, but
ports and trailing dots are not collapsed. The key name cannot be selected by
a schema author. Docker config host entries and helpers still select the actual
credential, while the separate fixed key proves the Secret owner's consent to
one effective authority without exposing that configuration to the guard.
Observe and Plan run the credential-free authority guard between runner
installation and the credentialed schema fetch. Registry credentials and
database credentials remain routed to different containers.

Ptah never consumes custom CA bytes directly from the mutable ConfigMap volume.
Resolve and Verify copy at most 1 MiB into a private runner snapshot before the
first Ptah child starts. Observe and Plan make the credential-free authority
guard hash and copy the selected bytes into a dedicated memory-backed EmptyDir;
the later credentialed fetch mounts only that read-only snapshot. When registry
authentication is configured, the same authentication Secret must contain the
fixed `caSHA256` key with the exact lowercase `sha256:<64 hex>` digest of the
selected ConfigMap bytes. A missing, malformed, or mismatched grant stops the
Job before any Ptah process or network request. The key name cannot be selected
by a schema author. Anonymous registry access may use a custom CA without a
Secret grant, but it is still size-bounded and snapshotted before use.

A registry that requires a client certificate is not supported, and the API
has no field for one. The pinned executor loads a client pair into a
process-wide TLS configuration and cannot constrain certificate selection after
a cross-host redirect. A client-certificate transport needs an executor
contract that selects the certificate against the effective TLS authority on
every handshake.

## ConfigMap sources

A source ConfigMap is trusted by Kubernetes RBAC on its namespace, as the
resource that names it is: whoever may write the ConfigMap may change the
desired state, and so what an approval is asked for. Grant writes to it as you
grant writes to the resource. It has no verification policy, and the API
refuses one beside it rather than accepting a gate nothing evaluates.

The manager copies the selected files into a snapshot ConfigMap in the same
namespace. Whoever may read ConfigMaps there could already read the source, so
the snapshot adds no reader, except where a role grants the source ConfigMap
by name alone. The controller-write webhook admits the snapshot only as it
rebuilds it from the source that moment: the selected files of the ConfigMap
the owning resource names, owned by that resource. The manager's RBAC grants
`delete` on ConfigMaps for one purpose, removing a snapshot nothing names, and
an admission policy refuses the manager the deletion of any ConfigMap that is
not a snapshot.

An operation Pod checks the snapshot against the content digest the operation
was bound to before the container holding the database URL starts, so a
snapshot replaced after a plan was approved is refused rather than read.

## Git sources

A Git source is trusted by whoever may push to the ref it names: they may
change the desired state, and so what an approval is asked for, as whoever may
write a source ConfigMap may. Name a branch only where pushes to it are
reviewed, or name a tag or a commit. A commit holds only on a server that says it
gives commits by id; on any other it is refused once its branch moves on, as
[A Git source](../../reference/reconciliation/#a-git-source) says, and a tag is
the pin that holds. It has no verification policy, and the
API refuses one beside it.

The manager never reads the repository or the credential Secret. The Secret
names its own scope in fixed keys a resource's author cannot choose: `host`,
the only `host[:port]` the credential is sent to, and `caSHA256` or
`knownHostsSHA256`, the digests of the trust it may be sent under. A resource
that names the Secret with another URL, CA or known-hosts file is refused in
the Pod before Ptah starts. A credential goes in the Secret, never in the URL,
which the API refuses with one. Over `ssh://` the known hosts are required:
the fetch never trusts a host key on first use.

What a fetch reads is bounded: one commit, fetched shallowly; the selected path
only, at most 1,000 files and 64 MiB; no submodule, symbolic link or LFS
pointer, each refused by name; no checkout, hook or filter, so a repository
cannot run anything in the Pod.

## Source policies

A team that wants to bound what its ConfigMap and Git sources may name sets
`sourcePolicyFrom` on the resource: an immutable ConfigMap holding a YAML
document with up to three rules.

```yaml
allowedGitHosts:          # the only host[:port] a Git source may name
  - git.example.com
allowedConfigMaps:        # the only ConfigMaps a ConfigMap source may name
  - inventory-schema
maxContentBytes: 1048576  # the most the selected files may hold together
```

A rule that is absent refuses nothing, and a list that is present and empty
allows nothing of its kind. A field the document does not define is refused,
so a misspelled rule is not read as one kept. The manager reads the policy,
never the source's credential: a Git host it does not allow gets no Resolve
Job, so the credential is never sent there; a ConfigMap it does not allow, or
files that hold too much, leave no snapshot; a Git commit whose selected files
hold too much is not recorded. The refusal names the rule.

A plan binds the policy's object and bytes beside the source. Another policy,
the same bytes in another ConfigMap included, retires the plan, and so does
naming a policy where none was or dropping one; approval and Apply check the
live policy again. The API refuses `sourcePolicyFrom` beside `ociRef`: an OCI
source has its verification policy.

## Plan and approval visibility

Plan chunks contain schema-changing SQL, not credentials. They are
intentionally inspectable by independently authorized approvers, but arbitrary
schema names, defaults, comments, and literals may still be sensitive. Restrict
`PtahSchemaPlanChunk` read access in application namespaces accordingly. One
rule on that kind reads every plan in the namespace, and the built-in approver
role carries it, as described in [Exact-plan approvals](../approvals/). What
that access is used with is `kubectl ptah`. Its read commands only read: they
need `get` on the schema, the plan and its chunks and nothing else, and list
approvals only where `list` on them is granted. `kubectl ptah approve` creates
an approval as the person who runs it, and needs `create` on the approval kinds
as well. `kubectl ptah suspend`, `resume`, `refresh` and `set-source` patch the
resource as the person who runs them, and need `patch` on it, which no field
narrows: an identity that may suspend a resource or point it at another
artifact may edit the rest of its spec too, so grant it with the desired-state
author's verbs and not with the approver's;
[Read a plan](../read-a-plan/) carries the Role and the
[install](../read-a-plan/#install).

A plan that reached an Apply has one more copy. The Apply Pod holds no
Kubernetes API credential and the kubelet mounts no custom resource, so the
operator writes the plan into immutable ConfigMaps of its chunks' names, owned
by the plan, just before it creates the Apply Job, and they are deleted with
the plan. Whoever may read ConfigMaps in the namespace reads the plans that
were applied there. A plan that is waiting for a person, or that was never
applied, has no ConfigMap.

### Plan results stay out of logs {#pod-logs-carry-plans}

The default runner sends its result over TLS with a Pod-bound token whose sole
audience is `operator.ptah.run/results`.
The exact plan is stored in confidential `PtahResultRecord` payloads before
acknowledgment, then independently validated and published into immutable plan
chunks. Neither SQL nor reusable delivery credentials belong in diagnostic
logs, Events, or ordinary status. A failed Apply's Pod log holds one line of what the
database answered, with the text the statement holds replaced, as
[Output handling](#output-handling) says. Access to result records exposes plans; old development credential records
can also contain private delivery keys. Exclude result records from ordinary
reader and author roles and cover them with encryption at rest and restricted
backups. The diagnostic reader example grants no result-record access.

Kubelet rotates the explicit projected token. Automatic API-token mounting stays
disabled, and the runner receives no API permissions. The receiver audience must
not be configured as an accepted Kubernetes API audience. Admission keeps the
projection out of helper containers. The receiver verifies TokenReview, the
immutable original-Pod binding, and the live operation; a body that claims
another resource, Job or Pod cannot acquire that identity. No per-operation
Secret is created. A manager restart
does not discard the stored result or require its original process key.

When `resultDelivery.enabled=false`, legacy Jobs instead write one frame to
stdout. A Plan payload in that frame is sealed to a manager process key;
logs hold ciphertext. Loss of that key can require another read-only Plan.
This legacy transport is outside the stable 0.2.0 qualification profile. The manager
keeps its grant on result records in that mode, to retire and collect what an earlier
setting left, and admission lets it create a retirement and no other record.

Migration run error text that can disclose database values is removed from the
structured result in both transports. Failure reports name the outcome and
affected versions, not the database's free-text description. Its first line stays
in the Pod log, as [Output handling](#output-handling) says.

Approval admission fails closed. It binds names to UIDs, rejects a plan whose
storage commit is incomplete, rejects changed policy bytes or target state, and
makes the stamped decision immutable. Both approval webhook configurations use
the non-configurable `Fail` policy, so an unavailable webhook cannot admit a
caller-supplied identity stamp. Ordinary Kubernetes RBAC is not treated as
field-level authorization.

## The approver's name in the target database {#approver-in-the-database}

A `PtahMigration` Apply writes who approved it to the database it migrates. The
value is the username the API server authenticated when the approval was
created, the one admission stamped in `spec.approver.username`, and it lands in
`schema_migrations_log.actor` on every row the run writes. It is not a secret.
It is personal data whenever it names a person, and an OIDC username is often an
email address.

That copy leaves the cluster, and Kubernetes RBAC stops applying to it. Anyone
who can read that table reads the name, and backups, replicas and dumps of the
database carry it. The operator never deletes a row and has no setting that
turns the write off, so retention and erasure belong to whoever owns the
database. Editing or removing a row with SQL is safe for the operator and for
Ptah: neither reads this table to decide anything, and `ptah migrations status`
stays the answer to where the database stands. Choose the claim your
authenticator uses as the username with this in mind.

Inside the cluster the same encoded value sits in
`status.activeOperation.actor` while the run is claimed, and in the `PTAH_ACTOR`
variable of the Apply Job's executor container, so whoever may read Jobs or Pods
in the namespace reads it until the Job is removed. It reaches the executor
through that variable alone and is never on a command line. Ptah prints it on
neither stdout nor stderr on a run measured with v0.18.0, and the operator puts
it in no condition, Event or metric.

**What the record is worth.** `actor_source` reads `provided`, Ptah's word for a
name a caller supplied and Ptah did not check. The operator supplies what
admission stamped, but nothing in the database shows that: someone who runs
`ptah migrations up --actor` writes the same source, and the table is an ordinary
table that the account running migrations can write. It answers who approved a
migration after the cluster is gone. It does not prove it against someone who
holds that account. An audit that needs proof also keeps the API server's audit
log outside the cluster, if its audit policy records writes to these kinds: that
is where each approval's creation and who made it would be.

**A run nobody approved.** Under `spec.policy.apply: Always` the value is
`system:ptah-operator:apply-always:<namespace>/<name>`, which names no person.
Admission refuses a migration approval from a username under
`system:ptah-operator:`, so `system:ptah-operator:apply-always:` is how a reader
tells such a run from an approved one, since the source reads `provided` for
both. Match that whole prefix. A ServiceAccount that approves reads
`system:serviceaccount:...`, so `LIKE 'system:%'` would count approved runs as
unapproved.

**How the name is stored.** The operator cannot know the target database's
character set when it writes the claim, so it percent-encodes the name first:
every byte outside `0x20`-`0x7E`, and `%` and `$`, become `%XX`. `$` is there
because the kubelet expands `$(VAR)` in a container's environment, so a username
could otherwise grow past the column on its way to the process. Spaces, colons
and the rest of printable ASCII stay as they are, so an ordinary username or
email reads unchanged, and `oidc:José García` reads
`oidc:Jos%C3%A9 Garc%C3%ADa`. Read it back with any percent decoder that leaves
`+` alone:

```sh
python3 -c 'import sys, urllib.parse; print(urllib.parse.unquote(sys.argv[1]))' 'oidc:Jos%C3%A9 Garc%C3%ADa'
```

A form decoder, such as Python's `unquote_plus` or Go's `url.QueryUnescape`,
reads a `+` in the name as a space. Use `unquote` or `url.PathUnescape`.

**When the name is shortened.** The column holds 256 characters. A name whose
encoded form is longer is cut between two of its characters and ends in `...`,
so no escape is cut in half and what remains still decodes. The part that was
cut cannot be recovered from the log. Encoding can take a name over the bound: a
Cyrillic letter takes 6 bytes and an emoji 12.

**When the row is lost.** Ptah writes the log beside the run, and a failed write
does not fail the run: the migration applies and exits 0, and the only trace is
a warning on the executor's stderr. The encoded name itself no longer causes
that. Measured on Ptah v0.18.0, it is stored byte for byte in MySQL under
`latin1`, `utf8mb3` and `utf8mb4`, strict or not, and in PostgreSQL under
`LATIN1`, `SQL_ASCII` and `UTF8`. Anything else that refuses the write still
loses the row, such as a trigger or a privilege that makes the table read-only.
A run Ptah stops before it selects a migration writes no row at all, as on a
database it could not open or a history that moved after the approval.

## Output handling

The runner never invokes a shell. It checks command arguments against known
credential values, derives and redacts standalone and escaped credentials from
database URLs, bounds stdout and stderr, and validates a structured result containing
the operation ID, coordination digest, and protocol version. A target sets
exactly one of `spec.target.coordinationKey` and `spec.target.realmRef`, and
both are non-secret operator inputs. A key is hashed with the normalized engine
and the resource's namespace, a realm's name with the normalized engine, and
the plaintext key is never copied into status.
Status otherwise stores only hashes, counts, classification, immutable
references, timestamps, and the
[approver's name](#approver-in-the-database) on a migration Apply's claim.

The credential-free target identity includes connection-security semantics,
not merely host and database names. Rotating password or certificate bytes is
allowed when the non-secret route and certificate paths stay fixed, while a
change to TLS verification, channel binding, authentication requirements, or
plaintext fallback invalidates the plan before the mutating child dispatches. The
username is part of that identity too. What each kind of rotation did to a waiting
plan and a running Apply, as measured, is in
[When the credential rotates](../../reference/credentials-and-admission/#external-secret-stores-rotation).

Raw drift details are parsed in memory and excluded from the structured result.
Observe exposes one canonical category aggregate for each category the report
found, each containing only a category from the closed v1 machine vocabulary, a
positive count, and a severity. A syntactically valid but unknown category fails the operation rather
than being published or dropped. The vocabulary is the list the pinned Ptah can
emit, recorded in `support/ptah-drift-categories.json`, and a Ptah pin that grows
that list cannot merge until the vocabulary, the status enum and the CRD schema
version follow it. The frame never carries
object names, SQL, schema literals, or the native diff. The vocabulary fits
under the frame's and the status's bound of 128 aggregates, so none is ever
left out and `driftFindingCount` is their sum. Drift the report has no
category for, such as a grant, crosses as drift with no aggregates at all: a
zero count and a `safe` highest severity, and nothing that names the grant.
Resolve and Verify follow the same boundary: native stdout is strictly decoded
before a small typed descriptor or requirement-name set is emitted, arbitrary
verification details and inspection metadata are discarded, and native stderr
or executor errors can produce only generic typed failures. No Resolve,
Verify, Observe, or Apply result carries native stdout.
Planning executes twice under the target Lease, reads each plan from the file
`schema plan --output` saved, requires the two files to be byte-identical, and
validates the accepted bytes through a native Apply dry-run before
publication. Every Plan and Apply read goes through the versioned JSON document
Ptah prints under `--json`: a document of another contract version, an outcome
the runner does not know, or a digest that does not name the exact plan bytes is
refused, and nothing is matched in the text Ptah writes for a person. The
independent operator classifier may raise destructive severity from the
rendered SQL and never lowers executor metadata. It also rejects
credential-bearing principal DDL.

Native stderr from Plan and Apply is discarded rather than forwarded: under
`--json` it lists the planned statements, which can carry declared row values.
Apply native stdout is never copied into the structured result, including failure
paths, and neither is the error sentence its
report carries. A failure leaves the runner as a typed code with a sentence of
the runner's own, apart from the ones listed below. A document
whose error names why the target could not be opened, from the closed set of
kinds Ptah declares, is reported by that kind and a sentence of the runner's
own, on every operation that opens the target; the error's message is not
read, and a kind outside the set is read as none. A stale plan is
reported as `stale_plan`, whether the schema or the declared rows moved, and
like every other Apply failure it stays uncertain until an observation settles
it: another Pod of the same Job may have run, so no one child's report proves
that nothing was sent.

Ptah's own words reach the frame, a condition and an Event for these failures:

| Failure | Code | Whose words | Cut at |
| --- | --- | --- | --- |
| A desired file Ptah cannot read | `desired_source_unreadable` | A second child with no database URL and no credential | 485 bytes, so the code and the reason fit an Event's 512 |
| A desired schema Ptah read and refuses as a whole | `desired_schema_invalid` | The same second child, when its words end the failed command's own sentence | 485 bytes, as above |
| A row file outside Ptah's working directory | `desired_source_unreadable` | The `error.message` of the failed `schema drift` or `schema plan`, only in the form Ptah's row-file guard writes | 485 bytes, as above |
| A plan that would change a protected table | `protected_table` | The `error.message` of the `schema plan` report, from a command with a database connection, which names the fenced tables | 4 KiB in the frame, then 512 bytes in the Event and 1 KiB in the condition |
| A Git source a Resolve cannot fetch | `child_exit` | The last line of standard error from the Git pull, which holds the Git credential and no database URL, and names the refusal | 512 bytes, and an Event's 512 include the words in front of it |

One function in the runner handles all of them, and the Pod log line below. It removes
the credentials of the Pod's environment wherever one appears whole, in any case,
replaces control and format characters with a space, joins the text onto one line,
takes the result frame's marker out and cuts it at a character boundary, in that order.
The marker goes because the manager searches a merged log for it anywhere and these
lines end in a line break, so a value holding `PTAH_RUNNER_RESULT_V1 12 <digest>` would
otherwise read as a frame header, and a header naming another digest makes the manager
refuse the termination summary that stands in for a lost frame.

For a desired file, when Observe, Plan or a migration history read fails and its
document names no such kind, the runner asks Ptah again in a second child:
`schema validate` for a schema, and `migrations status` against an in-memory
SQLite database for a migration directory. That child is told the path of the
desired files, `HOME` and `TMPDIR`, and nothing else of the Pod's environment, so
it holds no database URL and no credential and has no connection to quote. The
failed command's own error is not copied: it had a connection, and its sentence
could repeat what the database answered. When the second child reports that the
files do not load, the frame carries its reason, and the controller writes that
reason to the condition and the Event.

A row file a `data` block names outside Ptah's working directory is the one
failure whose sentence is copied from the failed command, since `schema
validate` reads no row file and cannot say it. Ptah's guard writes that sentence
from the declaration and the paths in the Pod before it opens the file. The
runner copies it only on one line and whole, in that form: the declared path and
the root are each one quoted string, as is the path it led to when Ptah names
one, the root and that path are absolute, and nothing follows the root. A database's answer reaches a failed
sentence in another form, under another prefix or with a line of SQL after it,
and leaves the failure with the code it had. The sentence names paths inside the
operation container, which mounts no credential; the function above removes a
credential that appears in it anyway.

When the second child reports a fault of the schema as a whole, the frame carries
its words only if they end the failed command's sentence. That sentence is
compared and goes no further, so a refusal that came from reading the database,
such as a foreign key into a schema it holds, stays out, and so does a fault of the
file that did not stop the command. A fault names what the file declares. The
second child reads no row of the database, and the validator quotes no row a
schema declares. Any other answer leaves the failure with the code it had.

That reason can quote the file. A parser names the token it stopped at, and a
YAML error repeats the start of the value it could not decode:
`schema.sql:7:1: unsupported SQL statement: SELCT`, or
``cannot unmarshal !!str `hunter2...` ``. Status and Events are readable by more
roles than the file is: the diagnostic reader may read them and is denied
ConfigMaps and plan chunks, so a fragment of a source file can reach someone who
cannot read the file itself. Redaction cannot cover a fragment. The YAML reader
cuts `Hunter2Hunter2` to `Hunter2...`, and the SQL reader stops at `PASS` in
`pass word`, and neither leaves a whole credential to match. Redaction does not
know what else in a file is sensitive either, so keep a value that must not reach
status out of a desired file.

Outside status and Events, a failed `fetch-git-source` init container writes
standard error, handled by the same function, to its log, and the kubelet copies
the end of that log into the Pod's status. The `fetch-schema` and `fetch-migrations` init containers are Ptah itself,
with no runner around them, so their logs hold what Ptah wrote with nothing
removed. Those containers hold the registry credentials, which is why status and
Events name only the step and its exit status. Whoever holds `pods/log` reads
those logs, and whoever reads Pods reads the status.

The first line of a failed Apply's error sentence goes to the operation Pod's log,
and nowhere else: not to status, an Event, the result frame or the termination
summary. It is the database's own words, and it can carry a role, a host, a
database name and row values, such as the value a duplicate key or a failed cast
quotes. The runner bounds it to 1 KiB. Before that, it compares each quoted part of
the line with the statement Ptah ends its sentence with, and replaces what they share
with `[in the statement]`. A database echoes what it refused: measured on MySQL 8.4, a
statement it could not parse gives `near 'CREAT USER 'app'@'%' IDENTIFIED BY
'a-password'' at line 1`, and a literal gives `Duplicate entry 'a-literal' for key`.
Both are replaced, and so is a name the statement declares, because a comparison cannot
tell a name from a literal. The rest of the sentence is dropped, because it is the
statement, and a statement is plan SQL, which the diagnostic reader may not read.

What remains is what the database read. A value from a table is not in the statement,
and no redactor can recognize a row, so the line is exactly as sensitive as the data
the failed statement touched. Whoever holds `pods/log` in the namespace reads it, the
diagnostic reader role included, and anything that ships Pod logs elsewhere carries it
too; decide what such a pipeline may store with that in mind.

The Job keeps that log for 24 hours after it finishes when a person approved the Apply,
so that person can read it the next morning, and when a migration run under
`apply: Always` stopped the resource: it reported `Partial` or `Unknown`, or nothing, or
it failed at a statement and left a dirty revision row. Someone has to read the database
then, so what that statement's failure quoted stays readable for those 24 hours under
`Always` as well. Any other Job [keeps five minutes](../operations/#failed-apply-log).
What a Pod kept that long still holds: it has no API token, because automatic
token mounting is disabled. Its projected result token has the receiver as its
only audience and lasts at most an hour, and the receiver refuses it once the Job
carries a cleanup TTL or its claim has retired, as it refuses a client certificate
of the older transport. No credential is in the Pod's spec, but the container runtime
on the node keeps a stopped container's environment until the Pod is removed, so
anyone with root on that node can read it for those 24 hours as they could while the
Pod ran. That is the database URL of the operation container, and for a migration Apply
the registry or Git credential of its fetch init container as well. Once you have read
the reason you can end that early: `kubectl delete job -n <namespace> <job>` removes
the Job and its Pod, and the node drops what it kept. It takes `delete` on `jobs`,
which the diagnostic reader role does not grant, and no admission policy or webhook
judges it. The run is already recorded by the time status names the log, so nothing
waits on the Job, and the resource goes on as it would have. Delete a Job that has
finished: one that is still running holds a statement. A client other than `kubectl`
has to ask for `propagationPolicy: Background`, because the API server otherwise leaves
a Job's Pods behind. Admission lets the
manager write the long TTL only on an Apply Job, and the webhook refuses it on a run
whose termination summary reports success and on a claim that names no approval.

A successful Plan frame is the one frame that carries the plan: it transports
the exact plan bytes through the authenticated receiver before they are
committed to immutable chunks. See [Plan results stay out of logs](#pod-logs-carry-plans).
Apply frames never contain native SQL output.

The runner also writes a summary of each frame into its container's
termination message, which the kubelet copies into Pod status, where anyone who
can read the Pod can read it. It holds the operation id, two flags, an error
code from a fixed vocabulary, the realm and target identity digests, a
migration run's outcome with the count and the first and last applied
versions, and the frame's SHA-256. Apart from that digest, the resource's own
status already carries all of it, and it holds no plan bytes, error text, SQL
or native output. Pod status is written by the kubelet and by control-plane
controllers, and no default user-facing role grants it, so forging a summary
takes either the node's credentials, which can already forge the log it stands
in for, or a grant the default roles keep for the control plane.

## Remaining deployment responsibilities

- Apply namespace NetworkPolicies that allow executor Pods to reach only the
  required registry, DNS, and database endpoints. Start from the
  egress-policy example in `examples/networkpolicy-egress.yaml`. It covers
  both families and narrows by operation: a schema Apply runs bytes the
  operator already stored and is given no registry egress, while a migration
  Apply fetches its artifact and is. The policies go in the namespace the
  operation Pods run in, which is the namespace of the `PtahSchema` or
  `PtahMigration` rather than the release namespace, and they assume the
  registry and the database are in-cluster; for either one outside, replace
  the selector with an admission-controlled CIDR rather than opening Internet
  egress. A Pod no policy selects is not isolated at all, so
  `TestTheEgressExampleSelectsEveryOperationPod` builds one Pod per operation
  of both families and fails when one stops being covered. That test reads
  selectors; the acceptance suite checks enforcement. On both engines it
  applies the example in a cluster whose CNI enforces NetworkPolicy, adapted
  only where the example says to adapt it, and checks what each operation can
  reach: DNS always, the database and the registry exactly where the example
  grants them, and nothing else. The readings come from probe Pods labeled as
  each operation. Admission refuses a Pod that claims the operator's own
  `managed-by` value without a Job the operator made, so the probes carry
  another value and a copy of the policies selects it; the copy differs from
  the example in that one value, which the suite checks. A real migration then
  runs to `InSync` under the example itself. A CNI that does not enforce NetworkPolicy makes every one
  of these policies a no-op, so check yours does before relying on them.
- Grant the database user the minimum DDL and introspection privileges needed
  for the selected schemas. See [Database support and privileges](../../support/databases/)
  and do not use a cluster-wide administrative account.
- Pin manager, runner, and executor images by digest and verify their release
  provenance before installation.
- Keep the release namespace and the coordination namespace, which holds the
  shared target locks, to Ptah administrators, as
  [the release namespace contract](#release-namespace) says.
- Install exactly one operator Helm release per cluster. Scale replicas within
  that release for high availability; the singleton admission configuration
  intentionally prevents ordinary independent-release ownership.
- Assign one stable coordination key to every physical database and reuse it
  across all aliases, proxies, credentials, and Ptah resource kinds in the
  namespace. For a database that more than one namespace manages, create a
  `PtahRealm` listing exactly those namespaces, and keep write access to
  realms with the cluster's administrators. Remove a namespace from every
  realm before deleting it, and where tenants can create namespaces, list only
  names that already exist.
- Use separate database credentials for production and optional dev rehearsal
  targets.

## Reporting a way across one of these boundaries

Privately, to the address the
[security policy](https://github.com/stokaro/ptah-operator/blob/master/SECURITY.md)
names, rather than on the issue tracker: an issue is public from the moment it
is filed. The policy also says which findings are in scope and which are a
deployment's own decision.
