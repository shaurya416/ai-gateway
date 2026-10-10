package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers/replicate"
)

// TestImages_NoCapableProvider_Returns404 verifies that a request whose model
// has no registered ImageProvider returns HTTP 404 with an OpenAI
// invalid_request_error/model_not_found body rather than 500/routing_error.
// Regression test for the capability-miss-as-server-error bug.
func TestImages_NoCapableProvider_Returns404(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "unused"}},
	})

	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := `{"model":"no-such-image-model","prompt":"a cat"}`
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	w := httptest.NewRecorder()

	Images(gw)(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (body=%s)", w.Code, w.Body.String())
	}

	var resp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Type != "invalid_request_error" {
		t.Errorf("expected type invalid_request_error, got %q", resp.Error.Type)
	}
	if resp.Error.Code != "model_not_found" {
		t.Errorf("expected code model_not_found, got %q", resp.Error.Code)
	}
}

// TestImages_NonPositiveNIsTheCallersError pins n's lower bound at the trust
// boundary. No target can generate fewer than one image, yet nothing checked n
// before one was called: replicate (and bedrock's titan/nova) drop a zero n as
// an omitted field, so `n: 0` reached the upstream as no count at all and was
// generated, billed and answered 200 at its default count, and a negative n was
// forwarded for the upstream to refuse.
func TestImages_NonPositiveNIsTheCallersError(t *testing.T) {
	const model = "black-forest-labs/flux-schnell"

	var (
		mu     sync.Mutex
		inputs []map[string]any
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input map[string]any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		inputs = append(inputs, body.Input)
		mu.Unlock()
		// The prediction payload the adapter's own tests decode.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"img-1","status":"succeeded","output":["https://example.com/img.png"]}`))
	}))
	defer upstream.Close()

	provider, err := replicate.New("test-token", upstream.URL, nil, []string{model})
	if err != nil {
		t.Fatalf("replicate.New: %v", err)
	}
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: replicate.Name, Models: []string{model}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw.RegisterProvider(provider)

	generate := func(n string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		Images(gw)(w, jsonRequest(t, "/v1/images/generations", `{"model":"`+model+`","prompt":"a cat","n":`+n+`}`))
		return w
	}
	calls := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(inputs)
	}

	for _, n := range []string{"0", "-1"} {
		w := generate(n)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("n %s: status = %d, want 400: %s", n, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"type":"invalid_request_error"`) {
			t.Errorf("n %s: body = %s, want an invalid_request_error", n, w.Body.String())
		}
		if got := calls(); got != 0 {
			t.Fatalf("n %s reached the upstream (%d call(s)); a request for no images must not generate one", n, got)
		}
	}

	// The bound is a floor: a count is still forwarded as asked.
	if w := generate("1"); w.Code != http.StatusOK {
		t.Fatalf("n 1: status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := calls(); got != 1 {
		t.Fatalf("n 1: upstream calls = %d, want 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := inputs[0]["num_outputs"]; got != float64(1) {
		t.Errorf("n 1: upstream num_outputs = %v, want 1", got)
	}
}
