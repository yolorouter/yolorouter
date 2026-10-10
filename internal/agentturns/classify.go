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
// wrappers, and prefixes its local-mode caveat with "Caveat:". These are
// the shapes observed in the wild on the 2026-10 replay fixtures; anything
// not matching stays user_input. A table rather than a parser: the arm is
// deliberately conservative (recognize only what is certain) and cheap to
// extend when a new wrapper shows up.
var injectedContentPrefixes = []string{
	"<system-reminder",
	"<command-name",
	"<command-message",
	"Caveat:",
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
// quoted content, not an injection.
func classifyTextContent(text string) string {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	for _, prefix := range injectedContentPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return EventInjected
		}
	}
	return EventUserInput
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
