---
title: Capacity
description: What the operator costs per resource, what the defaults are, and what has not been measured.
---

No capacity envelope has been measured. This page is the arithmetic a
deployment can do before it runs anything, and the defaults it would be running
with. Neither is a throughput claim, and both are here because the alternative
is a reader assuming the defaults fit their scale.

What an installation needs before it can size itself is on
[issue #224](https://github.com/stokaro/ptah-operator/issues/224): measured
latency, queue delay, API demand, CPU, memory, Pod count and retained growth,
against a declared workload. Until that exists, treat what follows as the
lower bound on cost rather than as a limit.

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

## What has not been measured

Everything that decides whether an installation is inside its budget:

- reconciliation lag and queue delay at a given resource count;
- Job creation rate and completion latency under a simultaneous refresh after a
  restart, which is the burst every installation gets on every rollout;
- manager CPU and resident memory against a named workload;
- API throttling and admission latency;
- fairness across independent database realms, and the time to reach
  safety-critical work while ordinary work is queued;
- behavior beyond the admitted limits: whether it degrades or refuses.

An installation that needs a number for any of those has to measure it. Saying
so is more useful than a figure nobody produced.
