#!/usr/bin/env node
// Refuses a built directory that does not say what it is.
//
// It reads dist/ after the build rather than the arguments the build was given:
// the file a deploy publishes is the only thing a reader and a later gate can
// consult, and a build that produced no such file looks identical to one that
// did until somebody opens the page.

import { existsSync, readFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDir = dirname(fileURLToPath(import.meta.url));

export function problems(info, { version, sourceRef, commit }) {
  const found = [];
  if (info.documentation_version !== version) {
    found.push(`build-info.json names version ${info.documentation_version}, and this build produced ${version}`);
  }
  if (sourceRef && info.source_ref !== sourceRef) {
    found.push(`build-info.json names source ref ${info.source_ref}, and this build read ${sourceRef}`);
  }
  if (commit && info.source_commit !== commit) {
    found.push(`build-info.json names commit ${info.source_commit}, and this build read ${commit}`);
  }
  if (!/^[0-9a-f]{40}$/.test(info.source_commit ?? '')) {
    found.push(`build-info.json names source commit ${info.source_commit}, which is not an exact commit`);
  }
  if (Number.isNaN(Date.parse(info.built_at ?? ''))) {
    found.push(`build-info.json names build time ${info.built_at}, which is not a timestamp`);
  }
  return found;
}

function selftest() {
  const commit = '0123456789abcdef0123456789abcdef01234567';
  const good = {
    documentation_version: 'edge',
    source_ref: 'master',
    source_commit: commit,
    built_at: '2026-09-12T08:00:00Z',
  };
  if (problems(good, { version: 'edge', sourceRef: 'master', commit }).length !== 0) {
    throw new Error('a correct build info was refused');
  }
  if (problems(good, { version: 'v0.1.0' }).length === 0) {
    throw new Error('build info for another version was accepted');
  }
  if (problems({ ...good, source_commit: 'master' }, { version: 'edge' }).length === 0) {
    throw new Error('a branch name was accepted as a commit');
  }
  console.log('check-build-info.mjs --selftest: OK (match, wrong version, non-commit)');
}

function main() {
  if (process.argv.includes('--selftest')) {
    selftest();
    return;
  }
  const path = resolve(scriptDir, '..', 'dist', 'build-info.json');
  if (!existsSync(path)) {
    console.error('check-build-info.mjs: dist/build-info.json is missing; the publish step writes it after the build');
    process.exit(1);
  }
  const found = problems(JSON.parse(readFileSync(path, 'utf8')), {
    version: process.env.DOCS_VERSION || 'edge',
    sourceRef: process.env.DOCS_SOURCE_REF,
    commit: process.env.DOCS_SOURCE_COMMIT,
  });
  if (found.length > 0) {
    console.error(`check-build-info.mjs: ${found.length} problem(s):\n- ${found.join('\n- ')}`);
    process.exit(1);
  }
  console.log('check-build-info.mjs: OK (the build says which version and revision it is)');
}

main();
