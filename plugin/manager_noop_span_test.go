package plugin

import (
	"context"
	"testing"

	"github.com/ferro-labs/ai-gateway/observability"
	"github.com/ferro-labs/ai-gateway/providers"
)

// A gateway with tracing off hands every plugin context the NoOp provider's
// root span. A child of that span records nothing, so opening one per plugin
// must cost nothing either. Naming the child and boxing its three attributes
// cost four allocations per plugin per stage, so a request through five plugins
// paid twenty allocations for telemetry nobody would ever receive — more than
// the whole of Route's own allocation baseline.
func TestManager_NoOpRootSpanAddsNoAllocations(t *testing.T) {
	m := NewManager(nil)
	for _, name := range []string{"word-filter", "regex-guard", "pii-redact"} {
		if err := m.Register(StageBeforeRequest, &mockPlugin{name: name, typ: TypeGuardrail}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"secret-scan", "request-logger"} {
		if err := m.Register(StageAfterRequest, &mockPlugin{name: name, typ: TypeLogging}); err != nil {
			t.Fatal(err)
		}
	}

	ctx := context.Background()
	_, noopRoot := observability.NoOp().StartRequestSpan(ctx, observability.RequestAttrs{})
	req := &providers.Request{Model: "gpt-4o"}
	run := func(root observability.Span) float64 {
		return testing.AllocsPerRun(500, func() {
			pctx := NewContext(req)
			pctx.Span = root
			_ = m.RunBefore(ctx, pctx)
			_ = m.RunAfter(ctx, pctx)
			PutContext(pctx)
		})
	}

	withoutSpan := run(nil)
	withNoOp := run(noopRoot)
	if withNoOp > withoutSpan {
		t.Fatalf("a NoOp root span added allocations to the plugin stages: %v per request with it, %v without",
			withNoOp, withoutSpan)
	}
}
