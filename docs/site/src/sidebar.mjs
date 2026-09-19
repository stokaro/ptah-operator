// The navigation, in a module of its own so a plain Node script can read it.
//
// astro.config.mjs cannot be imported by a gate -- Starlight's entry point is
// TypeScript inside node_modules and Node refuses to strip types there -- so
// the sidebar the gate checks is this value, and the site renders the same one.
//
// The order is a reader's path through the operator: what it is, how to install
// it, one worked example, then how to configure it, what it does with a change,
// and finally the reference a reader returns to.
import { Runs } from './lib/runs.mjs';

export const sidebar = [
  {
    label: 'Start',
    items: [
      { label: 'Overview', link: '/' },
      { label: 'Try it locally', link: '/start/try-it/' },
      { label: 'Install', link: '/start/install/' },
      { label: 'First schema', link: '/start/first-schema/' },
    ],
  },
  {
    label: 'See it run',
    items: [
      { label: 'Recorded runs', link: '/demo/' },
      // Every run has a page, so every run is in the rail: a reader who is
      // reading one session should be able to see the others without going
      // back to the catalog first. The list is the recording's, in the order
      // the catalog puts them in, so a scenario recorded tomorrow appears here
      // without anybody remembering to add it.
      {
        label: 'Sessions',
        items: Runs.map((run) => ({ label: run.title, link: `/demo/${run.id}/` })),
      },
    ],
  },
  {
    label: 'Use',
    items: [
      { label: 'Configuration', link: '/use/configuration/' },
      { label: 'Read a plan', link: '/use/read-a-plan/' },
      { label: 'Run versioned migrations', link: '/use/migrations/' },
      { label: 'Manage reference data', link: '/use/reference-data/' },
      { label: 'Exact-plan approvals', link: '/use/approvals/' },
      { label: 'Operations', link: '/use/operations/' },
      { label: 'Security model', link: '/use/security/' },
    ],
  },
  // Architecture first, then the field reference generated from the API types
  // by make docs-reference. The group sits after the task pages because it is
  // what a reader returns to once they know which resource they are holding,
  // and before the support pages because those are about builds rather than
  // about the model.
  {
    label: 'Reference',
    items: [
      { label: 'Architecture', link: '/reference/architecture/' },
      { label: 'PtahSchema', link: '/reference/ptahschema/' },
      { label: 'PtahSchemaPlan', link: '/reference/ptahschemaplan/' },
      { label: 'PtahSchemaApproval', link: '/reference/ptahschemaapproval/' },
      { label: 'PtahMigration', link: '/reference/ptahmigration/' },
      { label: 'PtahMigrationPlan', link: '/reference/ptahmigrationplan/' },
      { label: 'PtahMigrationApproval', link: '/reference/ptahmigrationapproval/' },
    ],
  },
  {
    label: 'Support',
    items: [
      { label: 'Ptah compatibility', link: '/support/ptah/' },
      { label: 'Kubernetes support', link: '/support/kubernetes/' },
      { label: 'Databases and privileges', link: '/support/databases/' },
      { label: 'Releases and provenance', link: '/support/releases/' },
    ],
  },
  // The two pages a reader reaches for when something is wrong. They answer
  // across the groups above rather than inside one, so they sit beside them.
  { label: 'Condition reasons', link: '/troubleshoot/condition-reasons/' },
  { label: 'FAQ', link: '/faq/', badge: { text: '28', variant: 'default' } },
];
