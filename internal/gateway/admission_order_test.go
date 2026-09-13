package gateway

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yolorouter/yolorouter/internal/testutil"
)

// TestAMalformedRequestIsRefusedBeforeItsModelIsLookedUp pins an order that
// used to run the other way.
//
// Admitting a request is one question and it is answered in one place, so a body
// the protocol cannot accept is refused before anything is asked of the
// database. That is a change: the model lookup used to come first, and a caller
// who named a model that does not exist AND sent a body that does not parse was
// told about the model. They are now told about the body.
//
// Which is the better answer is arguable; that the answer is stable is not.
// Nothing pinned it before, so it could drift back on the next rearrangement
// without a single test noticing.
func TestAMalformedRequestIsRefusedBeforeItsModelIsLookedUp(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	apiKey := createAPIKey(t, db, APIKeyStatusActive, nil)

	// Both wrong at once: no model by this name exists, and an empty message
	// list is a body no OpenAI-shaped request may have.
	c, w := newCtx([]byte(`{"model":"no-such-model-anywhere","messages":[]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — the body is refused before the model is looked up; body = %s", w.Code, w.Body.String())
	}
	var got struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	if got.Error.Type != errTypeInvalidRequest {
		t.Errorf("error type = %q, want %q — a 404 here would mean the lookup ran first", got.Error.Type, errTypeInvalidRequest)
	}
}
