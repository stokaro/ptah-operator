#!/usr/bin/env node
// Assembles the site root: the version index, versions.json, and the stub that
// answers the apex.
//
// Which versions exist is not this script's opinion. It reads the published
// directories that the deploy actually assembled, and holds them against the
// compatibility catalogue, which is where a version's documentation is
// declared. A directory nobody declared and a declaration nobody built are
// both refused: the first publishes pages no catalogue knows about, and the
// second is the missing link the matrix would otherwise render.

import { existsSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
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

// declaredVersions reads the catalogue for the versions whose guide is
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
      problems.push(`${name} was built and the catalogue does not publish it`);
    }
  }
  for (const name of declared) {
    if (!built.includes(name)) {
      problems.push(`${name} is published by the catalogue and was not built`);
    }
  }
  return problems;
}

function indexHTML(defaultVersion, versions) {
  const target = `${Origin}/${defaultVersion}/`;
  const items = versions
    .map((name) => `      <li><a href="/${name}/">${name}</a></li>`)
    .join('\n');
  return `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>Ptah Operator documentation</title>
    <link rel="canonical" href="${target}" />
    <meta http-equiv="refresh" content="0; url=${target}" />
  </head>
  <body>
    <h1>Ptah Operator documentation</h1>
    <p>Redirecting to <a href="${target}">${defaultVersion}</a>.</p>
    <ul>
${items}
    </ul>
  </body>
</html>
`;
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
  console.log('gen-versions.mjs --selftest: OK (order, default, both reconcile directions, folder shape)');
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
  writeFileSync(join(root, 'versions.json'), `${JSON.stringify({ default: defaultVersion, versions }, null, 2)}\n`);
  writeFileSync(join(root, 'index.html'), indexHTML(defaultVersion, versions));
  console.log(`gen-versions.mjs: ${versions.length} version(s), apex serves ${defaultVersion}`);
}

// Only when this file is the program. check-versions.mjs imports the ordering
// and the folder shape from here, and an unguarded call would run the
// assembler -- with that script's arguments -- as a side effect of the import.
if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  main();
}
