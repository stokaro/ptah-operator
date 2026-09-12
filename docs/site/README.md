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

## Versions

One directory per operator version, each built from that version's own
revision. Which versions exist is declared in `support/ptah.json`, beside the
compatibility claim, so the published set and the matrix cannot disagree:
`gen-versions.mjs` refuses a directory the catalogue does not publish and a
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
