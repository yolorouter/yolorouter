package gateway

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/yolorouter/yolorouter-ce/internal/service/safehttp"
)

// UpstreamClient sends the rewritten body to a provider. It reuses safehttp's
// SSRF-safe transport (provider_client.go's contract: every outbound dial is
// SSRF-checked) and disables redirect following so the decrypted upstream
// key cannot leak to a host the admin never confirmed.
type UpstreamClient struct {
	httpClient *http.Client
}

// NewUpstreamClient builds a gateway upstream client. The Transport is the
// same SSRF-safe one provider_client.go uses for connection tests, so a
// provider that tests green also serves real traffic through the identical
// network path.
func NewUpstreamClient() *UpstreamClient {
	return &UpstreamClient{
		httpClient: &http.Client{
			Transport: safehttp.NewTransport(),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout: 0, // see upstreamDialTimeout comment + relay loop's per-call ctx
		},
	}
}

// upstreamURL builds the POST URL for a chat-completions call. baseURL is the
// provider's configured base (e.g. https://api.openai.com/v1); the path
// /chat/completions is appended after trimming any trailing slash.
func upstreamURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/chat/completions"
}

// SendUpstream POSTs body to the provider with the decrypted upstream key.
// The dial carries upstreamDialTimeout; the response body is the caller's
// responsibility and must be closed. A non-nil error means a transport-level
// failure (network/timeout/SSRF-block) — HTTP status codes, including 5xx,
// come back as a non-nil response with nil error.
func (c *UpstreamClient) SendUpstream(ctx context.Context, baseURL, apiKey string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL(baseURL), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return c.httpClient.Do(req)
}
