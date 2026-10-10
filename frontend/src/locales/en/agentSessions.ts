export default {
  eyebrow: 'Analytics',
  pageTitle: 'Tool Sessions',
  pageDescription:
    "Requests grouped by the calling tool's own session identifier — Claude Code / Codex / OpenCode; identifiers arrive by live session-header capture or the startup backfill; for other traffic use Log Audit's client-tool filter.",

  listEmpty: 'No tool sessions',

  // Filters
  filterAgentClient: 'Client Tool',
  allFilterAgentClient: 'All Client Tools',

  // Columns
  col_tool: 'Tool',
  col_tool_tip: 'The calling tool this session is attributed to (decided by the session\'s earliest row)',
  col_session: 'Session ID',
  col_session_tip: 'The calling tool\'s own session identifier (agent_session_id)',
  col_timeRange: 'Time Range',
  col_timeRange_tip: 'Earliest and latest request timestamps inside the session',
  firstSeenLabel: 'From',
  lastSeenLabel: 'To',
  col_requests: 'Requests',
  col_requests_tip: 'Total requests in this session and how many of them were successes (the success bucket of the five-class status taxonomy)',
  successCountLine: '{n} succeeded',
  col_tokens: 'Total Tokens',
  col_tokens_tip: 'Sum of input and output tokens across the session\'s requests',
  col_cost: 'Cost',
  col_cost_tip: 'Sum of the session\'s known costs (CNY); requests that could not be priced are disclosed separately, never folded into the total',
  costUnknownNote: 'incl. {n} of unknown cost',

  // Detail page (the session replay)
  detailEyebrow: 'Analytics',
  detailTitle: 'Session Replay',
  detailDescription: "The whole session as one continuous conversation: each turn's newly-added input and the model's reply in chronological order, deduplicated, with the model, duration, and usage labelled on the turn.",
  backToList: 'Back to List',
  notFound: 'Session not found or its request logs have been cleaned up',
  loadFailed: 'Failed to load the session replay',
  detailRequestsUnit: 'requests',
  replayElapsed: '{span} elapsed',
  replaySuccessRatio: '{success}/{total} succeeded',
  replayRequestLink: 'Request Details',
  systemBlockTitle: 'System Prompt',
  systemSeenAt: 'first seen on request {id}',
  systemGroupChanges: 'System Prompt · first seen + {n} changes',
  systemGroupFirstOnly: 'System Prompt · first seen',

  // Message flow (bubble view of the captured bodies). The disclosure-row
  // and stream-merge keys below migrated here from the retired request
  // message page's own namespace when that page was removed — the bubble
  // renderer outlived it.
  flowTruncated: 'Content too long, truncated',
  flowFallbackTitle: 'Raw body',
  partImage: 'Image',
  expandFull: 'Show all',
  collapse: 'Collapse',
  partCharsNote: '{n} characters',
  streamMergedNote: 'streamed reply, merged from the sent chunks',
}
