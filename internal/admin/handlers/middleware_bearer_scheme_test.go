package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
)

// schemeRequest builds a request carrying header verbatim as its Authorization
// value, so the scheme's spelling is the only thing a case varies.
func schemeRequest(method, url, header string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), method, url, nil)
	req.Header.Set("Authorization", header)
	return req
}

// TestAuth_BearerSchemeIsCaseInsensitive pins RFC 9110 §11.1: an auth-scheme is
// a case-insensitive token, separated from its credential by one or more
// spaces. Both the auth middleware and the session exchange matched the literal
// "Bearer " byte for byte, so a valid credential sent as "bearer <key>" was
// answered 401 "missing or invalid authorization header" — on /v1/*, /admin/*
// and /metrics alike, since they share the middleware.
func TestAuth_BearerSchemeIsCaseInsensitive(t *testing.T) {
	const masterKey = "test-master-key"
	_, router := routerWithStores(repository.NewKeyStore(), repository.NewSessionStore(), masterKey)

	for name, header := range map[string]string{
		"lower case":     "bearer " + masterKey,
		"upper case":     "BEARER " + masterKey,
		"mixed case":     "BeArEr " + masterKey,
		"several spaces": "Bearer   " + masterKey,
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, schemeRequest(http.MethodGet, "/admin/health", header))
			if w.Code != http.StatusOK {
				t.Fatalf("authenticated route: status = %d, want 200: %s", w.Code, w.Body.String())
			}

			w = httptest.NewRecorder()
			router.ServeHTTP(w, schemeRequest(http.MethodPost, "/admin/session", header))
			if w.Code != http.StatusCreated {
				t.Fatalf("session exchange: status = %d, want 201: %s", w.Code, w.Body.String())
			}
		})
	}

	// The scheme is still checked: another scheme, or "Bearer" run into the
	// credential, is not a bearer credential at all.
	for name, header := range map[string]string{
		"another scheme":   "Basic " + masterKey,
		"no separator":     "Bearer" + masterKey,
		"no scheme at all": masterKey,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			for _, req := range []*http.Request{
				schemeRequest(http.MethodGet, "/admin/health", header),
				schemeRequest(http.MethodPost, "/admin/session", header),
			} {
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("%s %s: status = %d, want 401: %s", req.Method, req.URL.Path, w.Code, w.Body.String())
				}
				if code := errorCode(t, w.Body.Bytes()); code != "missing_api_key" {
					t.Errorf("%s %s: code = %q, want missing_api_key", req.Method, req.URL.Path, code)
				}
			}
		})
	}
}
