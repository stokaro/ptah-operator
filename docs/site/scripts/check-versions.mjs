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

import { isVersionFolder, order } from './gen-versions.mjs';

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
    if ((index.versions ?? []).join(',') !== expected.join(',')) {
      problems.push(`versions.json lists ${(index.versions ?? []).join(',')} and the root holds ${expected.join(',')}`);
    }
    if (!directories.includes(index.default)) {
      problems.push(`versions.json serves ${index.default}, which is not a published directory`);
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
  writeFileSync(join(root, 'versions.json'), JSON.stringify({ default: 'v0.1.0', versions: ['edge', 'v0.1.0'] }));
  let problems = inspect(root);
  if (problems.length !== 0) throw new Error(`a correct root was refused: ${problems.join('; ')}`);

  // The failure this file exists for: one build published twice.
  write('v0.1.0', { documentation_version: 'v0.1.0', source_commit: commit });
  problems = inspect(root);
  if (!problems.some((problem) => problem.includes('two builds rather than one build'))) {
    throw new Error(`a relabelled build was accepted: ${problems.join('; ')}`);
  }

  // And a directory whose build info belongs to another version.
  write('v0.1.0', { documentation_version: 'edge', source_commit: other });
  problems = inspect(root);
  if (!problems.some((problem) => problem.includes('carries build info for'))) {
    throw new Error(`a mislabelled directory was accepted: ${problems.join('; ')}`);
  }

  rmSync(root, { recursive: true, force: true });
  console.log('check-versions.mjs --selftest: OK (correct root, relabelled build, mislabelled directory)');
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
