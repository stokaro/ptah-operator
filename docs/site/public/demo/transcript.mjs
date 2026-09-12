/* How a recorded run becomes lines, for both readers of it.
 *
 * The page renders every run as a transcript in the markup, which is what a
 * reader without JavaScript gets and what a reader who does not want to wait
 * for a typewriter reads. The player replaces one of those blocks with a live
 * screen built the same way. Both import this file, so a printed session and a
 * played one cannot wrap differently or colour a line differently.
 */

// The class each event kind carries. A kind missing here renders unwrapped,
// which is what an ordinary line of output is.
export const CLASS = { mute: 'm', sql: 'a', new: 'n', err: 'e', note: 'c' };

export function esc(text) {
	return String(text).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

// A line whose meaning is its alignment: runs of spaces holding columns apart,
// or the rule that underlines them. Wrapping one turns a table into rubble, so
// these scroll instead while the prose around them keeps wrapping.
const COLUMNS = /[^\s] {2,}\S/;
const TABLE_RULE = /^[-+=|\s]{8,}$/;

export function tabular(html) {
	const text = String(html).replace(/<[^>]*>/g, '');
	return COLUMNS.test(text) || TABLE_RULE.test(text);
}

// Alignment is a property of a block, not of a line. A label followed by one
// space because it is the widest in its table looks like prose on its own, and
// would wrap away from the column it sets. A run of output lines is therefore
// wide if any line in it is, and a note, a command or a blank ends the run.
export function wideRuns(rows) {
	const breaks = (html) =>
		html === '' || /<span class="c">/.test(html) || /<span class="p">/.test(html);
	const wide = [];
	let index = 0;
	while (index < rows.length) {
		if (breaks(rows[index])) {
			wide[index] = false;
			index++;
			continue;
		}
		let end = index;
		let any = false;
		while (end < rows.length && !breaks(rows[end])) {
			if (tabular(rows[end])) any = true;
			end++;
		}
		for (; index < end; index++) wide[index] = any;
	}
	return wide;
}

// The session at rest: every event committed in order. `wait` is a beat and
// `sync` moves the pill, so neither leaves a line behind.
export function rowsOf(script) {
	const rows = [];
	for (const [kind, text] of script) {
		if (kind === 'wait' || kind === 'sync') continue;
		if (kind === 'blank') {
			rows.push('');
			continue;
		}
		// A note opens a step, and the typed run puts a blank row in front of
		// it. Printing it without one makes the printed session a different
		// text from the played one.
		if (kind === 'note' && rows.length && rows[rows.length - 1] !== '') rows.push('');
		let body = esc(text);
		if (kind === 'cmd') body = `<span class="p">$</span> ${body}`;
		const cls = CLASS[kind];
		rows.push(cls ? `<span class="${cls}">${body}</span>` : body);
	}
	return rows;
}

// The transcript as one string of line blocks. One line, one block, exactly as
// the player builds the live screen: a line too long for the frame wraps under
// its own indent instead of restarting at column zero.
export function transcript(script) {
	const rows = rowsOf(script);
	const wide = wideRuns(rows);
	return rows
		.map((inner, index) => `<span class="${wide[index] ? 'l l-wide' : 'l'}">${inner || ' '}</span>`)
		.join('');
}

// What a tile says about a run before you open it: the first command, and how
// much there is. Both are read off the script rather than typed beside it.
export function preview(script) {
	const command = script.find(([kind]) => kind === 'cmd');
	return command ? command[1].replace(/\s*\\$/, '') : '';
}

export function counts(script) {
	const commands = script.filter(([kind]) => kind === 'cmd').length;
	const lines = script.filter(([kind]) => kind !== 'wait' && kind !== 'sync').length;
	return `${commands} command${commands === 1 ? '' : 's'} · ${lines} lines`;
}
