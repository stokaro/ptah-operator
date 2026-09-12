#!/usr/bin/env node
// Renders the chart's values into the configuration page.
//
// values.yaml is the source: every key there already carries the sentence that
// says what it does, and a hand-written table beside it would be a second copy
// that drifts the first time somebody adds a key. This reads the file, keeps
// the comment that sits above each key, and writes the table between the
// markers in the page.
//
// A YAML parser is deliberately not used. It returns the values and drops the
// comments, and the comments are most of what a reader needs.

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const repositoryRoot = join(scriptDir, '..', '..', '..');
const valuesPath = join(repositoryRoot, 'charts', 'ptah-operator', 'values.yaml');
const pagePath = join(scriptDir, '..', 'src', 'content', 'docs', 'use', 'configuration.md');

const BEGIN = '<!-- BEGIN GENERATED VALUES -->';
const END = '<!-- END GENERATED VALUES -->';

// parseValues reads the chart values into rows of {key, default, note}.
//
// The shape it understands is the shape the chart is written in: two-space
// indentation, `key: value` and `key:` for a mapping, `#` comments above a key,
// and no block scalars. It refuses anything else rather than guessing, because
// a row quietly dropped from this table is a setting the documentation stops
// mentioning.
export function parseValues(text) {
  const rows = [];
  const path = [];
  let comment = [];

  text.split('\n').forEach((line, index) => {
    if (line.trim() === '') {
      comment = [];
      return;
    }
    const commentMatch = /^(\s*)#\s?(.*)$/.exec(line);
    if (commentMatch) {
      comment.push(commentMatch[2].trim());
      return;
    }
    const keyMatch = /^(\s*)([A-Za-z][A-Za-z0-9_-]*):(.*)$/.exec(line);
    if (!keyMatch) {
      // A list item under a key, or a shape this reader does not model.
      if (/^\s*-\s/.test(line)) {
        comment = [];
        return;
      }
      throw new Error(`values.yaml line ${index + 1} is not a key, a comment or a list item: ${line}`);
    }
    const [, indent, key, rest] = keyMatch;
    if (indent.length % 2 !== 0) {
      throw new Error(`values.yaml line ${index + 1} is indented by ${indent.length} spaces; the chart uses two`);
    }
    const depth = indent.length / 2;
    path.length = depth;
    path.push(key);

    const value = rest.trim();
    rows.push({
      key: path.join('.'),
      default: value,
      note: comment.join(' '),
      mapping: value === '',
    });
    comment = [];
  });
  return rows;
}

// render writes the table. A mapping key becomes a heading row rather than a
// value row, so the reader sees the structure the chart has.
export function render(rows) {
  const lines = ['| Value | Default | What it does |', '| --- | --- | --- |'];
  for (const row of rows) {
    const shown = row.mapping ? '' : `\`${row.default}\``;
    const note = row.note === '' ? '' : row.note.replace(/\|/g, '\\|');
    lines.push(`| \`${row.key}\` | ${shown} | ${note} |`);
  }
  return lines.join('\n');
}

function replaceBlock(page, table) {
  const begin = page.indexOf(BEGIN);
  const end = page.indexOf(END);
  if (begin === -1 || end === -1 || end < begin) {
    throw new Error(`${pagePath} does not carry the generated block markers`);
  }
  return `${page.slice(0, begin + BEGIN.length)}\n${table}\n${page.slice(end)}`;
}

function selftest() {
  const rows = parseValues(['# what it is', 'top: "1"', 'group:', '  # nested note', '  inner: 2', ''].join('\n'));
  const keys = rows.map((row) => row.key).join(',');
  if (keys !== 'top,group,group.inner') throw new Error(`paths are ${keys}`);
  if (rows[0].note !== 'what it is') throw new Error('the comment above a key is lost');
  if (!rows[1].mapping) throw new Error('a mapping key is rendered as a value');
  if (rows[2].default !== '2') throw new Error('a nested value is lost');
  let threw = false;
  try {
    parseValues('   odd: 1\n');
  } catch {
    threw = true;
  }
  if (!threw) throw new Error('an unexpected indentation was accepted');
  console.log('build-values-reference.mjs --selftest: OK (paths, comments, mappings, refusal)');
}

function main() {
  const arguments_ = process.argv.slice(2);
  if (arguments_.includes('--selftest')) {
    selftest();
    return;
  }
  const table = render(parseValues(readFileSync(valuesPath, 'utf8')));
  const page = readFileSync(pagePath, 'utf8');
  const updated = replaceBlock(page, table);
  if (arguments_.includes('--write')) {
    writeFileSync(pagePath, updated);
    console.log('build-values-reference.mjs: wrote the values table');
    return;
  }
  if (updated !== page) {
    console.error('build-values-reference.mjs: the values table is stale; run npm run values:write in docs/site');
    process.exit(1);
  }
  console.log(`build-values-reference.mjs: OK (${table.split('\n').length - 2} values)`);
}

main();
