import { defineCollection } from 'astro:content';
import { z } from 'astro/zod';
import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';

// One collection, because the site publishes one language. Starlight also
// looks for an `i18n` collection and says so on every build; declaring an
// empty one replaces that notice with two others, so the notice stays.
//
// Two keys beyond Starlight's own, both for the FAQ: the words a reader
// searches for when they have the failure rather than the vocabulary, and the
// flag that draws the word an answer commits to as a tag
// (src/lib/markdown-faq-verdict.mjs).
const pageExtras = z.object({
  searchAliases: z.array(z.string().min(1)).optional(),
  faqVerdicts: z.boolean().optional(),
});

export const collections = {
  docs: defineCollection({ loader: docsLoader(), schema: docsSchema({ extend: pageExtras }) }),
};
