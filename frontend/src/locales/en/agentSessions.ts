export default {
  eyebrow: 'Analytics',
  pageTitle: 'Tool Sessions',
  pageDescription: 'Browse requests aggregated by the calling tool\'s task session: one row per session — how much work it did and what it cost.',
  // Page-top support-scope note: the session view only ever collects
  // requests that carried a session header — three tools today.
  scopeNote: 'The session view currently supports Claude Code / Codex / OpenCode (only requests carrying a session header appear here).',
  // The pointer sentence is split around the linked menu name so the link
  // itself stays the localized nav.logAudit copy (the menu name) rather
  // than a duplicated string.
  pointerPre: 'Tools that send no session header never show up here — open the "',
  pointerPost: '" page and filter by Client Tool instead.',
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

  // Detail page
  detailEyebrow: 'Analytics',
  detailTitle: 'Session Detail',
  detailDescription: 'Every request of this session in chronological order; click any row to expand it in place without leaving the session.',
  backToList: 'Back to List',
  notFound: 'Session not found or its request logs have been cleaned up',
  detailRequestsUnit: 'requests',
  col_created: 'Time',
  col_created_tip: 'Request start time, shown in the browser timezone',
  col_model: 'Model',
  col_model_tip: 'The public model name the caller asked for',
  col_status: 'Status',
  col_status_tip: 'Final status bucket of the request: success, failed, partial, cancelled, rejected',
  detail_col_tokens: 'Tokens',
  detail_col_tokens_tip: 'The request\'s input and output token counts',
  col_duration: 'Duration',
  col_duration_tip: 'End-to-end duration from request received to response completed',
  col_waterfall: 'Timeline',
  col_waterfall_tip: 'Each request\'s duration bar on the session\'s real time axis: position aligns with the start time, length with the duration, colour follows the status bucket; hover for the precise start time and duration',
  waterfallTip: 'started {time} · took {duration}',

  // Summary card (figures recomputed from the detail's own requests array,
  // same semantics as the list aggregate)
  summaryRequests: 'Requests',
  summaryRequests_tip: 'Total requests in this session and how many of them were successes (the success bucket of the five-class status taxonomy), same semantics as the list page',
  summaryTokens: 'Total Tokens',
  summaryTokens_tip: 'Sum of input and output tokens across the session\'s requests',
  summaryCost: 'Cost',
  summaryCost_tip: 'Sum of the session\'s known costs (CNY); requests that could not be priced are disclosed separately, never folded into the total',
  summaryDuration: 'Session Duration',
  summaryDuration_tip: 'Time between the session\'s earliest and latest requests',

  // Request drawer (the in-place inspector opened by a row click)
  drawerViewFull: 'View full details',
  drawerLoadFailed: 'Failed to load the request detail',
  drawerStreamMergedNote: 'streamed reply, merged from the sent chunks',

  // Message flow (bubble view of the captured bodies)
  flowTruncated: 'Content too long, truncated',
  flowFallbackTitle: 'Raw body',
  partImage: 'Image',
}
