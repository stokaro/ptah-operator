---
title: Ptah Operator
description: A Kubernetes control plane that converges PostgreSQL and MySQL schemas from immutable OCI artifacts.
# Starlight appends the site title to every page title. This page is named
# after the site, so without the override its tab reads the name twice.
head:
  - tag: title
    content: Ptah Operator
---

Ptah Operator is a Kubernetes-native control plane for continuously converging
PostgreSQL and MySQL schemas from immutable OCI artifacts. Database work runs
in short-lived, hardened Jobs; the controller itself has no permission to read
database Secrets.

The API is currently `v1alpha1`. Treat it as an implementation preview until
the complete database end-to-end matrix is green and a release is published.

## Reconciliation model

```text
resolve tag to digest -> verify artifact -> observe database -> publish plan
       ^                                                        |
       |                         approval (when required) <------+
       |                                                        |
       +--- verify convergence <- apply exact approved plan <---+
```

The controller records a claim before creating each Job, permits at most one
active Job per `PtahSchema`, and resumes that claim after a restart. A process
exit is never treated as proof of convergence: every apply is followed by a
new read-only observation.

Key safety properties:

- OCI tags are resolved once and all later artifact access uses the digest.
- Plans bind exact bytes to artifact, target, observed state, policy, the
  digest-pinned manager image, manager revision and state semantics, Ptah
  version, executor image, runner image, and runner protocol.
- Approvals are separate immutable resources stamped with authenticated
  admission identity.
- Destructive plans are disabled by default and still require an exact-plan
  approval when enabled.
- Database and registry credentials are isolated from each other and are never
  placed in status, Events, plan resources, or command arguments.
- Deletion and suspension never execute cleanup SQL.
- An uncertain apply outcome always returns to observation instead of replay.

## Where to go next

[Install](start/install.md) the chart, then give it
[a first schema](start/first-schema.md). What the operator does with that
schema afterwards is [operations](use/operations.md), and what it refuses is
[the security model](use/security.md).

## Ptah itself

The artifacts this operator applies are built with Ptah, and the schema
formats, the OCI artifact layout and the CLI are documented on
[Ptah's own site](https://docs.ptah.run/edge/). Which Ptah build this operator
runs, and which have actually been verified with it, is
[Ptah compatibility](support/ptah.md).
