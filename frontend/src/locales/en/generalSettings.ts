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
  language: {
    title: 'Language',
    desc: 'Your interface language. A personal preference kept in this browser — it does not reach the database and applies immediately.',
    label: 'Interface language',
    labelTip:
      'Saved per browser, only for you. The sidebar Language entry switches the same stored preference — the two always stay in sync.',
  },
}
