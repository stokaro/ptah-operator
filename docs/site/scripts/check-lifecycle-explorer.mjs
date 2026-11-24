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
//     map fits with every object open.
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

import { audit, defaultFamily, families, objectMap, parentOf, walkOf } from '../src/lifecycle-explorer.mjs';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const siteRoot = join(scriptDir, '..');
const PAGE = 'reference/reconciliation/';
// The views of the view bar, the default first.
export const VIEWS = ['journey', 'audit', 'objects'];
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
// sources and the object map, with where it is shown.
export function explorerCommands(familyList, auditModel, map) {
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
export function viewSwitchProblems(readings, familyModels) {
  const problems = [];
  const first = familyModels[0];
  for (const to of VIEWS) {
    if (!readings.some((reading) => reading.to === to)) problems.push(`no switch went to the ${to} view`);
  }
  for (const { to, from, got } of readings) {
    const at = `switching from ${from} to the ${to} view`;
    const search = new URLSearchParams(got.search);
    if (got.view !== to) problems.push(`${at} shows the ${got.view} view`);
    if (to !== 'journey') {
      if (got.loss !== null) problems.push(`${at} keeps the loss ${got.loss}`);
      if (got.source !== null) problems.push(`${at} keeps the source ${got.source}`);
      if (got.node !== null) problems.push(`${at} keeps the object ${got.node}`);
      if (search.get('lx-view') !== to) problems.push(`${at} writes ${got.search || 'nothing'}, not lx-view=${to}`);
      for (const param of ['lx-family', 'lx-path', 'lx-stage', 'lx-object', 'lx-loss', 'lx-source', 'lx-node']) {
        if (search.has(param)) problems.push(`${at} writes ${got.search}, with ${param}`);
      }
      continue;
    }
    if (got.family !== first.id) problems.push(`${at} opens the family ${got.family ?? 'none'}, not ${first.id}`);
    if (got.path !== first.paths[0].id) problems.push(`${at} opens the path ${got.path ?? 'none'}, not ${first.paths[0].id}`);
    if (got.stage !== first.stages[0].id) problems.push(`${at} opens the stage ${got.stage ?? 'none'}, not ${first.stages[0].id}`);
    if (got.object !== null) problems.push(`${at} keeps ${got.object} selected`);
    for (const param of ['lx-view', 'lx-loss', 'lx-source', 'lx-node', 'lx-family', 'lx-path', 'lx-object']) {
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
  );
  if (
    shown.map((one) => one.where).join(' | ') !==
    'the stage "s1" | the stage "s2" | the object "o" | the source "a-one" | the object map’s "o-n"'
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

  const auditRender = { viewButtons: 3, shown: true, journeyShown: false, sourceButtons: 2, panels: 2, lossButtons: 4, visiblePanels: 0, marked: 0 };
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

  const toAudit = { to: 'audit', from: 'two on b-nested', got: { view: 'audit', loss: null, source: null, node: null, search: '?lx-view=audit' } };
  const toJourney = {
    to: 'journey',
    from: 'the audit view after cluster with a-two',
    got: { view: 'journey', family: 'one', path: 'main', stage: 'one', object: null, search: '?lx-stage=one' },
  };
  const toObjects = {
    to: 'objects',
    from: 'the audit view after cluster with a-two',
    got: { view: 'objects', loss: null, source: null, node: null, search: '?lx-view=objects' },
  };
  const switchesThatHold = [toAudit, toJourney, toObjects];
  if (viewSwitchProblems(switchesThatHold, models).length !== 0) {
    fail('viewSwitchProblems refused switches that hold', viewSwitchProblems(switchesThatHold, models));
  }
  const spoilTo = (one, change) => [{ ...one, got: { ...one.got, ...change } }];
  for (const [name, spoiled, want] of [
    ['no switch to the audit view', [toJourney], 'no switch went to the audit view'],
    ['no switch to the journey', [toAudit], 'no switch went to the journey view'],
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
    refuses(name, viewSwitchProblems(spoiled, models), want);
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
    viewButtons: 3,
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
    ['a view button missing', { viewButtons: 2 }, '2 view buttons, not 3'],
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

  // The CRDs this tree ships are read, every one of them.
  const read = crdKinds();
  if (read.length === 0 || read.some((one) => !one.kind || !one.scope)) fail('crdKinds read the CRDs as', read);

  console.log(
    'check-lifecycle-explorer.mjs --selftest: OK (the families, their models and their paths, nested turns, the ' +
      'rendered view, the keyboard on every path and at every turn, each selection, the family switch and its reset, ' +
      'the text without JavaScript, shared links, the address, the narrow layout, the audit view: its model, ' +
      'its commands, its text, each loss and source, the view switch, its links and its keyboard, and the object ' +
      'map: its model against the CRDs, its owner references, its text, each object and its marks, its links and its keyboard)',
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
// open, and without JavaScript, where the text is what is on screen. Then
// the audit view: at rest, after every loss, and with every source open; and
// the object map at rest and with every object open. One long word in one
// panel is enough to push a phone column sideways, so no panel is left
// unread.
async function measureNarrow(browser, url, models, auditModel, map) {
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
    const shown = [
      ['journey', root?.querySelector('[data-lx-journey]')],
      ['audit', audit],
      ['objects', objects],
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

async function main() {
  if (process.argv.includes('--selftest')) {
    selftest();
    return;
  }

  const models = families.map(familyModel);
  const problems = familiesProblems(models);
  problems.push(...auditProblems(audit, models));
  problems.push(...objectMapProblems(objectMap, { kinds: crdKinds(), familyModels: models, auditModel: audit }));
  problems.push(...commandProblems(explorerCommands(families, audit, objectMap)));
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
    if (quiet.present) problems.push(...auditTextProblems(quiet, audit));
    if (quiet.present) problems.push(...objectMapTextProblems(quiet, objectMap));
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
        problems.push(...viewSwitchProblems([...audited.switches, ...mapped.switches], models));
      }
      problems.push(...overflowProblems(await measureNarrow(browser, url, models, audit, objectMap)));
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
      `${crdKinds().length} CRD kinds, ${explorerCommands(families, audit, objectMap).length} commands, and ` +
      `${NARROW.map((one) => `${one.width}px`).join(' and ')} wide)`,
  );
}

await main();
