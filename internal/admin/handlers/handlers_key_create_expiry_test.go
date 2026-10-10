package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
)

// TestCreateKey_ExpiryNotInTheFutureIsRefused pins POST /admin/keys against an
// expires_at at or before the moment the key would be stored. Such a key can
// never authenticate, yet it was stored and answered 201 with its secret, so the
// caller was handed a credential that failed on first use with nothing in the
// response to say why. The ferrogw CLI refused it on its own side; every other
// client of the Admin API did not.
func TestCreateKey_ExpiryNotInTheFutureIsRefused(t *testing.T) {
	h, r := setupTestRouter()
	admin := createAdminKey(t, h)

	before, err := h.Keys.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	for name, expiresAt := range map[string]string{
		"long past":        "2020-01-01T00:00:00Z",
		"a minute ago":     time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		"this very second": time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, authedRequest(http.MethodPost, "/admin/keys", `{"name":"ci","expires_at":"`+expiresAt+`"}`, admin))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s (%s): status = %d, want 400: %s", name, expiresAt, w.Code, w.Body.String())
			continue
		}
		if !strings.Contains(w.Body.String(), "expires_at") || strings.Contains(w.Body.String(), `"key"`) {
			t.Errorf("%s: body = %s, want a refusal naming expires_at and no key", name, w.Body.String())
		}
	}

	after, err := h.Keys.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("keys stored = %d, want %d: a refused create must store nothing", len(after), len(before))
	}

	// A future expiry is still accepted, and the key it returns authenticates.
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodPost, "/admin/keys", `{"name":"ci","expires_at":"`+future+`"}`, admin))
	if w.Code != http.StatusCreated {
		t.Fatalf("future expiry: status = %d, want 201: %s", w.Code, w.Body.String())
	}
	var created model.APIKey
	decodeJSON(t, w.Body, &created)
	validateTestKey(t, h, created.Key)
}
