---
title: Release notes
description: What a cluster already running the previous version has to change before it moves to the next one.
---

One entry per version, newest first, answering one question: what a cluster
already running the version before it has to change before it moves.

This is not a commit log. What a release publishes and how to verify it before
you install it is [Releases and provenance](../releases/). What the API holds
to between versions, and what it does not, is
[API compatibility](../api-compatibility/).

Before `v1` there are no deprecations: a field that has to go, goes, in the
release that says so, and this is where it is said. A version that asks
nothing of you says so in as many words, rather than leaving you to read it
out of silence.

## 0.2.6

**Before you upgrade.** The manager image is new, so keep `--force-conflicts`
on the upgrade, and check the following.

- **Operation Pod memory.** The executor is now Ptah v0.14.0, which needs
  about twice the memory v0.13.0 did. The runner and Ptah share the operation
  container, and a 64Mi limit that 0.2.5 fit in gets Observe and Plan Pods
  OOM-killed; the resource then reports that the runner's result frame was not
  found. If `spec.execution.resources`, or the LimitRange default where it is
  unset, gives the container less than 128Mi, raise it to 128Mi, which is what
  the acceptance suite runs with.
- **Drift categories.** `status.target.driftFindings[].category` now holds the
  category exactly as Ptah reports it. Ptah v0.14.0 hands PostgreSQL extensions
  and the YDB objects to feature owners and counts a change to one under
  `feature_changes` followed by the type of object, as in
  `feature_changes:ptah.run/pgextension/extension-change`, and an owner's
  access verdict under `feature_access_widened`, `feature_access_narrowed`,
  `feature_access_unchanged` or `feature_access_unknown`. Thirty-one
  per-object categories, such as `extensions_added`, are no longer written. An
  alert rule, dashboard or script that matches those names stops matching.
  Objects the comparison could not decide are counted under `undecided`; 0.2.5
  refused a report that held it, and the Observe failed.
- **Approvals.** A new executor retires the execution binding, so a plan
  waiting for approval is planned again and needs a new approval. Applied
  state is not touched.

A failed or partial migration run now also says where in the migration it
stopped and what the database answered, without the database's text:

```text
Migration 2 (Read Ledger) failed at statement 2 of 3 with SQLSTATE 42P01 and committed nothing; ...
Migration 2 (Fill Ledger) committed 2 of its 4 statements, then failed with SQLSTATE 23000 (error 1062); ...
```

Each manager process now paces its requests to the Kubernetes API server:
80 a second with a burst of 40, shared by every client in the process, so one
process sends at most 4,840 in any minute. After a controller-manager leader
change, the Job controller can hold back Job completions for minutes and then
report them at once; the work they wake is now spread over the following
minutes instead of sent in one. An installation larger than the 0.2.0 profile
can raise `apiRequests.perSecond` and `apiRequests.burst`.

The binaries are built with Go 1.27.3, which fixes GO-2026-6809 in
`crypto/x509`, a path the manager reaches.

## 0.2.5

**Before you upgrade.** Nothing has to change. The manager image is new, so
keep `--force-conflicts` on the upgrade.

The chart's ServiceMonitor now selects the metrics Service. In 0.2.4 it
selected Services by a label only the manager Pods carry, so
`monitoring.serviceMonitor.enabled` gave Prometheus no target and every alert
rule read an absent series. The metrics Service now carries
`app.kubernetes.io/component: metrics`, and the ServiceMonitor selects that.
A ServiceMonitor you wrote yourself to work around it keeps working; remove it
once the chart's own one is scraped, so the managers are not scraped twice.

A failed or partial migration run now names the migration it stopped in, in
`status.lastRun.message` and in its `MigrationRunFinished` Event: its version,
the description Ptah reads from its file name, and for a partial run how many
of its statements it committed. The message still carries nothing the
database said.

`pkg/planread` is new. It reads a stored schema plan through the same checks
`kubectl ptah plan` makes, for a Go program that shows plans, such as the
optional console in stokaro/ptah-console. Nothing in the operator's own
behavior changes with it.

## 0.2.4

**Before you upgrade.** Nothing has to change. This upgrade swaps the manager
image as well, and `--force-conflicts` stays on the command.

Schemas and migrations that share an interval no longer refresh at the same
moment. Each Resolve set the next refresh exactly one interval after it
started, so resources created together stayed in step for as long as they
lived, and every interval began with the whole fleet's API traffic at once.
Each of them now takes an evenly spaced slot in its interval, in UID order,
and a refresh that started well past its slot sets the next one later onto it.
The next refresh is never earlier than one interval after the last; aligning
can lengthen one or two cycles by at most half an interval, after a resource
joins or leaves the fleet or drifts from its slot. A resource alone in its
interval keeps the plain cadence.

## 0.2.3

**Before you upgrade.** Nothing has to change. The manager image is new again,
so keep `--force-conflicts` on the upgrade.

Replacing every manager at once while an approval arrives no longer blocks the
migration or schema. Deleting all manager Pods removes the endpoints of the
manager's own Job-write webhook before either process is told to stop, so a
leader still dispatching had its Job create refused and left an approved Apply
that only a person could acknowledge. A create the API server refused before
storing it proves no Job exists, so the dispatch is now withdrawn and made
again by the next leader under the same approval. A create whose outcome is
uncertain, such as a timeout, still blocks for a person, as before.

## 0.2.2

**Before you upgrade.** Nothing has to change. As from 0.2.0, the manager image
changes, so the upgrade needs `--force-conflicts`.

A manager that is stopping no longer starts a Job, and one whose dispatch had
already begun finishes creating it. Before, an approval that arrived while the
managers were being replaced could be taken by the stopping manager, whose
interrupted create left an outcome nobody established, and a migration then
blocked until a person acknowledged the run. The manager now finishes stopping
within 20 seconds, inside the Pod's grace period, so it releases its leader
Lease before it is killed and the next manager takes over at once.

## 0.2.1

**Before you upgrade.** Nothing has to change. The upgrade replaces the
manager image, so run it as
[Upgrade to a new release](../../use/operations/#upgrade) says, with
`--force-conflicts`. One behavior changes:
`spec.interval` now runs from the start of one refresh to the start of the
next, as the API describes it. 0.2.0 waited one interval after each refresh
finished, so the refresh's own Jobs lengthened every period.

At the same interval a converged resource now refreshes more often. With the
default 10m and a refresh of about a minute, that is about one Job in ten more.
Raise `spec.interval` if that load matters. A published plan still waits a full
interval for its approval, and a refresh that takes longer than its interval
waits one interval after it ends rather than starting the next at once.

The manager's leader Lease now lasts 60 seconds and is renewed within 45, up
from 15 and 10, and a manager that stops releases it. An API server stall
shorter than 45 seconds no longer restarts the leader, and a restart or an
upgrade hands over without waiting out the Lease. A manager that dies without
stopping is replaced after about a minute instead of 15 seconds.

## 0.2.0

**Before you upgrade.** There is no earlier published release to upgrade from.

0.2.0 is the first published release. Its images, chart and clients were
qualified on their own digests across Kubernetes 1.35, 1.36 and 1.37 on amd64
and arm64, and published as an immutable release; see
[Releases and provenance](../releases/) to verify them.

Installing it is [Install](../../start/install/), and the database privileges
it needs before the first schema converges are
[Databases and privileges](../databases/).
