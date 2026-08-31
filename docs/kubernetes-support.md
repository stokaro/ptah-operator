# Kubernetes support

Ptah Operator supports the three minor Kubernetes release branches that the Kubernetes project currently maintains. Every pull request and the weekly default-branch run execute the same complete PostgreSQL and MySQL reconciliation lifecycle against a real cluster for each supported minor; a version is not part of the supported window unless its matrix job is required and green.

The current window is:

<!-- BEGIN GENERATED KUBERNETES SUPPORT -->
| Kubernetes minor | CI node image |
| --- | --- |
| 1.35 | `kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0` |
| 1.36 | `kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed` |
| 1.37 | `kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5` |
<!-- END GENERATED KUBERNETES SUPPORT -->

The exact patch versions in this table are test environments, not a restriction to those patches. The compatibility contract covers every supported minor at a currently available patch level. Production clusters should run a patch release still maintained by the Kubernetes project.

## Source of truth

[`support/kubernetes.json`](../support/kubernetes.json) is the machine-readable source of truth. It records the ordered minor window, the kind version, digest-pinned multi-architecture node images, and the UTC date on which live upstream metadata was last checked. CI builds its matrix directly from this file; it does not maintain a second version list.

The Helm `kubeVersion` range is a derived, packaged value because a chart cannot read a repository file after publication. Run this guard after changing support metadata:

```sh
go run ./hack/verify-kubernetes-support.go
```

The command performs no network requests. It fails when the manifest is malformed, minors are not consecutive, an image is not digest-pinned to the matching minor, the chart range differs, automation no longer consumes the generated matrix, or `lastVerified` is in the future or more than 35 UTC days old. The updater runs weekly. It proposes any support-bundle change immediately and records a date-only verification checkpoint after 21 days, leaving three weekly opportunities to merge that checkpoint before the next day after the 35-day limit becomes stale. Tests inject a fixed validation date with `-now YYYY-MM-DD`; ordinary CI always uses the current UTC date.

Live discovery is deliberately separate:

```sh
make update-kubernetes-support
```

That command reads the official Kubernetes stable release, derives the consecutive three-minor maintained window, and selects a single stable official kind release containing digest-pinned node images for the entire window. It updates only the support manifest, the Helm `kubeVersion` range, and the generated table above.

## Moving the window

The weekly [`update-kubernetes-support.yml`](../.github/workflows/update-kubernetes-support.yml) workflow performs live discovery and opens or updates the `automation/kubernetes-support-window` pull request. It explicitly dispatches the regular CI workflow for the automation branch, ensuring the token-generated update receives the same source verification and complete three-minor cluster matrix as a human-authored pull request.

When Kubernetes publishes a new minor release, the automation follows this policy:

1. Read the official stable release and derive the current and previous two consecutive minors.
2. Select one stable official kind release containing digest-pinned node images for all three minors. This keeps the kind binary and node-image set tied to one published compatibility bundle.
3. Replace the oldest entry in `support/kubernetes.json`, keeping exactly three consecutive, ascending minors.
4. Update the derived `kubeVersion` field in `charts/ptah-operator/Chart.yaml` to the range printed by `go run ./hack/verify-kubernetes-support.go -output=helm-range`.
5. Merge the window change only after all three cluster jobs pass. The merge makes adding the new minor and removing the old minor one atomic support-policy change.

If no reproducible cluster image exists yet, the window does not move on an untested assumption. Prepare and verify a reproducible image first, then use its immutable reference in the manifest.

If live discovery or parsing fails, the workflow makes no support claim and opens no partial update. The deterministic 35-day freshness gate eventually fails normal verification, making a broken updater or an unreviewed support lag visible.

## Patch refreshes

Patch-level image refreshes are independent from minor-window changes. The same scheduled updater inspects official kind releases, opens or refreshes a pull request changing `kindVersion`, `lastVerified`, and affected `nodeImage` values, and lets the full matrix validate the update. A tag alone is not accepted: the manifest requires the registry digest so a later tag mutation cannot change CI silently.

The matrix installs the chart, serves the CRDs, and exercises OCI resolution and verification, observation, planning, approval, application, failure recovery, and post-apply convergence. The suite must consume the generated matrix and must not introduce a private list of Kubernetes versions.
