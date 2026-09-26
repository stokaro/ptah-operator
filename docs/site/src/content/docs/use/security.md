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

### Where the product boundary is {#product-boundary}

The boundary the operator is built to hold lies between it and the application
namespaces: the people who write `PtahSchema`, `PtahMigration` and approval
resources there, the database credentials those namespaces hold, and the
operation Pods that run there with those credentials. The four authorities in
[Trust boundaries](#trust-boundaries) are that boundary, and the rest of this
page says how each is held. The open work that hardens it:

- [#445](https://github.com/stokaro/ptah-operator/issues/445): authorize
  membership of a database realm instead of letting any resource claim a
  coordination key.
- [#446](https://github.com/stokaro/ptah-operator/issues/446): keep status
  writable only by the manager, and resolve an unaccounted run through an
  identity-stamped acknowledgment.
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
  bug cannot create a Job, a plan or a chunk outside its shape. See
  [Admission](../../reference/credentials-and-admission/#admission).

None of these is a defense against an administrator acting in bad faith. The
release machinery also carries mechanisms that assume a hostile writer inside
the release namespace: the hook-progress policies, the uninstall fences, the
per-release ServiceAccounts and the credential grace windows that
[Release lifecycle](../../reference/release-lifecycle/) describes. They sit
outside this contract, and
[#443](https://github.com/stokaro/ptah-operator/issues/443) removes them.

## Trust boundaries

The operator separates four authorities:

1. A desired-state author may change `PtahSchema` but cannot approve a plan
   merely by editing that resource. They can, however, make approvals
   unnecessary: `spec.policy.apply` is a field of the resource they own, and
   selecting `Always` applies non-destructive plans with no approval at all.
   RBAC cannot close that, because the bypass is not an approval. See
   [Who may turn the approval requirement off](#who-may-turn-the-approval-requirement-off).
2. An approver may read schemas, migrations and their plans, and create
   immutable approvals for either family. The chart creates an optional
   ClusterRole but never binds it automatically.
3. The controller may manage plans, Jobs, ConfigMaps, Leases, status, and
   Events. Its shipped ClusterRole contains no Secret permission. Retained,
   typed admission policies constrain its main-resource writes to structural
   Job, immutable plan, and immutable chunk shapes; a fail-closed webhook then
   reconstructs and compares the complete write intent through direct API
   reads.
4. A Job receives only the credentials needed for its fixed operation through
   same-namespace Secret selectors resolved by the kubelet.

### Who may turn the approval requirement off {#who-may-turn-the-approval-requirement-off}

The separation above is about who may write an approval. It says nothing about
who may decide one is not needed, and those are different questions with
different answers.

`spec.policy.apply` lives on the desired-state resource. An author with the
rights the example Role grants may set it to `Always`, after which
non-destructive schema plans apply without an approval; `PtahMigration`
exposes the same field. Separating the approver Role from the author Role does
not prevent this, and no amount of RBAC on approval objects will, because
nothing is approving anything.

`Always` is not wrong. It is the deliberate unattended mode, and an
installation that wants it should have it. What matters is that choosing it is
a decision someone made on purpose, rather than a default an author can reach
without anyone else noticing.

Where independent approval is an operational requirement, install
[`examples/approval-policy-guard.yaml`](https://github.com/stokaro/ptah-operator/blob/master/examples/approval-policy-guard.yaml).
It is a `ValidatingAdmissionPolicy`, cluster-scoped and administrator-owned, so
an author with complete rights over resources in their own namespace cannot
edit, rebind or delete it. It covers both kinds and both `CREATE` and `UPDATE`:
a guard that watched only updates is bypassed by creating the resource with
`Always` already set.

What it refuses is the transition into `Always`, not the value itself. That
distinction is load-bearing. The operator patches these resources to add and
remove its operation finalizer, and its service account is not exempt, so a
guard that refused every write leaving `Always` in place would stop operations
from starting and stop a finished one from releasing its finalizer -- an
administrator who chose `Always` would have wedged every resource they chose it
for. Leaving the field where an administrator put it is permitted, and so is
moving back to `OnApproval`; arriving at `Always` from anywhere else is not.

Two things to check after installing it. A policy with no binding is inert and
reads exactly like one in force, so confirm the binding exists and that its
`validationActions` is `Deny` -- `Warn` and `Audit` record the bypass rather
than refusing it. And the binding's namespace selector decides which namespaces
are covered; the example covers all of them.

Start namespace-scoped bindings from the
desired-state author (`examples/desired-state-author-role.yaml`) and
diagnostic reader (`examples/diagnostic-reader-role.yaml`) examples. The
chart's optional approver ClusterRole remains unbound, so these three human
permission sets can be assigned to different identities. Diagnostic access
deliberately excludes Secrets, plan-chunk ConfigMaps and operation Pod logs,
because [Pod logs carry plans](#pod-logs-carry-plans); grant exact plan-chunk
access separately for an approver reviewing one immutable plan.

These write boundaries reduce the effect of controller bugs and prevent its
RBAC from becoming arbitrary workload or ConfigMap creation authority. The
manager binary and its status-write authority remain trusted: the admission
layers do not claim to contain a malicious replacement image that can forge
the status records used to reconstruct intent.

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

`clientCertificateFrom` remains in the alpha source shape for compatibility but
is rejected by both API validation and Job construction. The pinned executor
loads a client pair into a process-wide TLS configuration and cannot constrain
certificate selection after a cross-host redirect. Re-enable this field only
with an executor contract that selects the certificate against the effective
TLS authority on every handshake.

## Plan and approval visibility

Plan ConfigMaps contain schema-changing SQL, not credentials. They are
intentionally inspectable by independently authorized approvers, but arbitrary
schema names, defaults, comments, and literals may still be sensitive. Restrict
ConfigMap and `PtahSchemaPlan` read access in application namespaces
accordingly. The built-in approver role can read plan metadata but deliberately
cannot read every ConfigMap cluster-wide. Grant a separate namespace Role
restricted to the current plan chunk names, as described in
[Exact-plan approvals](../approvals/). What that access is used with is
`kubectl ptah`, a read-only client that needs `get` on the schema, the plan and
those ConfigMaps and nothing else; [Read a plan](../read-a-plan/) carries the
Role and the [install](../read-a-plan/#install).

### Pod logs carry plans {#pod-logs-carry-plans}

A Plan Job reports to the controller through its container log. The runner
writes one framed result to stdout, and a successful Plan frame holds the whole
plan document: every statement, and for
[declared reference data](../reference-data/) the row values in them. The
controller reads the frame through the `pods/log` API, checks it, and only then
commits the same bytes to the chunk ConfigMaps. The frame also stays where the
chunk Role does not reach:

- in the Pod's log, readable by anyone with `get` on `pods/log` in the
  namespace until the Job is removed, which is five minutes after it finished
  at the earliest;
- in the container log file on the node, until the kubelet garbage-collects the
  container;
- in any log store a node agent ships container logs to, with that store's
  readers and its retention.

RBAC cannot narrow `pods/log` to the operation Pods that carry no plan. A rule
has no label selector, and `resourceNames` cannot name a Pod whose name is
generated for each attempt. A grant of `pods/log` in an application namespace
therefore reads every plan published there, including plans its holder was
never asked to review, which is broader than the exact-chunk Role in
[Exact-plan approvals](../approvals/).

What to do about it:

- Treat `pods/log` in an application namespace as plan access. Grant it in a
  Role of its own, only to people who may read every plan in that namespace,
  and keep it out of diagnostic and developer Roles. The diagnostic reader
  example leaves it out for this reason; status, conditions and Events carry
  what the controller made of each result.
- Keep Plan Pod logs out of shared log stores, or store them with the access
  control and retention a plan needs. Plan Pods carry the labels
  `app.kubernetes.io/component: schema-operation` and
  `operator.ptah.run/operation: plan`, which an agent that adds Pod labels to
  each record can match to drop or reroute them. Dropping them costs the
  operator nothing: it reads the frame from the kubelet, never from a log
  store.
- Check what the pipeline already shipped. Changing it does not recall a plan
  it copied earlier, which stays in the store until that store's retention
  ends.

The manager's own ClusterRole keeps `get` on `pods/log`, because reading the
frame is how it learns every result. Only a Plan frame carries the plan.
Sealing that payload to the manager, so that the log holds only ciphertext, is
tracked in [#449](https://github.com/stokaro/ptah-operator/issues/449).

Approval admission fails closed. It binds names to UIDs, rejects a plan whose
storage commit is incomplete, rejects changed policy bytes or target state, and
makes the stamped decision immutable. Both approval webhook configurations use
the non-configurable `Fail` policy, so an unavailable webhook cannot admit a
caller-supplied identity stamp. Ordinary Kubernetes RBAC is not treated as
field-level authorization.

## Output handling

The runner never invokes a shell. It checks command arguments against known
credential values, derives and redacts standalone and escaped credentials from
database URLs, bounds stdout and stderr, and validates a framed result containing
the operation ID, coordination digest, and protocol version. The required
`spec.target.coordinationKey` is a non-secret operator input; it is hashed with
the normalized engine, and the plaintext key is never copied into status.
Status otherwise stores only hashes, counts, classification, immutable
references, and timestamps.

The credential-free target identity includes connection-security semantics,
not merely host and database names. Rotating password or certificate bytes is
allowed when the non-secret route and certificate paths stay fixed, while a
change to TLS verification, channel binding, authentication requirements, or
plaintext fallback invalidates the plan before the mutating child dispatches.

Raw drift details are parsed in memory and excluded from the framed result.
Observe exposes at most 64 canonical category aggregates, each containing only
a category from the closed v1 machine vocabulary, a positive count, and a
severity. A syntactically valid but unknown category fails the operation; adding
a category requires an explicit runner protocol update. The frame never carries
object names, SQL, schema literals, or the native diff. `driftFindingCount`
remains the complete aggregate count; `driftFindingsTruncated=true` explicitly
reports that additional categories were omitted.
Resolve and Verify follow the same boundary: native stdout is strictly decoded
before a small typed descriptor or requirement-name set is emitted, arbitrary
verification details and inspection metadata are discarded, and native stderr
or executor errors can produce only generic typed failures. No Resolve,
Verify, Observe, or Apply frame carries native stdout.
Planning executes twice under the target Lease, requires byte-identical native
plans, and validates the accepted bytes through a native Apply dry-run before
publication. The independent operator classifier may raise destructive
severity from the rendered SQL and never lowers executor metadata. It also
rejects credential-bearing principal DDL.

Apply native stdout and stderr are never copied into the framed result or
runner diagnostics, including failure paths. Only generic typed failures leave
the runner. Stale Apply is classified as pre-mutation only for the exact native
diagnostic bound to the reconstructed plan's source fingerprint; altered,
extra, or truncated output is treated as uncertain.

A successful Plan frame is the one frame that carries the plan: it transports
the exact plan bytes to the controller before they are committed to immutable
chunks, so access to Plan Pod logs is plan access. See
[Pod logs carry plans](#pod-logs-carry-plans). Apply frames never contain
native SQL output.

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
  across all aliases, proxies, credentials, namespaces, and Ptah resource kinds.
- Use separate database credentials for production and optional dev rehearsal
  targets.

## Reporting a way across one of these boundaries

Privately, to the address the
[security policy](https://github.com/stokaro/ptah-operator/blob/master/SECURITY.md)
names, rather than on the issue tracker: an issue is public from the moment it
is filed. The policy also says which findings are in scope and which are a
deployment's own decision.
