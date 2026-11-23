#!/usr/bin/env node
// Drives the lifecycle explorer on the Reconciliation page in a browser.
//
// The build proves the component compiled, and the link checker proves its
// links resolve. Neither notices an explorer that never became interactive,
// a stage only a mouse can reach, a path the keyboard cannot choose, an
// object whose relationships the view leaves out, a text version that lost a
// stage or a path, or a layout that scrolls sideways on a phone. This reads
// src/lifecycle-explorer.mjs for what the page owes and holds the built page
// to it:
//
//   - with JavaScript the interactive view replaces the text, with one panel
//     per stage and one list of steps per path;
//   - on every path, Tab and Enter choose the path from the page, Tab reaches
//     its stage list, the arrow keys, Home and End reach every stage it walks
//     in order, and Next walks forward; at the stage where a path turns, Tab
//     and Enter on its choice open its first stage;
//   - selecting each object shows its own panel, lists every relationship the
//     model gives it from both ends, and marks exactly the related objects;
//   - a link carrying a path, a stage and an object restores all three, a link
//     naming another path's stage alone opens that path, and the default path
//     writes no path parameter;
//   - without JavaScript the text carries every path, every stage with its
//     commands, and every object;
//   - at 390px and 320px wide neither the page nor the explorer scrolls
//     sideways, on any path.
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

import { defaultPath, objects, paths, relations, scenario, walkOf } from '../src/lifecycle-explorer.mjs';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const siteRoot = join(scriptDir, '..');
const PAGE = 'reference/reconciliation/';
const NARROW = [
  { width: 390, height: 844 },
  { width: 320, height: 700 },
];

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

// The stages one path walks, and every stage once, from the model alone.
export const walkIds = (path, model) => walkOf(path, model.stages).map((stage) => stage.id);
export const allStages = (model) => [...model.stages, ...model.paths.flatMap((path) => path.stages ?? [])];
const ownStages = (path, model) => (path.turnsAfter ? (path.stages ?? []) : model.stages);

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
// twice, a path that turns at a stage the successful path does not have or
// comes back before it turned. A model with no stage, no object or no path
// measures nothing.
export function modelProblems(model) {
  const problems = [];
  const objectIds = model.objects.map((object) => object.id);
  const mainIds = model.stages.map((stage) => stage.id);
  const stages = allStages(model);
  if (mainIds.length === 0) problems.push('the scenario has no stages');
  if (objectIds.length === 0) problems.push('the model has no objects');
  if (model.paths.length === 0) problems.push('the model has no paths');
  else if (model.paths[0].turnsAfter) problems.push(`the default path "${model.paths[0].id}" turns off itself`);
  for (const [what, ids] of [
    ['stage', stages.map((stage) => stage.id)],
    ['object', objectIds],
    ['path', model.paths.map((path) => path.id)],
    ['path label', model.paths.map((path) => path.label)],
  ]) {
    const seen = new Set();
    for (const id of ids) {
      if (seen.has(id)) problems.push(`the ${what} id "${id}" is used twice`);
      seen.add(id);
    }
  }
  for (const stage of stages) {
    for (const id of stage.objects) {
      if (!objectIds.includes(id)) problems.push(`stage "${stage.id}" names the object "${id}", which does not exist`);
    }
    if (!stage.actors[stage.lead]) problems.push(`stage "${stage.id}" is led by "${stage.lead}", who does nothing in it`);
    if (stage.inspect.length === 0) problems.push(`stage "${stage.id}" says no way to inspect it`);
  }
  for (const path of model.paths.slice(1)) {
    const turn = mainIds.indexOf(path.turnsAfter);
    if (turn < 0) problems.push(`the path "${path.id}" turns after "${path.turnsAfter}", which is not a stage of the default path`);
    if (path.rejoins !== undefined && path.rejoins !== null) {
      const rejoin = mainIds.indexOf(path.rejoins);
      if (rejoin < 0) problems.push(`the path "${path.id}" rejoins at "${path.rejoins}", which is not a stage of the default path`);
      else if (rejoin <= turn) problems.push(`the path "${path.id}" rejoins at "${path.rejoins}", before the stage it turns after`);
    }
    if ((path.stages ?? []).length === 0) problems.push(`the path "${path.id}" has no stage of its own`);
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
  const stages = allStages(model).length;
  if (reading.panels !== stages) {
    problems.push(`the interactive view has ${reading.panels} stage panels and the model ${stages} stages`);
  }
  if (reading.visiblePanels !== 1) problems.push(`${reading.visiblePanels} stage panels are shown at once`);
  if (reading.lists !== model.paths.length) {
    problems.push(`the interactive view has ${reading.lists} lists of steps and the model ${model.paths.length} paths`);
  }
  if (reading.visibleLists !== 1) problems.push(`${reading.visibleLists} lists of steps are shown at once`);
  return problems;
}

// keyboardProblems judges where the keys went, one reading per path.
// `chosen` is whether Tab and Enter from the top of the page made the path
// current, `tabbed` whether Tab then reached its stage list, `start` the stage
// Home reached, `arrows` the stage shown after each ArrowRight and `focus`
// where focus was then, `home` and `end` the stage each key reached, `next`
// the stages Next walked, and `turned` where Tab and Enter on the path's
// choice, at the stage it turns after, led.
export function keyboardProblems(readings, model) {
  const problems = [];
  for (const path of model.paths) {
    const reading = readings.find((one) => one.path === path.id);
    const on = `on the path "${path.id}"`;
    if (!reading) {
      problems.push(`the path "${path.id}" was never walked by keyboard`);
      continue;
    }
    if (!reading.chosen) problems.push(`Tab and Enter from the top of the page never choose the path "${path.id}"`);
    if (!reading.tabbed) problems.push(`${on}, Tab never reaches the stage list`);
    const ids = walkIds(path, model);
    const walked = [reading.start, ...reading.arrows];
    const missing = ids.filter((id) => !walked.includes(id));
    if (missing.length > 0) problems.push(`${on}, the arrow keys never reach ${missing.join(', ')}`);
    if (walked.join(' ') !== ids.join(' ') && missing.length === 0) {
      problems.push(`${on}, the arrow keys walk the stages as ${walked.join(', ')}, not in order`);
    }
    const lost = reading.focus.filter((id, index) => id !== reading.arrows[index]);
    if (lost.length > 0) problems.push(`${on}, after an arrow key focus is not on the stage it selected`);
    if (reading.end !== ids.at(-1)) problems.push(`${on}, End reaches ${reading.end}, not the last stage`);
    if (reading.home !== ids[0]) problems.push(`${on}, Home reaches ${reading.home}, not the first stage`);
    if (reading.next.join(' ') !== ids.slice(1).join(' ')) {
      problems.push(`${on}, Next walks ${reading.next.join(', ') || 'nowhere'}, not every stage after the first`);
    }
    if (path.turnsAfter) {
      const want = path.stages[0].id;
      const got = reading.turned;
      if (!got || got.path !== path.id || got.stage !== want || got.focus !== want) {
        problems.push(
          `at the stage "${path.turnsAfter}", Tab and Enter on the choice "${path.id}" reach ` +
            (got ? `${got.stage ?? 'no stage'} on the path ${got.path ?? 'none'}, focus on ${got.focus ?? 'nothing'}` : 'nothing') +
            `, not ${want}`,
        );
      }
    }
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

// textProblems judges the page without JavaScript: every path, each with its
// own stages written out with their titles and commands and the successful
// stages it shares named, and every object, in the text a reader, a crawler or
// a printer gets.
export function textProblems(reading, model) {
  const problems = [];
  if (!reading.present) return ['without JavaScript the page carries no lifecycle explorer'];
  if (!reading.appHidden) problems.push('without JavaScript the interactive view is on screen, with no script to drive it');
  if (!reading.textShown) problems.push('without JavaScript the text version is not on screen');
  for (const path of model.paths) {
    const block = reading.paths.find((one) => one.id === path.id);
    if (!block) {
      problems.push(`without JavaScript the text leaves out the path "${path.id}"`);
      continue;
    }
    const own = ownStages(path, model);
    const stages = new Map(block.stages.map((stage) => [stage.id, stage.text]));
    for (const stage of own) {
      const text = stages.get(stage.id);
      if (text === undefined) {
        problems.push(`without JavaScript the path "${path.id}" leaves out the stage "${stage.id}"`);
        continue;
      }
      if (!text.includes(stage.title)) problems.push(`without JavaScript the stage "${stage.id}" lacks its title`);
      for (const step of stage.inspect) {
        if (!text.includes(step.command)) {
          problems.push(`without JavaScript the stage "${stage.id}" lacks the command ${JSON.stringify(step.command)}`);
        }
      }
    }
    const order = block.stages.map((stage) => stage.id).filter((id) => own.some((stage) => stage.id === id));
    const wantOrder = own.map((stage) => stage.id).filter((id) => stages.has(id));
    if (order.join(' ') !== wantOrder.join(' ')) problems.push(`without JavaScript the stages of the path "${path.id}" are out of order`);
    const ownIds = own.map((stage) => stage.id);
    for (const stage of walkOf(path, model.stages)) {
      if (ownIds.includes(stage.id)) continue;
      if (!block.text.includes(stage.title)) {
        problems.push(`without JavaScript the path "${path.id}" does not name the stage "${stage.id}" it shares`);
      }
    }
  }
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

// deepLinkProblems judges pages opened from links that name a state.
export function deepLinkProblems(readings) {
  const problems = [];
  if (readings.length === 0) return ['no shared link was opened'];
  for (const { link, want, got } of readings) {
    if (got.path !== want.path) problems.push(`the link ${link} opens the path ${got.path ?? 'none'}, not ${want.path}`);
    if (got.stage !== want.stage) problems.push(`the link ${link} opens the stage ${got.stage ?? 'none'}, not ${want.stage}`);
    if (got.object !== want.object) problems.push(`the link ${link} selects ${got.object ?? 'nothing'}, not ${want.object ?? 'nothing'}`);
  }
  return problems;
}

// urlProblems judges the address the explorer wrote: a stage chosen on the
// default path writes no path parameter, so links made before there were
// paths keep their meaning, and a stage on another path names it.
export function urlProblems(reading) {
  const problems = [];
  const plain = new URLSearchParams(reading.defaultSearch);
  if (plain.has('lx-path')) problems.push(`a stage chosen on the default path writes ${reading.defaultSearch}, with a path parameter`);
  if (!plain.has('lx-stage')) problems.push(`a stage chosen on the default path writes ${reading.defaultSearch || 'nothing'}, with no stage`);
  const branch = new URLSearchParams(reading.branchSearch);
  if (branch.get('lx-path') !== reading.branchPath) {
    problems.push(`choosing the path ${reading.branchPath} writes ${reading.branchSearch || 'nothing'}, not lx-path=${reading.branchPath}`);
  }
  return problems;
}

function selftest() {
  const stage = (id, title, objects, extra = {}) => ({
    id,
    title,
    lead: 'you',
    actors: { you: 'act' },
    objects,
    inspect: [{ command: `cmd ${id}` }],
    ...extra,
  });
  const model = {
    stages: [stage('one', 'First', ['a']), stage('two', 'Second', ['a', 'b']), stage('three', 'Third', ['c'])],
    paths: [
      { id: 'main', label: 'Main' },
      { id: 'side', label: 'Side', turnsAfter: 'one', rejoins: 'three', stages: [stage('side-a', 'Aside', ['b'])] },
      { id: 'end', label: 'End', turnsAfter: 'two', stages: [stage('end-a', 'Ending', ['c'])] },
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
  const withPath = (index, change) => ({
    ...model,
    paths: model.paths.map((path, at) => (at === index ? { ...path, ...change } : path)),
  });

  const walks = model.paths.map((path) => walkIds(path, model).join(' '));
  if (walks.join(' | ') !== 'one two three | one side-a three | one two end-a') fail('walkOf read the paths as', walks);

  const a = expectedRelations('a', model);
  if (a.listed.join(' ') !== 'owns:b references:b references:c' || a.related.join(' ') !== 'b c') {
    fail('expectedRelations read a as', a);
  }

  if (modelProblems(model).length !== 0) fail('modelProblems refused a sound model', modelProblems(model));
  refuses('a stage naming a missing object', modelProblems({ ...model, stages: [{ ...model.stages[0], objects: ['z'] }, ...model.stages.slice(1)] }), 'names the object "z"');
  refuses('a branch stage naming a missing object', modelProblems(withPath(1, { stages: [stage('side-a', 'Aside', ['z'])] })), 'names the object "z"');
  refuses('a relation to nowhere', modelProblems({ ...model, relations: [...model.relations, { from: 'a', to: 'z', type: 'owns' }] }), 'names "z"');
  refuses('a stage id used twice', modelProblems({ ...model, stages: [model.stages[0], model.stages[0], model.stages[2]] }), 'used twice');
  refuses('a branch stage that takes a successful id', modelProblems(withPath(1, { stages: [stage('two', 'Aside', ['b'])] })), '"two" is used twice');
  refuses('a lead who does nothing', modelProblems({ ...model, stages: [{ ...model.stages[0], lead: 'executor' }, ...model.stages.slice(1)] }), 'who does nothing');
  refuses('an object related to nothing', modelProblems({ ...model, objects: [...model.objects, { id: 'd' }] }), '"d" is related to nothing');
  refuses('an empty scenario', modelProblems({ ...model, stages: [] }), 'no stages');
  refuses('no paths at all', modelProblems({ ...model, paths: [] }), 'no paths');
  refuses('a default path that turns', modelProblems(withPath(0, { turnsAfter: 'one' })), 'turns off itself');
  refuses('a turn at no stage', modelProblems(withPath(1, { turnsAfter: 'zero' })), 'turns after "zero"');
  refuses('a rejoin at no stage', modelProblems(withPath(1, { rejoins: 'zero' })), 'rejoins at "zero", which is not');
  refuses('a rejoin before the turn', modelProblems(withPath(2, { rejoins: 'one' })), 'before the stage it turns after');
  refuses('a path with no stage of its own', modelProblems(withPath(1, { stages: [] })), 'no stage of its own');
  refuses('a path label used twice', modelProblems(withPath(2, { label: 'Side' })), 'path label id "Side"');

  const goodRender = { present: true, ready: true, appHidden: false, textShown: false, panels: 5, visiblePanels: 1, lists: 3, visibleLists: 1 };
  if (renderProblems(goodRender, model).length !== 0) fail('renderProblems refused a page that holds', renderProblems(goodRender, model));
  for (const [name, spoil, want] of [
    ['a page with no explorer', { present: false }, 'carries no lifecycle explorer'],
    ['a script that never ran', { ready: false }, 'never ran'],
    ['an interactive view left hidden', { appHidden: true }, 'still hidden'],
    ['the text left beside it', { textShown: true }, 'still on screen'],
    ['a stage panel missing', { panels: 4 }, '4 stage panels'],
    ['two panels shown at once', { visiblePanels: 2 }, '2 stage panels are shown'],
    ['a path with no list of steps', { lists: 2 }, '2 lists of steps'],
    ['two lists shown at once', { visibleLists: 2 }, '2 lists of steps are shown'],
  ]) {
    refuses(name, renderProblems({ ...goodRender, ...spoil }, model), want);
  }

  const goodKeys = [
    { path: 'main', chosen: true, tabbed: true, start: 'one', arrows: ['two', 'three'], focus: ['two', 'three'], home: 'one', end: 'three', next: ['two', 'three'] },
    {
      path: 'side',
      chosen: true,
      tabbed: true,
      start: 'one',
      arrows: ['side-a', 'three'],
      focus: ['side-a', 'three'],
      home: 'one',
      end: 'three',
      next: ['side-a', 'three'],
      turned: { path: 'side', stage: 'side-a', focus: 'side-a' },
    },
    {
      path: 'end',
      chosen: true,
      tabbed: true,
      start: 'one',
      arrows: ['two', 'end-a'],
      focus: ['two', 'end-a'],
      home: 'one',
      end: 'end-a',
      next: ['two', 'end-a'],
      turned: { path: 'end', stage: 'end-a', focus: 'end-a' },
    },
  ];
  if (keyboardProblems(goodKeys, model).length !== 0) fail('keyboardProblems refused keys that hold', keyboardProblems(goodKeys, model));
  for (const [name, index, spoil, want] of [
    ['arrows that stop short', 0, { arrows: ['two', 'two'], focus: ['two', 'two'] }, 'never reach three'],
    ['arrows out of order', 0, { arrows: ['three', 'two'], focus: ['three', 'two'] }, 'not in order'],
    ['focus left behind', 0, { focus: ['one', 'one'] }, 'focus is not on the stage'],
    ['End that misses', 0, { end: 'two' }, 'End reaches two'],
    ['Home that misses', 0, { home: 'two' }, 'Home reaches two'],
    ['a stage list Tab skips', 0, { tabbed: false }, 'Tab never reaches the stage list'],
    ['Next that does nothing', 0, { next: [] }, 'Next walks nowhere'],
    ['a path the keyboard cannot choose', 2, { chosen: false }, 'never choose the path "end"'],
    [
      'a branch stage the keyboard cannot reach',
      1,
      { arrows: ['two', 'three'], focus: ['two', 'three'], next: ['two', 'three'] },
      'never reach side-a',
    ],
    ['a turn the keyboard cannot take', 1, { turned: null }, 'reach nothing, not side-a'],
    ['a turn that leads elsewhere', 2, { turned: { path: 'main', stage: 'three', focus: 'three' } }, 'reach three on the path main'],
    ['a turn that drops focus', 2, { turned: { path: 'end', stage: 'end-a', focus: null } }, 'focus on nothing'],
  ]) {
    const bad = structuredClone(goodKeys);
    Object.assign(bad[index], spoil);
    refuses(name, keyboardProblems(bad, model), want);
  }
  refuses('a path never walked', keyboardProblems(goodKeys.slice(0, 2), model), 'path "end" was never walked');

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
    paths: [
      {
        id: 'main',
        text: 'First. cmd one Second. cmd two Third. cmd three',
        stages: [
          { id: 'one', text: 'First. cmd one' },
          { id: 'two', text: 'Second. cmd two' },
          { id: 'three', text: 'Third. cmd three' },
        ],
      },
      { id: 'side', text: 'Stage 1 is First. Aside. cmd side-a Then Third.', stages: [{ id: 'side-a', text: 'Aside. cmd side-a' }] },
      { id: 'end', text: 'Stages 1 to 2: First, Second. Ending. cmd end-a', stages: [{ id: 'end-a', text: 'Ending. cmd end-a' }] },
    ],
    objects: ['a', 'b', 'c'],
  };
  if (textProblems(goodText, model).length !== 0) fail('textProblems refused a text that holds', textProblems(goodText, model));
  const spoilPath = (index, change) => ({ paths: goodText.paths.map((path, at) => (at === index ? { ...path, ...change } : path)) });
  for (const [name, spoil, want] of [
    ['a page with no explorer', { present: false }, 'carries no lifecycle explorer'],
    ['an interactive view shown without its script', { appHidden: false }, 'with no script to drive it'],
    ['a text hidden without a script', { textShown: false }, 'text version is not on screen'],
    ['a path dropped', { paths: goodText.paths.slice(0, 2) }, 'leaves out the path "end"'],
    ['a stage dropped', spoilPath(0, { stages: goodText.paths[0].stages.slice(0, 2) }), 'leaves out the stage "three"'],
    ['a branch stage dropped', spoilPath(1, { stages: [] }), 'path "side" leaves out the stage "side-a"'],
    ['a stage without its title', spoilPath(0, { stages: [{ id: 'one', text: 'cmd one' }, ...goodText.paths[0].stages.slice(1)] }), 'lacks its title'],
    ['a stage without its command', spoilPath(2, { stages: [{ id: 'end-a', text: 'Ending.' }] }), 'lacks the command "cmd end-a"'],
    [
      'stages out of order',
      spoilPath(0, { stages: [goodText.paths[0].stages[1], goodText.paths[0].stages[0], goodText.paths[0].stages[2]] }),
      'path "main" are out of order',
    ],
    ['a shared stage left unnamed', spoilPath(1, { text: 'Aside. cmd side-a Then Third.' }), 'does not name the stage "one"'],
    ['an object dropped', { objects: ['a', 'b'] }, 'leaves out the object "c"'],
  ]) {
    refuses(name, textProblems({ ...goodText, ...spoil }, model), want);
  }

  const fits = { what: 'at rest', viewport: 390, page: 390, explorer: 340, explorerBox: 340 };
  if (overflowProblems([fits]).length !== 0) fail('overflowProblems refused a page that fits', overflowProblems([fits]));
  refuses('a page wider than the viewport', overflowProblems([{ ...fits, page: 520 }]), 'the page is 520px wide');
  refuses('an explorer that scrolls', overflowProblems([{ ...fits, explorer: 410 }]), 'the explorer scrolls');
  refuses('no reading at all', overflowProblems([]), 'no narrow-screen reading');

  const link = { link: '?lx-path=side&lx-stage=side-a&lx-object=c', want: { path: 'side', stage: 'side-a', object: 'c' } };
  const held = [{ ...link, got: link.want }];
  if (deepLinkProblems(held).length !== 0) fail('deepLinkProblems refused a link that holds', deepLinkProblems(held));
  refuses('a link whose path was dropped', deepLinkProblems([{ ...link, got: { ...link.want, path: 'main' } }]), 'opens the path main');
  refuses('a link whose stage was dropped', deepLinkProblems([{ ...link, got: { ...link.want, stage: 'one' } }]), 'opens the stage one');
  refuses('a link whose object was dropped', deepLinkProblems([{ ...link, got: { ...link.want, object: null } }]), 'selects nothing');
  refuses('no link opened', deepLinkProblems([]), 'no shared link');

  const goodUrl = { defaultSearch: '?lx-stage=two', branchSearch: '?lx-path=side&lx-stage=side-a', branchPath: 'side' };
  if (urlProblems(goodUrl).length !== 0) fail('urlProblems refused an address that holds', urlProblems(goodUrl));
  refuses('a default path that writes itself', urlProblems({ ...goodUrl, defaultSearch: '?lx-path=main&lx-stage=two' }), 'with a path parameter');
  refuses('a stage that is not written', urlProblems({ ...goodUrl, defaultSearch: '' }), 'with no stage');
  refuses('a branch path that is not written', urlProblems({ ...goodUrl, branchSearch: '?lx-stage=side-a' }), 'not lx-path=side');

  console.log(
    'check-lifecycle-explorer.mjs --selftest: OK (the model and its paths, the rendered view, the keyboard on ' +
      'every path and at every turn, each selection, the text without JavaScript, shared links, the address, ' +
      'and the narrow layout)',
  );
}

const readRender = (page) =>
  page.evaluate(() => {
    const root = document.getElementById('lifecycle-explorer');
    if (!root) return { present: false };
    const visible = (element) => element && element.getClientRects().length > 0 && getComputedStyle(element).display !== 'none';
    const panels = [...root.querySelectorAll('[data-lx-stage-panel]')];
    const lists = [...root.querySelectorAll('[data-lx-steps]')];
    return {
      present: true,
      ready: root.hasAttribute('data-lx-ready'),
      appHidden: !visible(root.querySelector('[data-lx-app]')),
      textShown: visible(root.querySelector('[data-lx-text]')),
      panels: panels.length,
      visiblePanels: panels.filter((panel) => visible(panel)).length,
      lists: lists.length,
      visibleLists: lists.filter((list) => visible(list)).length,
    };
  });

const readStage = (page) =>
  page.evaluate(() => {
    const root = document.getElementById('lifecycle-explorer');
    const list = root?.querySelector('[data-lx-steps]:not([hidden])');
    return {
      path: root?.querySelector('[data-lx-path][aria-pressed="true"]')?.dataset.lxPath ?? null,
      stage: list?.querySelector('[data-lx-step][aria-current="step"]')?.dataset.lxStep ?? null,
      shown: [...(root?.querySelectorAll('[data-lx-stage-panel]') ?? [])].find((panel) => !panel.hidden)?.dataset.lxStagePanel ?? null,
      focus: document.activeElement?.dataset?.lxStep ?? null,
      object: root?.querySelector('[data-lx-object][aria-pressed="true"]')?.dataset.lxObject ?? null,
    };
  });

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

const activeMatches = (page, selector) => page.evaluate((one) => document.activeElement?.matches(one) ?? false, selector);

// Tab until focus is on what `selector` matches, from wherever focus is.
async function tabTo(page, selector, presses) {
  for (let press = 0; press < presses; press += 1) {
    await page.keyboard.press('Tab');
    if (await activeMatches(page, selector)) return true;
  }
  return false;
}

async function focusHeading(page) {
  await page.evaluate(() => {
    const heading = document.getElementById('follow-one-schema-change');
    heading?.setAttribute('tabindex', '-1');
    heading?.focus();
  });
}

const VISIBLE_STEP = '#lifecycle-explorer [data-lx-steps]:not([hidden]) [data-lx-step]';

// One path, by keyboard alone from the heading above the explorer: choose it,
// reach its stage list, walk it with the arrow keys, Home, End and Next, and
// take its turn at the stage it turns after.
async function walkPath(page, url, path, model) {
  await open(page, url);
  const length = walkIds(path, model).length;
  await focusHeading(page);
  const reached = await tabTo(page, `#lifecycle-explorer [data-lx-path="${path.id}"]`, 60);
  if (reached) await page.keyboard.press('Enter');
  const chosen = reached && (await readStage(page)).path === path.id;
  const tabbed = await tabTo(page, `${VISIBLE_STEP}[aria-current="step"]`, 20);
  if (!tabbed) await page.locator(`${VISIBLE_STEP}[aria-current="step"]`).focus();
  await page.keyboard.press('Home');
  const start = (await readStage(page)).stage;
  const arrows = [];
  const focus = [];
  for (let index = 1; index < length; index += 1) {
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

  const next = [];
  await page.locator('#lifecycle-explorer [data-lx-next]').focus();
  for (let index = 1; index < length; index += 1) {
    await page.keyboard.press('Enter');
    next.push((await readStage(page)).stage);
  }

  let turned;
  if (path.turnsAfter) {
    await open(page, url);
    await page.locator(`${VISIBLE_STEP}[data-lx-step="${path.turnsAfter}"]`).click();
    await page.locator(`${VISIBLE_STEP}[data-lx-step="${path.turnsAfter}"]`).focus();
    turned = null;
    if (await tabTo(page, `#lifecycle-explorer [data-lx-choose="${path.id}"]`, 60)) {
      await page.keyboard.press('Enter');
      const now = await readStage(page);
      turned = { path: now.path, stage: now.stage, focus: now.focus };
    }
  }
  return { path: path.id, chosen, tabbed, start, arrows, focus, home, end, next, turned };
}

// measure drives the page and reports readings. Nothing is judged here: the
// functions above own that, and they are the ones the self-test exercises.
async function measure(browser, url, model) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  const thrown = [];
  page.on('pageerror', (error) => thrown.push(String(error)));
  try {
    await open(page, url);
    const render = await readRender(page);
    if (!render.present || !render.ready) return { render, thrown };

    const keyboard = [];
    for (const path of model.paths) keyboard.push(await walkPath(page, url, path, model));

    // Each object, selected from its node.
    await open(page, url);
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

    // What the explorer writes: a stage on the default path, then a branch.
    await open(page, url);
    await page.locator(VISIBLE_STEP).nth(1).click();
    const defaultSearch = await page.evaluate(() => location.search);
    const branchPath = model.paths[1]?.id ?? null;
    if (branchPath) await page.locator(`#lifecycle-explorer [data-lx-path="${branchPath}"]`).click();
    const branchSearch = await page.evaluate(() => location.search);

    // Links that name a state: one from before there were paths, one per
    // other path, and one that names another path's stage alone.
    const links = [
      { link: `?lx-stage=${model.stages[Math.min(3, model.stages.length - 1)].id}&lx-object=${model.objects.at(-1).id}`, want: { path: model.paths[0].id, stage: model.stages[Math.min(3, model.stages.length - 1)].id, object: model.objects.at(-1).id } },
      ...model.paths.slice(1).map((path) => ({
        link: `?lx-path=${path.id}&lx-stage=${path.stages.at(-1).id}&lx-object=${model.objects[0].id}`,
        want: { path: path.id, stage: path.stages.at(-1).id, object: model.objects[0].id },
      })),
      ...(model.paths[1]
        ? [{ link: `?lx-stage=${model.paths[1].stages[0].id}`, want: { path: model.paths[1].id, stage: model.paths[1].stages[0].id, object: null } }]
        : []),
    ];
    const deepLinks = [];
    for (const one of links) {
      await open(page, `${url}${one.link}`);
      const got = await readStage(page);
      deepLinks.push({ ...one, got: { path: got.path, stage: got.stage, object: got.object } });
    }

    return { render, keyboard, selection, url: { defaultSearch, branchSearch, branchPath }, deepLinks, thrown };
  } finally {
    await context.close();
  }
}

// The narrow layouts: at rest, at each path's turn with its choices on
// screen, at each path's last stage with an object open, and without
// JavaScript, where the text is what is on screen.
async function measureNarrow(browser, url, model) {
  const readings = [];
  for (const viewport of NARROW) {
    for (const javaScriptEnabled of [true, false]) {
      const context = await browser.newContext({ viewport, javaScriptEnabled });
      const page = await context.newPage();
      const at = `${viewport.width}px`;
      try {
        await open(page, url);
        if (!javaScriptEnabled) {
          readings.push(await readOverflow(page, `${at}, without JavaScript`));
          continue;
        }
        readings.push(await readOverflow(page, `${at}, at rest`));
        if ((await page.locator(VISIBLE_STEP).count()) === 0) continue;
        for (const path of model.paths) {
          await page.locator(`#lifecycle-explorer [data-lx-path="${model.paths[0].id}"]`).click();
          if (path.turnsAfter) {
            await page.locator(`${VISIBLE_STEP}[data-lx-step="${path.turnsAfter}"]`).click();
            readings.push(await readOverflow(page, `${at}, at the turn to ${path.id}`));
          }
          await page.locator(`#lifecycle-explorer [data-lx-path="${path.id}"]`).click();
          await page.locator(VISIBLE_STEP).last().click();
          await page.locator('#lifecycle-explorer [data-lx-object]').first().click();
          readings.push(await readOverflow(page, `${at}, on ${path.id} at the last stage with an object selected`));
          await page.locator('#lifecycle-explorer [data-lx-object]').first().click();
        }
      } finally {
        await context.close();
      }
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
      const flat = (element) => element.textContent.replace(/\s+/g, ' ');
      return {
        present: true,
        appHidden: !visible(root.querySelector('[data-lx-app]')),
        textShown: visible(text),
        paths: [...(text?.querySelectorAll('[data-lx-text-path]') ?? [])].map((block) => ({
          id: block.dataset.lxTextPath,
          text: flat(block),
          stages: [...block.querySelectorAll('[data-lx-text-stage]')].map((item) => ({
            id: item.dataset.lxTextStage,
            text: flat(item),
          })),
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

  const model = { stages: scenario.stages, paths, objects, relations };
  const problems = modelProblems(model);
  if (paths[0].id !== defaultPath) problems.push(`the default path is "${defaultPath}", not the first path "${paths[0].id}"`);

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
      const measured = await measure(browser, url, model);
      const rendered = renderProblems(measured.render, model);
      problems.push(...rendered);
      if (measured.keyboard) {
        problems.push(...keyboardProblems(measured.keyboard, model));
        problems.push(...selectionProblems(measured.selection, model));
        problems.push(...urlProblems(measured.url));
        problems.push(...deepLinkProblems(measured.deepLinks));
      }
      if (measured.thrown.length > 0) problems.push(`the explorer threw: ${measured.thrown.join('; ')}`);
      problems.push(...overflowProblems(await measureNarrow(browser, url, model)));
    }
  } finally {
    await browser.close();
    server.close();
  }

  if (problems.length > 0) {
    console.error(`check-lifecycle-explorer.mjs: ${problems.length} problem(s):\n- ${problems.join('\n- ')}`);
    process.exit(1);
  }
  const stageCount = allStages(model).length;
  console.log(
    `check-lifecycle-explorer.mjs: OK (${paths.length} paths and ${stageCount} stages by keyboard, every turn, ` +
      `${objects.length} objects and ${relations.length} relationships, the text without JavaScript, shared links, ` +
      `and ${NARROW.map((one) => `${one.width}px`).join(' and ')} wide)`,
  );
}

await main();
