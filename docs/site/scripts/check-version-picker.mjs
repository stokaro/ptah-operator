#!/usr/bin/env node
// Runs the documentation version picker in a browser against the built site.
//
// The picker is public/version-picker.js and public/version-picker.css, served
// at the site root and loaded by every documentation version from there, so a
// defect in it reaches every version at once. This check serves the built edge
// site under its base, the two picker files at the root as gen-versions.mjs
// publishes them, and a version index and other versions it makes up, then
// reads what a reader would get:
//
//   - the mount point replaced by a button named for the page's version, with
//     both files loaded from the root;
//   - the panel: the index's versions in order under their groups, the page's
//     own version marked, the latest release badged, each release's date;
//   - a choice that lands on the same page in the other version, and one that
//     lands on that version's home page because the page does not exist there;
//   - scripting disabled, where the mount point shows the version as text;
//   - the banner a page from an older release shows at the top of <main>:
//     present on such a page, linking to the same page in the latest release
//     or to its home page when the page does not exist there, and absent on
//     edge, on the latest release, on a release newer than it and on a page
//     with no index.
//
// The picker is a copy of the one Ptah's documentation serves, where the same
// behaviors and axe's WCAG rules are checked; this check proves the copy is
// wired into this site.
//
//   node scripts/check-version-picker.mjs [--dist <dir>] [--selftest]
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
const publicDir = join(siteRoot, 'public');

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

const PAGE = 'start/install/';
// Two releases that exist only in this check: the first has the page, the
// second has only a home page. The first is the latest.
const WITH_PAGE = 'v9.9.0';
const WITHOUT_PAGE = 'v9.8.0';
const RELEASED = { [WITH_PAGE]: '2030-02-01', [WITHOUT_PAGE]: '2030-01-01' };
const LISTED = ['edge', WITH_PAGE, WITHOUT_PAGE];
const GROUPS = ['In development', 'Releases'];
// Pages this check serves as other versions of the built page. OLDER is a
// release older than the latest that the index does not list; NEWER is newer
// than the latest, and compares as newer only by number, since "v10" sorts
// before "v9" as text.
const OLDER = 'v9.7.0';
const NEWER = 'v10.0.0';
const GONE = 'gone/';
const BANNER_TEXT = `This page documents ${OLDER}, an older release. `;
const BANNER_SAME_PAGE = {
  text: `${BANNER_TEXT}Read it in ${WITH_PAGE}, the latest release`,
  href: `/${WITH_PAGE}/${PAGE}`,
  first: true,
  ignored: true,
};
const BANNER_HOME = {
  text: `${BANNER_TEXT}Go to ${WITH_PAGE}, the latest release`,
  href: `/${WITH_PAGE}/`,
  first: true,
  ignored: true,
};

function detectBase(distRoot) {
  const home = join(distRoot, 'index.html');
  if (!existsSync(home)) return '';
  const match = readFileSync(home, 'utf8').match(/(?:href|src)="([^"]*)\/_astro\//);
  return match ? match[1] : '';
}

// startServer serves the build under its base, and whatever `route` answers
// first: the root picker files, the version index and the made-up versions.
function startServer(distRoot, base, route) {
  const server = createServer((request, response) => {
    let url = decodeURIComponent((request.url ?? '/').split('?')[0]);
    const routed = route(url);
    if (routed) {
      response.writeHead(routed.status, routed.type ? { 'content-type': routed.type } : {});
      response.end(request.method === 'HEAD' ? undefined : routed.body);
      return;
    }
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

// pickerProblems judges every reading one run takes.
export function pickerProblems(readings) {
  const problems = [];
  const expect = (label, got, want) => {
    if (JSON.stringify(got) !== JSON.stringify(want)) {
      problems.push(`${label}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
    }
  };
  expect('the mount point', readings.mount, {
    hydrated: true,
    label: `Documentation version: ${readings.version}`,
    scripts: ['/version-picker.js'],
    stylesheets: ['/version-picker.css'],
  });
  expect('the listed versions', readings.panel.slugs, LISTED);
  expect('the groups', readings.panel.groups, GROUPS);
  expect("the version marked as the page's own", readings.panel.current, [readings.version]);
  expect('the versions badged latest', readings.panel.latest, [WITH_PAGE]);
  expect('the release dates', readings.panel.dates, RELEASED);
  expect(`choosing ${WITH_PAGE} leads to`, readings.samePage, `/${WITH_PAGE}/${PAGE}`);
  expect(`choosing ${WITHOUT_PAGE} leads to`, readings.homePage, `/${WITHOUT_PAGE}/`);
  expect('the mount point without scripting', readings.noScript, readings.version);
  expect('the banner on an older release', readings.banner.older, BANNER_SAME_PAGE);
  expect('the banner on an older release whose page the latest lacks', readings.banner.gone, BANNER_HOME);
  expect('the banner on edge', readings.banner.edge, null);
  expect('the banner on the latest release', readings.banner.latest, null);
  expect('the banner on a release newer than the latest', readings.banner.newer, null);
  expect('the banner with no index', readings.banner.missing, null);
  expect('page errors', readings.errors, []);
  return problems;
}

function selftest() {
  const valid = {
    version: 'edge',
    mount: { hydrated: true, label: 'Documentation version: edge', scripts: ['/version-picker.js'], stylesheets: ['/version-picker.css'] },
    panel: { slugs: LISTED, groups: GROUPS, current: ['edge'], latest: [WITH_PAGE], dates: RELEASED },
    samePage: `/${WITH_PAGE}/${PAGE}`,
    homePage: `/${WITHOUT_PAGE}/`,
    noScript: 'edge',
    banner: { older: BANNER_SAME_PAGE, gone: BANNER_HOME, edge: null, latest: null, newer: null, missing: null },
    errors: [],
  };
  const accepted = pickerProblems(valid);
  if (accepted.length > 0) throw new Error(`a valid run was refused: ${accepted.join('; ')}`);
  const mutations = [
    ['a mount point the script never replaced', (r) => { r.mount.hydrated = false; }],
    ['the picker loaded from inside the version', (r) => { r.mount.scripts = ['/edge/version-picker.js']; }],
    ['no root stylesheet', (r) => { r.mount.stylesheets = []; }],
    ['a listed version missing from the panel', (r) => { r.panel.slugs = ['edge', WITH_PAGE]; }],
    ['the groups swapped', (r) => { r.panel.groups = [...GROUPS].reverse(); }],
    ["no version marked as the page's own", (r) => { r.panel.current = []; }],
    ['the latest badge on an older release', (r) => { r.panel.latest = [WITHOUT_PAGE]; }],
    ['a release date missing', (r) => { r.panel.dates = { [WITH_PAGE]: RELEASED[WITH_PAGE] }; }],
    ['a choice that lands on the home page although the page exists', (r) => { r.samePage = `/${WITH_PAGE}/`; }],
    ['a choice that lands on a missing page', (r) => { r.homePage = `/${WITHOUT_PAGE}/${PAGE}`; }],
    ['no text without scripting', (r) => { r.noScript = ''; }],
    ['no banner on an older release', (r) => { r.banner.older = null; }],
    ['a banner linking to a page the latest release lacks', (r) => { r.banner.gone = { ...BANNER_HOME, href: `/${WITH_PAGE}/${GONE}` }; }],
    ['a banner below the content rather than above it', (r) => { r.banner.older = { ...BANNER_SAME_PAGE, first: false }; }],
    ['a banner the search index would read', (r) => { r.banner.older = { ...BANNER_SAME_PAGE, ignored: false }; }],
    ['a banner on edge', (r) => { r.banner.edge = BANNER_SAME_PAGE; }],
    ['a banner on the latest release', (r) => { r.banner.latest = BANNER_SAME_PAGE; }],
    ['a banner on a release that compares older only as text', (r) => { r.banner.newer = BANNER_SAME_PAGE; }],
    ['a banner with no index to name the latest release', (r) => { r.banner.missing = BANNER_SAME_PAGE; }],
    ['a page error', (r) => { r.errors = ['boom']; }],
  ];
  for (const [label, mutate] of mutations) {
    const broken = structuredClone(valid);
    mutate(broken);
    if (pickerProblems(broken).length === 0) throw new Error(`${label} was accepted`);
  }
  console.log(`check-version-picker.mjs --selftest: OK (${mutations.length} refused readings)`);
}

const stub = (text) => ({ status: 200, body: `<!doctype html><title>${text}</title><p>${text}</p>`, type: 'text/html' });

function readMount(tab) {
  return tab.evaluate(() => {
    const mount = document.querySelector('header [data-ptah-version-picker]');
    const trigger = mount?.querySelector('button');
    const path = (url) => new URL(url, location.href).pathname;
    return {
      hydrated: Boolean(trigger),
      label: trigger ? trigger.textContent.replace(/[▾\s]+$/u, '').replace(/\s+/g, ' ').trim() : null,
      scripts: [...document.querySelectorAll('script[src*="version-picker"]')].map((node) => path(node.src)),
      stylesheets: [...document.querySelectorAll('link[href*="version-picker"]')].map((node) => path(node.href)),
    };
  });
}

function readPanel(tab) {
  return tab.evaluate(() => {
    const trigger = document.querySelector('header [data-ptah-version-picker] button');
    const panel = document.getElementById(trigger.getAttribute('aria-controls'));
    const rows = [...panel.querySelectorAll('.ptah-version-picker__item:not([hidden]) a')];
    return {
      slugs: rows.map((row) => row.dataset.version),
      groups: [...panel.querySelectorAll('.ptah-version-picker__group:not([hidden]) .ptah-version-picker__group-title')].map(
        (title) => title.textContent,
      ),
      current: rows.filter((row) => row.getAttribute('aria-current') === 'page').map((row) => row.dataset.version),
      latest: rows.filter((row) => row.querySelector('.ptah-version-picker__badge')).map((row) => row.dataset.version),
      dates: Object.fromEntries(
        rows.filter((row) => row.querySelector('time')).map((row) => [row.dataset.version, row.querySelector('time').dateTime]),
      ),
    };
  });
}

function readBanner(tab) {
  return tab.evaluate(() => {
    const banner = document.querySelector('.ptah-version-banner');
    if (!banner) return null;
    const link = banner.querySelector('a');
    return {
      text: banner.textContent,
      href: link ? new URL(link.href).pathname : null,
      first: banner.parentElement?.tagName === 'MAIN' && banner.parentElement.firstElementChild === banner,
      ignored: banner.hasAttribute('data-pagefind-ignore'),
    };
  });
}

async function main() {
  if (process.argv.includes('--selftest')) {
    selftest();
    return;
  }
  const distIndex = process.argv.indexOf('--dist');
  const distRoot = distIndex >= 0 ? process.argv[distIndex + 1] : join(siteRoot, 'dist');
  if (!existsSync(join(distRoot, PAGE, 'index.html'))) {
    console.error(`check-version-picker.mjs: dist/${PAGE} is missing; build the site first`);
    process.exit(1);
  }

  let chromium;
  try {
    ({ chromium } = await import('playwright'));
  } catch {
    const message =
      'check-version-picker.mjs: playwright is not installed (npm i -D playwright && npx playwright install chromium)';
    if (process.env.CI) {
      console.error(message);
      process.exit(1);
    }
    console.warn(`${message}; skipping`);
    return;
  }

  const base = detectBase(distRoot);
  const version = base.split('/').filter(Boolean).pop() ?? 'edge';
  const state = { index: undefined };
  // The built page as another version would serve it: the same bytes, with
  // the mount point naming that version.
  const pageAs = (slug) => ({
    status: 200,
    type: 'text/html',
    body: readFileSync(join(distRoot, PAGE, 'index.html'), 'utf8').replaceAll(`data-current="${version}"`, `data-current="${slug}"`),
  });
  const others = new Map([
    ['/version-picker.js', { status: 200, type: mimeTypes['.js'], body: readFileSync(join(publicDir, 'version-picker.js')) }],
    ['/version-picker.css', { status: 200, type: mimeTypes['.css'], body: readFileSync(join(publicDir, 'version-picker.css')) }],
    [`/${WITH_PAGE}/`, stub(WITH_PAGE)],
    [`/${WITH_PAGE}/${PAGE}`, pageAs(WITH_PAGE)],
    [`/${WITHOUT_PAGE}/`, stub(WITHOUT_PAGE)],
    [`/${OLDER}/${PAGE}`, pageAs(OLDER)],
    [`/${OLDER}/${GONE}`, pageAs(OLDER)],
    [`/${NEWER}/${PAGE}`, pageAs(NEWER)],
  ]);
  const route = (path) => {
    if (path === '/versions.json') {
      return state.index ? { status: 200, type: 'application/json', body: JSON.stringify(state.index) } : { status: 404, body: 'not found' };
    }
    return others.get(path);
  };
  const index = () => ({
    default: 'edge',
    latest: WITH_PAGE,
    versions: LISTED.map((slug) => (RELEASED[slug] ? { slug, label: slug, released: RELEASED[slug] } : { slug, label: slug })),
  });

  const { server, port } = await startServer(distRoot, base, route);
  const origin = `http://127.0.0.1:${port}`;
  const trigger = 'header [data-ptah-version-picker] button';
  const browser = await chromium.launch();
  try {
    const errors = [];
    const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
    const tab = await context.newPage();
    tab.on('pageerror', (error) => errors.push(error.message));
    // The script requests the index after it replaces the mount point, and the
    // banner waits for the latest release to answer, so a quiet network means
    // both have settled.
    const load = async (path, withIndex = true) => {
      state.index = withIndex ? index() : undefined;
      await tab.goto(`${origin}${path}`, { waitUntil: 'networkidle' });
    };
    const openPanel = async () => {
      await tab.click(trigger);
      await tab.locator('.ptah-version-picker__panel:not([hidden])').waitFor();
      return readPanel(tab);
    };
    const choose = async (target) => {
      await load(`${base}/${PAGE}`);
      await openPanel();
      await Promise.all([
        tab.waitForURL((address) => address.pathname.startsWith(`/${target}/`)),
        tab.click(`.ptah-version-picker__option[data-version="${target}"]`),
      ]);
      return new URL(tab.url()).pathname;
    };
    const bannerAt = async (path, withIndex = true) => {
      await load(path, withIndex);
      return readBanner(tab);
    };

    await load(`${base}/${PAGE}`);
    const mount = await readMount(tab);
    if (!mount.hydrated) {
      // Nothing below can be read without the button; say why instead of
      // waiting for a click to time out.
      console.error(`check-version-picker.mjs: the header of ${base}/${PAGE} has no picker button: ${JSON.stringify(mount)}`);
      process.exitCode = 1;
      return;
    }
    const panel = await openPanel();
    const samePage = await choose(WITH_PAGE);
    const homePage = await choose(WITHOUT_PAGE);
    const banner = {
      older: await bannerAt(`/${OLDER}/${PAGE}`),
      gone: await bannerAt(`/${OLDER}/${GONE}`),
      edge: await bannerAt(`${base}/${PAGE}`),
      latest: await bannerAt(`/${WITH_PAGE}/${PAGE}`),
      newer: await bannerAt(`/${NEWER}/${PAGE}`),
      missing: await bannerAt(`/${OLDER}/${PAGE}`, false),
    };

    const plain = await browser.newContext({ javaScriptEnabled: false });
    const quiet = await plain.newPage();
    await quiet.goto(`${origin}${base}/${PAGE}`, { waitUntil: 'load' });
    const noScript = await quiet.evaluate(() => document.querySelector('header [data-ptah-version-picker]')?.textContent.trim() ?? '');
    await plain.close();

    const problems = pickerProblems({ version, mount, panel, samePage, homePage, noScript, banner, errors });
    if (problems.length > 0) {
      console.error(`check-version-picker.mjs: ${problems.length} problem(s):\n- ${problems.join('\n- ')}`);
      process.exitCode = 1;
      return;
    }
  } finally {
    await browser.close();
    await new Promise((resolveClose) => server.close(resolveClose));
  }
  console.log(
    `check-version-picker.mjs: OK (${version}: root files, panel, groups, latest, dates, same page, home page, ` +
      'no scripting, older-release banner)',
  );
}

main().catch((error) => {
  console.error(`check-version-picker.mjs: FAILED: ${error instanceof Error ? error.message : error}`);
  process.exitCode = 1;
});
