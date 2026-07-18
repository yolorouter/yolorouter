export const errcodes: Record<number, string> = {
  0: 'Success',
  10001: 'Invalid username or password',
  10003: 'Session expired, please log in again',
  10005: 'Too many failed attempts, account temporarily locked',
  10007: 'First-run setup already completed',
  90001: 'Route not found',
  90002: 'Method not allowed',
  90003: 'Request entity too large',
  50001: 'Internal error',
  50002: 'Database error',
  50003: 'Invalid parameter',
  50005: 'Service temporarily unavailable, please try again shortly',
}
