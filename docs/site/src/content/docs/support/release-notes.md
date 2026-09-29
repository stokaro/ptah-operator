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

## 0.1.0-rc.1

**Before you upgrade.** There is no earlier published release. Use this first
release candidate in an isolated evaluation cluster with disposable databases.
It is experimental and is not qualified for production.

The source acceptance matrix covers Kubernetes 1.35, 1.36, and 1.37. Publication
still has to prove the signed image and chart digests, anonymous image access,
and the immutable release transaction against the shipped bytes. The
[production qualification record](https://github.com/stokaro/ptah-operator/issues/242)
remains open: the `lab-20` profile is not assessed, and it excludes database
restore/RTO and production capacity/soak qualification. Follow the
[release candidate checks](../releases/#prepare-a-release-candidate) before
creating the tag.

Installing it is [Install](../../start/install/), and the database privileges
it needs before the first schema converges are
[Databases and privileges](../databases/).
