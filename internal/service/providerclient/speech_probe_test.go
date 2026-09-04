package providerclient

// The speech probe battery: a mapping is healthy when one short synthesis
// comes back as audio — announced as audio, or bare octets, or hex inside
// the t2a envelope — and nothing else passes for a pass.

import (
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols/audio"
)

func TestSpeechGenerationProbePassesOnAnnouncedAudio(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			t.Errorf("unexpected probe path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3probe"))
	})
	defer srv.Close()

	res, err := c.TestSpeechGeneration(t.Context(), srv.URL, "sk-test", "cosyvoice")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestSuccess {
		t.Fatalf("an audio answer must pass, got %+v", res)
	}
}

func TestSpeechGenerationProbeSendsTheOpenAIShape(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		body := readAll(t, r)
		for _, want := range []string{`"model":"cosyvoice"`, `"response_format":"mp3"`, `"voice":"alloy"`} {
			if !strings.Contains(body, want) {
				t.Errorf("probe body %s does not contain %s", body, want)
			}
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("x"))
	})
	defer srv.Close()

	if _, err := c.TestSpeechGeneration(t.Context(), srv.URL, "sk-test", "cosyvoice"); err != nil {
		t.Fatalf("probe errored: %v", err)
	}
}

// A dialect whose probe voice is empty (the vendor applies its own default)
// must omit the field rather than send a name nobody chose.
func TestSpeechGenerationProbeOmitsVoiceWhereTheDialectDefaults(t *testing.T) {
	previous := speechDialectFor
	speechDialectFor = func(string) audio.Dialect { return audio.DialectZhipu }
	t.Cleanup(func() { speechDialectFor = previous })

	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		body := readAll(t, r)
		if strings.Contains(body, "voice") {
			t.Errorf("probe body %s carries a voice the dialect never named", body)
		}
		if !strings.Contains(body, `"response_format":"wav"`) {
			t.Errorf("probe body %s does not state the dialect's own default format", body)
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("x"))
	})
	defer srv.Close()

	if _, err := c.TestSpeechGeneration(t.Context(), srv.URL, "sk-test", "glm-tts"); err != nil {
		t.Fatalf("probe errored: %v", err)
	}
}

func TestSpeechGenerationProbePassesOnBareOctets(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("raw-bytes"))
	})
	defer srv.Close()

	res, err := c.TestSpeechGeneration(t.Context(), srv.URL, "sk-test", "m")
	if err != nil || res.Outcome != TestSuccess {
		t.Fatalf("bare octets must pass like announced audio: %+v %v", res, err)
	}
}

func TestSpeechGenerationProbeRefusesAudioless200(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"model overloaded"}}`))
	})
	defer srv.Close()

	res, err := c.TestSpeechGeneration(t.Context(), srv.URL, "sk-test", "m")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestUpstreamError {
		t.Fatalf("a JSON 200 is a failure wearing a success status, got %+v", res)
	}
}

func TestSpeechGenerationProbeClassifiesUnauthorized(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`))
	})
	defer srv.Close()

	res, err := c.TestSpeechGeneration(t.Context(), srv.URL, "sk-bad", "m")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestAuthFailed {
		t.Fatalf("a 401 must classify as auth failure, got %+v", res)
	}
}

// The minimax arm: one t2a_v2 submit whose hex envelope parses, through the
// same predicate the gateway's dialect table routes by.
func TestSpeechGenerationProbeMiniMaxHexEnvelope(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != audio.MiniMaxSpeechPath {
			t.Errorf("unexpected probe path %s, want the t2a_v2 route", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"audio":"` + hex.EncodeToString([]byte("mp3-bytes")) + `"},` +
			`"base_resp":{"status_code":0,"status_msg":""}}`))
	})
	defer srv.Close()

	withMiniMaxSpeechBase(t, srv.URL)

	res, err := c.TestSpeechGeneration(t.Context(), srv.URL, "sk-test", "speech-2.8-turbo")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestSuccess {
		t.Fatalf("a parsed hex envelope must pass, got %+v", res)
	}
}

func TestSpeechGenerationProbeMiniMaxRefusalInside200(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"audio":""},"base_resp":{"status_code":1004,"status_msg":"invalid api key"}}`))
	})
	defer srv.Close()

	withMiniMaxSpeechBase(t, srv.URL)

	res, err := c.TestSpeechGeneration(t.Context(), srv.URL, "sk-test", "speech-2.8-turbo")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestUpstreamError || !strings.Contains(res.Detail, "1004") {
		t.Fatalf("a base_resp refusal must fail with the vendor's code, got %+v", res)
	}
}

func readAll(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read probe body: %v", err)
	}
	return string(body)
}

// withMiniMaxSpeechBase forces the speech probe's minimax arm on for one
// test, scoped to this rig's local server.
func withMiniMaxSpeechBase(t *testing.T, serverURL string) {
	t.Helper()
	previous := isMiniMaxSpeechBase
	isMiniMaxSpeechBase = func(baseURL string) bool { return baseURL == serverURL }
	t.Cleanup(func() { isMiniMaxSpeechBase = previous })
}
