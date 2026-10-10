package handler

import (
	"encoding/json"
	"net/http"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/internal/apierror"
	"github.com/ferro-labs/ai-gateway/providers"
)

// Images handles POST /v1/images/generations.
// It routes image generation requests to the first registered ImageProvider that
// supports the requested model.
func Images(gw *aigateway.Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req providers.ImageRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}
		if req.Model == "" {
			apierror.WriteOpenAI(w, http.StatusBadRequest, "model is required", "invalid_request_error", "invalid_request")
			return
		}
		if req.Prompt == "" {
			apierror.WriteOpenAI(w, http.StatusBadRequest, "prompt is required", "invalid_request_error", "invalid_request")
			return
		}
		// No target can generate fewer than one image, so a count below one is
		// the caller's mistake. Left to the adapters it was not refused:
		// replicate and bedrock's titan/nova drop a zero count as an omitted
		// field, so `n: 0` reached the upstream as no count at all and was
		// generated, billed and answered 200 at the upstream's default count.
		if req.N != nil && *req.N < 1 {
			apierror.WriteOpenAI(w, http.StatusBadRequest, "n must be at least 1", "invalid_request_error", "invalid_request")
			return
		}

		attribution := &aigateway.RoutingAttribution{}
		ctx := aigateway.WithRoutingAttribution(r.Context(), attribution)
		resp, err := gw.GenerateImage(ctx, req)
		attribution.SetHeaders(w.Header())
		if err != nil {
			apierror.WriteRouteError(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}
