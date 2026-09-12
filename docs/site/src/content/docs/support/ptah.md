---
title: Ptah compatibility
description: Which Ptah build this operator runs, what has been verified, and what nobody has measured.
---

The operator does not contain Ptah. It runs one, as a job whose container image
and version identity the installation binds, so "which Ptah works with this
operator" is a question about that executor and not about the CLI a person used
to build the OCI artifact.

`support/ptah.json` is the machine-readable answer, and
it is canonical: the lifecycle suite takes the commit it builds the executor
from out of this file, and the compatibility table published on the Ptah
documentation site is generated from a copy of it.

## Three states, kept apart

A compatibility table usually blurs three different claims. This one keeps them
in separate fields, because the difference is what a reader is actually asking
about.

**Declared** is a promise. `declared.range` names the Ptah versions an operator
version is meant to work with. It is `null` today and `declared.statement` says
why: the chart ships no executor default, an installation supplies both a
digest-pinned `executorImage` and the `ptahVersion` verified from that image's
provenance, and the supported build is exactly the one the matrix exercised.

**Verified** is a measurement. Each `verified` entry names a Ptah build the
operator was actually run against, the evidence behind it, and what ran.
`ptahRelease` is `null` when the build is not a released Ptah, which is the case
today: the suite builds from a development commit.

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
| `documentation.published` | Whether a guide exists for this operator version. The published matrix renders a link only when it does. |
| `documentation.source` | The revision that guide is built from: `master` for the development state, and its own tag for a release. |
| `documentation.fixRevision` | An exact commit that replaces the tag as the build revision for one release. |
| `documentation.fixReason` | What that fix corrects. Required beside a fix revision. |
| `declared.range` | The promised Ptah versions, or `null`. |
| `declared.statement` | Why there is no range, when there is none. |
| `verified[].ptahRelease` | The released Ptah version, or `null` when the verified build is not a release. |
| `verified[].ptahCommit` | The exact Ptah commit the suite built its executor from. |
| `verified[].ptahDescribe` | What `git describe --tags --always` calls that commit in a complete checkout, so a reader sees something other than forty hex characters. |
| `verified[].evidence` | A key into `evidence`, which says what ran and how the tested build and this claim stay one declaration. |
| `verified[].scope` | What the run covered. |
| `limitations` | Known constraints on the rows above. |

## One pin, not two

The commit in `verified[].ptahCommit` is the commit the lifecycle suite builds.
`hack/e2e-kind.sh` reads it from this file, and the `kubernetes-e2e` job takes
it from the same place through the support job's output. Nothing else may write
it down:

```sh
make verify-ptah-support
```

The guard refuses a catalog that is malformed or self-contradictory, and it
refuses the lifecycle script or the CI workflow carrying the commit a second
time. A literal in either place would keep working on the day the two
disagreed, and the published matrix would then name a build nothing ran.

The command performs no network requests.

## Changing the claim

Changing which Ptah the suite verifies is one edit to `verified[].ptahCommit`
plus its `ptahDescribe`, and the matrix jobs then run against that build. Do not
raise `lastVerified` without a run behind it; the field records when the claim
was last measured, not when the file was last touched.

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

Database engines are a separate axis and live in
[database support](databases.md); Kubernetes versions live in
[Kubernetes support](kubernetes.md).
