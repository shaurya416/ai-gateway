package bedrock

import (
	"context"
	"fmt"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestBedrockProvider_Complete_LlamaDeveloperTurnIsSystem asserts an OpenAI
// "developer" turn reaches Llama under the system header. Llama 3's chat format
// defines the system, user, assistant and ipython headers only, so the turn was
// written under a "developer" header the model was never tuned on and its
// instructions were read as an unknown speaker's text, under a 200.
func TestBedrockProvider_Complete_LlamaDeveloperTurnIsSystem(t *testing.T) {
	const turn = "<|start_header_id|>%s<|end_header_id|>\n\n%s<|eot_id|>\n"
	fake := &fakeBedrockRuntimeClient{responses: [][]byte{[]byte(llamaFakeResponse)}}
	p := &Provider{name: Name, client: fake}

	if _, err := p.Complete(context.Background(), core.Request{
		Model: "meta.llama3-1-8b-instruct-v1:0",
		Messages: []core.Message{
			{Role: core.RoleDeveloper, Content: "You are terse."},
			{Role: core.RoleUser, Content: "hi"},
		},
	}); err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	var body bedrockLlamaRequest
	mustUnmarshalBody(t, fake.invokeCalls[0].Body, &body)
	want := "<|begin_of_text|>" +
		fmt.Sprintf(turn, "system", "You are terse.") +
		fmt.Sprintf(turn, "user", "hi") +
		"<|start_header_id|>assistant<|end_header_id|>\n\n"
	if body.Prompt != want {
		t.Errorf("prompt = %q, want %q", body.Prompt, want)
	}
}
