// frontend/src/utils/markdown.ts
//
// Pure-function markdown renderer for the chat bubbles: model replies (and
// system prompts, which frequently carry markdown) arrive as plain text and
// must render as safe HTML before any v-html. The pipeline is
//
//   markdown-it  — text → HTML (tables/lists/headings enabled by the
//                  default preset; html:true so model-authored inline HTML
//                  flows onward instead of being displayed as entities)
//   DOMPurify    — the security boundary. Everything markdown-it emits is
//                  sanitized before return: <script> elements and their
//                  content are dropped, event-handler attributes (onerror,
//                  onload, ...) are stripped, and javascript:/vbscript:
//                  URLs are removed (markdown-it's own link validator
//                  already refuses them for markdown links; DOMPurify
//                  covers the raw-HTML spelling)
//   highlight.js — fenced code blocks are highlighted through the app's
//                  shared core instance (utils/hljs.ts, the same five
//                  languages NCode uses). An unknown or missing language
//                  falls back to markdown-it's own escaping — never a
//                  crash and never un-highlighted raw HTML
//
// The function is deterministic and side-effect free (the MarkdownIt
// instance is configured once at module load and render() keeps no state),
// which is what makes it easy to pin down with table-driven unit tests.
import MarkdownIt from 'markdown-it'
import DOMPurify from 'dompurify'
import hljs from './hljs'

const md = new MarkdownIt({
  html: true,
  // Plain URLs stay plain text — auto-linking is not asked for and would
  // enlarge the attack surface the sanitizer has to police.
  linkify: false,
  // Newlines inside a paragraph stay soft breaks (CommonMark). Model text
  // already uses blank lines for paragraph separation.
  breaks: false,
  highlight(code, lang) {
    // Only highlight languages the shared core instance registered; for
    // anything else return '' so markdown-it applies its own escaping.
    if (lang && hljs.getLanguage(lang)) {
      try {
        return hljs.highlight(code, { language: lang }).value
      } catch {
        // A highlighter hiccup must never break rendering — fall through
        // to the escaped-plain path below.
      }
    }
    return ''
  },
})

// DOMPurify's default profile already removes scripts, event handlers and
// dangerous URL schemes; style sheets are additionally forbidden because a
// bubble has no business embedding page-level CSS (a stray <style> from a
// model reply could restyle the whole page).
const SANITIZE_CONFIG = { FORBID_TAGS: ['style'] }

/** Render untrusted markdown text to sanitized HTML (empty in → empty out).
 *  The output is trimmed to exactly the rendered fragment — plain text is
 *  precisely one <p> wrap whose textContent equals the input verbatim, with
 *  no renderer-added trailing newline. The result is safe for v-html; it is
 *  NOT safe to concatenate with unsanitized markup. */
export function renderMarkdown(text: string): string {
  if (text === '') return ''
  return DOMPurify.sanitize(md.render(text), SANITIZE_CONFIG).trim()
}
