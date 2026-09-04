package audio

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

// Dialect is how one provider family answers a speech request: which
// response formats it serves and which one an unspecified caller gets, how
// it counts the characters it bills, and the voice a probe asks for. The
// meter is the VENDOR's counting rule, not a rune count — settlement bills
// what the vendor's own invoice would count, and the same text meters
// differently per provider by design.
//
// The table lives here rather than in the gateway because two consumers
// must never disagree about it: the modality routes and bills by it, and
// the provider client probes by it. A private copy on either side is a
// drift class, not a saving.
type Dialect struct {
	Name          string
	Formats       map[string]bool
	DefaultFormat string
	Meter         func(input string) int
	// MeterLabel names what the meter counts, in the settlement snapshot's
	// spelling — the bill's own account of which vendor rule priced it.
	MeterLabel string
	// ProbeVoice is the voice a verification probe asks for; empty means
	// omit the field entirely (a dialect whose endpoint applies its own
	// documented default).
	ProbeVoice string
}

func utf8ByteMeter(input string) int  { return len(input) }
func characterMeter(input string) int { return utf8.RuneCountInString(input) }

var (
	// DialectSiliconFlow: the OpenAI speech shape on the SiliconFlow host,
	// priced per UTF-8 byte of input — bytes are what the meter counts, and
	// a rune count there would understate every CJK request against the
	// invoice.
	DialectSiliconFlow = Dialect{
		Name:          "siliconflow",
		Formats:       map[string]bool{"mp3": true, "opus": true, "wav": true, "pcm": true},
		DefaultFormat: "mp3",
		Meter:         utf8ByteMeter,
		MeterLabel:    "utf8_bytes",
		// The probe omits the voice: the preset-voice vocabulary is the
		// vendor's own and unverified against this build; an omitted voice
		// either applies their default or answers a readable refusal the
		// probe reports as-is.
		ProbeVoice: "",
	}
	// DialectZhipu: wav and pcm only, wav the default an unspecified caller
	// gets — announcing the OpenAI default mp3 there would promise bytes
	// the endpoint cannot render.
	DialectZhipu = Dialect{
		Name:          "zhipu",
		Formats:       map[string]bool{"wav": true, "pcm": true},
		DefaultFormat: "wav",
		Meter:         characterMeter,
		MeterLabel:    "characters",
		// The endpoint documents its own default voice; asking for one by
		// name would tie the probe to a vocabulary list it has no other
		// reason to carry.
		ProbeVoice: "",
	}
	// DialectOpenAI is the default: every base this build does not know by
	// name is spoken to in the OpenAI speech shape, whole format
	// vocabulary and all.
	DialectOpenAI = Dialect{
		Name:          "openai",
		Formats:       FormatVocabulary(),
		DefaultFormat: "mp3",
		Meter:         characterMeter,
		MeterLabel:    "characters",
		ProbeVoice:    "alloy",
	}
	// DialectMiniMax carries only the table half of the t2a dialect — the
	// format set and the meter — because its submit and answer shapes are
	// its own rather than the OpenAI form; the encode and decode live in
	// this package beside it.
	DialectMiniMax = Dialect{
		Name:          "minimax",
		Formats:       map[string]bool{"mp3": true, "pcm": true, "wav": true, "flac": true, "opus": true},
		DefaultFormat: "mp3",
		Meter:         MiniMaxMeter,
		MeterLabel:    "minimax_characters",
		ProbeVoice:    "male-qn-qingse",
	}
)

// speechFormats is the endpoint's own format vocabulary in canonical order —
// the union every dialect narrows from, the order a refusal lists them in,
// and the order a dialect's supported subset is named in. One slice so the
// spellings cannot drift apart.
var speechFormats = []string{"mp3", "opus", "aac", "flac", "wav", "pcm"}

// Formats is the canonical vocabulary, for callers that list rather than
// test membership.
func Formats() []string { return append([]string(nil), speechFormats...) }

// FormatVocabulary is the canonical order as a membership test.
func FormatVocabulary() map[string]bool {
	m := make(map[string]bool, len(speechFormats))
	for _, f := range speechFormats {
		m[f] = true
	}
	return m
}

// FormatList renders a dialect's format set in the canonical order, so a
// refusal names a stable list rather than whatever order a map iteration
// happens to produce.
func FormatList(d Dialect) string {
	supported := make([]string, 0, len(d.Formats))
	for _, f := range speechFormats {
		if d.Formats[f] {
			supported = append(supported, f)
		}
	}
	return strings.Join(supported, ", ")
}

// BaseHost strips a provider base URL to its bare lowercase hostname, the
// key the dialect table is keyed by. An unparseable base falls back to the
// trimmed raw string, which simply matches nothing.
func BaseHost(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		return strings.ToLower(u.Hostname())
	}
	return strings.ToLower(strings.TrimSpace(baseURL))
}

// DialectFor picks the dialect a provider's base URL speaks. The minimax
// case is the predicate rather than a host string so every spelling of the
// base resolves the same way here and in the encode/decode call sites.
func DialectFor(baseURL string) Dialect {
	switch {
	case MiniMaxSpeechBase(baseURL):
		return DialectMiniMax
	case BaseHost(baseURL) == "api.siliconflow.cn":
		return DialectSiliconFlow
	case BaseHost(baseURL) == "open.bigmodel.cn":
		return DialectZhipu
	default:
		return DialectOpenAI
	}
}
