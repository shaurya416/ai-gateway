package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
)

func TestRotateKey(t *testing.T) {
	h, r := setupTestRouter()
	adminKey := createAdminKey(t, h)

	// Create a key to rotate, save its original key string before rotation mutates it.
	key, err := h.Keys.Create(context.Background(), "rotatable-key", []string{model.ScopeReadOnly}, nil)
	if err != nil {
		t.Fatalf("failed to create key: %v", err)
	}
	keyID := key.ID
	originalKey := key.Key

	req := authedRequest(http.MethodPost, "/admin/keys/"+keyID+"/rotate", "", adminKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var rotated model.APIKey
	if err := json.NewDecoder(w.Body).Decode(&rotated); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if rotated.ID != keyID {
		t.Errorf("expected rotated key to keep ID %q, got %q", keyID, rotated.ID)
	}
	if !strings.HasPrefix(rotated.Key, "fgw_") {
		t.Errorf("expected rotated key to start with fgw_, got %q", rotated.Key)
	}
	if rotated.Key == originalKey {
		t.Error("expected rotation to generate a new credential")
	}
}

func TestRotateKeyNotFound(t *testing.T) {
	h, r := setupTestRouter()
	adminKey := createAdminKey(t, h)

	req := authedRequest(http.MethodPost, "/admin/keys/nonexistent/rotate", "", adminKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestRotateKey_KeyThatCannotAuthenticateIsRefused pins that rotation never
// hands out a secret that cannot authenticate.
//
// A revoked key stays revoked and an expired key stays expired, so a secret
// minted for either authenticates nothing. Rotating one answered 200 with that
// secret and recorded a successful rotation: an operator distributed a
// credential that failed on first use, with nothing in the response to say
// why. The dashboard already refuses to offer rotation for these keys; the
// API refuses it too, with a 409 naming the reason and a denied audit row.
func TestRotateKey_KeyThatCannotAuthenticateIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		arrange  func(t *testing.T, h *Handlers, id string)
		wantCode string
	}{
		{
			name: "revoked",
			arrange: func(t *testing.T, h *Handlers, id string) {
				if err := h.Keys.Revoke(t.Context(), id); err != nil {
					t.Fatalf("revoke: %v", err)
				}
			},
			wantCode: "key_revoked",
		},
		{
			name: "expired",
			arrange: func(t *testing.T, h *Handlers, id string) {
				past := time.Now().Add(-time.Minute)
				if err := h.Keys.SetExpiration(t.Context(), id, &past); err != nil {
					t.Fatalf("expire: %v", err)
				}
			},
			wantCode: "key_expired",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, r := setupTestRouter()
			audit := repository.NewMemoryAuditStore()
			h.Audit = audit
			adminKey := createAdminKey(t, h)
			target := createTestKey(t, h, "service", []string{model.ScopeReadOnly}, nil)
			tc.arrange(t, h, target.ID)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, authedRequest(http.MethodPost, "/admin/keys/"+target.ID+"/rotate", "", adminKey))

			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "fgw_") {
				t.Fatalf("a refused rotation returned a secret: %s", w.Body.String())
			}
			var payload struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			decodeJSON(t, w.Body, &payload)
			if payload.Error.Code != tc.wantCode {
				t.Fatalf("error code = %q, want %q", payload.Error.Code, tc.wantCode)
			}

			stored, err := h.Keys.Lookup(t.Context(), target.ID)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if stored.RotatedAt != nil {
				t.Fatalf("a refused rotation was applied: rotated_at = %v", stored.RotatedAt)
			}

			entries := auditEntries(t, audit, "key.rotate")
			if len(entries) != 1 || entries[0].Outcome != model.AuditDenied || !strings.Contains(entries[0].Detail, tc.wantCode) {
				t.Fatalf("key.rotate audit rows = %+v; want one denied row naming %q", entries, tc.wantCode)
			}
		})
	}
}
