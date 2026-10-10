package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ferro-labs/ai-gateway/internal/apierror"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
	"github.com/ferro-labs/ai-gateway/providers"
)

// ResponsesSource is what the Responses surface needs from the gateway: provider
// resolution (via the embedded ProviderSource), the configured id-subroute
// target, and the governed+priced forward for the create endpoint. *Gateway
// implements it.
type ResponsesSource interface {
	providers.ProviderSource
	ResponsesTarget() string
	RouteResponsesWithPricingProvider(ctx context.Context, target, priceProvider, model, body string, bodyInspectable bool, maxOutputTokens int, usage *providers.Usage, forward func(context.Context) error) error
}

// ResponsesCreate returns the handler for POST /v1/responses. It routes by the
// body's model exactly as chat does, forwards to that provider governed by the
// full pipeline (plugins, breaker, concurrency, request log), and — unlike the
// generic pass-through — extracts the response usage so the request is PRICED.
// The body is forwarded verbatim; only model, max_output_tokens and the
// guardrail text projection are parsed from it, all schema-stable fields.
func ResponsesCreate(src ResponsesSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if unsafeProxyPath(r.URL) {
			apierror.WriteOpenAI(w, http.StatusBadRequest,
				"pass-through path contains a disallowed traversal segment",
				"invalid_request_error", "invalid_proxy_path")
			return
		}

		model, maxOutputTokens, unreadable := peekResponsesFields(r)
		if unreadable != "" {
			apierror.WriteOpenAI(w, http.StatusBadRequest, unreadable, "invalid_request_error", "invalid_request")
			return
		}

		p, ok := resolveResponsesProvider(r, src, model)
		if !ok {
			if r.Header.Get("X-Provider") != "" {
				apierror.WriteOpenAI(w, http.StatusNotFound, "no configured target serves the requested provider", "invalid_request_error", "provider_not_found")
				return
			}
			if model == "" {
				apierror.WriteOpenAI(w, http.StatusBadRequest, "model is required", "invalid_request_error", "invalid_request")
				return
			}
			apierror.WriteModelNotFound(w)
			return
		}

		pp, canProxy := providers.As[providers.ProxiableProvider](p)
		if !canProxy {
			apierror.WriteOpenAI(w, http.StatusNotImplemented, "provider "+p.Name()+" does not support the responses endpoint", "invalid_request_error", "responses_not_supported")
			return
		}
		if _, nativeOnly := providers.As[providers.NonOpenAIWireProvider](p); nativeOnly {
			apierror.WriteOpenAI(w, http.StatusNotImplemented, "provider "+p.Name()+" is not available for the OpenAI-compatible responses endpoint; use its native chat endpoint", "invalid_request_error", "responses_not_supported")
			return
		}

		target, err := url.Parse(pp.BaseURL())
		if err != nil {
			logger.Default().Error("invalid provider base URL", "provider", p.Name(), "error", err)
			apierror.WriteOpenAI(w, http.StatusInternalServerError, "upstream provider is unavailable", "server_error", "internal_error")
			return
		}

		projText, inspectable := projectBody(r)
		providerName := p.Name()
		priceProvider := providers.CanonicalName(p)
		authHeaders := pp.AuthHeaders()

		// The usage tee wraps the response body once the upstream headers are in:
		// text/event-stream selects the SSE terminal-event scan, otherwise the
		// non-streaming JSON body is parsed for its top-level usage.
		var usage providers.Usage
		wrapBody := func(resp *http.Response) {
			stream := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
			resp.Body = newResponsesUsageReader(resp.Body, stream, &usage)
		}

		// The lifecycle runs under ctx, so cancelling it is what ends a request
		// whose body the caller could not deliver as the caller's. See
		// callerBody.
		ctx, cancelRequest := context.WithCancelCause(r.Context())
		defer cancelRequest(nil)
		watchCallerBody(r, func() { cancelRequest(nil) })

		forwarded := false
		var forwardErr error
		forward := func(ctx context.Context) error {
			forwarded = true
			// An upstream error status comes back as a failure for the lifecycle
			// to record; which statuses the breaker scores is the gateway's call
			// (see Gateway.RoutePassthrough).
			forwardErr = forwardFixedTarget(w, r.WithContext(ctx), target, authHeaders, providerName, propagatesTrace(src), wrapBody)
			return forwardErr
		}

		err = src.RouteResponsesWithPricingProvider(ctx, providerName, priceProvider, model, projText, inspectable, maxOutputTokens, &usage, forward)
		reraiseAbort(forwardErr)
		if err != nil && !forwarded {
			apierror.WriteRouteError(w, err)
		}
	}
}

// ResponsesIDs returns the handler for the stateful Responses id sub-routes —
// GET/DELETE /v1/responses/{id}, POST /v1/responses/{id}/cancel, and GET
// /v1/responses/{id}/input_items. These carry no model and reference an opaque,
// provider-scoped response id, so a single configured responses_target serves
// them all (the same reasoning as Files/Batches). Off (501) when unset.
func ResponsesIDs(src ResponsesSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if unsafeProxyPath(r.URL) {
			apierror.WriteOpenAI(w, http.StatusBadRequest,
				"pass-through path contains a disallowed traversal segment",
				"invalid_request_error", "invalid_proxy_path")
			return
		}

		name := src.ResponsesTarget()
		if name == "" {
			apierror.WriteOpenAI(w, http.StatusNotImplemented,
				"the responses id sub-routes are not configured; set responses_target",
				"invalid_request_error", "responses_not_configured")
			return
		}
		p, ok := src.Get(name)
		if !ok {
			apierror.WriteOpenAI(w, http.StatusNotImplemented, "the configured responses target is unavailable", "invalid_request_error", "responses_not_configured")
			return
		}
		pp, canProxy := providers.As[providers.ProxiableProvider](p)
		if !canProxy {
			apierror.WriteOpenAI(w, http.StatusNotImplemented, "provider "+p.Name()+" does not support the responses endpoint", "invalid_request_error", "responses_not_supported")
			return
		}
		// The same refusal the create route makes. A non-OpenAI-wire provider
		// serves no Responses id under its base URL, so forwarding sent the
		// request, under that provider's credential, to a path its upstream
		// does not have, and relayed whatever it answered.
		if _, nativeOnly := providers.As[providers.NonOpenAIWireProvider](p); nativeOnly {
			apierror.WriteOpenAI(w, http.StatusNotImplemented, "provider "+p.Name()+" is not available for the OpenAI-compatible responses endpoint", "invalid_request_error", "responses_not_supported")
			return
		}

		target, err := url.Parse(pp.BaseURL())
		if err != nil {
			logger.Default().Error("invalid provider base URL", "provider", p.Name(), "error", err)
			apierror.WriteOpenAI(w, http.StatusInternalServerError, "upstream provider is unavailable", "server_error", "internal_error")
			return
		}

		// The id sub-routes are a straight forward like Files/Batches: no
		// governance to score, so the reported failure is unused; only a
		// mid-body abort is re-raised.
		err = forwardFixedTarget(w, r, target, pp.AuthHeaders(), p.Name(), propagatesTrace(src), nil)
		reraiseAbort(err)
	}
}

// resolveResponsesProvider picks the provider for a create call: an explicit
// X-Provider header wins (gated on target membership), otherwise the body's
// model owns it. It mirrors ResolveProvider but takes the model the caller
// already peeked, so the body is not scanned twice.
func resolveResponsesProvider(r *http.Request, src providers.ProviderSource, model string) (providers.Provider, bool) {
	if name := r.Header.Get("X-Provider"); name != "" {
		p, ok := src.Get(name)
		if ok && !providerIsAllowed(src, p.Name()) {
			return nil, false
		}
		return p, ok
	}
	if model == "" {
		return nil, false
	}
	return findModelOwner(src, model)
}

// peekResponsesFields reads the two schema-stable top-level fields the create
// path governs on — model (routing) and max_output_tokens (the completion
// ceiling a guardrail caps) — then restores the body so it forwards intact. A
// body past the projection cap yields zero values; the guardrail projection
// (projectBody) refuses it separately when a content guardrail is configured.
//
// The keys are matched exactly, and a body that also carries another spelling
// of either is reported ambiguous rather than read. Decoded into a struct,
// encoding/json matches a key in any letter case, so a body carrying
// "max_output_tokens": 100000 followed by "Max_Output_Tokens": 10 was governed
// on 10 — approved by a max-token guardrail — while an upstream whose parser
// matches exactly generated against 100000, and a trailing "MODEL" chose the
// routing and pricing model while that upstream ran the "model" one. Reading
// only the exact key does not settle it either: an upstream that folds case as
// encoding/json does honours the other spelling. Which one a given upstream
// reads is not something the gateway can know, so it refuses to guess.
//
// A max_output_tokens the gateway cannot read as an integer is refused for the
// same reason. Read as absent, 100000.0, 1e5 or "100000" reached the max-token
// guardrail as no ceiling at all, which it approves, and was forwarded verbatim
// to an upstream free to honour it. Chat refuses the same value in max_tokens
// when it decodes the body. A JSON null is absent, as it is on chat.
//
// unreadable is the refusal to answer with, empty when both fields were read.
func peekResponsesFields(r *http.Request) (model string, maxOutputTokens int, unreadable string) {
	if r.Body == nil || r.ContentLength == 0 {
		return "", 0, ""
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, projectionCap+1))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))
	if err != nil || len(buf) > projectionCap {
		return "", 0, ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(buf, &fields) != nil {
		return "", 0, ""
	}
	for key := range fields {
		for _, governed := range [...]string{"model", "max_output_tokens"} {
			if key != governed && strings.EqualFold(key, governed) {
				return "", 0, "model and max_output_tokens must be spelled exactly as named; the body carries another spelling of one"
			}
		}
	}
	_ = json.Unmarshal(fields["model"], &model)
	if ceiling, present := fields["max_output_tokens"]; present && json.Unmarshal(ceiling, &maxOutputTokens) != nil {
		return "", 0, "max_output_tokens must be an integer"
	}
	return model, maxOutputTokens, ""
}
