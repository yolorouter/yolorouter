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

  // Detail page (the session replay)
  detailEyebrow: '统计分析',
  detailTitle: '会话回放',
  detailDescription: '整个会话的连续对话回放：按时间先后去重展示每轮新增的输入与模型回复，轮次上直接标注模型、耗时与消耗。',
  backToList: '返回列表',
  notFound: '会话不存在或请求日志已被清理',
  loadFailed: '加载会话回放失败',
  detailRequestsUnit: '条请求',
  replayElapsed: '历时 {span}',
  replaySuccessRatio: '成功 {success}/{total}',
  replayRequestLink: '请求详情',
  systemBlockTitle: '系统提示',
  systemSeenAt: '首次出现于请求 {id}',
  systemGroupChanges: '系统提示·首次出现+变化 {n} 次',
  systemGroupFirstOnly: '系统提示·首次出现',

  // Message flow (bubble view of the captured bodies). The disclosure-row
  // and stream-merge keys below migrated here from the retired request
  // message page's own namespace when that page was removed — the bubble
  // renderer outlived it.
  flowTruncated: '内容过长已截断',
  flowFallbackTitle: '原始正文',
  partImage: '图片',
  expandFull: '展开全部',
  collapse: '收起',
  partCharsNote: '{n} 字符',
  streamMergedNote: '流式响应，已按发送片段合并',
}
