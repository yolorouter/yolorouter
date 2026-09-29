export default {
  eyebrow: '统计分析',
  pageTitle: '工具会话',
  pageDescription: '按调用方工具的任务会话聚合浏览请求：一个会话一行，回答"这个会话干了多少活、花了多少钱"。',
  // Page-top support-scope note: the session view only ever collects
  // requests that carried a session header — three tools today.
  scopeNote: '会话视图当前支持 Claude Code / Codex / OpenCode（携带会话头的请求才会进入会话）。',
  // The pointer sentence is split around the linked menu name so the link
  // itself stays the localized nav.logAudit copy (the menu name) rather
  // than a duplicated string.
  pointerPre: '不带会话头的工具不会在这里出现，请到「',
  pointerPost: '」页用"客户端工具"过滤查看。',
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
  detailDescription: '该会话的全部请求按时间先后排列，点击任一行进入请求详情。',
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
}
