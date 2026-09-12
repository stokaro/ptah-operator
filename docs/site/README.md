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

The repository part is complete. What remains is account configuration that no
workflow can perform:

1. In the repository settings, set **Pages → Build and deployment → Source** to
   **GitHub Actions**. The publish workflow supplies the artifact; no branch is
   served.
2. Set **Pages → Custom domain** to `operator.ptah.run` and leave
   **Enforce HTTPS** enabled once the certificate is issued.
3. In the DNS zone for `ptah.run`, add:

   ```dns
   operator   CNAME   stokaro.github.io.
   ```

   A CNAME is correct here because `operator` is a subdomain. Do not change the
   records that serve `docs.ptah.run`.
4. Wait for the certificate. GitHub issues it after the CNAME resolves; until
   then the site answers over HTTP only.

The custom domain is configured in the repository settings rather than by a
`CNAME` file in the artifact, because this deploy assembles `_site` from
scratch on every run and a file committed to the tree would have to be copied
into it by hand each time.
