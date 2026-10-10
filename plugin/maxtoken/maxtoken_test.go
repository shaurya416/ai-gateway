package maxtoken

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

func intPtr(v int) *int { return &v }

func testRequest(model string, messages ...string) *providers.Request {
	msgs := make([]providers.Message, len(messages))
	for i, m := range messages {
		msgs[i] = providers.Message{Role: "user", Content: m}
	}
	return &providers.Request{
		Model:    model,
		Messages: msgs,
	}
}

func initMaxToken(t *testing.T, config map[string]any) *MaxToken {
	t.Helper()
	m := &MaxToken{}
	if err := m.Init(config); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	return m
}

func TestMaxToken_MaxTokensEnforcement(t *testing.T) {
	m := initMaxToken(t, map[string]any{"max_tokens": 100})

	t.Run("exceeds limit", func(t *testing.T) {
		req := testRequest("gpt-4", "hello")
		req.MaxTokens = intPtr(200)
		pctx := plugin.NewContext(req)

		if err := m.Execute(context.Background(), pctx); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if !pctx.Reject {
			t.Error("expected request to be rejected")
		}
	})

	t.Run("within limit", func(t *testing.T) {
		req := testRequest("gpt-4", "hello")
		req.MaxTokens = intPtr(50)
		pctx := plugin.NewContext(req)

		if err := m.Execute(context.Background(), pctx); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if pctx.Reject {
			t.Error("expected request to be allowed")
		}
	})
}

// TestMaxToken_EnforcesEitherTokenField covers the ceiling being expressed in
// either field, or both. A request that named a small max_tokens and a huge
// max_completion_tokens passed a cap of 10 and then had the huge value
// forwarded upstream, because providers on the OpenAI API surface forward
// max_completion_tokens and drop max_tokens. The guardrail reads the ceiling
// the request actually asks for, where max_completion_tokens supersedes the
// deprecated max_tokens.
func TestMaxToken_EnforcesEitherTokenField(t *testing.T) {
	m := initMaxToken(t, map[string]any{"max_tokens": 10})

	tests := []struct {
		name                string
		maxTokens           *int
		maxCompletionTokens *int
		wantReject          bool
	}{
		{name: "neither field"},
		{name: "max_tokens within limit", maxTokens: intPtr(5)},
		{name: "max_completion_tokens within limit", maxCompletionTokens: intPtr(5)},
		{name: "both within limit", maxTokens: intPtr(5), maxCompletionTokens: intPtr(9)},
		{name: "max_tokens over limit", maxTokens: intPtr(500000), wantReject: true},
		{name: "max_completion_tokens over limit", maxCompletionTokens: intPtr(500000), wantReject: true},
		{
			name:                "small max_tokens hiding a huge max_completion_tokens",
			maxTokens:           intPtr(5),
			maxCompletionTokens: intPtr(500000),
			wantReject:          true,
		},
		{
			// The superseded field carries the huge value, so the request can
			// only ever obtain 5 tokens and the cap is not in play.
			name:                "huge max_tokens superseded by a small max_completion_tokens",
			maxTokens:           intPtr(500000),
			maxCompletionTokens: intPtr(5),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := testRequest("gpt-4o-mini", "hello")
			req.MaxTokens = tt.maxTokens
			req.MaxCompletionTokens = tt.maxCompletionTokens
			pctx := plugin.NewContext(req)

			if err := m.Execute(context.Background(), pctx); err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if pctx.Reject != tt.wantReject {
				t.Fatalf("Reject = %v (%s), want %v", pctx.Reject, pctx.Reason, tt.wantReject)
			}
			if tt.wantReject && !strings.Contains(pctx.Reason, "500000") {
				t.Errorf("Reason = %q, want it to name the offending value", pctx.Reason)
			}
		})
	}
}

func TestMaxToken_MaxMessagesEnforcement(t *testing.T) {
	m := initMaxToken(t, map[string]any{"max_messages": 2})

	t.Run("exceeds limit", func(t *testing.T) {
		req := testRequest("gpt-4", "msg1", "msg2", "msg3")
		pctx := plugin.NewContext(req)

		if err := m.Execute(context.Background(), pctx); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if !pctx.Reject {
			t.Error("expected request to be rejected")
		}
	})

	t.Run("within limit", func(t *testing.T) {
		req := testRequest("gpt-4", "msg1", "msg2")
		pctx := plugin.NewContext(req)

		if err := m.Execute(context.Background(), pctx); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if pctx.Reject {
			t.Error("expected request to be allowed")
		}
	})
}

func TestMaxToken_MaxInputLengthEnforcement(t *testing.T) {
	m := initMaxToken(t, map[string]any{"max_input_length": 10})

	t.Run("exceeds limit", func(t *testing.T) {
		req := testRequest("gpt-4", "this is a long message")
		pctx := plugin.NewContext(req)

		if err := m.Execute(context.Background(), pctx); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if !pctx.Reject {
			t.Error("expected request to be rejected")
		}
	})

	t.Run("within limit", func(t *testing.T) {
		req := testRequest("gpt-4", "short")
		pctx := plugin.NewContext(req)

		if err := m.Execute(context.Background(), pctx); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if pctx.Reject {
			t.Error("expected request to be allowed")
		}
	})
}

func TestMaxToken_ZeroDisablesLimit(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]any
		req    *providers.Request
	}{
		{
			name:   "max_messages 0 admits many messages",
			config: map[string]any{"max_messages": 0},
			req:    testRequest("gpt-4", "msg1", "msg2", "msg3"),
		},
		{
			name:   "max_tokens 0 admits a large max_tokens",
			config: map[string]any{"max_tokens": 0},
			req: func() *providers.Request {
				r := testRequest("gpt-4", "hello")
				r.MaxTokens = intPtr(1000000)
				return r
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := initMaxToken(t, tt.config)
			pctx := plugin.NewContext(tt.req)

			if err := m.Execute(context.Background(), pctx); err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if pctx.Reject {
				t.Errorf("expected request to be allowed, rejected with: %s", pctx.Reason)
			}
		})
	}
}

func TestMaxToken_MaxInputLengthCountsContentParts(t *testing.T) {
	imageURL := "data:image/png;base64," + strings.Repeat("A", 5000)

	tests := []struct {
		name       string
		message    providers.Message
		wantReject bool
	}{
		{
			name: "image data uri counts toward the limit",
			message: providers.Message{
				Role: "user",
				ContentParts: []providers.ContentPart{
					{Type: "image_url", ImageURL: &providers.ImageURLPart{URL: imageURL}},
				},
			},
			wantReject: true,
		},
		{
			name: "text parts are not counted twice",
			message: providers.Message{
				Role:    "user",
				Content: "0123456789",
				ContentParts: []providers.ContentPart{
					{Type: "text", Text: "01234"},
					{Type: "text", Text: "56789"},
				},
			},
			wantReject: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := initMaxToken(t, map[string]any{"max_input_length": 10})
			req := &providers.Request{Model: "gpt-4", Messages: []providers.Message{tt.message}}
			pctx := plugin.NewContext(req)

			if err := m.Execute(context.Background(), pctx); err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if pctx.Reject != tt.wantReject {
				t.Errorf("Reject = %v (%s), want %v", pctx.Reject, pctx.Reason, tt.wantReject)
			}
		})
	}
}

// PLUGIN-003: max-token is REJECT-ONLY over a ceiling the caller declared. A
// request that declares neither max_tokens nor max_completion_tokens — the
// common case — is not rejected and is not given one, so it runs to the
// provider's own default.
//
// This is the behaviour the catalog summary now describes, and the test that
// makes the two one thing: adding a clamp here would change what is sent
// upstream, which is transform behaviour in a guardrail and shows up in a
// provider bill, so anyone adding it must fail this test and revisit the
// summary rather than quietly widening the plugin.
func TestMaxToken_DeclaringNoCeilingIsNeitherRejectedNorCapped(t *testing.T) {
	m := initMaxToken(t, map[string]any{"max_tokens": 10})
	req := testRequest("gpt-4", "hello")
	pctx := plugin.NewContext(req)

	if err := m.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if pctx.Reject {
		t.Errorf("Reject = true (%s), want false: the request declares no ceiling to exceed", pctx.Reason)
	}
	if req.MaxTokens != nil {
		t.Errorf("MaxTokens = %d, want nil: this guardrail must not impose a ceiling the caller did not set", *req.MaxTokens)
	}
	if req.MaxCompletionTokens != nil {
		t.Errorf("MaxCompletionTokens = %d, want nil: this guardrail must not impose a ceiling the caller did not set", *req.MaxCompletionTokens)
	}
}

func TestMaxToken_AllowedRequestPassesThrough(t *testing.T) {
	m := initMaxToken(t, map[string]any{})
	req := testRequest("gpt-4", "hello")
	req.MaxTokens = intPtr(100)
	pctx := plugin.NewContext(req)

	if err := m.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if pctx.Reject {
		t.Error("expected default config to allow request")
	}
}

// A limit that cannot be the number the operator meant fails the load, at
// startup and under `ferrogw validate` alike. A quoted number or a ${VAR}
// reference, which resolves to a string, used to be skipped: max_input_length
// stayed off and the other two kept their defaults, while the plugin reported
// itself enabled. A negative value switched its limit off the same way.
func TestMaxToken_RejectsAMalformedLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]any
		key    string
	}{
		{"quoted max_input_length", map[string]any{"max_input_length": "2000"}, "max_input_length"},
		{"quoted max_tokens", map[string]any{"max_tokens": "8192"}, "max_tokens"},
		{"resolved reference", map[string]any{"max_messages": "20"}, "max_messages"},
		{"unresolved reference", map[string]any{"max_input_length": "${MAX_INPUT_LENGTH}"}, "max_input_length"},
		{"boolean", map[string]any{"max_tokens": true}, "max_tokens"},
		{"negative max_tokens", map[string]any{"max_tokens": -1}, "max_tokens"},
		{"negative max_input_length", map[string]any{"max_input_length": -5.0}, "max_input_length"},
		{"fractional max_messages", map[string]any{"max_messages": 2.5}, "max_messages"},
		{"NaN", map[string]any{"max_tokens": math.NaN()}, "max_tokens"},
		{"infinite", map[string]any{"max_input_length": math.Inf(1)}, "max_input_length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (&MaxToken{}).Init(tc.config)
			if err == nil {
				t.Fatalf("Init accepted %v; the limit it enforces is not the one written", tc.config)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("Init error %q does not name %q", err, tc.key)
			}
			if err := plugin.ValidateConfigFor("max-token", tc.config); err == nil {
				t.Errorf("ValidateConfigFor accepted %v, which Init rejects", tc.config)
			}
		})
	}
}

// A null value is an unset key, and a whole number written as a float is the
// number it reads as; neither is a malformed limit.
func TestMaxToken_AcceptsNullAndWholeFloatLimits(t *testing.T) {
	config := map[string]any{"max_tokens": nil, "max_messages": 3.0, "max_input_length": 100.0}
	if err := plugin.ValidateConfigFor("max-token", config); err != nil {
		t.Fatalf("ValidateConfigFor: %v", err)
	}
	m := initMaxToken(t, config)
	if m.maxTokens != 4096 || m.maxMessages != 3 || m.maxInputLen != 100 {
		t.Fatalf("limits = %d/%d/%d, want 4096/3/100", m.maxTokens, m.maxMessages, m.maxInputLen)
	}
}

// CORE-007: max-token's answer to "do you read request content" is decided by
// its config, and only max_input_length makes it true. A surface that cannot
// show a guardrail the content it would screen refuses on the strength of this
// answer, so a wrong `true` over-refuses and a wrong `false` forwards a body
// past a length cap that would have been satisfied at length zero.
func TestMaxToken_IgnoresRequestContent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]any
		want   bool
	}{
		{name: "defaults read no content", config: map[string]any{}, want: true},
		{name: "token and message caps are counts, not content", config: map[string]any{"max_tokens": 10, "max_messages": 2}, want: true},
		{name: "max_input_length measures the content", config: map[string]any{"max_input_length": 100}, want: false},
		{name: "max_input_length 0 is off", config: map[string]any{"max_input_length": 0}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := initMaxToken(t, tc.config)
			if got := m.IgnoresRequestContent(); got != tc.want {
				t.Errorf("IgnoresRequestContent() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A guardrail records every decision it makes, so a denial reaches the
// guardrail-match signal an operator sizes and audits a policy from. Only the
// denial for unreadable content did; a request over any of the three limits
// was refused with nothing recorded, and read as though no guardrail had acted.
func TestMaxToken_EveryDenialRecordsAGuardrailMatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]any
		req    func() *providers.Request
	}{
		{"max_tokens", map[string]any{"max_tokens": 10}, func() *providers.Request {
			req := testRequest("gpt-4", "hello")
			req.MaxTokens = intPtr(20)
			return req
		}},
		{"max_messages", map[string]any{"max_messages": 1}, func() *providers.Request {
			return testRequest("gpt-4", "one", "two")
		}},
		{"max_input_length", map[string]any{"max_input_length": 3}, func() *providers.Request {
			return testRequest("gpt-4", "hello")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := initMaxToken(t, tc.config)
			pctx := &plugin.Context{Request: tc.req(), Stage: plugin.StageBeforeRequest, Metadata: map[string]any{}}

			if err := m.Execute(context.Background(), pctx); err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if !pctx.Reject {
				t.Fatalf("expected the request to be rejected")
			}
			if len(pctx.GuardrailMatches) != 1 || pctx.GuardrailMatches[0].Action != plugin.ActionBlock {
				t.Errorf("GuardrailMatches = %+v, want one %q match for the denial", pctx.GuardrailMatches, plugin.ActionBlock)
			}
		})
	}
}

// A replayed tool call's arguments travel to the provider with the rest of the
// conversation and are billed as prompt tokens like it, so they count toward the
// input length. Measuring only the message bodies let a request carry any
// amount of input in an assistant turn's arguments past a cap of any size.
func TestMaxToken_MaxInputLengthCountsToolCallArguments(t *testing.T) {
	m := initMaxToken(t, map[string]any{"max_input_length": 100})
	req := &providers.Request{Model: "gpt-4", Messages: []providers.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{{Function: providers.FunctionCall{
			Name:      "write",
			Arguments: `{"body":"` + strings.Repeat("A", 5000) + `"}`,
		}}}},
	}}
	pctx := plugin.NewContext(req)

	if err := m.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !pctx.Reject {
		t.Fatal("5000 characters of tool-call arguments passed a 100-character input cap")
	}
}
