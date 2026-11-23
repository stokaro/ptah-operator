#!/usr/bin/env node
// Drives the lifecycle explorer on the Reconciliation page in a browser.
//
// The build proves the component compiled, and the link checker proves its
// links resolve. Neither notices an explorer that never became interactive,
// a stage only a mouse can reach, a family or a path the keyboard cannot
// choose, an object whose relationships the view leaves out, a family switch
// that carries one family's path, stage or object into the other, a text
// version that lost a family, a stage or a path, or a layout that scrolls
// sideways on a phone. This reads src/lifecycle-explorer.mjs for what the page
// owes and holds the built page to it:
//
//   - with JavaScript the interactive view replaces the text, with one view
//     per family, one panel per stage and one list of steps per path;
//   - on every path of every family, Tab and Enter choose the family and the
//     path from the page, Tab reaches its stage list, the arrow keys, Home and
//     End reach every stage it walks in order, and Next walks forward; at the
//     stage where a path turns, on the path it turns off, Tab and Enter on its
//     choice open its first stage;
//   - selecting each object shows its own panel, lists every relationship the
//     model gives it from both ends, and marks exactly the related objects;
//   - choosing another family opens it at its default path and its first
//     stage with nothing selected, even when it was left elsewhere, and
//     writes no path or object parameter;
//   - a link carrying a family, a path, a stage and an object restores them, a
//     link naming another family's stage, path or object alone opens that
//     family, a value that belongs to another family than the one a link
//     names is ignored, and the default family and path write no parameter;
//   - without JavaScript the text carries every family, every path, every
//     stage with its commands, and every object;
//   - at 390px and 320px wide neither the page nor the explorer scrolls
//     sideways, at any stage of any path or with any object selected.
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

import { defaultFamily, families, parentOf, walkOf } from '../src/lifecycle-explorer.mjs';

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

// One family as the checks read it: its stages, paths, objects and relations.
export const familyModel = (family) => ({
  id: family.id,
  label: family.label,
  stages: family.scenario.stages,
  paths: family.paths,
  objects: family.objects,
  relations: family.relations,
});

// The stages one path walks, and every stage once, from the model alone.
export const walkIds = (path, model) => walkOf(path, model).map((stage) => stage.id);
export const allStages = (model) => [...model.stages, ...model.paths.flatMap((path) => path.stages ?? [])];
const ownStages = (path, model) => (path.turnsAfter ? (path.stages ?? []) : model.stages);
const isDefaultState = (state, model) =>
  state.path === model.paths[0].id || state.stage === model.stages[0].id || state.object === null;

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

// The paths a path turns off, nearest first, and whether following them came
// back to one already seen.
function ancestry(path, model) {
  const chain = [path.id];
  for (let at = parentOf(path, model); at; at = parentOf(at, model)) {
    if (chain.includes(at.id)) return { chain: [...chain, at.id], cycle: true };
    chain.push(at.id);
  }
  return { chain, cycle: false };
}

// modelProblems refuses one family's model the page could not render
// honestly: a stage naming an object that does not exist, a relation to
// nowhere, an id used twice, a path that turns at a stage neither the
// successful path nor another path has, turns off itself, or comes back
// before it turned. A model with no stage, no object or no path measures
// nothing.
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
  const branchIds = model.paths.slice(1).flatMap((path) => (path.stages ?? []).map((stage) => stage.id));
  for (const path of model.paths.slice(1)) {
    if ((path.stages ?? []).length === 0) problems.push(`the path "${path.id}" has no stage of its own`);
    if ((path.stages ?? []).some((stage) => stage.id === path.turnsAfter)) {
      problems.push(`the path "${path.id}" turns after "${path.turnsAfter}", one of its own stages`);
      continue;
    }
    if (!mainIds.includes(path.turnsAfter) && !branchIds.includes(path.turnsAfter)) {
      problems.push(`the path "${path.id}" turns after "${path.turnsAfter}", which is not a stage of the default path or of another path`);
      continue;
    }
    const { chain, cycle } = ancestry(path, model);
    if (cycle) {
      problems.push(`the paths ${chain.join(', ')} turn off each other`);
      continue;
    }
    const parent = parentOf(path, model);
    const parentWalk = walkIds(parent, model);
    const turn = parentWalk.indexOf(path.turnsAfter);
    if (path.rejoins !== undefined && path.rejoins !== null) {
      const rejoin = parentWalk.indexOf(path.rejoins);
      if (rejoin < 0) {
        problems.push(`the path "${path.id}" rejoins at "${path.rejoins}", which is not a stage of the path "${parent.id}" it turns off`);
      } else if (rejoin <= turn) problems.push(`the path "${path.id}" rejoins at "${path.rejoins}", before the stage it turns after`);
    }
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

// familiesProblems refuses the set of families: none at all, a family id or
// label used twice, or a stage, path or object id two families share. A link
// names a stage, a path or an object without its family, so an id has to say
// which family it belongs to. Each family's own model is held to
// modelProblems.
export function familiesProblems(models) {
  const problems = [];
  if (models.length === 0) return ['the explorer has no family'];
  for (const [what, ids] of [
    ['family', models.map((model) => model.id)],
    ['family label', models.map((model) => model.label)],
  ]) {
    const seen = new Set();
    for (const id of ids) {
      if (seen.has(id)) problems.push(`the ${what} "${id}" is used twice`);
      seen.add(id);
    }
  }
  for (const [what, idsOf] of [
    ['stage', (model) => allStages(model).map((stage) => stage.id)],
    ['path', (model) => model.paths.map((path) => path.id)],
    ['object', (model) => model.objects.map((object) => object.id)],
  ]) {
    const owner = new Map();
    for (const model of models) {
      for (const id of new Set(idsOf(model))) {
        if (owner.has(id)) problems.push(`the ${what} id "${id}" is used in the families ${owner.get(id)} and ${model.id}`);
        else owner.set(id, model.id);
      }
    }
  }
  for (const model of models) {
    for (const problem of modelProblems(model)) problems.push(`the family "${model.id}": ${problem}`);
  }
  return problems;
}

// renderProblems judges the page as JavaScript left it.
export function renderProblems(reading, models) {
  const problems = [];
  if (!reading.present) return ['the page carries no lifecycle explorer'];
  if (!reading.ready) problems.push('the explorer’s script never ran: the interactive view did not replace the text');
  if (reading.appHidden) problems.push('the interactive view is still hidden');
  if (reading.textShown) problems.push('the text version is still on screen beside the interactive view');
  if (reading.familyButtons !== models.length) {
    problems.push(`the interactive view has ${reading.familyButtons} family buttons and the model ${models.length} families`);
  }
  if (reading.views !== models.length) {
    problems.push(`the interactive view has ${reading.views} family views and the model ${models.length} families`);
  }
  if (reading.visibleViews !== 1) problems.push(`${reading.visibleViews} family views are shown at once`);
  const stages = models.reduce((sum, model) => sum + allStages(model).length, 0);
  if (reading.panels !== stages) {
    problems.push(`the interactive view has ${reading.panels} stage panels and the model ${stages} stages`);
  }
  if (reading.visiblePanels !== 1) problems.push(`${reading.visiblePanels} stage panels are shown at once`);
  const paths = models.reduce((sum, model) => sum + model.paths.length, 0);
  if (reading.lists !== paths) {
    problems.push(`the interactive view has ${reading.lists} lists of steps and the model ${paths} paths`);
  }
  if (reading.visibleLists !== 1) problems.push(`${reading.visibleLists} lists of steps are shown at once`);
  return problems;
}

// keyboardProblems judges where the keys went, one reading per path of one
// family. `chosen` is whether Tab and Enter from the top of the page made the
// family and then the path current, `tabbed` whether Tab then reached its
// stage list, `start` the stage Home reached, `arrows` the stage shown after
// each ArrowRight and `focus` where focus was then, `home` and `end` the stage
// each key reached, `next` the stages Next walked, and `turned` where Tab and
// Enter on the path's choice, at the stage it turns after on the path it
// turns off, led.
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

// selectionProblems judges what selecting each object of one family showed.
export function selectionProblems(readings, model) {
  const problems = [];
  if (readings.length !== model.objects.length) {
    problems.push(`${readings.length} of ${model.objects.length} objects of the family "${model.id}" were selected`);
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

// familyProblems judges each switch from one family to another. The family
// switched to has to open at its default path and its first stage with
// nothing selected, and the address has to name the stage and, for any but
// the default family, the family, and nothing else. A family that had never
// been opened is at its defaults anyway, so each family also has to be
// switched back to from a state it was left in that is default in nothing:
// otherwise its reset was never measured.
export function familyProblems(readings, models) {
  const problems = [];
  if (readings.length === 0) return ['no family was switched to'];
  for (const model of models) {
    const returns = readings.filter((reading) => reading.to === model.id && reading.left);
    if (!readings.some((reading) => reading.to === model.id)) problems.push(`the family "${model.id}" was never switched to`);
    else if (!returns.some((reading) => !isDefaultState(reading.left, model))) {
      problems.push(`no switch came back to the family "${model.id}" from a state it was left in, so its reset was never measured`);
    }
  }
  for (const { to, from, got } of readings) {
    const model = models.find((one) => one.id === to);
    if (!model) {
      problems.push(`a switch went to the family "${to}", which the model does not have`);
      continue;
    }
    const at = `switching from ${from} to the family "${to}"`;
    if (got.family !== to || got.view !== to) {
      problems.push(`${at} marks the family ${got.family ?? 'none'} and shows the view of ${got.view ?? 'none'}`);
    }
    if (got.views !== 1) problems.push(`${at} shows ${got.views} family views`);
    const path = model.paths[0].id;
    const stage = model.stages[0].id;
    if (got.path !== path) problems.push(`${at} opens the path ${got.path ?? 'none'}, not its default ${path}`);
    if (got.stage !== stage) problems.push(`${at} opens the stage ${got.stage ?? 'none'}, not its first ${stage}`);
    if (got.object !== null) problems.push(`${at} keeps ${got.object} selected`);
    const search = new URLSearchParams(got.search);
    const family = to === models[0].id ? null : to;
    if (search.get('lx-family') !== family) {
      problems.push(`${at} writes ${got.search || 'nothing'}, ${family ? `not lx-family=${family}` : 'with a family parameter'}`);
    }
    if (search.has('lx-path')) problems.push(`${at} writes ${got.search}, with a path parameter`);
    if (search.has('lx-object')) problems.push(`${at} writes ${got.search}, with an object parameter`);
    if (search.get('lx-stage') !== stage) problems.push(`${at} writes ${got.search || 'nothing'}, not lx-stage=${stage}`);
  }
  return problems;
}

// textProblems judges the page without JavaScript: every family, and in it
// every path, each with its own stages written out with their titles and
// commands and the stages it shares with the path it turns off named, and
// every object, in the text a reader, a crawler or a printer gets.
export function textProblems(reading, models) {
  const problems = [];
  if (!reading.present) return ['without JavaScript the page carries no lifecycle explorer'];
  if (!reading.appHidden) problems.push('without JavaScript the interactive view is on screen, with no script to drive it');
  if (!reading.textShown) problems.push('without JavaScript the text version is not on screen');
  for (const model of models) {
    const family = reading.families.find((one) => one.id === model.id);
    if (!family) {
      problems.push(`without JavaScript the text leaves out the family "${model.id}"`);
      continue;
    }
    for (const path of model.paths) {
      const block = family.paths.find((one) => one.id === path.id);
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
      for (const stage of walkOf(path, model)) {
        if (ownIds.includes(stage.id)) continue;
        if (!block.text.includes(stage.title)) {
          problems.push(`without JavaScript the path "${path.id}" does not name the stage "${stage.id}" it shares`);
        }
      }
    }
    for (const object of model.objects) {
      if (!family.objects.includes(object.id)) problems.push(`without JavaScript the text leaves out the object "${object.id}"`);
    }
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
    if (got.family !== want.family) problems.push(`the link ${link} opens the family ${got.family ?? 'none'}, not ${want.family}`);
    if (got.path !== want.path) problems.push(`the link ${link} opens the path ${got.path ?? 'none'}, not ${want.path}`);
    if (got.stage !== want.stage) problems.push(`the link ${link} opens the stage ${got.stage ?? 'none'}, not ${want.stage}`);
    if (got.object !== want.object) problems.push(`the link ${link} selects ${got.object ?? 'nothing'}, not ${want.object ?? 'nothing'}`);
  }
  return problems;
}

// urlProblems judges the address the explorer wrote: a stage chosen on the
// default path of the default family writes no path or family parameter, so
// links made before there were paths or families keep their meaning, a stage
// on another path names it, and a stage in another family names the family.
export function urlProblems(reading) {
  const problems = [];
  const plain = new URLSearchParams(reading.defaultSearch);
  if (plain.has('lx-path')) problems.push(`a stage chosen on the default path writes ${reading.defaultSearch}, with a path parameter`);
  if (plain.has('lx-family')) problems.push(`a stage chosen in the default family writes ${reading.defaultSearch}, with a family parameter`);
  if (!plain.has('lx-stage')) problems.push(`a stage chosen on the default path writes ${reading.defaultSearch || 'nothing'}, with no stage`);
  const branch = new URLSearchParams(reading.branchSearch);
  if (branch.get('lx-path') !== reading.branchPath) {
    problems.push(`choosing the path ${reading.branchPath} writes ${reading.branchSearch || 'nothing'}, not lx-path=${reading.branchPath}`);
  }
  const other = new URLSearchParams(reading.familySearch);
  if (other.get('lx-family') !== reading.family) {
    problems.push(`a stage chosen in the family ${reading.family} writes ${reading.familySearch || 'nothing'}, not lx-family=${reading.family}`);
  }
  if (!other.has('lx-stage')) problems.push(`a stage chosen in the family ${reading.family} writes ${reading.familySearch || 'nothing'}, with no stage`);
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
    id: 'one',
    label: 'One',
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
  // A second family, with a path that turns off another path's own stage.
  const other = {
    id: 'two',
    label: 'Two',
    stages: [stage('b-one', 'Begin', ['x']), stage('b-two', 'Finish', ['x', 'y'])],
    paths: [
      { id: 'b-main', label: 'Main' },
      { id: 'b-turn', label: 'Turn', turnsAfter: 'b-one', stages: [stage('b-turn-a', 'Swerve', ['y']), stage('b-turn-b', 'Settle', ['x'])] },
      { id: 'b-nested', label: 'Nested', turnsAfter: 'b-turn-a', stages: [stage('b-nested-a', 'Deeper', ['y'])] },
    ],
    objects: [{ id: 'x' }, { id: 'y' }],
    relations: [{ from: 'x', to: 'y', type: 'owns' }],
  };
  const models = [model, other];
  const fail = (what, found) => {
    throw new Error(`${what}: ${JSON.stringify(found)}`);
  };
  const refuses = (name, found, want) => {
    if (!found.some((problem) => problem.includes(want))) fail(`a check read ${name} as`, found);
  };
  const withPath = (of, index, change) => ({
    ...of,
    paths: of.paths.map((path, at) => (at === index ? { ...path, ...change } : path)),
  });

  const walks = [...model.paths.map((path) => walkIds(path, model)), ...other.paths.map((path) => walkIds(path, other))].map(
    (walk) => walk.join(' '),
  );
  if (
    walks.join(' | ') !==
    'one two three | one side-a three | one two end-a | b-one b-two | b-one b-turn-a b-turn-b | b-one b-turn-a b-nested-a'
  ) {
    fail('walkOf read the paths as', walks);
  }

  const a = expectedRelations('a', model);
  if (a.listed.join(' ') !== 'owns:b references:b references:c' || a.related.join(' ') !== 'b c') {
    fail('expectedRelations read a as', a);
  }

  if (familiesProblems(models).length !== 0) fail('familiesProblems refused sound families', familiesProblems(models));
  refuses('a stage naming a missing object', modelProblems({ ...model, stages: [{ ...model.stages[0], objects: ['z'] }, ...model.stages.slice(1)] }), 'names the object "z"');
  refuses('a branch stage naming a missing object', modelProblems(withPath(model, 1, { stages: [stage('side-a', 'Aside', ['z'])] })), 'names the object "z"');
  refuses('a relation to nowhere', modelProblems({ ...model, relations: [...model.relations, { from: 'a', to: 'z', type: 'owns' }] }), 'names "z"');
  refuses('a stage id used twice', modelProblems({ ...model, stages: [model.stages[0], model.stages[0], model.stages[2]] }), 'used twice');
  refuses('a branch stage that takes a successful id', modelProblems(withPath(model, 1, { stages: [stage('two', 'Aside', ['b'])] })), '"two" is used twice');
  refuses('a lead who does nothing', modelProblems({ ...model, stages: [{ ...model.stages[0], lead: 'executor' }, ...model.stages.slice(1)] }), 'who does nothing');
  refuses('an object related to nothing', modelProblems({ ...model, objects: [...model.objects, { id: 'd' }] }), '"d" is related to nothing');
  refuses('an empty scenario', modelProblems({ ...model, stages: [] }), 'no stages');
  refuses('no paths at all', modelProblems({ ...model, paths: [] }), 'no paths');
  refuses('a default path that turns', modelProblems(withPath(model, 0, { turnsAfter: 'one' })), 'turns off itself');
  refuses('a turn at no stage', modelProblems(withPath(model, 1, { turnsAfter: 'zero' })), 'turns after "zero", which is not');
  refuses('a rejoin at no stage', modelProblems(withPath(model, 1, { rejoins: 'zero' })), 'rejoins at "zero", which is not');
  refuses('a rejoin before the turn', modelProblems(withPath(model, 2, { rejoins: 'one' })), 'before the stage it turns after');
  refuses('a path with no stage of its own', modelProblems(withPath(model, 1, { stages: [] })), 'no stage of its own');
  refuses('a path label used twice', modelProblems(withPath(model, 2, { label: 'Side' })), 'path label id "Side"');
  refuses('a path that turns at its own stage', modelProblems(withPath(other, 2, { turnsAfter: 'b-nested-a' })), 'one of its own stages');
  refuses(
    'two paths that turn off each other',
    modelProblems(withPath(other, 1, { turnsAfter: 'b-nested-a' })),
    'turn off each other',
  );
  refuses(
    'a nested rejoin the path it turns off does not walk',
    modelProblems(withPath(other, 2, { rejoins: 'b-two' })),
    'which is not a stage of the path "b-turn" it turns off',
  );
  refuses('a nested rejoin before the turn', modelProblems(withPath(other, 2, { rejoins: 'b-one' })), 'before the stage it turns after');
  refuses('no family at all', familiesProblems([]), 'no family');
  refuses('a family id used twice', familiesProblems([model, { ...other, id: 'one' }]), 'family "one" is used twice');
  refuses('a family label used twice', familiesProblems([model, { ...other, label: 'One' }]), 'family label "One" is used twice');
  refuses(
    'a stage id two families share',
    familiesProblems([model, { ...other, stages: [stage('one', 'Begin', ['x']), other.stages[1]] }]),
    'stage id "one" is used in the families one and two',
  );
  refuses('a path id two families share', familiesProblems([model, withPath(other, 0, { id: 'main' })]), 'path id "main" is used in the families');
  refuses(
    'an object id two families share',
    familiesProblems([model, { ...other, objects: [{ id: 'a' }, { id: 'y' }], relations: [{ from: 'a', to: 'y', type: 'owns' }] }]),
    'object id "a" is used in the families',
  );
  refuses('a family whose own model is unsound', familiesProblems([model, { ...other, stages: [] }]), 'the family "two": the scenario has no stages');

  const goodRender = {
    present: true,
    ready: true,
    appHidden: false,
    textShown: false,
    familyButtons: 2,
    views: 2,
    visibleViews: 1,
    panels: 10,
    visiblePanels: 1,
    lists: 6,
    visibleLists: 1,
  };
  if (renderProblems(goodRender, models).length !== 0) fail('renderProblems refused a page that holds', renderProblems(goodRender, models));
  for (const [name, spoil, want] of [
    ['a page with no explorer', { present: false }, 'carries no lifecycle explorer'],
    ['a script that never ran', { ready: false }, 'never ran'],
    ['an interactive view left hidden', { appHidden: true }, 'still hidden'],
    ['the text left beside it', { textShown: true }, 'still on screen'],
    ['a family with no button', { familyButtons: 1 }, '1 family buttons'],
    ['a family with no view', { views: 1 }, '1 family views'],
    ['two family views shown at once', { visibleViews: 2 }, '2 family views are shown'],
    ['a stage panel missing', { panels: 9 }, '9 stage panels'],
    ['two panels shown at once', { visiblePanels: 2 }, '2 stage panels are shown'],
    ['a path with no list of steps', { lists: 5 }, '5 lists of steps'],
    ['two lists shown at once', { visibleLists: 2 }, '2 lists of steps are shown'],
  ]) {
    refuses(name, renderProblems({ ...goodRender, ...spoil }, models), want);
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
  const nestedKeys = [
    { path: 'b-main', chosen: true, tabbed: true, start: 'b-one', arrows: ['b-two'], focus: ['b-two'], home: 'b-one', end: 'b-two', next: ['b-two'] },
    {
      path: 'b-turn',
      chosen: true,
      tabbed: true,
      start: 'b-one',
      arrows: ['b-turn-a', 'b-turn-b'],
      focus: ['b-turn-a', 'b-turn-b'],
      home: 'b-one',
      end: 'b-turn-b',
      next: ['b-turn-a', 'b-turn-b'],
      turned: { path: 'b-turn', stage: 'b-turn-a', focus: 'b-turn-a' },
    },
    {
      path: 'b-nested',
      chosen: true,
      tabbed: true,
      start: 'b-one',
      arrows: ['b-turn-a', 'b-nested-a'],
      focus: ['b-turn-a', 'b-nested-a'],
      home: 'b-one',
      end: 'b-nested-a',
      next: ['b-turn-a', 'b-nested-a'],
      turned: { path: 'b-nested', stage: 'b-nested-a', focus: 'b-nested-a' },
    },
  ];
  if (keyboardProblems(nestedKeys, other).length !== 0) fail('keyboardProblems refused nested keys that hold', keyboardProblems(nestedKeys, other));
  {
    const bad = structuredClone(nestedKeys);
    Object.assign(bad[2], { turned: { path: 'b-turn', stage: 'b-turn-b', focus: 'b-turn-b' } });
    refuses('a nested turn that stays on the path it turns off', keyboardProblems(bad, other), 'reach b-turn-b on the path b-turn');
  }
  {
    const bad = structuredClone(nestedKeys);
    Object.assign(bad[2], { arrows: ['b-turn-a', 'b-turn-b'], focus: ['b-turn-a', 'b-turn-b'], end: 'b-turn-b', next: ['b-turn-a', 'b-turn-b'] });
    refuses('a nested path that walks its parent instead', keyboardProblems(bad, other), 'never reach b-nested-a');
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
  refuses('objects never selected', selectionProblems(goodSelection.slice(0, 1), model), '1 of 3 objects of the family "one"');

  // Three switches: to two, back to one from where one was left, and to two
  // again from where two was left.
  const fresh = (family, path, stage, search) => ({ family, view: family, views: 1, path, stage, object: null, search });
  const goodSwitches = [
    { from: 'one at end-a', to: 'two', left: null, got: fresh('two', 'b-main', 'b-one', '?lx-family=two&lx-stage=b-one') },
    {
      from: 'two at b-nested-a',
      to: 'one',
      left: { path: 'end', stage: 'end-a', object: 'c' },
      got: fresh('one', 'main', 'one', '?lx-stage=one'),
    },
    {
      from: 'one at end-a',
      to: 'two',
      left: { path: 'b-nested', stage: 'b-nested-a', object: 'y' },
      got: fresh('two', 'b-main', 'b-one', '?lx-family=two&lx-stage=b-one'),
    },
  ];
  if (familyProblems(goodSwitches, models).length !== 0) fail('familyProblems refused switches that hold', familyProblems(goodSwitches, models));
  for (const [name, index, spoil, want] of [
    ['a switch that keeps the path', 1, { path: 'end' }, 'opens the path end, not its default main'],
    ['a switch that keeps the stage', 2, { stage: 'b-nested-a', path: 'b-main' }, 'opens the stage b-nested-a, not its first b-one'],
    ['a switch that keeps the object', 1, { object: 'c' }, 'keeps c selected'],
    ['a switch that marks one family and shows another', 0, { view: 'one' }, 'shows the view of one'],
    ['a switch that shows two views', 0, { views: 2 }, 'shows 2 family views'],
    ['a switch that writes the path left behind', 1, { search: '?lx-path=end&lx-stage=one' }, 'with a path parameter'],
    ['a switch that writes the object left behind', 2, { search: '?lx-family=two&lx-stage=b-one&lx-object=y' }, 'with an object parameter'],
    ['a switch to another family that does not name it', 0, { search: '?lx-stage=b-one' }, 'not lx-family=two'],
    ['a switch to the default family that names it', 1, { search: '?lx-family=one&lx-stage=one' }, 'with a family parameter'],
    ['a switch that writes the stage left behind', 1, { search: '?lx-stage=end-a' }, 'not lx-stage=one'],
  ]) {
    const bad = structuredClone(goodSwitches);
    Object.assign(bad[index].got, spoil);
    refuses(name, familyProblems(bad, models), want);
  }
  refuses('no switch at all', familyProblems([], models), 'no family was switched to');
  refuses('a family never switched to', familyProblems(goodSwitches.slice(0, 1), models), 'the family "one" was never switched to');
  refuses(
    'a family only ever switched to fresh',
    familyProblems(goodSwitches.slice(0, 2), models),
    'no switch came back to the family "two"',
  );
  {
    const bad = structuredClone(goodSwitches);
    bad[1].left = { path: 'main', stage: 'two', object: 'a' };
    refuses('a family left at its default path', familyProblems(bad, models), 'no switch came back to the family "one"');
  }
  {
    const bad = structuredClone(goodSwitches);
    bad[2].left = { path: 'b-nested', stage: 'b-nested-a', object: null };
    refuses('a family left with nothing selected', familyProblems(bad, models), 'no switch came back to the family "two"');
  }

  const goodText = {
    present: true,
    appHidden: true,
    textShown: true,
    families: [
      {
        id: 'one',
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
      },
      {
        id: 'two',
        paths: [
          { id: 'b-main', text: 'Begin. cmd b-one Finish. cmd b-two', stages: [{ id: 'b-one', text: 'Begin. cmd b-one' }, { id: 'b-two', text: 'Finish. cmd b-two' }] },
          {
            id: 'b-turn',
            text: 'Stage 1 is Begin. Swerve. cmd b-turn-a Settle. cmd b-turn-b',
            stages: [
              { id: 'b-turn-a', text: 'Swerve. cmd b-turn-a' },
              { id: 'b-turn-b', text: 'Settle. cmd b-turn-b' },
            ],
          },
          { id: 'b-nested', text: 'Stages 1 to 2: Begin, Swerve. Deeper. cmd b-nested-a', stages: [{ id: 'b-nested-a', text: 'Deeper. cmd b-nested-a' }] },
        ],
        objects: ['x', 'y'],
      },
    ],
  };
  if (textProblems(goodText, models).length !== 0) fail('textProblems refused a text that holds', textProblems(goodText, models));
  const spoilPath = (family, index, change) => ({
    families: goodText.families.map((one, at) =>
      at === family ? { ...one, paths: one.paths.map((path, where) => (where === index ? { ...path, ...change } : path)) } : one,
    ),
  });
  const firstPaths = goodText.families[0].paths;
  for (const [name, spoil, want] of [
    ['a page with no explorer', { present: false }, 'carries no lifecycle explorer'],
    ['an interactive view shown without its script', { appHidden: false }, 'with no script to drive it'],
    ['a text hidden without a script', { textShown: false }, 'text version is not on screen'],
    ['a family dropped', { families: goodText.families.slice(0, 1) }, 'leaves out the family "two"'],
    ['a path dropped', { families: [{ ...goodText.families[0], paths: firstPaths.slice(0, 2) }, goodText.families[1]] }, 'leaves out the path "end"'],
    [
      'a nested path dropped',
      { families: [goodText.families[0], { ...goodText.families[1], paths: goodText.families[1].paths.slice(0, 2) }] },
      'leaves out the path "b-nested"',
    ],
    ['a stage dropped', spoilPath(0, 0, { stages: firstPaths[0].stages.slice(0, 2) }), 'leaves out the stage "three"'],
    ['a branch stage dropped', spoilPath(0, 1, { stages: [] }), 'path "side" leaves out the stage "side-a"'],
    ['a stage without its title', spoilPath(0, 0, { stages: [{ id: 'one', text: 'cmd one' }, ...firstPaths[0].stages.slice(1)] }), 'lacks its title'],
    ['a stage without its command', spoilPath(0, 2, { stages: [{ id: 'end-a', text: 'Ending.' }] }), 'lacks the command "cmd end-a"'],
    [
      'stages out of order',
      spoilPath(0, 0, { stages: [firstPaths[0].stages[1], firstPaths[0].stages[0], firstPaths[0].stages[2]] }),
      'path "main" are out of order',
    ],
    ['a shared stage left unnamed', spoilPath(0, 1, { text: 'Aside. cmd side-a Then Third.' }), 'does not name the stage "one"'],
    [
      'a stage of the path a nested path turns off left unnamed',
      spoilPath(1, 2, { text: 'Stages 1 to 2: Begin. Deeper. cmd b-nested-a' }),
      'path "b-nested" does not name the stage "b-turn-a"',
    ],
    ['an object dropped', { families: [{ ...goodText.families[0], objects: ['a', 'b'] }, goodText.families[1]] }, 'leaves out the object "c"'],
    ['an object of the second family dropped', { families: [goodText.families[0], { ...goodText.families[1], objects: ['x'] }] }, 'leaves out the object "y"'],
  ]) {
    refuses(name, textProblems({ ...goodText, ...spoil }, models), want);
  }

  const fits = { what: 'at rest', viewport: 390, page: 390, explorer: 340, explorerBox: 340 };
  if (overflowProblems([fits]).length !== 0) fail('overflowProblems refused a page that fits', overflowProblems([fits]));
  refuses('a page wider than the viewport', overflowProblems([{ ...fits, page: 520 }]), 'the page is 520px wide');
  refuses('an explorer that scrolls', overflowProblems([{ ...fits, explorer: 410 }]), 'the explorer scrolls');
  refuses('no reading at all', overflowProblems([]), 'no narrow-screen reading');

  const link = { link: '?lx-family=two&lx-path=b-nested&lx-stage=b-nested-a&lx-object=y', want: { family: 'two', path: 'b-nested', stage: 'b-nested-a', object: 'y' } };
  const held = [{ ...link, got: link.want }];
  if (deepLinkProblems(held).length !== 0) fail('deepLinkProblems refused a link that holds', deepLinkProblems(held));
  refuses('a link whose family was dropped', deepLinkProblems([{ ...link, got: { ...link.want, family: 'one' } }]), 'opens the family one');
  refuses('a link whose path was dropped', deepLinkProblems([{ ...link, got: { ...link.want, path: 'b-main' } }]), 'opens the path b-main');
  refuses('a link whose stage was dropped', deepLinkProblems([{ ...link, got: { ...link.want, stage: 'b-one' } }]), 'opens the stage b-one');
  refuses('a link whose object was dropped', deepLinkProblems([{ ...link, got: { ...link.want, object: null } }]), 'selects nothing');
  refuses('no link opened', deepLinkProblems([]), 'no shared link');

  const goodUrl = {
    defaultSearch: '?lx-stage=two',
    branchSearch: '?lx-path=side&lx-stage=side-a',
    branchPath: 'side',
    familySearch: '?lx-family=two&lx-stage=b-two',
    family: 'two',
  };
  if (urlProblems(goodUrl).length !== 0) fail('urlProblems refused an address that holds', urlProblems(goodUrl));
  refuses('a default path that writes itself', urlProblems({ ...goodUrl, defaultSearch: '?lx-path=main&lx-stage=two' }), 'with a path parameter');
  refuses('a default family that writes itself', urlProblems({ ...goodUrl, defaultSearch: '?lx-family=one&lx-stage=two' }), 'with a family parameter');
  refuses('a stage that is not written', urlProblems({ ...goodUrl, defaultSearch: '' }), 'with no stage');
  refuses('a branch path that is not written', urlProblems({ ...goodUrl, branchSearch: '?lx-stage=side-a' }), 'not lx-path=side');
  refuses('another family that is not written', urlProblems({ ...goodUrl, familySearch: '?lx-stage=b-two' }), 'not lx-family=two');
  refuses('another family’s stage that is not written', urlProblems({ ...goodUrl, familySearch: '?lx-family=two' }), 'family two writes ?lx-family=two, with no stage');

  console.log(
    'check-lifecycle-explorer.mjs --selftest: OK (the families, their models and their paths, nested turns, the ' +
      'rendered view, the keyboard on every path and at every turn, each selection, the family switch and its reset, ' +
      'the text without JavaScript, shared links, the address, and the narrow layout)',
  );
}

const ROOT = '#lifecycle-explorer';
const VIEW = `${ROOT} [data-lx-family-view]:not([hidden])`;
const VISIBLE_STEP = `${VIEW} [data-lx-steps]:not([hidden]) [data-lx-step]`;

const readRender = (page) =>
  page.evaluate(() => {
    const root = document.getElementById('lifecycle-explorer');
    if (!root) return { present: false };
    const visible = (element) => element && element.getClientRects().length > 0 && getComputedStyle(element).display !== 'none';
    const views = [...root.querySelectorAll('[data-lx-family-view]')];
    const panels = [...root.querySelectorAll('[data-lx-stage-panel]')];
    const lists = [...root.querySelectorAll('[data-lx-steps]')];
    return {
      present: true,
      ready: root.hasAttribute('data-lx-ready'),
      appHidden: !visible(root.querySelector('[data-lx-app]')),
      textShown: visible(root.querySelector('[data-lx-text]')),
      familyButtons: root.querySelectorAll('[data-lx-family]').length,
      views: views.length,
      visibleViews: views.filter((view) => visible(view)).length,
      panels: panels.length,
      visiblePanels: panels.filter((panel) => visible(panel)).length,
      lists: lists.length,
      visibleLists: lists.filter((list) => visible(list)).length,
    };
  });

// Where the explorer stands: the family its bar marks and the view on screen,
// and within that view the path, the stage, the panel shown, the focus and
// the selection, with the address it wrote.
const readStage = (page) =>
  page.evaluate(() => {
    const root = document.getElementById('lifecycle-explorer');
    const views = [...(root?.querySelectorAll('[data-lx-family-view]') ?? [])].filter((view) => !view.hidden);
    const view = views[0];
    const list = view?.querySelector('[data-lx-steps]:not([hidden])');
    return {
      family: root?.querySelector('[data-lx-family][aria-pressed="true"]')?.dataset.lxFamily ?? null,
      view: view?.dataset.lxFamilyView ?? null,
      views: views.length,
      path: view?.querySelector('[data-lx-path][aria-pressed="true"]')?.dataset.lxPath ?? null,
      stage: list?.querySelector('[data-lx-step][aria-current="step"]')?.dataset.lxStep ?? null,
      shown: [...(view?.querySelectorAll('[data-lx-stage-panel]') ?? [])].find((panel) => !panel.hidden)?.dataset.lxStagePanel ?? null,
      focus: document.activeElement?.dataset?.lxStep ?? null,
      object: view?.querySelector('[data-lx-object][aria-pressed="true"]')?.dataset.lxObject ?? null,
      search: location.search,
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

const pageErrors = new WeakMap();

// What went wrong on a page since it was first opened: script errors and a
// crash.
function errorsOf(page) {
  let errors = pageErrors.get(page);
  if (!errors) {
    errors = [];
    pageErrors.set(page, errors);
    page.on('pageerror', (error) => errors.push(error.message));
    page.on('crash', () => errors.push('the page crashed'));
  }
  return errors;
}

// Open the page and, where scripts run, wait for the explorer to start. A
// page that did not load, or whose script stopped, fails here and says what it
// saw, rather than as a timeout on the first control a later step looks for.
async function open(page, url, { scripted = true } = {}) {
  const errors = errorsOf(page);
  const response = await page.goto(url, { waitUntil: 'load' });
  if (response && !response.ok()) throw new Error(`${url} answered ${response.status()}`);
  if (!scripted) return;
  try {
    await page.locator(`${ROOT} [data-lx-app]:not([hidden])`).waitFor({ state: 'attached', timeout: 15_000 });
  } catch {
    const seen = await page
      .evaluate(() => ({ title: document.title, present: Boolean(document.getElementById('lifecycle-explorer')) }))
      .catch(() => null);
    const what = seen ? `the page "${seen.title}", explorer ${seen.present ? 'present' : 'absent'}` : 'no answer from the page';
    throw new Error(`the explorer did not start at ${url}: ${what}${errors.length ? `; ${errors.join('; ')}` : ''}`);
  }
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

const chooseFamily = (page, model) => page.locator(`${ROOT} [data-lx-family="${model.id}"]`).click();

// The path a path turns off, chosen from the bar when it is not the default,
// so that the stage it turns after is on screen.
async function standOnParent(page, path, model) {
  const parent = parentOf(path, model);
  if (parent && parent.id !== model.paths[0].id) await page.locator(`${VIEW} [data-lx-path="${parent.id}"]`).click();
}

// One path, by keyboard alone from the heading above the explorer: choose its
// family and then the path, reach its stage list, walk it with the arrow
// keys, Home, End and Next, and take its turn at the stage it turns after, on
// the path it turns off.
async function walkPath(page, url, path, model) {
  await open(page, url);
  const length = walkIds(path, model).length;
  await focusHeading(page);
  const family = await tabTo(page, `${ROOT} [data-lx-family="${model.id}"]`, 20);
  if (family) await page.keyboard.press('Enter');
  const reached = family && (await tabTo(page, `${VIEW} [data-lx-path="${path.id}"]`, 60));
  if (reached) await page.keyboard.press('Enter');
  const now = await readStage(page);
  const chosen = reached && now.family === model.id && now.path === path.id;
  const tabbed = await tabTo(page, `${VISIBLE_STEP}[aria-current="step"]`, 20);
  if (!tabbed) await page.locator(`${VISIBLE_STEP}[aria-current="step"]`).focus();
  await page.keyboard.press('Home');
  const start = (await readStage(page)).stage;
  const arrows = [];
  const focus = [];
  for (let index = 1; index < length; index += 1) {
    await page.keyboard.press('ArrowRight');
    const after = await readStage(page);
    arrows.push(after.stage === after.shown ? after.stage : `${after.stage}/${after.shown}`);
    focus.push(after.focus);
  }
  await page.keyboard.press('Home');
  const home = (await readStage(page)).stage;
  await page.keyboard.press('End');
  const end = (await readStage(page)).stage;
  await page.keyboard.press('Home');

  const next = [];
  await page.locator(`${VIEW} [data-lx-next]`).focus();
  for (let index = 1; index < length; index += 1) {
    await page.keyboard.press('Enter');
    next.push((await readStage(page)).stage);
  }

  let turned;
  if (path.turnsAfter) {
    await open(page, url);
    await chooseFamily(page, model);
    await standOnParent(page, path, model);
    await page.locator(`${VISIBLE_STEP}[data-lx-step="${path.turnsAfter}"]`).click();
    await page.locator(`${VISIBLE_STEP}[data-lx-step="${path.turnsAfter}"]`).focus();
    turned = null;
    if (await tabTo(page, `${VIEW} [data-lx-choose="${path.id}"]`, 60)) {
      await page.keyboard.press('Enter');
      const after = await readStage(page);
      turned = { path: after.path, stage: after.stage, focus: after.focus };
    }
  }
  return { family: model.id, path: path.id, chosen, tabbed, start, arrows, focus, home, end, next, turned };
}

// A state of one family that is default in nothing: its last path, at its
// last stage, with its last object selected.
async function unsettle(page, model) {
  await page.locator(`${VIEW} [data-lx-path="${model.paths.at(-1).id}"]`).click();
  await page.locator(VISIBLE_STEP).last().click();
  await page.locator(`${VIEW} [data-lx-object]`).last().click();
  return readStage(page);
}

const describe = (state) => `${state.family} on ${state.path} at ${state.stage} with ${state.object ?? 'nothing'} selected`;

// Switch through every family twice, leaving each in a state that is default
// in nothing before the next switch, so that the second visit to each family
// shows whether it was reset.
async function measureSwitches(page, url, models) {
  await open(page, url);
  const switches = [];
  const left = new Map();
  const order = [...models, ...models];
  let before = await unsettle(page, order[0]);
  left.set(order[0].id, before);
  for (const model of order.slice(1)) {
    await chooseFamily(page, model);
    const got = await readStage(page);
    switches.push({ from: describe(before), to: model.id, left: left.get(model.id) ?? null, got });
    before = await unsettle(page, model);
    left.set(model.id, before);
  }
  return switches;
}

// The links a reader may be handed: one from before there were paths or
// families, one per other path of each family, one naming another path's
// stage alone, a stage, a path or an object of another family named without
// its family, and links that name one family and another's values.
function linksFor(models) {
  const [first, ...others] = models;
  const pick = (model, index) => model.stages[Math.min(index, model.stages.length - 1)].id;
  const links = [
    {
      link: `?lx-stage=${pick(first, 3)}&lx-object=${first.objects.at(-1).id}`,
      want: { family: first.id, path: first.paths[0].id, stage: pick(first, 3), object: first.objects.at(-1).id },
    },
  ];
  for (const model of models) {
    const named = model.id === first.id ? '' : `lx-family=${model.id}&`;
    for (const path of model.paths.slice(1)) {
      links.push({
        link: `?${named}lx-path=${path.id}&lx-stage=${path.stages.at(-1).id}&lx-object=${model.objects[0].id}`,
        want: { family: model.id, path: path.id, stage: path.stages.at(-1).id, object: model.objects[0].id },
      });
    }
    if (model.paths[1]) {
      links.push({
        link: `?lx-stage=${model.paths[1].stages[0].id}`,
        want: { family: model.id, path: model.paths[1].id, stage: model.paths[1].stages[0].id, object: null },
      });
    }
    for (const path of model.paths.slice(1).filter((one) => parentOf(one, model).id !== model.paths[0].id)) {
      links.push({ link: `?lx-path=${path.id}`, want: { family: model.id, path: path.id, stage: model.stages[0].id, object: null } });
    }
  }
  for (const model of others) {
    links.push(
      { link: `?lx-stage=${pick(model, 2)}`, want: { family: model.id, path: model.paths[0].id, stage: pick(model, 2), object: null } },
      { link: `?lx-object=${model.objects.at(-1).id}`, want: { family: model.id, path: model.paths[0].id, stage: model.stages[0].id, object: model.objects.at(-1).id } },
      {
        link: `?lx-family=${model.id}&lx-path=${first.paths.at(-1).id}&lx-stage=${pick(first, 2)}&lx-object=${first.objects[0].id}`,
        want: { family: model.id, path: model.paths[0].id, stage: model.stages[0].id, object: null },
      },
      {
        link: `?lx-family=${first.id}&lx-path=${model.paths.at(-1).id}&lx-stage=${pick(model, 2)}&lx-object=${model.objects[0].id}`,
        want: { family: first.id, path: first.paths[0].id, stage: first.stages[0].id, object: null },
      },
    );
  }
  return links;
}

// measure drives the page and reports readings. Nothing is judged here: the
// functions above own that, and they are the ones the self-test exercises.
async function measure(browser, url, models) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  const thrown = [];
  page.on('pageerror', (error) => thrown.push(String(error)));
  try {
    await open(page, url);
    const render = await readRender(page);
    if (!render.present || !render.ready) return { render, thrown };

    const keyboard = [];
    for (const model of models) {
      for (const path of model.paths) keyboard.push(await walkPath(page, url, path, model));
    }

    // Each object of each family, selected from its node.
    const selection = [];
    for (const model of models) {
      await open(page, url);
      await chooseFamily(page, model);
      for (const id of await page.$$eval(`${VIEW} [data-lx-object]`, (nodes) => nodes.map((node) => node.dataset.lxObject))) {
        await page.locator(`${VIEW} [data-lx-object="${id}"]`).click();
        selection.push(
          await page.evaluate(
            ({ selected, family }) => {
              const view = document.querySelector(`#lifecycle-explorer [data-lx-family-view="${family}"]`);
              const panel = [...view.querySelectorAll('[data-lx-object-panel]')].find((one) => !one.hidden);
              const listed = panel
                ? [...panel.querySelectorAll('.lx__relations li')].map(
                    (item) => `${item.dataset.type}:${item.querySelector('[data-lx-select]')?.dataset.lxSelect}`,
                  )
                : [];
              return {
                family,
                id: selected,
                panel: panel?.dataset.lxObjectPanel ?? null,
                listed,
                marked: [...view.querySelectorAll('[data-lx-object][data-related]')].map((node) => node.dataset.lxObject),
                pressed: view.querySelector(`[data-lx-object="${selected}"]`)?.getAttribute('aria-pressed') === 'true',
              };
            },
            { selected: id, family: model.id },
          ),
        );
      }
    }

    const switches = await measureSwitches(page, url, models);

    // What the explorer writes: a stage on the default path, then a branch,
    // then a stage in the second family.
    await open(page, url);
    await page.locator(VISIBLE_STEP).nth(1).click();
    const defaultSearch = await page.evaluate(() => location.search);
    const branchPath = models[0].paths[1]?.id ?? null;
    if (branchPath) await page.locator(`${VIEW} [data-lx-path="${branchPath}"]`).click();
    const branchSearch = await page.evaluate(() => location.search);
    const family = models[1]?.id ?? null;
    if (family) {
      await chooseFamily(page, models[1]);
      await page.locator(VISIBLE_STEP).nth(1).click();
    }
    const familySearch = await page.evaluate(() => location.search);

    const deepLinks = [];
    for (const one of linksFor(models)) {
      await open(page, `${url}${one.link}`);
      const got = await readStage(page);
      deepLinks.push({ ...one, got: { family: got.family, path: got.path, stage: got.stage, object: got.object } });
    }

    return {
      render,
      keyboard,
      selection,
      switches,
      url: { defaultSearch, branchSearch, branchPath, familySearch, family },
      deepLinks,
      thrown,
    };
  } finally {
    await context.close();
  }
}

// The narrow layouts, in each family: at rest, at each path's turn with its
// choices on screen, at every stage of every path once, with every object
// open, and without JavaScript, where the text is what is on screen. One
// long word in one panel is enough to push a phone column sideways, so no
// panel is left unread.
async function measureNarrow(browser, url, models) {
  const readings = [];
  for (const viewport of NARROW) {
    for (const javaScriptEnabled of [true, false]) {
      const context = await browser.newContext({ viewport, javaScriptEnabled });
      const page = await context.newPage();
      const at = `${viewport.width}px`;
      try {
        await open(page, url, { scripted: javaScriptEnabled });
        if (!javaScriptEnabled) {
          readings.push(await readOverflow(page, `${at}, without JavaScript`));
          continue;
        }
        if ((await page.locator(VISIBLE_STEP).count()) === 0) {
          readings.push(await readOverflow(page, `${at}, at rest`));
          continue;
        }
        for (const model of models) {
          await chooseFamily(page, model);
          readings.push(await readOverflow(page, `${at}, ${model.id} at rest`));
          const seen = new Set();
          for (const path of model.paths) {
            await page.locator(`${VIEW} [data-lx-path="${model.paths[0].id}"]`).click();
            if (path.turnsAfter) {
              await standOnParent(page, path, model);
              await page.locator(`${VISIBLE_STEP}[data-lx-step="${path.turnsAfter}"]`).click();
              readings.push(await readOverflow(page, `${at}, at the turn to ${path.id}`));
            }
            await page.locator(`${VIEW} [data-lx-path="${path.id}"]`).click();
            for (const stage of walkIds(path, model).filter((id) => !seen.has(id))) {
              seen.add(stage);
              await page.locator(`${VISIBLE_STEP}[data-lx-step="${stage}"]`).click();
              readings.push(await readOverflow(page, `${at}, on ${path.id} at ${stage}`));
            }
          }
          for (const object of model.objects) {
            await page.locator(`${VIEW} [data-lx-object="${object.id}"]`).click();
            readings.push(await readOverflow(page, `${at}, with ${object.id} selected`));
            await page.locator(`${VIEW} [data-lx-object="${object.id}"]`).click();
          }
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
    await open(page, url, { scripted: false });
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
        families: [...(text?.querySelectorAll('[data-lx-text-family]') ?? [])].map((family) => ({
          id: family.dataset.lxTextFamily,
          paths: [...family.querySelectorAll('[data-lx-text-path]')].map((block) => ({
            id: block.dataset.lxTextPath,
            text: flat(block),
            stages: [...block.querySelectorAll('[data-lx-text-stage]')].map((item) => ({
              id: item.dataset.lxTextStage,
              text: flat(item),
            })),
          })),
          objects: [...family.querySelectorAll('[data-lx-text-object]')].map((item) => item.dataset.lxTextObject),
        })),
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

  const models = families.map(familyModel);
  const problems = familiesProblems(models);
  if (models[0]?.id !== defaultFamily) problems.push(`the default family is "${defaultFamily}", not the first family "${models[0]?.id}"`);

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
    problems.push(...textProblems(quiet, models));
    if (quiet.present) {
      const measured = await measure(browser, url, models);
      problems.push(...renderProblems(measured.render, models));
      if (measured.keyboard) {
        for (const model of models) {
          problems.push(...keyboardProblems(measured.keyboard.filter((reading) => reading.family === model.id), model));
          problems.push(...selectionProblems(measured.selection.filter((reading) => reading.family === model.id), model));
        }
        problems.push(...familyProblems(measured.switches, models));
        problems.push(...urlProblems(measured.url));
        problems.push(...deepLinkProblems(measured.deepLinks));
      }
      if (measured.thrown.length > 0) problems.push(`the explorer threw: ${measured.thrown.join('; ')}`);
      problems.push(...overflowProblems(await measureNarrow(browser, url, models)));
    }
  } finally {
    await browser.close();
    server.close();
  }

  if (problems.length > 0) {
    console.error(`check-lifecycle-explorer.mjs: ${problems.length} problem(s):\n- ${problems.join('\n- ')}`);
    process.exit(1);
  }
  const count = (what) => models.reduce((sum, model) => sum + what(model), 0);
  console.log(
    `check-lifecycle-explorer.mjs: OK (${models.length} families, ${count((model) => model.paths.length)} paths and ` +
      `${count((model) => allStages(model).length)} stages by keyboard, every turn, the family switch, ` +
      `${count((model) => model.objects.length)} objects and ${count((model) => model.relations.length)} relationships, ` +
      `the text without JavaScript, shared links, and ${NARROW.map((one) => `${one.width}px`).join(' and ')} wide)`,
  );
}

await main();
