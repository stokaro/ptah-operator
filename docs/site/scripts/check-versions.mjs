#!/usr/bin/env node
// Reads back the assembled Pages root.
//
// This is the check behind the promise a version switcher makes. Two published
// versions must be two builds: each directory carries its own build-info.json,
// naming itself and the revision it came from, and two directories may not name
// the same source commit. A switcher that only changed a label would pass every
// other gate in this repository and fail here.

import { existsSync, readFileSync, readdirSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { aliasVersion, isVersionFolder, latestRelease, order, parseSemver, publicDir, ROOT_ALIASES, ROOT_ASSETS } from './gen-versions.mjs';

const scriptDir = dirname(fileURLToPath(import.meta.url));

// inspect returns one problem per way the assembled root is not what it claims.
export function inspect(root) {
  const problems = [];
  const directories = readdirSync(root, { withFileTypes: true })
    .filter((entry) => entry.isDirectory() && isVersionFolder(entry.name))
    .map((entry) => entry.name);

  if (directories.length === 0) {
    return ['the assembled root holds no version directory'];
  }

  const indexPath = join(root, 'versions.json');
  if (!existsSync(indexPath)) {
    problems.push('versions.json is missing, so nothing tells a reader which versions exist');
  } else {
    const index = JSON.parse(readFileSync(indexPath, 'utf8'));
    const expected = order(directories);
    const listed = (index.versions ?? []).map((entry) => entry?.slug);
    if (listed.join(',') !== expected.join(',')) {
      problems.push(`versions.json lists ${listed.join(',')} and the root holds ${expected.join(',')}`);
    }
    for (const entry of index.versions ?? []) {
      if (!entry?.slug || !entry?.label) {
        problems.push(`versions.json carries an entry with no slug or label; the version picker reads both`);
      }
      if (entry?.slug && parseSemver(entry.slug) !== null && !/^\d{4}-\d{2}-\d{2}$/.test(entry.released ?? '')) {
        problems.push(`versions.json lists ${entry.slug} with no release date; the version picker shows one per release`);
      }
    }
    // The picker badges this release, and a page from an older one links to
    // it, so naming anything but the newest sends readers backwards.
    const newest = latestRelease(directories);
    if ((index.latest ?? undefined) !== newest) {
      problems.push(`versions.json names ${index.latest} as latest, want ${newest}`);
    }
    if (!directories.includes(index.default)) {
      problems.push(`versions.json serves ${index.default}, which is not a published directory`);
    }
  }

  // Every version loads the picker from the root, so a root without it leaves
  // every header with the plain version text, and a stale copy runs an older
  // picker on every page at once.
  for (const name of ROOT_ASSETS) {
    const published = join(root, name);
    if (!existsSync(published)) {
      problems.push(`/${name} is missing, and every version loads the version picker from the root`);
    } else if (!readFileSync(published).equals(readFileSync(join(publicDir, name)))) {
      problems.push(`/${name} differs from public/${name}, so the root serves another picker than this revision`);
    }
  }

  const commits = new Map();
  for (const name of directories) {
    const infoPath = join(root, name, 'build-info.json');
    if (!existsSync(infoPath)) {
      problems.push(`${name} carries no build-info.json, so nothing says what it was built from`);
      continue;
    }
    const info = JSON.parse(readFileSync(infoPath, 'utf8'));
    if (info.documentation_version !== name) {
      problems.push(`${name} carries build info for ${info.documentation_version}`);
    }
    if (!/^[0-9a-f]{40}$/.test(info.source_commit ?? '')) {
      problems.push(`${name} names source commit ${info.source_commit}, which is not an exact commit`);
      continue;
    }
    const seen = commits.get(info.source_commit);
    if (seen) {
      problems.push(
        `${name} and ${seen} were built from the same commit ${info.source_commit}; ` +
          'two published versions have to be two builds rather than one build under two labels',
      );
    }
    commits.set(info.source_commit, name);
  }
  // A root alias is what another site links to. An alias with nothing behind it
  // is a 404 on someone else's page, which is invisible from here.
  const defaultVersion = existsSync(indexPath)
    ? JSON.parse(readFileSync(indexPath, 'utf8')).default
    : null;
  for (const alias of ROOT_ALIASES) {
    const { route } = alias;
    if (!existsSync(join(root, route, 'index.html'))) {
      problems.push(`/${route}/ is linked from elsewhere and the assembled root does not answer it`);
      continue;
    }
    const version = aliasVersion(alias, defaultVersion);
    if (version && !existsSync(join(root, version, route, 'index.html'))) {
      problems.push(`/${route}/ redirects into ${version}, which publishes no such page`);
    }
  }

  return problems;
}

function selftest() {
  const root = join(scriptDir, '..', '.selftest-versions');
  rmSync(root, { recursive: true, force: true });
  const write = (version, info) => {
    mkdirSync(join(root, version), { recursive: true });
    writeFileSync(join(root, version, 'build-info.json'), JSON.stringify(info));
  };
  const commit = '0123456789abcdef0123456789abcdef01234567';
  const other = 'fedcba9876543210fedcba9876543210fedcba98';

  write('edge', { documentation_version: 'edge', source_commit: commit });
  write('v0.1.0', { documentation_version: 'v0.1.0', source_commit: other });
  const index = {
    default: 'v0.1.0',
    latest: 'v0.1.0',
    versions: [
      { slug: 'edge', label: 'edge' },
      { slug: 'v0.1.0', label: 'v0.1.0', released: '2030-01-01' },
    ],
  };
  writeFileSync(join(root, 'versions.json'), JSON.stringify(index));
  for (const name of ROOT_ASSETS) writeFileSync(join(root, name), readFileSync(join(publicDir, name)));
  for (const alias of ROOT_ALIASES) {
    const { route } = alias;
    const version = aliasVersion(alias, 'v0.1.0');
    mkdirSync(join(root, route), { recursive: true });
    writeFileSync(join(root, route, 'index.html'), '<!doctype html>');
    mkdirSync(join(root, version, route), { recursive: true });
    writeFileSync(join(root, version, route, 'index.html'), '<!doctype html>');
  }
  let problems = inspect(root);
  if (problems.length !== 0) throw new Error(`a correct root was refused: ${problems.join('; ')}`);

  // The picker's files and the index fields it reads.
  const refused = (label, mutate, restore, fragment) => {
    mutate();
    const found = inspect(root);
    restore();
    if (!found.some((problem) => problem.includes(fragment))) {
      throw new Error(`${label} was accepted: ${found.join('; ')}`);
    }
  };
  const asset = join(root, ROOT_ASSETS[0]);
  const bytes = readFileSync(asset);
  refused('a root without the picker', () => rmSync(asset), () => writeFileSync(asset, bytes), 'is missing, and every version');
  refused('a stale root picker', () => writeFileSync(asset, '/* old */'), () => writeFileSync(asset, bytes), 'differs from public/');
  const writeIndex = (value) => writeFileSync(join(root, 'versions.json'), JSON.stringify(value));
  refused('an undated release', () => writeIndex({ ...index, versions: [index.versions[0], { slug: 'v0.1.0', label: 'v0.1.0' }] }), () => writeIndex(index), 'with no release date');
  refused('no latest release', () => writeIndex({ ...index, latest: undefined }), () => writeIndex(index), 'as latest, want v0.1.0');

  // An alias nothing is behind is the failure another site would carry.
  for (const { route } of ROOT_ALIASES) rmSync(join(root, route), { recursive: true, force: true });
  problems = inspect(root);
  if (!problems.some((problem) => problem.includes('does not answer it'))) {
    throw new Error(`a missing root alias was accepted: ${problems.join('; ')}`);
  }
  for (const { route } of ROOT_ALIASES) {
    mkdirSync(join(root, route), { recursive: true });
    writeFileSync(join(root, route, 'index.html'), '<!doctype html>');
  }

  // An alias that names its own version is read against that version, not
  // against the apex: the page missing from edge is refused even though the
  // default release publishes one.
  const pinned = ROOT_ALIASES.find((alias) => alias.version);
  if (!pinned) throw new Error('no root alias names its own version, so the case below measures nothing');
  mkdirSync(join(root, 'v0.1.0', pinned.route), { recursive: true });
  writeFileSync(join(root, 'v0.1.0', pinned.route, 'index.html'), '<!doctype html>');
  rmSync(join(root, pinned.version, pinned.route), { recursive: true, force: true });
  problems = inspect(root);
  if (!problems.some((problem) => problem.includes(`/${pinned.route}/ redirects into ${pinned.version}`))) {
    throw new Error(`an alias into a version that lacks the page was accepted: ${problems.join('; ')}`);
  }
  mkdirSync(join(root, pinned.version, pinned.route), { recursive: true });
  writeFileSync(join(root, pinned.version, pinned.route, 'index.html'), '<!doctype html>');

  // The failure this file exists for: one build published twice.
  write('v0.1.0', { documentation_version: 'v0.1.0', source_commit: commit });
  problems = inspect(root);
  if (!problems.some((problem) => problem.includes('two builds rather than one build'))) {
    throw new Error(`a relabeled build was accepted: ${problems.join('; ')}`);
  }

  // And a directory whose build info belongs to another version.
  write('v0.1.0', { documentation_version: 'edge', source_commit: other });
  problems = inspect(root);
  if (!problems.some((problem) => problem.includes('carries build info for'))) {
    throw new Error(`a mislabeled directory was accepted: ${problems.join('; ')}`);
  }

  rmSync(root, { recursive: true, force: true });
  console.log(
    'check-versions.mjs --selftest: OK (correct root, missing and stale root picker, undated release, ' +
      'latest release, missing alias, alias into its own version, relabeled build, mislabeled directory)',
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
    console.error('usage: check-versions.mjs <assembled-site-root>');
    process.exit(2);
  }
  const problems = inspect(root);
  if (problems.length > 0) {
    console.error(`check-versions.mjs: ${problems.length} problem(s):\n- ${problems.join('\n- ')}`);
    process.exit(1);
  }
  console.log('check-versions.mjs: OK (every version directory is its own build)');
}

main();
