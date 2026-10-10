package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	nvidianim "github.com/ferro-labs/ai-gateway/providers/nvidia_nim"
)

// TestRerank_NegativeTopNIsTheCallersError pins top_n's lower bound at the
// trust boundary. A negative top_n names no count — core.RankedResults and the
// bedrock adapter both refuse it — but nothing checked it before a target was
// called, so on a gateway-capped adapter (nvidia-nim, deepinfra) the upstream
// rerank was made and paid for, the cap then failed as a plain error, and the
// caller got 500 "the gateway could not complete the request": a caller's
// mistake reported as the gateway's, which every SDK retries.
func TestRerank_NegativeTopNIsTheCallersError(t *testing.T) {
	const model = "nvidia/nv-rerankqa-mistral-4b-v3"

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// The NIM ranking payload the adapter's own tests decode.
		_, _ = w.Write([]byte(`{"rankings":[{"index":1,"logit":0.0},{"index":0,"logit":-2.0}],"usage":{"prompt_tokens":10,"total_tokens":10}}`))
	}))
	defer upstream.Close()

	provider, err := nvidianim.New("test-key", upstream.URL)
	if err != nil {
		t.Fatalf("nvidianim.New: %v", err)
	}
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: nvidianim.Name, Models: []string{model}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw.RegisterProvider(provider)

	rerank := func(topN string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		Rerank(gw)(w, jsonRequest(t, "/v1/rerank",
			`{"model":"`+model+`","query":"q","documents":["doc a","doc b"],"top_n":`+topN+`}`))
		return w
	}

	w := rerank("-1")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("top_n -1: status = %d, want 400: %s", w.Code, w.Body.String())
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Type != "invalid_request_error" {
		t.Fatalf("top_n -1: error type = %q (%v), want invalid_request_error: %s", body.Error.Type, err, w.Body.String())
	}
	if n := upstreamCalls.Load(); n != 0 {
		t.Fatalf("top_n -1 reached the upstream %d time(s); a request refused for its own content must not be paid for", n)
	}

	// The bound is a floor, not a refusal of top_n: a count is still served.
	if w := rerank("1"); w.Code != http.StatusOK {
		t.Fatalf("top_n 1: status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if n := upstreamCalls.Load(); n != 1 {
		t.Fatalf("top_n 1: upstream calls = %d, want 1", n)
	}
}
