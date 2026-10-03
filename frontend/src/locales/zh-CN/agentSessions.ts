export default {
  eyebrow: '统计分析',
  pageTitle: '工具会话',
  pageDescription:
    '按工具自带的会话标识聚合 Claude Code / Codex / OpenCode 的请求；会话标识来自实时请求头捕获或启动回填，其余流量请到日志审计按客户端工具查。',

  listEmpty: '没有工具会话',

  // Filters
  filterAgentClient: '客户端工具',
  allFilterAgentClient: '全部客户端工具',

  // Columns
  col_tool: '工具',
  col_tool_tip: '会话归属的调用方工具（按会话内最早一行判定）',
  col_session: '会话标识',
  col_session_tip: '调用方工具自定义的会话编号（agent_session_id）',
  col_timeRange: '起止时间',
  col_timeRange_tip: '会话内最早与最晚一次请求的时间',
  firstSeenLabel: '起',
  lastSeenLabel: '止',
  col_requests: '请求数',
  col_requests_tip: '该会话的请求总数及其中成功（五分类中的 success）的数量',
  successCountLine: '成功 {n}',
  col_tokens: '总 Token',
  col_tokens_tip: '会话内全部请求的输入与输出 token 之和',
  col_cost: '成本',
  col_cost_tip: '会话内已知成本之和（人民币元）；无法定价的请求单列披露，不计入合计',
  costUnknownNote: '含 {n} 条成本未知',

  // Detail page
  detailEyebrow: '统计分析',
  detailTitle: '会话详情',
  detailDescription: '该会话的全部请求按时间先后排列；点击任一行整页打开该请求的消息页，查看捕获的完整对话。',
  backToList: '返回列表',
  notFound: '会话不存在或请求日志已被清理',
  detailRequestsUnit: '条请求',
  col_created: '时间',
  col_created_tip: '请求开始时间，按浏览器时区显示',
  col_model: '模型',
  col_model_tip: '调用方请求的对外模型名',
  col_status: '状态',
  col_status_tip: '请求的最终结果分类：成功、失败、部分成功、调用方取消、被拒绝',
  detail_col_tokens: 'Token',
  detail_col_tokens_tip: '该请求的输入与输出 token 数',
  col_duration: '耗时',
  col_duration_tip: '该请求从接收到响应完成的端到端耗时',
  col_waterfall: '时间线',
  col_waterfall_tip: '每条请求在会话真实时间轴上的耗时条：位置对齐开始时间，长度对应耗时，颜色同状态分类；悬停查看精确开始时间与耗时',
  waterfallTip: '{time} 开始 · 耗时 {duration}',

  // Summary card (figures recomputed from the detail's own requests array,
  // same semantics as the list aggregate)
  summaryRequests: '请求数',
  summaryRequests_tip: '该会话的请求总数及其中成功（五分类中的 success）的数量，口径与列表页一致',
  summaryTokens: '总 Token',
  summaryTokens_tip: '会话内全部请求的输入与输出 token 之和',
  summaryCost: '成本',
  summaryCost_tip: '会话内已知成本之和（人民币元）；无法定价的请求单列披露，不计入合计',
  summaryDuration: '会话时长',
  summaryDuration_tip: '会话内最晚一次请求与最早一次请求的时间差',

  // (The request drawer's copy moved to the requestMessages namespace when
  // the drawer was replaced by the full-page request message view.)

  // Message flow (bubble view of the captured bodies)
  flowTruncated: '内容过长已截断',
  flowFallbackTitle: '原始正文',
  partImage: '图片',
}
