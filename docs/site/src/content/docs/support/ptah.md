---
title: Ptah compatibility
description: Which Ptah build this operator runs, what has been verified, and what nobody has measured.
---

The operator does not contain Ptah. It runs one, as a job whose container image
and version identity the installation binds, so "which Ptah works with this
operator" is a question about that executor and not about the CLI a person used
to build the OCI artifact.

`support/ptah.json` is the machine-readable answer, and it is canonical: the
lifecycle suite takes the commit it builds the executor from out of this file,
and the table below is generated from it.

## The table

Each operator version has a row in the summary and a section with the detail.
A declared range, a verified build and the absence of both mean different
things, and [the next section](#three-states-kept-apart) says what.

<!-- BEGIN GENERATED COMPATIBILITY -->
**Axis:** Operator version to the Ptah build the operator executes, identified by the digest-pinned executorImage and the ptahVersion bound beside it. It is not the Ptah CLI a person used to build the OCI artifact. The runner is not a third axis: it is built from the operator source this row names, and the exact protocol both halves speak is in runnerProtocolVersion.

**Last measured:** 2026-09-27.

| Operator | Declared range | Verified builds | Guide |
| --- | --- | --- | --- |
| `edge` | No range | `v0.9.0-73-gf6e562c5b`, not a release | [edge](https://operator.ptah.run/edge/) |

### edge

The development state. Its guide is at [https://operator.ptah.run/edge/](https://operator.ptah.run/edge/).

**Declared:** No range is claimed. The chart ships no executor default, installation requires both a digest-pinned executorImage and the ptahVersion verified from that image's provenance, and the supported build is exactly the one the lifecycle matrix exercised.

**Verified:**

- `v0.9.0-73-gf6e562c5b`, which is not a Ptah release. Commit [`f6e562c5b0986cd29a53a5cc01938827336b780a`](https://github.com/stokaro/ptah/commit/f6e562c5b0986cd29a53a5cc01938827336b780a), runner protocol version 1, evidence `kubernetes-e2e`. The complete OCI resolution and verification, observation, planning, approval, apply, failure-recovery and convergence lifecycle against PostgreSQL and MySQL, on every supported Kubernetes minor, for both artifact formats this operator reads: application/vnd.stokaro.ptah.schema.v1 for a declared schema, whose declared rows converge on both engines, and application/vnd.stokaro.ptah.migrations.v1 for a versioned migration directory, whose history read, approval gate and applied sequence run against PostgreSQL and MySQL.

**Limitations:**

- The verified commit is not a Ptah release. It descends from stokaro/ptah#3794, which completes the `--json` documents stokaro/ptah#3777 added to `schema plan` and `schema apply`: a run that panics still writes one, and `--json` reads no environment variable. The operator reads those documents on every declared-schema Plan and Apply. An executor without #3777, v0.9.0 included, rejects `--json` with exit code 2, so the declared-schema workflow needs at least that merge rather than any build that has the commands. The suite builds the executor image from the verified commit, and an installation has to build or obtain an image carrying at least it until a release does.
- The scope above was measured against v0.7.0 at 127aa2477. It holds for this commit when a complete lifecycle run repeats it, and this limitation stays until one has.
- The migration machine contracts -- `migrations status --json` carrying contract_version and a per-migration record, and `migrations up --json` carrying an outcome -- reached Ptah after v0.3.0-201, and `migrations up --expect-sequence`, which the operator passes on every migration Apply, reached it in v0.9.0. A build older than v0.9.0 resolves and verifies a migration artifact and then fails at its own history document or at the flag every migration Apply passes, so the versioned workflow needs at least v0.9.0 rather than any build that has the commands.
- The verified scope is the operator's database support window, PostgreSQL 17.x and MySQL 8.4.x LTS. Other engines Ptah addresses are unverified here, whatever the Ptah build.
- The published verified set is a repository document. It is not compiled into the manager, and nothing compares a running executor's ptahVersion against it, so a combination outside this set is outside what the matrix measured rather than something the operator refuses. What is refused before a database is mutated is a change of any bound execution component between the plan and the apply -- Ptah, the executor image, the runner protocol version, the controller-state version -- checked before the Job is created. The manager's image and revision and the runner image are recorded and not bound: a manager release that changes only them applies the plans and approvals its predecessor left. What is refused after a Job has run is a runner speaking another protocol version, which is a rejected result frame rather than a prevented mutation. Pinning both the executor image and its ptahVersion at install is a requirement on whoever installs, not a check at runtime.

### Evidence

- `kubernetes-e2e`: The full lifecycle suite in test/e2e, run by the kubernetes-e2e job of .github/workflows/ci.yml on every pull request and on the weekly default-branch run. hack/e2e-kind.sh and the workflow both take the commit from this file, so the Ptah the suite builds and the claim published here are one declaration.
<!-- END GENERATED COMPATIBILITY -->

## Three states, kept apart

A compatibility table usually blurs three different claims. This one keeps them
in separate fields, because the difference is what a reader is actually asking
about.

**Declared** is a promise. `declared.range` names the Ptah versions an operator
version is meant to work with. Where it is `null`, nothing is promised, and
`declared.statement` says why.

**Verified** is a measurement. Each `verified` entry names a Ptah build the
operator was actually run against, the evidence behind it, and what ran.
`ptahRelease` is `null` when the build is not a released Ptah, and the table
then names the build by `ptahDescribe`. The suite builds its executor from the
pinned commit rather than pulling a published image, so what the measurement
covers is the code at that commit. An operator release builds its executor from
the same commit with the same recipe, and
[Releases and provenance](../releases/#the-executor) says how that image is
checked against this row before it is signed.

**Absent** is neither. A combination no row mentions is untested, and untested
is not incompatible. A version with nothing verified carries
`unverifiedReason` naming the check that has not run; the verifier refuses an
empty list that says nothing, because an empty list reads as "works with
everything".

## What each field means

| Field | Meaning |
| --- | --- |
| `operator` | `edge` for the development state, or an exact `vMAJOR.MINOR.PATCH` release. |
| `stage` | `development` or `released`. |
| `documentation.published` | Whether a guide exists for this operator version. The table links to it only when it does. |
| `documentation.source` | The revision that guide is built from: `master` for the development state, and its own tag for a release. |
| `documentation.fixRevision` | An exact commit that replaces the tag as the build revision for one release. |
| `documentation.fixReason` | What that fix corrects. Required beside a fix revision. |
| `declared.range` | The promised Ptah versions, or `null`. |
| `declared.statement` | Why there is no range, when there is none. |
| `verified[].ptahRelease` | The released Ptah version, or `null` when the verified build is not a release. |
| `verified[].ptahCommit` | The exact Ptah commit the suite built its executor from. |
| `verified[].ptahDescribe` | What `git describe --tags --always` calls that commit in a complete checkout, so a reader sees something other than forty hex characters. A tag name carries no commit to compare, so the verifier accepts one only when it repeats this row's `ptahRelease`. |
| `verified[].runnerProtocolVersion` | The frame version the executor and the operator spoke in that run. Not a third axis — the runner is built from the operator source the row names — and checked against the constant rather than trusted. |
| `verified[].evidence` | A key into `evidence`, which says what ran and how the tested build and this claim stay one declaration. |
| `verified[].scope` | What the run covered. |
| `limitations` | Known constraints on the rows above. |

## One pin, not two

The commit in `verified[].ptahCommit` is the commit the lifecycle suite builds.
`hack/e2e-kind.sh` reads it from this file, and the `kubernetes-e2e` job takes
it from the same place through the support job's output. The release workflow
reads it here too, through `hack/releaseverify`, when it builds the executor it
publishes. Nothing else may write it down:

```sh
make verify-ptah-support
```

The guard refuses a catalog that is malformed or self-contradictory, and it
refuses the lifecycle script, the CI or release workflow, or the executor
recipe carrying the commit a second time. A literal in any of them would keep
working on the day it and the catalog disagreed, and the table on this page
would then name a build nothing ran.

The command performs no network requests.

## Changing the claim

Changing which Ptah the suite verifies is one edit to `verified[].ptahCommit`
plus its `ptahDescribe`, and the matrix jobs then run against that build. Do not
raise `lastVerified` without a run behind it; the field records when the claim
was last measured, not when the file was last touched.

The drift finding categories the pinned build can emit are recorded in
`support/ptah-drift-categories.json`, with the digests of the Ptah files that
produce them. The job that builds the executor compares the record with the
pinned source. A new pin whose drift command, row summary or schema classifier
changed fails there until the record is rewritten:

```sh
go run ./hack/ptahdriftcategories -ptah <Ptah checkout> -write
```

A unit test holds the operator's vocabulary to the recorded list, because the
operator refuses a drift report that names a category it does not know. A
category the new build adds therefore joins the vocabulary, the `PtahSchema`
status enum and the CRD schema version in the same change. A pin that leaves
those files alone needs no edit.

Any change to the file changes the table, so regenerate it in the same commit:

```sh
cd docs/site
npm run compatibility:write
```

Adding an operator release adds a row with its own `verified` list. A release
that has not been run against any Ptah build gets `unverifiedReason` rather than
an empty list, and the table then shows a version nobody has measured instead of
a version that works with everything.

## Correcting a release guide

A release's pages are built from that release's own tag, so a correction cannot
arrive by editing master. Assign the corrected revision to the release instead:

```json
"documentation": {
  "published": true,
  "source": "v0.1.0",
  "fixRevision": "<exact 40-character commit>",
  "fixReason": "the install command named a chart path that never shipped"
}
```

The Git tag does not move and no new binary is released. The revision is a
commit rather than a branch because the published pages must stay the ones
somebody named, and the reason is required because a fix with no stated
correction is indistinguishable from a rebuild.

What a fix may carry is a correction to a command or an explanation. What it may
not carry is a feature from development into an older guide. The offline guard
holds the shape; the publish workflow holds the property it cannot see, that the
commit descends from the release it is assigned to.

Most releases need no entry. This is the explicit exception rather than a
documentation branch per release.

## Where it is published

The table on this page is the published one. The documentation gate renders it
from `support/ptah.json` again and refuses a page that says something else, so
the catalog and the table change in one commit or not at all.

Each guide renders the file at the revision it was built from, so a release
guide shows the catalog as it stood at that release's tag. The development
guide is rebuilt from master whenever the file changes, and
[operator.ptah.run/support/ptah/](https://operator.ptah.run/support/ptah/)
always opens its copy. That is the address to link to from elsewhere.

Database engines are a separate axis and live in
[database support](../databases/); Kubernetes versions live in
[Kubernetes support](../kubernetes/).
