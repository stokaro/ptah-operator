#!/usr/bin/env node
// Holds every Mermaid diagram on the site to a size a phone can read.
//
// A diagram is drawn as one SVG and scaled down to the column it sits in, so a
// wide one keeps its 14px labels in the source and paints them at 4px on a
// phone. Nothing in the Markdown says how wide a diagram comes out, and a
// diagram that has grown by one box reads exactly like one that has not. This
// check opens each page that carries a diagram at a 390px viewport, waits for
// every diagram to render, and refuses one whose smallest label is painted
// below the floor, one that did not render, and a page that scrolls sideways.
//
//   node scripts/check-diagram-size.mjs [--dist <dir>] [--selftest]
//
// Requires Playwright's chromium. Without it this skips and says what to
// install; with CI=1 it fails instead, because a green check that measured
// nothing is worse than a red one.

import { createServer } from 'node:http';
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, extname, join, relative, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const siteRoot = join(scriptDir, '..');

// The width of a common phone, and the smallest label size measured readable on
// it when the lifecycle diagrams were redrawn (#888, #923).
export const viewportWidth = 390;
export const labelFloor = 11;

const diagramMarker = /<pre class="mermaid" data-diagram>/g;

const mimeTypes = {
  '.html': 'text/html; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.json': 'application/json',
  '.svg': 'image/svg+xml',
  '.woff2': 'font/woff2',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
};

function detectBase(distRoot) {
  const home = join(distRoot, 'index.html');
  if (!existsSync(home)) return '';
  const match = readFileSync(home, 'utf8').match(/(?:href|src)="([^"]*)\/_astro\//);
  return match ? match[1] : '';
}

function startServer(distRoot, base) {
  const server = createServer((request, response) => {
    let url = decodeURIComponent((request.url ?? '/').split('?')[0]);
    if (base && url.startsWith(base)) url = url.slice(base.length) || '/';
    let filePath = join(distRoot, url);
    if (existsSync(filePath) && statSync(filePath).isDirectory()) filePath = join(filePath, 'index.html');
    if (!existsSync(filePath) || !statSync(filePath).isFile()) {
      response.writeHead(404);
      response.end('not found');
      return;
    }
    response.writeHead(200, { 'content-type': mimeTypes[extname(filePath)] ?? 'application/octet-stream' });
    response.end(readFileSync(filePath));
  });
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => resolve({ server, port: server.address().port }));
  });
}

// Every built page that carries a diagram, with how many it carries, read from
// the HTML the build wrote rather than from a list kept here.
export function pagesWithDiagrams(distRoot) {
  const pages = [];
  const walk = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const path = join(dir, entry.name);
      if (entry.isDirectory()) walk(path);
      else if (entry.name === 'index.html') {
        const count = (readFileSync(path, 'utf8').match(diagramMarker) ?? []).length;
        if (count > 0) {
          const route = relative(distRoot, dirname(path)).split(sep).join('/');
          pages.push({ route: route ? `${route}/` : '', count });
        }
      }
    }
  };
  walk(distRoot);
  return pages.sort((a, b) => a.route.localeCompare(b.route));
}

// What a page's readings owe: as many rendered diagrams as the HTML carries,
// each with a label painted at the floor or above, and no sideways scroll.
export function diagramProblems(page, reading) {
  const problems = [];
  if (reading.diagrams.length !== page.count) {
    problems.push(`${page.route}: ${reading.diagrams.length} of ${page.count} diagrams were measured`);
  }
  reading.diagrams.forEach((diagram, index) => {
    const name = `${page.route} diagram ${index + 1}`;
    if (diagram.state !== 'true') {
      problems.push(`${name} did not render (${diagram.state ?? 'never drawn'})`);
      return;
    }
    if (!(diagram.labels > 0)) {
      problems.push(`${name} has no label to measure`);
      return;
    }
    if (!(diagram.smallest >= labelFloor)) {
      problems.push(
        `${name} paints "${diagram.smallestText}" at ${diagram.smallest.toFixed(1)}px, below ${labelFloor}px: ` +
          `it is ${Math.round(diagram.natural)}px wide and drawn at ${Math.round(diagram.drawn)}px`,
      );
    }
  });
  if (reading.scrollWidth > viewportWidth) {
    problems.push(`${page.route} scrolls sideways: ${reading.scrollWidth}px wide at ${viewportWidth}px`);
  }
  return problems;
}

function selftest() {
  const page = { route: 'reference/example/', count: 2 };
  const readable = { state: 'true', labels: 9, smallest: 11.6, smallestText: 'Ready', natural: 419, drawn: 348 };
  const failures = [];
  const expect = (label, reading, want) => {
    const problems = diagramProblems(page, reading);
    const got = problems.find((problem) => problem.includes(want));
    if (want === '' ? problems.length !== 0 : got === undefined) {
      failures.push(`${label}: got ${JSON.stringify(problems)}, want ${want === '' ? 'none' : JSON.stringify(want)}`);
    }
  };
  expect('readable diagrams', { diagrams: [readable, readable], scrollWidth: 390 }, '');
  expect('a label exactly at the floor', { diagrams: [readable, { ...readable, smallest: labelFloor }], scrollWidth: 390 }, '');
  // The diagram the architecture page drew before #923.
  expect(
    'a wide diagram',
    { diagrams: [readable, { ...readable, smallest: 4.3, natural: 1297 }], scrollWidth: 390 },
    'at 4.3px, below 11px',
  );
  expect('a diagram that stayed source', { diagrams: [readable, { state: 'failed' }], scrollWidth: 390 }, 'did not render (failed)');
  expect('a diagram never drawn', { diagrams: [readable, {}], scrollWidth: 390 }, 'did not render (never drawn)');
  expect('a diagram with no labels', { diagrams: [readable, { ...readable, labels: 0, smallest: Infinity }], scrollWidth: 390 }, 'has no label');
  expect('a missing diagram', { diagrams: [readable], scrollWidth: 390 }, '1 of 2 diagrams were measured');
  expect('no diagram at all', { diagrams: [], scrollWidth: 390 }, '0 of 2 diagrams were measured');
  expect('a page that scrolls sideways', { diagrams: [readable, readable], scrollWidth: 412 }, 'scrolls sideways');
  if (failures.length > 0) {
    console.error(`check-diagram-size.mjs --selftest: ${failures.length} failure(s):\n- ${failures.join('\n- ')}`);
    process.exit(1);
  }
  console.log(
    'check-diagram-size.mjs --selftest: OK (refuses a label under the floor, a diagram that did not render ' +
      'or has no label, a missing diagram, and a page that scrolls sideways)',
  );
}

async function measure(browser, url) {
  const page = await browser.newPage({ viewport: { width: viewportWidth, height: 844 } });
  try {
    await page.goto(url, { waitUntil: 'networkidle' });
    await page
      .waitForFunction(
        () => [...document.querySelectorAll('pre.mermaid[data-diagram]')].every((node) => node.dataset.rendered),
        null,
        { timeout: 30000 },
      )
      .catch(() => {});
    return await page.evaluate(() => {
      const diagrams = [...document.querySelectorAll('pre.mermaid[data-diagram]')].map((node) => {
        const svg = node.querySelector('svg');
        if (node.dataset.rendered !== 'true' || !svg) return { state: node.dataset.rendered };
        const natural = svg.viewBox.baseVal?.width || svg.getBBox().width;
        const drawn = svg.getBoundingClientRect().width;
        let smallest = Infinity;
        let smallestText = '';
        let labels = 0;
        for (const label of svg.querySelectorAll('text, foreignObject div, foreignObject span')) {
          const text = label.textContent.trim();
          if (!text || label.getBoundingClientRect().width === 0) continue;
          labels += 1;
          const painted = parseFloat(getComputedStyle(label).fontSize) * (drawn / natural);
          if (painted < smallest) {
            smallest = painted;
            smallestText = text.slice(0, 40);
          }
        }
        return { state: 'true', natural, drawn, labels, smallest, smallestText };
      });
      return { diagrams, scrollWidth: document.documentElement.scrollWidth };
    });
  } finally {
    await page.close();
  }
}

async function main() {
  if (process.argv.includes('--selftest')) {
    selftest();
    return;
  }

  const distIndex = process.argv.indexOf('--dist');
  const distRoot = distIndex >= 0 ? process.argv[distIndex + 1] : join(siteRoot, 'dist');
  if (!existsSync(distRoot)) {
    console.error('check-diagram-size.mjs: dist/ is missing; build the site first');
    process.exit(1);
  }

  let chromium;
  try {
    ({ chromium } = await import('playwright'));
  } catch {
    const message =
      'check-diagram-size.mjs: playwright is not installed (npm i -D playwright && npx playwright install chromium)';
    if (process.env.CI) {
      console.error(message);
      process.exit(1);
    }
    console.warn(`${message}; skipping`);
    return;
  }

  const pages = pagesWithDiagrams(distRoot);
  if (pages.length === 0) {
    console.error('check-diagram-size.mjs: no built page carries a diagram, so nothing was measured');
    process.exit(1);
  }
  const base = detectBase(distRoot);
  const { server, port } = await startServer(distRoot, base);
  const browser = await chromium.launch();
  const problems = [];
  let measured = 0;
  try {
    for (const page of pages) {
      const reading = await measure(browser, `http://127.0.0.1:${port}${base}/${page.route}`);
      measured += reading.diagrams.length;
      problems.push(...diagramProblems(page, reading));
    }
  } finally {
    await browser.close();
    server.close();
  }

  if (problems.length > 0) {
    console.error(`check-diagram-size.mjs: ${problems.length} problem(s):\n- ${problems.join('\n- ')}`);
    process.exit(1);
  }
  console.log(
    `check-diagram-size.mjs: OK (${measured} diagrams on ${pages.length} pages, every label at ` +
      `${labelFloor}px or more at ${viewportWidth}px)`,
  );
}

await main();
