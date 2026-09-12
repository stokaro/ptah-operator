#!/usr/bin/env node
// Holds the sidebar and the built pages against each other.
//
// A page nobody can navigate to is published and unreachable; a sidebar entry
// with no page behind it is a link to a 404. Both are invisible in a build log,
// and both are what this refuses.

import { existsSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { sidebar } from '../src/sidebar.mjs';

const scriptDir = dirname(fileURLToPath(import.meta.url));

// routes flattens the sidebar into the addresses it offers.
export function routes(groups) {
  const found = [];
  const walk = (items) => {
    for (const item of items) {
      if (item.link) found.push(item.link);
      if (item.items) walk(item.items);
    }
  };
  walk(groups);
  return found;
}

// builtRoutes lists what the build published, as sidebar-shaped links.
export function builtRoutes(root) {
  const found = [];
  const walk = (directory) => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const full = join(directory, entry.name);
      if (entry.isDirectory()) {
        walk(full);
        continue;
      }
      if (entry.name !== 'index.html') continue;
      const route = relative(root, directory).split('\\').join('/');
      found.push(route === '' ? '/' : `/${route}/`);
    }
  };
  walk(root);
  return found.sort();
}

// EXEMPT are pages a reader reaches without navigating to them.
const EXEMPT = new Set(['/404/']);

function selftest() {
  const flattened = routes([
    { label: 'A', items: [{ label: 'one', link: '/one/' }, { label: 'nested', items: [{ label: 'two', link: '/two/' }] }] },
  ]);
  if (flattened.join(',') !== '/one/,/two/') throw new Error(`flattened as ${flattened.join(',')}`);
  console.log('check-navigation.mjs --selftest: OK (flattening, including nested groups)');
}

function main() {
  if (process.argv.includes('--selftest')) {
    selftest();
    return;
  }
  const root = resolve(scriptDir, '..', 'dist');
  if (!existsSync(root) || !statSync(root).isDirectory()) {
    console.error('check-navigation.mjs: dist/ is missing; build the site first');
    process.exit(1);
  }
  const declared = routes(sidebar);
  const built = builtRoutes(root).filter((route) => !EXEMPT.has(route));
  if (declared.length === 0 || built.length === 0) {
    console.error('check-navigation.mjs: the sidebar or the build is empty, so this check would pass by comparing nothing');
    process.exit(1);
  }

  const problems = [];
  for (const route of declared) {
    if (!built.includes(route)) problems.push(`the sidebar offers ${route}, which this build does not publish`);
  }
  for (const route of built) {
    if (!declared.includes(route)) problems.push(`${route} is published and the sidebar does not offer it`);
  }
  if (problems.length > 0) {
    console.error(`check-navigation.mjs: ${problems.length} problem(s):\n- ${problems.join('\n- ')}`);
    process.exit(1);
  }
  console.log(`check-navigation.mjs: OK (${declared.length} pages, each navigable and each navigated to)`);
}

main();
