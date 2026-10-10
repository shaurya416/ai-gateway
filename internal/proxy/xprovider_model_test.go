package proxy

import (
	"net/http"
	"testing"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
)

// A pass-through request that names its provider with X-Provider is recorded
// under the model its body names, as one placed by that model is. The header
// is the documented way to reach a model the routing index cannot enumerate —
// a fine-tune's base model, a model newer than the catalog — and resolving it
// skipped reading the body, so every such request reached the lifecycle naming
// no model: its request-log rows and span carried none, and a log filtered by
// the model the caller sent did not find it.
func TestPassthrough_XProviderRequestIsRecordedUnderItsModel(t *testing.T) {
	up := newCountingUpstream(t)
	store := &recordingLogStore{}

	t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
	gw, err := aigateway.New(config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "stub", Models: []string{"stub-model"}}},
		Plugins:  requestLoggerConfig(),
	})
	if err != nil {
		t.Fatalf("aigateway.New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	gw.RegisterProvider(&proxiableStub{name: "stub", baseURL: up.URL, models: []string{"stub-model"}})
	gw.SetRequestLogWriter(store)
	if err := gw.LoadPlugins(); err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}

	w := passthroughPOST(t, gw, "/v1/fine_tuning/jobs",
		`{"training_file":"file-abc","model":"base-model-the-index-does-not-list"}`,
		map[string]string{"X-Provider": "stub"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	entries := store.all()
	if len(entries) == 0 {
		t.Fatal("request log has no rows for the pass-through request")
	}
	for _, e := range entries {
		if e.Model != "base-model-the-index-does-not-list" {
			t.Errorf("%s row model = %q, want the model the body named", e.Stage, e.Model)
		}
	}
}
