#!/usr/bin/env node
// Holds the recording, the scenarios and the page against each other.
//
// The page is built from demo/recordings/runs.json, and the recording is
// written by demo/cmd/record against a live cluster. What can go wrong between
// them is invisible in a build log: a recording that no longer matches the
// scenarios it claims to be of, a run with no checks, a transcript carrying a
// credential, or a page that says Live about a replay.
//
//   node scripts/check-demo.mjs
//   node scripts/check-demo.mjs --selftest

import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const repositoryRoot = resolve(scriptDir, '..', '..', '..');

// Every run states what it is of, and has checks behind it. A scenario with no
// check is a session nobody verified, which is the one thing a recording may
// not be.
export function problemsIn(record, scenarioIds) {
  const problems = [];
  const recorded = record.scenarios ?? [];

  if (recorded.length === 0) problems.push('the recording holds no run');
  if (!record.recordedAt) problems.push('the recording does not say when it was made');
  for (const key of ['KUBERNETES_VERSION', 'CONTROLLER_REVISION', 'PTAH_VERSION', 'EXECUTOR_IMAGE']) {
    if (!record.lab?.[key]) problems.push(`the recording does not say which ${key} it ran against`);
  }
  // Per run, because one may be re-recorded without the others. The
  // record-level commit is present only when they all agree, so its absence is
  // a fact about the recording rather than a gap in it.

  const seen = new Set();
  for (const run of recorded) {
    seen.add(run.id);
    if (!scenarioIds.includes(run.id)) {
      problems.push(`${run.id} is recorded and demo/scenarios/${run.id}.yaml is not there`);
    }
    if ((run.checks ?? []).length === 0) {
      problems.push(`${run.id} carries no check, so nothing about it was verified`);
    }
    for (const check of run.checks ?? []) {
      if (!check.passed) problems.push(`${run.id} step ${check.step} is published with a failed check`);
    }
    if (!run.source?.commit) problems.push(`${run.id} does not say which commit produced it`);
    if ((run.events ?? []).length === 0) problems.push(`${run.id} carries no event`);
    if (!(run.events ?? []).some(([kind]) => kind === 'cmd')) {
      problems.push(`${run.id} runs no command`);
    }
    for (const [kind, text] of run.events ?? []) {
      if (typeof kind !== 'string') problems.push(`${run.id} carries an event with no kind`);
      if (kind !== 'blank' && typeof text !== 'string') {
        problems.push(`${run.id} carries a ${kind} event with no text`);
      }
    }
  }
  for (const id of scenarioIds) {
    if (!seen.has(id)) problems.push(`demo/scenarios/${id}.yaml has no recorded run`);
  }
  for (const id of record.order ?? []) {
    if (!seen.has(id)) problems.push(`the reading order names ${id}, which is not recorded`);
  }
  if ((record.order ?? []).length !== recorded.length) {
    problems.push('the reading order and the recorded runs are different lengths');
  }
  return problems;
}

// A replay is never announced as live. The word is the one thing a reader would
// take at face value, and taking it at face value would be wrong.
//
// Nor does a page type the number of runs. The recording is where that is
// known, and a page that repeats it keeps working on the day it stops being
// true -- which is the only day it matters.
const SPELLED = /\b(?:one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)\s+(?:runs|sessions|scenarios|recordings)\b/i;

export function livenessProblemsIn(sources) {
  const problems = [];
  for (const [name, text] of sources) {
    if (/\bLive\b/.test(text)) problems.push(`${name} calls a replay live`);
    const spelled = SPELLED.exec(text);
    if (spelled) problems.push(`${name} types the count of runs ("${spelled[0]}") instead of reading it`);
    const digits = /\b\d+\s+(?:runs|sessions|scenarios|recordings)\b/i.exec(text);
    if (digits) problems.push(`${name} types the count of runs ("${digits[0]}") instead of reading it`);
  }
  return problems;
}

function selftest() {
  const good = {
    recordedAt: '2026-09-12T00:00:00Z',
    lab: {
      KUBERNETES_VERSION: '1.37.0',
      CONTROLLER_REVISION: 'abc',
      PTAH_VERSION: 'v0',
      EXECUTOR_IMAGE: 'registry/x@sha256:ab',
    },
    order: ['one'],
    scenarios: [
      {
        id: 'one',
        source: { commit: 'abc' },
        events: [['cmd', 'kubectl get ptahschema']],
        checks: [{ step: 1, passed: true }],
      },
    ],
  };
  const clean = problemsIn(good, ['one']);
  if (clean.length !== 0) throw new Error(`a sound recording reported ${clean.join('; ')}`);

  const cases = [
    [{ ...good, scenarios: [{ ...good.scenarios[0], checks: [] }] }, 'carries no check'],
    [
      { ...good, scenarios: [{ ...good.scenarios[0], checks: [{ step: 1, passed: false }] }] },
      'published with a failed check',
    ],
    [{ ...good, scenarios: [{ ...good.scenarios[0], events: [['out', 'x']] }] }, 'runs no command'],
    [{ ...good, scenarios: [{ ...good.scenarios[0], source: {} }] }, 'which commit produced it'],
  ];
  for (const [record, want] of cases) {
    const found = problemsIn(record, ['one']).join('; ');
    if (!found.includes(want)) throw new Error(`expected ${want}, found ${found || 'nothing'}`);
  }
  const missing = problemsIn(good, ['one', 'two']).join('; ');
  if (!missing.includes('two.yaml has no recorded run')) throw new Error(`expected a missing run, found ${missing}`);

  const live = livenessProblemsIn([['page', 'Live cluster']]);
  if (live.length !== 1) throw new Error('a page calling a replay live was not reported');
  const counted = livenessProblemsIn([['page', 'Nine sessions, recorded'], ['page', '9 runs of the operator']]);
  if (counted.length !== 2) throw new Error(`a typed count was not reported: ${JSON.stringify(counted)}`);
  const derived = livenessProblemsIn([['page', '{Runs.length} sessions, recorded at the terminal']]);
  if (derived.length !== 0) throw new Error(`a derived count was reported: ${JSON.stringify(derived)}`);

  console.log(
    'check-demo.mjs --selftest: OK (checks, failures, coverage, order, the word Live, and a typed count)',
  );
}

function main() {
  if (process.argv.includes('--selftest')) {
    selftest();
    return;
  }
  const recordPath = join(repositoryRoot, 'demo', 'recordings', 'runs.json');
  if (!existsSync(recordPath)) {
    console.error(`check-demo.mjs: ${recordPath} is missing; record the runs first (make demo)`);
    process.exit(1);
  }
  const record = JSON.parse(readFileSync(recordPath, 'utf8'));
  const scenarioDir = join(repositoryRoot, 'demo', 'scenarios');
  const scenarioIds = readdirSync(scenarioDir)
    .filter((name) => name.endsWith('.yaml'))
    .map((name) => name.slice(0, -'.yaml'.length));

  const pages = ['index.astro', '[run].astro'].map((name) => [
    `src/pages/demo/${name}`,
    readFileSync(join(scriptDir, '..', 'src', 'pages', 'demo', name), 'utf8'),
  ]);

  const problems = problemsIn(record, scenarioIds).concat(livenessProblemsIn(pages));
  if (problems.length > 0) {
    console.error(`check-demo.mjs: ${problems.length} problem(s):\n- ${problems.join('\n- ')}`);
    process.exit(1);
  }
  console.log(
    `check-demo.mjs: OK (${record.scenarios.length} runs, ` +
      `${record.scenarios.reduce((total, one) => total + one.checks.length, 0)} checks, all held)`,
  );
}

main();
