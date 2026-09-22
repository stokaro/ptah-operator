---
title: Release lifecycle
description: What install, upgrade, retirement and uninstall do, and what each refuses.
---

A release is installed, upgraded and removed by Helm hooks that refuse more
than they do. The runbooks for performing any of it are
[Operations](../../use/operations/); what a release promises about objects
you have stored is [API compatibility](../../support/api-compatibility/).

## The release lifecycle

`ptah-crd-manager` runs as Helm hooks. Each mode validates its preconditions
before proceeding:

| Mode | What it decides |
| --- | --- |
| `identity-probe` | Whether the live release identity matches the one being installed |
| `preflight` | Whether the install or upgrade may start at all: ownership, RBAC, namespaces, PriorityClass, resource quota |
| `reconcile` | The CRDs themselves, against the stored schema history |
| `verify`, `runtime-verify` | That what was applied is what runs |
| `teardown-retirement-probe-a`, `teardown-retirement-gate` | Whether a predecessor epoch may be retired |
| `teardown-quiesce`, `teardown`, `teardown-retirement-final` | Uninstall, privilege teardown and the final retirement record |

A refusal is written to the container's termination message as well as to
stderr, because Helm reports only that a hook Job failed: without that, an
operator is told an uninstall was refused and never why.

`ptah-cert-rotator` owns the webhook serving certificates. One reconciliation
at a time, serialized by a Lease of its own: it issues or renews the CA
through a staged transition, issues the serving certificate, repairs the trust
bundles on both webhook configurations, and probes every endpoint directly to
confirm the replacement is being served before it adopts it. A transition is
accepted only after every directly addressed API server observes both canary
webhooks continuously for a stability window. The manager never receives
permission to read the Secret this writes.
