// @vitest-environment jsdom
//
// frontend/src/utils/markdown.test.ts
//
// Table-driven unit matrix for the bubble markdown renderer (pure function:
// untrusted text → sanitized HTML), pinning acceptance case TC-01:
//
//   - structure     headings / bullet list / table / fenced code block
//                   with the language class and highlight.js spans
//   - degenerate    empty string → empty output; plain text → exactly a
//                   <p> wrap whose textContent equals the input verbatim
//   - malformed     unclosed code fence, table missing its separator row,
//                   hash/asterisk-only text — no crash, no fence markers
//                   or half-parsed table markup leaking through
//   - malicious     <script>, onerror attribute, javascript: links (both
//                   the markdown and the raw-HTML spelling) never survive
//                   as live markup; a script inside a code fence stays
//                   escaped display text
//
// The environment is jsdom, not the happy-dom the component tests use:
// DOMPurify reads node names through a getter captured from
// Node.prototype, and happy-dom's base nodeName getter returns '' for
// elements (the real name comes from getters further down the chain), so
// the allowlist walk strips legitimate tags and lets scripts slip through
// hoisted content. jsdom resolves nodeName the way browsers do, keeping
// the sanitizer on its supported path — the production code stays the
// canonical DOMPurify.sanitize(string).
import { describe, expect, it } from 'vitest'
import { renderMarkdown } from './markdown'

/** Parse an HTML fragment back through a detached element — the same
 *  contract v-html has at runtime. */
function textContentOf(html: string): string {
  const div = document.createElement('div')
  div.innerHTML = html
  return div.textContent ?? ''
}

describe('renderMarkdown (TC-01 unit matrix)', () => {
  // -------------------------------------------------------------------------
  // Structure rendering
  // -------------------------------------------------------------------------
  describe('structure', () => {
    it.each<[name: string, input: string, expected: string[]]>([
      ['ATX heading', '# Title', ['<h1>Title</h1>']],
      ['bullet list', '- one\n- two', ['<ul>', '<li>one</li>', '<li>two</li>']],
      ['GFM table', '| a | b |\n| --- | --- |\n| 1 | 2 |', ['<table>', '<th>a</th>', '<td>2</td>']],
      [
        'fenced code block: language class + highlight.js spans',
        '```go\nfunc main() {}\n```',
        ['<pre><code class="language-go">', 'hljs-keyword', '>func</span>'],
      ],
      [
        'unknown fence language falls back to escaped plain code',
        '```notalang\n<script>x</script>\n```',
        ['class="language-notalang"', '&lt;script&gt;'],
      ],
    ])('%s', (_name, input, expectedParts) => {
      const html = renderMarkdown(input)
      for (const part of expectedParts) expect(html).toContain(part)
    })
  })

  // -------------------------------------------------------------------------
  // Degenerate inputs: empty and plain text
  // -------------------------------------------------------------------------
  describe('degenerate input', () => {
    it('empty string renders to empty output', () => {
      expect(renderMarkdown('')).toBe('')
    })

    it.each<[name: string, input: string]>([
      ['plain ASCII text', 'hello, plain world 123'],
      // Entity-bearing text: the output wraps < > & quotes in entities, so
      // this row pins that textContent still round-trips byte for byte.
      ['plain text with markup-shaped characters', '5 < 6 & "quotes" \'single\''],
    ])('%s is exactly one <p> wrap with verbatim textContent', (_name, input) => {
      const html = renderMarkdown(input)
      // Exactly a <p> wrap: one paragraph, nothing before or after (the
      // renderer's trailing newline aside).
      expect(html.startsWith('<p>')).toBe(true)
      expect(html.trimEnd().endsWith('</p>')).toBe(true)
      expect(html.match(/<p>/g)?.length).toBe(1)
      expect(textContentOf(html)).toBe(input)
    })
  })

  // -------------------------------------------------------------------------
  // Malformed markdown: must not crash and must not leak raw markers
  // -------------------------------------------------------------------------
  describe('malformed markdown', () => {
    it('unclosed code fence renders to end of input with no fence markers left', () => {
      const html = renderMarkdown('```js\nconst x = 1')
      expect(html).toContain('<pre><code class="language-js">')
      expect(html).toContain('hljs-keyword')
      expect(html).not.toContain('```')
    })

    it('table missing its separator row degrades to plain text, not half-table markup', () => {
      const html = renderMarkdown('| a | b |\n| 1 | 2 |')
      expect(html).not.toContain('<table')
      expect(html).not.toContain('<th')
      expect(html).not.toContain('<td')
      expect(html).toContain('<p>| a | b |')
    })

    it('asterisk-only line is a thematic break — asterisks consumed', () => {
      expect(renderMarkdown('***').trim()).toBe('<hr>')
    })

    it('hash-prefixed asterisks render as a heading with no stray markup', () => {
      expect(renderMarkdown('# ***').trim()).toBe('<h1>***</h1>')
    })
  })

  // -------------------------------------------------------------------------
  // Malicious content: the sanitizer is the boundary
  // -------------------------------------------------------------------------
  describe('malicious content is sanitized away', () => {
    it.each<[name: string, input: string, mustNotContain: string[]]>([
      ['<script> element and its payload are removed', 'hello <script>alert(1)</script> world', ['<script', 'alert(1)']],
      ['onerror attribute is stripped, the img itself may stay', '<img src="x" onerror="alert(2)">', ['onerror', 'alert(2)']],
      // markdown-it's own link validator refuses javascript: destinations,
      // so the markdown spelling degrades to visible text with no anchor
      // and no href anywhere in the output
      ['markdown javascript: link never becomes an anchor', '[click me](javascript:alert(3))', ['href']],
      ['raw-HTML javascript: link loses its href', '<a href="javascript:alert(4)">raw</a>', ['href', 'javascript:']],
      // FORBID_TAGS removes the whole subtree: the <style> element AND its
      // text content go together (verified output for this row is exactly
      // "styled" — only the text outside the element survives).
      ['style element is dropped together with its text content', '<style>body{display:none}</style>styled', ['<style', 'display:none']],
    ])('%s', (_name, input, mustNotContain) => {
      const html = renderMarkdown(input)
      for (const banned of mustNotContain) expect(html).not.toContain(banned)
    })

    it('a script tag inside a code fence stays escaped display text', () => {
      const html = renderMarkdown('```\n<script>alert(5)</script>\n```')
      // Shown as code (escaped), never executable markup.
      expect(html).toContain('&lt;script&gt;')
      expect(html).not.toContain('<script')
    })
  })
})
