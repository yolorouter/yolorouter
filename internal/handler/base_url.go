// Package handler: how this instance names itself to the outside world.
// Both the OAuth callback address and the gateway endpoint the console
// shows to API clients resolve the same way, so the resolution lives here
// rather than inside either caller.
package handler

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// publicBaseURL resolves the base URL clients outside the process should
// use to reach this instance: the configured server.external_url when set
// (the operator-pinned origin — trusted), otherwise derived from the
// request's Host header and forwarding proxies' X-Forwarded-Proto, both
// client-controlled on deployments whose proxy does not pin them.
func publicBaseURL(c *gin.Context, externalURL string) string {
	if externalURL != "" {
		return strings.TrimRight(externalURL, "/")
	}
	// This binary only ever serves plain HTTP — TLS is terminated by
	// whatever proxy sits in front — so the forwarded header is the only
	// signal there is. A c.Request.TLS check would never fire.
	scheme := "http"
	if c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host
}
