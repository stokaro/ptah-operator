// The symptoms a reader arrives with, and the questions each one reaches.
//
// A chip is a curated route rather than a search term: somebody whose apply
// stopped has the condition reason, not the word the answer is written in. Free
// text in the same field still matches what a question and its answer say.
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
