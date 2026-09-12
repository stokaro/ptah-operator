import { defineCollection } from 'astro:content';
import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';

// One collection, because the site publishes one language. Starlight also
// looks for an `i18n` collection and says so on every build; declaring an
// empty one replaces that notice with two others, so the notice stays.
export const collections = {
  docs: defineCollection({ loader: docsLoader(), schema: docsSchema() }),
};
