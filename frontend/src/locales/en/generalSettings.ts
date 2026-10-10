export default {
  pageDescription: 'Instance-wide settings that apply to every user.',
  retention: {
    title: 'Request Log Retention',
    desc: 'How long request logs (rows, bodies, and stream files) are kept before the background task deletes them in small batches.',
    days: 'Retention (days)',
    daysTip:
      '0 keeps request logs forever. N days deletes logs older than N days, a few hundred rows per round — effective on the next round, no restart needed.',
    daysRequired: 'Enter a retention in days (0 keeps everything)',
    daysWholeNumber: 'The retention must be a whole number of days',
    daysMin: 'The retention cannot be negative — use 0 to keep logs forever',
    daysMax: 'The retention must be at most 3650 days (about 10 years)',
    saved: 'Saved',
    loadFailed: 'Failed to load the current setting',
    retry: 'Retry',
    conflict: 'This setting was changed elsewhere. Reloaded the latest version — review and save again.',
  },
  auditRetention: {
    title: 'Admin Audit Retention',
    desc: 'How long admin audit entries (the console change trail) are kept before the background task deletes them in small batches. Independent of request log retention.',
    days: 'Retention (days)',
    daysTip:
      '0 keeps audit entries forever. N days deletes entries older than N days, a bounded batch per round — effective on the next round, no restart needed.',
    daysRequired: 'Enter a retention in days (0 keeps everything)',
    daysWholeNumber: 'The retention must be a whole number of days',
    daysMin: 'The retention cannot be negative — use 0 to keep entries forever',
    daysMax: 'The retention must be at most 3650 days (about 10 years)',
    saved: 'Saved',
    loadFailed: 'Failed to load the current setting',
    retry: 'Retry',
    conflict: 'This setting was changed elsewhere. Reloaded the latest version — review and save again.',
  },
  outboundProxy: {
    title: 'Outbound Proxy',
    desc: 'The global default proxy for upstream traffic. Providers can override it individually in their edit dialog; priority: provider override > global > direct. Off by default — upgrading changes no behavior.',
    mode: 'Mode',
    modeTip: 'off = always direct (the default); system = follow the process environment; custom = use the proxy URL below. Saves take effect within about 30 seconds, no restart needed.',
    modeOff: 'Direct (off)',
    modeSystem: 'Follow process environment',
    modeCustom: 'Custom proxy',
    systemNotice:
      'System mode reads the process environment variables HTTP_PROXY / HTTPS_PROXY / NO_PROXY. Environment variables are process-level: in containerized deployments, changing them requires a container restart to take effect.',
    url: 'Proxy URL',
    urlTip:
      'The proxy URL used by custom mode — http, https, socks5, or socks5h (socks5h is equivalent to socks5; both resolve DNS on the proxy side). Private and loopback addresses are allowed.',
    urlPlaceholder: 'socks5://192.168.1.5:1080',
    saved: 'Saved',
    loadFailed: 'Failed to load the current setting',
    retry: 'Retry',
    conflict: 'This setting was changed elsewhere. Reloaded the latest version — review and save again.',
    proxyUrlRequired: 'A custom-mode proxy needs a proxy URL',
    proxyUrlUnparseable: 'The proxy URL cannot be parsed as a valid URL',
    proxyUrlScheme: 'The proxy URL scheme must be http, https, socks5, or socks5h',
    proxyUrlNoHost: 'The proxy URL is missing its host',
  },
  language: {
    title: 'Language',
    desc: 'Your interface language. A personal preference kept in this browser — it does not reach the database and applies immediately.',
    label: 'Interface language',
    labelTip:
      'Saved per browser, only for you. The sidebar Language entry switches the same stored preference — the two always stay in sync.',
  },
  capability: {
    title: 'Capability Enforcement',
    desc: 'Six forwarding rules driven by each model’s declared capability fields, each an independent switch, all off by default. The capability fields (model capability metadata) live on the model edit page; a rule only consults declared fields — an undeclared capability never triggers a refusal. Saves take effect within about 30 seconds, no restart needed.',
    contextLimitBlock: 'Context overflow block',
    contextLimitBlockTip:
      'A request whose estimated input breaks the addressed model’s declared context window (plus the margin) is refused with 413 before any upstream call; models without a declared window are never refused.',
    temperatureDefault: 'Default temperature',
    temperatureDefaultTip:
      'When the caller sends no temperature and the model declares one, the declared default is injected before forwarding; explicit values pass through untouched (unless the override below is also on).',
    temperatureForce: 'Force temperature override',
    temperatureForceTip:
      'When on, even an explicitly sent temperature is overridden by the model’s declared default. Off by default so caller parameters are never rewritten by accident.',
    toolsStrict: 'Strict tools refusal',
    toolsStrictTip:
      'A request carrying tools aimed at a model declared tools-incapable is refused with 400; models with an undeclared capability are never refused.',
    reasoningStrict: 'Strict reasoning refusal',
    reasoningStrictTip:
      'Judged against the addressed model only: a reasoning payload aimed at a model declared reasoning-incapable is refused with 400. A non-reasoning model the routing falls back to is not refused here — the landing clear below handles it.',
    reasoningEgressClear: 'Reasoning clear at landing',
    reasoningEgressClearTip:
      'Before dispatch to a landing candidate declared reasoning-incapable (fallback targets included), the request’s reasoning parameters are stripped so the upstream never sees a parameter it does not understand.',
    margin: 'Margin (percent)',
    marginTip:
      'The overflow threshold is the declared window × (1 + margin%). The band lets boundary requests through under estimation error instead of refusing them; ships at 5, allowed 0–1000.',
    marginRequired: 'Enter a margin percent',
    marginWholeNumber: 'The margin must be a whole percent',
    marginMin: 'The margin cannot be below 0',
    marginMax: 'The margin must be at most 1000',
    saved: 'Saved',
    loadFailed: 'Failed to load the current setting',
    retry: 'Retry',
    conflict: 'This setting was changed elsewhere. Reloaded the latest version — review and save again.',
  },
}
