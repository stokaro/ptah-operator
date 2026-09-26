#!/usr/bin/env node
// Assembles the site root: the version index, versions.json, and the stub that
// answers the apex.
//
// Which versions exist is not this script's opinion. It reads the published
// directories that the deploy actually assembled, and holds them against the
// compatibility catalog, which is where a version's documentation is
// declared. A directory nobody declared and a declaration nobody built are
// both refused: the first publishes pages no catalog knows about, and the
// second is the missing link the matrix would otherwise render.

import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

import { Origin } from '../src/lib/docs-origin.mjs';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const repositoryRoot = join(scriptDir, '..', '..', '..');

export const EDGE = 'edge';
const VERSION_RE = /^v(\d+)\.(\d+)\.(\d+)$/;

export function isVersionFolder(name) {
  return name === EDGE || VERSION_RE.test(name);
}

export function parseSemver(name) {
  const match = VERSION_RE.exec(name);
  return match ? [Number(match[1]), Number(match[2]), Number(match[3])] : null;
}

// order lists the development state first and then releases, newest first.
// That is the order a picker shows and the order a reader expects.
export function order(names) {
  const releases = names.filter((name) => parseSemver(name) !== null);
  releases.sort((left, right) => {
    const a = parseSemver(left);
    const b = parseSemver(right);
    for (let index = 0; index < 3; index += 1) {
      if (a[index] !== b[index]) return b[index] - a[index];
    }
    return 0;
  });
  return names.includes(EDGE) ? [EDGE, ...releases] : releases;
}

// computeDefault names the version the apex serves.
//
// The newest release, once one exists. Until then the development state, which
// is the only guide there is -- and calling a prerelease state a release is the
// one thing this must not do.
export function computeDefault(names) {
  const ordered = order(names);
  const release = ordered.find((name) => parseSemver(name) !== null);
  return release ?? (names.includes(EDGE) ? EDGE : null);
}

// declaredVersions reads the catalog for the versions whose guide is
// published, so the assembled root and the compatibility claim cannot disagree.
export function declaredVersions(catalog) {
  return (catalog.releases ?? [])
    .filter((entry) => entry.documentation?.published)
    .map((entry) => entry.operator);
}

export function reconcile(built, declared) {
  const problems = [];
  for (const name of built) {
    if (!declared.includes(name)) {
      problems.push(`${name} was built and the catalog does not publish it`);
    }
  }
  for (const name of declared) {
    if (!built.includes(name)) {
      problems.push(`${name} is published by the catalog and was not built`);
    }
  }
  return problems;
}

// The stub every redirect at the site root is made of.
//
// Three ways to the same address, in the order a browser takes them. The script
// runs while the head is still being parsed, so the navigation starts before
// this document has a body to paint -- which is the whole point: a meta refresh
// alone is honored after the document renders, and the reader watches an
// unstyled page for as long as the destination takes to arrive. The refresh is
// the fallback where scripts do not run, and the link is the fallback where
// neither does.
//
// It is styled for the case where it is seen anyway. The site's own stylesheet
// is under a version directory with a hashed name, so these few declarations
// are written out: the page's two backgrounds, its ink, and a sans face. A
// reader who sees this should see the site's colors and not a browser default.
function redirectPage(target, title, body) {
  return `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>${title}</title>
    <link rel="canonical" href="${target}" />
    <meta http-equiv="refresh" content="0; url=${target}" />
    <script>location.replace(${JSON.stringify(target)});</script>
    <style>
      :root {
        color-scheme: light dark;
      }
      body {
        margin: 0;
        min-block-size: 100svh;
        display: grid;
        place-content: center;
        gap: 0.75rem;
        padding: 2rem;
        background: #161311;
        color: #ede7de;
        font: 15px/1.5 'Instrument Sans', system-ui, -apple-system, 'Segoe UI', sans-serif;
        text-align: center;
      }
      @media (prefers-color-scheme: light) {
        body {
          background: #fbfbfa;
          color: #111417;
        }
      }
      a {
        color: inherit;
      }
      ul {
        display: flex;
        flex-wrap: wrap;
        justify-content: center;
        gap: 0 1rem;
        margin: 0;
        padding: 0;
        list-style: none;
        font-size: 13.5px;
        opacity: 0.7;
      }
    </style>
  </head>
  <body>
${body}
  </body>
</html>
`;
}

function indexHTML(defaultVersion, versions) {
  const target = `${Origin}/${defaultVersion}/`;
  const items = versions
    .map((name) => `      <li><a href="/${name}/">${name}</a></li>`)
    .join('\n');
  return redirectPage(
    target,
    'Ptah Operator documentation',
    `    <p>Opening the Ptah Operator documentation: <a href="${target}">${defaultVersion}</a>.</p>
    <ul>
${items}
    </ul>`,
  );
}

// ROOT_ALIASES are addresses other sites link to, kept at the site root so a
// link from elsewhere does not name a version and then rot when a release
// ships. Each one redirects to the same page of one published version. The
// list is short on purpose: a root alias per page would be a second copy of
// the site's routes, free to disagree with the first.
//
// An alias follows whichever version the apex serves unless it names one. The
// Ptah compatibility page names edge: its table is rendered from the catalog
// at the revision a guide was built from, edge is rebuilt from master whenever
// the catalog changes, and a release guide keeps the catalog as it stood at its
// tag. Whoever follows a link to the table is asking what works now.
export const ROOT_ALIASES = [{ route: 'demo' }, { route: 'support/ptah', version: EDGE }];

// aliasVersion is the version an alias redirects into.
export function aliasVersion(alias, defaultVersion) {
  return alias.version ?? defaultVersion;
}

function aliasHTML(version, route) {
  const target = `${Origin}/${version}/${route}/`;
  return redirectPage(
    target,
    'Ptah Operator documentation',
    `    <p>Opening <a href="${target}">${version}/${route}/</a>.</p>`,
  );
}

function selftest() {
  const ordered = order(['v0.2.0', EDGE, 'v0.10.0', 'v0.1.0']);
  if (ordered.join(',') !== 'edge,v0.10.0,v0.2.0,v0.1.0') {
    throw new Error(`ordered as ${ordered.join(',')}`);
  }
  if (computeDefault([EDGE]) !== EDGE) throw new Error('the development state is not the default when alone');
  if (computeDefault([EDGE, 'v0.1.0']) !== 'v0.1.0') throw new Error('a release did not become the default');
  const problems = reconcile([EDGE, 'v0.1.0'], [EDGE]);
  if (problems.length !== 1 || !problems[0].includes('does not publish it')) {
    throw new Error(`reconcile said ${JSON.stringify(problems)}`);
  }
  const missing = reconcile([EDGE], [EDGE, 'v0.1.0']);
  if (missing.length !== 1 || !missing[0].includes('was not built')) {
    throw new Error(`reconcile said ${JSON.stringify(missing)}`);
  }
  if (isVersionFolder('v1.2') || isVersionFolder('nightly')) throw new Error('a non-version folder was accepted');
  const demo = ROOT_ALIASES.find((entry) => entry.route === 'demo');
  const compatibility = ROOT_ALIASES.find((entry) => entry.route === 'support/ptah');
  if (aliasVersion(demo, 'v0.1.0') !== 'v0.1.0') throw new Error('the demo alias does not follow the apex');
  // Once a release is the default, the table a link from elsewhere opens is
  // still the current one.
  if (aliasVersion(compatibility, 'v0.1.0') !== EDGE) {
    throw new Error('the compatibility alias follows the apex instead of the current catalog');
  }
  const alias = aliasHTML('v0.1.0', 'demo');
  if (!alias.includes('/v0.1.0/demo/')) throw new Error('a root alias did not address the default version');
  // Every redirect at the root leaves before it is drawn. Without the script a
  // reader watches this page until the destination arrives, which is what the
  // apex used to do; the refresh and the link are what is left when scripts do
  // not run, so all three are asserted rather than only the newest.
  const index = indexHTML(EDGE, [EDGE, 'v0.1.0']);
  for (const [name, page] of [['the apex', index], ['a root alias', alias]]) {
    if (!page.includes('<script>location.replace(')) {
      throw new Error(`${name} redirect does not leave before it is drawn`);
    }
    if (!page.includes('http-equiv="refresh"')) throw new Error(`${name} redirect has no refresh fallback`);
    if (!page.includes('<a href=')) throw new Error(`${name} redirect has no link to follow by hand`);
    if (!page.includes('background: #161311')) throw new Error(`${name} redirect is unstyled`);
  }
  console.log(
    'gen-versions.mjs --selftest: OK (order, default, both reconcile directions, folder shape, ' +
      'root aliases, and a redirect that leaves before it is drawn)',
  );
}

function main() {
  const arguments_ = process.argv.slice(2);
  if (arguments_.includes('--selftest')) {
    selftest();
    return;
  }
  const root = arguments_[0];
  if (!root || !existsSync(root)) {
    console.error('usage: gen-versions.mjs <assembled-site-root>');
    process.exit(2);
  }
  const built = readdirSync(root, { withFileTypes: true })
    .filter((entry) => entry.isDirectory() && isVersionFolder(entry.name))
    .map((entry) => entry.name);

  const catalog = JSON.parse(readFileSync(join(repositoryRoot, 'support', 'ptah.json'), 'utf8'));
  const declared = declaredVersions(catalog);
  const problems = reconcile(built, declared);
  if (problems.length > 0) {
    console.error(`gen-versions.mjs: the assembled root and support/ptah.json disagree:\n- ${problems.join('\n- ')}`);
    process.exit(1);
  }

  const versions = order(built);
  const defaultVersion = computeDefault(built);
  if (!defaultVersion) {
    console.error('gen-versions.mjs: no version directory was assembled');
    process.exit(1);
  }
  // {slug, label} rather than bare strings: the version pill in
  // SiteTitle.astro reads this file at runtime and builds its options from
  // those two fields, and Ptah's own version index has the same shape, so one
  // reader can be written against both.
  const index = {
    default: defaultVersion,
    versions: versions.map((name) => ({ slug: name, label: name })),
  };
  writeFileSync(join(root, 'versions.json'), `${JSON.stringify(index, null, 2)}\n`);
  writeFileSync(join(root, 'index.html'), indexHTML(defaultVersion, versions));
  for (const alias of ROOT_ALIASES) {
    const { route } = alias;
    const version = aliasVersion(alias, defaultVersion);
    const published = join(root, version, route, 'index.html');
    if (!existsSync(published)) {
      console.error(`gen-versions.mjs: ${version} publishes no /${route}/, so the root alias would 404`);
      process.exit(1);
    }
    mkdirSync(join(root, route), { recursive: true });
    writeFileSync(join(root, route, 'index.html'), aliasHTML(version, route));
  }
  console.log(
    `gen-versions.mjs: ${versions.length} version(s), apex serves ${defaultVersion}, ` +
      `${ROOT_ALIASES.length} root alias(es)`,
  );
}

// Only when this file is the program. check-versions.mjs imports the ordering
// and the folder shape from here, and an unguarded call would run the
// assembler -- with that script's arguments -- as a side effect of the import.
if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  main();
}
