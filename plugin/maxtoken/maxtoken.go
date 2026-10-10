// Package maxtoken provides a max-token guardrail that REJECTS a request
// exceeding a configured limit. Register it with a blank import:
//
//	_ "github.com/ferro-labs/ai-gateway/plugin/maxtoken"
//
// # Scope
//
// It is reject-only. It never writes to the request, so max_tokens bounds the
// ceiling a CALLER DECLARED and nothing else: a request that sets neither
// max_tokens nor max_completion_tokens declares no ceiling, passes, and runs to
// the provider's own default. That is deliberate. Injecting a ceiling the
// caller never asked for is transform behaviour in a guardrail — it changes
// what is sent upstream, and the change shows up in the provider's bill and in
// truncated completions nobody configured. An operator who needs a hard
// completion bound sets it on the client or picks a model whose default is the
// bound they want.
//
// max_messages and max_input_length have no such gap: both measure something
// every request carries.
package maxtoken

import (
	"context"
	"fmt"
	"math"

	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

func init() {
	plugin.RegisterFactory("max-token", func() plugin.Plugin {
		return &MaxToken{}
	})
}

// MaxToken is a guardrail plugin that enforces a maximum token limit
// on requests. It checks the max_tokens field and message length.
type MaxToken struct {
	maxTokens   int
	maxMessages int
	maxInputLen int
}

var _ plugin.ContentAgnostic = (*MaxToken)(nil)

// Name returns the plugin identifier.
func (m *MaxToken) Name() string { return "max-token" }

// Type returns the plugin lifecycle hook type.
func (m *MaxToken) Type() plugin.PluginType { return plugin.TypeGuardrail }

// limits are the values one max-token config block resolves to.
type limits struct {
	maxTokens   int
	maxMessages int
	maxInputLen int
}

// parseLimits reads and checks a max-token config block. It is the single
// place the plugin's rules live, shared by Init and ValidateConfig so a value
// the gateway would refuse to start on is the same value `ferrogw validate`
// reports.
//
// Every limit uses the same convention: 0 disables it. An operator who wants a
// guardrail off for one dimension writes 0 for that key and gets the other two
// unchanged. Omitting a key — or leaving it null — keeps its default
// (max_tokens 4096, max_messages 100); max_input_length has no default and is
// off until configured.
//
// A value that is not a whole number of zero or more is a load error. A quoted
// number, or a ${VAR} reference, which resolves to a string, used to be skipped
// in silence: max_tokens and max_messages kept their defaults and
// max_input_length stayed off, and a negative value turned its limit off too —
// a guardrail that loaded, reported itself enabled and enforced something other
// than what was written.
func parseLimits(config map[string]any) (limits, error) {
	l := limits{maxTokens: 4096, maxMessages: 100}
	var err error
	if v, ok := config["max_tokens"]; ok && v != nil {
		if l.maxTokens, err = limit("max_tokens", v); err != nil {
			return limits{}, err
		}
	}
	if v, ok := config["max_messages"]; ok && v != nil {
		if l.maxMessages, err = limit("max_messages", v); err != nil {
			return limits{}, err
		}
	}
	if v, ok := config["max_input_length"]; ok && v != nil {
		if l.maxInputLen, err = limit("max_input_length", v); err != nil {
			return limits{}, err
		}
	}
	return l, nil
}

// limit converts one configured value and rejects anything that is not a
// usable limit.
func limit(key string, v any) (int, error) {
	f, err := plugin.ToFloat64(v)
	if err != nil {
		return 0, fmt.Errorf("max-token: %s: %w", key, err)
	}
	if math.IsNaN(f) || f < 0 || f >= math.MaxInt || f != math.Trunc(f) {
		return 0, fmt.Errorf("max-token: %s must be a whole number >= 0, got %v; 0 turns this limit off", key, v)
	}
	return int(f), nil
}

// ValidateConfig checks the config block without building anything, so
// `ferrogw validate` and `ferrogw doctor` reject a limit this plugin would
// reject at startup. See plugin.ConfigValidator.
func (m *MaxToken) ValidateConfig(config map[string]any) error {
	_, err := parseLimits(config)
	return err
}

// Init configures the plugin from the provided options map. See parseLimits
// for the rules every limit follows.
func (m *MaxToken) Init(config map[string]any) error {
	l, err := parseLimits(config)
	if err != nil {
		return err
	}
	m.maxTokens = l.maxTokens
	m.maxMessages = l.maxMessages
	m.maxInputLen = l.maxInputLen
	return nil
}

// IgnoresRequestContent reports that this guardrail can reach its verdict
// without reading what the request actually says — true unless
// max_input_length is configured.
//
// The other two limits are counts, not content. max_tokens is the caller's own
// declared ceiling, and max_messages is the length of the message list; neither
// changes when the text inside those messages cannot be read. max_input_length
// is the exception: it MEASURES the content, so a body that projects to nothing
// satisfies it at length zero, which is exactly the vacuous approval the
// pass-through's refusal exists to prevent.
//
// Answering here rather than at Type() keeps the enforcement role honest: this
// is a guardrail either way, and it fails closed either way. See
// plugin.ContentAgnostic.
func (m *MaxToken) IgnoresRequestContent() bool { return m.maxInputLen == 0 }

// Execute runs the plugin logic for the current request context.
func (m *MaxToken) Execute(_ context.Context, pctx *plugin.Context) error {
	if pctx.Request == nil {
		return nil
	}

	// Enforce the request's completion-length ceiling, however it is expressed.
	//
	// EffectiveMaxTokens, not Request.MaxTokens: max_completion_tokens
	// supersedes max_tokens, and providers on the OpenAI API surface forward
	// only the former. Reading max_tokens alone let a request pair a small
	// max_tokens with a huge max_completion_tokens and have the huge one
	// forwarded past this cap.
	if requested, ok := pctx.Request.EffectiveMaxTokens(); m.maxTokens > 0 && ok && requested > m.maxTokens {
		deny(pctx, fmt.Sprintf("max_tokens %d exceeds limit of %d", requested, m.maxTokens))
		return nil
	}

	// Enforce max messages count.
	//
	// Only on a chat-shaped request. Embeddings and image generation reach the
	// plugin stages through a projection that turns each input element into one
	// user message, so a 150-document batch embedding arrives here as 150
	// messages — and a conversation-turn ceiling is not a statement about how
	// many documents may be embedded at once. The size limits below still
	// apply, because total input length means the same thing either way.
	_, projected := pctx.Metadata[plugin.MetadataSurface]
	if !projected && m.maxMessages > 0 && len(pctx.Request.Messages) > m.maxMessages {
		deny(pctx, fmt.Sprintf("message count %d exceeds limit of %d", len(pctx.Request.Messages), m.maxMessages))
		return nil
	}

	// Enforce max input length
	if m.maxInputLen > 0 {
		// Content the gateway could not project as text — an embeddings input
		// sent as token IDs — projects to no messages and measured zero, so a
		// cap of any size approved it. This limit reads the content, so it
		// takes the verdict every content guardrail gives what it cannot read.
		// See plugin.MetadataUninspectableContent.
		if plugin.RejectUninspectable(pctx) {
			return nil
		}
		totalLen := 0
		for _, msg := range pctx.Request.Messages {
			totalLen += messageLen(msg)
		}
		if totalLen > m.maxInputLen {
			deny(pctx, fmt.Sprintf("total input length %d exceeds limit of %d", totalLen, m.maxInputLen))
			return nil
		}
	}

	return nil
}

// deny refuses the request and records the refusal as a guardrail match, as
// every guardrail does on every decision. A denial left unrecorded reaches no
// guardrail-match consumer, so a policy sized or audited from that signal reads
// as though this plugin never acted.
func deny(pctx *plugin.Context, reason string) {
	pctx.NoteGuardrailMatch(plugin.ActionBlock)
	pctx.Reject = true
	pctx.Reason = reason
}

// messageLen measures a message the size it travels to the provider. A
// multipart message is measured from its parts alone: Content already holds
// the concatenated text parts, so adding both would count the text twice while
// still ignoring the image payload — which, for a base64 data URI, is nearly
// the whole request.
//
// The arguments of each tool call the message replays travel with it and are
// billed as prompt tokens like the rest of it, so they count too. Leaving them
// out let an assistant turn carry any amount of input past a cap of any size.
func messageLen(msg providers.Message) int {
	n := 0
	for _, call := range msg.ToolCalls {
		n += len(call.Function.Arguments)
	}
	if len(msg.ContentParts) == 0 {
		return n + len(msg.Content)
	}
	for _, part := range msg.ContentParts {
		n += len(part.Text)
		if part.ImageURL != nil {
			n += len(part.ImageURL.URL)
		}
	}
	return n
}

// Close releases plugin resources.
func (m *MaxToken) Close() error { return nil }
