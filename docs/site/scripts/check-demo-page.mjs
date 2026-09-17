#!/usr/bin/env node
// Drives the recorded-run pages in a browser.
//
// What it checks is what a reader would notice and a build log would not: the
// transcript is in the markup without the player, a tile is a link to the run's
// own page, the frame is the whole listing at rest and one fixed box from the
// first press onward, the frame fills the window when asked and gives it back,
// the controls are reachable from the keyboard, and a reader who asked for
// reduced motion is given the session at rest instead of a typewriter.
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

import { rank } from '../src/lib/run-order.mjs';

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
    tiles: document.querySelectorAll('.tiles a.tile').length,
    hrefs: Array.prototype.map.call(document.querySelectorAll('.tiles a.tile'), (tile) =>
      tile.getAttribute('href'),
    ),
    frames: document.querySelectorAll('[data-demo]').length,
    lines: document.querySelectorAll('[data-demo-transcript] .l').length,
  };
}

// The beat a pause is measured in: the last line of output before a note.
//
// The player gives a reader at least a second to read output before the next
// step is announced, and the note at the end of that beat is a visible change.
// A pause taken there and released is therefore something a clock can see.
export function pausePoint(events) {
  const output = new Set(['out', 'sql', 'new', 'err']);
  const skipped = new Set(['blank', 'sync', 'wait', 'mute']);
  for (let index = 1; index < events.length; index += 1) {
    if (events[index][0] !== 'note') continue;
    for (let back = index - 1; back >= 0; back -= 1) {
      const [kind, text] = events[back];
      if (skipped.has(kind)) continue;
      if (!output.has(kind)) break;
      return { after: String(text).trim(), then: String(events[index][1]).trim() };
    }
  }
  return null;
}

function selftest() {
  // The shape the page has to have, asserted against a stub rather than a
  // browser: a check whose only failure mode is "playwright is not installed"
  // would pass on a page that lost its tiles.
  const stub = {
    querySelectorAll(selector) {
      const table = {
        '.tiles a.tile': [
          { getAttribute: () => './first-apply/' },
          { getAttribute: () => './drift/' },
        ],
        '[data-demo]': [1],
        '[data-demo-transcript] .l': [1, 2, 3],
      };
      return table[selector] ?? [];
    },
  };
  const counts = countsIn(stub);
  if (counts.tiles !== 2 || counts.frames !== 1 || counts.lines !== 3) {
    throw new Error(`countsIn read ${JSON.stringify(counts)}`);
  }
  if (counts.hrefs.join(',') !== './first-apply/,./drift/') {
    throw new Error(`countsIn read the tiles' addresses as ${counts.hrefs.join(',')}`);
  }
  const beat = pausePoint([
    ['sync', 'ready'],
    ['note', '# first'],
    ['cmd', 'kubectl get'],
    ['out', 'NAME'],
    ['out', 'ptah-operator'],
    ['blank', ''],
    ['note', '# second'],
  ]);
  if (!beat || beat.after !== 'ptah-operator' || beat.then !== '# second') {
    throw new Error(`pausePoint read ${JSON.stringify(beat)}`);
  }
  // A run that never announces a step after output has no beat of this kind,
  // and saying so is better than pausing somewhere a clock cannot read.
  if (pausePoint([['note', '# only'], ['cmd', 'kubectl get'], ['out', 'NAME']]) !== null) {
    throw new Error('pausePoint invented a beat in a run that has none');
  }
  // A note straight after a command is not it: nothing was read in between.
  if (pausePoint([['cmd', 'kubectl get'], ['note', '# next']]) !== null) {
    throw new Error('pausePoint took a note that follows no output');
  }

  framingSelftest();
  expansionSelftest();

  console.log(
    'check-demo-page.mjs --selftest: OK (what a page has to carry, where a pause is measured, ' +
      'and what the frame owes its reader)',
  );
}

// The frame's contract, against readings rather than a browser. Each bad set
// below is one of the two ways the frame can let a reader down: a listing cut
// to a box at rest, and a box that moves once the run is under way.
function framingSelftest() {
  const listing = { client: 2838, scroll: 2838 };
  const box = (extra) => ({ width: 960, height: 421, page: 2575, transcript: listing, screen: 372, ...extra });
  const good = [
    { state: 'at rest', box: box({ height: 2887, page: 5041, screen: 0 }) },
    { state: 'playing', box: box() },
    { state: 'further into the run', box: box() },
    { state: 'paused', box: box() },
  ];
  const clean = framingProblems('first-apply', good, 372);
  if (clean.length !== 0) throw new Error(`framingProblems refused a frame that holds: ${clean.join('; ')}`);

  const clipped = structuredClone(good);
  clipped[0].box.transcript = { client: 372, scroll: 2838 };
  const cut = framingProblems('first-apply', clipped, 372);
  if (cut.length !== 1 || !cut[0].includes('372px of a 2838px transcript')) {
    throw new Error(`framingProblems read a clipped listing as ${JSON.stringify(cut)}`);
  }

  // A scrollbar's worth is not a clipped listing.
  const narrow = structuredClone(good);
  narrow[0].box.transcript = { client: 2823, scroll: 2838 };
  if (framingProblems('first-apply', narrow, 372).length !== 0) {
    throw new Error('framingProblems read a scrollbar as a clipped listing');
  }

  const grew = structuredClone(good);
  grew[3].box.height = 500;
  const moved = framingProblems('first-apply', grew, 372);
  if (moved.length !== 1 || !moved[0].includes('960x500 paused')) {
    throw new Error(`framingProblems read a frame that grew as ${JSON.stringify(moved)}`);
  }

  const reflowed = structuredClone(good);
  reflowed[2].box.page = 3000;
  const shifted = framingProblems('first-apply', reflowed, 372);
  if (shifted.length !== 1 || !shifted[0].includes('3000px further into the run')) {
    throw new Error(`framingProblems read a page that reflowed as ${JSON.stringify(shifted)}`);
  }

  const shrunk = structuredClone(good);
  shrunk[1].box.screen = 300;
  const wrong = framingProblems('first-apply', shrunk, 372);
  if (wrong.length !== 1 || !wrong[0].includes('300px playing')) {
    throw new Error(`framingProblems read a screen off its declared box as ${JSON.stringify(wrong)}`);
  }
}

// measurePause drives one pause over a beat and reports what it took to come
// back, or the problems it found on the way.
async function measurePause(browser, origin, beat) {
  const problems = [];
  const context = await browser.newContext();
  const page = await context.newPage();
  const screenLength = () =>
    page.evaluate(() => (document.querySelector('[data-demo-screen]')?.textContent ?? '').length);
  try {
    await page.goto(`${origin}demo/`, { waitUntil: 'load' });
    await page.waitForSelector('[data-demo-controls]:not([hidden])', { timeout: 10_000 });
    const toggle = page.locator('[data-demo-toggle]').first();
    await toggle.click();
    await page.waitForFunction(
      (needle) => (document.querySelector('[data-demo-screen]')?.textContent ?? '').includes(needle),
      beat.after.slice(0, 48),
      { timeout: 60_000, polling: 25 },
    );

    // Into the beat, rather than at its edge. What is in flight the moment the
    // last line of output lands is the short beat between two lines; the beat
    // this measures is the one after it, which the player gives a reader to
    // read what was printed before the next step is announced.
    let settled = await screenLength();
    for (let attempt = 0; attempt < 40; attempt += 1) {
      await page.waitForTimeout(120);
      const now = await screenLength();
      if (now === settled) break;
      settled = now;
    }

    await toggle.click();
    const held = await screenLength();
    // Longer than the beat it interrupted. A pause the beat outlives says
    // nothing: what the defect did was subtract the time spent paused from the
    // beat, so only a pause that outlasts it leaves nothing to come back to.
    await page.waitForTimeout(8000);
    if ((await screenLength()) !== held) problems.push('the player kept typing while it was paused');

    const resumedAt = Date.now();
    await toggle.click();
    await page.waitForFunction(
      (was) => (document.querySelector('[data-demo-screen]')?.textContent ?? '').length > was,
      held,
      { timeout: 20_000, polling: 20 },
    );
    const waited = Date.now() - resumedAt;
    // The beat this pause interrupted is at least a second long, and a pause
    // longer than the beat used to leave nothing of it: the screen moved the
    // instant the reader pressed Play.
    if (waited < 400) {
      problems.push(
        `resuming skipped the beat it interrupted: the screen moved ${waited}ms after Play, ` +
          'and the beat a pause taken there interrupts runs for at least a second',
      );
    }
  } finally {
    await context.close();
  }
  return problems;
}

// A platform that draws a classic scrollbar puts it inside the client box, so a
// transcript with one wide line measures a few tens of pixels short of itself.
// What this tolerance has to separate is that from a transcript held to the
// terminal's box, which is short by thousands.
const SCROLLBAR = 24;

// framingProblems reads the boxes one run's page had in each state and reports
// what breaks the frame's contract.
//
// At rest the frame is the listing: the whole transcript in the page, with
// nothing to scroll inside it. From the first press onward it is a terminal --
// the screen is the box the stylesheet declares, and the frame keeps that size
// while it types and while it is paused. Growing there is the defect a reader
// meets mid-run, and nothing in a build log can see it. The page's own scroll
// height is measured with it, because a frame that keeps its size while the
// page around it moves is the same complaint one step out.
export function framingProblems(runId, states, declaredScreen) {
  const problems = [];
  const rest = states.find((one) => one.state === 'at rest');
  if (!rest?.box) return [`demo/${runId}/ carries no frame`];

  const shown = rest.box.transcript;
  if (shown.client === 0) {
    problems.push(`demo/${runId}/ shows none of its transcript at rest`);
  } else if (shown.scroll - shown.client > SCROLLBAR) {
    problems.push(
      `demo/${runId}/ shows ${shown.client}px of a ${shown.scroll}px transcript at rest, ` +
        'so a reader who came to read is given a box to scroll inside a page that scrolls',
    );
  }

  const running = states.filter((one) => one.state !== 'at rest');
  const first = running.find((one) => one.box);
  if (!first) return [...problems, `demo/${runId}/ lost its frame once the player took over`];
  for (const { state, box } of running) {
    if (!box) {
      problems.push(`demo/${runId}/ lost its frame ${state}`);
      continue;
    }
    if (box.screen !== declaredScreen) {
      problems.push(
        `demo/${runId}/ has a screen of ${box.screen}px ${state} and the stylesheet declares ` +
          `${declaredScreen}px`,
      );
    }
    if (box.width !== first.box.width || box.height !== first.box.height) {
      problems.push(
        `demo/${runId}/ has a frame of ${first.box.width}x${first.box.height} ${first.state} and ` +
          `${box.width}x${box.height} ${state}`,
      );
    }
    if (box.page !== first.box.page) {
      problems.push(
        `demo/${runId}/ is ${first.box.page}px tall ${first.state} and ${box.page}px ${state}`,
      );
    }
  }
  return problems;
}

// What the frame owes a reader who asked it to fill the window.
//
// The control has to be there before anything has played, because the reader
// who came to read is the one who wants the session bigger. While the frame
// holds the window it is the window, the page behind it stops scrolling, the
// session is what scrolls, and Tab stays inside a frame that covers the page
// and tells a screen reader the page is not there. The press gives all of it
// back, including the reader's place in the page and their focus.
export function expansionProblems(runId, readings) {
  const problems = [];
  const { rest, expanded, collapsed, tabbedOut, keptPlaying } = readings;

  if (!rest.reachable) {
    problems.push(`demo/${runId}/ hides the expand control until something has played`);
  }

  if (!expanded.filling) {
    problems.push(`demo/${runId}/ did not take the window when the control was pressed`);
  } else {
    if (expanded.width !== expanded.viewportWidth || expanded.height !== expanded.viewportHeight) {
      problems.push(
        `demo/${runId}/ covers ${expanded.width}x${expanded.height} of a ` +
          `${expanded.viewportWidth}x${expanded.viewportHeight} window`,
      );
    }
    if (!expanded.pageHeld) {
      problems.push(`demo/${runId}/ leaves the page behind scrolling while it covers it`);
    }
    if (!expanded.sessionScrolls) {
      problems.push(`demo/${runId}/ takes the window with a session a reader cannot scroll`);
    }
    if (expanded.pressed !== 'true') {
      problems.push(
        `demo/${runId}/ reports aria-pressed=${expanded.pressed} while it holds the window`,
      );
    }
  }

  if (tabbedOut) {
    problems.push(`demo/${runId}/ lets Tab reach the page behind the frame that covers it`);
  }
  if (collapsed.filling) {
    problems.push(`demo/${runId}/ kept the window after Escape`);
  }
  if (!collapsed.pageRestored) {
    problems.push(`demo/${runId}/ left the page unable to scroll after Escape`);
  }
  if (!collapsed.focusReturned) {
    problems.push(`demo/${runId}/ dropped focus on its way back into the page`);
  }
  if (!keptPlaying) {
    problems.push(`demo/${runId}/ stopped the run when the frame took the window`);
  }
  return problems;
}

// The expansion's contract, against readings rather than a browser. Each bad
// set is one promise withdrawn, so a rule that stopped firing is named by the
// case it was written for.
function expansionSelftest() {
  const good = {
    rest: { reachable: true },
    expanded: {
      filling: true,
      width: 1280,
      height: 800,
      viewportWidth: 1280,
      viewportHeight: 800,
      pageHeld: true,
      sessionScrolls: true,
      pressed: 'true',
    },
    collapsed: { filling: false, pageRestored: true, focusReturned: true },
    tabbedOut: false,
    keptPlaying: true,
  };
  const clean = expansionProblems('first-apply', good);
  if (clean.length !== 0) {
    throw new Error(`expansionProblems refused an expansion that holds: ${clean.join('; ')}`);
  }

  const cases = [
    ['rest.reachable', (one) => { one.rest.reachable = false; }, 'hides the expand control'],
    ['expanded.filling', (one) => { one.expanded.filling = false; }, 'did not take the window'],
    ['expanded size', (one) => { one.expanded.height = 421; }, '1280x421 of a 1280x800 window'],
    ['expanded.pageHeld', (one) => { one.expanded.pageHeld = false; }, 'leaves the page behind scrolling'],
    ['expanded.sessionScrolls', (one) => { one.expanded.sessionScrolls = false; }, 'cannot scroll'],
    ['expanded.pressed', (one) => { one.expanded.pressed = 'false'; }, 'aria-pressed=false'],
    ['tabbedOut', (one) => { one.tabbedOut = true; }, 'lets Tab reach the page behind'],
    ['collapsed.filling', (one) => { one.collapsed.filling = true; }, 'kept the window after Escape'],
    ['collapsed.pageRestored', (one) => { one.collapsed.pageRestored = false; }, 'unable to scroll after Escape'],
    ['collapsed.focusReturned', (one) => { one.collapsed.focusReturned = false; }, 'dropped focus'],
    ['keptPlaying', (one) => { one.keptPlaying = false; }, 'stopped the run'],
  ];
  for (const [name, spoil, want] of cases) {
    const bad = structuredClone(good);
    spoil(bad);
    const found = expansionProblems('first-apply', bad);
    if (found.length !== 1 || !found[0].includes(want)) {
      throw new Error(`expansionProblems read a withdrawn ${name} as ${JSON.stringify(found)}`);
    }
  }
}

// measureExpansion drives the expand control on one run's own page: at rest,
// filling the window, tabbing inside it, and back out through Escape.
async function measureExpansion(browser, origin, runId) {
  const context = await browser.newContext();
  const page = await context.newPage();
  const failures = [];
  page.on('pageerror', (error) => failures.push(String(error)));
  // SCROLLBAR is passed in rather than closed over: the function runs in the
  // page, where this file's constants do not exist.
  const stateOf = () =>
    page.evaluate((slack) => {
      const frame = document.querySelector('[data-demo]');
      const session = document.querySelector('[data-demo-transcript]');
      const rect = frame.getBoundingClientRect();
      const control = document.querySelector('[data-demo-full]');
      return {
        filling: frame.classList.contains('is-expanded'),
        width: Math.round(rect.width),
        height: Math.round(rect.height),
        viewportWidth: window.innerWidth,
        viewportHeight: window.innerHeight,
        pageHeld: document.documentElement.style.overflow === 'hidden',
        pageRestored: document.documentElement.style.overflow !== 'hidden',
        sessionScrolls: session ? session.scrollHeight > session.clientHeight + slack : false,
        pressed: control ? control.getAttribute('aria-pressed') : null,
        focusReturned: document.activeElement !== document.body,
      };
    }, SCROLLBAR);
  try {
    await page.goto(`${origin}demo/${runId}/`, { waitUntil: 'load' });
    // The control before anything has played. A frame whose bar waits for a
    // press offers no way to make the session bigger to the reader who never
    // presses one.
    const control = page.locator('[data-demo-full]').first();
    const rest = { reachable: await control.isVisible() };

    // The run goes first, so the same press has to leave a typewriter running.
    await page.locator('[data-demo-toggle]').first().click();
    await page.waitForTimeout(1200);
    const typedBefore = await page.evaluate(
      () => document.querySelector('[data-demo-screen]').textContent.length,
    );
    await control.click();
    await page.waitForTimeout(500);
    const expanded = await stateOf();
    await page.waitForTimeout(1500);
    const typedAfter = await page.evaluate(
      () => document.querySelector('[data-demo-screen]').textContent.length,
    );

    // Tab all the way round the bar and one step past it. A ring of four
    // controls walked six times is the wrap, not a lucky stop.
    let tabbedOut = false;
    for (let press = 0; press < 6; press += 1) {
      await page.keyboard.press('Tab');
      const inside = await page.evaluate(() =>
        document.querySelector('[data-demo]').contains(document.activeElement),
      );
      if (!inside) tabbedOut = true;
    }

    await page.keyboard.press('Escape');
    await page.waitForTimeout(600);
    const collapsed = await stateOf();

    const problems = expansionProblems(runId, {
      rest,
      expanded,
      collapsed,
      tabbedOut,
      keptPlaying: typedAfter > typedBefore,
    });
    if (failures.length > 0) problems.push(`the player threw: ${failures.join('; ')}`);
    return problems;
  } finally {
    await context.close();
  }
}

// measureFrame drives one run's own page through rest, playing and paused.
async function measureFrame(browser, origin, runId) {
  const context = await browser.newContext();
  const page = await context.newPage();
  const boxOf = () =>
    page.evaluate(() => {
      const frame = document.querySelector('[data-demo]');
      if (!frame) return null;
      const rect = frame.getBoundingClientRect();
      const transcript = document.querySelector('[data-demo-transcript]');
      const screen = document.querySelector('[data-demo-screen]');
      return {
        width: Math.round(rect.width),
        height: Math.round(rect.height),
        page: document.documentElement.scrollHeight,
        transcript: {
          client: transcript ? transcript.clientHeight : 0,
          scroll: transcript ? transcript.scrollHeight : 0,
        },
        // The border box, not the client box: a classic scrollbar is drawn
        // inside the client box and would read as a screen short of its
        // declared height on the platforms that draw one.
        screen: screen ? Math.round(screen.getBoundingClientRect().height) : 0,
      };
    });
  try {
    await page.goto(`${origin}demo/${runId}/`, { waitUntil: 'load' });
    await page.waitForSelector('[data-demo-controls]:not([hidden])', { timeout: 10_000 });
    // Read off the frame rather than written here, so this measures the box the
    // stylesheet declares at this width and not a figure that was true once.
    const declared = await page.evaluate(() =>
      Math.round(
        parseFloat(
          getComputedStyle(document.querySelector('[data-demo]')).getPropertyValue(
            '--demo-screen-height',
          ),
        ),
      ),
    );
    const states = [{ state: 'at rest', box: await boxOf() }];
    const toggle = page.locator('[data-demo-toggle]').first();
    await toggle.click();
    await page.waitForTimeout(2500);
    states.push({ state: 'playing', box: await boxOf() });
    await page.waitForTimeout(5000);
    states.push({ state: 'further into the run', box: await boxOf() });
    await toggle.click();
    await page.waitForTimeout(400);
    states.push({ state: 'paused', box: await boxOf() });
    return framingProblems(runId, states, declared);
  } finally {
    await context.close();
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
  // The same order the page puts them in, read from the same module, so this
  // check cannot be measuring a different first run from the one on the page.
  const ordered = [...record.scenarios].sort((left, right) => rank(left.tags?.[0]) - rank(right.tags?.[0]));
  const base = detectBase(distRoot);
  const { server, port } = await startServer(distRoot, base);
  // The trailing slash is the site's own: `base` is read off a built asset's
  // address and comes back without one, and an origin that ends inside the
  // version segment turns every relative link on the page into a different
  // address from the one a reader would follow.
  const origin = `http://127.0.0.1:${port}${base}/`;
  const browser = await chromium.launch();
  const problems = [];

  try {
    // Without JavaScript the catalog is still a catalog: every tile is a link
    // to the run's own page, which carries that session in full. This is what a
    // crawler, a screen reader and a reader on a slow connection get.
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
    // A tile is a link, and the address it carries is the run's own page. A
    // tile that opened something with a script behind it would leave a reader
    // without one holding a card that does nothing.
    const addressed = new Set(withoutScript.hrefs);
    for (const run of record.scenarios) {
      if (!addressed.has(`./${run.id}/`)) {
        problems.push(`no tile links to ./${run.id}/ (the catalog offers ${withoutScript.hrefs.join(', ')})`);
      }
    }
    // The frame carries the first run in full. Counted from the recording
    // rather than against a figure written here, which would pass whatever the
    // page happened to render.
    const firstRunLines = ordered[0].events.filter(([kind]) => kind !== 'sync' && kind !== 'wait').length;
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

    // A tile opens the run's own page, and the frame there is that run. With one
    // run the assertion would pass by comparing a value with itself.
    const second = ordered[1];
    if (!second) {
      console.warn('check-demo-page.mjs: one run recorded, so opening a tile was not measured');
    }
    if (second) {
      await page.locator(`.tiles a.tile[href="./${second.id}/"]`).first().click();
      await page.waitForURL(`**/demo/${second.id}/`, { timeout: 10_000 }).catch(() => {
        problems.push(`a tile for ${second.id} left the reader on ${page.url()}`);
      });
      const showing = await page.evaluate(() =>
        document.querySelector('[data-demo]')?.getAttribute('data-demo-scenario'),
      );
      if (showing !== second.id) problems.push(`demo/${second.id}/ shows the frame on ${showing}`);
    }
    if (failures.length > 0) problems.push(`the player threw: ${failures.join('; ')}`);
    await live.close();

    // Pausing keeps the beat it interrupted, and resuming waits it out.
    const beat = pausePoint(ordered[0].events);
    if (!beat) {
      console.warn('check-demo-page.mjs: the first run announces no step after output, so pausing was not measured');
    } else {
      problems.push(...(await measurePause(browser, origin, beat)));
    }

    // The frame is one box: the same width and the same height at rest, while
    // it types, and while it is paused.
    problems.push(...(await measureFrame(browser, origin, ordered[0].id)));
    problems.push(...(await measureExpansion(browser, origin, ordered[0].id)));

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
