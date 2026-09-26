---
title: Install
description: Installing the chart, and the three values it refuses to guess.
---

This is the path to your own cluster and your own database. To see the
operator run against a cluster and database it builds for itself, with no
digests to choose, read [Try it locally](../try-it/) instead.

## What you need

- A Kubernetes cluster in the supported window, and cluster-admin on it. The
  window is [Kubernetes support](../../support/kubernetes/).
- Helm 4 or newer. Helm 3 is not supported and is not tested: it reaches end of
  life before this operator's first release.
- `kubectl`, and `jq` for the commands on the pages that follow.
- The chart, at a version you chose deliberately. The next section is how.
- Three image digests and one Ptah version, covered below.
- A database the operator can reach, and an OCI registry it can read. Both are
  needed by [First schema](../first-schema/) rather than by the install.

## Choose a version, once

Every command on these pages comes from one version: the chart, the examples
beside it, and the images it names. Mixing them is the common way a first
install fails, so choose once and stay there.

A published release is the reproducible choice. Each one ships the chart as a
`.tgz` with checksums, signatures and build provenance, and
[Releases and provenance](../../support/releases/#verify-before-installation)
carries the verification.

```sh
VERSION=<release tag>
gh release download "$VERSION" --repo stokaro/ptah-operator \
  --pattern 'ptah-operator-*.tgz' --pattern release-manifest.txt --pattern SHA256SUMS
# Verify the assets before installing them, then use the .tgz below in place of
# ./charts/ptah-operator.
```

To run master instead, name the commit rather than the branch, and keep the
examples from the same commit:

```sh
git clone https://github.com/stokaro/ptah-operator
cd ptah-operator
git checkout <commit>
```

There is no third option: the chart is not published to a Helm repository, and
`./charts/ptah-operator` without a selected commit is whatever the working tree
happens to hold.

The manager, runner, and Ptah executor images must be selected explicitly. All
three are required to use immutable SHA-256 references. The executor version
is explicit too: it must identify the verified build in the selected executor
digest and is never inferred from an image tag or supplied by a chart default.

A release names every one of them in its `release-manifest.txt`. The digest in
`image` is `<operator-image-digest>`, and the same image is the runner. The
release also builds the executor from the Ptah commit it was tested with: the
digest in `executor` is `<executor-image-digest>`, and `executor-ptah-version`
is the `<ptah-version>` to pass with it. The chart does not read the manifest,
and it would take any other verified executor just as well; the values below
are where the choice is made. Authenticate the manifest and verify each image's
signature first, as
[Releases and provenance](../../support/releases/#verify-before-installation)
shows.

```sh
helm upgrade --install ptah-operator ./charts/ptah-operator \
  --namespace ptah-system \
  --create-namespace \
  --set-string image.digest=sha256:<operator-image-digest> \
  --set-string execution.runnerImage=ghcr.io/stokaro/ptah-operator@sha256:<operator-image-digest> \
  --set-string execution.executorImage=ghcr.io/stokaro/ptah-operator-executor@sha256:<executor-image-digest> \
  --set-string execution.ptahVersion=<ptah-version>
```

Use a dedicated release namespace and keep its first installation under
exclusive administrative control until Helm reports success. Before the
hook-progress policies that installation creates have converged on every API
server, Kubernetes cannot let an in-chart Job prove its own status and deletion
integrity against an already-authorized concurrent namespace writer. Every
release retains those policies, so later upgrades and uninstalls do not rely on
that assumption. The exact trust boundary is documented in
[Operations](../../use/operations/#install-before).

The supplied version is recorded in plans, approvals, Jobs, and applied status
alongside the executor digest. Verify both values from the executor's release
provenance before installation; changing the digest requires verifying and
supplying its version again. Which Ptah builds have been run against this
operator is [Ptah compatibility](../../support/ptah/).

The chart supports the Kubernetes window documented in
[Kubernetes support](../../support/kubernetes/). It intentionally does not
bind the optional approver ClusterRole to any identity.

## Confirm it installed

Helm reports success when its hooks completed, which is not the same as the
manager serving. Two readings settle it:

```sh
kubectl -n ptah-system wait --for=condition=Available deployment --all --timeout=5m
kubectl get crd -o json | jq -r '
  .items[] | select(.spec.group == "operator.ptah.run") |
  "\(.metadata.name)\t\([.status.conditions[]? | select(.type == "Established") | .status] | first // "unknown")"'
```

Six CRDs, each `Established=True`, and the manager and certificate-rotator
Deployments available. Until the webhook certificate has been issued and
accepted the admission webhooks reject writes, so a `PtahSchema` created in the
first seconds can be refused; the wait above is what settles it.

Every value the chart takes is in [configuration](../../use/configuration/).

## The client

The chart installs the operator. Reading what it planned or applied is a
separate install on your own machine: `kubectl ptah`, a read-only plugin
published with each release. [Read a plan](../../use/read-a-plan/#install)
carries the download, the checksum check and the RBAC it needs.
