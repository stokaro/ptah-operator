/**
 * Renders ```mermaid fences as diagrams instead of as source.
 *
 * A Sätteri mdast plugin, registered on `markdown.processor` in
 * astro.config.mjs beside the other two.
 *
 * It runs on the Markdown tree rather than on the HTML one so the diagram
 * never reaches the syntax highlighter: a fence that has been highlighted is a
 * tree of coloured spans, and recovering the author's source from it to hand to
 * Mermaid would be guesswork. Here the source is still one string on the node.
 *
 * The fence stays a fence in the file because GitHub renders it natively, and
 * this document is read there as often as on the site. What the site needs is
 * the element Mermaid looks for, which is what this writes: the same source,
 * escaped, inside `<pre class="mermaid">`. MarkdownContent.astro loads the
 * runtime only for pages that contain one.
 */

const ESCAPES = new Map([
  ['&', '&amp;'],
  ['<', '&lt;'],
  ['>', '&gt;'],
]);

// The source is spliced into HTML, so the three characters that would end the
// element early are escaped. Mermaid reads the text content, which the browser
// unescapes back to exactly what the author wrote.
function escapeDiagram(source) {
  return source.replace(/[&<>]/g, (character) => ESCAPES.get(character));
}

export default function markdownMermaid() {
  return {
    name: 'ptah-mermaid',
    code(node, ctx) {
      if (node.lang !== 'mermaid') return;
      if (typeof node.value !== 'string' || node.value.trim() === '') return;

      ctx.replaceNode(node, {
        raw: `<pre class="mermaid" data-diagram>${escapeDiagram(node.value)}</pre>`,
        mdxExpressions: false,
      });
    },
  };
}
