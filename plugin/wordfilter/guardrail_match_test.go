package wordfilter

import (
	"context"
	"testing"

	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// Every block is recorded as a guardrail match, on the request and on the
// response, so a policy audited from that signal sees this plugin act.
func TestWordFilter_EveryBlockRecordsAGuardrailMatch(t *testing.T) {
	f := initFilter(t, map[string]any{"blocked_words": []any{"badword"}})

	request := plugin.NewContext(testRequest("a badword here"))
	request.Stage = plugin.StageBeforeRequest

	response := plugin.NewContext(testRequest("clean prompt"))
	response.Stage = plugin.StageAfterRequest
	response.Response = &providers.Response{Choices: []providers.Choice{{
		Message: providers.Message{Role: "assistant", Content: "a badword back"},
	}}}

	for name, pctx := range map[string]*plugin.Context{"request": request, "response": response} {
		if err := f.Execute(context.Background(), pctx); err != nil {
			t.Fatalf("%s: Execute error: %v", name, err)
		}
		if !pctx.Reject {
			t.Fatalf("%s: expected a block", name)
		}
		if len(pctx.GuardrailMatches) != 1 || pctx.GuardrailMatches[0].Action != plugin.ActionBlock {
			t.Errorf("%s: GuardrailMatches = %+v, want one %q match", name, pctx.GuardrailMatches, plugin.ActionBlock)
		}
	}
}
