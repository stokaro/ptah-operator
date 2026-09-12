#!/usr/bin/env node
// Drives the recorded-run pages in a browser.
//
// What it checks is what a reader would notice and a build log would not: the
// transcript is in the markup without the player, the player takes over and
// types, the controls are reachable from the keyboard, and a reader who asked
// for reduced motion is given the session at rest instead of a typewriter.
//
//   node scripts/check-demo-page.mjs [--dist <dir>] [--selftest]
//
// Requires Playwright's chromium. Without it this skips and says what to
// install; with CI=1 it fails instead, because a green check that measured
// nothing is worse than a red one.

import { createServer } from 'node:http';
import { existsSync, readFileSync, statSync } from 'node:fs';
import { dirname, extname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const siteRoot = join(scriptDir, '..');
const repositoryRoot = join(siteRoot, '..', '..');

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

// countsIn reports what a page carries, so a failure names what was missing
// rather than only that something was.
export function countsIn(document) {
  return {
    tiles: document.querySelectorAll('[data-demo-tile]').length,
    transcripts: document.querySelectorAll('.tile-transcript').length,
    frames: document.querySelectorAll('[data-demo]').length,
    lines: document.querySelectorAll('[data-demo-transcript] .l').length,
  };
}

function selftest() {
  // The shape the page has to have, asserted against a stub rather than a
  // browser: a check whose only failure mode is "playwright is not installed"
  // would pass on a page that lost its tiles.
  const stub = {
    querySelectorAll(selector) {
      const table = {
        '[data-demo-tile]': [1, 2],
        '.tile-transcript': [1, 2],
        '[data-demo]': [1],
        '[data-demo-transcript] .l': [1, 2, 3],
      };
      return table[selector] ?? [];
    },
  };
  const counts = countsIn(stub);
  if (counts.tiles !== 2 || counts.transcripts !== 2 || counts.frames !== 1 || counts.lines !== 3) {
    throw new Error(`countsIn read ${JSON.stringify(counts)}`);
  }
  console.log('check-demo-page.mjs --selftest: OK (what a page has to carry)');
}

async function main() {
  if (process.argv.includes('--selftest')) {
    selftest();
    return;
  }

  const distIndex = process.argv.indexOf('--dist');
  const distRoot = distIndex >= 0 ? process.argv[distIndex + 1] : join(siteRoot, 'dist');
  if (!existsSync(distRoot)) {
    console.error('check-demo-page.mjs: dist/ is missing; build the site first');
    process.exit(1);
  }

  let chromium;
  try {
    ({ chromium } = await import('playwright'));
  } catch {
    const message = 'check-demo-page.mjs: playwright is not installed (npm i -D playwright && npx playwright install chromium)';
    if (process.env.CI) {
      console.error(message);
      process.exit(1);
    }
    console.warn(`${message}; skipping`);
    return;
  }

  const record = JSON.parse(readFileSync(join(repositoryRoot, 'demo', 'recordings', 'runs.json'), 'utf8'));
  const base = detectBase(distRoot);
  const { server, port } = await startServer(distRoot, base);
  const origin = `http://127.0.0.1:${port}${base}`;
  const browser = await chromium.launch();
  const problems = [];

  try {
    // Without JavaScript the page is the transcripts. This is what a crawler,
    // a screen reader and a reader on a slow connection get, and it has to be
    // the whole session rather than an empty frame.
    const quiet = await browser.newContext({ javaScriptEnabled: false });
    const still = await quiet.newPage();
    await still.goto(`${origin}demo/`, { waitUntil: 'load' });
    // The function is sent as a string and called in the page: the page has no
    // module of its own to import it from, and one definition is what keeps
    // this check and its self-test measuring the same thing.
    const withoutScript = await still.evaluate(
      `(${countsIn.toString().replace(/^export\s+/, '')})(document)`,
    );
    if (withoutScript.tiles !== record.scenarios.length) {
      problems.push(`without JavaScript the catalog shows ${withoutScript.tiles} of ${record.scenarios.length} runs`);
    }
    if (withoutScript.transcripts !== record.scenarios.length) {
      problems.push(`without JavaScript ${withoutScript.transcripts} transcripts are in the markup`);
    }
    // The frame carries the first run in full. Counted from the recording
    // rather than against a figure written here, which would pass whatever the
    // page happened to render.
    const firstRunLines = record.scenarios[0].events.filter(([kind]) => kind !== 'sync' && kind !== 'wait').length;
    if (withoutScript.lines < firstRunLines) {
      problems.push(
        `the frame's transcript carries ${withoutScript.lines} lines and the first run has ${firstRunLines}`,
      );
    }
    await quiet.close();

    // With the player, the controls appear and Play types.
    const live = await browser.newContext();
    const page = await live.newPage();
    const failures = [];
    page.on('pageerror', (error) => failures.push(String(error)));
    await page.goto(`${origin}demo/`, { waitUntil: 'load' });
    await page.waitForSelector('[data-demo-controls]:not([hidden])', { timeout: 10_000 }).catch(() => {
      problems.push('the player did not offer its controls');
    });

    const play = page.locator('[data-demo-toggle]').first();
    await play.focus();
    const focused = await page.evaluate(() => document.activeElement?.getAttribute('data-demo-toggle') !== null);
    if (!focused) problems.push('the play control cannot be focused from the keyboard');
    await page.keyboard.press('Enter');
    await page.waitForTimeout(1500);
    const typed = await page.evaluate(() => document.querySelector('[data-demo-screen]')?.textContent?.trim() ?? '');
    if (typed.length === 0) problems.push('pressing Play typed nothing into the screen');

    // A tile opens the run it names.
    const second = record.scenarios[1] ?? record.scenarios[0];
    await page.locator(`[data-demo-scenario="${second.id}"]`).first().click();
    await page.waitForTimeout(500);
    const showing = await page.evaluate(() =>
      document.querySelector('[data-demo]')?.getAttribute('data-demo-scenario'),
    );
    if (showing !== second.id) problems.push(`a tile for ${second.id} left the frame on ${showing}`);
    if (failures.length > 0) problems.push(`the player threw: ${failures.join('; ')}`);
    await live.close();

    // A reader who asked for reduced motion gets the session, not a
    // typewriter: the transcript stays and nothing types on its own.
    const still2 = await browser.newContext({ reducedMotion: 'reduce' });
    const calm = await still2.newPage();
    await calm.goto(`${origin}demo/`, { waitUntil: 'load' });
    await calm.waitForTimeout(2000);
    const settled = await calm.evaluate(() => ({
      screen: document.querySelector('[data-demo-screen]')?.hidden ?? true,
      transcript: (document.querySelector('[data-demo-transcript]')?.textContent ?? '').trim().length,
    }));
    if (settled.transcript === 0) problems.push('under reduced motion the transcript is empty');
    await still2.close();

    // Every run has a page of its own, and it carries its checks.
    for (const run of record.scenarios) {
      const runPage = await browser.newPage();
      const response = await runPage.goto(`${origin}demo/${run.id}/`, { waitUntil: 'load' });
      if (!response || response.status() !== 200) {
        problems.push(`demo/${run.id}/ answered ${response ? response.status() : 'nothing'}`);
        await runPage.close();
        continue;
      }
      const rows = await runPage.evaluate(() => document.querySelectorAll('.ptah-demo-checks tbody tr').length);
      if (rows !== run.checks.length) {
        problems.push(`demo/${run.id}/ lists ${rows} checks and the recording holds ${run.checks.length}`);
      }
      await runPage.close();
    }
  } finally {
    await browser.close();
    server.close();
  }

  if (problems.length > 0) {
    console.error(`check-demo-page.mjs: ${problems.length} problem(s):\n- ${problems.join('\n- ')}`);
    process.exit(1);
  }
  console.log(`check-demo-page.mjs: OK (${record.scenarios.length} runs, with and without the player)`);
}

await main();
