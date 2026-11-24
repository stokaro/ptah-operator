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
//     sideways, at any stage of any path or with any object selected;
//   - the audit view covers every evidence source, loss, identity and gap
//     #817 asks for, each source says what it records, what it cannot
//     establish, how it joins the others, what reads it and what is left of
//     it after every loss, and every identity and gap names sources that
//     exist;
//   - no command the explorer shows is a jq filter, a JSONPath, Go-template
//     or custom-columns projection, or a raw object dump;
//   - chosen from the view bar, by mouse or by keyboard, the audit view
//     opens with nothing chosen, each loss marks every source with its
//     verdict in words, each source opens its own panel, and an identity's
//     source opens that source;
//   - switching views starts the target afresh and writes only its own
//     parameters, and a link that names the audit view, a loss or a source
//     opens it, while one whose audit values name nothing opens the journey;
//   - without JavaScript the text carries every source with its readers and
//     its verdict after every loss, every identity and every gap, and at
//     390px and 320px the audit view fits after every loss and with every
//     source open;
//   - the object map has one object for every kind config/crd/bases defines,
//     at the scope the CRD gives it, and the other objects #817 names; each
//     says who writes it, what it holds, what the operator does with it,
//     whether it can change and what deleting it does; and no owner
//     reference leaves its namespace, makes a plan own a Job, or makes
//     anything own what a person writes;
//   - chosen from the view bar, by mouse or by keyboard, the object map opens
//     with nothing selected, each object opens its own panel, lists every
//     relationship the model gives it and marks each related object with its
//     relationship in words, and a related object named in a panel opens;
//     a link that names the object map or an object of it opens it;
//   - without JavaScript the text carries every object with what it holds,
//     who writes it, whether it can change, what deleting it does, its
//     commands and every relationship, and at 390px and 320px the object
//     map fits with every object open;
//   - the composed workflow has #817's stages in its order, each in the
//     family that runs it, says it is proposed orchestration while no CRD
//     defines a Workflow or a WorkflowRun, states what sharing one database
//     takes, places each stop only where it can happen, authorizes each stage
//     by its own family's decision, keeps the external checks apart from
//     Migration InSync, and names every field of the stage record;
//   - chosen from the view bar, by mouse or by keyboard, the composed
//     workflow opens at its first stage with nothing stopped; each stage marks
//     the stages before it done; each stop marks its stage stopped and every
//     later stage held, in words, and says so; moving on, choosing another
//     stage or choosing the stop again clears it; and a link that names a
//     stage and a stop restores them, while a stop the stage cannot have is
//     ignored;
//   - without JavaScript the text carries the proposal, every precondition,
//     every stage with what the operator does marked shipped and what moves
//     it on marked proposed, every stop with what clears it, and every
//     decision, record field and reader, and at 390px and 320px the workflow
//     fits at every stage and every stop;
//   - the depths are Overview, Product and Implementation in that order, the
//     deepest the default; every part of every entry, in the interactive
//     view and in the text, is tagged at the depth the content gives it, and
//     each essential at Overview; at each depth chosen, with the script and
//     without it, every part deeper than it is hidden and every other part
//     shows, on a panel open on screen as well, and no link of the text leads
//     to a part it hides; the choice is written to the address as lx-depth
//     and the rest of the address left as it was; choosing a view, a family,
//     a path, a stage or a link keeps it; a link that names a depth opens at
//     it and one that names an unknown depth at the default; and the arrow
//     keys choose it;
//   - a related object, an identity's source and a link into another view is
//     an anchor whose address opens what it names, laid out inline where a
//     full stop follows it; every distinct address opens what it names; a
//     plain click follows it without loading the page again, and a click
//     with a modifier opens it in a page of its own; and every link the text
//     makes within the page leads to an entry of the text.
//
//   node scripts/check-lifecycle-explorer.mjs [--dist <dir>] [--selftest]
//
// Requires Playwright's chromium. Without it this skips and says what to
// install; with CI=1 it fails instead, because a green check that measured
// nothing is worse than a red one.

import { createServer } from 'node:http';
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, extname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  audit,
  defaultDepth,
  defaultFamily,
  depthOf,
  depths,
  families,
  objectMap,
  parentOf,
  walkOf,
  workflow,
} from '../src/lifecycle-explorer.mjs';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const siteRoot = join(scriptDir, '..');
const PAGE = 'reference/reconciliation/';
// The views of the view bar, the default first.
export const VIEWS = ['journey', 'audit', 'objects', 'workflow'];
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

// What #817 asks the audit view to cover, written down once so that a source,
// a loss, an identity or a gap dropped from the model fails here rather than
// leaving a page that quietly says less. Each source the epic lists is one
// id; the losses, the identities and the gaps are its own.
export const AUDIT_REQUIRED = {
  sources: [
    'a-status',
    'a-journal',
    'a-plans',
    'a-source',
    'a-decisions',
    'a-jobs',
    'a-results',
    'a-revisions',
    'a-db-log',
    'a-api-audit',
    'a-console',
  ],
  losses: ['job', 'resource', 'namespace', 'cluster'],
  identities: ['author', 'approver', 'executor', 'actor'],
  gaps: [787, 800, 802, 803, 804, 806],
};

// A projection or a raw dump in a command is what the epic rules out of the
// normal inspection path: the supported read is a kubectl ptah command, or a
// named system read with its own tools.
const PROJECTIONS = [
  [/(^|[\s|])jq(\s|$)/, 'a jq filter'],
  [/jsonpath/i, 'a JSONPath projection'],
  [/go-template/i, 'a Go template'],
  [/custom-columns/i, 'a custom-columns projection'],
  [/\bkubectl\b.*\bget\b.*\s-o\s*(json|yaml)\b/, 'a raw object dump'],
];

// Every command the explorer shows, from the stages, the objects, the audit
// sources, the object map and the composed workflow, with where it is shown.
export function explorerCommands(familyList, auditModel, map, flow) {
  const commands = [];
  for (const family of familyList) {
    const stages = [...family.scenario.stages, ...family.paths.flatMap((path) => path.stages ?? [])];
    for (const stage of stages) {
      for (const step of stage.inspect) commands.push({ where: `the stage "${stage.id}"`, command: step.command });
    }
    for (const object of family.objects) {
      for (const step of object.inspect ?? []) commands.push({ where: `the object "${object.id}"`, command: step.command });
    }
  }
  for (const source of auditModel.sources) {
    for (const reader of source.readers ?? []) {
      if (reader.command) commands.push({ where: `the source "${source.id}"`, command: reader.command });
    }
  }
  for (const node of map?.nodes ?? []) {
    for (const step of node.inspect ?? []) commands.push({ where: `the object map’s "${node.id}"`, command: step.command });
  }
  for (const stage of flow?.stages ?? []) {
    for (const step of stage.inspect ?? []) {
      if (step.command) commands.push({ where: `the workflow’s "${stage.id}"`, command: step.command });
    }
  }
  for (const reader of flow?.readers ?? []) commands.push({ where: 'the workflow’s stage record', command: reader.command });
  return commands;
}

export function commandProblems(commands) {
  const problems = [];
  if (commands.length === 0) return ['the explorer shows no command'];
  for (const { where, command } of commands) {
    for (const [pattern, what] of PROJECTIONS) {
      if (pattern.test(command)) problems.push(`${where} shows ${what}: ${JSON.stringify(command)}`);
    }
  }
  return problems;
}

const filled = (value) => typeof value === 'string' && value.trim() !== '';

// auditProblems refuses an audit model the view could not render honestly:
// a source, a loss, an identity or a gap the epic asks for left out, an id
// used twice or shared with a family, a source missing what each one owes, a
// survival verdict for a loss that does not exist or missing for one that
// does, and a reference from an identity or a gap to a source that does not
// exist.
export function auditProblems(auditModel, familyModels, required = AUDIT_REQUIRED) {
  const problems = [];
  const sources = auditModel.sources ?? [];
  if (sources.length === 0) return ['the audit view has no source'];
  const sourceIds = sources.map((source) => source.id);
  const lossIds = (auditModel.losses ?? []).map((loss) => loss.id);
  const verdictIds = (auditModel.verdicts ?? []).map((verdict) => verdict.id);
  const basisIds = (auditModel.bases ?? []).map((basis) => basis.id);
  const groupIds = (auditModel.groups ?? []).map((group) => group.id);

  for (const [what, ids] of [
    ['source', sourceIds],
    ['loss', lossIds],
    ['identity', (auditModel.identities ?? []).map((identity) => identity.id)],
    ['gap', (auditModel.gaps ?? []).map((gap) => gap.issue)],
  ]) {
    const seen = new Set();
    for (const id of ids) {
      if (seen.has(id)) problems.push(`the audit ${what} "${id}" is used twice`);
      seen.add(id);
    }
  }
  const familyIds = new Set(
    familyModels.flatMap((model) => [
      ...allStages(model).map((stage) => stage.id),
      ...model.paths.map((path) => path.id),
      ...model.objects.map((object) => object.id),
    ]),
  );
  for (const id of sourceIds) {
    if (!id.startsWith('a-')) problems.push(`the audit source "${id}" lacks the prefix a-`);
    if (familyIds.has(id)) problems.push(`the audit source "${id}" shares its id with a family`);
  }

  for (const id of required.sources) {
    if (!sourceIds.includes(id)) problems.push(`the audit view leaves out the source "${id}" the epic asks for`);
  }
  if (lossIds.join(' ') !== required.losses.join(' ')) {
    problems.push(`the audit losses are ${lossIds.join(', ') || 'none'}, not ${required.losses.join(', ')}`);
  }
  const identityIds = (auditModel.identities ?? []).map((identity) => identity.id);
  if (identityIds.join(' ') !== required.identities.join(' ')) {
    problems.push(`the audit identities are ${identityIds.join(', ') || 'none'}, not ${required.identities.join(', ')}`);
  }
  const gapIssues = (auditModel.gaps ?? []).map((gap) => gap.issue);
  for (const issue of required.gaps) {
    if (!gapIssues.includes(issue)) problems.push(`the audit view leaves out the gap #${issue} the epic asks for`);
  }

  for (const group of auditModel.groups ?? []) {
    if (!sources.some((source) => source.group === group.id)) problems.push(`the audit group "${group.id}" holds no source`);
  }
  for (const source of sources) {
    const at = `the audit source "${source.id}"`;
    if (!groupIds.includes(source.group)) problems.push(`${at} is in the group "${source.group}", which does not exist`);
    for (const field of ['label', 'records', 'establishes', 'cannot', 'retention']) {
      if (!filled(source[field])) problems.push(`${at} says nothing in ${field}`);
    }
    if (!(source.ids ?? []).some(filled)) problems.push(`${at} names no correlation identifier`);
    if ((source.readers ?? []).length === 0) problems.push(`${at} names no reader`);
    for (const reader of source.readers ?? []) {
      if (filled(reader.command) === filled(reader.system)) {
        problems.push(`${at} has a reader with ${reader.command ? 'both a command and a system' : 'neither a command nor a system'}`);
      }
      if (!filled(reader.says)) problems.push(`${at} has a reader that does not say what it shows`);
    }
    if ((source.links ?? []).length === 0) problems.push(`${at} links nothing`);
    const survives = source.survives ?? {};
    for (const loss of lossIds) {
      const verdict = survives[loss];
      if (!verdict) {
        problems.push(`${at} says nothing about what is left after "${loss}"`);
        continue;
      }
      if (!verdictIds.includes(verdict.verdict)) problems.push(`${at} gives "${loss}" the verdict "${verdict.verdict}", which does not exist`);
      if (!basisIds.includes(verdict.basis)) problems.push(`${at} gives "${loss}" the basis "${verdict.basis}", which does not exist`);
      if (!filled(verdict.text)) problems.push(`${at} gives "${loss}" no text`);
    }
    for (const loss of Object.keys(survives)) {
      if (!lossIds.includes(loss)) problems.push(`${at} has a verdict for "${loss}", which is not a loss`);
    }
  }

  for (const identity of auditModel.identities ?? []) {
    const at = `the identity "${identity.id}"`;
    if (!filled(identity.label) || !filled(identity.text)) problems.push(`${at} has no label or text`);
    if ((identity.recordedIn ?? []).length === 0) problems.push(`${at} is recorded nowhere`);
    for (const id of identity.recordedIn ?? []) {
      if (!sourceIds.includes(id)) problems.push(`${at} is recorded in "${id}", which is not a source`);
    }
  }
  for (const gap of auditModel.gaps ?? []) {
    const at = `the gap #${gap.issue}`;
    if (!Number.isInteger(gap.issue) || gap.issue <= 0) problems.push(`${at} is not an issue number`);
    if (!/^\d+\.\d+\.\d+$/.test(gap.version ?? '')) problems.push(`${at} names no release that closed it`);
    if (!filled(gap.title) || !filled(gap.text)) problems.push(`${at} has no title or text`);
    if ((gap.sources ?? []).length === 0) problems.push(`${at} names no source`);
    for (const id of gap.sources ?? []) {
      if (!sourceIds.includes(id)) problems.push(`${at} names "${id}", which is not a source`);
    }
  }
  if ((auditModel.deferred ?? []).length === 0) problems.push('the audit view lists nothing deferred');
  return problems;
}

// auditTextProblems judges the audit view's text, the one a reader without
// JavaScript, a crawler or a printer gets: every source with its records,
// what it cannot establish, its readers and its verdict after every loss;
// every identity; and every gap with a link to its issue.
export function auditTextProblems(reading, auditModel) {
  const problems = [];
  if (!reading.audit) return ['without JavaScript the text leaves out the audit view'];
  const { sources, identities, gaps } = reading.audit;
  for (const source of auditModel.sources) {
    const block = sources.find((one) => one.id === source.id);
    if (!block) {
      problems.push(`without JavaScript the audit text leaves out the source "${source.id}"`);
      continue;
    }
    for (const [what, text] of [
      ['label', source.label],
      ['records', source.records],
      ['cannot', source.cannot],
      ['retention', source.retention],
    ]) {
      if (!block.text.includes(text)) problems.push(`without JavaScript the source "${source.id}" lacks its ${what}`);
    }
    for (const reader of source.readers) {
      const shown = reader.command ?? reader.system;
      if (!block.text.includes(shown)) problems.push(`without JavaScript the source "${source.id}" lacks the reader ${JSON.stringify(shown)}`);
    }
    for (const loss of auditModel.losses) {
      if (!block.survives.includes(loss.id)) problems.push(`without JavaScript the source "${source.id}" says nothing after "${loss.id}"`);
    }
  }
  for (const identity of auditModel.identities) {
    if (!identities.includes(identity.id)) problems.push(`without JavaScript the audit text leaves out the identity "${identity.id}"`);
  }
  for (const gap of auditModel.gaps) {
    const shown = gaps.find((one) => one.issue === String(gap.issue));
    if (!shown) problems.push(`without JavaScript the audit text leaves out the gap #${gap.issue}`);
    else if (!shown.href.endsWith(`/issues/${gap.issue}`)) problems.push(`without JavaScript the gap #${gap.issue} links ${shown.href}`);
  }
  return problems;
}

// auditRenderProblems judges the audit view once chosen: the view bar, the
// view shown and the journey hidden, one button and one panel per source, one
// button per loss, and nothing open before the reader chose it.
export function auditRenderProblems(reading, auditModel) {
  const problems = [];
  if (reading.viewButtons !== VIEWS.length) problems.push(`the explorer has ${reading.viewButtons} view buttons, not ${VIEWS.length}`);
  if (!reading.shown) problems.push('choosing the audit view does not show it');
  if (reading.journeyShown) problems.push('choosing the audit view leaves the change journey on screen');
  if (reading.sourceButtons !== auditModel.sources.length) {
    problems.push(`the audit view has ${reading.sourceButtons} source buttons and the model ${auditModel.sources.length} sources`);
  }
  if (reading.panels !== auditModel.sources.length) {
    problems.push(`the audit view has ${reading.panels} source panels and the model ${auditModel.sources.length} sources`);
  }
  if (reading.lossButtons !== auditModel.losses.length) {
    problems.push(`the audit view has ${reading.lossButtons} loss buttons and the model ${auditModel.losses.length} losses`);
  }
  if (reading.visiblePanels !== 0) problems.push(`the audit view opens with ${reading.visiblePanels} source panels shown`);
  if (reading.marked !== 0) problems.push(`the audit view opens with ${reading.marked} sources marked by a loss`);
  return problems;
}

// auditSelectionProblems judges each loss and each source chosen: a loss
// marks every source with the verdict the model gives it, in words as well as
// in style, and presses its button alone; a source opens its own panel alone,
// marks the chosen loss's row in it, and presses its button alone.
export function auditSelectionProblems(readings, auditModel) {
  const problems = [];
  const label = new Map(auditModel.verdicts.map((verdict) => [verdict.id, verdict.label]));
  for (const loss of auditModel.losses) {
    const reading = readings.losses.find((one) => one.loss === loss.id);
    if (!reading) {
      problems.push(`the loss "${loss.id}" was never chosen`);
      continue;
    }
    if (reading.pressed.join(' ') !== loss.id) problems.push(`choosing "${loss.id}" presses ${reading.pressed.join(', ') || 'nothing'}`);
    for (const source of auditModel.sources) {
      const want = source.survives[loss.id].verdict;
      const got = reading.verdicts[source.id];
      if (!got) problems.push(`after "${loss.id}" the source "${source.id}" is not on screen`);
      else if (got.verdict !== want || got.badge !== label.get(want)) {
        problems.push(`after "${loss.id}" the source "${source.id}" reads ${got.verdict ?? 'nothing'} "${got.badge}", not ${want} "${label.get(want)}"`);
      }
    }
  }
  for (const source of auditModel.sources) {
    const reading = readings.sources.find((one) => one.source === source.id);
    if (!reading) {
      problems.push(`the source "${source.id}" was never selected`);
      continue;
    }
    if (reading.panels.join(' ') !== source.id) problems.push(`selecting "${source.id}" shows the panels ${reading.panels.join(', ') || 'none'}`);
    if (reading.pressed.join(' ') !== source.id) problems.push(`selecting "${source.id}" presses ${reading.pressed.join(', ') || 'nothing'}`);
    if (reading.current !== reading.loss) {
      problems.push(`selecting "${source.id}" after "${reading.loss}" marks the row ${reading.current ?? 'none'}`);
    }
  }
  if (readings.chip) {
    const { want, got } = readings.chip;
    if (got !== want) problems.push(`an identity’s source "${want}" opens ${got ?? 'nothing'}`);
  } else problems.push('no identity’s source was followed');
  return problems;
}

// viewSwitchProblems judges each switch between the views, made from a state
// that is default in nothing: the audit view opens with no loss and no
// source, and the object map with no object, and each writes lx-view alone;
// the change journey opens in the default family at its default path and
// first stage with nothing selected, and writes none of the other views'
// parameters.
export function viewSwitchProblems(readings, familyModels, flow) {
  const problems = [];
  const first = familyModels[0];
  const firstStep = flow.stages[0].id;
  for (const to of VIEWS) {
    if (!readings.some((reading) => reading.to === to)) problems.push(`no switch went to the ${to} view`);
  }
  for (const { to, from, got } of readings) {
    const at = `switching from ${from} to the ${to} view`;
    const search = new URLSearchParams(got.search);
    if (got.view !== to) problems.push(`${at} shows the ${got.view} view`);
    if (got.outcome !== null) problems.push(`${at} keeps the stop ${got.outcome}`);
    if (got.step !== firstStep) problems.push(`${at} leaves the composed workflow at ${got.step ?? 'no stage'}, not ${firstStep}`);
    if (to !== 'journey') {
      if (got.loss !== null) problems.push(`${at} keeps the loss ${got.loss}`);
      if (got.source !== null) problems.push(`${at} keeps the source ${got.source}`);
      if (got.node !== null) problems.push(`${at} keeps the object ${got.node}`);
      if (search.get('lx-view') !== to) problems.push(`${at} writes ${got.search || 'nothing'}, not lx-view=${to}`);
      for (const param of ['lx-family', 'lx-path', 'lx-stage', 'lx-object', 'lx-loss', 'lx-source', 'lx-node', 'lx-step', 'lx-outcome']) {
        if (search.has(param)) problems.push(`${at} writes ${got.search}, with ${param}`);
      }
      continue;
    }
    if (got.family !== first.id) problems.push(`${at} opens the family ${got.family ?? 'none'}, not ${first.id}`);
    if (got.path !== first.paths[0].id) problems.push(`${at} opens the path ${got.path ?? 'none'}, not ${first.paths[0].id}`);
    if (got.stage !== first.stages[0].id) problems.push(`${at} opens the stage ${got.stage ?? 'none'}, not ${first.stages[0].id}`);
    if (got.object !== null) problems.push(`${at} keeps ${got.object} selected`);
    for (const param of ['lx-view', 'lx-loss', 'lx-source', 'lx-node', 'lx-family', 'lx-path', 'lx-object', 'lx-step', 'lx-outcome']) {
      if (search.has(param)) problems.push(`${at} writes ${got.search}, with ${param}`);
    }
    if (search.get('lx-stage') !== first.stages[0].id) problems.push(`${at} writes ${got.search || 'nothing'}, not lx-stage=${first.stages[0].id}`);
  }
  return problems;
}

// auditLinkProblems judges pages opened from links into the audit view, and
// links that name the audit view and a journey value together.
export function auditLinkProblems(readings) {
  const problems = [];
  if (readings.length === 0) return ['no link into the audit view was opened'];
  for (const { link, want, got } of readings) {
    if (got.view !== want.view) problems.push(`the link ${link} opens the ${got.view} view, not ${want.view}`);
    if (got.loss !== want.loss) problems.push(`the link ${link} chooses the loss ${got.loss ?? 'none'}, not ${want.loss ?? 'none'}`);
    if (got.source !== want.source) problems.push(`the link ${link} opens ${got.source ?? 'no source'}, not ${want.source ?? 'no source'}`);
    if (want.family && got.family !== want.family) problems.push(`the link ${link} opens the family ${got.family ?? 'none'}, not ${want.family}`);
  }
  return problems;
}

// auditKeyboardProblems judges the audit view by keyboard alone: Tab and
// Enter from the heading choose it, then a loss, then a source.
export function auditKeyboardProblems(reading) {
  const problems = [];
  if (!reading.chosen) problems.push('Tab and Enter from the heading do not choose the audit view');
  if (!reading.loss) problems.push('Tab and Enter do not reach and choose a loss');
  if (!reading.source) problems.push('Tab and Enter do not reach and open a source');
  return problems;
}

// What #817 asks the object map to cover besides the kinds config/crd/bases
// defines, which crdKinds reads: the objects an operation reads or writes that
// are not the operator's own kinds.
export const OBJECT_MAP_REQUIRED = {
  nodes: ['o-source', 'o-secret', 'o-snapshot', 'o-job', 'o-lease', 'o-database', 'o-artifact'],
};

// The kinds the CRDs define, each with its scope, so that a kind added there
// is one the object map has to show.
export function crdKinds(dir = join(siteRoot, '..', '..', 'config', 'crd', 'bases')) {
  return readdirSync(dir)
    .filter((name) => name.endsWith('.yaml'))
    .sort()
    .map((name) => {
      const text = readFileSync(join(dir, name), 'utf8');
      return {
        file: name,
        kind: text.match(/^ {2}names:\n(?: {4}\S.*\n)*? {4}kind: (\w+)$/m)?.[1] ?? null,
        scope: text.match(/^ {2}scope: (\w+)$/m)?.[1] ?? null,
      };
    });
}

// A plan owns no Job, and nothing owns a decision a person made.
const PLAN_KINDS = ['PtahSchemaPlan', 'PtahMigrationPlan'];
const DECISION_KINDS = ['PtahSchemaApproval', 'PtahMigrationApproval', 'PtahMigrationRunAcknowledgment'];

// objectMapProblems refuses an object map the view could not render
// honestly: a kind the CRDs define left out or shown at another scope, an
// object #817 names left out, an id used twice or shared with a family or the
// audit view, an object missing what each one owes, a relation to nowhere,
// and an owner reference Kubernetes could not hold or the operator does not
// set: one that leaves its namespace or the cluster, one from a plan to a Job,
// and one onto an object a person writes.
export function objectMapProblems(map, { kinds, familyModels, auditModel, required = OBJECT_MAP_REQUIRED }) {
  const problems = [];
  const nodes = map.nodes ?? [];
  if (nodes.length === 0) return ['the object map has no object'];
  const byId = new Map(nodes.map((node) => [node.id, node]));
  const groupIds = (map.groups ?? []).map((group) => group.id);
  const scopeIds = (map.scopes ?? []).map((scope) => scope.id);
  const changeIds = (map.changes ?? []).map((change) => change.id);
  const typeIds = (map.types ?? []).map((type) => type.id);
  const familyIds = familyModels.map((model) => model.id);

  for (const [what, ids] of [
    ['object', nodes.map((node) => node.id)],
    ['object label', nodes.map((node) => node.label)],
    ['group', groupIds],
    ['scope', scopeIds],
    ['change', changeIds],
    ['relationship type', typeIds],
  ]) {
    const seen = new Set();
    for (const id of ids) {
      if (seen.has(id)) problems.push(`the object map’s ${what} "${id}" is used twice`);
      seen.add(id);
    }
  }
  const taken = new Set([
    ...familyModels.flatMap((model) => [
      ...allStages(model).map((stage) => stage.id),
      ...model.paths.map((path) => path.id),
      ...model.objects.map((object) => object.id),
    ]),
    ...(auditModel.sources ?? []).map((source) => source.id),
  ]);
  for (const node of nodes) {
    if (!node.id.startsWith('o-')) problems.push(`the object map’s "${node.id}" lacks the prefix o-`);
    if (taken.has(node.id)) problems.push(`the object map’s "${node.id}" shares its id with a family or the audit view`);
  }
  for (const id of required.nodes) {
    if (!byId.has(id)) problems.push(`the object map leaves out "${id}", which #817 asks for`);
  }

  if (kinds.length === 0) problems.push('no kind was read from the CRDs');
  for (const { file, kind, scope } of kinds) {
    if (!kind || !scope) {
      problems.push(`the kind and scope of ${file} could not be read`);
      continue;
    }
    const shown = nodes.filter((node) => node.kind === kind);
    if (shown.length !== 1) {
      problems.push(`the object map shows ${shown.length} objects of the kind ${kind}, which the CRDs define, not one`);
      continue;
    }
    if ((scope === 'Cluster') !== (shown[0].scope === 'cluster')) {
      problems.push(`the object map puts ${kind} at the scope "${shown[0].scope}", and its CRD says ${scope}`);
    }
  }

  for (const node of nodes) {
    const at = `the object map’s "${node.id}"`;
    for (const field of ['label', 'kind', 'holds', 'writtenBy', 'operator', 'deletion']) {
      if (!filled(node[field])) problems.push(`${at} says nothing in ${field}`);
    }
    if (!groupIds.includes(node.group)) problems.push(`${at} is in the group "${node.group}", which does not exist`);
    if (!scopeIds.includes(node.scope)) problems.push(`${at} has the scope "${node.scope}", which does not exist`);
    if ((node.group === 'outside') !== (node.scope === 'outside')) {
      problems.push(`${at} is in the group "${node.group}" at the scope "${node.scope}": only what is outside the cluster is both`);
    }
    if (!changeIds.includes(node.change?.verdict)) problems.push(`${at} says it changes as "${node.change?.verdict}", which does not exist`);
    if (!filled(node.change?.text)) problems.push(`${at} does not say whether it can change`);
    if ((node.families ?? []).length === 0) problems.push(`${at} belongs to no family`);
    for (const family of node.families ?? []) {
      if (!familyIds.includes(family)) problems.push(`${at} belongs to the family "${family}", which does not exist`);
    }
    if ((node.inspect ?? []).length === 0) problems.push(`${at} says no way to inspect it`);
    for (const step of node.inspect ?? []) {
      if (!filled(step.command) || !filled(step.says)) problems.push(`${at} has a command without the command or what it shows`);
    }
    if (node.gap !== undefined && !filled(node.gap)) problems.push(`${at} names an empty gap`);
    if ((node.links ?? []).length === 0) problems.push(`${at} links nothing`);
  }
  for (const group of groupIds) {
    if (!nodes.some((node) => node.group === group)) problems.push(`the object map’s group "${group}" holds no object`);
  }

  const relations = map.relations ?? [];
  const seen = new Set();
  for (const relation of relations) {
    const at = `the object map’s ${relation.type} relation from "${relation.from}" to "${relation.to}"`;
    if (!typeIds.includes(relation.type)) problems.push(`${at} has a type the legend does not draw`);
    for (const end of [relation.from, relation.to]) {
      if (!byId.has(end)) problems.push(`${at} names "${end}", which does not exist`);
    }
    if (relation.from === relation.to) problems.push(`${at} relates it to itself`);
    const key = `${relation.from} ${relation.type} ${relation.to}`;
    if (seen.has(key)) problems.push(`${at} is written twice`);
    seen.add(key);
    if (!filled(relation.text)) problems.push(`${at} does not say what it is`);
    const owner = byId.get(relation.from);
    const owned = byId.get(relation.to);
    if (relation.type !== 'owns' || !owner || !owned) continue;
    if (owner.scope === 'outside' || owned.scope === 'outside') {
      problems.push(`${at} is an owner reference outside the cluster, which no garbage collector follows`);
    } else if (owner.scope !== 'cluster' && owner.scope !== owned.scope) {
      problems.push(`${at} leaves the namespace "${owner.scope}": an owner reference cannot`);
    }
    if (PLAN_KINDS.includes(owner.kind) && owned.kind === 'Job') {
      problems.push(`${at} makes a plan own a Job: the resource owns every operation Job, and an Apply only runs the plan`);
    }
    if (owned.group === 'person' || DECISION_KINDS.includes(owned.kind)) {
      problems.push(`${at} owns what a person writes, which nothing collects with "${relation.from}"`);
    }
  }
  for (const type of typeIds) {
    if (!relations.some((relation) => relation.type === type)) problems.push(`the object map’s legend draws "${type}", which no relation is`);
  }
  for (const node of nodes) {
    if (!relations.some((relation) => relation.from === node.id || relation.to === node.id)) {
      problems.push(`the object map’s "${node.id}" is related to nothing`);
    }
  }
  if ((map.distinctions ?? []).length === 0) problems.push('the object map says nothing about what its relationships mean');
  if ((map.links ?? []).length === 0) problems.push('the object map links nothing');
  return problems;
}

// What the object map says of one object: every relationship from either end
// as "type:other", and for each related object the mark its node carries, an
// owner reference over anything else between the same two, read from the
// selected end.
export function expectedNodeMarks(id, map) {
  const types = new Map(map.types.map((type) => [type.id, type]));
  const touching = map.relations.filter((relation) => relation.from === id || relation.to === id);
  const listed = touching.map((relation) => `${relation.type}:${relation.from === id ? relation.to : relation.from}`).sort();
  const marks = {};
  for (const relation of touching) {
    const other = relation.from === id ? relation.to : relation.from;
    if (marks[other] && marks[other].type === 'owns') continue;
    if (marks[other] && relation.type !== 'owns') continue;
    const type = types.get(relation.type);
    marks[other] = { type: relation.type, label: relation.from === id ? type.label : type.inverse };
  }
  return { listed, marks };
}

// objectMapTextProblems judges the object map's text, the one a reader
// without JavaScript, a crawler or a printer gets: every relationship type,
// and every object with what it holds, who writes it, what the operator does
// with it, whether it can change, what deleting it does, its commands, its
// gap and every relationship it has.
export function objectMapTextProblems(reading, map) {
  const problems = [];
  if (!reading.objects) return ['without JavaScript the text leaves out the object map'];
  const { types, nodes } = reading.objects;
  const changeLabel = new Map(map.changes.map((change) => [change.id, change.label]));
  for (const type of map.types) {
    if (!types.includes(type.id)) problems.push(`without JavaScript the object map leaves out the relationship type "${type.id}"`);
  }
  for (const node of map.nodes) {
    const block = nodes.find((one) => one.id === node.id);
    if (!block) {
      problems.push(`without JavaScript the object map leaves out "${node.id}"`);
      continue;
    }
    for (const [what, text] of [
      ['label', node.label],
      ['holds', node.holds],
      ['writtenBy', node.writtenBy],
      ['operator', node.operator],
      ['change', `${changeLabel.get(node.change.verdict)}. ${node.change.text}`],
      ['deletion', node.deletion],
      ['gap', node.gap],
      ...node.inspect.map((step) => [`command ${JSON.stringify(step.command)}`, step.command]),
    ]) {
      if (text !== undefined && !block.text.includes(text)) problems.push(`without JavaScript "${node.id}" lacks its ${what}`);
    }
    const want = expectedNodeMarks(node.id, map).listed;
    const got = [...block.relations].sort();
    const missing = want.filter((entry) => !got.includes(entry));
    const extra = got.filter((entry) => !want.includes(entry));
    if (missing.length > 0) problems.push(`without JavaScript "${node.id}" leaves out ${missing.join(', ')}`);
    if (extra.length > 0) problems.push(`without JavaScript "${node.id}" also lists ${extra.join(', ')}`);
  }
  return problems;
}

// objectMapRenderProblems judges the object map once chosen: the view bar,
// the map shown and the other views hidden, one button and one panel per
// object, the legend drawing every relationship type in order, and nothing
// open or marked before the reader chose it.
export function objectMapRenderProblems(reading, map) {
  const problems = [];
  if (reading.viewButtons !== VIEWS.length) problems.push(`the explorer has ${reading.viewButtons} view buttons, not ${VIEWS.length}`);
  if (!reading.shown) problems.push('choosing the object map does not show it');
  if (reading.journeyShown) problems.push('choosing the object map leaves the change journey on screen');
  if (reading.auditShown) problems.push('choosing the object map leaves the audit view on screen');
  if (reading.nodeButtons !== map.nodes.length) {
    problems.push(`the object map has ${reading.nodeButtons} object buttons and the model ${map.nodes.length} objects`);
  }
  if (reading.panels !== map.nodes.length) problems.push(`the object map has ${reading.panels} panels and the model ${map.nodes.length} objects`);
  const legend = map.types.map((type) => type.id).join(' ');
  if (reading.legend.join(' ') !== legend) problems.push(`the object map’s legend draws ${reading.legend.join(', ') || 'nothing'}, not ${legend}`);
  if (reading.visiblePanels !== 0) problems.push(`the object map opens with ${reading.visiblePanels} panels shown`);
  if (reading.marked !== 0) problems.push(`the object map opens with ${reading.marked} objects marked as related`);
  if (!reading.hintShown) problems.push('the object map opens without saying what selecting an object does');
  return problems;
}

// objectMapSelectionProblems judges each object selected: it opens its own
// panel alone and presses its own button alone, the panel lists every
// relationship the model gives it, and every related object, and nothing
// else, carries the relationship's style and its name in words. Then a
// related object named in a panel has to open.
export function objectMapSelectionProblems(readings, map) {
  const problems = [];
  if (readings.nodes.length !== map.nodes.length) {
    problems.push(`${readings.nodes.length} of the object map’s ${map.nodes.length} objects were selected`);
  }
  for (const reading of readings.nodes) {
    const at = `selecting "${reading.id}"`;
    if (reading.panels.join(' ') !== reading.id) problems.push(`${at} shows the panels ${reading.panels.join(', ') || 'none'}`);
    if (reading.pressed.join(' ') !== reading.id) problems.push(`${at} presses ${reading.pressed.join(', ') || 'nothing'}`);
    const want = expectedNodeMarks(reading.id, map);
    const listed = [...reading.listed].sort();
    const missing = want.listed.filter((entry) => !listed.includes(entry));
    const extra = listed.filter((entry) => !want.listed.includes(entry));
    if (missing.length > 0) problems.push(`the panel of "${reading.id}" leaves out ${missing.join(', ')}`);
    if (extra.length > 0) problems.push(`the panel of "${reading.id}" also lists ${extra.join(', ')}`);
    for (const [other, mark] of Object.entries(want.marks)) {
      const got = reading.marks[other];
      if (!got) problems.push(`${at} leaves "${other}" unmarked, which it ${mark.label.toLowerCase()}`);
      else if (got.type !== mark.type || got.badge !== mark.label) {
        problems.push(`${at} marks "${other}" ${got.type} "${got.badge}", not ${mark.type} "${mark.label}"`);
      }
    }
    for (const other of Object.keys(reading.marks)) {
      if (!want.marks[other]) problems.push(`${at} marks "${other}", which the model does not relate to it`);
    }
  }
  if (readings.follow) {
    const { from, want, got } = readings.follow;
    if (got !== want) problems.push(`following "${want}" from the panel of "${from}" opens ${got ?? 'nothing'}`);
  } else problems.push('no related object was followed from a panel');
  return problems;
}

// objectMapLinkProblems judges pages opened from links into the object map,
// and links that name another view or a value that is not an object of it.
export function objectMapLinkProblems(readings) {
  const problems = [];
  if (readings.length === 0) return ['no link into the object map was opened'];
  for (const { link, want, got } of readings) {
    if (got.view !== want.view) problems.push(`the link ${link} opens the ${got.view} view, not ${want.view}`);
    if (got.node !== want.node) problems.push(`the link ${link} opens ${got.node ?? 'no object'}, not ${want.node ?? 'no object'}`);
  }
  return problems;
}

// objectMapKeyboardProblems judges the object map by keyboard alone: Tab and
// Enter from the heading choose it, then an object, then a related object
// its panel names.
export function objectMapKeyboardProblems(reading) {
  const problems = [];
  if (!reading.chosen) problems.push('Tab and Enter from the heading do not choose the object map');
  if (!reading.node) problems.push('Tab and Enter do not reach and open an object of the map');
  if (!reading.follow) problems.push('Tab and Enter do not follow a related object from its panel');
  return problems;
}

// What #817 asks of the composed workflow: its stages in this order, each in
// the family that runs it, the preconditions for sharing one database, where
// each stop can happen, the stages a decision authorizes and whose decision
// that is, and the fields of the stage record. It is written here rather than
// read from the model, so a model cannot relax a rule by changing what it
// declares about itself.
export const WORKFLOW_REQUIRED = {
  stages: [
    ['w-expand', 'schema'],
    ['w-verify-expand', 'schema'],
    ['w-backfill', 'migration'],
    ['w-history', 'migration'],
    ['w-checks', 'outside'],
    ['w-contract', 'schema'],
    ['w-verify-contract', 'schema'],
  ],
  preconditions: ['w-pre-realm', 'w-pre-shared', 'w-pre-scope', 'w-pre-order'],
  // A schema Apply fails in no way between a refusal and an outcome nobody
  // knows, so an ordinary failure is a migration's alone; only the check
  // stage has a check to fail.
  outcomes: {
    'w-refused': ['schema', 'migration'],
    'w-stale': ['schema', 'migration'],
    'w-failed': ['migration'],
    'w-unresolved': ['schema', 'migration'],
    'w-drift': ['schema'],
    'w-check-failed': ['outside'],
  },
  decided: ['w-expand', 'w-backfill', 'w-checks', 'w-contract'],
  decisions: { schema: 'w-dec-schema', migration: 'w-dec-migration', outside: 'w-dec-check' },
  policy: 'w-dec-policy',
  record: [
    'w-rec-incarnation',
    'w-rec-source',
    'w-rec-plan',
    'w-rec-decision',
    'w-rec-operation',
    'w-rec-outcome',
    'w-rec-verification',
    'w-rec-recovery',
  ],
  // The orchestration the view calls proposed: no kind the CRDs define may
  // be it.
  proposedKinds: ['Workflow', 'WorkflowRun'],
};

// workflowProblems refuses a composed workflow the view could not render
// honestly: stages out of the order #817 gives or in another family, an id
// used twice or shared with another view, a proposal that does not say what
// does not exist or a kind the CRDs now define, a precondition, a stop, a
// decision or a record field left out, a stop at a stage where it cannot
// happen, a resource stage that cannot end with an unknown outcome, a stage
// authorized by another family's decision, a check stage that does not say
// Migration InSync is not it, and a reference to an object, a path or a
// decision that does not exist.
export function workflowProblems(flow, { familyModels, auditModel, map, kinds, required = WORKFLOW_REQUIRED }) {
  const problems = [];
  const stages = flow.stages ?? [];
  if (stages.length === 0) return ['the composed workflow has no stage'];
  const familyIds = (flow.families ?? []).map((family) => family.id);
  const outcomeIds = (flow.outcomes ?? []).map((outcome) => outcome.id);
  const decisionIds = (flow.decisions ?? []).map((decision) => decision.id);
  const preconditions = flow.preconditions ?? [];
  const record = flow.record ?? [];
  const nodeIds = new Set((map.nodes ?? []).map((node) => node.id));

  const order = stages.map((stage) => stage.id).join(' ');
  const wanted = required.stages.map(([id]) => id).join(' ');
  if (order !== wanted) problems.push(`the composed workflow’s stages are ${order}, not ${wanted}`);
  for (const [id, family] of required.stages) {
    const stage = stages.find((one) => one.id === id);
    if (stage && stage.family !== family) problems.push(`the stage "${id}" runs in "${stage.family}", not "${family}"`);
  }

  const taken = new Set([
    ...familyModels.flatMap((model) => [
      ...allStages(model).map((stage) => stage.id),
      ...model.paths.map((path) => path.id),
      ...model.objects.map((object) => object.id),
    ]),
    ...(auditModel.sources ?? []).map((source) => source.id),
    ...nodeIds,
  ]);
  const seen = new Set();
  for (const id of [
    ...stages.map((stage) => stage.id),
    ...outcomeIds,
    ...decisionIds,
    ...preconditions.map((one) => one.id),
    ...record.map((field) => field.id),
  ]) {
    if (seen.has(id)) problems.push(`the composed workflow’s "${id}" is used twice`);
    seen.add(id);
    if (!id.startsWith('w-')) problems.push(`the composed workflow’s "${id}" lacks the prefix w-`);
    if (taken.has(id)) problems.push(`the composed workflow’s "${id}" shares its id with another view`);
  }

  if (!filled(flow.proposal?.label) || !filled(flow.proposal?.text)) {
    problems.push('the composed workflow does not say it is proposed orchestration');
  }
  for (const kind of required.proposedKinds) {
    if (!new RegExp(`\\b${kind}\\b`).test(flow.proposal?.text ?? '')) {
      problems.push(`the composed workflow’s proposal does not say there is no ${kind}`);
    }
    if (kinds.some((one) => one.kind === kind)) problems.push(`the CRDs define ${kind}, which the composed workflow calls proposed`);
  }
  const marks = (flow.marks ?? []).map((mark) => mark.id).join(' ');
  if (marks !== 'shipped proposed') problems.push(`the composed workflow marks ${marks || 'nothing'}, not shipped and proposed`);
  for (const mark of flow.marks ?? []) {
    if (!filled(mark.label) || !filled(mark.text)) problems.push(`the mark "${mark.id}" says nothing`);
  }

  for (const id of required.preconditions) {
    if (!preconditions.some((one) => one.id === id)) {
      problems.push(`the composed workflow leaves out the precondition "${id}", which #817 asks for`);
    }
  }
  for (const precondition of preconditions) {
    const at = `the precondition "${precondition.id}"`;
    if (!filled(precondition.title) || !filled(precondition.text)) problems.push(`${at} says nothing`);
    if ((precondition.objects ?? []).length === 0) problems.push(`${at} names no object`);
    for (const id of precondition.objects ?? []) {
      if (!nodeIds.has(id)) problems.push(`${at} names "${id}", which the object map does not have`);
    }
    if ((precondition.links ?? []).length === 0) problems.push(`${at} links nothing`);
  }

  for (const id of Object.keys(required.outcomes)) {
    if (!outcomeIds.includes(id)) problems.push(`the composed workflow leaves out the stop "${id}"`);
  }
  for (const outcome of flow.outcomes ?? []) {
    if (!filled(outcome.label) || !filled(outcome.text)) problems.push(`the stop "${outcome.id}" says nothing`);
    if (!required.outcomes[outcome.id]) problems.push(`the stop "${outcome.id}" is one this check does not know where it can happen`);
    if (!stages.some((stage) => (stage.outcomes ?? []).some((one) => one.outcome === outcome.id))) {
      problems.push(`the stop "${outcome.id}" stops no stage`);
    }
  }

  for (const id of [...Object.values(required.decisions), required.policy]) {
    if (!decisionIds.includes(id)) problems.push(`the composed workflow leaves out the decision "${id}"`);
  }
  for (const decision of flow.decisions ?? []) {
    const at = `the decision "${decision.id}"`;
    if (!filled(decision.label) || !filled(decision.covers) || !filled(decision.notCovered)) {
      problems.push(`${at} does not say what it covers and what it does not`);
    }
    if (decision.node !== undefined && !nodeIds.has(decision.node)) {
      problems.push(`${at} names "${decision.node}", which the object map does not have`);
    }
  }

  for (const stage of stages) {
    const at = `the stage "${stage.id}"`;
    for (const field of ['title', 'goal', 'person', 'operator', 'gate', 'done']) {
      if (!filled(stage[field])) problems.push(`${at} says nothing in ${field}`);
    }
    if (!familyIds.includes(stage.family)) problems.push(`${at} runs in "${stage.family}", which does not exist`);
    if ((stage.inspect ?? []).length === 0) problems.push(`${at} says no way to act on it or inspect it`);
    for (const step of stage.inspect ?? []) {
      if (filled(step.command) === filled(step.system) || !filled(step.says)) {
        problems.push(`${at} has a step that is not one command or one system with what it shows`);
      }
    }
    if (required.decided.includes(stage.id) && !stage.decision) problems.push(`${at} names no decision, and #817 asks what authorizes it`);
    if (stage.decision) {
      const own = required.decisions[stage.family];
      if (stage.decision !== own) problems.push(`${at} runs in "${stage.family}" and names the decision "${stage.decision}", not "${own}"`);
      if (!filled(stage.decisionText)) problems.push(`${at} does not say what its decision is`);
    }
    const stops = (stage.outcomes ?? []).map((one) => one.outcome);
    if (stops.length === 0) problems.push(`${at} can stop on nothing`);
    if (new Set(stops).size !== stops.length) problems.push(`${at} lists a stop twice`);
    if (stage.family !== 'outside' && !stops.includes('w-unresolved')) {
      problems.push(`${at} runs SQL or proves it, and cannot end with an outcome nobody knows`);
    }
    for (const one of stage.outcomes ?? []) {
      const where = `${at} stopped by "${one.outcome}"`;
      if (!outcomeIds.includes(one.outcome)) problems.push(`${where}, which does not exist`);
      else if (!(required.outcomes[one.outcome] ?? []).includes(stage.family)) {
        problems.push(`${where}, which cannot happen in "${stage.family}"`);
      }
      if (!filled(one.text) || !filled(one.recovery)) problems.push(`${where} does not say what happened and what clears it`);
      if (one.journey) {
        const family = familyModels.find((model) => model.id === one.journey.family);
        const path = family?.paths.find((candidate) => candidate.id === one.journey.path);
        if (!path) problems.push(`${where} names the journey path "${one.journey.family}/${one.journey.path}", which does not exist`);
        else if (path.id === family.paths[0].id) problems.push(`${where} names the successful path "${path.id}"`);
      }
    }
    if ((stage.objects ?? []).length === 0) problems.push(`${at} names no object`);
    for (const id of stage.objects ?? []) {
      if (!nodeIds.has(id)) problems.push(`${at} names "${id}", which the object map does not have`);
    }
    if ((stage.links ?? []).length === 0) problems.push(`${at} links nothing`);
    if (stage.family === 'outside' && !/\bInSync\b/.test(stage.gate ?? '')) {
      problems.push(`${at} does not say that Migration InSync does not satisfy it`);
    }
  }

  for (const id of required.record) {
    if (!record.some((field) => field.id === id)) problems.push(`the stage record leaves out "${id}", which #817 asks for`);
  }
  for (const field of record) {
    if (!filled(field.label) || !filled(field.text)) problems.push(`the stage record’s "${field.id}" says nothing`);
  }
  if (!filled(flow.recordText)) problems.push('the composed workflow does not say who keeps the stage record');
  if ((flow.readers ?? []).length === 0) problems.push('the composed workflow says no way to read the stage record');
  for (const reader of flow.readers ?? []) {
    if (!filled(reader.command) || !filled(reader.says)) problems.push('a reader of the stage record lacks its command or what it shows');
  }
  if ((flow.distinctions ?? []).length === 0) problems.push('the composed workflow says nothing about what it keeps apart');
  if ((flow.links ?? []).length === 0) problems.push('the composed workflow links nothing');
  return problems;
}

// workflowTextProblems judges the composed workflow's text: the proposal,
// both marks, every precondition, every stage in order with its commands,
// what the operator does marked shipped and what moves it on marked proposed,
// each stop it can stop on with what clears it, and every stop, decision,
// record field and reader.
export function workflowTextProblems(reading, flow) {
  const problems = [];
  const text = reading.workflow;
  if (!text) return ['without JavaScript the text leaves out the composed workflow'];
  if (!text.proposal.includes(flow.proposal.text)) problems.push('without JavaScript the composed workflow does not say it is proposed');
  for (const mark of flow.marks) {
    if (!text.marks.includes(mark.id)) problems.push(`without JavaScript the composed workflow leaves out the mark "${mark.id}"`);
  }
  for (const precondition of flow.preconditions) {
    const block = text.preconditions.find((one) => one.id === precondition.id);
    if (!block) problems.push(`without JavaScript the composed workflow leaves out the precondition "${precondition.id}"`);
    else if (!block.text.includes(precondition.text)) problems.push(`without JavaScript the precondition "${precondition.id}" lacks its text`);
  }
  const order = text.stages.map((stage) => stage.id).join(' ');
  const want = flow.stages.map((stage) => stage.id).join(' ');
  if (order !== want) problems.push(`without JavaScript the composed workflow’s stages read ${order || 'nothing'}, not ${want}`);
  for (const stage of flow.stages) {
    const block = text.stages.find((one) => one.id === stage.id);
    if (!block) continue;
    for (const [what, value] of [
      ['title', stage.title],
      ['goal', stage.goal],
      ['person', stage.person],
      ['decision', stage.decisionText],
      ['done', stage.done],
      ['limits', stage.limits],
      ...stage.inspect.map((step) => [`step ${JSON.stringify(step.command ?? step.system)}`, step.command ?? step.system]),
    ]) {
      if (value !== undefined && !block.text.includes(value)) problems.push(`without JavaScript the stage "${stage.id}" lacks its ${what}`);
    }
    if (!block.operator.includes(stage.operator) || !/\bshipped\b/i.test(block.operator)) {
      problems.push(`without JavaScript the stage "${stage.id}" does not give what the operator does as shipped`);
    }
    if (!block.gate.includes(stage.gate) || !/\bproposed\b/i.test(block.gate)) {
      problems.push(`without JavaScript the stage "${stage.id}" does not give what moves it on as proposed`);
    }
    const stops = stage.outcomes.map((one) => one.outcome).join(' ');
    if (block.stops.join(' ') !== stops) {
      problems.push(`without JavaScript the stage "${stage.id}" stops on ${block.stops.join(', ') || 'nothing'}, not ${stops.split(' ').join(', ')}`);
    }
    for (const one of stage.outcomes) {
      if (!block.text.includes(one.text) || !block.text.includes(one.recovery)) {
        problems.push(`without JavaScript the stage "${stage.id}" lacks what "${one.outcome}" holds or what clears it`);
      }
    }
  }
  for (const outcome of flow.outcomes) {
    if (!text.outcomes.includes(outcome.id)) problems.push(`without JavaScript the composed workflow leaves out the stop "${outcome.id}"`);
  }
  for (const decision of flow.decisions) {
    const block = text.decisions.find((one) => one.id === decision.id);
    if (!block || !block.text.includes(decision.covers) || !block.text.includes(decision.notCovered)) {
      problems.push(`without JavaScript the decision "${decision.id}" does not say what it covers and what it does not`);
    }
  }
  for (const field of flow.record) {
    if (!text.record.includes(field.id)) problems.push(`without JavaScript the stage record leaves out "${field.id}"`);
  }
  for (const reader of flow.readers) {
    if (!text.text.includes(reader.command)) problems.push(`without JavaScript the stage record lacks the command ${JSON.stringify(reader.command)}`);
  }
  return problems;
}

// workflowRenderProblems judges the composed workflow once chosen: the view
// bar, the workflow shown and every other view hidden, one step and one
// panel per stage, the first stage open with nothing stopped, the proposal on
// screen, each panel giving what the operator does as shipped and what moves
// the stage on as proposed, and each stage tagged with what runs it.
export function workflowRenderProblems(reading, flow) {
  const problems = [];
  const count = flow.stages.length;
  if (reading.viewButtons !== VIEWS.length) problems.push(`the explorer has ${reading.viewButtons} view buttons, not ${VIEWS.length}`);
  if (!reading.shown) problems.push('choosing the composed workflow does not show it');
  for (const other of reading.othersShown) problems.push(`choosing the composed workflow leaves the ${other} view on screen`);
  if (reading.steps !== count) problems.push(`the composed workflow has ${reading.steps} steps and the model ${count} stages`);
  if (reading.panels !== count) problems.push(`the composed workflow has ${reading.panels} panels and the model ${count} stages`);
  if (reading.visiblePanels.join(' ') !== flow.stages[0].id) {
    problems.push(`the composed workflow opens with the panels ${reading.visiblePanels.join(', ') || 'none'}, not ${flow.stages[0].id}`);
  }
  if (reading.stopsShown !== 0 || reading.statusShown) problems.push('the composed workflow opens with a stop chosen');
  if (!reading.proposalShown) problems.push('the composed workflow does not say on screen that it is proposed orchestration');
  if (reading.shipped !== count) problems.push(`${reading.shipped} of ${count} stages give what the operator does as shipped`);
  if (reading.proposed !== count) problems.push(`${reading.proposed} of ${count} stages give what moves them on as proposed`);
  const label = new Map(flow.families.map((family) => [family.id, family.label]));
  const tags = flow.stages.map((stage) => label.get(stage.family)).join(' | ');
  if (reading.tags.join(' | ') !== tags) problems.push(`the stages are tagged ${reading.tags.join(', ')}, not ${tags.split(' | ').join(', ')}`);
  return problems;
}

// The marks a reader at one stage should see on every step: the stages
// before it done, the stage itself current or stopped, and the stages after
// it ahead, or held when it stopped.
export function expectedStepMarks(flow, index, outcome) {
  const label = outcome ? flow.outcomes.find((one) => one.id === outcome)?.label : null;
  return flow.stages.map((stage, at) => {
    if (at < index) return ['done', 'Done'];
    if (at === index) return outcome ? ['stopped', `Stopped: ${label}`] : ['current', ''];
    return outcome ? ['held', 'Held'] : ['ahead', ''];
  });
}

// The address a reader at one stage should find: the view, the stage unless
// it is the first, and the stop.
export function expectedWorkflowSearch(flow, index, outcome) {
  const params = new URLSearchParams({ 'lx-view': 'workflow' });
  if (index > 0) params.set('lx-step', flow.stages[index].id);
  if (outcome) params.set('lx-outcome', outcome);
  return `?${params}`;
}

// workflowWalkProblems judges every stage chosen in turn with nothing
// stopped, and every stop each stage can stop on chosen there: the panel
// shown, the count, the stop pressed and its detail shown alone, the mark on
// every step in words, the status line, and the address written.
export function workflowWalkProblems(readings, flow) {
  const problems = [];
  if (readings.steps.length !== flow.stages.length) {
    problems.push(`${readings.steps.length} of the composed workflow’s ${flow.stages.length} stages were chosen`);
  }
  const marksProblem = (at, got, want) => {
    const shown = (marks) => marks.map(([state, text]) => `${state}${text ? ` "${text}"` : ''}`).join(', ');
    if (shown(got) !== shown(want)) problems.push(`${at} marks the steps ${shown(got)}, not ${shown(want)}`);
  };
  for (const reading of readings.steps) {
    const index = flow.stages.findIndex((stage) => stage.id === reading.id);
    const at = `choosing the stage "${reading.id}"`;
    if (reading.current !== reading.id) problems.push(`${at} makes "${reading.current}" current`);
    if (reading.panels.join(' ') !== reading.id) problems.push(`${at} shows the panels ${reading.panels.join(', ') || 'none'}`);
    if (!reading.count.startsWith(`Stage ${index + 1} of ${flow.stages.length}: `)) problems.push(`${at} counts "${reading.count}"`);
    marksProblem(at, reading.marks, expectedStepMarks(flow, index, null));
    if (reading.status !== null) problems.push(`${at} says it stopped: "${reading.status}"`);
    const search = expectedWorkflowSearch(flow, index, null);
    if (reading.search !== search) problems.push(`${at} writes ${reading.search || 'nothing'}, not ${search}`);
  }
  const want = flow.stages.reduce((sum, stage) => sum + stage.outcomes.length, 0);
  if (readings.stops.length !== want) problems.push(`${readings.stops.length} of the composed workflow’s ${want} stops were chosen`);
  for (const reading of readings.stops) {
    const index = flow.stages.findIndex((stage) => stage.id === reading.stage);
    const outcome = flow.outcomes.find((one) => one.id === reading.outcome);
    const at = `stopping "${reading.stage}" with "${reading.outcome}"`;
    if (reading.pressed.join(' ') !== reading.outcome) problems.push(`${at} presses ${reading.pressed.join(', ') || 'nothing'}`);
    if (reading.details.join(' ') !== reading.outcome) problems.push(`${at} shows the detail of ${reading.details.join(', ') || 'nothing'}`);
    marksProblem(at, reading.marks, expectedStepMarks(flow, index, reading.outcome));
    const later = flow.stages.slice(index + 1).map((stage) => stage.title);
    const status = reading.status ?? '';
    if (!status.includes(outcome?.label ?? reading.outcome) || !later.every((title) => status.includes(title))) {
      problems.push(`${at} says "${status}", which does not name the stop and every stage it holds`);
    }
    const search = expectedWorkflowSearch(flow, index, reading.outcome);
    if (reading.search !== search) problems.push(`${at} writes ${reading.search || 'nothing'}, not ${search}`);
  }
  // A stop is cleared by moving on, by choosing another stage, and by
  // choosing it again: none of them leaves a stop or a held stage behind.
  if (readings.clears.length === 0) problems.push('no stop was cleared');
  for (const { how, want: stage, got } of readings.clears) {
    const at = `clearing a stop by ${how}`;
    if (got.step !== stage) problems.push(`${at} opens "${got.step}", not "${stage}"`);
    if (got.outcome !== null) problems.push(`${at} keeps the stop "${got.outcome}"`);
    if (got.held !== 0) problems.push(`${at} leaves ${got.held} stages held`);
    if (new URLSearchParams(got.search).has('lx-outcome')) problems.push(`${at} writes ${got.search}, with lx-outcome`);
  }
  return problems;
}

// workflowLinkProblems judges pages opened from links into the composed
// workflow. The stop pressed is read from the stage's own options, which a
// stop the stage cannot have never presses, so the steps and the status line
// are read as well: a stop nobody chose must hold no stage and say nothing,
// and a stop the link names must be said.
export function workflowLinkProblems(readings) {
  const problems = [];
  if (readings.length === 0) return ['no link into the composed workflow was opened'];
  for (const { link, want, got } of readings) {
    if (got.view !== want.view) problems.push(`the link ${link} opens the ${got.view} view, not ${want.view}`);
    if (want.step !== undefined && got.step !== want.step) problems.push(`the link ${link} opens the stage ${got.step ?? 'none'}, not ${want.step}`);
    if (got.outcome !== want.outcome) problems.push(`the link ${link} stops on ${got.outcome ?? 'nothing'}, not ${want.outcome ?? 'nothing'}`);
    if (want.node !== undefined && got.node !== want.node) problems.push(`the link ${link} opens ${got.node ?? 'no object'}, not ${want.node ?? 'no object'}`);
    if (got.view !== 'workflow') continue;
    if (want.outcome === null && (got.held !== 0 || got.status !== null)) {
      problems.push(`the link ${link} shows a stop nobody chose: ${got.held} stages held, and the status says "${got.status ?? ''}"`);
    }
    if (want.outcome !== null && got.status === null) problems.push(`the link ${link} says nothing of the stop ${want.outcome}`);
  }
  return problems;
}

// workflowKeyboardProblems judges the composed workflow by keyboard alone:
// Tab and Enter from the heading choose it, Tab reaches the current step, the
// arrow keys, End and Home move between stages, and Tab and Enter choose a
// stop.
export function workflowKeyboardProblems(reading) {
  const problems = [];
  if (!reading.chosen) problems.push('Tab and Enter from the heading do not choose the composed workflow');
  if (!reading.step) problems.push('Tab does not reach the composed workflow’s current step');
  if (!reading.arrow) problems.push('the arrow key does not move to the next stage of the composed workflow');
  if (!reading.end) problems.push('End does not move to the last stage of the composed workflow');
  if (!reading.home) problems.push('Home does not move to the first stage of the composed workflow');
  if (!reading.stop) problems.push('Tab and Enter do not choose a stop of the composed workflow');
  return problems;
}

// anchorProblems judges the links between objects, sources and views, read
// from the built page. In the interactive view each is an anchor whose
// address names what it opens, with the value it selects in place; no
// related object is left a button, which no address opens; a related name
// stays inline, so the full stop after it cannot wrap alone; and in the text
// every link within the page leads somewhere a reader without JavaScript
// sees: an entry of the text or a heading of the page, never the hidden
// view. The page's ids are each used once, or a link to one leads to
// whichever comes first.
export function anchorProblems(reading, home) {
  const problems = [];
  const { text, app, buttons, duplicates } = reading.anchors;
  if (!text.some((one) => one.target === 'text')) problems.push('the text links none of its own entries');
  for (const { href, target } of text) {
    if (target === 'app' || target === 'none') {
      problems.push(`without JavaScript the link ${href} leads ${target === 'app' ? 'into the hidden view' : 'nowhere'}`);
    }
  }
  for (const kind of ['select', 'source', 'node', 'open']) {
    if (!app.some((one) => one.kind === kind)) problems.push(`the interactive view has no ${kind} link`);
  }
  for (const { href, kind, value, family, display } of app) {
    const at = `the ${kind} link ${href}`;
    const url = new URL(href, 'https://site.invalid/page/');
    const params = url.searchParams;
    if (url.pathname !== '/page/' || url.hash !== '#lifecycle-explorer') problems.push(`${at} does not lead to the explorer on this page`);
    if (kind === 'select') {
      if (params.get('lx-object') !== value) problems.push(`${at} names ${params.get('lx-object') ?? 'no object'}, and selects ${value}`);
      const named = params.get('lx-family');
      if (named !== (family === home ? null : family)) problems.push(`${at} names the family ${named ?? 'none'}, and sits in ${family}`);
      if (params.has('lx-view')) problems.push(`${at} names a view`);
    }
    if (kind === 'source' && (params.get('lx-view') !== 'audit' || params.get('lx-source') !== value)) {
      problems.push(`${at} does not name the audit view and ${value}`);
    }
    if (kind === 'node' && (params.get('lx-view') !== 'objects' || params.get('lx-node') !== value)) {
      problems.push(`${at} does not name the object map and ${value}`);
    }
    if (kind === 'open' && [...params.keys()].length === 0) problems.push(`${at} names nothing to open`);
    if (kind !== 'source' && kind !== 'open' && display !== 'inline') problems.push(`${at} is laid out ${display}, not inline`);
  }
  if (buttons > 0) problems.push(`${buttons} related objects or sources are buttons, which no address opens`);
  for (const id of duplicates) problems.push(`the id "${id}" is used twice on the page`);
  return problems;
}

// What an address into the explorer names, read the way the page reads it.
export function landingFor(href, home) {
  const params = new URL(href, 'https://site.invalid/page/').searchParams;
  const view = params.get('lx-view') ?? 'journey';
  if (view === 'audit') return { view, source: params.get('lx-source') };
  if (view === 'objects') return { view, node: params.get('lx-node') };
  if (view === 'workflow') return { view, step: params.get('lx-step'), outcome: params.get('lx-outcome') };
  return {
    view,
    family: params.get('lx-family') ?? home,
    path: params.get('lx-path') ?? undefined,
    stage: params.get('lx-stage') ?? undefined,
    object: params.get('lx-object'),
  };
}

// anchorLandingProblems judges every distinct link of the interactive view,
// opened as an address: the view it names is on screen with the value it
// names chosen.
export function anchorLandingProblems(readings, home) {
  const problems = [];
  if (readings.length === 0) return ['no link between objects, sources or views was opened'];
  for (const { href, got } of readings) {
    const want = landingFor(href, home);
    for (const [key, value] of Object.entries(want)) {
      if (value === undefined) continue;
      if (got[key] !== value) problems.push(`the link ${href} opens with ${key} ${got[key] ?? 'none'}, not ${value ?? 'none'}`);
    }
  }
  return problems;
}

// inPlaceProblems judges links followed by a click. A plain click opens what
// the link names without loading the page again, keeps the rest of the view
// it was made in, and writes the address the link names when it opens
// another view; a click with a modifier is the browser's, and opens the
// address in a new page while the view it was made in stays as it was.
export function inPlaceProblems(readings) {
  const problems = [];
  for (const kind of ['select', 'source', 'node', 'open']) {
    if (!readings.plain.some((one) => one.kind === kind)) problems.push(`no ${kind} link was followed by a click`);
  }
  for (const { kind, href, reloaded, landed, kept, search } of readings.plain) {
    const at = `a plain click on the ${kind} link ${href}`;
    if (reloaded) problems.push(`${at} loads the page again`);
    if (!landed) problems.push(`${at} does not open what it names`);
    if (kept === false) problems.push(`${at} does not keep the stage it was made at`);
    if (kind === 'open') {
      const want = new URL(href, 'https://site.invalid/page/').search;
      if (search !== want) problems.push(`${at} writes ${search || 'nothing'}, not ${want}`);
    }
  }
  if (!readings.modified) problems.push('no link was clicked with a modifier');
  else {
    const { href, opened, stayed } = readings.modified;
    const want = new URL(href, 'https://site.invalid/page/').search;
    if (opened !== want) problems.push(`a click with a modifier on ${href} opens ${opened ?? 'no page'}, not ${want}`);
    if (!stayed) problems.push(`a click with a modifier on ${href} also changes the view it was made in`);
  }
  return problems;
}

// What #817 asks of the depths: Overview, Product and Implementation, in that
// order, each showing more of the same entries. The parts are the ones the
// renderer tags, kind by kind, and the essentials are the parts an entry
// must say at Overview: who acts and what a person does, what an object or
// a source is for, and how a reader knows a step is done.
export const DEPTH_REQUIRED = {
  ids: ['overview', 'product', 'implementation'],
  parts: {
    stage: ['lead', 'you', 'done', 'phase', 'operator', 'expect', 'inspect', 'limits', 'kubernetes', 'executor', 'objects'],
    object: ['purpose', 'writer', 'inspect', 'lives'],
    source: ['establishes', 'cannot', 'records', 'readers', 'retention', 'survives', 'names', 'gaps', 'ids'],
    node: ['holds', 'writtenBy', 'change', 'deletion', 'inspect', 'gap', 'operator', 'scope'],
    wstage: ['goal', 'person', 'gate', 'done', 'operator', 'inspect', 'decision', 'limits', 'objects'],
    block: ['auditMeasured', 'auditIdentities', 'auditGaps', 'workflowPreconditions', 'workflowDecisions', 'workflowRecord'],
  },
  essentials: {
    stage: ['lead', 'you', 'done'],
    object: ['purpose', 'writer'],
    source: ['establishes', 'cannot'],
    node: ['holds', 'writtenBy'],
    wstage: ['goal', 'person', 'gate', 'done'],
  },
};

const rankOf = (ids, id) => ids.indexOf(id);

// depthModelProblems judges the depths and the table of parts against what
// #817 asks: three depths in order, the default the deepest so a link that
// names none shows what it always showed, every part the renderer tags named
// once at a depth that exists, each essential at Overview, and every kind of
// entry saying more at each depth than at the one before.
export function depthModelProblems({ depths, defaultDepth, depthOf }, required = DEPTH_REQUIRED) {
  const problems = [];
  const ids = (depths ?? []).map((one) => one.id);
  if (ids.join(' ') !== required.ids.join(' ')) problems.push(`the depths are ${ids.join(', ') || 'none'}, not ${required.ids.join(', ')}`);
  for (const one of depths ?? []) {
    if (!one.label?.trim() || !one.text?.trim()) problems.push(`the depth "${one.id}" lacks its label or what it shows`);
  }
  if (defaultDepth !== required.ids.at(-1)) {
    problems.push(`the default depth is "${defaultDepth}", not "${required.ids.at(-1)}", so a link that names no depth would show less than it did`);
  }
  const kinds = Object.keys(depthOf ?? {});
  for (const kind of Object.keys(required.parts)) {
    if (!kinds.includes(kind)) problems.push(`depthOf leaves out the kind "${kind}"`);
  }
  for (const kind of kinds) {
    const want = required.parts[kind];
    if (!want) {
      problems.push(`depthOf names the kind "${kind}", which nothing renders`);
      continue;
    }
    const parts = depthOf[kind];
    for (const name of want) {
      if (!(name in parts)) problems.push(`depthOf.${kind} leaves out the part "${name}"`);
    }
    for (const [name, at] of Object.entries(parts)) {
      if (!want.includes(name)) problems.push(`depthOf.${kind} names "${name}", which nothing renders`);
      if (!required.ids.includes(at)) problems.push(`depthOf.${kind}.${name} is "${at}", which is not a depth`);
    }
    for (const name of required.essentials[kind] ?? []) {
      if (name in parts && parts[name] !== required.ids[0]) {
        problems.push(`depthOf.${kind}.${name} first shows at "${parts[name]}", and ${required.ids[0]} has to say it`);
      }
    }
    if (kind === 'block') continue;
    for (const at of required.ids) {
      if (!Object.values(parts).includes(at)) problems.push(`depthOf.${kind} shows nothing first at "${at}"`);
    }
  }
  return problems;
}

// expectedParts is the parts one entry carries where it is rendered: the
// interactive view's panel or the text's entry. A part of a field the entry
// leaves out is not expected, an actor's row is `lead` when that actor
// leads, and a source's names and closed gaps are listed in its panel alone,
// since the text gives both a block of their own.
export function expectedParts(kind, entry, where, context = {}) {
  const parts = [];
  const add = (name, when = true) => when && parts.push(name);
  if (kind === 'stage') {
    add('lead');
    for (const actor of ['you', 'operator', 'kubernetes', 'executor']) add(actor, Boolean(entry.actors[actor]) && actor !== entry.lead);
    add('phase');
    add('objects');
    add('expect', Boolean(entry.expect));
    add('inspect');
    add('done');
    add('limits', Boolean(entry.limits));
  } else if (kind === 'object') {
    for (const name of ['purpose', 'writer', 'inspect', 'lives']) add(name);
  } else if (kind === 'source') {
    for (const name of ['records', 'establishes', 'cannot', 'ids', 'readers', 'retention', 'survives']) add(name);
    add('names', where === 'app' && (context.identities ?? []).some((identity) => identity.recordedIn.includes(entry.id)));
    add('gaps', where === 'app' && (context.gaps ?? []).some((gap) => gap.sources.includes(entry.id)));
  } else if (kind === 'node') {
    for (const name of ['holds', 'writtenBy', 'operator', 'scope', 'change', 'deletion', 'inspect']) add(name);
    add('gap', Boolean(entry.gap));
  } else if (kind === 'wstage') {
    for (const name of ['goal', 'person', 'inspect', 'operator', 'gate', 'done', 'objects']) add(name);
    add('decision', Boolean(entry.decision));
    add('limits', Boolean(entry.limits));
  }
  return parts.sort();
}

// depthRenderProblems judges what the page tags against the content: every
// entry, in the interactive view and in the text, carries each part it has
// and no other, every tagged part sits at the depth the table gives it, and
// every block is tagged in both.
export function depthRenderProblems(reading, { depthOf, entries, context }) {
  const problems = [];
  const found = new Map((reading.entries ?? []).map((one) => [`${one.where} ${one.kind} ${one.id}`, one]));
  for (const where of ['app', 'text']) {
    for (const [kind, list] of Object.entries(entries)) {
      for (const entry of list) {
        const at = `the ${where === 'app' ? 'interactive view' : 'text'}’s ${kind} "${entry.id}"`;
        const got = found.get(`${where} ${kind} ${entry.id}`);
        if (!got) {
          problems.push(`${at} is not rendered with its parts`);
          continue;
        }
        const want = expectedParts(kind, entry, where, context);
        const names = [...new Set(got.parts.map(([name]) => name))].sort();
        if (names.join(' ') !== want.join(' ')) problems.push(`${at} carries the parts ${names.join(', ') || 'none'}, not ${want.join(', ')}`);
      }
    }
    for (const name of Object.keys(depthOf.block)) {
      if (!(reading.blocks ?? []).some((one) => one.where === where && one.name === name)) {
        problems.push(`the ${where === 'app' ? 'interactive view' : 'text'} does not tag the block "${name}"`);
      }
    }
  }
  for (const one of reading.entries ?? []) {
    for (const [name, at] of one.parts) {
      const want = depthOf[one.kind]?.[name];
      if (want !== at) problems.push(`the ${one.where}’s ${one.kind} "${one.id}" shows ${name} at "${at}", not "${want ?? 'nowhere'}"`);
    }
  }
  for (const one of reading.blocks ?? []) {
    if (depthOf.block[one.name] !== one.at) problems.push(`the ${one.where}’s block "${one.name}" shows at "${one.at}", not "${depthOf.block[one.name] ?? 'nowhere'}"`);
  }
  for (const stray of reading.strays ?? []) problems.push(`a part "${stray}" sits outside any entry or block`);
  return problems;
}

// depthShownProblems judges the page at each depth chosen: every tagged part
// shows when its depth is no deeper than the one chosen and is hidden when it
// is; exactly the chosen depth's note shows; one entry of each kind, open on
// screen, shows the parts it should and only those; and a link in the text
// never leads to a part the depth hides. `where` says which page was read,
// with or without its script, and `text` that the text has to be on screen.
export function depthShownProblems(readings, { ids, where, text = false }) {
  const problems = [];
  for (const id of ids) {
    if (!readings.some((one) => one.depth === id)) problems.push(`${where}: the depth "${id}" was never chosen`);
  }
  for (const reading of readings) {
    const at = `${where} at ${reading.depth}`;
    const rank = rankOf(ids, reading.depth);
    if (reading.checked !== reading.depth) problems.push(`${at}: the choice reads "${reading.checked}"`);
    if ((reading.notes ?? []).join(' ') !== reading.depth) problems.push(`${at}: the notes shown are ${(reading.notes ?? []).join(', ') || 'none'}, not ${reading.depth}`);
    if ((reading.parts ?? []).length === 0) problems.push(`${at}: no part was read`);
    const wrong = new Map();
    for (const [place, part, depth, shown] of reading.parts ?? []) {
      const should = rankOf(ids, depth) <= rank;
      if (shown !== should) {
        const key = `${place} ${part} ${shown ? 'shown' : 'hidden'}`;
        wrong.set(key, (wrong.get(key) ?? 0) + 1);
      }
    }
    for (const [key, count] of wrong) {
      const [place, part, state] = key.split(' ');
      problems.push(`${at}: ${count} ${place} ${part} part(s) are ${state}`);
    }
    for (const { kind, id, parts, want } of reading.open ?? []) {
      const expected = [...new Set(want.filter(([, depth]) => rankOf(ids, depth) <= rank).map(([name]) => name))].sort();
      const shown = [...new Set(parts)].sort();
      if (shown.join(' ') !== expected.join(' ')) problems.push(`${at}: the ${kind} "${id}" on screen shows ${shown.join(', ') || 'nothing'}, not ${expected.join(', ')}`);
    }
    if (text && !reading.textShown) problems.push(`${at}: the text is not on screen`);
    if (reading.textShown && !reading.links) problems.push(`${at}: no link of the text was read`);
    for (const link of reading.hiddenTargets ?? []) problems.push(`${at}: the link ${link} leads to a part this depth hides`);
  }
  return problems;
}

// depthAddressProblems judges the address the depth choice writes: a depth
// other than the default as lx-depth, the default as no parameter, and every
// other parameter left as it was.
export function depthAddressProblems(readings, { ids, defaultDepth }) {
  const problems = [];
  if (readings.length === 0) return ['no depth was chosen in the interactive view'];
  for (const { depth, before, search } of readings) {
    const got = new URLSearchParams(search);
    const want = depth === defaultDepth ? null : depth;
    if (got.get('lx-depth') !== want) problems.push(`choosing ${depth} writes ${search || 'nothing'}, with lx-depth ${got.get('lx-depth') ?? 'absent'}, not ${want ?? 'absent'}`);
    got.delete('lx-depth');
    const kept = new URLSearchParams(before);
    kept.delete('lx-depth');
    if (got.toString() !== kept.toString()) problems.push(`choosing ${depth} changes the rest of the address from ?${kept} to ?${got}`);
  }
  for (const id of ids) {
    if (!readings.some((one) => one.depth === id)) problems.push(`the depth "${id}" was never chosen in the interactive view`);
  }
  return problems;
}

// depthKeepProblems judges what happens to a depth while the reader moves:
// choosing a view, a family, a path, a stage or a link leaves it chosen and
// in the address, and a link that names a depth opens at it, while one that
// names none or a depth that does not exist opens at the default.
export function depthKeepProblems({ moves, links }, { defaultDepth }) {
  const problems = [];
  if ((moves ?? []).length === 0) problems.push('no move was made with a depth chosen');
  for (const { how, want, checked, search, hidden } of moves ?? []) {
    if (checked !== want) problems.push(`${how} leaves the depth at ${checked}, not ${want}`);
    if (new URLSearchParams(search).get('lx-depth') !== want) problems.push(`${how} writes ${search || 'nothing'}, without lx-depth=${want}`);
    if (!hidden) problems.push(`${how} shows what ${want} hides`);
  }
  if ((links ?? []).length === 0) problems.push('no link that names a depth was opened');
  for (const { link, want, got } of links ?? []) {
    if (got.depth !== want.depth) problems.push(`the link ${link} opens at the depth ${got.depth}, not ${want.depth}`);
    if (want.view && got.view !== want.view) problems.push(`the link ${link} opens the ${got.view} view, not ${want.view}`);
  }
  if (!(links ?? []).some((one) => one.want.depth === defaultDepth && /lx-depth=/.test(one.link))) {
    problems.push('no link that names a depth nobody defined was opened');
  }
  return problems;
}

// depthKeyboardProblems judges the depth choice by keyboard: Tab from the
// heading reaches it first, and the arrow keys choose the depth before and
// after, which applies at once and is written to the address.
export function depthKeyboardProblems(reading) {
  const problems = [];
  if (!reading.reached) problems.push('Tab from the heading does not reach the depth choice');
  if (!reading.back) problems.push('the arrow key does not choose the shallower depth');
  if (!reading.applied) problems.push('a depth chosen by keyboard does not hide what it hides');
  if (!reading.written) problems.push('a depth chosen by keyboard is not written to the address');
  if (!reading.forward) problems.push('the arrow key does not choose the deeper depth again');
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


  // The audit view: a model of two sources, held to what it requires of
  // itself, then each way the model, the text, the view and the links can
  // fall short.
  const verdict = (id) => ({ verdict: id, basis: 'read', text: `after, ${id}` });
  const auditSource = (id) => ({
    id,
    label: `Label ${id}`,
    group: 'g',
    records: `Records ${id}.`,
    establishes: 'Establishes.',
    cannot: `Cannot ${id}.`,
    retention: `Kept ${id}.`,
    ids: ['uid'],
    readers: [{ command: `kubectl ptah schema ${id}`, says: 'Says.' }],
    links: [{ label: 'More', href: '#more' }],
    survives: { job: verdict('kept'), resource: verdict('gone'), namespace: verdict('gone'), cluster: verdict('gone') },
  });
  const identity = (id) => ({ id, label: `Who ${id}`, text: 'Text.', recordedIn: ['a-one'] });
  const auditFixture = {
    losses: ['job', 'resource', 'namespace', 'cluster'].map((id) => ({ id, label: id })),
    verdicts: [
      { id: 'kept', label: 'Kept' },
      { id: 'gone', label: 'Gone' },
    ],
    bases: [{ id: 'measured' }, { id: 'read' }],
    groups: [{ id: 'g' }],
    sources: [auditSource('a-one'), { ...auditSource('a-two'), readers: [{ system: 'A store of its own.', says: 'Says.' }] }],
    identities: ['author', 'approver', 'executor', 'actor'].map(identity),
    gaps: [{ issue: 1, version: '0.1.0', title: 'Title', text: 'Text.', sources: ['a-one'] }],
    deferred: [{ id: 'later', title: 'Later', text: 'Text.' }],
  };
  const auditRequired = {
    sources: ['a-one', 'a-two'],
    losses: ['job', 'resource', 'namespace', 'cluster'],
    identities: ['author', 'approver', 'executor', 'actor'],
    gaps: [1],
  };
  const withSource = (index, change) => ({
    ...auditFixture,
    sources: auditFixture.sources.map((source, at) => (at === index ? { ...source, ...change } : source)),
  });
  if (auditProblems(auditFixture, models, auditRequired).length !== 0) {
    fail('auditProblems refused a model that holds', auditProblems(auditFixture, models, auditRequired));
  }
  for (const [name, spoiled, want] of [
    ['a required source dropped', { ...auditFixture, sources: [auditFixture.sources[0]] }, 'leaves out the source "a-two"'],
    ['a source id used twice', { ...auditFixture, sources: [auditFixture.sources[0], auditFixture.sources[0], auditFixture.sources[1]] }, 'source "a-one" is used twice'],
    ['a source without its prefix', withSource(1, { id: 'two-b' }), 'lacks the prefix a-'],
    ['a source id a family uses', withSource(1, { id: 'b-one' }), 'shares its id with a family'],
    ['a source in no group', withSource(0, { group: 'nowhere' }), 'group "nowhere", which does not exist'],
    ['a group with no source', { ...auditFixture, groups: [{ id: 'g' }, { id: 'empty' }] }, 'group "empty" holds no source'],
    ['a source that cannot establish nothing', withSource(0, { cannot: ' ' }), 'says nothing in cannot'],
    ['a source with no retention', withSource(0, { retention: '' }), 'says nothing in retention'],
    ['a source with no identifier', withSource(0, { ids: [] }), 'names no correlation identifier'],
    ['a source with no reader', withSource(0, { readers: [] }), 'names no reader'],
    ['a reader that is neither', withSource(0, { readers: [{ says: 'Says.' }] }), 'neither a command nor a system'],
    ['a reader that is both', withSource(0, { readers: [{ command: 'c', system: 's', says: 'Says.' }] }), 'both a command and a system'],
    ['a reader that says nothing', withSource(0, { readers: [{ command: 'c' }] }), 'does not say what it shows'],
    ['a source with no link', withSource(0, { links: [] }), 'links nothing'],
    [
      'a loss with no verdict',
      withSource(0, { survives: { job: verdict('kept'), resource: verdict('gone'), namespace: verdict('gone') } }),
      'what is left after "cluster"',
    ],
    ['an unknown verdict', withSource(0, { survives: { ...auditFixture.sources[0].survives, job: verdict('lost') } }), 'the verdict "lost", which does not exist'],
    [
      'an unknown basis',
      withSource(0, { survives: { ...auditFixture.sources[0].survives, job: { ...verdict('kept'), basis: 'guessed' } } }),
      'the basis "guessed", which does not exist',
    ],
    ['a verdict with no text', withSource(0, { survives: { ...auditFixture.sources[0].survives, job: { ...verdict('kept'), text: '' } } }), 'gives "job" no text'],
    ['a verdict for no loss', withSource(0, { survives: { ...auditFixture.sources[0].survives, upgrade: verdict('kept') } }), '"upgrade", which is not a loss'],
    ['a loss dropped', { ...auditFixture, losses: auditFixture.losses.slice(0, 3) }, 'the audit losses are job, resource, namespace,'],
    ['an identity dropped', { ...auditFixture, identities: auditFixture.identities.slice(0, 3) }, 'the audit identities are author, approver, executor,'],
    [
      'an identity recorded nowhere',
      { ...auditFixture, identities: [{ ...identity('author'), recordedIn: [] }, ...auditFixture.identities.slice(1)] },
      'identity "author" is recorded nowhere',
    ],
    [
      'an identity recorded in no source',
      { ...auditFixture, identities: [{ ...identity('author'), recordedIn: ['a-nowhere'] }, ...auditFixture.identities.slice(1)] },
      'recorded in "a-nowhere", which is not a source',
    ],
    ['a required gap dropped', { ...auditFixture, gaps: [] }, 'leaves out the gap #1'],
    ['a gap with no release', { ...auditFixture, gaps: [{ ...auditFixture.gaps[0], version: 'soon' }] }, 'names no release that closed it'],
    ['a gap naming no source', { ...auditFixture, gaps: [{ ...auditFixture.gaps[0], sources: ['a-nowhere'] }] }, 'names "a-nowhere", which is not a source'],
    ['nothing deferred', { ...auditFixture, deferred: [] }, 'lists nothing deferred'],
  ]) {
    refuses(name, auditProblems(spoiled, models, auditRequired), want);
  }
  refuses('no audit source at all', auditProblems({ ...auditFixture, sources: [] }, models, auditRequired), 'has no source');

  // Commands: every one the explorer shows is read, and a projection or a raw
  // dump in any of them is refused.
  const shown = explorerCommands(
    [
      {
        scenario: { stages: [{ id: 's1', inspect: [{ command: 'kubectl ptah schema s1' }] }] },
        paths: [{ id: 'p', stages: [{ id: 's2', inspect: [{ command: 'kubectl ptah plan s2' }] }] }],
        objects: [{ id: 'o', inspect: [{ command: 'kubectl ptah history schema/o' }] }],
      },
    ],
    auditFixture,
    { nodes: [{ id: 'o-n', inspect: [{ command: 'kubectl ptah migration o-n' }] }] },
    {
      stages: [{ id: 'w-s', inspect: [{ command: 'kubectl ptah wait schema/w-s --for in-sync' }, { system: 'A client.', says: 'Rows.' }] }],
      readers: [{ command: 'kubectl ptah history schema/w' }],
    },
  );
  if (
    shown.map((one) => one.where).join(' | ') !==
    'the stage "s1" | the stage "s2" | the object "o" | the source "a-one" | the object map’s "o-n" | the workflow’s "w-s" | the workflow’s stage record'
  ) {
    fail('explorerCommands read the commands as', shown);
  }
  if (commandProblems(shown).length !== 0) fail('commandProblems refused supported commands', commandProblems(shown));
  for (const [name, command, want] of [
    ['a jq filter', 'kubectl -n ptah-system logs -l app=x -c manager | jq -c .', 'a jq filter'],
    ['jq on its own', "jq -c 'select(.logger == \"journal\")' journal.log", 'a jq filter'],
    ['a JSONPath projection', "kubectl get ptahmigration orders -o jsonpath='{.status.phase}'", 'a JSONPath projection'],
    ['a Go template', "kubectl get ptahschema x -o go-template='{{.status.phase}}'", 'a Go template'],
    ['custom columns', 'kubectl get ptahschemas -o custom-columns=NAME:.metadata.name', 'a custom-columns projection'],
    ['a raw dump', 'kubectl -n application get ptahschemaapprovals -o yaml', 'a raw object dump'],
  ]) {
    refuses(name, commandProblems([...shown, { where: 'the stage "x"', command }]), want);
  }
  refuses('no command at all', commandProblems([]), 'shows no command');

  const auditText = {
    audit: {
      sources: auditFixture.sources.map((source) => ({
        id: source.id,
        text: `${source.label}. ${source.records} ${source.cannot} ${source.retention} ${source.readers
          .map((reader) => reader.command ?? reader.system)
          .join(' ')}`,
        survives: ['job', 'resource', 'namespace', 'cluster'],
      })),
      identities: ['author', 'approver', 'executor', 'actor'],
      gaps: [{ issue: '1', href: 'https://github.com/stokaro/ptah-operator/issues/1' }],
    },
  };
  const textSource = (index, change) => ({
    audit: { ...auditText.audit, sources: auditText.audit.sources.map((source, at) => (at === index ? { ...source, ...change } : source)) },
  });
  if (auditTextProblems(auditText, auditFixture).length !== 0) {
    fail('auditTextProblems refused a text that holds', auditTextProblems(auditText, auditFixture));
  }
  for (const [name, spoiled, want] of [
    ['no audit text', { audit: null }, 'leaves out the audit view'],
    ['a source dropped from the text', { audit: { ...auditText.audit, sources: [auditText.audit.sources[0]] } }, 'leaves out the source "a-two"'],
    ['a source without its records', textSource(0, { text: 'Label a-one. Cannot a-one. Kept a-one. kubectl ptah schema a-one' }), 'lacks its records'],
    ['a source without its limits', textSource(0, { text: 'Label a-one. Records a-one. Kept a-one. kubectl ptah schema a-one' }), 'lacks its cannot'],
    ['a source without its command', textSource(0, { text: 'Label a-one. Records a-one. Cannot a-one. Kept a-one.' }), 'lacks the reader "kubectl ptah schema a-one"'],
    ['a source without its system', textSource(1, { text: 'Label a-two. Records a-two. Cannot a-two. Kept a-two.' }), 'lacks the reader "A store of its own."'],
    ['a loss left unsaid', textSource(1, { survives: ['job', 'resource', 'namespace'] }), 'source "a-two" says nothing after "cluster"'],
    ['an identity dropped from the text', { audit: { ...auditText.audit, identities: ['author', 'approver', 'executor'] } }, 'leaves out the identity "actor"'],
    ['a gap dropped from the text', { audit: { ...auditText.audit, gaps: [] } }, 'leaves out the gap #1'],
    ['a gap linking elsewhere', { audit: { ...auditText.audit, gaps: [{ issue: '1', href: 'https://github.com/stokaro/ptah-operator/issues/10' }] } }, 'gap #1 links'],
  ]) {
    refuses(name, auditTextProblems(spoiled, auditFixture), want);
  }

  const auditRender = { viewButtons: VIEWS.length, shown: true, journeyShown: false, sourceButtons: 2, panels: 2, lossButtons: 4, visiblePanels: 0, marked: 0 };
  if (auditRenderProblems(auditRender, auditFixture).length !== 0) {
    fail('auditRenderProblems refused a view that holds', auditRenderProblems(auditRender, auditFixture));
  }
  for (const [name, change, want] of [
    ['a view button missing', { viewButtons: 1 }, '1 view buttons'],
    ['an audit view that stays hidden', { shown: false }, 'does not show it'],
    ['a journey left on screen', { journeyShown: true }, 'leaves the change journey on screen'],
    ['a source button missing', { sourceButtons: 1 }, '1 source buttons'],
    ['a source panel too many', { panels: 3 }, '3 source panels'],
    ['a loss button missing', { lossButtons: 3 }, '3 loss buttons'],
    ['a panel open at the start', { visiblePanels: 1 }, 'opens with 1 source panels shown'],
    ['sources marked at the start', { marked: 2 }, 'opens with 2 sources marked'],
  ]) {
    refuses(name, auditRenderProblems({ ...auditRender, ...change }, auditFixture), want);
  }

  const lossReading = (loss) => ({
    loss,
    pressed: [loss],
    verdicts: Object.fromEntries(
      auditFixture.sources.map((source) => {
        const id = source.survives[loss].verdict;
        return [source.id, { verdict: id, badge: id === 'kept' ? 'Kept' : 'Gone' }];
      }),
    ),
  });
  const auditSelection = {
    losses: auditFixture.losses.map((loss) => lossReading(loss.id)),
    sources: auditFixture.sources.map((source) => ({ source: source.id, loss: 'cluster', panels: [source.id], pressed: [source.id], current: 'cluster' })),
    chip: { want: 'a-one', got: 'a-one' },
  };
  const withLoss = (change) => ({ ...auditSelection, losses: [{ ...auditSelection.losses[0], ...change }, ...auditSelection.losses.slice(1)] });
  const withPicked = (change) => ({ ...auditSelection, sources: [{ ...auditSelection.sources[0], ...change }, ...auditSelection.sources.slice(1)] });
  if (auditSelectionProblems(auditSelection, auditFixture).length !== 0) {
    fail('auditSelectionProblems refused readings that hold', auditSelectionProblems(auditSelection, auditFixture));
  }
  for (const [name, spoiled, want] of [
    ['a loss never chosen', { ...auditSelection, losses: auditSelection.losses.slice(1) }, 'loss "job" was never chosen'],
    ['two losses pressed', withLoss({ pressed: ['job', 'cluster'] }), 'choosing "job" presses job, cluster'],
    ['a wrong verdict', withLoss({ verdicts: { ...lossReading('job').verdicts, 'a-one': { verdict: 'gone', badge: 'Gone' } } }), 'reads gone "Gone", not kept "Kept"'],
    ['a verdict in style alone', withLoss({ verdicts: { ...lossReading('job').verdicts, 'a-one': { verdict: 'kept', badge: '' } } }), 'reads kept "", not kept "Kept"'],
    ['a source off screen', withLoss({ verdicts: { 'a-one': lossReading('job').verdicts['a-one'] } }), 'source "a-two" is not on screen'],
    ['a source never selected', { ...auditSelection, sources: auditSelection.sources.slice(1) }, 'source "a-one" was never selected'],
    ['another source’s panel', withPicked({ panels: ['a-two'] }), 'selecting "a-one" shows the panels a-two'],
    ['two panels at once', withPicked({ panels: ['a-one', 'a-two'] }), 'shows the panels a-one, a-two'],
    ['two sources pressed', withPicked({ pressed: ['a-one', 'a-two'] }), 'selecting "a-one" presses a-one, a-two'],
    ['the chosen loss unmarked in the panel', withPicked({ current: null }), 'marks the row none'],
    ['an identity’s source that opens nothing', { ...auditSelection, chip: { want: 'a-one', got: null } }, 'opens nothing'],
    ['no identity’s source followed', { ...auditSelection, chip: null }, 'no identity’s source was followed'],
  ]) {
    refuses(name, auditSelectionProblems(spoiled, auditFixture), want);
  }

  const switchFlow = { stages: [{ id: 'w-a' }, { id: 'w-b' }] };
  const rested = { step: 'w-a', outcome: null };
  const toAudit = { to: 'audit', from: 'two on b-nested', got: { view: 'audit', loss: null, source: null, node: null, ...rested, search: '?lx-view=audit' } };
  const toJourney = {
    to: 'journey',
    from: 'the audit view after cluster with a-two',
    got: { view: 'journey', family: 'one', path: 'main', stage: 'one', object: null, ...rested, search: '?lx-stage=one' },
  };
  const toObjects = {
    to: 'objects',
    from: 'the audit view after cluster with a-two',
    got: { view: 'objects', loss: null, source: null, node: null, ...rested, search: '?lx-view=objects' },
  };
  const toWorkflow = {
    to: 'workflow',
    from: 'the object map with o-job open',
    got: { view: 'workflow', loss: null, source: null, node: null, ...rested, search: '?lx-view=workflow' },
  };
  const switchesThatHold = [toAudit, toJourney, toObjects, toWorkflow];
  if (viewSwitchProblems(switchesThatHold, models, switchFlow).length !== 0) {
    fail('viewSwitchProblems refused switches that hold', viewSwitchProblems(switchesThatHold, models, switchFlow));
  }
  const spoilTo = (one, change) => [{ ...one, got: { ...one.got, ...change } }];
  for (const [name, spoiled, want] of [
    ['no switch to the audit view', [toJourney, toObjects, toWorkflow], 'no switch went to the audit view'],
    ['no switch to the journey', [toAudit, toObjects, toWorkflow], 'no switch went to the journey view'],
    ['no switch to the workflow', [toAudit, toJourney, toObjects], 'no switch went to the workflow view'],
    ['a workflow left at the stage it was at', [toAudit, toJourney, toObjects, ...spoilTo(toWorkflow, { step: 'w-b' })], 'leaves the composed workflow at w-b, not w-a'],
    ['a workflow that keeps its stop', [toAudit, toJourney, toObjects, ...spoilTo(toWorkflow, { outcome: 'w-failed' })], 'keeps the stop w-failed'],
    ['a workflow that writes its stage', [toAudit, toJourney, toObjects, ...spoilTo(toWorkflow, { search: '?lx-view=workflow&lx-step=w-b' })], 'with lx-step'],
    ['a workflow written as another view', [toAudit, toJourney, toObjects, ...spoilTo(toWorkflow, { search: '?lx-view=objects' })], 'not lx-view=workflow'],
    ['an object map that keeps the workflow’s stop', [toAudit, toJourney, ...spoilTo(toObjects, { outcome: 'w-failed' }), toWorkflow], 'keeps the stop w-failed'],
    ['a journey that keeps the workflow’s address', [toAudit, ...spoilTo(toJourney, { search: '?lx-stage=one&lx-outcome=w-failed' }), toObjects, toWorkflow], 'with lx-outcome'],
    ['an audit view that does not show', [...spoilTo(toAudit, { view: 'journey' }), toJourney], 'shows the journey view'],
    ['an audit view that keeps a loss', [...spoilTo(toAudit, { loss: 'cluster' }), toJourney], 'keeps the loss cluster'],
    ['an audit view that keeps a source', [...spoilTo(toAudit, { source: 'a-two' }), toJourney], 'keeps the source a-two'],
    ['an audit view that does not write itself', [...spoilTo(toAudit, { search: '' }), toJourney], 'not lx-view=audit'],
    ['an audit view that keeps a stage', [...spoilTo(toAudit, { search: '?lx-view=audit&lx-stage=b-one' }), toJourney], 'with lx-stage'],
    ['a journey in the family it was left in', [toAudit, ...spoilTo(toJourney, { family: 'two' })], 'opens the family two, not one'],
    ['a journey on the path it was left on', [toAudit, ...spoilTo(toJourney, { path: 'side' })], 'opens the path side, not main'],
    ['a journey at the stage it was left at', [toAudit, ...spoilTo(toJourney, { stage: 'two' })], 'opens the stage two, not one'],
    ['a journey with its object still selected', [toAudit, ...spoilTo(toJourney, { object: 'a' })], 'keeps a selected'],
    ['a journey that keeps the audit view’s address', [toAudit, ...spoilTo(toJourney, { search: '?lx-stage=one&lx-source=a-two' })], 'with lx-source'],
    ['a journey that writes no stage', [toAudit, ...spoilTo(toJourney, { search: '' })], 'not lx-stage=one'],
    ['no switch to the object map', [toAudit, toJourney], 'no switch went to the objects view'],
    ['an object map that does not show', [toAudit, toJourney, ...spoilTo(toObjects, { view: 'audit' })], 'shows the audit view'],
    ['an object map that keeps an object', [toAudit, toJourney, ...spoilTo(toObjects, { node: 'o-job' })], 'keeps the object o-job'],
    ['an object map written as another view', [toAudit, toJourney, ...spoilTo(toObjects, { search: '?lx-view=audit' })], 'not lx-view=objects'],
    [
      'an object map that keeps the audit view’s address',
      [toAudit, toJourney, ...spoilTo(toObjects, { search: '?lx-view=objects&lx-source=a-two' })],
      'with lx-source',
    ],
    ['an audit view that keeps an object', [...spoilTo(toAudit, { node: 'o-job' }), toJourney, toObjects], 'keeps the object o-job'],
    [
      'an audit view that keeps the object map’s address',
      [...spoilTo(toAudit, { search: '?lx-view=audit&lx-node=o-job' }), toJourney, toObjects],
      'with lx-node',
    ],
    [
      'a journey that keeps the object map’s address',
      [toAudit, ...spoilTo(toJourney, { search: '?lx-stage=one&lx-node=o-job' }), toObjects],
      'with lx-node',
    ],
  ]) {
    refuses(name, viewSwitchProblems(spoiled, models, switchFlow), want);
  }

  const auditLink = { link: '?lx-view=audit&lx-loss=cluster&lx-source=a-two', want: { view: 'audit', loss: 'cluster', source: 'a-two' } };
  const journeyLink = { link: '?lx-source=a&lx-loss=one', want: { view: 'journey', loss: null, source: null, family: 'one' } };
  const linksThatHold = [
    { ...auditLink, got: { ...auditLink.want, family: 'one' } },
    { ...journeyLink, got: journeyLink.want },
  ];
  if (auditLinkProblems(linksThatHold).length !== 0) fail('auditLinkProblems refused links that hold', auditLinkProblems(linksThatHold));
  refuses('a link that opens the journey', auditLinkProblems([{ ...auditLink, got: { ...auditLink.want, view: 'journey' } }]), 'opens the journey view, not audit');
  refuses('a link whose loss was dropped', auditLinkProblems([{ ...auditLink, got: { ...auditLink.want, loss: null } }]), 'chooses the loss none, not cluster');
  refuses('a link whose source was dropped', auditLinkProblems([{ ...auditLink, got: { ...auditLink.want, source: null } }]), 'opens no source, not a-two');
  refuses('a link into the wrong family', auditLinkProblems([{ ...journeyLink, got: { ...journeyLink.want, family: 'two' } }]), 'opens the family two, not one');
  refuses('no link into the audit view', auditLinkProblems([]), 'no link into the audit view');

  const keysThatHold = { chosen: true, loss: true, source: true };
  if (auditKeyboardProblems(keysThatHold).length !== 0) fail('auditKeyboardProblems refused keys that hold', auditKeyboardProblems(keysThatHold));
  refuses('a view the keyboard cannot choose', auditKeyboardProblems({ ...keysThatHold, chosen: false }), 'do not choose the audit view');
  refuses('a loss the keyboard cannot choose', auditKeyboardProblems({ ...keysThatHold, loss: false }), 'choose a loss');
  refuses('a source the keyboard cannot open', auditKeyboardProblems({ ...keysThatHold, source: false }), 'open a source');

  // The object map: a fixture of its own, small enough to read, with an
  // owner reference and a name between one pair, a Lease in another
  // namespace, a cluster-scoped kind and a database outside the cluster.
  const mapNode = (id, extra = {}) => ({
    id,
    label: `Label ${id}`,
    kind: `Kind ${id}`,
    group: 'person',
    scope: 'namespace',
    families: ['one'],
    writtenBy: `Writer ${id}.`,
    holds: `Holds ${id}.`,
    operator: `Operator ${id}.`,
    change: { verdict: 'editable', text: `Change ${id}.` },
    deletion: `Deletion ${id}.`,
    inspect: [{ command: `kubectl ptah schema ${id}`, says: 'Says.' }],
    links: [{ label: 'L', href: '#l' }],
    ...extra,
  });
  const mapFixture = {
    groups: [
      { id: 'person', label: 'P' },
      { id: 'operator', label: 'O' },
      { id: 'outside', label: 'X' },
    ],
    scopes: [{ id: 'namespace' }, { id: 'coordination' }, { id: 'cluster' }, { id: 'outside' }],
    changes: [
      { id: 'editable', label: 'Editable' },
      { id: 'immutable', label: 'Immutable' },
    ],
    types: [
      { id: 'owns', label: 'Owns', inverse: 'Owned by' },
      { id: 'references', label: 'Names', inverse: 'Named by' },
      { id: 'holds', label: 'Holds', inverse: 'Held by' },
      { id: 'executes', label: 'Runs SQL on', inverse: 'SQL run by' },
    ],
    nodes: [
      mapNode('o-res', { kind: 'Res' }),
      mapNode('o-plan', { kind: 'PtahSchemaPlan', group: 'operator', change: { verdict: 'immutable', text: 'Change o-plan.' } }),
      mapNode('o-job', { kind: 'Job', group: 'operator' }),
      mapNode('o-ok', { kind: 'PtahSchemaApproval' }),
      mapNode('o-lock', { kind: 'Lease', group: 'operator', scope: 'coordination' }),
      mapNode('o-realm', { kind: 'Realm', scope: 'cluster' }),
      mapNode('o-db', { kind: 'Database', group: 'outside', scope: 'outside', gap: 'Gap o-db.' }),
    ],
    relations: [
      { from: 'o-res', to: 'o-plan', type: 'owns', text: 'R owns P.' },
      { from: 'o-res', to: 'o-plan', type: 'references', text: 'R names P.' },
      { from: 'o-res', to: 'o-job', type: 'owns', text: 'R owns J.' },
      { from: 'o-job', to: 'o-plan', type: 'references', text: 'J names P.' },
      { from: 'o-ok', to: 'o-plan', type: 'references', text: 'A names P.' },
      { from: 'o-res', to: 'o-lock', type: 'holds', text: 'R holds L.' },
      { from: 'o-res', to: 'o-realm', type: 'references', text: 'R names the realm.' },
      { from: 'o-job', to: 'o-db', type: 'executes', text: 'J runs SQL on D.' },
    ],
    distinctions: ['D.'],
    links: [{ label: 'L', href: '#l' }],
  };
  const mapKinds = [
    { file: 'res.yaml', kind: 'Res', scope: 'Namespaced' },
    { file: 'realm.yaml', kind: 'Realm', scope: 'Cluster' },
    { file: 'plan.yaml', kind: 'PtahSchemaPlan', scope: 'Namespaced' },
    { file: 'ok.yaml', kind: 'PtahSchemaApproval', scope: 'Namespaced' },
  ];
  const mapContext = (change = {}) => ({ kinds: mapKinds, familyModels: models, auditModel: auditFixture, required: { nodes: ['o-job', 'o-lock'] }, ...change });
  const withNode = (id, change) => ({ ...mapFixture, nodes: mapFixture.nodes.map((node) => (node.id === id ? { ...node, ...change } : node)) });
  const withRelation = (relation) => ({ ...mapFixture, relations: [...mapFixture.relations, { text: 'T.', ...relation }] });
  if (objectMapProblems(mapFixture, mapContext()).length !== 0) {
    fail('objectMapProblems refused a map that holds', objectMapProblems(mapFixture, mapContext()));
  }
  // A cluster-scoped owner may own a namespaced object: the namespace rule
  // has to admit it, or it measures scope equality rather than the rule.
  const clusterOwner = {
    ...mapFixture,
    nodes: [...mapFixture.nodes, mapNode('o-cl', { kind: 'Cl', group: 'operator', scope: 'cluster' })],
    relations: [...mapFixture.relations, { from: 'o-cl', to: 'o-job', type: 'owns', text: 'C owns J.' }],
  };
  if (objectMapProblems(clusterOwner, mapContext()).length !== 0) {
    fail('objectMapProblems refused a cluster-scoped owner', objectMapProblems(clusterOwner, mapContext()));
  }
  for (const [name, spoiled, context, want] of [
    ['no object at all', { ...mapFixture, nodes: [] }, mapContext(), 'has no object'],
    [
      'a required object dropped',
      { ...mapFixture, nodes: mapFixture.nodes.filter((node) => node.id !== 'o-lock'), relations: mapFixture.relations.filter((one) => one.to !== 'o-lock') },
      mapContext(),
      'leaves out "o-lock"',
    ],
    ['an object id used twice', { ...mapFixture, nodes: [...mapFixture.nodes, mapFixture.nodes[0]] }, mapContext(), 'object "o-res" is used twice'],
    ['an object label used twice', withNode('o-job', { label: 'Label o-res' }), mapContext(), 'object label "Label o-res" is used twice'],
    ['an object without its prefix', withNode('o-db', { id: 'db' }), mapContext(), '"db" lacks the prefix o-'],
    ['an id a family uses', withNode('o-db', { id: 'b-one' }), mapContext(), 'shares its id with a family or the audit view'],
    ['an id the audit view uses', withNode('o-db', { id: 'a-one' }), mapContext(), 'shares its id with a family or the audit view'],
    ['a kind the CRDs define left out', mapFixture, mapContext({ kinds: [...mapKinds, { file: 'x.yaml', kind: 'Extra', scope: 'Namespaced' }] }), 'shows 0 objects of the kind Extra'],
    ['a kind shown twice', withNode('o-job', { kind: 'Res' }), mapContext(), 'shows 2 objects of the kind Res'],
    ['a cluster-scoped kind put in a namespace', withNode('o-realm', { scope: 'namespace' }), mapContext(), 'puts Realm at the scope "namespace", and its CRD says Cluster'],
    ['a namespaced kind made cluster-scoped', withNode('o-res', { scope: 'cluster' }), mapContext(), 'puts Res at the scope "cluster", and its CRD says Namespaced'],
    ['a CRD that could not be read', mapFixture, mapContext({ kinds: [...mapKinds, { file: 'x.yaml', kind: null, scope: null }] }), 'scope of x.yaml could not be read'],
    ['no CRD read at all', mapFixture, mapContext({ kinds: [] }), 'no kind was read'],
    ['an object that holds nothing', withNode('o-res', { holds: '' }), mapContext(), 'says nothing in holds'],
    ['an object nobody writes', withNode('o-res', { writtenBy: ' ' }), mapContext(), 'says nothing in writtenBy'],
    ['an object the operator does nothing with', withNode('o-res', { operator: '' }), mapContext(), 'says nothing in operator'],
    ['an object whose deletion is unsaid', withNode('o-res', { deletion: '' }), mapContext(), 'says nothing in deletion'],
    ['an object in no group', withNode('o-res', { group: 'nowhere' }), mapContext(), 'group "nowhere", which does not exist'],
    ['an object at no scope', withNode('o-job', { scope: 'galaxy' }), mapContext(), 'scope "galaxy", which does not exist'],
    ['an object outside the cluster in a namespace', withNode('o-db', { scope: 'namespace' }), mapContext(), 'only what is outside the cluster is both'],
    ['an object in the cluster grouped outside it', withNode('o-job', { group: 'outside' }), mapContext(), 'only what is outside the cluster is both'],
    ['an unknown change', withNode('o-res', { change: { verdict: 'frozen', text: 'T.' } }), mapContext(), 'changes as "frozen"'],
    ['a change left unsaid', withNode('o-res', { change: { verdict: 'editable', text: '' } }), mapContext(), 'does not say whether it can change'],
    ['an object of no family', withNode('o-res', { families: [] }), mapContext(), 'belongs to no family'],
    ['an object of an unknown family', withNode('o-res', { families: ['three'] }), mapContext(), 'family "three", which does not exist'],
    ['an object with no command', withNode('o-res', { inspect: [] }), mapContext(), 'says no way to inspect it'],
    ['a command that says nothing', withNode('o-res', { inspect: [{ command: 'c' }] }), mapContext(), 'without the command or what it shows'],
    ['an empty gap', withNode('o-res', { gap: '' }), mapContext(), 'names an empty gap'],
    ['an object with no link', withNode('o-res', { links: [] }), mapContext(), '"o-res" links nothing'],
    ['a group with no object', { ...mapFixture, groups: [...mapFixture.groups, { id: 'empty' }] }, mapContext(), 'group "empty" holds no object'],
    ['a relation of no type', withRelation({ from: 'o-res', to: 'o-db', type: 'likes' }), mapContext(), 'has a type the legend does not draw'],
    ['a relation to nowhere', withRelation({ from: 'o-res', to: 'o-zz', type: 'references' }), mapContext(), 'names "o-zz", which does not exist'],
    ['a relation to itself', withRelation({ from: 'o-res', to: 'o-res', type: 'references' }), mapContext(), 'relates it to itself'],
    ['a relation written twice', withRelation(mapFixture.relations[0]), mapContext(), 'is written twice'],
    ['a relation that says nothing', withRelation({ from: 'o-ok', to: 'o-res', type: 'references', text: '' }), mapContext(), 'does not say what it is'],
    ['an owner reference outside the cluster', withRelation({ from: 'o-res', to: 'o-db', type: 'owns' }), mapContext(), 'outside the cluster, which no garbage collector follows'],
    ['an owner reference into another namespace', withRelation({ from: 'o-res', to: 'o-lock', type: 'owns' }), mapContext(), 'leaves the namespace "namespace"'],
    ['a namespaced owner of a cluster-scoped object', withRelation({ from: 'o-job', to: 'o-realm', type: 'owns' }), mapContext(), 'leaves the namespace "namespace"'],
    ['a plan that owns the Job', withRelation({ from: 'o-plan', to: 'o-job', type: 'owns' }), mapContext(), 'makes a plan own a Job'],
    ['a resource that owns an approval', withRelation({ from: 'o-res', to: 'o-ok', type: 'owns' }), mapContext(), 'owns what a person writes'],
    [
      'an approval filed as the operator’s, owned',
      { ...withNode('o-ok', { group: 'operator' }), relations: [...mapFixture.relations, { from: 'o-res', to: 'o-ok', type: 'owns', text: 'T.' }] },
      mapContext(),
      'owns what a person writes',
    ],
    ['an owner of what a person writes', withRelation({ from: 'o-plan', to: 'o-res', type: 'owns' }), mapContext(), 'owns what a person writes'],
    ['a legend type no relation is', { ...mapFixture, types: [...mapFixture.types, { id: 'delivers' }] }, mapContext(), 'draws "delivers", which no relation is'],
    ['an object related to nothing', { ...mapFixture, nodes: [...mapFixture.nodes, mapNode('o-lone', { kind: 'Lone' })] }, mapContext(), '"o-lone" is related to nothing'],
    ['nothing said about the relationships', { ...mapFixture, distinctions: [] }, mapContext(), 'says nothing about what its relationships mean'],
    ['a map that links nothing', { ...mapFixture, links: [] }, mapContext(), 'the object map links nothing'],
  ]) {
    refuses(name, objectMapProblems(spoiled, context), want);
  }
  const plan = expectedNodeMarks('o-plan', mapFixture);
  if (
    plan.listed.join(' ') !== 'owns:o-res references:o-job references:o-ok references:o-res' ||
    JSON.stringify(plan.marks) !==
      JSON.stringify({ 'o-res': { type: 'owns', label: 'Owned by' }, 'o-job': { type: 'references', label: 'Named by' }, 'o-ok': { type: 'references', label: 'Named by' } })
  ) {
    fail('expectedNodeMarks read o-plan as', plan);
  }
  const reversed = expectedNodeMarks('o-plan', { ...mapFixture, relations: [mapFixture.relations[1], mapFixture.relations[0], ...mapFixture.relations.slice(2)] });
  if (reversed.marks['o-res']?.type !== 'owns') fail('expectedNodeMarks let a name hide an owner reference', reversed.marks);

  const changeText = (node) => `${node.change.verdict === 'immutable' ? 'Immutable' : 'Editable'}. ${node.change.text}`;
  const textOf = (node) =>
    [node.label, node.holds, node.writtenBy, node.operator, changeText(node), node.deletion, node.gap ?? '', ...node.inspect.map((step) => step.command)].join(' ');
  const mapText = {
    objects: {
      types: mapFixture.types.map((type) => type.id),
      nodes: mapFixture.nodes.map((node) => ({ id: node.id, text: textOf(node), relations: expectedNodeMarks(node.id, mapFixture).listed })),
    },
  };
  const textNode = (id, change) => ({
    objects: { ...mapText.objects, nodes: mapText.objects.nodes.map((node) => (node.id === id ? { ...node, ...change } : node)) },
  });
  const resNode = mapFixture.nodes[0];
  if (objectMapTextProblems(mapText, mapFixture).length !== 0) {
    fail('objectMapTextProblems refused a text that holds', objectMapTextProblems(mapText, mapFixture));
  }
  for (const [name, spoiled, want] of [
    ['no object map text', { objects: null }, 'leaves out the object map'],
    ['a relationship type dropped', { objects: { ...mapText.objects, types: ['owns', 'references', 'holds'] } }, 'relationship type "executes"'],
    ['an object dropped', { objects: { ...mapText.objects, nodes: mapText.objects.nodes.slice(0, -1) } }, 'leaves out "o-db"'],
    ['an object without what it holds', textNode('o-res', { text: textOf({ ...resNode, holds: '' }) }), '"o-res" lacks its holds'],
    ['an object without its writer', textNode('o-res', { text: textOf({ ...resNode, writtenBy: '' }) }), '"o-res" lacks its writtenBy'],
    ['an object without its deletion', textNode('o-res', { text: textOf({ ...resNode, deletion: '' }) }), '"o-res" lacks its deletion'],
    ['a change without its verdict', textNode('o-res', { text: textOf(resNode).replace('Editable. ', '') }), '"o-res" lacks its change'],
    ['an object without its gap', textNode('o-db', { text: textOf({ ...mapFixture.nodes.at(-1), gap: '' }) }), '"o-db" lacks its gap'],
    ['an object without its command', textNode('o-res', { text: textOf({ ...resNode, inspect: [] }) }), 'lacks its command "kubectl ptah schema o-res"'],
    ['a relationship dropped from the text', textNode('o-plan', { relations: ['owns:o-res', 'references:o-job'] }), '"o-plan" leaves out references:o-ok'],
    ['a relationship the model does not have', textNode('o-db', { relations: ['executes:o-job', 'owns:o-res'] }), '"o-db" also lists owns:o-res'],
  ]) {
    refuses(name, objectMapTextProblems(spoiled, mapFixture), want);
  }

  const mapRender = {
    viewButtons: VIEWS.length,
    shown: true,
    journeyShown: false,
    auditShown: false,
    nodeButtons: 7,
    panels: 7,
    legend: ['owns', 'references', 'holds', 'executes'],
    visiblePanels: 0,
    marked: 0,
    hintShown: true,
  };
  if (objectMapRenderProblems(mapRender, mapFixture).length !== 0) {
    fail('objectMapRenderProblems refused a view that holds', objectMapRenderProblems(mapRender, mapFixture));
  }
  for (const [name, change, want] of [
    ['a view button missing', { viewButtons: VIEWS.length - 1 }, `${VIEWS.length - 1} view buttons, not ${VIEWS.length}`],
    ['an object map that stays hidden', { shown: false }, 'does not show it'],
    ['a journey left on screen', { journeyShown: true }, 'leaves the change journey on screen'],
    ['an audit view left on screen', { auditShown: true }, 'leaves the audit view on screen'],
    ['an object button missing', { nodeButtons: 6 }, '6 object buttons'],
    ['a panel too many', { panels: 8 }, '8 panels'],
    ['a legend out of order', { legend: ['references', 'owns', 'holds', 'executes'] }, 'legend draws references, owns'],
    ['a legend type missing', { legend: ['owns', 'references', 'holds'] }, 'legend draws owns, references, holds,'],
    ['a panel open at the start', { visiblePanels: 1 }, 'opens with 1 panels shown'],
    ['objects marked at the start', { marked: 2 }, 'opens with 2 objects marked'],
    ['no hint at the start', { hintShown: false }, 'without saying what selecting an object does'],
  ]) {
    refuses(name, objectMapRenderProblems({ ...mapRender, ...change }, mapFixture), want);
  }

  const nodeReading = (id) => {
    const want = expectedNodeMarks(id, mapFixture);
    return {
      id,
      panels: [id],
      pressed: [id],
      listed: want.listed,
      marks: Object.fromEntries(Object.entries(want.marks).map(([other, mark]) => [other, { type: mark.type, badge: mark.label }])),
    };
  };
  const mapSelection = { nodes: mapFixture.nodes.map((node) => nodeReading(node.id)), follow: { from: 'o-res', want: 'o-plan', got: 'o-plan' } };
  const withReading = (id, change) => ({
    ...mapSelection,
    nodes: mapSelection.nodes.map((reading) => (reading.id === id ? { ...reading, ...change } : reading)),
  });
  const planMarks = nodeReading('o-plan').marks;
  if (objectMapSelectionProblems(mapSelection, mapFixture).length !== 0) {
    fail('objectMapSelectionProblems refused readings that hold', objectMapSelectionProblems(mapSelection, mapFixture));
  }
  for (const [name, spoiled, want] of [
    ['an object never selected', { ...mapSelection, nodes: mapSelection.nodes.slice(1) }, '6 of the object map’s 7 objects were selected'],
    ['another object’s panel', withReading('o-res', { panels: ['o-job'] }), 'selecting "o-res" shows the panels o-job'],
    ['two panels at once', withReading('o-res', { panels: ['o-res', 'o-job'] }), 'shows the panels o-res, o-job'],
    ['two objects pressed', withReading('o-res', { pressed: ['o-res', 'o-job'] }), 'selecting "o-res" presses o-res, o-job'],
    ['a relationship left out of the panel', withReading('o-plan', { listed: ['owns:o-res', 'references:o-job', 'references:o-ok'] }), 'panel of "o-plan" leaves out references:o-res'],
    ['a relationship the model does not have', withReading('o-db', { listed: ['executes:o-job', 'owns:o-res'] }), 'panel of "o-db" also lists owns:o-res'],
    ['a related object left unmarked', withReading('o-plan', { marks: { 'o-job': planMarks['o-job'], 'o-ok': planMarks['o-ok'] } }), 'leaves "o-res" unmarked, which it owned by'],
    [
      'an owner shown as a name',
      withReading('o-plan', { marks: { ...planMarks, 'o-res': { type: 'references', badge: 'Named by' } } }),
      'marks "o-res" references "Named by", not owns "Owned by"',
    ],
    ['a relationship in style alone', withReading('o-plan', { marks: { ...planMarks, 'o-res': { type: 'owns', badge: '' } } }), 'marks "o-res" owns "", not owns "Owned by"'],
    ['a relationship read from the wrong end', withReading('o-plan', { marks: { ...planMarks, 'o-res': { type: 'owns', badge: 'Owns' } } }), 'not owns "Owned by"'],
    ['an unrelated object marked', withReading('o-plan', { marks: { ...planMarks, 'o-db': { type: 'references', badge: 'Names' } } }), 'marks "o-db", which the model does not relate to it'],
    ['a related object that opens nothing', { ...mapSelection, follow: { from: 'o-res', want: 'o-plan', got: null } }, 'opens nothing'],
    ['no related object followed', { ...mapSelection, follow: null }, 'no related object was followed'],
  ]) {
    refuses(name, objectMapSelectionProblems(spoiled, mapFixture), want);
  }

  const mapLinks = [
    { link: '?lx-view=objects&lx-node=o-job', want: { view: 'objects', node: 'o-job' }, got: { view: 'objects', node: 'o-job' } },
    { link: '?lx-node=a-one', want: { view: 'journey', node: null }, got: { view: 'journey', node: null } },
  ];
  if (objectMapLinkProblems(mapLinks).length !== 0) fail('objectMapLinkProblems refused links that hold', objectMapLinkProblems(mapLinks));
  refuses('a link that opens the journey', objectMapLinkProblems([{ ...mapLinks[0], got: { view: 'journey', node: null } }]), 'opens the journey view, not objects');
  refuses('a link whose object was dropped', objectMapLinkProblems([{ ...mapLinks[0], got: { view: 'objects', node: null } }]), 'opens no object, not o-job');
  refuses('a link that opens an object nobody named', objectMapLinkProblems([{ ...mapLinks[1], got: { view: 'journey', node: 'o-job' } }]), 'opens o-job, not no object');
  refuses('no link into the object map', objectMapLinkProblems([]), 'no link into the object map');

  const mapKeys = { chosen: true, node: true, follow: true };
  if (objectMapKeyboardProblems(mapKeys).length !== 0) fail('objectMapKeyboardProblems refused keys that hold', objectMapKeyboardProblems(mapKeys));
  refuses('a map the keyboard cannot choose', objectMapKeyboardProblems({ ...mapKeys, chosen: false }), 'do not choose the object map');
  refuses('an object the keyboard cannot open', objectMapKeyboardProblems({ ...mapKeys, node: false }), 'open an object of the map');
  refuses('a related object the keyboard cannot follow', objectMapKeyboardProblems({ ...mapKeys, follow: false }), 'follow a related object');

  // The composed workflow: a fixture of three stages, one per family that
  // runs one, held to a requirement of its own size, then each way the model,
  // the text, the view, the walk, the links and the keyboard can fall short.
  const flowStage = (id, family, stops, extra = {}) => ({
    id,
    title: `Title ${id}`,
    family,
    goal: `Goal ${id}.`,
    person: `Person ${id}.`,
    inspect: [{ command: `kubectl ptah schema ${id}`, says: 'Says.' }],
    operator: `Operator ${id}.`,
    gate: `Gate ${id}.`,
    done: `Done ${id}.`,
    outcomes: stops.map((outcome) => ({ outcome, text: `Here ${id} ${outcome}.`, recovery: `Clears ${id} ${outcome}.` })),
    objects: ['o-res'],
    links: [{ label: 'L', href: '#l' }],
    ...extra,
  });
  const flowDecision = (id, node) => ({ id, label: `Label ${id}`, ...(node ? { node } : {}), covers: `Covers ${id}.`, notCovered: `Not ${id}.` });
  const flowFixture = {
    families: [
      { id: 'schema', label: 'S' },
      { id: 'migration', label: 'M' },
      { id: 'outside', label: 'X' },
    ],
    marks: [
      { id: 'shipped', label: 'Shipped', text: 'Ships.' },
      { id: 'proposed', label: 'Proposed', text: 'Proposes.' },
    ],
    proposal: { label: 'Proposed orchestration', text: 'There is no Workflow or WorkflowRun resource.' },
    preconditions: [{ id: 'w-pre', title: 'Pre', text: 'Pre holds.', objects: ['o-realm'], links: [{ label: 'L', href: '#l' }] }],
    outcomes: ['w-refused', 'w-failed', 'w-unresolved', 'w-check-failed'].map((id) => ({ id, label: `Label ${id}`, text: `Text ${id}.` })),
    decisions: [flowDecision('w-dec-schema', 'o-ok'), flowDecision('w-dec-migration'), flowDecision('w-dec-check'), flowDecision('w-dec-policy')],
    stages: [
      flowStage('w-a', 'schema', ['w-refused', 'w-unresolved'], { decision: 'w-dec-schema', decisionText: 'Decided a.' }),
      flowStage('w-b', 'migration', ['w-failed', 'w-unresolved'], { decision: 'w-dec-migration', decisionText: 'Decided b.', limits: 'Limits b.' }),
      flowStage('w-c', 'outside', ['w-check-failed'], {
        decision: 'w-dec-check',
        decisionText: 'Decided c.',
        gate: 'Migration InSync does not satisfy it.',
        inspect: [{ system: 'A client.', says: 'Rows.' }],
      }),
    ],
    record: [{ id: 'w-rec-a', label: 'A', text: 'Field a.' }],
    recordText: 'Kept by a pipeline.',
    readers: [{ command: 'kubectl ptah history schema/a', says: 'Reads.' }],
    distinctions: ['Apart.'],
    links: [{ label: 'L', href: '#l' }],
  };
  flowFixture.stages[0].outcomes[0].journey = { family: 'one', path: 'side' };
  const flowRequired = {
    stages: [
      ['w-a', 'schema'],
      ['w-b', 'migration'],
      ['w-c', 'outside'],
    ],
    preconditions: ['w-pre'],
    outcomes: { 'w-refused': ['schema', 'migration'], 'w-failed': ['migration'], 'w-unresolved': ['schema', 'migration'], 'w-check-failed': ['outside'] },
    decided: ['w-a', 'w-b', 'w-c'],
    decisions: { schema: 'w-dec-schema', migration: 'w-dec-migration', outside: 'w-dec-check' },
    policy: 'w-dec-policy',
    record: ['w-rec-a'],
    proposedKinds: ['Workflow', 'WorkflowRun'],
  };
  const flowContext = (change = {}) => ({ familyModels: models, auditModel: auditFixture, map: mapFixture, kinds: mapKinds, required: flowRequired, ...change });
  const withFlow = (change) => ({ ...flowFixture, ...change });
  const withStage = (id, change) => ({ ...flowFixture, stages: flowFixture.stages.map((one) => (one.id === id ? { ...one, ...change } : one)) });
  const stopsOf = (id, stops) => flowStage(id, 'x', stops).outcomes;
  if (workflowProblems(flowFixture, flowContext()).length !== 0) {
    fail('workflowProblems refused a workflow that holds', workflowProblems(flowFixture, flowContext()));
  }
  for (const [name, spoiled, context, want] of [
    ['no stage at all', withFlow({ stages: [] }), flowContext(), 'has no stage'],
    ['stages out of order', withFlow({ stages: [flowFixture.stages[1], flowFixture.stages[0], flowFixture.stages[2]] }), flowContext(), 'stages are w-b w-a w-c, not w-a w-b w-c'],
    ['a stage dropped', withFlow({ stages: flowFixture.stages.slice(0, 2) }), flowContext(), 'stages are w-a w-b, not w-a w-b w-c'],
    ['a stage in another family', withStage('w-a', { family: 'migration' }), flowContext(), '"w-a" runs in "migration", not "schema"'],
    ['an id used twice', withFlow({ record: [...flowFixture.record, { id: 'w-a', label: 'A', text: 'T.' }] }), flowContext(), '"w-a" is used twice'],
    ['an id without its prefix', withFlow({ record: [{ id: 'rec-a', label: 'A', text: 'T.' }] }), flowContext(), '"rec-a" lacks the prefix w-'],
    ['an id another view uses', withFlow({ record: [...flowFixture.record, { id: 'o-res', label: 'A', text: 'T.' }] }), flowContext(), '"o-res" shares its id with another view'],
    ['no proposal', withFlow({ proposal: { label: '', text: '' } }), flowContext(), 'does not say it is proposed orchestration'],
    ['a proposal that leaves a kind unnamed', withFlow({ proposal: { label: 'P', text: 'There is no Workflow resource.' } }), flowContext(), 'does not say there is no WorkflowRun'],
    ['a proposed kind the CRDs define', flowFixture, flowContext({ kinds: [...mapKinds, { file: 'w.yaml', kind: 'WorkflowRun', scope: 'Namespaced' }] }), 'the CRDs define WorkflowRun'],
    ['no proposed mark', withFlow({ marks: flowFixture.marks.slice(0, 1) }), flowContext(), 'marks shipped, not shipped and proposed'],
    ['a mark that says nothing', withFlow({ marks: [flowFixture.marks[0], { id: 'proposed', label: 'P', text: '' }] }), flowContext(), 'the mark "proposed" says nothing'],
    ['a precondition dropped', withFlow({ preconditions: [] }), flowContext(), 'leaves out the precondition "w-pre"'],
    ['a precondition on an unknown object', withFlow({ preconditions: [{ ...flowFixture.preconditions[0], objects: ['o-zz'] }] }), flowContext(), 'names "o-zz", which the object map does not have'],
    ['a precondition that links nothing', withFlow({ preconditions: [{ ...flowFixture.preconditions[0], links: [] }] }), flowContext(), 'precondition "w-pre" links nothing'],
    ['a stop left out', withFlow({ outcomes: flowFixture.outcomes.filter((one) => one.id !== 'w-failed') }), flowContext(), 'leaves out the stop "w-failed"'],
    ['a stop that says nothing', withFlow({ outcomes: flowFixture.outcomes.map((one) => (one.id === 'w-failed' ? { ...one, text: '' } : one)) }), flowContext(), 'the stop "w-failed" says nothing'],
    ['a stop nobody placed', withFlow({ outcomes: [...flowFixture.outcomes, { id: 'w-new', label: 'N', text: 'T.' }] }), flowContext(), '"w-new" is one this check does not know'],
    ['a stop that stops no stage', withStage('w-b', { outcomes: stopsOf('w-b', ['w-unresolved']) }), flowContext(), 'the stop "w-failed" stops no stage'],
    ['a decision left out', withFlow({ decisions: flowFixture.decisions.slice(0, 3) }), flowContext(), 'leaves out the decision "w-dec-policy"'],
    ['a decision that hides what it leaves', withFlow({ decisions: [{ ...flowFixture.decisions[0], notCovered: '' }, ...flowFixture.decisions.slice(1)] }), flowContext(), 'does not say what it covers and what it does not'],
    ['a decision on an unknown object', withFlow({ decisions: [{ ...flowFixture.decisions[0], node: 'o-zz' }, ...flowFixture.decisions.slice(1)] }), flowContext(), 'decision "w-dec-schema" names "o-zz"'],
    ['a stage that says nothing of its gate', withStage('w-a', { gate: ' ' }), flowContext(), '"w-a" says nothing in gate'],
    ['a stage that says nothing of the operator', withStage('w-b', { operator: '' }), flowContext(), '"w-b" says nothing in operator'],
    ['a stage in no family', withStage('w-b', { family: 'elsewhere' }), flowContext(), 'runs in "elsewhere", which does not exist'],
    ['a stage with no step', withStage('w-a', { inspect: [] }), flowContext(), 'says no way to act on it or inspect it'],
    ['a step that is a command and a system', withStage('w-a', { inspect: [{ command: 'kubectl ptah plan a', system: 'S.', says: 'Says.' }] }), flowContext(), 'not one command or one system'],
    ['a step that says nothing', withStage('w-c', { inspect: [{ system: 'S.' }] }), flowContext(), 'not one command or one system'],
    ['a decided stage with no decision', withStage('w-b', { decision: undefined }), flowContext(), '"w-b" names no decision'],
    ['a stage authorized by the other family', withStage('w-b', { decision: 'w-dec-schema' }), flowContext(), 'names the decision "w-dec-schema", not "w-dec-migration"'],
    ['a decision without its text', withStage('w-a', { decisionText: '' }), flowContext(), '"w-a" does not say what its decision is'],
    ['a stage that cannot stop', withStage('w-c', { outcomes: [] }), flowContext(), '"w-c" can stop on nothing'],
    ['a stop listed twice', withStage('w-b', { outcomes: stopsOf('w-b', ['w-failed', 'w-unresolved', 'w-failed']) }), flowContext(), '"w-b" lists a stop twice'],
    ['a resource stage that cannot end unknown', withStage('w-a', { outcomes: stopsOf('w-a', ['w-refused']) }), flowContext(), 'cannot end with an outcome nobody knows'],
    ['a schema stage that fails like a migration', withStage('w-a', { outcomes: stopsOf('w-a', ['w-failed', 'w-unresolved']) }), flowContext(), '"w-failed", which cannot happen in "schema"'],
    ['a check stopping a resource stage', withStage('w-b', { outcomes: stopsOf('w-b', ['w-check-failed', 'w-failed', 'w-unresolved']) }), flowContext(), '"w-check-failed", which cannot happen in "migration"'],
    ['a stop of no kind', withStage('w-b', { outcomes: stopsOf('w-b', ['w-gone', 'w-failed', 'w-unresolved']) }), flowContext(), '"w-gone", which does not exist'],
    ['a stop with nothing to clear it', withStage('w-b', { outcomes: [{ ...stopsOf('w-b', ['w-failed'])[0], recovery: '' }, stopsOf('w-b', ['w-unresolved'])[0]] }), flowContext(), 'does not say what happened and what clears it'],
    ['a journey path that does not exist', withStage('w-a', { outcomes: [{ ...flowFixture.stages[0].outcomes[0], journey: { family: 'one', path: 'nowhere' } }, flowFixture.stages[0].outcomes[1]] }), flowContext(), '"one/nowhere", which does not exist'],
    ['a journey path of another family', withStage('w-a', { outcomes: [{ ...flowFixture.stages[0].outcomes[0], journey: { family: 'two', path: 'side' } }, flowFixture.stages[0].outcomes[1]] }), flowContext(), '"two/side", which does not exist'],
    ['a stop walked on the successful path', withStage('w-a', { outcomes: [{ ...flowFixture.stages[0].outcomes[0], journey: { family: 'one', path: 'main' } }, flowFixture.stages[0].outcomes[1]] }), flowContext(), 'names the successful path "main"'],
    ['a stage on an unknown object', withStage('w-a', { objects: ['o-zz'] }), flowContext(), '"w-a" names "o-zz"'],
    ['a stage that links nothing', withStage('w-a', { links: [] }), flowContext(), '"w-a" links nothing'],
    ['a check stage InSync could pass', withStage('w-c', { gate: 'The contract waits for the checks.' }), flowContext(), 'does not say that Migration InSync does not satisfy it'],
    ['a record field left out', withFlow({ record: [] }), flowContext(), 'stage record leaves out "w-rec-a"'],
    ['a record field that says nothing', withFlow({ record: [{ id: 'w-rec-a', label: 'A', text: '' }] }), flowContext(), 'record’s "w-rec-a" says nothing'],
    ['a record nobody keeps', withFlow({ recordText: '' }), flowContext(), 'does not say who keeps the stage record'],
    ['a record nothing reads', withFlow({ readers: [] }), flowContext(), 'says no way to read the stage record'],
    ['a reader without its command', withFlow({ readers: [{ says: 'S.' }] }), flowContext(), 'lacks its command or what it shows'],
    ['nothing kept apart', withFlow({ distinctions: [] }), flowContext(), 'says nothing about what it keeps apart'],
    ['a workflow that links nothing', withFlow({ links: [] }), flowContext(), 'the composed workflow links nothing'],
  ]) {
    refuses(name, workflowProblems(spoiled, context), want);
  }
  // The requirement this check holds the explorer to is #817's sequence.
  if (WORKFLOW_REQUIRED.stages.map(([id]) => id).join(' ') !== 'w-expand w-verify-expand w-backfill w-history w-checks w-contract w-verify-contract') {
    fail('WORKFLOW_REQUIRED names the stages', WORKFLOW_REQUIRED.stages);
  }

  const flowStageText = (stage) =>
    [
      stage.title,
      stage.goal,
      stage.person,
      stage.decisionText ?? '',
      stage.done,
      stage.limits ?? '',
      ...stage.inspect.map((step) => step.command ?? step.system),
      ...stage.outcomes.flatMap((one) => [one.text, one.recovery]),
    ].join(' ');
  const flowText = {
    workflow: {
      proposal: `Proposed orchestration. ${flowFixture.proposal.text}`,
      marks: ['shipped', 'proposed'],
      preconditions: flowFixture.preconditions.map((one) => ({ id: one.id, text: `${one.title}. ${one.text}` })),
      stages: flowFixture.stages.map((stage) => ({
        id: stage.id,
        text: flowStageText(stage),
        operator: `The operator, shipped: ${stage.operator}`,
        gate: `Moves on when, proposed: ${stage.gate}`,
        stops: stage.outcomes.map((one) => one.outcome),
      })),
      outcomes: flowFixture.outcomes.map((one) => one.id),
      decisions: flowFixture.decisions.map((one) => ({ id: one.id, text: `${one.covers} ${one.notCovered}` })),
      record: flowFixture.record.map((one) => one.id),
      text: flowFixture.readers.map((one) => one.command).join(' '),
    },
  };
  const textStage = (id, change) => ({
    workflow: { ...flowText.workflow, stages: flowText.workflow.stages.map((one) => (one.id === id ? { ...one, ...change } : one)) },
  });
  const textFlow = (change) => ({ workflow: { ...flowText.workflow, ...change } });
  const stageA = flowFixture.stages[0];
  if (workflowTextProblems(flowText, flowFixture).length !== 0) {
    fail('workflowTextProblems refused a text that holds', workflowTextProblems(flowText, flowFixture));
  }
  for (const [name, spoiled, want] of [
    ['no workflow text', { workflow: null }, 'leaves out the composed workflow'],
    ['a text that does not say it is proposed', textFlow({ proposal: 'Orchestration.' }), 'does not say it is proposed'],
    ['a mark dropped', textFlow({ marks: ['shipped'] }), 'leaves out the mark "proposed"'],
    ['a precondition dropped', textFlow({ preconditions: [] }), 'leaves out the precondition "w-pre"'],
    ['a precondition without its text', textFlow({ preconditions: [{ id: 'w-pre', text: 'Pre.' }] }), 'precondition "w-pre" lacks its text'],
    ['a stage dropped', textFlow({ stages: flowText.workflow.stages.slice(1) }), 'stages read w-b w-c, not w-a w-b w-c'],
    ['stages out of order', textFlow({ stages: [...flowText.workflow.stages].reverse() }), 'stages read w-c w-b w-a'],
    ['a stage without its goal', textStage('w-a', { text: flowStageText({ ...stageA, goal: '' }) }), '"w-a" lacks its goal'],
    ['a stage without its command', textStage('w-a', { text: flowStageText({ ...stageA, inspect: [] }) }), 'lacks its step "kubectl ptah schema w-a"'],
    ['a stage without its decision', textStage('w-a', { text: flowStageText({ ...stageA, decisionText: '' }) }), '"w-a" lacks its decision'],
    ['a stage without its limits', textStage('w-b', { text: flowStageText({ ...flowFixture.stages[1], limits: '' }) }), '"w-b" lacks its limits'],
    ['the operator not marked shipped', textStage('w-a', { operator: `The operator: ${stageA.operator}` }), 'does not give what the operator does as shipped'],
    ['the gate not marked proposed', textStage('w-a', { gate: `Moves on when: ${stageA.gate}` }), 'does not give what moves it on as proposed'],
    ['the gate left out', textStage('w-a', { gate: 'Moves on when, proposed.' }), 'does not give what moves it on as proposed'],
    ['a stop dropped from a stage', textStage('w-a', { stops: ['w-unresolved'] }), '"w-a" stops on w-unresolved, not w-refused, w-unresolved'],
    ['a stop without what clears it', textStage('w-a', { text: flowStageText({ ...stageA, outcomes: [{ ...stageA.outcomes[0], recovery: 'x' }, stageA.outcomes[1]] }) }), 'lacks what "w-refused" holds or what clears it'],
    ['a stop dropped from the list', textFlow({ outcomes: ['w-refused'] }), 'leaves out the stop "w-failed"'],
    ['a decision without what it leaves', textFlow({ decisions: flowText.workflow.decisions.map((one) => ({ ...one, text: one.text.split(' Not')[0] })) }), 'does not say what it covers and what it does not'],
    ['a record field dropped', textFlow({ record: [] }), 'stage record leaves out "w-rec-a"'],
    ['a reader dropped', textFlow({ text: '' }), 'lacks the command "kubectl ptah history schema/a"'],
  ]) {
    refuses(name, workflowTextProblems(spoiled, flowFixture), want);
  }

  const flowRender = {
    viewButtons: VIEWS.length,
    shown: true,
    othersShown: [],
    steps: 3,
    panels: 3,
    visiblePanels: ['w-a'],
    stopsShown: 0,
    statusShown: false,
    proposalShown: true,
    shipped: 3,
    proposed: 3,
    tags: ['S', 'M', 'X'],
  };
  if (workflowRenderProblems(flowRender, flowFixture).length !== 0) {
    fail('workflowRenderProblems refused a view that holds', workflowRenderProblems(flowRender, flowFixture));
  }
  for (const [name, change, want] of [
    ['a view button missing', { viewButtons: VIEWS.length - 1 }, `view buttons, not ${VIEWS.length}`],
    ['a workflow that stays hidden', { shown: false }, 'does not show it'],
    ['another view left on screen', { othersShown: ['objects'] }, 'leaves the objects view on screen'],
    ['a step missing', { steps: 2 }, '2 steps and the model 3 stages'],
    ['a panel too many', { panels: 4 }, '4 panels and the model 3 stages'],
    ['the second stage open first', { visiblePanels: ['w-b'] }, 'opens with the panels w-b, not w-a'],
    ['two panels open', { visiblePanels: ['w-a', 'w-b'] }, 'opens with the panels w-a, w-b'],
    ['a stop chosen at the start', { stopsShown: 1 }, 'opens with a stop chosen'],
    ['a status line at the start', { statusShown: true }, 'opens with a stop chosen'],
    ['no proposal on screen', { proposalShown: false }, 'does not say on screen that it is proposed'],
    ['an operator part not marked shipped', { shipped: 2 }, '2 of 3 stages give what the operator does as shipped'],
    ['a gate not marked proposed', { proposed: 2 }, '2 of 3 stages give what moves them on as proposed'],
    ['a stage tagged with another family', { tags: ['S', 'S', 'X'] }, 'tagged S, S, X, not S, M, X'],
  ]) {
    refuses(name, workflowRenderProblems({ ...flowRender, ...change }, flowFixture), want);
  }

  const marksAt = (index, outcome) => expectedStepMarks(flowFixture, index, outcome);
  if (JSON.stringify(marksAt(1, 'w-failed')) !== JSON.stringify([['done', 'Done'], ['stopped', 'Stopped: Label w-failed'], ['held', 'Held']])) {
    fail('expectedStepMarks read a stop at w-b as', marksAt(1, 'w-failed'));
  }
  if (expectedWorkflowSearch(flowFixture, 0, null) !== '?lx-view=workflow' || expectedWorkflowSearch(flowFixture, 1, 'w-failed') !== '?lx-view=workflow&lx-step=w-b&lx-outcome=w-failed') {
    fail('expectedWorkflowSearch read the address as', [expectedWorkflowSearch(flowFixture, 0, null), expectedWorkflowSearch(flowFixture, 1, 'w-failed')]);
  }
  const stepReading = (stage, index) => ({
    id: stage.id,
    current: stage.id,
    panels: [stage.id],
    count: `Stage ${index + 1} of 3: ${stage.title}`,
    marks: marksAt(index, null),
    status: null,
    search: expectedWorkflowSearch(flowFixture, index, null),
  });
  const stopReading = (stage, index, outcome) => ({
    stage: stage.id,
    outcome,
    pressed: [outcome],
    details: [outcome],
    marks: marksAt(index, outcome),
    status: `Stopped at ${stage.title}: Label ${outcome}. Held: ${flowFixture.stages
      .slice(index + 1)
      .map((one) => one.title)
      .join(', ')}.`,
    search: expectedWorkflowSearch(flowFixture, index, outcome),
  });
  const cleared = (how, want) => ({ how, want, got: { step: want, outcome: null, held: 0, search: '?lx-view=workflow&lx-step=w-b' } });
  const flowWalk = {
    steps: flowFixture.stages.map(stepReading),
    stops: flowFixture.stages.flatMap((stage, index) => stage.outcomes.map((one) => stopReading(stage, index, one.outcome))),
    clears: [cleared('Next', 'w-b'), cleared('choosing it again', 'w-b')],
  };
  const withStep = (id, change) => ({ ...flowWalk, steps: flowWalk.steps.map((one) => (one.id === id ? { ...one, ...change } : one)) });
  const withStop = (stage, outcome, change) => ({
    ...flowWalk,
    stops: flowWalk.stops.map((one) => (one.stage === stage && one.outcome === outcome ? { ...one, ...change } : one)),
  });
  const withClear = (change) => ({ ...flowWalk, clears: [{ ...flowWalk.clears[0], got: { ...flowWalk.clears[0].got, ...change } }] });
  if (workflowWalkProblems(flowWalk, flowFixture).length !== 0) {
    fail('workflowWalkProblems refused a walk that holds', workflowWalkProblems(flowWalk, flowFixture));
  }
  for (const [name, spoiled, want] of [
    ['a stage never chosen', { ...flowWalk, steps: flowWalk.steps.slice(1) }, '2 of the composed workflow’s 3 stages were chosen'],
    ['another stage made current', withStep('w-b', { current: 'w-a' }), 'choosing the stage "w-b" makes "w-a" current'],
    ['another stage’s panel', withStep('w-b', { panels: ['w-c'] }), '"w-b" shows the panels w-c'],
    ['a count of another stage', withStep('w-b', { count: 'Stage 1 of 3: Title w-a' }), 'counts "Stage 1 of 3'],
    ['an earlier stage not marked done', withStep('w-c', { marks: [['done', 'Done'], ['ahead', ''], ['current', '']] }), 'marks the steps done "Done", ahead, current'],
    ['a done stage marked in style alone', withStep('w-b', { marks: [['done', ''], ['current', ''], ['ahead', '']] }), 'marks the steps done, current, ahead'],
    ['a status with nothing stopped', withStep('w-a', { status: 'Stopped.' }), 'says it stopped'],
    ['the first stage written to the address', withStep('w-a', { search: '?lx-view=workflow&lx-step=w-a' }), 'not ?lx-view=workflow'],
    ['a stage not written to the address', withStep('w-b', { search: '?lx-view=workflow' }), 'not ?lx-view=workflow&lx-step=w-b'],
    ['a stop never chosen', { ...flowWalk, stops: flowWalk.stops.slice(1) }, '4 of the composed workflow’s 5 stops were chosen'],
    ['another stop pressed', withStop('w-a', 'w-refused', { pressed: ['w-unresolved'] }), 'presses w-unresolved'],
    ['two details at once', withStop('w-a', 'w-refused', { details: ['w-refused', 'w-unresolved'] }), 'shows the detail of w-refused, w-unresolved'],
    ['a later stage not held', withStop('w-a', 'w-refused', { marks: [['stopped', 'Stopped: Label w-refused'], ['ahead', ''], ['held', 'Held']] }), 'marks the steps stopped "Stopped: Label w-refused", ahead, held'],
    ['a held stage in style alone', withStop('w-a', 'w-refused', { marks: [['stopped', 'Stopped: Label w-refused'], ['held', ''], ['held', 'Held']] }), 'held, held "Held"'],
    ['a stop that does not mark its stage', withStop('w-b', 'w-failed', { marks: [['done', 'Done'], ['current', ''], ['held', 'Held']] }), 'done "Done", current, held "Held"'],
    ['a status that names no held stage', withStop('w-a', 'w-refused', { status: 'Stopped at Title w-a: Label w-refused.' }), 'does not name the stop and every stage it holds'],
    ['no status for a stop', withStop('w-a', 'w-refused', { status: null }), 'does not name the stop'],
    ['a stop not written to the address', withStop('w-b', 'w-failed', { search: '?lx-view=workflow&lx-step=w-b' }), 'not ?lx-view=workflow&lx-step=w-b&lx-outcome=w-failed'],
    ['no stop cleared', { ...flowWalk, clears: [] }, 'no stop was cleared'],
    ['a stop that moves with the stage', withClear({ outcome: 'w-failed' }), 'clearing a stop by Next keeps the stop "w-failed"'],
    ['held stages left behind', withClear({ held: 1 }), 'leaves 1 stages held'],
    ['a stop left in the address', withClear({ search: '?lx-view=workflow&lx-step=w-b&lx-outcome=w-failed' }), 'with lx-outcome'],
    ['a clear that lands elsewhere', withClear({ step: 'w-c' }), 'opens "w-c", not "w-b"'],
  ]) {
    refuses(name, workflowWalkProblems(spoiled, flowFixture), want);
  }

  const flowLinks = [
    {
      link: '?lx-view=workflow&lx-step=w-b&lx-outcome=w-failed',
      want: { view: 'workflow', step: 'w-b', outcome: 'w-failed' },
      got: { view: 'workflow', step: 'w-b', outcome: 'w-failed', node: null, held: 1, status: 'Stopped at Title w-b: Label w-failed. Held: Title w-c.' },
    },
    {
      link: '?lx-view=workflow&lx-step=w-c&lx-outcome=w-failed',
      want: { view: 'workflow', step: 'w-c', outcome: null },
      got: { view: 'workflow', step: 'w-c', outcome: null, node: null, held: 0, status: null },
    },
    { link: '?lx-view=objects&lx-step=w-b', want: { view: 'objects', outcome: null, node: null }, got: { view: 'objects', step: 'w-a', outcome: null, node: null, held: 2, status: null } },
  ];
  if (workflowLinkProblems(flowLinks).length !== 0) fail('workflowLinkProblems refused links that hold', workflowLinkProblems(flowLinks));
  refuses('a link that opens the journey', workflowLinkProblems([{ ...flowLinks[0], got: { ...flowLinks[0].got, view: 'journey' } }]), 'opens the journey view, not workflow');
  refuses('a link whose stage was dropped', workflowLinkProblems([{ ...flowLinks[0], got: { ...flowLinks[0].got, step: 'w-a' } }]), 'opens the stage w-a, not w-b');
  refuses('a link whose stop was dropped', workflowLinkProblems([{ ...flowLinks[0], got: { ...flowLinks[0].got, outcome: null } }]), 'stops on nothing, not w-failed');
  refuses('a stop the stage cannot have, taken', workflowLinkProblems([{ ...flowLinks[1], got: { ...flowLinks[1].got, outcome: 'w-failed' } }]), 'stops on w-failed, not nothing');
  refuses(
    'a stop the stage cannot have, shown on its steps',
    workflowLinkProblems([{ ...flowLinks[1], got: { ...flowLinks[1].got, held: 0, status: 'Stopped at Title w-c: Label w-failed. No stage follows it.' } }]),
    'shows a stop nobody chose: 0 stages held',
  );
  refuses('a stop the stage cannot have, holding the stages after it', workflowLinkProblems([{ ...flowLinks[1], got: { ...flowLinks[1].got, held: 1 } }]), 'shows a stop nobody chose: 1 stages held');
  refuses('a stop the link names, never said', workflowLinkProblems([{ ...flowLinks[0], got: { ...flowLinks[0].got, status: null } }]), 'says nothing of the stop w-failed');
  refuses('another view that opens an object nobody named', workflowLinkProblems([{ ...flowLinks[2], got: { ...flowLinks[2].got, node: 'o-job' } }]), 'opens o-job, not no object');
  refuses('no link into the workflow', workflowLinkProblems([]), 'no link into the composed workflow');

  const flowKeys = { chosen: true, step: true, arrow: true, end: true, home: true, stop: true };
  if (workflowKeyboardProblems(flowKeys).length !== 0) fail('workflowKeyboardProblems refused keys that hold', workflowKeyboardProblems(flowKeys));
  for (const [key, want] of [
    ['chosen', 'do not choose the composed workflow'],
    ['step', 'does not reach the composed workflow’s current step'],
    ['arrow', 'the arrow key does not move'],
    ['end', 'End does not move'],
    ['home', 'Home does not move'],
    ['stop', 'do not choose a stop'],
  ]) {
    refuses(`a workflow whose ${key} key fails`, workflowKeyboardProblems({ ...flowKeys, [key]: false }), want);
  }

  // The links between objects, sources and views.
  const anchorsThatHold = {
    anchors: {
      text: [
        { href: '#lx-text-o-res', target: 'text' },
        { href: '#a-heading', target: 'page' },
      ],
      app: [
        { href: '?lx-object=a#lifecycle-explorer', kind: 'select', value: 'a', family: 'one', display: 'inline' },
        { href: '?lx-family=two&lx-object=x#lifecycle-explorer', kind: 'select', value: 'x', family: 'two', display: 'inline' },
        { href: '?lx-view=audit&lx-source=a-one#lifecycle-explorer', kind: 'source', value: 'a-one', display: 'inline-block' },
        { href: '?lx-view=objects&lx-node=o-job#lifecycle-explorer', kind: 'node', value: 'o-job', display: 'inline' },
        { href: '?lx-view=objects&lx-node=o-res#lifecycle-explorer', kind: 'open', value: null, display: 'inline-block' },
      ],
      buttons: 0,
      duplicates: [],
    },
  };
  const spoilAnchor = (index, change) => ({
    anchors: { ...anchorsThatHold.anchors, app: anchorsThatHold.anchors.app.map((one, at) => (at === index ? { ...one, ...change } : one)) },
  });
  const spoilAnchors = (change) => ({ anchors: { ...anchorsThatHold.anchors, ...change } });
  if (anchorProblems(anchorsThatHold, 'one').length !== 0) fail('anchorProblems refused anchors that hold', anchorProblems(anchorsThatHold, 'one'));
  for (const [name, spoiled, want] of [
    ['a text that links nothing', spoilAnchors({ text: [] }), 'the text links none of its own entries'],
    ['a text that links only headings', spoilAnchors({ text: [{ href: '#a-heading', target: 'page' }] }), 'the text links none of its own entries'],
    ['a text link to nowhere', spoilAnchors({ text: [{ href: '#lx-text-gone', target: 'none' }] }), 'the link #lx-text-gone leads nowhere'],
    ['a text link into the hidden view', spoilAnchors({ text: [{ href: '#x', target: 'app' }] }), 'leads into the hidden view'],
    ['no relation link', spoilAnchors({ app: anchorsThatHold.anchors.app.filter((one) => one.kind !== 'select') }), 'has no select link'],
    ['no link into another view', spoilAnchors({ app: anchorsThatHold.anchors.app.filter((one) => one.kind !== 'open') }), 'has no open link'],
    ['a relation link to another object', spoilAnchor(0, { href: '?lx-object=b#lifecycle-explorer' }), 'names b, and selects a'],
    ['a relation link without its family', spoilAnchor(1, { href: '?lx-object=x#lifecycle-explorer' }), 'names the family none, and sits in two'],
    ['a relation link naming the default family', spoilAnchor(0, { href: '?lx-family=one&lx-object=a#lifecycle-explorer' }), 'names the family one, and sits in one'],
    ['a relation link that leaves the explorer', spoilAnchor(0, { href: '?lx-object=a' }), 'does not lead to the explorer on this page'],
    ['a relation link to another page', spoilAnchor(0, { href: '../other/?lx-object=a#lifecycle-explorer' }), 'does not lead to the explorer on this page'],
    ['a source link to the journey', spoilAnchor(2, { href: '?lx-source=a-one#lifecycle-explorer' }), 'does not name the audit view and a-one'],
    ['an object map link to another object', spoilAnchor(3, { href: '?lx-view=objects&lx-node=o-res#lifecycle-explorer' }), 'does not name the object map and o-job'],
    ['a link that names nothing', spoilAnchor(4, { href: '#lifecycle-explorer' }), 'names nothing to open'],
    ['a relation link laid out as a block', spoilAnchor(3, { display: 'inline-block' }), 'is laid out inline-block, not inline'],
    ['a related object left a button', spoilAnchors({ buttons: 2 }), '2 related objects or sources are buttons'],
    ['an id used twice', spoilAnchors({ duplicates: ['lx-text-a'] }), 'the id "lx-text-a" is used twice'],
  ]) {
    refuses(name, anchorProblems(spoiled, 'one'), want);
  }

  const landings = [
    { href: '?lx-family=two&lx-object=x#lifecycle-explorer', got: { view: 'journey', family: 'two', object: 'x' } },
    { href: '?lx-path=side&lx-stage=side-a#lifecycle-explorer', got: { view: 'journey', family: 'one', path: 'side', stage: 'side-a', object: null } },
    { href: '?lx-view=audit&lx-source=a-one#lifecycle-explorer', got: { view: 'audit', source: 'a-one' } },
    { href: '?lx-view=objects&lx-node=o-job#lifecycle-explorer', got: { view: 'objects', node: 'o-job' } },
  ];
  if (anchorLandingProblems(landings, 'one').length !== 0) fail('anchorLandingProblems refused landings that hold', anchorLandingProblems(landings, 'one'));
  const spoilLanding = (index, change) => landings.map((one, at) => (at === index ? { ...one, got: { ...one.got, ...change } } : one));
  for (const [name, spoiled, want] of [
    ['an object link that opens the default family', spoilLanding(0, { family: 'one' }), 'opens with family one, not two'],
    ['an object link that selects nothing', spoilLanding(0, { object: null }), 'opens with object none, not x'],
    ['a path link that opens another path', spoilLanding(1, { path: 'main' }), 'opens with path main, not side'],
    ['a path link at another stage', spoilLanding(1, { stage: 'one' }), 'opens with stage one, not side-a'],
    ['a source link that opens the journey', spoilLanding(2, { view: 'journey' }), 'opens with view journey, not audit'],
    ['an object map link that opens nothing', spoilLanding(3, { node: null }), 'opens with node none, not o-job'],
    ['no link opened', [], 'no link between objects, sources or views was opened'],
  ]) {
    refuses(name, anchorLandingProblems(spoiled, 'one'), want);
  }

  const plain = (kind, href, extra = {}) => ({ kind, href, reloaded: false, landed: true, search: new URL(href, 'https://site.invalid/page/').search, ...extra });
  const clicksThatHold = {
    plain: [
      plain('select', '?lx-object=a#lifecycle-explorer', { kept: true, search: '?lx-stage=two&lx-object=a' }),
      plain('source', '?lx-view=audit&lx-source=a-one#lifecycle-explorer'),
      plain('node', '?lx-view=objects&lx-node=o-job#lifecycle-explorer'),
      plain('open', '?lx-view=objects&lx-node=o-res#lifecycle-explorer'),
    ],
    modified: { href: '?lx-view=objects&lx-node=o-res#lifecycle-explorer', opened: '?lx-view=objects&lx-node=o-res', stayed: true },
  };
  const spoilClick = (index, change) => ({ ...clicksThatHold, plain: clicksThatHold.plain.map((one, at) => (at === index ? { ...one, ...change } : one)) });
  if (inPlaceProblems(clicksThatHold).length !== 0) fail('inPlaceProblems refused clicks that hold', inPlaceProblems(clicksThatHold));
  for (const [name, spoiled, want] of [
    ['a link that loads the page', spoilClick(3, { reloaded: true }), 'loads the page again'],
    ['a link that opens nothing', spoilClick(2, { landed: false }), 'does not open what it names'],
    ['a relation that leaves the stage', spoilClick(0, { kept: false }), 'does not keep the stage it was made at'],
    ['a view opened with another address', spoilClick(3, { search: '?lx-view=objects' }), 'writes ?lx-view=objects, not ?lx-view=objects&lx-node=o-res'],
    ['a kind of link never clicked', { ...clicksThatHold, plain: clicksThatHold.plain.slice(1) }, 'no select link was followed by a click'],
    ['no click with a modifier', { ...clicksThatHold, modified: null }, 'no link was clicked with a modifier'],
    ['a modified click that opens no page', { ...clicksThatHold, modified: { ...clicksThatHold.modified, opened: null } }, 'opens no page'],
    ['a modified click followed in place', { ...clicksThatHold, modified: { ...clicksThatHold.modified, stayed: false } }, 'also changes the view it was made in'],
  ]) {
    refuses(name, inPlaceProblems(spoiled), want);
  }

  // The depths: the model, what the page tags, what each depth shows, the
  // address, what a move keeps, and the keyboard.
  const depthFixture = {
    depths: [
      { id: 'overview', label: 'Overview', text: 'Less.' },
      { id: 'product', label: 'Product', text: 'More.' },
      { id: 'implementation', label: 'Implementation', text: 'All.' },
    ],
    defaultDepth: 'implementation',
    depthOf: {
      stage: { lead: 'overview', you: 'overview', done: 'overview', phase: 'product', operator: 'product', expect: 'product', inspect: 'product', limits: 'product', kubernetes: 'implementation', executor: 'implementation', objects: 'implementation' },
      object: { purpose: 'overview', writer: 'overview', inspect: 'product', lives: 'implementation' },
      source: { establishes: 'overview', cannot: 'overview', records: 'product', readers: 'product', retention: 'product', survives: 'product', names: 'product', gaps: 'product', ids: 'implementation' },
      node: { holds: 'overview', writtenBy: 'overview', change: 'product', deletion: 'product', inspect: 'product', gap: 'product', operator: 'implementation', scope: 'implementation' },
      wstage: { goal: 'overview', person: 'overview', gate: 'overview', done: 'overview', operator: 'product', inspect: 'product', decision: 'product', limits: 'product', objects: 'implementation' },
      block: { auditMeasured: 'product', auditIdentities: 'product', auditGaps: 'product', workflowPreconditions: 'product', workflowDecisions: 'product', workflowRecord: 'implementation' },
    },
  };
  const withDepthOf = (kind, change) => ({ ...depthFixture, depthOf: { ...depthFixture.depthOf, [kind]: { ...depthFixture.depthOf[kind], ...change } } });
  const withoutPart = (kind, name) => {
    const { [name]: dropped, ...rest } = depthFixture.depthOf[kind];
    return { ...depthFixture, depthOf: { ...depthFixture.depthOf, [kind]: rest } };
  };
  if (depthModelProblems(depthFixture).length !== 0) fail('depthModelProblems refused depths that hold', depthModelProblems(depthFixture));
  // The page's own model is held to the same rule.
  if (depthModelProblems({ depths, defaultDepth, depthOf }).length !== 0) fail('the explorer’s depths', depthModelProblems({ depths, defaultDepth, depthOf }));
  for (const [name, spoiled, want] of [
    ['a depth missing', { ...depthFixture, depths: depthFixture.depths.slice(0, 2) }, 'the depths are overview, product, not overview, product, implementation'],
    ['depths out of order', { ...depthFixture, depths: [...depthFixture.depths].reverse() }, 'the depths are implementation, product, overview'],
    ['a depth that says nothing', { ...depthFixture, depths: [depthFixture.depths[0], { id: 'product', label: 'Product', text: ' ' }, depthFixture.depths[2]] }, 'the depth "product" lacks its label'],
    ['a shallower default', { ...depthFixture, defaultDepth: 'product' }, 'the default depth is "product", not "implementation"'],
    ['a kind left out', (() => { const { node, ...rest } = depthFixture.depthOf; return { ...depthFixture, depthOf: rest }; })(), 'depthOf leaves out the kind "node"'],
    ['a kind nothing renders', { ...depthFixture, depthOf: { ...depthFixture.depthOf, chart: { bars: 'overview' } } }, 'depthOf names the kind "chart", which nothing renders'],
    ['a part left out', withoutPart('stage', 'limits'), 'depthOf.stage leaves out the part "limits"'],
    ['a part nothing renders', withDepthOf('object', { colour: 'product' }), 'depthOf.object names "colour", which nothing renders'],
    ['a part at no depth', withDepthOf('source', { ids: 'deep' }), 'depthOf.source.ids is "deep", which is not a depth'],
    ['a stage that hides who acts', withDepthOf('stage', { lead: 'product' }), 'depthOf.stage.lead first shows at "product"'],
    ['a stage that hides when it is done', withDepthOf('stage', { done: 'implementation' }), 'depthOf.stage.done first shows at "implementation"'],
    ['a source that hides what it cannot establish', withDepthOf('source', { cannot: 'product' }), 'depthOf.source.cannot first shows at "product"'],
    ['a workflow stage that hides its gate', withDepthOf('wstage', { gate: 'product' }), 'depthOf.wstage.gate first shows at "product"'],
    ['a kind that adds nothing at a depth', withDepthOf('object', { lives: 'product' }), 'depthOf.object shows nothing first at "implementation"'],
  ]) {
    refuses(name, depthModelProblems(spoiled), want);
  }

  const depthStage = { id: 's1', lead: 'executor', actors: { you: 'Y.', operator: 'O.', executor: 'E.' }, expect: 'X.', limits: '' };
  if (expectedParts('stage', depthStage, 'app').join(' ') !== 'done expect inspect lead objects operator phase you') {
    fail('expectedParts read a stage as', expectedParts('stage', depthStage, 'app'));
  }
  const depthSource = { id: 'a-one' };
  const depthContext = { identities: [{ recordedIn: ['a-one'] }], gaps: [{ sources: ['a-two'] }] };
  if (expectedParts('source', depthSource, 'app', depthContext).includes('gaps') || !expectedParts('source', depthSource, 'app', depthContext).includes('names')) {
    fail('expectedParts read a source panel as', expectedParts('source', depthSource, 'app', depthContext));
  }
  if (expectedParts('source', depthSource, 'text', depthContext).includes('names')) fail('expectedParts gave the text a source’s names', []);
  const depthEntries = { stage: [depthStage], source: [depthSource] };
  const tagged = (where, kind, entry) => ({
    where,
    kind,
    id: entry.id,
    parts: expectedParts(kind, entry, where, depthContext).map((name) => [name, depthFixture.depthOf[kind][name]]),
  });
  const depthRendered = {
    entries: ['app', 'text'].flatMap((where) => [tagged(where, 'stage', depthStage), tagged(where, 'source', depthSource)]),
    blocks: ['app', 'text'].flatMap((where) => Object.entries(depthFixture.depthOf.block).map(([name, at]) => ({ where, name, at }))),
    strays: [],
  };
  const renderContext = { depthOf: depthFixture.depthOf, entries: depthEntries, context: depthContext };
  if (depthRenderProblems(depthRendered, renderContext).length !== 0) fail('depthRenderProblems refused tags that hold', depthRenderProblems(depthRendered, renderContext));
  const spoilEntry = (where, kind, change) => ({
    ...depthRendered,
    entries: depthRendered.entries.map((one) => (one.where === where && one.kind === kind ? { ...one, ...change(one) } : one)),
  });
  for (const [name, spoiled, want] of [
    ['an entry rendered without its parts', { ...depthRendered, entries: depthRendered.entries.filter((one) => !(one.where === 'text' && one.kind === 'stage')) }, 'the text’s stage "s1" is not rendered with its parts'],
    ['a part left untagged', spoilEntry('app', 'stage', (one) => ({ parts: one.parts.filter(([part]) => part !== 'expect') })), 'carries the parts done, inspect, lead, objects, operator, phase, you, not done, expect,'],
    ['the lead tagged as its actor', spoilEntry('app', 'stage', (one) => ({ parts: one.parts.map(([part, at]) => (part === 'lead' ? ['executor', 'implementation'] : [part, at])) })), 'carries the parts done, executor, expect'],
    ['a part tagged at another depth', spoilEntry('text', 'stage', (one) => ({ parts: one.parts.map(([part, at]) => (part === 'done' ? [part, 'product'] : [part, at])) })), 'shows done at "product", not "overview"'],
    ['a source’s names put in the text', spoilEntry('text', 'source', (one) => ({ parts: [...one.parts, ['names', 'product']] })), 'the text’s source "a-one" carries the parts cannot, establishes, ids, names'],
    ['a block left untagged', { ...depthRendered, blocks: depthRendered.blocks.filter((one) => !(one.where === 'text' && one.name === 'workflowRecord')) }, 'the text does not tag the block "workflowRecord"'],
    ['a block at another depth', { ...depthRendered, blocks: depthRendered.blocks.map((one) => (one.name === 'auditGaps' && one.where === 'app' ? { ...one, at: 'overview' } : one)) }, 'block "auditGaps" shows at "overview", not "product"'],
    ['a part outside any entry', { ...depthRendered, strays: ['stage:done'] }, 'a part "stage:done" sits outside any entry or block'],
  ]) {
    refuses(name, depthRenderProblems(spoiled, renderContext), want);
  }

  const ids = depthFixture.depths.map((one) => one.id);
  const shownAt = (depth) => ({
    depth,
    checked: depth,
    notes: [depth],
    parts: [
      ['app', 'stage:done', 'overview', true],
      ['app', 'stage:phase', 'product', rankOf(ids, depth) >= 1],
      ['text', 'stage:objects', 'implementation', rankOf(ids, depth) >= 2],
    ],
    open: [{ kind: 'stage', id: 's1', parts: ['done', ...(rankOf(ids, depth) >= 1 ? ['phase'] : [])], want: [['done', 'overview'], ['phase', 'product']] }],
    textShown: true,
    links: 3,
    hiddenTargets: [],
  });
  const shownThatHold = ids.map(shownAt);
  if (depthShownProblems(shownThatHold, { ids, where: 'with its script' }).length !== 0) {
    fail('depthShownProblems refused depths that hold', depthShownProblems(shownThatHold, { ids, where: 'with its script' }));
  }
  const spoilShown = (depth, change) => shownThatHold.map((one) => (one.depth === depth ? { ...one, ...change(one) } : one));
  for (const [name, spoiled, want] of [
    ['a depth never chosen', shownThatHold.slice(1), 'the depth "overview" was never chosen'],
    ['a choice that reads another depth', spoilShown('product', () => ({ checked: 'implementation' })), 'the choice reads "implementation"'],
    ['two notes at once', spoilShown('overview', () => ({ notes: ['overview', 'product'] })), 'the notes shown are overview, product'],
    ['no note', spoilShown('overview', () => ({ notes: [] })), 'the notes shown are none'],
    ['nothing read', spoilShown('overview', () => ({ parts: [] })), 'no part was read'],
    ['a deeper part shown', spoilShown('overview', (one) => ({ parts: one.parts.map((p) => (p[1] === 'stage:phase' ? [...p.slice(0, 3), true] : p)) })), '1 app stage:phase part(s) are shown'],
    ['a part hidden at its own depth', spoilShown('product', (one) => ({ parts: one.parts.map((p) => (p[1] === 'stage:phase' ? [...p.slice(0, 3), false] : p)) })), '1 app stage:phase part(s) are hidden'],
    ['an essential hidden', spoilShown('overview', (one) => ({ parts: one.parts.map((p) => (p[1] === 'stage:done' ? [...p.slice(0, 3), false] : p)) })), '1 app stage:done part(s) are hidden'],
    ['a panel on screen that hides its own part', spoilShown('product', (one) => ({ open: [{ ...one.open[0], parts: ['done'] }] })), 'the stage "s1" on screen shows done, not done, phase'],
    ['a text link to a hidden part', spoilShown('overview', () => ({ hiddenTargets: ['#lx-text-w-dec-schema'] })), 'the link #lx-text-w-dec-schema leads to a part this depth hides'],
    ['no text link read', spoilShown('product', () => ({ links: 0 })), 'at product: no link of the text was read'],
  ]) {
    refuses(name, depthShownProblems(spoiled, { ids, where: 'with its script' }), want);
  }
  for (const [name, spoiled, want] of [
    ['the text off screen without the script', spoilShown('overview', () => ({ textShown: false })), 'without its script at overview: the text is not on screen'],
  ]) {
    refuses(name, depthShownProblems(spoiled, { ids, where: 'without its script', text: true }), want);
  }
  if (depthShownProblems(shownThatHold, { ids, where: 'without its script', text: true }).length !== 0) {
    fail('depthShownProblems refused a text that holds', depthShownProblems(shownThatHold, { ids, where: 'without its script', text: true }));
  }

  const addressThatHolds = [
    { depth: 'overview', before: '?lx-stage=plan&lx-object=job', search: '?lx-stage=plan&lx-object=job&lx-depth=overview' },
    { depth: 'product', before: '?lx-stage=plan&lx-object=job&lx-depth=overview', search: '?lx-stage=plan&lx-object=job&lx-depth=product' },
    { depth: 'implementation', before: '?lx-stage=plan&lx-object=job&lx-depth=product', search: '?lx-stage=plan&lx-object=job' },
  ];
  const addressContext = { ids, defaultDepth: 'implementation' };
  if (depthAddressProblems(addressThatHolds, addressContext).length !== 0) fail('depthAddressProblems refused addresses that hold', depthAddressProblems(addressThatHolds, addressContext));
  const spoilAddress = (index, change) => addressThatHolds.map((one, at) => (at === index ? { ...one, ...change } : one));
  for (const [name, spoiled, want] of [
    ['a depth not written', spoilAddress(0, { search: '?lx-stage=plan&lx-object=job' }), 'choosing overview writes ?lx-stage=plan&lx-object=job, with lx-depth absent, not overview'],
    ['the default written', spoilAddress(2, { search: '?lx-stage=plan&lx-object=job&lx-depth=implementation' }), 'with lx-depth implementation, not absent'],
    ['a depth that resets the stage', spoilAddress(1, { search: '?lx-depth=product' }), 'changes the rest of the address from ?lx-stage=plan&lx-object=job to ?'],
    ['no depth chosen', [], 'no depth was chosen in the interactive view'],
    ['a depth never chosen by click', addressThatHolds.slice(0, 2), 'the depth "implementation" was never chosen in the interactive view'],
  ]) {
    refuses(name, depthAddressProblems(spoiled, addressContext), want);
  }

  const keepThatHolds = {
    moves: [{ how: 'choosing the audit view', want: 'overview', checked: 'overview', search: '?lx-view=audit&lx-depth=overview', hidden: true }],
    links: [
      { link: '?lx-depth=product', want: { depth: 'product' }, got: { depth: 'product', view: 'journey' } },
      { link: '?lx-view=audit&lx-depth=overview', want: { depth: 'overview', view: 'audit' }, got: { depth: 'overview', view: 'audit' } },
      { link: '?lx-depth=deep', want: { depth: 'implementation' }, got: { depth: 'implementation', view: 'journey' } },
    ],
  };
  const keepContext = { defaultDepth: 'implementation' };
  if (depthKeepProblems(keepThatHolds, keepContext).length !== 0) fail('depthKeepProblems refused moves that hold', depthKeepProblems(keepThatHolds, keepContext));
  const spoilMove = (change) => ({ ...keepThatHolds, moves: [{ ...keepThatHolds.moves[0], ...change }] });
  const spoilLink = (index, change) => ({ ...keepThatHolds, links: keepThatHolds.links.map((one, at) => (at === index ? { ...one, got: { ...one.got, ...change } } : one)) });
  for (const [name, spoiled, want] of [
    ['a view that resets the depth', spoilMove({ checked: 'implementation' }), 'choosing the audit view leaves the depth at implementation, not overview'],
    ['a view that drops it from the address', spoilMove({ search: '?lx-view=audit' }), 'writes ?lx-view=audit, without lx-depth=overview'],
    ['a move that shows what the depth hides', spoilMove({ hidden: false }), 'choosing the audit view shows what overview hides'],
    ['no move made', { ...keepThatHolds, moves: [] }, 'no move was made with a depth chosen'],
    ['a link whose depth is ignored', spoilLink(0, { depth: 'implementation' }), 'the link ?lx-depth=product opens at the depth implementation, not product'],
    ['a link whose view is dropped', spoilLink(1, { view: 'journey' }), 'opens the journey view, not audit'],
    ['an unknown depth taken', spoilLink(2, { depth: 'overview' }), 'the link ?lx-depth=deep opens at the depth overview, not implementation'],
    ['no unknown depth tried', { ...keepThatHolds, links: keepThatHolds.links.slice(0, 2) }, 'no link that names a depth nobody defined was opened'],
  ]) {
    refuses(name, depthKeepProblems(spoiled, keepContext), want);
  }

  const depthKeys = { reached: true, back: true, applied: true, written: true, forward: true };
  if (depthKeyboardProblems(depthKeys).length !== 0) fail('depthKeyboardProblems refused keys that hold', depthKeyboardProblems(depthKeys));
  for (const [key, want] of [
    ['reached', 'Tab from the heading does not reach the depth choice'],
    ['back', 'does not choose the shallower depth'],
    ['applied', 'does not hide what it hides'],
    ['written', 'is not written to the address'],
    ['forward', 'does not choose the deeper depth again'],
  ]) {
    refuses(`a depth choice whose ${key} fails`, depthKeyboardProblems({ ...depthKeys, [key]: false }), want);
  }

  // The CRDs this tree ships are read, every one of them.
  const read = crdKinds();
  if (read.length === 0 || read.some((one) => !one.kind || !one.scope)) fail('crdKinds read the CRDs as', read);

  console.log(
    'check-lifecycle-explorer.mjs --selftest: OK (the families, their models and their paths, nested turns, the ' +
      'rendered view, the keyboard on every path and at every turn, each selection, the family switch and its reset, ' +
      'the text without JavaScript, shared links, the address, the narrow layout, the audit view: its model, ' +
      'its commands, its text, each loss and source, the view switch, its links and its keyboard, and the object ' +
      'map: its model against the CRDs, its owner references, its text, each object and its marks, its links and its keyboard, ' +
      'the composed workflow: its model against #817, its text, its view, every stage and stop, how a stop clears, its links ' +
      'and its keyboard, and the links between objects, sources and views: their addresses, where each lands and how a click ' +
      'follows it, and the depths: their model, the parts each entry carries, what each depth shows with and without the ' +
      'script, the address, what a move keeps, and the keyboard)',
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
      .evaluate(() => ({
        title: document.title,
        present: Boolean(document.getElementById('lifecycle-explorer')),
        state: document.readyState,
        at: location.href,
        scripts: document.scripts.length,
      }))
      .catch(() => null);
    const what = seen
      ? `the page "${seen.title}" at ${seen.at}, ${seen.state}, with ${seen.scripts} scripts, explorer ${seen.present ? 'present' : 'absent'}`
      : 'no answer from the page';
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
// open, and without JavaScript, where the text is what is on screen. Then
// the audit view: at rest, after every loss, and with every source open; and
// the object map at rest and with every object open. One long word in one
// panel is enough to push a phone column sideways, so no panel is left
// unread.
async function measureNarrow(browser, url, models, auditModel, map, flow) {
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
          for (const { id } of depths.slice(0, -1)) {
            await chooseDepth(page, id);
            readings.push(await readOverflow(page, `${at}, without JavaScript at the depth ${id}`));
          }
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
        await chooseView(page, 'audit');
        readings.push(await readOverflow(page, `${at}, audit at rest`));
        for (const loss of auditModel.losses) {
          await page.locator(`${AUDIT_VIEW} [data-lx-loss="${loss.id}"]`).click();
          readings.push(await readOverflow(page, `${at}, audit after ${loss.id}`));
        }
        for (const source of auditModel.sources) {
          await page.locator(`${AUDIT_VIEW} [data-lx-source="${source.id}"]`).click();
          readings.push(await readOverflow(page, `${at}, audit with ${source.id} open`));
        }
        await chooseView(page, 'objects');
        readings.push(await readOverflow(page, `${at}, object map at rest`));
        for (const node of map.nodes) {
          await page.locator(`${OBJECTS_VIEW} [data-lx-node="${node.id}"]`).click();
          readings.push(await readOverflow(page, `${at}, object map with ${node.id} open`));
        }
        await chooseView(page, 'workflow');
        readings.push(await readOverflow(page, `${at}, workflow at rest`));
        for (const stage of flow.stages) {
          await chooseStep(page, stage.id);
          readings.push(await readOverflow(page, `${at}, workflow at ${stage.id}`));
          for (const { outcome } of stage.outcomes) {
            await chooseStop(page, outcome);
            readings.push(await readOverflow(page, `${at}, workflow at ${stage.id} stopped by ${outcome}`));
          }
        }
        // Each shallower depth reflows what is left; the deepest is every
        // reading above.
        for (const { id } of depths.slice(0, -1)) {
          await chooseDepth(page, id);
          for (const view of ['journey', 'audit', 'objects', 'workflow']) {
            await chooseView(page, view);
            readings.push(await readOverflow(page, `${at}, ${view} at the depth ${id}`));
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
      const flat = (element) => (element ? element.textContent.replace(/\s+/g, ' ') : '');
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
        audit: (() => {
          const block = text?.querySelector('[data-lx-text-audit]');
          if (!block) return null;
          return {
            sources: [...block.querySelectorAll('[data-lx-text-source]')].map((item) => ({
              id: item.dataset.lxTextSource,
              text: flat(item),
              survives: [...item.querySelectorAll('[data-lx-text-survives]')].map((row) => row.dataset.lxTextSurvives),
            })),
            identities: [...block.querySelectorAll('[data-lx-text-identity]')].map((item) => item.dataset.lxTextIdentity),
            gaps: [...block.querySelectorAll('[data-lx-text-gap]')].map((item) => ({
              issue: item.dataset.lxTextGap,
              href: item.querySelector('a')?.getAttribute('href') ?? '',
            })),
          };
        })(),
        objects: (() => {
          const block = text?.querySelector('[data-lx-text-objects]');
          if (!block) return null;
          return {
            types: [...block.querySelectorAll('[data-lx-text-type]')].map((item) => item.dataset.lxTextType),
            nodes: [...block.querySelectorAll('[data-lx-text-node]')].map((item) => ({
              id: item.dataset.lxTextNode,
              text: flat(item),
              relations: [...item.querySelectorAll('[data-lx-text-relation]')].map((row) => row.dataset.lxTextRelation),
            })),
          };
        })(),
        workflow: (() => {
          const block = text?.querySelector('[data-lx-text-workflow]');
          if (!block) return null;
          return {
            proposal: flat(block.querySelector('[data-lx-text-proposal]')),
            marks: [...block.querySelectorAll('[data-lx-text-mark]')].map((item) => item.dataset.lxTextMark),
            preconditions: [...block.querySelectorAll('[data-lx-text-precondition]')].map((item) => ({
              id: item.dataset.lxTextPrecondition,
              text: flat(item),
            })),
            stages: [...block.querySelectorAll('[data-lx-text-wstage]')].map((item) => ({
              id: item.dataset.lxTextWstage,
              text: flat(item),
              operator: flat(item.querySelector('[data-lx-text-wpart="operator"]')),
              gate: flat(item.querySelector('[data-lx-text-wpart="gate"]')),
              stops: [...item.querySelectorAll('[data-lx-text-wstop]')].map((row) => row.dataset.lxTextWstop),
            })),
            outcomes: [...block.querySelectorAll('[data-lx-text-woutcome]')].map((item) => item.dataset.lxTextWoutcome),
            decisions: [...block.querySelectorAll('[data-lx-text-decision]')].map((item) => ({ id: item.dataset.lxTextDecision, text: flat(item) })),
            record: [...block.querySelectorAll('[data-lx-text-record]')].map((item) => item.dataset.lxTextRecord),
            text: flat(block),
          };
        })(),
      };
    });
  } finally {
    await context.close();
  }
}

const AUDIT_VIEW = `${ROOT} [data-lx-audit]`;
const OBJECTS_VIEW = `${ROOT} [data-lx-objects]`;
const chooseView = (page, id) => page.locator(`${ROOT} [data-lx-view="${id}"]`).click();

// Which view is on screen, the loss and the source the audit view holds, the
// object the object map holds, and the family, path, stage and object the
// change journey holds, with the address the explorer wrote.
const readViews = (page) =>
  page.evaluate(() => {
    const root = document.getElementById('lifecycle-explorer');
    const audit = root?.querySelector('[data-lx-audit]');
    const objects = root?.querySelector('[data-lx-objects]');
    const flow = root?.querySelector('[data-lx-workflow]');
    const shown = [
      ['journey', root?.querySelector('[data-lx-journey]')],
      ['audit', audit],
      ['objects', objects],
      ['workflow', flow],
    ]
      .filter(([, element]) => element && !element.hidden)
      .map(([id]) => id);
    const familyView = [...(root?.querySelectorAll('[data-lx-family-view]') ?? [])].find((view) => !view.hidden);
    const list = familyView?.querySelector('[data-lx-steps]:not([hidden])');
    return {
      view: shown.length === 0 ? 'neither' : shown.join(' and '),
      loss: audit?.querySelector('[data-lx-loss][aria-pressed="true"]')?.dataset.lxLoss ?? null,
      source: [...(audit?.querySelectorAll('[data-lx-source-panel]') ?? [])].find((panel) => !panel.hidden)?.dataset.lxSourcePanel ?? null,
      node: [...(objects?.querySelectorAll('[data-lx-node-panel]') ?? [])].find((panel) => !panel.hidden)?.dataset.lxNodePanel ?? null,
      family: root?.querySelector('[data-lx-family][aria-pressed="true"]')?.dataset.lxFamily ?? null,
      path: familyView?.querySelector('[data-lx-path][aria-pressed="true"]')?.dataset.lxPath ?? null,
      stage: list?.querySelector('[data-lx-step][aria-current="step"]')?.dataset.lxStep ?? null,
      object: familyView?.querySelector('[data-lx-object][aria-pressed="true"]')?.dataset.lxObject ?? null,
      step: flow?.querySelector('[data-lx-wstep][aria-current="step"]')?.dataset.lxWstep ?? null,
      outcome: flow?.querySelector('[data-lx-wstage-panel]:not([hidden]) [data-lx-woutcome][aria-pressed="true"]')?.dataset.lxWoutcome ?? null,
      search: location.search,
    };
  });

// The links a reader may be handed into the audit view: the view alone, a
// loss and a source with it, a source or a loss alone, the view named with
// a journey value, and audit values that name nothing.
function auditLinksFor(models, auditModel) {
  const [first] = models;
  const other = models.at(-1);
  const firstSource = auditModel.sources[0].id;
  const lastSource = auditModel.sources.at(-1).id;
  const firstLoss = auditModel.losses[0].id;
  const lastLoss = auditModel.losses.at(-1).id;
  return [
    { link: '?lx-view=audit', want: { view: 'audit', loss: null, source: null } },
    { link: `?lx-view=audit&lx-loss=${lastLoss}&lx-source=${lastSource}`, want: { view: 'audit', loss: lastLoss, source: lastSource } },
    { link: `?lx-source=${firstSource}`, want: { view: 'audit', loss: null, source: firstSource } },
    { link: `?lx-loss=${firstLoss}`, want: { view: 'audit', loss: firstLoss, source: null } },
    {
      link: `?lx-view=audit&lx-family=${other.id}&lx-stage=${other.stages[1].id}&lx-source=${lastSource}`,
      want: { view: 'audit', loss: null, source: lastSource },
    },
    {
      link: `?lx-source=${first.objects[0].id}&lx-loss=${first.stages[0].id}`,
      want: { view: 'journey', loss: null, source: null, family: first.id },
    },
  ];
}

// The audit view, driven: chosen from the bar, every loss, every source with
// the last loss still chosen, an identity's source followed, the switches
// between the views from states that are default in nothing, the links, and
// the keyboard from the heading above the explorer.
async function measureAudit(browser, url, models, auditModel) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  const thrown = [];
  page.on('pageerror', (error) => thrown.push(String(error)));
  try {
    await open(page, url);
    await chooseView(page, 'audit');
    const render = await page.evaluate(() => {
      const root = document.getElementById('lifecycle-explorer');
      const audit = root.querySelector('[data-lx-audit]');
      const visible = (element) => element && element.getClientRects().length > 0 && getComputedStyle(element).display !== 'none';
      const panels = [...(audit?.querySelectorAll('[data-lx-source-panel]') ?? [])];
      return {
        viewButtons: root.querySelectorAll('[data-lx-view]').length,
        shown: visible(audit),
        journeyShown: visible(root.querySelector('[data-lx-journey]')),
        sourceButtons: audit?.querySelectorAll('[data-lx-source]').length ?? 0,
        panels: panels.length,
        visiblePanels: panels.filter((panel) => visible(panel)).length,
        lossButtons: audit?.querySelectorAll('[data-lx-loss]').length ?? 0,
        marked: audit?.querySelectorAll('[data-lx-source][data-verdict]').length ?? 0,
      };
    });

    const losses = [];
    for (const loss of auditModel.losses) {
      await page.locator(`${AUDIT_VIEW} [data-lx-loss="${loss.id}"]`).click();
      losses.push(
        await page.evaluate((id) => {
          const audit = document.querySelector('#lifecycle-explorer [data-lx-audit]');
          return {
            loss: id,
            pressed: [...audit.querySelectorAll('[data-lx-loss][aria-pressed="true"]')].map((button) => button.dataset.lxLoss),
            verdicts: Object.fromEntries(
              [...audit.querySelectorAll('[data-lx-source]')].map((node) => [
                node.dataset.lxSource,
                { verdict: node.dataset.verdict ?? null, badge: node.querySelector('[data-lx-verdict]')?.textContent ?? '' },
              ]),
            ),
          };
        }, loss.id),
      );
    }
    const sources = [];
    for (const source of auditModel.sources) {
      await page.locator(`${AUDIT_VIEW} [data-lx-source="${source.id}"]`).click();
      sources.push(
        await page.evaluate((id) => {
          const audit = document.querySelector('#lifecycle-explorer [data-lx-audit]');
          const shown = [...audit.querySelectorAll('[data-lx-source-panel]')].filter((panel) => !panel.hidden);
          return {
            source: id,
            loss: audit.querySelector('[data-lx-loss][aria-pressed="true"]')?.dataset.lxLoss ?? null,
            panels: shown.map((panel) => panel.dataset.lxSourcePanel),
            pressed: [...audit.querySelectorAll('[data-lx-source][aria-pressed="true"]')].map((node) => node.dataset.lxSource),
            current: shown[0]?.querySelector('[data-lx-survives][data-current]')?.dataset.lxSurvives ?? null,
          };
        }, source.id),
      );
    }
    // An identity's source other than the one open now.
    const opened = auditModel.sources.at(-1).id;
    const want = (await page.$$eval(`${AUDIT_VIEW} [data-lx-source-select]`, (chips) => chips.map((chip) => chip.dataset.lxSourceSelect))).find(
      (id) => id !== opened,
    );
    let chip = null;
    if (want) {
      await page.locator(`${AUDIT_VIEW} [data-lx-source-select="${want}"]`).first().click();
      chip = { want, got: (await readViews(page)).source };
    }

    // The switches: from the audit view with a loss and a source, from the
    // journey left in another family at its last path, stage and object, and
    // from the audit view again.
    const switches = [];
    const auditState = (state) => `the audit view after ${state.loss ?? 'no loss'} with ${state.source ?? 'no source'}`;
    let left = await readViews(page);
    await chooseView(page, 'journey');
    switches.push({ to: 'journey', from: auditState(left), got: await readViews(page) });
    const other = models.at(-1);
    await chooseFamily(page, other);
    const journeyLeft = await unsettle(page, other);
    await chooseView(page, 'audit');
    switches.push({ to: 'audit', from: describe(journeyLeft), got: await readViews(page) });
    await page.locator(`${AUDIT_VIEW} [data-lx-loss]`).last().click();
    await page.locator(`${AUDIT_VIEW} [data-lx-source]`).last().click();
    left = await readViews(page);
    await chooseView(page, 'journey');
    switches.push({ to: 'journey', from: auditState(left), got: await readViews(page) });

    const links = [];
    for (const one of auditLinksFor(models, auditModel)) {
      await open(page, `${url}${one.link}`);
      const got = await readViews(page);
      links.push({ ...one, got: { view: got.view, loss: got.loss, source: got.source, family: got.family } });
    }

    const keyboard = { chosen: false, loss: false, source: false };
    await open(page, url);
    await focusHeading(page);
    if (await tabTo(page, `${ROOT} [data-lx-view="audit"]`, 20)) {
      await page.keyboard.press('Enter');
      keyboard.chosen = (await readViews(page)).view === 'audit';
    }
    if (keyboard.chosen && (await tabTo(page, `${AUDIT_VIEW} [data-lx-loss]`, 20))) {
      const id = await page.evaluate(() => document.activeElement.dataset.lxLoss);
      await page.keyboard.press('Enter');
      keyboard.loss = (await readViews(page)).loss === id;
    }
    if (keyboard.loss && (await tabTo(page, `${AUDIT_VIEW} [data-lx-source]`, 20))) {
      const id = await page.evaluate(() => document.activeElement.dataset.lxSource);
      await page.keyboard.press('Enter');
      keyboard.source = (await readViews(page)).source === id;
    }

    return { render, selection: { losses, sources, chip }, switches, links, keyboard, thrown };
  } finally {
    await context.close();
  }
}

// The links a reader may be handed into the object map: the view alone, an
// object with it, an object alone, the view named with another view's
// values, a value that is no object of it, and another view named with an
// object.
function objectMapLinksFor(models, auditModel, map) {
  const [first] = models;
  const firstNode = map.nodes[0].id;
  const lastNode = map.nodes.at(-1).id;
  return [
    { link: '?lx-view=objects', want: { view: 'objects', node: null } },
    { link: `?lx-view=objects&lx-node=${lastNode}`, want: { view: 'objects', node: lastNode } },
    { link: `?lx-node=${firstNode}`, want: { view: 'objects', node: firstNode } },
    {
      link: `?lx-view=objects&lx-stage=${first.stages[1].id}&lx-source=${auditModel.sources[0].id}&lx-node=${firstNode}`,
      want: { view: 'objects', node: firstNode },
    },
    { link: `?lx-node=${auditModel.sources[0].id}`, want: { view: 'journey', node: null } },
    { link: `?lx-node=${first.objects[0].id}`, want: { view: 'journey', node: null } },
    { link: `?lx-view=audit&lx-node=${lastNode}`, want: { view: 'audit', node: null } },
  ];
}

// The object map, driven: chosen from the bar, every object selected in
// turn, a related object followed from a panel, the switches into and out of
// it from states that are default in nothing, the links, and the keyboard
// from the heading above the explorer.
async function measureObjectMap(browser, url, models, auditModel, map) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  const thrown = [];
  page.on('pageerror', (error) => thrown.push(String(error)));
  try {
    await open(page, url);
    await chooseView(page, 'objects');
    const render = await page.evaluate(() => {
      const root = document.getElementById('lifecycle-explorer');
      const objects = root.querySelector('[data-lx-objects]');
      const visible = (element) => element && element.getClientRects().length > 0 && getComputedStyle(element).display !== 'none';
      const panels = [...(objects?.querySelectorAll('[data-lx-node-panel]') ?? [])];
      return {
        viewButtons: root.querySelectorAll('[data-lx-view]').length,
        shown: visible(objects),
        journeyShown: visible(root.querySelector('[data-lx-journey]')),
        auditShown: visible(root.querySelector('[data-lx-audit]')),
        nodeButtons: objects?.querySelectorAll('[data-lx-node]').length ?? 0,
        panels: panels.length,
        visiblePanels: panels.filter((panel) => visible(panel)).length,
        legend: [...(objects?.querySelectorAll('.lx__legend [data-type]') ?? [])].map((item) => item.dataset.type),
        marked: objects?.querySelectorAll('[data-lx-node][data-related]').length ?? 0,
        hintShown: visible(objects?.querySelector('[data-lx-node-hint]')),
      };
    });

    const nodes = [];
    for (const node of map.nodes) {
      await page.locator(`${OBJECTS_VIEW} [data-lx-node="${node.id}"]`).click();
      nodes.push(
        await page.evaluate((id) => {
          const objects = document.querySelector('#lifecycle-explorer [data-lx-objects]');
          const shown = [...objects.querySelectorAll('[data-lx-node-panel]')].filter((panel) => !panel.hidden);
          return {
            id,
            panels: shown.map((panel) => panel.dataset.lxNodePanel),
            pressed: [...objects.querySelectorAll('[data-lx-node][aria-pressed="true"]')].map((button) => button.dataset.lxNode),
            listed: [...(shown[0]?.querySelectorAll('.lx__relations > li') ?? [])].map(
              (item) => `${item.dataset.type}:${item.querySelector('[data-lx-node-select]')?.dataset.lxNodeSelect}`,
            ),
            marks: Object.fromEntries(
              [...objects.querySelectorAll('[data-lx-node][data-related]')].map((button) => [
                button.dataset.lxNode,
                { type: button.dataset.related, badge: button.querySelector('[data-lx-rel]')?.textContent ?? '' },
              ]),
            ),
          };
        }, node.id),
      );
    }
    // A related object the open panel names, followed. Which one is read
    // from the model, so a panel that leaves it out fails here as a link that
    // opens nothing rather than as a wait for a button that never comes.
    const from = map.nodes.at(-1).id;
    const first = map.relations.find((relation) => relation.from === from || relation.to === from);
    const want = first && (first.from === from ? first.to : first.from);
    const target = page.locator(`${OBJECTS_VIEW} [data-lx-node-panel="${from}"] [data-lx-node-select="${want}"]`).first();
    let got = null;
    if ((await target.count()) > 0) {
      await target.click();
      got = (await readViews(page)).node;
    }
    const follow = { from, want, got };

    // The switches: from the object map with an object open to the journey
    // and to the audit view, and into the object map from the journey left in
    // another family and from the audit view left with a loss and a source.
    const switches = [];
    const mapState = (state) => `the object map with ${state.node ?? 'no object'} open`;
    let left = await readViews(page);
    await chooseView(page, 'journey');
    switches.push({ to: 'journey', from: mapState(left), got: await readViews(page) });
    const other = models.at(-1);
    await chooseFamily(page, other);
    const journeyLeft = await unsettle(page, other);
    await chooseView(page, 'objects');
    switches.push({ to: 'objects', from: describe(journeyLeft), got: await readViews(page) });
    await page.locator(`${OBJECTS_VIEW} [data-lx-node]`).last().click();
    left = await readViews(page);
    await chooseView(page, 'audit');
    switches.push({ to: 'audit', from: mapState(left), got: await readViews(page) });
    await page.locator(`${AUDIT_VIEW} [data-lx-loss]`).last().click();
    await page.locator(`${AUDIT_VIEW} [data-lx-source]`).last().click();
    const auditLeft = await readViews(page);
    await chooseView(page, 'objects');
    switches.push({ to: 'objects', from: `the audit view after ${auditLeft.loss} with ${auditLeft.source}`, got: await readViews(page) });

    const links = [];
    for (const one of objectMapLinksFor(models, auditModel, map)) {
      await open(page, `${url}${one.link}`);
      const got = await readViews(page);
      links.push({ ...one, got: { view: got.view, node: got.node } });
    }

    const keyboard = { chosen: false, node: false, follow: false };
    await open(page, url);
    await focusHeading(page);
    if (await tabTo(page, `${ROOT} [data-lx-view="objects"]`, 20)) {
      await page.keyboard.press('Enter');
      keyboard.chosen = (await readViews(page)).view === 'objects';
    }
    if (keyboard.chosen && (await tabTo(page, `${OBJECTS_VIEW} [data-lx-node]`, 20))) {
      const id = await page.evaluate(() => document.activeElement.dataset.lxNode);
      await page.keyboard.press('Enter');
      keyboard.node = (await readViews(page)).node === id;
    }
    const link = `${OBJECTS_VIEW} [data-lx-node-panel]:not([hidden]) [data-lx-node-select]`;
    if (keyboard.node && (await tabTo(page, link, map.nodes.length + 20))) {
      const id = await page.evaluate(() => document.activeElement.dataset.lxNodeSelect);
      await page.keyboard.press('Enter');
      keyboard.follow = (await readViews(page)).node === id;
    }

    return { render, selection: { nodes, follow }, switches, links, keyboard, thrown };
  } finally {
    await context.close();
  }
}

const WORKFLOW_VIEW = `${ROOT} [data-lx-workflow]`;

// Where the composed workflow stands: the current stage, the panel shown,
// the count, every step's state and mark, the status line, the stop pressed
// and its detail shown, how many stages are held, and the address.
const readWorkflow = (page) =>
  page.evaluate(() => {
    const flow = document.querySelector('#lifecycle-explorer [data-lx-workflow]');
    const steps = [...flow.querySelectorAll('[data-lx-wstep]')];
    const panels = [...flow.querySelectorAll('[data-lx-wstage-panel]')].filter((panel) => !panel.hidden);
    const status = flow.querySelector('[data-lx-wstatus]');
    return {
      current: steps.find((step) => step.getAttribute('aria-current') === 'step')?.dataset.lxWstep ?? null,
      panels: panels.map((panel) => panel.dataset.lxWstagePanel),
      count: flow.querySelector('[data-lx-wcount]')?.textContent.trim() ?? '',
      marks: steps.map((step) => [step.dataset.state ?? '', step.querySelector('[data-lx-wmark]')?.textContent.trim() ?? '']),
      status: status && !status.hidden ? status.textContent.trim() : null,
      pressed: panels.flatMap((panel) => [...panel.querySelectorAll('[data-lx-woutcome][aria-pressed="true"]')].map((one) => one.dataset.lxWoutcome)),
      details: panels.flatMap((panel) => [...panel.querySelectorAll('[data-lx-wstop]')].filter((one) => !one.hidden).map((one) => one.dataset.lxWstop)),
      held: steps.filter((step) => step.dataset.state === 'held').length,
      search: location.search,
    };
  });

const chooseStep = (page, id) => page.locator(`${WORKFLOW_VIEW} [data-lx-wstep="${id}"]`).click();
const chooseStop = (page, id) => page.locator(`${WORKFLOW_VIEW} [data-lx-wstage-panel]:not([hidden]) [data-lx-woutcome="${id}"]`).click();

// The links a reader may be handed into the composed workflow: the view
// alone, a stage and a stop with it, a stage alone, a stop the stage it names
// cannot have, a stage that does not exist, the view named with another
// view's values, a stage of it named under another view, and a journey path
// named in the place of a stage.
function workflowLinksFor(flow, models, map) {
  const [first] = models;
  const stopped = flow.stages.find((stage, index) => index > 0 && stage.outcomes.length > 0);
  const stop = stopped.outcomes.at(-1).outcome;
  const other = flow.stages.find((stage) => !stage.outcomes.some((one) => one.outcome === stop));
  const last = flow.stages.at(-1).id;
  const firstStage = flow.stages[0].id;
  return [
    { link: '?lx-view=workflow', want: { view: 'workflow', step: firstStage, outcome: null } },
    { link: `?lx-view=workflow&lx-step=${stopped.id}&lx-outcome=${stop}`, want: { view: 'workflow', step: stopped.id, outcome: stop } },
    { link: `?lx-step=${last}`, want: { view: 'workflow', step: last, outcome: null } },
    { link: `?lx-view=workflow&lx-step=${other.id}&lx-outcome=${stop}`, want: { view: 'workflow', step: other.id, outcome: null } },
    { link: '?lx-view=workflow&lx-step=w-nowhere', want: { view: 'workflow', step: firstStage, outcome: null } },
    { link: `?lx-view=workflow&lx-node=${map.nodes[0].id}&lx-stage=${first.stages[1].id}`, want: { view: 'workflow', step: firstStage, outcome: null, node: null } },
    { link: `?lx-view=objects&lx-step=${stopped.id}&lx-outcome=${stop}`, want: { view: 'objects', outcome: null, node: null } },
    { link: `?lx-step=${first.paths.at(-1).id}`, want: { view: 'journey', outcome: null } },
  ];
}

// The composed workflow, driven: chosen from the bar, every stage chosen in
// turn, every stop of every stage, each way a stop is cleared, the switches
// into and out of it from states that are default in nothing, the links, and
// the keyboard from the heading above the explorer.
async function measureWorkflow(browser, url, models, flow) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  const thrown = [];
  page.on('pageerror', (error) => thrown.push(String(error)));
  try {
    await open(page, url);
    await chooseView(page, 'workflow');
    const render = await page.evaluate(() => {
      const root = document.getElementById('lifecycle-explorer');
      const flow = root.querySelector('[data-lx-workflow]');
      const visible = (element) => element && element.getClientRects().length > 0 && getComputedStyle(element).display !== 'none';
      const panels = [...(flow?.querySelectorAll('[data-lx-wstage-panel]') ?? [])];
      return {
        viewButtons: root.querySelectorAll('[data-lx-view]').length,
        shown: visible(flow),
        othersShown: [
          ['journey', root.querySelector('[data-lx-journey]')],
          ['audit', root.querySelector('[data-lx-audit]')],
          ['objects', root.querySelector('[data-lx-objects]')],
        ]
          .filter(([, element]) => visible(element))
          .map(([id]) => id),
        steps: flow?.querySelectorAll('[data-lx-wstep]').length ?? 0,
        panels: panels.length,
        visiblePanels: panels.filter((panel) => visible(panel)).map((panel) => panel.dataset.lxWstagePanel),
        stopsShown: [...(flow?.querySelectorAll('[data-lx-wstop]') ?? [])].filter((one) => visible(one)).length,
        statusShown: visible(flow?.querySelector('[data-lx-wstatus]')),
        proposalShown: visible(flow?.querySelector('[data-lx-proposal]')),
        shipped: panels.filter((panel) => panel.querySelector('[data-lx-wpart="operator"] [data-mark="shipped"]')).length,
        proposed: panels.filter((panel) => panel.querySelector('[data-lx-wpart="gate"] [data-mark="proposed"]')).length,
        tags: panels.map((panel) => panel.querySelector('[data-lx-wfamily]')?.textContent.trim() ?? ''),
      };
    });

    const steps = [];
    for (const stage of flow.stages) {
      await chooseStep(page, stage.id);
      steps.push({ id: stage.id, ...(await readWorkflow(page)) });
    }
    const stops = [];
    for (const stage of flow.stages) {
      await chooseStep(page, stage.id);
      for (const { outcome } of stage.outcomes) {
        await chooseStop(page, outcome);
        stops.push({ stage: stage.id, outcome, ...(await readWorkflow(page)) });
      }
    }

    // A stop cleared by Next, by choosing an earlier stage, and by choosing
    // it again.
    const clears = [];
    const clearing = async (how, want) => {
      const got = await readWorkflow(page);
      clears.push({ how, want, got: { step: got.current, outcome: got.pressed[0] ?? null, held: got.held, search: got.search } });
    };
    const [stopping] = flow.stages.filter((stage) => stage.outcomes.length > 0).slice(0, -1);
    const after = flow.stages[flow.stages.indexOf(stopping) + 1];
    await chooseStep(page, stopping.id);
    await chooseStop(page, stopping.outcomes[0].outcome);
    await page.locator(`${WORKFLOW_VIEW} [data-lx-wnext]`).click();
    await clearing('Next', after.id);
    await chooseStop(page, after.outcomes[0].outcome);
    await chooseStep(page, stopping.id);
    await clearing('choosing an earlier stage', stopping.id);
    await chooseStop(page, stopping.outcomes[0].outcome);
    await chooseStop(page, stopping.outcomes[0].outcome);
    await clearing('choosing it again', stopping.id);

    // The switches: from the workflow stopped at a later stage to each other
    // view, and into the workflow from each of them left in a state that is
    // default in nothing.
    const switches = [];
    const flowState = async () => {
      const got = await readWorkflow(page);
      return `the composed workflow at ${got.current} stopped by ${got.pressed[0] ?? 'nothing'}`;
    };
    const stopAt = async () => {
      await chooseStep(page, after.id);
      await chooseStop(page, after.outcomes.at(-1).outcome);
    };
    for (const to of ['journey', 'audit', 'objects']) {
      await stopAt();
      const from = await flowState();
      await chooseView(page, to);
      switches.push({ to, from, got: await readViews(page) });
      let left;
      if (to === 'journey') {
        const other = models.at(-1);
        await chooseFamily(page, other);
        left = describe(await unsettle(page, other));
      } else if (to === 'audit') {
        await page.locator(`${AUDIT_VIEW} [data-lx-loss]`).last().click();
        await page.locator(`${AUDIT_VIEW} [data-lx-source]`).last().click();
        left = 'the audit view with a loss and a source';
      } else {
        await page.locator(`${OBJECTS_VIEW} [data-lx-node]`).last().click();
        left = 'the object map with an object open';
      }
      await chooseView(page, 'workflow');
      switches.push({ to: 'workflow', from: left, got: await readViews(page) });
    }

    const links = [];
    for (const one of workflowLinksFor(flow, models, objectMap)) {
      await open(page, `${url}${one.link}`);
      const got = await readViews(page);
      const flowNow = await readWorkflow(page);
      links.push({ ...one, got: { view: got.view, step: got.step, outcome: got.outcome, node: got.node, held: flowNow.held, status: flowNow.status } });
    }

    const keyboard = { chosen: false, step: false, arrow: false, end: false, home: false, stop: false };
    await open(page, url);
    await focusHeading(page);
    if (await tabTo(page, `${ROOT} [data-lx-view="workflow"]`, 20)) {
      await page.keyboard.press('Enter');
      keyboard.chosen = (await readViews(page)).view === 'workflow';
    }
    const current = `${WORKFLOW_VIEW} [data-lx-wstep][aria-current="step"]`;
    if (keyboard.chosen) keyboard.step = await tabTo(page, current, 20);
    if (keyboard.step) {
      await page.keyboard.press('ArrowRight');
      keyboard.arrow = (await readWorkflow(page)).current === flow.stages[1].id && (await activeMatches(page, current));
      await page.keyboard.press('End');
      keyboard.end = (await readWorkflow(page)).current === flow.stages.at(-1).id && (await activeMatches(page, current));
      await page.keyboard.press('Home');
      keyboard.home = (await readWorkflow(page)).current === flow.stages[0].id && (await activeMatches(page, current));
    }
    if (keyboard.home && (await tabTo(page, `${WORKFLOW_VIEW} [data-lx-wstage-panel]:not([hidden]) [data-lx-woutcome]`, 40))) {
      const id = await page.evaluate(() => document.activeElement.dataset.lxWoutcome);
      await page.keyboard.press('Enter');
      keyboard.stop = (await readWorkflow(page)).pressed.join(' ') === id;
    }

    return { render, walk: { steps, stops, clears }, switches, links, keyboard, thrown };
  } finally {
    await context.close();
  }
}

// The links between objects, sources and views, read from the page: every
// link the text makes within the page and where it leads, every link of the
// interactive view with what it selects in place and how it is laid out, the
// related objects left buttons, and the page's ids used more than once.
const readAnchors = (page) =>
  page.evaluate(() => {
    const root = document.getElementById('lifecycle-explorer');
    const text = root.querySelector('[data-lx-text]');
    const app = root.querySelector('[data-lx-app]');
    const kindOf = (anchor) =>
      anchor.dataset.lxSelect !== undefined
        ? ['select', anchor.dataset.lxSelect]
        : anchor.dataset.lxSourceSelect !== undefined
          ? ['source', anchor.dataset.lxSourceSelect]
          : anchor.dataset.lxNodeSelect !== undefined
            ? ['node', anchor.dataset.lxNodeSelect]
            : anchor.dataset.lxOpen !== undefined
              ? ['open', null]
              : ['none', null];
    const ids = [...root.querySelectorAll('[id]')].map((element) => element.id);
    return {
      anchors: {
        text: [...text.querySelectorAll('a[href^="#"]')].map((anchor) => {
          const target = document.getElementById(decodeURIComponent(anchor.getAttribute('href').slice(1)));
          const where = !target ? 'none' : text.contains(target) ? 'text' : app.contains(target) ? 'app' : 'page';
          return { href: anchor.getAttribute('href'), target: where };
        }),
        app: [...app.querySelectorAll('a[data-lx-link]')].map((anchor) => {
          const [kind, value] = kindOf(anchor);
          return {
            href: anchor.getAttribute('href'),
            kind,
            value,
            family: anchor.closest('[data-lx-family-view]')?.dataset.lxFamilyView ?? null,
            display: getComputedStyle(anchor).display,
          };
        }),
        buttons: app.querySelectorAll('button[data-lx-node-select], button[data-lx-source-select], .lx__relations button[data-lx-select]').length,
        duplicates: [...new Set(ids.filter((id) => document.querySelectorAll(`#${CSS.escape(id)}`).length > 1))],
      },
    };
  });

// The links, driven: every distinct address the interactive view links to
// opened as a page, one link of each kind followed by a plain click with a
// marker on the page that a reload would lose, and one followed with a
// modifier, which has to open a page of its own.
async function measureAnchors(browser, url, models, flow) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  const thrown = [];
  page.on('pageerror', (error) => thrown.push(String(error)));
  try {
    await open(page, url);
    const reading = await readAnchors(page);

    const landing = [];
    for (const href of [...new Set(reading.anchors.app.map((one) => one.href))]) {
      await open(page, new URL(href, url).toString());
      landing.push({ href, got: await readViews(page) });
    }

    const mark = () => page.evaluate(() => {
      window.lxMarker = true;
    });
    const marked = () => page.evaluate(() => window.lxMarker === true).catch(() => false);
    // Only a link on screen is clicked, so a link the view lost is a link
    // that was not followed rather than a wait that never ends.
    const follow = async (kind, selector, landed) => {
      const link = page.locator(selector).filter({ visible: true }).first();
      if ((await link.count()) === 0) return;
      const href = await link.getAttribute('href');
      const value = await link.evaluate((anchor) => anchor.dataset.lxSelect ?? anchor.dataset.lxSourceSelect ?? anchor.dataset.lxNodeSelect ?? null);
      await mark();
      await link.click();
      const reloaded = !(await marked());
      if (reloaded) await page.locator(`${ROOT}[data-lx-ready]`).waitFor();
      const got = await readViews(page);
      return { kind, href, reloaded, got, ...landed(got, href, value) };
    };
    const plain = [];
    const push = (one) => one && plain.push(one);

    // A related object in the journey, from a stage other than the first.
    const [first] = models;
    const object = first.relations[0].from;
    await open(page, url);
    await page.locator(`${VISIBLE_STEP}[data-lx-step="${first.stages[1].id}"]`).click();
    await page.locator(`${VIEW} [data-lx-object="${object}"]`).click();
    push(
      await follow('select', `${VIEW} [data-lx-object-panel="${object}"] a[data-lx-select]`, (got, href, value) => ({
        landed: got.object === value,
        kept: got.stage === first.stages[1].id,
      })),
    );
    // An identity's source in the audit view.
    await chooseView(page, 'audit');
    push(await follow('source', `${AUDIT_VIEW} a[data-lx-source-select]`, (got, href, value) => ({ landed: got.source === value })));
    // A related object in the object map.
    await chooseView(page, 'objects');
    await page.locator(`${OBJECTS_VIEW} [data-lx-node="${objectMap.nodes[0].id}"]`).click();
    push(
      await follow('node', `${OBJECTS_VIEW} [data-lx-node-panel]:not([hidden]) a[data-lx-node-select]`, (got, href, value) => ({
        landed: got.node === value,
      })),
    );
    // From the workflow into the object map, and into the change journey.
    const opens = (got, href) => {
      const want = landingFor(href, first.id);
      return { landed: Object.entries(want).every(([key, value]) => value === undefined || got[key] === value), search: got.search };
    };
    await chooseView(page, 'workflow');
    push(await follow('open', `${WORKFLOW_VIEW} [data-lx-wstage-panel]:not([hidden]) a[data-lx-open]`, opens));
    const toJourney = flow.stages.flatMap((stage) => stage.outcomes.filter((one) => one.journey).map((one) => ({ stage: stage.id, outcome: one.outcome })))[0];
    if (toJourney) {
      await open(page, `${url}?lx-view=workflow&lx-step=${toJourney.stage}&lx-outcome=${toJourney.outcome}`);
      push(await follow('open', `${WORKFLOW_VIEW} [data-lx-wstop]:not([hidden]) a[data-lx-open]`, opens));
    }

    // A link clicked with a modifier opens its address in a page of its own
    // and leaves this one as it was.
    let modified = null;
    await open(page, `${url}?lx-view=workflow`);
    const link = page.locator(`${WORKFLOW_VIEW} [data-lx-wstage-panel]:not([hidden]) a[data-lx-open]`).filter({ visible: true }).first();
    if ((await link.count()) > 0) {
      const href = await link.getAttribute('href');
      const before = await readViews(page);
      const popup = context.waitForEvent('page', { timeout: 10_000 }).catch(() => null);
      await link.click({ modifiers: ['ControlOrMeta'] });
      const opened = await popup;
      let search = null;
      if (opened) {
        await opened.waitForLoadState('domcontentloaded').catch(() => {});
        search = new URL(opened.url()).search;
        await opened.close();
      }
      const after = await readViews(page);
      modified = { href, opened: search, stayed: JSON.stringify(after) === JSON.stringify(before) };
    }

    return { reading, landing, clicks: { plain, modified }, thrown };
  } finally {
    await context.close();
  }
}

// The entries each depth is read on, the content they come from and the
// identities and gaps a source panel names.
const depthEntries = () => ({
  stage: families.flatMap((family) => [...family.scenario.stages, ...family.paths.flatMap((path) => path.stages ?? [])]),
  object: families.flatMap((family) => family.objects),
  source: audit.sources,
  node: objectMap.nodes,
  wstage: workflow.stages,
});
const depthContext = () => ({ identities: audit.identities, gaps: audit.gaps });

// Every tagged part of the page, by the entry or block it belongs to: an
// entry is a panel of the interactive view or an entry of the text, and a
// part of another kind inside it, or outside every entry and block, is a
// stray.
const readDepthTags = (page) =>
  page.evaluate(() => {
    const root = document.getElementById('lifecycle-explorer');
    const app = root.querySelector('[data-lx-app]');
    const text = root.querySelector('[data-lx-text]');
    const kinds = [
      ['stage', '[data-lx-stage-panel]', 'lxStagePanel', '[data-lx-text-stage]', 'lxTextStage'],
      ['object', '[data-lx-object-panel]', 'lxObjectPanel', '[data-lx-text-object]', 'lxTextObject'],
      ['source', '[data-lx-source-panel]', 'lxSourcePanel', '[data-lx-text-source]', 'lxTextSource'],
      ['node', '[data-lx-node-panel]', 'lxNodePanel', '[data-lx-text-node]', 'lxTextNode'],
      ['wstage', '[data-lx-wstage-panel]', 'lxWstagePanel', '[data-lx-text-wstage]', 'lxTextWstage'],
    ];
    const claimed = new Set();
    const entries = [];
    for (const [kind, appSelector, appKey, textSelector, textKey] of kinds) {
      for (const [where, scope, selector, key] of [
        ['app', app, appSelector, appKey],
        ['text', text, textSelector, textKey],
      ]) {
        for (const element of scope.querySelectorAll(selector)) {
          const parts = [...element.querySelectorAll(`[data-lx-part^="${kind}:"]`)];
          for (const one of parts) claimed.add(one);
          entries.push({
            where,
            kind,
            id: element.dataset[key],
            parts: parts.map((one) => [one.dataset.lxPart.slice(kind.length + 1), one.dataset.lxDepth]),
          });
        }
      }
    }
    const blocks = [...root.querySelectorAll('[data-lx-part^="block:"]')].map((one) => {
      claimed.add(one);
      return { where: app.contains(one) ? 'app' : 'text', name: one.dataset.lxPart.slice('block:'.length), at: one.dataset.lxDepth };
    });
    const strays = [...root.querySelectorAll('[data-lx-part]')].filter((one) => !claimed.has(one)).map((one) => one.dataset.lxPart);
    return { entries, blocks, strays };
  });

// What one depth shows: the choice, the note shown, every tagged part with
// whether its own style hides it, the parts of each entry open on screen,
// and, where the text is on screen, the links of it whose target the depth
// hides.
const readDepthShown = (page, opened) =>
  page.evaluate((opened) => {
    const root = document.getElementById('lifecycle-explorer');
    const app = root.querySelector('[data-lx-app]');
    const text = root.querySelector('[data-lx-text]');
    const onScreen = (element) => element.getClientRects().length > 0;
    return {
      checked: root.querySelector('[data-lx-depth-choice]:checked')?.value ?? null,
      notes: [...root.querySelectorAll('[data-lx-depth-note]')]
        .filter((note) => getComputedStyle(note).display !== 'none')
        .map((note) => note.dataset.lxDepthNote),
      parts: [...root.querySelectorAll('[data-lx-part]')].map((one) => [
        app.contains(one) ? 'app' : 'text',
        one.dataset.lxPart,
        one.dataset.lxDepth,
        getComputedStyle(one).display !== 'none',
      ]),
      open: opened.map(({ kind, selector, key }) => {
        const element = [...root.querySelectorAll(selector)].find(onScreen);
        if (!element) return { kind, id: `none on screen for ${selector}`, parts: [], want: [['an entry', 'overview']] };
        const tagged = [...element.querySelectorAll(`[data-lx-part^="${kind}:"]`)];
        return {
          kind,
          id: element.dataset[key],
          parts: tagged.filter(onScreen).map((one) => one.dataset.lxPart.slice(kind.length + 1)),
          want: tagged.map((one) => [one.dataset.lxPart.slice(kind.length + 1), one.dataset.lxDepth]),
        };
      }),
      textShown: onScreen(text),
      links: [...text.querySelectorAll('a[href^="#"]')].filter(onScreen).length,
      hiddenTargets: [...text.querySelectorAll('a[href^="#"]')]
        .filter(onScreen)
        .filter((link) => {
          const target = document.getElementById(decodeURIComponent(link.getAttribute('href').slice(1)));
          return target && text.contains(target) && !onScreen(target);
        })
        .map((link) => link.getAttribute('href')),
    };
  }, opened);

const chooseDepth = (page, id) => page.locator(`${ROOT} [data-lx-depth-choice][value="${id}"]`).check();
const readDepth = (page) => page.evaluate(() => document.querySelector('#lifecycle-explorer [data-lx-depth-choice]:checked')?.value ?? null);

// The panels a depth is read on in the interactive view, and the entries it
// is read on in the text.
const APP_OPENED = [
  { kind: 'stage', selector: '[data-lx-stage-panel]', key: 'lxStagePanel' },
  { kind: 'object', selector: '[data-lx-object-panel]', key: 'lxObjectPanel' },
];
const VIEW_OPENED = {
  audit: [{ kind: 'source', selector: '[data-lx-source-panel]', key: 'lxSourcePanel' }],
  objects: [{ kind: 'node', selector: '[data-lx-node-panel]', key: 'lxNodePanel' }],
  workflow: [{ kind: 'wstage', selector: '[data-lx-wstage-panel]', key: 'lxWstagePanel' }],
};
const TEXT_OPENED = [
  { kind: 'stage', selector: '[data-lx-text-stage]', key: 'lxTextStage' },
  { kind: 'object', selector: '[data-lx-text-object]', key: 'lxTextObject' },
  { kind: 'source', selector: '[data-lx-text-source]', key: 'lxTextSource' },
  { kind: 'node', selector: '[data-lx-text-node]', key: 'lxTextNode' },
  { kind: 'wstage', selector: '[data-lx-text-wstage]', key: 'lxTextWstage' },
];

// The depths, driven with the script: the tags read once; each depth chosen
// on a stage and an object of the journey, a source, an object of the map and
// a workflow stage, each read for what it shows and the address it writes;
// the depth kept through every kind of move; links that name a depth; and
// the keyboard.
async function measureDepths(browser, url) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  const thrown = [];
  page.on('pageerror', (error) => thrown.push(String(error)));
  try {
    await open(page, url);
    const tags = await readDepthTags(page);

    // The stage of the default path that carries the most parts, so each
    // depth hides something in it.
    const [first] = families;
    const stage = [...first.scenario.stages].sort(
      (left, right) => expectedParts('stage', right, 'app').length - expectedParts('stage', left, 'app').length,
    )[0];
    const object = first.objects.find((one) => stage.objects.includes(one.id)) ?? first.objects[0];
    const node = objectMap.nodes.find((one) => one.gap) ?? objectMap.nodes[0];
    const source = audit.sources.find((one) => audit.identities.some((identity) => identity.recordedIn.includes(one.id))) ?? audit.sources[0];
    const wstage = workflow.stages.find((one) => one.decision && one.limits) ?? workflow.stages[0];
    const states = [
      { search: `?lx-stage=${stage.id}&lx-object=${object.id}`, opened: APP_OPENED },
      { search: `?lx-view=audit&lx-source=${source.id}`, opened: VIEW_OPENED.audit },
      { search: `?lx-view=objects&lx-node=${node.id}`, opened: VIEW_OPENED.objects },
      { search: `?lx-view=workflow&lx-step=${wstage.id}`, opened: VIEW_OPENED.workflow },
    ];
    const shown = [];
    const address = [];
    for (const state of states) {
      await open(page, `${url}${state.search}`);
      for (const id of depths.map((one) => one.id)) {
        const before = await page.evaluate(() => location.search);
        await chooseDepth(page, id);
        const search = await page.evaluate(() => location.search);
        if (state === states[0]) address.push({ depth: id, before, search });
        shown.push({ depth: id, ...(await readDepthShown(page, state.opened)) });
      }
    }

    // Every kind of move with Overview chosen. What it hides has to stay
    // hidden wherever the move lands.
    const [shallow] = depths;
    const moves = [];
    const keep = async (how) => {
      const got = await page.evaluate(() => ({
        search: location.search,
        hidden: [...document.querySelectorAll('#lifecycle-explorer [data-lx-depth="product"], #lifecycle-explorer [data-lx-depth="implementation"]')].every(
          (one) => getComputedStyle(one).display === 'none',
        ),
      }));
      moves.push({ how, want: shallow.id, checked: await readDepth(page), ...got });
    };
    await open(page, url);
    await chooseDepth(page, shallow.id);
    for (const view of ['audit', 'objects', 'workflow', 'journey']) {
      await chooseView(page, view);
      await keep(`choosing the ${view} view`);
    }
    const other = families.at(-1);
    await page.locator(`${ROOT} [data-lx-family="${other.id}"]`).click();
    await keep('choosing another family');
    const turned = other.paths.find((path) => path.turnsAfter);
    await page.locator(`${VIEW} [data-lx-path="${turned.id}"]`).click();
    await keep('choosing a path');
    await page.locator(VISIBLE_STEP).nth(1).click();
    await keep('choosing a stage');
    await page.locator(`${VIEW} [data-lx-object]`).first().click();
    await page.locator(`${VIEW} [data-lx-object-panel]:not([hidden]) a[data-lx-select]`).first().click();
    await keep('following a related object');
    await chooseView(page, 'workflow');
    const stopped = workflow.stages.find((one) => one.outcomes.some((outcome) => outcome.journey));
    const stop = stopped.outcomes.find((outcome) => outcome.journey);
    await page.locator(`${WORKFLOW_VIEW} [data-lx-wstep="${stopped.id}"]`).click();
    await page.locator(`${WORKFLOW_VIEW} [data-lx-wstage-panel]:not([hidden]) [data-lx-woutcome="${stop.outcome}"]`).click();
    await page.locator(`${WORKFLOW_VIEW} [data-lx-wstop]:not([hidden]) a[data-lx-open]`).first().click();
    await keep('following a link into another view');

    const links = [];
    for (const one of [
      { link: '?lx-depth=product', want: { depth: 'product' } },
      { link: `?lx-view=audit&lx-depth=${shallow.id}`, want: { depth: shallow.id, view: 'audit' } },
      { link: '?lx-depth=deep', want: { depth: defaultDepth } },
      { link: `?lx-depth=${defaultDepth}`, want: { depth: defaultDepth } },
    ]) {
      await open(page, `${url}${one.link}`);
      links.push({ ...one, got: { depth: await readDepth(page), view: (await readViews(page)).view } });
    }

    const keyboard = { reached: false, back: false, applied: false, written: false, forward: false };
    await open(page, url);
    await focusHeading(page);
    // The first stop inside the explorer is the depth chosen.
    keyboard.reached = (await tabTo(page, `${ROOT} *`, 20)) && (await activeMatches(page, `${ROOT} [data-lx-depth-choice]:checked`));
    if (keyboard.reached) {
      await page.keyboard.press('ArrowLeft');
      const back = depths.at(-2).id;
      keyboard.back = (await readDepth(page)) === back;
      keyboard.applied = await page.evaluate(
        () => [...document.querySelectorAll('#lifecycle-explorer [data-lx-depth="implementation"]')].every((one) => getComputedStyle(one).display === 'none'),
      );
      keyboard.written = new URLSearchParams(await page.evaluate(() => location.search)).get('lx-depth') === back;
      await page.keyboard.press('ArrowRight');
      keyboard.forward = (await readDepth(page)) === defaultDepth && !(await page.evaluate(() => location.search)).includes('lx-depth');
    }

    return { tags, shown, address, keep: { moves, links }, keyboard, thrown };
  } finally {
    await context.close();
  }
}

// The depths without the script: the choice is the radio buttons alone, and
// the styles have to hide the same parts of the text. Read at rest, then at
// each depth chosen.
async function measureDepthsWithoutScript(browser, url) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, javaScriptEnabled: false });
  const page = await context.newPage();
  try {
    await open(page, url, { scripted: false });
    const readings = [{ depth: defaultDepth, ...(await readDepthShown(page, TEXT_OPENED)) }];
    for (const id of depths.map((one) => one.id)) {
      await chooseDepth(page, id);
      readings.push({ depth: id, ...(await readDepthShown(page, TEXT_OPENED)) });
    }
    return readings;
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
  problems.push(...auditProblems(audit, models));
  problems.push(...objectMapProblems(objectMap, { kinds: crdKinds(), familyModels: models, auditModel: audit }));
  problems.push(...workflowProblems(workflow, { familyModels: models, auditModel: audit, map: objectMap, kinds: crdKinds() }));
  problems.push(...depthModelProblems({ depths, defaultDepth, depthOf }));
  problems.push(...commandProblems(explorerCommands(families, audit, objectMap, workflow)));
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
  let anchors = { links: 0, text: 0 };
  let tagged = 0;

  try {
    const quiet = await measureWithoutScript(browser, url);
    problems.push(...textProblems(quiet, models));
    if (quiet.present) problems.push(...auditTextProblems(quiet, audit));
    if (quiet.present) problems.push(...objectMapTextProblems(quiet, objectMap));
    if (quiet.present) problems.push(...workflowTextProblems(quiet, workflow));
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
      if (measured.render?.ready) {
        const audited = await measureAudit(browser, url, models, audit);
        problems.push(...auditRenderProblems(audited.render, audit));
        problems.push(...auditSelectionProblems(audited.selection, audit));
        problems.push(...auditLinkProblems(audited.links));
        problems.push(...auditKeyboardProblems(audited.keyboard));
        if (audited.thrown.length > 0) problems.push(`the audit view threw: ${audited.thrown.join('; ')}`);
        const mapped = await measureObjectMap(browser, url, models, audit, objectMap);
        problems.push(...objectMapRenderProblems(mapped.render, objectMap));
        problems.push(...objectMapSelectionProblems(mapped.selection, objectMap));
        problems.push(...objectMapLinkProblems(mapped.links));
        problems.push(...objectMapKeyboardProblems(mapped.keyboard));
        if (mapped.thrown.length > 0) problems.push(`the object map threw: ${mapped.thrown.join('; ')}`);
        const flowed = await measureWorkflow(browser, url, models, workflow);
        problems.push(...workflowRenderProblems(flowed.render, workflow));
        problems.push(...workflowWalkProblems(flowed.walk, workflow));
        problems.push(...workflowLinkProblems(flowed.links));
        problems.push(...workflowKeyboardProblems(flowed.keyboard));
        if (flowed.thrown.length > 0) problems.push(`the composed workflow threw: ${flowed.thrown.join('; ')}`);
        problems.push(...viewSwitchProblems([...audited.switches, ...mapped.switches, ...flowed.switches], models, workflow));
        const linked = await measureAnchors(browser, url, models, workflow);
        problems.push(...anchorProblems(linked.reading, defaultFamily));
        problems.push(...anchorLandingProblems(linked.landing, defaultFamily));
        problems.push(...inPlaceProblems(linked.clicks));
        if (linked.thrown.length > 0) problems.push(`following the links threw: ${linked.thrown.join('; ')}`);
        anchors = { links: linked.landing.length, text: linked.reading.anchors.text.length };
        const deep = await measureDepths(browser, url);
        const ids = depths.map((one) => one.id);
        problems.push(...depthRenderProblems(deep.tags, { depthOf, entries: depthEntries(), context: depthContext() }));
        problems.push(...depthShownProblems(deep.shown, { ids, where: 'with its script' }));
        problems.push(...depthAddressProblems(deep.address, { ids, defaultDepth }));
        problems.push(...depthKeepProblems(deep.keep, { defaultDepth }));
        problems.push(...depthKeyboardProblems(deep.keyboard));
        if (deep.thrown.length > 0) problems.push(`the depths threw: ${deep.thrown.join('; ')}`);
        problems.push(...depthShownProblems(await measureDepthsWithoutScript(browser, url), { ids, where: 'without its script', text: true }));
        tagged = deep.tags.entries.reduce((sum, one) => sum + one.parts.length, 0) + deep.tags.blocks.length;
      }
      problems.push(...overflowProblems(await measureNarrow(browser, url, models, audit, objectMap, workflow)));
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
      `the text without JavaScript, shared links, the audit view's ${audit.sources.length} sources, ` +
      `${audit.losses.length} losses, ${audit.identities.length} identities and ${audit.gaps.length} gaps, ` +
      `the object map's ${objectMap.nodes.length} objects and ${objectMap.relations.length} relationships against ` +
      `${crdKinds().length} CRD kinds, the composed workflow's ${workflow.stages.length} stages and ` +
      `${workflow.stages.reduce((sum, stage) => sum + stage.outcomes.length, 0)} stops, ${anchors.links} linked addresses and ` +
      `${anchors.text} links in the text, ${tagged} parts at ${depths.length} depths, ` +
      `${explorerCommands(families, audit, objectMap, workflow).length} commands, and ` +
      `${NARROW.map((one) => `${one.width}px`).join(' and ')} wide)`,
  );
}

await main();
