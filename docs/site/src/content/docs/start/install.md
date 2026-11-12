---
title: Install
description: Installing a published release with one command, and the values to set when you select your own images.
---

This is the path to your own cluster and your own database. To see the
operator run against a cluster and database it builds for itself, read
[Try it locally](../try-it/) instead.

## What you need

- A Kubernetes cluster in the supported window, and cluster-admin on it. The
  window is [Kubernetes support](../../support/kubernetes/).
- Operation Pods must be able to reach the manager's result Service over HTTPS.
  The default installation uses durable results and needs no kubelet log-size
  change. See [Result delivery](../../support/kubernetes/#result-log-retention).
- Helm 4 or newer. Helm 3 is not supported and is not tested: it reaches end of
  life before this operator's first release.
- `kubectl`, and `jq` for the commands on the pages that follow.
- The `kubectl ptah` plugin, which [Read a plan](../../use/read-a-plan/#install)
  installs with its checksum checked. It installs the operator below, and reads
  what the operator planned afterwards.
- The chart, at a version you chose deliberately. The next section is how.
- A database the operator can reach, which [First schema](../first-schema/)
  needs rather than the install. A registry is optional and comes
  [later on that page](../first-schema/#from-a-registry).

## Choose a version, once

Every command on these pages comes from one version: the chart, the examples
beside it, and the images it names. Mixing them is the common way a first
install fails, so choose once and stay there.

A published release is the reproducible choice. From v0.2.10, each one publishes
its chart to an OCI registry with its images already pinned, so installing it
takes one command and no values. Check who built the chart first:

```sh
VERSION=<release version, such as 0.2.11>
gh attestation verify "oci://ghcr.io/stokaro/charts/ptah-operator:$VERSION" \
  --repo stokaro/ptah-operator \
  --signer-workflow stokaro/ptah-operator/.github/workflows/release.yml
kubectl ptah install --version "$VERSION"
```

`kubectl ptah install` installs that chart with Helm, passes it no image, and
waits until the manager and the certificate rotator are available. Without
`--version` it installs the release the plugin came from; a plugin built from a
checkout names no release and asks for one. It prints the Helm command before
it runs it, and that command is all it does:

```sh
helm upgrade --install ptah-operator oci://ghcr.io/stokaro/charts/ptah-operator \
  --version "$VERSION" \
  --namespace ptah-system \
  --create-namespace \
  --wait \
  --timeout 5m0s
```

Run it yourself where the plugin is not installed. Either way the chart carries
the manager, runner and executor digests and the verified Ptah version of its
release, and `helm show chart` prints them. They are the ones the release
manifest names, and plans and status record them as they record values you
set.
[Install from the registry](../../support/releases/#install-from-the-registry)
verifies the chart against the release manifest and installs it by digest, with
its signature checked, for an installation that wants to pin that too. An upgrade
that moves to a release with a new manager image also needs `--force-conflicts`,
as [Upgrade](../../use/operations/#upgrade-run) says, so upgrade with Helm
rather than with the plugin.

`kubectl ptah install` takes a few more flags:

- `--approver <username>` binds the chart's approver ClusterRole to that user in
  every namespace, through the `approverClusterRole.subjects` value, so the
  person can approve the first plan without another step. Repeat it for more
  people. A RoleBinding of your own keeps an approver to fewer namespaces, as
  [Approvals](../../use/approvals/) describes. Run the command again without
  the flag and the binding goes.
- `--yaml` prints the objects the install would apply, rendered by
  `helm template` from the same chart, version and values, and `--out-dir <dir>`
  writes them under `<dir>/ptah-operator` instead, for a GitOps repository. The
  render reads no cluster, so the checks below that read one do not run, and the
  webhook certificate and key it generates are new on every render. Provide the
  certificate yourself before committing a render, as
  [Operations](../../use/operations/#install-before) describes.
- `-f, --values <file>` hands the chart a values file. The chart takes other
  images only as the next section says.
- `-n, --namespace`, `--kubeconfig`, `--context` and `--timeout` mean what they
  mean to Helm.

A release before v0.2.10 has no chart in a registry, so it installs from its
assets with the values below.

### Select your own images

The published chart takes other images, one whole selection at a time:
`execution.executorImage` together with `execution.ptahVersion`, and
`image.digest` together with `execution.runnerImage`. It refuses half of either
pair, so an image and a version cannot drift apart.

To start from no defaults at all, use the chart asset a release ships as a `.tgz`
with checksums, signatures and build provenance.
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

`./charts/ptah-operator` without a selected commit is whatever the working tree
happens to hold.

The chart asset and the chart in the source tree carry no images, so the manager,
runner, and Ptah executor images must be selected explicitly. All three are
required to use immutable SHA-256 references. The executor version is explicit
too: it must identify the verified build in the selected executor digest and is
never inferred from an image tag.

A release names every one of them in its `release-manifest.txt`. The digest in
`image` is `<operator-image-digest>`, and the same image is the runner. The
executor is the Ptah release image the operator was tested with, unchanged:
the digest in `executor` is `<executor-image-digest>`, and
`executor-ptah-version` is the `<ptah-version>` to pass with it. This chart does not read the manifest,
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
  --set-string execution.executorImage=ghcr.io/stokaro/ptah@sha256:<executor-image-digest> \
  --set-string execution.ptahVersion=<ptah-version>
```

Install into a namespace of its own, and treat it as part of the cluster's
control plane for as long as the release exists. Whoever can create or modify
a workload in a namespace can run it as any ServiceAccount there, including the
operator's, so every principal with that right in the release namespace is a
Ptah administrator. Keep application workloads out of it, and grant
namespace-admin or workload-creation access there only to people you trust to
administer Ptah. A separate `coordination.namespace` is held the same way.
[Security model](../../use/security/#release-namespace) states the contract,
and [Operations](../../use/operations/#install-before) lists what else an
installation has to satisfy.

The chart refuses to install into `default`, into a `kube-*` namespace, or
into one that already runs workloads that are not this release's. Its notes
warn about RoleBindings there that would make somebody else a Ptah
administrator. Both read the cluster, so neither runs under `helm template` or
a GitOps tool that renders with it;
[What the chart checks](../../use/security/#release-namespace-check) says what
else they cannot see.

The version is recorded in plans, approvals, Jobs, and applied status alongside
the executor digest, whichever chart supplied them. When you select an executor
yourself, verify both values against the Ptah release before installation;
changing the digest requires verifying and supplying its version again. Which
Ptah builds have been run against this operator is
[Ptah compatibility](../../support/ptah/).

The chart supports the Kubernetes window documented in
[Kubernetes support](../../support/kubernetes/). It binds the optional
approver ClusterRole to nobody unless `approverClusterRole.subjects` names
someone, which is what `--approver` sets.

## Confirm it installed

Helm reports success when its hooks completed, which is not the same as the
manager serving. Two readings settle it:

```sh
kubectl -n ptah-system wait --for=condition=Available deployment --all --timeout=5m
kubectl get crd -o json | jq -r '
  .items[] | select(.spec.group == "operator.ptah.run") |
  "\(.metadata.name)\t\([.status.conditions[]? | select(.type == "Established") | .status] | first // "unknown")"'
```

Ten CRDs, each `Established=True`, and the manager and certificate-rotator
Deployments available. Until the webhook certificate has been issued and
accepted the admission webhooks reject writes, so a `PtahSchema` created in the
first seconds can be refused; the wait above is what settles it.

Every value the chart takes is in [configuration](../../use/configuration/).

## The client

The plugin that installed the operator is also how you read what it planned or
applied. Its read commands only read; `kubectl ptah approve` creates the
approval of a plan you reviewed, as you, and `suspend`, `resume`, `refresh` and
`set-source` patch a schema or a migration, as you. [Read a plan](../../use/read-a-plan/#install)
carries the download, the checksum check and the RBAC each command needs.
