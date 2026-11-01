package httpserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/go-chi/chi/v5"
)

// TestNativeV1RoutesMatchTheRouter holds nativeV1Routes and nativeV1Prefixes
// equal to the /v1 routes NewRouter mounts. routeNativeSpellings reads them to
// decide which spellings are the gateway's own, so a route mounted without an
// entry is reachable through the pass-through again by every spelling but the
// canonical one.
func TestNativeV1RoutesMatchTheRouter(t *testing.T) {
	reg := providers.NewRegistry()
	reg.Register(retryAfterStub{})
	router, ok := NewRouter(reg, repository.NewKeyStore(), repository.NewSessionStore(), nil, nil, nil, nil, nil, nil, nil, "", nil).(chi.Routes)
	if !ok {
		t.Fatal("NewRouter no longer returns a chi router; walk its routes another way")
	}

	mounted := map[string]bool{}
	walk := func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.ReplaceAll(route, "/*/", "/")
		if strings.HasPrefix(route, "/v1/") && route != "/v1/*" {
			mounted[route] = true
		}
		return nil
	}
	if err := chi.Walk(router, walk); err != nil {
		t.Fatalf("walk router: %v", err)
	}

	want := map[string]bool{}
	for route := range nativeV1Routes {
		want[route] = true
	}
	for _, prefix := range nativeV1Prefixes {
		want[prefix] = true
		want[prefix+"/*"] = true
	}

	for route := range mounted {
		if !want[route] {
			t.Errorf("%s is mounted but not listed in nativeV1Routes or nativeV1Prefixes", route)
		}
	}
	for route := range want {
		if !mounted[route] {
			t.Errorf("%s is listed as served natively but NewRouter does not mount it", route)
		}
	}
}
