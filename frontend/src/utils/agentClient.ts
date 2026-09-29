// frontend/src/utils/agentClient.ts
//
// Frontend mirror of the gateway recognizer's client enum
// (internal/gateway/agent_client.go): the complete, closed set of tool
// names that can be written into request_logs.agent_client and therefore
// offered as the "client tool" filter's options (exact-match values on the
// wire). Kept in one place so the filter dropdown and the detail page's
// display-name lookup cannot drift apart; the mounted component test pins
// the enumeration against this list.
//
// Adding a tool to the recognizer means adding one entry here plus the
// paired i18n keys in both locales (parity.test.ts enforces the pairing).

/** Every client name the recognizer can produce, filter-option order. */
export const AGENT_CLIENTS = [
  'claude-code',
  'codex',
  'opencode',
  'gemini-cli',
  'qwen-code',
  'crush',
  'codewhale',
  'cherry-studio',
  'pi',
] as const

export type AgentClient = (typeof AGENT_CLIENTS)[number]

// requestLogs-namespace i18n key per client. The labels are proper nouns
// (identical in zh-CN and en) but still go through t(): hardcoded
// user-visible copy is banned by the frontend conventions.
const AGENT_CLIENT_LABEL_KEYS: Record<AgentClient, string> = {
  'claude-code': 'requestLogs.agentClientClaudeCode',
  'codex': 'requestLogs.agentClientCodex',
  'opencode': 'requestLogs.agentClientOpencode',
  'gemini-cli': 'requestLogs.agentClientGeminiCli',
  'qwen-code': 'requestLogs.agentClientQwenCode',
  'crush': 'requestLogs.agentClientCrush',
  'codewhale': 'requestLogs.agentClientCodewhale',
  'cherry-studio': 'requestLogs.agentClientCherryStudio',
  'pi': 'requestLogs.agentClientPi',
}

// The i18n key for a client's display name. The AgentClient overload is
// total (every enum member has a key); the string overload returns null
// for a value outside the enum (a newer gateway's client, or a hand-edited
// row) so callers can fall back to rendering the raw value — an unknown
// tool name is still the truth about the row.
export function agentClientLabelKey(client: AgentClient): string
export function agentClientLabelKey(client: string): string | null
export function agentClientLabelKey(client: string): string | null {
  return (AGENT_CLIENTS as readonly string[]).includes(client)
    ? AGENT_CLIENT_LABEL_KEYS[client as AgentClient]
    : null
}
