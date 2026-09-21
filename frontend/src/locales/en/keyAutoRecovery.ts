export default {
  title: 'Key Auto Recovery',
  desc: 'Keys removed from rotation for failed verification are retested on a schedule and recover automatically; manually disabled keys are not affected.',
  enabled: 'Enable auto recovery',
  enabledTip:
    'When off, keys removed from rotation for failed verification are no longer retested automatically; the manual path (test connection) is unaffected.',
  interval: 'Probe interval (minutes)',
  intervalTip:
    'Time between probe rounds, 1–1440 minutes (a day). Changes take effect within about a minute — no restart needed.',
  intervalRequired: 'Enter a probe interval',
  intervalWholeNumber: 'The probe interval must be a whole number of minutes',
  intervalMin: 'The probe interval must be at least 1 minute',
  intervalMax: 'The probe interval must be at most 1440 minutes (a day)',
  saved: 'Saved',
  loadFailed: 'Failed to load the current setting',
  retry: 'Retry',
  conflict: 'This setting was changed elsewhere. Reloaded the latest version — review and save again.',
}
