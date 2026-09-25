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

## 0.1.0

**Before you upgrade.** Nothing. This is the first version, so there is no
earlier release to move a cluster from.

Installing it is [Install](../../start/install/), and the database privileges
it needs before the first schema converges are
[Databases and privileges](../databases/).
