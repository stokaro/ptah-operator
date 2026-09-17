// The symptoms a reader arrives with, and the questions each one reaches.
//
// A reader who hits a failure has the symptom, not the vocabulary: they know
// the operator said `Unknown`, not that the word for it is fail-closed. So a
// symptom typed into the filter field takes this curated route instead of a
// substring match, and reaches questions whose text never spells the phrase.
//
// Free text in the same field still matches what a question and its answer
// say. The symptoms are for the words that are not in the text.
//
// Each label is also a `searchAliases` entry on the page, so the same word
// typed into the site search lands here; scripts/check-faq-groups.mjs holds the
// two lists together.

/** @type {{ label: string, questions: string[] }[]} */
export const faqSymptoms = [
  {
    label: 'UnsupportedEngine',
    questions: ['unsupported-engine', 'untested-is-not-incompatible'],
  },
  {
    label: 'approval rejected',
    questions: ['approval-rejected', 'moved-tag-invalidates-approval', 'reviewer-cannot-read-sql'],
  },
  {
    label: 'nothing applies',
    questions: ['plan-recorded-not-applied', 'suspend-blocks-the-realm', 'apply-outcome-unknown'],
  },
  {
    label: 'tables still there',
    questions: ['deleting-never-drops-data', 'no-automatic-rollback'],
  },
  {
    label: 'registry auth fails',
    questions: ['registry-secret-authority', 'registry-outage-fail-closed'],
  },
  {
    label: 'status Unknown',
    questions: ['registry-outage-fail-closed', 'reading-convergence-status', 'apply-outcome-unknown'],
  },
  { label: 'digest refused', questions: ['digest-pin-refuses-a-tag', 'moved-tag-invalidates-approval'] },
  { label: 'plan too large', questions: ['plan-too-large', 'old-plan-objects'] },
  {
    label: 'install fails',
    questions: ['three-values-with-no-default', 'upgrade-cli-and-operator-together', 'kubernetes-version-window'],
  },
];
