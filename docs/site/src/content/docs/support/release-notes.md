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

## 0.2.2

**Before you upgrade.** Nothing has to change. The upgrade replaces the
manager image, so run it as
[Upgrade to a new release](../../use/operations/#upgrade) says, with
`--force-conflicts`.

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
