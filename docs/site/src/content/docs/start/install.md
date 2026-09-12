---
title: Install
description: Installing the chart, and the three values it refuses to guess.
---

Helm 4 or newer is required. Helm 3 is not supported and is not tested: it
reaches end of life before this operator's first release.

The manager, runner, and Ptah executor images must be selected explicitly. All
three are required to use immutable SHA-256 references. The executor version
is explicit too: it must identify the verified build in the selected executor
digest and is never inferred from an image tag or supplied by a chart default.

```sh
helm upgrade --install ptah-operator ./charts/ptah-operator \
  --namespace ptah-system \
  --create-namespace \
  --set-string image.digest=sha256:<operator-image-digest> \
  --set-string execution.runnerImage=ghcr.io/stokaro/ptah-operator@sha256:<operator-image-digest> \
  --set-string execution.executorImage=ghcr.io/stokaro/ptah@sha256:<ptah-image-digest> \
  --set-string execution.ptahVersion=<ptah-version>
```

Use a dedicated release namespace and keep both its first installation and its
first upgrade from a release without retained hook-progress protection under
exclusive administrative control until Helm reports success. Before the v2
hook-progress policies have converged on every API server, Kubernetes cannot
let an in-chart Job prove its own status and deletion integrity against an
already-authorized concurrent namespace writer. Later upgrades and uninstalls
between v2-aware releases do not rely on that bootstrap assumption. The exact
trust boundary is documented in
[Operations](../use/operations.md#installation-and-upgrades).

The supplied version is recorded in plans, approvals, Jobs, and applied status
alongside the executor digest. Verify both values from the executor's release
provenance before installation; changing the digest requires verifying and
supplying its version again. Which Ptah builds have been run against this
operator is [Ptah compatibility](../support/ptah.md).

The chart supports the Kubernetes window documented in
[Kubernetes support](../support/kubernetes.md). It intentionally does not
bind the optional approver ClusterRole to any identity.

Every value the chart takes is in [configuration](../use/configuration.md).
