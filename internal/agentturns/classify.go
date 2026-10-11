// Message classification for the turn store's event taxonomy — the
// write-time half of the conversation model. The replay pass keeps every
// non-assistant, non-system message of a request's new suffix in one
// undifferentiated `added` array (that is the transcript's wire contract);
// this file splits those same messages into the finer event types the
// persisted turns carry, so a reader can tell a human's input from a tool
// result or an injected system reminder without re-parsing anything.
//
// The classification has one hard contract and one soft one. Hard: it is
// TOTAL and LOSSLESS — every message AssembleAgentSessionReplay would keep
// in `added` lands in exactly one of user_input / tool_result / injected,
// and concatenating those three event rows' contents in order reproduces
// the `added` array exactly (the parity tests pin this). Soft: the split
// between "human typed this" and "a machine injected this" is heuristic —
// the marker table below is deliberately conservative and extendable; a
// message the table does not recognize stays user_input, which is the
// safe arm (nothing is ever dropped or double-counted by a wrong guess).
//
// Two refinements sharpen the human/machine split at the BLOCK level,
// because real caller messages are multi-block: an IDE-coupled client
// wraps the editor's context (selections, opened files) and its system
// reminders as sibling text blocks INSIDE the user's own message. The
// splitter below (SplitUserMessageForAdded) judges each text block on its
// own, so the machine blocks become injected events while the human's
// text stays user_input; and the duplicate-input folder (userInputFolder)
// drops a user_input whose real text is identical to the previous one's —
// the resent copy an interrupted client produces, folded the way mature
// conversation products fold repeated submits (an empty real text never
// folds: see userInputFolder).
package agentturns

import "strings"

// Event type constants — the agent_session_turns.event_type vocabulary.
// The first six are the assembly products; pending is the degraded marker
// for a request whose turn batch could not be written (see store.go).
const (
	EventSystemPrompt   = "system_prompt"
	EventUserInput      = "user_input"
	EventToolCall       = "tool_call"
	EventToolResult     = "tool_result"
	EventInjected       = "injected"
	EventAssistantReply = "assistant_reply"
	EventPending        = "pending"
)

// injectedContentPrefixes lists the leading markers of machine-injected
// context in caller-side user messages. Claude Code wraps its system
// reminders and skill listings in <system-reminder>…</system-reminder>
// blocks, echoes slash commands back as <command-name>/<command-message>
// wrappers, and prefixes its local-mode caveat with "Caveat:"; an
// IDE-coupled client wraps the editor's context in <ide_selection> /
// <ide_opened_file> blocks (matched by the bare "<ide_" stem — every
// observed wrapper shares it, and no human text leads with it). These are
// the shapes observed in the wild on the 2026-10 replay fixtures; anything
// not matching stays user_input. A table rather than a parser: the arm is
// deliberately conservative (recognize only what is certain) and cheap to
// extend when a new wrapper shows up.
var injectedContentPrefixes = []string{
	"<system-reminder",
	"<command-name",
	"<command-message",
	"Caveat:",
	"<ide_",
}

// ClassifyAddedMessage types one message of a request's new suffix for the
// turn store. Input is the parsed message object (exactly the element
// AssembleAgentSessionReplay's added filter kept); output is one of
// EventUserInput, EventToolResult, EventInjected. See the file header for
// the totality/losslessness contract and the heuristics' scope.
func ClassifyAddedMessage(m any) string {
	obj, ok := m.(map[string]any)
	if !ok {
		// A non-object element survived the added filter (it is neither
		// assistant nor system role); it stays on the conversation axis as
		// user input rather than being dropped.
		return EventUserInput
	}
	role, _ := obj["role"].(string)
	switch role {
	case "tool":
		// The OpenAI dialect's tool-result messages carry role "tool".
		return EventToolResult
	case "user":
		return classifyUserMessage(obj["content"])
	default:
		// No role or an unrecognized one: the catch-all keeps the
		// classification total.
		return EventUserInput
	}
}

// classifyUserMessage types a user-role message by its content shape:
// content blocks carrying tool_result are a tool-result turn; otherwise
// text content that begins with a machine-injection marker is injected;
// everything else is human input. Anthropic's dialect puts tool results in
// user-role messages as content blocks ({"type":"tool_result",...}), which
// is why the tool_result arm runs before the text inspection.
func classifyUserMessage(content any) string {
	switch c := content.(type) {
	case []any:
		hasToolResult := false
		texts := make([]string, 0, len(c))
		for _, block := range c {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := b["type"].(string); t == "tool_result" {
				hasToolResult = true
			}
			if t, _ := b["type"].(string); t == "text" {
				if text, ok := b["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
		if hasToolResult {
			return EventToolResult
		}
		return classifyTextContent(strings.Join(texts, "\n"))
	case string:
		return classifyTextContent(c)
	default:
		// Null / structured-but-unknown content: keep it on the axis.
		return EventUserInput
	}
}

// classifyTextContent applies the injected-marker table to one user
// message's text. The marker must lead the (trimmed) text: these wrappers
// are whole-message wrappers, and a marker appearing mid-text is ordinary
// quoted content, not an injection. The caller passes DECODED text — the
// parsed message's string, never the raw body JSON where "<" survives as
// the \u003c escape — so the table matches regardless of how the body was
// serialized on the wire.
func classifyTextContent(text string) string {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	for _, prefix := range injectedContentPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return EventInjected
		}
	}
	return EventUserInput
}

// SplitUserMessageForAdded judges one caller-side message's text blocks
// INDIVIDUALLY and returns the added elements they decompose into (the
// write path stores one turn event per element; the pass lists the same
// elements in `added`). The shapes the block-level view cannot improve —
// a non-user role, a tool-result-bearing body, string or null content, a
// non-object element — come back as the ORIGINAL message, one element,
// exactly as the pre-split classification treated them. A user message
// whose content is a block array without tool results is walked block by
// block: each text block leading with an injection marker becomes its own
// single-block element (classified injected), and ALL remaining blocks
// (non-injected text, images, unknown types) merge into ONE trailing
// element (classified user_input) — the human sent one message, so the
// axis shows one bubble, machine context extracted around it. Nothing is
// dropped and no block is duplicated; a purely injected message therefore
// yields injected elements and NO user_input element, and a message with
// no marker-led block yields exactly the original object.
func SplitUserMessageForAdded(m any) []any {
	obj, ok := m.(map[string]any)
	if !ok {
		return []any{m}
	}
	role, _ := obj["role"].(string)
	if role != "user" {
		return []any{m}
	}
	blocks, ok := obj["content"].([]any)
	if !ok {
		return []any{m}
	}
	for _, block := range blocks {
		b, ok := block.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := b["type"].(string); t == "tool_result" {
			// The tool-result shape wins over the text inspection (same
			// priority as classifyUserMessage); it is never split.
			return []any{m}
		}
	}
	var injected []any
	var kept []any
	split := false
	for _, block := range blocks {
		if b, ok := block.(map[string]any); ok {
			if t, _ := b["type"].(string); t == "text" {
				if text, ok := b["text"].(string); ok && classifyTextContent(text) == EventInjected {
					injected = append(injected, map[string]any{"role": role, "content": []any{block}})
					split = true
					continue
				}
			}
		}
		kept = append(kept, block)
	}
	if !split {
		// Nothing was extracted: hand the original object back untouched —
		// byte-identical content for every message shape the marker table
		// does not name, so the split changes nothing it does not fix.
		return []any{m}
	}
	out := injected
	if len(kept) > 0 {
		out = append(out, map[string]any{"role": role, "content": kept})
	}
	return out
}

// UserInputRealText extracts the human text of one user_input element —
// string content verbatim, block arrays reduced to their text blocks
// joined by newlines (the same extraction classifyUserMessage runs for
// classification). This is the identity the duplicate-input folder
// compares: an interrupted client resends the same words with fresh
// machine context around them, and the words are what "the same input"
// means.
func UserInputRealText(m any) string {
	obj, ok := m.(map[string]any)
	if !ok {
		return ""
	}
	switch c := obj["content"].(type) {
	case string:
		return c
	case []any:
		texts := make([]string, 0, len(c))
		for _, block := range c {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := b["type"].(string); t == "text" {
				if text, ok := b["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
		return strings.Join(texts, "\n")
	default:
		return ""
	}
}

// userInputFolder is the session-wide duplicate-input state: the real text
// of the last user_input element that was KEPT. A user_input whose real
// text equals it is the resent copy an interrupted client produced (the
// retry re-sends the same words with refreshed machine context, which the
// prefix diff cannot see as a repeat) and is folded — first kept, resent
// copy dropped. Non-user_input elements pass through untouched, and a
// folded element does not advance the state (it equals it by definition).
// The deliberately folded corner: a human genuinely repeating their last
// sentence word for word also collapses; mature conversation products fold
// repeated submits the same way, and a repeat that differs in any byte
// still lands.
//
// The one key that never folds: EMPTY real text. A no-text element (a pure
// image, null content) extracts "" — and "" is shared by every such
// message, so treating it as "the same words" would collide two DIFFERENT
// inputs and drop the second off the axis (the write path reads its
// baseline from the last user_input row's real text, so an empty-keyed
// row would then eat every later pure-image message too). Empty therefore
// keeps — and still ADVANCES the state to "", keeping the pass's folder
// and the stored-row baseline in lockstep; the next non-empty input
// simply does not fold against it.
type userInputFolder struct {
	last    string
	hasLast bool
}

// keep reports whether one added element survives the fold, advancing the
// state when it does.
func (f *userInputFolder) keep(el any) bool {
	if ClassifyAddedMessage(el) != EventUserInput {
		return true
	}
	text := UserInputRealText(el)
	// An empty key is no same-text evidence: fold only identical NON-EMPTY
	// real texts, never two elements that both happen to carry no words.
	if f.hasLast && text != "" && text == f.last {
		return false
	}
	f.last, f.hasLast = text, true
	return true
}

// MessageCarriesToolUse reports whether an assistant-role message's content
// contains at least one tool_use block — the test that decides whether a
// resent assistant copy becomes a tool_call event row beside the resend
// attribution. Both dialects name the block type "tool_use" (Anthropic
// content blocks; OpenAI function/tool calls arrive pre-normalized by the
// caller or as plain text, in which case there is nothing to mark).
func MessageCarriesToolUse(m any) bool {
	obj, ok := m.(map[string]any)
	if !ok {
		return false
	}
	blocks, ok := obj["content"].([]any)
	if !ok {
		return false
	}
	for _, block := range blocks {
		b, ok := block.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := b["type"].(string); t == "tool_use" {
			return true
		}
	}
	return false
}
