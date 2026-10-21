#!/usr/bin/env node
// Renders the Ptah compatibility catalog into the Ptah compatibility page.
//
// support/ptah.json is the answer, and the lifecycle suite runs the Ptah image
// it names as its executor. A table typed beside it would be a second copy that
// drifts the first time the pin moves, so this reads the file and writes the
// table between the markers in the page. Without --write it fails when the page
// no longer says what the file does.
//
// It keeps apart what the file keeps apart. A declared range is a promise, a
// verified row is a measurement, and a version with neither shows the check
// that has not run rather than an empty cell, which would read as "works with
// everything".
//
// The prose fields -- statements, scopes, limitations -- are rendered as
// Markdown, because they are written in it: a command in a limitation is in
// backticks, and a reader should see it as code.

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { PageURL } from '../src/lib/docs-origin.mjs';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const repositoryRoot = join(scriptDir, '..', '..', '..');
const catalogPath = join(repositoryRoot, 'support', 'ptah.json');
const pagePath = join(scriptDir, '..', 'src', 'content', 'docs', 'support', 'ptah.md');

const BEGIN = '<!-- BEGIN GENERATED COMPATIBILITY -->';
const END = '<!-- END GENERATED COMPATIBILITY -->';

// SCHEMA_VERSION is the catalog shape this renders. A later shape may carry a
// field this table would silently drop, so it is refused rather than read.
const SCHEMA_VERSION = 1;

const PTAH_COMMIT_URL = 'https://github.com/stokaro/ptah/commit/';

// prose puts a catalog string on one line. A line break inside a list item or
// a table cell ends it, and the rest would render as something else.
function prose(text) {
  return String(text ?? '').replace(/\s+/g, ' ').trim();
}

// cell makes prose safe inside a table row, where a bare pipe starts a column.
function cell(text) {
  return prose(text).replace(/\|/g, '\\|');
}

// buildName is how a reader recognizes a verified build: the release when
// there is one, and what git describe called the commit when there is not.
function buildName(row) {
  return row.ptahRelease ?? row.ptahDescribe;
}

// guideURL is the address of one operator version's guide, or null.
//
// Null when the catalog says that version publishes none. A link to a page
// nobody built looks like the guide exists and answers 404. The address is
// absolute because every version is its own build, and a link from one build
// into another is outside what either one can check.
export function guideURL(entry) {
  return entry.documentation?.published ? PageURL(entry.operator, '') : null;
}

// check refuses a catalog this table would misstate. The Go verifier holds the
// whole contract; these are the shapes that would render as a claim nobody
// made.
export function check(catalog) {
  if (catalog.schemaVersion !== SCHEMA_VERSION) {
    throw new Error(`support/ptah.json declares schema version ${catalog.schemaVersion}; this renders ${SCHEMA_VERSION}`);
  }
  const releases = catalog.releases ?? [];
  if (releases.length === 0) {
    throw new Error('support/ptah.json lists no operator version, and an empty table reads as complete');
  }
  for (const entry of releases) {
    const name = entry.operator;
    if (prose(name) === '') throw new Error('support/ptah.json carries a row with no operator version');
    const range = entry.declared?.range ?? null;
    if (range === null && prose(entry.declared?.statement) === '') {
      throw new Error(`${name} declares no range and does not say why`);
    }
    const verified = entry.verified ?? [];
    if (verified.length === 0 && prose(entry.unverifiedReason) === '') {
      throw new Error(`${name} records no verified build and does not say which check is missing`);
    }
    for (const row of verified) {
      if (prose(row.ptahCommit) === '' || prose(buildName(row)) === '') {
        throw new Error(`${name} verifies a build with no commit or no readable name`);
      }
      if (prose(row.scope) === '') {
        throw new Error(`${name} verifies ${row.ptahCommit} and does not say what ran`);
      }
    }
  }
  return catalog;
}

function summaryRow(entry) {
  const range = entry.declared?.range ?? null;
  const declared = range === null ? 'No range' : `\`${cell(range)}\``;
  const verified = entry.verified ?? [];
  const measured = verified.length === 0
    ? 'Not verified'
    : verified
      .map((row) => `\`${cell(buildName(row))}\`${row.ptahRelease ? '' : ', not a release'}`)
      .join('; ');
  const url = guideURL(entry);
  const guide = url ? `[${entry.operator}](${url})` : 'Not published';
  return `| \`${cell(entry.operator)}\` | ${declared} | ${measured} | ${guide} |`;
}

// measurement is one verified build. A scope an earlier version already
// states is referenced rather than repeated: the development state and a
// release that ran another build through the same suite measure the same
// thing, and the build is what differs.
function measurement(row, earlier = []) {
  const identity = row.ptahRelease
    ? `Ptah \`${prose(row.ptahRelease)}\``
    : `\`${prose(row.ptahDescribe)}\`, which is not a Ptah release`;
  const commit = prose(row.ptahCommit);
  const scope = prose(row.scope);
  const match = earlier.find((other) => (other.verified ?? []).some((candidate) => prose(candidate.scope) === scope));
  return (
    `- ${identity}. Commit [\`${commit}\`](${PTAH_COMMIT_URL}${commit}), ` +
    `runner protocol version ${row.runnerProtocolVersion}, evidence \`${prose(row.evidence)}\`. ` +
    (match ? `The scope is the same as for [\`${prose(match.operator)}\`](#${anchor(match)}).` : scope)
  );
}

// The three prose fields of a version, compared after the same whitespace
// folding the page applies.
const FIELDS = {
  declared: (entry) => ({ range: entry.declared?.range ?? null, statement: prose(entry.declared?.statement) }),
  verified: (entry) => ({
    verified: (entry.verified ?? []).map((row) => measurement(row)),
    reason: prose(entry.unverifiedReason),
  }),
  limitations: (entry) => (entry.limitations ?? []).map(prose),
};

// anchor is the heading slug the site gives a version's section: lowercase,
// punctuation dropped, so `### v0.2.0` is #v020.
function anchor(entry) {
  return prose(entry.operator).toLowerCase().replace(/[^a-z0-9 -]/g, '').replace(/ /g, '-');
}

// sameAs names the first earlier version whose field reads exactly like this
// one's. A release and the development state that run one build carry the
// same prose, and printing it twice makes the page say each sentence twice;
// the later section points at the earlier one instead.
function sameAs(field, entry, earlier) {
  const key = JSON.stringify(FIELDS[field](entry));
  const match = earlier.find((other) => JSON.stringify(FIELDS[field](other)) === key);
  return match ? `the same as [\`${prose(match.operator)}\`](#${anchor(match)}).` : null;
}

function section(entry, earlier) {
  const lines = [`### ${prose(entry.operator)}`, ''];
  const stage = entry.stage === 'development' ? 'The development state.' : 'A released version.';
  const url = guideURL(entry);
  lines.push(url ? `${stage} Its guide is at [${url}](${url}).` : `${stage} It publishes no guide.`, '');

  const range = entry.declared?.range ?? null;
  const statement = prose(entry.declared?.statement);
  const declared = range === null ? statement : [`\`${prose(range)}\`.`, statement].filter(Boolean).join(' ');
  lines.push(`**Declared:** ${sameAs('declared', entry, earlier) ?? declared}`, '');

  const verified = entry.verified ?? [];
  const sameVerified = sameAs('verified', entry, earlier);
  if (sameVerified) {
    lines.push(`**Verified:** ${sameVerified}`, '');
  } else if (verified.length === 0) {
    lines.push(`**Verified:** nothing. ${prose(entry.unverifiedReason)}`, '');
  } else {
    lines.push('**Verified:**', '', ...verified.map((row) => measurement(row, earlier)), '');
  }

  const limitations = entry.limitations ?? [];
  if (limitations.length > 0) {
    const sameLimitations = sameAs('limitations', entry, earlier);
    if (sameLimitations) {
      lines.push(`**Limitations:** ${sameLimitations}`, '');
    } else {
      lines.push('**Limitations:**', '', ...limitations.map((limitation) => `- ${prose(limitation)}`), '');
    }
  }
  return lines;
}

// render writes the generated block: the axis, when the claims were measured,
// one summary row per operator version, the detail of each version, and what
// each piece of evidence is.
export function render(catalog) {
  const releases = catalog.releases ?? [];
  const lines = [
    `**Axis:** ${prose(catalog.axis)}`,
    '',
    `**Last measured:** ${prose(catalog.lastVerified)}.`,
    '',
    '| Operator | Declared range | Verified builds | Guide |',
    '| --- | --- | --- | --- |',
    ...releases.map(summaryRow),
    '',
  ];
  releases.forEach((entry, index) => lines.push(...section(entry, releases.slice(0, index))));

  const evidence = Object.entries(catalog.evidence ?? {});
  if (evidence.length > 0) {
    lines.push('### Evidence', '');
    for (const [name, description] of evidence) {
      lines.push(`- \`${name}\`: ${[prose(description.what), prose(description.pin)].filter(Boolean).join(' ')}`);
    }
    lines.push('');
  }
  while (lines.at(-1) === '') lines.pop();
  return lines.join('\n');
}

export function replaceBlock(page, block) {
  const begin = page.indexOf(BEGIN);
  const end = page.indexOf(END);
  if (begin === -1 || end === -1 || end < begin) {
    throw new Error(`${pagePath} does not carry the generated block markers`);
  }
  return `${page.slice(0, begin + BEGIN.length)}\n${block}\n${page.slice(end)}`;
}

function refuses(catalog, pattern) {
  try {
    check(catalog);
  } catch (error) {
    return pattern.test(error.message);
  }
  return false;
}

function selftest() {
  const commit = '0123456789abcdef0123456789abcdef01234567';
  const catalog = {
    schemaVersion: 1,
    lastVerified: '2026-09-12',
    axis: 'an axis',
    releases: [
      {
        operator: 'edge',
        stage: 'development',
        documentation: { published: true, source: 'master' },
        declared: { range: null, statement: 'no range is claimed' },
        verified: [
          {
            ptahRelease: null,
            ptahCommit: commit,
            ptahDescribe: 'v0.3.0-201-g0123456789',
            runnerProtocolVersion: 5,
            evidence: 'kubernetes-e2e',
            scope: 'the lifecycle',
          },
        ],
        limitations: ['run `ptah migrations up`\nwith the flag'],
      },
      {
        operator: 'v0.2.0',
        stage: 'released',
        documentation: { published: true, source: 'v0.2.0' },
        declared: { range: '>=0.8.0 | <0.9.0', statement: '' },
        verified: [
          {
            ptahRelease: 'v0.8.1',
            ptahCommit: commit.replace('0', 'f'),
            ptahDescribe: 'v0.8.1',
            runnerProtocolVersion: 5,
            evidence: 'kubernetes-e2e',
            scope: 'the lifecycle again',
          },
        ],
      },
      {
        operator: 'v0.1.0',
        stage: 'released',
        documentation: { published: false },
        declared: { range: null, statement: 'nothing is promised' },
        verified: [],
        unverifiedReason: 'the suite has not run against it',
      },
      {
        operator: 'v0.3.0',
        stage: 'released',
        documentation: { published: false },
        declared: { range: null, statement: 'no  range is\nclaimed' },
        verified: [
          {
            ptahRelease: null,
            ptahCommit: commit,
            ptahDescribe: 'v0.3.0-201-g0123456789',
            runnerProtocolVersion: 5,
            evidence: 'kubernetes-e2e',
            scope: 'the lifecycle',
          },
        ],
        limitations: ['run `ptah migrations up` with the flag'],
      },
      {
        operator: 'v0.4.0',
        stage: 'released',
        documentation: { published: false },
        declared: { range: null, statement: 'no range is claimed' },
        verified: [
          {
            ptahRelease: 'v0.9.0',
            ptahCommit: commit.replace('1', 'e'),
            ptahDescribe: 'v0.9.0',
            runnerProtocolVersion: 5,
            evidence: 'kubernetes-e2e',
            scope: 'the lifecycle',
          },
        ],
        limitations: ['run `ptah migrations up` with the flag'],
      },
    ],
    evidence: { 'kubernetes-e2e': { what: 'the suite', pin: 'one declaration' } },
  };
  const block = render(check(catalog));
  const rows = block.split('\n').filter((line) => line.startsWith('| `'));
  if (rows.length !== 5) throw new Error(`the summary has ${rows.length} rows for five versions`);
  const [edge, released, absent] = rows;

  // Verified, and not a release: the describe string names it, and the row
  // says it is not a release rather than leaving the reader to infer it.
  if (!edge.includes('`v0.3.0-201-g0123456789`, not a release')) throw new Error('a commit pin read as a release');
  if (!block.includes(`[\`${commit}\`](https://github.com/stokaro/ptah/commit/${commit})`)) {
    throw new Error('the verified commit is not shown in full with its link');
  }
  // Verified, and a release.
  if (!released.includes('`v0.8.1`') || released.includes('not a release')) {
    throw new Error('a verified release was not named as one');
  }
  // Declared: a range is shown, and a pipe inside it does not split the row.
  if (!released.includes('`>=0.8.0 \\| <0.9.0`')) throw new Error('a declared range is missing or split the row');
  if (!edge.includes('| No range |')) throw new Error('an absent range was not said');
  if (!block.includes('**Declared:** no range is claimed')) throw new Error('an absent range lost its statement');
  // Absent: nothing verified, and the missing check is named.
  if (!absent.includes('| Not verified |')) throw new Error('an unmeasured version was not marked');
  if (!block.includes('**Verified:** nothing. the suite has not run against it')) {
    throw new Error('the check that has not run was not named');
  }
  // A guide is linked only where one is published.
  if (!edge.includes('(https://operator.ptah.run/edge/)')) throw new Error('a published guide got no link');
  if (block.includes('operator.ptah.run/v0.1.0/')) throw new Error('an unpublished guide was linked anyway');
  // Prose stays on one line and keeps its Markdown.
  if (!block.includes('- run `ptah migrations up` with the flag')) throw new Error('a limitation broke across lines');
  // A version that reads exactly like an earlier one points at it instead of
  // saying every sentence again; a version that differs keeps its own prose.
  const repeat = block.slice(block.indexOf('### v0.3.0'), block.indexOf('### v0.4.0'));
  for (const field of ['Declared', 'Verified', 'Limitations']) {
    if (!repeat.includes(`**${field}:** the same as [\`edge\`](#edge).`)) {
      throw new Error(`a repeated ${field} field was printed again rather than referenced`);
    }
  }
  if (block.split('the lifecycle').length !== 3 || block.split('with the flag').length !== 2) {
    throw new Error('a repeated version printed its prose twice');
  }
  if (block.slice(block.indexOf('### v0.2.0'), block.indexOf('### v0.1.0')).includes('the same as')) {
    throw new Error('a version with prose of its own was referenced to another');
  }
  // Another build through the same suite names its build and references the
  // scope and limitations it shares, so no sentence is said twice.
  const rebuilt = block.slice(block.indexOf('### v0.4.0'));
  if (!rebuilt.includes('Ptah `v0.9.0`') || !rebuilt.includes('The scope is the same as for [`edge`](#edge).') ||
      !rebuilt.includes('**Limitations:** the same as [`edge`](#edge).')) {
    throw new Error('a version that ran another build repeated or lost its shared prose');
  }
  if (anchor({ operator: 'v0.2.0' }) !== 'v020') throw new Error('a version anchor is not the heading slug');
  if (!block.includes('- `kubernetes-e2e`: the suite one declaration')) throw new Error('the evidence is not described');

  // The refusals: shapes that would render as a claim nobody made.
  if (!refuses({ ...catalog, schemaVersion: 2 }, /schema version 2/)) throw new Error('a later schema was read');
  if (!refuses({ ...catalog, releases: [] }, /lists no operator version/)) throw new Error('an empty catalog was rendered');
  const silent = structuredClone(catalog);
  delete silent.releases[2].unverifiedReason;
  if (!refuses(silent, /v0\.1\.0 records no verified build/)) throw new Error('an empty verified list with no reason was rendered');
  const unexplained = structuredClone(catalog);
  unexplained.releases[0].declared.statement = '';
  if (!refuses(unexplained, /edge declares no range and does not say why/)) {
    throw new Error('an absent range with no statement was rendered');
  }

  // The freshness check compares, and --write replaces only the block.
  const page = `before\n${BEGIN}\nold\n${END}\nafter\n`;
  const updated = replaceBlock(page, block);
  if (updated === page) throw new Error('a stale block read as current');
  if (replaceBlock(updated, block) !== updated) throw new Error('a current block read as stale');
  if (!updated.startsWith('before\n') || !updated.endsWith(`${END}\nafter\n`)) {
    throw new Error('the text around the block was changed');
  }
  let unmarked = false;
  try {
    replaceBlock('no markers here', block);
  } catch {
    unmarked = true;
  }
  if (!unmarked) throw new Error('a page with no markers was accepted');

  console.log(
    'build-ptah-compatibility.mjs --selftest: OK (declared, verified, release and commit pin, absent, ' +
      'guide links, one-line prose, repeated versions, refusals, staleness)',
  );
}

function main() {
  const arguments_ = process.argv.slice(2);
  if (arguments_.includes('--selftest')) {
    selftest();
    return;
  }
  const block = render(check(JSON.parse(readFileSync(catalogPath, 'utf8'))));
  const page = readFileSync(pagePath, 'utf8');
  const updated = replaceBlock(page, block);
  if (arguments_.includes('--write')) {
    writeFileSync(pagePath, updated);
    console.log('build-ptah-compatibility.mjs: wrote the compatibility table');
    return;
  }
  if (updated !== page) {
    console.error(
      'build-ptah-compatibility.mjs: the compatibility table is stale; run npm run compatibility:write in docs/site',
    );
    process.exit(1);
  }
  const versions = block.split('\n').filter((line) => line.startsWith('| `')).length;
  console.log(`build-ptah-compatibility.mjs: OK (${versions} operator version(s))`);
}

main();
