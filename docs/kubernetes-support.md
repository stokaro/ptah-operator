# Kubernetes support

Ptah Operator supports the three minor Kubernetes release branches that the Kubernetes project currently maintains. Every pull request and the weekly default-branch run execute the same API smoke suite against a real cluster for each supported minor; a version is not part of the supported window unless its matrix job is required and green.

The current window is:

<!-- BEGIN GENERATED KUBERNETES SUPPORT -->
| Kubernetes minor | CI node image |
| --- | --- |
| 1.35 | `kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95` |
| 1.36 | `kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5` |
| 1.37 | `kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5` |
<!-- END GENERATED KUBERNETES SUPPORT -->

The exact patch versions in this table are test environments, not a restriction to those patches. The compatibility contract covers every supported minor at a currently available patch level. Production clusters should run a patch release still maintained by the Kubernetes project.

## Source of truth

[`support/kubernetes.json`](../support/kubernetes.json) is the machine-readable source of truth. It records the ordered minor window, the kind version, and digest-pinned multi-architecture node images. CI builds its matrix directly from this file; it does not maintain a second version list.

The Helm `kubeVersion` range is a derived, packaged value because a chart cannot read a repository file after publication. Run this guard after changing support metadata:

```sh
go run ./hack/verify-kubernetes-support.go
```

The command fails when the manifest is malformed, minors are not consecutive, an image is not digest-pinned to the matching minor, the chart range differs, or CI no longer consumes the generated matrix.

## Moving the window

When Kubernetes publishes a new minor release:

1. Verify the new branch is in the upstream maintained set.
2. Select an official kind node image for the new minor and verify its multi-architecture registry digest. Update `kindVersion` if the image needs a newer kind release.
3. Replace the oldest entry in `support/kubernetes.json`, keeping exactly three consecutive, ascending minors.
4. Update the derived `kubeVersion` field in `charts/ptah-operator/Chart.yaml` to the range printed by `go run ./hack/verify-kubernetes-support.go -output=helm-range`.
5. Merge the window change only after all three cluster jobs pass. The merge makes adding the new minor and removing the old minor one atomic support-policy change.

If no reproducible cluster image exists yet, the window does not move on an untested assumption. Prepare and verify a reproducible image first, then use its immutable reference in the manifest.

## Patch refreshes

Patch-level image refreshes are independent from minor-window changes. Automation should periodically inspect official kind releases, open a pull request that changes only `kindVersion`, `lastVerified`, and affected `nodeImage` values, and let the full matrix validate the update. A tag alone is not accepted: the manifest requires the registry digest so a later tag mutation cannot change CI silently.

The smoke matrix establishes CRDs on each Kubernetes API server and verifies the served resources. Database and reconciliation end-to-end suites use the same generated matrix as they are added; they must not introduce a private list of Kubernetes versions.
