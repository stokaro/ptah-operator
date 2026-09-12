/* The recording, read once and shaped for the page.
 *
 * demo/recordings/runs.json is written by demo/cmd/record against a live
 * cluster. This module is the only place the site reads it, so a page that
 * shows a run and a page that lists them cannot disagree about what is in it.
 */
import runs from '../../../../demo/recordings/runs.json';

export const Recording = runs;

/** Every run, in the catalog's reading order. */
export const Runs = runs.order.map((id) => runs.scenarios.find((one) => one.id === id)).filter(Boolean);

/** One run by its id. */
export function runById(id) {
	return runs.scenarios.find((one) => one.id === id);
}

/**
 * What the player reads, built from the same recording the page renders.
 *
 * The player is the one ptah.run uses, and it takes a catalog keyed by id with
 * a `script` on each entry. Shaping it here rather than changing the player
 * keeps one player for two sites.
 */
export function playerCatalog() {
	const scenarios = {};
	for (const run of Runs) {
		scenarios[run.id] = {
			script: run.events,
			tag: run.tags[0] ?? '',
			label: run.title,
			where: `sh · ${run.id}`,
			caption: run.tagline,
		};
	}
	return { scenarios, pinned: runs.order.slice(0, 1), rotating: runs.order, order: runs.order };
}

/**
 * What the run was recorded against.
 *
 * A transcript without this pairing is a screenshot: the same commands against
 * a different operator or a different Ptah build are a different demonstration.
 */
export function labSummary() {
	const lab = runs.lab ?? {};
	return [
		{ label: 'Kubernetes', value: lab.KUBERNETES_VERSION },
		{ label: 'Operator', value: shortRevision(lab.CONTROLLER_REVISION) },
		{ label: 'Ptah', value: lab.PTAH_VERSION },
		{ label: 'Executor', value: digestOf(lab.EXECUTOR_IMAGE) },
		{ label: 'Recorded', value: (runs.recordedAt ?? '').slice(0, 10) },
	].filter((one) => one.value);
}

function shortRevision(revision) {
	return revision ? revision.slice(0, 12) : '';
}

function digestOf(image) {
	if (!image) return '';
	const at = image.indexOf('@');
	return at < 0 ? image : image.slice(at + 1, at + 20);
}

/** How many checks held, across every run. */
export function checkCount() {
	return runs.scenarios.reduce((total, one) => total + one.checks.length, 0);
}
