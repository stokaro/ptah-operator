# Releases and provenance

Every `v<chart-version>` tag publishes one version-addressed release set:

- a `linux/amd64` and `linux/arm64` manager/runner image;
- a reproducible Helm chart asset whose version and `appVersion` match the tag;
- a keyless signature and GitHub build provenance for the image digest;
- GitHub build provenance for every downloadable asset;
- the reproducibly packaged chart, a digest manifest, and SHA-256 checksums.

The release workflow refuses a mismatched tag, mutable external Docker image
input, unpinned action, incomplete publish permission set, or manager image tag
that differs from the chart version. It publishes no version or `latest` image
alias.

## Publication transaction

A fresh transaction first creates and attests a minimal `state=prepared` journal
that binds the tag, source commit, stable transaction ID, expected chart name,
and exact `ghcr.io/stokaro/ptah-operator:tx-<source-sha>-<run-id>` retention tag.
It stores those exact bytes as the body of an empty draft release and verifies
the body before any registry push. A rerun keeps the run ID recorded in that
journal; the attempt number is deliberately not part of the transaction
identity.

After the prepared draft exists, the workflow anonymously inspects its exact
retention tag. An existing raw manifest is reused only when its exact digest has
an authenticated build checkpoint from this release workflow, repository, tag,
and source commit. That checkpoint is created solely from the digest returned
by a successful image-build action. A missing or uncheckpointed tag is rebuilt;
the replacement cannot become reusable until the new build output has its own
checkpoint. The registry's one-line missing response must name the exact tag
that was inspected. An unavailable, multiline, or otherwise ambiguous response
fails closed. The transaction tag is not a release identity and must never be
consumed. The final authenticated manifest records the digest reference. The
image includes a maximal BuildKit provenance record and SBOM. Chart packaging
normalizes all source timestamps to the source commit time and CI requires two
independent packages to be byte-identical.

The workflow attests `release-manifest.txt`, compares the prepared draft body,
then replaces that body with the same authenticated final manifest bytes before
synchronizing the three release assets. It never replaces uploaded asset bytes.
A failed upload may leave an empty `starter` asset; recovery deletes only that
exact incomplete asset ID and uploads the journaled bytes again. Any uploaded
mismatch, duplicate name, unexpected asset, or unknown state fails closed.
Image signing, attestation, and anonymous digest pull verification all finish
before the draft is published.

The build boundary is versioned as data. Actions use audited commit pins,
Buildx uses an exact version, and its BuildKit daemon image is selected by
multi-architecture digest with the action's binary cache disabled. The BuildKit
SBOM generator is also selected by digest rather than its mutable convenience
tag. The manager binary is compiled in the digest-pinned Docker builder stage
and runs on a digest-pinned non-root base. The publish job also disables the Go
action cache; its small release verifiers and deterministic chart packager are
rebuilt from the checked-out source and checksum-locked modules for every
transaction.

Rerunning a partially completed release first authenticates either the prepared
journal or final manifest against the exact tag, source commit, and signer
workflow. From a prepared journal it reuses an existing staged digest only
after verifying the exact build checkpoint described above; otherwise it
rebuilds into the same transaction tag and checkpoints the action's returned
digest. It reproduces the chart bytes deterministically and resumes missing
additive steps. A published immutable release is a read-only recovery state:
the workflow re-authenticates its tag, body, assets, image signature, image
attestations, and anonymous image availability, then succeeds without mutation.
A published but mutable release, a moved source tag, a mismatched asset, or an
unavailable state check fails closed. No step uses asset replacement.

Before publication, the image gate compares the transaction retention tag with
the manifest-list digest recorded in the final release manifest. It requires
exactly the `linux/amd64` and `linux/arm64` runtime manifests, validates source,
revision, and version labels on each image, and binds each platform to its own
SBOM and maximal BuildKit provenance attestation. The provenance must carry the
release build arguments and detailed build graph. A single fail-closed parser
enumerates external image inputs from case-insensitive Dockerfile instructions,
line continuations, resolved `ARG` defaults, every `FROM`, every external
`COPY --from`, and every external `RUN --mount` source. Unsupported parser
constructs and unresolved or mutable references are rejected. The publish build
arguments are structurally fixed so they cannot override an enumerated image
reference. The pinned Dockerfile syntax frontend is an external input too. Every
enumerated digest must occur as an exact SHA-256 value in each platform's
structured `resolvedDependencies`; merely placing a digest string elsewhere in
the predicate cannot satisfy the gate. The same exact material check separately
requires the pinned SBOM generator digest. This structural and material
verification finishes before the final image attestation or signature is
created. The same gate runs during published recovery.

Repository administrators must enable immutable releases before publishing the
first version. This makes the published tag and assets platform-enforced
immutable and adds a release attestation that binds the tag, commit, and asset
digests. Drafts intentionally remain mutable so the transaction can recover;
the workflow authenticates and compares their contents before publication. The
workflow requires a fine-grained `IMMUTABLE_RELEASES_READ_TOKEN` with repository
Administration read permission and checks the setting before pushing anything.
Keep this token only in the protected `release` environment, never as a general
repository secret. The final step requires both the API's immutable flag and a
valid release attestation.

Configure the `release` environment with required reviewers and restrict it to
release tags. Protect `v*` creation with a repository ruleset and require the
tagged commit to have passed the default-branch review and CI policy. The
workflow independently proves that the tagged commit is reachable from the
current default branch. These repository controls are part of the release trust
boundary: a workflow file loaded from an unreviewed tag must never receive the
environment secret or OIDC publication authority.

Administrators must also make the stable manager image package public before
the first release. The workflow logs out of GHCR and proves that the exact image
digest is anonymously readable before publishing the draft.

Immediately before publishing, the workflow re-fetches the draft body and exact
asset-name set, downloads every asset, compares its bytes with the locally
attested transaction, re-verifies each build attestation, and re-validates the
manifest/checksum/chart relation. GitHub does not expose an atomic
compare-and-publish API for drafts, so repository write access remains a trusted
administrative boundary during this short gate. Immutable-release enforcement
locks the tag and bytes at publication, after which the workflow verifies the
release attestation.

## Verify before installation

Choose the tag independently, resolve its commit, and authenticate the release
and all three assets before reading the manifest or trusting its checksums:

```sh
tag=v0.1.0
version=${tag#v}
repository=stokaro/ptah-operator
source_sha="$(gh api "repos/$repository/commits/$tag" --jq .sha)"

gh release verify "$tag" --repo "$repository"
gh release download "$tag" --repo "$repository" \
  --pattern "ptah-operator-$version.tgz" \
  --pattern release-manifest.txt \
  --pattern SHA256SUMS

for asset in \
  "ptah-operator-$version.tgz" \
  release-manifest.txt \
  SHA256SUMS
do
  gh release verify-asset "$tag" "$asset" --repo "$repository"
  gh attestation verify "$asset" \
    --repo "$repository" \
    --source-ref "refs/tags/$tag" \
    --source-digest "$source_sha" \
    --signer-workflow "$repository/.github/workflows/release.yml"
done

grep -Fx "version=$version" release-manifest.txt
grep -Fx "source-repository=$repository" release-manifest.txt
grep -Fx "source-ref=refs/tags/$tag" release-manifest.txt
grep -Fx "source-sha=$source_sha" release-manifest.txt
sha256sum --check SHA256SUMS
```

Only after those checks should the image reference be read from the manifest.
Verify its provenance against the same tag and commit:

```sh
image="$(sed -n 's/^image=//p' release-manifest.txt)"

gh attestation verify "oci://$image" \
  --repo "$repository" \
  --source-ref "refs/tags/$tag" \
  --source-digest "$source_sha" \
  --signer-workflow "$repository/.github/workflows/release.yml"
```

Cosign provides an independent signature check. Bind its certificate identity
to the exact selected tag, not to a wildcard release identity:

```sh
identity="https://github.com/$repository/.github/workflows/release.yml@refs/tags/$tag"
cosign verify \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer='https://token.actions.githubusercontent.com' \
  "$image"
```

## Install by digest

The chart asset, manager/runner image, and independently selected Ptah executor
are all pinned separately. This lets an operator release be promoted without
silently changing the database executable, and vice versa.

```sh
helm upgrade --install ptah-operator \
  "./ptah-operator-$version.tgz" \
  --namespace ptah-system \
  --create-namespace \
  --set-string "image.digest=${image##*@}" \
  --set-string "execution.runnerImage=$image" \
  --set-string execution.executorImage=ghcr.io/stokaro/ptah@sha256:<ptah-image-digest> \
  --set-string execution.ptahVersion=<ptah-version>
```

`execution.ptahVersion` has no chart default and is not inferred from the image
reference. Set it to the version identity established by the provenance of the
exact executor digest. The operator records that pair in plans, approvals,
operation Jobs, and applied status, so an executor change is an explicit new
execution binding even when the operator release is unchanged.

For an air-gapped promotion, carry the authenticated chart asset and recursively
copy the operator image, executor image, and every referenced schema artifact
by digest into the destination registry. Record the source and destination
digest mapping, then update only repository names in deployment values and
`PtahSchema` references. A promotion that changes a digest is a rebuild and
must go through verification again.
