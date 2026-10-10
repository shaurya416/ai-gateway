package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/apierror"
	"github.com/ferro-labs/ai-gateway/internal/httpclient"
	"github.com/ferro-labs/ai-gateway/internal/streamio"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
)

// forwardFixedTarget transparently reverse-proxies r to one fixed upstream — the
// API root `target` with `authHeaders` injected — and streams the response back.
// It is the shared core of the surfaces that forward to a single configured
// backend rather than routing by model (Files/Batches and the Responses id
// sub-routes), and it reuses the /v1/* proxy's security machinery: it shares
// buildRewrite, so the gateway's own /v1 prefix is stripped, the client
// credential is replaced by the provider's, the caller-identity headers that
// address the gateway are dropped, and trace context is injected when
// propagateTrace is set. An upstream that echoes the injected credential is
// redacted (bounded by sanitizeScanBudget), and the streaming idle bound
// replaces the WriteTimeout that WrapResponseWriter clears. The caller has
// already validated the path (unsafeProxyPath) and resolved target/authHeaders.
//
// wrapBody, when non-nil, wraps the response body just before it streams to the
// client — used by the Responses surface to tee usage out of the body; a nil
// value forwards the body untouched. A non-nil wrapBody also withholds the
// caller's Accept-Encoding, so the body it reads arrives decoded and the client
// receives it uncompressed.
//
// It returns the forward's failure, nil when the upstream answered with a status
// below 400: the forward error when the upstream could not be reached or read
// (the ErrorHandler has already written the client's response in that case), or
// the upstream's own error status (see relayedStatusError; the upstream's
// response is already on the wire). A governed caller hands it to the lifecycle
// to record the outcome and score the circuit breaker. A response that broke off
// mid-body is reported as errResponseAborted rather than raised; every caller
// must pass the error to reraiseAbort once it has recorded the outcome, so the
// client connection is still dropped.
func forwardFixedTarget(w http.ResponseWriter, r *http.Request, target *url.URL, authHeaders map[string]string, providerName string, propagateTrace bool, wrapBody func(*http.Response)) error {
	secrets := injectedSecrets(authHeaders)
	var upstreamErr, forwardErr error

	upstreamCtx, cancelUpstream := context.WithCancelCause(r.Context())
	defer cancelUpstream(nil)
	r = r.WithContext(upstreamCtx)

	rewrite := buildRewrite(target, authHeaders, propagateTrace)
	if wrapBody != nil {
		// wrapBody reads the body, so it must arrive in a coding it can read.
		// The caller's Accept-Encoding is dropped so the transport negotiates
		// gzip itself and decodes it before the body is wrapped. Forwarded as
		// sent, it let the upstream answer compressed — common HTTP clients,
		// the official OpenAI Python SDK's among them, accept gzip by default —
		// and the usage tee parsed compressed bytes, found nothing, and left
		// the request unpriced.
		inner := rewrite
		rewrite = func(pr *httputil.ProxyRequest) {
			inner(pr)
			pr.Out.Header.Del("Accept-Encoding")
		}
	}

	proxy := &httputil.ReverseProxy{
		Transport:     httpclient.SharedStreamingTransport(),
		FlushInterval: proxyFlushInterval,
		Rewrite:       rewrite,
		ModifyResponse: func(resp *http.Response) error {
			upstreamErr = relayedStatusError(providerName, resp)
			resp.Header.Set("X-Gateway-Provider", providerName)
			scanTimer := time.AfterFunc(sanitizeScanBudget, func() { cancelUpstream(nil) })
			sanitizeErr := sanitizeResponse(resp, secrets)
			scanTimer.Stop()
			if sanitizeErr != nil {
				return sanitizeErr
			}
			if wrapBody != nil {
				wrapBody(resp)
			}
			resp.Body = streamio.NewIdleReadCloser(resp.Body, streamio.IdleTimeout(), func() { cancelUpstream(streamio.ErrIdleTimeout) })
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			forwardErr = err
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				apierror.WriteOpenAI(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error", "request_too_large")
				return
			}
			// providerName comes from the configured registry, not user input.
			logger.Default().Error("fixed-target proxy upstream error", "provider", providerName, "error", err)
			apierror.WriteOpenAI(w, http.StatusBadGateway, "upstream connection failed", "server_error", "upstream_error")
		},
	}

	if err := serveForward(proxy, streamio.WrapResponseWriter(w), r); err != nil {
		return err
	}
	if forwardErr != nil {
		return forwardErr
	}
	return upstreamErr
}
