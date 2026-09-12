// The navigation, in a module of its own so a plain Node script can read it.
//
// astro.config.mjs cannot be imported by a gate -- Starlight's entry point is
// TypeScript inside node_modules and Node refuses to strip types there -- so
// the sidebar the gate checks is this value, and the site renders the same one.
//
// The order is a reader's path through the operator: what it is, how to install
// it, one worked example, then how to configure it, what it does with a change,
// and finally the reference a reader returns to.
export const sidebar = [
  {
    label: 'Start',
    items: [
      { label: 'Overview', link: '/' },
      { label: 'Install', link: '/start/install/' },
      { label: 'First schema', link: '/start/first-schema/' },
    ],
  },
  {
    label: 'See it run',
    items: [{ label: 'Recorded runs', link: '/demo/' }],
  },
  {
    label: 'Use',
    items: [
      { label: 'Configuration', link: '/use/configuration/' },
      { label: 'Exact-plan approvals', link: '/use/approvals/' },
      { label: 'Operations', link: '/use/operations/' },
      { label: 'Security model', link: '/use/security/' },
    ],
  },
  {
    label: 'Troubleshoot',
    items: [{ label: 'Condition reasons', link: '/troubleshoot/condition-reasons/' }],
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
];
