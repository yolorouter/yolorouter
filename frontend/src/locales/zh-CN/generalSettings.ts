export default {
  pageDescription: '对全部用户生效的实例级设置。',
  retention: {
    title: '请求日志保留',
    desc: '请求日志（记录、请求体与流式文件）的保留期限，超期数据由后台任务小批量渐进删除。',
    days: '保留天数',
    daysTip:
      '0 表示永久保留。设为 N 天则删除超过 N 天的日志，每轮限量删除——下一轮即生效，无需重启服务。',
    daysRequired: '请输入保留天数（0 表示永久保留）',
    daysWholeNumber: '保留天数必须是整数天',
    daysMin: '保留天数不能为负数——永久保留请填 0',
    daysMax: '保留天数不能大于 3650 天（约十年）',
    saved: '已保存',
    loadFailed: '加载当前设置失败',
    retry: '重试',
    conflict: '该设置已被他人修改，已重新加载最新版本，请确认后再次保存。',
  },
  auditRetention: {
    title: '管理审计保留',
    desc: '管理审计记录（控制台变更留痕）的保留期限，超期数据由后台任务小批量渐进删除。与请求日志保留相互独立。',
    days: '保留天数',
    daysTip:
      '0 表示永久保留。设为 N 天则删除超过 N 天的审计记录，每轮限量删除——下一轮即生效，无需重启服务。',
    daysRequired: '请输入保留天数（0 表示永久保留）',
    daysWholeNumber: '保留天数必须是整数天',
    daysMin: '保留天数不能为负数——永久保留请填 0',
    daysMax: '保留天数不能大于 3650 天（约十年）',
    saved: '已保存',
    loadFailed: '加载当前设置失败',
    retry: '重试',
    conflict: '该设置已被他人修改，已重新加载最新版本，请确认后再次保存。',
  },
  outboundProxy: {
    title: '出站代理',
    desc: '上游出站流量的全局默认代理。供应商可在各自的编辑弹窗中逐个覆盖；优先级：供应商显式设置 > 全局 > 直连。默认关闭，升级不改变任何行为。',
    mode: '模式',
    modeTip: 'off=全部直连（默认）；system=跟随进程环境变量；custom=走下方代理地址。保存后约 30 秒内生效，无需重启服务。',
    modeOff: '直连（关闭）',
    modeSystem: '跟随进程环境变量',
    modeCustom: '自定义代理',
    systemNotice:
      'system 模式读取进程环境变量 HTTP_PROXY / HTTPS_PROXY / NO_PROXY。环境变量属于进程级：容器编排中改动后需重启容器才会生效。',
    url: '代理地址',
    urlTip:
      'custom 模式使用的代理 URL，支持 http、https、socks5、socks5h（socks5h 与 socks5 等价，均在代理侧解析域名）。私网与回环地址可用。',
    urlPlaceholder: 'socks5://192.168.1.5:1080',
    saved: '已保存',
    loadFailed: '加载当前设置失败',
    retry: '重试',
    conflict: '该设置已被他人修改，已重新加载最新版本，请确认后再次保存。',
    proxyUrlRequired: 'custom 模式需要填写代理地址',
    proxyUrlUnparseable: '代理地址无法解析为合法 URL',
    proxyUrlScheme: '代理地址的协议必须是 http、https、socks5 或 socks5h',
    proxyUrlNoHost: '代理地址缺少主机名',
  },
  language: {
    title: '语言',
    desc: '界面显示语言。属于保存在当前浏览器的个人偏好——不写入数据库，切换立即生效。',
    label: '界面语言',
    labelTip:
      '仅保存在当前浏览器、仅对你生效。侧边栏的「语言」入口切换的是同一份偏好，两处始终保持同步。',
  },
  capability: {
    title: '能力生效',
    desc: '按各模型声明的能力字段执行的六条转发生效规则，逐条独立开关、默认全关。能力字段（模型能力元数据）在模型编辑页维护；规则只对已声明的字段判定，未声明的字段不触发任何拦截。保存后约 30 秒内生效，无需重启服务。',
    contextLimitBlock: '上下文超限拦截',
    contextLimitBlockTip:
      '请求的估算输入超出寻址模型声明的上下文上限（加余量带）时，直接拒绝（413），不再消耗上游额度；未声明上限的模型不受影响。',
    temperatureDefault: '温度默认值',
    temperatureDefaultTip:
      '调用方未传温度且模型声明了默认温度时，转发前注入该默认值；显式传参不受影响（除非同时开启强制覆盖）。',
    temperatureForce: '强制覆盖温度',
    temperatureForceTip:
      '开启后连显式传入的温度也会被模型默认温度覆盖。默认关闭，避免意外改写调用方参数。',
    toolsStrict: '工具严格报错',
    toolsStrictTip:
      '给声明不支持工具的模型携带 tools 的请求被明确拒绝（400）；能力未声明的模型不拦截。',
    reasoningStrict: '推理严格报错',
    reasoningStrictTip:
      '只对调用方显式寻址的模型判定：声明不支持推理却携带推理参数的请求被明确拒绝（400）。路由回退落到的不支持模型不在此拦，由落点清空处理。',
    reasoningEgressClear: '推理落点清空',
    reasoningEgressClearTip:
      '转发到声明不支持推理的落点（含路由回退目标）之前，自动清空请求中的推理参数，避免上游因不认识的参数报错。',
    margin: '余量百分比',
    marginTip:
      '上下文超限拦截的判定阈值 = 声明上限 ×（1 + 余量%）。余量让估算偏差下的边界请求放行而不是误杀；默认 5，可设 0–1000。',
    marginRequired: '请输入余量百分比',
    marginWholeNumber: '余量百分比必须是整数',
    marginMin: '余量百分比不能小于 0',
    marginMax: '余量百分比不能大于 1000',
    saved: '已保存',
    loadFailed: '加载当前设置失败',
    retry: '重试',
    conflict: '该设置已被他人修改，已重新加载最新版本，请确认后再次保存。',
  },
}
