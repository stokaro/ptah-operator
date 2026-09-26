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
| Plan size | A plan travels as ConfigMap chunks up to the 8 MiB ceiling, so it costs API bytes, etcd, and a longer read on the way back |
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

`support/capacity/workload.json` declares the workload: how many resources of
each family, their interval, how long each condition lasts, how many resources
change at once, and how long the registry is unreachable. The harness gives
every resource a database of its own, so no two share a realm or an advisory
lock, and walks the workload through a cold start, a steady state, a restart of
every manager at once, a batch of changes and a registry outage.

Throughout, it reads the cluster rather than estimating it:

| Figure | Read from |
| --- | --- |
| Jobs per minute, time to start and to finish | Each operation Job's own timestamps |
| Pending and running operation Pods | The Pods, by phase |
| Oldest reading and time overdue | Each resource's `lastObservedAt` or `history.observedAt`, and its `nextReconciliationTime` |
| Manager memory and CPU | Each manager Pod's `process_resident_memory_bytes` and `process_cpu_seconds_total` |
| Queue depth and wait | `workqueue_depth` and `workqueue_queue_duration_seconds` |
| Client throttling | `rest_client_rate_limiter_duration_seconds` and HTTP 429 responses |
| Admission latency | The API server's `apiserver_admission_webhook_admission_duration_seconds` for this operator's webhooks |
| Retained plans and SQL | The plan objects and the bytes in their chunk ConfigMaps |
| Time to serve an approval during a restart | The approval's admission and its Apply Job's creation |

The report carries the workload and the environment beside the figures: the
Kubernetes version, the nodes' allocatable CPU and memory, and the manager's
image and resources. A figure without them says nothing about another
installation. The Capacity workflow runs the same thing on a schedule, on
request, and on any change to the measurement itself, and publishes the report
as an artifact of the run.

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
ConfigMaps holding 11,330 bytes of plan: small schemas make small plans, and a
real schema's plans are larger by as much as its SQL is.

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
