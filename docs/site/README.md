# The operator documentation site

The guide published at [operator.ptah.run](https://operator.ptah.run/). It
builds from this repository alone: no checkout of Ptah, and nothing fetched
from Ptah's site.

```sh
npm ci
npm run build          # the development guide, at /edge/
npm run check:links
npm run check:navigation
npm run check:values
```

`npm run values:write` regenerates the chart values table on the configuration
page from `charts/ptah-operator/values.yaml`, which is where those values are
declared.

## The recorded runs

`/demo/` and a page per run are built from `demo/recordings/runs.json`, which
[`demo/`](../../demo/README.md) writes by running each scenario against a live
cluster. The site renders every run as a transcript in the markup and ships
`public/demo/player.js`, which replays the same events; both read
`public/demo/transcript.mjs`, so the printed session and the played one cannot
differ.

```sh
npm run check:demo         # the recording matches the scenarios, and every check held
npm run check:demo-page    # both pages in a browser, with and without the player
```

The browser check needs Playwright's chromium. Without it the check says what to
install and skips; under `CI=1` it fails instead.

## Versions

One directory per operator version, each built from that version's own
revision. Which versions exist is declared in `support/ptah.json`, beside the
compatibility claim, so the published set and the matrix cannot disagree:
`gen-versions.mjs` refuses a directory the catalog does not publish and a
declaration nothing built.

The development guide is `edge`, built from `master`. A release is built from
its own tag, or from the exact commit assigned to it as a documentation fix —
see `docs/site/src/content/docs/support/ptah.md` for that mechanism. The apex
serves the newest release once one exists, and `edge` until then, because there
is no release to call stable yet.

`check-versions.mjs` reads the assembled root back: every directory carries its
own `build-info.json` naming itself and the commit it came from, and no two
directories may name the same commit. A switcher that only changed a label
fails there.

## Publishing

`.github/workflows/docs-publish.yml` assembles every published version on every
run and uploads the whole tree, so a deploy cannot drop a version by forgetting
it. Deploys queue rather than cancel, and a run whose tree master has moved past
publishes nothing and leaves the newer run to it.

## GitHub Pages and DNS

Configured, and verified on 2026-09-12 against the live zone and the repository
settings:

- `operator.ptah.run` is a CNAME to `stokaro.github.io`, resolving to GitHub's
  Pages addresses.
- Pages serves this repository with **Build and deployment → Source: GitHub
  Actions**, which is what the publish workflow supplies an artifact to. No
  branch is served.
- The custom domain is verified and its certificate is issued.

The custom domain lives in the repository settings rather than in a `CNAME`
file in the artifact, because this deploy assembles `_site` from scratch on
every run and a committed file would have to be copied into it each time.

One setting is still open: **Enforce HTTPS** is off, so the site answers over
HTTP as well as HTTPS. The certificate is approved, so turning it on is a
single toggle in the same settings page.

Nothing here touches the settings that serve `docs.ptah.run`.

## Announcing the catalog

`.github/workflows/notify-compatibility.yml` tells `stokaro/ptah` that the
compatibility catalog moved, so the matrix it publishes refreshes promptly. It
sends a repository and a commit and nothing else.

It needs one secret in this repository, **`PTAH_COMPATIBILITY_DISPATCH_TOKEN`**:
a fine-grained token scoped to `stokaro/ptah` alone with **Contents: read and
write**, which is the permission a `repository_dispatch` requires. Without it
the step says so and exits zero: the receiving repository reconciles the two
files on a schedule, so a missing notification makes the refresh late rather
than lost.
