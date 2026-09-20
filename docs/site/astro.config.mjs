// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import tailwindcss from '@tailwindcss/vite';

import { sidebar } from './src/sidebar.mjs';
import { Origin, BasePath } from './src/lib/docs-origin.mjs';
import { satteri } from '@astrojs/markdown-satteri';
import markdownHeadingIds from './src/lib/markdown-heading-ids.mjs';
import markdownFaqVerdict from './src/lib/markdown-faq-verdict.mjs';
import markdownMermaid from './src/lib/markdown-mermaid.mjs';

// The version this build documents, and the revision it was built from. Both
// come from the publishing workflow rather than from this file: a release is
// built from a worktree of its own source, so nothing in the tree can name the
// version being produced.
const DOCS_VERSION = process.env.DOCS_VERSION || 'edge';
const DOCS_SOURCE_REF = process.env.DOCS_SOURCE_REF || 'master';

export default defineConfig({
  site: Origin,
  base: BasePath(DOCS_VERSION),
  markdown: {
    // Astro 7 renders Markdown through Satteri, which slugs a heading from
    // its text and has no syntax for an author-chosen id. This plugin reads a
    // trailing `{#id}` and sets the id Satteri then honours, so a question
    // can be reworded without breaking a link somebody shared.
    processor: satteri({
      mdastPlugins: [markdownMermaid()],
      hastPlugins: [markdownHeadingIds(), markdownFaqVerdict()],
    }),
  },
  integrations: [
    starlight({
      // The product, without the version. Starlight appends the site title to
      // every page title, so a version here reads twice in one tab:
      // "Ptah Operator | Ptah Operator edge". The version belongs where a
      // reader looks for it -- in the address, and in build-info.json.
      // No `logo` option: SiteTitle.astro, which replaces the component that
      // would read it, imports src/assets/logo.svg itself and renders the
      // brand row beside the version pill.
      title: 'Ptah Operator',
      description: 'The Kubernetes operator for Ptah: install it, give it a schema, and read what it did.',
      // The design Ptah's documentation and ptah.run share: fonts.css declares
      // the faces, global.css holds the Tailwind theme and the layout
      // measures, ptah.css maps Starlight's variables onto the design's tokens
      // and restyles each surface. The files are copies rather than an import
      // of the other repository, because this site has to build from this
      // repository alone.
      customCss: ['./src/styles/fonts.css', './src/styles/global.css', './src/styles/ptah.css'],
      lastUpdated: true,
      // A symptom is the word a reader arrives with: the FAQ's symptoms are
      // phrases its prose does not spell, so a reader searching the site
      // reaches them only through the page's searchAliases, and a curated
      // alias outranks an incidental body match.
      pagefind: {
        ranking: { metaWeights: { searchAliases: 16 } },
      },
      components: {
        // The brand row with the version pill, the text header links across to
        // Ptah, and the light/dark toggle -- the three header surfaces the
        // shared design shapes.
        // The title, plus the page's search aliases as Pagefind metadata.
        PageTitle: './src/components/PageTitle.astro',
        SiteTitle: './src/components/SiteTitle.astro',
        SocialIcons: './src/components/HeaderLinks.astro',
        ThemeSelect: './src/components/ThemeToggle.astro',
        // The navigation rail. src/styles/ptah/sidebar.css styles this markup
        // and nothing else, so without the override the rail keeps Starlight's
        // own look while the rest of the page wears the design.
        Sidebar: './src/components/Sidebar.astro',
        // The meta row and the previous / next cards. furniture.css styles this
        // markup; Starlight's own footer would wear none of it.
        Footer: './src/components/Footer.astro',
        // Upstream's content wrapper, plus the FAQ's filter. The FAQ cannot
        // be MDX -- its anchors are `{#id}`, which MDX reads as an
        // expression -- so the component comes to the page.
        MarkdownContent: './src/components/MarkdownContent.astro',
        // The contents rail, with the FAQ's word for what it lists, and below
        // the rail's breakpoint the strip that replaces it.
        TableOfContents: './src/components/TableOfContents.astro',
        MobileTableOfContents: './src/components/MobileTableOfContents.astro',
      },
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
  vite: {
    plugins: [tailwindcss()],
    server: {
      // The recorded runs live in demo/, outside this site's root, because the
      // recorder writes them and the repository's own build reads them. A
      // second copy under src/ is the thing that would go stale.
      fs: { allow: ['../..'] },
    },
  },
});
