---
title: Capacity
description: What the operator costs per resource, what the defaults are, what one lab workload read, and what has not been measured.
---

No capacity envelope has been measured. One workload has been, on a
GitHub-hosted runner, and [what it read](#lab-20) is below: it describes that
runner and not an installation. The rest of this page is the arithmetic a
deployment can do before it runs anything, and the defaults it would be running
with. None of it is a throughput claim, and all of it is here because the
alternative is a reader assuming the defaults fit their scale.

What an installation needs before it can size itself is the same measurement
against its own workload and nodes: latency, queue delay, API demand, CPU,
memory, Pod count and retained growth, which is what
[issue #224](https://github.com/stokaro/ptah-operator/issues/224) asked for.
[How to measure it](#how-to-measure-it) is the tool for that. Until an
installation has run it, treat what follows as the lower bound on cost rather
than as a limit.

## What a resource costs while nothing changes

A resource that is converged still works. The operator re-reads it at
`spec.interval`, and each pass runs the read-only operations its family needs.

| | `PtahSchema` | `PtahMigration` |
| --- | --- | --- |
| Operations per refresh | Resolve, Verify, Observe, Plan | Resolve, Verify, History |
| Default `spec.interval` | 10m | 10m |
| Jobs per resource per day | 576 | 432 |

Each of those is a Job, a Pod, an image pull against the node's cache, a
database connection for the operations that open one, and a result read from
the Pod's log. The numbers are cadence arithmetic: four operations every ten
minutes is 576 a day, three is 432. They are what the resource costs before a
change, a retry, an approval wait or a post-Apply proof adds anything.

Raising `spec.interval` divides all of it. A resource whose artifact changes
once a week and whose database nothing else writes does not need a reading
every ten minutes, and the interval is the first thing to move on a large
installation.

## What one manager is

One manager reconciles, whatever the replica count. Additional replicas serve
admission and take over on failure; they do not divide the reconciliation work,
because a single leader holds it.

The chart's defaults for that manager:

| | Request | Limit |
| --- | --- | --- |
| CPU | 50m | — |
| Memory | 96Mi | 256Mi |

Those are defaults, not a measured working set. The manager caches the live
status of every resource it watches, the Jobs it dispatched and the policy
objects it reads, so the number that matters is the one your resource count
produces, and nothing here establishes it.

## What storage grows with

A plan is immutable, and a new fingerprint publishes a new one. Nothing is
pruned automatically.

| | Value |
| --- | --- |
| Maximum executable plan | 8 MiB |
| Chunk size | 512 KiB |

A hundred distinct retained plans at that ceiling hold eight hundred mebibytes
of SQL before metadata. That is arithmetic about the limit rather than a
measurement of any installation. Which plans may be deleted is
[Pruning stored plans](../../use/operations/#pruning-stored-plans).

## The dimensions an envelope has to be stated over

A measured envelope is a set of numbers against a workload, and these are the
axes that workload varies along. They are named here because a measurement that
holds one of them fixed says nothing about an installation that moves it, and
because the reader sizing an installation needs to know which of their own
numbers matter.

| Dimension | Why it changes the answer |
| --- | --- |
| Resources, and realms across them | Every resource refreshes on its own interval; resources sharing a database realm serialize against each other, and resources in separate realms do not |
| `spec.interval` | The cadence above multiplies by resource count, and it is the first thing to move on a large installation |
| Plan size | A plan is stored as chunks up to the 8 MiB ceiling, and an applied plan once more as the ConfigMaps its Apply mounts, so it costs API bytes, etcd, and a longer read on the way back |
| Migration history length | History is read on every refresh of a `PtahMigration`, and the reading grows with the sequence already applied |
| Approvals waiting | A resource waiting for a person keeps refreshing and keeps its plan and its chunks, so a backlog is retained storage as well as queue |
| Changes at once | A rollout, or a restart, refreshes everything at once; the burst is what a steady-state figure does not describe |

None of these has a supported maximum, because none has been measured. What
follows is what a measurement would have to produce for them.

## How to measure it

`hack/capacity` measures a declared workload on the demonstration lab, which is
the acceptance harness stopped after its bootstrap, so what it measures is the
chart and images of the commit it runs from:

```sh
make demo-up
CAPACITY_OUT_DIR=/tmp/capacity hack/capacity.sh
```

The default workload selects PostgreSQL. To measure MySQL on the same lab, use
a fresh output directory and the MySQL workload:

```sh
CAPACITY_WORKLOAD=support/capacity/workload-mysql.json \
  CAPACITY_OUT_DIR=/tmp/capacity-mysql hack/capacity.sh
```

The manual Capacity workflow also accepts an engine selection. Each run
measures one engine and records it in the report; a PostgreSQL result supplies
no MySQL measurement.

`support/capacity/workload.json` declares the workload: how many resources of
each family, their interval, how long each condition lasts, how many resources
change at once, and how long the registry is unreachable. The harness gives
every resource a database of its own, so no two share a realm or an advisory
lock, and walks the workload through a cold start, a steady state, a restart of
every manager at once, a batch of changes and a registry outage.

The Go collector accepts `-namespace work-a,work-b` when both namespaces have
been prepared. It distributes each resource family round-robin, so ten schemas
and ten migrations produce five of each in each namespace. Each namespace
needs the declared registry credentials, verification policies, image-pull
configuration and the database Secrets for its assigned resources. Resource
index `i` goes to namespace `i % namespaceCount` within each family; database
Secret indices remain global, with migrations following schemas. The extra
restart-approval fixture stays in the first namespace. The lab wrapper prepares
two fresh workload namespaces with the profile's object quotas and restricted
Pod Security Admission pinned to the server minor. It copies the lab's registry
credentials and immutable verification policies into each namespace, and gives
their default ServiceAccounts pull credentials without API write grants or
automatic token mounting.

On a cluster with enforced egress, set
`CAPACITY_REGISTRY_EGRESS_POLICIES=ptah-schema-operations-registry,ptah-migration-operations-registry`
to name the registry-only allowances from the network-policy example in every
workload namespace. The collector's equivalent is `-registry-egress-policies`.
The outage temporarily withdraws those allowances and restores their original
rules, preserving DNS, database, and result-delivery policies. Recovery refuses
to overwrite a replaced or concurrently edited policy. An interrupted process
that cannot run cleanup requires restoring the named policies before another run.
The IP-based fault is limited to the unrestricted HTTP lab: additive policies
cannot override an existing allowance, and an HTTP backend address does not
identify the TLS proxy. The collector checks this configuration before creating
the fleet. A timed hold still passes only after fresh registry-read failures
have been observed in both resource families in every workload namespace.

The restart scenario requires a fresh approval, its exact Apply Job, and a
fresh converged History reading for the approved migration. It watches the
claim from before admission and checks the migration, plan, approval, and Job
UIDs. Missing admission, a broken watch, an unrelated Job, or incomplete
recovery fails the run even when the rest of the workload converges. An
ambiguous create response followed by an existing approval is inconclusive
and fails the measurement.

The selected database server runs in a separate fixture namespace. Every
workload database has its own login. PostgreSQL owners have no superuser,
role creation, database creation, replication or row-security bypass privileges,
and public database access is revoked. MySQL users receive privileges on their
own database only, without global privileges or grant options. Their database
names contain no wildcard characters. Database credentials travel through stdin and Secrets; the ownership
journal contains no passwords. NetworkPolicies refuse inbound traffic to task
Pods and admit database traffic only from the workload namespaces.

The wrapper records created namespace UIDs in `bootstrap-state.json` inside a
fresh output directory. Cleanup uses those UIDs and waits for workload namespace
deletion before removing the database; it never forces finalizers. If cleanup
fails, it reports the retained fixtures and journal. With the same lab environment
loaded, retry it with `python3 support/qualification/probes/capacity_bootstrap.py
cleanup --state /path/to/bootstrap-state.json`.

This bootstrap remains a lab configuration. Verified TLS, signed
artifacts, the full egress profile, the declared artifact/history/data
sizes and the complete soak remain required for 0.2.0 qualification.

Samples aggregate workload reads across every declared namespace and mark the
source incomplete if any read fails. Jobs retain their namespace and UID,
cycle watches run separately for each namespace and family, and artifact
updates address the resource's declared namespace. A right resource count in
the wrong namespace does not satisfy convergence.

The outage must produce a failed Resolve in each loaded resource family in
each occupied namespace.
The report retains each failure's Pod UID and runner frame digest. Only a Pod
created after the fault, with a matching operation ID and a read-only
`child_exit` result, counts. A NetworkPolicy that blocks nothing fails this
check. Removing the policy starts the recovery clock; every workload resource
must then converge with a fresh database observation. Each created policy is also
removed if measurement fails, with its UID checked before deletion. A failure
in the second namespace still restores the first; an existing policy that
prevented creation is never removed by that cleanup.

Throughout, it reads the cluster rather than estimating it:

| Figure | Read from |
| --- | --- |
| Jobs per minute, time to start and to finish | Each operation Job's own timestamps |
| Pending and running operation Pods | The Pods, by phase |
| Oldest reading and time overdue | Each resource's `lastObservedAt` or `history.observedAt`, and its `nextReconciliationTime` |
| Manager memory and CPU | Each manager Pod's `process_resident_memory_bytes` and `process_cpu_seconds_total` |
| Queue depth and wait | `workqueue_depth` and `workqueue_queue_duration_seconds` |
| Client throttling | `rest_client_rate_limiter_duration_seconds` and HTTP 429 responses |
| Admission latency | Each API server's own `apiserver_admission_webhook_admission_duration_seconds` for this operator's webhooks |
| Retained plans and SQL | The plan objects, the bytes in their chunks, and the ConfigMaps each applied plan was projected into |
| Time to serve an approval during a restart | The approval's admission and its Apply Job's creation |

The report carries the workload and the environment beside the figures: the
Kubernetes version, the nodes' allocatable CPU and memory, and the manager's
image and resources. The lab wrapper also records the Docker daemon's CPU and
memory once, with its identity and timestamp in `host.json`. Kind nodes share
that host: their summed allocatable resources are scheduling capacity, not
additional physical CPU or memory. A direct collector run without `-host-info`
marks host capacity unobserved. Neither reading establishes exclusive use of
the machine. A figure without its environment says nothing about another
installation. The Capacity workflow runs the same thing on a schedule, on
request, and on any change to the measurement itself, and publishes the report
as an artifact of the run.

Report format 3 records failed reads in each sample's `incomplete` list and
counts them by source in each scenario. Figures affected by a failed read are
`null` in JSON and `n/a` in the summary, including a maximum that was only
partly observed. Missing manager process metrics and a window with no samples
are also missing evidence. A fault may explain a missing reading, but the
report cannot establish a performance bound for that figure during that window.

Each complete sample retains the queue-wait and admission histograms, including
observation counts, sums and cumulative buckets. Recomputing a scenario from
these samples preserves its histogram percentiles. Bucket upper bounds and
percentiles above the largest finite bucket use the JSON string `"+Inf"`;
finite values remain numbers. The summary displays this as `> largest bucket`.
Reports written before format 3 lack these raw histograms and cannot establish
their histogram percentiles from the retained samples alone.

Manager samples retain the Pod UID, container ID and start time, restart count,
and process start time. The sampler checks the container identity again after
each scrape. `-expected-managers` declares the required population and defaults
to two. The sampler checks the full Pod inventory before and after collection.
A missing or extra replica, a terminating Pod, a missing identity or a
replacement during collection makes the manager reading incomplete, including
when a replica is absent from the first sample.

Scenario counter deltas require at least two ordered readings, the same manager
processes throughout, and no reset in any intermediate counter or histogram.
A replacement, missing replica or counter reset adds
`manager-counter-continuity` to the scenario's `incomplete` counts and records
its reason in `counterProblems`. CPU, client throttling, HTTP 429 counts and
queue-wait percentiles then become `null`/`n/a`; complete RSS and queue-depth
readings remain available. A planned restart explains this gap but does not
supply the missing counter measurements.

The sampler requires one running kube-apiserver Pod per control-plane node.
`-expected-api-servers` declares the count and defaults to three for the 0.2.0
profile. Each collection checks the node and Pod inventory before and after
scraping. It opens a bounded Pod-specific port-forward to port 6443 and uses
HTTPS with the kubeconfig's cluster CA, client credentials and
`kubernetes.default.svc` certificate name. Insecure TLS and redirects are
refused. The lab identity needs node and Pod reads, Pod port-forward access in
`kube-system`, and `get /metrics`; the sampler creates no RBAC grants.

Each sample's `apiTargets` retains the declared node and container identities.
`apiServers` holds separate process identities, scrape timestamps, admission
histograms and API priority/fairness rejection counters. Each scenario reports
admission percentiles, population size and time boundaries per server. A
missing member, identity change or counter reset makes the affected scenario
figures incomplete; `apiCounterProblems` records continuity failures. The
summary lists every server and its observation count. An empty population is
`n/a`, and fewer than 20 admission observations cannot meet the qualification's
percentile requirement. API priority/fairness rejections count all traffic;
attribution to workload, monitoring and fixtures needs separate evidence.

The collector also lists and watches the workload's `PtahSchema` and
`PtahMigration` resources before creating them. Its identity needs `list` and
`watch` on both kinds in the workload namespace. Watch reconnects resume the
last resource version. An expired cursor, failed watch or full event buffer
stops the workload and makes the cycle count incomplete; a new list cannot
replace missing history.

The report's `cycles` section retains compact readings, resource lifetimes and
completed read-only cycles. A cycle requires Resolve/Verify/Observe/Plan for a
schema, or Resolve/Verify/History for a migration, with distinct bound Job UIDs
and fresh convergence for the same resource UID and generation. Initial status,
Apply, unresolved runs and repeated completion readings do not count. A pending
lock release must clear before a cycle counts. Condition messages are omitted.

Each scenario's `refreshCycles` lists counts per resource UID, including zero.
A replacement starts its own count, and a cycle crossing a scenario boundary
is excluded. The final convergence reading must arrive before the window ends;
an older observation timestamp cannot move a later lock release into that window. A history gap makes this field `null` and records `cycleProblems`.
These observed cycles are not estimates from elapsed intervals or Job totals.
They do not yet establish the qualification profile's churn, per-slot minimum
or full soak.

The soak workload also sets `unrelatedObjects: true`. Before the cold start,
the bootstrap creates 1,000 immutable ConfigMaps with 1 KiB payloads and 1,000
Jobs across two additional namespaces. At most twenty of these Jobs run at
once during preparation. Each runs a non-root, read-only shell that exits
successfully. No service account token is mounted; a NetworkPolicy denies
ingress and egress in each namespace. Completed
Jobs and their Pods remain present during measurement. They carry a separate
label and live outside the workload namespaces, so they affect the cluster
without entering workload Job or Pod counts.

The bootstrap checks the full population, original UIDs, ConfigMap bytes,
Job completion, and each matching Pod's successful execution before and after
the workload. The raw inventories and their hashes accompany the ownership
journal. Cleanup deletes these namespaces with their recorded UIDs, using the
same cleanup path as the workload fixtures. This fixture supplies the declared
background population; its presence alone does not establish performance.

Each sample retains the UID, resource version, generation, suspension/deletion
state, read time, active operation claim, and database observation and reconciliation timestamps of
every workload resource. The report keeps its original maxima across all
resources and separately computes `eligibleFreshness` for resources that are
neither suspended nor deleting. Waiting for approval does not exclude a
resource: it still refreshes. Missing timestamps, partial lists, and older
reports without these per-resource readings cannot establish an eligible
freshness bound; the affected values remain `null` (`n/a` in the summary).
Freshness belongs to the scenario in which its resource list was read, even
when collection began in an earlier scenario. Each resource collection records
its start and finish, including failed attempts. The report counts readings
excluded from a boundary-crossing sample as `outsideWindowReadings`; a partial
or failed list cannot become a complete population by filtering its timestamps.

Claiming an operation clears its scheduled reconciliation timestamp. A valid
persisted claim without that timestamp counts as in flight, and its age is
reported separately. It remains eligible for observation age: a long-running
operation cannot hide stale database observations. A phase name alone cannot
establish a claim. A missing deadline without a claim remains unavailable,
and a population with no scheduled readings has no scheduled overdue bound.
Recovery can retain both a claim and a deadline; those readings contribute to
both counts, and their actual deadline still determines scheduled overdue.

The full workload and 90-minute soak remain required for 0.2.0 qualification.
A successful metrics collection does not establish a capacity bound.

## Repeated updates and retention maintenance

`CAPACITY_VARIED_INPUTS=1 CAPACITY_WORKLOAD=support/capacity/soak.json`
selects the populated 90-minute workload. Each ten-minute round updates five
resources of each family, replaces two idle resources of each family, and
checks the databases before and after those changes.

The round then exports retained plans and suspends all twenty resources. It
waits for the controllers to acknowledge suspension and for every workload Job
and Pod to finish. Managers remain running. Before each obsolete-plan deletion,
the collector re-reads every documented retention pin, including the unresolved
migration record's metadata copy, and verifies the exported bytes. Deletions
carry the plan UID and resource version. The collector waits for the plan and
its owned chunks and projections to disappear, verifies pinned plans again,
then resumes the resources and checks convergence and database contents.

The report records the before/after inventories, archive hashes, exact deleted
identities, garbage collection results and a complete post-cleanup metric
sample. The final three checkpoints require non-growing retained payload within
128 MiB and the same manager processes, each within 192 MiB RSS and 10% of its
first checkpoint. Missing measurements and broken process continuity fail the
comparison. A shorter diagnostic with fewer than three rounds cannot establish
that plateau.

After the measured soak, `soak.retentionFault` exercises pending approval and
unresolved Apply on the same twenty resource identities. Native Ptah rolls back
one fixture migration, and the probe drops one empty schema payload table. Before approval, it changes the
schema's lock timeout under `Never`, then restores the original policy. This
produces an actual superseded plan whose deletion and garbage collection the
fault must prove. A
temporary admission policy prevents one schema's Apply claim from being saved;
the collector requires the actual refusal before suspending that resource with
its admitted approval still pending. A thirty-second database table lock holds
the migration's native Apply while the collector suspends that migration. The
controller records its uncertain outcome and retains the metadata copy.

With the fleet suspended, the collector exports the plans, prunes only unpinned
plans, and verifies both fault subjects and their payloads before and after
garbage collection. It then removes its admission gate, resumes the fleet,
records whether the retained schema approval was consumed or retired by the
fresh read. A retired approval stays retained, and recovery uses a new admitted
approval bound to the current plan. The collector also requires fresh
migration history to settle the unresolved run without another Apply. Complete
database inventories check that all rows, defaults and migration versions are
restored. These deliberately blocked resources are outside the measured soak
window. A failure preserves the evidence and attempts to remove the owned gate
and resume the exact resources it suspended.

For a focused local retry, the collector accepts
`-retention-fault-baseline <prior-after-prune.json>`. Restore the failed fixture
through ordinary reconciliation first. Use a fresh output directory and a copy
of the original input bundle with a fresh checkpoint directory. The collector
requires exactly the baseline's twenty UIDs, their resumed specs and current
artifact references; it refuses replacements, changed targets and additional
workload resources. Its report declares `retention-fault-only`. Keep the owned
lab and fixture journal until the retry finishes, then run the original cleanup.
This mode reuses preparation and does not repeat the soak.

The approval backlog, overload cases and the full qualified
execution matrix remain required
by [the frozen profile](https://github.com/stokaro/ptah-operator/blob/master/support/qualification/0.2.0.md).

## The first reading {#lab-20}

The Capacity workflow ran the `lab-20` workload on 2026-09-26, run
[36204346798](https://github.com/stokaro/ptah-operator/actions/runs/36204346798):
ten `PtahSchema` and ten `PtahMigration` resources, each on a database of its
own, at an interval of two minutes. The report is kept in full in
[`support/capacity/readings/lab-20-2026-09-26.json`](https://github.com/stokaro/ptah-operator/blob/master/support/capacity/readings/lab-20-2026-09-26.json).

It ran on Kubernetes v1.37.0 in kind, with two manager replicas requesting 50m
of CPU and limited to 256 MiB. The report records 16 allocatable CPUs and
62 GiB across four nodes, which is one 4-CPU, 16 GiB runner counted four times:
every kind node reports the machine it runs on.

| Scenario | Seconds | Jobs/min avg (peak) | Job done p50/p95 s | Pending Pods max | Oldest reading s | Overdue s | Manager RSS MiB | Manager cores | Queue wait p95 s | Throttled s | Admission p95 s | Outcome |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cold start | 113 | 63.9 (64) | 11 / 19 | 12 | 50 | 0 | 64 | 0.16 | <= 10 | 0.0 | <= 0.5 | converged in 1m52s |
| steady state | 900 | 27.9 (52) | 6 / 10 | 7 | 154 | 0 | 77 | 0.06 | <= 1 | 0.0 | <= 0.1 | |
| restart burst | 146 | 26.0 (42) | 6 / 11 | 5 | 170 | 10 | 77 | 0.06 | <= 1 | 0.0 | <= 0.5 | approval admitted in 22s, dispatched in 26s, converged in 1m59s |
| change batch | 142 | 41.4 (59) | 8 / 17 | 10 | 154 | 0 | 81 | 0.12 | <= 1 | 0.0 | <= 0.5 | 10 resources moved, converged in 2m22s |
| registry outage | 180 | 15.3 (25) | 65 / 71 | 2 | 268 | 42 | 75 | 0.03 | <= 0.1 | 0.0 | <= 0.1 | |
| recovery | 55 | 50.1 (44) | 14 / 24 | 17 | 313 | 2 | 75 | 0.13 | <= 1 | 0.0 | <= 0.5 | converged in 55s |

Across the run the plan store grew from nothing to 31 plans and 15 chunk
ConfigMaps holding 11,330 bytes of plan; chunks were ConfigMaps when this was
measured, and are `PtahSchemaPlanChunk` objects now. Small schemas make small
plans, and a real schema's plans are larger by as much as its SQL is.

What it shows is the shape of each figure and how the tool reads it. The
steady state ran about 28 Jobs a minute for twenty resources at two minutes,
below the 35 that four and three operations every two minutes would give, so
the cadence arithmetic above is an upper bound rather than a prediction. A
restart of both managers put
nothing behind by more than ten seconds and admitted an approval in the middle
of it within 22. The registry outage is the one scenario that slowed anything,
and it slowed the operations that need the registry, which is what it is for.
Nothing waited on client throttling.

What it does not show is any installation's limit. Twenty resources on one
runner measure the operator well inside what the machine could give it; the
figures that decide a budget are where these curves bend, and this workload
never reached one.

The acceptance record carries these figures in one place, as the operating
targets and recovery objectives of a profile scoped to this lab:
[`support/acceptance/lab-20.json`](https://github.com/stokaro/ptah-operator/blob/master/support/acceptance/lab-20.json),
which `make acceptance-record ACCEPTANCE_PROFILE=support/acceptance/lab-20.json`
reads. It says the lab restores no database and claims no recovery point or
time for one. A test rebuilds its text from the report above, so the profile
and the reading cannot disagree.

## What has not been measured

Everything that decides whether an installation is inside its budget, at that
installation's scale:

- reconciliation lag and queue delay at a given resource count;
- Job creation rate and completion latency under a simultaneous refresh after a
  restart, which is the burst every installation gets on every rollout;
- manager CPU and resident memory against a named workload;
- API throttling and admission latency;
- fairness across independent database realms, and the time to reach
  safety-critical work while ordinary work is queued;
- behavior beyond the admitted limits: whether it degrades or refuses.

The reading above is one workload on one runner; it bounds none of these for
anyone else. An installation that needs a number for any of them has to measure
it. Saying so is more useful than a figure nobody produced.

## Approval backlog

Run `CAPACITY_VARIED_INPUTS=1 CAPACITY_WORKLOAD=support/capacity/backlog.json`
with `hack/capacity.sh` to measure approval waiting and execution separately.
The scenario starts with twenty populated resources. It changes five resources
of each family to their next artifacts and `OnApproval` in the same update,
keeps every resource UID, and waits a complete configured refresh interval.
Database checkpoints must still show the original schema defaults, row digests,
and migration histories before any approval is submitted.

Each approval binds the current resource and plan in that resource's namespace.
The five-minute execution budget begins at the earlier of the first approval's admission
stamp and API creation timestamp and includes the final database verification. The report retains
consumed approvals, successful Apply Jobs, plans and payloads, and continuous
watch history through the final resource versions. Missing bindings, an Apply
before its approval, a second Apply claim, a replaced resource, or an incomplete
watch prevents success. Waiting is reported separately from execution.

This workload adds the declared unrelated-object population by default. Its
implementation and unit tests do not establish a backlog capacity result; that
requires a completed run and comparison with the frozen profile's thresholds.
