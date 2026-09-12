#!/usr/bin/env node
// Writes what a published directory was built from.
//
// A version switcher that only changes a label is the failure this file exists
// against: build-info.json records the operator version, the revision the pages
// came from, and the exact commit that revision resolved to, so a reader and a
// gate can both tell one published version from another.

import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';

const fullCommit = /^[0-9a-f]{40}$/;

export function buildInfo({ version, sourceRef, commit, builtAt = new Date().toISOString() }) {
  if (typeof version !== 'string' || version.trim() === '') {
    throw new Error('version must be a non-empty string');
  }
  if (typeof sourceRef !== 'string' || sourceRef.trim() === '') {
    throw new Error('source ref must be a non-empty string');
  }
  if (typeof commit !== 'string' || !fullCommit.test(commit)) {
    throw new Error('source commit must be a full lowercase Git SHA');
  }
  if (Number.isNaN(Date.parse(builtAt))) {
    throw new Error('built-at must be an ISO-8601 timestamp');
  }
  return {
    documentation_version: version,
    source_ref: sourceRef,
    source_commit: commit,
    built_at: builtAt,
  };
}

function value(arguments_, name) {
  const index = arguments_.indexOf(name);
  return index === -1 ? undefined : arguments_[index + 1];
}

function selftest() {
  const commit = '0123456789abcdef0123456789abcdef01234567';
  const edge = buildInfo({ version: 'edge', sourceRef: 'master', commit, builtAt: '2026-09-12T08:00:00Z' });
  if (edge.source_ref !== 'master') throw new Error('the development ref is not master');
  const release = buildInfo({ version: 'v0.1.0', sourceRef: 'v0.1.0', commit, builtAt: '2026-09-12T08:00:00Z' });
  if (release.source_ref !== 'v0.1.0') throw new Error('the release ref is not its tag');
  // A documentation fix keeps the version and names its own revision, which is
  // the whole point of recording the two separately.
  const fixed = buildInfo({ version: 'v0.1.0', sourceRef: commit, commit, builtAt: '2026-09-12T08:00:00Z' });
  if (fixed.documentation_version !== 'v0.1.0' || fixed.source_ref !== commit) {
    throw new Error('a fixed release lost either its version or its revision');
  }
  for (const broken of [
    { version: '', sourceRef: 'master', commit },
    { version: 'edge', sourceRef: '', commit },
    { version: 'edge', sourceRef: 'master', commit: 'abc' },
  ]) {
    let threw = false;
    try {
      buildInfo(broken);
    } catch {
      threw = true;
    }
    if (!threw) throw new Error(`accepted ${JSON.stringify(broken)}`);
  }
  console.log('write-build-info.mjs --selftest: OK (development, release, fix, three refusals)');
}

function main() {
  const arguments_ = process.argv.slice(2);
  if (arguments_.includes('--selftest')) {
    selftest();
    return;
  }
  const output = value(arguments_, '--output');
  const version = value(arguments_, '--version');
  const sourceRef = value(arguments_, '--source-ref');
  const commit = value(arguments_, '--source-commit');
  const builtAt = value(arguments_, '--built-at');
  if (!output) {
    console.error('usage: write-build-info.mjs --output <path> --version <v> --source-ref <ref> --source-commit <sha> [--built-at <iso>]');
    process.exit(2);
  }
  const info = buildInfo({ version, sourceRef, commit, builtAt });
  mkdirSync(dirname(output), { recursive: true });
  writeFileSync(output, `${JSON.stringify(info, null, 2)}\n`);
  console.log(`write-build-info.mjs: wrote ${output} for ${version} at ${info.source_commit}`);
}

main();
