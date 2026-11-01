package handlers

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
)

// TestListSessions_ReportsTheScopesTheSessionAuthenticatesWith pins that
// GET /admin/sessions lists each session with the scopes it is admitted under,
// not the ones it was minted with.
//
// A session is presented downstream with its source key's current scopes, so a
// change to that key's scopes takes effect on the session at once. The listing
// served the mint-time snapshot instead: a read-only operator whose key was
// raised to admin was listed as read_only while creating keys, and an admin
// whose key was cut back to read_only was still listed as admin — the one page
// an operator reads to see who holds admin access answered wrongly both ways.
func TestListSessions_ReportsTheScopesTheSessionAuthenticatesWith(t *testing.T) {
	cases := map[string]struct {
		minted      []string
		changed     []string
		createKeyOK int
	}{
		"raised to admin":    {minted: []string{model.ScopeReadOnly}, changed: []string{model.ScopeAdmin}, createKeyOK: http.StatusCreated},
		"cut to read_only":   {minted: []string{model.ScopeAdmin}, changed: []string{model.ScopeReadOnly}, createKeyOK: http.StatusForbidden},
		"left as it was":     {minted: []string{model.ScopeAdmin}, changed: nil, createKeyOK: http.StatusCreated},
		"read_only, unmoved": {minted: []string{model.ScopeReadOnly}, changed: nil, createKeyOK: http.StatusForbidden},
	}
	for name, tc := range cases {
		for storeName, sessions := range newTestSessionStores(t) {
			t.Run(name+"/"+storeName, func(t *testing.T) {
				keys := repository.NewKeyStore()
				h, r := routerWithStores(keys, sessions, "")
				observer := createAdminKey(t, h)
				operator := createTestKey(t, h, "operator", tc.minted, nil)
				token := exchangeSession(t, r, operator.Key)

				want := tc.minted
				if tc.changed != nil {
					if _, err := keys.Update(t.Context(), operator.ID, "", tc.changed); err != nil {
						t.Fatalf("change source key scopes: %v", err)
					}
					want = tc.changed
				}

				// What the session is actually admitted under.
				if status := createKeyViaSession(t, r, token, "probe"); status != tc.createKeyOK {
					t.Fatalf("POST /admin/keys with the session = %d, want %d", status, tc.createKeyOK)
				}

				w := httptest.NewRecorder()
				r.ServeHTTP(w, authedRequest(http.MethodGet, "/admin/sessions", "", observer))
				if w.Code != http.StatusOK {
					t.Fatalf("GET /admin/sessions = %d, want 200: %s", w.Code, w.Body.String())
				}
				var body struct {
					Data []model.Session `json:"data"`
				}
				decodeJSON(t, w.Body, &body)
				var listed *model.Session
				for i := range body.Data {
					if body.Data[i].CredentialID == operator.ID {
						listed = &body.Data[i]
					}
				}
				if listed == nil {
					t.Fatalf("GET /admin/sessions did not list the operator's session: %+v", body.Data)
				}
				if !slices.Equal(listed.Scopes, want) {
					t.Fatalf("GET /admin/sessions listed the session with scopes %v, want %v: the scopes it authenticates with", listed.Scopes, want)
				}
			})
		}
	}
}
