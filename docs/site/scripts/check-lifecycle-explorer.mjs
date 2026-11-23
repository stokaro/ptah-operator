#!/usr/bin/env node
// Drives the lifecycle explorer on the Reconciliation page in a browser.
//
// The build proves the component compiled, and the link checker proves its
// links resolve. Neither notices an explorer that never became interactive,
// a stage only a mouse can reach, an object whose relationships the view
// leaves out, a text version that lost a stage, or a layout that scrolls
// sideways on a phone. This reads src/lifecycle-explorer.mjs for what the page
// owes and holds the built page to it:
//
//   - with JavaScript the interactive view replaces the text, with one panel
//     per stage;
//   - the arrow keys, Home and End reach every stage in order from the
//     keyboard, Tab reaches the stage list from the page, and Next walks
//     forward;
//   - selecting each object shows its own panel, lists every relationship the
//     model gives it from both ends, and marks exactly the related objects;
//   - a link carrying a stage and an object restores both;
//   - without JavaScript the text carries every stage, every command and every
//     object;
//   - at 390px wide neither the page nor the explorer scrolls sideways.
//
//   node scripts/check-lifecycle-explorer.mjs [--dist <dir>] [--selftest]
//
// Requires Playwright's chromium. Without it this skips and says what to
// install; with CI=1 it fails instead, because a green check that measured
// nothing is worse than a red one.

import { createServer } from 'node:http';
import { existsSync, readFileSync, statSync } from 'node:fs';
import { dirname, extname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { objects, relations, scenario } from '../src/lifecycle-explorer.mjs';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const siteRoot = join(scriptDir, '..');
const PAGE = 'reference/reconciliation/';
const NARROW = { width: 390, height: 844 };

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

// What the model says about one object: every other object it is related to,
// and every relationship from either end as "type:other". Computed from the
// model rather than written down, so a relation added there is one this check
// expects to see.
export function expectedRelations(id, model) {
  const listed = model.relations
    .filter((relation) => relation.from === id || relation.to === id)
    .map((relation) => `${relation.type}:${relation.from === id ? relation.to : relation.from}`);
  const related = [...new Set(listed.map((entry) => entry.split(':')[1]))];
  return { listed: listed.sort(), related: related.sort() };
}

// modelProblems refuses a model the page could not render honestly: a stage
// naming an object that does not exist, a relation to nowhere, an id used
// twice. A model with no stage or no object measures nothing.
export function modelProblems(model) {
  const problems = [];
  const objectIds = model.objects.map((object) => object.id);
  const stageIds = model.stages.map((stage) => stage.id);
  if (stageIds.length === 0) problems.push('the scenario has no stages');
  if (objectIds.length === 0) problems.push('the model has no objects');
  for (const [what, ids] of [
    ['stage', stageIds],
    ['object', objectIds],
  ]) {
    const seen = new Set();
    for (const id of ids) {
      if (seen.has(id)) problems.push(`the ${what} id "${id}" is used twice`);
      seen.add(id);
    }
  }
  for (const stage of model.stages) {
    for (const id of stage.objects) {
      if (!objectIds.includes(id)) problems.push(`stage "${stage.id}" names the object "${id}", which does not exist`);
    }
    if (!stage.actors[stage.lead]) problems.push(`stage "${stage.id}" is led by "${stage.lead}", who does nothing in it`);
    if (stage.inspect.length === 0) problems.push(`stage "${stage.id}" says no way to inspect it`);
  }
  for (const relation of model.relations) {
    for (const end of [relation.from, relation.to]) {
      if (!objectIds.includes(end)) problems.push(`a ${relation.type} relation names "${end}", which does not exist`);
    }
  }
  for (const id of objectIds) {
    if (expectedRelations(id, model).listed.length === 0) problems.push(`the object "${id}" is related to nothing`);
  }
  return problems;
}

// renderProblems judges the page as JavaScript left it.
export function renderProblems(reading, model) {
  const problems = [];
  if (!reading.present) return ['the page carries no lifecycle explorer'];
  if (!reading.ready) problems.push('the explorer’s script never ran: the interactive view did not replace the text');
  if (reading.appHidden) problems.push('the interactive view is still hidden');
  if (reading.textShown) problems.push('the text version is still on screen beside the interactive view');
  if (reading.panels !== model.stages.length) {
    problems.push(`the interactive view has ${reading.panels} stage panels and the scenario ${model.stages.length} stages`);
  }
  if (reading.visiblePanels !== 1) problems.push(`${reading.visiblePanels} stage panels are shown at once`);
  return problems;
}

// keyboardProblems judges where the keys went. `arrows` is the stage shown
// after each ArrowRight from the first, `focus` where focus was then; `home`
// and `end` the stage each key reached; `tabbed` whether Tab from the top of
// the page reached the stage list; `next` the stages Next walked.
export function keyboardProblems(reading, model) {
  const problems = [];
  const ids = model.stages.map((stage) => stage.id);
  const walked = [reading.start, ...reading.arrows];
  const missing = ids.filter((id) => !walked.includes(id));
  if (missing.length > 0) problems.push(`the arrow keys never reach ${missing.join(', ')}`);
  if (walked.join(' ') !== ids.join(' ') && missing.length === 0) {
    problems.push(`the arrow keys walk the stages as ${walked.join(', ')}, not in order`);
  }
  const lost = reading.focus.filter((id, index) => id !== reading.arrows[index]);
  if (lost.length > 0) problems.push('after an arrow key, focus is not on the stage it selected');
  if (reading.end !== ids.at(-1)) problems.push(`End reaches ${reading.end}, not the last stage`);
  if (reading.home !== ids[0]) problems.push(`Home reaches ${reading.home}, not the first stage`);
  if (!reading.tabbed) problems.push('Tab from the top of the page never reaches the stage list');
  if (reading.next.join(' ') !== ids.slice(1).join(' ')) {
    problems.push(`Next walks ${reading.next.join(', ') || 'nowhere'}, not every stage after the first`);
  }
  return problems;
}

// selectionProblems judges what selecting each object showed.
export function selectionProblems(readings, model) {
  const problems = [];
  if (readings.length !== model.objects.length) {
    problems.push(`${readings.length} of ${model.objects.length} objects were selected`);
  }
  for (const reading of readings) {
    const want = expectedRelations(reading.id, model);
    if (reading.panel !== reading.id) {
      problems.push(`selecting ${reading.id} shows the panel of ${reading.panel ?? 'nothing'}`);
      continue;
    }
    const listed = [...reading.listed].sort();
    const missing = want.listed.filter((entry) => !listed.includes(entry));
    const extra = listed.filter((entry) => !want.listed.includes(entry));
    if (missing.length > 0) problems.push(`the panel of ${reading.id} leaves out ${missing.join(', ')}`);
    if (extra.length > 0) problems.push(`the panel of ${reading.id} also lists ${extra.join(', ')}`);
    const marked = [...reading.marked].sort();
    if (marked.join(' ') !== want.related.join(' ')) {
      problems.push(
        `selecting ${reading.id} marks ${marked.join(', ') || 'nothing'} as related, and the model relates ` +
          want.related.join(', '),
      );
    }
    if (!reading.pressed) problems.push(`the node of ${reading.id} does not say it is selected`);
  }
  return problems;
}

// textProblems judges the page without JavaScript: every stage with its title
// and commands, and every object, in the text a reader, a crawler or a printer
// gets.
export function textProblems(reading, model) {
  const problems = [];
  if (!reading.present) return ['without JavaScript the page carries no lifecycle explorer'];
  if (!reading.appHidden) problems.push('without JavaScript the interactive view is on screen, with no script to drive it');
  if (!reading.textShown) problems.push('without JavaScript the text version is not on screen');
  const stages = new Map(reading.stages.map((stage) => [stage.id, stage.text]));
  for (const stage of model.stages) {
    const text = stages.get(stage.id);
    if (text === undefined) {
      problems.push(`without JavaScript the text leaves out the stage "${stage.id}"`);
      continue;
    }
    if (!text.includes(stage.title)) problems.push(`without JavaScript the stage "${stage.id}" lacks its title`);
    for (const step of stage.inspect) {
      if (!text.includes(step.command)) {
        problems.push(`without JavaScript the stage "${stage.id}" lacks the command ${JSON.stringify(step.command)}`);
      }
    }
  }
  const order = reading.stages.map((stage) => stage.id).filter((id) => stages.has(id));
  const wantOrder = model.stages.map((stage) => stage.id).filter((id) => stages.has(id));
  if (order.join(' ') !== wantOrder.join(' ')) problems.push('without JavaScript the stages are out of order');
  for (const object of model.objects) {
    if (!reading.objects.includes(object.id)) problems.push(`without JavaScript the text leaves out the object "${object.id}"`);
  }
  return problems;
}

// overflowProblems judges each narrow-screen reading: the page and the
// explorer have to fit the viewport they were measured in.
export function overflowProblems(readings) {
  const problems = [];
  if (readings.length === 0) return ['no narrow-screen reading was taken'];
  for (const reading of readings) {
    if (reading.page > reading.viewport) {
      problems.push(`${reading.what}: the page is ${reading.page}px wide in a ${reading.viewport}px viewport`);
    }
    if (reading.explorer > reading.explorerBox) {
      problems.push(`${reading.what}: the explorer scrolls, ${reading.explorer}px of content in ${reading.explorerBox}px`);
    }
  }
  return problems;
}

// deepLinkProblems judges a page opened from a link that names a state.
export function deepLinkProblems(reading, want) {
  const problems = [];
  if (reading.stage !== want.stage) problems.push(`a link naming the stage ${want.stage} opens ${reading.stage}`);
  if (reading.object !== want.object) problems.push(`a link naming the object ${want.object} selects ${reading.object ?? 'nothing'}`);
  return problems;
}

function selftest() {
  const model = {
    stages: [
      { id: 'one', title: 'First', lead: 'you', actors: { you: 'act' }, objects: ['a'], inspect: [{ command: 'cmd one' }] },
      { id: 'two', title: 'Second', lead: 'operator', actors: { operator: 'act' }, objects: ['a', 'b'], inspect: [{ command: 'cmd two' }] },
      { id: 'three', title: 'Third', lead: 'you', actors: { you: 'act' }, objects: ['c'], inspect: [{ command: 'cmd three' }] },
    ],
    objects: [{ id: 'a' }, { id: 'b' }, { id: 'c' }],
    relations: [
      { from: 'a', to: 'b', type: 'owns' },
      { from: 'a', to: 'b', type: 'references' },
      { from: 'c', to: 'a', type: 'references' },
    ],
  };
  const fail = (what, found) => {
    throw new Error(`${what}: ${JSON.stringify(found)}`);
  };
  const refuses = (name, found, want) => {
    if (!found.some((problem) => problem.includes(want))) fail(`a check read ${name} as`, found);
  };

  const a = expectedRelations('a', model);
  if (a.listed.join(' ') !== 'owns:b references:b references:c' || a.related.join(' ') !== 'b c') {
    fail('expectedRelations read a as', a);
  }

  if (modelProblems(model).length !== 0) fail('modelProblems refused a sound model', modelProblems(model));
  refuses('a stage naming a missing object', modelProblems({ ...model, stages: [{ ...model.stages[0], objects: ['z'] }] }), 'names the object "z"');
  refuses('a relation to nowhere', modelProblems({ ...model, relations: [...model.relations, { from: 'a', to: 'z', type: 'owns' }] }), 'names "z"');
  refuses('a stage id used twice', modelProblems({ ...model, stages: [model.stages[0], model.stages[0]] }), 'used twice');
  refuses('a lead who does nothing', modelProblems({ ...model, stages: [{ ...model.stages[0], lead: 'executor' }] }), 'who does nothing');
  refuses('an object related to nothing', modelProblems({ ...model, objects: [...model.objects, { id: 'd' }] }), '"d" is related to nothing');
  refuses('an empty scenario', modelProblems({ ...model, stages: [] }), 'no stages');

  const goodRender = { present: true, ready: true, appHidden: false, textShown: false, panels: 3, visiblePanels: 1 };
  if (renderProblems(goodRender, model).length !== 0) fail('renderProblems refused a page that holds', renderProblems(goodRender, model));
  for (const [name, spoil, want] of [
    ['a page with no explorer', { present: false }, 'carries no lifecycle explorer'],
    ['a script that never ran', { ready: false }, 'never ran'],
    ['an interactive view left hidden', { appHidden: true }, 'still hidden'],
    ['the text left beside it', { textShown: true }, 'still on screen'],
    ['a stage panel missing', { panels: 2 }, '2 stage panels'],
    ['two panels shown at once', { visiblePanels: 2 }, '2 stage panels are shown'],
  ]) {
    refuses(name, renderProblems({ ...goodRender, ...spoil }, model), want);
  }

  const goodKeys = { start: 'one', arrows: ['two', 'three'], focus: ['two', 'three'], home: 'one', end: 'three', tabbed: true, next: ['two', 'three'] };
  if (keyboardProblems(goodKeys, model).length !== 0) fail('keyboardProblems refused keys that hold', keyboardProblems(goodKeys, model));
  for (const [name, spoil, want] of [
    ['arrows that stop short', { arrows: ['two', 'two'], focus: ['two', 'two'] }, 'never reach three'],
    ['arrows out of order', { arrows: ['three', 'two'], focus: ['three', 'two'] }, 'not in order'],
    ['focus left behind', { focus: ['one', 'one'] }, 'focus is not on the stage'],
    ['End that misses', { end: 'two' }, 'End reaches two'],
    ['Home that misses', { home: 'two' }, 'Home reaches two'],
    ['a stage list Tab skips', { tabbed: false }, 'Tab from the top'],
    ['Next that does nothing', { next: [] }, 'Next walks nowhere'],
  ]) {
    refuses(name, keyboardProblems({ ...goodKeys, ...spoil }, model), want);
  }

  const goodSelection = [
    { id: 'a', panel: 'a', listed: ['owns:b', 'references:b', 'references:c'], marked: ['b', 'c'], pressed: true },
    { id: 'b', panel: 'b', listed: ['owns:a', 'references:a'], marked: ['a'], pressed: true },
    { id: 'c', panel: 'c', listed: ['references:a'], marked: ['a'], pressed: true },
  ];
  if (selectionProblems(goodSelection, model).length !== 0) {
    fail('selectionProblems refused a selection that holds', selectionProblems(goodSelection, model));
  }
  for (const [name, index, spoil, want] of [
    ['a panel of another object', 0, { panel: 'b' }, 'shows the panel of b'],
    ['no panel at all', 0, { panel: null }, 'shows the panel of nothing'],
    ['a relation read from one end only', 1, { listed: ['owns:a'] }, 'leaves out references:a'],
    ['a relation the model does not have', 2, { listed: ['references:a', 'owns:b'] }, 'also lists owns:b'],
    ['a related object left unmarked', 0, { marked: ['b'] }, 'marks b as related'],
    ['an unrelated object marked', 2, { marked: ['a', 'b'] }, 'marks a, b as related'],
    ['a selection the node does not show', 0, { pressed: false }, 'does not say it is selected'],
  ]) {
    const bad = structuredClone(goodSelection);
    Object.assign(bad[index], spoil);
    refuses(name, selectionProblems(bad, model), want);
  }
  refuses('objects never selected', selectionProblems(goodSelection.slice(0, 1), model), '1 of 3 objects');

  const goodText = {
    present: true,
    appHidden: true,
    textShown: true,
    stages: [
      { id: 'one', text: 'First. cmd one' },
      { id: 'two', text: 'Second. cmd two' },
      { id: 'three', text: 'Third. cmd three' },
    ],
    objects: ['a', 'b', 'c'],
  };
  if (textProblems(goodText, model).length !== 0) fail('textProblems refused a text that holds', textProblems(goodText, model));
  for (const [name, spoil, want] of [
    ['a page with no explorer', { present: false }, 'carries no lifecycle explorer'],
    ['an interactive view shown without its script', { appHidden: false }, 'with no script to drive it'],
    ['a text hidden without a script', { textShown: false }, 'text version is not on screen'],
    ['a stage dropped', { stages: goodText.stages.slice(0, 2) }, 'leaves out the stage "three"'],
    ['a stage without its title', { stages: [{ id: 'one', text: 'cmd one' }, ...goodText.stages.slice(1)] }, 'lacks its title'],
    ['a stage without its command', { stages: [{ id: 'one', text: 'First.' }, ...goodText.stages.slice(1)] }, 'lacks the command "cmd one"'],
    ['stages out of order', { stages: [goodText.stages[1], goodText.stages[0], goodText.stages[2]] }, 'out of order'],
    ['an object dropped', { objects: ['a', 'b'] }, 'leaves out the object "c"'],
  ]) {
    refuses(name, textProblems({ ...goodText, ...spoil }, model), want);
  }

  const fits = { what: 'at rest', viewport: 390, page: 390, explorer: 340, explorerBox: 340 };
  if (overflowProblems([fits]).length !== 0) fail('overflowProblems refused a page that fits', overflowProblems([fits]));
  refuses('a page wider than the viewport', overflowProblems([{ ...fits, page: 520 }]), 'the page is 520px wide');
  refuses('an explorer that scrolls', overflowProblems([{ ...fits, explorer: 410 }]), 'the explorer scrolls');
  refuses('no reading at all', overflowProblems([]), 'no narrow-screen reading');

  const want = { stage: 'two', object: 'c' };
  if (deepLinkProblems(want, want).length !== 0) fail('deepLinkProblems refused a link that holds', deepLinkProblems(want, want));
  refuses('a link whose stage was dropped', deepLinkProblems({ stage: 'one', object: 'c' }, want), 'opens one');
  refuses('a link whose object was dropped', deepLinkProblems({ stage: 'two', object: null }, want), 'selects nothing');

  console.log(
    'check-lifecycle-explorer.mjs --selftest: OK (the model, the rendered view, the keyboard, each ' +
      'selection, the text without JavaScript, a shared link and the narrow layout)',
  );
}

const readRender = (page) =>
  page.evaluate(() => {
    const root = document.getElementById('lifecycle-explorer');
    if (!root) return { present: false };
    const visible = (element) => element && element.getClientRects().length > 0 && getComputedStyle(element).display !== 'none';
    const panels = [...root.querySelectorAll('[data-lx-stage-panel]')];
    return {
      present: true,
      ready: root.hasAttribute('data-lx-ready'),
      appHidden: !visible(root.querySelector('[data-lx-app]')),
      textShown: visible(root.querySelector('[data-lx-text]')),
      panels: panels.length,
      visiblePanels: panels.filter((panel) => visible(panel)).length,
    };
  });

const readStage = (page) =>
  page.evaluate(() => ({
    stage: document.querySelector('#lifecycle-explorer [data-lx-step][aria-current="step"]')?.dataset.lxStep ?? null,
    shown:
      [...document.querySelectorAll('#lifecycle-explorer [data-lx-stage-panel]')].find((panel) => !panel.hidden)?.dataset
        .lxStagePanel ?? null,
    focus: document.activeElement?.dataset?.lxStep ?? null,
    object: document.querySelector('#lifecycle-explorer [data-lx-object][aria-pressed="true"]')?.dataset.lxObject ?? null,
  }));

const readOverflow = (page, what) =>
  page.evaluate((label) => {
    const root = document.getElementById('lifecycle-explorer');
    return {
      what: label,
      viewport: document.documentElement.clientWidth,
      page: document.documentElement.scrollWidth,
      explorer: root?.scrollWidth ?? 0,
      explorerBox: root?.clientWidth ?? 0,
    };
  }, what);

async function open(page, url) {
  await page.goto(url, { waitUntil: 'load' });
}

// measure drives the page and reports readings. Nothing is judged here: the
// functions above own that, and they are the ones the self-test exercises.
async function measure(browser, url) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  const thrown = [];
  page.on('pageerror', (error) => thrown.push(String(error)));
  try {
    await open(page, url);
    const render = await readRender(page);
    if (!render.present || !render.ready) return { render, thrown };

    // The arrow keys, from the stage list's one tab stop.
    const stepCount = await page.locator('#lifecycle-explorer [data-lx-step]').count();
    await page.locator('#lifecycle-explorer [data-lx-step][aria-current="step"]').focus();
    const start = (await readStage(page)).stage;
    const arrows = [];
    const focus = [];
    for (let index = 1; index < stepCount; index += 1) {
      await page.keyboard.press('ArrowRight');
      const now = await readStage(page);
      arrows.push(now.stage === now.shown ? now.stage : `${now.stage}/${now.shown}`);
      focus.push(now.focus);
    }
    await page.keyboard.press('Home');
    const home = (await readStage(page)).stage;
    await page.keyboard.press('End');
    const end = (await readStage(page)).stage;
    await page.keyboard.press('Home');

    // Tab from the heading above the explorer reaches the stage list.
    await page.evaluate(() => {
      const heading = document.getElementById('follow-one-schema-change');
      heading?.setAttribute('tabindex', '-1');
      heading?.focus();
    });
    let tabbed = false;
    for (let press = 0; press < 40 && !tabbed; press += 1) {
      await page.keyboard.press('Tab');
      tabbed = (await readStage(page)).focus !== null;
    }

    // Next, pressed with the keyboard, walks forward.
    const next = [];
    await page.locator('#lifecycle-explorer [data-lx-next]').focus();
    for (let index = 1; index < stepCount; index += 1) {
      await page.keyboard.press('Enter');
      next.push((await readStage(page)).stage);
    }

    // Each object, selected from its node.
    const selection = [];
    for (const id of await page.$$eval('#lifecycle-explorer [data-lx-object]', (nodes) => nodes.map((node) => node.dataset.lxObject))) {
      await page.locator(`#lifecycle-explorer [data-lx-object="${id}"]`).click();
      selection.push(
        await page.evaluate((selected) => {
          const root = document.getElementById('lifecycle-explorer');
          const panel = [...root.querySelectorAll('[data-lx-object-panel]')].find((one) => !one.hidden);
          const listed = panel
            ? [...panel.querySelectorAll('.lx__relations li')].map(
                (item) => `${item.dataset.type}:${item.querySelector('[data-lx-select]')?.dataset.lxSelect}`,
              )
            : [];
          return {
            id: selected,
            panel: panel?.dataset.lxObjectPanel ?? null,
            listed,
            marked: [...root.querySelectorAll('[data-lx-object][data-related]')].map((node) => node.dataset.lxObject),
            pressed: root.querySelector(`[data-lx-object="${selected}"]`)?.getAttribute('aria-pressed') === 'true',
          };
        }, id),
      );
    }

    // A link that names a stage and an object.
    const want = { stage: scenario.stages[Math.min(3, scenario.stages.length - 1)].id, object: objects.at(-1).id };
    await open(page, `${url}?lx-stage=${want.stage}&lx-object=${want.object}`);
    const linked = await readStage(page);

    return {
      render,
      keyboard: { start, arrows, focus, home, end, tabbed, next },
      selection,
      deepLink: { want, got: { stage: linked.stage, object: linked.object } },
      thrown,
    };
  } finally {
    await context.close();
  }
}

// The narrow layout, at rest, at the last stage with an object open, and
// without JavaScript, where the text is what is on screen.
async function measureNarrow(browser, url) {
  const readings = [];
  for (const javaScriptEnabled of [true, false]) {
    const context = await browser.newContext({ viewport: NARROW, javaScriptEnabled });
    const page = await context.newPage();
    try {
      await open(page, url);
      if (!javaScriptEnabled) {
        readings.push(await readOverflow(page, 'without JavaScript'));
        continue;
      }
      readings.push(await readOverflow(page, 'at rest'));
      if ((await page.locator('#lifecycle-explorer [data-lx-step]').count()) > 0) {
        await page.locator('#lifecycle-explorer [data-lx-step]').last().click();
        await page.locator('#lifecycle-explorer [data-lx-object]').first().click();
        readings.push(await readOverflow(page, 'at the last stage with an object selected'));
      }
    } finally {
      await context.close();
    }
  }
  return readings;
}

async function measureWithoutScript(browser, url) {
  const context = await browser.newContext({ javaScriptEnabled: false });
  const page = await context.newPage();
  try {
    await open(page, url);
    return await page.evaluate(() => {
      const root = document.getElementById('lifecycle-explorer');
      if (!root) return { present: false };
      const visible = (element) => element && element.getClientRects().length > 0 && getComputedStyle(element).display !== 'none';
      const text = root.querySelector('[data-lx-text]');
      return {
        present: true,
        appHidden: !visible(root.querySelector('[data-lx-app]')),
        textShown: visible(text),
        stages: [...(text?.querySelectorAll('[data-lx-text-stage]') ?? [])].map((item) => ({
          id: item.dataset.lxTextStage,
          text: item.textContent.replace(/\s+/g, ' '),
        })),
        objects: [...(text?.querySelectorAll('[data-lx-text-object]') ?? [])].map((item) => item.dataset.lxTextObject),
      };
    });
  } finally {
    await context.close();
  }
}

async function main() {
  if (process.argv.includes('--selftest')) {
    selftest();
    return;
  }

  const model = { stages: scenario.stages, objects, relations };
  const problems = modelProblems(model);

  const distIndex = process.argv.indexOf('--dist');
  const distRoot = distIndex >= 0 ? process.argv[distIndex + 1] : join(siteRoot, 'dist');
  if (!existsSync(distRoot)) {
    console.error('check-lifecycle-explorer.mjs: dist/ is missing; build the site first');
    process.exit(1);
  }

  let chromium;
  try {
    ({ chromium } = await import('playwright'));
  } catch {
    const message =
      'check-lifecycle-explorer.mjs: playwright is not installed (npm i -D playwright && npx playwright install chromium)';
    if (process.env.CI) {
      console.error(message);
      process.exit(1);
    }
    console.warn(`${message}; skipping`);
    return;
  }

  const base = detectBase(distRoot);
  const { server, port } = await startServer(distRoot, base);
  const url = `http://127.0.0.1:${port}${base}/${PAGE}`;
  const browser = await chromium.launch();

  try {
    const quiet = await measureWithoutScript(browser, url);
    problems.push(...textProblems(quiet, model));
    if (quiet.present) {
      const measured = await measure(browser, url);
      const rendered = renderProblems(measured.render, model);
      problems.push(...rendered);
      if (rendered.length === 0 || measured.keyboard) {
        problems.push(...keyboardProblems(measured.keyboard, model));
        problems.push(...selectionProblems(measured.selection, model));
        problems.push(...deepLinkProblems(measured.deepLink.got, measured.deepLink.want));
      }
      if (measured.thrown.length > 0) problems.push(`the explorer threw: ${measured.thrown.join('; ')}`);
      problems.push(...overflowProblems(await measureNarrow(browser, url)));
    }
  } finally {
    await browser.close();
    server.close();
  }

  if (problems.length > 0) {
    console.error(`check-lifecycle-explorer.mjs: ${problems.length} problem(s):\n- ${problems.join('\n- ')}`);
    process.exit(1);
  }
  console.log(
    `check-lifecycle-explorer.mjs: OK (${scenario.stages.length} stages by keyboard, ${objects.length} objects ` +
      `and ${relations.length} relationships, the text without JavaScript, a shared link, and ${NARROW.width}px wide)`,
  );
}

await main();
