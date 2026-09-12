#!/usr/bin/env node
// Refuses a built site whose own links do not resolve.
//
// It reads dist/ rather than the Markdown, because what a reader follows is the
// rendered href: a link that Starlight rewrote, a heading whose slug changed,
// and a page that moved all look fine in the source and land on a 404.

import { existsSync, readFileSync, readdirSync, statSync, writeFileSync, mkdirSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const distinct = (values) => [...new Set(values)];

export function htmlFiles(root) {
  const found = [];
  const walk = (directory) => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const full = join(directory, entry.name);
      if (entry.isDirectory()) walk(full);
      else if (entry.name.endsWith('.html')) found.push(full);
    }
  };
  walk(root);
  return found.sort();
}

// internalLinks returns the hrefs a reader can follow inside this build.
export function internalLinks(html) {
  return distinct([...html.matchAll(/href="([^"]+)"/g)].map((match) => match[1]))
    .filter((href) => href.startsWith('/'))
    .filter((href) => !href.startsWith('//'));
}

export function anchors(html) {
  return new Set([...html.matchAll(/id="([^"]+)"/g)].map((match) => match[1]));
}

// resolveTarget maps a site-absolute href onto the file the deploy serves.
export function resolveTarget(root, base, href) {
  const [path, fragment] = href.split('#');
  if (!path.startsWith(base)) return { missing: true, path, fragment };
  const withinVersion = path.slice(base.length);
  const candidates = withinVersion === '' || withinVersion.endsWith('/')
    ? [join(root, withinVersion, 'index.html')]
    : [join(root, withinVersion), join(root, `${withinVersion}.html`)];
  const file = candidates.find((candidate) => existsSync(candidate) && statSync(candidate).isFile());
  return { file, missing: file === undefined, path, fragment };
}

function selftest() {
  const root = join(scriptDir, '..', '.selftest-links');
  mkdirSync(join(root, 'page'), { recursive: true });
  writeFileSync(join(root, 'index.html'), '<a href="/edge/page/">ok</a><a href="/edge/page/#here">ok</a><a href="/edge/gone/">bad</a>');
  writeFileSync(join(root, 'page', 'index.html'), '<h2 id="here">here</h2>');

  const html = readFileSync(join(root, 'index.html'), 'utf8');
  const links = internalLinks(html);
  if (links.length !== 3) throw new Error(`found ${links.length} links`);
  if (resolveTarget(root, '/edge/', '/edge/page/').missing) throw new Error('a real page was reported missing');
  if (!resolveTarget(root, '/edge/', '/edge/gone/').missing) throw new Error('a missing page was reported present');
  const target = resolveTarget(root, '/edge/', '/edge/page/#here');
  if (!anchors(readFileSync(target.file, 'utf8')).has('here')) throw new Error('a real anchor was not found');
  console.log('check-links.mjs --selftest: OK (link extraction, resolution, both directions, anchors)');
}

function main() {
  if (process.argv.includes('--selftest')) {
    selftest();
    return;
  }
  const version = process.env.DOCS_VERSION || 'edge';
  const base = `/${version}/`;
  const root = resolve(scriptDir, '..', 'dist');
  if (!existsSync(root)) {
    console.error('check-links.mjs: dist/ is missing; build the site first');
    process.exit(1);
  }
  const pages = htmlFiles(root);
  if (pages.length === 0) {
    console.error('check-links.mjs: dist/ holds no pages, so this check would pass by reading nothing');
    process.exit(1);
  }

  const problems = [];
  let checked = 0;
  for (const page of pages) {
    const html = readFileSync(page, 'utf8');
    for (const href of internalLinks(html)) {
      checked += 1;
      const target = resolveTarget(root, base, href);
      if (target.missing) {
        problems.push(`${relative(root, page)} links to ${href}, which this build does not serve`);
        continue;
      }
      if (target.fragment && !anchors(readFileSync(target.file, 'utf8')).has(target.fragment)) {
        problems.push(`${relative(root, page)} links to ${href}, whose target has no such anchor`);
      }
    }
  }
  if (problems.length > 0) {
    console.error(`check-links.mjs: ${problems.length} broken link(s):\n- ${problems.join('\n- ')}`);
    process.exit(1);
  }
  console.log(`check-links.mjs: OK (${checked} internal links across ${pages.length} pages)`);
}

main();
