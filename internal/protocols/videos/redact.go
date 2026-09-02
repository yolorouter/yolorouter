package videos

// The JSON half of audit rendering: a create body's only pixel carrier is
// input_reference.image_url, which may hold a base64 data URL or bare
// base64 where the caller skipped the prefix. The two get different
// floors, for the reason the images dialect taught: a data URL this
// package itself could have encoded is an image by construction, so it is
// redacted at any length, while a bare long base64 run is redacted only
// past a floor that no real URL can reach — a plain URL never matches the
// base64 alphabet for that many characters.

import (
	"fmt"
	"regexp"
)

var (
	dataURIRefRe = regexp.MustCompile(`("image_url"\s*:\s*")(data:[^"]{0,128};base64,)([A-Za-z0-9+/=]+)(")`)
	bareB64Re    = regexp.MustCompile(`("image_url"\s*:\s*")([A-Za-z0-9+/=]{1000,})(")`)
)

// RedactRequestBody returns body as text with reference-image payloads
// replaced by a note of their length. Everything else — the prompt, the
// model, the knobs — is exactly what an operator needs to see and stays.
func RedactRequestBody(body []byte) string {
	out := dataURIRefRe.ReplaceAllStringFunc(string(body), func(m string) string {
		parts := dataURIRefRe.FindStringSubmatch(m)
		return fmt.Sprintf("%s%s[base64 image omitted: %d chars]%s", parts[1], parts[2], len(parts[3]), parts[4])
	})
	out = bareB64Re.ReplaceAllStringFunc(out, func(m string) string {
		parts := bareB64Re.FindStringSubmatch(m)
		return fmt.Sprintf("%s[base64 image omitted: %d chars]%s", parts[1], len(parts[2]), parts[3])
	})
	return out
}
