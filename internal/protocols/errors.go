package protocols

// AppendRequestID appends " (request: <id>)" to message so a caller
// reporting an error can quote the id and the operator can find the row. A
// no-op (returns message unchanged) when requestID is empty. Shared by every
// call site that builds this exact suffix, so they can't drift out of sync.
func AppendRequestID(message, requestID string) string {
	if requestID == "" {
		return message
	}
	return message + " (request: " + requestID + ")"
}
