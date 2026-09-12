/* The reading order of the recorded runs, by tag.
 *
 * What the runs are against, what the operator does with a schema, what it
 * refuses, and how you operate it afterwards.
 *
 * It lives here rather than in the recording because it is a decision about the
 * page: the recorder records what happened, and the order a reader meets it in
 * is not something that happened. It lives in a module of its own because a
 * gate reads it too, and a plain Node script cannot import the module that
 * imports the recording.
 */
export const TagOrder = ['Lab', 'Lifecycle', 'Safety', 'Operations'];

/**
 * Where one tag sorts.
 *
 * A tag nobody put in the order sorts last rather than first: a new scenario
 * appears at the end of the catalog instead of in front of the one a reader is
 * meant to start with. The gate refuses that case, so this is what happens
 * between somebody adding a tag and somebody noticing.
 */
export function rank(tag) {
	const index = TagOrder.indexOf(tag);
	return index < 0 ? TagOrder.length : index;
}
