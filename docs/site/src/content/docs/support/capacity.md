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

## What has not been measured

Everything that decides whether an installation is inside its budget:

- reconciliation lag and queue delay at a given resource count;
- Job creation rate and completion latency under a simultaneous refresh after a
  restart, which is the burst every installation gets on every rollout;
- manager CPU and resident memory against a named workload;
- API throttling and admission latency;
- fairness across independent database realms, and the time to reach
  safety-critical work while ordinary work is queued;
- behaviour beyond the admitted limits: whether it degrades or refuses.

An installation that needs a number for any of those has to measure it. Saying
so is more useful than a figure nobody produced.
