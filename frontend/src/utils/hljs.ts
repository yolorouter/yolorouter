// frontend/src/utils/hljs.ts
//
// The single highlight.js instance for NCode in this app: core build plus
// only the languages the API examples actually use (bash for curl, python,
// javascript for Node, go, json for response bodies), keeping the bundle at
// a sliver of the full library. highlight.js itself arrives as naive-ui's
// dependency — NCode takes the instance via its `hljs` prop and does no
// highlighting without one.
import hljs from 'highlight.js/lib/core'
import bash from 'highlight.js/lib/languages/bash'
import go from 'highlight.js/lib/languages/go'
import javascript from 'highlight.js/lib/languages/javascript'
import json from 'highlight.js/lib/languages/json'
import python from 'highlight.js/lib/languages/python'

hljs.registerLanguage('bash', bash)
hljs.registerLanguage('go', go)
hljs.registerLanguage('javascript', javascript)
hljs.registerLanguage('json', json)
hljs.registerLanguage('python', python)

export default hljs
