package images

import "testing"

// The stream event parse reads the JSON payload of a data line: the type is
// required, the usage rides completed events, and anything that is not this
// vocabulary's JSON is refused rather than silently skipped.
func TestParseStreamEvent(t *testing.T) {
	ev, err := ParseStreamEvent(`{"type":"image_generation.completed","usage":{"input_tokens":10,"output_tokens":1020,"total_tokens":1030}}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Type != EventGenerationCompleted || ev.Usage == nil {
		t.Fatalf("event = %+v, want a completed event with usage", ev)
	}
	if ev.Usage.InputTokens != 10 || ev.Usage.OutputTokens != 1020 || ev.Usage.TotalTokens != 1030 {
		t.Errorf("usage = %+v", ev.Usage)
	}

	ev, err = ParseStreamEvent(`{"type":"image_generation.partial_image","b64_json":"..."}`)
	if err != nil {
		t.Fatalf("parse partial: %v", err)
	}
	if ev.Type != EventGenerationPartial || ev.Usage != nil {
		t.Errorf("partial event = %+v, want no usage on it", ev)
	}

	for _, bad := range []string{`{}`, `not json`, `{"usage":{"total_tokens":5}}`} {
		if _, err := ParseStreamEvent(bad); err == nil {
			t.Errorf("payload %q parsed, want refusal", bad)
		}
	}
}
