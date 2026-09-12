// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

import { sidebar } from './src/sidebar.mjs';
import { Origin, BasePath } from './src/lib/docs-origin.mjs';

// The version this build documents, and the revision it was built from. Both
// come from the publishing workflow rather than from this file: a release is
// built from a worktree of its own source, so nothing in the tree can name the
// version being produced.
const DOCS_VERSION = process.env.DOCS_VERSION || 'edge';
const DOCS_SOURCE_REF = process.env.DOCS_SOURCE_REF || 'master';

export default defineConfig({
  site: Origin,
  base: BasePath(DOCS_VERSION),
  integrations: [
    starlight({
      title: `Ptah Operator ${DOCS_VERSION}`,
      description: 'The Kubernetes operator for Ptah: install it, give it a schema, and read what it did.',
      customCss: ['./src/styles/operator.css'],
      lastUpdated: true,
      // Edit links address the revision this build came from, not the default
      // branch. A reader on a release page who follows "Edit page" has to land
      // on the source of the page they are reading; master would hand them a
      // later one.
      editLink: {
        baseUrl: `https://github.com/stokaro/ptah-operator/edit/${DOCS_SOURCE_REF}/docs/site/`,
      },
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/stokaro/ptah-operator',
        },
      ],
      sidebar,
    }),
  ],
});
