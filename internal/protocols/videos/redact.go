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

// The key alternation below covers every spelling a reference image takes on
// its way upstream: the caller's own "image_url" (a plain string), the
// legacy dialect's "img_url", and the nested "url" inside the media and
// image_url objects the native dialects build. Exact quoted tokens, so
// sibling keys like "video_url" cannot match.
var (
	dataURIRefRe = regexp.MustCompile(`("(?:image_url|img_url|url)"\s*:\s*")(data:[^"]{0,128};base64,)([A-Za-z0-9+/=]+)(")`)
	bareB64Re    = regexp.MustCompile(`("(?:image_url|img_url|url)"\s*:\s*")([A-Za-z0-9+/=]{1000,})(")`)
)

// RedactRequestBody returns body as text with reference-image payloads
// replaced by a note of their length, in every spelling they appear in:
// the caller's request and the re-encoded bodies the native dialects
// send upstream both pass through here (the log policy stores both
// rendered), and a redactor that only knew the caller's spelling would
// let the same pixels back in one hop later. Everything else — the
// prompt, the model, the knobs — is exactly what an operator needs to
// see and stays.
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
