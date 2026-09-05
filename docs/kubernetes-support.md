# Kubernetes support

Ptah Operator supports the three minor Kubernetes release branches that the Kubernetes project currently maintains. Every pull request and the weekly default-branch run execute the same complete PostgreSQL and MySQL reconciliation lifecycle against a real cluster for each supported minor; a version is not part of the supported window unless its matrix job is required and green. Every matrix cluster has three control-plane nodes and one worker. Upgrade and teardown barriers address each advertised API server directly, so a warm cache on one peer cannot hide stale authorization or admission state on another peer.

The admission sentinel proves its versioned credential fence and exact activation tuple on every ready API endpoint that remains continuously advertised for the full five-second stability window. The same policy contains the protected controller, certificate, hook, and cleanup caller and bound-TokenRequest origin checks. Seven relied-on credential and workload-provenance policies also expose mutually exclusive, content-versioned marker probes: the ServiceAccount-origin guard plus the six workload guards that constrain the executable identities behind those credentials. Each direct dry-run must return the one exact policy-and-binding denial for the sentinel or selected guard; a missing or stale cache entry cannot be hidden by another policy's denial. Every sweep also compares the stored policy and binding contracts exactly, so an old cached denial cannot hide a foreign replacement waiting to publish. EndpointSlice membership, identity, readiness, canonical address, stored contract, or probe result changes reset the window; the collection `resourceVersion` is pagination diagnostics and is deliberately not part of the topology fingerprint. Other retained functional guards remain covered by exact stored-object verification, but are not part of this per-endpoint credential/provenance publication claim. The optional certificate Secret-recovery pair is installed later and is fenced independently before certificate startup rather than being covered by this retained credential/provenance proof. Teardown convergence is scoped to validating admission policy/binding enforcement; it does not claim that both admission caches, or any webhook-configuration cache, are empty.

Final uninstall credential retirement uses a stricter closed topology. Before deleting its own cleanup ServiceAccount, the final Job freezes direct clients for the complete EndpointSlice inventory and opens a watch from that LIST resourceVersion. All frozen endpoints must reject the deleted token as `Unauthorized` for one continuous five-second interval while that watch remains unchanged and healthy. Any topology event, expired or closed watch, non-authentication error, or endpoint that still accepts the credential aborts the attempt. This makes control-plane membership changes visible even after the Job has intentionally removed its own permission to rediscover endpoints.

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

[`support/kubernetes.json`](../support/kubernetes.json) is the machine-readable source of truth. It records the ordered minor window, the kind version, digest-pinned multi-architecture node images, and the UTC date of the last persisted upstream-verification checkpoint. CI builds its matrix directly from this file; it does not maintain a second version list.

The Helm `kubeVersion` range is a derived, packaged value because a chart cannot read a repository file after publication. Run this guard after changing support metadata:

```sh
go run ./hack/verify-kubernetes-support.go
```

The command performs no network requests. It fails when the manifest is malformed, minors are not consecutive, an image is not digest-pinned to the matching minor, the chart range differs, automation no longer consumes the generated matrix, the compiled Kubernetes API boundary has not been reviewed for the complete window, or `lastVerified` is in the future or more than 35 UTC days old. The frozen API digest includes every Kubernetes Job and Pod spec field reachable by the hook workloads plus the Job and Pod status graphs relied on by protected progress reporting. A new field therefore requires an explicit guard review before the support window can advance. The updater runs weekly. It proposes any support-bundle change immediately and records a date-only verification checkpoint after 21 days, leaving three weekly opportunities to merge that checkpoint before the next day after the 35-day limit becomes stale. Tests inject a fixed validation date with `-now YYYY-MM-DD`; ordinary CI always uses the current UTC date.

Live discovery is deliberately separate:

```sh
make update-kubernetes-support
```

That command reads the official Kubernetes stable release, derives the consecutive three-minor maintained window, and selects a single stable official kind release containing digest-pinned node images for the entire window. It updates only the support manifest, the Helm `kubeVersion` range, and the generated table above.

## Moving the window

The weekly [`update-kubernetes-support.yml`](../.github/workflows/update-kubernetes-support.yml) workflow performs live discovery with read-only permissions and exports only a size-limited, digest-bound patch over the three generated support files. Its `-output=proposal` validation mode accepts either the already reviewed profile or exactly the immediate next supported minor while requiring the compiled dependency and reachable Job/Pod API digest to remain at the frozen reviewed boundary. This mode authorizes publishing discovery evidence only; it cannot produce a CI matrix or release evidence. A separate job with content and pull-request write access applies that patch, opens or updates the `automation/kubernetes-support-window` pull request, and exports the exact pushed commit and pull-request identity. It replaces an existing automation branch only when the remote head is already an ancestor of the validated base or is exactly one untouched bot-authored support commit over such an ancestor, with no mode changes or paths outside the generated support bundle; any unmerged human review commit makes the refresh fail closed instead of overwriting that work. A final job has Actions write access but no content or pull-request write access; it revalidates the remote branch and open pull request against those outputs, explicitly dispatches the CI and Release workflows, and confirms that both new manual runs resolve to that commit. The token-generated update therefore receives the same source verification, complete three-minor cluster matrix, and release-package smoke checks as a human-authored pull request without combining repository publication and workflow-dispatch authority in one job.

When Kubernetes publishes a new minor release, the automation follows this policy:

1. Read the official stable release and derive the current and previous two consecutive minors.
2. Select one stable official kind release containing digest-pinned node images for all three minors. This keeps the kind binary and node-image set tied to one published compatibility bundle.
3. Replace the oldest entry in `support/kubernetes.json`, keeping exactly three consecutive, ascending minors.
4. Update the derived `kubeVersion` field in `charts/ptah-operator/Chart.yaml` to the exact range represented by the proposed manifest.
5. If the maximum minor changed, review and update the compiled Kubernetes dependencies, the reachable Job/Pod API boundary, the structural admission guard, and their frozen digests in the proposed branch. Ordinary verification deliberately remains red until this review is explicit.
6. Merge the window change only after all three cluster jobs pass. The merge makes adding the new minor and removing the old minor one atomic support-policy change.

If no reproducible cluster image exists yet, the window does not move on an untested assumption. Prepare and verify a reproducible image first, then use its immutable reference in the manifest.

If live discovery or parsing fails, the workflow makes no support claim and opens no partial update. The deterministic 35-day freshness gate eventually fails normal verification, making a broken updater or an unreviewed support lag visible.

## Patch refreshes

Patch-level image refreshes are independent from minor-window changes. The same scheduled updater inspects official kind releases, opens or refreshes a pull request changing `kindVersion`, `lastVerified`, and affected `nodeImage` values, and lets the full matrix validate the update. A tag alone is not accepted: the manifest requires the registry digest so a later tag mutation cannot change CI silently.

The matrix installs the chart, serves the CRDs, and exercises OCI resolution and verification, observation, planning, approval, application, failure recovery, and post-apply convergence. It also verifies the exact three-control-plane, one-worker topology, requires every control-plane component to run once on every control-plane node, and configures the isolated registry on every node. The suite must consume the generated matrix and must not introduce a private list of Kubernetes versions.
